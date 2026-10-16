package gateway

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/tokens"
)

// ── 测试替身 ────────────────────────────────────────────────────────────

const simpleRule = `{"prompt": 0.000001, "completion": 0.000002}`

type fakeRules struct{ err error }

func (f *fakeRules) Match(context.Context, pipeline.RuleQuery) (pipeline.Rule, error) {
	if f.err != nil {
		return pipeline.Rule{}, f.err
	}
	return pipeline.Rule{ID: 1, Version: 1, Source: simpleRule}, nil
}

type fakeCandidates struct {
	cands []route.Candidate
	err   error
}

func (f *fakeCandidates) Candidates(context.Context, string) ([]route.Candidate, error) {
	return f.cands, f.err
}

type fakeReservations struct {
	saved []*pipeline.Reservation
	err   error
	next  int64
}

func (f *fakeReservations) Save(_ context.Context, r *pipeline.Reservation) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.next++
	f.saved = append(f.saved, r)
	return f.next, nil
}

type fakeSettlements struct {
	saved []*pipeline.Charge
	err   error
}

func (f *fakeSettlements) Save(_ context.Context, _ pipeline.Request, c *pipeline.Charge) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, c)
	return nil
}

type fakeExecutor struct {
	resp    *channel.Response
	err     error
	failFor map[int64]error
	calls   []int64
}

func (f *fakeExecutor) Chat(_ context.Context, ch channel.Channel, _ *channel.Request) (*channel.Response, error) {
	f.calls = append(f.calls, ch.ID)
	if err, ok := f.failFor[ch.ID]; ok {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeExecutor) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	_, err := f.Chat(ctx, ch, req)
	if err != nil {
		return nil, err
	}
	out := make(chan channel.StreamEvent, 1)
	out <- channel.StreamEvent{Kind: "done"}
	close(out)
	return out, nil
}

type fakeRedactor struct {
	reqErr  error
	respErr error
	mu      sync.Mutex
	redacts int
}

func (f *fakeRedactor) Redact(*channel.Request) (*Snapshot, error) {
	f.mu.Lock()
	f.redacts++
	f.mu.Unlock()
	if f.reqErr != nil {
		return nil, f.reqErr
	}
	return &Snapshot{Skeleton: map[string]any{"model": "placeholder"}}, nil
}

func (f *fakeRedactor) RedactResponse(*channel.Response) (*Snapshot, error) {
	if f.respErr != nil {
		return nil, f.respErr
	}
	return &Snapshot{}, nil
}

type fakeSnapshots struct {
	mu      sync.Mutex
	saved   []*Snapshot
	err     error
	blockOn chan struct{}
}

func (f *fakeSnapshots) Save(_ context.Context, s *Snapshot) error {
	if f.blockOn != nil {
		<-f.blockOn
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, s)
	return f.err
}

func (f *fakeSnapshots) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saved)
}

// ── 组装 ────────────────────────────────────────────────────────────────

func candidate(id int64, model string) route.Candidate {
	return route.Candidate{
		Channel: channel.Channel{
			ID: id, Name: "ch", BaseURL: "https://api.example.com",
			Protocol: channel.ProtocolOpenAI, Credential: "placeholder",
			Models: []string{model},
		},
		OwnerUserID: 7,
		Health:      route.Health{SuccessRate: 1.0},
	}
}

type harness struct {
	gw           *Gateway
	rules        *fakeRules
	candidates   *fakeCandidates
	reservations *fakeReservations
	settlements  *fakeSettlements
	executor     *fakeExecutor
	redactor     *fakeRedactor
	snapshots    *fakeSnapshots
}

