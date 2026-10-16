package host

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// fakeCalendar 是一个可编排的日历数据源。
//
// 它同时记录被调用的次数、拿到的 ctx 与参数，用来钉住两件事：
// 超时/非法参数时**一次都不查**，以及宿主把 ctx 继续传了下去。
type fakeCalendar struct {
	calls   int
	lastCtx context.Context
	lastReg string
	lastAt  time.Time

	// info 是 Day 的返回值；err 非空时 Day 直接报错。
	info DayInfo
	err  error
	// peak 是 Peak 的返回值。
	peak    string
	peakErr error
}

func (f *fakeCalendar) Day(ctx context.Context, region string, instant time.Time) (DayInfo, error) {
	f.calls++
	f.lastCtx, f.lastReg, f.lastAt = ctx, region, instant
	if f.err != nil {
		return DayInfo{}, f.err
	}
	return f.info, nil
}

func (f *fakeCalendar) Peak(ctx context.Context, region string, instant time.Time) (string, error) {
	f.calls++
	f.lastCtx, f.lastReg, f.lastAt = ctx, region, instant
	return f.peak, f.peakErr
}

// calEnv 造一个只有日历能力的 Env。
func calEnv(cal CalendarSource) Env {
	return Env{
		Plugin:   "test-plugin",
		Logger:   testLogger(&bytes.Buffer{}),
		Calendar: cal,
		Now:      func() time.Time { return testInstant },
	}
}

func TestCalendarKindReportsUnknown(t *testing.T) {
	// 未知是独立状态：如实回答 "unknown"，不压成 workday。
	// 静默压成工作日会让一整天按错误分类计价，而账上看不出来。
	cases := []struct {
		name string
		info DayInfo
		want string
	}{
		{"工作日", DayInfo{Kind: kindWorkday}, "workday"},
		{"休息日", DayInfo{Kind: kindRestDay}, "rest_day"},
		{"未知", DayInfo{Kind: kindUnknown}, "unknown"},
		{"契约外的取值也如实说不知道", DayInfo{Kind: "半休"}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := &fakeCalendar{info: tc.info}
			fn := capability(t, Standard(calEnv(cal)), capCalendar).Funcs["kind"]

			got, err := fn(context.Background(), js(int64(testInstant.Unix()), "CN"))
			if err != nil {
				t.Fatalf("kind 出错: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("kind = %q，期望 %q", got.String(), tc.want)
			}
			if cal.lastReg != "CN" {
				t.Errorf("地区参数 = %q", cal.lastReg)
			}
			if !cal.lastAt.Equal(testInstant) {
				t.Errorf("时刻 = %v，期望 %v", cal.lastAt, testInstant)
			}
		})
	}
}

// TestCalendarTimestampIsSeconds 钉住「时间戳按秒解释」（JS 惯例 Date.now()/1000）。
// 按毫秒解释会让判定落在 1970 年附近，而那是一个看起来「有结果」的错答案。
func TestCalendarTimestampIsSeconds(t *testing.T) {
	cal := &fakeCalendar{info: DayInfo{Kind: kindWorkday}}
	fn := capability(t, Standard(calEnv(cal)), capCalendar).Funcs["kind"]

	if _, err := fn(context.Background(), js(int64(1700000000), "CN")); err != nil {
		t.Fatalf("kind 出错: %v", err)
	}
	if want := time.Unix(1700000000, 0); !cal.lastAt.Equal(want) {
		t.Errorf("时刻 = %v，期望 %v（按秒解释）", cal.lastAt, want)
	}

	// 按毫秒解释的话会落到 1970 年，这里反过来钉一下它不是那个值。
	if cal.lastAt.Equal(time.Unix(1700000000000/1000, 0)) && cal.lastAt.Year() < 2000 {
		t.Error("时间戳被当成了毫秒")
	}
}

// TestIsOffDayErrorsOnUnknown 是这条行为要求里最要紧的一条：
// 未知时**返回错误**，而不是 false。
//
// false 的意思是「这一天是工作日」，那是一个具体的判断，而未知不是。
// 压成 false 等于替插件猜了一次，猜错的后果是整天按错误分类计价。
func TestIsOffDayErrorsOnUnknown(t *testing.T) {
	cases := []struct {
		name       string
		info       DayInfo
		wantOff    bool
		wantErr    bool
		reasonText string
	}{
		{"休息日", DayInfo{Kind: kindRestDay}, true, false, "休息日本身就是 off day"},
		{"工作日", DayInfo{Kind: kindWorkday}, false, false, "工作日是 off day 的否"},
		{"调休上班的周末", DayInfo{Kind: kindWorkday, MakeupWorkday: true}, false, false, "调休上班那天是上班日，所以不是 off day"},
		{"未知", DayInfo{Kind: kindUnknown}, false, true, "未知不是 false：抛错而不是替插件猜"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := &fakeCalendar{info: tc.info}
			fn := capability(t, Standard(calEnv(cal)), capCalendar).Funcs["isOffDay"]

			got, err := fn(context.Background(), js(int64(testInstant.Unix()), "CN"))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("未知时应当报错，却返回了 %v（%s）", got.String(), tc.reasonText)
				}
				return
			}
			if err != nil {
				t.Fatalf("isOffDay 出错: %v", err)
			}
			if got.AsBool() != tc.wantOff {
				t.Errorf("isOffDay = %v，期望 %v（%s）", got.AsBool(), tc.wantOff, tc.reasonText)
			}
		})
	}
}

