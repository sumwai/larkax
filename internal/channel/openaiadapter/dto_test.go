package openaiadapter

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
)

// 助手消息的 content 有三形态：字符串、内容块数组、缺失。断言落在转换之后
// 的内容本身，而不只是「没报错」——形状对了但内容丢了的实现照样能走完
// Chat，客户端拿到的是空回答。
//
// 「缺失」这一条同时说明了 `len(m.Content) > 0` 那一格为什么没有能把两版
// 分开的输入：把 `>` 改成 `>=` 之后，只有内容为空时会多走进那个分支，而
// 空字节上字符串与块数组两次 unmarshal 都报错（空不是合法 JSON），内容
// 停在零值 nil，与不进入分支的结果一样。这个等价依赖 kitutil 的编解码器
// 对空输入报错：默认是 encoding/json，且仓里没有别处调用 SetCodec 换掉
// 它；换了实现要重新看这一条。
func TestMessageContentShapes(t *testing.T) {
	t.Run("字符串", func(t *testing.T) {
		out := openAIMessage{Content: json.RawMessage(`"纯文本"`)}.toRelaykit()
		s, ok := out.Content.(string)
		if !ok {
			t.Fatalf("Content 类型为 %T，期望 string", out.Content)
		}
		if s != "纯文本" {
			t.Errorf("Content = %q，期望 %q", s, "纯文本")
		}
	})

	t.Run("块数组", func(t *testing.T) {
		out := openAIMessage{
			Content: json.RawMessage(`[{"type":"text","text":"块文本"}]`),
		}.toRelaykit()
		blocks, ok := out.Content.([]dto.MediaContent)
		if !ok {
			t.Fatalf("Content 类型为 %T，期望 []dto.MediaContent", out.Content)
		}
		if len(blocks) != 1 || blocks[0].Text != "块文本" {
			t.Errorf("Content = %+v，期望一段文本块「块文本」", blocks)
		}
	})

	t.Run("缺失", func(t *testing.T) {
		out := openAIMessage{}.toRelaykit()
		if out.Content != nil {
			t.Errorf("Content = %#v，缺失时应为 nil", out.Content)
		}
	})
}

// tool_calls 有就转成 relaykit 的结构，没有就必须留 nil。
//
// 两条各自固定一侧：有调用时丢了参数，上游的工具调用在对外响应里成了空壳；
// 没有调用时写一个 `[]`，客户端会看到「有 tool_calls 字段但是空数组」，
// 与「这次没有工具调用」是两件事。
func TestMessageToolCallsConversion(t *testing.T) {
	t.Run("有 tool_calls", func(t *testing.T) {
		var msg openAIMessage
		raw := `{"role":"assistant","tool_calls":[{"id":"call_1","type":"function",` +
			`"function":{"name":"f","arguments":"{\"a\":1}"}}]}`
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("解析消息失败: %v", err)
		}
		out := msg.toRelaykit()

		if out.ToolCalls == nil {
			t.Fatal("ToolCalls 为 nil，期望转成的调用数组")
		}
		var calls []dto.ToolCallResponse
		if err := json.Unmarshal(out.ToolCalls, &calls); err != nil {
			t.Fatalf("ToolCalls 不是调用数组: %v（%s）", err, out.ToolCalls)
		}
		if len(calls) != 1 {
			t.Fatalf("调用数 = %d，期望 1", len(calls))
		}
		c := calls[0]
		if c.ID != "call_1" || c.Type != "function" || c.Function.Name != "f" || c.Function.Arguments != `{"a":1}` {
			t.Errorf("调用 = %+v", c)
		}
	})

	t.Run("没有 tool_calls", func(t *testing.T) {
		out := openAIMessage{Role: "assistant"}.toRelaykit()
		if out.ToolCalls != nil {
			t.Errorf("ToolCalls = %s，没有调用时应为 nil", out.ToolCalls)
		}
	})
}

// 同一条消息里没写 reasoning_content 时留 nil，写了才带指针。
//
// 空串与「字段不在」在这条路径上是同一件事：上游没给思考内容。区别在于
// 写一个指向空串的指针会让下游以为「上游报了空思考」，快照里多出一列。
func TestMessageReasoningContentPresence(t *testing.T) {
	if got := (openAIMessage{Role: "assistant"}).toRelaykit().ReasoningContent; got != nil {
		t.Errorf("ReasoningContent = %q，没写时应为 nil", *got)
	}

	got := (openAIMessage{ReasoningContent: "思考"}).toRelaykit().ReasoningContent
	if got == nil || *got != "思考" {
		t.Errorf("ReasoningContent = %v，期望指向「思考」", got)
	}
}

// defaultString 只在传入空串时使用传进来的默认值，有值时原样返回。
//
// 调用点在工具调用的 type 上：上游常常不写这个字段，没有值时要用
// "function"。判据取反之后两个方向一起坏——没写的那种留空，客户端读到的
// 调用类型是空串；写了的反而被替换成默认值。两侧都要断言：只断言一侧的话，
// 一个永远返回默认值的实现也能通过。
func TestDefaultStringFallback(t *testing.T) {
	if got := defaultString("", "function"); got != "function" {
		t.Errorf("defaultString(%q, %q) = %q，期望 %q", "", "function", got, "function")
	}
	if got := defaultString("custom", "function"); got != "custom" {
		t.Errorf("defaultString(%q, %q) = %q，期望 %q", "custom", "function", got, "custom")
	}
}
