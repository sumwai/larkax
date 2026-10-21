package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// 本文件是**请求路径按额度包选源与预扣**（#860 第 2 步 / #918）的用例。
//
// 要钉住的东西按重要性排：
//
//   - 额度包覆盖的请求**不冻钱包**：余额为零、只有额度包的用户必须能用；
//   - 选择用哪一条是「先到期先用」，且整单只从一条扣；
//   - 两个维度任一不够就跳过（面值 / 可用量），不是只看其中一个；
//   - 预留与结算用同一个订阅，结算记 prepaid 科目、按实际用量改 quota_used；
//   - 余额回落与只走余额两档按配置走，配置读不动时按默认值。

// fixedChargeOrder 是一个写死取值的扣费顺序读点。
type fixedChargeOrder string

func (f fixedChargeOrder) RequestChargeOrder(context.Context, int64) string { return string(f) }

// seedPlan 建一个额度包，返回它的 id。
func seedPlan(t *testing.T, ctx context.Context, db *sql.DB, unit, quota, face string, models ...string) int64 {
	t.Helper()
	id := nextID()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (id, key, name, unit, quota_total, face_value)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric)`,
		id, "plan-"+itoa(id), "测试包", unit, quota, face); err != nil {
		t.Fatalf("建额度包失败: %v", err)
	}
	for _, model := range models {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO plan_models (plan_id, model) VALUES ($1, $2)`, id, model); err != nil {
			t.Fatalf("写包模型范围失败: %v", err)
		}
	}
	return id
}

// seedUserPlan 发一条订阅并记一笔授予分录，返回订阅 id。
//
// 走账本而不是只插一行订阅：面值余额是选源判据的一半，直接写一个数会造出
// 「账本里不存在的面值」，而那正是这些用例要判断的东西。
func seedUserPlan(t *testing.T, ctx context.Context, db *sql.DB, userID, planID int64, face string, expires time.Time) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO user_plans (user_id, plan_id, status, period_start, period_end, ref)
		VALUES ($1, $2, 'active', now(), $3, $4)
		RETURNING id`,
		userID, planID, expires, "ref-"+itoa(nextID())).Scan(&id); err != nil {
		t.Fatalf("建订阅失败: %v", err)
	}
	amt := mustMoney(t, face)
	txn := ledger.NewTxnID()
	for _, e := range []ledger.Entry{
		{TxnID: txn, Account: ledger.PrepaidAccount(id), Asset: ledger.AssetCNY,
			Amount: amt, EntryType: ledger.EntryPlanGrant},
		{TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
			Amount: amt.Neg(), EntryType: ledger.EntryPlanGrant},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
			VALUES ($1, $2, $3, $4::numeric, $5)`,
			string(e.TxnID), e.Account.String(), string(e.Asset),
			e.Amount.String(), string(e.EntryType)); err != nil {
			t.Fatalf("写授予分录失败: %v", err)
		}
	}
	return id
}

