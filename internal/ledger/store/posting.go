package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sumwai/larkax/internal/ledger"
)

// PostTransaction 把一组自洽的分录落库，并在同一个事务里更新钱包快照。
//
// 它是 `larkaxctl wallet credit` 这类**运营写入**的落库口：分录由
// internal/ledger 的构造函数（例如 Topup）造好，这里只负责「写进去」。
// 记账逻辑不在这里，也不该在这里——见 internal/ledger 的包注释。
//
// # 幂等由 txn_id 承担
//
// 同一个 txn_id 已经有分录时不再写入，返回 applied=false。调用方要幂等，
// 就把「同一个业务事件」映射到同一个 txn_id（命令行把 --order 确定性地
// 映射成一个 uuid）。**判据是 txn_id，不是备注**：备注会被人改、会过
// sanitizeMemo 的清洗，而 txn_id 才是账本里唯一标识一笔交易的字段。
//
// 重复到达时分两种，且两件事在运维上完全不同：
//
//   - 内容一致：这次是重放，返回 applied=false。调用方照常退出 0，
//     但要把「未改动」说出来，否则一次静默的空操作与一次真的入账
//     在输出上分不开（LESSONS 第 16 条）。
//   - 内容不同：ErrTxnConflict。同一个 txn_id 代表两笔不同的交易，
//     说明调用方把两个不同的事件映射到了同一个标识上。**不静默放过**：
//     放过会让第二笔钱在账面上消失，而账本本身仍然平，事后无从发现。
//
// # 并发
//
// 同一 txn_id 的并发写入由一条事务级 advisory lock 串行化。
// ledger_entries 上**没有** txn_id 的唯一约束（迁移里只有普通索引），
// 而「先查再插」中间有一个窗口：两个并发请求会各自看到「不存在」，
// 然后各写一组分录。那两组分录各自平，合起来也平（同一个 txn 里两组
// 一样的分录只会在校验时被当成重复），但对账与余额会各算两遍。
//
// 锁挂在 txn_id 上而不是某个调用方传入的键上：写库这一层只认 txn_id。
// 事务级锁随 commit / rollback 一起释放，不需要手工解锁。
//
// # 钱包行必须已经存在
//
// 写入前按固定顺序锁住涉及的钱包行；任何一行不存在都返回 ErrNoWallet，
// 而不是让 applyToWallets 把「没有这一行」误报成「余额不足」——后者是
// 用户欠费，前者是还没开户，运维的下一步动作完全不同。
//
// # Transaction.CreatedAt 目前不落库
//
// `insertEntries` 无条件写库上的 `now()`，本方法不读 `txn.CreatedAt`：
// 传了非零的 `At` 也不会让 `created_at` 变成那个时刻，落库的仍是当前时间，
// 而且不会报错。当前的调用方（手工入账）不设 `At`，所以这条命令上两者
// 一致；**将来拿这个入口补录一笔历史交易时要先改这里**——把 `At` 接进
// `insertEntries` 会同时改到结算那条写路径，那是另一件事，不该顺手做。
func (s *Store) PostTransaction(ctx context.Context, txn ledger.Transaction) (applied bool, err error) {
	if verifyErr := txn.Verify(); verifyErr != nil {
		return false, verifyErr
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("开启事务失败: %w", err)
	}
	// 已经 commit 的路径上 Rollback 返回 sql.ErrTxDone，忽略它。
	defer func() { _ = tx.Rollback() }()

	// 先给 txn_id 上锁，再读已有分录：读与写之间因此没有窗口。
	if _, lockErr := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(txn.TxnID)); lockErr != nil {
		return false, fmt.Errorf("锁定交易 %s 失败: %w", txn.TxnID, lockErr)
	}

	stored, err := txnEntriesInTx(ctx, tx, txn.TxnID)
	if err != nil {
		return false, err
	}
	if len(stored) > 0 {
		if !sameTxnContent(stored, txn.Entries) {
			return false, fmt.Errorf(
				"%w: 交易 %s 已经存在，但内容与这次要写的不同（库里 %d 条分录、这次 %d 条）",
				ErrTxnConflict, txn.TxnID, len(stored), len(txn.Entries))
		}
		// 重放：什么都不写，也不需要 commit。
		return false, nil
	}

	if err := lockPostingWallets(ctx, tx, txn.Entries); err != nil {
		return false, err
	}
	if err := insertEntries(ctx, tx, txn.Denormalize()); err != nil {
		return false, err
	}
	if err := applyToWallets(ctx, tx, txn.Entries); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("提交交易 %s 失败: %w", txn.TxnID, err)
	}
	return true, nil
}

