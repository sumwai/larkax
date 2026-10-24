package expr

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"
)

// 规则的规模上限。
//
// 规则是**商家可写的输入**，这三个数字是它的第一道闸门：
//
//   - maxSourceBytes 在解析之前拦住：几兆字节的表达式会让词法与语法分析
//     先把 CPU 吃掉，那时再报「节点太多」已经晚了。
//   - maxNodes 交给 expr 的 MaxNodes，挡住「节点数爆炸」——一个递归展开的
//     表达式可以让 AST 大到把内存吃光，而源码本身只有几百字节。
//   - maxNesting 是本包自己挡的：expr 的解析器是递归下降的，而括号不产生
//     节点，MaxNodes 拦不住「两千层括号」。深度大了会让递归把 goroutine 的
//     栈打穿——那是**进程级**故障，不是一个商家、一条规则该有的影响面。
//
// 数字取得偏紧，是因为正常的计价规则就一行：几百字节、几十个节点、两三层嵌套。
// 紧一点只会挡住写得离谱的规则，漏一点就是线上事故。
const (
	maxSourceBytes = 4096
	maxNodes       = 512
	maxNesting     = 24
)

// Reason 是编译失败的大类。
//
// 分大类而不是只给一句错误文本：调用方要能区别处置——语法错要让商家改规则，
// 规模超限要提示他简化，形状不对要告诉他结果必须是一组单价。
type Reason string

const (
	// ReasonSyntax 是语法或类型错误：规则作者自己能修。
	ReasonSyntax Reason = "语法或类型错误"
	// ReasonShape 是结果形状不对：求值出来不是一组单价。
	ReasonShape Reason = "结果形状不对"
	// ReasonScale 是规则规模超限。
	ReasonScale Reason = "规则规模超限"
	// ReasonGlobals 是注入常量的问题。
	ReasonGlobals Reason = "注入常量非法"
)

// CompileError 是编译一条规则时的失败。
type CompileError struct {
	// Reason 是失败的大类。
	Reason Reason
	// Source 是原始源码，便于在管理界面上定位到出错的那一条规则。
	Source string
	// Err 是底层原因，通常来自 expr：它带行号与指针，可以直接给写规则的人看。
	Err error
}

func (e *CompileError) Error() string {
	return fmt.Sprintf("计价规则编译失败（%s）: %v", e.Reason, e.Err)
}

func (e *CompileError) Unwrap() error { return e.Err }

// Program 是一条编译好的规则。
//
// 编译一次、求值多次。**不可变且并发安全**：编译之后没有任何字段被写入，
// 求值期的可变状态全在每次 Eval 新建的 runtime 里（见 runtime.env）。
// 包内有一条 -race 测试让多个 goroutine 同时求值同一份 Program。
type Program struct {
	// prog 是普通求值的字节码。
	prog *vm.Program
	// worst 是「所有分支取最高价」版本的字节码（见 EvalWorstCase）。
	worst *vm.Program
	// keys 是**结果位置**上出现过的键的并集（所有三元分支一起看）。
	//
	// 并集而不是按分支分别校验，是有意的：预留与结算可能走不同的分支，
	// 若只看实际命中的那一支，「未命中的分支写错了键」会一直躲过校验，
	// 直到某天订单真的走进那一支才炸。按并集校验把这个错误提前到编译期。
	//
	// 收的是「结果位置能到达的表」，不是「遍历到的每张表」。区别在两处：
	//
	//	let m = {"prompt": 1.5, "completion": 2.0}; {"prompt": m["prompt"]}
	//	({"prompt": 9.9}; {"completion": 2.0})
	//
	// 前者 m 里没用到的 completion、后者被丢弃的中间式里的 prompt，都不进
	// 并集——它们成不了任何一次结算的 Declared，进了并集只会让 price list 的
	// declared_keys 与写入提示说「覆盖齐全」，而结算按命中那张表的声明报缺价。
	// 没命中的**分支**仍然进并集：那是并集要防的另一件事（见 walkShape 的
	// MapNode 分支与 checkBranchTokenKeys）。形状校验不受影响——那些表照旧
	// 走完全部检查，收窄的只是收集。
	//
	// 并集里 token 计价项在根位置三元的**两支之间**是一致的（见
	// checkBranchTokenKeys），所以这些键不会因为命中了哪一支而少。
	// units / fixed / minimum 可以只在某些分支出现，它们的并集比单次结算宽
	// ——那是有意的，见 worstTable。
	//
	// EvalWorstCase 不读这份键集：最坏情况走另一份字节码，由 worstTable 自己
	// 按键取并集（见 worst.go）。所以这份并集收窄只动展示与诊断，不动上界。
	keys map[string]struct{}
}

