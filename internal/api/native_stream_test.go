package api

// 本文件测的是流式响应的「同协议直通」：调用协议与上游协议一致时，上游
// 那些 SSE 块的原始字节原样交给调用方，网关一个字节都不自己合成。
//
// 断言一律按**字节**比，不走 parseFrames：它只认 `event: ` 与 `data: ` 两种
// 行，遇到 `:` 开头的注释行会判红，而同一个块里的多条 `data:` 只留最后一条
// ——原生块两者都带。解析一遍再比，等于把「上游字节原样转发」这件事测成了
// 「网关能从上游字节里解析出什么」，而后者不是这条契约。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/tokens"
)

// nativeTestRequestID 是测试里固定的请求标识。
//
// 固定它而不是让它随机：非直通那条路上 id 会进每一帧，比对「改动前后逐字节
// 相同」时它必须是个常量。
const nativeTestRequestID = "req-native-test"

// nativeStreamPost 发一次流式请求，事件流由 events 决定。
//
// 与 streamRequest 的差别只有两处：请求标识固定（见上），以及返回原始
// ResponseRecorder——本文件的断言要看**字节**，不看解析出来的帧。
//
// 夹具里那几块的 `id` 写的**就是**这个标识：直通要把它改写成请求标识，两者
// 相同时改写是空操作，于是「逐字节等于上游那几块」这条判据才成立。改写本身
// 由 TestNativePassthroughRewritesIDAndModelInRawFrames 单独钉——那里造的是
// 与请求标识**不同**的上游 id，短路掉改写它会红。
func nativeStreamPost(t *testing.T, path string, events ...channel.StreamEvent) *httptest.ResponseRecorder {
	t.Helper()

	body := streamBody
	if path == PathResponses {
		body = responsesStreamBody
	}

	h, _, chatter := newTestHandler()
	chatter.streamResult = &gateway.Result{Events: streamOf(events...)}

	ctx := observe.WithRequestID(context.Background(), nativeTestRequestID)
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	return rec
}

// sseBody 把若干帧按 SSE 的块分隔符拼成响应体。
//
// 每一帧是「块内全部行用 \n 连接」的那一段，末尾不带 \n——与
// channel.StreamEvent.Frame 的口径相同，所以原生帧的期望值可以直接用它们。
func sseBody(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return b.String()
}

// nativeStreamCase 是一套协议的直通测试夹具。
type nativeStreamCase struct {
	name     string
	path     string
	protocol channel.Protocol
	// nativeFrames 是上游那几个块的原始字节，覆盖了注释行、event: 行与
	// 同块内多条 data: 行这三种形状（见 channel.StreamEvent 的 Frame）。
	nativeFrames []string
	// finishReason 是非直通那条路上 done 事件给的值。
	finishReason string
	// hubBaseline 是**改动前**这条流（delta + usage + done）写出的响应体。
	hubBaseline string
	// balanceErrorFrame 是余额切断在直通流里写出的那一帧。
	balanceErrorFrame string
	// upstreamErrorBlock 是上游自己发的那一块错误原文（三套协议的形状各不
	// 相同）。它**不该**出现在响应里：错误帧的措辞由我们给，不回显上游原文。
	upstreamErrorBlock string
	// errorFrameAfterUpstream 是上游流内错误时我们写出的那一帧。与
	// balanceErrorFrame 的差别只在措辞（它是「上游失败」而不是「余额不足」）。
	errorFrameAfterUpstream string
	// firstEventErrorBody 是**首事件就是 error** 时写出的响应体（见
	// TestStreamErrorAsFirstEventWritesNoPreamble）。
	firstEventErrorBody string
}

