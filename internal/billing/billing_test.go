package billing

import (
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
)

func prices() Prices {
	return Prices{
		Prompt:     money.MustParse("0.000002"),  // 每 token 2e-6 元
		Completion: money.MustParse("0.000008"),  // 输出更贵
		CacheRead:  money.MustParse("0.0000002"), // 缓存命中便宜一个数量级
		CacheWrite: money.MustParse("0.0000025"),
		Reasoning:  money.MustParse("0.00001"),
	}
}

func TestComputeBasic(t *testing.T) {
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500}
	b := Compute(usage, prices())

	if err := b.Verify(); err != nil {
		t.Fatalf("明细不自洽: %v", err)
	}
	// 1000 * 2e-6 = 0.002；500 * 8e-6 = 0.004
	if b.Prompt.String() != "0.00200000" {
		t.Errorf("Prompt = %s", b.Prompt)
	}
	if b.Completion.String() != "0.00400000" {
		t.Errorf("Completion = %s", b.Completion)
	}
	if b.Total.String() != "0.00600000" {
		t.Errorf("Total = %s", b.Total)
	}
}

// 逐项保留而不只留总额：对账时要能回答「这一分钱是哪个环节产生的」。
func TestComputeKeepsEveryComponent(t *testing.T) {
	usage := Usage{
		PromptTokens: 100, CompletionTokens: 50,
		CacheReadTokens: 200, CacheWriteTokens: 30, ReasoningTokens: 10,
		UnitCounts: map[string]int64{"searches": 3},
	}
	p := prices()
	p.Units = map[string]money.Amount{"searches": money.MustParse("0.01")}

	b := Compute(usage, p)
	if err := b.Verify(); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]money.Amount{
		"Prompt": b.Prompt, "Completion": b.Completion, "CacheRead": b.CacheRead,
		"CacheWrite": b.CacheWrite, "Reasoning": b.Reasoning, "Units": b.Units["searches"],
	} {
		if v.IsZero() {
			t.Errorf("%s 项为零，应该有金额", name)
		}
	}
}

// 总额必须等于逐项之和。有测试钉它，是因为一旦不等，
// 对账时就会出现一笔永远找不平的差额。
func TestTotalEqualsSumOfParts(t *testing.T) {
	usage := Usage{
		PromptTokens: 333, CompletionTokens: 777,
		CacheReadTokens: 111, CacheWriteTokens: 222, ReasoningTokens: 555,
	}
	b := Compute(usage, prices())

	manual := money.Sum(b.Prompt, b.Completion, b.CacheRead, b.CacheWrite, b.Reasoning)
	if b.Total.Cmp(manual) != 0 {
		t.Errorf("Total %s 与逐项之和 %s 不符", b.Total, manual)
	}
}

// 外部构造的 Breakdown 也要能校验——那正是最需要校验的地方。
func TestVerifyDetectsMismatch(t *testing.T) {
	b := Breakdown{
		Prompt: money.MustParse("1"),
		Total:  money.MustParse("2"), // 与逐项之和不符
	}
	if err := b.Verify(); err == nil {
		t.Fatal("不自洽的明细应被检出")
	}
}

func TestComputeZoomAndNegativeUnits(t *testing.T) {
	b := Compute(Usage{}, prices())
	if !b.Total.IsZero() {
		t.Errorf("零用量应为零金额，得到 %s", b.Total)
	}
	if err := b.Verify(); err != nil {
		t.Error(err)
	}
}

// ── 校验 ────────────────────────────────────────────────────────────────

// 负用量只可能来自解析错误。放过去会算出一笔负数消费，即「上游付钱给用户」。
func TestValidateRejectsNegativeUsage(t *testing.T) {
	usage := Usage{PromptTokens: -1}
	if err := Validate(usage, prices()); err == nil {
		t.Error("负用量应被拒绝")
	}
	if err := Validate(Usage{UnitCounts: map[string]int64{"images": -2}}, prices()); err == nil {
		t.Error("负计量应被拒绝")
	}
}

