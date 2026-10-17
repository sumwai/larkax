package channels

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// 这一层测的是写入侧的 SQL：桶怎么累加、连续失败怎么动、额度怎么扣。
// 纯映射与口径在 record_test.go 里用构造出来的值测，不需要数据库。

// testStaleAfter 是这些用例喂给 Recorder 的陈旧阈值。
//
// 写成**字面量**而不是拿 quota.SelfAccountingStaleAfter 算一个：后者是被测
// 生产代码，让它给自己出题，把倍数改成 1 也照样通过。15 分钟与出厂配置一致
// （探测间隔 5 分钟 × 3 倍）。要分辨「阈值跟着配置走」的那条用例自己带两个
// 不同的值。
const testStaleAfter = 15 * time.Minute

// outcome 造一条调用结局。
func outcome(channelID int64, at time.Time, verdict route.Verdict) route.CallOutcome {
	return route.CallOutcome{ChannelID: channelID, At: at, Verdict: verdict}
}

// readStreak 读回连续失败计数与它的时间戳。
func readStreak(t *testing.T, db *sql.DB, channelID int64) (int, sql.NullTime) {
	t.Helper()
	var (
		count int
		at    sql.NullTime
	)
	err := db.QueryRowContext(context.Background(),
		`SELECT consecutive_failures, last_failure_at FROM channels WHERE id = $1`,
		channelID).Scan(&count, &at)
	if err != nil {
		t.Fatalf("读连续失败失败: %v", err)
	}
	return count, at
}

// readRollup 读回某个小时桶的三个计数。
func readRollup(t *testing.T, db *sql.DB, channelID int64, bucket time.Time) (requests, successes, failures int) {
	t.Helper()
	err := db.QueryRowContext(context.Background(), `
		SELECT requests, successes, failures FROM channel_health_rollup
		 WHERE channel_id = $1 AND bucket = $2`,
		channelID, bucket).Scan(&requests, &successes, &failures)
	if err != nil {
		t.Fatalf("读健康度桶失败: %v", err)
	}
	return requests, successes, failures
}

// ── 连续失败 ────────────────────────────────────────────────────────────

// 失败累加、成功清零，时间戳跟着走。
//
// 这一列是「连续失败 N 次就别再试」唯一的来源：它恒为 0 时那条保护
// 从不触发，而一个正在连续失败的渠道还会拿满分健康分。
func TestRecordCallMaintainsStreak(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()

	for i := 1; i <= 3; i++ {
		if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictFailure)); err != nil {
			t.Fatalf("第 %d 次记录失败: %v", i, err)
		}
		count, lastAt := readStreak(t, db, id)
		if count != i {
			t.Fatalf("第 %d 次失败之后 streak = %d，期望 %d", i, count, i)
		}
		if !lastAt.Valid {
			t.Fatal("失败之后 last_failure_at 不该为 NULL")
		}
	}

	if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictSuccess)); err != nil {
		t.Fatalf("记录成功失败: %v", err)
	}
	count, lastAt := readStreak(t, db, id)
	if count != 0 {
		t.Errorf("成功之后 streak = %d，期望 0", count)
	}
	if lastAt.Valid {
		t.Errorf("成功之后 last_failure_at 该清成 NULL，实际 %v", lastAt.Time)
	}
}

// 写进去的计数读得出来，而且足以触发 route 的摘除判据。
//
// 这条把写入侧与读取侧接起来测：两边的列名或类型对不上时，单看任何一侧
// 都是绿的。
func TestRecordCallStreakIsVisibleToCandidates(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})
	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()

	for i := 0; i < 3; i++ {
		if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictFailure)); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Health.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures = %d，期望 3", got[0].Health.ConsecutiveFailures)
	}
	if reason := got[0].DrainReason(); reason == "" {
		t.Error("连续失败 3 次的渠道应被判为暂时不该使用")
	}
}

