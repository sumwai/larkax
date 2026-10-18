package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/calendar"
	calendarstore "github.com/sumwai/larkax/internal/calendar/store"
)

// 导入成功时不会有任何错误输出，而「什么都没写」与「写成功了」在安静的终端里
// 长得一样（LESSONS.md 第 16 条）。所以输出里的每一个数都要被断言。
func TestPrintCalendarImportReportsEveryCount(t *testing.T) {
	out := captureStdout(t, func() {
		printCalendarImport(os.Stdout, calendarstore.Result{
			Region: "CN", Year: 2031, Status: calendar.YearSynced,
			DaysInYear: 365, DaysOutsideYear: 2, Replaced: 363,
		})
	})
	for _, want := range []string{"CN", "2031", "synced", "365", "2", "363"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里应当出现 %q，实际为 %q", want, out)
		}
	}
}

// 非 synced 的状态本来就不带数据行，「0 天」要有一句说明，
// 否则它看起来像数据被吞了。
func TestPrintCalendarImportExplainsZeroDays(t *testing.T) {
	out := captureStdout(t, func() {
		printCalendarImport(os.Stdout, calendarstore.Result{
			Region: "CN", Year: 2031, Status: calendar.YearPending,
		})
	})
	if !strings.Contains(out, "pending") {
		t.Errorf("输出里应当说明状态，实际为 %q", out)
	}
	if !strings.Contains(out, "不带数据行") {
		t.Errorf("0 天时要说明这是预期结果，实际为 %q", out)
	}
}

func TestRunCalendarRequiresSubcommand(t *testing.T) {
	if err := runCalendar(nil); err == nil {
		t.Error("没有子命令时应当报错")
	}
	if err := runCalendar([]string{"清理"}); err == nil {
		t.Error("未知子命令应当报错")
	}
}

func TestRunCalendarImportRequiresOneFile(t *testing.T) {
	if err := runCalendarImport(nil); err == nil {
		t.Error("没有文件参数时应当报错")
	}
	if err := runCalendarImport([]string{"a.json", "b.json"}); err == nil {
		t.Error("两个文件参数时应当报错")
	}
}

// --help 不是失败。flag 解析到 -h / --help 时会打印用法并返回 flag.ErrHelp，
// 而 main 把任何非 nil 的错误都当成失败（退出码 1）——照着 main.go 的用法
// 文本跑 `larkaxctl calendar import --help` 的人会以为命令失败了。
func TestRunCalendarImportHelpSucceeds(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		if err := runCalendarImport([]string{arg}); err != nil {
			t.Errorf("calendar import %s 应当成功返回，实际为 %v", arg, err)
		}
	}
}

func TestRunCalendarImportReportsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "没有这个文件.json")
	err := runCalendarImport([]string{path})
	if err == nil {
		t.Fatal("文件不存在时应当报错")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("报错里要点出是哪个文件：%v", err)
	}
}

// 文件本身有问题时，命令必须在**连数据库之前**就失败。
//
// 这个用例跑在数据库连接串已经配好的环境里（CI 就是那样），所以它同时
// 证明了失败发生在碰库之前：真去连的话，导入会往测试库里写进一整年的数据。
//
// 本包的用例一个都不 skip，所以它不属于「集成测试包」，不进
// .github/actions/check-integration-tests 的包清单——那里的判据是「测试里
// 有 t.Skip，且跳过的条件是数据库连接串」。
func TestRunCalendarImportRejectsBadFileBeforeConnecting(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"键名写错", `{"region":"CN","year":2031,"status":"pending","offDay":true}`, "offDay"},
		{"不是 JSON", `这不是 JSON`, "解析"},
		{"状态认不出", `{"region":"CN","year":2031,"status":"published"}`, "status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "calendar.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("写测试文件失败: %v", err)
			}
			err := runCalendarImport([]string{path})
			if err == nil {
				t.Fatal("坏文件应当被拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里应当提到 %q，实际为 %v", tc.want, err)
			}
		})
	}
}

// ── price calendar list（#694） ───────────────────────────────────────

// 空清单打一句明确的话，而不是什么都不输出：一个没导入过的库与「命令没跑起来」
// 在安静的终端里长得一样（LESSONS.md 第 16 条）。
func TestPrintCalendarListEmpty(t *testing.T) {
	var buf bytes.Buffer
	printCalendarList(&buf, nil, false)

	want := "暂无日历。可使用 larkaxctl price calendar import 添加。\n"
	if got := buf.String(); got != want {
		t.Errorf("空清单 = %q，期望 %q", got, want)
	}
}

// 三个状态栏都要有话说：nil 是「没得比」，不是「一致」也不是「不一致」。
func TestAgreeText(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		in   *bool
		want string
	}{
		{nil, "没得比"},
		{&yes, "一致"},
		{&no, "不一致"},
	}
	for _, tc := range cases {
		if got := agreeText(tc.in); got != tc.want {
			t.Errorf("agreeText(%v) = %q，期望 %q", showPtr(tc.in), got, tc.want)
		}
	}
}

