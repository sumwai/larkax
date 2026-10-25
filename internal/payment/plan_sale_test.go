package payment

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/plans"
)

// 这一组钉「买额度包」这条链：下单带 plan_key、付款通知到达后发放订阅并写
// plan_purchase 分录。充值的判据在 service_test.go，两条路共用同一个状态机，
// 这里只测它们分叉的那几处。

// seedSalePlan 建一个可售的平台包。
func seedSalePlan(t *testing.T, ctx context.Context, db *sql.DB, key, face string, validityDays int) plans.Plan {
	t.Helper()
	p, err := plans.New(db).CreatePlan(ctx, plans.CreatePlanInput{
		Key:          key,
		Name:         "可售额度包 " + key,
		Asset:        ledger.AssetCNY,
		Unit:         plans.UnitAmount,
		FaceValue:    money.MustParse(face),
		ValidityDays: &validityDays,
	})
	if err != nil {
		t.Fatalf("建额度包 %s 失败: %v", key, err)
	}
	return p
}

// seedMerchantPlan 直接写一个归属商户的包（CreatePlan 没有归属入参）。
func seedMerchantPlan(t *testing.T, ctx context.Context, db *sql.DB, key string, ownerID int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (key, name, asset, unit, quota_total, face_value, owner_user_id, validity_days)
		VALUES ($1, $2, 'CNY', 'amount', 50, 50, $3, 30)`,
		key, "商户额度包 "+key, ownerID); err != nil {
		t.Fatalf("建商户包 %s 失败: %v", key, err)
	}
}

// createPlanOrder 用只给 plan_key 的请求下一张单：金额与币种都由服务端从包里取。
func (h *harness) createPlanOrder(t *testing.T, ctx context.Context, userID int64, orderNo, planKey string) Order {
	t.Helper()
	result, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: orderNo,
		UserID:  userID,
		PlanKey: planKey,
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	return result.Order
}

// userPlanByRef 读一条按订单号发放的订阅。
func userPlanByRef(t *testing.T, ctx context.Context, db *sql.DB, ref string) (id int64, planKey string, expires time.Time) {
	t.Helper()
	var row struct {
		id        int64
		key       string
		periodEnd sql.NullTime
	}
	if err := db.QueryRowContext(ctx, `
		SELECT up.id, p.key, up.period_end
		  FROM user_plans up JOIN plans p ON p.id = up.plan_id
		 WHERE up.ref = $1`, ref).Scan(&row.id, &row.key, &row.periodEnd); err != nil {
		t.Fatalf("读订阅失败: %v", err)
	}
	if !row.periodEnd.Valid {
		t.Fatalf("订阅 %d 没有到期时刻", row.id)
	}
	return row.id, row.key, row.periodEnd.Time
}

// 付款通知到达后：发放一条订阅、面值进 prepaid、通道科目记应收，全是
// plan_purchase 一档分录；用户钱包既不要求存在，也不被改动。
func TestPlanPurchaseGrantsSubscriptionAndPostsPurchaseEntries(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlan(t, ctx, h.db, "sale-basic", "88", 30)

	// 刻意不给这个用户开钱包：买额度包不动钱包，那条前置不该挡在这里。
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-BUY-1"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	if order.Amount.Cmp(plan.FaceValue) != 0 {
		t.Errorf("支付单金额 = %s，期望从包里取到的 %s", order.Amount, plan.FaceValue)
	}
	if order.Asset != plan.Asset {
		t.Errorf("支付单币种 = %s，期望从包里取到的 %s", order.Asset, plan.Asset)
	}
	if order.PlanKey != plan.Key {
		t.Errorf("支付单的额度包 = %q，期望 %q", order.PlanKey, plan.Key)
	}

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	fee := money.MustParse("0.88")
	result, err := h.service.Notify(ctx, stubName, Notification{
		OrderNo:      orderNo,
		GatewayTxnID: "TRADE-" + orderNo,
		Status:       StatusPaid,
		Amount:       plan.FaceValue,
		Asset:        plan.Asset,
		Fee:          fee,
		PaidAt:       paidAt,
	})
	if err != nil {
		t.Fatalf("处理通知失败: %v", err)
	}
	if !result.Applied {
		t.Fatal("首次通知应当写分录并发放订阅")
	}
	if result.Order.Status != StatusPaid {
		t.Errorf("支付单状态 = %q，期望 %q", result.Order.Status, StatusPaid)
	}

	subID, planKey, expires := userPlanByRef(t, ctx, h.db, orderNo)
	if planKey != plan.Key {
		t.Errorf("订阅的额度包 = %q，期望 %q", planKey, plan.Key)
	}
	if want := paidAt.AddDate(0, 0, 30); expires.UnixMicro() != want.UnixMicro() {
		t.Errorf("订阅到期 = %s，期望收款时刻加 30 天 = %s", expires, want)
	}

	// 账本那一笔的形状：四条、平、都是 plan_purchase、方向与金额逐格对得上。
	// 判据全落在库里的行上（见 assertPurchaseLedger）。
	assertPurchaseLedger(t, ctx, h.db, orderNo, plan.FaceValue, fee)

	// prepaid 的账面余额就是这一份面值：断言取账本，不取列上的镜像。
	prepaid := ledger.PrepaidAccount(subID).String()
	var balance string
	if err := h.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		  FROM ledger_entries WHERE account = $1 AND asset = 'CNY'`, prepaid).Scan(&balance); err != nil {
		t.Fatalf("读 prepaid 余额失败: %v", err)
	}
	if balance != plan.FaceValue.String() {
		t.Errorf("prepaid:%d 的余额 = %s，期望 %s", subID, balance, plan.FaceValue)
	}

	// 钱包一行都不该有：账本没有写用户科目，也就不会去建快照。
	var wallets int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&wallets); err != nil {
		t.Fatalf("数钱包行失败: %v", err)
	}
	if wallets != 0 {
		t.Errorf("买额度包不该建钱包，实际 %d 行", wallets)
	}
}

// 下单给的金额与包的面值不符时拒绝，并且一张单子都不落。
func TestPlanPurchaseRejectsAmountMismatch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlan(t, ctx, h.db, "sale-amount", "88", 30)
	userID := seedUser(t, ctx, h.db)

	const orderNo = "ORD-PLAN-AMOUNT"
	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo:     orderNo,
		UserID:      userID,
		Asset:       ledger.AssetCNY,
		AssetGiven:  true,
		Amount:      money.MustParse("1.00"),
		AmountGiven: true,
		PlanKey:     plan.Key,
	})
	if err == nil {
		t.Fatal("金额与面值不符时应当拒绝")
	}
	if !strings.Contains(err.Error(), "面值是") {
		t.Errorf("报错没有点名面值：%v", err)
	}
	var orders int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&orders); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if orders != 0 {
		t.Errorf("被拒的下单不该落库，实际 %d 行", orders)
	}
}

// 调用方明确给了金额 0：拒绝，文案点名金额，且一张单子都不落。
//
// 「给没给」看 AmountGiven，不看 Amount 是不是零：money.Amount 没有「未指定」
// 这个值，把 0 当未指定会让「调用方明确给了 0」与「调用方没给」变成同一件事，
// 而前者会被下面那一步静默覆盖成面值——调用方以为自己下的是别的金额。
func TestPlanPurchaseRejectsExplicitZeroAmount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlan(t, ctx, h.db, "sale-zero-given", "88", 30)
	userID := seedUser(t, ctx, h.db)

	const orderNo = "ORD-PLAN-ZERO-GIVEN"
	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo:     orderNo,
		UserID:      userID,
		Amount:      money.Zero,
		AmountGiven: true,
		PlanKey:     plan.Key,
	})
	if err == nil {
		t.Fatal("明确给了金额 0 时应当拒绝")
	}
	if !strings.Contains(err.Error(), "金额必须为正数") {
		t.Errorf("报错没有点名金额：%v", err)
	}
	var orders int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&orders); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if orders != 0 {
		t.Errorf("被拒的下单不该落库，实际 %d 行", orders)
	}
}

