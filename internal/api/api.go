// Package api 是 larkax 的公共 HTTP 入口。
//
// 它只做一件事：把一次 HTTP 请求变成一次上游调用，再把结果变回 HTTP 响应。
// 计费、选路、协议转换、账本都在别的包里，这里只决定它们的调用顺序，
// 以及**对外契约的形状**——路径、状态码、错误体与非流式响应体的字段。
//
// # 协议由路径决定
//
// /v1/chat/completions 说 OpenAI 协议，/v1/messages 说 Anthropic 协议，
// /v1/systemone 说 System One 决策协议（见 systemone.go）。
// 不从请求体猜：前两套协议的请求体在浅层很像（都有 model 与 messages），
// 猜错时解析往往照样成功，于是错误被推迟到上游才暴露。路径是调用方明确
// 给出的信息，用它就不需要猜。
//
// 错误体也按调用协议给形状（见 docs/versioning.md）：用 OpenAI 格式调用
// 就得到 OpenAI 形状的错误，Anthropic 同理。这一条不能混——调用方的 SDK
// 按自己那套解错误体，形状不对时它连错误类型都读不出来。
//
// # 未实现的路径仍然回 501
//
// 这几条路径之外的任何请求都得到 501，与推理接口尚未实现时的行为一致。
// 这是有意的：部署验证靠「任何请求都立刻得到明确答复」来确认端口、TLS
// 与负载均衡转发是通的，而这个性质不该因为多了一条路由就消失。
//
// # 两个接口是刻意的窄缝
//
// 鉴权与调上游都不是本包的职责，各自用一个窄接口表示（Authenticator 与
// Chatter）。这样本包要测的是「HTTP 请求怎么变成一次调用、结果怎么变回
// 响应」，而不必搭起真实的渠道、路由与账本——那些与本包的行为无关。
package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/keyquota"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	"github.com/sumwai/larkax/internal/relay"
	"github.com/sumwai/larkax/internal/route"
)

// 公共入口的两条推理路径。它们与调用协议一一对应，见包注释。
//
// 第三条（PathSystemOne）在 systemone.go 里：它带的常量只有那条路径用得上，
// 两处分开是为了让「这条表面有什么、没有什么」在一处读完。
const (
	PathChatCompletions = "/v1/chat/completions"
	PathMessages        = "/v1/messages"
)

// HeaderVersion 是响应头里回带发布版本的字段名。
//
// 它与 docs/versioning.md 里承诺的那个头是同一个。放在本包是因为这里是
// 唯一会给公共端口写响应的地方——写成字面量的话，两处迟早会不一致。
// 用**规范形式**（首字母与每个连字符段的首字母大写），不是全小写：
// http.Header 是个 map，Set 会做规范化，而直接按下标读**不会**。
// 两者不一致时，`rec.Header()[HeaderVersion]` 永远取不到刚写进去的那个头——
// 断言会静默地永远成立。写成规范形式，两边就对上了。
const HeaderVersion = "X-Larkax-Version"

// maxRequestBodyBytes 是请求体的上限，超出回 413。
//
// 依据是「合法请求里最大的那部分是什么」：多模态请求把图片按 base64 放在
// 请求体里，编码后比原图大约大三分之一，所以几 MB 的请求很常见；纯文本
// 对话则远到不了这个量级。8 MiB 能装下几张图加一段长上下文，同时把
// 「把整个文件贴进 prompt」这类误用挡在解析之前。
//
// 上限同时也是**内存**上限：请求体在解析前整段读进内存，峰值约为
// 「并发请求数 × 上限」，所以它不能定得宽松到「反正内存够」。
//
// 上游自己的上限（有的到 32 MiB）不构成理由：那是它们的实现细节，
// 而我们的限制必须在解析之前生效。
const maxRequestBodyBytes = 8 << 20

// User 是一次请求的调用者。
//
// 只放计费与限流必须知道的东西。鉴权实现将来可能解析出更多信息（套餐、
// 分组），那些属于鉴权自己的类型，不该顺着这个结构漏进本包。
//
// 限流的两个输入是例外，而且是有意的：额度存在**那把 key** 上，本包要
// 把它交给限流实现，就必须先拿到它。本包仍然**不解释**这两个值（见
// RateLimiter）——拿到与解释是两件事，把解释放在这里等于开了第二个
// 定义语义的地方。
type User struct {
	ID int64

	// KeyID 是这次请求用的那把 API Key（api_keys.id）。限流按 key 计，
	// 所以它是限流的输入之一。它与 ID 不是一回事：一个用户可以有多个 key，
	// 而额度是逐把设的。
	KeyID int64

	// RateLimitRPM 是这把 key 的每分钟额度，即 api_keys.rate_limit_rpm 的
	// 原样值：nil 表示列值为 NULL。NULL 与 0 各是什么意思由限流实现定，
	// 本包只负责原样传下去。
	RateLimitRPM *int

	// QuotaLimit 是这把 key 的累计花费上限，即 api_keys.quota_limit 的
	// 原样值：nil 表示列值为 NULL。同样由实现解释（见 QuotaChecker）。
	QuotaLimit *money.Amount
}

// Authenticator 从请求里取出凭证并解析出用户。
//
// 这是本包对鉴权的全部要求：入口只管「有没有一个用户」，不管凭证是
// API Key、会话还是别的什么。
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (User, error)
}

