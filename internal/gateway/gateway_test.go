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

type fakeRules struct {
	err error
	// queries 记下每次匹配的条件：预留那一次拿到的应当是**整个候选集**，
	// 结算那一次是**实际选中的那一个**。
	queries []pipeline.RuleQuery
}

func (f *fakeRules) Match(_ context.Context, q pipeline.RuleQuery) (pipeline.Rule, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return pipeline.Rule{}, f.err
	}
	return pipeline.Rule{ID: 1, Version: 1, Source: simpleRule}, nil
}

func (f *fakeRules) MatchAll(_ context.Context, q pipeline.RuleQuery) ([]pipeline.Rule, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]pipeline.Rule, 0, len(q.Candidates))
	for range q.Candidates {
		out = append(out, pipeline.Rule{ID: 1, Version: 1, Source: simpleRule})
	}
	return out, nil
}

type fakeCandidates struct {
	cands []route.Candidate
	err   error
}

func (f *fakeCandidates) Candidates(context.Context, string) ([]route.Candidate, error) {
	return f.cands, f.err
}

// releaseCall 是一次释放：释放的是哪次请求，带着什么失败依据。
//
// 两者放在同一条记录里而不是两个并列的切片：它们说的是同一次释放，
// 分成两份之后「第 i 个对应第 i 个」这条约定只活在读的人脑子里。
type releaseCall struct {
	req     pipeline.Request
	failure *pipeline.UpstreamFailure
}

type fakeReservations struct {
	saved []*pipeline.Reservation
	err   error
	next  int64

	released []releaseCall
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

func (f *fakeReservations) Release(ctx context.Context, req pipeline.Request, failure *pipeline.UpstreamFailure) error {
	f.releaseCtxErr = append(f.releaseCtxErr, ctx.Err())
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.released = append(f.released, releaseCall{req: req, failure: failure})
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
	// reqs 按顺序记下**交给结算的那份请求**。上游模型名与渠道 id 一样，
	// 只有选路之后才知道，所以它由网关从选路结果里搬过来——搬没搬只有
	// 看这份请求才知道（见 #338）。
	reqs []pipeline.Request
	err  error
}

func (f *fakeSettlements) Save(_ context.Context, req pipeline.Request, c *pipeline.Charge) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, c)
	f.reqs = append(f.reqs, req)
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

// identityAliases 是「规范名就是上游名」的别名来源，代表**一条别名都没配**
// 的部署：大多数部署都是这样，而它们必须照旧能打通。
//
// 网关这一层不关心映射怎么算，只关心「选路给出的上游名有没有跟着账单落下去」。
// 所以除了下面那条专门钉这件事的用例，其余用例都用它。
type identityAliases struct{}

func (identityAliases) UpstreamModel(_ context.Context, _ int64, canonical string) (string, error) {
	return canonical, nil
}

// fixedAliases 不管渠道、一律把规范名翻成同一个上游名。
//
// 只用来分辨「网关有没有把选路结果里的上游名搬到结算请求上」——按渠道配
// 不同的名字是路由层那条用例的事。
type fixedAliases struct{ upstream string }

func (f fixedAliases) UpstreamModel(_ context.Context, _ int64, _ string) (string, error) {
	return f.upstream, nil
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
	recorder        *fakeRecorder
	// aliases 是选路用的别名来源，默认按同名回落。
	aliases route.ModelAliases
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
		recorder:        &fakeRecorder{},
		aliases:         identityAliases{},
	}
	for _, o := range opts {
		o(h)
	}

	h.gw = &Gateway{
		Pipeline: &pipeline.Pipeline{
			Rules:    h.rules,
			Programs: expr.NewCache(16),
		},
		Router:          &route.Router{Rand: rand.New(rand.NewSource(1)), Aliases: h.aliases},
		Candidates:      h.candidates,
		Reservations:    h.reservations,
		Settlements:     h.settlements,
		Executor:        h.executor,
		Redactor:        h.redactor,
		Snapshots:       h.snapshots,
		ReleaseFailures: h.releaseFailures,
		Recorder:        h.recorder,
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
		// 资产品种由入口从运行期配置填（见 pipeline.Request.Asset），
		// 计费层不再兜默认值。夹具给一个具体的值，与生产的形状一致。
		Asset: "CNY",
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

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游失败应返回错误")
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("上游失败应释放预留一次，实际 %d 次", len(h.reservations.released))
	}
	if got := h.reservations.released[0].req.RequestID; got != "req-1" {
		t.Errorf("释放的是 %q，期望 req-1", got)
	}
	// 释放的是**落过库的那次预留**：没落过库就没有可释放的东西。
	if len(h.reservations.saved) != 1 {
		t.Errorf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
}

