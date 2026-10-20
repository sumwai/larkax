package overrides

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// signatureOf 把请求上**可被覆盖的那三个字段**拼成一个字符串。
//
// 用它来判「这份覆盖到底动没动这次请求」，而不是逐个字段比对：逐个比对要求
// 用例自己也写一份「哪个键对应哪个字段」的表，而那张表正是要被钉住的东西
// ——它错了，两处会一起错。
func signatureOf(req *channel.Request) string {
	return fmt.Sprintf("temperature=%v top_p=%v max_tokens=%d",
		req.Temperature, req.TopP, req.MaxTokens)
}

// 白名单里每一个键都**真的**会被应用。
//
// 这条用例钉的是白名单与应用路径之间的那条缝：往白名单里加一个键、而 Apply
// 不认它，写入会成功、管理面会把它列出来，请求却一点没变——那是这一列此前
// 的缺陷形状（配了跟没配一样）。有了它，加键时忘了改 Apply 会立刻红。
//
// 样本值是手写的，所以新键没有样本时这里直接失败（而不是跳过它）：
// 「遍历一堆东西」的用例在集合为空或漏项时静默通过，是另一条踩过的坑。
func TestEveryAllowedKeyApplies(t *testing.T) {
	samples := map[string]float64{
		keyTemperature: 0.25,
		keyTopP:        0.75,
		keyMaxTokens:   4096,
	}

	for _, key := range allowedKeys {
		t.Run(key, func(t *testing.T) {
			sample, ok := samples[key]
			if !ok {
				t.Fatalf("白名单里的 %q 在这条用例里没有样本值：新加的键要在这里补一个，"+
					"否则「它是否真的会被应用」没有任何东西钉着", key)
			}

			req := &channel.Request{MaxTokens: 7}
			before := signatureOf(req)
			if err := Apply(req, map[string]any{key: sample}); err != nil {
				t.Fatalf("Apply 出错: %v", err)
			}
			if after := signatureOf(req); after == before {
				t.Errorf("覆盖 %s 之后请求没有变化（%s）：白名单里有这个键，而热路径不认它", key, before)
			}
		})
	}
}

// 采样类参数：**调用方给了就听调用方的**，没给才用覆盖值。
//
// 两条断言各钉一半：只听调用方（覆盖永远不生效）与覆盖总是赢（调用方设的值
// 被吞掉）都会让这条用例红，而它们在现象上完全不同——后者正是「我设的
// temperature 没生效」那类不可解释的现象。
func TestApplySamplingParametersLetCallerWin(t *testing.T) {
	override := map[string]any{keyTemperature: 0.9, keyTopP: 0.9}

	// 一、调用方没给 → 覆盖值生效。
	empty := &channel.Request{MaxTokens: 7}
	if err := Apply(empty, override); err != nil {
		t.Fatalf("Apply 出错: %v", err)
	}
	if empty.Temperature == nil || *empty.Temperature != 0.9 {
		t.Errorf("调用方没给 temperature 时 = %v，期望用覆盖值 0.9", empty.Temperature)
	}
	if empty.TopP == nil || *empty.TopP != 0.9 {
		t.Errorf("调用方没给 top_p 时 = %v，期望用覆盖值 0.9", empty.TopP)
	}

	// 二、调用方给了 → 调用方赢。temperature 的 0 是合法取值，也必须算「给了」。
	given := &channel.Request{MaxTokens: 7, Temperature: ptr(0.0), TopP: ptr(0.1)}
	if err := Apply(given, override); err != nil {
		t.Fatalf("Apply 出错: %v", err)
	}
	if *given.Temperature != 0 {
		t.Errorf("调用方给了 temperature=0 时变成了 %v，期望仍是 0（调用方赢）", *given.Temperature)
	}
	if *given.TopP != 0.1 {
		t.Errorf("调用方给了 top_p=0.1 时变成了 %v，期望仍是 0.1（调用方赢）", *given.TopP)
	}
}