func nativeStreamCases() []nativeStreamCase {
	const msg = msgInsufficientBalanceStream
	anthropicInput := strconv.FormatInt(tokens.EstimatedTokens("m-1", "hi"), 10)

	return []nativeStreamCase{
		{
			name:     "OpenAI",
			path:     PathChatCompletions,
			protocol: channel.ProtocolOpenAI,
			nativeFrames: []string{
				// keep-alive：纯注释块，没有归一化事件。
				": keep-alive",
				`data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,"model":"m-1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,"model":"m-1","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
				`data: [DONE]`,
			},
			finishReason: "stop",
			hubBaseline: sseBody(
				`data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,"model":"m-1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
				`data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,"model":"m-1","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
				`data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,"model":"m-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`,
				`data: [DONE]`,
			),
			balanceErrorFrame: `data: {"error":{"message":"` + msg + `","type":"server_error","param":null,"code":null}}`,
			firstEventErrorBody: sseBody(
				`data: {"error":{"message":"upstream call failed","type":"server_error","param":null,"code":null}}`,
			),
			upstreamErrorBlock:      `data: {"error":{"message":"placeholder upstream failure","type":"server_error"}}`,
			errorFrameAfterUpstream: `data: {"error":{"message":"upstream call failed","type":"server_error","param":null,"code":null}}`,
		},
		{
			name:     "Anthropic",
			path:     PathMessages,
			protocol: channel.ProtocolAnthropic,
			nativeFrames: []string{
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"req-native-test","type":"message","role":"assistant","model":"m-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":42,"output_tokens":1}}}`,
				`event: ping` + "\n" + `data: {"type":"ping"}`,
				// 同块内两条 data: 行：SSE 规范按换行拼接，转发时要原样保留。
				`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`,
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
			},
			finishReason: "end_turn",
			hubBaseline: sseBody(
				`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"req-native-test","type":"message","role":"assistant","model":"m-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":`+anthropicInput+`,"output_tokens":0}}}`,
				`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
				`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
				`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"output_tokens":7}}`,
				`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
			),
			balanceErrorFrame: `event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"` + msg + `"}}`,
			firstEventErrorBody: sseBody(
				`event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"upstream call failed"}}`,
			),
			upstreamErrorBlock:      `event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"placeholder upstream failure"}}`,
			errorFrameAfterUpstream: `event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"upstream call failed"}}`,
		},
		{
			name:     "Responses",
			path:     PathResponses,
			protocol: channel.ProtocolOpenAIResponses,
			nativeFrames: []string{
				`event: response.created` + "\n" + `data: {"type":"response.created","sequence_number":0,"response":{"id":"req-native-test","object":"response","status":"in_progress","model":"m-1","output":[]}}`,
				`event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","sequence_number":1,"output_index":0,"content_index":0,"delta":"你"}` + "\n" + `data: {"type":"response.output_text.delta","sequence_number":2,"output_index":0,"content_index":0,"delta":"好"}`,
				`event: response.completed` + "\n" + `data: {"type":"response.completed","sequence_number":3,"response":{"id":"req-native-test","object":"response","status":"completed","model":"m-1","output":[]}}`,
			},
			finishReason: "stop",
			hubBaseline: sseBody(
				`event: response.created`+"\n"+`data: {"type":"response.created","sequence_number":0,"response":{"id":"req-native-test","object":"response","created_at":1700000000,"status":"in_progress","model":"m-1","output":[],"parallel_tool_calls":false,"tool_choice":"auto","tools":[]}}`,
				`event: response.output_item.added`+"\n"+`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item_id":"req-native-test_msg_0","item":{"type":"message","id":"req-native-test_msg_0","status":"in_progress","role":"assistant","content":[]}}`,
				`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","sequence_number":2,"output_index":0,"content_index":0,"item_id":"req-native-test_msg_0","delta":"你好"}`,
				`event: response.output_text.done`+"\n"+`data: {"type":"response.output_text.done","sequence_number":3,"output_index":0,"content_index":0,"item_id":"req-native-test_msg_0","text":"你好"}`,
				`event: response.output_item.done`+"\n"+`data: {"type":"response.output_item.done","sequence_number":4,"output_index":0,"item_id":"req-native-test_msg_0","item":{"type":"message","id":"req-native-test_msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"你好","annotations":[]}]}}`,
				`event: response.completed`+"\n"+`data: {"type":"response.completed","sequence_number":5,"response":{"id":"req-native-test","object":"response","created_at":1700000000,"status":"completed","model":"m-1","output":[{"type":"message","id":"req-native-test_msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"你好","annotations":[]}]}],"usage":{"input_tokens":12,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":19},"parallel_tool_calls":false,"tool_choice":"auto","tools":[]}}`,
			),
			// 4 是接着上游那三帧排的（0..3，见上面的 nativeFrames）：
			// 直通下客户端手里的序号是上游给的，这一帧插在它们后面（见
			// nativeStreamPassthrough.observeSequence）。
			balanceErrorFrame: `event: error` + "\n" + `data: {"type":"error","sequence_number":4,"code":"server_error","message":"` + msg + `"}`,
			firstEventErrorBody: sseBody(
				`event: error` + "\n" + `data: {"type":"error","sequence_number":0,"code":"server_error","message":"upstream call failed"}`,
			),
			upstreamErrorBlock: `event: error` + "\n" + `data: {"type":"error","sequence_number":3,"message":"placeholder upstream failure"}`,
			// 序号接着上游那三帧（0..3）排——错误块自己带的是 3，我们这一帧
			// 排在它之后，所以是 4。上游那一块**不转发**，但它的序号也不该
			// 被我们用（它是上游的编号）。
			errorFrameAfterUpstream: `event: error` + "\n" + `data: {"type":"error","sequence_number":4,"code":"server_error","message":"upstream call failed"}`,
		},
	}
}

