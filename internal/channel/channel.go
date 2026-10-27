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
	"slices"
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

// valid 报告它是不是本包认的协议取值。
//
// 判据是「表里有没有这一行」（见 endpoint.go 的 protocolEndpoints），不另立
// 一份清单：两份清单对不上时，认得的协议算不出端点、或算得出端点的协议不被认，
// 两种都是静默的——前者让那条渠道绕过端点评据，后者让注册在启动时报一个
// 看不懂的错。
func (p Protocol) valid() bool {
	_, ok := protocolEndpoints[p]
	return ok
}

// Channel 是某一次调用要用的渠道信息，由路由层从库里取出后传入适配器。
//
// 做成值类型而不是让适配器自己去查库：适配器要做的事与存储无关，
// 而它每多一个依赖，测试就多一层要搭的台子。
type Channel struct {
	ID   int64
	Name string
	// Type 是这条渠道的**主协议**，也是它压在 channels.type 里的那个值。
	//
	// 多协议渠道的主协议只是集合里的一个成员（见 Protocols）；单协议渠道的
	// 主协议就是它唯一的那一个。执行器按这次请求的入口协议分派时，找不到
	// 匹配就退回这里（见 DispatchProtocol），所以它必须有值。
	Type string
	// Protocols 是这条渠道声明支持的协议集合。
	//
	// 它来自 channel_protocols 那张表。为空表示这条渠道**没有**这份声明
	// （迁移之前建的库、或者测试里直接拼出来的值）：那时按「只有 Type
	// 一种」处理，与加这张表之前的行为逐字相同（见 Declares）。
	Protocols []Protocol
	BaseURL   string
	// ProtocolBaseURLs 是每个协议自己的上游地址（见迁移 00069 的
	// channel_protocols.base_url）。
	//
	// 没有条目的协议沿用 BaseURL；整张表为空时与加这一列之前逐字相同。
	// 只有一处的程序读它：BaseURLFor（见那里的注释）。适配器仍然只读
	// BaseURL——调用方在把渠道交给适配器之前用 ForProtocol 定一次地址。
	ProtocolBaseURLs map[Protocol]string
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
	// 因此管理面不做键名校验：拼错一个键不会被拒绝，只在写入之后与列举时
	// 被指名点出来（见下面「今天哪些键有读者」与 SettingUnreadNote）。
	//
	// # 今天哪些键有读者
	//
	// 由渠道的 type 决定：openai 与 openai-responses 两个适配器读
	// reasoning_effort_map 与 usage 两个键（翻译在 ReasoningEffortDialect，
	// 见本包的 reasoning_effort.go；用量口径在 UsageCompletionIncludesReasoning，
	// 见本包的 usage_setting.go）；anthropic 只读 usage。其余适配器与选路层
	// 仍不读这里的任何键。这张「类型 → 键」的对应关系在 settingKeysByType 里
	// （见 setting.go），管理面据此在写入之后与列举时把没有读者的键逐个点出来
	// （见 SettingUnreadNote）。
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

// Declares 报告这条渠道有没有声明某套协议。
//
// Protocols 为空时只看 Type：那是「没有 channel_protocols 这份声明」的老行
// （或者测试里直接拼出来的值），它的含义与加这张表之前一样——这条渠道只说
// 一种协议。
func (c Channel) Declares(p Protocol) bool {
	if p == "" {
		return false
	}
	if len(c.Protocols) == 0 {
		return p == Protocol(c.Type)
	}
	return slices.Contains(c.Protocols, p)
}

// ProtocolBaseURL 返回一个协议实际要打的上游地址。
//
// 先看这个协议自己的地址，没有（或者给的是空串）就退回渠道的地址。
//
// # 这是取地址的唯一一处判据
//
// 执行器、额度探测、模型同步与模型探测都经它定地址，适配器只读
// Channel.BaseURL。各读各的症状是「探测走新地址、真实请求走老地址」——
// 两条路各自看起来都对，只有配了分址的上游才会现形，而且是从上游那边
// 回来的错。
func ProtocolBaseURL(p Protocol, overrides map[Protocol]string, fallback string) string {
	if u, ok := overrides[p]; ok && u != "" {
		return u
	}
	return fallback
}

// BaseURLFor 是这个渠道上某个协议的实际地址（见 ProtocolBaseURL）。
func (c Channel) BaseURLFor(p Protocol) string {
	return ProtocolBaseURL(p, c.ProtocolBaseURLs, c.BaseURL)
}

// ForProtocol 返回一份 BaseURL 已定成该协议实际地址的副本。
//
// 交给适配器之前用它定一次地址：适配器仍然只读 Channel.BaseURL，于是
// 「哪个协议打哪条地址」只在调用的这一侧判一次（见 ProtocolBaseURL）。
func (c Channel) ForProtocol(p Protocol) Channel {
	c.BaseURL = c.BaseURLFor(p)
	return c
}

// DispatchProtocol 返回这次上游调用该用哪套协议。
//
// # 判据是入口协议，不是主协议
//
// 一条渠道同时说 OpenAI 与 Anthropic 时，两套端点的请求体、流式分片与用量
// 字段都不同：把 Anthropic 的请求交给 OpenAI 适配器，上游收到的是它不认的
// 形状。所以先看入站协议在不在声明里。
//
// # 不在声明里就退回主协议
//
// 这是加这张表**之前**的行为，也是单协议渠道必须逐字保留的那一条：一条只说
// anthropic 的渠道收到 OpenAI 入口的请求，今天照样由 anthropic 适配器转换后
// 发出（跨协议转换是既有的能力，见 internal/relay）。声明为空时同理。
//
// inbound 为空表示调用方没有传入口协议（例如额度查询）：同样退回主协议。
func (c Channel) DispatchProtocol(inbound Protocol) Protocol {
	if c.Declares(inbound) {
		return inbound
	}
	return Protocol(c.Type)
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

// EndsWithToolResult 判断这次提交的最后一句是不是工具结果的回填。
//
// 口径：**最后一条消息的最后一个内容块**是 tool_result。块后面还跟着正文
// 的不算——那已经是调用方对结果的补充说明，不是纯粹的结果回填；多条
// tool_result 合并在一条消息里是常态（并行工具调用），按最后一个块判
// 正好把它们盖住。
//
// 三条 chat 形状的入口在 relay.ParseRequest 里都归一化成这个块类型
// （OpenAI 的 role=tool 与 Responses 的 function_call_output 都改写为
// tool_result），所以这里只看归一化形状，不认协议。消息为空（内部调用）
// 或最后一块不是 tool_result 时为否。
func (r *Request) EndsWithToolResult() bool {
	if len(r.Messages) == 0 {
		return false
	}
	last := r.Messages[len(r.Messages)-1]
	if len(last.Content) == 0 {
		return false
	}
	return last.Content[len(last.Content)-1].Type == "tool_result"
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
	// Protocol 是调用方走的那条入口（由请求路径决定），不是这条请求要发往的
	// 上游协议——上游协议由渠道声明与这个入口协议共同决定，见
	// Channel.DispatchProtocol。
	//
	// 它只出现在一件事上：执行器按它把同一渠道下多套协议分派到对应适配器
	// （见 internal/upstream）。适配器自己不读它（它本来就知道自己说的是
	// 哪套协议），也不进上游报文。
	//
	// 空串 = 不知道入口是哪条（内部调用，例如额度查询），执行器退回渠道的
	// 主协议。
	Protocol Protocol

	// Model 这个字段在链路的每一段装的是**那一侧的名字**，三种叫法依次是：
	//
	//  1. 刚解析完请求体：调用方写的名字（可能是入站别名）；
	//  2. API 入口做过入站解析之后：规范名。选路、计价、记账都用它；
	//  3. 路由层按选中的渠道翻完出站映射之后：**上游模型名**。
	//
	// 所以「它是不是上游名」取决于手上这一份处在哪一段。适配器拿到的总是
	// 第 3 种（见 internal/route 的 upstreamRequest）；成本一侧绑定的也是
	// 上游名（见 internal/upstreamcost），别把它也折算成规范名。
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

	// NativeBody 是调用方发来的**原始请求体字节**（解析之前的那一份），
	// 只在同协议直通时被适配器读取：上游那套协议与调用方一致时，适配器
	// 以它为报文来源，而不是从上面的统一字段重新编码一份。
	//
	// nil 表示没有。内部调用（systemone.Parse 那条路）与跨协议请求都是这一种，
	// 那时适配器一律走统一字段编码的路径。幂等重放**不是** nil：重放经
	// relay.ParseRequest 重新解析同一份原始请求体，这个字段照常带上，
	// 与首次请求走同一条路。
	//
	// 它与上游报文的关系要读准：枢纽编码那条路完全不读它，所以跨协议请求的
	// 报文不会因为它的存在而改变；而同协议直通时它**本身就是**报文的来源——
	// 适配器解析它、改动原文里必须由网关决定的字段（例如上游模型名）、
	// 再序列化发出，不是在编好的报文上另加东西。反过来做（先按统一字段编码
	// 一份，再从原文回填几个字段）等于把原文里 dto 不认识的字段又丢一次，
	// 正好废掉直通。
	//
	// 它也不进日志、不进快照：原样转储请求体会把调用方塞进来的任何东西
	// 写进这两处，而它们不该承载调用方的原始字节。
	NativeBody []byte
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

	// Annotations 是**引用标注**，只对 text 块有意义：模型在正文里引用了
	// 外部来源时，上游会另外给一批标注（哪一段对应哪个 URL），客户端按它
	// 渲染脚注或高亮。
	//
	// 保持原样（map 而不是本项目另立一套形状）：标注的字段集是上游协议的
	// 一部分（type、url、start_index、end_index，以及各家自己加的），而
	// 我们既不解释也不改写它们，只负责送到调用方。另起一个结构体等于凭空
	// 造出第二套口径，而两套迟早会分岔。
	//
	// 它不是正文：不参与分词，也不进计费口径。
	Annotations []map[string]any
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
	// Raw 是上游响应的原始形状，用于排查。它有两种来源，取决于这次走的是
	// 哪条路：
	//
	//   - 跨协议（Request.NativeBody == nil）：由 relay.toRawMap 把内部响应
	//     结构编回 map。dto 不认识的字段在那一层已经丢了，所以它不是上游字节，
	//     只是「我们所理解的那份响应」的再序列化。
	//   - 同协议直通：上游响应体 json.Unmarshal 出的 map，不做任何 dto 裁剪，
	//     上游给了什么这里就有什么。
	//
	// 两种都只供排查，不参与计费：键名与嵌套形状随实现变化而不报错，读它的
	// 计算会静默错下去（计费要读 Usage，见它的 Reported）。
	Raw map[string]any
}

// StreamEvent 是流式响应里的一次事件。
//
// 用「事件」而不是「分片」：各协议的流式结构差异不小，
// 但归一化之后要传达的信息就是这几类。适配器负责把上游的分片序列
// 翻译成这几种事件，路由层与计费层只认这里。
type StreamEvent struct {
	// Kind 取值：start、delta、annotation、tool_use、usage_input、usage、done、error、native。
	//
	// "start" 是每条流的第一条事件，只带 StatusCode。它不是正文：
	// 网关消费它（记下上游那次响应的状态码），**不转发给调用方**。
	// 它存在的理由见 StatusCode。
	//
	// "native" 是**叠加**事件而不是替代：直通模式下归一化事件照旧一条不少地发，
	// 它只是额外带上上游那一帧的原始载荷（见 Frame）。把它当成「有它就不发别的」
	// 会丢掉 delta 与 tool_use，而下游的中途冻结估算只在这两个 Kind 上推进、
	// 快照正文也靠它们——缺了它们，上游不报 usage 时结算会按输出 token 为 0
	// 计费，快照骨架里也没有正文。
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

	// Annotations 在 Kind == "annotation" 时非空：模型在已经吐出的正文里
	// 引用了外部来源，这一条事件给出一批标注。
	//
	// 它是一条**独立**的事件，而不是挂在 delta 上：上游把标注单独报（正文
	// 流到一半才出现引用），而挂进某一条 delta 会让「客户端的正文分片里
	// 突然多出一批标注」与「上游真的在那一刻给了」混为一谈。转发时它被
	// 渲染成调用协议自己的标注事件。
	//
	// 第一次让调用方看见标注的时刻，可能早于第一次看见对应正文的时刻
	// （上游的顺序不保证），渲染层要按自己的状态处理，而不是假定它一定落在
	// 某一帧正文之后。
	Annotations []map[string]any

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

	// NativeFormat 在 Kind == "native" 时有效：这一帧属于哪套协议。
	//
	// 渲染层按它判「调用方协议的与它一致才原样转发」——一致时把 Frame 原样
	// 写出去，不一致时这条事件被忽略，调用方拿到的是按自己协议重新渲染的
	// 归一化事件。
	NativeFormat Protocol

	// Frame 在 Kind == "native" 时有效：上游 SSE `data:` 行的**载荷字节**。
	//
	// 已经去掉 `data:` 前缀与行尾换行，首尾空白已 trim；不含 `event:` 行。
	// Anthropic 的载荷里带 `type` 字段（message_start、content_block_delta 等），
	// 事件名取它；OpenAI chat 既没有 `event:` 行、载荷里也没有 `type` 字段
	// （见 internal/channel/openaiadapter/dto.go 的 openAIStreamChunk），
	// 帧就是纯 data 载荷。
	//
	// 不带 `data:` 前缀是有意的：渲染层用 internal/api/stream.go 的 frame
	// 结构重写帧，由它自己写 `event:` 行与 `data: ` 前缀。带前缀进来会写出
	// `data: data: {...}`。裸换行同样不能有——SSE 按行切帧，一帧会被拆成两帧。
	Frame []byte
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

	// Endpoint 返回这套适配器把请求发到哪条路径。
	//
	// 它与 Protocol 一一对应（见 Protocol.Endpoint），注册表拿它做自检：两者
	// 对不上说明适配器抄错了——声明 openai 却往 responses 那条路径发。
	//
	// 为什么要它而不是从别处推：选路要判「这个模型的目录声明里有没有这条
	// 端点」，而那条端点必须与**适配器真正发出去的路径**是同一个东西。
	// 让选路去猜（按协议族再写一份映射）就是第二份判据，它一旦与适配器漂移，
	// 表现是「选中的渠道配好了却打不通」，而那与上游真的不支持这件事长得一样。
	//
	// 实现返回的必须是本包的那几个常量之一，而且要**用在发请求那一处**：
	// 路径写字面量、这里另说一个常量的话，两者可以各自改而自检看不出来。
	Endpoint() Endpoint

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
	//
	// # 出口
	//
	// 生产里的调用点是运营命令 `larkaxctl channel sync-models`（经
	// channels.ModelSync）：从库里取密文、解出凭证、按渠道类型调到这里，
	// 再把清单落进 channel_upstream_models 供人查看。它是一次**显式**动作——
	// 启动时不拉、请求路径上不调用、没有后台轮询，所以落库的是「上次同步
	// 那一刻」的事实。
	//
	// 它不在渠道的写入路径上：`channel create` / `update` 与 doctor 第 3 项
	// 的核查仍是**静态**判据（模型目录里声明的端点 × 渠道 type 打的那条端点，
	// 见 internal/channels 的 endpoints.go），只读库里的表、不碰网络。配置
	// 命令不该依赖「上游这一刻可达」。
	//
	// 客户端面也不在这里：公共的 GET /v1/models 由模型目录与「有渠道在供」
	// 那一份清单拼出来（见 internal/api/models.go），不问任何上游——它回答
	// 的是「这把 key 能用什么」，而那是网关自己知道的事。
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

// IsEmbeddedErrorRetryable 判断**2xx 响应体里内嵌的那个错误对象**该不该换渠道重试。
//
// 上游把错误写在 200 的响应体里时，状态码那一侧的判据（httpx.RetryableStatus）
// 用不上：200 不是失败码，而这次调用其实失败了。所以这里另判一次，判据按
// HTTP 语义写——**上游这一侧暂时答不上来**的（限流、过载、超时、它自己内部
// 出错）值得换一个渠道再试；**这次请求本身不对**的（参数、鉴权、权限、找不到、
// 请求体过大）换到哪都一样，只会把费用与延迟乘以候选渠道数。
//
// 「可重试」在这套代码里有两个下游：路由层据此换下一个候选，渠道健康度据此把
// 这次失败算在这个渠道头上（见 IsRetryable）。上游限流时它确实该被算上——那正是
// 「这个渠道此刻不行」的意思。
//
// # 两个字段都要看
//
// type 与 code 是各家对同一件事的两种写法，而且不保证填的是哪一个：Anthropic
// 把分类写在 type 里（"rate_limit_error"），OpenAI 兼容上游常常写在 code 里、
// type 那边写的是别的（429 的响应体里形如 `"type": "requests"` 配
// `"code": "rate_limit_exceeded"`）。只看其中一个，同一个 429 语义就会因为走
// 哪条入口而分岔。
//
// # 匹配是子串加归一，不是枚举
//
// 这两个字段里写的不是枚举值，而是 "RateLimitError"、"rate_limit_exceeded"
// 这类带前后缀、大小写不一的写法，也有上游直接把状态码填进去（`"code": 429`）。
// 所以先把分隔符与大小写归一（见 normalizeErrorField），再做子串匹配：把每种
// 写法列成枚举，等于赌上游不改写法。**数字形态的那几个状态码例外**——它们要求
// 整个字段就是那个数，否则 `15003` 里的 "500" 也会被当成 5xx。
//
// 认不出来的一律返回 false，方向与 IsRetryable 一致：把不可重试的错当成
// 可重试，会在每个候选渠道上重复一次注定失败的调用；反过来只是少一次本可
// 成功的尝试，用户重试一次即可。
func IsEmbeddedErrorRetryable(typeName, code string) bool {
	for _, field := range []string{normalizeErrorField(typeName), normalizeErrorField(code)} {
		if field == "" {
			continue
		}
		if slices.Contains(retryableEmbeddedStatusCodes, field) {
			return true
		}
		for _, marker := range retryableEmbeddedMarkers {
			if strings.Contains(field, marker) {
				return true
			}
		}
	}
	return false
}

// retryableEmbeddedMarkers 是「上游这一侧暂时答不上来」在 type / code 里的各种
// 写法，**都按 normalizeErrorField 归一过**（小写、去分隔符）。
//
// 三类：限流、过载、超时，加上上游自己内部出错——最后那一种多半也是暂时的，
// 与下面那组状态码说的是同一件事。
var retryableEmbeddedMarkers = []string{
	// 限流
	"ratelimit", "toomanyrequests", "throttl",
	// 过载
	"overload", "capacity",
	// 超时
	"timeout", "timedout", "deadline",
	// 上游自身出错
	"servererror", "internalerror", "apierror", "unavailable", "temporarily",
}

// retryableEmbeddedStatusCodes 是上游把状态码直接填进 code 里的那几种写法，
// 按**整个字段**比对（理由见 IsEmbeddedErrorRetryable）：408 是上游（或它前面
// 的网关）替自己的超时回的那个码，429 是限流，529 是上游报过载用的那个非标准码，
// 其余四个是 5xx。
//
// 408 这一条与 httpx.RetryableStatus 认的那几个 4xx 是同一件事的两处判据，两处
// 都要认：只在一处认，上游把错误写在 200 响应体里、把 408 填进 code 时就不会换
// 渠道重试（issue #613）。425 只在那一边认——这一组收的是「上游真的会把状态码
// 填进 code」的那几个，而 425 是 TLS 早数据的码，不出现在错误体里。
var retryableEmbeddedStatusCodes = []string{"408", "429", "500", "502", "503", "504", "529"}

// normalizeErrorField 把上游写在 type / code 里的那个词归一成可做子串匹配的
// 形状：转小写，去掉下划线、连字符与空格——这三样在上游的写法里是同一个
// 分隔符，rate_limit_error / rate-limit-error / Rate Limit Error 说的是同一件
// 事。
func normalizeErrorField(s string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(s))
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
	// 类型名与协议名的自检：两者必须是**同一个字符串**。
	//
	// 渠道表里的 type 存的就是适配器的 Type()，而 internal/channels 没有注册表
	// ——它只读库，把那一列**当枚举**查 Protocol.Endpoint 算这条渠道打哪条端点；
	// 执行那一侧却按适配器自己声明的 Protocol() 走。两者不是同一个协议时，选路
	// 算出的端点是另一条路径上的字符串，而请求实际发去了这一条——那次比对判的
	// 就不是这个请求的落点。类型名自造一个（算出来是空串）更静默：空串在端点
	// 判据里的意思是「不知道」= **放行**。两种都不报错、不打日志，现形方式只剩
	// 「上游回 400 unsupported_model」，而从我们这边看与「模型不存在」无从分辨。
	//
	// 它同时是 Protocol 注释里那条约定的执行点：「取值与渠道表里的 channels.type
	// 是同一个字符串（适配器的 Type()）」。
	//
	// 这一条同时盖住了「类型名不是一个协议名」：上面刚判过声明的协议合法，所以
	// 两者同一个字符串就意味着那个类型名一定落在协议取值上。
	if t != string(a.Protocol()) {
		return fmt.Errorf("适配器类型 %q 与它声明的协议 %q 不是同一个名字："+
			"渠道表里的 type 存的就是 Type()，选路按它算出这条渠道打哪条端点，"+
			"执行那一侧却按声明的协议走——两者不一致时，同一次请求在两侧被当成两条端点，"+
			"且不报错。本包认的协议名只有 %s，类型名要与协议同名",
			t, a.Protocol(), strings.Join(protocolNames, "、"))
	}
	// 端点的自检：协议决定适配器打哪条路径（Protocol.Endpoint），而适配器
	// 自述的端点必须与它一致。抄错的表现是静默的——选路按声明的协议算出
	// 端点去比对目录，而请求实际发去了另一条路径，于是那次比对判了个别的
	// 东西；上游那边则是 404 或 400，看不出是我们配错了。
	if want := a.Protocol().Endpoint(); a.Endpoint() != want {
		return fmt.Errorf("适配器 %q 声明的协议是 %q（那条适配器应当打 %q），但它说自己的端点是 %q",
			t, a.Protocol(), want, a.Endpoint())
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
