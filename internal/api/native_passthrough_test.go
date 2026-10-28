package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/relay"
)

// 本文件测的是非流式响应的同协议直通：判据成立时按上游原文交出响应体
// （只换顶层的 id 与 model），不成立时走原来的 renderSuccess。
//
// 断言按 JSON 对象比，不比字节：直通那一路的 Raw 是 map，Marshal 出来
// 会重排键序（见 renderNativePassthrough 的注释）。

// nativeCase 是直通用例的一份夹具：一条入口路径、它对应的协议、请求体，
// 以及上游原文。raw 用 JSON 字面量而不是手写 map，于是断言时两边的数字
// 类型（都是 float64）天然一致。
type nativeCase struct {
	name     string
	path     string
	protocol channel.Protocol
	body     string
	raw      string
}

func nativeCases() []nativeCase {
	return []nativeCase{
		{
			name:     "openai",
			path:     PathChatCompletions,
			protocol: channel.ProtocolOpenAI,
			body:     validBody,
			raw: `{
				"id":"upstream-id",
				"object":"chat.completion",
				"created":1700000000,
				"model":"upstream-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":"你好"},
					"finish_reason":"stop"}],
				"usage":{"prompt_tokens":10,"completion_tokens":5},
				"some_upstream_only_field":1
			}`,
		},
		{
			name:     "anthropic",
			path:     PathMessages,
			protocol: channel.ProtocolAnthropic,
			body:     validBody,
			raw: `{
				"id":"upstream-id",
				"type":"message",
				"role":"assistant",
				"model":"upstream-model",
				"content":[{"type":"text","text":"你好"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":5},
				"some_upstream_only_field":1
			}`,
		},
		{
			name:     "openai-responses",
			path:     PathResponses,
			protocol: channel.ProtocolOpenAIResponses,
			body:     responsesBody,
			raw: `{
				"id":"upstream-id",
				"object":"response",
				"created_at":1700000000,
				"status":"completed",
				"model":"upstream-model",
				"output":[{"type":"message","id":"upstream-msg","role":"assistant",
					"content":[{"type":"output_text","text":"你好","annotations":[]}]}],
				"some_upstream_only_field":1
			}`,
		},
	}
}

// decodeObject 把一个 JSON 字面量解成对象，数字是 float64。
func decodeObject(t *testing.T, src string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("夹具不是 JSON 对象: %v\n%s", err, src)
	}
	return m
}

// nativeRequest 发一次请求，并让请求标识是可知的（直通要把顶层 id 换成它）。
func nativeRequest(h *Handler, path, body, requestID string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(observe.HeaderRequestID, requestID)
	rec := httptest.NewRecorder()
	observe.Middleware(h).ServeHTTP(rec, req)
	return rec
}

// 直通判据成立时：响应体是上游 Raw 的逐字段等价，id / model 换成这次请求的
// 那两个，别的键一个都不动——包括枢纽渲染器根本不会产出的那一个。
func TestNativePassthroughRendersUpstreamRaw(t *testing.T) {
	const requestID = "native-req-1"

	for _, c := range nativeCases() {
		t.Run(c.name, func(t *testing.T) {
			h, _, chatter := newTestHandler()
			raw := decodeObject(t, c.raw)
			chatter.result = &gateway.Result{
				Response: &channel.Response{
					UpstreamModel: "upstream-model",
					Message: channel.Message{
						Role:    "assistant",
						Content: []channel.ContentBlock{{Type: "text", Text: "你好"}},
					},
					FinishReason: "stop",
					Usage:        channel.Usage{PromptTokens: 10, CompletionTokens: 5},
					Raw:          raw,
				},
				// 渠道分派给入口协议的就是入口协议自己（见 DispatchProtocol）。
				Channel: channel.Channel{ID: 1, Type: string(c.protocol)},
			}

			rec := nativeRequest(h, c.path, c.body, requestID)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
			}

			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
			}

			if got["id"] != requestID {
				t.Errorf("id = %v，期望本次请求标识 %s", got["id"], requestID)
			}
			// 回显的是调用方要的那个名字，不是上游回报的那个。
			if got["model"] != "m-1" {
				t.Errorf("model = %v，期望 m-1", got["model"])
			}
			// 没有多出来的键，也没有被裁掉的键。
			if len(got) != len(raw) {
				t.Errorf("响应体键数 = %d，期望 %d（夹具里 id 与 model 本来就有）: %v",
					len(got), len(raw), got)
			}
			for k, want := range raw {
				if k == "id" || k == "model" {
					continue
				}
				if !reflect.DeepEqual(got[k], want) {
					t.Errorf("%s = %#v，期望 %#v", k, got[k], want)
				}
			}
			// 单独把这一条写出来：它证明这次没有走 dto 裁剪，而上面的循环
			// 在键整个消失时也会红——两句话说的是同一件事的两面。
			if want := raw["some_upstream_only_field"]; !reflect.DeepEqual(got["some_upstream_only_field"], want) {
				t.Errorf("some_upstream_only_field = %#v，期望原样交出 %#v（直通不该裁字段）",
					got["some_upstream_only_field"], want)
			}

			// 直通只换写给调用方的那一份，不该就地改掉 Raw：响应方向的快照
			// 读的是同一份数据，「上游回报的是什么」不能在渲染这一步走样。
			if raw["id"] != "upstream-id" || raw["model"] != "upstream-model" {
				t.Errorf("渲染改动了 Raw 本身: id = %v，model = %v", raw["id"], raw["model"])
			}
		})
	}
}

