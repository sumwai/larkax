package expr

import (
	"sort"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
)

// 这一组用例检查 #1153 的判据：键只从**结果位置**的表里收。
//
// 收集原先发生在「遍历到的每一张单价表」上，于是 let 绑定里没被结果引用的
// 表也把键带进 Program.Keys。price list 的 declared_keys 与 #1077 的写入
// 提示按它判「覆盖齐全」，结算却只认命中那张表的 Declared——与 #1149 消灭的
// 是同一幕，只是走了另一个入口。
//
// 收窄的只是**收集**，不是校验：那些表照旧走完形状检查（见
// TestUnrelatedTableStillValidated）。

// TestKeysIgnoreUnreferencedLetTable 是 #1153 的核心反例。
//
// 结果表只引用 m["prompt"]，m 里没被用到的 completion 不进 Keys()：它成不了
// 任何一次结算的 Declared，留在并集里只会让两处展示说「覆盖齐全」。
func TestKeysIgnoreUnreferencedLetTable(t *testing.T) {
	const source = `let m = {"prompt": 1.5, "completion": 2.0}; {"prompt": m["prompt"]}`
	p := mustCompile(t, source)

	if got := strings.Join(p.Keys(), ","); got != "prompt" {
		t.Fatalf("Keys() = %q，期望 %q：m 里没被结果引用的 completion 不该进并集", got, "prompt")
	}

	// 结算本来就是按命中那张表的声明判的，这一格确认两处现在同口径。
	res := evalProgram(t, p, billing.Usage{PromptTokens: 10, CompletionTokens: 5})
	if declared := strings.Join(sortedDeclaredKeys(res.Prices.Declared), ","); declared != "prompt" {
		t.Fatalf("Declared = %q，期望 %q", declared, "prompt")
	}
}

// TestKeysIgnoreDiscardedSequenceExpression 是同一条判据的另一个入口：
// 语句序列里被丢弃的中间式不是结果，里面的表不贡献键。
func TestKeysIgnoreDiscardedSequenceExpression(t *testing.T) {
	const source = `({"prompt": 9.9, "completion": 8.8}; {"prompt": 1.0})`
	p := mustCompile(t, source)

	if got := strings.Join(p.Keys(), ","); got != "prompt" {
		t.Fatalf("Keys() = %q，期望 %q：被丢弃的中间式不该进并集", got, "prompt")
	}
}