// nativeStreamEvents 是直通流的事件序列：每个原生块之后跟着由它解析出的归一化
// 事件（适配器保证这个次序）。
//
// 归一化事件给全了要抑制的那几类（delta / tool_use / usage / done）：
// 少给一类，那一类「不该写」的行为就没被钉住。
func nativeStreamEvents(tc nativeStreamCase) []channel.StreamEvent {
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	events := make([]channel.StreamEvent, 0, len(tc.nativeFrames)*2+3)
	for _, frame := range tc.nativeFrames {
		events = append(events, channel.StreamEvent{
			Kind:         "native",
			NativeFormat: tc.protocol,
			Frame:        []byte(frame),
		})
		events = append(events,
			channel.StreamEvent{Kind: "delta", Text: "你好"},
			channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
				Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{"city":`,
			}},
		)
		if tc.protocol == channel.ProtocolAnthropic {
			// Anthropic 的输入用量在流的最前面就报，归一化事件里对应
			// usage_input；给一条，确认直通时它也不写。
			events = append(events, channel.StreamEvent{Kind: "usage_input", Usage: &usage})
		}
	}
	events = append(events,
		channel.StreamEvent{Kind: "usage", Usage: &usage},
		channel.StreamEvent{Kind: "done", FinishReason: tc.finishReason},
	)
	return events
}

// 直通流：响应体**逐字节等于**上游那几块原样拼起来的样子。
//
// 它同时证明「零合成帧」：多写一帧合成信封、多渲一条归一化事件、多写一个
// 结束标记，这里都会对不上。注释行与同块多条 data: 行也在夹具里——用
// frame{} 写原生块的话，前者会让这一帧多出一个 `data:` 前缀，后者会被拆成
// 两帧。
func TestNativePassthroughForwardsRawFrames(t *testing.T) {
	for _, tc := range nativeStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			rec := nativeStreamPost(t, tc.path, nativeStreamEvents(tc)...)

			want := sseBody(tc.nativeFrames...)
			if got := rec.Body.String(); got != want {
				t.Errorf("响应体与上游字节不一致\n got = %q\nwant = %q", got, want)
			}
		})
	}
}

// 上游块里的 `id` 与 `model` 被改写成我们承诺的值，其余键一个不动。
//
// # 为什么这条要单独钉
//
// 上面那些夹具里这两个键恰好就是我们要的值（`req-native-test` 与 `m-1`），
// 所以改写是空操作——把 rewriteBlockKeys 整个短路掉，它们照样全绿。那样一条
// 恒真的路径不能证明改写生效（见 LESSONS 里「钉住改动前后都成立的行为」）。
//
// 这条造一份两个值都与我们要的**不同**的上游字节，逐字段判三件事：
//   - `id` 变成本次请求的 request_id（与非流式直通、枢纽两条路同一个答案）；
//   - `model` 变成调用方要的那个名字（docs/api.md 的「入站别名」一节承诺
//     流式与非流式一样）；
//   - 其余键与值原样保留（改写只动那两个键，不是重造整帧）。
//
// 第三件里最要紧的是**下层 id 不许动**：Anthropic 的 `content_block.id`
// （`toolu_…`）与 Responses 的 `item_id` 是上游内容自己的标识，客户端靠它们
// 在同一块流里配对，改掉就是在别处破坏。
func TestNativePassthroughRewritesIDAndModelInRawFrames(t *testing.T) {
	const upstreamModel = "upstream-real-name"
	// 调用方要的名字由测试处理器固定（见 newTestHandler 的别名/模型名）。
	const wantedModel = "m-1"
	// 上游报的响应标识，与 nativeTestRequestID 不同——改写才看得出来。
	const upstreamID = "upstream-response-id"

	cases := []struct {
		name     string
		path     string
		protocol channel.Protocol
		frame    string
	}{
		{
			name:     "OpenAI",
			path:     PathChatCompletions,
			protocol: channel.ProtocolOpenAI,
			frame: `data: {"id":"` + upstreamID + `","object":"chat.completion.chunk","created":1700000000,` +
				`"model":"` + upstreamModel + `","choices":[{"index":0,"delta":{"content":"你好"},` +
				`"finish_reason":null}]}`,
		},
		{
			name:     "Anthropic",
			path:     PathMessages,
			protocol: channel.ProtocolAnthropic,
			// content_block.id 是下游的工具调用标识，改它会让客户端配不上对。
			frame: `event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"` + upstreamID + `","type":"message",` +
				`"role":"assistant","model":"` + upstreamModel + `","content":[],` +
				`"usage":{"input_tokens":42,"output_tokens":1}}}`,
		},
		{
			name:     "Responses",
			path:     PathResponses,
			protocol: channel.ProtocolOpenAIResponses,
			// Responses 把 id 与 model 都嵌在 response 对象里，位置与另外两套不同；
			// 而 item_id 在**顶层**，属于上游内容的标识，不许动。
			frame: `event: response.created` + "\n" +
				`data: {"type":"response.created","sequence_number":0,"item_id":"msg_upstream",` +
				`"response":{"id":"` + upstreamID + `",` +
				`"object":"response","status":"in_progress","model":"` + upstreamModel + `",` +
				`"output":[]}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := nativeStreamPost(t, tc.path, channel.StreamEvent{
				Kind:         "native",
				NativeFormat: tc.protocol,
				Frame:        []byte(tc.frame),
			})

			got := rec.Body.String()
			if strings.Contains(got, upstreamModel) {
				t.Errorf("上游的模型名泄漏给了调用方：%q\n"+
					"响应里的 model 必须是调用方请求的那个（见 docs/api.md 的「入站别名」）", got)
			}
			if strings.Contains(got, upstreamID) {
				t.Errorf("上游的响应标识泄漏给了调用方：%q\n"+
					"响应里的 id 必须是本次请求的 request_id（与非流式直通、枢纽两条路一致）", got)
			}
			inner := jsonObjectOf(t, got)
			// 这两个键的落点按协议不同，与 rewriteBlockKeys 里那三层对应。
			switch tc.protocol {
			case channel.ProtocolAnthropic:
				nested, _ := inner["message"].(map[string]any)
				if nested == nil {
					t.Fatalf("message 对象没了：%q", got)
				}
				inner = nested
			case channel.ProtocolOpenAIResponses:
				nested, _ := inner["response"].(map[string]any)
				if nested == nil {
					t.Fatalf("response 对象没了：%q", got)
				}
				inner = nested
			}
			if model, _ := inner["model"].(string); model != wantedModel {
				t.Errorf("model = %q，期望 %q", model, wantedModel)
			}
			if id, _ := inner["id"].(string); id != nativeTestRequestID {
				t.Errorf("id = %q，期望本次请求标识 %q（流式与非流式直通必须给同一个答案）",
					id, nativeTestRequestID)
			}
			// 其余键原样：这条造的是 OpenAI 那块，它至少有 object。
			if tc.protocol == channel.ProtocolOpenAI {
				if object, _ := inner["object"].(string); object != "chat.completion.chunk" {
					t.Errorf("object = %q，期望原样保留的 chat.completion.chunk", object)
				}
			}
			// 下游的标识不许动：改了整个流里客户端就配不上对了。
			if tc.protocol == channel.ProtocolOpenAIResponses {
				if itemID, _ := jsonObjectOf(t, got)["item_id"].(string); itemID != "msg_upstream" {
					t.Errorf("item_id = %q，期望原样保留的 msg_upstream（它是上游内容的标识，不是响应标识）",
						itemID)
				}
			}
		})
	}
}

