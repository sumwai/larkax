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
// 区别只在这里带渠道数与「配过价没有」。归属照旧由 roleGroup 注入。

// merchantModelJSON 是清单里的一行。
type merchantModelJSON struct {
	Model       string `json:"model"`
	DisplayName string `json:"display_name"`
	// Channels 是名下在供这个模型的渠道数；同一地址上的多个账号各算一条。
	Channels int64 `json:"channels"`
	// Priced 表示这个商家有一条生效中的计价规则命中这个模型。
	//
	// 它回答的是「配过价没有」，**不是**「这个模型实际按哪条计价」：多条
	// 命中时引擎按裁决次序只取一条，渠道作用域的规则还可能让商家的规则
	// 完全不生效。
	Priced bool `json:"priced"`
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
	byModel, anyModel, err := a.applicableRules(ctx, owner, time.Now())
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
			// 兜底规则（model_patterns 为空）作用于全部模型，所以有一它就都算配过。
			Priced: byModel[m.Model] || anyModel,
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

// applicableRules 返回「哪些模型名被一条规则命中」与「是否存在兜底规则」。
//
// 口径与计价读路径一致：只算 merchant 作用域、启用中、且在有效期内的规则；
// model_patterns 为空表示适用于全部模型（命中每一行），非空时按元素**精确**
// 匹配（与 `r.model_patterns ? 模型名` 同义，规则写入时已拒掉通配符）。
//
// 商家会话只取自己的规则（ListOwned）；admin 不注入归属，取全部商家的
// merchant 规则——同一套判据、不同的主语：管理员那一列问的是「全平台有没有
// 一条商家规则命中它」，**不要求规则的主人正在供这个模型**。平台规则与渠道
// 规则不算：这一列问的是「商家自己配过价没有」。
//
// # 为什么是布尔而不是条数
//
// 计价引擎是「多条命中取一条」（作用域 channel > merchant > platform →
// priority 大的 → 模型名单非空的优先于兜底 → id 大的），所以「几条」既答
// 不了「配过没有」，也答不了「实际按哪条」；写成条数会被读成「生效的价」。
func (a *API) applicableRules(ctx context.Context, owner *int64,
	now time.Time,
) (byModel map[string]bool, anyModel bool, err error) {
	var rules []pricing.RuleInfo
	if owner != nil {
		rules, err = a.rules.ListOwned(ctx, *owner)
	} else {
		rules, err = a.rules.List(ctx)
	}
	if err != nil {
		return nil, false, err
	}
	byModel, anyModel = applicableRuleModels(rules, now)
	return byModel, anyModel, nil
}

// applicableRuleModels 是上面那段口径的纯函数部分，供单测直接钉住。
func applicableRuleModels(rules []pricing.RuleInfo, now time.Time) (map[string]bool, bool) {
	byModel := make(map[string]bool, len(rules))
	var anyModel bool
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
			anyModel = true
			continue
		}
		for _, model := range rule.ModelPatterns {
			byModel[model] = true
		}
	}
	return byModel, anyModel
}
