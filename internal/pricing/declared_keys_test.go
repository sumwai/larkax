package pricing

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
)

// TestDeclaredKeysMatchSettlement 检查 #1153 的展示面：price list 的
// declared_keys 与结算同口径。
//
// 展示读 decorate → expr.Program.Keys()，结算读命中那张表的
// billing.Prices.Declared。拿一条 let 绑定里躺着未引用键的规则，两处必须是
// 同一个键集——#1153 之前展示把 completion 也算上，结算却按结果表的声明
// 说没定价。
//
// 这条用例不碰库：decorate 只编译表达式。admin / cost_admin 两条列举走的是
// 同一个 decorate（decorateCost），所以这一条同时覆盖它们。
func TestDeclaredKeysMatchSettlement(t *testing.T) {
	const source = `let m = {"prompt": 0.0000015, "completion": 0.0000075}; {"prompt": m["prompt"]}`

	info := decorate(RuleInfo{Expr: source})
	if info.ExprErr != nil {
		t.Fatalf("规则应当能编译：%v", info.ExprErr)
	}
	if got := strings.Join(info.DeclaredKeys, ","); got != "prompt" {
		t.Fatalf("declared_keys = %q，期望 %q", got, "prompt")
	}

	prog, err := expr.Compile(source)
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
	// 同一规则、同一用量：展示的键集就是结算声明的那一组。
	usage := billing.Usage{PromptTokens: 10, CompletionTokens: 5}
	res, err := prog.Eval(expr.Env{
		Usage: usage,
		At:    time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("求值失败：%v", err)
	}
	declared := make([]string, 0, len(res.Prices.Declared))
	for name := range res.Prices.Declared {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	if strings.Join(declared, ",") != strings.Join(info.DeclaredKeys, ",") {
		t.Fatalf("declared_keys = %v，Declared = %v，两者必须相等", info.DeclaredKeys, declared)
	}
}
