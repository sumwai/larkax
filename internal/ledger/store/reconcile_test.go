package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/settings"
)

// ── 测试装置 ────────────────────────────────────────────────────────────

// noRepair 是「只报告」（默认）的配置。
//
// 全量扫描是刻意的：增量测试单独写一条，其余用例都希望一眼看到全部结果，
// 不必先猜「哪些行落在这次的时间窗里」。
func noRepair() ReconcileConfig {
	return ReconcileConfig{
		AutoRepair:        false,
		RepairMode:        RepairResyncSnapshot,
		DifferenceAccount: ledger.ReconcileDifferenceAccount(),
		ShardSize:         1000,
		FullScanInterval:  0,
	}
}

func withResyncRepair() ReconcileConfig {
	cfg := noRepair()
	cfg.AutoRepair = true
	return cfg
}

func withLedgerRepair() ReconcileConfig {
	cfg := withResyncRepair()
	cfg.RepairMode = RepairAdjustLedger
	return cfg
}

// findingsFor 挑出属于某个用户的不一致。
//
// 不按「总条数」断言：同一个数据库上跑着本包的其他用例，它们各自留下的
// 用户也在全量扫描的范围里。按用户过滤，用例之间就不会互相干扰。
func findingsFor(r *Report, userID int64) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.UserID == userID {
			out = append(out, f)
		}
	}
	return out
}

func kindOf(t *testing.T, r *Report, userID int64, kind FindingKind) Finding {
	t.Helper()
	for _, f := range findingsFor(r, userID) {
		if f.Kind == kind {
			return f
		}
	}
	t.Fatalf("用户 %d 上没有 %s 这类不一致，报告里是 %v", userID, kind, findingsFor(r, userID))
	return Finding{}
}

func hasKind(r *Report, userID int64, kind FindingKind) bool {
	for _, f := range findingsFor(r, userID) {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// countFindings 数出某一类不一致有几条。
//
// 与 Report 上那几个方法分开，而且必须分开：reconcile_runs 里那几列就是拿
// 它们写进去的（见 finishReconcileRun），所以「落库的条数对不对」这个问题的
// 期望值不能取同一个方法——两边一起被改坏时恒等，报表歪了也看不出来。
func countFindings(r *Report, kind FindingKind) int {
	n := 0
	for _, f := range r.Findings {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// countRepairedFindings 数出被修好的有几条。
//
// 理由与 countFindings 相同：Report.Repaired 既是数法也是落库的来源。
func countRepairedFindings(r *Report) int {
	n := 0
	for _, f := range r.Findings {
		if f.Repaired {
			n++
		}
	}
	return n
}

// malformedOf 挑出某个**科目原文**的畸形科目报告。
//
// 畸形科目没有 UserID（它归不到任何人头上），所以不能按用户找；
// 标识是科目原文（见 Finding.Scope）。
func malformedOf(t *testing.T, r *Report, account string) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Kind == FindingMalformedAccount && f.Key == account {
			return f
		}
	}
	t.Fatalf("没有 %q 的畸形科目报告，报告里是 %v", account, kindsOf(r))
	return Finding{}
}

func reconcile(t *testing.T, db *sql.DB, cfg ReconcileConfig) *Report {
	t.Helper()
	r, err := New(db).Reconcile(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	return r
}

// breakBalance 绕过 store 直接改余额——这正是对账要抓的那种写入路径。
func breakBalance(t *testing.T, db *sql.DB, userID int64, delta string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE user_wallets SET balance = balance + $2::numeric, updated_at = now()
		 WHERE user_id = $1 AND asset = 'CNY'`, userID, delta); err != nil {
		t.Fatalf("直接改余额失败: %v", err)
	}
}

// breakBalanceAhead 与 breakBalance 相同，但把 updated_at 写到**未来**。
//
// # 为什么增量用例需要它
//
// 增量的起点取自「上一次跑成的那一轮的开始时刻」。同一个库上可能有另一轮
// 对账在跑（多台机器、CI 与本地同时跑、变异测试起的那一遍），而它的开始
// 时刻可能晚于本用例写入的时刻——那几行就落在窗口之外，用例失败，而失败
// 原因与它想验证的东西无关。
//
// 把时间戳放到未来之后，任何以「现在或更早」为起点的增量都会带上这几行，
// 于是用例只依赖它自己的数据。代价是这个时间戳不真实，所以它只出现在
// **需要这种隔离**的那几条用例里，其余用例仍走 breakBalance。
func breakBalanceAhead(t *testing.T, db *sql.DB, userID int64, delta string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE user_wallets
		   SET balance = balance + $2::numeric, updated_at = now() + interval '1 hour'
		 WHERE user_id = $1 AND asset = 'CNY'`, userID, delta); err != nil {
		t.Fatalf("直接改余额失败: %v", err)
	}
}

func ledgerSumOf(t *testing.T, db *sql.DB, userID int64) money.Amount {
	t.Helper()
	return ledgerBalance(t, db, userID)
}

// ── 一致时什么都不报 ────────────────────────────────────────────────────

func TestReconcileReportsNothingWhenConsistent(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")

	report := reconcile(t, db, noRepair())

	if got := findingsFor(report, userID); len(got) != 0 {
		t.Fatalf("一致的钱包不该被报出来: %v", got)
	}
	if report.RunID == 0 {
		t.Error("应留下一条运行记录")
	}
	if !report.FullScan {
		t.Error("第一次运行没有起点可用，必须是全量")
	}
}

// 一次结算走正路（写分录 + 同步快照）之后，账本与快照仍然一致。
// 这条钉住的是「正常写入路径本身不会制造不一致」——它是下面所有
// 「改坏了要能查出来」用例的前提。
func TestReconcileAcceptsNormalSettlement(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "50")

	req := testRequest(ctx, t, db, userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}

	// 冻着的那 10 块是 frozen 的权威值；余额没动，账本也没动。
	report := reconcile(t, db, noRepair())
	if got := findingsFor(report, userID); len(got) != 0 {
		t.Fatalf("预留中的钱包不该被报出来: %v", got)
	}
}

// ── 余额不一致 ──────────────────────────────────────────────────────────

func TestReconcileDetectsBalanceMismatch(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "7")

	report := reconcile(t, db, noRepair())
	f := kindOf(t, report, userID, FindingBalanceMismatch)

	if f.Snapshot.Cmp(money.MustParse("107")) != 0 {
		t.Errorf("快照值 = %s，期望 107", f.Snapshot)
	}
	if f.Ledger.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("权威值 = %s，期望 100", f.Ledger)
	}
	if f.Diff().Cmp(money.MustParse("-7")) != 0 {
		t.Errorf("差额 = %s，期望 -7（权威值 − 快照值）", f.Diff())
	}
	if f.Repaired {
		t.Error("默认只报告，不该修")
	}
}

// 只报告时余额一动不动：这就是「不开开关就绝不动钱」这条承诺的实现。
func TestReconcileReportOnlyLeavesBalanceAlone(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "7")

	reconcile(t, db, noRepair())

	if balance, _ := walletOf(t, db, userID); balance != "107.00000000" {
		t.Errorf("余额 = %s，期望保持 107（只报告不修）", balance)
	}
}

// resync 口径：快照被重算成账本值，而**账本一个字都不加**。
//
// 这条是整个设计的核心断言：修正一个偏离的快照不需要分录，
// 因为账本从来没有记错。
func TestReconcileResyncsSnapshotWithoutWritingEntries(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "7")

	// 账本一个字都不该加：resync 修的是快照这个派生值，而账本从来没有
	// 记错。这里数的是**这个用户**的分录条数。
	before := entriesOfUser(t, db, userID)

	report := reconcile(t, db, withResyncRepair())
	f := kindOf(t, report, userID, FindingBalanceMismatch)

	if !f.Repaired {
		t.Error("开了自动修正就该被修")
	}
	if balance, _ := walletOf(t, db, userID); balance != "100.00000000" {
		t.Errorf("余额 = %s，期望被重算成 100", balance)
	}
	if got := entriesOfUser(t, db, userID); got != before {
		t.Errorf("账本分录数从 %d 变成 %d，resync 口径不该写分录", before, got)
	}

	// 修完之后再跑一轮，同一个用户不该再被报出来——否则「修好了」
	// 只是这一轮的假象。
	again := reconcile(t, db, withResyncRepair())
	if hasKind(again, userID, FindingBalanceMismatch) {
		t.Error("修完之后还是不一致，说明修正没有生效")
	}
}

// adjust_ledger 口径：写一条调账分录把账本对齐到快照，快照不动。
//
// 与 resync 相反的方向，用来钉住「换一个口径，行为真的跟着换」。
func TestReconcileAdjustLedgerMovesLedgerToSnapshot(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "7")

	before := entriesOfUser(t, db, userID)

	report := reconcile(t, db, withLedgerRepair())
	f := kindOf(t, report, userID, FindingBalanceMismatch)
	if !f.Repaired {
		t.Fatal("开了自动修正就该被修")
	}

	if balance, _ := walletOf(t, db, userID); balance != "107.00000000" {
		t.Errorf("余额 = %s，期望保持 107（快照为准）", balance)
	}
	// 一次调账写两条分录：一条落在被调的用户科目上，一条落在对家科目上。
	// 数的是**这个用户**的分录，不数全表——库里还有别的用例留下的行。
	// 快照比账本多 7，账本要补 7：借 user 7、贷对家 7。
	if got := entriesOfUser(t, db, userID); got != before+1 {
		t.Errorf("这个用户的分录数从 %d 变成 %d，期望 +1", before, got)
	}
	if counter, amount := counterOfLatestAdjustment(t, db, userID); amount.Cmp(money.MustParse("-7")) != 0 {
		t.Errorf("对家 %s 的金额 = %s，期望 -7", counter, amount)
	}
	// 账本被改到快照的值，于是两边一致了。
	if got := ledgerSumOf(t, db, userID); got.Cmp(money.MustParse("107")) != 0 {
		t.Errorf("账本汇总 = %s，期望 107", got)
	}

	again := reconcile(t, db, withLedgerRepair())
	if hasKind(again, userID, FindingBalanceMismatch) {
		t.Error("调账之后账本应当与快照一致")
	}
}

// entriesOfUser 数某个用户在账本里的分录条数。
//
// 按用户数而不是数全表：库里留着同一个测试库上其他用例的行，
// 而「全表多了两条」这种断言会被并行的写入路径弄成偶发失败。
func entriesOfUser(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()).Scan(&n); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	return n
}

