package calendar

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidPeakWindow 表示高峰时段配置不成立。
var ErrInvalidPeakWindow = errors.New("高峰时段配置不成立")

// 本文件的一切**今天接不进任何地方**：规则能调用的函数里没有一个走 PeakAt，
// 所以峰谷价在规则里写不出来；插件那一侧也没有对应函数可调（宿主日历接口里
// 有 Peak，而 SDK 的 calendar 命名空间里没有它）。要接上它得先定「高峰窗口
// 从哪来」。
//
// 唯一的调用点是插件适配器里那个调不到的入口（见 internal/assembly 的
// pluginCalendar.Peak）——`PeakAt` 因此**不在** deadmethods 的清单里：那道门
// 按调用语法数，而它确实有一个调用点。提醒在哪，见包注释那一节。
//
// 这一支不会再被 deadcode 提醒——*Calendar 进了可达图，它的方法就都算可达，
// 于是下面这几条从不可达清单里掉了出去（见包注释与 LESSONS.md 第 22 条）。
// 改这一块、或者要往规则里加峰谷价时，先读包注释那一节。

// PeakWindow 是一段高峰时段：一天之内的 [Start, End)。
//
// 结束时刻不包含在内，所以 [08:00, 10:00) 覆盖到 09:59:59.999…。
// 紧邻的两段（[08:00, 10:00) 与 [10:00, 12:00)）因此不会在 10:00 重叠，
// 也不会留下没人认领的瞬间。
//
// 用分钟而不是 time.Duration 或「小时」表示：上游的时段切分常常落在半点
// 上（例如 08:30 起），只按小时配置表达不了；而 Duration 容易被误当成
// 「从某个时刻起的偏移」。
type PeakWindow struct {
	StartMinute int
	EndMinute   int
}

// NewPeakWindow 按当天零点起的分钟数构造一段高峰时段。
//
// 允许跨零点（例如 22:00–02:00 的夜间高峰）：EndMinute < StartMinute 时
// 视为跨到次日。两端相等视为跨满一整天而不是空区间——空的高峰时段配置
// 出来没有意义，几乎总是把「全天」写错了一笔。
func NewPeakWindow(startMinute, endMinute int) (PeakWindow, error) {
	w := PeakWindow{StartMinute: startMinute, EndMinute: endMinute}
	if err := w.Validate(); err != nil {
		return PeakWindow{}, err
	}
	return w, nil
}

// Validate 检查时段的两端落在一天之内。
func (w PeakWindow) Validate() error {
	if w.StartMinute < 0 || w.StartMinute >= minutesPerDay {
		return fmt.Errorf("%w: 起点 %d 不在 [0, %d) 内", ErrInvalidPeakWindow, w.StartMinute, minutesPerDay)
	}
	if w.EndMinute < 0 || w.EndMinute >= minutesPerDay {
		return fmt.Errorf("%w: 终点 %d 不在 [0, %d) 内", ErrInvalidPeakWindow, w.EndMinute, minutesPerDay)
	}
	// 两端相等不算错误：见 NewPeakWindow，它是「全天」的写法。
	// 但「全天段与其他段并存」是错的，那一条在 PeakSchedule.Validate 里查。
	return nil
}

// String 输出 HH:MM-HH:MM，方便日志里直接读。
func (w PeakWindow) String() string {
	sep := "-"
	if w.EndMinute <= w.StartMinute {
		// 跨零点的段标出来，否则 22:00-02:00 看起来是写反了。
		sep = "-次日"
	}
	return fmt.Sprintf("%s%s%s", clock(w.StartMinute), sep, clock(w.EndMinute))
}

// fullDay 判断是否跨满一整天。
func (w PeakWindow) fullDay() bool { return w.StartMinute == w.EndMinute }

// containsMinute 判断一天之内的第 m 分钟是否落在这一段里。
func (w PeakWindow) containsMinute(m int) bool {
	if w.fullDay() {
		return true
	}
	if w.EndMinute > w.StartMinute {
		return m >= w.StartMinute && m < w.EndMinute
	}
	// 跨零点：当天从起点到午夜，加上次日零点到终点。
	return m >= w.StartMinute || m < w.EndMinute
}

const minutesPerDay = 24 * 60

