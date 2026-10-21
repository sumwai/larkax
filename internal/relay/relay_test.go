package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// 没有工具调用的助手消息，ToolCalls 必须保持为 nil。
//
// 「空切片序列化出来是 null」这件事很容易漏：`len(toolCalls) > 0` 的边界变异体
// 会让这一支永远进入，于是每条纯文本助手消息都挂上 `"null"`，下游按
// 「有没有 ToolCalls」判断时就会把一条普通回答当成工具调用。
func TestAssistantWithoutToolCallsKeepsToolCallsNil(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Messages: []channel.Message{{Role: "assistant", Content: []channel.ContentBlock{
			{Type: "text", Text: "直接回答"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].ToolCalls != nil {
		t.Errorf("ToolCalls = %q，没有工具调用时应当是 nil", out.Messages[0].ToolCalls)
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

// 空 Extra 与没有 Extra 走的是同一条结果路径。
//
// `len(req.Extra) > 0` 的边界变异体（`>= 0`）是等价的：为空时 extrasToRelaykit
// 返回空 map 与 nil 错误，applyExtras 对空 map 的每个键都查不到，什么也不改，
// 两版产出的请求逐字段相同。构造不出能区分两版的输入，所以这里只证明空 Extra
// 的转换结果是确定的，不去追那个分支本身。
func TestEmptyExtraChangesNothing(t *testing.T) {
	out, err := ToRelaykit(&channel.Request{
		Model: "m",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{
			{Type: "text", Text: "你好"},
		}}},
		Extra: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "m" || len(out.Messages) != 1 {
		t.Errorf("请求 = %+v，空 Extra 不该改动任何字段", out)
	}
	if out.TopK != nil || out.FrequencyPenalty != nil || out.PresencePenalty != nil ||
		out.Seed != nil || out.ParallelTooCalls != nil || out.ServiceTier != nil || out.User != nil {
		t.Errorf("空 Extra 不该挂上任何扩展字段: %+v", out)
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

// splitDataURL 里 `semi < 0` 的边界变异体（`<= 0`）是等价的：semi 为 0 意味着
// rest 以 `;base64,` 开头，此时媒体类型是空串，原版走到后面的 `mediaType == ""`
// 同样返回 false；semi 为 -1（没找到）两版都返回 false。其余取值两版一致。
// 没有能区分两版的输入，所以「缺媒体类型」那一行只是覆盖取值为 0 的分支。
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

// derefString 的两侧都要取到：nil 取成空串，有值时取到的是那个值本身。
// 只测 nil 那一侧的话，「有值时也返回空串」这个错误看不见。
func TestDerefString(t *testing.T) {
	if got := derefString(ptr("回答")); got != "回答" {
		t.Errorf("derefString(有值) = %q，期望 回答", got)
	}
	if got := derefString(nil); got != "" {
		t.Errorf("derefString(nil) = %q，期望空串", got)
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

	got, err := ResponseFromRelaykit(src, true)
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

	got, err := ResponseFromRelaykit(src, true)
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

	got, err := ResponseFromRelaykit(src, true)
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

	got, err := ResponseFromRelaykit(src, true)
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

	got, err := ResponseFromRelaykit(src, true)
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

// 响应方向的引用标注要落到正文块上，不能丢。
//
// 非流式那条路以前把它丢了：relaykit 的 Responses→Chat 转换把上游
// content[].annotations 折成 dto.Message.Annotations，而 messageFromRelaykit
// 只读 Role / Content / ToolCalls，于是「非流式有标注」这件事在两个入口上都
// 不成立。这条用例钉住转换层——流式那条另有用例（见 responsesadapter 与 api）。
func TestResponseAnnotationsReachContentBlock(t *testing.T) {
	src := &dto.OpenAIResponsesResponse{
		ID: "resp-1",
		Output: []dto.ResponsesOutput{{
			Type: "message",
			Role: "assistant",
			Content: []dto.ResponsesOutputContent{{
				Type: "output_text",
				Text: "你好",
				Annotations: []any{map[string]any{
					"type": "url_citation", "url": "https://example.com/a",
				}},
			}},
		}},
	}
	got, err := ResponseFromRelaykit(src, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Message.Content) != 1 {
		t.Fatalf("内容块 = %d 块，期望 1 块: %+v", len(got.Message.Content), got.Message.Content)
	}
	block := got.Message.Content[0]
	if block.Type != "text" || block.Text != "你好" {
		t.Fatalf("内容块 = %+v，期望一段正文", block)
	}
	if len(block.Annotations) != 1 {
		t.Fatalf("标注 = %d 条，期望 1 条: %+v", len(block.Annotations), block.Annotations)
	}
	// 形状照 relaykit 的转换结果断言，不是照上游那一份：它把 url_citation
	// 折成了 {"type":"url_citation","url_citation":{…}}（chat 那套非官方扩展
	// 的形状，见 relayconvert 的 responseAnnotationToChat）。这里钉的是
	// 「标注到了正文块上」，而**到的是 relaykit 认的那个形状**——按上游的
	// 扁平形状断言会对着一个真实链路里不存在的中间态。
	//
	// 折过的那一层不会露给客户端：Responses 这条入口把它摊平成协议本身定义的
	// 那一份扁平形状，流式与非流式因此是同一个（见 internal/api 的
	// responsesAnnotation）。本包不解释标注内容，所以摊平不在这里做（见
	// attachAnnotations）。
	if got := block.Annotations[0]["type"]; got != "url_citation" {
		t.Errorf("标注的 type = %v，期望 url_citation", got)
	}
	citation, ok := block.Annotations[0]["url_citation"].(map[string]any)
	if !ok {
		t.Fatalf("标注里没有 url_citation 对象: %+v", block.Annotations[0])
	}
	if citation["url"] != "https://example.com/a" {
		t.Errorf("引用的 url = %v，期望 https://example.com/a", citation["url"])
	}
}

// 没有正文块时不凭空造一个去装标注。
//
// 纯工具调用的回答里没有正文，标注也就没有可挂的地方——补一个空正文块会让
// 客户端看到一条并不存在的 assistant 消息。
func TestResponseAnnotationsNeedATextBlock(t *testing.T) {
	src := &dto.OpenAIResponsesResponse{
		ID: "resp-1",
		Output: []dto.ResponsesOutput{{
			Type: "message",
			Role: "assistant",
			Content: []dto.ResponsesOutputContent{{
				Type:        "output_text",
				Text:        "",
				Annotations: []any{map[string]any{"type": "url_citation"}},
			}},
		}},
	}
	got, err := ResponseFromRelaykit(src, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got.Message.Content {
		if len(b.Annotations) != 0 {
			t.Errorf("没有正文时标注不该挂到 %q 块上: %+v", b.Type, b.Annotations)
		}
	}
}

// 空的标注对象不是一条标注，要丢掉；丢掉之后剩下的照常挂。
//
// `!ok || len(obj) == 0` 的与/或互换变异体会把空对象也收进来，标注张数因此
// 会多算一条——客户端拿到一个没有任何字段的引用。
func TestAnnotationsSkipEmptyObjects(t *testing.T) {
	got, err := messageFromRelaykit(dto.Message{
		Role:        "assistant",
		Content:     "正文",
		Annotations: json.RawMessage(`[{}, 42, {"type":"url_citation","url":"https://example.com/a"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("内容块 = %d，期望 1: %+v", len(got.Content), got.Content)
	}
	annotations := got.Content[0].Annotations
	if len(annotations) != 1 {
		t.Fatalf("标注 = %d 条，期望 1（空对象与标量都不是标注）: %+v", len(annotations), annotations)
	}
	if annotations[0]["type"] != "url_citation" {
		t.Errorf("标注 = %+v，期望剩下的是那条 url_citation", annotations[0])
	}
}

// 标注只挂正文块。工具结果块也带 Text，但它不是正文——
// 把 `Type == "text" && Text != ""` 的与换成或，标注会落到 tool_result 上。
func TestAnnotationsOnlyOnTextBlocks(t *testing.T) {
	got, err := messageFromRelaykit(dto.Message{
		Role:        "tool",
		ToolCallId:  "call_1",
		Content:     "工具结果",
		Annotations: json.RawMessage(`[{"type":"url_citation","url":"https://example.com/a"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "tool_result" {
		t.Fatalf("内容块 = %+v，期望一块 tool_result", got.Content)
	}
	if len(got.Content[0].Annotations) != 0 {
		t.Errorf("标注挂到了工具结果块上: %+v", got.Content[0].Annotations)
	}
}

// 没有 tool_call_id 的 tool 消息不改成 tool_result。
//
// `Role == "tool" && ToolCallId != ""` 的与换成或之后，这种消息会被包成一个
// ToolUseID 为空的工具结果块，工具往返从此对不上号。
func TestToolMessageWithoutCallIDStaysText(t *testing.T) {
	got, err := messageFromRelaykit(dto.Message{Role: "tool", Content: "没有关联的文本"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("内容块 = %d，期望 1: %+v", len(got.Content), got.Content)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "没有关联的文本" {
		t.Errorf("内容块 = %+v，期望原样的文本块", got.Content[0])
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

	got, err := ResponseFromRelaykit(src, true)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.FinishReason != "length" {
		t.Errorf("FinishReason = %q", got.FinishReason)
	}
}

// 上游给出了空内容时，响应照常产出：没有正文，用量一定要保留。
//
// OpenAI 那一侧的 Choices 可以为空，`len(r.Choices) > 0` 的边界变异体会让它
// 取下标 0 而 panic——真实上游在内容被过滤时就会给空 choices，那正是不该拿不到
// 用量的时刻。
//
// Responses 那一侧的 `len(chat.Choices) > 0` 在可及输入上恒真：relaykit 的
// ResponsesResponseToChatCompletionsResponse 对任何非 nil 响应都构造恰好一个
// choice（空 output 也一样，结束原因折成 stop），nil 已在函数开头拦掉。所以
// 那个边界变异体与本版等价，构造不出能区分两版的输入；这一侧的子用例只把
// 「空 output 也有一个 choice」这件事钉住。
func TestResponseWithoutChoices(t *testing.T) {
	t.Run("OpenAI", func(t *testing.T) {
		got, err := ResponseFromRelaykit(&dto.OpenAITextResponse{
			Model: "gpt-4o",
			Usage: dto.Usage{PromptTokens: 10},
		}, true)
		if err != nil {
			t.Fatalf("空 choices 不该让响应失败: %v", err)
		}
		if got.FinishReason != "" {
			t.Errorf("FinishReason = %q，期望空", got.FinishReason)
		}
		if len(got.Message.Content) != 0 {
			t.Errorf("内容 = %+v，期望空", got.Message.Content)
		}
		if got.Usage.PromptTokens != 10 {
			t.Errorf("PromptTokens = %d，期望 10（空 choices 也要保住用量）", got.Usage.PromptTokens)
		}
	})

	t.Run("Responses", func(t *testing.T) {
		got, err := ResponseFromRelaykit(&dto.OpenAIResponsesResponse{
			ID:    "resp_empty",
			Model: "gpt-5",
			Usage: &dto.Usage{PromptTokens: 12, CompletionTokens: 3},
		}, true)
		if err != nil {
			t.Fatalf("空 output 不该让响应失败: %v", err)
		}
		if got.FinishReason != "stop" {
			t.Errorf("FinishReason = %q，期望 stop（relaykit 对空 output 折成 stop）", got.FinishReason)
		}
		if len(got.Message.Content) != 0 {
			t.Errorf("内容 = %+v，期望空", got.Message.Content)
		}
		if got.Usage.PromptTokens != 12 || got.Usage.CompletionTokens != 3 {
			t.Errorf("用量 = %+v，期望 12 / 3（空 output 也要保住用量）", got.Usage)
		}
	})
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
	if _, err := ResponseFromRelaykit("字符串", true); err == nil {
		t.Error("未知类型应报错")
	}
	if _, err := ResponseFromRelaykit((*dto.OpenAITextResponse)(nil), true); err == nil {
		t.Error("nil 响应应报错")
	}
	if _, err := ResponseFromRelaykit((*dto.ClaudeResponse)(nil), true); err == nil {
		t.Error("nil 响应应报错")
	}
	if _, err := ResponseFromRelaykit((*dto.OpenAIResponsesResponse)(nil), true); err == nil {
		t.Error("nil 响应应报错")
	}
}

// ── 用量口径 ────────────────────────────────────────────────────────────

func TestUsageFromRelaykitNil(t *testing.T) {
	got := UsageFromRelaykit(nil, true)
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
	}, true)
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
	}, true)
	if got.PromptTokens != 40 {
		t.Errorf("PromptTokens = %d，期望 40", got.PromptTokens)
	}
}

// 缓存写入的两个来源字段要相加，三种输入口径的合成结果各自正确。
//
// 三种口径放在同一张表里，是为了让「同一笔用量在三种声明下各是多少」可以直接
// 对照：分岔只在 prompt_tokens 含不含缓存这一件事上，缓存明细三种口径**都应当
// 相同**。断言的是具体数，不是「非零」——这一包算错的表现是账偏，而不是报错。
//
// 这张表同时钉住那条兜底条件：只有缓存写入（读为 0、写为正）时，prompt 仍要
// 由缓存写入量兜底，不能因为读那一项为 0 就整条分支失效。
//
// 同一处两个边界变异体（`cacheRead > 0` / `cacheWrite > 0` 改成 `>= 0`）
// **只在计数非负时**两版一致：进入分支时 prompt 为 0，分支体把它赋成
// cacheRead + cacheWrite；某一项为 0 时另一项为正则原版本就进入分支（赋出
// 同一个数），两项都为 0 则赋进去的也是 0（与不进分支相同）。
//
// 负值能走到这里，不能拿「非负」当前提：openaiadapter 对这三个缓存字段
// 只做指针解引用，不校验符号（internal/channel/openaiadapter/dto.go 里
// CachedTokens / CachedCreationTokens / CacheWriteTokens 的拷贝）。负值下
// 两版会分岔——某一项为 0、另一项为负时，变异版把它当 `>= 0` 放行并算出
// 负的 prompt。下面两个「负的缓存」子用例钉住这一格：prompt 保持 0，
// 而负值本身原样透传到 CacheReadTokens / CacheWriteTokens。
//
// 透传是当前行为，不是本包该定的口径：要不要把负计数钳到 0 属于计费层的
// 决定，这一条只把它钉住，不改源码。
func TestUsageCacheAccountingDialects(t *testing.T) {
	cases := []struct {
		name      string
		usage     dto.Usage
		wantProm  int64
		wantRead  int64
		wantWrite int64
	}{
		{
			name: "openai 原生：prompt_tokens 已含缓存，两个缓存写字段相加",
			usage: dto.Usage{
				PromptTokens:     9000,
				CompletionTokens: 200,
				PromptTokensDetails: dto.InputTokenDetails{
					CachedTokens:         8000,
					CachedCreationTokens: 200,
					CacheWriteTokens:     50,
				},
				UsageSemantic: dto.BillingUsageSemanticOpenAI,
			},
			wantProm: 9000, wantRead: 8000, wantWrite: 250,
		},
		{
			name: "anthropic：prompt_tokens 不含缓存，补回读 + 写",
			usage: dto.Usage{
				PromptTokens:     1000,
				CompletionTokens: 200,
				PromptTokensDetails: dto.InputTokenDetails{
					CachedTokens:         8000,
					CachedCreationTokens: 200,
					CacheWriteTokens:     50,
				},
				UsageSemantic: dto.BillingUsageSemanticAnthropic,
			},
			wantProm: 9250, wantRead: 8000, wantWrite: 250,
		},
		{
			name: "只有缓存字段：prompt 由读 + 写兜底",
			usage: dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 7, CachedCreationTokens: 3},
			},
			wantProm: 10, wantRead: 7, wantWrite: 3,
		},
		{
			name: "只有缓存写入：prompt 仍要由写作兜底",
			usage: dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: 3},
			},
			wantProm: 3, wantRead: 0, wantWrite: 3,
		},
		{
			name: "负的缓存写入不参与兜底：prompt 保持 0",
			usage: dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: -3},
			},
			wantProm: 0, wantRead: 0, wantWrite: -3,
		},
		{
			name: "负的缓存读取不参与兜底：prompt 保持 0",
			usage: dto.Usage{
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: -3},
			},
			wantProm: 0, wantRead: -3, wantWrite: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UsageFromRelaykit(&tc.usage, true)
			if got.PromptTokens != tc.wantProm {
				t.Errorf("PromptTokens = %d，期望 %d", got.PromptTokens, tc.wantProm)
			}
			if got.CacheReadTokens != tc.wantRead {
				t.Errorf("CacheReadTokens = %d，期望 %d", got.CacheReadTokens, tc.wantRead)
			}
			if got.CacheWriteTokens != tc.wantWrite {
				t.Errorf("CacheWriteTokens = %d，期望 %d", got.CacheWriteTokens, tc.wantWrite)
			}
			if want := int64(tc.usage.CompletionTokens); got.CompletionTokens != want {
				t.Errorf("CompletionTokens = %d，期望 %d", got.CompletionTokens, want)
			}
		})
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
	}, true)
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
	}, true)
	if got.Raw["estimated"] != true {
		t.Errorf("Raw 应标记 estimated: %+v", got.Raw)
	}
}

// 「上游报没报 usage」归一化之后有两个落点：计费读的 Reported，与排查读的
// Raw["source"]。
//
// internal/gateway 的 usageSourceOf 靠 Reported 把「上游报了 0」与「压根没报」
// 分开（两者用量逐列相同），所以它的出现条件要钉住：**适配器解析到了 usage
// 块**（给了 UsageSource）才是 true，没给就是 false。Raw 里那个键只是排查用的
// 副本，两者必须同进同退——但判据在 Reported 上，删掉/改名 Raw 不会让来源
// 静默翻面。
//
// 这里不验「适配器什么时候给 UsageSource」——那是各适配器自己的用例；
// 这条只管归一化这一层不会把两者写成同一种形状。
func TestUsageRawCarriesSourceMarker(t *testing.T) {
	reported := UsageFromRelaykit(&dto.Usage{
		PromptTokens: 0, CompletionTokens: 0,
		UsageSource: "openai_chat",
	}, true)
	if got, _ := reported.Raw["source"].(string); got != "openai_chat" {
		t.Errorf("报过 usage 时 Raw[source] = %v，期望 openai_chat", reported.Raw["source"])
	}

	// Reported 才是计费来源的判据（见 gateway.usageSourceOf）；Raw 里的
	// source 只是排查用的副本，两者必须同时成立、同时缺席。
	if !reported.Reported {
		t.Error("报过 usage 时 Reported 应为 true")
	}

	absent := UsageFromRelaykit(&dto.Usage{}, true)
	if _, ok := absent.Raw["source"]; ok {
		t.Errorf("没报 usage 时 Raw 不该有 source: %+v", absent.Raw)
	}
	if absent.Reported {
		t.Error("没报 usage 时 Reported 应为 false")
	}

	// nil 与「零值 dto.Usage」是同一件事：上游整块没报。
	if (UsageFromRelaykit(nil, true)).Reported {
		t.Error("usage 为 nil 时 Reported 应为 false")
	}
}

func TestUsageCacheCreationSplit(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{
		PromptTokens:                1,
		ClaudeCacheCreation5mTokens: 100,
		ClaudeCacheCreation1hTokens: 200,
	}, true)
	if got.UnitCounts["cache_write_5m"] != 100 || got.UnitCounts["cache_write_1h"] != 200 {
		t.Errorf("缓存写入拆分 = %+v", got.UnitCounts)
	}
}

// 没有 5m/1h 拆分时 UnitCounts 应当是 nil 而不是空 map：空 map 会被
// 序列化成 {}，在账上看起来像「报了拆分但每一项都是 0」。
func TestUsageUnitCountsNilWithoutSplit(t *testing.T) {
	got := UsageFromRelaykit(&dto.Usage{PromptTokens: 1}, true)
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
			got := UsageFromRelaykit(tc.usage, true)
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
	}, true)
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
	got, err := ResponseFromRelaykit(src, true)
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
	got := UsageFromRelaykit(&dto.Usage{PromptTokens: 7}, true)
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
	got := UsageFromRelaykit(&dto.Usage{}, true)
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
	}, true)
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
	}, true)
	openai := UsageFromRelaykit(&dto.Usage{
		PromptTokens:        9000, // OpenAI 的 prompt_tokens 已含缓存
		CompletionTokens:    200,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
		UsageSemantic:       dto.BillingUsageSemanticOpenAI,
	}, true)
	// Responses 的 input_tokens 与 OpenAI 的 prompt_tokens 同义（都含缓存），
	// 所以它与 openai 那一支归一后应当是同一个数——它是**同一族**，
	// 而不是像 Anthropic 那样要补一次缓存。
	responses := UsageFromRelaykit(&dto.Usage{
		PromptTokens:        9000,
		CompletionTokens:    200,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 8000},
		UsageSource:         dto.BillingUsageSourceOAIResponses,
		UsageSemantic:       dto.BillingUsageSemanticOpenAI,
	}, true)

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

