package route

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

func candidate(id int64, model string, opts ...func(*Candidate)) Candidate {
	c := Candidate{
		Channel: channel.Channel{
			ID:         id,
			Name:       fmt.Sprintf("ch%d", id),
			BaseURL:    "https://api.example.com",
			Protocol:   channel.ProtocolOpenAI,
			Credential: "placeholder",
			Models:     []string{model},
		},
		Priority: 0,
		Weight:   1,
		Health:   Health{SuccessRate: 0.9},
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func withPriority(p int) func(*Candidate)        { return func(c *Candidate) { c.Priority = p } }
func withHealth(h Health) func(*Candidate)       { return func(c *Candidate) { c.Health = h } }
func withQuota(q channel.Quota) func(*Candidate) { return func(c *Candidate) { c.Quota = q } }
func withCost(v float64) func(*Candidate)        { return func(c *Candidate) { c.CostPerUnit = v } }
func withLatency(d time.Duration) func(*Candidate) {
	return func(c *Candidate) { c.Health.LatencyP95 = d }
}
func withCache(rate float64, n int64) func(*Candidate) {
	return func(c *Candidate) { c.CacheHitRate, c.SampleCount = rate, n }
}

func seededRouter(seed int64) *Router {
	return &Router{Rand: rand.New(rand.NewSource(seed))}
}

func TestSelectFiltersByModel(t *testing.T) {
	r := seededRouter(1)
	plan := r.Select([]Candidate{
		candidate(1, "gpt-4o"),
		candidate(2, "claude-sonnet"),
	}, "gpt-4o")

	if len(plan.Candidates) != 1 {
		t.Fatalf("候选数为 %d，期望 1", len(plan.Candidates))
	}
	if plan.Candidates[0].Channel.ID != 1 {
		t.Errorf("选中了渠道 %d", plan.Candidates[0].Channel.ID)
	}
	// 排除的原因要记下来，否则商家问「我配了三个为什么只用一个」时无从回答
	if len(plan.Excluded) != 1 || !strings.Contains(plan.Excluded[0].Reason, "不支持模型") {
		t.Errorf("排除记录 = %+v", plan.Excluded)
	}
}

func TestEligibleRejectsBrokenConfig(t *testing.T) {
	cases := map[string]Candidate{
		"缺 ID":   {Channel: channel.Channel{BaseURL: "https://a", Credential: "c", Protocol: channel.ProtocolOpenAI}},
		"缺 base": {Channel: channel.Channel{ID: 1, Credential: "c", Protocol: channel.ProtocolOpenAI}},
		"缺凭证":    {Channel: channel.Channel{ID: 1, BaseURL: "https://a", Protocol: channel.ProtocolOpenAI}},
		"协议未知":   {Channel: channel.Channel{ID: 1, BaseURL: "https://a", Credential: "c", Protocol: "gemini"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if ok, _ := c.Eligible(); ok {
				t.Error("应判定为不可用")
			}
		})
	}
}

// 连续失败的渠道无论多便宜都不该被选中。
func TestConsecutiveFailuresDrainsChannel(t *testing.T) {
	c := candidate(1, "m", withHealth(Health{ConsecutiveFailures: consecutiveFailureCutoff}))
	if reason := c.DrainReason(); reason == "" {
		t.Fatal("连续失败达到阈值应当被挡下")
	}

	r := seededRouter(1)
	plan := r.Select([]Candidate{c}, "m")
	if len(plan.Candidates) != 0 {
		t.Error("被挡下的渠道不该出现在候选里")
	}
}

// 单次失败不该摘掉渠道：上游偶发抖动很常见，一失败就摘会让流量无规律漂移。
func TestSingleFailureDoesNotDrain(t *testing.T) {
	c := candidate(1, "m", withHealth(Health{ConsecutiveFailures: 1}))
	if reason := c.DrainReason(); reason != "" {
		t.Errorf("单次失败不该被挡下，实际原因: %s", reason)
	}
}

// 额度耗尽与「查不到额度」是两件事。
func TestQuotaExhaustionVsUnknown(t *testing.T) {
	exhausted := candidate(1, "m", withQuota(channel.Quota{Known: true, Remaining: 0}))
	if reason := exhausted.DrainReason(); reason == "" {
		t.Error("额度耗尽应当被挡下")
	}

	unknown := candidate(2, "m", withQuota(channel.Quota{Known: false}))
	if reason := unknown.DrainReason(); reason != "" {
		t.Errorf("查不到额度不该被挡下，实际原因: %s", reason)
	}
}

// priority 是商家的显式意图，不该被任何打分盖过。
func TestPriorityBeatsScore(t *testing.T) {
	expensiveBad := candidate(1, "m", withPriority(10),
		withHealth(Health{SuccessRate: 0.1}), withCost(100))
	cheapGood := candidate(2, "m", withPriority(0),
		withHealth(Health{SuccessRate: 1.0}), withCost(0.001))

	r := seededRouter(1)
	plan := r.Select([]Candidate{cheapGood, expensiveBad}, "m")

	if plan.Candidates[0].Channel.ID != 1 {
		t.Errorf("priority 应当优先于分数，实际第一个是渠道 %d", plan.Candidates[0].Channel.ID)
	}
}

// 同 priority 内分数高的排前面。
func TestScoreOrdersWithinPriority(t *testing.T) {
	healthy := candidate(1, "m", withHealth(Health{SuccessRate: 1.0}))
	sick := candidate(2, "m", withHealth(Health{SuccessRate: 0.1, ConsecutiveFailures: 2}))

	r := seededRouter(1)
	plan := r.Select([]Candidate{sick, healthy}, "m")

	if plan.Candidates[0].Channel.ID != 1 {
		t.Errorf("健康度高的应先选，实际第一个是渠道 %d", plan.Candidates[0].Channel.ID)
	}
}

// 同分之间必须是**打散**的，否则所有流量压在排序第一的渠道上：
// 一台坏渠道影响全部流量，其余渠道长期空转导致健康数据陈旧。
func TestEqualScoresAreShuffled(t *testing.T) {
	cands := []Candidate{candidate(1, "m"), candidate(2, "m"), candidate(3, "m")}

	seenFirst := map[int64]int{}
	r := seededRouter(42)
	for i := 0; i < 300; i++ {
		plan := r.Select(cands, "m")
		if len(plan.Candidates) == 0 {
			t.Fatal("候选不应为空")
		}
		seenFirst[plan.Candidates[0].Channel.ID]++
	}
	if len(seenFirst) < 2 {
		t.Fatalf("同分候选应被打散，实际只出现过 %v", seenFirst)
	}
	for id, n := range seenFirst {
		if n < 20 {
			t.Errorf("渠道 %d 只排第一 %d 次，分布过于不均: %v", id, n, seenFirst)
		}
	}
}

// 未知成本不能被当成免费：那会让所有未知成本的渠道排在已知便宜的前面，
// 而那是超支最容易发生的路径。
func TestUnknownCostIsNeutralNotBest(t *testing.T) {
	r := seededRouter(1)
	unknown := r.Score(candidate(1, "m", withCost(0)))
	cheap := r.Score(candidate(2, "m", withCost(0.001)))
	if unknown >= cheap {
		t.Errorf("未知成本(%v) 不该高于已知便宜(%v)", unknown, cheap)
	}
}

// 样本太少时缓存命中率不该影响路由：一两次请求的命中率不说明任何事。
func TestCacheRateNeedsSamples(t *testing.T) {
	r := seededRouter(1)
	few := r.Score(candidate(1, "m", withCache(1.0, 3)))
	many := r.Score(candidate(2, "m", withCache(1.0, 100)))
	if few == many {
		t.Errorf("样本不足与样本充足应给出不同分数（%v vs %v）", few, many)
	}
}

// 没有历史的渠道给中性分，而不是零分：新品没有历史不等于它不好。
func TestNoHistoryIsNeutral(t *testing.T) {
	r := seededRouter(1)
	fresh := r.Score(candidate(1, "m", withHealth(Health{})))
	broken := r.Score(candidate(2, "m", withHealth(Health{SuccessRate: 0.1, ConsecutiveFailures: 2})))
	if fresh <= broken {
		t.Errorf("无历史(%v) 应高于明显有问题(%v)", fresh, broken)
	}
}

// 额度策略：剩余越少、越接近到期，越该被优先用掉。
func TestQuotaPrefersScarceAndSoonExpiring(t *testing.T) {
	r := seededRouter(1)

	plenty := r.Score(candidate(1, "m", withQuota(channel.Quota{
		Known: true, Remaining: 1000, ExpiresAt: ptr(time.Now().Add(300 * 24 * time.Hour)),
	})))
	scarceSoon := r.Score(candidate(2, "m", withQuota(channel.Quota{
		Known: true, Remaining: 5, ExpiresAt: ptr(time.Now().Add(time.Hour)),
	})))
	if scarceSoon <= plenty {
		t.Errorf("余额少且快到期(%v) 应高于余额充足(%v)", scarceSoon, plenty)
	}
}

// 估算的额度不如实测可信，但不该被完全排除——它仍然比「完全不知道」有用。
func TestEstimatedQuotaIsDiscountedNotExcluded(t *testing.T) {
	r := seededRouter(1)
	measured := r.Score(candidate(1, "m", withQuota(channel.Quota{Known: true, Remaining: 5})))
	estimated := r.Score(candidate(2, "m", withQuota(channel.Quota{Known: true, Remaining: 5, Estimated: true})))

	if estimated >= measured {
		t.Error("估算值应被打折")
	}
	if estimated <= 0 {
		t.Error("估算值不该被压到零，它仍然有信息量")
	}
}

// 延迟越大分越低。
func TestLatencyAffectsScore(t *testing.T) {
	r := seededRouter(1)
	fast := r.Score(candidate(1, "m", withLatency(200*time.Millisecond)))
	slow := r.Score(candidate(2, "m", withLatency(20*time.Second)))
	if fast <= slow {
		t.Errorf("快(%v) 应高于慢(%v)", fast, slow)
	}
}

func TestZeroWeightsFallsBackToDefaults(t *testing.T) {
	r := &Router{Rand: rand.New(rand.NewSource(1))}
	a := r.Score(candidate(1, "m", withHealth(Health{SuccessRate: 1.0})))
	b := r.Score(candidate(2, "m", withHealth(Health{SuccessRate: 0.1, ConsecutiveFailures: 2})))
	if a <= b {
		t.Errorf("零值权重应回落到默认权重，得到 a=%v b=%v", a, b)
	}
}

// ── 执行 ────────────────────────────────────────────────────────────────

type fakeExecutor struct {
	calls   []int64
	results map[int64]error
	resp    *channel.Response
}

func (f *fakeExecutor) Chat(_ context.Context, ch channel.Channel, _ *channel.Request) (*channel.Response, error) {
	f.calls = append(f.calls, ch.ID)
	if err, ok := f.results[ch.ID]; ok && err != nil {
		return nil, err
	}
	return f.resp, nil
}

func retryableErr() error { return &channel.ErrUpstream{StatusCode: 500, Retryable: true} }
func permanentErr() error { return &channel.ErrUpstream{StatusCode: 401, Retryable: false} }
func plainErr() error     { return errors.New("随便一个错") }

func TestChatSucceedsOnFirstCandidate(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{UpstreamModel: "m"}}
	r := seededRouter(1)

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m")}, "m")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Response == nil || res.Channel.ID != 1 {
		t.Errorf("结果 = %+v", res)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Err != nil {
		t.Errorf("尝试记录 = %+v", res.Attempts)
	}
}

// 可重试的失败换下一个渠道；失败的调用不计费，但要知道发生过。
func TestChatFailsOverOnRetryableError(t *testing.T) {
	exec := &fakeExecutor{
		results: map[int64]error{1: retryableErr()},
		resp:    &channel.Response{UpstreamModel: "m"},
	}
	r := seededRouter(1)

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}, "m")
	if err != nil {
		t.Fatalf("应换渠道成功: %v", err)
	}
	if res.Channel.ID == 1 {
		t.Fatal("不应停在失败的渠道上")
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("尝试记录数为 %d，期望 2（含失败的那次）", len(res.Attempts))
	}
	if res.Attempts[0].Err == nil || !res.Attempts[0].Retryable {
		t.Errorf("第一次尝试应记为可重试失败: %+v", res.Attempts[0])
	}
}

