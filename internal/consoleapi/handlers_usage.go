package consoleapi

import (
	"errors"
	"net/http"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger/store"
)

// 本文件是用量的四个读：汇总、序列、分组、请求日志。
//
// 它们共用同一组查询参数（from/to/key_id/model），各自再收一两个自己的。
// 时间窗与筛选条件在这里解析一次，再交给 store 的聚合方法——聚合口径不在
// 本包，这一层只决定「问哪一段、按什么分」。

// tokensJSON 是五个 token 槽位。
//
// 与 usage_events 的列同名同义；`input` / `output` 对应 prompt / completion，
// 因为界面上的说法是前者。
type tokensJSON struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

// newTokens 把聚合结果里的 token 字段折成响应形状。
func newTokens(t store.UsageTotals) tokensJSON {
	return tokensJSON{
		Input:      t.PromptTokens,
		Output:     t.CompletionTokens,
		CacheRead:  t.CacheReadTokens,
		CacheWrite: t.CacheWriteTokens,
		Reasoning:  t.ReasoningTokens,
	}
}

// usageTotalsJSON 是一段用量的总额。
type usageTotalsJSON struct {
	Cost string `json:"cost"`
	// PlanReservedAmount 是这段用量占着的额度包面值上界，与 Cost 并列。
	//
	// Cost 是这笔用量实际收了多少；PlanReservedAmount 是预留时占住的面值
	// 上界，已随结算释放。两者不合并，也不相加——它们是同一笔消费的实收与
	// 上界，相加会把同一笔钱算两遍。行级的 usage/requests 用同一个字段名。
	PlanReservedAmount string     `json:"plan_reserved_amount"`
	Requests           int64      `json:"requests"`
	Tokens             tokensJSON `json:"tokens"`
}

// usagePathJSON 是一档计费路径。
type usagePathJSON struct {
	Path     string `json:"path"`
	Cost     string `json:"cost"`
	Requests int64  `json:"requests"`
}

// usageSummaryResponse 是汇总响应。
type usageSummaryResponse struct {
	From         string          `json:"from"`
	To           string          `json:"to"`
	Currency     string          `json:"currency"`
	Totals       usageTotalsJSON `json:"totals"`
	BillingPaths []usagePathJSON `json:"billing_paths"`
}

// parseUsageWindow 解析用量端点共用的时间窗与筛选条件。
//
// 错误已经是可以直接给调用方看的一句话；调用方只需要把它写进 400。
func (a *API) parseUsageWindow(r *http.Request, userID int64) (store.UsageWindow, error) {
	from, to, err := a.parseWindow(r, a.now())
	if err != nil {
		return store.UsageWindow{}, err
	}
	keyID, err := queryKeyID(r)
	if err != nil {
		return store.UsageWindow{}, err
	}
	return store.UsageWindow{
		UserID:   userID,
		From:     from,
		To:       to,
		APIKeyID: keyID,
		Model:    r.URL.Query().Get("model"),
	}, nil
}

// handleUsageSummary 返回一段时间里的总额与计费路径分布。
func (a *API) handleUsageSummary(w http.ResponseWriter, r *http.Request, session auth.Session) {
	window, err := a.parseUsageWindow(r, session.UserID)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	summary, err := a.deps.usage.UsageSummary(r.Context(), window)
	if err != nil {
		a.usageError(w, r, err)
		return
	}

	paths := make([]usagePathJSON, 0, len(summary.Paths))
	for _, path := range summary.Paths {
		paths = append(paths, usagePathJSON{
			Path:     path.Path,
			Cost:     path.Cost.String(),
			Requests: path.Requests,
		})
	}

	a.ok(w, http.StatusOK, usageSummaryResponse{
		From:     formatTime(window.From),
		To:       formatTime(window.To),
		Currency: a.currency,
		Totals: usageTotalsJSON{
			Cost:               summary.Totals.Cost.String(),
			PlanReservedAmount: summary.Totals.PlanReservedAmount.String(),
			Requests:           summary.Totals.Requests,
			Tokens:             newTokens(summary.Totals),
		},
		BillingPaths: paths,
	})
}

// usagePointJSON 是序列上的一个桶。
type usagePointJSON struct {
	Start              string     `json:"start"`
	Cost               string     `json:"cost"`
	PlanReservedAmount string     `json:"plan_reserved_amount"`
	Requests           int64      `json:"requests"`
	Tokens             tokensJSON `json:"tokens"`
}