// Chatter 是调上游的能力。
//
// 用窄接口而不是直接依赖 *gateway.Gateway：本包只关心「给它两个请求，
// 它回一个结果」。*gateway.Gateway 天然满足它。
//
// 流式与非流式放在**同一个**接口上，而不是两个字段各要一个实现：两者是
// 同一种能力（调上游）的两种形状，而拆开会让「装了一个没装另一个」变成
// 一种能装出来的状态——它的现场是一半请求正常、一半请求回 501。
type Chatter interface {
	// Chat 处理一次非流式请求。hooks 是这次请求的插件挂载点，
	// 零值表示不跑插件（见 gateway.Hooks）。
	Chat(ctx context.Context, req pipeline.Request, upstream *channel.Request, hooks gateway.Hooks) (*gateway.Result, error)

	// ChatStream 处理一次流式请求。返回的 Result.Events 由调用方读到关闭
	// 为止——**必须读到底**：网关的转发 goroutine 在关闭它之前完成结算，
	// 不读到底那条 goroutine 会阻塞在发送上，那笔预留就一直冻着。
	//
	// 返回错误只发生在**上游接受请求之前**（没有可用渠道、连不上、非 2xx），
	// 那时还没有任何内容发给客户端，调用方可以照常回一个错误响应。
	//
	// hooks 与 Chat 同义，而且**两个入口都要给**：选渠道这件事流式与非流式
	// 是同一件（见 gateway.ChatStream）。只给一边，一个「把这个模型的流量钉到
	// 某个渠道」的插件会对流式请求静默不生效——接了一半最难发现的样子。
	//
	// 这条路上**独有**的挂载点是 onStream（逐块决定）；onResponse 则相反，
	// 它只可能发生在非流式那条路上。onRequest / onRoute / onUsage 两条路都有，
	// 位置也相同（见 docs/plugin-contract.md 的「接线」一节）。
	ChatStream(ctx context.Context, req pipeline.Request, upstream *channel.Request, hooks gateway.Hooks) (*gateway.Result, error)
}

// IdempotencyStore 是幂等记录的读写口。
//
// 与 Authenticator / RateLimiter 同一个做法：本包只声明自己要什么，
// 具体实现（internal/idempotency 的 Store）不必 import 本包。
//
// # 为什么只有这三个方法
//
// 建立记录那一步**不在这里**。它必须与预留写在同一个事务里，所以它由
// 落库层在预留那个事务内完成（见 internal/ledger/store 的 SaveReservation），
// 而不是由入口先记一笔、再让落库层记另一笔——两个事务之间的窗口会让
// 「记了幂等但没预留」或「预留了但没记幂等」这两种状态真实出现，
// 而它们都没有任何东西会报出来。
//
// 所以本包拿到的能力是：读一条记录、把走完的响应写回去、把没走完的标成失败。
type IdempotencyStore interface {
	// Lookup 按 (api_key_id, key) 读一条记录，没有时返回 idempotency.ErrNotFound。
	Lookup(ctx context.Context, apiKeyID int64, key string) (*idempotency.Record, error)

	// Complete 把一次**走完**的响应写回记录，供后续同键请求重放。
	Complete(ctx context.Context, apiKeyID int64, key string, status int, contentType string, body []byte) error

	// Fail 把一次**没走完**的请求标成失败。它不会被重放，也不会被重新执行。
	Fail(ctx context.Context, apiKeyID int64, key string) error
}

// RateLimiter 按 API Key 判断一次请求是否放行。
//
// 它是本包对限流的全部要求：入口只管「这一次要不要放行」，不管计数放在
// 进程内还是别处、也不管窗口怎么切。
//
// rpm 是 api_keys.rate_limit_rpm 的原样值（nil 表示 NULL），**它的含义由
// 实现解释**，本包不做第二次判断：把「NULL 是不限、0 是禁止」写在两处，
// 两处迟早会不一致，而不一致时没有任何东西会报错。
type RateLimiter interface {
	// Allow 记一次请求并返回是否放行。
	//
	// allowed 为 false 时，retryAfter 是建议客户端等多久再试；入口会把它
	// 取整到秒写进 Retry-After（见 writeRateLimited），所以实现给多少精度
	// 都行。
	Allow(keyID int64, rpm *int) (allowed bool, retryAfter time.Duration)
}

// QuotaChecker 判断一把 API Key 的额度还收不收得下这一笔请求。
//
// 与 RateLimiter 同一个做法：本包只声明自己要什么，具体实现
// （internal/ledger/store 的 CheckQuota）不必 import 本包。limit 是
// api_keys.quota_limit 的原样值（nil 表示列值为 NULL），**它的含义由实现
// 解释**——把「NULL 是不限、0 是一个都不放行」写在两处，两处迟早会不一致。
//
// # 它是早拒绝，不是判据
//
// 这一层不加锁、不开事务，并发的两个请求可能都过得了它——所以它**不能**是
// 唯一的判定。真正的判据在预留那个事务里（见 internal/ledger/store 的
// SaveReservation），那里与插 pending 行同一个事务、靠行锁串行化。两处共用
// internal/keyquota 里同一份口径，所以「同一份判据被调两次」不会有两份实现。
//
// 留着它的理由与限流那一处相同：超限的请求不该让我们为它读进最多
// maxRequestBodyBytes 的内存，更不该走到预留——那会先把余额冻住，再因为
// 额度把它拒掉。
type QuotaChecker interface {
	// CheckQuota 报告这把 key 还收不收得下这一笔。
	//
	// 返回 nil 表示放行；错误链里有 keyquota.ErrExceeded 表示超限；其余错误
	// 表示这一次没查出来（连不上库、超时），入口会记一条日志然后**放行**——
	// 理由见 serve 里那一段。
	CheckQuota(ctx context.Context, keyID int64, limit *money.Amount) error
}

