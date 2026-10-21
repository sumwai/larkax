package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是商家运营页的「我的模型」清单（GroupOverview 的第三个读）。
//
// 它与概览的 models_in_supply 认同一份在供判据（channels.suppliesModel），
// 区别只在这里带渠道数与商家的计价规则数。归属照旧由 roleGroup 注入。

// merchantModelJSON 是清单里的一行。
type merchantModelJSON struct {
	Model       string `json:"model"`
	DisplayName string `json:"display_name"`
	// Channels 是名下在供这个模型的渠道数；同一地址上的多个账号各算一条。
	Channels int64 `json:"channels"`
	// OwnRules 是该商家生效中、以这个模型为目标或兜底匹配到它的计价规则数。
	OwnRules int64 `json:"own_rules"`
}

type merchantModelsJSON struct {
	Items     []merchantModelJSON `json:"items"`
	Total     int                 `json:"total"`
	Truncated bool                `json:"truncated"`
}

func (a *API) merchantModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, err := parseListLimit(r.URL.Query().Get("limit"))
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	owner := ownerScope(ctx)

	models, err := a.channels.SuppliedModelsOwned(ctx, owner)
	if err != nil {
		a.failMerchantRead(w, r, "在供模型", err)
		return
	}
	byModel, anyModel, err := a.applicableRuleCounts(ctx, owner, time.Now())
	if err != nil {
		a.failMerchantRead(w, r, "计价规则", err)
		return
	}

	items := make([]merchantModelJSON, 0, len(models))
	for _, m := range models {
		items = append(items, merchantModelJSON{
			Model:       m.Model,
			DisplayName: m.DisplayName,
			Channels:    m.Channels,
			// 兜底规则（model_patterns 为空）作用于全部模型，所以每一行都加一次。
			OwnRules: byModel[m.Model] + anyModel,
		})
	}

	total := len(items)
	truncated := total > limit
	if truncated {
		items = items[:limit]
	}
	a.writeJSON(w, http.StatusOK, merchantModelsJSON{
		Items: items, Total: total, Truncated: truncated,
	})
}

// applicableRuleCounts 返回「每个模型名 → 命中的规则数」与兜底规则的条数。
//
// 口径与计价读路径一致：只算 merchant 作用域、启用中、且在有效期内的规则；
// model_patterns 为空表示适用于全部模型（命中每一行），非空时按元素**精确**
// 匹配（与 `r.model_patterns ? 模型名` 同义，规则写入时已拒掉通配符）。
//
// 商家会话只取自己的规则（ListOwned）；admin 不注入归属，取全部商家的
// merchant 规则——运营页那一列对管理员是全局合计。平台规则与渠道规则不算：
// 这一列问的是「我自己定的价有几条」。
func (a *API) applicableRuleCounts(ctx context.Context, owner *int64,
	now time.Time,
) (byModel map[string]int64, anyModel int64, err error) {
	var rules []pricing.RuleInfo
	if owner != nil {
		rules, err = a.rules.ListOwned(ctx, *owner)
	} else {
		rules, err = a.rules.List(ctx)
	}
	if err != nil {
		return nil, 0, err
	}
	byModel, anyModel = countApplicableRules(rules, now)
	return byModel, anyModel, nil
}

// countApplicableRules 是上面那段口径的纯函数部分，供单测直接钉住。
func countApplicableRules(rules []pricing.RuleInfo, now time.Time) (map[string]int64, int64) {
	byModel := make(map[string]int64, len(rules))
	var anyModel int64
	for _, rule := range rules {
		if rule.Scope != pricing.ScopeMerchant {
			continue
		}
		if !rule.Enabled {
			continue
		}
		if rule.EffectiveFrom.After(now) {
			continue
		}
		if rule.EffectiveTo != nil && !rule.EffectiveTo.After(now) {
			continue
		}
		if len(rule.ModelPatterns) == 0 {
			anyModel++
			continue
		}
		for _, model := range rule.ModelPatterns {
			byModel[model]++
		}
	}
	return byModel, anyModel
}
