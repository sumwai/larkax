package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
)

// 本文件是「科目余额下界」这一类对账检查的用例。
//
// 判据来自 issue：负数要报出科目与余额；0 与正数不报（正常状态不能被报成问题）；
// 负余额尽量走 plan_carry / plan_carry_merge 的真实路径造，不手写分录。

// TestReconcileReportsAccountBelowFloor 同时判三格：负余额报出来，0 与正余额不报。
//
// # 负余额怎么造
//
// 走真实路径：先 plan_carry 从源订阅结转出一笔额度（prepaid 减、carryover 加），
// 再 plan_carry_merge 把它并进一条新订阅（carryover 减、prepaid 加）。分录由
// ledger.PlanCarry / ledger.PlanCarryMerge 构造、经 store.PostTransaction 落库，
// 与 plans 服务的 mergeCarryovers 是同一组构造函数与同一个写入口。
//
// 差别只有金额：mergeCarryovers 在锁内读余额、按余额决定并入多少，正常路径永远
// 并不出负数。负数只可能来自「并出去的比账上多」，所以这里按那个形状取了一个
// 服务不会取的金额——它就是要被对账抓到的那种多扣。
//
// 三份额度用三个源订阅造，金额分别是 10（并 15，余额 -5）、10（并 10，余额 0）、
// 10（不并，余额 +10）。同一个库里连着比这三格，顺带钉住「下界是含的」：
// 恰好等于 0 不算越界。
func TestReconcileReportsAccountBelowFloor(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "amount", "50", "50")

	expires := time.Now().Add(-time.Hour)
	sourceNegative := seedUserPlan(t, ctx, db, userID, planID, "50", expires)
	sourceZero := seedUserPlan(t, ctx, db, userID, planID, "50", expires)
	sourcePositive := seedUserPlan(t, ctx, db, userID, planID, "50", expires)
	target := seedUserPlan(t, ctx, db, userID, planID, "1", time.Now().Add(time.Hour))

	// carry 是期末结转：从一条源订阅结转出 face。
	carry := func(sourceID int64, face string) {
		t.Helper()
		txn, err := ledger.PlanCarry(ledger.PlanCarryParams{
			TxnID:          ledger.NewTxnID(),
			SubscriptionID: sourceID,
			Amount:         mustMoney(t, face),
			Asset:          ledger.AssetCNY,
			Reason:         "到期",
			Ref:            "test-carry-" + itoa(sourceID),
		})
		if err != nil {
			t.Fatalf("构造结转分录失败: %v", err)
		}
		applied, err := s.PostTransaction(ctx, txn)
		if err != nil {
			t.Fatalf("写结转分录失败: %v", err)
		}
		if !applied {
			t.Fatalf("结转分录没有被写入（被当成了重放）")
		}
	}

	// merge 是领新包时的并入：把 sourceID 结转出来的 face 并进 target。
	merge := func(sourceID int64, face string) {
		t.Helper()
		txn, err := ledger.PlanCarryMerge(ledger.PlanCarryMergeParams{
			TxnID:                ledger.NewTxnID(),
			TargetSubscriptionID: target,
			Moves: []ledger.PlanCarryMove{
				{SourceSubscriptionID: sourceID, Amount: mustMoney(t, face)},
			},
			Asset: ledger.AssetCNY,
			Ref:   "test-merge-" + itoa(sourceID),
		})
		if err != nil {
			t.Fatalf("构造并入分录失败: %v", err)
		}
		applied, err := s.PostTransaction(ctx, txn)
		if err != nil {
			t.Fatalf("写并入分录失败: %v", err)
		}
		if !applied {
			t.Fatalf("并入分录没有被写入（被当成了重放）")
		}
	}

	carry(sourceNegative, "10")
	carry(sourceZero, "10")
	carry(sourcePositive, "10")
	merge(sourceNegative, "15") // 比结转进来的多并了 5：余额 -5
	merge(sourceZero, "10")     // 并干净了：余额 0
	// sourcePositive 不并：余额 +10

	report := reconcile(t, db, noRepair())

	negativeAccount := ledger.CarryoverAccount(sourceNegative).String()
	zeroAccount := ledger.CarryoverAccount(sourceZero).String()
	positiveAccount := ledger.CarryoverAccount(sourcePositive).String()

	var floorFindings []Finding
	for _, f := range report.Findings {
		if f.Kind == FindingAccountBelowFloor {
			floorFindings = append(floorFindings, f)
		}
	}

	// 负余额那一条必须先找到：它是这一类的全部意义。找不到时直接停，后面那些
	// 断言都在「有一条正确答案」的前提上。
	var found *Finding
	for i := range floorFindings {
		if floorFindings[i].Key == negativeAccount {
			found = &floorFindings[i]
		}
	}
	if found == nil {
		t.Fatalf("负余额的科目 %q 没有被报出来，实际报出 %v", negativeAccount, floorFindings)
	}
	f := *found

	// 0 与正余额不能报：那是正常状态（结转的额度并干净了 / 还没并）。
	// 这一条与上面的「必须找到负余额」是两道独立的判据——只报 0、不报负数的
	// 实现过第一道会红，全报的实现过第二道会红。
	for _, account := range []string{zeroAccount, positiveAccount} {
		for _, got := range floorFindings {
			if got.Key == account {
				t.Errorf("余额 %s 没有越界，却被报了出来：%+v", account, got)
			}
		}
	}

	// 这一轮只该有负余额那一格：多出来的那一条说不出是哪个科目越了界，
	// 等于清单里多了一行读不懂的记录。
	if len(floorFindings) != 1 {
		t.Errorf("这一轮只该报出负余额那一格，实际 %d 条: %v", len(floorFindings), floorFindings)
	}
	if f.Asset != "CNY" {
		t.Errorf("资产 = %q，期望 CNY", f.Asset)
	}
	if f.Ledger.Cmp(mustMoney(t, "-5")) != 0 {
		t.Errorf("余额 = %s，期望 -5", f.Ledger)
	}
	if !f.Snapshot.IsZero() {
		t.Errorf("下界 = %s，期望 0", f.Snapshot)
	}
	if f.Diff().Cmp(mustMoney(t, "-5")) != 0 {
		t.Errorf("比下界低 %s，期望 -5", f.Diff())
	}
	if want := floorScopePrefix + negativeAccount; f.Scope() != want {
		t.Errorf("scope = %q，期望 %q", f.Scope(), want)
	}
	// 报出来的时候要说清是哪个科目、余额是多少——处置的人只拿到这几行。
	if !strings.Contains(f.Detail, negativeAccount) {
		t.Errorf("详情里没有点出科目 %q：%s", negativeAccount, f.Detail)
	}
	if !strings.Contains(f.Detail, "-5.00000000") {
		t.Errorf("详情里没有点出余额 -5：%s", f.Detail)
	}
	if f.Repaired {
		t.Error("越界的科目不该被自动修正")
	}
	if f.Unrepairable == "" {
		t.Error("只报告不修的那一类要把原因写进 Unrepairable")
	}

	// 运行记录上这一档要单独落一个数：findings 修掉就删，那一行是唯一的历史。
	var persisted int
	if err := db.QueryRowContext(ctx,
		`SELECT account_below_floors FROM reconcile_runs WHERE id = $1`,
		report.RunID).Scan(&persisted); err != nil {
		t.Fatalf("读运行记录的科目下界计数失败: %v", err)
	}
	if persisted != 1 {
		t.Errorf("运行记录上 account_below_floors = %d，期望 1", persisted)
	}

	// 增量那一轮没有任何新分录：负余额那一格只能靠「已记未消除的那些科目」
	// 捞回来。没有那一段，下一轮就再也不会报它——而它仍然挂着。
	incrementalCfg := noRepair()
	incrementalCfg.FullScanInterval = reconcileInterval
	second := reconcile(t, db, incrementalCfg)
	if second.FullScan {
		t.Fatalf("第二轮应当走增量，却又做了一次全量：增量侧的「已记未消除」这一段没有被走到")
	}

	var secondFloor []Finding
	for _, got := range second.Findings {
		if got.Kind == FindingAccountBelowFloor {
			secondFloor = append(secondFloor, got)
		}
	}
	if len(secondFloor) != 1 || secondFloor[0].Key != negativeAccount {
		t.Fatalf("增量那一轮应当仍然报出 %q，实际 %v", negativeAccount, secondFloor)
	}
	if secondFloor[0].Ledger.Cmp(mustMoney(t, "-5")) != 0 {
		t.Errorf("增量那一轮的余额 = %s，期望 -5", secondFloor[0].Ledger)
	}

	// 标识要真的落进 reconcile_findings：它是「还没消除」的那张清单的主键。
	var kind string
	if err := db.QueryRowContext(ctx,
		`SELECT kind FROM reconcile_findings WHERE scope = $1`,
		floorScopePrefix+negativeAccount).Scan(&kind); err != nil {
		t.Fatalf("reconcile_findings 里没有科目 %q 那一行: %v", negativeAccount, err)
	}
	if kind != string(FindingAccountBelowFloor) {
		t.Errorf("落库的 kind = %q，期望 %q", kind, FindingAccountBelowFloor)
	}
}
