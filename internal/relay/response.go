package relay

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
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
// # 口径由声明的字段决定，不靠数字猜
//
// relaykit 的 dto.Usage 是**多来源归一化后的产物**，它同时带着两套字段名：
// PromptTokens/CompletionTokens 与 InputTokens/OutputTokens。同一个位置上的
// 数字在两家口径下含义不同，所以不能挑一个字段就用。哪一套成立由
// dto.Usage.UsageSemantic 声明——生产方（两个协议适配器）填它，这里读它。
//
// **不再用「PromptTokens 是不是 0」反推来源**：整段 prompt 命中缓存时
// Anthropic 的 input_tokens 本来就是 0，而那时正是最需要判对口径的时候。
//
// 本项目的口径（见 channel.Usage 的注释）是 OpenAI 那套：PromptTokens
// **含**缓存，缓存读写另立字段。各取值读哪些字段：
//
//	UsageSemantic   输入总量                        输出
//	anthropic       PromptTokens + 缓存读 + 缓存写   CompletionTokens
//	openai          PromptTokens（已含缓存）         CompletionTokens
//	gemini          同 openai                       同 openai
//	空 / 未知       同 openai                       同 openai
//
// anthropic 那一支为什么不读 InputTokens：在 relaykit 里 InputTokens 是
// **Responses 口径**的字段，含义是「含缓存的总量」——dto.BillingUsage 的
// canonicalClaudeUsage 写的就是 InputTokens = input + cacheRead +
// cacheCreation。拿它当「不含缓存的输入」再加一次缓存就是重复计费。
// 而 Claude 那个不含缓存的 input_tokens 在 relaykit 里一律落在 PromptTokens：
// relayconvert 的 FormatClaudeResponseInfo 与 UsageFromClaudeAPIUsage 都这么填，
// canonicalClaudeUsage 也一样（PromptTokens = claudeUsage.InputTokens，
// 而它旁边的 InputTokens 才是总量）。所以这一支读 PromptTokens。
//
// 空 / 未知为什么按 openai 读：dto.Usage 的主字段名（PromptTokens /
// CompletionTokens）就是 OpenAI 口径的，relaykit 的规范形也把 UsageSemantic
// 设成 openai；未知取值更可能是 OpenAI 形状的扩展而不是 Anthropic 的。
// 这仍然不是「猜」——读的是一个声明出来的字段，默认值只是「没声明时按这个
// 格式自己的原生口径读」。缺声明这件事会留在 Raw 里（semantic_missing），
// 事后能查出一笔用量是按声明读的还是按默认分支读的；一个数都没读到时不标，
// 那时没有「按哪个口径读」的问题（见 rawUsage）。
//
// # 缓存字段
//
// 缓存读写只从 PromptTokensDetails 取：CachedTokens 是缓存读取，
// CachedCreationTokens + CacheWriteTokens 是缓存写入。各生产方的这三个
// 落点互不重叠（Anthropic 只填 CachedCreationTokens；OpenAI 兼容上游的
// 三个字段各来自上游的一个字段名），所以没有第二条路径把同一个量再填一遍。
//
// # 其余字段
//
//   - ReasoningTokens 照抄 CompletionTokenDetails.ReasoningTokens，
//     CompletionTokens 也照抄——而在这套口径下推理**是**输出的一部分
//     （见下面 CompletionIncludesReasoning 处的说明）。两者在这里并不互斥，
//     减掉推理是计费层的事。
func UsageFromRelaykit(u *dto.Usage) channel.Usage {
	if u == nil {
		return channel.Usage{}
	}

	cacheRead := int64(u.PromptTokensDetails.CachedTokens)
	cacheWrite := int64(u.PromptTokensDetails.CachedCreationTokens + u.PromptTokensDetails.CacheWriteTokens)

	prompt := int64(u.PromptTokens)
	completion := int64(u.CompletionTokens)

	if strings.EqualFold(u.UsageSemantic, dto.BillingUsageSemanticAnthropic) {
		// Anthropic：PromptTokens 装的是 input_tokens，**不含**缓存
		// （缓存断点之后那部分；整段命中缓存时它就是 0）。
		// 加上缓存读写才是与 OpenAI 口径同义的输入总量。
		//
		// 这一支不做「PromptTokens 为 0 就改读 InputTokens」的兜底：
		// InputTokens 含缓存，那样兜底会在整段命中缓存时把缓存算两遍。
		prompt += cacheRead + cacheWrite
	} else if prompt == 0 {
		// 非 anthropic 口径下 PromptTokens 与 InputTokens 同义（都含缓存），
		// 后者只是前者的兜底。这一步换的是**字段名**，不是口径：
		// relaykit 的规范形（canonicalOpenAIUsage）会把两者填成同一个数。
		prompt = int64(u.InputTokens)
	}
	if completion == 0 {
		completion = int64(u.OutputTokens)
	}

	if prompt == 0 && (cacheRead > 0 || cacheWrite > 0) {
		// 来源只报了缓存部分。此时 prompt 至少等于缓存量，
		// 否则会算出「输入为 0 但有缓存命中」这种自相矛盾的账。
		prompt = cacheRead + cacheWrite
	}

	reasoning := int64(u.CompletionTokenDetails.ReasoningTokens)

	// 上游整条响应没带 usage 时，上面读出来的全是 0。那与「生产方忘了声明
	// 口径」都会走到默认分支，但只有后者值得留在账上，所以标记只在
	// **确实读到了数字**时才打（见 rawUsage）。
	hasCounts := prompt != 0 || completion != 0 || cacheRead != 0 || cacheWrite != 0 || reasoning != 0

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
		Raw:                         rawUsage(u, hasCounts),
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
//
// hasCounts 表示这次用量里确实读到了数字。它只影响 semantic_missing
// 打不打：上游整条响应没带 usage 时也会缺声明，但那种情况没有「按哪个
// 口径读」的问题，一起标会让这个标记读起来像生产方的疏漏。
func rawUsage(u *dto.Usage, hasCounts bool) map[string]any {
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
	} else if hasCounts {
		// 生产方没有声明口径，上面按 dto.Usage 的原生口径（OpenAI）读了。
		// 把「没声明」这件事留在账上：否则事后分不出这一笔是按声明的口径
		// 读的，还是按默认分支读的。
		out["semantic_missing"] = true
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
// src 允许是 OpenAI 形状、Claude 形状或 Responses 形状：上游说什么协议是
// 适配器决定的，而计费与路由不关心那个。三条路径在这里汇合。
func ResponseFromRelaykit(src any) (*channel.Response, error) {
	switch v := src.(type) {
	case *dto.OpenAITextResponse:
		return fromOpenAIResponse(v)
	case *dto.ClaudeResponse:
		return fromClaudeResponse(v)
	case *dto.OpenAIResponsesResponse:
		return fromResponsesResponse(v)
	default:
		return nil, fmt.Errorf("不认识的响应类型 %T", src)
	}
}

// fromResponsesResponse 把 Responses 形状的上游响应转成本项目的响应。
//
// 正文、工具调用与结束原因交给 relaykit 的
// ResponsesResponseToChatCompletionsResponse：output[] 里按 type 分块
// （message 的 content[]、reasoning 的 summary[]、function_call 与
// custom_tool_call）、以及 incomplete 的 reason 折成 finish_reason，
// 那些它已经做过并有测试覆盖。
//
// **用量不走它返回的那一份，而是直接读 r.Usage。** 理由有两条：
//
//  1. r.Usage 是适配器按本项目口径填好的（PromptTokens 含缓存、缓存读写
//     另立字段、推理记在 CompletionTokenDetails），与 fromClaudeResponse
//     的做法一致——换算只在 UsageFromRelaykit 里发生一次。
//  2. 那个函数返回的用量是拿 r.Usage 再按 Responses 口径读一遍的产物，
//     两份都留着就会出现「哪一个算数」的问题，而它们迟早会分岔。
func fromResponsesResponse(r *dto.OpenAIResponsesResponse) (*channel.Response, error) {
	if r == nil {
		return nil, fmt.Errorf("响应为 nil")
	}

	chat, _, err := relayconvert.ResponsesResponseToChatCompletionsResponse(r, r.ID)
	if err != nil {
		return nil, fmt.Errorf("解析 Responses 响应失败: %w", err)
	}

	out := &channel.Response{
		UpstreamModel: r.Model,
		Usage:         UsageFromRelaykit(r.Usage),
	}
	if len(chat.Choices) > 0 {
		out.FinishReason = chat.Choices[0].FinishReason
		msg, msgErr := messageFromRelaykit(chat.Choices[0].Message)
		if msgErr != nil {
			return nil, msgErr
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
		// 只填 Claude 协议自己的字段，口径由 UsageSemantic 声明。
		//
		// input_tokens 落在 PromptTokens 而不是 InputTokens：在 relaykit 里
		// InputTokens 是 Responses 口径的「含缓存总量」，而 Claude 那个不含
		// 缓存的 input_tokens 一律落在 PromptTokens（relayconvert 的
		// FormatClaudeResponseInfo 与 UsageFromClaudeAPIUsage 都这么填）。
		//
		// **补回缓存那一步只在 UsageFromRelaykit 里发生一次**，这里不再自己加：
		// 两处各加一次时，input_tokens 非 0 的请求看着是对的，而整段 prompt
		// 命中缓存（input_tokens == 0）时缓存会被算两遍。
		out.Usage = UsageFromRelaykit(&dto.Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			PromptTokensDetails: dto.InputTokenDetails{
				CachedTokens:         r.Usage.CacheReadInputTokens,
				CachedCreationTokens: r.Usage.CacheCreationInputTokens,
			},
			ClaudeCacheCreation5mTokens: r.Usage.ClaudeCacheCreation5mTokens,
			ClaudeCacheCreation1hTokens: r.Usage.ClaudeCacheCreation1hTokens,
			UsageSource:                 dto.BillingUsageSourceClaudeMessages,
			UsageSemantic:               dto.BillingUsageSemanticAnthropic,
		})
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
