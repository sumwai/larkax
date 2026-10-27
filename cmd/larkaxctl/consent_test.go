package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/snapshotwire"
)

// consentIDRe 从 grant 与 revoke 打出的那一行里取出授权 ID。
var consentIDRe = regexp.MustCompile(`id=(\d+)`)

// createConsentUser 建一个用户，返回 ID 与用户名。
//
// 名字自己起、并且留着，而不是复用 createTestUser：授权命令既认 ID 也认用户名，
// 而「打出来的那一栏是用户名」只有手里有名字才断言得了。
//
// 授权记录跟着用户一起清掉：capture_consents.user_id 上是级联删除，但先删授权
// 再删用户把这件事写明在用例里——留下的行会让下一条用例的 list 多出一条。
func createConsentUser(t *testing.T, db *sql.DB) (int64, string) {
	t.Helper()
	name := uniqueName("consent")
	var id int64
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (username, status) VALUES ($1, 'active') RETURNING id`,
		name).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.ExecContext(bg, `DELETE FROM capture_consents WHERE user_id = $1`, id)
		_, _ = db.ExecContext(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	return id, name
}

// consentRow 是库里那一行的关键列。
type consentRow struct {
	scope          string
	requestID      sql.NullString
	windowStart    sql.NullTime
	windowEnd      sql.NullTime
	consentText    string
	consentVersion int16
	expiresAt      time.Time
	revokedAt      sql.NullTime
}

// readConsentRow 把一条授权记录按列读回来。
//
// 按**列**读而不是按包里的结构读：这一层要证明的是「命令写进库里的形状对不对」，
// 走一遍包里的读路径会把两侧的同一个误解一起证明成对的。
func readConsentRow(t *testing.T, db *sql.DB, id int64) consentRow {
	t.Helper()
	var r consentRow
	if err := db.QueryRowContext(context.Background(), `
		SELECT scope, request_id, window_start, window_end,
		       consent_text, consent_version, expires_at, revoked_at
		  FROM capture_consents
		 WHERE id = $1`, id).Scan(
		&r.scope, &r.requestID, &r.windowStart, &r.windowEnd,
		&r.consentText, &r.consentVersion, &r.expiresAt, &r.revokedAt); err != nil {
		t.Fatalf("读授权记录失败: %v", err)
	}
	return r
}

// countConsents 数一个用户在库里有多少条授权记录。
func countConsents(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM capture_consents WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("数授权记录失败: %v", err)
	}
	return n
}

// consentIDFromStderr 从 grant / revoke 打出的那一行里取出授权 ID。
func consentIDFromStderr(t *testing.T, stderr string) int64 {
	t.Helper()
	m := consentIDRe.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("stderr 里没有授权 ID：%q", stderr)
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("授权 ID 不是整数：%q", m[1])
	}
	return id
}

// rfc3339 把一段时间之后的时刻写成参数要的格式。
func rfc3339(d time.Duration) string {
	return time.Now().UTC().Add(d).Format(time.RFC3339)
}

// listJSON 在机器可读那一档下跑一次 list，跑完立刻还原。
//
// 不把这一档挂到整条用例上（那要用 withFormat）：写类子命令在 json 档下会被
// 拦下来（见 jsonUnsupported），而用例里紧接着还有 grant 与 revoke——它们会
// 以一句「不支持 json」失败，看起来像授权命令坏了。
func listJSON(t *testing.T, args []string) string {
	t.Helper()
	saved := cli.OutputFormat
	cli.OutputFormat = cli.FormatJSON
	defer func() { cli.OutputFormat = saved }()

	var err error
	stdout, _ := captureStreams(t, func() { err = runConsentList(args) })
	if err != nil {
		t.Fatalf("list 的机器可读输出失败: %v", err)
	}
	return stdout
}

// 不带 --agree 时只打同意文案，什么都不写。
//
// # 为什么这一条要连库
//
// 「拒绝」这件事有两半：命令报错，以及**库里没有多出那一行**。只判前一半时，
// 一个「先写库再问同不同意」的实现会安静地通过——而它正是这段代码要防的那种
// 写入：操作者从没看见自己同意的是什么。
func TestConsentGrantNeedsAgree(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	userID, name := createConsentUser(t, db)

	var err error
	stdout, stderr := captureStreams(t, func() {
		err = runConsentGrant([]string{
			"--user", name, "--scope", "single_request",
			"--request-id", "consent-agree-gate", "--expires-at", rfc3339(2 * time.Hour),
		})
	})

	if err == nil {
		t.Fatal("不带 --agree 时应当拒绝")
	}
	if !strings.Contains(err.Error(), "未确认") {
		t.Errorf("错误 = %q，没说清是没确认", err.Error())
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("退出码 = %d，期望 %d（补一个 --agree 重发就能成）", got, cli.ExitUsage)
	}

	// 文案要在**没同意**的那条路上就打出来：不带 --agree 时它就是这次运行的全部
	// 产出，而它写的是 os.Stderr 而不是 humanStderr()——`-q` 压不掉它。
	if !strings.Contains(stderr, snapshotwire.ConsentText) {
		t.Errorf("stderr 里没有完整的同意文案：\n%s", stderr)
	}
	if want := fmt.Sprintf("版本 %d", snapshotwire.ConsentVersion); !strings.Contains(stderr, want) {
		t.Errorf("stderr 里没有 %q：\n%s", want, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout 应当什么都没有，得到 %q", stdout)
	}

	if n := countConsents(t, db, userID); n != 0 {
		t.Errorf("没同意却写了 %d 行授权", n)
	}
}

// 参数写错的那几条路一律是参数错（退出码 2），且不碰库。
//
// 分档的意义在脚本那一侧：参数错要改命令重发，而连库失败要去看环境。混成一个
// 码，脚本只能去匹配错误文本。
func TestConsentRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		fn   func() error
		want string
	}{
		{"没有子命令", func() error { return runConsent(nil) }, "需要子命令"},
		{"未知子命令", func() error { return runConsent([]string{"bogus"}) }, "未知的 consent 子命令"},
		{"grant 缺 --user", func() error {
			return runConsentGrant([]string{"--scope", "single_request"})
		}, "缺少 --user"},
		{"grant 缺 --scope", func() error {
			return runConsentGrant([]string{"--user", "1"})
		}, "缺少 --scope"},
		{"single_request 带了 --from", func() error {
			return runConsentGrant([]string{"--user", "1", "--scope", "single_request",
				"--request-id", "r", "--from", rfc3339(time.Hour), "--agree"})
		}, "不接受 --from / --to"},
		{"single_request 没给到期时间", func() error {
			return runConsentGrant([]string{"--user", "1", "--scope", "single_request",
				"--request-id", "r", "--agree"})
		}, "缺少过期时间"},
		{"时刻不是 RFC3339", func() error {
			return runConsentGrant([]string{"--user", "1", "--scope", "single_request",
				"--request-id", "r", "--expires-at", "2026-10-18 15:00", "--agree"})
		}, "不是 RFC3339 时刻"},
		{"time_window 带了 --request-id", func() error {
			return runConsentGrant([]string{"--user", "1", "--scope", "time_window",
				"--request-id", "r", "--from", rfc3339(time.Hour), "--to", rfc3339(2 * time.Hour),
				"--agree"})
		}, "不接受 --request-id / --expires-at"},
		{"time_window 缺 --to", func() error {
			return runConsentGrant([]string{"--user", "1", "--scope", "time_window",
				"--from", rfc3339(time.Hour), "--agree"})
		}, "缺少 --from 或 --to"},
		{"未知的 --scope", func() error {
			return runConsentGrant([]string{"--user", "1", "--scope", "window", "--agree"})
		}, "--scope 只能是"},
		{"grant 收了位置参数", func() error {
			return runConsentGrant([]string{"--user", "1", "--scope", "single_request",
				"--request-id", "r", "--expires-at", rfc3339(time.Hour), "--agree", "多余"})
		}, "不接受位置参数"},
		{"list 收了位置参数", func() error { return runConsentList([]string{"多余"}) }, "不接受位置参数"},
		{"revoke 缺 ID", func() error { return runConsentRevoke(nil) }, "缺少要撤销的授权 ID"},
		{"revoke 给了两个 ID", func() error { return runConsentRevoke([]string{"1", "2"}) }, "只接受一个授权 ID"},
		{"revoke 的 ID 不是正整数", func() error { return runConsentRevoke([]string{"0"}) }, "正整数"},
		{"revoke 的 ID 不是数字", func() error { return runConsentRevoke([]string{"abc"}) }, "正整数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.fn)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，里面没有 %q", err.Error(), tc.want)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", got, cli.ExitUsage)
			}
		})
	}
}

// 单条请求那一条路：落库的形状、清单上那一栏、撤销之后立刻失效。
func TestConsentSingleRequestRoundTrip(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	_, name := createConsentUser(t, db)

	expires := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)

	var err error
	var stderr string
	_, stderr = captureStreams(t, func() {
		err = runConsentGrant([]string{"--user", name, "--scope", "single_request",
			"--request-id", "consent-rt-1", "--expires-at", expires.Format(time.RFC3339), "--agree"})
	})
	if err != nil {
		t.Fatalf("grant 失败: %v", err)
	}
	if !strings.Contains(stderr, "范围=单条请求") || !strings.Contains(stderr, "request_id=consent-rt-1") {
		t.Errorf("结果行没说清这条授权的范围：%q", stderr)
	}
	if !strings.Contains(stderr, "用户="+name) {
		t.Errorf("结果行里没有用户名：%q", stderr)
	}
	id := consentIDFromStderr(t, stderr)

	row := readConsentRow(t, db, id)
	if row.scope != snapshotwire.ConsentScopeSingleRequest {
		t.Errorf("scope = %q，期望 %q", row.scope, snapshotwire.ConsentScopeSingleRequest)
	}
	if row.requestID.String != "consent-rt-1" {
		t.Errorf("request_id = %q，期望 consent-rt-1", row.requestID.String)
	}
	// 另一组列留 NULL：表上的 shape 约束只要求「该有的那一组非空」，
	// 多写一组不会报错，却会让「这条授权按什么范围生效」要读代码才知道。
	if row.windowStart.Valid || row.windowEnd.Valid {
		t.Errorf("单条请求不该写时间窗：%v / %v", row.windowStart, row.windowEnd)
	}
	if row.consentText != snapshotwire.ConsentText {
		t.Error("落库的同意文案与命令打出来的那一份不是同一段")
	}
	if row.consentVersion != snapshotwire.ConsentVersion {
		t.Errorf("consent_version = %d，期望 %d", row.consentVersion, snapshotwire.ConsentVersion)
	}
	// 上面两句钉的是「写入方与常量同源」与「版本号就是命令用的那个」；这两句
	// 钉的是**同源的那一段到底是哪一段**：只有前两句时，把常量连同命令一起
	// 改掉的实现永远是绿的，而合规举证要的正是这段文字与这个版本号本身。
	if !strings.Contains(row.consentText, "我同意开启请求原文采集。") {
		t.Errorf("落库的同意文案不是这一段：%q", row.consentText)
	}
	if row.consentVersion != 1 {
		t.Errorf("consent_version = %d，期望 1", row.consentVersion)
	}
	if !row.expiresAt.Equal(expires) {
		t.Errorf("expires_at = %s，期望 %s", row.expiresAt, expires)
	}
	if row.revokedAt.Valid {
		t.Error("刚落库的授权不该有撤销时刻")
	}

	// 表格那一档：范围那一栏打的是 request_id，状态那一栏是给人看的中文。
	//
	// 这一条用 -u 这个简写（--user 的短写，与别处同级命令一致）：它是活的
	// 写法，而「活的但没人用」正是简写被悄悄删掉的方式——下面那次 list 用全名，
	// 两个写法各走一遍。
	var stdout string
	stdout, _ = captureStreams(t, func() {
		err = runConsentList([]string{"-u", name})
	})
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}
	for _, want := range []string{"ID", "USER", "SCOPE", "STATE", "consent-rt-1", "single_request", "生效中"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("清单里没有 %q：\n%s", want, stdout)
		}
	}

	// 机器可读那一档：同意文案与版本号要照打——「当初同意的是什么」正是这条
	// 命令要能回答的问题。
	stdout = listJSON(t, []string{"--user", name})
	var doc struct {
		Summary struct {
			Total  int `json:"total"`
			Active int `json:"active"`
		} `json:"summary"`
		Consents []struct {
			ID             int64  `json:"id"`
			Scope          string `json:"scope"`
			RequestID      string `json:"request_id"`
			ConsentText    string `json:"consent_text"`
			ConsentVersion int16  `json:"consent_version"`
			State          string `json:"state"`
		} `json:"consents"`
	}
	if err = json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout 不是 JSON：%v\n%s", err, stdout)
	}
	if doc.Summary.Total != 1 || doc.Summary.Active != 1 {
		t.Errorf("summary = %+v，期望 total=1 active=1", doc.Summary)
	}
	if len(doc.Consents) != 1 {
		t.Fatalf("consents 有 %d 条，期望 1 条", len(doc.Consents))
	}
	got := doc.Consents[0]
	if got.ID != id || got.Scope != snapshotwire.ConsentScopeSingleRequest ||
		got.RequestID != "consent-rt-1" || got.State != snapshotwire.ConsentStateActive {
		t.Errorf("清单里那一条 = %+v，与落库的那一条对不上", got)
	}
	if got.ConsentText != snapshotwire.ConsentText || got.ConsentVersion != snapshotwire.ConsentVersion {
		t.Error("机器可读输出里少了同意文案或版本号")
	}

	// 撤销：一行成功提示，库里那一列真的写上了。
	_, stderr = captureStreams(t, func() {
		err = runConsentRevoke([]string{strconv.FormatInt(id, 10)})
	})
	if err != nil {
		t.Fatalf("revoke 失败: %v", err)
	}
	if !strings.Contains(stderr, "已撤销授权") {
		t.Errorf("revoke 的结果行 = %q", stderr)
	}
	if !readConsentRow(t, db, id).revokedAt.Valid {
		t.Error("撤销之后 revoked_at 应当有值")
	}

	stdout, _ = captureStreams(t, func() {
		err = runConsentList([]string{"--user", name})
	})
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}
	if !strings.Contains(stdout, "已撤销") {
		t.Errorf("撤销之后清单里没显示已撤销：\n%s", stdout)
	}

	// 再撤销一次：那一行已经不在「还没撤销」的那一档里了，按冲突报（退出码 4），
	// 而不是「其他失败」——脚本对这两者的处置不同。
	err = runCaptured(t, func() error { return runConsentRevoke([]string{strconv.FormatInt(id, 10)}) })
	if err == nil {
		t.Fatal("重复撤销应当报错")
	}
	if !errors.Is(err, snapshotwire.ErrConsentNotFound) {
		t.Errorf("错误 = %v，期望包住 ErrConsentNotFound", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("退出码 = %d，期望 %d（冲突）", got, cli.ExitConflict)
	}
}

// 时间窗那一条路：两端进窗口那两列，到期时间就是窗口结束，清单上按窗口显示。
func TestConsentTimeWindowRoundTrip(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	_, name := createConsentUser(t, db)

	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	end := start.Add(2 * time.Hour)

	var err error
	var stderr string
	_, stderr = captureStreams(t, func() {
		err = runConsentGrant([]string{"--user", name, "--scope", "time_window",
			"--from", start.Format(time.RFC3339), "--to", end.Format(time.RFC3339), "--agree"})
	})
	if err != nil {
		t.Fatalf("grant 失败: %v", err)
	}
	id := consentIDFromStderr(t, stderr)

	row := readConsentRow(t, db, id)
	if row.scope != snapshotwire.ConsentScopeTimeWindow {
		t.Errorf("scope = %q，期望 %q", row.scope, snapshotwire.ConsentScopeTimeWindow)
	}
	if row.requestID.Valid {
		t.Errorf("时间窗不该写 request_id：%q", row.requestID.String)
	}
	if !row.windowStart.Valid || !row.windowStart.Time.Equal(start) {
		t.Errorf("window_start = %v，期望 %s", row.windowStart, start)
	}
	if !row.windowEnd.Valid || !row.windowEnd.Time.Equal(end) {
		t.Errorf("window_end = %v，期望 %s", row.windowEnd, end)
	}
	// 到期时间就是窗口结束：撤销与到期两条路各自独立成立。
	if !row.expiresAt.Equal(end) {
		t.Errorf("expires_at = %s，期望与 window_end 相同（%s）", row.expiresAt, end)
	}

	stdout, _ := captureStreams(t, func() {
		err = runConsentList([]string{"--user", name})
	})
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}
	// 窗口还没开始，清单上那一栏就该是「未开始」——它回答的是「这一条此刻能不能
	// 换来原文」，而此刻它换不来。
	if !strings.Contains(stdout, "未开始") {
		t.Errorf("还没开始的窗口没显示为未开始：\n%s", stdout)
	}
	if !strings.Contains(stdout, " → ") {
		t.Errorf("范围那一栏没有打出窗口的两端：\n%s", stdout)
	}
}

// 一条授权都没有时给出明确的空状态文案，而不是一个只有表头的空表。
func TestConsentListEmptyNote(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	_, name := createConsentUser(t, db)

	var err error
	stdout, _ := captureStreams(t, func() {
		err = runConsentList([]string{"--user", name})
	})
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}

	const want = "暂无授权记录。可使用 larkaxctl consent grant 添加。"
	if !strings.Contains(stdout, want) {
		t.Errorf("空清单的文案 = %q，里面没有 %q", stdout, want)
	}
	if strings.Contains(stdout, "SCOPE") {
		t.Errorf("空清单不该打表头：\n%s", stdout)
	}
}

// 超过上限的申请被拒，错误里同时给出上限与申请值；上限确实来自 settings。
//
// # 为什么后半句要单独证明
//
// 只判「25 小时被拒」的话，一个把 24 写死在代码里的实现照样通过——而那个键
// 就还是一个没人读的配置项，改它不会有任何效果。所以这里把它改成 1，看
// 2 小时是不是也跟着被拒。
func TestConsentGrantRejectsOverLimit(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	userID, name := createConsentUser(t, db)

	grant := func(from, to time.Duration) error {
		return runCaptured(t, func() error {
			return runConsentGrant([]string{"--user", name, "--scope", "time_window",
				"--from", rfc3339(from), "--to", rfc3339(to), "--agree"})
		})
	}

	// 默认上限 24 小时。
	err := grant(0, 25*time.Hour)
	if err == nil {
		t.Fatal("超过默认上限的申请应当被拒")
	}
	for _, want := range []string{"25 小时", "24 小时"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %q，里面没有 %q（上限与申请值都要给）", err.Error(), want)
		}
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("退出码 = %d，期望 %d（改一下时长重发就能成）", got, cli.ExitUsage)
	}

	// 上限是 settings 里的那个值，不是写死的 24。
	if err = runCaptured(t, func() error {
		return runSettingsSet([]string{"--key", "capture.consent_max_window_hours",
			"--value", "1", "--by", name})
	}); err != nil {
		t.Fatalf("改上限失败: %v", err)
	}
	t.Cleanup(func() {
		_ = runCaptured(t, func() error {
			return runSettingsSet([]string{"--key", "capture.consent_max_window_hours",
				"--value", "24", "--by", name})
		})
	})

	err = grant(0, 2*time.Hour)
	if err == nil {
		t.Fatal("上限改成 1 小时之后，2 小时的申请应当被拒")
	}
	for _, want := range []string{"2 小时", "1 小时"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %q，里面没有 %q", err.Error(), want)
		}
	}

	// 一条都不该落库：超限是拒绝，不是夹到上限。
	if n := countConsents(t, db, userID); n != 0 {
		t.Errorf("被拒的申请写进了 %d 行授权", n)
	}
}

// grant 的 --ttl 按注入的现在换算，写进库里的时刻逐秒相等。
func TestConsentGrantTTLUsesInjectedNow(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	_, name := createConsentUser(t, db)

	now := time.Now().UTC().Truncate(time.Second)
	withExpiryNow(t, now)

	var err error
	var stderr string
	_, stderr = captureStreams(t, func() {
		err = runConsentGrant([]string{"--user", name, "--scope", "single_request",
			"--request-id", "consent-ttl-1", "--ttl", "2h", "--agree"})
	})
	if err != nil {
		t.Fatalf("grant 失败: %v", err)
	}
	id := consentIDFromStderr(t, stderr)

	want := now.Add(2 * time.Hour)
	if row := readConsentRow(t, db, id); !row.expiresAt.Equal(want) {
		t.Errorf("expires_at = %s，期望 %s", row.expiresAt, want.Format(time.RFC3339))
	}
}

// --forever 换出来的是远期上限，仍然要过时长上限；它被拒时一行都不写。
func TestConsentGrantForeverIsStillCapped(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	userID, name := createConsentUser(t, db)

	err := runCaptured(t, func() error {
		return runConsentGrant([]string{"--user", name, "--scope", "single_request",
			"--request-id", "consent-forever-1", "--forever", "--agree"})
	})
	if err == nil {
		t.Fatal("永久授权应当被时长上限拒绝")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("错误 = %q，里面没有「超过上限」", err.Error())
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("退出码 = %d，期望 %d（参数错）", got, cli.ExitUsage)
	}
	if n := countConsents(t, db, userID); n != 0 {
		t.Errorf("被拒的申请写进了 %d 行授权", n)
	}
}

// 三种过期写法互斥、time_window 不收它们：这些判据都在连库之前。
func TestConsentGrantRejectsExpiryFlagMixups(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"expires-at 与 ttl", []string{"--user", "1", "--scope", "single_request",
			"--request-id", "r", "--expires-at", "2030-01-01T00:00:00Z", "--ttl", "30d",
			"--agree"}, "不能同时给"},
		{"ttl 与 forever", []string{"--user", "1", "--scope", "single_request",
			"--request-id", "r", "--ttl", "30d", "--forever", "--agree"}, "不能同时给"},
		{"三个都没给", []string{"--user", "1", "--scope", "single_request",
			"--request-id", "r", "--agree"}, "缺少过期时间"},
		{"ttl 单位不认识", []string{"--user", "1", "--scope", "single_request",
			"--request-id", "r", "--ttl", "30w", "--agree"}, "单位"},
		{"time_window 收 ttl", []string{"--user", "1", "--scope", "time_window",
			"--from", "2030-01-01T00:00:00Z", "--to", "2030-01-01T01:00:00Z",
			"--ttl", "30d", "--agree"}, "不接受"},
		{"time_window 收 forever", []string{"--user", "1", "--scope", "time_window",
			"--from", "2030-01-01T00:00:00Z", "--to", "2030-01-01T01:00:00Z",
			"--forever", "--agree"}, "不接受"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runConsentGrant(tc.args)
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", got, cli.ExitUsage)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，里面没有 %q", err.Error(), tc.want)
			}
		})
	}
}
