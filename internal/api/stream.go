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
// 截断的信号**，这与两个适配器「不补 done 事件」的决定是同一件事。
//
// # 两处已知的简化
//
//   - OpenAI 的用量合并在结束分片里，而官方是在它之后另发一个 choices 为空的
//     分片。分开写会让按 choices[0] 取值的朴素客户端读到空数组而崩，合并
//     则是多一个它们不认识的字段——后者是更安全的偏差。
//   - Anthropic 的 message_start 里 input_tokens 是本地的输入估算（就是预留
//     用的那个数），因为上游的实测用量要到流末尾才拿得到。上游报过 usage 时，
//     真实数字在 message_delta 里给出。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// sseContentType 是流式响应的 Content-Type。
//
// charset 不是协议要求的，但缺了它有些客户端会按 latin-1 解，而事件流里
// 常常有中文。
const sseContentType = "text/event-stream; charset=utf-8"

// writeStreamHeaders 写出流式响应的响应头。**必须在第一个事件之前调用**。
func writeStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", sseContentType)
	// SSE 是给「一边生成一边看」用的，缓存住它就等于把它变成非流式。
	w.Header().Set("Cache-Control", "no-cache")
	// 反向代理（nginx 等）默认会缓冲响应体。缓冲住 SSE 的后果与缓存相同，
	// 而它发生在链路中间——从我们的日志和指标上都看不出来。
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// streamRenderer 把一个统一事件渲染成某个调用协议的 SSE 帧。
//
// 三件事分开：start 写开头的帧（OpenAI 是带 role 的分片，Anthropic 是
// message_start），event 写中间的事件，finish 写结束帧。**调用顺序由调用方
// 保证**，渲染器只负责形状。
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
			openIndex:  -1,
			toolBlocks: map[int]int{},
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

// streamErrorFrame 把一个流中途的错误渲染成调用协议的错误帧。
//
// 措辞按「已经吐过字没有」分两种，判据是 route.ErrOutputStarted（由网关
// 在吐字之后失败时包上）。这个区分是有用的：已经吐过字时调用方手里有半截
// 回答，他需要知道的是「这段被截断了」，而不是「服务暂时不可用」——后者
// 会让他直接重试，而重试会把两段回答接在一起。
//
// 不回上游的错误原文：那里面可能回显请求内容（与非流式的 writeError 同一条
// 规矩）。细节留在日志里。
func streamErrorFrame(protocol channel.Protocol, err error) (frame, error) {
	message := "upstream call failed"
	if errors.Is(err, route.ErrOutputStarted) {
		message = "the response was interrupted before it finished"
	}

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
type anthropicStream struct {
	id    string
	model string
	// input 是输入 token 的本地估算，只用在 message_start 里。
	input int64

	usage        *channel.Usage
	finishReason string

	// nextIndex 是下一个块的分配下标。
	nextIndex int
	// openIndex 是当前打开的块，-1 表示没有。
	openIndex int
	// textOpen 表示当前打开的是正文块。
	textOpen bool
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

func (s *anthropicStream) start(w io.Writer) error {
	msg := claudeStreamMessage{
		ID:    s.id,
		Type:  "message",
		Role:  "assistant",
		Model: s.model,
		// content 必须是数组，不能是 null。
		Content: []any{},
		Usage:   claudeUsage{InputTokens: s.input},
	}
	return writeSSE(w, "message_start", claudeStreamMessageStart{Type: "message_start", Message: msg})
}

func (s *anthropicStream) event(w io.Writer, ev channel.StreamEvent) error {
	switch ev.Kind {
	case "delta":
		if err := s.openTextBlock(w); err != nil {
			return err
		}
		delta := claudeTextDelta{Type: "content_block_delta", Index: s.openIndex}
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
	if err := s.closeBlock(w); err != nil {
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
func (s *anthropicStream) openTextBlock(w io.Writer) error {
	if s.textOpen {
		return nil
	}
	if err := s.closeBlock(w); err != nil {
		return err
	}
	s.openIndex = s.allocate()
	s.textOpen = true
	return writeSSE(w, "content_block_start", claudeBlockStart{
		Type:         "content_block_start",
		Index:        s.openIndex,
		ContentBlock: claudeTextBlock{Type: "text", Text: ""},
	})
}

// openToolBlock 打开（或复用）一次工具调用对应的块，返回块下标。
func (s *anthropicStream) openToolBlock(w io.Writer, tc *channel.ToolCallDelta) (int, error) {
	if index, ok := s.toolBlocks[tc.Index]; ok {
		return index, nil
	}
	if err := s.closeBlock(w); err != nil {
		return 0, err
	}
	index := s.allocate()
	s.toolBlocks[tc.Index] = index
	s.openIndex = index
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

// closeBlock 关掉当前打开的块，没有则什么都不做。
func (s *anthropicStream) closeBlock(w io.Writer) error {
	if s.openIndex < 0 {
		return nil
	}
	index := s.openIndex
	s.openIndex = -1
	s.textOpen = false
	return writeSSE(w, "content_block_stop", claudeBlockStop{Type: "content_block_stop", Index: index})
}

// allocate 分配下一个块下标。
func (s *anthropicStream) allocate() int {
	index := s.nextIndex
	s.nextIndex++
	return index
}
