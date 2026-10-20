package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
)

// 本文件测的是 OpenAI Responses 这一条入口：非流式响应体、流式事件序列、
// 工具调用、上游失败时的错误形状，以及协议确实被带进了管道。
//
// 请求体用**真的 Responses 形状**（input / max_output_tokens），不是把
// chat 的请求体换个路径发出去：这两套请求体的差别正是这条入口存在的理由，
// 用 chat 的体会让「解析走错了分支」这件事测不出来。

const responsesBody = `{"model":"m-1","input":"hi","max_output_tokens":16}`

const responsesStreamBody = `{"model":"m-1","input":"hi","max_output_tokens":16,"stream":true}`

// responsesResult 是一条带全部用量细项的助手回复。
//
// 缓存两项与推理量都给了非零值：它们是两个已知偏差的现场（relaykit 的
// 转换器会把推理量写进 chat 名字的那个字段、缓存写入量根本不写），
// 全给 0 的话，漏掉整个 details 对象也能测过。
func responsesResult() *gateway.Result {
	return &gateway.Result{
		Response: &channel.Response{
			UpstreamModel: "m-1-20250101",
			Message: channel.Message{
				Role:    "assistant",
				Content: []channel.ContentBlock{{Type: "text", Text: "你好"}},
			},
			FinishReason: "stop",
			Usage: channel.Usage{
				PromptTokens:     10,
				CompletionTokens: 5,
				CacheReadTokens:  3,
				CacheWriteTokens: 2,
				ReasoningTokens:  1,
			},
		},
	}
}

// responsesStreamRequest 发一次 Responses 的流式请求。
func responsesStreamRequest(t *testing.T, events ...channel.StreamEvent) (*fakeChatter, []sseFrame) {
	t.Helper()

	h, _, chatter := newTestHandler()
	chatter.streamResult = &gateway.Result{Events: streamOf(events...)}

	rec := post(h, PathResponses, responsesStreamBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	return chatter, parseFrames(t, rec.Body.String())
}

// ── 非流式 ────────────────────────────────────────────────────────────

func TestResponsesNonStreamShape(t *testing.T) {
	h, _, chatter := newTestHandler()
	chatter.result = responsesResult()

	rec := post(h, PathResponses, responsesBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}

	if body["object"] != "response" {
		t.Errorf("object = %v，期望 response", body["object"])
	}
	if body["status"] != "completed" {
		t.Errorf("status = %v，期望 completed", body["status"])
	}
	// 报给调用方的是**他要的那个**模型名，不是上游名（见 serveOnce）。
	if body["model"] != "m-1" {
		t.Errorf("model = %v，期望 m-1", body["model"])
	}
	if _, ok := body["incomplete_details"]; ok {
		t.Errorf("正常结束不该有 incomplete_details: %v", body["incomplete_details"])
	}

	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("id 为空：客户端要靠它关联后续事件")
	}
	if got := dig(t, body, "output", "0", "id"); got != id+"_msg_0" {
		t.Errorf("正文项的 id = %v，期望 %s_msg_0", got, id)
	}
	if got := dig(t, body, "output", "0", "type"); got != "message" {
		t.Errorf("output[0].type = %v，期望 message", got)
	}
	if got := dig(t, body, "output", "0", "role"); got != "assistant" {
		t.Errorf("output[0].role = %v，期望 assistant", got)
	}
	if got := dig(t, body, "output", "0", "content", "0", "type"); got != "output_text" {
		t.Errorf("content[0].type = %v，期望 output_text", got)
	}
	if got := dig(t, body, "output", "0", "content", "0", "text"); got != "你好" {
		t.Errorf("content[0].text = %v，期望 你好", got)
	}
	// annotations 必须是数组：官方 SDK 的模型里它是必填列表，null 会让
	// 客户端读到 nil 而不是空列表。
	if got := dig(t, body, "output", "0", "content", "0", "annotations"); got == nil {
		t.Errorf("content[0].annotations 是 null，期望 []")
	}

	// 三个 SDK 必填的常量字段（见 responsesResponse 的注释）。
	if got := body["parallel_tool_calls"]; got != false {
		t.Errorf("parallel_tool_calls = %v，期望 false", got)
	}
	if got := body["tool_choice"]; got != "auto" {
		t.Errorf("tool_choice = %v，期望 auto", got)
	}
	if _, ok := body["tools"].([]any); !ok {
		t.Errorf("tools = %v，期望数组", body["tools"])
	}
}

