package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ratelimit"
)

// fakeLimiter 是一个可编程的限流器，用来把「放行/拒绝」这一侧固定住。
//
// 它同时记下被调用时收到的参数：限流按 key 计，入口必须把**这把 key** 的
// id 与额度传下去，传错（例如传用户 ID、或传了另一个字段）时判定结果
// 在别的用例里看不出来。
type fakeLimiter struct {
	allowed    bool
	retryAfter time.Duration

	calls int
	keyID int64
	rpm   *int
}

func (f *fakeLimiter) Allow(keyID int64, rpm *int) (bool, time.Duration) {
	f.calls++
	f.keyID = keyID
	f.rpm = rpm
	return f.allowed, f.retryAfter
}

// rateLimitBody 是两套协议共同的错误体形状里本文件要断言的部分。
type rateLimitBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestRateLimitedRequestIsRejectedBeforeUpstream 钉住超限时的三件事：
// 429、Retry-After、以及**没有走到上游**。
//
// 第三件是这一条里最要紧的：上游那一侧就是预留发生的地方，被限流的请求
// 走到那里等于先把余额冻住、再把它拒掉。
func TestRateLimitedRequestIsRejectedBeforeUpstream(t *testing.T) {
	limit := 60

	cases := []struct {
		name        string
		path        string
		wantTopType string
	}{
		{"OpenAI 协议", PathChatCompletions, ""},
		{"Anthropic 协议", PathMessages, "error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, auth, chatter := newTestHandler()
			auth.user = User{ID: 7, KeyID: 42, RateLimitRPM: &limit}
			limiter := &fakeLimiter{allowed: false, retryAfter: 30 * time.Second}
			h.RateLimiter = limiter

			rec := post(h, tc.path, validBody)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("状态码 = %d，期望 429。响应体：%s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get(HeaderRetryAfter); got != "30" {
				t.Errorf("Retry-After = %q，期望 %q", got, "30")
			}
			if chatter.calls != 0 {
				t.Errorf("被限流的请求走到了上游 %d 次：超限的请求不该先冻住余额", chatter.calls)
			}
			if auth.calls != 1 {
				t.Errorf("鉴权调用 %d 次，期望 1：限流按 key 计，必须先知道是哪把 key", auth.calls)
			}
			if limiter.calls != 1 {
				t.Fatalf("限流器被调用 %d 次，期望 1", limiter.calls)
			}
			// 传下去的是**这把 key** 的 id 与额度，不是用户 id，也不是别的字段。
			if limiter.keyID != 42 {
				t.Errorf("限流器收到的 keyID = %d，期望 42", limiter.keyID)
			}
			if limiter.rpm == nil || *limiter.rpm != 60 {
				t.Errorf("限流器收到的 rpm = %v，期望指向 60", limiter.rpm)
			}

			// 错误体按调用协议给形状：调用方的 SDK 按自己那套解，形状不对
			// 时它连错误类型都读不出来。
			var body rateLimitBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是 JSON: %v（%s）", err, rec.Body)
			}
			if body.Type != tc.wantTopType {
				t.Errorf("顶层 type = %q，期望 %q", body.Type, tc.wantTopType)
			}
			if body.Error.Type != "rate_limit_error" {
				t.Errorf("error.type = %q，期望 rate_limit_error", body.Error.Type)
			}
			if body.Error.Message == "" {
				t.Error("错误体里应当有一句给调用方的话")
			}
		})
	}
}

// TestAllowedRequestProceeds 限流放行时请求照常走完。
//
// 与上一条配对：只测「拒绝」的话，一个永远返回拒绝的实现也能全绿。
func TestAllowedRequestProceeds(t *testing.T) {
	h, auth, chatter := newTestHandler()
	limit := 60
	auth.user = User{ID: 7, KeyID: 42, RateLimitRPM: &limit}
	limiter := &fakeLimiter{allowed: true}
	h.RateLimiter = limiter

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}
	if chatter.calls != 1 {
		t.Errorf("上游被调用 %d 次，期望 1", chatter.calls)
	}
	if limiter.calls != 1 {
		t.Errorf("限流器被调用 %d 次，期望 1", limiter.calls)
	}
}

// TestUnauthenticatedRequestIsNotCounted 没通过鉴权的请求不消耗额度。
//
// 判据是「限流器一次都没被调用」：它要是被调了，一个没有凭证的请求就能
// 把某个 key 的额度打满——而它连是哪把 key 都还不知道。
func TestUnauthenticatedRequestIsNotCounted(t *testing.T) {
	h, auth, _ := newTestHandler()
	auth.err = errors.New("凭证无效")
	limiter := &fakeLimiter{allowed: true}
	h.RateLimiter = limiter

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", rec.Code)
	}
	if limiter.calls != 0 {
		t.Errorf("鉴权失败的请求调用了限流器 %d 次，期望 0", limiter.calls)
	}
}

// TestWithoutLimiterRequestsPass 没装限流器时不限流，也不 panic。
//
// 这条保护的是「装配时忘了接」这条路径：它必须是**不限流**，而不是
// 每个请求都 429 或者崩掉——后者会让一次漏接变成全站不可用。
func TestWithoutLimiterRequestsPass(t *testing.T) {
	h, _, chatter := newTestHandler()

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}
	if chatter.calls != 1 {
		t.Errorf("上游被调用 %d 次，期望 1", chatter.calls)
	}
}

// TestRetryAfterIsAtLeastOneSecond 钉住 Retry-After 的下限。
//
// 写 0 会让客户端立刻重试，而它必然还在同一个窗口里，白白多一次往返。
// 取整方向同样是契约的一部分：向上取整，否则客户端会在窗口结束前重试。
func TestRetryAfterIsAtLeastOneSecond(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter time.Duration
		want       string
	}{
		{"零", 0, "1"},
		{"不足一秒", 200 * time.Millisecond, "1"},
		{"有零头向上取整", 2500 * time.Millisecond, "3"},
		{"整秒", 30 * time.Second, "30"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			h.RateLimiter = &fakeLimiter{allowed: false, retryAfter: tc.retryAfter}

			rec := post(h, PathChatCompletions, validBody)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("状态码 = %d，期望 429", rec.Code)
			}
			if got := rec.Header().Get(HeaderRetryAfter); got != tc.want {
				t.Errorf("Retry-After = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestSixtyPerMinuteAtHTTPLevel 走一遍真实的限流实现，断言具体数字。
//
// 前一条用的是可编程的桩，只证明入口按限流器的答复给响应；这一条用真的
// 计数器，证明「额度 60 → 前 60 次 200、第 61 次 429」这件事在 HTTP 这一层
// 成立。时钟是拨的，不 sleep。
func TestSixtyPerMinuteAtHTTPLevel(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC)

	h, auth, chatter := newTestHandler()
	limit := 60
	auth.user = User{ID: 7, KeyID: 42, RateLimitRPM: &limit}
	h.RateLimiter = ratelimit.New(func() time.Time { return now })

	for i := 1; i <= 60; i++ {
		if rec := post(h, PathChatCompletions, validBody); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次状态码 = %d，期望 200。响应体：%s", i, rec.Code, rec.Body)
		}
	}

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 61 次状态码 = %d，期望 429。响应体：%s", rec.Code, rec.Body)
	}
	// 12:00:30 被拒 → 当前窗口还剩 30 秒。
	if got := rec.Header().Get(HeaderRetryAfter); got != "30" {
		t.Errorf("Retry-After = %q，期望 %q", got, "30")
	}
	if chatter.calls != 60 {
		t.Errorf("上游被调用 %d 次，期望 60（第 61 次不该走到上游）", chatter.calls)
	}
}
