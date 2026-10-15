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