// PeakSchedule 是一套分时规则：哪些时段算高峰，以及休息日是否整天算高峰。
//
// 段与段之间允许重叠，判定时取「包含该时刻的任意一段」即算高峰。
// 之所以不做「重叠就报错」：上游的规则表常常分「工作日高峰」「周末高峰」
// 两组，把它们合并成一组时重叠是常态，工具不该为此拦人。
type PeakSchedule struct {
	// Windows 是高峰时段。为空表示没有按时段划分的高峰。
	Windows []PeakWindow
	// RestDayAllPeak 为 true 时，休息日整天算高峰。
	//
	// 这是常见的上游形态：「工作日只有 08:00–22:00 算高峰，周末全天算高峰」。
	// 它只对已知的休息日生效——DayUnknown 既不是工作日也不是休息日，
	// 不会因为这一项变成高峰（见 PeakAt）。
	RestDayAllPeak bool
}

// NewPeakSchedule 校验并构造一套规则。
func NewPeakSchedule(restDayAllPeak bool, windows ...PeakWindow) (PeakSchedule, error) {
	s := PeakSchedule{Windows: windows, RestDayAllPeak: restDayAllPeak}
	if err := s.Validate(); err != nil {
		return PeakSchedule{}, err
	}
	return s, nil
}

// Validate 逐段检查，并挡住「全天段与其他段并存」这种自相矛盾的配置。
func (s PeakSchedule) Validate() error {
	for i, w := range s.Windows {
		if err := w.Validate(); err != nil {
			return fmt.Errorf("第 %d 段: %w", i+1, err)
		}
	}
	if len(s.Windows) > 1 {
		for i, w := range s.Windows {
			if w.fullDay() {
				// 全天段已经盖住每分每秒，别的段写在这里说明配置者以为
				// 它们有区别。安静接受会让「改了一段却没有任何效果」发生。
				return fmt.Errorf("%w: 第 %d 段是全天时段，不能再与其他时段并列", ErrInvalidPeakWindow, i+1)
			}
		}
	}
	return nil
}

// PeakKind 是高峰/低谷的二态判定。
type PeakKind string

const (
	// PeakKindPeak 是高峰时段，按高价计。
	PeakKindPeak PeakKind = "peak"
	// PeakKindOffPeak 是低谷时段，按低价计。
	PeakKindOffPeak PeakKind = "off_peak"
	// PeakKindUnknown 是这一天是不是休息日还没查清，因此高峰与否也无法判定。
	PeakKindUnknown PeakKind = "unknown"
)

// Known 判断这个判定是不是确定的结论。
func (k PeakKind) Known() bool { return k == PeakKindPeak || k == PeakKindOffPeak }

// PeakStatus 是一次高峰判定的结果。
type PeakStatus struct {
	// Day 是那天的工作日/休息日判定。它是 Peak 的依据，所以一并带出来，
	// 免得调用方为了写清楚「为什么按高峰算」再查一次。
	Day DayStatus
	// Peak 是时段判定。
	Peak PeakKind
}

// PeakAt 判定某地区某个时刻属于高峰还是低谷。
//
// 判定顺序是「先定日期，再看时段」：
//
//  1. 按地区时区把时刻落到某一天，拿到工作日/休息日/未知；
//  2. 未知 → 直接返回未知。
//     这里**不套用**「周末算低谷」之类的星期五规则：未知多半意味着当年
//     放假安排还没公布，而那几天里可能有调休、也可能有连休，
//     用星期猜一天的答案正是本包要避免的事；
//  3. 休息日且 RestDayAllPeak → 高峰；
//  4. 否则看时刻是否落在某段高峰时段里。跨零点的段按**当地日期**判：
//     22:00–02:00 在当天 22:00 之后成立，在次日 02:00 之前也成立。
func (c *Calendar) PeakAt(ctx context.Context, region Region, instant time.Time, s PeakSchedule) (PeakStatus, error) {
	if err := s.Validate(); err != nil {
		return PeakStatus{}, err
	}

	loc, err := Timezone(region)
	if err != nil {
		return PeakStatus{}, err
	}
	local := instant.In(loc)

	day, err := c.Lookup(ctx, region, DateOf(local))
	if err != nil {
		return PeakStatus{}, err
	}

	st := PeakStatus{Day: day, Peak: PeakKindUnknown}
	if !day.Kind.Known() {
		return st, nil
	}
	if day.Kind == DayRestDay && s.RestDayAllPeak {
		st.Peak = PeakKindPeak
		return st, nil
	}

	// 当天零点起算的分钟数。用当地时钟的时分直接算，而不是拿时刻减去
	// 零点：夏令时切换那天两者会差一小时，而单价是按钟表上的时段定的。
	minute := local.Hour()*60 + local.Minute()
	st.Peak = PeakKindOffPeak
	for _, w := range s.Windows {
		if w.containsMinute(minute) {
			st.Peak = PeakKindPeak
			break
		}
	}
	return st, nil
}

// clock 把一天之内的分钟数格式化成 HH:MM。
func clock(minute int) string {
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}
