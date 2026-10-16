package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/tokens"
)

// 本文件测的是「事件 → 客户端 SSE 帧」这一段：帧的事件名、data: 前缀、
// 结束标记、以及两条路各自的形状。
//
// 断言按**帧序列**逐条给，不写「有内容就行」：形状错的时候（少一个
// content_block_stop、把 [DONE] 写在错误的地方）客户端不会报错，只会
// 渲染出半截内容——那是最难发现的一类问题。

const streamBody = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`

// ── 解析 ──────────────────────────────────────────────────────────────

// sseFrame 是一帧解析出来的 SSE。
type sseFrame struct {
	event string
	data  string
}

// parseFrames 把响应体切成帧。
//
// 它同时是一道**形状检查**：不认识的字段名直接报错。SSE 里字段名拼错
// （`Event:` / `data :`）在客户端表现为「这一帧被忽略」，而忽略掉一帧的
// 流看起来仍然是一条正常的流。
func parseFrames(t *testing.T, body string) []sseFrame {
	t.Helper()

	var out []sseFrame
	for _, block := range strings.Split(strings.TrimRight(body, "\n"), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			default:
				t.Errorf("不认识的 SSE 行: %q（整帧 %q）", line, block)
			}
		}
		out = append(out, f)
	}
	return out
}

// decode 把一帧的 data 解成 map。
func (f sseFrame) decode(t *testing.T) map[string]any {
	t.Helper()

	var m map[string]any
	if err := json.Unmarshal([]byte(f.data), &m); err != nil {
		t.Fatalf("帧的 data 不是 JSON: %v\n%s", err, f.data)
	}
	return m
}

// at 取第 i 帧，越界时直接失败（避免后续断言读一个零值帧而静默通过）。
func at(t *testing.T, frames []sseFrame, i int) sseFrame {
	t.Helper()
	if i >= len(frames) {
		t.Fatalf("只有 %d 帧，取不到第 %d 帧: %+v", len(frames), i, frames)
	}
	return frames[i]
}

// dig 逐层取嵌套的字段，数组按下标取（choices / content 都是数组）。
func dig(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()

	var cur any = m
	for _, key := range path {
		switch obj := cur.(type) {
		case map[string]any:
			cur = obj[key]
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(obj) {
				t.Fatalf("取 %v 时下标 %q 越界（长度 %d）", path, key, len(obj))
			}
			cur = obj[i]
		default:
			t.Fatalf("取 %v 时 %v 既不是对象也不是数组", path, cur)
		}
	}
	return cur
}

// streamRequest 发一次流式请求，返回响应记录与假的上游结果。
func streamRequest(t *testing.T, path string, events ...channel.StreamEvent) (*fakeChatter, []sseFrame) {
	t.Helper()

	h, _, chatter := newTestHandler()
	chatter.streamResult = &gateway.Result{Events: streamOf(events...)}

	rec := post(h, path, streamBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	// 事件流不能被缓存，也不能被反向代理缓冲——两条都会把它变回非流式。
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q，期望 no-cache", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q，期望 no", got)
	}
	return chatter, parseFrames(t, rec.Body.String())
}

// ── OpenAI ────────────────────────────────────────────────────────────

func TestOpenAIStreamFrames(t *testing.T) {
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	_, frames := streamRequest(t, PathChatCompletions,
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		channel.StreamEvent{Kind: "usage", Usage: &usage},
		channel.StreamEvent{Kind: "done", FinishReason: "stop"},
	)

	// 四帧：开场分片、正文分片、结束分片、[DONE]。
	if len(frames) != 4 {
		t.Fatalf("帧数 = %d，期望 4: %+v", len(frames), frames)
	}
	for i, f := range frames {
		// OpenAI 的流不带事件名：多一个 event: 行不会让客户端报错，
		// 但会让「按 event 名分派」的通用 SSE 客户端把它丢掉。
		if f.event != "" {
			t.Errorf("第 %d 帧不该有事件名: %q", i, f.event)
		}
	}

	first := at(t, frames, 0).decode(t)
	if first["object"] != "chat.completion.chunk" {
		t.Errorf("object = %v，期望 chat.completion.chunk", first["object"])
	}
	if first["model"] != "m-1" {
		t.Errorf("model = %v，期望 m-1（报给调用方的是他要的那个模型名）", first["model"])
	}
	if got := dig(t, first, "choices", "0", "delta", "role"); got != "assistant" {
		t.Errorf("首帧的 role = %v，期望 assistant", got)
	}

	second := at(t, frames, 1).decode(t)
	if got := dig(t, second, "choices", "0", "delta", "content"); got != "你好" {
		t.Errorf("正文 = %v，期望 你好", got)
	}
	if got := dig(t, second, "choices", "0", "finish_reason"); got != nil {
		t.Errorf("正文分片的 finish_reason = %v，期望 null", got)
	}

	// 结束分片：finish_reason 与用量在**同一帧**里（见 stream.go 的说明）。
	last := at(t, frames, 2).decode(t)
	if got := dig(t, last, "choices", "0", "finish_reason"); got != "stop" {
		t.Errorf("finish_reason = %v，期望 stop", got)
	}
	if got := dig(t, last, "usage", "prompt_tokens"); got != float64(12) {
		t.Errorf("prompt_tokens = %v，期望 12", got)
	}
	if got := dig(t, last, "usage", "completion_tokens"); got != float64(7) {
		t.Errorf("completion_tokens = %v，期望 7", got)
	}
	if got := dig(t, last, "usage", "total_tokens"); got != float64(19) {
		t.Errorf("total_tokens = %v，期望 19", got)
	}

	if done := at(t, frames, 3); done.data != "[DONE]" {
		t.Errorf("最后一帧 = %q，期望 data: [DONE]", done.data)
	}
}

// OpenAI 的工具调用分片：参数是**分片拼接**的，index 必须原样带上，
// 否则多个并行的工具调用会拼到一起。
func TestOpenAIStreamToolCall(t *testing.T) {
	_, frames := streamRequest(t, PathChatCompletions,
		channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
			Index: 1, ID: "call_1", Name: "get_weather", Arguments: `{"city":`,
		}},
		channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
			Index: 1, Arguments: `"北京"}`,
		}},
		channel.StreamEvent{Kind: "done", FinishReason: "tool_calls"},
	)

	first := at(t, frames, 1).decode(t)
	if got := dig(t, first, "choices", "0", "delta", "tool_calls", "0", "index"); got != float64(1) {
		t.Errorf("index = %v，期望 1", got)
	}
	if got := dig(t, first, "choices", "0", "delta", "tool_calls", "0", "id"); got != "call_1" {
		t.Errorf("id = %v，期望 call_1", got)
	}
	if got := dig(t, first, "choices", "0", "delta", "tool_calls", "0", "function", "name"); got != "get_weather" {
		t.Errorf("name = %v，期望 get_weather", got)
	}
	if got := dig(t, first, "choices", "0", "delta", "tool_calls", "0", "function", "arguments"); got != `{"city":` {
		t.Errorf("第一个参数分片 = %v", got)
	}

	// 后续分片只有 index 与参数片段：带上 id 会让客户端以为这是**另一次**
	// 工具调用。
	second := at(t, frames, 2).decode(t)
	if got := dig(t, second, "choices", "0", "delta", "tool_calls", "0", "id"); got != nil {
		t.Errorf("后续分片不该带 id: %v", got)
	}
	if got := dig(t, second, "choices", "0", "delta", "tool_calls", "0", "function", "arguments"); got != `"北京"}` {
		t.Errorf("第二个参数分片 = %v", got)
	}

	if got := dig(t, at(t, frames, 3).decode(t), "choices", "0", "finish_reason"); got != "tool_calls" {
		t.Errorf("finish_reason = %v，期望 tool_calls", got)
	}
}

// ── Anthropic ─────────────────────────────────────────────────────────

func TestAnthropicStreamFrames(t *testing.T) {
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	_, frames := streamRequest(t, PathMessages,
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		channel.StreamEvent{Kind: "usage", Usage: &usage},
		channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
	)

	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(frames) != len(want) {
		t.Fatalf("帧数 = %d，期望 %d: %+v", len(frames), len(want), frames)
	}
	for i, name := range want {
		if got := at(t, frames, i).event; got != name {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, got, name)
		}
	}

	start := at(t, frames, 0).decode(t)
	if start["type"] != "message_start" {
		t.Errorf("type = %v，期望 message_start", start["type"])
	}
	if got := dig(t, start, "message", "model"); got != "m-1" {
		t.Errorf("model = %v，期望 m-1", got)
	}
	if got := dig(t, start, "message", "role"); got != "assistant" {
		t.Errorf("role = %v，期望 assistant", got)
	}
	// message_start 里的输入量是本地估算（预留用的那个数）：上游的实测用量
	// 要到流末尾才拿得到，而这一帧必须在最前面。
	wantInput := float64(tokens.EstimatedTokens("m-1", "hi"))
	if got := dig(t, start, "message", "usage", "input_tokens"); got != wantInput {
		t.Errorf("message_start 的 input_tokens = %v，期望 %v", got, wantInput)
	}

	blockStart := at(t, frames, 1).decode(t)
	if got := dig(t, blockStart, "content_block", "type"); got != "text" {
		t.Errorf("content_block.type = %v，期望 text", got)
	}
	if got := blockStart["index"]; got != float64(0) {
		t.Errorf("正文块的下标 = %v，期望 0", got)
	}

	delta := at(t, frames, 2).decode(t)
	if got := dig(t, delta, "delta", "type"); got != "text_delta" {
		t.Errorf("delta.type = %v，期望 text_delta", got)
	}
	if got := dig(t, delta, "delta", "text"); got != "你好" {
		t.Errorf("delta.text = %v，期望 你好", got)
	}

	if got := at(t, frames, 3).decode(t)["index"]; got != float64(0) {
		t.Errorf("content_block_stop 的下标 = %v，期望 0", got)
	}

	end := at(t, frames, 4).decode(t)
	if got := dig(t, end, "delta", "stop_reason"); got != "end_turn" {
		t.Errorf("stop_reason = %v，期望 end_turn", got)
	}
	if got := dig(t, end, "usage", "output_tokens"); got != float64(7) {
		t.Errorf("output_tokens = %v，期望 7", got)
	}
	if got := dig(t, end, "usage", "input_tokens"); got != float64(12) {
		t.Errorf("input_tokens = %v，期望 12（上游报过 usage 时以实测为准）", got)
	}

	if got := at(t, frames, 5).decode(t)["type"]; got != "message_stop" {
		t.Errorf("最后一帧的 type = %v，期望 message_stop", got)
	}
}

// Anthropic 的工具调用：块要在**开着的正文块之后**开，参数分片要落到同一个
// 块下标上——落到别的块上会让客户端拼出一个半截的 JSON。
func TestAnthropicStreamToolCall(t *testing.T) {
	_, frames := streamRequest(t, PathMessages,
		channel.StreamEvent{Kind: "delta", Text: "我查一下"},
		channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
			Index: 0, ID: "toolu_1", Name: "get_weather",
		}},
		channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
			Index: 0, Arguments: `{"city":"北京"}`,
		}},
		channel.StreamEvent{Kind: "done", FinishReason: "tool_use"},
	)

	want := []string{
		"message_start",       // 0
		"content_block_start", // 1 正文块
		"content_block_delta", // 2 正文增量
		"content_block_stop",  // 3 关正文块
		"content_block_start", // 4 工具块
		"content_block_delta", // 5 参数分片
		"content_block_stop",  // 6 关工具块
		"message_delta",       // 7
		"message_stop",        // 8
	}
	if len(frames) != len(want) {
		t.Fatalf("帧数 = %d，期望 %d: %+v", len(frames), len(want), frames)
	}
	for i, name := range want {
		if got := at(t, frames, i).event; got != name {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, got, name)
		}
	}

	tool := at(t, frames, 4).decode(t)
	if got := dig(t, tool, "content_block", "type"); got != "tool_use" {
		t.Errorf("content_block.type = %v，期望 tool_use", got)
	}
	if got := dig(t, tool, "content_block", "name"); got != "get_weather" {
		t.Errorf("name = %v，期望 get_weather", got)
	}
	toolIndex := tool["index"]

	partial := at(t, frames, 5).decode(t)
	if partial["index"] != toolIndex {
		t.Errorf("参数分片的下标 = %v，期望与工具块相同（%v）", partial["index"], toolIndex)
	}
	if got := dig(t, partial, "delta", "type"); got != "input_json_delta" {
		t.Errorf("delta.type = %v，期望 input_json_delta", got)
	}
	if got := dig(t, partial, "delta", "partial_json"); got != `{"city":"北京"}` {
		t.Errorf("partial_json = %v", got)
	}
}

// ── 截断 ──────────────────────────────────────────────────────────────

// 已经吐字之后上游报错：错误作为**流里的事件**表达（响应头早就发出去了），
// 且**不写结束标记**——写了会让半截回答看起来像完整回答。
func TestStreamTruncatedAfterOutput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		wantName string
	}{
		{name: "OpenAI", path: PathChatCompletions, wantName: ""},
		{name: "Anthropic", path: PathMessages, wantName: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 网关在吐字之后失败时会把 route.ErrOutputStarted 包在错误上。
			cause := fmt.Errorf("%w: %w", route.ErrOutputStarted,
				&channel.ErrUpstream{Body: "placeholder upstream failure"})

			_, frames := streamRequest(t, tc.path,
				channel.StreamEvent{Kind: "delta", Text: "半句"},
				channel.StreamEvent{Kind: "error", Err: cause},
			)

			last := frames[len(frames)-1]
			if last.event != tc.wantName {
				t.Errorf("最后一帧的事件名 = %q，期望 %q", last.event, tc.wantName)
			}
			if last.data == "[DONE]" {
				t.Error("截断的流不该写 [DONE]")
			}
			for _, f := range frames {
				if strings.Contains(f.data, "message_stop") {
					t.Error("截断的流不该写 message_stop")
				}
			}

			// 措辞要说「被截断了」，而不是「服务暂时不可用」——后者会让
			// 调用方直接重试，而重试会把两段回答接在一起。
			// 两套协议的错误帧都是 {"error":{"message":…}} 这个形状。
			message, _ := dig(t, last.decode(t), "error", "message").(string)
			if !strings.Contains(message, "interrupted") {
				t.Errorf("错误措辞 = %q，期望说明回答被截断", message)
			}
			// 上游的错误原文不回给调用方：它可能回显请求内容。
			if strings.Contains(last.data, "placeholder upstream failure") {
				t.Errorf("不该回上游错误原文: %s", last.data)
			}
		})
	}
}

// 上游正常结束但没带 usage：帧序列照常，只是没有用量——而不是写一个 0。
func TestAnthropicStreamWithoutUsageOmitsIt(t *testing.T) {
	_, frames := streamRequest(t, PathMessages,
		channel.StreamEvent{Kind: "delta", Text: "好"},
		channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
	)

	end := at(t, frames, len(frames)-2).decode(t)
	if _, ok := end["usage"]; ok {
		t.Errorf("没有实测用量时不该写 usage（写 0 会被当成「这次没消耗」）: %v", end)
	}
}

// ── 认不出的协议 ──────────────────────────────────────────────────────

// 两套协议的帧形状不兼容，所以认不出的协议要报错，而不是回落到其中一套。
func TestNewStreamRendererRejectsUnknownProtocol(t *testing.T) {
	if _, err := newStreamRenderer(channel.Protocol("gemini"), "id", "m", time.Now(), 0); err == nil {
		t.Error("未知协议应当报错，而不是回落到某一套形状")
	}
}
