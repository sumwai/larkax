package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/consoleapi"
)

// 本文件是 usage summary 的用例：窗口怎么落、合计与三档口径怎么打、
// 空窗口怎么说、以及同一窗口下与控制台 /usage/summary 的数字是否逐字相等。

// usageSummaryEvent 是一条汇总用例的用量事件夹具。
//
// 两笔金额分开给：cost 是实收，plan_reserved_amount 是预留时的面值上界。
// 用例把两个数造得不一样，才验得出「两栏没有被相加或互相替代」。
type usageSummaryEvent struct {
	requestID  string
	model      string
	path       string
	at         time.Time
	cost       string
	plan       string
	prompt     int64
	completion int64
	cacheRead  int64
	cacheWrite int64
	reasoning  int64
}

// insertUsageSummaryEvent 直接写一行带全部 token 槽位与面值的用量事件。
func insertUsageSummaryEvent(t *testing.T, db *sql.DB, userID int64, e usageSummaryEvent) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO usage_events
			(request_id, user_id, canonical_model, prompt_tokens, completion_tokens,
			 cache_read_tokens, cache_write_tokens, reasoning_tokens,
			 billing_path, cost, plan_reserved_amount, asset, priced_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11::numeric,
		        'CNY', $12, $12)`,
		e.requestID, userID, e.model, e.prompt, e.completion, e.cacheRead,
		e.cacheWrite, e.reasoning, e.path, e.cost, e.plan, e.at); err != nil {
		t.Fatalf("写用量事件失败: %v", err)
	}
}

// summaryFixtureEvents 是三行固定夹具：两行 measured、一行 estimated。
//
// 数字刻意选成两个口径不相等（2.50 与 3.25），且 measured 与 estimated
// 各自的合计也各不相同：把 cost 与 plan 相加、只取其中一列、或者把两条路径
// 的金额对调，都会让下面某一条断言红。
func summaryFixtureEvents(windowStart time.Time) []usageSummaryEvent {
	return []usageSummaryEvent{
		{
			requestID: uniqueName("req-measured-a"), model: uniqueName("model"),
			path: "measured", at: windowStart.Add(10 * time.Hour),
			cost: "1.25000000", plan: "2.00000000",
			prompt: 100, completion: 50, cacheRead: 10, cacheWrite: 5, reasoning: 7,
		},
		{
			requestID: uniqueName("req-measured-b"), model: uniqueName("model"),
			path: "measured", at: windowStart.Add(11 * time.Hour),
			cost: "0.50000000", plan: "0.25000000",
			prompt: 30, completion: 20, cacheRead: 2, cacheWrite: 1, reasoning: 1,
		},
		{
			requestID: uniqueName("req-estimated"), model: uniqueName("model"),
			path: "estimated", at: windowStart.Add(12 * time.Hour),
			cost: "0.75000000", plan: "1.00000000",
			prompt: 7, completion: 3,
		},
	}
}

// usageSummaryRow 找出首栏等于 label 的那一行，按空白切开交回来。
//
// 不写死项数：表头里的栏名、金额与 token 都是不含空白的单值，但将来某一栏
// 多一个词就会让下标整体错位（见 LESSONS 里「清单表用例按空白项数解析」）。
// 这里只按**首栏**认行，再从行尾取确定的几栏。
func usageSummaryRow(t *testing.T, out, label string) []string {
	t.Helper()

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == label {
			return fields
		}
	}
	t.Fatalf("表格里没有首栏为 %q 的行：\n%s", label, out)
	return nil
}

// TestResolveUsageWindowUsesSiteTimezone 钉住相对窗口的起止时刻。
//
// 两个时刻都按站点时区切，不按进程时区：同一瞬间在 UTC 的机器上跑，
// --today 仍然要是站点时区的那一天。跨零点那一格是这条用例的重点——
// 把 23:59 算成次日，窗口会整体错一天。
func TestResolveUsageWindowUsesSiteTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载 Asia/Shanghai 失败（缺 tzdata？）: %v", err)
	}

	cases := []struct {
		name string
		mode usageWindowMode
		now  time.Time
		from time.Time
		to   time.Time
	}{
		{
			name: "当天上午",
			mode: usageWindowToday,
			now:  time.Date(2026, 10, 23, 0, 30, 0, 0, loc),
			from: time.Date(2026, 10, 23, 0, 0, 0, 0, loc),
			to:   time.Date(2026, 10, 24, 0, 0, 0, 0, loc),
		},
		{
			name: "跨零点前一刻",
			mode: usageWindowToday,
			now:  time.Date(2026, 10, 23, 23, 59, 59, 0, loc),
			from: time.Date(2026, 10, 23, 0, 0, 0, 0, loc),
			to:   time.Date(2026, 10, 24, 0, 0, 0, 0, loc),
		},
		{
			// 同一瞬间写成 UTC：17:00Z 是东八区的次日 01:00，窗口必须跟着
			// 站点时区走，而不是跟着 now 的 location 走。
			name: "now 带 UTC location",
			mode: usageWindowToday,
			now:  time.Date(2026, 10, 22, 17, 0, 0, 0, time.UTC),
			from: time.Date(2026, 10, 23, 0, 0, 0, 0, loc),
			to:   time.Date(2026, 10, 24, 0, 0, 0, 0, loc),
		},
		{
			name: "本月到现在",
			mode: usageWindowThisMonth,
			now:  time.Date(2026, 10, 23, 12, 0, 0, 0, loc),
			from: time.Date(2026, 10, 1, 0, 0, 0, 0, loc),
			to:   time.Date(2026, 10, 23, 12, 0, 0, 0, loc),
		},
		{
			name: "跨年那个月",
			mode: usageWindowThisMonth,
			now:  time.Date(2027, 1, 2, 8, 0, 0, 0, loc),
			from: time.Date(2027, 1, 1, 0, 0, 0, 0, loc),
			to:   time.Date(2027, 1, 2, 8, 0, 0, 0, loc),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to := resolveUsageWindow(tc.mode, tc.now, loc)
			if !from.Equal(tc.from) || !to.Equal(tc.to) {
				t.Errorf("窗口 = [%s, %s)，想要 [%s, %s)",
					from.Format(time.RFC3339), to.Format(time.RFC3339),
					tc.from.Format(time.RFC3339), tc.to.Format(time.RFC3339))
			}
		})
	}
}

// TestUsageSummaryRejectsBadWindows 钉住四类参数错：都在连库之前报出来。
//
// 这一组不连库：判据是参数错（退出码 2），而参数错必须先于「连不上库」出现
// ——先连库再报的话，一台连不上库的机器上这几个错都看不出来。
func TestUsageSummaryRejectsBadWindows(t *testing.T) {
	absolute := []string{"--from", "2026-10-01T00:00:00Z", "--to", "2026-11-01T00:00:00Z"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "没有 --user",
			args: absolute,
			want: "需要 --user",
		},
		{
			name: "相对窗口与绝对窗口一起给",
			args: append([]string{"--user", "1", "--today"}, absolute...),
			want: "只能给一种",
		},
		{
			name: "两个相对窗口一起给",
			args: []string{"--user", "1", "--today", "--this-month"},
			want: "只能给一个",
		},
		{
			name: "只给 --from",
			args: []string{"--user", "1", "--from", "2026-10-01T00:00:00Z"},
			want: "缺 --to",
		},
		{
			name: "只给 --to",
			args: []string{"--user", "1", "--to", "2026-11-01T00:00:00Z"},
			want: "缺 --from",
		},
		{
			name: "起点不早于终点",
			args: []string{"--user", "1",
				"--from", "2026-11-01T00:00:00Z", "--to", "2026-11-01T00:00:00Z"},
			want: "--from 必须早于 --to",
		},
		{
			name: "时刻看不懂",
			args: []string{"--user", "1", "--from", "昨天", "--to", "今天"},
			want: "--from 的取值",
		},
		{
			name: "多余的位置参数",
			args: []string{"summary-positional"},
			want: "不接受位置参数",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnce(t, runUsage, append([]string{"summary"}, tc.args...))
			if r.err == nil {
				t.Fatalf("这一次应当报错，却成功返回了：stdout=%q", r.stdout)
			}
			if got := exitCode(r.err); got != exitUsage {
				t.Errorf("退出码 = %d，参数错应当是 %d（err=%v）", got, exitUsage, r.err)
			}
			if !strings.Contains(r.err.Error(), tc.want) {
				t.Errorf("错误里没有 %q：%v", tc.want, r.err)
			}
		})
	}
}

// TestUsageSummaryEmptyWindowPrintsZeros 钉住空窗口打 0，不是空表、不是 null。
//
// 「这段窗口里一条都没有」是正常结果，退出 0；但表照样打出来，合计与三档
// 口径都是 0.00000000。只打一句「没有数据」的话，读的人分不清是查过了，
// 还是命令没跑成（见 LESSONS.md 第 16 条）。
func TestUsageSummaryEmptyWindowPrintsZeros(t *testing.T) {
	setupEnv(t)
	// 先确认库能连上再建用户：没有库时这一条跳过，而不是在造夹具那一步
	// 报一个与它想验证的东西无关的错。
	testDB(t)

	userID := createTestUser(t)
	out := runUsageSummaryOut(t, "--user", itoa64(userID),
		"--from", "2020-01-01T00:00:00Z", "--to", "2020-02-01T00:00:00Z")

	if !strings.Contains(out, "窗口：") || !strings.Contains(out, "Asia/Shanghai") {
		t.Errorf("首行没有打出窗口与站点时区：\n%s", out)
	}

	total := usageSummaryRow(t, out, "合计")
	if len(total) < 9 {
		t.Fatalf("合计行切出来只有 %d 项：%q", len(total), total)
	}
	if total[1] != "0" || total[2] != "0.00000000" || total[3] != "0.00000000" {
		t.Errorf("空窗口的合计应当是 0 请求、两个 0.00000000，得到 %v", total)
	}
	for _, label := range []string{"measured", "estimated", "fallback"} {
		row := usageSummaryRow(t, out, label)
		if len(row) < 3 || row[1] != "0" || row[2] != "0.00000000" {
			t.Errorf("空窗口里 %s 那一档应当是 0 请求、金额 0.00000000，得到 %v", label, row)
		}
	}
	// 空窗口不是「没值」：不打 null、不打 `-` 冒充金额、也不打那句空清单说明。
	for _, bad := range []string{"null", "这个范围内没有"} {
		if strings.Contains(out, bad) {
			t.Errorf("空窗口里出现了 %q：\n%s", bad, out)
		}
	}
}

// TestUsageSummaryTotalsAndPaths 钉住合计、两笔金额与三档口径。
//
// 断言取**字面量**而不是从夹具常量回算：从同一份常量推期望值时，常量被改小
// 或两列被对调也一起跟着改，等于没钉住。
func TestUsageSummaryTotalsAndPaths(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	windowStart := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, e := range summaryFixtureEvents(windowStart) {
		insertUsageSummaryEvent(t, db, userID, e)
	}

	window := []string{"--user", itoa64(userID),
		"--from", "2026-10-01T00:00:00Z", "--to", "2026-11-01T00:00:00Z"}
	out := runUsageSummaryOut(t, window...)

	// 合计：请求数、实收、面值、五组 token。
	total := usageSummaryRow(t, out, "合计")
	wantTotal := []string{"合计", "3", "2.50000000", "3.25000000", "137", "73", "12", "6", "8"}
	if strings.Join(total, " ") != strings.Join(wantTotal, " ") {
		t.Errorf("合计行 = %v，想要 %v：\n%s", total, wantTotal, out)
	}

	// 三档口径：只有请求数与金额，面值与 token 打 `-`。
	for _, tc := range []struct {
		path     string
		requests string
		cost     string
	}{
		{"measured", "2", "1.75000000"},
		{"estimated", "1", "0.75000000"},
		{"fallback", "0", "0.00000000"},
	} {
		row := usageSummaryRow(t, out, tc.path)
		if len(row) != 9 || row[1] != tc.requests || row[2] != tc.cost {
			t.Errorf("%s 那一行 = %v，想要请求数 %s、金额 %s：\n%s",
				tc.path, row, tc.requests, tc.cost, out)
		}
		for i := 3; i < len(row); i++ {
			if row[i] != "-" {
				t.Errorf("%s 那一行第 %d 栏 = %q，面值与 token 该打 `-`：%v",
					tc.path, i+1, row[i], row)
			}
		}
	}
}

// TestUsageSummaryDefaultsToToday 钉住「都不给时按 --today 算」与站点时区。
//
// 注入一个固定时刻，让「今天」落得可预期：同一个瞬间在东八区是 10 月 20 日
// 傍晚，在 UTC 仍是同一天下午；窗口要按站点时区切。两条夹具一条在窗口内、
// 一条在窗口外（东八区 10 月 19 日深夜），只有按站点时区切才会分开。
func TestUsageSummaryDefaultsToToday(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	saved := usageNow
	usageNow = func() time.Time { return time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { usageNow = saved })

	userID := createTestUser(t)
	insertUsageSummaryEvent(t, db, userID, usageSummaryEvent{
		requestID: uniqueName("req-inside"), model: uniqueName("model"), path: "measured",
		// 2026-10-20T00:00Z 是东八区 08:00，在「今天」窗口里。
		at:   time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		cost: "0.10000000", plan: "0.20000000", prompt: 1,
	})
	insertUsageSummaryEvent(t, db, userID, usageSummaryEvent{
		requestID: uniqueName("req-outside"), model: uniqueName("model"), path: "measured",
		// 2026-10-19T15:00Z 是东八区 23:00，属于前一天，不在窗口里。
		at:   time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC),
		cost: "0.30000000", plan: "0.40000000", prompt: 2,
	})

	out := runUsageSummaryOut(t, "--user", itoa64(userID))
	if !strings.Contains(out, "2026-10-20T00:00:00+08:00") ||
		!strings.Contains(out, "2026-10-21T00:00:00+08:00") {
		t.Errorf("不给窗口时首行应当打出东八区的当天起止：\n%s", out)
	}
	total := usageSummaryRow(t, out, "合计")
	if len(total) < 3 || total[1] != "1" || total[2] != "0.10000000" {
		t.Errorf("默认 --today 应当只算窗口内的那一条（1 请求、0.10000000），得到 %v：\n%s",
			total, out)
	}
}

// TestUsageSummaryJSONKeepsTheTwoAmountsApart 钉住机器可读档的两笔金额各自独立。
//
// 造两列都非零的夹具，断言 cost 与 plan_reserved_amount 各自出现且等于库里的
// 合计：相加（5.75）、互相替代（两列相等）都会让这一条红。
func TestUsageSummaryJSONKeepsTheTwoAmountsApart(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	windowStart := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, e := range summaryFixtureEvents(windowStart) {
		insertUsageSummaryEvent(t, db, userID, e)
	}

	withFormat(t, formatJSON)
	r := runOnce(t, runUsage, []string{"summary", "--user", itoa64(userID),
		"--from", "2026-10-01T00:00:00Z", "--to", "2026-11-01T00:00:00Z"})
	if r.err != nil {
		t.Fatalf("usage summary -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var doc struct {
		Summary struct {
			UserID   int64  `json:"user_id"`
			Window   string `json:"window"`
			From     string `json:"from"`
			To       string `json:"to"`
			Timezone string `json:"timezone"`
		} `json:"summary"`
		Totals struct {
			Cost               string `json:"cost"`
			PlanReservedAmount string `json:"plan_reserved_amount"`
			Requests           int64  `json:"requests"`
			Tokens             struct {
				Input      int64 `json:"input"`
				Output     int64 `json:"output"`
				CacheRead  int64 `json:"cache_read"`
				CacheWrite int64 `json:"cache_write"`
				Reasoning  int64 `json:"reasoning"`
			} `json:"tokens"`
		} `json:"totals"`
		BillingPaths []struct {
			Path     string `json:"path"`
			Cost     string `json:"cost"`
			Requests int64  `json:"requests"`
		} `json:"billing_paths"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}

	if doc.Summary.UserID != userID || doc.Summary.Window != string(usageWindowAbsolute) {
		t.Errorf("summary 没说清这次查的是谁、哪种窗口：%+v", doc.Summary)
	}
	if doc.Summary.From != "2026-10-01T00:00:00Z" || doc.Summary.To != "2026-11-01T00:00:00Z" {
		t.Errorf("summary 的起止 = %q / %q，机器可读档该恒 UTC", doc.Summary.From, doc.Summary.To)
	}
	if doc.Totals.Cost != "2.50000000" {
		t.Errorf("cost = %q，想要 2.50000000", doc.Totals.Cost)
	}
	if doc.Totals.PlanReservedAmount != "3.25000000" {
		t.Errorf("plan_reserved_amount = %q，想要 3.25000000", doc.Totals.PlanReservedAmount)
	}
	if doc.Totals.Cost == doc.Totals.PlanReservedAmount ||
		doc.Totals.Cost == "5.75000000" || doc.Totals.PlanReservedAmount == "5.75000000" {
		t.Errorf("两笔金额被相加或互相替代了：cost=%q plan=%q",
			doc.Totals.Cost, doc.Totals.PlanReservedAmount)
	}
	if doc.Totals.Requests != 3 {
		t.Errorf("requests = %d，想要 3", doc.Totals.Requests)
	}
	tokens := doc.Totals.Tokens
	if tokens.Input != 137 || tokens.Output != 73 || tokens.CacheRead != 12 ||
		tokens.CacheWrite != 6 || tokens.Reasoning != 8 {
		t.Errorf("tokens = %+v，想要 137 / 73 / 12 / 6 / 8", tokens)
	}

	// 三档恒在，顺序与 store 的固定次序一致。
	if len(doc.BillingPaths) != 3 {
		t.Fatalf("billing_paths 有 %d 档，想要恒三档：%+v", len(doc.BillingPaths), doc.BillingPaths)
	}
	wantPaths := []struct {
		path     string
		cost     string
		requests int64
	}{
		{"measured", "1.75000000", 2},
		{"estimated", "0.75000000", 1},
		{"fallback", "0.00000000", 0},
	}
	for i, want := range wantPaths {
		got := doc.BillingPaths[i]
		if got.Path != want.path || got.Cost != want.cost || got.Requests != want.requests {
			t.Errorf("第 %d 档 = %+v，想要 %+v", i+1, got, want)
		}
	}
}