// 上游原文缺 id 或 model 时补进去，除此之外不多加键。
func TestNativePassthroughFillsMissingIDAndModel(t *testing.T) {
	const requestID = "native-req-2"

	h, _, chatter := newTestHandler()
	raw := decodeObject(t, `{"object":"chat.completion","some_upstream_only_field":1}`)
	chatter.result = &gateway.Result{
		Response: &channel.Response{
			Message: channel.Message{Role: "assistant", Content: []channel.ContentBlock{{Type: "text", Text: "你好"}}},
			Raw:     raw,
		},
		Channel: channel.Channel{ID: 1, Type: string(channel.ProtocolOpenAI)},
	}

	rec := nativeRequest(h, PathChatCompletions, validBody, requestID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}
	if got["id"] != requestID {
		t.Errorf("id = %v，期望 %s", got["id"], requestID)
	}
	if got["model"] != "m-1" {
		t.Errorf("model = %v，期望 m-1", got["model"])
	}
	if len(got) != len(raw)+2 {
		t.Errorf("响应体键数 = %d，期望 %d: %v", len(got), len(raw)+2, got)
	}
	if got["object"] != "chat.completion" || !reflect.DeepEqual(got["some_upstream_only_field"], float64(1)) {
		t.Errorf("原有键被改动: %v", got)
	}
}

// 判据不成立时走 renderSuccess：形状是渲染器给的那一份，Raw 里的额外键不会
// 漏出来。
func TestNativePassthroughFallsBackToRenderer(t *testing.T) {
	const requestID = "native-req-3"

	// 跨协议渠道：原文在（这是同一条入口解析出来的），但 DispatchProtocol
	// 分派给的是另一套协议，适配器走的不是直通，Raw 是枢纽那份。
	t.Run("跨协议渠道", func(t *testing.T) {
		h, _, chatter := newTestHandler()
		chatter.result = &gateway.Result{
			Response: &channel.Response{
				UpstreamModel: "upstream-model",
				Message: channel.Message{
					Role:    "assistant",
					Content: []channel.ContentBlock{{Type: "text", Text: "你好"}},
				},
				FinishReason: "stop",
				Usage:        channel.Usage{PromptTokens: 10, CompletionTokens: 5},
				Raw:          decodeObject(t, `{"id":"upstream-id","model":"upstream-model","some_upstream_only_field":1}`),
			},
			Channel: channel.Channel{ID: 1, Type: string(channel.ProtocolAnthropic)},
		}

		rec := nativeRequest(h, PathChatCompletions, validBody, requestID)
		assertRenderedOpenAI(t, rec, requestID)
	})

	// 手上没有原文（内部调用、或者挂了 hook 被摘掉）：即便渠道协议对得上，
	// 也没有可以直通的东西。
	t.Run("没有原文", func(t *testing.T) {
		h, _, chatter := newTestHandler()
		chatter.result = &gateway.Result{
			Response: &channel.Response{
				UpstreamModel: "upstream-model",
				Message: channel.Message{
					Role:    "assistant",
					Content: []channel.ContentBlock{{Type: "text", Text: "你好"}},
				},
				FinishReason: "stop",
				Usage:        channel.Usage{PromptTokens: 10, CompletionTokens: 5},
				Raw:          decodeObject(t, `{"id":"upstream-id","model":"upstream-model","some_upstream_only_field":1}`),
			},
			Channel: channel.Channel{ID: 1, Type: string(channel.ProtocolOpenAI)},
		}

		upstream, err := relay.ParseRequest(context.Background(), channel.ProtocolOpenAI, []byte(validBody))
		if err != nil {
			t.Fatalf("解析请求失败: %v", err)
		}
		// 摘掉原文，等价于 serveOnce 里挂了 hook 时做的那一步。
		upstream.NativeBody = nil

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, PathChatCompletions, strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.serveOnce(rec, req, channel.ProtocolOpenAI, upstream, User{ID: 7}, requestID, nil,
			modelNames{requested: "m-1", canonical: "m-1"})

		assertRenderedOpenAI(t, rec, requestID)
	})
}

// assertRenderedOpenAI 断言响应走的是 renderSuccess 的 OpenAI 形状。
func assertRenderedOpenAI(t *testing.T, rec *httptest.ResponseRecorder, requestID string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}
	if got["id"] != requestID {
		t.Errorf("id = %v，期望 %s", got["id"], requestID)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v，期望 chat.completion（渲染器的形状）", got["object"])
	}
	if _, ok := got["choices"]; !ok {
		t.Errorf("渲染器的响应应有 choices: %v", got)
	}
	if _, ok := got["some_upstream_only_field"]; ok {
		t.Errorf("走了渲染器就不该带出 Raw 的额外键: %v", got)
	}
}
