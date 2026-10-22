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
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
)

// 上游连接这一段的两条时限，以及空闲连接的探测间隔。
//
// 只有 ResponseHeaderTimeout 是不够的，它从**请求写出去之后**才开始计：
// DNS 解析卡住、TCP 连不上、TLS 握手不返回，这三种它一个都不管。
// 这三段里最坏的是黑洞上游——SYN 发出去被丢掉，内核按自己的节奏重发，
// 那期间这个候选一声不响。放弃的时机由内核定，Linux 默认重试六次、
// 两分钟出头，调用方等到的是分钟级的停顿。
// 上限的取值因此按同一件事来定：正常建连几百毫秒（跨洲链路也是这个量级），
// 黑洞那边是两分钟——取在一两个数量级之上的余量。
const (
	// dialTimeout 覆盖 DNS 解析与 TCP 建连两段。
	//
	// 五秒的余地给的是冷 resolver 上偶尔要一两秒的解析：把一段解析慢但可用的
	// 上游判成失败，比多等几秒更亏——候选之间本来就会失败转移，慢的那一个
	// 让下一条请求去试。反过来黑洞那边从两分钟压到五秒，
	// 「换下一个候选」因此还来得及。
	dialTimeout = 5 * time.Second
	// tlsHandshakeTimeout 只管握手一把：TCP 已经建好，握手只有一到两个往返，
	// 所以它比拨号更短。三秒还没握完，对端基本就是不通了，不是慢。
	tlsHandshakeTimeout = 3 * time.Second
	// keepAlive 与 http.DefaultTransport 一致：空闲连接上按这个间隔发探测包，
	// 让中间设备上的连接状态不至于悄悄失效。
	keepAlive = 30 * time.Second
)

// Client 是适配器应当使用的 HTTP 客户端。
//
// 集中构造而不是各处 http.DefaultClient：默认客户端没有超时，
// 而上游卡住时表现为「请求永远不返回」，那比报错难排查得多。
//
// ResponseHeaderTimeout 而不是整体 Timeout：流式响应会长期占用连接，
// 整体超时会在长回复中途把连接掐断。超时只该管「多久拿不到响应头」。
//
// 响应头之前的那两段另外设限（见上面的常量）：拨号与 TLS 握手都是
// 「对面通不通」的判断，卡住时没有可读的输出，只能等时限把它停下。
//
// 代理读环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）：零值时 Go 不报错，
// 而是静默直连——声明了代理的部署会从错误的出口出去，而这一点在日志里
// 看不出来（请求确实发得出去）。回环地址与 NO_PROXY 命中的不上代理，
// 那是 net/http 的行为，不是这里的判断。
func Client(responseHeaderTimeout time.Duration) *http.Client {
	return &http.Client{Transport: newTransport(responseHeaderTimeout)}
}

// ProbeBodyLimit 是一次探测允许读的响应体上限（8 MiB）。
//
// 探测打的是**用户给的地址**：对方不是我们配好的上游，回什么都可能。
// 没有上限时，一个一直吐字节的地址会让这次调用一直读下去，直到内存或
// 超时先到。
const ProbeBodyLimit = 8 << 20

// ErrBodyTooLarge 表示响应体超过了 ProbeBodyLimit。
//
// 单独一个错误而不是「解析失败」：调用方要能说出「对方回得太大了」，
// 而不是把它报成「上游的 JSON 坏了」。
var ErrBodyTooLarge = errors.New("上游响应体超过上限")

// ProbeClient 是「拿用户给的地址直接打一次」的客户端（见控制台的模型探测）。
//
// 与 Client 的两处不同，都来自「地址不是我们配的」：
//
//   - **不跟随重定向**：默认客户端最多跟十跳，而每一跳都可能是另一个地址。
//     探测本来就只问「这个地址上的 /v1/models 能不能读」，跳到别处读到的
//     已经是另一个事实；禁止跟随也让「用我们的服务端探测内网」失去绕道的
//     那一半。
//   - **响应体有上限**：超过 ProbeBodyLimit 时读操作返回 ErrBodyTooLarge，
//     而不是给调用方半截正文——半个 JSON 解出来的会是一个看起来「格式坏了」
//     的错误，与真正的原因无关。
//
// 超时用整体 Timeout（而不是 Client 那样的 ResponseHeaderTimeout）：探测是
// 一次把清单读完的短动作，不存在「流式长回复」那种长连接。
func ProbeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     &bodyLimitTransport{base: newTransport(timeout), limit: ProbeBodyLimit},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       timeout,
	}
}

// newTransport 是 Client 与 ProbeClient 共用的连接参数。
func newTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// bodyLimitTransport 把响应体包上一层上限。
type bodyLimitTransport struct {
	base  http.RoundTripper
	limit int64
}

// RoundTrip 在响应体上套一层上限，见 limitedBody。
func (t *bodyLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &limitedBody{rc: resp.Body, remaining: t.limit}
	return resp, nil
}

// limitedBody 在读到上限之后返回 ErrBodyTooLarge。
//
// 恰好读满还不算超限：后面可能还有字节，也可能刚好结束。下一次 Read 会
// 区分这两种——还有内容时先返回 ErrBodyTooLarge，没有时返回底层给的 EOF。
type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		// 先探一个字节：读得到就说明确实超了，读不到才是正常结束。
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, ErrBodyTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// Close 关闭底层响应体。
func (b *limitedBody) Close() error { return b.rc.Close() }

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