// 非 function 的工具声明必须在解析层拒掉。
//
// 不拒的表现是**静默改写**：channel.Tool 没有承载工具类型的字段，于是
// {"type":"mcp"} 与 {"type":"web_search"} 都会被变成同一个
// {"parameters":null,"type":"function"}（名字还被 omitempty 掉）。上游收到
// 一个我们伪造的、谁都认不出的工具；而远端 MCP 在上游一律 400——错误信息
// 指向的是我们发出去的请求，排查方向会落到对方身上。
//
// 三档判据各有用例：显式 function 放行、**缺省 type 放行**、其余拒绝。
// 中间那一档最容易被下一个人「顺手统一掉」，所以它单独占一条：省略 type 的
// 客户端今天能用，拒掉它是把一个能用的形状改坏，不是为了修 bug。
func TestParseRequestRejectsNonFunctionTools(t *testing.T) {
	cases := []struct {
		name string
		// tools 是请求体里 tools 字段的原文。
		tools string
		// wantErrType 非空表示应当报错，且错误里要出现这个类型名。
		wantErrType string
	}{
		{
			name:        "远端 MCP",
			tools:       `[{"type":"mcp","server_label":"x","server_url":"https://mcp.example.com"}]`,
			wantErrType: "mcp",
		},
		{name: "内置工具", tools: `[{"type":"web_search"}]`, wantErrType: "web_search"},
		{name: "自定义工具类型", tools: `[{"type":"custom","name":"f"}]`, wantErrType: "custom"},
		{
			name:  "显式 function 是最常见的那条路",
			tools: `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`,
		},
		{
			name:  "缺省 type 按 function 用",
			tools: `[{"function":{"name":"lookup","parameters":{"type":"object"}}}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":` + tc.tools + `}`
			req, err := ParseRequest(context.Background(), channel.ProtocolOpenAI, []byte(body))

			if tc.wantErrType != "" {
				if err == nil {
					t.Fatalf("应当报错，否则它会被静默改写成 function 工具：%+v", req.Tools)
				}
				if !strings.Contains(err.Error(), tc.wantErrType) {
					t.Errorf("错误里要带上收到的类型 %q，否则调用方不知道改哪里：%v", tc.wantErrType, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
				t.Fatalf("工具 = %+v，期望一个叫 lookup 的工具", req.Tools)
			}
			if _, ok := req.Tools[0].InputSchema["type"]; !ok {
				t.Errorf("schema 要原样留下，否则上游收到一个没有参数的工具：%+v", req.Tools[0].InputSchema)
			}
		})
	}
}

// ── OpenAI Responses 入口的请求体 ──────────────────────────────────────

// Responses 的请求体与另外两条差得远（input / instructions / 平铺的工具声明），
// 它走的是 parseResponsesRequest 那一条。这里钉住的是「转换之后落进管道的东西
// 对不对」，而不是 relaykit 自己怎么转的——那有它自己的用例。
func TestParseResponsesRequest(t *testing.T) {
	body := `{"model":"m-1","instructions":"你是助手","input":"hi",` +
		`"max_output_tokens":32,"stream":true}`

	req, err := ParseRequest(context.Background(), channel.ProtocolOpenAIResponses, []byte(body))
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if req.Model != "m-1" {
		t.Errorf("Model = %q", req.Model)
	}
	if !req.Stream {
		t.Error("stream=true 没被读出来，这条请求会按非流式发出去")
	}
	// max_output_tokens 经 relaykit 落在 chat 的 MaxCompletionTokens 上，
	// 只读 MaxTokens 的话它会静默丢掉——而预留要有输出上界才做得出来，
	// 表现为每一个 Responses 请求都以「max_tokens 缺失」被拒。
	if req.MaxTokens != 32 {
		t.Errorf("MaxTokens = %d，期望 32（max_output_tokens 要变成输出上界）", req.MaxTokens)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("消息 = %+v，期望 instructions 与 input 各一条", req.Messages)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content[0].Text != "你是助手" {
		t.Errorf("instructions 应折成 system 消息: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content[0].Text != "hi" {
		t.Errorf("input 字符串应折成 user 消息: %+v", req.Messages[1])
	}
}

// input 用数组形态（带类型的 item）也要能解析：这是 Responses 最常见的写法。
func TestParseResponsesRequestInputItems(t *testing.T) {
	body := `{"model":"m-1","max_output_tokens":16,"input":[` +
		`{"role":"user","content":"hi"},` +
		`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"42"}]}`

	req, err := ParseRequest(context.Background(), channel.ProtocolOpenAIResponses, []byte(body))
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	var roles []string
	var toolUse, toolResult bool
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				toolUse = b.ToolUseID == "call_1" && b.ToolName == "lookup"
			case "tool_result":
				toolResult = b.ToolUseID == "call_1"
			}
		}
	}
	if !toolUse {
		t.Errorf("function_call 应折成 tool_use 块: %+v", req.Messages)
	}
	if !toolResult {
		t.Errorf("function_call_output 应折成 tool_result 块: %+v", req.Messages)
	}
	if len(roles) < 3 {
		t.Errorf("消息 = %v，期望至少三条", roles)
	}
}

