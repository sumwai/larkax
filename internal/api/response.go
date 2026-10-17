package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// 本文件负责「项目 → 调用方」方向的序列化：把管道内的统一响应渲染成
// 调用协议认识的 JSON。
//
// # 为什么自己声明结构体，而不是让 relaykit 的 dto 直接落成响应体
//
//   - 这是**对外契约**。docs/versioning.md 写着路径里的 /v1 是永久承诺，
//     而 relaykit 是 v0.x 的依赖，它的字段集随时可能在小版本里变。让它的
//     结构体直接成为响应体，等于把我们的契约挂在别人的发布节奏上。
//   - relaykit 的响应结构体里混着宿主专用的字段（usage 里带 source、
//     semantic、billing_usage 等）。它们对调用方没有意义，却会随着
//     序列化一起漏出去。
//
// 所以这里逐个字段地写清楚「给调用方的是什么」。内部结构怎么变都不影响
// 这个文件的输出——除非这里主动改，而那就是一次契约变更，该按
// docs/versioning.md 的规则来。

// ── OpenAI ─────────────────────────────────────────────────────────────

// openAIChatCompletion 是 OpenAI 的非流式响应体。
type openAIChatCompletion struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIMessage struct {
	Role string `json:"role"`
	// Content 是指针：只有工具调用而没有正文时，OpenAI 给的是 null
	// 而不是空串，而这两者在客户端里会被区别对待（前者表示「没有正文」，
	// 后者表示「正文是空的」）。
	Content   *string          `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name string `json:"name"`
	// Arguments 是**字符串**形态的 JSON，不是对象——这是 OpenAI 的形状，
	// 与 Anthropic 的 input 是对象正好相反。
	Arguments string `json:"arguments"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func newOpenAIChatCompletion(id, model string, created int64, resp *channel.Response) openAIChatCompletion {
	msg := openAIMessage{Role: "assistant"}
	var text strings.Builder
	for _, b := range resp.Message.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
				ID:   b.ToolUseID,
				Type: "function",
				Function: openAIToolFunction{
					Name:      b.ToolName,
					Arguments: toolArguments(b.ToolInput),
				},
			})
		}
		// 其余块类型（image / tool_result / thinking）不会出现在助手的回复里：
		// 前两种是调用方给进来的，第三种不是可见输出。适配器把它们归一化掉，
		// 真漏到这里说明归一化少了一条，丢掉比塞进正文更接近原意。
	}
	// 有正文、或者连工具调用都没有时，正文就存在（哪怕是空串）；
	// 只有「纯工具调用」这一种情形下正文是 null。
	if s := text.String(); s != "" || len(msg.ToolCalls) == 0 {
		msg.Content = &s
	}

	return openAIChatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []openAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: openAIFinishReason(resp.FinishReason),
		}},
		Usage: openAIUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			// 推理 token 是 completion 的分解而不是并列项（见 channel.Usage
			// 的注释），所以它不进 total——加了会让 total 大于两项之和。
			TotalTokens: resp.Usage.PromptTokens + resp.Usage.CompletionTokens,
		},
	}
}

// toolArguments 把工具参数编码成 OpenAI 要的字符串形态。
//
// 参数在管道里是结构化的 map（见 channel.ContentBlock），而 OpenAI 的
// tool_calls 里要的是 JSON 字符串。这是两家的形状差异，不是编码选择。
//
// 编码失败时给一个空对象：一次工具调用不该因为参数里出现无法编码的值
// 就让整个响应失败，而参数为空与参数编码不出来对调用方是同一种处置
// （都只能重问一次）。
func toolArguments(input map[string]any) string {
	if input == nil {
		return "{}"
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// openAIFinishReason 把上游给的结束原因转成 OpenAI 的说法。
//
// 上游说的可能是另一套词汇（Anthropic 的 end_turn / max_tokens / tool_use），
// 因为上游协议与调用协议是两件事。这里把两套都收进来。
//
// 认不出来的值原样透出，而不是编一个 "stop"：把「上游没正常结束」说成
// 「正常结束」，会让调用方按完成去处理一个半截结果，而那正是最难发现的
// 一类错误。
func openAIFinishReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use", "function_call":
		return "tool_calls"
	default:
		return reason
	}
}

// ── Anthropic ──────────────────────────────────────────────────────────

// claudeMessage 是 Anthropic 的非流式响应体。
type claudeMessage struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content 是块序列。用 []any 而不是一个带 omitempty 的联合结构：
	// 每种块的字段集不同（text 只有 text，tool_use 只有 id/name/input），
	// 合并成一个结构会让空字段也出现在另一种块里。
	Content []any `json:"content"`
	// StopReason 用指针：没有结束原因时是 null，而不是空串。
	StopReason *string `json:"stop_reason"`
	// StopSequence 恒为 null——我们不用自定义停止序列，但这个字段在
	// Anthropic 的响应里始终存在，缺了它有些客户端会当成字段丢失。
	StopSequence *string     `json:"stop_sequence"`
	Usage        claudeUsage `json:"usage"`
}

