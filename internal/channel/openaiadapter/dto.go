package openaiadapter

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"

	"github.com/sumwai/larkax/internal/channel"
)

// 上游 JSON 的解析结构。
//
// 为什么不直接用 relaykit 的 dto 去 Unmarshal：dto 的类型覆盖了 OpenAI 全部
// 端点，字段极多，而这里的解析要处理的正是「各家兼容但不完全一致」——
// 缺字段、多字段、类型不符。自己声明一个窄结构，缺什么一目了然，
// 也便于在注释里写清「这个字段为什么可能不在」。
//
// 解析完之后立刻转成 relaykit 的类型，由它负责协议转换与 usage 归一化。
// 那些逻辑不该在这里重写一遍。

type openAITextResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		Message      openAIMessage   `json:"message"`
		FinishReason string          `json:"finish_reason"`
		LogProbs     json.RawMessage `json:"logprobs"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []openAIToolCall `json:"tool_calls"`
}

type openAIToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role      string           `json:"role"`
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// openAIUsage 是上游报的用量。
//
// 全部用指针：**「字段不在」与「字段是 0」必须分开**。
// 上游没报 cached_tokens 说明它不支持这个概念，报了 0 说明这次没命中缓存——
// 前者不该出现在账单明细里，后者应该。用值类型会把两者混成一件事。
type openAIUsage struct {
	PromptTokens         *int `json:"prompt_tokens"`
	CompletionTokens     *int `json:"completion_tokens"`
	TotalTokens          *int `json:"total_tokens"`
	PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens         *int `json:"cached_tokens"`
		CachedCreationTokens *int `json:"cached_creation_tokens"`
		CacheWriteTokens     *int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// toRelaykit 把解析结果转成 relaykit 的响应类型。
//
// 这一层映射是必须的：usage 的归一化、缓存口径的换算、内容块的处理
// 都已经在 relaykit 里做过（并且被它的测试覆盖过），在这里重做一遍
// 等于凭空造出第二套口径。
func (r openAITextResponse) toRelaykit() *dto.OpenAITextResponse {
	out := &dto.OpenAITextResponse{
		Id:    r.ID,
		Model: r.Model,
	}
	if r.Usage != nil {
		out.Usage = *r.Usage.toRelaykit()
	}
	for _, c := range r.Choices {
		out.Choices = append(out.Choices, dto.OpenAITextResponseChoice{
			Index:        c.Index,
			FinishReason: c.FinishReason,
			Message:      c.Message.toRelaykit(),
		})
	}
	return out
}

func (m openAIMessage) toRelaykit() dto.Message {
	out := dto.Message{
		Role:             m.Role,
		ReasoningContent: stringOrNil(m.ReasoningContent),
	}
	// content 可能是字符串，也可能是内容块数组，还可能缺失。
	// 用 RawMessage 接住，再按首字符判断形态——这比尝试两种 Unmarshal
	// 更省事，也避免把一个内容块数组误判成字符串。
	if len(m.Content) > 0 {
		var s string
		if err := kitutil.Unmarshal(m.Content, &s); err == nil {
			out.Content = s
		} else {
			var blocks []dto.MediaContent
			if err := kitutil.Unmarshal(m.Content, &blocks); err == nil {
				out.Content = blocks
			}
		}
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]dto.ToolCallResponse, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			calls = append(calls, dto.ToolCallResponse{
				ID:   tc.ID,
				Type: defaultString(tc.Type, "function"),
				Function: dto.FunctionResponse{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		if raw, err := kitutil.Marshal(calls); err == nil {
			out.ToolCalls = raw
		}
	}
	return out
}

func (u openAIUsage) toRelaykit() *dto.Usage {
	// 用 relaykit 自己的常量而不是字面量：这两个值是**它**的口径标识，
	// 写死了将来它改值我们不知道。同一条约定见 internal/relay/response.go。
	out := &dto.Usage{
		UsageSource:   dto.BillingUsageSourceOAIChat,
		UsageSemantic: dto.BillingUsageSemanticOpenAI,
	}
	if u.PromptTokens != nil {
		out.PromptTokens = *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		out.CompletionTokens = *u.CompletionTokens
	}
	if u.TotalTokens != nil {
		out.TotalTokens = *u.TotalTokens
	}
	// 缓存命中在不同上游有两个字段名：prompt_cache_hit_tokens 是 DeepSeek
	// 一类上游的写法，cached_tokens 是 OpenAI 的写法。取到哪个用哪个。
	if u.PromptCacheHitTokens != nil {
		out.PromptCacheHitTokens = *u.PromptCacheHitTokens
	}
	// 回退这一支必须在 PromptTokensDetails != nil **之外**：只报
	// prompt_cache_hit_tokens 的上游连 prompt_tokens_details 都不发，
	// 写在里面等于永远走不到，那几个 token 会按普通输入价计费。
	if d := u.PromptTokensDetails; d != nil && d.CachedTokens != nil {
		out.PromptTokensDetails.CachedTokens = *d.CachedTokens
	} else if u.PromptCacheHitTokens != nil {
		out.PromptTokensDetails.CachedTokens = *u.PromptCacheHitTokens
	}
	if d := u.PromptTokensDetails; d != nil {
		if d.CachedCreationTokens != nil {
			out.PromptTokensDetails.CachedCreationTokens = *d.CachedCreationTokens
		}
		if d.CacheWriteTokens != nil {
			out.PromptTokensDetails.CacheWriteTokens = *d.CacheWriteTokens
		}
	}
	if d := u.CompletionTokensDetails; d != nil && d.ReasoningTokens != nil {
		out.CompletionTokenDetails.ReasoningTokens = *d.ReasoningTokens
	}
	return out
}

func stringOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// 编译期断言：适配器必须满足接口。
// 放在这里而不是放在使用方，是为了让「接口改了但这个实现没跟上」
// 在改接口的那一次就报错。
var _ channel.Adapter = (*Adapter)(nil)
