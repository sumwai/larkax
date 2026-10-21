package adminapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/observe"
)

// 本文件是用户管理（GroupUsers）的真库用例。
//
// 拒绝路径占了大多数：能改成功的那些只是「写进去了」，而把最后一个管理员
// 降掉、改自己的角色这两类事没有第二次机会验证。判据里有两处只有真库能
// 说话——auth.UpdateUser 的事务与咨询锁、以及 users 上的两条角色标记约束。

// usersTestHandler 装一份只注册 users 组的控制台形状 handler。
//
// 前缀用 /api/v1/admin/：与控制台挂的挂载点逐字一致。鉴权在这里放行——
// 会话与角色的那两道门在 consoleapi 的用例里（见 roles_test.go）。
func usersTestHandler(t *testing.T, logger *slog.Logger) http.Handler {
	t.Helper()
	cfg := Config{
		Prefix:    "/api/v1/admin/",
		Authorize: func(next http.Handler) http.Handler { return next },
		Groups:    []string{GroupUsers},
	}
	if logger != nil {
		cfg.Logger = logger
	}
	return testSurface(t, cfg)
}

// usersTestUser 是一条夹具。
type usersTestUser struct {
	ID       int64
	Username string
	Email    string
}

// newUsersTestUser 建一个用户并登记收尾删除。
func newUsersTestUser(t *testing.T, db *sql.DB, role auth.Role, prefix string) usersTestUser {
	t.Helper()
	username := uniqueName(prefix)
	email := username + "@example.com"
	created, err := auth.New(db).CreateUser(t.Context(), auth.CreateUserParams{
		Username: username, Email: email, Role: role,
	})
	if err != nil {
		t.Fatalf("建用户 %q 失败: %v", username, err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(t.Context()),
			`DELETE FROM users WHERE id = $1`, created.ID); err != nil {
			t.Logf("清理用户 %d 失败: %v", created.ID, err)
		}
	})
	return usersTestUser{ID: created.ID, Username: username, Email: email}
}