// accountBalanceOf 读一个科目的余额。
func accountBalanceOf(t *testing.T, db *sql.DB, account string) string {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		  FROM ledger_entries WHERE account = $1 AND asset = 'CNY'`, account).Scan(&raw); err != nil {
		t.Fatalf("读科目余额失败: %v", err)
	}
	return raw
}

// billingPlanRow 是一次预留行上与额度包有关的那几个数。
type billingPlanRow struct {
	status             string
	userPlanID         sql.NullInt64
	reservedAmount     string
	planReservedAmount string
	reservedQuota      string
	finalAmount        sql.NullString
}

func readBillingPlanRow(t *testing.T, db *sql.DB, requestID string) billingPlanRow {
	t.Helper()
	var row billingPlanRow
	if err := db.QueryRowContext(context.Background(), `
		SELECT status, user_plan_id, reserved_amount::text,
		       plan_reserved_amount::text, reserved_quota::text, final_amount::text
		  FROM billing_requests WHERE request_id = $1`, requestID).Scan(
		&row.status, &row.userPlanID, &row.reservedAmount,
		&row.planReservedAmount, &row.reservedQuota, &row.finalAmount); err != nil {
		t.Fatalf("读预留行失败: %v", err)
	}
	return row
}

func quotaUsedOf(t *testing.T, db *sql.DB, planID int64) string {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(context.Background(),
		`SELECT quota_used::text FROM user_plans WHERE id = $1`, planID).Scan(&raw); err != nil {
		t.Fatalf("读订阅已用量失败: %v", err)
	}
	return raw
}

// 先到期的先用，且整单只从一条扣；额度包覆盖时**一分钱都不冻**。
func TestReserveFromEarliestExpiringPlanWithoutFreezingWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	later := seedPlan(t, ctx, db, "amount", "50", "50")
	earlier := seedPlan(t, ctx, db, "amount", "40", "40")
	lateID := seedUserPlan(t, ctx, db, userID, later, "50", time.Now().Add(2*time.Hour))
	earlyID := seedUserPlan(t, ctx, db, userID, earlier, "40", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	row := readBillingPlanRow(t, db, requestID)
	if !row.userPlanID.Valid || row.userPlanID.Int64 != earlyID {
		t.Fatalf("选中的订阅 = %+v，期望先到期的 %d（候选还有 %d）", row.userPlanID, earlyID, lateID)
	}
	if row.reservedAmount != "0.00000000" {
		t.Errorf("钱包预留额 = %s，额度包覆盖的请求不该冻钱包", row.reservedAmount)
	}
	if row.planReservedAmount != "10.00000000" {
		t.Errorf("包面值占用 = %s，期望 10", row.planReservedAmount)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("钱包冻结 = %s，期望 0", frozen)
	}
}

// token 包按「输入估算 + 输出上界」占可用量，不看金额那一列。
func TestReserveFromTokenPlanReservesQuota(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "token", "5000", "50", "gpt-4o")
	upID := seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID) // 输入 1000
	r := reservation(t, userID, requestID, "10", "CNY")
	r.OutputTokens = 500
	if _, err := s.SaveReservation(ctx, req, r); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	row := readBillingPlanRow(t, db, requestID)
	if !row.userPlanID.Valid || row.userPlanID.Int64 != upID {
		t.Fatalf("应选中 token 包，实际 user_plan_id=%+v", row.userPlanID)
	}
	if row.reservedQuota != "1500.00000000" {
		t.Errorf("占用的可用量 = %s，期望 1500（1000 输入 + 500 输出上界）", row.reservedQuota)
	}
}

// 可用量不够时跳过这个包，回落到余额；两个维度任一不够都算不够。
func TestReserveSkipsPlanWithoutEnoughQuota(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "token", "1000", "50", "gpt-4o")
	seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID) // 需要 1000 + 500
	r := reservation(t, userID, requestID, "10", "CNY")
	r.OutputTokens = 500
	if _, err := s.SaveReservation(ctx, req, r); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	row := readBillingPlanRow(t, db, requestID)
	if row.userPlanID.Valid {
		t.Fatalf("可用量不够时不该选中这个包，实际 user_plan_id=%d", row.userPlanID.Int64)
	}
	if row.reservedAmount != "10.00000000" {
		t.Errorf("钱包预留额 = %s，期望 10（回落到余额）", row.reservedAmount)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "10.00000000" {
		t.Errorf("钱包冻结 = %s，期望 10", frozen)
	}
}

// 模型范围把包排除在候选之外。
func TestReservePlanRespectsModelScope(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	scopeMiss := seedPlan(t, ctx, db, "amount", "50", "50", "gpt-4o-mini")
	allowed := seedPlan(t, ctx, db, "amount", "50", "50", "gpt-4o")
	// 范围不符的那个先到期：没有模型范围判据时它会赢，断言才可能变红。
	seedUserPlan(t, ctx, db, userID, scopeMiss, "50", time.Now().Add(time.Hour))
	allowedID := seedUserPlan(t, ctx, db, userID, allowed, "50", time.Now().Add(2*time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	row := readBillingPlanRow(t, db, requestID)
	if !row.userPlanID.Valid || row.userPlanID.Int64 != allowedID {
		t.Fatalf("选中的订阅 = %+v，期望模型范围覆盖本次请求的 %d", row.userPlanID, allowedID)
	}
}

// key 的允许集把包排除在候选之外；名单里的包必须属于这个用户，名单为空才表示全部。
func TestReservePlanRespectsKeyAllowList(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	notListed := seedPlan(t, ctx, db, "amount", "50", "50", "gpt-4o")
	allowed := seedPlan(t, ctx, db, "amount", "50", "50", "gpt-4o")
	// 不在名单里的那个先到期：没有允许集判据时它会赢，断言才可能变红。
	excludedID := seedUserPlan(t, ctx, db, userID, notListed, "50", time.Now().Add(time.Hour))
	allowedID := seedUserPlan(t, ctx, db, userID, allowed, "50", time.Now().Add(2*time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_key_plans (api_key_id, user_plan_id) VALUES ($1, $2)`,
		req.APIKeyID, allowedID); err != nil {
		t.Fatalf("写 key 允许集失败: %v", err)
	}
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	row := readBillingPlanRow(t, db, requestID)
	if !row.userPlanID.Valid || row.userPlanID.Int64 != allowedID {
		t.Fatalf("选中的订阅 = %+v，期望允许集里的 %d（不在名单里的 %d 要跳过）",
			row.userPlanID, allowedID, excludedID)
	}
}

