package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
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

// 取不出主机名的 base_url 按原样拼接：`localhost:8080/` 会被 url.Parse
// 当成 scheme 与 opaque 部分，Host 为空、又不以 / 开头，于是直接拼上去，
// 结尾斜杠保留。
//
// 这一条同时是 `err == nil` 那一格的证据：判据取反之后，这条输入两版都
// 解析成功、都不 panic，只有返回值不同（变异版会落进 TrimRight 那一支，
// 把结尾斜杠去掉）。只用解析失败的输入去杀，红是 nil 解引用，那是 panic，
// 说不清是哪条断言没通过。
func TestEndpointOpaqueBase(t *testing.T) {
	const base = "localhost:8080/"
	if got, want := Endpoint(base, "/v1/chat"), "localhost:8080//v1/chat"; got != want {
		t.Errorf("Endpoint(%q, %q) = %q，期望 %q", base, "/v1/chat", got, want)
	}
}

// 解析失败的 base_url 也按原样拼接，结尾斜杠照样去掉。
//
// 这条固定的是正常行为：url.Parse 报错时不做主机判断，直接拼出去，让上层
// 报一个含糊但可读的错。它在变异体上的红是 panic（变异版会在 url.Parse
// 返回 nil 的那条路径上解引用 u），所以只作为行为说明，不作为证据——证据
// 在 TestEndpointOpaqueBase。
func TestEndpointUnparseableBase(t *testing.T) {
	const base = "http://[::1]:namedport/"
	if got, want := Endpoint(base, "/v1/chat"), "http://[::1]:namedport/v1/chat"; got != want {
		t.Errorf("Endpoint(%q, %q) = %q，期望 %q", base, "/v1/chat", got, want)
	}
}

