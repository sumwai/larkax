package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/tokens"
)

// 本文件测流式那条路的四个结局与一条约束：
//
//	正常结束 / 客户端断开 / 上游中途失败 / 上游接受之前就失败
//	已经吐字之后**不再换渠道**
//
// 断言都落在钱上（结算有没有、金额是多少、预留放没放），而不只是「返回了
// 什么错误」——上游失败却记了账，或客户端断开却没记账，都不会报错。

// ── 脚本化的执行器 ──────────────────────────────────────────────────────

// scriptedExecutor 按渠道回一串预先写好事件。
type scriptedExecutor struct {
	// preErr 让某个渠道在**接受请求之前**失败——它可以换渠道。
	preErr map[int64]error
	// script 是接受之后要吐的事件。
	script map[int64][]channel.StreamEvent
	// hold 非 nil 时事件吐完之后挂着不关通道，等 ctx 结束或它被关闭。
	// 用它模拟「流还没完」——客户端断开、上游卡住都发生在这种时刻。
	hold map[int64]chan struct{}

	mu    sync.Mutex
	calls []int64
}

func (s *scriptedExecutor) Chat(context.Context, channel.Channel, *channel.Request) (*channel.Response, error) {
	return nil, errors.New("流式用例不该走非流式入口")
}

func (s *scriptedExecutor) ChatStream(ctx context.Context, ch channel.Channel, _ *channel.Request) (<-chan channel.StreamEvent, error) {
	s.mu.Lock()
	s.calls = append(s.calls, ch.ID)
	s.mu.Unlock()

	if err, ok := s.preErr[ch.ID]; ok {
		return nil, err
	}

	events := make(chan channel.StreamEvent)
	go func() {
		defer close(events)
		for _, ev := range s.script[ch.ID] {
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
		if hold := s.hold[ch.ID]; hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
			}
		}
	}()
	return events, nil
}

// calledChannels 返回被调用过的渠道，按顺序。
func (s *scriptedExecutor) calledChannels() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.calls...)
}

// useStreamExecutor 把测试用的脚本执行器装进 harness。
func useStreamExecutor(h *harness, exec *scriptedExecutor) *scriptedExecutor {
	h.gw.Executor = exec
	return exec
}

// collect 把事件流读到底并返回读到的事件。
//
// 必须读到底：网关在关闭事件通道**之前**完成结算，所以读完就说明账已经
// 记好了——测试因此不需要 sleep，也不需要轮询。
func collect(t *testing.T, res *Result) []channel.StreamEvent {
	t.Helper()

	var out []channel.StreamEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-res.Events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("事件流一直没有关闭")
		}
	}
}

func delta(text string) channel.StreamEvent {
	return channel.StreamEvent{Kind: "delta", Text: text}
}

// ── 正常结束 ────────────────────────────────────────────────────────────

// 上游报过 usage：按实测用量计费，来源标成 measured，Estimated 为 false。
func TestChatStreamBillsMeasuredUsage(t *testing.T) {
	h := newHarness(t)
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "usage", Usage: &usage}, {Kind: "done", FinishReason: "stop"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	events := collect(t, res)

	if len(events) != 3 {
		t.Errorf("事件数 = %d，期望 3（网关原样转发，不改事件序列）", len(events))
	}
	if res.Charge == nil {
		t.Fatal("正常结束必须结算")
	}
	// 12 × 1e-6 + 7 × 2e-6 = 0.00002600
	if got := res.Charge.Settlement.Actual.String(); got != "0.00002600" {
		t.Errorf("金额 = %s，期望 0.00002600（按上游报的 12/7 算）", got)
	}
	if res.Charge.UsageSource != tokens.SourceMeasured {
		t.Errorf("用量来源 = %v，期望实测", res.Charge.UsageSource)
	}
	if res.Estimated {
		t.Error("上游报过 usage 时 Estimated 应为 false")
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("正常结束不该释放预留（解冻由结算事务做）：%+v", h.reservations.released)
	}
	// 另一头也要钉：正常结束不能被标成客户端取消。只钉上面那条的话，
	// 把这一列写成恒真也能过，而恒真会让每一笔账单都记上「客户端断的」。
	if len(h.settlements.saved) == 1 && h.settlements.saved[0].ClientCanceled {
		t.Error("上游正常返回被标成了客户端取消")
	}
}

// 上游没报 usage 但正常结束：按本地估算计费，来源标成估算。
func TestChatStreamEstimatesWhenUpstreamOmitsUsage(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "done", FinishReason: "stop"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	if res.Charge == nil {
		t.Fatal("正常结束必须结算")
	}
	if !res.Estimated {
		t.Error("用量是估算的，Estimated 应为 true")
	}
	if res.Charge.UsageSource != tokens.SourceEstimated {
		t.Errorf("用量来源 = %v，期望估算", res.Charge.UsageSource)
	}
	if res.Charge.Settlement.Actual.IsZero() {
		t.Error("估算出来的用量不该是零：那等于这次请求白送")
	}
}