// TestIsMakeupWorkday 与 isOffDay 同一条保守规则：未知时报错，不返回 false。
func TestIsMakeupWorkday(t *testing.T) {
	cases := []struct {
		name    string
		info    DayInfo
		want    bool
		wantErr bool
	}{
		{"调休上班", DayInfo{Kind: kindWorkday, MakeupWorkday: true}, true, false},
		{"普通工作日", DayInfo{Kind: kindWorkday}, false, false},
		{"休息日", DayInfo{Kind: kindRestDay}, false, false},
		{"未知", DayInfo{Kind: kindUnknown}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := &fakeCalendar{info: tc.info}
			fn := capability(t, Standard(calEnv(cal)), capCalendar).Funcs["isMakeupWorkday"]

			got, err := fn(context.Background(), js(int64(testInstant.Unix()), "CN"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("错误 = %v，期望报错 %v", err, tc.wantErr)
			}
			if err == nil && got.AsBool() != tc.want {
				t.Errorf("= %v，期望 %v", got.AsBool(), tc.want)
			}
		})
	}
}

// TestHolidayName 名称不是判定，所以缺名字返回 null（伪值）而不是错误。
func TestHolidayName(t *testing.T) {
	cases := []struct {
		name     string
		info     DayInfo
		wantNull bool
		want     string
	}{
		{"有名字", DayInfo{Kind: kindRestDay, Name: "样例节"}, false, "样例节"},
		{"没有名字", DayInfo{Kind: kindWorkday}, true, ""},
		{"未知也没有名字", DayInfo{Kind: kindUnknown}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := &fakeCalendar{info: tc.info}
			fn := capability(t, Standard(calEnv(cal)), capCalendar).Funcs["holidayName"]

			got, err := fn(context.Background(), js(int64(testInstant.Unix()), "CN"))
			if err != nil {
				t.Fatalf("holidayName 出错: %v", err)
			}
			if tc.wantNull {
				if !got.IsNull() {
					t.Fatalf("= %q，期望 null", got.String())
				}
				return
			}
			if got.String() != tc.want {
				t.Errorf("= %q，期望 %q", got.String(), tc.want)
			}
		})
	}
}

// TestCalendarHourAndWeekday 覆盖 hourIn / weekdayIn：
// 它们用 env.Now() 的当前时刻，而不是参数里的时间戳。
func TestCalendarHourAndWeekday(t *testing.T) {
	cases := []struct {
		name    string
		fn      string
		info    DayInfo
		want    int64
		wantErr string
	}{
		{"hourIn 正常", "hourIn", DayInfo{Kind: kindWorkday, LocalHour: 9}, 9, ""},
		{"hourIn 0 点", "hourIn", DayInfo{Kind: kindWorkday, LocalHour: 0}, 0, ""},
		{"hourIn 23 点", "hourIn", DayInfo{Kind: kindWorkday, LocalHour: 23}, 23, ""},
		{"hourIn 越界", "hourIn", DayInfo{Kind: kindWorkday, LocalHour: 24}, 0, "超出范围"},
		{"hourIn 负数", "hourIn", DayInfo{Kind: kindWorkday, LocalHour: -1}, 0, "超出范围"},
		{"weekdayIn 周日", "weekdayIn", DayInfo{Kind: kindRestDay, LocalWeekday: 0}, 0, ""},
		{"weekdayIn 周六", "weekdayIn", DayInfo{Kind: kindRestDay, LocalWeekday: 6}, 6, ""},
		{"weekdayIn 越界", "weekdayIn", DayInfo{Kind: kindWorkday, LocalWeekday: 7}, 0, "超出范围"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := &fakeCalendar{info: tc.info}
			fn := capability(t, Standard(calEnv(cal)), capCalendar).Funcs[tc.fn]

			got, err := fn(context.Background(), js("CN"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %v，期望含 %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s 出错: %v", tc.fn, err)
			}
			if got.String() != moejs.Int(tc.want).String() {
				t.Errorf("%s = %q，期望 %d", tc.fn, got.String(), tc.want)
			}
			// 用当前时刻，而不是参数里的时间戳。
			if !cal.lastAt.Equal(testInstant) {
				t.Errorf("时刻 = %v，期望 env.Now() 的 %v", cal.lastAt, testInstant)
			}
		})
	}
}

