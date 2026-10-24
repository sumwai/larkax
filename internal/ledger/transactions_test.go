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
	txnC = "33333333-3333-4333-8333-333333333333"
)

func amount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatalf("测试金额 %q 不合法: %v", s, err)
	}
	return a
}

// balanceOf 算一个科目的余额，并断言它算得出来。
//
// 「算不出来」（求和超出 money 的表示范围）由 TestBalanceOfReportsOverflow
// 单独钉住；其余用例关心的是余额本身，所以这里把它当成用例失败，而不是让
// 每个调用点各写一遍错误分支。
func balanceOf(t *testing.T, entries []Entry, account Account, asset Asset) money.Amount {
	t.Helper()
	got, err := BalanceOf(entries, account, asset)
	if err != nil {
		t.Fatalf("算 %s 在 %s 上的余额失败: %v", account, asset, err)
	}
	return got
}

// balances 汇总一组分录，并断言它算得出来。理由与 balanceOf 相同。
func balances(t *testing.T, entries []Entry) map[Asset]map[Account]money.Amount {
	t.Helper()
	got, err := Balances(entries)
	if err != nil {
		t.Fatalf("汇总分录失败: %v", err)
	}
	return got
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
	if verifyErr := topup.Verify(); verifyErr != nil {
		t.Errorf("Topup 不自洽: %v", verifyErr)
	}

	consume, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "6"), UpstreamCost: amount(t, "3"),
		BillingRequestID: 55,
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if verifyErr := consume.Verify(); verifyErr != nil {
		t.Errorf("Consume 不自洽: %v", verifyErr)
	}

	refund, err := Refund(RefundParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "6"), UpstreamCost: amount(t, "3"),
		BillingRequestID: 55, Memo: "上游 500",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if verifyErr := refund.Verify(); verifyErr != nil {
		t.Errorf("Refund 不自洽: %v", verifyErr)
	}

	settle, err := Settle(SettleParams{
		TxnID: txn, UserID: 7, Amount: amount(t, "6"), Asset: AssetCNY,
		PayoutID: 88, Memo: "2026-10 结算",
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if verifyErr := settle.Verify(); verifyErr != nil {
		t.Errorf("Settle 不自洽: %v", verifyErr)
	}

	req, err := PayoutRequest(PayoutRequestParams{
		TxnID: txn, PayoutID: 88, UserID: 7, Amount: amount(t, "6"), Asset: AssetCNY,
	})
	if err != nil {
		t.Fatalf("PayoutRequest: %v", err)
	}
	if verifyErr := req.Verify(); verifyErr != nil {
		t.Errorf("PayoutRequest 不自洽: %v", verifyErr)
	}

	paid, err := PayoutPaid(PayoutPaidParams{
		TxnID: txn, PayoutID: 88, UserID: 7, Amount: amount(t, "6"),
		Asset: AssetCNY, Gateway: "stripe", Ref: "ref-1",
	})
	if err != nil {
		t.Fatalf("PayoutPaid: %v", err)
	}
	if verifyErr := paid.Verify(); verifyErr != nil {
		t.Errorf("PayoutPaid 不自洽: %v", verifyErr)
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

// 结算单号并进备注：交易号是它的哈希，不落备注就对不回那张结算单。
//
// 没有说明时前缀是「平台清算」；给了说明（结算周期、对账批次号）时说明在前。
// 两种情况都锚在分成那一条分录上——Settle 的备注一直写在那里。
func TestSettleRecordsRefInMemo(t *testing.T) {
	txn := newTxnID(t, txnA)

	cases := []struct {
		name string
		memo string
		want string
	}{
		{"没有说明", "", "平台清算（单据 SETTLE-1）"},
		{"有说明", "2026-10 结算", "2026-10 结算（单据 SETTLE-1）"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settle, err := Settle(SettleParams{
				TxnID: txn, UserID: 7, Amount: amount(t, "5"), Asset: AssetCNY,
				PayoutID: 3, Memo: tc.memo, Ref: "SETTLE-1",
			})
			if err != nil {
				t.Fatalf("Settle: %v", err)
			}
			if got := settle.Entries[0].Memo; got != tc.want {
				t.Errorf("备注 = %q，期望 %q", got, tc.want)
			}
		})
	}

	// 不给单号时备注原样保留：Settle 的既有调用方不该因为多了一个字段而变。
	settle, err := Settle(SettleParams{
		TxnID: txn, UserID: 7, Amount: amount(t, "5"), Asset: AssetCNY,
		PayoutID: 3, Memo: "2026-10 结算",
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := settle.Entries[0].Memo; got != "2026-10 结算" {
		t.Errorf("没给单号时备注 = %q，期望原样保留", got)
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

// 调账双向都支持，对家默认是平台收入。
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

	if got := balanceOf(t, add.Entries, UserAccount(42), AssetCNY); got.Cmp(amount(t, "5")) != 0 {
		t.Errorf("加钱的调账后余额 = %s，期望 5", got)
	}
	if got := balanceOf(t, sub.Entries, UserAccount(42), AssetCNY); got.Cmp(amount(t, "-5")) != 0 {
		t.Errorf("扣钱的调账后余额 = %s，期望 -5", got)
	}
	for _, e := range append(append([]Entry{}, add.Entries...), sub.Entries...) {
		if e.Account.Type() != AccountUser && e.Account != PlatformRevenueAccount() {
			t.Errorf("默认的对家只能是平台收入，得到 %s", e.Account)
		}
	}
	// 零金额调账没有意义，且会撞上「金额为零」的分录校验。
	if _, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(1), Amount: money.Zero, Asset: AssetCNY, Memo: "无",
	}); err == nil {
		t.Error("零金额的调账应当被拒")
	}
}

// 对家可以指定，方向跟着金额符号走。
//
// 对账差额的来路不明，把它记为平台收入等于让一笔没人解释过的钱混进
// 经营收入，所以它需要一个单独归集的科目；而这两个方向都要能用，
// 因为差额可正可负。
func TestAdjustmentHonoursCounterAccount(t *testing.T) {
	txn := newTxnID(t, txnA)
	counter := ReconcileDifferenceAccount()

	up, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(42), Amount: amount(t, "5"), Asset: AssetCNY,
		CounterAccount: counter, Memo: "对账差额入账",
	})
	if err != nil {
		t.Fatal(err)
	}
	down, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(42), Amount: amount(t, "-5"), Asset: AssetCNY,
		CounterAccount: counter, Memo: "对账差额入账",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 补钱（金额为正）：借用户、贷对家。对家的余额因此是负的。
	if got := balanceOf(t, up.Entries, counter, AssetCNY); got.Cmp(amount(t, "-5")) != 0 {
		t.Errorf("加钱时对家余额 = %s，期望 -5", got)
	}
	// 扣钱（金额为负）：贷用户、借对家。
	if got := balanceOf(t, down.Entries, counter, AssetCNY); got.Cmp(amount(t, "5")) != 0 {
		t.Errorf("扣钱时对家余额 = %s，期望 5", got)
	}
	// 平台收入一个字都没动——这正是单独归集的目的。
	for _, e := range append(append([]Entry{}, up.Entries...), down.Entries...) {
		if e.Account == PlatformRevenueAccount() {
			t.Error("指定了对家之后不该再落到平台收入上")
		}
	}
	for _, tx := range []Transaction{up, down} {
		if err := tx.Verify(); err != nil {
			t.Errorf("调账不自洽: %v", err)
		}
	}
}

// 对家写成被调科目自己时，两条分录互相抵消、求和为零，于是 Verify 放行
// ——而这笔交易什么都没改。静默成立会让一次「以为修好了」的调账变成空操作。
func TestAdjustmentRejectsSelfCounterAccount(t *testing.T) {
	txn := newTxnID(t, txnA)
	_, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(1), Amount: amount(t, "5"), Asset: AssetCNY,
		CounterAccount: UserAccount(1), Memo: "笔误",
	})
	if err == nil {
		t.Fatal("对家不能是被调科目自己")
	}
	if !strings.Contains(err.Error(), "自己") {
		t.Errorf("报错要说清是对家写成了被调科目，得到: %v", err)
	}
}