// usersCall 发一次请求。actor > 0 时按控制台 admin 组的方式注入操作者。
func usersCall(t *testing.T, ctx context.Context, h http.Handler,
	method, path string, actor int64, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(ctx, method, path, reader)
	if actor > 0 {
		//nolint:contextcheck // WithActor 只是 context.WithValue，不是新建一条链
		req = req.WithContext(WithActor(req.Context(), actor))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// usersListItem 是清单行的**独立**解码形状：不复用生产结构体，字段名改了
// 这里会失败，而不是跟着改。
type usersListItem struct {
	ID          int64   `json:"id"`
	Username    string  `json:"username"`
	Email       *string `json:"email"`
	DisplayName string  `json:"display_name"`
	Role        string  `json:"role"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	LastSeenAt  *string `json:"last_seen_at"`
}

type usersListPage struct {
	Items      []usersListItem `json:"items"`
	NextCursor *string         `json:"next_cursor"`
}

type usersDetail struct {
	usersListItem
	EmailFull      *string `json:"email_full"`
	Balance        string  `json:"balance"`
	ActiveKeyCount int64   `json:"active_key_count"`
	Requests7d     int64   `json:"requests_7d"`
	Amount7d       string  `json:"amount_7d"`
	ChannelCount   int64   `json:"channel_count"`
	KeyLastUsedAt  *string `json:"key_last_used_at"`
}

func decodeUsers(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}
}

// TestUsersListPaginatesWithoutGapsOrDuplicates 翻到底：不重、不漏。
func TestUsersListPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	h := usersTestHandler(t, nil)
	db := testDB(t)
	base := uniqueName("page")
	var want []int64
	for i := 0; i < 5; i++ {
		u := newUsersTestUser(t, db, auth.RoleUser, fmt.Sprintf("%s-%d", base, i))
		want = append(want, u.ID)
	}

	var (
		got      []int64
		cursor   string
		pages    int
		first    usersListItem
		sawEmail bool
	)
	for {
		path := "/api/v1/admin/users?limit=2&query=" + url.QueryEscape(base)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := usersCall(t, t.Context(), h, http.MethodGet, path, 0, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("翻页请求状态码 = %d；正文：%s", rec.Code, rec.Body)
		}
		var page usersListPage
		decodeUsers(t, rec, &page)
		pages++
		// 每页不能超过 limit：服务端无视 limit 一次全给时，最后的并集仍相等，
		// 上面那条断言看不见。
		if len(page.Items) > 2 {
			t.Fatalf("limit=2 的一页回了 %d 条：%+v", len(page.Items), page.Items)
		}
		for _, item := range page.Items {
			if first.Username == "" {
				first = item
			}
			if item.Email != nil {
				sawEmail = true
			}
			got = append(got, item.ID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
		if pages > 10 {
			t.Fatal("翻页没有终点")
		}
	}

	if len(got) != len(want) {
		t.Fatalf("翻完得到 %d 条，期望 %d 条：%v", len(got), len(want), got)
	}
	seen := make(map[int64]bool, len(got))
	for i, id := range got {
		if seen[id] {
			t.Fatalf("id %d 出现了两次：%v", id, got)
		}
		seen[id] = true
		if i > 0 && got[i-1] >= id {
			t.Fatalf("id 不是严格升序：%v", got)
		}
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("漏了 id %d", id)
		}
	}

	// 邮箱打码在这里顺带钉一下：清单里不该出现完整地址。
	if !sawEmail {
		t.Fatal("清单里一条邮箱都没有，打码断言于是没验到东西")
	}
	if first.Email == nil {
		t.Fatal("第一行的 email 是 null")
	}
	if !strings.Contains(*first.Email, "***") || strings.HasSuffix(*first.Email, first.Username) {
		t.Errorf("清单里 email = %q，期望打码后只留首字符与域名", *first.Email)
	}
	if strings.Contains(*first.Email, first.Username) {
		t.Errorf("清单里 email = %q，仍然带着完整用户名", *first.Email)
	}
}

// TestUsersSearchEscapesWildcards 搜一个 % 或 _ 不能变成通配符。
func TestUsersSearchEscapesWildcards(t *testing.T) {
	h := usersTestHandler(t, nil)
	db := testDB(t)
	u := newUsersTestUser(t, db, auth.RoleUser, "esc")
	// 第二个用户让「不转义时 `%` 会吃掉后半段」有东西可匹配。
	newUsersTestUser(t, db, auth.RoleUser, "esc")

	// 原文里没有 % 时，带上 % 的查询只能匹配到字面量，于是 0 条。
	// 不转义的话 `%` 会吃掉后半段，把两个用户都列出来。
	for _, suffix := range []string{"%", "_"} {
		path := "/api/v1/admin/users?query=" + url.QueryEscape(u.Username+suffix)
		rec := usersCall(t, t.Context(), h, http.MethodGet, path, 0, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("查询 %q 状态码 = %d；正文：%s", suffix, rec.Code, rec.Body)
		}
		var page usersListPage
		decodeUsers(t, rec, &page)
		if len(page.Items) != 0 {
			t.Errorf("搜 %q 得到 %d 条，期望 0 条：%+v", u.Username+suffix, len(page.Items), page.Items)
		}
	}

	// 不转义的方向要能搜到：这是上一条的前提，否则「0 条」什么也证明不了。
	path := "/api/v1/admin/users?query=" + url.QueryEscape(u.Username)
	rec := usersCall(t, t.Context(), h, http.MethodGet, path, 0, nil)
	var page usersListPage
	decodeUsers(t, rec, &page)
	if len(page.Items) != 1 || page.Items[0].ID != u.ID {
		t.Errorf("按完整用户名搜到 %+v，期望只有 %d", page.Items, u.ID)
	}
}

// TestUsersRoleFilterExcludesAdminFromMerchant merchant 的意思是
// 「is_merchant 真且 is_admin 假」，admin 不能被混进来。
func TestUsersRoleFilterExcludesAdminFromMerchant(t *testing.T) {
	h := usersTestHandler(t, nil)
	db := testDB(t)
	base := uniqueName("role")

	merchant := newUsersTestUser(t, db, auth.RoleMerchant, base+"-m")
	admin := newUsersTestUser(t, db, auth.RoleAdmin, base+"-a")
	plain := newUsersTestUser(t, db, auth.RoleUser, base+"-u")

	cases := []struct {
		role string
		want []int64
	}{
		{"merchant", []int64{merchant.ID}},
		{"admin", []int64{admin.ID}},
		{"user", []int64{plain.ID}},
	}
	for _, tc := range cases {
		path := "/api/v1/admin/users?query=" + url.QueryEscape(base) + "&role=" + tc.role
		rec := usersCall(t, t.Context(), h, http.MethodGet, path, 0, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("role=%s 状态码 = %d；正文：%s", tc.role, rec.Code, rec.Body)
		}
		var page usersListPage
		decodeUsers(t, rec, &page)
		if len(page.Items) != len(tc.want) {
			t.Fatalf("role=%s 得到 %d 条：%+v", tc.role, len(page.Items), page.Items)
		}
		for i, id := range tc.want {
			if page.Items[i].ID != id {
				t.Errorf("role=%s 第 %d 条 id = %d，期望 %d", tc.role, i, page.Items[i].ID, id)
			}
			if page.Items[i].Role != tc.role {
				t.Errorf("role=%s 第 %d 条折出来的角色是 %q", tc.role, i, page.Items[i].Role)
			}
		}
	}
}

// TestUsersListReportsLastSeenAt 清单里的 last_seen_at 取该用户会话的最大
// 活动时刻；从没登录过的是 null。
func TestUsersListReportsLastSeenAt(t *testing.T) {
	ctx := t.Context()
	h := usersTestHandler(t, nil)
	db := testDB(t)
	store := auth.New(db)

	seenUser := newUsersTestUser(t, db, auth.RoleUser, "seen")
	if _, _, err := store.CreateSession(ctx, seenUser.ID, "test-agent", nil); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	seen := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Second)
	if _, err := db.ExecContext(ctx,
		`UPDATE user_sessions SET last_seen_at = $2 WHERE user_id = $1`,
		seenUser.ID, seen); err != nil {
		t.Fatalf("改 last_seen_at 失败: %v", err)
	}
	neverUser := newUsersTestUser(t, db, auth.RoleUser, "neverseen")

	cases := []struct {
		name string
		user usersTestUser
		want *string
	}{
		{"登录过", seenUser, strPtr(seen.Format(time.RFC3339))},
		{"从没登录", neverUser, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/v1/admin/users?query=" + url.QueryEscape(tc.user.Username)
			rec := usersCall(t, ctx, h, http.MethodGet, path, 0, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d；正文：%s", rec.Code, rec.Body)
			}
			var page usersListPage
			decodeUsers(t, rec, &page)
			if len(page.Items) != 1 || page.Items[0].ID != tc.user.ID {
				t.Fatalf("搜 %q 得到 %+v，期望只有 %d", tc.user.Username, page.Items, tc.user.ID)
			}
			got := page.Items[0].LastSeenAt
			if tc.want == nil {
				if got != nil {
					t.Errorf("从没登录的用户 last_seen_at = %q，期望 null", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Errorf("last_seen_at = %v，期望 %q", got, *tc.want)
			}
		})
	}
}

// TestUsersStatusFilter 状态筛选真的下到了 SQL：active 与 suspended 各只
// 回自己那一批。
func TestUsersStatusFilter(t *testing.T) {
	ctx := t.Context()
	h := usersTestHandler(t, nil)
	db := testDB(t)
	base := uniqueName("status")

	active := newUsersTestUser(t, db, auth.RoleUser, base+"-a")
	suspended := newUsersTestUser(t, db, auth.RoleUser, base+"-s")
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET status = 'suspended' WHERE id = $1`, suspended.ID); err != nil {
		t.Fatalf("置成 suspended 失败: %v", err)
	}

	cases := []struct {
		status string
		want   int64
	}{
		{"active", active.ID},
		{"suspended", suspended.ID},
	}
	for _, tc := range cases {
		path := "/api/v1/admin/users?query=" + url.QueryEscape(base) + "&status=" + tc.status
		rec := usersCall(t, ctx, h, http.MethodGet, path, 0, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%s 状态码 = %d；正文：%s", tc.status, rec.Code, rec.Body)
		}
		var page usersListPage
		decodeUsers(t, rec, &page)
		if len(page.Items) != 1 || page.Items[0].ID != tc.want {
			t.Errorf("status=%s 得到 %+v，期望只有 %d", tc.status, page.Items, tc.want)
		}
		if len(page.Items) == 1 && page.Items[0].Status != tc.status {
			t.Errorf("status=%s 回了一行 status=%q", tc.status, page.Items[0].Status)
		}
	}
}

// TestUsersResponsesAreNotCacheable 成功与失败响应都带 no-store：这一组的
// 错误体也可能带着用户名。
func TestUsersResponsesAreNotCacheable(t *testing.T) {
	ctx := t.Context()
	h := usersTestHandler(t, nil)
	db := testDB(t)
	u := newUsersTestUser(t, db, auth.RoleUser, "nostore")

	cases := []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"清单成功", usersCall(t, ctx, h, http.MethodGet,
			"/api/v1/admin/users?query="+url.QueryEscape(u.Username), 0, nil)},
		{"详情成功", usersCall(t, ctx, h, http.MethodGet,
			fmt.Sprintf("/api/v1/admin/users/%d", u.ID), 0, nil)},
		{"404", usersCall(t, ctx, h, http.MethodGet,
			"/api/v1/admin/users/not-a-number", 0, nil)},
		{"400", usersCall(t, ctx, h, http.MethodGet,
			"/api/v1/admin/users?limit=0", 0, nil)},
		{"403", usersCall(t, ctx, h, http.MethodPatch,
			fmt.Sprintf("/api/v1/admin/users/%d", u.ID), 0, map[string]string{"status": "suspended"})},
	}
	for _, tc := range cases {
		if got := tc.rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s 的 Cache-Control = %q，期望 no-store（状态码 %d）",
				tc.name, got, tc.rec.Code)
		}
	}
}

