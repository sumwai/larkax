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
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/keyquota"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// Store 是账本与钱包的持久化。
type Store struct {
	db *sql.DB

	// logger 是结算路径上「不值得让结算失败、但必须有人看见」的出口。
	// nil 表示用 slog.Default()。
	logger *slog.Logger

	// now 是写入侧补建分区时用的时钟。nil 表示 time.Now。
	//
	// 它**只**影响「补哪个月的分区」这一件事：用量事件的 created_at 取的是
	// 库上的 now()（见 usage_event.go 里那一列的注释），不经过这里。所以
	// 换掉它不会让落库的时刻跟着变——两边刻意分开，是为了让用例能确定性地
	// 指向某个月，而不必去改系统时钟。
	now func() time.Time

	// chargeOrder 是扣费顺序的读点，由装配层接上（见 plancharge.go）。
	// nil 表示按平台默认值 plan_first 走——测试与不接这一层的调用方都落在这里。
	chargeOrder ChargeOrderSource

	// usagePartition 记录本进程已经确认过「哪个月的 usage_events 分区存在」。
	//
	// 记到月而不是一个 bool：跨月之后必须重新确认一次，否则新月份的第一笔
	// 结算会落进兜底分区——而那条路是静默的。
	partMu      sync.Mutex
	partMonth   string
	partEnsured bool
}

// New 包装一个已打开的数据库句柄。
func New(db *sql.DB) *Store { return &Store{db: db} }

