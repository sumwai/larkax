package adminapi

import (
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是「我的模型」里规则计数那一半的纯单测。清单本身要真库，见
// merchant_models_integration_test.go。

// TestCountApplicableRules 钉住「生效中」的四条边界与作用域过滤：
// 停用、未到生效时刻、已过期（含恰好到终点）、别人的作用域都不算；
// 兜底规则（model_patterns 为空）不挂在具体模型上，单独计数。
func TestCountApplicableRules(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	expired := past
	stillActive := future
	exactlyNow := now

	rule := func(scope string, enabled bool, from time.Time, to *time.Time,
		patterns ...string,
	) pricing.RuleInfo {
		return pricing.RuleInfo{
			Scope: scope, Enabled: enabled,
			EffectiveFrom: from, EffectiveTo: to, ModelPatterns: patterns,
		}
	}

	rules := []pricing.RuleInfo{
		// 命中 gpt-4o：具体模型 + 生效中。
		rule(pricing.ScopeMerchant, true, past, nil, "gpt-4o"),
		// 一条规则可以挂多个模型，两个都算。
		rule(pricing.ScopeMerchant, true, past, nil, "gpt-4o", "claude-3"),
		// 兜底：model_patterns 为空，作用于全部模型。
		rule(pricing.ScopeMerchant, true, past, nil),
		// 恰好从此刻开始：算（effective_from <= now）。
		rule(pricing.ScopeMerchant, true, exactlyNow, nil, "gpt-4o"),
		// 停用：不算。
		rule(pricing.ScopeMerchant, false, past, nil, "gpt-4o"),
		// 还没到生效时刻：不算。
		rule(pricing.ScopeMerchant, true, future, nil, "gpt-4o"),
		// 已过期：终点是「开区间」，恰好等于 now 也不算。
		rule(pricing.ScopeMerchant, true, past, &expired, "gpt-4o"),
		rule(pricing.ScopeMerchant, true, past, &exactlyNow, "gpt-4o"),
		// 终点还在未来：算。
		rule(pricing.ScopeMerchant, true, past, &stillActive, "gpt-4o"),
		// 平台与渠道作用域：这一列只问「我自己定的价」，两种都不算。
		rule(pricing.ScopePlatform, true, past, nil, "gpt-4o"),
		rule(pricing.ScopeChannel, true, past, nil, "gpt-4o"),
	}

	byModel, anyModel := countApplicableRules(rules, now)
	if got, want := byModel["gpt-4o"], int64(4); got != want {
		t.Errorf("gpt-4o 命中 %d 条，期望 %d（具体 + 多模型 + 恰好生效 + 未过终点）", got, want)
	}
	if got, want := byModel["claude-3"], int64(1); got != want {
		t.Errorf("claude-3 命中 %d 条，期望 %d", got, want)
	}
	if anyModel != 1 {
		t.Errorf("兜底规则 = %d 条，期望 1", anyModel)
	}
	// 不在任何规则里的模型是 0，不是「没有键就跳过」——调用方直接相加。
	if got := byModel["no-such-model"]; got != 0 {
		t.Errorf("未出现的模型 = %d，期望 0", got)
	}
}