// 差额必须落在指定的对家科目上，而不是混进平台收入。
//
// 这是 issue 里「被修正的钱从哪来」那一条的实现：对家可配置，默认单独归集。
//
// 断言盯的是**这个用户那一条调账分录的对家**，不是某个科目的全局余额增量：
// 同一个库上可能有另一轮对账在跑（换一台机器、另一次手跑、变异测试起的一轮），
// 它也会往同一个科目里记，全局增量就不是这条用例能独占的量了。
func TestReconcileDifferenceLandsOnConfiguredAccount(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "7")

	cfg := withLedgerRepair()
	cfg.DifferenceAccount = ledger.PlatformRevenueAccount()
	reconcile(t, db, cfg)

	counter, amount := counterOfLatestAdjustment(t, db, userID)
	if counter != ledger.PlatformRevenueAccount().String() {
		t.Errorf("对家科目 = %q，期望 %q", counter, ledger.PlatformRevenueAccount())
	}
	// 快照比账本多 7，账本要补 7 才追上：贷对家 7。
	if amount.Cmp(money.MustParse("-7")) != 0 {
		t.Errorf("对家金额 = %s，期望 -7", amount)
	}
}

// 默认口径下差额进的是 reconcile_difference，而不是 platform_revenue。
func TestReconcileDefaultDifferenceAccountIsDedicated(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "7")

	reconcile(t, db, withLedgerRepair())

	counter, amount := counterOfLatestAdjustment(t, db, userID)
	if counter != ledger.ReconcileDifferenceAccount().String() {
		t.Errorf("对家科目 = %q，期望 %q（默认不该记进平台收入）",
			counter, ledger.ReconcileDifferenceAccount())
	}
	if amount.Cmp(money.MustParse("-7")) != 0 {
		t.Errorf("对家金额 = %s，期望 -7", amount)
	}
}

// counterOfLatestAdjustment 返回某用户最新一条调账分录的对家科目与金额。
//
// 一次调账写两条分录、共用一个 txn_id，所以「对家」就是同一 txn 里
// 不属于这个用户科目的那一条。
func counterOfLatestAdjustment(t *testing.T, db *sql.DB, userID int64) (string, money.Amount) {
	t.Helper()
	var (
		account, amountText string
	)
	err := db.QueryRowContext(context.Background(), `
		SELECT counter.account, counter.amount::text
		  FROM ledger_entries own
		  JOIN ledger_entries counter
		    ON counter.txn_id = own.txn_id AND counter.account <> own.account
		 WHERE own.account = $1 AND own.entry_type = 'adjustment'
		 ORDER BY own.id DESC
		 LIMIT 1`, ledger.UserAccount(userID).String()).Scan(&account, &amountText)
	if err != nil {
		t.Fatalf("找调账分录的对家失败: %v", err)
	}
	amt, err := money.Parse(amountText)
	if err != nil {
		t.Fatalf("解析对家金额 %q 失败: %v", amountText, err)
	}
	return account, amt
}

// 账本值低于零（用户欠平台钱）时不能自动抹平：夹到零等于平台悄悄认下
// 差额，而那笔钱从此在任何报表上都看不见。
//
// 「账本比快照少」这条要造得能通过钱包的 CHECK——直接减余额会被
// wallet_non_negative 挡下。所以改成往账本里补一条负分录：那正是
// 「有代码路径只写了账本、没写快照」的形状。
func TestReconcileRefusesNegativeLedgerBalance(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "10")
	appendLedgerEntry(t, db, ledger.UserAccount(userID), "-25", ledger.EntryConsume)

	report := reconcile(t, db, withResyncRepair())
	f := kindOf(t, report, userID, FindingBalanceMismatch)

	if f.Ledger.Cmp(money.MustParse("-15")) != 0 {
		t.Fatalf("权威值 = %s，期望 -15", f.Ledger)
	}
	if f.Repaired {
		t.Error("负的账本值不该被自动修正")
	}
	if f.Unrepairable == "" {
		t.Error("要说清为什么不能修")
	}
	// 快照一动没动：夹到零等于平台认下那 25 块。
	if balance, _ := walletOf(t, db, userID); balance != "10.00000000" {
		t.Errorf("余额 = %s，期望保持 10", balance)
	}
}

// ── 冻结不一致 ──────────────────────────────────────────────────────────

func TestReconcileDetectsFrozenMismatch(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")

	req := testRequest(ctx, t, db, userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	// 直接改冻结额：模拟一条绕过 store 的写入路径。
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 50 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	report := reconcile(t, db, noRepair())
	f := kindOf(t, report, userID, FindingFrozenMismatch)

	// 权威值是未结算预留之和（30），不是快照里的 50。
	if f.Ledger.Cmp(money.MustParse("30")) != 0 {
		t.Errorf("权威值 = %s，期望 30", f.Ledger)
	}
	if f.Snapshot.Cmp(money.MustParse("50")) != 0 {
		t.Errorf("快照值 = %s，期望 50", f.Snapshot)
	}
}

// 释放之后冻结额归零，两边重新一致——「解冻」这条路径不会制造
// 永久性的不一致。
func TestReconcileAcceptsReleasedReservation(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")

	req := testRequest(ctx, t, db, userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatal(err)
	}

	report := reconcile(t, db, noRepair())
	if got := findingsFor(report, userID); len(got) != 0 {
		t.Fatalf("释放之后不该还有不一致: %v", got)
	}
}

// 冻结的修正不做多重口径：权威值只有预留表一个，修正它不涉及资金流动。
func TestReconcileRepairsFrozen(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100")
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 50 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	report := reconcile(t, db, withResyncRepair())
	if !kindOf(t, report, userID, FindingFrozenMismatch).Repaired {
		t.Fatal("冻结额应当被修正")
	}
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("冻结额 = %s，期望被修正成 0", frozen)
	}
}

// 未结算预留之和为负时，这一条标成不可修，**整轮照跑**。
//
// # 为什么必须有这一格
//
// 负的和意味着冻结额该被改成负数。改下去的后果有两层：钱包上的
// wallet_non_negative 会直接拒掉这次 UPDATE，于是报错顺着
// compareAndRepair → reconcileUsers → Reconcile 一路上去，把**整轮**对账
// 带成 failed——同一批里别的用户那条本来能修的不一致也一起不修了；
// 而若哪天那个 CHECK 被去掉，负的冻结额就真的写进去了，那时
// 「可用 = 余额 − 冻结」会比余额还大。
//
// 所以判据必须是「这一条不修」，而不是「修不了就报错」。
func TestReconcileRefusesNegativePendingSum(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")

	// 先造一条正常的预留：冻结 30、预留 30，两边本来是平的。
	req := testRequest(ctx, t, db, userID, reqID(t))
	res := reservation(t, userID, req.RequestID, "30", "CNY")
	// started_at 留默认的「刚刚」，免得这条又被算成陈旧预留。
	if _, err := s.SaveReservation(ctx, req, res); err != nil {
		t.Fatal(err)
	}
	// 再把金额改成负的。reserved_amount 那一列上没有 CHECK，所以改得进去
	// ——正是这一点让「预留之和为负」真的可达。
	if _, err := db.ExecContext(ctx,
		`UPDATE billing_requests SET reserved_amount = -30 WHERE request_id = $1`,
		req.RequestID); err != nil {
		t.Fatal(err)
	}

	// 同一批里再放一个**能修**的冻结不一致：这一条挡住的只是不可修的那一条，
	// 不是整轮不做。没有它的话，「干脆整轮跳过冻结修正」也能让上面那条过。
	otherID := seed(t, db, "100")
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 50 WHERE user_id = $1`, otherID); err != nil {
		t.Fatal(err)
	}

	// 自动修正是打开的：不可修的那一条必须自己挡住，而不是撞上 CHECK。
	report := reconcile(t, db, withResyncRepair())

	f := kindOf(t, report, userID, FindingFrozenMismatch)
	if f.Unrepairable == "" {
		t.Error("预留之和为负是修不了的，这一条必须标成不可修")
	}
	if f.Repaired {
		t.Error("不可修的不一致不该被标成已修正")
	}
	if _, frozen := walletOf(t, db, userID); frozen != "30.00000000" {
		t.Errorf("冻结额 = %s，期望原样 30——被改成负数会让可用大于余额", frozen)
	}

	if !kindOf(t, report, otherID, FindingFrozenMismatch).Repaired {
		t.Error("可修的那一条要在同一轮里照修，不能被这一条带得整轮不做")
	}
	if _, frozen := walletOf(t, db, otherID); frozen != "0.00000000" {
		t.Errorf("另一个用户的冻结额 = %s，期望被修正成 0", frozen)
	}
}

// 不可修的条目根本不去碰库：判据要发生在任何一次往返**之前**。
//
// 传 nil 当 *sql.Tx——真去改库的话这里会 panic。这条用例因此同时钉住两件事：
// Unrepairable 非空时不修，以及这个判断在任何一次往返之前。它不需要
// LARKAX_DB_DSN（同 TestPartitionMonthsValidatedBeforeTouchingDB）。
//
// 与上一条的分工：上一条看的是最终行为（标不可修、整轮不报错），
// 这条看的是那一格判断本身；上一条要连库，这条不要。
func TestRepairFrozenTouchesNothingWhenUnrepairable(t *testing.T) {
	f := Finding{
		Kind:         FindingFrozenMismatch,
		Ledger:       money.MustParse("-30"),
		Snapshot:     money.MustParse("30"),
		Unrepairable: "未结算预留之和为负",
	}
	key := userAsset{userID: 1, asset: "CNY"}

	if err := repairFrozen(context.Background(), nil, withResyncRepair(), &f, key); err != nil {
		t.Fatalf("不可修的条目应当直接返回，得到 %v", err)
	}
	if f.Repaired {
		t.Error("不可修的条目不该被标成已修正")
	}
}

// ── 缺钱包行与畸形科目 ──────────────────────────────────────────────────

// 有分录却没有钱包行：报出来，且**不新建**。
// 新建要猜默认资产与用户是否存在，那不是对账该猜的事。
func TestReconcileReportsMissingWallet(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100")
	if _, err := db.ExecContext(ctx,
		`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	report := reconcile(t, db, withResyncRepair())
	f := kindOf(t, report, userID, FindingWalletMissing)

	if f.Ledger.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("权威值 = %s，期望 100", f.Ledger)
	}
	if f.Unrepairable == "" {
		t.Error("要说清为什么不修")
	}
	if !strings.Contains(f.Detail, "分录") {
		t.Errorf("Detail 要说清是账本那一侧缺的，得到 %q", f.Detail)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("对账不该凭空建钱包行")
	}
}

