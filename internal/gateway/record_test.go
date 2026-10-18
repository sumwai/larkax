package gateway

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// fakeRecorder 记下每一次调用结局。
//
// 它是同步调用的（见 recordCall），所以断言可以紧跟在 Chat 返回之后，
// 不需要 waitFor 轮询。
type fakeRecorder struct {
	mu   sync.Mutex
	recs []route.CallOutcome
	// err 非空时每次记录都失败，用于验证「写健康度失败不影响请求」。
	err error
	// onRecord 非 nil 时在每次记录前调一次，供用例观察它与其他步骤的先后。
	onRecord func()
}

func (f *fakeRecorder) RecordCall(_ context.Context, rec route.CallOutcome) error {
	if f.onRecord != nil {
		f.onRecord()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, rec)
	return f.err
}

func (f *fakeRecorder) all() []route.CallOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]route.CallOutcome(nil), f.recs...)
}

func (f *fakeRecorder) count() int { return len(f.all()) }

// only 返回唯一一条记录，多于或少于一条都让用例失败。
func (f *fakeRecorder) only(t *testing.T) route.CallOutcome {
	t.Helper()
	recs := f.all()
	if len(recs) != 1 {
		t.Fatalf("记录数 = %d，期望 1（%+v）", len(recs), recs)
	}
	return recs[0]
}

// preferred 让一个候选排在前面。
//
// priority 大者先选，所以用例不必依赖「同分之间按随机键打散」的结果——
// 那个随机源虽然固定了种子，但候选个数一变顺序就会变。
func preferred(c route.Candidate) route.Candidate {
	c.Priority = 1
	return c
}

// upstreamErr 造一个带状态码的上游错误。
func upstreamErr(status int, retryable bool) error {
	return &channel.ErrUpstream{StatusCode: status, Retryable: retryable}
}

// ── 非流式 ──────────────────────────────────────────────────────────────

// 成功：记一次成功，用量跟着走（额度自记账要用它）。
func TestChatRecordsSuccessWithUsage(t *testing.T) {
	h := newHarness(t)

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	rec := h.recorder.only(t)
	if rec.ChannelID != 1 {
		t.Errorf("ChannelID = %d，期望 1", rec.ChannelID)
	}
	if rec.Verdict != route.VerdictSuccess {
		t.Errorf("Verdict = %q，期望成功", rec.Verdict)
	}
	if rec.Usage.PromptTokens != 1000 || rec.Usage.CompletionTokens != 100 {
		t.Errorf("Usage = %+v，期望上游报的 1000/100", rec.Usage)
	}
}

// 可重试的失败（429、5xx、网络层）：记成渠道的失败，这是「连续失败 N 次
// 就别再试」唯一的来源。
func TestChatRecordsRetryableFailure(t *testing.T) {
	h := newHarness(t)
	h.executor.err = upstreamErr(503, true)

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游失败时该返回错误")
	}

	rec := h.recorder.only(t)
	if rec.Verdict != route.VerdictFailure {
		t.Errorf("Verdict = %q，期望失败", rec.Verdict)
	}
	if rec.Usage.PromptTokens != 0 || rec.Usage.CompletionTokens != 0 {
		t.Errorf("失败的调用没有用量，实际 %+v", rec.Usage)
	}
}

// 客户端中途断开（点停止、LB 空闲超时、客户端自己超时）：**不是渠道的失败**，
// 记成「不归因」——连续失败计数既不加一也不清零。
//
// 它与上面那条可重试失败的差别只有一处，而这一处正是缺陷本身：适配器把 ctx
// 取消包成的就是 ErrUpstream{Retryable: true}，形态与 429、5xx 一样，只按错误
// 的形态判就会记成渠道故障。三次之后这个渠道按 streakCooldown 被摘掉十五分钟，
// 而它一次都没坏过。
//
// 判据是 ctx 而不是错误的形态：客户端断开时换一个渠道不会有任何不同
// （见 channelAtFault）。流式那条路对同一件事就是这么判的
// （见 TestChatStreamRecordsClientDisconnectAsUnattributed）。
func TestChatRecordsClientDisconnectAsUnattributed(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 取消由执行器在返回之前触发（见 cancelingExecutor）：预先取消的 ctx
	// 一次调用都不会发生，也就没有尝试可记——这个窗口正是「调用期间断开」。
	h.gw.Executor = &cancelingExecutor{
		cancel: cancel,
		inner:  &fakeExecutor{err: upstreamErr(500, true)},
	}

	if _, err := h.gw.Chat(ctx, request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("ctx 已取消时不该成功")
	}

	// 连续失败计数由结局推出来（见 internal/channels 的 streakChange）：
	// 只有 VerdictFailure 加一。所以这一条同时就是「计数没有增加」。
	rec := h.recorder.only(t)
	if rec.Verdict != route.VerdictUnattributed {
		t.Fatalf("Verdict = %q，期望不归因（是客户端断的，换一个渠道不会有任何不同）",
			rec.Verdict)
	}
}

