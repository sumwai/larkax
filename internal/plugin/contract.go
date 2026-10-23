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
	"reflect"
	"sort"
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
	RequestID string `json:"requestId"`
	// CanonicalModel 是**入站解析之后的**规范模型名：注册表的模型匹配
	// （meta.models）与 onRoute 的上下文用的都是它。
	//
	// 它不是调用方在请求体里写的那个名字：请求用了入站别名时，入口已经把
	// 别名翻成了它指向的模型（见 internal/api 的 serve）。
	CanonicalModel string `json:"canonicalModel"`
	// UpstreamModel 是**别名翻译之前**的模型名。
	//
	// 名字说的是「上游模型名」，但 onRequest 时刻那个值还不存在：上游名是
	// 逐渠道配的（同一个规范名在这家叫 vendor/x、在那家叫 x），要知道它得先
	// 选中渠道，而选路在 onRequest 之后（见 docs/plugin-contract.md 的接线
	// 一节与 internal/route 的 upstreamRequest）。所以它恒等于 CanonicalModel。
	//
	// **不要拿它拼上游特有的请求体**：它不是实际发给上游的名字。上游实际用了
	// 哪个名字，看 onResponse 的 body.model（那是上游回报的，见
	// channel.Response.UpstreamModel）——流式请求拿不到 onResponse。
	//
	// 字段名保留是为了不打断已有插件；语义以这里为准。
	UpstreamModel string `json:"upstreamModel"`
	Protocol      string `json:"protocol"`
	Streamed      bool   `json:"streamed"`
	// Body 是请求体的**协议中立视图**，取自别名翻译之前的那份请求：
	// `body.model` 与 UpstreamModel 同值，也是规范名。插件只能通过
	// RequestPatch.Body 改其中列出的字段——**能读到什么与能改什么是两件事**，
	// 读整个 body 是必要的（要按内容判断），改则限定在白名单。
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

// decodePatch 严格解析插件返回的补丁：出现契约之外的字段就报错。
//
// # 为什么不能宽松
//
// 宽松解析在这里不是「不够严谨」，而是**造出最难查的一类静默**。插件写
// `return { channeId: 7 }` 时那个字段被丢掉，于是这次「不决策」——
// 作者看到的是「我的插件没生效」，而没有任何一条日志或错误告诉他为什么。
// 越权也是同一个形状：插件写 `{ body: { model: "更贵的模型" } }` 时宽松解析
// 把 model 悄悄丢掉，作者以为改成功了、而上游收到的是原模型，
// 一次「看起来生效了」的越权尝试。
//
// 这与 parseMeta 对插件元数据的态度是同一个：作者写给宿主的声明，
// 写错了要立刻说，而不是让它静默失效（见 registry.go 的 parseMeta）。
//
// # 报错用 ErrPatchScope
//
// 与 RequestBodyPatch 共用同一个哨兵：「写了契约里没有的字段」与「试图改一个
// 改不了的字段」在实现上是同一件事——字段不在类型里，而作者要做的动作也一样，
// 都是把字段名改对。分成两个哨兵只会让同一类错误有两个名字，
// 调用方要同时判两个才算判全。
//
// # 报错里的字段清单是推出来的
//
// 从结构体标签推（见 contractFieldNames），不手写一份清单：手写的那份与结构体
// 会各自漂移，而它只出现在报错文本里——漂移的表现是「报错少列了一个本来能用
// 的字段」，没有测试会因此失败。
func decodePatch(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		if errors.Is(err, ErrPatchScope) {
			// 内层已经按契约报过错了（`body` 里那一份由 RequestBodyPatch 报出），
			// 原样传出去。再包一层会让「可改的字段只有」出现两次，
			// 而两份清单指的是不同的东西——一份是请求体里能改的字段，
			// 一份是这个补丁能给的字段。作者要的是**出错那一层**的那份。
			return err
		}
		return fmt.Errorf("%w: %w（可改的字段只有 %s）",
			ErrPatchScope, err, strings.Join(contractFieldNames(target), "、"))
	}
	return nil
}