// 上游失败时，账单要拿到「上游回了什么、为什么不计费」这份依据。
//
// 它由释放那一步带着落库（见 handle.release）：不计费的结局不写账本，
// 账单那一行只有这一次写入机会，而网关是唯一知道上游回了什么的那一层。
//
// 四类失败**分开钉**，因为它们在两列上的落法不同——只钉其中一类的话，
// 把另外几类也写成同一个状态码同样能通过，而那两个数会是编出来的：
//
//   - 非 2xx：状态码就是上游回的那个
//   - 上游没给出状态码（连不上、响应解不开）：状态码必须留空
//     （HTTP 里没有 0 这个状态码）
//   - 上游接受了请求却没吐字：状态码同样留空，分类是另一档
//   - 不是上游的失败：两列都留空（插件阻断时上游其实是好的）
func TestUpstreamFailureCarriesEvidenceToRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail 是执行器回的错误，也就是这次请求的失败成因。
		fail error
		// hook 非空时改由插件阻断，执行器那一次调用是成功的。
		hook bool
		// wantStatus 与 wantCode 是期望落库的两列；wantNone 表示两列都该留空。
		wantStatus int
		wantCode   string
		wantNone   bool
	}{
		{
			name:       "上游回了 400",
			fail:       &channel.ErrUpstream{StatusCode: 400, Retryable: false},
			wantStatus: 400,
			wantCode:   pipeline.ErrorUpstreamHTTPError,
		},
		{
			name:       "上游回了 500",
			fail:       &channel.ErrUpstream{StatusCode: 500, Retryable: true},
			wantStatus: 500,
			wantCode:   pipeline.ErrorUpstreamHTTPError,
		},
		{
			// 连不上：ErrUpstream 的 StatusCode 是零值，因为一个 HTTP 响应
			// 都没有过。这一条与上面两条的差别正是「0 该不该写进库里」。
			name:     "连接根本没建立",
			fail:     &channel.ErrUpstream{Retryable: true, Err: errors.New("connection refused")},
			wantCode: pipeline.ErrorUpstreamNoAnswer,
		},
		{
			// 上游回了 2xx，却把错误写在体里（适配器的 extractEmbeddedError）。
			// 状态码同样是零值，所以它与「连不上」落进同一档——这一层分不出
			// 两者，用例把这件事钉下来，免得下一个人以为漏了一种。
			name:     "上游 2xx 但体里带 error",
			fail:     &channel.ErrUpstream{Body: "upstream said: model not found"},
			wantCode: pipeline.ErrorUpstreamNoAnswer,
		},
		{
			// 上游接受了请求、一个 token 都没吐就断了（流式）。它与上面两条
			// 的差别是**成因不同**：那两条是「没拿到响应」，这条是拿到了
			// 2xx 响应头、正文一个字都没有。
			//
			// 单独一条钉着，是因为这一支是 upstreamEvidence 里唯一一个不看
			// ErrUpstream 的分支——把它删掉的话，这条路上的依据会整个丢掉
			// （两列都 NULL），而库里 NULL 与「这一批恰好没有失败」长得一样。
			name:     "上游接受请求后没吐字就断了",
			fail:     errStreamAborted,
			wantCode: pipeline.ErrorUpstreamStreamAborted,
		},
		{
			// 插件阻断：上游那一次调用是成功的，所以这不是上游的失败，
			// 两列都该留空。写成「上游失败」会让事后统计把插件的拦截
			// 记到上游头上。
			name:     "插件阻断",
			hook:     true,
			wantNone: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			hooks := Hooks{}
			if tc.hook {
				hooks.OnResponse = func(context.Context, *channel.Response) error {
					return errors.New("插件阻断")
				}
			} else {
				h.executor.failFor = map[int64]error{1: tc.fail}
			}

			if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), hooks); err == nil {
				t.Fatal("这次请求应当失败")
			}
			if len(h.reservations.released) != 1 {
				t.Fatalf("不计费的结局应释放预留一次，实际 %d 次", len(h.reservations.released))
			}

			got := h.reservations.released[0].failure
			if tc.wantNone {
				if got != nil {
					t.Fatalf("这次失败不是上游造成的，不该带依据，得到 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("上游失败应当带着依据去释放，实际没有")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("upstream_status = %d，期望 %d", got.Status, tc.wantStatus)
			}
			if got.Code != tc.wantCode {
				t.Errorf("error_code = %q，期望 %q", got.Code, tc.wantCode)
			}
		})
	}
}

// 结算之前就返回（客户端断开、ctx 取消）时没有上游失败的依据。
//
// 这条路上上游那一次调用可能压根没发生（路由看见 ctx 已经 Done 就返回了），
// 所以「上游回了什么」没有答案。写一个默认值进去会让「没有这回事」与
// 「上游回了某个码」在库里长得一样。
func TestReleaseWithoutUpstreamCallCarriesNoEvidence(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.gw.Chat(ctx, request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("ctx 已取消时不该成功")
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("应释放预留一次，实际 %d 次", len(h.reservations.released))
	}
	if got := h.reservations.released[0].failure; got != nil {
		t.Errorf("上游根本没被调用，不该带依据，得到 %+v", got)
	}
}