// 调用方明确给了负数金额：同样拒绝。
func TestPlanPurchaseRejectsNegativeAmount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlan(t, ctx, h.db, "sale-negative-amount", "88", 30)
	userID := seedUser(t, ctx, h.db)

	const orderNo = "ORD-PLAN-NEGATIVE"
	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo:     orderNo,
		UserID:      userID,
		Amount:      money.MustParse("-1"),
		AmountGiven: true,
		PlanKey:     plan.Key,
	})
	if err == nil {
		t.Fatal("金额为负数时应当拒绝")
	}
	if !strings.Contains(err.Error(), "金额必须为正数") || !strings.Contains(err.Error(), "-1") {
		t.Errorf("报错没有点名金额与它收到的值：%v", err)
	}
	var orders int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&orders); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if orders != 0 {
		t.Errorf("被拒的下单不该落库，实际 %d 行", orders)
	}
}

// 调用方没给金额（AmountGiven=false）：按包的面值下单。
//
// 这条钉的是 `wallet payment create --plan` 不带 --amount 的正常用法：
// 把「没给」判成错误，这条路就走不通了。
func TestPlanPurchaseWithoutAmountTakesFaceValue(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlan(t, ctx, h.db, "sale-no-amount", "88", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)

	const orderNo = "ORD-PLAN-NO-AMOUNT"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	if order.Amount.Cmp(plan.FaceValue) != 0 {
		t.Errorf("落库金额 = %s，期望按面值 %s 下单", order.Amount, plan.FaceValue)
	}
	if order.Asset != plan.Asset {
		t.Errorf("落库币种 = %s，期望由包决定的 %s", order.Asset, plan.Asset)
	}
	var stored string
	if err := h.db.QueryRowContext(ctx,
		`SELECT amount::text FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&stored); err != nil {
		t.Fatalf("读支付单失败: %v", err)
	}
	if stored != plan.FaceValue.String() {
		t.Errorf("库里那一行的金额 = %s，期望 %s", stored, plan.FaceValue)
	}
}

// 下单给的币种与包的币种不符时拒绝，报错同时点名两个币种，且一张单子都不落。
//
// 金额相符、只有币种不符：这一格只能由「币种由商品决定」那条判据挡住。
func TestPlanPurchaseRejectsAssetMismatch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlan(t, ctx, h.db, "sale-asset", "88", 30)
	userID := seedUser(t, ctx, h.db)

	const orderNo = "ORD-PLAN-ASSET"
	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo:     orderNo,
		UserID:      userID,
		Asset:       "USDT",
		AssetGiven:  true,
		Amount:      plan.FaceValue,
		AmountGiven: true,
		PlanKey:     plan.Key,
	})
	if err == nil {
		t.Fatal("币种与包不符时应当拒绝")
	}
	// 两个币种都要出现：只写其中一个，运维看不出是哪一边写的。
	if !strings.Contains(err.Error(), string(plan.Asset)) || !strings.Contains(err.Error(), "USDT") {
		t.Errorf("报错要同时点名包的币种 %q 与下单给的 %q：%v", plan.Asset, "USDT", err)
	}
	var orders int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&orders); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if orders != 0 {
		t.Errorf("被拒的下单不该落库，实际 %d 行", orders)
	}
}

// 商户包今天不售卖：拒绝，并且报错点名归属。
func TestPlanPurchaseRejectsMerchantPlan(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ownerID := seedUserWithoutWallet(t, ctx, h.db)
	seedMerchantPlan(t, ctx, h.db, "sale-merchant", ownerID)
	userID := seedUser(t, ctx, h.db)

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: "ORD-PLAN-MERCHANT",
		UserID:  userID,
		PlanKey: "sale-merchant",
	})
	if err == nil {
		t.Fatal("商户包应当拒绝售卖")
	}
	if !strings.Contains(err.Error(), "归属商户") || !strings.Contains(err.Error(), strconv.FormatInt(ownerID, 10)) {
		t.Errorf("报错没有点名归属：%v", err)
	}
}

// 没有售卖有效期的包不可售：拒绝，并且报错点名 validity_days。
func TestPlanPurchaseRejectsPlanWithoutValidity(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := plans.New(h.db).CreatePlan(ctx, plans.CreatePlanInput{
		Key:       "sale-no-validity",
		Name:      "没有有效期的包",
		Asset:     ledger.AssetCNY,
		Unit:      plans.UnitAmount,
		FaceValue: money.MustParse("30"),
	}); err != nil {
		t.Fatalf("建包失败: %v", err)
	}
	userID := seedUser(t, ctx, h.db)

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: "ORD-PLAN-NO-VALIDITY",
		UserID:  userID,
		PlanKey: "sale-no-validity",
	})
	if err == nil {
		t.Fatal("没有有效期的包应当拒绝售卖")
	}
	if !strings.Contains(err.Error(), "validity_days") {
		t.Errorf("报错没有点名 validity_days：%v", err)
	}
}

// 停用的包不可售：拒绝，并且报错把复原命令一并指出。
//
// 同一个 switch 的三条守卫里，商户包（TestPlanPurchaseRejectsMerchantPlan）与
// 没有有效期（TestPlanPurchaseRejectsPlanWithoutValidity）都有下单侧用例，停用
// 这条没有——这是不对称，不是工具口径。这一支今天**没有变异体**：那道门开着的
// 变异器里没有一个碰一元 `!`（INVERT_LOGICAL 只管 `&&`/`||`，日志里 165:15 的
// `||`、192:21 的 `&&` 是它的料；150 的 `if !hasWallet` 同样一个变异体都没有），
// 所以这道门结构上保护不到它，用例是唯一的东西。
func TestPlanPurchaseRejectsDisabledPlan(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-disabled", "39", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-DISABLED"

	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET enabled = false WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("停用额度包失败: %v", err)
	}

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: orderNo,
		UserID:  userID,
		PlanKey: plan.Key,
	})
	if err == nil {
		t.Fatal("停用的包应当拒绝售卖")
	}
	// 报错要给两条信息：为什么拒（已停用），以及下一步怎么做（plan enable）。
	if !strings.Contains(err.Error(), "已停用") {
		t.Errorf("报错没有点名「已停用」：%v", err)
	}
	if !strings.Contains(err.Error(), "plan enable") {
		t.Errorf("报错没有给出复原命令 plan enable：%v", err)
	}
	var orders int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&orders); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if orders != 0 {
		t.Errorf("被拒的下单不该落库，实际 %d 行", orders)
	}
}

// 同一份付款通知重放 N 次（N=3）：只有一条订阅、一组分录，之后每一次都走
// applied=false。
//
// 判据是**查库计数**，不是看返回值：重放 N 次之后库里仍然只有四条分录、一行
// 订阅、prepaid 余额等于面值**一次**。只断 applied=false 的话，一个「返回值
// 说没写、其实写了两组」的实现照样过。
func TestPlanPurchaseReplayGrantsOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlan(t, ctx, h.db, "sale-replay", "66", 7)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-REPLAY"
	h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	paidAt := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	fee := money.MustParse("0.66")
	notify := func(at time.Time) (NotificationResult, error) {
		return h.service.Notify(ctx, stubName, Notification{
			OrderNo:      orderNo,
			GatewayTxnID: "TRADE-" + orderNo,
			Status:       StatusPaid,
			Amount:       plan.FaceValue,
			Asset:        plan.Asset,
			Fee:          fee,
			PaidAt:       at,
		})
	}
	first, err := notify(paidAt)
	if err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}
	if !first.Applied {
		t.Fatal("首次通知应当写入")
	}

	// 重放两次：通道重发时给的时间戳不同，到期时刻仍要与第一次一致。
	for i, at := range []time.Time{paidAt.Add(5 * time.Minute), paidAt.Add(9 * time.Minute)} {
		again, err := notify(at)
		if err != nil {
			t.Fatalf("第 %d 次重放失败: %v", i+2, err)
		}
		if again.Applied {
			t.Errorf("第 %d 次重放不该再写分录，也不该二次发放", i+2)
		}
	}

	var subs int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_plans WHERE ref = $1`, orderNo).Scan(&subs); err != nil {
		t.Fatalf("数订阅失败: %v", err)
	}
	if subs != 1 {
		t.Errorf("订阅 %d 条，期望 1 条", subs)
	}
	// 按**这个订阅的 prepaid 科目**数，不按 txn_id 数：万一哪天 txn 派生成
	// 别的东西、第二组落在另一个 txn_id 上，按 txn_id 数会看不见它，而账面
	// 确实翻倍了。prepaid:<订阅 id> 只在这一笔里出现一次，重放写第二组就会
	// 变成两行。不能按全表 plan_purchase 数：同一个测试库里还有别的用例的
	// 售卖分录。
	subID, _, _ := userPlanByRef(t, ctx, h.db, orderNo)
	prepaidAccount := ledger.PrepaidAccount(subID).String()
	var prepaidRows int
	var prepaidBalance string
	if err := h.db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		  FROM ledger_entries WHERE account = $1 AND asset = 'CNY'`,
		prepaidAccount).Scan(&prepaidRows, &prepaidBalance); err != nil {
		t.Fatalf("读 prepaid 分录失败: %v", err)
	}
	if prepaidRows != 1 {
		t.Errorf("prepaid:%d 的分录 %d 条，期望 1 条（重放不该再写一组）", subID, prepaidRows)
	}
	if prepaidBalance != plan.FaceValue.String() {
		t.Errorf("prepaid:%d 的余额 = %s，期望面值 %s 一次", subID, prepaidBalance, plan.FaceValue)
	}
	// 形状与方向也要在：四条、平、prepaid 为正、通道为负。
	assertPurchaseLedger(t, ctx, h.db, orderNo, plan.FaceValue, fee)
}

// ── 入账时核对「单子记的商品」与「包现在是什么」 ──────────────────────
//
// 下单时把面值与币种写进单子，入账时 GrantTx 会**重新读一次包**。中间运营改了
// 面值、币种、售卖有效期、启停或归属时，这张单与这个包已经不是同一件事了。
// 五项各有一条用例，售卖有效期那一项分改长、改短、清空三种情形：它们是独立的
// 守卫分支，合成一条会出现「一条断言挡两件事、红在哪条说不清」。

// seedSalePlanRow 建一个可售平台包，返回它当前那一行。
func seedSalePlanRow(t *testing.T, ctx context.Context, db *sql.DB, key, face string, days int) plans.Plan {
	t.Helper()
	seedSalePlan(t, ctx, db, key, face, days)
	p, err := plans.New(db).PlanByKey(ctx, key)
	if err != nil {
		t.Fatalf("读回额度包 %s 失败: %v", key, err)
	}
	return p
}

// notifyPaid 发一份「已支付」通知。
func (h *harness) notifyPaid(ctx context.Context, orderNo string, amount money.Amount, asset ledger.Asset, fee money.Amount, paidAt time.Time) (NotificationResult, error) {
	return h.service.Notify(ctx, stubName, Notification{
		OrderNo:      orderNo,
		GatewayTxnID: "TRADE-" + orderNo,
		Status:       StatusPaid,
		Amount:       amount,
		Asset:        asset,
		Fee:          fee,
		PaidAt:       paidAt,
	})
}

// postCallback 把一份通知从 HTTP 那一层送进真的 Handler，返回通道收到的响应。
//
// h.notifyPaid 直接调 Service，看不到「通道拿到哪个状态码」——那件事发生在
// writeServiceError 里。409 与 500 的分界只能在这一层钉。
func postCallback(ctx context.Context, h *harness, n Notification) *httptest.ResponseRecorder {
	channel := &scriptedChannel{notification: n}
	service := NewService(h.store, h.service.ledger, plans.New(h.db), map[string]Channel{stubName: channel})
	handler := NewHandler(service, stubName, "")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, CallbackPath(stubName),
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// assertCallbackRejectedWith500 断言通道收到的是回调入口的 500 与那句通用文案。
//
// 这是 409 与 500 的分界那一格：`!errors.Is(err, ErrOrderConflict)` 只证明没走
// 409 那条分支，不证明走的是 default → 500。将来谁把这类错误包一层哨兵、加一格
// 映射（比如 400），那条断言仍然绿，而通道会把这一单当成已处理、停止重试。
//
// 正文里**不带单号**：单号只在服务端日志里（default 分支回的是固定一句给通道
// 看的文案），所以这里钉的是那句话本身。
func assertCallbackRejectedWith500(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("通道应当收到 500（default 分支），实际 %d：%s", rec.Code, rec.Body.String())
	}
	const wantBody = `"error":"failed to process payment notification"`
	if !strings.Contains(rec.Body.String(), wantBody) {
		t.Errorf("响应体应当是那句通用文案 %s，实际 %s", wantBody, rec.Body.String())
	}
}

// readTxnEntries 从库里读回一笔交易的全部分录，拼成 ledger.Entry。
//
// 与 internal/ledger/store 的只读路径同一个思路：校验的对象是**库里那一批行**，
// 不是内存里刚构造的那一组。科目用 ledger.ParseAccount 解析，读不出来就让用例
// 失败——那说明账本里有一行归不到任何科目上。
func readTxnEntries(t *testing.T, ctx context.Context, db *sql.DB, txnID ledger.TxnID) []ledger.Entry {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT txn_id, account, asset, amount::text, entry_type
		  FROM ledger_entries WHERE txn_id = $1 ORDER BY id`, string(txnID))
	if err != nil {
		t.Fatalf("读交易 %s 的分录失败: %v", txnID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ledger.Entry
	for rows.Next() {
		var (
			rawTxn, account, asset, amountText, entryType string
		)
		if err := rows.Scan(&rawTxn, &account, &asset, &amountText, &entryType); err != nil {
			t.Fatalf("读分录失败: %v", err)
		}
		acct, acctErr := ledger.ParseAccount(account)
		if acctErr != nil {
			t.Fatalf("库里那一行的科目 %q 不合法: %v", account, acctErr)
		}
		amount, amountErr := money.Parse(amountText)
		if amountErr != nil {
			t.Fatalf("库里那一行的金额 %q 不合法: %v", amountText, amountErr)
		}
		out = append(out, ledger.Entry{
			TxnID:     ledger.TxnID(rawTxn),
			Account:   acct,
			Asset:     ledger.Asset(asset),
			Amount:    amount,
			EntryType: ledger.EntryType(entryType),
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历分录失败: %v", err)
	}
	return out
}

// assertPurchaseLedger 核对售卖那一笔在库里的形状，判据全部落在**库里的行**上。
//
// 四件事缺一件都不算对：
//
//  1. 正好四条。少了说明某一对被吞掉，多了说明写了两次；
//  2. `ledger.VerifyEntries` 放行——它按 txn_id 分组后逐组校验「和为零」，
//     库里的四条真的平才过得去；
//  3. 每条分录的 entry_type 都是 plan_purchase。混进 plan_grant 会让对账按类型
//     分组时把售卖算成赠送；
//  4. 方向与金额逐格对得上：prepaid 与 upstream_cost 为正（借），payment_gateway
//     与 platform_revenue 为负（贷）。**方向写反在金额上是对称的**，只断「有
//     分录」看不出来。
//
// 关于第 2 条能兜住什么：VerifyEntries 的重复判据键是
// {account, entry_type, asset, amount}，**不含 txn_id**，但它内部先按 txn_id
// 分组再逐组校验（entry.go:538-546），所以跨 txn 的同键不会互相触发。也就是说
// 它兜得住「同一 txn_id 里写了两次」，兜不住「写进了不同 txn_id 的第二组」——
// 那两组各自都平，逐组都判绿而账面翻倍。所以计数判据不能删，它是主判据，
// VerifyEntries 是补充。
func assertPurchaseLedger(t *testing.T, ctx context.Context, db *sql.DB, orderNo string,
	face, fee money.Amount) {
	t.Helper()
	entries := readTxnEntries(t, ctx, db, PlanTxnIDForOrder(orderNo))
	if len(entries) != 4 {
		t.Fatalf("交易 %s 的分录 %d 条，期望 4 条：%v", PlanTxnIDForOrder(orderNo), len(entries), entries)
	}
	// 空切片 VerifyEntries 报的是「没有分录可校验」，那看起来像不平。先断条数。
	if err := ledger.VerifyEntries(entries); err != nil {
		t.Errorf("库里的售卖分录不自洽（和不为零或方向错）：%v", err)
	}

	var sum money.Amount
	var prepaidAccount ledger.Account
	for _, e := range entries {
		if e.EntryType != ledger.EntryPlanPurchase {
			t.Errorf("科目 %s 的分录类型 = %q，期望 %q", e.Account, e.EntryType, ledger.EntryPlanPurchase)
		}
		sum = sum.Add(e.Amount)
		if strings.HasPrefix(e.Account.String(), "prepaid:") {
			prepaidAccount = e.Account
		}
	}
	if !sum.IsZero() {
		t.Errorf("四条分录之和 = %s，期望 0；方向写反时和会跟着翻，这里能立刻看出来", sum)
	}
	if prepaidAccount.IsZero() {
		t.Fatal("这一笔里没有 prepaid 科目的分录")
	}

	want := map[ledger.Account]money.Amount{
		prepaidAccount:                         face,
		ledger.PaymentGatewayAccount(stubName): face.Neg(),
		ledger.UpstreamCostAccount():           fee,
		ledger.PlatformRevenueAccount():        fee.Neg(),
	}
	got := make(map[ledger.Account]money.Amount, len(entries))
	for _, e := range entries {
		got[e.Account] = got[e.Account].Add(e.Amount)
	}
	for account, wantAmount := range want {
		if gotAmount, ok := got[account]; !ok {
			t.Errorf("缺科目 %s 的分录（期望 %s）", account, wantAmount)
		} else if gotAmount.Cmp(wantAmount) != 0 {
			t.Errorf("科目 %s 的金额 = %s，期望 %s", account, gotAmount, wantAmount)
		}
	}
}

// planSaleState 是「一次被拒的通知之后」要核对的三件事。
type planSaleState struct {
	subs   int
	orders int
	status Status
}

func planSaleStateOf(t *testing.T, ctx context.Context, db *sql.DB, orderNo string, txnID ledger.TxnID) planSaleState {
	t.Helper()
	var s planSaleState
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_plans WHERE ref = $1`, orderNo).Scan(&s.subs); err != nil {
		t.Fatalf("数订阅失败: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1`, string(txnID)).Scan(&s.orders); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&s.status); err != nil {
		t.Fatalf("读支付单状态失败: %v", err)
	}
	return s
}

// assertNothingGranted 是「通知被拒」的完整判据：单子仍 created、没有订阅、
// 没有分录。三件缺一件都不算拒成功——只断言返回错的话，一个「报错之前已经把
// 订阅写进去」的实现照样过。
func assertNothingGranted(t *testing.T, ctx context.Context, db *sql.DB, orderNo string) {
	t.Helper()
	got := planSaleStateOf(t, ctx, db, orderNo, PlanTxnIDForOrder(orderNo))
	if got.status != StatusCreated {
		t.Errorf("被拒之后支付单状态 = %q，期望仍是 %q", got.status, StatusCreated)
	}
	if got.subs != 0 {
		t.Errorf("被拒之后 user_plans 多了 %d 行，期望 0 行", got.subs)
	}
	if got.orders != 0 {
		t.Errorf("被拒之后 ledger_entries 多了 %d 行，期望 0 行", got.orders)
	}
}

// 下单之后包被改了面值：通知被拒，且单子仍 created、无订阅、无分录。
// 面值改回之后同一份通知能正常发放——证明拒的是「两张单对不上」，不是这张单坏了。
func TestPlanPurchaseRejectsFaceValueChangedAfterOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-face-changed", "88", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-FACE-CHANGED"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	paidAt := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	fee := money.MustParse("0.88")

	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET face_value = 99, quota_total = 99 WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("改面值失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, fee, paidAt)
	if err == nil {
		t.Fatal("包的面值在下单之后改过，通知应当被拒")
	}
	for _, want := range []string{"面值在售出之后改过", "88", "99", "先核对这张单", orderNo} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q（要点名哪一格、并带上两边的值）：%v", want, err)
		}
	}
	// 这一类**不能**映射成 409：409 会让通道当成「已经处理完」而停止重试，
	// 正是 handler 的 default 分支要避免的后果。它要落到 500 等人来核。
	if errors.Is(err, ErrOrderConflict) {
		t.Errorf("「包被改过」被映射成 409，通道会停止重试：%v", err)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)

	// 上一句只说明「没走 409 那条分支」。走真的 Handler 再看一眼通道实际收到
	// 什么码：这一类要落到 default 的 500，通道才会重试到有人来核。
	assertCallbackRejectedWith500(t, postCallback(ctx, h, Notification{
		OrderNo:      orderNo,
		GatewayTxnID: "TRADE-" + orderNo,
		Status:       StatusPaid,
		Amount:       order.Amount,
		Asset:        order.Asset,
		Fee:          fee,
		PaidAt:       paidAt,
	}))

	// 面值改回去：同一份通知应当能发出去，说明拦住它的是「对不上」而不是别的。
	if _, updErr := h.db.ExecContext(ctx,
		`UPDATE plans SET face_value = 88, quota_total = 88 WHERE key = $1`, plan.Key); updErr != nil {
		t.Fatalf("还原面值失败: %v", updErr)
	}
	result, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, fee, paidAt)
	if err != nil {
		t.Fatalf("面值还原之后通知应当成功: %v", err)
	}
	if !result.Applied {
		t.Error("面值还原之后的这次通知应当真的发放")
	}
	got := planSaleStateOf(t, ctx, h.db, orderNo, PlanTxnIDForOrder(orderNo))
	if got.subs != 1 || got.orders != 4 || got.status != StatusPaid {
		t.Errorf("还原之后 = 订阅 %d 条 / 分录 %d 条 / 单子 %q，期望 1 / 4 / %q",
			got.subs, got.orders, got.status, StatusPaid)
	}
}