// TestCalendarRejectsBadArguments 钉住「参数不合法就报错」，
// 并且**在报错之前一次都不去查数据源**。
func TestCalendarRejectsBadArguments(t *testing.T) {
	cases := []struct {
		name  string
		fn    string
		args  []moejs.Value
		wantR string
	}{
		{"ts 缺失", "kind", js("", "CN"), "时间戳"},
		{"ts 是 null", "kind", js(nil, "CN"), "时间戳"},
		{"ts 是小数", "kind", js("1.5", "CN"), "时间戳"},
		{"ts 不是数字", "kind", js("昨天", "CN"), "时间戳"},
		{"region 缺失", "kind", js(int64(1700000000)), "地区"},
		{"region 是 null", "kind", js(int64(1700000000), nil), "地区"},
		{"isOffDay region 缺失", "isOffDay", js(int64(1700000000)), "地区"},
		{"isMakeupWorkday ts 缺失", "isMakeupWorkday", js("", "CN"), "时间戳"},
		{"isMakeupWorkday region 缺失", "isMakeupWorkday", js(int64(1700000000)), "地区"},
		{"holidayName ts 是小数", "holidayName", js("1.5", "CN"), "时间戳"},
		{"hourIn region 缺失", "hourIn", nil, "地区"},
		{"weekdayIn region 空串", "weekdayIn", js(""), "地区"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cal := &fakeCalendar{info: DayInfo{Kind: kindWorkday}}
			fn := capability(t, Standard(calEnv(cal)), capCalendar).Funcs[tc.fn]

			_, err := fn(context.Background(), tc.args)
			if err == nil {
				t.Fatal("非法参数应当报错，而不是静默按零值处理")
			}
			if !strings.Contains(err.Error(), tc.wantR) {
				t.Errorf("错误 = %v，期望提到 %q", err, tc.wantR)
			}
			if cal.calls != 0 {
				t.Errorf("参数不合法却查了 %d 次日历", cal.calls)
			}
		})
	}
}

// TestCalendarSourceErrorSurfaces 确认数据源报错时如实往上抛，
// 而不是回落成某个判定结果。
func TestCalendarSourceErrorSurfaces(t *testing.T) {
	sentinel := errors.New("数据源不可用")
	for _, tc := range []struct {
		name string
		args []moejs.Value
	}{
		{"kind", js(int64(testInstant.Unix()), "CN")},
		{"isOffDay", js(int64(testInstant.Unix()), "CN")},
		{"isMakeupWorkday", js(int64(testInstant.Unix()), "CN")},
		{"holidayName", js(int64(testInstant.Unix()), "CN")},
		{"hourIn", js("CN")},
		{"weekdayIn", js("CN")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cal := &fakeCalendar{err: sentinel}
			impl := capability(t, Standard(calEnv(cal)), capCalendar).Funcs[tc.name]

			_, err := impl(context.Background(), tc.args)
			if !errors.Is(err, sentinel) {
				t.Fatalf("错误 = %v，期望包住 %v", err, sentinel)
			}
		})
	}
}

func TestCalendarNotAvailableWithoutSource(t *testing.T) {
	c := capability(t, Standard(Env{}), capCalendar)
	if c.Available() {
		t.Error("Calendar 为 nil 时 Available() 应当为 false")
	}
}

// TestCalendarUsesNowWhenNotInjected 覆盖 Now 为 nil 的兜底：改用 time.Now()。
func TestCalendarUsesNowWhenNotInjected(t *testing.T) {
	cal := &fakeCalendar{info: DayInfo{Kind: kindWorkday, LocalHour: 1}}
	env := Env{Logger: testLogger(&bytes.Buffer{}), Calendar: cal}
	// 不设 Now。

	before := time.Now()
	if _, err := capability(t, Standard(env), capCalendar).Funcs["hourIn"](context.Background(), js("CN")); err != nil {
		t.Fatalf("hourIn 出错: %v", err)
	}
	if cal.lastAt.Before(before.Add(-time.Second)) || cal.lastAt.After(time.Now().Add(time.Second)) {
		t.Errorf("没有注入 Now 时应当用当前时刻，得到 %v", cal.lastAt)
	}
}