// max_tokens 与采样参数相反：**覆盖值直接生效**，包括比调用方给的更大。
//
// 大的那一侧要单独钉：它决定预留上界必须跟着覆盖走（见 EffectiveMaxTokens）。
// 只钉「覆盖更小」的话，把 Apply 写成「取两者较小」也能通过，而那样一条配了
// 想把上限放宽的别名会静默不生效。
func TestApplyMaxTokensOverrideWins(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller int
		want   int
	}{
		{"覆盖更小", 1000, 256},
		{"覆盖更大", 100, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &channel.Request{MaxTokens: tc.caller}
			if err := Apply(req, map[string]any{keyMaxTokens: float64(tc.want)}); err != nil {
				t.Fatalf("Apply 出错: %v", err)
			}
			if req.MaxTokens != tc.want {
				t.Errorf("max_tokens = %d，期望覆盖值 %d（调用方给的是 %d）",
					req.MaxTokens, tc.want, tc.caller)
			}
		})
	}
}

// Apply 改的是**传进来的那一份**，不会顺着指针改到调用方那份原始请求上。
//
// 热路径把请求副本交给它（见 internal/route 的 upstreamRequest），而那边
// 之所以要副本，是因为同一份原始请求还要用来渲染响应与重试。这里钉住的是
// 这件事的另一半：temperature 是 *float64，写 `req.Temperature = &v` 与
// `*req.Temperature = v` 只差一个星号，而后者会顺着指针改到原始请求上
// ——浅拷贝之后两个字段指着同一块内存，现象是「调用方那份请求也被改了」。
//
// 原始请求的 temperature 留 nil，于是错写成 `*req.Temperature = v` 的那一版
// 直接空指针崩掉，而不是悄悄改掉调用方的值。
func TestApplyLeavesTheCallerRequestAlone(t *testing.T) {
	original := &channel.Request{MaxTokens: 100}
	attempt := *original // 与 route 一样：浅拷贝一份再改

	if err := Apply(&attempt, map[string]any{
		keyTemperature: 0.9,
		keyTopP:        0.9,
		keyMaxTokens:   4096,
	}); err != nil {
		t.Fatalf("Apply 出错: %v", err)
	}

	if original.Temperature != nil || original.TopP != nil || original.MaxTokens != 100 {
		t.Errorf("原始请求被改动了：temperature=%v top_p=%v max_tokens=%d，期望 nil / nil / 100",
			original.Temperature, original.TopP, original.MaxTokens)
	}
	if attempt.Temperature == nil || *attempt.Temperature != 0.9 {
		t.Errorf("副本上的 temperature = %v，期望 0.9（覆盖要写进副本）", attempt.Temperature)
	}
}

// 改变请求形状的键一律拒绝，而且错误里说得出是哪一个、为什么。
//
// 判据落在**错误文本**上，不只是 err != nil：写入被拒之后，配这条别名的人手上
// 只有这句话，而「invalid overrides」这种措辞会让他去猜是哪个键。
func TestValidateRejectsShapeChangingKeys(t *testing.T) {
	for _, key := range []string{"model", "messages", "tools", "stream"} {
		t.Run(key, func(t *testing.T) {
			err := Validate(map[string]any{key: "whatever"})
			if err == nil {
				t.Fatalf("覆盖 %q 被收下了，期望被拒", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("错误里没有说出是哪个键：%v", err)
			}
			if !strings.Contains(err.Error(), "不允许覆盖") {
				t.Errorf("错误里没有说明它为什么不能覆盖：%v", err)
			}
		})
	}
}

// 白名单之外的键（包括拼错的）也拒绝，并列出能用的那些。
func TestValidateRejectsUnknownKey(t *testing.T) {
	err := Validate(map[string]any{"temperatur": 0.5})
	if err == nil {
		t.Fatal("拼错的键被收下了，期望被拒")
	}
	if !strings.Contains(err.Error(), "temperatur") {
		t.Errorf("错误里没有说出是哪个键：%v", err)
	}
	for _, allowed := range allowedKeys {
		if !strings.Contains(err.Error(), allowed) {
			t.Errorf("错误里没有列出允许的键 %q：%v", allowed, err)
		}
	}
}

// 报错里那一串白名单**逐字**都对：键名、顺序、顿号的位置。
//
// 三样都出自同一句拼接（allowedList），坏法却各不相同：少一个键、换了顺序、
// 在第一个键前面多一个顿号。最后一种是 `i > 0` 写错一格的结果，而
// 三种坏法在报错文本里都只显得「那句话有点别扭」——别处不会因此报错。
//
// 白名单那一串写成字面量：从 allowedKeys 推出来的期望会跟着缺陷一起漂移。
// 理由文案不钉（改措辞不该让这条红），它开头那几句由 contains 管。
// 「不在白名单」与「显式拒绝」是两条不同的路径，各自都要带上这一串。
func TestUnknownKeyErrorSpellsTheAllowedKeysExactly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    map[string]any
		contains []string
	}{
		{
			name:     "不在白名单",
			value:    map[string]any{"temperatur": 0.5},
			contains: []string{`"temperatur"`, "不在允许覆盖的字段里"},
		},
		{
			name:     "显式拒绝",
			value:    map[string]any{"model": "x"},
			contains: []string{`"model"`, "不允许覆盖"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.value)
			if err == nil {
				t.Fatalf("%v 被收下了，期望被拒", tc.value)
			}
			got := err.Error()
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("错误文本 = %s，期望里面说得出 %s", got, want)
				}
			}
			tail := "（允许覆盖的只有 temperature、top_p、max_tokens）"
			if !strings.HasSuffix(got, tail) {
				t.Errorf("错误文本 = %s，期望以 %s 结尾", got, tail)
			}
		})
	}
}