func newHarness(t *testing.T, opts ...func(*harness)) *harness {
	t.Helper()

	h := &harness{
		rules:        &fakeRules{},
		candidates:   &fakeCandidates{cands: []route.Candidate{candidate(1, "gpt-4o")}},
		reservations: &fakeReservations{},
		settlements:  &fakeSettlements{},
		executor: &fakeExecutor{resp: &channel.Response{
			UpstreamModel: "gpt-4o-2024",
			Usage:         channel.Usage{PromptTokens: 1000, CompletionTokens: 100},
		}},
		redactor:  &fakeRedactor{},
		snapshots: &fakeSnapshots{},
	}
	for _, o := range opts {
		o(h)
	}

	h.gw = &Gateway{
		Pipeline: &pipeline.Pipeline{
			Rules:    h.rules,
			Programs: expr.NewCache(16),
		},
		Router:       &route.Router{Rand: rand.New(rand.NewSource(1))},
		Candidates:   h.candidates,
		Reservations: h.reservations,
		Settlements:  h.settlements,
		Executor:     h.executor,
		Redactor:     h.redactor,
		Snapshots:    h.snapshots,
		Now:          func() time.Time { return fixedAt },
	}
	return h
}

var fixedAt = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func request() pipeline.Request {
	return pipeline.Request{
		RequestID:      "req-1",
		UserID:         42,
		CanonicalModel: "gpt-4o",
		InputTokens:    1000,
		MaxTokens:      500,
	}
}

func upstreamRequest() *channel.Request {
	return &channel.Request{
		Model:    "gpt-4o",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
}

// ── 正常路径 ────────────────────────────────────────────────────────────

func TestChatSucceeds(t *testing.T) {
	h := newHarness(t)

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Response == nil || res.Channel.ID != 1 {
		t.Errorf("结果 = %+v", res)
	}
	if res.Charge == nil {
		t.Fatal("应产生账单")
	}
	// 1000 * 1e-6 + 100 * 2e-6 = 0.001 + 0.0002
	if res.Charge.Settlement.Actual.String() != "0.00120000" {
		t.Errorf("金额 = %s", res.Charge.Settlement.Actual)
	}
	if len(res.Charge.Entries) == 0 {
		t.Error("应生成账本分录")
	}
}

// 预留必须先落库再调上游：中间隔着一次可能数分钟的上游调用，
// 内存里那份冻结金额在重启后就没了。
func TestReservationSavedBeforeUpstreamCall(t *testing.T) {
	var reservedAt, calledAt int
	h := newHarness(t)
	h.reservations = &fakeReservations{}
	h.gw.Reservations = h.reservations

	// 用一个记录调用顺序的执行器
	h.gw.Executor = &orderingExecutor{
		inner:    h.executor,
		onCall:   func() { calledAt = len(h.reservations.saved) },
		reserved: func() int { return reservedAt },
	}

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
	if calledAt != 1 {
		t.Errorf("调上游时预留落库数 = %d，期望 1（预留必须先落库）", calledAt)
	}
}

type orderingExecutor struct {
	inner    Executor
	onCall   func()
	reserved func() int
}

func (o *orderingExecutor) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	o.onCall()
	return o.inner.Chat(ctx, ch, req)
}

func (o *orderingExecutor) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	o.onCall()
	return o.inner.ChatStream(ctx, ch, req)
}

// ── 失败与释放 ──────────────────────────────────────────────────────────

func TestChatUpstreamFailureIsNotBilled(t *testing.T) {
	h := newHarness(t)
	h.executor.failFor = map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 400, Retryable: false},
	}

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err == nil {
		t.Fatal("上游失败应返回错误")
	}
	if res != nil {
		t.Error("上游失败不该有结果")
	}
	// 不计费：结算表里不该有东西
	if len(h.settlements.saved) != 0 {
		t.Errorf("上游失败不该落结算：%+v", h.settlements.saved)
	}
}

// 没有可用渠道时**不该预留**：冻住用户的钱却没有可用的渠道，
// 那笔钱要等到超时才释放。
func TestNoCandidatesDoesNotReserve(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = nil

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if !errors.Is(err, route.ErrNoCandidates) {
		t.Errorf("期望 ErrNoCandidates，得到 %v", err)
	}
	if len(h.reservations.saved) != 0 {
		t.Errorf("没有候选时不该预留：%+v", h.reservations.saved)
	}
}

// 取候选失败时同样不该预留。
func TestCandidateErrorDoesNotReserve(t *testing.T) {
	h := newHarness(t)
	h.candidates.err = errors.New("数据库挂了")

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err == nil {
		t.Fatal("应报错")
	}
	if len(h.reservations.saved) != 0 {
		t.Error("取候选失败时不该预留")
	}
}

