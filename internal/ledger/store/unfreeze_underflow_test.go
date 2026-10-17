package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件钉住解冻路径遇到不一致时的行为：**报错，不夹零**。
//
// 两条解冻语句（`Release` 与 `Save` 内部的 `releaseFrozen`）曾经用
// `GREATEST(frozen - <预留额>, 0)` 把「frozen 已小于 pending 预留之和」静默夹成 0，
// 连唯一的证据一起抹掉。现在负值撞上 `user_wallets` 的 CHECK，由
// unfreezeFailure 翻译成 ErrFrozenUnderflow。**别把 GREATEST 加回去**——
// 加回去这两条用例就会失败，那正是它们的作用。
//
// 需要 LARKAX_DB_DSN，没有时跳过（与其余集成测试相同）。

// pendingReserved 汇总某用户处于 pending 的预留之和——即 frozen 的权威值。
//
// 与 `ledgerBalance` 对称：那个从账本算权威余额，这个从预留记录算权威冻结额。
//
// 返回 money.Amount 而不是字符串：numeric 的文本形态会随值变化
// （`30.00000000` 与 `0` 都是合法的 numeric 文本），按文本比较会把
// 「两边相等」这件事变成一个格式问题。金额的比较要落在定点数上。
func pendingReserved(t *testing.T, db *sql.DB, userID int64) money.Amount {
	t.Helper()
	var total string
	err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(reserved_amount), 0)::text
		  FROM billing_requests
		 WHERE user_id = $1 AND reserved_asset = 'CNY' AND status = 'pending'`,
		userID).Scan(&total)
	if err != nil {
		t.Fatalf("汇总 pending 预留失败: %v", err)
	}
	amt, err := money.Parse(total)
	if err != nil {
		t.Fatalf("解析汇总金额 %q 失败: %v", total, err)
	}
	return amt
}

// frozenGap 是「快照冻结额 − 预留之和」。非零即不变量被违反。
//
// 对账要发现的正是这个量。下面两条用例断言它在解冻失败后**仍留在原地**——
// 夹零会把它抹平成 0，而 0 意味着对账判定一致。
func frozenGap(t *testing.T, db *sql.DB, userID int64) money.Amount {
	t.Helper()
	_, frozen := walletOf(t, db, userID)
	snapshot, err := money.Parse(frozen)
	if err != nil {
		t.Fatalf("解析 frozen %q 失败: %v", frozen, err)
	}
	return snapshot.Sub(pendingReserved(t, db, userID))
}

// requestStatus 读一次预留记录的状态与最终金额。
func requestStatus(t *testing.T, db *sql.DB, requestID string) (status string, finalAmount sql.NullString) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT status, final_amount::text FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&status, &finalAmount)
	if err != nil {
		t.Fatalf("读预留记录失败: %v", err)
	}
	return status, finalAmount
}

// assertFrozenUnderflow 检查错误链同时满足三件事。
//
// 三件事都要：分类能单独判定（ErrFrozenUnderflow）、与既有判断兼容
// （链里仍有 ErrInsufficientBalance）、原始驱动错误不丢（SQLSTATE 与 DETAIL
// 都在 *pgconn.PgError 上）。少任何一件都会让调用方少一条可用的信息。
func assertFrozenUnderflow(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("解冻会让 frozen 变成负数，应当失败而不是夹到零")
	}
	if !errors.Is(err, ErrFrozenUnderflow) {
		t.Errorf("期望 ErrFrozenUnderflow，得到 %v", err)
	}
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Errorf("错误链里应当还有 ErrInsufficientBalance，得到 %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("错误链里应当留着原始 *pgconn.PgError，得到 %v", err)
	} else if pgErr.Code != pgCheckViolation {
		t.Errorf("SQLSTATE = %s，期望 %s", pgErr.Code, pgCheckViolation)
	}
	// 报出来的错误要说得清是什么不一致——「解冻失败」四个字不足以
	// 让人知道去查冻结额。结算那条尤其要钉住：`applyToWallets` 的余额
	// 不足走的是另一个分支，只断言 errors.Is 的话两边都会通过。
	if !strings.Contains(err.Error(), "冻结额变成负数") {
		t.Errorf("错误信息应当说清原因，得到 %q", err)
	}
}

// Release 走 store.go 里 `Release` 那条解冻语句。
func TestReleaseRefusesFrozenUnderflow(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(t, db, userID, requestID)

	// 1 · 正常预留 30：frozen 应当等于预留之和 30
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "30.00000000" {
		t.Fatalf("预留后 frozen = %s，期望 30", frozen)
	}

	// 2 · 制造分歧：把 frozen 改小，模拟「有另一条路径动了冻结额」。
	//     此刻 frozen(10) < 预留之和(30)，差额 20。
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 10 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if before := frozenGap(t, db, userID); before.Cmp(money.MustParse("-20")) != 0 {
		t.Fatalf("释放前的差额 = %s，期望 -20（frozen 10 比预留之和 30 少 20）", before)
	}

	// 3 · 释放必须失败：夹零会把差额抹掉，所以这里不能静默。
	assertFrozenUnderflow(t, s.Release(ctx, req))

	// 4 · 整个事务回滚：快照没被改成 0（也没被扣成 -20），
	//     预留记录仍停在 pending——释放没有发生。
	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("失败时余额不该变: %s", balance)
	}
	if frozen != "10.00000000" {
		t.Errorf("失败时冻结额不该变（夹零会变成 0）: %s", frozen)
	}
	if status, final := requestStatus(t, db, requestID); status != "pending" || final.Valid {
		t.Errorf("失败时预留记录不该被改动，得到 status=%s final_amount=%v", status, final)
	}

	// 5 · **证据仍在**：差额还是 −20，对账跑起来看得见这次不一致。
	if after := frozenGap(t, db, userID); after.Cmp(money.MustParse("-20")) != 0 {
		t.Errorf("释放后的差额 = %s，期望仍为 -20；若归零说明差额又被抹掉了", after)
	}
}

// 同一条解冻的另一处：结算路径。
//
// Save 内部在 applyToWallets 之后调 releaseFrozen。
//
// 与 Release 那条不同，这里的失败要连着**已经写下的分录**一起回滚——
// 结算的一整套动作在同一个事务里，任何一步失败都不该留下半步。
func TestSettleRefusesFrozenUnderflow(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(t, db, userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	// 制造分歧：frozen(10) < 预留之和(30)
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 10 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	assertFrozenUnderflow(t, s.Save(ctx, req, settledCharge(t, userID, requestID, "10")))

	// 回滚：余额没扣、冻结额没动、账本里没有这次结算的分录。
	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("失败时余额不该变: %s", balance)
	}
	if frozen != "10.00000000" {
		t.Errorf("失败时冻结额不该变（夹零会变成 0）: %s", frozen)
	}
	// seed 造过一笔 100 的充值，所以期望值是充值额而不是零——
	// 按零断言会把「充值也没了」也当成通过。
	if got := ledgerBalance(t, db, userID); got.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("失败时账本不该有变化，期望仍是充值的 100，得到 %s", got)
	}
	if status, final := requestStatus(t, db, requestID); status != "pending" || final.Valid {
		t.Errorf("失败时预留记录不该被改动，得到 status=%s final_amount=%v", status, final)
	}

	// 证据仍在：对账看得见这 20 的差额。
	if after := frozenGap(t, db, userID); after.Cmp(money.MustParse("-20")) != 0 {
		t.Errorf("结算失败后的差额 = %s，期望仍为 -20；若归零说明差额被抹掉了", after)
	}
}

// settledCharge 造一次「扣掉 amount」的结算，形状与生产路径一致：
// 一组平衡的分录，外加 Settlement.Actual。
func settledCharge(t *testing.T, userID int64, requestID, amount string) *pipeline.Charge {
	t.Helper()
	amt, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析金额失败: %v", err)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: consumeEntries(t, userID, amount),
	}
	charge.Settlement.Actual = amt
	return charge
}
