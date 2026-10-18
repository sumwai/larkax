package calendaradapter

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/calendar"
)

// 2026-01-05 是周一。
var monday = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

func mustCalendar(t *testing.T, src calendar.Source) *calendar.Calendar {
	t.Helper()
	cal, err := calendar.New(src)
	if err != nil {
		t.Fatalf("构造日历出错: %v", err)
	}
	return cal
}

// 一个只有工作日/周末的完整数据集，用来把未知的场景隔离出去。
func workdaySource(t *testing.T) *calendar.MemorySource {
	t.Helper()
	src := calendar.NewMemorySource()
	// 一整年都按「周末休、其余上班」铺，且年份标为已同步——
	// 这样任何一天都查得到，未知只可能来自别的地方。
	start := calendar.Date{Year: 2026, Month: time.January, Day: 1}
	end := calendar.Date{Year: 2026, Month: time.December, Day: 31}
	err := src.SetDefaultRange(calendar.RegionCN, start, end, func(d calendar.Date) calendar.DayRow {
		off := d.IsWeekend()
		return calendar.DayRow{Date: d, OffDay: off}
	})
	if err != nil {
		t.Fatalf("铺设数据出错: %v", err)
	}
	src.SetYearState(calendar.RegionCN, 2026, calendar.YearSynced)
	return src
}

func adapt(t *testing.T, src calendar.Source) *Adapter {
	t.Helper()
	a, err := New(mustCalendar(t, src))
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	return a
}

func TestNewRejectsNil(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("nil 日历应被拒绝")
	}
}

func TestTimeZone(t *testing.T) {
	a := adapt(t, workdaySource(t))
	tz, err := a.TimeZone("CN")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if tz != "Asia/Shanghai" {
		t.Errorf("时区 = %q，期望 Asia/Shanghai", tz)
	}
}

func TestTimeZoneRejectsUnknownRegion(t *testing.T) {
	a := adapt(t, workdaySource(t))
	if _, err := a.TimeZone("ZZ"); err == nil {
		t.Error("未知地区应报错")
	}
}

func TestDayInfoWorkday(t *testing.T) {
	a := adapt(t, workdaySource(t))
	info, err := a.DayInfo("CN", monday)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if info.OffDay {
		t.Error("周一不该是休息日")
	}
}

func TestDayInfoWeekend(t *testing.T) {
	a := adapt(t, workdaySource(t))
	saturday := monday.AddDate(0, 0, 5)
	info, err := a.DayInfo("CN", saturday)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if !info.OffDay {
		t.Error("周六应是休息日")
	}
}

// 节假日要带出名字：账单要能解释「为什么这天算高峰」。
func TestDayInfoCarriesHolidayName(t *testing.T) {
	src := workdaySource(t)
	// 把周一改成国庆节
	if err := src.AddDay(calendar.RegionCN, calendar.DayRow{
		Date: calendar.Date{Year: 2026, Month: time.January, Day: 5},
		Name: "国庆节", OffDay: true,
	}); err != nil {
		t.Fatal(err)
	}

	a := adapt(t, src)
	info, err := a.DayInfo("CN", monday)
	if err != nil {
		t.Fatal(err)
	}
	if !info.OffDay || info.HolidayName != "国庆节" {
		t.Errorf("info = %+v，期望休息日且带名字", info)
	}
}

