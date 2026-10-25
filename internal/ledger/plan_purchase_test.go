package ledger

import (
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// TestPlanPurchaseShape 钉住售卖那一笔分录的科目、方向与类型。
//
// 它和 PlanGrant 只差对方科目——这一条就是「卖的」与「送的」在账面上的区别：
// 贷方是 payment_gateway（钱从通道进来），不是 platform_revenue（平台让出收入）。
func TestPlanPurchaseShape(t *testing.T) {
	txnID := TxnID("11111111-2222-4333-8444-555555555555")
	subID := int64(42)

	withFee, err := PlanPurchase(PlanPurchaseParams{
		TxnID:          txnID,
		SubscriptionID: subID,
		Amount:         money.MustParse("100"),
		Asset:          AssetCNY,
		Gateway:        "stub",
		GatewayFee:     money.MustParse("1.5"),
		OrderNo:        "ORD-1",
	})
	if err != nil {
		t.Fatalf("构造售卖分录失败: %v", err)
	}
	if len(withFee.Entries) != 4 {
		t.Fatalf("带手续费的售卖该有 4 条分录，实际 %d 条", len(withFee.Entries))
	}
	want := map[Account]money.Amount{
		PrepaidAccount(subID):         money.MustParse("100"),
		PaymentGatewayAccount("stub"): money.MustParse("-100"),
		UpstreamCostAccount():         money.MustParse("1.5"),
		PlatformRevenueAccount():      money.MustParse("-1.5"),
	}
	for _, e := range withFee.Entries {
		if e.EntryType != EntryPlanPurchase {
			t.Errorf("科目 %s 的分录类型 = %q，期望 %q", e.Account, e.EntryType, EntryPlanPurchase)
		}
		amount, ok := want[e.Account]
		if !ok {
			t.Errorf("多出一条不该有的分录：%s", e)
			continue
		}
		if e.Amount.Cmp(amount) != 0 {
			t.Errorf("科目 %s 的金额 = %s，期望 %s", e.Account, e.Amount, amount)
		}
	}

	noFee, err := PlanPurchase(PlanPurchaseParams{
		TxnID:          txnID,
		SubscriptionID: subID,
		Amount:         money.MustParse("100"),
		Asset:          AssetCNY,
		Gateway:        "stub",
	})
	if err != nil {
		t.Fatalf("构造无手续费的售卖分录失败: %v", err)
	}
	if len(noFee.Entries) != 2 {
		t.Errorf("没有手续费时该有 2 条分录，实际 %d 条", len(noFee.Entries))
	}
}

// TestPlanPurchaseRejectsBadInput 钉住构造前的几条护栏。
func TestPlanPurchaseRejectsBadInput(t *testing.T) {
	base := PlanPurchaseParams{
		TxnID:          TxnID("11111111-2222-4333-8444-555555555555"),
		SubscriptionID: 7,
		Amount:         money.MustParse("10"),
		Asset:          AssetCNY,
		Gateway:        "stub",
	}
	cases := []struct {
		name string
		edit func(*PlanPurchaseParams)
		want string
	}{
		{"没有订阅", func(p *PlanPurchaseParams) { p.SubscriptionID = 0 }, "订阅 id 非法"},
		{"没有通道", func(p *PlanPurchaseParams) { p.Gateway = "" }, "支付通道"},
		{"手续费为负", func(p *PlanPurchaseParams) { p.GatewayFee = money.MustParse("-1") }, "手续费为负"},
		{"手续费超过面值", func(p *PlanPurchaseParams) { p.GatewayFee = money.MustParse("11") }, "大于额度包面值"},
		{"金额为零", func(p *PlanPurchaseParams) { p.Amount = money.Zero }, "为正数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.edit(&p)
			_, err := PlanPurchase(p)
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里没有 %q：%v", tc.want, err)
			}
		})
	}
}

// 手续费恰好等于面值：**放行**。护栏拒的是「大于面值」，相等是边界上的另一侧，
// 必须落在放行这一边。
//
// 它与上表的「手续费超过面值」成对：只测 11 > 10 的话，把 `>` 写成 `>=` 也能
// 全绿——那会把「手续费正好吃掉整个面值」这种合法情况判成不可能。
func TestPlanPurchaseAllowsFeeEqualToFaceValue(t *testing.T) {
	txn, err := PlanPurchase(PlanPurchaseParams{
		TxnID:          TxnID("11111111-2222-4333-8444-555555555555"),
		SubscriptionID: 7,
		Amount:         money.MustParse("10"),
		Asset:          AssetCNY,
		Gateway:        "stub",
		GatewayFee:     money.MustParse("10"),
		OrderNo:        "ORD-FEE-CAP",
	})
	if err != nil {
		t.Fatalf("手续费等于面值应当放行（拒的是大于）: %v", err)
	}
	if len(txn.Entries) != 4 {
		t.Fatalf("带手续费的售卖该有 4 条分录，实际 %d 条", len(txn.Entries))
	}
}