// jsonObjectOf 取响应体里第一帧的 JSON 载荷。
//
// 直通写的帧就是上游那一块的字节，所以这里按 SSE 的 data: 行取出第一条载荷
// 就够了——本用例每条都只发一帧。
func jsonObjectOf(t *testing.T, body string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &m); err != nil {
			t.Fatalf("响应帧的载荷不是 JSON: %v\n%s", err, body)
		}
		return m
	}
	t.Fatalf("响应体里没有 data: 行：%q", body)
	return nil
}

// OpenAI 的用量分片**不单独转发**，它的 usage 并进结束分片。
//
// # 这是第四类例外，不是直通的副作用
//
// docs/api.md 的「与上游契约的已知偏差」写着：官方在结束分片**之后**另发
// 一个 `choices` 为空的用量分片，而我们把它合并进结束分片——分开写会让按
// `choices[0]` 取值的朴素客户端读到空数组而崩。这一条是**刻意的偏离**，
// 所以直通也必须照做，不能「上游发什么就转什么」。
//
// 判据按 pm 的口径钉**原因**不钉后果：响应里不出现 `choices` 为空的分片。
// 帧数只是它的一个后果，只钉帧数的话，把合并换成「丢掉用量分片」也照样绿。
func TestNativePassthroughMergesUsageIntoFinishBlock(t *testing.T) {
	const (
		finish = `data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,` +
			`"model":"m-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
		usageOnly = `data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,` +
			`"model":"m-1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`
	)

	rec := nativeStreamPost(t, PathChatCompletions,
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolOpenAI, Frame: []byte(finish)},
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolOpenAI, Frame: []byte(usageOnly)},
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolOpenAI, Frame: []byte("data: [DONE]")},
		channel.StreamEvent{Kind: "usage", Usage: &channel.Usage{PromptTokens: 12, CompletionTokens: 7}},
		channel.StreamEvent{Kind: "done", FinishReason: "stop"},
	)

	frames := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n")
	if len(frames) != 2 {
		t.Fatalf("收到 %d 帧，期望 2 帧（结束分片 + [DONE]）：%q", len(frames), rec.Body.String())
	}
	for i, f := range frames {
		if strings.Contains(f, `"choices":[]`) {
			t.Errorf("第 %d 帧的 choices 是空数组：%s\n"+
				"我们承诺把用量合并进结束分片（见 docs/api.md 的已知偏差）", i, f)
		}
	}

	// 用量在结束分片里，且结束分片的其余键原样保留。
	merged := jsonObjectOf(t, frames[0])
	if _, ok := merged["choices"].([]any); !ok {
		t.Errorf("结束分片的 choices 没了：%s", frames[0])
	}
	usage, _ := merged["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("结束分片里没有 usage——用量分片被丢掉了，而不是合并：%s", frames[0])
	}
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(7) ||
		usage["total_tokens"] != float64(19) {
		t.Errorf("合并进来的 usage = %v，期望上游那片报的 12/7/19", usage)
	}
	if object, _ := merged["object"].(string); object != "chat.completion.chunk" {
		t.Errorf("object = %q，期望原样保留（合并只加 usage 一个键）", object)
	}
	if frames[1] != "data: [DONE]" {
		t.Errorf("最后一帧 = %q，期望 [DONE]", frames[1])
	}
}