// TestResponsesUsageDetailsArePopulated 钉住两个已知偏差被绕开了。
//
// 这一条**不能**只断言「有 usage」：relaykit 的
// ChatCompletionsResponseToResponsesResponse 会给出一个带 usage 的响应，
// 只是 output_tokens_details 恒为 nil、input_tokens_details 只在有缓存量
// 时才设。推理量还会被写进 chat 名字的 completion_tokens_details——那个
// 字段在这里根本不该出现。所以断言要落到**具体子字段**上。
func TestResponsesUsageDetailsArePopulated(t *testing.T) {
	h, _, chatter := newTestHandler()
	chatter.result = responsesResult()

	rec := post(h, PathResponses, responsesBody)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}

	if got := dig(t, body, "usage", "input_tokens"); got != float64(10) {
		t.Errorf("input_tokens = %v，期望 10", got)
	}
	if got := dig(t, body, "usage", "output_tokens"); got != float64(5) {
		t.Errorf("output_tokens = %v，期望 5", got)
	}
	if got := dig(t, body, "usage", "total_tokens"); got != float64(15) {
		t.Errorf("total_tokens = %v，期望 15", got)
	}
	if got := dig(t, body, "usage", "output_tokens_details", "reasoning_tokens"); got != float64(1) {
		t.Errorf("output_tokens_details.reasoning_tokens = %v，期望 1"+
			"（读到 <nil> 说明又走回了 relaykit 的转换器）", got)
	}
	if got := dig(t, body, "usage", "input_tokens_details", "cached_tokens"); got != float64(3) {
		t.Errorf("input_tokens_details.cached_tokens = %v，期望 3", got)
	}
	if got := dig(t, body, "usage", "input_tokens_details", "cache_write_tokens"); got != float64(2) {
		t.Errorf("input_tokens_details.cache_write_tokens = %v，期望 2", got)
	}
	// chat 名字的那两个字段一个都不该出现：它们是另一套协议的口径，
	// 出现在这里说明我们漏了宿主内部的字段出去。
	if got := dig(t, body, "usage", "completion_tokens_details"); got != nil {
		t.Errorf("usage.completion_tokens_details = %v，期望不存在（那是 chat 口径）", got)
	}
	if got := dig(t, body, "usage", "prompt_tokens"); got != nil {
		t.Errorf("usage.prompt_tokens = %v，期望不存在（那是 chat 口径）", got)
	}
}

// TestResponsesToolCallOutput 钉住工具调用在 output[] 里的形状。
func TestResponsesToolCallOutput(t *testing.T) {
	h, _, chatter := newTestHandler()
	chatter.result = &gateway.Result{
		Response: &channel.Response{
			UpstreamModel: "m-1-20250101",
			Message: channel.Message{
				Role: "assistant",
				Content: []channel.ContentBlock{
					{Type: "text", Text: "查一下"},
					{Type: "tool_use", ToolUseID: "call_abc", ToolName: "get_weather",
						ToolInput: map[string]any{"city": "上海"}},
				},
			},
			FinishReason: "tool_calls",
			Usage:        channel.Usage{PromptTokens: 10, CompletionTokens: 5},
		},
	}

	rec := post(h, PathResponses, responsesBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}

	// 顺序是「正文项在前、工具项在后」：反了的话按顺序渲染的界面会把
	// 「说一句话然后调用工具」显示成「调用工具然后说话」。
	if got := dig(t, body, "output", "0", "type"); got != "message" {
		t.Errorf("output[0].type = %v，期望 message", got)
	}
	if got := dig(t, body, "output", "1", "type"); got != "function_call" {
		t.Fatalf("output[1].type = %v，期望 function_call", got)
	}
	// 关联 id 叫 call_id，项自己的 id 是另一个东西。
	if got := dig(t, body, "output", "1", "call_id"); got != "call_abc" {
		t.Errorf("call_id = %v，期望 call_abc", got)
	}
	if got := dig(t, body, "output", "1", "name"); got != "get_weather" {
		t.Errorf("name = %v，期望 get_weather", got)
	}
	if got := dig(t, body, "output", "1", "id"); got == "call_abc" {
		t.Errorf("项的 id 与 call_id 是同一个值，它们不是一回事")
	}
	// arguments 是字符串形态的 JSON，与 chat 的 tool_calls 一致。
	args, _ := dig(t, body, "output", "1", "arguments").(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments 不是 JSON 字符串: %v（%q）", err, args)
	}
	if parsed["city"] != "上海" {
		t.Errorf("arguments.city = %v，期望 上海", parsed["city"])
	}
}

// TestResponsesIncompleteStatus 钉住「没答完」这一支。
func TestResponsesIncompleteStatus(t *testing.T) {
	h, _, chatter := newTestHandler()
	chatter.result = responsesResult()
	chatter.result.Response.FinishReason = "length"

	rec := post(h, PathResponses, responsesBody)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}

	if body["status"] != "incomplete" {
		t.Errorf("status = %v，期望 incomplete", body["status"])
	}
	if got := dig(t, body, "incomplete_details", "reason"); got != "max_output_tokens" {
		t.Errorf("incomplete_details.reason = %v，期望 max_output_tokens", got)
	}
	// 项自己的 status 跟着走：客户端按项的 status 判断这一项完不完整。
	if got := dig(t, body, "output", "0", "status"); got != "incomplete" {
		t.Errorf("output[0].status = %v，期望 incomplete", got)
	}
}

// ── 流式 ──────────────────────────────────────────────────────────────

