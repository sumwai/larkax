package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
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
	// type 只在**第一个**分片里给：OpenAI 的 SDK 按它决定把这一段计入哪一类
	// 调用，后续分片再带一次会让客户端以为这是另一路调用。
	if got := dig(t, first, "choices", "0", "delta", "tool_calls", "0", "type"); got != "function" {
		t.Errorf("第一个分片的 type = %v，期望 function", got)
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
	if got := dig(t, second, "choices", "0", "delta", "tool_calls", "0", "type"); got != nil {
		t.Errorf("后续分片不该带 type: %v", got)
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
	// message_start 里的输入量是本地估算：这条流里**没有** usage_input
	// （上游没在流的最前面报输入用量），而第一个事件——那个内容帧——一到，
	// 这一帧就必须写出去，等不了后面那个 usage（见 anthropicStream.writeStart）。
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

// ── 块的括号 ──────────────────────────────────────────────────────────

// 下面这几条验的是**块的括号**：每个 content_block_delta 都必须落在自己那个
// 下标的 content_block_start 与 content_block_stop 之间。
//
// 不写成逐帧比对：这里要钉的是「顺序错了」这一类缺陷，而它的表现随输入而变
// （哪个下标、哪一帧错位），逐帧比对会变成「改一次输入抄一遍帧表」。所以先
// 把帧序列解析成一个结构，再对结构断言。

// blockEvent 是与内容块有关的一帧。
type blockEvent struct {
	kind  string // start / delta / stop
	index int
	// partial 是 input_json_delta 的分片（只有 delta 有）。
	partial string
}

// frameIndex 取帧里的 index（JSON 数字解出来是 float64）。
func frameIndex(t *testing.T, m map[string]any, i int) int {
	t.Helper()

	v, ok := m["index"].(float64)
	if !ok {
		t.Fatalf("第 %d 帧没有数字形态的 index: %v", i, m)
	}
	return int(v)
}

// strAt 逐层取一个字符串字段，取不到就失败（而不是拿到零值继续跑）。
func strAt(t *testing.T, m map[string]any, path ...string) string {
	t.Helper()

	s, ok := dig(t, m, path...).(string)
	if !ok {
		t.Fatalf("取 %v 时不是字符串: %v", path, dig(t, m, path...))
	}
	return s
}

// blockEvents 把帧序列抽成与块有关的帧，与块无关的（message_start 等）跳过。
func blockEvents(t *testing.T, frames []sseFrame) []blockEvent {
	t.Helper()

	var out []blockEvent
	for i, f := range frames {
		switch f.event {
		case "content_block_start":
			out = append(out, blockEvent{kind: "start", index: frameIndex(t, f.decode(t), i)})
		case "content_block_delta":
			m := f.decode(t)
			ev := blockEvent{kind: "delta", index: frameIndex(t, m, i)}
			if dig(t, m, "delta", "type") == "input_json_delta" {
				ev.partial = strAt(t, m, "delta", "partial_json")
			}
			out = append(out, ev)
		case "content_block_stop":
			out = append(out, blockEvent{kind: "stop", index: frameIndex(t, f.decode(t), i)})
		}
	}
	return out
}

// checkBlockBrackets 遍历一次块事件，断言三件事：
//
//   - 每个分片都落在自己那个下标**当前开着**的块里（它的 start 已经写出、
//     stop 还没写出）；
//   - 同一个下标只被打开一次——重开会让客户端在最终消息里多出一个同 id 的
//     空 tool_use 块（两个官方 SDK 的累加器都是「start 往 content 数组里
//     追加、delta 与 stop 按 index 取块」）；
//   - 打开过的块都等到了 stop。
func checkBlockBrackets(t *testing.T, evs []blockEvent) {
	t.Helper()

	open := map[int]bool{}
	starts := map[int]int{}
	for _, ev := range evs {
		switch ev.kind {
		case "start":
			starts[ev.index]++
			if starts[ev.index] > 1 {
				t.Errorf("下标 %d 的块被打开了 %d 次", ev.index, starts[ev.index])
			}
			open[ev.index] = true
		case "delta":
			if !open[ev.index] {
				t.Errorf("分片 %q 落在下标 %d 的块之外（那个块没开着：要么还没 start，要么它的 start 已经配过 stop 了）",
					ev.partial, ev.index)
			}
		case "stop":
			if !open[ev.index] {
				t.Errorf("下标 %d 的块在打开之前（或第二次）被关掉了", ev.index)
			}
			delete(open, ev.index)
		}
	}
	for index := range open {
		t.Errorf("下标 %d 的块一直开着，没等到 stop", index)
	}
}

// assembledToolCall 是一个工具块按流里的分片拼出来的样子。
type assembledToolCall struct {
	index   int
	id      string
	name    string
	partial string
}

// assembleToolCalls 从帧序列里拼出工具块：id/name 来自 start，参数来自落在
// 同一个下标上的 input_json_delta。
func assembleToolCalls(t *testing.T, frames []sseFrame) []assembledToolCall {
	t.Helper()

	var out []assembledToolCall
	pos := map[int]int{} // 块下标 → out 里的位置
	for i, f := range frames {
		switch f.event {
		case "content_block_start":
			m := f.decode(t)
			if dig(t, m, "content_block", "type") != "tool_use" {
				continue
			}
			index := frameIndex(t, m, i)
			pos[index] = len(out)
			out = append(out, assembledToolCall{
				index: index,
				id:    strAt(t, m, "content_block", "id"),
				name:  strAt(t, m, "content_block", "name"),
			})
		case "content_block_delta":
			m := f.decode(t)
			if dig(t, m, "delta", "type") != "input_json_delta" {
				continue
			}
			index := frameIndex(t, m, i)
			at, ok := pos[index]
			if !ok {
				t.Fatalf("下标 %d 的参数分片出现在它的块打开之前", index)
			}
			out[at].partial += strAt(t, m, "delta", "partial_json")
		}
	}
	return out
}

// toolUseEvent 造一次工具调用增量。
func toolUseEvent(index int, id, name, args string) channel.StreamEvent {
	return channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
		Index: index, ID: id, Name: name, Arguments: args,
	}}
}