// usageSummaryConsole 是控制台 /usage/summary 的响应里本用例要比的那几栏。
type usageSummaryConsole struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Totals struct {
		Cost               string `json:"cost"`
		PlanReservedAmount string `json:"plan_reserved_amount"`
		Requests           int64  `json:"requests"`
		Tokens             struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cache_read"`
			CacheWrite int64 `json:"cache_write"`
			Reasoning  int64 `json:"reasoning"`
		} `json:"tokens"`
	} `json:"totals"`
	BillingPaths []struct {
		Path     string `json:"path"`
		Cost     string `json:"cost"`
		Requests int64  `json:"requests"`
	} `json:"billing_paths"`
}

// fetchConsoleUsageSummary 用真库与一个真会话请求控制台端点。
//
// 走真正的 HTTP 路由而不是直接调 store：这一条要证明的是「两个入口的数字
// 逐字相等」，绕过 HTTP 层就等于只证明了 store 等于它自己。
func fetchConsoleUsageSummary(t *testing.T, db *sql.DB, userID int64,
	from, to time.Time) usageSummaryConsole {
	t.Helper()

	handler := consoleapi.New(consoleapi.Config{
		DB:       db,
		KDF:      auth.KDFParams{Iterations: 1000, SaltBytes: 16, KeyBytes: 32},
		Region:   "CN",
		Timezone: "Asia/Shanghai",
		Currency: "CNY",
	})

	ctx := context.Background()
	_, token, err := auth.New(db).CreateSession(ctx, userID, "usage-summary-test", nil)
	if err != nil {
		t.Fatalf("建立会话失败: %v", err)
	}

	query := url.Values{}
	query.Set("from", from.UTC().Format(time.RFC3339))
	query.Set("to", to.UTC().Format(time.RFC3339))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		consoleapi.PathPrefix+"usage/summary?"+query.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("控制台 usage/summary 返回 %d：%s", rec.Code, rec.Body.String())
	}
	var body usageSummaryConsole
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("控制台响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	return body
}

