package httpx

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

func TestEndpointJoining(t *testing.T) {
	cases := []struct {
		base string
		path string
		want string
	}{
		{"https://api.example.com", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1/", "/v1/models", "https://api.example.com/v1/models"},
	}
	for _, tc := range cases {
		if got := Endpoint(tc.base, tc.path); got != tc.want {
			t.Errorf("endpoint(%q, %q) = %q，期望 %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := ParseRetryAfter("30"); got != 30*time.Second {
		t.Errorf("秒数形式解析为 %v", got)
	}
	if got := ParseRetryAfter(""); got != 0 {
		t.Errorf("空值应为 0，得到 %v", got)
	}
	if got := ParseRetryAfter("不是时间"); got != 0 {
		t.Errorf("非法值应为 0，得到 %v", got)
	}
	// HTTP 日期形式，取未来一个时间点
	future := time.Now().Add(45 * time.Second).UTC().Format(http.TimeFormat)
	if got := ParseRetryAfter(future); got <= 0 || got > time.Minute {
		t.Errorf("日期形式解析为 %v", got)
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusUnprocessableEntity: false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusGatewayTimeout:      true,
	}
	for code, want := range cases {
		if got := RetryableStatus(code); got != want {
			t.Errorf("RetryableStatus(%d) = %v，期望 %v", code, got, want)
		}
	}
}

// 非 2xx 要翻译成带可重试判断的错误，并把响应体保留在结构体里供排查。
func TestCheckStatus(t *testing.T) {
	if err := CheckStatus(&http.Response{StatusCode: http.StatusOK}); err != nil {
		t.Errorf("2xx 不该报错: %v", err)
	}

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"12"}},
		Body:       http.NoBody,
	}
	err := CheckStatus(resp)
	if err == nil {
		t.Fatal("429 应报错")
	}
	if !channel.IsRetryable(err) {
		t.Error("429 应可重试")
	}
}

// 响应体要截断后保留：上游的错误信息可能与请求内容有关，
// 而上层排查时需要看到它。
func TestCheckStatusKeepsTruncatedBody(t *testing.T) {
	long := make([]byte, MaxErrorBody*2)
	for i := range long {
		long[i] = 'x'
	}
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(long)),
	}
	err := CheckStatus(resp)
	var ue *channel.ErrUpstream
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型为 %T", err)
	}
	if len(ue.Body) > MaxErrorBody {
		t.Errorf("响应体未截断，长度为 %d", len(ue.Body))
	}
	if len(ue.Body) == 0 {
		t.Error("响应体应保留内容")
	}
}

// 客户端必须设置响应头超时：上游卡住时表现为「请求永远不返回」，
// 那比报错难排查得多。整体 Timeout 不能设——长流会被中途掐断。
func TestClientConfiguresTimeout(t *testing.T) {
	c := Client(7 * time.Second)
	if c.Timeout != 0 {
		t.Error("不应设置整体 Timeout：长流会被中途掐断")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型为 %T", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}
}

// 排空之后再关闭：留着未读数据会让连接无法复用。
func TestDrainAndCloseConsumesBody(t *testing.T) {
	body := &recordingBody{Reader: strings.NewReader("还有没读完的数据")}
	Drain(body)
	if body.closed {
		t.Error("Drain 不该关闭 body")
	}
	if body.read != len("还有没读完的数据") {
		t.Errorf("Drain 读走了 %d 字节，期望全部", body.read)
	}
	DrainAndClose(body)
	if !body.closed {
		t.Error("DrainAndClose 应关闭 body")
	}
	Drain(nil)         // 不该 panic
	DrainAndClose(nil) // 不该 panic
}

// recordingBody 记录读了多少字节、以及是否被关闭。
type recordingBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *recordingBody) Close() error {
	b.closed = true
	return nil
}

// ── 自定义请求头 ────────────────────────────────────────────────────────

// 这个集合必须是**封闭的**：漏掉一个就是一条越权路径。
// 黑名单永远可能漏——上游加一个新的鉴权头，黑名单不会跟着更新。
func TestSafeRequestHeadersRejectsAuthHeaders(t *testing.T) {
	cases := map[string]string{
		"标准鉴权":         "Authorization",
		"小写的鉴权":        "authorization",
		"大小写混写":        "AUTHORIZATION",
		"Anthropic 风格": "x-api-key",
		"通用 api-key":   "api-key",
		"Google 前缀":    "x-goog-api-key",
		"AWS 前缀":       "x-amz-security-token",
		"自家的头":         "x-larkax-internal",
		"Content-Type": "Content-Type",
		"Host":         "Host",
		"Connection":   "Connection",
	}
	// cases 的键是说明、值是头名。构造时要用**头名**做键——
	// 早先写成用说明做键，于是所有头都变成了正常自定义头，测试假绿。
	custom := make(map[string]string, len(cases))
	for _, header := range cases {
		custom[header] = "值"
	}

	safe, rejected := SafeRequestHeaders(custom)
	if len(safe) != 0 {
		t.Errorf("这些头都该被拒，实际放行 %v", safe)
	}
	if len(rejected) != len(cases) {
		t.Errorf("被拒的应有 %d 个，实际 %d 个: %v", len(cases), len(rejected), rejected)
	}
}