// Handler 是公共入口的 HTTP handler。
type Handler struct {
	Auth    Authenticator
	Chatter Chatter

	// RateLimiter 按 key 限流。为 nil 时不做限流。
	//
	// 它接在鉴权之后、读请求体之前：被限流的请求不该让我们为它读进最多
	// maxRequestBodyBytes 的内存，更不该走到预留——那会先把钱冻住，
	// 再因为限流把请求拒掉。
	RateLimiter RateLimiter

	// Quota 按 key 的累计花费判断这一次要不要放行。为 nil 时不做这道早拒绝。
	//
	// 与 RateLimiter 接在同一个位置、同一个理由。区别是它**不是判据**：
	// 真正的判定在预留那个事务里（见 QuotaChecker）。
	//
	// 「为 nil 就不做」是有意的默认，与 Idempotency 同一个取舍：没接上这条
	// 链路时，行为与这道检查出现之前完全一样，而权威判定仍然在。反过来
	// （nil 时回 501）会让一次装配疏漏表现为「所有请求都失败」。
	Quota QuotaChecker

	// Version 是发布版本，写进每个响应的 x-larkax-version 头。
	// 见 docs/versioning.md：用户报问题时，「你调的是哪个版本」是最该先
	// 确认的事。留空则不写这个头。
	Version string

	// Decider 是调上游决策接口的能力（见 systemone.go）。为 nil 时
	// /v1/systemone 回 501，其余路径不受影响。
	//
	// 与 Chatter 分开而不是合并成一个接口：两者的签名不同（一个收
	// *channel.Request，一个收 *systemone.Request），合并只能靠类型断言，
	// 而那会把编译期的事推到运行期。装配层通常把同一个 *gateway.Gateway
	// 填进两个字段。
	Decider Decider

	// Idempotency 是幂等记录的读写口。为 nil 时忽略请求头 Idempotency-Key，
	// 每个请求都照常执行一次。
	//
	// 「为 nil 就不做幂等」是有意的默认：没接上这条链路时，行为与这个头
	// 出现之前完全一样。反过来（nil 时回 501）会让一次装配疏漏表现为
	// 「所有带这个头的请求都失败」，而那比不做幂等更糟。
	Idempotency IdempotencyStore

	// DefaultAsset 是结算用的资产品种，由装配层从 settings 的
	// billing.default_asset 读出来填上。
	//
	// 它在这里而不是在计费包里：那个值是**运行期配置**，而计费层是纯编排，
	// 不连数据库也不读配置。入口是知道配置的那一层，于是由它填进请求。
	//
	// 留空不会得到一个默认品种——计费层拒绝空值，每个请求都会失败。
	// 那是有意的：装配漏了一步要当场响，而不是安静地记错品种。
	DefaultAsset string

	// Plugins 是插件注册表。为 nil 时请求路径上不跑插件——插件系统关着、
	// 或者装配时没接上，都是这个状态。
	//
	// 「为 nil 就不做」与 RateLimiter / Quota / Idempotency 同一个取舍：
	// 没接上这条链路时，行为与插件系统出现之前完全一样。反过来（nil 时
	// 回 501）会让一次装配疏漏表现为「所有请求都失败」。
	Plugins *plugin.Registry

	// PluginLimits 是插件每次 hook 调用的限额，由装配层从 settings 表的
	// 四个 plugin.* 键读出来填上（见 internal/assembly）。
	//
	// 它与 Plugins 一起给，而不是让本包自己去读那四个键：那份映射只该有
	// 一处实现，两处不一致的表现是插件跑在另一套限额下——不报错，
	// 只是行为不同。
	PluginLimits sandbox.Limits

	// PluginHost 按请求构造宿主能力（日志、日历、KV）。为 nil 时插件拿不到
	// 任何宿主能力，`log(...)` 之类的调用会得到「能力未启用」。
	//
	// 三个参数都是请求期的：插件 key 要进宿主日志，请求标识与用户标识决定
	// 日志字段与 KV 命名空间。插件 key 由注册表选中之后回填，所以这里收的是
	// 一个按插件构造的函数（见 plugin.HostBuilder）。
	PluginHost func(p *plugin.Plugin, requestID string, userID int64) sandbox.Host

	// Now 可注入，便于测试与时间相关的字段。
	Now func() time.Time
}

// New 构造一个 Handler。
func New(auth Authenticator, chatter Chatter) *Handler {
	return &Handler{Auth: auth, Chatter: chatter}
}

// ServeHTTP 按路径分派到对应的协议。
//
// 它自己分派而不是交给 http.ServeMux：405 的 Allow 头、未知路径的 501、
// 以及错误体的形状都要按协议给，而 ServeMux 只能给出它自己那套
// 纯文本响应。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Version != "" {
		w.Header().Set(HeaderVersion, h.Version)
	}

	switch r.URL.Path {
	case PathChatCompletions:
		h.serve(w, r, channel.ProtocolOpenAI)
	case PathMessages:
		h.serve(w, r, channel.ProtocolAnthropic)
	case PathSystemOne:
		h.serveDecision(w, r)
	default:
		// 未知路径：这里不知道调用方说哪套协议（协议由路径决定，而路径
		// 我们并不认识），所以用 OpenAI 形状——两套里更通用的那一套。
		writeError(w, channel.ProtocolOpenAI, http.StatusNotImplemented,
			"this endpoint is not implemented")
	}
}

// serve 处理一条推理路径上的一次请求。
//
// 解析之前的检查全在 admit 里，三条路径共用同一份顺序与措辞。
//
// 幂等键是那条顺序上的**最后一步**（解析之后、调上游之前）：指纹要算在
// 解析后的请求上，而它必须在任何有副作用的事情之前生效——排在预留之后
// 就等于没做，因为那时钱已经冻上了。
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, protocol channel.Protocol) {
	user, body, ok := h.admit(w, r, protocol, h.Auth != nil && h.Chatter != nil)
	if !ok {
		return
	}

	upstream, err := relay.ParseRequest(r.Context(), protocol, body)
	if err != nil {
		// 解析失败的细节不回给调用方：那里面会带上 relaykit 的类型名与
		// 内部字段名，而调用方需要知道的只是「body 不是合法的 JSON」。
		observe.FromContext(r.Context()).Warn("解析请求失败", "err", err)
		writeError(w, protocol, http.StatusBadRequest, "invalid request body")
		return
	}
	if rejected := checkRequest(upstream); rejected != nil {
		writeError(w, protocol, rejected.status, rejected.message)
		return
	}

	requestID := requestIDOf(r)

	// 幂等键接在**解析成功之后**：指纹算的是解析后的请求（见
	// idempotency.HashRequest），而解析失败（400）与必要字段缺失都还没有
	// 「一次请求」可言，那时也没有任何东西需要幂等——把它们记下来只会让
	// 一个写错请求体的客户端在改正之后还拿到 409。
	if key := r.Header.Get(idempotency.HeaderName); key != "" && h.Idempotency != nil {
		h.serveIdempotent(w, r, protocol, upstream, body, user, requestID, key)
		return
	}
	h.serveOnce(w, r, protocol, upstream, user, requestID, nil)
}