// TestUsageSummaryMatchesConsole 钉住同一窗口下 CLI 与控制台的数字逐字相等。
//
// 两个入口调的是同一个 store 函数（UsageSummary），这一条用例从两边各读一遍
// 再比，而不是把期望值抄一份：抄一份的话，store 口径变了而用例没变，两边
// 同时错也照样绿。
//
// **窗口必须显式给**：控制台不给 from/to 时默认最近 30 天，而 CLI 默认
// --today，两边的默认值本来就不是同一段。
func TestUsageSummaryMatchesConsole(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, e := range summaryFixtureEvents(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		insertUsageSummaryEvent(t, db, userID, e)
	}

	// 先取控制台那一份，再切到 json 打 CLI 那一份：写类命令在 json 档会被拒，
	// 两条都是读类，顺序只是为了不把夹具留在切换之后。
	console := fetchConsoleUsageSummary(t, db, userID, from, to)

	withFormat(t, formatJSON)
	r := runOnce(t, runUsage, []string{"summary", "--user", itoa64(userID),
		"--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339)})
	if r.err != nil {
		t.Fatalf("usage summary -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	// CLI 的 from/to 在 summary 里，控制台的在同一层；两边的 totals 与
	// billing_paths 逐字同名。解成同一份结构体之前先把窗口那一栏摆平——
	// 这里比对的是数字，不把两种信封混为一谈。
	var cli struct {
		usageSummaryConsole
		Summary struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &cli); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}

	// 先钉一遍绝对值：两边同时算成 0 时，下面的逐字段比较会全绿。
	if console.Totals.Cost != "2.50000000" ||
		console.Totals.PlanReservedAmount != "3.25000000" ||
		console.Totals.Requests != 3 {
		t.Fatalf("控制台那一份就先不对：%+v", console.Totals)
	}

	if cli.Summary.From != console.From || cli.Summary.To != console.To {
		t.Errorf("窗口不同：CLI %q/%q，控制台 %q/%q",
			cli.Summary.From, cli.Summary.To, console.From, console.To)
	}
	if cli.Totals.Cost != console.Totals.Cost {
		t.Errorf("cost 不同：CLI %q，控制台 %q", cli.Totals.Cost, console.Totals.Cost)
	}
	if cli.Totals.PlanReservedAmount != console.Totals.PlanReservedAmount {
		t.Errorf("plan_reserved_amount 不同：CLI %q，控制台 %q",
			cli.Totals.PlanReservedAmount, console.Totals.PlanReservedAmount)
	}
	if cli.Totals.Requests != console.Totals.Requests {
		t.Errorf("requests 不同：CLI %d，控制台 %d", cli.Totals.Requests, console.Totals.Requests)
	}
	if cli.Totals.Tokens != console.Totals.Tokens {
		t.Errorf("tokens 不同：CLI %+v，控制台 %+v", cli.Totals.Tokens, console.Totals.Tokens)
	}
	if len(cli.BillingPaths) != len(console.BillingPaths) {
		t.Fatalf("billing_paths 档数不同：CLI %d，控制台 %d",
			len(cli.BillingPaths), len(console.BillingPaths))
	}
	for i := range cli.BillingPaths {
		if cli.BillingPaths[i] != console.BillingPaths[i] {
			t.Errorf("第 %d 档不同：CLI %+v，控制台 %+v",
				i+1, cli.BillingPaths[i], console.BillingPaths[i])
		}
	}
}

// TestUsageSummaryHelpMentionsTheNewAction 钉住短版与长版都写了 summary。
//
// 帮助文本的两道通用门（行数、首行、指针）不检查「某一条子命令在不在里面」：
// 只加进长版时，`usage --help` 上就看不到它，而那正是使用者先看的那一版。
func TestUsageSummaryHelpMentionsTheNewAction(t *testing.T) {
	for name, text := range map[string]string{
		"usageShortUsage": usageShortUsage,
		"usageUsage":      usageUsage,
	} {
		if !strings.Contains(text, "usage summary") {
			t.Errorf("%s 里没有 usage summary", name)
		}
		for _, flag := range []string{"--user", "--today", "--this-month", "--from", "--to"} {
			if !strings.Contains(text, flag) {
				t.Errorf("%s 里没有 %s", name, flag)
			}
		}
	}
}

// runUsageSummaryOut 跑一次 usage summary，返回 stdout。
func runUsageSummaryOut(t *testing.T, args ...string) string {
	t.Helper()

	r := runOnce(t, runUsage, append([]string{"summary"}, args...))
	if r.err != nil {
		t.Fatalf("usage summary %v 失败: %v\nstderr:\n%s", args, r.err, r.stderr)
	}
	return r.stdout
}
