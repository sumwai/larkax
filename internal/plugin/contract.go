// Package plugin 把插件挂到请求生命周期上：注册、按 hook 分发、决定失败怎么办。
//
// 三个子包各管一件事：
//
//	plugin/sandbox  跑一段 JS（moejs 封装、限额、中断）
//	plugin/host     宿主能力（日志、日历、KV），每个函数自带 deadline
//	plugin          注册表、五个 hook 的类型与分发、失败策略
//
// # 权限边界落在「类型里没有那个字段」
//
// 插件能改什么，由下面那些 Patch 类型的**字段表**决定，而不是靠运行时检查。
// 没有出现在 Patch 里的东西就改不了：它到不了调用方，也不会有第二处代码
// 需要记得去过滤。契约里「不能碰钱、不能读渠道密钥」两句话在实现上就是
// 「计价与凭证的类型不出现在这个包里」。
//
// # 一个请求只跑一个插件
//
// 多个插件如何串联（后一个看到前一个改过的，还是都看到原始数据）是一个
// 未定的契约问题：它决定了插件作者的调试体验，也决定了失败时的回退语义。
// 这里按**最保守**的做法收窄——按优先级只取一个，其余不参与。
// 收窄是可逆的（放宽比收紧容易），而猜错链式语义会让已经写好的插件行为变化。
//
// 注册表本身按多插件设计（可以注册任意多个、按匹配条件筛出候选），
// 所以将来放开串联不需要改动注册与编译路径。
package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// Hook 名。取值必须与 sandbox.HookNames 一致（两份独立的字面量，改一处要
// 同时改另一处），顺序即生命周期的先后。
const (
	HookRequest  = "onRequest"
	HookRoute    = "onRoute"
	HookStream   = "onStream"
	HookResponse = "onResponse"
	HookUsage    = "onUsage"
)

// RequestContext 是 onRequest 看到的请求。
//
// 它是一个**独立的快照类型**，不是 channel.Request 的别名：插件看到的东西
// 要少得多（没有凭证、没有渠道配置），而共用一个类型会让「加一个字段」
// 悄悄扩大插件的可见范围。
type RequestContext struct {
	RequestID      string `json:"requestId"`
	CanonicalModel string `json:"canonicalModel"`
	UpstreamModel  string `json:"upstreamModel"`
	Protocol       string `json:"protocol"`
	Streamed       bool   `json:"streamed"`
	// Body 是上游请求体。插件只能通过 RequestPatch.Body 改其中列出的字段——
	// **能读到什么与能改什么是两件事**，读整个 body 是必要的（要按内容判断），
	// 改则限定在白名单。
	Body map[string]any `json:"body"`
	// Headers 是客户端请求头里**白名单前缀**可见的那部分。
	//
	// 当前固定为空：哪些头该给插件看（要不要给到租户标识）、给的粒度是什么，
	// 属于对外契约，尚未定。空对象比「先全给」保守得多，后者是泄露方向。
	Headers map[string]string `json:"headers"`
	// User 是请求方。**只有 ID**：邮箱、密钥、套餐细节都不在这里。
	User RequestUser `json:"user"`
}

// RequestUser 是插件看到的请求方。
//
// 单独一个结构体而不是在 RequestContext 上放一个 int64：契约里它是
// 一个嵌套对象（`user.id`），而「以后要不要加一个非敏感的字段」
// 这个问题在这里有一个比改签名更轻的答案。
type RequestUser struct {
	ID int64 `json:"id"`
}

