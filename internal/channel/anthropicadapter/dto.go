package anthropicadapter

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
)

// 上游 JSON 的解析结构。
//
// 与 openaiadapter 同理：自己声明窄结构而不是直接用 relaykit 的 dto，
// 是为了让「这个字段为什么可能不在」有地方写清楚，也便于处理各家
// 自称「兼容 Anthropic」但实际有出入的地方。

type claudeResponse struct {
	ID         string               `json:"id"`
	Type       string               `json:"type"`
	Role       string               `json:"role"`
	Model      string               `json:"model"`
	Content    []claudeMediaMessage `json:"content"`
	StopReason string               `json:"stop_reason"`
	Usage      *claudeUsage         `json:"usage"`
	Error      *claudeError         `json:"error"`
}

type claudeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type claudeMediaMessage struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

// claudeUsage 是上游报的输入侧用量。
//
// 全部用指针：**「字段不在」与「字段是 0」必须分开**。
// 上游没报 cache_read_input_tokens 说明它不支持这个概念，报了 0 说明这次
// 没命中缓存——前者不该出现在账单明细里，后者应该。
type claudeUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
}

func (r claudeResponse) toRelaykit() *dto.ClaudeResponse {
	out := &dto.ClaudeResponse{
		Id:         r.ID,
		Type:       r.Type,
		Role:       r.Role,
		Model:      r.Model,
		StopReason: r.StopReason,
	}
	for _, block := range r.Content {
		out.Content = append(out.Content, block.toRelaykit())
	}
	if r.Usage != nil {
		out.Usage = r.Usage.toRelaykit()
	}
	return out
}

func (m claudeMediaMessage) toRelaykit() dto.ClaudeMediaMessage {
	out := dto.ClaudeMediaMessage{
		Type: m.Type,
		Id:   m.ID,
		Name: m.Name,
	}
	if m.Text != "" {
		text := m.Text
		out.Text = &text
	}
	if m.Thinking != "" {
		thinking := m.Thinking
		out.Thinking = &thinking
	}
	if len(m.Input) > 0 {
		var input map[string]any
		switch err := kitutil.Unmarshal(m.Input, &input); err {
		case nil:
			out.Input = input
		default:
			// 工具参数不是合法对象：上游或模型给了半截内容。
			// 保留原文而不是丢弃整条响应——丢掉一个参数比丢掉整次调用轻。
			//
			// 先试字符串形态：有的上游把参数当 JSON 字符串给（"{\"a\":1}"），
			// 那种情况下 \" 之类的转义还在原文里，直接展示反而更难读。
			var asString string
			if kitutil.Unmarshal(m.Input, &asString) == nil {
				out.Input = map[string]any{"_raw": asString}
			} else {
				out.Input = map[string]any{"_raw": string(m.Input)}
			}
		}
	}
	return out
}

func (u claudeUsage) toRelaykit() *dto.ClaudeUsage {
	out := &dto.ClaudeUsage{}
	if u.InputTokens != nil {
		out.InputTokens = *u.InputTokens
	}
	if u.OutputTokens != nil {
		out.OutputTokens = *u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		out.CacheReadInputTokens = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		out.CacheCreationInputTokens = *u.CacheCreationInputTokens
	}
	return out
}

func (r claudeResponse) errorType() string {
	if r.Error == nil {
		return ""
	}
	return r.Error.Type
}

func (r claudeResponse) errorMessage() string {
	if r.Error == nil {
		return "上游返回错误"
	}
	if r.Error.Message != "" {
		return r.Error.Message
	}
	if r.Error.Type != "" {
		return r.Error.Type
	}
	return "上游返回错误"
}

// isRetryableErrorType 按 Anthropic 的错误分类判断是否值得换渠道重试。
//
// 分三类：
//   - overloaded_error：上游过载，换一个渠道可能成功 → 可重试
//   - rate_limit_error：限流，同上 → 可重试
//   - 其余（invalid_request_error、authentication_error、permission_error、
//     not_found_error、request_too_large）：重试多少次结果一样，
//     换渠道也是同一个错，只会把费用与延迟乘以候选渠道数
//
// 未知类型按不可重试处理：默认方向与 channel.IsRetryable 一致——
// 宁可少一次本可成功的尝试，也不要在每个候选渠道上重复注定失败的调用。
func isRetryableErrorType(t string) bool {
	switch t {
	case "overloaded_error", "rate_limit_error", "api_error":
		return true
	}
	return false
}