type claudeTextBlock struct {
	Type string `json:"type"`
	// Text 不加 omitempty：空的正文块仍然是正文块。
	Text string `json:"text"`
}

type claudeToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// claudeUsage 是 Anthropic 口径的用量。
//
// 缓存两项只在非零时出现：Anthropic 对不支持缓存的上游不报这两个字段，
// 而报了 0 与没报是两件事——前者表示这次没命中缓存，后者表示这家没有
// 这个概念。
type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

func newClaudeMessage(id, model string, resp *channel.Response) claudeMessage {
	out := claudeMessage{
		ID:    id,
		Type:  "message",
		Role:  "assistant",
		Model: model,
		Usage: claudeUsageOf(resp.Usage),
	}
	if reason := claudeStopReason(resp.FinishReason); reason != "" {
		out.StopReason = &reason
	}

	var text strings.Builder
	for _, b := range resp.Message.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			out.Content = append(out.Content, claudeToolUseBlock{
				Type:  "tool_use",
				ID:    b.ToolUseID,
				Name:  b.ToolName,
				Input: toolInput(b.ToolInput),
			})
		}
	}
	// 正文块排在工具块之前：Anthropic 的响应里正文在前，顺序反了会让
	// 按顺序渲染的界面把「说一句话然后调用工具」显示成「调用工具然后说话」。
	//
	// 没有工具块时正文块一定要在（哪怕是空的），因为 content 不能为空数组。
	if text.Len() > 0 || len(out.Content) == 0 {
		out.Content = append([]any{claudeTextBlock{Type: "text", Text: text.String()}}, out.Content...)
	}
	return out
}

// toolInput 把工具参数取成 Anthropic 要的对象形态。
//
// 参数为空时给空对象而不是 null：Anthropic 的 input 是必填的对象。
func toolInput(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	return input
}

// claudeUsageOf 把归一化用量转成 Anthropic 的口径。
//
// 两家的差别就在缓存上：本项目的 PromptTokens **含**缓存部分（OpenAI 口径，
// 见 channel.Usage 的注释），而 Anthropic 的 input_tokens 不含，缓存读写
// 单独报。直接把 PromptTokens 填进 input_tokens，会让按两家口径各自加总的
// 调用方得到两个不同的输入量——差的正好是缓存那部分，而它在响应里看不出
// 是重复计了还是真多了。
func claudeUsageOf(u channel.Usage) claudeUsage {
	input := u.PromptTokens - u.CacheReadTokens - u.CacheWriteTokens
	if input < 0 {
		// 上游给的数自相矛盾（缓存量大于输入总量）。夹到 0 而不是回一个
		// 负数：负数 token 在响应里只会被当成解析错误，而真正的原因在
		// 上游，调用方拿到它也无从下手。矛盾本身会留在内部用量里。
		input = 0
	}
	return claudeUsage{
		InputTokens:              input,
		OutputTokens:             u.CompletionTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
	}
}

// claudeStopReason 把上游给的结束原因转成 Anthropic 的说法。
//
// 认不出来的值原样透出，理由与 openAIFinishReason 相同。
func claudeStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return reason
	}
}

// ── 分派 ───────────────────────────────────────────────────────────────

// renderSuccess 把统一响应渲染成 protocol 协议的非流式响应体。
//
// created 只有 OpenAI 用得上：Anthropic 的响应体里没有时间戳字段。
//
// id 用的是本次请求的关联标识（见 requestIDOf）：它同时出现在响应体与
// x-request-id 头里，于是用户报问题时贴哪一个都指向同一批日志。两家各有
// 自己的 id 前缀（chatcmpl- / msg_），那是它们的惯例而不是契约——客户端
// 不该按前缀判断什么，所以这里也不假装。
func renderSuccess(protocol channel.Protocol, id, model string, now time.Time, resp *channel.Response) ([]byte, error) {
	switch protocol {
	case channel.ProtocolOpenAI:
		return json.Marshal(newOpenAIChatCompletion(id, model, now.Unix(), resp))
	case channel.ProtocolAnthropic:
		return json.Marshal(newClaudeMessage(id, model, resp))
	case channel.ProtocolOpenAIResponses:
		// 形状在 responses.go 里（那里也写了为什么没有用 relaykit 的转换器）。
		return json.Marshal(newResponsesResponse(id, model, now.Unix(), resp))
	default:
		// 走到这里说明入口把一条不认识的路径接到了本函数上——调用协议
		// 只有这几套，多一套就该多一个渲染器，而不是回落到其中一套。
		return nil, fmt.Errorf("协议 %q 没有非流式响应形状", protocol)
	}
}
