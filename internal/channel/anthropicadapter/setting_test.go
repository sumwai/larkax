package anthropicadapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// usageSetting 拼一块 {"usage": {"completion_includes_reasoning": v}} 形状的
// 渠道配置。键名写成字面量：它是这条判据的输入，引用常量的话源与用例会一起
// 改，而口径可以悄悄变。
func usageSetting(v bool) map[string]any {
	return map[string]any{"usage": map[string]any{"completion_includes_reasoning": v}}
}

func usageRequest() *channel.Request {
	return &channel.Request{
		Model:     "claude-sonnet-4",
		MaxTokens: 100,
		Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
}

// 推理口径按渠道声明走，非流式与流式两条路都要传下去。
//
// Anthropic 的 output_tokens 本来就含思考 token、协议也不单独报推理量，所以
// 声明缺省时是「已含」。这里验的是读法接通了：没声明时 true，显式写 false 时
// 跟着变——读函数留在 channel 包里而适配器没调用它的话，两种声明会算出同一
// 个数，而那张「声明有没有读者」的表格仍然是绿的。
func TestChatUsageDeclaration(t *testing.T) {
	const body = `{"id":"msg_1","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":18}}`

	cases := []struct {
		name    string
		setting map[string]any
		want    bool
	}{
		{"没声明时按已含", nil, true},
		{"声明 false", usageSetting(false), false},
		{"声明 true", usageSetting(true), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			ch := testChannel(srv.URL)
			ch.Setting = tc.setting
			resp, err := newAdapter().Chat(context.Background(), ch, usageRequest())
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if resp.Usage.CompletionIncludesReasoning != tc.want {
				t.Errorf("CompletionIncludesReasoning = %v，期望 %v",
					resp.Usage.CompletionIncludesReasoning, tc.want)
			}
			// 声明只改这一位：输出照抄上游报的。
			if resp.Usage.CompletionTokens != 18 {
				t.Errorf("CompletionTokens = %d，期望 18", resp.Usage.CompletionTokens)
			}
		})
	}
}

// 流式那一支单独验一遍：它走 pump 里的跨事件累计，与非流式不是同一条路。
func TestChatStreamUsageDeclaration(t *testing.T) {
	const stream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":18}}

event: message_stop
data: {"type":"message_stop"}

`

	for _, declared := range []bool{true, false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream)
		}))

		ch := testChannel(srv.URL)
		ch.Setting = usageSetting(declared)
		events, err := newAdapter().ChatStream(context.Background(), ch, usageRequest())
		if err != nil {
			srv.Close()
			t.Fatalf("出错: %v", err)
		}

		var got *channel.Usage
		for ev := range events {
			if ev.Kind == "usage" {
				u := *ev.Usage
				got = &u
			}
		}
		srv.Close()
		if got == nil {
			t.Fatalf("声明 %v 时没有 usage 事件", declared)
		}
		if got.CompletionIncludesReasoning != declared {
			t.Errorf("声明 %v 时 CompletionIncludesReasoning = %v", declared, got.CompletionIncludesReasoning)
		}
	}
}
