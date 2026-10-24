// Package calendar 回答分时计价依赖的日期问题：某一天是工作日还是休息日，
// 某个时刻属于高峰还是低谷。
//
// # 为什么单独一层
//
// 判定依据来自外部数据（放假安排），而这份数据有「尚未公布」这种状态。
// 把它与计价公式分开，是为了让「这一天到底算不算工作日」能脱离数据库单独
// 测试——它是整条计价链路上最容易被悄悄搞错的一环。
//
// # 查不到就是未知，不是工作日
//
// 数据缺失时返回**未知**，而不是回落到「普通工作日」。未来年份的放假安排
// 要到前一年年底才公布，在那之前查不到是正常状态；而把它当成工作日会让
// 春节、国庆按工作日计价，并且**不会报错**——账目只是慢慢地偏掉。
// 调用方拿到未知后可以自行决定保守策略（按低谷计价、按更贵的一档预留、
// 或拒绝本次请求），但那个决定必须由调用方做出并留下痕迹，不由本包代劳。
//
// # 时区必须显式
//
// 所有判定都在地区时区下进行，不依赖进程的本地时区：生产容器通常跑 UTC，
// 而一个 UTC 时刻换算到北京很可能已经跨过午夜，同一瞬间在两处会落进不同的
// 日期、甚至不同的一周。因此每个入口都要地区代码，日期一律按地区时区切分。
//
// # 峰谷这一支两条路都通了，窗口都随调用走
//
// 上面两句里，「某个时刻属于高峰还是低谷」这一半今天有两条路，两条都通：
//
//   - **规则里**：`isPeak("08:00-22:00")` 把高峰时段写在规则自己身上，与
//     `hour(tz)` 的时区同一类——峰谷是定价口径，不由地区推断（同一地区的两个
//     渠道可以有各自的峰谷）。窗口与地区、时刻一起交给本包判定，见
//     internal/billing/expr 的 Peak 与 internal/billing/calendaradapter。
//   - **插件里**：`calendar.peak(ts, region, "08:00-22:00")` 把窗口写在调用
//     上，写法与规则里那个串同形（两侧共用 expr.ParsePeakWindows，见
//     internal/plugin/host 的 peakFn）。于是插件也写得出峰谷价。
//
// 「高峰窗口从哪来」两边是同一个答案：**不从全局设置或渠道配置里隐式取**，
// 窗口随调用走——规则写在规则里，插件写在调用里。
//
// 窗口随调用走是**同一条判定的两个入口**，不是两份实现：解析在 expr，判定在
// 本包的 PeakAt，两侧都不另写一遍。
//
// 这件事**工具不会再提醒**：`PeakAt` 挂在 `*Calendar` 这个类型上，而
// #182 把 `*Calendar` 接进 assembly 之后，这个类型进了可达图，`deadcode`
// 就把它的方法都算成可达，于是 peak.go 里那几条**从不可达清单里消失了**。
// 清单里没有它，读起来像「已经在用了」。见 LESSONS.md 第 22 条。
// 适配器那一侧的提醒也已经兑现掉了（`pluginCalendar.Peak` 从
// scripts/deadmethods-allowlist.txt 里删掉了，它现在真的被插件调到）。
//
// # 边界
//
// 本包不查数据库也不发 HTTP：数据由调用方通过 Source 注入。这样判定逻辑
// 可以脱离存储单独测试，而「查不到时怎么办」也不会被埋在 SQL 里。
package calendar

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Region 是日历数据的地区代码，取值与 calendar_days.region 一致。
// settings 里的 calendar.default_region 存的也是它——那个值是默认值，
// 调用方仍可逐次传入不同地区，所以本包不收下它当构造参数。
type Region string

// RegionCN 是中国大陆。目前只有这一个地区有公开的数据源。
const RegionCN Region = "CN"

// regionZones 把地区映射到 IANA 时区名。
//
// 只登记有数据源的地区：把一个查不到数据的地区放进来，会让调用方以为判定
// 有效，而结果显示「未知」——那与「这个地区不支持」是两回事，前者会让人
// 一直排查数据同步，后者一眼就知道要加数据源。
var regionZones = map[Region]string{
	RegionCN: "Asia/Shanghai",
}

var (
	// ErrUnknownRegion 表示地区代码没有登记。
	ErrUnknownRegion = errors.New("未知的日历地区")
	// ErrTimezoneUnavailable 表示时区数据加载失败，通常是容器里没有 tzdata。
	//
	// 这种情况不要退化成固定偏移或 UTC：那样在时区规则变更的年份会悄悄差
	// 一小时，而错误只在少数日期的边界上出现，比直接报错难查得多。
	ErrTimezoneUnavailable = errors.New("时区数据不可用")
	// ErrInvalidDate 表示日期本身不成立（例如 2 月 30 日）。
	ErrInvalidDate = errors.New("日期不成立")
	// ErrNilSource 表示没有提供数据源。
	ErrNilSource = errors.New("日历数据源为空")
)