// admit 跑一次请求在**解析请求体之前**的全部检查，返回调用者与请求体。
//
// ok 为 false 表示这一次已经处理完了——响应写出去了，调用方直接返回。
//
// # 检查顺序是刻意的
//
// 先是纯请求头层面的判断（方法、Content-Type），再是装配检查，然后是鉴权，
// 最后才读请求体。这样没通过鉴权的请求不会让我们为它读进最多
// maxRequestBodyBytes 的内存，而 Content-Type 这种一眼可判的错误也轮不到
// 花代价去解析。三条路径共用这一份，所以「哪条路径少了一道检查」不会发生。
//
// # ready 为什么由调用方给
//
// 它说的是「这条路径要的协作者都装上了吗」。三条路径要的东西不同（决策那条
// 要的是 Decider 而不是 Chatter），在这里写死一个判据会让「决策没接上」
// 表现为「chat 也没接上」——而这两句话的排查方向完全不同。
func (h *Handler) admit(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, ready bool) (User, []byte, bool) {
	if r.Method != http.MethodPost {
		// Allow 头是 405 的必要部分：没有它，调用方无从知道该用什么方法，
		// 只能去翻文档。
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, protocol, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return User{}, nil, false
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, protocol, http.StatusUnsupportedMediaType,
			"Content-Type must be application/json")
		return User{}, nil, false
	}
	if !ready {
		// 依赖没装配。这不是调用方的错，也不该在请求期 panic——
		// 一个 501 说明「这个构建没接上」，比一个栈回溯有用。
		writeError(w, protocol, http.StatusNotImplemented,
			"inference is not wired in this build")
		return User{}, nil, false
	}

	user, err := h.Auth.Authenticate(r.Context(), r)
	if err != nil {
		// 两类失败必须分开（见 internal/auth 的包注释）：凭证本身的问题回
		// 401，查询失败（连不上库、超时、脏数据）回 500。混在一起的话，
		// 一次数据库抖动会表现为「全站密钥突然失效」——SDK 按「key 无效」
		// 处理而**不重试**，监控上是 401 尖峰而不是 5xx 尖峰，值班的人去
		// 查 key 而不是查库，两个方向完全相反的排查。
		//
		// 500 而不是 503：503 承诺「稍后重试」，而这一类里也有 quota_limit
		// 解析不了这种永久性错误（见 internal/auth 的 lookup）。本包对
		// 「我们这一侧坏了」也一直用 500（见 failUpstream 的默认分支），
		// 503 在包内已经是另一个意思：这个模型没有可用上游。
		//
		// 区分的是状态码，不是响应体：凭证为什么不对不回给调用方——区分开
		// 等于送出一个「这个 Key 存在吗」的探测器，而库为什么连不上同样是
		// 我们内部的事。所以 5xx 这一支还要有一条日志，否则这类故障在生产
		// 里只有一个状态码可看。
		//
		// 401 这一支**不写日志，只记指标**，理由与上面正相反：401 是攻击者
		// 可控的，拿垃圾 key 打就能刷日志、刷盘；而「库里没有这个哈希」与
		// 「这把 key 上周过期了」这两种失败必须分得开（用户报「我的 key 不能
		// 用了」时，它们是两条完全不同的排查路径，见 internal/auth 的包注释）。
		// 按 auth.Reason 分桶计数同时满足这两条：标签值是一个闭合枚举，
		// 基数有界，刷不动。
		if errors.Is(err, auth.ErrUnauthenticated) {
			observe.Metrics.AuthFailures.With(authFailureReason(err)).Inc()
			writeError(w, protocol, http.StatusUnauthorized, "invalid credentials")
			return User{}, nil, false
		}
		observe.FromContext(r.Context()).Error("鉴权查询失败", "err", err)
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return User{}, nil, false
	}

	// 限流在鉴权之后、读请求体之前。
	//
	// 「之后」是必须的：按 key 计就要先知道是哪把 key，而没通过鉴权的请求
	// 不该消耗任何人的额度。
	// 「之前」也是必须的：超限的请求不该让我们为它读进最多
	// maxRequestBodyBytes 的内存，更不该走到预留——那会先把余额冻住，
	// 再因为限流把请求拒掉，一笔钱为一次不会被处理的请求白白占住。
	if h.RateLimiter != nil {
		if allowed, retryAfter := h.RateLimiter.Allow(user.KeyID, user.RateLimitRPM); !allowed {
			observe.FromContext(r.Context()).Warn("请求超出该 Key 的 RPM 额度", "key_id", user.KeyID)
			writeRateLimited(w, protocol, retryAfter)
			return User{}, nil, false
		}
	}

	// 额度判定接在限流之后、读请求体之前，理由与限流那一处逐字相同：
	// 超限的请求不该让我们为它读进最多 maxRequestBodyBytes 的内存，更不该
	// 走到预留——那会先把余额冻住，再因为额度把它拒掉。
	//
	// 排在限流**之后**是因为限流是进程内计数（不查库），先做便宜的那个，
	// 被限流的请求就不必为此多一次数据库往返。
	//
	// # 这一层不是判据
	//
	// 它不加锁、不开事务，并发的两个请求可能都过得了它。真正的判定在预留
	// 那个事务里（见 QuotaChecker），两处共用同一份口径。所以这里**不需要**
	// 挡得住并发——挡得住的那一处才是判据。
	if h.Quota != nil {
		switch quotaErr := h.Quota.CheckQuota(r.Context(), user.KeyID, user.QuotaLimit); {
		case quotaErr == nil:
		case errors.Is(quotaErr, keyquota.ErrExceeded):
			observe.FromContext(r.Context()).Warn("请求超出该 Key 的额度上限", "key_id", user.KeyID)
			writeInsufficientQuota(w, protocol)
			return User{}, nil, false
		default:
			// 查不出来（连不上库、超时）时**放行**，把判定交给预留那一层。
			//
			// 放行是安全的，而不是「赌一把」：权威判定读的是同一个库，这一次
			// 读不出来而那一次读得出来时，超限的请求照样会在那里被拒；两次都
			// 读不出来时，预留本身就失败了，请求不会因此被放过去。
			//
			// 反过来在这里拒掉，一次数据库抖动就会表现为「全站额度突然用尽」，
			// 而那与「有人真的把额度花完了」是两个方向完全相反的排查。
			observe.FromContext(r.Context()).Warn(
				"额度早拒绝检查失败，交给预留那一层判定", "err", quotaErr)
		}
	}

	body, err := readBody(w, r)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(w, protocol, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxRequestBodyBytes))
			return User{}, nil, false
		}
		writeError(w, protocol, http.StatusBadRequest, "could not read request body")
		return User{}, nil, false
	}

	return user, body, true
}

