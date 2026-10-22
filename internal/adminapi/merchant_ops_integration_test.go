package adminapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// 本文件是商家运营页两个端点的真库用例。
//
// 它要证明的是「归属真的进了 SQL」与「窗口/口径真的按文档那样切」：
// 单测只能证明折行与理由枚举，证明不了跨租户不串、时区切日、以及
// 售价与上游成本不会取错列。

// merchantSurface 装一份控制台 merchant 组的形状：渠道 + 计价 + 概览。
func merchantSurface(t *testing.T, timezone string) http.Handler {
	t.Helper()
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatalf("加载时区 %q 失败: %v", timezone, err)
	}
	return testSurface(t, Config{
		Prefix:    "/api/v1/merchant/",
		Authorize: func(next http.Handler) http.Handler { return next },
		Groups:    []string{GroupChannels, GroupPricing, GroupOverview},
		Loc:       loc,
	})
}

// merchantCall 发一次商家组请求；owner > 0 时按会话 merchant 的方式注入归属。
func merchantCall(t *testing.T, h http.Handler, owner int64, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	ctx := context.Background()
	if owner > 0 {
		ctx = WithOwner(ctx, owner)
	}
	req := httptest.NewRequestWithContext(ctx, method, path, reader)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// merchantChannel 以某位商家的身份建一条渠道。
func merchantChannel(t *testing.T, h http.Handler, owner int64,
	name, typ, baseURL string, models []string,
) int64 {
	t.Helper()
	rec := merchantCall(t, h, owner, http.MethodPost, "/api/v1/merchant/channels", map[string]any{
		"name": name, "type": typ, "base_url": baseURL,
		"credential": "sk-placeholder", "models": models,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("建渠道 %q 状态码 = %d；正文：%s", name, rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &created)
	return created.ID
}

// seedCatalogModel 往模型目录里放一行：在供模型数的判据会 join 这张表。
func seedCatalogModel(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO model_catalog (canonical_name, display_name) VALUES ($1, $1)`,
		name)
	if err != nil {
		t.Fatalf("写入模型目录失败: %v", err)
	}
}

// insertRollup 往小时桶里写一行健康计数。
func insertRollup(t *testing.T, db *sql.DB, channelID int64,
	bucket time.Time, requests, successes, failures int64,
) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO channel_health_rollup
			(channel_id, bucket, requests, successes, failures, cost)
		VALUES ($1, $2, $3, $4, $5, 0)
		ON CONFLICT (channel_id, model, bucket) DO UPDATE
		   SET requests = EXCLUDED.requests,
		       successes = EXCLUDED.successes,
		       failures = EXCLUDED.failures`,
		channelID, bucket, requests, successes, failures)
	if err != nil {
		t.Fatalf("写健康桶失败: %v", err)
	}
}

// insertQuotaState 写一行额度状态；probe 为空表示不写探测结论。
func insertQuotaState(t *testing.T, db *sql.DB, channelID int64, health, probe string) {
	t.Helper()
	var probeArg any
	if probe != "" {
		probeArg = probe
	}
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO channel_quota_state (channel_id, health, last_probe_status)
		VALUES ($1, $2, $3)
		ON CONFLICT (channel_id) DO UPDATE
		   SET health = EXCLUDED.health,
		       last_probe_status = EXCLUDED.last_probe_status`,
		channelID, health, probeArg)
	if err != nil {
		t.Fatalf("写额度状态失败: %v", err)
	}
}

// insertUsage 写一条用量事件。createdAt 为零值时由库取 now()。
func insertUsage(t *testing.T, db *sql.DB, userID, channelID int64,
	requestID, model, cost, upstreamCost string, createdAt time.Time,
) {
	t.Helper()
	args := []any{requestID, userID, channelID, model, cost, upstreamCost}
	columns := "request_id, user_id, channel_id, canonical_model, billing_path, cost, upstream_cost"
	values := "$1, $2, $3, $4, 'measured', $5, $6"
	if !createdAt.IsZero() {
		args = append(args, createdAt)
		columns += ", created_at"
		values += ", $7"
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO usage_events (`+columns+`) VALUES (`+values+`)`, args...); err != nil {
		t.Fatalf("写用量事件失败: %v", err)
	}
}

// channelModel 造一个只用于建渠道的模型名，并保证目录里有它。
//
// 建渠道的入口要求 models 非空（空数组永远选不中），所以运维页这组用例
// 即便不关心模型也得给一个。
func channelModel(t *testing.T, db *sql.DB) []string {
	t.Helper()
	name := uniqueName("model-ops")
	seedCatalogModel(t, db, name)
	return []string{name}
}

type overviewTestResponse struct {
	Channels       int64 `json:"channels"`
	ModelsInSupply int64 `json:"models_in_supply"`
	Today          struct {
		Requests int64  `json:"requests"`
		Cost     string `json:"cost"`
	} `json:"today"`
	UnhealthyChannels []struct {
		ID      int64    `json:"id"`
		Name    string   `json:"name"`
		Reasons []string `json:"reasons"`
	} `json:"unhealthy_channels"`
}

func (o overviewTestResponse) reasonsByID() map[int64][]string {
	out := make(map[int64][]string, len(o.UnhealthyChannels))
	for _, item := range o.UnhealthyChannels {
		out[item.ID] = item.Reasons
	}
	return out
}

func getOverview(t *testing.T, h http.Handler, owner int64) overviewTestResponse {
	t.Helper()
	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/overview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("概览状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body overviewTestResponse
	decode(t, rec, &body)
	return body
}

// TestMerchantOverviewScopesToOwner 跨租户不串：A 的概览只有 A 的数，
// 异常清单里不出现 B 的渠道；admin（不注入归属）看到的是全局。
func TestMerchantOverviewScopesToOwner(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)

	ownerA := seedUser(t, db, "m-owned-a-")
	ownerB := seedUser(t, db, "m-owned-b-")

	modelA := uniqueName("model-a")
	modelB := uniqueName("model-b")
	seedCatalogModel(t, db, modelA)
	seedCatalogModel(t, db, modelB)
	channelA := merchantChannel(t, h, ownerA, uniqueName("ch-a"), "openai",
		"https://a.example.com/v1", []string{modelA})
	channelB := merchantChannel(t, h, ownerB, uniqueName("ch-b"), "openai",
		"https://b.example.com/v1", []string{modelB})

	bucket := time.Now().UTC().Truncate(time.Hour)
	insertRollup(t, db, channelA, bucket, 100, 100, 0)
	insertRollup(t, db, channelB, bucket, 100, 100, 0)
	insertUsage(t, db, ownerA, channelA, uniqueName("req-a"), modelA, "1.5", "9.9", time.Time{})
	insertUsage(t, db, ownerB, channelB, uniqueName("req-b"), modelB, "2.5", "9.9", time.Time{})

	// B 的渠道连续失败：它只该出现在 B（以及 admin）的异常清单里。
	if _, err := db.ExecContext(t.Context(),
		`UPDATE channels SET consecutive_failures = 3 WHERE id = $1`, channelB); err != nil {
		t.Fatalf("置连续失败失败: %v", err)
	}

	a := getOverview(t, h, ownerA)
	if a.Channels != 1 || a.ModelsInSupply != 1 {
		t.Errorf("A 的概览 = %d 渠道 / %d 模型，期望 1 / 1", a.Channels, a.ModelsInSupply)
	}
	if a.Today.Requests != 1 || a.Today.Cost != "1.50000000" {
		t.Errorf("A 的今日 = %d 次 / %q，期望 1 次 / 1.50000000", a.Today.Requests, a.Today.Cost)
	}
	if _, ok := a.reasonsByID()[channelB]; ok {
		t.Errorf("A 的异常清单里出现了 B 的渠道 %d：%+v", channelB, a.UnhealthyChannels)
	}
	if len(a.UnhealthyChannels) != 0 {
		t.Errorf("A 的异常清单 = %+v，期望空", a.UnhealthyChannels)
	}

	b := getOverview(t, h, ownerB)
	if b.Channels != 1 || b.Today.Requests != 1 || b.Today.Cost != "2.50000000" {
		t.Errorf("B 的概览 = %+v，期望 1 渠道 / 1 次 / 2.50000000", b)
	}
	if reasons, ok := b.reasonsByID()[channelB]; !ok || !containsReason(reasons, reasonConsecutiveFailures) {
		t.Errorf("B 的异常清单 = %+v，期望含渠道 %d 与 %s", b.UnhealthyChannels, channelB, reasonConsecutiveFailures)
	}

	admin := getOverview(t, h, 0)
	if admin.Channels < 2 || admin.ModelsInSupply < 2 {
		t.Errorf("admin 概览 = %d 渠道 / %d 模型，期望至少 2 / 2", admin.Channels, admin.ModelsInSupply)
	}
	if admin.Today.Requests <= a.Today.Requests {
		t.Errorf("admin 的今日请求数 = %d，应当大于 A 自己的 %d", admin.Today.Requests, a.Today.Requests)
	}
	if _, ok := admin.reasonsByID()[channelB]; !ok {
		t.Errorf("admin 的异常清单 = %+v，期望含 B 的渠道 %d", admin.UnhealthyChannels, channelB)
	}
}

// TestMerchantOverviewEmptyMerchant 没有渠道的商家：四个数是 0，异常清单是
// [] 而不是 null，且不 500。
func TestMerchantOverviewEmptyMerchant(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-empty-")

	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/overview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body overviewTestResponse
	decode(t, rec, &body)
	if body.Channels != 0 || body.ModelsInSupply != 0 ||
		body.Today.Requests != 0 || body.Today.Cost != "0.00000000" {
		t.Errorf("概览 = %+v，期望四个 0", body)
	}
	if body.UnhealthyChannels == nil {
		t.Error("unhealthy_channels 是 null，期望 []")
	}
	if !strings.Contains(rec.Body.String(), `"unhealthy_channels":[]`) {
		t.Errorf("响应里 unhealthy_channels 不是空数组：%s", rec.Body)
	}
}

// TestMerchantOverviewTodayIsSiteDayAndSellingPrice 钉住两件事：
// 「今天」是站点当地的日历日（不是最近 24 小时）；cost 取售价列，不取上游成本。
func TestMerchantOverviewTodayIsSiteDayAndSellingPrice(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	h := merchantSurface(t, "Asia/Shanghai")
	db := testDB(t)
	owner := seedUser(t, db, "m-today-")
	model := uniqueName("model-today")
	seedCatalogModel(t, db, model)
	channel := merchantChannel(t, h, owner, uniqueName("ch-today"), "openai",
		"https://today.example.com/v1", []string{model})

	start, _ := todayWindow(time.Now(), loc)
	insertUsage(t, db, owner, channel, uniqueName("req-yesterday"), model,
		"10.0", "0.5", start.Add(-time.Minute))
	insertUsage(t, db, owner, channel, uniqueName("req-today"), model,
		"2.5", "99.9", start.Add(time.Minute))

	body := getOverview(t, h, owner)
	if body.Today.Requests != 1 {
		t.Errorf("今日请求数 = %d，期望 1（当地昨天 23:5x 那条不算）", body.Today.Requests)
	}
	if body.Today.Cost != "2.50000000" {
		t.Errorf("今日消耗 = %q，期望 2.50000000（售价口径，不是上游成本 99.9）", body.Today.Cost)
	}
}

// TestMerchantOverviewUnhealthyReasons 每个理由各造一条命中，外加一条
// 不该命中的健康渠道。
func TestMerchantOverviewUnhealthyReasons(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-reasons-")
	bucket := time.Now().UTC().Truncate(time.Hour)

	baseURL := "https://reasons.example.com/v1"
	healthy := merchantChannel(t, h, owner, uniqueName("healthy"), "openai", baseURL, channelModel(t, db))
	insertRollup(t, db, healthy, bucket, 100, 100, 0)
	insertQuotaState(t, db, healthy, "healthy", "ok")

	failure := merchantChannel(t, h, owner, uniqueName("failure"), "openai", baseURL, channelModel(t, db))
	insertRollup(t, db, failure, bucket, 100, 70, 30)

	quota := merchantChannel(t, h, owner, uniqueName("quota"), "openai", baseURL, channelModel(t, db))
	insertRollup(t, db, quota, bucket, 100, 100, 0)
	insertQuotaState(t, db, quota, "degraded", "ok")

	probe := merchantChannel(t, h, owner, uniqueName("probe"), "openai", baseURL, channelModel(t, db))
	insertRollup(t, db, probe, bucket, 100, 100, 0)
	insertQuotaState(t, db, probe, "healthy", "error")

	consecutive := merchantChannel(t, h, owner, uniqueName("consecutive"), "openai", baseURL, channelModel(t, db))
	insertRollup(t, db, consecutive, bucket, 100, 100, 0)
	if _, err := db.ExecContext(t.Context(),
		`UPDATE channels SET consecutive_failures = 2 WHERE id = $1`, consecutive); err != nil {
		t.Fatalf("置连续失败失败: %v", err)
	}

	idle := merchantChannel(t, h, owner, uniqueName("idle"), "openai", baseURL, channelModel(t, db))

	body := getOverview(t, h, owner)
	got := body.reasonsByID()

	if _, ok := got[healthy]; ok {
		t.Errorf("健康渠道 %d 被标成异常：%+v", healthy, got[healthy])
	}
	want := map[int64]string{
		failure:     reasonFailureRate,
		quota:       reasonQuotaState,
		probe:       reasonProbeFailed,
		consecutive: reasonConsecutiveFailures,
		idle:        reasonNoRequests,
	}
	for id, reason := range want {
		if !containsReason(got[id], reason) {
			t.Errorf("渠道 %d 的理由 = %v，期望含 %s", id, got[id], reason)
		}
	}
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// TestMerchantUpstreamsGroupsAndHidesCredentials 同一 (type, base_url) 归一组，
// 不同 base_url 分两组；响应逐键断言没有凭证字段，limit 有上限。
func TestMerchantUpstreamsGroupsAndHidesCredentials(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-upstream-")

	shared := "https://upstream-shared.example.com/v1"
	first := merchantChannel(t, h, owner, uniqueName("up-1"), "openai", shared, channelModel(t, db))
	second := merchantChannel(t, h, owner, uniqueName("up-2"), "openai", shared, channelModel(t, db))
	merchantChannel(t, h, owner, uniqueName("up-3"), "openai",
		"https://upstream-other.example.com/v1", channelModel(t, db))

	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/upstreams", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("上游清单状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		Items []struct {
			Type        string  `json:"type"`
			BaseURL     string  `json:"base_url"`
			Health      string  `json:"health"`
			Requests    int64   `json:"requests"`
			Failures    int64   `json:"failures"`
			FailureRate float64 `json:"failure_rate"`
			Channels    []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Health string `json:"health"`
			} `json:"channels"`
		} `json:"items"`
		Total     int  `json:"total"`
		Truncated bool `json:"truncated"`
	}
	decode(t, rec, &body)
	if body.Total != 2 || len(body.Items) != 2 {
		t.Fatalf("上游分组 = %d 条（total %d），期望 2", len(body.Items), body.Total)
	}
	sharedGroup := -1
	for i := range body.Items {
		if body.Items[i].BaseURL == shared {
			sharedGroup = i
		}
	}
	if sharedGroup < 0 {
		t.Fatalf("上游分组里没有共享地址 %q：%+v", shared, body.Items)
	}
	if len(body.Items[sharedGroup].Channels) != 2 {
		t.Errorf("共享地址下渠道数 = %d，期望 2", len(body.Items[sharedGroup].Channels))
	}
	ids := []int64{body.Items[sharedGroup].Channels[0].ID, body.Items[sharedGroup].Channels[1].ID}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if ids[0] != min64(first, second) || ids[1] != max64(first, second) {
		t.Errorf("组内渠道 = %v，期望 %d 与 %d", ids, first, second)
	}

	// 逐键断言：顶层与组内渠道都只含约定字段。
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	items, ok := generic["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v", generic["items"])
	}
	firstItem, ok := items[sharedGroup].(map[string]any)
	if !ok {
		t.Fatalf("item = %v", items[sharedGroup])
	}
	assertExactKeys(t, "上游", firstItem,
		[]string{"base_url", "channels", "failures", "failure_rate", "health", "requests", "type"})
	channelList, ok := firstItem["channels"].([]any)
	if !ok || len(channelList) != 2 {
		t.Fatalf("channels = %v", firstItem["channels"])
	}
	for _, one := range channelList {
		channel, ok := one.(map[string]any)
		if !ok {
			t.Fatalf("channel = %v", one)
		}
		assertExactKeys(t, "上游里的渠道", channel, []string{"health", "id", "name", "status"})
	}
	// 凭证明文与提示都不该出现在响应里。
	if strings.Contains(rec.Body.String(), "sk-placeholder") {
		t.Errorf("响应里出现了凭证明文：%s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "lder") {
		t.Errorf("响应里出现了凭证提示：%s", rec.Body)
	}

	// limit 真的生效：1 条 + total 2 + truncated。
	rec = merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/upstreams?limit=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("limit=1 状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var limited struct {
		Items     []json.RawMessage `json:"items"`
		Total     int               `json:"total"`
		Truncated bool              `json:"truncated"`
	}
	decode(t, rec, &limited)
	if len(limited.Items) != 1 || limited.Total != 2 || !limited.Truncated {
		t.Errorf("limit=1 得到 %d 条 / total %d / truncated %v",
			len(limited.Items), limited.Total, limited.Truncated)
	}
	if rec := merchantCall(t, h, owner, http.MethodGet,
		"/api/v1/merchant/upstreams?limit=0", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0 状态码 = %d，期望 400", rec.Code)
	}
}