// 客户端在上游那一次调用**成功之后**才断开：这一次照记成功，不因取消改判。
//
// 取消发生在记账之前（健康度排在结算之后记），所以记这一笔时 ctx 已经结束了。
// 改判它会连用量一起丢掉，而成功那一次的用量是额度自记账的依据。
//
// 与流式那条路同判：那边也是「流正常走完之后调用方断开不算断开」
// （clientGone 要求流没走完，见 pumpStream）。
func TestChatKeepsSuccessWhenClientDisconnectsAfterUpstream(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 结算落库的那一刻断开：上游那一次调用早就成功了。
	h.settlements.onSave = func() { cancel() }

	res, err := h.gw.Chat(ctx, request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("断开发生在上游返回之后，这次请求该照常成功: %v", err)
	}
	if res.Charge == nil {
		t.Fatal("这次请求该照常结算")
	}

	rec := h.recorder.only(t)
	if rec.Verdict != route.VerdictSuccess {
		t.Errorf("Verdict = %q，期望成功（上游那一次调用真的成功了）", rec.Verdict)
	}
	if rec.Usage.PromptTokens != 1000 {
		t.Errorf("成功那一次的用量要照记，实际 %+v", rec.Usage)
	}
}

// 上游拒绝了我们的身份（401/403）或余额（402）：换谁都是同一个错，
// 但那正是**这个渠道**坏了，所以记成失败。
func TestChatRecordsAuthAndCreditFailures(t *testing.T) {
	for _, status := range []int{401, 402, 403} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := newHarness(t)
			h.executor.err = upstreamErr(status, false)

			if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
				t.Fatal("上游失败时该返回错误")
			}
			if got := h.recorder.only(t).Verdict; got != route.VerdictFailure {
				t.Errorf("状态码 %d：Verdict = %q，期望失败", status, got)
			}
		})
	}
}

// 请求本身有错（400 参数错、404 模型名不存在）：**不记在渠道头上**。
//
// 三次参数错把一个健康渠道摘掉比不摘它更糟——摘掉之后没有流量，
// 连续失败计数也不会自己降回去（见 internal/channels 的 streakCooldown）。
func TestChatDoesNotBlameChannelForRequestErrors(t *testing.T) {
	for _, status := range []int{400, 404, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := newHarness(t)
			h.executor.err = upstreamErr(status, false)

			if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
				t.Fatal("上游失败时该返回错误")
			}
			if got := h.recorder.only(t).Verdict; got != route.VerdictUnattributed {
				t.Errorf("状态码 %d：Verdict = %q，期望不记在渠道头上", status, got)
			}
		})
	}
}

// 换渠道重试：每一次尝试各记一次，失败的那些也要记。
//
// 只记最后一次的话，排在前面那个一直失败的渠道永远不会攒够连续失败数。
func TestChatRecordsEveryAttempt(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{
		preferred(candidate(1, "gpt-4o")), candidate(2, "gpt-4o")}
	h.executor.failFor = map[int64]error{1: upstreamErr(503, true)}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	recs := h.recorder.all()
	if len(recs) != 2 {
		t.Fatalf("记录数 = %d，期望 2（两次尝试各一条）", len(recs))
	}
	if recs[0].ChannelID != 1 || recs[0].Verdict != route.VerdictFailure {
		t.Errorf("第一条 = %+v，期望渠道 1 失败", recs[0])
	}
	if recs[1].ChannelID != 2 || recs[1].Verdict != route.VerdictSuccess {
		t.Errorf("第二条 = %+v，期望渠道 2 成功", recs[1])
	}
	// 用量只属于成功的那一次。
	if recs[0].Usage.PromptTokens != 0 {
		t.Errorf("失败的尝试不该带用量：%+v", recs[0].Usage)
	}
	if recs[1].Usage.PromptTokens != 1000 {
		t.Errorf("成功的尝试该带用量：%+v", recs[1].Usage)
	}
}

