package gateway

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/pipeline"
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
	// sentRefs 按顺序记下交给执行器的每一份请求的**指针**。
	//
	// 与 fakeExecutor.sentRefs 同一个用途：请求快照该采的是发出去的那一个
	// 对象，而按值比较分不出「同一个」与「内容相同」（见
	// snapshot_request_test.go）。
	sentRefs []*channel.Request
}

func (s *scriptedExecutor) Chat(context.Context, channel.Channel, *channel.Request) (*channel.Response, error) {
	return nil, errors.New("流式用例不该走非流式入口")
}

func (s *scriptedExecutor) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	s.mu.Lock()
	s.calls = append(s.calls, ch.ID)
	s.sentRefs = append(s.sentRefs, req)
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

// 不计费的流式失败同样要带上「打到了哪个渠道、用哪个上游名」。
//
// 非流式那条路由 TestUpstreamFailureCarriesEvidenceToRelease 钉着；流式这条
// 路有两种不计费的失败，它们走的是**不同的**收尾出口（前者直接
// settleAndFail + finish，后者在 pumpStream 里），两条都要写进失败账单行
// ——它们都真的打到了渠道 1。没有这个值，失败行上那两列是 NULL，与「确实
// 没有渠道可记」在库里逐列相同。
func TestChatStreamFailureCarriesPlacementToRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec *scriptedExecutor
	}{
		{
			name: "上游接受请求之前就失败",
			exec: &scriptedExecutor{preErr: map[int64]error{
				1: &channel.ErrUpstream{StatusCode: 500, Retryable: false},
			}},
		},
		{
			name: "接受之后没吐字就断开",
			exec: &scriptedExecutor{script: map[int64][]channel.StreamEvent{
				1: {{Kind: "error", Err: errors.New("上游在首片之前断开")}},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			useStreamExecutor(h, tc.exec)

			res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
			if err == nil {
				// 收尾在 pumpStream 里，读完流才等得到释放。
				collect(t, res)
			}
			if len(h.reservations.released) != 1 {
				t.Fatalf("不计费的结局应释放预留一次，实际 %d 次", len(h.reservations.released))
			}
			rel := h.reservations.released[0].req
			if rel.ChannelID != 1 {
				t.Errorf("释放时带的 channel_id = %d，期望 1", rel.ChannelID)
			}
			if rel.UpstreamModel != "gpt-4o" {
				t.Errorf("释放时带的 upstream_model = %q，期望 gpt-4o", rel.UpstreamModel)
			}
		})
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

// ── 收尾的预算从哪一刻开始算 ────────────────────────────────────────────

// withSettleTimeout 把收尾的预算压到毫秒级（见 Gateway.SettleTimeout）。
//
// 不压的话，「一条跑得比收尾预算久的流」要真等十秒才走得到收尾。这条用例验的
// 是**预算从哪一刻开始算**，与那个数本身多大无关，所以形状不变、时间省掉。
func withSettleTimeout(d time.Duration) func(*harness) {
	return func(h *harness) { h.settleTimeout = d }
}

// 跑得比收尾预算久的流**照样记账**：那段预算从收尾那一刻开始算，不是从流的
// 第一片开始算。
//
// 这是 #529 的形状。预算从流的开头算起时，一条跑过十秒的流（LLM 的流跑过十秒
// 是常态）在收尾时拿到的是一份早就过期的 ctx，结算与落库一并失败，于是按既有
// 规则把预留释放掉——钱一分没收，而调用方看到的是一次正常结束的流、日志里只有
// 一行，对账时表现为余额与用量对不上。
//
// 夹具里 hold 的那段等待，就是真实世界里上游在两个分片之间慢慢吐的那段时间：
// 对「这一次收尾」来说两者是同一件事——泵流的那个 goroutine 活过了预算才走到
// 收尾那一段。
func TestChatStreamSettlesAfterStreamOutlivesTheBudget(t *testing.T) {
	const budget = 100 * time.Millisecond
	h := newHarness(t, withSettleTimeout(budget))

	hold := make(chan struct{})
	useStreamExecutor(h, &scriptedExecutor{
		script: map[int64][]channel.StreamEvent{
			1: {delta("半句"), {Kind: "done", FinishReason: "stop"}},
		},
		hold: map[int64]chan struct{}{1: hold},
	})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	// 让这条流活过收尾的预算，再放它走到收尾。
	//
	// 睡的是固定时长，但它只会**变长**（机器忙的时候），而这条用例要的正是
	// 「流比预算久」——方向对得上，不会偶发失败。
	time.Sleep(3 * budget)
	close(hold)
	collect(t, res)

	// 账要记上：结算落库成功，而预留**不**释放（冻着的钱由结算那一步处置）。
	if res.Charge == nil {
		t.Fatal("流正常结束了就该交出账单，实际没有——收尾那一步拿到的 ctx 已经过期")
	}
	if len(h.settlements.saved) != 1 {
		t.Errorf("结算落库 %d 次，期望 1", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("结算成功就不该释放预留，实际释放 %d 次", len(h.reservations.released))
	}
}

// 收尾预算那个旋钮的三支：零值走常量、正数走注入、负数也走常量。
//
// 形状照 TestNowFallsBackToRealClock：那一条管的是「注入被移除时没人拦得住」，
// 这一条同理。上面那条长流用例只钉住**deadline 从哪一刻开始算**，把
// newHarness 里那行 wiring 删掉它照样绿（预算回到十秒的常量，300ms 的流过不了期）；
// 而「注入真的被读到了」要这一条来钉。
//
// 三支各有各的红法：删掉 wiring → 第二条变红；把 settleTimeout 的 `> 0` 改成
// `>= 0` → 第一条变红。负数那一支只是跟着正数走同一个判据（`> 0` 挡住 0 与负数），
// 不给它单独的语义——它不是会出现的输入，只是「旋钮怎么读」这件事的一个边界。
func TestSettleTimeoutFallsBackToTheConstant(t *testing.T) {
	cases := []struct {
		name     string
		injected time.Duration
		want     time.Duration
	}{
		{"没有注入", 0, streamSettleTimeout},
		{"注入了正数", 3 * time.Second, 3 * time.Second},
		{"注入了负数", -time.Second, streamSettleTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withSettleTimeout(c.injected))
			if got := h.gw.settleTimeout(); got != c.want {
				t.Errorf("settleTimeout() = %v，期望 %v", got, c.want)
			}
		})
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

// 流式那条路同样要把选路给出的上游名交给结算：两条路各写一份结算调用，
// 只钉一条的话另一条漏搬不会有任何迹象。
func TestChatStreamSettlementUsesActualUpstreamModel(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.aliases = fixedAliases{upstream: "vendor/gpt-4o"}
	})
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("好"), {Kind: "done", FinishReason: "stop"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, res)

	if len(h.settlements.reqs) != 1 {
		t.Fatalf("结算应当落库一次，实际 %d 次", len(h.settlements.reqs))
	}
	if got := h.settlements.reqs[0].UpstreamModel; got != "vendor/gpt-4o" {
		t.Errorf("结算带的上游模型名 = %q，期望选路给出的 %q", got, "vendor/gpt-4o")
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

// ── 响应方向的快照 ──────────────────────────────────────────────────────

// 一条正常结束的流要落一行响应方向的快照，内容是**上游回来的**那份响应。
//
// 事件流里没有整份响应的形状，这一行是网关把事件归一回 channel.Response
// 得来的（见 streamResponseBuilder）。用它钉住归一：拼错了不会报错，
// 只会让快照少一块内容。
func TestStreamRecordsResponseSnapshot(t *testing.T) {
	h := newHarness(t)
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7, Reported: true}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			delta("你"),
			delta("好"),
			{Kind: "usage", Usage: &usage},
			{Kind: "done", FinishReason: "stop"},
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	snap := snaps[0]
	if snap.UpstreamStatus == nil || *snap.UpstreamStatus != http.StatusOK {
		t.Errorf("upstream_status = %v，期望 %d（start 事件里带的就是它）",
			snap.UpstreamStatus, http.StatusOK)
	}
	if snap.ErrorKind != nil {
		t.Errorf("正常结束时 error_kind 应为 nil，实际 %q", *snap.ErrorKind)
	}

	bodies := h.redactor.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("交给脱敏器的响应 = %d 份，期望 1", len(bodies))
	}
	body := bodies[0]
	if body == nil {
		t.Fatal("正常结束的流不该交一份 nil 响应给脱敏器")
	}
	if body.UpstreamModel != "gpt-4o" {
		t.Errorf("归一后的上游模型 = %q，期望选路给出的 gpt-4o", body.UpstreamModel)
	}
	if body.FinishReason != "stop" {
		t.Errorf("finish_reason = %q，期望 stop", body.FinishReason)
	}
	if len(body.Message.Content) != 1 || body.Message.Content[0].Text != "你好" {
		t.Errorf("归一后的内容 = %#v，期望把两段增量拼成一个文本块「你好」", body.Message.Content)
	}
	if body.Usage.PromptTokens != 12 || body.Usage.CompletionTokens != 7 {
		t.Errorf("归一后的用量 = %+v，期望 12/7", body.Usage)
	}
}

// 上游在接受请求之前就失败：仍然落一行响应方向的快照。
func TestStreamPreAcceptFailureRecordsResponse(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{preErr: map[int64]error{
		1: &channel.ErrUpstream{StatusCode: http.StatusBadGateway, GotResponse: true},
	}})

	if _, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游在接受请求前失败，ChatStream 应当报错")
	}

	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	if snaps[0].UpstreamStatus == nil || *snaps[0].UpstreamStatus != http.StatusBadGateway {
		t.Errorf("upstream_status = %v，期望 %d", snaps[0].UpstreamStatus, http.StatusBadGateway)
	}
	if snaps[0].ErrorKind == nil || *snaps[0].ErrorKind != "upstream_http_error" {
		t.Errorf("error_kind = %v，期望 upstream_http_error", snaps[0].ErrorKind)
	}
}