// 客户端中途断开：请求的 ctx 已经 Done，释放仍必须真的发生。
//
// 这是这个修复最容易漏的地方——拿那个已经取消的 ctx 去释放，
// 释放本身就会失败，而它恰恰是最需要释放的时刻。
//
// 断开在这里表现为「预留已经落库、调上游时 ctx 已经取消」：路由在
// 调上游之前就看见 ctx 的结局并返回，请求于是带着一个失败的结局收尾。
func TestReleaseWorksWithCanceledRequestContext(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已经断开

	if _, err := h.gw.Chat(ctx, request(), upstreamRequest(), Hooks{}); err == nil {
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatal(err)
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("没有结算落库层时成功路径也必须释放预留，实际 %d 次", len(h.reservations.released))
	}
	if got := h.reservations.released[0].req.RequestID; got != "req-1" {
		t.Errorf("释放的应是本次请求，实际 %q", got)
	}
}

// 结算这一步自己失败时，钱同样不能冻着——它是同一个缺陷的另一条入口。
//
// 结算落库失败时事务已经回滚，一分钱都没扣，冻着的那笔钱没有理由留着。
func TestSettlementFailureReleasesReservation(t *testing.T) {
	h := newHarness(t)
	h.settlements.err = errors.New("账本写不进去")

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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
		_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

// 预留要把**整个候选集**交给规则来源，每个候选带上自己的商家。
//
// 两件事各自都会让预留算小：
//
//   - 只按平台规则算（候选集里那条更贵的渠道价被漏掉），结算于是撞上
//     Capped，实际金额被预留截住、差额由平台吃掉——账目仍然是平的。
//   - 候选里只带渠道 id、不带 owner（商家级规则一条都匹配不上）。
//
// 结算则相反：候选集收窄成实际选中的那一个。候选集是拿来挑一个的，
// 不是拿来结算的。
func TestReserveSeesWholeCandidateSet(t *testing.T) {
	h := newHarness(t)

	a := candidate(1, "gpt-4o")
	a.OwnerUserID = 11
	b := candidate(2, "gpt-4o")
	b.OwnerUserID = 22
	h.candidates.cands = []route.Candidate{a, b}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatal(err)
	}

	if len(h.rules.queries) != 2 {
		t.Fatalf("规则匹配 %d 次，期望 2 次（预留与结算各一次）", len(h.rules.queries))
	}

	reserve := h.rules.queries[0].Candidates
	if len(reserve) != 2 {
		t.Fatalf("预留的候选 = %+v，期望整个候选集（2 个）", reserve)
	}
	if reserve[0].ChannelID != 1 || reserve[0].OwnerUserID != 11 {
		t.Errorf("预留的第一个候选 = %+v，期望渠道 1 商家 11", reserve[0])
	}
	if reserve[1].ChannelID != 2 || reserve[1].OwnerUserID != 22 {
		t.Errorf("预留的第二个候选 = %+v，期望渠道 2 商家 22", reserve[1])
	}

	settle := h.rules.queries[1].Candidates
	if len(settle) != 1 {
		t.Fatalf("结算的候选 = %+v，期望收窄成恰好一个", settle)
	}
	matched := false
	for _, c := range []route.Candidate{a, b} {
		if c.Channel.ID == settle[0].ChannelID && c.OwnerUserID == settle[0].OwnerUserID {
			matched = true
		}
	}
	if !matched {
		t.Errorf("结算的候选 %+v 不是任何一个输入候选——商家的对应关系错位了", settle[0])
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
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

// 上游模型名与渠道 id 一样，只有选路之后才知道，所以由网关从选路结果里
// 搬到结算请求上。搬没搬，只有看**交给结算的那份请求**才知道。
//
// 漏搬的表现是 billing_requests.upstream_model 恒为 NULL——而它从建表那天
// 起就一直是 NULL，所以「漏了」与「历史行」在库里长得一样（见 #338）。
func TestSettlementUsesActualUpstreamModel(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.aliases = fixedAliases{upstream: "vendor/gpt-4o"}
	})

	var seenUpstreamModel string
	h.gw.Settlements = &capturingSettlements{onSave: func(req pipeline.Request) {
		seenUpstreamModel = req.UpstreamModel
	}}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatal(err)
	}
	if seenUpstreamModel != "vendor/gpt-4o" {
		t.Errorf("结算带的上游模型名 = %q，期望选路给出的 %q", seenUpstreamModel, "vendor/gpt-4o")
	}
}

// ── 快照 ────────────────────────────────────────────────────────────────

// 快照绝不能阻塞请求。这里让 Save 永久阻塞，请求仍必须返回。
func TestSnapshotDoesNotBlockRequest(t *testing.T) {
	h := newHarness(t)
	h.snapshots.blockOn = make(chan struct{})
	defer close(h.snapshots.blockOn)

	done := make(chan error, 1)
	go func() {
		_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
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

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
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

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
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
	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); !errors.Is(err, ErrNoExecutor) {
		t.Errorf("期望 ErrNoExecutor，得到 %v", err)
	}
}

// 上游没报 usage 时金额为零，而不是按某个默认价收钱。
//
// 零金额在账上是可见的（有请求无消费），而默认价不可见。
func TestMissingUsageYieldsZeroAmount(t *testing.T) {
	h := newHarness(t)
	h.executor.resp = &channel.Response{UpstreamModel: "m"} // 没有 Usage

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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
	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
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
