package consoleapi

import (
	"errors"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/sumwai/larkax/internal/auth"
)

// 本文件是 `/me`：读资料、改资料、改口令。

// handleMe 返回当前用户。
func (a *API) handleMe(w http.ResponseWriter, r *http.Request, session auth.Session) {
	info, err := a.loadUser(r.Context(), session.UserID)
	if errors.Is(err, auth.ErrUserNotFound) {
		a.fail(w, r, http.StatusUnauthorized, codeUnauthenticated, "Sign in to continue.")
		return
	}
	if err != nil {
		a.unavailable(w, r, err, "读取用户资料失败")
		return
	}
	a.ok(w, http.StatusOK, map[string]userJSON{"user": newUserJSON(info)})
}

// patchMeRequest 是改资料请求体。
//
// 两个字段都是指针套 optionalString：缺省不动、null 清空、给值设成它。
// `username` 不在结构体里，于是 `DisallowUnknownFields` 会让它报错——
// 这正是想要的：这个字段不可改（见设计 4.2）。
type patchMeRequest struct {
	DisplayName optionalString `json:"display_name"`
	Email       optionalString `json:"email"`
}

// 显示名的长度上限。下限是 1：空串不是「清空」的说法，`null` 才是。
const displayNameMaxRunes = 64

// handlePatchMe 改资料。
func (a *API) handlePatchMe(w http.ResponseWriter, r *http.Request, session auth.Session) {
	var req patchMeRequest
	if err := decodeJSON(r, &req); err != nil {
		a.invalid(w, r, err)
		return
	}
	if !req.DisplayName.Set && !req.Email.Set {
		a.invalid(w, r, errors.New("没有要改的字段"))
		return
	}

	var params auth.UpdateUserParams
	if req.DisplayName.Set {
		value := ""
		if req.DisplayName.Value != nil {
			value = *req.DisplayName.Value
			if n := utf8.RuneCountInString(value); n < 1 || n > displayNameMaxRunes {
				a.invalid(w, r, errors.New("display_name 必须是 1..64 个字符"))
				return
			}
		}
		params.DisplayName = &value
	}
	if req.Email.Set {
		value := ""
		if req.Email.Value != nil {
			value = *req.Email.Value
		}
		params.Email = &value
	}

	info, err := a.deps.accounts.UpdateUser(r.Context(),
		strconv.FormatInt(session.UserID, 10), params)
	if errors.Is(err, auth.ErrUserNotFound) {
		a.fail(w, r, http.StatusUnauthorized, codeUnauthenticated, "Sign in to continue.")
		return
	}
	if errors.Is(err, auth.ErrEmailTaken) {
		a.conflict(w, r, "That email is already in use.")
		return
	}
	if err != nil {
		// 邮箱格式、显示名形状这些校验也在 UpdateUser 里。它们与上面本包
		// 的检查重叠一部分：本包判「请求形状」，下层判「这一列收不收」。
		// 只有校验类哨兵是 400；其余（查询、事务、提交失败）是库这一侧
		// 的问题，回 503，否则一次连接抖动会被说成「你的邮箱写错了」。
		if auth.IsValidationError(err) {
			a.invalid(w, r, err)
			return
		}
		a.unavailable(w, r, err, "修改用户资料失败")
		return
	}
	a.ok(w, http.StatusOK, map[string]userJSON{"user": newUserJSON(info)})
}

// changePasswordRequest 是改口令请求体。
type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword 校验旧口令、写入新口令，并让别处的会话下线。
//
// 当前会话保留：用户刚刚在这里证明过自己是本人，把他自己踢出去只会在
// 下一步让他重新登录一次，而那个动作本身不带任何安全收益。
func (a *API) handleChangePassword(w http.ResponseWriter, r *http.Request, session auth.Session) {
	if !a.transportSecure(r) {
		a.fail(w, r, http.StatusBadRequest, codeInsecureTransport,
			"Changing a password requires HTTPS.")
		return
	}

	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		a.invalid(w, r, err)
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		a.invalid(w, r, errors.New("current_password 与 new_password 都必须给出"))
		return
	}

	user, err := a.deps.accounts.LoginUserByID(r.Context(), session.UserID)
	if err != nil {
		a.unavailable(w, r, err, "查询账号口令失败")
		return
	}
	if user.PasswordHash == "" {
		a.fail(w, r, http.StatusForbidden, codeAccountNotReady,
			"This account has not been activated yet. Contact the administrator.")
		return
	}

	verified, err := auth.VerifyPassword(a.kdf, user.PasswordHash, req.CurrentPassword)
	if err != nil {
		a.internal(w, r, err, "校验口令失败")
		return
	}
	if !verified.OK {
		// 401 而不是 403：这是「凭证不对」，与登录同一条判据。这里不改
		// failed_login_count——手里已经有一个有效会话，这一条不是在线猜口令。
		a.fail(w, r, http.StatusUnauthorized, codeInvalidCredential,
			"Invalid username or password.")
		return
	}

	// 新口令与旧口令相同：不是「弱」，是「没变」。409 让前端能单独提示。
	reused, err := auth.VerifyPassword(a.kdf, user.PasswordHash, req.NewPassword)
	if err != nil {
		a.internal(w, r, err, "校验新口令失败")
		return
	}
	if reused.OK {
		a.conflict(w, r, "The new password must be different from the current one.")
		return
	}

	if validateErr := auth.ValidatePassword(user.Username, req.NewPassword); validateErr != nil {
		a.fail(w, r, http.StatusBadRequest, codeWeakPassword, validateErr.Error())
		return
	}

	hash, err := auth.HashPassword(a.kdf, req.NewPassword)
	if err != nil {
		a.internal(w, r, err, "计算口令哈希失败")
		return
	}
	if err := a.deps.accounts.SetPassword(r.Context(), user.ID, hash, &session.ID); err != nil {
		a.unavailable(w, r, err, "写入口令失败")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