// TestUsersDetailReportsAccountSide 详情给出完整邮箱、余额、key 计数、
// 最近登录与 key 最近使用。
func TestUsersDetailReportsAccountSide(t *testing.T) {
	ctx := t.Context()
	h := usersTestHandler(t, nil)
	db := testDB(t)
	u := newUsersTestUser(t, db, auth.RoleMerchant, "detail")
	store := auth.New(db)

	// 一次登录：last_seen_at 手工挪到固定时刻，免得断言依赖「刚刚」。
	if _, _, err := store.CreateSession(ctx, u.ID, "test-agent", nil); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	seen := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	if _, err := db.ExecContext(ctx,
		`UPDATE user_sessions SET last_seen_at = $2 WHERE user_id = $1`, u.ID, seen); err != nil {
		t.Fatalf("改 last_seen_at 失败: %v", err)
	}

	// 一把 key，并给它一个 last_used_at。
	created, err := store.Create(ctx, auth.CreateParams{UserID: u.ID, Name: "detail-key"})
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	used := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	if _, err := db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, created.ID, used); err != nil {
		t.Fatalf("改 last_used_at 失败: %v", err)
	}

	// 钱包一行。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_wallets (user_id, asset, balance) VALUES ($1, 'CNY', 12.34)
		 ON CONFLICT (user_id, asset) DO UPDATE SET balance = EXCLUDED.balance`, u.ID); err != nil {
		t.Fatalf("写钱包失败: %v", err)
	}

	rec := usersCall(t, ctx, h, http.MethodGet,
		fmt.Sprintf("/api/v1/admin/users/%d", u.ID), 0, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var detail usersDetail
	decodeUsers(t, rec, &detail)

	if detail.ID != u.ID || detail.Username != u.Username {
		t.Errorf("详情指向 %d/%q，期望 %d/%q", detail.ID, detail.Username, u.ID, u.Username)
	}
	if detail.EmailFull == nil || *detail.EmailFull != u.Email {
		t.Errorf("email_full = %v，期望完整邮箱 %q", detail.EmailFull, u.Email)
	}
	if detail.Email == nil || *detail.Email == u.Email {
		t.Errorf("清单那一栏 email = %v；详情里也必须是打码后的", detail.Email)
	}
	if detail.Balance != "12.34000000" {
		t.Errorf("balance = %q，期望 12.34000000", detail.Balance)
	}
	if detail.ActiveKeyCount != 1 {
		t.Errorf("active_key_count = %d，期望 1", detail.ActiveKeyCount)
	}
	if detail.Requests7d != 0 || detail.Amount7d != "0.00000000" {
		t.Errorf("七日用量 = %d 次 / %q，期望 0 次 / 0.00000000",
			detail.Requests7d, detail.Amount7d)
	}
	if detail.ChannelCount != 0 {
		t.Errorf("channel_count = %d，期望 0", detail.ChannelCount)
	}
	if detail.LastSeenAt == nil || *detail.LastSeenAt != seen.Format(time.RFC3339) {
		t.Errorf("last_seen_at = %v，期望 %q", detail.LastSeenAt, seen.Format(time.RFC3339))
	}
	if detail.KeyLastUsedAt == nil || *detail.KeyLastUsedAt != used.Format(time.RFC3339) {
		t.Errorf("key_last_used_at = %v，期望 %q", detail.KeyLastUsedAt, used.Format(time.RFC3339))
	}
}

// TestUsersRejectsNonNumericIDs 非数字路径段一律 404，不能让
// auth.UserByRef 把它当用户名读出来。
func TestUsersRejectsNonNumericIDs(t *testing.T) {
	h := usersTestHandler(t, nil)
	db := testDB(t)
	u := newUsersTestUser(t, db, auth.RoleUser, "numeric")

	for _, path := range []string{
		"/api/v1/admin/users/" + u.Username,
		"/api/v1/admin/users/0",
		"/api/v1/admin/users/-1",
		"/api/v1/admin/users/1x",
	} {
		rec := usersCall(t, t.Context(), h, http.MethodGet, path, 0, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d，期望 404；正文：%s", path, rec.Code, rec.Body)
		}
		rec = usersCall(t, t.Context(), h, http.MethodPatch, path, u.ID,
			map[string]string{"status": "suspended"})
		if rec.Code != http.StatusNotFound {
			t.Errorf("PATCH %s 状态码 = %d，期望 404；正文：%s", path, rec.Code, rec.Body)
		}
	}
}

// TestUsersPatchSelfAndLastAdminRejections 拒绝路径逐条钉住。
func TestUsersPatchSelfAndLastAdminRejections(t *testing.T) {
	ctx := t.Context()
	h := usersTestHandler(t, nil)
	db := testDB(t)

	// 先确认这个包的测试库里没有遗留管理员——判据按全表数。
	var existing int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE is_admin AND status = 'active'`).Scan(&existing); err != nil {
		t.Fatalf("统计管理员失败: %v", err)
	}
	if existing != 0 {
		t.Fatalf("测试库里有 %d 个遗留的 active 管理员", existing)
	}

	first := newUsersTestUser(t, db, auth.RoleAdmin, "admin")
	second := newUsersTestUser(t, db, auth.RoleAdmin, "admin")

	patch := func(actor int64, target int64, body any) *httptest.ResponseRecorder {
		return usersCall(t, ctx, h, http.MethodPatch,
			fmt.Sprintf("/api/v1/admin/users/%d", target), actor, body)
	}

	// 1. 改自己：角色与状态各一句，且两句不同。
	roleRec := patch(first.ID, first.ID, map[string]string{"role": "user"})
	if roleRec.Code != http.StatusBadRequest {
		t.Fatalf("改自己角色的状态码 = %d，期望 400；正文：%s", roleRec.Code, roleRec.Body)
	}
	statusRec := patch(first.ID, first.ID, map[string]string{"status": "suspended"})
	if statusRec.Code != http.StatusBadRequest {
		t.Fatalf("改自己状态的状态码 = %d，期望 400；正文：%s", statusRec.Code, statusRec.Body)
	}
	if roleRec.Body.String() == statusRec.Body.String() {
		t.Errorf("改自己角色与改自己状态返回了同一句话：%s", roleRec.Body)
	}

	// 2. 把另一个管理员降成 user：留着一个，所以放行；admin→非 merchant
	//    在 role 形状下就是「降成 user」，不该撞 CHECK 变 500。
	ok := patch(first.ID, second.ID, map[string]string{"role": "user"})
	if ok.Code != http.StatusOK {
		t.Fatalf("降级另一个管理员的状态码 = %d，期望 200；正文：%s", ok.Code, ok.Body)
	}
	var isMerchant, isAdmin bool
	if err := db.QueryRowContext(ctx,
		`SELECT is_merchant, is_admin FROM users WHERE id = $1`, second.ID,
	).Scan(&isMerchant, &isAdmin); err != nil {
		t.Fatalf("读回被降级用户失败: %v", err)
	}
	if isMerchant || isAdmin {
		t.Errorf("降成 user 之后是 (merchant=%v, admin=%v)，期望两个都假", isMerchant, isAdmin)
	}

	// 3. 现在 first 是最后一个 active 管理员：降级、停用、封禁都要拒绝。
	for name, body := range map[string]any{
		"降级": map[string]string{"role": "merchant"},
		"停用": map[string]string{"status": "suspended"},
		"封禁": map[string]string{"status": "banned"},
	} {
		rec := patch(second.ID, first.ID, body)
		if rec.Code != http.StatusConflict {
			t.Errorf("%s最后一个管理员的状态码 = %d，期望 409；正文：%s",
				name, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "最后一个 active 管理员") {
			t.Errorf("%s最后一个管理员的正文没有说清原因：%s", name, rec.Body)
		}
	}

	// 4. 目标不存在。
	missing := patch(first.ID, 999_999_999, map[string]string{"status": "suspended"})
	if missing.Code != http.StatusNotFound {
		t.Errorf("改不存在的用户状态码 = %d，期望 404；正文：%s", missing.Code, missing.Body)
	}

	// 5. 空 body、未知字段、未知取值都是 400，不是 500。
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"空 body": patch(first.ID, second.ID, map[string]string{}),
		"未知字段":   patch(first.ID, second.ID, map[string]string{"nickname": "x"}),
		"未知角色":   patch(first.ID, second.ID, map[string]string{"role": "superuser"}),
		"销户状态":   patch(first.ID, second.ID, map[string]string{"status": "deleted"}),
		"没有操作者": usersCall(t, ctx, h, http.MethodPatch,
			fmt.Sprintf("/api/v1/admin/users/%d", second.ID), 0, map[string]string{"status": "suspended"}),
	} {
		want := http.StatusBadRequest
		if name == "没有操作者" {
			want = http.StatusForbidden
		}
		if rec.Code != want {
			t.Errorf("%s 的状态码 = %d，期望 %d；正文：%s", name, rec.Code, want, rec.Body)
		}
	}
}

