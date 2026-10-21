package gateway

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/snapshot"
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

	// added 是每次追加冻结额的记录（请求 + 增量），按调用顺序。
	//
	// 「追加了几次、每次多少」是流式递进冻结的**全部外部表现**：金额对不对、
	// 检查点落在哪、以及「中断之后有没有再多冻一笔」都在这一份记录里。
	added []addCall
	// onAdd 非 nil 时由它决定这次追加的结果；nil 表示成功。
	//
	// 用回调而不是一个按次序的错误列表：用例要判的是「冻到第二段的时候余额
	// 不够」，而按金额或按累计量判比数「第几次调用」稳——那个数会随实现里
	// 检查点的位置变，于是用例会在一次无害的重构之后开始测别的东西。
	onAdd func(pipeline.Request, money.Amount) error

	released []releaseCall
	// releaseCtxErr 记下每次释放时传进来的 ctx 是不是已经不可用。
	//
	// 这一条必须被钉住：客户端断开时请求的 ctx 已经 Done，用那个 ctx
	// 去释放等于没释放，而那笔钱会永久冻着。
	releaseCtxErr []error
	releaseErr    error
}

// addCall 是一次追加冻结额：给哪次请求追加了多少。
type addCall struct {
	req   pipeline.Request
	delta money.Amount
}

// AddReservation 记下一次追加，并按 onAdd 给出结果。
//
// 它**不改**已经保存的那份 Reservation：真实实现改的是库里的两处（钱包与账单
// 行），而内存里那份由调用方自己推进（见 streamFreeze.observe）。假实现跟着改
// 的话，「调用方忘了推进」这件事就测不出来了——而那正是最要紧的一半：结算读的
// 是内存里那份。
func (f *fakeReservations) AddReservation(_ context.Context, req pipeline.Request, delta money.Amount, outputTokens int64) error {
	f.added = append(f.added, addCall{req: req, delta: delta})
	if f.onAdd != nil {
		return f.onAdd(req, delta)
	}
	return nil
}

func (f *fakeReservations) SaveReservation(_ context.Context, _ pipeline.Request, r *pipeline.Reservation) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.next++
	// 存**副本**而不是那个指针：流式那条路上网关会在流中间就地推进这份预留
	// （补冻要加金额与「已经冻了多少个 token」，见 streamFreeze），而这一份
	// 记录要回答的是「开始之前冻了多少」——那是递进冻结最要紧的一个断言
	// （第一段），被后来那些补冻盖掉就什么都断言不了了。落库那一刻的值就是
	// 库里那一行刚插进去时的值，副本与它同形。
	//
	// 最终值（结算用的那个数）从别处拿：它就在 Charge.Settlement.Reserved 上。
	snapshot := *r
	f.saved = append(f.saved, &snapshot)
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
	// onSave 非 nil 时在每次落库前调一次。
	//
	// 它让用例能观察到「结算」与别的事件（记录健康度）的先后。先后错了
	// 不会让任何金额出错：延迟那一维只是少一个样本，而它要等下一次调用
	// 才补进去。
	onSave func()
}

func (f *fakeSettlements) Save(ctx context.Context, req pipeline.Request, c *pipeline.Charge) error {
	// 与真实落库层同形：过期的 ctx 让这次写失败（驱动层就是这么报的）。
	//
	// 少了这一条，夹具比真实存储层宽容，「收尾的预算已经过期」那条路就测不
	// 出来——而那正是 #529：失败的方向是释放，钱一分没收却什么都不报。
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.onSave != nil {
		f.onSave()
	}
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
	// sentRefs 是交给执行器的那几份请求的**指针**。
	//
	// 按值记不下这一点：快照该采的是发出去的那一个对象，而不是一份内容相同
	// 的副本，而这两者在按值比较时一样（见 snapshot_request_test.go）。
	sentRefs []*channel.Request
}

func (f *fakeExecutor) Chat(_ context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	f.calls = append(f.calls, ch.ID)
	f.sentRefs = append(f.sentRefs, req)
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

// cancelingExecutor 在每次调用**返回之前**取消 ctx。
//
// 取消放在返回之前，路由下一次转圈才会命中 ctx.Err()——那正是
// 「已经试过一个候选之后才被取消」的窗口（预先取消的 ctx 进不去：那时
// 一次调用都不会发生）。它对非流式与流式两条路都这样。
type cancelingExecutor struct {
	cancel context.CancelFunc
	inner  *fakeExecutor
}

func (c *cancelingExecutor) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	resp, err := c.inner.Chat(ctx, ch, req)
	c.cancel()
	return resp, err
}

func (c *cancelingExecutor) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	events, err := c.inner.ChatStream(ctx, ch, req)
	c.cancel()
	return events, err
}

