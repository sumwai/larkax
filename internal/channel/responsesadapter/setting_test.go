package responsesadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/channel"
)

// reasoningEffortRequest 是一次「关闭推理」的请求：入站把 Anthropic 的
// 未启用 thinking 翻成了 OpenAI 的 none。
func reasoningEffortRequest() *channel.Request {
	return &channel.Request{
		Model:           "up-model",
		ReasoningEffort: "none",
		Messages:        textRequest().Messages,
	}
}

// effortMap 拼一张 {"reasoning_effort_map": 表} 形状的渠道配置。
//
// 键名写成字面量：这里要钉的是「配了这张表时出站报文长什么样」，
// 而键名正是那条判据的输入。
func effortMap(table map[string]any) map[string]any {
	return map[string]any{"reasoning_effort_map": table}
}

// TestChatReasoningEffortBody 把每种情况比完整报文，而不是只断言字段在不在。
//
// 要证明「没配映射表的渠道与没有这张表时逐字一致」，只能整串比：一个错位的
// 空格、一个多出来的 null 都不会让「字段还在」这条断言变红。
func TestChatReasoningEffortBody(t *testing.T) {
	cases := []struct {
		name    string
		setting map[string]any
		req     *channel.Request
		want    string
	}{
		{
			name: "没配映射表时报文不变",
			req:  reasoningEffortRequest(),
			want: `{"model":"up-model","input":[{"content":"hi","role":"user"}],"reasoning":{"effort":"none"}}`,
		},
		{
			name:    "配了 none 到 off 时发出 off",
			setting: effortMap(map[string]any{"none": "off"}),
			req:     reasoningEffortRequest(),
			want:    `{"model":"up-model","input":[{"content":"hi","role":"user"}],"reasoning":{"effort":"off"}}`,
		},
		{
			name:    "配成空串时 effort 与整个 reasoning 都不出现",
			setting: effortMap(map[string]any{"none": ""}),
			req:     reasoningEffortRequest(),
			want:    `{"model":"up-model","input":[{"content":"hi","role":"user"}]}`,
		},
		{
			// relaykit 对 high 会补 summary:"detailed"；effort 被映射掉之后
			// reasoning 里还剩这一项，整个对象必须留着。判据写成「effort 是
			// 不是空」时这条会红——那种写法会把 summary 一起丢掉。
			name:    "effort 映射到空串时 reasoning 里的其它内容要留下",
			setting: effortMap(map[string]any{"high": ""}),
			req: &channel.Request{
				Model:           "up-model",
				ReasoningEffort: "high",
				Messages:        textRequest().Messages,
			},
			want: `{"model":"up-model","input":[{"content":"hi","role":"user"}],"reasoning":{"summary":"detailed"}}`,
		},
		{
			name:    "表里没有这个取值时原样透传",
			setting: effortMap(map[string]any{"high": "max"}),
			req:     reasoningEffortRequest(),
			want:    `{"model":"up-model","input":[{"content":"hi","role":"user"}],"reasoning":{"effort":"none"}}`,
		},
		{
			name:    "请求没带这个参数时不查表",
			setting: effortMap(map[string]any{"": "off"}),
			req:     textRequest(),
			want:    `{"model":"up-model","input":[{"content":"hi","role":"user"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, gotBody, _ := captureServer(t, http.StatusOK,
				`{"id":"r1","model":"up-model","status":"completed","output":[]}`)

			ch := testChannel(srv.URL)
			ch.Setting = tc.setting
			if _, err := newAdapter().Chat(context.Background(), ch, tc.req); err != nil {
				t.Fatalf("出错: %v", err)
			}
			if *gotBody != tc.want {
				t.Errorf("出站报文 =\n  %s\n期望\n  %s", *gotBody, tc.want)
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
	srv, gotBody, _ := captureServer(t, http.StatusOK, "data: [DONE]\n\n")

	ch := testChannel(srv.URL)
	ch.Setting = effortMap(map[string]any{"none": "off"})
	events, err := newAdapter().ChatStream(context.Background(), ch, reasoningEffortRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	for range events {
	}

	const want = `{"model":"up-model","input":[{"content":"hi","role":"user"}],` +
		`"reasoning":{"effort":"off"},"stream":true}`
	if *gotBody != want {
		t.Errorf("流式出站报文 =\n  %s\n期望\n  %s", *gotBody, want)
	}
}

// TestReasoningHasNothing 钉住「reasoning 里还有没有内容」这条判据。
//
// 走适配器的那条路今天只会生成带 effort 的对象，所以「还带着 summary 时
// 不该整块删掉」只能在这里直接比。
func TestReasoningHasNothing(t *testing.T) {
	cases := []struct {
		name string
		r    *dto.Reasoning
		want bool
	}{
		{name: "空对象", r: &dto.Reasoning{}, want: true},
		{name: "还带着 summary", r: &dto.Reasoning{Summary: "auto"}, want: false},
		{name: "mode 是 null 时算没有内容", r: &dto.Reasoning{Mode: json.RawMessage("null")}, want: true},
		{name: "mode 有内容", r: &dto.Reasoning{Mode: json.RawMessage(`"auto"`)}, want: false},
		{name: "context 是 null 时算没有内容", r: &dto.Reasoning{Context: json.RawMessage("null")}, want: true},
		{name: "context 有内容", r: &dto.Reasoning{Context: json.RawMessage(`{"window":"long"}`)}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasoningHasNothing(tc.r); got != tc.want {
				t.Errorf("reasoningHasNothing(%+v) = %v，期望 %v", tc.r, got, tc.want)
			}
		})
	}
}
