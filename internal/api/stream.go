package api

// 本文件是「项目 → 调用方」方向的**流式**序列化：把管道里的事件
// （channel.StreamEvent）渲染成调用协议认识的 SSE 帧。
//
// 与非流式那一份（response.go）分开写，因为两边几乎没有共同点：那边是一个
// JSON 体，这边是一串带事件名的帧，而且「先写头、再逐帧写、每帧 flush」这个
// 顺序本身就是契约的一部分。
//
// # 响应头一旦发出去就改不了了
//
// 状态码与响应头在**第一个事件之前**写出去（见 writeStreamHeaders）：之后再
// 想回 500 或 429，只能靠一个已经发出的 200 去解释。所以流开始之后的任何
// 失败都只能作为**流里的事件**表达，不能走 writeError。
//
// # 结束标记不能伪造
//
// 只有真的收到 done 事件才写 [DONE] / message_stop。上游中途断了、连接被
// 关掉，都不写——伪造一个正常结束会让「回答被截断」看起来像「模型答完了」，
// 而调用方会据此把半截结果当成完整结果用。**帧序列里没有结束标记，就是
// 截断的信号**，这与各协议适配器「不补 done 事件」的决定是同一件事。
//
// # 两处已知的简化
//
//   - OpenAI 的用量合并在结束分片里，而官方是在它之后另发一个 choices 为空的
//     分片。分开写会让按 choices[0] 取值的朴素客户端读到空数组而崩，合并
//     则是多一个它们不认识的字段——后者是更安全的偏差。
//   - Anthropic 的 message_start 里的 input_tokens 在**上游没在流的最前面报过
//     输入用量**时是本地的输入估算（就是预留用的那个数）。上游报过就是真值：
//     Anthropic 上游在自己的 message_start 里就给了输入用量，适配器把它发成
//     一个 usage_input 事件，第一帧因此等得到它。跨协议那条路（OpenAI 上游的
//     用量在结束分片里）没有这个数，只能填估算——**等下去会把整个流卡到结束**，
//     那比填估算更坏。真值在 message_delta 里也给，那一处一直是真的。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/route"
)

// sseContentType 是流式响应的 Content-Type。
//
// charset 不是协议要求的，但缺了它有些客户端会按 latin-1 解，而事件流里
// 常常有中文。
const sseContentType = "text/event-stream; charset=utf-8"

// sseContentTypePrefix 是不带 charset 的那一段。
//
// 重放那条路要按**留档的** Content-Type 判断第一次是不是流式（见 writeReplay），
// 而留档的值可能带别的 charset 写法，所以比的是前缀而不是整个常量。
const sseContentTypePrefix = "text/event-stream"

// streamHeaders 把流式响应那几个头写进 h。
//
// # 为什么单独抽出来
//
// 它有**两个**调用方：第一次请求（writeStreamHeaders）与重放（writeReplay）。
// 两处各写一遍的话，以后往这里加一个头就只在新请求上生效，重放悄悄少一个——
// 而重放少了 Cache-Control 恰恰是这几个头要防的事：一个按它行事的中间缓存
// 能把重放出去的 SSE 缓存住，而第一次请求特意防住了这一点。
func streamHeaders(h http.Header) {
	h.Set("Content-Type", sseContentType)
	// SSE 是给「一边生成一边看」用的，缓存住它就等于把它变成非流式。
	h.Set("Cache-Control", "no-cache")
	// 反向代理（nginx 等）默认会缓冲响应体。缓冲住 SSE 的后果与缓存相同，
	// 而它发生在链路中间——从我们的日志和指标上都看不出来。
	h.Set("X-Accel-Buffering", "no")
}

// writeStreamHeaders 写出流式响应的响应头。**必须在第一个事件之前调用**。
func writeStreamHeaders(w http.ResponseWriter) {
	streamHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
}

// streamRenderer 把一个统一事件渲染成某个调用协议的 SSE 帧。
//
// 三件事分开：start 写开头的帧（OpenAI 是带 role 的分片，Anthropic 是
// message_start），event 写中间的事件，finish 写结束帧。**调用顺序由调用方
// 保证**，渲染器只负责形状。
//
// # 开头的帧可以推迟到第一个事件
//
// 调用顺序是「先 start、再逐个 event」，但这不等于第一帧必须在 start 里写出去：
// Anthropic 的 message_start 里要填上游报的输入用量，而那个数**可能就在第一个
// 事件里**（见 channel.StreamEvent 的 usage_input）。所以它的 start 什么都不写，
// 把第一帧留到 event 里——谁先到就按谁写。
//
// **推迟的实现必须在第一个 event 里把它写出去**：帧的顺序是契约的一部分，
// 留到 finish 就等于整条流都没有 message_start。
//
// 三个方法都不导出：接口与两个实现都是本包的内部结构，没有任何包外调用点。
// 导出它们只会让人以为别处可以按形状直接调，而顺序这一半契约在调用方那边。
//
// 方法收 io.Writer 而不是 http.ResponseWriter：渲染器不该知道 flush 的存在，
// 而「每帧之后 flush 一次」是调用方的事（见 serveStream）。
type streamRenderer interface {
	start(w io.Writer) error
	event(w io.Writer, ev channel.StreamEvent) error
	finish(w io.Writer) error
}

