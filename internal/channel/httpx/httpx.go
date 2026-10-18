// Package httpx 是上游适配器共用的 HTTP 细节。
//
// 这些逻辑与「说哪套协议」无关：URL 拼接、状态码到可重试的映射、
// 响应体的排空与关闭。各适配器各写一份的话，
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
	"sort"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
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
//
// # 这里为什么不解析 Retry-After
//
// 它曾经解析过，写进 ErrUpstream.RetryAfter，而那个字段零个读取点。
// 退避在这条路径上没有落点：internal/route 遇到可重试的失败是「换下一个
// 候选」，不是「等一等同一个上游」。它真正的落点是对外响应上的 Retry-After，
// 而那要先把「上游限流该回几号」定下来——那是另一批要一起定的对外契约。
// 所以这里不解析：留一个没人读的解析结果，只会让下一个人以为退避接上了。
func CheckStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
	return &channel.ErrUpstream{
		StatusCode:  resp.StatusCode,
		GotResponse: true,
		Body:        string(body),
		Retryable:   RetryableStatus(resp.StatusCode),
	}
}

// MaxErrorBody 是保留的错误响应体长度上限。
const MaxErrorBody = 2048

// RetryableStatus 判断某个状态码是否值得换渠道重试。
//
// 判据与 channel.IsEmbeddedErrorRetryable 同一句（上游把错误写在 200 响应体里
// 时由那里判）：**上游这一侧暂时答不上来**的值得换一个渠道再试，**这次请求
// 本身不对**的换到哪都一样，重试只会把费用与延迟乘以候选渠道数。
//
// 4xx 里落进「暂时答不上来」的只有三个：
//
//   - 408 请求超时：上游在它愿意等的时间内没拿到完整请求，实际部署里更常见的是
//     它前面的网关替自己的后端超时回了这个码。两种都是「这条链路此刻不行」。
//   - 425 Too Early：上游不愿冒重放的风险处理这次请求，RFC 8470 的语义就是
//     「过一会儿再来」。
//   - 429 限流：这个渠道此刻不给量。
//
// 其余 4xx（400 参数错、401 钥匙错、403 权限、404 路径错、409 状态冲突、
// 413 请求体过大、422 语义错…）都是这次请求与这一个上游之间确定对不上的地方，
// 换渠道也是同一个结果。
//
// 5xx 一律算：上游自己出错、或者它前面的网关出错。501 与 505 严格说是「对端不
// 支持这件事」而不是「暂时不行」，但换一条渠道确实可能支持，所以留在里面——
// 代价只是失败时多试一个候选。
//
// 可重试还有第二个下游：渠道健康度把这次失败算在渠道头上（见 internal/gateway
// 的 channelAtFault，它第一句就是「可重试即故障」）。408 落在这里是有意的，与
// 429、5xx 同档：上游真的卡住时，这个渠道必须留下连续失败，否则它会一直留在
// 候选里（issue #613）。
func RetryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return code >= 500
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
//
// 目前只有测试在调它：适配器手里都有完整的 *http.Response，走的是那两行。
func DrainAndClose(body io.ReadCloser) {
	Drain(body)
	if body == nil {
		return
	}
	_ = body.Close()
}

// 上游请求头里不允许被自定义头覆盖的那些。
//
// 白名单而不是黑名单：**这个集合必须是封闭的，否则漏掉一个就是一条
// 越权路径**。黑名单永远可能漏——上游加一个新的鉴权头，黑名单不会跟着更新。
//
// 按前缀与固定名两部分判：
//   - 固定名是协议与鉴权的关键头，覆盖它们等于冒充身份或破坏协议
//   - 前缀覆盖各家自己的鉴权头（x-api-key、x-goog-api-key、api-key 等）
var reservedHeaderPrefixes = []string{
	"authorization",
	"x-api-key",
	"api-key",
	"x-goog-",
	"x-amz-",
	"x-larkax-", // 自己的头也不该从这条路径进来：那是宿主写的，不是插件
}