// 结束分片之后、用量分片之前被切断时，结束分片仍然交出去。
//
// 这是上面那条 flush 放在 error 分支里的判据：上游按「结束分片 → 用量分片」
// 发流，余额在那两步之间用完是一个真实存在的时刻。丢掉攒着的那一块，客户端
// 手里就是一条**有正文、没 finish_reason、也没 [DONE]** 的流——它分不出这是
// 「正常结束但尾部丢了」还是「上游挂了」。
func TestNativePassthroughFlushesFinishBlockBeforeCutoff(t *testing.T) {
	const finish = `data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,` +
		`"model":"m-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

	rec := nativeStreamPost(t, PathChatCompletions,
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolOpenAI, Frame: []byte(finish)},
		channel.StreamEvent{Kind: "error", Err: store.ErrInsufficientAvailable},
	)

	want := sseBody(finish) + sseBody(
		`data: {"error":{"message":"`+msgInsufficientBalanceStream+`","type":"server_error","param":null,"code":null}}`,
	)
	if got := rec.Body.String(); got != want {
		t.Errorf("结束分片被吞掉了\n got = %q\nwant = %q", got, want)
	}
}

// 上游的 message_delta 报了 input_tokens 时原样转发，一个字节都不改。
//
// # 这里曾经钉的是「补写」
//
// 直通曾经在 message_delta 缺这个键时补一个值：先是本地估算，后来改成上游在
// message_start 报过的数。两种补法都错了同一个地方——那个数只进了客户端，
// 结算走的是适配器累计出的 usage 事件，于是同一次请求里客户端读到 14、账上
// 记 0，而两边都标着「实测」。补写的依据是「无论哪条路 message_delta 都是
// 真值」，补了才让那句话成假。
//
// 现在没有补写：上游写什么就是什么。这一条钉「写了就原样到」，下一条钉
// 「没写就没有」——两条合起来才是「逐字节转发」。
func TestNativePassthroughForwardsDeltaInputTokensFromUpstream(t *testing.T) {
	// 上游报 42，与本地估算（按字数估 "hi"）必然不同，所以「原样 42」不会
	// 与「网关自己填了一个数」混淆。
	const (
		start = `event: message_start` + "\n" +
			`data: {"type":"message_start","message":{"id":"req-native-test","type":"message",` +
			`"role":"assistant","model":"m-1","content":[],"usage":{"input_tokens":42,"output_tokens":1}}}`
		delta = `event: message_delta` + "\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
			`"usage":{"input_tokens":42,"output_tokens":7}}`
		stop = `event: message_stop` + "\n" + `data: {"type":"message_stop"}`
	)

	rec := nativeStreamPost(t, PathMessages,
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic, Frame: []byte(start)},
		channel.StreamEvent{Kind: "usage_input", Usage: &channel.Usage{PromptTokens: 42}},
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic, Frame: []byte(delta)},
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic, Frame: []byte(stop)},
		channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
	)

	// 三个块逐字节等于上游原文，中间只隔块分隔符。
	want := sseBody(start) + sseBody(delta) + sseBody(stop)
	if got := rec.Body.String(); got != want {
		t.Errorf("message_delta 没有原样转发\n got = %q\nwant = %q", got, want)
	}
}

// 上游没在 message_delta 里报 input_tokens 时，**那个键就是不存在**——不补估算。
//
// # 为什么不是「补一个至少量级对的数」
//
// 补进去的数只有客户端看得到。结算层拿的是适配器累计出的 usage 事件，那条路上
// 没有这个数（上游确实没报），于是账上记 0。两份数就分叉了，而调用方拿客户端
// 那一份去对账会永远对不上。
//
// # 与枢纽的差别（别把两者写成一条）
//
// 枢纽不拿估算顶上去，但它**也不表现为「键不存在」**：anthropicStream.finish 只在
// `s.usage != nil` 时写整个 usage 对象，一旦写出来，`claudeUsage` 的 input_tokens
// 没有 omitempty，没读到输入量时就是 `0`。所以枢纽上「上游没报」与「上游报了 0」
// 同形、都是 0；而这条路上两者**不同形**：上游报 0 就转一个 0（键在、值是 0），
// 上游没报就没有那个键。那条不对称归 #1284 的 B 节。
//
// 本条用例钉的是后一半（没报 → 键不存在）。「报了 0 → 键在、值是 0」由下面
// 那条零值用例钉着——**不能指望这条或上面那条顺带钉住它**：上面那条喂的是 42，
// 把「值为 0 的 input_tokens 删掉」注入进去，本文件全部用例照样绿（实测过）。
func TestNativePassthroughForwardsDeltaWithoutInputTokens(t *testing.T) {
	// 本地估算确实存在（handler 按 streamBody 估出来的那个），断言它**没有**
	// 出现在响应里——只断言「键不存在」的话，实现改成填 0 也照样绿。
	wantEstimate := strconv.FormatInt(tokens.EstimatedTokens("m-1", "hi"), 10)

	rec := nativeStreamPost(t, PathMessages,
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic,
			Frame: []byte(`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"req-native-test","type":"message",` +
				`"role":"assistant","model":"m-1","content":[],"usage":{"output_tokens":1}}}`)},
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic,
			Frame: []byte(`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
				`"usage":{"output_tokens":7}}`)},
	)

	body := rec.Body.String()
	if strings.Contains(body, `"input_tokens"`) {
		t.Errorf("上游没报输入量，响应里却有 input_tokens：%q", body)
	}
	if strings.Contains(body, `"input_tokens":`+wantEstimate) {
		t.Errorf("本地估算 %s 被补进了响应，而账上没有这个数：%q", wantEstimate, body)
	}
	// 上游报的 output_tokens 照旧原样在。
	if !strings.Contains(body, `"output_tokens":7`) {
		t.Errorf("上游报的 output_tokens 没到调用方：%q", body)
	}
}

