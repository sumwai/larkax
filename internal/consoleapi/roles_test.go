package consoleapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
)

// addRoleSession 给替身会话表加一条指定角色的会话。
func (f *consoleFixture) addRoleSession(token string, id, userID int64, role auth.Role) {
	f.accounts.infos[userID] = auth.UserInfo{
		ID: userID, Username: fmt.Sprintf("u%d", userID), Status: auth.UserStatusActive,
		Role: role, CreatedAt: testNow,
	}
	f.sessions.add(token, auth.Session{
		ID: id, UserID: userID, CSRFToken: "csrf-" + token,
		CreatedAt: testNow, LastSeenAt: testNow,
		ExpiresAt:  testNow.Add(auth.SessionIdleTTL),
		UserStatus: auth.UserStatusActive, Role: role,
	})
}

// TestRoleGroupsGateBySessionRole 钉住三档角色 × 两组路由的放行与拒绝。
//
// 判据只读会话里那一列：替身会话带着角色，请求本身不带任何「当前视图」。
func TestRoleGroupsGateBySessionRole(t *testing.T) {
	f := newFixture(t)
	f.addRoleSession("user-token", 10, 100, auth.RoleUser)
	f.addRoleSession("merchant-token", 11, 101, auth.RoleMerchant)
	f.addRoleSession("admin-token", 12, 102, auth.RoleAdmin)

	groups := []struct {
		path string
		// want 是 user / merchant / admin 各自该拿到的状态码。
		user, merchant, admin int
	}{
		{"/api/v1/merchant/channels", http.StatusForbidden, http.StatusNoContent, http.StatusNoContent},
		{"/api/v1/merchant/overview", http.StatusForbidden, http.StatusNoContent, http.StatusNoContent},
		{"/api/v1/merchant/upstreams", http.StatusForbidden, http.StatusNoContent, http.StatusNoContent},
		{"/api/v1/merchant/models", http.StatusForbidden, http.StatusNoContent, http.StatusNoContent},
		// 渠道表单的输入：适配器枚举跟着渠道组走。
		{"/api/v1/merchant/adapters", http.StatusForbidden, http.StatusNoContent, http.StatusNoContent},
		{"/api/v1/admin/channels", http.StatusForbidden, http.StatusForbidden, http.StatusNoContent},
		{"/api/v1/admin/adapters", http.StatusForbidden, http.StatusForbidden, http.StatusNoContent},
		// 用户管理也是 admin 组的一条：普通用户与 merchant 都不该进得去。
		{"/api/v1/admin/users", http.StatusForbidden, http.StatusForbidden, http.StatusNoContent},
	}

	for _, group := range groups {
		t.Run(group.path, func(t *testing.T) {
			cases := []struct {
				token string
				want  int
			}{
				{"", http.StatusUnauthorized},
				{"user-token", group.user},
				{"merchant-token", group.merchant},
				{"admin-token", group.admin},
			}
			for _, tc := range cases {
				rec := f.requestAsWith(t, "127.0.0.1:8080",
					map[string]string{"Cookie": auth.SessionCookieName + "=" + tc.token},
					http.MethodGet, group.path, "")
				if rec.Code != tc.want {
					t.Errorf("token %q 访问 %s 的状态码 = %d，期望 %d；正文：%s",
						tc.token, group.path, rec.Code, tc.want, rec.Body)
				}
			}
		})
	}
}

// TestForbiddenBodyDoesNotNameTheRole 钉住 403 的正文固定：不说缺哪个角色，
// 也不说这条路径是什么。前端只按状态码分支，正文给人看。
func TestForbiddenBodyDoesNotNameTheRole(t *testing.T) {
	f := newFixture(t)
	f.addRoleSession("user-token", 10, 100, auth.RoleUser)

	rec := f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Cookie": auth.SessionCookieName + "=user-token"},
		http.MethodGet, "/api/v1/admin/channels", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403", rec.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v", err)
	}
	if body.Error.Code != codeForbidden {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, codeForbidden)
	}
	// 字面量而不是 msgForbidden 常量：拿常量比时，把那一句改成点名角色的
	// 版本，用例照样绿——而「不回显角色」正是这条判据本身。
	const wantMessage = "You do not have permission to perform this action."
	if body.Error.Message != wantMessage {
		t.Errorf("正文 = %q，期望固定那一句 %q", body.Error.Message, wantMessage)
	}
	for _, leaked := range []string{"admin", "merchant", "user"} {
		if strings.Contains(strings.ToLower(body.Error.Message), leaked) {
			t.Errorf("403 正文回显了角色词 %q：%s", leaked, body.Error.Message)
		}
	}
}