// ── 客户端断开 ──────────────────────────────────────────────────────────

// 客户端在流中途断开：**要计费**。已经产生的 token 上游是算了钱的，
// 而它的 usage 分片永远不会到达——只能本地估。
func TestChatStreamClientDisconnectIsBilled(t *testing.T) {
	h := newHarness(t)
	hold := make(chan struct{})
	defer close(hold)
	useStreamExecutor(h, &scriptedExecutor{
		script: map[int64][]channel.StreamEvent{1: {delta("你好")}},
		hold:   map[int64]chan struct{}{1: hold},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res, err := h.gw.ChatStream(ctx, request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	// 收到第一片之后客户端就断了。
	ev, ok := <-res.Events
	if !ok || ev.Kind != "delta" {
		t.Fatalf("第一片 = %+v，期望一个 delta", ev)
	}
	cancel()

	collect(t, res)

	if res.Charge == nil {
		t.Fatal("客户端断开必须计费：已经产生的 token 上游算了钱")
	}
	if !res.Estimated {
		t.Error("断开时拿不到上游的 usage，Estimated 应为 true")
	}
	if res.Charge.UsageSource != tokens.SourceEstimated {
		t.Errorf("用量来源 = %v，期望估算", res.Charge.UsageSource)
	}
	// 金额按本地估算算：输入（正文 + 一条消息的框架开销）与输出各乘单价。
	// 这两个数是**具体值**而不是「大于零」——估多估少都要能被看出来。
	//
	// 输入 5 token（"hi" 估 1 + 一条消息的框架开销 4），输出 2 token（"你好"）：
	// 5 × 1e-6 + 2 × 2e-6 = 0.00000900
	if got := res.Charge.Settlement.Actual.String(); got != "0.00000900" {
		t.Errorf("金额 = %s，期望 0.00000900（5 个输入 token + 2 个输出 token）", got)
	}
	// 断开时**不能**释放：钱要真的扣掉。释放意味着这次请求不计费。
	if len(h.reservations.released) != 0 {
		t.Errorf("客户端断开仍要计费，不该释放预留：%+v", h.reservations.released)
	}
	if len(h.settlements.saved) != 1 {
		t.Errorf("结算落库 %d 次，期望 1 次", len(h.settlements.saved))
	}
	// 落库的那份账单要带着「客户端断的」这个事实：从判定到落库中间隔着
	// 三层（pumpStream → Outcome → Charge），任何一层漏掉它，库里那一列
	// 就永远是 NULL，而这条链上的每一层单看都像是写了的。
	if len(h.settlements.saved) == 1 && !h.settlements.saved[0].ClientCanceled {
		t.Error("结算落库的账单没带上「客户端取消」：client_canceled_at 会永远是 NULL")
	}
}

// ── 上游中途失败 ────────────────────────────────────────────────────────

// 已经吐字之后上游报错：**不换渠道**，已经产生的部分照计费。
//
// 换渠道会让客户端看到两段拼接的回答，而它无法察觉——这是流式那条路上
// 最不能犯的错。
func TestChatStreamDoesNotFailoverAfterOutputStarted(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}

	cause := errors.New("上游在流中途断开")
	exec := useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("半句"), {Kind: "error", Err: cause}},
		2: {delta("另一段"), {Kind: "done"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("上游已接受请求时不该返回错误（那时响应头可能已经发出去了）: %v", err)
	}
	events := collect(t, res)

	if got := exec.calledChannels(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("被调用的渠道 = %v，期望只有 1——已经吐字之后不能再换渠道", got)
	}

	// 错误事件要带上 ErrOutputStarted，调用方据此知道「这是截断，不是
	// 服务不可用」——后者会让它直接重试。
	var found bool
	for _, ev := range events {
		if ev.Kind != "error" {
			continue
		}
		found = true
		if !errors.Is(ev.Err, route.ErrOutputStarted) {
			t.Errorf("错误 = %v，期望包着 route.ErrOutputStarted", ev.Err)
		}
		if !errors.Is(ev.Err, cause) {
			t.Errorf("包装不该丢掉最初那个错误: %v", ev.Err)
		}
	}
	if !found {
		t.Fatal("错误事件没有被转发出去")
	}

	if res.Channel.ID != 1 {
		t.Errorf("最终渠道 = %d，期望 1", res.Channel.ID)
	}
	if res.Charge == nil {
		t.Fatal("已经吐出的部分要计费")
	}
	if !res.Estimated {
		t.Error("中途失败拿不到上游的 usage，Estimated 应为 true")
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("已经吐字之后失败仍要计费，不该释放预留：%+v", h.reservations.released)
	}
}

// 上游接受请求之后、吐出任何内容之前就失败：等于没成功返回，不计费。
func TestChatStreamUpstreamErrorBeforeOutputIsNotBilled(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {{Kind: "error", Err: errors.New("上游在首片之前断开")}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	events := collect(t, res)

	if len(events) != 1 || events[0].Kind != "error" {
		t.Fatalf("事件 = %+v，期望一个错误事件", events)
	}
	// 一个字都没吐：这不是「截断」，不该套上 ErrOutputStarted。
	if errors.Is(events[0].Err, route.ErrOutputStarted) {
		t.Error("还没吐字时的失败不是「已经吐字」，不该套 ErrOutputStarted")
	}
	if res.Charge != nil {
		t.Errorf("没有产生任何内容时不该计费，得到 %+v", res.Charge)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("不该落结算：%+v", h.settlements.saved)
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("不计费就必须释放预留，实际释放 %d 次", len(h.reservations.released))
	}
}

// 上游连接被直接关掉、没有结束标记：不能当成正常结束，已吐字的部分照计费。
func TestChatStreamTruncatedWithoutDoneIsBilled(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("半句")}, // 没有 done
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	events := collect(t, res)

	for _, ev := range events {
		if ev.Kind == "done" {
			t.Error("上游没给结束标记时不该伪造一个 done")
		}
	}
	if res.Charge == nil {
		t.Fatal("已经吐出的部分要计费")
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("已吐字之后截断仍要计费，不该释放预留：%+v", h.reservations.released)
	}
}

// ── 上游接受请求之前失败 ────────────────────────────────────────────────

// 所有候选都在接受请求之前失败：ChatStream 返回错误，调用方还能回一个
// 正常的错误响应；不计费，预留释放。
func TestChatStreamPreAcceptFailureReturnsError(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{preErr: map[int64]error{
		1: &channel.ErrUpstream{StatusCode: 503, Retryable: true},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err == nil {
		t.Fatal("全部候选失败应返回错误")
	}
	if res != nil {
		t.Errorf("失败不该有结果: %+v", res)
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("不计费就必须释放预留，实际释放 %d 次", len(h.reservations.released))
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("不该落结算：%+v", h.settlements.saved)
	}
}

// 接受请求之前失败**可以**换渠道：第一个 503，第二个接上。
func TestChatStreamFailsOverBeforeAccept(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}

	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	exec := useStreamExecutor(h, &scriptedExecutor{
		preErr: map[int64]error{1: &channel.ErrUpstream{StatusCode: 503, Retryable: true}},
		script: map[int64][]channel.StreamEvent{
			2: {{Kind: "usage", Usage: &usage}, {Kind: "done", FinishReason: "stop"}},
		},
	})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("应换渠道成功: %v", err)
	}
	collect(t, res)

	if got := exec.calledChannels(); len(got) != 2 {
		t.Fatalf("被调用的渠道 = %v，期望试过两个", got)
	}
	if res.Channel.ID != 2 {
		t.Errorf("最终渠道 = %d，期望 2", res.Channel.ID)
	}
	if len(res.Attempts) != 2 {
		t.Errorf("尝试数 = %d，期望 2（失败的也要记下来）", len(res.Attempts))
	}
	if res.Charge == nil {
		t.Fatal("接上的那次要计费")
	}
	// 12 × 1e-6 + 7 × 2e-6
	if got := res.Charge.Settlement.Actual.String(); got != "0.00002600" {
		t.Errorf("金额 = %s，期望 0.00002600", got)
	}
}

// 没有可用渠道时不预留、不调上游。
func TestChatStreamNoCandidatesDoesNotReserve(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = nil

	_, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if !errors.Is(err, route.ErrNoCandidates) {
		t.Errorf("期望 ErrNoCandidates，得到 %v", err)
	}
	if len(h.reservations.saved) != 0 || len(h.reservations.released) != 0 {
		t.Errorf("没有候选时既不该预留也不该释放: saved=%d released=%d",
			len(h.reservations.saved), len(h.reservations.released))
	}
}

// 没有执行器时立刻报错，而不是留一个空的事件流。
func TestChatStreamNoExecutor(t *testing.T) {
	h := newHarness(t)
	h.gw.Executor = nil

	if _, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{}); !errors.Is(err, ErrNoExecutor) {
		t.Errorf("期望 ErrNoExecutor，得到 %v", err)
	}
}

// ── 结算失败 ────────────────────────────────────────────────────────────

// 结算落库失败时那笔钱不能冻着：流已经结束了，这条错误只能留痕，
// 而钱由释放那条路放开——与非流式那条规则一致。
func TestChatStreamSettlementFailureReleases(t *testing.T) {
	h := newHarness(t)
	h.settlements.err = errors.New("账本写不进去")
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {{Kind: "done", FinishReason: "stop"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("结算失败发生在流开始之后，不能作为返回值交出去: %v", err)
	}
	collect(t, res)

	if res.Charge != nil {
		t.Error("结算没落库就不该交出账单")
	}
	if len(h.reservations.released) != 1 {
		t.Fatalf("结算落库失败必须释放预留，实际释放 %d 次", len(h.reservations.released))
	}
}

// ── 与调用方的约定 ──────────────────────────────────────────────────────

// 结算在事件流关闭**之前**完成：调用方读完流就不必再等一个信号，
// 测试也不必 sleep（本文件里的断言都建立在这条约定上）。
func TestChatStreamSettlesBeforeClosingEvents(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("好"), {Kind: "done", FinishReason: "stop"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, res)

	// 读完之后立刻断言，中间没有 sleep、没有轮询。
	if len(h.settlements.saved) != 1 {
		t.Errorf("读完事件流时结算应当已经落库，实际 %d 次", len(h.settlements.saved))
	}
}

// 请求快照仍然在**调上游之前**采集，流式那条路也一样。
func TestChatStreamSnapshotTakenBeforeUpstream(t *testing.T) {
	var redactsAtCall int
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {{Kind: "done"}},
	}})
	h.gw.Executor = &countingRedactExecutor{
		inner:   h.gw.Executor,
		redacts: func() int { return h.redactor.redacts },
		seen:    &redactsAtCall,
	}

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, res)

	if redactsAtCall != 1 {
		t.Errorf("调上游时的脱敏次数 = %d，期望 1（快照要先采集）", redactsAtCall)
	}
}

