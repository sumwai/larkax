package pricing

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
)

// scheduleCalendar 是最小的一份日历实现，只够 worst-case 用例用。
type scheduleCalendar struct{}

func (scheduleCalendar) TimeZone(region string) (string, error) { return "Asia/Shanghai", nil }

func (scheduleCalendar) DayInfo(region string, day time.Time) (expr.DayInfo, error) {
	return expr.DayInfo{}, nil
}

func (scheduleCalendar) Peak(region string, instant time.Time, windows []expr.PeakWindow) (expr.PeakVerdict, error) {
	return expr.PeakVerdict{}, errors.New("这个日历不判峰谷")
}

// exampleSchedule 是 issue 正文里那张表：五个维度各一条覆盖，position 齐全。
const exampleSchedule = `{
  "tz": "Asia/Shanghai",
  "base": { "prompt": 0.0000015, "completion": 0.0000075 },
  "overrides": [
    { "position": 10, "holidays": ["春节", "国庆节"], "prompt": 0.000003 },
    { "position": 20, "day_kind": "rest",            "prompt": 0.00000075 },
    { "position": 30, "weekdays": ["fri"],           "prompt": 0.000001 },
    { "position": 40, "window": "22:00-08:00",       "prompt": 0.0000005 },
    { "position": 50, "min_context_tokens": 200000,   "prompt": 0.000003 }
  ]
}`

// fixedCalendar 是每一天都给同一份 DayInfo 的日历。
//
// 它用来造「某一天是放假的春节 / 是调休上班的春节 / 只是普通工作日」这几种
// 判定，而 scheduleCalendar 只会给零值。
type fixedCalendar struct{ info expr.DayInfo }

func (fixedCalendar) TimeZone(string) (string, error) { return "Asia/Shanghai", nil }

func (c fixedCalendar) DayInfo(string, time.Time) (expr.DayInfo, error) { return c.info, nil }

func (fixedCalendar) Peak(string, time.Time, []expr.PeakWindow) (expr.PeakVerdict, error) {
	return expr.PeakVerdict{}, errors.New("这个日历不判峰谷")
}

// mustCompileSchedule 解析、校验并编译一张表，任何一步失败就结束用例。
func mustCompileSchedule(t *testing.T, source string) *expr.Program {
	t.Helper()
	sched, err := ParseSchedule([]byte(source))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	compiled, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	prog, err := expr.Compile(compiled)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	return prog
}

// evalPrompt 在给定的日历判定下算一次 prompt 单价。
func evalPrompt(t *testing.T, prog *expr.Program, cal expr.Calendar) float64 {
	t.Helper()
	res, err := prog.Eval(expr.Env{
		At:       time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC),
		Region:   "CN",
		Calendar: cal,
	})
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	return res.Prices.Prompt.Float64()
}

