package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// 错误响应的形状与调用协议绑定（见 docs/versioning.md）：用 OpenAI 格式
// 调用就返回 OpenAI 形状的错误，Anthropic 同理。这一条不能混——调用方的
// SDK 按自己那套去解错误体，形状不对时它连错误类型都读不出来，于是
// 「参数写错了」会表现为「服务返回了无法解析的响应」。
//
// 所以本文件里每种形状都是一份**独立的**结构体，不做「一套字段两种渲染」：
// 两家的字段名与嵌套层次都不一样（OpenAI 是 error.message，Anthropic 是
// error.message 加顶层的 type），硬凑成一套只会让两边的形状都不对。
//
// # Responses 那一套为什么没有自己的结构体
//
// 它的**非流式**错误体与 OpenAI 那一套是同一个形状（{"error":{message,
// type,param,code}}，两家共用同一个官方 SDK 的错误解析器），逐字相同，
// 所以这里没有第三个结构体——多一份一模一样的定义只会在改一处时漏掉另一处。
// 这与 systemone.go 把「未知路径」那一支也归到 OpenAI 形状是同一个判断。
//
// 它**流式**那一条是第三套形状：一个平铺的 error 事件（type / code /
// message / param 在顶层，没有 error 外层）。那一份在 stream.go 的
// responsesStream.errorFrame 里，与这里不是同一件事。

// openAIErrorBody 是 OpenAI 的错误体。
type openAIErrorBody struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	// Param 与 Code 恒为 null 或具体的机器可读值。两个字段都出现在
	// OpenAI 的真实响应里，缺了它们有些客户端会当成字段丢失。
	Param *string `json:"param"`
	Code  *string `json:"code"`
}

// claudeErrorBody 是 Anthropic 的错误体。
type claudeErrorBody struct {
	Type  string      `json:"type"`
	Error claudeError `json:"error"`
}

type claudeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// writeError 按调用协议写出一个错误响应，机器可读的错误码由状态码推出。
//
// message 是**给调用方看的**：不回上游的错误原文、不回内部路径与标识。
// 上游的错误体里可能回显请求内容，而调用方不需要它也知道该改什么；
// 需要细节时用响应头里的请求标识去查日志。
func writeError(w http.ResponseWriter, protocol channel.Protocol, status int, message string) {
	writeErrorCode(w, protocol, status, "", message)
}

// writeErrorCode 与 writeError 相同，但错误码由调用方给定而不是从状态码推。
//
// 用途是同一状态码下的几种情形**处置不同**的时候——幂等键的三条 409 就是
// 这样：一条该换个键重发，一条该稍后重试，一条该换个新键重试。
// 只给状态码的话客户端只能靠读 message 来分辨，而 SDK 不做那件事。
//
// # 码放在哪里，两套协议不一样
//
// 这不是可以统一的地方：
//
//   - OpenAI 的错误体有独立的 error.code 字段，码写在那里，type 仍然是
//     它自己那套（见 errorKind）。
//   - Anthropic 的错误体只有 error.type 一个机器可读的位置，所以码只能
//     写在那里。它的官方取值集合里没有这几个码，但 SDK 按字符串分支，
//     读得出来；写进 message 才是真的读不出来。
func writeErrorCode(w http.ResponseWriter, protocol channel.Protocol, status int, code, message string) {
	kind, derived := errorKind(protocol, status)

	var body any
	if protocol == channel.ProtocolAnthropic {
		typ := code
		if typ == "" {
			typ = kind
		}
		body = claudeErrorBody{
			Type:  "error",
			Error: claudeError{Type: typ, Message: message},
		}
	} else {
		out := openAIError{Message: message, Type: kind}
		// 调用方给的码优先，其次才是从状态码推出来的那个（凭证、体积那几条
		// 靠它区分，见 errorKind）。
		switch {
		case code != "":
			out.Code = &code
		case derived != "":
			out.Code = &derived
		}
		body = openAIErrorBody{Error: out}
	}

	// 结构体里只有字符串与指针，json.Marshal 不会失败，所以这里没有
	// 一条「序列化失败」的分支：那种分支永远测不到，而它一旦真的存在
	// 就说明这个结构体里混进了不能编码的东西——那该在改结构体的那一次
	// 被发现，不是在这里兜住。
	raw, _ := json.Marshal(body)
	writeJSON(w, status, raw)
}

