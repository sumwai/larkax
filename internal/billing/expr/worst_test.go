package expr

import (
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/money"
)

// worstUsage 是一批真实用量的样本。
//
// 覆盖各种形状：空请求、短上下文、长上下文、缓存命中、缓存写入、有计量项、
// 只读不写、只写不读。最坏情况的验证要在**每一条**上都成立，
// 而不是在挑出来的那几条上成立。
//
// 缓存写必须有样本：生产走 pipeline.boundOf，那里把 CacheWriteTokens
// 一并传给 billing.Reserve。夹具不传的话，这条测试测的是「没有缓存写时
// 上界够不够」，而缓存写正是单价最高的一项——它漏掉的那一侧恰是最容易
// 撞 Capped 的一侧（见 LESSONS 第 19 条）。
var worstUsage = []billing.Usage{
	{},
	{PromptTokens: 1},
	{PromptTokens: 1000, CompletionTokens: 500},
	{PromptTokens: 31_999},
	{PromptTokens: 32_000},
	{PromptTokens: 32_001},
	{PromptTokens: 200_000, CompletionTokens: 4096},
	{PromptTokens: 1_000_000},
	{PromptTokens: 50_000, CacheReadTokens: 250_000},
	{CacheReadTokens: 10_000},
	{PromptTokens: 1000, CacheWriteTokens: 5000},
	{PromptTokens: 100, ReasoningTokens: 20_000},
	// 计量项的数量都取 1。数量大于 1 时预留本来就不够：
	// billing.Reserve 对非 token 计量只留**一份**的量（请求前数量未知，
	// 见 reserve.go 的注释），多出来的部分必然撞 Capped。
	// 那是 reserve 侧的设计取舍，与上界算法无关，所以样本里避开它。
	{UnitCounts: map[string]int64{"img": 1}},
	{UnitCounts: map[string]int64{"img": 1, "search": 1}},
	{PromptTokens: 500_000, UnitCounts: map[string]int64{"img": 3}},
}

// settle 用预留—结算的完整路径核对一次上界。
//
// 用 billing 的真接口而不是在这里重算一遍：这条测试要证明的正是
// 「上界足以让结算不撞 Capped」，而撞不撞是 Reserve/Settle 的判断。
func settle(t *testing.T, p *Program, source string, env Env, maxTokens int64) {
	t.Helper()

	// 这条测试只核对**单价**构成的上界，所以把非 token 计量的数量压到 1。
	// billing.Reserve 对这类计量只留一份（请求前数量未知，见 reserve.go），
	// 所以数量大于 1 时预留必然不够——那是 reserve 侧的既定取舍，
	// 与上界算法无关，混进来只会把信号淹没。
	probe := env
	probe.Usage = env.Usage
	if len(env.Usage.UnitCounts) > 0 {
		units := make(map[string]int64, len(env.Usage.UnitCounts))
		for name := range env.Usage.UnitCounts {
			units[name] = 1
		}
		probe.Usage.UnitCounts = units
	}
	// completion 侧的上界由 Reserve 用 max_tokens 估，而结算用的是实际输出量。
	// 探针只给输出留 1 个 token 的余量，实际输出超过它时撞 Capped 是调用方
	// （上游报了超过 max_tokens 的用量）的问题，不是上界的问题。
	if probe.Usage.CompletionTokens > maxTokens {
		probe.Usage.CompletionTokens = maxTokens
	}
	if probe.Usage.ReasoningTokens > maxTokens {
		probe.Usage.ReasoningTokens = maxTokens
	}

	worst, err := p.EvalWorstCase(probe)
	if err != nil {
		t.Fatalf("最坏情况求值失败: %v", err)
	}
	reserved, err := billing.Reserve(billing.ReserveRequest{
		Prices:          worst.Prices,
		Charges:         worst.Charges,
		OutputTokens:    maxTokens,
		InputTokens:     probe.Usage.PromptTokens,
		CacheReadTokens: probe.Usage.CacheReadTokens,
		// 生产传了它（internal/pipeline/pipeline.go 的 boundOf）。探针少传一项，
		// 预留就少算一份缓存写的钱，而结算照收——于是这条测试会在缓存写
		// 单价高于输入时撞 Capped，看起来像「上界算法错了」，实际是夹具漏了。
		CacheWriteTokens: probe.Usage.CacheWriteTokens,
	})
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	actual, err := p.Eval(probe)
	if err != nil {
		t.Fatalf("结算求值失败: %v", err)
	}

	s, err := billing.SettleWithCharges(probe.Usage, actual.Prices, actual.Charges, reserved)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if s.Capped {
		// Capped 本该是「上界算错了」的信号，不该在正常用量下出现。
		t.Fatalf("结算撞上 Capped：用量 %+v，预留 %s，实际 %s\n规则: %s",
			probe.Usage, s.Reserved, s.Actual, source)
	}
	if err := s.Breakdown.Verify(); err != nil {
		t.Fatalf("明细不自洽: %v", err)
	}
}

