package snapshot

import (
	"strings"
	"testing"
)

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 来自变异测试（gremlins unleash）：`internal/snapshot` 的 Test efficacy
// 是 66%，是已跑的包里最低的。
//
// 这个包是「不留内容」这条承诺的**执行者**——它的分支没被钉住，
// 比别处的分支没被钉住更要紧：别处错了会算错钱，这里错了会把用户的
// 原文写进库而没有任何信号。

// charCount 对字符串数的是 **rune** 而不是字节。
//
// 变异：`len([]rune(s))` 改成 `len(s)`。中文与 emoji 的长度会变成字节数，
// 而「长度」是骨架里唯一保留的量化信息——它错了，排查时的判断就错了。
func TestCharCountCountsRunesNotBytes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"空串", "", 0},
		{"ASCII", "hello", 5},
		{"中文四个字", "网络网关", 4},
		{"emoji 一个", "🎉", 1},
		{"ASCII 与中文与 emoji 混合", "a中🎉b", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := charCount(tc.in)
			if !ok {
				t.Fatal("应当数得出来")
			}
			if got != tc.want {
				t.Errorf("charCount(%v) = %d，期望 %d", tc.in, got, tc.want)
			}
			// 按字节数算的话中文会得到 3 倍——这条断言把两种算法分开
			if s, isStr := tc.in.(string); isStr && len(s) != tc.want && got == len(s) {
				t.Errorf("结果是按字节数的（len=%d，rune 数=%d）", len(s), tc.want)
			}
		})
	}
}

// nil 与不可序列化的值：前者是 0 且成功，后者是 0 且失败。
//
// 变异：`if v == nil` 去掉，nil 会走 json.Marshal 得到 `null` 四个字符——
// 于是「没有这个字段」与「字段是 null」在骨架里分不开。
func TestCharCountNilAndUnserializable(t *testing.T) {
	if n, ok := charCount(nil); n != 0 || !ok {
		t.Errorf("nil 应当是 (0, true)，得到 (%d, %v)", n, ok)
	}
	// 函数值无法序列化
	if _, ok := charCount(func() {}); ok {
		t.Error("无法序列化的值应当返回 ok=false")
	}
}

// 非字符串值按 JSON 序列化后数 rune。
//
// 变异：去掉 json.Marshal 那条路径，容器类型的长度会算错。
func TestCharCountSerializesContainers(t *testing.T) {
	got, ok := charCount(map[string]any{"a": 1})
	if !ok || got != 7 {
		t.Errorf("map 的字符数 = %d/%v，期望 7（`{\"a\":1}`）", got, ok)
	}
	got, ok = charCount([]any{1, 2, 3})
	if !ok || got != 7 {
		t.Errorf("数组的字符数 = %d/%v，期望 7（`[1,2,3]`）", got, ok)
	}
}

// 文本占位符的形状是 `{"_t":"text","chars":N}`。
//
// 变异：键名或类型名被改，对账与排查的判据就变了；chars 用字节数则
// 中文的长度会是 3 倍。
func TestTextPlaceholderShape(t *testing.T) {
	m := textPlaceholder("网络网关")
	if m[fieldType] != typeText {
		t.Errorf("_t = %v，期望 %s", m[fieldType], typeText)
	}
	if m[fieldChars] != 4 {
		t.Errorf("chars = %v，期望 4（rune 数而不是字节数）", m[fieldChars])
	}
}

// 二进制占位保留块类型与媒体类型，**但不留数据本身**。
//
// 变异：把 data 带进结果就是泄露；去掉 type 则块类型序列读不出来，
// 而「第几块是图片、第几块是文本」正是复现时要看的第一件事。
func TestBlobPlaceholderKeepsMetadataNotData(t *testing.T) {
	const data = "QUJDREVGR0g=" // 假的 base64，不是任何真实内容

	m := blobPlaceholder("image", "image/png", data)

	if m["type"] != "image" {
		t.Errorf("块类型丢了: %v", m)
	}
	if m["media_type"] != "image/png" {
		t.Errorf("媒体类型丢了: %v", m)
	}
	// 数据本身绝不能出现
	if rendered := renderAny(m); strings.Contains(rendered, data) {
		t.Errorf("占位里带了原始数据: %s", rendered)
	}
	if _, has := m[fieldBytes]; !has {
		t.Errorf("应保留字节数: %v", m)
	}
}

