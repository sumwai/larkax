package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCredentialFrom(t *testing.T) {
	// 用不到真实 key，取值是占位符：这一层只看头的形状。
	const key = "test-key-placeholder"

	cases := []struct {
		name    string
		headers map[string]string
		want    string
		reason  Reason
	}{
		{
			name:    "Bearer 方案",
			headers: map[string]string{"Authorization": "Bearer " + key},
			want:    key,
		},
		{
			name:    "方案名大小写不敏感",
			headers: map[string]string{"Authorization": "bearer " + key},
			want:    key,
		},
		{
			name:    "多空格",
			headers: map[string]string{"Authorization": "Bearer   " + key},
			want:    key,
		},
		{
			name:    "x-api-key",
			headers: map[string]string{"x-api-key": key},
			want:    key,
		},
		{
			name:    "x-api-key 首尾空白",
			headers: map[string]string{"x-api-key": "  " + key + "\n"},
			want:    key,
		},
		{
			name:    "x-api-key 头名大小写",
			headers: map[string]string{"X-Api-Key": key},
			want:    key,
		},
		{
			name:    "没有凭证",
			headers: map[string]string{},
			reason:  ReasonMissingCredential,
		},
		{
			name:    "x-api-key 只有空白",
			headers: map[string]string{"x-api-key": "   "},
			reason:  ReasonMissingCredential,
		},
		{
			name:    "Authorization 没有方案名",
			headers: map[string]string{"Authorization": key},
			reason:  ReasonMalformedCredential,
		},
		{
			name:    "Authorization 用了别的方案",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
			reason:  ReasonMalformedCredential,
		},
		{
			name:    "Bearer 后面没有值",
			headers: map[string]string{"Authorization": "Bearer"},
			reason:  ReasonMalformedCredential,
		},
		{
			name:    "Bearer 后面有多个段",
			headers: map[string]string{"Authorization": "Bearer " + key + " extra"},
			reason:  ReasonMalformedCredential,
		},
		{
			// 两个头同时出现且 Authorization 是坏的：报格式错，而不是
			// 悄悄改用 x-api-key。静默回退会让「换的 key 没生效」无从查起。
			name: "坏 Authorization 不退回 x-api-key",
			headers: map[string]string{
				"Authorization": "Basic dXNlcjpwYXNz",
				"x-api-key":     key,
			},
			reason: ReasonMalformedCredential,
		},
		{
			// 反过来：Authorization 好用时以它为准。
			name: "两个头都在时以 Authorization 为准",
			headers: map[string]string{
				"Authorization": "Bearer " + key,
				"x-api-key":     "test-key-other-placeholder",
			},
			want: key,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}

			got, err := credentialFrom(r)
			if tc.reason != "" {
				assertReason(t, err, tc.reason)
				return
			}
			if err != nil {
				t.Fatalf("取凭证失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("取到 %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestAuthenticateWithoutCredentialSkipsDatabase 验证「没有凭证就不查库」。
//
// 用一个 nil 的 *sql.DB：只要它被碰一下就会 panic，所以这条用例通过
// 就证明鉴权在取不到凭证时确实提前返回了。
//
// 为什么值得测：不带凭证的请求是最容易被反复打的那一类（配置错了的客户端
// 会一直重试），让它每个都产生一次数据库往返，是把自己的库交给对端决定。
func TestAuthenticateWithoutCredentialSkipsDatabase(t *testing.T) {
	store := New(nil)

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", nil)
	_, err := store.Authenticate(t.Context(), r)
	assertReason(t, err, ReasonMissingCredential)
}

// TestErrorHidesReason 钉住「对外不区分」这件事。
//
// 它不是措辞问题：调用方很自然会把 err.Error() 写进响应体，
// 而「这个 key 存在但过期了」正是试探者想知道的东西。
func TestErrorHidesReason(t *testing.T) {
	reasons := []Reason{
		ReasonMissingCredential,
		ReasonMalformedCredential,
		ReasonKeyNotFound,
		ReasonKeyDisabled,
		ReasonKeyRevoked,
		ReasonKeyInactive,
		ReasonKeyExpired,
		ReasonUserInactive,
	}

	for _, reason := range reasons {
		err := &Error{Reason: reason}

		if err.Error() != ErrUnauthenticated.Error() {
			t.Errorf("Reason %q 的文案是 %q，应当与 ErrUnauthenticated 完全一致",
				reason, err.Error())
		}
		if !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Reason %q 的错误不满足 errors.Is(err, ErrUnauthenticated)", reason)
		}

		// 消费侧必须拿得到原因，否则「排查时要区分」只是句空话。
		var target *Error
		if !errors.As(err, &target) {
			t.Fatalf("Reason %q 的错误无法用 errors.As 取出", reason)
		}
		if target.Reason != reason {
			t.Errorf("取出的 Reason = %q，期望 %q", target.Reason, reason)
		}
	}
}

// assertReason 断言 err 是一次带指定原因的认证失败。
func assertReason(t *testing.T, err error, want Reason) {
	t.Helper()

	if err == nil {
		t.Fatalf("期望认证失败（原因 %q），却成功了", want)
	}
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("错误 %v 不满足 errors.Is(err, ErrUnauthenticated)", err)
	}
	var target *Error
	if !errors.As(err, &target) {
		t.Fatalf("错误 %v 不是 *Error", err)
	}
	if target.Reason != want {
		t.Fatalf("失败原因 = %q，期望 %q", target.Reason, want)
	}
}
