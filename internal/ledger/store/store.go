// Package store 是账本与钱包的持久化层。
//
// 它把 internal/ledger 造出来的分录落库，并同步维护 user_wallets 里的
// **余额快照**。这是唯一同时知道「账本」与「钱包」两个概念的地方。
//
// 一次结算落的是**两条只追加的底稿**，在同一个事务里：ledger_entries（动了
// 多少钱）与 usage_events（那笔钱是按什么用量、哪个时刻算出来的，见
// usage_event.go）。钱包余额是从它们推出来的快照。
//
// # 权威与快照
//
// 账本是权威，钱包余额是它的**具体化物**——一个读优化。余额可以随时由
// 账本重新算出来（见 Reconcile），而反过来不成立。所以：
//
//   - 任何写入都先写分录，再改快照，**在同一个事务里**
//   - 不一致时以账本为准，快照被修正
//   - 修正动作写一条 adjustment 分录，而不是静默 UPDATE 余额——
//     账本只追加，这一点没有例外
//
// # 钱包里装的是哪个账户
//
// user_wallets 的主键是 (user_id, asset)，而账本里的科目比它细：
// 同一个用户既有 user:<id>（他能花的钱），也可能有 merchant_payable:<id>
// （平台欠他的分成）。表结构决定了钱包只能具体化**其中一个**。
//
// 这里选 user:<id>：它是热路径上唯一会被读的那个（「这个用户付得起吗」），
// 而商户应付是在结算时才查的，从账本现算完全可以接受。
// 其余科目一律从账本推导，不进快照。
//
// # 加锁顺序
//
// 一次交易可能涉及多个用户（例如转账），那时要给多个钱包行加锁。
// **顺序不固定就会死锁**：A 锁了 1 等 2，B 锁了 2 等 1。
// 所以这里把所有要锁的用户排序后再逐个锁，与交易里科目的出现顺序无关。
//
// # 为什么它 import internal/pipeline
//
// 本包的方法入参用的是 internal/pipeline 的 DTO（Request / Charge /
// Reservation），而不是自己另定义一套。全仓只有这一处「持久化层以编排层的
// 类型为入参」，方向看着反常，所以说清楚：
//
// 这几个类型描述的是「一次请求在账本上长什么样」（哪个用户、哪次请求、
// 多少钱、什么状态），它是编排层与持久化层之间的**共同词汇**。网关侧声明
// 自己要什么落库能力时用的也是同一组类型（见 internal/gateway 的
// SettlementStore 与 ReservationStore），两边认同一份定义才不必在中间再加
// 一层转换。
//
// 本包只用到它们的**形状**，不调用 internal/pipeline 的任何函数——这条依赖
// 不会把编排逻辑带进持久化层。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// Store 是账本与钱包的持久化。
type Store struct {
	db *sql.DB
}

// New 包装一个已打开的数据库句柄。
func New(db *sql.DB) *Store { return &Store{db: db} }

// 编译期断言：它要满足网关对预留与结算落库的要求。
//
// 写成匿名接口而不是直接引用 gateway 的类型：本包不该 import gateway
// （见 scripts/check-layering.sh 规则三），而结构满足不需要那条 import。
// 代价是这里要与那边的接口定义**逐字对齐**——两边任意一侧改了签名，
// 这个断言就会红，这正是它存在的意义。
var _ interface {
	SaveReservation(context.Context, pipeline.Request, *pipeline.Reservation) (int64, error)
	Save(context.Context, pipeline.Request, *pipeline.Charge) error
	Release(context.Context, pipeline.Request) error
} = (*Store)(nil)

// ErrAlreadySettled 表示这次请求已经结算过。
//
// 它是**正常结局**而不是错误条件：重放一次已经完成的结算不该重复扣费，
// 也不该让调用方以为出了问题。单独一个错误类型是为了让调用方能区分
// 「重复」与「失败」。
var ErrAlreadySettled = errors.New("这次请求已经结算过")

// WalletKey 是一个钱包行的主键。
type WalletKey struct {
	UserID int64
	Asset  string
}

