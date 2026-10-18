package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/systemone"
	"github.com/sumwai/larkax/internal/tokens"
)

// fakeDecider 是决策那条路上的上游替身。
//
// 它记下**上游模型名**：那个值由路由层按选中的渠道翻出来，再经载体交给
// 执行器（见 decideCall.Chat）。翻没翻、翻对没翻对，只有看它才知道。
type fakeDecider struct {
	resp    *systemone.Response
	err     error
	failFor map[int64]error

	models   []string
	channels []int64
}

func (f *fakeDecider) Decide(_ context.Context, ch channel.Channel, req *systemone.Request) (*systemone.Response, error) {
	f.models = append(f.models, req.Model)
	f.channels = append(f.channels, ch.ID)
	if err, ok := f.failFor[ch.ID]; ok {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// decisionBilling 是决策这条路的计费输入。协议由入口按路径填
// （见 internal/api 的 decideBillingRequest），这里照着那个形状给。
func decisionBilling() pipeline.Request {
	req := request()
	req.Protocol = channel.ProtocolSystemOne
	return req
}

func decision() *systemone.Request {
	return &systemone.Request{
		Model: "gpt-4o",
		State: "工单: 用户说付款后没到账",
		Questions: map[string]systemone.Question{
			"q_noul": {Type: systemone.TypeNoul, Instructions: "这是账单问题吗"},
		},
	}
}

// decisionResponse 是上游会回的那份响应。用量取自实测的形状：一次含三个
// 问题的请求 input 387 / output 72。
func decisionResponse() *systemone.Response {
	return &systemone.Response{
		Model:   "gpt-4o-2024",
		Answers: map[string]systemone.Answer{"q_noul": {Type: systemone.TypeNoul, Noul: 0.75}},
		Usage:   systemone.Usage{InputTokens: 387, OutputTokens: 72, Reported: true},
	}
}

// newDecideHarness 在既有 harness 上接一个决策执行器。
func newDecideHarness(t *testing.T, d *fakeDecider, opts ...func(*harness)) *harness {
	t.Helper()
	h := newHarness(t, opts...)
	h.gw.Decider = d
	return h
}

// 正常路径：预留 → 选路 → 调上游 → 结算，用量与上游名都要落到账单上。
func TestDecideSucceeds(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d)

	res, err := h.gw.Decide(context.Background(), decisionBilling(), decision())
	if err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}

	if res.Response == nil || res.Response.Answers["q_noul"].Noul != 0.75 {
		t.Fatalf("交回来的响应不对: %+v", res.Response)
	}
	// 账单上记的是**这次实际发出去**的那个名字（选路翻出来的），与 chat
	// 那条路同一个来源。上游回报的名字留在 res.Response.Model 里，
	// 它是排查用的，不进账单。
	if res.UpstreamModel != "gpt-4o" {
		t.Errorf("UpstreamModel = %q，期望 gpt-4o（别名按同名回落）", res.UpstreamModel)
	}
	if res.Response.Model != "gpt-4o-2024" {
		t.Errorf("上游回报的模型名 = %q", res.Response.Model)
	}
	if len(d.models) != 1 || d.models[0] != "gpt-4o" {
		t.Errorf("适配器收到的模型名 = %v（别名来源按同名回落）", d.models)
	}

	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留数 = %d，期望 1", len(h.reservations.saved))
	}
	if len(h.settlements.saved) != 1 {
		t.Fatalf("结算数 = %d，期望 1", len(h.settlements.saved))
	}
	// 上游名要跟着账单落下去：结算那份请求是选路之后才组装的。
	if got := h.settlements.reqs[0].UpstreamModel; got != "gpt-4o" {
		t.Errorf("结算请求里的上游名 = %q", got)
	}
	// 用量按 input → prompt、output → completion 折算，两个数都要在。
	// 只断言「金额不为零」是不够的：两个桶对调之后金额总量照样对
	//（单价相同时完全看不出来），所以这里逐项看用量。
	charge := h.settlements.saved[0]
	if charge.Usage.PromptTokens != 387 || charge.Usage.CompletionTokens != 72 {
		t.Errorf("结算用量 = %+v，期望 prompt=387 completion=72", charge.Usage)
	}
	if charge.UsageSource != tokens.SourceMeasured {
		t.Errorf("用量来源 = %v，期望实测（来自上游的响应体）", charge.UsageSource)
	}
	if len(h.settlements.reqs) != 1 || h.settlements.reqs[0].Protocol != channel.ProtocolSystemOne {
		t.Errorf("账单上的协议 = %q，期望 %q",
			h.settlements.reqs[0].Protocol, channel.ProtocolSystemOne)
	}
}

