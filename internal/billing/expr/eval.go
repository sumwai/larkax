package expr

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/expr-lang/expr/vm"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/money"
)

// LocalTime 是求值用到的「当地」口径：地区时区下的那个钟点，与那一天在日历上
// 算不算休息日。
//
// 两个值出自**同一份**解析：钟点由地区时区折出来，日历属性取自当地那一天
// ——问 isWorkday() / isHoliday() 的规则走 DayInfo，问 isPeak() 的规则用判定
// 一并带回来的那一份（见 runtime.resolveLocal 与 runtime.adoptPeakDay）。
// 也就是说，它们与规则里 isPeak() / isWorkday() 判的是同一份依据，而不是
// 结算那一侧另算一遍凑出来的。
type LocalTime struct {
	// Hour 是 Env.Region 的时区下 Env.At 是几点（0–23）。
	//
	// 它是这次求值解析「当地」时算出来的那个钟点，而这份解析是 isWorkday() /
	// isHoliday() / isPeak() 共用的：只问 isWorkday() 的规则一样会填这一格，
	// 不是只有峰谷那条路才有值。isPeak 的窗口按当地零点起算（见 PeakWindow），
	// 所以这一格也是「当时当地几点」——复算一笔分时单子缺的输入。
	Hour int
	// OffDay 是当地那一天在日历上算不算休息日。
	//
	// 它是 isWorkday() 的相反数：周末与法定节假日都是休息日，调休补班日
	// 不是（见 DayInfo.OffDay）。
	OffDay bool
}

// Result 是一次求值的结果。
//
// 它只装单价与一次性费用，不装金额：单价乘用量由 billing.ComputeWithCharges
// 做，那一步是整数定点运算、无舍入，而且逐项拆得开（见包注释）。
type Result struct {
	// Prices 是求值得到的一组单价。
	Prices billing.Prices
	// Charges 是每请求的固定费与最低消费。
	Charges billing.Charges
	// Tier 是规则用 tier() 记下的档位名；规则没调 tier、或这是最坏情况
	// 求值时为空。最坏情况要算遍所有分支，「命中了哪一档」因而没有答案——
	// 与其给一个会误导的值，不如留空（见 runtime.worstCase）。
	Tier string
	// At 是这次求值用的时刻，直接来自 Env。
	//
	// 单独回传是为复算准备的：事后要能回答「当时按哪一刻算的价」。
	//
	// 这条链路已经接上：pipeline.Settle 把它放进 Charge.PricedAt，落库层
	// 写进 usage_events.priced_at（见 internal/ledger/store 的 Save）。
	// 于是「当时按哪一刻算的价」事后读得出来，不必再猜。
	At time.Time
	// LocalTime 是这次求值用到的「当地」口径：Env.Region 的时区下 Env.At
	// 是几点，以及那一天在日历上算不算休息日。
	//
	// 回传它是为复算：usage_events.local_hour / is_off_day 两列取的就是它
	// （见 internal/ledger/store 的 insertUsageEvent）。那两列**必须**来自
	// 求值自己——在结算或落库那一侧按 Env.Region 重算一遍，等于把一条计费
	// 判据抄成两份，而两份迟早会不一致。
	//
	// # 什么时候有值
	//
	// 规则用到**地区**这一套口径时非 nil——isWorkday() / isHoliday() /
	// isPeak() 里任意一个被求值过。其余情形为 nil，落库层写 NULL——
	// 「不知道」，而不是把零值写成「当地 0 点、不是休息日」。
	//
	// hour(tz) 不算：它的时区由规则作者逐次给出，问的是那一个钟表，与地区的
	// 日历不是一套口径；而那个时区名随规则源码落在
	// billing_requests.pricing_expr_snapshot 里，复算拿它与 At 就够。给这类
	// 规则填一个地区的钟点，会得到一个看起来权威、却与规则实际用的那个不同
	// 的数——那比留空更糟。
	//
	// 最坏情况求值时恒为 nil：那一侧把条件整个换成 __worst（两支都求值，
	// 条件不求），这几个函数根本不会被调到——与 Tier 留空同源
	// （见 EvalWorstCase），而预留那一次的结果本来也不落库。
	LocalTime *LocalTime
	// Keys 是这条规则可能给出的键，已排序。
	//
	// 它是编译期收集到的并集**加上**本次结果里出现的键：
	// 本次没走到的分支这次不会产生键，但账单展示需要知道
	// 「这条规则还可能给哪些项定价」。
	Keys []string
}

