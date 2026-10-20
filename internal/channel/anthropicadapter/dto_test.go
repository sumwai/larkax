package anthropicadapter

import (
	"encoding/json"
	"reflect"
	"testing"
)

// thinking 是可选字段：块里有思考内容才落字段，没有就留 nil。
//
// 「字段不在」与「字段是空串」在下游要能分开，所以空值时不能落一个
// 指向空串的指针——那等于替上游说了一句它没说过的话。
func TestClaudeMediaMessageThinking(t *testing.T) {
	withThinking := claudeMediaMessage{Type: "thinking", Thinking: "想了一会儿"}.toRelaykit()
	if withThinking.Thinking == nil {
		t.Fatal("有 thinking 内容时应落字段")
	}
	if *withThinking.Thinking != "想了一会儿" {
		t.Errorf("Thinking = %q，期望上游原文", *withThinking.Thinking)
	}

	withoutThinking := claudeMediaMessage{Type: "text", Text: "回答"}.toRelaykit()
	if withoutThinking.Thinking != nil {
		t.Errorf("没有 thinking 内容时应留 nil，得到 %q", *withoutThinking.Thinking)
	}
}

// 工具参数有三种形态：合法对象、上游给的一截坏 JSON、以及把 JSON 当字符串
// 再包一层。后两种都要保留原文而不是丢掉整块——丢掉一个参数比丢掉整次调用轻。
//
// 没有 input 字段（或长度为零）时不该凭空造出一个 map：那会让「这个块没有
// 参数」变成「参数是空串」，下游按参数是否存在做的判断会跟着翻面。
func TestClaudeMediaMessageInput(t *testing.T) {
	cases := []struct {
		name  string
		input json.RawMessage
		want  map[string]any
	}{
		{name: "没有 input 字段", input: nil},
		{name: "合法对象", input: json.RawMessage(`{"city":"上海"}`), want: map[string]any{"city": "上海"}},
		{name: "半截对象保留原文", input: json.RawMessage(`{半截`), want: map[string]any{"_raw": `{半截`}},
		{name: "字符串形态拆一层转义", input: json.RawMessage(`"{\"a\":1}"`), want: map[string]any{"_raw": `{"a":1}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeMediaMessage{Type: "tool_use", Input: tc.input}.toRelaykit().Input
			if tc.want == nil {
				if got != nil {
					t.Errorf("没有参数时应留 nil，得到 %#v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Input = %#v，期望 %#v", got, tc.want)
			}
		})
	}
}