var (
	locMu    sync.Mutex
	locCache = make(map[Region]*time.Location)
)

// Timezone 返回地区对应的时区。未知地区返回 ErrUnknownRegion。
//
// 结果会缓存：time.LoadLocation 每次都要读时区文件并解析，而这里会被
// 每个请求调到。缓存的是 *time.Location，可以安全共享。
func Timezone(region Region) (*time.Location, error) {
	locMu.Lock()
	defer locMu.Unlock()
	if loc, ok := locCache[region]; ok {
		return loc, nil
	}

	name, ok := regionZones[region]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRegion, region)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// 用 %w 包住底层错误而不是 %v：调用方可能想分辨「时区名不存在」
		// 与「整个时区数据库缺失」——前者是配置错，后者是镜像少装东西，
		// 两者的处置完全不同。
		return nil, fmt.Errorf("%w: 加载 %s（地区 %s）失败: %w；容器内缺时区文件时可在主程序 import _ %q",
			ErrTimezoneUnavailable, name, region, err, "time/tzdata")
	}
	locCache[region] = loc
	return loc, nil
}

// Date 是一个不带时刻的日历日期。
//
// 用独立类型而不是 time.Time：time.Time 同时带着时刻与时区，比较两个
// 「日期」时很容易拿一个带时刻的值去比，结果因为差几个小时判成相邻两天。
// 这里没有时刻可带，两个 Date 用 == 比较就是在问同一件事。
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// Valid 判断这个日期在公历里成立。
//
// 需要单独判断，是因为 Go 的 time.Date 会把 2 月 30 日**规范化**成 3 月 2 日：
// 不成立的日期不会在构造时报错，只会在比较时表现得像另一天。判定入口据此
// 拒绝非法日期，而不是替调用方猜一个。
func (d Date) Valid() bool {
	if d.Year < 1 || d.Year > 9999 {
		return false
	}
	if d.Month < time.January || d.Month > time.December {
		return false
	}
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).Day() == d.Day
}

// Weekday 返回星期几。日期不成立时返回值没有意义，调用方先查 Valid。
//
// 用 UTC 构造是安全的：这里只问「这个公历日期是星期几」，
// 与钟表时刻和时区都无关。
func (d Date) Weekday() time.Weekday {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC).Weekday()
}

// IsWeekend 判断是否周六或周日。
//
// 它是纯星期规则，不看任何数据。数据缺失时调用方可以拿它实现回退
// （例如「公告未发布时按周末低谷」），但那属于调用方的策略——
// 本包不会把这条规则悄悄当成判定结果，因为调休补班恰恰能推翻它。
//
// 目前只有测试在调它：还没有哪一处按周末做回退（PeakAt 明确不套用这条规则，
// 见 peak.go）。留着是为了让回退策略有现成的判据可用，而不是各自重写一遍。
func (d Date) IsWeekend() bool {
	wd := d.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

// String 输出 YYYY-MM-DD。
func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
}

// cmp 比较两个日期的先后，返回 -1 / 0 / 1。
//
// 不用时刻相减：那要绕开时区与夏令时，而这里问的纯粹是公历日期的先后。
//
// 目前只有 MemorySource 在调它，而那个类型是测试夹具（见 memory.go）——
// 所以这一条也只被测试走到。生产里比较日期用的是 Date 的取值本身。
func (d Date) cmp(o Date) int {
	switch {
	case d.Year != o.Year:
		return sign(d.Year - o.Year)
	case d.Month != o.Month:
		return sign(int(d.Month) - int(o.Month))
	case d.Day != o.Day:
		return sign(d.Day - o.Day)
	}
	return 0
}

// sign 把差值的符号取出来。
//
// 只被 Date.cmp 调用，所以与它同一条处境：目前只有测试走到（见 cmp）。
func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

// DateOf 返回 t 在**它自己的时区**下的日历日期。
//
// 想按某个地区切分日期，先把时刻换算过去（Calendar.At 就是这么做的）：
// 传一个 UTC 时刻进来得到的就是 UTC 的日期，与北京的日期可能差一天。
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}
}

// DayKind 是一天的三态判定。
type DayKind string