// ErrMissingProtocol 表示调用方没有在 pipeline.Request 上设置 Protocol。
//
// 它必须是一个**明确的失败**，而不是往那一列写一个空串：billing_requests.protocol
// 是 NOT NULL，但拦的是 NULL，空串写得进去。于是「漏设了字段」会表现成一条
// 协议为空的账单——事后按协议统计时它既不是 openai 也不是 anthropic，
// 看起来像数据脏了，而不像代码里少了一处赋值。这与本包此前写死 "openai"
// 是同一个形状的错误：**错值静默落库，事后才发现**。
var ErrMissingProtocol = errors.New(
	"缺少协议标识：调用方没有设置 pipeline.Request.Protocol（它由入口按请求路径设置）")

// ErrUnknownProtocol 表示 Protocol 不是已知的入口协议。
//
// 与 ErrMissingProtocol 分开：那个是**漏了赋值**，这个是**赋了一个不存在
// 的值**。前者的修法是补一处调用点，后者要查的是赋值处从哪拿到的这个值。
var ErrUnknownProtocol = errors.New("协议标识不是已知的入口协议")

// ErrMissingAPIKey 表示调用方没有在 pipeline.Request 上设置 APIKeyID。
//
// 与 ErrMissingProtocol 同一个形状，但理由要多一层：billing_requests.api_key_id
// **可空**，而且这一列的历史行本来就是空的（补上写入点之前每一行都是 NULL，
// 见 internal/migrate/sql/00017_api_key_id_comment.sql 的列注释）。于是
// 「调用方漏了赋值」与「这是补写入点之前的行」在库里长得一模一样——事后按
// key 归因用量时，那一行既不属于任何一把 key，也不像缺了什么。
//
// 分辨这两者只有写进去的那一刻还做得到，所以分辨放在这里：新行一律有值，
// 列上的可空留给历史。
var ErrMissingAPIKey = errors.New(
	"缺少 API Key 标识：调用方没有设置 pipeline.Request.APIKeyID（它由入口从鉴权结果里取）")

// checkAPIKey 检查调用方给没给这次请求用的那把 API Key。
//
// 0 是「没设置」而不是一把 key：api_keys.id 是 bigserial，从 1 起。
func checkAPIKey(id int64) error {
	if id <= 0 {
		return ErrMissingAPIKey
	}
	return nil
}

// checkProtocol 检查这次请求的协议是不是入口真实存在的那两种。
//
// 只认两套：调用方的入口只有这两条路径，已注册的适配器也只有这两个
// （见 channel.Protocol）。列上写着「openai / anthropic」是注释，不是约束——
// 约束在这里，也在 channel.Protocol 的取值集合里，两处指的是同一份定义。
func checkProtocol(p channel.Protocol) error {
	switch p {
	case channel.ProtocolOpenAI, channel.ProtocolAnthropic:
		return nil
	case "":
		return ErrMissingProtocol
	default:
		return fmt.Errorf("%w：%q，只有 %q 与 %q 两种",
			ErrUnknownProtocol, string(p), channel.ProtocolOpenAI, channel.ProtocolAnthropic)
	}
}

// ErrMissingPricedAt 表示调用方没有在 pipeline.Charge 上设置 PricedAt。
//
// 与 ErrMissingProtocol 同一个形状，代价还更大一些：usage_events.priced_at
// 是「事后复算按哪一刻的档位算的价」**唯一**的落点（见 pipeline.Charge
// 的注释——价格按受理时刻定，拿结算时刻去复算会算出另一个档位的价，而两个价
// 看起来都合理，靠账单本身分不出来）。
//
// 它的零值落库是公元 1 年：不报错，事后也看不出那一行是「当时就是按这一刻
// 算的」还是「漏设了」。分辨这两者只有写进去的那一刻还做得到，所以分辨放在
// 这里。
//
// 生产路径不会撞上它：pipeline.Settle 一定填这个字段（取自受理时刻，见
// pipeline 的 env）。它挡的是手工构造 Charge 的调用方与测试夹具——那种行
// 是库里不可能出现的数据。
var ErrMissingPricedAt = errors.New(
	"缺少受理时刻：调用方没有设置 pipeline.Charge.PricedAt（它由 pipeline.Settle 填）")

// checkPricedAt 检查这次结算有没有带上「价格是按哪一刻定的」。
func checkPricedAt(at time.Time) error {
	if at.IsZero() {
		return ErrMissingPricedAt
	}
	return nil
}