// 结算按快照记 prepaid 科目，并按实际用量累加 quota_used。
func TestSettleFromPlanDebitsPrepaidAndConsumesQuota(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "token", "5000", "50", "gpt-4o")
	upID := seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	req.UpstreamModel = "gpt-4o-upstream"
	r := reservation(t, userID, requestID, "10", "CNY")
	r.OutputTokens = 500
	billingID, err := s.SaveReservation(ctx, req, r)
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	actual := mustMoney(t, "5")
	consume, err := ledger.Consume(ledger.ConsumeParams{
		TxnID:            ledger.NewTxnID(),
		UserID:           userID,
		Asset:            ledger.AssetCNY,
		Amount:           actual,
		PrepaidAccount:   ledger.PrepaidAccount(upID),
		BillingRequestID: billingID,
	})
	if err != nil {
		t.Fatalf("构造消费分录失败: %v", err)
	}
	charge := &pipeline.Charge{
		RequestID:   requestID,
		UserID:      userID,
		Asset:       "CNY",
		Settlement:  billing.Settlement{Actual: actual, Reserved: r.Amount, Breakdown: billing.Breakdown{Total: actual}},
		Entries:     consume.Denormalize(),
		Usage:       billing.Usage{PromptTokens: 1000, CompletionTokens: 200},
		UsageSource: tokens.SourceMeasured,
		PricedAt:    time.Now(),
	}
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	row := readBillingPlanRow(t, db, requestID)
	if row.status != "settled" {
		t.Errorf("状态 = %s，期望 settled", row.status)
	}
	if !row.finalAmount.Valid || row.finalAmount.String != "5.00000000" {
		t.Errorf("final_amount = %+v，期望 5", row.finalAmount)
	}
	if got := accountBalanceOf(t, db, ledger.PrepaidAccount(upID).String()); got != "45.00000000" {
		t.Errorf("prepaid 余额 = %s，期望 45（50 − 5）", got)
	}
	if got := quotaUsedOf(t, db, upID); got != "1200.00000000" {
		t.Errorf("quota_used = %s，期望 1200（实际 1000 + 200）", got)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("钱包冻结 = %s，额度包覆盖的请求不该有冻结", frozen)
	}
}

