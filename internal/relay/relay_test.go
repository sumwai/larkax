package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/channel"
)

func ptr[T any](v T) *T { return &v }

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

func TestToRelaykitBasicFields(t *testing.T) {
	req := &channel.Request{
		Model:           "claude-sonnet-4",
		MaxTokens:       1024,
		Temperature:     ptr(0.0),
		TopP:            ptr(0.9),
		ReasoningEffort: "high",
		Messages: []channel.Message{
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "你好"}}},
		},
	}

	out, err := ToRelaykit(req)
	if err != nil {
		t.Fatalf("ToRelaykit 出错: %v", err)
	}
	if out.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q", out.Model)
	}
	if out.MaxTokens == nil || *out.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %s", showPtr(out.MaxTokens))
	}
	// Temperature 为 0 是合法取值，必须原样传出去而不是被当成「未设置」
	if out.Temperature == nil || *out.Temperature != 0 {
		t.Errorf("Temperature = %s，期望 0（指针非空）", showPtr(out.Temperature))
	}
	if out.TopP == nil || *out.TopP != 0.9 {
		t.Errorf("TopP = %s", showPtr(out.TopP))
	}
	if out.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %q", out.ReasoningEffort)
	}
	if out.Stream != nil {
		t.Error("未要求流式时 Stream 应为 nil")
	}
}

// MaxTokens 为 0 表示「未指定」，不能传成 0——那在上游是「输出 0 个 token」。
func TestToRelaykitOmitsZeroMaxTokens(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{Model: "m", MaxTokens: 0})
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens != nil {
		t.Errorf("MaxTokens 应为 nil，实际 %v", *out.MaxTokens)
	}
}

func TestToRelaykitStream(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stream == nil || !*out.Stream {
		t.Error("要求流式时 Stream 应为 true")
	}
}

func TestToRelaykitRejectsNil(t *testing.T) {
	if _, err := ToRelaykit(nil); err == nil {
		t.Error("nil 请求应报错")
	}
}

// 单块纯文本用字符串形态：那是各家 SDK 最普遍接受的写法，
// 日志与结构快照读起来也干净。
func TestPlainTextBecomesString(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model:    "m",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := out.Messages[0].Content.(string); !ok || got != "hi" {
		t.Errorf("Content = %#v，期望字符串 hi", out.Messages[0].Content)
	}
}

