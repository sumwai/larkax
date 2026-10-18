// Package calendaradapter 把 internal/calendar 接到计价引擎的日历接口上。
//
// 单独一个包而不是二选一：
//
//   - `internal/calendar` 不该 import 计价引擎——它是通用日期判定，
//     计价只是它的一个使用者。
//   - `internal/billing/expr` 不该 import `internal/calendar`——
//     那会让「换一个节假日数据源」变成一次跨包改动，而这个包存在的意义
//     就是让求值只依赖一个最小的接口。
//
// 于是适配层放在这里：它同时 import 两边，是唯一知道两者的地方。
// 代价是这一层的方向要写清楚——**它只做形状转换，不做判定**。
package calendaradapter

import (
	"context"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/calendar"
)

// Adapter 让 *calendar.Calendar 满足 expr.Calendar。
type Adapter struct {
	cal *calendar.Calendar
}

// New 包装一个日历。
func New(cal *calendar.Calendar) (*Adapter, error) {
	if cal == nil {
		return nil, fmt.Errorf("日历为空")
	}
	return &Adapter{cal: cal}, nil
}

// 编译期断言：形状必须对得上，不对就在改动的这一次报错，
// 而不是在某个计费请求上。
var _ expr.Calendar = (*Adapter)(nil)

// TimeZone 返回地区对应的时区名。
//
// `expr` 要的是**名字**（它自己 LoadLocation），而 calendar 给的是
// *time.Location。用 loc.String() 还原：calendar 的时区表本身就是由
// IANA 名字构造出来的，所以往返是精确的。
func (a *Adapter) TimeZone(region string) (string, error) {
	loc, err := calendar.Timezone(calendar.Region(region))
	if err != nil {
		return "", err
	}
	return loc.String(), nil
}

// DayInfo 返回某地区某个日历日的属性。
//
// # 未知一律变成错误，不返回一个「猜测的」DayInfo
//
// 这是本适配器最要紧的一条。`calendar` 包特意把 DayKind 做成三态
// （workday / rest_day / unknown），就是为了防止有人把未知静默压成工作日——
// 那会让节假日按工作日计价，而账目上完全看不出来。
//
// 压成工作日在这里尤其危险：它发生在**计费路径**上，错的是钱。
// 所以未知直接返回错误，让求值失败。这与别处「未知要传出去让调用方决定」
// 的原则一致——这里只是把「怎么处置」定成了失败。
//
// 为什么不返回 `DayInfo{}` 加一个错误位：`expr` 的接口没有那个位，
// 而返回零值加错误会让两个调用点里总有一个忘记检查错误。
func (a *Adapter) DayInfo(region string, day time.Time) (expr.DayInfo, error) {
	ctx := a.ctx()
	status, err := a.cal.Lookup(ctx, calendar.Region(region), calendar.DateOf(day))
	if err != nil {
		return expr.DayInfo{}, err
	}

	switch status.Kind {
	case calendar.DayWorkday:
		return expr.DayInfo{
			OffDay:        false,
			HolidayName:   status.Name,
			MakeupWorkday: status.MakeupWorkday,
		}, nil

	case calendar.DayRestDay:
		return expr.DayInfo{
			OffDay:        true,
			HolidayName:   status.Name,
			MakeupWorkday: status.MakeupWorkday,
		}, nil

	case calendar.DayUnknown:
		// 把成因带进错误里：`year_pending`（公告未发布，正常）与
		// `row_missing`（数据不完整，要查）对运维是两个完全不同的问题，
		// 而它们在这里都表现为「这一天算不出价」。
		return expr.DayInfo{}, unknownDayError(region, status)

	default:
		// 新增了 DayKind 取值却忘了在这里处理。报错而不是落到某个分支：
		// 落到工作日会让新状态被静默当成工作日。
		return expr.DayInfo{}, fmt.Errorf("日历返回了未知的判定类型 %q", status.Kind)
	}
}

// Peak 判定某地区某个时刻算不算高峰，高峰时段由规则给出。
//
// 它把 expr 侧的时段形状翻成 calendar 侧的，再交给 PeakAt 判——与 DayInfo
// 一样，这一层只做形状转换，判定不在这里重写（窗口怎么算、跨零点怎么算，
// 见 internal/calendar 的 peak.go）。
//
// # 窗口的形状在这里再校验一次
//
// 规则的窗口在**保存规则**时就判过（见 internal/billing/expr 的
// ParsePeakWindows），所以从规则走下来的窗口都是合法的。这里仍然走 calendar
// 那两个构造函数、让它们再判一次：它们的入口不止规则一条（插件适配器与夹具
// 都直接构造排班），少判一次换不来什么。
//
// # unknown 与 DayInfo 同一条口径
//
// PeakAt 给出的是三态，其中 unknown 表示「那一天是不是休息日还没查清」，
// 于是峰谷也判不出来。这里把它折成错误，理由与上面那个 switch 完全一样：
// false 是一个具体的判断（「按低谷算」），用它代理未知会在春节期间按低谷价
// 收费，而账单看上去完全正常。
//
// Peak 为 unknown 只可能是日子未知：那一天已知时它必然落到 peak 或 off_peak
// （见 PeakAt 的判定顺序），所以这条错误讲的还是「哪一天的日历没有数据」。
func (a *Adapter) Peak(region string, instant time.Time, windows []expr.PeakWindow) (bool, error) {
	converted := make([]calendar.PeakWindow, 0, len(windows))
	for _, w := range windows {
		pw, err := calendar.NewPeakWindow(w.StartMinute, w.EndMinute)
		if err != nil {
			return false, err
		}
		converted = append(converted, pw)
	}
	// 休息日整天算不算高峰留给规则组合（`!isWorkday() || isPeak(...)`），
	// 所以这里恒为 false——接口上那一个开关由规则表达，见 expr.Calendar 的 Peak。
	schedule, err := calendar.NewPeakSchedule(false, converted...)
	if err != nil {
		return false, err
	}

	st, err := a.cal.PeakAt(a.ctx(), calendar.Region(region), instant, schedule)
	if err != nil {
		return false, err
	}
	if !st.Peak.Known() {
		return false, unknownDayError(region, st.Day)
	}
	return st.Peak == calendar.PeakKindPeak, nil
}

// unknownDayError 把「这一天的日历数据还没有」讲成一句指向配置缺口的话。
//
// DayInfo 与 Peak 共用它：两处是同一件事在两个入口上的表现，各写一份的话，
// 运维会为同一个原因拿到两句不一样的解释。
//
// 措辞指向**配置缺口**而不是服务故障：日历数据是运维灌进来的
// （见 internal/calendar/store），缺数据时该去看的是「这一年的数据导入了没有」，
// 而一句「日历不可用」会把人引向「日历服务挂了」——这个项目里根本没有那样
// 一个服务。
func unknownDayError(region string, day calendar.DayStatus) error {
	return fmt.Errorf("%w：%s %s 的节假日日历还没有数据（%s）。"+
		"这是配置缺口，不是服务故障：该年的日历数据还没有导入，在它补齐之前"+
		"用到日历的计价规则一直算不出价",
		expr.ErrCalendarRequired, region, day.Date, day.Reason)
}

// ctx 提供一个供适配器使用的 context。
//
// `expr.Calendar` 的接口不接 context（求值本身是同步的、不查库），
// 而 `calendar.Lookup` 需要它。这里用一个空的：数据源实现若不是
// 网络型的，不需要取消；若将来接了网络数据源，这个接口要跟着改，
// 而不是在这里塞一个超时——超时的长度只有调用方知道该是多少。
func (a *Adapter) ctx() context.Context { return context.Background() }
