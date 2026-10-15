package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// newTxnID 给测试一个固定的、可读的交易标识。
// 用固定值而不是 NewTxnID()：测试失败时的报错文本要能直接比对。
func newTxnID(t *testing.T, s string) TxnID {
	t.Helper()
	id, err := ParseTxnID(s)
	if err != nil {
		t.Fatalf("测试用的交易标识不合法: %v", err)
	}
	return id
}

const (
	txnA = "11111111-1111-4111-8111-111111111111"
	txnB = "22222222-2222-4222-8222-222222222222"
)

func amount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatalf("测试金额 %q 不合法: %v", s, err)
	}
	return a
}

// 每个构造函数产出的交易都必须自洽——这是「构造的一定平」这条性质。
func TestConstructorsProduceBalancedTransactions(t *testing.T) {
	txn := newTxnID(t, txnA)

	topup, err := Topup(TopupParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "100"), Asset: AssetCNY,
		Gateway: "stripe", GatewayFee: amount(t, "2"), OrderNo: "order-1",
	})
	if err != nil {
		t.Fatalf("Topup: %v", err)
	}
	if err := topup.Verify(); err != nil {
		t.Errorf("Topup 不自洽: %v", err)
	}

	consume, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "6"), UpstreamCost: amount(t, "3"),
		BillingRequestID: 55,
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := consume.Verify(); err != nil {
		t.Errorf("Consume 不自洽: %v", err)
	}

	refund, err := Refund(RefundParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "6"), UpstreamCost: amount(t, "3"),
		BillingRequestID: 55, Memo: "上游 500",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if err := refund.Verify(); err != nil {
		t.Errorf("Refund 不自洽: %v", err)
	}

	settle, err := Settle(SettleParams{
		TxnID: txn, UserID: 7, Amount: amount(t, "6"), Asset: AssetCNY,
		PayoutID: 88, Memo: "2026-10 结算",
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if err := settle.Verify(); err != nil {
		t.Errorf("Settle 不自洽: %v", err)
	}

	req, err := PayoutRequest(PayoutRequestParams{
		TxnID: txn, PayoutID: 88, UserID: 7, Amount: amount(t, "6"), Asset: AssetCNY,
	})
	if err != nil {
		t.Fatalf("PayoutRequest: %v", err)
	}
	if err := req.Verify(); err != nil {
		t.Errorf("PayoutRequest 不自洽: %v", err)
	}

	paid, err := PayoutPaid(PayoutPaidParams{
		TxnID: txn, PayoutID: 88, UserID: 7, Amount: amount(t, "6"),
		Asset: AssetCNY, Gateway: "stripe", Ref: "ref-1",
	})
	if err != nil {
		t.Fatalf("PayoutPaid: %v", err)
	}
	if err := paid.Verify(); err != nil {
		t.Errorf("PayoutPaid 不自洽: %v", err)
	}

	adj, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(42), Amount: amount(t, "-3"),
		Asset: AssetCNY, Memo: "对账差额补扣",
	})
	if err != nil {
		t.Fatalf("Adjustment: %v", err)
	}
	if err := adj.Verify(); err != nil {
		t.Errorf("Adjustment 不自洽: %v", err)
	}
}

// 消费的四项必须能逐项对上：售价 = 分成 + 上游成本 + 平台收入。
func TestConsumeSplitsRevenue(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "6"), UpstreamCost: amount(t, "3"),
	})
	if err != nil {
		t.Fatal(err)
	}

	byAccount := map[string]money.Amount{}
	for _, e := range tx.Entries {
		byAccount[e.Account.String()] = e.Amount
	}
	if got := byAccount["user:42"]; got.Cmp(amount(t, "-10")) != 0 {
		t.Errorf("用户 = %s，期望 -10", got)
	}
	if got := byAccount["merchant_payable:7"]; got.Cmp(amount(t, "6")) != 0 {
		t.Errorf("应付商户 = %s，期望 6", got)
	}
	if got := byAccount["upstream_cost"]; got.Cmp(amount(t, "3")) != 0 {
		t.Errorf("上游成本 = %s，期望 3", got)
	}
	if got := byAccount["platform_revenue"]; got.Cmp(amount(t, "1")) != 0 {
		t.Errorf("平台收入 = %s，期望 1（推出来的，不是给的）", got)
	}
}

// 没有分账、没有上游成本的消费仍要平：平台收入等于售价。
func TestConsumePlatformOwnedChannel(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Verify(); err != nil {
		t.Fatal(err)
	}
	if len(tx.Entries) != 2 {
		t.Fatalf("应当只有用户与平台收入两条分录，得到 %d 条", len(tx.Entries))
	}
}