// TestScheduleHolidaysMatchRestDaysOnly 钉住 holidays 的语义是「放假的那几天」。
//
// 调休上班日也带着节日名：春节前后挪用周末上班的那天在日历上 HolidayName
// 是「春节」而 OffDay 是 false。只比名字会把那天算进节假日价，而它实际上
// 是工作日；holidays 因此编译成「名字命中 **且** 不是工作日」。
func TestScheduleHolidaysMatchRestDaysOnly(t *testing.T) {
	const source = `{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [
	    {"holidays": ["春节"], "prompt": 2},
	    {"day_kind": "work", "prompt": 3}
	  ]
	}`
	prog := mustCompileSchedule(t, source)

	cases := []struct {
		name string
		info expr.DayInfo
		want float64
	}{
		{"放假的春节", expr.DayInfo{OffDay: true, HolidayName: "春节"}, 2},
		{"调休上班的春节", expr.DayInfo{OffDay: false, HolidayName: "春节"}, 3},
		{"普通工作日", expr.DayInfo{OffDay: false}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evalPrompt(t, prog, fixedCalendar{info: tc.info}); math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("%s 的 prompt 单价 = %v，期望 %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestScheduleHolidayAndWorkOrderIndependent 钉住「移动数组元素不改价」。
//
// 节假日与 work 两条静态不相交，所以不必给 position；这一对正是判据最容易
// 出错的地方——把调休上班日算进节假日之后，两条会同时命中，价格就取决于
// 数组顺序。
func TestScheduleHolidayAndWorkOrderIndependent(t *testing.T) {
	const forward = `{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [
	    {"holidays": ["春节"], "prompt": 2},
	    {"day_kind": "work", "prompt": 3}
	  ]
	}`
	const reversed = `{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [
	    {"day_kind": "work", "prompt": 3},
	    {"holidays": ["春节"], "prompt": 2}
	  ]
	}`

	forwardProg := mustCompileSchedule(t, forward)
	reversedProg := mustCompileSchedule(t, reversed)

	for _, info := range []expr.DayInfo{
		{OffDay: true, HolidayName: "春节"},
		{OffDay: false, HolidayName: "春节"},
		{OffDay: false},
	} {
		cal := fixedCalendar{info: info}
		a := evalPrompt(t, forwardProg, cal)
		b := evalPrompt(t, reversedProg, cal)
		if math.Abs(a-b) > 1e-12 {
			t.Errorf("DayInfo %+v 下两种数组顺序算出两个价：%v vs %v", info, a, b)
		}
	}
}

// TestScheduleCompilesToPerKeyChain 钉住编译产物形状：每个键一条三元链、
// 链上的次序按 position 从大到小，而不是 2^N 的笛卡尔展开。
func TestScheduleCompilesToPerKeyChain(t *testing.T) {
	sched, err := ParseSchedule([]byte(exampleSchedule))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	src, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}

	// completion 只有 base 一档：它不该被任何条件包起来。
	if !strings.Contains(src, `"completion": 7.5e-06`) {
		t.Errorf("completion 应当是 base 单值，实际源码：%s", src)
	}
	// prompt 一条链：五个条件 = 五个问号。
	if got := strings.Count(src, "?"); got != 5 {
		t.Errorf("生成的源码里有 %d 个三元，期望 5 个（每条覆盖一个）：%s", got, src)
	}

	// 次序：position 大的先命中。按出现位置从前到后核对。
	order := []string{
		"len >= 200000",
		`hour("Asia/Shanghai")`,
		`weekday("Asia/Shanghai") in [5]`,
		"!isWorkday()",
		`holidayName() in ["国庆节", "春节"]`,
	}
	last := -1
	for _, fragment := range order {
		idx := strings.Index(src, fragment)
		if idx < 0 {
			t.Fatalf("源码里找不到 %q：%s", fragment, src)
		}
		if idx <= last {
			t.Errorf("%q 的位置（%d）不在上一个片段之后（%d）：%s", fragment, idx, last, src)
		}
		last = idx
	}
}

// TestScheduleOrderIgnoresArrayOrder 钉住「移动数组元素不改价」。
func TestScheduleOrderIgnoresArrayOrder(t *testing.T) {
	forward, err := ParseSchedule([]byte(exampleSchedule))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	forwardSrc, err := forward.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}

	reversed := `{
  "tz": "Asia/Shanghai",
  "base": { "prompt": 0.0000015, "completion": 0.0000075 },
  "overrides": [
    { "position": 50, "min_context_tokens": 200000,   "prompt": 0.000003 },
    { "position": 40, "window": "22:00-08:00",       "prompt": 0.0000005 },
    { "position": 30, "weekdays": ["fri"],           "prompt": 0.000001 },
    { "position": 20, "day_kind": "rest",            "prompt": 0.00000075 },
    { "position": 10, "holidays": ["春节", "国庆节"], "prompt": 0.000003 }
  ]
}`
	sched, err := ParseSchedule([]byte(reversed))
	if err != nil {
		t.Fatalf("解析倒序表失败: %v", err)
	}
	src, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验倒序表失败: %v", err)
	}
	if src != forwardSrc {
		t.Errorf("数组顺序变了，编译结果也变了：\n正序：%s\n倒序：%s", forwardSrc, src)
	}
}

// TestScheduleWorstCaseWalksEveryBranch 钉住「编译成表之后预留上界照旧成立」。
//
// 这是把表编译成普通表达式、而不是另写一个表匹配器的全部理由：最坏情况
// 求值必须能看到所有分支。若换成「按时刻选一条」的匹配器，它天然只算得出
// 命中的那一支，上界会退化成那一支的价。
func TestScheduleWorstCaseWalksEveryBranch(t *testing.T) {
	const source = `{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 0.000001},
	  "overrides": [
	    {"position": 10, "weekdays": ["mon"], "prompt": 0.000009},
	    {"position": 20, "window": "00:00-12:00", "prompt": 0.000005}
	  ]
	}`
	sched, err := ParseSchedule([]byte(source))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	compiled, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	prog, err := expr.Compile(compiled)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}

	// 上海时间 2026-10-19 09:00（周一上午）：两条覆盖都命中，
	// position 20 的那条（5e-6）赢。
	mondayMorning := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	env := expr.Env{At: mondayMorning, Region: "CN", Calendar: scheduleCalendar{}}
	res, err := prog.Eval(env)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if got := res.Prices.Prompt.Float64(); math.Abs(got-5e-6) > 1e-12 {
		t.Errorf("周一上午的 prompt 单价 = %v，期望 5e-6", got)
	}

	// 上海时间 2026-10-20 01:00（周二凌晨）：只有 22:00-08:00 那条命中。
	tuesdayNight := time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC)
	env.At = tuesdayNight
	res, err = prog.Eval(env)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if got := res.Prices.Prompt.Float64(); math.Abs(got-5e-6) > 1e-12 {
		t.Errorf("周二凌晨的 prompt 单价 = %v，期望 5e-6", got)
	}

	// 上海时间 2026-10-20 12:30（周二中午）：两条都不命中，落到 base。
	tuesdayNoon := time.Date(2026, 10, 20, 4, 30, 0, 0, time.UTC)
	env.At = tuesdayNoon
	res, err = prog.Eval(env)
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	if got := res.Prices.Prompt.Float64(); math.Abs(got-1e-6) > 1e-12 {
		t.Errorf("周二中午的 prompt 单价 = %v，期望 1e-6", got)
	}

	// 最坏情况不管落在哪一刻，都取所有分支的最大值：9e-6。
	for _, at := range []time.Time{mondayMorning, tuesdayNight, tuesdayNoon} {
		env.At = at
		worst, err := prog.EvalWorstCase(env)
		if err != nil {
			t.Fatalf("%v 的最坏情况求值失败: %v", at, err)
		}
		if got := worst.Prices.Prompt.Float64(); math.Abs(got-9e-6) > 1e-12 {
			t.Errorf("%v 的最坏情况 prompt 单价 = %v，期望 9e-6（所有分支的最大值）", at, got)
		}
	}
}