// 幂等键的三条 409。状态码相同而处置不同，所以**码**才是契约。
//
// 三条的措辞都要说清「接下来该做什么」：拿到 409 的调用方多半正在重试，
// 而不知道该换键还是该等一等的重试只会再撞一次。
const (
	// CodeIdempotencyKeyReused：同一个键配上了一个不同的请求体。
	// 换一个新的键重发即可。
	CodeIdempotencyKeyReused = "idempotency_key_reused"
	msgIdempotencyKeyReused  = "this Idempotency-Key was already used with a different request body; " +
		"send a different request with a new Idempotency-Key"

	// CodeIdempotencyInProgress：同键的第一次请求还在跑，等待超过了这次请求
	// 自己的超时。稍后重试同一个键即可。
	CodeIdempotencyInProgress = "idempotency_in_progress"
	msgIdempotencyInProgress  = "another request with this Idempotency-Key is still in progress; retry later"

	// CodeIdempotencyIncomplete：同键的第一次请求没走完（上游失败、客户端
	// 中途断开、进程没了）。它不会被重放，用同一个键再发也永远不会成功——
	// 所以要换一个新的键。
	CodeIdempotencyIncomplete = "idempotency_incomplete"
	msgIdempotencyIncomplete  = "the first request with this Idempotency-Key did not complete; " +
		"retry with a new Idempotency-Key"
)

// writeIdempotencyError 写出一条幂等键的 409。
func writeIdempotencyError(w http.ResponseWriter, protocol channel.Protocol, code, message string) {
	writeErrorCode(w, protocol, http.StatusConflict, code, message)
}

// CodeInsufficientQuota 是「这个账户现在没法再花这笔钱」的错误码。
//
// 它有两个来源，两者给出**同一个**状态码与码：
//
//   - 这把 API Key 的累计花费用完了它的额度上限（见 writeInsufficientQuota）；
//   - 用户钱包的可用余额不够这次预留（见 writeInsufficientBalance）。
//
// 共用一套码是刻意的：两者都是**账户状态**而不是服务故障，调用方只该写
// 一个分支。它们的差别在文案，因为该做的事不一样（改 key 的额度 / 充值）。
//
// 它与限流共用 429，但**不是同一件事**，所以码必须分开给：
//
//   - 限流是「这一秒打得太快」——窗口会过去，等一会儿重发就对了；
//   - 额度或余额是「现在花不了」——等多久都不会变。
//
// 状态码相同而处置相反，正是 writeErrorCode 存在的理由（见它的注释）：
// 只给状态码的话，SDK 只能靠读 message 来分辨，而 SDK 不做那件事。
const CodeInsufficientQuota = "insufficient_quota"

// MsgInsufficientQuota 是「这把 API Key 的累计额度用尽」时那句 message。
//
// 它与 MsgInsufficientBalance 共用状态码与码，只有文案不同：两者的处置相反
// （改 key 的额度 / 充值），而码分不开这两个来源，能分开的是这里的文案
// （见 CodeInsufficientQuota 的注释）。导出是为了让别的包（例如 larkaxctl
// doctor 的归因）能逐字钉住它，而不是各抄一份、迟早分叉。
const MsgInsufficientQuota = "this API key has reached its spending limit; " +
	"raise the limit on this key or use a different key"

// MsgInsufficientBalance 是钱包可用余额不够这次预留时的那句话。
//
// 它与 MsgInsufficientQuota 共用状态码与码，但**不能共用文案**：那句说的是
// 「改这把 key 的额度，或者换一把 key」，而余额不足时这两件事都不解决问题
// ——要做的只有充值。码是给 SDK 分支用的，文案是给人读的。
const MsgInsufficientBalance = "the account balance is insufficient for this request; " +
	"top up the wallet before retrying"

// writeInsufficientQuota 写出一次「额度用尽」的响应。
//
// # 为什么不回 Retry-After
//
// 这与 writeRateLimited 的差别是有意的，不是漏了。那个头的意思是「等这么久
// 再来」，而额度**不会自己回来**：回一个等待时间等于让客户端在一个不会变的
// 状态上反复重试，还给了它一个「照做就行」的理由。额度要变只能靠人（改额度
// 或换 key），所以这里没有可等的时长可给——不给头，客户端就没有可照做的
// 退避，只能去看码。
//
// # 错误体
//
// 仍然按调用协议给形状，码由 writeErrorCode 放到各自那个机器可读的位置：
// OpenAI 在 error.code，Anthropic 在 error.type（理由见 writeErrorCode）。
// 状态码取 429 而不是 402：402 在各家 SDK 里普遍没有对应的处理分支，而 429
// 是「现在别发了」这个语义的通用落点，两套协议都认。
func writeInsufficientQuota(w http.ResponseWriter, protocol channel.Protocol) {
	writeErrorCode(w, protocol, http.StatusTooManyRequests, CodeInsufficientQuota, MsgInsufficientQuota)
}

