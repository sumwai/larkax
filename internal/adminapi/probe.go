package adminapi

// 本文件是渠道表单要的两个只读能力：适配器枚举与上游模型探测。
//
// 两个都挂在 GroupChannels 下——它们是「配一条渠道」时的输入，不是独立的
// 资源。枚举读的是适配器注册表（与写渠道时的类型校验同一份来源），探测走
// channels.ModelProbe（明文凭证只在那一个包里解）。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/upstream"
)

// 探测的限流：同一调用方两次之间至少隔 probeInterval，一分钟最多
// probePerMinute 次。
//
// 探测是唯一一条「服务器替调用方打一个任意地址」的路径。它不打私网拦截
// （自建上游在内网是真实用法），所以这里用限流把「扫一片地址」的成本压住。
// 商家按用户 id 计；内部管理面与管理员的会话没有归属，按全局计。
const (
	probeInterval  = 2 * time.Second
	probePerMinute = 10
)

// probeLimiter 是进程内的滑动窗口限流。
//
// 不落库、不跨进程：一次重启把窗口清空是可以接受的代价——它挡的是「短时间内
// 把一个表单点成扫描器」，不是计费或配额。
type probeLimiter struct {
	mu     sync.Mutex
	recent map[int64][]time.Time
}

func newProbeLimiter() *probeLimiter {
	return &probeLimiter{recent: make(map[int64][]time.Time)}
}

// allow 判断这次探测放不放行；不放行时给出还要等多久。
func (l *probeLimiter) allow(key int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-time.Minute)
	times := l.recent[key]
	kept := times[:0]
	for _, at := range times {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	times = kept

	var wait time.Duration
	if n := len(times); n > 0 {
		if since := now.Sub(times[n-1]); since < probeInterval {
			wait = probeInterval - since
		}
	}
	if len(times) >= probePerMinute {
		// 等最早那一格滑出窗口。
		if w := times[len(times)-probePerMinute].Add(time.Minute).Sub(now); w > wait {
			wait = w
		}
	}
	if wait > 0 {
		l.recent[key] = times
		return false, wait
	}
	l.recent[key] = append(times, now)
	return true, 0
}

// adapterJSON 是枚举里的一行。
type adapterJSON struct {
	Type     string `json:"type"`
	Protocol string `json:"protocol"`
	Endpoint string `json:"endpoint"`
}

type adaptersJSON struct {
	Items []adapterJSON `json:"items"`
}

// listAdapters 列出内置适配器。
//
// 只给事实，不给能力位：「这个类型能不能列模型」由探测的运行时结果回答。
// 静态能力表会是第二份判据，而它迟早与适配器漂移（见 ProbeRegistry 的注释）。
func (a *API) listAdapters(w http.ResponseWriter, _ *http.Request) {
	registry, err := a.probeRegistry()
	if err != nil {
		a.logger().Error("装配探测用适配器失败", "error", err)
		a.writeError(w, http.StatusServiceUnavailable, msgUnavailable)
		return
	}
	types := registry.Types()
	items := make([]adapterJSON, 0, len(types))
	for _, name := range types {
		adapter, lookupErr := registry.Lookup(name)
		if lookupErr != nil {
			// Types 与 Lookup 读的是同一张表，这里不可能落空；落空也不该让
			// 整个枚举失败——少一行比一条都没有好。
			a.logger().Error("适配器注册表不一致", "type", name, "error", lookupErr)
			continue
		}
		items = append(items, adapterJSON{
			Type:     adapter.Type(),
			Protocol: string(adapter.Protocol()),
			Endpoint: string(adapter.Endpoint()),
		})
	}
	a.writeJSON(w, http.StatusOK, adaptersJSON{Items: items})
}

// probeRegistry 惰性建一次探测用的适配器注册表。
//
// 惰性而不是在 New 里建：读路径的用例不会调到探测，而每次 New 都造四个
// 适配器与一个 HTTP 客户端没有意义。出错时缓存错误，下一次同样报错，不会
// 每次请求重试一遍。
func (a *API) probeRegistry() (*channel.Registry, error) {
	a.probeOnce.Do(func() {
		a.probeReg, a.probeErr = upstream.ProbeRegistry()
	})
	return a.probeReg, a.probeErr
}