// 有状态的字段一律拒绝，不静默丢弃。
//
// 它们会让输入里含**上游持有的历史**，而本项目按「输入全在请求体里」估算
// token 做预留。估算低估的后果是预留不足、结算超支，所以宁可让请求失败。
func TestParseResponsesRequestRejectsStatefulFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "previous_response_id",
			body: `{"model":"m-1","input":"hi","max_output_tokens":16,"previous_response_id":"resp_1"}`,
		},
		{
			name: "conversation",
			body: `{"model":"m-1","input":"hi","max_output_tokens":16,"conversation":"conv_1"}`,
		},
		{
			name: "context_management",
			body: `{"model":"m-1","input":"hi","max_output_tokens":16,"context_management":[]}`,
		},
		{
			name: "prompt",
			body: `{"model":"m-1","input":"hi","max_output_tokens":16,"prompt":{"id":"pmpt_1"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseRequest(context.Background(), channel.ProtocolOpenAIResponses, []byte(tc.body))
			if err == nil {
				t.Fatalf("应当报错，否则输入里会有一段我们看不见的历史: %+v", req.Messages)
			}
		})
	}
}

// store 是**丢弃**而不是拒绝：它只表达「上游可以留存这次请求」，不影响我们
// 这边的输入量，也不影响上游这次要算什么。这一条钉住的是「它不会被当成
// 有状态字段一起拒掉」——拒掉会让一批能用的请求失败。
func TestParseResponsesRequestDropsStore(t *testing.T) {
	body := `{"model":"m-1","input":"hi","max_output_tokens":16,"store":true}`
	if _, err := ParseRequest(context.Background(), channel.ProtocolOpenAIResponses, []byte(body)); err != nil {
		t.Fatalf("store 不该让请求失败: %v", err)
	}
}

// 非 function 的工具声明必须在**解析这一步**拒掉。
//
// 与 chat 入口那条不同：这里不拒的表现不是改写，是**静默消失**——relaykit
// 的转换先经 toolconv 抽出工具、再按 chat 格式挂回去，表达不了的类型直接
// 丢掉，请求照常成功。调用方拿到 200、工具一次都不会被调用。
//
// 判据复用 checkToolType，所以三档（显式 function 放行、缺省 type 放行、
// 其余拒绝）与 chat 入口完全一致。
func TestParseResponsesRequestRejectsNonFunctionTools(t *testing.T) {
	cases := []struct {
		name        string
		tools       string
		wantErrType string
	}{
		{
			name:        "远端 MCP",
			tools:       `[{"type":"mcp","server_label":"x","server_url":"https://mcp.example.com"}]`,
			wantErrType: "mcp",
		},
		{name: "内置工具", tools: `[{"type":"web_search"}]`, wantErrType: "web_search"},
		{name: "自定义工具类型", tools: `[{"type":"custom","name":"f"}]`, wantErrType: "custom"},
		// 与 chat 入口**相反**的一档：那边缺 type 放行（解出空串照样能转发），
		// 这边缺了它整条声明会被 toolconv 丢掉，所以必须拒。
		{name: "缺 type", tools: `[{"name":"lookup","parameters":{"type":"object"}}]`, wantErrType: "type"},
		{
			name:  "显式 function 是最常见的那条路",
			tools: `[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m-1","input":"hi","max_output_tokens":16,"tools":` + tc.tools + `}`
			req, err := ParseRequest(context.Background(), channel.ProtocolOpenAIResponses, []byte(body))

			if tc.wantErrType != "" {
				if err == nil {
					t.Fatalf("应当报错，否则这个工具会被静默丢掉而请求照常成功：%+v", req.Tools)
				}
				if !strings.Contains(err.Error(), tc.wantErrType) {
					t.Errorf("错误里要带上收到的类型 %q，否则调用方不知道改哪里：%v", tc.wantErrType, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
				t.Fatalf("工具 = %+v，期望一个叫 lookup 的工具", req.Tools)
			}
			if _, ok := req.Tools[0].InputSchema["type"]; !ok {
				t.Errorf("schema 要原样留下，否则上游收到一个没有参数的工具：%+v", req.Tools[0].InputSchema)
			}
		})
	}
}

