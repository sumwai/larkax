package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件是一个**最小复现**，不是验收用的测试。
//
// 它钉住的是当前实现的错误行为，用来证明问题确实存在：
//
//	`user_wallets.frozen` 与 `billing_requests` 里 pending 预留之和
//	已经不等（frozen < 预留之和）时，解冻路径会**静默**把 frozen 夹到 0，
//	不返回任何错误，也不留下任何痕迹。
//
// # 为什么这是缺陷，而不只是保守写法
//
// 「夹零」在这里恰好把**唯一的证据**抹掉了：
//
//	夹之前：frozen = 10，pending 预留之和 = 30 → 差额 20，可被对账发现
//	夹之后：frozen = 0，pending 预留之和 = 0  → 不变量「frozen = 预留之和」成立
//
// 释放之后两边都归零，差额无处可查。这与 `store.go` 里
// `ErrInsufficientBalance` 的注释直接冲突——那里写着
// 「**报出来而不是夹到零**：夹零等于平台替用户承担差额，而账上完全看不出来」，
// 而同一文件的两个解冻函数（`Release` 与 `releaseFrozen`）正是夹零。
//
// # `GREATEST` 在这里的作用，比「保守」更严重
//
// 把两处 `GREATEST(..., 0)` 去掉（见下面的验证方法），负值会撞上
// `user_wallets` 的 `CHECK (balance >= 0 AND frozen >= 0)`，
// PostgreSQL 直接拒绝这次更新，错误会正常传播上来。
//
// 也就是说：**约束本来是守得住这道门的，而 `GREATEST` 恰好把那个错误
// 转成了静默成功。** 它不是在做兜底，它是在消音。
//
// 验证方法（不改工作区文件）：
//
//	cp internal/ledger/store/store.go /tmp/store_noclamp.go
//	# 把两处 GREATEST(...) 去掉
//	go test -overlay=<overlay.json> ./internal/ledger/store/ -run TestCurrentBehavior
//	# 两条用例都失败，报 SQLSTATE 23514（check 约束被违反）
//
// # 这些用例与前缀 TestCurrentBehavior 的关系
//
// 断言写的是**当前**行为，所以它会一直通过——直到有人修好夹零。
// 修好之后它会失败，那正是它的用途：**它是一个绊线，不是一条规范**。
// 因此：修 store.go 时请把这个文件整体删掉或改写成「应当报错」，
// 不要让一条断言错误行为的用例留在仓库里。
//
// 需要 LARKAX_DB_DSN，没有时跳过（与其余集成测试相同）。

// 造出「frozen < 预留之和」，然后调 Release。
//
// Release 走 store.go:282 那条 `GREATEST(frozen - $3, 0)`。
func TestCurrentBehavior_ReleaseClampsFrozenToZeroSilently(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

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
	before := frozenGap(t, db, userID)
	if before.Cmp(money.MustParse("-20")) != 0 {
		t.Fatalf("释放前的差额 = %s，期望 -20（frozen 10 比预留之和 30 少 20）", before)
	}

	// 3 · 释放。**当前实现返回 nil**——分歧被当成正常路径处理掉了。
	if err := s.Release(ctx, req); err != nil {
		t.Fatalf("当前实现应当静默返回 nil（这正是缺陷）；得到错误: %v", err)
	}

	// 4 · frozen 被夹到 0，而不是 10-30=-20，也不是报出差额 20。
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("frozen = %s，期望被夹到 0（这就是要记录的行为）", frozen)
	}

	// 5 · **证据消失**：释放后 frozen 与 pending 之和都是 0，差额归零。
	//     此刻跑对账会判定一致——那 20 元的差额没有任何地方记录过它存在。
	if after := frozenGap(t, db, userID); !after.IsZero() {
		t.Errorf("释放后的差额 = %s，期望 0；若不为 0 说明证据还在，这条复现就要重写", after)
	}
}

// 同一条夹零的另一处：结算路径。
//
// Save 内部在 applyToWallets 之后调 releaseFrozen（store.go:414），
// 那里也是 `GREATEST(w.frozen - b.reserved_amount, 0)`。
func TestCurrentBehavior_SettleClampsFrozenToZeroSilently(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	// 制造分歧：frozen(10) < 预留之和(30)
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 10 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	if err := s.Save(ctx, req, settledCharge(t, userID, requestID, "10")); err != nil {
		t.Fatalf("当前实现应当静默成功（这正是缺陷）；得到错误: %v", err)
	}

	// 余额按实际扣了 10；frozen 被夹到 0 而不是 -20，也没有任何报错。
	balance, frozen := walletOf(t, db, userID)
	if balance != "90.00000000" {
		t.Errorf("balance = %s，期望 90", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("frozen = %s，期望被夹到 0（这就是要记录的行为）", frozen)
	}
}

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

// frozenGap 是「快照冻结额 − 预留之和」。非零即 I3 被违反。
//
// 对账要发现的正是这个量。本文件的两条用例说明它如何被夹零抹平：
// 夹零之前它是 20，夹零之后是 0，中间没有任何错误、日志或表记录。
func frozenGap(t *testing.T, db *sql.DB, userID int64) money.Amount {
	t.Helper()
	_, frozen := walletOf(t, db, userID)
	snapshot, err := money.Parse(frozen)
	if err != nil {
		t.Fatalf("解析 frozen %q 失败: %v", frozen, err)
	}
	return snapshot.Sub(pendingReserved(t, db, userID))
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