// 一个候选都没有：没有尝试可记，也不该凭空造一条。
func TestChatNoCandidatesRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = nil

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("没有候选时该返回错误")
	}
	if got := h.recorder.count(); got != 0 {
		t.Errorf("记录数 = %d，期望 0", got)
	}
}

// 写健康度失败不影响请求本身：用户拿到的必须是上游那个结论，
// 不能被一次记录失败盖掉。
func TestRecordFailureDoesNotBreakRequest(t *testing.T) {
	h := newHarness(t)
	h.recorder.err = errors.New("库写不进去")

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("写健康度失败不该让请求失败: %v", err)
	}
	if res.Charge == nil {
		t.Error("请求该照常结算")
	}
}

// 写健康度失败要留一条错误日志，写成功时一个字都不写。
//
// 两个方向都要看：只断言失败那一侧时，把条件写反（成功的时候刷失败、失败的
// 时候闭嘴）不会被发现，而那条日志正好把排查的人引到反方向去。
//
// 两格断的都是那句话本身，而不是「有没有错误级日志」：这句话是写不进去时
// 唯一被人读到的东西，改了措辞就该有红用例。
func TestRecordLogsWriteFailureOnlyWhenItFails(t *testing.T) {
	const wants = "写渠道健康度失败"

	t.Run("写得进去：没有日志", func(t *testing.T) {
		h := newHarness(t)
		logs := adoptLogs(t)

		if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
			t.Fatalf("出错: %v", err)
		}
		if got := logs.text(); strings.Contains(got, wants) {
			t.Errorf("写健康度成功了却报失败，日志：%s", got)
		}
	})

	t.Run("写不进去：有一条日志，请求照常成功", func(t *testing.T) {
		h := newHarness(t)
		h.recorder.err = errors.New("库写不进去")
		logs := adoptLogs(t)

		if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
			t.Fatalf("写健康度失败不该让请求失败: %v", err)
		}
		if got := logs.text(); !strings.Contains(got, wants) {
			t.Errorf("写健康度失败时没有日志，实际：%q", got)
		}
	})
}

// 没接写入方时一切照常：它是可选接线，缺了不该 panic。
func TestRecordOptional(t *testing.T) {
	h := newHarness(t)
	h.gw.Recorder = nil

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
}

// 记的时刻取尝试**结束**的时刻，而不是记录的时刻。
//
// 它决定健康度桶的归属与 last_failure_at。用「记的时刻」的话，一次
// 跨过整点的调用会被算进后一个桶，而它其实发生在整点之前。
func TestChatRecordsAttemptEndTime(t *testing.T) {
	h := newHarness(t)
	started := fixedAt.Add(-time.Minute)
	h.gw.Router.Now = func() time.Time { return started }
	// 执行器立即返回，时长是 0，所以这次尝试的结束时刻就是 started。

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got := h.recorder.only(t).At; !got.Equal(started) {
		t.Errorf("At = %v，期望这次调用的时刻 %v", got, started)
	}
}

// 耗时随账单带进结算，供 usage_events.duration_ms 落库。
//
// # 这一条守的是接线的两头
//
// 延迟那一维的样本从 usage_events 按桶聚合，所以网关必须把一个正数交给
// 结算层。漏了这一步时账单金额照旧正确、分录照旧平衡，只有延迟永远没有
// 样本——与「这一列从来没有人写」这条缺陷长得一模一样。
//
// 取**最后一次尝试**的耗时：前面失败的那几次各是一次独立调用，加起来是
// 「这次请求因为重试耽误了多久」，不是「这个渠道多快」。
func TestChatCarriesLastAttemptDuration(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{
		preferred(candidate(1, "gpt-4o")), candidate(2, "gpt-4o")}
	h.executor.failFor = map[int64]error{1: upstreamErr(503, true)}

	// 每次调用推进 100 毫秒：渠道 1 失败、渠道 2 成功，两次尝试各 100ms。
	// 账单上要的是后一次（100ms），不是合计的 200ms。
	tick := fixedAt
	h.gw.Router.Now = func() time.Time {
		tick = tick.Add(100 * time.Millisecond)
		return tick
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	charge := h.settlements.saved[0]
	if charge.Duration != 100*time.Millisecond {
		t.Errorf("Charge.Duration = %s，期望 100ms（最后一次尝试，不是两次之和）", charge.Duration)
	}
}

// 健康度排在结算**之后**记：延迟与缓存两列由写入方按桶从 usage_events
// 重算，而那一行是结算事务写的。
//
// # 顺序为什么能被观察到
//
// 两个假件共用一个队列：谁先被调用谁的标记先进。顺序反了时桶里算出来的
// 分位数不含这一次请求——它要等下一次调用才被带进去，于是那个数永远落后
// 一个请求。这件事在金额上完全看不出来。
func TestChatRecordsHealthAfterSettlement(t *testing.T) {
	h := newHarness(t)

	var order []string
	h.settlements.onSave = func() { order = append(order, "settle") }
	h.recorder.onRecord = func() { order = append(order, "record") }

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	if want := []string{"settle", "record"}; !reflect.DeepEqual(order, want) {
		t.Errorf("调用顺序 = %v，期望 %v（健康度要等用量事件落库之后再记）", order, want)
	}
}

// 结算失败时健康度仍然要记：这些调用真的发生过。
//
// 这一次没有用量事件，缓存与延迟两列不会被重算；但成功率与连续失败的
// 分子分母不该因为一次结算失败而少掉一次。
func TestChatStillRecordsHealthWhenSettlementFails(t *testing.T) {
	h := newHarness(t)
	h.settlements.err = errors.New("写不进去")

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("结算失败时该返回错误")
	}
	if got := h.recorder.only(t).Verdict; got != route.VerdictSuccess {
		t.Errorf("Verdict = %q，期望成功（上游那一次调用成功了）", got)
	}
}