// 不可重试的失败立刻返回：换渠道也是同一个错，重试只会把费用与延迟
// 乘以候选渠道数。
func TestChatStopsOnPermanentError(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{1: permanentErr(), 2: permanentErr()}}
	r := seededRouter(1)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}, "m")
	if err == nil {
		t.Fatal("应报错")
	}
	if len(exec.calls) != 1 {
		t.Errorf("不可重试的错误不该继续尝试，实际调用了 %v", exec.calls)
	}
}

// 未知类型的错误默认不可重试——与 channel.IsRetryable 的方向一致。
func TestChatTreatsUnknownErrorAsPermanent(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{1: plainErr(), 2: plainErr()}}
	r := seededRouter(1)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}, "m")
	if err == nil {
		t.Fatal("应报错")
	}
	if len(exec.calls) != 1 {
		t.Errorf("未知错误默认不可重试，实际调用了 %v", exec.calls)
	}
}

func TestChatExhaustsAllCandidates(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{1: retryableErr(), 2: retryableErr(), 3: retryableErr()}}
	r := seededRouter(1)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m"), candidate(2, "m"), candidate(3, "m")}, "m")
	if err == nil {
		t.Fatal("全失败应报错")
	}
	var all *Errors
	if !errors.As(err, &all) {
		t.Fatalf("错误类型为 %T", err)
	}
	if len(all.Attempts) != 3 {
		t.Errorf("尝试记录数为 %d，期望 3", len(all.Attempts))
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("错误信息应说明试了几个: %s", err.Error())
	}
}

