package openaiadapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// reasoningEffortRequest 是一次「关闭推理」的请求：入站把 Anthropic 的
// 未启用 thinking 翻成了 OpenAI 的 none。
func reasoningEffortRequest() *channel.Request {
	return &channel.Request{
		Model:           "gpt-4o",
		ReasoningEffort: "none",
		Messages: []channel.Message{
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}
}

// effortMap 拼一张 {"reasoning_effort_map": 表} 形状的渠道配置。
func effortMap(table map[string]any) map[string]any {
	return map[string]any{reasoningEffortMapKey: table}
}

func TestReasoningEffortDialect(t *testing.T) {
	cases := []struct {
		name    string
		setting map[string]any
		effort  string
		want    string
	}{
		{
			name:   "没有 setting 时原样返回",
			effort: "none",
			want:   "none",
		},
		{
			name:    "配了别的键但没配映射表",
			setting: map[string]any{"supports_optional_param": true},
			effort:  "none",
			want:    "none",
		},
		{
			name:    "表里没有这个取值时原样返回",
			setting: effortMap(map[string]any{"high": "max"}),
			effort:  "none",
			want:    "none",
		},
		{
			name:    "映射成该渠道的方言",
			setting: effortMap(map[string]any{"none": "off"}),
			effort:  "none",
			want:    "off",
		},
		{
			name:    "映射成空串表示剔掉这个字段",
			setting: effortMap(map[string]any{"none": ""}),
			effort:  "none",
			want:    "",
		},
		{
			name:    "请求没带这个参数时不去查表",
			setting: effortMap(map[string]any{"": "off"}),
			effort:  "",
			want:    "",
		},
		{
			name:    "映射表本身不是对象时按没配处理",
			setting: map[string]any{reasoningEffortMapKey: "off"},
			effort:  "none",
			want:    "none",
		},
		{
			name:    "映射表是 null 时按没配处理",
			setting: map[string]any{reasoningEffortMapKey: nil},
			effort:  "none",
			want:    "none",
		},
		{
			name:    "某一项的值不是字符串时跳过该项",
			setting: effortMap(map[string]any{"none": true}),
			effort:  "none",
			want:    "none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasoningEffortDialect(tc.setting, tc.effort); got != tc.want {
				t.Errorf("reasoningEffortDialect(%v, %q) = %q，期望 %q",
					tc.setting, tc.effort, got, tc.want)
			}
		})
	}
}

// TestChatReasoningEffortBody 把三种情况比完整报文，而不是只断言字段在不在。
//
// 要证明「没配的渠道与今天逐字一致」，只能整串比：一个错位的空格、一个
// 多出来的 null 都不会让「字段还在」这条断言变红。
func TestChatReasoningEffortBody(t *testing.T) {
	cases := []struct {
		name    string
		setting map[string]any
		want    string
	}{
		{
			name: "没配映射表时报文不变",
			want: `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`,
		},
		{
			name:    "配了 none 到 off 时发出 off",
			setting: effortMap(map[string]any{"none": "off"}),
			want:    `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"off"}`,
		},
		{
			name:    "配成空串时报文里没有这个字段",
			setting: effortMap(map[string]any{"none": ""}),
			want:    `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				got = string(raw)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"cmpl-1","model":"gpt-4o",`+
					`"choices":[{"index":0,"finish_reason":"stop",`+
					`"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			defer srv.Close()

			ch := testChannel(srv.URL)
			ch.Setting = tc.setting
			if _, err := newAdapter().Chat(context.Background(), ch, reasoningEffortRequest()); err != nil {
				t.Fatalf("出错: %v", err)
			}
			if got != tc.want {
				t.Errorf("出站报文 =\n  %s\n期望\n  %s", got, tc.want)
			}
		})
	}
}

// TestChatStreamReasoningEffortBody 覆盖流式那条编码路径。
//
// 流式与非流式各调一次 encodeRequest，方言翻译写在里面；这条用例要证明
// 流式报文也过了它——只看非流式的话，把翻译挪到某个只走非流式的分支上
// 不会被发现。
func TestChatStreamReasoningEffortBody(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch := testChannel(srv.URL)
	ch.Setting = effortMap(map[string]any{"none": "off"})
	events, err := newAdapter().ChatStream(context.Background(), ch, reasoningEffortRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	for range events {
	}

	const want = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],` +
		`"stream":true,"stream_options":{"include_usage":true},"reasoning_effort":"off"}`
	if got != want {
		t.Errorf("流式出站报文 =\n  %s\n期望\n  %s", got, want)
	}
}
