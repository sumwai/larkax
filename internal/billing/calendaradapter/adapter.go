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
		return expr.DayInfo{}, fmt.Errorf("%w: %s %s 的日历数据不可用（%s）",
			expr.ErrCalendarRequired, region, status.Date, status.Reason)

	default:
		// 新增了 DayKind 取值却忘了在这里处理。报错而不是落到某个分支：
		// 落到工作日会让新状态被静默当成工作日。
		return expr.DayInfo{}, fmt.Errorf("日历返回了未知的判定类型 %q", status.Kind)
	}
}

// ctx 提供一个供适配器使用的 context。
//
// `expr.Calendar` 的接口不接 context（求值本身是同步的、不查库），
// 而 `calendar.Lookup` 需要它。这里用一个空的：数据源实现若不是
// 网络型的，不需要取消；若将来接了网络数据源，这个接口要跟着改，
// 而不是在这里塞一个超时——超时的长度只有调用方知道该是多少。
func (a *Adapter) ctx() context.Context { return context.Background() }
