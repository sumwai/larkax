// Package httpx 是上游适配器共用的 HTTP 细节。
//
// 这些逻辑与「说哪套协议」无关：URL 拼接、状态码到可重试的映射、
// Retry-After 的两种合法形式、响应体的排空与关闭。两个适配器各写一份的话，
// **两份「可重试判断」迟早会不一致**——一处认为 429 可重试、另一处不认为，
// 表现为「同一类失败在不同渠道上行为不同」，极难排查。
//
// 放在 internal/channel/httpx 而不是 internal/channel 里：channel 包定义的是
// 接口与统一类型，是纯数据契约；这里会 import net/http。
// 混在一起会让「只想用类型定义」的地方被迫拖上 HTTP 栈。
package httpx

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// Client 是适配器应当使用的 HTTP 客户端。
//
// 集中构造而不是各处 http.DefaultClient：默认客户端没有超时，
// 而上游卡住时表现为「请求永远不返回」，那比报错难排查得多。
//
// ResponseHeaderTimeout 而不是整体 Timeout：流式响应会长期占用连接，
// 整体超时会在长回复中途把连接掐断。超时只该管「多久拿不到响应头」。
func Client(responseHeaderTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
		},
	}
}

// Endpoint 拼出完整 URL。
//
// 手工拼接而不是 url.ResolveReference：上游给的 base_url 常见写法有带与不带
// 结尾斜杠两种，路径里也可能已经含 /v1。用 TrimRight 统一掉结尾斜杠，
// 比 ResolveReference 的语义更容易预测（后者对「base 带路径、ref 也带路径」
// 的处理是替换而非拼接，与直觉不符）。
func Endpoint(baseURL, path string) string {
	if baseURL == "" {
		return path
	}
	if u, err := url.Parse(baseURL); err == nil && u.Host == "" && !strings.HasPrefix(baseURL, "/") {
		// 没有 scheme 也没有主机名，说明是配置写错了。
		// 拼出来的 URL 会在发起请求时报一个含糊的错，不如原样返回让上层报。
		return baseURL + path
	}
	base := strings.TrimRight(baseURL, "/")
	// base 已经带了版本前缀时不重复加。
	if v := versionPrefix(path); v != "" && strings.HasSuffix(base, v) {
		path = strings.TrimPrefix(path, v)
	}
	return base + path
}

// versionPrefix 取出路径里的版本段，如 "/v1/chat" → "/v1"。
func versionPrefix(path string) string {
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	rest := path[1:]
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return ""
	}
	seg := rest[:idx]
	if len(seg) < 2 || seg[0] != 'v' {
		return ""
	}
	for i := 1; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return ""
		}
	}
	return "/" + seg
}

// CheckStatus 把非 2xx 的响应翻译成带可重试判断的错误。
//
// 响应体截断后保留在结构体里：上游的错误信息可能与请求内容有关，
// 上层排查时需要，但 Error() 不带出来——错误会进日志，
// 而日志的去向比密钥本身更难控制。
func CheckStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
	return &channel.ErrUpstream{
		StatusCode: resp.StatusCode,
		Body:       string(body),
		Retryable:  RetryableStatus(resp.StatusCode),
		RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

// MaxErrorBody 是保留的错误响应体长度上限。
const MaxErrorBody = 2048

// RetryableStatus 判断某个状态码是否值得换渠道重试。
//
// 429 与 5xx 可重试；4xx 里的其余部分（400 参数错、401 钥匙错、403 权限、
// 404 路径错、422 语义错）重试多少次结果都一样，换渠道也是同一个错，
// 只会把费用与延迟乘以候选渠道数。
func RetryableStatus(code int) bool {
	if code == http.StatusTooManyRequests {
		return true
	}
	return code >= 500
}

// ParseRetryAfter 解析 Retry-After 头。
//
// 它有两种合法形式：秒数、或 HTTP 日期。只认一种的实现会在另一种上
// 静默退化为「不等待就重试」，那正好会加剧限流。
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v + "s"); err == nil && d > 0 {
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Drain 读掉响应体里剩余的内容。
//
// 排空是必须的：留着未读数据会让底层连接无法复用，表现为「QPS 一高就
// 大量新建连接」，而指标上看不出原因（连接数在涨，但没有错误）。
//
// 与关闭分开是因为 defer 的执行顺序：defer 后进先出，
//
//	defer resp.Body.Close()
//	defer httpx.Drain(resp.Body)
//
// 实际执行的是「先排空、后关闭」，正合需要。合成一个函数写成
// `defer DrainAndClose(resp.Body)` 语义相同，但 bodyclose 那次静态检查
// 只认 `resp.Body` 上直接出现的 Close，看不见被调用函数内部的关闭，
// 于是每一处正常的收尾都会被报成「响应体未关闭」。
// 分开之后调用点各自表达完整，检查也就跟得上了。
func Drain(body io.Reader) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
}

// DrainAndClose 排空并关闭响应体。
//
// 只给拿不到 *http.Response 的调用点用（例如把 body 单独传出去的地方）。
// 收尾一个 *http.Response 用 Drain + resp.Body.Close() 两行，
// 原因见 Drain 的注释。
func DrainAndClose(body io.ReadCloser) {
	Drain(body)
	if body == nil {
		return
	}
	_ = body.Close()
}