// Keys 返回结果位置上出现过的键，已排序。用于展示与诊断。
//
// 它可能比某一次结算的 Declared 宽：units / fixed / minimum 只在部分分支写，
// 没命中的分支也算在内。但它不会宽在「结果引用不到的表」上——那些表不会成为
// 任何一次结算的 Declared，见 Program.keys。
func (p *Program) Keys() []string {
	out := make([]string, 0, len(p.keys))
	for k := range p.keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Compile 编译一条计价规则。
//
// 编译期要拦下的东西分四类，每一类不拦都会在线上算错钱：
//
//	语法错、未知变量、未知函数、类型不匹配   由 expr 的类型检查给出
//	表达式规模超限                          见 maxSourceBytes/maxNodes/maxNesting
//	结果形状不对                            必须能求值出一组单价（见 inspectShape）
//
// 内置函数按白名单放行，见 builtinOptions：读墙钟的、浮点边界不明确的、
// 能改变值类型的，一律挡掉。
//
// 关于 `len`：它是 expr 的内置函数名，而本项目要用 `len` 作变量名
// （上下文长度）。两者会冲突，而 expr 的行为是**变量遮蔽内置函数**：
// `len <= 32000` 取变量，`len([1,2])` 报 `int64 is not callable`。
// 我们依赖这个行为，并且额外把 len 内置显式禁掉——拼错的名字宁可报
// unknown name，也不要静默地调到一个函数上。
func Compile(source string) (*Program, error) {
	return compile(source, nil)
}

// CompileWithGlobals 与 Compile 相同，但声明了可注入的常量及其类型。
//
// **目前没有生产入口：只有本包的测试在调它。** 生产走 pipeline →
// Cache.Get → Compile，不声明任何注入常量；要用这条路，先回答
// Env.Globals 注释里列的三个问题。
//
// globals 的**值**不重要，只有键与类型进编译期环境：求值时注入的常量
// 必须与这里声明的名字和类型一致，否则规则在编译期就该被拒绝——
// 一个 `library == "arxiv"` 的规则配上一个整数 library 会安静地永远为假，
// 而那种错误在账单上表现为「这个套餐的折扣从来没生效过」。
func CompileWithGlobals(source string, globals map[string]any) (*Program, error) {
	for name, v := range globals {
		if _, err := globalType(name, v); err != nil {
			return nil, &CompileError{Reason: ReasonGlobals, Source: source, Err: err}
		}
	}
	return compile(source, globals)
}

// compile 是 Compile 与 CompileWithGlobals 的共同实现。
func compile(source string, globals map[string]any) (*Program, error) {
	if source == "" {
		return nil, &CompileError{Reason: ReasonShape, Err: errors.New("规则为空")}
	}
	if len(source) > maxSourceBytes {
		return nil, &CompileError{Reason: ReasonScale, Source: source,
			Err: fmt.Errorf("源码 %d 字节，超过上限 %d", len(source), maxSourceBytes)}
	}
	if err := checkNesting(source); err != nil {
		return nil, &CompileError{Reason: ReasonScale, Source: source, Err: err}
	}

	env := compileEnv(globals)

	prog, err := expr.Compile(source, compileOptions(env)...)
	if err != nil {
		return nil, &CompileError{Reason: ReasonSyntax, Source: source, Err: err}
	}

	sh, err := inspectShape(prog.Node())
	if err != nil {
		return nil, &CompileError{Reason: ReasonShape, Source: source, Err: err}
	}

	worst, err := expr.Compile(source, worstOptions(env)...)
	if err != nil {
		// 最坏情况版本编不过，说明两条编译路径的环境不一致——那是引擎的
		// 缺陷而不是规则的缺陷，所以不按语法错报。
		return nil, &CompileError{Reason: ReasonShape, Source: source,
			Err: fmt.Errorf("最坏情况版本编译失败: %w", err)}
	}

	return &Program{
		prog:  prog,
		worst: worst,
		keys:  sh.keys,
	}, nil
}

// compileEnv 组装编译期环境。
//
// 必须与运行期环境（runtime.env）给出**同一组名字**：少一个会让
// 「编译通过、求值时报 unknown name」，多一个更坏——规则作者以为某个
// 变量能用，而它永远不会出现。两个函数各写一份是有意的，
// 因为它们的值类型不同（编译期给类型样本，运行期给真实值），
// 但名字集合由同一组常量决定。
func compileEnv(globals map[string]any) map[string]any {
	env := usageVars()
	for _, name := range clockVarNames {
		// 编译期只需要名字存在；类型由 expr 从这里的样本值推断，
		// 而 at/region 在规则里的用法是 at.Hour() 与 region == "CN"，
		// 所以给它们与运行期相同的类型。
		switch name {
		case "at":
			env[name] = time.Time{}
		case "region":
			env[name] = ""
		}
	}
	for name, v := range funcSchemas {
		env[name] = v
	}
	// __worst 以**环境值**的形式提供，而不是注册进函数表：函数表里的实现
	// 会随字节码打包进编译产物，环境值不会（见 worstPair 的注释）。
	// 普通编译里它永远不会被引用——名字带双下划线，规则作者不会去写。
	env[worstIdent] = worstPair
	for name, v := range globals {
		t, err := globalType(name, v)
		if err != nil {
			// 调用方在 CompileWithGlobals 里已经校验过；真到这里说明两个
			// 入口不一致，跳过——expr 的类型检查会把它当未知名字报出来。
			continue
		}
		env[name] = t
	}
	return env
}

// compileOptions 生成普通编译的选项。
func compileOptions(env map[string]any) []expr.Option {
	opts := []expr.Option{
		expr.Env(env),
		// 结果必须是一组单价，因此是个 map。AsKind 让 expr 在编译期就检查
		// 返回类型，而不是等求值回来才发现是个数字。
		expr.AsKind(reflect.Map),
		expr.MaxNodes(maxNodes),
	}
	return append(opts, builtinOptions()...)
}

// worstOptions 生成「最坏情况」编译的选项。
//
// 与普通编译的唯一差别是那个 patcher：它把每个三元换成 __worst(真, 假)，
// 于是两个分支都会被求值（见 worstPatcher 的注释）。环境里的名字集合
// 必须与普通编译**完全一致**——不一致会让最坏情况这条路径报出普通路径
// 不认的名字，而那类错只会在内部测试里露出来。
func worstOptions(env map[string]any) []expr.Option {
	opts := []expr.Option{
		expr.Env(env),
		expr.AsKind(reflect.Map),
		expr.MaxNodes(maxNodes),
		expr.Patch(worstPatcher{}),
	}
	return append(opts, builtinOptions()...)
}

// builtinOptions 生成内置函数的禁用与白名单。
//
// 先全禁，再按白名单放行。全禁是默认安全的做法：expr 升级带来新的内置时，
// 它默认是被挡住的，而不是自动可用。
//
// 明确挡掉的几类：
//
//   - 读墙钟：now/date/duration/timezone。预留与结算必须给出同一个答案，
//     而读墙钟会让跨整点的请求按低谷预留、按高峰结算，撞上 Capped——
//     而 Capped 本该是「上界算错了」的信号，不该成为常态。事后复算同理：
//     没有求值时刻就无法重放。
//   - 改变值类型：toJSON/fromJSON/toBase64/fromBase64。它们能把字符串
//     变成对象、把对象变成字符串，绕过「结果必须是数值」这条校验。
//   - math 包（pow/exp/log…）：浮点边界不明确，极端输入下给出 Inf 或
//     平台相关的值，而它要变成金额。单价本身可以是小数，
//     但**用来算单价的中间量**不该有平台相关的边界行为。
//   - len：见 Compile 的注释，它是本项目的变量名。
//
// 不用 expr.DisableBuiltin 逐个列举「要挡的」就完事，是因为 expr 的
// 内置清单会随版本增加；白名单让新增的那些默认不可用。
func builtinOptions() []expr.Option {
	opts := []expr.Option{expr.DisableAllBuiltins()}
	for _, name := range mathWhitelist {
		opts = append(opts, expr.EnableBuiltin(name))
	}
	return opts
}

// mathWhitelist 是放行的内置函数。
//
// 只放数值变换：单调、无平台相关边界、不读环境、不改变量纲。
// max/min/abs/ceil/floor/round 用于把单价修成规则想要的形式；
// int/float 用于显式转换。
//
// 不放 sum/mean/median：它们接收数组，而规则的输入里没有数组，
// 放行只会让作者写出「看起来能用但不该能用」的规则。
var mathWhitelist = []string{
	"max", "min", "abs", "ceil", "floor", "round", "int", "float",
}

// blockedBuiltins 是**必须单独挡掉**的内置函数。
//
// 与 mathWhitelist 相对：那些是普通内置，Disabled 表能拦住；这一批走的是
// 解析器的 predicates 表（sum/map/reduce 之类），关掉内置不影响它们，
// 所以要在结构检查里显式拒绝。
//
// 它们共同的性质是**第一个参数是一个集合**（`[]any`），而规则的输入里
// 没有集合——所以任何一次调用要么写不出来，要么就是一次静默的量纲错：
// `{"prompt": map([1,2], #)}` 会返回一个数组，而 asPrice 到那时才报
// 「不是数值」，报错的位置离规则作者写的那个词很远。
//
// 返回值则各不相同：数组（map/filter/sortBy）、对象（groupBy）、布尔
// （all/none/any/one）、整数（sum/count/findIndex/findLastIndex）、
// 以及 any（find/reduce）。所以**按返回类型拦不住**，只能把名字列出来。
var blockedBuiltins = map[string]struct{}{
	"all": {}, "none": {}, "any": {}, "one": {},
	"filter": {}, "map": {}, "find": {}, "findIndex": {},
	"findLast": {}, "findLastIndex": {}, "count": {}, "sum": {},
	"groupBy": {}, "sortBy": {}, "reduce": {},
}

// shape 是规则结果的形状检查结果。
type shape struct {
	// keys 是结果位置上出现过的键的并集（所有三元分支一起看）。
	// 只有能成为结果的那张表才贡献键，见 Program.keys。
	keys map[string]struct{}
	// binds 是 let 绑定：名字 → 绑定值所在的 AST 节点。
	//
	// 根位置出现一个绑定的名字（`let price = {…}; price`）时，要按它绑定的
	// 值继续检查，否则结果是个标识符，会被误判成「不是一张单价表」。
	// 表达式已经过类型检查，所以这里不做作用域管理：越界的名字在那一步
	// 就是 unknown name，走不到形状检查。
	binds map[string]ast.Node
	// visiting 记着正在按绑定解析的名字，挡住自引用（`let a = a; a`）
	// 把递归转成环。类型检查通常先拒掉这类写法，但不指望它。
	visiting map[string]bool
}

// inspectShape 检查求值结果是不是「一组单价」，并收集键。
//
// 检查的是 AST 而不是求值结果：只有 AST 才能把**每个分支**都看到。
// 若只看实际求值返回的那个 map，一条分时规则里未被命中的那支写错了键
// 会一直躲过检查，直到某天订单真的走进那一支。
//
// 允许的形状：
//
//	{"prompt": 1.5, "completion": 7.5, "units": {"search": 0.01}}   单价表
//	len <= 32000 ? {...} : {...}                                     按条件选一组单价
//	tier("peak", {...})                                              带档位追踪的单价表
//	{"prompt": 1.5 + (isHoliday() ? 0.5 : 0.0)}                      价格位置上的数值运算
//	let price = {...}; price                                         let 绑定整张表
//
// 根位置的三元还要求两支的 token 计价项键集一致：见 walkShape 的
// ConditionalNode 分支。`units` / `fixed` / `minimum` 不在这条判据内。
//
// 检查顺带把用量变量限制在条件位置上（见 pos.cond 与 usageInArithmetic）。
func inspectShape(root ast.Node) (shape, error) {
	sh := shape{
		keys:     map[string]struct{}{},
		binds:    map[string]ast.Node{},
		visiting: map[string]bool{},
	}
	// 从根位置开始：整条表达式的结果必须是一张单价表（或按条件选出的一张）。
	if err := walkShape(root, &sh, pos{root: true}); err != nil {
		return shape{}, err
	}
	return sh, nil
}

// walkShape 递归检查 AST，按节点在结果里的**位置**判断它该是什么。
//
// 位置是这里唯一需要追踪的状态：判断一个节点的合法形状要用到它，
// 判断里面的用量变量能不能出现也要用到它——而这两件事的判据都只能由
// 「这个子树在结果里处于什么位置」决定，节点自身的类型答不出来。
type pos struct {
	// root 为 true 表示这个节点**本身**必须是单价表或条件，
	// 不能是一个裸的数值——`len <= 1 ? {...} : 2.0` 这种写法要在这里被拦住。
	//
	// 它同时是「这个子树能不能成为结果」的标记，键只从置了它的表里收
	// （见 walkShape 的 MapNode 分支）。两件事共用一个字段是有意的：它们
	// 问的是同一个问题，分成两个字段就得在每个入口同时传对，漏一个不会报错，
	// 只会让「校验看到了、收集没收到」，那正是 #1153 要消灭的分叉。
	root bool
	// slot 非空表示它在单价表的一个具名槽里（键名，或计量项名）。
	// 这种位置允许数值运算，但不允许再出现一张表。
	slot string
	// cond 为 true 表示这个子树只当条件用，它的值不会成为单价。
	//
	// 只有这里可以出现用量变量。原因是上界：EvalWorstCase 靠「每个价格位置
	// 取所有分支的最高价」工作，而那个上界只有当每个分支里的单价都是**与用量
	// 无关的常数**时才成立（见 Program.EvalWorstCase）。`{"prompt": p * 1.5}`
	// 让单价随用量变化，于是「余额不为负」所依赖的上界不再成立，而它不会
	// 报错，只会让账悄悄错掉。
	//
	// 为什么不干脆禁止用量变量出现：用量作条件是**正常且必要**的写法，
	// `p > 0 ? A : B` 与 `len <= 32000 ? A : B` 是同一回事，一刀切会连它一起
	// 拒掉。所以判据是**上下文**而不是「标识符出现过」——
	//
	//	{"prompt": p * 1.5}                  拒绝：p 在算术位置
	//	{"prompt": p > 0 ? 1.5 : 0}          允许：p 在条件里
	//	{"prompt": (p > 0 ? 1.5 : 0) * c}    拒绝：c 在算术位置
	//
	// cond **向下继承**：条件里的 `p > 0 && len > 100` 整棵都是条件，
	// 比较与逻辑运算符的两侧也不例外。而三元的两个分支从条件进入值位置，
	// 所以要清掉——除非外层本来就在条件里（嵌套条件）。
	cond bool
}

func walkShape(node ast.Node, sh *shape, at pos) error {
	switch n := node.(type) {
	case nil:
		return nil

	case *ast.MapNode:
		if at.slot != "" {
			// 一个槽里又出现了一张表：`{"prompt": {...}}`。那多半是把
			// units 写错了位置，而它会一路变成一次「不是数值」的报错，
			// 报错的地方离写错的地方很远。
			return fmt.Errorf("%s 的位置上不能是一张单价表；计量项要写在 units 里", at.slot)
		}
		for _, p := range n.Pairs {
			pair, ok := p.(*ast.PairNode)
			if !ok {
				return fmt.Errorf("单价表里有无法识别的元素 %T", p)
			}
			name, err := stringKey(pair.Key)
			if err != nil {
				return err
			}
			if _, known := priceSlots[name]; !known {
				return fmt.Errorf("未知的键 %q；可用的键是 %s", name, slotList())
			}
			// 只有**结果位置**的表才贡献键（at.root 就是那个标记，见 pos）。
			//
			//	let m = {"prompt": 1.5, "completion": 2.0}; {"prompt": m["prompt"]}
			//
			// 结果表只引用了 prompt，m 里没用到的 completion 不该进 Keys()：
			// 它成不了任何一次结算的 Declared，进了并集只会让 price list 与
			// 写入提示说「覆盖齐全」，而结算按命中那张表的声明报缺价。
			// 语句序列里被丢弃的中间式同理。
			//
			// 这条判据**只收窄收集，不放松校验**：下面这些检查（未知键、
			// 槽位、units 的形状、用量变量的位置）对每一张表照旧全跑，
			// 无论它最终成不成得了结果。
			//
			// 三元的两个分支仍走在 root 位置，所以没命中的分支照样进并集：
			// 「未走到的分支里写错键要在编译期暴露」那条原意不变。
			if at.root {
				sh.keys[name] = struct{}{}
			}
			if name == "units" {
				if err := walkUnits(pair.Value, sh); err != nil {
					return err
				}
				continue
			}
			// 槽里的表达式是**单价**，不是条件：用量变量到这里就不许再出现了。
			if err := walkShape(pair.Value, sh, pos{slot: name}); err != nil {
				return err
			}
		}
		return nil

	case *ast.ConditionalNode:
		// 条件子树整棵都在条件位置：`p > 0 && len > 100` 的两个比较都在里面，
		// 而其中用量变量只能作条件——这个位置正是它们唯一合法的去处。
		// 它同时是**值位置**（root 清掉）：条件求值出来是布尔值，
		// 从来不参与结果形状，`{...} ? A : B` 里那个条件并不是一张单价表。
		if err := walkShape(n.Cond, sh, condPos(valuePos(at))); err != nil {
			return err
		}
		// 两个分支回到值位置：三元在表里选一个槽、在根位置选一张表，
		// 两种都要继续按同一个位置检查——包括 root：根位置的三元两支都得是表，
		// `len <= 100 ? {"prompt": 1.0} : 2.0` 正是要在这里被拦住。分支里也
		// 可能用到用量变量，但那时它必须自己再写一个条件。
		//
		// cond 在这里**不无条件清掉**：`p > 0 ? A : B` 的三元本身就在外层
		// 条件里时（`p > 0 ? (c > 0 ? A : B) : B`），内层三元整棵都还在条件
		// 位置，用量变量照样合法——那正是嵌套条件要能通过的原因。
		if err := walkShape(n.Exp1, sh, at); err != nil {
			return err
		}
		// 走完一支就取一次键，别等两支都走完再回头解析。
		//
		// 两支可以各写一个同名 let，而 binds 是一张平表：第二支登记的绑定
		// 会盖掉第一支的。等到最后统一解析时，两支都会被解析成后登记的那个
		// 绑定，键集看起来就一致了——「两支各写一张不同的表」正是要拦的写法。
		var thenKeys map[string]struct{}
		if at.root {
			thenKeys = sh.rootTokenKeys(n.Exp1)
		}
		if err := walkShape(n.Exp2, sh, at); err != nil {
			return err
		}
		// 根位置选出的是**整张**单价表，而结算只认命中那一支的键
		// （billing.Prices.Declared）；Program.Keys 却是结果位置上所有分支的
		// 并集（见 Program.keys）。两支的 token 计价项不同时，两处口径就分岔：
		//
		//	c > 100 ? {"prompt": 1.5, "completion": 7.5} : {"prompt": 1.5}
		//
		// 按并集看两项都定了价（price list、写入提示都这么看），而用量含
		// completion 又走进假分支时，整笔结算 ErrMissingPrice。所以两支必须
		// 给同一组 token 计价项——单价可以不同，不用的那一项写 0。
		//
		// 嵌套三元由下面的递归逐层管：每层只比较自己两支，各层都一致
		// 就等价于所有叶子分支的键集一致。
		//
		// 只比 token 计价项：units 与一次性费用不在 ErrMissingPrice 的判据里，
		// 一支给 units 而另一支不给是正常写法（见 worstTable）。
		if at.root {
			return checkBranchTokenKeys(thenKeys, sh.rootTokenKeys(n.Exp2))
		}
		return nil

	case *ast.CallNode:
		if ident, ok := n.Callee.(*ast.IdentifierNode); ok && ident.Value == "tier" {
			if len(n.Arguments) != 2 {
				return fmt.Errorf("tier 需要两个参数（档位名与值），当前 %d 个", len(n.Arguments))
			}
			if _, err := stringKey(n.Arguments[0]); err != nil {
				return fmt.Errorf("tier 的档位名必须是字符串字面量: %w", err)
			}
			// tier 是透明的：它只记下档位名并原样返回第二个参数。
			// 它外面的位置原样穿过第一个参数，所以 cond 也跟着穿过去。
			return walkShape(n.Arguments[1], sh, at)
		}
		if ident, ok := n.Callee.(*ast.IdentifierNode); ok && ident.Value == "isPeak" {
			// 高峰时段必须是字面量，而且当场解析一遍：它是这条规则固定的一部分，
			// 写错了不该等到某一次计价才发现——规则入库时会走 Compile
			// （见 internal/pricing 的 admin），于是错写法在保存时就报出来。
			// 与 tier 要求档位名是字面量同一条理由：运行期才知道的窗口
			// 没法在规则被保存时给人看。
			// 这一遍解析里包含「全天段不能与其他段并列」，那一条是为了钱
			// 上移到这里来的——预留那一步不跑求值（见 ParsePeakWindows）。
			if len(n.Arguments) != 1 {
				return fmt.Errorf("isPeak 需要一个参数（高峰时段，例如 \"08:00-22:00\"），当前 %d 个", len(n.Arguments))
			}
			literal, err := stringKey(n.Arguments[0])
			if err != nil {
				return fmt.Errorf("isPeak 的高峰时段必须是字符串字面量，当前是 %T", n.Arguments[0])
			}
			if _, err := ParsePeakWindows(literal); err != nil {
				// 写法错就报写法错，不裹一层 ErrCalendarRequired：那句话说的是
				// 「日历这一头没接上，或者那一天的数据查不到」，而这里错的是
				// 规则自己写的那个串，要改的是它。裹上去之后写规则的人看到的
				// 第一句是「日历不可用」，矛头指向了数据那一侧。
				return err
			}
			// isPeak 的返回值是布尔，落在根位置就不是一张单价表。
			if at.root {
				return fmt.Errorf("结果必须是一张单价表，当前是函数 %q 的结果", calleeName(n))
			}
			return nil
		}
		// fixed/minimum/hour/isWorkday/isHoliday/isPeak 返回数值或布尔，
		// 位置不变。
		// 参数本身不参与结果形状，但里面不该有单价表——
		// 规则作者不会那么写，真写了也一定是把 units 放错了地方。
		//
		// 参数**不算条件**：fixed(isHoliday() ? 0.2 : 0.5) 里的三元在函数
		// 外面看是个什么值？分支里出现用量变量时同样要自己写条件，
		// 所以这里给值位置。
		for _, arg := range n.Arguments {
			if err := walkShape(arg, sh, pos{slot: "函数参数"}); err != nil {
				return err
			}
		}
		// fixed/minimum/hour/isWorkday/isHoliday 返回数值或布尔。落在根位置
		// 时结果不是一张单价表——这种写法历来能编过、到求值才炸，而现在
		// 根位置的三元比较会先看到它，不在这里说清楚，作者照着
		// 「键 X 只在某支出现」那条去改是补不出来的。
		if at.root {
			return fmt.Errorf("结果必须是一张单价表，当前是函数 %q 的结果", calleeName(n))
		}
		return nil

	case *ast.BinaryNode:
		// 运算符决定子节点是什么位置：比较与逻辑运算符的两侧都是条件，
		// 算术运算符（含 `+ - * / %`、位运算、拼接、合并）的两侧都是值。
		// `in` 也归条件一侧：`units["img"] in [1,2]` 是条件写法。
		sub := at
		if !isConditionOperator(n.Operator) {
			// 算术位置的子树整体都是「价格位置」：即便它外面包着三元，
			// `(p > 0 ? A : B) * c` 里的 c 仍然在乘数上。
			sub.cond = false
		}
		if err := walkShape(n.Left, sh, sub); err != nil {
			return err
		}
		return walkShape(n.Right, sh, sub)

	case *ast.UnaryNode:
		// `!`/`not` 取反的是条件，所以里面仍是条件位置；
		// `-`/`+` 改的是数值，用量变量在那里就是一个用量乘数。
		sub := at
		if !isConditionOperator(n.Operator) {
			sub.cond = false
		}
		return walkShape(n.Node, sh, sub)

	case *ast.BuiltinNode:
		// 谓词类内置（sum/map/reduce/all/groupBy/sortBy…）走的是解析器的
		// predicates 表，**不受 DisableAllBuiltins 影响**——它们不是普通
		// 内置函数，解析阶段就定型了，检查阶段只看名字在不在表里。
		// 因而必须在结构检查这一层挡掉：它们都要对一个集合求值，
		// 而规则的输入里没有集合，落进价格位置就是一次静默的量纲错。
		//
		// 白名单里的数学函数也会以 BuiltinNode 出现（max/min/…），
		// 所以这里按**拒绝名单**判而不是整体拒绝。
		if _, blocked := blockedBuiltins[n.Name]; blocked {
			return fmt.Errorf("内置函数 %q 不可用于计价规则：它要对一个集合求值，而规则的输入里没有集合", n.Name)
		}
		// 参数与函数调用同理：`max(p, 1.0)` 是拿用量当乘数。
		for _, arg := range n.Arguments {
			if err := walkShape(arg, sh, pos{slot: "函数参数"}); err != nil {
				return err
			}
		}
		if at.root {
			return fmt.Errorf("结果必须是一张单价表，当前是内置函数 %q 的结果", n.Name)
		}
		return nil

	case *ast.VariableDeclaratorNode:
		// `let x = ...; <结果>` 里的声明部分不是结果。它的值会在后面被按
		// 名字引用，所以按**值**检查：`let x = p * 1.5` 是把用量算进了
		// 一个接下来要当价格用的名字里，`let x = p > 0 ? A : B` 才是分档。
		//
		// 局限：`let x = p; ... x * 1.5` 这样把用量转个手再乘的写法看不出来
		// （要看出它得做一遍数据流分析）。规则作者真要绕过，直接写
		// `let x = p * 1.5` 就行，所以拦这里的价值有限，写下来是为了让
		// 「为什么 let 单独一个分支」有答案。
		if err := walkShape(n.Value, sh, pos{}); err != nil {
			return err
		}
		// 先走值再登记：自引用（`let a = a; a`）在登记前就按普通标识符处理，
		// 不会一上来就顺着绑定绕回自己。登记之后，根位置引用这个名字
		// （`let price = {…}; price`）就按绑定的值继续检查。
		sh.binds[n.Name] = n.Value
		// 结果本身挂在 Expr 上（`let x = ...; <结果>`），与 SequenceNode 的
		// 最后一项同一含义：它继承当前位置，包括 root。漏掉这一支时，
		// `let x = 1.0; c > 100 ? {...} : {...}` 整段不过形状检查——
		// 根位置的三元判据、用量变量的位置判据都能被一个 let 前缀绕过。
		return walkShape(n.Expr, sh, at)

	case *ast.SequenceNode:
		// 形如 `(1.0; {…})`：最后一个节点是结果，其余是中间式。
		// `let` 不走这里——它解析成 VariableDeclaratorNode，结果挂在 Expr 上。
		if len(n.Nodes) == 0 {
			return nil
		}
		for _, stmt := range n.Nodes[:len(n.Nodes)-1] {
			if err := walkShape(stmt, sh, pos{}); err != nil {
				return err
			}
		}
		// 结果那一个继承当前位置——与 expr 的另外两种写法（`if`/`else`
		// 与顶层三元）保持一致：三种写法给同样的规则，检查结果也该一样。
		return walkShape(n.Nodes[len(n.Nodes)-1], sh, at)

	case *ast.IdentifierNode:
		if err := usageInArithmetic(n.Value, at); err != nil {
			return err
		}
		// 用量变量**直接**落在价格位置（`{"prompt": p}`）也是同一件事的
		// 极端形态：单价就是用量本身。它在语法与类型上都合法，
		// 上界的破坏却与 `p * 1.5` 一模一样，所以在同一处拦。
		// 条件位置上的识别符（`p > 0` 里的 p）不受影响。
		if !at.cond {
			if _, isUsage := usageVarSet[n.Value]; isUsage {
				return usagePriceError(n.Value)
			}
		}
		// 其余（注入常量、let 绑定的名字）在槽里是合法的数值，
		// 但在**根位置**意味着结果不是一张表：
		// `len <= 1 ? {"prompt": 1.0} : x` 的假分支正是这一形态，
		// 而它在运行期会让最坏情况求值在 __worst 里撞上「两支形状不一致」。
		//
		// 例外是 let 绑定的名字：`let price = {…}; price` 的结果就是那张表，
		// 改动之前一直是合法写法。按绑定的值、在同一个位置继续检查；
		// visiting 挡住 `let a = a; a` 这类把递归转成环的写法。
		if at.root {
			if bound, ok := sh.binds[n.Value]; ok && !sh.visiting[n.Value] {
				sh.visiting[n.Value] = true
				err := walkShape(bound, sh, at)
				delete(sh.visiting, n.Value)
				return err
			}
			return fmt.Errorf("结果必须是一张单价表，当前是 %s", n.Value)
		}
		return nil

	default:
		// 数值/字符串字面量、成员访问（units["x"]）等。
		//
		// 它们在槽里是合法的数值，但在**根位置**意味着结果不是一张表：
		// `len <= 1 ? {"prompt": 1.0} : 2.0` 的假分支正是这一形态，
		// 而它在运行期会给出一个数字，让最坏情况求值在 __worst 里
		// 撞上「两支形状不一致」。类型检查看不出问题（两支都合法），
		// 所以必须在这里、按位置判。
		if at.root {
			return fmt.Errorf("结果必须是一张单价表，当前是 %T", node)
		}
		return nil
	}
}

// checkBranchTokenKeys 比较根位置三元两支里出现的 token 计价项。
//
// 键集由调用方在走完各自那一支时就取好：let 的绑定是同一张表按名字覆盖的，
// 两支都走完再回头解析，两边会解析成同一个绑定（见 walkShape 的
// ConditionalNode 分支）。
//
// 报错点名**哪个键只在部分分支里出现**、以及它在哪一支里缺席：只写一句
// 「两支键集不一致」，写规则的人还得自己逐字比对两张表。
func checkBranchTokenKeys(thenKeys, elseKeys map[string]struct{}) error {
	onlyThen := keysOnlyIn(thenKeys, elseKeys)
	onlyElse := keysOnlyIn(elseKeys, thenKeys)
	if len(onlyThen) == 0 && len(onlyElse) == 0 {
		return nil
	}

	parts := make([]string, 0, 2)
	if len(onlyThen) > 0 {
		parts = append(parts, fmt.Sprintf("键 %s 只在真分支里出现，假分支里缺席", quoteKeys(onlyThen)))
	}
	if len(onlyElse) > 0 {
		parts = append(parts, fmt.Sprintf("键 %s 只在假分支里出现，真分支里缺席", quoteKeys(onlyElse)))
	}
	// 只说「要一致」不说怎么改，作者会把它当成一条无理由的限制：
	// 结算只认命中那一支的键，另一支没写的项等于没定价。
	return fmt.Errorf("根位置的三元两支必须给同一组 token 计价项（结算只认命中那一支的键）：%s；"+
		"不用那一项的分支请显式写 0", strings.Join(parts, "；"))
}

// rootTokenKeys 取出一个**根位置**子树最终求值出的那张单价表里的 token 计价项。
//
// 调用前提是该子树已经通过 walkShape：根位置只可能是单价表、三元、
// tier(...)、语句序列、let 绑定，或以单价表收尾的 let 表达式。三元在这里
// 只取真分支——两支已由递归校验成同一组键（见 walkShape 的 ConditionalNode
// 分支）。
//
// 只看 token 计价项，键名以 billing.TokenSlots 为唯一来源（见 tokenPriceSlots
// 与 dsl.go 的 priceSlots）。
//
// 绑定按调用那一刻的 sh.binds 解析，所以要在自己那一支走完、另一支**还没走**
// 的时候调用。
func (sh *shape) rootTokenKeys(node ast.Node) map[string]struct{} {
	return sh.tokenKeysIn(node, map[string]bool{})
}

// tokenKeysIn 是 rootTokenKeys 的递归实现；seen 记着已经顺着绑定走过的名字，
// 挡住 `let a = a; …` 这类自引用。
func (sh *shape) tokenKeysIn(node ast.Node, seen map[string]bool) map[string]struct{} {
	out := map[string]struct{}{}
	switch n := node.(type) {
	case *ast.MapNode:
		for _, p := range n.Pairs {
			pair, ok := p.(*ast.PairNode)
			if !ok {
				continue
			}
			name, err := stringKey(pair.Key)
			if err != nil {
				continue
			}
			if _, token := tokenPriceSlots[name]; token {
				out[name] = struct{}{}
			}
		}
	case *ast.ConditionalNode:
		return sh.tokenKeysIn(n.Exp1, seen)
	case *ast.CallNode:
		if ident, ok := n.Callee.(*ast.IdentifierNode); ok &&
			ident.Value == "tier" && len(n.Arguments) == 2 {
			return sh.tokenKeysIn(n.Arguments[1], seen)
		}
	case *ast.VariableDeclaratorNode:
		// let 的结果挂在 Expr 上（与 walkShape 那一支同一件事）；嵌套 let
		// 是层层套着的 VariableDeclaratorNode，递归自然剥到最后的表。
		return sh.tokenKeysIn(n.Expr, seen)
	case *ast.IdentifierNode:
		// 分支是一个 let 绑定的名字（`let t = {…}; c > 100 ? t : {…}`）：
		// 按绑定的值取键，否则这一支会被当成空集、判成键集不一致。
		if bound, ok := sh.binds[n.Value]; ok && !seen[n.Value] {
			seen[n.Value] = true
			return sh.tokenKeysIn(bound, seen)
		}
	case *ast.SequenceNode:
		if len(n.Nodes) > 0 {
			return sh.tokenKeysIn(n.Nodes[len(n.Nodes)-1], seen)
		}
	}
	return out
}

// calleeName 是调用节点的名字，用于报错。规则里的函数都是标识符，
// 其余形态只是兜底。
func calleeName(n *ast.CallNode) string {
	if ident, ok := n.Callee.(*ast.IdentifierNode); ok {
		return ident.Value
	}
	return fmt.Sprintf("%T", n.Callee)
}

// keysOnlyIn 返回在 a 里、不在 b 里的键，已排序。
func keysOnlyIn(a, b map[string]struct{}) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// quoteKeys 把键名排成 `"a"、"b"` 的形状，让报错里的键名能直接照抄。
func quoteKeys(keys []string) string {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = fmt.Sprintf("%q", k)
	}
	return strings.Join(quoted, "、")
}