// NaN 与 ±Inf 一律拒，而且各自说得出是哪种。
//
// 这几个值走 JSON 进不来，但管理面的 Go 调用方给得出来。放过去之后两条路
// 的表现不同：temperature / top_p 会被**悄悄收下**，热路径照着发出去；
// max_tokens 仍会被后面几道界挡住，但报的是「必须是整数」「必须为正」
// 「超出可精确表示的整数范围」——看着像取值太大，看不出根因不是有限数。
//
// 三条子用例各有分工：`||` 写成 `&&` 时，NaN 与 Inf 不会同时成立，NaN 这一
// 条就能杀；只留下一个操作数时，漏掉的正是另一类——只留 IsNaN 时两条 Inf
// 子用例红，只留 IsInf 时 NaN 子用例红。
func TestValidateRejectsNonFiniteNumbers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value float64
		want  string
	}{
		{"NaN", math.NaN(), `overrides 里的 "temperature" 不是有限数（NaN）`},
		{"正无穷", math.Inf(1), `overrides 里的 "temperature" 不是有限数（+Inf）`},
		{"负无穷", math.Inf(-1), `overrides 里的 "temperature" 不是有限数（-Inf）`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(map[string]any{keyTemperature: tc.value})
			if err == nil {
				t.Fatalf("temperature = %v 被收下了，期望被拒", tc.value)
			}
			if got := err.Error(); got != tc.want {
				t.Errorf("错误文本 = %s\n期望 = %s", got, tc.want)
			}
		})
	}
}

// 可精确表示的整数范围**闭**在上界：恰好 2^53 收下，再上面一格拒。
//
// `>` 写成 `>=` 会把 2^53 也拒掉，而它正是 float64 能精确表示的最大整数，
// 落在合法的那一侧。上界的下一格不能写 2^53+1：它在 float64 里就等于 2^53，
// 造不出来，所以用 2^53+2（float64 在这个量级的步长是 2）。
func TestMaxTokensAtExactIntegerBoundary(t *testing.T) {
	const maxExact = 9007199254740992 // 2^53

	got, err := EffectiveMaxTokens(100, map[string]any{keyMaxTokens: float64(maxExact)})
	if err != nil {
		t.Fatalf("max_tokens = 2^53 被拒了: %v，期望收下", err)
	}
	if got != maxExact {
		t.Errorf("EffectiveMaxTokens = %d，期望 2^53 = %d", got, maxExact)
	}

	err = Validate(map[string]any{keyMaxTokens: 9007199254740994.0}) // 2^53+2
	if err == nil {
		t.Fatal("max_tokens = 2^53+2 被收下了，期望被拒")
	}
	// 只钉「拒绝的理由是超出精确范围」这一句：浮点数的格式化是 Go 的事，
	// 不是这个判据的行为，钉住它会因为换一种 %v 写法而红。
	if want := "超出可精确表示的整数范围"; !strings.Contains(err.Error(), want) {
		t.Errorf("错误 = %v，期望里面说明 %q", err, want)
	}
}

