package expr

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/billing"
)

// Env 是一次求值的全部外部输入。
//
// 它刻意是个纯数据结构：不持有数据库连接、不读环境变量、不读墙钟。
// 求值因此可以脱离数据库与网络测试，而「同一次请求在预留时与结算时得到
// 同一个答案」也才可能被证明——这是本包最要紧的一条不变量。
type Env struct {
	// Usage 是参与计费的用量。
	//
	// 它在规则里**只作条件**：`len <= 32000 ? A : B`。Compile 会拒绝把用量
	// 变量写进价格位置（`p * 2.5`），原因见包注释。
	Usage billing.Usage

	// Globals 是调用方注入的常量，例如套餐名、订阅档位、库名白名单。
	//
	// 规则源码来自数据库，没法引用 Go 里的值；这些量既不是用量也不是时间，
	// 于是单独一条通道。**名字与类型必须与编译时一致**（编译期按这里的样本
	// 值声明类型，值本身可以每次求值不同）。
	//
	// **目前没有生产入口：只有本包的测试在填它。** 生产里的求值环境
	// （internal/pipeline 的 env）只填 Usage / At / Region / Calendar，
	// 编译走 Compile，不声明任何注入常量。所以规则作者写 `plan == "pro"`
	// 会在编译期得到 unknown name——这条路没接上，不是「注入了空值」。
	//
	// 接线前要先回答三个问题：
	//
	//  1. 值从哪来。套餐名要先有套餐表，订阅档位要先有订阅记录，
	//     库名白名单要先有那份名单——这些都不是计价引擎能自己产生的。
	//  2. 谁在每次求值前把它们取出来，并保证名字与类型和编译期声明一致。
	//  3. 预留与结算拿到的是**同一份**值。两次求值之间值变了，同一个请求
	//     就会算出两个价，而账面差异看不出来（与 At 那条注释是同一个道理）。
	Globals map[string]any

	// At 是这次求值适用的时刻。**不允许读 time.Now()**：
	// 预留与结算跨整点时分别读到不同的钟点，就会按低谷预留、按高峰结算，
	// 撞上 Capped——而 Capped 本该是「上界算错了」的信号，不该成为常态。
	// 事后复算同理，没有求值时刻就无法重放。
	At time.Time

	// Region 是适用哪个地区的日历，例如 "CN"。
	//
	// 与 At 分开是因为它们是两件事：At 是「现在几点」，Region 是「那边的
	// 日历怎么排」。同一个时刻在上海与在纽约是不同的工作日；而某个地区的
	// 时区又是该地区的属性，不该由规则作者写死——它由 Calendar 提供。
	Region string

	// Calendar 提供工作日与节假日的查询。
	//
	// 抽成接口而不是直接查库，是为了让求值能脱离数据库测试，
	// 也让日历数据的来源（定时同步的表、缓存、离线快照）与计价解耦。
	// 规则用不到日历函数时可以是 nil。
	Calendar Calendar
}