// 大小写不敏感：按字面比较会让插件用 `Authorization` 绕过 `authorization` 的检查。
func TestSafeRequestHeadersCaseInsensitive(t *testing.T) {
	for _, name := range []string{"authorization", "AUTHORIZATION", "Authorization", "aUtHoRiZaTiOn"} {
		safe, _ := SafeRequestHeaders(map[string]string{name: "值"})
		if len(safe) != 0 {
			t.Errorf("%q 应被拒", name)
		}
	}
}

func TestSafeRequestHeadersAllowsCustom(t *testing.T) {
	safe, rejected := SafeRequestHeaders(map[string]string{
		"X-Custom-Trace": "abc",
		"X-Tenant":       "t1",
	})
	if len(rejected) != 0 {
		t.Errorf("正常的自定义头不该被拒: %v", rejected)
	}
	if len(safe) != 2 {
		t.Errorf("应放行两个，实际 %v", safe)
	}
}

// 值里带换行等于能注入出额外的头，与日志注入是同一类问题。
func TestSafeRequestHeadersRejectsHeaderInjection(t *testing.T) {
	safe, rejected := SafeRequestHeaders(map[string]string{
		"X-Ok":   "fine",
		"X-Bad":  "值\r\nX-Injected: evil",
		"X-Bad2": "值\nX-Injected: evil",
	})
	if len(safe) != 1 {
		t.Errorf("带换行的头应被拒，实际放行 %v", safe)
	}
	if len(rejected) != 2 {
		t.Errorf("应拒两个，实际 %v", rejected)
	}
}

// 值里的控制字符与换行是同一类问题：只挡 CR/LF 的话，NUL 之类的字符会一路
// 走到 net/http 才报错，那时它表现为「上游失败」而不是「这个头不合法」。
//
// 同时钉住反面：HTAB 是 RFC 7230 的 field-value 允许的，别把它一起拒了。
func TestSafeRequestHeadersRejectsControlCharsInValue(t *testing.T) {
	safe, rejected := SafeRequestHeaders(map[string]string{
		"X-Nul": "\x00",
		"X-Vt":  "a\x0bb",
		"X-Del": "a\x7fb",
		"X-Tab": "a\tb",
		"X-Ok":  "a b",
	})
	if len(rejected) != 3 {
		t.Errorf("带控制字符的三个头都该被拒，实际被拒 %v", rejected)
	}
	if safe["X-Tab"] != "a\tb" {
		t.Errorf("HTAB 是 RFC 允许的，不该被拒: %v", safe)
	}
	if safe["X-Ok"] != "a b" {
		t.Errorf("普通值不该被拒: %v", safe)
	}
	if len(safe) != 2 {
		t.Errorf("只该放行两个，实际 %v", safe)
	}
}

// 空头名与全被拒时要返回 nil 而不是空 map。
func TestSafeRequestHeadersEdgeCases(t *testing.T) {
	if safe, rejected := SafeRequestHeaders(nil); safe != nil || rejected != nil {
		t.Errorf("空输入应返回 nil, nil，得到 %v, %v", safe, rejected)
	}

	safe, rejected := SafeRequestHeaders(map[string]string{"": "值", "   ": "值"})
	if safe != nil {
		t.Errorf("空头名应被拒，实际 %v", safe)
	}
	if len(rejected) != 2 {
		t.Errorf("应拒两个，实际 %v", rejected)
	}
}

// 被拒的名单要排序，否则日志里同一个错两次运行顺序不同，搜不到。
func TestSafeRequestHeadersRejectedIsSorted(t *testing.T) {
	_, rejected := SafeRequestHeaders(map[string]string{
		"X-Zeta": "1", "X-Alpha": "2", "X-Middle": "3",
		"authorization": "4", "api-key": "5",
	})
	for i := 1; i < len(rejected); i++ {
		if rejected[i-1] > rejected[i] {
			t.Errorf("被拒名单未排序: %v", rejected)
			break
		}
	}
}
