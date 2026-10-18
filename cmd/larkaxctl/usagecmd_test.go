package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件是 usage 命令的用例。**根帮助页那一组不在里**：它们在 usage_test.go
// （那个文件名说的是「帮助文本里的 usage」）。

// usageFixtureTime 是这一组用例用的两个时间窗。
//
// 两个窗落在**相邻的两个月**上：usage_events 按月分区，跨月那道边界正是最容易
// 写错、又在库里看不出来的那一格（见 TestUsageEventsWindowCrossesMonthBoundary）。
var (
	usageWindowOctFrom = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	usageWindowOctTo   = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	usageWindowNovFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	usageWindowNovTo   = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
)

// insertUsageEvent 直接写一行用量事件。
//
// 走 SQL 而不是走结算路径：这一组用例要的是「库里已经有这几行」这个前提，而结算
// 那条路上的每一步（预留、求值、落库）都有自己的用例。created_at 显式给——它既是
// 分区键也是这条命令筛窗口用的那一列，用例要把它钉在指定的月份里。
func insertUsageEvent(t *testing.T, db *sql.DB, requestID string,
	userID int64, model string, createdAt time.Time, cost string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO usage_events
			(request_id, user_id, canonical_model, prompt_tokens, completion_tokens,
			 billing_path, cost, asset, priced_at, created_at)
		VALUES ($1, $2, $3, 100, 50, 'measured', $4::numeric, 'CNY', $5, $5)`,
		requestID, userID, model, cost, createdAt)
	if err != nil {
		t.Fatalf("写用量事件失败: %v", err)
	}
}

// usageEventsModels 跑一次 usage events，返回打出来的那些模型名。
//
// 判据取**模型名**这一栏而不是行数：给每一行起一个各不相同的模型名之后，
// 「按谁筛的、筛掉了谁」在输出里逐行对得上；只数行数的话，把两条记录筛反了
// 也是同样的行数。
func usageEventsModels(t *testing.T, args ...string) []string {
	t.Helper()

	r := runOnce(t, runUsage, append([]string{"events"}, args...))
	if r.err != nil {
		t.Fatalf("usage events %v 失败: %v\nstderr:\n%s", args, r.err, r.stderr)
	}
	return tableModels(r.stdout)
}

// tableModels 从一张 usage events 表格里抠出模型名那一栏。
//
// 表头是 10 栏、模型在第 4 栏；这一组用例造出来的模型名都是 uniqueName 的唯一值，
// 所以直接在行里找得到它们，不必解析列宽。
func tableModels(out string) []string {
	var models []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 10 {
			continue
		}
		models = append(models, fields[3])
	}
	return models
}

// TestUsageEventsFiltersByUserAndWindow 钉住筛选真的生效。
//
// 同一张表里放两个用户、两个时间窗共四条记录，然后分别按用户筛、按窗口筛：
// 每次都只该捞回该捞的那几条。「有输出就算过」在这里没有意义——四条记录全捞回来
// 也是一次有输出的成功。
func TestUsageEventsFiltersByUserAndWindow(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	alice := createTestUser(t)
	bob := createTestUser(t)
	aliceOct := uniqueName("model-alice-oct")
	aliceNov := uniqueName("model-alice-nov")
	bobOct := uniqueName("model-bob-oct")

	// 窗口内的两条各在不同月份，第三条是 bob 的同月记录，第四条在下一个窗口里。
	insertUsageEvent(t, db, uniqueName("req"), alice, aliceOct,
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "0.01000000")
	insertUsageEvent(t, db, uniqueName("req"), alice, aliceNov,
		time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC), "0.02000000")
	insertUsageEvent(t, db, uniqueName("req"), bob, bobOct,
		time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), "0.03000000")

	aliceRef := itoa64(alice)

	// ① 按用户 + 窗口筛：只有 alice 在十月的那一条。
	// 用户名与 ID 两种写法都要认（与其余命令同一条约定），所以各跑一次。
	for _, ref := range []string{aliceRef, usernameOf(t, db, alice)} {
		got := usageEventsModels(t, "--user", ref,
			"--from", usageWindowOctFrom.Format(time.RFC3339),
			"--to", usageWindowOctTo.Format(time.RFC3339))
		if len(got) != 1 || got[0] != aliceOct {
			t.Errorf("--user %s 十月窗口筛出来 %v，只该有 %s", ref, got, aliceOct)
		}
	}

	// ② 换用户：同一窗口里只剩下 bob 那一条。
	got := usageEventsModels(t, "--user", itoa64(bob),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339))
	if len(got) != 1 || got[0] != bobOct {
		t.Errorf("bob 十月窗口筛出来 %v，只该有 %s", got, bobOct)
	}

	// ③ 只按窗口筛（不给 --user）：同一个用户的两条按月份分开。
	got = usageEventsModels(t, "--user", aliceRef,
		"--from", usageWindowNovFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if len(got) != 1 || got[0] != aliceNov {
		t.Errorf("alice 十一月窗口筛出来 %v，只该有 %s", got, aliceNov)
	}
}

// TestUsageEventsWindowCrossesMonthBoundary 钉住跨月窗口一条都不漏。
//
// usage_events 按月分区，而窗口谓词写的正是分区键那一列（created_at）。两个月的
// 交界上：窗口跨月时两条都要在，窗口只覆盖后一个月时前一条不能进来。这一格错了
// 在库里看不出来——少一行与「那个月本来就没用量」在输出上长得一样。
func TestUsageEventsWindowCrossesMonthBoundary(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	lastOfOct := uniqueName("model-oct-last")
	firstOfNov := uniqueName("model-nov-first")

	// 十月最后一小时与十一月第一小时，落在两个相邻分区上。
	insertUsageEvent(t, db, uniqueName("req"), userID, lastOfOct,
		time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), "0.01000000")
	insertUsageEvent(t, db, uniqueName("req"), userID, firstOfNov,
		time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC), "0.01000000")

	got := usageEventsModels(t, "--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if !contains(got, lastOfOct) || !contains(got, firstOfNov) {
		t.Errorf("跨月窗口（十月一日到十二月一日）筛出来 %v，交界两边的 %s 与 %s 都要在",
			got, lastOfOct, firstOfNov)
	}

	// 窗口起点压在十一月一日零点：十月那一条（含 10-31 23:00）不再进来，
	// 十一月那一条照旧——左闭右开的边界在**分区边界**上也要成立。
	got = usageEventsModels(t, "--user", itoa64(userID),
		"--from", usageWindowNovFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if contains(got, lastOfOct) {
		t.Errorf("十一月窗口里出现了十月那一条（%s）：窗口起点是左闭的，十月的不该在", lastOfOct)
	}
	if !contains(got, firstOfNov) {
		t.Errorf("十一月窗口里没有 %s：跨月之后的那一条被漏掉了", firstOfNov)
	}
}

// insertBillingRequest 直接写一行计费请求。
//
// 与用量事件那条同一条理由：这一组要的是「库里已经有这几行」这个前提。
func insertBillingRequest(t *testing.T, db *sql.DB, requestID string,
	userID int64, model string, startedAt time.Time, status string) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO billing_requests
			(request_id, user_id, canonical_model, protocol, streamed,
			 reserved_amount, reserved_asset, status, started_at)
		VALUES ($1, $2, $3, 'openai', false, 0.02, 'CNY', $4, $5)`,
		requestID, userID, model, status, startedAt)
	if err != nil {
		t.Fatalf("写计费请求失败: %v", err)
	}
}

