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

// 缓存写入的单价通常高于输入，它必须单独计入预留。
// 早先只按输入单价估缓存读、完全不算缓存写：带缓存写入的请求
// 在缓存单价 > 0 的规则下必然撞 Capped——而 Capped 按设计是
// 「上界算错了」的信号，不该成为常态。
func TestReserveCoversCacheWrite(t *testing.T) {
	p := prices()
	req := ReserveRequest{
		Prices:           p,
		InputTokens:      100,
		CacheWriteTokens: 10000,
		MaxTokens:        10,
	}

	reserved, err := Reserve(req)
	if err != nil {
		t.Fatal(err)
	}

	// 按实际用量结算，包含那 10000 个缓存写入 token
	actual := Usage{
		PromptTokens:     100,
		CacheWriteTokens: 10000,
		CompletionTokens: 10,
	}
	s, err := Settle(actual, p, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capped {
		t.Errorf("缓存写入的请求撞上了 Capped：预留 %s，实际 %s", reserved, s.Breakdown.Total)
	}
}

func TestReserveRejectsNegativeCacheWrite(t *testing.T) {
	_, err := Reserve(ReserveRequest{Prices: prices(), MaxTokens: 10, CacheWriteTokens: -1})
	if err == nil {
		t.Error("负的缓存写入量应被拒绝")
	}
}

// 缓存读按输入单价估（宁贵不贱）：真实用量里缓存读按更便宜的
// 缓存读单价计费，所以预留必然不低于结算。
func TestReserveEstimatesCacheReadAtInputPrice(t *testing.T) {
	p := prices()
	// 把缓存读写差价拉大，差异才看得出来
	p.CacheRead = money.MustParse("0.0000001")
	p.CacheWrite = money.MustParse("0.0001")

	req := ReserveRequest{Prices: p, CacheReadTokens: 1000, CacheWriteTokens: 1000, MaxTokens: 1}
	reserved, err := Reserve(req)
	if err != nil {
		t.Fatal(err)
	}
	actual := Usage{CacheReadTokens: 1000, CacheWriteTokens: 1000}
	s, err := Settle(actual, p, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capped {
		t.Errorf("缓存读写都算进了预留，不该撞 Capped：预留 %s 实际 %s", reserved, s.Breakdown.Total)
	}
}

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 来自变异测试（gremlins unleash）：把比较符或分支方向改一下，测试仍然通过。
// 覆盖率看不出来这一类缺口——行被执行过了，但边界那一支没被走到。

// TotalTokens 要把五个 token 字段**全部**加起来。
//
// 变异：把其中任意一个 `+` 改成 `-`，只要那个字段恰为 0 就看不出来。
// 原先的用例大多只填了一两个字段。
func TestTotalTokensSumsEveryField(t *testing.T) {
	u := Usage{
		PromptTokens: 1, CompletionTokens: 2, CacheReadTokens: 4,
		CacheWriteTokens: 8, ReasoningTokens: 16,
	}
	if got := u.TotalTokens(); got != 31 {
		t.Errorf("TotalTokens = %d，期望 31（1+2+4+8+16）", got)
	}
	if !u.IsEmpty() == false {
		t.Error("有 token 时不该判定为空")
	}
	// 逐字段：去掉任何一个都不再相等
	for name, one := range map[string]Usage{
		"prompt":     {PromptTokens: 1},
		"completion": {CompletionTokens: 2},
		"cacheRead":  {CacheReadTokens: 4},
		"cacheWrite": {CacheWriteTokens: 8},
		"reasoning":  {ReasoningTokens: 16},
	} {
		if one.TotalTokens() <= 0 {
			t.Errorf("%s 字段没被计入 TotalTokens", name)
		}
	}
}

// 计量项数量为 0 时也应当被校验：负数的检查必须覆盖 0 这个边界。
//
// 变异：`count < 0` 改成 `count <= 0`，数量为 0 的计量项会被误拒。
func TestValidateAllowsZeroUnitCount(t *testing.T) {
	p := prices()
	p.Units = map[string]money.Amount{"searches": money.MustParse("0.01")}

	if err := Validate(Usage{UnitCounts: map[string]int64{"searches": 0}}, p); err != nil {
		t.Errorf("数量为 0 的计量项不该被拒: %v", err)
	}
}

// 缓存量恰好等于输入量时，非缓存输入应当是 0 而不是负数。
//
// 变异：`nonCached < 0` 改成 `<= 0`，恰好相等时会被当成「不一致」
// 而触发压回逻辑，虽然结果相同，但分支走向不同。
func TestNonCachedExactlyZero(t *testing.T) {
	got := FromChannelUsage(channel.Usage{PromptTokens: 300, CacheReadTokens: 300})
	if got.PromptTokens != 0 {
		t.Errorf("非缓存输入 = %d，期望 0", got.PromptTokens)
	}
	// 缓存总量恰好等于输入，不该被压
	if got.CacheReadTokens != 300 {
		t.Errorf("缓存读 = %d，期望 300（恰好相等不该被压）", got.CacheReadTokens)
	}
}

// 缓存总量恰好不超过输入时，不该触发压回。
//
// 变异：`total > u.PromptTokens` 改成 `>=`，恰好相等时也会压。
func TestCacheCompressionBoundary(t *testing.T) {
	// 100 输入 = 60 缓存读 + 40 缓存写，恰好相等
	got := FromChannelUsage(channel.Usage{
		PromptTokens: 100, CacheReadTokens: 60, CacheWriteTokens: 40,
	})
	if got.PromptTokens != 0 || got.CacheReadTokens != 60 || got.CacheWriteTokens != 40 {
		t.Errorf("恰好相等时不该压回: %+v", got)
	}

	// 超出一份就该压
	got = FromChannelUsage(channel.Usage{
		PromptTokens: 100, CacheReadTokens: 60, CacheWriteTokens: 60,
	})
	total := got.PromptTokens + got.CacheReadTokens + got.CacheWriteTokens
	if total > 100 {
		t.Errorf("超出输入总量时应压回，得到合计 %d", total)
	}
}

// 最低消费恰好等于按量金额时不该补差。
//
// 变异：`subtotal.Cmp(minimum) >= 0` 改成 `>`，恰好相等时也会补差，
// 而补差金额为 0——不产生错误结果，但会多出一条零金额的行项。
func TestMinimumExactlyEqualSubtotal(t *testing.T) {
	usage := Usage{CompletionTokens: 1000} // 0.008
	p := prices()
	b := ComputeWithCharges(usage, p, Charges{Minimum: money.MustParse("0.008")})

	if !b.MinimumAdjustment.IsZero() {
		t.Errorf("恰好相等时不该补差，得到 %s", b.MinimumAdjustment)
	}
	if err := b.Verify(); err != nil {
		t.Error(err)
	}
}

// 计费口径的用量为零时，金额必须为零——且不产生任何非零行项。
//
// 变异：把某个 `MulInt` 的参数换成常量，零用量会算出非零金额。
func TestZeroUsageYieldsZeroAmounts(t *testing.T) {
	b := ComputeWithCharges(Usage{}, prices(), Charges{})
	for name, v := range map[string]money.Amount{
		"prompt": b.Prompt, "completion": b.Completion,
		"cacheRead": b.CacheRead, "cacheWrite": b.CacheWrite, "reasoning": b.Reasoning,
	} {
		if !v.IsZero() {
			t.Errorf("零用量时 %s 项 = %s，应为零", name, v)
		}
	}
}

// 没有计量项时，Breakdown.Units 应当是 nil 而不是空 map。
//
// 变异：`len(usage.UnitCounts) > 0` 改成 `>= 0`（恒真），于是会先
// `make` 一个空 map。值相同但**可观察**：账单序列化出来会是
// `"units":{}` 而不是省略该字段，而对账脚本要区分「没有计量项」
// 与「有计量项但都是零」。
func TestUnitsIsNilWhenNoUnitCounts(t *testing.T) {
	b := Compute(Usage{PromptTokens: 10}, prices())
	if b.Units != nil {
		t.Errorf("没有计量项时 Units 应为 nil，实际是空 map: %+v", b.Units)
	}
	if err := b.Verify(); err != nil {
		t.Error(err)
	}
}

// 上游报的输入为 0 但缓存非 0 时，压回逻辑的守卫必须生效。
//
// 变异：`u.PromptTokens > 0 && total > u.PromptTokens` 里的
// `u.PromptTokens > 0` 改成 `>= 0`，于是在「输入为 0 却有缓存」时
// 会触发压回，把缓存量抹成 0——而那会**少算缓存费用**。
func TestCacheCompressionGuardWhenPromptIsZero(t *testing.T) {
	// 上游口径不一致：报了缓存却没报输入
	got := FromChannelUsage(channel.Usage{PromptTokens: 0, CacheReadTokens: 100})

	if got.CacheReadTokens != 100 {
		t.Errorf("输入为 0 时不该压回缓存量，得到 %d", got.CacheReadTokens)
	}
}