const (
	// DayWorkday 是工作日：周一到周五，以及被调休挪来上班的周末。
	DayWorkday DayKind = "workday"
	// DayRestDay 是休息日：周末，以及落在周一到周五的法定节假日。
	DayRestDay DayKind = "rest_day"
	// DayUnknown 是查不出来。它既不是工作日也不是休息日。
	DayUnknown DayKind = "unknown"
)

// Known 判断这个判定是不是一个确定的结论。
//
// 单独提供它，是为了不让「未知」落进某个兜底分支：写成
// `if kind == DayWorkday { ... } else { ... }` 时，未知会安静地落进
// 休息日那一支，而两侧的价差不会报错。
func (k DayKind) Known() bool { return k == DayWorkday || k == DayRestDay }

// YearState 是某一年数据的同步状态，取值与 calendar_years.status 一致。
//
// 零值取 YearUnknown，故意让它与任何一个真实状态都不同：数据源忘了设置时
// 查出来是「不知道」，而不是被当成「已同步」。
type YearState string

const (
	// YearUnknown 是库里没有这一年的记录，从未同步过。
	YearUnknown YearState = ""
	// YearSynced 表示该年数据已同步且双源一致，年内每一天都应当有记录。
	YearSynced YearState = "synced"
	// YearPending 表示该年放假安排尚未公布。对未来的年份这是**正常状态**，
	// 不是故障——安排要到前一年年底才发布。
	YearPending YearState = "pending"
	// YearConflict 表示两个数据源不一致，待人工裁决。
	YearConflict YearState = "conflict"
	// YearDegraded 表示安排已公布但同步失败——这才是需要告警的状态。
	YearDegraded YearState = "degraded"
)

// UnknownReason 说明为什么查不到。
//
// 区分它们，是因为处置方式完全不同：YearPending 是正常的（调用方按保守
// 策略继续），而 degraded 与 conflict 需要有人去看一眼。只告诉调用方
// 「未知」的话，这两类就混在一起了。
type UnknownReason string

// 未知的成因。分开记是因为**出路不同**：year_pending 是正常的
// （公告还没发布），其余几种都要人去查。
const (
	// ReasonYearPending 表示该年公告尚未公布。这是正常状态，不该告警。
	ReasonYearPending UnknownReason = "year_pending"
	// ReasonYearDegraded 表示公告已公布但同步失败。
	ReasonYearDegraded UnknownReason = "year_degraded"
	// ReasonYearConflict 表示两个数据源不一致。
	ReasonYearConflict UnknownReason = "year_conflict"
	// ReasonYearUnknown 表示库里没有该年的记录。
	ReasonYearUnknown UnknownReason = "year_unknown"
	// ReasonRowMissing 表示年份标着已同步，这一天却没有记录。
	// 这是数据不完整：按普通工作日处理会让节假日按工作日计价，
	// 所以仍然算未知。
	ReasonRowMissing UnknownReason = "row_missing"
	// ReasonRowInconsistent 表示记录本身自相矛盾（既放假又标成补班日）。
	// 猜任何一边都可能让一整天的价格算错，所以判定为未知。
	ReasonRowInconsistent UnknownReason = "row_inconsistent"
)

// DayRow 是 calendar_days 表的一行。
type DayRow struct {
	Date Date
	// Name 是节假日名称（「春节」「国庆节」），普通日子为空。
	// 判定用不到它，但账单要能解释「为什么这天算高峰」。
	Name string
	// OffDay 对应 is_off_day：这一天是否放假。
	OffDay bool
	// MakeupWorkday 对应 is_makeup_workday：这一天是调休补班日，
	// 即把周末挪来上班。它必然与 OffDay 为 false 的日子配对。
	// 数据源不一定直接给这一列（holiday-cn 只给「放不放假」），由导入方按
	// 「不是假期、且该日是周六或周日」推断；本包的判定不替调用方推断。
	MakeupWorkday bool
}

// Source 提供日历数据。抽成接口是为了让判定逻辑脱离数据库测试——
// 出错的分支（未来年份、数据缺失、记录自相矛盾）恰好是最需要覆盖的部分，
// 而它们在真库上很难构造。
//
// 实现方约定：
//
//   - 查不到不是错误，exists 返回 false 即可；
//   - 没有年份记录时 YearState 返回 YearUnknown；
//   - 不要用「返回一条 OffDay 为 false 的默认行」来表示查不到，
//     那正是本包要防的静默兜底，而且会绕过 Reason 的分支。
type Source interface {
	// LookupDay 查某地区某天的数据行。
	LookupDay(ctx context.Context, region Region, day Date) (row DayRow, exists bool, err error)
	// YearState 查某地区某年的同步状态。
	YearState(ctx context.Context, region Region, year int) (YearState, error)
}