// 版本段只在「整个段都是 v 加数字」时成立。
//
// 各条各自固定一处边界：路径里没有第二段（/v1）、版本段只有一个字符（/v）、
// 段里混进非数字（/v1x、/vx）、以及两位数的合法版本（/v12）。数字两端各取
// 一个恰好在边界上的值（0 与 9），它们仍然合法——判据是「每位都是数字」，
// 不是「不是 1 到 8」。
//
// 「没有第二段」这一条固定的是「不往下切」；而 idx < 0 改成 <= 没有能把
// 两版分开的输入：两者只在 idx == 0 时不同，那时 rest 以 / 开头、切出来的
// 段是空串，紧跟着的长度判据照样返回 ""。同理，空段那条（//chat）也走
// 这条出口。
func TestVersionPrefixRecognition(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/v1/chat", "/v1"},
		{"/v12/chat", "/v12"},
		{"/v0/chat", "/v0"},
		{"/v9/chat", "/v9"},
		{"/v1", ""},
		{"/chat", ""},
		{"v1/chat", ""},
		{"/v/chat", ""},
		{"/v1x/chat", ""},
		{"/vx/chat", ""},
		{"//chat", ""},
	}
	for _, tc := range cases {
		if got := versionPrefix(tc.path); got != tc.want {
			t.Errorf("versionPrefix(%q) = %q，期望 %q", tc.path, got, tc.want)
		}
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusBadRequest:            false,
		http.StatusUnauthorized:          false,
		http.StatusForbidden:             false,
		http.StatusNotFound:              false,
		http.StatusConflict:              false,
		http.StatusRequestEntityTooLarge: false,
		http.StatusUnprocessableEntity:   false,
		// 上游这一侧「暂时答不上来」的三个 4xx：与 5xx 同档，值得换一个渠道。
		http.StatusRequestTimeout:      true,
		http.StatusTooEarly:            true,
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

// 408（上游自己的超时）要走完到「换渠道重试」那一档：与 429、5xx 同路。
//
// 这条把两步连起来看——上游回 408、CheckStatus 把它翻成错误，而路由层换不换
// 下一个候选、渠道健康度算不算这次失败，读的都是后一步（见 channel.IsRetryable）。
// 只钉 RetryableStatus 的话，「它有没有接到错误上」没有人回答。
//
// 与 #607 的客户端断开不是一回事：那一种**根本没有响应**，归不归因由 ctx 判
// （见 internal/gateway 的 channelAtFault），不走这条状态码路径。
func TestRequestTimeoutIsRetryable(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusRequestTimeout,
		Header:     http.Header{},
		Body:       http.NoBody,
	}
	err := CheckStatus(resp)
	if err == nil {
		t.Fatal("408 应报错")
	}
	if !channel.IsRetryable(err) {
		t.Error("408 应可重试：路由层据此换下一个候选渠道")
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("错误类型为 %T", err)
	}
	if up.StatusCode != http.StatusRequestTimeout {
		t.Errorf("StatusCode = %d，期望 %d", up.StatusCode, http.StatusRequestTimeout)
	}
	if !up.GotResponse {
		t.Error("408 是拿到过响应的失败，GotResponse 应为 true")
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

// 300 不是 2xx：它是「有多种选择」的重定向，不是成功响应。
//
// 边界写宽一格，上游回 300 时会被当成成功。后果分两种情况：响应体能解
// 成 JSON 的，会变成一份空回答，账上记成「成功但用量为零」；解不成的，
// 报的是「响应不是 JSON」这条与真实原因无关的错。两种都让「上游给的是
// 重定向」这件事查不出来。
func TestCheckStatusTreats300AsFailure(t *testing.T) {
	resp := &http.Response{StatusCode: 300, Header: http.Header{}, Body: http.NoBody}
	err := CheckStatus(resp)
	if err == nil {
		t.Fatal("300 不是 2xx，应报错")
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("错误类型为 %T", err)
	}
	if up.StatusCode != 300 {
		t.Errorf("StatusCode = %d，期望 300", up.StatusCode)
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

// 空闲连接要有回收时限：这个字段取零值表示**永不回收**。
//
// 上游换地址、或者中间设备悄悄丢了连接之后，池里那些连接会一直以
// 「可复用」的样子留着；请求复用到它们就失败，而连接数指标上看不出原因。
// 断言用字面量 90s，不从被测代码里取常量。
func TestClientExpiresIdleConnections(t *testing.T) {
	tr, ok := Client(time.Second).Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型为 %T", Client(time.Second).Transport)
	}
	if tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v，期望 90s", tr.IdleConnTimeout)
	}
}

// ── 响应头之前的那两段时限 ──────────────────────────────────────────────

// blackholeAddr 是一个黑洞地址：SYN 发出去既没有应答也没有 RST，
// 于是一路等到内核自己的重试结束。
//
// 192.0.2.0/24 是 RFC 5737 留给文档与示例的网段，任何主机都不会用它，
// 端口取 81 同理（那个网段上不会有服务，取 81 只是免得与 80 上的约定俗成混淆）。
// 用这个地址的用例会先确认它确实是黑洞——失败必须是**超时**，不能是立刻被拒，
// 换了网络环境而它被路由到别处时，那一条会先报出来，而不是让用例假绿。
const blackholeAddr = "192.0.2.1:81"

// 拨号必须受 Transport 自己的时限约束，DNS 卡住与 TCP 连不上都算在这一段里。
//
// 直接调 Transport 上的 DialContext，而不是发一次完整的请求：拨黑洞地址的
// 完整请求要先过代理判断，本机环境变量里如果配了代理、而黑洞地址又不在
// NO_PROXY 里，请求会被代理接走，这个用例就成了什么都没证明。直接调拨号
// 与代理无关。
//
// 没有拨号函数时它是 nil：读成 nil 会让本用例 panic，而 panic 说不清
// 少了哪一条，所以先判 nil 并明说。
func TestClientBoundsDial(t *testing.T) {
	c := Client(10 * time.Minute)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型为 %T", c.Transport)
	}
	dial := tr.DialContext
	if dial == nil {
		t.Fatal("Transport 没有 DialContext：走的是默认拨号，黑洞地址上要等到内核 SYN 重试结束（两分钟出头）")
	}

	// 本用例另外给自己设一个上限：拨号若真的没有时限，这一条会在 30 秒处
	// 失败，而不是把整个 go test 拖到它自己超时。
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+30*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := dial(ctx, "tcp", blackholeAddr)
	elapsed := time.Since(start)
	if conn != nil {
		_ = conn.Close()
	}

	if err == nil {
		t.Fatalf("%s 居然连上了，这个用例的前提（那里没人应答）在本机不成立", blackholeAddr)
	}
	var nerr net.Error
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Fatalf("%s 的失败不是超时而是 %v：本机不把这个地址当黑洞（可能立刻回了不可达），"+
			"这个用例因此证明不了时限", blackholeAddr, err)
	}
	if elapsed > dialTimeout+2*time.Second {
		t.Errorf("拨号等了 %v 才失败，超过 dialTimeout(%v)：这个候选会把整条请求路径拖住",
			elapsed, dialTimeout)
	}
}

// TLS 握手也要有时限：一个愿意建 TCP 但不说话的上游，能把请求挂在握手上
// ——ResponseHeaderTimeout 从请求写出去之后才起算，管不到它前面这一段。
//
// 假上游在回环地址上，所以这个用例与代理环境变量无关（net/http 对回环地址
// 一律不上代理）。
func TestClientBoundsTLSHandshake(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// 接住连接就不管了：不回 ServerHello，也不关，握手因此永远完不成。
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		accepted <- conn
	}()
	t.Cleanup(func() {
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+ln.Addr().String()+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}

	c := Client(10 * time.Minute)
	// 去程用 https：那一侧才走 TLS 握手。共享的 Client 与整体 Timeout 无关，
	// 所以这里只可能被握手的时限收场。
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		resp, err := c.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()

	// 真到「没有时限」那一版，这个 goroutine 会一直挂着——那正是要证明的东西，
	// 测试进程结束时它会跟着消失，所以不做额外收尾。
	bound := tlsHandshakeTimeout + 5*time.Second
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("对一个不说话的上游居然拿到了响应")
		}
		if elapsed := time.Since(start); elapsed > tlsHandshakeTimeout+2*time.Second {
			t.Errorf("握手在 %v 处才失败，超过 TLSHandshakeTimeout(%v)：像是别处的时限让它失败的",
				elapsed, tlsHandshakeTimeout)
		}
	case <-time.After(bound):
		t.Fatalf("TLS 握手没有时限：等了 %v 还没返回（上游不回 ServerHello 时这一段会一直挂着）", bound)
	}
}