// RequestBodyPatch 是插件能改的请求体字段。
//
// 用指针区分「没给」与「给了零值」：温度改 0 与不改温度是两件事，
// 而 float64 的零值让它们无法区分。
type RequestBodyPatch struct {
	Messages    []any    `json:"messages,omitempty"`
	System      *string  `json:"system,omitempty"`
	MaxTokens   *int64   `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Tools       []any    `json:"tools,omitempty"`
	ToolChoice  any      `json:"tool_choice,omitempty"`
}

// UnmarshalJSON 严格解析请求体补丁：出现白名单之外的字段就报错。
//
// **宽松解析在这里是危险的**，而不是仅仅「不够严谨」：插件写
// `{ body: { model: "更贵的模型" } }` 时，宽松解析会把 model 悄悄丢掉，
// 于是插件作者以为改成功了、而上游收到的是原模型——一次「看起来生效了」
// 的越权尝试。报错让它变成一次显式的契约违规。
//
// 同理适用于拼错的字段名（`maxTokens`）：静默丢掉的表现是
// 「插件改了但没生效」，而那是最难排查的一类。
func (p *RequestBodyPatch) UnmarshalJSON(data []byte) error {
	type plain RequestBodyPatch // 去掉方法集，避免递归调到自己
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var out plain
	if err := dec.Decode(&out); err != nil {
		return fmt.Errorf("%w: %w（可改的字段只有 %s）",
			ErrPatchScope, err, strings.Join(patchableBodyFields, "、"))
	}
	*p = RequestBodyPatch(out)
	return nil
}

// empty 判断这份补丁有没有真的改动什么。
//
// `{ body: {} }` 解出来是非 nil 的空结构体，而它表达的是「没改任何东西」。
// 不归一化的话调用方要对「有补丁但补丁是空的」多判一次。
func (p *RequestBodyPatch) empty() bool {
	if p == nil {
		return true
	}
	switch {
	case p.Messages != nil, p.System != nil, p.MaxTokens != nil, p.Temperature != nil,
		p.TopP != nil, p.Stop != nil, p.Tools != nil, p.ToolChoice != nil:
		return false
	}
	return true
}

// RequestPatch 是 onRequest 的返回值。
//
// **模型名不在里面**：改模型即改价，而定价权只在路由层。`stream`、`user`
// 与任何鉴权头同理——它们要么改价，要么改身份。
type RequestPatch struct {
	Body    *RequestBodyPatch `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// RouteContext 是 onRoute 看到的候选集。
type RouteContext struct {
	RequestID      string           `json:"requestId"`
	CanonicalModel string           `json:"canonicalModel"`
	Candidates     []RouteCandidate `json:"candidates"`
}

// RouteCandidate 是一个候选渠道。
//
// **没有 base_url，也没有凭证**：插件只需要知道「有哪些选择、各自什么状态」，
// 而 base_url 足以让它推断出上游是谁。状态字段是为了让插件能表达
// 「优先用快过期的那个」这类偏好。
type RouteCandidate struct {
	ChannelID    int64    `json:"channelId"`
	OwnerUserID  int64    `json:"ownerUserId"`
	Weight       int      `json:"weight"`
	Priority     int      `json:"priority"`
	Health       string   `json:"health"`
	Remaining    *float64 `json:"remainingEstimate"`
	Confidence   float64  `json:"confidence"`
	UpstreamCost *float64 `json:"upstreamCostHint"`
	ResetsAt     *string  `json:"resetsAt"`
}

// RouteDecision 是 onRoute 的返回值。
//
// ChannelID **必须在 ctx.Candidates 之内**，否则按插件失败处置——
// 允许它选候选集之外的渠道等于让插件决定流量去哪，
// 而候选集是配额、健康度与商家配置共同算出来的。
type RouteDecision struct {
	ChannelID int64  `json:"channelId"`
	Reason    string `json:"reason,omitempty"`
}

// StreamContext 是 onStream 看到的当前块。
type StreamContext struct {
	RequestID string `json:"requestId"`
	// Chunk 是已解析的当前块。它的形状由上游协议决定，
	// 所以插件改它的方式是 drop / replace 正文，而不是改这个对象。
	Chunk  any    `json:"chunk"`
	Index  int    `json:"index"`
	Text   string `json:"accumulatedText"`
	Tokens int64  `json:"accumulatedTokensApprox"`
}

// StreamDecision 是 onStream 的返回值。
//
// Replace 是指针：`replace: ""` 是「把这一块替换成空」，与「不替换」不同。
type StreamDecision struct {
	Drop      bool    `json:"drop,omitempty"`
	Replace   *string `json:"replace,omitempty"`
	Terminate bool    `json:"terminate,omitempty"`
}

// ResponseContext 是 onResponse 看到的响应。
type ResponseContext struct {
	RequestID string         `json:"requestId"`
	Status    int            `json:"status"`
	Body      map[string]any `json:"body"`
	// Usage 是上游给的真实用量，**只读**：它是计费的依据，
	// 插件要改用量只能走 onUsage 的上报通道，那条通道只收数量不收金额。
	Usage *UsageFacts `json:"usage"`
}

// ResponsePatch 是 onResponse 的返回值。
//
// 只有正文与元数据。**没有 usage、没有模型名、没有 finish_reason**：
// 前两个会改价与改身份，第三个会改变「这次回答是否完整」的判定。
type ResponsePatch struct {
	Text     *string        `json:"text,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// UsageContext 是 onUsage 看到的原始用量。
type UsageContext struct {
	RequestID string `json:"requestId"`
	Protocol  string `json:"protocol"`
	Raw       any    `json:"raw"`
}

// UsageFacts 是插件上报的用量。
//
// **只有数量，没有金额。** 定价权只在内核：插件报价等于把定价权交出去，
// 而定价错了是这套系统里最贵的一类 bug——它不报错，只让账目悄悄错掉。
//
// 全部用指针：0 个补全 token 与「没上报补全 token」是两件事，
// 前者是事实（上游返回了空回答），后者是插件没管这一项。
type UsageFacts struct {
	PromptTokens     *int64           `json:"promptTokens,omitempty"`
	CompletionTokens *int64           `json:"completionTokens,omitempty"`
	CacheReadTokens  *int64           `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64           `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  *int64           `json:"reasoningTokens,omitempty"`
	Units            map[string]int64 `json:"units,omitempty"`

	// CompletionIncludesReasoning 声明 CompletionTokens 里是否**已经含**
	// ReasoningTokens，语义与 channel.Usage 上的同名字段一致。
	//
	// 它不是描述而是**计费口径的一部分**：声明为「已含」时结算先把推理从
	// completion 里减掉，声明为「不含」时两项各乘一次单价。两种口径在上游都
	// 真实存在，而光看数字判不出来——completion=100、reasoning=50 既可能是
	// 「100 里含 50」，也可能是「100 可见输出 + 50 推理」。所以这个关系由
	// 知道协议约定的那一层如实声明，不在计费侧按启发式推断。
	//
	// 指针的三种取值各有含义：
	//
	//   - nil：**没声明**。重建回协议层用量时沿用内核原有的口径，见
	//     UsageFacts.ChannelUsage。
	//   - &true：completion 已含推理（OpenAI 的 completion_tokens 含
	//     completion_tokens_details.reasoning_tokens，Anthropic 的
	//     output_tokens 含扩展思考）。
	//   - &false：推理是单独报出的，与 CompletionTokens 互斥。
	//
	// 用指针而不是 bool：「没声明」与「声明为不含」在计费上不是同一件事，
	// 前者要沿用内核口径、后者要覆盖它，而 bool 的零值会把前者悄悄变成后者——
	// 一个没声明口径的插件会被当成「推理单独报出」，于是那 50 个推理 token
	// 与它们在 completion 里的那一份被各乘一次单价，账单只是偏高。
	CompletionIncludesReasoning *bool `json:"completionIncludesReasoning,omitempty"`
}

// Result 是一次 hook 调用的结果。
//
// 三种结局用一个结构体表达，而不是 `(*T, error)` 那种两值返回：
//
//   - Failure 为 nil、Patch 可能为 nil：正常（插件没给补丁也是正常）
//   - Failure 非 nil 且 Blocked 为 false：**按没发生处理**，Patch 一定为 nil
//   - Failure 非 nil 且 Blocked 为 true：请求失败，Err() 给出那个错误
//
// 两值返回看着更简单，但它把「没有补丁」与「出错了」挤进同一对返回值里，
// 于是每个调用点都要写一遍「err 为 nil 但 patch 也为 nil 是什么意思」。
// 那种解释散在各处，迟早有两处不一致。
type Result[T any] struct {
	Patch   *T
	Failure *sandbox.Failure
	Blocked bool
}

// Failed 表示这次调用出错了。非阻断的失败也返回 true——
// 「出错了但请求继续」与「没出错」在观测上必须能分开。
func (r Result[T]) Failed() bool { return r.Failure != nil }

// Err 在阻断型失败时给出请求该失败的原因，其余情况返回 nil。
//
// 调用方统一用 `if res.Err() != nil` 判「这次要不要中止」，
// 于是「什么算阻断」只有一处定义。
func (r Result[T]) Err() error {
	if r.Failure == nil || !r.Blocked {
		return nil
	}
	return fmt.Errorf("插件声明为阻断型，它的 %s 失败: %w", r.Failure.Hook, r.Failure)
}

// patchOf 是失败时的统一处置：非阻断就丢掉补丁。
//
// 「出错就丢掉这次改动」而不是「用部分改动」：部分应用过的补丁是没法解释的状态，
// 而插件作者看到的会是「有的字段生效了、有的没有」——比整个不生效难查得多。
func patchOf[T any](patch *T, failure *sandbox.Failure, blocking bool) Result[T] {
	if failure == nil {
		return Result[T]{Patch: patch}
	}
	return Result[T]{Failure: failure, Blocked: blocking}
}

// ErrUnknownModel 表示插件声称支持的模型不在本次请求里。
var ErrUnknownModel = errors.New("插件不支持这个模型")

// ErrChannelOutsideCandidates 表示插件选了候选集之外的渠道。
var ErrChannelOutsideCandidates = errors.New("插件选了候选集之外的渠道")

// pickChannel 校验插件选中的渠道在候选集里。
//
// 这是**唯一一处必须在校验上花力气的地方**：其余补丁字段本身就是受限的
// （改不了金额、改不了身份），而渠道 ID 是一个可以随便写的整数。
//
// ChannelID 为 0 时算「没给决策」：JS 里 `return {}` 解出来就是 0，
// 而 0 不是一个合法渠道 ID（channel 表的主键从 1 起）。校验必须在
// 这之前做，否则「没给决策」会被当成一次越界。
func pickChannel(decision *RouteDecision, cands []RouteCandidate) error {
	if decision == nil {
		return nil
	}
	for _, c := range cands {
		if c.ChannelID == decision.ChannelID {
			return nil
		}
	}
	return fmt.Errorf("%w: %d", ErrChannelOutsideCandidates, decision.ChannelID)
}
