package consoleapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是 API Key 的读写。
//
// 写路径全部走 auth.Store：`larkaxctl apikey` 能改的字段，这里都能改，反之
// 亦然。两条写路径各写一份 SQL 的那天，就是「命令行改得动、控制台改不动」
// 这类偏差开始的那天。

// keyJSON 是响应里的密钥对象。
//
// 不带完整明文，也不带 key_hash：前者只在创建时出现一次，后者没有展示价值。
type keyJSON struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Prefix       string  `json:"prefix"`
	Suffix       *string `json:"suffix"`
	Masked       string  `json:"masked"`
	Status       string  `json:"status"`
	QuotaLimit   *string `json:"quota_limit"`
	QuotaUsed    string  `json:"quota_used"`
	RateLimitRPM *int    `json:"rate_limit_rpm"`
	ExpiresAt    *string `json:"expires_at"`
	LastUsedAt   *string `json:"last_used_at"`
	CreatedAt    string  `json:"created_at"`
}

// keyMaskStars 是掩码里星号的个数：`sk-` + 8 + 18 + 6 = 35，与完整密钥等长。
const keyMaskStars = 18

// newKeyJSON 把一行 key 折成响应形状。
//
// used 是**累计**花费（keyquota.Spent 的口径：已结算 + 在飞预留）。它与
// quota_limit 一样是累计的，界面上的文案必须写「累计」——写成「每月」会让
// 人以为下个月会重置。
func newKeyJSON(k auth.Key, used money.Amount) keyJSON {
	out := keyJSON{
		ID:           k.ID,
		Name:         k.Name,
		Prefix:       k.Prefix,
		Masked:       maskKey(k.Prefix, k.Suffix),
		Status:       k.Status,
		QuotaUsed:    used.String(),
		RateLimitRPM: k.RateLimitRPM,
		CreatedAt:    formatTime(k.CreatedAt),
	}
	if k.Suffix != "" {
		suffix := k.Suffix
		out.Suffix = &suffix
	}
	if k.QuotaLimit != nil {
		limit := k.QuotaLimit.String()
		out.QuotaLimit = &limit
	}
	out.ExpiresAt = optionalTime(k.ExpiresAt)
	out.LastUsedAt = optionalTime(k.LastUsedAt)
	return out
}

// maskKey 拼出展示用的掩码。
//
// 老行（`larkax_` 方案）没有 key_suffix——那一列是迁移 00050 才加的，而只存
// 哈希的后缀无从恢复。它们退化成 `prefix + "…"`，比猜一个后缀好：猜错的后缀
// 会让人以为「就是这一把」，而它可能对应另一把钥匙。
func maskKey(prefix, suffix string) string {
	if suffix == "" {
		return prefix + "…"
	}
	return prefix + strings.Repeat("*", keyMaskStars) + suffix
}