// Calendar 查询某个地区的日历信息。
//
// 实现方通常把 calendar_days / calendar_years 两张表封在后面。
// 只读、无副作用；实现必须并发安全，因为求值会在多个 goroutine 上同时进行。
type Calendar interface {
	// TimeZone 返回该地区判断「当地日历日」用的时区名，例如 "Asia/Shanghai"。
	//
	// 放在这里而不是让规则写死：工作日与节假日是**地区**属性，
	// 而「当地日历日」要先知道时区才算得出来。规则作者不该也不需要知道时区。
	TimeZone(region string) (string, error)

	// DayInfo 返回该地区某个日历日的属性。
	//
	// day 是当天零点，表示「当地的那一天」——时区换算由调用方做完，
	// 实现方只按日期查表，不必关心时区。
	DayInfo(region string, day time.Time) (DayInfo, error)

	// Peak 判定某地区某个时刻算不算高峰，并带回判定所依据的那一天。
	//
	// windows 是这条规则认的高峰时段，和 hour(tz) 的时区一样由**规则**给出
	// （见 runtime.env 的 isPeak）：窗口决定「按哪一档计价」，是定价口径，
	// 所以写规则的人自己声明，而不是从 Region 推断——同一个地区的两个渠道
	// 可以有各自的峰谷。
	//
	// **判不出来时返回错误，不答 false**：与 DayInfo 同一条口径，那边是
	// 未知直接报错。答 false 会在春节期间按低谷价收费，而账单看上去完全正常。
	//
	// **那一天要一起带回来**（见 PeakVerdict）：窗口按当地零点起算，所以判峰谷
	// 本来就要先落到当地那一天、问出它是不是休息日——实现方答 Peak 时手上已经
	// 有这一份。它随判定回来之后，求值不必为了 Result.LocalTime 的两格再查一次
	// 日历（那两格落的是 usage_events.local_hour / is_off_day），而落库的值与
	// 这次判定也就成了同一个变量，不必靠「两次查询的入参相同」这个推论。
	//
	// 走这条路时实现方还要答得出来 TimeZone：那两格里的钟点按**当地**算，而
	// 时区是 Calendar 才知道的事（见 runtime.localClock）。**DayInfo 不必**——
	// 那一天已经随判定回来了；只有 isWorkday() / isHoliday() 才走 DayInfo。
	//
	// 休息日整天算不算高峰**不在这个接口上**：那是规则用 isWorkday() 组合
	// 出来的事（`!isWorkday() || isPeak("08:00-22:00")`），不必再多一个开关。
	Peak(region string, instant time.Time, windows []PeakWindow) (PeakVerdict, error)
}

// PeakVerdict 是一次峰谷判定的结果：判定本身，以及判定所依据的那一天。
//
// 那一天不是附赠的。判峰谷本来就要先落到当地那一天、问出它是不是休息日
// （见 internal/calendar 的 PeakAt），所以实现方手里已经有这一份；把它交出来
// 是为了让求值填 Result.LocalTime 时不必再问一次 DayInfo——落库的那两格于是
// 与这次判定出自同一个变量，而不是「两次查询问的是同一天」这个推论。
type PeakVerdict struct {
	// IsPeak 是判定：这一刻算不算高峰。
	IsPeak bool
	// Day 是判定所依据的那一天的日历属性——地区时区下 instant 落在的那一天，
	// 与求值自己折出来的那一天是同一个。
	Day DayInfo
}

// PeakWindow 是一段高峰时段：一天之内的 [StartMinute, EndMinute)，
// 从当地零点起算，左闭右开。
//
// 与 DayInfo 一样，它是 expr 侧的形状，实现方负责翻译成自己那一侧的类型
// （见 internal/billing/calendaradapter）。跨零点的段写成 EndMinute <
// StartMinute；两端相等表示全天。两种写法都要由实现方按同一套语义解释——
// 规则里写 `22:00-02:00` 的人指的是「当天 22:00 到次日 02:00」。
type PeakWindow struct {
	StartMinute int
	EndMinute   int
}

// DayInfo 是一个日历日的属性，字段与 calendar_days 表对应。
type DayInfo struct {
	// OffDay 为 true 表示休息日：周末，或按放假安排不上班的日子。
	OffDay bool
	// HolidayName 是节假日名（元旦、春节、国庆节……）。
	// 休息日未必有名字——普通周末就没有。所以「有名字」才等于法定节假日。
	HolidayName string
	// MakeupWorkday 为 true 表示调休补班日：安排里挪用周末上班的那天。
	// 它是**工作日**，不是休息日。
	MakeupWorkday bool
}

// ErrEnvIncomplete 表示 Env 缺少求值必需的信息。
//
// 与编译错误分开：那类是规则写错了，这类是调用方没把信息给全。
// 处置方式不同——前者要让商家改规则，后者要改代码。
var ErrEnvIncomplete = errors.New("求值的输入不完整")

