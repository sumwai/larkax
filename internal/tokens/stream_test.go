package tokens

import (
	"strings"
	"testing"
)

// 这些用例覆盖的正是本包存在的理由：客户端在流式响应中途断开，
// 上游的 usage 永远不会到达，只能本地算一个数并如实标注。

func TestStreamEstimatorNormalCompletionUsesMeasured(t *testing.T) {
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest("What is the capital of France?", 1, 0)
	e.ObserveDelta("The capital of France is Paris.")
	e.ObserveMeasured(Count{Tokens: 42})
	e.ObserveDone()

	u := e.Result()
	if !u.Complete {
		t.Error("正常结束且带 usage 时应 Complete")
	}
	if u.Input.Source != SourceMeasured {
		t.Errorf("输入来源 = %v, 期望 measured", u.Input.Source)
	}
	if u.Input.Tokens != 42 {
		t.Errorf("输入 = %d, 期望 42（实测值覆盖估算）", u.Input.Tokens)
	}
}

func TestStreamEstimatorMeasuredDoesNotDoubleCountInput(t *testing.T) {
	// 覆盖而不是相加。写错的表现是「流里带 usage 的请求，输入翻倍」，
	// 而它不会触发任何错误，只会让账单变高。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest(strings.Repeat("long prompt content ", 50), 1, 0)
	localInput := e.input.Tokens
	if localInput < 100 {
		t.Fatalf("构造失败：本地输入只有 %d token", localInput)
	}
	e.ObserveMeasured(Count{Tokens: 200})
	u := e.Result()
	if u.Input.Tokens != 200 {
		t.Errorf("输入 = %d, 期望 200（不是 200 + 本地估算 %d）", u.Input.Tokens, localInput)
	}
}

func TestStreamEstimatorMidStreamDisconnect(t *testing.T) {
	// 核心场景：收了几个增量就断了，流里没有 usage。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest("Please summarize the document.", 1, 0)
	e.ObserveDelta("The document ")
	e.ObserveDelta("describes a ")
	e.ObserveDelta("model routing gateway.")
	// 没有 ObserveMeasured，也没有 ObserveDone——客户端断了。

	u := e.Result()
	if u.Complete {
		t.Error("中途断开时不应 Complete——这笔账要按估算处理")
	}
	if u.Output.Tokens < 1 {
		t.Error("已收到的内容必须计入输出")
	}
	if u.Output.Source != SourceTokenized {
		t.Errorf("输出来源 = %v, 期望 tokenized（gpt-4o 有词表）", u.Output.Source)
	}
	if u.Note == "" {
		t.Error("应给出「为什么这个数是本地算的」的说明")
	}
}

func TestStreamEstimatorNoUsageOnNormalEnd(t *testing.T) {
	// 正常结束但上游没带 usage——同样要退到本地算。
	// 「正常结束」与「用量是实测的」是两件事。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest("hi", 1, 0)
	e.ObserveDelta("hello there")
	e.ObserveDone()

	u := e.Result()
	if u.Complete {
		t.Error("没有 usage 时 Complete 必须为 false，否则调用方会以为可以对账")
	}
	if u.Output.Tokens < 1 {
		t.Error("输出必须被计入")
	}
}

func TestStreamEstimatorCountsIncompleteToolArgs(t *testing.T) {
	// 工具调用的参数是分片到达的，中途断开时必然是半截 JSON。
	// 丢弃它们等于那次调用完全不计费，而它恰恰发生在长输出场景里。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest("Read the file.", 1, 1)
	e.ObserveToolCall("call_abc", "read_file", "")
	e.ObserveToolCall("", "", `{"path":"<PATH_PLACEHOLDER>`)
	// 参数在这里被截断了——客户端断开。

	u := e.Result()
	if u.Output.Tokens < ToolCallFrameTokens {
		t.Errorf("输出 = %d, 至少要包含一次工具调用的固定开销 %d",
			u.Output.Tokens, ToolCallFrameTokens)
	}
}

func TestStreamEstimatorToolArgsAddTokens(t *testing.T) {
	c := newPrepared(t)
	without := NewStreamEstimator(c, "gpt-4o")
	without.ObserveToolCall("call_abc", "f", "")

	with := NewStreamEstimator(c, "gpt-4o")
	with.ObserveToolCall("call_abc", "f", strings.Repeat(`"key":"value",`, 20))

	if with.Result().Output.Tokens <= without.Result().Output.Tokens {
		t.Error("参数多的那次必须算出更多的 token")
	}
}