// TestMerchantUpstreamsScopesToOwner 上游清单也只列自己的：B 的地址不出现在
// A 的清单里，admin（不注入归属）两条都看得到。
func TestMerchantUpstreamsScopesToOwner(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	ownerA := seedUser(t, db, "m-up-a-")
	ownerB := seedUser(t, db, "m-up-b-")

	baseA := "https://a-up.example.com/" + uniqueName("v1")
	baseB := "https://b-up.example.com/" + uniqueName("v1")
	merchantChannel(t, h, ownerA, uniqueName("a-up"), "openai", baseA, channelModel(t, db))
	merchantChannel(t, h, ownerB, uniqueName("b-up"), "openai", baseB, channelModel(t, db))

	baseURLs := func(owner int64) map[string]bool {
		t.Helper()
		rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/upstreams", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("上游清单状态码 = %d；正文：%s", rec.Code, rec.Body)
		}
		var body struct {
			Items []struct {
				BaseURL string `json:"base_url"`
			} `json:"items"`
		}
		decode(t, rec, &body)
		out := make(map[string]bool, len(body.Items))
		for _, item := range body.Items {
			out[item.BaseURL] = true
		}
		return out
	}

	a := baseURLs(ownerA)
	if !a[baseA] || a[baseB] {
		t.Errorf("A 的上游地址 = %v，期望含 %q、不含 B 的 %q", a, baseA, baseB)
	}
	b := baseURLs(ownerB)
	if !b[baseB] || b[baseA] {
		t.Errorf("B 的上游地址 = %v，期望含 %q、不含 A 的 %q", b, baseB, baseA)
	}
	admin := baseURLs(0)
	if !admin[baseA] || !admin[baseB] {
		t.Errorf("admin 的上游地址 = %v，期望两个商家都在", admin)
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
