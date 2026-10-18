package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 本文件是 `snapshot access-log` 只读入口的用例。
//
// 写入那一侧（`snapshot get` 读一次就写一行）由 snapshot_test.go 的
// TestSnapshotGetWritesAccessLog 钉住。这一组补的是另一半：那些行读得回来、
// 筛得动、空结果与截断都说得出话。
//
// # 为什么有两类夹具
//
// 一条走**真写入方**（insertSnapshot + runSnapshotGet）：证明「谁读过」这件事
// 从写到读是通的，而不是「我以为那几列长这样」。
//
// 另一类直接写 snapshot_access_log 行：accessed_at 由库上的 now() 决定，
// 要验「按窗口筛」就得有**时刻可控**的行（几分钟之内写下的几行落在同一个窗口里，
// 拿它们验窗口等于什么都没验）。这一类的取舍写在各条用例上面。

// accessLogFixture 是要直接写进 snapshot_access_log 的一行。
type accessLogFixture struct {
	requestID  string
	snapshotID int64
	actor      sql.NullInt64
	kind       string
	reason     sql.NullString
	at         time.Time
}

// insertAccessLog 直接写一行访问记录并返回它的 id。
func insertAccessLog(t *testing.T, db *sql.DB, f accessLogFixture) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO snapshot_access_log
			(snapshot_id, request_id, actor_user_id, actor_kind, reason, accessed_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		f.snapshotID, f.requestID, f.actor, f.kind, f.reason, f.at).Scan(&id); err != nil {
		t.Fatalf("写访问记录失败: %v", err)
	}
	return id
}

// accessLogOut 跑一次 access-log 并把 stdout 交回来。
func accessLogOut(t *testing.T, args ...string) string {
	t.Helper()

	r := runOnce(t, runSnapshot, append([]string{"access-log"}, args...))
	if r.err != nil {
		t.Fatalf("snapshot access-log %v 失败: %v\nstderr:\n%s", args, r.err, r.stderr)
	}
	return r.stdout
}

// accessLogLine 取出打出来的那一行里含 requestID 的那条。
//
// 按 request_id 找而不是按行号找：这张表是共享的（别的用例也往里写），
// 而 request_id 每个用例各不相同。
func accessLogLine(t *testing.T, out, requestID string) string {
	t.Helper()

	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, requestID) {
			return line
		}
	}
	t.Fatalf("输出里没有 %s 那一行：\n%s", requestID, out)
	return ""
}

