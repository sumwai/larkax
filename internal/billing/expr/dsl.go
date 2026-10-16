package expr

import (
	"fmt"
	"math"
	"time"

	"github.com/sumwai/larkax/internal/billing"
)

// 规则里可用的变量，分成三组。
//
// 分组是为了让「用量」与「时间」在编译期的检查方式不同：
// 用量变量是条件，时间与地区是外部输入。
var (
	// usageVarNames 是用量变量。名字都很短，因为规则要能一行写完。
	//
	//	p         非缓存输入 token
	//	c         输出 token（不含推理 token）
	//	cr        命中缓存的输入 token
	//	cc        写入缓存的 token
	//	reasoning 推理 token
	//	len       输入侧上下文长度，含缓存读写（见 contextLength）
	//	units     非 token 计量的计数表，例如 units["img"]
	//
	// 前五项与 billing.Usage 的字段一一对应。len 与它们不重叠是有意的：
	// 判档看的是「送进去多少上下文」，而不是「其中有多少要花钱」。
	usageVarNames = []string{"p", "c", "cr", "cc", "reasoning", "len", "units"}

	// clockVarNames 是求值时刻与地区。
	//
	// 它们来自 Env，**绝不能由规则自己去取**：读墙钟会让预留与结算
	// 在不同时刻得到不同的结果（见包注释）。
	clockVarNames = []string{"at", "region"}
)

// usageVarSet 是 usageVarNames 的集合形态，供按名字判断的地方用。
//
// 从同一个切片派生而不是另写一份字面量：用量变量只在一个地方定义，
// 结构检查与运行期环境都从它出发。手抄一份的风险正好落在最需要它的那件事上——
// 加一个用量变量时漏掉清单，于是新变量能被写进乘数位置而检查看不出来。
var usageVarSet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(usageVarNames))
	for _, name := range usageVarNames {
		out[name] = struct{}{}
	}
	return out
}()

// usageVars 构造编译期环境里的用量变量。
//
// 计数一律声明成 int64：规则里既有 `len <= 32000` 这样的比较，也有
// `units["img"] <= 3` 这样的查表；同一个表达式里混着 int 与 int64
// 会让 expr 报「类型不匹配」，而那个错误对规则作者毫无提示性。
//
// units 是个 map 而不是固定字段：非 token 计量（搜索次数、图片张数、
// 视频秒数）会随供应商增加，写死成字段意味着每加一项都要改引擎。
// 用 map 的代价是拼错计量项名不会报错——`units["imges"]` 取到 0。
// 编译期无法拦住它（键是运行时才知道的字符串），所以在文档里写明，
// 并让规则作者在条件里显式给出期望范围。
func usageVars() map[string]any {
	return map[string]any{
		"p":         int64(0),
		"c":         int64(0),
		"cr":        int64(0),
		"cc":        int64(0),
		"reasoning": int64(0),
		"len":       int64(0),
		"units":     map[string]int64(nil),
	}
}

// reservedNames 是规则不能用作注入常量的名字。
//
// 与用量、时间、函数名一起构成「环境里已经有的名字」。注入常量撞上它们会被拒绝：
// 一个叫 p 的常量会让规则的含义随调用方的数据而变，而规则源码看起来完全没变。
var reservedNames = func() map[string]struct{} {
	out := make(map[string]struct{}, 24)
	for _, name := range usageVarNames {
		out[name] = struct{}{}
	}
	for _, name := range clockVarNames {
		out[name] = struct{}{}
	}
	for name := range funcSchemas {
		out[name] = struct{}{}
	}
	// __worst 是最坏情况求值注入的名字（见 worstPair）。它以环境值的形式
	// 提供给 expr，规则作者不该去写它，注入常量也不该占用它。
	out[worstIdent] = struct{}{}
	return out
}()

// funcSchemas 是提供给规则的函数签名。
//
// 它同时是**编译期环境的值**与运行期必须提供的名字集合：编译期用这里的签名
// 做类型检查，运行期由 runtime.env 给出真正带状态的闭包（见那里的说明）。
// 两处用同一份签名，是为了让「编译期通过、求值时才炸」没有发生的机会。
//
// 三个时间函数的分工见 runtime.env 的实现：
//
//	hour(tz)     时区只决定「现在几点」，由规则显式给出
//	isWorkday()  地区来自 Env.Region，日历与时区由 Env.Calendar 提供
//	isHoliday()  同上
//
// tier/fixed/minimum 的返回值不参与运算，而是为了让表达式写得下去：
// 它们是副作用通道，把「命中了哪一档」「每请求收多少」带出表达式，
// 金额本身仍由 Go 侧的定点运算算（见包注释）。
var funcSchemas = map[string]any{
	// 参数名写成 _ 而不是具名的：这几个函数体是被签名占位用的
	//（真正的实现通过环境值注入），具名参数会让读者以为函数体里
	// 用到了它们。
	// tier 返回 nil 是签名占位的一部分（真值由环境值注入），
	// 不是「出错了但没报」——所以这里显式 nolint 而不是造一个哨兵错误。
	"tier":    func(_ string, _ any) (any, error) { return nil, nil }, //nolint:nilnil // 见上
	"fixed":   func(_ float64) float64 { return 0 },
	"minimum": func(_ float64) float64 { return 0 },
	"hour": func(_ string) (int, error) {
		return 0, nil
	},
	"isWorkday": func() (bool, error) {
		return false, nil
	},
	"isHoliday": func() (bool, error) {
		return false, nil
	},
}

