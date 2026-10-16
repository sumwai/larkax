package expr

import (
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/money"
)

// 固定一个求值时刻，避免测试依赖运行环境的时区与真实日期。
// 2026-10-16 是周五；下面用它来验证「地区日历」与「时区」是分开的两件事。
var testAt = time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)

// testCalendar 是一个照着 calendar_days 表形状写的假日历。
//
// 它证明 Calendar 抽象是对的：求值完全脱离数据库。
type testCalendar struct {
	tz    map[string]string
	days  map[string]DayInfo
	fails bool
}

func (c *testCalendar) TimeZone(region string) (string, error) {
	if c.fails {
		return "", ErrCalendarRequired
	}
	tz, ok := c.tz[region]
	if !ok {
		return "", ErrCalendarRequired
	}
	return tz, nil
}

func (c *testCalendar) DayInfo(region string, day time.Time) (DayInfo, error) {
	info, ok := c.days[region+"|"+day.Format("2006-01-02")]
	if !ok {
		return DayInfo{}, ErrCalendarRequired
	}
	return info, nil
}

func newTestCalendar() *testCalendar {
	return &testCalendar{
		tz: map[string]string{"CN": "Asia/Shanghai", "US": "America/New_York"},
		days: map[string]DayInfo{
			// 北京时间的 10 月 16 日：周五，普通工作日。
			"CN|2026-10-16": {},
			// 同一天在纽约的日历里：调休补班日。
			"US|2026-10-16": {MakeupWorkday: true},
			"CN|2026-10-01": {OffDay: true, HolidayName: "国庆节"},
			"CN|2026-10-17": {OffDay: true},
			"US|2026-10-01": {OffDay: true},
		},
	}
}

func mustCompile(t *testing.T, source string) *Program {
	t.Helper()
	p, err := Compile(source)
	if err != nil {
		t.Fatalf("编译 %q 失败: %v", source, err)
	}
	return p
}

