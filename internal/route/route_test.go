package route

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/quota"
)

func candidate(id int64, model string, opts ...func(*Candidate)) Candidate {
	c := Candidate{
		Channel: channel.Channel{
			ID:         id,
			Name:       fmt.Sprintf("ch%d", id),
			BaseURL:    "https://api.example.com",
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

func withPriority(p int) func(*Candidate)      { return func(c *Candidate) { c.Priority = p } }
func withType(t string) func(*Candidate)       { return func(c *Candidate) { c.Channel.Type = t } }
func withHealth(h Health) func(*Candidate)     { return func(c *Candidate) { c.Health = h } }
func withQuota(q quota.State) func(*Candidate) { return func(c *Candidate) { c.Quota = q } }
func withCost(v float64) func(*Candidate)      { return func(c *Candidate) { c.CostPerUnit = v } }
func withLatency(d time.Duration) func(*Candidate) {
	return func(c *Candidate) { c.Health.LatencyP95 = d }
}
func withCache(rate float64, n int64) func(*Candidate) {
	return func(c *Candidate) { c.CacheHitRate, c.SampleCount = rate, n }
}

// 下面三个助手都带 Confidence 1.0：它们造的是**一次实测**的额度状态，
// 而实测的置信度就是 1（见 internal/quota 的 confidenceAPIQuery）。
//
// 这个字段不能省。零值是 0，而 0 在打分里表示「这个数不可信」，于是分数会
// 退成中性分 0.5——助手造的「余量 3」与「余量未知」会得到同一个分，
// 用例钉住的具体数值全部失效，且失效的方式是「值变了」而不是「报错」。
// 要造一个不可信的状态，用 withConfidence 显式改它。

// quotaLeft 造一个「有数值、单位未知」的额度状态。
func quotaLeft(remaining float64) quota.State {
	return quota.State{Remaining: &remaining, Confidence: 1}
}

// quotaIn 造一个「有数值、单位已知」的额度状态。
func quotaIn(remaining float64, unit string) quota.State {
	return quota.State{Remaining: &remaining, Unit: unit, Confidence: 1}
}

// quotaExpiring 造一个「有数值、有到期时间」的额度状态。
func quotaExpiring(remaining float64, at time.Time) quota.State {
	return quota.State{Remaining: &remaining, ResetsAt: &at, Confidence: 1}
}

// withConfidence 改掉一个额度状态的可信度。
func withConfidence(q quota.State, c float64) quota.State {
	q.Confidence = c
	return q
}

// quotaObservedNoValue 造一个「观测过、却没有任何可用信息」的额度状态：
// 探针跑过（或从错误里看到过信号），但余量仍然是未知。
func quotaObservedNoValue() quota.State {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return quota.State{LastProbeAt: &at}
}

func seededRouter(seed int64) *Router {
	// 别名来源按同名回落：这些用例关心的是选路与重试，不是翻译。
	// 翻译本身由 alias_test.go 里的用例钉住。
	return &Router{Rand: rand.New(rand.NewSource(seed)), Aliases: identityAliases{}}
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
		"缺 ID":   {Channel: channel.Channel{BaseURL: "https://a", Credential: "c"}},
		"缺 base": {Channel: channel.Channel{ID: 1, Credential: "c"}},
		"缺凭证":    {Channel: channel.Channel{ID: 1, BaseURL: "https://a"}},
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
	exhausted := candidate(1, "m", withQuota(quotaLeft(0)))
	if reason := exhausted.DrainReason(); reason == "" {
		t.Error("额度耗尽应当被挡下")
	}

	unknown := candidate(2, "m", withQuota(quota.State{}))
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

	plenty := r.Score(candidate(1, "m", withQuota(quotaExpiring(1000, time.Now().Add(300*24*time.Hour)))))
	scarceSoon := r.Score(candidate(2, "m", withQuota(quotaExpiring(5, time.Now().Add(time.Hour)))))
	if scarceSoon <= plenty {
		t.Errorf("余额少且快到期(%v) 应高于余额充足(%v)", scarceSoon, plenty)
	}
}

// 估算的额度不如实测可信，这一条**不再由 Estimated 表达**。
//
// 早先这里有一个 estimateDiscount（估算值乘 0.7）。它与 Confidence 说的是
// 同一件事：自记账的置信度上限就是 0.8、永远达不到实测的 1，「推算不如实测
// 可信」已经被它说完了，两处一起看等于把同一件事扣两次。而且那个折扣是乘
// 出来的，它会把「额度未知」的中性分 0.5 也压到 0.35——与「可信度低就退成
// 中性分」正好相反。
//
// 所以现在 Estimated 只是一个标记（读者是插件契约），不参与本包打分：
// 两个只有它不同的状态，分数必须逐字节相同。
func TestEstimatedMarkerDoesNotChangeScore(t *testing.T) {
	r := seededRouter(1)
	measured := quotaIn(5, "money")
	flagged := measured
	flagged.Estimated = true

	a := r.Score(candidate(1, "m", withQuota(measured)))
	b := r.Score(candidate(2, "m", withQuota(flagged)))
	if a != b {
		t.Errorf("Estimated 只是标记，分数应相同：未标记 %v，标成推算 %v", a, b)
	}

	// 真正决定它的是可信度：同一个数，可信度低就更靠近中性分。
	//
	// 余量 5 这次观测说出的分是 0.3333，**低于**中性分，所以「更靠近中性分」
	// 在这里表现为总分变高——一个陈旧观测不再被当成「确实还剩不少」。
	// （方向由观测落在中性分的哪一侧决定，不是「陈旧一定更低」。）
	fresh := r.Score(candidate(3, "m", withQuota(withConfidence(measured, 0.8))))
	stale := r.Score(candidate(4, "m", withQuota(withConfidence(measured, 0.2))))
	if stale <= fresh {
		t.Errorf("可信度低的那一个(%v) 应比刚探测到的(%v) 更靠近中性分", stale, fresh)
	}
	if stale <= 0 {
		t.Errorf("陈旧观测不该被压到零：%v", stale)
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
	// sent 记下**实际发出去的那一份请求**（含 model）。
	//
	// 记它而不是只看入参：规范名到上游名的翻译就发生在调用之前，而
	// 「发出去的是什么」只有这里看得到（见 alias_test.go）。
	sent []channel.Request
	// unknownTypes 模拟执行器按 Type 分派适配器：命中的类型返回
	// channel.ErrUnknownType，与 internal/upstream 的包装方式一致。
	// 它让「库里有一条当前二进制不认识的渠道」这件事在路由层可被复现。
	unknownTypes map[string]bool
}

func (f *fakeExecutor) Chat(_ context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	f.calls = append(f.calls, ch.ID)
	f.sent = append(f.sent, *req)
	if err, ok := f.results[ch.ID]; ok && err != nil {
		return nil, err
	}
	if f.unknownTypes[ch.Type] {
		return nil, fmt.Errorf("渠道 %d: %w", ch.ID, channel.ErrUnknownType)
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

// 类型不被当前二进制认识的渠道只影响它自己：那是**这条候选**的问题，
// 不是**这次请求**的问题，后面的健康候选照常尝试。
//
// 这正是回滚版本之后最容易出现的形态——库里留着新版本写进去的渠道行，
// 旧二进制的候选集里就有它，而它排在前面时不该毒掉整条请求。
func TestChatSkipsUnknownChannelType(t *testing.T) {
	exec := &fakeExecutor{
		unknownTypes: map[string]bool{"newer-type": true},
		resp:         &channel.Response{UpstreamModel: "m"},
	}
	r := seededRouter(1)

	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{
			candidate(1, "m", withType("newer-type"), withPriority(10)),
			candidate(2, "m", withPriority(1)),
		}, "m")
	if err != nil {
		t.Fatalf("应跳过类型不认识的候选并成功: %v", err)
	}
	if res.Channel.ID != 2 {
		t.Fatalf("应落在第二个候选上，实际 %d", res.Channel.ID)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("调用序列 = %v，期望 [1 2]", exec.calls)
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("尝试记录数 = %d，期望 2（含失败的那次）", len(res.Attempts))
	}
	if !errors.Is(res.Attempts[0].Err, channel.ErrUnknownType) {
		t.Errorf("第一次尝试应记为未知类型: %v", res.Attempts[0].Err)
	}
}

// 全部候选都因类型不认识而失败时，返回的错误仍要能认出原因。
// 不能变成一句「全都试过了」把原因盖掉——排查时那等于没有信息。
func TestChatAllUnknownTypesKeepsCause(t *testing.T) {
	exec := &fakeExecutor{unknownTypes: map[string]bool{"newer-type": true}}
	r := seededRouter(1)

	_, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{
			candidate(1, "m", withType("newer-type")),
			candidate(2, "m", withType("newer-type")),
		}, "m")
	if err == nil {
		t.Fatal("全部候选失败应报错")
	}
	if !errors.Is(err, channel.ErrUnknownType) {
		t.Errorf("错误应仍能认出 channel.ErrUnknownType: %v", err)
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
		Rand:    rand.New(rand.NewSource(1)),
		Aliases: identityAliases{},
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

// ── 流式 ────────────────────────────────────────────────────────────────

type fakeStreamExecutor struct {
	calls   []int64
	results map[int64]error
	events  []channel.StreamEvent
	// sent 见 fakeExecutor.sent。
	sent []channel.Request
	// 见 fakeExecutor.unknownTypes。
	unknownTypes map[string]bool
}

func (f *fakeStreamExecutor) ChatStream(_ context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	f.calls = append(f.calls, ch.ID)
	f.sent = append(f.sent, *req)
	if err, ok := f.results[ch.ID]; ok && err != nil {
		return nil, err
	}
	if f.unknownTypes[ch.Type] {
		return nil, fmt.Errorf("渠道 %d: %w", ch.ID, channel.ErrUnknownType)
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

// 流式路径与 chatWithPlan 是同一条判据的两份实现，这里单独钉一次：
// 类型不认识的候选在输出前就被换掉，后面的健康候选照常接管。
func TestChatStreamSkipsUnknownChannelType(t *testing.T) {
	exec := &fakeStreamExecutor{
		unknownTypes: map[string]bool{"newer-type": true},
		events:       []channel.StreamEvent{{Kind: "delta", Text: "hi"}},
	}
	r := seededRouter(1)

	res, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "m", Stream: true},
		[]Candidate{
			candidate(1, "m", withType("newer-type"), withPriority(10)),
			candidate(2, "m", withPriority(1)),
		}, "m")
	if err != nil {
		t.Fatalf("应跳过类型不认识的候选并成功: %v", err)
	}
	if res.Channel.ID != 2 {
		t.Fatalf("应落在第二个候选上，实际 %d", res.Channel.ID)
	}
	if len(exec.calls) != 2 {
		t.Fatalf("调用序列 = %v，期望 [1 2]", exec.calls)
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("尝试记录数 = %d，期望 2（含失败的那次）", len(res.Attempts))
	}
	if !errors.Is(res.Attempts[0].Err, channel.ErrUnknownType) {
		t.Errorf("第一次尝试应记为未知类型: %v", res.Attempts[0].Err)
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

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 来自变异测试（gremlins unleash）：`internal/route` 的 Test efficacy 是
// 68%，`internal/quota` 是 79%。存活变异集中在打分函数的**边界**上。
//
// 原有用例全是「A 明显比 B 好」这种相对断言，于是把某个 `<=` 改成 `<`、
// 把某个常量改掉，只要相对顺序不变就测不出来。而选路的分差直接决定
// 走哪个渠道，也就直接决定成本。

// 打分用的是 DefaultWeights；这个断言让「权重被改」在这里就失败，
// 而不是表现为线上流量悄悄换了渠道。
func defaultScore(t *testing.T, c Candidate) float64 {
	t.Helper()
	r := &Router{Rand: rand.New(rand.NewSource(1))}
	return r.Score(c)
}

// 成功率为 0 与「没有样本」必须给同一个中性分。
//
// 变异：`base <= 0` 改成 `< 0`，恰好为 0 时不再给 0.5 而是直接用 0——
// 一个「成功率 0%」的渠道会被额外惩罚，而它可能只是样本数为 0。
func TestScoreHealthZeroSuccessRateIsNeutral(t *testing.T) {
	zero := defaultScore(t, candidate(1, "m", withHealth(Health{SuccessRate: 0})))
	half := defaultScore(t, candidate(2, "m", withHealth(Health{SuccessRate: 0.5})))
	if zero != half {
		t.Errorf("成功率为 0 应与中性的 0.5 等价：%v vs %v", zero, half)
	}
}

// 连续失败次数单调压低分数，且**严格递减**（不只是不增）。
//
// 变异：把 `1.0/(1.0+f)` 改成 `1.0/f` 之类的写法在 f=0 时行为不同；
// 把分母的加数去掉则 f=0 与 f=1 的差会变。严格递减能钉住曲线形状。
func TestScoreHealthStrictlyDecreasingInFailures(t *testing.T) {
	prev := defaultScore(t, candidate(1, "m", withHealth(Health{SuccessRate: 0.9, ConsecutiveFailures: 0})))
	for f := 1; f <= 5; f++ {
		got := defaultScore(t, candidate(1, "m", withHealth(Health{SuccessRate: 0.9, ConsecutiveFailures: f})))
		if got >= prev {
			t.Fatalf("失败 %d 次的分数 %v 未低于 %d 次的 %v", f, got, f-1, prev)
		}
		prev = got
	}
}

// 延迟恰好为 0 是「未知」，不是「最快」。
//
// 变异：`p95 <= 0` 改成 `< 0`，未知延迟会走反比函数得到满分类的值——
// 一个没测过延迟的渠道会被当成最快的。
func TestScoreLatencyUnknownIsNotPerfect(t *testing.T) {
	unknown := defaultScore(t, candidate(1, "m", withLatency(0)))
	veryFast := defaultScore(t, candidate(2, "m", withLatency(time.Millisecond)))
	if unknown >= veryFast {
		t.Errorf("未知延迟(%v) 不该高于实测极快(%v)——未知不等于最快", unknown, veryFast)
	}
}

// 缓存样本数的阈值是「恰好达到即生效」。
//
// 变异：`sampleCount < minSamples` 改成 `<=`，恰好达到阈值的渠道
// 会被当成样本不足——而阈值本身就是为了「样本足够」而定的。
func TestScoreCacheThresholdIsInclusiveAt20(t *testing.T) {
	at20 := defaultScore(t, candidate(1, "m", withCache(1.0, 20)))
	at19 := defaultScore(t, candidate(2, "m", withCache(1.0, 19)))
	if at20 == at19 {
		t.Errorf("样本 20 与 19 应给出不同分数（阈值恰好生效）：%v vs %v", at20, at19)
	}
}

// 命中率超出 [0,1] 要被夹住，而不是产生越界分数。
//
// 变异：`hitRate < 0` 或 `hitRate > 1` 被改成别的，越界值会直接进分数，
// 而 2.0 的命中率能让一个渠道的分数超过满分。
func TestScoreCacheClampsOutOfRange(t *testing.T) {
	max := defaultScore(t, candidate(1, "m", withCache(1.0, 100)))
	over := defaultScore(t, candidate(2, "m", withCache(5.0, 100)))
	if over != max {
		t.Errorf("命中率超过 1 应夹到 1：%v vs %v", over, max)
	}

	min := defaultScore(t, candidate(3, "m", withCache(0.0, 100)))
	under := defaultScore(t, candidate(4, "m", withCache(-1.0, 100)))
	if under != min {
		t.Errorf("命中率低于 0 应夹到 0：%v vs %v", under, min)
	}
}

// 额度：恰好耗尽是最低分，而「查不到」是中性、必须高于它。
//
// 变异：`q.Usable()` 那一档被去掉，恰好耗尽时会走反比函数得到**最高**分。
func TestScoreQuotaExhaustedIsLowest(t *testing.T) {
	exhausted := defaultScore(t, candidate(1, "m", withQuota(quotaLeft(0))))
	unknown := defaultScore(t, candidate(2, "m", withQuota(quota.State{})))

	if exhausted >= unknown {
		t.Errorf("耗尽(%v) 应低于未知(%v)——「查不到」不等于「没有」", exhausted, unknown)
	}

	// 恰好耗尽与「只剩极小量」应当不同（前者是零，后者还有一点）
	tiny := defaultScore(t, candidate(3, "m", withQuota(quotaLeft(0.000001))))
	if exhausted >= tiny {
		t.Errorf("恰好耗尽(%v) 应低于只剩极小量(%v)", exhausted, tiny)
	}
}

// 额度紧迫度：剩余越少分越高，且**严格**单调。
//
// 变异：`1.0/(1.0+*q.Remaining)` 里的加数被去掉，剩余 0 会除以零得到
// +Inf；而严格单调能钉住这条曲线的形状。
func TestScoreQuotaStrictlyIncreasingAsRemainingShrinks(t *testing.T) {
	var prev float64 = -1
	for _, remaining := range []float64{10000, 1000, 100, 10, 1, 0.1} {
		got := defaultScore(t, candidate(1, "m", withQuota(quotaLeft(remaining))))
		if got <= prev {
			t.Fatalf("剩余 %v 的分数 %v 未高于上一档 %v", remaining, got, prev)
		}
		prev = got
	}
}

// 额度恰好归零时分数必须是有限的——除以零会给出 +Inf，
// 而 +Inf 一旦进排序，那个渠道会排到所有渠道前面。
func TestScoreQuotaZeroRemainingIsFinite(t *testing.T) {
	got := defaultScore(t, candidate(1, "m", withQuota(quotaLeft(0))))
	if math.IsInf(got, 0) || math.IsNaN(got) {
		t.Errorf("额度耗尽时分数 = %v，不该是 Inf 或 NaN", got)
	}
}

// 零成本是「未知」，不是「免费」——它给中性分，而不是满分。
//
// 变异：`cost <= 0` 改成 `< 0`，成本未知的渠道会走反比函数得到满分数。
func TestScoreCostUnknownIsNotPerfect(t *testing.T) {
	unknown := defaultScore(t, candidate(1, "m", withCost(0)))
	nearlyFree := defaultScore(t, candidate(2, "m", withCost(0.000001)))
	if unknown >= nearlyFree {
		t.Errorf("未知成本(%v) 不该高于实测近乎免费(%v)", unknown, nearlyFree)
	}
}

// 同分的打散必须**由注入的随机源驱动**。
//
// 变异：`if r.Rand != nil` 改成 `== nil`，注入的种子源被忽略、
// 改用全局随机——那会让测试不可复现，而「可复现」正是注入 Rand 的目的。
func TestShuffleUsesInjectedRand(t *testing.T) {
	cands := []Candidate{candidate(1, "m"), candidate(2, "m"), candidate(3, "m")}

	// 同一个种子跑两次，顺序必须一致
	order := func() []int64 {
		r := seededRouter(42)
		plan := r.Select(cands, "m")
		out := make([]int64, 0, len(plan.Candidates))
		for _, c := range plan.Candidates {
			out = append(out, c.Channel.ID)
		}
		return out
	}
	a, b := order(), order()
	if len(a) != len(b) {
		t.Fatal("两次长度不同")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("同一个种子两次结果不同：%v vs %v", a, b)
		}
	}
}