// 实际用量超过预留额时截到预留额：宁可少记，也不让包的可用量透支。
//
// 截断是有意的取舍，但它让记下的用量小于实际用量——这种偏差在账上看不出来，
// 所以同时要留一条 WARN。这条用例把两个判据一起钉住。
func TestSettleFromPlanClipsUsageToReserved(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "token", "5000", "50", "gpt-4o")
	upID := seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	r := reservation(t, userID, requestID, "10", "CNY")
	r.OutputTokens = 500 // 预留 1000 + 500 = 1500 个 token
	billingID, err := s.SaveReservation(ctx, req, r)
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))

	actual := mustMoney(t, "5")
	consume, err := ledger.Consume(ledger.ConsumeParams{
		TxnID:            ledger.NewTxnID(),
		UserID:           userID,
		Asset:            ledger.AssetCNY,
		Amount:           actual,
		PrepaidAccount:   ledger.PrepaidAccount(upID),
		BillingRequestID: billingID,
	})
	if err != nil {
		t.Fatalf("构造消费分录失败: %v", err)
	}
	charge := &pipeline.Charge{
		RequestID:  requestID,
		UserID:     userID,
		Asset:      "CNY",
		Settlement: billing.Settlement{Actual: actual, Reserved: r.Amount, Breakdown: billing.Breakdown{Total: actual}},
		Entries:    consume.Denormalize(),
		// 实际 3000 个 token，预留只有 1500。
		Usage:       billing.Usage{PromptTokens: 1000, CompletionTokens: 2000},
		UsageSource: tokens.SourceMeasured,
		PricedAt:    time.Now(),
	}
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	if got := quotaUsedOf(t, db, upID); got != "1500.00000000" {
		t.Errorf("quota_used = %s，期望截到预留的 1500（而不是实际的 3000）", got)
	}
	if got := accountBalanceOf(t, db, ledger.PrepaidAccount(upID).String()); got != "45.00000000" {
		t.Errorf("prepaid 余额 = %s，期望 45（50 − 5）", got)
	}

	logged := logs.String()
	for _, want := range []string{
		"plan_id=" + itoa(upID),
		"actual=3000.00000000",
		"reserved=1500.00000000",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("截断日志里缺少 %q：%s", want, logged)
		}
	}
}

// 额度包覆盖的 pending 预留不冻钱包：对账的 frozen 不变量照旧成立，包占住的
// 面值不被当成钱。
//
// 会红的写法只有一种：把 plan_reserved_amount 混进「pending 预留之和」。那时
// 权威值是 10、快照是 0，对账会报一条 frozen 不一致。
func TestReconcileAcceptsPendingPlanReservation(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "amount", "50", "50")
	upID := seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	report := reconcile(t, db, noRepair())
	if got := findingsFor(report, userID); len(got) != 0 {
		t.Fatalf("在途的额度包预留不该被当成冻结的钱: %v", got)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("钱包冻结 = %s，期望 0（额度包覆盖的请求一分没冻）", frozen)
	}
	if got := accountBalanceOf(t, db, ledger.PrepaidAccount(upID).String()); got != "50.00000000" {
		t.Errorf("prepaid 余额 = %s，期望 50（在途不扣面值）", got)
	}
}

// 释放不动钱包、不记已用量；在途占用随之消失，下一次请求还能用这个包。
func TestReleasePlanReservationFreesQuota(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "token", "1500", "50", "gpt-4o")
	upID := seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	r := reservation(t, userID, requestID, "10", "CNY")
	r.OutputTokens = 500
	if _, err := s.SaveReservation(ctx, req, r); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	row := readBillingPlanRow(t, db, requestID)
	if row.status != "released" {
		t.Errorf("状态 = %s，期望 released", row.status)
	}
	if got := quotaUsedOf(t, db, upID); got != "0.00000000" {
		t.Errorf("quota_used = %s，释放不该记已用量", got)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("钱包冻结 = %s，期望 0", frozen)
	}

	// 占用释放之后，同样一笔请求还能再选到这个包（可用量刚好 1500）。
	next := reqID(t)
	nextReq := testRequest(ctx, t, db, userID, next)
	nextR := reservation(t, userID, next, "10", "CNY")
	nextR.OutputTokens = 500
	if _, err := s.SaveReservation(ctx, nextReq, nextR); err != nil {
		t.Fatalf("释放后的再次预留失败: %v", err)
	}
	if got := readBillingPlanRow(t, db, next); !got.userPlanID.Valid || got.userPlanID.Int64 != upID {
		t.Errorf("释放后应能再次选中同一个包，实际 user_plan_id=%+v", got.userPlanID)
	}
}