// walkUnits 检查计量项表。
//
// units 的键由规则自定义（非 token 计量的名字会随供应商增加，
// 写死成字段意味着每加一项都要改引擎），所以这里只检查结构：
// 它必须是一张 map，值必须是数值。
func walkUnits(node ast.Node, sh *shape) error {
	switch n := node.(type) {
	case *ast.MapNode:
		for _, p := range n.Pairs {
			pair, ok := p.(*ast.PairNode)
			if !ok {
				return fmt.Errorf("units 里有无法识别的元素 %T", p)
			}
			if _, err := stringKey(pair.Key); err != nil {
				return fmt.Errorf("units 的键必须是字符串字面量: %w", err)
			}
			if err := walkShape(pair.Value, sh, pos{slot: "units"}); err != nil {
				return err
			}
		}
		return nil

	case *ast.ConditionalNode:
		if err := walkShape(n.Cond, sh, condPos(pos{})); err != nil {
			return err
		}
		if err := walkUnits(n.Exp1, sh); err != nil {
			return err
		}
		return walkUnits(n.Exp2, sh)

	case *ast.CallNode:
		if ident, ok := n.Callee.(*ast.IdentifierNode); ok && ident.Value == "tier" {
			if len(n.Arguments) == 2 {
				return walkUnits(n.Arguments[1], sh)
			}
		}
		return fmt.Errorf("units 必须是一张计量项表或按条件选出的一张，当前是 %T", node)

	default:
		return fmt.Errorf("units 必须是一张计量项表或按条件选出的一张，当前是 %T", node)
	}
}