// 上游接受了请求、一个字没吐就结束：状态码是 start 事件里的那个真实值，
// 分类是 upstream_stream_aborted——与账单上的口径一致。
func TestStreamAbortedRecordsResponse(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {{Kind: "start", StatusCode: http.StatusOK}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	if snaps[0].UpstreamStatus == nil || *snaps[0].UpstreamStatus != http.StatusOK {
		t.Errorf("upstream_status = %v，期望 %d（拿到过响应这个事实成立）",
			snaps[0].UpstreamStatus, http.StatusOK)
	}
	if snaps[0].ErrorKind == nil || *snaps[0].ErrorKind != "upstream_stream_aborted" {
		t.Errorf("error_kind = %v，期望 upstream_stream_aborted", snaps[0].ErrorKind)
	}
	// 什么都没观察到时交 nil，让骨架为空而不是一份「上游报了零用量」的假象。
	bodies := h.redactor.responseBodies()
	if len(bodies) != 1 || bodies[0] != nil {
		t.Errorf("没吐字的流应当交 nil 响应体，实际 %#v", bodies)
	}
}

// ── 引用标注 ────────────────────────────────────────────────────────────

// 只收到标注、随后上游失败：标注同样是「客户端已经看见了的东西」，所以
// 它要算进 started，这条流按「已经吐过字」处置——带 ErrOutputStarted、
// 照常计费，而不是当成「一个字没吐、可以重试」。
//
// 这条用例钉住的是那个赋值的**后果**，不是赋值本身：删掉 pumpStream 里
// "annotation" 分支的 started = true，这条流会退回「接受请求后一个字没吐
// 就失败」——不计费、释放预留、错误里也不带 ErrOutputStarted。
func TestChatStreamAnnotationCountsAsOutputStarted(t *testing.T) {
	h := newHarness(t)
	usage := channel.Usage{PromptTokens: 5, CompletionTokens: 0}
	cause := errors.New("上游在标注之后断开")
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			{Kind: "annotation", Annotations: []map[string]any{{"type": "url_citation", "url": "https://example.com/a"}}},
			{Kind: "usage", Usage: &usage},
			{Kind: "error", Err: cause},
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("上游已接受请求时不该返回错误: %v", err)
	}
	events := collect(t, res)

	// 标注帧照常转发（它不在任何 continue 分支里）。
	var forwarded bool
	for _, ev := range events {
		if ev.Kind == "annotation" {
			forwarded = true
		}
	}
	if !forwarded {
		t.Error("标注事件没有被转发给调用方")
	}

	// 错误要带 ErrOutputStarted：客户端手里已经有标注帧，重试会把两批标注
	// 接在一起，所以这里不能报「服务暂时不可用」。
	var foundErr bool
	for _, ev := range events {
		if ev.Kind != "error" {
			continue
		}
		foundErr = true
		if !errors.Is(ev.Err, route.ErrOutputStarted) {
			t.Errorf("错误 = %v，期望包着 route.ErrOutputStarted", ev.Err)
		}
		if !errors.Is(ev.Err, cause) {
			t.Errorf("包装不该丢掉最初那个错误: %v", ev.Err)
		}
	}
	if !foundErr {
		t.Fatal("错误事件没有被转发出去")
	}

	// 已经吐过字：走计费路径，预留由结算解冻而不是释放。
	if res.Charge == nil {
		t.Fatal("客户端已经看到标注，这条流要按已经吐过字计费")
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("已经吐过标注之后不该释放预留：%+v", h.reservations.released)
	}
	if len(h.settlements.saved) != 1 {
		t.Fatalf("应当落一次结算，实际 %d 次", len(h.settlements.saved))
	}

	// 输出为 0：标注不参与分词，金额只来自输入。
	if got := res.Charge.Settlement.Actual.String(); got != "0.00000500" {
		t.Errorf("金额 = %s，期望 0.00000500（5 个输入 token，输出为 0）", got)
	}
	if res.Charge.ClientCanceled {
		t.Error("上游报错被标成了客户端取消")
	}
}

