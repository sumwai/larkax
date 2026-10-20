package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// 只有工具调用、没有正文时，OpenAI 的 content 是 null 而不是空串：
// 前者表示「没有正文」，后者表示「正文是空的」，客户端会区别对待。
func TestOpenAIContentIsNullWithoutText(t *testing.T) {
	resp := &channel.Response{
		Message: channel.Message{
			Role: "assistant",
			Content: []channel.ContentBlock{{
				Type:      "tool_use",
				ToolUseID: "call_1",
				ToolName:  "lookup",
				ToolInput: map[string]any{"q": "x"},
			}},
		},
		FinishReason: "tool_use",
	}

	raw, err := json.Marshal(newOpenAIChatCompletion("id", "m", 1, resp))
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	choice := got.Choices[0]
	if choice.Message.Content != nil {
		t.Errorf("没有正文时 content 应为 null，实际 %q", *choice.Message.Content)
	}
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q，期望 tool_calls", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("工具调用数 = %d", len(choice.Message.ToolCalls))
	}
	tc := choice.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "lookup" {
		t.Errorf("工具调用 = %+v", tc)
	}
	// 参数在管道里是对象，OpenAI 要的是字符串形态的 JSON。
	if tc.Function.Arguments != `{"q":"x"}` {
		t.Errorf("arguments = %q，期望对象编码成的字符串", tc.Function.Arguments)
	}
}

// 有正文时 content 是字符串，哪怕同时还有工具调用。
func TestOpenAIContentIsStringWithText(t *testing.T) {
	resp := &channel.Response{
		Message: channel.Message{
			Role: "assistant",
			Content: []channel.ContentBlock{
				{Type: "text", Text: "先查一下"},
				{Type: "tool_use", ToolUseID: "c1", ToolName: "f"},
			},
		},
	}

	got := newOpenAIChatCompletion("id", "m", 1, resp)
	if got.Choices[0].Message.Content == nil || *got.Choices[0].Message.Content != "先查一下" {
		t.Errorf("content = %s", showPtr(got.Choices[0].Message.Content))
	}
}

