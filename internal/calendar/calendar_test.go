package calendar

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 测试样本里的日期取自公开日历，取值本身只是占位形态；
// 星期几用 `TZ=Asia/Shanghai date -d <日期> +%A` 核对过。

func d(y int, m time.Month, day int) Date { return Date{Year: y, Month: m, Day: day} }

// plainYear 是测试用的「没有调休的一年」模板：工作日上班、周末休息。
// 它只用来给测试数据铺底色，不代表本包的判定规则——真实数据由数据源给出，
// 而调休恰恰会推翻这个假设，所以用例里总是单独覆盖有安排的那几天。
func plainYear(day Date) DayRow {
	return DayRow{Date: day, OffDay: day.IsWeekend()}
}

func mustLocation(t *testing.T, region Region) *time.Location {
	t.Helper()
	loc, err := Timezone(region)
	if err != nil {
		t.Fatalf("Timezone(%s) 出错: %v", region, err)
	}
	return loc
}

// countingSource 记录数据源被调用的次数，用来钉住「参数不合法时不查数据源」。
type countingSource struct {
	dayCalls   int
	yearCalls  int
	dayRow     DayRow
	dayExists  bool
	yearState  YearState
	lookupErr  error
	yearErr    error
	lastRegion Region
	lastDay    Date
}

func (c *countingSource) LookupDay(_ context.Context, region Region, day Date) (DayRow, bool, error) {
	c.dayCalls++
	c.lastRegion, c.lastDay = region, day
	return c.dayRow, c.dayExists, c.lookupErr
}

func (c *countingSource) YearState(_ context.Context, region Region, year int) (YearState, error) {
	c.yearCalls++
	c.lastRegion = region
	return c.yearState, c.yearErr
}

func TestTimezoneKnownRegion(t *testing.T) {
	loc, err := Timezone(RegionCN)
	if err != nil {
		t.Fatalf("Timezone(CN) 出错: %v", err)
	}
	// 中国大陆全境用一个时区，且无夏令时：冬夏偏移必须一致，
	// 否则按钟表时段定的单价会在大半年的时间里错一小时。
	winter := time.Date(2026, time.January, 15, 12, 0, 0, 0, loc)
	summer := time.Date(2026, time.July, 15, 12, 0, 0, 0, loc)
	if _, off := winter.Zone(); off != 8*3600 {
		t.Errorf("CN 冬季偏移 = %d 秒，期望 28800", off)
	}
	if _, off := summer.Zone(); off != 8*3600 {
		t.Errorf("CN 夏季偏移 = %d 秒，期望 28800（中国大陆无夏令时）", off)
	}
}

func TestTimezoneUnknownRegion(t *testing.T) {
	if _, err := Timezone("XX"); !errors.Is(err, ErrUnknownRegion) {
		t.Fatalf("未知地区应报 ErrUnknownRegion，得到 %v", err)
	}
}

func TestDateValid(t *testing.T) {
	cases := []struct {
		in   Date
		want bool
	}{
		{d(2026, time.January, 5), true},
		{d(2024, time.February, 29), true},  // 闰年
		{d(2026, time.February, 29), false}, // 平年
		{d(2026, time.February, 30), false},
		{d(2026, time.April, 31), false},
		{d(2026, time.December, 31), true},
		{d(2026, 0, 1), false},
		{d(2026, 13, 1), false},
		{d(2026, time.January, 0), false},
		{d(0, time.January, 1), false},
		// 年份的合法范围是 [1, 9999]（store 那份文件的表头校验用的是同一段
		// 范围），所以两端都要有：恰好 1 年与 9999 年的日期成立，10000 年
		// 不成立。把 `< 1`、`> 9999` 改成 `<= 1`、`>= 9999` 的变异就落在
		// 这两天上——它们会被判成非法，而这两年的日历都排得出来。
		{d(1, time.January, 1), true},
		{d(9999, time.December, 31), true},
		{d(10000, time.January, 1), false},
	}
	for _, tc := range cases {
		if got := tc.in.Valid(); got != tc.want {
			t.Errorf("%s.Valid() = %v，期望 %v", tc.in, got, tc.want)
		}
	}
}

func TestDateString(t *testing.T) {
	if got := d(2026, time.January, 5).String(); got != "2026-01-05" {
		t.Errorf("String() = %q，期望 2026-01-05", got)
	}
}

func TestDateWeekdayAndWeekend(t *testing.T) {
	cases := []struct {
		in      Date
		weekday time.Weekday
		weekend bool
	}{
		{d(2026, time.January, 5), time.Monday, false},
		{d(2026, time.January, 2), time.Friday, false},
		{d(2026, time.January, 3), time.Saturday, true},
		{d(2026, time.January, 4), time.Sunday, true},
	}
	for _, tc := range cases {
		if got := tc.in.Weekday(); got != tc.weekday {
			t.Errorf("%s.Weekday() = %v，期望 %v", tc.in, got, tc.weekday)
		}
		if got := tc.in.IsWeekend(); got != tc.weekend {
			t.Errorf("%s.IsWeekend() = %v，期望 %v", tc.in, got, tc.weekend)
		}
	}
}