// 标注先于正文到达：builder 先把它攒起来，等第一段正文出现时再挂上去，
// 不为它单独造一个空正文块。
//
// 直接测 builder 而不是快照：快照的响应骨架是显式字段清单，**不摊
// Annotations**（见 internal/snapshot），通过快照断言看不到挂载结果。
func TestStreamResponseBuilderFlushesEarlyAnnotationsOntoLaterText(t *testing.T) {
	early := map[string]any{"type": "url_citation", "url": "https://example.com/early"}
	late := map[string]any{"type": "url_citation", "url": "https://example.com/late"}

	b := newStreamResponseBuilder()
	b.observe(channel.StreamEvent{Kind: "annotation", Annotations: []map[string]any{early}})

	// 还没等来正文：不为标注造块。
	if len(b.blocks) != 0 {
		t.Fatalf("正文还没出现时不该造块，实际 %#v", b.blocks)
	}

	b.observe(delta("引用在这里"))
	b.observe(channel.StreamEvent{Kind: "annotation", Annotations: []map[string]any{late}})

	if len(b.blocks) != 1 {
		t.Fatalf("块数 = %d，期望 1（两批标注都挂到同一个正文块上）", len(b.blocks))
	}
	blk := b.blocks[0]
	if blk.Type != "text" || blk.Text != "引用在这里" {
		t.Errorf("块 = %+v，期望一个正文块「引用在这里」", blk)
	}
	if len(blk.Annotations) != 2 {
		t.Fatalf("标注数 = %d，期望 2（先到的被 flush，后到的直接挂上）", len(blk.Annotations))
	}
	// 顺序也要钉：先到的那批必须在前，否则「按到达顺序」这个口径就没了。
	if blk.Annotations[0]["url"] != "https://example.com/early" ||
		blk.Annotations[1]["url"] != "https://example.com/late" {
		t.Errorf("标注顺序 = %v，期望先到的 early 在后到的 late 之前", blk.Annotations)
	}
}