// 冷却期一过，读取侧把计数当作 0——否则被摘掉的渠道没有流量，
// 计数永远不会降回去（见 streakCooldown）。
func TestRecordCallStreakExpiresForCandidates(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})
	r := NewRecorder(db, testStaleAfter)

	// 失败发生在 16 分钟前：冷却期（15 分钟）已经过了。
	old := time.Now().UTC().Add(-16 * time.Minute)
	for i := 0; i < 3; i++ {
		if err := r.RecordCall(context.Background(), outcome(id, old, route.VerdictFailure)); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}

	// 库里仍是 3：写入侧不因为时间流逝改数，判断在读取侧做。
	if count, _ := readStreak(t, db, id); count != 3 {
		t.Fatalf("streak = %d，期望 3（写入侧不做冷却判断）", count)
	}

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Health.ConsecutiveFailures != 0 {
		t.Errorf("冷却期已过时 ConsecutiveFailures = %d，期望 0（放它进来试一次）",
			got[0].Health.ConsecutiveFailures)
	}
	if reason := got[0].DrainReason(); reason != "" {
		t.Errorf("冷却期已过不该被摘除：%s", reason)
	}
}

// 没有渠道这一行时什么都不写、也不报错：调用进行到一半时管理面删了它，
// 那不是故障。硬写的话 rollup 会撞外键。
func TestRecordCallMissingChannelIsNoop(t *testing.T) {
	db := testDB(t)
	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()

	if err := r.RecordCall(context.Background(), outcome(999999999, at, route.VerdictSuccess)); err != nil {
		t.Fatalf("渠道不存在时不该报错: %v", err)
	}

	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channel_health_rollup WHERE channel_id = $1`, 999999999).Scan(&n)
	if err != nil {
		t.Fatalf("查健康度失败: %v", err)
	}
	if n != 0 {
		t.Errorf("健康度行数 = %d，期望 0", n)
	}
}

// 未知结论不写任何东西，并且报错——静默当成「什么都不改」的话，
// 保护失效与「这个渠道没有失败」在库里长得一样。
func TestRecordCallRejectsUnknownVerdict(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()

	err := r.RecordCall(context.Background(), outcome(id, at, route.Verdict("bogus")))
	if err == nil {
		t.Fatal("未知结论该报错")
	}

	if count, _ := readStreak(t, db, id); count != 0 {
		t.Errorf("streak = %d，期望 0（一条都不该写）", count)
	}
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channel_health_rollup WHERE channel_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("查健康度失败: %v", err)
	}
	if n != 0 {
		t.Errorf("健康度行数 = %d，期望 0", n)
	}
}

// ── 健康度桶 ────────────────────────────────────────────────────────────

// 同一个小时里的多次调用累加到同一行。
func TestRecordCallAccumulatesRollupBucket(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()
	bucket := at.Truncate(time.Hour)

	calls := []route.Verdict{
		route.VerdictSuccess,
		route.VerdictSuccess,
		route.VerdictFailure,
		route.VerdictUnattributed,
	}
	for _, v := range calls {
		if err := r.RecordCall(context.Background(), outcome(id, at, v)); err != nil {
			t.Fatalf("记录 %s 失败: %v", v, err)
		}
	}

	requests, successes, failures := readRollup(t, db, id, bucket)
	if requests != 4 {
		t.Errorf("requests = %d，期望 4（每一次调用都算一次请求）", requests)
	}
	if successes != 2 {
		t.Errorf("successes = %d，期望 2", successes)
	}
	if failures != 1 {
		t.Errorf("failures = %d，期望 1（「不是渠道的失败」那一类不计入）", failures)
	}

	// 同一个渠道再记一次，仍然是同一行。
	if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictSuccess)); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if requests, _, _ = readRollup(t, db, id, bucket); requests != 5 {
		t.Errorf("累加之后 requests = %d，期望 5", requests)
	}
}

// 「不是渠道的失败」照常算一次请求，但不加连续失败、也不清零。
func TestRecordCallUnattributedDoesNotTouchStreak(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()

	if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictFailure)); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if count, _ := readStreak(t, db, id); count != 1 {
		t.Fatalf("streak = %d，期望 1", count)
	}

	// 客户端断开：那一串失败与它无关，不该被它清零。
	if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictUnattributed)); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if count, _ := readStreak(t, db, id); count != 1 {
		t.Errorf("streak = %d，期望 1（不记在渠道头上的调用不打断已有的连续失败）", count)
	}
}

// 跨小时的两次调用落在两个桶里。
func TestRecordCallBucketsByHour(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	r := NewRecorder(db, testStaleAfter)

	// 两个小时之前与现在。用整点减去一小时，避开「正好卡在整点」的边界。
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	older := now.Add(-2 * time.Hour)

	for _, at := range []time.Time{older, now} {
		if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictSuccess)); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}

	if requests, _, _ := readRollup(t, db, id, older.Truncate(time.Hour)); requests != 1 {
		t.Errorf("两小时前的桶 requests = %d，期望 1", requests)
	}
	if requests, _, _ := readRollup(t, db, id, now.Truncate(time.Hour)); requests != 1 {
		t.Errorf("当前桶 requests = %d，期望 1", requests)
	}
}

// ── 额度自记账 ──────────────────────────────────────────────────────────

// quotaSnapshot 是自记账会动的那几列。
//
// numeric 一律解析成 float 再断言：驱动把 numeric 交回来时用什么写法
// （850 还是 850.00000000）是它的实现细节，钉住那个字符串等于测驱动。
type quotaSnapshot struct {
	remaining  float64
	hasRemain  bool
	unit       string
	probeKind  string
	confidence float64
	health     string
}

func readQuota(t *testing.T, db *sql.DB, channelID int64) quotaSnapshot {
	t.Helper()

	var (
		remaining  sql.NullString
		unit       sql.NullString
		confidence string
		snap       quotaSnapshot
	)
	err := db.QueryRowContext(context.Background(), `
		SELECT remaining_estimate, unit, probe_kind, confidence, health
		  FROM channel_quota_state WHERE channel_id = $1`,
		channelID).Scan(&remaining, &unit, &snap.probeKind, &confidence, &snap.health)
	if err != nil {
		t.Fatalf("读额度状态失败: %v", err)
	}
	if snap.hasRemain = remaining.Valid; remaining.Valid {
		snap.remaining = parseNumeric(t, "remaining_estimate", remaining.String)
	}
	snap.unit = unit.String
	snap.confidence = parseNumeric(t, "confidence", confidence)
	return snap
}

func parseNumeric(t *testing.T, what, raw string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("%s 不是数字（%q）: %v", what, raw, err)
	}
	return v
}

func quotaRowCount(t *testing.T, db *sql.DB, channelID int64) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM channel_quota_state WHERE channel_id = $1`, channelID).Scan(&n)
	if err != nil {
		t.Fatalf("查额度状态失败: %v", err)
	}
	return n
}