// newStreamRenderer 按调用协议选一个渲染器。
//
// 认不出的协议返回错误而不是回落到其中一套：两套协议的帧形状不兼容，
// 回落会让调用方拿到一套它读不懂的帧，而失败现场离原因很远。
func newStreamRenderer(protocol channel.Protocol, id, model string, now time.Time, inputTokens int64) (streamRenderer, error) {
	switch protocol {
	case channel.ProtocolOpenAI:
		return &openAIStream{id: id, model: model, created: now.Unix()}, nil
	case channel.ProtocolAnthropic:
		return &anthropicStream{
			id:         id,
			model:      model,
			input:      inputTokens,
			textIndex:  -1,
			toolBlocks: map[int]int{},
		}, nil
	case channel.ProtocolOpenAIResponses:
		return &responsesStream{
			id:        id,
			model:     model,
			created:   now.Unix(),
			textIndex: -1,
			tools:     map[int]*responsesStreamTool{},
		}, nil
	default:
		return nil, fmt.Errorf("协议 %q 没有流式响应形状", protocol)
	}
}

// ── 帧 ─────────────────────────────────────────────────────────────────

// frame 是一帧 SSE。
type frame struct {
	// event 为空表示不写 event: 行。OpenAI 的帧全都没有事件名，
	// Anthropic 的每一帧都有。
	event string
	data  []byte
}

// write 把一帧写进 w。
//
// data 里不会有裸换行：它来自 json.Marshal，而 JSON 会把字符串里的换行
// 转义成 \n。这一点是必须的——SSE 按行切帧，data 里出现裸换行会把一帧
// 拆成两帧，客户端看到的是截断的 JSON。
func (f frame) write(w io.Writer) error {
	if f.event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", f.event); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", f.data)
	return err
}

// writeSSE 编码并写出一帧。
//
// 编码失败原样返回：这里的载荷都是字符串与数字，编不出来说明结构体里混进了
// 不能编码的东西——那该在改结构体的那一次被发现。静默跳过一帧会让客户端
// 收到一段缺了中间某片的内容，比截断难查得多。
func writeSSE(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码 SSE 帧失败: %w", err)
	}
	return frame{event: event, data: raw}.write(w)
}

// streamErrorMessage 是一句**给调用方看的**流中途失败原因。
//
// 措辞按失败的性质分三种，判据都是错误值本身：
//
//   - 余额耗尽（store.ErrInsufficientAvailable）：这次流是**我们**按余额切断的
//     （见 internal/gateway 的 streamFreeze）。它要说清「回答被截断了」与「该去
//     充值」，见 msgInsufficientBalanceStream；
//   - 已经吐过字之后上游失败：判据是 route.ErrOutputStarted（由网关在吐字之后
//     失败时包上）。调用方手里有半截回答，他需要知道的是「这段被截断了」，而不是
//     「服务暂时不可用」——后者会让他直接重试，而重试会把两段回答接在一起；
//   - 其余：上游调用失败。
//
// 余额那一条排在 ErrOutputStarted 之前：两者说的是两件事，而余额那个更具体
// （它同时说清了原因与处置）。次序也符合「先看是不是我们自己断的」这个读法。
//
// 三套协议共用它：同一个失败在三种形状里说同一句话，形状的差别只在
// 字段名与嵌套上（见 streamErrorFrame 与 responsesStream.errorFrame）。
func streamErrorMessage(err error) string {
	if errors.Is(err, store.ErrInsufficientAvailable) {
		return msgInsufficientBalanceStream
	}
	if errors.Is(err, route.ErrOutputStarted) {
		return "the response was interrupted before it finished"
	}
	return "upstream call failed"
}

// streamErrorFrame 把一个流中途的错误渲染成调用协议的错误帧。
//
// 不回上游的错误原文：那里面可能回显请求内容（与非流式的 writeError 同一条
// 规矩）。细节留在日志里。
//
// Responses 那套**不走这里**：它的事件里带 sequence_number，而那个计数器在
// 渲染器手里（见 responsesStream.errorFrame）。
func streamErrorFrame(protocol channel.Protocol, err error) (frame, error) {
	message := streamErrorMessage(err)

	if protocol == channel.ProtocolAnthropic {
		raw, mErr := json.Marshal(claudeErrorBody{
			Type:  "error",
			Error: claudeError{Type: "api_error", Message: message},
		})
		return frame{event: "error", data: raw}, mErr
	}
	raw, mErr := json.Marshal(openAIErrorBody{
		Error: openAIError{Message: message, Type: "server_error"},
	})
	return frame{data: raw}, mErr
}

// ── OpenAI ─────────────────────────────────────────────────────────────

// openAIStream 渲染 OpenAI 的流式分片。
//
// 形状是「每片一个 choices[0].delta」，结束分片带 finish_reason，最后是
// 一行 `data: [DONE]`。
type openAIStream struct {
	id      string
	model   string
	created int64

	usage        *channel.Usage
	finishReason string
}

type openAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	// Usage 只在结束分片里出现，且只在知道用量时出现。
	Usage *openAIUsage `json:"usage,omitempty"`
}

type openAIStreamChoice struct {
	Index int               `json:"index"`
	Delta openAIStreamDelta `json:"delta"`
	// FinishReason 用指针：中途分片里它是 null，而 null 与空串在客户端
	// 那里是两件事（前者是「还没结束」，后者是「结束了但不知道原因」）。
	FinishReason *string `json:"finish_reason"`
}

