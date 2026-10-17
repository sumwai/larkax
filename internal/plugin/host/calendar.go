package host

import (
	"context"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 日历判定的结果取值，与 DayInfo.Kind 的约定一致。
//
// 这三个字面量必须与 internal/calendar 的 DayKind 取值保持一致：本包刻意
// 不 import 那个包（见 host.go 的 CalendarSource），所以它们对不上时编译器
// 不会报错，只会让 kindFn 把这些取值当成契约外的值、静默降级成 unknown。
const (
	kindWorkday = "workday"
	kindRestDay = "rest_day"
	kindUnknown = "unknown"
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
