package adminapi

import (
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是「我的模型」里「配过价没有」那一半的纯单测。清单本身要真库，见
// merchant_models_integration_test.go。

// 两个固定的商家 id：归属过滤要能「跨 owner 不串」，同一批行必须来自不同
// owner 才测得出。
var (
	testOwnerA = int64(101)
	testOwnerB = int64(202)
)

func patternsAt(owner int64, enabled bool, from time.Time, to *time.Time,
	patterns ...string,
) pricing.RulePatterns {
	return pricing.RulePatterns{
		OwnerUserID: owner, Enabled: enabled,
		EffectiveFrom: from, EffectiveTo: to, ModelPatterns: patterns,
	}
}

// TestApplicableRulePatterns 钉住「生效中」的四条边界与归属过滤：
// 停用、未到生效时刻、已过期（含恰好到终点）、别的商家的规则都不算；
// 兜底规则（model_patterns 为空）不挂在具体模型上，单独回一个布尔。
func TestApplicableRulePatterns(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	exactlyNow := now

	rules := []pricing.RulePatterns{
		// 命中 gpt-4o：具体模型 + 生效中。
		patternsAt(testOwnerA, true, past, nil, "gpt-4o"),
		// 一条规则可以挂多个模型，两个都算。
		patternsAt(testOwnerA, true, past, nil, "gpt-4o", "claude-3"),
		// 兜底：model_patterns 为空，作用于全部模型。
		patternsAt(testOwnerA, true, past, nil),
		// 恰好从此刻开始：算（effective_from <= now）。
		patternsAt(testOwnerA, true, exactlyNow, nil, "gpt-4o"),
		// 停用：不算。
		patternsAt(testOwnerA, false, past, nil, "gpt-4o"),
		// 还没到生效时刻：不算。
		patternsAt(testOwnerA, true, future, nil, "gpt-4o"),
		// 已过期：终点是「开区间」，恰好等于 now 也不算。
		patternsAt(testOwnerA, true, past, &past, "gpt-4o"),
		patternsAt(testOwnerA, true, past, &exactlyNow, "gpt-4o"),
		// 终点还在未来：算。
		patternsAt(testOwnerA, true, past, &future, "gpt-4o"),
		// 别人家的规则：商家视角不算。
		patternsAt(testOwnerB, true, past, nil, "gpt-4o"),
		patternsAt(testOwnerB, true, past, nil),
	}

	byModel, anyModel := applicableRulePatterns(rules, &testOwnerA, now)
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
		byModel, anyModel := applicableRulePatterns([]pricing.RulePatterns{rules[i]}, &testOwnerA, now)
		hit := anyModel || len(byModel) > 0
		if hit != want {
			t.Errorf("第 %d 条命中 = %v，期望 %v", i, hit, want)
		}
	}
	// 停用 / 未来生效 / 已过期 / 恰好到终点 / 别人的规则：一条都不算。
	for _, i := range []int{4, 5, 6, 7, 9, 10} {
		byModel, anyModel := applicableRulePatterns([]pricing.RulePatterns{rules[i]}, &testOwnerA, now)
		if anyModel || len(byModel) != 0 {
			t.Errorf("第 %d 条不该命中：byModel=%v anyModel=%v", i, byModel, anyModel)
		}
	}
}

// TestApplicableRulePatternsAdminSeesAllOwners 钉住 admin 视角（owner 为 nil）
// 不按归属收窄：管理员那一列问的是「全平台有没有一条商家规则命中它」。
//
// 与上一条是同一段判据的两个主语：去掉归属过滤时，商家视角那条会红；反过来
// 把它无条件加上时，这一条会红。
func TestApplicableRulePatternsAdminSeesAllOwners(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)

	rules := []pricing.RulePatterns{
		patternsAt(testOwnerA, true, past, nil, "model-a"),
		patternsAt(testOwnerB, true, past, nil, "model-b"),
		patternsAt(testOwnerB, true, past, nil),
	}

	byModel, anyModel := applicableRulePatterns(rules, nil, now)
	if !anyModel {
		t.Error("admin 视角下 testOwnerB 的兜底规则没有算上")
	}
	if !byModel["model-a"] || !byModel["model-b"] {
		t.Errorf("admin 视角的命中集合 = %v，期望两个商家的模型都在", byModel)
	}

	// 同一批行换成商家视角：只剩自己的那一条。
	byModel, anyModel = applicableRulePatterns(rules, &testOwnerA, now)
	if anyModel {
		t.Error("testOwnerB 的兜底规则算到了 testOwnerA 头上")
	}
	if !byModel["model-a"] || byModel["model-b"] {
		t.Errorf("testOwnerA 的命中集合 = %v，期望只有 model-a", byModel)
	}
}