// 一个字没吐就结束，而流里那条错误帧带着 *channel.ErrUpstream（200 + 体里
// error）时，**快照与账单必须给出同一个分类**。
//
// 这是这次改动修的那个分歧：快照原先先看 streamErr，于是同一次失败在快照上
// 是 upstream_unusable_response、在账单上是 upstream_stream_aborted。判据
// 的顺序改成「先判有没有吐字」之后，两边都落进账单那一档。
//
// 断言两只脚都钉：账单那一侧的释放依据（failure.Code）与快照的 error_kind。
// 只钉一侧的话，改回去时另一侧照样能过。
func TestStreamFailureBeforeOutputClassifiedSameOnBothSides(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "上游 200 但体里是 error",
			err:  &channel.ErrUpstream{StatusCode: http.StatusOK, GotResponse: true, Body: "upstream said: overloaded"},
		},
		{
			name: "吐字前普通错误",
			err:  errors.New("上游在首片之前断开"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
				1: {
					{Kind: "start", StatusCode: http.StatusOK},
					{Kind: "error", Err: tc.err},
				},
			}})

			res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			collect(t, res)

			// 账单：不计费，依据是「流在接受之后断了」。
			if len(h.reservations.released) != 1 {
				t.Fatalf("一个字没吐的结局应释放预留一次，实际 %d 次", len(h.reservations.released))
			}
			if got := h.reservations.released[0].failure; got == nil ||
				got.Code != pipeline.ErrorUpstreamStreamAborted {
				t.Fatalf("账单依据 = %+v，期望 %q", got, pipeline.ErrorUpstreamStreamAborted)
			}

			// 快照：同一个值。
			var snaps []*Snapshot
			waitFor(t, "响应方向的快照落库", func() bool {
				snaps = h.snapshots.byDirection("response")
				return len(snaps) == 1
			})
			if snaps[0].ErrorKind == nil || *snaps[0].ErrorKind != pipeline.ErrorUpstreamStreamAborted {
				t.Errorf("快照的 error_kind = %s，期望 %q（与账单同一个值）",
					kindOf(snaps[0].ErrorKind), pipeline.ErrorUpstreamStreamAborted)
			}
			if snaps[0].UpstreamStatus == nil || *snaps[0].UpstreamStatus != http.StatusOK {
				t.Errorf("upstream_status = %v，期望 %d（上游确实回过一个响应）",
					snaps[0].UpstreamStatus, http.StatusOK)
			}
		})
	}
}