func TestChatNoCandidates(t *testing.T) {
	exec := &fakeExecutor{}
	r := seededRouter(1)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"}, []Candidate{}, "m")
	if err == nil {
		t.Fatal("没有候选应报错")
	}
	if len(exec.calls) != 0 {
		t.Error("不该发起任何调用")
	}
}

// 客户端断开时不再尝试剩余渠道：已经没有人等着这个响应了。
func TestChatStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	exec := &fakeExecutor{results: map[int64]error{1: retryableErr()}}
	r := seededRouter(1)

	_, err := r.Chat(ctx, exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m"), candidate(2, "m")}, "m")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("应返回 context 错误，得到 %v", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("已取消的 context 不该发起调用，实际 %v", exec.calls)
	}
}

// 尝试记录要带上耗时，用于统计与排查。
func TestAttemptsRecordTiming(t *testing.T) {
	now := time.Unix(0, 0)
	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{}}
	r := &Router{
		Rand: rand.New(rand.NewSource(1)),
		Now: func() time.Time {
			now = now.Add(50 * time.Millisecond)
			return now
		},
	}

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m")}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("尝试数 = %d", len(res.Attempts))
	}
	if res.Attempts[0].Duration != 50*time.Millisecond {
		t.Errorf("耗时 = %v，期望 50ms", res.Attempts[0].Duration)
	}
	if res.Attempts[0].StartedAt.IsZero() {
		t.Error("应记录开始时间")
	}
}

