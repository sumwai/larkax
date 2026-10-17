// Package host 实现注入到插件运行时的宿主能力。
//
// 插件侧看到的是 sandbox 里的虚拟 SDK：`import { log, calendar, kv } from "larkax"`，
// SDK 再调 `globalThis.__larkax.<能力名>.<函数名>(...)`。这个包写的是那道边界的
// 宿主这一侧：把 JS 参数验成 Go 值、把结果翻回 JS 值。
//
// # 三条约定
//
//  1. **每个宿主函数进门先看 ctx。** 宿主函数阻塞时 Interrupt 无效（实测：宿主
//     sleep 300ms 配 50ms deadline 实际跑满 301ms），所以超时只能靠宿主函数
//     自己提前返回；已经超时就立刻返回，不做任何工作。
//
//  2. **参数不合法就报错，不静默按零值处理。** 时间戳猜错会让一整天的计价
//     按错误的日子算，而那在账上看不出来。
//
//  3. **未知是独立状态，不许压成一个具体的判断。** 日历数据取不到时
//     kind() 如实返回 "unknown"，判定类函数宁可失败也不给一个错的答案。
//
// 这个包**不提供网络与文件系统**：插件拿不到 fetch 与 fs，靠的不是插件层的
// 检查，而是这里没有把它们注入进去。
package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 能力名。它们必须与 sandbox 的虚拟 SDK 里用的名字一致，
// 否则插件调用会得到「能力未启用」。
const (
	capLog      = "log"
	capCalendar = "calendar"
	capKV       = "kv"
)

// Env 是宿主能力需要的宿主侧依赖。每个字段都是窄接口或可空。
type Env struct {
	Logger    *slog.Logger // 必填；为 nil 时用 slog.Default()
	Plugin    string       // 插件 key，用于日志的 plugin 字段
	RequestID string       // 请求标识，用于日志的 request_id 字段
	UserID    int64        // 用户标识，KV 按它做命名空间隔离

	Calendar CalendarSource   // nil 表示本次请求不提供日历能力
	KV       KVStore          // nil 表示本次请求不提供 KV 能力
	Now      func() time.Time // 可注入，便于测试；nil 用 time.Now
}

// logger 返回这个请求要用的日志器。
func (e Env) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// clock 返回取当前时刻的函数。
func (e Env) clock() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Capability 是一组宿主能力，对应 SDK 里的一个命名空间。
type Capability struct {
	// Name 是 SDK 侧的名字：log、calendar、kv。它必须与 sandbox/sdk.go 里
	// 用到的名字一致，否则插件调用会得到「能力未启用」。
	Name string
	// Available 判断这次请求能不能提供它（依赖是否齐备）。
	// 上层按白名单挑选，挑中了但 Available 为 false 时**不注入**。
	Available func() bool
	// Funcs 是命名空间下的函数。
	Funcs map[string]sandbox.HostFunc
}

// HostValue 返回可以直接放进 sandbox.Host.Capabilities 的值。
//
// 必须转成 map[string]any 而不是直接塞 Funcs：sandbox 注入时按这个**具体类型**
// 识别能力的命名空间，map[string]sandbox.HostFunc 不匹配，会被当成
// 「这不是一个函数」。
func (c Capability) HostValue() any {
	out := make(map[string]any, len(c.Funcs))
	for name, fn := range c.Funcs {
		out[name] = fn
	}
	return out
}

// Standard 返回全部标准能力。上层按插件声明的能力白名单挑选。
//
// 挑中的能力若 Available() 为 false 就不注入：插件侧于是收到「能力未启用」，
// 而不是一个永远失败的空壳。
func Standard(env Env) []Capability {
	return []Capability{
		{Name: capLog, Available: func() bool { return true }, Funcs: logFuncs(env)},
		{Name: capCalendar, Available: func() bool { return env.Calendar != nil }, Funcs: calendarFuncs(env)},
		{Name: capKV, Available: func() bool { return env.KV != nil }, Funcs: kvFuncs(env)},
	}
}

// CalendarSource 是日历能力的宿主侧依赖。它刻意是窄接口而不是直接依赖
// internal/calendar 的具体类型——那个包的判定结果类型会演进，
// 而宿主函数只是把它翻成 JS 值。
type CalendarSource interface {
	// Day 判定某个时刻落在哪一天、那天是工作日还是休息日。
	// 返回的 Kind 取值 "workday" / "rest_day" / "unknown"。
	Day(ctx context.Context, region string, instant time.Time) (DayInfo, error)
	// Peak 判定某个时刻是高峰还是低谷。默认排班由实现决定。
	Peak(ctx context.Context, region string, instant time.Time) (string, error)
}

// DayInfo 是一次日历判定的结果。
type DayInfo struct {
	Kind          string // workday | rest_day | unknown
	Name          string // 节假日名称，没有则空
	MakeupWorkday bool
	LocalHour     int
	LocalWeekday  int // 0=周日 … 6=周六，与 JS 的 getDay() 一致
}