// 没有钱包行的用户（只有额度包）也能释放：额度包那条路从来没冻过钱包，
// 「减 0」不该因为缺行而失败。这是「余额为零、只有额度包的账户能正常用」
// 这条承诺的最后一步。
func TestReleasePlanReservationWithoutWalletRow(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedBareUser(t, db)
	planID := seedPlan(t, ctx, db, "amount", "50", "50")
	seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("无钱包行的用户释放失败: %v", err)
	}
	if row := readBillingPlanRow(t, db, requestID); row.status != "released" {
		t.Errorf("状态 = %s，期望 released", row.status)
	}
}

// 流式补冻给包加面值与可用量两处，不动钱包；加不下时返回可识别的错误。
func TestAddReservationGrowsPlanReservation(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "token", "5000", "50", "gpt-4o")
	seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	r := reservation(t, userID, requestID, "10", "CNY")
	r.OutputTokens = 500
	if _, err := s.SaveReservation(ctx, req, r); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	if err := s.AddReservation(ctx, req, mustMoney(t, "2"), 500); err != nil {
		t.Fatalf("补冻失败: %v", err)
	}
	row := readBillingPlanRow(t, db, requestID)
	if row.planReservedAmount != "12.00000000" {
		t.Errorf("包面值占用 = %s，期望 12", row.planReservedAmount)
	}
	if row.reservedQuota != "2000.00000000" {
		t.Errorf("可用量占用 = %s，期望 2000（1500 + 500）", row.reservedQuota)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("钱包冻结 = %s，补冻不该动钱包", frozen)
	}

	// 剩余可用量 3000，再要 5000 个 token 加不下——必须中断这条流。
	err := s.AddReservation(ctx, req, mustMoney(t, "1"), 5000)
	if !errors.Is(err, pipeline.ErrInsufficientAvailable) {
		t.Fatalf("超量补冻应返回 ErrInsufficientAvailable，实际 %v", err)
	}
	if got := readBillingPlanRow(t, db, requestID).reservedQuota; got != "2000.00000000" {
		t.Errorf("失败的补冻改动了占用：%s", got)
	}
}

