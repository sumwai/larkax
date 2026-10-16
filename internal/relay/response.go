package relay

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"

	"github.com/sumwai/larkax/internal/channel"
)

// 本文件负责「响应与用量」方向：relaykit → 本项目。
//
// 与请求方向分开是有意的。请求方向的损耗是确定的（我们知道自己传了什么），
// 响应方向则要面对上游的自由发挥——多给的字段、缺的字段、口径不同的同名数字。
// 把两者的处理放在不同文件里，可以让审阅响应逻辑的人不必同时读请求映射。

// UsageFromRelaykit 把 relaykit 的用量转成本项目的用量。
//
// 这是全项目最容易算错钱的地方，所以口径逐条写明。
//
// relaykit 的 dto.Usage 是**多来源归一化后的产物**，它同时带着三套字段：
// PromptTokens/CompletionTokens（OpenAI 口径）、InputTokens/OutputTokens
// （Anthropic 与 Responses 口径）、以及 PromptCacheHitTokens 等缓存明细。
// 不同来源填充哪几个字段并不一致，所以不能挑一个字段就用。
//
// 本项目的口径（见 channel.Usage 的注释）是 OpenAI 那套，因此：
//
//   - PromptTokens 取 dto.PromptTokens。
//   - 若 PromptTokens 为 0 而 InputTokens 非 0，说明来源是 Anthropic 口径，
//     用 InputTokens + 缓存读写还回来。Anthropic 的 input_tokens 不含缓存部分，
//     而 OpenAI 的 prompt_tokens 含，所以补上缓存才是同一件事。
//   - 缓存读写取 PromptTokensDetails，它同时覆盖 OpenAI 的
//     prompt_tokens_details.cached_tokens 与 Anthropic 的 cache_read；
//     写入侧另有 CachedCreationTokens 与 CacheWriteTokens 两个字段。
//   - ReasoningTokens 照抄 CompletionTokenDetails.ReasoningTokens，
//     CompletionTokens 也照抄——而在这套口径下推理**是**输出的一部分
//     （见下面 CompletionIncludesReasoning 处的说明）。两者在这里并不互斥，
//     减掉推理是计费层的事。
func UsageFromRelaykit(u *dto.Usage) channel.Usage {
	if u == nil {
		return channel.Usage{}
	}

	prompt := int64(u.PromptTokens)
	completion := int64(u.CompletionTokens)

	// Anthropic 口径：input_tokens 不含缓存，需要补上才与 OpenAI 口径对齐。
	if prompt == 0 && u.InputTokens > 0 {
		prompt = int64(u.InputTokens)
	}
	if completion == 0 && u.OutputTokens > 0 {
		completion = int64(u.OutputTokens)
	}

	cacheRead := int64(u.PromptTokensDetails.CachedTokens)
	cacheWrite := int64(u.PromptTokensDetails.CachedCreationTokens + u.PromptTokensDetails.CacheWriteTokens)

	if prompt == 0 && (cacheRead > 0 || cacheWrite > 0) {
		// 来源只报了缓存部分。此时 prompt 至少等于缓存量，
		// 否则会算出「输入为 0 但有缓存命中」这种自相矛盾的账。
		prompt = cacheRead + cacheWrite
	}

	reasoning := int64(u.CompletionTokenDetails.ReasoningTokens)

	return channel.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		ReasoningTokens:  reasoning,
		// dto.Usage 是 OpenAI 语义的形状，而它的规范化正是把推理并进输出里的：
		// dto.BillingUsage 在把 Gemini 的用量折成这一形状时写的是
		// CompletionTokens = CandidatesTokenCount + ThoughtsTokenCount，同时把
		// ThoughtsTokenCount 记进 CompletionTokenDetails.ReasoningTokens。
		// OpenAI 自己的定义也一样——completion_tokens 含
		// completion_tokens_details.reasoning_tokens。
		//
		// Anthropic 口径（InputTokens/OutputTokens）同样如此：output_tokens
		// 含扩展思考的 token，而协议不单独报推理量，所以 ReasoningTokens 恒为 0，
		// 声明成「已含」在数值上是空的。
		CompletionIncludesReasoning: true,
		UnitCounts:                  unitCountsFromRelaykit(u),
		Raw:                         rawUsage(u),
	}
}