// 吐过字之后上游把连接掐断（没有结束标记、也没有错误帧）：两表都记
// upstream_stream_truncated，而且这次**计了费**。
//
// 这条是 issue #461 的第二件事。改动之前两表都是 NULL，而 NULL 与「记录
// 没写完」在快照表里是同一个值；而这一次回答是真的产生了 token、账也真的
// 扣了钱——「这次被截断了」必须能事后读出来。
//
// 「计了费」这一半不能省：只钉分类的话，一个把截断当成失败去释放预留的
// 实现照样能过，而那是一次真实产生的费用没被记下。
func TestStreamTruncatedAfterOutputClassifiedAndBilled(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			delta("半句"), // 没有 done，也没有 error：连接就这样没了
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	// 结算发生了，预留没有被释放。
	if res.Charge == nil || len(h.settlements.saved) != 1 {
		t.Fatalf("已经吐出来的部分要计费：Charge=%v，落库 %d 条", res.Charge, len(h.settlements.saved))
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("已吐字之后截断仍要计费，不该释放预留：%+v", h.reservations.released)
	}

	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	if snaps[0].ErrorKind == nil || *snaps[0].ErrorKind != pipeline.ErrorUpstreamStreamTruncated {
		t.Errorf("快照的 error_kind = %s，期望 %q",
			kindOf(snaps[0].ErrorKind), pipeline.ErrorUpstreamStreamTruncated)
	}
	if snaps[0].UpstreamStatus == nil || *snaps[0].UpstreamStatus != http.StatusOK {
		t.Errorf("upstream_status = %v，期望 %d", snaps[0].UpstreamStatus, http.StatusOK)
	}
	// 骨架里要留着上游吐到一半的那份内容：快照记的是「回来的是什么」。
	bodies := h.redactor.responseBodies()
	if len(bodies) != 1 || bodies[0] == nil {
		t.Fatalf("截断的流仍然交回了内容，不该是 nil 响应体：%#v", bodies)
	}
}

// 吐过字之后上游在流里报错：分类按那个错误推，不按「截断」。
//
// 与上一条只差一个错误帧，而两件事在库里的说法不同：「流被截断」说的是
// 上游没给结束标记就断了，「上游回了错误」说的是它明确报了一个错。状态码
// 取错误帧里带的那个——它是上游那一刻真的回过的码，start 事件里的 200
// 只是接受请求时的信封。
func TestStreamErrorFrameAfterOutputClassifiedUpstreamError(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			delta("半句"),
			{Kind: "error", Err: &channel.ErrUpstream{
				StatusCode: http.StatusBadGateway, GotResponse: true,
			}},
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("上游已接受请求时不该返回错误: %v", err)
	}
	collect(t, res)

	// 吐过字就有 token 产生，照常计费。
	if res.Charge == nil || len(h.settlements.saved) != 1 {
		t.Fatalf("已经吐出来的部分要计费：Charge=%v，落库 %d 条", res.Charge, len(h.settlements.saved))
	}

	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	// 分类写成字面量而不是拿 pipeline 的常量来比：它是要落进快照、被人
	// 按字面读的取值，改了就该有一条红用例。
	if snaps[0].ErrorKind == nil || *snaps[0].ErrorKind != "upstream_http_error" {
		t.Errorf("快照的 error_kind = %s，期望 %q（上游报了错，不是静默截断）",
			kindOf(snaps[0].ErrorKind), "upstream_http_error")
	}
	if got := statusOf(snaps[0].UpstreamStatus); got != strconv.Itoa(http.StatusBadGateway) {
		t.Errorf("upstream_status = %s，期望 %d（错误帧里带的那个码）",
			got, http.StatusBadGateway)
	}
}

