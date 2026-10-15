// Package relay 是本项目与 relaykit 之间的唯一一道缝。
//
// 为什么要有这道缝，而不是让 relaykit 的类型直接进管道：
//
//   - relaykit 是 v0.x，它自己声明公开 API 可能在小版本之间变化。把它放在核心
//     词汇里，等于每次升级都要在管道的每一层做一次适配。
//   - relaykit 的 dto 包里混着大量 new-api 宿主专用结构（渠道设置、定价、
//     用户设置），那些与本项目无关，却会因为同一个包名而出现在补全列表里。
//   - 本项目自己的类型携带了明确的语义决定（Temperature 用指针区分「0」与
//     「未指定」、内容是有结构的块序列、流式工具调用给字符串片段）。这些决定
//     需要一个能被注释解释清楚的地方。
//
// 代价是每个请求多一次类型转换。那点开销换来的是：升级 relaykit 只改这一个
// 文件，以及管道里所有类型都有本项目的注释可读。
//
// 只在本包 import relaykit。其他包若需要 relaykit 的类型，说明这里缺一个转换函数。
package relay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/sumwai/larkax/internal/channel"
)

// Format 是 relaykit 对协议族的称呼，本包把它映射成本项目的 Protocol。
//
// 只映射本项目真正支持的两套。relaykit 还认 Gemini、Responses、Realtime 等，
// 那些等接进来时再补——提前为没有适配器的协议写映射，只会让「以为支持了」
// 这件事更容易发生。
func formatFor(p channel.Protocol) (types.RelayFormat, error) {
	switch p {
	case channel.ProtocolOpenAI:
		return types.RelayFormatOpenAI, nil
	case channel.ProtocolAnthropic:
		return types.RelayFormatClaude, nil
	default:
		// ProtocolCustom 没有固定的目标格式：它需要具体适配器自己说清楚
		// 落到哪一套。在这里猜一个会掩盖配置错误。
		return "", fmt.Errorf("协议 %q 没有对应的 relaykit 格式，需由适配器显式指定", p)
	}
}

// ToRelaykit 把本项目的请求转成 relaykit 的 OpenAI Chat 请求。
//
// 中间以 OpenAI Chat 为枢纽：relaykit 的转换矩阵从它出发能到 Claude，
// 反向也能回来。用枢纽而不是为「每种源 × 每种目标」各写一条路径，
// 是因为组合数增长得比代码快，而枢纽这一层的损耗已经被 relaykit 处理过
// （它的转换器读过两边的语义差异）。
func ToRelaykit(req *channel.Request) (*dto.GeneralOpenAIRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}

	out := &dto.GeneralOpenAIRequest{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
	}
	if req.MaxTokens > 0 {
		maxTokens := uint(req.MaxTokens)
		out.MaxTokens = &maxTokens
	}
	if req.Stream {
		stream := true
		out.Stream = &stream
	}

	for _, m := range req.Messages {
		converted, err := messageToRelaykit(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, converted)
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, dto.ToolCallRequest{
			Type: "function",
			Function: dto.FunctionRequest{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	if len(req.Extra) > 0 {
		extra, err := extrasToRelaykit(req.Extra)
		if err != nil {
			return nil, err
		}
		// 上游特有参数逐个挂上去。挂在已知字段之后，因此不会覆盖本项目
		// 明确设过的值——Extra 的定位是补充，不是覆盖。
		applyExtras(out, extra)
	}
	return out, nil
}

// messageToRelaykit 转换一条消息。
func messageToRelaykit(m channel.Message) (dto.Message, error) {
	out := dto.Message{Role: m.Role}

	// 工具结果是独立的一条消息（role=tool），它的 tool_call_id 挂在消息上而不是内容里。
	// 这与「内容块」的模型不同，需要单独处理，否则工具往返会丢掉关联关系。
	var (
		textParts []string
		media     []dto.MediaContent
		toolCalls []dto.ToolCallResponse
		toolResID string
	)
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
			media = append(media, dto.MediaContent{Type: "text", Text: b.Text})
		case "image":
			media = append(media, imageBlock(b))
		case "tool_use":
			args, err := json.Marshal(b.ToolInput)
			if err != nil {
				return out, fmt.Errorf("工具 %q 的参数无法序列化: %w", b.ToolName, err)
			}
			toolCalls = append(toolCalls, dto.ToolCallResponse{
				ID:   b.ToolUseID,
				Type: "function",
				Function: dto.FunctionResponse{
					Name:      b.ToolName,
					Arguments: string(args),
				},
			})
		case "tool_result":
			toolResID = b.ToolUseID
			// 工具结果的内容按文本处理。它可能是结构化数据，但上游协议里
			// 这一段的传输形式就是文本或内容块序列，保持原样即可。
			textParts = append(textParts, b.Text)
			media = append(media, dto.MediaContent{Type: "text", Text: b.Text})
		case "thinking":
			// 思考内容不进 messages：它不是对话的一部分，而是模型的中间产物。
			// 上游对它的处理方式各不相同（有的要单独字段回传，有的直接丢弃），
			// 由适配器按需要决定，这里不猜。
		default:
			return out, fmt.Errorf("未知的内容块类型 %q", b.Type)
		}
	}

	switch m.Role {
	case "tool":
		out.ToolCallId = toolResID
		out.Content = joinOrBlocks(textParts, media)
	case "assistant":
		if len(toolCalls) > 0 {
			raw, err := json.Marshal(toolCalls)
			if err != nil {
				return out, fmt.Errorf("工具调用无法序列化: %w", err)
			}
			out.ToolCalls = raw
		}
		out.Content = joinOrBlocks(textParts, media)
	default:
		out.Content = joinOrBlocks(textParts, media)
	}
	return out, nil
}