// log 返回本实例的日志出口，没设过就用进程的默认 logger。
func (s *Store) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// nowUTC 返回写入侧补建分区时的当前时刻，统一 UTC。
func (s *Store) nowUTC() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// 编译期断言：它要满足网关对预留与结算落库的要求。
//
// 写成匿名接口而不是直接引用 gateway 的类型：本包不该 import gateway
// （见 scripts/check-layering.sh 规则三），而结构满足不需要那条 import。
// 代价是这里要与那边的接口定义**逐字对齐**——两边任意一侧改了签名，
// 这个断言就会红，这正是它存在的意义。
var _ interface {
	SaveReservation(context.Context, pipeline.Request, *pipeline.Reservation) (int64, error)
	Save(context.Context, pipeline.Request, *pipeline.Charge) error
	Release(context.Context, pipeline.Request, *pipeline.UpstreamFailure) error
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

// checkProtocol 检查这次请求的协议是不是入口真实存在的那几个。
//
// 认的是**入口**那几条路径，不是渠道能说的协议种类：这个字段记的是入口协议
// （由 internal/api 分派时填），不是渠道的 type。接一个新的**上游**协议不会让
// 这里多一个合法取值，只有多一条**入口**路径才会。
//
// 四个取值与入口的四条路径一一对应：`/v1/chat/completions`、`/v1/messages`、
// `/v1/responses`、`/v1/systemone`（见 internal/api 的 ServeHTTP）。所以这里与
// channel.Protocol.valid 今天**取值相同**——但两者判的不是同一件事：那边问
// 「这个值是不是一个合法的协议」，这边问「它有没有对应的入口路径」。
// 一个只有渠道用得上、没有入口路径的协议会出现在那边而不在这边，那时两份
// 清单就分岔了，所以加取值时仍要各自过一遍。
//
// 列上写着「openai / anthropic」是注释，不是约束——约束在这里。
//
// 漏了这里的表现是「入口收得下这个请求，却在预留那一步失败」——而那一步在
// 开事务、冻余额之前，所以钱不会白冻，但每一个请求都会失败。
func checkProtocol(p channel.Protocol) error {
	switch p {
	case channel.ProtocolOpenAI, channel.ProtocolAnthropic,
		channel.ProtocolOpenAIResponses, channel.ProtocolSystemOne:
		return nil
	case "":
		return ErrMissingProtocol
	default:
		return fmt.Errorf("%w：%q，只有 %q、%q、%q 与 %q 四种",
			ErrUnknownProtocol, string(p), channel.ProtocolOpenAI, channel.ProtocolAnthropic,
			channel.ProtocolOpenAIResponses, channel.ProtocolSystemOne)
	}
}

// ErrMissingPricedAt 表示调用方没有在 pipeline.Charge 上设置 PricedAt。
//
// 与 ErrMissingProtocol 同一个形状，代价还更大一些：usage_events.priced_at
// 是「这笔钱按哪一刻算的」唯一的落点——同一行的 local_hour 与 is_off_day 记的是
// 那一刻的当地钟点与那一天算不算休息日，三者出自同一次求值（见 pipeline.Charge
// 的注释）。价格按受理时刻定，拿结算时刻去复算会算出另一个档位的价，而两个价
// 看起来都合理，靠账单本身分不出来。
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

// ErrMissingUpstreamModel 表示这次结算没有带上「实际发出去的上游模型名」。
//
// 与 ErrMissingProtocol / ErrMissingAPIKey 同一个形状，理由要多一层：
// billing_requests.upstream_model 从建表那天起**每一行都是 NULL**（别名
// 从来没有读取方，见 #338），于是「漏设」与「历史行」在库里长得完全一样。
// 写进去的那一刻是这两者唯一还分得清的地方。
//
// 生产路径不会撞上它：网关在选路之后把 route.Result.UpstreamModel 填进
// pipeline.Request（见 internal/gateway 的 finishCharge），而结算必然发生在
// 一次成功的选路之后。它挡的是手工构造请求的调用方与测试夹具——那种行
// 回答不了「这次实际用了哪个上游名」，而那一列正是为此存在的。
//
// # 它前面那两道管的是别的东西
//
// 这个值的来源是别名配置：网关按规范名查 model_aliases.upstream_model 拿到它
// （见 internal/catalog 的 UpstreamModel）。那一段现在有两道保证——
// 写入侧 catalog.normalizeUpstreamModel 规整并拒绝空白，schema 侧 model_aliases
// 上的 CHECK（迁移 00022）拒绝空串与纯空白。所以「配了一个空的上游名」在配置
// 写进去的时候就被挡下了，走不到这里。
//
// 但那两道判的都是**配置列**，与这里判的不是同一个东西：这里是这次结算带上来的
// 入参，配置之后还可能经过网关与 pipeline，没有哪一道替这一列做保证。
var ErrMissingUpstreamModel = errors.New(
	"缺少上游模型名：调用方没有设置 pipeline.Request.UpstreamModel（它由网关在选路之后填）")

// checkUpstreamModel 检查这次结算有没有带上实际发出去的上游模型名。
//
// 判的是**这次结算带上来的那个值**（pipeline.Request.UpstreamModel），不是别名
// 配置那一列——两者是两样东西：配置列由 model_aliases 上的 CHECK 与
// catalog.normalizeUpstreamModel 管（见迁移 00022），而这个是网关在选路之后填进
// 来的入参，schema 照不到它。
//
// 位置在 Save 的最前面（开事务之前）：为空时这次结算一个副作用都不该留下。
func checkUpstreamModel(model string) error {
	if model == "" {
		return ErrMissingUpstreamModel
	}
	return nil
}

// SaveReservation 落一条预留记录并占用它的来源（钱包或额度包）。
//
// 它做三件事，在**同一个事务**里：
//
//  1. 往 billing_requests 插一行（status=pending）。request_id 是唯一键，
//     所以重复预留会被数据库挡下，而不是靠调用方自觉。
//  2. 按扣费顺序占住来源：从账户余额扣时把 user_wallets.frozen 加上预留金额；
//     从额度包扣时一分钱都不冻，改为在那一行上记下订阅 id、占用的面值
//     （plan_reserved_amount）与可用量（reserved_quota）。后者见 plancharge.go。
//  3. req.Idempotency 非 nil 时，往 idempotency_keys 插一行。
//
// 冻结前要检查「可用的钱够不够」——`balance - frozen >= 预留额`。
// 表上的 CHECK 只管两个字段各自非负，管不了这个，所以判断在 SQL 的
// WHERE 里做：**更新影响 0 行就是余额不足**，比先查后改少一次竞态。
// 额度包那条路的同一句话是「面值与可用量都还够」，见 planCovers。
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

	// 扣费顺序在开事务之前读：它是一次**配置**查询，不参与事务内的判据；
	// 读不动时的兜底由那个读点自己完成（见 ChargeOrderSource）。
	order := s.chargeOrderFor(ctx, r.UserID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 额度判定排在**所有加锁与写入之前**，它自己拿的那把锁就是判据的
	// 串行化点（见 internal/keyquota 的 limitSQL：锁住 api_keys 那一行）。
	//
	// 为什么它必须是判据而不是像入口那样只是一次早拒绝：判据的形状是
	// 「读一次这把 key 的累计花费，再插一行 pending」，这两步之间只要有另一个
	// 事务插进一行，两个并发请求就都会看到「没超」，于是都放行——上限被整个
	// 穿过去，而事后从数据上完全看不出来（每一行都是合法的）。
	//
	// 为什么排在钱包锁**之前**：求和要扫这把 key 的全部历史行，而钱包行锁
	// 是这个用户所有请求（包括他别的 key）的串行点。把一次可能很慢的求和
	// 塞进钱包锁的持有期里，会连累那些与本次判定无关的请求。排在前面之后，
	// 钱包锁的持有期仍然只覆盖「改冻结额 + 插账单行」这两条按主键定位的语句。
	//
	// 拿锁顺序因此是「先 key 后钱包」。另一条会改钱包的路径是 Save（结算），
	// 它只锁钱包、不碰 api_keys；auth 的 touch 只 UPDATE api_keys、不碰钱包。
	// 没有任何路径按「先钱包后 key」拿锁，所以这个顺序不会成环。
	if quotaErr := keyquota.CheckInTx(ctx, tx, req.APIKeyID, r.Amount); quotaErr != nil {
		return 0, quotaErr
	}

	// 快照先清空：一次失败的尝试不该把上一次的订阅 id 留在调用方手里——
	// Settle 读的就是这个字段。
	r.UserPlanID = 0

	id, err := s.reserveInOrder(ctx, tx, order, req, r)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交预留失败: %w", err)
	}
	return id, nil
}