// 上游没报 usage 时，决策那条路的来源也要记成 unreported。
//
// 这一条与 chat 那条路同一个判据（channel.Usage.Reported），所以它同时钉住
// 「internal/systemone 的解析没有把块在不在压平」这件事：夹具把 Reported
// 置成 false，用量却非零——只按数字判的话这里会落成 measured。
func TestDecideReportsUnreportedUsage(t *testing.T) {
	d := &fakeDecider{resp: &systemone.Response{
		Model:   "gpt-4o-2024",
		Answers: map[string]systemone.Answer{},
		Usage:   systemone.Usage{InputTokens: 387, OutputTokens: 72, Reported: false},
	}}
	h := newDecideHarness(t, d)

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}
	if got := h.settlements.saved[0].UsageSource; got != tokens.SourceUnreported {
		t.Errorf("用量来源 = %v，期望 %v（上游没有 usage 块）", got, tokens.SourceUnreported)
	}
}

// 上游名要按**选中的那个渠道**翻。载体里带的是规范名，适配器收到的必须是
// 翻过之后的名字——不翻就是把规范名发给上游。
func TestDecideResolvesUpstreamNamePerChannel(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d, func(h *harness) {
		h.aliases = fixedAliases{upstream: "vendor/decision-model"}
	})

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}
	if len(d.models) != 1 || d.models[0] != "vendor/decision-model" {
		t.Errorf("适配器收到的模型名 = %v，期望 vendor/decision-model", d.models)
	}
}

// 可重试的失败要换下一个候选，且只有最后成功的那一个进账单。
func TestDecideRetriesNextCandidate(t *testing.T) {
	d := &fakeDecider{
		resp: decisionResponse(),
		failFor: map[int64]error{1: &channel.ErrUpstream{
			StatusCode: 503, Retryable: true, GotResponse: true,
		}},
	}
	h := newDecideHarness(t, d, func(h *harness) {
		h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}
	})

	res, err := h.gw.Decide(context.Background(), decisionBilling(), decision())
	if err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}
	if res.Channel.ID != 2 {
		t.Errorf("选中的渠道 = %d，期望 2", res.Channel.ID)
	}
	if len(d.channels) != 2 {
		t.Errorf("调用次数 = %d，期望 2", len(d.channels))
	}
	if len(h.settlements.saved) != 1 {
		t.Errorf("结算数 = %d，期望 1（只有成功的那一次进账单）", len(h.settlements.saved))
	}
}

// 上游全失败：不计费，且预留要放开。
func TestDecideUpstreamFailureIsNotBilled(t *testing.T) {
	d := &fakeDecider{err: &channel.ErrUpstream{StatusCode: 500, Retryable: true}}
	h := newDecideHarness(t, d)

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err == nil {
		t.Fatal("期望报错")
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("失败却结算了 %d 次", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("释放次数 = %d，期望 1", len(h.reservations.released))
	}
}

// 决策那条路的不计费失败也要带上「打到了哪个渠道、用哪个上游名」。
//
// 它与 Chat 走的是同一个收尾出口，但入口是另一条：漏了这一个，失败账单行
// 上那两列又是 NULL，而库里的 NULL 与「确实没有渠道可记」逐列相同。
func TestDecideUpstreamFailureCarriesPlacementToRelease(t *testing.T) {
	d := &fakeDecider{err: &channel.ErrUpstream{StatusCode: 500, Retryable: true}}
	h := newDecideHarness(t, d)

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err == nil {
		t.Fatal("期望报错")
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("释放次数 = %d，期望 1", len(h.reservations.released))
	}
	rel := h.reservations.released[0].req
	if rel.ChannelID != 1 {
		t.Errorf("释放时带的 channel_id = %d，期望 1", rel.ChannelID)
	}
	if rel.UpstreamModel != "gpt-4o" {
		t.Errorf("释放时带的 upstream_model = %q，期望 gpt-4o", rel.UpstreamModel)
	}
}

// 执行器成功却交回空响应时**报错，而不是解引用 nil 崩掉**。
//
// 今天不可达（唯一的实现自己检查过），留着是因为崩溃的现场是一次 panic：
// 调用方拿到的是断连，而账上那笔预留要靠 defer 才放开——一个能查的失败
// 比一次「服务重启了但不知道为什么」便宜得多。
func TestDecideRejectsNilResponse(t *testing.T) {
	h := newHarness(t)
	h.gw.Decider = deciderFunc(
		func(context.Context, channel.Channel, *systemone.Request) (*systemone.Response, error) {
			// 「成功（err 为 nil）却交回空响应」正是这条用例要造的形状，
			// 不是「出错了但没报」——所以这里显式 nolint。
			return nil, nil //nolint:nilnil // 见上
		})

	_, err := h.gw.Decide(context.Background(), decisionBilling(), decision())
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "没有交回响应") {
		t.Errorf("错误应说清是执行器没交回响应: %v", err)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("失败却结算了 %d 次", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("释放次数 = %d，期望 1", len(h.reservations.released))
	}
}

// 没有接上决策执行器时回 ErrNoExecutor，而不是 panic 或一个静默的 nil。
func TestDecideWithoutExecutor(t *testing.T) {
	h := newHarness(t)
	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("错误 = %v，期望 ErrNoExecutor", err)
	}
}