// usageSeriesResponse 是序列响应。
type usageSeriesResponse struct {
	Interval string           `json:"interval"`
	TZ       string           `json:"tz"`
	Points   []usagePointJSON `json:"points"`
}

// handleUsageSeries 返回按天或按小时分桶的用量。
func (a *API) handleUsageSeries(w http.ResponseWriter, r *http.Request, session auth.Session) {
	window, err := a.parseUsageWindow(r, session.UserID)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "day"
	}

	points, err := a.deps.usage.UsageSeries(r.Context(), window, interval, a.loc)
	if err != nil {
		a.usageError(w, r, err)
		return
	}

	out := make([]usagePointJSON, 0, len(points))
	for _, point := range points {
		out = append(out, usagePointJSON{
			Start:              formatTime(point.Start),
			Cost:               point.Cost.String(),
			PlanReservedAmount: point.PlanReservedAmount.String(),
			Requests:           point.Requests,
			Tokens:             newTokens(point.UsageTotals),
		})
	}

	a.ok(w, http.StatusOK, usageSeriesResponse{
		Interval: interval,
		TZ:       a.loc.String(),
		Points:   out,
	})
}

// usageModelGroupJSON 是按模型分组的一行。
type usageModelGroupJSON struct {
	Model              string     `json:"model"`
	Cost               string     `json:"cost"`
	PlanReservedAmount string     `json:"plan_reserved_amount"`
	Requests           int64      `json:"requests"`
	Tokens             tokensJSON `json:"tokens"`
}

// usageKeyGroupJSON 是按 key 分组的一行。
type usageKeyGroupJSON struct {
	KeyID              *int64     `json:"key_id"`
	KeyName            string     `json:"key_name"`
	Cost               string     `json:"cost"`
	PlanReservedAmount string     `json:"plan_reserved_amount"`
	Requests           int64      `json:"requests"`
	Tokens             tokensJSON `json:"tokens"`
}

// usageOtherJSON 是「其余各组之和」。
type usageOtherJSON struct {
	Cost               string     `json:"cost"`
	PlanReservedAmount string     `json:"plan_reserved_amount"`
	Requests           int64      `json:"requests"`
	Tokens             tokensJSON `json:"tokens"`
}

// usageBreakdownResponse 是分组响应。
type usageBreakdownResponse struct {
	GroupBy string         `json:"group_by"`
	Items   []any          `json:"items"`
	Other   usageOtherJSON `json:"other"`
}

// handleUsageBreakdown 按模型或 key 分组。
func (a *API) handleUsageBreakdown(w http.ResponseWriter, r *http.Request, session auth.Session) {
	window, err := a.parseUsageWindow(r, session.UserID)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		a.invalid(w, r, errors.New("group_by 必须给出（model 或 key）"))
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"),
		store.UsageBreakdownDefaultLimit, store.UsageBreakdownMaxLimit)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	breakdown, err := a.deps.usage.UsageBreakdown(r.Context(), window, groupBy, limit)
	if err != nil {
		a.usageError(w, r, err)
		return
	}

	items := make([]any, 0, len(breakdown.Items))
	for _, group := range breakdown.Items {
		switch groupBy {
		case "model":
			items = append(items, usageModelGroupJSON{
				Model:              group.Model,
				Cost:               group.Cost.String(),
				PlanReservedAmount: group.PlanReservedAmount.String(),
				Requests:           group.Requests,
				Tokens:             newTokens(group.UsageTotals),
			})
		default:
			items = append(items, usageKeyGroupJSON{
				KeyID:              group.KeyID,
				KeyName:            group.KeyName,
				Cost:               group.Cost.String(),
				PlanReservedAmount: group.PlanReservedAmount.String(),
				Requests:           group.Requests,
				Tokens:             newTokens(group.UsageTotals),
			})
		}
	}
	a.ok(w, http.StatusOK, usageBreakdownResponse{
		GroupBy: groupBy,
		Items:   items,
		Other: usageOtherJSON{
			Cost:               breakdown.Other.Cost.String(),
			PlanReservedAmount: breakdown.Other.PlanReservedAmount.String(),
			Requests:           breakdown.Other.Requests,
			Tokens:             newTokens(breakdown.Other.UsageTotals),
		},
	})
}