// SaveReservation 落一条预留记录并冻住金额。
//
// 它做三件事，在**同一个事务**里：
//
//  1. 往 billing_requests 插一行（status=pending）。request_id 是唯一键，
//     所以重复预留会被数据库挡下，而不是靠调用方自觉。
//  2. 把 user_wallets.frozen 加上预留金额。
//  3. req.Idempotency 非 nil 时，往 idempotency_keys 插一行。
//
// 冻结前要检查「可用的钱够不够」——`balance - frozen >= 预留额`。
// 表上的 CHECK 只管两个字段各自非负，管不了这个，所以判断在 SQL 的
// WHERE 里做：**更新影响 0 行就是余额不足**，比先查后改少一次竞态。
//
// # 第三件事为什么必须在这个事务里
//
// 幂等记录与预留必须一起成或一起不成：只记了幂等而没预留，重试会被告知
// 「已处理」而钱没扣；只预留而没记幂等，重试就重复扣费——那正是幂等键要
// 防的那个多收。所以它**不是**一个可以挪到调用方的步骤：任何两步之间的
// 窗口都会落在上面两种状态之一，而它没有任何东西会报出来。
//
// 顺序上它排在插完 billing_requests **之后**：那样才能把刚拿到的 id 写进
// billing_request_id，让「这个幂等键对应哪一笔预留」在库里直接读得出来。
// 这一步失败（含键已被别人占住）会让整个事务回滚，冻住的钱一并放开。
func (s *Store) SaveReservation(ctx context.Context, req pipeline.Request, r *pipeline.Reservation) (int64, error) {
	if r == nil {
		return 0, errors.New("预留为空")
	}
	if r.Amount.IsNegative() {
		return 0, fmt.Errorf("预留金额为负: %s", r.Amount)
	}
	// 在开事务、冻余额**之前**查：协议不对时这次预留一个副作用都不该留下。
	if err := checkProtocol(req.Protocol); err != nil {
		return 0, err
	}
	// 同一位置、同一个理由：漏了哪把 key 时也不该留下任何副作用。
	if err := checkAPIKey(req.APIKeyID); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	key := WalletKey{UserID: r.UserID, Asset: r.Asset}

	// 先给钱包行加锁，再插预留记录。
	//
	// 顺序固定为「先钱包后记录」是有意的：所有会改钱包的路径都按这个
	// 顺序拿锁，于是并发时不会出现「A 持钱包等记录、B 持记录等钱包」。
	if lockErr := lockWallets(ctx, tx, key); lockErr != nil {
		return 0, lockErr
	}

	// 可用余额不足时更新影响 0 行。不先 SELECT 再判断：那中间有窗口，
	// 两个并发请求会各自看到「够」，然后都扣。
	res, err := tx.ExecContext(ctx, `
		UPDATE user_wallets
		   SET frozen = frozen + $3::numeric, updated_at = now()
		 WHERE user_id = $1 AND asset = $2
		   AND balance - frozen >= $3::numeric`,
		key.UserID, key.Asset, r.Amount.String())
	if err != nil {
		return 0, fmt.Errorf("冻结余额失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		// 两种可能：没有这个钱包行，或者可用余额不足。分开查一次是为了
		// 给调用方可区分的信息——前者是「用户不存在」，后者是「钱不够」。
		return 0, diagnoseFreezeFailure(ctx, tx, key)
	}

	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO billing_requests
			(request_id, user_id, api_key_id, channel_id, canonical_model, protocol,
			 streamed, reserved_amount, reserved_asset, status,
			 pricing_rule_id, pricing_expr_snapshot, started_at)
		VALUES ($1, $2, $12, NULLIF($3, 0), $4, $5, $11, $6::numeric, $7, 'pending',
		        NULLIF($8, 0), NULLIF($9, ''), $10)
		RETURNING id`,
		// 协议由入口设置（见 pipeline.Request.Protocol）。这里**不填默认值**：
		// 写死一个会让另一条入口的账单全部记错，而这张表正是事后按协议统计的
		// 依据（#143 就是写死了 "openai"）。缺了或取值不对就报错，见 checkProtocol。
		req.RequestID, r.UserID, req.ChannelID, req.CanonicalModel, string(req.Protocol),
		r.Amount.String(), r.Asset, r.RuleID, r.PricingExprSnap, r.RuleMatchedAt,
		// 是不是流式请求由入口告诉计费层（见 pipeline.Request.Streamed）：
		// 这一层看不出请求的形状，也就不该替它填一个默认值——写死 false
		// 会让每条流式账单都记成非流式，而那张表正是事后区分两条路的依据。
		req.Streamed,
		// 用的是哪把 API Key 同样由入口给出（见 pipeline.Request.APIKeyID）。
		// 这里**不加 NULLIF**：checkAPIKey 已经挡下了 0，真放一个 0 进来会撞
		// 上 api_keys 的外键——两层都出声，好过一层静默写 NULL。
		req.APIKeyID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写预留记录失败: %w", err)
	}

	// 幂等记录（调用方带了 Idempotency-Key 时）。见上面第 3 条：
	// 它与预留同一个事务，所以这里返回错误会让冻住的钱一并回滚。
	//
	// ErrClaimed 原样往上抛（用 %w 包一层会保留它，但这里不必包）：
	// 入口要按 errors.Is 认出它，转成「等一等再重放」而不是一次失败。
	if req.Idempotency != nil {
		if err := idempotency.ClaimInTx(ctx, tx, req.Idempotency, id); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交预留失败: %w", err)
	}
	return id, nil
}

// diagnoseFreezeFailure 在冻结失败后查出**为什么**失败。
//
// 「更新影响 0 行」有两种可能：没有这个钱包行，或者可用余额不足。
// 两者的处置完全不同——前者要建钱包（或说明用户不存在），
// 后者要用户充值——所以这里分开报，而不是笼统的「冻结失败」。
func diagnoseFreezeFailure(ctx context.Context, tx *sql.Tx, key WalletKey) error {
	var balance, frozen string
	err := tx.QueryRowContext(ctx,
		`SELECT balance, frozen FROM user_wallets WHERE user_id = $1 AND asset = $2`,
		key.UserID, key.Asset).Scan(&balance, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 用户 %d 没有 %s 钱包", ErrNoWallet, key.UserID, key.Asset)
	}
	if err != nil {
		return fmt.Errorf("查钱包失败: %w", err)
	}
	return fmt.Errorf("%w: 余额 %s，已冻结 %s", ErrInsufficientAvailable, balance, frozen)
}

// ErrInsufficientAvailable 表示「余额 − 冻结」不够这次预留。
//
// 与 ErrInsufficientBalance 分开：那个是**结算**时发现扣不动
// （说明预留机制出了问题），这个是**预留**时就不够（正常的余额不足，
// 拒绝请求即可）。两者的处置与告警级别都不同。
var ErrInsufficientAvailable = errors.New("可用余额不足")

// ErrNoWallet 表示用户没有对应的钱包行。
//
// 它是一条**配置/数据**问题而不是余额问题：正常流程里注册时就该建钱包。
var ErrNoWallet = errors.New("没有对应的钱包")

// Save 落一次结算：写分录、写用量事件、解冻、扣余额，全在一个事务里。
//
// # 幂等
//
// 靠 billing_requests.request_id 的唯一键。重放同一次结算会在这里被
// 挡下并返回 ErrAlreadySettled，而**不是重复扣费**。
//
// 这不是可选的：上游超时、进程重启、队列重投都会让同一次结算到达两次，
// 而重复扣费是用户立刻能发现的错，也是事后最难解释的那类。
//
// # 用量事件为什么必须在这一个事务里
//
// 账本分录与 usage_events 是**同一件事的两面**：分录说动了多少钱，用量事件
// 说那笔钱是按什么用量算出来的。分成两次写就会出现「有分录、没有用量」的
// 行，而对账正是拿这两边对——一边缺失时，对出来的差额没有来历，查的人只能
// 猜。同一个事务还顺带解决幂等：重放整笔回滚，不会多出一行用量。
//
// 它**不是**一次额外的数据库往返：这次结算本来就开着事务、要写若干条分录，
// 多一条 INSERT 只多一次同一条连接上的语句，不新开连接、不新起 goroutine、
// 不多一次提交；它失败时走的路与分录写失败完全相同——回滚整笔、把错误交给
// 调用方（网关那一侧本来就是「结算落库失败必须报出来」）。真去另起一条异步
// 链路反而两头不落好：既丢了原子性，又让「写入静默丢失」成为可能——而这张表
// 是财务底稿，丢了没人会来问。
//
// # 它挂在热路径上吗
//
// Save 本身就在请求路径的同步段上：网关在把响应交回调用方之前调它
// （见 internal/gateway 的 finishCharge，非流式与流式两条路共用）。所以
// 「不要挂进热路径的同步段」这条约束，在这里落成的不是「异步写」，而是
// **不在这条路径上加新的等待**——用量事件跟着已经开着的那次事务走，不新开
// 连接、不新起 goroutine、不另查一次库。这与快照那条链路的取舍不同：快照是
// 排障辅助，可以异步、可以丢（见 internal/gateway 的 snapshotAsync）；用量
// 事件是账的一部分，必须与分录同生共死。
func (s *Store) Save(ctx context.Context, req pipeline.Request, charge *pipeline.Charge) error {
	if charge == nil {
		return errors.New("账单为空")
	}
	// 在开事务、写任何东西**之前**挡下：漏设受理时刻时这次结算一个副作用
	// 都不该留下。位置与 SaveReservation 的 checkProtocol / checkAPIKey 一致。
	if err := checkPricedAt(charge.PricedAt); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先按固定顺序锁住所有涉及的钱包。
	keys := walletKeysOf(charge.Entries)
	if lockErr := lockWallets(ctx, tx, keys...); lockErr != nil {
		return lockErr
	}

	// 判幂等要在加锁之后：锁之前查会有窗口，两个并发重放都能看到
	// 「还没结算」然后各自往下走。
	settled, err := markSettled(ctx, tx, req, charge)
	if err != nil {
		return err
	}
	if !settled {
		return ErrAlreadySettled
	}

	if err := insertEntries(ctx, tx, charge.Entries); err != nil {
		return err
	}
	// 用量事件紧跟着分录写：两条都是只追加的底稿，而钱包余额是从它们推出来的
	// 快照。顺序照「先底稿、后快照」——中途失败时回滚掉的是整组，不是快照单独
	// 跑在前面。
	if err := insertUsageEvent(ctx, tx, req, charge); err != nil {
		return err
	}
	if err := applyToWallets(ctx, tx, charge.Entries); err != nil {
		return err
	}
	if err := releaseFrozen(ctx, tx, req.RequestID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交结算失败: %w", err)
	}
	return nil
}

// Release 把一次失败的请求的预留解冻，并标记为 released。
//
// 上游失败不计费，但**预留必须释放**：不释放的话那笔钱会一直冻着，
// 而界面上表现为「用户说他的钱不见了」。
//
// 「释放一个不存在的东西」与「解冻时没有钱包行可落」是两件事，别混：
// 前者是没有预留记录（也就没冻过钱），目的已经达到，返回 nil；后者是
// 数据不一致——冻结额长在钱包行上，行不在就真的什么都没释放，报错回滚。
func (s *Store) Release(ctx context.Context, req pipeline.Request) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id     int64
		userID int64
		asset  string
		amount string
		status string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, reserved_asset, reserved_amount, status
		  FROM billing_requests
		 WHERE request_id = $1
		 FOR UPDATE`, req.RequestID).Scan(&id, &userID, &asset, &amount, &status)
	if errors.Is(err, sql.ErrNoRows) {
		// 没有预留记录：可能是预留根本没落库（那时也没冻钱）。
		// 不当成错误——释放一个不存在的东西，目的已经达到了。
		return nil
	}
	if err != nil {
		return fmt.Errorf("查预留记录失败: %w", err)
	}
	if status != "pending" {
		// 已经结算或已释放，重复调用是幂等的。
		return nil
	}

	// 释放**不写分录**：它没有发生任何资金流动，只是把冻住的钱放开。
	//
	// 写一条零金额分录留痕的诱惑很大，但账本里出现零金额分录会让
	// 「有分录就说明发生过资金流动」这个判据失效——而对账要靠它。
	// 释放的痕迹留在 billing_requests.status 上，那才是它该在的地方。
	//
	// 这一点比看上去重要：冻结与释放如果都在账本上留痕，
	// 对账脚本就必须区分「真实的零金额交易」与「只是解冻」，
	// 而它们在一个只追加的账本里长得一样。

	// 解冻**不夹零**：frozen 减到负数说明快照已经小于待释放的预留之和，
	// 而那一刻的差额是唯一的证据——夹到零会把两边同时归零，差额从此
	// 无处可查。交给表上的 CHECK 拒绝，由 unfreezeFailure 翻译成
	// ErrFrozenUnderflow。
	res, err := tx.ExecContext(ctx, `
		UPDATE user_wallets
		   SET frozen = frozen - $3::numeric, updated_at = now()
		 WHERE user_id = $1 AND asset = $2`,
		userID, asset, amount)
	if err != nil {
		return unfreezeFailure(err, fmt.Sprintf("用户 %d 的 %s 预留（%s）", userID, asset, amount))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		// 这条语句只按主键定位（userID 与 asset 都取自上面 SELECT 出来的
		// 那一行），所以 0 行只有一个解释：钱包行不在。**不能当成功**——
		// 那样紧接着的「标记已释放」会照常执行，冻结额却一分未动。
		return walletRowMissing(fmt.Sprintf("用户 %d 的 %s 预留（%s）", userID, asset, amount))
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_requests SET status = 'released', settled_at = now()
		 WHERE id = $1`, id); err != nil {
		return fmt.Errorf("标记释放失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交释放失败: %w", err)
	}
	return nil
}

// markSettled 把预留标记为已结算并写入最终金额。
//
// 返回 false 表示这次请求已经结算过（幂等命中）。
//
// # 渠道在这里才写得进去
//
// 预留发生在选路**之前**（见 internal/gateway 的 begin），那时还不知道会
// 选中哪个渠道，所以 SaveReservation 的 INSERT 拿到的是 0、落库成 NULL。
// 结算时渠道已经定了，req.ChannelID 带着它——**这一行是那个值唯一的落点**。
// 漏掉它，billing_requests.channel_id 就恒为 NULL，按渠道归因用量、渠道级
// 对账都拿不到数据，而单看任何一层都像是写了的。
//
// 重放不会把它抹掉：判据是下面 WHERE 里的 status = 'pending'，已经结算过的
// 行影响 0 行、直接返回 false（调用方转成 ErrAlreadySettled），
// 整条 UPDATE 都不执行。
//
// # 客户端取消的时刻也在这里落
//
// 「客户端在流式响应中途断开」这个事实由网关当场判定（internal/gateway 的
// pumpStream），一路上带到 pipeline.Outcome.ClientCanceled 与
// pipeline.Charge.ClientCanceled——但落库只有这一处。在它接上之前，那个
// 信号走到 Charge 就到头了：库里没有任何痕迹，于是「这个渠道是不是经常被
// 客户端掐断」这个问题答不出来，而客户端断开是**真实发生**的事。
//
// 写成 CASE 而不是两个分支：一行已经结算的记录只有两种结局——取消过，
// 或者没有。NULL 表示没有取消，而**不是**「不知道」：没取消是一次确定的
// 事实（这一行是在结算路径上写下的，那条路径必然知道客户端有没有走）。
func markSettled(ctx context.Context, tx *sql.Tx, req pipeline.Request, charge *pipeline.Charge) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE billing_requests
		   SET status = 'settled',
		       final_amount = $2::numeric,
		       settled_at = now(),
		       channel_id = NULLIF($5, 0),
		       upstream_status = NULLIF($3, 0),
		       error_code = NULLIF($4, ''),
		       client_canceled_at = CASE WHEN $6 THEN now() END
		 WHERE request_id = $1 AND status = 'pending'`,
		req.RequestID, charge.Settlement.Actual.String(), 0, "", req.ChannelID,
		charge.ClientCanceled)
	if err != nil {
		return false, fmt.Errorf("标记结算失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取影响行数失败: %w", err)
	}
	return affected > 0, nil
}

