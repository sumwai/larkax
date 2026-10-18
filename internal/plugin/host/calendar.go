package host

import (
	"context"
	"errors"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 日历判定的结果取值，与 DayInfo.Kind 的约定一致。
//
// 这三个字面量必须与 internal/calendar 的 DayKind 取值保持一致：本包的
// **生产代码**刻意不 import 那个包（见 host.go 的 CalendarSource），所以它们
// 对不上时编译器不会报错，只会让 kindFn 把这些取值当成契约外的值、静默降级成
// unknown。替编译器看着它们的是 calendar_test.go 里那条对拍用例——那是本包
// 唯一 import 那个包的地方。
const (
	kindWorkday = "workday"
	kindRestDay = "rest_day"
	kindUnknown = "unknown"
)

// 高峰判定的结果取值，与 internal/calendar 的 PeakKind 一致。
//
// 与上面那三个同一条理由：本包的生产代码不 import internal/calendar，两边
// 对不上时编译器不会报错。**没有 unknown 这一项**：两处都管「判不出来」叫同
// 一个字符串，所以那一个复用上面的 kindUnknown，而不是在这里再写一个同值的
// 名字——两个名字指同一个字符串时，迟早有人改一个忘另一个。
const (
	kindPeak    = "peak"
	kindOffPeak = "off_peak"
)

// calendarFuncs 实现 SDK 的 calendar 命名空间。
func calendarFuncs(env Env) map[string]sandbox.HostFunc {
	return map[string]sandbox.HostFunc{
		"isOffDay":        hostFunc(isOffDay(env)),
		"isMakeupWorkday": hostFunc(isMakeupWorkday(env)),
		"holidayName":     hostFunc(holidayNameFn(env)),
		"kind":            hostFunc(kindFn(env)),
		"hourIn":          hostFunc(hourIn(env)),
		"weekdayIn":       hostFunc(weekdayIn(env)),
		"peak":            hostFunc(peakFn(env)),
	}
}

// isOffDay 判断某个时刻是不是休息日（不上班的那一天）。
//
// **未知时返回错误，而不是 false，这是刻意的保守选择。** false 的意思是
// 「这一天是工作日」，那是一个具体的判断，而未知不是——把它压成 false
// 等于替插件猜了一次，猜错的后果是整天按错误分类计价，而账上看不出来。
// 抛错则让插件这条分支断掉，问题会浮到调用方。
//
// 调休上班的那一天落在周末，但它是**上班日**，所以这里回答 false；
// 要区分「周末上班」这件事本身，用 isMakeupWorkday。
func isOffDay(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		instant, region, err := calendarArgs(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		info, err := env.Calendar.Day(ctx, region, instant)
		if err != nil {
			return moejs.Undefined(), unavailable("查询日历失败", err)
		}
		switch info.Kind {
		case kindWorkday:
			return moejs.Bool(false), nil
		case kindRestDay:
			return moejs.Bool(true), nil
		default:
			return moejs.Undefined(), badArgument(
				"日历没有 %s 在 %s 的判定，无法回答是否休息日（不按工作日处理）", region, instant.UTC().Format(time.RFC3339))
		}
	}
}

// isMakeupWorkday 判断某个时刻是不是调休上班日。
//
// 未知时报错，理由同 isOffDay：false 是一个具体的判断，不能拿它代理未知。
func isMakeupWorkday(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		instant, region, err := calendarArgs(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		info, err := env.Calendar.Day(ctx, region, instant)
		if err != nil {
			return moejs.Undefined(), unavailable("查询日历失败", err)
		}
		if info.Kind == kindUnknown {
			return moejs.Undefined(), badArgument(
				"日历没有 %s 在 %s 的判定，无法回答是否调休上班日", region, instant.UTC().Format(time.RFC3339))
		}
		return moejs.Bool(info.MakeupWorkday), nil
	}
}

// holidayNameFn 返回节假日名称。
//
// 名称只回答「有没有名字」，不参与判定：未知的返回 null（伪值、调用方自己判断），
// 空名字与「没有这一天」在执行层没有区别，都不是错误。
func holidayNameFn(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		instant, region, err := calendarArgs(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		info, err := env.Calendar.Day(ctx, region, instant)
		if err != nil {
			return moejs.Undefined(), unavailable("查询日历失败", err)
		}
		if info.Name == "" {
			return moejs.Null(), nil
		}
		return moejs.String(info.Name), nil
	}
}

// kindFn 如实回报这一天是哪一种。
//
// **未知是独立状态，原样返回 "unknown"，不压成 workday。** 静默压成工作日
// 会让一整天按错误分类计价，而账上看不出来。
func kindFn(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		instant, region, err := calendarArgs(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		info, err := env.Calendar.Day(ctx, region, instant)
		if err != nil {
			return moejs.Undefined(), unavailable("查询日历失败", err)
		}
		switch info.Kind {
		case kindWorkday, kindRestDay, kindUnknown:
			return moejs.String(info.Kind), nil
		default:
			// 数据源给了一个契约外的取值。如实说不知道，而不是把它当成某一个。
			return moejs.String(kindUnknown), nil
		}
	}
}

// hourIn 返回某个地区此刻的钟点（0–23）。
func hourIn(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		region := argString(args, 0)
		if err := checkRegion(region); err != nil {
			return moejs.Undefined(), err
		}
		info, err := env.Calendar.Day(ctx, region, env.clock())
		if err != nil {
			return moejs.Undefined(), unavailable("查询日历失败", err)
		}
		if info.LocalHour < 0 || info.LocalHour > 23 {
			return moejs.Undefined(), badArgument("日历给出的钟点超出范围: %d", info.LocalHour)
		}
		return moejs.Int(int64(info.LocalHour)), nil
	}
}

