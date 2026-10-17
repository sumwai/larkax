package responsesadapter

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"

	"github.com/sumwai/larkax/internal/channel"
)

// 上游 JSON 的解析结构。
//
// 为什么不直接 Unmarshal 进 relaykit 的 dto.OpenAIResponsesResponse：这个包
// 要处理的正是「各家兼容但不完全一致」——同一个字段有的是数字有的是字符串、
// 有的是 null、有的干脆不在。一处不符会让**整条**响应解析失败，而那次请求
// 上游已经计了费。自己声明一个窄结构，缺什么一目了然，也便于在注释里写清
// 「这个字段为什么可能不在」。
//
// 解析完之后立刻转成 relaykit 的类型，由它负责协议转换；usage 例外，见
// responsesUsage.toRelaykit。

// responsesResponse 是一次响应（非流式响应体，或流式事件里嵌的那个对象）。
type responsesResponse struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	// Status 取值：completed / incomplete / failed。用 RawMessage 接住
	// 原样再交给 relaykit：它按「字符串」读这个字段，而这里不该替它决定
	// 形态——读不出来时它自己会退化成空状态。
	Status            json.RawMessage `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []responsesOutput `json:"output"`
	Usage  *responsesUsage   `json:"usage"`
	Error  *responsesError   `json:"error"`
}

// responsesOutput 是 output[] 里的一项。
//
// 按 type 分：message（正文在 content[] 里）、reasoning（思考摘要在
// summary[] 里）、function_call / custom_tool_call（工具调用）。
// 一个结构接住全部形态，而不是按 type 声明几个类型：上游可能加新的 type，
// 而「多了一个不认识的 type」不该让整条响应解析失败。
type responsesOutput struct {
	Type    string                   `json:"type"`
	ID      string                   `json:"id"`
	Role    string                   `json:"role"`
	Status  string                   `json:"status"`
	Content []responsesOutputContent `json:"content"`
	Summary []responsesSummaryPart   `json:"summary"`
	// CallID 是工具调用的关联 id，客户端拿它回传结果。function_call 用它，
	// 而有的上游只给 item 自己的 id——两者都要接住（见 callID）。
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Arguments 用 RawMessage：官方形状是**字符串**形态的 JSON
	//（`"{\"a\":1}"`），而有的兼容层直接给对象。按字符串读会让后者整条
	// 事件解析失败，于是工具参数静默消失（见 arguments）。
	Arguments json.RawMessage `json:"arguments"`
	// Input 是 custom 工具的自由文本入参，同样是分片吐出的。
	Input json.RawMessage `json:"input"`
}

type responsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Annotations 是引用标注，原样转交给 relaykit（它的类型就是 []any，
	// 形状不同的话在那边也一样读不出来）。它不参与计费，所以这里不为了
	// 容忍它而把整个响应体改成 RawMessage——那会让「正文」也失去检查。
	Annotations []any `json:"annotations"`
}

type responsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// empty 判断这个 error 对象是不是「没有错误」的写法。
//
// 上游有 `"error": null` 与 `"error": {}` 两种正常的空写法。只看
// 「字段在不在」会把它们当成失败，于是「上游成功了」在账上表现为
// 「一次失败的重试」。
func (e *responsesError) empty() bool {
	return e == nil || (e.Message == "" && e.Type == "" && e.Code == "")
}

// responsesUsage 是上游报的用量。
//
// 全部用指针：**「字段不在」与「字段是 0」必须分开**（与 openaiadapter
// 同一条理由）。上游没报 cached_tokens 说明它不支持这个概念，报了 0 说明
// 这次没命中缓存——前者不该出现在账单明细里，后者应该。
type responsesUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
	TotalTokens  *int `json:"total_tokens"`
	// 缓存与推理嵌在 details 里，这是与 chat completions 最要紧的一处差别：
	// chat 那边叫 prompt_tokens_details / completion_tokens_details。
	InputTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
		// CacheWriteTokens 是缓存写入量。有的上游报它，有的不报。
		CacheWriteTokens *int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// toRelaykit 把上游字段映射到 dto.Usage 里**本项目口径**的位置。
//
// 本项目的口径（见 channel.Usage）是 OpenAI 那套：PromptTokens **含**缓存，
// 缓存读写另立字段，推理记在 CompletionTokenDetails 里、与 completion 并列
// 存放，是否已含在 completion 里由 CompletionIncludesReasoning 声明。
//
// 上游的 input_tokens 正是「含缓存的总量」（它的 details 是子集），所以它
// 落在 PromptTokens，缓存两项落在 PromptTokensDetails。
//
// 推理为什么落在 CompletionTokenDetails 而不是 dto.Usage.OutputTokensDetails：
// **后者在 relaykit 里没有任何读者**（全仓只有它的声明），把它写进去等于把
// 推理量静默丢掉——那时响应形状完全正常，而推理那部分按普通输出价计费。
// relay.UsageFromRelaykit 读的就是 CompletionTokenDetails，与非流式、
// 流式两条路径同一处入口。
func (u responsesUsage) toRelaykit() *dto.Usage {
	// 用 relaykit 自己的常量而不是字面量：这两个值是**它**的口径标识，
	// 写死了将来它改值我们不知道。同一条约定见 internal/relay/response.go。
	out := &dto.Usage{
		UsageSource:   dto.BillingUsageSourceOAIResponses,
		UsageSemantic: dto.BillingUsageSemanticOpenAI,
	}
	if u.InputTokens != nil {
		out.PromptTokens = *u.InputTokens
	}
	if u.OutputTokens != nil {
		out.CompletionTokens = *u.OutputTokens
	}
	if u.TotalTokens != nil {
		out.TotalTokens = *u.TotalTokens
	}
	if d := u.InputTokensDetails; d != nil {
		if d.CachedTokens != nil {
			out.PromptTokensDetails.CachedTokens = *d.CachedTokens
		}
		if d.CacheWriteTokens != nil {
			out.PromptTokensDetails.CacheWriteTokens = *d.CacheWriteTokens
		}
	}
	if d := u.OutputTokensDetails; d != nil && d.ReasoningTokens != nil {
		out.CompletionTokenDetails.ReasoningTokens = *d.ReasoningTokens
	}
	return out
}

// toRelaykit 把解析结果转成 relaykit 的 Responses 响应类型。
//
// 这一层映射是必须的：正文与工具调用的提取、结束原因的折算都已经在
// relaykit 里做过（并有测试覆盖），在这里重做一遍等于凭空造出第二套口径。
func (r responsesResponse) toRelaykit() *dto.OpenAIResponsesResponse {
	out := &dto.OpenAIResponsesResponse{
		ID:     r.ID,
		Model:  r.Model,
		Status: r.Status,
	}
	if r.IncompleteDetails != nil {
		out.IncompleteDetails = &dto.IncompleteDetails{Reason: r.IncompleteDetails.Reason}
	}
	if r.Usage != nil {
		out.Usage = r.Usage.toRelaykit()
	}
	for _, item := range r.Output {
		converted := dto.ResponsesOutput{
			Type:      item.Type,
			ID:        item.ID,
			Role:      item.Role,
			Status:    item.Status,
			CallId:    item.CallID,
			Name:      item.Name,
			Arguments: item.Arguments,
			Input:     item.Input,
		}
		for _, c := range item.Content {
			converted.Content = append(converted.Content, dto.ResponsesOutputContent{
				Type:        c.Type,
				Text:        c.Text,
				Annotations: c.Annotations,
			})
		}
		for _, s := range item.Summary {
			converted.Summary = append(converted.Summary, dto.ResponsesReasoningSummaryPart{
				Type: s.Type,
				Text: s.Text,
			})
		}
		out.Output = append(out.Output, converted)
	}
	return out
}

// finishReason 把终态事件折成本项目的结束原因。
//
// incomplete → length / content_filter 的折算交给 relaykit
// （ResponsesFinishReasonFromStatus），与非流式那边同一条判据：两处各写一遍
// 的话，「上游为什么停了」在两条路径上会有两个答案。
//
// hasToolCall 由调用方给：relaykit 的非流式转换也是这么判的（有工具调用
// 就是 tool_calls，否则 stop），而流式这一侧只有我们知道自己发过没有。
func (r responsesResponse) finishReason(hasToolCall bool) string {
	if reason, ok := relayconvert.ResponsesFinishReasonFromStatus(r.toRelaykit()); ok {
		return reason
	}
	if hasToolCall {
		return "tool_calls"
	}
	return "stop"
}

// callID 是工具调用的关联 id。
//
// 官方的 function_call 项给 call_id，而有的上游只给 item 自己的 id；
// 后者也接住——客户端要拿它回传结果，缺了这次工具往返就断了。
func (o responsesOutput) callID() string {
	if o.CallID != "" {
		return o.CallID
	}
	return o.ID
}

// arguments 是工具参数，统一取成字符串。
//
// 官方形状是**字符串**形态的 JSON（`"{\"a\":1}"`），而有的兼容层直接给
// 对象。kitutil 的转换对两种都给出「参数文本」：字符串解出来，对象原样
// 序列化。用它的而不是自己判断，是因为 relaykit 的两条转换路径用的是同一个
// 函数——口径不一致时工具参数会在两条路径上长得不一样。
func (o responsesOutput) arguments() string {
	if o.Type == "custom_tool_call" && len(o.Input) > 0 {
		// custom 工具的入参是自由文本，放在 input 里而不是 arguments 里。
		return kitutil.JsonRawMessageToString(o.Input)
	}
	return kitutil.JsonRawMessageToString(o.Arguments)
}

// isToolCallItem 判断一个输出项是不是由客户端执行的工具调用。
//
// function_call 是普通工具；custom_tool_call 是自定义工具（参数是自由文本）。
// 两者的参数都由客户端执行，所以我们都要把它交给调用方。内置工具
// （web_search_call 一类）**不是**这一类：它们在服务端执行，没有客户端
// 可执行的动作，当成工具调用发出去会让客户端拿到一个它执行不了的调用。
func isToolCallItem(itemType string) bool {
	return itemType == "function_call" || itemType == "custom_tool_call"
}

// 编译期断言：适配器必须满足接口。
// 放在这里而不是放在使用方，是为了让「接口改了但这个实现没跟上」
// 在改接口的那一次就报错。
var _ channel.Adapter = (*Adapter)(nil)
