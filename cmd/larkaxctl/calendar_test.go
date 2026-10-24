package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// ── price calendar sync ──────────────────────────────────────────────
//
// 这一节分两层：纯函数与参数校验不碰库（没有 LARKAX_DB_DSN 也照常跑），
// 只有「写进库之后长什么样」那几格要真库。

// holidayCNTestBody 造一份 holiday-cn 形状的数据文件。days 每一项是一段
// JSON 对象文本，用 holidayCNDayJSON 拼。
func holidayCNTestBody(t *testing.T, year int, papers []string, days ...string) string {
	t.Helper()
	papersJSON, err := json.Marshal(papers)
	if err != nil {
		t.Fatalf("序列化公告地址失败: %v", err)
	}
	return fmt.Sprintf(`{"year":%d,"papers":%s,"days":[%s]}`,
		year, papersJSON, strings.Join(days, ","))
}

func holidayCNDayJSON(name, date string, offDay bool) string {
	return fmt.Sprintf(`{"name":%q,"date":%q,"isOffDay":%t}`, name, date, offDay)
}

// withHolidayCNServer 起一个假的数据源，把包级基地址指过去。用例一律不打真网。
func withHolidayCNServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	saved := holidayCNBaseURL
	holidayCNBaseURL = srv.URL
	t.Cleanup(func() { holidayCNBaseURL = saved })
	return srv
}

// withDeadHolidayCNServer 把基地址指向一个已经关掉的地址：连接会被拒绝。
func withDeadHolidayCNServer(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	saved := holidayCNBaseURL
	holidayCNBaseURL = deadURL
	t.Cleanup(func() { holidayCNBaseURL = saved })
}

// findCalendarDay 在转换结果里找一天，找不到就让用例失败。
func findCalendarDay(t *testing.T, doc calendarstore.Document, date string) calendarstore.Day {
	t.Helper()
	for _, day := range doc.Days {
		if day.Date == date {
			return day
		}
	}
	t.Fatalf("文档里没有 %s", date)
	return calendarstore.Day{}
}

// assertCalendarDay 断言一天的三项判定。
func assertCalendarDay(t *testing.T, doc calendarstore.Document, date, name string, offDay, makeup bool) {
	t.Helper()
	day := findCalendarDay(t, doc, date)
	if day.Name != name {
		t.Errorf("%s 的 name = %q，期望 %q", date, day.Name, name)
	}
	if day.OffDay == nil {
		t.Errorf("%s 没有 off_day", date)
	} else if *day.OffDay != offDay {
		t.Errorf("%s 的 off_day = %t，期望 %t", date, *day.OffDay, offDay)
	}
	if day.MakeupWorkday != makeup {
		t.Errorf("%s 的 makeup_workday = %t，期望 %t", date, day.MakeupWorkday, makeup)
	}
}

// 五条转换规则各一格。两格必须分开：数据源里没有的普通周末 name 为空，
// 而落在周末的法定节日要保留 name——「周末一律写空」的实现只会在后者上红。
func TestHolidayCNDocumentConversion(t *testing.T) {
	file := holidayCNFile{
		Year:   2026,
		Papers: []string{"https://example.com/notice"},
		Days: []holidayCNDaySource{
			{Name: "元旦", Date: "2026-01-01", IsOffDay: true},    // 周四
			{Name: "元旦", Date: "2026-01-03", IsOffDay: true},    // 周六
			{Date: "2026-01-04", IsOffDay: false},               // 周日，被挪来上班
			{Name: "调休上班", Date: "2026-01-05", IsOffDay: false}, // 周一
			{Name: "元旦", Date: "2025-12-31", IsOffDay: true},    // 年外
		},
	}
	doc, err := holidayCNDocument(2026, file)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}

	if doc.Region != "CN" || doc.Year != 2026 || doc.Status != "synced" || doc.Source != "holiday-cn" {
		t.Errorf("文档头部 = %+v", doc)
	}
	if len(doc.SourcePaperURLs) != 1 || doc.SourcePaperURLs[0] != "https://example.com/notice" {
		t.Errorf("source_paper_urls = %v", doc.SourcePaperURLs)
	}
	// 只有一个源，双源校验没有意义：写了等于写一个恒为 true 的字段。
	if doc.SourcesAgree != nil {
		t.Errorf("sources_agree = %v，期望不写", showPtr(doc.SourcesAgree))
	}
	// 2026 是平年：365 天，外加一条年外日期。
	if len(doc.Days) != 366 {
		t.Errorf("天数 = %d，期望 366（365 天 + 1 天年外）", len(doc.Days))
	}

	assertCalendarDay(t, doc, "2026-01-01", "元旦", true, false)    // 平日节日
	assertCalendarDay(t, doc, "2026-01-03", "元旦", true, false)    // 落在周末的节日，名称保留
	assertCalendarDay(t, doc, "2026-01-04", "", false, true)      // 周末补班
	assertCalendarDay(t, doc, "2026-01-05", "调休上班", false, false) // 非周末的 isOffDay=false
	assertCalendarDay(t, doc, "2026-01-10", "", true, false)      // 数据源没有的周六
	assertCalendarDay(t, doc, "2026-01-06", "", false, false)     // 数据源没有的周二
	assertCalendarDay(t, doc, "2025-12-31", "元旦", true, false)    // 年外原样保留

	seen := make(map[string]bool, len(doc.Days))
	for _, day := range doc.Days {
		if seen[day.Date] {
			t.Errorf("%s 在文档里出现了两次", day.Date)
		}
		seen[day.Date] = true
	}
}