// 二进制占位的字节数按 base64 **解码后**算，不是编码后的字符数。
//
// 变异：`decodedLen(data)` 改成 `len(data)`，字节数会比真实值大约 1/3，
// 于是「上传的图到底多大」对不上账。
func TestBlobPlaceholderBytesAreDecoded(t *testing.T) {
	// "QUJD" 解码是 "ABC" 三个字节，而编码后是 4 个字符
	m := blobPlaceholder("image", "", "QUJD")
	if got := m[fieldBytes]; got != 3 {
		t.Errorf("字节数 = %v，期望 3（解码后的长度，不是编码字符数 4）", got)
	}
}

// 空媒体类型时不写出 media_type 键，而不是写一个空串。
//
// 变异：去掉 `if mediaType != ""` 判断，骨架里会多出一个空值键——
// 而空值键会被读成「媒体类型是空的」。
func TestBlobPlaceholderOmitsEmptyMediaType(t *testing.T) {
	m := blobPlaceholder("image", "", "QUJD")
	if _, has := m["media_type"]; has {
		t.Errorf("空媒体类型不该写出这个键: %v", m)
	}
}

// 形状描述符对容器给出「类型 + 元素/键数量 + 已脱敏」，而不是空容器。
//
// 变异：`len(t)` 改成 0 或常量，「这里有几个元素」这条信息就没了；
// 而归零会被读成「这里本来就没有东西」。
func TestShapeOfContainersKeepCounts(t *testing.T) {
	obj := shapeOf(map[string]any{"a": 1, "b": 2, "c": 3})
	if obj[fieldType] != typeObject || obj["keys"] != 3 {
		t.Errorf("对象形状 = %v，期望 type=object keys=3", obj)
	}
	if obj["redacted"] != true {
		t.Errorf("应标记为已脱敏: %v", obj)
	}

	arr := shapeOf([]any{1, 2, 3, 4})
	if arr[fieldType] != typeArray || arr["items"] != 4 {
		t.Errorf("数组形状 = %v，期望 type=array items=4", arr)
	}
}

// nil 的形状是 null，而不是空对象。
//
// 变异：把 nil 的分支并到 default，它会变成 describeScalar(nil)——
// 而「字段是 null」与「字段是某种标量」是两回事。
func TestShapeOfNil(t *testing.T) {
	m := shapeOf(nil)
	if m[fieldType] != typeNull {
		t.Errorf("nil 的形状 = %v，期望 type=null", m)
	}
}

// isScalar 对 nil / bool / string / 各类数字都返回 true，
// 对容器返回 false。
//
// 变异：把 `case nil, bool, string` 里的某一项去掉，那种标量会被
// 当成容器处理——而 `isScalar` 的调用点用它决定「这个值是内容还是形状」。
func TestIsScalarCoversAllJSONScalars(t *testing.T) {
	scalars := []any{nil, true, false, "s", 1, int8(1), int16(1), int32(1), int64(1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1), 1.5, float32(1.5)}
	for _, v := range scalars {
		if !isScalar(v) {
			t.Errorf("%T 是标量，isScalar 却返回 false", v)
		}
	}
	containers := []any{map[string]any{}, []any{}, struct{}{}}
	for _, v := range containers {
		if isScalar(v) {
			t.Errorf("%T 不是标量，isScalar 却返回 true", v)
		}
	}
}

// 每种标量都有确定的类型名，未知类型退到 unknown 而不是 panic。
//
// 变异：把某个 case 去掉，那种数字会被报成 unknown——
// 而描述符里的类型名是骨架里唯一留下的取值线索。
func TestScalarKindNames(t *testing.T) {
	for _, v := range []any{1, int8(1), int64(1), uint(1), uint64(1), 1.5, float32(1.5)} {
		if got := scalarKind(v); got != typeNumber {
			t.Errorf("%T 的类型名 = %q，期望 %q", v, got, typeNumber)
		}
	}
	// 结构体不是标量，落到 unknown 而不是 panic
	if got := scalarKind(struct{}{}); got != typeUnknown {
		t.Errorf("未知类型 = %q，期望 %q", got, typeUnknown)
	}
}