func insertEntries(ctx context.Context, tx *sql.Tx, entries []ledger.Entry) error {
	for _, e := range entries {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries
				(txn_id, account, asset, amount, entry_type,
				 billing_request_id, payout_id, memo, created_at)
			VALUES ($1, $2, $3, $4::numeric, $5,
			        NULLIF($6, 0), NULLIF($7, 0), NULLIF($8, ''), now())`,
			string(e.TxnID), e.Account.String(), string(e.Asset), e.Amount.String(),
			string(e.EntryType), int64OrZero(e.BillingRequestID), int64OrZero(e.PayoutID),
			e.Memo)
		if err != nil {
			return fmt.Errorf("写分录失败（科目 %s）: %w", e.Account, err)
		}
	}
	return nil
}

// applyToWallets 把分录的净影响加到钱包余额上。
//
// # 只加 user:<id>
//
// 钱包只具体化 user 科目（见包注释）。其余科目（平台收入、上游成本、
// 商户应付）不进快照——它们不参与「这个用户付得起吗」这个判断，
// 需要时从账本现算。
//
// 同一科目在一个事务里可能出现多次，所以先按科目汇总再更新，
// 而不是逐条 UPDATE：后者会多几次往返，也可能让锁的持有时间变长。
func applyToWallets(ctx context.Context, tx *sql.Tx, entries []ledger.Entry) error {
	deltas := map[WalletKey]money.Amount{}
	for _, e := range entries {
		if e.Account.Type() != ledger.AccountUser {
			continue
		}
		// 用科目自己认自己的用户 id，与对账的汇总走同一条判定
		// （ledger.Account.UserID）：「一笔分录算给哪个用户」只有一处答案。
		// 非规范写法在这里解不出来只可能是账本层的 Bug——自己拼了一个
		// 绕过 ParseAccount 的 Account。报出来而不是跳过：跳过会让余额少扣。
		id, err := e.Account.UserID()
		if err != nil {
			return err
		}
		key := WalletKey{UserID: id, Asset: string(e.Asset)}
		deltas[key] = deltas[key].Add(e.Amount)
	}

	// 按固定的键序更新：与加锁顺序一致，避免并发时出现交叉等待。
	keys := make([]WalletKey, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	sortWalletKeys(keys)

	for _, key := range keys {
		delta := deltas[key]
		if delta.IsZero() {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE user_wallets
			   SET balance = balance + $3::numeric, updated_at = now()
			 WHERE user_id = $1 AND asset = $2
			   AND balance + $3::numeric >= 0`,
			key.UserID, key.Asset, delta.String())
		if err != nil {
			return fmt.Errorf("更新余额失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取影响行数失败: %w", err)
		}
		if affected == 0 {
			// 余额会变成负数。**不能静默夹到零**：那等于平台替用户
			// 承担了差额而账上看不出来。拒绝这次结算，让上层决定
			// 怎么办（补扣、冻结账号、人工处理）。
			return fmt.Errorf("%w: 用户 %d 的 %s 余额不足以扣 %s",
				ErrInsufficientBalance, key.UserID, key.Asset, delta.Neg())
		}
	}
	return nil
}