// TestUsageRequestsFiltersByUserAndWindow 与用量事件那条同一个判据：筛真的生效。
func TestUsageRequestsFiltersByUserAndWindow(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	alice := createTestUser(t)
	bob := createTestUser(t)
	aliceReq := uniqueName("req-alice")
	bobReq := uniqueName("req-bob")

	insertBillingRequest(t, db, aliceReq, alice, uniqueName("model"),
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "pending")
	insertBillingRequest(t, db, bobReq, bob, uniqueName("model"),
		time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC), "pending")

	out := runUsageRequestsOut(t, "--user", itoa64(alice),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339))
	if !strings.Contains(out, aliceReq) {
		t.Errorf("按 alice 筛出来的清单里没有她自己的请求 %s：\n%s", aliceReq, out)
	}
	if strings.Contains(out, bobReq) {
		t.Errorf("按 alice 筛出来的清单里出现了 bob 的请求 %s：\n%s", bobReq, out)
	}

	// 换窗口：十一月那一段里两条都不在。
	out = runUsageRequestsOut(t, "--user", itoa64(alice),
		"--from", usageWindowNovFrom.Format(time.RFC3339),
		"--to", usageWindowNovTo.Format(time.RFC3339))
	if strings.Contains(out, aliceReq) {
		t.Errorf("十一月窗口里出现了十月的请求 %s：\n%s", aliceReq, out)
	}
}