// fakeDecisionRedactor 是决策那条表面两个脱敏接口的替身。
//
// # 它记的是**足迹本身**，不是骨架
//
// 这一层要断言的是「网关交给脱敏器的判据对不对」：候选集合带没带上、
// 选中的是哪一个、降级经过记全了没有。骨架长什么样是另一层的事，
// 由 internal/snapshotwire 与 internal/snapshot 的用例管。
type fakeDecisionRedactor struct {
	mu       sync.Mutex
	traces   []*DecisionTrace
	outcomes []*DecisionOutcome
	err      error
}

func (f *fakeDecisionRedactor) RedactDecisionRequest(t *DecisionTrace) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traces = append(f.traces, t)
	if f.err != nil {
		return nil, f.err
	}
	return &Snapshot{Skeleton: map[string]any{"surface": "decision"}, Version: 1}, nil
}

func (f *fakeDecisionRedactor) RedactDecisionResponse(o *DecisionOutcome) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, o)
	if f.err != nil {
		return nil, f.err
	}
	return &Snapshot{Skeleton: map[string]any{"surface": "decision"}, Version: 1}, nil
}

func (f *fakeDecisionRedactor) firstTrace(t *testing.T) *DecisionTrace {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.traces) == 0 {
		t.Fatal("脱敏器没有收到请求方向的足迹")
	}
	return f.traces[0]
}

func (f *fakeDecisionRedactor) firstOutcome(t *testing.T) *DecisionOutcome {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.outcomes) == 0 {
		t.Fatal("脱敏器没有收到响应方向的足迹")
	}
	return f.outcomes[0]
}

// 决策这条路径**两个方向都采**：请求方向记「当时手上有哪些候选」，
// 响应方向记「实际选了谁、试过谁、为什么」。
//
// 它与 chat 那条路共用 request / response 两个方向取值，靠骨架里的 surface
// 标量区分（见 internal/snapshotwire）——所以这两行落在同一张表、同一对方向里，
// 与账单用同一个 request_id 关联。
func TestDecideCapturesBothDirections(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d)
	req := decisionBilling()

	if _, err := h.gw.Decide(context.Background(), req, decision()); err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}

	// 落库是异步的，先等两条都到。
	waitFor(t, "两个方向的快照都落库", func() bool { return h.snapshots.count() == 2 })
	if n := len(h.snapshots.byDirection("request")); n != 1 {
		t.Errorf("请求方向的快照数 = %d，期望 1", n)
	}
	if n := len(h.snapshots.byDirection("response")); n != 1 {
		t.Errorf("响应方向的快照数 = %d，期望 1", n)
	}
	// user_id 必须是**发起这次请求的用户**：这一列没有外键，填成商家或填 0
	// 都插得进去、不报错，所以只能断言真正落库的那个值。
	for _, s := range h.snapshots.saved {
		if s.UserID != req.UserID {
			t.Errorf("方向 %s 的快照 user_id = %d，期望 %d", s.Direction, s.UserID, req.UserID)
		}
	}

	// 请求方向交出去的是**候选集合**：这是这条表面的判据主体。
	trace := h.decisionRedactor.firstTrace(t)
	if trace.Model != req.CanonicalModel {
		t.Errorf("足迹里的模型名 = %q，期望 %q", trace.Model, req.CanonicalModel)
	}
	if len(trace.Candidates) == 0 {
		t.Fatal("请求方向的足迹没有候选集合——判据的主体就没了")
	}
	if trace.Candidates[0].ChannelID != 1 {
		t.Errorf("候选的 channel_id = %d，期望 1", trace.Candidates[0].ChannelID)
	}

	// 响应方向交出去的是「选了谁」。
	outcome := h.decisionRedactor.firstOutcome(t)
	if !outcome.Succeeded {
		t.Error("上游成功了，结局应当记成成功")
	}
	if outcome.SelectedChannelID != 1 {
		t.Errorf("选中的渠道 = %d，期望 1", outcome.SelectedChannelID)
	}
	if outcome.UpstreamModel != "gpt-4o" {
		t.Errorf("上游模型名 = %q，期望 gpt-4o（别名按同名回落）", outcome.UpstreamModel)
	}
	if outcome.Model != req.CanonicalModel {
		t.Errorf("结局里的模型名 = %q，期望 %q", outcome.Model, req.CanonicalModel)
	}
	if len(outcome.Candidates) == 0 {
		t.Error("响应方向也要带候选集合：否则「为什么不是另一个」要去 join 另一行")
	}
	if len(outcome.Attempts) != 1 || !outcome.Attempts[0].Succeeded {
		t.Errorf("尝试记录 = %+v，期望一条成功的", outcome.Attempts)
	}
}