// 只有预留、没有钱包行：同样是缺钱包行，但 Detail 要指出缺的是哪一侧
// ——查的地方完全不同（还没开过户 vs 预留落了而钱包被删）。
func TestReconcileReportsWalletMissingForReservationOnly(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")
	req := testRequest(ctx, t, db, userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	report := reconcile(t, db, noRepair())
	f := kindOf(t, report, userID, FindingWalletMissing)

	if f.Ledger.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("账本值 = %s，期望 100", f.Ledger)
	}
	if !strings.Contains(f.Detail, "预留") {
		t.Errorf("Detail 要提到预留那一侧，得到 %q", f.Detail)
	}
	// 冻结的权威值（预留之和 30）也在报出来的这一条里：
	// 没有钱包行时余额与冻结都无从比对，不该只报其中一个。
	if !strings.Contains(f.Detail, "30.00000000") {
		t.Errorf("Detail 要带上未结算预留之和，得到 %q", f.Detail)
	}
}

// 畸形的 user 科目是对账的盲区：按 user:<id> 汇总永远汇总不到它。
// 它必须被单独报出来，否则那笔钱谁也不会看见。
func TestReconcileReportsMalformedAccount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100")

	// 科目名每次都不同：库里留着上一轮跑测试时的行，用固定的名字会让
	// 金额随着跑的次数累加，而断言就成了「跑了第几次」的函数。
	// 形态固定是 `user :<id>`——中间多一个空格是最典型的写法错误，
	// 而它要求判据是 `LIKE 'user%'` 而不是 `LIKE 'user:%'`。
	account := fmt.Sprintf("user :%d", nextID())
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
		VALUES (gen_random_uuid(), $1, 'CNY', 5, 'adjustment')`, account); err != nil {
		t.Fatal(err)
	}

	report := reconcile(t, db, noRepair())

	found := false
	for _, f := range report.Findings {
		if f.Kind == FindingMalformedAccount && strings.Contains(f.Detail, account) {
			found = true
			if f.Ledger.Cmp(money.MustParse("5")) != 0 {
				t.Errorf("金额 = %s，期望 5", f.Ledger)
			}
			if f.UserID != 0 {
				t.Errorf("畸形科目解析不出用户 id，UserID 应为 0，得到 %d", f.UserID)
			}
		}
	}
	if !found {
		t.Fatalf("畸形的 user 科目应当被报出来，报告里的类别有 %v", kindsOf(report))
	}

	// 这个条数要落进运行记录：畸形科目是那笔钱唯一的去处，混进
	// balance_mismatches 等于把它藏起来。期望值从 Findings 现数——那一列就是
	// 拿 report 的方法写进去的（见 finishReconcileRun），两边比等于自己跟
	// 自己比。不写字面量，是因为库里还留着别的用例插进去的坏科目，这一轮数
	// 出来的条数会跟着变。
	var malformedCount int
	if err := db.QueryRowContext(ctx,
		`SELECT malformed_accounts FROM reconcile_runs WHERE id = $1`, report.RunID).
		Scan(&malformedCount); err != nil {
		t.Fatal(err)
	}
	if want := countFindings(report, FindingMalformedAccount); malformedCount != want {
		t.Errorf("落库的畸形科目数 = %d，Findings 里数出 %d", malformedCount, want)
	}
	if malformedCount == 0 {
		t.Error("报了畸形科目就该在运行记录上留个非零的数")
	}

	// 反证：删掉之后它必须消失。不然上面那条断言可能只是撞上了别的
	// 用例留下的坏科目，而刚插进去的那条其实没被看见。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM ledger_entries WHERE account = $1`, account); err != nil {
		t.Fatal(err)
	}
	again := reconcile(t, db, noRepair())
	for _, f := range again.Findings {
		if f.Kind == FindingMalformedAccount && strings.Contains(f.Detail, account) {
			t.Fatal("删掉之后还在报，说明报的不是刚插进去的那一条")
		}
	}

	if hasKind(report, userID, FindingBalanceMismatch) {
		// 畸形科目属于别人（甚至不属于任何用户），不该把正常用户带成
		// 一条假的余额不一致。
		t.Error("正常用户不该因为别人的畸形科目被报成不一致")
	}
}

// 已经记下的畸形科目，要在没人动它的情况下被下一轮增量带回来。
//
// # 这条钉的是「凭什么还看得见它」
//
// 增量查询只看上次起点之后新增的行，而一条早就存在、又没人处理的畸形科目
// 不会再新增——它之所以还在这一轮的 Findings 里，靠的是把已记未消除的那些
// 科目名一起查回来。少了那一步，Findings 里就没有它，而 persistFindings
// 会把「这一轮没报出来的」当成「已经解决了」而删掉那一行：告警响一轮就
// 永久安静，而那笔钱还挂在那里等着有人认领。那比不告警更糟——它看起来像
// 已经处理过了。
//
// 与 TestReconcileReportsMalformedAccount 的分工：那条管「第一次被看见」，
// 它跑第二轮之前先把坏科目**删掉**；这条一个字节都不动。
func TestReconcileCarriesMalformedAccountIntoIncrementalRound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// 科目名每次都不同，理由与 TestReconcileReportsMalformedAccount 相同。
	account := fmt.Sprintf("user :%d", nextID())
	scope := malformedScopePrefix + account
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM ledger_entries WHERE account = $1`, account)
		_, _ = db.ExecContext(ctx, `DELETE FROM reconcile_findings WHERE scope = $1`, scope)
	})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
		VALUES (gen_random_uuid(), $1, 'CNY', 5, 'adjustment')`, account); err != nil {
		t.Fatal(err)
	}

	// 第一轮是全量：发现它，并落进 reconcile_findings。
	malformedOf(t, reconcile(t, db, noRepair()), account)

	// 第二轮是增量，而这个畸形科目一个字节都没动过：它的 created_at 早于
	// 这一轮的起点，只有把已记下的科目名带回来才查得到。
	cfg := noRepair()
	cfg.FullScanInterval = 24 * time.Hour
	again := reconcile(t, db, cfg)
	if again.FullScan {
		t.Fatal("这一轮该是增量")
	}
	malformedOf(t, again, account)

	// 还没消除的不一致必须留在清单里：那一行被删掉，告警就永远停了。
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM reconcile_findings WHERE scope = $1 AND kind = $2`,
		scope, string(FindingMalformedAccount)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("还没消除的不一致必须留在 reconcile_findings 里（期望 1 行），实际 %d 行", n)
	}
}

// 非规范写法的 user 科目：那笔钱**属于**那个用户。
//
// 写路径把后缀读成整数，所以 `user:0<id>` 这笔钱当初就加进了那个用户的钱包；
// 而对账的汇总按字符串逐字相等去认，看不见它——于是快照（150）比它算出的
// 权威值（100）大，对账判定「快照多了」，在 auto_repair 打开时把**正确的**
// 余额改小 50。方向是用户余额减少，不是增加。
//
// 这条用例盯的就是那 50 不被改掉：钱按同一个用户归位，而那种写法本身
// 另外报出来（新写入已经产生不了它，库里还留着的那条要有人去订正）。
func TestReconcileKeepsAliasAccountBalance(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100")

	// 别名科目与 user:<id> 指向同一个用户。构造与「写路径会写出什么」
	// 一致：applyToWallets 把后缀读成整数，所以这 50 也在钱包里。
	alias := "user:0" + itoa(userID)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM ledger_entries WHERE account = $1`, alias)
	})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
		VALUES (gen_random_uuid(), $1, 'CNY', 50, 'topup')`, alias); err != nil {
		t.Fatal(err)
	}
	breakBalance(t, db, userID, "50") // 钱包 = 150，正是写路径会写出来的值

	// 先复现缺陷成立的前提：逐字相等的那种汇总（改前的口径）只看得见 100。
	// 权威值比快照小 50，而对账要「把快照对齐到权威值」——这 50 就是这么丢的。
	var exactSum string
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)::text
		  FROM ledger_entries WHERE account = $1 AND asset = 'CNY'`,
		ledger.UserAccount(userID).String()).Scan(&exactSum); err != nil {
		t.Fatal(err)
	}
	if exactSum != "100.00000000" {
		t.Fatalf("逐字相等的汇总 = %s，期望 100——这条用例要复现的正是它偏小", exactSum)
	}

	report := reconcile(t, db, withResyncRepair())

	if hasKind(report, userID, FindingBalanceMismatch) {
		t.Errorf("别名科目的钱属于这个用户，不该报成余额不一致：%v",
			findingsFor(report, userID))
	}
	if balance, _ := walletOf(t, db, userID); balance != "150.00000000" {
		t.Errorf("余额 = %s，期望 150 一动不动（改前这里会变成 100）", balance)
	}

	f := malformedOf(t, report, alias)
	if f.Ledger.Cmp(money.MustParse("50")) != 0 {
		t.Errorf("报出来的金额 = %s，期望 50", f.Ledger)
	}
	if !strings.Contains(f.Detail, ledger.UserAccount(userID).String()) {
		t.Errorf("Detail 要说清这笔钱按哪个科目归位了，得到 %q", f.Detail)
	}
	// 能归位的这一类要带上用户 id：这条不一致说的就是「某个用户名下有一笔
	// 写法要订正的钱」，按用户查 reconcile_findings 时应当能查到它。
	// 归不到任何人的那一类才留 0（见 TestReconcileReportsZeroUserAccount）。
	if f.UserID != userID {
		t.Errorf("能归位的畸形科目 UserID = %d，期望 %d", f.UserID, userID)
	}
}