func TestResponsesStreamFrames(t *testing.T) {
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7, ReasoningTokens: 2}
	chatter, frames := responsesStreamRequest(t,
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		channel.StreamEvent{Kind: "delta", Text: "世界"},
		channel.StreamEvent{Kind: "usage", Usage: &usage},
		channel.StreamEvent{Kind: "done", FinishReason: "stop"},
	)

	// 七帧：开场、开正文项、两个增量、正文 done、项 done、终态。
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(want) {
		t.Fatalf("帧数 = %d，期望 %d: %+v", len(frames), len(want), frames)
	}
	for i, name := range want {
		if got := at(t, frames, i).event; got != name {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, got, name)
		}
	}

	// 每一帧都要带 sequence_number，而且从 0 起递增——官方 SDK 把它标成
	// 必填，漏掉或重复会让客户端的顺序判据失效。
	for i := range frames {
		got := at(t, frames, i).decode(t)["sequence_number"]
		if got != float64(i) {
			t.Errorf("第 %d 帧的 sequence_number = %v，期望 %d", i, got, i)
		}
	}

	created := at(t, frames, 0).decode(t)
	if got := dig(t, created, "response", "status"); got != "in_progress" {
		t.Errorf("created 的 status = %v，期望 in_progress", got)
	}
	if got := dig(t, created, "response", "model"); got != "m-1" {
		t.Errorf("created 的 model = %v，期望 m-1", got)
	}
	if got := dig(t, created, "response", "output"); len(got.([]any)) != 0 {
		t.Errorf("created 的 output = %v，期望空数组", got)
	}

	added := at(t, frames, 1).decode(t)
	if got := added["output_index"]; got != float64(0) {
		t.Errorf("正文项的 output_index = %v，期望 0", got)
	}
	if got := dig(t, added, "item", "type"); got != "message" {
		t.Errorf("added 的 item.type = %v，期望 message", got)
	}
	if got := dig(t, added, "item", "status"); got != "in_progress" {
		t.Errorf("added 的 item.status = %v，期望 in_progress", got)
	}

	if got := at(t, frames, 2).decode(t)["delta"]; got != "你好" {
		t.Errorf("第一片增量 = %v，期望 你好", got)
	}
	if got := at(t, frames, 3).decode(t)["delta"]; got != "世界" {
		t.Errorf("第二片增量 = %v，期望 世界", got)
	}

	textDone := at(t, frames, 4).decode(t)
	// 收尾时给的是**拼起来的全文**，不是最后一片。
	if got := textDone["text"]; got != "你好世界" {
		t.Errorf("output_text.done 的 text = %v，期望 你好世界", got)
	}

	itemDone := at(t, frames, 5).decode(t)
	if got := dig(t, itemDone, "item", "status"); got != "completed" {
		t.Errorf("item.done 的 item.status = %v，期望 completed", got)
	}
	if got := dig(t, itemDone, "item", "content", "0", "text"); got != "你好世界" {
		t.Errorf("item.done 的正文 = %v，期望 你好世界", got)
	}

	last := at(t, frames, 6).decode(t)
	if got := dig(t, last, "response", "status"); got != "completed" {
		t.Errorf("终态的 status = %v，期望 completed", got)
	}
	if got := dig(t, last, "response", "usage", "output_tokens_details", "reasoning_tokens"); got != float64(2) {
		t.Errorf("终态的 reasoning_tokens = %v，期望 2", got)
	}
	if got := dig(t, last, "response", "usage", "total_tokens"); got != float64(19) {
		t.Errorf("终态的 total_tokens = %v，期望 19", got)
	}
	if got := dig(t, last, "response", "output", "0", "type"); got != "message" {
		t.Errorf("终态的 output[0].type = %v，期望 message", got)
	}

	// 流式与非流式报的是同一个模型名与同一个协议。
	if chatter.streamReq.Protocol != channel.ProtocolOpenAIResponses {
		t.Errorf("管道收到的协议 = %q，期望 openai-responses", chatter.streamReq.Protocol)
	}
}

// TestResponsesStreamToolFrames 钉住工具调用的帧序列。
func TestResponsesStreamToolFrames(t *testing.T) {
	_, frames := responsesStreamRequest(t,
		channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
			Index: 0, ID: "call_abc", Name: "get_weather", Arguments: `{"city":`}},
		channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
			Index: 0, Arguments: `"上海"}`}},
		channel.StreamEvent{Kind: "done", FinishReason: "tool_calls"},
	)

	want := []string{
		"response.created",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(want) {
		t.Fatalf("帧数 = %d，期望 %d: %+v", len(frames), len(want), frames)
	}
	for i, name := range want {
		if got := at(t, frames, i).event; got != name {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, got, name)
		}
	}

	added := at(t, frames, 1).decode(t)
	// 名字取第一次出现的那一片：后面的分片只有参数。
	if got := dig(t, added, "item", "name"); got != "get_weather" {
		t.Errorf("added 的 item.name = %v，期望 get_weather", got)
	}
	if got := dig(t, added, "item", "call_id"); got != "call_abc" {
		t.Errorf("added 的 item.call_id = %v，期望 call_abc", got)
	}
	if got := dig(t, added, "item", "type"); got != "function_call" {
		t.Errorf("added 的 item.type = %v，期望 function_call", got)
	}

	done := at(t, frames, 4).decode(t)
	if got := done["arguments"]; got != `{"city":"上海"}` {
		t.Errorf("arguments.done 的 arguments = %v，期望拼起来的全文", got)
	}

	last := at(t, frames, 6).decode(t)
	if got := dig(t, last, "response", "output", "0", "type"); got != "function_call" {
		t.Errorf("终态的 output[0].type = %v，期望 function_call", got)
	}
	if got := dig(t, last, "response", "output", "0", "arguments"); got != `{"city":"上海"}` {
		t.Errorf("终态的 arguments = %v", got)
	}
}

