package adminapi

import (
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是「我的模型」里「配过价没有」那一半的纯单测。清单本身要真库，见
// merchant_models_integration_test.go。

func ruleAt(scope string, enabled bool, from time.Time, to *time.Time,
	patterns ...string,
) pricing.RuleInfo {
	return pricing.RuleInfo{
		Scope: scope, Enabled: enabled,
		EffectiveFrom: from, EffectiveTo: to, ModelPatterns: patterns,
	}
}

// TestApplicableRuleModels 钉住「生效中」的四条边界与作用域过滤：
// 停用、未到生效时刻、已过期（含恰好到终点）、别人的作用域都不算；
// 兜底规则（model_patterns 为空）不挂在具体模型上，单独回一个布尔。
func TestApplicableRuleModels(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	exactlyNow := now

	rules := []pricing.RuleInfo{
		// 命中 gpt-4o：具体模型 + 生效中。
		ruleAt(pricing.ScopeMerchant, true, past, nil, "gpt-4o"),
		// 一条规则可以挂多个模型，两个都算。
		ruleAt(pricing.ScopeMerchant, true, past, nil, "gpt-4o", "claude-3"),
		// 兜底：model_patterns 为空，作用于全部模型。
		ruleAt(pricing.ScopeMerchant, true, past, nil),
		// 恰好从此刻开始：算（effective_from <= now）。
		ruleAt(pricing.ScopeMerchant, true, exactlyNow, nil, "gpt-4o"),
		// 停用：不算。
		ruleAt(pricing.ScopeMerchant, false, past, nil, "gpt-4o"),
		// 还没到生效时刻：不算。
		ruleAt(pricing.ScopeMerchant, true, future, nil, "gpt-4o"),
		// 已过期：终点是「开区间」，恰好等于 now 也不算。
		ruleAt(pricing.ScopeMerchant, true, past, &past, "gpt-4o"),
		ruleAt(pricing.ScopeMerchant, true, past, &exactlyNow, "gpt-4o"),
		// 终点还在未来：算。
		ruleAt(pricing.ScopeMerchant, true, past, &future, "gpt-4o"),
		// 平台与渠道作用域：这一列只问「商家自己配过价没有」，两种都不算。
		ruleAt(pricing.ScopePlatform, true, past, nil, "gpt-4o"),
		ruleAt(pricing.ScopeChannel, true, past, nil, "gpt-4o"),
	}

	byModel, anyModel := applicableRuleModels(rules, now)
	if !anyModel {
		t.Error("兜底规则没有算上")
	}
	if !byModel["gpt-4o"] || !byModel["claude-3"] {
		t.Errorf("命中集合 = %v，期望含 gpt-4o 与 claude-3", byModel)
	}
	// 不在任何规则里的模型不能凭空出现——调用方会直接读这个键。
	if byModel["no-such-model"] {
		t.Errorf("未出现的模型被判为已配价：%v", byModel)
	}

	// 逐条单独判，红的时候能指到是哪一条漏了守卫。
	for i, want := range map[int]bool{0: true, 1: true, 2: true, 3: true, 8: true} {
		byModel, anyModel := applicableRuleModels([]pricing.RuleInfo{rules[i]}, now)
		hit := anyModel || len(byModel) > 0
		if hit != want {
			t.Errorf("第 %d 条命中 = %v，期望 %v", i, hit, want)
		}
	}
	// 停用 / 未来生效 / 已过期 / 恰好到终点 / 两种别的作用域：一条都不算。
	for _, i := range []int{4, 5, 6, 7, 9, 10} {
		byModel, anyModel := applicableRuleModels([]pricing.RuleInfo{rules[i]}, now)
		if anyModel || len(byModel) != 0 {
			t.Errorf("第 %d 条不该命中：byModel=%v anyModel=%v", i, byModel, anyModel)
		}
	}
}
