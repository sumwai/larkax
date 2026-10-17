// Package channel 定义上游渠道的适配器接口与注册表。
//
// 分层是这样的：
//
//	gateway  ──►  route  ──►  channel.Adapter  ──►  上游
//	                │                │
//	                │                └── 只做协议转换：把统一请求翻译成上游格式，
//	                │                    把上游响应翻译回统一结构
//	                └── 负责选路、重试、换渠道
//
// 为什么把「协议转换」与「选路重试」分开：两者的变化原因完全不同。
// 新接一家上游 → 加一个适配器；改选路策略 → 改路由层。
// 混在一起的话，每接一家上游都要在重试逻辑里再判断一次「这家算不算失败」。
//
// 适配器**不持有密钥**：凭证由调用方解密后作为参数传入。这样插件层接触不到
// 解密路径，适配器也不必知道信封加密的存在。
package channel

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sumwai/larkax/internal/quota"
)

// Protocol 是协议族的名字。
//
// 它决定用哪套 usage 口径、哪种流式分片格式、以及请求体长什么样。
//
// 这个类型有三个用它的地方，指的是同一件事的三个视角：**调用方**说的是
// 哪套协议（由请求路径决定：/v1/chat/completions 是 openai，
// /v1/messages 是 anthropic，/v1/responses 是 openai-responses，
// /v1/systemone 是 systemone），
// **适配器**自述实现的是哪套（Adapter.Protocol()，注册表拿它做自检），
// 以及**账单**记下这次请求走的是哪条入口（pipeline.Request.Protocol，
// 落进 billing_requests.protocol，供事后按协议统计）。
//
// 这三个视角**不要求一一对应**：一个只有适配器的协议族（渠道说它，但入口
// 没有对应的路径）是能存在的，那时管道里的请求仍是统一格式（贴近 chat
// completions），由适配器经 internal/relay 转换过去。今天四个取值两边都有，
// 但两份清单判的不是同一件事，所以加取值时仍要各自过一遍。
//
// 渠道表里**没有**对应的列：一个渠道说哪套协议，由它的 type 指向的适配器
// 决定，不再单独存一份。存两份的话，第二份只会填错而无人发现。
//
// 与渠道类型（Channel.Type）是两个维度：同一家上游可能同时提供多套协议，
// 同一套协议也被多家上游使用。
type Protocol string

// 各协议的取值。
//
// 取值与渠道表里的 channels.type 是同一个字符串（适配器的 Type()）。
// 已注册的适配器只有这几个，调用方的入口也对应这几条路径。
// 「大体像 OpenAI 但有几处不一样」不是另一个协议族——它是某个渠道的私有
// 差异，归属在 Channel.Setting 里。
//
// # systemone 为什么算一个协议族而不是某套协议的变体
//
// 它的请求与响应形状与 chat 没有任何共同点：请求是 state 加一组带类型的
// 判定问题（questions 是按 id 索引的**对象**），响应是概率而不是文本，
// 而且从不流式。所以它不是「OpenAI 协议加几个参数」，中间表示也不共用
// （见 internal/systemone）。用同一个 Protocol 值会让账单把两类请求记成
// 同一套协议，而那张表正是事后按协议统计的依据。
const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	// ProtocolOpenAIResponses 是 OpenAI 的 Responses 表面：请求体是
	// input/instructions 而不是 messages，用量在顶层且缓存与推理两项
	// 嵌在 details 里，流式是一串带名字的事件而不是裸 delta。
	//
	// 它与 ProtocolOpenAI 分开而不是合并成一个「OpenAI 系」：两者的请求体、
	// 流式分片与用量字段名都不同，合并之后每个用它的地方都要再判一次
	// 「这次是哪一种」——那正是协议族这个概念要消掉的东西。
	ProtocolOpenAIResponses Protocol = "openai-responses"

	ProtocolSystemOne Protocol = "systemone"
)

func (p Protocol) valid() bool {
	switch p {
	case ProtocolOpenAI, ProtocolAnthropic, ProtocolOpenAIResponses, ProtocolSystemOne:
		return true
	}
	return false
}

