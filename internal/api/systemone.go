package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/systemone"
)

// PathSystemOne 是决策表面的入口路径。
//
// 与 /v1/chat/completions、/v1/messages 并列：**协议由路径决定**，不从请求体
// 猜（见包注释）。这条表面与那两条没有共同形状——请求是 state 加一组带类型的
// 判定问题，响应是概率而不是文本——所以它是第三条路径，不是某条路径的变体。
const PathSystemOne = "/v1/systemone"

// systemoneErrorProtocol 是这条表面错误体用的形状。
//
// 它自己没有错误体形状（上游也没有给），所以取两套里更通用的那一套——
// 与「未知路径」那一支同一个选择（见 ServeHTTP 的 default 分支）。
// 这与「协议由路径决定」不冲突：那一条说的是**成功响应**的形状，
// 而这里没有第三套可选。
const systemoneErrorProtocol = channel.ProtocolOpenAI

// Decider 是调上游决策接口的能力。
//
// 与 Chatter 并列，理由相同：本包只关心「给它一个计费请求与一份决策请求，
// 它回一个结果」，*gateway.Gateway 天然满足它。两条能力分开而不是合成一个
// 接口，是因为它们的**签名不同**（一个收 *channel.Request，一个收
// *systemone.Request），合并只能靠类型断言，而那会把编译期的事推到运行期。
type Decider interface {
	Decide(ctx context.Context, req pipeline.Request, decision *systemone.Request) (*gateway.DecideResult, error)
}

// serveDecision 处理一次决策请求。
//
// # 顺序与 chat 那条路逐字相同
//
// 解析之前的检查全在 admit 里（方法、Content-Type、装配、鉴权、限流、额度、
// 读体），三条路径共用同一份。这一条排在最后，是**有意的**：超限或没通过
// 鉴权的请求不该让我们为它读进最多 maxRequestBodyBytes 的内存，也不该走到
// 预留——那会先把余额冻住，再因为额度把它拒掉。
//
// # 这条路径没有的东西
//
//   - **没有幂等**：Idempotency-Key 不生效。指纹要算在解析后的请求上
//     （见 idempotency.HashRequest），而它算的是 *channel.Request——这条表面
//     不共用那个类型。带这个头发过来的调用方会得到一个正常执行的请求，
//     而不是重放。这一条写在 docs/development.md 的这条表面那一节里。
//   - **没有插件**：见 gateway.Decide 的注释。
//   - **没有请求快照**：见 gateway.Decide 的注释。
func (h *Handler) serveDecision(w http.ResponseWriter, r *http.Request) {
	user, body, ok := h.admit(w, r, systemoneErrorProtocol, h.Auth != nil && h.Decider != nil)
	if !ok {
		return
	}

	decision, err := systemone.Parse(body)
	if err != nil {
		// 与 chat 那条路同一个处置：解析失败的细节不回给调用方，那里面会带上
		// 内部类型名与字段名。
		observe.FromContext(r.Context()).Warn("解析决策请求失败", "err", err)
		writeError(w, systemoneErrorProtocol, http.StatusBadRequest, "invalid request body")
		return
	}

	// 流式**先判**：它不是「请求写错了」，而是这条表面没有那个能力。
	// 排在字段校验之前，是因为这一条无论请求体写得对不对都成立。
	//
	// 处置是拒掉而不是忽略。上游收到 stream 会当没看见、照常回普通 JSON，
	// 所以忽略它的结果是：调用方按 SSE 去解析一份 JSON，错误出现在他那一侧
	// 而且离原因很远。回 400 让他当场知道要改请求。
	if decision.Stream {
		writeError(w, systemoneErrorProtocol, http.StatusBadRequest, systemone.ErrStreamUnsupported.Error())
		return
	}

	if rejected := decision.Validate(); rejected != nil {
		// 校验失败的措辞是**给调用方看的**（他改请求就能过），所以原样回出去。
		// 与解析失败不同：那一类会带上内部类型名，这一类不会。
		writeError(w, systemoneErrorProtocol, http.StatusBadRequest, rejected.Error())
		return
	}

	requestID := requestIDOf(r)
	req := h.decideBillingRequest(user, decision, requestID)

	res, err := h.Decider.Decide(r.Context(), req, decision)
	if err != nil {
		if r.Context().Err() != nil {
			// 调用方已经断开。与 chat 那条路同一个处置：没有人在听，
			// 写响应只是白费一次序列化。
			observe.FromContext(r.Context()).Warn("调用方在响应前断开", "err", err)
			return
		}
		h.failUpstream(w, r, systemoneErrorProtocol, err)
		return
	}
	if res == nil || res.Response == nil {
		// 拿到没有响应体的结果只可能是装配错误（Decider 的实现在成功时
		// 必须给一份响应），不是调用方的错。
		observe.FromContext(r.Context()).Error("决策入口收到没有响应体的结果")
		writeError(w, systemoneErrorProtocol, http.StatusInternalServerError, "internal error")
		return
	}

	rendered, err := renderDecision(decision.Model, res.Response)
	if err != nil {
		observe.FromContext(r.Context()).Error("渲染决策响应失败", "err", err)
		writeError(w, systemoneErrorProtocol, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, rendered)
}

// decideBillingRequest 组装决策请求的计费输入。
//
// 与 billingRequest 同一个做法：这里只放计费要的，上游要的留在 decision 里。
//
// # 上界为什么是那两个常量
//
// 这条表面的请求里**没有** max_tokens 一类的参数，所以预留的上界只能来自
// internal/systemone 里那两个常量：
//
//   - 输入侧取 MaxInputTokens，那是**上游自己的预算**（state 加全部问题 64K
//     token）。它取代了「按正文估一个数」——估出来的是下界，而上界必须真的
//     是上界：拿一个偏小的数去预留，结算会撞上界被截断（Capped），差额由
//     平台吃掉，且账目仍然平，事后完全看不出来。
//   - 输出侧取 MaxOutputTokens。上游文档没给这个数，4096 是保守估计
//     （见那个常量的注释）。
func (h *Handler) decideBillingRequest(user User, decision *systemone.Request, requestID string) pipeline.Request {
	return pipeline.Request{
		RequestID: requestID,
		UserID:    user.ID,
		APIKeyID:  user.KeyID,
		Asset:     h.DefaultAsset,

		// CanonicalModel 是**调用方要的那个**名字。上游名由选路层按选中的
		// 渠道翻出来，与 chat 那条路逐字相同。
		CanonicalModel: decision.Model,
		InputTokens:    systemone.MaxInputTokens,
		MaxTokens:      systemone.MaxOutputTokens,

		// 这条表面从不流式：要了 stream 的请求在更前面就被拒了，所以这里
		// 是一个恒为 false 的字面量，而不是从请求里读出来的。
		Streamed: false,
		// 协议由路径决定，而这条路径就是决策那条。
		Protocol: channel.ProtocolSystemOne,
	}
}

// renderDecision 把决策响应渲染成回给调用方的 JSON。
//
// 报给调用方的模型名是**他要的那个**，不是上游回报的那个：上游名是我们与
// 上游之间的事（见 internal/gateway 的 DecideResult.UpstreamModel，它记的是
// 实际用了哪个），而调用方按自己请求里的名字理解响应。与 chat 那条路
// （renderSuccess）同一个取舍。
func renderDecision(model string, resp *systemone.Response) ([]byte, error) {
	if model == "" {
		return nil, errors.New("缺少要回给调用方的模型名")
	}
	out := *resp
	out.Model = model
	return systemone.EncodeResponse(&out)
}
