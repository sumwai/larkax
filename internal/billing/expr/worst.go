package expr

import (
	"fmt"
	"math"

	"github.com/expr-lang/expr/ast"
)

// worstPair 是「取更贵的那一个」，用于最坏情况求值。
//
// 它作为**环境里的值**传给 expr，不是注册进函数表。两者有本质区别：
// 函数表里的函数会随字节码一起被打包进 vm.Program 的常量表，
// 一份编译产物就固定了一份实现；而环境值在每次求值时从环境映射里取，
// 于是缓存里那份编译产物保持无状态、可被并发复用。
//
// 签名是固定的两个参数：expr 对函数表里的函数允许变参签名，
// 对环境里的值则按真实签名做类型检查，写成 func(any, any) 才能
// 让 `__worst(a, b)` 编得过，也才能让编译器确认它返回 any。
//
// 处理两种形状：
//
//	数值    取较大者（价格位置上的单价、一次性费用）
//	map     逐键取较大者（一组单价）
//
// 「逐键取较大者」而不是「返回两个 map 里较大的那个」是关键：规则完全可以
// 让一支的输入单价高、另一支的缓存单价高。整张 map 取其一作为上界，
// 另一支就会在某些用量下超出去——那正是「上界算小了」的典型形态，
// 而 Capped 本该是「上界算错了」的信号，不该成为常态。
//
// 键取并集：只在一边出现的键也带过来。若一支给了 units 而另一支没有，
// 上界里就该有 units——否则结算时按另一支算出了这笔费用，预留里却没有。
func worstPair(a, b any) (any, error) {
	if a == nil || b == nil {
		// nil 只可能来自显式写了 nil 的规则。nil 无法参与比较，
		// 而按「另一支」处理会让上界取决于哪一边写成了 nil，报错更安全。
		return nil, fmt.Errorf("__worst 的参数不能是 nil")
	}

	am, aIsMap := a.(map[string]any)
	bm, bIsMap := b.(map[string]any)
	switch {
	case aIsMap && bIsMap:
		return worstTable(am, bm)
	case aIsMap != bIsMap:
		// 两支的形状不一致时无法逐槽比较。编译期的形状检查看的是每个分支
		// 单独是否合法，而「一支是表、一支是数」在语法上完全合法
		// （例如 `len <= 1 ? {"prompt": 1.0} : 2.0`）。这里报错而不是猜：
		// 猜错的方向如果是「取小的那个」，上界就不成立。
		return nil, fmt.Errorf("__worst 的两个分支形状不一致: %T 与 %T", a, b)
	}

	af, aok := asFloat(a)
	bf, bok := asFloat(b)
	if !aok || !bok {
		return nil, fmt.Errorf("__worst 的参数必须是数值或单价表，当前是 %T 与 %T", a, b)
	}
	// 有一侧是 NaN 时取另一侧：NaN 与任何数比较都是假，math.Max(NaN, x)
	// 得到 NaN，而上界变成 NaN 意味着预留金额算不出来。NaN 本身会在
	// result 里被 asPrice 拦下——那是求值结束之后的事，不该在中间态就把
	// 上界毁掉。
	if math.IsNaN(af) {
		return bf, nil
	}
	if math.IsNaN(bf) {
		return af, nil
	}
	return math.Max(af, bf), nil
}

// worstTable 逐键取两个单价表的较大者，键取并集。
func worstTable(a, b map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(a)+len(b))
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			// 只在一边出现的键直接带过来。缺了它，预留里就没有这一项，
			// 而命中的那一支可能是有的。
			out[k] = av
			continue
		}
		v, err := worstPair(av, bv)
		if err != nil {
			return nil, fmt.Errorf("键 %q: %w", k, err)
		}
		out[k] = v
	}
	for k, bv := range b {
		if _, ok := a[k]; !ok {
			out[k] = bv
		}
	}
	return out, nil
}

// worstPatcher 把每个三元表达式换成 __worst(真分支, 假分支)。
//
// 为什么需要它：expr 默认短路求值，`a ? b : c` 只算一支，于是上界会
// 退化成「命中的那一支」。而未命中的那一支恰恰可能更贵——那正是
// 上界要覆盖的情形。
//
// 换成函数调用之后，两个参数都会被求值（字节码里先算完两侧再 OpCall），
// 于是「每个分支都算一次」是求值顺序决定的，不依赖 expr 的短路开关。
// 这一点比关掉短路更可靠：关掉短路还会让 `and`/`or` 的两侧都求值，
// 而那可能让一条本来能编过的规则报出类型错（`units["x"] != 0 and
// units["x"] > 3` 这类写法在短路下不会走到右面）。
//
// 嵌套的三元会被逐个替换：ast.Patch 之后外层节点仍然会被继续遍历，
// 因此链式条件（`a ? b : c ? d : e`）也全部覆盖到。
type worstPatcher struct{}

// Visit 遍历 AST，把每个三元表达式换成 __worst(真分支, 假分支)。
//
// 方法名是 ast.Visitor 接口要求的，无法改成不导出的。
func (worstPatcher) Visit(node *ast.Node) {
	cond, ok := (*node).(*ast.ConditionalNode)
	if !ok {
		return
	}
	// ast.Patch 会保留位置信息，于是 __worst 相关的运行期错误仍能指回源码。
	ast.Patch(node, &ast.CallNode{
		Callee:    &ast.IdentifierNode{Value: worstIdent},
		Arguments: []ast.Node{cond.Exp1, cond.Exp2},
	})
}

// worstIdent 是 __worst 的名字。
//
// 带双下划线：expr 的标识符允许下划线，而规则作者不会去写这个前缀。
// 名字同时写进 reservedNames，禁止注入常量占用它。
const worstIdent = "__worst"