// TestWorstCaseCoversActual 是本包最要紧的一条测试。
//
// 它逐个用量确认「实际金额 ≤ 预留金额」。上界算小了，结算就会撞 Capped，
// 而 Capped 是「上界算错了」的信号——它不该在正常用量下出现。
//
// 每一条规则都要覆盖：分档、分时、缓存差异、计量项、固定费、保底。
func TestWorstCaseCoversActual(t *testing.T) {
	cal := newTestCalendar()
	rules := map[string]string{
		"按上下文长度分档": `
			len <= 32000
				? tier("standard", {"prompt": 0.0000015, "completion": 0.0000075})
				: tier("long_context", {"prompt": 0.0000030, "completion": 0.00001125})`,

		// 关键的一条：两个分支各有各的贵处——高峰档的输入便宜但输出贵，
		// 低谷档反过来。取「某一支的全部」作为上界会在另一边漏掉。
		"各槽的贵处不在同一支": `
			hour("Asia/Shanghai") >= 9 and hour("Asia/Shanghai") < 21
				? tier("peak", {"prompt": 0.0000010, "completion": 0.0000090})
				: tier("off_peak", {"prompt": 0.0000020, "completion": 0.0000045})`,

		"缓存与非缓存差很多": `
			len <= 32000
				? {"prompt": 0.0000015, "completion": 0.0000075, "cache_read": 0.0000002, "cache_write": 0.0000019, "reasoning": 0.0000090}
				: {"prompt": 0.0000030, "completion": 0.0000113, "cache_read": 0.0000003, "cache_write": 0.0000038, "reasoning": 0.0000135}`,

		// 缓存写**低于**输入单价的那一侧。billing.Reserve 的输入侧取
		// max(prompt, cache_read, cache_write)，只配「缓存写更贵」的规则
		// 会把 max 的另一条路径永远遮住——那种配置下选错也是对的。
		"缓存写比输入便宜": `
			len <= 32000
				? {"prompt": 0.0000019, "completion": 0.0000075, "cache_read": 0.0000002, "cache_write": 0.0000005}
				: {"prompt": 0.0000038, "completion": 0.0000113, "cache_read": 0.0000003, "cache_write": 0.0000010}`,

		"计量项在某一支才有": `
			units["img"] > 0
				? {"prompt": 0.0000015, "completion": 0.0000075, "units": {"img": 0.02, "search": 0.01}}
				: {"prompt": 0.0000015, "completion": 0.0000075, "units": {"search": 0.01}}`,

		"带固定费与保底": `
			{
				"prompt": isHoliday() ? 0.0000030 : 0.0000015,
				"completion": 0.0000075,
				"fixed": fixed(0.02),
				"minimum": minimum(0.5)
			}`,

		"工作日与节假日走不同分支": `
			isWorkday()
				? tier("workday", {"prompt": 0.0000015, "completion": 0.0000075, "fixed": fixed(0.002)})
				: tier("rest_day", {"prompt": 0.0000030, "completion": 0.0000120, "fixed": fixed(0.005)})`,

		// 价格位置上的条件：上界要取条件两侧里较大的那个数。
		"价格位置上的条件": `
			{
				"prompt": 0.0000015 + (isHoliday() ? 0.0000015 : 0.0),
				"completion": 0.0000075
			}`,

		"嵌套条件": `
			len <= 10000
				? {"prompt": 0.0000010, "completion": 0.0000050}
				: (len <= 200000
					? {"prompt": 0.0000020, "completion": 0.0000100}
					: {"prompt": 0.0000040, "completion": 0.0000200})`,

		"单一价目表": `{"prompt": 0.0000015, "completion": 0.0000075}`,
	}

	// 两个求值时刻：一个工作日的高峰时段、一个休息日。
	atTimes := map[string]time.Time{
		"工作日高峰": time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC),
		"休息日":   time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC),
		"节假日":   time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC),
	}
	maxTokens := []int64{1, 4096, 16384}

	for ruleName, source := range rules {
		p := mustCompile(t, source)
		for timeName, at := range atTimes {
			for _, usage := range worstUsage {
				for _, mt := range maxTokens {
					env := Env{
						Usage:    usage,
						At:       at,
						Region:   "CN",
						Calendar: cal,
					}
					if !rulePricesEveryItem(t, p, env) {
						// 规则没给用到的某一项定价（token 或计量项）。那是
						// 「用量与规则不匹配」，billing.Validate 会报出来，
						// 与上界无关，换一条。
						continue
					}
					t.Run(ruleName+"/"+timeName, func(t *testing.T) {
						settle(t, p, source, env, mt)
					})
				}
			}
		}
	}
}