type fakeRedactor struct {
	reqErr  error
	respErr error
	mu      sync.Mutex
	// responses 按顺序记下交给响应方向脱敏的每一份响应。
	//
	// 它钉的是「归一对不对」：流式那条路手里只有事件流，交给脱敏器的
	// 那份 channel.Response 是网关自己拼的，拼错了不会报错，只会让快照
	// 少一块或多一块。
	responses []*channel.Response
	// requests 按顺序记下交给请求方向脱敏的每一份请求。
	//
	// 它钉的是「采的是哪一份」：发出去的那份与调用方发来的那份不是同一个
	// 对象（见 internal/gateway 的 snapshotRequest）。
	requests []*channel.Request
	// identities 按顺序记下与上面几份请求配着交出去的身份。
	//
	// 它钉的是「判定拿得到身份」：脱敏器按身份决定留骨架还是留原文，身份
	// 传空了不会报错，只会让授权永远匹配不上——那正是默认不采集这个方向，
	// 所以它错得很安静。两条路各记一次（请求方向与响应方向各判一次）。
	identities []CaptureIdentity
}

func (f *fakeRedactor) Redact(_ context.Context, req *channel.Request, id CaptureIdentity) (*Snapshot, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.identities = append(f.identities, id)
	f.mu.Unlock()
	if f.reqErr != nil {
		return nil, f.reqErr
	}
	return &Snapshot{Skeleton: map[string]any{"model": "placeholder"}}, nil
}

func (f *fakeRedactor) RedactResponse(_ context.Context, resp *channel.Response, id CaptureIdentity) (*Snapshot, error) {
	f.mu.Lock()
	f.responses = append(f.responses, resp)
	f.identities = append(f.identities, id)
	f.mu.Unlock()
	if f.respErr != nil {
		return nil, f.respErr
	}
	return &Snapshot{}, nil
}

// RedactUpstreamError 交给真正的允许表：那些用例要验的是网关有没有把
// 上游错误体交对、并进骨架的方式对不对，不是再模拟一遍脱敏。
func (f *fakeRedactor) RedactUpstreamError(body string) (map[string]any, []string) {
	return snapshot.UpstreamErrorSkeleton(body)
}

// seenIdentities 返回交给脱敏器的身份清单。
func (f *fakeRedactor) seenIdentities() []CaptureIdentity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CaptureIdentity(nil), f.identities...)
}

// responseBodies 返回交给响应方向脱敏的响应体清单。
func (f *fakeRedactor) responseBodies() []*channel.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*channel.Response(nil), f.responses...)
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

// byDirection 返回某个方向的全部快照，按落库顺序。
//
// 一次请求会落两条（请求方向与响应方向），所以「那一条快照」必须按方向
// 找，不能取第一条。落库是异步的，取用前先用 waitFor 等它出现。
func (f *fakeSnapshots) byDirection(dir string) []*Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Snapshot
	for _, s := range f.saved {
		if s.Direction == dir {
			out = append(out, s)
		}
	}
	return out
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

func (identityAliases) Alias(_ context.Context, _ int64, canonical string) (string, map[string]any, error) {
	return canonical, nil, nil
}

// fixedAliases 不管渠道、一律把规范名翻成同一个上游名。
//
// 只用来分辨「网关有没有把选路结果里的上游名搬到结算请求上」——按渠道配
// 不同的名字是路由层那条用例的事。
type fixedAliases struct{ upstream string }

func (f fixedAliases) Alias(_ context.Context, _ int64, _ string) (string, map[string]any, error) {
	return f.upstream, nil, nil
}

// identityOverrides 是「一个渠道上都没有参数覆盖」的覆盖来源。
//
// 与 identityAliases 分开：那两个是 route.ModelAliases，只要求 Alias 一个
// 方法；预留上界那一路要的是 OverridesFor（见 Gateway.AliasOverrides）。
// 用了同一个结构体就得给两个接口各加一个方法，而两个方法各自代表的东西不同
// ——一个是「名字怎么翻」，一个是「上限是多少」。
type identityOverrides struct{}

func (identityOverrides) OverridesFor(_ context.Context, _ string, _ []int64) (map[int64]map[string]any, error) {
	// 空 map 而不是 nil：两个都表示「没有渠道配了覆盖」，而 nil 会让 nilnil
	// 这条 linter 报「同时回了一个 nil error 和一个无效值」——那句话在
	// 「一个都查不到」这个正常结论上也是对的，值得按它的建议写。
	return map[int64]map[string]any{}, nil
}