// 用量里有规则没定价的计量项，不能当成免费——那会让这类用量永远不产生费用，
// 而它也不会报错，只会在对账时表现为「用量对不上金额」。
func TestValidateRejectsMissingUnitPrice(t *testing.T) {
	usage := Usage{UnitCounts: map[string]int64{"searches": 5}}
	err := Validate(usage, prices())
	if err == nil {
		t.Fatal("未定价的计量项应被拒绝")
	}
	if !errors.Is(err, ErrMissingPrice) {
		t.Errorf("期望 ErrMissingPrice，得到 %v", err)
	}
}

func TestValidateAcceptsPricedUnits(t *testing.T) {
	p := prices()
	p.Units = map[string]money.Amount{"searches": money.MustParse("0.01")}
	if err := Validate(Usage{UnitCounts: map[string]int64{"searches": 5}}, p); err != nil {
		t.Errorf("已定价的计量项不该被拒: %v", err)
	}
}

// ── 预留 ────────────────────────────────────────────────────────────────

func TestReserveCoversWorstCase(t *testing.T) {
	r, err := Reserve(ReserveRequest{
		Prices:      prices(),
		InputTokens: 1000,
		MaxTokens:   4000,
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 输入 1000 * 2e-6 = 0.002
	// 输出 4000 × max(输出价 8e-6, 推理价 1e-5) = 4000 * 1e-5 = 0.04
	// 推理单价更高，预留按贵的算
	want := money.MustParse("0.042")
	if r.Cmp(want) != 0 {
		t.Errorf("预留 = %s，期望 %s", r, want)
	}
}

// 预留必须不小于任何可能的实际金额。这条不变量是「余额不为负」的前提。
func TestReserveNeverBelowActual(t *testing.T) {
	req := ReserveRequest{Prices: prices(), InputTokens: 1000, MaxTokens: 1000}
	reserved, err := Reserve(req)
	if err != nil {
		t.Fatal(err)
	}

	// 枚举一批可能的实际用量，全部应落在预留之内
	for _, out := range []int64{0, 1, 100, 500, 999, 1000} {
		for _, in := range []int64{0, 500, 1000} {
			actual := Usage{PromptTokens: in, CompletionTokens: out}
			s, err := Settle(actual, prices(), reserved)
			if err != nil {
				t.Fatal(err)
			}
			if s.Capped {
				t.Errorf("实际(%d in, %d out) 超出了预留 %s", in, out, reserved)
			}
		}
	}
}

// 没有 max_tokens 就算不出上界。用典型值兜底会让结算可能超过预留，
// 从而破坏「余额不为负」——那正是预留机制要避免的。
func TestReserveRequiresMaxTokens(t *testing.T) {
	_, err := Reserve(ReserveRequest{Prices: prices(), InputTokens: 100, MaxTokens: 0})
	if err == nil {
		t.Fatal("缺少 max_tokens 应报错")
	}
	if !errors.Is(err, ErrNoUpperBound) {
		t.Errorf("期望 ErrNoUpperBound，得到 %v", err)
	}
}

func TestReserveRejectsNegativeInputs(t *testing.T) {
	cases := []ReserveRequest{
		{Prices: prices(), InputTokens: -1, MaxTokens: 100},
		{Prices: prices(), CacheReadTokens: -1, MaxTokens: 100},
		{Prices: prices(), InputTokens: 1, MaxTokens: 100, Charges: Charges{Minimum: money.MustParse("-1")}},
	}
	for i, req := range cases {
		if _, err := Reserve(req); err == nil {
			t.Errorf("第 %d 个请求应被拒绝", i)
		}
	}
}

func TestReserveRejectsNegativePrices(t *testing.T) {
	p := prices()
	p.Prompt = money.MustParse("-0.001")
	if _, err := Reserve(ReserveRequest{Prices: p, InputTokens: 1, MaxTokens: 100}); err == nil {
		t.Error("负单价应被拒绝")
	}
}

// 保底金额（固定费用、最低消费）要被算进去。
func TestReserveHonorsMinimum(t *testing.T) {
	base := ReserveRequest{Prices: prices(), InputTokens: 1, MaxTokens: 1}
	plain, err := Reserve(base)
	if err != nil {
		t.Fatal(err)
	}

	base.Charges = Charges{Minimum: money.MustParse("10")}
	withMin, err := Reserve(base)
	if err != nil {
		t.Fatal(err)
	}
	if withMin.Cmp(money.MustParse("10")) != 0 {
		t.Errorf("保底应生效，得到 %s", withMin)
	}
	if plain.Cmp(withMin) >= 0 {
		t.Error("保底应高于按量算出的金额")
	}
}

// 推理单价通常更高，预留按更贵的算。
func TestReserveUsesHigherOfCompletionAndReasoning(t *testing.T) {
	p := prices()
	// 让推理比输出便宜，预留应当用输出的价
	p.Reasoning = money.MustParse("0.000001")
	r, err := Reserve(ReserveRequest{Prices: p, MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	// 1000 * 8e-6 = 0.008（用输出价，而不是推理价 0.001）
	if r.Cmp(money.MustParse("0.008")) != 0 {
		t.Errorf("预留 = %s，期望 0.008", r)
	}
}

// 非 token 计量的数量在请求前未知，留一份的量兜底。
func TestReserveIncludesOneOfEachUnit(t *testing.T) {
	p := prices()
	p.Units = map[string]money.Amount{"images": money.MustParse("0.5")}
	r, err := Reserve(ReserveRequest{Prices: p, MaxTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Cmp(money.MustParse("0.50000800")) < 0 {
		t.Errorf("预留应含一份计量项，得到 %s", r)
	}
}

// ── 结算 ────────────────────────────────────────────────────────────────

func TestSettleReturnsDifference(t *testing.T) {
	reserved := money.MustParse("1")
	actual := Usage{PromptTokens: 1000} // 0.002

	s, err := Settle(actual, prices(), reserved)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if s.Actual.Cmp(money.MustParse("0.002")) != 0 {
		t.Errorf("Actual = %s", s.Actual)
	}
	if s.Delta.Cmp(money.MustParse("-0.998")) != 0 {
		t.Errorf("Delta = %s，期望 -0.998（要退回）", s.Delta)
	}
	if s.Capped {
		t.Error("未超预留不该标记 Capped")
	}
}

// 预留是上界，正常不会超。真超了说明上界算错了或上游不守信，
// 那是一条需要查的信号。
func TestSettleCapsOvershoot(t *testing.T) {
	reserved := money.MustParse("0.001")
	actual := Usage{CompletionTokens: 10000} // 远超预留

	s, err := Settle(actual, prices(), reserved)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if !s.Capped {
		t.Fatal("超过预留应标记 Capped")
	}
	if s.Actual.Cmp(reserved) != 0 {
		t.Errorf("超限时 Actual 应截到预留，得到 %s", s.Actual)
	}
	if !s.Delta.IsZero() {
		t.Errorf("截住之后 Delta 应为零，得到 %s", s.Delta)
	}
	// 明细仍要保留真实用量算出的金额：排查时要知道超了多少
	if s.Breakdown.Total.Cmp(reserved) <= 0 {
		t.Error("明细应保留未截断的金额，供排查超了多少")
	}
}

func TestSettleExactlyAtReserve(t *testing.T) {
	usage := Usage{CompletionTokens: 1000} // 0.008
	reserved := money.MustParse("0.008")

	s, err := Settle(usage, prices(), reserved)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capped {
		t.Error("正好相等不该标记 Capped")
	}
	if !s.Delta.IsZero() {
		t.Errorf("正好相等 Delta 应为零，得到 %s", s.Delta)
	}
}

func TestSettleRejectsInvalidUsage(t *testing.T) {
	if _, err := Settle(Usage{PromptTokens: -1}, prices(), money.One); err == nil {
		t.Error("负用量应被拒绝")
	}
}

func TestSettleUncappedAllowsExceeding(t *testing.T) {
	usage := Usage{CompletionTokens: 1000}
	s, err := SettleUncapped(usage, prices())
	if err != nil {
		t.Fatal(err)
	}
	if s.Capped {
		t.Error("无上界结算不该标记 Capped")
	}
	if s.Actual.Cmp(money.MustParse("0.008")) != 0 {
		t.Errorf("Actual = %s", s.Actual)
	}
	if err := s.Breakdown.Verify(); err != nil {
		t.Error(err)
	}
}

// ── 协议口径转换 ────────────────────────────────────────────────────────

// channel.Usage.PromptTokens 含缓存（OpenAI 口径），而计费时缓存与非缓存
// 单价不同，必须拆开。
func TestFromChannelUsageSplitsCache(t *testing.T) {
	got := FromChannelUsage(channel.Usage{PromptTokens: 1000, CacheReadTokens: 300, CacheWriteTokens: 100})
	if got.PromptTokens != 600 {
		t.Errorf("非缓存输入 = %d，期望 600", got.PromptTokens)
	}
	if got.CacheReadTokens != 300 || got.CacheWriteTokens != 100 {
		t.Errorf("缓存明细 = %+v", got)
	}
}

// 缓存量大于输入量：上游口径不一致（有的把缓存算在输入里，有的不算）。
// 取 0 而不是负数，否则会算出一笔负数金额。
func TestFromChannelUsageClampsInconsistentInput(t *testing.T) {
	got := FromChannelUsage(channel.Usage{PromptTokens: 100, CacheReadTokens: 500})
	if got.PromptTokens < 0 {
		t.Errorf("非缓存输入不该为负: %d", got.PromptTokens)
	}
	if got.PromptTokens+got.CacheReadTokens+got.CacheWriteTokens > 100 {
		t.Errorf("缓存量不该超过输入总量: %+v", got)
	}
}

func TestFromChannelUsageKeepsOtherFields(t *testing.T) {
	got := FromChannelUsage(channel.Usage{CompletionTokens: 50, ReasoningTokens: 20, UnitCounts: map[string]int64{"searches": 2}})
	if got.CompletionTokens != 50 || got.ReasoningTokens != 20 {
		t.Errorf("输出与推理 token 应原样保留: %+v", got)
	}
	if got.UnitCounts["searches"] != 2 {
		t.Errorf("计量项丢了: %+v", got.UnitCounts)
	}
}

func TestUsageHelpers(t *testing.T) {
	if !(Usage{}).IsEmpty() {
		t.Error("零用量应判定为空")
	}
	if (Usage{PromptTokens: 1}).IsEmpty() {
		t.Error("有 token 不该判定为空")
	}
	if (Usage{UnitCounts: map[string]int64{"a": 1}}).IsEmpty() {
		t.Error("有计量项不该判定为空")
	}
	if got := (Usage{PromptTokens: 1, CompletionTokens: 2}).TotalTokens(); got != 3 {
		t.Errorf("TotalTokens = %d", got)
	}
}

// ── 一次性费用 ──────────────────────────────────────────────────────────

// 保底在预留与结算**两边**都要生效。
//
// 早先只有预留算了保底、结算没算：一条「最低消费 1 元」的规则会让
// 预留是 1 元、结算是实际用量（比如 0.002 元），差额全额退回——
// 保底永远收不到，而且不会报错，只是长期少收钱。
func TestMinimumAppliesToBothReserveAndSettle(t *testing.T) {
	charges := Charges{Minimum: money.MustParse("1")}
	p := prices()

	reserved, err := Reserve(ReserveRequest{
		Prices: p, InputTokens: 1, MaxTokens: 1, Charges: charges,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved.Cmp(money.MustParse("1")) != 0 {
		t.Fatalf("预留应等于最低消费，得到 %s", reserved)
	}

	// 用量很小，按量算只有 0.000002 元
	actual := Usage{PromptTokens: 1}
	s, err := SettleWithCharges(actual, p, charges, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if s.Actual.Cmp(money.MustParse("1")) != 0 {
		t.Errorf("结算应补足到最低消费 1 元，得到 %s", s.Actual)
	}
	if !s.Delta.IsZero() {
		t.Errorf("补足之后 Delta 应为零，得到 %s", s.Delta)
	}
	if s.Capped {
		t.Error("不该标记 Capped：预留算了保底，结算也在同一上界内")
	}
}

// 保底补差要记成一条显式的调整项，而不是直接抬高 Total——
// 那样 Total 仍等于所有行项之和，Verify() 不必为例外开口子。
func TestMinimumShowsAsExplicitLine(t *testing.T) {
	b := ComputeWithCharges(Usage{PromptTokens: 1}, prices(), Charges{Minimum: money.MustParse("1")})

	if b.MinimumAdjustment.IsZero() {
		t.Fatal("补差应作为独立行项")
	}
	if err := b.Verify(); err != nil {
		t.Errorf("加了补差之后明细仍应自洽: %v", err)
	}
	// 按量部分 + 补差 = 最低消费
	if b.Subtotal().Add(b.MinimumAdjustment).Cmp(money.MustParse("1")) != 0 {
		t.Errorf("按量 %s + 补差 %s 应等于 1", b.Subtotal(), b.MinimumAdjustment)
	}
}

// 用量已经超过最低消费时不该再补。
func TestMinimumNotAppliedAboveThreshold(t *testing.T) {
	b := ComputeWithCharges(Usage{CompletionTokens: 1000}, prices(), Charges{Minimum: money.MustParse("0.001")})
	if !b.MinimumAdjustment.IsZero() {
		t.Errorf("超过保底时不该补差，得到 %s", b.MinimumAdjustment)
	}
}

// 固定费用与用量无关，且要作为独立一行。
func TestFixedChargeAddedOnce(t *testing.T) {
	p := prices()
	charges := Charges{Fixed: money.MustParse("0.5")}

	small := ComputeWithCharges(Usage{PromptTokens: 1}, p, charges)
	large := ComputeWithCharges(Usage{PromptTokens: 100000}, p, charges)

	if small.Fixed.Cmp(large.Fixed) != 0 {
		t.Error("固定费用与用量无关，两次应相同")
	}
	// 固定费只加一次：大用量的金额 = 按量 + 固定费
	want := large.Subtotal().Add(charges.Fixed)
	if large.Total.Cmp(want) != 0 {
		t.Errorf("Total = %s，期望 %s（按量 + 固定费一次）", large.Total, want)
	}
}

// 最低消费要拿**按量部分**比较，不是拿 Total 比较——
// 固定费会把补差抵消掉，「固定费 0.5 元、最低消费 1 元」就永远收不到补差。
func TestMinimumComparedAgainstSubtotalNotTotal(t *testing.T) {
	charges := Charges{Fixed: money.MustParse("0.5"), Minimum: money.MustParse("1")}
	b := ComputeWithCharges(Usage{PromptTokens: 1}, prices(), charges)

	// 按量部分极小，补差应为 1 - 按量，而不是 1 - (按量 + 0.5)
	expected := money.MustParse("1").Sub(b.Subtotal())
	if b.MinimumAdjustment.Cmp(expected) != 0 {
		t.Errorf("补差 = %s，期望 %s", b.MinimumAdjustment, expected)
	}
	if b.Total.Cmp(money.MustParse("1").Add(charges.Fixed)) != 0 {
		t.Errorf("总额 = %s，期望 1 + 固定费", b.Total)
	}
}

// 预留是上界：固定费与保底都算进去之后，结算仍不该超。
func TestReserveIncludesChargesSoSettleNeverCaps(t *testing.T) {
	p := prices()
	charges := Charges{Fixed: money.MustParse("0.5"), Minimum: money.MustParse("1")}

	reserved, err := Reserve(ReserveRequest{
		Prices: p, InputTokens: 100, MaxTokens: 200, Charges: charges,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []int64{0, 1, 100, 200} {
		s, err := SettleWithCharges(Usage{PromptTokens: 100, CompletionTokens: out}, p, charges, reserved)
		if err != nil {
			t.Fatal(err)
		}
		if s.Capped {
			t.Errorf("输出 %d 时撞上了 Capped，说明预留没算全一次性费用", out)
		}
	}
}

func TestChargesRejectNegative(t *testing.T) {
	if err := (Charges{Fixed: money.MustParse("-1")}).Validate(); err == nil {
		t.Error("负固定费应被拒绝")
	}
	if err := (Charges{Minimum: money.MustParse("-1")}).Validate(); err == nil {
		t.Error("负最低消费应被拒绝")
	}
	if !(Charges{}).IsZero() {
		t.Error("零值应为空")
	}
}

// 不传一次性费用时，行为必须与之前完全一致。
func TestZeroChargesPreserveBehavior(t *testing.T) {
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500}
	a := Compute(usage, prices())
	b := ComputeWithCharges(usage, prices(), Charges{})
	if a.Total.Cmp(b.Total) != 0 {
		t.Errorf("零费用应不改变结果: %s vs %s", a.Total, b.Total)
	}
	if !b.Fixed.IsZero() || !b.MinimumAdjustment.IsZero() {
		t.Error("零费用不该产生行项")
	}
}