func TestStreamEstimatorEmptyStream(t *testing.T) {
	// 什么都没收到：上游可能已经读了 prompt 所以输入要计费，
	// 输出记 0。不能因为「断开得早」就把整次调用算成零。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest("some prompt", 1, 0)

	u := e.Result()
	if u.Input.Tokens < 1 {
		t.Error("输入仍要计费——上游可能已经读了 prompt")
	}
	if u.Output.Tokens != 0 {
		t.Errorf("输出 = %d, 期望 0", u.Output.Tokens)
	}
}

func TestStreamEstimatorWarnsWhenRequestNotObserved(t *testing.T) {
	// 没提供输入时不能让人误读成「输入为零」。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveDelta("hi")
	u := e.Result()
	if !strings.Contains(u.Note, "输入内容") {
		t.Errorf("Note = %q, 应说明调用方未提供输入内容", u.Note)
	}
	if u.Input.Tokens != 0 {
		t.Errorf("输入 = %d, 期望 0（调用方没给）", u.Input.Tokens)
	}
}

func TestStreamEstimatorFrameTokens(t *testing.T) {
	// 每条消息与每个工具定义都要补框架开销：那部分 token 在词表之外，
	// 本地分词算不出来。
	c := newPrepared(t)
	bare := NewStreamEstimator(c, "gpt-4o")
	bare.ObserveRequest("hello", 0, 0)

	framed := NewStreamEstimator(c, "gpt-4o")
	framed.ObserveRequest("hello", 3, 2)

	want := bare.input.Tokens + 3*MessageFrameTokens + 2*ToolDefinitionFrameTokens
	if framed.input.Tokens != want {
		t.Errorf("带框架的输入 = %d, 期望 %d", framed.input.Tokens, want)
	}
}

func TestStreamEstimatorMergesToLessTrustedSource(t *testing.T) {
	// 只要有一段是估的，整体就是估的。反过来取会让估算值混进一个
	// 标着「精确」的结果里，而那种错误只会在对账时以「说好的实测
	// 怎么对不上」的形式暴露。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveDelta("with vocab")    // tokenized
	e.ObserveDelta("another piece") // tokenized
	if got := e.Result().Output.Source; got != SourceTokenized {
		t.Errorf("来源 = %v, 期望 tokenized", got)
	}

	// 换成没有词表的模型：来源应变成 estimated。
	c2 := newPrepared(t)
	e2 := NewStreamEstimator(c2, "claude-sonnet-4-6")
	e2.ObserveDelta("no vocab here")
	if got := e2.Result().Output.Source; got != SourceEstimated {
		t.Errorf("无词表模型的来源 = %v, 期望 estimated", got)
	}
}

func TestStreamEstimatorNilMethodsDoNotPanic(t *testing.T) {
	// 一个漏初始化的累加器不该让流式路径 panic——那条路径上 panic
	// 意味着一个已经计了费的请求连账都记不下来。
	var e *StreamEstimator
	e.ObserveRequest("x", 1, 1)
	e.ObserveDelta("x")
	e.ObserveToolCall("id", "name", "args")
	e.ObserveMeasured(Count{Tokens: 1})
	e.ObserveDone()
	if u := e.Result(); u.Total().Tokens != 0 {
		t.Errorf("nil 累加器应返回零用量, 得到 %+v", u)
	}
}

func TestStreamEstimatorHasMeasured(t *testing.T) {
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	if e.HasMeasured() {
		t.Error("初始状态不该有实测值")
	}
	e.ObserveMeasured(Count{Tokens: 1})
	if !e.HasMeasured() {
		t.Error("给了实测值之后 HasMeasured 应为 true")
	}

	var nilE *StreamEstimator
	if nilE.HasMeasured() {
		t.Error("nil 累加器不该报告已有实测值")
	}
}

func TestUsageTotalAndReview(t *testing.T) {
	u := Usage{
		Input:    Count{Tokens: 1000, Source: SourceEstimated},
		Output:   Count{Tokens: 500, Source: SourceEstimated},
		Complete: false,
	}
	if got := u.Total().Tokens; got != 1500 {
		t.Errorf("合计 = %d, 期望 1500", got)
	}
	if got := u.Total().Source; got != SourceEstimated {
		t.Errorf("合计来源 = %v, 期望 estimated", got)
	}
	if !u.RequiresReview() {
		t.Error("量大的估算值应要求人工复核")
	}

	// 实测值不需要复核——它可以拿去对账。
	measured := Usage{Input: Count{Tokens: 5000, Source: SourceMeasured}, Complete: true}
	if measured.RequiresReview() {
		t.Error("实测值不需要人工复核")
	}

	// 小额估算也不值得复核：一个每次都报的告警第三次就没人看了。
	small := Usage{Input: Count{Tokens: 5, Source: SourceEstimated}}
	if small.RequiresReview() {
		t.Error("小额估算不该触发复核")
	}

	// Complete 但输出来源是估算（正常结束却没带 usage）：仍要复核。
	mixed := Usage{
		Input:    Count{Tokens: 2000, Source: SourceMeasured},
		Output:   Count{Tokens: 2000, Source: SourceEstimated},
		Complete: false,
	}
	if !mixed.RequiresReview() {
		t.Error("输出来源是估算且量大时应要求复核")
	}
}

