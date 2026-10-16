package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

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
func checkRequest(req *channel.Request) *rejection {
	if req.Stream {
		// 流式还没实现，而**不能**就这样往下走：非流式那条路会照常调用
		// 上游（适配器的非流式入口不读 Stream），于是调用方拿到一个
		// 普通的 JSON 响应，而它按 SSE 去解——失败在客户端表现为
		// 「解析不了响应」，与服务端真正的原因隔了一层。
		return &rejection{
			status:  http.StatusNotImplemented,
			message: "streaming responses are not implemented yet, retry with \"stream\": false",
		}
	}
	if req.Model == "" {
		return &rejection{status: http.StatusBadRequest, message: "model is required"}
	}
	if len(req.Messages) == 0 {
		return &rejection{status: http.StatusBadRequest, message: "messages is required"}
	}
	return nil
}

// billingRequest 组装计费侧要的那份请求。
//
// 计费要知道的与调上游要知道的是**两个请求**（见 pipeline.Request 的注释
// 与 Chatter 的签名），这里只放计费要的，上游要的留在 upstream 里。
func billingRequest(user User, upstream *channel.Request, requestID string) pipeline.Request {
	return pipeline.Request{
		RequestID: requestID,
		UserID:    user.ID,
		// CanonicalModel 与上游模型名在这里取同一个值，因为规范名到上游名的
		// 映射还没做（那属于选路层）。这是**已知的临时状态**，不是设计：
		// 接通映射时改的正是这一行。
		CanonicalModel: upstream.Model,
		MaxTokens:      int64(upstream.MaxTokens),
		InputTokens:    inputTokensOf(upstream),
	}
}

// inputTokensOf 估算请求的输入 token 数。
//
// 预留发生在调用上游之前，那时拿不到实测值，只能用估算（见 internal/tokens）。
// 只估消息里的文本：工具定义与图片的 token 消耗各家口径不同，硬估出来的
// 数字比文本部分的估算更不可信。估算偏低会让预留偏小，而结算是按预留的
// 金额封顶的（见 pipeline.Reserve），所以这个取舍的代价是**少收**，
// 不是超收——宁可少收也不要在用户余额上多冻一笔。
func inputTokensOf(req *channel.Request) int64 {
	var text strings.Builder
	for _, m := range req.Messages {
		for _, b := range m.Content {
			text.WriteString(b.Text)
		}
	}
	return tokens.EstimatedTokens(req.Model, text.String())
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