// runUsageRequestsOut 跑一次 usage requests 并把 stdout 交回来。
func runUsageRequestsOut(t *testing.T, args ...string) string {
	t.Helper()

	r := runOnce(t, runUsage, append([]string{"requests"}, args...))
	if r.err != nil {
		t.Fatalf("usage requests %v 失败: %v\nstderr:\n%s", args, r.err, r.stderr)
	}
	return r.stdout
}

// TestUsageListsStateEmptyResults 钉住空结果也说一句话。
//
// 一行都不打的话，「这个窗口里没有记录」与「命令没跑成」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。两条命令各一档，机器可读档里同一件事是 total 为 0。
func TestUsageListsStateEmptyResults(t *testing.T) {
	setupEnv(t)
	// 先连库再建数据：没有库时这一条跳过，而不是在造夹具那一步报一个
	// 与它想验证的东西无关的错。
	testDB(t)
	userID := createTestUser(t)

	empty := []string{"--user", itoa64(userID),
		"--from", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"--to", time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)}

	cases := []struct {
		verb string
		want string
	}{
		{"events", "这个范围内没有用量事件。"},
		{"requests", "这个范围内没有计费请求。"},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			r := runOnce(t, runUsage, append([]string{tc.verb}, empty...))
			if r.err != nil {
				t.Fatalf("usage %s 失败: %v\nstderr:\n%s", tc.verb, r.err, r.stderr)
			}
			if !strings.Contains(r.stdout, tc.want) {
				t.Errorf("空清单没有说出那句话 %q：\nstdout=%s", tc.want, r.stdout)
			}
			// 空结果不是失败：退出码仍是 0，一行表格都没有。
			if strings.Contains(r.stdout, "时刻") {
				t.Errorf("空清单打出了表头：\n%s", r.stdout)
			}
		})
	}
}