// ── 流式 ────────────────────────────────────────────────────────────────

// 正常结束：记一次成功，用量是上游报的那个。
func TestChatStreamRecordsSuccess(t *testing.T) {
	h := newHarness(t)
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "usage", Usage: &usage}, {Kind: "done"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	rec := h.recorder.only(t)
	if rec.Verdict != route.VerdictSuccess {
		t.Errorf("Verdict = %q，期望成功", rec.Verdict)
	}
	if rec.Usage.PromptTokens != 12 || rec.Usage.CompletionTokens != 7 {
		t.Errorf("Usage = %+v，期望上游报的 12/7", rec.Usage)
	}
}

// 流式的耗时覆盖**整条流**，不只到上游接受请求那一下。
//
// route 记的 Attempt.Duration 只量到 ChatStream 返回（响应头），而用户感知
// 的耗时是到最后一个事件为止；一次长回答里前者可以比后者小几个数量级。
// 这里的时钟每次读都往前走一分钟，所以「只量到响应头」会得到 0，
// 而「量到流结束」会得到一个正数。
func TestChatStreamDurationCoversWholeStream(t *testing.T) {
	h := newHarness(t)
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "usage", Usage: &usage}, {Kind: "done"}},
	}})

	now := fixedAt
	h.gw.Router.Now = func() time.Time {
		now = now.Add(time.Minute)
		return now
	}
	h.gw.Now = func() time.Time { return now }

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	if res.Charge == nil {
		t.Fatal("流读完时账单该已经有值")
	}
	if res.Charge.Duration <= 0 {
		t.Errorf("Charge.Duration = %s，期望覆盖整条流（> 0）", res.Charge.Duration)
	}
}

// 客户端断开：**不是渠道的失败**。记成「既没成功也没失败」——
// 照常算一次请求（分母要诚实），但不加连续失败、也不清零。
//
// 用量照记：上游已经为输入计了费，额度自记账要跟着扣。
func TestChatStreamRecordsClientDisconnectAsUnattributed(t *testing.T) {
	h := newHarness(t)
	hold := make(chan struct{})
	defer close(hold)
	useStreamExecutor(h, &scriptedExecutor{
		script: map[int64][]channel.StreamEvent{1: {delta("部分")}},
		hold:   map[int64]chan struct{}{1: hold},
	})

	ctx, cancel := context.WithCancel(context.Background())
	res, err := h.gw.ChatStream(ctx, request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 读到一个事件之后断开，模拟客户端中途走掉。
	<-res.Events
	cancel()
	collect(t, res)

	rec := h.recorder.only(t)
	if rec.Verdict != route.VerdictUnattributed {
		t.Errorf("Verdict = %q，期望不记在渠道头上（是客户端断的）", rec.Verdict)
	}
	if rec.Usage.PromptTokens == 0 {
		t.Error("客户端断开时上游已经计了费，用量该带上（额度自记账要扣）")
	}
}

// 上游接受请求之后、吐出任何内容之前就结束了：渠道此刻不行，记成失败。
//
// 这条同时钉住「成功的那一次不会被记两遍」——路由层那次尝试的 err 是 nil
// （上游接受了请求），只有读完流才知道它其实失败了。
func TestChatStreamRecordsAbortBeforeOutputAsFailure(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{1: nil}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	rec := h.recorder.only(t)
	if rec.Verdict != route.VerdictFailure {
		t.Errorf("Verdict = %q，期望失败", rec.Verdict)
	}
}

// 吐字之后上游报错（回答被截断）：渠道此刻不行，记成失败。
func TestChatStreamRecordsMidStreamErrorAsFailure(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "error", Err: errors.New("上游中途断了")}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	if got := h.recorder.only(t).Verdict; got != route.VerdictFailure {
		t.Errorf("Verdict = %q，期望失败", got)
	}
}

