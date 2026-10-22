package adminapi

// 本文件是探测那两个端点里**不需要数据库与网络**的那一半：限流窗口、
// 错误体的截断与状态码映射。走真 HTTP 的端到端用例见
// probe_integration_test.go。

import (
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// TestProbeLimiter 钉住三条：两次之间要隔 probeInterval、一分钟最多
// probePerMinute 次、不同调用方各算各的。
func TestProbeLimiter(t *testing.T) {
	l := newProbeLimiter()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	if ok, _ := l.allow(1, now); !ok {
		t.Fatal("第一次就被挡了")
	}
	if ok, wait := l.allow(1, now.Add(time.Second)); ok || wait != time.Second {
		t.Errorf("一秒后再来 = ok %v / wait %v，期望挡下并等 1s", ok, wait)
	}
	if ok, _ := l.allow(1, now.Add(probeInterval)); !ok {
		t.Error("隔满 probeInterval 后被挡了")
	}
	// 不同的调用方互不影响。
	if ok, _ := l.allow(2, now.Add(time.Second)); !ok {
		t.Error("另一个调用方被前一个的请求挡了")
	}

	// 一分钟内第 probePerMinute 次之后要挡，且等待量是「最早那一格滑出窗口」。
	l = newProbeLimiter()
	base := now
	for i := range probePerMinute {
		at := base.Add(time.Duration(i) * probeInterval)
		if ok, _ := l.allow(1, at); !ok {
			t.Fatalf("第 %d 次就被挡了", i+1)
		}
	}
	last := base.Add(time.Duration(probePerMinute-1) * probeInterval)
	ok, wait := l.allow(1, last.Add(probeInterval))
	if ok {
		t.Fatalf("一分钟内第 %d 次之后还被放行", probePerMinute+1)
	}
	// 最早那一格在 base，滑出窗口需要到 base+1min。
	if want := base.Add(time.Minute).Sub(last.Add(probeInterval)); wait != want {
		t.Errorf("等待 = %v，期望 %v（等最早那一格滑出窗口）", wait, want)
	}
}

// TestTruncateRunes 按字符截断：多字节字符不会被切成半个。
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abc", 3); got != "abc" {
		t.Errorf("恰好够长被改了: %q", got)
	}
	if got := truncateRunes("abcdef", 3); got != "abc…" {
		t.Errorf("截断结果 = %q，期望 abc…", got)
	}
	if got := truncateRunes("错误体内容", 2); got != "错误…" {
		t.Errorf("中文截断结果 = %q，期望 错误…", got)
	}
}

// TestUpstreamFailureMessage 502 的正文：有响应就给状态码与截短的原文，
// 没响应只说调用失败。
func TestUpstreamFailureMessage(t *testing.T) {
	cases := []struct {
		name string
		up   channel.ErrUpstream
		want string
	}{
		{
			"有响应体",
			channel.ErrUpstream{StatusCode: 401, GotResponse: true, Body: `{"error":"bad key"}`},
			`the upstream returned 401: {"error":"bad key"}`,
		},
		{
			"空响应体",
			channel.ErrUpstream{StatusCode: 500, GotResponse: true},
			"the upstream returned 500",
		},
		{
			"连不上",
			channel.ErrUpstream{Err: errFake("dial tcp: refused")},
			"the upstream call failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamFailureMessage(&tc.up); got != tc.want {
				t.Errorf("正文 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestUpstreamFailureMessageTruncates 长的错误体不会整段回给浏览器。
func TestUpstreamFailureMessageTruncates(t *testing.T) {
	up := channel.ErrUpstream{
		StatusCode:  502,
		GotResponse: true,
		Body:        strings.Repeat("x", upstreamBodySnippet+50),
	}
	got := upstreamFailureMessage(&up)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("超长正文没有截断: %d 个字符", len(got))
	}
	if strings.Count(got, "x") != upstreamBodySnippet {
		t.Errorf("截断长度 = %d，期望 %d", strings.Count(got, "x"), upstreamBodySnippet)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