// 下单之后包的币种被改：通知被拒，且单子仍 created、无订阅、无分录。
//
// 库里只放得下本币（money 与 ledger 都只认 CNY），所以这里改的是一个读侧
// 认不出的值：scanPlan 在读到它时就报「资产 ... 不受支持」。拒绝同样成立，
// 只是那句话出自读侧，而不是 purchasePlanMismatch 的币种分支——断言因此
// 只钉「拒 + 三件齐全」，不去钉某一句文案。
func TestPlanPurchaseRejectsAssetChangedAfterOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-asset-changed", "60", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-ASSET-CHANGED"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET asset = 'USDT' WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("改币种失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.6"),
		time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("包的币种在下单之后改过，通知应当被拒")
	}
	if !strings.Contains(err.Error(), plan.Key) || !strings.Contains(err.Error(), "USDT") {
		t.Errorf("报错没有点名包与改后的币种：%v", err)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)
}

// 下单之后包被停用：通知被拒，且单子仍 created、无订阅、无分录。
func TestPlanPurchaseRejectsPlanDisabledAfterOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-disabled-after", "50", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-DISABLED-AFTER"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET enabled = false WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("停用包失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.5"),
		time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("包在下单之后被停用，通知应当被拒")
	}
	// 钉「在售出之后被停用」这句，而不是泛泛的「停用」：GrantTx 里还有一条
	// 更早的「已禁用，不能再发放」守卫，它同样会拦住这次通知。只断言「停用」
	// 的话，入账侧这条新守卫被删掉也照样绿。
	if !strings.Contains(err.Error(), "在售出之后被停用") || !strings.Contains(err.Error(), plan.Key) {
		t.Errorf("报错没有点名「包在售出之后被停用」：%v", err)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)
}