// requestKeyJSON 是请求日志里那一对 key 字段。
type requestKeyJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// usageRequestJSON 是请求日志里的一行。
type usageRequestJSON struct {
	ID        int64           `json:"id"`
	RequestID string          `json:"request_id"`
	Model     string          `json:"model"`
	Key       *requestKeyJSON `json:"key"`
	Status    string          `json:"status"`
	Cost      string          `json:"cost"`
	// PlanReservedAmount 是这笔请求占着的额度包面值，与 Cost 并列。
	//
	// Cost 在这笔请求上是钱包口径的预留额（pending）或最终实收（settled）；
	// PlanReservedAmount 是预留时占住的面值上界，结算后这笔占用随行退出
	// pending。两者不合并，也不相加——它们是同一笔消费的实收与上界，相加会
	// 把同一笔钱算两遍。
	PlanReservedAmount string     `json:"plan_reserved_amount"`
	Currency           string     `json:"currency"`
	Tokens             tokensJSON `json:"tokens"`
	BillingPath        *string    `json:"billing_path"`
	Streamed           bool       `json:"streamed"`
	Protocol           string     `json:"protocol"`
	DurationMS         *int64     `json:"duration_ms"`
	StartedAt          string     `json:"started_at"`
	SettledAt          *string    `json:"settled_at"`
}

// usageRequestsResponse 是请求日志的一页。
type usageRequestsResponse struct {
	Items      []usageRequestJSON `json:"items"`
	NextCursor *string            `json:"next_cursor"`
}

// usageError 把 store 的两种失败分开：参数问题 400，库故障 503。
//
// 判据是 store.ErrInvalidQuery 这个哨兵，不是错误文案（见它的注释）。
func (a *API) usageError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrInvalidQuery) {
		a.invalid(w, r, err)
		return
	}
	a.unavailable(w, r, err, "查询用量失败")
}

// handleUsageRequests 读一页请求日志。
func (a *API) handleUsageRequests(w http.ResponseWriter, r *http.Request, session auth.Session) {
	window, err := a.parseUsageWindow(r, session.UserID)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	limit, err := parseLimit(r.URL.Query().Get("limit"),
		store.RequestPageDefaultLimit, store.RequestPageMaxLimit)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	var cursor *store.RequestCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		at, id, cursorErr := decodeCursor(raw)
		if cursorErr != nil {
			a.invalid(w, r, cursorErr)
			return
		}
		cursor = &store.RequestCursor{StartedAt: at, ID: id}
	}

	page, err := a.deps.usage.UsageRequests(r.Context(), store.UsageRequestQuery{
		UserID:   window.UserID,
		From:     window.From,
		To:       window.To,
		APIKeyID: window.APIKeyID,
		Model:    window.Model,
		Status:   r.URL.Query().Get("status"),
		Cursor:   cursor,
		Limit:    limit,
	})
	if err != nil {
		a.usageError(w, r, err)
		return
	}

	items := make([]usageRequestJSON, 0, len(page.Requests))
	for _, req := range page.Requests {
		item := usageRequestJSON{
			ID:                 req.ID,
			RequestID:          req.RequestID,
			Model:              req.Model,
			Status:             req.Status,
			Cost:               req.Cost.String(),
			PlanReservedAmount: req.PlanReservedAmount.String(),
			Currency:           req.Asset,
			Tokens: newTokens(store.UsageTotals{
				PromptTokens:     req.PromptTokens,
				CompletionTokens: req.CompletionTokens,
				CacheReadTokens:  req.CacheReadTokens,
				CacheWriteTokens: req.CacheWriteTokens,
				ReasoningTokens:  req.ReasoningTokens,
			}),
			BillingPath: req.BillingPath,
			Streamed:    req.Streamed,
			Protocol:    req.Protocol,
			DurationMS:  req.DurationMS,
			StartedAt:   formatTime(req.StartedAt),
			SettledAt:   optionalTime(req.SettledAt),
		}
		if req.KeyID != nil {
			item.Key = &requestKeyJSON{ID: *req.KeyID, Name: req.KeyName}
		}
		items = append(items, item)
	}

	out := usageRequestsResponse{Items: items}
	if page.More && len(page.Requests) > 0 {
		last := page.Requests[len(page.Requests)-1]
		next := encodeCursor(last.StartedAt, last.ID)
		out.NextCursor = &next
	}
	a.ok(w, http.StatusOK, out)
}