// isConditionOperator 判断一个运算符的两侧是不是**条件**位置。
//
// 只有这几类算条件：比较（`> < >= <= == !=`）、逻辑（`&& || and or`）、
// 取反（`! not`）、集合与字符串判定（`in matches contains startsWith endsWith`）。
// 其余运算符——`+ - * / % ** .. ??` 与 `|`——的子节点都算值位置，
// 用量变量出现在那里就是一个用量乘数。
//
// 按名字判而不是按「结果类型是不是 bool」判：类型要等 expr 的类型检查跑完
// 才有，而这一步在形状检查里，拿到的是已经过类型的 AST（这时真有类型），
// 但把一个「看起来是 bool」的算术表达式放行是错的——`p > 0 == q > 0`
// 是条件，而 `p * (q > 0)` 里的 `q > 0` 在外面是乘数，两处的判据不同，
// 只有运算符本身能可靠地区分它们。
func isConditionOperator(op string) bool {
	switch op {
	case ">", "<", ">=", "<=", "==", "!=",
		"&&", "||", "and", "or", "!", "not",
		"in", "matches", "contains", "startsWith", "endsWith":
		return true
	default:
		return false
	}
}

// condPos 在保留当前位置的同时标记「这棵子树是条件」。
//
// 保留 slot 是有意的：条件本身不参与结果形状，而它所在的那个位置的规矩
// （槽里不能再出现一张表）在这里同样适用，只是用量变量被额外允许了。
// root 由调用方用 valuePos 清掉——条件求值出来是布尔值，从来不是结果本身。
func condPos(at pos) pos {
	at.cond = true
	return at
}

