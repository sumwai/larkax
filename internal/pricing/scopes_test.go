package pricing

import (
	"slices"
	"testing"
)

// TestScopesPinsTheList 钉住作用域清单本身。
//
// 期望值写成字面量，不从 Scopes() 拼：拼出来的期望等于自己比自己，删掉
// 一项时两边同时少，没有用例会红。
func TestScopesPinsTheList(t *testing.T) {
	want := []string{"platform", "merchant", "channel"}
	got := Scopes()

	if !slices.Equal(got, want) {
		t.Errorf("Scopes() = %q，期望 %q", got, want)
	}
	if len(got) != len(want) {
		t.Errorf("Scopes() 有 %d 项，期望 %d 项", len(got), len(want))
	}
}

// TestValidateRuleRejectsUnknownScope 钉住「作用域不认识」那一句的逐字输出。
//
// 整句相等而不是只判「提到了 platform」：报错里那一串取值由 Scopes() 渲染，
// 渲染漏掉一项时，只有逐字对照才会红。
func TestValidateRuleRejectsUnknownScope(t *testing.T) {
	err := validateRule(RuleInfo{
		Scope: "bogus",
		Name:  "scope-check",
		Expr:  `{"prompt": 0.00001}`,
	})
	if err == nil {
		t.Fatal("validateRule 接受了不认识的作用域，期望报错")
	}

	const want = `作用域 "bogus" 不是有效取值（只接受 "platform"、"merchant" 与 "channel"）`
	if got := err.Error(); got != want {
		t.Errorf("err = %s，期望 %s", got, want)
	}
}

// TestValidateRuleAcceptsEachKnownScope 让三个取值各自过一次取值检查。
//
// 这一条只管「取值合法」，与「作用域和 owner/channel 两栏 id 的组合」是
// 两件事：后者由 admin_test.go 的 TestCreateRejectsInvalidFields 管。
func TestValidateRuleAcceptsEachKnownScope(t *testing.T) {
	cases := []struct {
		name string
		rule RuleInfo
	}{
		{"platform", RuleInfo{Scope: ScopePlatform}},
		{"merchant", RuleInfo{Scope: ScopeMerchant, OwnerUserID: 1}},
		{"channel", RuleInfo{Scope: ScopeChannel, ChannelID: 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.rule.Name = "scope-check"
			tc.rule.Expr = `{"prompt": 0.00001}`
			if err := validateRule(tc.rule); err != nil {
				t.Errorf("validateRule(%s) = %v，期望通过", tc.rule.Scope, err)
			}
		})
	}
}

// TestScopeChoicesRendersTheList 钉住报错里那一串的拼法。
//
// 顿号、「与」与引号都算在内：换掉任何一个，报错文本就与改动前不再逐字
// 相同，而这类变化在 diff 里看不出来（admin.go 里只是一处格式串）。
func TestScopeChoicesRendersTheList(t *testing.T) {
	const want = `"platform"、"merchant" 与 "channel"`
	if got := scopeChoices(); got != want {
		t.Errorf("scopeChoices() = %s，期望 %s", got, want)
	}
}
