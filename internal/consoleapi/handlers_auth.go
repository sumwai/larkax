package consoleapi

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sumwai/larkax/internal/auth"
)

// 本文件是登录、登出与会话查询。

// loginRequest 是登录请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin 校验口令并建立会话。
//
// # 为什么几条失败的处置顺序不能换
//
//  1. 用户不存在与口令不对都给 401、都跑一次 KDF。任何一个分支提前返回，
//     响应时间或文案就成了一张用户名清单。
//  2. 锁定检查在验证之前：设计明说锁定期间不验证口令，于是「再试一次也许
//     就对了」这句话不再是暗示——它根本没有被验证过。
//  3. 账号状态（suspended 等）在口令**正确之后**才说。反过来做等于告诉
//     一个不知道口令的人「这个账号是存在的、而且被停了」。
//
// 没有任何一条分支把用户名、口令或哈希写进日志。
func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.transportSecure(r) {
		a.fail(w, r, http.StatusBadRequest, codeInsecureTransport,
			"Sign-in requires HTTPS.")
		return
	}
	// 登录也要过同源关：没有 CSRF 令牌的请求在这里没有可比的令牌，但一个
	// 跨站表单可以把受害者登进**攻击者自己的账号**，此后受害者的操作都
	// 落在那个账号上。与写方法用同一条判据（两个头都缺省时放行）。
	if !a.sameOrigin(r) {
		a.fail(w, r, http.StatusForbidden, codeCSRFFailed,
			"The sign-in request must come from this site.")
		return
	}

	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		a.invalid(w, r, err)
		return
	}
	if req.Username == "" || req.Password == "" {
		a.invalid(w, r, errors.New("username 与 password 都必须给出"))
		return
	}

	now := a.now().UTC()
	ip := a.remoteIP(r)
	if wait, ok := a.loginLimiter.allow(ip, now); !ok {
		a.retryAfter(w, r, wait)
		return
	}

	user, err := a.deps.accounts.LoginUserByName(r.Context(), req.Username)
	if errors.Is(err, auth.ErrUserNotFound) {
		// 不存在的用户也跑一次哈希：否则这条分支的耗时与真实用户差着
		// 几万轮 PBKDF2，而那个时间差就是一份可枚举的用户名清单。
		a.verifyDummy(req.Password)
		a.fail(w, r, http.StatusUnauthorized, codeInvalidCredential,
			"Invalid username or password.")
		return
	}
	if err != nil {
		a.unavailable(w, r, err, "查询登录用户失败")
		return
	}

	if user.PasswordHash == "" {
		// 运维还没给这个账号设口令。它不是 401：401 会让人以为自己敲错了，
		// 然后一遍遍重试，直到把自己锁上。
		a.verifyDummy(req.Password)
		a.fail(w, r, http.StatusForbidden, codeAccountNotReady,
			"This account has not been activated yet. Contact the administrator.")
		return
	}

	if remaining := user.LockRemaining(now); remaining > 0 {
		a.retryAfter(w, r, remaining)
		return
	}

	verified, err := auth.VerifyPassword(a.kdf, user.PasswordHash, req.Password)
	if err != nil {
		// 库里的哈希串坏了。它不是「口令不对」——报 401 会让用户一直重试，
		// 而真正能修它的人在看日志。
		a.internal(w, r, err, "校验口令失败")
		return
	}
	if !verified.OK {
		if _, _, recordErr := a.deps.accounts.RecordLoginFailure(r.Context(), user.ID, now); recordErr != nil {
			// 记录失败不该把「口令不对」变成 500：用户拿到的判据仍然是
			// 401，只是这条路径上的锁定计数少了这一次。错误进日志。
			a.log.Error("记录登录失败失败", "error", recordErr,
				"request_id", string(requestID(r)))
		}
		a.fail(w, r, http.StatusUnauthorized, codeInvalidCredential,
			"Invalid username or password.")
		return
	}

	if user.Status != auth.UserStatusActive {
		a.fail(w, r, http.StatusForbidden, codeAccountDisabled,
			"This account is not active.")
		return
	}

	if clearErr := a.deps.accounts.ClearLoginFailures(r.Context(), user.ID); clearErr != nil {
		a.log.Warn("清除登录失败计数失败", "error", clearErr,
			"request_id", string(requestID(r)))
	}
	if verified.NeedsRehash {
		// 渐进重哈希。它不改会话、不清计数（见 auth.RehashPassword）；
		// 失败也不影响这次登录——旧参数仍然能验证。
		if hash, hashErr := auth.HashPassword(a.kdf, req.Password); hashErr != nil {
			a.log.Warn("重算口令哈希失败", "error", hashErr)
		} else if rehashErr := a.deps.accounts.RehashPassword(r.Context(), user.ID, hash); rehashErr != nil {
			a.log.Warn("写回口令哈希失败", "error", rehashErr)
		}
	}

	session, token, err := a.deps.sessions.CreateSession(r.Context(), user.ID,
		truncateUserAgent(r.UserAgent()), a.remoteIP(r))
	if err != nil {
		a.unavailable(w, r, err, "建立会话失败")
		return
	}

	a.loginLimiter.reset(ip)
	a.setSessionCookie(w, token)

	// 这里不再查一次用户：LoginUser 里已经有 id / username / status，
	// 但缺 display_name / email / created_at。补一次读是最省事的做法，
	// 而它只发生在登录成功的路径上（一天几次），不在每个请求上。
	resp, err := a.newSessionResponse(r.Context(), session)
	if err != nil {
		a.unavailable(w, r, err, "读取用户资料失败")
		return
	}
	a.ok(w, http.StatusOK, resp)
}