// 分成与成本之和超过售价时不写负收入：那是定价问题，不是记账问题。
func TestConsumeRejectsLoss(t *testing.T) {
	txn := newTxnID(t, txnA)
	_, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "8"), UpstreamCost: amount(t, "3"),
	})
	if err == nil {
		t.Fatal("亏损的单子应当被拒，而不是写一笔负收入")
	}
	if !strings.Contains(err.Error(), "亏钱") {
		t.Errorf("报错要说清是亏钱，得到: %v", err)
	}
}

// 给了分成金额却没有商户 id：分成的归属无法确定，不能猜一个 id。
func TestConsumeRejectsShareWithoutMerchant(t *testing.T) {
	txn := newTxnID(t, txnA)
	_, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantShare: amount(t, "6"),
	})
	if err == nil {
		t.Fatal("缺少商户 id 应当被拒")
	}
}

// 金额必须由调用方给出正数：方向由构造函数决定。接受负数会让
// 「方向写反」在上游被悄悄吞掉。
func TestConstructorsRejectNegativeAmounts(t *testing.T) {
	txn := newTxnID(t, txnA)
	neg := amount(t, "-1")

	if _, err := Topup(TopupParams{TxnID: txn, UserID: 1, Amount: neg, Asset: AssetCNY, Gateway: "g"}); err == nil {
		t.Error("负的充值金额应当被拒")
	}
	if _, err := Consume(ConsumeParams{TxnID: txn, UserID: 1, Amount: neg, Asset: AssetCNY}); err == nil {
		t.Error("负的消费金额应当被拒")
	}
	if _, err := Refund(RefundParams{TxnID: txn, UserID: 1, Amount: neg, Asset: AssetCNY}); err == nil {
		t.Error("负的退款金额应当被拒")
	}
	if _, err := Settle(SettleParams{TxnID: txn, UserID: 1, Amount: neg, Asset: AssetCNY, PayoutID: 1}); err == nil {
		t.Error("负的结算金额应当被拒")
	}
	if _, err := PayoutRequest(PayoutRequestParams{TxnID: txn, PayoutID: 1, UserID: 1, Amount: neg, Asset: AssetCNY}); err == nil {
		t.Error("负的提现金额应当被拒")
	}
	if _, err := PayoutPaid(PayoutPaidParams{TxnID: txn, PayoutID: 1, UserID: 1, Amount: neg, Asset: AssetCNY, Gateway: "g"}); err == nil {
		t.Error("负的打款金额应当被拒")
	}
}

func TestConstructorsRequireTxnID(t *testing.T) {
	if _, err := Topup(TopupParams{UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY, Gateway: "g"}); err == nil {
		t.Error("缺少 txn_id 应当被拒")
	}
	if _, err := Adjustment(AdjustmentParams{Account: UserAccount(1), Amount: amount(t, "1"), Asset: AssetCNY, Memo: "x"}); err == nil {
		t.Error("缺少 txn_id 应当被拒")
	}
}

// 通道手续费是平台的成本，不是从用户余额里扣的钱：用户充 100 应当到账 100。
func TestTopupKeepsUserWholeAndBooksFeeAsCost(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Topup(TopupParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "100"), Asset: AssetCNY,
		Gateway: "stripe", GatewayFee: amount(t, "2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Verify(); err != nil {
		t.Fatal(err)
	}

	var user, gateway, cost, revenue money.Amount
	for _, e := range tx.Entries {
		switch e.Account.String() {
		case "user:42":
			user = user.Add(e.Amount)
		case "payment_gateway:stripe":
			gateway = gateway.Add(e.Amount)
		case "upstream_cost":
			cost = cost.Add(e.Amount)
		case "platform_revenue":
			revenue = revenue.Add(e.Amount)
		}
	}
	if user.Cmp(amount(t, "100")) != 0 {
		t.Errorf("用户到账 = %s，期望 100（手续费不该从用户身上扣）", user)
	}
	// 通道收的是全额：通道科目的余额等于「通道替我们收走的钱」，
	// 负数表示它是我们的资产（见 account.go 的符号约定）。
	if gateway.Cmp(amount(t, "-100")) != 0 {
		t.Errorf("通道科目 = %s，期望 -100（按全额入账，可与通道账单逐笔核对）", gateway)
	}
	if cost.Cmp(amount(t, "2")) != 0 {
		t.Errorf("通道费 = %s，期望 2", cost)
	}
	if revenue.Cmp(amount(t, "-2")) != 0 {
		t.Errorf("平台承担的手续费 = %s，期望 -2", revenue)
	}
}