// IsWeekend 是纯星期规则，不代表判定结果：调休的周六算工作日，
// 落在工作日的法定假日算休息日。这条测试固定「它只是规则，不是结论」。
func TestIsWeekendIsWeekdayRuleOnly(t *testing.T) {
	tue := d(2026, time.February, 17) // 周二
	if tue.IsWeekend() {
		t.Fatal("周二不该是周末")
	}
	eve := d(2025, time.December, 31) // 周三
	if eve.IsWeekend() {
		t.Fatal("周三不该是周末")
	}
}

func TestDateOfUsesOwnZone(t *testing.T) {
	loc := mustLocation(t, RegionCN)
	instant := time.Date(2025, time.December, 31, 16, 30, 0, 0, time.UTC)

	if got, want := DateOf(instant), d(2025, time.December, 31); got != want {
		t.Errorf("DateOf(UTC 时刻) = %s，期望 %s", got, want)
	}
	if got, want := DateOf(instant.In(loc)), d(2026, time.January, 1); got != want {
		t.Errorf("DateOf(北京时刻) = %s，期望 %s", got, want)
	}
}

// newCNCalendar 给出一份最小但真实形态的数据：2026 年上半年已同步
// （下半年尚未铺入，用来构造「年份已同步但缺这一天」），元月 1 日放假，
// 2 月 16 至 18 日春节放假，2 月 21 日（周六）调休补班、2 月 22 日（周日）放假。
func newCNCalendar(t *testing.T) (*Calendar, *MemorySource) {
	t.Helper()
	src := NewMemorySource()
	if err := src.SetDefaultRange(RegionCN,
		d(2026, time.January, 1), d(2026, time.June, 30), plainYear); err != nil {
		t.Fatalf("铺默认区间出错: %v", err)
	}
	src.SetYearState(RegionCN, 2026, YearSynced)

	add := func(row DayRow) {
		t.Helper()
		if err := src.AddDay(RegionCN, row); err != nil {
			t.Fatalf("登记 %s 出错: %v", row.Date, err)
		}
	}
	add(DayRow{Date: d(2026, time.January, 1), Name: "元旦", OffDay: true})
	add(DayRow{Date: d(2026, time.February, 16), Name: "春节", OffDay: true})
	add(DayRow{Date: d(2026, time.February, 17), Name: "春节", OffDay: true})
	add(DayRow{Date: d(2026, time.February, 18), Name: "春节", OffDay: true})
	add(DayRow{Date: d(2026, time.February, 21), MakeupWorkday: true}) // 周六补班
	add(DayRow{Date: d(2026, time.February, 22), Name: "春节", OffDay: true})

	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	return cal, src
}

func TestNewRejectsNilSource(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrNilSource) {
		t.Fatalf("New(nil) 应报 ErrNilSource，得到 %v", err)
	}
}

func TestLookupWorkday(t *testing.T) {
	cal, _ := newCNCalendar(t)
	got, err := cal.Lookup(context.Background(), RegionCN, d(2026, time.January, 5)) // 周一
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayWorkday {
		t.Errorf("周一判定为 %s，期望工作日", got.Kind)
	}
	if got.Name != "" || got.MakeupWorkday {
		t.Errorf("普通工作日不该带节日名称或补班标记: %+v", got)
	}
	if got.Date != d(2026, time.January, 5) || got.Region != RegionCN {
		t.Errorf("结果未回填日期与地区: %+v", got)
	}
}

func TestLookupRestDayOnWeekend(t *testing.T) {
	cal, _ := newCNCalendar(t)
	got, err := cal.Lookup(context.Background(), RegionCN, d(2026, time.January, 3)) // 周六
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayRestDay {
		t.Errorf("无数据的周六判定为 %s，期望休息日", got.Kind)
	}
}

// 调休是最容易搞错的一处：这两年常见的形态是「周六补班、周日放假」，
// 两天都在周末，判定却必须相反。
func TestLookupMakeupWorkdayAndSwappedRestDay(t *testing.T) {
	cal, _ := newCNCalendar(t)
	ctx := context.Background()

	sat, err := cal.Lookup(ctx, RegionCN, d(2026, time.February, 21)) // 周六补班
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if sat.Kind != DayWorkday {
		t.Errorf("调休补班的周六判定为 %s，期望工作日", sat.Kind)
	}
	if !sat.MakeupWorkday {
		t.Error("调休补班的周六未带上 MakeupWorkday 标记")
	}

	sun, err := cal.Lookup(ctx, RegionCN, d(2026, time.February, 22)) // 周日放假
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if sun.Kind != DayRestDay {
		t.Errorf("假期内的周日判定为 %s，期望休息日", sun.Kind)
	}
	if sun.Name != "春节" {
		t.Errorf("休息日的节日名称 = %q，期望 春节", sun.Name)
	}
	if sun.MakeupWorkday {
		t.Error("放假的周日不该带上补班标记")
	}
}

