package adminapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
)

// 本文件是控制台「用户」一页（GroupUsers）。
//
// 它只在控制台的 admin 组下注册：判据里有两件事离不开「操作者是谁」——
// 不能改自己的角色/状态、不能降掉最后一个 active 管理员。内部管理面的
// 调用者是运维令牌，没有「自己」，所以那一侧不挂这一组（见 InternalGroups）。
//
// 写路径不在这里建第二套语义：改角色/状态落库走 auth.UpdateUser，最后
// 一个管理员的判据也在那个事务里（见 auth.ErrLastAdmin）。

const (
	// userListDefaultLimit 与 userListMaxLimit 是一页的条数。
	userListDefaultLimit = 50
	userListMaxLimit     = 200
	// usageWindow 是详情里「最近用量」的跨度。
	usageWindow = 7 * 24 * time.Hour
)

// userSummaryJSON 是清单里的一行，也是详情的基本资料部分。
//
// email 是**打码后**的写法（`a***@example.com`）；完整邮箱只在详情里给。
// 打码解决的不是「同一个人看不见」，而是列表被截图、被投屏、被日志采集时
// 不把整张邮箱清单带走。
type userSummaryJSON struct {
	ID          int64   `json:"id"`
	Username    string  `json:"username"`
	Email       *string `json:"email"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	LastSeenAt  *string `json:"last_seen_at"`
}

// userListJSON 是一页用户。
type userListJSON struct {
	Items      []userSummaryJSON `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// userDetailJSON 是详情：基本资料 + 完整邮箱 + 账户侧的几个计数与最近用量。
type userDetailJSON struct {
	userSummaryJSON
	Email          *string `json:"email_full"`
	Balance        string  `json:"balance"`
	ActiveKeyCount int64   `json:"active_key_count"`
	Requests7d     int64   `json:"requests_7d"`
	Amount7d       string  `json:"amount_7d"`
	ChannelCount   int64   `json:"channel_count"`
	KeyLastUsedAt  *string `json:"key_last_used_at"`
}

// userUpdateRequest 是 PATCH 的请求体。
//
// 收一个 role 而不是两个布尔：库里 users_admin_implies_merchant 那条约束
// 要求 admin 同时是 merchant，而两个布尔让「admin 且非 merchant」变成一个
// 点两下就能发出来的请求——那会撞上 CHECK 变成 500。role 由 Role.Flags()
// 折算成两列，「不可表示」的输入在解析阶段就是 400（见 userUpdateParams）。
type userUpdateRequest struct {
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

// listUsers 读一页用户。
//
// 游标不绑筛选条件：翻页途中改查询，客户端从第一页重新取。把它绑上去要
// 对每个条件做规范化再比对，而漏掉一处（例如 query 的空白折叠）的表现是
// 「翻到第二页悄悄换了一批人」——不如在文档里写清这个约定。
func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit, err := parseUserLimit(q.Get("limit"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var role *auth.Role
	if raw := q.Get("role"); raw != "" {
		parsedRole, roleErr := auth.ParseRole(raw)
		if roleErr != nil {
			a.writeError(w, http.StatusBadRequest, roleErr.Error())
			return
		}
		role = &parsedRole
	}
	var status string
	if raw := q.Get("status"); raw != "" {
		parsedStatus, statusErr := auth.ParseUserStatus(raw)
		if statusErr != nil {
			a.writeError(w, http.StatusBadRequest, statusErr.Error())
			return
		}
		status = parsedStatus
	}
	var afterID int64
	if raw := q.Get("cursor"); raw != "" {
		afterID, err = decodeUserCursor(raw)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// 多要一行：有它说明还有下一页。用 len == limit 判「可能还有」会多给
	// 一个客户端，而它的下一页正好是空的——翻页的最后一跳看起来像丢了数据。
	rows, err := a.users.ListUserPage(r.Context(), auth.UserPageQuery{
		Query: q.Get("query"), Role: role, Status: status,
		AfterID: afterID, Limit: limit + 1,
	})
	if err != nil {
		a.failUserRead(w, r, err)
		return
	}

	next := (*string)(nil)
	if len(rows) > limit {
		rows = rows[:limit]
		cursor := encodeUserCursor(rows[len(rows)-1].ID)
		next = &cursor
	}

	items := make([]userSummaryJSON, 0, len(rows))
	for _, row := range rows {
		items = append(items, userSummaryOf(row))
	}
	a.writeUsersJSON(w, userListJSON{Items: items, NextCursor: next})
}

// getUser 读一个用户的详情。
func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUserID(r.PathValue("id"))
	if !ok {
		// 「不是一个用户 id」与「这个 id 不存在」对调用方是同一件事：
		// 它给的标识没有指向任何一个用户。不区分也省得把「alice 这个
		// 用户名存在」的确认给出去。
		a.writeError(w, http.StatusNotFound, "user not found")
		return
	}
	detail, err := a.userDetail(r.Context(), id)
	if err != nil {
		a.failUserRead(w, r, err)
		return
	}
	a.writeUsersJSON(w, detail)
}

// updateUser 改一个用户的角色或状态。
func (a *API) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUserID(r.PathValue("id"))
	if !ok {
		a.writeError(w, http.StatusNotFound, "user not found")
		return
	}
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		// users 组只挂在控制台 admin 组下面，而那一组一定注入操作者。
		// 走到这里说明装配错了；不拿一个「默认操作者」顶上，那会让
		// 「不能改自己」在生产里静默失效。
		a.logger().Error("用户管理请求没有操作者，检查路由装配", "target_id", id)
		a.writeError(w, http.StatusForbidden,
			"this endpoint requires a console administrator session")
		return
	}

	var req userUpdateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	params, err := userUpdateParams(req)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	before, err := a.users.UserByRef(r.Context(), strconv.FormatInt(id, 10))
	if err != nil {
		a.fail(w, err)
		return
	}
	switch before.Status {
	case auth.UserStatusDeletionRequested:
		a.fail(w, fmt.Errorf(
			"%w：账号 %q 正在走销户流程，这一页只管理 active / suspended / banned 的账号；"+
				"先按销户流程撤销它", auth.ErrDeletionAlreadyRequested, before.Username))
		return
	case auth.UserStatusDeleted:
		a.fail(w, fmt.Errorf(
			"%w：账号 %q 已经匿名化，终态不可逆", auth.ErrDeletionAlreadyDeleted, before.Username))
		return
	}

	// 改自己的角色/状态各自给一句：合成一句会让页面只能笼统地提示
	// 「不能改自己」，而这两种改法的后果不同（丢掉进管理组的能力 vs.
	// 把自己锁在门外）。
	if actor == id && req.Role != nil {
		a.writeError(w, http.StatusBadRequest,
			"an administrator cannot change their own role")
		return
	}
	if actor == id && req.Status != nil {
		a.writeError(w, http.StatusBadRequest,
			"an administrator cannot change their own status")
		return
	}

	after, err := a.users.UpdateUser(r.Context(), strconv.FormatInt(id, 10), params)
	if err != nil {
		a.fail(w, err)
		return
	}

	changes := make(map[string]string, 2)
	if req.Role != nil {
		changes["role"] = string(before.Role) + " -> " + string(after.Role)
	}
	if req.Status != nil {
		changes["status"] = before.Status + " -> " + after.Status
	}
	// 审计日志：谁把谁改成了什么。request_id 与请求日志可对上，而前后的值
	// 是事后回答「那次改动到底是哪一步」的唯一线索——库里只剩新值。
	a.logger().Info("管理员修改用户",
		"actor_id", actor,
		"target_id", id,
		"changes", changes,
		"request_id", string(observe.RequestIDFromContext(r.Context())))

	detail, err := a.userDetail(r.Context(), id)
	if err != nil {
		a.failUserRead(w, r, err)
		return
	}
	a.writeUsersJSON(w, detail)
}

// writeUsersJSON 写一条用户数据，并标成不可缓存。
//
// 渠道、规则那几组不带宽带头（它们是配置，不是个人信息）；这一组每一条都
// 带邮箱、余额与用量，浏览器与中间代理都不该留副本。账号被停用之后，一份
// 缓存里的旧响应还会把「他还能用」展示出去。
func (a *API) writeUsersJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Cache-Control", "no-store")
	a.writeJSON(w, http.StatusOK, body)
}

