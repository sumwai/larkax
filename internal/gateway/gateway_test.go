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

	released []pipeline.Request
	// releaseCtxErr 记下每次释放时传进来的 ctx 是不是已经不可用。
	//
	// 这一条必须被钉住：客户端断开时请求的 ctx 已经 Done，用那个 ctx
	// 去释放等于没释放，而那笔钱会永久冻着。
	releaseCtxErr []error
	releaseErr    error
}

func (f *fakeReservations) SaveReservation(_ context.Context, _ pipeline.Request, r *pipeline.Reservation) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.next++
	f.saved = append(f.saved, r)
	return f.next, nil
}

func (f *fakeReservations) Release(ctx context.Context, req pipeline.Request) error {
	f.releaseCtxErr = append(f.releaseCtxErr, ctx.Err())
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.released = append(f.released, req)
	return nil
}

type fakeReleaseFailures struct {
	mu      sync.Mutex
	recs    []ReleaseFailure
	err     error
	blockOn chan struct{}
}

func (f *fakeReleaseFailures) Save(_ context.Context, rec ReleaseFailure) error {
	if f.blockOn != nil {
		<-f.blockOn
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, rec)
	return f.err
}

func (f *fakeReleaseFailures) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}

func (f *fakeReleaseFailures) first(t *testing.T) ReleaseFailure {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recs) == 0 {
		t.Fatal("没有收到释放失败的记录")
	}
	return f.recs[0]
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

// first 返回落库的第一条快照，没有则返回 nil。
//
// 落库是异步的，所以取用前先用 waitFor 等它出现（见下面那几条用例）。
func (f *fakeSnapshots) first() *Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 {
		return nil
	}
	return f.saved[0]
}

// ── 组装 ────────────────────────────────────────────────────────────────

func candidate(id int64, model string) route.Candidate {
	return route.Candidate{
		Channel: channel.Channel{
			ID: id, Name: "ch", BaseURL: "https://api.example.com",
			Credential: "placeholder",
			Models:     []string{model},
		},
		OwnerUserID: 7,
		Health:      route.Health{SuccessRate: 1.0},
	}
}

type harness struct {
	gw              *Gateway
	rules           *fakeRules
	candidates      *fakeCandidates
	reservations    *fakeReservations
	settlements     *fakeSettlements
	executor        *fakeExecutor
	redactor        *fakeRedactor
	snapshots       *fakeSnapshots
	releaseFailures *fakeReleaseFailures
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
		redactor:        &fakeRedactor{},
		snapshots:       &fakeSnapshots{},
		releaseFailures: &fakeReleaseFailures{},
	}
	for _, o := range opts {
		o(h)
	}

	h.gw = &Gateway{
		Pipeline: &pipeline.Pipeline{
			Rules:    h.rules,
			Programs: expr.NewCache(16),
		},
		Router:          &route.Router{Rand: rand.New(rand.NewSource(1))},
		Candidates:      h.candidates,
		Reservations:    h.reservations,
		Settlements:     h.settlements,
		Executor:        h.executor,
		Redactor:        h.redactor,
		Snapshots:       h.snapshots,
		ReleaseFailures: h.releaseFailures,
		Now:             func() time.Time { return fixedAt },
	}
	return h
}

// waitFor 等到条件成立，超时则让用例失败。
//
// 上报是**异步**的（与快照落库同理），所以断言不能紧跟在 Chat 返回之后。
// 轮询而不是睡一个固定时长：睡短了在慢机器上会偶发失败，睡长了每次跑都
// 慢一截，而这两者都会让人开始怀疑用例本身。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待「%s」超时", what)
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

// 上游失败**必须**把预留释放掉，否则那笔钱永久冻着——
// 界面上表现为「用户说他的钱不见了」。
//
// 三次断言对应三件事：释放被调用过、释放的是这一次请求、只释放一次
// （重复释放会把冻结额减两次）。
func TestUpstreamFailureReleasesReservation(t *testing.T) {
	h := newHarness(t)
	h.executor.failFor = map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 500, Retryable: false},
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err == nil {
		t.Fatal("上游失败应返回错误")
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("上游失败应释放预留一次，实际 %d 次", len(h.reservations.released))
	}
	if got := h.reservations.released[0].RequestID; got != "req-1" {
		t.Errorf("释放的是 %q，期望 req-1", got)
	}
	// 释放的是**落过库的那次预留**：没落过库就没有可释放的东西。
	if len(h.reservations.saved) != 1 {
		t.Errorf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
}

