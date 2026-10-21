package expr

import (
	"errors"
	"testing"
	"time"
)

// evalAt 在指定时刻、指定地区求值一条规则。
//
// 与 eval 分开是因为这几个用例要的正是「同一个时刻在两个时区看到不同的
// 星期」，而 eval 把 At 与 Region 固定成了 CN 的某个上午。
func evalAt(t *testing.T, source string, at time.Time, region string, cal Calendar) Result {
	t.Helper()
	p := mustCompile(t, source)
	res, err := p.Eval(Env{At: at, Region: region, Calendar: cal})
	if err != nil {
		t.Fatalf("求值 %q 失败: %v", source, err)
	}
	return res
}

// TestWeekdayUsesExplicitTimeZone 钉住 weekday(tz) 的三个判据：
// 0 是周日、时区由规则显式给出、结果按那个时区的当地星期算。
//
// 取 2026-10-16 20:00 UTC：上海已经是次日 04:00（周六，6），纽约还是
// 当天 16:00（周五，5）。同一个时刻给出两个不同的答案，才证明时区真的
// 起作用了——只测一个时区的话，把结果换成 UTC 星期也能过。
func TestWeekdayUsesExplicitTimeZone(t *testing.T) {
	at := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	source := `{"prompt": weekday("Asia/Shanghai") == 6 ? 6.0 : weekday("America/New_York") == 5 ? 5.0 : 1.0}`
	res := evalAt(t, source, at, "CN", newTestCalendar())
	if got := res.Prices.Prompt.Float64(); got != 6 {
		t.Fatalf("上海时区下的星期判定 = %v，期望周六（6）", got)
	}
}

// TestWeekdaySundayIsZero 钉住 0 = 周日，与 Go 的 time.Weekday 一致。
func TestWeekdaySundayIsZero(t *testing.T) {
	// 2026-10-18 是周日；上海时间 10:00。
	at := time.Date(2026, 10, 18, 2, 0, 0, 0, time.UTC)
	source := `{"prompt": weekday("Asia/Shanghai") == 0 ? 7.0 : 1.0}`
	res := evalAt(t, source, at, "CN", newTestCalendar())
	if got := res.Prices.Prompt.Float64(); got != 7 {
		t.Fatalf("周日的 weekday = %v，期望 0 命中", got)
	}
}

// TestWeekdayBadTimeZone 钉住坏时区报的是「日历不可用」这一大类。
//
// 与 hour(tz) 同一条路径，所以两者在时区写错时的处置必须一致：
// 退回 UTC 会让分时与星期整体错位，账目上完全看不出来。
func TestWeekdayBadTimeZone(t *testing.T) {
	p := mustCompile(t, `{"prompt": weekday("Mars/Olympus") == 1 ? 2.0 : 1.0}`)
	_, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: newTestCalendar()})
	if !errors.Is(err, ErrCalendarRequired) {
		t.Fatalf("坏时区的错误 = %v，期望 ErrCalendarRequired", err)
	}
}

// TestHolidayNameReturnsNamesOnly 钉住 holidayName() 的三件事：
// 节假日当天给名字、普通工作日给空串、周末（没有名字）也给空串。
//
// 周末那一格是关键：isHoliday() 已经写明「有名字才算节假日」，如果
// holidayName() 在周末给出一个非空值（比如 "周六"），两条函数的判据就分家了。
func TestHolidayNameReturnsNamesOnly(t *testing.T) {
	source := `{"prompt": holidayName() == "国庆节" ? 3.0 : holidayName() == "" ? 1.0 : 2.0}`

	holidayAt := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	if got := evalAt(t, source, holidayAt, "CN", newTestCalendar()).Prices.Prompt.Float64(); got != 3 {
		t.Fatalf("国庆节的 holidayName() = %v，期望命中名字", got)
	}

	workdayAt := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	if got := evalAt(t, source, workdayAt, "CN", newTestCalendar()).Prices.Prompt.Float64(); got != 1 {
		t.Fatalf("工作日的 holidayName() = %v，期望空串", got)
	}

	weekendAt := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	if got := evalAt(t, source, weekendAt, "CN", newTestCalendar()).Prices.Prompt.Float64(); got != 1 {
		t.Fatalf("普通周末的 holidayName() = %v，期望空串（周末不是节假日）", got)
	}
}

// TestHolidayNameInList 钉住 `in [...]` 的用法：结构化定价表生成的就是这一形。
func TestHolidayNameInList(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	source := `{"prompt": holidayName() in ["春节", "国庆节"] ? 3.0 : 1.0}`
	if got := evalAt(t, source, at, "CN", newTestCalendar()).Prices.Prompt.Float64(); got != 3 {
		t.Fatalf("国庆节没有命中 in 名单：%v", got)
	}
}

// TestIsMakeupWorkday 钉住调休补班日与「工作日」是两个问题。
//
// 同一时刻在上海是普通工作日（补班为 false），在纽约的测试日历里是补班日
// （补班为 true）——两边都查同一份 DayInfo，差别只在地区。
func TestIsMakeupWorkday(t *testing.T) {
	source := `{"prompt": isMakeupWorkday() ? 2.0 : 1.0}`

	if got := evalAt(t, source, testAt, "CN", newTestCalendar()).Prices.Prompt.Float64(); got != 1 {
		t.Fatalf("普通工作日的补班判定 = %v，期望 false", got)
	}
	if got := evalAt(t, source, testAt, "US", newTestCalendar()).Prices.Prompt.Float64(); got != 2 {
		t.Fatalf("调休补班日的补班判定 = %v，期望 true", got)
	}
}

// TestCalendarFunctionsWithoutCalendar 钉住三个新函数都走「日历不可用」。
//
// 漏掉哪个函数，那条规则在没接日历的实例上会静默算出一个默认值——
// 对星期函数给 0（周日）、对名字给空串，都不是「不知道」。
func TestCalendarFunctionsWithoutCalendar(t *testing.T) {
	cases := map[string]string{
		"holidayName":     `{"prompt": holidayName() == "" ? 2.0 : 1.0}`,
		"isMakeupWorkday": `{"prompt": isMakeupWorkday() ? 2.0 : 1.0}`,
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			p := mustCompile(t, source)
			_, err := p.Eval(Env{At: testAt, Region: "CN"})
			if !errors.Is(err, ErrCalendarRequired) {
				t.Fatalf("%s 在没有日历时 = %v，期望 ErrCalendarRequired", name, err)
			}
		})
	}

	// weekday 不查日历，但没有时区数据时也要报同一个大类。
	p := mustCompile(t, `{"prompt": weekday("") == 1 ? 2.0 : 1.0}`)
	_, err := p.Eval(Env{At: testAt, Region: "CN"})
	if !errors.Is(err, ErrCalendarRequired) {
		t.Fatalf("空时区的 weekday = %v，期望 ErrCalendarRequired", err)
	}
}