// 调休补班日是**工作日**，不是休息日。这是最容易搞反的一处。
func TestDayInfoMakeupWorkdayIsWorkday(t *testing.T) {
	src := workdaySource(t)
	saturday := calendar.Date{Year: 2026, Month: time.January, Day: 10}
	if err := src.AddDay(calendar.RegionCN, calendar.DayRow{
		Date: saturday, OffDay: false, MakeupWorkday: true,
	}); err != nil {
		t.Fatal(err)
	}

	a := adapt(t, src)
	info, err := a.DayInfo("CN", time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if info.OffDay {
		t.Error("补班日不该是休息日")
	}
	if !info.MakeupWorkday {
		t.Error("应带出调休标记")
	}
}

// ── 未知一律变成错误 ────────────────────────────────────────────────────

// 这是本适配器最要紧的一条：`calendar` 特意把判定做成三态，
// 就是为了防止有人把未知静默压成工作日——那会让节假日按工作日计价，
// 而账目上完全看不出来。压成工作日在这里尤其危险：它错的是钱。
func TestUnknownDayBecomesError(t *testing.T) {
	src := calendar.NewMemorySource()
	// 一天数据都没有：任何查询都是未知
	src.SetYearState(calendar.RegionCN, 2026, calendar.YearSynced)

	a := adapt(t, src)
	_, err := a.DayInfo("CN", monday)
	if err == nil {
		t.Fatal("未知的日期应当报错，而不是猜一个值")
	}
	if !errors.Is(err, expr.ErrCalendarRequired) {
		t.Errorf("期望 ErrCalendarRequired，得到 %v", err)
	}
	// 成因要带进错误里：year_pending（正常）与 row_missing（要查）
	// 对运维是两个完全不同的问题
	if !strings.Contains(err.Error(), string(calendar.ReasonRowMissing)) &&
		!strings.Contains(err.Error(), string(calendar.ReasonYearUnknown)) {
		t.Errorf("错误信息应带上成因: %v", err)
	}
}

// 未来年份公告未发布是**正常状态**，但它同样要变成错误——
// 只是成因不同。静默按工作日算会在一整年里按错误的分类计价。
func TestPendingYearBecomesError(t *testing.T) {
	src := calendar.NewMemorySource()
	src.SetYearState(calendar.RegionCN, 2026, calendar.YearPending)

	a := adapt(t, src)
	_, err := a.DayInfo("CN", monday)
	if err == nil {
		t.Fatal("公告未发布的年份应当报错")
	}
	if !strings.Contains(err.Error(), string(calendar.ReasonYearPending)) {
		t.Errorf("错误信息应指出是 pending: %v", err)
	}
}

// 错误措辞要指向**配置缺口**，不是服务故障。
//
// 日历数据是运维灌进来的（见 internal/calendar/store），缺数据时该去看的是
// 「这一年的数据导入了没有」。一句「日历不可用」会把人引向「日历服务挂了」，
// 而这个项目里根本没有那样一个服务——照着那句话排查会一直找不到东西。
func TestUnknownDayErrorReadsAsConfigGap(t *testing.T) {
	src := calendar.NewMemorySource()
	src.SetYearState(calendar.RegionCN, 2026, calendar.YearPending)

	a := adapt(t, src)
	_, err := a.DayInfo("CN", monday)
	if err == nil {
		t.Fatal("公告未发布的年份应当报错")
	}
	if !strings.Contains(err.Error(), "配置缺口") {
		t.Errorf("错误信息应当说明这是配置缺口: %v", err)
	}
	if !strings.Contains(err.Error(), "不是服务故障") {
		t.Errorf("错误信息应当排除「服务故障」这个解释: %v", err)
	}
	// 说清缺的是哪一天、哪个地区：否则拿到这条错误的人不知道从哪查起。
	if !strings.Contains(err.Error(), "CN") || !strings.Contains(err.Error(), "2026-") {
		t.Errorf("错误信息应当带出地区与日期: %v", err)
	}
}

func TestUnknownRegionBecomesError(t *testing.T) {
	a := adapt(t, workdaySource(t))
	if _, err := a.DayInfo("ZZ", monday); err == nil {
		t.Error("未知地区应报错")
	}
}

// ── 与求值引擎接起来 ────────────────────────────────────────────────────

// 端到端：一个按「工作日/休息日」分档的规则，在已知日期上求值成功。
func TestEvalWithAdapter(t *testing.T) {
	a := adapt(t, workdaySource(t))
	prog, err := expr.Compile(`isWorkday() ? {"prompt": 2.0} : {"prompt": 1.0}`)
	if err != nil {
		t.Fatalf("编译出错: %v", err)
	}

	// 周一：工作日档
	res, err := prog.Eval(expr.Env{
		Usage:    billing.Usage{PromptTokens: 100},
		At:       monday,
		Region:   "CN",
		Calendar: a,
	})
	if err != nil {
		t.Fatalf("求值出错: %v", err)
	}
	if res.Prices.Prompt.String() != "2.00000000" {
		t.Errorf("工作日单价 = %s，期望 2", res.Prices.Prompt)
	}

	// 周六：休息日档
	saturday := monday.AddDate(0, 0, 5)
	res, err = prog.Eval(expr.Env{
		Usage:    billing.Usage{PromptTokens: 100},
		At:       saturday,
		Region:   "CN",
		Calendar: a,
	})
	if err != nil {
		t.Fatalf("求值出错: %v", err)
	}
	if res.Prices.Prompt.String() != "1.00000000" {
		t.Errorf("休息日单价 = %s，期望 1", res.Prices.Prompt)
	}
}

// 日历数据缺失时，**求值必须失败**，而不是按某个分支算出价。
//
// 这是 Jev 判的结论（fail_the_request 概率 0.93、静默回退 0.00）：
// 未知的日子价格本来就是未知的，收一个没人背书的数字等于让账悄悄错掉。
func TestEvalFailsOnUnknownCalendarDay(t *testing.T) {
	src := calendar.NewMemorySource()
	src.SetYearState(calendar.RegionCN, 2026, calendar.YearPending)
	a := adapt(t, src)

	prog, err := expr.Compile(`isWorkday() ? {"prompt": 2.0} : {"prompt": 1.0}`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = prog.Eval(expr.Env{
		Usage:    billing.Usage{PromptTokens: 100},
		At:       monday,
		Region:   "CN",
		Calendar: a,
	})
	if err == nil {
		t.Fatal("日历数据不可用时求值必须失败，不能猜一个价")
	}
}

// ── 峰谷：规则声明的窗口 ────────────────────────────────────────────────

// 端到端：一条按峰谷分档的规则，窗口由规则自己声明。
//
// 这里走的是**真日历**：窗口串解析成分钟数之后，判定由 internal/calendar 的
// PeakAt 做。所以这条用例同时证明了「两边接得上」与「算出来的价与手算一致」——
// 工作日 09:00 落在 08:00–22:00 里按高峰收，12:00 不在窗口里按低谷收。
func TestPeakWindowsDecideThePrice(t *testing.T) {
	a := adapt(t, workdaySource(t))
	prog, err := expr.Compile(`isPeak("08:00-12:00,14:00-18:00")
		? {"prompt": 2.0}
		: {"prompt": 1.0}`)
	if err != nil {
		t.Fatalf("编译出错: %v", err)
	}

	// 2026-01-05 是周一，北京时间 = UTC+8。
	for _, tc := range []struct {
		name  string
		hour  int
		price string
	}{
		{"上午高峰（窗口内）", 9, "2.00000000"},
		{"午休（第一段之后、第二段之前）", 13, "1.00000000"},
		{"下午高峰（第二段内）", 15, "2.00000000"},
		{"晚间（窗口之外）", 20, "1.00000000"},
	} {
		at := time.Date(2026, 1, 5, tc.hour-8, 0, 0, 0, time.UTC)
		res, err := prog.Eval(expr.Env{
			Usage:    billing.Usage{PromptTokens: 100},
			At:       at,
			Region:   "CN",
			Calendar: a,
		})
		if err != nil {
			t.Fatalf("%s: 求值出错: %v", tc.name, err)
		}
		if got := res.Prices.Prompt.String(); got != tc.price {
			t.Errorf("%s（北京时间 %d:00）单价 = %s，期望 %s", tc.name, tc.hour, got, tc.price)
		}
	}
}

// 跨零点的窗口：`22:00-02:00` 指的是当天 22:00 到次日 02:00。
//
// 这一条放在这一层而不是 expr 里：分号两边的含义要由**判定**来兑现，
// 而判定在日历那边。写出 22:00 到 02:00 两个整数只是形状对。
func TestPeakWindowAcrossMidnight(t *testing.T) {
	a := adapt(t, workdaySource(t))
	prog, err := expr.Compile(`isPeak("22:00-02:00") ? {"prompt": 2.0} : {"prompt": 1.0}`)
	if err != nil {
		t.Fatalf("编译出错: %v", err)
	}

	for _, tc := range []struct {
		name  string
		hour  int
		price string
	}{
		{"当天 23:00（窗口内）", 23, "2.00000000"},
		{"次日 01:00（窗口内）", 25, "2.00000000"},
		{"当天 21:00（窗口之前）", 21, "1.00000000"},
		{"次日 03:00（窗口之后）", 27, "1.00000000"},
	} {
		// UTC 的小时数 = 北京时间 - 8；超过 24 的落到次日。
		at := time.Date(2026, 1, 5, tc.hour-8, 0, 0, 0, time.UTC)
		res, err := prog.Eval(expr.Env{
			Usage:    billing.Usage{PromptTokens: 100},
			At:       at,
			Region:   "CN",
			Calendar: a,
		})
		if err != nil {
			t.Fatalf("%s: 求值出错: %v", tc.name, err)
		}
		if got := res.Prices.Prompt.String(); got != tc.price {
			t.Errorf("%s 单价 = %s，期望 %s", tc.name, got, tc.price)
		}
	}
}

// 休息日整天算不算高峰由规则组合出来：接口上没有那个开关（见 expr.Calendar
// 的 Peak）。`!isWorkday() || isPeak(...)` 就是「周末全天高峰」的写法。
//
// 周六 10:00 落在窗口里、20:00 不在——两个都要按高峰收，因为那一天整天算高峰。
func TestPeakOnRestDayIsComposedByTheRule(t *testing.T) {
	a := adapt(t, workdaySource(t))
	prog, err := expr.Compile(`(!isWorkday() || isPeak("08:00-12:00"))
		? {"prompt": 2.0}
		: {"prompt": 1.0}`)
	if err != nil {
		t.Fatalf("编译出错: %v", err)
	}

	// 2026-01-10 是周六。
	for _, hour := range []int{10, 20} {
		at := time.Date(2026, 1, 10, hour-8, 0, 0, 0, time.UTC)
		res, err := prog.Eval(expr.Env{
			Usage:    billing.Usage{PromptTokens: 100},
			At:       at,
			Region:   "CN",
			Calendar: a,
		})
		if err != nil {
			t.Fatalf("周六 %d:00 求值出错: %v", hour, err)
		}
		if got := res.Prices.Prompt.String(); got != "2.00000000" {
			t.Errorf("周六 %d:00（北京时间）单价 = %s，期望整天算高峰的 2", hour, got)
		}
	}
}

// 日历判不出峰谷时，**求值必须失败**——与 isWorkday() 同一条口径。
//
// PeakAt 给出的是三态，unknown 表示「那一天是不是休息日还没查清」。这里
// 断言的是**报了哪一个错**：静默按低谷算会在春节期间按低谷价收费，
// 而账单看上去完全正常。
func TestEvalIsPeakFailsOnUnknownDay(t *testing.T) {
	src := calendar.NewMemorySource()
	src.SetYearState(calendar.RegionCN, 2026, calendar.YearPending)
	a := adapt(t, src)

	prog, err := expr.Compile(`isPeak("08:00-22:00") ? {"prompt": 2.0} : {"prompt": 1.0}`)
	if err != nil {
		t.Fatal(err)
	}

	_, err = prog.Eval(expr.Env{
		Usage:    billing.Usage{PromptTokens: 100},
		At:       monday,
		Region:   "CN",
		Calendar: a,
	})
	if err == nil {
		t.Fatal("日历数据不可用时峰谷必须算不出来，不能按某一档算价")
	}
	if !errors.Is(err, expr.ErrCalendarRequired) {
		t.Errorf("期望 ErrCalendarRequired，得到 %v", err)
	}
	// 措辞与 isWorkday() 那条一模一样：同一个原因不该有两句解释。
	if !strings.Contains(err.Error(), "配置缺口") {
		t.Errorf("错误信息应当说明这是配置缺口: %v", err)
	}
	if !strings.Contains(err.Error(), string(calendar.ReasonYearPending)) {
		t.Errorf("错误信息应带上成因: %v", err)
	}
}

// 端点必须按左闭右开判：`08:00-22:00` 里 08:00 算高峰、22:00 不算。
//
// 这一条特意落在**适配器**这一层，而不是只靠 internal/calendar 的分钟层用例：
// 把 containsMinute 的 `m >=` 改成 `m >`，日历那边会红，而这一层与 expr 那一层
// 不会——两层用例的取样点都在窗口中间，端点的错就穿过去了。整点是规则作者
// 最常写的边界（上游那张表写的就是 8:00 起、22:00 止）。
func TestPeakWindowEndpointsAreHalfOpen(t *testing.T) {
	a := adapt(t, workdaySource(t))
	prog, err := expr.Compile(`isPeak("08:00-22:00")
		? {"prompt": 2.0}
		: {"prompt": 1.0}`)
	if err != nil {
		t.Fatalf("编译出错: %v", err)
	}

	for _, tc := range []struct {
		name  string
		hour  int
		price string
	}{
		{"起点 08:00 在窗口内", 8, "2.00000000"},
		{"终点 22:00 不在窗口内", 22, "1.00000000"},
	} {
		at := time.Date(2026, 1, 5, tc.hour-8, 0, 0, 0, time.UTC)
		res, err := prog.Eval(expr.Env{
			Usage:    billing.Usage{PromptTokens: 100},
			At:       at,
			Region:   "CN",
			Calendar: a,
		})
		if err != nil {
			t.Fatalf("%s: 求值出错: %v", tc.name, err)
		}
		if got := res.Prices.Prompt.String(); got != tc.price {
			t.Errorf("%s（北京时间 %d:00）单价 = %s，期望 %s", tc.name, tc.hour, got, tc.price)
		}
	}
}

// 全天段与其他段并列时不成立——这条校验在适配器与日历那一层都还在。
//
// 规则走不到这里：窗口的写法在保存规则时就判过一遍（见 expr 的
// ParsePeakWindows 与 compile 的那条用例），全天段并列的规则编不过。这一条
// 直接调适配器，为的是另一件事：这两个入口不止服务规则（测试与夹具直接构造
// 排班），转换那一段的防线不该因为规则挡在前面就被拆掉。
func TestPeakRejectsFullDayWindowAlongsideOthers(t *testing.T) {
	a := adapt(t, workdaySource(t))
	_, err := a.Peak("CN", monday, []expr.PeakWindow{
		{StartMinute: 0, EndMinute: 0},
		{StartMinute: 8 * 60, EndMinute: 12 * 60},
	})
	if err == nil {
		t.Fatal("全天段与其他段并列应当报错")
	}
	if !errors.Is(err, calendar.ErrInvalidPeakWindow) {
		t.Errorf("期望 ErrInvalidPeakWindow（日历的排班校验），得到 %v", err)
	}
}

// 最坏情况求值同样要能接上适配器——它跑的也是同一份环境。
func TestEvalWorstCaseWithAdapter(t *testing.T) {
	a := adapt(t, workdaySource(t))
	prog, err := expr.Compile(`isWorkday() ? {"prompt": 2.0} : {"prompt": 1.0}`)
	if err != nil {
		t.Fatal(err)
	}

	res, err := prog.EvalWorstCase(expr.Env{
		At:       monday,
		Region:   "CN",
		Calendar: a,
	})
	if err != nil {
		t.Fatalf("最坏情况求值出错: %v", err)
	}
	// 上界要取两个分支里更贵的那个
	if res.Prices.Prompt.String() != "2.00000000" {
		t.Errorf("最坏情况单价 = %s，期望 2", res.Prices.Prompt)
	}
}