// priceSlots 是求值结果里允许出现的键，对应 billing.Prices 与 billing.Charges。
//
// 用下划线而不用驼峰，与 SQL/JSON 侧的字段名保持一致。单位一律是
// 「每位 token 多少元」和「每次请求多少元」，与 billing 的约定相同——
// 按千 token 标价的供应商在录入规则时换算；换算错在这边无从发现，
// 所以不做单位推断。
var priceSlots = map[string]struct{}{
	"prompt": {}, "completion": {}, "cache_read": {}, "cache_write": {},
	"reasoning": {}, "units": {}, "fixed": {}, "minimum": {},
}

// --- 运行时 ---

// runtime 是一次求值期间的可变状态。
//
// 它由 Eval 为**每一次求值**新建，所以这里的累加器不需要加锁。
// 编译产物（Program）本身不含任何可变状态——这是它能被并发复用的前提。
type runtime struct {
	at       time.Time
	region   string
	calendar Calendar
	// worstCase 为 true 时不做档位追踪。
	//
	// 最坏情况要把每个三元的所有分支都求值（见 worstPatcher），
	// 于是 tier() 会被调用不止一次，「命中了哪一档」这句话就没有答案了——
	// 报出来的是最后一次调用的那个名字，读的人会以为是真实的档位。
	// 与其给一个会误导的值，不如不给。
	worstCase bool

	// tierName 是最后一次 tier() 命中的档位名。
	//
	// 「最后一次」而不是「全部」：档位的语义是「这条规则最终把价格定在哪一档」。
	// 嵌套调用（内层先算、外层收尾）时只有外层代表最终结论，
	// 而账单要回答的正是「当时命中了哪一档」。
	tierName string

	fixedAmount   float64
	minimumAmount float64
}

// env 构造这一次求值传给 expr 的环境映射。
//
// # 为什么函数是闭包而不是注册进函数表
//
// tier/fixed/minimum 需要把结果写进本次求值的 runtime——同一个编译产物要能
// 被并发求值，而可变状态不能挂在它上面。expr 的 expr.Function 注册的函数是
// 编译进字节码的，一份编译产物只能配一组实现；把带状态的闭包放进去，
// 缓存里那份就固定在了第一次求值的状态上。**放进 Env** 则相反：
// 每次求值都新建一份闭包，编译产物保持无状态。这条是并发安全的关键，
// 改动这里的写法前先看包内那条 -race 测试。
//
// 所有函数都返回 error 而不是 panic：规则是可写的输入，算不出来要把原因
// 带回去，而不是把进程带走。expr 会把函数的 error 原样传出来。
func (rt *runtime) env(u usageView) map[string]any {
	return map[string]any{
		"p":         u.raw.PromptTokens,
		"c":         u.raw.CompletionTokens,
		"cr":        u.raw.CacheReadTokens,
		"cc":        u.raw.CacheWriteTokens,
		"reasoning": u.raw.ReasoningTokens,
		"len":       u.length,
		"units":     u.units,

		"at":     rt.at,
		"region": rt.region,

		"tier": func(name string, value any) (any, error) {
			if rt.worstCase {
				// 仍然校验参数，好让规则的写法保持一致；
				// 但不记录档位名。
				if name == "" {
					return nil, fmt.Errorf("tier 的档位名不能为空")
				}
				return value, nil
			}
			if name == "" {
				// 空档位名会让账单上出现「命中了某一档」却无名可查的记录，
				// 而对账恰恰要靠这个名字。宁可报错。
				return nil, fmt.Errorf("tier 的档位名不能为空")
			}
			rt.tierName = name
			return value, nil
		},
		// fixed/minimum 既写累加器又返回原值：返回是为了让表达式写得下去
		// （它们可以出现在任何数值位置），累加是为了让规则只用
		// `fixed(0.02)` 而不把它写进结果表时也能收到这笔钱。
		//
		// 最坏情况求值时取较大者而不是覆盖：所有分支都会被求值，
		// 覆盖会留下「最后算到的那一支」的值，而那一支未必是最贵的。
		"fixed": func(amount float64) float64 {
			rt.fixedAmount = rt.raise(rt.fixedAmount, amount)
			return amount
		},
		"minimum": func(amount float64) float64 {
			rt.minimumAmount = rt.raise(rt.minimumAmount, amount)
			return amount
		},
		"hour": func(tz string) (int, error) {
			// 只回答「现在几点」：时区是钟表问题，与地区日历无关，
			// 所以由规则显式给出，而不是从 Env.Region 推导。
			loc, err := loadLocation(tz)
			if err != nil {
				return 0, err
			}
			return rt.at.In(loc).Hour(), nil
		},
		"isWorkday": func() (bool, error) {
			info, err := rt.dayInfo()
			if err != nil {
				return false, err
			}
			return !info.OffDay, nil
		},
		"isHoliday": func() (bool, error) {
			info, err := rt.dayInfo()
			if err != nil {
				return false, err
			}
			// 「节假日」严格指放假且**有名字**的日子，不含普通周末。
			// 周末在绝大多数规则里是另一档（off_peak / weekend），
			// 把周末也算成节假日会让两条规则互相覆盖，
			// 而覆盖顺序只能靠写规则的人记住。
			return info.HolidayName != "", nil
		},

		// __worst 只在最坏情况求值时被引用（编译期由 worstPatcher 注入）。
		// 普通求值的字节码里不会有它，放进来是为了让两种求值共用一份环境，
		// 也让最坏情况那条路径不必单独构造环境——两份环境一旦漂移，
		// 表现就是「普通求值正常、预留的时候报 unknown name」。
		worstIdent: worstPair,
	}
}