// 上游可能在断开前已经产生了费用，只是我们没拿到回执。
// 所以失败的尝试也要完整记录，计费层需要据此判断。
func TestFailedAttemptsAreRecordedForBilling(t *testing.T) {
	exec := &fakeExecutor{results: map[int64]error{1: retryableErr()}, resp: &channel.Response{}}
	r := seededRouter(1)

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts[0].ChannelID != 1 || res.Attempts[0].Err == nil {
		t.Errorf("失败的那次尝试应被完整记录: %+v", res.Attempts[0])
	}
}

func ptr[T any](v T) *T { return &v }

// ── 流式 ────────────────────────────────────────────────────────────────

type fakeStreamExecutor struct {
	calls   []int64
	results map[int64]error
	events  []channel.StreamEvent
}

func (f *fakeStreamExecutor) ChatStream(_ context.Context, ch channel.Channel, _ *channel.Request) (<-chan channel.StreamEvent, error) {
	f.calls = append(f.calls, ch.ID)
	if err, ok := f.results[ch.ID]; ok && err != nil {
		return nil, err
	}
	out := make(chan channel.StreamEvent, len(f.events))
	for _, ev := range f.events {
		out <- ev
	}
	close(out)
	return out, nil
}

// 上游在响应头阶段拒绝时还没发出任何内容，可以换渠道。
func TestChatStreamFailsOverBeforeOutput(t *testing.T) {
	exec := &fakeStreamExecutor{
		results: map[int64]error{1: retryableErr()},
		events:  []channel.StreamEvent{{Kind: "delta", Text: "hi"}},
	}
	r := seededRouter(1)

	res, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "m", Stream: true},
		[]Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}, "m")
	if err != nil {
		t.Fatalf("应在输出前换渠道成功: %v", err)
	}
	if res.Channel.ID == 1 {
		t.Fatal("不应停在失败的渠道")
	}
	if len(res.Attempts) != 2 {
		t.Errorf("尝试数 = %d，期望 2", len(res.Attempts))
	}
}