// Eval 按用量求值，得到单价与一次性费用。
//
// 这个函数**不读墙钟、不查库、不发请求**：时间从 Env.At 来，日历从
// Env.Calendar 来。于是「同一次请求在预留时与结算时给出同一个答案」
// 是可以被证明的，而不是靠约定维持的。
func (p *Program) Eval(env Env) (Result, error) {
	return p.eval(p.prog, env, false)
}

// EvalWorstCase 求这次请求的**上界**：每个价格位置取所有分支里的最高价。
//
// # 为什么需要上界
//
// 预留必须在请求发出前冻结一笔钱，而这笔钱不能小于结算的金额——否则
// 「余额不为负」这条不变量就需要例外，而例外要牵扯到每一处读余额的代码。
//
// # 怎么算
//
// 编译期已经把每个三元节点换成了 `__worst(exp1, exp2)`（见 worstOptions），
// 于是这里只有一条求值路径，而它给出的是各分支的**逐槽最大值**——
// 不是「某一支的全部」，而是「每一槽都取各支里最贵的那一个」。
//
// 逐槽取最大值是必要的：规则完全可以让一个分支的输入单价高、另一个分支的
// 缓存单价高。取其中一支作为上界，另一支就会在某些用量下超出去，
// 而那正是「上界算小了」的典型形态——结算撞 Capped，
// 而 Capped 本该是「上界算错了」的信号，不该成为常态。
//
// # 上界成立的前提
//
// 单价不随用量变化。这是本包强制的（用量只能作条件，见 Compile 与
// inspectShape），也是逐槽取最大值能成立的原因：既然每一支的单价都是常数，
// 那么对任意用量，命中的那一支的每个槽都 ≤ 逐槽最大值，而用量非负，
// 所以金额也 ≤。包内有一条测试枚举一批真实用量逐个验证这条。
func (p *Program) EvalWorstCase(env Env) (Result, error) {
	return p.eval(p.worst, env, true)
}

// eval 是两种求值的共同实现。worst 为 true 表示这是最坏情况那一次。
func (p *Program) eval(prog *vm.Program, env Env, worst bool) (Result, error) {
	if err := env.validate(); err != nil {
		return Result{}, err
	}

	rt := &runtime{
		at:        env.At,
		region:    env.Region,
		calendar:  env.Calendar,
		worstCase: worst,
	}
	runEnv := rt.env(newUsageView(env.Usage))
	// 注入常量覆盖到运行期环境。名字与类型已在编译期检查过；
	// 这里再挡一次保留名，是因为 Env 是调用方构造的，
	// 而编译产物可能来自缓存——两份数据未必出自同一次调用。
	for name, v := range env.Globals {
		if _, reserved := reservedNames[name]; reserved {
			return Result{}, fmt.Errorf("%w: 注入常量 %q 与保留名冲突", ErrGlobalsInvalid, name)
		}
		runEnv[name] = v
	}

	out, err := vm.Run(prog, runEnv)
	if err != nil {
		return Result{}, fmt.Errorf("规则求值失败: %w", err)
	}
	return p.result(rt, out)
}