// ── 代理环境变量 ────────────────────────────────────────────────────────

// proxyHelperEnv 标记「现在跑的是子进程那一半」。
const proxyHelperEnv = "LARKAX_TEST_HTTPX_PROXY_HELPER"

// 代理必须读环境变量：零值时 Go 不报错，而是静默直连——声明了代理的部署
// 会从错误的出口出去，而这一点在日志里看不出来（请求确实发得出去）。
//
// 断言走子进程：net/http 把 ProxyFromEnvironment 的结果**缓存**在进程级的
// sync.Once 里，本进程里 t.Setenv 之后再发请求，读到的仍是缓存下来的那份
// 环境，于是断言会恒绿。子进程带着设好的 HTTP_PROXY 重新起一次，
// 它读到的才是这一份。
func TestClientHonoursProxyFromEnvironment(t *testing.T) {
	if os.Getenv(proxyHelperEnv) == "1" {
		proxyProbeRequest(t)
		return
	}

	// 假代理：回一条只有代理会回的响应体。直连时请求到不了这里
	// （api.example.com 解析不出来），所以「拿到了这条响应体」就等于
	// 「请求走了代理」。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 经代理的 HTTP 请求，请求行里是绝对 URI，r.URL 因此带主机名。
		_, _ = io.WriteString(w, proxyResponseMark+r.URL.String())
	}))
	defer srv.Close()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestClientHonoursProxyFromEnvironment$")
	cmd.Env = append(withoutProxyEnv(os.Environ()),
		proxyHelperEnv+"=1",
		"HTTP_PROXY="+srv.URL,
		"http_proxy="+srv.URL,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("子进程里请求没走代理（%v）:\n%s", err, out)
	}
}

// proxyResponseMark 是假代理回的响应体开头，子进程据此判断请求经过了谁。
const proxyResponseMark = "via-proxy "

// proxyProbeRequest 是子进程里跑的那一半：在设好 HTTP_PROXY 的环境里发一次请求。
func proxyProbeRequest(t *testing.T) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://api.example.com/v1/models", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := Client(10 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("请求失败，像是没读 HTTP_PROXY 而直连了 api.example.com: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	if !strings.HasPrefix(string(body), proxyResponseMark) {
		t.Fatalf("请求没经过代理，响应体为 %q", body)
	}
}

// withoutProxyEnv 去掉继承来的代理相关变量，免得本机的 NO_PROXY 把子进程里
// 那个假代理绕过。
func withoutProxyEnv(env []string) []string {
	drop := map[string]bool{
		"HTTP_PROXY": true, "http_proxy": true,
		"NO_PROXY": true, "no_proxy": true,
	}
	var out []string
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok && drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
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

// TestProbeClientDoesNotFollowRedirects 探测不跟随重定向：拿到的是 302 本身。
//
// 「跟着走了」在用例里的表现是 200 与跳转目标那一侧的正文——两者都是另一个
// 地址上的事实，而探测要回答的是「这个地址返回什么」。
func TestProbeClientDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			_, _ = io.WriteString(w, "followed")
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := ProbeClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("状态码 = %d，期望 %d（没有跟随重定向）", resp.StatusCode, http.StatusFound)
	}
	if followed.Load() {
		t.Error("客户端跟随了重定向，探测打到了另一个地址")
	}
}

// TestProbeClientCapsBody 响应体有上限：超一个字节就报 ErrBodyTooLarge，
// 恰好等于上限不算超。
//
// 两种情况都跑：只测「超了会报错」的话，把上限写成 0 也能过（什么都报错），
// 而那样正常的上游清单一条都读不出来。
func TestProbeClientCapsBody(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	cases := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"恰好到上限", ProbeBodyLimit, false},
		{"超一个字节", ProbeBodyLimit + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// 分块写、不给 Content-Length：上限必须在读的时候生效，
				// 不能靠响应头里的长度提前判。
				remaining := tc.size
				for remaining > 0 {
					n := min(remaining, len(chunk))
					if _, err := w.Write(chunk[:n]); err != nil {
						return
					}
					remaining -= n
				}
			}))
			defer srv.Close()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			resp, err := ProbeClient(30 * time.Second).Do(req)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if tc.wantErr {
				if !errors.Is(err, ErrBodyTooLarge) {
					t.Fatalf("读错误 = %v，期望 ErrBodyTooLarge（读了 %d 字节）", err, len(body))
				}
				if len(body) != ProbeBodyLimit {
					t.Errorf("截断处读了 %d 字节，期望 %d", len(body), ProbeBodyLimit)
				}
				return
			}
			if err != nil {
				t.Fatalf("恰好等于上限时读失败: %v", err)
			}
			if len(body) != tc.size {
				t.Errorf("读了 %d 字节，期望 %d", len(body), tc.size)
			}
		})
	}
}
