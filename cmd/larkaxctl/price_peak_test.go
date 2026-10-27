package main

import (
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/pricing"
)

// peakScheduleBase 是一份只有兜底单价的表：峰段由 price peak add 追加。
//
// 单价按每 token 写（与 --schedule-file 的口径一致），旗标那一侧给的是每百万，
// 换算在写入时完成——用例两边的期望值都来自这张表与命令行上的字面量。
const peakScheduleBase = `{
  "tz": "Asia/Shanghai",
  "base": { "prompt": 0.000001, "completion": 0.000005 }
}`

// addPeakRule 建一条带结构化定价表的规则，返回它的 ID。
func addPeakRule(t *testing.T) int64 {
	t.Helper()
	return addPriceRule(t, []string{
		"--scope", "platform", "--name", uniqueName("peak"),
		"--models", uniqueName("peak-model"),
		"--schedule-file", writeScheduleFile(t, peakScheduleBase),
	})
}

// runPeakAdd 跑一次 price peak add，返回它打出来的段号与 stderr。
func runPeakAdd(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runPricePeak(append([]string{"add"}, args...))
	})
	if err != nil {
		t.Fatalf("price peak add 失败: %v（stderr=%q）", err, stderr)
	}
	segment, parseErr := strconv.Atoi(strings.TrimSpace(stdout))
	if parseErr != nil {
		t.Fatalf("stdout 不是段号：%q（%v）", stdout, parseErr)
	}
	return segment, stderr
}

// currentSchedule 从库里读回一条规则的定价表。
func currentSchedule(t *testing.T, db *sql.DB, id int64) *pricing.Schedule {
	t.Helper()
	info := findRule(t, listRules(t, db), id)
	if info.Schedule == nil {
		t.Fatalf("规则 %d 从库里读回来没有定价表", id)
	}
	return info.Schedule
}

// evalPromptAt 按规则落库的表达式求一次 prompt 单价。
//
// 峰段落在表达式里（hour/weekday 条件），所以「这一段算不算峰」的判据就是这一次
// 求值——price explain 只列适用规则与裁决次序，不打单价，看不出峰谷。
func evalPromptAt(t *testing.T, source string, at time.Time, usage billing.Usage) float64 {
	t.Helper()
	prog, err := expr.Compile(source)
	if err != nil {
		t.Fatalf("落库的表达式编译不过: %v\n%s", err, source)
	}
	res, err := prog.Eval(expr.Env{At: at, Usage: usage})
	if err != nil {
		t.Fatalf("求值失败: %v", err)
	}
	return res.Prices.Prompt.Float64()
}

// shanghaiHour 造一个上海时间某天某点的时刻。
func shanghaiHour(t *testing.T, year int, month time.Month, day, hour int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	return time.Date(year, month, day, hour, 0, 0, 0, loc)
}

// TestPricePeakAddLandsTwoWindows 钉住「两段窗口同星期落成一条覆盖，峰谷判定正确」。
//
// 两段（01:00-04:00 与 06:00-10:00）写在同一段覆盖的 window 里：那个字段本身就是
// 逗号分隔的多段，四条判据取峰内、缝里、峰内、峰外各一次，每段都各自命中。
func TestPricePeakAddLandsTwoWindows(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)
	id := addPeakRule(t)
	idArg := strconv.FormatInt(id, 10)

	segment, stderr := runPeakAdd(t, idArg,
		"--tz", "Asia/Shanghai",
		"--window", "01:00-04:00,06:00-10:00",
		"--weekdays", "mon-fri",
		"--in", "0.3", "--out", "1.2",
		"--by", by)
	if segment != 1 {
		t.Errorf("add 打出的段号 = %d，期望 1", segment)
	}
	if !strings.Contains(stderr, "第 1 段峰段") {
		t.Errorf("add 的回显里没有段号：%q", stderr)
	}

	schedule := currentSchedule(t, db, id)
	if len(schedule.Overrides) != 1 {
		t.Fatalf("落库之后有 %d 段覆盖，期望 1 段", len(schedule.Overrides))
	}
	got := schedule.Overrides[0]
	if got.Window != "01:00-04:00,06:00-10:00" {
		t.Errorf("window = %q，期望 %q", got.Window, "01:00-04:00,06:00-10:00")
	}
	if want := []string{"mon", "tue", "wed", "thu", "fri"}; !slices.Equal(got.Weekdays, want) {
		t.Errorf("weekdays = %v，期望 %v（mon-fri 展开成五个缩写）", got.Weekdays, want)
	}
	if got.MinContextTokens != nil {
		t.Errorf("没给 --above 时 min_context_tokens = %d，期望不写这一栏", *got.MinContextTokens)
	}
	if got.DayKind != "" {
		t.Errorf("没给 --exclude-holidays 时 day_kind = %q，期望空", got.DayKind)
	}
	if len(got.Prices) != 2 || got.Prices["prompt"] != 3e-7 || got.Prices["completion"] != 1.2e-6 {
		t.Errorf("单价 = %v，期望 prompt=3e-07、completion=1.2e-06（每百万 0.3 / 1.2 换算成每 token）",
			got.Prices)
	}

	source := findRule(t, listRules(t, db), id).Expr
	for _, tc := range []struct {
		what string
		at   time.Time
		want float64
	}{
		{"周一 02:00（第一段里）", shanghaiHour(t, 2026, 10, 19, 2), 3e-7},
		{"周一 05:00（两段之间的缝）", shanghaiHour(t, 2026, 10, 19, 5), 1e-6},
		{"周一 08:00（第二段里）", shanghaiHour(t, 2026, 10, 19, 8), 3e-7},
		{"周一 12:00（两段之外）", shanghaiHour(t, 2026, 10, 19, 12), 1e-6},
		{"周六 02:00（窗口里但不是 mon-fri）", shanghaiHour(t, 2026, 10, 24, 2), 1e-6},
	} {
		if got := evalPromptAt(t, source, tc.at, billing.Usage{}); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s 的 prompt 单价 = %v，期望 %v", tc.what, got, tc.want)
		}
	}
}