// ErrInsufficientBalance 表示扣款会让余额变成负数。
//
// 预留机制本该让这种情况不发生（预留是上界），所以真出现它说明
// 有别的路径动了这个钱包，或者预留算错了。**报出来而不是夹到零**：
// 夹零等于平台替用户承担差额，而账上完全看不出来。
//
// 解冻让 `frozen` 变成负数报的是 ErrFrozenUnderflow，不是这个错误；
// 但那个错误的链里也带着这个 sentinel（为了兼容既有判断），
// 所以**别拿它判断用户欠费**——见 ErrFrozenUnderflow 的注释。
var ErrInsufficientBalance = errors.New("余额不足")

// releaseFrozen 按预留记录解冻。
//
// 与 Release 一样**不夹零**，理由见那里的注释。
func releaseFrozen(ctx context.Context, tx *sql.Tx, requestID string) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE user_wallets w
		   SET frozen = w.frozen - b.reserved_amount,
		       updated_at = now()
		  FROM billing_requests b
		 WHERE b.request_id = $1
		   AND b.status = 'settled'
		   AND w.user_id = b.user_id
		   AND w.asset = b.reserved_asset`, requestID)
	if err != nil {
		return unfreezeFailure(err, "请求 "+requestID+" 的预留")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		// 0 行只有一个解释：钱包行不在。调用点 Save 里，b.status = 'settled'
		// 是同一事务刚被 markSettled 置上的（它影响 0 行时 Save 已经返回了），
		// 所以 join 的另一边必然缺行。**不能当成功**：这次结算的分录已经
		// 写下，冻结额却一分未动，两边从此对不上。
		return walletRowMissing(unfreezeSubject(ctx, tx, requestID))
	}
	return nil
}

// unfreezeSubject 给「解冻时没有钱包行」这条错误补上对象描述。
//
// 由 request_id 回查预留记录拿用户与资产，**只用于错误信息**：releaseFrozen
// 的调用点手里只有 request_id，而排障先要看的就是「哪个用户、哪种资产」。
// 这次回查在错误路径上才发生，正常结算不付这份代价；它的结果也不参与判断
// ——判断已经由「影响 0 行」做出，所以查不到时只是描述得粗一点，
// 不改变结论（把回查的错也写进文本，免得它悄悄消失）。
func unfreezeSubject(ctx context.Context, tx *sql.Tx, requestID string) string {
	var (
		userID int64
		asset  string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT user_id, reserved_asset FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&userID, &asset)
	if err != nil {
		return fmt.Sprintf("请求 %s 的预留（回查预留记录失败: %v）", requestID, err)
	}
	// 与 Release 那条 subject 同构：都是「用户 X 的 <资产> 预留（<补充信息>）」。
	return fmt.Sprintf("用户 %d 的 %s 预留（请求 %s）", userID, asset, requestID)
}

// lockWallets 按固定顺序给钱包行加锁。
//
// # 为什么必须排序
//
// 一次交易可能涉及多个用户。两个并发交易各自按自己的顺序拿锁时，
// 只要涉及的用户集合有交集就会死锁：A 锁了 1 等 2，B 锁了 2 等 1。
// 排序之后所有事务按同一个全局顺序拿锁，死锁就不可能形成。
//
// 去重也是必须的：同一行锁两次在某些隔离级别下会自己等自己。
func lockWallets(ctx context.Context, tx *sql.Tx, keys ...WalletKey) error {
	uniq := map[WalletKey]struct{}{}
	for _, k := range keys {
		if k.Asset == "" {
			return errors.New("钱包的资产品种为空")
		}
		uniq[k] = struct{}{}
	}
	ordered := make([]WalletKey, 0, len(uniq))
	for k := range uniq {
		ordered = append(ordered, k)
	}
	sortWalletKeys(ordered)

	for _, k := range ordered {
		var one int
		// FOR UPDATE 锁的是**已存在的行**。行不存在时不加锁——
		// 那种情况下后续的 UPDATE 会失败，错误信息比一把空锁有用。
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM user_wallets WHERE user_id = $1 AND asset = $2 FOR UPDATE`,
			k.UserID, k.Asset).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("锁定钱包失败: %w", err)
		}
	}
	return nil
}

// sortWalletKeys 按 (user_id, asset) 排序。
//
// 按 asset 次之是为了让「同一个用户的不同币种」也有确定顺序——
// 只按 user_id 排的话，同一个用户的两种币种之间顺序不定。
func sortWalletKeys(keys []WalletKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].UserID != keys[j].UserID {
			return keys[i].UserID < keys[j].UserID
		}
		return keys[i].Asset < keys[j].Asset
	})
}

// walletKeysOf 从分录里收集需要加锁的钱包。
//
// 认用户 id 走的是与写路径、对账汇总同一条判定（ledger.Account.UserID）。
// 加锁的键必须与更新时算出来的键是同一批：两处各认各的科目，表现是**漏锁**，
// 而漏锁只在并发时才看得出来。
func walletKeysOf(entries []ledger.Entry) []WalletKey {
	var out []WalletKey
	for _, e := range entries {
		id, err := e.Account.UserID()
		if err != nil {
			// 不是 user 科目，或者后缀读不出用户 id。两者都不进钱包，
			// 也就不需要加锁——applyToWallets 对前者跳过、对后者报错。
			continue
		}
		out = append(out, WalletKey{UserID: id, Asset: string(e.Asset)})
	}
	return out
}

func int64OrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
