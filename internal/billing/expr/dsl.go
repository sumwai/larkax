package expr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
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
// 时间函数的取数方式见 runtime.env 的实现：
//
//	hour(tz)             时区只决定「现在几点」，由规则显式给出
//	isWorkday()          地区来自 Env.Region，日历与时区由 Env.Calendar 提供
//	isHoliday()          同上
//	isPeak("08:00-22:00")  同上，另外**高峰时段也由规则给出**：
//	                     峰谷是定价口径，与 hour 的时区同一类，所以不由
//	                     Region 推断——同一地区的两个渠道可以有各自的峰谷
//
// tier/fixed/minimum 除了走副作用通道，**返回值也照常参与表达式**：
// 三者都把入参原样返回——tier 返回规则作者在调用里给的那张表（它只记下
// 档位名，不替作者选表），fixed/minimum 返回原值。副作用通道把「命中了
// 哪一档」「每请求收多少」带出表达式，金额本身仍由 Go 侧的定点运算算
// （见包注释）。
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
	"isPeak": func(_ string) (bool, error) {
		return false, nil
	},
}

// nonTokenPriceSlots 是不在 billing.Usage 里的结果键：计量项表与两项一次性费用。
//
// 它们只在这里列一份，因为它们不属于「token 槽位」这个事实——
// 加一项 token 槽位时不该需要改这里，反过来也不该。
var nonTokenPriceSlots = []string{"units", "fixed", "minimum"}

