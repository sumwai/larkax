// Package store 是账本与钱包的持久化层。
//
// 它把 internal/ledger 造出来的分录落库，并同步维护 user_wallets 里的
// **余额快照**。这是唯一同时知道「账本」与「钱包」两个概念的地方。
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
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"

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

// 编译期断言：它要满足网关对结算落库的要求。
var _ interface {
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

// SaveReservation 落一条预留记录并冻住金额。
//
// 它做两件事，在同一个事务里：
//
//  1. 往 billing_requests 插一行（status=pending）。request_id 是唯一键，
//     所以重复预留会被数据库挡下，而不是靠调用方自觉。
//  2. 把 user_wallets.frozen 加上预留金额。
//
// 冻结前要检查「可用的钱够不够」——`balance - frozen >= 预留额`。
// 表上的 CHECK 只管两个字段各自非负，管不了这个，所以判断在 SQL 的
// WHERE 里做：**更新影响 0 行就是余额不足**，比先查后改少一次竞态。
func (s *Store) SaveReservation(ctx context.Context, req pipeline.Request, r *pipeline.Reservation) (int64, error) {
	if r == nil {
		return 0, errors.New("预留为空")
	}
	if r.Amount.IsNegative() {
		return 0, fmt.Errorf("预留金额为负: %s", r.Amount)
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
			(request_id, user_id, channel_id, canonical_model, protocol,
			 streamed, reserved_amount, reserved_asset, status,
			 pricing_rule_id, pricing_expr_snapshot, started_at)
		VALUES ($1, $2, NULLIF($3, 0), $4, $5, false, $6::numeric, $7, 'pending',
		        NULLIF($8, 0), NULLIF($9, ''), $10)
		RETURNING id`,
		req.RequestID, r.UserID, req.ChannelID, req.CanonicalModel, "openai",
		r.Amount.String(), r.Asset, r.RuleID, r.PricingExprSnap, r.RuleMatchedAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写预留记录失败: %w", err)
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

// Save 落一次结算：写分录、解冻、扣余额，全在一个事务里。
//
// # 幂等
//
// 靠 billing_requests.request_id 的唯一键。重放同一次结算会在这里被
// 挡下并返回 ErrAlreadySettled，而**不是重复扣费**。
//
// 这不是可选的：上游超时、进程重启、队列重投都会让同一次结算到达两次，
// 而重复扣费是用户立刻能发现的错，也是事后最难解释的那类。
func (s *Store) Save(ctx context.Context, req pipeline.Request, charge *pipeline.Charge) error {
	if charge == nil {
		return errors.New("账单为空")
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

	if _, err := tx.ExecContext(ctx, `
		UPDATE user_wallets
		   SET frozen = GREATEST(frozen - $3::numeric, 0), updated_at = now()
		 WHERE user_id = $1 AND asset = $2`,
		userID, asset, amount); err != nil {
		return fmt.Errorf("解冻失败: %w", err)
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
func markSettled(ctx context.Context, tx *sql.Tx, req pipeline.Request, charge *pipeline.Charge) (bool, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE billing_requests
		   SET status = 'settled',
		       final_amount = $2::numeric,
		       settled_at = now(),
		       upstream_status = NULLIF($3, 0),
		       error_code = NULLIF($4, '')
		 WHERE request_id = $1 AND status = 'pending'`,
		req.RequestID, charge.Settlement.Actual.String(), 0, "")
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
		id, err := strconv.ParseInt(e.Account.ID(), 10, 64)
		if err != nil {
			// 科目是 user:<id> 但 id 解不出来——那是账本层的 Bug，
			// 不是数据问题。报出来而不是跳过：跳过会让余额少扣。
			return fmt.Errorf("科目 %s 的用户 id 非法: %w", e.Account, err)
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
var ErrInsufficientBalance = errors.New("余额不足")

// releaseFrozen 按预留记录解冻。
func releaseFrozen(ctx context.Context, tx *sql.Tx, requestID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE user_wallets w
		   SET frozen = GREATEST(w.frozen - b.reserved_amount, 0),
		       updated_at = now()
		  FROM billing_requests b
		 WHERE b.request_id = $1
		   AND b.status = 'settled'
		   AND w.user_id = b.user_id
		   AND w.asset = b.reserved_asset`, requestID)
	if err != nil {
		return fmt.Errorf("解冻失败: %w", err)
	}
	return nil
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
func walletKeysOf(entries []ledger.Entry) []WalletKey {
	var out []WalletKey
	for _, e := range entries {
		if e.Account.Type() != ledger.AccountUser {
			continue
		}
		id, err := strconv.ParseInt(e.Account.ID(), 10, 64)
		if err != nil {
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