// valuePos 标记「这棵子树是值」：它会变成一个数值，参与价格、参与比较、
// 或直接成为结果，总之不是「只被用来判断真假」的那种位置。
//
// root 同时清掉：根位置那条规矩（结果必须是一张单价表）是对**结果**说的，
// 而这里已经进入了某个表达式的内部——`{...} ? A : B` 里那个条件、
// 三元的分支、函数的参数，都不该再按「它就是最终结果」判。
func valuePos(at pos) pos {
	at.root = false
	return at
}

// usageInArithmetic 拦住「用量变量出现在算术位置」。
//
// 这条约定写在包注释里：用量只作条件，不作乘数。破坏它不会报错——
// `{"prompt": p * 1.5}` 语法合法、类型合法、求值也返回一个数，
// 只是单价随用量变了。后果是 EvalWorstCase 给出的上界不再成立，
// 而「余额不为负」这条不变量依赖那个上界（见 Program.EvalWorstCase）。
//
// 判据是**位置**而不是名字出现过：`p > 0 ? A : B` 里 p 也在，
// 但它在条件里，不影响单价的确定性，所以允许。
func usageInArithmetic(name string, at pos) error {
	if at.cond {
		return nil
	}
	if _, isUsage := usageVarSet[name]; !isUsage {
		return nil
	}
	return usagePriceError(name)
}