// authFailureReason 把一次认证失败归到 larkax_auth_failures_total 的一个
// 标签值上。
//
// 判据走 errors.As 取 *auth.Error，而不是 errors.Is：后者只说明「这是凭证
// 问题」，取不出是哪一种，而这一层要的正是那一种。
//
// # 为什么取不到原因时单列一个取值，而不是不计
//
// errors.Is 认哨兵、errors.As 认类型，两者可以不一致——一条路径只要
// `return auth.ErrUnauthenticated`（不包 *auth.Error），这里就会回一个 401
// 却归不了类。今天没有这样的路径（internal/auth 里每一处失败都构造
// *auth.Error，哨兵本身只经 Error.Unwrap 出现），但将来加一条是不报错的。
//
// 丢进 unclassified 而不是不计，是为了让那种情况**看得见**：不计的话，
// 「这条 401 归不了类」与「这条路径没接上」在指标上都是「什么都没有」。
// 空串同理——它会在采集端变成一条 reason="" 的序列，读的人不知道那是
// 什么意思。所以零值的 *auth.Error（Reason 没填）也走 unclassified。
func authFailureReason(err error) string {
	var authErr *auth.Error
	if errors.As(err, &authErr) && authErr.Reason != "" {
		return string(authErr.Reason)
	}
	return reasonUnclassified
}

// reasonUnclassified 是「回了 401 但取不到原因」这一类在指标上的取值。
//
// 它不是 auth.Reason 的取值之一：那个枚举是闭合的（八个），本包不去扩它
// ——扩了的话，采集端的告警规则要跟着改，而它本来只需要认 auth 那八个。
//
// 这条序列按设计不该出现。它一旦出现，说明有一条 401 不是由 *auth.Error
// 产生的，而那种 401 在排查时回答不了「为什么不能用了」。
const reasonUnclassified = "unclassified"

// served 是一次请求走完之后的结局。
//
// 两个字段都不是「有没有出错」的另一种说法，所以它们不能被合成一个 error：
// 一个是「能不能重放」，一个是「这次请求到底做没做」。调用方要按它们
// 分别处置，而「失败了」这句话把两件事都盖住了。
type served struct {
	// complete 为 true 表示响应**完整地**写给了调用方，可以重放。
	//
	// 为 false 不等于出错：调用方中途断开、上游失败都是正常结局，
	// 只是这一次没有形成完整的响应。
	complete bool

	// claimed 为 true 表示这个幂等键已被另一个并发请求占住，本次请求
	// 什么都没做——预留与冻住的钱都随事务回滚了。
	//
	// 它与 complete 的差别在**有没有副作用**：claimed 为 true 时上游
	// 一次都没被调用过，所以调用方可以直接去等那个正在跑的请求。
	claimed bool
}

// serveOnce 走一次原路径：选路、调上游、把结果写出去。
//
// claim 非 nil 时它会被带进计费请求，落库层据此把幂等记录与预留写在
// 同一个事务里（见 pipeline.Request.Idempotency）。
//
// 它返回的是**结局**而不是错误：各个失败分支已经自己把响应写出去了
// （或者判断出没人在听），调用方需要的只是「能不能重放」。
func (h *Handler) serveOnce(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, upstream *channel.Request, user User, requestID string, claim *idempotency.Claim) served {
	// 插件在**构造计费请求之前**跑，位置是有约束的而不是随手放的：
	// 补丁能改 max_tokens，而它是预留上界的关键（见 billingRequest 的
	// MaxTokens 与 pipeline.Reserve）。排在 billingRequest 之后的话，那份
	// 计费请求里留着的仍是补丁前的值，于是插件调小 max_tokens 不影响预留
	// ——上界与上报值就成了两个来源，而这件事在响应上完全看不出来。
	//
	// 它同时排在预留之前（预留在 gateway.begin 里）。这一条由
	// plugin.ApplyRequest 的注释与 api 的用例一起钉住。
	sess, ok := h.runRequestHook(w, r, protocol, upstream, user, requestID)
	if !ok {
		// 阻断型插件失败，响应已经写出去了。
		return served{}
	}
	if sess != nil {
		defer sess.Close()
		defer h.logPluginFailures(r.Context(), sess, requestID)
	}

	req := h.billingRequest(user, upstream, requestID, protocol)
	req.Idempotency = claim

	if upstream.Stream {
		// 流式与非流式的差别只在拿到结果之后怎么写出：前面的检查顺序
		//（方法、Content-Type、鉴权、请求体、解析、必要字段）完全一样。
		//
		// 挂载点**两个入口都要传**：onRoute 与选渠道是同一件事，流式与非流式
		// 都要走（见 Chatter.ChatStream）。各自只调自己那条路上会发生的那些
		// ——onResponse 在流式那边不会被调到（响应是边生成边发的，没有
		// 「整份响应」这个时刻），onStream 在非流式那边不会被调到（没有
		// 「块」可言）。传下去的那个闭包本来就不会被调，所以这里不额外判断
		// （见 docs/plugin-contract.md）。
		return h.serveStream(w, r, protocol, upstream, req, h.pluginHooks(sess, requestID, upstream.Model, protocol))
	}

	res, err := h.Chatter.Chat(r.Context(), req, upstream, h.pluginHooks(sess, requestID, upstream.Model, protocol))
	if err != nil {
		if errors.Is(err, idempotency.ErrClaimed) {
			// 键被另一个并发请求抢走了。这不是失败：那次预留整个回滚了，
			// 一分钱没冻、上游一次没调。交给调用方去等那个请求的结果。
			return served{claimed: true}
		}
		if r.Context().Err() != nil {
			// 调用方已经断开（取消或超时）。没有人在听，写响应只是白费
			// 一次序列化；原因留在日志里，那条日志带着请求标识。
			observe.FromContext(r.Context()).Warn("调用方在响应前断开", "err", err)
			return served{}
		}
		h.failUpstream(w, r, protocol, err)
		return served{}
	}
	if res == nil || res.Response == nil {
		// 非流式入口拿到没有 Response 的结果，只可能是上游那条路走了流式。
		// 这是装配错误，不是调用方的错。
		observe.FromContext(r.Context()).Error("非流式入口收到没有响应体的结果")
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return served{}
	}

	// 报给调用方的模型名是**他要的那个**，不是上游回报的那个：
	// 上游名是我们与上游之间的事，而调用方按自己请求里的名字理解响应。
	// 上游实际用了什么记在 channel.Response.UpstreamModel 里，供计费与排查。
	rendered, err := renderSuccess(protocol, requestID, upstream.Model, h.now(), res.Response)
	if err != nil {
		observe.FromContext(r.Context()).Error("渲染响应失败", "err", err)
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return served{}
	}
	writeJSON(w, http.StatusOK, rendered)
	return served{complete: true}
}