// 数据源回错了文件（year 字段对不上）要报错，不能拿它当这一年的判定。
func TestHolidayCNDocumentRejectsMismatchedYear(t *testing.T) {
	_, err := holidayCNDocument(2026, holidayCNFile{
		Year: 2025,
		Days: []holidayCNDaySource{{Name: "元旦", Date: "2025-01-01", IsOffDay: true}},
	})
	if err == nil {
		t.Fatal("year 对不上时应当报错")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("报错里应当说清年份不一致，实际为 %v", err)
	}
}

// 空 days 是「还没发布」，不是错误；写库的文档只记年份状态。
func TestHolidayCNDocumentEmptyDaysIsPending(t *testing.T) {
	doc, err := holidayCNDocument(2026, holidayCNFile{Year: 2026})
	if err != nil {
		t.Fatalf("空 days 不该报错: %v", err)
	}
	if doc.Status != "pending" {
		t.Errorf("status = %q，期望 pending", doc.Status)
	}
	if len(doc.Days) != 0 || doc.Source != "" {
		t.Errorf("pending 文档不该带 days / source：days=%d source=%q", len(doc.Days), doc.Source)
	}
}

// 数据源里的日期格式不对或不成立时整份拒绝：规范化成另一天会让判定落到错的日子。
func TestHolidayCNDocumentRejectsBadDates(t *testing.T) {
	cases := []struct {
		name string
		day  holidayCNDaySource
		want string
	}{
		{"格式不对", holidayCNDaySource{Date: "2026-1-5"}, "YYYY-MM-DD"},
		// 0000 年能过 time.Parse，但不是一个成立的公历日期；带它走 Date.Valid
		// 那一支（2 月 30 日这类在 Parse 那一步就被拒了）。
		{"日期不成立", holidayCNDaySource{Date: "0000-01-01"}, "不成立"},
		{"重复出现", holidayCNDaySource{Name: "第二次", Date: "2026-01-01"}, "两次"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := holidayCNFile{
				Year: 2026,
				Days: []holidayCNDaySource{
					{Name: "元旦", Date: "2026-01-01", IsOffDay: true},
					tc.day,
				},
			}
			if _, err := holidayCNDocument(2026, file); err == nil {
				t.Fatal("坏日期应当被拒绝")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里应当提到 %q，实际为 %v", tc.want, err)
			}
		})
	}
}

// --year 缺省取的是**地区时区**的年份，不是机器本地时区。
//
// 用例喂一个 UTC 时刻进来（本地 12-31 16:00 = 次年 1 月 1 日 00:00 CST），
// 于是「用 now.Year()」的实现会给出 2025，按地区换算的实现给出 2026。
func TestSyncYearDefaultsToRegionTime(t *testing.T) {
	now := time.Date(2025, 12, 31, 16, 0, 0, 0, time.UTC)
	year, err := syncYear("", now)
	if err != nil {
		t.Fatalf("缺省年份取值失败: %v", err)
	}
	if year != 2026 {
		t.Errorf("缺省年份 = %d，期望 2026（按 CN 时区而不是机器时区）", year)
	}
}

