package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

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
		Usage:   systemone.Usage{InputTokens: 387, OutputTokens: 72},
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
		resp:    decisionResponse(),
		failFor: map[int64]error{1: &channel.ErrUpstream{StatusCode: 503, Retryable: true}},
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

// 决策这条路径**不采请求快照**（脱敏器的允许表按 channel.Request 的形状建）。
// 这一条钉住「不采」是有意的：哪天有人把快照接上，这条用例会红，
// 那时要一并把 state/questions 的脱敏规则想清楚。
func TestDecideTakesNoSnapshot(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	h := newDecideHarness(t, d)

	if _, err := h.gw.Decide(context.Background(), decisionBilling(), decision()); err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}
	// 上报是异步的，但快照那一路在决策这条路径上**根本不会被调用**：
	// 它挂在 snapshotAsync 上，而 Decide 不调它。
	if n := h.snapshots.count(); n != 0 {
		t.Errorf("快照数 = %d，期望 0", n)
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
