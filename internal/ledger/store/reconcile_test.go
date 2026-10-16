package store

import (
	"context"
	"database/sql"
	"fmt"
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

	req := testRequest(userID, reqID(t))
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

	req := testRequest(userID, reqID(t))
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

	req := testRequest(userID, reqID(t))
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req); err != nil {
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
// ——查的地方完全不同（注册流程 vs 预留落了而钱包被删）。
func TestReconcileReportsWalletMissingForReservationOnly(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")
	req := testRequest(userID, reqID(t))
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
// ——而注册流程漏建钱包的用户恰好是这个形状。WalletMissing 那条分支存在的
// 意义正是把他报出来，所以候选集必须带上未结算预留这一侧。
func TestReconcileReportsWalletMissingForReservationWithoutOtherSides(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	req := testRequest(userID, reqID(t))
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
	req := testRequest(userID, reqID(t))
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
	req := testRequest(userID, reqID(t))
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
	if balMismatch == 0 {
		t.Error("余额不一致的条数应当记进运行记录")
	}
	if malformedCount != report.MalformedAccounts() {
		// 畸形科目单独一列，而它**不**计入 balance_mismatches：那笔钱
		// 归不到任何用户头上，与「某个用户的余额对不上」是两件事。
		t.Errorf("畸形科目数 = %d，报告里是 %d", malformedCount, report.MalformedAccounts())
	}
	if watermark == "" {
		t.Error("起点必须落库，否则下一轮无从增量")
	}
}

// 「修了几条」要真的落库：只看 balance_mismatches 数不出这一轮动过手，
// 而「发现了 3 条、修了 3 条」与「发现了 3 条、一条都没修」是两种处境。
//
// 断言盯的是**这个用户**那一条，不是 report.Repaired() 的总数：同一个测试
// 库上可能同时跑着别的进程（变异测试就是并行的），总数会被它们影响。
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
	if repaired != report.Repaired() {
		t.Errorf("落库的 repaired = %d，报告里是 %d", repaired, report.Repaired())
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
		"billing.default_asset":            `"CNY"`,
		"calendar.default_region":          `"CN"`,
		"plugin.enabled":                   "false",
		"plugin.call_timeout_ms":           "5000",
		"plugin.memory_limit_bytes":        "67108864",
		"plugin.max_payload_bytes":         "4194304",
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