func TestLookupHolidayOnWeekday(t *testing.T) {
	cal, _ := newCNCalendar(t)
	got, err := cal.Lookup(context.Background(), RegionCN, d(2026, time.January, 1)) // 周四
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayRestDay {
		t.Errorf("元旦（周四）判定为 %s，期望休息日", got.Kind)
	}
	if got.Name != "元旦" {
		t.Errorf("节日名称 = %q，期望 元旦", got.Name)
	}
}

// 未来年份查不到数据是正常状态，绝不能静默当成普通工作日。
func TestLookupUnknownYearPending(t *testing.T) {
	cal, src := newCNCalendar(t)
	src.SetYearState(RegionCN, 2027, YearPending)

	got, err := cal.Lookup(context.Background(), RegionCN, d(2027, time.June, 7)) // 周一，但数据没发布
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayUnknown {
		t.Fatalf("公告未发布时判定为 %s，期望未知", got.Kind)
	}
	if got.Reason != ReasonYearPending {
		t.Errorf("Reason = %q，期望 %q", got.Reason, ReasonYearPending)
	}
}

func TestLookupUnknownYearNoRecord(t *testing.T) {
	cal, _ := newCNCalendar(t)
	got, err := cal.Lookup(context.Background(), RegionCN, d(2028, time.January, 3))
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayUnknown || got.Reason != ReasonYearUnknown {
		t.Errorf("没有年份记录时应为未知 + %q，得到 %s + %q",
			ReasonYearUnknown, got.Kind, got.Reason)
	}
}

func TestLookupUnknownYearDegradedAndConflict(t *testing.T) {
	cases := []struct {
		state YearState
		want  UnknownReason
	}{
		{YearDegraded, ReasonYearDegraded},
		{YearConflict, ReasonYearConflict},
		{YearPending, ReasonYearPending},
	}
	for _, tc := range cases {
		cal, src := newCNCalendar(t)
		src.SetYearState(RegionCN, 2027, tc.state)
		got, err := cal.Lookup(context.Background(), RegionCN, d(2027, time.March, 1))
		if err != nil {
			t.Fatalf("Lookup 出错: %v", err)
		}
		if got.Kind != DayUnknown || got.Reason != tc.want {
			t.Errorf("状态 %s 时得到 %s + %q，期望未知 + %q", tc.state, got.Kind, got.Reason, tc.want)
		}
	}
}

// 年份标着已同步，这一天却没有记录：那是数据不完整。
// 若悄悄当成工作日，节假日就会按工作日计价。
func TestLookupRowMissingIsUnknownNotWorkday(t *testing.T) {
	cal, src := newCNCalendar(t)
	src.SetYearState(RegionCN, 2026, YearSynced)

	missing := d(2026, time.July, 6) // 周一，且未被任何区间或单日登记覆盖
	got, err := cal.Lookup(context.Background(), RegionCN, missing)
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayUnknown {
		t.Fatalf("已同步年份里缺记录的日子判定为 %s，期望未知", got.Kind)
	}
	if got.Reason != ReasonRowMissing {
		t.Errorf("Reason = %q，期望 %q", got.Reason, ReasonRowMissing)
	}
}

func TestLookupInconsistentRowIsUnknown(t *testing.T) {
	src := NewMemorySource()
	day := d(2026, time.October, 10) // 周六
	if err := src.AddDay(RegionCN, DayRow{Date: day, OffDay: true, MakeupWorkday: true}); err != nil {
		t.Fatalf("AddDay 出错: %v", err)
	}
	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}

	got, err := cal.Lookup(context.Background(), RegionCN, day)
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayUnknown {
		t.Fatalf("自相矛盾的记录判定为 %s，期望未知", got.Kind)
	}
	if got.Reason != ReasonRowInconsistent {
		t.Errorf("Reason = %q，期望 %q", got.Reason, ReasonRowInconsistent)
	}
}