// joinOrBlocks 在「纯文本」与「内容块序列」之间选择传输形式。
//
// 单块纯文本用字符串：那是各家 SDK 最普遍接受的形态，也能让日志与
// 结构快照读起来干净。一旦含图片或有多块，就必须用块序列，否则信息会丢。
func joinOrBlocks(parts []string, media []dto.MediaContent) any {
	if len(media) == 1 && media[0].Type == "text" {
		return media[0].Text
	}
	if len(media) == 0 {
		if len(parts) == 0 {
			return ""
		}
		return parts[0]
	}
	return media
}

func imageBlock(b channel.ContentBlock) dto.MediaContent {
	switch {
	case b.ImageURL != "":
		return dto.MediaContent{
			Type:     "image_url",
			ImageUrl: map[string]any{"url": b.ImageURL},
		}
	case b.ImageBase64 != "":
		url := "data:" + b.ImageMediaType + ";base64," + b.ImageBase64
		return dto.MediaContent{
			Type:     "image_url",
			ImageUrl: map[string]any{"url": url},
		}
	default:
		return dto.MediaContent{Type: "image_url"}
	}
}

func extrasToRelaykit(extra map[string]any) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(extra))
	for k, v := range extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("扩展参数 %q 无法序列化: %w", k, err)
		}
		out[k] = raw
	}
	return out, nil
}

// applyExtras 把扩展参数挂到请求上。
//
// 只认已知的白名单字段。未知的键被忽略而不是塞进某个 RawMessage：
// 透传任意键意味着上游能通过 Extra 影响本项目的请求语义，
// 而那正是「插件不能改钱」这条边界的旁边。
func applyExtras(out *dto.GeneralOpenAIRequest, extra map[string]json.RawMessage) {
	if v, ok := extra["top_k"]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil {
			out.TopK = &n
		}
	}
	if v, ok := extra["frequency_penalty"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.FrequencyPenalty = &f
		}
	}
	if v, ok := extra["presence_penalty"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.PresencePenalty = &f
		}
	}
	if v, ok := extra["seed"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.Seed = &f
		}
	}
	if v, ok := extra["parallel_tool_calls"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			out.ParallelTooCalls = &b
		}
	}
	if v, ok := extra["service_tier"]; ok {
		out.ServiceTier = v
	}
	if v, ok := extra["user"]; ok {
		out.User = v
	}
}