type openAIStreamDelta struct {
	// Role 只在第一片里出现。
	Role string `json:"role,omitempty"`
	// Content 不加 omitempty 的反面：空的正文增量没有意义，省掉它让
	// 「这一片只是框架」在报文里一眼可见。
	Content   string                 `json:"content,omitempty"`
	ToolCalls []openAIStreamToolCall `json:"tool_calls,omitempty"`
}

type openAIStreamToolCall struct {
	Index int `json:"index"`
	// ID 与 Type 只在一次工具调用的第一个分片里出现，后续分片里没有。
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type,omitempty"`
	Function openAIStreamToolFunction `json:"function"`
}

type openAIStreamToolFunction struct {
	Name string `json:"name,omitempty"`
	// Arguments 始终写出（哪怕是空串）：它是**分片拼接**的载体，
	// 客户端靠「收到一个 arguments 片段」推进拼接，缺字段与空串
	// 在严格实现里不等价。
	Arguments string `json:"arguments"`
}

func (s *openAIStream) start(w io.Writer) error {
	// 第一片只带 role。官方的第一片是 {"role":"assistant","content":""}，
	// 空 content 被 omitempty 省掉，客户端行为不变。
	return s.chunk(w, openAIStreamDelta{Role: "assistant"}, nil)
}

func (s *openAIStream) event(w io.Writer, ev channel.StreamEvent) error {
	switch ev.Kind {
	case "delta":
		return s.chunk(w, openAIStreamDelta{Content: ev.Text}, nil)

	case "tool_use":
		if ev.ToolCall == nil {
			return nil
		}
		tc := openAIStreamToolCall{
			Index: ev.ToolCall.Index,
			ID:    ev.ToolCall.ID,
			Function: openAIStreamToolFunction{
				Name:      ev.ToolCall.Name,
				Arguments: ev.ToolCall.Arguments,
			},
		}
		// type 只在第一个分片里给：那时才有 id。后续分片只带
		// index 与 arguments 片段。
		if ev.ToolCall.ID != "" {
			tc.Type = "function"
		}
		return s.chunk(w, openAIStreamDelta{ToolCalls: []openAIStreamToolCall{tc}}, nil)

	case "annotation":
		// 引用标注。chat completions 的流式分片里**没有**它的位置：
		// 这套协议没有对应的官方事件，而 relaykit 的 dto 里那个
		// delta.annotations 是非官方扩展，我们不往对外的帧里放它
		//（那会是一次契约变更，见 docs/versioning.md）。
		//
		// 写成显式的空分支而不是让它落到 switch 外面：少这一支时行为完全
		// 相同（默认什么都不做），而读这段的人分不出「漏了」与「决定不转」。
		// Responses 入口那一份会把它渲染成 response.output_text.annotation.added
		//（见 responsesStream.emitAnnotations）。
		return nil

	case "usage":
		if ev.Usage != nil {
			u := *ev.Usage
			s.usage = &u
		}
		return nil

	case "done":
		// 结束分片由 Finish 写：它要带上用量，而用量在 done 之前的
		// 那一片里，这里只把它记下来。
		s.finishReason = ev.FinishReason
		return nil

	case "error":
		f, err := streamErrorFrame(channel.ProtocolOpenAI, ev.Err)
		if err != nil {
			return err
		}
		return f.write(w)
	}
	return nil
}

func (s *openAIStream) finish(w io.Writer) error {
	var reason *string
	// 认不出来的结束原因原样透出，没有就写 null——不编一个 "stop"：
	// 把「上游没正常结束」说成「正常结束」，会让调用方按完成去处理一个
	// 半截结果（与非流式的 openAIFinishReason 同一条规矩）。
	if r := openAIFinishReason(s.finishReason); r != "" {
		reason = &r
	}
	if err := s.chunk(w, openAIStreamDelta{}, reason); err != nil {
		return err
	}
	return frame{data: []byte("[DONE]")}.write(w)
}

// chunk 写一个分片。
func (s *openAIStream) chunk(w io.Writer, delta openAIStreamDelta, reason *string) error {
	return writeSSE(w, "", openAIStreamChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []openAIStreamChoice{{Index: 0, Delta: delta, FinishReason: reason}},
		Usage:   s.usageJSON(),
	})
}

// usageJSON 把上游的用量转成 OpenAI 的口径。
//
// 与结束分片一起发出，见文件头的说明。
func (s *openAIStream) usageJSON() *openAIUsage {
	if s.usage == nil {
		return nil
	}
	return &openAIUsage{
		PromptTokens:     s.usage.PromptTokens,
		CompletionTokens: s.usage.CompletionTokens,
		TotalTokens:      s.usage.PromptTokens + s.usage.CompletionTokens,
	}
}

// ── Anthropic ──────────────────────────────────────────────────────────