// usagePriceError 生成「用量变量出现在价格位置」的错误。
//
// 信息里要同时给出三件事，规则作者才知道怎么改：哪个变量不行、
// 为什么不行、以及该怎么写。只报「非法表达式」会让他去查语法，
// 而那个方向永远找不到答案——这句话本身是合法的。
func usagePriceError(name string) error {
	return fmt.Errorf("价格位置不能出现用量 %q：单价不该依赖用了多少，"+
		"否则最坏情况求值不再是上界（预留会不够，结算撞 Capped）；"+
		"如果只是想按用量分档，把它写成条件（如 `%s > 0 ? A : B`）", name, name)
}

// stringKey 取一个字符串字面量键。
//
// 只接受字面量：键名要参与并集检查与账单展示，动态键（变量、拼接）
// 会让这两件事都做不到。规则作者也没有需要动态键的场景——计量项名是固定的几个。
func stringKey(node ast.Node) (string, error) {
	switch n := node.(type) {
	case *ast.StringNode:
		return n.Value, nil
	case *ast.ConstantNode:
		s, ok := n.Value.(string)
		if !ok {
			return "", fmt.Errorf("键 %v 不是字符串", n.Value)
		}
		return s, nil
	default:
		return "", fmt.Errorf("键必须是字符串字面量，当前是 %T", node)
	}
}