// 下单时是平台包，入账前被改成商户包：通知被拒，且单子仍 created、无订阅、
// 无分录。
//
// **入账侧必须有自己的归属守卫**：下单时 `applyPlan` 只挡住「一开始就是商户包」
// 的那一格，而归属可以在下单之后被改。归属改到商户名下时发放本身没问题，
// 有问题的是这笔钱该记给谁——那要人工决定，所以停在发放之前。
func TestPlanPurchaseRejectsPlanSoldToMerchantAfterOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-merchant-after", "40", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-MERCHANT-AFTER"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	ownerID := seedUserWithoutWallet(t, ctx, h.db)

	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET owner_user_id = $2 WHERE key = $1`, plan.Key, ownerID); err != nil {
		t.Fatalf("改归属失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.4"),
		time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("包在下单之后被改成商户包，通知应当被拒")
	}
	if !strings.Contains(err.Error(), "归属商户") || !strings.Contains(err.Error(), strconv.FormatInt(ownerID, 10)) {
		t.Errorf("报错没有点名归属：%v", err)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)
}

// 同一单号重放但手续费不同：返回订单冲突，不新增订阅、不新增分录。
// 手续费改回原值再重放：正常返回 applied=false。
//
// 这条钉的是 Go 侧的显式核对：售卖的重放在 GrantTx 里早返回，账本层根本
// 没有机会比较内容。
func TestPlanPurchaseReplayWithDifferentFeeConflicts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-replay-fee", "70", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-REPLAY-FEE"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	txnID := PlanTxnIDForOrder(orderNo)
	paidAt := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

	first, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.7"), paidAt)
	if err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}
	if !first.Applied {
		t.Fatal("首次通知应当发放")
	}

	_, err = h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.9"), paidAt.Add(time.Minute))
	if err == nil {
		t.Fatal("重放换了手续费应当报冲突，而不是静默当重放")
	}
	if !errors.Is(err, ErrOrderConflict) {
		t.Errorf("错误没有包成 ErrOrderConflict：%v", err)
	}
	for _, want := range []string{"手续费", "0.7", "0.9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q：%v", want, err)
		}
	}
	if got := planSaleStateOf(t, ctx, h.db, orderNo, txnID); got.subs != 1 || got.orders != 4 {
		t.Errorf("冲突之后 = 订阅 %d 条 / 分录 %d 条，期望 1 / 4（不该新增）", got.subs, got.orders)
	}

	// 手续费回到原值：这次是真重放，返回 applied=false。
	third, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.7"), paidAt.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("原样重放应当成功: %v", err)
	}
	if third.Applied {
		t.Error("原样重放不该再写分录")
	}
	if got := planSaleStateOf(t, ctx, h.db, orderNo, txnID); got.subs != 1 || got.orders != 4 {
		t.Errorf("重放之后 = 订阅 %d 条 / 分录 %d 条，期望 1 / 4", got.subs, got.orders)
	}
}

// 账本层发现「同一个交易号内容不同」时，那条错误要带着订单号一路走到 409。
//
// 这一格与 TestPlanPurchaseReplayWithDifferentFeeConflicts 不是同一层：那一格是
// 支付层自己比出来的（售卖的重放不写分录，账本没有机会发现），这一格是**账本**
// 比出来的——这个交易号底下已经写着别的内容。两层最后都会落到
// notifyLocked 的 errors.Is(store.ErrTxnConflict)，再由它转成 ErrOrderConflict。
//
// 钉两件事：
//   - `purchasePlan` 那句包装用的是 %w：写成 %v 时 errors.Is 断链，409 变 500
//     （通道从此停止重试）；
//   - 报错里带订单号：账本那句只有交易号，「内容与这次要写的不相同」落到日志里
//     之后，运维还得从 uuid 反查是哪张单。
func TestPlanPurchaseLedgerTxnConflictKeepsOrderNo(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-ledger-conflict", "60", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-LEDGER-CONFLICT"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	txnID := PlanTxnIDForOrder(orderNo)

	// 先在这个交易号底下写一条别的内容（真实现里可能是手工入账写错了单号）。
	// 账本只做只追加，所以这里直接写一行即可：PostTransactionTx 读回它之后，
	// 与这次构造的整组一比就知道对不上。
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
		VALUES ($1, 'prepaid:1', 'CNY', 1, 'plan_purchase')`, string(txnID)); err != nil {
		t.Fatalf("写预置分录失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.6"), time.Now().UTC())
	if err == nil {
		t.Fatal("这个交易号底下已有别的内容，通知应当报冲突")
	}
	if !errors.Is(err, ErrOrderConflict) {
		t.Errorf("错误没有包成 ErrOrderConflict（包装一旦断链，409 就变 500）：%v", err)
	}
	for _, want := range []string{orderNo, "内容与这次要写的不同"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q：%v", want, err)
		}
	}

	// 整笔事务回滚：预置的那一行还在，没有新增订阅、也没有新增分录。
	got := planSaleStateOf(t, ctx, h.db, orderNo, txnID)
	if got.subs != 0 {
		t.Errorf("冲突之后 user_plans 多了 %d 行，期望 0 行", got.subs)
	}
	if got.orders != 1 {
		t.Errorf("冲突之后这个交易号下有 %d 条分录，期望仍是预置的那 1 条", got.orders)
	}
	if got.status != StatusCreated {
		t.Errorf("冲突之后支付单状态 = %q，期望仍是 %q", got.status, StatusCreated)
	}
}

