package relay

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/channel"
)

func ptr[T any](v T) *T { return &v }

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
		t.Errorf("MaxTokens = %v", out.MaxTokens)
	}
	// Temperature 为 0 是合法取值，必须原样传出去而不是被当成「未设置」
	if out.Temperature == nil || *out.Temperature != 0 {
		t.Errorf("Temperature = %v，期望 0（指针非空）", out.Temperature)
	}
	if out.TopP == nil || *out.TopP != 0.9 {
		t.Errorf("TopP = %v", out.TopP)
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
		t.Errorf("TopK = %v", out.TopK)
	}
	if out.ParallelTooCalls == nil || !*out.ParallelTooCalls {
		t.Errorf("ParallelTooCalls = %v", out.ParallelTooCalls)
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
		channel.ProtocolOpenAI:    "openai",
		channel.ProtocolAnthropic: "claude",
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

// custom 没有固定目标格式，猜一个会掩盖配置错误。
func TestFormatForCustomIsRejected(t *testing.T) {
	if _, err := formatFor(channel.ProtocolCustom); err == nil {
		t.Error("custom 协议应报错，要求适配器显式指定")
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

// 推理 token 只记单独报出来的那部分，不重复计入 completion。
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
	if _, err := FromRelaykitRequest(context.Background(), channel.ProtocolCustom, &dto.GeneralOpenAIRequest{}); err == nil {
		t.Error("custom 协议应报错")
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
		t.Errorf("Temperature = %v", back.Temperature)
	}
	if back.TopP == nil || *back.TopP != 0.95 {
		t.Errorf("TopP = %v", back.TopP)
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
		{name: "custom 没有固定形状", protocol: channel.ProtocolCustom, body: `{"model":"m"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseRequest(context.Background(), tc.protocol, []byte(tc.body)); err == nil {
				t.Error("应当报错")
			}
		})
	}
}