// 上游接受了请求、给了结束标记、什么都没吐：这一条快照仍然要写。
//
// 不采集的条件是**三者全空**，它说的是「这次请求根本没走到上游」（见
// responseSnapshotAsync 的注释）。这一条里状态码在——上游接受了请求，只是
// 没有内容、也没有结束原因——所以该有一行记着它。把那个条件放宽一格，这一类
// 会被整个丢掉，而它是「上游接受了请求、然后什么都没给」的唯一记录。
func TestStreamEmptyContentWithStatusStillRecordsResponse(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			{Kind: "done"},
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	// 脱敏是同步的（只有落库在后台，见 responseSnapshotAsync），所以这里
	// 不必等：少交一份就是少交一份。
	bodies := h.redactor.responseBodies()
	if len(bodies) != 1 {
		t.Fatalf("交给脱敏器的响应 = %d 份，期望 1（三者不全空就该采）", len(bodies))
	}
	if bodies[0] != nil {
		t.Errorf("没有内容的流应当交 nil 响应体，实际 %#v", bodies[0])
	}

	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	if got := statusOf(snaps[0].UpstreamStatus); got != strconv.Itoa(http.StatusOK) {
		t.Errorf("upstream_status = %s，期望 %d（start 事件里带的那个）", got, http.StatusOK)
	}
	if snaps[0].ErrorKind != nil {
		t.Errorf("没有内容但正常结束，不该有分类，实际 %s", kindOf(snaps[0].ErrorKind))
	}
}

// 正常结束（有 done、没报错）两表都没有分类。
//
// 它与截断那条只差一个 done——把判据写成「没报错就算截断」时，这条会红。
func TestStreamNormalEndHasNoClassification(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {
			{Kind: "start", StatusCode: http.StatusOK},
			delta("你好"),
			{Kind: "done", FinishReason: "stop"},
		},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	if snaps[0].ErrorKind != nil {
		t.Errorf("正常结束的流不该有分类，实际 %q", kindOf(snaps[0].ErrorKind))
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("正常结束要计费，不该释放预留：%+v", h.reservations.released)
	}
	if res.Charge == nil {
		t.Error("正常结束应当计费")
	}
}

// 攒着的标注挂到「最后一个正文块」上，而最后一个块不一定是正文。
//
// 事件流里块与块之间只由工具调用分隔，于是正文、工具调用、标注可能依次到达：
// 标注落地时最后一块是工具调用，它要往回找到正文那一块。这一条就是让那个往回
// 找的循环真的走一轮——此前每一条用例里最后一块都是正文，第一次判断就返回了。
func TestStreamResponseBuilderAttachesAnnotationBehindToolCall(t *testing.T) {
	b := newStreamResponseBuilder()
	b.observe(delta("正文在这里"))
	b.observe(channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
		Index: 0, ID: "call_1", Name: "lookup", Arguments: "{}",
	}})
	b.observe(channel.StreamEvent{Kind: "annotation", Annotations: []map[string]any{
		{"type": "url_citation", "url": "https://example.com/behind"},
	}})

	// 收尾时冲洗一次：标注还没挂出去，落点是正文那一块。
	resp := b.response("gpt-4o-2024")
	if resp == nil {
		t.Fatal("有正文与工具调用时该归一出一份响应")
	}
	if len(resp.Message.Content) != 2 {
		t.Fatalf("内容块 = %d 个，期望 2（正文 + 工具调用）", len(resp.Message.Content))
	}
	text := resp.Message.Content[0]
	if text.Type != "text" || len(text.Annotations) != 1 {
		t.Errorf("第一个块 = %+v，期望正文块带着那一条标注", text)
	}
	// 标注说的是「正文引用了哪个来源」，挂到工具调用块上等于是另一件事。
	if got := resp.Message.Content[1]; len(got.Annotations) != 0 {
		t.Errorf("第二个块 = %+v，期望工具调用块上没有标注", got)
	}
}

