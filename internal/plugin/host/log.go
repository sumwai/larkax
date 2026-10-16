package host

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

const (
	// logMessageMaxBytes 是 message 的字节上限。插件可以塞进任意长的字符串，
	// 而日志的留存期与传播范围远大于插件本身。
	logMessageMaxBytes = 4096

	// fieldKeyMax 与 fieldDepthMax 限制日志字段的规模。fields 是插件给的，
	// 不设界的转换等于让插件决定宿主要为此花多少内存。
	fieldKeyMax   = 32
	fieldDepthMax = 4

	// truncatedMark 标在被截断的 message 末尾。截断不标注的话，
	// 读的人会以为插件只打印了这么多。
	truncatedMark = "…(已截断)"

	// arrayClassName 用来把 JS 数组与普通对象分开。判定用名字是因为
	// 引擎的 Class 常量没有从 moejs 导出。
	arrayClassName = "Array"
)

// logFuncs 实现 SDK 的 log(level, message, fields)。
//
// 日志写不进去不该让请求失败：level 认不出来按 info，fields 转不出来就丢掉，
// 只有 ctx 已经结束才返回错误（那时上层也不想要结果了）。
func logFuncs(env Env) map[string]sandbox.HostFunc {
	return map[string]sandbox.HostFunc{
		"log": hostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			level := parseLevel(argString(args, 0))
			message := truncateMessage(argString(args, 1))

			// plugin 与 request_id 始终带上：日志里只有一句插件说的话时，
			// 排查要从头翻一遍才知道是谁、在哪一次请求里说的。
			attrs := make([]slog.Attr, 0, 2+fieldKeyMax)
			attrs = append(attrs,
				slog.String("plugin", env.Plugin),
				slog.String("request_id", env.RequestID),
			)
			attrs = append(attrs, fieldAttrs(args, 2)...)

			env.logger().LogAttrs(ctx, level, message, attrs...)
			return moejs.Undefined(), nil
		}),
	}
}

// parseLevel 把文本级别映射为 slog 级别。
//
// 与 internal/observe 的 ParseLevel 取值一致，但**不 import 它**：
// 宿主能力包与日志基础设施之间没有别的关系，少一条依赖就少一处耦合。
//
// 认不出来的值一律回落到 info：级别写错不该让插件调用失败，
// 而 info 是既能看见异常、又不会淹没在调试输出里的选择。
func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// truncateMessage 把 message 截到字节上限，落在一个 rune 边界上并标注截断。
func truncateMessage(s string) string {
	if len(s) <= logMessageMaxBytes {
		return s
	}
	cut := logMessageMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMark
}

// fieldAttrs 把可选的 fields 转成日志字段。转不出来就返回 nil。
func fieldAttrs(args []moejs.Value, i int) []slog.Attr {
	fields, ok := safeFields(args, i)
	if !ok || len(fields) == 0 {
		return nil
	}
	// 按名字排序：slog 输出的是 map 的遍历顺序，不排的话同一段 fields
	// 每次打印的次序都不同，比对两次日志时会多出一堆噪音差异。
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	attrs := make([]slog.Attr, 0, len(names))
	for _, name := range names {
		attrs = append(attrs, slog.Any(name, fields[name]))
	}
	return attrs
}

// safeFields 读取 fields 并转成 Go 值。
//
// 读的是**自有可枚举属性**的原始值，不跑 getter、不跑 toJSON：
// 插件给的一个 fields 对象不该在宿主里执行任意代码，转不出来（函数、
// BigInt、嵌套过深）就整个丢掉，而不是让这次 log 失败。
//
// 这里不去 recover 引擎的 panic：实测读一个被撤销的代理、或者一个
// ownKeys 会抛异常的代理，引擎都只是给回一个空对象，不会把 panic 漏到 Go 这一侧。
// 为一个构造不出来的分支写 recover，只会让下一个人以为它被验证过。
func safeFields(args []moejs.Value, i int) (fields map[string]any, ok bool) {
	if !argObject(args, i) {
		return nil, false
	}
	v, _ := valueToGo(args[i], fieldDepthMax)
	// 顶层不是一个对象时 v 是别的类型，断言失败即「忽略它」。
	fields, ok = v.(map[string]any)
	return fields, ok
}

// valueToGo 把 JS 值翻成 Go 值。ok 为 false 表示这个值转不出来。
func valueToGo(v moejs.Value, depth int) (any, bool) {
	switch {
	case v.IsNull(), v.IsUndefined():
		return nil, true
	case v.IsBool():
		return v.AsBool(), true
	case v.IsNumber():
		return v.AsNumber(), true
	case v.IsString():
		return v.AsString().GoString(), true
	case v.IsBigInt(), v.IsSymbol():
		// 这两个没有 JSON 对应物：BigInt 直接当数字记会悄悄丢精度，
		// symbol 的调试名字也不是它的身份。宁可不记。
		return nil, false
	case v.IsObject():
		return objectToGo(v.AsObject(), depth)
	default:
		// hole 之类的引擎内部值。它不会到达插件，moejs 也没有导出构造它的
		// 函数，所以这一支在测试里覆盖不到——留着是为了「认不出来的就当读不出来」
		// 这条规则本身没有缺口。
		return nil, false
	}
}

// objectToGo 把 JS 对象或数组翻成 map[string]any / []any。
func objectToGo(o *moejs.Object, depth int) (any, bool) {
	if depth <= 0 || o.IsCallable() {
		return nil, false
	}
	keys := o.OwnEnumerableStringKeys()

	if o.ClassName() == arrayClassName {
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			if !k.IsIndex() {
				// 数组上的自定义属性不进日志字段：它与下标含义不同，
				// 混进同一个序列里会读错。
				continue
			}
			elem, ok := o.GetOwnDataValue(k)
			if !ok {
				continue
			}
			g, ok := valueToGo(elem, depth-1)
			if !ok {
				continue
			}
			out = append(out, g)
		}
		return out, true
	}

	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if len(out) >= fieldKeyMax {
			break
		}
		val, ok := o.GetOwnDataValue(k)
		if !ok {
			continue
		}
		g, ok := valueToGo(val, depth-1)
		if !ok {
			continue
		}
		// OwnEnumerableStringKeys 只给字符串键与下标键，GoString 两种都覆盖
		// （对象上写的 "0" 是下标键，字面上要还原成 "0"）。
		out[k.GoString()] = g
	}
	return out, true
}