// anthropicStream 渲染 Anthropic 的流式事件。
//
// 与 OpenAI 的差别不只是字段名：那边是「每片一个 delta」，这边是**有名字的
// 事件**，而且正文要包在 content_block 的开/关之间：
//
//	message_start → content_block_start → content_block_delta… →
//	content_block_stop → message_delta → message_stop
//
// 块的下标是**我们自己分配**的：上游给的下标属于它自己那条流，而这里要
// 发出的是一条新的流，两者对不上。正文块按下标递增分配，工具调用按上游的
// 分片下标映射到我们分配的块上（同一个工具调用的分片必须落到同一个块里，
// 否则客户端会看到好几个半截的工具调用）。
//
// # 正文块与工具块的开合规则不一样
//
// 正文没有下标：一个 delta 就是「当前这一段正文又长了」，所以正文块开着时
// 又来一段正文，就是同一块在增长。工具调用的分片则**带着上游给的下标，而且
// 可以交错**（OpenAI 允许多个 tool_calls[].index 交替出现，见
// internal/channel/openaiadapter）。两条规则因此必须分开：
//
//   - 打开工具块时关掉开着的正文块：那一段正文已经说完了，它的 stop 该在
//     工具块之前写出去，让它在最终 content 数组里占住自己的位置。
//   - 打开正文块时**不**关工具块：工具块一关，它后面那个分片就落在自己的
//     stop 之后了——那正是这个渲染器要防的事。
//
// 也就是说工具块一旦打开就开到流末尾，多个工具块可以同时开着。两个官方 SDK
// 都接得住这一点：它们的累加器都是「content_block_start 往 content 数组里
// 追加，content_block_delta 与 content_block_stop 按帧里的 index 取块」，
// 所以同时开着不影响拼出来的结果；而**重开同一个下标**会让最终消息里多出
// 一个同 id 的空 tool_use 块，客户端会拿它再调一次工具。协议文档也是按
// 「index 就是它在最终 content 数组里的位置」描述的，所以宁可让块同时开着，
// 也不能让一个下标出现两次。
//
// 顺带一提，「等流末尾再关」不只是取舍的结果，也是这里唯一能做的：统一事件
// 流里没有「块结束了」这件事（anthropicadapter 把上游的 content_block_stop
// 丢掉了，openaiadapter 那边 OpenAI 压根不发），所以渲染器根本收不到「这个
// 工具调用说完了」的信号。
type anthropicStream struct {
	id    string
	model string
	// input 是输入 token 的本地估算。它只在**上游没在流的最前面报过输入用量**
	// 时用来填 message_start（见 writeStart）——也就是跨协议那条路。
	input int64

	// started 表示 message_start 已经写出去了。第一帧由第一个事件写出，
	// 理由见 writeStart。
	started bool

	usage        *channel.Usage
	finishReason string

	// nextIndex 是下一个块的分配下标。
	nextIndex int
	// textIndex 是当前打开的正文块下标，-1 表示没有正文块开着。
	textIndex int
	// openToolBlocks 是当前开着的工具块下标，按打开顺序排列（关的时候也按
	// 这个顺序）。它只在流末尾清空：见上面「正文块与工具块的开合规则不一样」。
	openToolBlocks []int
	// toolBlocks 把上游的工具调用下标映射到本流里分配的块下标。
	toolBlocks map[int]int
}

type claudeStreamMessageStart struct {
	Type    string              `json:"type"`
	Message claudeStreamMessage `json:"message"`
}

