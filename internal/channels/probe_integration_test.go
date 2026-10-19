package channels

import (
	"context"
	"database/sql"
	"errors"
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