// runRequestHook 为这次请求建插件会话，并把 onRequest 的补丁应用上去。
//
// 返回的会话为 nil 表示这次请求不跑插件：插件系统没接上、没有匹配的插件、
// 或者运行时建不起来。ok 为 false 表示**阻断型插件失败**——那时响应已经
// 写出去了，调用方直接返回。
//
// 建会话失败按「这次请求没有插件」处置而不是让请求失败：插件是可选增强，
// 一个建不起运行时的插件（内存限额、宿主注入出问题）不该让整个入口开始
// 拒服务。代价要说清楚：**声明了 blocking 的插件在建会话这一步失败时
// 也没能让请求失败**，因为那时还拿不到那个插件的 meta。原因是记进日志的。
func (h *Handler) runRequestHook(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, upstream *channel.Request, user User, requestID string) (*plugin.Session, bool) {
	if h.Plugins == nil {
		return nil, true
	}
	ctx := r.Context()

	// 规范名与上游模型名在这里取同一个值，与 billingRequest 同一条已知的
	// 临时状态：规范名到上游名的映射还没做（那属于选路层）。
	sess, err := h.Plugins.NewSession(ctx, plugin.SessionRequest{
		CanonicalModel: upstream.Model,
		Protocol:       string(protocol),
	}, h.pluginHost(requestID, user.ID), h.PluginLimits)
	switch {
	case errors.Is(err, plugin.ErrNoPlugin):
		// 正常：这次请求没有匹配的插件。
		return nil, true
	case err != nil:
		observe.FromContext(ctx).Error("建插件会话失败，这次请求不跑插件",
			"err", err, "request_id", requestID)
		return nil, true
	}

	rejected, err := sess.ApplyRequest(ctx, requestID, upstream, upstream.Model, string(protocol))
	if err != nil {
		// 阻断型插件失败：请求失败。这时还没有任何副作用——预留还没发生，
		// 幂等记录也还没建，所以调用方重试是安全的。
		h.failUpstream(w, r, protocol, err)
		sess.Close()
		return nil, false
	}
	for _, name := range rejected {
		// 被拒的头要报出去：插件作者写了一个被拒的头却没有任何反馈，
		// 会以为是上游不支持——而真正的原因是它不在白名单里。
		observe.FromContext(ctx).Warn("插件加的请求头被拒",
			"header", name, "plugin", sess.Plugin.Key, "request_id", requestID)
	}
	return sess, true
}

// pluginHost 把宿主能力的构造绑上这次请求的标识。
//
// 插件 key 由注册表选中之后回填（见 plugin.HostBuilder）：宿主日志里那个
// 字段要与真正跑的那个插件对得上。
func (h *Handler) pluginHost(requestID string, userID int64) plugin.HostBuilder {
	if h.PluginHost == nil {
		return nil
	}
	return func(p *plugin.Plugin) sandbox.Host { return h.PluginHost(p, requestID, userID) }
}