// 工具调用块上的 id 与 name 要留在归一后的响应里，而且**后面那些分片不能把它们
// 抹掉**。
//
// 协议里 id 与 name 只在第一个分片给出，后面的分片带空串（见 channel.ToolCallDelta），
// 所以那两行是「有值才写」而不是「有值就写空」。判据写反的表现是快照上那两格
// 永远是空的：流本身一切正常，只有回看那份响应时才发现工具调用的 id 与名字没了。
//
// 参数那一侧在同一条路上，但判据不同：分片要**拼接**，拼完是一个完整对象才写
// 进快照（下一对手）。
func TestStreamResponseBuilderKeepsToolCallFieldsAcrossDeltas(t *testing.T) {
	b := newStreamResponseBuilder()
	b.observe(channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
		Index: 0, ID: "call_abc", Name: "lookup", Arguments: `{"city":"上海"`,
	}})
	// 第二个分片：id 与 name 是空串（协议如此），参数再拼一段。
	b.observe(channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
		Index: 0, Arguments: `,"n":2}`,
	}})

	resp := b.response("gpt-4o-2024")
	if resp == nil {
		t.Fatal("有一个工具调用块时该归一出一份响应")
	}
	if len(resp.Message.Content) != 1 {
		t.Fatalf("内容块 = %d 个，期望 1（两个分片拼成同一个工具调用块）", len(resp.Message.Content))
	}
	blk := resp.Message.Content[0]
	if blk.Type != "tool_use" {
		t.Fatalf("块类型 = %q，期望 tool_use", blk.Type)
	}
	if blk.ToolUseID != "call_abc" {
		t.Errorf("ToolUseID = %q，期望 call_abc（第一个分片给的那个；后面那片空串不该盖掉它）", blk.ToolUseID)
	}
	if blk.ToolName != "lookup" {
		t.Errorf("ToolName = %q，期望 lookup（同上）", blk.ToolName)
	}
	if in := blk.ToolInput; len(in) != 2 || in["city"] != "上海" || in["n"] != float64(2) {
		t.Errorf("ToolInput = %#v，期望两个键都拼进去：city=上海、n=2", in)
	}
}

// 参数拼不成一个 JSON 对象时留空，**哪怕 json 已经把它解了一半**。
//
// 「非空就写」与这条判据在多数串上给出同一个结果——整串语法就不成立时，目标
// map 一个字节都不会被写（json.Unmarshal 先跑一遍语法校验）。分得开的是**语法
// 合法、解到中途才失败**的那一类：`{"a":1e999}` 里的数越界，报错时 map 里已经
// 落下了 a→nil。把半份参数写进快照等于替上游猜内容，而快照那条铁律是不猜
// （见 internal/snapshot）。
//
// 空串那一条钉的是另一半：参数一个分片都没到（或只给了空串）时同样留空。
func TestStreamResponseBuilderLeavesUnparsableToolInputEmpty(t *testing.T) {
	for _, tc := range []struct{ name, arguments string }{
		{"断流只到一半：少一个右括号", `{"city":"上海"`},
		{"语法合法但解到一半失败：有一个数越界", `{"a":1e999}`},
		{"一个分片都没带参数", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newStreamResponseBuilder()
			b.observe(channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
				Index: 0, ID: "call_abc", Name: "lookup", Arguments: tc.arguments,
			}})

			resp := b.response("gpt-4o-2024")
			if resp == nil {
				t.Fatal("有一个工具调用块时该归一出一份响应")
			}
			blk := resp.Message.Content[0]
			if blk.ToolInput != nil {
				t.Errorf("ToolInput = %#v，期望 nil（整串没解成功，半份结果不写进去）", blk.ToolInput)
			}
			// 参数留空不该连 id 与 name 一起丢：三样是各自独立的判据。
			if blk.ToolUseID != "call_abc" || blk.ToolName != "lookup" {
				t.Errorf("id/name = %q/%q，期望 call_abc/lookup（参数解不开不牵动这两格）",
					blk.ToolUseID, blk.ToolName)
			}
		})
	}
}

// streamResponseEvidence 把 started / done / ours 的每一格各归哪一档。
//
// 写成一张表是为了让每一格都有一句话，而不是只钉住走过的那几条路。此前缺的
// 那一格是「一个字没吐，但收到了结束标记」：上游正常结束、只是没有内容。它
// 不是中断——aborted 说的是「接受之后断掉」，而正常结束（哪怕没有内容）没有
// 上游失败可归，该留空。
func TestStreamResponseEvidenceClasses(t *testing.T) {
	for _, tc := range []struct {
		name                string
		started, done, ours bool
		want                string
	}{
		{"一个字没吐就结束", false, false, false, pipeline.ErrorUpstreamStreamAborted},
		{"空内容但正常结束", false, true, false, ""},
		{"吐过字、上游没给结束标记", true, false, false, pipeline.ErrorUpstreamStreamTruncated},
		{"吐过字且正常结束", true, true, false, ""},
		{"我们自己把这条流停下的", true, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, kind := streamResponseEvidence(http.StatusOK, nil, tc.started, tc.done, tc.ours)
			if kind != tc.want {
				t.Errorf("error_kind = %q，期望 %q", kind, tc.want)
			}
			if status != http.StatusOK {
				t.Errorf("status = %d，期望 %d", status, http.StatusOK)
			}
		})
	}
}

