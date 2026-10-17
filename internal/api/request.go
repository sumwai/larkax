package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// errBodyTooLarge 表示请求体超过了 maxRequestBodyBytes。
var errBodyTooLarge = errors.New("请求体超过上限")

// readBody 读取请求体，超过 maxRequestBodyBytes 时返回 errBodyTooLarge。
//
// 用 http.MaxBytesReader 而不是先看 Content-Length：那个头是调用方给的，
// 可以缺失（分块传输）也可以撒谎，而 MaxBytesReader 是在**读的时候**限制，
// 于是一个声称很小、实际很大的请求体也吃不到多余的内存。超限时它还会
// 让连接不再复用，剩下的字节不会被接着读下去。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errBodyTooLarge
		}
		return nil, fmt.Errorf("读取请求体失败: %w", err)
	}
	return body, nil
}

// rejection 是一次请求检查没通过的结果。
//
// 它是个普通的决策值而不是 error：这里既没有要 unwrap 的成因链，也没有
// 要按类型分支的调用方——状态码与给调用方的措辞一起决定，然后直接写出响应。
// 拆成 error 再映射一次，只会让「为什么拒绝」与「回什么状态码」在两处
// 各写一遍，然后慢慢不一致。
type rejection struct {
	status  int
	message string
}

// checkRequest 检查请求里缺了就没法处理的部分。返回 nil 表示可以继续。
//
// 只检查「无论如何都做不成」的两种：没有模型名、没有消息。其余的参数由
// 上游判断——各家支持的范围不同，在这里按我们的猜测拒收，会把上游支持的
// 请求挡在门外。
//
// protocol 是**调用方走的入口**，不参与判据（判据是转换之后的
// channel.Request.Messages，三条入口共用），只决定措辞该点哪个字段——见
// emptyRequestMessage。
func checkRequest(req *channel.Request, protocol channel.Protocol) *rejection {
	if req.Model == "" {
		return &rejection{status: http.StatusBadRequest, message: "model is required"}
	}
	if len(req.Messages) == 0 {
		return &rejection{status: http.StatusBadRequest, message: emptyRequestMessage(protocol)}
	}
	return nil
}

// emptyRequestMessage 是「请求里一条消息都没有」时给调用方的措辞。
//
// 判据在三条入口上是同一个（转换之后没有消息），但**字段名是各协议自己的**：
// /v1/chat/completions 与 /v1/messages 的调用方发的是 messages，而
// /v1/responses 的调用方发的是顶层 input——后者收到「messages is required」
// 会去改一个它从来没写过的字段。
func emptyRequestMessage(protocol channel.Protocol) string {
	if protocol == channel.ProtocolOpenAIResponses {
		return "input is required"
	}
	return "messages is required"
}

// billingRequest 组装计费侧要的那份请求。
//
// 计费要知道的与调上游要知道的是**两个请求**（见 pipeline.Request 的注释
// 与 Chatter 的签名），这里只放计费要的，上游要的留在 upstream 里。
//
// protocol 由调用方从路由处带进来：它是「调用方走的是哪条入口」这件事，
// 与 upstream 里那些上游参数无关，所以不放进 upstream。
//
// 资产品种取自 Handler.DefaultAsset——那是装配层从 settings 的
// billing.default_asset 读出来的值。本包不认识那个键，也不给默认值：
// 计费层拒绝空品种（见 pipeline.Request.validate），所以「装配时忘了填」
// 会表现为每个请求都失败，而不是一笔记错品种的账。
func (h *Handler) billingRequest(user User, upstream *channel.Request, requestID string, protocol channel.Protocol) pipeline.Request {
	return pipeline.Request{
		RequestID: requestID,
		UserID:    user.ID,
		// APIKeyID 与 UserID 一起取自鉴权结果，但两者不是一回事：一个用户
		// 可以有多把 key，而账单要能追到**哪一把**在用。鉴权那一步已经把它
		// 解析出来了（限流按 key 计，用的就是同一个值），这里只是不让它在
		// 入口与落库层之间断掉——断了的表现是那一列恒为 NULL，
		// 而单看任何一层都像是写了的。
		APIKeyID: user.KeyID,
		Asset:    h.DefaultAsset,
		// CanonicalModel 是**调用方要的那个**名字。上游名由选路层按选中的
		// 渠道翻出来（见 internal/route 的 upstreamRequest），落进
		// billing_requests.upstream_model；两者在这一层分开，是因为入口
		// 还不知道会走哪个渠道，而别名是逐渠道配的。
		CanonicalModel: upstream.Model,
		MaxTokens:      int64(upstream.MaxTokens),
		InputTokens:    inputTokensOf(upstream),
		Streamed:       upstream.Stream,
		// 协议不能由这一层推：它由路径决定，而路径是调用方的事
		// （见 pipeline.Request.Protocol）。
		Protocol: protocol,
	}
}

// inputTokensOf 估算请求的输入 token 数。
//
// 预留发生在调用上游之前，那时拿不到实测值，只能用估算（见 internal/tokens）。
// 只估消息里的文本：工具定义与图片的 token 消耗各家口径不同，硬估出来的
// 数字比文本部分的估算更不可信（口径见 channel.Request.PromptText）。估算
// 偏低会让预留偏小，而结算是按预留的金额封顶的（见 pipeline.Reserve），
// 所以这个取舍的代价是**少收**，不是超收——宁可少收也不要在用户余额上
// 多冻一笔。
//
// 流式那条路用同一个函数：它要填进 Anthropic 的 message_start，而同一个
// 请求在预留与响应里报出两个不同的输入量，只会让对账的人去找一个不存在的
// 差额。
func inputTokensOf(req *channel.Request) int64 {
	return tokens.EstimatedTokens(req.Model, req.PromptText())
}

// requestIDOf 取出本次请求的关联标识，没有就生成一个。
//
// 不能留空：它同时是响应里的 id 与计费的幂等键，而空字符串在库里是
// 一条会与下一条撞上的唯一约束（billing_requests.request_id）。
// 调用链上没有 observe.Middleware 时（例如某个调用方直接用了本 handler），
// 这里补一个，而不是把「有没有装中间件」变成计费能不能落库的前提。
func requestIDOf(r *http.Request) string {
	if id := observe.RequestIDFromContext(r.Context()); id != "" {
		return string(id)
	}
	return string(observe.NewRequestID())
}
