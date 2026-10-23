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
		// 字段级的问题（criteria 的形状与类型对不上）**原样回给调用方**：
		// systemone.Invalid 的 Field/Message 就是给他看的，他改请求就能过。
		// 它与 Validate 那一支是同一类，所以共用同一个出口。
		//
		// 压成一句「invalid request body」的话，我们精心构造的那句
		// 「questions.q.criteria: ...」只进日志，而调用方拿到的是一句没有
		// 指向的概括——那条只在测试里可见的错误信息，等于不存在。
		var invalid *systemone.Invalid
		if errors.As(err, &invalid) {
			rejectDecision(w, invalid)
			return
		}
		// 其余是 JSON 层面的错误（语法错误，或类型对不上——例如 model 给了
		// 一个数组）：细节里会带上内部类型名与字段名，不回给调用方
		// （与 chat 那条路同一个处置）。
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
		rejectDecision(w, rejected)
		return
	}

	// 入站别名在**这里**解析一次，之后选路、计费与回显各取自己那一侧的名字。
	//
	// # 位置的两侧约束
	//
	//   - **不能更早**：Validate 与 rejectDecision 报的是**请求本身**的问题
	//     （字段名指向调用方写的那个字段，回的是 400），而解析失败回的是 500。
	//     解析排在前面时，一个 model 为空的请求会先撞上「模型名为空」拿到 500，
	//     而不是「model is required」拿到 400——调用方于是以为是我们坏了。
	//     另外，字段都不全的请求也不值得为它多一次查询。
	//   - **不能更晚**：decideBillingRequest 与 Decider.Decide 都要规范名。
	//
	// 这条表面没有幂等（见本函数注释），所以不存在 chat 那条路上「解析必须
	// 排在幂等指纹之前」的约束——这里没有指纹。
	//
	// requested 是不可变的值：decision.Model 在下面被改成规范名，而回显要的
	// 是调用方原来写的那个。
	requested := decision.Model
	if h.Inbound != nil {
		canonical, err := h.Inbound.ResolveInbound(r.Context(), requested)
		if err != nil {
			// 与 chat 那条同一个处置：**不吞成原样放行**。那会把别名当成一个
			// 不存在的模型去选路，症状是「查无此模型」，排查方向整个指向另一边。
			observe.FromContext(r.Context()).Error("解析入站别名失败", "err", err)
			writeError(w, systemoneErrorProtocol, http.StatusInternalServerError, "internal error")
			return
		}
		decision.Model = canonical
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

	// 回显调用方写的名字，不是 decision.Model（那已经是规范名了）。
	rendered, err := renderDecision(requested, res.Response)
	if err != nil {
		observe.FromContext(r.Context()).Error("渲染决策响应失败", "err", err)
		writeError(w, systemoneErrorProtocol, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, rendered)
}

// rejectDecision 回一句**给调用方看的** 400。
//
// Parse 与 Validate 两条支路共用它：两条报的都是「请求本身不合法」，措辞
// 都是给调用方改请求用的（Field 指向线格式里的字段名），所以出口只有一个。
// 与「body 不是合法 JSON」那一类分开：后者的细节带内部类型名与字段名，
// 只进日志。
func rejectDecision(w http.ResponseWriter, err error) {
	writeError(w, systemoneErrorProtocol, http.StatusBadRequest, err.Error())
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
//     平台吃掉，且账目仍然平——事后只有按协议分的 capped 计数器能看见。
//   - 输出侧取 MaxOutputTokens。上游文档没给这个数，4096 是保守估计
//     （见那个常量的注释）。
func (h *Handler) decideBillingRequest(user User, decision *systemone.Request, requestID string) pipeline.Request {
	return pipeline.Request{
		RequestID: requestID,
		UserID:    user.ID,
		APIKeyID:  user.KeyID,
		Asset:     h.DefaultAsset,

		// CanonicalModel 是**入站解析之后的规范名**：别名已经翻成它指向的
		// 那个模型。上游名由选路层按选中的渠道翻出来，与 chat 那条路逐字
		// 相同。它与响应体里回显的名字是两件事——那里回显的是调用方写的
		// 原名，两者只在请求用了入站别名时不同（见 renderDecision）。
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
// 报给调用方的模型名是**他要的那个**（入站解析之前的原名），不是上游回报的
// 那个，也不是解析之后的规范名：上游名是我们与上游之间的事（见
// internal/gateway 的 DecideResult.UpstreamModel，它记的是实际用了哪个），
// 而规范名是我们内部的名字。调用方按自己请求里的名字理解响应。与 chat 那条
// 路（renderSuccess）同一个取舍。
func renderDecision(model string, resp *systemone.Response) ([]byte, error) {
	if model == "" {
		return nil, errors.New("缺少要回给调用方的模型名")
	}
	out := *resp
	out.Model = model
	return systemone.EncodeResponse(&out)
}
