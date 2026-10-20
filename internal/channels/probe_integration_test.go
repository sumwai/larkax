package channels

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/route"
)

// 这一层测的是额度探测的写入侧：什么时候触发、同一渠道怎么单飞、三种结果
// （拿到值 / 查不到 / 出错）各自落到哪几列。用真库，因为它们要证明的正是
// 「写进去的值与时刻读出来是什么」；适配器一律是假的，不接真实上游。

// fakeQuotaAdapter 是一个可控的额度查询适配器。
//
// started / release 用来把一次探测卡在半路，从而能确定地观察单飞：第一个
// 调用卡住时，其余调用要是也进来了，calls 就会大于 1。
type fakeQuotaAdapter struct {
	mu      sync.Mutex
	calls   []int64
	state   quota.State
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *fakeQuotaAdapter) Quota(ctx context.Context, ch channel.Channel) (quota.State, error) {
	f.mu.Lock()
	f.calls = append(f.calls, ch.ID)
	f.mu.Unlock()

	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return quota.State{}, ctx.Err()
		}
	}
	return f.state, f.err
}

func (f *fakeQuotaAdapter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// timeoutAdapter 一直等到探测的 ctx 结束才返回，把一次探测放在
// 「查询恰好超时」那个边界上。
type timeoutAdapter struct{}

func (timeoutAdapter) Quota(ctx context.Context, _ channel.Channel) (quota.State, error) {
	<-ctx.Done()
	return quota.State{}, ctx.Err()
}

// probeRow 是探测写入的那几列。
type probeRow struct {
	remaining   sql.NullString
	unit        sql.NullString
	confidence  sql.NullString
	probeKind   string
	health      sql.NullString
	resetsAt    sql.NullTime
	lastProbeAt sql.NullTime
	attemptAt   sql.NullTime
	status      sql.NullString
}

func readProbeRow(t *testing.T, db *sql.DB, channelID int64) probeRow {
	t.Helper()
	var row probeRow
	err := db.QueryRowContext(context.Background(), `
		SELECT remaining_estimate, unit, confidence, probe_kind, health,
		       resets_at, last_probe_at, last_probe_attempt_at, last_probe_status
		  FROM channel_quota_state WHERE channel_id = $1`, channelID).Scan(
		&row.remaining, &row.unit, &row.confidence, &row.probeKind, &row.health,
		&row.resetsAt, &row.lastProbeAt, &row.attemptAt, &row.status)
	if err != nil {
		t.Fatalf("读额度状态失败: %v", err)
	}
	return row
}

func floatPtr(v float64) *float64 { return &v }

// assertNear 比较两个时刻，容忍数据库的微秒精度。
func assertNear(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if d := got.Sub(want); d < -time.Second || d > time.Second {
		t.Errorf("%s = %v，期望 %v（容差 1s）", what, got, want)
	}
}

// 过期之后触发一次探测，表里的值与两个时刻都更新；TTL 内再请求不再探。
func TestQuotaProbeRefreshesStaleValue(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	interval := 5 * time.Minute
	now := time.Now().UTC()
	stale := now.Add(-2 * interval)
	resets := now.Add(24 * time.Hour)
	insertQuota(t, db, id, "100", "tokens", "api_query", "healthy", 1)
	if _, err := db.ExecContext(t.Context(), `
		UPDATE channel_quota_state SET last_probe_at = $2, last_probe_attempt_at = $2
		 WHERE channel_id = $1`, id, stale); err != nil {
		t.Fatalf("摆旧时刻失败: %v", err)
	}

	adapter := &fakeQuotaAdapter{state: quota.State{
		Remaining: floatPtr(250),
		Unit:      "tokens",
		ResetsAt:  &resets,
	}}
	p, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}
	p.Now = func() time.Time { return now }

	cand := route.Candidate{
		Channel: channel.Channel{ID: id, Type: "openai-compatible"},
		Quota:   quota.State{LastProbeAttemptAt: &stale},
	}
	p.ProbeIfStale(cand)
	p.wg.Wait()

	if got := adapter.count(); got != 1 {
		t.Fatalf("过期后适配器被调用了 %d 次，期望 1", got)
	}
	row := readProbeRow(t, db, id)
	if !row.remaining.Valid || parseNumeric(t, "remaining_estimate", row.remaining.String) != 250 {
		t.Errorf("remaining_estimate = %v，期望 250", row.remaining)
	}
	if row.unit.String != "tokens" {
		t.Errorf("unit = %q，期望 tokens", row.unit.String)
	}
	if row.probeKind != "api_query" {
		t.Errorf("probe_kind = %q，期望 api_query", row.probeKind)
	}
	if got := parseNumeric(t, "confidence", row.confidence.String); got != 1 {
		t.Errorf("confidence = %v，期望 1", got)
	}
	if !row.lastProbeAt.Valid {
		t.Fatal("last_probe_at 仍是 NULL，实测值没有带时刻")
	}
	assertNear(t, "last_probe_at", row.lastProbeAt.Time, now)
	if !row.attemptAt.Valid {
		t.Fatal("last_probe_attempt_at 仍是 NULL")
	}
	assertNear(t, "last_probe_attempt_at", row.attemptAt.Time, now)
	if row.status.String != probeStatusOK {
		t.Errorf("last_probe_status = %q，期望 %q", row.status.String, probeStatusOK)
	}
	// 重置时刻是 scoreQuota 的紧迫度输入：探测拿回来却没写进去的话，
	// 那一半永远停在「不知道」。
	if !row.resetsAt.Valid {
		t.Fatal("resets_at 没有被写回")
	}
	assertNear(t, "resets_at", row.resetsAt.Time, resets)
	// health 来自别的来源，探测不该动它。
	if row.health.String != "healthy" {
		t.Errorf("health = %q，期望 healthy（探测不碰这一列）", row.health.String)
	}

	// TTL 内：同一个候选（快照里还是旧时刻）再触发一次，适配器不该再被调用。
	p.ProbeIfStale(cand)
	p.wg.Wait()
	if got := adapter.count(); got != 1 {
		t.Errorf("TTL 内又探了一次，适配器共被调用 %d 次，期望 1", got)
	}

	// 换一个进程内状态为空的探测者，喂给它从库里读回的候选快照：TTL 不能只靠
	// 本进程的内存，否则每次重启都会把所有渠道重探一遍。
	src := New(db, sealer)
	cands, err := src.Candidates(t.Context(), model)
	if err != nil {
		t.Fatalf("取候选失败: %v", err)
	}
	if len(cands) != 1 || cands[0].Quota.LastProbeAttemptAt == nil {
		t.Fatalf("读回的候选没有带上尝试时刻: %+v", cands)
	}
	fresh, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造第二个探测者失败: %v", err)
	}
	fresh.Now = func() time.Time { return now }
	fresh.ProbeIfStale(cands[0])
	fresh.wg.Wait()
	if got := adapter.count(); got != 1 {
		t.Errorf("新探测者没有按库里的尝试时刻跳过，适配器共被调用 %d 次", got)
	}
}

