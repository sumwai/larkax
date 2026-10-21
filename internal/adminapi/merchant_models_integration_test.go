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

// 本文件是 /api/v1/merchant/models 的真库用例。
//
// 单测只能钉住规则计数的折行（见 merchant_models_test.go）；「归属真的进了
// SQL」「渠道数按渠道条数算」「停用 / 软删 / 不含该名的渠道不算」这三件事
// 只有真库答得了。

type merchantModelsResponse struct {
	Items []struct {
		Model       string `json:"model"`
		DisplayName string `json:"display_name"`
		Channels    int64  `json:"channels"`
		OwnRules    int64  `json:"own_rules"`
	} `json:"items"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

func getModels(t *testing.T, h http.Handler, owner int64, query string) merchantModelsResponse {
	t.Helper()
	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/models"+query, nil)
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

	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/models", nil)
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
			"/api/v1/merchant/models"+bad, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 状态码 = %d，期望 400", bad, rec.Code)
		}
	}
}

// TestMerchantModelsCountsOwnRules 钉住 own_rules 的三个方向：自己的规则算、
// 别人的不算、停用的不算；兜底规则（model_patterns 为空）作用于每个模型。
func TestMerchantModelsCountsOwnRules(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	ownerA := seedUser(t, db, "m-models-rules-a-")
	ownerB := seedUser(t, db, "m-models-rules-b-")

	withRule := uniqueName("model-rule")
	withoutRule := uniqueName("model-norule")
	for _, name := range []string{withRule, withoutRule} {
		seedCatalogModel(t, db, name)
		merchantChannel(t, h, ownerA, uniqueName("ch-rule"), "openai",
			"https://rule.example.com/v1", []string{name})
	}

	// admin 那一列是全部商家规则的合计：同包别的用例可能也建了兜底规则，
	// 所以先取一次基线，断言的是这次新增了多少。
	beforeAdmin := getModels(t, h, 0, "")
	baseline := make(map[string]int64, len(beforeAdmin.Items))
	for _, item := range beforeAdmin.Items {
		baseline[item.Model] = item.OwnRules
	}

	seedOwnedRule(t, db, ownerA, []string{withRule}, true)
	seedOwnedRule(t, db, ownerA, []string{withRule}, false)   // 停用
	seedOwnedRule(t, db, ownerA, nil, true)                   // 兜底
	seedOwnedRule(t, db, ownerB, []string{withRule}, true)    // 别人的
	seedOwnedRule(t, db, ownerB, []string{withoutRule}, true) // 别人的

	body := getModels(t, h, ownerA, "")
	rules := make(map[string]int64, len(body.Items))
	for _, item := range body.Items {
		rules[item.Model] = item.OwnRules
	}
	// withRule：自己一条具体规则 + 一条兜底 = 2；别人的那条不算。
	if got := rules[withRule]; got != 2 {
		t.Errorf("%q 的 own_rules = %d，期望 2（自己的具体规则 + 兜底）", withRule, got)
	}
	// withoutRule：只有兜底规则命中。
	if got := rules[withoutRule]; got != 1 {
		t.Errorf("%q 的 own_rules = %d，期望 1（只有兜底）", withoutRule, got)
	}

	// admin 那一列是全部商家规则的合计：withRule 再算上 B 的那一条 = 3。
	admin := getModels(t, h, 0, "")
	for _, item := range admin.Items {
		if item.Model == withRule {
			if delta := item.OwnRules - baseline[withRule]; delta != 3 {
				t.Errorf("admin 视角 %q 的 own_rules 增加 %d，期望 3（两个商家的规则合计）",
					withRule, delta)
			}
		}
	}

	// 规则不是「越多越好」的另一半：没有价的模型是 0，不是 null。
	rec := merchantCall(t, h, ownerA, http.MethodGet, "/api/v1/merchant/models", nil)
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	for _, item := range raw.Items {
		if item["own_rules"] == nil {
			t.Errorf("%v 的 own_rules 是 null，期望数字", item["model"])
		}
	}
}

// TestMerchantModelsRuleWindowIsRespected 未来生效与已停用的规则都不进这一列。
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
	if body.Items[0].OwnRules != 0 {
		t.Errorf("own_rules = %d，期望 0（未到生效时刻）", body.Items[0].OwnRules)
	}
}