// probeModelsRequest 是探测的请求体。
type probeModelsRequest struct {
	ChannelID  int64  `json:"channel_id"`
	Type       string `json:"type"`
	BaseURL    string `json:"base_url"`
	Credential string `json:"credential"`
}

type probeModelsJSON struct {
	Supported bool     `json:"supported"`
	Models    []string `json:"models"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated"`
}

// probeModels 拿一份地址与凭证向上游问一次模型清单。
func (a *API) probeModels(w http.ResponseWriter, r *http.Request) {
	var req probeModelsRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	if req.ChannelID < 0 {
		a.writeError(w, http.StatusBadRequest, "channel_id must not be negative")
		return
	}
	owner := ownerScope(r.Context())
	key := int64(0)
	if owner != nil {
		key = *owner
	}
	if ok, wait := a.probes.allow(key, time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		a.writeError(w, http.StatusTooManyRequests,
			"too many probe requests; wait before trying again")
		return
	}
	registry, err := a.probeRegistry()
	if err != nil {
		a.logger().Error("装配探测用适配器失败", "error", err)
		a.writeError(w, http.StatusServiceUnavailable, msgUnavailable)
		return
	}

	probe := channels.NewModelProbe(a.db, a.sealer, registry)
	res, err := probe.Probe(r.Context(), channels.ProbeRequest{
		ChannelID:  req.ChannelID,
		Type:       req.Type,
		BaseURL:    req.BaseURL,
		Credential: req.Credential,
		OwnerID:    owner,
	})
	if err != nil {
		a.failProbe(w, r, err)
		return
	}
	if res.Models == nil {
		res.Models = []string{}
	}
	a.writeJSON(w, http.StatusOK, probeModelsJSON{
		Supported: res.Supported,
		Models:    res.Models,
		Total:     res.Total,
		Truncated: res.Truncated,
	})
}

// upstreamBodySnippet 是错误体回显的长度上限。
//
// 探测的错误体是上游给的原文，长度由对方决定；截短之后再进响应，免得一次
// 探测把几 MiB 的 HTML 错误页当正文回给浏览器。
const upstreamBodySnippet = 200

// failProbe 把一次探测的失败翻成状态码。
//
// 与 fail 分开：那条默认「认不出来的当输入错（400）」，而探测的失败大多
// 来自上游——报成 400 会让调用方去改一个没写错的字段。
func (a *API) failProbe(w http.ResponseWriter, r *http.Request, err error) {
	var up *channel.ErrUpstream
	switch {
	case errors.Is(err, channels.ErrChannelNotFound):
		a.writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, channel.ErrUnknownType):
		a.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, channels.ErrNoMasterKey):
		// 与写库故障分开：这个部署没有主密钥，换一次请求也不会好。
		a.writeError(w, http.StatusServiceUnavailable,
			"the master key is not configured on this instance")
	case errors.Is(err, httpx.ErrBodyTooLarge):
		// 与「上游挂了」分开给码：同一个 502，处置完全不同。
		a.writeErrorCode(w, http.StatusBadGateway, "upstream_response_too_large",
			"the upstream response exceeded the 8 MiB limit")
	case errors.Is(err, context.DeadlineExceeded):
		a.writeError(w, http.StatusGatewayTimeout,
			fmt.Sprintf("the upstream did not answer within %s", upstream.ProbeTimeout))
	case errors.As(err, &up):
		a.logger().WarnContext(r.Context(), "上游模型探测失败",
			"status", up.StatusCode, "got_response", up.GotResponse)
		a.writeError(w, http.StatusBadGateway, upstreamFailureMessage(up))
	default:
		a.fail(w, err)
	}
}

// upstreamFailureMessage 是 502 的正文：状态码 + 截短的响应体。
//
// 凭证不在里面：channels.ModelProbe 已经把这次用的明文从 Body 里换掉了
// （见 redactUpstream）。
func upstreamFailureMessage(up *channel.ErrUpstream) string {
	switch {
	case up.GotResponse:
		message := fmt.Sprintf("the upstream returned %d", up.StatusCode)
		if body := strings.TrimSpace(up.Body); body != "" {
			message += ": " + truncateRunes(body, upstreamBodySnippet)
		}
		return message
	default:
		return "the upstream call failed"
	}
}

// truncateRunes 按字符截断，避免把多字节字符切成半个。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