// reserveInOrder 按扣费顺序挑来源，并在**同一个事务**里落预留行。
//
// 两条路的锁不一样：余额那条锁钱包行，额度包那条锁订阅行（并读账本科目余额），
// 两者不会同时持有——额度包覆盖的请求不动钱包（见 plancharge.go 的说明）。
//
// 都覆盖不住时返回的是**钱包那一侧的诊断**：那条错误已经分得清「没有开户」与
// 「余额不足」，而额度包那侧没有覆盖不是一类错误，只是这条请求用不了那些包。
func (s *Store) reserveInOrder(ctx context.Context, tx *sql.Tx, order string, req pipeline.Request, r *pipeline.Reservation) (int64, error) {
	switch order {
	case chargeOrderBalanceOnly:
		id, ok, err := s.reserveFromWallet(ctx, tx, req, r)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, diagnoseFreezeFailure(ctx, tx, WalletKey{UserID: r.UserID, Asset: r.Asset})
		}
		return id, nil

	case chargeOrderBalanceFirst:
		id, ok, err := s.reserveFromWallet(ctx, tx, req, r)
		if err != nil {
			return 0, err
		}
		if ok {
			return id, nil
		}
		id, ok, err = s.reserveBestPlan(ctx, tx, req, r)
		if err != nil {
			return 0, err
		}
		if ok {
			return id, nil
		}
		return 0, diagnoseFreezeFailure(ctx, tx, WalletKey{UserID: r.UserID, Asset: r.Asset})

	default: // plan_first，也是认不出的取值那一侧
		id, ok, err := s.reserveBestPlan(ctx, tx, req, r)
		if err != nil {
			return 0, err
		}
		if ok {
			return id, nil
		}
		id, ok, err = s.reserveFromWallet(ctx, tx, req, r)
		if err != nil {
			return 0, err
		}
		if ok {
			return id, nil
		}
		return 0, diagnoseFreezeFailure(ctx, tx, WalletKey{UserID: r.UserID, Asset: r.Asset})
	}
}

// reserveBestPlan 读候选并按序挑第一条覆盖得住的。
func (s *Store) reserveBestPlan(ctx context.Context, tx *sql.Tx, req pipeline.Request, r *pipeline.Reservation) (int64, bool, error) {
	candidates, err := selectPlanCandidates(ctx, tx, req, r.UserID, r.Asset)
	if err != nil {
		return 0, false, err
	}
	if len(candidates) == 0 {
		return 0, false, nil
	}
	return reserveFromPlan(ctx, tx, req, r, candidates)
}