type claudeStreamMessage struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []any       `json:"content"`
	StopReason   *string     `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        claudeUsage `json:"usage"`
}

type claudeBlockStart struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type claudeTextDelta struct {
	Type  string              `json:"type"`
	Index int                 `json:"index"`
	Delta claudeTextDeltaBody `json:"delta"`
}

type claudeTextDeltaBody struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeJSONDelta struct {
	Type  string              `json:"type"`
	Index int                 `json:"index"`
	Delta claudeJSONDeltaBody `json:"delta"`
}

type claudeJSONDeltaBody struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type claudeBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type claudeMessageDelta struct {
	Type  string              `json:"type"`
	Delta claudeStopDeltaBody `json:"delta"`
	// Usage 只在知道用量时出现：没有实测值时不写，而不是写一个 0——
	// 0 会被客户端当成「这次真的没消耗」。
	Usage *claudeUsage `json:"usage,omitempty"`
}

type claudeStopDeltaBody struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type claudeMessageStop struct {
	Type string `json:"type"`
}

// start 在这里**什么都不写**，第一帧由第一个事件写出去（见 writeStart）。
//
// 理由是时序：message_start 里的 input_tokens 要用上游报的真值，而那个数
// **可能就在第一个事件里**（usage_input——Anthropic 上游在自己的 message_start
// 里就给了输入用量），也可能永远不来（跨协议那条路：OpenAI 的用量在结束分片
// 里）。所以「写第一帧」只能发生在**看到第一个事件之后**：谁先到就按谁写。
//
// 调用方那边没有变化：它照旧先 start、再逐个 event，而 start 不写东西与写一帧
// 在它看来是同一件事（每帧之后 flush 一次）。
func (s *anthropicStream) start(io.Writer) error { return nil }

// writeStart 写 message_start，input_tokens 取第一个事件里能拿到的东西。
//
// 判据是**第一个事件是谁**：
//
//   - 带用量的事件（usage_input 是输入侧刚确定，usage 是完整用量）：上游在流
//     的最前面就报了输入用量，用真值；
//   - 其余（内容帧、结束标记、错误）：流里没有更早的输入用量可给，用本地估算。
//
// 最后一条是**必须**的，不是保守：等下去的话，跨协议那条路会把整个流卡到结束
// 才发第一帧——那比报一个偏低的估算更坏。
//
// 估算与真值都走 claudeUsageOf 这**同一个**换算入口（message_delta 用的也是
// 它）：自己拼一份的话，缓存那一份会只加在一边，两帧对同一个请求报出两个不同的
// input_tokens，而对账的人会去找一个不存在的差额。
func (s *anthropicStream) writeStart(w io.Writer, first channel.StreamEvent) error {
	u := channel.Usage{PromptTokens: s.input}
	if first.Usage != nil && (first.Kind == "usage_input" || first.Kind == "usage") {
		u = *first.Usage
	}
	msg := claudeStreamMessage{
		ID:    s.id,
		Type:  "message",
		Role:  "assistant",
		Model: s.model,
		// content 必须是数组，不能是 null。
		Content: []any{},
		Usage:   claudeUsageOf(u),
	}
	return writeSSE(w, "message_start", claudeStreamMessageStart{Type: "message_start", Message: msg})
}

func (s *anthropicStream) event(w io.Writer, ev channel.StreamEvent) error {
	if !s.started {
		s.started = true
		if err := s.writeStart(w, ev); err != nil {
			return err
		}
	}
	switch ev.Kind {
	case "delta":
		if err := s.openTextBlock(w); err != nil {
			return err
		}
		delta := claudeTextDelta{Type: "content_block_delta", Index: s.textIndex}
		delta.Delta.Type = "text_delta"
		delta.Delta.Text = ev.Text
		return writeSSE(w, "content_block_delta", delta)

	case "tool_use":
		if ev.ToolCall == nil {
			return nil
		}
		index, err := s.openToolBlock(w, ev.ToolCall)
		if err != nil {
			return err
		}
		if ev.ToolCall.Arguments == "" {
			return nil
		}
		delta := claudeJSONDelta{Type: "content_block_delta", Index: index}
		delta.Delta.Type = "input_json_delta"
		delta.Delta.PartialJSON = ev.ToolCall.Arguments
		return writeSSE(w, "content_block_delta", delta)

	case "usage_input":
		// 输入侧的用量。第一个事件就是它时，writeStart 已经用它填了
		// message_start；更晚才到时，那一帧早就写出去了，这里也补不回来。
		// 两种情况下这一层都没有别的可做——它不是结算依据，结算等 "usage"
		// （见 channel.StreamEvent 的 Usage）。
		return nil

	case "annotation":
		// 引用标注。Anthropic 的对应物是 content_block_delta 里的
		// citations_delta，而那条形状要挂在**哪一个正文块**上、以及
		// citation 的字段怎么映射，都是另一次契约变更（见 docs/versioning.md）。
		// 今天只把 Responses 入口那一条补上：它与非流式那条路是同一个协议，
		// 而 chat 与 Anthropic 两侧没有「非流式已经有、流式丢了」这个缺口。
		//
		// 写成显式的空分支而不是让它落到 switch 外面：见 openAIStream.event
		// 里同一个位置的说明。
		return nil

	case "usage":
		if ev.Usage != nil {
			u := *ev.Usage
			s.usage = &u
		}
		return nil

	case "done":
		s.finishReason = ev.FinishReason
		return nil

	case "error":
		f, err := streamErrorFrame(channel.ProtocolAnthropic, ev.Err)
		if err != nil {
			return err
		}
		return f.write(w)
	}
	return nil
}

func (s *anthropicStream) finish(w io.Writer) error {
	// 关的顺序不影响客户端（它按 index 找块），但按下标从小到大更顺眼：与
	// 最终 content 数组的顺序一致。同时开着的时候正文块的下标总是最大的
	// （打开工具块会先关掉正文块再分配自己的下标），所以先关工具块。
	if err := s.closeToolBlocks(w); err != nil {
		return err
	}
	if err := s.closeTextBlock(w); err != nil {
		return err
	}

	md := claudeMessageDelta{Type: "message_delta"}
	if r := claudeStopReason(s.finishReason); r != "" {
		md.Delta.StopReason = &r
	}
	if s.usage != nil {
		u := claudeUsageOf(*s.usage)
		md.Usage = &u
	}
	if err := writeSSE(w, "message_delta", md); err != nil {
		return err
	}
	return writeSSE(w, "message_stop", claudeMessageStop{Type: "message_stop"})
}

// openTextBlock 打开正文块，已经开着就什么都不做。
//
// 延迟打开而不是在 Start 里就开：一条只有工具调用的回答不该出现空的正文块，
// 而空正文块会让客户端渲染出一段空白。
//
// 每次打开都分配一个新下标，哪怕前面开过正文块：工具调用之后的正文是
// **另一块**，复用下标会让客户端把两段正文当成同一段在增长。
//
// 这里不关工具块：理由见结构体上「正文块与工具块的开合规则不一样」。
func (s *anthropicStream) openTextBlock(w io.Writer) error {
	if s.textIndex >= 0 {
		return nil
	}
	s.textIndex = s.allocate()
	return writeSSE(w, "content_block_start", claudeBlockStart{
		Type:         "content_block_start",
		Index:        s.textIndex,
		ContentBlock: claudeTextBlock{Type: "text", Text: ""},
	})
}

// openToolBlock 打开（或复用）一次工具调用对应的块，返回块下标。
func (s *anthropicStream) openToolBlock(w io.Writer, tc *channel.ToolCallDelta) (int, error) {
	if index, ok := s.toolBlocks[tc.Index]; ok {
		// 复用：这个工具调用的块已经开着。工具块只在流末尾关，所以它一定
		// 还开着——分片直接续在它后面，不必重开（重开会多出一个同 id 的
		// 空块，见结构体上的说明）。
		return index, nil
	}
	// 新的工具调用：它开在正文块之后，先把那一段正文关掉。
	if err := s.closeTextBlock(w); err != nil {
		return 0, err
	}
	index := s.allocate()
	s.toolBlocks[tc.Index] = index
	s.openToolBlocks = append(s.openToolBlocks, index)
	err := writeSSE(w, "content_block_start", claudeBlockStart{
		Type:  "content_block_start",
		Index: index,
		ContentBlock: claudeToolUseBlock{
			Type: "tool_use",
			ID:   tc.ID,
			Name: tc.Name,
			// input 是必填对象，参数要靠后面的 input_json_delta 拼出来。
			Input: map[string]any{},
		},
	})
	if err != nil {
		return 0, err
	}
	return index, nil
}

// closeTextBlock 关掉开着的正文块，没有则什么都不做。
func (s *anthropicStream) closeTextBlock(w io.Writer) error {
	if s.textIndex < 0 {
		return nil
	}
	index := s.textIndex
	s.textIndex = -1
	return writeBlockStop(w, index)
}

// closeToolBlocks 关掉所有开着的工具块，没有则什么都不做。
//
// 只在流末尾调用：中途关掉其中任何一个，它后面那些分片就落在自己的 stop
// 之后了——那正是这个渲染器要防的事。
func (s *anthropicStream) closeToolBlocks(w io.Writer) error {
	open := s.openToolBlocks
	s.openToolBlocks = nil
	for _, index := range open {
		if err := writeBlockStop(w, index); err != nil {
			return err
		}
	}
	return nil
}

// writeBlockStop 写一帧 content_block_stop。
func writeBlockStop(w io.Writer, index int) error {
	return writeSSE(w, "content_block_stop", claudeBlockStop{Type: "content_block_stop", Index: index})
}

// allocate 分配下一个块下标。
func (s *anthropicStream) allocate() int {
	index := s.nextIndex
	s.nextIndex++
	return index
}

// ── OpenAI Responses ───────────────────────────────────────────────────

// responsesStream 渲染 OpenAI Responses 的流式事件。
//
// 形状是「一串带名字的事件」，每一项输出都有开始 / 增量 / 结束三段：
//
//	response.created
//	response.output_item.added    → response.output_text.delta…
//	                              → response.output_text.done
//	                              → response.output_item.done
//	response.output_item.added    → response.function_call_arguments.delta…
//	                              → response.function_call_arguments.done
//	                              → response.output_item.done
//	response.completed（或 response.incomplete）
//
// 与非流式那一份（responses.go）共用同一批结构体：事件里嵌的那个 response
// 对象就是非流式的响应体，两处各写一份只会让它们慢慢分岔。
//
// # output_index 在**打开时**分配，而且只分配一次
//
// 它是这一项在最终 output[] 里的位置，客户端按它把增量拼回项里。所以正文项
// 在第一个正文增量到达时才分配（一个只有工具调用的回答不该出现空的正文项），
// 工具项在它第一次出现时才分配。
//
// # 工具项一旦打开就开到流末尾
//
// 中途关掉它，后面那些参数分片就落在自己的 done 之后了。这与 anthropicStream
// 的取舍相同，理由也相同：统一事件流里没有「这个工具调用说完了」这件事
// （anthropicadapter 把上游的 content_block_stop 丢掉了，openaiadapter 那边
// OpenAI 压根不发），所以渲染器根本收不到那个信号。
//
// # 正文项只有一项
//
// 与 anthropicStream 不同，这里不给工具调用之后的正文另开一项：正文项的
// 下标已经分配过，后续增量拼进同一项即可（客户端按 index 找项），而多开一项
// 会让最终 output[] 里出现两条助手消息。顺序因此是「正文项在最前、工具项
// 按出现顺序在后」，与 responsesOutput 的非流式那一份一致。
type responsesStream struct {
	id      string
	model   string
	created int64

	// seq 是每个事件的 sequence_number。官方 SDK 的事件模型把它标成必填，
	// 所以每个事件都要带上，而且从 0 起递增。
	seq int64

	// items 按分配顺序记下每一项。finish 时按这个顺序收尾，它就是最终
	// output[] 的顺序。
	items []responsesStreamItem

	// textIndex 是正文项的下标，-1 表示还没有正文。
	textIndex int
	text      strings.Builder

	// annotations 是这条流里已经转出去的引用标注，按下标顺序。
	//
	// 它是收尾事件里 output[].content[].annotations 的来源：增量事件与最终
	// 那一份必须同序同内容，否则客户端按 annotation_index 拼出来的结果与
	// 收尾时整份替换掉的结果对不上。与非流式那条路（responses.go 的
	// responsesOutput）是同一个口径。
	annotations []map[string]any

	// tools 把上游给的工具调用下标映射到本流里的一项。
	tools map[int]*responsesStreamTool

	usage        *channel.Usage
	finishReason string
}

// responsesStreamItem 是已分配的一项：tool 为 nil 表示这是正文项。
type responsesStreamItem struct {
	outputIndex int
	tool        *responsesStreamTool
}

// responsesStreamTool 是一次工具调用在本流里的状态。
type responsesStreamTool struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
}

// responsesEvent 是一帧 Responses 事件的载荷。
//
// 用一个结构接住全部事件类型，而不是按 type 各声明一个：这里只会发其中几种，
// 而字段集高度重合（每一条都带 type 与 sequence_number）。不在这条事件里的
// 字段靠 omitempty 省掉——这与另外两套协议的做法不同（那边每帧一个结构体），
// 因为那边一帧里的字段少且互不重合。
type responsesEvent struct {
	Type           string             `json:"type"`
	SequenceNumber int64              `json:"sequence_number"`
	Response       *responsesResponse `json:"response,omitempty"`
	OutputIndex    *int               `json:"output_index,omitempty"`
	ContentIndex   *int               `json:"content_index,omitempty"`
	ItemID         string             `json:"item_id,omitempty"`
	Item           any                `json:"item,omitempty"`
	Delta          string             `json:"delta,omitempty"`
	// Text 与 Arguments 用指针：它们只在 done 事件里出现，而**空串是合法值**
	// （模型答了个空串、工具没有参数），省掉字段与给空串在严格实现里不等价。
	Text      *string `json:"text,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
	// 引用标注（response.output_text.annotation.added）。Annotation 是标注
	// 本身，AnnotationIndex 是它在这一项的标注列表里的下标——客户端按它
	// 把标注拼回最终 output[] 的那一项（收尾事件里那份是本流的权威列表）。
	Annotation      map[string]any `json:"annotation,omitempty"`
	AnnotationIndex *int           `json:"annotation_index,omitempty"`
	// 错误事件（平铺，没有 error 外层，见 errorFrame）。
	Code    string  `json:"code,omitempty"`
	Message string  `json:"message,omitempty"`
	Param   *string `json:"param,omitempty"`
}