// ── 估算口径 ────────────────────────────────────────────────────────────

// streamUsage：上游报过就用实测，没报就用估算，且来源与数字一起给。
func TestStreamUsagePrefersMeasured(t *testing.T) {
	measured := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	est := tokens.NewStreamEstimator(nil, "gpt-4o")
	est.ObserveDelta("这段估算出来的内容应当被实测覆盖")

	usage, source := streamUsage(est, &measured)
	if usage.PromptTokens != measured.PromptTokens || usage.CompletionTokens != measured.CompletionTokens {
		t.Errorf("用量 = %+v，期望上游报的那一份 %+v", usage, measured)
	}
	if source != tokens.SourceMeasured {
		t.Errorf("来源 = %v，期望实测", source)
	}

	usage, source = streamUsage(est, nil)
	if source == tokens.SourceMeasured {
		t.Error("没有实测值时的来源不该是实测")
	}
	if usage.CompletionTokens != est.Result().Output.Tokens {
		t.Errorf("输出 = %d，期望估算出来的 %d",
			usage.CompletionTokens, est.Result().Output.Tokens)
	}
	if usage.PromptTokens != 0 {
		// 没调 ObserveRequest 时输入记 0，而这不是「没有输入」——
		// tokens 那一侧会在 Note 里说明，这里只钉住它不凭空造一个数。
		t.Errorf("没有输入观察时的输入 = %d，期望 0", usage.PromptTokens)
	}
}