// 通知送晚：收款时刻早于「现在 − 售卖天数」，算出的到期时刻已经过去。
//
// 这不是「包被改过」那一类——包一动没动，是通道的回调来得太晚。它同样只能
// 人工处理（补发或退款），改包再重发不管用：GrantTx 每次都从收款时刻重新算，
// 算出来的仍然是过去。
//
// 钉三件事：这句话点名了订单号（运维要知道核哪张单）；它**不能**是 409
// （409 会让通道停止重试）；而通道**实际**收到的是 default 分支的 500——
// 只断 `!errors.Is(ErrOrderConflict)` 只说清前一半，给这类错误另加一格映射
// 时它照样绿。
func TestPlanPurchaseRejectsLateNotification(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-late-notify", "40", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-LATE-NOTIFY"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	// 40 天前付的款、包卖 30 天：到期时刻在 10 天前。
	latePaidAt := time.Now().Add(-40 * 24 * time.Hour).UTC()
	fee := money.MustParse("0.4")
	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, fee, latePaidAt)
	if err == nil {
		t.Fatal("收款时刻早于「现在 − 天数」时通知应当被拒")
	}
	for _, want := range []string{orderNo, "不晚于现在"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q：%v", want, err)
		}
	}
	if errors.Is(err, ErrOrderConflict) {
		t.Errorf("「通知送晚」被映射成 409，通道会停止重试：%v", err)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)

	// 通道实际收到的码：这一类同样要落到 default 的 500，而不是 4xx——包一动
	// 没动，重发算出来还是同一个过去的时刻，本来就该有人来核。
	assertCallbackRejectedWith500(t, postCallback(ctx, h, Notification{
		OrderNo:      orderNo,
		GatewayTxnID: "TRADE-" + orderNo,
		Status:       StatusPaid,
		Amount:       order.Amount,
		Asset:        order.Asset,
		Fee:          fee,
		PaidAt:       latePaidAt,
	}))

	// 重发不管用：到期时刻每次都从收款时刻重算，算出来仍是过去。
	if _, retryErr := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset,
		fee, latePaidAt); retryErr == nil {
		t.Fatal("同一份通知重发仍然发不出去：这一单要人工处理")
	}
}