// 命令行实际走的是 syncTargetYear：它必须把注入的「现在」接进去。
func TestSyncTargetYearUsesInjectedClock(t *testing.T) {
	saved := calendarSyncNow
	calendarSyncNow = func() time.Time { return time.Date(2025, 12, 31, 16, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { calendarSyncNow = saved })

	year, err := syncTargetYear("")
	if err != nil {
		t.Fatalf("缺省年份取值失败: %v", err)
	}
	if year != 2026 {
		t.Errorf("缺省年份 = %d，期望 2026", year)
	}
}

// 只收单年：`--year 2025,2026` 要报参数错，不能悄悄只取第一个。
func TestSyncYearRejectsBadValues(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"2025,2026", "只接受一个年份"},
		{"", ""}, // 空串是缺省，走地区时区，由另一条用例管
		{"0", "[1, 9999]"},
		{"10000", "[1, 9999]"},
	}
	for _, tc := range cases {
		if tc.raw == "" {
			continue
		}
		t.Run(tc.raw, func(t *testing.T) {
			now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
			_, err := syncYear(tc.raw, now)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里应当提到 %q，实际为 %v", tc.want, err)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", got, exitUsage)
			}
		})
	}
}

// sync 的分派与参数校验都在碰库之前，所以这一组在无库环境也跑。
func TestPriceCalendarSyncRejectsMultipleYears(t *testing.T) {
	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("参数错时不该拉数据源，却请求了 %s", r.URL.Path)
	})

	err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", "2025,2026"})
	})
	if err == nil {
		t.Fatal("多写一个年份时应当报参数错")
	}
	if !strings.Contains(err.Error(), "--year") {
		t.Errorf("报错应当说的是 --year，实际为 %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d", got, exitUsage)
	}
}

func TestPriceCalendarSyncRejectsPositionalArguments(t *testing.T) {
	err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "extra"})
	})
	if err == nil {
		t.Fatal("位置参数应当被拒绝")
	}
	if !strings.Contains(err.Error(), "不接受位置参数") {
		t.Errorf("报错应该点出位置参数，实际为 %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d", got, exitUsage)
	}
}

func TestPriceCalendarSyncHelpSucceeds(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		if err := runPriceCalendarSync([]string{arg}); err != nil {
			t.Errorf("price calendar sync %s 应当成功返回，实际为 %v", arg, err)
		}
	}
}

// 拉取与状态映射这一层不碰库：五种数据源回应各走哪条路，全在这里钉住。
func TestFetchCalendarDocumentStatusMapping(t *testing.T) {
	const year = 2409
	published := holidayCNTestBody(t, year, []string{"https://example.com/notice"},
		holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-01", year), true))

	cases := []struct {
		name     string
		handler  http.HandlerFunc
		wantStat string
		wantErr  string
	}{
		{
			name: "已发布",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(published))
			},
			wantStat: "synced",
		},
		{
			name: "days 为空",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(holidayCNTestBody(t, year, nil)))
			},
			wantStat: "pending",
		},
		{
			name: "404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantStat: "pending",
		},
		{
			name: "HTTP 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "HTTP 500",
		},
		{
			name: "不是 JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("这不是 JSON"))
			},
			wantErr: "解析",
		},
		{
			name: "year 对不上",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(holidayCNTestBody(t, year-1, nil,
					holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-01", year-1), true))))
			},
			wantErr: "不一致",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withHolidayCNServer(t, tc.handler)
			doc, err := fetchCalendarDocument(context.Background(), year)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望失败，却拿到了 %+v", doc)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("报错里应当提到 %q，实际为 %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该失败: %v", err)
			}
			if doc.Status != tc.wantStat {
				t.Errorf("status = %q，期望 %q", doc.Status, tc.wantStat)
			}
		})
	}
}

// 连接被拒绝也是「真正的失败」，且报错里能看出是拉取这一步。
func TestFetchCalendarDocumentReportsUnreachableSource(t *testing.T) {
	withDeadHolidayCNServer(t)
	if _, err := fetchCalendarDocument(context.Background(), 2409); err == nil {
		t.Fatal("连不上数据源时应当报错")
	} else if !strings.Contains(err.Error(), "拉取") {
		t.Errorf("报错里应当点出拉取失败，实际为 %v", err)
	}
}