// slotList 列出可用的键名，用于错误信息。
func slotList() string {
	names := make([]string, 0, len(priceSlots))
	for k := range priceSlots {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// checkNesting 检查括号的嵌套深度与配对。
//
// expr 的解析器是递归下降的，而 MaxNodes 只数节点——「两千层括号」既不产生
// 节点也不触发 MaxNodes，却会把解析器的递归压到很深；深度到某个点之后
// 会把 goroutine 的栈打穿，那已经不是一个错误而是一次故障。所以这里
// 按字符扫一遍，在**解析之前**把它拦住。
//
// 扫字符而不是走 AST：检查必须发生在解析之前，否则就已经晚了。
// 这只是一个上界估计（字符串里的括号也计入），而计价规则里没有
// 字符串恰好含大量括号的场景——宁可误报，不可漏判。
func checkNesting(source string) error {
	depth, maxDepth := 0, 0
	for i := 0; i < len(source); i++ {
		switch source[i] {
		case '(', '{', '[':
			depth++
			if depth > maxDepth {
				maxDepth = depth
			}
		case ')', '}', ']':
			depth--
			if depth < 0 {
				return fmt.Errorf("括号不配对: 第 %d 个字符处出现了多余的右括号", i+1)
			}
		}
		if maxDepth > maxNesting {
			return fmt.Errorf("嵌套深度超过上限 %d", maxNesting)
		}
	}
	if depth != 0 {
		return fmt.Errorf("括号不配对: 还差 %d 个右括号", depth)
	}
	return nil
}

// --- 编译缓存 ---

// Cache 是一份**有容量上限**的编译产物缓存。
//
// 为什么必须有上限：规则数有限但请求量很大，编译一次求值多次是必须的；
// 而缓存的键来自规则，也就是商家可写的数据。只往里放不清理的话，
// 商家改一次规则就多一条缓存项，改上一万次就是一万条——那是一处
// 由外部输入决定的无限增长，最终把内存吃光。
//
// 淘汰用**近似 LRU**：命中时把条目移到队首，满员时一次丢掉队尾的一批。
// 不用精确 LRU 是因为它的每一步都要写锁，而计价的读远多于写；
// 这里一次丢一批（而不是每次插入丢一条）把写锁里的链表操作摊薄了。
//
// 容量保证：淘汰在插入之前发生，且每次丢掉的条数 ≥ 1，
// 所以 items 的长度永远不超过 max。
type Cache struct {
	mu    sync.Mutex
	max   int
	items map[string]*cacheEntry
	// order 是使用顺序：队首最新、队尾最旧。
	order *entryList
}

// cacheEntry 是缓存里的一项。
type cacheEntry struct {
	key  string
	prog *Program
	// elem 是它在 order 里的位置，淘汰时用来摘掉自己。
	//
	// 它恒非 nil：条目只在 Get 与 GetWithGlobals 里构造，`elem` 取的是
	// `order.pushFront` 的返回值（恒非 nil），而 items 里的条目删除时
	// 链表上那一节一并摘掉。`moveToFront` 里 `e == nil` 那一支因此走不到；
	// 若哪天多出一处构造 `cacheEntry` 却漏设 `elem`，那一支会从「返回」
	// 变成解引用 nil。
	elem *listElement
}

// entryList 是一个极简的双向链表。
//
// 用自己写的而不是 container/list：只需要「移到头部」「摘掉尾部」两个操作，
// 而 container/list 的值是 any，每次操作都要一次接口断言与装箱。
//
// **它不自己维护长度**：条目数只有 `items` 一个权威来源（见 Cache.Len）。
// 顺序表再记一份的话，那一份没有任何读点，而读代码的人会以为存在第二份真相。
type entryList struct {
	head, tail *listElement
}

type listElement struct {
	key        string
	prev, next *listElement
}

// NewCache 创建一份容量为 max 的编译缓存。max 小于 1 时取 1。
func NewCache(max int) *Cache {
	if max < 1 {
		max = 1
	}
	return &Cache{
		max:   max,
		items: make(map[string]*cacheEntry, max),
		order: &entryList{},
	}
}

// Get 取一条编译好的规则。未命中时编译并存入。
//
// 键由调用方给出（见 pipeline.Rule.CacheKey，它是 `<规则 id>:<版本>`）。
// 那个「版本」是**源码原文的哈希**（见 internal/pricing 的 versionOf），
// 与 pricing_rule_versions 表里的 version **不是一回事**，两套编号互不相干：
//
//   - 缓存键里的版本 = `fnv64a(表达式源码原文)`。**空白敏感**——改一个空格，
//     哈希就变，缓存分成两份而不是命中原来那份。
//   - pricing_rule_versions.version = 那条规则的第几次改版，
//     由 `COALESCE(MAX(version), 0) + 1` 数出来。它不进缓存键，
//     改它**不会**让缓存失效。
//
// 所以「用 (ruleID, version) 做键就不会被空白差异分成两份」是不成立的：
// 那个 version 本身就是源码的函数。真要让它对空白不敏感，该改的是 versionOf
// （先把源码规范化再哈希），而不是这里。
//
// 返回值里的 *Program 是共享的：它不可变且并发安全，调用方不要改动它。
func (c *Cache) Get(key, source string) (*Program, error) {
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		c.order.moveToFront(e.elem)
		c.mu.Unlock()
		return e.prog, nil
	}
	c.mu.Unlock()

	// 编译在锁外做：它可能很慢（解析 + 类型检查），
	// 而锁里只有一次 map 写入。代价是并发同一个键时会重复编译一次，
	// 但结果一致，多花的是 CPU 而不是正确性。
	prog, err := Compile(source)
	if err != nil {
		// 编译失败**不入缓存**：规则是商家可写的，把失败也缓存等于给
		// 每条写错的规则留一个常驻条目，而失败结果的复用价值为零。
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// 期间别人可能已经放进去了，用先到的那份。
	if e, ok := c.items[key]; ok {
		c.order.moveToFront(e.elem)
		return e.prog, nil
	}
	c.evictLocked()
	elem := c.order.pushFront(key)
	c.items[key] = &cacheEntry{key: key, prog: prog, elem: elem}
	return prog, nil
}

// GetWithGlobals 与 Get 相同，但用 CompileWithGlobals 编译。
//
// **目前没有生产入口：只有本包的测试在调它。** 与 CompileWithGlobals 同样，
// 接线前先回答 Env.Globals 注释里列的问题。
//
// 键里必须体现 globals 的名字与类型：同一个 (ruleID, version) 配上不同的
// globals 声明是不同的编译产物，而它们调用的校验规则也不同。
// 调用方若搞错了键，表现为「编译通过但字段对不上」，很难定位，
// 所以这里把 globals 的签名并进键、由本方法负责拼接。
func (c *Cache) GetWithGlobals(key, source string, globals map[string]any) (*Program, error) {
	full := key + "\x00" + globalsSignature(globals)

	c.mu.Lock()
	if e, ok := c.items[full]; ok {
		c.order.moveToFront(e.elem)
		c.mu.Unlock()
		return e.prog, nil
	}
	c.mu.Unlock()

	prog, err := CompileWithGlobals(source, globals)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[full]; ok {
		c.order.moveToFront(e.elem)
		return e.prog, nil
	}
	c.evictLocked()
	elem := c.order.pushFront(full)
	c.items[full] = &cacheEntry{key: full, prog: prog, elem: elem}
	return prog, nil
}

// Len 返回当前缓存的条目数。
//
// 长度以 `items` 为准——顺序表不自己维护长度（见 entryList 的注释）。
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// evictLocked 在满员时一次性淘汰最旧的约 1/8。
//
// 一次淘汰一批而不是每次淘汰一条，是为了把「写锁 + 链表操作」的次数
// 摊薄：每次插入都淘汰一条的话，满载之后每次插入都要做一次链表摘除，
// 而那是在热路径的写锁里。
//
// 淘汰比例取 1/8：太小会在满载后频繁触发淘汰，太大则让常用规则也被丢掉。
func (c *Cache) evictLocked() {
	if len(c.items) < c.max {
		return
	}
	drop := c.max / 8
	if drop < 1 {
		drop = 1
	}
	for i := 0; i < drop; i++ {
		oldest := c.order.tail
		if oldest == nil {
			return
		}
		c.order.remove(oldest)
		delete(c.items, oldest.key)
	}
}

func (l *entryList) pushFront(key string) *listElement {
	e := &listElement{key: key, next: l.head}
	if l.head != nil {
		l.head.prev = e
	}
	l.head = e
	if l.tail == nil {
		l.tail = e
	}
	return e
}

func (l *entryList) moveToFront(e *listElement) {
	if e == nil || l.head == e {
		return
	}
	l.remove(e)
	e.prev, e.next = nil, l.head
	if l.head != nil {
		l.head.prev = e
	}
	l.head = e
	if l.tail == nil {
		l.tail = e
	}
}

func (l *entryList) remove(e *listElement) {
	if e == nil {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		l.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		l.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

// globalsSignature 把 globals 的名字与类型折成一个稳定的字符串，用于缓存键。
//
// 只取名字与类型，不取值：值每次求值都可以不同，缓存的是**编译产物**，
// 而编译产物只依赖声明。
func globalsSignature(globals map[string]any) string {
	if len(globals) == 0 {
		return ""
	}
	names := make([]string, 0, len(globals))
	for k := range globals {
		names = append(names, k)
	}
	sort.Strings(names)
	var b []byte
	for _, name := range names {
		t, err := globalType(name, globals[name])
		if err != nil {
			b = append(b, name...)
			b = append(b, '?')
			continue
		}
		b = append(b, name...)
		b = append(b, ':')
		b = append(b, reflect.TypeOf(t).String()...)
		b = append(b, ';')
	}
	return string(b)
}