// 不可重试的错误立刻返回，不换渠道。
func TestChatStreamStopsOnPermanentError(t *testing.T) {
	exec := &fakeStreamExecutor{results: map[int64]error{1: permanentErr(), 2: permanentErr()}}
	r := seededRouter(1)

	_, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "m", Stream: true},
		[]Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}, "m")
	if err == nil {
		t.Fatal("应报错")
	}
	if len(exec.calls) != 1 {
		t.Errorf("不可重试的错误不该继续尝试，实际 %v", exec.calls)
	}
}

func TestChatStreamNoCandidates(t *testing.T) {
	exec := &fakeStreamExecutor{}
	r := seededRouter(1)
	if _, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "m"}, nil, "m"); err == nil {
		t.Fatal("没有候选应报错")
	}
}

// 一旦拿到事件流就不再换渠道：上游已接受请求，后续失败属于「中途断了」。
func TestChatStreamCommitsOnceEventsReturned(t *testing.T) {
	exec := &fakeStreamExecutor{
		results: map[int64]error{},
		events: []channel.StreamEvent{
			{Kind: "delta", Text: "部分"},
			{Kind: "error", Err: retryableErr()},
		},
	}
	r := seededRouter(1)

	res, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "m", Stream: true},
		[]Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}, "m")
	if err != nil {
		t.Fatalf("拿到事件流即算成功: %v", err)
	}
	if len(exec.calls) != 1 {
		t.Errorf("拿到事件流之后不该再调用其他渠道，实际 %v", exec.calls)
	}

	// 流中途的错误原样透出，由调用方决定怎么处理
	var sawErr bool
	for ev := range res.Events {
		if ev.Kind == "error" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("中途错误应透出")
	}
}

// WrapStreamStarted 让调用方能判断「失败发生在吐字之后吗」。
// 这个判断不该由每个调用点自己数——漏掉一处就会产生一次不该发生的重试。
func TestWrapStreamStartedTracksOutput(t *testing.T) {
	cases := []struct {
		name    string
		events  []channel.StreamEvent
		started bool
	}{
		{"只有 done", []channel.StreamEvent{{Kind: "done"}}, false},
		{"只有 usage", []channel.StreamEvent{{Kind: "usage"}}, false},
		{"有 delta", []channel.StreamEvent{{Kind: "delta", Text: "x"}}, true},
		{"有 tool_use", []channel.StreamEvent{{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{}}}, true},
		{"先 delta 后 error", []channel.StreamEvent{
			{Kind: "delta", Text: "x"},
			{Kind: "error", Err: errors.New("断了")},
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := make(chan channel.StreamEvent, len(tc.events))
			for _, ev := range tc.events {
				in <- ev
			}
			close(in)

			out, started := WrapStreamStarted(in)
			for range out {
			}
			if got := started(); got != tc.started {
				t.Errorf("started() = %v，期望 %v", got, tc.started)
			}
		})
	}
}

// 包一层不该丢掉任何事件，也不该改变顺序。
func TestWrapStreamStartedPassesEventsThrough(t *testing.T) {
	in := make(chan channel.StreamEvent, 3)
	want := []string{"delta", "usage", "done"}
	for _, k := range want {
		in <- channel.StreamEvent{Kind: k}
	}
	close(in)

	out, _ := WrapStreamStarted(in)
	var got []string
	for ev := range out {
		got = append(got, ev.Kind)
	}
	if len(got) != len(want) {
		t.Fatalf("事件数 = %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个事件 = %q，期望 %q", i, got[i], want[i])
		}
	}
}