// TestResponsesStreamErrorIsFlat 钉住第三套错误形状。
//
// Responses 的流中途失败是一个**平铺**的事件（type / code / message 在
// 顶层），既没有 OpenAI 的 error 外层，也没有 Anthropic 的 error.type。
func TestResponsesStreamErrorIsFlat(t *testing.T) {
	_, frames := responsesStreamRequest(t,
		channel.StreamEvent{Kind: "delta", Text: "半"},
		channel.StreamEvent{Kind: "error", Err: errStreamTest{}},
	)

	last := at(t, frames, len(frames)-1)
	if last.event != "error" {
		t.Fatalf("最后一帧的事件名 = %q，期望 error", last.event)
	}
	m := last.decode(t)
	if m["type"] != "error" {
		t.Errorf("type = %v，期望 error", m["type"])
	}
	if m["code"] != "server_error" {
		t.Errorf("code = %v，期望 server_error", m["code"])
	}
	if _, ok := m["error"]; ok {
		t.Errorf("不该有 error 外层（那是另外两套协议的形状）: %v", m["error"])
	}
	msg, _ := m["message"].(string)
	if msg == "" {
		t.Error("message 为空：调用方只能看到一条没有原因的流")
	}
}

// ── 引用标注 ──────────────────────────────────────────────────────────

// flatCitation 是 url_citation 在 Responses 协议里的形状：字段并排。
//
// 两条路的用例都拿它当期望值——同一个形状只写这一份，免得两边各写各的。
func flatCitation(url string) map[string]any {
	return map[string]any{
		"type": "url_citation", "url": url, "title": "甲",
		"start_index": 0, "end_index": 2,
	}
}

// foldedCitation 是 relaykit 的 Responses→Chat 转换折出来的那一份：
// chat 那套非官方扩展的形状，除 type 以外的字段被挪进了内层
// （见 relayconvert 的 responseAnnotationToChat）。非流式那条路上游送来的就是它。
//
// 它与 flatCitation 各写各的，不从对方推出来：一边推另一边的话，这个用例就
// 变成在测这份夹具自己了。
func foldedCitation(url string) map[string]any {
	return map[string]any{
		"type": "url_citation",
		"url_citation": map[string]any{
			"url": url, "title": "甲", "start_index": 0, "end_index": 2,
		},
	}
}

// assertAnnotation 按字面值断言一条标注：整条对上，不是挑几个字段看。
//
// 两边都先序列化再比。这一步顺带管住三件事：
// 键集合多一个少一个都会让两串不一样；同一份内容每次都是同一串
// （encoding/json 对 map 的键排序）；数字写成 0 还是 float64(0) 也不影响
// （序列化出来都是 0）。
func assertAnnotation(t *testing.T, got any, want map[string]any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("标注序列化失败: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("期望值序列化失败: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("标注 = %s，期望 %s", gotJSON, wantJSON)
	}
}

// soleAnnotation 从一条路的 output[].content[].annotations 里取出唯一的那一条。
func soleAnnotation(t *testing.T, got any) any {
	t.Helper()
	list, ok := got.([]any)
	if !ok {
		t.Fatalf("annotations 不是数组: %v", got)
	}
	if len(list) != 1 {
		t.Fatalf("annotations 有 %d 条，期望 1 条: %v", len(list), list)
	}
	return list[0]
}

func streamAnnotation(url string) channel.StreamEvent {
	return channel.StreamEvent{Kind: "annotation", Annotations: []map[string]any{{
		"type": "url_citation", "url": url, "title": "甲",
		"start_index": float64(0), "end_index": float64(2),
	}}}
}