// TestScheduleNeedsCalendar 钉住「哪几个维度要日历」。
//
// 判据只影响写入路径要不要拒绝一条规则，所以它必须与编译产物一致：
// 生成源码里出现 isWorkday / holidayName 的那些维度才返回 true。
func TestScheduleNeedsCalendar(t *testing.T) {
	cases := []struct {
		name     string
		schedule string
		want     bool
	}{
		{"只有 base", `{"tz":"Asia/Shanghai","base":{"prompt":1}}`, false},
		{"weekdays", `{"tz":"Asia/Shanghai","base":{"prompt":1},
			"overrides":[{"weekdays":["fri"],"prompt":2}]}`, false},
		{"window", `{"tz":"Asia/Shanghai","base":{"prompt":1},
			"overrides":[{"window":"22:00-08:00","prompt":2}]}`, false},
		{"门槛", `{"tz":"Asia/Shanghai","base":{"prompt":1},
			"overrides":[{"min_context_tokens":100,"prompt":2}]}`, false},
		{"day_kind", `{"tz":"Asia/Shanghai","base":{"prompt":1},
			"overrides":[{"day_kind":"rest","prompt":2}]}`, true},
		{"holidays", `{"tz":"Asia/Shanghai","base":{"prompt":1},
			"overrides":[{"holidays":["春节"],"prompt":2}]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched, err := ParseSchedule([]byte(tc.schedule))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := sched.NeedsCalendar(); got != tc.want {
				t.Errorf("NeedsCalendar() = %v，期望 %v", got, tc.want)
			}
		})
	}
	if (*Schedule)(nil).NeedsCalendar() {
		t.Error("nil 表不应声称要用日历")
	}
}

// TestScheduleMinThresholdOrder 钉住门槛的包含关系自动化简：门槛高的先命中。
func TestScheduleMinThresholdOrder(t *testing.T) {
	sched, err := ParseSchedule([]byte(`{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [
	    {"min_context_tokens": 100, "prompt": 2},
	    {"min_context_tokens": 200, "prompt": 3}
	  ]
	}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	src, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	high := strings.Index(src, "len >= 200")
	low := strings.Index(src, "len >= 100")
	if high < 0 || low < 0 || high > low {
		t.Errorf("门槛高的应当先命中（200 在 100 之前）：%s", src)
	}
}

// TestScheduleBaseOnly 钉住「没有任何覆盖」也是合法的一张表。
func TestScheduleBaseOnly(t *testing.T) {
	sched, err := ParseSchedule([]byte(`{"tz":"Asia/Shanghai","base":{"prompt":1.5,"completion":7.5}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	src, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if src != `{"completion": 7.5, "prompt": 1.5}` {
		t.Errorf("生成的源码 = %s", src)
	}
}

// TestScheduleRejects 钉住写入时该拒的几种表。
//
// 每一种在运行期都表现为「静默算错价」——没有一条会在库层面报错，
// 所以判据必须落在保存那一步。
func TestScheduleRejects(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{
			name:    "重复键",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1,"prompt":2}}`,
			wantSub: "重复的键",
		},
		{
			name:    "未知顶层字段",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overide":[]}`,
			wantSub: "未知的字段",
		},
		{
			name:    "缺 tz",
			raw:     `{"base":{"prompt":1}}`,
			wantSub: "缺少 tz",
		},
		{
			name:    "坏 tz",
			raw:     `{"tz":"Mars/Olympus","base":{"prompt":1}}`,
			wantSub: "无法解析",
		},
		{
			name:    "空 base",
			raw:     `{"tz":"Asia/Shanghai","base":{}}`,
			wantSub: "base 不能为空",
		},
		{
			name:    "units 不支持",
			raw:     `{"tz":"Asia/Shanghai","base":{"units":{"img":1}}}`,
			wantSub: "没有单值",
		},
		{
			name:    "负单价",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"weekdays":["fri"],"prompt":-1}]}`,
			wantSub: "单价不能为负",
		},
		{
			name:    "没有条件",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"prompt":2}]}`,
			wantSub: "没有任何条件",
		},
		{
			name:    "没有单价",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"weekdays":["fri"]}]}`,
			wantSub: "没有任何单价",
		},
		{
			name:    "覆盖里的键不在 base",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"weekdays":["fri"],"completion":2}]}`,
			wantSub: "不在 base 里",
		},
		{
			name:    "work 与节假日同现",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"day_kind":"work","holidays":["春节"],"prompt":2}]}`,
			wantSub: "自相矛盾",
		},
		{
			name:    "星期缩写写错",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"weekdays":["friday"],"prompt":2}]}`,
			wantSub: "不是星期缩写",
		},
		{
			name:    "窗口带分钟",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"window":"08:30-12:00","prompt":2}]}`,
			wantSub: "不是整点边界",
		},
		{
			name:    "窗口覆盖全天",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"window":"00:00-00:00","prompt":2}]}`,
			wantSub: "覆盖全天",
		},
		{
			name: "同一维度重叠（星期）",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"weekdays":["fri"],"prompt":2},
				{"weekdays":["fri","sat"],"prompt":3}]}`,
			wantSub: "同一维度上重叠",
		},
		{
			name: "同一维度重叠（时段，跨零点）",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"window":"22:00-08:00","prompt":2},
				{"window":"23:00-01:00","prompt":3}]}`,
			wantSub: "同一维度上重叠",
		},
		{
			name: "跨维度没有 position",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"weekdays":["sat"],"prompt":2},
				{"day_kind":"work","prompt":3}]}`,
			wantSub: "都要显式给 position",
		},
		{
			name: "跨维度 position 相同",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"position":10,"weekdays":["sat"],"prompt":2},
				{"position":10,"day_kind":"work","prompt":3}]}`,
			wantSub: "position 相同",
		},
		{
			name: "门槛与 position 方向相反",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"position":10,"min_context_tokens":200,"prompt":2},
				{"position":20,"min_context_tokens":100,"prompt":3}]}`,
			wantSub: "方向相反",
		},
		{
			name:    "position 为负",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"position":-1,"weekdays":["fri"],"prompt":2}]}`,
			wantSub: "position 不能为负",
		},
		{
			name:    "门槛为零",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"min_context_tokens":0,"prompt":2}]}`,
			wantSub: "门槛必须大于 0",
		},
		{
			name:    "day_kind 取值不对",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"day_kind":"holiday","prompt":2}]}`,
			wantSub: "不是取值",
		},
		{
			name:    "节假日名是空白",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"holidays":["  "],"prompt":2}]}`,
			wantSub: "空名字",
		},
		{
			name:    "覆盖对象内部的重复键",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[{"weekdays":["fri"],"prompt":2,"prompt":3}]}`,
			wantSub: "重复的键",
		},
		{
			name: "同一维度重叠（节假日）",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"holidays":["春节"],"prompt":2},
				{"holidays":["春节","国庆节"],"prompt":3}]}`,
			wantSub: "同一维度上重叠",
		},
		{
			name: "同一维度重叠（相等的门槛）",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"min_context_tokens":100,"prompt":2},
				{"min_context_tokens":100,"prompt":3}]}`,
			wantSub: "同一维度上重叠",
		},
		{
			name: "同一维度重叠（时段，不跨零点）",
			raw: `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
				{"window":"08:00-12:00","prompt":2},
				{"window":"10:00-14:00","prompt":3}]}`,
			wantSub: "同一维度上重叠",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched, err := ParseSchedule([]byte(tc.raw))
			if err != nil {
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("解析报错 = %v，期望包含 %q", err, tc.wantSub)
				}
				return
			}
			_, err = sched.Validate()
			if err == nil {
				t.Fatalf("这张表应当被拒：%s", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("报错 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}

// TestScheduleErrorNamesTheOverride 钉住报错指着第几条覆盖，而不是第几列。
func TestScheduleErrorNamesTheOverride(t *testing.T) {
	sched, err := ParseSchedule([]byte(`{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [
	    {"position": 10, "weekdays": ["fri"], "prompt": 2},
	    {"position": 20, "min_context_tokens": 100, "prompt": -3}
	  ]
	}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	_, err = sched.Validate()
	if err == nil {
		t.Fatal("负单价应当被拒")
	}
	for _, want := range []string{"第 2 条 override", "prompt", "单价不能为负"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错 %q 里没有 %q", err.Error(), want)
		}
	}
}

// TestScheduleQuoteInHolidayName 钉住生成源码里的字符串转义。
//
// 节假日名来自表，作者写什么名字都可能；一个引号没转义就会把整条表达式
// 变成语法错，而错误指向的位置与作者写的那一行对不上。
func TestScheduleQuoteInHolidayName(t *testing.T) {
	sched, err := ParseSchedule([]byte(`{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1},
	  "overrides": [{"holidays": ["带\"引号\"的名字"], "prompt": 2}]
	}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	src, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if !strings.Contains(src, `holidayName() in ["带\"引号\"的名字"]`) {
		t.Errorf("生成源码里的名字没有正确转义：%s", src)
	}
}

// TestScheduleFillsEveryBaseKey 钉住「覆盖只改它写了的键，其余键仍是 base」。
func TestScheduleFillsEveryBaseKey(t *testing.T) {
	sched, err := ParseSchedule([]byte(`{
	  "tz": "Asia/Shanghai",
	  "base": {"prompt": 1, "completion": 2, "fixed": 0},
	  "overrides": [{"weekdays": ["fri"], "prompt": 3}]
	}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	src, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	for _, plain := range []string{`"completion": 2`, `"fixed": 0`} {
		if !strings.Contains(src, plain) {
			t.Errorf("没有覆盖的键应当保持 base 单值，找不到 %q：%s", plain, src)
		}
	}
	if !strings.Contains(src, `weekday("Asia/Shanghai") in [5]`) {
		t.Errorf("prompt 的覆盖没有生成：%s", src)
	}
	if got := strings.Count(src, "?"); got != 1 {
		t.Errorf("只有 prompt 该有链，实际 %d 个三元：%s", got, src)
	}
}

// TestScheduleParseRejectsBadDocuments 钉住 JSON 层那几种「不成立」的形状。
//
// 它们都发生在解码阶段，报错来自 ParseSchedule 而不是 Validate；分开放是
// 因为写入路径对两者的处置相同（都是拒绝），但调用方拿到的是不同的函数。
func TestScheduleParseRejectsBadDocuments(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{"空输入", "", "定价表为空"},
		{"只有空白", " \n\t ", "定价表为空"},
		{
			name:    "第一个对象之后还有内容",
			raw:     `{"tz":"Asia/Shanghai","base":{"prompt":1}} {"tz":"Asia/Shanghai","base":{"prompt":2}}`,
			wantSub: "多余的内容",
		},
		{"顶层是数组", `[]`, "定价表解析失败"},
		{"顶层是标量", `42`, "定价表解析失败"},
		{"base 不是对象", `{"tz":"Asia/Shanghai","base":42}`, "必须是单价对象"},
		{"overrides 不是数组", `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":{}}`, "overrides"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSchedule([]byte(tc.raw))
			if err == nil {
				t.Fatalf("这份输入应当被拒：%q", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("报错 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}

// TestScheduleOrderTiesBreakByIndex 钉住「都没有 position 时按数组下标」。
//
// 两条互不相交的时段本来就不必给 position，先后只影响生成源码的排布；
// 固定的次序让「同一张表编译两次得到同一份源码」成立，对账时不用怀疑
// 顺序是从哪来的。
func TestScheduleOrderTiesBreakByIndex(t *testing.T) {
	const firstMorning = `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
		{"window":"08:00-10:00","prompt":2},
		{"window":"10:00-12:00","prompt":3}]}`
	const firstNoon = `{"tz":"Asia/Shanghai","base":{"prompt":1},"overrides":[
		{"window":"10:00-12:00","prompt":3},
		{"window":"08:00-10:00","prompt":2}]}`

	morning := mustCompileSource(t, firstMorning)
	noon := mustCompileSource(t, firstNoon)

	eight := `hour("Asia/Shanghai") >= 8`
	ten := `hour("Asia/Shanghai") >= 10`
	if strings.Index(morning, eight) > strings.Index(morning, ten) {
		t.Errorf("正序表里第一条应当先出现：%s", morning)
	}
	if strings.Index(noon, ten) > strings.Index(noon, eight) {
		t.Errorf("倒序表里第一条应当先出现：%s", noon)
	}
}

// mustCompileSource 解析并校验一张表，返回生成的表达式源码。
func mustCompileSource(t *testing.T, source string) string {
	t.Helper()
	sched, err := ParseSchedule([]byte(source))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	compiled, err := sched.Validate()
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	return compiled
}