// start 写 response.created，它是这条流的第一条事件。
//
// 与非流式那份的差别只有 status 与空的 output：这条流还没开始产出，
// 而客户端要拿这个 id 与 model 去建自己的状态。
func (s *responsesStream) start(w io.Writer) error {
	return s.emit(w, "response.created", responsesEvent{
		Response: &responsesResponse{
			ID:         s.id,
			Object:     "response",
			CreatedAt:  s.created,
			Status:     "in_progress",
			Model:      s.model,
			Output:     []any{},
			ToolChoice: "auto",
			Tools:      []any{},
		},
	})
}

func (s *responsesStream) event(w io.Writer, ev channel.StreamEvent) error {
	switch ev.Kind {
	case "delta":
		if err := s.openText(w); err != nil {
			return err
		}
		s.text.WriteString(ev.Text)
		index := s.textIndex
		return s.emit(w, "response.output_text.delta", responsesEvent{
			OutputIndex:  &index,
			ContentIndex: intPtr(0),
			ItemID:       responsesMessageItemID(s.id),
			Delta:        ev.Text,
		})

	case "annotation":
		return s.emitAnnotations(w, ev.Annotations)

	case "tool_use":
		if ev.ToolCall == nil {
			return nil
		}
		tool, err := s.openTool(w, ev.ToolCall)
		if err != nil {
			return err
		}
		if ev.ToolCall.Arguments == "" {
			return nil
		}
		tool.arguments.WriteString(ev.ToolCall.Arguments)
		index := tool.outputIndex
		return s.emit(w, "response.function_call_arguments.delta", responsesEvent{
			OutputIndex: &index,
			ItemID:      tool.itemID,
			Delta:       ev.ToolCall.Arguments,
		})

	case "usage_input":
		// 输入侧的用量。Responses 的用量只在终态事件里报（见 responsesadapter
		// 的说明），所以这里没有可写的地方——它不是结算依据。
		return nil

	case "usage":
		if ev.Usage != nil {
			u := *ev.Usage
			s.usage = &u
		}
		return nil

	case "done":
		// 终态事件由 finish 写：它要带上用量与最终的 output[]，而用量在
		// done 之前的那一片里。
		s.finishReason = ev.FinishReason
		return nil

	case "error":
		return s.errorFrame(w, ev.Err)
	}
	return nil
}