// 流式下标注真的要送到客户端：一条 response.output_text.annotation.added，
// 而且它原样进收尾事件的 output[].content[].annotations。
//
// 这是 #389 第 2 问的核心判据。非流式那条路一直把这批标注交给调用方，流式这条
// 以前把它们当噪音丢掉——只断言「流跑通了」证明不了它，因为丢掉标注的那条流
// 一样跑得通、一样有正文、一样结算。
func TestResponsesStreamForwardsAnnotations(t *testing.T) {
	// 两条标注的 url 按到达顺序给出：下面既用它造事件，也用它算期望值。
	annotationURLs := []string{"https://example.com/a", "https://example.com/b"}
	_, frames := responsesStreamRequest(t,
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		streamAnnotation(annotationURLs[0]),
		streamAnnotation(annotationURLs[1]),
		channel.StreamEvent{Kind: "done", FinishReason: "stop"},
	)

	var annotationFrames []map[string]any
	for i, f := range frames {
		if f.event != "response.output_text.annotation.added" {
			continue
		}
		m := f.decode(t)
		// output_index / item_id / content_index 必须指向已经打开的那一项：
		// 客户端按 (item, content, annotation_index) 三元组把标注拼回去。
		if got := m["output_index"]; got != float64(0) {
			t.Errorf("第 %d 帧的 output_index = %v，期望 0", i, got)
		}
		if got := m["item_id"]; got != dig(t, at(t, frames, 1).decode(t), "item", "id") {
			t.Errorf("第 %d 帧的 item_id = %v，与 output_item.added 的 %v 不一致",
				i, got, dig(t, at(t, frames, 1).decode(t), "item", "id"))
		}
		if got := m["content_index"]; got != float64(0) {
			t.Errorf("第 %d 帧的 content_index = %v，期望 0", i, got)
		}
		if got := m["annotation_index"]; got != float64(len(annotationFrames)) {
			t.Errorf("第 %d 帧的 annotation_index = %v，期望 %d（从 0 起、连续）",
				i, got, len(annotationFrames))
		}
		anno, ok := m["annotation"].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 帧的 annotation 不是对象: %v", i, m["annotation"])
		}
		// 形状按字面值比，比的是**没被改过**的那一份（反向断言）：
		// 这条路原样转发上游给的扁平标注，不能被顺手改成折过的那一层。
		// 摊平是另一侧的事（见 responsesAnnotation），对已经是扁平形状的
		// 标注是空操作。
		assertAnnotation(t, anno, flatCitation(annotationURLs[len(annotationFrames)]))
		annotationFrames = append(annotationFrames, m)
	}
	if len(annotationFrames) != 2 {
		t.Fatalf("标注事件有 %d 条，期望 2 条", len(annotationFrames))
	}

	// 收尾事件的 output[] 里是同一个顺序、同一批：客户端可能整份替换掉
	// 增量拼出来的那一份，两处不一致时它看到的是「标注变成了另一批」。
	last := frames[len(frames)-1].decode(t)
	got := dig(t, last, "response", "output", "0", "content", "0", "annotations")
	list, ok := got.([]any)
	if !ok {
		t.Fatalf("终态的 annotations 不是数组: %v", got)
	}
	if len(list) != 2 {
		t.Fatalf("终态的 annotations 有 %d 条，期望 2 条: %v", len(list), list)
	}
	// 收尾那一份与增量帧是同一个形状（同一条标注在两处不能有两种读法）。
	for i, url := range annotationURLs {
		obj, ok := list[i].(map[string]any)
		if !ok {
			t.Fatalf("终态第 %d 条标注不是对象: %v", i, list[i])
		}
		assertAnnotation(t, obj, flatCitation(url))
	}
}

// 一条事件里的**一批**标注要全部发出，不是只发第一条。
//
// Annotations 在事件契约里是一批（见 channel.StreamEvent）：一条事件可以带
// 好几条标注，渲染层按顺序每条给一帧、下标从 0 起连续。循环里那次写失败要
// 中止整批——判据取反之后，**第一条写成功就立刻返回**，后面几条从此消失：
// 客户端只看到一半的引用，而收尾事件里的 output[] 仍按整批给出，两处对不上。
func TestResponsesStreamEmitsEveryAnnotationInABatch(t *testing.T) {
	urls := []string{"https://example.com/a", "https://example.com/b"}
	event := channel.StreamEvent{Kind: "annotation"}
	for _, u := range urls {
		event.Annotations = append(event.Annotations, flatCitation(u))
	}

	_, frames := responsesStreamRequest(t,
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		event,
		channel.StreamEvent{Kind: "done", FinishReason: "stop"},
	)

	var got []map[string]any
	for i, f := range frames {
		if f.event != "response.output_text.annotation.added" {
			continue
		}
		m := f.decode(t)
		if idx := m["annotation_index"]; idx != float64(len(got)) {
			t.Errorf("第 %d 帧的 annotation_index = %v，期望 %d（从 0 起、连续）",
				i, idx, len(got))
		}
		got = append(got, m)
	}
	if len(got) != len(urls) {
		t.Fatalf("标注事件有 %d 条，期望 %d 条（一条事件里的整批都要发出）", len(got), len(urls))
	}
	for i, u := range urls {
		anno, ok := got[i]["annotation"].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 条标注不是对象: %v", i, got[i]["annotation"])
		}
		assertAnnotation(t, anno, flatCitation(u))
	}

	// 收尾事件的 output[] 也是整批：增量帧与终态必须是同一批、同一个顺序。
	last := frames[len(frames)-1].decode(t)
	list, ok := dig(t, last, "response", "output", "0", "content", "0", "annotations").([]any)
	if !ok || len(list) != len(urls) {
		t.Fatalf("终态的 annotations = %v，期望 %d 条", list, len(urls))
	}
}

// 标注先于正文到达时也要落到**同一个**正文项上，而不是另开一项。
//
// 上游的事件顺序不保证（标注可能先于它还解释的那段正文）。另开一项会让客户端
// 看到两条 message，而其中一条只有标注没有正文。
func TestResponsesStreamAnnotationBeforeTextKeepsOneItem(t *testing.T) {
	_, frames := responsesStreamRequest(t,
		streamAnnotation("https://example.com/a"),
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		channel.StreamEvent{Kind: "done", FinishReason: "stop"},
	)

	added := 0
	for _, f := range frames {
		if f.event == "response.output_item.added" {
			added++
		}
	}
	if added != 1 {
		t.Errorf("output_item.added 有 %d 帧，期望 1 帧（标注与正文是同一项）", added)
	}
	if frames[2].event != "response.output_text.annotation.added" {
		t.Errorf("第 2 帧 = %q，期望标注事件在正文项打开之后", frames[2].event)
	}
	// annotation 事件的 output_index 与后面正文增量的一致。
	if got := frames[2].decode(t)["output_index"]; got != float64(0) {
		t.Errorf("标注的 output_index = %v，期望 0", got)
	}
	for _, f := range frames {
		if f.event == "response.output_text.delta" {
			if got := f.decode(t)["output_index"]; got != float64(0) {
				t.Errorf("正文增量的 output_index = %v，期望 0（与标注同一项）", got)
			}
		}
	}
}