// reservedHeaderNames 是**适配器自己会设置**的协议头。
//
// 规则很简单：凡是适配器写入的头，就不允许被自定义头覆盖。
// anthropic-version 被覆盖会让协议版本被插件改掉——上游可能因此
// 返回完全不同的响应结构，而错误现场离原因很远。
var reservedHeaderNames = map[string]bool{
	"anthropic-version":   true,
	"anthropic-beta":      true,
	"openai-version":      true,
	"openai-beta":         true,
	"openai-organization": true,
	"openai-project":      true,
}

// SafeRequestHeaders 从自定义头里滤出可以安全附加的那些。
//
// 返回被拒绝的头名，供调用方记录——**静默丢弃是不行的**：插件作者
// 写了一个被拒的头却没有任何反馈，会以为是上游不支持。
//
// 大小写不敏感：HTTP 头名不区分大小写，按字面比较会让插件用
// `Authorization` 绕过 `authorization` 的检查。
func SafeRequestHeaders(custom map[string]string) (map[string]string, []string) {
	if len(custom) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(custom))
	var rejected []string

	for name, value := range custom {
		lower := strings.ToLower(strings.TrimSpace(name))
		if lower == "" {
			rejected = append(rejected, name)
			continue
		}
		if isReservedHeader(lower) {
			rejected = append(rejected, name)
			continue
		}
		// 值必须符合 RFC 7230 的 field-value 文法：换行能注入出额外的头，
		// 而 NUL 之类的控制字符会被 net/http 拒掉——那时失败看起来像
		// 「上游出错」，而不是「这个头的值不合法」。
		if !channel.ValidHeaderValue(value) {
			rejected = append(rejected, name)
			continue
		}
		out[name] = value
	}

	// 排序在两条出口**之前**：rejected 的先后来自 map 遍历序，同一份配置
	// 两次运行可能不同，而它要进日志（见函数注释）。原先排序写在下面那条
	// 出口之后，于是「所有自定义头都被拒」时走的是另一条出口、拿到的是
	// 未排序的名单——那正是最需要靠日志比对的情形。
	sort.Strings(rejected)
	if len(out) == 0 {
		return nil, rejected
	}
	return out, rejected
}

// ApplyCustomHeaders 把渠道请求里的自定义头附加到 HTTP 请求上。
//
// 鉴权头与协议头由 SafeRequestHeaders 滤掉，而**被拒的头会被记下来**而不是
// 静默丢弃：插件作者写了一个被拒的头却没有任何反馈，会以为是上游不支持它。
//
// 三套协议共用这一份，而不是每个适配器各留一份：接下一个协议时再抄一遍，
// 某天「某个头只对某一家要带」的改动就只会落在其中一份上，另外两份不会跟着
// 变，而那时没有任何东西会提示这一点。
//
// 调用点仍在适配器那一层，而不只在插件层：插件层的白名单写错时，这里是最后
// 一道。「插件能覆盖 Authorization」意味着它能冒充别的渠道身份。
func ApplyCustomHeaders(httpReq *http.Request, req *channel.Request) {
	if req == nil {
		return
	}
	safe, rejected := SafeRequestHeaders(req.Headers)
	for name, value := range safe {
		httpReq.Header.Set(name, value)
	}
	if len(rejected) > 0 {
		// 只记名字不记值：值可能含敏感内容，而这里需要的只是「谁被拒了」。
		observe.FromContext(httpReq.Context()).Warn("自定义请求头被拒",
			"headers", strings.Join(rejected, ","))
	}
}

func isReservedHeader(lower string) bool {
	// content-type / content-length 之类由 Go 的 http 包自己管，
	// 覆盖它们会让请求体与声明不符。
	switch lower {
	case "content-type", "content-length", "host", "transfer-encoding",
		"connection", "accept", "accept-encoding", "user-agent":
		return true
	}
	if reservedHeaderNames[lower] {
		return true
	}
	for _, prefix := range reservedHeaderPrefixes {
		if lower == prefix || strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}
