package adminapi

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
)

// 本文件只测能脱离数据库判断的部分：路径 id、邮箱打码、游标编解码、请求体
// 到 auth.UpdateUserParams 的折算。要真库的拒绝路径在 users_integration_test.go。

func TestParseUserID(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"42", 42, true},
		// 前导零仍是同一个正整数：id 是数字，不是字符串。
		{"007", 7, true},
		{"", 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"1.0", 0, false},
		{"1x", 0, false},
		{"alice", 0, false},
		// 溢出：ParseInt 报错，不能截断成另一个 id——那会改到别人。
		{"9223372036854775808", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseUserID(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseUserID(%q) = (%d, %v)，期望 (%d, %v)",
				tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" 表示期望 null
	}{
		{"alice@example.com", "a***@example.com"},
		// 单字母的本地部分也保留那一个字符，不额外多留一位。
		{"a@example.com", "a***@example.com"},
		// 多字节首字符按 rune 取，不能切出一个半个汉字。
		{"张三@example.com", "张***@example.com"},
		{"", ""},
		// 没有 @ 的值整段打码：它可能不是邮箱，但也不该原样出去。
		{"not-an-email", "***"},
		{"@example.com", "***"},
	}
	for _, tc := range cases {
		got := maskEmail(tc.in)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("maskEmail(%q) = %q，期望 null", tc.in, *got)
		case tc.want != "" && got == nil:
			t.Errorf("maskEmail(%q) = null，期望 %q", tc.in, tc.want)
		case tc.want != "" && *got != tc.want:
			t.Errorf("maskEmail(%q) = %q，期望 %q", tc.in, *got, tc.want)
		}
	}
}

func TestUserCursorRoundTrip(t *testing.T) {
	for _, id := range []int64{1, 7, 1 << 40} {
		cursor := encodeUserCursor(id)
		got, err := decodeUserCursor(cursor)
		if err != nil {
			t.Fatalf("解自造游标 %q 失败: %v", cursor, err)
		}
		if got != id {
			t.Errorf("游标 %q 解出 %d，期望 %d", cursor, got, id)
		}
	}
}

func TestDecodeUserCursorRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"!!!",
		// 合法 base64、但没有版本前缀：那是另一种编码，不能按 id 解释。
		"MTIz",
		// 版本对、id 非正。
		encodeRaw("u1:0"),
		encodeRaw("u1:-3"),
		encodeRaw("u1:abc"),
	}
	for _, raw := range cases {
		if _, err := decodeUserCursor(raw); err == nil {
			t.Errorf("decodeUserCursor(%q) 没有报错", raw)
		}
	}
}

// encodeRaw 是测试里造「形状合法、内容非法」的游标用的。
func encodeRaw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestParseUserLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		ok   bool
	}{
		{"", userListDefaultLimit, true},
		{"1", 1, true},
		{"200", 200, true},
		{"0", 0, false},
		{"201", 0, false},
		{"-1", 0, false},
		{"ten", 0, false},
	}
	for _, tc := range cases {
		got, err := parseUserLimit(tc.raw)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseUserLimit(%q) = (%d, %v)，期望 (%d, ok=%v)",
				tc.raw, got, err, tc.want, tc.ok)
		}
	}
}

func TestUserUpdateParamsFoldsRoleIntoBothFlags(t *testing.T) {
	role := "admin"
	p, err := userUpdateParams(userUpdateRequest{Role: &role})
	if err != nil {
		t.Fatalf("折算 admin 失败: %v", err)
	}
	if p.IsMerchant == nil || !*p.IsMerchant || p.IsAdmin == nil || !*p.IsAdmin {
		t.Fatalf("admin 折成 (merchant=%v, admin=%v)，期望两个都真",
			showBool(p.IsMerchant), showBool(p.IsAdmin))
	}

	// 形状上就表达不出「admin 且非 merchant」：两个列总是一起给。
	user := "user"
	p, err = userUpdateParams(userUpdateRequest{Role: &user})
	if err != nil {
		t.Fatalf("折算 user 失败: %v", err)
	}
	if p.IsMerchant == nil || *p.IsMerchant || p.IsAdmin == nil || *p.IsAdmin {
		t.Errorf("user 折成 (merchant=%v, admin=%v)，期望两个都假",
			showBool(p.IsMerchant), showBool(p.IsAdmin))
	}
}

func TestUserUpdateParamsRejectsEmptyAndUnknown(t *testing.T) {
	if _, err := userUpdateParams(userUpdateRequest{}); !errors.Is(err, auth.ErrNoChanges) {
		t.Errorf("空 body 报 %v，期望 ErrNoChanges", err)
	}

	badRole := "superuser"
	if _, err := userUpdateParams(userUpdateRequest{Role: &badRole}); err == nil {
		t.Error("未知角色没有报错")
	}

	// deletion_requested / deleted 不在 PATCH 的取值里：它们只能走销户流程，
	// 用 update 写进去会跳过 delete revert 那条判据。
	for _, bad := range []string{"deleted", "deletion_requested", ""} {
		status := bad
		if _, err := userUpdateParams(userUpdateRequest{Status: &status}); err == nil {
			t.Errorf("状态 %q 没有报错", bad)
		}
	}
}

func TestInternalGroupsExcludesUsers(t *testing.T) {
	groups := InternalGroups()
	seen := make(map[string]bool, len(groups))
	for _, g := range groups {
		seen[g] = true
		if g == GroupUsers {
			t.Fatal("内部管理面不能注册 users 组：它的判据依赖会话里的操作者")
		}
	}
	for _, want := range []string{GroupChannels, GroupModels, GroupAliases, GroupPricing, GroupCosts} {
		if !seen[want] {
			t.Errorf("内部管理面缺了 %q 组", want)
		}
	}
}

func showBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