// 上游报的 input_tokens 是 0 时，直通**转一个 0 出去**（键在、值是 0），
// 不是把它当成「没报」删掉。
//
// # 为什么要单开一条
//
// 「报了正数就原样到」钉不住这一条：42 那条用例在「值为 0 就删键」的实现上照样绿，
// 而那个实现正是 #1284 A 节要修的那种错读（把真值 0 当成没报）。整段 prompt 命中
// 缓存时上游报的 0 就是真值——缓存量另在 `cache_read_input_tokens` 里——丢掉它
// 等于把一次真实的输入量抹成「不知道」。
//
// 断言是**逐字节等于上游原文**：只断言「响应里有 input_tokens」的话，实现改成把
// 0 改成别的数（比如本地估算）也照样绿。
func TestNativePassthroughForwardsZeroDeltaInputTokens(t *testing.T) {
	const (
		start = `event: message_start` + "\n" +
			`data: {"type":"message_start","message":{"id":"req-native-test","type":"message",` +
			`"role":"assistant","model":"m-1","content":[],"usage":{"input_tokens":0,"output_tokens":0,` +
			`"cache_read_input_tokens":31}}}`
		delta = `event: message_delta` + "\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
			`"usage":{"input_tokens":0,"output_tokens":7}}`
		stop = `event: message_stop` + "\n" + `data: {"type":"message_stop"}`
	)

	rec := nativeStreamPost(t, PathMessages,
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic, Frame: []byte(start)},
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic, Frame: []byte(delta)},
		channel.StreamEvent{Kind: "native", NativeFormat: channel.ProtocolAnthropic, Frame: []byte(stop)},
		channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
	)

	want := sseBody(start) + sseBody(delta) + sseBody(stop)
	if got := rec.Body.String(); got != want {
		t.Errorf("上游报的 0 没有被原样转发\n got = %q\nwant = %q", got, want)
	}
	// 单独再钉一次键在不在：整段比对上一条已经覆盖，但失败时那句话读不出
	// 「是键丢了还是值变了」，这里给出更具体的一条。
	if !strings.Contains(rec.Body.String(), `"input_tokens":0`) {
		t.Errorf("input_tokens 的 0 没到调用方（真值 0 被当成没报？）：%q", rec.Body.String())
	}
}

