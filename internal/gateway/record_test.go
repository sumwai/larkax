package gateway

import (
	"context"
	"errors"
	"strconv"
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
}

func (f *fakeRecorder) RecordCall(_ context.Context, rec route.CallOutcome) error {
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

// ── 流式 ────────────────────────────────────────────────────────────────

// 正常结束：记一次成功，用量是上游报的那个。
func TestChatStreamRecordsSuccess(t *testing.T) {
	h := newHarness(t)
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "usage", Usage: &usage}, {Kind: "done"}},
	}})

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest())
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
	res, err := h.gw.ChatStream(ctx, request(), upstreamRequest())
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

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest())
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

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest())
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

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest())
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

// 全部候选都在接受请求之前失败：只记失败的那些，一条不多。
func TestChatStreamAllPreAcceptFailuresRecorded(t *testing.T) {
	h := newHarness(t)
	h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}
	useStreamExecutor(h, &scriptedExecutor{preErr: map[int64]error{
		1: upstreamErr(503, true),
		2: upstreamErr(502, true),
	}})

	if _, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest()); err == nil {
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