// ── 要真库的那几格：写进去之后长什么样 ────────────────────────────
//
// 用例用固定的、1 月 1 日是周四的平年：这样「1 月 3 日是周六」这类期望值
// 才写得成字面量。每格各用一个年份，开跑前先清一遍——上一次运行中途挂掉
// 留下的行会让「这一年什么都没有」这类断言读到别人的数据。

// resetCalendarYear 清掉某一年在库里的痕迹，结束时再清一次。
func resetCalendarYear(t *testing.T, year int) {
	t.Helper()
	deleteCalendarYear(t, year)
	t.Cleanup(func() { deleteCalendarYear(t, year) })
}

func deleteCalendarYear(t *testing.T, year int) {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	// 按 source_year 删数据行：那一年写下的每一行都带着这个来源年份。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM calendar_days WHERE region = 'CN' AND source_year = $1`, year); err != nil {
		t.Errorf("清理 %d 年的日历行失败: %v", year, err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM calendar_years WHERE region = 'CN' AND year = $1`, year); err != nil {
		t.Errorf("清理 %d 年的年份状态失败: %v", year, err)
	}
}

type calendarYearRow struct {
	status    string
	dayCount  int
	lastError string
	papers    []string
	syncedAt  time.Time
}

func readCalendarYear(t *testing.T, year int) calendarYearRow {
	t.Helper()
	db := testDB(t)
	var (
		row      calendarYearRow
		lastErr  sql.NullString
		papersJS string
	)
	err := db.QueryRowContext(context.Background(), `
		SELECT status, day_count, last_error, array_to_json(source_paper_urls)::text, synced_at
		  FROM calendar_years
		 WHERE region = 'CN' AND year = $1`, year).
		Scan(&row.status, &row.dayCount, &lastErr, &papersJS, &row.syncedAt)
	if err != nil {
		t.Fatalf("读 %d 年的日历状态失败: %v", year, err)
	}
	row.lastError = lastErr.String
	if err := json.Unmarshal([]byte(papersJS), &row.papers); err != nil {
		t.Fatalf("解析 %d 年的公告地址失败: %v", year, err)
	}
	return row
}

func countCalendarYearDays(t *testing.T, year int) int {
	t.Helper()
	db := testDB(t)
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM calendar_days WHERE region = 'CN' AND source_year = $1`,
		year).Scan(&n); err != nil {
		t.Fatalf("数 %d 年的日历行失败: %v", year, err)
	}
	return n
}

func readCalendarDay(t *testing.T, date string) (name string, offDay, makeup bool, source string) {
	t.Helper()
	db := testDB(t)
	var nameCol sql.NullString
	err := db.QueryRowContext(context.Background(), `
		SELECT holiday_name, is_off_day, is_makeup_workday, source
		  FROM calendar_days
		 WHERE region = 'CN' AND day = $1::date`, date).
		Scan(&nameCol, &offDay, &makeup, &source)
	if err != nil {
		t.Fatalf("读 %s 的日历行失败: %v", date, err)
	}
	return nameCol.String, offDay, makeup, source
}

// 200 且 days 非空：库里是整年 365 行，状态 synced，来源与公告地址都落库。
func TestPriceCalendarSyncWritesSyncedYear(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2409 // 平年，1 月 1 日周四
	resetCalendarYear(t, year)

	body := holidayCNTestBody(t, year, []string{"https://example.com/notice"},
		holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-01", year), true), // 周四
		holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-03", year), true), // 周六
		holidayCNDayJSON("", fmt.Sprintf("%d-01-04", year), false),  // 周日补班
		holidayCNDayJSON("调休上班", fmt.Sprintf("%d-01-05", year), false),
	)
	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		if want := fmt.Sprintf("/%d.json", year); r.URL.Path != want {
			t.Errorf("请求的路径 = %q，期望 %q", r.URL.Path, want)
		}
		_, _ = w.Write([]byte(body))
	})

	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	}); err != nil {
		t.Fatalf("price calendar sync 失败: %v", err)
	}

	row := readCalendarYear(t, year)
	if row.status != "synced" || row.dayCount != 365 || row.lastError != "" {
		t.Errorf("年份状态 = %+v，期望 synced / 365 天 / 无失败原因", row)
	}
	if len(row.papers) != 1 || row.papers[0] != "https://example.com/notice" {
		t.Errorf("source_paper_urls = %v", row.papers)
	}
	if n := countCalendarYearDays(t, year); n != 365 {
		t.Errorf("数据行数 = %d，期望 365", n)
	}

	// 落在周末的节日要保留名称：它是账单解释「为什么这天算高峰」的依据。
	name, off, makeup, source := readCalendarDay(t, fmt.Sprintf("%d-01-03", year))
	if name != "元旦" || !off || makeup || source != "holiday-cn" {
		t.Errorf("周六的元旦 = name %q off %t makeup %t source %q", name, off, makeup, source)
	}
	// 数据源里没有的普通周末：休息、没有名称。
	name, off, makeup, _ = readCalendarDay(t, fmt.Sprintf("%d-01-10", year))
	if name != "" || !off || makeup {
		t.Errorf("普通周六 = name %q off %t makeup %t", name, off, makeup)
	}
	// 补班日。
	name, off, makeup, _ = readCalendarDay(t, fmt.Sprintf("%d-01-04", year))
	if name != "" || off || !makeup {
		t.Errorf("补班的周日 = name %q off %t makeup %t", name, off, makeup)
	}
	// 非周末的 isOffDay=false：不补班，名称照样保留。
	name, off, makeup, _ = readCalendarDay(t, fmt.Sprintf("%d-01-05", year))
	if name != "调休上班" || off || makeup {
		t.Errorf("非周末的 isOffDay=false = name %q off %t makeup %t", name, off, makeup)
	}
}