// 空 tools 数组与完全不给 tools 都不该报错。
func TestParseResponsesRequestWithoutTools(t *testing.T) {
	for _, body := range []string{
		`{"model":"m-1","input":"hi","max_output_tokens":16}`,
		`{"model":"m-1","input":"hi","max_output_tokens":16,"tools":[]}`,
	} {
		req, err := ParseRequest(context.Background(), channel.ProtocolOpenAIResponses, []byte(body))
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if len(req.Tools) != 0 {
			t.Errorf("工具 = %+v，期望没有", req.Tools)
		}
	}
}

// unrecognizedContent 是一个本项目没有枚举的内容承载形态，用来钉住兜底报错。
// 真实世界里它是「客户端给了个我们还没见过的形状」。
type unrecognizedContent struct{ Text string }

// 块数组形态的 content 在**三条入口**上都要转出来。
//
// 三条入口的承载类型不一样，而其中一条曾经整条丢掉：Anthropic 请求先经
// relaykit 转成 OpenAI 形状，那一步用 dto.Message.SetMediaContent 挂内容，
// 于是 m.Content 是带类型的 []dto.MediaContent，而不是 JSON 反序列化出来的
// []any。认不出这一种时**没有任何一层报错**——文本与图片一起消失，转发给
// 上游的是空 content，而调用方拿到的是「模型答非所问」。
//
// 三条分别钉住：
//
//   - Anthropic：带类型的那一种承载（本组用例里唯一会红过的一条）；
//   - OpenAI：[]any 那一种，防回归对照；
//   - Responses：另一条转换链（parseResponsesRequest 经
//     ResponsesRequestToChatCompletionsRequest），它给出的同样是 []any，
//     所以走的是与 OpenAI 相同的那条分支——这条用例钉的是«它不要漂到
//     带类型那一种上去»，而不是«它今天坏着»。
func TestParseRequestBlockArrayContent(t *testing.T) {
	cases := []struct {
		name     string
		protocol channel.Protocol
		body     string
	}{
		{
			name:     "anthropic",
			protocol: channel.ProtocolAnthropic,
			body: `{"model":"m-1","max_tokens":64,"messages":[{"role":"user","content":[
				{"type":"text","text":"你好"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
			]}]}`,
		},
		{
			name:     "openai",
			protocol: channel.ProtocolOpenAI,
			body: `{"model":"m-1","messages":[{"role":"user","content":[
				{"type":"text","text":"你好"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,aGVsbG8="}}
			]}]}`,
		},
		{
			name:     "openai-responses",
			protocol: channel.ProtocolOpenAIResponses,
			body: `{"model":"m-1","max_output_tokens":64,"input":[{"role":"user","content":[
				{"type":"input_text","text":"你好"},
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}
			]}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRequest(context.Background(), tc.protocol, []byte(tc.body))
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if len(got.Messages) != 1 {
				t.Fatalf("消息 = %+v，期望一条", got.Messages)
			}
			blocks := got.Messages[0].Content
			if len(blocks) != 2 {
				t.Fatalf("内容块 = %+v，期望文本与图片各一块（少了就是被静默丢了）", blocks)
			}
			if blocks[0].Type != "text" || blocks[0].Text != "你好" {
				t.Errorf("文本块 = %+v", blocks[0])
			}
			if blocks[1].Type != "image" || blocks[1].ImageBase64 != "aGVsbG8=" || blocks[1].ImageMediaType != "image/png" {
				t.Errorf("图片块 = %+v", blocks[1])
			}
		})
	}
}

// 认不出的内容承载形态必须报错，不能静默跳过。
//
// 这条兜底针对的正是上面那类事故：在代码里「没命中任何 case」与「这条消息
// 本来就没内容」长得一模一样，于是整条内容可以一直丢着而没人发现。报错时
// 带上实际类型 %T——只说「不支持」的话，排查时不知道不支持的是什么。
//
// 两个形态**必须放过**，它们不是「认不出」，是「本来就没内容」：
// content 为 nil（只有工具调用的助手消息）与空字符串。把它们判成错误，
// 等于把一批正常请求拒之门外。
func TestFromRelaykitContentShapes(t *testing.T) {
	cases := []struct {
		name       string
		content    any
		wantBlocks int
		// wantErrType 非空表示这个形态必须报错，且错误里要带上这个 %T 渲染值。
		wantErrType string
	}{
		{name: "nil（只有工具调用的助手消息）", content: nil},
		{name: "空字符串", content: ""},
		{name: "字符串", content: "文本", wantBlocks: 1},
		{
			name:       "JSON 反序列化出来的块数组",
			content:    []any{map[string]any{"type": "text", "text": "文本"}},
			wantBlocks: 1,
		},
		{
			name:       "relaykit 的带类型块数组",
			content:    []dto.MediaContent{{Type: "text", Text: "文本"}},
			wantBlocks: 1,
		},
		{
			name:    "对象",
			content: map[string]any{"type": "text"},
			// 不是数组的对象：今天没有任何一层会产出它，认不出就要说得出类型。
			wantErrType: "map[string]interface {}",
		},
		{name: "数字", content: 42, wantErrType: "int"},
		{name: "自定义类型", content: unrecognizedContent{}, wantErrType: "relay.unrecognizedContent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := messageFromRelaykit(dto.Message{Role: "user", Content: tc.content})

			if tc.wantErrType != "" {
				if err == nil {
					t.Fatalf("应当报错，否则这个形态的内容会被静默丢掉: %+v", got.Content)
				}
				if !strings.Contains(err.Error(), tc.wantErrType) {
					t.Errorf("错误里要带上实际类型 %q，否则排查时不知道不支持的是什么: %v", tc.wantErrType, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if len(got.Content) != tc.wantBlocks {
				t.Errorf("内容块 = %+v，期望 %d 块", got.Content, tc.wantBlocks)
			}
		})
	}
}

// 响应方向遇到认不出的内容形状**不能失败**，判据与请求方向不同。
//
// 理由具体，不是口味：out.Usage 在调用 messageFromRelaykit **之前**就填好了，
// 而 return nil, err 会把已经解析出来的用量一起丢掉——这一次调用的账就漏了，
// 而调用方多半会重试一次、上游再收一次钱。相比之下「回答是空的」调用方当场
// 就能看见，不是静默的。完整理由见 response.go 里那两处调用点的注释。
//
// 这条用例钉住三件事：响应照常返回、内容为空、Usage 与 FinishReason 完好。
// 请求方向那一条判据相反的用例见 TestFromRelaykitContentShapes。
func TestResponseKeepsUsageOnUnknownContentShape(t *testing.T) {
	src := &dto.OpenAITextResponse{
		Model: "gpt-4o",
		Choices: []dto.OpenAITextResponseChoice{{
			Message:      dto.Message{Role: "assistant", Content: unrecognizedContent{Text: "x"}},
			FinishReason: "stop",
		}},
		Usage: dto.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	got, err := ResponseFromRelaykit(src, true)
	if err != nil {
		t.Fatalf("响应方向不该因为认不出的内容形状失败，用量会跟着一起丢: %v", err)
	}
	if got.Usage.PromptTokens != 10 || got.Usage.CompletionTokens != 5 {
		t.Errorf("用量 = %+v，抛错会连它一起丢", got.Usage)
	}
	if got.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", got.FinishReason)
	}
	if len(got.Message.Content) != 0 {
		t.Errorf("内容 = %+v，认不出的形状只该让内容是空的", got.Message.Content)
	}
	if got.Raw == nil {
		t.Error("Raw 应保留上游原样数据，供事后发现口径变化")
	}
}