// pluginHooks 把这次请求的插件会话接到编排层的挂载点上。
//
// 挂载点一起给，因为它们出自**同一个会话**：分几次建会话会让各个 hook 看到
// 几份互不相干的插件状态（KV、计数器、Failures），而插件作者眼里它们是一次
// 请求的几步。
//
// sess 为 nil 时返回零值：不跑插件，与「这次请求没有插件」是同一个意思。
// **关掉插件的行为不变是结构性的**：零值 Hooks 的字段全是 nil，编排层各自
// 走那条不碰插件的分支（见 gateway.routeChat 与 gateway.pumpStream）。
//
// canonicalModel 由调用方给出，与插件在别的 hook 里看到的那个名字是同一个
// （今天规范名与上游名还是同一个值，见 serveOnce）。
func (h *Handler) pluginHooks(sess *plugin.Session, requestID, canonicalModel string, protocol channel.Protocol) gateway.Hooks {
	if sess == nil {
		return gateway.Hooks{}
	}
	return gateway.Hooks{
		OnRoute: func(ctx context.Context, candidates []route.Candidate) (int64, error) {
			res := sess.OnRoute(ctx, plugin.RouteContextFrom(requestID, canonicalModel, candidates))
			if err := res.Err(); err != nil {
				// 阻断型插件失败：这次请求失败。非阻断的失败这里为 nil
				// ——它已经记进 Failures()，请求按内核排序继续。
				return 0, err
			}
			if res.Patch == nil {
				// 没给决策（插件没注册这个 hook、返回 undefined、或返回值
				// 解出来是 0）。三种都归成「不钉」，与插件出现之前一样。
				return 0, nil
			}
			return res.Patch.ChannelID, nil
		},
		OnStream: func(ctx context.Context, ev channel.StreamEvent, in gateway.StreamChunk) (gateway.StreamDecision, error) {
			res := sess.OnStream(ctx, plugin.StreamContext{RequestID: requestID}, plugin.StreamInput{
				// 交给插件的是**挑出来的那几项**，不是 channel.StreamEvent
				// 本身：共用一个类型意味着给那个类型加一个字段就静默扩大
				// 了插件的可见范围（见 plugin.StreamChunkFrom）。
				Chunk:                   plugin.StreamChunkFrom(ev),
				Index:                   in.Index,
				AccumulatedText:         in.AccumulatedText,
				AccumulatedTokensApprox: in.AccumulatedTokensApprox,
			})
			if err := res.Err(); err != nil {
				// 阻断型插件失败：这条流到此为止（网关按提前终止收尾）。
				// 非阻断的失败这里为 nil——它已经记进 Failures()，而返回的
				// 零值决策让这一块原样转发。
				return gateway.StreamDecision{}, err
			}
			if res.Patch == nil {
				return gateway.StreamDecision{}, nil
			}
			return gateway.StreamDecision{
				Drop:      res.Patch.Drop,
				Replace:   res.Patch.Replace,
				Terminate: res.Patch.Terminate,
			}, nil
		},
		OnResponse: func(ctx context.Context, resp *channel.Response) error {
			metadata, err := sess.ApplyResponse(ctx, requestID, resp)
			if len(metadata) > 0 {
				// 元数据的落点是日志：它在协议层没有对应字段，所以这里是
				// 它唯一看得见的出口（见 plugin.ApplyResponsePatch）。
				observe.FromContext(ctx).Info("插件给了响应元数据",
					"plugin", sess.Plugin.Key, "request_id", requestID, "metadata", metadata)
			}
			return err
		},
		OnUsage: func(ctx context.Context, usage channel.Usage) (channel.Usage, error) {
			// 合并这一步在插件包里（见 plugin.Session.ApplyUsage）：一次
			// 越界的上报会被整份丢掉，而丢掉必须记进会话才看得见——那两个
			// 方法未导出，在包外接不出来。
			return sess.ApplyUsage(ctx, requestID, string(protocol), usage)
		},
	}
}

// logPluginFailures 把这次请求里被吞掉的插件失败写进日志。
//
// 非阻断的失败不改变请求的结果，所以它**只能靠日志被看见**：一个每次都
// 超时但「不影响请求」的插件，从响应上完全看不出来（见 Session.Failures）。
// 阻断型的失败另有出口（请求以错误结束），但一起记在这里也不重复——
// 它同样是这次请求的现场。
func (h *Handler) logPluginFailures(ctx context.Context, sess *plugin.Session, requestID string) {
	failures := sess.Failures()
	if len(failures) == 0 {
		return
	}
	log := observe.FromContext(ctx)
	for _, f := range failures {
		log.Warn("插件失败", "err", f, "plugin", sess.Plugin.Key, "hook", f.Hook, "request_id", requestID)
	}
}

// serveStream 处理一次流式请求。
//
// # 两个阶段的分界是「响应头发出去没有」
//
// 拿到事件流**之前**的一切失败都还能照常回一个错误响应（状态码与错误体都
// 按调用协议给）；之后的一切失败只能作为流里的事件表达。分界点就是
// ChatStream 的返回值：它只在**上游接受请求之前**失败时才返回错误。
//
// # 每帧都要 flush
//
// 不 flush 的话事件会攒在 Go 的响应缓冲里，「流式」在客户端看来就是
// 「等一会儿一次性返回」。这里的取舍与计费无关，但它是这个接口存在的
// 全部意义。
//
// # 返回值的含义
//
// 只有**完整走完**（收到 done 且结束帧真的写出去了）才返回 complete。
// 截断的流不能重放：重放一段半截的回答比不重放更坏，调用方会把它当成
// 一次成功的完整回答。
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, upstream *channel.Request, req pipeline.Request, hooks gateway.Hooks) served {
	// 渲染器要在写响应头**之前**建好：协议认不出来时还能回一个 500，
	// 而头一旦发出去就只剩下截断一条路。
	renderer, err := newStreamRenderer(protocol, req.RequestID, upstream.Model, h.now(), inputTokensOf(upstream))
	if err != nil {
		observe.FromContext(r.Context()).Error("流式入口收到未知协议", "err", err)
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return served{}
	}

	res, err := h.Chatter.ChatStream(r.Context(), req, upstream, hooks)
	if err != nil {
		if errors.Is(err, idempotency.ErrClaimed) {
			// 与并发请求撞上了，理由见 serveOnce 里同一条分支。
			return served{claimed: true}
		}
		if r.Context().Err() != nil {
			observe.FromContext(r.Context()).Warn("调用方在流开始前断开", "err", err)
			return served{}
		}
		// 流还没开始，一个字都没发出去：这里的处置与非流式完全一样。
		h.failUpstream(w, r, protocol, err)
		return served{}
	}
	if res == nil || res.Events == nil {
		// 流式入口拿到没有事件流的结果，只可能是上游那条路走了非流式。
		// 这是装配错误，不是调用方的错。
		observe.FromContext(r.Context()).Error("流式入口收到没有事件流的结果")
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return served{}
	}

	writeStreamHeaders(w)
	rc := http.NewResponseController(w)
	// 写一帧再 flush。写失败与 flush 失败在这里是同一件事：调用方不在了。
	emit := func(frameErr error) error {
		if frameErr != nil {
			return frameErr
		}
		return rc.Flush()
	}

	if err := emit(renderer.start(w)); err != nil {
		// 连第一帧都写不出去，说明调用方已经不在了。事件流仍然要读空。
		observe.FromContext(r.Context()).Warn("写首帧失败", "err", err)
		drainStream(res.Events)
		return served{}
	}

	// 结束帧只在**真的收到 done** 时才写。没有 done 就是截断：不补一个
	// 正常结束，调用方据此知道回答是半截的（见 stream.go 的说明）。
	done := false
	for ev := range res.Events {
		if ev.Kind == "done" {
			done = true
		}
		if err := emit(renderer.event(w, ev)); err != nil {
			// 写不出去：调用方多半已经断开。剩下的不再写，但**必须把事件流
			// 读空**——网关的转发 goroutine 阻塞在发送上，不读空它就不会
			// 结算，那笔预留也就一直冻着。
			observe.FromContext(r.Context()).Warn("转发事件失败", "err", err)
			drainStream(res.Events)
			return served{}
		}
	}
	if !done {
		// 截断的流：客户端收到的是半截回答，所以它**不能**被重放。
		return served{}
	}
	if err := emit(renderer.finish(w)); err != nil {
		// 结束帧没写出去，客户端看到的同样是半截回答。
		observe.FromContext(r.Context()).Warn("写结束帧失败", "err", err)
		return served{}
	}
	return served{complete: true}
}