// 归一化表达式**必须**与写法判据一起用：单用它会把钱算到错的人头上。
//
// 表达式只做字符串处理，`split_part(account, ':', 2)` 取的是第一个冒号之后
// 的全部，于是**别的科目**归一之后与 `user:<id>` 长得一模一样——实测
// `merchant_payable:42`、`payout:42`、`payment_gateway:42` 都归成 `user:42`；
// `user :42`（分隔符前多一个空格）与 `user:42:9`（多余的分隔符）也一样。
// 是 userAccountAliasPattern 把它们挡在查询之外的。
//
// 少了那个判据的表现是**余额被抬高**：别人的钱被算进这个用户的权威值，
// 权威值比快照大，resync 就会把余额改大。方向与主线缺陷相反，但同样是
// 「对账自己把钱改了」，所以这一条也要钉住。
func TestLedgerSumsOnlyCountUserAccounts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100") // 账本上 user:<id> 是 100，钱包也是 100

	// 后缀与这个用户同一个 id，但都不是 user 科目——归一之后都会变成 user:<id>。
	noise := []struct{ account, amount string }{
		{"merchant_payable:" + itoa(userID), "7"},
		{"payout:" + itoa(userID), "11"},
		{"payment_gateway:" + itoa(userID), "13"},
		{"user :" + itoa(userID), "17"},
		{"user:" + itoa(userID) + ":9", "19"},
	}
	t.Cleanup(func() {
		for _, n := range noise {
			_, _ = db.ExecContext(ctx,
				`DELETE FROM ledger_entries WHERE account = $1`, n.account)
		}
	})
	for _, n := range noise {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
			VALUES (gen_random_uuid(), $1, 'CNY', $2::numeric, 'adjustment')`,
			n.account, n.amount); err != nil {
			t.Fatal(err)
		}
	}

	report := reconcile(t, db, withResyncRepair())

	if hasKind(report, userID, FindingBalanceMismatch) {
		t.Errorf("别的科目的钱被算进了这个用户，余额会被抬高：%v",
			findingsFor(report, userID))
	}
	if balance, _ := walletOf(t, db, userID); balance != "100.00000000" {
		t.Errorf("余额 = %s，期望 100 一动不动", balance)
	}

	// 归一只看后缀、认不出大类，所以判据里的那两种畸形也不能被它「归位」：
	// 它们归一之后也是 user:<id>，但那笔钱谁也不属于。
	for _, account := range []string{
		"user :" + itoa(userID),
		"user:" + itoa(userID) + ":9",
	} {
		if f := malformedOf(t, report, account); f.UserID != 0 {
			t.Errorf("科目 %q 的钱不属于任何人，UserID 应为 0，得到 %d", account, f.UserID)
		}
	}
}

// `user:0` 曾经是整体盲区：旧判据把它当成规范写法（所以不算畸形），
// 而候选查询是 `user_id > 0`，于是它既不被比对、也不被报出来——
// 那笔钱在任何地方都看不见。
//
// 用户 id 从 1 起（users.id 是 bigserial，钱包行还有外键），0 不是任何用户，
// 所以这条写法现在按畸形报出来。它也不可能由 store 写出来：钱包行不存在，
// applyToWallets 会「影响 0 行」而拒绝整笔。
//
// 报出来的**话**也要与这个事实一致：`user:0` 与 `user:abc` 同类（钱谁也不
// 属于），不是「写法要订正、钱已经归位」那类。后者会让处置的人以为钱找着了。
func TestReconcileReportsZeroUserAccount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	seed(t, db, "1") // 让这一轮有别的用户可跑

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM ledger_entries WHERE account = 'user:0'`)
	})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
		VALUES (gen_random_uuid(), 'user:0', 'CNY', 999, 'adjustment')`); err != nil {
		t.Fatal(err)
	}

	f := malformedOf(t, reconcile(t, db, noRepair()), "user:0")
	if f.UserID != 0 {
		t.Errorf("畸形科目的 UserID 应为 0，得到 %d", f.UserID)
	}
	// 0 号用户不存在，这 999 谁也不属于。报成「写法非规范、金额已按 user:0
	// 归位」是与事实相反的说法，会让处置的人以为钱已经找着了。
	if !strings.Contains(f.Unrepairable, "归属无法确定") {
		t.Errorf("Unrepairable = %q，期望「归属无法确定」那一条", f.Unrepairable)
	}
	if strings.Contains(f.Detail, "归位") {
		t.Errorf("Detail = %q 不该声称金额已归位——它不属于任何人", f.Detail)
	}
}

// 「有 pending 预留、既没有钱包行也没有账本分录」的用户整体在候选集之外
// ——而还没开过户的用户恰好是这个形状。WalletMissing 那条分支存在的
// 意义正是把他报出来，所以候选集必须带上未结算预留这一侧。
func TestReconcileReportsWalletMissingForReservationWithoutOtherSides(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	req := testRequest(ctx, t, db, userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	// 抹掉另外两侧：钱包行与账本分录都不在，只剩这条 pending 预留。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()); err != nil {
		t.Fatal(err)
	}

	f := kindOf(t, reconcile(t, db, noRepair()), userID, FindingWalletMissing)
	if !strings.Contains(f.Detail, "预留") {
		t.Errorf("Detail 要指出缺的是预留那一侧，得到 %q", f.Detail)
	}
	if !strings.Contains(f.Detail, "30.00000000") {
		t.Errorf("Detail 要带上未结算预留之和，得到 %q", f.Detail)
	}
}

// moneyTokenPattern 抠出一段文字里的金额（`0.00000000` 这种形态）。
var moneyTokenPattern = regexp.MustCompile(`\d+\.\d{8}`)

// hasMoneyToken 判断这段文字里有没有这个金额，按**数字**比而不是子串。
//
// 子串判据在这里不成立：`30.00000000` 含 `0.00000000` 这段子串，于是
// 「文字里只写了预留、没写分录之和」会被 `strings.Contains` 判为通过。
func hasMoneyToken(text, want string) bool {
	for _, tok := range moneyTokenPattern.FindAllString(text, -1) {
		if tok == want {
			return true
		}
	}
	return false
}

// 落库之后，detail 里说的数要与同一行的两个数字列对得上。
//
// # 这条针对的是一个具体的矛盾
//
// wallet_missing 的 ledger 列只表示分录之和（见 00004_reconcile.sql 的列注释），
// 只有预留、没有分录时它是 0。Detail 从前只写「有未结算预留 30.00000000」，
// 读这一行的人对着 ledger 列的 0 分不清那 30 算没算进去——文字与数字列互相
// 矛盾，而数字列是给人和后续查询看的。
//
// 断言按数字比（见 hasMoneyToken）而不是子串，否则旧措辞也能通过这条用例。
func TestReconcileWalletMissingDetailMatchesColumns(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	req := testRequest(ctx, t, db, userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	// 只留未结算预留这一侧：钱包行与账本分录都不在。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()); err != nil {
		t.Fatal(err)
	}

	reconcile(t, db, noRepair())

	var snapshot, ledgerCol, detail string
	if err := db.QueryRowContext(ctx, `
		SELECT snapshot::text, ledger::text, detail FROM reconcile_findings
		 WHERE scope = $1 AND kind = $2`,
		fmt.Sprintf("user:%d@CNY", userID), string(FindingWalletMissing)).
		Scan(&snapshot, &ledgerCol, &detail); err != nil {
		t.Fatalf("这条不一致应当记在 reconcile_findings 里: %v", err)
	}

	// 前提：两个数字列都是 0，这条不一致的全部信息都在文字里。
	if snapshot != "0.00000000" || ledgerCol != "0.00000000" {
		t.Fatalf("前提是两列都为 0，得到 snapshot=%s ledger=%s", snapshot, ledgerCol)
	}

	// 分录之和要写出来，哪怕是 0：文字里没有它，读的人就没法把 ledger 列的
	// 0 与「有 30 的预留」对上。
	if !hasMoneyToken(detail, ledgerCol) {
		t.Errorf("Detail %q 里没有 ledger 列的值 %s，读的人对不上文字与列", detail, ledgerCol)
	}
	if !hasMoneyToken(detail, "30.00000000") {
		t.Errorf("Detail %q 里没有未结算预留之和 30.00000000", detail)
	}
	// 预留那一侧不在任何数字列里，文字必须说明这一点：不然它就是一个
	// 「列里找不到的数」，读的人只能猜它算没算进 ledger 列。
	if !strings.Contains(detail, "不在 ledger 列") {
		t.Errorf("Detail %q 要说清预留那一侧不在 ledger 列里", detail)
	}
	if !strings.Contains(detail, "钱包行不存在") {
		t.Errorf("Detail %q 要说明钱包行不存在——那是 snapshot 列为 0 的原因", detail)
	}
}

// ── 陈旧的 pending 预留 ────────────────────────────────────────────────

// staleAge 与 freshAge 是造「陈旧」与「还不到阈值」的预留时用的时长。
//
// 两边都不贴着阈值取，各留了六小时以上的余量：started_at 由**应用时钟**
// 写（SaveReservation 绑的是 RuleMatchedAt），判据由**数据库时钟**比
// （见 staleReservationAfter 的注释），两台机器之间的时钟偏差会直接从
// 余量里扣。贴着阈值取的话，测试机与库之间差几分钟就会让用例飘。
const (
	staleAge = staleReservationAfter + 6*time.Hour
	freshAge = staleReservationAfter / 2
)

// reserveAt 造一条预留，并让它的 started_at 落在 age 之前。
//
// started_at 由 SaveReservation 写成 RuleMatchedAt，所以这里在**写入之前**
// 就把它设好，而不是写完之后 UPDATE：走的是真实那条路。
//
// 必须显式设它。助手的默认值是 `time.Now()`（预留刚刚发生），而这里要测的
// 是**超过阈值**，所以得自己把时间拨回去。
func reserveAt(t *testing.T, s *Store, userID int64, amount string, age time.Duration) string {
	t.Helper()
	requestID := reqID(t)
	res := reservation(t, userID, requestID, amount, "CNY")
	res.RuleMatchedAt = time.Now().Add(-age)
	// 用 s.db 而不是另收一个参数：本包的测试与实现同包，Store 的句柄就在手边，
	// 多一个参数只会让每个调用点多传一次同一个值。
	if _, err := s.SaveReservation(context.Background(), testRequest(t.Context(), t, s.db, userID, requestID), res); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	return requestID
}

// 挂了一天以上的 pending 预留要被报出来。
//
// # 它与其余几类不是同一回事
//
// 那几类报的是「两边对不上」，这一条报的是「两边对得上，但这笔钱冻着没人
// 管」：frozen 与未结算预留之和恰好相等（钱冻在钱包上、预留记在预留表上，
// 两个都在），所以余额与冻结那两类一条都不会报，对账对得平、不报错。
// 要发现它只能另立判据。
func TestReconcileReportsStalePendingReservation(t *testing.T) {
	db := testDB(t)
	s := New(db)
	userID := seed(t, db, "100")
	requestID := reserveAt(t, s, userID, "30", staleAge)

	report := reconcile(t, db, noRepair())

	f := kindOf(t, report, userID, FindingStaleReservation)
	if f.Key != requestID {
		t.Errorf("Key = %q，期望请求标识 %q", f.Key, requestID)
	}
	if want := reservationScopePrefix + requestID; f.Scope() != want {
		t.Errorf("Scope = %q，期望 %q", f.Scope(), want)
	}
	// 金额装在 Ledger 里：这一类的两个数字列不成对（见 Finding 的字段
	// 注释），放在这一列是为了让「一共冻着多少」能直接由 SQL 汇总回答。
	if f.Ledger.Cmp(money.MustParse("30")) != 0 {
		t.Errorf("金额 = %s，期望 30", f.Ledger)
	}
	if !f.Snapshot.IsZero() {
		t.Errorf("这一类没有快照侧，Snapshot 应为零，实际 %s", f.Snapshot)
	}
	if f.Unrepairable == "" {
		// 「报出来等人处置」与「报出来又被修掉」处置完全不同，必须说清。
		t.Error("要写明为什么不自动释放")
	}
	if f.Repaired {
		t.Error("这一类只报不修")
	}

	// 只报**不释放**：那笔钱照旧冻着，那一行照旧 pending。
	//
	// 自动释放与「请求还在途」有竞态，误放一笔在途请求的预留会让冻结额
	// 低于实际待结算额（frozen 减到负数，被表上的 CHECK 拒绝）。
	if _, frozen := walletOf(t, db, userID); frozen != "30.00000000" {
		t.Errorf("冻结额 = %s，期望原样 30——这一类只报不释放", frozen)
	}
	if status, _ := requestStatus(t, db, requestID); status != "pending" {
		t.Errorf("预留状态 = %q，期望 pending（不该被自动放开）", status)
	}

	// 条数要落进运行记录：它既不是余额差额也不是冻结差额，混进那两项里
	// 等于把它藏起来（与 malformed_accounts 单列一列同一个理由）。
	var staleCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT stale_reservations FROM reconcile_runs WHERE id = $1`, report.RunID).
		Scan(&staleCount); err != nil {
		t.Fatal(err)
	}
	// 期望值从这一轮的 Findings 现数，不取 report.StaleReservations()：那一列
	// 就是拿它写进去的（见 finishReconcileRun），两边比等于自己跟自己比。
	if want := countFindings(report, FindingStaleReservation); staleCount != want {
		t.Errorf("落库的陈旧预留条数 = %d，Findings 里数出 %d", staleCount, want)
	}
	if staleCount == 0 {
		t.Error("报了就该记进运行记录")
	}
}