// txnEntriesInTx 在一个事务里读回某个交易的全部分录。
//
// 用 entryColumns / scanEntry 与只读路径同一套解析：幂等比较必须拿
// 账本认的写法去比，而不是拿库里存的原文——旧写路径留下的 `user:042`
// 会被归一成 `user:42`，与这次构造的分录本来就该相等。
func txnEntriesInTx(ctx context.Context, tx *sql.Tx, txnID ledger.TxnID) ([]ledger.Entry, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM ledger_entries WHERE txn_id = $1 ORDER BY id`,
		string(txnID))
	if err != nil {
		return nil, fmt.Errorf("读交易 %s 的已有分录失败: %w", txnID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ledger.Entry
	for rows.Next() {
		e, raw, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("交易 %s 的已有分录读不出来（原文 %q）: %w", txnID, raw, scanErr)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历交易 %s 的已有分录失败: %w", txnID, err)
	}
	return out, nil
}

// sameTxnContent 判断库里那笔交易与这次要写的是不是同一批分录。
//
// 按多重集比较，不按顺序：分录的顺序不是账本的一部分（写库与读回都不
// 保证它），而金额、科目、方向与备注才是。
func sameTxnContent(stored, wanted []ledger.Entry) bool {
	if len(stored) != len(wanted) {
		return false
	}
	counts := make(map[string]int, len(stored))
	for _, e := range stored {
		counts[entryFingerprint(e)]++
	}
	for _, e := range wanted {
		key := entryFingerprint(e)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

// entryFingerprint 把一条分录压成可比较的字符串。
//
// 用 \x00 分隔：科目、资产、类型与备注都是人给的文本，用一个可能出现在
// 其中的字符（例如 `|`）拼接会让 `a|b` 与 `a` + `|b` 撞成同一个键。
func entryFingerprint(e ledger.Entry) string {
	return strings.Join([]string{
		e.Account.String(),
		string(e.Asset),
		e.Amount.String(),
		string(e.EntryType),
		fmt.Sprint(int64OrZero(e.BillingRequestID)),
		fmt.Sprint(int64OrZero(e.PayoutID)),
		e.Memo,
	}, "\x00")
}

// lockPostingWallets 按固定顺序锁住这次交易涉及的钱包行，并确认它们存在。
//
// 与 lockWallets 的差别只有一条：行不存在时报 ErrNoWallet，而不是
// 像那里一样跳过。跳过在结算路径上是对的（后续 UPDATE 会报出更具体的
// 原因），在这里不行——一次入账的正数金额不会让 UPDATE 失败，缺行会被
// applyToWallets 误报成「余额不足以扣 -100」。
func lockPostingWallets(ctx context.Context, tx *sql.Tx, entries []ledger.Entry) error {
	uniq := map[WalletKey]struct{}{}
	for _, k := range walletKeysOf(entries) {
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
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM user_wallets WHERE user_id = $1 AND asset = $2 FOR UPDATE`,
			k.UserID, k.Asset).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 用户 %d 没有 %s 钱包，先用 larkaxctl wallet create 开户",
				ErrNoWallet, k.UserID, k.Asset)
		}
		if err != nil {
			return fmt.Errorf("锁定钱包失败: %w", err)
		}
	}
	return nil
}