// errorFrame 写一次流中途失败。
//
// Responses 的流中途失败是一个**平铺**的事件——type / code / message / param
// 就在顶层，没有另外两套那样的 error 外层。这是第三套形状，不能与它们合并，
// 所以它没有走 streamErrorFrame。
func (s *responsesStream) errorFrame(w io.Writer, err error) error {
	return s.emit(w, "error", responsesEvent{
		Code:    "server_error",
		Message: streamErrorMessage(err),
	})
}

// emitAnnotations 把一批引用标注渲染成 response.output_text.annotation.added。
//
// 每一项一条事件，下标是它在本流标注列表里的位置（从 0 起、连续）。这个列表
// 也原样进收尾事件的 output[]——客户端按 collection 语义把增量拼成最终那一份，
// 两处必须是同一个顺序、同一批内容（与非流式的 output[].content[].annotations
// 也是同一批）。
//
// 标注到达时正文项可能还没打开（上游只保证「标注属于这一项」，不保证先有
// 哪怕一个正文增量）。这里先把项打开：事件里的 output_index / item_id 必须
// 指向一个已经存在、且与 item.added 里那个一致的项，客户端才能把它挂上去。
func (s *responsesStream) emitAnnotations(w io.Writer, annotations []map[string]any) error {
	if len(annotations) == 0 {
		return nil
	}
	if err := s.openText(w); err != nil {
		return err
	}
	index := s.textIndex
	for _, annotation := range annotations {
		if len(annotation) == 0 {
			continue
		}
		annotationIndex := len(s.annotations)
		s.annotations = append(s.annotations, annotation)
		if err := s.emit(w, "response.output_text.annotation.added", responsesEvent{
			OutputIndex:     &index,
			ContentIndex:    intPtr(0),
			ItemID:          responsesMessageItemID(s.id),
			Annotation:      annotation,
			AnnotationIndex: intPtr(annotationIndex),
		}); err != nil {
			return err
		}
	}
	return nil
}

