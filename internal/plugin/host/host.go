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
	"sort"
	"strconv"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 能力名。它们是 sandbox 那份虚拟 SDK 里用的名字（见 sandbox.CapabilityNames），
// 也是插件 `import { log, calendar, kv } from "larkax"` 拿到的那三样东西。
//
// 取自 sandbox 的常量而不是在这里再写一遍字面量：两处对不上时编译器不会报错，
// 只会让插件调用拿到「能力未启用」。
const (
	capLog      = sandbox.CapabilityLog
	capCalendar = sandbox.CapabilityCalendar
	capKV       = sandbox.CapabilityKV
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

// definition 是一项标准能力的定义：名字、这套依赖下能不能提供、函数表。
//
// # 为什么是「一张表」而不是两处清单
//
// 请求期注入哪些能力（Standard 的 Available）与注册期允许申请哪些能力
// （Available 返回的名单）读的是同一份定义，于是「能申请的」与「能提供的」
// 不会各自漂移。分开维护过一次的代价是：插件申请 kv 注册成功、
// 运行期 `globalThis.__larkax.kv` 是 undefined，插件拿到的是 JS 的 TypeError，
// 与宿主承诺的「能力未启用」不是一回事。
type definition struct {
	name      string
	available func(Env) bool
	funcs     func(Env) map[string]sandbox.HostFunc
}

// standardDefinitions 返回标准能力的定义表。
//
// 三项都是「读或写插件的私有空间」，没有一项扩大权限：log 写结构化日志、
// calendar 读日历、kv 读写插件按用户隔离的私有存储。网络与文件系统不在这张表里，
// 也不在任何一张表里——宿主没有实现它们（见包注释）。
func standardDefinitions() []definition {
	return []definition{
		{capLog, func(Env) bool { return true }, logFuncs},
		{capCalendar, func(env Env) bool { return env.Calendar != nil }, calendarFuncs},
		{capKV, func(env Env) bool { return env.KV != nil }, kvFuncs},
	}
}

// Standard 返回全部标准能力。上层按插件声明的能力白名单挑选。
//
// 挑中的能力若 Available() 为 false 就不注入：插件侧于是收到「能力未启用」，
// 而不是一个永远失败的空壳。
func Standard(env Env) []Capability {
	defs := standardDefinitions()
	out := make([]Capability, 0, len(defs))
	for _, d := range defs {
		out = append(out, Capability{
			Name:      d.name,
			Available: func() bool { return d.available(env) },
			Funcs:     d.funcs(env),
		})
	}
	return out
}

// Available 返回这套依赖下**真的能提供**的能力名，按字典序。
//
// 装配期用它算注册校验的「允许集」（见 internal/plugin 的 CompileWith）：
// 判据与请求期注入的能力读的是同一张定义表，两份清单从此同源。
//
// 传进来的 Env 只用到 Calendar 与 KV 两个字段非空与否——Plugin、RequestID、
// UserID 是请求期才有的，这里不看也不该看（见 Build 的注释）。日志能力永远在，
// 它不依赖任何外部东西，注入它也就没有失败可言。
func Available(env Env) []string {
	var out []string
	for _, d := range standardDefinitions() {
		if d.available(env) {
			out = append(out, d.name)
		}
	}
	sort.Strings(out)
	return out
}

// CalendarSource 是日历能力的宿主侧依赖。它刻意是窄接口而不是直接依赖
// internal/calendar 的具体类型——那个包的判定结果类型会演进，
// 而宿主函数只是把它翻成 JS 值。
type CalendarSource interface {
	// Day 判定某个时刻落在哪一天、那天是工作日还是休息日。
	// 返回的 Kind 取值 "workday" / "rest_day" / "unknown"。
	Day(ctx context.Context, region string, instant time.Time) (DayInfo, error)
	// Peak 判定某个时刻是高峰还是低谷。高峰窗口由**调用方**给出
	// （window 是插件写的那个串，写法与规则的 `isPeak("08:00-22:00")` 同形），
	// 返回 "peak" / "off_peak" / "unknown" 三态之一。
	//
	// 窗口随调用走，而不是由实现自己决定：峰谷是定价口径，不由地区推断。
	// 写法由调用方先校验（见 calendar.go 的 checkPeakWindow），实现拿到的串
	// 因此是合法的——它仍然可以自己再判一次，不该假设。
	Peak(ctx context.Context, region string, instant time.Time, window string) (string, error)
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
//
// # 隔离只到用户这一层
//
// 每个方法都带 plugin 与 userID，实现按 (plugin, userID, key) 三段隔离。
// **没有插件级的共享命名空间**，而且不该有：KV 是插件唯一的存储（它不能访问
// 网络、不能读写文件），也就成了唯一可能让数据从一个用户流向另一个用户的地方；
// 多开一层共享就是凭空造出第一个这样的出口。将来真需要跨用户共享，应该做成
// **单独一个能力名**（例如 kv_shared），而不是同一个 kv 里的一层——那样它会
// 出现在插件的 capabilities 里、可审计，也能单独被拒绝。
type KVStore interface {
	Get(ctx context.Context, plugin string, userID int64, key string) (string, bool, error)
	Set(ctx context.Context, plugin string, userID int64, key, value string, ttl time.Duration) error
	// Del 删一个键，返回**是否真的删掉了**（删之前它存在，且还没过期）。
	//
	// 早先它只回错误码，于是宿主函数要「先问在不在、再删」，而两次调用之间不是
	// 原子的：并发的两次 del 会都报 true。结果由实现一次回之后，插件看到的那个
	// 布尔值才是准的，也少一次往返（见 kv.go 的 kvDel）。
	//
	// 已经过期的键算「不存在」：删它回 false（它本来就查不到）。
	Del(ctx context.Context, plugin string, userID int64, key string) (bool, error)
}

// 每个 (plugin, userID) 的累计上限：键数与键值加起来的字节数。
//
// 单次上限（键 ≤ 512 字节、值 ≤ 64 KiB，见 kv.go）挡不住「写一万个键」这种用法，
// 而 KV 是**共享资源**：一个插件写进去的字节数会影响同一张表上所有人的成本。
// 两个数都是硬约束，越过时写入被拒、插件拿到 quota_exceeded（见 kvWriteError）。
//
// 取值是一处权衡之后的**选择**，不是推出来的：1 MiB 放得下一个用户在该插件下的
// 摘要、偏好与游标这类状态，而后端不至于被一个插件把盘占掉。它是这个包的常量
// 而不是配置项——加一个配置项要先进 settings 表与它自己的一串校验，而这件事
// 现在没有那个需要（真要调，改这里的两个数并在用例里跟着改）。
const (
	// KVMaxKeys 是键数上限。
	KVMaxKeys = 1024
	// KVMaxBytes 是键与值一共占的字节数上限。
	KVMaxBytes = 1 << 20
)

// ErrKVQuotaExceeded 表示这次写入会让 (plugin, userID) 的累计用量越过限额。
//
// 由 KVStore 的实现返回，宿主函数据此给出 quota_exceeded 那个码——**这一层不自己
// 数**：键数与总字节是存储里的事实，在宿主函数里数要先读一遍，而那又是两次调用
// 之间不原子的一件事（同一个键的两次写会各自看到「还没到顶」）。
var ErrKVQuotaExceeded = errors.New("插件 KV 的配额已用尽")

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
