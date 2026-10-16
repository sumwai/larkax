// Package api 是 larkax 的公共 HTTP 入口。
//
// 它只做一件事：把一次 HTTP 请求变成一次上游调用，再把结果变回 HTTP 响应。
// 计费、选路、协议转换、账本都在别的包里，这里只决定它们的调用顺序，
// 以及**对外契约的形状**——路径、状态码、错误体与非流式响应体的字段。
//
// # 协议由路径决定
//
// /v1/chat/completions 说 OpenAI 协议，/v1/messages 说 Anthropic 协议。
// 不从请求体猜：两套协议的请求体在浅层很像（都有 model 与 messages），
// 猜错时解析往往照样成功，于是错误被推迟到上游才暴露。路径是调用方明确
// 给出的信息，用它就不需要猜。
//
// 错误体也按调用协议给形状（见 docs/versioning.md）：用 OpenAI 格式调用
// 就得到 OpenAI 形状的错误，Anthropic 同理。这一条不能混——调用方的 SDK
// 按自己那套解错误体，形状不对时它连错误类型都读不出来。
//
// # 未实现的路径仍然回 501
//
// 这两条路径之外的任何请求都得到 501，与推理接口尚未实现时的行为一致。
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

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/relay"
	"github.com/sumwai/larkax/internal/route"
)

// 公共入口的两条推理路径。它们与调用协议一一对应，见包注释。
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
	Chat(ctx context.Context, req pipeline.Request, upstream *channel.Request) (*gateway.Result, error)

	// ChatStream 处理一次流式请求。返回的 Result.Events 由调用方读到关闭
	// 为止——**必须读到底**：网关的转发 goroutine 在关闭它之前完成结算，
	// 不读到底那条 goroutine 会阻塞在发送上，那笔预留就一直冻着。
	//
	// 返回错误只发生在**上游接受请求之前**（没有可用渠道、连不上、非 2xx），
	// 那时还没有任何内容发给客户端，调用方可以照常回一个错误响应。
	ChatStream(ctx context.Context, req pipeline.Request, upstream *channel.Request) (*gateway.Result, error)
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

	// Version 是发布版本，写进每个响应的 x-larkax-version 头。
	// 见 docs/versioning.md：用户报问题时，「你调的是哪个版本」是最该先
	// 确认的事。留空则不写这个头。
	Version string

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
	default:
		// 未知路径：这里不知道调用方说哪套协议（协议由路径决定，而路径
		// 我们并不认识），所以用 OpenAI 形状——两套里更通用的那一套。
		writeError(w, channel.ProtocolOpenAI, http.StatusNotImplemented,
			"this endpoint is not implemented")
	}
}

// serve 处理一条推理路径上的一次请求。
//
// 检查顺序是刻意的：先是纯请求头层面的判断（方法、Content-Type），
// 再是鉴权，最后才读请求体。这样没通过鉴权的请求不会让我们为它读进
// 最多 maxRequestBodyBytes 的内存，而 Content-Type 这种一眼可判的
// 错误也轮不到花代价去解析。
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, protocol channel.Protocol) {
	if r.Method != http.MethodPost {
		// Allow 头是 405 的必要部分：没有它，调用方无从知道该用什么方法，
		// 只能去翻文档。
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, protocol, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		writeError(w, protocol, http.StatusUnsupportedMediaType,
			"Content-Type must be application/json")
		return
	}
	if h.Auth == nil || h.Chatter == nil {
		// 依赖没装配。这不是调用方的错，也不该在请求期 panic——
		// 一个 501 说明「这个构建没接上」，比一个栈回溯有用。
		writeError(w, protocol, http.StatusNotImplemented,
			"inference is not wired in this build")
		return
	}

	user, err := h.Auth.Authenticate(r.Context(), r)
	if err != nil {
		// 具体原因不回给调用方：凭证不对、用户被停用、Key 过期在响应上
		// 是同一种答复，区分开等于送出一个「这个 Key 存在吗」的探测器。
		writeError(w, protocol, http.StatusUnauthorized, "invalid credentials")
		return
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
			return
		}
	}

	body, err := readBody(w, r)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeError(w, protocol, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", maxRequestBodyBytes))
			return
		}
		writeError(w, protocol, http.StatusBadRequest, "could not read request body")
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
	if upstream.Stream {
		// 流式与非流式的差别只在拿到结果之后怎么写出：前面的检查顺序
		//（方法、Content-Type、鉴权、请求体、解析、必要字段）完全一样。
		h.serveStream(w, r, protocol, upstream, user, requestID)
		return
	}

	res, err := h.Chatter.Chat(r.Context(), billingRequest(user, upstream, requestID), upstream)
	if err != nil {
		if r.Context().Err() != nil {
			// 调用方已经断开（取消或超时）。没有人在听，写响应只是白费
			// 一次序列化；原因留在日志里，那条日志带着请求标识。
			observe.FromContext(r.Context()).Warn("调用方在响应前断开", "err", err)
			return
		}
		h.failUpstream(w, r, protocol, err)
		return
	}
	if res == nil || res.Response == nil {
		// 非流式入口拿到没有 Response 的结果，只可能是上游那条路走了流式。
		// 这是装配错误，不是调用方的错。
		observe.FromContext(r.Context()).Error("非流式入口收到没有响应体的结果")
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return
	}

	// 报给调用方的模型名是**他要的那个**，不是上游回报的那个：
	// 上游名是我们与上游之间的事，而调用方按自己请求里的名字理解响应。
	// 上游实际用了什么记在 channel.Response.UpstreamModel 里，供计费与排查。
	rendered, err := renderSuccess(protocol, requestID, upstream.Model, h.now(), res.Response)
	if err != nil {
		observe.FromContext(r.Context()).Error("渲染响应失败", "err", err)
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, rendered)
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
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, upstream *channel.Request, user User, requestID string) {
	// 渲染器要在写响应头**之前**建好：协议认不出来时还能回一个 500，
	// 而头一旦发出去就只剩下截断一条路。
	renderer, err := newStreamRenderer(protocol, requestID, upstream.Model, h.now(), inputTokensOf(upstream))
	if err != nil {
		observe.FromContext(r.Context()).Error("流式入口收到未知协议", "err", err)
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return
	}

	res, err := h.Chatter.ChatStream(r.Context(), billingRequest(user, upstream, requestID), upstream)
	if err != nil {
		if r.Context().Err() != nil {
			observe.FromContext(r.Context()).Warn("调用方在流开始前断开", "err", err)
			return
		}
		// 流还没开始，一个字都没发出去：这里的处置与非流式完全一样。
		h.failUpstream(w, r, protocol, err)
		return
	}
	if res == nil || res.Events == nil {
		// 流式入口拿到没有事件流的结果，只可能是上游那条路走了非流式。
		// 这是装配错误，不是调用方的错。
		observe.FromContext(r.Context()).Error("流式入口收到没有事件流的结果")
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return
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
		return
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
			return
		}
	}
	if done {
		if err := emit(renderer.finish(w)); err != nil {
			observe.FromContext(r.Context()).Warn("写结束帧失败", "err", err)
		}
	}
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

// failUpstream 把一次上游调用的失败翻译成响应，并把原因写进日志。
//
// 两件事都要做，因为它们是给不同的人看的：调用方拿到一句概括（不回上游的
// 错误原文——那里面可能回显请求内容），运维从日志里拿到完整原因。
// 只做前者的话，一次线上故障就没有任何现场。
func (h *Handler) failUpstream(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, err error) {
	status, message := httpStatusOfUpstream(err)
	log := observe.FromContext(r.Context())
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