// openText 打开正文项，已经开着就什么都不做。
func (s *responsesStream) openText(w io.Writer) error {
	if s.textIndex >= 0 {
		return nil
	}
	s.textIndex = s.allocate(nil)
	index := s.textIndex
	return s.emit(w, "response.output_item.added", responsesEvent{
		OutputIndex: &index,
		ItemID:      responsesMessageItemID(s.id),
		Item: responsesMessageItem{
			Type:    "message",
			ID:      responsesMessageItemID(s.id),
			Status:  "in_progress",
			Role:    "assistant",
			Content: []responsesContent{},
		},
	})
}

// openTool 打开（或复用）一次工具调用对应的项。
//
// 名字取**第一次**出现的那一片：三家的适配器都把 id 与名字放在这个工具调用
// 的第一个分片里（OpenAI 的首片带 function.name、Anthropic 的
// content_block_start 带 name、Responses 的 output_item.added 带 name），
// 后面那些分片只有参数片段。
func (s *responsesStream) openTool(w io.Writer, tc *channel.ToolCallDelta) (*responsesStreamTool, error) {
	if tool, ok := s.tools[tc.Index]; ok {
		return tool, nil
	}
	tool := &responsesStreamTool{
		itemID: responsesCallItemID(s.id, tc.Index),
		callID: tc.ID,
		name:   tc.Name,
	}
	tool.outputIndex = s.allocate(tool)
	s.tools[tc.Index] = tool
	index := tool.outputIndex
	return tool, s.emit(w, "response.output_item.added", responsesEvent{
		OutputIndex: &index,
		ItemID:      tool.itemID,
		Item: responsesFunctionCallItem{
			Type:   "function_call",
			ID:     tool.itemID,
			Status: "in_progress",
			CallID: tool.callID,
			Name:   tool.name,
		},
	})
}

// finish 收尾：逐项写 done 事件，再写终态事件。
//
// 逐项的顺序按 output_index 升序——最终 output[] 的顺序就是它，而每项的
// done 事件要与它在数组里的位置一致。
func (s *responsesStream) finish(w io.Writer) error {
	status, incomplete := responsesStatus(s.finishReason)

	output := make([]any, 0, len(s.items))
	for _, item := range s.items {
		index := item.outputIndex
		if item.tool == nil {
			text := s.text.String()
			if err := s.emit(w, "response.output_text.done", responsesEvent{
				OutputIndex:  &index,
				ContentIndex: intPtr(0),
				ItemID:       responsesMessageItemID(s.id),
				Text:         &text,
			}); err != nil {
				return err
			}
			done := s.messageItem(status)
			output = append(output, done)
			if err := s.emit(w, "response.output_item.done", responsesEvent{
				OutputIndex: &index,
				ItemID:      done.ID,
				Item:        done,
			}); err != nil {
				return err
			}
			continue
		}

		arguments := item.tool.arguments.String()
		if err := s.emit(w, "response.function_call_arguments.done", responsesEvent{
			OutputIndex: &index,
			ItemID:      item.tool.itemID,
			Arguments:   &arguments,
		}); err != nil {
			return err
		}
		done := responsesFunctionCallItem{
			Type:      "function_call",
			ID:        item.tool.itemID,
			Status:    status,
			CallID:    item.tool.callID,
			Name:      item.tool.name,
			Arguments: arguments,
		}
		output = append(output, done)
		if err := s.emit(w, "response.output_item.done", responsesEvent{
			OutputIndex: &index,
			ItemID:      done.ID,
			Item:        done,
		}); err != nil {
			return err
		}
	}

	resp := &responsesResponse{
		ID:                s.id,
		Object:            "response",
		CreatedAt:         s.created,
		Status:            status,
		Model:             s.model,
		Output:            output,
		IncompleteDetails: incomplete,
		ToolChoice:        "auto",
		Tools:             []any{},
	}
	if s.usage != nil {
		resp.Usage = responsesUsageOf(*s.usage)
	}
	return s.emit(w, responsesFinishEvent(status), responsesEvent{Response: resp})
}

// allocate 分配下一个输出项下标并把它记进顺序表。
func (s *responsesStream) allocate(tool *responsesStreamTool) int {
	index := len(s.items)
	s.items = append(s.items, responsesStreamItem{outputIndex: index, tool: tool})
	return index
}

// messageItem 是收尾时写出的那个完整的正文项。
func (s *responsesStream) messageItem(status string) responsesMessageItem {
	return responsesMessageItem{
		Type:   "message",
		ID:     responsesMessageItemID(s.id),
		Status: status,
		Role:   "assistant",
		Content: []responsesContent{{
			Type:        "output_text",
			Text:        s.text.String(),
			Annotations: annotationList(s.annotations),
		}},
	}
}

// emit 写一帧事件，sequence_number 由这里递增。
//
// 计数器放在一处而不是每个调用点各给一个数：漏掉一个或重复一个都会让客户端的
// 顺序判据失效，而那种失效在报文里看不出来。
func (s *responsesStream) emit(w io.Writer, eventType string, payload responsesEvent) error {
	payload.Type = eventType
	payload.SequenceNumber = s.seq
	s.seq++
	return writeSSE(w, eventType, payload)
}

// intPtr 取一个 int 的地址。事件的字段用指针区分「不在这条事件里」与「值是 0」。
func intPtr(v int) *int { return &v }
