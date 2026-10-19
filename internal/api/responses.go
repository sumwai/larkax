package api

import (
	"fmt"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
)

// 本文件是 OpenAI Responses 协议的**对外形状**：非流式响应体，以及流式事件里
// 嵌的那个 response 对象。帧的序列在 stream.go 里（它与另外两套协议一样，
// 是「先写头、再逐帧写」那一套）。
//
// # 为什么没有用 relaykit 的转换器
//
// relaykit 有 ChatCompletionsResponseToResponsesResponseWithTools，但它的返回值
// 不能直接当响应体，两处都是硬伤：
//
//   - 它的 usage 里 **output_tokens_details 恒为 nil**（推理量被写进了 chat 名字的
//     completion_tokens_details 里），input_tokens_details 也只在有非零缓存量时才设。
//     而这两个字段是**官方 SDK 的必填项**（ResponseUsage 的这两个字段都没有默认值），
//     照它序列化，客户端读 usage.output_tokens_details 时拿到的是一个不存在的属性。
//   - 它带着 dto.Usage 的一整套宿主专用字段一起出来：billing_usage（**我方成本与
//     计价数据**）、usage_semantic、usage_source、prompt_cache_hit_tokens、
//     claude_cache_creation_*_tokens。前两个漏给调用方等于泄露内部定价。
//
// 这与 response.go 开篇那条理由同源：这是对外契约，字段集由我们钉住，不挂在
// relaykit 的发布节奏上。**请求方向仍然用 relaykit**——input[] 那十几种 item
// 类型的转换没有第二条实现（见 internal/relay 的 parseResponsesRequest）。

// ── 响应对象 ───────────────────────────────────────────────────────────

// responsesResponse 是 Responses 的响应对象。
//
// 它同时是**非流式响应体**与流式事件里 response.created / response.completed
// 内嵌的那个对象——两处是同一个东西，拆成两个结构体只会让它们慢慢分岔。
//
// 带默认值的必填字段（parallel_tool_calls / tool_choice / tools）按官方 SDK 的
// 模型给：它的 Response 把这几个标成没有默认值，缺一个就会让 SDK 构造出一个
// 缺属性的对象——**解析时不报错，读到时才炸**，而现场离原因很远。
type responsesResponse struct {
	ID        string          `json:"id"`
	Object    string          `json:"object"`
	CreatedAt int64           `json:"created_at"`
	Status    string          `json:"status"`
	Model     string          `json:"model"`
	Output    []any           `json:"output"`
	Usage     *responsesUsage `json:"usage,omitempty"`

	// IncompleteDetails 只在 status 是 incomplete 时出现。
	IncompleteDetails *responsesIncompleteDetails `json:"incomplete_details,omitempty"`

	// Error 只在流式那条路的失败事件里出现（非流式的失败走 writeError，
	// 是另一套形状）。
	Error *responsesError `json:"error,omitempty"`

	// 下面三项对我们恒为常量，但 SDK 把它们标成必填，所以必须写出去。
	// parallel_tool_calls 写 false：我们从不替调用方打开它，而上游是否并行
	// 由上游决定——这个字段描述的是**请求时的选择**，不是结果。
	ParallelToolCalls bool `json:"parallel_tool_calls"`
	// ToolChoice 写 "auto"：我们不解释这个字段（请求里的 tool_choice 原样
	// 转给上游），而这里要的是一个 SDK 认得的值。
	ToolChoice string `json:"tool_choice"`
	// Tools 写空数组而不是省略：SDK 的 Response.tools 是必填的列表。
	Tools []any `json:"tools"`
}

// responsesIncompleteDetails 说明这次响应为什么没有正常结束。
type responsesIncompleteDetails struct {
	// Reason 是 SDK 认的取值之一：max_output_tokens / content_filter。
	Reason string `json:"reason"`
}

