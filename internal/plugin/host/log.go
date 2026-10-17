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

	// fieldValueMaxBytes 是**单个字符串值**的字节上限。
	//
	// 键的个数有 fieldKeyMax 管着，值的长度一直没人管：一个 200 KB 的字符串
	// 会原样进日志（实测 204891 字节）。日志的留存期与传播范围远大于插件本身，
	// 所以「插件决定宿主为此花多少内存」这件事在值这一维上同样不能成立。
	fieldValueMaxBytes = 1024

	// fieldTotalMaxBytes 是整条 fields 转换结果的字节预算。
	//
	// **只限单个值不够。** fieldKeyMax 限的是对象的键数，而数组的长度没有
	// 上限——一百万个短字符串照样能撑爆日志。所以**凡是进 fields 的东西都要过
	// spend**：字符串按自己的字节数（至少 scalarCost，见 boundedString）、
	// 键按自己的字节数、其余每个值按 scalarCost。漏掉任何一份，「这条 fields
	// 最多值多少字节」就不再是一个可以说出来的数。
	//
	// 于是**记账**不超过 8192：数组最多 1023 个元素（容器自己记 8 字节；外层
	// 对象与键还要再扣几个，实测 1021），对象最多 fieldKeyMax 个条目。
	//
	// **记账不等于渲染。** 数字记 8 字节，而最长的 float64 写成 JSON 有 24 字节，
	// 加一个逗号是 25——所以日志行的上限是 32 KiB 那一档，不是 8192（最坏情形
	// 实测 24610 字节，见 TestLogBoundsWorstCaseRenderedBytes）。说「整条不超过
	// 8 KiB」时说的是记账口径；两个数要一起说，否则「上限」会被读成日志行的长度。
	fieldTotalMaxBytes = 8 << 10

	// scalarCost 是每个非字符串值计入预算的名义开销。
	//
	// 数字与布尔本身很小，但它们在日志里各自占一个位置，而数组长度无上限。
	// 不记账的话「一百万个数字」这条路径就没有任何一道界。
	scalarCost = 8

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
	budget := fieldBudget{left: fieldTotalMaxBytes}
	v, _ := valueToGo(args[i], fieldDepthMax, &budget)
	// 顶层不是一个对象时 v 是别的类型，断言失败即「忽略它」。
	fields, ok = v.(map[string]any)
	return fields, ok
}

// fieldBudget 是转换 fields 时的字节预算。
//
// 字符串值按自己的字节数记账，其余每个值按 scalarCost 记账。预算用尽之后
// valueToGo 一律返回「转不出来」，于是剩下的部分被丢掉——**丢掉而不是
// 截成一个更短的日志**：一条被截过的 fields 在日志里读起来和完整的一样，
// 而「这里少了几个字段」这件事没人看得出来。
type fieldBudget struct{ left int }

// spend 扣掉 n 字节。返回 false 表示预算不够，调用方应当丢掉这个值。
func (b *fieldBudget) spend(n int) bool {
	if b.left < n {
		b.left = 0
		return false
	}
	b.left -= n
	return true
}

// valueToGo 把 JS 值翻成 Go 值。ok 为 false 表示这个值转不出来
// （转不出来、或者预算已经不够）。
func valueToGo(v moejs.Value, depth int, budget *fieldBudget) (any, bool) {
	switch {
	case v.IsNull(), v.IsUndefined():
		return nil, budget.spend(scalarCost)
	case v.IsBool():
		return v.AsBool(), budget.spend(scalarCost)
	case v.IsNumber():
		return v.AsNumber(), budget.spend(scalarCost)
	case v.IsString():
		return boundedString(v.AsString().GoString(), budget)
	case v.IsBigInt(), v.IsSymbol():
		// 这两个没有 JSON 对应物：BigInt 直接当数字记会悄悄丢精度，
		// symbol 的调试名字也不是它的身份。宁可不记。
		return nil, false
	case v.IsObject():
		return objectToGo(v.AsObject(), depth, budget)
	default:
		// hole 之类的引擎内部值。它不会到达插件，moejs 也没有导出构造它的
		// 函数，所以这一支在测试里覆盖不到——留着是为了「认不出来的就当读不出来」
		// 这条规则本身没有缺口。
		return nil, false
	}
}

