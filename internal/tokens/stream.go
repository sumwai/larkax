package tokens

// 本文件是流式中断时的估算兜底——包存在的核心理由。
//
// # 场景
//
// 客户端在流式响应中途断开（关掉标签页、网络掉线、超时取消）。费用已经
// 产生：上游按它吐出的 token 计费，与我们这边是否断开无关。但**上游的
// usage 只在最后一个分片里返回**——断开就永远拿不到那一片。
//
// 于是只能本地算。算出来的数字与实测值在账上必须能区分：
// 前者可能偏、不能拿去找上游对账，后者才是权威。这个区分由 Source 承载。
//
// # 增量计数，不留全文
//
// 每收到一个增量就立刻算它的 token 并累加，不把正文留在内存里。
// 理由是流式响应可能很长（128K token 的输出），而本包只需要一个数字；
// 为了「等全部收完再精确算一遍」而把整段输出驻留在内存里，是把一个
// 计费问题变成了一个内存问题。
//
// 代价是**分片边界上的误差**：BPE 的切分不看分片边界，而逐片计数会
// 在每片的两端各切一次。影响方向不确定，量级很小（每片最多差一两个
// token），而分片数在长回答里可能上千，累计起来可达千分之几。
// 这个误差被 ErrorBounds 的最坏值覆盖，不需要单独处理。
//
// # 保守方向
//
// 估算偏高（理由见包注释）。此外还有三条额外的保守约定：
//
//  1. 每条已开始的消息按其框架开销计费，不因为它「看起来是空的」而跳过。
//  2. 分片到达但不完整的工具调用参数，按**已收到的片段**算，而不是丢弃。
//     丢弃会让一次中途断开的工具调用完全不计费——而工具调用恰恰是长输出
//     场景，也就是金额最大的那一类。
//  3. 流里若已经带过 usage，一律用实测值（实测永远优先）。

import (
	"context"
)

// ── 消息框架开销 ───────────────────────────────────────────────────

// MessageFrameTokens 是每条消息在词表之外还要消耗的 token 数。
//
// 这个数字**无法由本地词表算出**：它是上游在消息外面加的标记（role 名、
// 分隔符、边界特殊 token）。实测（o200k_base）得到：
//
//	"<|im_start|>user\nhello<|im_end|>\n"   每条 15 token
//	"Human: hello\n\nAssistant: ok\n\n"     每条 8 token
//	"user\nhello\n"（只有 role 与换行）      每条 4 token
//
// 那些特殊 token（<|im_start|> 之类）在公开词表里**不作为特殊 token 存在**，
// 本地分词会把它们拆成 6~7 个普通 token，而上游算的是 1 个特殊 token——
// 两者不是一回事，所以复现不了，只能给常数。
//
// 取 4 而不是 15：15 是带上游私有特殊 token 的观测值，而那套特殊 token
// 各家不同（有的上游不加）。4 是「role 名 + 两个换行」这个所有协议都有的
// 部分，是**下界**。偏低在这里可接受：框架开销相对正文很小，而估高它会
// 按消息条数放大——一次 50 轮的工具调用循环，每条多算 10 token 就是
// 500 token 的凭空费用。
const MessageFrameTokens = 4

// ToolDefinitionFrameTokens 是每个工具定义的 JSON 包装开销。
//
// 实测：一个最小工具定义的完整 JSON 是 50~62 token，其中包装
// （`{"type":"function","function":{"name":…,"description":…,"parameters":{…}}}`）
// 约 26 token。工具定义的**正文**（name、description、schema）走正常文本
// 计数，这里只补包装。取 8 而不是 26，理由同 MessageFrameTokens：
// 宁可留一个下界，也不按条数放大。
const ToolDefinitionFrameTokens = 8

// ToolCallFrameTokens 是一次工具调用的固定开销（id、type、function 包装），
// 不含 name 与 arguments——那两者是内容，走正常计数。
//
// 实测：`{"index":0,"id":"call_…","type":"function","function":{"name":"…",
// "arguments":"{}"}}` 共 25~33 token，其中固定部分约 6 token，
// 其余由 id（约 20 字符）与 name 组成。id 由上游生成，我们只能在收到时
// 按内容计——但 ToolCallDelta 的 ID 只在第一个分片里给一次，
// 所以这里按「每次工具调用 6 + id 长度」处理，见 ObserveToolCall。
const ToolCallFrameTokens = 6

// ── 累加器 ─────────────────────────────────────────────────────────