// 估算用的是**整个请求的正文**，不是最后一条消息：一个 agent 循环里
// 输入是输出的几十倍，只算最后一条会让账单偏得很远。
func TestChatStreamEstimatesWholeRequestInput(t *testing.T) {
	h := newHarness(t)
	hold := make(chan struct{})
	defer close(hold)
	useStreamExecutor(h, &scriptedExecutor{
		script: map[int64][]channel.StreamEvent{1: {delta("x")}},
		hold:   map[int64]chan struct{}{1: hold},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream := &channel.Request{
		Model: "gpt-4o",
		Messages: []channel.Message{
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "第一轮的输入内容比较长"}}},
			{Role: "assistant", Content: []channel.ContentBlock{{Type: "text", Text: "第一轮的回答"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	res, err := h.gw.ChatStream(ctx, request(), upstream, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	<-res.Events
	cancel()
	collect(t, res)

	if res.Charge == nil {
		t.Fatal("客户端断开必须计费")
	}
	// 三段正文的估算都算进去，再加三条消息的框架开销。只算最后一条
	// （"hi" 估 1 + 框架 4 = 5）时金额会小得多。
	// 输入 26 token：三段正文估 8 + 5 + 1 = 14，加 3 条 × 4 框架开销 = 26。
	// 输出 1 token。26 × 1e-6 + 1 × 2e-6 = 0.00002800
	if got := res.Charge.Settlement.Actual.String(); got != "0.00002800" {
		t.Errorf("金额 = %s，期望 0.00002800（整个请求的输入，不只是最后一条消息）", got)
	}
	if res.Charge.Usage.PromptTokens != 26 {
		t.Errorf("输入 = %d，期望 26（三段正文 + 三条消息的框架开销）",
			res.Charge.Usage.PromptTokens)
	}
}