// FromRelaykitRequest 把一个 relaykit 请求转成本项目的请求。
//
// 用途是接收客户端请求：客户端说 OpenAI 或 Anthropic 协议，先解析成
// relaykit 的类型，再用这里转成管道内的类型。放在本包而不是散在 gateway 里，
// 是为了让「relaykit 的类型」这件事只在这一个文件出现。
func FromRelaykitRequest(target channel.Protocol, src any) (*channel.Request, error) {
	_, err := formatFor(target)
	if err != nil {
		return nil, err
	}

	// 先统一到 OpenAI Chat 形状，再从它翻译成管道类型。
	// Anthropic 请求先经 relaykit 转成 OpenAI 形状，于是下面只需要一条路径。
	var openaiReq *dto.GeneralOpenAIRequest
	switch v := src.(type) {
	case *dto.GeneralOpenAIRequest:
		openaiReq = v
	case dto.GeneralOpenAIRequest:
		openaiReq = &v
	case *dto.ClaudeRequest:
		res, err := relayconvert.ConvertRequest(context.Background(), nil, types.RelayFormatOpenAI, *v)
		if err != nil {
			return nil, fmt.Errorf("解析 Anthropic 请求失败: %w", err)
		}
		converted, ok := res.Value.(*dto.GeneralOpenAIRequest)
		if !ok {
			return nil, fmt.Errorf("Anthropic 请求转换后得到意外类型 %T", res.Value)
		}
		openaiReq = converted
	default:
		return nil, fmt.Errorf("不认识的请求类型 %T", src)
	}

	out := &channel.Request{
		Model:           openaiReq.Model,
		ReasoningEffort: openaiReq.ReasoningEffort,
		Temperature:     openaiReq.Temperature,
		TopP:            openaiReq.TopP,
		Stream:          openaiReq.Stream != nil && *openaiReq.Stream,
	}
	if openaiReq.MaxTokens != nil {
		out.MaxTokens = int(*openaiReq.MaxTokens)
	}

	for _, m := range openaiReq.Messages {
		msg, err := messageFromRelaykit(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msg)
	}

	for _, t := range openaiReq.Tools {
		schema, _ := t.Function.Parameters.(map[string]any)
		out.Tools = append(out.Tools, channel.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

func messageFromRelaykit(m dto.Message) (channel.Message, error) {
	out := channel.Message{Role: m.Role}

	if len(m.ToolCalls) > 0 {
		var calls []dto.ToolCallResponse
		if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
			return out, fmt.Errorf("解析工具调用失败: %w", err)
		}
		for _, c := range calls {
			var input map[string]any
			if c.Function.Arguments != "" {
				// 参数解析失败不算致命：上游或客户端可能给了半截 JSON，
				// 而丢掉整条请求比丢掉一段参数更糟。保留原文供排查。
				if err := json.Unmarshal([]byte(c.Function.Arguments), &input); err != nil {
					input = map[string]any{"_raw": c.Function.Arguments}
				}
			}
			out.Content = append(out.Content, channel.ContentBlock{
				Type:      "tool_use",
				ToolUseID: c.ID,
				ToolName:  c.Function.Name,
				ToolInput: input,
			})
		}
	}

	switch content := m.Content.(type) {
	case string:
		if content != "" {
			out.Content = append([]channel.ContentBlock{{Type: "text", Text: content}}, out.Content...)
		}
	case []any:
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := block["type"].(string)
			switch typ {
			case "text":
				text, _ := block["text"].(string)
				out.Content = append(out.Content, channel.ContentBlock{Type: "text", Text: text})
			case "image_url":
				out.Content = append(out.Content, fromImageBlock(block))
			}
		}
	}

	// role=tool 的消息靠消息上的 tool_call_id 关联，把它补成内容块，
	// 否则管道里那条结果就成了无主的文本。
	if m.Role == "tool" && m.ToolCallId != "" {
		text := ""
		for _, b := range out.Content {
			if b.Type == "text" {
				text += b.Text
			}
		}
		out.Content = []channel.ContentBlock{{
			Type:      "tool_result",
			ToolUseID: m.ToolCallId,
			Text:      text,
		}}
	}
	return out, nil
}

func fromImageBlock(block map[string]any) channel.ContentBlock {
	out := channel.ContentBlock{Type: "image"}
	urlObj, ok := block["image_url"]
	if !ok {
		return out
	}
	var url string
	switch v := urlObj.(type) {
	case string:
		url = v
	case map[string]any:
		url, _ = v["url"].(string)
	}
	if data, mediaType, ok := splitDataURL(url); ok {
		out.ImageBase64 = data
		out.ImageMediaType = mediaType
		return out
	}
	out.ImageURL = url
	return out
}

// ToRelaykitClaude 把统一请求转成 relaykit 的 Anthropic Messages 请求。
//
// 路径是「本项目 → OpenAI Chat → Claude Messages」：relaykit 的转换矩阵
// 以 OpenAI Chat 为枢纽，两边都能到，而直接写「本项目 → Claude」会让
// 枢纽这一层的语义差异（system 是顶层字段、max_tokens 必填、
// tool_use 与 tool_result 的结构不同）散落在两个地方。
func ToRelaykitClaude(req *dto.GeneralOpenAIRequest) (*dto.ClaudeRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}
	res, err := relayconvert.ConvertRequest(context.Background(), nil, types.RelayFormatClaude, *req)
	if err != nil {
		return nil, fmt.Errorf("转换为 Anthropic 请求失败: %w", err)
	}
	out, ok := res.Value.(*dto.ClaudeRequest)
	if !ok {
		return nil, fmt.Errorf("转换后得到意外类型 %T", res.Value)
	}
	return out, nil
}