// boundedString 把一个字符串截到「单值上限」与「剩余预算」之中更小的那个。
//
// 截断标记也占预算，所以先把它留出来再切。不留的话总输出会超出预算一小段
// （每个被截的值多出标记那么长），而「这条 fields 最多多少字节」就成了一句
// 大约的话——上限说不准就等于没有上限。
//
// **每个值至少记 scalarCost。** 字符串的字节数可以是 0，而 spend(0) 恒真，
// 于是「零字节的值」是一条不记账的路径：数组那一支只限了初始容量，append 之后
// 长度不受约束，于是一百万个空字符串能一个字节都不花地进日志（实测日志行
// 3000106 字节，一次 log 调用在宿主侧多分配约 116 MB）。这不是「字符串比标量
// 贵」，而是「凡是进 fields 的东西都要过 spend」这条规则在零字节上不能有洞；
// 标量那一支用的就是 scalarCost，这里照同一个形状办，不另发明一个常数。
func boundedString(s string, budget *fieldBudget) (any, bool) {
	if len(s) <= fieldValueMaxBytes && budget.spend(max(len(s), scalarCost)) {
		return s, true
	}
	limit := min(fieldValueMaxBytes, budget.left)
	if limit <= len(truncatedMark) {
		// 剩下的预算连标记都放不下。这个值整个丢掉，而不是留一个只剩标记的
		// 空壳——那在日志里比没有这个键还难读。
		return nil, false
	}
	cut := limit - len(truncatedMark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	out := s[:cut] + truncatedMark
	budget.spend(len(out))
	return out, true
}

// objectToGo 把 JS 对象或数组翻成 map[string]any / []any。
func objectToGo(o *moejs.Object, depth int, budget *fieldBudget) (any, bool) {
	if depth <= 0 || o.IsCallable() {
		return nil, false
	}
	// 容器本身也占一个位置：一个空对象在日志里同样是两个字节。
	if !budget.spend(scalarCost) {
		return nil, false
	}
	// **预分配的容量跟着上限走，不跟着 len(keys) 走。** len(keys) 是插件
	// 决定的：一个一百万元素的 Float64Array（JS 侧每元素 8 字节）会让这里
	// 为了留下 32 个键先造一个能装一百万个键的容器——实测一次 log 调用在
	// 宿主侧分配约 100 MB（其中约 84 字节/元素就是这一句 map），而写出来的
	// 日志行只有几百字节。上限是 32 个键、1024 个元素（预算 8192 除以每个值
	// 至少记的 8 字节），两者都远小于插件能给的规模。
	//
	// 这一句管不到 OwnEnumerableStringKeys 自己那次分配（实测约 16 字节/键）：
	// 它一次把键全物化出来，而 moejs 没有给出「键有多少个」的廉价接口。
	// 那一份与插件的 JS 侧数据同量级，比容器这一份小一个数量级。
	keys := o.OwnEnumerableStringKeys()

	if o.ClassName() == arrayClassName {
		out := make([]any, 0, min(len(keys), fieldTotalMaxBytes/scalarCost))
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
			g, ok := valueToGo(elem, depth-1, budget)
			if !ok {
				continue
			}
			out = append(out, g)
		}
		return out, true
	}

	out := make(map[string]any, min(len(keys), fieldKeyMax))
	for _, k := range keys {
		// 数量到顶：后面的键**不再看**。这与下面「这个条目放不下」不是同一个判据，
		// 所以一个是 break、一个是 continue。
		if len(out) >= fieldKeyMax {
			break
		}
		val, ok := o.GetOwnDataValue(k)
		if !ok {
			continue
		}
		// OwnEnumerableStringKeys 只给字符串键与下标键，GoString 两种都覆盖
		// （对象上写的 "0" 是下标键，字面上要还原成 "0"）。
		name := k.GoString()
		// **键也占日志的字节，所以也要过 spend。** 不记的话一个 1 MB 的键原样
		// 进日志（实测日志行 1000105 字节）：值与键都是「插件决定宿主为此花多少
		// 内存」的入口，而键还额外决定日志行的长度。
		if !budget.spend(len(name)) {
			// 键放不下：整个条目丢掉，与值一样——不截短键，也不记一个只剩键名的
			// 空壳（那在日志里比没有这个键还难读）。
			//
			// 这里是 continue 而不是 break：放不下的是**这个键**，不是后面的键。
			// （spend 失败会把剩余预算清零，所以后面实际上也放不进东西了；但那是
			// spend 的性质，不是这条判据的意思——两者不该合成一个。）
			continue
		}
		g, ok := valueToGo(val, depth-1, budget)
		if !ok {
			continue
		}
		out[name] = g
	}
	return out, true
}