// 非流式那条路的标注同样要落到客户端，而且交出去的是扁平那一份——
// Responses 协议本身定义的那个形状，不是转换层折出来的中间产物。
//
// 这一条以前不成立：relaykit 的 dto.Message.Annotations 在
// messageFromRelaykit 里被丢掉了。
//
// 送进来的这一份是这条链路上真实的形状：relaykit 的 Responses→Chat 转换会把
// 上游的 url_citation 折成两层（见 foldedCitation，以及 relay 那条链上的
// TestResponseAnnotationsReachContentBlock）。
// 到 internal/api 时它已经折过了。协议对客户端承诺的是扁平那一份，同一个形状
// 流式那条路也给（见下面 TestResponsesAnnotationShapeIsSameOnBothPaths）。
func TestResponsesNonStreamForwardsAnnotations(t *testing.T) {
	h, _, chatter := newTestHandler()
	res := responsesResult()
	res.Response.Message.Content = []channel.ContentBlock{{
		Type: "text", Text: "你好",
		Annotations: []map[string]any{foldedCitation("https://example.com/a")},
	}}
	chatter.result = res

	rec := post(h, PathResponses, responsesBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}
	got := soleAnnotation(t, dig(t, body, "output", "0", "content", "0", "annotations"))
	assertAnnotation(t, got, flatCitation("https://example.com/a"))
}

// 后一块正文没有标注时，不能把前面那一块的标注冲掉。
//
// 判据是「这一块带标注才取它」。写成 `>= 0` 之后恒真，于是**最后一块**正文
// 说了算：它没有标注时就把已经取到的标注换成空。多段正文是上游允许的
// （relay.attachAnnotations 只往其中一块上挂标注），响应体上看到的是标注整批
// 消失，而正文一个字都没变。
func TestResponsesKeepsAnnotationsWhenLaterTextHasNone(t *testing.T) {
	msg := channel.Message{
		Role: "assistant",
		Content: []channel.ContentBlock{
			{
				Type: "text", Text: "前半",
				Annotations: []map[string]any{foldedCitation("https://example.com/a")},
			},
			{Type: "text", Text: "后半"},
		},
	}

	out := responsesOutput("id", "completed", msg)
	if len(out) != 1 {
		t.Fatalf("output 项数 = %d，期望 1（正文项）: %+v", len(out), out)
	}
	item, ok := out[0].(responsesMessageItem)
	if !ok {
		t.Fatalf("output[0] 不是正文项: %T", out[0])
	}
	if len(item.Content) != 1 {
		t.Fatalf("正文项的 content 有 %d 段，期望 1: %+v", len(item.Content), item.Content)
	}
	if got := item.Content[0].Text; got != "前半后半" {
		t.Errorf("正文 = %q，期望两段拼起来", got)
	}
	annos := item.Content[0].Annotations
	if len(annos) != 1 {
		t.Fatalf("标注有 %d 条，期望 1 条（后一段正文没有标注，不该把前一段的冲掉）: %+v",
			len(annos), annos)
	}
	assertAnnotation(t, annos[0], flatCitation("https://example.com/a"))
}

// 空回答也要有一个正文项：output 是必填列表。
//
// 判据是「有正文，或者根本没有工具项」。把 `len(calls) == 0` 取反之后，空回答
// （正文空、也没有工具调用）落进 false 那一支：output 是空数组，客户端拿到的
// 是一份没有 output 的回答——而「模型答了个空串」与「模型什么都没说」在调用方
// 那里本该是同一件事。
func TestResponsesEmptyAnswerStillHasMessageItem(t *testing.T) {
	out := responsesOutput("id", "completed", channel.Message{Role: "assistant"})
	if len(out) != 1 {
		t.Fatalf("output 项数 = %d，期望 1（空回答也要一个正文项）: %+v", len(out), out)
	}
	item, ok := out[0].(responsesMessageItem)
	if !ok || item.Type != "message" {
		t.Fatalf("output[0] = %+v，期望一个 message 项", out[0])
	}
	if len(item.Content) != 1 || item.Content[0].Text != "" {
		t.Errorf("正文项 = %+v，期望一段空正文", item.Content)
	}
	// annotations 是 [] 而不是 null：SDK 的模型里它是必填列表。
	if item.Content[0].Annotations == nil {
		t.Error("annotations 是 nil——序列化后是 null，而协议里它是必填数组")
	}
}

// 内层为空（一条引用只有类型、没有字段）时也照常摊平，结果是只带 type 的对象。
//
// 这一格同时是 TestResponsesAnnotationShapeIsSameOnBothPaths 注释里那条等价
// 论证的实证：内层为空时，`len(inner)-1` 算出 -1，而 `make(map[string]any, -1)`
// 在 Go 里是合法的（与切片不同，切片的负数长度会当场 panic）。
func TestResponsesAnnotationWithEmptyInner(t *testing.T) {
	got := responsesAnnotation(map[string]any{
		"type":         "url_citation",
		"url_citation": map[string]any{},
	})
	assertAnnotation(t, got, map[string]any{"type": "url_citation"})
}