func TestTopupRejectsFeeAboveAmount(t *testing.T) {
	txn := newTxnID(t, txnA)
	_, err := Topup(TopupParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY,
		Gateway: "stripe", GatewayFee: amount(t, "2"),
	})
	if err == nil {
		t.Fatal("手续费大于充值额应当被拒：差额没有来源")
	}
}

func TestTopupRequiresGateway(t *testing.T) {
	txn := newTxnID(t, txnA)
	if _, err := Topup(TopupParams{TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY}); err == nil {
		t.Error("缺少通道标识应当被拒")
	}
}

// 打款分录必须有对家：没有对家的一组分录和不为零。
func TestPayoutPaidRequiresGateway(t *testing.T) {
	txn := newTxnID(t, txnA)
	if _, err := PayoutPaid(PayoutPaidParams{TxnID: txn, PayoutID: 1, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY}); err == nil {
		t.Error("缺少打款通道应当被拒")
	}
}

// 结算与提现申请都要带上提现单 id：payout 科目没有 id 时，
// 这一笔负债无法区分是哪一条提现单。
func TestSettleAndPayoutRequestRequirePayoutID(t *testing.T) {
	txn := newTxnID(t, txnA)
	if _, err := Settle(SettleParams{TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY}); err == nil {
		t.Error("缺少提现单 id 的结算应当被拒")
	}
	if _, err := PayoutRequest(PayoutRequestParams{TxnID: txn, PayoutID: 0, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY}); err == nil {
		t.Error("缺少提现单 id 的提现申请应当被拒")
	}
}

// 结算与提现申请的账面形状相同，但类型不同——「谁发起的」要在账面上可见。
func TestSettleAndPayoutRequestDifferInType(t *testing.T) {
	txn := newTxnID(t, txnA)
	settle, err := Settle(SettleParams{TxnID: txn, UserID: 7, Amount: amount(t, "5"), Asset: AssetCNY, PayoutID: 3})
	if err != nil {
		t.Fatal(err)
	}
	req, err := PayoutRequest(PayoutRequestParams{TxnID: txn, PayoutID: 3, UserID: 7, Amount: amount(t, "5"), Asset: AssetCNY})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range settle.Entries {
		if e.EntryType != EntrySettle {
			t.Errorf("结算的分录类型 = %q", e.EntryType)
		}
	}
	for _, e := range req.Entries {
		if e.EntryType != EntryPayoutRequest {
			t.Errorf("提现申请的分录类型 = %q", e.EntryType)
		}
		if e.PayoutID == nil || *e.PayoutID != 3 {
			t.Error("提现申请的分录必须挂上 payout_id")
		}
	}
}

// 调账没有备注就无法在事后解释，只能当成来路不明的一笔。
func TestAdjustmentRequiresMemo(t *testing.T) {
	txn := newTxnID(t, txnA)
	_, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(1), Amount: amount(t, "1"), Asset: AssetCNY,
	})
	if err == nil {
		t.Fatal("无备注的调账应当被拒")
	}
}

// 调账双向都支持，且对家固定是平台收入。
func TestAdjustmentBothDirections(t *testing.T) {
	txn := newTxnID(t, txnA)

	add, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(42), Amount: amount(t, "5"), Asset: AssetCNY, Memo: "补发",
	})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(42), Amount: amount(t, "-5"), Asset: AssetCNY, Memo: "补扣",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := BalanceOf(add.Entries, UserAccount(42), AssetCNY); got.Cmp(amount(t, "5")) != 0 {
		t.Errorf("加钱的调账后余额 = %s，期望 5", got)
	}
	if got := BalanceOf(sub.Entries, UserAccount(42), AssetCNY); got.Cmp(amount(t, "-5")) != 0 {
		t.Errorf("扣钱的调账后余额 = %s，期望 -5", got)
	}
	for _, e := range append(append([]Entry{}, add.Entries...), sub.Entries...) {
		if e.Account.Type() != AccountUser && e.Account != PlatformRevenueAccount() {
			t.Errorf("调账的对家只能是平台收入，得到 %s", e.Account)
		}
	}
	// 零金额调账没有意义，且会撞上「金额为零」的分录校验。
	if _, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(1), Amount: money.Zero, Asset: AssetCNY, Memo: "无",
	}); err == nil {
		t.Error("零金额的调账应当被拒")
	}
}