// redactedMask 每次返回**副本**，不共享同一个 map。
//
// 变异：改成返回包级变量，一处改动会出现在所有字段上——
// 而骨架会被调用方拿去改与序列化。
func TestRedactedMaskIsCopied(t *testing.T) {
	a := redactedMask()
	b := redactedMask()
	a["injected"] = "改了"

	if _, leaked := b["injected"]; leaked {
		t.Error("两次调用共享了同一个 map：一处改动漏到了另一处")
	}
}

// 短哈希：同样的输入给同样的输出，不同的输入给不同的输出，且长度固定。
//
// 变异：把哈希算法或截断长度改掉，同一份请求两次脱敏的头部指纹会不同,
// 而「同一个头名在两次快照里可关联」正是短哈希存在的理由。
func TestShortHashIsStableAndDistinct(t *testing.T) {
	a1 := shortHash("x-custom-header")
	a2 := shortHash("x-custom-header")
	if a1 != a2 {
		t.Errorf("同样的输入给了不同输出：%q vs %q", a1, a2)
	}
	if shortHash("x-other-header") == a1 {
		t.Error("不同的输入不该给同样的输出")
	}
	if len(a1) == 0 {
		t.Error("哈希不该为空")
	}
	// 长度固定：骨架里同一列的宽度一致，比对脚本才好写
	if len(shortHash("a")) != len(shortHash("a-much-longer-header-name")) {
		t.Error("哈希长度应当固定")
	}
}

// renderAny 把任意骨架渲染成字符串，供「某个子串是否出现」的断言用。
func renderAny(v any) string {
	var sb strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				sb.WriteString(k)
				sb.WriteString(":")
				walk(val)
				sb.WriteString(",")
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		case string:
			sb.WriteString(t)
		case nil:
		default:
			sb.WriteString(scalarKind(x))
		}
	}
	walk(v)
	return sb.String()
}

// ── #675：整包变异存活条目的判据 ─────────────────────────────────────────

// decodedLen 只在认得出 base64 时给解码长度，其余退回编码长度。
//
// 变异（同一处两条）：
//   - `trimmed != "" && isBase64Text(trimmed)` 的 `&&` 翻成 `||`：含标点的
//     普通文本（不是 base64）也会走解码长度，`abc!` 报 3 而不是 4；
//   - isBase64Text 的循环边界 `<` 翻成 `>=`：循环一次都不走、函数恒为 true，
//     同样把非 base64 的取值当成 base64 解码。
//
// 两条都要一个「看着是文本、但落在 base64 字符集外」的取值才分得开；
// `QUJD` 这种真 base64 两版答案一样。
func TestDecodedLenOnlyForBase64(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"QUJD", 3},  // 4 个编码字符 → 3 字节
		{"abcd", 3},  // 同上，字符集内即可
		{"abc!", 4},  // `!` 不在字符集里：退回编码长度
		{"====", 4},  // 全是填充：认不出，退回编码长度
		{"a b c", 5}, // 空格不在字符集里
	}
	for _, tc := range cases {
		if got := decodedLen(tc.in); got != tc.want {
			t.Errorf("decodedLen(%q) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// isEnumWord 的三条拒绝理由各钉在边界上：空串、超长、非 UTF-8。
//
// 变异（同一行上三条）：
//   - 第一个 `||` 翻成 `&&`：`s == ""` 那一格丢了，空串会一路走到循环末尾
//     返回 true——空的 code 于是被当成枚举词写进骨架；
//   - `len(s) > 64` 翻成 `>= 64`：恰好 64 个字符的枚举词被拒；
//   - 第二个 `||` 翻成 `&&`：只有「超长**且**非法 UTF-8」才拒，超过 64 个
//     字符的正常枚举词照样放行。
func TestIsEnumWordBoundaries(t *testing.T) {
	if isEnumWord("") {
		t.Error("空串不是枚举词")
	}
	if !isEnumWord(strings.Repeat("a", 64)) {
		t.Error("64 个字符仍在长度上限之内")
	}
	if isEnumWord(strings.Repeat("a", 65)) {
		t.Error("超过 64 个字符不再是枚举词")
	}
	// 非 ASCII 的内容（哪怕不到 64 个字符）也不放行
	if isEnumWord("用户的名字") {
		t.Error("含非 ASCII 字符的取值不是枚举词")
	}
}