// 换渠道重试的经过要逐条记下来，且每一条的失败分类与账单**同源**。
//
// 「降级到了哪一步、每一步为什么不成」是响应方向存在的理由；只记最后一条的话，
// 一次三次重试与一次直接失败在快照上长得一样。
func TestDecisionSnapshotRecordsDegradationPerAttempt(t *testing.T) {
	d := &fakeDecider{
		resp: decisionResponse(),
		failFor: map[int64]error{1: &channel.ErrUpstream{
			StatusCode: 503, Retryable: true, GotResponse: true,
		}},
	}
	h := newDecideHarness(t, d, func(h *harness) {
		h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}
	})

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}

	outcome := h.decisionRedactor.firstOutcome(t)
	if len(outcome.Attempts) != 2 {
		t.Fatalf("尝试记录 %d 条，期望 2（第一个失败、第二个成功）", len(outcome.Attempts))
	}
	first := outcome.Attempts[0]
	if first.Succeeded {
		t.Error("第一个候选回的是 503，不该记成成功")
	}
	if !first.Retryable {
		t.Error("503 值得换下一个候选，retryable 该是 true")
	}
	if first.UpstreamStatus == nil || *first.UpstreamStatus != 503 {
		t.Errorf("第一条尝试的状态码 = %v，期望 503", first.UpstreamStatus)
	}
	// 分类与账单上那一列同一套取值（见 upstreamEvidence）。
	if first.ErrorKind == nil || *first.ErrorKind == "" {
		t.Error("失败的那次尝试要有分类：降级原因靠它说清失败发生在哪一段")
	}
	if !outcome.Attempts[1].Succeeded {
		t.Error("第二个候选成功了，该记成成功")
	}
	if outcome.SelectedChannelID != 2 {
		t.Errorf("选中的渠道 = %d，期望 2", outcome.SelectedChannelID)
	}
}

// 一次尝试的耗时原样进足迹，负数才归零。
//
// 归零那一步管的是时钟回拨：负的耗时经「非负整数」那一档会变成一个脱敏
// 描述符，一条时钟抖动换来一行噪音。只断言负的那一格时，把比较符写反——正的
// 也压成 0——不会被发现，而那让延迟这一列等于没有：账单上记着花了多久，足迹
// 里却永远是零。
//
// 变异记录：`item.Duration < 0` 改成 `<= 0` 是**等价变异**——多出来的一格是
// 耗时恰好为 0，而那一支赋的也是 0，与不赋同值。第三个候选就是那一格：它进来
// 的耗时本来就是 0，两版给的都是 0，写在这里是为了让「这一格分不开」有个
// 落点，而不是只写在注释里。
func TestDecisionAttemptDurationKeepsPositive(t *testing.T) {
	out := decisionAttemptsOf([]route.Attempt{
		{ChannelID: 1, Duration: 1200 * time.Millisecond},
		{ChannelID: 2, Err: errors.New("上游在首片之前断开"), Retryable: true, Duration: -time.Second},
		// 耗时恰好为 0：同一个瞬间发了又回，或者夹具没给时长。
		{ChannelID: 3, Duration: 0},
	})
	if len(out) != 3 {
		t.Fatalf("足迹 %d 条，期望 3", len(out))
	}
	if got := out[0].Duration; got != 1200*time.Millisecond {
		t.Errorf("耗时 = %s，期望 1.2s（原样带出去，不归零）", got)
	}
	if got := out[1].Duration; got != 0 {
		t.Errorf("负耗时 = %s，期望 0（时钟回拨不写成负数）", got)
	}
	if got := out[2].Duration; got != 0 {
		t.Errorf("零耗时 = %s，期望 0（本来就对，不该被写成别的数）", got)
	}
}