// 备注会进日志，外部来源的字符串不能伪造出额外的日志行。
func TestMemosAreSanitized(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Refund(RefundParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY,
		Memo: "上游失败\nfake log line\tend",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tx.Entries {
		if strings.ContainsAny(e.Memo, "\r\n\t") {
			t.Errorf("备注没被清洗: %q", e.Memo)
		}
	}
}

// 充值单据号同样来自外部系统（支付网关），也要清洗。
func TestTopupOrderNoSanitized(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Topup(TopupParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY,
		Gateway: "stripe", OrderNo: "a\nb",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tx.Entries {
		if strings.ContainsAny(e.Memo, "\r\n") {
			t.Errorf("备注没被清洗: %q", e.Memo)
		}
	}
}

// 关联字段不是附带信息：少了 billing_request_id，一笔扣费就关联不到
// 具体是哪次请求，争议发生时无从追溯。
func TestConsumeAttachesBillingRequest(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY, BillingRequestID: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tx.Entries {
		if e.BillingRequestID == nil || *e.BillingRequestID != 99 {
			t.Errorf("分录 %s 没有挂上 billing_request_id", e.Account)
		}
	}
}

// Denormalize 返回副本：调用方可以在上面挂写库需要的字段，
// 而不会改动原交易。
func TestDenormalizeReturnsCopy(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Consume(ConsumeParams{TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY})
	if err != nil {
		t.Fatal(err)
	}
	out := tx.Denormalize()
	if len(out) != len(tx.Entries) {
		t.Fatalf("Denormalize 的条数 = %d，期望 %d", len(out), len(tx.Entries))
	}
	out[0].Memo = "改副本"
	if tx.Entries[0].Memo == "改副本" {
		t.Error("改副本不该影响原交易")
	}
	for i, e := range out {
		if e.TxnID != tx.Entries[i].TxnID {
			t.Errorf("第 %d 条分录丢了 txn_id", i)
		}
	}
}

// 未知资产与未列出的资产都不能通过——空资产不退回默认值。
func TestConstructorsRejectUnknownAsset(t *testing.T) {
	txn := newTxnID(t, txnA)
	if _, err := Consume(ConsumeParams{TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: Asset("")}); err == nil {
		t.Error("空资产应当被拒")
	}
	if _, err := Consume(ConsumeParams{TxnID: txn, UserID: 1, Amount: amount(t, "1"), Asset: Asset("USDT")}); err == nil {
		t.Error("未支持的资产应当被拒")
	}
}

func TestMismatchedTxnIDsAreRejectedOnConstruction(t *testing.T) {
	// 用一个真实的构造路径覆盖不到混入别的 txn 的情形，
	// 这里直接构造：它是从数据库读回时才可能出现的情况。
	txn := newTxnID(t, txnA)
	other := newTxnID(t, txnB)
	tx := Transaction{
		TxnID: txn,
		Entries: []Entry{
			{TxnID: txn, Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "-1"), EntryType: EntryConsume},
			{TxnID: other, Account: PlatformRevenueAccount(), Asset: AssetCNY, Amount: amount(t, "1"), EntryType: EntryConsume},
		},
	}
	err := tx.Verify()
	if err == nil {
		t.Fatal("混入别的交易的分录应当被拒")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("报错要说清是 txn_id 不一致，得到: %v", err)
	}
}

// 未被支持的场景类型不能通过校验。这里用一个手工构造的交易覆盖：
// 从数据库读回的 entry_type 可能是一个已经下线的旧取值。
func TestVerifyRejectsUnknownEntryType(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: []Entry{
			{TxnID: txn, Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "1"), EntryType: EntryType("gift")},
		},
	}
	if err := tx.Verify(); err == nil {
		t.Fatal("未知的分录类型应当被拒")
	}
}

// Verify 的错误必须能 errors.Is 到 TxnID 的哨兵错误，供调用方分类处置。
func TestVerifyTxnIDErrorIsWrapped(t *testing.T) {
	tx := Transaction{
		TxnID: "not-a-uuid",
		Entries: []Entry{
			{TxnID: "not-a-uuid", Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "1"), EntryType: EntryAdjustment},
		},
	}
	err := tx.Verify()
	if !errors.Is(err, ErrInvalidTxnID) {
		t.Errorf("期望 ErrInvalidTxnID，得到 %v", err)
	}
}