// 还不到阈值的预留不报。
//
// 与上一条配对，钉的是**阈值本身**：只测「旧的被报出来」的话，把阈值
// 改成零（每一条 pending 都报）也照样通过，而那会把在途的请求报成滞留。
// 这一类 finding 一旦开始误报，人就再也不看它了。
func TestReconcileIgnoresReservationWithinThreshold(t *testing.T) {
	db := testDB(t)
	s := New(db)
	userID := seed(t, db, "100")
	requestID := reserveAt(t, s, userID, "30", freshAge)

	report := reconcile(t, db, noRepair())

	if hasKind(report, userID, FindingStaleReservation) {
		t.Errorf("还不到阈值的预留不该被报成陈旧: %v", findingsFor(report, userID))
	}
	if status, _ := requestStatus(t, db, requestID); status != "pending" {
		t.Errorf("预留状态 = %q，期望 pending", status)
	}
}

// 陈旧预留要落进 reconcile_findings、每轮重新报出来，放开之后消失。
//
// # 为什么增量那一轮也要报
//
// 增量扫描只看「上次起点之后变动过的行」，而这条预留一个字节都没动过。
// 它之所以还在报告里，靠的是判据本身每轮重筛一遍全部 pending 行——这一条
// 钉的正是那件事真的接上了：判据、落库、被带回来、消失了就删掉。
func TestReconcileKeepsReportingStaleReservationUntilReleased(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")
	requestID := reserveAt(t, s, userID, "30", staleAge)

	reconcile(t, db, noRepair()) // 建立起点，让接下来这一轮是增量

	cfg := noRepair()
	cfg.FullScanInterval = 24 * time.Hour
	report := reconcile(t, db, cfg)
	if report.FullScan {
		t.Fatal("这一轮该是增量")
	}
	if !hasKind(report, userID, FindingStaleReservation) {
		t.Fatalf("增量那一轮把它丢了：%v", findingsFor(report, userID))
	}

	scope := reservationScopePrefix + requestID
	countRows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM reconcile_findings WHERE scope = $1 AND kind = $2`,
			scope, string(FindingStaleReservation)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countRows(); n != 1 {
		t.Fatalf("应当记在 reconcile_findings 里，实际 %d 行", n)
	}

	// 放开之后它必须从清单里消失，否则告警会永远响着。
	if err := s.Release(ctx, testRequest(ctx, t, db, userID, requestID), nil); err != nil {
		t.Fatal(err)
	}
	report = reconcile(t, db, cfg)
	if hasKind(report, userID, FindingStaleReservation) {
		t.Error("放开之后不该还报陈旧")
	}
	if n := countRows(); n != 0 {
		t.Errorf("放开之后必须从 reconcile_findings 里删掉，实际还剩 %d 行", n)
	}
}

// 打开自动修正也不会放开陈旧的预留。
//
// 修正只处理「两边对不上」的那几类，而这一类两边是对得上的——没有任何
// 一类修正会碰它。这一条把「只报不自动释放」钉在行为上，而不是只写在
// 注释里：误放一笔在途请求的预留会让冻结额低于实际待结算额。
func TestReconcileAutoRepairDoesNotReleaseStaleReservation(t *testing.T) {
	db := testDB(t)
	s := New(db)
	userID := seed(t, db, "100")
	requestID := reserveAt(t, s, userID, "30", staleAge)

	report := reconcile(t, db, withResyncRepair())

	f := kindOf(t, report, userID, FindingStaleReservation)
	if f.Repaired {
		t.Error("这一类不该被自动修掉")
	}
	if status, _ := requestStatus(t, db, requestID); status != "pending" {
		t.Errorf("预留状态 = %q，期望 pending——自动修正不该放开它", status)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "30.00000000" {
		t.Errorf("冻结额 = %s，期望原样 30", frozen)
	}
}

// 增量扫描也要看得见这类用户：候选集里新加的那一侧有自己的时间窗口
// （`started_at`），与钱包、账本两侧的窗口不是同一个，得单独钉一次。
//
// 时间戳放到未来，理由与 breakBalanceAhead 相同：增量窗口的起点取自
// 「最后一轮跑成的对账的开始时刻」，而同一个库上可能有另一轮对账在跑
// （变异测试就会另起一遍 `go test`），那几行就会落在窗口之外。
func TestReconcileIncrementalFindsReservationOnlyUser(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	reconcile(t, db, noRepair()) // 建立起点，让接下来这一轮是增量

	userID := seed(t, db, "100")
	req := testRequest(ctx, t, db, userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE billing_requests SET started_at = now() + interval '1 hour'
		 WHERE request_id = $1`, req.RequestID); err != nil {
		t.Fatal(err)
	}
	// 抹掉另外两侧，于是这个用户只能靠未结算预留那一侧进候选集。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()); err != nil {
		t.Fatal(err)
	}

	cfg := noRepair()
	cfg.FullScanInterval = 24 * time.Hour
	report := reconcile(t, db, cfg)
	if report.FullScan {
		t.Fatal("这一轮应当是增量")
	}
	kindOf(t, report, userID, FindingWalletMissing)
}

// 「规范写法」的定义写在两个地方：ledger.ParseAccount（Go）与畸形判据里的
// 正则（SQL）。这条用例把它们钉在一起。
//
// 两处漂开的时候，会出现「写路径拒绝的写法又不被判为畸形」这类缝——
// 而本条缺陷正是从那种缝里来的：正则放行了 `user:0<id>`，字符串比对却不认。
func TestCanonicalUserPatternMatchesParseAccount(t *testing.T) {
	db := testDB(t)

	forms := []string{
		"user:1", "user:42", "user:100000", "user:123456789012345678",
		"user:0", "user:00", "user:042", "user:+42", "user:+0042",
		"user:1234567890123456789", "user:0000000000000000000042",
		"user:abc", "user:", "user:1:2", "user :42", "user: 42", "user:-5", "user",
	}
	for _, s := range forms {
		var sqlCanonical, sqlAlias bool
		if err := db.QueryRowContext(context.Background(),
			`SELECT $1 ~ $2, $1 ~ $3`,
			s, userAccountCanonicalPattern, userAccountAliasPattern,
		).Scan(&sqlCanonical, &sqlAlias); err != nil {
			t.Fatal(err)
		}

		_, parseErr := ledger.ParseAccount(s)
		goAccepts := parseErr == nil

		if goAccepts != sqlCanonical {
			t.Errorf("写法 %q：ParseAccount 接受 = %v，SQL 认为规范 = %v（两处必须一致）",
				s, goAccepts, sqlCanonical)
		}
		if goAccepts && !sqlAlias {
			// 能写进账本的写法必须是汇总认得的写法，否则这笔钱会进不了
			// 权威值，对账就会把余额改小。
			t.Errorf("写法 %q：ParseAccount 接受但汇总认不出，这笔钱会漏出权威值", s)
		}
	}
}

// kindsOf 列出报告里出现过的类别，供失败信息用。
//
// 直接打印 Findings 会把几十条别的用例的明细铺满输出，而排查时真正
// 想知道的是「有没有这一类」。
func kindsOf(r *Report) []string {
	seen := map[FindingKind]int{}
	for _, f := range r.Findings {
		seen[f.Kind]++
	}
	out := make([]string, 0, len(seen))
	for k, n := range seen {
		out = append(out, fmt.Sprintf("%s×%d", k, n))
	}
	sort.Strings(out)
	return out
}

// ── 增量与全量 ──────────────────────────────────────────────────────────

// 全量跑过之后按增量跑：没动过的用户不再被比对。
func TestReconcileIncrementalSkipsUntouchedRows(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")

	// 先跑一轮全量建立起点。
	reconcile(t, db, noRepair())

	// 起点是上一轮的开始时刻，而上一轮没改过这个用户（它本来就一致），
	// 所以下一次增量不该再比它。
	cfg := noRepair()
	cfg.FullScanInterval = 24 * time.Hour
	report := reconcile(t, db, cfg)

	if report.FullScan {
		t.Fatal("距上次全量只有一瞬间，这一轮该是增量")
	}
	// 不断言 CheckedWallets 为零：库里留着别的用例留下的**未消除不一致**，
	// 而带回它们是有意的（见 TestReconcileCarriesOpenFindings）。能断言的
	// 是「这个没动过的用户本身没被报出来」。
	if got := findingsFor(report, userID); len(got) != 0 {
		t.Errorf("没动过的用户不该出现在增量结果里: %v", got)
	}
}

