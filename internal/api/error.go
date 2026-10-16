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

// writeError 按调用协议写出一个错误响应。
//
// message 是**给调用方看的**：不回上游的错误原文、不回内部路径与标识。
// 上游的错误体里可能回显请求内容，而调用方不需要它也知道该改什么；
// 需要细节时用响应头里的请求标识去查日志。
func writeError(w http.ResponseWriter, protocol channel.Protocol, status int, message string) {
	kind, code := errorKind(protocol, status)

	var body any
	if protocol == channel.ProtocolAnthropic {
		body = claudeErrorBody{
			Type:  "error",
			Error: claudeError{Type: kind, Message: message},
		}
	} else {
		out := openAIError{Message: message, Type: kind}
		if code != "" {
			out.Code = &code
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
func errorKind(protocol channel.Protocol, status int) (kind, code string) {
	if protocol == channel.ProtocolAnthropic {
		switch status {
		case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType:
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
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType:
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