// 有基准（remaining 有值）且单位是 token 时，按上游给的用量扣减。
//
// 用的是 quota.Project 与 quota.Merge，所以「实测值之后自记账仍然要扣减」
// 这条规则由那一侧保证，这里测的是它真的被接上了、并且真的写回了库里。
func TestRecordCallSelfAccountsAgainstBaseline(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	insertQuota(t, db, id, "1000", "tokens", "api_query", "healthy", 1)

	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()
	call := route.CallOutcome{
		ChannelID: id,
		At:        at,
		Verdict:   route.VerdictSuccess,
		Usage:     channel.Usage{PromptTokens: 100, CompletionTokens: 50},
	}
	if err := r.RecordCall(context.Background(), call); err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	snap := readQuota(t, db, id)
	if !snap.hasRemain || snap.remaining != 850 {
		t.Errorf("remaining = %v（有值 %v），期望 850（1000 - 150）", snap.remaining, snap.hasRemain)
	}
	if snap.unit != "tokens" {
		t.Errorf("unit = %q，期望 tokens", snap.unit)
	}
	if snap.probeKind != "self_accounting" {
		t.Errorf("probe_kind = %q，期望 self_accounting（最近一次观测的方式变了）", snap.probeKind)
	}
	// 刚实测过，自记账的可信度取基线。
	if snap.confidence != 0.8 {
		t.Errorf("confidence = %v，期望 0.8", snap.confidence)
	}
	// health 来自别的来源，自记账不碰它。
	if snap.health != "healthy" {
		t.Errorf("health = %q，期望 healthy（自记账不写这一列）", snap.health)
	}

	// 再扣一次：自记账是一次推进，不是一次测量。
	call.Usage = channel.Usage{PromptTokens: 200}
	if err := r.RecordCall(context.Background(), call); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if snap = readQuota(t, db, id); snap.remaining != 650 {
		t.Errorf("第二次之后 remaining = %v，期望 650（850 - 200）", snap.remaining)
	}
}