// rulePricesEveryItem 判断规则是否为这次用量用到的每一项都给了单价
// （token 与计量项都算）。
//
// 缺一项时 billing 会以 ErrMissingPrice 报错——那是「用量与规则不匹配」，
// 属于另一类问题，不该混进「上界够不够」的验证里。
//
// 判据直接用 billing.Validate，不在这里另写一套：Validate 查什么，这里就跳什么。
// 早先这里写的是「没有计量项就直接放行」，因为那时 Validate 只查计量项；
// token 单价也纳入检查之后，那个提前返回会把一批组合放进 settle 里，
// 让它们以「规则不完整」失败——而那不是这条测试要测的东西。
func rulePricesEveryItem(t *testing.T, p *Program, env Env) bool {
	t.Helper()
	res, err := p.Eval(env)
	if err != nil {
		return false
	}
	return billing.Validate(env.Usage, res.Prices) == nil
}

// TestWorstCaseIsNotJustOneBranch 钉住「逐槽取最大值」这个做法。
//
// 一条规则的两支各有各的贵处：高峰档输出贵、低谷档输入贵。
// 若上界取的是「某一支的全部」，另一支就会在某一边超出去。
// 这条测试直接检查逐槽结果，比 settle 那条更早定位到问题。
func TestWorstCaseIsNotJustOneBranch(t *testing.T) {
	p := mustCompile(t, `hour("Asia/Shanghai") >= 9 and hour("Asia/Shanghai") < 21
		? {"prompt": 0.0000010, "completion": 0.0000090}
		: {"prompt": 0.0000020, "completion": 0.0000045}`)

	env := Env{
		Usage:  billing.Usage{PromptTokens: 1000, CompletionTokens: 1000},
		At:     testAt, // 上海时间 17:30，落在高峰支
		Region: "CN",
	}

	peak, err := p.Eval(env)
	if err != nil {
		t.Fatal(err)
	}
	worst, err := p.EvalWorstCase(env)
	if err != nil {
		t.Fatal(err)
	}

	// 实际走的是高峰支。
	if peak.Prices.Prompt.String() != "0.00000100" {
		t.Fatalf("应当是高峰支的输入单价: %s", peak.Prices.Prompt)
	}
	// 上界逐槽取最大值：输入取低谷支的 0.0000020，输出取高峰支的 0.0000090。
	if got := worst.Prices.Prompt.String(); got != "0.00000200" {
		t.Errorf("上界的输入单价 = %s，期望 0.00000200（低谷支更贵）", got)
	}
	if got := worst.Prices.Completion.String(); got != "0.00000900" {
		t.Errorf("上界的输出单价 = %s，期望 0.00000900（高峰支更贵）", got)
	}

	// 逐槽上界一定不低于实际命中的那一支的任意一槽。
	if worst.Prices.Prompt.Cmp(peak.Prices.Prompt) < 0 {
		t.Error("上界的输入单价低于实际")
	}
	if worst.Prices.Completion.Cmp(peak.Prices.Completion) < 0 {
		t.Error("上界的输出单价低于实际")
	}
}

