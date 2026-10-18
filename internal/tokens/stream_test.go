package tokens

import (
	"strings"
	"testing"
)

// 这些用例覆盖的正是本包存在的理由：客户端在流式响应中途断开，
// 上游的 usage 永远不会到达，只能本地算一个数并如实标注。

func TestStreamEstimatorMidStreamDisconnect(t *testing.T) {
	// 核心场景：收了几个增量就断了，流里没有 usage。
	c := newPrepared(t)
	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest("Please summarize the document.", 1, 0)
	e.ObserveDelta("The document ")
	e.ObserveDelta("describes a ")
	e.ObserveDelta("model routing gateway.")

	u := e.Result()
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
	if u := e.Result(); u.Total().Tokens != 0 {
		t.Errorf("nil 累加器应返回零用量, 得到 %+v", u)
	}
}

func TestUsageTotal(t *testing.T) {
	u := Usage{
		Input:  Count{Tokens: 1000, Source: SourceEstimated},
		Output: Count{Tokens: 500, Source: SourceEstimated},
	}
	if got := u.Total().Tokens; got != 1500 {
		t.Errorf("合计 = %d, 期望 1500", got)
	}
	if got := u.Total().Source; got != SourceEstimated {
		t.Errorf("合计来源 = %v, 期望 estimated", got)
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
// 变异：`messages > 0`（155 行）与 `tools > 0`（158 行）两处，`>` 改成 `>=`
// 都是**等价变异**：两版只在计数正好为 0 那一格分岔，而那一格加的是
// `0 × 框架费`，本来就是 0，加与不加同值。这两处杀不掉，理由在值上，
// 不是「想不到输入」。
//
// 下面那几行不是为这两处写的——它固定的是**方向**：负数必须当成没给
// （零增量），而不是反向减一笔。那一格是 `>` 改成 `<=` 那一版（NEGATION，
// 不在存活清单里）与原版唯一分岔的地方；**今天的调用方到不了那里**
// （internal/gateway 传的是 `len(...)`，见 stream.go 顶上那行用法）。
// 写下来是为了把这个函数的口径边界写清楚，不是因为真有传负数的调用方。
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

	// 传负数是 `>` 改成 `<=` 那一版唯一分岔的格子；今天的调用方到不了这里
	// （internal/gateway 传的是 `len(...)`），这一句把口径写下来：
	// 负数当成没给，而不是反向的一笔。
	negative := NewStreamEstimator(nil, model)
	negative.ObserveRequest("hello", -3, -2)
	if negative.input.Tokens != withoutFrames {
		t.Errorf("负计数时输入 = %d, 期望 %d（当作没给）",
			negative.input.Tokens, withoutFrames)
	}
}

// ObserveToolCall 的 id/name 与 args 是**两段**独立的计量：前者算框架与标识，
// 后者算参数正文。断言要**逐项对上**——「非零」这种写法会被另一段计量满足：
// 只给 args 却漏掉那 6 个 token 的框架费时，它照样非零。
//
// 三个变异位置分开判。工具写回的是重打印的整棵 AST（`&&` 比 `||` 结合得紧，
// 打印机按优先级补括号），所以第二个 `||` 那一版实际是 `(A || B) && C`，
// 与按字符替换得到的 `A || B && C`（=`A || (B && C)`）不是同一个表达式——
// 下面写的是**工具那一版**。
//
//   - 第一个 `||` 改成 `&&`：整句变成 `(A && B) || C`（A=args，B=id，C=name）。
//     只给 args 时 B 为假、只给 id 时 A 为假，两格都不再进这一支，漏掉框架费；
//     只给 name 时 C 为真，照旧计费。
//   - 第二个 `||` 改成 `&&`：整句变成 `(A || B) && C`。三格里凡 name 为空的
//     都进不去，只给 args、只给 id、只给 name 三种都漏掉框架费。
//     两个变异体在「只给 name」那一格分开：第一个 `||` 那一版照旧计费，
//     第二个 `||` 那一版不计。
//   - 第二个 `if` 的 `||` 改成 `&&`：只给其中一个时，id 与 name 拼起来
//     那一份正文整段不计（框架费照旧）。
func TestObserveToolCallCountsPartialInput(t *testing.T) {
	const model = "claude-placeholder"
	// 框架费是常量，正文那份走估算（这里传 nil 计数器）——两段都是具体的数。
	const frame = int64(ToolCallFrameTokens)

	empty := NewStreamEstimator(nil, model)
	empty.ObserveToolCall("", "", "")
	if empty.toolArgs != 0 {
		t.Errorf("三个都空时不该计费，得到 %d", empty.toolArgs)
	}

	// 只给 args：框架费 + 参数正文。
	onlyArgs := NewStreamEstimator(nil, model)
	onlyArgs.ObserveToolCall("", "", `{"a":1}`)
	if want := frame + EstimatedTokens(model, `{"a":1}`); onlyArgs.toolArgs != want {
		t.Errorf("只给 args 时 toolArgs = %d，期望 %d（框架 %d + 正文 %d）",
			onlyArgs.toolArgs, want, frame, EstimatedTokens(model, `{"a":1}`))
	}

	// 只给 id：框架费 + 正文（id 与 name 之间那个空格照算）。
	onlyID := NewStreamEstimator(nil, model)
	onlyID.ObserveToolCall("call_1", "", "")
	if want := frame + EstimatedTokens(model, "call_1 "); onlyID.toolArgs != want {
		t.Errorf("只给 id 时 toolArgs = %d，期望 %d（框架 %d + 正文 %d）",
			onlyID.toolArgs, want, frame, EstimatedTokens(model, "call_1 "))
	}

	// 只给 name：同上。
	onlyName := NewStreamEstimator(nil, model)
	onlyName.ObserveToolCall("", "get_weather", "")
	if want := frame + EstimatedTokens(model, " get_weather"); onlyName.toolArgs != want {
		t.Errorf("只给 name 时 toolArgs = %d，期望 %d（框架 %d + 正文 %d）",
			onlyName.toolArgs, want, frame, EstimatedTokens(model, " get_weather"))
	}
}

// 有词表、且模型真的认得时，输入必须走**精确**那条路，而不是悄悄退回估算。
//
// 变异：`if e.counter != nil` 改成 `== nil`——计数器不为空时反而走 else 分支，
// 输入从量出来的 5 变成估出来的 11，来源也从 tokenized 变成 estimated。
// 「精确」与「估算」在账上的处置不同（能不能拿去对账），所以来源那一栏
// 是这条路径唯一能自证的地方。
//
// 原来杀不掉的原因：别的用例里的计数器都没法真算（未预热，或模型没有对应
// 词表），两条路在那些夹具上给出同一个数与同一个来源。
func TestObserveRequestUsesPreparedCounter(t *testing.T) {
	c := newPrepared(t)
	const text = "Please summarize the document."

	e := NewStreamEstimator(c, "gpt-4o")
	e.ObserveRequest(text, 0, 0)

	u := e.Result()
	if u.Input.Source != SourceTokenized {
		t.Errorf("输入来源 = %v, 期望 tokenized（计数器已预热）", u.Input.Source)
	}
	if u.Input.Tokens != 5 {
		t.Errorf("输入 = %d, 期望 5（这套词表数出来的数）", u.Input.Tokens)
	}
	// 估算那条路给出的是另一个数：有这一条，上面那个 5 才不是在测巧合。
	if est := EstimatedTokens("gpt-4o", text); est == u.Input.Tokens {
		t.Errorf("估算与精确数相同（%d），这条用例已经分不开两条路", est)
	}
}