// 被选路筛掉的候选要跟着响应方向的足迹一起交出去。
//
// # 它补的是哪一段
//
// 「配了三个渠道为什么只用一个」从前在留痕里看不出来：请求方向记的是选路
// **之前**有几个候选，attempts 记的是**真的发出去过**哪几个，而那两个没被
// 发出去的候选**为什么**不在候选里，没有任何一处说得出来——差额本身也只是一道
// 差额，看不出它是被选路筛掉的（配置问题：那几条渠道的声明或状态不对）还是
// 没轮到就被提前放弃的（选路那一步之外的事）。
//
// 分类钉成字面量而不是拿 route 的常量来比：这条记录是给排障的人读的，
// 取值改了就是一次对外的行为变化，用例该跟着变红（见 docs/development.md 的
// 「用常量测常量等于没测」）。
func TestDecisionSnapshotRecordsExcludedCandidates(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d, func(h *harness) {
		h.candidates.cands = []route.Candidate{
			candidate(1, "gpt-4o"),
			candidate(2, "gpt-4o"),
			candidate(3, "gpt-4o"),
		}
		// 渠道 2 连续失败 3 次，到了该被摘掉的阈值（见 route 的
		// consecutiveFailureCutoff）。
		h.candidates.cands[1].Health.ConsecutiveFailures = 3
	})

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}

	outcome := h.decisionRedactor.firstOutcome(t)
	if len(outcome.Candidates) != 3 {
		t.Fatalf("候选集合 = %d 条，期望 3（选路之前那一份）", len(outcome.Candidates))
	}
	if len(outcome.Excluded) != 1 {
		t.Fatalf("排除记录 = %+v，期望 1 条", outcome.Excluded)
	}
	if got := outcome.Excluded[0].ChannelID; got != 2 {
		t.Errorf("被筛掉的渠道 = %d，期望 2", got)
	}
	if got := outcome.Excluded[0].Code; got != "health" {
		t.Errorf("排除分类 = %q，期望 health", got)
	}
	// 它真的被试过没有：被筛掉的候选不进尝试列表，这正是两者之和回答
	// 「三个候选怎么只用了一个」的地方。
	for _, a := range outcome.Attempts {
		if a.ChannelID == 2 {
			t.Errorf("被筛掉的渠道不该出现在尝试列表里: %+v", outcome.Attempts)
		}
	}
}

// 一个候选都没剩下时，被筛掉的那些也要落进留痕。
//
// 这一支里选路直接返回错误、不进 Result，所以排除记录只能从错误上取回来
// （见 route.Errors.Excluded）。它是**最该被看见**的一支：「这些渠道现在
// 一条都用不了」与「压根没配渠道」在留痕上从前长得一样。
func TestDecisionSnapshotRecordsExcludedWhenNothingLeft(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d, func(h *harness) {
		h.candidates.cands = []route.Candidate{
			candidate(1, "gpt-4o"),
			candidate(2, "gpt-4o"),
		}
		h.candidates.cands[0].Health.ConsecutiveFailures = 3
		h.candidates.cands[1].Health.ConsecutiveFailures = 3
	})

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err == nil {
		t.Fatal("一条候选都用不了时应当报错")
	}
	if len(d.channels) != 0 {
		t.Errorf("一个候选都没剩下时不该打到上游，实际打了 %v", d.channels)
	}

	outcome := h.decisionRedactor.firstOutcome(t)
	if outcome.Succeeded {
		t.Error("没选中任何渠道，不该记成成功")
	}
	if len(outcome.Excluded) != 2 {
		t.Fatalf("排除记录 = %+v，期望 2 条", outcome.Excluded)
	}
	if outcome.Excluded[0].ChannelID != 1 || outcome.Excluded[1].ChannelID != 2 {
		t.Errorf("被筛掉的渠道 = %+v，期望 1 与 2", outcome.Excluded)
	}
	if outcome.Excluded[0].Code != "health" {
		t.Errorf("排除分类 = %q，期望 health", outcome.Excluded[0].Code)
	}
}