// KVStore 是插件私有 KV 的宿主侧依赖。
type KVStore interface {
	Get(ctx context.Context, plugin string, userID int64, key string) (string, bool, error)
	Set(ctx context.Context, plugin string, userID int64, key, value string, ttl time.Duration) error
	Del(ctx context.Context, plugin string, userID int64, key string) error
}

// badArgument 构造一个「插件给的参数不合法」的错误。
//
// 它属于**可以跨到插件侧**的那一类：文案里除了插件自己给的值没有别的东西，
// 而那些值插件本来就拿得到。所以文案要写清楚是哪个参数、收到了什么——
// 只说一句「参数不合法」会让插件作者只能靠猜。
func badArgument(format string, a ...any) error {
	return &sandbox.CapabilityError{
		Code:    sandbox.CodeBadArgument,
		Message: fmt.Sprintf(format, a...),
	}
}

// unavailable 构造一个「宿主依赖不可用」的错误。
//
// 依赖的错误原文（连接串、驱动细节、上游地址）既对插件无用，又不该离开宿主，
// 所以这里给一个固定的分类文案，原文放在 Cause 里——由 sandbox 的 wrapFunc
// 交给 Host.OnError 进宿主日志。**这一层不要用 fmt.Errorf("%w", err) 包依赖
// 错误**：那样原文就成了插件能 catch 到的 message。
func unavailable(message string, err error) error {
	return &sandbox.CapabilityError{
		Code:    sandbox.CodeUnavailable,
		Message: message,
		Cause:   err,
	}
}

// hostFunc 把一段实现包成 sandbox.HostFunc，并在进门处统一检查 ctx。
//
// 「每个宿主函数自带 deadline」放在这里实现，就不会有哪个函数漏掉这一步。
func hostFunc(fn func(ctx context.Context, args []moejs.Value) (moejs.Value, error)) sandbox.HostFunc {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		if err := expired(ctx); err != nil {
			return moejs.Undefined(), err
		}
		return fn(ctx, args)
	}
}

// expired 判断 ctx 是否已经结束，并在结束时给出「为什么」。
//
// 已经结束的 ctx 上不要再做任何工作：moejs 的 Interrupt 在宿主函数阻塞时
// 无效，晚一步返回只是让上游多等一会儿。
//
// 返回的是**未分类**的错误，分类交给 sandbox 的 wrapFunc 那一处（它认
// ErrHostTimeout 与 context.Canceled）。这样「ctx 已结束」这件事只有一处
// 决定插件看到哪个码——在这一层自己拼一个的话，wrapFunc 进门那一次判断
// 会给出另一个码，同一种情形于是有两个答案，而插件正是按码分支的。
//
// 错误里也**不带 ctx 的 cause 原文**：cause 由上层给出，可能带渠道名之类
// 的东西。原文经 wrapFunc 进宿主日志，插件那一侧只有固定文案。
func expired(ctx context.Context) error {
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, sandbox.ErrCallTimeout) {
		// 调用预算用尽时发现的超时是宿主慢（查日历、查 KV），与插件在 JS 里
		// 死循环不是一回事，排查方向不同，所以报宿主的那个 kind。
		return fmt.Errorf("%w: %w", sandbox.ErrHostTimeout, cause)
	}
	return cause
}

// argString 从 JS 参数里取一个字符串。取不到（缺参数、undefined、null）给空串。
func argString(args []moejs.Value, i int) string {
	if i >= len(args) || args[i].IsUndefined() || args[i].IsNull() {
		return ""
	}
	return args[i].String()
}

// argInt64 从 JS 参数里取一个整数。label 是它叫什么，只用于报错——
// 「时间戳不是整数」比「第 1 个参数不是整数」有用得多。
//
// JS 没有整数类型，1.5 的 String() 是 "1.5"，所以这里解析而不是断言。
// 取不到就报错——**不静默按 0 处理**：时间戳猜错会让一整天按错的日子计价，
// 而那在账上看不出来。
func argInt64(args []moejs.Value, i int, label string) (int64, error) {
	if i >= len(args) || args[i].IsUndefined() || args[i].IsNull() {
		return 0, badArgument("%s 缺失", label)
	}
	s := args[i].String()
	if s == "" {
		return 0, badArgument("%s 为空", label)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, badArgument("%s 不是整数: %q", label, s)
	}
	return n, nil
}

// argInt64Or 取一个可缺省的整数：缺参数、undefined、null 都落到 fallback。
func argInt64Or(args []moejs.Value, i int, label string, fallback int64) (int64, error) {
	if i >= len(args) || args[i].IsUndefined() || args[i].IsNull() {
		return fallback, nil
	}
	return argInt64(args, i, label)
}

// argObject 判断某个位置是不是一个普通对象（不是 null、不是函数）。
func argObject(args []moejs.Value, i int) bool {
	if i >= len(args) || !args[i].IsObject() {
		return false
	}
	return !args[i].AsObject().IsCallable()
}