// --year 缺省走 calendarSyncNow：命令层也要钉住这条接线，而不只是测 syncYear
// 那个纯函数——把 run 里的 syncTargetYear 换回 time.Now() 时，纯函数用例照绿。
//
// 时刻取 UTC 的 12-31 16:00：按机器本地时区是 2030 年，按 CN 时区已经是次年
// 1 月 1 日 00:00。所以请求的路径是 /2031.json，不是 /2030.json。
func TestPriceCalendarSyncDefaultsToRegionCurrentYear(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2031
	resetCalendarYear(t, year)

	saved := calendarSyncNow
	calendarSyncNow = func() time.Time { return time.Date(2030, 12, 31, 16, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { calendarSyncNow = saved })

	var requested string
	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		_, _ = w.Write([]byte(holidayCNTestBody(t, year, nil)))
	})

	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync"})
	}); err != nil {
		t.Fatalf("缺省年份同步失败: %v", err)
	}
	if requested != "/2031.json" {
		t.Errorf("请求的路径 = %q，期望 /2031.json（按 CN 时区跨年）", requested)
	}
	if row := readCalendarYear(t, year); row.status != "pending" {
		t.Errorf("status = %q，期望 pending", row.status)
	}
}

// 闰年也要补齐 366 天。2400 是整百年，但能被 400 整除，仍是闰年——「能被 100
// 整除就算平年」这种朴素判据会在这条上红。
func TestPriceCalendarSyncWritesLeapYear(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2400
	resetCalendarYear(t, year)

	body := holidayCNTestBody(t, year, nil,
		holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-01", year), true)) // 周六
	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})

	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	}); err != nil {
		t.Fatalf("闰年同步失败: %v", err)
	}

	row := readCalendarYear(t, year)
	if row.status != "synced" || row.dayCount != 366 {
		t.Errorf("年份状态 = %+v，期望 synced / 366 天", row)
	}
	if n := countCalendarYearDays(t, year); n != 366 {
		t.Errorf("数据行数 = %d，期望 366", n)
	}
	// 2 月 29 日是周二：数据源没有这一天，按工作日补齐。
	name, off, makeup, _ := readCalendarDay(t, fmt.Sprintf("%d-02-29", year))
	if name != "" || off || makeup {
		t.Errorf("2 月 29 日 = name %q off %t makeup %t，期望普通工作日", name, off, makeup)
	}
}

// 200 但 days 为空：写 pending，退出码 0，且那一年没有数据行。
func TestPriceCalendarSyncWritesPendingWhenDaysEmpty(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2415
	resetCalendarYear(t, year)

	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(holidayCNTestBody(t, year, nil)))
	})

	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	}); err != nil {
		t.Fatalf("未发布的年份不该让命令失败: %v", err)
	}

	row := readCalendarYear(t, year)
	if row.status != "pending" || row.dayCount != 0 || row.lastError != "" {
		t.Errorf("年份状态 = %+v，期望 pending / 0 天 / 无失败原因", row)
	}
	if n := countCalendarYearDays(t, year); n != 0 {
		t.Errorf("pending 的年份不该有数据行，实际 %d 行", n)
	}
}