// contractFieldNames 按声明顺序取出一份补丁类型的契约字段名（json 标签那部分）。
//
// 只用于报错文本：真正决定「哪些字段收得下」的是结构体本身，
// 也就是 DisallowUnknownFields 读的那份标签。
func contractFieldNames(target any) []string {
	t := reflect.TypeOf(target)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
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
	var out plain
	if err := decodePatch(data, &out); err != nil {
		return err
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

// UnmarshalJSON 严格解析 onRequest 的返回值。
//
// 外层与内层用同一把尺子：只严格解析 Body 的话，`return { bodyy: {...} }`
// 仍然静默无效果——而它与 `{ body: { model: ... } }` 是同一类问题，
// 只是错在更外面一层。理由见 decodePatch。
func (p *RequestPatch) UnmarshalJSON(data []byte) error {
	type plain RequestPatch
	var out plain
	if err := decodePatch(data, &out); err != nil {
		return err
	}
	*p = RequestPatch(out)
	return nil
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
	ChannelID   int64  `json:"channelId"`
	OwnerUserID int64  `json:"ownerUserId"`
	Weight      int    `json:"weight"`
	Priority    int    `json:"priority"`
	Health      string `json:"health"`
	// Remaining 是剩余额度，**只在本次候选集内可与其他候选的 Remaining
	// 直接比较时才给**。它是 null 的两种情况：
	//
	//   - 这个渠道查不到额度。注意这与「额度为零」不同，后者是 0。
	//   - 候选集里出现了两种以上额度单位。按 token 计的套餐与按元计的账户
	//     之间，「还剩 100」谁更该先用掉取决于我们不知道的定价与消耗速度，
	//     比出来的次序完全由单位决定。此时**所有**候选一律为 null，
	//     包括单位相同的那几个。
	//
	// 所以**不要拿它跨候选直接比**：JS 里 null 参与算术会变成 0，
	// `b.remainingEstimate - a.remainingEstimate` 不会报错，只会把 null
	// 当成「余量为零」排到最前面——一次静默的选错渠道。判断「可不可比」是
	// **集合级**的，已经由构造候选集的一方做完（判据见 route.MixedQuotaUnits，
	// 与内核打分共用同一处实现），插件只需在 null 时当作「这里没有可用的信息」。
	Remaining *float64 `json:"remainingEstimate"`
	// Confidence 是 Remaining 这个数的可信度，0..1。它与 Remaining 同生共死：
	// Remaining 为 null 时它一定是 0，它非 0 就一定能拿到数——免得出现
	// 「可信度 1 而数字是 null」这种读不通的组合。上游实测的给 1，
	// 本地推算的估算值打折。
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

// UnmarshalJSON 严格解析路由决策：契约之外的字段报错，理由见 decodePatch。
//
// 这一处尤其要紧：写错字段名的表现是 ChannelID 停在 0，而 0 归一成
// 「没给决策」（见 Session.OnRoute）——于是插件「跑过了、没报错、也没生效」，
// 三件事同时成立，作者无从下手。
func (d *RouteDecision) UnmarshalJSON(data []byte) error {
	type plain RouteDecision
	var out plain
	if err := decodePatch(data, &out); err != nil {
		return err
	}
	*d = RouteDecision(out)
	return nil
}

// StreamContext 是 onStream 看到的当前块。
//
// 它**只在内容块上出现**：usage 与 done / error 是这次流的计费依据与框架，
// 不进插件的手（见 gateway.Hooks.OnStream），所以下面几个字段说的都是正文
// 或工具调用那两种块。
type StreamContext struct {
	RequestID string `json:"requestId"`
	// Chunk 是当前这一块。形状由内核给出（见 adapter.go 的 StreamChunkFrom）：
	// delta 是 `{ kind, text }`，tool_use 是 `{ kind, toolCall: {...} }`。
	// 插件改这一块的方式是 drop / replace 正文，而不是改这个对象——
	// 它是一份快照，改它不影响任何东西。
	Chunk any `json:"chunk"`
	// Index 是这一块在本次流里的序号，从 0 起，只数内容块。
	Index int `json:"index"`
	// Text 与 Tokens 是**这一块之前**已经累积的正文与它的 token 数，
	// 不含当前块——当前块在 Chunk 里，插件要看全文自己拼。
	//
	// 两者与内核的分词累加器是同一个数，因为它们是**同一处**累加出来的：
	// 插件 Replace 之后，进累加器的是替换后的正文（见 StreamDecision）。
	Text   string `json:"accumulatedText"`
	Tokens int64  `json:"accumulatedTokensApprox"`
}

// StreamDecision 是 onStream 的返回值。
//
// Replace 是指针：`replace: ""` 是「把这一块替换成空」，与「不替换」不同。
//
// Drop 与 Replace 的差别只在**这一块还有没有正文**：两者都不把原文发给
// 调用方，也都不把原文算进累计正文（累计正文是流式中断时的计费依据，
// 见 StreamContext.Text）。所以 `drop: true` 与 `replace: ""` 在计费上
// 是同一件事——这不是漏了，是刻意的：同一类决策只有一套口径。
//
// Replace 只对正文块（`chunk.kind === "delta"`）有效。工具调用块上它没有
// 可作用的对象，按无效果处理——那一种块要用的是 Drop 或 Terminate。
type StreamDecision struct {
	Drop      bool    `json:"drop,omitempty"`
	Replace   *string `json:"replace,omitempty"`
	Terminate bool    `json:"terminate,omitempty"`
}

// UnmarshalJSON 严格解析逐块决策：契约之外的字段报错，理由见 decodePatch。
//
// 它每个块都要解一次，所以「拼错就报错」在这里的收益也最大：错一次就报一次，
// 而不是让插件在整条流上一直不生效。
func (d *StreamDecision) UnmarshalJSON(data []byte) error {
	type plain StreamDecision
	var out plain
	if err := decodePatch(data, &out); err != nil {
		return err
	}
	*d = StreamDecision(out)
	return nil
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

// UnmarshalJSON 严格解析响应补丁：契约之外的字段报错，理由见 decodePatch。
//
// Metadata 是自由形状的，它的**键**不受影响：严格解析管的是这个结构体自己的
// 字段名（`text` / `metadata`），map 里放什么由插件决定。
func (p *ResponsePatch) UnmarshalJSON(data []byte) error {
	type plain ResponsePatch
	var out plain
	if err := decodePatch(data, &out); err != nil {
		return err
	}
	*p = ResponsePatch(out)
	return nil
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

// UnmarshalJSON 严格解析插件上报的用量：契约之外的字段报错，理由见 decodePatch；
// 字段名对而值为负也报错，理由见 checkNonNegative。
//
// 这里报错的对象多半不是「越权」而是「写错」——用量里没有金额字段，
// 所以 `{ amount: 5 }` 与 `{ promptToken: 5 }` 都是「契约里没有这个名字」。
// 两者都要报：前者正是「不能报价」这条在解析期的落点，
// 后者的静默失效会让一次漏报看起来像一次正常上报。
//
// Units 与 ResponsePatch.Metadata 同理：它的**键**是自由形状的，
// 严格解析只管 `units` 这个名字本身。
//
// 键自由，**值不自由**：负数会被拒掉，理由见 checkNonNegative。那不是
// 「键不能自定义」的例外——插件照样可以用任何键名，受约束的只是数量。
func (f *UsageFacts) UnmarshalJSON(data []byte) error {
	type plain UsageFacts
	var out plain
	if err := decodePatch(data, &out); err != nil {
		return err
	}
	// 先判错再赋值，与同文件其它 UnmarshalJSON 一致：报错时接收者里不该
	// 留着那份被判为无效的数据。今天的调用方（Session.OnUsage）在失败时
	// 整份丢掉，所以还没有实际后果——但「fail closed」要落到接收者上，
	// 不能靠调用方每次都记得丢。
	if err := UsageFacts(out).checkNonNegative(); err != nil {
		return err
	}
	*f = UsageFacts(out)
	return nil
}

// errNegativeUsage 是插件上报的用量为负时的错误。
//
// 与 errNonPositiveMaxTokens 同族：都是「字段名对而值不能用」，共用 ErrPatchScope。
// 文本里带字段名与值——token 字段有五个，Units 的键又由插件自己定，
// 只说「用量为负」查不出是哪一个。Units 的键写成 `units["searches"]`
// 那种形状，指得到 JSON 里的位置。
func errNegativeUsage(field string, v int64) error {
	return fmt.Errorf("%w: %s 不能为负，收到 %d", ErrPatchScope, field, v)
}

// checkNonNegative 逐个检查上报的用量没有负数。
//
// # 为什么必须在解码层拦
//
// 负数进计费没有一个方向是对的，而下游各自只挡一半：
//
//   - **completion 为负**：声明了「completion 含推理」时（OpenAI 系的常规
//     口径）billing.FromChannelUsage 把它钳成 0——输出费用整块消失。金额侧
//     没有任何信号（Capped 仍是 false），只有一条
//     billing.ReasoningExceedsCompletion 的告警会响（pipeline 拿它发 Warn），
//     而那条告警说的是「推理量多于输出总量」，与「用量是负数」不是同一件事；
//     没声明时它原样进 billing.Validate，用量合计为负就**整次结算失败**——
//     一次上报把一笔已经产生的费用变成一笔收不回来的账。
//   - **promptTokens 为负**：相减之后被钳成 0，输入费用同样消失。
//   - **reasoning 与缓存量为负**：相减减掉了一个负数，把另一项**抬高**。
//
// 下游两处各挡一半，说明这件事本来就该在**唯一**的入口处判：插件给的值
// 从这里进来，别处再判都是在补一个已经漏出去的洞。
//
// # Units 为什么也在这里
//
// 它的负数由 billing.Validate 拦下（`计量项 %q 的数量为负`），代价与上面那条
// 一模一样——**整次结算失败**，而那时上游那次调用已经成功、token 已经花掉。
// 判据是「下游会怎么处置」而不是字段的类型：后果相同就该同处置。
//
// # 不查 NaN 与 ±Inf
//
// 不是漏了：这个结构里每个数都是 int64（`channel.Usage` 也是），它们表达
// 不出来；`1e999` 这种溢出由解码器自己拒（cannot unmarshal ... into int64），
// `NaN` / `Infinity` 根本不是合法 JSON。所以这里没有第二道检查可加。
func (f UsageFacts) checkNonNegative() error {
	for _, item := range []struct {
		name string
		v    *int64
	}{
		{"promptTokens", f.PromptTokens},
		{"completionTokens", f.CompletionTokens},
		{"cacheReadTokens", f.CacheReadTokens},
		{"cacheWriteTokens", f.CacheWriteTokens},
		{"reasoningTokens", f.ReasoningTokens},
	} {
		if item.v != nil && *item.v < 0 {
			return errNegativeUsage(item.name, *item.v)
		}
	}
	// 先按键排序再查：map 的遍历顺序是随机的，而这个报错会进日志与测试
	// ——同一份坏上报报出哪个键，不该每次不一样。
	keys := make([]string, 0, len(f.Units))
	for k := range f.Units {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := f.Units[k]; v < 0 {
			return errNegativeUsage(fmt.Sprintf("units[%q]", k), v)
		}
	}
	return nil
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