// 零值对家按「没填」处理，落到默认的平台收入上。
//
// 与「对家写错」分开说：错的对家在类型层面就构造不出来（Account 的字段
// 不导出，唯一入口 ParseAccount 会校验），所以这里只剩「没填」这一种，
// 而它必须有明确行为而不是报错——人工调账的绝大多数调用点要的正是默认值。
func TestAdjustmentZeroCounterFallsBackToPlatformRevenue(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Adjustment(AdjustmentParams{
		TxnID: txn, Account: UserAccount(1), Amount: amount(t, "5"), Asset: AssetCNY, Memo: "补发",
	})
	if err != nil {
		t.Fatalf("没填对家不该报错: %v", err)
	}
	if got := balanceOf(t, tx.Entries, PlatformRevenueAccount(), AssetCNY); got.Cmp(amount(t, "-5")) != 0 {
		t.Errorf("对家余额 = %s，期望落到 platform_revenue 上的 -5", got)
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

// 只有一侧为负也要在这一句上被拒——判据是 `||`，两侧各是一半。
//
// 变异：把 `||` 改成 `&&`（变异体 transactions.go:186:34，INVERT_LOGICAL）。
// 只一侧为负时那句守卫不再拒，交易会被构造出来、再被自检的「不平」拦下——
// 负的那一项不会记进分录，差额恰好等于它，所以和不为零。**结果仍然报错**，
// 断言里得认出「都不能为负」才分得出来：变异版报的是「分录之和为
// 1.00000000，应为 0」，那读起来像我们算错了，而不是调用方传了负数。
func TestConsumeRejectsNegativeShareOrCost(t *testing.T) {
	txn := newTxnID(t, txnA)
	base := ConsumeParams{TxnID: txn, UserID: 1, Amount: amount(t, "10"), Asset: AssetCNY, MerchantID: 7}

	negShare := base
	negShare.MerchantShare = amount(t, "-1")
	_, err := Consume(negShare)
	if err == nil {
		t.Fatal("负的分成应当被拒")
	}
	if !strings.Contains(err.Error(), "都不能为负") {
		t.Errorf("只有分成为负时应当报「都不能为负」，得到 %v", err)
	}

	negCost := base
	negCost.UpstreamCost = amount(t, "-1")
	_, err = Consume(negCost)
	if err == nil {
		t.Fatal("负的上游成本应当被拒")
	}
	if !strings.Contains(err.Error(), "都不能为负") {
		t.Errorf("只有上游成本为负时应当报「都不能为负」，得到 %v", err)
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
	// 负的分成：只有一侧为负也要在这一句上被拒。
	//
	// 变异：把 Refund 里的 `||` 改成 `&&`（变异体 transactions.go:268:34，
	// INVERT_LOGICAL）。只一侧为负时那句守卫不再拒，交易被构造出来、再被自检
	// 的「不平」拦下（理由与 TestConsumeRejectsNegativeShareOrCost 里那段相同，
	// 两侧的判据是分开写的两处，改一处不影响另一处）。
	_, err := Refund(RefundParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "10"), Asset: AssetCNY, MerchantShare: amount(t, "-1"),
	})
	if err == nil {
		t.Fatal("负的分成应当被拒")
	}
	if !strings.Contains(err.Error(), "都不能为负") {
		t.Errorf("只有分成为负时应当报「都不能为负」，得到 %v", err)
	}

	// 另一半：只有上游成本为负。这一半原先一格都没有。
	_, err = Refund(RefundParams{
		TxnID: txn, UserID: 1, Amount: amount(t, "10"), Asset: AssetCNY, UpstreamCost: amount(t, "-1"),
	})
	if err == nil {
		t.Fatal("负的上游成本应当被拒")
	}
	if !strings.Contains(err.Error(), "都不能为负") {
		t.Errorf("只有上游成本为负时应当报「都不能为负」，得到 %v", err)
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

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 下面这一组来自变异测试（gremlins unleash）：把某个比较符或分支方向
// 改一下，测试**仍然通过**。这一类缺口用覆盖率看不出来——行被执行过了，
// 但边界上的那一支没被走到。

// 手续费恰好等于充值额时应当被接受，只有**大于**才拒。
//
// 变异：把 `Cmp(p.Amount) > 0` 改成 `>= 0`，恰好相等就被误拒。
// 原先只测过「手续费明显大于金额」，没测相等这个边界。
func TestTopupGatewayFeeExactlyEqualAmount(t *testing.T) {
	p := TopupParams{
		TxnID: NewTxnID(), UserID: 1, Asset: AssetCNY,
		Amount: money.MustParse("100"), Gateway: "placeholder-gateway",
		GatewayFee: money.MustParse("100"),
	}
	if _, err := Topup(p); err != nil {
		t.Errorf("手续费恰好等于充值额应当被接受，得到 %v", err)
	}

	// 大一点点就该拒
	p.GatewayFee = money.MustParse("100.00000001")
	if _, err := Topup(p); err == nil {
		t.Error("手续费大于充值额应被拒绝")
	}
}

// 用户 id 为 0 必须被拒——它是「没传」的表现，而不是一个合法的 id。
//
// 断言里要认出「用户 id 非法: 0」这句文案——**连插值出来的那个数一起认**：
// 只认「用户 id 非法」的话，把 `%d` 换成别的（或插错一个数）仍然绿。把
// `UserID <= 0` 改成 `< 0`（变异体 transactions.go:183:14，CONDITIONALS_BOUNDARY）
// 之后，0 会被放行到分录层，再由 `ParseAccount("user:0")` 拦下（0 号用户不存在，
// 见 account.go 的「为什么 0 不算读得出」）——**结果同样是报错**，所以只判
// `err != nil` 的用例照样绿。两版的差别在文案，而这两句话指的是**两类故障**：
// 原样是「调用方漏填」，变异版读起来像「我们构造出了一组不自洽的分录」。
//
// 说「两类」只是分清这两句各自在说什么，**不是说账本这一层今天会按类型分成两种
// 响应**：这一层没有任何按类型分的机制——包内没有对应哨兵（只有 ErrInvalidTxnID），
// `internal/pipeline` 把 `Consume` 的错一律包成「构造账本分录失败: %w」，
// CLI 的 `usageErr` 也不认这两句。
func TestConsumeRejectsZeroUserID(t *testing.T) {
	p := ConsumeParams{
		TxnID: NewTxnID(), UserID: 0, Asset: AssetCNY,
		Amount: money.MustParse("1"),
	}
	_, err := Consume(p)
	if err == nil {
		t.Fatal("用户 id 为 0 应被拒绝")
	}
	if !strings.Contains(err.Error(), "用户 id 非法: 0") {
		t.Errorf("0 应当由构造函数那句守卫拒（文案里连插值出来的 0 一起认），而不是拖到分录层；得到 %v", err)
	}
	if _, err := Consume(ConsumeParams{
		TxnID: NewTxnID(), UserID: -1, Asset: AssetCNY,
		Amount: money.MustParse("1"),
	}); err == nil {
		t.Error("负的用户 id 应被拒绝")
	}
}

// Topup 侧的同一条守卫也要认出那句文案。
//
// 变异：transactions.go:92:14（CONDITIONALS_BOUNDARY）。0 被放行之后仍会被分录层
// 拦下（`user:0` 不是规范写法），所以「被拒」这个结果两版一样，差的是文案与故障
// 类别（见上一条用例的注释）。三个构造函数各写一句守卫，改一处不影响另外两处，
// 所以每一处都要单独钉。
func TestTopupRejectsZeroUserIDAtTheGuard(t *testing.T) {
	_, err := Topup(TopupParams{
		TxnID: NewTxnID(), UserID: 0, Amount: money.MustParse("1"), Asset: AssetCNY,
		Gateway: "placeholder-gateway",
	})
	if err == nil {
		t.Fatal("用户 id 为 0 应被拒绝")
	}
	if !strings.Contains(err.Error(), "用户 id 非法: 0") {
		t.Errorf("0 应当由构造函数那句守卫拒（文案里连插值出来的 0 一起认），而不是拖到分录层；得到 %v", err)
	}
}

// 退款侧同一条守卫（变异体 transactions.go:265:14），理由与上面两条相同。
func TestRefundRejectsZeroUserIDAtTheGuard(t *testing.T) {
	_, err := Refund(RefundParams{
		TxnID: NewTxnID(), UserID: 0, Amount: money.MustParse("1"), Asset: AssetCNY,
	})
	if err == nil {
		t.Fatal("用户 id 为 0 应被拒绝")
	}
	if !strings.Contains(err.Error(), "用户 id 非法: 0") {
		t.Errorf("0 应当由构造函数那句守卫拒（文案里连插值出来的 0 一起认），而不是拖到分录层；得到 %v", err)
	}
}

// 没给计费请求时，Consume 不该往分录上挂「关联到 0」。
//
// 变异：把 Consume 里的 `if p.BillingRequestID > 0` 改成 `>= 0`（变异体
// transactions.go:210:24，CONDITIONALS_BOUNDARY）。BillingRequestID 是零值时
// （绝大多数调用都这样）每条分录都会拿到 `&0`，而整笔交易**仍然自洽、照样返回**
// ——`nil` 与 `&0` 在这一层不是一回事：落到库里是 NULL 与一个指向
// billing_requests 的 0，后者追不出任何一次请求。退款侧同一条判据的两半在
// TestRefundAttachesBillingRequest 里都有，消费侧原先只判了「给了的时候挂上」。
func TestConsumeWithoutBillingRequestLeavesItUnset(t *testing.T) {
	tx, err := Consume(ConsumeParams{
		TxnID: NewTxnID(), UserID: 1, Amount: money.MustParse("1"), Asset: AssetCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tx.Entries {
		if e.BillingRequestID != nil {
			t.Errorf("没给计费请求时分录 %s 不该挂上 billing_request_id，得到 %d",
				e.Account, *e.BillingRequestID)
		}
	}
}

// 提现打款带了单据号时，备注里要带上它。
//
// 变异：把 `p.Ref != ""` 改成 `p.Ref == ""`，有单据号时反而不带，
// 而原先的用例只断言了「不带单据号时的备注」，没断言带的时候。
func TestPayoutPaidMemoCarriesReference(t *testing.T) {
	base := PayoutPaidParams{
		TxnID: NewTxnID(), PayoutID: 1, UserID: 7, Asset: AssetCNY,
		Amount: money.MustParse("10"), Gateway: "placeholder-gateway",
	}

	// 不带单据号
	tr, err := PayoutPaid(base)
	if err != nil {
		t.Fatal(err)
	}
	var withoutRef string
	for _, e := range tr.Entries {
		if e.Memo != "" {
			withoutRef = e.Memo
		}
	}

	// 带单据号
	base.Ref = "ORDER-1"
	tr, err = PayoutPaid(base)
	if err != nil {
		t.Fatal(err)
	}
	var withRef string
	for _, e := range tr.Entries {
		if e.Memo != "" {
			withRef = e.Memo
		}
	}
	if withRef == withoutRef {
		t.Error("带单据号时备注应当不同")
	}
	if !strings.Contains(withRef, "ORDER-1") {
		t.Errorf("备注里应带单据号，得到 %q", withRef)
	}
}

// 退款的单据号与提现申请单号同样要进分录备注。
//
// 变异：去掉 Refund / PayoutRequest 里的 memoWithOrder，这一组会红——而幂等本身
// 仍然正常（交易号照样从键算得出来），所以别的用例看不出来。少了这一栏，事后
// 只能反算哈希才知道这张退款单 / 申请单对应哪一笔账，而那个方向算不回来。
func TestRefundAndPayoutRequestMemosCarryReference(t *testing.T) {
	refund, err := Refund(RefundParams{
		TxnID: NewTxnID(), UserID: 1, Amount: money.MustParse("10"), Asset: AssetCNY,
		Memo: "上游 500", Ref: "REF-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := refund.Entries[0].Memo; got != "退款：上游 500（单据 REF-9）" {
		t.Errorf("退款分录备注 = %q，期望带上原因与单据号", got)
	}

	// 没给原因时也带单据号，只是没有「退款：…」那一段。
	plain, err := Refund(RefundParams{
		TxnID: NewTxnID(), UserID: 1, Amount: money.MustParse("1"), Asset: AssetCNY,
		Ref: "REF-10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.Entries[0].Memo; got != "退款（单据 REF-10）" {
		t.Errorf("没有原因的退款备注 = %q，期望「退款（单据 REF-10）」", got)
	}

	req, err := PayoutRequest(PayoutRequestParams{
		TxnID: NewTxnID(), PayoutID: 3, UserID: 7, Amount: money.MustParse("5"),
		Asset: AssetCNY, Ref: "REQ-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 单号挂在 payout 那一条上（这张申请单冻住的那一笔），分成那一侧不重复写。
	if got := req.Entries[0].Memo; got != "" {
		t.Errorf("分成分录的备注 = %q，期望为空", got)
	}
	if got := req.Entries[1].Memo; got != "提现申请（单据 REQ-9）" {
		t.Errorf("待付分录备注 = %q，期望带上申请单号", got)
	}
}

// 单据号为空串与只有空白，都应视为「没有单据号」。
//
// 变异：`orderNo == ""` 改成别的会让空串走进拼接分支，
// 产生一条「（单据 ）」这样带空括号的备注。
func TestMemoWithOrderEmptyVariants(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		got := memoWithOrder("提现打款", in)
		if strings.Contains(got, "单据") {
			t.Errorf("输入 %q 不该产生单据备注，得到 %q", in, got)
		}
	}
	// 非空时应当带上
	if got := memoWithOrder("提现打款", "R-1"); !strings.Contains(got, "R-1") {
		t.Errorf("应带单据号，得到 %q", got)
	}
	// 单据号里的控制字符要被清掉：它来自外部系统
	if got := memoWithOrder("提现打款", "R\n1"); strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("单据号里的控制字符应被清洗，得到 %q", got)
	}
}

// 分成与上游成本之和**恰好**等于售价时应当被接受，只有大于才拒。
//
// 变异：把 `Cmp(p.Amount) > 0` 改成 `>= 0`，恰好相等的那些单被误拒——而它们
// 正是「上游成本原样转给用户」这一类零毛利单，拒掉等于整类单发不出去。
// 原先只测过「之和明显超过售价」，没测相等这个边界。
func TestConsumeZeroMarginIsAccepted(t *testing.T) {
	p := ConsumeParams{
		TxnID: NewTxnID(), UserID: 1, Asset: AssetCNY,
		Amount:        money.MustParse("10"),
		MerchantShare: money.MustParse("5"),
		MerchantID:    7,
		UpstreamCost:  money.MustParse("5"),
	}
	if _, err := Consume(p); err != nil {
		t.Errorf("分成与上游成本之和恰好等于售价时应当被接受，得到 %v", err)
	}

	// 大一点点就该拒：那一单在亏钱，该改定价而不是写负收入。
	p.UpstreamCost = money.MustParse("5.00000001")
	if _, err := Consume(p); err == nil {
		t.Error("分成与成本之和超过售价应被拒绝")
	}
}

// 退款侧的同一条边界：要冲回的分成与成本恰好等于退款额时应当被接受。
//
// 变异：把 Refund 里的 `Cmp(p.Amount) > 0` 改成 `>= 0`。Consume 与 Refund 的
// 判据是**分开写的两处**，改一处不影响另一处，所以上面的用例管不到这里——
// 退款侧的这条边界原先一格都没有。
func TestRefundZeroMarginIsAccepted(t *testing.T) {
	p := RefundParams{
		TxnID: NewTxnID(), UserID: 1, Asset: AssetCNY,
		Amount:        money.MustParse("10"),
		MerchantShare: money.MustParse("5"),
		MerchantID:    7,
		UpstreamCost:  money.MustParse("5"),
	}
	if _, err := Refund(p); err != nil {
		t.Errorf("要冲回的分成与成本之和恰好等于退款额时应当被接受，得到 %v", err)
	}

	// 大一点点就该拒：多冲回的那一分钱没有对应的消费。
	p.UpstreamCost = money.MustParse("5.00000001")
	if _, err := Refund(p); err == nil {
		t.Error("要冲回的分成与成本之和超过退款额应被拒绝")
	}
}

// 退款分录也要挂上被退的那次计费请求——与 Consume 侧那条同构。
//
// 变异：把 Refund 里的 `if p.BillingRequestID > 0` 方向改掉，给了计费请求的
// 那些分录退化成没有关联。两处判据分开写，Consume 侧那条管不到这里，而少了
// billing_request_id 的退款在争议发生时无从追溯「退的是哪一次请求」。
func TestRefundAttachesBillingRequest(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Refund(RefundParams{
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

	// 反着也要判：没给计费请求时，字段该是**空的**。「关联到 0」与「没有关联」
	// 在这一层不能混同，少判这一半的话判据往边界上偏一格仍然通过。
	plain, err := Refund(RefundParams{
		TxnID: newTxnID(t, txnB), UserID: 1, Amount: amount(t, "1"), Asset: AssetCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plain.Entries {
		if e.BillingRequestID != nil {
			t.Errorf("没给计费请求时分录 %s 不该挂上 billing_request_id，得到 %d",
				e.Account, *e.BillingRequestID)
		}
	}
}

// PayoutReject 是 PayoutRequest 的镜像：借 merchant_payable、贷 payout。
//
// 金额、科目、方向与 payout_id 逐条钉住，而不只断言「没报错」：这一组分录写反
// 方向同样和为零（与被测的那一组一样「平」），而它会**再冻一次**商户的分成——
// 驳回之后钱还留在 payout:<id> 上，账面看不出来，只有等到那一栏长期不平才发现。
//
// 变异：把两条分录的方向对调（debit/credit 互换），这条用例红；只断言
// 「2 条分录、和为 0」的用例不会红。
func TestPayoutRejectMirrorsRequest(t *testing.T) {
	txn := newTxnID(t, txnA)
	tr, err := PayoutReject(PayoutRejectParams{
		TxnID: txn, PayoutID: 3, UserID: 7, Amount: amount(t, "5"), Asset: AssetCNY,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 借为正、贷为负（见 Entry.Amount），所以退回来的分成是正的，
	// 核销掉的那条待付是负的。
	want := map[string]string{
		MerchantPayableAccount(7).String(): amount(t, "5").String(),
		PayoutAccount(3).String():          amount(t, "-5").String(),
	}
	got := map[string]string{}
	for _, e := range tr.Entries {
		if e.EntryType != EntryPayoutReject {
			t.Errorf("分录 %s 的类型 = %q，期望 %q", e.Account, e.EntryType, EntryPayoutReject)
		}
		if e.PayoutID == nil || *e.PayoutID != 3 {
			t.Errorf("分录 %s 没有挂上 payout_id 3", e.Account)
		}
		got[e.Account.String()] = e.Amount.String()
	}
	if len(got) != len(want) {
		t.Fatalf("写了 %d 条分录（%v），期望 %d 条", len(got), got, len(want))
	}
	for account, amount := range want {
		if got[account] != amount {
			t.Errorf("科目 %s 的金额 = %q，期望 %q", account, got[account], amount)
		}
	}

	// 申请与驳回必须是同一组科目的两条相反分录，不然这一对在账本里对不上。
	req, err := PayoutRequest(PayoutRequestParams{
		TxnID: newTxnID(t, txnB), PayoutID: 3, UserID: 7, Amount: amount(t, "5"), Asset: AssetCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	flipped := map[string]string{}
	for _, e := range req.Entries {
		flipped[e.Account.String()] = e.Amount.Neg().String()
	}
	for account, amount := range want {
		if flipped[account] != amount {
			t.Errorf("驳回时 %s 的金额 = %q，期望是申请那一笔的反向 %q",
				account, amount, flipped[account])
		}
	}
}

// 驳回的输入校验：提现单 id、商户 id、金额与资产各判一遍。
//
// 这几条与 PayoutRequest 是同一批判据（都用 validateCommon 加那两个 id），
// 但它们各自挡下的是一类**会把分录记到错归属上**的输入：没有单 id 的核销
// 分不清是哪条待付，没有商户 id 的退回分不清还给谁。
func TestPayoutRejectRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *PayoutRejectParams)
	}{
		{"缺提现单 id", func(p *PayoutRejectParams) { p.PayoutID = 0 }},
		{"缺商户 id", func(p *PayoutRejectParams) { p.UserID = 0 }},
		{"金额为零", func(p *PayoutRejectParams) { p.Amount = money.Zero }},
		{"金额为负", func(p *PayoutRejectParams) { p.Amount = money.MustParse("-5") }},
		{"资产不认识", func(p *PayoutRejectParams) { p.Asset = Asset("USDT") }},
		{"缺交易号", func(p *PayoutRejectParams) { p.TxnID = "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PayoutRejectParams{
				TxnID: newTxnID(t, txnA), PayoutID: 3, UserID: 7,
				Amount: amount(t, "5"), Asset: AssetCNY,
			}
			tc.mutate(&p)
			if _, err := PayoutReject(p); err == nil {
				t.Error("这条输入应当被拒，却构造出了一笔交易")
			}
		})
	}
}

// 额度包的三段账走完，prepaid 的余额依次等于「还剩多少面值」：
// 发放后是满的、消费后少一截、作废后归零。它们靠余额判，不靠分录条数
// ——条数写对而方向写反时，账面照样平，只有余额会反。
func TestPlanBalanceTracksRemainingFaceValue(t *testing.T) {
	grantTxn := newTxnID(t, txnA)
	grant, err := PlanGrant(PlanGrantParams{
		TxnID: grantTxn, SubscriptionID: 11, Amount: amount(t, "100"),
		Asset: AssetCNY, Ref: "grant-1",
	})
	if err != nil {
		t.Fatalf("PlanGrant: %v", err)
	}
	prepaid := PrepaidAccount(11)
	if got := balanceOf(t, grant.Entries, prepaid, AssetCNY); got.Cmp(amount(t, "100")) != 0 {
		t.Fatalf("发放后 prepaid 余额 = %s，期望 100", got)
	}
	if got := balanceOf(t, grant.Entries, PlatformRevenueAccount(), AssetCNY); got.Cmp(amount(t, "-100")) != 0 {
		t.Errorf("发放后平台收入 = %s，期望 -100（让出去的额度）", got)
	}

	consumeTxn := newTxnID(t, txnB)
	consume, err := Consume(ConsumeParams{
		TxnID: consumeTxn, UserID: 42, Amount: amount(t, "30"), Asset: AssetCNY,
		PrepaidAccount: prepaid,
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got := balanceOf(t, consume.Entries, prepaid, AssetCNY); got.Cmp(amount(t, "-30")) != 0 {
		t.Fatalf("消费后 prepaid 余额 = %s，期望 -30（与发放那笔相加才是剩下的 70）", got)
	}

	voidTxn := newTxnID(t, txnC)
	void, err := PlanVoid(PlanVoidParams{
		TxnID: voidTxn, SubscriptionID: 11, Amount: amount(t, "70"),
		Asset: AssetCNY, Reason: "到期", Ref: "void-1",
	})
	if err != nil {
		t.Fatalf("PlanVoid: %v", err)
	}

	// 三段合起来看：prepaid 归零，平台收入也归零——发放让出的额度要么被消费
	// 记成收入，要么在作废时收回，两条路都把那一份还回去。
	all := append(append(append([]Entry{}, grant.Entries...), consume.Entries...), void.Entries...)
	if got := balanceOf(t, all, prepaid, AssetCNY); !got.IsZero() {
		t.Errorf("生命周期走完后 prepaid 余额 = %s，期望 0", got)
	}
	if got := balanceOf(t, all, PlatformRevenueAccount(), AssetCNY); !got.IsZero() {
		t.Errorf("生命周期走完后平台收入 = %s，期望 0", got)
	}
}

// 从额度包扣的消费，用户侧那条分录换成了 prepaid，商户分成与平台收入不变。
// 分成那一行是商户分成（#732）在额度包覆盖的调用上照常成立的地方。
func TestConsumeFromPrepaidKeepsSplit(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "6"), UpstreamCost: amount(t, "3"),
		PrepaidAccount: PrepaidAccount(11),
	})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	byAccount := map[string]money.Amount{}
	for _, e := range tx.Entries {
		byAccount[e.Account.String()] = e.Amount
	}
	if _, has := byAccount["user:42"]; has {
		t.Error("从额度包扣时不该再出现 user 科目：钱不是从用户钱包出的")
	}
	if got := byAccount["prepaid:11"]; got.Cmp(amount(t, "-10")) != 0 {
		t.Errorf("额度包 = %s，期望 -10", got)
	}
	if got := byAccount["merchant_payable:7"]; got.Cmp(amount(t, "6")) != 0 {
		t.Errorf("应付商户 = %s，期望 6", got)
	}
	if got := byAccount["upstream_cost"]; got.Cmp(amount(t, "3")) != 0 {
		t.Errorf("上游成本 = %s，期望 3", got)
	}
	if got := byAccount["platform_revenue"]; got.Cmp(amount(t, "1")) != 0 {
		t.Errorf("平台收入 = %s，期望 1（与从钱包扣时一样）", got)
	}
}

// 额度包科目只放行 prepaid：传别的合法科目进来同样要拒，不能因为「它是个
// 合法科目」就把它当成扣款来源。
func TestConsumeRejectsNonPrepaidAccount(t *testing.T) {
	txn := newTxnID(t, txnA)
	bad := []Account{
		UserAccount(42),
		PlatformRevenueAccount(),
		MerchantPayableAccount(7),
	}
	for _, acct := range bad {
		if _, err := Consume(ConsumeParams{
			TxnID: txn, UserID: 42, Amount: amount(t, "1"), Asset: AssetCNY,
			PrepaidAccount: acct,
		}); err == nil {
			t.Errorf("从 %q 扣的消费应当被拒", acct)
		}
	}
}

// 授予与作废的备注要带单号：交易号是单号的哈希，反过来算不回来，
// 不落备注就再没有第二条路把这张单子对上这笔账。
func TestPlanMemosCarryReference(t *testing.T) {
	grant, err := PlanGrant(PlanGrantParams{
		TxnID: newTxnID(t, txnA), SubscriptionID: 11, Amount: amount(t, "100"),
		Asset: AssetCNY, Ref: "grant-42",
	})
	if err != nil {
		t.Fatalf("PlanGrant: %v", err)
	}
	if !strings.Contains(grant.Entries[0].Memo, "grant-42") {
		t.Errorf("发放备注里没有单号: %q", grant.Entries[0].Memo)
	}

	void, err := PlanVoid(PlanVoidParams{
		TxnID: newTxnID(t, txnB), SubscriptionID: 11, Amount: amount(t, "70"),
		Asset: AssetCNY, Reason: "人工作废", Ref: "void-42",
	})
	if err != nil {
		t.Fatalf("PlanVoid: %v", err)
	}
	if !strings.Contains(void.Entries[0].Memo, "void-42") {
		t.Errorf("作废备注里没有单号: %q", void.Entries[0].Memo)
	}
	if !strings.Contains(void.Entries[0].Memo, "人工作废") {
		t.Errorf("作废备注里没有处置原因: %q", void.Entries[0].Memo)
	}
}

// 非法输入不构造交易：订阅 id 必须有、金额必须为正。
func TestPlanConstructorsRejectBadInput(t *testing.T) {
	txn := newTxnID(t, txnA)

	if _, err := PlanGrant(PlanGrantParams{
		TxnID: txn, SubscriptionID: 0, Amount: amount(t, "100"), Asset: AssetCNY,
	}); err == nil {
		t.Error("订阅 id 为 0 的发放应当被拒")
	}
	if _, err := PlanGrant(PlanGrantParams{
		TxnID: txn, SubscriptionID: 11, Amount: money.Zero, Asset: AssetCNY,
	}); err == nil {
		t.Error("零金额的发放应当被拒")
	}
	if _, err := PlanVoid(PlanVoidParams{
		TxnID: txn, SubscriptionID: -1, Amount: amount(t, "1"), Asset: AssetCNY,
	}); err == nil {
		t.Error("负订阅 id 的作废应当被拒")
	}
	if _, err := PlanVoid(PlanVoidParams{
		TxnID: txn, SubscriptionID: 11, Amount: amount(t, "-1"), Asset: AssetCNY,
	}); err == nil {
		t.Error("负金额的作废应当被拒")
	}
}

// 期末结转把面值从 prepaid 挪到 carryover，两边是同一个订阅；并入把
// carryover 挪进新订阅的 prepaid。两段合起来之后，平台欠用户的总数不变
// ——只是「这个订阅还能花」变成了「待并入」再变成「新订阅能花」。
func TestPlanCarryMovesFaceValueToCarryover(t *testing.T) {
	grantTxn := newTxnID(t, txnA)
	grant, err := PlanGrant(PlanGrantParams{
		TxnID: grantTxn, SubscriptionID: 11, Amount: amount(t, "100"),
		Asset: AssetCNY, Ref: "grant-1",
	})
	if err != nil {
		t.Fatalf("PlanGrant: %v", err)
	}

	carryTxn := newTxnID(t, txnB)
	carry, err := PlanCarry(PlanCarryParams{
		TxnID: carryTxn, SubscriptionID: 11, Amount: amount(t, "40"),
		Asset: AssetCNY, Reason: "到期", Ref: "void-1",
	})
	if err != nil {
		t.Fatalf("PlanCarry: %v", err)
	}

	voidTxn := newTxnID(t, txnC)
	void, err := PlanVoid(PlanVoidParams{
		TxnID: voidTxn, SubscriptionID: 11, Amount: amount(t, "60"),
		Asset: AssetCNY, Reason: "到期", Ref: "void-1",
	})
	if err != nil {
		t.Fatalf("PlanVoid: %v", err)
	}

	// 期末处置的两笔合起来：prepaid 归零、carryover = 40、平台收回 60。
	disposal := append(append([]Entry{}, carry.Entries...), void.Entries...)
	if got := balanceOf(t, append(grant.Entries, disposal...), PrepaidAccount(11), AssetCNY); !got.IsZero() {
		t.Errorf("期末处置之后 prepaid 余额 = %s，期望 0", got)
	}
	if got := balanceOf(t, disposal, CarryoverAccount(11), AssetCNY); got.Cmp(amount(t, "40")) != 0 {
		t.Errorf("结转之后 carryover 余额 = %s，期望 40", got)
	}
	if got := balanceOf(t, disposal, PlatformRevenueAccount(), AssetCNY); got.Cmp(amount(t, "60")) != 0 {
		t.Errorf("期末归平台 = %s，期望 60（100 − 40）", got)
	}

	// 并入新订阅 12：carryover 清零，新订阅拿到 40。
	mergeTxn := newTxnID(t, "44444444-4444-4444-8444-444444444444")
	merge, err := PlanCarryMerge(PlanCarryMergeParams{
		TxnID: mergeTxn, TargetSubscriptionID: 12,
		Moves: []PlanCarryMove{{SourceSubscriptionID: 11, Amount: amount(t, "40")}},
		Asset: AssetCNY, Ref: "grant-2",
	})
	if err != nil {
		t.Fatalf("PlanCarryMerge: %v", err)
	}
	all := append(append([]Entry{}, grant.Entries...), disposal...)
	all = append(all, merge.Entries...)
	if got := balanceOf(t, all, CarryoverAccount(11), AssetCNY); !got.IsZero() {
		t.Errorf("并入之后 carryover 余额 = %s，期望 0", got)
	}
	if got := balanceOf(t, all, PrepaidAccount(12), AssetCNY); got.Cmp(amount(t, "40")) != 0 {
		t.Errorf("并入之后新订阅 prepaid = %s，期望 40", got)
	}
	// 平台欠用户的总数（prepaid + carryover）在整个链条里不变：40。
	sum := balanceOf(t, all, PrepaidAccount(11), AssetCNY).
		Add(balanceOf(t, all, PrepaidAccount(12), AssetCNY)).
		Add(balanceOf(t, all, CarryoverAccount(11), AssetCNY))
	if sum.Cmp(amount(t, "40")) != 0 {
		t.Errorf("结转出来那部分的总数 = %s，期望 40（只换了科目，没有增减）", sum)
	}
}

// 一次并入可以带多个源：每个源一条贷方，目标一条借方，和为零。
func TestPlanCarryMergeMultipleSources(t *testing.T) {
	txn, err := PlanCarryMerge(PlanCarryMergeParams{
		TxnID: newTxnID(t, txnA), TargetSubscriptionID: 20,
		Moves: []PlanCarryMove{
			{SourceSubscriptionID: 11, Amount: amount(t, "30")},
			{SourceSubscriptionID: 12, Amount: amount(t, "70")},
		},
		Asset: AssetCNY, Ref: "grant-9",
	})
	if err != nil {
		t.Fatalf("PlanCarryMerge: %v", err)
	}
	if got := balanceOf(t, txn.Entries, PrepaidAccount(20), AssetCNY); got.Cmp(amount(t, "100")) != 0 {
		t.Errorf("并入之后新订阅 prepaid = %s，期望 100", got)
	}
	for _, id := range []int64{11, 12} {
		if got := balanceOf(t, txn.Entries, CarryoverAccount(id), AssetCNY); !got.IsNegative() {
			t.Errorf("源订阅 %d 的 carryover 分录 = %s，期望为负（转出去）", id, got)
		}
	}
}

// 结转与并入的构造函数挡住不自洽的输入：零金额、重复源、空的并入、
// 非法的订阅 id。
func TestPlanCarryConstructorsRejectBadInput(t *testing.T) {
	txn := newTxnID(t, txnA)

	if _, err := PlanCarry(PlanCarryParams{
		TxnID: txn, SubscriptionID: 0, Amount: amount(t, "10"), Asset: AssetCNY,
	}); err == nil {
		t.Error("订阅 id 为 0 的结转应当被拒")
	}
	if _, err := PlanCarry(PlanCarryParams{
		TxnID: txn, SubscriptionID: 11, Amount: money.Zero, Asset: AssetCNY,
	}); err == nil {
		t.Error("零金额的结转应当被拒")
	}

	base := PlanCarryMergeParams{
		TxnID: txn, TargetSubscriptionID: 20,
		Moves: []PlanCarryMove{{SourceSubscriptionID: 11, Amount: amount(t, "10")}},
		Asset: AssetCNY,
	}
	bad := map[string]func(*PlanCarryMergeParams){
		"没有源": func(p *PlanCarryMergeParams) { p.Moves = nil },
		"源 id 为 0": func(p *PlanCarryMergeParams) {
			p.Moves = []PlanCarryMove{{SourceSubscriptionID: 0, Amount: amount(t, "10")}}
		},
		"金额为负": func(p *PlanCarryMergeParams) {
			p.Moves = []PlanCarryMove{{SourceSubscriptionID: 11, Amount: amount(t, "-1")}}
		},
		"源重复": func(p *PlanCarryMergeParams) {
			p.Moves = append(p.Moves, PlanCarryMove{SourceSubscriptionID: 11, Amount: amount(t, "5")})
		},
		"没有目标": func(p *PlanCarryMergeParams) { p.TargetSubscriptionID = 0 },
	}
	for name, mutate := range bad {
		p := base
		mutate(&p)
		if _, err := PlanCarryMerge(p); err == nil {
			t.Errorf("%s 的并入应当被拒", name)
		}
	}
}