// result 把 expr 返回的 map 折成 Result。
//
// 这里的取舍是**全部报错、不做任何猜测**：未知的键、不是数值的单价、
// NaN/Inf、负数——都返回错误而不是绕过。理由是这些值最终会变成金额，
// 而金额算错不会报错，只会在账目上悄悄错掉。
func (p *Program) result(rt *runtime, out any) (Result, error) {
	table, err := asTable(out)
	if err != nil {
		return Result{}, err
	}

	res := Result{At: rt.at, Tier: rt.tierName}
	// 这两个值只取**已经解析出来的**那一份，不在这里补解析：没解析过就说明
	// 规则没用地区那一套口径，这两格没有值可写（见 Result.LocalTime）。
	if rt.local != nil {
		res.LocalTime = &LocalTime{Hour: rt.local.hour, OffDay: rt.local.info.OffDay}
	}

	// 先落到浮点，全部校验通过之后一次性转成 money.Amount。
	// **舍入只在这一个地方发生**，之后全是整数运算——
	// 一处舍入与另一处不一致会在对账时表现为一笔永远找不平的差额。
	//
	// 扫两遍，顺序由代码定死：第一遍只收单价键，第二遍才处理 units。
	// 合成一个循环写就会让撞名判据查到一个**只填了一半**的 floats——
	// 「查得到」还是「查不到」取决于 map 先迭代到哪个键，而 map 的迭代顺序
	// 是随机的，于是同一条撞名规则有时报错、有时静默通过。
	// 判据不能挂在一个随机的东西上。
	floats := map[string]float64{}
	units := map[string]float64{}

	for key, v := range table {
		if _, known := priceSlots[key]; !known {
			return Result{}, fmt.Errorf("结果里有未知的键 %q；可用的键是 %s", key, slotList())
		}
		if key == "units" {
			continue
		}
		price, priceErr := asPrice(v)
		if priceErr != nil {
			return Result{}, fmt.Errorf("单价 %q 非法: %w", key, priceErr)
		}
		floats[key] = price
	}

	// 第二遍：units 表。走到这里 floats 已经收全，撞名判据是确定的。
	//
	// units 表内部不需要同一条判据的对应物：它自己是一张 map，键名天然唯一，
	// 「计量项名之间撞名」无从发生。哪天真把它换成键名可以重复的形态（例如
	// 数组），这里才需要补一条。
	if unitValue, ok := table["units"]; ok {
		m, isTable := unitValue.(map[string]any)
		if !isTable {
			return Result{}, fmt.Errorf("units 必须是一张计量项表，当前是 %T", unitValue)
		}
		for name, raw := range m {
			price, priceErr := asPrice(raw)
			if priceErr != nil {
				return Result{}, fmt.Errorf("计量项 %q 的单价非法: %w", name, priceErr)
			}
			if _, clash := floats[name]; clash {
				// 计量项名与单价的键名撞了（例如一个叫 prompt 的计量项）。
				// 两者分别进 Prices 与 Prices.Units 两个命名空间，
				// 撞了之后账单上分不清哪个是哪个。
				return Result{}, fmt.Errorf("计量项名 %q 与单价键名冲突", name)
			}
			units[name] = price
		}
	}

	// 一次性费用由副作用通道（fixed/minimum）写入，不是结果表里的键。
	// 分开是有意的：fixed(0.2) 可以写在任何一个价格位置的表达式里
	// （例如 `{"prompt": isWeekend ? fixed(0.2) : 1.5}`），而
	// 「结果是数值」这件事不能因此改变——否则形状校验就没法做了。
	// 两个通道合起来取：结果表里的 fixed/minimum 是**主**通道，
	// fixed()/minimum() 是补充。两者都只写一次是正常写法，
	// 同时出现说明规则写重了——那时以表里的值为准，因为它是规则作者
	// 最终看到的那一处，而取重会让一笔固定费收两次。
	//
	// 之所以要支持表里写固定值：`"fixed": 0.02` 是任何人第一次写规则时
	// 会写的形态，而固定费**与用量无关**，本来就不需要包一层函数。
	// 只支持函数形态的话，`"fixed": 0.02` 会被当成一个普通单价静默丢掉。
	fixed, err := chargeOf(floats, "fixed", rt.fixedAmount)
	if err != nil {
		return Result{}, err
	}
	minimum, err := chargeOf(floats, "minimum", rt.minimumAmount)
	if err != nil {
		return Result{}, err
	}
	res.Charges = billing.Charges{
		Fixed:   amountFrom(fixed),
		Minimum: amountFrom(minimum),
	}

	res.Prices = billing.Prices{Declared: make(map[string]bool, len(floats))}
	// token 单价逐项从 billing.TokenSlots 写入：那份表是「有哪些 token 槽位」
	// 的唯一来源，这里不再手抄键名与 Prices 字段的对应关系。
	//
	// Declared 记的是规则里**写过**哪几个 token 键。floats 只装规则给了的键，
	// 所以查它的存在性即可。不能拿单价是不是 0 判断：「写了 0」（这一项免费）
	// 与「没写」（规则漏了）都落在 0 上，而计费层要区别对待这两件事。
	for _, slot := range billing.TokenSlots {
		price, ok := floats[slot.Name]
		if !ok {
			continue
		}
		slot.SetPrice(&res.Prices, amountFrom(price))
		res.Prices.Declared[slot.Name] = true
	}
	// fixed/minimum 不是单价，不进 Prices。上面已经把它们折进 Charges 了。
	if len(units) > 0 {
		res.Prices.Units = make(map[string]money.Amount, len(units))
		for name, f := range units {
			res.Prices.Units[name] = amountFrom(f)
		}
	}

	res.Keys = p.keysOf(table)
	return res, nil
}

