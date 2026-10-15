package calendar

import (
	"context"
	"errors"
	"testing"
	"time"
)

func mustWindow(t *testing.T, start, end int) PeakWindow {
	t.Helper()
	w, err := NewPeakWindow(start, end)
	if err != nil {
		t.Fatalf("NewPeakWindow(%d, %d) 出错: %v", start, end, err)
	}
	return w
}

func mustSchedule(t *testing.T, restDayAllPeak bool, windows ...PeakWindow) PeakSchedule {
	t.Helper()
	s, err := NewPeakSchedule(restDayAllPeak, windows...)
	if err != nil {
		t.Fatalf("NewPeakSchedule 出错: %v", err)
	}
	return s
}

// beijing 把北京的钟表时间转成时刻，用于构造与时段相关的用例。
func beijing(t *testing.T, d Date, hour, minute int) time.Time {
	t.Helper()
	loc := mustLocation(t, RegionCN)
	return time.Date(d.Year, d.Month, d.Day, hour, minute, 0, 0, loc)
}

func TestPeakWindowValidate(t *testing.T) {
	cases := []struct {
		start, end int
		wantErr    bool
	}{
		{8 * 60, 10 * 60, false},
		{0, 90, false},
		{22 * 60, 2 * 60, false}, // 跨零点
		{1339, 0, false},         // 跨零点到尾端
		{-1, 60, true},           // 起点为负
		{60, -1, true},           // 终点为负
		{0, minutesPerDay, true}, // 终点越界
		{minutesPerDay, 0, true}, // 起点越界
		{8 * 60, 8 * 60, false},  // 等长表示全天
	}
	for _, tc := range cases {
		w := PeakWindow{StartMinute: tc.start, EndMinute: tc.end}
		err := w.Validate()
		if tc.wantErr && !errors.Is(err, ErrInvalidPeakWindow) {
			t.Errorf("{%d, %d} 应报 ErrInvalidPeakWindow，得到 %v", tc.start, tc.end, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("{%d, %d} 不该报错: %v", tc.start, tc.end, err)
		}
	}
}

func TestPeakWindowString(t *testing.T) {
	cases := []struct {
		start, end int
		want       string
	}{
		{8 * 60, 10 * 60, "08:00-10:00"},
		{0, 60, "00:00-01:00"},
		{22 * 60, 2 * 60, "22:00-次日02:00"},
	}
	for _, tc := range cases {
		if got := (PeakWindow{tc.start, tc.end}).String(); got != tc.want {
			t.Errorf("String() = %q，期望 %q", got, tc.want)
		}
	}
}

// 端点归属：结束时刻不含在内，所以首尾相接的两段不重叠、也不留空隙。
func TestPeakWindowHalfOpenInterval(t *testing.T) {
	w := PeakWindow{StartMinute: 8 * 60, EndMinute: 10 * 60}
	cases := []struct {
		minute int
		want   bool
	}{
		{8*60 - 1, false},
		{8 * 60, true},
		{10*60 - 1, true},
		{10 * 60, false},
	}
	for _, tc := range cases {
		if got := w.containsMinute(tc.minute); got != tc.want {
			t.Errorf("containsMinute(%d) = %v，期望 %v", tc.minute, got, tc.want)
		}
	}

	next := PeakWindow{StartMinute: 10 * 60, EndMinute: 12 * 60}
	if !next.containsMinute(10 * 60) {
		t.Error("10:00 应属于下一段，两段之间不该有空隙")
	}
}

func TestPeakWindowAcrossMidnight(t *testing.T) {
	w := PeakWindow{StartMinute: 22 * 60, EndMinute: 2 * 60}
	cases := []struct {
		minute int
		want   bool
	}{
		{21*60 + 59, false},
		{22 * 60, true},
		{23*60 + 59, true},
		{0, true},
		{2*60 - 1, true},
		{2 * 60, false},
		{12 * 60, false},
	}
	for _, tc := range cases {
		if got := w.containsMinute(tc.minute); got != tc.want {
			t.Errorf("跨零点段 containsMinute(%d) = %v，期望 %v", tc.minute, got, tc.want)
		}
	}
}

func TestPeakWindowFullDay(t *testing.T) {
	w := PeakWindow{StartMinute: 9 * 60, EndMinute: 9 * 60}
	for _, m := range []int{0, 8 * 60, 12 * 60, 23*60 + 59} {
		if !w.containsMinute(m) {
			t.Errorf("等长时段（全天）应包含第 %d 分钟", m)
		}
	}
}

func TestPeakScheduleValidate(t *testing.T) {
	if err := (PeakSchedule{}).Validate(); err != nil {
		t.Errorf("空规则不该报错: %v", err)
	}

	bad := PeakWindow{StartMinute: -1, EndMinute: 60}
	if err := (PeakSchedule{Windows: []PeakWindow{bad}}).Validate(); !errors.Is(err, ErrInvalidPeakWindow) {
		t.Errorf("含非法段的规则应报错，得到 %v", err)
	}
	if err := (PeakSchedule{Windows: []PeakWindow{bad}}).Validate(); err == nil {
		t.Error("错误信息应指认是哪一段")
	}

	// 全天段与其他段并列：后者永远不会生效，属于自相矛盾的配置。
	full := PeakWindow{StartMinute: 0, EndMinute: 0}
	other := PeakWindow{StartMinute: 8 * 60, EndMinute: 10 * 60}
	s := PeakSchedule{Windows: []PeakWindow{full, other}}
	if err := s.Validate(); !errors.Is(err, ErrInvalidPeakWindow) {
		t.Errorf("全天段与其他段并列应报错，得到 %v", err)
	}

	// 段之间重叠是允许的：合并多组上游规则时这是常态。
	overlap := PeakSchedule{Windows: []PeakWindow{
		{StartMinute: 8 * 60, EndMinute: 12 * 60},
		{StartMinute: 10 * 60, EndMinute: 14 * 60},
	}}
	if err := overlap.Validate(); err != nil {
		t.Errorf("重叠的时段不该报错: %v", err)
	}
}

func TestNewPeakWindowRejectsBadWindow(t *testing.T) {
	if _, err := NewPeakWindow(-1, 60); !errors.Is(err, ErrInvalidPeakWindow) {
		t.Errorf("非法起点应报 ErrInvalidPeakWindow，得到 %v", err)
	}
	if _, err := NewPeakWindow(0, 24*60); !errors.Is(err, ErrInvalidPeakWindow) {
		t.Errorf("非法终点应报 ErrInvalidPeakWindow，得到 %v", err)
	}
	if w, err := NewPeakWindow(8*60, 10*60); err != nil || w.StartMinute != 8*60 {
		t.Errorf("合法时段构造失败: %+v, %v", w, err)
	}
}

func TestNewPeakScheduleRejectsBadWindow(t *testing.T) {
	if _, err := NewPeakSchedule(false, PeakWindow{StartMinute: 0, EndMinute: minutesPerDay}); !errors.Is(err, ErrInvalidPeakWindow) {
		t.Errorf("非法时段应报错，得到 %v", err)
	}
}

// 工作日：落在高峰段内算高峰，段外算低谷。
func TestPeakAtWeekdayWindows(t *testing.T) {
	cal, _ := newCNCalendar(t)
	s := mustSchedule(t, false, mustWindow(t, 8*60, 10*60), mustWindow(t, 18*60, 20*60))
	workday := d(2026, time.January, 5) // 周一
	ctx := context.Background()

	cases := []struct {
		hour, minute int
		want         PeakKind
	}{
		{8, 0, PeakKindPeak},
		{9, 59, PeakKindPeak},
		{10, 0, PeakKindOffPeak},
		{12, 0, PeakKindOffPeak},
		{18, 0, PeakKindPeak},
		{19, 59, PeakKindPeak},
		{20, 0, PeakKindOffPeak},
		{23, 59, PeakKindOffPeak},
	}
	for _, tc := range cases {
		got, err := cal.PeakAt(ctx, RegionCN, beijing(t, workday, tc.hour, tc.minute), s)
		if err != nil {
			t.Fatalf("PeakAt 出错: %v", err)
		}
		if got.Peak != tc.want {
			t.Errorf("工作日 %02d:%02d 判定为 %s，期望 %s", tc.hour, tc.minute, got.Peak, tc.want)
		}
		if got.Day.Kind != DayWorkday {
			t.Errorf("工作日 %02d:%02d 的日期判定为 %s", tc.hour, tc.minute, got.Day.Kind)
		}
	}
}

// 休息日整天算高峰的配置：周末任何时刻都是高峰。
func TestPeakAtRestDayAllPeak(t *testing.T) {
	cal, _ := newCNCalendar(t)
	s := mustSchedule(t, true, mustWindow(t, 8*60, 10*60))
	sat := d(2026, time.January, 3) // 周六
	ctx := context.Background()

	for _, hour := range []int{0, 3, 8, 12, 23} {
		got, err := cal.PeakAt(ctx, RegionCN, beijing(t, sat, hour, 30), s)
		if err != nil {
			t.Fatalf("PeakAt 出错: %v", err)
		}
		if got.Peak != PeakKindPeak {
			t.Errorf("休息日 %02d:30 判定为 %s，期望高峰", hour, got.Peak)
		}
	}
}

// 调休补班的周六在工作日/休息日判定上已经不同，高峰判定必须跟着它走：
// 同样的周六下午，普通周六是高峰，补班周六却要看时段。
func TestPeakAtMakeupWorkdayUsesWindows(t *testing.T) {
	cal, _ := newCNCalendar(t)
	s := mustSchedule(t, false, mustWindow(t, 8*60, 10*60))
	ctx := context.Background()

	makeup := d(2026, time.February, 21) // 周六，调休补班
	got, err := cal.PeakAt(ctx, RegionCN, beijing(t, makeup, 12, 0), s)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if got.Day.Kind != DayWorkday || !got.Day.MakeupWorkday {
		t.Fatalf("补班周六的日期判定 = %s（补班 %v），期望工作日 + 补班",
			got.Day.Kind, got.Day.MakeupWorkday)
	}
	if got.Peak != PeakKindOffPeak {
		t.Errorf("补班周六 12:00 判定为 %s，期望低谷（该时刻不在高峰段内）", got.Peak)
	}

	// 即使配了「休息日全天高峰」，补班日也不受它影响。
	s2 := mustSchedule(t, true, mustWindow(t, 8*60, 10*60))
	got2, err := cal.PeakAt(ctx, RegionCN, beijing(t, makeup, 12, 0), s2)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if got2.Peak != PeakKindOffPeak {
		t.Errorf("补班周六在「休息日全天高峰」下判定为 %s，期望低谷", got2.Peak)
	}
}

// 数据未知时高峰也不得判定：不能靠星期几猜一个答案。
func TestPeakAtUnknownDayStaysUnknown(t *testing.T) {
	cal, src := newCNCalendar(t)
	src.SetYearState(RegionCN, 2027, YearPending)
	s := mustSchedule(t, true, mustWindow(t, 8*60, 10*60))

	got, err := cal.PeakAt(context.Background(), RegionCN, beijing(t, d(2027, time.June, 7), 9, 0), s)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if got.Peak != PeakKindUnknown {
		t.Fatalf("日期未知时高峰判定为 %s，期望未知", got.Peak)
	}
	if got.Peak.Known() {
		t.Error("未知不该被当成确定结论")
	}
	if got.Day.Kind != DayUnknown || got.Day.Reason != ReasonYearPending {
		t.Errorf("随结果带出的日期判定 = %s / %q", got.Day.Kind, got.Day.Reason)
	}
}

// 跨零点的高峰段在次日凌晨依然成立，且按当地日期判。
func TestPeakAtAcrossMidnight(t *testing.T) {
	src := NewMemorySource()
	// 两天都铺成工作日：这两天里唯一的区别是钟表时刻。
	if err := src.SetDefaultRange(RegionCN,
		d(2026, time.January, 5), d(2026, time.January, 6),
		func(day Date) DayRow { return DayRow{Date: day} }); err != nil {
		t.Fatalf("铺默认区间出错: %v", err)
	}
	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}

	// 22:00–02:00 这一段跨过午夜，所以它同时覆盖当天的深夜与次日的凌晨。
	s := mustSchedule(t, false, mustWindow(t, 22*60, 2*60))
	ctx := context.Background()

	mon := d(2026, time.January, 5)
	tue := d(2026, time.January, 6)

	night, err := cal.PeakAt(ctx, RegionCN, beijing(t, mon, 23, 0), s)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if night.Peak != PeakKindPeak || night.Day.Date != mon {
		t.Errorf("周一 23:00 = %s（日期 %s），期望 高峰 + %s", night.Peak, night.Day.Date, mon)
	}

	early, err := cal.PeakAt(ctx, RegionCN, beijing(t, tue, 1, 30), s)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if early.Peak != PeakKindPeak || early.Day.Date != tue {
		t.Errorf("周二 01:30 = %s（日期 %s），期望 高峰 + %s", early.Peak, early.Day.Date, tue)
	}

	after, err := cal.PeakAt(ctx, RegionCN, beijing(t, tue, 2, 0), s)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if after.Peak != PeakKindOffPeak {
		t.Errorf("周二 02:00 = %s，期望低谷", after.Peak)
	}
}