func eval(t *testing.T, source string, usage billing.Usage) Result {
	t.Helper()
	p := mustCompile(t, source)
	res, err := p.Eval(Env{Usage: usage, At: testAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatalf("求值 %q 失败: %v", source, err)
	}
	return res
}

// TestEvalPricesAndUnits 覆盖最基本的形状：一组单价 + 一个计量项表。
func TestEvalPricesAndUnits(t *testing.T) {
	source := `{
		"prompt": 0.0000015,
		"completion": 0.0000075,
		"cache_read": 0.00000015,
		"units": {"search": 0.01}
	}`
	res := eval(t, source, billing.Usage{})

	if got := res.Prices.Prompt.String(); got != "0.00000150" {
		t.Errorf("prompt 单价 = %s", got)
	}
	if got := res.Prices.Completion.String(); got != "0.00000750" {
		t.Errorf("completion 单价 = %s", got)
	}
	if got := res.Prices.Units["search"].String(); got != "0.01000000" {
		t.Errorf("search 单价 = %s", got)
	}
	// 没有写 fixed/minimum 时一次性费用必须是零，不能是某个来自上一条规则的残留。
	if !res.Charges.IsZero() {
		t.Errorf("未声明一次性费用却得到 %+v", res.Charges)
	}
	if res.At != testAt {
		t.Errorf("回传的求值时刻 = %v，期望 %v", res.At, testAt)
	}

	// Declared 只记规则里**写过**的键：上面写了 prompt / completion /
	// cache_read，没写 cache_write 与 reasoning。
	for _, name := range []string{"prompt", "completion", "cache_read"} {
		if !res.Prices.Declared[name] {
			t.Errorf("规则写过 %q，Declared 里却没有", name)
		}
	}
	for _, name := range []string{"cache_write", "reasoning"} {
		if res.Prices.Declared[name] {
			t.Errorf("规则没写 %q，Declared 里却有", name)
		}
	}
}

// TestEvalRecordsExplicitZeroAsDeclared 验证显式写 0 也记进 Declared。
// 它是「这一项免费」，与「规则没写」不是一回事——两者的单价都落在 0，
// 而 Validate 只拒后者。
func TestEvalRecordsExplicitZeroAsDeclared(t *testing.T) {
	res := eval(t, `{"prompt": 0.0000015, "reasoning": 0}`, billing.Usage{})
	if !res.Prices.Declared["reasoning"] {
		t.Error("规则里显式写了 reasoning: 0，Declared 应记上")
	}
	if !res.Prices.Reasoning.IsZero() {
		t.Errorf("reasoning 单价应为 0，得到 %s", res.Prices.Reasoning)
	}
	if res.Prices.Declared["completion"] {
		t.Error("规则没写 completion，Declared 里不该有")
	}
}

// TestEvalConditionalPicksBranch 验证条件按用量选档。
func TestEvalConditionalPicksBranch(t *testing.T) {
	source := `len <= 32000
		? tier("standard", {"prompt": 0.0000015, "completion": 0.0000075})
		: tier("long_context", {"prompt": 0.0000030, "completion": 0.00001125})`

	short := eval(t, source, billing.Usage{PromptTokens: 1000})
	if short.Tier != "standard" || short.Prices.Prompt.String() != "0.00000150" {
		t.Errorf("短上下文: tier=%q prompt=%s", short.Tier, short.Prices.Prompt)
	}

	// 缓存读进来的部分也算进 len：300K 上下文被缓存了 250K，
	// 仍然是长上下文档——用非缓存输入判档会把档位判错。
	long := eval(t, source, billing.Usage{PromptTokens: 50_000, CacheReadTokens: 250_000})
	if long.Tier != "long_context" || long.Prices.Prompt.String() != "0.00000300" {
		t.Errorf("长上下文: tier=%q prompt=%s", long.Tier, long.Prices.Prompt)
	}
	if long.Prices.Units != nil {
		t.Errorf("这条规则没有 units，却得到 %v", long.Prices.Units)
	}
}

// TestEvalLenIsVariableNotBuiltin 钉住「变量遮蔽内置函数」这个行为。
//
// len 是 expr 的内置函数名，而本项目把它用作上下文长度变量。
// 这个行为是整个 DSL 的基础，一旦 expr 改了它，规则的含义会静默改变。
func TestEvalLenIsVariableNotBuiltin(t *testing.T) {
	res := eval(t, `len <= 100 ? {"prompt": 1.0} : {"prompt": 2.0}`,
		billing.Usage{PromptTokens: 100})
	if res.Prices.Prompt.String() != "1.00000000" {
		t.Errorf("len 没有被当作变量: prompt=%s", res.Prices.Prompt)
	}

	// 作为函数调用时应当报「不是可调用的」，而不是静默调用内置的 len()。
	_, err := Compile(`{"prompt": len("abc")}`)
	if err == nil {
		t.Fatal("len 作为函数调用竟然编过了")
	}
	if !strings.Contains(err.Error(), "not callable") {
		t.Errorf("错误信息里应当说明不可调用，得到: %v", err)
	}
}

// TestEvalUnitsConditions 验证非 token 计量可以作条件。
func TestEvalUnitsConditions(t *testing.T) {
	source := `{
		"prompt": 0.0000015,
		"units": units["img"] > 0 ? {"img": 0.02} : {"search": 0.01}
	}`
	res := eval(t, source, billing.Usage{UnitCounts: map[string]int64{"img": 2}})
	if got := res.Prices.Units["img"].String(); got != "0.02000000" {
		t.Errorf("命中有图片的那一支: units=%v", res.Prices.Units)
	}
	if _, ok := res.Prices.Units["search"]; ok {
		t.Errorf("未命中的分支不该出现在单价里: %v", res.Prices.Units)
	}

	// 计量项完全没出现时，units 是空表而不是 nil ——
	// 否则 `units["img"] > 0` 会报一个与规则无关的类型错。
	empty := eval(t, source, billing.Usage{})
	if got := empty.Prices.Units["search"].String(); got != "0.01000000" {
		t.Errorf("无用量时应当走另一支: units=%v", empty.Prices.Units)
	}
}

// TestEvalTimeAndRegion 验证时间函数的切分。
func TestEvalTimeAndRegion(t *testing.T) {
	// hour 只回答「现在几点」：时区由规则给出。
	res := eval(t, `{"prompt": hour("Asia/Shanghai") >= 17 ? 0.0000020 : 0.0000010}`,
		billing.Usage{})
	// testAt 是 09:30 UTC = 17:30 北京时间，落在高峰档。
	if res.Prices.Prompt.String() != "0.00000200" {
		t.Errorf("上海时间 17:30 应当是高峰: prompt=%s", res.Prices.Prompt)
	}

	// 同一个时刻在纽约是 05:30，不是高峰。
	ny := eval(t, `{"prompt": hour("America/New_York") >= 17 ? 0.0000020 : 0.0000010}`,
		billing.Usage{})
	if ny.Prices.Prompt.String() != "0.00000100" {
		t.Errorf("纽约时间 05:30 不应当是高峰: prompt=%s", ny.Prices.Prompt)
	}
}

// TestEvalCalendar 验证工作日/节假日取自地区日历。
func TestEvalCalendar(t *testing.T) {
	workday := `{"prompt": isWorkday() ? 0.0000015 : 0.0000010}`
	res := eval(t, workday, billing.Usage{})
	if res.Prices.Prompt.String() != "0.00000150" {
		t.Errorf("北京时间 10 月 16 日是周五，应当是工作日: %s", res.Prices.Prompt)
	}

	// 日历里 10 月 1 日是节假日。
	holidayAt := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	p := mustCompile(t, `{"prompt": isHoliday() ? 0.0000030 : 0.0000010}`)
	res2, err := p.Eval(Env{At: holidayAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Prices.Prompt.String() != "0.00000300" {
		t.Errorf("10 月 1 日应当判为节假日: %s", res2.Prices.Prompt)
	}

	// 周末是休息日但不是「节假日」：两者必须是不同的判据，
	// 否则规则里「节假日」那一档会把每个周末也吃进来。
	p3 := mustCompile(t, `{"prompt": (isHoliday() ? 1.0 : 0.0) + (isWorkday() ? 0.0 : 1.0)}`)
	weekendAt := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	res3, err := p3.Eval(Env{At: weekendAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatal(err)
	}
	if res3.Prices.Prompt.String() != "1.00000000" {
		t.Errorf("周六应当是「非工作日且非节假日」，两者之和应为 1: %s", res3.Prices.Prompt)
	}
}

// TestEvalRegionIsSeparateFromTimeZone 验证地区与时区分开。
//
// 同一个时刻、同一个日历日，在两个地区下的判定不同——
// 这正是为什么地区不能由时区推导、时区也不能由规则写死。
func TestEvalRegionIsSeparateFromTimeZone(t *testing.T) {
	p := mustCompile(t, `{"prompt": isWorkday() ? 1.0 : 2.0}`)
	cal := newTestCalendar()

	cn, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	us, err := p.Eval(Env{At: testAt, Region: "US", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	// 两个地区都给出「工作日」，因为 10 月 16 日在北京是普通工作日、
	// 在纽约的日历里是调休补班日——**补班日也是工作日**。
	// 这条正是要说明：判定依据是地区日历，而不是「哪个时区的周五」。
	if cn.Prices.Prompt.String() != "1.00000000" {
		t.Errorf("CN: %s", cn.Prices.Prompt)
	}
	if us.Prices.Prompt.String() != "1.00000000" {
		t.Errorf("US: %s", us.Prices.Prompt)
	}

	// 换一个日历日：纽约 10 月 1 日是休息日，北京同一天是国庆节。
	// 两个地区的判定必须来自各自查到的日历数据。
	p2 := mustCompile(t, `{"prompt": isWorkday() ? 1.0 : 2.0}`)
	first := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	cn1, err := p2.Eval(Env{At: first, Region: "CN", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	us1, err := p2.Eval(Env{At: first, Region: "US", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	if cn1.Prices.Prompt.String() != "2.00000000" || us1.Prices.Prompt.String() != "2.00000000" {
		t.Errorf("10 月 1 日两个地区都应当是休息日: CN=%s US=%s",
			cn1.Prices.Prompt, us1.Prices.Prompt)
	}

	// 时区仍然只决定「现在几点」：同一时刻在两个时区的小时数不同。
	p3 := mustCompile(t, `{"prompt": hour("Asia/Shanghai") == hour("UTC") + 8 ? 1.0 : 2.0}`)
	h, err := p3.Eval(Env{At: testAt})
	if err != nil {
		t.Fatal(err)
	}
	if h.Prices.Prompt.String() != "1.00000000" {
		t.Errorf("两个时区的小时数应当相差 8: %s", h.Prices.Prompt)
	}
}

// TestEvalFixedAndMinimum 验证一次性费用的两个通道。
func TestEvalFixedAndMinimum(t *testing.T) {
	res := eval(t, `{
		"prompt": 0.0000015,
		"fixed": fixed(0.002),
		"minimum": minimum(0.05)
	}`, billing.Usage{})
	if got := res.Charges.Fixed.String(); got != "0.00200000" {
		t.Errorf("fixed = %s", got)
	}
	if got := res.Charges.Minimum.String(); got != "0.05000000" {
		t.Errorf("minimum = %s", got)
	}

	// 结果形状不受影响：fixed 返回的是数值，不是一笔费用对象。
	if res.Prices.Prompt.String() != "0.00000150" {
		t.Errorf("prompt = %s", res.Prices.Prompt)
	}
}

// TestEvalNoWallClock 验证规则读不到墙钟。
//
// 这是本包最要紧的一条不变量：预留与结算必须能给出同一个答案。
// 一个 now() 就会让跨整点的请求按低谷预留、按高峰结算。
func TestEvalNoWallClock(t *testing.T) {
	for _, source := range []string{
		`{"prompt": now() ? 1.0 : 2.0}`,
		`{"prompt": date("2006-01-02")}`,
		`{"prompt": duration("1h")}`,
		`{"prompt": timezone("Asia/Shanghai")}`,
	} {
		_, err := Compile(source)
		if err == nil {
			t.Errorf("%q 用到了墙钟函数却编过了", source)
			continue
		}
		if !strings.Contains(err.Error(), "unknown name") {
			t.Errorf("%q 的错误信息应当说明名字未知，得到: %v", source, err)
		}
	}
}

// TestEvalBuiltinWhitelist 验证内置函数的白名单是真的白名单。
func TestEvalBuiltinWhitelist(t *testing.T) {
	allowed := map[string]string{
		"max":   `{"prompt": max(1.0, 2.0)}`,
		"min":   `{"prompt": min(1.0, 2.0)}`,
		"abs":   `{"prompt": abs(1.0)}`,
		"ceil":  `{"prompt": ceil(1.5)}`,
		"floor": `{"prompt": floor(1.5)}`,
		"round": `{"prompt": round(1.5)}`,
		"int":   `{"prompt": int(1.5)}`,
		"float": `{"prompt": float(1)}`,
	}
	for name, source := range allowed {
		if _, err := Compile(source); err != nil {
			t.Errorf("白名单里的 %s 编不过: %v", name, err)
		}
	}

	// 这些要么改变值类型（把字符串变成对象），要么走浮点边界，
	// 要么是数组操作——都不该出现在计价规则里。
	blocked := []string{
		`{"prompt": toJSON(1)}`,
		`{"prompt": fromJSON("{}")}`,
		`{"prompt": toBase64("a")}`,
		`{"prompt": fromBase64("YQ==")}`,
		`{"prompt": sum([1,2])}`,
		`{"prompt": mean([1,2])}`,
		`{"prompt": median([1,2])}`,
		`{"prompt": first([1,2])}`,
		`{"prompt": map([1,2], # > 0)}`,
		`{"prompt": reduce([1,2], #acc + #, 0)}`,
		`{"prompt": all([1,2], # > 0)}`,
		`{"prompt": groupBy([1,2], #)}`,
		`{"prompt": sortBy([1,2], #)}`,
		`{"prompt": sort([1,2])}`,
	}
	for _, source := range blocked {
		if _, err := Compile(source); err != nil {
			continue
		}
		t.Errorf("%q 用到了未放行的内置函数却编过了", source)
	}
}

// TestEvalGlobals 验证注入常量。
func TestEvalGlobals(t *testing.T) {
	source := `{"prompt": plan == "pro" ? 0.0000015 : 0.0000030}`
	p, err := CompileWithGlobals(source, map[string]any{"plan": ""})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Eval(Env{At: testAt, Globals: map[string]any{"plan": "pro"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Prices.Prompt.String() != "0.00000150" {
		t.Errorf("prompt = %s", res.Prices.Prompt)
	}

	// 类型对不上时不能编过：`plan <= 3` 配上字符串常量是明显的笔误。
	if _, err := CompileWithGlobals(`{"prompt": plan <= 3 ? 1.0 : 2.0}`,
		map[string]any{"plan": ""}); err == nil {
		t.Error("字符串常量参与了数值比较却编过了")
	}

	// 名字撞上保留名时拒绝：一个叫 p 的常量会让规则的含义随调用方而变。
	if _, err := CompileWithGlobals(`{"prompt": p}`, map[string]any{"p": 1}); err == nil {
		t.Error("注入常量占用了保留名 p 却编过了")
	}
	if _, err := CompileWithGlobals(`{"prompt": 1.0}`,
		map[string]any{"tier": "x"}); err == nil {
		t.Error("注入常量占用了函数名却编过了")
	}
}

// TestEvalConcurrent 用 -race 验证「编译一次、并发求值多次」。
//
// 这条测试是包内最要紧的并发保障：编译产物必须无状态，
// 而 tier/fixed/minimum 的累加器必须落在每次求值自己的 runtime 里。
// 如果哪次改动把闭包挪进了编译期（例如改用 expr.Function 注册），
// 这条测试会报 data race。
func TestEvalConcurrent(t *testing.T) {
	source := `len <= 32000
		? tier("standard", {
			"prompt": 0.0000015,
			"completion": hour("Asia/Shanghai") >= 17 ? 0.0000090 : 0.0000045,
			"units": {"search": 0.01},
			"fixed": fixed(0.002)
		})
		: tier("long_context", {
			"prompt": 0.0000030,
			"completion": 0.0000135,
			"units": {"search": 0.02},
			"fixed": fixed(0.004)
		})`
	p := mustCompile(t, source)
	cal := newTestCalendar()

	const workers = 16
	const rounds = 200
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			for i := 0; i < rounds; i++ {
				usage := billing.Usage{PromptTokens: int64(1000 + i)}
				if w%2 == 0 {
					// 一半的 goroutine 走长上下文档，确保两条分支被同时求值。
					usage = billing.Usage{PromptTokens: 400_000}
				}
				res, err := p.Eval(Env{
					Usage:    usage,
					At:       testAt,
					Region:   "CN",
					Calendar: cal,
				})
				if err != nil {
					errs <- err
					return
				}
				// 每个 goroutine 都必须看到与自己那次用量相称的档位，
				// 而不是别的 goroutine 留下来的。
				want := "standard"
				if w%2 == 0 {
					want = "long_context"
				}
				if res.Tier != want {
					errs <- &tierMismatch{got: res.Tier, want: want}
					return
				}
			}
			errs <- nil
		}(w)
	}
	for w := 0; w < workers; w++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

type tierMismatch struct{ got, want string }

func (e *tierMismatch) Error() string { return "档位串了: " + e.got + " != " + e.want }

// TestEvalUsageIsNotAMultiplier 钉住「用量只作条件、不作乘数」这条约定。
//
// 用量作乘数在语法与类型上都合法（`p * 1.5` 是个合法的数值表达式），
// 破坏的是**求值结果的性质**：单价随用量变化，于是最坏情况求值给出的
// 上界不再成立，而「余额不为负」依赖那个上界。所以拦它的位置不是类型检查，
// 而是按 AST 上下文判的结构检查（见 usageInArithmetic）。
//
// 三种形态都要拦：包一层函数、直接当结果、藏在条件的另一边。
func TestEvalUsageIsNotAMultiplier(t *testing.T) {
	// 两类错会在这里被拦下，**错误信息必须说清是哪一类**：
	//
	//   - 用量进了算术位置 → 说「价格位置」与「条件」，告诉作者怎么改
	//   - 结果根本不是一张单价表 → 说「单价表」，那是形状问题不是约定问题
	//
	// 前者是规则作者会反复踩的，所以它的提示要具体到「改成条件式」；
	// 后者是结构性写错（`p * 1.5` 直接当结果），报「不是单价表」就够。
	cases := []struct {
		source string
		want   []string
	}{
		{`{"prompt": 0.0000015 * float(p)}`, []string{"价格位置", "用量", "条件"}},
		{`{"prompt": (p > 0 ? 1.5 : 0) * c}`, []string{"价格位置", "用量", "条件"}},
		{`{"prompt": p}`, []string{"价格位置", "用量", "条件"}},
		// 直接当结果：类型检查先于结构检查拦下，报的是「期望一张表」。
		// 这里不断言具体措辞——那句话来自 expr 引擎，改引擎版本会变；
		// 要钉的是「它被拦下了」，以及另一组用例钉住的
		// 「用量作条件时不该被拦」。
		{`p * 1.5`, []string{"计价规则编译失败"}},
	}
	for _, tc := range cases {
		_, err := Compile(tc.source)
		if err == nil {
			t.Errorf("%q 把用量写进了价格位置却编过了", tc.source)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q 的错误信息里缺少 %q: %v", tc.source, want, err)
			}
		}
	}

	// 用量作条件是正常写法，不能被一起拒掉。
	for _, source := range []string{
		`{"prompt": p > 0 ? 1.5 : 0}`,
		`{"prompt": (p > 0 ? 1.5 : 2.5)}`,
		`{"prompt": p > 0 && len > 100 ? 1.5 : 0}`,
	} {
		if _, err := Compile(source); err != nil {
			t.Errorf("%q 里的用量只作条件，却编不过: %v", source, err)
		}
	}
}

// TestEvalAmountRounding 验证舍入只发生一次，而且发生在转 money.Amount 那里。
func TestEvalAmountRounding(t *testing.T) {
	// 一个超过 8 位小数的单价：money.Parse 会四舍五入到 8 位。
	res := eval(t, `{"prompt": 0.000000015}`, billing.Usage{})
	if got := res.Prices.Prompt.String(); got != "0.00000002" {
		t.Errorf("超过 8 位小数应当四舍五入到 8 位: %s", got)
	}

	// 之后乘以用量是整数运算，不再产生新的舍入。
	b := billing.Compute(billing.Usage{PromptTokens: 3}, res.Prices)
	if got := b.Prompt.String(); got != "0.00000006" {
		t.Errorf("单价 × 用量 = %s，期望 0.00000006", got)
	}
	if err := b.Verify(); err != nil {
		t.Errorf("明细不自洽: %v", err)
	}
}

// TestEvalZeroPrices 验证零单价是允许的：免费档是正常规则。
func TestEvalZeroPrices(t *testing.T) {
	res := eval(t, `{"prompt": 0.0, "completion": 0}`, billing.Usage{})
	if !res.Prices.Prompt.IsZero() || !res.Prices.Completion.IsZero() {
		t.Errorf("零单价应当被接受: %+v", res.Prices)
	}
}

// TestEvalResultIsNotNegative 验证负数单价被拦下。
func TestEvalResultIsNotNegative(t *testing.T) {
	p := mustCompile(t, `{"prompt": -0.0000015}`)
	if _, err := p.Eval(Env{At: testAt}); err == nil {
		t.Error("负单价应当报错")
	}

	p2 := mustCompile(t, `{"fixed": fixed(-0.01)}`)
	if _, err := p2.Eval(Env{At: testAt}); err == nil {
		t.Error("负的固定费用应当报错")
	}
}

// TestEvalRejectsNonFinite 验证 NaN/Inf 不会流进金额。
func TestEvalRejectsNonFinite(t *testing.T) {
	for _, source := range []string{
		`{"prompt": 0.0 / 0.0}`,
		`{"prompt": 1.0 / 0.0}`,
	} {
		p := mustCompile(t, source)
		_, err := p.Eval(Env{At: testAt})
		if err == nil {
			t.Errorf("%q 算出了非有限的单价却没报错", source)
			continue
		}
		if !strings.Contains(err.Error(), "NaN") && !strings.Contains(err.Error(), "无穷") {
			t.Errorf("%q 的错误信息应当说明是 NaN/无穷，得到: %v", source, err)
		}
	}
}

// TestEvalRejectsWrongTypes 验证非数值的单价被拦下。
func TestEvalRejectsWrongTypes(t *testing.T) {
	p := mustCompile(t, `{"prompt": "1.5"}`)
	if _, err := p.Eval(Env{At: testAt}); err == nil {
		t.Error("字符串单价应当报错")
	}

	p2 := mustCompile(t, `{"prompt": true}`)
	if _, err := p2.Eval(Env{At: testAt}); err == nil {
		t.Error("布尔单价应当报错")
	}

	// units 不是表在**编译期**就该报错：它是一张计量项表而不是一个单价，
	// 留到运行期才发现会让商家以为规则能用。
	if _, err := Compile(`{"units": 1.5}`); err == nil {
		t.Error("units 不是表时应当报错")
	}
}

// TestEvalRejectsIncompleteEnv 验证 Env 缺信息时报错而不是用零值顶上。
func TestEvalRejectsIncompleteEnv(t *testing.T) {
	p := mustCompile(t, `{"prompt": 0.0000015}`)

	if _, err := p.Eval(Env{}); err == nil {
		t.Error("没有 At 时应当报错")
	}
	if _, err := p.Eval(Env{At: testAt, Usage: billing.Usage{PromptTokens: -1}}); err == nil {
		t.Error("负用量应当报错")
	}
	if _, err := p.Eval(Env{At: testAt,
		Usage: billing.Usage{UnitCounts: map[string]int64{"search": -1}}}); err == nil {
		t.Error("负计量项应当报错")
	}

	// 日历函数在没有 Calendar 时必须报错，而不是当成「不是工作日」——
	// 静默取假会让所有节假日都按工作日计价。
	p2 := mustCompile(t, `{"prompt": isWorkday() ? 1.0 : 2.0}`)
	if _, err := p2.Eval(Env{At: testAt}); err == nil {
		t.Error("没有 Calendar 时应当报错")
	}
	if _, err := p2.Eval(Env{At: testAt, Region: "", Calendar: newTestCalendar()}); err == nil {
		t.Error("没有 Region 时应当报错")
	}
}

// TestEvalUnknownUnitNameSilentlyZero 记录一个已知的口径。
//
// units 是字符串键的表，编译期看不到运行时会用哪些键，
// 所以拼错的计量项名不会报错，只会取到 0。这条测试把行为写下来，
// 避免后来者以为它是缺陷而「修」成别的样子。
func TestEvalUnknownUnitNameSilentlyZero(t *testing.T) {
	res := eval(t, `{"prompt": units["imges"] > 0 ? 1.0 : 2.0}`, billing.Usage{})
	if res.Prices.Prompt.String() != "2.00000000" {
		t.Errorf("拼错的计量项名应当取到 0（走假分支）: %s", res.Prices.Prompt)
	}
}

// TestEvalTierEmptyNameRejected 验证空档位名被拒绝。
func TestEvalTierEmptyNameRejected(t *testing.T) {
	p := mustCompile(t, `{"prompt": tier("", 1.0)}`)
	if _, err := p.Eval(Env{At: testAt}); err == nil {
		t.Error("空档位名应当报错：账单上要靠它回答「命中了哪一档」")
	}
}

// TestEvalTierOnWholeTable 验证 tier 可以包住整张单价表。
func TestEvalTierOnWholeTable(t *testing.T) {
	res := eval(t, `tier("peak", {"prompt": 0.0000020, "completion": 0.0000090})`,
		billing.Usage{})
	if res.Tier != "peak" {
		t.Errorf("tier = %q", res.Tier)
	}
	if res.Prices.Prompt.String() != "0.00000200" {
		t.Errorf("prompt = %s", res.Prices.Prompt)
	}
}

// TestEvalProgramMetadata 验证 Program 的元信息。
func TestEvalProgramMetadata(t *testing.T) {
	source := `len <= 32000 ? {"prompt": 1.0} : {"prompt": 2.0, "completion": 3.0}`
	p := mustCompile(t, source)

	if p.Source() != source {
		t.Errorf("Source 应当返回原始源码")
	}
	if p.BranchCount() != 1 {
		t.Errorf("分支数 = %d，期望 1", p.BranchCount())
	}
	// 键取并集：未命中的分支写了什么也要看到，
	// 否则「未命中的那支写错键」会一直躲过检查。
	keys := p.Keys()
	if len(keys) != 2 || keys[0] != "completion" || keys[1] != "prompt" {
		t.Errorf("键应当是两个分支的并集，得到 %v", keys)
	}
}

// TestEvalUnknownSlotInUnvisitedBranch 验证未命中分支里的错键也在编译期报出来。
func TestEvalUnknownSlotInUnvisitedBranch(t *testing.T) {
	_, err := Compile(`len <= 32000
		? {"prompt": 1.0}
		: {"promt": 2.0}`)
	if err == nil {
		t.Fatal("未命中分支里的错键应当在编译期报出来")
	}
	if !strings.Contains(err.Error(), "promt") {
		t.Errorf("错误信息应当指出错的键名: %v", err)
	}
}

// TestEvalMoneyAmountsAreExact 验证同一组单价在多次求值下给出完全相同的金额。
func TestEvalMoneyAmountsAreExact(t *testing.T) {
	source := `{"prompt": 0.0000015, "completion": 0.0000075}`
	p := mustCompile(t, source)
	usage := billing.Usage{PromptTokens: 12345, CompletionTokens: 678}

	var first money.Amount
	for i := 0; i < 50; i++ {
		res, err := p.Eval(Env{Usage: usage, At: testAt})
		if err != nil {
			t.Fatal(err)
		}
		b := billing.Compute(usage, res.Prices)
		if i == 0 {
			first = b.Total
			continue
		}
		if b.Total.Cmp(first) != 0 {
			t.Fatalf("第 %d 次求值得到 %s，第一次是 %s", i, b.Total, first)
		}
	}
}