// TestSnapshotAccessLogShowsWhatGetWrote 是这条命令最要紧的判据：
// 读快照留下的那笔痕迹，在命令行上读得回来。
//
// 夹具走真写入方：先用 insertSnapshot 造一条快照，再真的跑一次
// `snapshot get`，然后拿 access-log 把它读回来。两边用的是同一批列，
// 但写出那一行的代码与我在这里断言的字段没有共享过——所以对不上就会红。
func TestSnapshotAccessLogShowsWhatGetWrote(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	userID, username := seedSnapshotUser(t, db)
	requestID := uniqueName("req")
	reason := uniqueName("查一下这个请求的形状")
	snapshotID := insertSnapshot(t, db, probeSnapshot{
		requestID: requestID, userID: userID, direction: "request",
		skeleton:  `{"model":"probe-model"}`,
		createdAt: time.Now().UTC().Add(-time.Minute),
	})

	r := runOnce(t, runSnapshot, []string{"get", requestID, "--actor", username, "--reason", reason})
	if r.err != nil {
		t.Fatalf("snapshot get 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}

	out := accessLogOut(t, "--request-id", requestID)
	line := accessLogLine(t, out, requestID)
	fields := strings.Fields(line)

	// 行里那几栏：快照 ID、读取者的用户 ID、用户名、身份、理由原文。
	// 理由按原文打（它是写下的那句话），身份是写日志那一侧定下的档。
	if !strings.Contains(line, itoa64(snapshotID)) {
		t.Errorf("那一行里没有快照 ID %d：%s", snapshotID, line)
	}
	if !strings.Contains(line, itoa64(userID)) || !strings.Contains(line, username) {
		t.Errorf("那一行里没有读取者（user_id=%d username=%s）：%s", userID, username, line)
	}
	if !strings.Contains(line, snapshotActorKindAdmin) {
		t.Errorf("那一行的身份不是 %q：%s", snapshotActorKindAdmin, line)
	}
	if !strings.Contains(line, reason) {
		t.Errorf("那一行里没有理由原文 %q：%s", reason, line)
	}
	if len(fields) < 5 {
		t.Errorf("那一行字段数不对（%d）：%q", len(fields), line)
	}

	// 库里那一行与打出来的这一行说的是同一个人、同一条快照。
	logRows := readAccessLog(t, db, requestID)
	if len(logRows) != 1 {
		t.Fatalf("库里 %s 名下应当正好一行访问记录，得到 %d 行", requestID, len(logRows))
	}
	if logRows[0].ActorUserID.Int64 != userID || logRows[0].ActorKind != snapshotActorKindAdmin {
		t.Errorf("库里那一行是 %+v，与命令行打出来的对不上", logRows[0])
	}
}

// TestSnapshotAccessLogFiltersByActorRequestAndWindow 钉住三个筛选各自生效。
//
// 三条记录分属两个用户、两个时间窗、三个 request_id。判据是**每次只捞该捞的
// 那一条**：三条全捞回来同样是一次有输出的成功，所以「有输出」在这里没有意义。
func TestSnapshotAccessLogFiltersByActorRequestAndWindow(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	alice, _ := seedSnapshotUser(t, db)
	bob, _ := seedSnapshotUser(t, db)

	octFrom := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	octTo := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	novFrom := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	novTo := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	// 时刻可控的行：见文件头那一节。三条的 request_id 互不相同，
	// 于是「哪一条被捞回来了」在输出里逐条对得上。
	aliceOct := uniqueName("req-alice-oct")
	aliceNov := uniqueName("req-alice-nov")
	bobOct := uniqueName("req-bob-oct")
	insertAccessLog(t, db, accessLogFixture{
		requestID: aliceOct, snapshotID: 1, actor: sql.NullInt64{Int64: alice, Valid: true},
		kind: "owner", reason: sql.NullString{String: "自己的请求", Valid: true},
		at: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
	})
	insertAccessLog(t, db, accessLogFixture{
		requestID: aliceNov, snapshotID: 2, actor: sql.NullInt64{Int64: alice, Valid: true},
		kind: "owner", reason: sql.NullString{String: "自己的请求", Valid: true},
		at: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
	})
	insertAccessLog(t, db, accessLogFixture{
		requestID: bobOct, snapshotID: 3, actor: sql.NullInt64{Int64: bob, Valid: true},
		kind: "admin", reason: sql.NullString{String: "排查", Valid: true},
		at: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
	})

	oct := []string{"--from", octFrom.Format(time.RFC3339), "--to", octTo.Format(time.RFC3339)}
	nov := []string{"--from", novFrom.Format(time.RFC3339), "--to", novTo.Format(time.RFC3339)}

	// ① 按读取者 + 窗口：alice 在十月的那一条，另两条都不该在。
	out := accessLogOut(t, append([]string{"--user", itoa64(alice)}, oct...)...)
	if !strings.Contains(out, aliceOct) {
		t.Errorf("按 alice 筛十月的清单里没有她自己那一条：\n%s", out)
	}
	if strings.Contains(out, aliceNov) {
		t.Errorf("十月的窗口里出现了十一月那一条：\n%s", out)
	}
	if strings.Contains(out, bobOct) {
		t.Errorf("按 alice 筛的清单里出现了 bob 那一条：\n%s", out)
	}

	// ② 换读取者：同一窗口里只剩 bob 那一条。
	out = accessLogOut(t, append([]string{"--user", itoa64(bob)}, oct...)...)
	if !strings.Contains(out, bobOct) || strings.Contains(out, aliceOct) {
		t.Errorf("按 bob 筛十月的清单不对：\n%s", out)
	}

	// ③ 换窗口：alice 那两条按月份分开。
	out = accessLogOut(t, append([]string{"--user", itoa64(alice)}, nov...)...)
	if !strings.Contains(out, aliceNov) || strings.Contains(out, aliceOct) {
		t.Errorf("按 alice 筛十一月的清单不对：\n%s", out)
	}

	// ④ 按 request_id 筛：不管是谁、什么时候，只捞那一条。
	out = accessLogOut(t, "--request-id", bobOct)
	if !strings.Contains(out, bobOct) || strings.Contains(out, aliceOct) {
		t.Errorf("按 request_id 筛的清单不对：\n%s", out)
	}

	// ⑤ 窗口左闭右开：起点押在十一月一日零点，十月那一条要落在外头。
	out = accessLogOut(t, "--user", itoa64(alice),
		"--from", novFrom.Format(time.RFC3339),
		"--to", novFrom.Add(time.Hour).Format(time.RFC3339))
	if !strings.Contains(out, "这个范围内没有快照访问记录。") {
		t.Errorf("窗口里没有记录时也该说一句话：\n%s", out)
	}
}

// TestSnapshotAccessLogSystemRow 钉住没有读取者的那一行怎么打。
//
// actor_kind 为 system 的行没有用户（表上该列可空），也没有理由。表格那一档打
// 一个短横——**不打 0**：0 不是一个合法的用户 ID，打出来会被读成「编号为 0 的
// 用户读了它」；机器可读档里同一件事是 null，让按数字解析它的一方知道这里没有值。
func TestSnapshotAccessLogSystemRow(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	requestID := uniqueName("req-system")
	insertAccessLog(t, db, accessLogFixture{
		requestID: requestID, snapshotID: 7, kind: "system",
		at: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
	})

	line := accessLogLine(t, accessLogOut(t, "--request-id", requestID), requestID)
	// 表格是按显示宽度补空格的，而这一行的用户名为空——按空格切会把空栏丢掉，
	// 于是「用户 ID」那一栏就是第 5 个字段（前四个是日期、时间、request_id、
	// 快照 ID），最后一栏是理由。
	fields := strings.Fields(line)
	if len(fields) < 6 {
		t.Fatalf("那一行字段数不对（%d）：%q", len(fields), line)
	}
	if fields[4] != "-" {
		t.Errorf("没有读取者时用户 ID 那一栏应当是一个短横，得到 %q：%s", fields[4], line)
	}
	if got := fields[len(fields)-1]; got != "-" {
		t.Errorf("没有理由时理由那一栏应当是一个短横，得到 %q：%s", got, line)
	}
	if !strings.Contains(line, "system") {
		t.Errorf("那一行的身份应当是 system：%s", line)
	}

	withFormat(t, formatJSON)
	r := runOnce(t, runSnapshot, []string{"access-log", "--request-id", requestID})
	if r.err != nil {
		t.Fatalf("snapshot access-log -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var doc struct {
		Accesses []struct {
			SnapshotID  int64   `json:"snapshot_id"`
			RequestID   string  `json:"request_id"`
			ActorUserID *int64  `json:"actor_user_id"`
			Username    *string `json:"username"`
			ActorKind   string  `json:"actor_kind"`
			Reason      *string `json:"reason"`
			AccessedAt  string  `json:"accessed_at"`
		} `json:"accesses"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if len(doc.Accesses) != 1 {
		t.Fatalf("应当正好一行，得到 %d 行：\n%s", len(doc.Accesses), r.stdout)
	}
	got := doc.Accesses[0]
	// 三条都是 null（用户 ID、用户名、理由），不是空串也不是 0。
	if got.ActorUserID != nil || got.Username != nil || got.Reason != nil {
		t.Errorf("系统那一行的用户与理由应当是 null：%+v", got)
	}
	if got.ActorKind != "system" || got.SnapshotID != 7 || got.RequestID != requestID {
		t.Errorf("那一行是 %+v，与写进去的对不上", got)
	}
	if got.AccessedAt == "" {
		t.Errorf("时刻那一栏不该是空的：%+v", got)
	}
}

// TestSnapshotAccessLogEmptyAndTruncated 钉住空清单与截断各说一句话。
//
// 空清单直接调打印函数：这一张表是共享的，别的用例也往里写，所以「一条都没有」
// 这个状态在集成档里造不出来。截断那条走命令行——读类命令在 json 档下也能跑，
// 而截断的判据是**打出来的行数**与末尾那句话。
func TestSnapshotAccessLogEmptyAndTruncated(t *testing.T) {
	var buf bytes.Buffer
	printSnapshotAccessLog(&buf, nil, nil, false)
	if !strings.Contains(buf.String(), "这个范围内没有快照访问记录。") {
		t.Errorf("空清单没有说出那句话：\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "时刻") {
		t.Errorf("空清单打出了表头：\n%s", buf.String())
	}

	setupEnv(t)
	db := testDB(t)

	actor, _ := seedSnapshotUser(t, db)
	requestID := uniqueName("req-many")
	for i := 0; i < 3; i++ {
		insertAccessLog(t, db, accessLogFixture{
			requestID: requestID, snapshotID: int64(i + 1),
			actor: sql.NullInt64{Int64: actor, Valid: true}, kind: "owner",
			reason: sql.NullString{String: "挨个看", Valid: true},
			at:     time.Date(2026, 10, 23, 10, i, 0, 0, time.UTC),
		})
	}

	out := accessLogOut(t, "--request-id", requestID, "--limit", "2")
	if got := strings.Count(out, requestID); got != 2 {
		t.Errorf("--limit 2 应当只打两行，打了 %d 行：\n%s", got, out)
	}
	if !strings.Contains(out, "更早的没打出来") {
		t.Errorf("被 --limit 截住时没有说明只打了多少条：\n%s", out)
	}

	// 不限条数时三条都在，那句话也不该出现：每次都补一句「可能还有」与不说一样没用。
	out = accessLogOut(t, "--request-id", requestID, "--limit", "0")
	if got := strings.Count(out, requestID); got != 3 {
		t.Errorf("--limit 0 应当把三条都打出来，打了 %d 行：\n%s", got, out)
	}
	if strings.Contains(out, "更早的没打出来") {
		t.Errorf("没被截住却说了「更早的没打出来」：\n%s", out)
	}

	// 边界那一格：--limit 正好等于这个 request_id 下的行数。这时后面没有别的行了，
	// 而「打出来的条数等于上限」在两件事上长得一样（刚好打完 / 恰好被截住），所以判据
	// 只能是末尾那句话——它读的是**多读一行**的结果（见 snapshot.go 的
	// readSnapshotAccessLog）。这一格写错时，一份已经读完的清单会劝人再去调大 --limit。
	out = accessLogOut(t, "--request-id", requestID, "--limit", "3")
	if got := strings.Count(out, requestID); got != 3 {
		t.Fatalf("--limit 3（正好等于行数）打了 %d 行：\n%s", got, out)
	}
	if strings.Contains(out, "更早的没打出来") {
		t.Errorf("--limit 正好等于行数（后面没有别的行了）却说被截住了：\n%s", out)
	}
}

// TestSnapshotAccessLogJSONShape 钉住机器可读档把筛的条件复述出来。
//
// 一份只有总数的清单说不清这几十条是哪一段、哪一个请求的；而看 JSON 的一方要能
// 不看命令行就知道这次筛的是什么（与其余清单命令同一条）。
func TestSnapshotAccessLogJSONShape(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	actor, username := seedSnapshotUser(t, db)
	requestID := uniqueName("req-json")
	at := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	insertAccessLog(t, db, accessLogFixture{
		requestID: requestID, snapshotID: 11,
		actor: sql.NullInt64{Int64: actor, Valid: true}, kind: "admin",
		reason: sql.NullString{String: "排查账单", Valid: true},
		at:     at,
	})

	from := at.Add(-time.Hour)
	to := at.Add(time.Hour)

	withFormat(t, formatJSON)
	r := runOnce(t, runSnapshot, []string{"access-log", "--request-id", requestID,
		"--user", itoa64(actor),
		"--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339)})
	if r.err != nil {
		t.Fatalf("snapshot access-log -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var doc struct {
		Status  string `json:"status"`
		Summary struct {
			Total     int     `json:"total"`
			Limit     int     `json:"limit"`
			Truncated bool    `json:"truncated"`
			RequestID *string `json:"request_id"`
			UserID    *int64  `json:"user_id"`
			From      string  `json:"from"`
			To        string  `json:"to"`
		} `json:"summary"`
		Accesses []struct {
			ID          int64  `json:"id"`
			SnapshotID  int64  `json:"snapshot_id"`
			RequestID   string `json:"request_id"`
			ActorUserID *int64 `json:"actor_user_id"`
			Username    string `json:"username"`
			ActorKind   string `json:"actor_kind"`
			Reason      string `json:"reason"`
			AccessedAt  string `json:"accessed_at"`
		} `json:"accesses"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if doc.Status != jsonOK || doc.Summary.Total != 1 || len(doc.Accesses) != 1 {
		t.Fatalf("形状不对：status=%q total=%d 行数=%d\n%s",
			doc.Status, doc.Summary.Total, len(doc.Accesses), r.stdout)
	}
	if doc.Summary.RequestID == nil || *doc.Summary.RequestID != requestID {
		t.Errorf("summary 里没有说这次筛的是哪个请求：%s", r.stdout)
	}
	if doc.Summary.UserID == nil || *doc.Summary.UserID != actor {
		t.Errorf("summary 里没有说这次筛的是哪个读取者：%s", r.stdout)
	}
	if doc.Summary.From != from.UTC().Format(time.RFC3339) ||
		doc.Summary.To != to.UTC().Format(time.RFC3339) {
		t.Errorf("summary 里的窗口与传进去的对不上：%s", r.stdout)
	}
	// 默认上限写成字面量，不引 snapshotAccessLogDefaultLimit：引那个常量的话，它哪天
	// 被改小，这一条会跟着一起改，等于没钉住「不给 --limit 时是文档里那个数」。
	if doc.Summary.Limit != 50 || doc.Summary.Truncated {
		t.Errorf("summary 里的 limit / truncated 不对（limit=%d truncated=%v），"+
			"没给 --limit 时上限应当是默认的 50：%s",
			doc.Summary.Limit, doc.Summary.Truncated, r.stdout)
	}
	got := doc.Accesses[0]
	if got.ActorUserID == nil || *got.ActorUserID != actor {
		t.Errorf("读取者那一栏应当是 %d：%+v", actor, got)
	}
	if got.Username != username || got.ActorKind != "admin" || got.Reason != "排查账单" {
		t.Errorf("那一行是 %+v：用户名、身份、理由三栏都要照原文给", got)
	}
	if got.SnapshotID != 11 || got.AccessedAt == "" {
		t.Errorf("快照 ID 或时刻不对：%+v", got)
	}
}

// TestSnapshotAccessLogRejectsBadFlags 钉住参数错在连库之前报出来。
//
// 这一组不连库：判据是参数错（退出码 2），而那要发生在配置加载与连库之前。
func TestSnapshotAccessLogRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"窗口上下颠倒", []string{"access-log", "--from", "2026-11-01", "--to", "2026-10-01"},
			"--from 必须早于 --to"},
		{"时刻看不懂", []string{"access-log", "--to", "明天"}, "--to 的取值"},
		{"条数为负", []string{"access-log", "--limit", "-1"}, "--limit 不能为负"},
		{"多余的位置参数", []string{"access-log", "req-1"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnce(t, runSnapshot, tc.args)
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
