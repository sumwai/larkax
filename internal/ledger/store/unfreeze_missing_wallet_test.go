package store

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件钉住解冻路径的另一半：**钱包行不存在时必须报错**。
//
// 与 unfreeze_underflow_test.go 那条互补。那条压的是「frozen 会变负」——
// 语句执行了，被 CHECK 拒绝；这条压的是「语句根本没碰到行」——
// `UPDATE ... WHERE user_id = ... AND asset = ...` 影响 0 行，而 SQL 对 0 行
// 不报错。不主动看 RowsAffected 的话，一次解冻在账上表现为成功，
// 冻结额却一分未动，而预留记录已经被标成 released——那笔钱再也追不回来。
//
// 两条解冻语句各有一条用例：`Release` 与 `Save` 内部的 `releaseFrozen`。
//
// 需要 LARKAX_DB_DSN，没有时跳过（与其余集成测试相同）。

// Release 走 store.go 里 `Release` 那条解冻语句。
func TestReleaseRefusesMissingWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	// 1 · 正常预留 30，冻结额落在钱包行上
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	// 2 · 让钱包行消失。冻结额长在钱包行上，此刻这次释放已经无处可落。
	if _, err := db.ExecContext(ctx, `DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("删钱包失败: %v", err)
	}

	// 3 · 必须报错。「释放成功」而冻结额一分未动，是账上看不出来的失败。
	err := s.Release(ctx, req)
	if err == nil {
		t.Fatal("钱包行不存在时解冻应当报错，而不是静默成功（冻结额一分未动）")
	}
	if !errors.Is(err, ErrNoWallet) {
		t.Errorf("期望 ErrNoWallet，得到 %v", err)
	}

	// 4 · 回滚的证据：预留记录仍停在 pending。报错却把 status 改成 released
	//     的话，那条预留就再也不会被任何一次释放或结算碰到。
	if status, final := requestStatus(t, db, requestID); status != "pending" || final.Valid {
		t.Errorf("失败时预留记录不该被改动，得到 status=%s final_amount=%v", status, final)
	}
}

// 同一条解冻的另一处：结算路径，`Save` 内部在 applyToWallets 之后调 releaseFrozen。
//
// 与 Release 那条不同，这里的失败要连着**已经写下的分录**一起回滚——
// 结算的一整套动作在同一个事务里，任何一步失败都不该留下半步。
func TestSettleRefusesMissingWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("删钱包失败: %v", err)
	}

	entries := walletlessEntries(t, "10")
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: entries,
	}
	charge.Settlement.Actual = money.MustParse("10")

	err := s.Save(ctx, req, charge)
	if err == nil {
		t.Fatal("钱包行不存在时结算应当报错，而不是静默成功（冻结额一分未动）")
	}
	if !errors.Is(err, ErrNoWallet) {
		t.Errorf("期望 ErrNoWallet，得到 %v", err)
	}

	// 回滚：预留仍 pending
	if status, final := requestStatus(t, db, requestID); status != "pending" || final.Valid {
		t.Errorf("失败时预留记录不该被改动，得到 status=%s final_amount=%v", status, final)
	}
	// 回滚：insertEntries 写下的那两条分录也没了。
	// 按**这一笔的 txn_id** 数，不按科目数：库是多个用例共用的，
	// 按科目数会把别的用例（乃至上一次运行的残留）一起数进来。
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1`,
		string(entries[0].TxnID)).Scan(&n); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	if n != 0 {
		t.Errorf("失败时账本不该多出分录，实际多出 %d 条", n)
	}
}

// walletlessEntries 造一组**不涉及用户钱包**的平衡分录：平台收入 ↔ 上游成本。
//
// 为什么不能像别处那样用 consumeEntries：这条用例要打的是 releaseFrozen
// 那一步，而 Save 的顺序是 applyToWallets → releaseFrozen。分录一旦落在
// 用户科目上，applyToWallets 会先撞上「钱包行不存在」而失败（那也是一条
// 正确的拒绝，另有用例覆盖），releaseFrozen 根本走不到。
// 用这两个科目，前一步对钱包无话可说，失败点就只剩 releaseFrozen。
func walletlessEntries(t *testing.T, amount string) []ledger.Entry {
	t.Helper()
	amt, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析金额失败: %v", err)
	}
	txn := ledger.NewTxnID()
	return []ledger.Entry{
		{
			TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
			Amount: amt, EntryType: ledger.EntryConsume,
		},
		{
			TxnID: txn, Account: ledger.UpstreamCostAccount(), Asset: ledger.AssetCNY,
			Amount: amt.Neg(), EntryType: ledger.EntryConsume,
		},
	}
}