// 并行的工具调用：上游给的 index 可以**交错**（OpenAI 的 tool_calls[].index
// 不保证一个用完再出现下一个），而 Anthropic 的每个分片都必须落在自己那个
// 下标的 start 与 stop 之间。
//
// 这条用例喂的就是交错序列：两个工具调用的**头**先来（上游常常把它们放在
// 同一个分片里），参数分片随后交替到达。
func TestAnthropicStreamInterleavedToolIndices(t *testing.T) {
	_, frames := streamRequest(t, PathMessages,
		toolUseEvent(0, "toolu_1", "get_weather", ""),
		toolUseEvent(1, "toolu_2", "get_forecast", ""),
		toolUseEvent(0, "", "", `{"city":`),
		toolUseEvent(1, "", "", `{"days":`),
		toolUseEvent(0, "", "", `"北京"}`),
		toolUseEvent(1, "", "", `3}`),
		channel.StreamEvent{Kind: "done", FinishReason: "tool_use"},
	)

	checkBlockBrackets(t, blockEvents(t, frames))

	// 括号对了还不够：分片还要落回**自己那次**工具调用的块里。错位在客户端
	// 那里表现为「拼出半截 JSON」，所以按块把分片拼回去逐块比对。
	got := assembleToolCalls(t, frames)
	want := []struct{ id, name, partial string }{
		{"toolu_1", "get_weather", `{"city":"北京"}`},
		{"toolu_2", "get_forecast", `{"days":3}`},
	}
	if len(got) != len(want) {
		t.Fatalf("工具块数 = %d，期望 %d（每个上游下标一个块）: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].id != w.id || got[i].name != w.name || got[i].partial != w.partial {
			t.Errorf("第 %d 个工具块（下标 %d）= %+v，期望 id=%s name=%s 参数=%s",
				i, got[i].index, got[i], w.id, w.name, w.partial)
		}
	}
}

// 工具调用的两个参数分片之间夹了一段正文。工具块这时**不能**被关掉：关掉
// 它，后面那个分片就落在自己的 stop 之后了，而重开同一个下标会让客户端多出
// 一个同 id 的空块——所以正文块开在工具块之后，两个块同时开着。
func TestAnthropicStreamTextBetweenToolFragments(t *testing.T) {
	_, frames := streamRequest(t, PathMessages,
		toolUseEvent(0, "toolu_1", "get_weather", `{"city":`),
		channel.StreamEvent{Kind: "delta", Text: "查一下"},
		toolUseEvent(0, "", "", `"北京"}`),
		channel.StreamEvent{Kind: "done", FinishReason: "tool_use"},
	)

	checkBlockBrackets(t, blockEvents(t, frames))

	got := assembleToolCalls(t, frames)
	if len(got) != 1 {
		t.Fatalf("工具块数 = %d，期望 1: %+v", len(got), got)
	}
	if got[0].partial != `{"city":"北京"}` {
		t.Errorf("工具参数 = %q，期望两个分片拼成一个完整的 JSON", got[0].partial)
	}

	// 正文块只有一个，且正文分片落在它自己那个下标上。
	textIndex, textDeltaIndex := -1, -1
	for i, f := range frames {
		if f.event != "content_block_start" && f.event != "content_block_delta" {
			continue
		}
		m := f.decode(t)
		switch {
		case f.event == "content_block_start" && dig(t, m, "content_block", "type") == "text":
			if textIndex >= 0 {
				t.Errorf("正文块被打开了两次（下标 %d 与 %d）", textIndex, frameIndex(t, m, i))
			}
			textIndex = frameIndex(t, m, i)
		case f.event == "content_block_delta" && dig(t, m, "delta", "type") == "text_delta":
			textDeltaIndex = frameIndex(t, m, i)
		}
	}
	if textIndex < 0 || textDeltaIndex != textIndex {
		t.Errorf("正文分片的下标 = %d，期望落在正文块 %d 上", textDeltaIndex, textIndex)
	}
}