// ── 更多参数守卫 ────────────────────────────────────────────────────────

func TestConsumeRejectsNegativeShareOrCost(t *testing.T) {
	txn := newTxnID(t, txnA)
	base := ConsumeParams{TxnID: txn, UserID: 1, Amount: amount(t, "10"), Asset: AssetCNY, MerchantID: 7}

	negShare := base
	negShare.MerchantShare = amount(t, "-1")
	if _, err := Consume(negShare); err == nil {
		t.Error("负的分成应当被拒")
	}

	negCost := base
	negCost.UpstreamCost = amount(t, "-1")
	if _, err := Consume(negCost); err == nil {
		t.Error("负的上游成本应当被拒")
	}
}

func TestRefundRejectsInconsistentParams(t *testing.T) {
	txn := newTxnID(t, txnA)

	// 分成没有归属。
	if _, err := Refund(RefundParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "10"), Asset: AssetCNY, MerchantShare: amount(t, "6"),
	}); err == nil {
		t.Error("给了分成却没有商户 id 应当被拒")
	}
	// 要冲回的三项之和超过退款额：那会把平台收入冲成负数。
	if _, err := Refund(RefundParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "8"), UpstreamCost: amount(t, "3"),
	}); err == nil {
		t.Error("冲回金额超过退款额应当被拒")
	}
	// 负的分成。
	if _, err := Refund(RefundParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "10"), Asset: AssetCNY, MerchantShare: amount(t, "-1"),
	}); err == nil {
		t.Error("负的分成应当被拒")
	}
}

func TestSettlePayoutPaidRejectBadIDs(t *testing.T) {
	txn := newTxnID(t, txnA)
	if _, err := Settle(SettleParams{TxnID: txn, UserID: 0, Amount: amount(t, "1"), Asset: AssetCNY, PayoutID: 1}); err == nil {
		t.Error("商户 id 非法应当被拒")
	}
	if _, err := PayoutPaid(PayoutPaidParams{TxnID: txn, PayoutID: 0, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY, Gateway: "g"}); err == nil {
		t.Error("提现单 id 非法应当被拒")
	}
	if _, err := PayoutPaid(PayoutPaidParams{TxnID: txn, PayoutID: 1, UserID: 0, Amount: amount(t, "1"), Asset: AssetCNY, Gateway: "g"}); err == nil {
		t.Error("商户 id 非法应当被拒")
	}
	if _, err := PayoutPaid(PayoutPaidParams{TxnID: txn, PayoutID: 1, UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY}); err == nil {
		t.Error("缺少打款通道应当被拒")
	}
}

func TestAdjustmentRejectsMalformedAccountAndAsset(t *testing.T) {
	txn := newTxnID(t, txnA)
	if _, err := Adjustment(AdjustmentParams{TxnID: txn, Amount: amount(t, "1"), Asset: AssetCNY, Memo: "x"}); err == nil {
		t.Error("缺少被调科目应当被拒")
	}
	if _, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(1), Amount: amount(t, "1"), Asset: Asset(""), Memo: "x",
	}); err == nil {
		t.Error("空资产应当被拒")
	}
}

// 用户 id 是分成的归属，不能缺；构造函数只在它非法时拒，
// 零值（未填）与负数一样被拦下。
func TestConstructorsRejectBadUserID(t *testing.T) {
	txn := newTxnID(t, txnA)
	if _, err := Topup(TopupParams{TxnID: txn, UserID: 0, Amount: amount(t, "1"), Asset: AssetCNY, Gateway: "g"}); err == nil {
		t.Error("用户 id 为 0 应当被拒")
	}
	if _, err := PayoutRequest(PayoutRequestParams{TxnID: txn, PayoutID: 1, UserID: 0, Amount: amount(t, "1"), Asset: AssetCNY}); err == nil {
		t.Error("商户 id 为 0 应当被拒")
	}
}

// 空的通道名不 panic，而是走成「单例科目」后被校验拦下——
// 构造函数保持「怎么给就怎么构造」，合法性统一交给校验。
func TestEmptyGatewayFailsValidation(t *testing.T) {
	if _, err := Topup(TopupParams{
		TxnID: newTxnID(t, txnA), UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY,
		Gateway: "",
	}); err == nil {
		t.Error("空通道名应当被拒")
	}
	// 直接构造出的科目形态：`payment_gateway` 缺后缀。
	if _, err := ParseAccount(PaymentGatewayAccount("").String()); err == nil {
		t.Error("缺后缀的通道科目不该能解析")
	}
}