// Channel 是某一次调用要用的渠道信息，由路由层从库里取出后传入适配器。
//
// 做成值类型而不是让适配器自己去查库：适配器要做的事与存储无关，
// 而它每多一个依赖，测试就多一层要搭的台子。
type Channel struct {
	ID      int64
	Name    string
	Type    string
	BaseURL string
	// Credential 是**已解密**的凭证。适配器不得记录、不得回传、不得写进错误信息。
	Credential string
	// Models 是该渠道支持的规范模型名。
	Models []string
	// Setting 是该渠道的私有配置，例如是否支持某个可选参数。
	//
	// # 键归适配器解释，选路层不认
	//
	// 一个键是什么意思、看不看它，由**该渠道 type 指向的那个适配器**决定；
	// 选路层不读这里的任何键。理由：渠道私有差异是协议细节——「大体像
	// OpenAI 但有几处不一样」说的正是「这个渠道与它的协议族差在哪」，
	// 而选路层要认的判据（能力、上下文上限）在 model_catalog 那边，
	// 不该再从自由形态的 jsonb 里读一份。混着用的后果是「这个键谁认」
	// 变成读遍代码才知道的事。
	//
	// 因此管理面不做键名校验：拼错一个键不会报错，只会静默不生效。
	//
	// # 今天还没有读者
	//
	// 目前四个适配器与选路层都没有任何一处读它，所以写进来的键一律不影响
	// 请求路径。这件事由 SettingReadByAdapters 声明，管理面据此在写入之后
	// 与列举时把「没有消费者」说出来（见 SettingUnreadNote）。第一个键被
	// 某个适配器实现时，把它改成 true，那几处提示一起消失。
	Setting map[string]any
}

// Supports 判断该渠道是否声称支持某个规范模型名。
func (c Channel) Supports(canonicalModel string) bool {
	for _, m := range c.Models {
		if m == canonicalModel {
			return true
		}
	}
	return false
}