// 同一 order_no 但买的是**另一个包**：下单时就冲突，不新增任何东西。
//
// 这条走的是下单侧的幂等判据（store.CreateTx 比较内容），不是通知侧。它与
// 上面那条是两处不同的闸门：一个挡「同一个单号被用来买两件商品」，一个挡
// 「同一件商品的两份通知说了不同的手续费」。
func TestPlanPurchaseRejectsOrderNoReusedForAnotherPlan(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	first := seedSalePlan(t, ctx, h.db, "sale-reuse-first", "50", 30)
	second := seedSalePlan(t, ctx, h.db, "sale-reuse-second", "50", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-REUSE"
	h.createPlanOrder(t, ctx, userID, orderNo, first.Key)

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: orderNo,
		UserID:  userID,
		PlanKey: second.Key,
	})
	if err == nil {
		t.Fatal("同一个单号换一个包应当报冲突")
	}
	if !errors.Is(err, ErrOrderConflict) {
		t.Errorf("错误没有包成 ErrOrderConflict：%v", err)
	}
	for _, want := range []string{first.Key, second.Key} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q（要点名两个包）：%v", want, err)
		}
	}

	// 冲突之后库里仍是下单时那一行，没有任何分录与订阅。
	var storedPlan string
	if qErr := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(plan_key, '') FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&storedPlan); qErr != nil {
		t.Fatalf("读支付单失败: %v", qErr)
	}
	if storedPlan != first.Key {
		t.Errorf("冲突之后单子上的包 = %q，期望仍是最初那个 %q", storedPlan, first.Key)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)

	// 另一维：同一个单号、**同一个包**，但包上的天数在下单之后被改过。
	//
	// 这一格与上面那条是同一句判据的另一半（CreateTx 的内容比较里
	// `sameValidityDays` 那一项）。不钉的话，删掉它整包仍然绿：包 key 相同、
	// 金额相同，第二次下单会静默返回第一次那一行（applied=false），调用方
	// 以为下了一张按 60 天算的单，实际拿到的是 30 天那张——而这个值现在决定
	// 用户拿到多少天。
	const daysOrderNo = "ORD-PLAN-REUSE-DAYS"
	daysPlan := seedSalePlan(t, ctx, h.db, "sale-reuse-days", "50", 30)
	h.createPlanOrder(t, ctx, userID, daysOrderNo, daysPlan.Key)
	if _, updErr := h.db.ExecContext(ctx,
		`UPDATE plans SET validity_days = 60 WHERE key = $1`, daysPlan.Key); updErr != nil {
		t.Fatalf("改售卖有效期失败: %v", updErr)
	}
	_, err = h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: daysOrderNo,
		UserID:  userID,
		PlanKey: daysPlan.Key,
	})
	if err == nil {
		t.Fatal("同一个单号、包上的天数改过之后重下单，应当报冲突而不是静默返回旧单")
	}
	if !errors.Is(err, ErrOrderConflict) {
		t.Errorf("错误没有包成 ErrOrderConflict：%v", err)
	}
	for _, want := range []string{"30 天", "60 天"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q（要点名两边的天数）：%v", want, err)
		}
	}
	var storedDays *int
	if err := h.db.QueryRowContext(ctx,
		`SELECT validity_days FROM payment_orders WHERE order_no = $1`, daysOrderNo).Scan(&storedDays); err != nil {
		t.Fatalf("读单子上的天数失败: %v", err)
	}
	if storedDays == nil || *storedDays != 30 {
		t.Errorf("冲突之后单子上记的天数 = %v，期望仍是最初的 30", storedDays)
	}
	assertNothingGranted(t, ctx, h.db, daysOrderNo)
}

// 同一张售卖单、同一个包、同样由包给出的天数，再下一次：**不算冲突**——
// applied=false、库里仍一行、没有新分录。
//
// 它杀的是 sameValidityDays 里「两边都有值且相等」这一格的两个变异体：
// store.go:446:7（`a == nil` → `!=`）与 446:19（`b == nil` → `!=`）。这两个在
// 两边都有值时都会提前返回 false，于是「相等」被判成「不等」，同一件商品重下
// 变成冲突——通道与 CLI 的重试从此全报错。
//
// 充值单两边都是 nil，走的是同一个函数里另一个早返回，覆盖不到这一格：
// service_test.go 的 TestCreateReplaysOrConflictsOnTheSameOrderNo 只在「两边
// 都没有」这一格上有鉴别力。
func TestPlanPurchaseCreateReplayOnSameParamsIsNotConflict(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-create-replay", "66", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-CREATE-REPLAY"
	first := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	again, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: orderNo,
		UserID:  userID,
		PlanKey: plan.Key,
	})
	if err != nil {
		t.Fatalf("同内容重跑下单失败（天数相等被判成了不等）: %v", err)
	}
	if again.Applied {
		t.Error("同内容重跑不该新写一行")
	}
	if again.Order.OrderNo != first.OrderNo {
		t.Errorf("回执单号 = %q，期望仍是 %q", again.Order.OrderNo, first.OrderNo)
	}
	// 回执必须是库里那一行：created_at 由数据库补，入参里是零值。
	if again.Order.CreatedAt.IsZero() {
		t.Error("读回的那一行带着零值 created_at，说明回的不是库里那一行")
	}

	var orders int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&orders); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if orders != 1 {
		t.Errorf("支付单 %d 行，期望 1 行", orders)
	}
	var days *int
	if err := h.db.QueryRowContext(ctx,
		`SELECT validity_days FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&days); err != nil {
		t.Fatalf("读单子上的天数失败: %v", err)
	}
	if days == nil || *days != 30 {
		t.Errorf("单子上记的天数 = %v，期望仍是 30", days)
	}
	if got := planSaleStateOf(t, ctx, h.db, orderNo, PlanTxnIDForOrder(orderNo)); got.orders != 0 {
		t.Errorf("重下之后多了 %d 条分录，期望 0 条", got.orders)
	}
}

// 库里那张售卖单没有记天数（validity_days 为 NULL），这次要写的单有 30 天：
// 算冲突，报错是「内容与这次要写的不一样」。
//
// 这一格不是硬造：payment_orders 的 plan_key 与 validity_days 是 00070 同一条
// 迁移加的两列、都可空，中间没有约束把它们绑在一起，而 purchasePlan 里那条 nil
// 天数的守卫把「有 plan_key、没有天数」当成要处理的形态。来路要写准：00070 之前
// 的行连 plan_key 都没有，所以这不会是迁移留下的；能造出它的是手工修数、从列不全
// 的备份恢复、或者以后的写入漏了天数。所以下面那条 UPDATE 造的是真正可能出现的
// 行；谁把它当成脏数据「清理」掉，这一格的判据也会一起没。
//
// 它杀的是「一边 nil、一边有值」这一格：store.go:447:19（`a == nil && b == nil`
// 的 `&&` → `||`）会把这种混比判成相等，于是不报冲突，红在下面那条
// errors.Is 上。同一句 446:14（`a == nil || b == nil` 的 `||` → `&&`）在这里
// 会掉到 `return *a == *b` 解引用 nil——那是 panic，不证明判据有鉴别力，只是
// 「这一格今天完全没被走过」的旁证，不进红证。
func TestPlanPurchaseCreateConflictsWhenStoredDaysAreNull(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-create-null-days", "55", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-CREATE-NULL-DAYS"
	h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	// 把这一行改成「有 plan_key、没有天数」——正常写路径造不出来的那种行：
	// 包还在，天数被清空了。
	if _, err := h.db.ExecContext(ctx,
		`UPDATE payment_orders SET validity_days = NULL WHERE order_no = $1`, orderNo); err != nil {
		t.Fatalf("清空单子上的天数失败: %v", err)
	}

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: orderNo,
		UserID:  userID,
		PlanKey: plan.Key,
	})
	if !errors.Is(err, ErrOrderConflict) {
		t.Fatalf("库里没有天数、这次有 30 天，重下应当报冲突（错误 = %v）", err)
	}
	if !strings.Contains(err.Error(), "内容与这次要写的不一样") {
		t.Errorf("报错不是「内容与这次要写的不一样」那一句：%v", err)
	}

	// 拒了之后什么都没变：仍一行、天数仍是 NULL、没有分录。
	var orders int
	var days *int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*), min(validity_days) FROM payment_orders WHERE order_no = $1`,
		orderNo).Scan(&orders, &days); err != nil {
		t.Fatalf("读支付单失败: %v", err)
	}
	if orders != 1 {
		t.Errorf("支付单 %d 行，期望 1 行", orders)
	}
	if days != nil {
		t.Errorf("单子上的天数 = %d，期望仍是 NULL", *days)
	}
	if got := planSaleStateOf(t, ctx, h.db, orderNo, PlanTxnIDForOrder(orderNo)); got.orders != 0 {
		t.Errorf("冲突之后多了 %d 条分录，期望 0 条", got.orders)
	}
}