// Calendar 用给定数据源回答日期判定。
type Calendar struct {
	src Source
}

// New 构造一个 Calendar。
func New(src Source) (*Calendar, error) {
	if src == nil {
		return nil, ErrNilSource
	}
	return &Calendar{src: src}, nil
}

// DayStatus 是一次日期判定的结果。
type DayStatus struct {
	// Date 是判定所依据的**当地**日期，未必等于调用方手里那个时刻的 UTC 日期。
	// 带上它是为了让「按 UTC 取日期」这类错误在日志里一眼可见。
	Date Date
	// Region 是判定所用的地区。
	Region Region
	// Kind 是三态判定。未知时不要把它当成工作日的默认值。
	Kind DayKind
	// Name 是节假日名称，仅在该日有数据且属于节假日时非空。
	Name string
	// MakeupWorkday 为 true 表示这是调休补班日（挪来上班的周末）。
	MakeupWorkday bool
	// Reason 只在 Kind 为 DayUnknown 时有值。
	Reason UnknownReason
}

// Lookup 判定某地区某一天是工作日还是休息日。
//
// 数据行的优先级最高：只要这一天有记录就以它为准。这样跨年的假期
// （例如元旦的安排里把去年 12 月 31 日挪成休息）也能正确判定，
// 不必假设「一条记录一定出自同一年的数据集」。
func (c *Calendar) Lookup(ctx context.Context, region Region, day Date) (DayStatus, error) {
	// 先校验地区与日期，再碰数据源：参数写错时不该产生一次查询。
	if _, err := Timezone(region); err != nil {
		return DayStatus{}, err
	}
	if !day.Valid() {
		return DayStatus{}, fmt.Errorf("%w: %s", ErrInvalidDate, day)
	}

	row, exists, err := c.src.LookupDay(ctx, region, day)
	if err != nil {
		return DayStatus{}, fmt.Errorf("读取 %s %s 的日历数据失败: %w", region, day, err)
	}
	st := DayStatus{Date: day, Region: region}
	if exists {
		return st.withRow(row), nil
	}

	state, err := c.src.YearState(ctx, region, day.Year)
	if err != nil {
		return DayStatus{}, fmt.Errorf("读取 %s %d 年的日历状态失败: %w", region, day.Year, err)
	}
	st.Kind = DayUnknown
	st.Reason = reasonOf(state)
	return st, nil
}

// withRow 把一行数据翻成判定结果。
func (st DayStatus) withRow(row DayRow) DayStatus {
	st.Name = row.Name
	st.MakeupWorkday = row.MakeupWorkday
	switch {
	case row.OffDay && row.MakeupWorkday:
		// 既放假又标成补班日，两条记录互相矛盾。这是数据错误，
		// 判定为未知并保留 Reason，让调用方有机会发现它。
		st.Kind = DayUnknown
		st.Reason = ReasonRowInconsistent
	case row.OffDay:
		st.Kind = DayRestDay
	default:
		// 非休息日一律是工作日。调休补班的周末正是走这一支：
		// 导入方把「不是假期、又落在周末」的日子记成 isOffDay=false，
		// 所以「周末就算休息日」这条纯星期规则在这里必须被数据压过。
		st.Kind = DayWorkday
	}
	return st
}

// reasonOf 把年份状态翻成「为什么查不到」。
func reasonOf(state YearState) UnknownReason {
	switch state {
	case YearSynced:
		return ReasonRowMissing
	case YearPending:
		return ReasonYearPending
	case YearDegraded:
		return ReasonYearDegraded
	case YearConflict:
		return ReasonYearConflict
	default:
		return ReasonYearUnknown
	}
}

// At 判定 instant 落在哪一天，以及那一天是工作日还是休息日。
//
// 时刻先换算到地区时区再取日期：这就是「不依赖进程本地时区」的落点。
// 换算之后的当地日期可能与时刻自带的 UTC 日期相差一天甚至一年，
// 那是预期行为——账单按当地的那一天计价。
//
// 今天有调用方：插件宿主那一条（internal/assembly 的 pluginCalendar.Day）——
// 插件传进来的是一个时间戳，折算时区这一步得有人替它做。需要「按时刻判定」的
// 另外两处各自做了那两步：计价引擎在自己的运行时里换算时区再把日期交给适配器
// （见 internal/billing/expr 的 dayInfo），PeakAt 也自己换算了一遍（见 peak.go）。
func (c *Calendar) At(ctx context.Context, region Region, instant time.Time) (DayStatus, error) {
	loc, err := Timezone(region)
	if err != nil {
		return DayStatus{}, err
	}
	return c.Lookup(ctx, region, DateOf(instant.In(loc)))
}