// 适配器这次没带回重置时刻时，库里已有的值要保留——「这次没提」不等于
// 「确认不会重置」。这是写回 SQL 里 COALESCE 的保留分支。
func TestQuotaProbeKeepsResetsAtWhenAdapterOmitsIt(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	interval := 5 * time.Minute
	now := time.Now().UTC()
	stale := now.Add(-2 * interval)
	oldResets := now.Add(72 * time.Hour)
	insertQuota(t, db, id, "100", "tokens", "api_query", "healthy", 1)
	if _, err := db.ExecContext(t.Context(), `
		UPDATE channel_quota_state SET last_probe_at = $2, last_probe_attempt_at = $2,
		       resets_at = $3
		 WHERE channel_id = $1`, id, stale, oldResets); err != nil {
		t.Fatalf("摆旧时刻失败: %v", err)
	}

	// 这次实测只有余量与单位，没有 ResetsAt。
	adapter := &fakeQuotaAdapter{state: quota.State{Remaining: floatPtr(88), Unit: "tokens"}}
	p, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}
	p.Now = func() time.Time { return now }

	p.ProbeIfStale(route.Candidate{
		Channel: channel.Channel{ID: id, Type: "openai-compatible"},
		Quota:   quota.State{LastProbeAttemptAt: &stale},
	})
	p.wg.Wait()

	row := readProbeRow(t, db, id)
	if !row.remaining.Valid || parseNumeric(t, "remaining_estimate", row.remaining.String) != 88 {
		t.Errorf("remaining_estimate = %v，期望 88", row.remaining)
	}
	if !row.resetsAt.Valid {
		t.Fatal("resets_at 被空值抹掉了，期望保留旧值")
	}
	assertNear(t, "resets_at", row.resetsAt.Time, oldResets)
}