// 客户端中途断开：请求的 ctx 已经 Done，释放仍必须真的发生。
//
// 这是这个修复最容易漏的地方——拿那个已经取消的 ctx 去释放，
// 释放本身就会失败，而它恰恰是最需要释放的时刻。
//
// 断开在这里表现为「预留已经落库、调上游时 ctx 已经取消」：路由在
// 调上游之前就看见 ctx 的结局并返回，请求于是带着一个失败的结局收尾。
func TestReleaseWorksWithCancelledRequestContext(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已经断开

	if _, err := h.gw.Chat(ctx, request(), upstreamRequest()); err == nil {
		t.Fatal("ctx 已取消时不该成功")
	}
	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留应先落库，实际落库 %d 次", len(h.reservations.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("ctx 已取消时仍应释放预留，实际 %d 次", len(h.reservations.released))
	}
	if err := h.reservations.releaseCtxErr[0]; err != nil {
		t.Errorf("释放用的是已经取消的 ctx（%v）——那等于没释放", err)
	}
}

// 成功路径**不该**释放：解冻由结算那个事务做。
//
// 在这里再释放一次会把冻结额减两次（或让结算的解冻失败），
// 而那是账目对不上的来源。
func TestSuccessDoesNotRelease(t *testing.T) {
	h := newHarness(t)

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err != nil {
		t.Fatal(err)
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("成功路径不该释放预留：%+v", h.reservations.released)
	}
}

// 没有配结算落库层时，成功路径也必须把预留放开。
//
// 它守的是 charged 语义里的一个分叉：charged 记的必须是「结算事务真的跑过」，
// 而不是「Settle 算出来了」。没有落库层时那次 Save 与它事务里的解冻都不会
// 发生，此时若把它当成结算过，Release 会被挡下——两条路都没走，那笔钱就
// 永久冻着；而对账把 Σpending 当作「本来就该冻住的量」，报不出来。
//
// 这与 TestSuccessDoesNotRelease 不矛盾：那一条有落库层，解冻由结算事务做，
// 再释放一次会减两次；这一条没有落库层，只有 Release 这一条出路。
func TestSuccessWithoutSettlementStoreReleasesReservation(t *testing.T) {
	h := newHarness(t)
	// 必须直接把这个接口字段置空，不能把 harness 里那个指针字段设成 nil
	// 再让组装去赋值：一个 nil 的具体指针装进接口之后**接口本身不是 nil**
	// （有类型、没有值），`g.Settlements != nil` 会为真，然后 Save 会在
	// nil 接收者上 panic。生产装配里「不配」就是把这个字段留成零值，
	// 这里要测的正是那一种。
	h.gw.Settlements = nil

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err != nil {
		t.Fatal(err)
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("没有结算落库层时成功路径也必须释放预留，实际 %d 次", len(h.reservations.released))
	}
	if got := h.reservations.released[0].RequestID; got != "req-1" {
		t.Errorf("释放的应是本次请求，实际 %q", got)
	}
}

// 结算这一步自己失败时，钱同样不能冻着——它是同一个缺陷的另一条入口。
//
// 结算落库失败时事务已经回滚，一分钱都没扣，冻着的那笔钱没有理由留着。
func TestSettlementFailureReleasesReservation(t *testing.T) {
	h := newHarness(t)
	h.settlements.err = errors.New("账本写不进去")

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err == nil {
		t.Fatal("结算落库失败必须报出来")
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("结算失败时也该释放预留，实际 %d 次", len(h.reservations.released))
	}
}

// 释放失败不能盖掉最初那个错误：调用方要能分清「上游 500」与
// 「释放失败」，否则排查的人会朝错的方向找。
func TestReleaseFailureDoesNotMaskCause(t *testing.T) {
	h := newHarness(t)
	h.reservations.releaseErr = errors.New("释放写不进去")
	h.executor.failFor = map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 500, Retryable: false},
	}

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Errorf("返回的应是上游错误本身，得到 %v", err)
	}
}

