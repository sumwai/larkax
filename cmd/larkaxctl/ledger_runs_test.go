package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger/store"
)

// 本文件是 `ledger runs` 只读入口的用例。
//
// 写入那一侧是 `ledger reconcile`（每轮开一行、收尾时把结果填进同一行）。这一组
// 要回答的是另一半：跑过几次、上次扫到哪、是全量还是增量，读得回来吗。
//
// # 为什么真的跑两轮对账
//
// 一条「库里已经有这几行」的用例证明不了 watermark 对不对：那个值由写入方按
// 「上一轮跑成的那一行的开始时刻」算出来（见 store.reconcileStart），我自己编一个
// 填进去，断言的就只是「我填的那个数读得回来」。所以这里跑真的两轮——一次全量、
// 一次增量——再断言读回来的起点正好是上一轮的开始时刻。

// runReconcileOnce 真跑一轮对账，返回它的报告。
//
// interval 是「两次全量之间最长隔多久」：小于等于零表示每次都全量，给一个大的
// 间隔时，因为上一轮刚刚全量过，这一轮就是增量。两轮之间那条分界线正是不想在
// 用例里假设的东西——所以跑完之后要**断言这一轮确实是那一档**，否则后面几条
// 断言全落在一个与预期不同的前提上。
func runReconcileOnce(t *testing.T, db *sql.DB, interval time.Duration) *store.Report {
	t.Helper()

	report, err := store.New(db).Reconcile(context.Background(), store.ReconcileConfig{
		RepairMode:       store.RepairResyncSnapshot,
		FullScanInterval: interval,
	}, nil)
	if err != nil {
		t.Fatalf("跑一轮对账失败: %v", err)
	}
	if report.FullScan != (interval <= 0) {
		t.Fatalf("这一轮的全量标记是 %v，与给的间隔 %v 对不上", report.FullScan, interval)
	}
	return report
}

// ledgerRunsOut 跑一次 ledger runs 并把 stdout 交回来。
func ledgerRunsOut(t *testing.T, args ...string) string {
	t.Helper()

	r := runOnce(t, runLedger, append([]string{"runs"}, args...))
	if r.err != nil {
		t.Fatalf("ledger runs %v 失败: %v\nstderr:\n%s", args, r.err, r.stderr)
	}
	return r.stdout
}

// runLine 取出第一栏正好是 runID 的那一行。
//
// 按 run_id 找而不是按行号找：这张表只追加，别的用例也在往里写行，而 run_id
// 是它的主键。比**第一栏整栏相等**而不是「这一行里出现过这个数」：时刻与计数
// 里也会出现同样的数字，按子串找会找错行。
func runLine(t *testing.T, out string, runID int64) string {
	t.Helper()

	id := itoa64(runID)
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 13 && fields[0] == id {
			return line
		}
	}
	t.Fatalf("输出里没有 run_id=%d 那一行：\n%s", runID, out)
	return ""
}

// countRunRows 数一张对账运行清单里打了几行数据。
//
// 表头是 11 栏（`run_id`、`开始`、`状态`、…），数据行多出来的两栏是那两个时刻里的
// 空格：`2026-10-19 12:00` 按空白切开是两个字段。按 13 个数数据行，不必解析列宽。
func countRunRows(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if len(strings.Fields(line)) == 13 {
			n++
		}
	}
	return n
}

// TestLedgerRunsListsFullAndIncremental 钉住两轮对账读得回来、且有先后之分。
//
// 判据四处：两条都在；新的在前；范围那一栏与库里那一轮对得上（全量 / 增量）；
// 起点那一栏等于**上一轮的开始时刻**——增量从那里接着扫，读错了这条清单就会
// 让人以为上一轮扫到的位置比实际更早或更晚。
func TestLedgerRunsListsFullAndIncremental(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	full := runReconcileOnce(t, db, 0)
	incr := runReconcileOnce(t, db, 168*time.Hour)

	// 上一轮的开始时刻从库里读，不从报告里取：这一条要断言的正是「库里那一列
	// 与下一轮的起点是一回事」。
	var fullStarted time.Time
	if err := db.QueryRowContext(context.Background(),
		`SELECT started_at FROM reconcile_runs WHERE id = $1`, full.RunID).Scan(&fullStarted); err != nil {
		t.Fatalf("读上一轮的开始时刻失败: %v", err)
	}
	if !incr.Watermark.Equal(fullStarted) {
		t.Fatalf("增量那一轮的起点是 %s，上一轮开始于 %s：增量要接着上一轮的起点扫",
			incr.Watermark, fullStarted)
	}

	out := ledgerRunsOut(t, "--limit", "0")
	fullLine := runLine(t, out, full.RunID)
	incrLine := runLine(t, out, incr.RunID)

	// ① 新的在前。两条都在时才有先后可言，所以这一条要先判存在（上面已经判过）。
	if strings.Index(out, incrLine) > strings.Index(out, fullLine) {
		t.Errorf("清单里增量那一轮排在上一轮全量之后，而它是新的：\n%s", out)
	}

	// ② 范围那一栏照 full_scan 翻，不另编一套词。
	if !strings.Contains(fullLine, "全量") || strings.Contains(fullLine, "增量") {
		t.Errorf("全量那一轮的范围栏不对：%s", fullLine)
	}
	if !strings.Contains(incrLine, "增量") || strings.Contains(incrLine, "全量") {
		t.Errorf("增量那一轮的范围栏不对：%s", incrLine)
	}

	// ③ 起点那一栏就是上一轮的开始时刻，按带年份的写法打。布局在这里写成字面量而
	// 不引 longStampLayout：引那个常量的话，它哪天被改成不带年份，这一条会跟着一起
	// 改，等于没钉住「要带年份」。
	if want := fullStarted.Format("2006-01-02 15:04"); !strings.Contains(incrLine, want) {
		t.Errorf("增量那一轮的起点栏里没有 %s：%s", want, incrLine)
	}

	// ④ 状态是那一轮对账的状态（跑完了是 done），而整份输出本身也是 ok。
	if !strings.Contains(incrLine, "done") {
		t.Errorf("跑完的那一轮状态应当是 done：%s", incrLine)
	}
}