// 上游没有另发用量分片时，结束分片原样写出——不补一个 usage：替上游编一个
// 用量就是另一种错。
//
// 两条分支各测一次：下一块是别的内容（flush 在那一刻发生），以及流直接结束
// （flush 在 finish 里发生）。少了后者，严格按「结束分片 → [DONE]」发流的
// 上游会把 finish_reason 弄丢，而客户端看到的是一条没有终止原因的正常结束。
func TestNativePassthroughForwardsFinishBlockWithoutUsage(t *testing.T) {
	const (
		finish = `data: {"id":"req-native-test","object":"chat.completion.chunk","created":1700000000,` +
			`"model":"m-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
		done = "data: [DONE]"
	)

	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{name: "下一块是 [DONE]", frames: []string{finish, done}},
		{name: "流到结束分片为止", frames: []string{finish}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []channel.StreamEvent
			for _, f := range tc.frames {
				events = append(events, channel.StreamEvent{
					Kind: "native", NativeFormat: channel.ProtocolOpenAI, Frame: []byte(f),
				})
			}
			events = append(events, channel.StreamEvent{Kind: "done", FinishReason: "stop"})

			rec := nativeStreamPost(t, PathChatCompletions, events...)

			want := sseBody(tc.frames...)
			if got := rec.Body.String(); got != want {
				t.Errorf("响应体\n got = %q\nwant = %q", got, want)
			}
			if strings.Contains(rec.Body.String(), `"usage"`) {
				t.Errorf("上游没报用量，我们却写了一个：%q", rec.Body.String())
			}
		})
	}
}

// 这是有意改变：零事件流本就是失败流（不计费、没有 done、按截断处置），
// 写一帧 role 片 / response.created 会让客户端以为流正常开始了。Anthropic
// 的 start 本来就是空的，另外两套以前会写。
func TestNativePassthroughZeroEventStreamIsEmpty(t *testing.T) {
	for _, tc := range nativeStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			rec := nativeStreamPost(t, tc.path)
			if rec.Body.Len() != 0 {
				t.Errorf("零事件流的响应体长度 = %d，期望 0：%q", rec.Body.Len(), rec.Body)
			}
		})
	}
}

// 格式不匹配的 native 帧被忽略：调用方读不懂另一套协议的块，而它的归一化
// 事件仍然要按自己的协议渲染。
func TestNativePassthroughIgnoresForeignFormat(t *testing.T) {
	for _, tc := range nativeStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			foreign := channel.ProtocolOpenAI
			if tc.protocol == channel.ProtocolOpenAI {
				foreign = channel.ProtocolAnthropic
			}
			usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}

			rec := nativeStreamPost(t, tc.path,
				channel.StreamEvent{
					Kind:         "native",
					NativeFormat: foreign,
					// 这段字节出现在响应体里，就说明它被错当成直通帧转发了。
					Frame: []byte("event: foreign-frame\ndata: {\"foreign\":true}"),
				},
				channel.StreamEvent{Kind: "delta", Text: "你好"},
				channel.StreamEvent{Kind: "usage", Usage: &usage},
				channel.StreamEvent{Kind: "done", FinishReason: tc.finishReason},
			)

			if got := rec.Body.String(); got != tc.hubBaseline {
				t.Errorf("响应体与枢纽渲染不一致\n got = %q\nwant = %q", got, tc.hubBaseline)
			}
			if strings.Contains(rec.Body.String(), "foreign-frame") {
				t.Errorf("外协议的字节被转发了: %q", rec.Body.String())
			}
		})
	}
}

// 上游流内的错误：native 字节已经把那份错误块送到客户端手里了，再写一帧
// 合成的错误就是同一件事说两遍。响应体只剩上游那几块。
// 上游流内的错误块**不转发**，改成我们自己的错误帧。
//
// docs/api.md 的「流中途的错误」一节写着「`message` 里不回上游的错误原文」——
// 上游的错误体可能回显请求内容。所以原生块里认出上游错误（OpenAI
// `{"error":…}` / Anthropic 与 Responses 的 `type:"error"`）就不写它，由随后
// 到达的归一化 error 事件走 inner 的 errorFrame 说我们自己的措辞。
//
// 判据是**逐字节**的：期望体 = 上游那些非错误帧 + 我们的错误帧，既不能多出
// 一块上游原文，也不能少了这一帧终止信号。
func TestNativePassthroughReplacesUpstreamErrorBlock(t *testing.T) {
	for _, tc := range nativeStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			// 把上游的错误块也塞进 native 帧里：真实上游就是先发这块、再发
			// 由它解析出的归一化 error 事件（见 channel.StreamEvent 的 Frame）。
			events := nativeStreamEvents(tc)
			events = append(events, channel.StreamEvent{
				Kind:         "native",
				NativeFormat: tc.protocol,
				Frame:        []byte(tc.upstreamErrorBlock),
			})
			events = append(events, channel.StreamEvent{Kind: "error", Err: errStreamTest{}})

			rec := nativeStreamPost(t, tc.path, events...)

			if strings.Contains(rec.Body.String(), "placeholder upstream failure") {
				t.Errorf("上游错误块被原样转发了：%q\n"+
					"错误帧的措辞必须由我们给，不能回显上游原文（见 docs/api.md）",
					rec.Body.String())
			}
			want := sseBody(tc.nativeFrames...) + sseBody(tc.errorFrameAfterUpstream)
			if got := rec.Body.String(); got != want {
				t.Errorf("错误帧的形状不对\n got = %q\nwant = %q", got, want)
			}
		})
	}
}

// 余额切断是唯一例外：网关自己注入这条错误事件，上游流里没有对应的 native
// 帧。抑制它等于无因截断——客户端只会看到一条没有结束标记的流。
func TestNativePassthroughKeepsBalanceCutoff(t *testing.T) {
	for _, tc := range nativeStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			events := nativeStreamEvents(tc)
			events = append(events, channel.StreamEvent{
				Kind: "error",
				Err:  store.ErrInsufficientAvailable,
			})

			rec := nativeStreamPost(t, tc.path, events...)

			want := sseBody(tc.nativeFrames...) + sseBody(tc.balanceErrorFrame)
			if got := rec.Body.String(); got != want {
				t.Errorf("余额切断的错误帧缺失或位置不对\n got = %q\nwant = %q", got, want)
			}
		})
	}
}

// 非直通流的输出与改动前**逐字节相同**。
//
// 期望值是改动前实测记下来的（固定请求标识后录制）。惰性首帧把首帧从 start
// 挪到了第一个事件，但写出的字节与时机对调用方是同一件事——这条用例就是
// 那句话的判据。
func TestNonNativeStreamBytesUnchanged(t *testing.T) {
	usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	for _, tc := range nativeStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			rec := nativeStreamPost(t, tc.path,
				channel.StreamEvent{Kind: "delta", Text: "你好"},
				channel.StreamEvent{Kind: "usage", Usage: &usage},
				channel.StreamEvent{Kind: "done", FinishReason: tc.finishReason},
			)

			if got := rec.Body.String(); got != tc.hubBaseline {
				t.Errorf("非直通流的输出变了\n got = %q\nwant = %q", got, tc.hubBaseline)
			}
		})
	}
}

// 首事件就是 error 的流不写首帧：先写 message_start / role 片，客户端会以为
// 这条流正常开始了，而它一个字都没有。错误帧本身已经说明这次调用失败了。
//
// # 与「非直通逐字节不变」的关系
//
// 这是**有意改变**：HEAD 的实现里 openAIStream.start 无条件写 role 片、
// anthropicStream 的惰性首帧在 switch 之前且不判 Kind，所以首事件是 error
// 时三套协议都会先写一帧合成信封。惰性首帧把「写首帧」挪到了错误帧前面，
// 那一帧就成了每个错误流的固定前缀——这条用例钉住的是「不写」。
//
// 直通流里这条本来就走 errorFrame、不经过 event 的惰性首帧（见
// nativeStreamPassthrough.event），所以这里测的是非直通流。
func TestStreamErrorAsFirstEventWritesNoPreamble(t *testing.T) {
	for _, tc := range nativeStreamCases() {
		t.Run(tc.name, func(t *testing.T) {
			rec := nativeStreamPost(t, tc.path, channel.StreamEvent{Kind: "error", Err: errStreamTest{}})

			if got := rec.Body.String(); got != tc.firstEventErrorBody {
				t.Errorf("首事件是 error 时的响应体\n got = %q\nwant = %q", got, tc.firstEventErrorBody)
			}
		})
	}
}

// inner 渲染器的 start 不写任何东西：首帧要等到第一个事件。
//
// 包装器（nativeStreamPassthrough）的 start 也是空的，所以经 HTTP 那一路
// 测不到 inner 的惰性——上面三条用例（零事件流、非直通基线、首事件 error）
// 绿的原因在包装器，不在 inner。这一条直接对 inner 断言，两者才都被钉住。
func TestRendererStartIsLazy(t *testing.T) {
	for _, c := range []struct {
		name string
		r    streamRenderer
	}{
		{"openai", &openAIStream{id: "i", model: "m", created: 1}},
		{"responses", &responsesStream{id: "i", model: "m", created: 1, textIndex: -1, tools: map[int]*responsesStreamTool{}}},
		{"anthropic", &anthropicStream{id: "i", model: "m", input: 3, textIndex: -1, toolBlocks: map[int]int{}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := c.r.start(&buf); err != nil {
				t.Fatalf("start: %v", err)
			}
			if buf.Len() != 0 {
				t.Errorf("start 写了 %q，期望什么都不写（首帧要等到第一个事件）", buf.String())
			}
			// 首个非 error 事件到达时，首帧必须写出去——惰性不等于不写。
			var buf2 bytes.Buffer
			if err := c.r.event(&buf2, channel.StreamEvent{Kind: "delta", Text: "x"}); err != nil {
				t.Fatalf("event: %v", err)
			}
			if buf2.Len() == 0 {
				t.Error("首个事件没写出首帧")
			}
		})
	}
}

// 三个 inner 渲染器的首事件是 error 时都不写首帧。
//
// HTTP 层那条（TestStreamErrorAsFirstEventWritesNoPreamble）经过包装器，而
// 包装器的 start 是空的——它挡住了 inner 的 start，所以那一条只能证明
// 「经包装器时有正确输出」，证明不了「inner 自己也是惰性的」。这一条直接
// 对 inner 断言。
func TestRendererFirstEventErrorWritesNoPreamble(t *testing.T) {
	for _, c := range []struct {
		name string
		r    streamRenderer
	}{
		{"openai", &openAIStream{id: "i", model: "m", created: 1}},
		{"responses", &responsesStream{id: "i", model: "m", created: 1, textIndex: -1, tools: map[int]*responsesStreamTool{}}},
		{"anthropic", &anthropicStream{id: "i", model: "m", input: 3, textIndex: -1, toolBlocks: map[int]int{}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := c.r.event(&buf, channel.StreamEvent{Kind: "error", Err: errStreamTest{}}); err != nil {
				t.Fatalf("event: %v", err)
			}
			got := buf.String()
			// 输出里只该有错误帧本身：混进首帧时这几个标记会出现。
			for _, preamble := range []string{"message_start", "response.created", "chat.completion.chunk"} {
				if strings.Contains(got, preamble) {
					t.Errorf("首事件是 error 时写出了首帧（含 %q）: %q", preamble, got)
				}
			}
			if got == "" {
				t.Error("错误帧本身应该写出，响应体却是空的")
			}
		})
	}
}

// 包装器的 errorFrame 不该被调用——它炸掉而不是转发。
//
// 接口上它是个必须实现的方法，而 serveStream 只按 start / event / finish
// 驱动：三个 inner 的 errorFrame 被真正调到（余额切断走它），包装器的那个
// 永远不会。若它转发而不炸，「接口上有这个方法」就会让人以为它是活的，
// 下一份实现照着抄时也跟着写一个假的。这条用例把「假接口成员」钉成红的。
func TestPassthroughErrorFrameIsNotCallable(t *testing.T) {
	p := &nativeStreamPassthrough{
		inner:    &openAIStream{id: "i", model: "m", created: 1},
		protocol: channel.ProtocolOpenAI,
	}
	defer func() {
		if recover() == nil {
			t.Error("包装器的 errorFrame 没有炸：它应当是不可达的断言，不是转发")
		}
	}()
	_ = p.errorFrame(io.Discard, errStreamTest{})
}