// PromptText 把所有消息里的正文块拼成一段文本。
//
// 它是 token 计数的输入（见 internal/tokens）：预留要在调上游之前估输入，
// 流式中断时要估整条请求。两处用同一份口径——各拼一次的话，同一个请求
// 在预留与结算里会被算成两个不同的量，而差额没有解释。
//
// 只拼正文：图片与工具参数是**另一个口径**的东西，各家对它们的计量方式
// 不同，按字符估出来的数字比正文部分的估算更不可信。工具定义的框架开销
// 另行按条数补（见 tokens.ObserveRequest 的 messages / tools 参数）。
func (r *Request) PromptText() string {
	var b strings.Builder
	for _, m := range r.Messages {
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// Request 是统一格式的请求。
//
// 它刻意贴近 OpenAI 的 chat completions 形状：那是这个领域事实上的公共语汇，
// 各家 SDK 与上游都围着它转。换一套自定义的抽象会让每个适配器都要多写一层映射，
// 而那层映射不携带任何新信息。
//
// 上游特有的、公共语汇里没有的参数走 Extra，避免为每一家的每个新参数
// 都往这里加一个字段。
type Request struct {
	// Model 是**上游模型名**，不是规范名。规范名到上游名的映射由路由层完成，
	// 因为一个规范名在不同渠道下对应不同上游名。
	Model string

	Messages []Message

	// MaxTokens 是这次请求允许的最大输出。**到适配器时它总是正数**：
	// 入口侧的 `pipeline.Request.validate` 在预留之前就拒掉 0
	// （没有它算不出预留上界，见 `billing.ErrNoUpperBound`），所以「0 表示
	// 未指定、交给上游默认值」这条路走不到——它是给将来直接调用适配器的
	// 场景留的语义，不是当前的取值域。
	MaxTokens int
	// Temperature 用指针：0 是合法取值，与「未指定」必须区分开。
	Temperature *float64
	TopP        *float64

	// ReasoningEffort 是推理深度（low/medium/high 之类）。
	// 各家叫法与取值不同，由适配器翻译；空字符串表示未指定。
	ReasoningEffort string

	Tools []Tool

	Stream bool

	// Extra 是上游特有参数，由适配器决定是否透传。
	Extra map[string]any

	// Headers 是要附加到上游请求上的自定义头。
	//
	// 用途是插件系统里插件加的 x-larkax-* 头——**它不是通用透传通道**。
	// 适配器在写入前必须滤掉鉴权头与协议头（Authorization、x-api-key、
	// anthropic-version、Content-Type 等），见 httpx.SafeRequestHeaders。
	//
	// 为什么滤在适配器而不只在插件层：插件层的白名单写错时，
	// 适配器是最后一道。而「插件能覆盖 Authorization」意味着它能
	// 冒充别的渠道身份。
	Headers map[string]string
}

// Message 是一条对话消息。
type Message struct {
	Role    string
	Content []ContentBlock
}

// ContentBlock 是消息内容的一块。
//
// 用块而不是纯字符串：多模态与工具调用都要求内容是有结构的序列，
// 而纯字符串模型会迫使每个适配器自己做一轮隐式解析。
type ContentBlock struct {
	// Type 取值：text、image、tool_use、tool_result、thinking。
	Type string
	Text string

	// 工具调用相关
	ToolUseID string
	ToolName  string
	// ToolInput 是工具参数。用 map 而非原始 JSON 字符串，
	// 于是适配器可以按需重新编码，不必先解析再序列化。
	ToolInput map[string]any

	// 图片相关。URL 与 Base64 二选一。
	ImageURL       string
	ImageBase64    string
	ImageMediaType string
}

// Tool 是一个工具定义。
type Tool struct {
	Name        string
	Description string
	// InputSchema 是 JSON Schema。用 map 而非字符串的理由同上。
	InputSchema map[string]any
}

// Usage 是归一化后的用量。
//
// 各家口径不同，这里逐个说清，因为**算错 token 就是算错钱**：
//
//   - PromptTokens 是「本次请求计入输入计费的全部 token」。
//     Anthropic 把缓存读取与缓存写入**分开报**，而 OpenAI 把缓存命中
//     算在 prompt_tokens 里。归一化时必须统一：这里取 OpenAI 口径，
//     即 PromptTokens 含缓存部分，缓存部分另行记在 CacheRead/CacheWrite。
//   - CompletionTokens 是输出。Anthropic 的 output_tokens 与 OpenAI 的
//     completion_tokens 含义一致。
//   - CacheReadTokens / CacheWriteTokens 是缓存明细。没有这概念的渠道留 0。
//   - ReasoningTokens 是推理模型的思考 token。它与 CompletionTokens 的关系
//     **不是固定的**：OpenAI 口径下 completion_tokens 含 reasoning_tokens，
//     而有的上游把两者分列。这里如实记下上游报的那个数，关系由
//     CompletionIncludesReasoning 显式声明——不靠数字推断。
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	// CompletionIncludesReasoning 声明 ReasoningTokens 是否**已经算在**
	// CompletionTokens 里。
	//
	// 两种口径在上游都真实存在，而光看数字判不出来：completion=100、reasoning=50
	// 既可能是「100 里含 50」，也可能是「100 可见输出 + 50 推理」。靠启发式猜
	// 会在另一种上游上把该收的钱减掉，所以这个关系由**知道协议约定的那一层**
	// 如实声明，不在这里推断。
	//
	//   - true：推理已计入 CompletionTokens。OpenAI 口径的官方定义就是这样
	//     （completion_tokens 含 completion_tokens_details.reasoning_tokens），
	//     Anthropic 的 output_tokens 同样含扩展思考的 token。
	//     计费前必须把推理从 completion 里减掉，否则同一批 token 会被收两次钱。
	//   - false（零值）：推理是**单独报出**的，与 CompletionTokens 互斥，两项各算各的。
	//
	// 零值取 false 而不是 true，理由有二：
	//
	//  1. 结构体里两个并列的数字字段，按字面读就是互不相干。把「已包含」当默认，
	//     零值结构体的字面含义会与实际语义相反。
	//  2. 零值是**漏填**时落到的那一边。漏填的 OpenAI 系生产者会多收——金额偏高
	//     是用户会来问的那种错；反过来，漏填的「独立报」生产者会少收，
	//     而少收没有任何人会来问，只会悄悄少赚钱。
	CompletionIncludesReasoning bool
	// UnitCounts 是非 token 计量：搜索次数、图片张数、视频秒数。
	// 键是单位名（如 "searches"、"images"），值是数量。
	UnitCounts map[string]int64
	// Reported 表示上游确实报了一个 usage 块。
	//
	// 它与「数字是不是 0」是两件事：上游可以报一个全 0 的 usage，也可以
	// 整块不报；两种情形下上面那些字段全是零值，**逐列相同**。计费来源
	// 要按这一位判，不按数字，也不读 Raw——Raw 是排查用的松散字段，
	// 谁改了里面的键名都不会有人报错（见 gateway.usageSourceOf）。
	//
	// 由解析那一层填：internal/relay 在把上游用量折进本结构时写它，
	// 而生产方只在确实解析到 usage 块时才走那条路。零值是「上游没报」，
	// 也是漏填时落到的那一边——漏填的代价只是把可对账的值记成不可对账，
	// 金额不变，方向与「宁可偏保守」一致。
	Reported bool
	// Raw 保留上游原样的 usage，便于事后发现口径变化。
	// 它是排查用的，不参与计费。
	Raw map[string]any
}

// Response 是统一格式的非流式响应。
type Response struct {
	// StatusCode 是这次调用真正拿到的上游 HTTP 状态码，2xx 也照实填。
	//
	// 适配器在 CheckStatus 通过之后填它（那时一定有一个状态码），零值表示
	// 「没填」——生产适配器不会留零值。它与 ErrUpstream.StatusCode 是同一个
	// 事实的两侧：成功走这里，失败走那里，因为失败时没有 Response 可回。
	//
	// 为什么它不在归一化的正文里：状态码是传输层的事实，不属于任何一家
	// 协议的响应体。以前它只存在于适配器内部，于是「这次到底收到了几号」
	// 在成功路径上只有 2xx 这个区间，没有确切值——响应方向的快照要把它
	// 作为独立一列记下来（见 request_snapshots.upstream_status）。
	StatusCode int
	// UpstreamModel 是上游回报的实际模型名。它与请求里的可能不同
	//（上游做了别名或版本替换），计费要按实际用的那个算。
	UpstreamModel string
	Message       Message
	FinishReason  string
	Usage         Usage
	// Raw 是上游原样响应，用于排查。
	Raw map[string]any
}

// StreamEvent 是流式响应里的一次事件。
//
// 用「事件」而不是「分片」：各协议的流式结构差异不小，
// 但归一化之后要传达的信息就是这几类。适配器负责把上游的分片序列
// 翻译成这几种事件，路由层与计费层只认这里。
type StreamEvent struct {
	// Kind 取值：start、delta、tool_use、usage_input、usage、done、error。
	//
	// "start" 是每条流的第一条事件，只带 StatusCode。它不是正文：
	// 网关消费它（记下上游那次响应的状态码），**不转发给调用方**。
	// 它存在的理由见 StatusCode。
	Kind string

	// StatusCode 在 Kind == "start" 时有效：这条流对应的那次 HTTP 响应
	// 的状态码（适配器已经过了 CheckStatus，所以它是 2xx）。
	//
	// 为什么让它在事件流里走一遍：上游接受了请求、却一个字都没吐就结束
	// （连接直接关掉、或流里只给了一个错误帧）时，网关要把那个状态码写进
	// 账单——「拿到过响应」是这次调用真实发生过的事实（见
	// ErrUpstream.GotResponse 与 pipeline.ErrorUpstreamStreamAborted）。
	// 那条收尾路径手里只有事件流，状态码不出现在事件里，网关就无从知道它。
	//
	// 不把它塞进 error 事件：正常结束的流里没有 error 事件，而「接受了请求
	// 却一个字没吐」恰恰可能一条事件都没有。
	StatusCode int

	// Text 是正文增量（Kind == "delta" 时有效）。
	Text string

	// 工具调用增量。
	ToolCall *ToolCallDelta

	// Usage 在 Kind == "usage" 与 "usage_input" 时有效。
	// 为 nil 表示这一片没带用量，计费层需要自己累计或估算。
	//
	// 两个 Kind 给的是同一件事的两个时刻，**用途不同**：
	//
	//   - "usage" 是这次调用的完整用量，**结算以它为准**。
	//   - "usage_input" 是输入侧已经确定的那一部分（输出还没产生）。它的用处
	//     是让调用方在流的最前面就能报出输入用量：Anthropic 的 message_start
	//     里填的就是它，而等到流末尾那个完整用量才报，第一帧就只能填本地估算，
	//     估算偏低——只读第一帧的客户端读到的是那个偏低的数，还不报错。
	//     **它不是结算依据**：只有输入、没有输出，当成完整的会把输出算成 0。
	Usage *Usage

	// FinishReason 在末尾事件里给出。
	FinishReason string

	// Err 在 Kind == "error" 时非空。上游在流中途报错是常见情形：
	// 已经吐出的部分要计费，剩余部分不能假装正常结束。
	Err error
}

// ToolCallDelta 是流式响应里工具调用的一段增量。
//
// 上游会把工具调用的 JSON 参数**分片吐出**，所以这里给的是字符串片段，
// 由调用方拼接。试图在这里就解析成 map 会在中途失败——那时 JSON 还不完整。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// Adapter 是一个上游协议的实现。
//
// 实现必须是并发安全的：同一个适配器实例会被多个请求同时使用。
// 实现也不得在内部保存请求状态——那会让并发请求互相污染。
type Adapter interface {
	// Type 返回适配器的注册名，与 channels.type 对应。
	Type() string

	// Protocol 返回它说哪套协议。
	Protocol() Protocol

	// Chat 发起一次非流式请求。
	Chat(ctx context.Context, ch Channel, req *Request) (*Response, error)

	// ChatStream 发起一次流式请求，把事件写进返回的 channel。
	//
	// 实现必须在返回前确认请求已被上游接受，这样「上游连不上」与
	// 「流中途断了」能被区分开——前者可以换渠道重试，后者不能。
	//
	// 返回的 channel 由实现负责关闭。ctx 取消时实现应尽快停止并关闭 channel。
	ChatStream(ctx context.Context, ch Channel, req *Request) (<-chan StreamEvent, error)

	// ListModels 返回上游账号可用的模型名。
	// 不是所有上游都提供，不支持时返回 ErrUnsupported。
	ListModels(ctx context.Context, ch Channel) ([]string, error)

	// Quota 查询额度。查不到时返回 ErrUnsupported，而不是一个「余量未知」
	// 的状态——「这家查不到」是正常情况，不是故障。
	//
	// 返回的是 quota.State 而不是一个本包自己的快照类型：额度状态是路由与
	// 监控共用的一个东西，本包再声明一个只会变成它的**有损投影**——投影里
	// 没有 last_probe_at / last_error_code 这两列，而正是这两列让
	// quota.State.Routable 能区分「从未观测过」（可用）与「观测过却没拿到
	// 数值」（排除）。类型统一之后，适配器拿到的状态可以不经转换地交给
	// route.Candidate.Quota。
	Quota(ctx context.Context, ch Channel) (quota.State, error)
}

// ErrUnsupported 表示该上游不提供这项能力。
//
// 与「调用失败」分开：前者是设计如此，不该告警；后者是真的出问题了。
//
// # 它在请求热路径上意味着「换下一个候选」，但不代表可重试
//
// 这条错误以前只从 ListModels / Quota 出来（都不在请求路径上）。现在
// Chat / ChatStream / Decide 也会返回它——渠道类型与这条入口对不上。
// 路由层据此**跳过这条候选**（见 internal/route 的 worthNextCandidate）：
// 候选池是按模型名筛的、不看渠道类型，一个配错类型的渠道不该打断该模型的
// 全部流量。
//
// 但 IsRetryable 对它仍然是 false，两者不要混：IsRetryable 回答的是「这次
// 失败该算在上游头上吗」，它决定渠道健康度（见 internal/gateway 的
// channelAtFault）——而一个渠道类型不对不是它坏了，把它记成故障会让健康度
// 统计里多出一批无辜的失败。
var ErrUnsupported = errors.New("该上游不支持这项能力")

// ErrUpstream 包装上游返回的错误。
//
// 保留状态码与响应体片段，因为「上游说 429，重试要等多久」与
// 「上游说 401，钥匙坏了」要做的处置完全不同。
type ErrUpstream struct {
	// StatusCode 是上游那次 HTTP 响应的状态码；没拿到响应时是 0。
	//
	// 拿到过响应就照实填，**包括 2xx**：上游回 200 却把错误写在响应体里时，
	// 200 是这次调用真实发生过的事实，丢掉它会让下游读成「没有记录到状态码」。
	StatusCode int
	// GotResponse 表示这次失败**拿到过上游的 HTTP 响应**吗。
	//
	// 它把「连不上 / TLS 失败 / 超时」（false）与「拿到了响应、但体里是错误
	// 或者体根本解不开」（true）分开。这个事实以前只能从 Body 是否为空反推，
	// 而 Body 有没有内容取决于上游写没写、适配器截没截——那是另一个包的隐式
	// 约定，谁哪天给网络错误也塞上 Body，下游的分类就静默翻面。
	//
	// 不变量：GotResponse 为 true 时 StatusCode 必须大于 0（拿到了响应就
	// 一定有一个状态码）。构造点都必须同时设这两个字段。
	GotResponse bool
	// Body 是上游响应体的**截断副本**。上游的错误信息里可能回显请求内容，
	// 所以写入日志前要按需再截一次。
	Body string
	// Retryable 由适配器判断：网络层错误、429、5xx 可重试；
	// 400、401、403 重试多少次都一样。
	Retryable bool
	// Err 是底层错误，可能为 nil。
	Err error

	// # 这里为什么没有 Retry-After
	//
	// 它曾经有一个 RetryAfter 字段，由 httpx.CheckStatus 从响应头解析，
	// 而全仓库没有任何读取点。退避在这条路径上没有落点：internal/route
	// 遇到可重试的失败是「换下一个候选」，不是「等一等同一个上游」。
	// 它真正的落点是对外响应上的 Retry-After，而那要先把「上游限流该回几号」
	// 定下来——那是另一批要一起定的对外契约。定下来之前留一个没人读的字段，
	// 只会让下一个人以为退避已经接上了。
}

func (e *ErrUpstream) Error() string {
	if e.StatusCode >= 200 && e.StatusCode < 300 {
		// 2xx 只可能是「拿到了响应但用不了」：非 2xx 会走下面那条。
		return fmt.Sprintf("上游返回 %d，但响应不可用", e.StatusCode)
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("上游返回 %d", e.StatusCode)
	}
	if e.Err != nil {
		return fmt.Sprintf("上游调用失败: %v", e.Err)
	}
	return "上游调用失败"
}

func (e *ErrUpstream) Unwrap() error { return e.Err }

// IsRetryable 判断一个错误是否值得换渠道重试。
//
// 只有适配器给出的 ErrUpstream 带明确判断时才算数；其余错误一律**不可重试**。
// 这个默认方向是有意的：把不可重试的错当成可重试，会在每个候选渠道上
// 重复一次注定失败的调用，把费用与延迟乘以渠道数；反过来只是少一次本可
// 成功的尝试，用户重试一次即可。
func IsRetryable(err error) bool {
	var up *ErrUpstream
	if errors.As(err, &up) {
		return up.Retryable
	}
	return false
}

// Registry 是适配器的注册表。
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry 建立空注册表。
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

// Register 注册一个适配器。
//
// 重复注册同一个 Type 返回错误而不是覆盖：覆盖会让「两个适配器抢一个名字」
// 表现为「其中一个静默失效」，而失效的那个通常是新加的、本该生效的那个。
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return errors.New("适配器为 nil")
	}
	t := a.Type()
	if t == "" {
		return errors.New("适配器未声明 Type")
	}
	if !a.Protocol().valid() {
		return fmt.Errorf("适配器 %q 声明的协议 %q 未知", t, a.Protocol())
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.adapters[t]; dup {
		return fmt.Errorf("适配器类型 %q 已注册", t)
	}
	r.adapters[t] = a
	return nil
}

// Lookup 按类型取适配器。未知类型返回 ErrUnknownType。
//
// 返回错误而不是 nil：调用方若拿到 nil 接口，第一次调用时才崩，
// 那时现场是「某个请求 panic」而不是「这个渠道类型没接」。
func (r *Registry) Lookup(channelType string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[channelType]
	if !ok {
		return nil, fmt.Errorf("%w: %q（已知类型：%v）", ErrUnknownType, channelType, r.types())
	}
	return a, nil
}

// Types 返回已注册的全部类型，已排序。
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.types()
}

// types 在已持锁的情况下返回排序后的类型列表。
func (r *Registry) types() []string {
	out := make([]string, 0, len(r.adapters))
	for t := range r.adapters {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownType 表示库里记着的渠道类型没有对应的适配器。
//
// 单独一个错误类型是有用的：它说明「数据库里有一条配置，但当前版本的二进制
// 不认识它」——回滚版本之后最容易出现这种情况，而它必须被明确报出来，
// 不能当成普通的上游故障。
var ErrUnknownType = errors.New("未知的渠道类型")