// TestResponsesAnnotationShapeIsSameOnBothPaths 是 #784 的判据：两条路交出的
// 引用标注是**同一个形状**。
//
// 两条路各写一条用例、各断言各的，正是这个差异能活到今天的原因：
// 两边单看都对得上，合起来才不一致。所以这里两条路各断言一次，而且期望值是
// 同一份字面值（见 flatCitation）：改了一边而没改另一边，这个用例就红。
//
// 两边送进去的输入不同，是它们在链路上的位置不同：
// 非流式那一侧已经被 relaykit 折过（foldedCitation），
// 流式那一侧转发的还是上游给的扁平原样。
//
// # 容量提示上的变异不改变任何东西
//
// responsesAnnotation 里 `make(map[string]any, len(inner)+1)` 的 `+1` 被改成
// `-1` 时，两版交出的内容是同一份：容量提示只影响分配，不进 map、也不改变
// 写进去的键值。所以**没有输入能区分这两版**，它是等价变异而不是缺口。
// 前提是 Go 对负的容量提示不 panic——实测 `n := 0; make(map[string]any, n-1)`
// 正常返回一个空 map；这一点与切片不同，切片的负数长度会当场 panic。
func TestResponsesAnnotationShapeIsSameOnBothPaths(t *testing.T) {
	h, _, chatter := newTestHandler()

	// 非流式。
	res := responsesResult()
	res.Response.Message.Content = []channel.ContentBlock{{
		Type: "text", Text: "你好",
		Annotations: []map[string]any{foldedCitation("https://example.com/a")},
	}}
	chatter.result = res

	rec := post(h, PathResponses, responsesBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("非流式状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}
	nonStream := soleAnnotation(t, dig(t, body, "output", "0", "content", "0", "annotations"))
	assertAnnotation(t, nonStream, flatCitation("https://example.com/a"))

	// 流式：同一份字面值，另一条路。
	chatter.streamResult = &gateway.Result{Events: streamOf(
		channel.StreamEvent{Kind: "delta", Text: "你好"},
		streamAnnotation("https://example.com/a"),
		channel.StreamEvent{Kind: "done", FinishReason: "stop"},
	)}
	rec = post(h, PathResponses, responsesStreamBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("流式状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	frames := parseFrames(t, rec.Body.String())
	last := frames[len(frames)-1].decode(t)
	streamed := soleAnnotation(t, dig(t, last, "response", "output", "0", "content", "0", "annotations"))
	assertAnnotation(t, streamed, flatCitation("https://example.com/a"))
}

// 只有 url_citation 会被摊平：其余类型没有折过，客户端拿到的就是上游那一份。
//
// 上游的标注类型不止 url_citation（Responses 协议里还有 file_citation 这些）。
// 别的类型我们没有折过，多认一种就等于替上游解释它的字段，
// 而标注的字段集归上游决定。两条路的行为一样，摊平都在共用的
// responsesAnnotation 里。
//
// 下面这个输入是**造出来的**，上游不会这么给：真实的 file_citation 没有
// url_citation 这一层。带上它，是为了让「type 必须是 url_citation」那一条判据
// 真的被读到——只删掉那一行判据，内层就会被摊出来（type 变成 url_citation），
// 这个用例随即变红；如果输入里没有这一层，删掉判据它照样绿。
func TestResponsesOtherAnnotationTypesPassThrough(t *testing.T) {
	h, _, chatter := newTestHandler()
	res := responsesResult()
	res.Response.Message.Content = []channel.ContentBlock{{
		Type: "text", Text: "你好",
		Annotations: []map[string]any{{
			"type": "file_citation",
			"url_citation": map[string]any{
				"file_id": "file-1", "filename": "a.txt", "index": 3,
			},
		}},
	}}
	chatter.result = res

	rec := post(h, PathResponses, responsesBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}
	got := soleAnnotation(t, dig(t, body, "output", "0", "content", "0", "annotations"))
	assertAnnotation(t, got, map[string]any{
		"type": "file_citation",
		"url_citation": map[string]any{
			"file_id": "file-1", "filename": "a.txt", "index": 3,
		},
	})
}

// ── 协议与请求方向 ────────────────────────────────────────────────────

func TestResponsesProtocolReachesPipeline(t *testing.T) {
	h, _, chatter := newTestHandler()
	rec := post(h, PathResponses, responsesBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	// 协议是**入口**协议，由路径决定。它决定账本里那一列、插件看到的
	// protocol、以及错误体的形状，所以它不能由别的层推。
	if chatter.req.Protocol != channel.ProtocolOpenAIResponses {
		t.Errorf("管道收到的协议 = %q，期望 openai-responses", chatter.req.Protocol)
	}
	if chatter.upstream.Stream {
		t.Error("非流式请求被当成流式发了出去")
	}
}

// TestResponsesRequestFieldsSurvive 钉住请求方向上几处容易静默丢掉的字段。
func TestResponsesRequestFieldsSurvive(t *testing.T) {
	h, _, chatter := newTestHandler()
	body := `{"model":"m-1","instructions":"你是助手","input":"hi",` +
		`"max_output_tokens":32,"tools":[{"type":"function","name":"get_weather",` +
		`"description":"查天气","parameters":{"type":"object"}}]}`

	rec := post(h, PathResponses, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}

	// instructions 变成 system 消息，input 变成 user 消息。
	if len(chatter.upstream.Messages) != 2 {
		t.Fatalf("消息数 = %d，期望 2: %+v", len(chatter.upstream.Messages), chatter.upstream.Messages)
	}
	if got := chatter.upstream.Messages[0].Role; got != "system" {
		t.Errorf("第一条消息的角色 = %q，期望 system（instructions）", got)
	}
	if got := chatter.upstream.Messages[1].Role; got != "user" {
		t.Errorf("第二条消息的角色 = %q，期望 user（input）", got)
	}

	// max_output_tokens 必须变成输出上界：预留要有上界才做得出来，
	// 漏掉它每个请求都会以「max_tokens is required」被拒。
	if chatter.req.MaxTokens != 32 {
		t.Errorf("计费请求的 MaxTokens = %d，期望 32", chatter.req.MaxTokens)
	}
	if chatter.upstream.MaxTokens != 32 {
		t.Errorf("上游请求的 MaxTokens = %d，期望 32", chatter.upstream.MaxTokens)
	}

	if len(chatter.upstream.Tools) != 1 {
		t.Fatalf("工具数 = %d，期望 1", len(chatter.upstream.Tools))
	}
	if got := chatter.upstream.Tools[0].Name; got != "get_weather" {
		t.Errorf("工具名 = %q，期望 get_weather", got)
	}
}

// TestResponsesRejectsNonFunctionTools 钉住「远端 MCP 在入口就被拒」。
//
// 不拦的话这些声明会被**静默丢掉**：relaykit 的转换先经 toolconv 抽出工具、
// 再按 chat 格式挂回去，表达不了的类型直接消失，而请求照常成功。调用方拿到
// 200、工具一次都不会被调用，响应上看不出任何异常。上游适配器那一层照不到
// ——channel.Tool 没有承载类型的字段，到那里已经看不出原始声明是什么。
func TestResponsesRejectsNonFunctionTools(t *testing.T) {
	for _, toolType := range []string{"mcp", "web_search", "file_search"} {
		t.Run(toolType, func(t *testing.T) {
			h, _, chatter := newTestHandler()
			body := `{"model":"m-1","input":"hi","max_output_tokens":16,` +
				`"tools":[{"type":"` + toolType + `","server_label":"x"}]}`

			rec := post(h, PathResponses, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400（响应体 %s）", rec.Code, rec.Body)
			}
			if chatter.calls != 0 {
				t.Errorf("上游被调了 %d 次，期望 0：请求不该带着一个被改写的工具发出去", chatter.calls)
			}
		})
	}
}

// TestResponsesRejectsPreviousResponseID 钉住那条明确不支持的字段。
//
// 它一旦被静默接受，输入就不只是本地这段 input 了——上游还持有一段历史，
// 而我们的输入量估算看不见它，预留会偏小（见 internal/tokens）。
func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	h, _, chatter := newTestHandler()
	rec := post(h, PathResponses,
		`{"model":"m-1","input":"hi","max_output_tokens":16,"previous_response_id":"resp_1"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应体 %s）", rec.Code, rec.Body)
	}
	if chatter.calls != 0 {
		t.Errorf("上游被调了 %d 次，期望 0", chatter.calls)
	}
}

// ── 错误体 ────────────────────────────────────────────────────────────

// TestResponsesErrorBodyMatchesOpenAI 钉住非流式错误体的形状。
//
// 它与 OpenAI 那一套逐字相同（两家共用同一个官方 SDK 的错误解析器），
// 所以这里断言的是「与 OpenAI 入口给出的一模一样」，而不是各写一份期望值
// ——两份期望值会在改一处时漏掉另一处。
func TestResponsesErrorBodyMatchesOpenAI(t *testing.T) {
	h, _, _ := newTestHandler()

	responses := post(h, PathResponses, `{"model":"m-1"`)
	openai := post(h, PathChatCompletions, `{"model":"m-1"`)

	if responses.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应体 %s）", responses.Code, responses.Body)
	}
	if responses.Body.String() != openai.Body.String() {
		t.Errorf("错误体不同：\nResponses: %s\nOpenAI:    %s", responses.Body, openai.Body)
	}

	var body map[string]any
	if err := json.Unmarshal(responses.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 JSON: %v\n%s", err, responses.Body)
	}
	if got := dig(t, body, "error", "type"); got != "invalid_request_error" {
		t.Errorf("error.type = %v，期望 invalid_request_error", got)
	}
	// 解析失败的细节不回给调用方（里面会有 relaykit 的类型名）。
	if got := dig(t, body, "error", "message"); got != "invalid request body" {
		t.Errorf("error.message = %v，期望 invalid request body", got)
	}
}

// TestResponsesUnknownPathUnchanged 确认多了一条路径没动到未知路径那一支。
func TestResponsesUnknownPathUnchanged(t *testing.T) {
	h, _, _ := newTestHandler()
	rec := post(h, "/v1/responses/extra", validBody)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("状态码 = %d，期望 501", rec.Code)
	}
}

// errStreamTest 是流中途失败用的桩错误。
type errStreamTest struct{}

func (errStreamTest) Error() string { return "上游连接断了" }