// TestWorstCaseCoversUnitsOnEitherBranch 钉住「键取并集」。
//
// 一支给了 units 而另一支没有时，上界必须带上 units——
// 否则结算时按另一支算出了这笔费用，预留里却没有，就撞 Capped。
func TestWorstCaseCoversUnitsOnEitherBranch(t *testing.T) {
	p := mustCompile(t, `units["img"] > 0
		? {"prompt": 0.0000015, "units": {"img": 0.02}}
		: {"prompt": 0.0000015}`)

	// 用量里没有 img，实际会走假分支；但请求期间完全可能有图片，
	// 所以上界要把 img 那一槽带上。
	env := Env{Usage: billing.Usage{PromptTokens: 100}, At: testAt}
	worst, err := p.EvalWorstCase(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := worst.Prices.Units["img"]; !ok {
		t.Fatalf("上界里缺了只在真分支出现的计量项: %v", worst.Prices.Units)
	}
	if got := worst.Prices.Units["img"].String(); got != "0.02000000" {
		t.Errorf("img 的上界 = %s", got)
	}

	// 反过来也一样：用量里有 img 时，假分支里的搜索费也要进上界。
	p2 := mustCompile(t, `units["img"] > 0
		? {"prompt": 0.0000015, "units": {"img": 0.02}}
		: {"prompt": 0.0000015, "units": {"search": 0.01}}`)
	env2 := Env{Usage: billing.Usage{UnitCounts: map[string]int64{"img": 1}}, At: testAt}
	worst2, err := p2.EvalWorstCase(env2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := worst2.Prices.Units["search"]; !ok {
		t.Errorf("上界里缺了只在假分支出现的计量项: %v", worst2.Prices.Units)
	}
	if _, ok := worst2.Prices.Units["img"]; !ok {
		t.Errorf("上界里缺了只在真分支出现的计量项: %v", worst2.Prices.Units)
	}
}

// TestWorstCaseCoversTheWiderBranch 验证一侧的键比另一侧多时，多出来的那些照样进上界。
//
// 并集的容量按两支的键数之和预留，那只是**容量提示**：写小了顶多多扩容一次，
// 两支键数不等时并集的内容一项不少。这条用例给的是「真分支只有 img、
// 假分支多一个 search」——键只在一侧出现的那条路要走到，而现有那两条用例
// 两侧键数相同，绕开了这个形状。
func TestWorstCaseCoversTheWiderBranch(t *testing.T) {
	p := mustCompile(t, `len <= 1
		? {"prompt": 0.0000015, "units": {"img": 0.02}}
		: {"prompt": 0.0000015, "units": {"img": 0.02, "search": 0.01}}`)

	worst, err := p.EvalWorstCase(Env{Usage: billing.Usage{PromptTokens: 100}, At: testAt})
	if err != nil {
		t.Fatal(err)
	}
	// 只在假分支出现的 search 也要进上界：命中的那一支未必更贵，
	// 而上界要覆盖每一支。
	if got := worst.Prices.Units["search"].String(); got != "0.01000000" {
		t.Errorf("上界里的 search = %s，期望 0.01000000（整组：%v）", got, worst.Prices.Units)
	}
	if got := worst.Prices.Units["img"].String(); got != "0.02000000" {
		t.Errorf("上界里的 img = %s，期望 0.02000000", got)
	}
}

// TestWorstCaseCoversFixedAndMinimum 验证一次性费用也取上界。
func TestWorstCaseCoversFixedAndMinimum(t *testing.T) {
	p := mustCompile(t, `{
		"prompt": 0.0000015,
		"fixed": isWorkday() ? fixed(0.002) : fixed(0.010),
		"minimum": 0.5
	}`)

	// 工作日：实际 0.002，上界要取休息日的 0.010。
	workday, err := p.Eval(Env{Usage: billing.Usage{PromptTokens: 1}, At: testAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatal(err)
	}
	if got := workday.Charges.Fixed.String(); got != "0.00200000" {
		t.Errorf("工作日的固定费 = %s", got)
	}

	worst, err := p.EvalWorstCase(Env{Usage: billing.Usage{PromptTokens: 1}, At: testAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatal(err)
	}
	if got := worst.Charges.Fixed.String(); got != "0.01000000" {
		t.Errorf("固定费的上界 = %s，期望 0.01000000", got)
	}
	if got := worst.Charges.Minimum.String(); got != "0.50000000" {
		t.Errorf("保底的上界 = %s", got)
	}
}

// TestWorstCaseEvaluatesAllBranches 验证未命中的分支也确实被求值。
//
// 用「未命中分支里的价格出现在上界里」来证明，而不是用计数器：
// 那正是这条性质要保证的后果。条件本身（`cond ?` 里条件那一部分）
// 在最坏情况里被丢掉了——上界不该与条件有关，所以条件里的副作用
// 不会执行。这里刻意把价值写在两个分支的**值位置**上。
//
// 这条比「结果大于等于实际」更细：结果可能碰巧相等，但未命中的分支
// 完全没参与，那样的上界是脆的——一旦规则改成一个分支更贵就会立刻撞 Capped。
func TestWorstCaseEvaluatesAllBranches(t *testing.T) {
	// 用量走短上下文档（真分支），但假分支的输入单价更贵：
	// 上界必须取假分支的 0.0000040，这说明假分支被求值过。
	p := mustCompile(t, `len <= 100
		? {"prompt": 0.0000010, "completion": 0.0000050}
		: {"prompt": 0.0000040, "completion": 0.0000200}`)

	env := Env{Usage: billing.Usage{PromptTokens: 10}, At: testAt}

	actual, err := p.Eval(env)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Prices.Prompt.String() != "0.00000100" {
		t.Fatalf("实际应当走真分支: %s", actual.Prices.Prompt)
	}

	worst, err := p.EvalWorstCase(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := worst.Prices.Prompt.String(); got != "0.00000400" {
		t.Errorf("上界的输入单价 = %s，期望未命中分支的 0.00000400", got)
	}
	if got := worst.Prices.Completion.String(); got != "0.00002000" {
		t.Errorf("上界的输出单价 = %s，期望未命中分支的 0.00002000", got)
	}
}

// TestWorstCaseDropsConditions 记录一条刻意的行为：最坏情况不求值条件。
//
// 上界只由「每个价格位置在各分支里的最大值」构成，与哪个条件成立无关，
// 所以条件被丢掉是设计的一部分。后果是**条件里的副作用不会执行**——
// 例如 `hour("Asia/Shanghai") >= 9 ? A : B` 在预留时不会去解析时区。
// 这本身是好事（预留更快、更少依赖），但它意味着规则作者不能把
// 「必须发生的事」写在条件里。
func TestWorstCaseDropsConditions(t *testing.T) {
	p := mustCompile(t, `hour("Not/A/Real_Zone") >= 9
		? {"prompt": 0.0000010}
		: {"prompt": 0.0000040}`)

	// 普通求值会去解析时区，因此报错。
	if _, err := p.Eval(Env{Usage: billing.Usage{}, At: testAt}); err == nil {
		t.Error("普通求值应当因为时区名非法而失败")
	}
	// 最坏情况求值把条件丢掉了，因此能给出上界。
	worst, err := p.EvalWorstCase(Env{Usage: billing.Usage{}, At: testAt})
	if err != nil {
		t.Fatalf("最坏情况不该求值条件: %v", err)
	}
	if got := worst.Prices.Prompt.String(); got != "0.00000400" {
		t.Errorf("上界 = %s，期望 0.00000400", got)
	}
}

// TestWorstCaseRejectsNonNumericBranch 验证一支不是数值时上界算不出来，而不是照另一支算。
//
// 最坏情况求值把条件丢掉、两支都要过一遍，所以它读到的形状不受「哪一支会命中」
// 影响——规则里一支写成了字符串时，这里就该报错。
// 若只校验其中一支不是数值，上界会静默地取另一支的数：那是一个偏小的上界，
// 结算越过它就会撞上 Capped，差额由平台承担。
func TestWorstCaseRejectsNonNumericBranch(t *testing.T) {
	p := mustCompile(t, `{"prompt": len <= 1 ? "x" : 2.0}`)
	worst, err := p.EvalWorstCase(Env{Usage: billing.Usage{}, At: testAt})
	if err == nil {
		t.Fatalf("一支不是数值时应当报错，实际算出 prompt = %s", worst.Prices.Prompt)
	}
	if !strings.Contains(err.Error(), "__worst 的参数必须是数值或单价表") {
		t.Errorf("错误信息应当说清是 __worst 收到的东西不对，得到: %v", err)
	}
}

// TestWorstCaseIsMonotoneInQuantity 验证「单价 × 用量」那一侧的上界也成立。
//
// 单价不随用量变化，所以金额的上界可以由上界单价乘同一个用量得到——
// 这条是 EvalWorstCase 能用的前提。它不需要 Reserve 参与，
// 单独钉一遍是为了让失败的原因更清楚。
func TestWorstCaseIsMonotoneInQuantity(t *testing.T) {
	p := mustCompile(t, `len <= 32000
		? {"prompt": 0.0000015, "completion": 0.0000075, "cache_read": 0.0000002, "cache_write": 0.0000019, "reasoning": 0.0000150}
		: {"prompt": 0.0000030, "completion": 0.0000113, "cache_read": 0.0000003, "cache_write": 0.0000038, "reasoning": 0.0000300}`)

	for _, usage := range worstUsage {
		env := Env{Usage: usage, At: testAt}
		actual, err := p.Eval(env)
		if err != nil {
			t.Fatalf("%+v: %v", usage, err)
		}
		worst, err := p.EvalWorstCase(env)
		if err != nil {
			t.Fatalf("%+v: %v", usage, err)
		}

		actualAmount := billing.ComputeWithCharges(usage, actual.Prices, billing.Charges{}).Total
		worstAmount := billing.ComputeWithCharges(usage, worst.Prices, billing.Charges{}).Total
		if worstAmount.Cmp(actualAmount) < 0 {
			t.Errorf("用量 %+v: 上界金额 %s 小于实际 %s", usage, worstAmount, actualAmount)
		}
		for name, actualUnit := range actual.Prices.Units {
			worstUnit, ok := worst.Prices.Units[name]
			if !ok {
				t.Errorf("用量 %+v: 上界里缺了计量项 %q", usage, name)
				continue
			}
			if worstUnit.Cmp(actualUnit) < 0 {
				t.Errorf("用量 %+v: 计量项 %q 的上界 %s 小于实际 %s",
					usage, name, worstUnit, actualUnit)
			}
		}
	}
}

// TestWorstCaseHasNoTier 验证最坏情况求值不报档位名。
//
// 它要算遍所有分支，而「命中了哪一档」在那种语义下没有答案。
// 与其给一个会误导的值（最后一次调用的那个），不如留空。
func TestWorstCaseHasNoTier(t *testing.T) {
	p := mustCompile(t, `len <= 32000
		? tier("standard", {"prompt": 0.0000015})
		: tier("long_context", {"prompt": 0.0000030})`)

	env := Env{Usage: billing.Usage{PromptTokens: 1000}, At: testAt}
	actual, err := p.Eval(env)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Tier != "standard" {
		t.Errorf("实际命中的档位 = %q", actual.Tier)
	}

	worst, err := p.EvalWorstCase(env)
	if err != nil {
		t.Fatal(err)
	}
	if worst.Tier != "" {
		t.Errorf("最坏情况不该报档位名，得到 %q", worst.Tier)
	}
	if worst.Prices.Prompt.String() != "0.00000300" {
		t.Errorf("上界的输入单价 = %s", worst.Prices.Prompt)
	}
}

// TestWorstCaseMatchesForConstantRules 验证只有一支价目表时，
// 上界与实际完全相等——那种规则不该有多留的部分。
func TestWorstCaseMatchesForConstantRules(t *testing.T) {
	p := mustCompile(t, `{"prompt": 0.0000015, "completion": 0.0000075, "fixed": fixed(0.002), "minimum": minimum(0.05)}`)
	env := Env{Usage: billing.Usage{PromptTokens: 1_000_000}, At: testAt}

	actual, err := p.Eval(env)
	if err != nil {
		t.Fatal(err)
	}
	worst, err := p.EvalWorstCase(env)
	if err != nil {
		t.Fatal(err)
	}
	if !pricesEqual(actual.Prices, worst.Prices) {
		t.Errorf("单价不同:\n实际 %+v\n上界 %+v", actual.Prices, worst.Prices)
	}
	if actual.Charges != worst.Charges {
		t.Errorf("一次性费用不同:\n实际 %+v\n上界 %+v", actual.Charges, worst.Charges)
	}
}

// TestWorstCaseUnsafeWithoutTimerFreeze 说明为什么时间必须从 Env 来。
//
// 假设两条求值路径各自读了墙钟，跨整点时预留与结算会落在不同的档上。
// 这里用一个「时刻不同」的场景把后果摆出来：同一个规则、
// 同一份用量，仅因为 At 不同，预留的上界就该覆盖到另一个时刻的价格。
func TestWorstCaseUnsafeWithoutTimerFreeze(t *testing.T) {
	p := mustCompile(t, `hour("Asia/Shanghai") >= 9 and hour("Asia/Shanghai") < 21
		? {"prompt": 0.0000030, "completion": 0.0000120}
		: {"prompt": 0.0000010, "completion": 0.0000045}`)

	peak := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)    // 上海 17:30，高峰
	offPeak := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC) // 上海 23:00，低谷

	// 同一条规则在两个时刻的实际价不同，这正是不能读墙钟的原因。
	a, err := p.Eval(Env{Usage: billing.Usage{PromptTokens: 1000}, At: peak})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Eval(Env{Usage: billing.Usage{PromptTokens: 1000}, At: offPeak})
	if err != nil {
		t.Fatal(err)
	}
	if a.Prices.Prompt.Cmp(b.Prices.Prompt) == 0 {
		t.Fatal("两个时刻应当给出不同的单价")
	}

	// 而上界对两个时刻是同一个（取贵的那一支），
	// 所以只要预留与结算用的是同一个 At，上界就覆盖得住。
	for _, at := range []time.Time{peak, offPeak} {
		env := Env{Usage: billing.Usage{PromptTokens: 1000}, At: at}
		worst, err := p.EvalWorstCase(env)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := p.Eval(env)
		if err != nil {
			t.Fatal(err)
		}
		if worst.Prices.Prompt.Cmp(actual.Prices.Prompt) < 0 {
			t.Errorf("时刻 %v: 上界 %s 小于实际 %s", at, worst.Prices.Prompt, actual.Prices.Prompt)
		}
	}
}

// TestWorstCaseAmountsAreExact 验证上界也是确定性的：同样的输入给出同样的金额。
func TestWorstCaseAmountsAreExact(t *testing.T) {
	p := mustCompile(t, `len <= 32000
		? {"prompt": 0.0000015, "completion": 0.0000075}
		: {"prompt": 0.0000030, "completion": 0.0000113}`)
	env := Env{Usage: billing.Usage{PromptTokens: 1000, CompletionTokens: 100}, At: testAt}

	var first money.Amount
	for i := 0; i < 20; i++ {
		res, err := p.EvalWorstCase(env)
		if err != nil {
			t.Fatal(err)
		}
		got := billing.ComputeWithCharges(env.Usage, res.Prices, billing.Charges{}).Total
		if i == 0 {
			first = got
			continue
		}
		if got.Cmp(first) != 0 {
			t.Fatalf("第 %d 次的上界金额 %s，第一次是 %s", i, got, first)
		}
	}
}

// pricesEqual 比较两组单价。Prices 里有 map，不能用 == 比较。
func pricesEqual(a, b billing.Prices) bool {
	if a.Prompt != b.Prompt || a.Completion != b.Completion ||
		a.CacheRead != b.CacheRead || a.CacheWrite != b.CacheWrite ||
		a.Reasoning != b.Reasoning {
		return false
	}
	if len(a.Units) != len(b.Units) {
		return false
	}
	for k, v := range a.Units {
		if b.Units[k] != v {
			return false
		}
	}
	return true
}