// 值本身不合法的一律拒绝：非数字、小数型的 max_tokens、0 / 负数、
// 超出可精确表示的范围。这几条都是**写入时**才拦得住的错误——它们
// 进了库之后，表现是这个别名上的每个请求都失败，而原因看起来在上游。
func TestValidateRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value map[string]any
		want  string
	}{
		{"temperature 不是数字", map[string]any{keyTemperature: "0.5"}, "不是数字"},
		{"top_p 是布尔", map[string]any{keyTopP: true}, "不是数字"},
		{"max_tokens 是小数", map[string]any{keyMaxTokens: 1.5}, "必须是整数"},
		{"max_tokens 是 0", map[string]any{keyMaxTokens: 0.0}, "必须为正"},
		{"max_tokens 是负数", map[string]any{keyMaxTokens: -1.0}, "必须为正"},
		{"max_tokens 超出可表示范围", map[string]any{keyMaxTokens: 1e17}, "超出可精确表示的整数范围"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.value)
			if err == nil {
				t.Fatalf("%v 被收下了，期望被拒", tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %v，期望里面说明 %q", err, tc.want)
			}
		})
	}
}

// 空对象与 nil 都是「没配覆盖」，必须收下。
//
// 清空这一栏用的就是空对象（`larkaxctl alias update --overrides ”`），
// 把它拒掉等于这一栏再也清不掉。
func TestValidateAcceptsEmpty(t *testing.T) {
	for _, v := range []map[string]any{nil, {}} {
		if err := Validate(v); err != nil {
			t.Errorf("Validate(%v) 出错: %v，期望通过", v, err)
		}
	}
}

// 数字的各种 Go 类型都收得下。
//
// 值走一遍 jsonb 之后一律是 float64，但直接调管理面写入路径的代码（包括用例
// 自己）传的是 int：那种调用方拿到「不是数字」是纯粹的意外，而它会把一次本来
// 正确的配置拦在门外。要拒的是非数字，见 TestValidateRejectsBadValues。
func TestValidateAcceptsEveryNumericGoType(t *testing.T) {
	for _, raw := range []any{
		float64(1), float32(1), int(1), int8(1), int16(1), int32(1), int64(1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1),
	} {
		if err := Validate(map[string]any{keyMaxTokens: raw}); err != nil {
			t.Errorf("max_tokens = %v（%T）被拒了: %v", raw, raw, err)
		}
	}
}

// 多个非法键时，报哪个是**确定**的。
//
// map 的遍历顺序是随机的：不排序的话，同一条配置两次给出两条不同的错误消息，
// 而用例断言的是错误文本——那会变成一条偶发失败的用例。
func TestValidateReportsTheSameKeyEveryTime(t *testing.T) {
	v := map[string]any{"zzz": 1, "aaa": 2}
	first := Validate(v)
	if first == nil {
		t.Fatal("非法键被收下了")
	}
	for i := 0; i < 20; i++ {
		again := Validate(v)
		if again == nil || again.Error() != first.Error() {
			t.Fatalf("第 %d 次给出的错误是 %v，第一次是 %v", i+1, again, first)
		}
	}
	if !strings.Contains(first.Error(), "aaa") {
		t.Errorf("错误 = %v，期望报排序后的第一个键 aaa", first)
	}
}

// 覆盖之后的最大输出：没有覆盖时就是调用方那个数，有覆盖时是覆盖值。
func TestEffectiveMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller int
		value  map[string]any
		want   int
	}{
		{"没配覆盖", 100, nil, 100},
		{"配了别的参数", 100, map[string]any{keyTemperature: 0.5}, 100},
		{"覆盖更小", 100, map[string]any{keyMaxTokens: 64.0}, 64},
		{"覆盖更大", 100, map[string]any{keyMaxTokens: 4096.0}, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EffectiveMaxTokens(tc.caller, tc.value)
			if err != nil {
				t.Fatalf("EffectiveMaxTokens 出错: %v", err)
			}
			if got != tc.want {
				t.Errorf("EffectiveMaxTokens = %d，期望 %d", got, tc.want)
			}
		})
	}
}

// 非法覆盖在取上界这一路上也报错，不悄悄退回调用方那个数。
//
// 退回的话，这条配置的表现是「上界按调用方算、发出去的却是覆盖值」，而两者
// 不一致时结算会撞上界——一次配置错误变成一次少收，且没有任何地方说得出原因。
func TestEffectiveMaxTokensPropagatesBadOverrides(t *testing.T) {
	if _, err := EffectiveMaxTokens(100, map[string]any{"messages": "x"}); err == nil {
		t.Fatal("非法覆盖被当成「没配覆盖」了，期望报错")
	}
}

func ptr(f float64) *float64 { return &f }