// TestUsersPatchRejectsDeletionStatuses 销户流程里的账号不归这一页管。
func TestUsersPatchRejectsDeletionStatuses(t *testing.T) {
	ctx := t.Context()
	h := usersTestHandler(t, nil)
	db := testDB(t)
	actor := newUsersTestUser(t, db, auth.RoleAdmin, "actor")

	requested := newUsersTestUser(t, db, auth.RoleUser, "deleting")
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET status = 'deletion_requested', deletion_prior_status = 'active'
		  WHERE id = $1`, requested.ID); err != nil {
		t.Fatalf("把用户置成 deletion_requested 失败: %v", err)
	}
	rec := usersCall(t, ctx, h, http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/users/%d", requested.ID), actor.ID,
		map[string]string{"status": "active"})
	if rec.Code != http.StatusConflict {
		t.Errorf("改 deletion_requested 用户的状态码 = %d，期望 409；正文：%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "销户") {
		t.Errorf("正文没有指出销户流程：%s", rec.Body)
	}

	deleted := newUsersTestUser(t, db, auth.RoleUser, "deleted")
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET status = 'deleted', deleted_at = now() WHERE id = $1`, deleted.ID); err != nil {
		t.Fatalf("把用户置成 deleted 失败: %v", err)
	}
	rec = usersCall(t, ctx, h, http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/users/%d", deleted.ID), actor.ID,
		map[string]string{"role": "merchant"})
	if rec.Code != http.StatusConflict {
		t.Errorf("改 deleted 用户的状态码 = %d，期望 409；正文：%s", rec.Code, rec.Body)
	}
}