// 上游在流里报错时快照那两列取什么。
//
// 与上面那张表分开：那张每一行都传一个 nil 的错误，说的是 started / done /
// ours 三个布尔把结论拨到哪一档。这一张固定「吐过字之后上游报错」，按错误帧
// 带没带状态码分成两格，再加一格「没有错误帧」作对照。带没带决定状态码用谁
// 的：错误里那个是上游真的回过的码，start 事件里的那个只是接受请求时的信封。
func TestStreamResponseEvidenceFromErrorFrame(t *testing.T) {
	for _, tc := range []struct {
		name       string
		streamErr  error
		wantStatus int
		wantKind   string
	}{
		{
			"错误帧带了状态码：用它",
			&channel.ErrUpstream{StatusCode: http.StatusBadGateway, GotResponse: true},
			http.StatusBadGateway, "upstream_http_error",
		},
		{
			"错误帧没带状态码：退回 start 事件那个",
			&channel.ErrUpstream{GotResponse: true},
			http.StatusOK, "upstream_no_answer",
		},
		{
			"没有错误帧：吐过字之后静默结束算截断",
			nil,
			http.StatusOK, "upstream_stream_truncated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, kind := streamResponseEvidence(http.StatusOK, tc.streamErr, true, false, false)
			if status != tc.wantStatus {
				t.Errorf("status = %d，期望 %d", status, tc.wantStatus)
			}
			if kind != tc.wantKind {
				t.Errorf("error_kind = %q，期望 %q", kind, tc.wantKind)
			}
		})
	}
}

// 一次流式调用的耗时从哪儿起算。
//
// 起点是最后一次尝试真正发出去的那一刻；适配器没给这个时刻时，退回开始泵流
// 的那一个。退回这一步不能省：拿一个零值时刻去减，算出来的是「从公元元年
// 到现在」，而它会经账单落进 usage_events.duration_ms，延迟那一列的分位数
// 从此混进一个假样本。
func TestStreamDurationStartPoint(t *testing.T) {
	fallback := fixedAt.Add(-10 * time.Second)
	for _, tc := range []struct {
		name     string
		attempts []route.Attempt
		want     time.Duration
	}{
		{
			"适配器给了发出时刻：从它起算",
			[]route.Attempt{{ChannelID: 1, StartedAt: fixedAt.Add(-2 * time.Second)}},
			2 * time.Second,
		},
		{
			"最后一次尝试没给时刻：退回泵流的起点",
			[]route.Attempt{{ChannelID: 1}},
			10 * time.Second,
		},
		{"一次尝试都没有：退回泵流的起点", nil, 10 * time.Second},
		{
			"结束时刻早于起点：归零，不编一个正数出来",
			[]route.Attempt{{ChannelID: 1, StartedAt: fixedAt.Add(time.Second)}},
			0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamDuration(tc.attempts, fallback, fixedAt); got != tc.want {
				t.Errorf("耗时 = %s，期望 %s", got, tc.want)
			}
		})
	}
}

// kindOf 把快照的分类打成能读的形态。
//
// ErrorKind 是 *string，直接 %v 打出来的是地址——失败信息里出现一串
// 0xc000... 时，看的人得自己回去读代码才知道是哪个值。NULL 打成一个词，
// 让「没有分类」与「分类是空串」在输出里也分得开。
func kindOf(kind *string) string {
	if kind == nil {
		return "<nil>"
	}
	return strconv.Quote(*kind)
}

// statusOf 把快照里那个可空的状态码打成能读的形态。
//
// 理由与 kindOf 相同：那个字段是 *int，直接 %v 打出来的是地址，而失败信息里
// 出现一串 0xc000... 时，看的人得自己回去读代码才知道是哪个值。
func statusOf(status *int) string {
	if status == nil {
		return "<nil>"
	}
	return strconv.Itoa(*status)
}