// StreamEstimator 累计一条流式响应已产生的用量。
//
// 用法：
//
//	est := tokens.NewStreamEstimator(counter, upstreamModel)
//	est.ObserveRequest(text, len(messages), len(tools))
//	for ev := range events {
//	    switch ev.Kind {
//	    case "delta":
//	        est.ObserveDelta(ev.Text)
//	    case "tool_use":
//	        est.ObserveToolCall(ev.ToolCall.ID, ev.ToolCall.Name, ev.ToolCall.Arguments)
//	    }
//	}
//	est.ObserveDone()
//	u := est.Result()            // 无论正常结束还是中途断开
//
// 上游在流里带了实测用量时，用 ObserveMeasured 覆盖估算（覆盖而不是相加）。
//
// **不是并发安全的**：一条流只有一个消费者。要跨 goroutine 用，
// 由调用方自己加锁——本类型不内置锁，因为流式路径上每个事件都加锁
// 是白花掉的开销，而唯一的调用者本来就在同一个 goroutine 里。
type StreamEstimator struct {
	counter *Counter
	model   string

	// input 是输入侧的计数结论，由 ObserveRequest 一次性给出。
	input Count
	// requestObserved 标记调用方是否提供过输入。没提供过时 Result 会
	// 在 Note 里说明，避免把「没算输入」误读成「输入为零」。
	requestObserved bool

	// outputText 是已累计的正文 token 数（已换算成 token，不是字符数）。
	outputText int64
	// outputSource 是累计过程中用到的**最不可信**的来源。
	outputSource Source
	// toolArgs 是已累计的工具参数 token 数。
	toolArgs int64
	// toolCalls 是已开始的工具调用个数。
	toolCalls int

	// measured 是流里带过的实测用量。非 nil 时它是权威值。
	measured *Count
	// measuredComplete 表示这次流已经正常结束（收到 done 事件）。
	measuredComplete bool
}

// NewStreamEstimator 构造累加器。model 决定用哪套词表与标定表。
//
// 传**上游实际用的**模型名而不是客户端请求的那个：上游可能做别名替换，
// 而词表要看实际那个模型。
func NewStreamEstimator(c *Counter, model string) *StreamEstimator {
	return &StreamEstimator{counter: c, model: model, outputSource: SourceMeasured}
}

// ObserveRequest 记下这次请求的输入侧大小，在每个请求上调用一次。
//
// text 应当是**已经拼好的请求正文**：把每条消息的 role 与内容都写进去，
// 再把工具定义的正文也写进去。只传用户那一条消息是这一层最常见的错误，
// 而它的表现是多轮对话的输入被算成只有最后一轮——一个 agent 循环里
// 输入是输出的几十倍，所以这个错误会让账单偏得很远。
//
// messages 与 tools 只用于补框架开销；传 0 表示「不知道，不补」。
func (e *StreamEstimator) ObserveRequest(text string, messages, tools int) {
	if e == nil {
		return
	}
	e.requestObserved = true

	var c Count
	if e.counter != nil {
		// context.Background：分词与估算都是纯 CPU 计算，不可取消。
		// 这里没有值得传下去的取消点——调用方要取消整次请求，
		// 该取消的是上游请求本身。
		c, _ = e.counter.CountText(context.Background(), e.model, text)
	} else {
		c = Count{Tokens: EstimatedTokens(e.model, text), Source: SourceEstimated, Model: e.model}
	}
	if messages > 0 {
		c.Tokens += int64(messages) * MessageFrameTokens
	}
	if tools > 0 {
		c.Tokens += int64(tools) * ToolDefinitionFrameTokens
	}
	e.input = c
}

// ObserveDelta 记下一段正文增量。
//
// 每段增量单独计数再累加，见文件头「增量计数，不留全文」。
func (e *StreamEstimator) ObserveDelta(text string) {
	if e == nil || text == "" {
		return
	}
	c := e.countFragment(text)
	e.outputText += c.Tokens
	e.mergeOutputSource(c.Source)
}

// ObserveToolCall 记下一次工具调用的增量。
//
// args 是本次收到的**参数分片**（可能不完整，这是常态）；id 与 name
// 只在第一个分片里给出，后续分片为空。
//
// 参数分片照算而不是等拼齐：中途断开的工具调用，上游仍然为那些参数
// token 计了费。丢弃等于那次调用完全不计费，而它恰恰最容易发生在长输出
// 场景（也就是金额最大的场景）里。
func (e *StreamEstimator) ObserveToolCall(id, name, args string) {
	if e == nil {
		return
	}
	if args != "" || id != "" || name != "" {
		e.toolCalls++
		e.toolArgs += int64(ToolCallFrameTokens)
	}
	if id != "" || name != "" {
		c := e.countFragment(id + " " + name)
		e.toolArgs += c.Tokens
		e.mergeOutputSource(c.Source)
	}
	if args != "" {
		c := e.countFragment(args)
		e.toolArgs += c.Tokens
		e.mergeOutputSource(c.Source)
	}
}

// ObserveMeasured 用上游给的实测用量覆盖估算。
//
// **覆盖而不是相加**：实测值本身就是整次请求的用量，与累加值相加会把
// 输入算两遍。这是本文件里最容易写错的一处——写错的表现是「流里带 usage
// 的请求，输入翻倍」，而它不会触发任何错误，只会让账单变高。
func (e *StreamEstimator) ObserveMeasured(m Count) {
	if e == nil {
		return
	}
	cp := m
	cp.Source = SourceMeasured
	if cp.Model == "" {
		cp.Model = e.model
	}
	e.measured = &cp
}