// userUpdateParams 把请求体折成 auth.UpdateUserParams。
//
// 两个字段都缺时返回 auth.ErrNoChanges（空 body 与「只写了 null」是同一件
// 事：没有要改的字段）。角色经 Role.Flags() 同时给出两列，于是
// 「admin 且非 merchant」这种组合在形状上就表达不出来。
func userUpdateParams(req userUpdateRequest) (auth.UpdateUserParams, error) {
	var p auth.UpdateUserParams
	if req.Role == nil && req.Status == nil {
		return p, auth.ErrNoChanges
	}
	if req.Role != nil {
		role, err := auth.ParseRole(*req.Role)
		if err != nil {
			return p, err
		}
		isMerchant, isAdmin := role.Flags()
		p.IsMerchant, p.IsAdmin = &isMerchant, &isAdmin
	}
	if req.Status != nil {
		status, err := auth.ParseUserStatus(*req.Status)
		if err != nil {
			return p, err
		}
		p.Status = &status
	}
	return p, nil
}

// userDetail 读一页详情要用到的全部数据。
//
// 六次读彼此独立（用户、最近登录、key 清单、名下渠道、钱包、七日用量），
// 不追求快照一致：这是给运营看的「现在是什么样」，不是账本对账。
func (a *API) userDetail(ctx context.Context, id int64) (userDetailJSON, error) {
	info, err := a.users.UserByRef(ctx, strconv.FormatInt(id, 10))
	if err != nil {
		return userDetailJSON{}, err
	}

	lastSeen, sawLogin, err := a.users.UserLastSeenAt(ctx, id)
	if err != nil {
		return userDetailJSON{}, err
	}
	var lastSeenPtr *time.Time
	if sawLogin {
		lastSeenPtr = &lastSeen
	}

	keys, err := a.users.ListKeys(ctx, &id)
	if err != nil {
		return userDetailJSON{}, fmt.Errorf("读取用户 %d 的 API Key 失败: %w", id, err)
	}
	var (
		activeKeys int64
		keyLastUse *time.Time
	)
	for _, key := range keys {
		if key.Status == keyStatusActive {
			activeKeys++
		}
		if key.LastUsedAt != nil && (keyLastUse == nil || key.LastUsedAt.After(*keyLastUse)) {
			used := *key.LastUsedAt
			keyLastUse = &used
		}
	}

	owned, err := a.channels.ListOwned(ctx, id)
	if err != nil {
		return userDetailJSON{}, fmt.Errorf("读取用户名下渠道失败: %w", err)
	}

	// 没有钱包行的用户是 0 元，与 /api/v1/wallet 同口径：读路径不建行。
	var balance money.Amount
	wallets, err := a.ledger.Wallets(ctx, &id)
	if err != nil {
		return userDetailJSON{}, fmt.Errorf("读取用户钱包失败: %w", err)
	}
	for i := range wallets {
		if string(wallets[i].Asset) != a.currency {
			continue
		}
		if wallets[i].BalanceErr != nil {
			return userDetailJSON{}, fmt.Errorf("用户 %d 的钱包余额读不出来: %w",
				id, wallets[i].BalanceErr)
		}
		balance = wallets[i].Balance
		break
	}

	now := time.Now().UTC()
	usage, err := a.ledger.UsageSummary(ctx, store.UsageWindow{
		UserID: id, From: now.Add(-usageWindow), To: now,
	})
	if err != nil {
		return userDetailJSON{}, fmt.Errorf("读取用户最近用量失败: %w", err)
	}

	return userDetailJSON{
		userSummaryJSON: userSummaryOf(auth.UserRow{UserInfo: info, LastSeenAt: lastSeenPtr}),
		Email:           nullableString(info.Email),
		Balance:         balance.String(),
		ActiveKeyCount:  activeKeys,
		Requests7d:      usage.Totals.Requests,
		Amount7d:        usage.Totals.Cost.String(),
		ChannelCount:    int64(len(owned)),
		KeyLastUsedAt:   timeString(keyLastUse),
	}, nil
}