// 全部候选都失败时也要落一条响应方向的快照，且**没有** selected。
//
// route.Result.Channel 在失败路径上是「最后试过的那个」（见那里的注释），
// 照抄进 selected 会让一次全军覆没读成一次成功选路。
func TestDecisionSnapshotWithoutSelection(t *testing.T) {
	d := &fakeDecider{err: &channel.ErrUpstream{
		StatusCode: 500, Retryable: true, GotResponse: true,
	}}
	h := newDecideHarness(t, d)

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err == nil {
		t.Fatal("期望报错")
	}

	outcome := h.decisionRedactor.firstOutcome(t)
	if outcome.Succeeded {
		t.Error("上游全失败，不该记成成功")
	}
	if outcome.SelectedChannelID != 0 || outcome.UpstreamModel != "" {
		t.Errorf("没有选中项时两者该是零值，实际 %d / %q",
			outcome.SelectedChannelID, outcome.UpstreamModel)
	}
	// 整次请求的失败证据要落到快照那两列上（与账单同源）。
	if outcome.UpstreamStatus == nil || *outcome.UpstreamStatus != 500 {
		t.Errorf("上游状态码 = %v，期望 500", outcome.UpstreamStatus)
	}
	if outcome.ErrorKind == nil || *outcome.ErrorKind == "" {
		t.Error("失败分类要落到快照那一列上")
	}
}

// 没接决策脱敏器、或没接落库层时不采集，且不报错——留痕是可选能力。
func TestDecisionSnapshotOptional(t *testing.T) {
	cases := []struct {
		name string
		off  func(gw *Gateway)
	}{
		{"没接脱敏器", func(gw *Gateway) { gw.DecisionRedactor = nil }},
		{"没接落库层", func(gw *Gateway) { gw.Snapshots = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDecider{resp: decisionResponse()}
			h := newDecideHarness(t, d)
			tc.off(h.gw)

			if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
				t.Fatalf("没配快照不该影响决策请求: %v", err)
			}
			if n := h.snapshots.count(); n != 0 {
				t.Errorf("不该落任何快照，实际 %d 条", n)
			}
		})
	}
}

// 脱敏失败就丢快照，绝不回落成存原文——两个方向都要钉。
//
// 只让请求方向出错的话，「一条都没落下」证明不了响应那一条也是被丢掉的。
func TestDecisionRedactionFailureDropsSnapshot(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d)
	h.decisionRedactor.err = errors.New("脱敏器出错")

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
		t.Fatalf("脱敏失败不该影响决策请求: %v", err)
	}
	if n := h.snapshots.count(); n != 0 {
		t.Errorf("脱敏失败应当丢快照，实际落了 %d 条", n)
	}
}

// 决策请求没有 chat 形状的请求体，所以不该有任何一处把空消息当成
// 「一条没有内容的对话」去处理。这条用例通过「适配器收到的请求里
// Questions 原样带过去」来钉住：如果哪天有人把中间表示换成了
// channel.Request，这里就没有 Questions 可断言了。
func TestDecidePassesQuestionsThrough(t *testing.T) {
	var seen *systemone.Request
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d)
	// 用一个记录型的替身把整份请求留下来。
	h.gw.Decider = deciderFunc(func(_ context.Context, _ channel.Channel, req *systemone.Request) (*systemone.Response, error) {
		seen = req
		return decisionResponse(), nil
	})

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}
	if seen == nil || len(seen.Questions) != 1 {
		t.Fatalf("适配器收到的请求 = %+v", seen)
	}
	if !strings.Contains(seen.State, "没到账") {
		t.Errorf("state 没有带过去: %q", seen.State)
	}
	// 上游名替换的是副本，入口那份请求不该被就地改掉——它还要拿去渲染
	// 响应与排查。
	if decision().Model != "gpt-4o" {
		t.Error("夹具本身变了")
	}
}

// deciderFunc 让用例用闭包当替身。
type deciderFunc func(context.Context, channel.Channel, *systemone.Request) (*systemone.Response, error)

func (f deciderFunc) Decide(ctx context.Context, ch channel.Channel, req *systemone.Request) (*systemone.Response, error) {
	return f(ctx, ch, req)
}