// amountFrom 把一个已经校验过的浮点单价转成 money.Amount。
//
// **这是本包唯一做舍入的地方。** money.Parse 的十进制定点化对超过 8 位
// 小数的部分四舍五入，转换之后单价就是整数定点数，乘用量时不再产生小数。
//
// 走十进制字符串而不是直接乘 10^8 再取整：规则作者写的是十进制字面量
// （0.0000015 这种每 token 的单价很常见），对账的人也是按十进制核。
//
// 格式化必须用 'f'（定点）而不是 'g'：'g' 对小于 1e-4 的值会给出科学计数法
// （1.5e-06），而 money.Parse 只认十进制字面量。那会让**每 token 量级的单价
// 全部变成零**，而且不报错——账单上表现为「这段时间的用量都不花钱」。
// -1 精度表示「用最少的位数但不丢信息」，所以 0.1 写出来就是 "0.1"，
// 不会带上浮点尾巴。
//
// 解析失败只可能是超出了 money 可表示的范围，返回零：
// 一个金额为零的单价是看得出来的，而回绕出来的值不是。
func amountFrom(v float64) money.Amount {
	if !finite(v) || v < 0 {
		// 走不到这里：asPrice 已经拦过。留着兜底是因为
		// 「用户被倒收钱」的代价远大于一个零值。
		return money.Zero
	}
	a, err := money.Parse(strconv.FormatFloat(v, 'f', -1, 64))
	if err != nil {
		return money.Zero
	}
	return a
}

// chargeOf 取一项一次性费用。
//
// 两个来源按优先级合并：结果表里的键（`"fixed": 0.02`）优先，
// 副作用通道（`fixed(0.02)`）兜底。表里的值优先是因为它是规则作者
// 最终写下的那一处；两个来源同时出现说明规则写重了，取其一比求和安全——
// 求和会让一笔固定费收两次，而那种错在账单上表现为「固定费莫名其妙翻倍」。
func chargeOf(table map[string]float64, key string, fromCall float64) (float64, error) {
	if v, ok := table[key]; ok {
		return v, nil
	}
	if !finite(fromCall) {
		return 0, fmt.Errorf("费用 %q 非法: 不是有限数值", key)
	}
	if fromCall < 0 {
		return 0, fmt.Errorf("费用 %q 为负: %v", key, fromCall)
	}
	return fromCall, nil
}

// asTable 把求值结果折成 map[string]any。
//
// expr 返回的本来就是 map[string]any，但接口类型在这里断一次更安全：
// 若哪天编译选项变了、结果成了别的类型，要在这一步报出来，
// 而不是在后面按字段取值时 panic。
func asTable(out any) (map[string]any, error) {
	switch t := out.(type) {
	case map[string]any:
		return t, nil
	case nil:
		return nil, errors.New("规则求值结果为空")
	default:
		return nil, fmt.Errorf("规则求值结果应当是一组单价，当前是 %T", out)
	}
}

// asPrice 校验一个单价或一次性费用的金额。
//
// 三条都拦：必须是数值、不能是 NaN/Inf、不能为负。
//
//   - 非数值：规则里写出 `"1.5"` 这种字符串单价时会一路变成金额。
//   - NaN/Inf：一旦流进金额，比较（Capped、Max、求和）的行为就由 IEEE
//     而不是由业务决定，「预留 ≥ 结算」那条不变量会静默失效。
//     expr 里 `0.0/0.0` 与 `1.0/0.0` 都编得过，所以这条必须查。
//   - 负数：负单价意味着上游付钱给用户。它不会报错，只会让余额慢慢往上涨，
//     而那种现象通常被当成「额度没扣对」，排查方向完全跑偏。
//
// 零是允许的：免费档、限时免费、某个计量项这一档不收钱，都是正常规则。
func asPrice(v any) (float64, error) {
	f, ok := asFloat(v)
	if !ok {
		return 0, fmt.Errorf("必须是数值，当前是 %T", v)
	}
	if math.IsNaN(f) {
		return 0, errors.New("是 NaN")
	}
	if math.IsInf(f, 0) {
		return 0, errors.New("是无穷大")
	}
	if f < 0 {
		return 0, fmt.Errorf("为负: %v", f)
	}
	return f, nil
}

// asFloat 把 expr 的值折成 float64。
//
// expr 的整数运算是 native int、浮点运算是 float64，同一张表里两种都可能出现
// （`{"prompt": 1}` 与 `{"prompt": 1.5}`），所以两种都要认。
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case float32:
		return float64(n), true
	default:
		return 0, false
	}
}

// keysOf 返回这条规则可能给出的键，稳定排序。
func (p *Program) keysOf(table map[string]any) []string {
	seen := make(map[string]struct{}, len(p.keys)+len(table))
	for k := range p.keys {
		seen[k] = struct{}{}
	}
	for k := range table {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