// 没被修掉的旧不一致必须每轮重新报出来。
//
// # 这条是针对一个真实的坑
//
// 增量扫描只看「上次起点之后变动过的行」。一条只报告、没被修掉的不一致
// （默认就是不修）本身不会再变动，于是下一轮增量根本不会再看它——
// 告警响一次就永久安静，而不一致还挂在那里。那比「不告警」更糟：
// 它看起来像已经处理过了。
//
// 这里造一个「有分录、没钱包行」的用户：它两侧的写入时间都在起点之前，
// 增量扫描**只有**在把已记未消除的不一致带回来时才看得见它。
func TestReconcileCarriesOpenFindings(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100")
	if _, err := db.ExecContext(ctx,
		`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	// 第一轮（全量）发现它。
	first := reconcile(t, db, noRepair())
	if !hasKind(first, userID, FindingWalletMissing) {
		t.Fatalf("第一轮应当发现缺钱包行，报告里有 %v", findingsFor(first, userID))
	}

	// 第二、三轮都是增量，而这个用户一个字节都没动过。
	cfg := noRepair()
	cfg.FullScanInterval = 24 * time.Hour
	for i := 0; i < 2; i++ {
		report := reconcile(t, db, cfg)
		if report.FullScan {
			t.Fatal("这一轮该是增量")
		}
		if !hasKind(report, userID, FindingWalletMissing) {
			t.Fatalf("第 %d 轮增量把还没解决的不一致丢了，报告里有 %v", i+2, findingsFor(report, userID))
		}
	}

	// 它第一次被看见的时刻要留住：那是「这笔钱从什么时候开始不对的」
	// 的答案，被每轮覆盖掉就等于没有。
	var firstSeen, lastSeen string
	if err := db.QueryRowContext(ctx, `
		SELECT first_seen_at::text, last_seen_at::text FROM reconcile_findings
		 WHERE scope = $1 AND kind = $2`,
		fmt.Sprintf("user:%d@CNY", userID), string(FindingWalletMissing)).
		Scan(&firstSeen, &lastSeen); err != nil {
		t.Fatalf("这条不一致应当记在 reconcile_findings 里: %v", err)
	}
	if firstSeen == lastSeen {
		t.Error("last_seen_at 要与 first_seen_at 分开：前者每轮刷新，后者只在首次写入")
	}

	// 修好之后（补上钱包行）它必须从表里消失，否则告警会永远响着。
	//
	// updated_at 同样放到未来：这条用例要验证的是「解决了就会被清掉」，
	// 而窗口起点由最后一轮对账的运行时刻决定。放未来之后，
	// 有没有别人同时在跑对账都不影响这件事。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_wallets (user_id, asset, balance, frozen, updated_at)
		VALUES ($1, 'CNY', 100, 0, now() + interval '1 hour')`,
		userID); err != nil {
		t.Fatal(err)
	}
	report := reconcile(t, db, cfg)
	if hasKind(report, userID, FindingWalletMissing) {
		t.Error("补上钱包行之后不该还报缺钱包")
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM reconcile_findings WHERE scope = $1`,
		fmt.Sprintf("user:%d@CNY", userID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("解决之后必须从 reconcile_findings 里删掉，否则告警永远响着")
	}
}

// 增量扫描要抓住**这场运行之前**刚变动过的行——这正是它存在的理由。
//
// 同样用 breakBalanceAhead：这条用例的全部意义就是「刚变动的行会被看到」，
// 而窗口起点由外部的对账运行决定。时间戳放到未来之后，「刚变动」这件事
// 不再取决于此刻有没有别人在跑对账。
func TestReconcileIncrementalCatchesFreshChange(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")

	reconcile(t, db, noRepair()) // 建立起点
	breakBalanceAhead(t, db, userID, "3")

	cfg := noRepair()
	cfg.FullScanInterval = 24 * time.Hour
	report := reconcile(t, db, cfg)

	if report.FullScan {
		t.Fatal("这一轮该是增量")
	}
	if !hasKind(report, userID, FindingBalanceMismatch) {
		t.Fatalf("刚被改过的行应当被增量发现，报告里有 %v", findingsFor(report, userID))
	}
}

// 距上次全量超过间隔时必须转回全量：增量看不见「两边都早就错着」
// 的行，定期全量是唯一的兜底。
func TestReconcileFallsBackToFullScan(t *testing.T) {
	db := testDB(t)
	seed(t, db, "100")

	reconcile(t, db, noRepair())

	cfg := noRepair()
	cfg.FullScanInterval = time.Nanosecond // 等效于「每次都超期」
	report := reconcile(t, db, cfg)

	if !report.FullScan {
		t.Error("超过全量间隔之后必须做全量")
	}
}

// 距上次全量**恰好等于** FullScanInterval 时要全量——这是那一行比较符的边界。
//
// # 为什么边界要单独钉
//
// 间隔的语义是「两次全量之间最长隔多久」（见 SettingFullScanIntervalHours 的
// 说明），所以到了间隔那一刻就该扫。写成 `>` 的话，恰好到点的那一轮仍然是
// 增量，而它下一轮会不会全量要看这一轮跑了多久——中间那段时间里「两边都早就
// 错着」的不一致继续看不见，而这类不一致只有全量能发现。上面那条
// TestReconcileFallsBackToFullScan 用 1 纳秒的间隔跑，离边界很远，`>` 与 `>=`
// 在那里没有分别。
//
// 两侧都断言：差恰好等于间隔时是全量，还差一纳秒时是增量。只钉一侧的话，
// 把这一支写成恒真（永远全量）也能过。
//
// 时钟是**传进去的**，不取 now()：要的正是「差恰好等于间隔」这个点。
// 上一轮那一行的时间戳放到未来，理由与 breakBalanceAhead 相同——lastDone 与
// lastFull 取的是全表里最晚的那两条，放到未来才不会踩到别的用例刚写下的行
// （变异测试会另起一遍 `go test`）。跑完把这一行删掉，后面的用例看不见它。
func TestReconcileStartFullScanBoundary(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	interval := 24 * time.Hour
	stamp := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Millisecond)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO reconcile_runs (started_at, watermark, full_scan, status)
		VALUES ($1, $1, true, 'done')`, stamp); err != nil {
		t.Fatalf("造上一轮的全量记录失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM reconcile_runs WHERE started_at = $1`, stamp)
	})

	cfg := noRepair()
	cfg.FullScanInterval = interval

	// 恰好到点：全量。顺带钉住起点仍是上一轮的**开始**时刻，不是这一轮的。
	watermark, full, err := s.reconcileStart(ctx, stamp.Add(interval), cfg)
	if err != nil {
		t.Fatalf("算起点失败: %v", err)
	}
	if !full {
		t.Error("距上次全量恰好等于间隔时必须全量——不然到点的那一轮会一直往后拖")
	}
	if !watermark.Equal(stamp) {
		t.Errorf("起点 = %v，期望上一轮的开始时刻 %v", watermark, stamp)
	}

	// 还差一纳秒：仍然是增量。
	if _, full, err = s.reconcileStart(
		ctx, stamp.Add(interval-time.Nanosecond), cfg); err != nil {
		t.Fatalf("算起点失败: %v", err)
	}
	if full {
		t.Error("没到间隔时不该全量")
	}
}

// 分片：分片大小小于待比对的用户数时，循环要把所有分片都走完。
//
// 用增量跑而不是全量：库里的用户会随每次跑测试累积，而全量 +
// 小分片意味着上千次查询——测试会从「证明循环没漏」变成「测机器有多快」。
// 增量只看到刚动过的那几行，于是分片数与要断言的用户数直接对应。
//
// 写入用 breakBalanceAhead（时间戳放到未来）：增量窗口的起点取自「最后一轮
// 跑成的对账的开始时刻」，而同一个库上可能有另一轮对账在跑（变异测试就会
// 另起一遍 `go test`），那时窗口会被推后，这几行就落在外面。放到未来之后，
// 这条用例只依赖它自己的数据。
func TestReconcileShardsThroughAllUsers(t *testing.T) {
	db := testDB(t)

	reconcile(t, db, noRepair()) // 建立起点，让接下来这一轮是增量

	var users []int64
	for i := 0; i < 3; i++ {
		u := seed(t, db, "10")
		users = append(users, u)
		breakBalanceAhead(t, db, u, "1")
	}

	cfg := noRepair()
	cfg.FullScanInterval = 24 * time.Hour
	cfg.ShardSize = 1 // 每个分片只放一行，逼出多轮循环
	report := reconcile(t, db, cfg)

	if report.FullScan {
		t.Fatal("这一轮应当是增量")
	}
	if report.CheckedWallets < len(users) {
		t.Errorf("比对行数 = %d，期望至少 %d（分片循环漏了）", report.CheckedWallets, len(users))
	}
	for _, u := range users {
		if !hasKind(report, u, FindingBalanceMismatch) {
			t.Errorf("用户 %d 的不一致没被发现（分片循环漏了它）", u)
		}
	}
}

// ── 运行记录 ────────────────────────────────────────────────────────────

// 结局那一步自己失败时，Reconcile 必须把错误交出去。
//
// 这一支（reconcile.go 尾部 `s.finishReconcileRun(ctx, report, nil)` 后面的
// `err != nil`）此前被当成「防御性分支，没有输入造得出它来——别为它编一条用例」。
// 造得出：给 reconcile_runs 挂一个 BEFORE UPDATE 触发器就能让那次 UPDATE 失败，
// 而且是**确定性的**，不必真把连接弄断。
//
// 为什么不用取消 ctx：那一步刻意用 context.WithoutCancel + 自己的超时写
// （见 finishReconcileRun 的注释），为的就是在调用方的 ctx 已经失效时仍写得进去。
// 取消 ctx 走到的是**另一条**路（reconcileAll 返错，那一支本来就不看
// finish 的错误，见 TestReconcileClosesRunWhenContextCanceled），对它无效。
//
// 触发器只拦 status = 'done' 的那一次 UPDATE：一轮对账会碰 reconcile_runs 三处
// ——beginReconcileRun 先把僵死的 'running' 标成 'failed'、再插一行 'running'，
// 最后 finishReconcileRun 把它改成 'done'。只拦最后那一次，于是「结局写不回去」
// 是这一轮唯一的失败点，前面该查的、该报的都照常发生。
func TestReconcileReportsFailureToWriteRunOutcome(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	blockRunOutcomeWrite(t, db)

	report, err := New(db).Reconcile(ctx, noRepair(), nil)
	if err == nil {
		t.Fatal("结局写不回去时这一轮应当报错，而不是当成跑完了")
	}
	if report == nil {
		t.Fatal("失败也要返回已经查出来的部分")
	}

	// 那一行停在 'running'：这是 finishReconcileRun 的注释里写下的收尾
	// ——与硬崩同一种结果，由下一轮的 stale 清理（staleRunAfter 之后）捞走。
	// 写不回去就是写不回去，别在这里另找一个「看起来结掉了」的写法。
	var (
		status string
		closed bool
	)
	if err := db.QueryRowContext(ctx, `
		SELECT status, finished_at IS NOT NULL
		  FROM reconcile_runs WHERE id = $1`, report.RunID).Scan(&status, &closed); err != nil {
		t.Fatal(err)
	}
	if status != "running" || closed {
		t.Errorf("运行记录 status = %q、finished_at 已写 = %v，期望仍是一行没结掉的 running",
			status, closed)
	}
}