// TestPricePeakRejectsOverlappingWindow 钉住「同维度重叠被拒、库里不变」。
func TestPricePeakRejectsOverlappingWindow(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)
	id := addPeakRule(t)
	idArg := strconv.FormatInt(id, 10)

	runPeakAdd(t, idArg,
		"--tz", "Asia/Shanghai", "--window", "01:00-04:00",
		"--in", "0.3", "--by", by)

	err := runCaptured(t, func() error {
		return runPricePeak([]string{
			"add", idArg,
			"--tz", "Asia/Shanghai", "--window", "03:00-06:00",
			"--in", "0.4", "--by", by,
		})
	})
	if err == nil {
		t.Fatal("与已有覆盖重叠的峰段应当被拒")
	}
	if !strings.Contains(err.Error(), "同一维度上重叠") {
		t.Errorf("错误信息没点出重叠：%v", err)
	}

	schedule := currentSchedule(t, db, id)
	if len(schedule.Overrides) != 1 {
		t.Fatalf("被拒之后有 %d 段覆盖，期望还是 1 段", len(schedule.Overrides))
	}
	if got := schedule.Overrides[0].Window; got != "01:00-04:00" {
		t.Errorf("被拒之后 window = %q，期望还是 %q", got, "01:00-04:00")
	}
}

// TestPricePeakRejectsTZConflict 钉住「一张表一个时区」：--tz 与表里的不一致是参数错。
func TestPricePeakRejectsTZConflict(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)
	id := addPeakRule(t)
	idArg := strconv.FormatInt(id, 10)

	err := runCaptured(t, func() error {
		return runPricePeak([]string{
			"add", idArg,
			"--tz", "UTC", "--window", "01:00-04:00",
			"--in", "0.3", "--by", by,
		})
	})
	if err == nil {
		t.Fatal("--tz 与表里的时区不一致时应当被拒")
	}
	if code := cli.ExitCode(err); code != cli.ExitUsage {
		t.Errorf("退出码 = %d，期望 %d（参数错）", code, cli.ExitUsage)
	}
	for _, want := range []string{"UTC", "Asia/Shanghai", "不一致"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里没有 %q：%v", want, err)
		}
	}

	if schedule := currentSchedule(t, db, id); len(schedule.Overrides) != 0 {
		t.Errorf("被拒之后库里多了 %d 段覆盖，期望一段都没有", len(schedule.Overrides))
	}
}