// 连续的正文分片必须续在**同一个**正文块上。
//
// 判据是「已经开着就不重开」。把 `textIndex >= 0` 写成 `> 0` 之后，下标 0
// 那个正文块永远算「没开」：第二个正文分片会另开一块（下标 1），正文接着往
// 新块里写。客户端按块渲染，看到的不是一段连续的正文，而是两段各写了一半的
// 正文；final message 里也会多出一个正文块。
//
// 夹具只要两个连续正文分片：第一个把块开在下标 0 上，第二个落进判据的另一边。
func TestAnthropicStreamTextDeltasShareOneBlock(t *testing.T) {
	_, frames := streamRequest(t, PathMessages,
		channel.StreamEvent{Kind: "delta", Text: "你"},
		channel.StreamEvent{Kind: "delta", Text: "好"},
		channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
	)

	checkBlockBrackets(t, blockEvents(t, frames))

	starts := 0
	textIndex := -1
	for i, f := range frames {
		switch f.event {
		case "content_block_start":
			m := f.decode(t)
			if dig(t, m, "content_block", "type") != "text" {
				continue
			}
			starts++
			textIndex = frameIndex(t, m, i)
		case "content_block_delta":
			m := f.decode(t)
			if dig(t, m, "delta", "type") != "text_delta" {
				continue
			}
			if got := frameIndex(t, m, i); got != textIndex {
				t.Errorf("第 %d 帧的正文分片落在块 %d 上，正文块是 %d", i, got, textIndex)
			}
		}
	}
	if starts != 1 {
		t.Errorf("正文块被打开了 %d 次，期望 1 次——一段连续的正文只该有一个块", starts)
	}
}

// 工具块开在哪个下标上，参数分片就要落在哪个下标上。
//
// # 为什么这一格在别的夹具里看不出来
//
// `openToolBlock` 的返回值只有「这一片同时带着参数」时才被读到：不带参数时
// 调用方提前返回，参数留给下一片；而下一片走的是 toolBlocks 的**复用**分支，
// 不经过这个返回值。上游把 id、名字与第一段参数放在同一个分片里是常见形状
// （见 TestOpenAIStreamToolCall 的夹具），所以这一条喂的就是它。
//
// 返回一个错的下标时块照样开、参数照样写，只是写在另一个块上：客户端拼出来
// 的是两个 tool_use 块，一个只有名字、一个只有半截参数。
func TestAnthropicStreamToolDeltaStaysOnTheOpenedBlock(t *testing.T) {
	_, frames := streamRequest(t, PathMessages,
		channel.StreamEvent{Kind: "delta", Text: "我查一下"},
		toolUseEvent(0, "toolu_1", "get_weather", `{"city":`),
		channel.StreamEvent{Kind: "done", FinishReason: "tool_use"},
	)

	checkBlockBrackets(t, blockEvents(t, frames))

	toolIndex := -1
	for i, f := range frames {
		if f.event != "content_block_start" {
			continue
		}
		m := f.decode(t)
		if dig(t, m, "content_block", "type") != "tool_use" {
			continue
		}
		toolIndex = frameIndex(t, m, i)
	}
	if toolIndex < 0 {
		t.Fatal("流里没有工具块")
	}
	for i, f := range frames {
		if f.event != "content_block_delta" {
			continue
		}
		m := f.decode(t)
		if dig(t, m, "delta", "type") != "input_json_delta" {
			continue
		}
		if got := frameIndex(t, m, i); got != toolIndex {
			t.Errorf("参数分片落在块 %d 上，工具块开在 %d 上", got, toolIndex)
		}
	}
}