// blockRunOutcomeWrite 让「把这一轮的结局写进 reconcile_runs」那一次 UPDATE 失败。
//
// 用触发器而不是把连接弄断：注入的失败要**确定性**，且只落在这一个点上，否则
// 用例测的是网络而不是这条分支。
//
// 收尾走 t.Cleanup（本来就在动 schema 的 partition_test.go 也这么收）：库是同包
// 用例共用的，触发器留下来会让后面每一条对账用例都失败。开头先卸一次，是为了
// 上一次跑崩留下的那一份。
func blockRunOutcomeWrite(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	dropRunOutcomeWriteTrigger(ctx, t, db)
	if _, err := db.ExecContext(ctx, `
		CREATE FUNCTION reconcile_runs_block_outcome() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.status = 'done' THEN
				RAISE EXCEPTION '注入的失败：结局写不回去';
			END IF;
			RETURN NEW;
		END
		$$`); err != nil {
		t.Fatalf("建触发器函数失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER reconcile_runs_block_outcome
			BEFORE UPDATE ON reconcile_runs
			FOR EACH ROW EXECUTE FUNCTION reconcile_runs_block_outcome()`); err != nil {
		t.Fatalf("挂触发器失败: %v", err)
	}

	t.Cleanup(func() { dropRunOutcomeWriteTrigger(context.Background(), t, db) })
}

func dropRunOutcomeWriteTrigger(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`DROP TRIGGER IF EXISTS reconcile_runs_block_outcome ON reconcile_runs`); err != nil {
		t.Fatalf("卸触发器失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`DROP FUNCTION IF EXISTS reconcile_runs_block_outcome()`); err != nil {
		t.Fatalf("删触发器函数失败: %v", err)
	}
}

// 报告里那几个条数：数与验分开。
//
// 它们都会落进 reconcile_runs 的列（见 finishReconcileRun），而运维就是看
// 那几列判断账本还对不对。此前每一条断言都拿库里的值与写这个值的方法比：
// 方法被改坏时两边一起变，恒等，`n++` 写成 `n--` 也照样通过。这一条用手工
// 搭的一份报告与字面量比，期望值不与数法共用一句话。
//
// 畸形科目不在这张表里：它的计数不走「遍历 Findings」这一步，而是顺着扫描
// 逐个加（见 reconcileMalformedAccounts），所以要到真的扫出一个畸形科目才
// 动得了——那一格在 TestReconcileReportsMalformedAccount 里。
func TestReportCountsComeFromFindings(t *testing.T) {
	r := &Report{Findings: []Finding{
		{Kind: FindingBalanceMismatch, Repaired: true},
		{Kind: FindingBalanceMismatch},
		{Kind: FindingFrozenMismatch, Repaired: true},
		{Kind: FindingStaleReservation},
		{Kind: FindingStaleReservation},
		{Kind: FindingMalformedAccount},
	}}

	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"余额不一致", r.BalanceMismatches(), 2},
		{"冻结不一致", r.FrozenMismatches(), 1},
		{"陈旧预留", r.StaleReservations(), 2},
		{"被修好的", r.Repaired(), 2},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d，期望 %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestReconcileRunRecordsOutcome(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "5")

	report := reconcile(t, db, noRepair())

	var (
		status, watermark                                              string
		full                                                           bool
		checked, balMismatch, frozenMismatch, repaired, malformedCount int
		finishedNull                                                   bool
	)
	err := db.QueryRowContext(context.Background(), `
		SELECT status, full_scan, checked_wallets, balance_mismatches,
		       frozen_mismatches, repaired, malformed_accounts,
		       watermark::text, finished_at IS NULL
		  FROM reconcile_runs WHERE id = $1`, report.RunID).
		Scan(&status, &full, &checked, &balMismatch, &frozenMismatch,
			&repaired, &malformedCount, &watermark, &finishedNull)
	if err != nil {
		t.Fatalf("读运行记录失败: %v", err)
	}

	if status != "done" {
		t.Errorf("状态 = %q，期望 done", status)
	}
	if finishedNull {
		t.Error("跑完的运行记录必须有 finished_at")
	}
	if !full {
		t.Error("第一次运行应当记成全量")
	}
	if checked == 0 {
		t.Error("比对行数应当非零")
	}
	// 下面每一格的期望值都从这一轮的 report.Findings 现数，不取 Report 上那
	// 几个方法：那几列就是拿它们写进去的（见 finishReconcileRun），两边比
	// 等于自己跟自己比。
	wantBal := countFindings(report, FindingBalanceMismatch)
	if wantBal == 0 {
		t.Fatal("夹具动过一个用户的余额，这一轮应当报出余额不一致")
	}
	if balMismatch != wantBal {
		t.Errorf("落库的余额不一致条数 = %d，Findings 里数出 %d", balMismatch, wantBal)
	}
	// 这一轮被直接改过的只有余额，冻结那一侧没动：Findings 里不该有冻结那
	// 一类，落库的那一列也该是 0。把余额差算进冻结列里，报表上就成了
	// 「钱冻错了」。
	if wantFrozen := countFindings(report, FindingFrozenMismatch); frozenMismatch != wantFrozen {
		t.Errorf("落库的冻结不一致条数 = %d，Findings 里数出 %d", frozenMismatch, wantFrozen)
	}
	// 畸形科目单独一列，而它**不**计入 balance_mismatches：那笔钱归不到
	// 任何用户头上，与「某个用户的余额对不上」是两件事。
	if wantMalformed := countFindings(report, FindingMalformedAccount); malformedCount != wantMalformed {
		t.Errorf("落库的畸形科目数 = %d，Findings 里数出 %d", malformedCount, wantMalformed)
	}
	// 这一轮只报告不修（见 noRepair），所以一条都没动手。
	if wantRepaired := countRepairedFindings(report); repaired != wantRepaired {
		t.Errorf("落库的 repaired = %d，Findings 里数出 %d", repaired, wantRepaired)
	}
	if watermark == "" {
		t.Error("起点必须落库，否则下一轮无从增量")
	}
}

// 「修了几条」要真的落库：只看 balance_mismatches 数不出这一轮动过手，
// 而「发现了 3 条、修了 3 条」与「发现了 3 条、一条都没修」是两种处境。
//
// 期望值从这一轮的 Findings 现数，既不用 report.Repaired() 也不用字面量：
// 前者就是写那一列的来源，两边比等于自己跟自己比；而同一个测试库上可能同时
// 跑着别的进程（变异测试就是并行的），总数不该由用例写死。
func TestReconcileRunRecordsRepairedCount(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "5")

	report := reconcile(t, db, withResyncRepair())
	if f := kindOf(t, report, userID, FindingBalanceMismatch); !f.Repaired {
		t.Fatal("这一轮应当把这个用户的余额修回来")
	}

	var repaired int
	if err := db.QueryRowContext(context.Background(),
		`SELECT repaired FROM reconcile_runs WHERE id = $1`, report.RunID).Scan(&repaired); err != nil {
		t.Fatal(err)
	}
	if want := countRepairedFindings(report); repaired != want {
		t.Errorf("落库的 repaired = %d，Findings 里数出 %d", repaired, want)
	}
	if repaired == 0 {
		t.Error("修了东西就该有非零的 repaired")
	}
}

// 上一次崩在中间留下的 running 必须被清掉：留着它，下一轮读到的
// 就是一个假的「在跑」状态。
//
// 断言盯的是**自己插进去的那一行**，不是全表的 failed 条数：库里留着
// 别的运行留下的记录，按条数断言会变成「这台机器上跑过几次」的函数。
func TestReconcileClearsStaleRunningRun(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	var staleID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO reconcile_runs (started_at, watermark, status)
		VALUES (now() - interval '3 hours', now() - interval '3 hours', 'running')
		RETURNING id`).Scan(&staleID); err != nil {
		t.Fatal(err)
	}

	report := reconcile(t, db, noRepair())

	var (
		status string
		reason sql.NullString
		closed bool
	)
	if err := db.QueryRowContext(ctx, `
		SELECT status, error, finished_at IS NOT NULL
		  FROM reconcile_runs WHERE id = $1`, staleID).Scan(&status, &reason, &closed); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("停了三小时的那条状态 = %q，期望 failed", status)
	}
	if reason.String != "进程中断，未跑完" {
		t.Errorf("原因 = %q，期望说明是进程中断", reason.String)
	}
	if !closed {
		t.Error("被清掉的那条必须有 finished_at，否则它看起来还在跑")
	}

	// 这一轮自己那条必须已经结掉，不能还挂在 running 上。
	//
	// 不断言「全表一条 running 都没有」：同一个库上可能有另一轮对账正在跑
	// （另一台机器、另一次手跑、变异测试起的一轮），而它与这条用例无关。
	// 能断言的是**本轮这条**的结局，下面就是。
	var ended bool
	if err := db.QueryRowContext(ctx, `
		SELECT finished_at IS NOT NULL AND status = 'done'
		  FROM reconcile_runs WHERE id = $1`, report.RunID).Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if !ended {
		t.Error("本轮运行应当以 done 收尾")
	}
}

// ctx 被取消之后，这次运行的结局仍然要写回去。
//
// # 这条用例守的是什么
//
// 结账那一步曾经跟着调用方的 ctx 走，于是它最需要写进去的时刻——ctx 已经
// 不能用的那一刻——恰好是它写不进去的时刻。库里于是留一行 status='running'，
// 而它挡住的是「上一轮跑到哪了」这个判断，要等一小时才被 stale 清理捞走。
// 最常触发它的是**超时**：CLI 给单轮对账的上限（30 分钟）必然短于 stale
// 清理的一小时，所以那一行本来永远等不到自己这一轮来结。
//
// # 怎么让 ctx 在跑到一半时取消
//
// 在第一条告警里取消。告警是主循环里**确定会走到**的一步（这一轮必然报出
// 一条不一致，下面那行 breakBalance 就是为此），而它之后主循环还要再取一批
// 候选用户，那一次查询必然失败。这样不必靠 sleep 或超时去撞一个时刻——
// 撞不准的用例会变成偶发失败，而偶发失败最后总是被当成噪声忽略掉。
func TestReconcileClosesRunWhenContextCanceled(t *testing.T) {
	db := testDB(t)
	userID := seed(t, db, "100")
	breakBalance(t, db, userID, "5")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alerted := false
	alert := func(_ context.Context, _ Finding) {
		alerted = true
		cancel()
	}
	report, err := New(db).Reconcile(ctx, noRepair(), alert)
	if err == nil {
		t.Fatal("ctx 取消之后这一轮应当以错误收尾")
	}
	if !alerted {
		t.Fatal("这一轮没有报出任何不一致，用例没能取消到 ctx（断言会空转通过）")
	}
	if report == nil {
		t.Fatal("失败也要返回已经查出来的部分")
	}

	var (
		status string
		reason sql.NullString
		closed bool
	)
	if err := db.QueryRowContext(context.Background(), `
		SELECT status, error, finished_at IS NOT NULL
		  FROM reconcile_runs WHERE id = $1`, report.RunID).Scan(&status, &reason, &closed); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Errorf("状态 = %q，期望 failed：ctx 取消不应当留下一行结不掉的 running", status)
	}
	if !closed {
		t.Error("这一行必须有 finished_at，否则它看起来还在跑")
	}
	if !strings.Contains(reason.String, context.Canceled.Error()) {
		t.Errorf("原因 = %q，期望说明是 ctx 被取消", reason.String)
	}
}