// drainStream 把剩下的事件读掉。
//
// 它必须在**后台**做：事件流的另一端可能还在等上游吐下一片，在请求处理
// 的 goroutine 里等下去会把一个已经断开的请求一直挂在这里。读空之后网关
// 才会走结算，那笔钱也才会被处置。
func drainStream(events <-chan channel.StreamEvent) {
	go func() {
		for range events {
		}
	}()
}

// failUpstream 把一次请求处理的失败翻译成响应，并把原因写进日志。
//
// 两件事都要做，因为它们是给不同的人看的：调用方拿到一句概括（不回上游的
// 错误原文——那里面可能回显请求内容），运维从日志里拿到完整原因。
// 只做前者的话，一次线上故障就没有任何现场。
//
// 进来的不全是上游的错误：这个函数是 serveOnce 与 serveStream 两条路上
// **唯一**的失败出口，所以候选来源、预留、账本这些我们自己的环节也走这里
// （见 httpStatusOfUpstream 的说明）。
func (h *Handler) failUpstream(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, err error) {
	log := observe.FromContext(r.Context())

	// 额度用尽要单独接住：它是唯一一个**状态码推不出码**的情形。
	//
	// 它与限流共用 429，但处置相反——限流等一会儿就对了，额度等多久都不会
	// 变——所以码必须显式给出（见 writeInsufficientQuota）。走下面那条按
	// 状态码推形状的路，调用方只会看到一个光秃秃的 429，然后照限流那样退避
	// 重试，而它永远不会成功。
	if errors.Is(err, keyquota.ErrExceeded) {
		log.Warn("请求超出该 Key 的额度上限", "err", err)
		writeInsufficientQuota(w, protocol)
		return
	}

	status, message := httpStatusOfUpstream(err)
	if status >= http.StatusInternalServerError {
		// 5xx 意味着「不是调用方的错」，这类故障必须有人能看到。
		// 措辞不写「上游失败」：5xx 里也有账本、候选来源这些我们自己的环节。
		log.Error("请求处理失败", "err", err, "status", status)
	} else {
		// 4xx 里既有上游拒绝的（限流），也有我们自己在调上游之前就判定的
		// （缺 max_tokens）。措辞不写「上游拒绝」：那样一次「调用方漏填字段」
		// 会被记成上游的问题，看日志的人会朝上游找。是谁拒的由 err 自己说。
		log.Warn("请求被拒绝", "err", err, "status", status)
	}
	writeError(w, protocol, status, message)
}

// httpStatusOfUpstream 把一个上游调用错误映射成给调用方的状态码与一句话。
//
// 原则是「谁的错回谁的错」：
//
//   - 没有可用渠道是**我们的**问题（渠道挂了或没配），回 503。
//   - 缺 max_tokens 是**调用方的**问题，回 400 并点名这个字段。预留要有
//     输出上界才做得出来（见 pipeline.Request.validate），而「没给上界」
//     是请求本身的缺陷。不映射的话它会落到默认分支，表现为 500——把
//     调用方漏填一个字段说成我们内部故障，他会去查我们而不是改请求。
//   - 上游说限流原样透出 429：调用方该退避，这个信息对他有用。
//   - 其余上游错误一律回 502，**不**跟着上游回 400。上游说「你的请求
//     不对」时回 400，会让调用方去改一个本来正确的请求，而真正要改的是
//     我们与上游之间的适配。
//   - 认不出来的错误回 500：它们来自候选来源、预留、账本这些我们自己的
//     环节。把它们当成 502 会把我们的故障说成上游的故障，调用方据此去
//     联系上游就找错了人。
func httpStatusOfUpstream(err error) (status int, message string) {
	switch {
	case errors.Is(err, billing.ErrNoUpperBound):
		return http.StatusBadRequest, "max_tokens is required: the request must set a positive output token limit"
	case errors.Is(err, route.ErrNoCandidates):
		return http.StatusServiceUnavailable, "no available upstream for the requested model"
	case errors.Is(err, gateway.ErrNoExecutor):
		return http.StatusNotImplemented, "no upstream executor is configured"
	}

	var up *channel.ErrUpstream
	switch {
	case errors.As(err, &up) && up.StatusCode == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "upstream is rate limited, retry later"
	case errors.As(err, &up):
		return http.StatusBadGateway, "upstream call failed"
	}

	// 候选都失败了，而最后一个错误不是上游错误（例如上游不支持这个协议）。
	// 这一条要排在 ErrUpstream 之后：最后一次尝试是限流时，那个信息比
	// 「全都失败了」更有用。
	var allFailed *route.Errors
	if errors.As(err, &allFailed) {
		return http.StatusBadGateway, "upstream call failed"
	}
	return http.StatusInternalServerError, "internal error"
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// isJSON 判断 Content-Type 是不是 JSON。
//
// 用 mime.ParseMediaType 而不是前缀比较：真实客户端会带参数
// （application/json; charset=utf-8），前缀比较会把它们全部挡在 415 上，
// 而这类错误在联调时最费时间——请求本身完全正确。
func isJSON(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// writeJSON 写出一个 JSON 响应体。
//
// 状态码在写体之前定好：WriteHeader 一调用头就发出去了，之后想改状态码
// 只能靠日志解释。
func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// 编译期断言：*gateway.Gateway 满足本包的 Chatter。
//
// 放在这里是为了让「gateway.Chat 的签名改了」在改的那一次就报错，
// 而不是等到装配的时候才发现接不上。
var _ Chatter = (*gateway.Gateway)(nil)