// 下单时 PlanKey 首尾带空白：拒绝，报「首尾有空白」——不拿 trim 后的 key 接着
// 查包，更不落单。
//
// 它杀的是 service.go:165:15：`if key == "" || key != req.PlanKey` 的 `||` 翻成
// `&&` 之后，带空白的 key 不再被这一条挡住。用例里的包是存在的，于是变异体上
// 不仅没报错，还会把 req.PlanKey 改写成 trim 后的值、把单子直接落库：调用方
// 以为下了一张「key 带空格」的单，拿到的却是另一个 key 的单。
//
// 断言里带上「不是 ErrNoPlan」：若守卫被短路，报错来自查包那一层，红的地方就
// 不对了。
func TestPlanPurchaseRejectsPlanKeyWithSurroundingSpace(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-key-space", "44", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-KEY-SPACE"

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: orderNo,
		UserID:  userID,
		PlanKey: " " + plan.Key + " ",
	})
	if err == nil {
		t.Fatal("PlanKey 首尾带空白应当被拒，而不是静默 trim 之后按包下单")
	}
	if !strings.Contains(err.Error(), "首尾有空白") {
		t.Errorf("报错不是「首尾有空白」那一句：%v", err)
	}
	if errors.Is(err, plans.ErrNoPlan) {
		t.Errorf("报错来自查包那一层（ErrNoPlan），说明空格守卫被短路了：%v", err)
	}

	// 被拒的单子一行都不该落，也不该有分录。
	var orders int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, orderNo).Scan(&orders); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if orders != 0 {
		t.Errorf("拒了之后 payment_orders 有 %d 行，期望 0 行", orders)
	}
}

// 下单之后、首次发放之前改 validity_days：两个方向各一条子用例。
//
// 为什么要分两个方向：改长是平台亏（用户按 30 天付钱拿到 365 天），改短是
// 用户亏（按年付钱拿到一周），而且改短那条更难举证。合成一条只测变长，就
// 等于默认「天数只会被改大」——那不是事实，也不该是假设。
//
// 这一格比另外四格更隐蔽：它不报错、没有日志，静默生效。挡它的依据是单子上
// 记下的天数（payment_orders.validity_days），所以这一条同时也是「下单时把
// 天数记进单子」这件事的判据。
func TestPlanPurchaseRejectsValidityDaysChangedAfterOrder(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		orderNo   string
		daysAfter int
		wantFrom  string
		wantTo    string
	}{
		{name: "改长", key: "sale-days-longer", orderNo: "ORD-PLAN-DAYS-LONGER", daysAfter: 365, wantFrom: "30", wantTo: "365"},
		{name: "改短", key: "sale-days-shorter", orderNo: "ORD-PLAN-DAYS-SHORTER", daysAfter: 7, wantFrom: "30", wantTo: "7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			plan := seedSalePlanRow(t, ctx, h.db, tc.key, "55", 30)
			userID := seedUserWithoutWallet(t, ctx, h.db)
			order := h.createPlanOrder(t, ctx, userID, tc.orderNo, plan.Key)

			// 单子上要记着下单时的 30 天——后面改包之后，判据就靠它。
			var recorded *int
			if err := h.db.QueryRowContext(ctx,
				`SELECT validity_days FROM payment_orders WHERE order_no = $1`, tc.orderNo).Scan(&recorded); err != nil {
				t.Fatalf("读单子上的天数失败: %v", err)
			}
			if recorded == nil || *recorded != 30 {
				t.Fatalf("单子上记的天数 = %v，期望 30", recorded)
			}

			if _, err := h.db.ExecContext(ctx,
				`UPDATE plans SET validity_days = $2 WHERE key = $1`, plan.Key, tc.daysAfter); err != nil {
				t.Fatalf("改售卖有效期失败: %v", err)
			}

			_, err := h.notifyPaid(ctx, tc.orderNo, order.Amount, order.Asset, money.MustParse("0.55"),
				time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
			if err == nil {
				t.Fatal("售卖有效期在下单之后被改过，通知应当被拒")
			}
			for _, want := range []string{"售卖有效期在售出之后改过", tc.wantFrom, tc.wantTo, tc.orderNo} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("报错里没有 %q（要点名哪一格、并带上两个天数）：%v", want, err)
				}
			}
			// 与其余四格一样，这一类必须是 500 而不是 409：409 会让通道停止重试。
			if errors.Is(err, ErrOrderConflict) {
				t.Errorf("「包被改过」被映射成 409，通道会停止重试：%v", err)
			}
			assertNothingGranted(t, ctx, h.db, tc.orderNo)
		})
	}
}

// 下单之后包被改成「不出售」（validity_days 清空）：通知被拒，并且报错说的是
// 「在售出之后被改成不出售」，不是「没有设置售卖有效期」。
//
// 这两句话给人的下一步完全不同：前者是「这张单要人工核对」，后者听起来像
// 「这个包从来没设过天数」。清空这一格如果没有自己的分支，落到 GrantTx 那句
// 上，运维会照着一个错的前提去查。
func TestPlanPurchaseRejectsPlanNoLongerOnSaleAfterOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-days-cleared", "45", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-DAYS-CLEARED"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET validity_days = NULL WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("清空售卖有效期失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.45"),
		time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("包被改成不出售之后，这张单的通知应当被拒")
	}
	if !strings.Contains(err.Error(), "在售出之后被改成不出售") {
		t.Errorf("报错不是「被改成不出售」那一句（别落到「没有设置售卖有效期」上）：%v", err)
	}
	if strings.Contains(err.Error(), "没有设置售卖有效期") {
		t.Errorf("报错落到了 GrantTx 那句上，运维会以为包从来没设过天数：%v", err)
	}
	if !strings.Contains(err.Error(), "30") {
		t.Errorf("报错要带上单子上记的天数：%v", err)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)
}

// 首次发放之前包被改：通知被拒，报错点名是哪一格。
//
// 守卫防的是「下单与**首次**发放之间包被改了」——这笔交易还没成立，按新值发
// 是让用户按旧价拿到新包，按旧值发是让一个已经下架的包按旧价长期生效。
func TestPlanPurchaseFirstGrantAfterPlanChangedIsRejected(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-first-after-change", "77", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-FIRST-AFTER-CHANGE"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET face_value = 88, quota_total = 88 WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("改面值失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.77"),
		time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("首次发放前包被改过，通知应当被拒")
	}
	if !strings.Contains(err.Error(), "面值在售出之后改过") {
		t.Errorf("报错不是「面值改过」那一句：%v", err)
	}
	assertNothingGranted(t, ctx, h.db, orderNo)
}

// 售卖单上没记天数（带 plan_key 却没有 validity_days，正常写路径造不出来）：
// 拒绝入账，并且报错要说得清来路。
//
// 这一格**不是**在钉「不能静默回落」——那件事由 plans.GrantTx 兜着（拿到 nil
// 就拒绝，不解引用、也不读现在的包），删掉 payment 这一层的判断照样不会回落，
// 只是会报成 GrantTx 那句「额度包的售卖没有给出天数」：那句话点名的是**包**，
// 运维会去查包，而真正要查的是这一行为什么没有天数。所以这里钉的是**说法**——
// 单号 + 「这种行不该存在」+「先核来路」。
//
// 两层都留着是有意的：一层保不变量，一层保可读性。
func TestPlanPurchaseRejectsOrderWithoutValidityDays(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-order-no-days", "33", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-NO-DAYS"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)

	// 模拟被测的形态：带 plan_key 的行没有天数（正常写路径造不出来）。
	if _, err := h.db.ExecContext(ctx,
		`UPDATE payment_orders SET validity_days = NULL WHERE order_no = $1`, orderNo); err != nil {
		t.Fatalf("清空单子上的天数失败: %v", err)
	}

	_, err := h.notifyPaid(ctx, orderNo, order.Amount, order.Asset, money.MustParse("0.33"),
		time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("单子上没有天数时应当拒绝入账，而不是回落到包上的值")
	}
	for _, want := range []string{"没有记下售卖有效期", "不会长成这样"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q：%v", want, err)
		}
	}
	// 不能落到「按现在的包发」：库里一条订阅、一条分录都不该有。
	assertNothingGranted(t, ctx, h.db, orderNo)
}