// ── 第一帧的输入用量 ──────────────────────────────────────────────────
// 上游在流的最前面报了输入用量时，message_start 里给的是**真值**，不是估算。
//
// 断言钉在**客户端读到的数**上：帧从 HTTP 响应里按 SSE 解出来，读的就是
// message_start.message.usage.input_tokens 那一个字段——只读第一帧的客户端
// （成本统计、用量面板、限流预算）读到的正是它。「我们内部把某个字段填对了」
// 是另一个维度，那个维度对而客户端读到错数，正是这条缺陷本来的样子。
//
// 真值 12 与估算值（「hi」两个字在未知族估算表上是 1）不同，所以这条断言
// 分辨得出用的是哪一个（见 LESSONS 第 23 条）。
func TestAnthropicStreamStartUsesUpstreamInputTokens(t *testing.T) {
	// 缓存那一份也要跟着走：message_delta 里的 input_tokens 是「输入总量减去
	// 缓存」（见 claudeUsageOf），第一帧走的必须是**同一个**换算——各写一份的
	// 话，两帧会对同一个请求报出两个不同的输入量，而对账的人会去找一个不存在的
	// 差额。42 与 30 就是为这件事选的：减完是 12，与上游报的 input_tokens 同口径。
	early := channel.Usage{PromptTokens: 42, CacheReadTokens: 30}
	final := channel.Usage{PromptTokens: 42, CompletionTokens: 7, CacheReadTokens: 30}
	_, frames := streamRequest(t, PathMessages,
		channel.StreamEvent{Kind: "usage_input", Usage: &early},
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		channel.StreamEvent{Kind: "usage", Usage: &final},
		channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
	)

	start := at(t, frames, 0)
	if start.event != "message_start" {
		t.Fatalf("第 0 帧的事件名 = %q，期望 message_start", start.event)
	}
	startMsg := start.decode(t)
	if got := dig(t, startMsg, "message", "usage", "input_tokens"); got != float64(12) {
		t.Errorf("message_start 的 input_tokens = %v，期望 12（上游报的真值：输入 42 减缓存 30）", got)
	}
	if got := dig(t, startMsg, "message", "usage", "cache_read_input_tokens"); got != float64(30) {
		t.Errorf("message_start 的 cache_read_input_tokens = %v，期望 30", got)
	}

	// 收尾帧里的输入量必须与第一帧一致。
	end := at(t, frames, 4).decode(t)
	if got := dig(t, end, "usage", "input_tokens"); got != float64(12) {
		t.Errorf("message_delta 的 input_tokens = %v，期望 12（与第一帧同一个数）", got)
	}
	if got := dig(t, end, "usage", "output_tokens"); got != float64(7) {
		t.Errorf("message_delta 的 output_tokens = %v，期望 7", got)
	}
}

// 上游只发一个完整用量、而它就在最前面时，同样用真值：判据是「第一个事件里
// 有没有输入用量」，不是「那个事件叫什么」。
func TestAnthropicStreamStartUsesUsageWhenItComesFirst(t *testing.T) {
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	_, frames := streamRequest(t, PathMessages,
		channel.StreamEvent{Kind: "usage", Usage: &usage},
		channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
	)

	if got := dig(t, at(t, frames, 0).decode(t), "message", "usage", "input_tokens"); got != float64(12) {
		t.Errorf("message_start 的 input_tokens = %v，期望 12（第一个事件就是完整用量）", got)
	}
}

// 事件契约说 Usage 只在 usage 与 usage_input 上有效；上游违反它时，第一帧
// 要用本地估算写出去，而不是去解引用一个 nil。
//
// 这条用例针对的是判据的**合取**：「第一个事件带用量」与「它是个报用量的事件」
// 两件事同时成立才用真值。写成析取之后，一个不带用量的 usage_input 事件就落进了
// 「用真值」那一支——`*first.Usage` 当场 panic，这条流连第一帧都出不去。
//
// 输入是**造出来的**：本仓的适配器都会在 usage_input 上带 Usage（见
// channel.StreamEvent 的注释）。造它是为了让那条判据真的被读到——真实输入里
// 两件事总是一起成立，判据写成析取也照样绿。
func TestAnthropicStreamStartFallsBackWithoutUsage(t *testing.T) {
	s := &anthropicStream{id: "id", model: "m", input: 3, textIndex: -1, toolBlocks: map[int]int{}}
	var buf bytes.Buffer

	// panic 也算失败：这一格要的正是不 panic。
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("没有用量的 usage_input 事件让第一帧 panic 了: %v", p)
		}
	}()
	if err := s.writeStart(&buf, channel.StreamEvent{Kind: "usage_input"}); err != nil {
		t.Fatalf("写第一帧失败: %v", err)
	}

	frames := parseFrames(t, buf.String())
	start := at(t, frames, 0)
	if start.event != "message_start" {
		t.Fatalf("第 0 帧的事件名 = %q，期望 message_start", start.event)
	}
	if got := dig(t, start.decode(t), "message", "usage", "input_tokens"); got != float64(3) {
		t.Errorf("input_tokens = %v，期望 3（拿不到真值时用本地估算）", got)
	}
}