// 一旦含图片就必须用块序列，否则信息会丢。
func TestImageForcesBlockForm(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{
			{Type: "text", Text: "看这个"},
			{Type: "image", ImageURL: "https://example.com/a.png"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocks, ok := out.Messages[0].Content.([]dto.MediaContent)
	if !ok {
		t.Fatalf("Content 类型为 %T，期望块序列", out.Messages[0].Content)
	}
	if len(blocks) != 2 {
		t.Fatalf("块数为 %d，期望 2", len(blocks))
	}
	if blocks[1].Type != "image_url" {
		t.Errorf("第二块类型为 %q", blocks[1].Type)
	}
}

// base64 图片要还原成 data URL，上游认的是 URL 形态。
func TestBase64ImageBecomesDataURL(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{
			{Type: "image", ImageBase64: "AAAA", ImageMediaType: "image/png"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocks := out.Messages[0].Content.([]dto.MediaContent)
	urlObj, ok := blocks[0].ImageUrl.(map[string]any)
	if !ok {
		t.Fatalf("ImageUrl 类型为 %T", blocks[0].ImageUrl)
	}
	if urlObj["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("url = %v", urlObj["url"])
	}
}

func TestToolDefinitionConversion(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Tools: []channel.Tool{{
			Name:        "get_weather",
			Description: "查天气",
			InputSchema: map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("工具数为 %d", len(out.Tools))
	}
	tool := out.Tools[0]
	if tool.Type != "function" {
		t.Errorf("Type = %q，期望 function", tool.Type)
	}
	if tool.Function.Name != "get_weather" {
		t.Errorf("Name = %q", tool.Function.Name)
	}
	if tool.Function.Parameters == nil {
		t.Error("Parameters 丢了")
	}
}

// 助手发起的工具调用要序列化到 ToolCalls 上，参数必须是 JSON 字符串，
// 因为上游协议里它就是字符串。
func TestAssistantToolCallConversion(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Messages: []channel.Message{{Role: "assistant", Content: []channel.ContentBlock{
			{Type: "tool_use", ToolUseID: "call_1", ToolName: "get_weather",
				ToolInput: map[string]any{"city": "上海"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := out.Messages[0].ToolCalls
	if len(raw) == 0 {
		t.Fatal("ToolCalls 为空")
	}
	var calls []dto.ToolCallResponse
	if err := json.Unmarshal(raw, &calls); err != nil {
		t.Fatalf("ToolCalls 不是合法 JSON: %v", err)
	}
	if calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" {
		t.Errorf("工具调用丢了信息: %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"city":"上海"}` {
		t.Errorf("Arguments = %q", calls[0].Function.Arguments)
	}
}

// 工具结果靠消息上的 tool_call_id 关联。这条关系丢了，
// 整轮工具往返就无从解释。
func TestToolResultKeepsCallID(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Messages: []channel.Message{{Role: "tool", Content: []channel.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_9", Text: "晴，25 度"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].ToolCallId != "call_9" {
		t.Errorf("ToolCallId = %q，期望 call_9", out.Messages[0].ToolCallId)
	}
}

// 思考内容不进 messages：它是模型的中间产物，不是对话的一部分。
// 上游对它的处理方式各不相同，由适配器决定，转换层不猜。
func TestThinkingBlocksAreDropped(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Messages: []channel.Message{{Role: "assistant", Content: []channel.ContentBlock{
			{Type: "thinking", Text: "让我想想……"},
			{Type: "text", Text: "答案是 42"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := out.Messages[0].Content.(string); got != "答案是 42" {
		t.Errorf("Content = %#v，思考内容不应出现", out.Messages[0].Content)
	}
}

func TestUnknownContentTypeIsRejected(t *testing.T) {
	_, err := ToRelaykit(&channel.Request{
		Model:    "m",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "video"}}}},
	})
	if err == nil {
		t.Error("未知内容块类型应报错，而不是静默丢弃")
	}
}

// Extra 只认白名单字段。透传任意键意味着上游能通过它影响请求语义，
// 而那正是「插件不能改钱」这条边界旁边的事。
func TestExtrasAreWhitelisted(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Extra: map[string]any{
			"top_k":               5,
			"parallel_tool_calls": true,
			"something_unknown":   "ignored",
			"messages":            "不能覆盖消息",
			"temperature":         999.0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.TopK == nil || *out.TopK != 5 {
		t.Errorf("TopK = %s", showPtr(out.TopK))
	}
	if out.ParallelTooCalls == nil || !*out.ParallelTooCalls {
		t.Errorf("ParallelTooCalls = %s", showPtr(out.ParallelTooCalls))
	}
	// 白名单之外的一律忽略：不能被 Extra 覆盖语义
	if len(out.Messages) != 0 {
		t.Error("Extra 不应能注入 messages")
	}
	if out.Temperature != nil {
		t.Error("Extra 不应能覆盖 temperature")
	}
}

func TestExtrasUnserializableIsRejected(t *testing.T) {
	_, err := ToRelaykit(&channel.Request{
		Model: "m",
		Extra: map[string]any{"top_k": func() {}},
	})
	if err == nil {
		t.Error("无法序列化的扩展参数应报错")
	}
}

func TestFormatForKnownProtocols(t *testing.T) {
	cases := map[channel.Protocol]string{
		channel.ProtocolOpenAI:          "openai",
		channel.ProtocolAnthropic:       "claude",
		channel.ProtocolOpenAIResponses: "openai_responses",
	}
	for p, want := range cases {
		got, err := formatFor(p)
		if err != nil {
			t.Errorf("协议 %q 应能映射: %v", p, err)
		}
		if string(got) != want {
			t.Errorf("协议 %q 映射为 %q，期望 %q", p, got, want)
		}
	}
}

// 不认识的协议没有目标格式，猜一个会掩盖配置错误。
func TestFormatForUnknownProtocolIsRejected(t *testing.T) {
	if _, err := formatFor(channel.Protocol("custom")); err == nil {
		t.Error("未知协议应报错")
	}
}

func TestSplitDataURL(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantData string
		wantType string
		wantOK   bool
	}{
		{"正常", "data:image/png;base64,AAAA", "AAAA", "image/png", true},
		{"非 data 开头", "https://example.com/a.png", "", "", false},
		{"缺 base64 标记", "data:image/png,AAAA", "", "", false},
		{"缺媒体类型", "data:;base64,AAAA", "", "", false},
		{"缺数据", "data:image/png;base64,", "", "", false},
		{"空串", "", "", "", false},
		{"只有前缀", "data:", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, mediaType, ok := splitDataURL(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v，期望 %v", ok, tc.wantOK)
			}
			if data != tc.wantData || mediaType != tc.wantType {
				t.Errorf("得到 (%q, %q)，期望 (%q, %q)", data, mediaType, tc.wantData, tc.wantType)
			}
		})
	}
}

// ── 响应方向 ────────────────────────────────────────────────────────────

func TestResponseFromOpenAI(t *testing.T) {
	src := &dto.OpenAITextResponse{
		Model: "gpt-4o-2024",
		Choices: []dto.OpenAITextResponseChoice{{
			Message:      dto.Message{Role: "assistant", Content: "你好"},
			FinishReason: "stop",
		}},
		Usage: dto.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	got, err := ResponseFromRelaykit(src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 上游回报的模型名可能与请求不同（别名或版本替换），计费要按实际的算
	if got.UpstreamModel != "gpt-4o-2024" {
		t.Errorf("UpstreamModel = %q", got.UpstreamModel)
	}
	if got.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", got.FinishReason)
	}
	if len(got.Message.Content) != 1 || got.Message.Content[0].Text != "你好" {
		t.Errorf("内容 = %+v", got.Message.Content)
	}
	if got.Usage.PromptTokens != 10 || got.Usage.CompletionTokens != 5 {
		t.Errorf("用量 = %+v", got.Usage)
	}
	if got.Raw == nil {
		t.Error("Raw 应保留上游原样数据，供事后发现口径变化")
	}
}

func TestResponseFromOpenAIWithToolCall(t *testing.T) {
	src := &dto.OpenAITextResponse{
		Model: "gpt-4o",
		Choices: []dto.OpenAITextResponseChoice{{
			Message: dto.Message{
				Role:      "assistant",
				ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]`),
			},
			FinishReason: "tool_calls",
		}},
	}

	got, err := ResponseFromRelaykit(src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(got.Message.Content) != 1 {
		t.Fatalf("内容块数为 %d", len(got.Message.Content))
	}
	block := got.Message.Content[0]
	if block.Type != "tool_use" || block.ToolUseID != "call_1" || block.ToolName != "f" {
		t.Errorf("工具块 = %+v", block)
	}
	if block.ToolInput["a"] != float64(1) {
		t.Errorf("参数 = %+v", block.ToolInput)
	}
}

// 参数解析失败不算致命：上游可能给了半截 JSON，丢掉整条响应比丢掉一段参数更糟。
func TestResponseWithMalformedToolArguments(t *testing.T) {
	src := &dto.OpenAITextResponse{
		Model: "gpt-4o",
		Choices: []dto.OpenAITextResponseChoice{{
			Message: dto.Message{
				Role:      "assistant",
				ToolCalls: json.RawMessage(`[{"id":"c","type":"function","function":{"name":"f","arguments":"{半截"}}]`),
			},
		}},
	}

	got, err := ResponseFromRelaykit(src)
	if err != nil {
		t.Fatalf("半截参数不应让整条响应失败: %v", err)
	}
	block := got.Message.Content[0]
	if block.ToolInput["_raw"] != "{半截" {
		t.Errorf("原文应保留在 _raw 里: %+v", block.ToolInput)
	}
}

func TestResponseFromClaude(t *testing.T) {
	src := &dto.ClaudeResponse{
		Model:      "claude-sonnet-4",
		StopReason: "end_turn",
		Content: []dto.ClaudeMediaMessage{
			{Type: "text", Text: ptr("回答")},
			{Type: "thinking", Thinking: ptr("思考过程")},
			{Type: "tool_use", Id: "tu_1", Name: "f", Input: map[string]any{"x": 1}},
		},
		Usage: &dto.ClaudeUsage{
			InputTokens:              100,
			OutputTokens:             20,
			CacheReadInputTokens:     30,
			CacheCreationInputTokens: 10,
		},
	}

	got, err := ResponseFromRelaykit(src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Message.Role != "assistant" {
		t.Errorf("Role = %q", got.Message.Role)
	}
	// 思考内容不进 Message：它与 assistant 的可见输出是两件事
	for _, b := range got.Message.Content {
		if b.Text == "思考过程" {
			t.Error("思考内容不应进 Message")
		}
	}
	if len(got.Message.Content) != 2 {
		t.Fatalf("内容块数为 %d，期望 2（文本 + 工具）", len(got.Message.Content))
	}

	// Claude 的 input_tokens 不含缓存，归一化后必须补上，否则输入被低估
	if got.Usage.PromptTokens != 140 {
		t.Errorf("PromptTokens = %d，期望 140（100 输入 + 30 读缓存 + 10 写缓存）", got.Usage.PromptTokens)
	}
	if got.Usage.CacheReadTokens != 30 || got.Usage.CacheWriteTokens != 10 {
		t.Errorf("缓存明细 = %+v", got.Usage)
	}
	if got.Usage.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d", got.Usage.CompletionTokens)
	}
}

func TestResponseFromResponses(t *testing.T) {
	src := &dto.OpenAIResponsesResponse{
		ID:     "resp_1",
		Model:  "gpt-5-2026",
		Status: json.RawMessage(`"completed"`),
		Output: []dto.ResponsesOutput{
			{
				Type:    "reasoning",
				Summary: []dto.ResponsesReasoningSummaryPart{{Type: "summary_text", Text: "先想一下"}},
			},
			{
				Type:    "message",
				Role:    "assistant",
				Content: []dto.ResponsesOutputContent{{Type: "output_text", Text: "你好"}},
			},
			{
				Type:      "function_call",
				CallId:    "call_1",
				Name:      "lookup",
				Arguments: json.RawMessage(`"{\"q\":\"天气\"}"`),
			},
		},
		Usage: &dto.Usage{
			PromptTokens:        100,
			CompletionTokens:    40,
			PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 30},
			CompletionTokenDetails: dto.OutputTokenDetails{
				ReasoningTokens: 25,
			},
			UsageSource:   dto.BillingUsageSourceOAIResponses,
			UsageSemantic: dto.BillingUsageSemanticOpenAI,
		},
	}

	got, err := ResponseFromRelaykit(src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.UpstreamModel != "gpt-5-2026" {
		t.Errorf("UpstreamModel = %q", got.UpstreamModel)
	}
	if got.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", got.FinishReason)
	}

	var text, tool string
	for _, b := range got.Message.Content {
		switch b.Type {
		case "text":
			text += b.Text
		case "tool_use":
			tool = b.ToolName
			if b.ToolUseID != "call_1" {
				t.Errorf("工具调用 id = %q", b.ToolUseID)
			}
			if b.ToolInput["q"] != "天气" {
				t.Errorf("工具参数 = %+v", b.ToolInput)
			}
		case "thinking":
			t.Errorf("思考摘要不该进正文块: %+v", b)
		}
	}
	if text != "你好" {
		t.Errorf("正文 = %q（思考摘要不是可见输出）", text)
	}
	if tool != "lookup" {
		t.Errorf("工具名 = %q", tool)
	}

	// 用量直接读 r.Usage，不经 relaykit 那条 Responses→Chat 的转换。
	if got.Usage.PromptTokens != 100 || got.Usage.CompletionTokens != 40 ||
		got.Usage.CacheReadTokens != 30 || got.Usage.ReasoningTokens != 25 {
		t.Errorf("用量 = %+v", got.Usage)
	}
	if !got.Usage.CompletionIncludesReasoning {
		t.Error("OpenAI 口径下推理含在输出里，要声明出来")
	}
	if got.Raw == nil {
		t.Error("Raw 应保留上游原样数据，供事后发现口径变化")
	}
}

// status=incomplete 是「停在了长度上限上」，不是失败：结束原因折成 length。
func TestResponseFromResponsesIncomplete(t *testing.T) {
	src := &dto.OpenAIResponsesResponse{
		ID:                "resp_1",
		Model:             "m",
		Status:            json.RawMessage(`"incomplete"`),
		IncompleteDetails: &dto.IncompleteDetails{Reason: "max_output_tokens"},
	}

	got, err := ResponseFromRelaykit(src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.FinishReason != "length" {
		t.Errorf("FinishReason = %q", got.FinishReason)
	}
}

func TestToRelaykitResponses(t *testing.T) {
	req := &channel.Request{
		Model:     "gpt-5",
		MaxTokens: 256,
		Messages: []channel.Message{
			{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "你是助手"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	openaiReq, err := ToRelaykit(req)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got, err := ToRelaykitResponses(context.Background(), openaiReq)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Model != "gpt-5" {
		t.Errorf("Model = %q", got.Model)
	}
	if got.MaxOutputTokens == nil || *got.MaxOutputTokens != 256 {
		t.Errorf("MaxOutputTokens = %s", showPtr(got.MaxOutputTokens))
	}
	// system 消息折成顶层字段，不再占 input 里的一项。
	var instructions string
	if err := json.Unmarshal(got.Instructions, &instructions); err != nil || instructions != "你是助手" {
		t.Errorf("Instructions = %s（%v）", got.Instructions, err)
	}
	var input []map[string]any
	if err := json.Unmarshal(got.Input, &input); err != nil {
		t.Fatalf("input 解析失败: %v", err)
	}
	if len(input) != 1 || input[0]["role"] != "user" {
		t.Errorf("input = %v", input)
	}
}

func TestToRelaykitResponsesRejectsNil(t *testing.T) {
	if _, err := ToRelaykitResponses(context.Background(), nil); err == nil {
		t.Error("nil 请求应报错")
	}
}

func TestResponseRejectsUnknownAndNil(t *testing.T) {
	if _, err := ResponseFromRelaykit("字符串"); err == nil {
		t.Error("未知类型应报错")
	}
	if _, err := ResponseFromRelaykit((*dto.OpenAITextResponse)(nil)); err == nil {
		t.Error("nil 响应应报错")
	}
	if _, err := ResponseFromRelaykit((*dto.ClaudeResponse)(nil)); err == nil {
		t.Error("nil 响应应报错")
	}
	if _, err := ResponseFromRelaykit((*dto.OpenAIResponsesResponse)(nil)); err == nil {
		t.Error("nil 响应应报错")
	}
}

// ── 用量口径 ────────────────────────────────────────────────────────────

func TestUsageFromRelaykitNil(t *testing.T) {
	got := UsageFromRelaykit(nil)
	if got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Errorf("nil 用量应为零值，得到 %+v", got)
	}
}

// Anthropic 口径只填 InputTokens/OutputTokens，归一化后要能读出来。
func TestUsageFromAnthropicSemantic(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		InputTokens:  50,
		OutputTokens: 7,
		UsageSource:  "claude_messages",
	})
	if got.PromptTokens != 50 {
		t.Errorf("PromptTokens = %d，期望 50", got.PromptTokens)
	}
	if got.CompletionTokens != 7 {
		t.Errorf("CompletionTokens = %d，期望 7", got.CompletionTokens)
	}
	// Anthropic 的 output_tokens 含扩展思考的 token，协议不单独报推理量。
	if !got.CompletionIncludesReasoning {
		t.Error("Anthropic 口径的 output_tokens 含思考 token，应声明「已含」")
	}
}

// 只报了缓存而没报输入时，输入至少等于缓存量。
// 否则会算出「输入为 0 但有缓存命中」这种自相矛盾的账。
func TestUsageCacheOnlyImpliesPrompt(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 40},
	})
	if got.PromptTokens != 40 {
		t.Errorf("PromptTokens = %d，期望 40", got.PromptTokens)
	}
}

// 推理 token 照抄上游报的数，输出也照抄——两者的关系由声明字段给出，
// 减掉推理是计费层（billing.FromChannelUsage）的事。
//
// 这里把 completion 改小是错的：channel.Usage 是协议层的归一化结果，
// 它还要被序列化给调用方看（响应里的 completion_tokens 就是它），
// 而调用方看到的应当是上游报的那个数。
func TestUsageReasoningTokens(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		PromptTokens:           10,
		CompletionTokens:       100,
		CompletionTokenDetails: dto.OutputTokenDetails{ReasoningTokens: 60},
	})
	if got.ReasoningTokens != 60 {
		t.Errorf("ReasoningTokens = %d", got.ReasoningTokens)
	}
	if got.CompletionTokens != 100 {
		t.Errorf("CompletionTokens 不应被推理 token 改动，得到 %d", got.CompletionTokens)
	}
	if !got.CompletionIncludesReasoning {
		t.Error("dto.Usage 是 OpenAI 语义：completion_tokens 含 reasoning_tokens，应声明「已含」")
	}
}

// 上游报的是估算值时，Raw 里要留下标记，
// 否则「估的」和「实测的」在账上没区别。
func TestUsageMarksEstimated(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		PromptTokens: 1,
		BillingUsage: &dto.BillingUsage{Estimated: true},
	})
	if got.Raw["estimated"] != true {
		t.Errorf("Raw 应标记 estimated: %+v", got.Raw)
	}
}

func TestUsageCacheCreationSplit(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		PromptTokens:                1,
		ClaudeCacheCreation5mTokens: 100,
		ClaudeCacheCreation1hTokens: 200,
	})
	if got.UnitCounts["cache_write_5m"] != 100 || got.UnitCounts["cache_write_1h"] != 200 {
		t.Errorf("缓存写入拆分 = %+v", got.UnitCounts)
	}
}

// 没有 5m/1h 拆分时 UnitCounts 应当是 nil 而不是空 map：空 map 会被
// 序列化成 {}，在账上看起来像「报了拆分但每一项都是 0」。
func TestUsageUnitCountsNilWithoutSplit(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{PromptTokens: 1})
	if got.UnitCounts != nil {
		t.Errorf("没有 5m/1h 拆分时 UnitCounts 应为 nil, 得到 %v", got.UnitCounts)
	}
}

// 口径由**声明出来的** UsageSemantic 决定，不由数字反推。
//
// 五条各一个取值。同一个逻辑请求（非缓存输入 1000、命中缓存 8000、输出 200）
// 按两家口径各报一次，归一之后必须落到同一组数——这是这套换算的全部意义
// 所在：口径写反了不会报错，只会让某一家的账单悄悄变高。
//
// 五个取值里 anthropic 是唯一需要补缓存的；openai / gemini / 空 / 未知
// 走同一条路（默认分支），各自单列一行是为了把「这几个取值都归到
// 「已含缓存」那一类」这件事写成断言，而不是留在注释里。
func TestUsageSemanticDecidesCacheAccounting(t *testing.T) {
	cases := []struct {
		name      string
		semantic  string
		usage     *dto.Usage
		wantProm  int64
		wantRead  int64
		wantWrite int64
	}{
		{
			name:     "openai：prompt_tokens 已含缓存，不能再加",
			semantic: dto.BillingUsageSemanticOpenAI,
			usage: &dto.Usage{
				PromptTokens:        9000,
				CompletionTokens:    200,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
			},
			wantProm: 9000, wantRead: 8000, wantWrite: 0,
		},
		{
			name:     "anthropic：PromptTokens 是 input_tokens，不含缓存，要补回来",
			semantic: dto.BillingUsageSemanticAnthropic,
			usage: &dto.Usage{
				PromptTokens:        1000,
				CompletionTokens:    200,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
			},
			wantProm: 9000, wantRead: 8000, wantWrite: 0,
		},
		{
			name:     "gemini：relaykit 折成这一形状时 prompt 已含 cachedContent",
			semantic: dto.BillingUsageSemanticGemini,
			usage: &dto.Usage{
				PromptTokens:        9000,
				CompletionTokens:    200,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
			},
			wantProm: 9000, wantRead: 8000, wantWrite: 0,
		},
		{
			name:     "未声明：按 dto.Usage 的原生口径（OpenAI）读",
			semantic: "",
			usage: &dto.Usage{
				PromptTokens:        9000,
				CompletionTokens:    200,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
			},
			wantProm: 9000, wantRead: 8000, wantWrite: 0,
		},
		{
			name:     "未知取值：同未声明，原值留在 Raw 里",
			semantic: "bedrock",
			usage: &dto.Usage{
				PromptTokens:        9000,
				CompletionTokens:    200,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
			},
			wantProm: 9000, wantRead: 8000, wantWrite: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.usage.UsageSemantic = tc.semantic
			got := UsageFromRelaykit(tc.usage)
			if got.PromptTokens != tc.wantProm {
				t.Errorf("PromptTokens = %d，期望 %d", got.PromptTokens, tc.wantProm)
			}
			if got.CompletionTokens != 200 {
				t.Errorf("CompletionTokens = %d，期望 200", got.CompletionTokens)
			}
			if got.CacheReadTokens != tc.wantRead || got.CacheWriteTokens != tc.wantWrite {
				t.Errorf("缓存明细 = 读 %d / 写 %d，期望 读 %d / 写 %d",
					got.CacheReadTokens, got.CacheWriteTokens, tc.wantRead, tc.wantWrite)
			}
			// 非缓存输入是缓存单价与普通单价的分界，两边都要落在 1000。
			if nonCached := got.PromptTokens - got.CacheReadTokens - got.CacheWriteTokens; nonCached != 1000 {
				t.Errorf("非缓存输入 = %d，期望 1000", nonCached)
			}
		})
	}
}

// Anthropic 口径的缓存写入也分列，两个字段都要进 CacheWriteTokens，
// 且都算进输入总量。
func TestUsageAnthropicCacheWriteCounts(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		PromptTokens:        1000,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100, CachedCreationTokens: 200},
		UsageSemantic:       dto.BillingUsageSemanticAnthropic,
	})
	if got.PromptTokens != 1300 {
		t.Errorf("PromptTokens = %d，期望 1300（1000 + 100 + 200）", got.PromptTokens)
	}
	if got.CacheReadTokens != 100 || got.CacheWriteTokens != 200 {
		t.Errorf("缓存明细 = 读 %d / 写 %d，期望 100 / 200", got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// 整段 prompt 命中缓存：Anthropic 的 input_tokens 就是 0。
//
// 这条**必须走 ResponseFromRelaykit**而不是直接调 UsageFromRelaykit：
// 缺陷长在「调用方与被调用方各加一次缓存」的缝里，直接调后者会绕过
// 调用方那次加法，两条用例各自都绿而缝还在（LESSONS.md 第 24 条）。
func TestUsageAnthropicAllCachedPrompt(t *testing.T) {
	src := &dto.ClaudeResponse{
		Model: "m",
		Usage: &dto.ClaudeUsage{
			InputTokens:              0,
			OutputTokens:             20,
			CacheReadInputTokens:     30,
			CacheCreationInputTokens: 10,
		},
	}
	got, err := ResponseFromRelaykit(src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Usage.PromptTokens != 40 {
		t.Errorf("PromptTokens = %d，期望 40（缓存读 30 + 缓存写 10，只加一次）",
			got.Usage.PromptTokens)
	}
	if got.Usage.CacheReadTokens != 30 || got.Usage.CacheWriteTokens != 10 {
		t.Errorf("缓存明细 = 读 %d / 写 %d，期望 30 / 10",
			got.Usage.CacheReadTokens, got.Usage.CacheWriteTokens)
	}
	if got.Usage.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d，期望 20", got.Usage.CompletionTokens)
	}
}

// 未声明口径时按 dto.Usage 的原生口径（OpenAI）读——但这件事要留在 Raw 里，
// 否则事后分不出一笔用量是按声明的口径读的，还是按默认分支读的。
func TestUsageSemanticMissingIsVisible(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{PromptTokens: 7})
	if got.Raw["semantic_missing"] != true {
		t.Errorf("未声明口径应在 Raw 里留痕: %+v", got.Raw)
	}
	if _, ok := got.Raw["semantic"]; ok {
		t.Errorf("未声明口径时不该凭空写一个 semantic: %+v", got.Raw)
	}
}

// 上游整条响应没带 usage 时也会缺声明，但那不是生产方的疏漏：
// 一个数都没读到，就不存在「按哪个口径读」的问题。
//
// 两种缺声明都要能走到底，所以这条与上一条成对——只留一条的话，
// 门槛写成「恒不打」或「恒打」都能通过。
func TestUsageSemanticMissingNeedsCounts(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{})
	if got.Raw["semantic_missing"] != nil {
		t.Errorf("一个数都没读到，不该标 semantic_missing: %+v", got.Raw)
	}
	if got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Errorf("空用量应读成 0: %+v", got)
	}
}

// 未知取值照 openai 读，而原值留在 Raw 里。
//
// 带上缓存是为了让分支可观测：按 openai 读是 7，误按 anthropic 读会是 10。
func TestUsageUnknownSemanticIsVisible(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		PromptTokens:        7,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 3},
		UsageSemantic:       "bedrock",
	})
	if got.PromptTokens != 7 {
		t.Errorf("PromptTokens = %d，期望 7（未知取值不该按 anthropic 补缓存）", got.PromptTokens)
	}
	if got.Raw["semantic"] != "bedrock" {
		t.Errorf("未知取值应留在 Raw 里: %+v", got.Raw)
	}
	if _, ok := got.Raw["semantic_missing"]; ok {
		t.Errorf("声明了取值（哪怕是未知的）就不该标 missing: %+v", got.Raw)
	}
}

// 同一个逻辑请求按两家口径各报一次，归一之后必须落到同一组数。
func TestUsageDialectsAgreeOnSameRequest(t *testing.T) {
	// 真实构成：非缓存输入 1000、命中缓存 8000、输出 200。
	anthropic := UsageFromRelaykit(&dto.Usage{
		PromptTokens:        1000, // Anthropic 的 input_tokens 不含缓存
		CompletionTokens:    200,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
		UsageSemantic:       dto.BillingUsageSemanticAnthropic,
	})
	openai := UsageFromRelaykit(&dto.Usage{
		PromptTokens:        9000, // OpenAI 的 prompt_tokens 已含缓存
		CompletionTokens:    200,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
		UsageSemantic:       dto.BillingUsageSemanticOpenAI,
	})
	// Responses 的 input_tokens 与 OpenAI 的 prompt_tokens 同义（都含缓存），
	// 所以它与 openai 那一支归一后应当是同一个数——它是**同一族**，
	// 而不是像 Anthropic 那样要补一次缓存。
	responses := UsageFromRelaykit(&dto.Usage{
		PromptTokens:        9000,
		CompletionTokens:    200,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
		UsageSource:         dto.BillingUsageSourceOAIResponses,
		UsageSemantic:       dto.BillingUsageSemanticOpenAI,
	})

	// 逐个字段比而不是比结构体：channel.Usage 里有 map，整个结构体不可比较，
	// 而 Raw 那一栏本来就该有差别（它记着来源）。
	for _, tc := range []struct {
		name       string
		responses  int64
		openaiLike int64
	}{
		{"输入总量", responses.PromptTokens, openai.PromptTokens},
		{"缓存读取", responses.CacheReadTokens, openai.CacheReadTokens},
		{"输出", responses.CompletionTokens, openai.CompletionTokens},
		{"推理", responses.ReasoningTokens, openai.ReasoningTokens},
	} {
		if tc.responses != tc.openaiLike {
			t.Errorf("%s: Responses %d, OpenAI %d（同一族的两种表面应当一致）",
				tc.name, tc.responses, tc.openaiLike)
		}
	}

	if anthropic.PromptTokens != openai.PromptTokens {
		t.Errorf("归一后输入总量不一致: Anthropic %d, OpenAI %d（应为同一个数）",
			anthropic.PromptTokens, openai.PromptTokens)
	}
	if anthropic.PromptTokens != 9000 {
		t.Errorf("输入 = %d, 期望 9000", anthropic.PromptTokens)
	}
	if anthropic.CacheReadTokens != openai.CacheReadTokens {
		t.Errorf("缓存读取不一致: %d vs %d", anthropic.CacheReadTokens, openai.CacheReadTokens)
	}
	if anthropic.CompletionTokens != openai.CompletionTokens {
		t.Errorf("输出不一致: %d vs %d", anthropic.CompletionTokens, openai.CompletionTokens)
	}
	// 非缓存输入也要一致，它是缓存单价与普通单价的分界。
	for name, u := range map[string]channel.Usage{"anthropic": anthropic, "openai": openai} {
		if nonCached := u.PromptTokens - u.CacheReadTokens - u.CacheWriteTokens; nonCached != 1000 {
			t.Errorf("%s 的非缓存输入 = %d, 期望 1000", name, nonCached)
		}
	}
}

// ── 反向：relaykit → 本项目 ──────────────────────────────────────────────

func TestFromRelaykitOpenAIRequest(t *testing.T) {
	src := dto.GeneralOpenAIRequest{
		Model:           "claude-sonnet-4",
		MaxTokens:       ptr(uint(512)),
		Temperature:     ptr(0.0),
		ReasoningEffort: "medium",
		Stream:          ptr(true),
		Messages: []dto.Message{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "问题"},
		},
		Tools: []dto.ToolCallRequest{{
			Type: "function",
			Function: dto.FunctionRequest{
				Name:        "f",
				Description: "d",
				Parameters:  map[string]any{"type": "object"},
			},
		}},
	}

	got, err := FromRelaykitRequest(context.Background(), channel.ProtocolOpenAI, &src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Model != "claude-sonnet-4" || got.MaxTokens != 512 {
		t.Errorf("基本字段 = %+v", got)
	}
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Error("Temperature 为 0 是合法值，必须保留指针")
	}
	if !got.Stream {
		t.Error("Stream 应为 true")
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" {
		t.Errorf("消息 = %+v", got.Messages)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "f" {
		t.Errorf("工具 = %+v", got.Tools)
	}
	if got.Tools[0].InputSchema["type"] != "object" {
		t.Errorf("工具 schema 丢了: %+v", got.Tools[0].InputSchema)
	}
}

// Anthropic 请求先经 relaykit 统一到 OpenAI 形状，再翻译成管道类型。
// 这样下面只需要一条路径。
func TestFromRelaykitClaudeRequest(t *testing.T) {
	src := &dto.ClaudeRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: ptr(uint(256)),
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "你好"},
		},
	}

	got, err := FromRelaykitRequest(context.Background(), channel.ProtocolAnthropic, src)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q", got.Model)
	}
	if got.MaxTokens != 256 {
		t.Errorf("MaxTokens = %d", got.MaxTokens)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) == 0 {
		t.Fatalf("消息 = %+v", got.Messages)
	}
	if got.Messages[0].Content[0].Text != "你好" {
		t.Errorf("文本 = %q", got.Messages[0].Content[0].Text)
	}
}

func TestFromRelaykitRejectsUnknownType(t *testing.T) {
	if _, err := FromRelaykitRequest(context.Background(), channel.ProtocolOpenAI, "不是请求"); err == nil {
		t.Error("未知类型应报错")
	}
	if _, err := FromRelaykitRequest(context.Background(), channel.Protocol("custom"), &dto.GeneralOpenAIRequest{}); err == nil {
		t.Error("未知协议应报错")
	}
}

// 客户端给的助手消息里带工具调用时，要还原成 tool_use 内容块，
// 参数解析不回就保留原文——丢掉整条请求比丢掉一段参数更糟。
func TestFromRelaykitAssistantToolCalls(t *testing.T) {
	src := dto.GeneralOpenAIRequest{
		Model: "m",
		Messages: []dto.Message{{
			Role:      "assistant",
			ToolCalls: json.RawMessage(`[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"k\":\"v\"}"}}]`),
		}},
	}

	got, err := FromRelaykitRequest(context.Background(), channel.ProtocolOpenAI, &src)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range got.Messages[0].Content {
		if b.Type == "tool_use" {
			found = true
			if b.ToolUseID != "c1" || b.ToolName != "f" || b.ToolInput["k"] != "v" {
				t.Errorf("工具块 = %+v", b)
			}
		}
	}
	if !found {
		t.Fatalf("未还原出工具调用: %+v", got.Messages[0].Content)
	}
}

// role=tool 的消息靠消息上的 tool_call_id 关联。
// 不补成内容块的话，管道里那条结果就成了无主的文本。
func TestFromRelaykitToolResultBecomesBlock(t *testing.T) {
	src := dto.GeneralOpenAIRequest{
		Model: "m",
		Messages: []dto.Message{{
			Role:       "tool",
			ToolCallId: "c9",
			Content:    "结果文本",
		}},
	}

	got, err := FromRelaykitRequest(context.Background(), channel.ProtocolOpenAI, &src)
	if err != nil {
		t.Fatal(err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("内容块 = %+v", blocks)
	}
	if blocks[0].ToolUseID != "c9" || blocks[0].Text != "结果文本" {
		t.Errorf("工具结果块 = %+v", blocks[0])
	}
}

// 内容块序列形式（多模态）也要能解析回来。
func TestFromRelaykitContentBlocks(t *testing.T) {
	src := dto.GeneralOpenAIRequest{
		Model: "m",
		Messages: []dto.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "描述"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/jpeg;base64,ZZZ",
				}},
			},
		}},
	}

	got, err := FromRelaykitRequest(context.Background(), channel.ProtocolOpenAI, &src)
	if err != nil {
		t.Fatal(err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 2 {
		t.Fatalf("内容块 = %+v", blocks)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "描述" {
		t.Errorf("文本块 = %+v", blocks[0])
	}
	if blocks[1].Type != "image" || blocks[1].ImageBase64 != "ZZZ" || blocks[1].ImageMediaType != "image/jpeg" {
		t.Errorf("图片块 = %+v", blocks[1])
	}
}

// 普通图片 URL 保持 URL 形态，不强行下载或解码——那是媒体解析器的事。
func TestFromRelaykitImageURLStaysURL(t *testing.T) {
	src := dto.GeneralOpenAIRequest{
		Model: "m",
		Messages: []dto.Message{{
			Role: "user",
			Content: []any{
				map[string]any{"type": "image_url", "image_url": "https://example.com/a.png"},
			},
		}},
	}

	got, err := FromRelaykitRequest(context.Background(), channel.ProtocolOpenAI, &src)
	if err != nil {
		t.Fatal(err)
	}
	block := got.Messages[0].Content[0]
	if block.ImageURL != "https://example.com/a.png" {
		t.Errorf("图片块 = %+v", block)
	}
}

// 往返一遍：本项目的请求 → relaykit → 本项目的请求。
// 关键字段不能在这个过程中丢掉或变形。
func TestRequestRoundTrip(t *testing.T) {
	original := &channel.Request{
		Model:           "claude-sonnet-4",
		MaxTokens:       1024,
		Temperature:     ptr(0.7),
		TopP:            ptr(0.95),
		ReasoningEffort: "high",
		Messages: []channel.Message{
			{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "系统提示"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "你好"}}},
		},
		Tools: []channel.Tool{{Name: "f", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	}

	mid, err := ToRelaykit(original)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromRelaykitRequest(context.Background(), channel.ProtocolOpenAI, mid)
	if err != nil {
		t.Fatal(err)
	}

	if back.Model != original.Model || back.MaxTokens != original.MaxTokens {
		t.Errorf("基本字段丢失: %+v", back)
	}
	if back.Temperature == nil || *back.Temperature != 0.7 {
		t.Errorf("Temperature = %s", showPtr(back.Temperature))
	}
	if back.TopP == nil || *back.TopP != 0.95 {
		t.Errorf("TopP = %s", showPtr(back.TopP))
	}
	if back.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %q", back.ReasoningEffort)
	}
	if len(back.Messages) != 2 {
		t.Fatalf("消息数为 %d", len(back.Messages))
	}
	if back.Messages[0].Content[0].Text != "系统提示" || back.Messages[1].Content[0].Text != "你好" {
		t.Errorf("消息内容变形: %+v", back.Messages)
	}
	if len(back.Tools) != 1 || back.Tools[0].Name != "f" {
		t.Errorf("工具丢失: %+v", back.Tools)
	}
}

// ParseRequest 是入口唯一的解析点：协议由调用方给出，不由请求体猜。
func TestParseRequestOpenAIBody(t *testing.T) {
	body := []byte(`{
		"model": "m-1",
		"messages": [{"role": "user", "content": "你好"}],
		"max_tokens": 64,
		"temperature": 0
	}`)

	got, err := ParseRequest(context.Background(), channel.ProtocolOpenAI, body)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Model != "m-1" || got.MaxTokens != 64 {
		t.Errorf("基本字段 = %+v", got)
	}
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Error("temperature 为 0 是合法值，必须保留指针")
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "你好" {
		t.Errorf("消息 = %+v", got.Messages)
	}
}

func TestParseRequestClaudeBody(t *testing.T) {
	body := []byte(`{
		"model": "m-1",
		"max_tokens": 32,
		"messages": [{"role": "user", "content": "你好"}]
	}`)

	got, err := ParseRequest(context.Background(), channel.ProtocolAnthropic, body)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Model != "m-1" || got.MaxTokens != 32 {
		t.Errorf("基本字段 = %+v", got)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content[0].Text != "你好" {
		t.Errorf("消息 = %+v", got.Messages)
	}
}

// 不认识的顶层字段要忽略：调用方可能比我们新，因为多了一个字段就拒收，
// 等于把一个非破坏性的加法变成了破坏性的（见 docs/versioning.md）。
func TestParseRequestIgnoresUnknownFields(t *testing.T) {
	body := []byte(`{
		"model": "m-1",
		"messages": [{"role": "user", "content": "hi"}],
		"future_option": {"nested": [1, 2, 3]}
	}`)

	got, err := ParseRequest(context.Background(), channel.ProtocolOpenAI, body)
	if err != nil {
		t.Fatalf("多了一个未知字段就拒收: %v", err)
	}
	if got.Model != "m-1" {
		t.Errorf("Model = %q", got.Model)
	}
}

func TestParseRequestRejectsBadInput(t *testing.T) {
	cases := []struct {
		name     string
		protocol channel.Protocol
		body     string
	}{
		{name: "不是 JSON", protocol: channel.ProtocolOpenAI, body: `{`},
		{name: "不是对象", protocol: channel.ProtocolOpenAI, body: `[1,2,3]`},
		{name: "空体", protocol: channel.ProtocolAnthropic, body: ``},
		{name: "未知协议没有固定形状", protocol: channel.Protocol("custom"), body: `{"model":"m"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRequest(context.Background(), tc.protocol, []byte(tc.body)); err == nil {
				t.Error("应当报错")
			}
		})
	}
}