// TestUsersPatchAuditLog PATCH 写一条结构化日志：actor、target、前后值、
// request_id。
func TestUsersPatchAuditLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := usersTestHandler(t, logger)
	db := testDB(t)

	first := newUsersTestUser(t, db, auth.RoleAdmin, "audit")
	second := newUsersTestUser(t, db, auth.RoleAdmin, "audit")

	ctx := observe.WithRequestID(t.Context(), observe.RequestID("test-request-id"))
	rec := usersCall(t, ctx, h, http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/users/%d", second.ID), first.ID,
		map[string]string{"role": "merchant"})
	if rec.Code != http.StatusOK {
		t.Fatalf("修改状态码 = %d；正文：%s", rec.Code, rec.Body)
	}

	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		if parsed["msg"] == "管理员修改用户" {
			entry = parsed
			break
		}
	}
	if entry == nil {
		t.Fatalf("日志里没有审计那一行：%s", buf.String())
	}
	if got := fmt.Sprint(entry["actor_id"]); got != fmt.Sprint(first.ID) {
		t.Errorf("actor_id = %s，期望 %d", got, first.ID)
	}
	if got := fmt.Sprint(entry["target_id"]); got != fmt.Sprint(second.ID) {
		t.Errorf("target_id = %s，期望 %d", got, second.ID)
	}
	if got := fmt.Sprint(entry["request_id"]); got != "test-request-id" {
		t.Errorf("request_id = %s，期望 test-request-id", got)
	}
	changes, ok := entry["changes"].(map[string]any)
	if !ok {
		t.Fatalf("changes 不是对象：%v", entry["changes"])
	}
	if got := fmt.Sprint(changes["role"]); got != "admin -> merchant" {
		t.Errorf("changes.role = %q，期望 admin -> merchant", got)
	}
}