// 表格那一档：六栏、状态原文、天数、同步失败原因单独一行、公告地址只在 -v 下打。
func TestPrintCalendarList(t *testing.T) {
	at := time.Date(2026, 10, 18, 3, 4, 5, 0, time.UTC)
	yes, no := true, false

	years := []calendarstore.Year{
		{
			Region: "CN", Year: 2031, Status: "synced", DayCount: 365,
			SourcesAgree: &yes, SyncedAt: at,
			SourcePaperURLs: []string{"https://example.com/a", "https://example.com/b"},
		},
		{
			Region: "CN", Year: 2030, Status: "conflict", DayCount: 0,
			SourcesAgree: &no, SyncedAt: at,
		},
		{
			Region: "CN", Year: 2029, Status: "degraded", DayCount: 0,
			LastError: "公告页解析失败", SyncedAt: at,
		},
		{
			Region: "CN", Year: 2028, Status: "pending", DayCount: 0,
			SyncedAt: at,
		},
	}

	var buf bytes.Buffer
	printCalendarList(&buf, years, false)
	out := buf.String()

	for _, want := range []string{
		"地区", "年份", "状态", "天数", "双源", "同步于",
		"2031", "synced", "365", "一致",
		"2030", "conflict", "不一致",
		"2029", "degraded",
		"2028", "pending",
		"没得比",
		// 同步失败的原因单独排在表后，一行一条。
		"CN 2029 年的同步失败原因：公告页解析失败",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("清单里没有 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "https://example.com/a") {
		t.Errorf("默认档不该打公告地址：\n%s", out)
	}

	// 2031 在 2030 之前：同地区内从新到旧。
	if strings.Index(out, "2031") > strings.Index(out, "2030") {
		t.Errorf("年份顺序不对，2031 应当在 2030 之前：\n%s", out)
	}

	// -v 那一档补上公告地址。
	var verbose bytes.Buffer
	printCalendarList(&verbose, years, true)
	if !strings.Contains(verbose.String(), "CN 2031 年的公告：https://example.com/a https://example.com/b") {
		t.Errorf("-v 档没有公告地址：\n%s", verbose.String())
	}
}

// JSON 是另一条渲染路径：键名照库里的列名，空数组打 []，没得比打 null。
func TestCalendarListDocIsMachineReadable(t *testing.T) {
	at := time.Date(2026, 10, 18, 3, 4, 5, 0, time.UTC)
	yes := true

	var got struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Summary  struct {
			Total int `json:"total"`
		} `json:"summary"`
		Calendars []struct {
			Region          string   `json:"region"`
			Year            int      `json:"year"`
			Status          string   `json:"status"`
			DayCount        int      `json:"day_count"`
			SourcePaperURLs []string `json:"source_paper_urls"`
			SourcesAgree    *bool    `json:"sources_agree"`
			LastError       *string  `json:"last_error"`
			SyncedAt        string   `json:"synced_at"`
		} `json:"calendars"`
	}
	decodeDoc(t, calendarListDoc([]calendarstore.Year{
		{
			Region: "CN", Year: 2031, Status: "synced", DayCount: 365,
			SourcesAgree: &yes, SyncedAt: at,
			SourcePaperURLs: []string{"https://example.com/a"},
		},
		{
			Region: "CN", Year: 2030, Status: "degraded",
			SyncedAt: at, LastError: "解析失败",
		},
	}), &got)

	if got.Status != jsonOK || got.ExitCode != exitOK {
		t.Errorf("status=%q exit_code=%d，想要 %q / %d", got.Status, got.ExitCode, jsonOK, exitOK)
	}
	if got.Summary.Total != 2 || len(got.Calendars) != 2 {
		t.Fatalf("summary.total=%d、清单 %d 条，想要 2 / 2", got.Summary.Total, len(got.Calendars))
	}

	first := got.Calendars[0]
	if first.Region != "CN" || first.Year != 2031 || first.Status != "synced" || first.DayCount != 365 {
		t.Errorf("第一行对不上：%+v", first)
	}
	if len(first.SourcePaperURLs) != 1 || first.SourcePaperURLs[0] != "https://example.com/a" {
		t.Errorf("source_paper_urls = %v", first.SourcePaperURLs)
	}
	if first.SourcesAgree == nil || !*first.SourcesAgree {
		t.Errorf("sources_agree = %q，想要 true", showPtr(first.SourcesAgree))
	}
	// 时刻恒 UTC、完整精度（jsonTime）。
	if first.SyncedAt != "2026-10-18T03:04:05Z" {
		t.Errorf("synced_at = %q，想要 UTC 的 RFC3339", first.SyncedAt)
	}

	second := got.Calendars[1]
	// 空数组是 []，不是 null：source_paper_urls 那一列 NOT NULL DEFAULT '{}'。
	if second.SourcePaperURLs == nil || len(second.SourcePaperURLs) != 0 {
		t.Errorf("source_paper_urls = %v，想要 []", second.SourcePaperURLs)
	}
	// 没得比是 null，不是 false——false 是「比过、结果不一致」。
	if second.SourcesAgree != nil {
		t.Errorf("sources_agree = %q，想要 null", showPtr(second.SourcesAgree))
	}
	if second.LastError == nil || *second.LastError != "解析失败" {
		t.Errorf("last_error = %q，想要「解析失败」", showPtr(second.LastError))
	}
}