// TestLedgerRunsJSONShape 钉住机器可读档那几栏。
//
// 最要紧的是 full_scan 与 watermark 两栏：前者是布尔而不是「全量 / 增量」那两个字，
// 后者要能按时刻解析。finished_at 与 error 在跑完且没出错的那一轮上是 null——
// 不是空串，也不是零值时刻（零值时刻会打成一个看起来正常的日期）。
func TestLedgerRunsJSONShape(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	full := runReconcileOnce(t, db, 0)

	// 这一轮还开着（status='running'）的行：finished_at 与 error 都该是 null。
	// 直接写一行而不是让一轮对账停在中间——造那个状态要在库上挂触发器，
	// 而这里要判的是**读回来的那一栏**。
	var runningID int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO reconcile_runs (started_at, watermark, full_scan, status)
		VALUES (now() + interval '1 hour', now(), false, 'running')
		RETURNING id`).Scan(&runningID); err != nil {
		t.Fatalf("写一行未收尾的运行记录失败: %v", err)
	}

	withFormat(t, formatJSON)
	r := runOnce(t, runLedger, []string{"runs", "--limit", "0"})
	if r.err != nil {
		t.Fatalf("ledger runs -o json 失败: %v\nstderr:\n%s", r.err, r.stderr)
	}
	assertOnlyJSON(t, r.stdout)

	var doc struct {
		Status  string `json:"status"`
		Summary struct {
			Total     int  `json:"total"`
			Limit     int  `json:"limit"`
			Truncated bool `json:"truncated"`
		} `json:"summary"`
		Runs []struct {
			ID         int64   `json:"id"`
			StartedAt  string  `json:"started_at"`
			FinishedAt *string `json:"finished_at"`
			Watermark  string  `json:"watermark"`
			FullScan   bool    `json:"full_scan"`
			Checked    int     `json:"checked_wallets"`
			Status     string  `json:"status"`
			Error      *string `json:"error"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if doc.Status != jsonOK {
		t.Fatalf("最外层的状态是 %q，想要 %q：\n%s", doc.Status, jsonOK, r.stdout)
	}

	// --limit 0 不限条数：打出来的行数就是表里的行数。
	var total int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM reconcile_runs`).Scan(&total); err != nil {
		t.Fatalf("数运行记录失败: %v", err)
	}
	if doc.Summary.Total != total || len(doc.Runs) != total || doc.Summary.Truncated {
		t.Errorf("--limit 0 应当把 %d 条都打出来：total=%d 行数=%d truncated=%v",
			total, doc.Summary.Total, len(doc.Runs), doc.Summary.Truncated)
	}
	if doc.Summary.Limit != 0 {
		t.Errorf("summary.limit 应当是这一次用的上限，得到 %d", doc.Summary.Limit)
	}

	byID := make(map[int64]int, len(doc.Runs))
	for i, run := range doc.Runs {
		byID[run.ID] = i
	}

	fullIdx, ok := byID[full.RunID]
	if !ok {
		t.Fatalf("JSON 里没有全量那一轮（run_id=%d）：\n%s", full.RunID, r.stdout)
	}
	got := doc.Runs[fullIdx]
	if !got.FullScan || got.Status != "done" {
		t.Errorf("全量那一轮是 %+v，full_scan 应当是 true、status 应当是 done", got)
	}
	if got.Error != nil {
		t.Errorf("跑成的那一轮 error 应当是 null，得到 %q", *got.Error)
	}
	// 起点按时刻解析回来比对，不按字符串比：字符串的形状是 jsonTime 的事，
	// 这一条要钉的是「读完还是同一个时刻」。
	watermark, err := time.Parse(time.RFC3339Nano, got.Watermark)
	if err != nil {
		t.Fatalf("watermark 不是能解析的时刻：%q", got.Watermark)
	}
	if !watermark.Equal(full.Watermark) {
		t.Errorf("watermark = %s，库里那一轮是 %s", watermark, full.Watermark)
	}
	checkedFromDB := 0
	if err := db.QueryRowContext(context.Background(),
		`SELECT checked_wallets FROM reconcile_runs WHERE id = $1`, full.RunID).
		Scan(&checkedFromDB); err != nil {
		t.Fatalf("读比对行数失败: %v", err)
	}
	if got.Checked != checkedFromDB {
		t.Errorf("checked_wallets = %d，库里是 %d", got.Checked, checkedFromDB)
	}

	runningIdx, ok := byID[runningID]
	if !ok {
		t.Fatalf("JSON 里没有未收尾那一行（run_id=%d）：\n%s", runningID, r.stdout)
	}
	running := doc.Runs[runningIdx]
	if running.FinishedAt != nil {
		t.Errorf("没跑完的那一轮 finished_at 应当是 null，得到 %q", *running.FinishedAt)
	}
	if running.Status != "running" {
		t.Errorf("没跑完的那一轮状态是 %q，应当是 running", running.Status)
	}
	if running.FullScan {
		t.Errorf("这一行写的是增量，full_scan 却是 true：%+v", running)
	}
}

// TestLedgerRunsEmpty 钉住一条记录都没有时也说一句话。
//
// 直接调打印函数：这张表是共享的，别的用例也在往里写行，所以「一条都没有」
// 这个状态在集成档里造不出来。一行都不打时，「一轮对账都没跑过」与「命令没跑成」
// 在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
func TestLedgerRunsEmpty(t *testing.T) {
	var buf bytes.Buffer
	printLedgerRuns(&buf, nil, false)

	if !strings.Contains(buf.String(), "还没有跑过对账，一条运行记录都没有。") {
		t.Errorf("空清单没有说出那句话：\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "run_id") {
		t.Errorf("空清单打出了表头：\n%s", buf.String())
	}
}

// TestLedgerRunsSaysWhenTruncated 钉住被 --limit 截住时清单自己说出来。
//
// 表只追加，一份截过的清单与一份完整的清单在屏幕上长得一样，而看的人会照第一份
// 下结论（见 LESSONS.md 第 16 条）。这一条自己造两行，不靠别的用例留下的记录。
func TestLedgerRunsSaysWhenTruncated(t *testing.T) {
	setupEnv(t)
	db := testDB(t)

	for i := 0; i < 2; i++ {
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO reconcile_runs (started_at, watermark, full_scan, status, finished_at)
			VALUES ($1, $1, true, 'done', $1)`,
			time.Date(2026, 9, 1+i, 10, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("写运行记录失败: %v", err)
		}
	}

	out := ledgerRunsOut(t, "--limit", "1")
	if got := countRunRows(out); got != 1 {
		t.Errorf("--limit 1 应当只打一行，打了 %d 行：\n%s", got, out)
	}
	if !strings.Contains(out, "更早的没打出来") {
		t.Errorf("被 --limit 截住时没有说明只打了多少条：\n%s", out)
	}

	// 不限条数时那两行都在，那句话也不该出现。
	out = ledgerRunsOut(t, "--limit", "0")
	if got := countRunRows(out); got < 2 {
		t.Errorf("--limit 0 应当把两行都打出来，打了 %d 行：\n%s", got, out)
	}
	if strings.Contains(out, "更早的没打出来") {
		t.Errorf("没被截住却说了「更早的没打出来」：\n%s", out)
	}

	// 边界那一格：--limit 正好等于表里的行数。这时后面没有别的行了，而「打出来的
	// 条数等于上限」在两件事上长得一样（刚好打完 / 恰好被截住），所以判据只能是末尾
	// 那句话——它读的是**多读一行**的结果（见 store 的 ReconcileRuns）。这一格写错
	// 时，一份已经读完的清单会劝人再去调大 --limit。行数当场从库里数：这张表是共享
	// 的，写死一个数就会随别的用例写下的行一起失效。
	var total int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM reconcile_runs`).Scan(&total); err != nil {
		t.Fatalf("数运行记录失败: %v", err)
	}
	out = ledgerRunsOut(t, "--limit", itoa64(int64(total)))
	if got := countRunRows(out); got != total {
		t.Fatalf("--limit %d（正好等于表里的行数）打了 %d 行：\n%s", total, got, out)
	}
	if strings.Contains(out, "更早的没打出来") {
		t.Errorf("--limit 正好等于行数（后面没有别的行了）却说被截住了：\n%s", out)
	}
}

// TestLedgerRunsRejectsBadFlags 钉住参数错在连库之前报出来。
func TestLedgerRunsRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"条数为负", []string{"--limit", "-1"}, "--limit 不能为负"},
		{"多余的位置参数", []string{"3"}, "不接受位置参数"},
		{"不认识的开关", []string{"--since", "2026-10-01"}, "flag provided but not defined"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnce(t, runLedger, append([]string{"runs"}, tc.args...))
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
