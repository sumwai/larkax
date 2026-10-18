package billing

import (
	"errors"
	"fmt"
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
		// 五个 token 单价都「写过」——Validate 认的是这个，不是单价是否为 0。
		Declared: map[string]bool{
			"prompt": true, "completion": true, "cache_read": true,
			"cache_write": true, "reasoning": true,
		},
	}
}

func TestComputeBasic(t *testing.T) {
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500}
	b := ComputeWithCharges(usage, prices(), Charges{})

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

	b := ComputeWithCharges(usage, p, Charges{})
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
	b := ComputeWithCharges(usage, prices(), Charges{})

	manual := money.Sum(b.Prompt, b.Completion, b.CacheRead, b.CacheWrite, b.Reasoning)
	if b.Total.Cmp(manual) != 0 {
		t.Errorf("Total %s 与逐项之和 %s 不符", b.Total, manual)
	}
}

// TestSumCheckedHandlesManyUnits 验证计量项多的时候求和照样算得对。
//
// 行项的容量按 8 + 计量项个数预留。容量只影响分配：写小了顶多多扩容一次，
// 金额不会因此变样；而方向写反时那个值是负的，make 会先 panic，
// 连一个错金额都拿不到。这里给 9 个计量项——那是那个预留刚好不够用的第一格。
func TestSumCheckedHandlesManyUnits(t *testing.T) {
	units := make(map[string]money.Amount, 9)
	for i := 0; i < 9; i++ {
		units[fmt.Sprintf("u%d", i)] = money.MustParse("0.01")
	}
	b := Breakdown{
		Prompt: money.MustParse("0.5"),
		Units:  units,
		Fixed:  money.MustParse("0.2"),
	}

	got, ok := b.sumChecked()
	if !ok {
		t.Fatal("这些金额都放得进 int64，不该报放不下")
	}
	// 0.5 + 9 × 0.01 + 0.2 = 0.79
	if want := money.MustParse("0.79"); got.Cmp(want) != 0 {
		t.Errorf("求和 = %s，期望 %s", got, want)
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
	b := ComputeWithCharges(Usage{}, prices(), Charges{})
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

	// 逐项判，而不是判五项之和：单项为负、被另一项的正值抵消时，
	// 求和与「完全没用 token」分不出来（-100 + 100 = 0）。
	// 五项逐个来一遍是为了钉住「每一项都查了」——只查其中一两项时，
	// 其余几行会挂。
	for name, u := range map[string]Usage{
		"prompt":      {PromptTokens: -100, CompletionTokens: 100},
		"completion":  {CompletionTokens: -100, PromptTokens: 100},
		"cache_read":  {CacheReadTokens: -100, PromptTokens: 100},
		"cache_write": {CacheWriteTokens: -100, PromptTokens: 100},
		"reasoning":   {ReasoningTokens: -100, PromptTokens: 100},
	} {
		if err := Validate(u, prices()); err == nil {
			t.Errorf("%s 为负、被别的项抵消成非负，应被拒绝", name)
		}
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

// token 单价漏写同样不能当成免费。改了计费口径之后推理与输出互斥，规则没写
// reasoning 时它按 0 元计——那是一笔没人会来问的少收。
func TestValidateRejectsMissingTokenPrice(t *testing.T) {
	p := prices()
	delete(p.Declared, "reasoning")
	err := Validate(Usage{ReasoningTokens: 40}, p)
	if !errors.Is(err, ErrMissingPrice) {
		t.Fatalf("漏写 reasoning 单价应被拒绝，得到 %v", err)
	}
}

// 反过来：显式写 0 是「这一项免费」，是一个正当的配置，不该被拒。
// 这一条与上一条一起把「写了 0」和「没写」钉开——两者的单价都是 0，
// 差别只在规则里有没有那个键。
func TestValidateAcceptsExplicitZeroTokenPrice(t *testing.T) {
	p := prices()
	p.Reasoning = money.Zero
	if err := Validate(Usage{ReasoningTokens: 40}, p); err != nil {
		t.Errorf("显式写 0 的单价不该被拒: %v", err)
	}
}

// 没有用量的那一项不要求单价：一条只按计量项计费的规则不必写 token 单价。
func TestValidateIgnoresUnusedTokenSlots(t *testing.T) {
	p := Prices{Declared: map[string]bool{}}
	if err := Validate(Usage{}, p); err != nil {
		t.Errorf("零用量不该要求任何单价: %v", err)
	}
}

// TestTokenSlotsPinFieldMapping 逐个钉住槽位与 Usage/Prices 字段的对应关系。
//
// TokenSlots 是 Validate 与 internal/billing/expr 共同的事实来源。表里写错
// 一行不会编译失败——把 cache_write 的计数函数写成 CacheReadTokens，两边都
// 还能跑，只是缓存写从此按缓存读判负、判「有没有写单价」。所以每一项都用
// **各不相同的值**钉一遍：读错、写错、读和写错到同一处，都会让某一行挂。
func TestTokenSlotsPinFieldMapping(t *testing.T) {
	usage := Usage{
		PromptTokens:     11,
		CompletionTokens: 22,
		CacheReadTokens:  33,
		CacheWriteTokens: 44,
		ReasoningTokens:  55,
	}
	wantCount := map[string]int64{
		"prompt": 11, "completion": 22, "cache_read": 33,
		"cache_write": 44, "reasoning": 55,
	}
	if len(TokenSlots) != len(wantCount) {
		t.Fatalf("槽位数 = %d，期望 %d；加槽位时下面这张表也要一起更新", len(TokenSlots), len(wantCount))
	}

	seen := make(map[string]bool, len(TokenSlots))
	for i, slot := range TokenSlots {
		want, ok := wantCount[slot.Name]
		if !ok {
			t.Fatalf("未知的槽位名 %q", slot.Name)
		}
		seen[slot.Name] = true
		if got := slot.Count(usage); got != want {
			t.Errorf("%s 的计数 = %d，期望 %d", slot.Name, got, want)
		}

		// 每个槽位给不同的单价，写进去再读回来：写错字段、读错字段都会挂。
		price := money.MustParse(fmt.Sprintf("0.%08d", i+1))
		var p Prices
		slot.SetPrice(&p, price)
		if got := slot.Price(p); got.Cmp(price) != 0 {
			t.Errorf("%s 的单价写回 = %s，期望 %s", slot.Name, got, price)
		}
	}
	if len(seen) != len(TokenSlots) {
		t.Errorf("槽位名有重复: %d 个名字，%d 条清单", len(seen), len(TokenSlots))
	}
}

// TestEveryTokenSlotIsCharged 逐槽确认「清单里有的，算术里也真的算了」。
//
// 它防的是另一种漏改：TokenSlots 加了一项、Prices 也写进去了，而
// ComputeWithCharges 里少了一行乘法。那样 Validate 照样通过、单价也照样
// 出现在求值结果里，只是这一项永远收不到钱。逐槽单独造一份用量，
// 金额对不上就会挂。
func TestEveryTokenSlotIsCharged(t *testing.T) {
	for _, slot := range TokenSlots {
		var usage Usage
		slot.SetCount(&usage, 1000)

		var p Prices
		price := money.MustParse("0.000001")
		slot.SetPrice(&p, price)
		p.Declared = map[string]bool{slot.Name: true}

		if err := Validate(usage, p); err != nil {
			t.Errorf("%s: %v", slot.Name, err)
		}

		b := ComputeWithCharges(usage, p, Charges{})
		want := money.MustParse("0.001")
		if b.Total.Cmp(want) != 0 {
			t.Errorf("%s 的金额 = %s，期望 %s（槽位进了清单却没进 ComputeWithCharges？）",
				slot.Name, b.Total, want)
		}
	}
}

// ── 预留 ────────────────────────────────────────────────────────────────

func TestReserveCoversWorstCase(t *testing.T) {
	r, err := Reserve(ReserveRequest{
		Prices:       prices(),
		InputTokens:  1000,
		OutputTokens: 4000,
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 输入 1000 × max(输入 2e-6, 缓存读 2e-7, 缓存写 2.5e-6) = 1000 * 2.5e-6 = 0.0025
	// 输出 4000 × max(输出 8e-6, 推理 1e-5) = 4000 * 1e-5 = 0.04
	//
	// 两侧都按最贵的单价估：请求前不知道结算会把输入拆成
	// prompt / cache_read / cache_write 各多少，也不知道输出会不会被判成推理，
	// 所以取最大值——那是与拆法无关的上界。
	want := money.MustParse("0.0425")
	if r.Cmp(want) != 0 {
		t.Errorf("预留 = %s，期望 %s", r, want)
	}
}

// 预留必须不小于任何可能的实际金额。这条不变量是「余额不为负」的前提。
func TestReserveNeverBelowActual(t *testing.T) {
	req := ReserveRequest{Prices: prices(), InputTokens: 1000, OutputTokens: 1000}
	reserved, err := Reserve(req)
	if err != nil {
		t.Fatal(err)
	}

	// 枚举一批可能的实际用量，全部应落在预留之内
	for _, out := range []int64{0, 1, 100, 500, 999, 1000} {
		for _, in := range []int64{0, 500, 1000} {
			actual := Usage{PromptTokens: in, CompletionTokens: out}
			s, err := SettleWithCharges(actual, prices(), Charges{}, reserved)
			if err != nil {
				t.Fatal(err)
			}
			if s.Capped {
				t.Errorf("实际(%d in, %d out) 超出了预留 %s", in, out, reserved)
			}
		}
	}
}

// 没有「要冻多少输出 token」就算不出上界。用典型值兜底会让结算可能超过预留，
// 从而破坏「余额不为负」——那正是预留机制要避免的。
//
// 名字说的是 OutputTokens 而不是 max_tokens：那一个量在非流式那条路上**就是**
// 声明的 max_tokens，在流式那条路上是第一段（见 ReserveRequest.OutputTokens）。
// 用例钉的是「这个字段必须为正」，与它是怎么来的无关。
func TestReserveRequiresOutputTokens(t *testing.T) {
	_, err := Reserve(ReserveRequest{Prices: prices(), InputTokens: 100, OutputTokens: 0})
	if err == nil {
		t.Fatal("缺少 max_tokens 应报错")
	}
	if !errors.Is(err, ErrNoUpperBound) {
		t.Errorf("期望 ErrNoUpperBound，得到 %v", err)
	}
}

func TestReserveRejectsNegativeInputs(t *testing.T) {
	cases := []ReserveRequest{
		{Prices: prices(), InputTokens: -1, OutputTokens: 100},
		{Prices: prices(), CacheReadTokens: -1, OutputTokens: 100},
		{Prices: prices(), InputTokens: 1, OutputTokens: 100, Charges: Charges{Minimum: money.MustParse("-1")}},
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
	if _, err := Reserve(ReserveRequest{Prices: p, InputTokens: 1, OutputTokens: 100}); err == nil {
		t.Error("负单价应被拒绝")
	}
}

// 保底金额（固定费用、最低消费）要被算进去。
func TestReserveHonorsMinimum(t *testing.T) {
	base := ReserveRequest{Prices: prices(), InputTokens: 1, OutputTokens: 1}
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
	r, err := Reserve(ReserveRequest{Prices: p, OutputTokens: 1000})
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
	r, err := Reserve(ReserveRequest{Prices: p, OutputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Cmp(money.MustParse("0.50000800")) < 0 {
		t.Errorf("预留应含一份计量项，得到 %s", r)
	}
}

// ── 补冻（流式递进冻结用） ──────────────────────────────────────────────

// 补冻的单价必须与预留的**输出侧**逐字相同。
//
// 它是「补冻与结算同源」这条口径里最容易走偏的一环：结算按实际用量把输出拆成
// completion 与 reasoning 两个桶，而补冻只知道自己又产出了多少个 token、不知道
// 上游会把它算进哪个桶——所以它只能取两者里贵的那个。取错（例如固定用
// completion）时，一条按推理计价的流会在补冻上少冻一截，差额由平台吃掉，
// 而账目仍然是平的、对账看不出来。
func TestOutputPriceIsTheHigherOfCompletionAndReasoning(t *testing.T) {
	p := prices() // Completion 8e-6，Reasoning 1e-5
	if got := OutputPrice(p); got.Cmp(money.MustParse("0.00001")) != 0 {
		t.Errorf("OutputPrice = %s，期望较高的推理价 0.00001", got)
	}

	// 反过来：推理比输出便宜时取输出价。两个方向都钉，免得实现写成「总是取
	// Reasoning」——那种写法在上面那一条上是绿的。
	p.Reasoning = money.MustParse("0.000001")
	if got := OutputPrice(p); got.Cmp(money.MustParse("0.000008")) != 0 {
		t.Errorf("OutputPrice = %s，期望较高的输出价 0.000008", got)
	}
}

// 补冻金额 = 输出侧单价 × 又产出的 token 数，逐项与预留里那一段同口径。
func TestReserveOutputMatchesReserveOutputSide(t *testing.T) {
	p := prices()
	// 一次冻满 1000 个：输入侧为 0，所以整个金额就是输出侧那一段。
	whole, err := Reserve(ReserveRequest{Prices: p, OutputTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	// 分成两段冻：3:7。两段之和必须**恰好**等于整段——不等就说明补冻那一侧
	// 用了另一份价（多了或少了），而那正是「同源」要防的。
	a, err := ReserveOutput(p, 300)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReserveOutput(p, 700)
	if err != nil {
		t.Fatal(err)
	}
	if sum := a.Add(b); sum.Cmp(whole) != 0 {
		t.Errorf("两段补冻之和 %s ≠ 整段 %s（补冻与预留用的不是同一份价？）", sum, whole)
	}
}

// 补冻只算输出侧那一项：固定费、最低消费、非 token 计量与输入侧都不进来。
//
// 这一条钉的是「按段重复加」这个错法。那些量一次请求只发生一次，初始预留里
// 已经各算过一份；补冻时若跟着加一遍，冻结额会随段数一路长上去——而它长到
// 超过用户的余额时，那条流会被自己冻死（中断），表现得像「用户没钱了」。
func TestReserveOutputOnlyCountsTheOutputSide(t *testing.T) {
	p := prices()
	p.Units = map[string]money.Amount{"images": money.MustParse("1")}
	got, err := ReserveOutput(p, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 100 × max(8e-6, 1e-5) = 0.001。与 Units 里那一份、与任何固定费都无关。
	if want := money.MustParse("0.001"); got.Cmp(want) != 0 {
		t.Errorf("补冻 = %s，期望 %s（只该有输出侧那一项）", got, want)
	}
}

// 单价乘 token 数溢出时报错，而不是绕回一个看起来正常的数。
//
// 这一条与 Reserve 的溢出用例同一个理由：金额是 int64 最小单位，绕回之后
// 可能是一个**负数**（冻负数的钱）或者一个小得离谱的正数（等于没冻）。
func TestReserveOutputRejectsOverflow(t *testing.T) {
	p := prices()
	p.Completion = money.MustParse("1000000") // 1e6 元/token
	if _, err := ReserveOutput(p, 1<<40); err == nil {
		t.Error("溢出应报错")
	}
}

// TestReserveRejectsClampedTotal 验证求和被夹住时报错，而不是交回那个夹住的数。
//
// 夹住的值从返回值上看不出来：它是 int64 的边界，形如一个正常金额。而这个
// 函数交出去的是**预留上界**——一个夹住的、偏小的上界会让结算越过它，
// 撞上 Capped，差额静默由平台承担。所以这里必须是错误。
//
// 与上面几处溢出的取数方式不同：那些是某一项相乘放不下，这一处各项都放得下，
// 夹住发生在**最低消费与固定费相加**那一步。
func TestReserveRejectsClampedTotal(t *testing.T) {
	// 最低消费取 money 能表示的最大值，再加一个最小单位：
	// 真实和越过 int64，而相加那一步只会夹到边界。
	req := ReserveRequest{
		Prices:       prices(),
		InputTokens:  1,
		OutputTokens: 1,
		Charges: Charges{
			Minimum: money.MustParse("92233720368.54775807"),
			Fixed:   money.MustParse("0.00000001"),
		},
	}
	got, err := Reserve(req)
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("总额夹住时应当报 money.ErrOverflow，实际 err=%v 预留=%s"+
			"（夹住的边界值不等于算得出来的金额）", err, got)
	}
}

// ── 结算 ────────────────────────────────────────────────────────────────

func TestSettleReturnsDifference(t *testing.T) {
	reserved := money.MustParse("1")
	actual := Usage{PromptTokens: 1000} // 0.002

	s, err := SettleWithCharges(actual, prices(), Charges{}, reserved)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if s.Actual.Cmp(money.MustParse("0.002")) != 0 {
		t.Errorf("Actual = %s", s.Actual)
	}
	// 退回的差额是 Reserved - Actual。它不在结构体里单存一份（见 Settlement
	// 的注释），要用就当场减。
	if refund := s.Reserved.Sub(s.Actual); refund.Cmp(money.MustParse("0.998")) != 0 {
		t.Errorf("退回差额 = %s，期望 0.998", refund)
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

	s, err := SettleWithCharges(actual, prices(), Charges{}, reserved)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if !s.Capped {
		t.Fatal("超过预留应标记 Capped")
	}
	if s.Actual.Cmp(reserved) != 0 {
		t.Errorf("超限时 Actual 应截到预留，得到 %s", s.Actual)
	}
	// 明细仍要保留真实用量算出的金额：排查时要知道超了多少
	if s.Breakdown.Total.Cmp(reserved) <= 0 {
		t.Error("明细应保留未截断的金额，供排查超了多少")
	}
}

func TestSettleExactlyAtReserve(t *testing.T) {
	usage := Usage{CompletionTokens: 1000} // 0.008
	reserved := money.MustParse("0.008")

	s, err := SettleWithCharges(usage, prices(), Charges{}, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capped {
		t.Error("正好相等不该标记 Capped")
	}
	// 边界是 `>` 而不是 `>=`：正好相等时收的钱就是预留额，不多不少。
	if s.Actual.Cmp(money.MustParse("0.008")) != 0 {
		t.Errorf("正好相等时实收应等于预留，得到 %s", s.Actual)
	}
}

func TestSettleRejectsInvalidUsage(t *testing.T) {
	if _, err := SettleWithCharges(Usage{PromptTokens: -1}, prices(), Charges{}, money.One); err == nil {
		t.Error("负用量应被拒绝")
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
	// 没声明「已含」时两项原样保留（零值即「独立报」，见 channel.Usage 的注释）。
	got := FromChannelUsage(channel.Usage{CompletionTokens: 50, ReasoningTokens: 20, UnitCounts: map[string]int64{"searches": 2}})
	if got.CompletionTokens != 50 || got.ReasoningTokens != 20 {
		t.Errorf("输出与推理 token 应原样保留: %+v", got)
	}
	if got.UnitCounts["searches"] != 2 {
		t.Errorf("计量项丢了: %+v", got.UnitCounts)
	}
}

// 声明「推理已含在输出里」时，同一批 token 不能既按输出价又按推理价收一遍。
//
// 上游的 completion_tokens 按 OpenAI 定义含 reasoning_tokens，而规则可以给
// 推理单独定价。不减掉的话 completion 与 reasoning 两个变量各自乘一次单价，
// 指的是同一批 token——账单只是偏高，对账发现不了。
func TestFromChannelUsageDoesNotChargeReasoningTwice(t *testing.T) {
	ch := channel.Usage{
		CompletionTokens:            100,
		ReasoningTokens:             40,
		CompletionIncludesReasoning: true,
	}
	got := FromChannelUsage(ch)
	if got.CompletionTokens != 60 {
		t.Errorf("输出 = %d，期望 60（100 里含 40 推理）", got.CompletionTokens)
	}
	if got.ReasoningTokens != 40 {
		t.Errorf("推理 = %d，期望 40", got.ReasoningTokens)
	}

	// 60*8e-6 + 40*1e-5 = 0.00088；收两遍会是 100*8e-6 + 40*1e-5 = 0.0012
	total := ComputeWithCharges(got, prices(), Charges{}).Total
	if total.Cmp(money.MustParse("0.00088")) != 0 {
		t.Errorf("总额 = %s，期望 0.00088", total)
	}
	if total.Cmp(money.MustParse("0.0012")) == 0 {
		t.Error("推理被收了两次：总额等于「输出整段 + 推理」")
	}
}

// 声明「推理是单独报的」时，两项各算各的——那才是真的该相加。
//
// 同一组数字（输出 100、推理 40）在两种声明下金额必须不同，否则说明
// 声明字段根本没被读。
func TestFromChannelUsageChargesSeparateReasoningSeparately(t *testing.T) {
	ch := channel.Usage{CompletionTokens: 100, ReasoningTokens: 40}
	got := FromChannelUsage(ch)
	if got.CompletionTokens != 100 {
		t.Errorf("独立报时输出不该被减: %d", got.CompletionTokens)
	}
	if got.ReasoningTokens != 40 {
		t.Errorf("推理 = %d，期望 40", got.ReasoningTokens)
	}

	// 100*8e-6 + 40*1e-5 = 0.0012
	if total := ComputeWithCharges(got, prices(), Charges{}).Total; total.Cmp(money.MustParse("0.0012")) != 0 {
		t.Errorf("总额 = %s，期望 0.0012", total)
	}
}

// 整段输出都是推理：拆完输出为 0，推理量原样留着。
//
// 这是合法用量（模型把 max_tokens 全用在思考上），不是自相矛盾的输入，
// 所以告警判据不该命中它。
func TestFromChannelUsageAllOutputIsReasoning(t *testing.T) {
	ch := channel.Usage{
		CompletionTokens:            100,
		ReasoningTokens:             100,
		CompletionIncludesReasoning: true,
	}
	got := FromChannelUsage(ch)
	if got.CompletionTokens != 0 {
		t.Errorf("输出 = %d，期望 0", got.CompletionTokens)
	}
	if got.ReasoningTokens != 100 {
		t.Errorf("推理 = %d，期望 100", got.ReasoningTokens)
	}
	if ReasoningExceedsCompletion(ch) {
		t.Error("整段输出都是推理是合法用量，不该被判成矛盾")
	}
}

// 声明说推理含在输出里，而报出来的推理量大于输出总量：上游自相矛盾。
//
// 拆分把输出钳到 0——负数进金额运算会算出一笔「上游付钱给用户」。
// 但矛盾本身不能就此消失：钳过之后它与「整段输出都是推理」得到同一个
// 计费口径用量，所以判断只能拿协议层的用量来做。
func TestFromChannelUsageClampsContradictoryReasoning(t *testing.T) {
	ch := channel.Usage{
		CompletionTokens:            50,
		ReasoningTokens:             80,
		CompletionIncludesReasoning: true,
	}
	got := FromChannelUsage(ch)
	if got.CompletionTokens != 0 {
		t.Errorf("矛盾时输出应钳到 0，得到 %d", got.CompletionTokens)
	}
	if got.ReasoningTokens != 80 {
		t.Errorf("推理 = %d，期望 80（上游报的照记）", got.ReasoningTokens)
	}
	if total := ComputeWithCharges(got, prices(), Charges{}).Total; total.IsNegative() {
		t.Errorf("金额为负: %s", total)
	}
	if !ReasoningExceedsCompletion(ch) {
		t.Error("推理量大于输出总量、而声明说推理含在输出里，应被判成矛盾")
	}
}

// 同一组数字，声明「独立报」时不是矛盾：可见输出 50 之外另有 80 推理。
// 判据里漏掉声明那一半，这条就会误报。
func TestReasoningExceedsCompletionRequiresDeclaration(t *testing.T) {
	ch := channel.Usage{CompletionTokens: 50, ReasoningTokens: 80}
	if ReasoningExceedsCompletion(ch) {
		t.Error("声明是「独立报」时，推理量大于输出总量是合法的，不该被判成矛盾")
	}
}

// 推理量与输出量恰好相等不是矛盾——它是「整段输出都是推理」。
// 判据写成 >= 就会把这一条误报。
func TestReasoningExceedsCompletionAllowsEqual(t *testing.T) {
	ch := channel.Usage{
		CompletionTokens:            80,
		ReasoningTokens:             80,
		CompletionIncludesReasoning: true,
	}
	if ReasoningExceedsCompletion(ch) {
		t.Error("推理量等于输出量是合法的，不该被判成矛盾")
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
		Prices: p, InputTokens: 1, OutputTokens: 1, Charges: charges,
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
		Prices: p, InputTokens: 100, OutputTokens: 200, Charges: charges,
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
}

// 不传一次性费用时，行为必须与之前完全一致。
func TestZeroChargesPreserveBehavior(t *testing.T) {
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500}
	a := ComputeWithCharges(usage, prices(), Charges{})
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
		OutputTokens:     10,
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
	s, err := SettleWithCharges(actual, p, Charges{}, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capped {
		t.Errorf("缓存写入的请求撞上了 Capped：预留 %s，实际 %s", reserved, s.Breakdown.Total)
	}
}

func TestReserveRejectsNegativeCacheWrite(t *testing.T) {
	_, err := Reserve(ReserveRequest{Prices: prices(), OutputTokens: 10, CacheWriteTokens: -1})
	if err == nil {
		t.Error("负的缓存写入量应被拒绝")
	}
}

// 输入侧按 prompt / cache_read / cache_write 三者里**最贵**的单价估。
//
// 取三者最大值而不是固定取输入单价：上界算法成立的前提是「缓存读、缓存写的
// 单价都不高于输入单价」，而这个前提**没有任何地方被校验**——规则里
// cache_read > prompt 是合法配置（两个数填反、抄错行都会写成这样）。
// 此时按输入单价估会让预留小于结算，Capped 变成常态，差额静默由平台吃掉。
func TestReserveEstimatesCacheReadAtHigherPrice(t *testing.T) {
	p := prices()
	// 把缓存读写差价拉大，差异才看得出来
	p.CacheRead = money.MustParse("0.0000001")
	p.CacheWrite = money.MustParse("0.0001")

	req := ReserveRequest{Prices: p, CacheReadTokens: 1000, CacheWriteTokens: 1000, OutputTokens: 1}
	reserved, err := Reserve(req)
	if err != nil {
		t.Fatal(err)
	}
	// 断言具体金额，而不是「没撞 Capped」——后者在按缓存读单价估时也成立
	//（1e-7 × 1000 = 0.0001，比输入单价更便宜），分辨不出用的是哪一个价。
	//
	// 0.002（缓存读 2e-6 × 1000）+ 0.1（缓存写 1e-4 × 1000）
	// + 0.00001（输出 1e-5 × 1）
	if want := money.MustParse("0.10201"); reserved.Cmp(want) != 0 {
		t.Errorf("预留 = %s，期望 %s", reserved, want)
	}

	actual := Usage{CacheReadTokens: 1000, CacheWriteTokens: 1000}
	s, err := SettleWithCharges(actual, p, Charges{}, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capped {
		t.Errorf("缓存读写都算进了预留，不该撞 Capped：预留 %s 实际 %s", reserved, s.Breakdown.Total)
	}

	// 规则把缓存读单价写成输入的 5 倍时，预留仍须覆盖结算。
	// 按输入单价估会得到预留 10、实际 50——被截到 10 且 Capped，
	// 那 40 由平台静默吃掉。
	flipped := prices()
	flipped.Prompt = money.MustParse("1")
	flipped.CacheRead = money.MustParse("5")
	flipped.Completion = money.Zero
	flipped.CacheWrite = money.Zero
	flipped.Reasoning = money.Zero

	reserved, err = Reserve(ReserveRequest{Prices: flipped, CacheReadTokens: 10, OutputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 10 × max(1, 5) = 50
	if want := money.MustParse("50"); reserved.Cmp(want) != 0 {
		t.Errorf("缓存读单价高于输入时，预留要按贵的那个估：得到 %s，期望 %s", reserved, want)
	}

	s, err = SettleWithCharges(Usage{CacheReadTokens: 10}, flipped, Charges{}, reserved)
	if err != nil {
		t.Fatal(err)
	}
	// 断言 Breakdown.Total 而不是 s.Actual：Capped 时 s.Actual **就是**预留
	//（结算在上界处截断），于是「结算真是 50」与「结算其实是 60、被截到 50」
	// 在 s.Actual 上取值相同。Total 才是没被截断的那个数。
	if want := money.MustParse("50"); s.Breakdown.Total.Cmp(want) != 0 {
		t.Errorf("实际金额 = %s，期望 %s（10 token × 单价 5）", s.Breakdown.Total, want)
	}
	if s.Capped {
		t.Errorf("预留应覆盖结算，却撞了 Capped：预留 %s 实际 %s", reserved, s.Breakdown.Total)
	}
}

// **生产的请求形状**下，输入侧自己就是上界。
//
// 这一条与上一条测的不是同一件事：上一条里调用方**显式给了** CacheReadTokens，
// 于是缓存读那一项非零、能自己撑住上界。而生产里 `pipeline.Request.Usage`
// **从来没有被填过**（入口只给 InputTokens），CacheReadTokens 与
// CacheWriteTokens 恒为 0——乘出来还是 0。所以上界只能靠输入侧。
//
// 输入侧估的是**整段** prompt，而结算时那一段会按三种单价拆开。这条用例钉的
// 就是「拆法怎么变，预留都盖得住」：1000 个 token 里 900 个命中缓存，
// 而缓存读单价是输入的 5 倍。
func TestReserveCoversSplitPromptWithoutCacheEstimates(t *testing.T) {
	p := prices()
	p.Prompt = money.MustParse("1")
	p.CacheRead = money.MustParse("5")
	p.CacheWrite = money.Zero
	p.Completion = money.Zero
	p.Reasoning = money.Zero

	// 生产的形状：只有 InputTokens，两个缓存估计量都是零值。
	reserved, err := Reserve(ReserveRequest{Prices: p, InputTokens: 1000, OutputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 1000 × max(输入 1, 缓存读 5, 缓存写 0) = 5000
	if want := money.MustParse("5000"); reserved.Cmp(want) != 0 {
		t.Errorf("输入侧要按三者里最贵的单价估：得到 %s，期望 %s", reserved, want)
	}

	// 结算时整段 prompt 被拆成「100 未命中 + 900 命中」。
	actual := Usage{PromptTokens: 100, CacheReadTokens: 900}
	s, err := SettleWithCharges(actual, p, Charges{}, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if want := money.MustParse("4600"); s.Breakdown.Total.Cmp(want) != 0 {
		t.Fatalf("实际金额 = %s，期望 %s（100 × 1 + 900 × 5）", s.Breakdown.Total, want)
	}
	if s.Capped {
		t.Errorf("预留 %s 应覆盖结算 %s，却撞了 Capped——输入侧没有按最贵的单价估",
			reserved, s.Breakdown.Total)
	}
}

// 规则没配缓存读单价（零值）时，max 取输入单价——行为与取 max 之前一致。
//
// 这条边界要钉住：max 是「两个单价里取较大的那个」，不是「缓存读有单价就用它」。
func TestReserveCacheReadWithoutPriceUsesInputPrice(t *testing.T) {
	p := prices()
	p.CacheRead = money.Zero
	// 其余两侧清零，断言里剩下的就只有缓存读那一项
	p.Completion = money.Zero
	p.Reasoning = money.Zero

	reserved, err := Reserve(ReserveRequest{Prices: p, CacheReadTokens: 1000, OutputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 1000 × 2e-6 = 0.002（按没配的缓存读单价算是 0）
	if want := money.MustParse("0.002"); reserved.Cmp(want) != 0 {
		t.Errorf("没配缓存读单价时应按输入单价估：得到 %s，期望 %s", reserved, want)
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
// 变异：`nonCached < 0` 改成 `<= 0` 是**等价变异**——边界那一格
// （nonCached 正好是 0）进分支之后做的正是 `nonCached = 0`，赋的还是它本来
// 就有的那个值。要说「加上哪一组输入会让两版不同」：没有。
//
// 这条断言杀不掉上面那一条，但杀得掉方向写反的那些（`<` 改成 `>=` 之类）：
// 那时负数会漏出去，非缓存输入是 -10 而不是 0。
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
// 这段压回逻辑里有三处边界，都是**等价变异**，理由各不相同，分开判：
//
//   - `total > u.PromptTokens` 改成 `>=`：恰好相等那一格算出的 over 是 0，
//     接着 cut = min64(0, cacheWrite) = 0，减掉的是一个零，结果一个数不变。
//   - `over > 0` 改成 `over >= 0`：进分支时 over 也已经是 0（缓存写正好把
//     超出的量吃完那一格），cut = min64(0, cacheRead) = 0，同样是零增量。
//   - `min64` 与 `max64` 自身的边界见 TestMin64AndMax64AtEqualArguments。
//
// 说不出「加上哪一组输入会让两版不同」，所以它们是等价变异；下面第二段照样
// 把 over 归零那一格的具体数固定下来——它是上面第二条变异的边界。
func TestCacheCompressionBoundary(t *testing.T) {
	// 100 输入 = 60 缓存读 + 40 缓存写，恰好相等
	got := FromChannelUsage(channel.Usage{
		PromptTokens: 100, CacheReadTokens: 60, CacheWriteTokens: 40,
	})
	if got.PromptTokens != 0 || got.CacheReadTokens != 60 || got.CacheWriteTokens != 40 {
		t.Errorf("恰好相等时不该压回: %+v", got)
	}

	// 超出的量正好由缓存写吃完：写完 over 归零，第二段压回（缓存读那一支）
	// 进到 `over > 0` 的边界格上。
	got = FromChannelUsage(channel.Usage{
		PromptTokens: 100, CacheReadTokens: 50, CacheWriteTokens: 60,
	})
	if got.PromptTokens != 0 || got.CacheReadTokens != 50 || got.CacheWriteTokens != 50 {
		t.Errorf("超出的 10 该只由缓存写承担: %+v", got)
	}

	// 超出一份就该压
	got = FromChannelUsage(channel.Usage{
		PromptTokens: 100, CacheReadTokens: 60, CacheWriteTokens: 60,
	})
	// 只该抹掉**超出的那 20**，不该把缓存整段抹光。断言写成等式而不是
	// 「不超过 100」：后者抹光了也满足，而抹光会少算缓存费用。
	//
	// 变异：`over := total - u.PromptTokens` 改成 `+`，或 `min64` 改成 max，
	// 超出的量都算大了，于是缓存被抹成 0——合计变成 60 或 0，等式抓得到。
	total := got.PromptTokens + got.CacheReadTokens + got.CacheWriteTokens
	if total != 100 {
		t.Errorf("压回后合计 = %d，应当恰好是 100（只抹掉超出的 20）", total)
	}
}

// min64 与 max64 在两侧相等那一格上是等价变异：两个分支返回的是同一个值。
//
// 变异：min64 的 `a < b` 改成 `a <= b`、max64 的 `a > b` 改成 `a >= b`——
// 边界那一格（a == b）换了个分支走，而两支返回的数是同一个。要说「加上哪一组
// 输入会让两版不同」：得造一组「a == b 而两支返回不同」的输入，这组输入不存在，
// 所以抓不到。
//
// 这一格在计费里真的会出现：缓存量恰好不超过输入量时，压回那一步的
// `min64(over, cacheWrite)` 两侧就相等（见 TestCacheCompressionBoundary）。
// 顺手把两侧不等的方向也固定住：`<` 改成 `>=`（方向反了）在这里现形。
func TestMin64AndMax64AtEqualArguments(t *testing.T) {
	if got := min64(5, 5); got != 5 {
		t.Errorf("min64(5, 5) = %d，期望 5", got)
	}
	if got := max64(5, 5); got != 5 {
		t.Errorf("max64(5, 5) = %d，期望 5", got)
	}
	if got := min64(3, 7); got != 3 {
		t.Errorf("min64(3, 7) = %d，期望 3", got)
	}
	if got := max64(3, 7); got != 7 {
		t.Errorf("max64(3, 7) = %d，期望 7", got)
	}
}

// 最低消费恰好等于按量金额时不该补差。
//
// 变异：`subtotal.Cmp(minimum) >= 0` 改成 `>` 是**等价变异**：恰好相等那一格
// 补出来的差是 `minimum.Sub(subtotal)`，也就是 money.Amount{units: 0}——
// 那个类型只有 units 一个字段，零只有一种表示，写进去与不写是同一个值。
// 校验用的求和里 MinimumAdjustment 本来就在（它不看是不是零），所以也不会
// 因此多出一项。说不出「加上哪一组输入会让两版不同」，所以抓不到。
//
// 这条断言杀得掉方向写反的那些：`>=` 改成 `<` 时，按量金额**高于**最低消费的
// 那一格反而会补出一笔负数（`minimum.Sub(subtotal)` 为负），那一格由
// TestMinimumNotAppliedAboveThreshold 钉着；低于最低消费的那一格则不再补差，
// 另有用例钉着。（这里的夹具两边恰好相等，方向反了也补出 0，所以它单独杀不掉。）
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
	b := ComputeWithCharges(Usage{PromptTokens: 10}, prices(), Charges{})
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

// ── 写路径上的溢出 ──────────────────────────────────────────────────────

// 单项在**相乘**那一步被夹住时，结算必须报错。
//
// 这是「夹住 = 少收」的主形状：Breakdown 的字段存的就是 MulInt 夹住之后的值，
// 所以判据必须在产生它的那一步记下来（见 Breakdown.clamped）——下游拿字段去
// 求和、去比对，看到的都是被污染的数，得不出任何异常。
//
// 输入取真实量级：单价 0.000014 元/token（比常见价高一个数量级），上游报的
// prompt_tokens = 1e16。真实金额是 1400 亿元，放不进 int64 的最小单位表示。
// 这条不需要任何数据前提——上游报什么数、就按什么数算。
func TestSettleRejectsSingleTermOverflow(t *testing.T) {
	p := Prices{Prompt: money.MustParse("0.000014"), Declared: map[string]bool{"prompt": true}}
	actual := Usage{PromptTokens: 1e16}

	// 前提：这一项确实被夹过，且夹住之后**和**放得下——旧判据（求字段之和）
	// 正是因为这一点才放过它。
	raw := ComputeWithCharges(actual, p, Charges{})
	if _, ok := money.MulIntChecked(p.Prompt, actual.PromptTokens); ok {
		t.Fatal("前提不成立：这一项应当在相乘时被夹住")
	}
	if _, ok := raw.sumChecked(); !ok {
		t.Fatal("前提不成立：夹住之后的字段之和应当放得下（这正是旧判据的盲区）")
	}

	_, err := SettleWithCharges(actual, p, Charges{}, money.MustParse("92233720368"))
	if err == nil {
		t.Fatalf("单项夹住时结算必须报错，得到一项 = %s", raw.Prompt)
	}
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("错误里应当能认出 money.ErrOverflow，得到 %v", err)
	}
}

// 同一形状的另一个价量组合：单价 1000 元/token，prompt_tokens = 9.3e9。
//
// 两个数都「看起来不大」，乘积 93 亿亿个最小单位却放不进 int64。它钉的是
// 判据不依赖某个特定量级——单价高、用量中等与单价中等、用量高各测一次。
func TestSettleRejectsSingleTermOverflowHighPrice(t *testing.T) {
	p := Prices{Prompt: money.MustParse("1000"), Declared: map[string]bool{"prompt": true}}
	actual := Usage{PromptTokens: 9_300_000_000}

	raw := ComputeWithCharges(actual, p, Charges{})
	if _, ok := money.MulIntChecked(p.Prompt, actual.PromptTokens); ok {
		t.Fatal("前提不成立：这一项应当在相乘时被夹住")
	}

	_, err := SettleWithCharges(actual, p, Charges{}, money.MustParse("92233720368"))
	if err == nil {
		t.Fatalf("单项夹住时结算必须报错，得到一项 = %s", raw.Prompt)
	}
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("错误里应当能认出 money.ErrOverflow，得到 %v", err)
	}
}

// 各项都放得下、但**和**放不下时，同样要报错。
//
// 这一条与上面两条各自钉一处：那两条是相乘夹住（和放得下），这条是求和
// 溢出（各项都放得下）。判据里两条缺一不可。
func TestSettleRejectsSumOverflow(t *testing.T) {
	// 两项各 6e18 个最小单位（单价 6e-8 元 × 1e18 个 token = 6e10 元），
	// 各自放得下；和是 1.2e19，放不进 int64。
	p := Prices{
		Prompt:     money.MustParse("0.00000006"),
		Completion: money.MustParse("0.00000006"),
		Declared:   map[string]bool{"prompt": true, "completion": true},
	}
	actual := Usage{PromptTokens: 1e18, CompletionTokens: 1e18}

	raw := ComputeWithCharges(actual, p, Charges{})
	if raw.clamped {
		t.Fatal("前提不成立：两项各自应当放得下")
	}
	if _, ok := raw.sumChecked(); ok {
		t.Fatal("前提不成立：两项之和应当放不进 int64")
	}

	_, err := SettleWithCharges(actual, p, Charges{}, money.MustParse("92233720368"))
	if err == nil {
		t.Fatalf("求和溢出时结算必须报错，得到 Total = %s", raw.Total)
	}
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("错误里应当能认出 money.ErrOverflow，得到 %v", err)
	}
}

// 正常金额不受影响：没有溢出时结算照旧。
func TestSettleAcceptsNormalAmounts(t *testing.T) {
	s, err := SettleWithCharges(Usage{PromptTokens: 1000}, prices(), Charges{}, money.One)
	if err != nil {
		t.Fatalf("正常金额不该报错: %v", err)
	}
	if s.Actual.String() != "0.00200000" {
		t.Errorf("Actual = %s，期望 0.00200000", s.Actual)
	}
}

// 空输入 + 调用方给的巨大 max_tokens：预留必须报错。
//
// 这是三条 P0 输入里**唯一不需要任何前提**的一条：max_tokens 是调用方给的
// （internal/api 直接取请求里的字段），pipeline.Request.validate 只要求它
// 为正、没有上界，所以一个请求就能走到。夹住的预留是一个**偏小**的上界，
// 结算会超过它、撞上 Capped，差额静默由平台承担。
func TestReserveRejectsSingleTermOverflow(t *testing.T) {
	p := Prices{
		Completion: money.MustParse("0.000006"),
		Declared:   map[string]bool{"completion": true},
	}
	const maxTokens = int64(4e18)

	// 前提：这一项在相乘时就被夹住了。真实金额是 2.4e13 元。
	if _, ok := money.MulIntChecked(p.Completion, maxTokens); ok {
		t.Fatal("前提不成立：这一项应当在相乘时被夹住")
	}

	reserved, err := Reserve(ReserveRequest{Prices: p, OutputTokens: maxTokens})
	if err == nil {
		t.Fatalf("预留算不出来时必须报错，得到 %s", reserved)
	}
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("错误里应当能认出 money.ErrOverflow，得到 %v", err)
	}
}

// 各分项都放得下、但求和溢出时，预留同样要报错。
//
// 与上一条各自钉一处：那条是相乘夹住，这条是求和溢出。
func TestReserveRejectsSumOverflow(t *testing.T) {
	// 输入侧与输出侧各 6e18 个最小单位，和是 1.2e19，放不进 int64。
	p := Prices{
		Prompt:     money.MustParse("0.00000006"),
		Completion: money.MustParse("0.00000006"),
		Declared:   map[string]bool{"prompt": true, "completion": true},
	}
	const tokens = int64(1e18)

	if _, ok := money.MulIntChecked(p.Prompt, tokens); !ok {
		t.Fatal("前提不成立：输入侧应当放得下")
	}

	_, err := Reserve(ReserveRequest{Prices: p, InputTokens: tokens, OutputTokens: tokens})
	if err == nil {
		t.Fatal("预留求和溢出时必须报错，而不是交回一个偏小的上界")
	}
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("错误里应当能认出 money.ErrOverflow，得到 %v", err)
	}
}

// 正常量级不受影响：预留照旧、与结算对得上。
func TestReserveAcceptsNormalAmounts(t *testing.T) {
	reserved, err := Reserve(ReserveRequest{
		Prices: prices(), InputTokens: 1000, OutputTokens: 500,
	})
	if err != nil {
		t.Fatalf("正常金额不该报错: %v", err)
	}
	// 输入侧按 prompt / cache_read / cache_write 里最贵的估（cacheWrite
	// 2.5e-6）：1000 × 2.5e-6 = 0.0025；输出侧按 max(completion, reasoning)
	// 估（reasoning 1e-5）：500 × 1e-5 = 0.005。合计 0.0075。
	if reserved.String() != "0.00750000" {
		t.Errorf("预留 = %s，期望 0.00750000", reserved)
	}
}