// ObserveDone 标记这次流正常结束。
//
// 「正常结束」与「用量是实测的」是两件事：上游可能在正常结束时分片里
// 仍然不带 usage。Complete 只在两者同时成立时才为 true。
func (e *StreamEstimator) ObserveDone() {
	if e == nil {
		return
	}
	e.measuredComplete = true
}

// HasMeasured 表示流里是否已经带过实测用量。
func (e *StreamEstimator) HasMeasured() bool { return e != nil && e.measured != nil }

// Result 返回最终的用量结论。三种结局，按可信度从高到低：
//
//  1. 流里带过 usage → 用实测值（Source = measured），Complete = true。
//  2. 没带 usage，但收到了内容 → 全部本地算（tokenized 或 estimated），
//     Complete = false。这是最常见的中断场景，也是本类型存在的理由。
//  3. 什么都没收到 → 输入仍然要计费（上游可能已经读了 prompt），
//     输出记 0。
func (e *StreamEstimator) Result() Usage {
	if e == nil {
		return Usage{}
	}

	if e.measured != nil {
		return Usage{
			Input:    Count{Tokens: e.measured.Tokens, Source: SourceMeasured, Model: e.model},
			Output:   Count{Tokens: 0, Source: SourceMeasured, Model: e.model},
			Model:    e.model,
			Complete: e.measuredComplete,
			Note:     "流里带过 usage，输入输出均以后者为准",
		}
	}

	out := Usage{
		Input: e.input,
		Output: Count{
			Tokens: e.outputText + e.toolArgs,
			Source: e.outputSource,
			Model:  e.model,
		},
		Model:    e.model,
		Complete: false,
		Note:     "流里没有 usage，用量为本地计算",
	}
	if !e.requestObserved {
		out.Note += "；调用方未提供输入内容，输入计为 0（这不是「没有输入」）"
	}
	return out
}

// countFragment 对一段增量计数。
func (e *StreamEstimator) countFragment(text string) Count {
	if text == "" {
		return Count{Source: SourceMeasured, Model: e.model}
	}
	if e.counter != nil {
		c, err := e.counter.CountText(context.Background(), e.model, text)
		if err == nil {
			return c
		}
	}
	return Count{Tokens: EstimatedTokens(e.model, text), Source: SourceEstimated, Model: e.model}
}

// mergeOutputSource 记下累计过程中出现过的最不可信来源。
//
// 取最大而不是最小：只要有一段是估的，整体就是估的。反过来取会把
// 估算值混进一个标着「精确」的结果里，而那种错误不会报错，只会让
// 调用方在对账时发现「说好的实测怎么对不上」。
func (e *StreamEstimator) mergeOutputSource(s Source) {
	if s > e.outputSource {
		e.outputSource = s
	}
}

// ── 结论 ───────────────────────────────────────────────────────────

// Usage 是一次调用的用量结论。
//
// 分成 Input / Output 两个 Count 而不是混成一个：两者的来源常常不同
// （输入是本地算的，输出来自流里的实测 usage），混在一起会把来源信息
// 压成最保守的那一个，于是看不出「到底哪一部分是估的」。
type Usage struct {
	Input  Count
	Output Count
	// Model 是本次调用实际使用的模型。
	Model string
	// Complete 为 true 表示这次流的结局是「正常结束**且**输出侧是实测值」。
	// 为 false 表示输出侧是本地算的（客户端断开、上游报错、流被截断，
	// 或上游正常结束但没带 usage）。
	//
	// 它是调用方最该看的字段：false 意味着这笔账要按估算处理。
	Complete bool
	// Note 是给运维看的一句话说明。不参与计费，只是让「为什么这个数是
	// 估的」在日志里有个答案——没有它，排查时只能看到一堆 estimated。
	Note string
}

// Total 返回输入与输出的合计，来源取两者中较不可信的那个。
func (u Usage) Total() Count { return u.Input.Add(u.Output) }

// ReviewThresholdTokens 是需要人工复核的估算量阈值。
//
// 取 1000：按典型定价（每百万 token 几元到几十元），1000 token 的偏差在
// 分币级别，一万以上才进入元级别。阈值调低会让「需要复核」的告警变成噪音，
// 而一个每次都报的告警第三次就没人看了。
const ReviewThresholdTokens = 1000

// RequiresReview 表示这笔用量是否值得人工看一眼。
//
// 判据是「输出侧是估算值且量不小」：小额估算（几个 token）不值得复核，
// 而大额估算意味着账单可能有可观偏差。
//
// 让调用方自己决定「要不要上报」而不是本包直接告警：本包不持有告警通道，
// 也不该持有。
func (u Usage) RequiresReview() bool {
	return !u.Complete && u.Output.Source == SourceEstimated &&
		u.Total().Tokens >= ReviewThresholdTokens
}