// verifyDummy 用固定假哈希跑一次验证，并丢弃结果。
//
// 存在的唯一目的是让「用户不存在」与「口令不对」耗时相当。
func (a *API) verifyDummy(password string) {
	if _, err := auth.VerifyPassword(a.kdf, a.dummyHash, password); err != nil {
		a.log.Error("假哈希校验失败", "error", err)
	}
}

// handleLogout 吊销当前会话。
//
// 登出是幂等的：没有 Cookie、会话已经过期或已被吊销，一律 204 并清 Cookie。
// 只有一件事会让它失败——CSRF 不匹配；那时请求看起来像是别处发来的，
// 而「帮别人登出」虽然只是个小麻烦，仍不该由跨站页面决定。
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		a.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	session, err := a.deps.sessions.SessionByToken(r.Context(), cookie.Value)
	if errors.Is(err, auth.ErrSessionNotFound) {
		a.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		a.unavailable(w, r, err, "查询会话失败")
		return
	}

	if !a.csrfValid(r, session) {
		a.fail(w, r, http.StatusForbidden, codeCSRFFailed,
			"The CSRF token is missing or does not match.")
		return
	}

	if session.RevokedAt == nil && a.now().UTC().Before(session.ExpiresAt) {
		if err := a.deps.sessions.RevokeSession(r.Context(), session.ID); err != nil {
			a.unavailable(w, r, err, "吊销会话失败")
			return
		}
	}

	a.clearSessionCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleSession 返回当前会话与 CSRF 令牌。
//
// 它不刷新 CSRF 令牌（每次返回同一份）：并发请求里有一半拿着旧令牌的话，
// 下一次写操作就会莫名其妙地 403。
func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	session, ok := a.currentSession(w, r)
	if !ok {
		return
	}
	resp, err := a.newSessionResponse(r.Context(), session)
	if err != nil {
		a.unavailable(w, r, err, "读取用户资料失败")
		return
	}
	a.ok(w, http.StatusOK, resp)
}

// truncateUserAgent 把 User-Agent 截断到一个合理的长度。
//
// 这一列只用于排查「这个会话是从哪个浏览器来的」，而客户端完全可以送来
// 一段几 KB 的串。截断而不是拒绝：User-Agent 不该成为登录失败的理由。
//
// 按**完整字符**截：直接切字节会让多字节字符剩下半截，库里就存进一段
// 无效 UTF-8，读出来（JSON 序列化）会变成替换字符或直接报错。上限仍是
// 512 字节，所以放得下的字符个数取决于它们各占几个字节。
func truncateUserAgent(ua string) string {
	const maxLen = 512
	if len(ua) <= maxLen && utf8.ValidString(ua) {
		return ua
	}

	var out strings.Builder
	out.Grow(min(len(ua), maxLen))
	for _, r := range ua {
		size := utf8.RuneLen(r)
		if out.Len()+size > maxLen {
			break
		}
		out.WriteRune(r)
	}
	return out.String()
}