// 预留落库失败时不该调上游：钱没冻住就不该花。
func TestReservationSaveFailureStopsBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	h.reservations.err = errors.New("写库失败")

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err == nil {
		t.Fatal("应报错")
	}
	if len(h.executor.calls) != 0 {
		t.Errorf("预留落库失败却调了上游：%v", h.executor.calls)
	}
}

// 规则坏了要在调上游之前发现。
func TestBrokenRuleStopsBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	h.rules.err = pipeline.ErrNoRule

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err == nil {
		t.Fatal("应报错")
	}
	if len(h.executor.calls) != 0 {
		t.Error("规则坏了却调了上游")
	}
}

// 结算落库失败必须报出来：账已经算出来了但没记下，而钱是真金白银。
func TestSettlementSaveFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.settlements.err = errors.New("账本写不进去")

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err == nil {
		t.Fatal("结算落库失败必须报出来，不能吞掉")
	}
	if !strings.Contains(err.Error(), "结算落库失败") {
		t.Errorf("错误信息应说明是哪一步失败: %v", err)
	}
}

// 换渠道：第一个渠道可重试地失败，第二个成功。
func TestFailoverToSecondChannel(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{
		candidate(1, "gpt-4o"),
		candidate(2, "gpt-4o"),
	}
	h.executor.failFor = map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 503, Retryable: true},
	}

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err != nil {
		t.Fatalf("应换渠道成功: %v", err)
	}
	if res.Channel.ID == 1 {
		t.Error("不该停在失败的渠道上")
	}
	// 失败的尝试也要记下来，但它不计费
	if len(res.Attempts) != 2 {
		t.Errorf("尝试数 = %d，期望 2", len(res.Attempts))
	}
	if res.Charge == nil {
		t.Error("成功的这次应当计费")
	}
}

// 结算用的是**实际选中的渠道**，而不是请求里给的。
func TestSettlementUsesActualChannel(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{candidate(9, "gpt-4o")}

	var seenChannelID int64
	h.gw.Settlements = &capturingSettlements{onSave: func(req pipeline.Request) {
		seenChannelID = req.ChannelID
	}}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err != nil {
		t.Fatal(err)
	}
	if seenChannelID != 9 {
		t.Errorf("结算用的渠道 = %d，期望实际选中的 9", seenChannelID)
	}
}

type capturingSettlements struct{ onSave func(pipeline.Request) }

func (c *capturingSettlements) Save(_ context.Context, req pipeline.Request, _ *pipeline.Charge) error {
	c.onSave(req)
	return nil
}

// ── 快照 ────────────────────────────────────────────────────────────────

// 快照绝不能阻塞请求。这里让 Save 永久阻塞，请求仍必须返回。
func TestSnapshotDoesNotBlockRequest(t *testing.T) {
	h := newHarness(t)
	h.snapshots.blockOn = make(chan struct{})
	defer close(h.snapshots.blockOn)

	done := make(chan error, 1)
	go func() {
		_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("快照落库阻塞了请求——它是排障辅助，不该影响定价路径")
	}
}

// 脱敏失败就丢快照，绝不回落成存原文。
func TestRedactionFailureDropsSnapshot(t *testing.T) {
	h := newHarness(t)
	h.redactor.reqErr = errors.New("脱敏器出错")

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err != nil {
		t.Fatalf("脱敏失败不该影响请求: %v", err)
	}
	// 给 goroutine 一点时间
	time.Sleep(50 * time.Millisecond)
	if h.snapshots.count() != 0 {
		t.Errorf("脱敏失败应当丢快照，实际落了 %d 条", h.snapshots.count())
	}
}

// 没配脱敏器或落库层时不采集，且不报错——快照是可选能力。
func TestSnapshotOptional(t *testing.T) {
	h := newHarness(t)
	h.gw.Redactor = nil
	h.gw.Snapshots = nil

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err != nil {
		t.Fatalf("没配快照不该报错: %v", err)
	}
}