// ── 配置 ────────────────────────────────────────────────────────────────

// fakeSource 给 settings.Load 喂一份完整配置。
//
// settings 包自己的 fakeSource 没有导出，所以这里另写一份。
type fakeSource struct {
	rows []settings.Row
	err  error
}

func (f *fakeSource) Load(context.Context) ([]settings.Row, error) { return f.rows, f.err }

// settingsRows 造一份完整的配置行集，并允许覆盖其中若干项。
func settingsRows(overrides map[string]string) []settings.Row {
	vals := map[string]string{
		"request_snapshot.retention_days":  "30",
		"request_snapshot.enabled":         "true",
		"request_snapshot.max_bytes":       "65536",
		"capture.consent_max_window_hours": "24",
		"quota.probe_interval_seconds":     "300",
		"quota.exhausted_cooldown_seconds": "900",
		"billing.default_asset":            `"CNY"`,
		"billing.max_output_tokens":        "65536",
		"billing.stream_freeze_step":       "4096",
		"billing.merchant_share_bp":        "0",
		"calendar.default_region":          `"CN"`,
		"idempotency.retention_hours":      "24",
		"console.session_retention_days":   "30",
		"plugin.enabled":                   "false",
		"plugin.call_timeout_ms":           "5000",
		"plugin.memory_limit_bytes":        "67108864",
		"plugin.max_payload_bytes":         "4194304",
		"payment.fake_enabled":             "false",
		"plans.charge_order":               `"plan_first"`,
		"admin.token_sha256":               `""`,
		SettingAutoRepair:                  "false",
		SettingRepairMode:                  `"resync_snapshot"`,
		SettingDifferenceAccount:           `"reconcile_difference"`,
		SettingShardSize:                   "1000",
		SettingFullScanIntervalHours:       "168",
	}
	for k, v := range overrides {
		vals[k] = v
	}
	out := make([]settings.Row, 0, len(vals))
	for k, v := range vals {
		out = append(out, settings.Row{Key: k, Value: []byte(v)})
	}
	return out
}

func loadValues(t *testing.T, overrides map[string]string) *settings.Values {
	t.Helper()
	v, err := settings.Load(context.Background(), &fakeSource{rows: settingsRows(overrides)})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return v
}

func TestConfigFromSettingsReadsDefaults(t *testing.T) {
	cfg, err := ConfigFromSettings(loadValues(t, nil))
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}

	if cfg.AutoRepair {
		t.Error("默认必须只报告不修正")
	}
	if cfg.RepairMode != RepairResyncSnapshot {
		t.Errorf("默认口径 = %q，期望 resync_snapshot", cfg.RepairMode)
	}
	if cfg.DifferenceAccount != ledger.ReconcileDifferenceAccount() {
		t.Errorf("默认差额科目 = %s，期望 reconcile_difference", cfg.DifferenceAccount)
	}
	if cfg.ShardSize != 1000 {
		t.Errorf("分片大小 = %d，期望 1000", cfg.ShardSize)
	}
	if cfg.FullScanInterval != 168*time.Hour {
		t.Errorf("全量间隔 = %v，期望 168h", cfg.FullScanInterval)
	}
}

func TestConfigFromSettingsReadsOverrides(t *testing.T) {
	cfg, err := ConfigFromSettings(loadValues(t, map[string]string{
		SettingAutoRepair:            "true",
		SettingRepairMode:            `"adjust_ledger"`,
		SettingDifferenceAccount:     `"platform_revenue"`,
		SettingShardSize:             "50",
		SettingFullScanIntervalHours: "0",
	}))
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}

	if !cfg.AutoRepair || cfg.RepairMode != RepairAdjustLedger {
		t.Errorf("自动修正 = %v/%q，期望 true/adjust_ledger", cfg.AutoRepair, cfg.RepairMode)
	}
	if cfg.DifferenceAccount != ledger.PlatformRevenueAccount() {
		t.Errorf("差额科目 = %s", cfg.DifferenceAccount)
	}
	if cfg.ShardSize != 50 || cfg.FullScanInterval != 0 {
		t.Errorf("分片 = %d，全量间隔 = %v", cfg.ShardSize, cfg.FullScanInterval)
	}
}

// 写错的口径必须在跑之前就被拦住，而不是按一个没人预期的方向去改钱。
func TestConfigRejectsUnknownRepairMode(t *testing.T) {
	_, err := ConfigFromSettings(loadValues(t, map[string]string{
		SettingRepairMode: `"fix_everything"`,
	}))
	if err == nil {
		t.Fatal("未知的口径应当报错")
	}
	if !strings.Contains(err.Error(), "fix_everything") {
		t.Errorf("报错要指出是哪个值：%v", err)
	}
}

// 差额科目落在用户钱包上时，那条分录会让另一个用户偏离账本，
// 而对账修正不走 applyToWallets，没人会去修它。必须拦下。
func TestConfigRejectsUserAccountAsDifference(t *testing.T) {
	_, err := ConfigFromSettings(loadValues(t, map[string]string{
		SettingDifferenceAccount: `"user:1"`,
	}))
	if err == nil {
		t.Fatal("差额科目不该允许是用户钱包")
	}
}

func TestConfigRejectsNegativeFullScanInterval(t *testing.T) {
	_, err := ConfigFromSettings(loadValues(t, map[string]string{
		SettingFullScanIntervalHours: "-1",
	}))
	if err == nil {
		t.Fatal("负的全量间隔应当报错")
	}
}

// 负的分片大小会被 normalize 当成「没填」而换成默认值——那是静默回落，
// 表现为「我明明调小了分片，它却按默认值跑」。
func TestConfigRejectsNegativeShardSize(t *testing.T) {
	_, err := ConfigFromSettings(loadValues(t, map[string]string{
		SettingShardSize: "-5",
	}))
	if err == nil {
		t.Fatal("负的分片大小应当报错")
	}
	if !strings.Contains(err.Error(), SettingShardSize) {
		t.Errorf("报错要指出是哪个键：%v", err)
	}
}

// 零是写明的合法值：它的含义是「用默认值」，不是「分片大小为 0」。
//
// 另一半在 TestConfigRejectsNegativeShardSize 里：负数要挡住，零要放行。
// `shard < 0` 与 `shard <= 0` 的差别全落在这一格上——后者把「没填」也当成
// 填错了，而它报的错与负数那条一模一样，读的人只会以为自己填的是负数。
func TestConfigAcceptsZeroShardSizeAsDefault(t *testing.T) {
	cfg, err := ConfigFromSettings(loadValues(t, map[string]string{
		SettingShardSize: "0",
	}))
	if err != nil {
		t.Fatalf("0 是合法值（含义是用默认值），报错说明判据把零与负数一起挡住了: %v", err)
	}
	if cfg.ShardSize != defaultShardSize {
		t.Errorf("分片大小 = %d，期望默认值 %d——零要落到默认值上，而不是原样传下去",
			cfg.ShardSize, defaultShardSize)
	}
}

// 零值配置要有明确行为：分片大小为零会让分片循环空转，
// 而那看起来跟「跑完了，没有不一致」一模一样。
func TestConfigNormalizeFillsZeroValues(t *testing.T) {
	cfg, err := (ReconcileConfig{}).normalize()
	if err != nil {
		t.Fatalf("normalize 出错: %v", err)
	}
	if cfg.ShardSize != defaultShardSize {
		t.Errorf("分片大小 = %d，期望 %d", cfg.ShardSize, defaultShardSize)
	}
	if cfg.RepairMode != RepairResyncSnapshot {
		t.Errorf("口径 = %q，期望 resync_snapshot", cfg.RepairMode)
	}
}

// ── 告警 ────────────────────────────────────────────────────────────────

// 告警里不给陈旧预留打 diff。
//
// 一个叫 diff 而其实是金额本身的数会被读成一条差额，而这一类恰恰一条差额
// 都没有（见 Finding.Diff 的注释）。其余类别照旧打 snapshot 与 diff。
//
// 这条不需要数据库：告警只是一段格式化，而它容易被顺手改回去。
func TestLogAlertsOmitsDiffForStaleReservation(t *testing.T) {
	var buf bytes.Buffer
	alert := LogAlerts(slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()

	alert(ctx, Finding{
		Kind: FindingStaleReservation, UserID: 7, Asset: "CNY", Key: "req-1",
		Ledger:       money.MustParse("30"),
		Unrepairable: "只报告不释放：自动释放与在途请求有竞态",
	})
	out := buf.String()
	if !strings.Contains(out, "kind=stale_reservation") {
		t.Errorf("告警里要有类别：%s", out)
	}
	if strings.Contains(out, "diff=") {
		t.Errorf("陈旧预留没有差额，不该打出 diff：%s", out)
	}
	if !strings.Contains(out, "ledger=30") {
		t.Errorf("金额要打出来（它装在 ledger 那一栏里）：%s", out)
	}

	buf.Reset()
	alert(ctx, Finding{
		Kind: FindingFrozenMismatch, UserID: 7, Asset: "CNY",
		Snapshot: money.MustParse("50"), Ledger: money.MustParse("30"),
	})
	if out := buf.String(); !strings.Contains(out, "diff=-20") {
		t.Errorf("其余类别要打 diff：%s", out)
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

// appendLedgerEntry 往账本里补一条分录，**不动快照**。
//
// 它造的是「有代码路径只写了账本、没写快照」的形状——与 breakBalance 的
// 方向相反，两条合起来才覆盖了不一致的两个来源。
func appendLedgerEntry(t *testing.T, db *sql.DB, account ledger.Account, amount string, kind ledger.EntryType) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
		VALUES (gen_random_uuid(), $1, 'CNY', $2::numeric, $3)`,
		account.String(), amount, string(kind)); err != nil {
		t.Fatalf("补账本分录失败: %v", err)
	}
}