// 并发命中同一个过期渠道时，适配器只被调用一次。
//
// 关键在于时钟：第一次探测卡住后要把它推过 TTL，否则后来的调用者连
// 「过期」都判不出来，测到的是 lastAttempt 那一半而不是 inflight——把
// inflight 整段删掉，用例照样会绿。
func TestQuotaProbeSingleFlight(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	interval := 5 * time.Minute
	now := time.Now().UTC()
	stale := now.Add(-2 * interval)
	adapter := &fakeQuotaAdapter{
		state:   quota.State{Remaining: floatPtr(7), Unit: "requests"},
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	p, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}

	var clockMu sync.Mutex
	clock := now
	p.Now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}

	cand := route.Candidate{
		Channel: channel.Channel{ID: id, Type: "openai-compatible"},
		Quota:   quota.State{LastProbeAttemptAt: &stale},
	}

	// 第一次探测先同步登记，然后卡在适配器里。
	p.ProbeIfStale(cand)
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("第一次探测没有开始")
	}

	// 把时钟推过 TTL：接下来的调用者都会通过「过期」判断，只能被 inflight
	// 挡住。
	clockMu.Lock()
	clock = now.Add(2 * interval)
	clockMu.Unlock()

	var callers sync.WaitGroup
	for range 20 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			p.ProbeIfStale(cand)
		}()
	}
	callers.Wait()

	if got := adapter.count(); got != 1 {
		t.Fatalf("并发命中时适配器被调用了 %d 次，期望 1", got)
	}

	close(adapter.release)
	p.wg.Wait()

	// 写回之后再数一次：卡住期间那 20 个调用者不能排在后面补探。
	if got := adapter.count(); got != 1 {
		t.Errorf("等第一次探测结束后适配器共被调用 %d 次，期望 1", got)
	}

	row := readProbeRow(t, db, id)
	if !row.remaining.Valid || parseNumeric(t, "remaining_estimate", row.remaining.String) != 7 {
		t.Errorf("remaining_estimate = %v，期望 7", row.remaining)
	}
}

// 适配器说「这家没有额度查询」时，落成查不到：不是错误、不改余量、留下尝试
// 时刻，并且 TTL 内不再重复探测；读侧仍把它当作可路由的渠道。
func TestQuotaProbeUnsupportedIsNotAnError(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	interval := 5 * time.Minute
	now := time.Now().UTC()
	adapter := &fakeQuotaAdapter{err: channel.ErrUnsupported}
	p, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}
	p.Now = func() time.Time { return now }

	cand := route.Candidate{
		Channel: channel.Channel{ID: id, Type: "openai-compatible"},
	}
	p.ProbeIfStale(cand)
	p.wg.Wait()

	if got := adapter.count(); got != 1 {
		t.Fatalf("适配器被调用了 %d 次，期望 1", got)
	}
	row := readProbeRow(t, db, id)
	if row.remaining.Valid {
		t.Errorf("查不到被写成了一个余量 %q", row.remaining.String)
	}
	if row.lastProbeAt.Valid {
		t.Error("查不到不该写 last_probe_at：这一列只记成功的实测")
	}
	if !row.attemptAt.Valid {
		t.Fatal("查不到也要留下尝试时刻，否则每一条请求都会再探一次")
	}
	assertNear(t, "last_probe_attempt_at", row.attemptAt.Time, now)
	if row.status.String != probeStatusUnsupported {
		t.Errorf("last_probe_status = %q，期望 %q", row.status.String, probeStatusUnsupported)
	}

	// 读侧：余量未知、从未实测过，渠道照旧可以参与路由。
	src := New(db, sealer)
	cands, err := src.Candidates(t.Context(), model)
	if err != nil {
		t.Fatalf("取候选失败: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("候选数为 %d，期望 1", len(cands))
	}
	state := cands[0].Quota
	if state.Remaining != nil {
		t.Errorf("读回的余量 = %v，期望未知", *state.Remaining)
	}
	if state.LastProbeAt != nil {
		t.Error("读回的 last_probe_at 有值：查不到被读成了实测")
	}
	if state.LastProbeAttemptAt == nil {
		t.Error("读回的 last_probe_attempt_at 是 NULL：触发侧拿不到 TTL 时钟")
	}
	if !state.Routable() {
		t.Error("查不到额度的渠道被判成不可路由——多数订阅制上游会被整体排除")
	}

	// TTL 内：即便调用方拿的是探测之前的快照，也不再探。
	p.ProbeIfStale(cand)
	p.wg.Wait()
	if got := adapter.count(); got != 1 {
		t.Errorf("查不到之后 TTL 内又探了一次，适配器共被调用 %d 次", got)
	}
}