// 请求快照在**调上游之前**采集：出错要能复现「当时发出去的是什么」。
func TestRequestSnapshotTakenBeforeUpstream(t *testing.T) {
	var redactsAtCall int
	h := newHarness(t)
	h.gw.Executor = &countingRedactExecutor{
		inner:   h.executor,
		redacts: func() int { return h.redactor.redacts },
		seen:    &redactsAtCall,
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err != nil {
		t.Fatal(err)
	}
	if redactsAtCall != 1 {
		t.Errorf("调上游时的脱敏次数 = %d，期望 1（快照要先采集）", redactsAtCall)
	}
}

type countingRedactExecutor struct {
	inner   Executor
	redacts func() int
	seen    *int
}

func (c *countingRedactExecutor) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	*c.seen = c.redacts()
	return c.inner.Chat(ctx, ch, req)
}

func (c *countingRedactExecutor) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	*c.seen = c.redacts()
	return c.inner.ChatStream(ctx, ch, req)
}

// ── 其它 ────────────────────────────────────────────────────────────────

func TestNoExecutor(t *testing.T) {
	h := newHarness(t)
	h.gw.Executor = nil
	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); !errors.Is(err, ErrNoExecutor) {
		t.Errorf("期望 ErrNoExecutor，得到 %v", err)
	}
}

// 上游没报 usage 时金额为零，而不是按某个默认价收钱。
//
// 零金额在账上是可见的（有请求无消费），而默认价不可见。
func TestMissingUsageYieldsZeroAmount(t *testing.T) {
	h := newHarness(t)
	h.executor.resp = &channel.Response{UpstreamModel: "m"} // 没有 Usage

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Charge.Settlement.Actual.IsZero() {
		t.Errorf("上游没报用量时金额应为零，得到 %s", res.Charge.Settlement.Actual)
	}
	if len(res.Charge.Entries) != 0 {
		t.Error("零金额不该产生账本分录")
	}
}

// 用量来源要标明是实测。
func TestUsageSourceIsMeasured(t *testing.T) {
	h := newHarness(t)
	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.Charge.UsageSource != tokens.SourceMeasured {
		t.Errorf("用量来源 = %v，期望实测", res.Charge.UsageSource)
	}
}

// ownerOf 找到渠道时返回它的商家；**找不到时返回 0**。
//
// 变异：`c.Channel.ID == channelID` 改成 `!=`，会返回第一个不匹配的
// 渠道的商家——那是把结算挂到别人的账上。
func TestOwnerOfPicksMatchingChannel(t *testing.T) {
	a := candidate(1, "m")
	a.OwnerUserID = 11
	b := candidate(2, "m")
	b.OwnerUserID = 22
	cands := []route.Candidate{a, b}

	if got := ownerOf(cands, 2); got != 22 {
		t.Errorf("渠道 2 的商家 = %d，期望 22", got)
	}
	if got := ownerOf(cands, 1); got != 11 {
		t.Errorf("渠道 1 的商家 = %d，期望 11", got)
	}
	// 找不到时给 0（平台自营），而不是某个别的渠道的商家
	if got := ownerOf(cands, 999); got != 0 {
		t.Errorf("找不到渠道时 = %d，期望 0", got)
	}
	// 空候选同理
	if got := ownerOf(nil, 1); got != 0 {
		t.Errorf("空候选时 = %d，期望 0", got)
	}
}

// now() 没有注入时用真实时间——这条分支在测试里也要走到，
// 否则「注入被移除」这件事没有测试拦得住。
func TestNowFallsBackToRealClock(t *testing.T) {
	g := &Gateway{}
	got := g.now()
	if got.IsZero() {
		t.Error("没有注入时应返回真实时间，而不是零值")
	}
	if time.Since(got) > time.Minute {
		t.Errorf("返回的时间 %v 看起来不是「现在」", got)
	}

	// 注入了就用注入的
	injected := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	g.Now = func() time.Time { return injected }
	if g.now() != injected {
		t.Error("注入的时钟应生效")
	}
}

// requestIDOf：ctx 里没有请求标识时返回空串，而不是 panic 或乱码。
func TestRequestIDOfWithoutValue(t *testing.T) {
	if got := requestIDOf(context.Background()); got != "" {
		t.Errorf("没有值时 = %q，期望空串", got)
	}
}