// 记一处**等价**变异，免得下一轮有人当成缺口去补：
//
// PlanCarryMerge 里 `entries := make([]Entry, 0, len(p.Moves)+1)`
// （transactions.go:617）的 `+1` 会被算术类变异器改成 `-1`。它看着像「负容量
// 要 panic」，实际是等价的：同一个函数在前面先拒了空并入
// （transactions.go:587 的 `if len(p.Moves) == 0`），所以走到这一行时
// `len(p.Moves)` 最小是 1，减一之后最小是 0。make 的容量只要求非负，0 合法，
// 不 panic；而容量只是提示，append 会自己扩容，任何可观察行为都不变。
//
// 所以这一格不造用例——造了也只能靠「读代码读出来的等价」，用例本身证明不了
// 什么。

// TestVerifyEntriesCatchesFlippedPrepaidDirection 证明 VerifyEntries 自己真的
// 能发现「方向写反」，而不是「库里存得进去所以应该平」。
//
// 手工构造四条售卖分录（外部来源的形态，不走 PlanPurchase 构造函数），把
// prepaid 那条改成贷方。这是方向错里最隐蔽的一种：**只断「有分录」看不出
// 来**，因为金额上的绝对值一个没变。
//
// 报的是哪一句取决于校验顺序：`verify` 逐条先跑 checkDirection
// （entry.go:241），所以带符号的金额一进去就被方向表挡下，走不到求和平账
// 那一条。断言因此写「被拒 + 点名 prepaid 只能借」，不写「不平」——
// 那是另一条分支，见下面那条用例。
//
// 重复判据（同一 txn_id 下完全相同的两条）由 verify_test.go 的
// TestVerifyRejectsDuplicateEntries 覆盖，本用例只管方向。
func TestVerifyEntriesCatchesFlippedPrepaidDirection(t *testing.T) {
	txn := newTxnID(t, txnA)
	entries := []Entry{
		{TxnID: txn, Account: PrepaidAccount(9), Asset: AssetCNY,
			Amount: amount(t, "-100"), EntryType: EntryPlanPurchase},
		{TxnID: txn, Account: PaymentGatewayAccount("stub"), Asset: AssetCNY,
			Amount: amount(t, "-100"), EntryType: EntryPlanPurchase},
		{TxnID: txn, Account: UpstreamCostAccount(), Asset: AssetCNY,
			Amount: amount(t, "1.5"), EntryType: EntryPlanPurchase},
		{TxnID: txn, Account: PlatformRevenueAccount(), Asset: AssetCNY,
			Amount: amount(t, "-1.5"), EntryType: EntryPlanPurchase},
	}
	err := VerifyEntries(entries)
	if err == nil {
		t.Fatal("prepaid 写成贷方应当被拒")
	}
	if strings.Contains(err.Error(), "重复") {
		t.Errorf("这一组不构成重复（prepaid 与 payment_gateway 科目不同），报错却是重复：%v", err)
	}
	if !strings.Contains(err.Error(), "prepaid") || !strings.Contains(err.Error(), "借方") {
		t.Errorf("报错要点名 prepaid 只能是借方：%v", err)
	}
}

// TestVerifyEntriesCatchesUnbalancedSaleTxn 证明「四条之和为零」这条不变量
// 在售卖这一档上真的会红。
//
// 与上一条分开：方向错与不平是两条不同的分支，一个用例同时覆盖时只会红在
// 先跑的那条上。这里构造的是**方向全合法**、但少了一对的不平：两条 prepaid
// 借方（金额不同，因此不触发重复判据）配一条通道贷方，和为 +1。
func TestVerifyEntriesCatchesUnbalancedSaleTxn(t *testing.T) {
	txn := newTxnID(t, txnB)
	entries := []Entry{
		{TxnID: txn, Account: PrepaidAccount(9), Asset: AssetCNY,
			Amount: amount(t, "100"), EntryType: EntryPlanPurchase},
		{TxnID: txn, Account: PrepaidAccount(9), Asset: AssetCNY,
			Amount: amount(t, "1"), EntryType: EntryPlanPurchase},
		{TxnID: txn, Account: PaymentGatewayAccount("stub"), Asset: AssetCNY,
			Amount: amount(t, "-100"), EntryType: EntryPlanPurchase},
	}
	err := VerifyEntries(entries)
	if err == nil {
		t.Fatal("和不平的售卖交易应当被拒")
	}
	if !strings.Contains(err.Error(), "不平") {
		t.Errorf("报错要说清是不平，得到: %v", err)
	}
}