// TestUsageLimitSaysHowManyItDropped 钉住被截住时清单自己说出来。
//
// 一份少了后文的清单与一份完整的清单在屏幕上长得一样，看的人会照着第一份下结论
// （见 LESSONS.md 第 16 条），所以末尾那句话必须有；而没被截住时它不该出现——
// 每次都说一句「可能还有」与不说一样没用。
func TestUsageLimitSaysHowManyItDropped(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	window := []string{"--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)}
	for i := 0; i < 3; i++ {
		insertUsageEvent(t, db, uniqueName("req"), userID, uniqueName("model"),
			time.Date(2026, 10, 20, 10, i, 0, 0, time.UTC), "0.01000000")
	}

	got := usageEventsModels(t, append(window, "--limit", "2")...)
	if len(got) != 2 {
		t.Fatalf("--limit 2 打了 %d 行，想要 2 行：%v", len(got), got)
	}
	r := runOnce(t, runUsage, append([]string{"events", "--limit", "2"}, window...))
	if !strings.Contains(r.stdout, "更早的没打出来") {
		t.Errorf("被 --limit 截住时没有说明只打了多少条：\n%s", r.stdout)
	}

	// 不限条数时三条都在，那句话也不该出现。
	got = usageEventsModels(t, append(window, "--limit", "0")...)
	if len(got) != 3 {
		t.Fatalf("--limit 0 打了 %d 行，想要 3 行：%v", len(got), got)
	}
	r = runOnce(t, runUsage, append([]string{"events", "--limit", "0"}, window...))
	if strings.Contains(r.stdout, "更早的没打出来") {
		t.Errorf("没被截住却说了「更早的没打出来」：\n%s", r.stdout)
	}

	// 边界那一格：--limit 正好等于行数。这时后面没有别的行了，而「打出来的条数
	// 等于上限」在两件事上长得一样（刚好打完 / 恰好被截住），所以截断的判据不能
	// 由行数推——多读一行才分得开。这一格写错时，一份已经读完的清单会劝人再去
	// 调大 --limit。
	r = runOnce(t, runUsage, append([]string{"events", "--limit", "3"}, window...))
	if got := len(tableModels(r.stdout)); got != 3 {
		t.Fatalf("--limit 3 打了 %d 行，想要 3 行：\n%s", got, r.stdout)
	}
	if strings.Contains(r.stdout, "更早的没打出来") {
		t.Errorf("--limit 正好等于行数（后面没有别的行了）却说被截住了：\n%s", r.stdout)
	}
}

// TestUsageJSONShapes 钉住机器可读档的两条形状。
//
// 判据是「只有一份 JSON、金额是定点字符串、没结算的那一栏是 null、筛的是什么
// 带在 summary 里」——这几样都是给管道下游看的契约（见 docs/cli-output.md）。
func TestUsageJSONShapes(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID := createTestUser(t)
	model := uniqueName("model")
	pendingReq := uniqueName("req-pending")
	insertUsageEvent(t, db, uniqueName("req"), userID, model,
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "0.01234500")
	insertBillingRequest(t, db, pendingReq, userID, model,
		time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "pending")

	// 夹具都在切到 json 之前建好：写类的命令在那一档会被拒（见 format.go 的
	// jsonUnsupported）。这一组只用读类命令，但这条纪律照样守着。
	withFormat(t, formatJSON)

	r := runOnce(t, runUsage, []string{"events", "--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)})
	if r.err != nil {
		t.Fatalf("usage events -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var events struct {
		Status  string `json:"status"`
		Summary struct {
			Total     int    `json:"total"`
			Truncated bool   `json:"truncated"`
			Limit     int    `json:"limit"`
			UserID    *int64 `json:"user_id"`
			From      string `json:"from"`
		} `json:"summary"`
		Events []struct {
			RequestID  string `json:"request_id"`
			UserID     int64  `json:"user_id"`
			Model      string `json:"canonical_model"`
			Cost       string `json:"cost"`
			Billing    string `json:"billing_path"`
			CreatedAt  string `json:"created_at"`
			Prompt     int64  `json:"prompt_tokens"`
			Completion int64  `json:"completion_tokens"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &events); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if events.Status != jsonOK || events.Summary.Total != 1 || len(events.Events) != 1 {
		t.Fatalf("形状不对：status=%q total=%d 行数=%d\n%s",
			events.Status, events.Summary.Total, len(events.Events), r.stdout)
	}
	if events.Summary.Truncated {
		t.Errorf("一条都没截掉，truncated 却是 true：\n%s", r.stdout)
	}
	if events.Summary.Limit != usageDefaultLimit {
		t.Errorf("summary.limit = %d，没给 --limit 时应当是默认的 %d",
			events.Summary.Limit, usageDefaultLimit)
	}
	if events.Summary.UserID == nil || *events.Summary.UserID != userID {
		t.Errorf("summary 里没有说这次筛的是哪个用户：%s", r.stdout)
	}
	if events.Summary.From == "" {
		t.Errorf("summary 里没有说这次窗口从哪开始：%s", r.stdout)
	}
	e := events.Events[0]
	if e.UserID != userID || e.Model != model {
		t.Errorf("这一行是 %+v，想要 user_id=%d model=%s", e, userID, model)
	}
	// 金额是十进制定点字符串，不是 JSON 数字：过一遍 float64 会丢精度。
	if e.Cost != "0.01234500" {
		t.Errorf("cost = %q，想要定点字符串 0.01234500", e.Cost)
	}
	if e.Billing != "measured" || e.CreatedAt == "" {
		t.Errorf("billing_path 或 created_at 不对：%+v", e)
	}

	r = runOnce(t, runUsage, []string{"requests", "--user", itoa64(userID),
		"--from", usageWindowOctFrom.Format(time.RFC3339),
		"--to", usageWindowOctTo.Format(time.RFC3339)})
	if r.err != nil {
		t.Fatalf("usage requests -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var requests struct {
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Requests []struct {
			RequestID     string  `json:"request_id"`
			Status        string  `json:"status"`
			Reserved      string  `json:"reserved_amount"`
			FinalAmount   *string `json:"final_amount"`
			SettledAt     *string `json:"settled_at"`
			Streamed      bool    `json:"streamed"`
			CanonicalName string  `json:"canonical_model"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &requests); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if requests.Summary.Total != 1 || len(requests.Requests) != 1 {
		t.Fatalf("形状不对：total=%d 行数=%d\n%s",
			requests.Summary.Total, len(requests.Requests), r.stdout)
	}
	req := requests.Requests[0]
	if req.RequestID != pendingReq || req.Status != "pending" {
		t.Errorf("这一行是 %+v，想要 request_id=%s status=pending", req, pendingReq)
	}
	// 没结算：结算金额与结算时刻都是 null，不是 `-`，也不是 0。
	if req.FinalAmount != nil || req.SettledAt != nil {
		t.Errorf("没结算的请求，final_amount 与 settled_at 应当是 null：%+v", req)
	}
	if req.Reserved != "0.02000000" {
		t.Errorf("reserved_amount = %q，想要 0.02000000", req.Reserved)
	}
}

// TestUsageRejectsBadFlags 钉住参数错在连库之前就报出来。
//
// 这一组**不连库**：判据是参数错（退出码 2），而那要发生在配置加载与连库之前
// ——先连库再报参数错的话，一台连不上库的机器上这几个错都看不出来。
func TestUsageRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"窗口上下颠倒", []string{"events", "--from", "2026-11-01", "--to", "2026-10-01"},
			"--from 必须早于 --to"},
		{"时刻看不懂", []string{"events", "--from", "昨天"}, "--from 的取值"},
		{"条数为负", []string{"events", "--limit", "-1"}, "--limit 不能为负"},
		{"多余的位置参数", []string{"events", "alice"}, "不接受位置参数"},
		{"没有子命令", nil, "usage 需要子命令"},
		{"未知子命令", []string{"spend"}, "未知的 usage 子命令"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnce(t, runUsage, tc.args)
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

// usernameOf 取一个用户的用户名，供「ID 与用户名两种写法都认」那一档用。
func usernameOf(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()

	var name string
	if err := db.QueryRowContext(context.Background(),
		`SELECT username FROM users WHERE id = $1`, userID).Scan(&name); err != nil {
		t.Fatalf("查用户名失败: %v", err)
	}
	return name
}

// itoa64 把用户 ID 打成命令行上那种写法。
func itoa64(v int64) string { return fmt.Sprintf("%d", v) }

// contains 判断一串里有没有这一项。
func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