// TestMerchantGroupScopesOwnership 钉住归属只注入给 merchant 组里的 merchant：
// admin 在这一组里是「看全部」，不能被当成某个用户。
func TestMerchantGroupScopesOwnership(t *testing.T) {
	f := newFixture(t)
	f.addRoleSession("merchant-token", 11, 101, auth.RoleMerchant)
	f.addRoleSession("admin-token", 12, 102, auth.RoleAdmin)

	rec := f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Cookie": auth.SessionCookieName + "=merchant-token"},
		http.MethodGet, "/api/v1/merchant/channels", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("merchant 的状态码 = %d，期望 204；正文：%s", rec.Code, rec.Body)
	}
	if f.merchantOwner != 101 {
		t.Errorf("merchant 组注入的归属 = %d，期望会话用户 101", f.merchantOwner)
	}

	// admin 进 merchant 组：放行，但不带归属。
	f.merchantOwner = -1
	rec = f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Cookie": auth.SessionCookieName + "=admin-token"},
		http.MethodGet, "/api/v1/merchant/channels", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin 在 merchant 组的状态码 = %d，期望 204", rec.Code)
	}
	if f.merchantOwner != 0 {
		t.Errorf("admin 在 merchant 组被注入了归属 %d；它应当看全部", f.merchantOwner)
	}

	// admin 组同样不带归属（注入只发生在 merchant 组里的 merchant）。
	f.adminOwner = -1
	rec = f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Cookie": auth.SessionCookieName + "=admin-token"},
		http.MethodGet, "/api/v1/admin/channels", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin 组的状态码 = %d，期望 204", rec.Code)
	}
	if f.adminAdminCalls != 1 {
		t.Errorf("admin 组资源 handler 被调用 %d 次，期望 1", f.adminAdminCalls)
	}
	if f.adminOwner != 0 {
		t.Errorf("admin 组被注入了归属 %d；它不按归属过滤", f.adminOwner)
	}
	// 与归属相反，操作者**要**注入：用户管理判「是不是在改自己」、审计日志
	// 记谁改的，都只认这一个值（请求头与 body 不参与）。
	if f.adminActor != 102 {
		t.Errorf("admin 组注入的操作者 = %d，期望会话用户 102", f.adminActor)
	}
}

// TestAdminGroupRequiresCSRFForUserPatch 钉住用户管理的写方法也过 CSRF：
// 它是 admin 组下的 PATCH，与渠道、计价那些写路径同一道门。
func TestAdminGroupRequiresCSRFForUserPatch(t *testing.T) {
	f := newFixture(t)
	f.addRoleSession("admin-token", 12, 102, auth.RoleAdmin)

	missing := f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Cookie": auth.SessionCookieName + "=admin-token"},
		http.MethodPatch, "/api/v1/admin/users/1", `{"status":"suspended"}`)
	if missing.Code != http.StatusForbidden {
		t.Fatalf("不带 CSRF 的 PATCH 状态码 = %d，期望 403；正文：%s", missing.Code, missing.Body)
	}
	if f.adminAdminCalls != 0 {
		t.Errorf("CSRF 没过的请求进到了资源 handler（调用 %d 次）", f.adminAdminCalls)
	}

	ok := f.requestAsWith(t, "127.0.0.1:8080", map[string]string{
		"Cookie":       auth.SessionCookieName + "=admin-token",
		"X-CSRF-Token": "csrf-admin-token",
	}, http.MethodPatch, "/api/v1/admin/users/1", `{"status":"suspended"}`)
	if ok.Code != http.StatusNoContent {
		t.Fatalf("过了 CSRF 的 PATCH 状态码 = %d，期望 204；正文：%s", ok.Code, ok.Body)
	}
	if f.adminAdminCalls != 1 {
		t.Errorf("资源 handler 被调用 %d 次，期望 1", f.adminAdminCalls)
	}
}

// TestRoleGroupsRequireCSRFForWrites 钉住写方法在放行角色之后仍要过 CSRF。
func TestRoleGroupsRequireCSRFForWrites(t *testing.T) {
	f := newFixture(t)
	f.addRoleSession("merchant-token", 11, 101, auth.RoleMerchant)

	missing := f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Cookie": auth.SessionCookieName + "=merchant-token"},
		http.MethodPost, "/api/v1/merchant/channels", `{}`)
	if missing.Code != http.StatusForbidden {
		t.Fatalf("不带 CSRF 的写请求状态码 = %d，期望 403；正文：%s", missing.Code, missing.Body)
	}
	if f.merchantAdminCalls != 0 {
		t.Errorf("CSRF 没过的请求进到了资源 handler（调用 %d 次）", f.merchantAdminCalls)
	}

	ok := f.requestAsWith(t, "127.0.0.1:8080", map[string]string{
		"Cookie":       auth.SessionCookieName + "=merchant-token",
		"X-CSRF-Token": "csrf-merchant-token",
	}, http.MethodPost, "/api/v1/merchant/channels", `{}`)
	if ok.Code != http.StatusNoContent {
		t.Fatalf("过了 CSRF 的写请求状态码 = %d，期望 204；正文：%s", ok.Code, ok.Body)
	}
	if f.merchantAdminCalls != 1 {
		t.Errorf("资源 handler 被调用 %d 次，期望 1", f.merchantAdminCalls)
	}
}