// keyStatusActive 与 auth.Key.Status 的取值之一对应。写成字面量是因为
// auth 没有导出这个常量（它只在库的 CHECK 里定义），而这里只需要比一次。
const keyStatusActive = "active"

// userSummaryOf 把一行读侧数据折成响应形状。
func userSummaryOf(row auth.UserRow) userSummaryJSON {
	return userSummaryJSON{
		ID:          row.ID,
		Username:    row.Username,
		Email:       maskEmail(row.Email),
		DisplayName: row.DisplayName,
		Role:        string(row.Role),
		Status:      row.Status,
		CreatedAt:   row.CreatedAt.UTC().Format(time.RFC3339),
		LastSeenAt:  timeString(row.LastSeenAt),
	}
}

// maskEmail 把邮箱打成 `a***@example.com`。
//
// 本地部分保留首字符，域名整段保留：运营要能按域名认出是谁家的账号，
// 而首字符足以让人对上是哪一个。没有 @ 的列（不该出现，但列上没有约束）
// 整段打成 ***，不把可能是邮箱的值原样带出去。
func maskEmail(email string) *string {
	if email == "" {
		return nil
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		masked := "***"
		return &masked
	}
	first, _ := firstRune(local)
	masked := string(first) + "***@" + domain
	return &masked
}