// 上游在接受请求之前就失败：路由层换过渠道，失败的那些要记，
// 而成功接受的那个渠道此时**不记**——它的结局还没定。
func TestChatStreamRecordsOnlyPreAcceptFailures(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{
		preferred(candidate(1, "gpt-4o")), candidate(2, "gpt-4o")}
	useStreamExecutor(h, &scriptedExecutor{
		preErr: map[int64]error{1: upstreamErr(503, true)},
		script: map[int64][]channel.StreamEvent{2: {{Kind: "done"}}},
	})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	recs := h.recorder.all()
	if len(recs) != 2 {
		t.Fatalf("记录数 = %d，期望 2（渠道 1 的失败 + 渠道 2 的结局）", len(recs))
	}
	if recs[0].ChannelID != 1 || recs[0].Verdict != route.VerdictFailure {
		t.Errorf("第一条 = %+v，期望渠道 1 失败", recs[0])
	}
	if recs[1].ChannelID != 2 || recs[1].Verdict != route.VerdictSuccess {
		t.Errorf("第二条 = %+v，期望渠道 2 成功", recs[1])
	}
}

// 客户端在「换过渠道、还没开始读流」这段窗口里断开：那几次失败的尝试
// **不是渠道的失败**，记成「不归因」——连续失败计数既不加一也不清零。
//
// 它与上面那条的差别只有一处，而这一处正是缺陷本身：上游那一次失败被适配器
// 包成 ErrUpstream{Retryable: true}，而 ctx 取消就是这么包出来的，形态与 503
// 一样，只按错误的形态判就会记成渠道故障。
//
// 最终结局那一支由 pumpStream 判（见 TestChatStreamRecordsClientDisconnectAsUnattributed）：
// 那个管读完流之后，这个管还没开始读流就已经失败掉的那几次尝试。
func TestChatStreamRecordsPreAcceptFailureOnDisconnectAsUnattributed(t *testing.T) {
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 取消由执行器在返回之前触发（见 cancelingExecutor）：预先取消的 ctx
	// 一次调用都不会发生，也就没有尝试可记——这个窗口正是「调用期间断开」。
	h.gw.Executor = &cancelingExecutor{
		cancel: cancel,
		inner:  &fakeExecutor{failFor: map[int64]error{1: upstreamErr(503, true)}},
	}

	if _, err := h.gw.ChatStream(ctx, request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("ctx 已取消时不该成功")
	}

	// 连续失败计数由结局推出来（见 internal/channels 的 streakChange）：
	// 只有 VerdictFailure 加一。所以这一条同时就是「计数没有增加」。
	rec := h.recorder.only(t)
	if rec.ChannelID != 1 {
		t.Fatalf("ChannelID = %d，期望 1", rec.ChannelID)
	}
	if rec.Verdict != route.VerdictUnattributed {
		t.Fatalf("Verdict = %q，期望不归因（是客户端断的，换一个渠道不会有任何不同）",
			rec.Verdict)
	}
}

// 全部候选都在接受请求之前失败：只记失败的那些，一条不多。
func TestChatStreamAllPreAcceptFailuresRecorded(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}
	useStreamExecutor(h, &scriptedExecutor{preErr: map[int64]error{
		1: upstreamErr(503, true),
		2: upstreamErr(502, true),
	}})

	if _, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("全部失败时该返回错误")
	}

	recs := h.recorder.all()
	if len(recs) != 2 {
		t.Fatalf("记录数 = %d，期望 2", len(recs))
	}
	for _, rec := range recs {
		if rec.Verdict != route.VerdictFailure {
			t.Errorf("渠道 %d 的结论 = %q，期望失败", rec.ChannelID, rec.Verdict)
		}
	}
}