// 探测失败时不留下失败值、不覆盖上一次有效值，只记尝试时刻与失败结论。
func TestQuotaProbeErrorKeepsLastValue(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	interval := 5 * time.Minute
	now := time.Now().UTC()
	stale := now.Add(-2 * interval)
	resets := now.Add(48 * time.Hour)
	insertQuota(t, db, id, "42", "tokens", "api_query", "healthy", 1)
	if _, err := db.ExecContext(t.Context(), `
		UPDATE channel_quota_state SET last_probe_at = $2, last_probe_attempt_at = $2,
		       resets_at = $3
		 WHERE channel_id = $1`, id, stale, resets); err != nil {
		t.Fatalf("摆旧时刻失败: %v", err)
	}

	adapter := &fakeQuotaAdapter{err: errors.New("上游连不上（占位）")}
	p, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}
	p.Now = func() time.Time { return now }

	cand := route.Candidate{
		Channel: channel.Channel{ID: id, Type: "openai-compatible"},
		Quota:   quota.State{LastProbeAttemptAt: &stale},
	}
	p.ProbeIfStale(cand)
	p.wg.Wait()

	row := readProbeRow(t, db, id)
	if !row.remaining.Valid || parseNumeric(t, "remaining_estimate", row.remaining.String) != 42 {
		t.Errorf("remaining_estimate = %v，期望仍是 42", row.remaining)
	}
	if row.unit.String != "tokens" {
		t.Errorf("unit = %q，期望仍是 tokens", row.unit.String)
	}
	if row.probeKind != "api_query" {
		t.Errorf("probe_kind = %q，期望仍是 api_query", row.probeKind)
	}
	if got := parseNumeric(t, "confidence", row.confidence.String); got != 1 {
		t.Errorf("confidence = %v，期望仍是 1", got)
	}
	if row.health.String != "healthy" {
		t.Errorf("health = %q，期望仍是 healthy", row.health.String)
	}
	if !row.resetsAt.Valid {
		t.Fatal("resets_at 被探测失败抹掉了")
	}
	assertNear(t, "resets_at", row.resetsAt.Time, resets)
	if !row.lastProbeAt.Valid {
		t.Fatal("last_probe_at 被探测失败抹掉了")
	}
	assertNear(t, "last_probe_at", row.lastProbeAt.Time, stale)
	if !row.attemptAt.Valid {
		t.Fatal("探测失败也该留下尝试时刻")
	}
	assertNear(t, "last_probe_attempt_at", row.attemptAt.Time, now)
	if row.status.String != probeStatusError {
		t.Errorf("last_probe_status = %q，期望 %q", row.status.String, probeStatusError)
	}

	// TTL 内：同一个旧快照再触发也不该再探。失败同样要占住这个窗口，
	// 否则一个持续失败的上游会被每一条请求打一次。
	p.ProbeIfStale(cand)
	p.wg.Wait()
	if got := adapter.count(); got != 1 {
		t.Errorf("探测失败之后 TTL 内又探了一次，适配器共被调用 %d 次", got)
	}
}