// keyPage 是一页密钥。
type keyPage struct {
	Items      []keyJSON `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// handleListKeys 列当前用户的密钥。
func (a *API) handleListKeys(w http.ResponseWriter, r *http.Request, session auth.Session) {
	q := r.URL.Query()

	status := q.Get("status")
	if status != "" {
		known := false
		for _, candidate := range auth.KeyStatuses() {
			if status == candidate {
				known = true
				break
			}
		}
		if !known {
			a.invalid(w, r, errors.New("status 只能是 active、disabled、revoked"))
			return
		}
	}

	limit, err := parseLimit(q.Get("limit"), auth.KeyPageDefaultLimit, auth.KeyPageMaxLimit)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	var cursor *auth.KeyCursor
	if raw := q.Get("cursor"); raw != "" {
		at, id, cursorErr := decodeCursor(raw)
		if cursorErr != nil {
			a.invalid(w, r, cursorErr)
			return
		}
		cursor = &auth.KeyCursor{CreatedAt: at, ID: id}
	}

	page, err := a.deps.keys.ListKeysPage(r.Context(), auth.KeyListQuery{
		UserID: session.UserID,
		Status: status,
		Cursor: cursor,
		Limit:  limit,
	})
	if err != nil {
		a.unavailable(w, r, err, "读取密钥清单失败")
		return
	}

	ids := make([]int64, 0, len(page.Keys))
	for _, key := range page.Keys {
		ids = append(ids, key.ID)
	}
	used, err := a.deps.spent(r.Context(), ids)
	if err != nil {
		a.unavailable(w, r, err, "读取密钥累计花费失败")
		return
	}

	items := make([]keyJSON, 0, len(page.Keys))
	for _, key := range page.Keys {
		items = append(items, newKeyJSON(key, used[key.ID]))
	}

	out := keyPage{Items: items}
	if page.More && len(page.Keys) > 0 {
		last := page.Keys[len(page.Keys)-1]
		next := encodeCursor(last.CreatedAt, last.ID)
		out.NextCursor = &next
	}
	a.ok(w, http.StatusOK, out)
}

// createKeyRequest 是创建请求体。全部字段可选。
type createKeyRequest struct {
	Name         optionalString `json:"name"`
	QuotaLimit   optionalMoney  `json:"quota_limit"`
	RateLimitRPM optionalInt    `json:"rate_limit_rpm"`
	ExpiresAt    optionalString `json:"expires_at"`
}

// handleCreateKey 建一把新密钥，明文只出现这一次。
func (a *API) handleCreateKey(w http.ResponseWriter, r *http.Request, session auth.Session) {
	var req createKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		a.invalid(w, r, err)
		return
	}

	params := auth.CreateParams{UserID: session.UserID}

	if req.Name.Set && req.Name.Value != nil {
		params.Name = *req.Name.Value
	}

	if req.QuotaLimit.Set {
		if req.QuotaLimit.Value == nil {
			// null 在这一列上没有意义：创建时要么给一个上限，要么不给字段
			// 表示不限。把 null 也当成「不限」会让「清空」与「没给」混成一件事。
			a.invalid(w, r, errors.New("quota_limit 不能是 null；不限就不给这个字段"))
			return
		}
		if !req.QuotaLimit.Value.IsPositive() {
			a.invalid(w, r, errors.New("quota_limit 必须大于 0；不限就不给这个字段"))
			return
		}
		limit := *req.QuotaLimit.Value
		params.QuotaLimit = &limit
	}

	if req.RateLimitRPM.Set && req.RateLimitRPM.Value != nil {
		if *req.RateLimitRPM.Value < 0 {
			a.invalid(w, r, errors.New("rate_limit_rpm 不能为负数"))
			return
		}
		rpm := *req.RateLimitRPM.Value
		params.RateLimitRPM = &rpm
	}

	if req.ExpiresAt.Set && req.ExpiresAt.Value != nil {
		expires, err := time.Parse(time.RFC3339, *req.ExpiresAt.Value)
		if err != nil {
			a.invalid(w, r, errors.New("expires_at 必须是 RFC3339 时刻"))
			return
		}
		if !expires.After(a.now()) {
			a.invalid(w, r, errors.New("expires_at 必须是未来时刻"))
			return
		}
		utc := expires.UTC()
		params.ExpiresAt = &utc
	}

	created, err := a.deps.keys.Create(r.Context(), params)
	if err != nil {
		// 备注形状这类校验错在请求里（400）；外键、熵源、连接这些错在
		// 库这一侧（503）。两者都归 400 的话，一次数据库故障会被说成
		// 「你的备注写错了」。
		if auth.IsValidationError(err) {
			a.invalid(w, r, err)
			return
		}
		a.unavailable(w, r, err, "创建密钥失败")
		return
	}

	// 再读一次是为了拿到完整的一行（状态、创建时刻、额度）：Create 只交回
	// id / prefix / suffix / 明文。多这一次读换来的是「创建与列表里的对象
	// 形状逐字相同」，前端不必为创建响应再写一份映射。
	key, err := a.deps.keys.KeyForUser(r.Context(), session.UserID, created.ID)
	if err != nil {
		a.unavailable(w, r, err, "读取新建的密钥失败")
		return
	}

	w.Header().Set("Location", PathPrefix+"keys/"+strconv.FormatInt(key.ID, 10))
	a.ok(w, http.StatusCreated, map[string]any{
		"key":    newKeyJSON(key, money.Zero),
		"secret": created.Plaintext,
	})
}

// handleGetKey 读一把密钥。不属于当前用户与不存在都是 404。
func (a *API) handleGetKey(w http.ResponseWriter, r *http.Request, session auth.Session) {
	id, err := pathID(r)
	if err != nil {
		a.notFound(w, r)
		return
	}
	key, err := a.deps.keys.KeyForUser(r.Context(), session.UserID, id)
	if errors.Is(err, auth.ErrKeyNotFound) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.unavailable(w, r, err, "读取密钥失败")
		return
	}
	used, err := a.deps.spent(r.Context(), []int64{id})
	if err != nil {
		a.unavailable(w, r, err, "读取密钥累计花费失败")
		return
	}
	a.ok(w, http.StatusOK, newKeyJSON(key, used[id]))
}

// patchKeyRequest 是修改请求体。
//
// 每一项都是指针套可选类型：缺省不动、null 清空（对额度与限速而言是
// 「不限」）、给值设成它。`status` 给 null 没有意义，这里按 400 处理。
type patchKeyRequest struct {
	Name         optionalString `json:"name"`
	QuotaLimit   optionalMoney  `json:"quota_limit"`
	RateLimitRPM optionalInt    `json:"rate_limit_rpm"`
	ExpiresAt    optionalString `json:"expires_at"`
	Status       optionalString `json:"status"`
}

// handlePatchKey 改一把密钥。
func (a *API) handlePatchKey(w http.ResponseWriter, r *http.Request, session auth.Session) {
	id, err := pathID(r)
	if err != nil {
		a.notFound(w, r)
		return
	}

	current, err := a.deps.keys.KeyForUser(r.Context(), session.UserID, id)
	if errors.Is(err, auth.ErrKeyNotFound) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.unavailable(w, r, err, "读取密钥失败")
		return
	}
	// 已吊销的 key 拒绝一切修改：它已经是一份历史，改备注会让「当时那把」
	// 变得不可辨认。吊销端点自身是幂等的，所以那条路径不走这里。
	if current.Status == "revoked" {
		a.conflict(w, r, "This key has been revoked and can no longer be modified.")
		return
	}

	var req patchKeyRequest
	if decodeErr := decodeJSON(r, &req); decodeErr != nil {
		a.invalid(w, r, decodeErr)
		return
	}

	var params auth.KeyParams

	if req.Name.Set {
		if req.Name.Value == nil {
			a.invalid(w, r, errors.New("name 不能是 null；要清空就给空字符串"))
			return
		}
		params.SetName = true
		params.Name = *req.Name.Value
	}

	if req.QuotaLimit.Set {
		params.SetQuota = true
		if req.QuotaLimit.Value != nil {
			// 0 是合法值：它表示「一分不放行」，与 null 的「不限」是两件事
			// （见 values.go 的三态约定）。只有负数是形状错误。
			if req.QuotaLimit.Value.IsNegative() {
				a.invalid(w, r, errors.New("quota_limit 不能为负数；不限就用 null"))
				return
			}
			limit := *req.QuotaLimit.Value
			params.QuotaLimit = &limit
		}
	}

	if req.RateLimitRPM.Set {
		params.SetRateLimit = true
		if req.RateLimitRPM.Value != nil {
			if *req.RateLimitRPM.Value < 0 {
				a.invalid(w, r, errors.New("rate_limit_rpm 不能为负数"))
				return
			}
			rpm := *req.RateLimitRPM.Value
			params.RateLimitRPM = &rpm
		}
	}

	if req.ExpiresAt.Set {
		params.SetExpiresAt = true
		if req.ExpiresAt.Value != nil {
			expires, parseErr := time.Parse(time.RFC3339, *req.ExpiresAt.Value)
			if parseErr != nil {
				a.invalid(w, r, errors.New("expires_at 必须是 RFC3339 时刻"))
				return
			}
			if !expires.After(a.now()) {
				// 与创建同一条判据：写一个已经过去的时刻等于把这把 key
				// 立刻作废，而作废有 revoke 端点，它留下吊销的痕迹。
				a.invalid(w, r, errors.New("expires_at 必须是未来时刻"))
				return
			}
			utc := expires.UTC()
			params.ExpiresAt = &utc
		}
	}

	if req.Status.Set {
		if req.Status.Value == nil {
			a.invalid(w, r, errors.New("status 不能是 null"))
			return
		}
		status := *req.Status.Value
		switch status {
		case "active", "disabled":
			params.SetStatus = true
			params.Status = status
		case "revoked":
			// 吊销只由吊销端点产生，且不可逆。从这里写进来会让「谁吊销的、
			// 什么时候」失去唯一的来源。
			a.conflict(w, r, "Use the revoke endpoint to revoke a key.")
			return
		default:
			a.invalid(w, r, errors.New("status 只能是 active 或 disabled"))
			return
		}
	}

	if !params.SetName && !params.SetQuota && !params.SetRateLimit &&
		!params.SetExpiresAt && !params.SetStatus {
		a.invalid(w, r, errors.New("没有要改的字段"))
		return
	}

	key, err := a.deps.keys.UpdateKey(r.Context(), id, params)
	if errors.Is(err, auth.ErrKeyNotFound) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		// 与创建同一套判据：只把校验类哨兵当 400（备注形状），
		// 查询与提交失败是库这一侧的问题，回 503。
		if auth.IsValidationError(err) {
			a.invalid(w, r, err)
			return
		}
		a.unavailable(w, r, err, "修改密钥失败")
		return
	}

	used, err := a.deps.spent(r.Context(), []int64{id})
	if err != nil {
		a.unavailable(w, r, err, "读取密钥累计花费失败")
		return
	}
	a.ok(w, http.StatusOK, newKeyJSON(key, used[id]))
}

// handleRevokeKey 吊销一把密钥。
//
// 幂等：已经吊销的再调一次仍然 200，返回同一份状态。重试不该报错——
// 客户端在一次超时之后无法知道第一次到底成没成。
func (a *API) handleRevokeKey(w http.ResponseWriter, r *http.Request, session auth.Session) {
	id, err := pathID(r)
	if err != nil {
		a.notFound(w, r)
		return
	}

	current, err := a.deps.keys.KeyForUser(r.Context(), session.UserID, id)
	if errors.Is(err, auth.ErrKeyNotFound) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.unavailable(w, r, err, "读取密钥失败")
		return
	}

	key := current
	if current.Status != "revoked" {
		key, err = a.deps.keys.UpdateKey(r.Context(), id, auth.KeyParams{
			SetStatus: true,
			Status:    "revoked",
		})
		if errors.Is(err, auth.ErrKeyNotFound) {
			a.notFound(w, r)
			return
		}
		if err != nil {
			a.unavailable(w, r, err, "吊销密钥失败")
			return
		}
	}

	used, err := a.deps.spent(r.Context(), []int64{id})
	if err != nil {
		a.unavailable(w, r, err, "读取密钥累计花费失败")
		return
	}
	a.ok(w, http.StatusOK, newKeyJSON(key, used[id]))
}