func TestOpenAIUsageTotals(t *testing.T) {
	// 推理 token 是 completion 的分解，不进 total：加了会让 total
	// 大于两项之和。
	resp := &channel.Response{
		Message: channel.Message{Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		Usage:   channel.Usage{PromptTokens: 10, CompletionTokens: 4, ReasoningTokens: 3},
	}

	got := newOpenAIChatCompletion("id", "m", 1, resp)
	if got.Usage.TotalTokens != 14 {
		t.Errorf("total_tokens = %d，期望 14", got.Usage.TotalTokens)
	}
}

// Anthropic 的 content 是块序列，正文块在工具块之前。
func TestClaudeContentOrder(t *testing.T) {
	resp := &channel.Response{
		Message: channel.Message{
			Role: "assistant",
			Content: []channel.ContentBlock{
				{Type: "tool_use", ToolUseID: "c1", ToolName: "f", ToolInput: map[string]any{"a": 1}},
				{Type: "text", Text: "查完了"},
			},
		},
		FinishReason: "tool_calls",
	}

	raw, err := json.Marshal(newClaudeMessage("id", "m", resp))
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Content) != 2 {
		t.Fatalf("块数 = %d，期望 2", len(got.Content))
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "查完了" {
		t.Errorf("第一个块应是正文: %+v", got.Content[0])
	}
	if got.Content[1].Type != "tool_use" || got.Content[1].ID != "c1" || got.Content[1].Name != "f" {
		t.Errorf("第二个块应是工具调用: %+v", got.Content[1])
	}
	// 参数在这里是**对象**，与 OpenAI 的字符串形态相反。
	if got.Content[1].Input["a"] != float64(1) {
		t.Errorf("input = %v", got.Content[1].Input)
	}
	if got.StopReason == nil || *got.StopReason != "tool_use" {
		t.Errorf("stop_reason = %s，期望 tool_use", showPtr(got.StopReason))
	}
	if got.StopSequence != nil {
		t.Errorf("stop_sequence 应为 null，实际 %v", *got.StopSequence)
	}
}

// content 不能是空数组：没有块时也要有一个（空的）正文块。
func TestClaudeAlwaysHasContentBlock(t *testing.T) {
	got := newClaudeMessage("id", "m", &channel.Response{})

	if len(got.Content) != 1 {
		t.Fatalf("块数 = %d，期望 1", len(got.Content))
	}
	block, ok := got.Content[0].(claudeTextBlock)
	if !ok || block.Type != "text" || block.Text != "" {
		t.Errorf("块 = %+v", got.Content[0])
	}
}

// 只有工具调用、没有正文时，content 里**不该**多出一个空正文块。
//
// 判据是「有正文，或者根本没有工具块」。把 `>` 写成 `>=` 之后它恒真：每一次
// 只带工具调用的回答都会在工具块前面多出一个 text 块，内容是空串。客户端按块
// 渲染时多出可见的空白，而把 content 当数组拼装的 SDK 会把它当成回答的一部分。
func TestClaudeToolOnlyResponseHasNoEmptyTextBlock(t *testing.T) {
	resp := &channel.Response{
		Message: channel.Message{
			Role: "assistant",
			Content: []channel.ContentBlock{
				{Type: "tool_use", ToolUseID: "c1", ToolName: "f", ToolInput: map[string]any{"a": 1}},
			},
		},
		FinishReason: "tool_calls",
	}

	got := newClaudeMessage("id", "m", resp)
	if len(got.Content) != 1 {
		t.Fatalf("块数 = %d，期望 1（只有工具块）: %+v", len(got.Content), got.Content)
	}
	block, ok := got.Content[0].(claudeToolUseBlock)
	if !ok || block.Type != "tool_use" || block.ID != "c1" {
		t.Errorf("唯一那个块应是工具调用: %+v", got.Content[0])
	}
}

// 没有结束原因时 stop_reason 是 null，而不是空串。
func TestClaudeStopReasonNullWhenUnknown(t *testing.T) {
	got := newClaudeMessage("id", "m", &channel.Response{FinishReason: ""})
	if got.StopReason != nil {
		t.Errorf("stop_reason = %q，期望 null", *got.StopReason)
	}
}

// 两家的缓存口径不同：本项目的 PromptTokens 含缓存，Anthropic 的
// input_tokens 不含。直接搬过去会让按两家口径各自加总的调用方差出
// 一个缓存量。
func TestClaudeUsageExcludesCacheFromInput(t *testing.T) {
	usage := channel.Usage{
		PromptTokens:     100,
		CompletionTokens: 20,
		CacheReadTokens:  30,
		CacheWriteTokens: 10,
	}

	got := claudeUsageOf(usage)
	if got.InputTokens != 60 {
		t.Errorf("input_tokens = %d，期望 100-30-10=60", got.InputTokens)
	}
	if got.OutputTokens != 20 {
		t.Errorf("output_tokens = %d", got.OutputTokens)
	}
	if got.CacheReadInputTokens != 30 || got.CacheCreationInputTokens != 10 {
		t.Errorf("缓存明细 = %+v", got)
	}
}

// 上游给的数自相矛盾时（缓存量大于输入总量）夹到 0，不回负数。
func TestClaudeUsageClampsNegativeInput(t *testing.T) {
	got := claudeUsageOf(channel.Usage{PromptTokens: 5, CacheReadTokens: 9})
	if got.InputTokens != 0 {
		t.Errorf("input_tokens = %d，期望夹到 0", got.InputTokens)
	}

	// 恰好为 0 的那一格与负数同处一条判据，但 `input < 0` 与 `input <= 0`
	// 两版在这里得到的是同一个数：守卫体做的事就是「赋 0」，而 input 本来
	// 就是 0。按「负数 / 等于 0 / 正数」三段看，两版对每一段都返回同一个值，
	// 所以这是**等价**变异，不是缺口——没有输入能区分它们。
	zero := claudeUsageOf(channel.Usage{PromptTokens: 5, CacheReadTokens: 5})
	if zero.InputTokens != 0 {
		t.Errorf("input_tokens = %d，期望 0（减完正好是 0）", zero.InputTokens)
	}
}

// 没有缓存的上游不该在响应里出现这两个字段：报了 0 与没报是两件事。
func TestClaudeUsageOmitsCacheWhenAbsent(t *testing.T) {
	raw, err := json.Marshal(claudeUsageOf(channel.Usage{PromptTokens: 10, CompletionTokens: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cache_") {
		t.Errorf("没有缓存时不该出现缓存字段: %s", raw)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	openaiCases := map[string]string{
		"stop":           "stop",
		"end_turn":       "stop",
		"stop_sequence":  "stop",
		"length":         "length",
		"max_tokens":     "length",
		"tool_calls":     "tool_calls",
		"tool_use":       "tool_calls",
		"function_call":  "tool_calls",
		"content_filter": "content_filter",
		// 认不出来的值原样透出，不编一个 "stop"。
		"":   "",
		"什么": "什么",
	}
	for in, want := range openaiCases {
		if got := openAIFinishReason(in); got != want {
			t.Errorf("openAIFinishReason(%q) = %q，期望 %q", in, got, want)
		}
	}

	claudeCases := map[string]string{
		"stop":          "end_turn",
		"length":        "max_tokens",
		"tool_calls":    "tool_use",
		"function_call": "tool_use",
		"end_turn":      "end_turn",
		"max_tokens":    "max_tokens",
		"tool_use":      "tool_use",
		"stop_sequence": "stop_sequence",
		"":              "",
		"什么":            "什么",
	}
	for in, want := range claudeCases {
		if got := claudeStopReason(in); got != want {
			t.Errorf("claudeStopReason(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestRenderSuccessRejectsUnknownProtocol(t *testing.T) {
	if _, err := renderSuccess(channel.Protocol("custom"), "id", "m", time.Now(), &channel.Response{}); err == nil {
		t.Error("未知协议没有非流式响应形状，应当报错")
	}
}

func TestToolArgumentsFallbacks(t *testing.T) {
	if got := toolArguments(nil); got != "{}" {
		t.Errorf("空参数应给空对象，实际 %q", got)
	}
	// 参数里有无法编码的值时不能连累整个响应。
	if got := toolArguments(map[string]any{"f": func() {}}); got != "{}" {
		t.Errorf("无法编码时给空对象，实际 %q", got)
	}
	// Anthropic 那边要求 input 是对象，空参数同样不能是 null。
	if got := toolInput(nil); got == nil || len(got) != 0 {
		t.Errorf("空参数应给空对象，实际 %v", got)
	}
}
