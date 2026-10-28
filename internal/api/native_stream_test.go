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
				`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m-1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
				`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"m-1","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
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
				`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m-1","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":42,"output_tokens":1}}}`,
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
				`event: response.created` + "\n" + `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"m-1","output":[]}}`,
				`event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","sequence_number":1,"output_index":0,"content_index":0,"delta":"你"}` + "\n" + `data: {"type":"response.output_text.delta","sequence_number":2,"output_index":0,"content_index":0,"delta":"好"}`,
				`event: response.completed` + "\n" + `data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_1","object":"response","status":"completed","model":"m-1","output":[]}}`,
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

// 上游块里的 model 被改写调用方请求的名字，其余键一个不动。
//
// # 为什么这条要单独钉
//
// 上面那些夹具的原生帧里 model 恰好就是请求名（m-1），所以改写是空操作——
// 把 rewriteModelKey 整个短路掉，它们照样全绿。那样一条恒真的路径不能证明
// 改写生效（见 LESSONS 里「钉住改动前后都成立的行为」）。
//
// 这条造一份 model 与请求名**不同**的上游字节，逐字段判两件事：
//   - model 变成了调用方要的那个名字（docs/api.md 的「入站别名」一节承诺
//     流式与非流式一样）；
//   - 其余键与值原样保留（改写只动那一个键，不是重造整帧）。
func TestNativePassthroughRewritesModelInRawFrames(t *testing.T) {
	const upstreamModel = "upstream-real-name"
	// 调用方要的名字由测试处理器固定（见 newTestHandler 的别名/模型名）。
	const wantedModel = "m-1"

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
			frame: `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,` +
				`"model":"` + upstreamModel + `","choices":[{"index":0,"delta":{"content":"你好"},` +
				`"finish_reason":null}]}`,
		},
		{
			name:     "Anthropic",
			path:     PathMessages,
			protocol: channel.ProtocolAnthropic,
			frame: `event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message",` +
				`"role":"assistant","model":"` + upstreamModel + `","content":[],` +
				`"usage":{"input_tokens":42,"output_tokens":1}}}`,
		},
		{
			name:     "Responses",
			path:     PathResponses,
			protocol: channel.ProtocolOpenAIResponses,
			// Responses 把 model 嵌在 response 对象里，与另外两套的顶层位置不同。
			frame: `event: response.created` + "\n" +
				`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1",` +
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
			inner := jsonObjectOf(t, got)
			// model 的落点按协议不同，与 rewriteModelKey 里那三层对应。
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
			// 其余键原样：这条造的是 OpenAI 那块，它至少有 id 与 object。
			if tc.protocol == channel.ProtocolOpenAI {
				if id, _ := inner["id"].(string); id != "chatcmpl-1" {
					t.Errorf("id = %q，期望原样保留的 chatcmpl-1（改写只动 model 一个键）", id)
				}
				if object, _ := inner["object"].(string); object != "chat.completion.chunk" {
					t.Errorf("object = %q，期望原样保留的 chat.completion.chunk", object)
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
