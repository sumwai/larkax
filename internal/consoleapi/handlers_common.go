package consoleapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/observe"
)

// 本文件是几处共用的响应与读法：用户对象、内部错误的落日志方式。

// userJSON 是响应里的用户对象。
//
// 它刻意不含 role / is_merchant / is_admin：控制台是给客户看的，角色与
// 商家标记属于内部拓扑。今天用户的角色全是 user，多放一个字段只会让人
// 以为它有意义。
type userJSON struct {
	ID          int64   `json:"id"`
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
}

// newUserJSON 把 auth.UserInfo 折成响应形状。
//
// 空串折成 null：库里的 NULL 与空串在这一层已经分不开（UserInfo 的注释
// 说明了这一点），而响应里两者都表示「没填」。
func newUserJSON(info auth.UserInfo) userJSON {
	out := userJSON{
		ID:        info.ID,
		Username:  info.Username,
		Status:    info.Status,
		CreatedAt: formatTime(info.CreatedAt),
	}
	if info.DisplayName != "" {
		name := info.DisplayName
		out.DisplayName = &name
	}
	if info.Email != "" {
		email := info.Email
		out.Email = &email
	}
	return out
}

// sessionResponse 是登录与 `GET /auth/session` 共用的响应体。
type sessionResponse struct {
	User      userJSON `json:"user"`
	CSRFToken string   `json:"csrf_token"`
	ExpiresAt string   `json:"expires_at"`
}

// loadUser 读一个用户的展示字段。
func (a *API) loadUser(ctx context.Context, userID int64) (auth.UserInfo, error) {
	return a.deps.accounts.UserByRef(ctx, strconv.FormatInt(userID, 10))
}

// newSessionResponse 组装登录成功之后的响应。
func (a *API) newSessionResponse(ctx context.Context, session auth.Session) (sessionResponse, error) {
	info, err := a.loadUser(ctx, session.UserID)
	if err != nil {
		return sessionResponse{}, err
	}
	return sessionResponse{
		User:      newUserJSON(info),
		CSRFToken: session.CSRFToken,
		ExpiresAt: formatTime(session.ExpiresAt),
	}, nil
}

// requestID 取这次请求的关联标识；没有时生成一个。
//
// 正常路径上它一定在（observe.Middleware 是最外层）；补一个生成是为了让
// 「日志里有一行没有 id」不会发生——而那种行在关联时等于不存在。
func requestID(r *http.Request) observe.RequestID {
	if id := observe.RequestIDFromContext(r.Context()); id != "" {
		return id
	}
	return observe.NewRequestID()
}

// internal 记一条内部错误并回 500。
//
// 真正的错误只进日志：它可能带连接串、主机名、SQL 片段，而这些都不该
// 出现在响应里。响应里是固定一句话（见设计 3.2）。
func (a *API) internal(w http.ResponseWriter, r *http.Request, err error, what string) {
	a.log.Error(what, "error", err, "request_id", string(requestID(r)))
	a.fail(w, r, http.StatusInternalServerError, codeInternal, "Internal server error.")
}

// unavailable 记一条依赖故障并回 503。
//
// 与 internal 分开：503 说的是「这次不是你的错，也不是我这边的代码错了」，
// 前端的处置是稍后再试；500 说的是「去看服务端日志」。
func (a *API) unavailable(w http.ResponseWriter, r *http.Request, err error, what string) {
	a.log.Error(what, "error", err, "request_id", string(requestID(r)))
	a.fail(w, r, http.StatusServiceUnavailable, codeUnavailable,
		"The console is temporarily unavailable.")
}

// invalid 回一个参数错误。
//
// 请求体超限是这一层唯一一个不该回 400 的「参数错」：413 告诉客户端该把
// 体积减下来，而 400 会让它照着字段去改。判据来自 http.MaxBytesReader
// 给出的错误类型，不是靠比较文案。
func (a *API) invalid(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		a.fail(w, r, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
			"The request body is too large.")
		return
	}
	a.fail(w, r, http.StatusBadRequest, codeInvalidRequest, err.Error())
}

// notFound 回 404。归属不符与不存在共用这一条（见设计 4.3）。
func (a *API) notFound(w http.ResponseWriter, r *http.Request) {
	a.fail(w, r, http.StatusNotFound, codeNotFound, "Not found.")
}

// conflict 回 409。
func (a *API) conflict(w http.ResponseWriter, r *http.Request, message string) {
	a.fail(w, r, http.StatusConflict, codeConflict, message)
}

// retryAfter 回 429 并带上建议的重试间隔。
func (a *API) retryAfter(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	if wait < time.Second {
		wait = time.Second
	}
	w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
	a.fail(w, r, http.StatusTooManyRequests, codeRateLimited,
		"Too many attempts. Try again later.")
}