type harness struct {
	gw               *Gateway
	rules            *fakeRules
	candidates       *fakeCandidates
	reservations     *fakeReservations
	settlements      *fakeSettlements
	executor         *fakeExecutor
	redactor         *fakeRedactor
	decisionRedactor *fakeDecisionRedactor
	snapshots        *fakeSnapshots
	releaseFailures  *fakeReleaseFailures
	recorder         *fakeRecorder
	// aliases 是选路用的别名来源，默认按同名回落。
	aliases route.ModelAliases
	// aliasOverrides 是预留上界用的覆盖来源，默认什么也不覆盖。
	//
	// 与 aliases 分成两个字段而不是共用一个：它们的接口不同（一个要 Alias、
	// 一个要 OverridesFor），而共享一个字段就得让两个假实现同时认下两件事
	// ——那是把两个概念塞进一个类型里，正是这两条路径分开声明接口要避免的。
	aliasOverrides AliasOverrideSource
	// maxOutputTokens 是单次输出上限的全局默认值（见 Gateway.MaxOutputTokens）。
	//
	// 默认取 65536：**远高于**下面夹具里的 max_tokens，所以绝大多数用例走的仍是
	// 「没超限」那条路——那些用例钉的是别的事，不该被这条判据绊住。要钉这条
	// 判据的用例自己把它压下来（见 outputcap_test.go 的 withMaxOutputTokens）。
	maxOutputTokens int64

	// streamFreezeStep 是流式递进冻结的阈值段（见 Gateway.StreamFreezeStep）。
	//
	// 默认取 4096，与 settings 里的默认值一致。夹具里 request() 声明的
	// max_tokens 是 500，**小于**这个段——于是初始预留就冻满了整笔，一次补冻
	// 都不会发生：那些用例原本钉的就是「按声明上界冻满」这条路，换成递进冻结
	// 不该把它们的行为改掉。要钉递进冻结自己的用例把段压到 max_tokens 以下
	// （见 stream_test.go 的 withStreamFreezeStep）。
	streamFreezeStep int64

	// settleTimeout 是流式收尾的预算（见 Gateway.SettleTimeout）。
	//
	// 零值是**默认值**：表示用 streamSettleTimeout 那个常量。要验「一条跑得比
	// 收尾预算久的流」的用例才把它压到毫秒级（见 stream_test.go 的
	// withSettleTimeout）——不压的话那条用例要真等十秒才走得到收尾。
	settleTimeout time.Duration
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
			Usage: channel.Usage{
				PromptTokens: 1000, CompletionTokens: 100,
				// Reported 是「上游确实报了 usage 块」这一事实的显式位，
				// 由 internal/relay 在解析到 usage 时填（见 usageSourceOf）。
				// 夹具少了它就等于「上游没报」，用量来源会落成 Unreported
				// ——那是另一个场景的夹具。
				Reported: true,
			},
		}},
		redactor:         &fakeRedactor{},
		decisionRedactor: &fakeDecisionRedactor{},
		snapshots:        &fakeSnapshots{},
		releaseFailures:  &fakeReleaseFailures{},
		recorder:         &fakeRecorder{},
		aliases:          identityAliases{},
		aliasOverrides:   identityOverrides{},
		maxOutputTokens:  65536,
		streamFreezeStep: 4096,
	}
	for _, o := range opts {
		o(h)
	}

	h.gw = &Gateway{
		Pipeline: &pipeline.Pipeline{
			Rules:    h.rules,
			Programs: expr.NewCache(16),
		},
		Router:         &route.Router{Rand: rand.New(rand.NewSource(1)), Aliases: h.aliases},
		AliasOverrides: h.aliasOverrides,
		// 单次输出上限的全局默认值，见 harness.maxOutputTokens。
		MaxOutputTokens: h.maxOutputTokens,
		// 流式递进冻结的阈值段，见 harness.streamFreezeStep。
		StreamFreezeStep: h.streamFreezeStep,
		// 流式收尾的预算，见 harness.settleTimeout。
		SettleTimeout:    h.settleTimeout,
		Candidates:       h.candidates,
		Reservations:     h.reservations,
		Settlements:      h.settlements,
		Executor:         h.executor,
		Redactor:         h.redactor,
		DecisionRedactor: h.decisionRedactor,
		Snapshots:        h.snapshots,
		ReleaseFailures:  h.releaseFailures,
		Recorder:         h.recorder,
		Now:              func() time.Time { return fixedAt },
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
// 五类失败**分开钉**，因为它们在两列上的落法不同——只钉其中一类的话，
// 把另外几类也写成同一个状态码同样能通过，而那两个数会是编出来的：
//
//   - 非 2xx：状态码就是上游回的那个
//   - 拿到了 2xx 但响应不可用：状态码照实记（通常是 200），分类另有一档
//   - 一个响应都没拿到（连不上）：状态码必须留空
//     （HTTP 里没有 0 这个状态码）
//   - 上游接受了请求却没吐字：状态码照实记（适配器在流的第一条事件里
//     交下了那个 2xx），分类是另一档；没有 start 事件时留空
//   - 不是上游的失败：两列都留空（插件阻断时上游其实是好的）
//
// 变异记录：上面那两条「接受之后没吐字」的行一起把 `aborted.status > 0` 的边界
// 钉住了——那是 upstreamEvidence 里唯一一处看 streamAbortedError 自己的状态的
// 判断。改成 `>= 0` 是**等价变异**：多出来的一格是状态码为 0，而把它照实写出去
// 得到的正是 (0, ErrorUpstreamStreamAborted)，与下面那一支回落时逐值相同；
// 状态码为负数时 `>= 0` 同样为假，两版都走回落支。唯一分得开的是**这一支有没有
// 被走**，而那不改变返回值。
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
			fail:       &channel.ErrUpstream{StatusCode: 400, GotResponse: true, Retryable: false},
			wantStatus: 400,
			wantCode:   pipeline.ErrorUpstreamHTTPError,
		},
		{
			name:       "上游回了 500",
			fail:       &channel.ErrUpstream{StatusCode: 500, GotResponse: true, Retryable: true},
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
			// 网络错误也可能带上一点文本（例如底层读错误）。**有 Body 不等于
			// 拿到过响应**：分类只看 GotResponse，不看 Body 有没有内容。
			// 这一条钉的正是「不从 Body 反推」。
			name:     "网络错误带了说明文本也不算拿到响应",
			fail:     &channel.ErrUpstream{Retryable: true, Body: "read: connection reset by peer", Err: errors.New("read: connection reset by peer")},
			wantCode: pipeline.ErrorUpstreamNoAnswer,
		},
		{
			// 上游回了 2xx，却把错误写在体里（适配器的 extractEmbeddedError）。
			// 这是一个**拿到过响应**的失败：状态码 200 是真的，要照实记下来，
			// 分类是「响应不可用」而不是「没拿到回答」。
			name:       "上游 2xx 但体里带 error",
			fail:       &channel.ErrUpstream{StatusCode: 200, GotResponse: true, Body: "upstream said: model not found"},
			wantStatus: 200,
			wantCode:   pipeline.ErrorUpstreamUnusableResponse,
		},
		{
			// 同一个「拿到了但用不了」的分支的另一半：体根本解不开。与上一条
			// 归在同一档，因为它们对下游是同一件事——响应到了，但不是回答。
			name:       "上游 2xx 但响应体解不开",
			fail:       &channel.ErrUpstream{StatusCode: 200, GotResponse: true, Retryable: true, Err: errors.New("invalid character")},
			wantStatus: 200,
			wantCode:   pipeline.ErrorUpstreamUnusableResponse,
		},
		{
			// 上游接受了请求、一个 token 都没吐就断了（流式）。它与「没拿到
			// 响应」那两条的差别是**成因不同**：这里拿到了 2xx 响应头、
			// 正文一个字都没有。适配器在流的第一条事件里交下了状态码，
			// 这一档要把它照实记下来。
			//
			// 单独一条钉着，是因为这一支是 upstreamEvidence 里唯一一个不看
			// ErrUpstream 的分支——把它删掉的话，这条路上的依据会整个丢掉
			// （两列都 NULL），而库里 NULL 与「这一批恰好没有失败」长得一样。
			name:       "上游接受请求后没吐字就断了",
			fail:       &streamAbortedError{status: 200},
			wantStatus: 200,
			wantCode:   pipeline.ErrorUpstreamStreamAborted,
		},
		{
			// 同一个结局，但事件流里没有 start 事件（例如脚本执行器）：
			// 编不出状态码就留空，不写一个假的 200。
			name:     "流在接受之后断了，但没带状态码",
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

			// 「打到了哪个渠道、用哪个上游名」跟着释放一起交下去。
			//
			// 六类失败**都**打到过渠道 1——包括插件阻断那一次，它上游其实
			// 成功了、钱也花了。这两列回答的是「某个渠道是不是对某批模型
			// 全在报错」，而那正是失败行上原本缺的那一半：只有 status 与
			// error_code 时，看得出「为什么失败」，看不出「打的是谁」。
			//
			// 夹具用的是同名别名，所以这里只钉得住「有没有从选路结果搬到
			// 请求上」；「搬的是别名翻出来的那个名字、不是规范名」由端到端
			// 那条钉（见 internal/assembly 的 TestUpstreamFailureRecordsChannelAndModel）。
			rel := h.reservations.released[0].req
			if rel.ChannelID != 1 {
				t.Errorf("释放时带的 channel_id = %d，期望 1", rel.ChannelID)
			}
			if rel.UpstreamModel != "gpt-4o" {
				t.Errorf("释放时带的 upstream_model = %q，期望 %q", rel.UpstreamModel, "gpt-4o")
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

	// 同一个道理，另外两列也留空：**一个候选都没选中**，没有渠道可记。
	// 兜一个候选进去会让「这个渠道在报错」这个结论指向一个没被碰过的渠道，
	// 而那种错在库里看不出来（那一行看起来只是「记了」）。
	rel := h.reservations.released[0].req
	if rel.ChannelID != 0 {
		t.Errorf("一次上游调用都没发生，channel_id 应当是 0（落库成 NULL），得到 %d", rel.ChannelID)
	}
	if rel.UpstreamModel != "" {
		t.Errorf("一次上游调用都没发生，upstream_model 应当是空（落库成 NULL），得到 %q", rel.UpstreamModel)
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

// 客户端在**一次真实的调用之后**才断开时，释放仍要带上那次调用打到的渠道。
//
// 与 TestReleaseWithoutUpstreamCallCarriesNoEvidence 是同一个问题在两个结局上的
// 答案：那一条是「一次调用都没发生」，这一条是「发生过一次」。
//
// 取消由执行器在返回之前触发，于是路由下一次转圈才看见 ctx 结束——预先取消
// 的 ctx（见上一条）进不了这个窗口。两列与依据由同一份选路结果取出（见
// placementOf），所以只要路由交回那条尝试，释放这边就一定有它。
func TestReleaseAfterCancelKeepsAttemptedChannel(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.gw.Executor = &cancelingExecutor{
		cancel: cancel,
		inner: &fakeExecutor{failFor: map[int64]error{
			1: &channel.ErrUpstream{StatusCode: 500, GotResponse: true, Retryable: true},
		}},
	}
	// 两个候选：第一个失败后路由转圈、看见 ctx 已取消，不再试第二个。
	h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}

	if _, err := h.gw.Chat(ctx, request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("ctx 已取消时不该成功")
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("ctx 已取消时仍应释放预留，实际 %d 次", len(h.reservations.released))
	}

	// 那次调用真的发出去了，所以它打到的渠道与上游名都要落下来——NULL
	// 会让这一行读成「一个候选都没选中」。
	rel := h.reservations.released[0].req
	if rel.ChannelID != 1 {
		t.Errorf("释放时带的 channel_id = %d，期望刚试过的那一条（1）", rel.ChannelID)
	}
	if rel.UpstreamModel != "gpt-4o" {
		t.Errorf("释放时带的 upstream_model = %q，期望 %q", rel.UpstreamModel, "gpt-4o")
	}

	// 依据（upstream_status / error_code）留空：它由**结束这次请求的那个错误**
	// 推出来（见 upstreamEvidence），而这里是 context.Canceled——结束它的是
	// 断开，不是上游那一次 500。这两列回答的是「为什么不计费」，写成上游 500
	// 会让事后统计把客户端断开记到上游头上。
	//
	// 与上面两列的差别要说清楚：渠道与上游名记的是「打到了哪儿」，那一次
	// 调用真的发生过；依据记的是「这一请求为什么收尾」，两者不是一回事。
	if got := h.reservations.released[0].failure; got != nil {
		t.Errorf("客户端断开不是上游失败，不该带依据，得到 %+v", got)
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
//
// 两个方向都要钉：只设请求方向的错误而让响应方向照常落库的话，
// 「一条都没落下」这句话就证明不了请求那一条是被丢掉的。
func TestRedactionFailureDropsSnapshot(t *testing.T) {
	h := newHarness(t)
	h.redactor.reqErr = errors.New("脱敏器出错")
	h.redactor.respErr = errors.New("脱敏器出错")

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
	if n := h.snapshots.count(); n != 0 {
		t.Errorf("没配脱敏器时不该落任何快照，实际 %d 条", n)
	}
}

// ── 为什么这里不再有「采集在调上游之前」那条用例 ─────────────────────
//
// 请求快照的采集点跟着选路走，因此**在调上游之后**。原先这里钉的是「采集发生
// 在调上游之前」（借 countingRedactExecutor 数那次调用时的脱敏次数），那时快照
// 采的就是调用方发来的那份，所以采得早与采得对是同一件事。别名上的参数覆盖生效
// 之后两者不再是同一件事：要记的是**发出去的那份**请求（上游名翻过、覆盖落上去
// 了），而它只有选路之后才存在。
//
// 判据因此从「什么时候采」换成「采的是哪一个对象」，逐条钉在
// snapshot_request_test.go：成功、失败、换渠道之后的最后一次尝试、一份都没发
// 出去，以及流式那条路。这里不再留一条只钉次序的用例——次序不再是要求。

// 只接了脱敏器、没接落库层：脱敏这一步都不该走。
//
// 两个方向各有一条守卫（见 snapshotAsync 与 responseSnapshotAsync），而它们
// 此前只在「一个都没接」的装配下被走过。判据是脱敏器有没有被调用，而不是
// 「有没有落库」——两种装配都不落库，差别在守卫让不让往下走：往下走的那条路
// 会去调一个 nil 落库层，而那发生在后台 goroutine 里，不会让请求失败，只会
// 让测试进程崩掉。
//
// 脱敏器故意配成会报错的：守卫万一失效，报出来的是下面的断言，而不是一次
// panic——后者还得回头看才知道是哪一处。
func TestSnapshotSkippedWithoutStore(t *testing.T) {
	h := newHarness(t)
	h.gw.Snapshots = nil
	h.redactor.reqErr = errors.New("没接落库层时不该脱敏")
	h.redactor.respErr = errors.New("没接落库层时不该脱敏")

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 脱敏是同步的（只有落库在后台，见 snapshotAsync），所以两个方向的清单
	// 在 Chat 返回时已经有了结论。
	if got := len(h.redactor.requests); got != 0 {
		t.Errorf("请求方向脱敏 %d 次，期望 0（没接落库层）", got)
	}
	if got := len(h.redactor.responseBodies()); got != 0 {
		t.Errorf("响应方向脱敏 %d 次，期望 0（没接落库层）", got)
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

	var snaps []*Snapshot
	waitFor(t, "请求快照落库", func() bool {
		snaps = h.snapshots.byDirection("request")
		return len(snaps) == 1
	})
	if snaps[0].UserID != req.UserID {
		t.Errorf("快照的 user_id = %d，期望 %d（发起这次请求的用户）",
			snaps[0].UserID, req.UserID)
	}
}

// 脱敏器要拿到「这次请求是谁发的」——判据是身份真的传到了它手上。
//
// # 为什么这条值得单独钉
//
// 身份传成零值不会让任何东西报错：它只让授权匹配不上，而「匹配不上」正是
// 默认不采集那个方向，看起来完全正常。等到有人开了授权却发现原文没留下时，
// 现场已经离这里很远了。
//
// 两个方向各判一次（脱敏器要分别决定这一行留什么），所以两次都要带身份。
// 请求标识按真实装配的做法放进 context——用观测层那个键，也就是 requestIDOf
// 唯一读得到的那个。
func TestSnapshotPassesIdentityToRedactor(t *testing.T) {
	h := newHarness(t)
	req := request()
	ctx := observe.WithRequestID(context.Background(), observe.RequestID("trace-1"))

	if _, err := h.gw.Chat(ctx, req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatal(err)
	}

	ids := h.redactor.seenIdentities()
	if len(ids) != 2 {
		t.Fatalf("交给脱敏器的身份有 %d 份，期望 2 份（两个方向各判一次）", len(ids))
	}
	for i, id := range ids {
		if id.UserID != req.UserID {
			t.Errorf("第 %d 次判定拿到的 user_id = %d，期望 %d",
				i+1, id.UserID, req.UserID)
		}
		if id.RequestID != "trace-1" {
			t.Errorf("第 %d 次判定拿到的 request_id = %q，期望 %q",
				i+1, id.RequestID, "trace-1")
		}
	}
}

// ── 响应方向的快照 ──────────────────────────────────────────────────────

// 一次成功的非流式请求要落**两行**：请求方向与响应方向。
//
// 两行是配对读的：前者回答「发出去的是什么」，后者回答「回来的是什么」。
// 只有一行的话，`direction = 'response'` 这个枚举值在表里等于不存在。
func TestChatRecordsResponseSnapshotOnSuccess(t *testing.T) {
	h := newHarness(t)
	h.executor.resp.StatusCode = http.StatusOK

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	// 两个方向各落一条，而它们是**各自异步写**的（见 Gateway.snapshotAsync）。
	// 只等响应那一条、转头就数请求那一条，会在请求慢一步的那次数到 0——
	// 所以这里等两个方向都到，再逐个断言（与 decide_test 里同一个写法）。
	waitFor(t, "两个方向的快照都落库", func() bool {
		return h.snapshots.count() == 2
	})
	if got := len(h.snapshots.byDirection("request")); got != 1 {
		t.Errorf("请求方向的快照 = %d 条，期望 1", got)
	}
	snaps := h.snapshots.byDirection("response")
	if len(snaps) != 1 {
		t.Fatalf("响应方向的快照 = %d 条，期望 1", len(snaps))
	}
	snap := snaps[0]
	if snap.UpstreamStatus == nil || *snap.UpstreamStatus != http.StatusOK {
		t.Errorf("upstream_status = %v，期望 %d（上游真的回了这个码）", snap.UpstreamStatus, http.StatusOK)
	}
	if snap.ErrorKind != nil {
		t.Errorf("成功时 error_kind 应为 nil，实际 %q", *snap.ErrorKind)
	}
	if snap.UserID != request().UserID {
		t.Errorf("响应快照的 user_id = %d，期望 %d", snap.UserID, request().UserID)
	}
}

// 上游回了 4xx：仍然要落一行响应方向的快照，且两列都说得出发生了什么。
//
// 这一条正是「没拿到」与「拿到了但不是 2xx」的分界用例之一：状态码是
// 上游真的回的那个，分类是非 2xx 那一档。
func TestResponseSnapshotOnUpstreamHTTPError(t *testing.T) {
	h := newHarness(t)
	h.executor.err = &channel.ErrUpstream{
		StatusCode:  http.StatusForbidden,
		GotResponse: true,
		Body:        "forbidden",
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游 403 应当让请求失败")
	}

	var snaps []*Snapshot
	waitFor(t, "失败也落响应方向的快照", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	snap := snaps[0]
	if snap.UpstreamStatus == nil || *snap.UpstreamStatus != http.StatusForbidden {
		t.Errorf("upstream_status = %v，期望 %d", snap.UpstreamStatus, http.StatusForbidden)
	}
	if snap.ErrorKind == nil || *snap.ErrorKind != "upstream_http_error" {
		t.Errorf("error_kind = %v，期望 upstream_http_error", snap.ErrorKind)
	}
	// 没有归一化的响应体，骨架是空的；两列才是这一行要说的东西。
	if len(snap.Skeleton) != 0 {
		t.Errorf("失败行不该凭空造一个骨架，实际 %v", snap.Skeleton)
	}
}

// 连不上上游：一个 HTTP 响应都没拿到，状态码必须是 NULL。
//
// 这一条钉的是「没拿到」：把它写成 0 或写成某个编出来的 5xx，都会让
// 前一条用例的分界失效。
func TestResponseSnapshotWhenNoUpstreamAnswer(t *testing.T) {
	h := newHarness(t)
	h.executor.err = &channel.ErrUpstream{
		GotResponse: false,
		Err:         errors.New("connection refused"),
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("连不上上游应当让请求失败")
	}

	var snaps []*Snapshot
	waitFor(t, "失败也落响应方向的快照", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	if snaps[0].UpstreamStatus != nil {
		t.Errorf("连不上时 upstream_status 必须是 nil（HTTP 里没有 0 号），实际 %d", *snaps[0].UpstreamStatus)
	}
	if snaps[0].ErrorKind == nil || *snaps[0].ErrorKind != "upstream_no_answer" {
		t.Errorf("error_kind = %v，期望 upstream_no_answer", snaps[0].ErrorKind)
	}
}

// 非流式、上游回了 2xx 但响应不可用：两表要对同一个值。
//
// 这一条是 #461 那两条用例的非流式对照。流式那两条走的是 pumpStream 的
// 三个闸门，非流式走 responseEvidence，两处判据不同——所以「流式对得上」
// 推不出「非流式也对得上」，这里单独钉一次。
//
// 这一条今天就是绿的（非流式两侧本来就调同一个 upstreamEvidence）。留着
// 是为了让以后改任一侧的人先看到它红，而不是等到库里两表分岔了才发现。
func TestNonStreamUnusableResponseClassifiedSameOnBothSides(t *testing.T) {
	h := newHarness(t)
	// 200 是真的（上游确实回了响应），但体里是错误：拿到的不是回答。
	h.executor.err = &channel.ErrUpstream{
		StatusCode:  http.StatusOK,
		GotResponse: true,
		Body:        "upstream said: model not found",
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游 200 但体里是 error 应当让请求失败")
	}

	if got := len(h.reservations.released); got != 1 {
		t.Fatalf("释放表 = %d 条，期望 1 条", got)
	}
	fail := h.reservations.released[0].failure
	if fail == nil {
		t.Fatal("这一档失败必须带依据（两列都 NULL 与「这一批没有失败」在库里长得一样）")
	}
	if fail.Code != pipeline.ErrorUpstreamUnusableResponse {
		t.Errorf("账单那一侧的分类 = %q，期望 %q", fail.Code, pipeline.ErrorUpstreamUnusableResponse)
	}

	var snaps []*Snapshot
	waitFor(t, "失败也落响应方向的快照", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	snap := snaps[0]
	if snap.UpstreamStatus == nil || *snap.UpstreamStatus != http.StatusOK {
		t.Errorf("upstream_status = %v，期望 %d（上游真的回了这个码）", snap.UpstreamStatus, http.StatusOK)
	}
	if snap.ErrorKind == nil || *snap.ErrorKind != pipeline.ErrorUpstreamUnusableResponse {
		t.Errorf("快照的 error_kind = %s，期望 %q（与账单同一个值）",
			kindOf(snap.ErrorKind), pipeline.ErrorUpstreamUnusableResponse)
	}
}

// 上游回来的状态码落在哪一档：2xx 与非 2xx 的分界在比较符上。
//
// 300 是重定向——它既不是「上游给了一个可用的回答」，也不是「上游没给出回答」。
// 今天它落在「上游回了错误状态码」那一档，与 4xx / 5xx 同一档；钱的处置不受
// 影响（两档都不计费），差别在快照与指标里记的是哪个结论。把这一档写下来，
// 将来谁挪动那个比较符，先看到的是一条红用例，而不是几张对不上的报表。
//
// 分类写成字面量而不是拿 pipeline 的常量来比：它是要落进快照、被人按字面读的
// 取值，改了就该有一条红用例。
func TestUpstreamEvidenceStatusBuckets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantStatus int
		wantKind   string
	}{
		{"200 拿到了响应但不可用", http.StatusOK, http.StatusOK, "upstream_unusable_response"},
		{"299 仍是 2xx", 299, 299, "upstream_unusable_response"},
		{"300 重定向：按非 2xx 记", http.StatusMultipleChoices, http.StatusMultipleChoices, "upstream_http_error"},
		{"404 上游回的错误码", http.StatusNotFound, http.StatusNotFound, "upstream_http_error"},
		{"500 也是", http.StatusInternalServerError, http.StatusInternalServerError, "upstream_http_error"},
		{"拿到响应却没给状态码：归到没拿到", 0, 0, "upstream_no_answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, kind := upstreamEvidence(&channel.ErrUpstream{
				StatusCode: tc.status, GotResponse: true,
			})
			if status != tc.wantStatus {
				t.Errorf("status = %d，期望 %d", status, tc.wantStatus)
			}
			if kind != tc.wantKind {
				t.Errorf("分类 = %q，期望 %q", kind, tc.wantKind)
			}
		})
	}
}

// 一次上游调用都没发生时不写响应方向的行。
//
// 插件在选路时阻断——这不是「上游回来了什么」，写一行方向为 response 的
// 空快照会让人以为上游回了个空。
func TestResponseSnapshotSkippedWithoutUpstreamCall(t *testing.T) {
	h := newHarness(t)
	blocked := errors.New("插件阻断")
	hooks := Hooks{OnRoute: func(context.Context, []route.Candidate) (int64, error) {
		return 0, blocked
	}}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), hooks); !errors.Is(err, blocked) {
		t.Fatalf("期望插件阻断的错误，实际 %v", err)
	}

	// 等请求方向那一条落下来，确保采集那一步已经跑过。
	waitFor(t, "请求方向的快照落库", func() bool {
		return len(h.snapshots.byDirection("request")) == 1
	})
	if got := len(h.snapshots.byDirection("response")); got != 0 {
		t.Errorf("没有上游调用时不该写响应方向的快照，实际 %d 条", got)
	}
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
	if res.Charge.UsageSource != tokens.SourceUnreported {
		t.Errorf("用量来源 = %v，期望 unreported（上游没报）", res.Charge.UsageSource)
	}
	if !res.Estimated {
		t.Error("上游没报用量时 Estimated 应为 true")
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
	if res.Estimated {
		t.Error("上游报过 usage 时 Estimated 应为 false")
	}
}

// 「上游报了 0」与「上游没报」要分开：两者用量逐列相同，只有来源不同。
//
// 上游报一个全 0 的 usage 时，那几个 0 是它给的数——可以拿去跟上游对账
// （measured）；整块不报时，零值是占位——不能对账（unreported）。
// 这条用例把两种响应的形状并排放在一起，判据只有一个：Usage.Reported
// 是不是真的——它由 relay 按「响应体里有没有 usage 块」填。
func TestUsageSourceDistinguishesReportedZeroFromUnreported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		usage  channel.Usage
		want   tokens.Source
		absent bool
	}{
		{
			name: "上游报了 0",
			usage: channel.Usage{
				PromptTokens: 0, CompletionTokens: 0,
				Reported: true,
			},
			want: tokens.SourceMeasured,
		},
		{
			name:   "上游没报 usage",
			usage:  channel.Usage{},
			want:   tokens.SourceUnreported,
			absent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.executor.resp = &channel.Response{
				UpstreamModel: "m",
				Usage:         tc.usage,
			}

			res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			// 两条路的金额与用量逐列相同——这正是「只看数字分不开」
			// 的证明，分开它们的只有来源。
			if !res.Charge.Settlement.Actual.IsZero() {
				t.Errorf("金额 = %s，期望零", res.Charge.Settlement.Actual)
			}
			if res.Charge.Usage.PromptTokens != 0 || res.Charge.Usage.CompletionTokens != 0 {
				t.Errorf("用量 = %+v，期望全零", res.Charge.Usage)
			}
			if res.Charge.UsageSource != tc.want {
				t.Errorf("用量来源 = %v，期望 %v", res.Charge.UsageSource, tc.want)
			}
			if res.Estimated != tc.absent {
				t.Errorf("Estimated = %v，期望 %v", res.Estimated, tc.absent)
			}
		})
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

// requestIDOf：观测层放进 context 的标识，这里取到的就是它。
//
// 断言的是**那一个具体的值**，不是「非空」：取值恒为空串的实现下两者都红，
// 而红在一起时分不出是「取错了另一个键」还是「压根取不到」。要钉的正是前一种
// ——两个同名的键类型各在一处，中间那座桥有没有接上，只有具体的值答得了。
func TestRequestIDOfTakesObserveValue(t *testing.T) {
	const id = "req-2f8a1c9d"

	ctx := observe.WithRequestID(context.Background(), observe.RequestID(id))
	if got := requestIDOf(ctx); got != id {
		t.Errorf("requestIDOf = %q，期望 %q", got, id)
	}
}

// 带工具的请求在插件之前就被目录判据筛掉了。
//
// 钉两件事：这条路上的判据来自**这份请求**（route.NeedsOf(upstream)），
// 以及插件看到的就是内核会执行的那一份候选。少了任何一件，插件与内核就
// 各自按不同的候选集做决定，而那种分叉表现为「插件选了一个渠道，请求却
// 发去了另一个」。
func TestRouteHooksSeeProfileFilteredCandidates(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		noTools := candidate(1, "gpt-4o")
		// 目录**明确声明**不支持工具（false）——与「没配过」（nil）不同：
		// 后者不参与判据，这条用例要的是前者。
		unsupported := false
		noTools.Profile = &route.ModelProfile{Tools: &unsupported}
		h.candidates.cands = []route.Candidate{noTools}
	})

	var seen []route.Candidate
	hooks := Hooks{OnRoute: func(_ context.Context, cands []route.Candidate) (int64, error) {
		seen = cands
		return 0, nil
	}}
	upstream := upstreamRequest()
	upstream.Tools = []channel.Tool{{Name: "get_weather"}}

	_, err := h.gw.Chat(context.Background(), request(), upstream, hooks)

	const want = "1 个候选渠道都不支持这次请求"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v，期望 %q", err, want)
	}
	if len(seen) != 0 {
		t.Errorf("插件看到的候选 = %+v，期望空", seen)
	}
	if len(h.executor.calls) != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", len(h.executor.calls))
	}
}