// 跨协议那条路（上游是 OpenAI）的流里**没有**早期的输入用量，而第一帧不能
// 因此等到流末尾：等下去整个流就变成「等一会儿一次性返回」，比报一个偏低的
// 估算更坏。所以第一个内容帧一到，就用估算把第一帧写出去。
//
// 这条用例用的是**真的 HTTP 服务端**，不像上面几条那样用 ResponseRecorder：
// recorder 是**整个响应攒完**才交出来的，帧的到达时刻在它上面看不出来——而这里
// 要钉的恰恰是时刻。判据是「流还开着，第一帧已经到了」：事件通道喂完第一个内容
// 事件就挂住（不关），第一帧仍然读得到。
func TestAnthropicStreamFirstFrameArrivesWhileStreamIsOpen(t *testing.T) {
	h, _, chatter := newTestHandler()

	events := make(chan channel.StreamEvent)
	release := make(chan struct{})
	chatter.streamResult = &gateway.Result{Events: events}
	go func() {
		events <- channel.StreamEvent{Kind: "delta", Text: "你好"}
		// 上游还在生成：通道既不关也不再发。第一帧要是被推迟到流末尾，
		// 下面那次读就会读到超时。
		<-release
		close(events)
	}()
	// LIFO：先放行事件通道（处理函数才走得完），再关服务端。
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer close(release)

	// 超时定得短：第一帧被推迟时这里读到的是**超时**，而不是空手挂住——
	// 挂住要等 go test 的整轮超时，报出来的现场不如这一条清楚。
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+PathMessages, strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("发流式请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 读满**第一帧**（读到分隔空行为止）就停：此刻流还开着，这一帧却已经在
	// 客户端手里了。
	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			lines = append(lines, line)
			continue
		}
		break
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读第一帧失败（超时说明第一帧被推迟到流末尾了）: %v", err)
	}
	frames := parseFrames(t, strings.Join(lines, "\n"))
	if len(frames) != 1 {
		t.Fatalf("只读到 %d 帧，期望第一帧 message_start：%+v", len(frames), lines)
	}
	if frames[0].event != "message_start" {
		t.Fatalf("第 0 帧的事件名 = %q，期望 message_start", frames[0].event)
	}
	// 这条路上没有更早的输入用量可给，填的是本地估算——这一条钉的是「没等」，
	// 数值上的真值那条路在 TestAnthropicStreamStartUsesUpstreamInputTokens 里。
	wantInput := float64(tokens.EstimatedTokens("m-1", "hi"))
	if got := dig(t, frames[0].decode(t), "message", "usage", "input_tokens"); got != wantInput {
		t.Errorf("message_start 的 input_tokens = %v，期望 %v（本地估算）", got, wantInput)
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

// 流式请求缺 max_tokens 与非流式是同一个处置：一个字都还没发出去，
// 所以错误按普通 HTTP 响应回，状态码落在 4xx。
//
// 这条单独测一次，是因为流式入口在**开始写帧之后**就换了一套错误表达方式
// （把错误写进流里，见 TestStreamTruncatedAfterOutput）。缺字段发生在更早的
// 一步，不该走到那套表达方式上去——真走到的话，调用方拿到的是一个 200 加
// 一条错误事件，而 4xx 才是他该看到的。
func TestStreamMissingMaxTokensIsClientError(t *testing.T) {
	h, _, chatter := newTestHandler()
	chatter.streamErr = fmt.Errorf("%w: max_tokens 必须为正，当前 %d", billing.ErrNoUpperBound, 0)

	rec := post(h, PathChatCompletions, streamBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应体 %s）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "max_tokens") {
		t.Errorf("错误体没点名缺的是 max_tokens: %s", rec.Body)
	}
}

// ── 认不出的协议 ──────────────────────────────────────────────────────

// 两套协议的帧形状不兼容，所以认不出的协议要报错，而不是回落到其中一套。
func TestNewStreamRendererRejectsUnknownProtocol(t *testing.T) {
	if _, err := newStreamRenderer(channel.Protocol("gemini"), "id", "m", time.Now(), 0); err == nil {
		t.Error("未知协议应当报错，而不是回落到某一套形状")
	}
}
