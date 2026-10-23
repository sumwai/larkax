package adminapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是 /api/v1/merchant/my-models 的真库用例。
//
// 单测只能钉住规则计数的折行（见 merchant_models_test.go）；「归属真的进了
// SQL」「渠道数按渠道条数算」「停用 / 软删 / 不含该名的渠道不算」这三件事
// 只有真库答得了。

type merchantModelsResponse struct {
	Items []struct {
		Model       string `json:"model"`
		DisplayName string `json:"display_name"`
		Channels    int64  `json:"channels"`
		Priced      bool   `json:"priced"`
	} `json:"items"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

func getModels(t *testing.T, h http.Handler, owner int64, query string) merchantModelsResponse {
	t.Helper()
	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/my-models"+query, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("模型清单状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body merchantModelsResponse
	decode(t, rec, &body)
	return body
}

// seedOwnedRule 建一条商家规则。enabled 用指针给，与 CreateRequest 一致。
func seedOwnedRule(t *testing.T, db *sql.DB, owner int64,
	patterns []string, enabled bool,
) {
	t.Helper()
	_, err := pricing.NewAdmin(db).Create(context.Background(), pricing.CreateRequest{
		Scope:         pricing.ScopeMerchant,
		OwnerUserID:   owner,
		Name:          uniqueName("rule"),
		ModelPatterns: patterns,
		Expr:          `{"prompt": 1}`,
		Enabled:       &enabled,
	})
	if err != nil {
		t.Fatalf("建计价规则失败: %v", err)
	}
}

// TestMerchantModelsScopesToOwner 跨租户不串：A 的清单只有 A 的模型与渠道数，
// admin（不注入归属）看到的是去重到规范名的全局清单。
func TestMerchantModelsScopesToOwner(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	ownerA := seedUser(t, db, "m-models-a-")
	ownerB := seedUser(t, db, "m-models-b-")

	modelA := uniqueName("model-a")
	modelB := uniqueName("model-b")
	seedCatalogModel(t, db, modelA)
	seedCatalogModel(t, db, modelB)

	merchantChannel(t, h, ownerA, uniqueName("ch-a1"), "openai",
		"https://a1.example.com/v1", []string{modelA})
	// 同一模型的两条渠道各算一条（不同账号）。
	merchantChannel(t, h, ownerA, uniqueName("ch-a2"), "openai",
		"https://a2.example.com/v1", []string{modelA})
	merchantChannel(t, h, ownerB, uniqueName("ch-b"), "openai",
		"https://b.example.com/v1", []string{modelB})

	a := getModels(t, h, ownerA, "")
	if a.Total != 1 || len(a.Items) != 1 {
		t.Fatalf("A 的模型清单 = %+v，期望只有 %q 一条", a, modelA)
	}
	if a.Items[0].Model != modelA || a.Items[0].Channels != 2 {
		t.Errorf("A 的条目 = %+v，期望 %q 两条渠道", a.Items[0], modelA)
	}
	if a.Items[0].DisplayName != modelA {
		t.Errorf("展示名 = %q，期望规范名 %q", a.Items[0].DisplayName, modelA)
	}

	admin := getModels(t, h, 0, "")
	seen := make(map[string]int64, len(admin.Items))
	for _, item := range admin.Items {
		seen[item.Model] = item.Channels
	}
	if seen[modelA] != 2 || seen[modelB] != 1 {
		t.Errorf("admin 的清单 = %v，期望 %s:2 / %s:1", seen, modelA, modelB)
	}
}

// TestMerchantModelsEmptyMerchant 没有在供模型的商家：items 是 []，total 0。
func TestMerchantModelsEmptyMerchant(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-models-empty-")

	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/my-models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body merchantModelsResponse
	decode(t, rec, &body)
	if body.Total != 0 || len(body.Items) != 0 {
		t.Errorf("清单 = %+v，期望空", body)
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("items 不是空数组：%s", rec.Body)
	}
}

// TestMerchantModelsFiltersBySupplyPredicate 停用、软删、不含该名的渠道都不计入。
func TestMerchantModelsFiltersBySupplyPredicate(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-models-filter-")

	live := uniqueName("model-live")
	disabled := uniqueName("model-disabled")
	deleted := uniqueName("model-deleted")
	wrongModel := uniqueName("model-wrong")
	for _, name := range []string{live, disabled, deleted, wrongModel} {
		seedCatalogModel(t, db, name)
	}

	merchantChannel(t, h, owner, uniqueName("live"), "openai",
		"https://live.example.com/v1", []string{live})
	disabledID := merchantChannel(t, h, owner, uniqueName("disabled"), "openai",
		"https://disabled.example.com/v1", []string{disabled})
	deletedID := merchantChannel(t, h, owner, uniqueName("deleted"), "openai",
		"https://deleted.example.com/v1", []string{deleted})
	// 挂着别的模型：它不该让 wrongModel 出现在清单里。
	unrelated := uniqueName("model-unrelated")
	seedCatalogModel(t, db, unrelated)
	merchantChannel(t, h, owner, uniqueName("other-model"), "openai",
		"https://other.example.com/v1", []string{unrelated})

	for _, exec := range []struct {
		stmt string
		id   int64
	}{
		{`UPDATE channels SET status = 'disabled' WHERE id = $1`, disabledID},
		{`UPDATE channels SET deleted_at = now() WHERE id = $1`, deletedID},
	} {
		if _, err := db.ExecContext(t.Context(), exec.stmt, exec.id); err != nil {
			t.Fatalf("改渠道状态失败: %v", err)
		}
	}

	body := getModels(t, h, owner, "")
	seen := make(map[string]bool, len(body.Items))
	for _, item := range body.Items {
		seen[item.Model] = true
	}
	for _, want := range []string{live, unrelated} {
		if !seen[want] {
			t.Errorf("清单里没有在供的 %q：%+v", want, body)
		}
	}
	for _, absent := range []string{disabled, deleted, wrongModel} {
		if seen[absent] {
			t.Errorf("清单里出现了不该在供的 %q（停用 / 软删 / 不含该名都不算）：%+v",
				absent, body)
		}
	}
}

// TestMerchantModelsOrdersAndLimits 排序固定、limit 截断并给 total/truncated，
// 越界回 400。
func TestMerchantModelsOrdersAndLimits(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-models-limit-")

	for range 3 {
		name := uniqueName("model-order")
		seedCatalogModel(t, db, name)
		merchantChannel(t, h, owner, uniqueName("ch-order"), "openai",
			"https://order.example.com/v1", []string{name})
	}

	all := getModels(t, h, owner, "")
	if all.Total != 3 || len(all.Items) != 3 || all.Truncated {
		t.Fatalf("清单 = %+v，期望 3 条、未截断", all)
	}
	for i := 1; i < len(all.Items); i++ {
		if all.Items[i-1].Model > all.Items[i].Model {
			t.Errorf("清单没有按规范名升序：%q 在 %q 之前",
				all.Items[i-1].Model, all.Items[i].Model)
		}
	}

	// 恰好等于条数不算截断：`total >= limit` 与 `total > limit` 在这里分得开。
	exact := getModels(t, h, owner, "?limit=3")
	if len(exact.Items) != 3 || exact.Total != 3 || exact.Truncated {
		t.Errorf("limit=3 得到 %+v，期望 3 条、未截断", exact)
	}

	limited := getModels(t, h, owner, "?limit=2")
	if len(limited.Items) != 2 || limited.Total != 3 || !limited.Truncated {
		t.Errorf("limit=2 得到 %+v，期望 2 条 / total 3 / truncated", limited)
	}
	for _, bad := range []string{"?limit=0", "?limit=201", "?limit=abc"} {
		rec := merchantCall(t, h, owner, http.MethodGet,
			"/api/v1/merchant/my-models"+bad, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400", bad, rec.Code)
		}
	}
}

// TestMerchantModelsPriced 钉住「配过价没有」的四个方向：自己的规则算、
// 别人的不算、停用的不算；兜底规则（model_patterns 为空）让每个模型都算。
// 这一列是布尔：没有价是 false，不是 null。
func TestMerchantModelsPriced(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	ownerA := seedUser(t, db, "m-models-priced-a-")
	ownerB := seedUser(t, db, "m-models-priced-b-")

	withRule := uniqueName("model-priced")
	withoutRule := uniqueName("model-unpriced")
	for _, name := range []string{withRule, withoutRule} {
		seedCatalogModel(t, db, name)
		merchantChannel(t, h, ownerA, uniqueName("ch-priced"), "openai",
			"https://priced.example.com/v1", []string{name})
	}

	pricedOf := func(owner int64) map[string]bool {
		t.Helper()
		body := getModels(t, h, owner, "")
		out := make(map[string]bool, len(body.Items))
		for _, item := range body.Items {
			out[item.Model] = item.Priced
		}
		return out
	}

	// 还没建规则：两个模型都没有价。
	before := pricedOf(ownerA)
	if before[withRule] || before[withoutRule] {
		t.Fatalf("建规则前就有价：%v", before)
	}

	// A 的具体规则只管 withRule；别人给 withoutRule 建的价不算在 A 头上。
	seedOwnedRule(t, db, ownerA, []string{withRule}, true)
	seedOwnedRule(t, db, ownerB, []string{withoutRule}, true)
	got := pricedOf(ownerA)
	if !got[withRule] || got[withoutRule] {
		t.Errorf("A 的 priced = %v，期望 %q 有、%q 无（别人的规则不算）",
			got, withRule, withoutRule)
	}

	// admin 视角是同一套判据、不同的主语：全平台有没有一条商家规则命中它，
	// 不要求规则的主人正在供这个模型——ownerB 没有任何渠道，它给 withoutRule
	// 建的那条规则仍让管理员看到 true。
	admin := pricedOf(0)
	if !admin[withRule] || !admin[withoutRule] {
		t.Errorf("admin 的 priced = %v，期望两个都有（两个商家各配了一条）", admin)
	}

	// 停用的规则不报价。
	seedOwnedRule(t, db, ownerA, []string{withoutRule}, false)
	if got = pricedOf(ownerA); got[withoutRule] {
		t.Errorf("停用的规则被算成有价：%v", got)
	}

	// 兜底规则作用于全部模型：两个都算。
	seedOwnedRule(t, db, ownerA, nil, true)
	got = pricedOf(ownerA)
	if !got[withRule] || !got[withoutRule] {
		t.Errorf("兜底规则没有覆盖全部模型：%v", got)
	}

	// 这一列是布尔而不是 null：页面直接读 true/false。
	rec := merchantCall(t, h, ownerA, http.MethodGet, "/api/v1/merchant/my-models", nil)
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	for _, item := range raw.Items {
		if _, ok := item["priced"].(bool); !ok {
			t.Errorf("%v 的 priced = %v，期望布尔", item["model"], item["priced"])
		}
		if _, found := item["own_rules"]; found {
			t.Errorf("%v 还带着 own_rules 字段", item["model"])
		}
	}
}

// TestMerchantModelsRuleWindowIsRespected 未来生效与已停用的规则都不算「配过价」。
func TestMerchantModelsRuleWindowIsRespected(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-models-window-")
	model := uniqueName("model-window")
	seedCatalogModel(t, db, model)
	merchantChannel(t, h, owner, uniqueName("ch-window"), "openai",
		"https://window.example.com/v1", []string{model})

	// 还没开始生效的规则。
	from := time.Now().Add(time.Hour)
	if _, err := pricing.NewAdmin(db).Create(t.Context(), pricing.CreateRequest{
		Scope: pricing.ScopeMerchant, OwnerUserID: owner,
		Name: uniqueName("rule-future"), ModelPatterns: []string{model},
		Expr: `{"prompt": 1}`, EffectiveFrom: from,
	}); err != nil {
		t.Fatalf("建未来生效规则失败: %v", err)
	}

	body := getModels(t, h, owner, "")
	if len(body.Items) != 1 {
		t.Fatalf("清单 = %+v", body)
	}
	if body.Items[0].Priced {
		t.Error("priced = true，期望 false（未到生效时刻）")
	}
}
