package adminapi

// 本文件是探测那两个端点里**不需要数据库与网络**的那一半：限流窗口、
// 错误体的截断与状态码映射。走真 HTTP 的端到端用例见
// probe_integration_test.go。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/channels"
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

// failProbeResult 把一份错误交给 failProbe，返回状态码与正文里那句话。
func failProbeResult(t *testing.T, owner *int64, err error) (int, string) {
	t.Helper()
	ctx := context.Background()
	if owner != nil {
		ctx = WithOwner(ctx, *owner)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/admin/v1/channels/probe-models", nil)
	rec := httptest.NewRecorder()
	(&API{}).failProbe(rec, req, err)

	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
		t.Fatalf("响应不是 JSON: %v；正文：%s", jerr, rec.Body)
	}
	return rec.Code, body.Error.Message
}

// TestFailProbeMapsErrors 把 failProbe 的每一格都钉一遍。
//
// 表驱动而不是只测新增的那一格：分档靠的是 switch 的顺序，把坏密文插在
// 默认那支之前会改变别的错误的码（例如 ErrNoMasterKey 落到 400）。这条用例
// 同时是「加了新错误之后旧格没被挤歪」的证据。
func TestFailProbeMapsErrors(t *testing.T) {
	scoped := int64(7)
	cases := []struct {
		name    string
		owner   *int64
		err     error
		want    int
		message string // 非空时正文必须逐字等于它
	}{
		{
			// 密文损坏是这一行的数据问题，不是调用方写错了请求体。
			name:    "坏密文",
			err:     fmt.Errorf("渠道 12: %w", channels.ErrCredentialCorrupt),
			want:    http.StatusInternalServerError,
			message: msgProbeCorruptCredential,
		},
		{
			name:    "坏密文裹在解密失败的链上",
			err:     fmt.Errorf("探测: %w", fmt.Errorf("渠道 12: %w", channels.ErrCredentialCorrupt)),
			want:    http.StatusInternalServerError,
			message: msgProbeCorruptCredential,
		},
		{
			name:    "商家面上自己的渠道不存在",
			owner:   &scoped,
			err:     fmt.Errorf("%w：id 12", channels.ErrChannelNotFound),
			want:    http.StatusNotFound,
			message: msgNotFound,
		},
		{
			// 管理面没有归属可收窄：正文带上 id，调用者是运维。
			name: "管理面上渠道不存在",
			err:  fmt.Errorf("%w：id 12", channels.ErrChannelNotFound),
			want: http.StatusNotFound,
		},
		{
			name:    "base_url 校验不过",
			err:     fmt.Errorf("%w：%w", channels.ErrInvalidBaseURL, errors.New("base_url 不能带 query")),
			want:    http.StatusBadRequest,
			message: msgProbeInvalidBaseURL,
		},
		{
			name: "未知渠道类型",
			err:  fmt.Errorf("渠道类型无效: %w", channel.ErrUnknownType),
			want: http.StatusBadRequest,
		},
		{
			name:    "没有主密钥",
			err:     fmt.Errorf("渠道 12: %w", channels.ErrNoMasterKey),
			want:    http.StatusServiceUnavailable,
			message: "the master key is not configured on this instance",
		},
		{
			name:    "响应体超限",
			err:     fmt.Errorf("探测上游模型失败: %w", httpx.ErrBodyTooLarge),
			want:    http.StatusBadGateway,
			message: "the upstream response exceeded the 8 MiB limit",
		},
		{
			name: "上游超时",
			err:  &channel.ErrUpstream{Err: fakeTimeout{timeout: true}},
			want: http.StatusGatewayTimeout,
		},
		{
			name: "上游答了个错",
			err: &channel.ErrUpstream{
				StatusCode: http.StatusUnauthorized, GotResponse: true, Body: `{"error":"bad key"}`,
			},
			want: http.StatusBadGateway,
		},
		{
			name: "上游没连上",
			err:  &channel.ErrUpstream{Err: errFake("connection refused")},
			want: http.StatusBadGateway,
		},
		{
			// 认不出来的错误仍按输入错：这一格的偏向没有因为新增分支而改变。
			name: "认不出来的错误",
			err:  errFake("boom"),
			want: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, message := failProbeResult(t, tc.owner, tc.err)
			if got != tc.want {
				t.Errorf("状态码 = %d，期望 %d；正文：%s", got, tc.want, message)
			}
			if tc.message != "" && message != tc.message {
				t.Errorf("正文 = %q，期望逐字 %q", message, tc.message)
			}
		})
	}
}

// TestFailProbeScopedNotFoundHidesDetails 钉住商家面的 404 正文里没有 id。
//
// 上面那一格已经比对了正文；这里单独判「不出现 id」，因为正文恰好相同而
// 正文里带着 id 是两件事（后者只是这一次凑巧对上了）。
func TestFailProbeScopedNotFoundHidesDetails(t *testing.T) {
	owner := int64(7)
	_, message := failProbeResult(t, &owner,
		fmt.Errorf("%w：id 123456", channels.ErrChannelNotFound))
	if strings.Contains(message, "123456") {
		t.Errorf("商家面的 404 正文回显了渠道 id：%q", message)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// fakeTimeout 是 net.Error 的超时形态：http 包自己的时限（Client.Timeout、
// ResponseHeaderTimeout）到点给的就是这一类——它的错误链里没有
// context.DeadlineExceeded。
type fakeTimeout struct{ timeout bool }

func (e fakeTimeout) Error() string   { return "i/o timeout" }
func (e fakeTimeout) Timeout() bool   { return e.timeout }
func (e fakeTimeout) Temporary() bool { return e.timeout }

// TestIsUpstreamTimeout 钉住「两条时限都算超时」。
//
// 只认 context.DeadlineExceeded 时，挂在那两条 http 时限上的探测会漏到 502，
// 而文档承诺 504——这一格就是那么漏掉的。
func TestIsUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"context 自带 deadline", context.DeadlineExceeded, true},
		{"包了一层", fmt.Errorf("探测: %w", context.DeadlineExceeded), true},
		{"net 层的 deadline 哨兵", os.ErrDeadlineExceeded, true},
		{"http 包的超时", fakeTimeout{timeout: true}, true},
		{
			"包进 ErrUpstream 的超时",
			&channel.ErrUpstream{StatusCode: 200, GotResponse: true, Err: fakeTimeout{timeout: true}},
			true,
		},
		{"普通错误", errFake("boom"), false},
		{"网络错误但不是超时", fakeTimeout{timeout: false}, false},
	}
	for _, c := range cases {
		if got := isUpstreamTimeout(c.err); got != c.want {
			t.Errorf("%s: isUpstreamTimeout = %v，期望 %v", c.name, got, c.want)
		}
	}
}