func TestLookupRejectsBadInputBeforeQuerying(t *testing.T) {
	src := &countingSource{}
	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}

	if _, err := cal.Lookup(context.Background(), "XX", d(2026, time.January, 5)); !errors.Is(err, ErrUnknownRegion) {
		t.Errorf("未知地区应报 ErrUnknownRegion，得到 %v", err)
	}
	if _, err := cal.Lookup(context.Background(), RegionCN, d(2026, time.February, 30)); !errors.Is(err, ErrInvalidDate) {
		t.Errorf("2 月 30 日应报 ErrInvalidDate，得到 %v", err)
	}
	if src.dayCalls != 0 || src.yearCalls != 0 {
		t.Errorf("参数不合法时仍查了数据源: 日 %d 次、年 %d 次", src.dayCalls, src.yearCalls)
	}
}

func TestLookupPropagatesSourceErrors(t *testing.T) {
	boom := errors.New("数据源不可用")
	src := &countingSource{lookupErr: boom}
	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	if _, lookupErr := cal.Lookup(context.Background(), RegionCN, d(2026, time.January, 5)); !errors.Is(lookupErr, boom) {
		t.Errorf("数据源错误未透出: %v", lookupErr)
	}

	src = &countingSource{yearErr: boom}
	cal, err = New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	if _, err := cal.Lookup(context.Background(), RegionCN, d(2026, time.January, 5)); !errors.Is(err, boom) {
		t.Errorf("年份状态读取错误未透出: %v", err)
	}
}

func TestLookupPassesRegionAndDateToSource(t *testing.T) {
	src := &countingSource{dayExists: true, dayRow: DayRow{Date: d(2026, time.January, 5)}}
	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	if _, err := cal.Lookup(context.Background(), RegionCN, d(2026, time.January, 5)); err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if src.lastRegion != RegionCN || src.lastDay != d(2026, time.January, 5) {
		t.Errorf("传给数据源的是 %s %s", src.lastRegion, src.lastDay)
	}
}

// 一年内没有查询的日子不查数据行：有数据行时不该再多问一次年份状态。
func TestLookupSkipsYearStateWhenRowExists(t *testing.T) {
	src := &countingSource{dayExists: true, dayRow: DayRow{Date: d(2026, time.January, 5)}}
	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	if _, err := cal.Lookup(context.Background(), RegionCN, d(2026, time.January, 5)); err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if src.yearCalls != 0 {
		t.Errorf("有数据行时仍查了年份状态 %d 次", src.yearCalls)
	}
}

// At 必须按地区时区切日期，而不是按时刻自带的时区。
func TestAtUsesRegionTimezone(t *testing.T) {
	cal, _ := newCNCalendar(t)
	instant := time.Date(2025, time.December, 31, 16, 30, 0, 0, time.UTC) // 北京 2026-01-01 00:30

	got, err := cal.At(context.Background(), RegionCN, instant)
	if err != nil {
		t.Fatalf("At 出错: %v", err)
	}
	if got.Date != d(2026, time.January, 1) {
		t.Errorf("At 落到 %s，期望 2026-01-01（按北京时区）", got.Date)
	}
	if got.Kind != DayRestDay || got.Name != "元旦" {
		t.Errorf("At 结果 = %s / %q，期望休息日 / 元旦", got.Kind, got.Name)
	}
}

func TestAtUnknownRegion(t *testing.T) {
	cal, _ := newCNCalendar(t)
	if _, err := cal.At(context.Background(), "XX", time.Now()); !errors.Is(err, ErrUnknownRegion) {
		t.Errorf("未知地区应报 ErrUnknownRegion，得到 %v", err)
	}
}

// 跨年的假期：元旦安排里把上一年 12 月 31 日挪成休息，
// 判定以数据行为准，不看那一天属于哪一年的数据集。
func TestLookupCrossYearHoliday(t *testing.T) {
	src := NewMemorySource()
	if err := src.AddDay(RegionCN, DayRow{
		Date:   d(2025, time.December, 31), // 周三
		Name:   "元旦",
		OffDay: true,
	}); err != nil {
		t.Fatalf("AddDay 出错: %v", err)
	}
	cal, err := New(src)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}

	got, err := cal.Lookup(context.Background(), RegionCN, d(2025, time.December, 31))
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got.Kind != DayRestDay {
		t.Errorf("跨年假期判定为 %s，期望休息日", got.Kind)
	}
}

func TestDayKindKnown(t *testing.T) {
	if !DayWorkday.Known() || !DayRestDay.Known() {
		t.Error("工作日与休息日都该是确定结论")
	}
	if DayUnknown.Known() {
		t.Error("未知不该被当成确定结论")
	}
}

func TestUnknownReasonIsNotEmpty(t *testing.T) {
	for _, r := range []UnknownReason{
		ReasonYearPending, ReasonYearDegraded, ReasonYearConflict,
		ReasonYearUnknown, ReasonRowMissing, ReasonRowInconsistent,
	} {
		if strings.TrimSpace(string(r)) == "" {
			t.Error("UnknownReason 不该有空值")
		}
	}
}