// 释放失败要留下痕。
//
// 不报的代价是这个缺陷原来的形状：那笔钱照旧冻着，而 billing_requests
// 那一行仍是 pending——对账把 pending 行之和当成「本来就该冻住的量」，
// 于是它对得平、不报错，全仓库没有一处会发现它。对账后来补上的
// stale_reservation 要等一天，而且它只看得到「这行挂了很久」，看不到
// 当时为什么没放成。
func TestReleaseFailureIsReportedToSink(t *testing.T) {
	h := newHarness(t)
	cause := errors.New("释放写不进去")
	h.reservations.releaseErr = cause
	h.executor.failFor = map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 500, Retryable: false},
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err == nil {
		t.Fatal("上游失败应返回错误")
	}

	waitFor(t, "释放失败的上报", func() bool { return h.releaseFailures.count() == 1 })
	rec := h.releaseFailures.first(t)
	if rec.RequestID != "req-1" {
		t.Errorf("上报的请求标识 = %q，期望 req-1", rec.RequestID)
	}
	if rec.UserID != 42 {
		t.Errorf("上报的用户 = %d，期望 42", rec.UserID)
	}
	if rec.ReservationID == 0 {
		// 没有它就对不上是哪一行预留——billing_requests 的 id 才是对账
		// 与运维真正要去查的那个键。
		t.Error("上报里要带预留落库拿到的 id")
	}
	if !errors.Is(rec.Err, cause) {
		t.Errorf("上报的错误 = %v，期望 %v", rec.Err, cause)
	}
	if !rec.At.Equal(fixedAt) {
		t.Errorf("上报的时刻 = %v，期望注入的 %v", rec.At, fixedAt)
	}
}

// 成功路径不释放预留，也就不该有释放失败的痕。
//
// 这一条钉的是「上报只在真的失败时发生」：把上报挂在收尾路径上而不看
// 结局，会造出一堆查不出原因的假痕迹。
func TestSuccessDoesNotReportReleaseFailure(t *testing.T) {
	h := newHarness(t)

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest()); err != nil {
		t.Fatal(err)
	}
	// 异步上报若被误触发，这里要给它冒出来的机会。
	time.Sleep(50 * time.Millisecond)
	if n := h.releaseFailures.count(); n != 0 {
		t.Errorf("成功路径不该有释放失败的上报，实际 %d 条", n)
	}
}

// 上报口是**可选**的：不实现就不采集，且请求本身的结局不受影响。
//
// 它是排障辅助，不是计费路径上的一步——把它做成必填依赖，等于让一次
// 留痕失败变成一次请求失败。
func TestReleaseFailureSinkOptional(t *testing.T) {
	h := newHarness(t)
	h.gw.ReleaseFailures = nil
	h.reservations.releaseErr = errors.New("释放写不进去")
	h.executor.failFor = map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 500, Retryable: false},
	}

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Errorf("没配上报口时返回的仍应是上游错误本身，得到 %v", err)
	}
}

// 上报口卡住时请求照样返回：它是留痕，不该拖住调用方。
//
// 释放本身是同步的（有自己的上限），上报不是——它在收尾路径上，
// 而调用方正在等这一次请求返回。
func TestReleaseFailureReportDoesNotBlockRequest(t *testing.T) {
	h := newHarness(t)
	h.releaseFailures.blockOn = make(chan struct{})
	defer close(h.releaseFailures.blockOn)
	h.reservations.releaseErr = errors.New("释放写不进去")
	h.executor.failFor = map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 500, Retryable: false},
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.gw.Chat(context.Background(), request(), upstreamRequest())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("上游失败应返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("上报口阻塞了请求——它是留痕，不该影响计费路径")
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
	// 没冻过钱就没有可释放的东西，别去多打一次数据库。
	if len(h.reservations.released) != 0 {
		t.Errorf("没预留过就不该释放：%+v", h.reservations.released)
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
	// 预留没落库（也就没冻住钱），没有可释放的东西。
	if len(h.reservations.released) != 0 {
		t.Errorf("预留没落库时不该释放：%+v", h.reservations.released)
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

// 快照要记在**发起这次请求的用户**名下。
//
// 这一列没有外键，填 0 或者填成别人都插得进去、不报错——所以「调用没报错」
// 在这里证明不了任何事，只能断言真正落库的那个值。
func TestRequestSnapshotRecordsRequestingUser(t *testing.T) {
	h := newHarness(t)
	req := request()

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest()); err != nil {
		t.Fatal(err)
	}

	var snap *Snapshot
	waitFor(t, "请求快照落库", func() bool {
		snap = h.snapshots.first()
		return snap != nil
	})
	if snap.UserID != req.UserID {
		t.Errorf("快照的 user_id = %d，期望 %d（发起这次请求的用户）", snap.UserID, req.UserID)
	}
	if snap.Direction != "request" {
		t.Errorf("方向 = %q，期望 request", snap.Direction)
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