// raise 按求值模式合并两次副作用写入的值。
//
// 普通求值下「最后一次写」是规则的真实语义（条件表达式只会走到一支）；
// 最坏情况求值下所有分支都会走到，此时必须取较大者——
// 否则一笔固定费的上界会变成「最后算到的那一支」的值，而那一支未必最贵。
func (rt *runtime) raise(old, fresh float64) float64 {
	if !rt.worstCase {
		return fresh
	}
	if math.IsNaN(old) {
		return fresh
	}
	if math.IsNaN(fresh) {
		return old
	}
	return math.Max(old, fresh)
}

// dayInfo 取 Env.At 在 Env.Region 下的日历日，再去查日历。
//
// 三件事都在 Go 侧完成，规则作者只写 isWorkday()：
//
//  1. 时区取自 Calendar.TimeZone(Region)——「当地几点」是地区属性；
//  2. 用该时区把 At 折成当地的年月日，做成一个零点的日期；
//  3. 查 Calendar.DayInfo(Region, 那一天)。
//
// 为什么地区与时区分开：它们回答不同的问题。地区决定「那边的日历怎么排」，
// 时区决定「现在几点」。让规则写 isWorkday("Asia/Shanghai") 会逼作者记住
// 「上海对应 CN」；让作者自己写地区则连「这个地区用哪个时区」都得他知道，
// 而那正是 Calendar 能回答的。
func (rt *runtime) dayInfo() (DayInfo, error) {
	if rt.calendar == nil {
		return DayInfo{}, fmt.Errorf("%w: 规则要用日历，但 Env.Calendar 为空", ErrCalendarRequired)
	}
	if rt.region == "" {
		return DayInfo{}, fmt.Errorf("%w: Env.Region 为空", ErrCalendarRequired)
	}
	tz, err := rt.calendar.TimeZone(rt.region)
	if err != nil {
		return DayInfo{}, err
	}
	loc, err := loadLocation(tz)
	if err != nil {
		return DayInfo{}, err
	}
	local := rt.at.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	return rt.calendar.DayInfo(rt.region, day)
}

// usageView 是用量在求值期间的形态。
type usageView struct {
	raw    billing.Usage
	length int64
	units  map[string]int64
}

// newUsageView 把计费用量折成求值用的形态。
func newUsageView(u billing.Usage) usageView {
	units := u.UnitCounts
	if units == nil {
		// 规则里 `units["img"]` 取不到键时给 0；而 nil map 上的成员访问
		// 在个别路径上会退化成 nil，让 `nil <= 3` 报一个与规则无关的错。
		// 统一成空 map，作者就不必考虑「这个计量项今天有没有出现」。
		units = map[string]int64{}
	}
	return usageView{raw: u, length: contextLength(u), units: units}
}

// contextLength 是规则里的 len：输入侧的上下文长度，含缓存读写。
//
// 判断「这段上下文属于哪个档」看的是请求送进去多少，而不是其中有多少要花钱。
// 缓存命中会让 PromptTokens 变小，用 p 判档会把一个 300K 上下文的请求
// 判成短上下文档——档位判对了，单价才谈得上对。
//
// 刻意不含输出与推理 token：它们不是「送进去的上下文」，
// 而分档的目的正是按上下文长度分。也不复用 Usage.TotalTokens——
// 那个口径不同，混用会让两个含义相近的名字给出不同的答案。
func contextLength(u billing.Usage) int64 {
	return u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// finite 判断一个浮点值可以安全地转成金额。
//
// NaN 与 Inf 必须在转 money.Amount 之前拦住：一旦流进金额，
// 比较（Capped、Max、求和）的行为就由 IEEE 决定而不是由业务决定，
// 「预留 ≥ 结算」那条不变量会静默失效。
func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