// 一年都没有时打 []，不是 null。
func TestCalendarListDocEmptyIsEmptyArray(t *testing.T) {
	var got map[string]any
	decodeDoc(t, calendarListDoc(nil), &got)

	calendars, ok := got["calendars"].([]any)
	if !ok {
		t.Fatalf("calendars 不是数组：%v", got["calendars"])
	}
	if len(calendars) != 0 {
		t.Errorf("calendars 有 %d 条，想要空数组", len(calendars))
	}
}

// 位置参数在连库之前拦下。
func TestCalendarListRejectsPositional(t *testing.T) {
	err := runCaptured(t, func() error { return runCalendarList([]string{"extra"}) })
	if err == nil {
		t.Fatal("期望报错，却通过了")
	}
	if !strings.Contains(err.Error(), "不接受位置参数") {
		t.Errorf("错误信息 %q 里没有「不接受位置参数」", err.Error())
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d", got, exitUsage)
	}
}

// seedCalendarYears 往 calendar_years 里写几行，用完删掉。
func seedCalendarYears(t *testing.T, region string, rows [][]any) {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	for _, r := range rows {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO calendar_years
				(region, year, status, day_count, source_paper_urls, sources_agree, last_error, synced_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, r...); err != nil {
			t.Fatalf("种日历行失败: %v", err)
		}
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx,
			`DELETE FROM calendar_years WHERE region = $1`, region); err != nil {
			t.Errorf("清理日历行失败: %v", err)
		}
	})
}

// 从库里列：状态原文、双源三档、同步失败原因。
func TestCalendarListShowsImportedYears(t *testing.T) {
	testDB(t)
	setupEnv(t)

	region := uniqueName("ZZ")
	at := time.Date(2026, 10, 18, 3, 4, 5, 0, time.UTC)
	seedCalendarYears(t, region, [][]any{
		{region, 2031, "synced", 365, []string{"https://example.com/a"}, true, "", at},
		{region, 2030, "conflict", 0, []string{}, false, "", at},
		{region, 2029, "degraded", 0, []string{}, nil, "公告页解析失败", at},
		{region, 2028, "pending", 0, []string{}, nil, "", at},
	})

	out, _ := captureStreams(t, func() {
		if err := runCalendarList(nil); err != nil {
			t.Errorf("price calendar list 失败: %v", err)
		}
	})

	for _, want := range []string{
		region, "2031", "synced", "365", "一致",
		"conflict", "不一致",
		"degraded", "没得比",
		"pending",
		region + " 2029 年的同步失败原因：公告页解析失败",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("清单里没有 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "https://example.com/a") {
		t.Errorf("默认档不该打公告地址：\n%s", out)
	}
}

// json 档：stdout 上只有一份 JSON。
func TestCalendarListJSONStdoutIsMachineReadable(t *testing.T) {
	testDB(t)
	setupEnv(t)

	region := uniqueName("ZZ")
	seedCalendarYears(t, region, [][]any{
		{region, 2031, "synced", 365, []string{}, true, "", time.Now().UTC()},
	})

	withFormat(t, formatJSON)
	var err error
	stdout, stderr := captureStreams(t, func() { err = runCalendarList(nil) })
	if err != nil {
		t.Fatalf("price calendar list 失败: %v（stderr: %s）", err, stderr)
	}
	assertOnlyJSON(t, stdout)

	var got struct {
		Status    string `json:"status"`
		Calendars []struct {
			Region string `json:"region"`
			Year   int    `json:"year"`
			Status string `json:"status"`
		} `json:"calendars"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, stdout)
	}
	found := false
	for _, c := range got.Calendars {
		if c.Region == region && c.Year == 2031 && c.Status == "synced" {
			found = true
		}
	}
	if !found {
		t.Errorf("清单里没有刚种下的那一行：%+v", got.Calendars)
	}
}

// 读日历不要主密钥。
func TestCalendarListDoesNotRequireMasterKey(t *testing.T) {
	testDB(t)
	setupEnv(t)
	t.Setenv("LARKAX_MASTER_KEYS", "")

	if err := runCaptured(t, func() error { return runCalendarList(nil) }); err != nil {
		t.Fatalf("price calendar list 不该要主密钥: %v", err)
	}
}
