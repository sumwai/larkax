package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/observe"
)

// 本文件是商家运营页的概览（GroupOverview）与上游清单。
//
// 它们只在控制台的 merchant 组下注册：每条读都要知道「这是谁在问」，否则
// 一个商家会看到全站的数。内部管理面没有归属上下文，所以这一组不随空
// Groups（= 全部资源）出现，见 New 里的注册条件。

const (
	// unhealthyWindow 是判异常的观察窗：最近 24 小时，不是站点当地的今天。
	// 两者只有在跨零点时才不同——「今天还没有请求」对刚开门的一小时是正常
	// 的，而「24 小时没有请求」才是一个值得看的信号。
	unhealthyWindow = 24 * time.Hour

	// failureRateMinRequests 是判失败率的最小样本：少于这个数的窗口里，
	// 一次失败就能让比例超过阈值，那不是「渠道有问题」，是「样本太少」。
	failureRateMinRequests int64 = 20
	// failureRateThreshold 是失败率阈值（含）。
	failureRateThreshold = 0.2

	// listDefaultLimit 与 listMaxLimit 是运营页清单（上游、我的模型）一页的条数。
	listDefaultLimit = 50
	listMaxLimit     = 200
)

// 异常理由的枚举。取值稳定：页面与文档都按这些字面量解释。
const (
	reasonFailureRate         = "failure_rate"
	reasonQuotaState          = "quota_state"
	reasonProbeFailed         = "probe_failed"
	reasonConsecutiveFailures = "consecutive_failures"
	reasonNoRequests          = "no_requests_24h"
)

// todayWindow 返回站点当地今天的 [起, 止)。
//
// 是「当地日历日」，不是「最近 24 小时」：跨零点时两者给的不是同一批行，
// 而运营页上的「今日」指向前者。用 AddDate 而不是加 24h，夏令时切换那天的
// 一天可能不是 24 小时。
func todayWindow(now time.Time, loc *time.Location) (time.Time, time.Time) {
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

// ownerScope 把上下文里的归属折成「要不要按 owner 筛」。
//
// nil 表示不筛（admin 进 merchant 组时看到全局）；这与 resources.go 里
// 每个 handler 的 OwnerFromContext 分支是同一条规则，不要另造一套。
func ownerScope(ctx context.Context) *int64 {
	if id, ok := OwnerFromContext(ctx); ok {
		return &id
	}
	return nil
}

// unhealthyChannelJSON 是概览里的一条异常渠道。
type unhealthyChannelJSON struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Reasons []string `json:"reasons"`
}

type todayJSON struct {
	Requests int64  `json:"requests"`
	Cost     string `json:"cost"`
}

type overviewJSON struct {
	Channels          int64                  `json:"channels"`
	ModelsInSupply    int64                  `json:"models_in_supply"`
	Today             todayJSON              `json:"today"`
	UnhealthyChannels []unhealthyChannelJSON `json:"unhealthy_channels"`
}

func (a *API) merchantOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := ownerScope(ctx)
	now := time.Now()
	from, to := todayWindow(now, a.loc)

	channelCount, err := a.channels.CountAlive(ctx, owner)
	if err != nil {
		a.failMerchantRead(w, r, "渠道数", err)
		return
	}
	modelsInSupply, err := a.channels.CountSuppliedModels(ctx, owner)
	if err != nil {
		a.failMerchantRead(w, r, "在供模型数", err)
		return
	}
	usage, err := a.ledger.ChannelUsageTotals(ctx, owner, from, to)
	if err != nil {
		a.failMerchantRead(w, r, "今日用量", err)
		return
	}
	health, err := a.merchantHealth(ctx, owner, now.Add(-unhealthyWindow))
	if err != nil {
		a.failMerchantRead(w, r, "渠道健康", err)
		return
	}

	// 从空切片开始：没有异常渠道是正常结果，JSON 里该是 []，不是 null。
	unhealthy := make([]unhealthyChannelJSON, 0)
	for _, h := range health {
		reasons := unhealthyReasons(h)
		if len(reasons) == 0 {
			continue
		}
		unhealthy = append(unhealthy, unhealthyChannelJSON{
			ID: h.ID, Name: h.Name, Reasons: reasons,
		})
	}

	a.writeJSON(w, http.StatusOK, overviewJSON{
		Channels:          channelCount,
		ModelsInSupply:    modelsInSupply,
		Today:             todayJSON{Requests: usage.Requests, Cost: usage.Cost.String()},
		UnhealthyChannels: unhealthy,
	})
}

// unhealthyReasons 给出一条渠道被判异常的全部理由。
//
// 返回空表示这条渠道不该出现在异常清单里。多条理由同时成立时全部列出：
// 页面要回答的是「为什么它算异常」，只报第一条会让人以为是另一条已经好了。
func unhealthyReasons(h channels.ChannelHealth) []string {
	reasons := make([]string, 0, 2)
	if h.Requests >= failureRateMinRequests &&
		float64(h.Failures)/float64(h.Requests) >= failureRateThreshold {
		reasons = append(reasons, reasonFailureRate)
	}
	switch h.Health {
	case "degraded", "drained":
		reasons = append(reasons, reasonQuotaState)
	}
	if h.LastProbeStatus != nil && *h.LastProbeStatus == "error" {
		reasons = append(reasons, reasonProbeFailed)
	}
	if h.ConsecutiveFailures > 0 {
		reasons = append(reasons, reasonConsecutiveFailures)
	}
	// 窗口里一次请求都没有：可能是新渠道，也可能是掉线了很久。两种情况
	// 都值得运营看一眼，理由名里写明是「24h 没有请求」而不是「异常」。
	if h.Requests == 0 {
		reasons = append(reasons, reasonNoRequests)
	}
	return reasons
}