// 扣费顺序：余额优先时钱包够就整单走余额，不够才找额度包；只走余额时不看包。
func TestChargeOrderSelectsSource(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "amount", "50", "50")
	seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	balanceFirst := New(db)
	balanceFirst.SetChargeOrderSource(fixedChargeOrder("balance_first"))
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := balanceFirst.SaveReservation(ctx, req,
		reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if row := readBillingPlanRow(t, db, requestID); row.userPlanID.Valid {
		t.Fatalf("余额优先且余额够时不该动额度包，实际选中 %d", row.userPlanID.Int64)
	}

	// 余额只剩 1，覆盖不了 10：这时才轮到额度包。
	poor := seed(t, db, "1")
	seedUserPlan(t, ctx, db, poor, planID, "50", time.Now().Add(time.Hour))
	fallbackID := reqID(t)
	fallbackReq := testRequest(ctx, t, db, poor, fallbackID)
	if _, err := balanceFirst.SaveReservation(ctx, fallbackReq,
		reservation(t, poor, fallbackID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if row := readBillingPlanRow(t, db, fallbackID); !row.userPlanID.Valid {
		t.Fatal("余额不够时应回落到额度包")
	}

	balanceOnly := New(db)
	balanceOnly.SetChargeOrderSource(fixedChargeOrder("balance_only"))
	onlyID := reqID(t)
	onlyReq := testRequest(ctx, t, db, poor, onlyID)
	_, err := balanceOnly.SaveReservation(ctx, onlyReq,
		reservation(t, poor, onlyID, "10", "CNY"))
	if !errors.Is(err, pipeline.ErrInsufficientAvailable) {
		t.Fatalf("只走余额且余额不够时应拒绝，实际 %v", err)
	}
}

// 默认（没接读点）按先额度包后余额走，与平台默认值一致。
func TestDefaultChargeOrderIsPlanFirst(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "amount", "50", "50")
	seedUserPlan(t, ctx, db, userID, planID, "50", time.Now().Add(time.Hour))

	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if row := readBillingPlanRow(t, db, requestID); !row.userPlanID.Valid {
		t.Fatal("默认顺序应是先额度包后余额")
	}
}

// 面值为零（或已在途占满）的包不再参与选源：任一维度耗尽即失效。
func TestReserveSkipsPlanWithExhaustedFace(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	planID := seedPlan(t, ctx, db, "amount", "5", "5")
	seedUserPlan(t, ctx, db, userID, planID, "5", time.Now().Add(time.Hour))

	first, next := reqID(t), reqID(t)
	if _, err := s.SaveReservation(ctx, testRequest(ctx, t, db, userID, first),
		reservation(t, userID, first, "5", "CNY")); err != nil {
		t.Fatalf("第一笔预留失败: %v", err)
	}
	// 面值全被第一笔占着，第二笔必须落到余额（用户有 100）。
	req := testRequest(ctx, t, db, userID, next)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, next, "5", "CNY")); err != nil {
		t.Fatalf("第二笔预留失败: %v", err)
	}
	if row := readBillingPlanRow(t, db, next); row.userPlanID.Valid {
		t.Fatalf("面值占满后不该再选这个包，实际 %d", row.userPlanID.Int64)
	}
}