// TestUsersNotOnInternalSurface 内部管理面不能顺手带上 users 组。
func TestUsersNotOnInternalSurface(t *testing.T) {
	h := testSurface(t, Config{
		Authorize: func(next http.Handler) http.Handler { return next },
		Groups:    InternalGroups(),
	})

	rec := call(t, h, http.MethodGet, PathPrefix+"users", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("内部管理面 /users 状态码 = %d，期望 404；正文：%s", rec.Code, rec.Body)
	}
	// 同一份装配下别的资源组是在的：证明 404 来自「没注册 users」，
	// 而不是整个 handler 都没挂上。
	rec = call(t, h, http.MethodGet, PathPrefix+"channels", "", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("内部管理面 /channels 状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
}

// TestUsersRoleChangeTakesEffectOnNextSessionRead 角色改动立即生效：控制台
// 每次请求都从 users 折角色，不靠吊销会话。这里用 SessionByToken 走一遍
// 中间件读的那一条路（同一份 JOIN）。
func TestUsersRoleChangeTakesEffectOnNextSessionRead(t *testing.T) {
	ctx := t.Context()
	h := usersTestHandler(t, nil)
	db := testDB(t)
	store := auth.New(db)

	actor := newUsersTestUser(t, db, auth.RoleAdmin, "rolechange")
	target := newUsersTestUser(t, db, auth.RoleAdmin, "rolechange")
	_, token, err := store.CreateSession(ctx, target.ID, "test-agent", nil)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	rec := usersCall(t, ctx, h, http.MethodPatch,
		fmt.Sprintf("/api/v1/admin/users/%d", target.ID), actor.ID,
		map[string]string{"role": "user"})
	if rec.Code != http.StatusOK {
		t.Fatalf("降级状态码 = %d；正文：%s", rec.Code, rec.Body)
	}

	got, err := store.SessionByToken(ctx, token)
	if err != nil {
		t.Fatalf("按令牌读会话失败: %v", err)
	}
	if got.Role != auth.RoleUser {
		t.Errorf("下一次请求读到的角色 = %q，期望 %q", got.Role, auth.RoleUser)
	}
	if got.RevokedAt != nil {
		t.Error("改角色吊销了会话；它应当立即生效而不必重登")
	}
}