// 时区只影响日期归属，改不了「这一天的哪一小时算高峰」。
// 同一瞬间按 UTC 与按北京属不同日期，是预期行为而不是缺陷。
func TestPeakAtTimezoneBoundary(t *testing.T) {
	cal, _ := newCNCalendar(t)
	s := mustSchedule(t, false, mustWindow(t, 0, 24*60-1))

	got, err := cal.PeakAt(context.Background(), RegionCN, time.Date(2025, time.December, 31, 16, 30, 0, 0, time.UTC), s)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if got.Day.Date != d(2026, time.January, 1) {
		t.Errorf("按北京时区应落在 2026-01-01，得到 %s", got.Day.Date)
	}
	if got.Peak != PeakKindPeak {
		t.Errorf("元旦在几乎全天的时段下应为高峰，得到 %s", got.Peak)
	}
}

func TestPeakAtEmptyScheduleIsOffPeak(t *testing.T) {
	cal, _ := newCNCalendar(t)
	s := mustSchedule(t, false)
	got, err := cal.PeakAt(context.Background(), RegionCN, beijing(t, d(2026, time.January, 5), 9, 0), s)
	if err != nil {
		t.Fatalf("PeakAt 出错: %v", err)
	}
	if got.Peak != PeakKindOffPeak {
		t.Errorf("没有高峰时段时判定为 %s，期望低谷", got.Peak)
	}
}

func TestPeakAtUnknownRegion(t *testing.T) {
	cal, _ := newCNCalendar(t)
	if _, err := cal.PeakAt(context.Background(), "XX", time.Now(), PeakSchedule{}); !errors.Is(err, ErrUnknownRegion) {
		t.Errorf("未知地区应报 ErrUnknownRegion，得到 %v", err)
	}
}

func TestPeakAtRejectsInvalidSchedule(t *testing.T) {
	cal, _ := newCNCalendar(t)
	bad := PeakSchedule{Windows: []PeakWindow{{StartMinute: -1, EndMinute: 60}}}
	if _, err := cal.PeakAt(context.Background(), RegionCN, time.Now(), bad); !errors.Is(err, ErrInvalidPeakWindow) {
		t.Errorf("非法规则应报 ErrInvalidPeakWindow，得到 %v", err)
	}
}

func TestPeakAtPropagatesSourceError(t *testing.T) {
	boom := errors.New("数据源不可用")
	cal, err := New(&countingSource{lookupErr: boom})
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	if _, err := cal.PeakAt(context.Background(), RegionCN, time.Now(), PeakSchedule{}); !errors.Is(err, boom) {
		t.Errorf("数据源错误未透出: %v", err)
	}
}