// writeInsufficientBalance 写出一次「钱包可用余额不足」的响应。
//
// 它与 writeInsufficientQuota 同形（429 + insufficient_quota、没有
// Retry-After），只有 message 不同——理由见 MsgInsufficientBalance。
//
// 不给 Retry-After 的理由与额度那一条逐字相同：余额不会因为等一会儿就回来，
// 回一个等待时间等于让客户端在一个不会变的状态上反复重试。充值之后即刻生效，
// 那是「有人做了事」而不是「等到某个时刻」。
func writeInsufficientBalance(w http.ResponseWriter, protocol channel.Protocol) {
	writeErrorCode(w, protocol, http.StatusTooManyRequests, CodeInsufficientQuota, MsgInsufficientBalance)
}

// HeaderRetryAfter 是限流响应里回给调用方的重试间隔头。
//
// 它与状态码 429 是一对：只回 429 而让调用方自己去猜该等多久，多数 SDK
// 会退化成「立刻重试」，那正好是限流要避免的行为。
const HeaderRetryAfter = "Retry-After"

// writeRateLimited 写出一次限流的响应。
//
// Retry-After 用整数秒：RFC 9110 允许 delay-seconds 与 HTTP-date 两种写法，
// 整数秒是各家 SDK 都直接解析的那种。至少 1 秒——写 0 会让客户端立刻重试，
// 而它必然还在同一个窗口里，白白多一次往返。
//
// 错误体仍然按调用协议给形状（429 → rate_limit_error，见 errorKind），
// 所以这一条不需要各协议各写一份。
func writeRateLimited(w http.ResponseWriter, protocol channel.Protocol, retryAfter time.Duration) {
	w.Header().Set(HeaderRetryAfter, strconv.FormatInt(retryAfterSeconds(retryAfter), 10))
	writeError(w, protocol, http.StatusTooManyRequests,
		"rate limit exceeded for this API key, retry later")
}

// retryAfterSeconds 把重试间隔换成整数秒，向上取整并至少 1 秒。
//
// 取整方向与下限都在这里定：实现给的是 duration，而响应头只能写整数秒，
// 这个换算放在一处，免得每个调用点各取整一次、方向还不同。
func retryAfterSeconds(d time.Duration) int64 {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		return 1
	}
	return secs
}

// errorKind 把状态码映射成调用协议里的错误类型。
//
// 两家用的词不一样，而调用方常常按类型分支（例如「凭证错了就别重试」），
// 所以这不是文案问题，是契约的一部分。
//
// 只列本入口真的会返回的状态码。多列的那些没有测试覆盖，而它们一旦与
// 真实语义不符，会静静地误导调用方——一份「看起来很全」的映射表比
// 一份「只覆盖实际用到」的表更容易出错。
//
// 唯一的例外是 409（见下面那条注释）：它现在走不到，但去掉它会让一条
// **将来**不带码的 409 落到 api_error 上，那是错的。
func errorKind(protocol channel.Protocol, status int) (kind, code string) {
	if protocol == channel.ProtocolAnthropic {
		switch status {
		case http.StatusBadRequest, http.StatusMethodNotAllowed,
			http.StatusUnsupportedMediaType, http.StatusConflict:
			// 409 这一支当前走不到：幂等那三条 409 都带码，而 Anthropic 的
			// 错误体只有 error.type 一个机器可读的位置，writeErrorCode 会把码
			// 写在那里（typ = code），轮不到这里。留着它是因为映射表要保证的
			// 是「任何走到这里的 409 都映射成 invalid_request_error」——删掉
			// 它，一条不带码的 409 就会落到 api_error 上，而 409 是 4xx。
			return "invalid_request_error", ""
		case http.StatusUnauthorized:
			return "authentication_error", ""
		case http.StatusRequestEntityTooLarge:
			return "request_too_large", ""
		case http.StatusTooManyRequests:
			return "rate_limit_error", ""
		case http.StatusNotImplemented:
			return "not_implemented", ""
		default:
			return "api_error", ""
		}
	}

	switch status {
	case http.StatusBadRequest, http.StatusMethodNotAllowed,
		http.StatusUnsupportedMediaType, http.StatusConflict:
		return "invalid_request_error", ""
	case http.StatusUnauthorized:
		// OpenAI 把凭证问题归在 invalid_request_error 下，靠 code 区分。
		return "invalid_request_error", "invalid_api_key"
	case http.StatusRequestEntityTooLarge:
		return "invalid_request_error", "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error", ""
	case http.StatusNotImplemented:
		return "not_implemented", ""
	default:
		return "api_error", ""
	}
}