// TestPricePeakRemoveBySegment 钉住「add 打出的段号能直接给 remove」。
func TestPricePeakRemoveBySegment(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)
	id := addPeakRule(t)
	idArg := strconv.FormatInt(id, 10)

	first, _ := runPeakAdd(t, idArg,
		"--tz", "Asia/Shanghai", "--window", "01:00-04:00", "--in", "0.3", "--by", by)
	second, _ := runPeakAdd(t, idArg,
		"--tz", "Asia/Shanghai", "--window", "06:00-10:00", "--in", "0.5", "--by", by)
	if first != 1 || second != 2 {
		t.Fatalf("两次 add 的段号 = %d、%d，期望 1、2", first, second)
	}

	before, _ := captureStreams(t, func() {
		if err := runPricePeak([]string{"list", idArg}); err != nil {
			t.Fatalf("price peak list 失败: %v", err)
		}
	})
	for _, want := range []string{"01:00-04:00", "06:00-10:00"} {
		if !strings.Contains(before, want) {
			t.Errorf("删之前 list 里没有 %q：\n%s", want, before)
		}
	}

	var err error
	_, stderr := captureStreams(t, func() {
		err = runPricePeak([]string{"remove", idArg, strconv.Itoa(second), "--by", by})
	})
	if err != nil {
		t.Fatalf("price peak remove 失败: %v", err)
	}
	if !strings.Contains(stderr, "第 2 段峰段") {
		t.Errorf("remove 的回显里没有段号：%q", stderr)
	}

	if schedule := currentSchedule(t, db, id); len(schedule.Overrides) != 1 {
		t.Fatalf("删掉一段之后还有 %d 段覆盖，期望 1 段", len(schedule.Overrides))
	}
	after, _ := captureStreams(t, func() {
		if listErr := runPricePeak([]string{"list", idArg}); listErr != nil {
			t.Fatalf("price peak list 失败: %v", listErr)
		}
	})
	if !strings.Contains(after, "01:00-04:00") || strings.Contains(after, "06:00-10:00") {
		t.Errorf("删之后 list 应当只剩第一段：\n%s", after)
	}

	// 删掉的那一段回到 base，留下的那一段照旧命中。
	source := findRule(t, listRules(t, db), id).Expr
	if got := evalPromptAt(t, source, shanghaiHour(t, 2026, 10, 19, 8), billing.Usage{}); math.Abs(got-1e-6) > 1e-12 {
		t.Errorf("删掉 06:00-10:00 之后该时段的 prompt 单价 = %v，期望 %v", got, 1e-6)
	}
	if got := evalPromptAt(t, source, shanghaiHour(t, 2026, 10, 19, 2), billing.Usage{}); math.Abs(got-3e-7) > 1e-12 {
		t.Errorf("留下的 01:00-04:00 的 prompt 单价 = %v，期望 %v", got, 3e-7)
	}
}

// TestPricePeakRemoveRejectsNonPeakSegment 钉住「remove 只删峰段、越界拒绝」。
func TestPricePeakRemoveRejectsNonPeakSegment(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)

	// 这份表的第一段是只有门槛的阶梯：它不是峰段。
	const withTier = `{
	  "tz": "Asia/Shanghai",
	  "base": { "prompt": 0.000001, "completion": 0.000005 },
	  "overrides": [ { "min_context_tokens": 200000, "prompt": 0.000003 } ]
	}`
	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", uniqueName("peak-tier"),
		"--models", uniqueName("peak-model"),
		"--schedule-file", writeScheduleFile(t, withTier),
	})
	idArg := strconv.FormatInt(id, 10)

	err := runCaptured(t, func() error {
		return runPricePeak([]string{"remove", idArg, "1", "--by", by})
	})
	if err == nil {
		t.Fatal("没有时段的阶梯不该被 price peak remove 删掉")
	}
	if !strings.Contains(err.Error(), "不是峰段") {
		t.Errorf("错误信息没点出「不是峰段」：%v", err)
	}
	if schedule := currentSchedule(t, db, id); len(schedule.Overrides) != 1 {
		t.Errorf("被拒之后还有 %d 段覆盖，期望 1 段", len(schedule.Overrides))
	}

	err = runCaptured(t, func() error {
		return runPricePeak([]string{"remove", idArg, "9", "--by", by})
	})
	if err == nil {
		t.Fatal("越界段号应当被拒")
	}
	if !strings.Contains(err.Error(), "超出范围") {
		t.Errorf("错误信息没点出越界：%v", err)
	}
}