// firstRune 取第一个字符；本地部分非空时一定有值。
func firstRune(s string) (rune, bool) {
	for _, r := range s {
		return r, true
	}
	return 0, false
}

// nullableString 把空串折成 null：users.email 为 NULL 与空串在库里是两件事
// （空串会被唯一约束当成一个具体值，见 auth.CreateUser），而读侧只呈现
// 「有邮箱」与「没有」。
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// timeString 把可空时刻折成 RFC3339 UTC 字符串；nil 写成 null。
func timeString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// parseUserLimit 解析分页的 limit。
func parseUserLimit(raw string) (int, error) {
	if raw == "" {
		return userListDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > userListMaxLimit {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", userListMaxLimit)
	}
	return n, nil
}

// parseUserID 解析路径里的用户 id，只接受十进制正整数。
//
// 不能直接把路径段交给 auth.UserByRef：它把非数字当成用户名（三条路径认的是
// 同一个人，见那里的注释），于是 /users/alice 会读到 alice，而不是 404。
func parseUserID(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// userCursorVersion 是游标编码的前缀。它让「这一版游标」与将来改了形状的
// 游标分得开：解不开旧游标时报 400，客户端从第一页重取，而不是把两套
// 编码混着解释。
const userCursorVersion = "u1:"

// encodeUserCursor 把「上一页最后一行」编成不透明串。
//
// 每页只有这一列，所以内层就是 id；外层 base64 让它看起来不像可以手写的
// 页码——客户端不该去构造或修改它（不绑筛选条件的约定见 listUsers）。
func encodeUserCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(userCursorVersion + strconv.FormatInt(id, 10)))
}

// decodeUserCursor 解出上一页最后一行的 id。
func decodeUserCursor(raw string) (int64, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, errors.New("invalid cursor")
	}
	text := string(decoded)
	if !strings.HasPrefix(text, userCursorVersion) {
		return 0, errors.New("invalid cursor")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(text, userCursorVersion), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid cursor")
	}
	return id, nil
}

// failUserRead 是读路径失败的统一出口。
//
// 「找不到」占 404；其余（库读不出来、金额解析失败）按 503：调用方该做的
// 是稍后重试或看日志，而不是改参数。原因只进日志——它可能带出库里的原文。
func (a *API) failUserRead(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrUserNotFound) {
		a.writeError(w, http.StatusNotFound, "user not found")
		return
	}
	a.logger().Error("读取用户数据失败", "error", err,
		"request_id", string(observe.RequestIDFromContext(r.Context())))
	a.writeError(w, http.StatusServiceUnavailable, msgUnavailable)
}