func (a *API) merchantHealth(ctx context.Context, owner *int64,
	since time.Time,
) ([]channels.ChannelHealth, error) {
	if owner != nil {
		return a.channels.HealthOwned(ctx, *owner, since)
	}
	return a.channels.Health(ctx, since, false)
}

// upstreamChannelJSON 是上游分组里的一条渠道。
//
// 字段是**逐个列出**的：这一组与凭证在同一张表上，任何一次「直接把
// ChannelInfo 序列化出去」都会把 Hint、CredentialKeys 一并带走。只写这里
// 列出的字段，测试再按 key 集合断言一遍。
type upstreamChannelJSON struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Health string `json:"health"`
}

// upstreamJSON 是一条上游（按 type + base_url 分组）。
type upstreamJSON struct {
	Type        string                `json:"type"`
	BaseURL     string                `json:"base_url"`
	Health      string                `json:"health"`
	Requests    int64                 `json:"requests"`
	Failures    int64                 `json:"failures"`
	FailureRate float64               `json:"failure_rate"`
	Channels    []upstreamChannelJSON `json:"channels"`
}

type upstreamsJSON struct {
	Items     []upstreamJSON `json:"items"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
}

func (a *API) merchantUpstreams(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, err := parseListLimit(r.URL.Query().Get("limit"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	owner := ownerScope(ctx)

	var infos []channels.ChannelInfo
	if owner != nil {
		infos, err = a.channels.ListOwned(ctx, *owner)
	} else {
		infos, err = a.channels.List(ctx)
	}
	if err != nil {
		a.failMerchantRead(w, r, "渠道清单", err)
		return
	}
	health, err := a.merchantHealth(ctx, owner, time.Now().Add(-unhealthyWindow))
	if err != nil {
		a.failMerchantRead(w, r, "渠道健康", err)
		return
	}

	byID := make(map[int64]channels.ChannelHealth, len(health))
	for _, h := range health {
		byID[h.ID] = h
	}
	groups := groupUpstreams(infos, byID)

	total := len(groups)
	truncated := total > limit
	if truncated {
		groups = groups[:limit]
	}
	a.writeJSON(w, http.StatusOK, upstreamsJSON{
		Items: groups, Total: total, Truncated: truncated,
	})
}

// groupUpstreams 把渠道按 (type, base_url) 分组。
//
// 同一个 base_url 上的多条渠道是**多个账号**（各自一份凭证），所以它们合成
// 一条上游、组内列渠，而不是去重成一条渠道。排序固定为 (type, base_url)、
// 组内按 id：没有固定次序时，前端的分页与快照对比都会随机抖动。
func groupUpstreams(infos []channels.ChannelInfo,
	health map[int64]channels.ChannelHealth,
) []upstreamJSON {
	type key struct{ typ, baseURL string }
	index := make(map[key]int)
	out := make([]upstreamJSON, 0)

	for _, info := range infos {
		k := key{typ: info.Type, baseURL: info.BaseURL}
		h := health[info.ID]
		// 清单与健康是两条语句：两步之间被软删或改了归属的渠道，清单里有、
		// 健康里没有，零值会是空串。空串不在对外那四档里，出口归一成 unknown
		// ——它与「还没探测过」是同一件事。
		if h.Health == "" {
			h.Health = channels.HealthUnknown
		}
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, upstreamJSON{
				Type: info.Type, BaseURL: info.BaseURL, Health: h.Health,
				Channels: make([]upstreamChannelJSON, 0, 1),
			})
		}
		group := &out[i]
		group.Requests += h.Requests
		group.Failures += h.Failures
		if healthRank(h.Health) > healthRank(group.Health) {
			group.Health = h.Health
		}
		group.Channels = append(group.Channels, upstreamChannelJSON{
			ID: info.ID, Name: info.Name, Status: info.Status, Health: h.Health,
		})
	}

	for i := range out {
		sort.Slice(out[i].Channels, func(x, y int) bool {
			return out[i].Channels[x].ID < out[i].Channels[y].ID
		})
		if out[i].Requests > 0 {
			out[i].FailureRate = float64(out[i].Failures) / float64(out[i].Requests)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].BaseURL < out[j].BaseURL
	})
	return out
}

// healthRank 给健康档位排严重度：组内取最差一档。
//
// unknown 排在 healthy 之后：一个还没探测过的渠道不该被说成「健康」，
// 但它也不是 degraded。
func healthRank(health string) int {
	switch health {
	case "healthy":
		return 0
	case "degraded":
		return 2
	case "drained":
		return 3
	default: // unknown
		return 1
	}
}

// parseListLimit 解析运营页清单的 limit。
func parseListLimit(raw string) (int, error) {
	if raw == "" {
		return listDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > listMaxLimit {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", listMaxLimit)
	}
	return n, nil
}

// failMerchantRead 是运营页读路径失败的统一出口。
//
// 原因只进日志：库报错可能带出连接串与表名。调用方能做的是稍后重试。
func (a *API) failMerchantRead(w http.ResponseWriter, r *http.Request, step string, err error) {
	a.logger().Error("读取商家运营数据失败", "step", step, "error", err,
		"request_id", string(observe.RequestIDFromContext(r.Context())))
	a.writeError(w, http.StatusServiceUnavailable, msgUnavailable)
}