// TestSessionResponseCarriesRole 钉住 session 响应与 /me 响应里的 role 是三种
// 取值之一，且取自服务端那一份。
func TestSessionResponseCarriesRole(t *testing.T) {
	f := newFixture(t)
	f.addRoleSession("user-token", 10, 100, auth.RoleUser)
	f.addRoleSession("merchant-token", 11, 101, auth.RoleMerchant)
	f.addRoleSession("admin-token", 12, 102, auth.RoleAdmin)

	cases := []struct {
		token string
		want  auth.Role
	}{
		{"user-token", auth.RoleUser},
		{"merchant-token", auth.RoleMerchant},
		{"admin-token", auth.RoleAdmin},
	}
	for _, tc := range cases {
		t.Run(string(tc.want), func(t *testing.T) {
			rec := f.requestAsWith(t, "127.0.0.1:8080",
				map[string]string{"Cookie": auth.SessionCookieName + "=" + tc.token},
				http.MethodGet, "/api/v1/auth/session", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
			}
			var body struct {
				User struct {
					Role auth.Role `json:"role"`
				} `json:"user"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应体不是 JSON: %v", err)
			}
			if body.User.Role != tc.want {
				t.Errorf("role = %q，期望 %q", body.User.Role, tc.want)
			}
		})
	}
}

// TestRoleAllowsIsTheOnlyOrdering 钉住那一条序：admin ⊇ merchant ⊇ user，
// 未知的 required 拒绝。
func TestRoleAllowsIsTheOnlyOrdering(t *testing.T) {
	cases := []struct {
		have, required auth.Role
		want           bool
	}{
		{auth.RoleUser, auth.RoleUser, true},
		{auth.RoleMerchant, auth.RoleUser, true},
		{auth.RoleAdmin, auth.RoleUser, true},
		{auth.RoleUser, auth.RoleMerchant, false},
		{auth.RoleMerchant, auth.RoleMerchant, true},
		{auth.RoleAdmin, auth.RoleMerchant, true},
		{auth.RoleUser, auth.RoleAdmin, false},
		{auth.RoleMerchant, auth.RoleAdmin, false},
		{auth.RoleAdmin, auth.RoleAdmin, true},
		// 拼错的 required 不能放开。
		{auth.RoleAdmin, auth.Role("superuser"), false},
	}
	for _, tc := range cases {
		if got := tc.have.Allows(tc.required); got != tc.want {
			t.Errorf("%q.Allows(%q) = %v，期望 %v", tc.have, tc.required, got, tc.want)
		}
	}
}

// TestRoleSessionExpiryStillApplies 钉住角色放行不绕过会话有效期。
func TestRoleSessionExpiryStillApplies(t *testing.T) {
	f := newFixture(t)
	f.sessions.add("expired-admin", auth.Session{
		ID: 20, UserID: 200, CSRFToken: "csrf-old",
		CreatedAt:  testNow.Add(-40 * 24 * time.Hour),
		ExpiresAt:  testNow.Add(-time.Second),
		UserStatus: auth.UserStatusActive, Role: auth.RoleAdmin,
	})
	rec := f.requestAsWith(t, "127.0.0.1:8080",
		map[string]string{"Cookie": auth.SessionCookieName + "=expired-admin"},
		http.MethodGet, "/api/v1/admin/channels", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("过期会话的状态码 = %d，期望 401", rec.Code)
	}
}

// TestLoginResponseCarriesRole 钉住登录响应里的 role：前端登录之后直接拿
// session.user，第一次登录就必须看得到运营视图。
//
// 这条与 TestSessionResponseCarriesRole 是两条路：登录走 CreateSession，
// 它的 RETURNING 不带 users 的两个标记；只测 /auth/session 盖不住这一条。
func TestLoginResponseCarriesRole(t *testing.T) {
	f := newFixture(t)
	hash, err := auth.HashPassword(testKDF, "merchant password")
	if err != nil {
		t.Fatalf("生成口令哈希失败: %v", err)
	}
	f.accounts.put(auth.LoginUser{
		ID: 21, Username: "merchant", Status: auth.UserStatusActive,
		PasswordHash: hash,
	}, auth.UserInfo{
		ID: 21, Username: "merchant", Status: auth.UserStatusActive,
		Role: auth.RoleMerchant, CreatedAt: testNow.Add(-24 * time.Hour),
	})

	rec := f.request(t, http.MethodPost, PathPrefix+"auth/login",
		`{"username":"merchant","password":"merchant password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		User struct {
			Role auth.Role `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON: %v", err)
	}
	if body.User.Role != auth.RoleMerchant {
		t.Errorf("登录响应的 role = %q，期望 %q", body.User.Role, auth.RoleMerchant)
	}
}