// TestUnrelatedTableStillValidated 检查收窄的边界：表成不了结果，校验照旧。
//
// 这条比上面两条更重要：「不贡献键」很容易顺手写成「不检查这个子树」，
// 那时上面两条照样绿，而未命中的绑定里写错键会一直躲到某次结算才炸。
func TestUnrelatedTableStillValidated(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "没被引用的绑定里有未知键",
			source: `let m = {"promt": 1.5}; {"prompt": 1.0}`,
			want:   `未知的键 "promt"`,
		},
		{
			name:   "被丢弃的中间式里有未知键",
			source: `({"promt": 1.5}; {"prompt": 1.0})`,
			want:   `未知的键 "promt"`,
		},
		{
			name:   "没被引用的绑定里 units 不是一张表",
			source: `let m = {"prompt": 1.5, "units": 5}; {"prompt": 1.0}`,
			want:   "units 必须是一张计量项表",
		},
		{
			name:   "没被引用的绑定里用量变量落在价格位置",
			source: `let m = {"prompt": p * 2.0}; {"prompt": 1.0}`,
			want:   `价格位置不能出现用量 "p"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compileErr(t, tc.source)
			if err.Reason != ReasonShape {
				t.Errorf("原因 = %s，期望 %s（%v）", err.Reason, ReasonShape, err.Err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误里应当出现 %q，得到 %q", tc.want, err.Error())
			}
		})
	}
}

// TestKeysAreResultPositionUnionOfDeclared 是 #1153 的性质用例。
//
// 判据按**结果位置**定义，不按可达分支：Keys() 等于所有处于结果位置的表
// 的键集之并——没命中的分支也算（让那里写错的键在编译期暴露），但 let
// 绑定里没被结果引用的表不算。
//
// 用例这样构造，才不会写成恒真：
//
//   - 每条规则的每个叶子分支都用 tier() 起了名字，且构造的用量**必须**把
//     每个名字都走到一次。走不到就报错——否则「各次 Declared 之并」只看到
//     走到的那几支，这条用例就证明不了没命中的分支也进并集。
//   - 每条规则只用 token 计价项，所以 Keys() 与 Declared 的键集可以逐字相等。
//     units / fixed / minimum 不在 Declared 里（那是另一套命名空间），
//     它们的并集比单次结算宽是有意的，见 TestEvalProgramMetadata。
//   - 旧行为下必有红：「let 绑定里只有被引用的一项进并集」与「语句序列里
//     被丢弃的中间式」这两条，会把结果用不到的键带进 Keys()，于是并集与
//     Keys() 不等。
func TestKeysAreResultPositionUnionOfDeclared(t *testing.T) {
	cases := []struct {
		name   string
		source string
		// usages 是构造出来的用量：hitTiers 里的每一档都要被其中某一次命中。
		usages []billing.Usage
		// hitTiers 是这条规则各叶子分支记下的档位名；没有三元时为空。
		hitTiers []string
	}{
		{
			name: "根位置三元按 len 两档",
			source: `len <= 100
				? tier("short", {"prompt": 1.0, "completion": 2.0})
				: tier("long", {"prompt": 3.0, "completion": 4.0})`,
			usages:   []billing.Usage{{PromptTokens: 50}, {PromptTokens: 101}},
			hitTiers: []string{"short", "long"},
		},
		{
			// 缓存读写也算进 len（见 contextLength）：第三档用缓存走到，
			// 顺带确认「档位判的是送进去多少，不是其中多少要花钱」。
			name: "嵌套三元三档，缓存也计入 len",
			source: `len <= 100
				? tier("s", {"prompt": 1.0, "completion": 2.0})
				: (len <= 1000
					? tier("m", {"prompt": 3.0, "completion": 4.0})
					: tier("l", {"prompt": 5.0, "completion": 6.0}))`,
			usages: []billing.Usage{
				{PromptTokens: 50},
				{CacheReadTokens: 300},
				{PromptTokens: 5000},
			},
			hitTiers: []string{"s", "m", "l"},
		},
		{
			name: "条件用 p 与 c 的区间",
			source: `p <= 1000
				? (c <= 500
					? tier("p_small_c_small", {"prompt": 1.0, "completion": 2.0})
					: tier("p_small_c_big", {"prompt": 3.0, "completion": 4.0}))
				: tier("p_big", {"prompt": 5.0, "completion": 6.0})`,
			usages: []billing.Usage{
				{PromptTokens: 100, CompletionTokens: 100},
				{PromptTokens: 100, CompletionTokens: 600},
				{PromptTokens: 2000},
			},
			hitTiers: []string{"p_small_c_small", "p_small_c_big", "p_big"},
		},
		{
			// 绑定在真分支被整张引用：里面的键照旧进并集。
			name: "let 绑定的表在分支里被引用",
			source: `let m = {"prompt": 1.0, "completion": 2.0, "cache_read": 0.5};
				len <= 100
					? tier("bound", m)
					: tier("inline", {"prompt": 3.0, "completion": 4.0, "cache_read": 1.5})`,
			usages:   []billing.Usage{{PromptTokens: 50}, {PromptTokens: 150}},
			hitTiers: []string{"bound", "inline"},
		},
		{
			// 绑定里只被引用了一项，另一项没有任何一次结算会声明它。
			name:   "let 绑定里只有被引用的一项进并集",
			source: `let m = {"prompt": 1.5, "completion": 2.0}; {"prompt": m["prompt"], "cache_read": 0.5}`,
			usages: []billing.Usage{{}, {PromptTokens: 10, CompletionTokens: 5}},
		},
		{
			name:   "绑定的表就是结果",
			source: `let m = {"prompt": 1.0, "completion": 2.0}; m`,
			usages: []billing.Usage{{}, {PromptTokens: 10, CompletionTokens: 5}},
		},
		{
			// 绑定转手一次：按绑定值继续走的那条路要能连着剥下去，
			// 只剥一层会让这里的 Keys() 少掉两项。
			name:   "绑定转手一次后成为结果",
			source: `let m = {"prompt": 1.0, "completion": 2.0}; let n = m; n`,
			usages: []billing.Usage{{}, {PromptTokens: 10, CompletionTokens: 5}},
		},
		{
			name:   "语句序列里被丢弃的中间式",
			source: `({"prompt": 9.9, "completion": 8.8}; {"prompt": 1.0})`,
			usages: []billing.Usage{{}},
		},
	}

	// 无条件成立的那一半：任取用量，结算声明的每个键都得在 Keys() 里。
	// 反过来（结算声明了、Keys() 里没有）意味着展示说「没定价」而实际收了钱，
	// 与 #1153 的方向相反，同样是两处口径分叉。
	broadUsages := []billing.Usage{
		{},
		{PromptTokens: 1, CompletionTokens: 1},
		{PromptTokens: 10000, CompletionTokens: 10000},
		{CacheReadTokens: 300, CacheWriteTokens: 300},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustCompile(t, tc.source)
			keys := p.Keys()
			keySet := make(map[string]struct{}, len(keys))
			for _, key := range keys {
				keySet[key] = struct{}{}
			}

			union := map[string]struct{}{}
			hitTiers := map[string]bool{}
			for _, usage := range append(append([]billing.Usage{}, tc.usages...), broadUsages...) {
				res := evalProgram(t, p, usage)
				for name := range res.Prices.Declared {
					if _, ok := keySet[name]; !ok {
						t.Errorf("%+v: 结算声明了 %q，Keys() 里却没有：%v", usage, name, keys)
					}
					union[name] = struct{}{}
				}
				if res.Tier != "" {
					hitTiers[res.Tier] = true
				}
			}

			// 每个叶子分支都要被走到一次，否则下面那个「并集相等」只剩
			// 「走到的那几支相等」，证明不了没命中的分支也进并集。
			for _, tier := range tc.hitTiers {
				if !hitTiers[tier] {
					t.Errorf("档位 %q 没有被任何一次用量命中，构造的用量走不到这一支", tier)
				}
			}

			got := make([]string, 0, len(union))
			for name := range union {
				got = append(got, name)
			}
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(keys, ",") {
				t.Errorf("各次 Declared 之并 = %v，Keys() = %v，两者必须相等", got, keys)
			}
		})
	}
}

// evalProgram 用给定的用量求值一条已经编译好的规则。
func evalProgram(t *testing.T, p *Program, usage billing.Usage) Result {
	t.Helper()
	res, err := p.Eval(Env{Usage: usage, At: testAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatalf("%+v: 求值失败: %v", usage, err)
	}
	return res
}

// sortedDeclaredKeys 把 Declared 的键排好序，便于逐字比较。
func sortedDeclaredKeys(declared map[string]bool) []string {
	out := make([]string, 0, len(declared))
	for name := range declared {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