// TestPricePeakCombinedOverrideSharesSegment 钉住「组合档是同一条覆盖、两个清单
// 段号相同，命中条件是峰段且门槛」。
func TestPricePeakCombinedOverrideSharesSegment(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)
	id := addPeakRule(t)
	idArg := strconv.FormatInt(id, 10)

	segment, _ := runPeakAdd(t, idArg,
		"--tz", "Asia/Shanghai", "--window", "01:00-04:00",
		"--above", "100000", "--in", "0.3", "--by", by)
	if segment != 1 {
		t.Fatalf("组合档的段号 = %d，期望 1", segment)
	}

	withFormat(t, cli.FormatJSON)
	peakOut, _ := captureStreams(t, func() {
		if err := runPricePeak([]string{"list", idArg}); err != nil {
			t.Fatalf("price peak list 失败: %v", err)
		}
	})
	var peakDoc struct {
		Peaks []struct {
			Segment int    `json:"segment"`
			Window  string `json:"window"`
			Above   *int64 `json:"above"`
		} `json:"peaks"`
	}
	if err := json.Unmarshal([]byte(peakOut), &peakDoc); err != nil {
		t.Fatalf("price peak list 的 JSON 解不开: %v\n%s", err, peakOut)
	}
	if len(peakDoc.Peaks) != 1 {
		t.Fatalf("peak list 里有 %d 段，期望 1 段：\n%s", len(peakDoc.Peaks), peakOut)
	}
	if peakDoc.Peaks[0].Segment != 1 || peakDoc.Peaks[0].Window != "01:00-04:00" {
		t.Errorf("peak list 那一段 = %+v，期望段号 1、时段 01:00-04:00", peakDoc.Peaks[0])
	}
	if peakDoc.Peaks[0].Above == nil || *peakDoc.Peaks[0].Above != 100000 {
		t.Errorf("peak list 里的门槛 = %v，期望 100000", showPtr(peakDoc.Peaks[0].Above))
	}

	tierOut, _ := captureStreams(t, func() {
		if err := runPriceTier([]string{"list", idArg}); err != nil {
			t.Fatalf("price tier list 失败: %v", err)
		}
	})
	var tierDoc struct {
		Tiers []struct {
			Segment int   `json:"segment"`
			Above   int64 `json:"above"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal([]byte(tierOut), &tierDoc); err != nil {
		t.Fatalf("price tier list 的 JSON 解不开: %v\n%s", err, tierOut)
	}
	if len(tierDoc.Tiers) != 1 {
		t.Fatalf("tier list 里有 %d 段，期望 1 段：\n%s", len(tierDoc.Tiers), tierOut)
	}
	if tierDoc.Tiers[0].Segment != 1 || tierDoc.Tiers[0].Above != 100000 {
		t.Errorf("tier list 那一段 = %+v，期望与 peak list 同一个段号 1、门槛 100000",
			tierDoc.Tiers[0])
	}

	// 两个维度是「且」：峰段里超门槛才命中，缺一头都回到底价。
	source := findRule(t, listRules(t, db), id).Expr
	peakTime := shanghaiHour(t, 2026, 10, 19, 2)
	offPeak := shanghaiHour(t, 2026, 10, 19, 12)
	over := billing.Usage{PromptTokens: 100000}
	under := billing.Usage{PromptTokens: 99999}
	for _, tc := range []struct {
		what  string
		at    time.Time
		usage billing.Usage
		want  float64
	}{
		{"峰段内且达到门槛", peakTime, over, 3e-7},
		{"峰段内但不到门槛", peakTime, under, 1e-6},
		{"非峰段但达到门槛", offPeak, over, 1e-6},
	} {
		if got := evalPromptAt(t, source, tc.at, tc.usage); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s 的 prompt 单价 = %v，期望 %v", tc.what, got, tc.want)
		}
	}
}

// TestPricePeakExcludeHolidaysBecomesWorkday 钉住 --exclude-holidays 落成 day_kind work。
func TestPricePeakExcludeHolidaysBecomesWorkday(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)
	id := addPeakRule(t)
	idArg := strconv.FormatInt(id, 10)

	runPeakAdd(t, idArg,
		"--tz", "Asia/Shanghai", "--window", "01:00-04:00",
		"--exclude-holidays", "--in", "0.3", "--by", by)

	schedule := currentSchedule(t, db, id)
	if len(schedule.Overrides) != 1 {
		t.Fatalf("覆盖有 %d 段，期望 1 段", len(schedule.Overrides))
	}
	if got := schedule.Overrides[0].DayKind; got != "work" {
		t.Errorf("day_kind = %q，期望 %q", got, "work")
	}
	if source := findRule(t, listRules(t, db), id).Expr; !strings.Contains(source, "isWorkday()") {
		t.Errorf("编译出来的表达式里没有 isWorkday()：%s", source)
	}
}

// TestPricePeakRejectsBadFlags 走的是不碰数据库的那几条路径。
//
// 参数校验必须在碰库之前：缺一张表时 `--tz` 写错会走成「要 --schedule-file」，
// 而那句话说错了地方。这里每条都断言退出码 2。
func TestPricePeakRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有子命令", nil, "price peak 需要子命令"},
		{"未知子命令", []string{"bogus"}, "未知的 price peak 子命令"},
		{"add 缺 --tz", []string{
			"add", "r1", "--window", "01:00-04:00", "--in", "0.3", "--by", "ops",
		}, "缺少 --tz"},
		{"add 缺 --window", []string{
			"add", "r1", "--tz", "Asia/Shanghai", "--in", "0.3", "--by", "ops",
		}, "缺少 --window"},
		{"add 缺单价旗标", []string{
			"add", "r1", "--tz", "Asia/Shanghai", "--window", "01:00-04:00", "--by", "ops",
		}, "缺少单价旗标"},
		{"add 缺 --by", []string{
			"add", "r1", "--tz", "Asia/Shanghai", "--window", "01:00-04:00", "--in", "0.3",
		}, "缺少 --by"},
		{"add 缺规则名", []string{
			"add", "--tz", "Asia/Shanghai", "--window", "01:00-04:00", "--in", "0.3", "--by", "ops",
		}, "缺少规则名或规则 ID"},
		{"add 的 --above 不是正数", []string{
			"add", "r1", "--tz", "Asia/Shanghai", "--window", "01:00-04:00",
			"--above", "0", "--in", "0.3", "--by", "ops",
		}, "--above 0 无效"},
		{"add 的星期写错", []string{
			"add", "r1", "--tz", "Asia/Shanghai", "--window", "01:00-04:00",
			"--weekdays", "mon-xyz", "--in", "0.3", "--by", "ops",
		}, "不是星期"},
		{"remove 缺段号", []string{"remove", "r1", "--by", "ops"}, "缺少段号"},
		{"remove 的段号是 0", []string{"remove", "r1", "0", "--by", "ops"}, "段号必须是正整数"},
		{"remove 的段号不是数字", []string{"remove", "r1", "x", "--by", "ops"}, "段号必须是正整数"},
		{"remove 缺 --by", []string{"remove", "r1", "1"}, "缺少 --by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runPricePeak(tc.args) })
			if err == nil {
				t.Fatal("这条参数应当被拒")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息里没有 %q：%v", tc.want, err)
			}
			if code := cli.ExitCode(err); code != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, cli.ExitUsage)
			}
		})
	}
}

// TestPricePeakShortAndLongHelp 钉住 -h 打短版、help 打长版且不连库。
func TestPricePeakShortAndLongHelp(t *testing.T) {
	var err error
	stdout, stderr := captureStreams(t, func() { err = runPricePeak([]string{"-h"}) })
	if err != nil {
		t.Fatalf("-h 报错了: %v", err)
	}
	if out := stdout + stderr; !strings.Contains(out, pricePeakShortUsage) {
		t.Errorf("-h 打的不是短版：stdout=%q stderr=%q", stdout, stderr)
	}

	stdout, stderr = captureStreams(t, func() { err = runPricePeak([]string{"help"}) })
	if err != nil {
		t.Fatalf("help 报错了: %v", err)
	}
	if !strings.Contains(stdout, pricePeakUsage) {
		t.Errorf("help 打的不是长版：stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestParsePeakWeekdays 钉住星期两种写法的展开口径。
func TestParsePeakWeekdays(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"空串表示每天", "", nil},
		{"逗号分隔", "mon,tue", []string{"mon", "tue"}},
		{"区间", "mon-fri", []string{"mon", "tue", "wed", "thu", "fri"}},
		{"区间与单项混写", "sat,mon-wed", []string{"mon", "tue", "wed", "sat"}},
		{"重复的缩写只留一份", "mon,mon,tue", []string{"mon", "tue"}},
		{"七个都等于每天", "sun-sat", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePeakWeekdays(tc.raw)
			if err != nil {
				t.Fatalf("解析 %q 失败: %v", tc.raw, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("解析 %q = %v，期望 %v", tc.raw, got, tc.want)
			}
		})
	}

	for _, raw := range []string{"mon-fri-sat", "fri-mon", ",mon", "mon,"} {
		if _, err := parsePeakWeekdays(raw); err == nil {
			t.Errorf("%q 应当被拒", raw)
		}
	}
}