// 404 与「days 为空」在 HTTP 层完全不同，业务上是同一件事，各测一格。
func TestPriceCalendarSyncWritesPendingOn404(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2426
	resetCalendarYear(t, year)

	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	}); err != nil {
		t.Fatalf("404 不该让命令失败: %v", err)
	}

	row := readCalendarYear(t, year)
	if row.status != "pending" || row.dayCount != 0 {
		t.Errorf("年份状态 = %+v，期望 pending / 0 天", row)
	}
	if n := countCalendarYearDays(t, year); n != 0 {
		t.Errorf("pending 的年份不该有数据行，实际 %d 行", n)
	}
}

// 网络失败且那一年还没有记录：记下 degraded 与原因，退出码非 0。
func TestPriceCalendarSyncRecordsDegradedOnNetworkFailure(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2437
	resetCalendarYear(t, year)

	withDeadHolidayCNServer(t)

	err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	})
	if err == nil {
		t.Fatal("连不上数据源时应当失败")
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", got, exitFailure)
	}

	row := readCalendarYear(t, year)
	if row.status != "degraded" {
		t.Errorf("status = %q，期望 degraded", row.status)
	}
	if row.lastError == "" {
		t.Error("degraded 的年份要记下失败原因")
	}
	if n := countCalendarYearDays(t, year); n != 0 {
		t.Errorf("degraded 的年份不该有数据行，实际 %d 行", n)
	}
}

// 已经 synced 的年份 + 网络失败：库里一个字节都不许变。
//
// 这条最容易漏：Import 会先删掉这一年的原有行，写一份不带 days 的 degraded
// 文档进去等于把整年好数据清空、再标成失败。
func TestPriceCalendarSyncLeavesSyncedYearUntouched(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2443
	resetCalendarYear(t, year)

	// 先同步成功，造出一份好数据。
	body := holidayCNTestBody(t, year, []string{"https://example.com/notice"},
		holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-01", year), true))
	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	}); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}
	before := readCalendarYear(t, year)
	beforeDays := countCalendarYearDays(t, year)
	if before.status != "synced" || beforeDays != 365 {
		t.Fatalf("前置条件不成立：status=%q 天数=%d", before.status, beforeDays)
	}

	// 第二次：数据源连不上。
	withDeadHolidayCNServer(t)
	err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	})
	if err == nil {
		t.Fatal("连不上数据源时应当失败")
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", got, exitFailure)
	}

	after := readCalendarYear(t, year)
	if after.status != before.status || after.dayCount != before.dayCount ||
		after.lastError != before.lastError || len(after.papers) != len(before.papers) {
		t.Errorf("已 synced 的年份被改动了：before=%+v after=%+v", before, after)
	}
	if !after.syncedAt.Equal(before.syncedAt) {
		t.Errorf("已 synced 的年份被重写了一次：synced_at %v → %v", before.syncedAt, after.syncedAt)
	}
	if n := countCalendarYearDays(t, year); n != beforeDays {
		t.Errorf("数据行数从 %d 变成了 %d", beforeDays, n)
	}
}

