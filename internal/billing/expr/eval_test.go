package expr

import (
	"errors"
	"fmt"
	"math"
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
//
// 峰谷那一半同样只查写死的表（peakMinutes），**不自己算窗口**：窗口怎么算、
// 跨零点怎么算的判定在 internal/calendar 里测（那边的 PeakAt）。这一层要证明的
// 是：规则里写的窗口、地区、时刻原样传到了日历上，而日历的答案决定用哪一档价。
// 所以它把收到的入参记在 saw 里，供用例断言。
type testCalendar struct {
	tz    map[string]string
	days  map[string]DayInfo
	fails bool

	// peakMinutes 是这一天里算高峰的当地分钟数。
	peakMinutes map[int]bool
	// saw 记下 Peak 收到的入参。
	saw []peakCall
}

// peakCall 是一次 Peak 调用的入参。
type peakCall struct {
	region  string
	instant time.Time
	windows []PeakWindow
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
	if c.fails {
		return DayInfo{}, ErrCalendarRequired
	}
	info, ok := c.days[region+"|"+day.Format("2006-01-02")]
	if !ok {
		return DayInfo{}, ErrCalendarRequired
	}
	return info, nil
}

func (c *testCalendar) Peak(region string, instant time.Time, windows []PeakWindow) (bool, error) {
	c.saw = append(c.saw, peakCall{region: region, instant: instant, windows: windows})
	if c.fails {
		return false, ErrCalendarRequired
	}
	loc, err := time.LoadLocation(c.tz[region])
	if err != nil {
		return false, ErrCalendarRequired
	}
	at := instant.In(loc)

	// 这一天不在表里就是「未知」——与 DayInfo 同一个信号（真实实现里那是
	// DayUnknown，由适配器折成 ErrCalendarRequired）。判不出来时报同一个错，
	// 不回答 false：false 是「按低谷算」这个具体的判断。
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	if _, ok := c.days[region+"|"+day.Format("2006-01-02")]; !ok {
		return false, fmt.Errorf("%w: 这一天的高峰与否判不出来", ErrCalendarRequired)
	}
	return c.peakMinutes[at.Hour()*60+at.Minute()], nil
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
	if res.Charges != (billing.Charges{}) {
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

// TestEveryTokenSlotComesFromBilling 钉住「token 槽位只有一份清单」。
//
// 规则源码按 billing.TokenSlots 动态生成：加一个槽位时**不需要改这条测试**，
// 但任何一处仍在手抄清单都会立刻挂——新槽位要么被结果键白名单当成未知键
// 拒绝（priceSlots 没派生），要么没被记成「规则写过」（Declared 没派生），
// 要么单价写到了别的字段上（SetPrice 指错）。
//
// 每个槽位给不同的价：写错字段时不会碰巧相等。
//
// 变异：`priceSlots` 那份表在建的时候写的是
// `len(billing.TokenSlots)+len(nonTokenPriceSlots)`，它的 `+` 改成 `-`
// 只动 map 的**容量提示**（5 + 3 与 5 - 3），键与值一个都不变。
// 没有哪一组输入能让两版不同，所以那一条是等价变异，抓不到。
func TestEveryTokenSlotComesFromBilling(t *testing.T) {
	parts := make([]string, 0, len(billing.TokenSlots))
	want := make(map[string]money.Amount, len(billing.TokenSlots))
	for i, slot := range billing.TokenSlots {
		price := money.MustParse(fmt.Sprintf("0.%08d", i+1))
		parts = append(parts, fmt.Sprintf("%q: %s", slot.Name, price))
		want[slot.Name] = price
	}

	res := eval(t, "{"+strings.Join(parts, ", ")+"}", billing.Usage{})

	if len(res.Prices.Declared) != len(billing.TokenSlots) {
		t.Errorf("Declared 有 %d 项，TokenSlots 有 %d 项",
			len(res.Prices.Declared), len(billing.TokenSlots))
	}
	for _, slot := range billing.TokenSlots {
		if !res.Prices.Declared[slot.Name] {
			t.Errorf("规则写了 %q，Declared 里却没有", slot.Name)
		}
		if got := slot.Price(res.Prices); got.Cmp(want[slot.Name]) != 0 {
			t.Errorf("%s 的单价 = %s，期望 %s", slot.Name, got, want[slot.Name])
		}
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

// TestEvalContextLengthCountsCacheWrite 验证 len 把缓存写进来的那一份也算在内。
//
// 缓存写同样占着窗口，只是单价与正文不同——它属于「送进去的上下文」。
// 少算这一份会让一个 35 000 token 的请求落回短上下文档、取到偏低的单价：
// 档位看着正常，而账单上少收的那一笔不会报错。
//
// 期望值写成字面量，不从规则文本里推：分档线与单价都是规则里写着的数，
// 拿它们当参照，改规则时两边会一起变。
func TestEvalContextLengthCountsCacheWrite(t *testing.T) {
	source := `len <= 32000
		? tier("standard", {"prompt": 0.0000015, "completion": 0.0000075})
		: tier("long_context", {"prompt": 0.0000030, "completion": 0.00001125})`

	// 30 000 + 5 000 = 35 000，越过 32 000 那条分档线。
	res := eval(t, source, billing.Usage{PromptTokens: 30_000, CacheWriteTokens: 5_000})
	if res.Tier != "long_context" || res.Prices.Prompt.String() != "0.00000300" {
		t.Errorf("30 000 正文加 5 000 缓存写的上下文: tier=%q prompt=%s，期望 long_context 与 0.00000300",
			res.Tier, res.Prices.Prompt)
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

// checkWindows 逐段比对规则里写的窗口解析成了什么。
//
// 逐段比而不是整个切片一起比：差一段、差一分钟时，报出来的那一行会写明是第几段、
// 期望多少、实际多少——那一行就是排查要的全部信息。
func checkWindows(t *testing.T, got, want []PeakWindow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("解析出 %d 段窗口 %+v，期望 %d 段 %+v", len(got), got, len(want), want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 段窗口 = %+v，期望 %+v", i+1, got[i], w)
		}
	}
}

// TestEvalIsPeak 钉住 isPeak() 的两端：规则里写的高峰时段原样传到日历上，
// 而日历给出的答案决定用哪一档价。
//
// 判定本身（窗口怎么算、跨零点算到哪一天）在 internal/calendar 里测；这里要
// 证明的是接得上——传下去的是哪三样东西、答案怎么变成价。三样缺一样，
// 判定就会在别处悄悄错：窗口少了它按别的时段算，地区错了它按别的日历算，
// 时刻错了它按别的钟点算。
func TestEvalIsPeak(t *testing.T) {
	// testAt 是北京时间 10 月 16 日 17:30，下面让这一分钟算高峰。
	cal := newTestCalendar()
	cal.peakMinutes = map[int]bool{17*60 + 30: true}

	p := mustCompile(t, `{"prompt": isPeak("08:00-22:00") ? 0.0000030 : 0.0000010}`)
	res, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Prices.Prompt.String(); got != "0.00000300" {
		t.Errorf("高峰时段里的单价 = %s，期望 0.00000300", got)
	}

	if len(cal.saw) != 1 {
		t.Fatalf("日历收到 %d 次峰谷判定，期望 1 次", len(cal.saw))
	}
	call := cal.saw[0]
	if call.region != "CN" {
		t.Errorf("传给日历的地区 = %q，期望 CN（它来自 Env.Region，不由规则声明）", call.region)
	}
	if !call.instant.Equal(testAt) {
		t.Errorf("传给日历的时刻 = %v，期望 %v（求值时刻，不是墙钟）", call.instant, testAt)
	}
	checkWindows(t, call.windows, []PeakWindow{{StartMinute: 8 * 60, EndMinute: 22 * 60}})

	// 同一份日历、同一时刻，只把它认的高峰分钟换掉：价跟着换档。
	// 这一半是必要的——一条永远走高峰档的规则也能通过上面那个断言。
	cal.peakMinutes = map[int]bool{}
	res, err = p.Eval(Env{At: testAt, Region: "CN", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Prices.Prompt.String(); got != "0.00000100" {
		t.Errorf("高峰时段之外的单价 = %s，期望 0.00000100", got)
	}
}

// TestEvalIsPeakParsesWindows 钉住窗口串的写法：多段用逗号分开，
// 跨零点由「终点小于起点」表达。
//
// 解析结果走日历收到的入参来看——分钟数是规则与日历之间唯一的约定，
// 而它是这里唯一能证伪的地方（日历那边只看到两个整数）。
func TestEvalIsPeakParsesWindows(t *testing.T) {
	cal := newTestCalendar()
	cal.peakMinutes = map[int]bool{}
	p := mustCompile(t, `{"prompt": isPeak(" 08:00-12:00 , 22:00-02:00 ") ? 1.0 : 2.0}`)
	if _, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: cal}); err != nil {
		t.Fatal(err)
	}

	checkWindows(t, cal.saw[0].windows, []PeakWindow{
		{StartMinute: 8 * 60, EndMinute: 12 * 60},
		// 22:00 到次日 02:00：终点小于起点。
		{StartMinute: 22 * 60, EndMinute: 2 * 60},
	})
}

// TestPeakWindowKeepsBothEndsOfTheDay 把窗口串两头边界上那一格固定下来：
// 一天之内的钟点**包含** 00:00 与 23:59，而分钟数要折成从零点起算的那个数。
//
// 这条拿一天的两端当输入，把同一个表达式里的两支分开判：
//
//   - `h > 23` 改成 `>= 23`、`m > 59` 改成 `>= 59`：`23:59` 会被当成
//     「不是一天之内的钟点」，这一条在编译或求值时报错（两个入口都校验写法）。
//   - `h < 0` 改成 `<= 0`、`m < 0` 改成 `<= 0`：正好是零点的那一格被当成越界。
//   - `h*60 + m` 的 `+` 改成 `-` 不报错，但 `23:59` 折成 1321 而不是 1439——
//     落在下面那段等值断言上。
//
// 断言写成两个字面量而不是 `23*60+59` 这种算式：算式与被测的那一行是同一个
// 写法，`+` 改成 `-` 时两边一起变，用例照样通过。
func TestPeakWindowKeepsBothEndsOfTheDay(t *testing.T) {
	cal := newTestCalendar()
	cal.peakMinutes = map[int]bool{}
	p := mustCompile(t, `{"prompt": isPeak("23:59-00:00, 00:00-23:59") ? 1.0 : 2.0}`)
	if _, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: cal}); err != nil {
		t.Fatal(err)
	}

	checkWindows(t, cal.saw[0].windows, []PeakWindow{
		{StartMinute: 1439, EndMinute: 0}, // 23:59 到当天结束（次日零点）
		{StartMinute: 0, EndMinute: 1439}, // 从零点到 23:59
	})
}

// TestEvalIsPeakUnknownDayFails 钉住 unknown 的口径：**报错，不猜**。
//
// 放假安排还没公布的那一天，峰谷也判不出来（PeakAt 的三态里那个 unknown）。
// 处置与 isWorkday() 是同一条：它们报的必须是同一个错。回落成 false 会在
// 春节期间按低谷价收费，而账单看上去完全正常——账错得看不出来是最坏的一种。
func TestEvalIsPeakUnknownDayFails(t *testing.T) {
	// 这一天没有日历数据：isWorkday() 与 isPeak() 都判不出来。
	cal := newTestCalendar()
	delete(cal.days, "CN|2026-10-16")

	p := mustCompile(t, `{"prompt": isPeak("08:00-22:00") ? 0.0000030 : 0.0000010}`)
	res, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: cal})
	if err == nil {
		t.Fatalf("日历判不出峰谷时按某一档算了价: %+v", res.Prices)
	}
	if !errors.Is(err, ErrCalendarRequired) {
		t.Errorf("峰谷判不出来时报的是 %v，期望 ErrCalendarRequired", err)
	}

	// 同一份日历上 isWorkday() 报同一个错：两条口径不一致的话，
	// 规则作者会以为「峰谷那一条特别容易出错」，而实际是同一件事。
	pw := mustCompile(t, `{"prompt": isWorkday() ? 0.0000030 : 0.0000010}`)
	_, werr := pw.Eval(Env{At: testAt, Region: "CN", Calendar: cal})
	if werr == nil {
		t.Fatal("同一天 isWorkday() 也该判不出来")
	}
	if !errors.Is(werr, ErrCalendarRequired) {
		t.Errorf("isWorkday() 报的是 %v，期望 ErrCalendarRequired", werr)
	}
}

// TestEvalIsPeakWorstCaseCoversBothBranches 钉住最坏情况那一侧。
//
// 预留是上界：峰谷这一档的规则必须让两支都被覆盖到，否则一笔高峰请求按
// 低谷预留，结算时撞上 Capped——而 Capped 本该是「上界算错了」的信号。
func TestEvalIsPeakWorstCaseCoversBothBranches(t *testing.T) {
	cal := newTestCalendar() // 17:30 不在 08:00–12:00 里：普通求值走低谷档
	p := mustCompile(t, `isPeak("08:00-12:00")
		? tier("peak", {"prompt": 0.0000030})
		: tier("off_peak", {"prompt": 0.0000010})`)

	env := Env{Usage: billing.Usage{PromptTokens: 1}, At: testAt, Region: "CN", Calendar: cal}
	actual, err := p.Eval(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := actual.Prices.Prompt.String(); got != "0.00000100" {
		t.Errorf("低谷档的单价 = %s，期望 0.00000100", got)
	}

	worst, err := p.EvalWorstCase(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := worst.Prices.Prompt.String(); got != "0.00000300" {
		t.Errorf("上界 = %s，期望高峰那一档 0.00000300", got)
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

// 注入常量的类型不受支持时，要在**校验**这一步就被拒掉。
//
// # 这一格为什么用类型，而不是用保留名
//
// 保留名有两道判据同时挡：env.go 的 globalType 一次，eval.go 往运行期环境
// 填值时又一次（理由见那里的注释：编译产物可能来自缓存）。所以拿保留名当
// 夹具，把 globalType 那一处改坏也照样报 ErrGlobalsInvalid，用例一直绿——
// 它证明不了自己声称的那件事。「类型不受支持」只有校验这一处看得见。
//
// 放过的后果：那个值会被原样塞进运行期环境交给 vm.Run。一个不在支持类型里
// 的 Go 值到不了规则那一侧——要么在求值中途以与规则无关的形状炸（读的人
// 会去查规则，而规则没写错），要么静默按零值参与计价。
//
// 变异：`if _, err := globalType(name, v); err != nil` 改成 `err == nil`。
func TestEvalRejectsUnsupportedGlobalType(t *testing.T) {
	p := mustCompile(t, `{"prompt": 0.0000015}`)

	// int32 不在这份支持清单里（globalType 只放行 string、int/int64、float64、
	// bool、[]string、time.Time），而名字不撞保留名——只有校验这一处会拒它。
	_, err := p.Eval(Env{At: testAt, Globals: map[string]any{"plan": int32(3)}})
	if !errors.Is(err, ErrGlobalsInvalid) {
		t.Fatalf("类型不受支持的注入常量应当报 ErrGlobalsInvalid，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "plan") || !strings.Contains(err.Error(), "int32") {
		t.Errorf("报错要指出是哪个常量、什么类型，得到 %v", err)
	}

	// 保留名这一格在求值时同样要挡住——它杀不掉任何一条单独的变异（两处
	// 各挡一次），留着是为了钉住行为：注入常量不能盖住保留名，否则规则的
	// 含义会随调用方的数据而变，而规则源码看起来没变。
	_, err = p.Eval(Env{At: testAt, Globals: map[string]any{"p": 1}})
	if !errors.Is(err, ErrGlobalsInvalid) {
		t.Fatalf("注入常量占用保留名 p 应当报 ErrGlobalsInvalid，得到 %v", err)
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
	b := billing.ComputeWithCharges(billing.Usage{PromptTokens: 3}, res.Prices, billing.Charges{})
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

// TestEvalRejectsNonFiniteFixedFee 验证一次性费用也走「必须有限」那一关。
//
// 单价那一侧由 asPrice 拦下，而 fixed() 走的是副作用通道、落到 chargeOf。
// 那里若把非有限值放过去，这笔费用会静默变成零：规则里写了固定费，
// 账单上一分不收，而且不报错——账目仍然平，只有对账时表现为
// 「用量对不上金额」。
func TestEvalRejectsNonFiniteFixedFee(t *testing.T) {
	// 一次性的固定费与用量无关，所以这里给的用量是空的：
	// 这条规则该不该收钱只看那一项费用本身。
	p := mustCompile(t, `{"prompt": min(fixed(1.0 / 0.0), 1.0)}`)
	res, err := p.Eval(Env{At: testAt})
	if err == nil {
		t.Fatalf("非有限的固定费没报错，算出来的 fixed = %s（期望报「不是有限数值」）", res.Charges.Fixed)
	}
	if !strings.Contains(err.Error(), `"fixed"`) || !strings.Contains(err.Error(), "不是有限数值") {
		t.Errorf("错误信息应当说清是哪一项费用非法，得到: %v", err)
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

// 计量项数量为 0 是合法输入：它既不表示「没填」，也不是负数。
//
// 0 很常见——某个计量项这次没产出（`{"img": 0}`），或者按规则算出来正好是 0。
// 把 0 与负数一起挡掉，拒的是**合法请求**，而错误信息说「数量为负」，
// 调用方看不出自己哪里错。
//
// 判据只有这一处（env.go 的 validateUsage），所以这一格钉的就是它。
//
// 变异：`n < 0` 改成 `n <= 0`。
func TestEvalAcceptsZeroUnitCount(t *testing.T) {
	p := mustCompile(t, `{"prompt": units["img"] > 0 ? 1.0 : 2.0}`)

	res, err := p.Eval(Env{At: testAt,
		Usage: billing.Usage{UnitCounts: map[string]int64{"img": 0}}})
	if err != nil {
		t.Fatalf("数量为 0 的计量项是合法输入，不该被拒: %v", err)
	}
	// 要的是「取到 0」而不是「被拒」：0 走的是没有图片的那一支。
	if got := res.Prices.Prompt.String(); got != "2.00000000" {
		t.Errorf("prompt = %s，期望走「没有图片」那一支的 2.0", got)
	}
}

// 负数仍然是非法：上面那一格的另一侧，0 放行不等于整条判据可以去掉。
func TestEvalRejectsNegativeUnitCount(t *testing.T) {
	p := mustCompile(t, `{"prompt": units["img"] > 0 ? 1.0 : 2.0}`)

	_, err := p.Eval(Env{At: testAt,
		Usage: billing.Usage{UnitCounts: map[string]int64{"img": -1}}})
	if !errors.Is(err, ErrEnvIncomplete) {
		t.Fatalf("数量为负应当报 ErrEnvIncomplete，得到 %v", err)
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
//
// 变异：`keysOf` 建那个并集时写的是 `len(p.keys)+len(table)`，它的 `+`
// 改成 `-` 同样只动容量提示（差为负时运行时按 0 处理，不会 panic），
// 键的并集与排序都不受影响——没有哪一组输入能让两版不同，等价变异。
func TestEvalProgramMetadata(t *testing.T) {
	source := `len <= 32000 ? {"prompt": 1.0} : {"prompt": 2.0, "completion": 3.0}`
	p := mustCompile(t, source)

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

// TestEvalUnitPriceClashIsOrderIndependent 钉住「计量项名与单价键名撞名」
// 这条判据与 map 的迭代顺序无关。
//
// 判据原先查的是 floats 里有没有这个名字，而 floats 与 units 是在**同一个
// `range table` 循环**里填的——于是「先迭代到 units 还是先迭代到 prompt」
// 决定了这条规则报不报错。table 是 map、迭代顺序随机，所以同一条规则
// 有时报错、有时静默通过。
//
// 跑 1000 次是刻意的：实测修之前单次**静默放过**的概率约 0.88
// （2000 次里放过 1757 次），也就是大多数求值根本不报错——只跑一次的用例
// 大概率是绿的。连着放过 1000 次的概率是 0.88^1000，所以这条用例在判据
// 修好之前**必然**红。
func TestEvalUnitPriceClashIsOrderIndependent(t *testing.T) {
	// prompt 既是 token 槽位、又被写成了一个计量项：这一条必须每次都拦下。
	p := mustCompile(t, `{"prompt": 1.5, "units": {"prompt": 0.01}}`)
	for i := 0; i < 1000; i++ {
		_, err := p.Eval(Env{Usage: billing.Usage{}, At: testAt, Region: "CN", Calendar: newTestCalendar()})
		if err == nil {
			t.Fatalf("第 %d 次求值静默放过：撞名的规则每一次都该被拦下", i)
		}
		if !strings.Contains(err.Error(), "prompt") {
			t.Fatalf("第 %d 次的错误没写明撞名的是哪个名字: %v", i, err)
		}
	}
}

// TestEvalDistinctUnitNameIsOrderIndependent 是上一条的另一半。
//
// 它不能省：只钉「撞名要报错」的话，一个「一律报错」的实现也能通过。
// 这里钉的是不撞名时每次都成功，且 Prices 与 Prices.Units 两个命名空间
// 各自都对——撞名的代价正是「账单上分不清哪个是哪个」。
func TestEvalDistinctUnitNameIsOrderIndependent(t *testing.T) {
	p := mustCompile(t, `{"prompt": 1.5, "units": {"search": 0.01}}`)
	for i := 0; i < 1000; i++ {
		res, err := p.Eval(Env{Usage: billing.Usage{}, At: testAt, Region: "CN", Calendar: newTestCalendar()})
		if err != nil {
			t.Fatalf("第 %d 次求值被拦下：不撞名的规则不该报错: %v", i, err)
		}
		if got := res.Prices.Prompt.String(); got != "1.50000000" {
			t.Fatalf("第 %d 次 prompt 单价 = %s，期望 1.50000000", i, got)
		}
		if got := res.Prices.Units["search"].String(); got != "0.01000000" {
			t.Fatalf("第 %d 次 search 单价 = %s，期望 0.01000000", i, got)
		}
		// token 槽位不该同时出现在计量项那一侧。
		if _, ok := res.Prices.Units["prompt"]; ok {
			t.Fatalf("第 %d 次 prompt 出现在了计量项命名空间里", i)
		}
		if !res.Prices.Declared["prompt"] {
			t.Fatalf("第 %d 次规则写过 prompt，Declared 里却没有", i)
		}
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
		b := billing.ComputeWithCharges(usage, res.Prices, billing.Charges{})
		if i == 0 {
			first = b.Total
			continue
		}
		if b.Total.Cmp(first) != 0 {
			t.Fatalf("第 %d 次求值得到 %s，第一次是 %s", i, b.Total, first)
		}
	}
}

// amountFrom 把算好的浮点价折成定点金额，它的兜底守卫两支要分开判。
//
// 变异：`!finite(v) || v < 0` 的 `||` 改成 `&&`——只有「既不是有限数、
// 又是负数」才走守卫，于是负的单价一路走到 money.Parse，而**它认负号**：
// amountFrom(-1.5) 会返回 -1.50000000 而不是零。负数单价就是「上游倒付钱
// 给用户」，这一支本是拦它的那一处。下面第一条断言在这一处红。
//
// 另一半**分不开**：非有限值格式化成 "+Inf" / "-Inf" / "NaN" 之后一样过不了
// money.Parse（里面含非数字字符），两版都落到 `return money.Zero`。
// 说不出哪一组输入能让两版不同，所以那一半是等价变异——下面那段 Inf / NaN 的
// 断言钉的是**行为**（非有限值一律折成零），不是杀变异体的证据：把 `!finite(v)`
// 那一半整个删掉，它们照样绿，因为解析失败那一支自己也 `return money.Zero`。
//
// 变异：`v < 0` 改成 `v <= 0`——边界那一格（正好 0）被守卫拦下返回 Zero，
// 不拦则 money.Parse("0") 得到 Amount{units: 0}。money.Amount 只有 units
// 一个字段，零只有一种表示，两版连 String() 都一样，所以这一条也是等价变异。
func TestAmountFromRejectsNegativePrice(t *testing.T) {
	if got := amountFrom(-1.5); got.Cmp(money.Zero) != 0 {
		t.Errorf("负数单价应当折成零，得到 %s", got)
	}
	for _, v := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		if got := amountFrom(v); got.Cmp(money.Zero) != 0 {
			t.Errorf("amountFrom(%v) 应当折成零，得到 %s", v, got)
		}
	}
	// 零也折成零：上面 `v <= 0` 那一段说的那一格。
	if got := amountFrom(0); got.Cmp(money.Zero) != 0 {
		t.Errorf("amountFrom(0) 应当折成零，得到 %s", got)
	}
	// 正常值照旧折成定点数：别把守卫写成「一律返回零」。
	if got := amountFrom(0.0000015); got.String() != "0.00000150" {
		t.Errorf("amountFrom(0.0000015) = %s，期望 0.00000150", got)
	}
}