// unitCountsFromRelaykit 提取非 token 计量。
//
// 目前只有缓存写入的 5 分钟/1 小时拆分属于这一类——它影响价格，
// 却不适合放进 token 字段里（那会让「缓存写入」这一个概念有两个口径）。
func unitCountsFromRelaykit(u *dto.Usage) map[string]int64 {
	out := map[string]int64{}
	if u.ClaudeCacheCreation5mTokens > 0 {
		out["cache_write_5m"] = int64(u.ClaudeCacheCreation5mTokens)
	}
	if u.ClaudeCacheCreation1hTokens > 0 {
		out["cache_write_1h"] = int64(u.ClaudeCacheCreation1hTokens)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rawUsage 保留上游原样数据，供事后发现口径变化。
//
// 它不参与计费——这一点很重要：Raw 是我方序列化的副本，字段名可能随
// relaykit 版本变化，把它当计费依据等于把价格绑在依赖的内部结构上。
func rawUsage(u *dto.Usage) map[string]any {
	out := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.UsageSource != "" {
		out["source"] = u.UsageSource
	}
	if u.UsageSemantic != "" {
		out["semantic"] = u.UsageSemantic
	}
	if u.BillingUsage != nil && u.BillingUsage.Estimated {
		// 上游报的是估算值。路由与界面都要能看出这一点，
		// 否则「估的」和「实测的」在账上没区别。
		out["estimated"] = true
	}
	return out
}

// ResponseFromRelaykit 把 relaykit 的响应转成本项目的响应。
//
// src 允许是 OpenAI 形状或 Claude 形状：上游说什么协议是适配器决定的，
// 而计费与路由不关心那个。两条路径在这里汇合。
func ResponseFromRelaykit(src any) (*channel.Response, error) {
	switch v := src.(type) {
	case *dto.OpenAITextResponse:
		return fromOpenAIResponse(v)
	case *dto.ClaudeResponse:
		return fromClaudeResponse(v)
	default:
		return nil, fmt.Errorf("不认识的响应类型 %T", src)
	}
}

func fromOpenAIResponse(r *dto.OpenAITextResponse) (*channel.Response, error) {
	if r == nil {
		return nil, fmt.Errorf("响应为 nil")
	}

	out := &channel.Response{
		UpstreamModel: r.Model,
		Usage:         UsageFromRelaykit(&r.Usage),
	}

	if len(r.Choices) > 0 {
		choice := r.Choices[0]
		out.FinishReason = choice.FinishReason
		msg, err := messageFromRelaykit(choice.Message)
		if err != nil {
			return nil, err
		}
		out.Message = msg
	}

	raw, err := toRawMap(r)
	if err != nil {
		return nil, err
	}
	out.Raw = raw
	return out, nil
}

func fromClaudeResponse(r *dto.ClaudeResponse) (*channel.Response, error) {
	if r == nil {
		return nil, fmt.Errorf("响应为 nil")
	}

	out := &channel.Response{
		UpstreamModel: r.Model,
		FinishReason:  r.StopReason,
	}

	for _, block := range r.Content {
		switch block.Type {
		case "text":
			out.Message.Content = append(out.Message.Content, channel.ContentBlock{
				Type: "text",
				Text: derefString(block.Text),
			})
		case "tool_use":
			input, _ := block.Input.(map[string]any)
			out.Message.Content = append(out.Message.Content, channel.ContentBlock{
				Type:      "tool_use",
				ToolUseID: block.Id,
				ToolName:  block.Name,
				ToolInput: input,
			})
		case "thinking":
			// 思考内容不放进 Message：它与 assistant 的可见输出是两件事。
			// 需要它的一方（例如按思考 token 计费）自己去读 Raw。
		}
	}
	out.Message.Role = "assistant"

	if r.Usage != nil {
		out.Usage = UsageFromRelaykit(&dto.Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			PromptTokensDetails: dto.InputTokenDetails{
				CachedTokens:         r.Usage.CacheReadInputTokens,
				CachedCreationTokens: r.Usage.CacheCreationInputTokens,
			},
			ClaudeCacheCreation5mTokens: r.Usage.ClaudeCacheCreation5mTokens,
			ClaudeCacheCreation1hTokens: r.Usage.ClaudeCacheCreation1hTokens,
			UsageSource:                 "claude_messages",
			UsageSemantic:               "anthropic",
		})
		// Claude 的 input_tokens 不含缓存，而我们刚把它塞进了 PromptTokens。
		// 补上缓存部分才是 OpenAI 口径，否则输入 token 会被低估。
		//
		// 用加法而不是让 UsageFromRelaykit 去做：那个函数不知道调用方
		// 是从哪个协议来的，而这里知道。
		out.Usage.PromptTokens += out.Usage.CacheReadTokens + out.Usage.CacheWriteTokens
	}

	raw, err := toRawMap(r)
	if err != nil {
		return nil, err
	}
	out.Raw = raw
	return out, nil
}

// toRawMap 把一个 relaykit 的响应结构转成通用 map，供排查用。
//
// 走 relaykit 自己的 kitutil 编解码而不是标准库：那个包是它的 JSON 钩子，
// 绕开它可能得到与转换器内部不一致的表示（例如自定义的数值类型）。
func toRawMap(v any) (map[string]any, error) {
	data, err := kitutil.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化响应失败: %w", err)
	}
	var out map[string]any
	if err := kitutil.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("反序列化响应失败: %w", err)
	}
	return out, nil
}

// splitDataURL 拆出 data URL 的媒体类型与 base64 内容。
//
// 放在本包是因为它只服务于「relaykit 的 URL 形态 ↔ 本项目的结构化形态」
// 这个转换点。写成独立函数便于单独测——它的边界（没有逗号、没有分号、
// 不是 data 开头）都不该 panic。
func splitDataURL(url string) (data, mediaType string, ok bool) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(url, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", "", false
	}
	mediaType = rest[:semi]
	data = rest[semi+len(";base64,"):]
	if mediaType == "" || data == "" {
		return "", "", false
	}
	return data, mediaType, true
}

// derefString 把可空字符串取成值。relaykit 的 Claude 内容块里文本是
// 指针类型（区分「没有这一块」与「这一块是空串」），而本项目的块用值。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