// pendingPlanAmount 读一个订阅**在途**预留占着的面值之和。
func pendingPlanAmount(t *testing.T, db *sql.DB, userPlanID int64) string {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(plan_reserved_amount), 0)::numeric(20, 8)::text
		  FROM billing_requests
		 WHERE user_plan_id = $1 AND status = 'pending'`, userPlanID).Scan(&raw); err != nil {
		t.Fatalf("读在途面值失败: %v", err)
	}
	return raw
}

// waitForLockWaiters 等到 want 个会话停在锁等待上。
//
// 用例用外部事务先锁住订阅行，让两笔预留都停在「拿锁」这一步；只有确认它们
// 真的等在那里，释放锁之后的交错才是确定的——不靠调度运气。
func waitForLockWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := db.QueryRowContext(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock' AND state = 'active'
			   AND query LIKE '%user_plans%'`).Scan(&n); err != nil {
			t.Fatalf("读锁等待数失败: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("只有 %d 个会话在等锁，期望 %d", n, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 同一用户、两把不同 key 同时预留同一个包：只有一笔能成功。
//
// 这是 #918 的阻断用例。若把在途求和与 FOR UPDATE 写在同一条语句里，READ
// COMMITTED 下两笔在等锁前就各自取好了语句快照，对方提交的 pending 行看不见
// ——两笔都放行，在途之和超过面值，结算把 prepaid 扣成负数。
//
// 用例用一个外部事务先把订阅行锁住，让两笔都停在拿锁这一步，确认它们都在等
// 之后再放行：两笔的语句快照因此都取在对方提交之前，交错是确定的。
func TestConcurrentPlanReservationsDoNotOversell(t *testing.T) {
	db := testDB(t)
	// 外部事务占一条连接，两笔预留各占一条，轮询再占一条——默认的 4 条刚好
	// 卡满，留出余量免得轮询自己排队。
	db.SetMaxOpenConns(8)
	s := New(db)
	ctx := context.Background()

	// 余额 1：包覆盖不住时回落到余额，1 不够 10，于是失败那笔可判为
	// ErrInsufficientAvailable 而不是「没有钱包」。
	userID := seed(t, db, "1")
	planID := seedPlan(t, ctx, db, "amount", "10", "10")
	upID := seedUserPlan(t, ctx, db, userID, planID, "10", time.Now().Add(time.Hour))

	keyA := seedAPIKey(ctx, t, db, userID)
	keyB := seedAPIKey(ctx, t, db, userID)
	reqA := testRequestWithKey(userID, reqID(t), keyA)
	reqB := testRequestWithKey(userID, reqID(t), keyB)
	resA := reservation(t, userID, reqA.RequestID, "10", "CNY")
	resB := reservation(t, userID, reqB.RequestID, "10", "CNY")

	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启阻塞事务失败: %v", err)
	}
	defer func() { _ = blocker.Rollback() }()
	var one int
	err = blocker.QueryRowContext(ctx,
		`SELECT 1 FROM user_plans WHERE id = $1 FOR UPDATE`, upID).Scan(&one)
	if err != nil {
		t.Fatalf("锁住订阅行失败: %v", err)
	}

	type outcome struct {
		req pipeline.Request
		id  int64
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, pair := range []struct {
		req pipeline.Request
		res *pipeline.Reservation
	}{{reqA, resA}, {reqB, resB}} {
		pair := pair
		go func() {
			<-start
			id, saveErr := s.SaveReservation(ctx, pair.req, pair.res)
			results <- outcome{req: pair.req, id: id, err: saveErr}
		}()
	}
	close(start)
	waitForLockWaiters(t, db, 2)
	err = blocker.Rollback()
	if err != nil {
		t.Fatalf("释放订阅行锁失败: %v", err)
	}

	var winners []outcome
	var failures []error
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err == nil {
			winners = append(winners, got)
		} else {
			failures = append(failures, got.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("面值 10 的包只装得下一笔 10 的预留，实际成功了 %d 笔", len(winners))
	}
	if len(failures) != 1 || !errors.Is(failures[0], pipeline.ErrInsufficientAvailable) {
		t.Fatalf("失败的那一笔 = %v，期望 ErrInsufficientAvailable", failures)
	}

	// 结算成功的那一笔：面值扣 5，余额不为负；在途随之清零。
	winner := winners[0]
	actual := mustMoney(t, "5")
	consume, err := ledger.Consume(ledger.ConsumeParams{
		TxnID:            ledger.NewTxnID(),
		UserID:           userID,
		Asset:            ledger.AssetCNY,
		Amount:           actual,
		PrepaidAccount:   ledger.PrepaidAccount(upID),
		BillingRequestID: winner.id,
	})
	if err != nil {
		t.Fatalf("构造消费分录失败: %v", err)
	}
	charge := &pipeline.Charge{
		RequestID:   winner.req.RequestID,
		UserID:      userID,
		Asset:       "CNY",
		Settlement:  billing.Settlement{Actual: actual, Reserved: mustMoney(t, "10"), Breakdown: billing.Breakdown{Total: actual}},
		Entries:     consume.Denormalize(),
		Usage:       billing.Usage{PromptTokens: 100, CompletionTokens: 100},
		UsageSource: tokens.SourceMeasured,
		PricedAt:    time.Now(),
	}
	if err := s.Save(ctx, winner.req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	if got := accountBalanceOf(t, db, ledger.PrepaidAccount(upID).String()); got != "5.00000000" {
		t.Errorf("prepaid 余额 = %s，期望 5（10 − 5，不能为负）", got)
	}
	if got := pendingPlanAmount(t, db, upID); got != "0.00000000" {
		t.Errorf("结算后仍有 %s 面值挂在在途上", got)
	}
}