func TestUsageTotalKeepsMostPreciseModelName(t *testing.T) {
	u := Usage{
		Input:  Count{Tokens: 1, Source: SourceMeasured, Model: "gpt-4o"},
		Output: Count{Tokens: 1, Source: SourceEstimated},
	}
	if got := u.Total().Model; got != "gpt-4o" {
		t.Errorf("合计的模型名 = %q, 期望 gpt-4o", got)
	}
}

func TestStreamEstimatorConcurrentIsNotRequired(t *testing.T) {
	// 这里只确认单 goroutine 下多次增量累加是稳定的。
	// 累加器**不承诺**并发安全，文档里已写明由调用方加锁。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	for i := 0; i < 100; i++ {
		e.ObserveDelta("chunk ")
	}
	if got := e.Result().Output.Tokens; got < 100 {
		t.Errorf("100 次增量后输出只有 %d token, 累加似乎丢了内容", got)
	}
}

func TestObserveRequestUsesContextBackground(t *testing.T) {
	// 确认传入的是可用的 context 而不是 nil（nil context 在 CountText
	// 里会 panic，而那是一条已经计费的路径上的 panic）。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest("x", 0, 0)
	if e.input.Tokens < 1 {
		t.Error("ObserveRequest 应正常完成")
	}
}

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 来自变异测试（gremlins unleash）：把条件改一下，测试仍然通过。
// 这些都落在「多个参数里只填了一个」的边界上——而流式估算恰好
// 是拿不到实测值时唯一的计费依据。

// ObserveRequest 的三个参数**各自**都要算进去。
//
// 变异：`messages > 0` 或 `tools > 0` 改成别的，只影响「恰好为 0」
// 与「只填一个」的情形，而原有用例里两个都填了。
func TestObserveRequestCountsEachParameterSeparately(t *testing.T) {
	model := "claude-placeholder"

	base := NewStreamEstimator(nil, model)
	base.ObserveRequest("hello", 0, 0)
	withoutFrames := base.input.Tokens

	withMessages := NewStreamEstimator(nil, model)
	withMessages.ObserveRequest("hello", 3, 0)
	if withMessages.input.Tokens <= withoutFrames {
		t.Error("有消息条数时应当多算框架 token")
	}

	withTools := NewStreamEstimator(nil, model)
	withTools.ObserveRequest("hello", 0, 2)
	if withTools.input.Tokens <= withoutFrames {
		t.Error("有工具定义时应当多算框架 token")
	}

	// 两者都填时应当比只填一个更多
	both := NewStreamEstimator(nil, model)
	both.ObserveRequest("hello", 3, 2)
	if both.input.Tokens <= withMessages.input.Tokens {
		t.Error("消息与工具都填时应当更多")
	}
}

// ObserveToolCall 的 id/name 与 args 是**两段**独立的计量：
// 前者算框架与标识，后者算参数正文。
//
// 变异：`args != "" || id != "" || name != ""` 改成 `&&`，
// 只给 args 的调用就不再计框架费——而那是流式工具调用里最常见的形态。
func TestObserveToolCallCountsPartialInput(t *testing.T) {
	model := "claude-placeholder"

	empty := NewStreamEstimator(nil, model)
	empty.ObserveToolCall("", "", "")
	if empty.toolArgs != 0 {
		t.Errorf("三个都空时不该计费，得到 %d", empty.toolArgs)
	}

	onlyArgs := NewStreamEstimator(nil, model)
	onlyArgs.ObserveToolCall("", "", `{"a":1}`)
	if onlyArgs.toolArgs == 0 {
		t.Error("只给 args 时应当计费——那是流式工具调用最常见的形态")
	}

	onlyID := NewStreamEstimator(nil, model)
	onlyID.ObserveToolCall("call_1", "", "")
	if onlyID.toolArgs == 0 {
		t.Error("只给 id 时应当计框架费")
	}

	onlyName := NewStreamEstimator(nil, model)
	onlyName.ObserveToolCall("", "get_weather", "")
	if onlyName.toolArgs == 0 {
		t.Error("只给 name 时应当计框架费")
	}
}