// weekdayIn 返回某个地区此刻的星期几（0=周日 … 6=周六，与 JS 的 getDay() 一致）。
func weekdayIn(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		region := argString(args, 0)
		if err := checkRegion(region); err != nil {
			return moejs.Undefined(), err
		}
		info, err := env.Calendar.Day(ctx, region, env.clock())
		if err != nil {
			return moejs.Undefined(), unavailable("查询日历失败", err)
		}
		if info.LocalWeekday < 0 || info.LocalWeekday > 6 {
			return moejs.Undefined(), badArgument("日历给出的星期超出范围: %d", info.LocalWeekday)
		}
		return moejs.Int(int64(info.LocalWeekday)), nil
	}
}

// peakFn 判定某个时刻是高峰还是低谷，高峰窗口**由插件自己给**。
//
// 参数是 (ts, region, window)：时刻与地区那两位与别的日历函数同形，窗口串多
// 一位。窗口随调用走，写法与规则里的 `isPeak("08:00-22:00")` 是同一套——峰谷是
// 计价口径里的一项，不由地区推断（同一地区的两个渠道可以有各自的峰谷），所以它
// 跟着调用走：规则写在规则里，插件写在调用里。
//
// # 窗口先校验，再判定
//
// 写法不合法时报错回插件，不下传给日历判：`"25:00-26:00"`、`"8:5-9:00"` 这类串
// 在判定里只会变成「不落在任何一段里」，也就是静默的 off_peak——插件那一侧看
// 不出自己写错了，这条路算出来的钱会一直按低谷那一档走，而账上看不出异常。
// 校验用的是规则侧同一个解析函数（见 expr.ParsePeakWindows），于是插件与规则
// 不会各认一套写法。
//
// 报错用的是 bad_argument，不是 unavailable：错的是插件给的串，该改的是插件
// 自己（分类见 docs/plugin-contract.md 那张表）。
//
// # 三个档位如实回给插件
//
// peak / off_peak / unknown 原样回给插件，与 kindFn 同一条口径：未知是独立状态，
// 压成 off_peak 就是替插件按低谷猜了一次，而那正是本包一路在避免的事。哪个档位
// 算高价、休息日算不算高峰，由插件按自己的口径定——插件改不了单价，它影响的是
// 上报什么用量、走哪条渠道；这里只回答「是不是高峰」。
func peakFn(env Env) func(context.Context, []moejs.Value) (moejs.Value, error) {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		instant, region, err := calendarArgs(args)
		if err != nil {
			return moejs.Undefined(), err
		}
		window := argString(args, 2)
		if winErr := checkPeakWindow(window); winErr != nil {
			return moejs.Undefined(), winErr
		}
		kind, err := env.Calendar.Peak(ctx, region, instant, window)
		if err != nil {
			return moejs.Undefined(), unavailable("查询日历失败", err)
		}
		switch kind {
		case kindPeak, kindOffPeak, kindUnknown:
			return moejs.String(kind), nil
		default:
			// 数据源给了一个契约外的取值。如实说不知道，而不是把它当成某一个
			// ——与 kindFn 同一个处置。
			return moejs.String(kindUnknown), nil
		}
	}
}

// checkPeakWindow 校验插件给的高峰窗口串的写法。
//
// 空串也走这里：解析器会说清它要的是 `HH:MM-HH:MM`，而「没传参数」与「传了个
// 空串」在 JS 那一侧是同一件事（`undefined` 与 `""` 都读成空串）。
func checkPeakWindow(window string) error {
	if _, err := expr.ParsePeakWindows(window); err != nil {
		// 只取解析器给出的那句原因，不是整条错误链：链上还挂着
		// ErrPeakWindowSyntax 这个分类（它说的是「写法不成立」，在
		// 「高峰窗口 %q 写法不合法」后面重复一次没有信息）。
		var syntax *expr.PeakWindowSyntaxError
		if errors.As(err, &syntax) {
			return badArgument("高峰窗口 %q 写法不合法：%s", window, syntax.Reason)
		}
		return badArgument("高峰窗口 %q 写法不合法：%v", window, err)
	}
	return nil
}

// calendarArgs 取出日历类函数的两个参数：时刻与地区。
//
// ts 按**秒**解释（JS 惯例 Date.now()/1000）。
func calendarArgs(args []moejs.Value) (time.Time, string, error) {
	ts, err := argInt64(args, 0, "时间戳")
	if err != nil {
		return time.Time{}, "", err
	}
	region := argString(args, 1)
	if err := checkRegion(region); err != nil {
		return time.Time{}, "", err
	}
	return time.Unix(ts, 0), region, nil
}

// checkRegion 校验地区标识非空。
//
// 地区为空时不能「按默认地区查」：地区的默认值是上层策略，而这里猜一个
// 等于让插件拿到一个它没要过的地方的判定结果。
func checkRegion(region string) error {
	if region == "" {
		return badArgument("地区不能为空")
	}
	return nil
}