// 适配器恰好在 deadline 上返回时，写回仍要成功。
//
// 探测与写回共用一个 ctx 的话，这个时刻的写库会因 context 已结束而失败，
// 于是「三种结果都会更新 last_probe_attempt_at」在最需要它的那一格不成立，
// 渠道会带着旧时刻被反复探测。这里把超时压到毫秒级，让适配器等 ctx 结束
// 再返回，正好落在那个边界上。
func TestQuotaProbeWriteBackOutlivesProbeTimeout(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	now := time.Now().UTC()
	p, err := NewQuotaProbe(db, timeoutAdapter{}, 5*time.Minute)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}
	p.Now = func() time.Time { return now }
	p.Timeout = 20 * time.Millisecond

	p.ProbeIfStale(route.Candidate{Channel: channel.Channel{ID: id, Type: "openai-compatible"}})
	p.wg.Wait()

	row := readProbeRow(t, db, id)
	if !row.attemptAt.Valid {
		t.Fatal("探测超时后没有留下尝试时刻：写回复用了已结束的 ctx")
	}
	assertNear(t, "last_probe_attempt_at", row.attemptAt.Time, now)
	if row.status.String != probeStatusError {
		t.Errorf("last_probe_status = %q，期望 %q", row.status.String, probeStatusError)
	}
}

// 余量见底的渠道靠冷却期恢复：见底被排除 → 冷却期到了回到候选 → 被尝试时
// 重新探测 → 仍是 0 就再等一个冷却期 → 探到有额度就恢复正常路由。
//
// 这是一条**真库**用例：它要证明的正是「库里那一列被写回之后，读侧看到的
// 判据真的变了」，单独的读写两侧各自绿着也说明不了这条链路通。
func TestQuotaProbeRecoversExhaustedChannel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	interval := 5 * time.Minute
	cooldown := 15 * time.Minute
	// 截到微秒：写进 timestamptz 的时刻只保留微秒，而这条用例会在
	// 「恰好一个冷却期」那个边界上比较。不截的话，库里的时刻可能比 now
	// 晚半个微秒，边界就从「已过」翻到「未过」。
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 见底，且上次探测尝试已经过去了一个冷却期。
	insertQuota(t, db, id, "0", "tokens", "api_query", "healthy", 1)
	lastAttempt := now.Add(-cooldown)
	if _, err := db.ExecContext(t.Context(), `
		UPDATE channel_quota_state SET last_probe_at = $2, last_probe_attempt_at = $2
		 WHERE channel_id = $1`, id, lastAttempt); err != nil {
		t.Fatalf("摆旧时刻失败: %v", err)
	}

	src := New(db, sealer)
	readCandidates := func(t *testing.T) []route.Candidate {
		t.Helper()
		cands, err := src.Candidates(t.Context(), model)
		if err != nil {
			t.Fatalf("取候选失败: %v", err)
		}
		if len(cands) != 1 {
			t.Fatalf("候选数为 %d，期望 1", len(cands))
		}
		return cands
	}

	clock := now
	r := &route.Router{
		Now:           func() time.Time { return clock },
		QuotaCooldown: cooldown,
	}

	plan := r.Select(readCandidates(t), model, route.Needs{})
	if len(plan.Candidates) != 1 {
		t.Fatalf("冷却期已过的见底渠道没有回到候选集: %+v", plan.Excluded)
	}

	// 第一次重探：仍然是 0。
	adapter := &fakeQuotaAdapter{state: quota.State{Remaining: floatPtr(0), Unit: "tokens"}}
	p, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}
	p.Now = func() time.Time { return clock }
	p.ProbeIfStale(plan.Candidates[0])
	p.wg.Wait()
	if got := adapter.count(); got != 1 {
		t.Fatalf("冷却期到了却没有探测，适配器被调用 %d 次", got)
	}

	// 仍是 0：这次探测把尝试时刻推到了 now，于是要再等一个冷却期。
	clock = now.Add(time.Minute)
	plan = r.Select(readCandidates(t), model, route.Needs{})
	if len(plan.Candidates) != 0 {
		t.Fatalf("重探仍是 0，渠道不该在这个冷却期里被选中: %+v", plan.Candidates)
	}
	if len(plan.Excluded) != 1 || plan.Excluded[0].Reason != "额度已耗尽" {
		t.Fatalf("Excluded = %+v，期望记下「额度已耗尽」", plan.Excluded)
	}

	clock = now.Add(cooldown)
	plan = r.Select(readCandidates(t), model, route.Needs{})
	if len(plan.Candidates) != 1 {
		t.Fatalf("又过了一个冷却期，渠道应当再次被放行: %+v", plan.Excluded)
	}

	// 第二次重探：上游的额度回来了。
	adapter.mu.Lock()
	adapter.state = quota.State{Remaining: floatPtr(42), Unit: "tokens"}
	adapter.mu.Unlock()
	p.ProbeIfStale(plan.Candidates[0])
	p.wg.Wait()
	if got := adapter.count(); got != 2 {
		t.Fatalf("第二次重探后适配器共被调用 %d 次，期望 2", got)
	}

	// 读侧恢复：余量是探到的那 42，渠道重新正常参与路由。
	cands := readCandidates(t)
	if got := cands[0].Quota.Remaining; got == nil || *got != 42 {
		t.Fatalf("重探之后的余量 = %v，期望 42", got)
	}
	if !cands[0].Quota.Usable() {
		t.Error("探到 42 之后渠道仍被判成不可用")
	}
	plan = r.Select(cands, model, route.Needs{})
	if len(plan.Candidates) != 1 {
		t.Fatalf("恢复之后渠道没有回到候选集: %+v", plan.Excluded)
	}

	row := readProbeRow(t, db, id)
	if row.probeKind != "api_query" {
		t.Errorf("probe_kind = %q，期望 api_query", row.probeKind)
	}
	if !row.attemptAt.Valid {
		t.Fatal("重探之后 last_probe_attempt_at 仍是 NULL")
	}
	assertNear(t, "last_probe_attempt_at", row.attemptAt.Time, clock)
}