// responsesError 是流式事件里那个错误对象。
//
// 它是**平铺**的（没有 error 外层），与另外两套协议的错误体都不一样——
// 那是 Responses 的 SSE 事件形状，不是可以统一的地方。
type responsesError struct {
	Type    string  `json:"type"`
	Code    string  `json:"code,omitempty"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

// ── output[] 的项 ──────────────────────────────────────────────────────

// responsesMessageItem 是一条助手消息在 output[] 里的样子。
//
// 与 function_call 分开声明，而不是合成一个带 omitempty 的大结构：两者的
// 必填字段集不同（message 要 content 且它必须是数组，function_call 要
// call_id / name / arguments），合并之后另一种类型里会多出几个 null。
type responsesMessageItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
	Role   string `json:"role"`
	// Content 不加 omitempty：SDK 的 ResponseOutputMessage.content 是必填的
	// 列表，空数组与缺字段在它那里不是一回事。
	Content []responsesContent `json:"content"`
}

// responsesContent 是消息项里的一个内容片段。
type responsesContent struct {
	Type string `json:"type"`
	// Text 不加 omitempty：空的正文片段仍然是正文片段。
	Text string `json:"text"`
	// Annotations 必须是数组，不能是 null（SDK 的模型里它是必填列表）。
	Annotations []any `json:"annotations"`
}

// responsesFunctionCallItem 是一次工具调用在 output[] 里的样子。
//
// 与 chat 的 tool_calls 有两处形状差异，都是契约的一部分：
//
//   - 参数叫 arguments，是**字符串**形态的 JSON（与 chat 相同）；
//   - 关联 id 叫 call_id（chat 叫 id），而项自己的 id 是另一个东西。
type responsesFunctionCallItem struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Arguments 不加 omitempty：它是字符串拼接的载体，缺字段与空串在严格
	// 实现里不等价（与 openAIStreamToolFunction.Arguments 同一个理由）。
	Arguments string `json:"arguments"`
}

// ── 用量 ───────────────────────────────────────────────────────────────

// responsesUsage 是 Responses 口径的用量。
//
// 四个子字段都是**必填**（官方 SDK 的 ResponseUsage 没有默认值），所以这里
// 全部按值给出、不加 omitempty：缺了 output_tokens_details 或
// input_tokens_details.cache_write_tokens，客户端读到的就是一个不存在的属性。
//
// 口径换算与另外两套一致，见 channel.Usage：本项目的 PromptTokens **含**缓存，
// 所以 input_tokens 直接取它，缓存两项另立（Anthropic 那边则是减掉缓存，
// 见 claudeUsageOf）。
type responsesUsage struct {
	InputTokens         int64                  `json:"input_tokens"`
	InputTokensDetails  responsesInputDetails  `json:"input_tokens_details"`
	OutputTokens        int64                  `json:"output_tokens"`
	OutputTokensDetails responsesOutputDetails `json:"output_tokens_details"`
	TotalTokens         int64                  `json:"total_tokens"`
}

type responsesInputDetails struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type responsesOutputDetails struct {
	// ReasoningTokens 是**输出的一部分**而不是并列项（见 channel.Usage 的
	// CompletionIncludesReasoning），所以它不进 total。
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

func responsesUsageOf(u channel.Usage) *responsesUsage {
	return &responsesUsage{
		InputTokens: u.PromptTokens,
		InputTokensDetails: responsesInputDetails{
			CachedTokens:     u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
		},
		OutputTokens:        u.CompletionTokens,
		OutputTokensDetails: responsesOutputDetails{ReasoningTokens: u.ReasoningTokens},
		TotalTokens:         u.PromptTokens + u.CompletionTokens,
	}
}

// ── 渲染 ───────────────────────────────────────────────────────────────

// newResponsesResponse 把统一响应渲染成 Responses 的响应对象。
//
// created 是响应体的 created_at（流式那一份由渲染器自己给，因为一条流的
// 开始与结束必须报同一个时刻）。
func newResponsesResponse(id, model string, created int64, resp *channel.Response) responsesResponse {
	status, incomplete := responsesStatus(resp.FinishReason)
	out := responsesResponse{
		ID:                id,
		Object:            "response",
		CreatedAt:         created,
		Status:            status,
		Model:             model,
		Output:            responsesOutput(id, status, resp.Message),
		Usage:             responsesUsageOf(resp.Usage),
		IncompleteDetails: incomplete,
		ToolChoice:        "auto",
		Tools:             []any{},
	}
	return out
}

// responsesMessageItemID 是正文项在 output[] 里的 id。
//
// 非流式与流式两处共用它：同一个请求的正文项在响应体与流式事件里必须是
// 同一个 id，否则客户端按 item_id 关联增量时会关联到另一项。
func responsesMessageItemID(responseID string) string { return responseID + "_msg_0" }

// responsesCallItemID 是一次工具调用项在 output[] 里的 id。
//
// 它取自**上游给的**工具调用下标（而不是我们分配的输出项下标）：同一个上游
// 分片流里，同一个工具调用必须落到同一个 id 上。与 call_id 不是一个东西——
// call_id 是上游与调用方之间关联「调用」与「结果」用的（见
// responsesFunctionCallItem 的说明）。
func responsesCallItemID(responseID string, index int) string {
	return fmt.Sprintf("%s_call_%d", responseID, index)
}

// responsesOutput 把助手消息折成 output[] 的项。
//
// 顺序是「正文项在前、工具调用项在后」，与 Anthropic 那份（newClaudeMessage）
// 同一个取舍：顺序反了，按顺序渲染的界面会把「说一句话然后调用工具」显示成
// 「调用工具然后说话」。
//
// 一项都没有时（空回答）也要给一个空的正文项：output 是必填列表，而
// 「模型答了个空串」与「模型什么都没说」在客户端那里是同一件事，给一个
// 空正文项比给空数组更接近它。
func responsesOutput(id, status string, msg channel.Message) []any {
	var (
		text        strings.Builder
		calls       []any
		annotations []map[string]any
	)
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
			// 这里取**最后**一个带标注的正文块：relay.attachAnnotations
			// 只会把标注挂到一个块上，它是同一份数据的唯一来源。流式那条路
			// 没有「多段」这个概念（它只有一个正文项），两者不构成同一口径。
			if len(b.Annotations) > 0 {
				annotations = b.Annotations
			}
		case "tool_use":
			calls = append(calls, responsesFunctionCallItem{
				Type:      "function_call",
				ID:        responsesCallItemID(id, len(calls)),
				Status:    status,
				CallID:    b.ToolUseID,
				Name:      b.ToolName,
				Arguments: toolArguments(b.ToolInput),
			})
		}
		// 其余块类型（image / tool_result / thinking）不会出现在助手的回复里：
		// 前两种是调用方给进来的，第三种不是可见输出。与 newOpenAIChatCompletion
		// 同一个处置——丢掉比塞进正文更接近原意。
	}

	out := make([]any, 0, len(calls)+1)
	if s := text.String(); s != "" || len(calls) == 0 {
		content := responsesContent{
			Type: "output_text",
			Text: s,
		}
		// annotations 必须是数组，不能是 null（SDK 的模型里它是必填列表）。
		content.Annotations = annotationList(annotations)
		out = append(out, responsesMessageItem{
			Type:    "message",
			ID:      id + "_msg_0",
			Status:  status,
			Role:    "assistant",
			Content: []responsesContent{content},
		})
	}
	return append(out, calls...)
}

// annotationList 把标注摊成对外契约里的数组，没有时给空数组而不是 nil。
//
// 每一条都先摊平（见 responsesAnnotation），所以流式与非流式两条路走到这里时，
// 形状已经是一样的了。
func annotationList(annotations []map[string]any) []any {
	out := make([]any, 0, len(annotations))
	for _, a := range annotations {
		out = append(out, responsesAnnotation(a))
	}
	return out
}

// responsesAnnotation 把一条引用标注摊成 Responses 协议本身定义的那一份形状。
//
// 说的是哪一份：不是上游给的那一份，也不是转换层折出来的中间产物，而是 Responses
// 协议里 url_citation 自己的定义——字段与 type 并排：
//
//	{"type":"url_citation","url":"…","start_index":0,"end_index":12}
//
// 流式那条路原样转发上游的标注，给的就是这一份；非流式那条路经过 relaykit 的
// Responses→Chat 转换，而它按 chat 那套非官方扩展把 url_citation 折成了两层
// `{"type":"url_citation","url_citation":{…除 type 以外的字段}}`（见 relayconvert
// 的 responseAnnotationToChat）。同一个协议的两条路因此对同一样东西给出两种
// 形状，客户端得按「这次是不是流式」写两套解析，所以这里摊平。
//
// 只认 url_citation：relaykit 也只折这一种，其余类型它原样带过。照搬它这一条
// 判据，才不会凭空解释我们并不知道的标注类型——标注的字段集由上游决定，relay
// 那一层不解释它（见 internal/relay 的 attachAnnotations）。
//
// 摊平是无损的：折的那一步只是把除 type 以外的字段挪进内层，取出来即还原，
// 连我们不认识的字段（title 这类）也一起带出来。认不出是折过的（没有
// url_citation 这一层，或者那一层不是对象）就原样交出去，所以对已经是扁平形状的
// 标注是空操作——流式那条路走的就是这一条。
func responsesAnnotation(annotation map[string]any) map[string]any {
	typ, _ := annotation["type"].(string)
	if strings.TrimSpace(typ) != "url_citation" {
		return annotation
	}
	inner, ok := annotation["url_citation"].(map[string]any)
	if !ok {
		return annotation
	}
	out := make(map[string]any, len(inner)+1)
	for k, v := range inner {
		out[k] = v
	}
	// type 用协议里的那个值，与 relaykit 折进去时写的是同一个字符串。
	out["type"] = "url_citation"
	return out
}

// responsesStatus 把结束原因折成 Responses 的 status。
//
// 上游给的可能是两套词汇里的任意一套（见 openAIFinishReason），所以两套都收。
//
// 认不出来的值一律算 completed，而不是编一个 incomplete：把「上游正常结束」
// 说成「没答完」会让客户端去重试一个已经完整的回答，而反过来（把截断说成
// 完成）在这里做不到——截断的流根本走不到这个函数（见 stream.go 的说明）。
func responsesStatus(reason string) (status string, incomplete *responsesIncompleteDetails) {
	switch reason {
	case "length", "max_tokens":
		return "incomplete", &responsesIncompleteDetails{Reason: "max_output_tokens"}
	case "content_filter":
		return "incomplete", &responsesIncompleteDetails{Reason: "content_filter"}
	default:
		return "completed", nil
	}
}

// responsesFinishEvent 是流式收尾那个事件名：正常结束与没答完是两个事件。
func responsesFinishEvent(status string) string {
	if status == "incomplete" {
		return "response.incomplete"
	}
	return "response.completed"
}