// reserveFromWallet 是余额那条路：冻住钱包、插预留行。
//
// 返回 ok=false 表示「这一条现在收不下」——没有钱包行，或者可用余额不够。
// 两个原因都由调用方在最终失败时用 diagnoseFreezeFailure 分开报；中途 ok=false
// 只意味着「该去看下一个来源了」，不是错误。
func (s *Store) reserveFromWallet(ctx context.Context, tx *sql.Tx, req pipeline.Request, r *pipeline.Reservation) (int64, bool, error) {
	key := WalletKey{UserID: r.UserID, Asset: r.Asset}

	// 先给钱包行加锁，再插预留记录。
	//
	// 顺序固定为「先钱包后记录」是有意的：所有会改钱包的路径都按这个
	// 顺序拿锁，于是并发时不会出现「A 持钱包等记录、B 持记录等钱包」。
	if lockErr := lockWallets(ctx, tx, key); lockErr != nil {
		return 0, false, lockErr
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
		return 0, false, constraintFailure(fmt.Errorf("冻结余额失败: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return 0, false, nil
	}

	// NULLIF 的第二个实参写成 0::bigint 而不是裸 0：NULLIF 要求两个实参
	// 同型，裸 0 是 int4，会把解析期还是 unknown 的 $3 / $8 一并推成 int4。
	// 那两列（channel_id、pricing_rule_id）在 schema 里是 bigint，推窄之后
	// int4 范围外的 id 会在**驱动编码阶段**失败，走不到外键校验。
	// 见 TestReservationChannelIDBeyondInt4。
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO billing_requests
			(request_id, user_id, api_key_id, channel_id, canonical_model, protocol,
			 streamed, reserved_amount, reserved_asset, status,
			 pricing_rule_id, pricing_expr_snapshot, started_at)
		VALUES ($1, $2, $12, NULLIF($3, 0::bigint), $4, $5, $11, $6::numeric, $7, 'pending',
		        NULLIF($8, 0::bigint), NULLIF($9, ''), $10)
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
		// 这条 INSERT 有四条外键（users / api_keys / channels / pricing_rules）
		// 与一条唯一键（request_id）。认得出是哪一条由驱动错误上的约束名回答，
		// 这里只负责把它们都归成「约束冲突」。
		return 0, false, constraintFailure(fmt.Errorf("写预留记录失败: %w", err))
	}

	// 幂等记录（调用方带了 Idempotency-Key 时）。见上面第 3 条：
	// 它与预留同一个事务，所以这里返回错误会让冻住的钱一并回滚。
	//
	// ErrClaimed 原样往上抛（用 %w 包一层会保留它，但这里不必包）：
	// 入口要按 errors.Is 认出它，转成「等一等再重放」而不是一次失败。
	if req.Idempotency != nil {
		if err := idempotency.ClaimInTx(ctx, tx, req.Idempotency, id); err != nil {
			// 幂等记录也有两条外键（api_keys / billing_requests）。它自己的
			// 撞键走 ON CONFLICT DO NOTHING，不会以 23505 冒上来，所以这里
			// 兜的是外键。
			return 0, false, constraintFailure(err)
		}
	}
	return id, true, nil
}

// AddReservation 给一次**已经落库**的预留追加冻结额。
//
// # 它只服务于流式那条路
//
// 流式请求不再按调用方声明的 max_tokens 一次性冻满，而是边转发边按本地累计的
// 真实产出补冻（见 internal/gateway 的 streamFreeze 与 #498）。一次补冻就是本函数：
// 把钱包的 frozen 与账单行的 reserved_amount 各加上同一个数。
//
// # 两处必须一起加
//
// 「冻了多少」有**两个**读者，而它们各读一处：
//
//   - 钱包的 frozen：准入判据读它（本条语句的 WHERE，与 SaveReservation 逐字
//     相同），对账的不变量也读它——`frozen == 所有 pending 行的 reserved_amount
//     之和`（见 reconcile.go 的 readPendingReservations）；
//   - 账单行的 reserved_amount：**释放**按它解冻（见 Release 与 Save 里的
//     releaseFrozen），两处都从这里读。
//
// 只加一处就破了对账的不变量，而且破法很安静：先加 frozen 会多出一笔没有
// 来历的冻结；先加 reserved_amount 会在释放时解冻它从未冻过的钱（frozen 变负，
// 由表上的 CHECK 挡住，报出来的是一个看不懂的 ErrFrozenUnderflow）。所以这两处
// 改动在**同一条语句**里（见下面的 CTE），要么都发生要么都不发生。
//
// # 准入判据复用同一条谓词，不先查后改
//
// `balance - frozen >= $4` 是**唯一**挡住透支的地方（见 SaveReservation 里那段
// 注释）。补冻走的是同一条 UPDATE、同一个条件，只是金额换成了这一次的增量：
// 换成「先 SELECT 余额再判断」会在两个并发请求之间开一个窗口，各自都看到
// 「够」，于是两边都冻——余额被透支，而事后从数据上完全看不出来。
//
// # 它为什么不重复判 API Key 的额度
//
// SaveReservation 会判（见 keyquota.CheckInTx），这里不判，是有意的：那次判定
// 是**准入**——「这把 key 还收不收得下这一笔」，而这次请求早就通过了。补冻是
// 同一次请求内部的递进，按段重复判它会让一条已经在跑的流中途因为额度判据失败
// 而断掉，而那条判据在那一刻并没有发生变化。代价说清楚：单条长输出流可以让
// 这把 key 的累计花费越过它的额度（额度按 pending 行的 reserved_amount 求和，
// 见 keyquota 的 spentSQL，所以下一次请求会立刻看到）。这条代价写在 #498 的
// 报告里，不在本函数里补一个「也判一下」——那会把两种错（余额不够 / 额度不够）
// 混进同一个返回值，而调用方对它们要做的处置完全不同。
//
// # 额度包覆盖的请求加的是包，不是钱包
//
// 预留快照里带 `user_plan_id` 时，这一次请求从来没有冻过钱包（见
// plancharge.go），所以这里也不能去动 `user_wallets.frozen`：那会把一个余额为
// 零、只有额度包的用户在流中间断掉。加的是账单行的 `plan_reserved_amount`
// 与 `reserved_quota`，判断用的是与初次选源同一组维度。outputTokens 是这一次
// 补冻覆盖的输出 token 增量，token 包按它加可用量的占用；其余单位不看它。
//
// # 一条语句，两处改动
//
// 余额这条路的「锁钱包 → 改冻结额 → 改账单行」压在**一条**语句里（数据修改
// CTE）。这不是为了少打几行 SQL：补冻在流中间按段反复调用（见 gateway 的
// streamFreeze），每次省下的往返都乘在补冻次数上，也乘在这把钱包行锁的
// 持有期上——而那是同一用户所有并发请求的串行点。
//
//   - `target` 只读一次账单行，拿到这一笔预留的来源（`user_plan_id`）。它不加锁：
//     来源在预留那一刻就定了、之后不变，而「还在不在 pending」由下面那条
//     UPDATE 的 WHERE 再判一次（影响 0 行就是已经结算或已释放）。
//   - `frozen` 对钱包做与 SaveReservation 逐字相同的 UPDATE（同一句
//     `balance - frozen >=`）；额度包覆盖的请求在这里命中 0 行，所以不冻钱包。
//   - `billed` 加账单行的 reserved_amount，金额取 `frozen` 的结果。它对额度包
//     那条路也会执行一次（值不变）：这次 UPDATE 在那里只做一件事——拿住账单行
//     的锁并确认它仍是 pending，后面的 growPlanReservation 据此不必自己再判。
//
// 代价要算全：额度包覆盖的流上，这次**同值** UPDATE 每次补冻都在账单行上留下
// 一个新行版本——一个死元组，有索引的话还有索引项，随补冻次数线性增长，靠
// autovacuum 回收。WAL 两边都写：改动前那句 `SELECT … FOR UPDATE` 置 xmax 的
// 锁记录同样落 WAL，多出来的是死元组，不是「从没有 WAL 到有 WAL」。也就是说
// 这条路用往返换了账单行的行版本增长；余额那条路本来就是一次真 UPDATE，
// 没有这笔额外的账。
//
// 锁序因此是「钱包 → 账单行」，与 Save（结算）一致；两步的先后不由文本顺序
// 碰运气，而是由 `billed` 对 `frozen` 的 RETURNING 依赖固定下来。额度包那条路
// 仍然先账单行、再 user_plans，与 Save 的「钱包 → 账单行 → 订阅行」同向。
// Release 仍是「账单行 → 钱包」，但它与 AddReservation 只会对同一笔预留相遇，
// 而这两步由同一个请求 goroutine 顺序驱动（预留 → 补冻 → 结算或释放），不并发。
func (s *Store) AddReservation(ctx context.Context, req pipeline.Request, delta money.Amount, outputTokens int64) error {
	// 非正的增量没有任何意义，而静默当成 0 会让「补冻了但没冻上」看起来像
	// 成功——调用方据此推进它的累计量，于是钱包与账单行少冻一段，没有任何
	// 东西会报出来。调用方只在确实要补时才调它（见 gateway 的 streamFreeze）。
	if !delta.IsPositive() {
		return fmt.Errorf("追加冻结额必须为正，当前 %s", delta)
	}
	if outputTokens < 0 {
		return fmt.Errorf("追加冻结的输出 token 数不能为负，当前 %d", outputTokens)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		pendingRows int
		planID      sql.NullInt64
		frozenRows  int
		billedRows  int
	)
	err = tx.QueryRowContext(ctx, `
		WITH target AS (
		        SELECT user_plan_id
		          FROM billing_requests
		         WHERE request_id = $3 AND status = 'pending'
		), frozen AS (
		        UPDATE user_wallets
		           SET frozen = frozen + $4::numeric, updated_at = now()
		         WHERE user_id = $1 AND asset = $2
		           AND (SELECT count(*) FROM target) = 1
		           AND (SELECT user_plan_id FROM target) IS NULL
		           AND balance - frozen >= $4::numeric
		        RETURNING 1
		), billed AS (
		        UPDATE billing_requests
		           SET reserved_amount = reserved_amount +
		               CASE WHEN EXISTS (SELECT 1 FROM frozen) THEN $4::numeric
		                    ELSE 0::numeric END
		         WHERE request_id = $3 AND status = 'pending'
		           AND (SELECT count(*) FROM target) = 1
		        RETURNING 1
		)
		SELECT (SELECT count(*) FROM target),
		       (SELECT user_plan_id FROM target),
		       (SELECT count(*) FROM frozen),
		       (SELECT count(*) FROM billed)`,
		req.UserID, req.Asset, req.RequestID, delta.String()).Scan(
		&pendingRows, &planID, &frozenRows, &billedRows)
	if err != nil {
		return constraintFailure(fmt.Errorf("追加冻结额失败: %w", err))
	}

	// 这一行已经不在 pending：请求标识对不上、或者同一次收尾已经先跑了。
	// 两种都不该再冻一笔——那笔钱没有解除路径。
	//
	// billedRows 为 0 是同一条结局的另一个窗口：target 读到的快照里还在
	// pending，而等账单行锁的时候有人把它结算/释放了；UPDATE 重判谓词之后
	// 影响 0 行。
	if pendingRows == 0 || billedRows == 0 {
		return fmt.Errorf(
			"请求 %s 的预留已经不在 pending 状态，不能追加冻结额（本该由同一次收尾处理）",
			req.RequestID)
	}

	if planID.Valid {
		if err = growPlanReservation(ctx, tx, req.RequestID, planID.Int64, delta, outputTokens); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("提交追加冻结失败: %w", err)
		}
		return nil
	}

	if frozenRows == 0 {
		// 空资产原先由 lockWallets 直接挡下（它逐个校验资产非空），报的就是
		// 这一句；压成一条语句之后不再经过那把锁，校验挪到这里。位置变了，
		// 报的还是同一句话——它不是「没有钱包行」，是调用方传错了。
		//
		// 放在额度包那一支**之后**：那条路从来不碰钱包，空资产在它那里不是
		// 错误（与改动前一致，那时 lockWallets 在额度包分支返回之后才调用）。
		if req.Asset == "" {
			return errors.New("钱包的资产品种为空")
		}
		// 与 SaveReservation 同一处、同一套诊断：钱包行不在（ErrNoWallet）与
		// 可用余额不够（ErrInsufficientAvailable）要分开报。整个语句已经回滚，
		// 账单行那一次同值 UPDATE 也一并撤掉。
		return diagnoseFreezeFailure(ctx, tx, WalletKey{UserID: req.UserID, Asset: req.Asset})
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交追加冻结失败: %w", err)
	}
	return nil
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
// （说明预留机制出了问题），这个是**冻结**时就不够（正常的余额不足，
// 拒绝请求、或者切断这条流即可）。两者的处置与告警级别都不同。
//
// # 值与 pipeline 里那个是同一个
//
// 判据的读者除了本包还有两层：入口层（把一个预留在开始之前就失败的请求回成
// 429）与网关（流式补冻够不着余额时据此中断那条流）。网关不认识本包——它只
// 声明一个 ReservationStore 接口，所以那个值只能住在两边都认识的 pipeline
// 里。这里重新导出一次而不是另声明一个同义的错误：`errors.Is` 靠**值**判，
// 两个同义的哨兵会让它只在其中一侧成立，而那一侧之外的地方读到的都是「不是
// 余额问题」——于是补冻失败被当成存储故障放行，透支就这么发生了。
var ErrInsufficientAvailable = pipeline.ErrInsufficientAvailable

// ErrNoWallet 表示用户没有对应的钱包行。
//
// 它是一条**配置/数据**问题而不是余额问题：用户该在打请求之前先开一次户
// （当前由 `larkaxctl wallet create` 建，见 internal/ledger/store/wallet.go）。
// 注册本身不建钱包行，`user create` 只写 users。
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
	// 同一位置、同一个理由：漏了上游模型名时也不该留下任何副作用。
	if err := checkUpstreamModel(req.UpstreamModel); err != nil {
		return err
	}
	// 用量事件按月分区，而分区不会自己长出来——没有这一步，种子覆盖的月份
	// 过完之后每一行都落进兜底分区，静默地。位置在 BeginTx **之前**，
	// 理由见 ensureUsageEventPartition 的注释。
	s.ensureUsageEventPartition(ctx)

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
	//
	// 同一条语句顺带读出这次预留的来源与它占着的可用量：结算要按快照里的
	// 订阅记 prepaid 科目，并把这笔请求实际用掉的量记进 user_plans——
	// **不重新选源**（在途期间订阅到期、新增都不影响这一笔）。
	//
	// 面值（plan_reserved_amount）也在这里读：用量事件要原样记下它，而
	// 「原样」意味着与账单行同一个数，不是在这一层另算一遍。流式那条路会在
	// 转发途中不断追加这一列，所以必须在结算这一刻读，早读会少掉后面的增量。
	var (
		planID           sql.NullInt64
		reservedQuotaStr string
		planReservedStr  string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT user_plan_id, reserved_quota::text, plan_reserved_amount::text
		  FROM billing_requests
		 WHERE request_id = $1 AND status = 'pending'
		 FOR UPDATE`, req.RequestID).Scan(&planID, &reservedQuotaStr, &planReservedStr)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAlreadySettled
	}
	if err != nil {
		return fmt.Errorf("查预留来源失败: %w", err)
	}
	planReserved, parseErr := money.Parse(planReservedStr)
	if parseErr != nil {
		return fmt.Errorf("预留的额度包面值读不出来（原文 %q）: %w", planReservedStr, parseErr)
	}

	settled, err := markSettled(ctx, tx, req, charge)
	if err != nil {
		return err
	}
	if !settled {
		return ErrAlreadySettled
	}

	// 已用量的写入与分录的写入在同一个事务里，而且排在分录**之前**：它要锁
	// 订阅行，作废那一侧读面值余额之前先拿的是同一把锁。顺序反过来的话，作废
	// 可能在「分录还没写、行已不再 pending」的窗口里把这份面值核销掉，随后
	// 结算把它扣成负数。
	if planID.Valid {
		reservedQuota, parseErr := money.Parse(reservedQuotaStr)
		if parseErr != nil {
			return fmt.Errorf("预留的可用量读不出来（原文 %q）: %w", reservedQuotaStr, parseErr)
		}
		if err := s.applyPlanQuota(ctx, tx, planID.Int64, charge.Usage, reservedQuota); err != nil {
			return err
		}
	}

	if err := insertEntries(ctx, tx, charge.Entries); err != nil {
		return err
	}
	// 用量事件紧跟着分录写：两条都是只追加的底稿，而钱包余额是从它们推出来的
	// 快照。顺序照「先底稿、后快照」——中途失败时回滚掉的是整组，不是快照单独
	// 跑在前面。
	if err := insertUsageEvent(ctx, tx, req, charge, planReserved.String()); err != nil {
		return err
	}
	if err := applyToWallets(ctx, tx, charge.Entries); err != nil {
		return err
	}
	// 额度包覆盖的请求从来没有冻过钱包（见 plancharge.go）：它的分录只动
	// prepaid 科目，`releaseFrozen` 会按 0 去减一个可能不存在的钱包行，
	// 报出一个与本次结算无关的「钱包行不在」。
	if !planID.Valid {
		if err := releaseFrozen(ctx, tx, req.RequestID); err != nil {
			return err
		}
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
//
// # upstream_status 与 error_code 在不计费的这一侧只有这一条写入路径
//
// 那两列在不计费的行上说的是「这次请求为什么没计费」，而不计费的结局**只有
// 这一条写库的路**：结算那条路（markSettled）只在计费时才走——上游失败时
// pipeline.Settle 直接返回一个错误、一个副作用都不留。
//
// 计费那一侧另有自己的那一份：静默截断那一次要计费，而「回答只写了一半」得
// 留在账单上，它由 markSettled 写（见那里的注释）。两处写的是同一对列，取的
// 是同一个词汇表里不同的子集——不计费那一侧是那四个失败分类，计费这一侧只有
// 截断一个。所以 `error_code IS NOT NULL` 要连着 status 一起读。
//
// failure 为 nil 表示这次没有上游失败的依据：插件阻断、结算本身出错，
// 以及「还没到结算那一步就返回了」那条路（结算之前客户端就断了、context
// 被取消——那时上游那一次调用多半压根没发生）都是这样。两列那时留 NULL，
// 而 **NULL 在这里不是「不知道」，是「这次不是因上游失败而不计费」**——
// 与 client_canceled_at 的 NULL 同一个口径（见 markSettled 的注释）。
//
// 它与 status 写在同一条 UPDATE 里，不另起一条：分开写会有一个窗口让这一行
// 已经是 released 而依据还没写上，而那个组合在库里读起来正是「释放了但
// 没有理由」——一个不存在却看得见的状态。
//
// # channel_id 与 upstream_model 也在这里补
//
// 同一件事的另一半：这次请求**打到了哪个渠道、用哪个上游名**。两列在预留
// 时写不出来（那时还没选路），成功那条路由 markSettled 补，不计费这条路
// 只有这里能补。口径与那一处逐字一致，理由见下面那条 UPDATE 前的注释。
func (s *Store) Release(ctx context.Context, req pipeline.Request, failure *pipeline.UpstreamFailure) error {
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
		planID sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, reserved_asset, reserved_amount, status, user_plan_id
		  FROM billing_requests
		 WHERE request_id = $1
		 FOR UPDATE`, req.RequestID).Scan(&id, &userID, &asset, &amount, &status, &planID)
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
	//
	// 额度包覆盖的请求跳过这一步：它从来没冻过钱包（reserved_amount 是 0），
	// 而「减 0」仍然要求钱包行存在——一个余额为零、只有额度包的用户没有
	// 钱包行时，那次释放会因为一个与它无关的缺行而失败，钱与状态都留在原地。
	if !planID.Valid {
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
	}

	// 依据跟着 status 一起写，就在这个事务里：这一行上面已经被 FOR UPDATE
	// 锁住、而且仍是 pending（不是 pending 的话上面就返回了），所以写进去的
	// 必然是这次释放带来的那一份，不会有别的写方插进来。
	//
	// NULLIF 挡住的是「没有值」而不是「值为空」：没有上游响应时状态码是 0，
	// 没有依据时分类是空串，两个都不该作为数据写进去（见 Release 的注释）。
	//
	// # 渠道与上游模型名为什么也在这里
	//
	// 它们与 status 是同一件事的两面：这一行在**预留**时插进去（见
	// SaveReservation），而预留发生在选路之前，那时还不知道会打到哪儿。
	// 成功那条路在结算时补上（见 markSettled）；不计费这条路**不走结算**，
	// 所以补在这里——不补的话失败行上这两列恒为 NULL，恰恰是「某个渠道对
	// 某批模型全在报错」时最需要它们的地方没有它们。
	//
	// 口径与 markSettled 逐字一致（同样是 NULLIF）：写进来的值由调用方在
	// 选路之后填进 pipeline.Request，一个候选都没选中时是 0 与空串，落成
	// NULL——那是「没有渠道可记」，不是「漏了」。
	//
	// 它写在**同一条** UPDATE 里，理由与 status 那条相同：分两次写会有一个
	// 窗口让这一行已经是 released 而「打到了哪儿」还没写上。
	failureStatus, failureCode := failureColumns(failure)
	// channel_id 的 NULLIF 实参标成 bigint，理由同 SaveReservation——
	// 该列是 bigint，裸 0 会把参数推成 int4。
	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_requests
		   SET status = 'released', settled_at = now(),
		       upstream_status = NULLIF($2, 0),
		       error_code = NULLIF($3, ''),
		       channel_id = NULLIF($4, 0::bigint),
		       upstream_model = NULLIF($5, '')
		 WHERE id = $1`,
		id, failureStatus, failureCode, req.ChannelID, req.UpstreamModel); err != nil {
		// channel_id 有外键指向 channels：选中的渠道在释放之前被删掉时，
		// 这条 UPDATE 会以 23503 冒上来。
		return constraintFailure(fmt.Errorf("标记释放失败: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交释放失败: %w", err)
	}
	return nil
}

// failureColumns 把一份可空的失败依据拆成两个 SQL 参数。
//
// 拆解本身在这里，是为了让「有没有依据」的判据只有一处：调用方拿到的是
// 两个标量，两个都空就是没有依据。顺带，nil 与零值因此落成同一个 NULL
// ——SQL 那侧的 NULLIF 收到的已经是标量，它分不出「没传」与「传了个 0」，
// 那一层挡不住这件事。
func failureColumns(failure *pipeline.UpstreamFailure) (status int, code string) {
	if failure == nil {
		return 0, ""
	}
	return failure.Status, failure.Code
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
//
// # 未截断的结算金额也在这里落
//
// final_amount 落的是 Settlement.Actual，而它撞上预留上界时**就是**预留额
// （见 internal/billing 的 SettleWithCharges）——超出预留的那一截到这里已经
// 被抹平了。所以光有 final_amount，事后从库里判不出这一行有没有撞上界：
// 撞了与没撞看起来一样，都是一笔不超过预留的金额。
//
// 未截断的那个数在 Settlement.Breakdown.Total 里（截断只改 Actual，明细不动），
// 这里把它写进 computed_amount。于是
//
//	final_amount = min(computed_amount, 有效预留)
//
// 在每一行上都成立，而 computed_amount > 有效预留 就是「这一行撞了」。
// 有效预留是两列之和：钱包那条路记在 reserved_amount 上，额度包覆盖的路记在
// plan_reserved_amount 上（钱包一分没冻，见 plancharge.go）。差出来的那一截
// 是被预留截住、由平台承担掉的金额——记下来才看得出上界是错得离谱还是擦边
// （见迁移 00021 的注释；读侧的两条 SQL 见 docs/metrics.md）。
//
// 两列写在同一条 UPDATE 里，不另起一条：分开写会有一个窗口让这一行
// status 已经是 settled 而 computed_amount 还是 NULL，而那个组合在库里
// 读起来正是「结算了但没有计算金额」——一个不存在却看得见的状态。
//
// # 上游模型名与渠道同一个位置落
//
// upstream_model 与 channel_id 说的是同一件事的两面：这次请求**实际发去了
// 哪里、用哪个名字**。两者都只有在选路之后才答得出来，所以都在这条 UPDATE
// 里写（见 pipeline.Request.UpstreamModel）。写成 NULLIF 而不是让它报错：
// 值本身非空已经由 Save 开头的 checkUpstreamModel 挡住，这里只是不把一个
// 空串当名字写进去。
//
// # 上游侧那两列也在这里写
//
// upstream_status 与 error_code 一度也在这条 UPDATE 里，参数位写的是字面量
// 0 与空串，所以那两列恒为 NULL——包括上游回了 400 的那条失败记录。参数位
// 是字面量时 NULLIF 挡不住任何东西，它读起来却像「这里可能有值」，于是
// 「这两列没有写入方」这件事被一个看起来有逻辑的表达式盖住了。
//
// 当时修的是另一半：不计费的依据写在 Release 里，而这条结算 UPDATE 只在
// **计费**时才走（因上游失败不计费时 pipeline.Settle 直接返回错误，这一行
// 一条都不执行）。计费这一侧那时也确实没有值可写：上游响应里没有状态码
// （channel.Response 不带它），「这次是 2xx」这件事在链路里根本没有一个
// 字段记着，编一个 200 出来就是假数据。
//
// 后来链路里有了那个字段：网关把这次响应的归类带了下来（见
// pipeline.Outcome 的 UpstreamStatus 与 ErrorCode，它由 Settle 搬进
// Charge）。于是**已计费的行**上有了唯一一种写得出来的依据——
// upstream_stream_truncated，也就是「上游吐过字之后静默把连接掐断」：那一次
// 回答照常计费，而它没有错误帧，调用方要靠「帧序列里没有结束标记」自己发现
// 被截短了（见 docs/api.md 的「没有结束标记就是被截断」一节）。按账单核对金额
// 的人看不到它，账单是唯一能说明「这次回答只写了一半」的地方（快照有留存期，
// 见 docs/request-retention.md）。这条 UPDATE 因此也写这两列了。
//
// 零值与空串落成 NULL 的口径与 Release 逐字一致：正常结束、客户端断开、插件
// 终止都不带依据，写进来的是零值，NULLIF 把它们折成 NULL。那里的 NULL 含义是
// 「这次没有上游侧的依据可写」——与 client_canceled_at 的 NULL 同一个口径
// （见上面那段注释）。
//
// **它仍然不能读成「这次没有上游失败」。** 上游吐了字之后**报错**的那一次
// 要计费（token 已经产生了，上游是算了钱的），账单就是已结算的行，两列同样
// 是 NULL——见 stream_e2e_test.go 的 TestStreamUpstreamErrorMidStream。所以
// `error_code IS NOT NULL` 不能当成「上游出过问题的请求」的判据；在已计费的
// 行上它答的是「钱收了，回答是不是在调用方不知情的情况下被截短」，在不计费
// 的行上答的才是「这次没向上游收钱，是不是因为上游这一侧出了事」。
func markSettled(ctx context.Context, tx *sql.Tx, req pipeline.Request, charge *pipeline.Charge) (bool, error) {
	// channel_id 是 bigint，NULLIF 的实参标成 bigint（理由见 SaveReservation）。
	res, err := tx.ExecContext(ctx, `
		UPDATE billing_requests
		   SET status = 'settled',
		       final_amount = $2::numeric,
		       computed_amount = $5::numeric,
		       settled_at = now(),
		       channel_id = NULLIF($3, 0::bigint),
		       upstream_model = NULLIF($6, ''),
		       upstream_status = NULLIF($7, 0),
		       error_code = NULLIF($8, ''),
		       client_canceled_at = CASE WHEN $4 THEN now() END
		 WHERE request_id = $1 AND status = 'pending'`,
		req.RequestID, charge.Settlement.Actual.String(), req.ChannelID,
		charge.ClientCanceled, charge.Settlement.Breakdown.Total.String(),
		req.UpstreamModel, charge.UpstreamStatus, charge.ErrorCode)
	if err != nil {
		// 与 Release 同一条外键：channel_id → channels(id)。
		return false, constraintFailure(fmt.Errorf("标记结算失败: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取影响行数失败: %w", err)
	}
	return affected > 0, nil
}

func insertEntries(ctx context.Context, tx *sql.Tx, entries []ledger.Entry) error {
	// 两个关联 id 列都是 bigint，NULLIF 的实参标成 bigint（理由见 SaveReservation）。
	for _, e := range entries {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ledger_entries
				(txn_id, account, asset, amount, entry_type,
				 billing_request_id, payout_id, memo, created_at)
			VALUES ($1, $2, $3, $4::numeric, $5,
			        NULLIF($6, 0::bigint), NULLIF($7, 0::bigint), NULLIF($8, ''), now())`,
			string(e.TxnID), e.Account.String(), string(e.Asset), e.Amount.String(),
			string(e.EntryType), int64OrZero(e.BillingRequestID), int64OrZero(e.PayoutID),
			e.Memo)
		if err != nil {
			return constraintFailure(fmt.Errorf("写分录失败（科目 %s）: %w", e.Account, err))
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
	// 按科目收集金额切片而不是边遍历边 Add：Add 溢出时夹到 int64 边界，
	// 而夹住之后写进钱包的是一个**错的增量**——账本（权威）与钱包快照
	// 从此不一致，且两处看起来都是「平的」，事后无从发现。判据要按真实和
	// 判（见 money.SumChecked），所以先把每条分录的金额留着。
	deltas := map[WalletKey][]money.Amount{}
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
		deltas[key] = append(deltas[key], e.Amount)
	}

	// 按固定的键序更新：与加锁顺序一致，避免并发时出现交叉等待。
	keys := make([]WalletKey, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	sortWalletKeys(keys)

	for _, key := range keys {
		delta, ok := money.SumChecked(deltas[key]...)
		if !ok {
			// 同一个事务里这个用户在这一资产上的净增量放不进 int64。库里
			// 的列是 numeric(20, 8)，存得下一个 money 表示不了的余额，所以
			// 这个数据真的读得出来；而写回快照时只能夹住——那会把错的增量
			// 落进 user_wallets，账本与快照从此不一致。拒绝整笔，由调用方
			// 决定怎么处置（拆成多笔、或先修数据）。
			return fmt.Errorf(
				"用户 %d 的 %s 分录净额超出金额可表示范围（%d 条分录之和放不进 int64；库里的 numeric(20, 8) 存得下，money 表示不了）: %w",
				key.UserID, key.Asset, len(deltas[key]), money.ErrOverflow)
		}
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
			return constraintFailure(fmt.Errorf("更新余额失败: %w", err))
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