// 零间隔必须由 NewQuotaProbe 自己拒绝，而不是交给下游去「顺带」拒绝。
//
// 零值不是「不探测」，而是「每一条命中的请求都探测」——它必须作为配置错误
// 在这里报出来。这一条同时钉住报错来自哪一层：让 SelfAccountingStaleAfter
// 去拒绝时，错误会多一层「由探测间隔导出陈旧阈值失败」的前缀，运维看到的
// 第一个词就成了派生失败，而不是「间隔无效」。两种拒绝在库上长得一样，
// 只有错误文本分得开。
func TestNewQuotaProbeRejectsZeroInterval(t *testing.T) {
	db := testDB(t)

	if _, err := NewQuotaProbe(db, &fakeQuotaAdapter{}, 0); err == nil {
		t.Fatal("零间隔应当被拒绝")
	} else if !strings.HasPrefix(err.Error(), "额度探测间隔必须为正") {
		t.Errorf("零间隔的错误 = %q，期望它以「额度探测间隔必须为正」开头——由这一层拒绝，不靠下游兜底", err)
	}

	if _, err := NewQuotaProbe(db, &fakeQuotaAdapter{}, -time.Second); err == nil {
		t.Error("负间隔应当被拒绝")
	}
}

// 距上一次尝试**恰好**一个间隔时就算过期。
//
// 边界两侧各钉一次：差一个纳秒不探，到点就探。只钉一侧的话，把 >= 改成 >
// 之后「恰好到点」那一格会静默地滑到下一轮——而 TTL 本来就是按配置给的值
// 兑现的，滑过去等于配置说 5 分钟、实际是 5 分钟多一点。
func TestQuotaProbeDueAtExactInterval(t *testing.T) {
	db := testDB(t)
	interval := 5 * time.Minute
	now := time.Now().UTC()

	p, err := NewQuotaProbe(db, &fakeQuotaAdapter{}, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}

	exactly := now.Add(-interval)
	p.mu.Lock()
	due := p.dueLocked(1, &exactly, now)
	p.mu.Unlock()
	if !due {
		t.Error("距上一次尝试恰好一个间隔时应当算过期（判据是 >=，不是 >）")
	}

	notYet := now.Add(-interval + time.Nanosecond)
	p.mu.Lock()
	due = p.dueLocked(1, &notYet, now)
	p.mu.Unlock()
	if due {
		t.Error("还差一点才到间隔时不该算过期")
	}
}