// priceSlots 是求值结果里允许出现的键，对应 billing.Prices 与 billing.Charges。
//
// token 槽位从 billing.TokenSlots 派生，不在这里再抄一份：那份表是
// 「有哪些 token 槽位、它们叫什么」的唯一来源。手抄一份的话，加一个槽位时
// 漏改这里不会编译失败，规则作者写新键会得到一句「未知的键」，
// 而引擎看起来一切正常。
//
// 用下划线而不用驼峰，与 SQL/JSON 侧的字段名保持一致。单位一律是
// 「每位 token 多少元」和「每次请求多少元」，与 billing 的约定相同——
// 按千 token 标价的供应商在录入规则时换算；换算错在这边无从发现，
// 所以不做单位推断。
var priceSlots = func() map[string]struct{} {
	out := make(map[string]struct{}, len(billing.TokenSlots)+len(nonTokenPriceSlots))
	for _, slot := range billing.TokenSlots {
		out[slot.Name] = struct{}{}
	}
	for _, name := range nonTokenPriceSlots {
		out[name] = struct{}{}
	}
	return out
}()

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
		"isPeak": func(peakWindows string) (bool, error) {
			// 窗口、地区、时刻三件事一起交给日历：窗口由**规则**给出，
			// 地区与时刻来自 Env（见 Env.Calendar 的 Peak）。
			//
			// 判不出来的那一天由实现方报错，这里既不接住那个错误、
			// 也不拿 false 顶上去：false 是一个具体的判断（「按低谷算」），
			// 用它代理未知会在春节期间按低谷价收费，而账单上完全看不出来。
			if err := rt.requireCalendar(); err != nil {
				return false, err
			}
			windows, err := ParsePeakWindows(peakWindows)
			if err != nil {
				// 写法错原样往上抛，不裹 ErrCalendarRequired——与编译期那一处
				// 同一条口径（见 ParsePeakWindows 的「两个调用方」一节）。
				//
				// 这一处**今天到不了**：窗口必须是字面量，而字面量在编译期
				// 已经解析过一遍（见 compile.go 的 isPeak 那一支），所以能跑到
				// 这里的串都是合法的。留着它是因为求值不只编译期一条入口，
				// 而万一真到了这里，报「写法不成立」比报「日历不可用」更贴。
				return false, err
			}
			return rt.calendar.Peak(rt.region, rt.at, windows)
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
	if err := rt.requireCalendar(); err != nil {
		return DayInfo{}, err
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

// requireCalendar 检查规则要用日历时的两个前提。
//
// 两处都查而不是让实现方去报：一条规则里漏了地区或日历，报出来的那句话
// 要说清缺的是哪一样，而实现方拿到的只是「地区为空」这个事实。
func (rt *runtime) requireCalendar() error {
	if rt.calendar == nil {
		return fmt.Errorf("%w: 规则要用日历，但 Env.Calendar 为空", ErrCalendarRequired)
	}
	if rt.region == "" {
		return fmt.Errorf("%w: Env.Region 为空", ErrCalendarRequired)
	}
	return nil
}

// ErrPeakWindowSyntax 表示高峰窗口串的**写法**不对。
//
// 它与 ErrCalendarRequired 分开：写法是调用方给的那个串本身有问题
// （`"25:00-26:00"`、`"8:5-9:00"`），处置是改那个串；ErrCalendarRequired 是
// 日历这一头没接上、或者那一天判不出来，处置在部署与数据那一侧。两者的处置
// 不同，所以规则那两条路上都不再把写法错裹成 ErrCalendarRequired——裹上去的
// 后果是写规则的人看到的第一句是「日历不可用」，矛头指向数据那一侧，而他要改
// 的其实是自己写的那一串。
//
// 插件宿主那一侧也要按这个分寸分类：插件写错窗口是 bad_argument，该改的是插件
// 自己的代码（见 internal/plugin/host 的 peakFn）。
var ErrPeakWindowSyntax = errors.New("高峰时段写法不成立")

// PeakWindowSyntaxError 是写法不成立时的错误，带一句照着能改的原因。
//
// 做成类型而不是只用 sentinel：那句原因要单独拿出来用——插件宿主把它放进
// bad_argument 的文案里（见 internal/plugin/host 的 checkPeakWindow），而它
// 不该连着「高峰时段写法不成立」这个分类前缀一起说给插件听；规则那一侧照旧
// 整条打印。取原因用 errors.As，不切字符串。
type PeakWindowSyntaxError struct {
	Reason string
}

func (e *PeakWindowSyntaxError) Error() string {
	return ErrPeakWindowSyntax.Error() + ": " + e.Reason
}

// Is 让分类认得出这个错误：errors.Is(err, ErrPeakWindowSyntax) 成立，
// 认分类的调用方不必知道原因那一截长什么样。
func (e *PeakWindowSyntaxError) Is(target error) bool { return target == ErrPeakWindowSyntax }

// ParsePeakWindows 把高峰时段串折成时段表。
//
// 写法是 `HH:MM-HH:MM`，多段用逗号分开（`"08:00-12:00,14:00-18:00"`），
// 段内允许跨零点（`"22:00-02:00"`，见 PeakWindow）。
//
// **用 HH:MM 而不是分钟数**：规则是给人写的，`"08:00-22:00"` 与上游那张写着
// 「高峰 8:00–22:00」的表长得一样，抄错的机会最小。
//
// 这里判的是**写法**：几段、每段两头是不是一个合法的时钟时刻，以及全天段
// 不许与其他段并列。段与段重叠是允许的（见 PeakSchedule）。写法不对时包的是
// ErrPeakWindowSyntax。
//
// # 两个调用方，约定一件事
//
// 规则那一条路（求值里的 `isPeak` 与保存规则时的编译期检查）与插件宿主那一条
// 路（见 internal/plugin/host 的 peakFn）都调本函数，**写法错原样往上报**：
// 本函数给出的错已经说清是哪个串、哪里不成（`高峰时段写法不成立: …`），调用方
// 再裹一层只会把这句话推到第二句去。规则那两侧因此都不包 ErrCalendarRequired，
// 插件那一侧按 bad_argument 分类。导出也是为了这两条路共用同一份解析——两侧
// 各写一份的话，规则里存得进去的写法与插件里被接受的写法会慢慢分叉，而分叉的
// 表现是同一句话在一处算高峰、在另一处算低谷。
//
// # 全天段并列为什么在这里判，而不留给日历
//
// 那一条本来只由 `calendar.PeakSchedule.Validate` 判，而它**只有求值那一步
// 会走到**——预留那一步走的是 `EvalWorstCase`，它把三元条件整个换成
// `__worst(真, 假)`（见 worst.go 的 worstPatcher），`isPeak` 在预留时根本
// 不被求值。于是这种规则会「存得进库、预留成功、结算时才炸」，而流式那条路
// 上那一炸只留一行日志、随后把预扣释放掉：客户端正常收完，一分钱不收。
// 写错了的规则因此要在**保存时**就挡下来，那时判它只需要窗口表本身，不查日历。
//
// 日历那边那条校验留着：它的入口不只规则一条（插件适配器与测试都直接构造
// 排班），少判一次换不来什么。
func ParsePeakWindows(s string) ([]PeakWindow, error) {
	segments := strings.Split(s, ",")
	out := make([]PeakWindow, 0, len(segments))
	for i, segment := range segments {
		segment = strings.TrimSpace(segment)
		start, end, ok := strings.Cut(segment, "-")
		if !ok {
			return nil, &PeakWindowSyntaxError{
				Reason: fmt.Sprintf("高峰时段 %q 应当写成「08:00-22:00」", segment)}
		}
		startMinute, err := parseClock(strings.TrimSpace(start))
		if err != nil {
			return nil, err
		}
		endMinute, err := parseClock(strings.TrimSpace(end))
		if err != nil {
			return nil, err
		}
		// 两端相等是「跨满一整天」（见 PeakWindow），而全天段与其他段并列
		// 不成立：并列的那几段完全被盖住，写的人多半是把「全天」当成了
		// 段之间的分隔符，或者只是想写一个 24 小时的窗口。
		if len(segments) > 1 && startMinute == endMinute {
			return nil, &PeakWindowSyntaxError{Reason: fmt.Sprintf(
				"高峰时段里的第 %d 段 %q 是全天时段，不能与其他段并列"+
					"（整天算高峰不必用窗口：把那条价格写成不带窗口的形式即可）",
				i+1, segment)}
		}
		out = append(out, PeakWindow{StartMinute: startMinute, EndMinute: endMinute})
	}
	return out, nil
}

// parseClock 把 HH:MM 折成从当天零点起算的分钟数。
//
// 这里认的是钟表的写法：0–23 时、0–59 分，所以 `"25:00"` 与 `"08:60"` 都在
// 这里被挡住，不会变成一个别的时刻。日历那边也校验一遍自己的取值范围——
// 它的入口不只规则一条。
func parseClock(s string) (int, error) {
	hour, minute, ok := strings.Cut(s, ":")
	if !ok {
		return 0, &PeakWindowSyntaxError{Reason: fmt.Sprintf("高峰时段里的时刻 %q 应当写成「08:00」", s)}
	}
	h, err := strconv.Atoi(hour)
	if err != nil {
		return 0, &PeakWindowSyntaxError{Reason: fmt.Sprintf("高峰时段里的时刻 %q 读不出小时", s)}
	}
	// 分必须是两位：`8:5` 读不出作者想写的是 08:05 还是别的东西。
	if len(minute) != 2 {
		return 0, &PeakWindowSyntaxError{Reason: fmt.Sprintf("高峰时段里的时刻 %q 的分钟要写成两位（08:05）", s)}
	}
	m, err := strconv.Atoi(minute)
	if err != nil {
		return 0, &PeakWindowSyntaxError{Reason: fmt.Sprintf("高峰时段里的时刻 %q 读不出分钟", s)}
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, &PeakWindowSyntaxError{Reason: fmt.Sprintf("高峰时段里的时刻 %q 不是一天之内的钟点", s)}
	}
	return h*60 + m, nil
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