// ErrGlobalsInvalid 表示注入常量非法：名字与保留名冲突，或类型不受支持。
var ErrGlobalsInvalid = errors.New("注入常量非法")

// ErrCalendarRequired 表示规则要用日历，但 Env 没给 Calendar，或日历查不到数据。
//
// 高峰窗口的**写法**错不是它（那一类见 ErrPeakWindowSyntax）：这里说的是
// 日历这一头不可用，处置在部署与数据那一侧；写法错要改的是规则自己那个串。
var ErrCalendarRequired = errors.New("日历不可用")

// validate 检查求值必需的外部输入是否齐备。
//
// 缺什么就报什么，不用零值顶上：零值会让「没传 At」表现为
// 「按公元 1 年 1 月 1 日计价」，算出一个看不出错的价。
func (e Env) validate() error {
	if e.At.IsZero() {
		return fmt.Errorf("%w: Env.At 必须给出求值时刻", ErrEnvIncomplete)
	}
	if err := validateUsage(e.Usage); err != nil {
		return err
	}
	for name, v := range e.Globals {
		if _, err := globalType(name, v); err != nil {
			return err
		}
	}
	return nil
}

// validateUsage 检查用量。
//
// 只拦负值：负数用量会让条件翻转，进而选出另一个价——而它不会报错，
// 只会在账目上留下笔说不清来历的金额。
func validateUsage(u billing.Usage) error {
	if u.PromptTokens < 0 || u.CompletionTokens < 0 || u.CacheReadTokens < 0 ||
		u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 {
		return fmt.Errorf("%w: 用量为负", ErrEnvIncomplete)
	}
	for name, n := range u.UnitCounts {
		if n < 0 {
			return fmt.Errorf("%w: 计量项 %q 的数量为负: %d", ErrEnvIncomplete, name, n)
		}
	}
	return nil
}

// globalType 校验一个注入常量，并返回它应当被声明成的编译期类型。
//
// 类型故意只有几种标量：声明成 any 会让 `tier <= 3` 这种明显写错的表达式
// 也编得过，规格错误被推迟到求值时才炸。按真实类型声明，写错在编译期就报。
func globalType(name string, v any) (any, error) {
	if _, reserved := reservedNames[name]; reserved {
		// 注入常量盖住用量变量（例如一个叫 p 的常量）会让规则的含义
		// 随调用方的数据而变，而规则源码看起来没变。这种歧义没有哪种
		// 静默处置是安全的，直接拒绝。
		return nil, fmt.Errorf("%w: 注入常量 %q 与保留名冲突", ErrGlobalsInvalid, name)
	}
	switch t := v.(type) {
	case string:
		return "", nil
	case int, int64:
		return int64(0), nil
	case float64:
		return float64(0), nil
	case bool:
		return false, nil
	case []string:
		return []string(nil), nil
	case time.Time:
		return time.Time{}, nil
	default:
		return nil, fmt.Errorf("%w: 注入常量 %q 的类型 %T 不受支持", ErrGlobalsInvalid, name, t)
	}
}

// tzCache 缓存已解析的时区。
//
// time.LoadLocation 内部有锁，而 hour(tz) 在计价热路径上每个请求都会调。
// 这里用一个无锁的 map 挡在前面；值是只读的 *time.Location，所以并发安全。
var tzCache sync.Map

// loadLocation 解析时区名，失败时报出是哪个时区。
//
// 容器镜像里常常没装 tzdata，`Asia/Shanghai` 会解析失败。这时必须报错而不是
// 退回 UTC：退回 UTC 会让高峰档与低谷档整体错位八小时，账目上完全看不出来。
func loadLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return nil, fmt.Errorf("%w: 时区名为空", ErrCalendarRequired)
	}
	if v, ok := tzCache.Load(tz); ok {
		return v.(*time.Location), nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法解析时区 %q（镜像是否装了 tzdata？）", ErrCalendarRequired, tz)
	}
	tzCache.Store(tz, loc)
	return loc, nil
}