// 进程内的尝试时刻只用来把快照往前推，不能反过来把更新的快照压回去。
//
// 候选快照可能来自另一个实例：它记下的尝试时刻比本进程这次更晚。两个时刻
// 取较晚的那个，于是这里应当按快照算——还没到 TTL，不探。把 && 改成 ||
// 会让条件在「进程内有记录」时恒真，于是用更旧的进程内时刻去覆盖更新的
// 快照，刚被别的实例探过的渠道会被本进程再探一次。
func TestQuotaProbeKeepsNewerCandidateAttempt(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	interval := 5 * time.Minute
	base := time.Now().UTC().Truncate(time.Microsecond)
	adapter := &fakeQuotaAdapter{state: quota.State{Remaining: floatPtr(5), Unit: "tokens"}}
	p, err := NewQuotaProbe(db, adapter, interval)
	if err != nil {
		t.Fatalf("构造额度探测失败: %v", err)
	}
	clock := base
	p.Now = func() time.Time { return clock }

	// 本进程在 base 发起过一次探测。
	p.ProbeIfStale(route.Candidate{Channel: channel.Channel{ID: id, Type: "openai-compatible"}})
	p.wg.Wait()
	if got := adapter.count(); got != 1 {
		t.Fatalf("首次探测的适配器调用数 = %d，期望 1", got)
	}

	// 另一个实例在 base+1.5 个间隔探过它；此刻离开那次尝试才 0.4 个间隔。
	newer := base.Add(interval + interval/2)
	clock = newer.Add(interval * 2 / 5)
	cand := route.Candidate{
		Channel: channel.Channel{ID: id, Type: "openai-compatible"},
		Quota:   quota.State{LastProbeAttemptAt: &newer},
	}
	p.ProbeIfStale(cand)
	p.wg.Wait()
	if got := adapter.count(); got != 1 {
		t.Errorf("更新的快照被更旧的进程内时刻覆盖：又探了一次，适配器共被调用 %d 次", got)
	}
}

// 写回的结果该不该告警：成功时安静，失败时叫一声。
//
// 探测跑在自己的 goroutine 里，写回失败没有返回值可以断言——日志是这件事
// **唯一**的外部信号。这里三种结局各看一次：成功写回、写回失败（实测不自洽
// 被拒）、查不到落库成功。把任何一个判据反过来，都会让「失败静默、成功告警」
// 这组现象出现，而它在库里看不出区别。
func TestQuotaProbeWarnsOnlyWhenWriteBackFails(t *testing.T) {
	interval := 5 * time.Minute
	now := time.Now().UTC()

	probeWith := func(t *testing.T, adapter QuotaAdapter) (logs string, db *sql.DB, id int64) {
		t.Helper()
		db = testDB(t)
		sealer := testSealer(t)
		id = insertChannel(t.Context(), t, db, sealer, channelSpec{model: uniqueModel()})

		var buf bytes.Buffer
		p, err := NewQuotaProbe(db, adapter, interval)
		if err != nil {
			t.Fatalf("构造额度探测失败: %v", err)
		}
		p.Now = func() time.Time { return now }
		p.Logger = slog.New(slog.NewTextHandler(&buf, nil))

		p.ProbeIfStale(route.Candidate{Channel: channel.Channel{ID: id, Type: "openai-compatible"}})
		p.wg.Wait()
		return buf.String(), db, id
	}

	t.Run("成功写回不告警", func(t *testing.T) {
		logs, db, id := probeWith(t, &fakeQuotaAdapter{state: quota.State{Remaining: floatPtr(10), Unit: "tokens"}})
		if strings.Contains(logs, "没有写进去") {
			t.Errorf("写回成功却告警了：%s", logs)
		}
		// 安静之外还要真的落库：这个渠道本来没有额度行，走的是 upsert 的插入分支。
		snap := readQuota(t, db, id)
		if !snap.hasRemain || snap.remaining != 10 {
			t.Errorf("remaining = %v（有值 %v），期望 10——「写回成功」要真的落库",
				snap.remaining, snap.hasRemain)
		}
		if snap.unit != "tokens" {
			t.Errorf("unit = %q，期望 tokens", snap.unit)
		}
		if snap.probeKind != "api_query" {
			t.Errorf("probe_kind = %q，期望 api_query", snap.probeKind)
		}
	})

	t.Run("写回失败要告警", func(t *testing.T) {
		// 没有余量也没有耗尽标记的「实测」会被 quota.Validate 拒绝。
		logs, _, _ := probeWith(t, &fakeQuotaAdapter{})
		if !strings.Contains(logs, "渠道额度探测结果没有写进去") {
			t.Errorf("写回失败了却没有告警：%q", logs)
		}
	})

	t.Run("查不到落库成功不告警", func(t *testing.T) {
		logs, _, _ := probeWith(t, &fakeQuotaAdapter{err: channel.ErrUnsupported})
		if strings.Contains(logs, "没有写进去") {
			t.Errorf("查不到的结论写进去了却告警：%s", logs)
		}
	})
}