// 数据源说「这一年还没发布」时，已经有定论的年份必须原样留着。
//
// 404 与 200 空 days 走到的是同一条路：fetchCalendarDocument 都返回一份 pending
// 文档，而 Import 会先删掉这一年原有的行。漏判这两格，整年的判定会被清空，状态
// 却变成看上去完全正常的 pending——list 看不出异常，计价规则从那一天起整年失败。
func TestPriceCalendarSyncKeepsSyncedYearWhenSourceSaysUnpublished(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	cases := []struct {
		name   string
		year   int
		second http.HandlerFunc
	}{
		{
			name: "404",
			year: 2451,
			second: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
		},
		{
			name: "空 days",
			year: 2453,
			second: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(holidayCNTestBody(t, 2453, nil)))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCalendarYear(t, tc.year)

			// 先同步成功，造出一份有定论的数据。
			body := holidayCNTestBody(t, tc.year, []string{"https://example.com/notice"},
				holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-01", tc.year), true))
			withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			if err := runCaptured(t, func() error {
				return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(tc.year)})
			}); err != nil {
				t.Fatalf("造前置数据失败: %v", err)
			}
			firstOfYear := fmt.Sprintf("%d-01-01", tc.year)
			before := readCalendarYear(t, tc.year)
			beforeDays := countCalendarYearDays(t, tc.year)
			beforeName, beforeOff, beforeMakeup, beforeSource := readCalendarDay(t, firstOfYear)
			if before.status != "synced" || beforeDays != 365 {
				t.Fatalf("前置条件不成立：status=%q 天数=%d", before.status, beforeDays)
			}

			// 第二次：数据源表示这一年还没发布。
			withHolidayCNServer(t, tc.second)
			var syncErr error
			stdout, _ := captureStreams(t, func() {
				syncErr = runPriceCalendar([]string{"sync", "--year", strconv.Itoa(tc.year)})
			})
			if syncErr != nil {
				t.Fatalf("数据源说未发布不该让命令失败: %v", syncErr)
			}
			if !strings.Contains(stdout, "未发布") || !strings.Contains(stdout, strconv.Itoa(tc.year)) {
				t.Errorf("stdout 里没有说明这一格: %q", stdout)
			}

			after := readCalendarYear(t, tc.year)
			if after.status != before.status || after.dayCount != before.dayCount ||
				after.lastError != before.lastError || len(after.papers) != len(before.papers) {
				t.Errorf("已有定论的年份被改动了：before=%+v after=%+v", before, after)
			}
			if !after.syncedAt.Equal(before.syncedAt) {
				t.Errorf("已有定论的年份被重写了一次：synced_at %v → %v", before.syncedAt, after.syncedAt)
			}
			if n := countCalendarYearDays(t, tc.year); n != beforeDays {
				t.Errorf("数据行数从 %d 变成了 %d", beforeDays, n)
			}
			afterName, afterOff, afterMakeup, afterSource := readCalendarDay(t, firstOfYear)
			if afterName != beforeName || afterOff != beforeOff ||
				afterMakeup != beforeMakeup || afterSource != beforeSource {
				t.Errorf("1 月 1 日的行被改动了：before=(%q,%v,%v,%q) after=(%q,%v,%v,%q)",
					beforeName, beforeOff, beforeMakeup, beforeSource,
					afterName, afterOff, afterMakeup, afterSource)
			}
		})
	}
}

// 拉回来的是数据时照常覆盖：修「已有定论不许降级」不能把同步本身也挡掉。
//
// 期望值写成字面量（365 天、普通工作日、国庆节），不调判定器从喂进去的数据反推。
func TestPriceCalendarSyncOverwritesSyncedYearWithNewData(t *testing.T) {
	requireDB(t)
	setupEnv(t)
	const year = 2454
	resetCalendarYear(t, year)

	first := holidayCNTestBody(t, year, []string{"https://example.com/old"},
		holidayCNDayJSON("元旦", fmt.Sprintf("%d-01-01", year), true))
	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(first))
	})
	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	}); err != nil {
		t.Fatalf("第一次同步失败: %v", err)
	}

	second := holidayCNTestBody(t, year, []string{"https://example.com/new"},
		holidayCNDayJSON("国庆节", fmt.Sprintf("%d-10-01", year), true))
	withHolidayCNServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(second))
	})
	if err := runCaptured(t, func() error {
		return runPriceCalendar([]string{"sync", "--year", strconv.Itoa(year)})
	}); err != nil {
		t.Fatalf("第二次同步失败: %v", err)
	}

	row := readCalendarYear(t, year)
	if row.status != "synced" || row.dayCount != 365 {
		t.Errorf("年份状态 = %+v，期望 synced / 365 天", row)
	}
	if len(row.papers) != 1 || row.papers[0] != "https://example.com/new" {
		t.Errorf("公告地址 = %v，期望只留第二份的 new", row.papers)
	}
	if n := countCalendarYearDays(t, year); n != 365 {
		t.Errorf("数据行数 = %d，期望 365", n)
	}
	name, off, makeup, _ := readCalendarDay(t, fmt.Sprintf("%d-01-01", year))
	if name != "" || off || makeup {
		t.Errorf("1 月 1 日 = (%q,off=%v,makeup=%v)，期望普通工作日（覆盖没发生）", name, off, makeup)
	}
	name, off, _, _ = readCalendarDay(t, fmt.Sprintf("%d-10-01", year))
	if name != "国庆节" || !off {
		t.Errorf("10 月 1 日 = (%q,off=%v)，期望 国庆节 / 放假", name, off)
	}
}