// 已经发放过之后改 validity_days，再重放：**成功**（applied=false，HTTP 200），
// 而且库里那一行的 period_end 仍是「收款时刻 + 下单时的 30 天」。
//
// 这一条钉的是「到期时刻按单子上记的天数算」：
//   - 按现在的包算（60 天）→ grantReplayConflict 比到期时刻不一样 → ErrTxnConflict
//     → 409。409 会让通道停止重试，正是要避免的后果；
//   - 按单子上记的 30 天算 → 与库里那一行相等 → 正常重放，200。
//
// 只断 200 不够：一个「不写第二组分录、但把 period_end 改成 60 天」的实现也能
// 返回 200。所以同时断 period_end 逐微秒等于首次那个值，且只有一行订阅。
//
// 走真的 Handler：500/409 的区分发生在 writeServiceError，只测 Service 看不到
// 「通道会收到哪个码」。
//
// 重放能算回库里那一行，前提是**单子上的天数在两次通知之间没有变**：重放走的是
// 同一条路，到期时刻仍按单子算，所以只要单子被改过（比如运营在两次通知之间改了
// payment_orders.validity_days），这条就不再成立，会变成 grantReplayConflict
// → 409。单子变成可改之日，这条要重验。
func TestPlanPurchaseReplayAfterValidityDaysChangedReturnsOK(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-replay-days", "68", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-REPLAY-DAYS"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	paidAt := time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)
	notification := Notification{
		OrderNo:      orderNo,
		GatewayTxnID: "TRADE-" + orderNo,
		Status:       StatusPaid,
		Amount:       order.Amount,
		Asset:        order.Asset,
		Fee:          money.MustParse("0.68"),
		PaidAt:       paidAt,
	}

	first, err := h.service.Notify(ctx, stubName, notification)
	if err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}
	if !first.Applied {
		t.Fatal("首次通知应当发放")
	}
	subID, _, periodEnd := userPlanByRef(t, ctx, h.db, orderNo)
	if want := paidAt.AddDate(0, 0, 30); periodEnd.UnixMicro() != want.UnixMicro() {
		t.Fatalf("首次发放的 period_end = %s，期望收款时刻 + 30 天 = %s", periodEnd, want)
	}

	// 包改成 60 天。
	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET validity_days = 60 WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("改售卖有效期失败: %v", err)
	}

	channel := &scriptedChannel{notification: notification}
	service := NewService(h.store, h.service.ledger, plans.New(h.db), map[string]Channel{stubName: channel})
	handler := NewHandler(service, stubName, "")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, CallbackPath(stubName),
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("改天数之后的重放应当 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"applied":false`) {
		t.Errorf("响应体应当说明这次没有新写：%s", rec.Body.String())
	}

	// 库里那一行不能变：订阅仍一条、period_end 仍是 30 天那个、分录仍四条。
	var subs int
	if err := h.db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_plans WHERE ref = $1`, orderNo).Scan(&subs); err != nil {
		t.Fatalf("数订阅失败: %v", err)
	}
	if subs != 1 {
		t.Errorf("订阅 %d 条，期望 1 条", subs)
	}
	var end sql.NullTime
	if err := h.db.QueryRowContext(ctx,
		`SELECT period_end FROM user_plans WHERE id = $1`, subID).Scan(&end); err != nil {
		t.Fatalf("读 period_end 失败: %v", err)
	}
	if !end.Valid || end.Time.UnixMicro() != periodEnd.UnixMicro() {
		t.Errorf("重放之后 period_end = %v，期望仍是首次那个 %s（不能按包现在的 60 天重算）",
			end, periodEnd)
	}
	if want := paidAt.AddDate(0, 0, 30); end.Valid && end.Time.UnixMicro() != want.UnixMicro() {
		t.Errorf("period_end = %s，期望收款时刻 + 30 天 = %s", end.Time, want)
	}
	assertPurchaseLedger(t, ctx, h.db, orderNo, order.Amount, money.MustParse("0.68"))
}

// scriptedChannel 让 ParseNotification 交回一份预先写好的通知。
//
// 回调那一层要验的是「HTTP 状态码」，所以走真的 Handler + 真的请求，
// 只有通道的解析换成固定答案。
type scriptedChannel struct {
	stubChannel
	notification Notification
}

func (c *scriptedChannel) ParseNotification(context.Context, NotificationRequest) (Notification, error) {
	return c.notification, nil
}

// 已经发放过之后包被改，同一份通知重放：**成功**（applied=false，HTTP 200）。
//
// 与上面那条成对：守卫只对首次发放生效。订阅已经存在，说明这笔交易已成立并
// 落库，包后来改成什么样都改变不了它；此时拒绝只换来通道无限重试和一堆 500。
//
// 这一条必须走 Handler：500 是 writeServiceError 的 default 分支产生的，
// 只测 Service 看不到「通道会收到 500」这个后果。
func TestPlanPurchaseReplayAfterPlanChangedReturnsOK(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	plan := seedSalePlanRow(t, ctx, h.db, "sale-replay-after-change", "77", 30)
	userID := seedUserWithoutWallet(t, ctx, h.db)
	const orderNo = "ORD-PLAN-REPLAY-AFTER-CHANGE"
	order := h.createPlanOrder(t, ctx, userID, orderNo, plan.Key)
	paidAt := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	notification := Notification{
		OrderNo:      orderNo,
		GatewayTxnID: "TRADE-" + orderNo,
		Status:       StatusPaid,
		Amount:       order.Amount,
		Asset:        order.Asset,
		Fee:          money.MustParse("0.77"),
		PaidAt:       paidAt,
	}

	first, err := h.service.Notify(ctx, stubName, notification)
	if err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}
	if !first.Applied {
		t.Fatal("首次通知应当发放")
	}

	// 改包：面值与可用量一起改，否则撞 plans_amount_quota_matches_face_value。
	if _, err := h.db.ExecContext(ctx,
		`UPDATE plans SET face_value = 88, quota_total = 88 WHERE key = $1`, plan.Key); err != nil {
		t.Fatalf("改面值失败: %v", err)
	}

	// 重放走真的回调入口：通道交回的仍是同一份「已支付」通知。
	channel := &scriptedChannel{notification: notification}
	service := NewService(h.store, h.service.ledger, plans.New(h.db), map[string]Channel{stubName: channel})
	handler := NewHandler(service, stubName, "")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, CallbackPath(stubName),
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("已发放之后的重放应当 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"applied":false`) {
		t.Errorf("响应体应当说明这次没有新写：%s", rec.Body.String())
	}
	if got := planSaleStateOf(t, ctx, h.db, orderNo, PlanTxnIDForOrder(orderNo)); got.subs != 1 || got.orders != 4 {
		t.Errorf("重放之后 = 订阅 %d 条 / 分录 %d 条，期望 1 / 4", got.subs, got.orders)
	}

	// 200 与 applied=false 只说明「没报错」，不说明「没重复记账」。包被改过
	// 之后的重放这条路上，库里必须**还是原来那一笔**：四条、平、每条都是
	// plan_purchase、prepaid 余额仍是面值一次。
	assertPurchaseLedger(t, ctx, h.db, orderNo, order.Amount, money.MustParse("0.77"))

	subID, _, _ := userPlanByRef(t, ctx, h.db, orderNo)
	var prepaidBalance string
	if err := h.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		  FROM ledger_entries WHERE account = $1 AND asset = 'CNY'`,
		ledger.PrepaidAccount(subID).String()).Scan(&prepaidBalance); err != nil {
		t.Fatalf("读 prepaid 余额失败: %v", err)
	}
	if prepaidBalance != order.Amount.String() {
		t.Errorf("重放之后 prepaid:%d 的余额 = %s，期望仍是面值 %s 一次",
			subID, prepaidBalance, order.Amount)
	}
}
