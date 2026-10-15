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