// 陈旧阈值跟着配置走：同一个夹具，阈值小的时候可信度已经衰减，
// 阈值大的时候还是基线。
//
// 这一条钉的是「装配层读出来的那个值真的走到了自记账上」。阈值写死时，
// 两种配置给出的是同一个数，而两次都算「通过」——那正是这个键原先的状态：
// 改 settings 里的 quota.probe_interval_seconds，写回库里的 confidence
// 一动不动，而读代码的人会以为它跟着走。
//
// 夹具里 last_probe_at 是 10 分钟前，于是 15 分钟的阈值之内、3 分钟的阈值
// 之外：两个断言分别是「恰好等于基线」与「已经低于基线」，两者都写死字面量。
func TestRecordCallStaleThresholdComesFromConfig(t *testing.T) {
	const (
		baseConfidence = 0.8 // quota 的自记账基线
		probeAgo       = 10 * time.Minute
	)

	// insertQuota 不写 last_probe_at（NULL 时不衰减，测不出差别），
	// 所以这一条自己插一行带探测时刻的。
	insertProbedQuota := func(t *testing.T, db *sql.DB, channelID int64, ago time.Duration) {
		t.Helper()
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO channel_quota_state
			       (channel_id, remaining_estimate, unit, probe_kind, health, last_probe_at)
			VALUES ($1, 1000, 'tokens', 'api_query', 'healthy', now() - $2::interval)`,
			channelID, ago.String())
		if err != nil {
			t.Fatalf("插入额度状态失败: %v", err)
		}
	}

	cases := []struct {
		name       string
		staleAfter time.Duration
		want       float64
	}{
		{"阈值之内取基线", testStaleAfter, baseConfidence},
		{"阈值之外开始衰减", 3 * time.Minute, baseConfidence - 0.01},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			sealer := testSealer(t)
			id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
			insertProbedQuota(t, db, id, probeAgo)

			r := NewRecorder(db, tc.staleAfter)
			call := route.CallOutcome{
				ChannelID: id,
				At:        time.Now().UTC(),
				Verdict:   route.VerdictSuccess,
				Usage:     channel.Usage{PromptTokens: 100},
			}
			if err := r.RecordCall(context.Background(), call); err != nil {
				t.Fatalf("记录失败: %v", err)
			}

			snap := readQuota(t, db, id)
			switch {
			case tc.want == baseConfidence && snap.confidence != baseConfidence:
				t.Errorf("阈值 %v（探测在 %v 前）的 confidence = %v，期望基线 %v",
					tc.staleAfter, probeAgo, snap.confidence, baseConfidence)
			case tc.want < baseConfidence && snap.confidence >= baseConfidence:
				t.Errorf("阈值 %v（探测在 %v 前）的 confidence = %v，"+
					"期望低于基线 %v——阈值没有跟着配置走",
					tc.staleAfter, probeAgo, snap.confidence, baseConfidence)
			}
		})
	}
}

// 扣到见底时记 0，不是负数——负余量在路由与界面上都没有意义。
func TestRecordCallSelfAccountingClampsAtZero(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	insertQuota(t, db, id, "100", "tokens", "api_query", "healthy", 1)

	r := NewRecorder(db, testStaleAfter)
	err := r.RecordCall(context.Background(), route.CallOutcome{
		ChannelID: id,
		At:        time.Now().UTC(),
		Verdict:   route.VerdictSuccess,
		Usage:     channel.Usage{PromptTokens: 500},
	})
	if err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	snap := readQuota(t, db, id)
	if !snap.hasRemain || snap.remaining != 0 {
		t.Errorf("remaining = %v，期望 0（消耗超过记录余量时记 0，不记负数）", snap.remaining)
	}
}

// 没有基准时不写：自记账是一次推进，基准未知时推不出任何数，
// 而写一行只有 NULL 的额度状态改变不了读侧（Known 仍是 false）。
func TestRecordCallSelfAccountingNeedsBaseline(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})

	r := NewRecorder(db, testStaleAfter)
	call := route.CallOutcome{
		ChannelID: id,
		At:        time.Now().UTC(),
		Verdict:   route.VerdictSuccess,
		Usage:     channel.Usage{PromptTokens: 100},
	}
	if err := r.RecordCall(context.Background(), call); err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	if n := quotaRowCount(t, db, id); n != 0 {
		t.Errorf("额度状态行数 = %d，期望 0（没有基准，什么都不该写）", n)
	}

	// 有行、单位也对，但余量是 NULL（查不到额度）：同样推不出来。
	//
	// 单位故意给成 tokens：给成别的单位的话，挡住这次写入的是**单位**
	// 那道判断，这条用例就测不到「没有基准」这一条了。
	insertQuota(t, db, id, nil, "tokens", "self_accounting", "unknown", 0.2)
	if err := r.RecordCall(context.Background(), call); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	snap := readQuota(t, db, id)
	if snap.hasRemain {
		t.Errorf("remaining = %v，期望仍是 NULL", snap.remaining)
	}
	if snap.probeKind != "self_accounting" {
		t.Errorf("probe_kind = %q，期望不动它", snap.probeKind)
	}
}

// 单位对不上就不扣：把 token 数当成元数扣下去是在**改错一个数**，
// 而它看起来完全正常。
func TestRecordCallSelfAccountingSkipsForeignUnit(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	r := NewRecorder(db, testStaleAfter)

	for _, tc := range []struct {
		name string
		unit any
	}{
		{"单位是按元计", "money"},
		{"单位未知", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
			insertQuota(t, db, id, "100", tc.unit, "api_query", "healthy", 1)

			err := r.RecordCall(context.Background(), route.CallOutcome{
				ChannelID: id,
				At:        time.Now().UTC(),
				Verdict:   route.VerdictSuccess,
				Usage:     channel.Usage{PromptTokens: 100},
			})
			if err != nil {
				t.Fatalf("记录失败: %v", err)
			}

			snap := readQuota(t, db, id)
			if !snap.hasRemain || snap.remaining != 100 {
				t.Errorf("remaining = %v，期望不动（单位对不上）", snap.remaining)
			}
			if snap.probeKind != "api_query" {
				t.Errorf("probe_kind = %q，期望不动它", snap.probeKind)
			}
		})
	}
}

// 失败的调用没有用量，不推进自记账；客户端断开那种带用量的照扣。
func TestRecordCallSelfAccountingOnlyWithUsage(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	insertQuota(t, db, id, "1000", "tokens", "api_query", "healthy", 1)

	r := NewRecorder(db, testStaleAfter)
	at := time.Now().UTC()

	// 失败：上游没给回执，用量未知，扣零等于没扣。
	if err := r.RecordCall(context.Background(), outcome(id, at, route.VerdictFailure)); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if snap := readQuota(t, db, id); snap.remaining != 1000 {
		t.Errorf("失败的调用之后 remaining = %v，期望不动", snap.remaining)
	}

	// 客户端断开：不是渠道的失败，但上游已经为输入计了费。
	if err := r.RecordCall(context.Background(), route.CallOutcome{
		ChannelID: id,
		At:        at,
		Verdict:   route.VerdictUnattributed,
		Usage:     channel.Usage{PromptTokens: 250},
	}); err != nil {
		t.Fatalf("记录失败: %v", err)
	}
	if snap := readQuota(t, db, id); snap.remaining != 750 {
		t.Errorf("客户端断开之后 remaining = %v，期望 750", snap.remaining)
	}
}

// Validate 接在落库**之前**：观测时间落在将来的那次写入被拒绝，而且库里
// 一个数都没动。
//
// 这条用例同时断言「报错」与「没写」——只断言其中一个的话，把 Validate
// 那一句删掉之后它照样通过（写入成功、值变了，但没人看错误），
// 而「观测被拒绝」与「观测悄悄丢了」在库上长得一样。
func TestRecordCallRejectsFutureObservation(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	id := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	insertQuota(t, db, id, "1000", "tokens", "api_query", "healthy", 1)

	r := NewRecorder(db, testStaleAfter)
	err := r.RecordCall(context.Background(), route.CallOutcome{
		ChannelID: id,
		// 超过 Validate 的一分钟容差。
		At:      time.Now().UTC().Add(2 * time.Hour),
		Verdict: route.VerdictSuccess,
		Usage:   channel.Usage{PromptTokens: 100},
	})
	if err == nil {
		t.Fatal("观测时间在将来的写入应被拒绝")
	}
	if !strings.Contains(err.Error(), "自记账观测不自洽") {
		t.Errorf("错误应说明是观测不自洽，实际: %v", err)
	}
	if snap := readQuota(t, db, id); snap.remaining != 1000 {
		t.Errorf("被拒绝之后 remaining = %v，期望不动（1000）", snap.remaining)
	}
}
