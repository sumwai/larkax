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
// 展示读 decorate / decorateCost → expr.Program.Keys()，结算读命中那张表的
// billing.Prices.Declared。拿一条 let 绑定里躺着未引用键的规则，两处必须是
// 同一个键集——#1153 之前展示把 completion 也算上，结算却按结果表的声明
// 说没定价。
//
// 这条用例不碰库：两个 decorate 都只编译表达式。价格表的列举走 `decorate`、
// 成本表的列举走 `decorateCost`，是两个各自独立的函数，所以分成两个子用例
// 分别断言——只断一处，另一处的口径就没人看着。
func TestDeclaredKeysMatchSettlement(t *testing.T) {
	const source = `let m = {"prompt": 0.0000015, "completion": 0.0000075}; {"prompt": m["prompt"]}`

	// 结算侧：同一规则、同一用量，命中那张表声明了哪些键。
	prog, err := expr.Compile(source)
	if err != nil {
		t.Fatalf("编译失败：%v", err)
	}
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
	want := strings.Join(declared, ",")

	t.Run("价格表", func(t *testing.T) {
		info := decorate(RuleInfo{Expr: source})
		if info.ExprErr != nil {
			t.Fatalf("规则应当能编译：%v", info.ExprErr)
		}
		got := strings.Join(info.DeclaredKeys, ",")
		if got != "prompt" {
			t.Fatalf("declared_keys = %q，期望 %q", got, "prompt")
		}
		if got != want {
			t.Fatalf("declared_keys = %q，Declared = %q，两者必须相等", got, want)
		}
	})

	t.Run("成本表", func(t *testing.T) {
		cost := decorateCost(CostInfo{Expr: source})
		if cost.ExprErr != nil {
			t.Fatalf("成本规则应当能编译：%v", cost.ExprErr)
		}
		got := strings.Join(cost.DeclaredKeys, ",")
		if got != "prompt" {
			t.Fatalf("成本表的 declared_keys = %q，期望 %q", got, "prompt")
		}
		if got != want {
			t.Fatalf("成本表的 declared_keys = %q，Declared = %q，两者必须相等", got, want)
		}
	})
}
