package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
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
//
// 这里同时钉住 GotResponse：非 2xx 是**拿到过响应**的失败，状态码是真的。
// 下游据此把「连不上」与「上游回了错误状态码」分到两档；漏掉它，这一条会
// 被读成「一个响应都没拿到」。
func TestCheckStatus(t *testing.T) {
	if err := CheckStatus(&http.Response{StatusCode: http.StatusOK}); err != nil {
		t.Errorf("2xx 不该报错: %v", err)
	}

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{},
		Body:       http.NoBody,
	}
	err := CheckStatus(resp)
	if err == nil {
		t.Fatal("429 应报错")
	}
	if !channel.IsRetryable(err) {
		t.Error("429 应可重试")
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("错误类型为 %T", err)
	}
	if !up.GotResponse {
		t.Error("非 2xx 是拿到过响应的失败，GotResponse 应为 true")
	}
	if up.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d，期望 %d", up.StatusCode, http.StatusTooManyRequests)
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
//
// **两条出口都要覆盖**：全都放行、以及全都拒绝。后者的名单原先直接返回、
// 没走排序那一行——而「所有自定义头都被拒」恰恰是最需要拿日志比对的情形。
// 这个用例只喂了「至少一个头能通过」的输入，所以那条出口一直没被测到。
//
// 每条输入跑很多轮：名单的先后来自 map 遍历序，跑一轮撞上「恰好有序」是有
// 可能的，跑多轮才不是靠运气。
func TestSafeRequestHeadersRejectedIsSorted(t *testing.T) {
	cases := []struct {
		name string
		// custom 里的头名全部会被拒；allRejected 为 true 时一个都不通过，
		// 用来走「全都拒绝」那条出口。
		custom      map[string]string
		allRejected bool
	}{
		{
			name: "部分被拒（有头通过）",
			custom: map[string]string{
				"X-Zeta": "1", "X-Alpha": "2", "X-Middle": "3",
				"content-type": "4", "host": "5", "user-agent": "6",
			},
		},
		{
			name: "全部被拒（一个都没通过）",
			custom: map[string]string{
				"content-type": "1", "host": "2", "user-agent": "3",
				"openai-version": "4", "anthropic-beta": "5", "accept": "6",
			},
			allRejected: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 跑很多轮：名单的先后来自 map 遍历序，跑一轮撞上「恰好有序」
			// 是有可能的，跑多轮才不是靠运气。
			for round := 0; round < 50; round++ {
				safe, rejected := SafeRequestHeaders(tc.custom)
				if tc.allRejected && len(safe) != 0 {
					t.Fatalf("这一组应当全被拒，却放行了 %v", safe)
				}
				if !tc.allRejected && len(safe) == 0 {
					t.Fatalf("这一组应当有头通过，却一个都没通过（那样就退化成另一条出口了）")
				}
				if len(rejected) < 2 {
					t.Fatalf("这一组应当有多个被拒的头，实际 %v", rejected)
				}
				for i := 1; i < len(rejected); i++ {
					if rejected[i-1] > rejected[i] {
						t.Fatalf("第 %d 轮：被拒名单未排序: %v", round, rejected)
					}
				}
			}
		})
	}
}

// ── 自定义头的落地 ──────────────────────────────────────────────────────

// captureLogs 把进程日志器换成写进缓冲区的测试日志器（debug 级），返回那个
// 缓冲区。
//
// 日志器是**进程级**的（observe.FromContext 取的就是 slog.Default()），所以
// 用完必须还原。本包的用例都不并行跑（没有 t.Parallel），换默认日志器因此
// 是安全的。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(observe.NewLoggerWithWriter(&buf, "debug", "text", false))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func newHeaderTestRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return req
}

// 放行的头要真的落到请求上，被拒的头一个都不许落上去——**即使请求上原本
// 已经有那个头**。
//
// 后一半是这条路径的全部意义：适配器先设鉴权头、再调这里，顺序反了（或
// 过滤失效）就是一条越权路径——插件能冒充别的渠道身份。
func TestApplyCustomHeadersSetsSafeAndDropsReserved(t *testing.T) {
	req := newHeaderTestRequest(t)
	req.Header.Set("Authorization", "Bearer placeholder-credential")

	ApplyCustomHeaders(req, &channel.Request{Headers: map[string]string{
		"X-Custom-Tenant": "t1",
		"Authorization":   "Bearer 冒充的凭证",
		"X-API-KEY":       "冒充的钥匙",
	}})

	if got := req.Header.Get("X-Custom-Tenant"); got != "t1" {
		t.Errorf("放行的自定义头没落到请求上，实际 %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer placeholder-credential" {
		t.Errorf("鉴权头被覆盖了，实际 %q", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("被拒的头不该落到请求上，实际 %q", got)
	}
}

// 被拒的头要**留下痕迹**，而且只记名字不记值。
//
// 静默丢弃是不行的：插件作者写了一个被拒的头却没有任何反馈，会以为是上游
// 不支持它。反过来，值可能含敏感内容，日志里出现它等于把密钥抄进日志。
func TestApplyCustomHeadersLogsRejectedNames(t *testing.T) {
	buf := captureLogs(t)
	req := newHeaderTestRequest(t)

	ApplyCustomHeaders(req, &channel.Request{Headers: map[string]string{
		"X-Ok":          "fine",
		"Authorization": "Bearer 冒充的凭证",
		"api-key":       "冒充的钥匙",
	}})

	logged := buf.String()
	if !strings.Contains(logged, "自定义请求头被拒") {
		t.Errorf("被拒的头没留下痕迹: %q", logged)
	}
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("这条日志该是 warn 级: %q", logged)
	}
	for _, name := range []string{"Authorization", "api-key"} {
		if !strings.Contains(logged, name) {
			t.Errorf("日志里没有头名 %q: %q", name, logged)
		}
	}
	if strings.Contains(logged, "冒充") {
		t.Errorf("日志里带上了头的**值**: %q", logged)
	}
}

// 一个头都没被拒时不打日志：这条路径上「有自定义头」是常态，每次都记一条
// warn 会把真正的告警淹掉。
func TestApplyCustomHeadersSilentWhenNothingRejected(t *testing.T) {
	buf := captureLogs(t)
	req := newHeaderTestRequest(t)

	ApplyCustomHeaders(req, &channel.Request{Headers: map[string]string{"X-Ok": "fine"}})

	if got := buf.String(); got != "" {
		t.Errorf("没有头被拒却打了日志: %q", got)
	}
	if got := req.Header.Get("X-Ok"); got != "fine" {
		t.Errorf("放行的头没落到请求上，实际 %q", got)
	}
}

// 载体是 nil 时什么都不做，而不是 panic：这条路径上的调用方未必保证非 nil。
func TestApplyCustomHeadersNilRequest(t *testing.T) {
	buf := captureLogs(t)
	req := newHeaderTestRequest(t)

	ApplyCustomHeaders(req, nil)

	if got := len(req.Header); got != 0 {
		t.Errorf("nil 载体不该设任何头，实际 %v", req.Header)
	}
	if got := buf.String(); got != "" {
		t.Errorf("nil 载体不该打日志: %q", got)
	}
}
