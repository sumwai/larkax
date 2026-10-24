package expr

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
)

// 这一组用例检查「根位置三元两支、`??` 两侧的 token 计价项必须一致」。
//
// 理由见 compile.go 的 checkBranchTokenKeys：Program.Keys 取所有分支的并集，
// 而结算只认求值到的那一侧的 Declared。两侧键集不同时，price list 与写入提示会
// 显示「覆盖齐全」，用量带上了只在另一侧出现的项就整笔 ErrMissingPrice。
// 两种写法同一条判据，只有报错里的叫法不同（三元：真分支/假分支；`??`：左侧/右侧）。

// TestCompileRejectsBranchTokenKeyMismatch 验证两支键集不同时编译失败，
// 且报错逐句点明哪个键、在哪一支里缺席。
func TestCompileRejectsBranchTokenKeyMismatch(t *testing.T) {
	cases := []struct {
		name   string
		source string
		// sentences 是错误里必须**逐字**出现的那几句。逐字而不是
		// 「两个键都出现」：两句对调、或者多拼一句没有主语的「键  只在…」，
		// 宽松断言都看不出来。
		sentences []string
		// missingInTrue / missingInFalse 为 true 表示「真分支里缺席」/
		// 「假分支里缺席」那一句该出现；为 false 时那一侧没有独有键，
		// 对应那半句一个字都不许出现。
		missingInTrue  bool
		missingInFalse bool
	}{
		{
			// 最典型的一条：短上下文那一支写了输出单价，长上下文那一支没写。
			name:           "只在真分支出现的输出单价",
			source:         `c > 100 ? {"prompt": 1.5, "completion": 7.5} : {"prompt": 1.5}`,
			sentences:      []string{`键 "completion" 只在真分支里出现，假分支里缺席`},
			missingInFalse: true,
		},
		{
			// 方向反过来也拦：判据不能只查「真支是假支的超集」。
			name:          "只在假分支出现的输出单价",
			source:        `c > 100 ? {"prompt": 1.5} : {"prompt": 1.5, "completion": 7.5}`,
			sentences:     []string{`键 "completion" 只在假分支里出现，真分支里缺席`},
			missingInTrue: true,
		},
		{
			// 两边各缺一个：两个方向的句子都要出现，两个键都要点名。
			name:   "两支各有对方没有的键",
			source: `c > 100 ? {"prompt": 1.5, "completion": 7.5} : {"prompt": 1.5, "reasoning": 9.0}`,
			sentences: []string{
				`键 "completion" 只在真分支里出现，假分支里缺席`,
				`键 "reasoning" 只在假分支里出现，真分支里缺席`,
			},
			missingInTrue:  true,
			missingInFalse: true,
		},
		{
			// 分支里再分三元：遗漏落在内层那一对上，报错也指那一对。
			name: "嵌套三元的分支里再分叉",
			source: `len <= 1000
				? (len <= 100
					? {"prompt": 1.0}
					: {"prompt": 1.0, "completion": 2.0})
				: {"prompt": 1.0, "completion": 2.0}`,
			sentences:     []string{`键 "completion" 只在假分支里出现，真分支里缺席`},
			missingInTrue: true,
		},
		{
			// 外层一致、内层不一致：一致性要逐层校验，不能只看最外层。
			name: "外层两支一致但内层不一致",
			source: `len <= 1000
				? (len <= 100
					? {"prompt": 1.0, "completion": 2.0}
					: {"prompt": 1.0})
				: {"prompt": 1.0, "completion": 2.0}`,
			sentences:      []string{`键 "completion" 只在真分支里出现，假分支里缺席`},
			missingInFalse: true,
		},
		{
			// tier 是透明的：它包住的表照样参与比较。
			name: "tier 包住的两支键集不同",
			source: `len <= 100
				? tier("standard", {"prompt": 1.0})
				: tier("long_context", {"prompt": 1.0, "cache_read": 0.5})`,
			sentences:     []string{`键 "cache_read" 只在假分支里出现，真分支里缺席`},
			missingInTrue: true,
		},
		{
			// 分支是括号里的语句序列：结果在最后一项上。
			name: "分支里的语句序列",
			source: `c > 100
				? (1.0; {"prompt": 1.0})
				: {"prompt": 1.0, "completion": 2.0}`,
			sentences:     []string{`键 "completion" 只在假分支里出现，真分支里缺席`},
			missingInTrue: true,
		},
		{
			// let 只是包了一层：里面的表照样参与比较，不能「见 let 就放过」。
			name: "let 包住的表与另一支键集不同",
			source: `c > 100
				? (let x = 1.0; {"prompt": 1.5, "completion": 2.0})
				: {"prompt": 2.0}`,
			sentences:      []string{`键 "completion" 只在真分支里出现，假分支里缺席`},
			missingInFalse: true,
		},
		{
			// let 表达式以三元收尾时同样在根位置。
			name: "let 收尾的三元",
			source: `let base = 1.0;
				c > 100 ? {"prompt": base, "completion": 2.0} : {"prompt": base}`,
			sentences:      []string{`键 "completion" 只在真分支里出现，假分支里缺席`},
			missingInFalse: true,
		},
		{
			// 分支是一个 let 绑定的名字：要按绑定的表取键，别当成空集。
			name: "绑定的表在真分支与另一支键集不同",
			source: `let t = {"prompt": 1.0};
				c > 100 ? {"prompt": 2.0, "completion": 3.0} : t`,
			sentences:      []string{`键 "completion" 只在真分支里出现，假分支里缺席`},
			missingInFalse: true,
		},
		{
			// 绑定在假分支那一侧同样要解析，否则这一支会被当成空集。
			name: "绑定的表在假分支与另一支键集不同",
			source: `let t = {"prompt": 1.0, "completion": 3.0};
				c > 100 ? {"prompt": 2.0} : t`,
			sentences: []string{
				`键 "completion" 只在假分支里出现，真分支里缺席`,
			},
			missingInTrue: true,
		},
		{
			// 两支各写一个同名 let：绑定是按名字存在同一张表里的，后走的那支
			// 会盖掉先走的那支。取键的时点必须在各支走完、另一支还没走的时候，
			// 否则两支都会解析成后登记的那张表，看起来就一致了。
			name: "两支各写同名的 let 但表不同",
			source: `len <= 100
				? (let price = {"prompt": 1.5, "completion": 7.5}; price)
				: (let price = {"prompt": 3.0}; price)`,
			sentences:      []string{`键 "completion" 只在真分支里出现，假分支里缺席`},
			missingInFalse: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compileErr(t, tc.source)
			if err.Reason != ReasonShape {
				t.Errorf("原因 = %s，期望 %s（%v）", err.Reason, ReasonShape, err.Err)
			}
			msg := err.Error()
			for _, sentence := range tc.sentences {
				if !strings.Contains(msg, sentence) {
					t.Errorf("错误里应当逐字出现 %q，得到 %q", sentence, msg)
				}
			}
			if tc.missingInFalse && !strings.Contains(msg, "假分支里缺席") {
				t.Errorf("错误里应当说清它在假分支里缺席，得到 %q", msg)
			}
			if tc.missingInTrue && !strings.Contains(msg, "真分支里缺席") {
				t.Errorf("错误里应当说清它在真分支里缺席，得到 %q", msg)
			}
			// 没有差异的那一侧，对应那半句一个字都不许出现：判据若写成
			// 「某一侧有没有差」之外的东西（例如 `len(...) > 0` 改成 `>= 0`），
			// 空的那一侧会被拼成一句没有主语的「键  只在…出现」。
			if !tc.missingInFalse && strings.Contains(msg, "只在真分支里出现") {
				t.Errorf("真分支这侧没有独有键，错误里不该出现这一句，得到 %q", msg)
			}
			if !tc.missingInTrue && strings.Contains(msg, "只在假分支里出现") {
				t.Errorf("假分支这侧没有独有键，错误里不该出现这一句，得到 %q", msg)
			}
			// 只说「不一致」没法照着改。
			if !strings.Contains(msg, "显式写 0") {
				t.Errorf("错误里应当说清不用的那一项怎么写，得到 %q", msg)
			}
		})
	}
}

// TestCompileRejectsCoalesceTokenKeyMismatch 验证根位置 `??` 两侧键集不同时
// 编译失败，且报错按「左侧/右侧」点名，不借用三元那套「真分支/假分支」。
func TestCompileRejectsCoalesceTokenKeyMismatch(t *testing.T) {
	cases := []struct {
		name   string
		source string
		// sentences 是必须**逐字**出现的那几句。
		sentences []string
		// onlyOnLeft / onlyOnRight 表示有键只在那一侧出现（另一侧缺席）。
		// 为 false 时那一侧的句子一个字都不许出现。
		onlyOnLeft  bool
		onlyOnRight bool
	}{
		{
			// 最典型的一条：右侧写了输出单价，左侧没写。
			name:        "只在右侧出现的输出单价",
			source:      `{"prompt": 1.0} ?? {"prompt": 1.0, "completion": 2.0}`,
			sentences:   []string{`键 "completion" 只在右侧里出现，左侧里缺席`},
			onlyOnRight: true,
		},
		{
			// 方向反过来也拦。
			name:       "只在左侧出现的输出单价",
			source:     `{"prompt": 1.0, "completion": 2.0} ?? {"prompt": 1.0}`,
			sentences:  []string{`键 "completion" 只在左侧里出现，右侧里缺席`},
			onlyOnLeft: true,
		},
		{
			// 两侧各缺一个：两个方向的句子都要出现。
			name:   "两侧各有对方没有的键",
			source: `{"prompt": 1.0, "completion": 7.5} ?? {"prompt": 1.5, "reasoning": 9.0}`,
			sentences: []string{
				`键 "completion" 只在左侧里出现，右侧里缺席`,
				`键 "reasoning" 只在右侧里出现，左侧里缺席`,
			},
			onlyOnLeft:  true,
			onlyOnRight: true,
		},
		{
			// let 只是包了一层：绑定在一侧的表照样参与比较。
			name:       "左侧是 let 绑定的表",
			source:     `let t = {"prompt": 1.0, "completion": 2.0}; t ?? {"prompt": 3.0}`,
			sentences:  []string{`键 "completion" 只在左侧里出现，右侧里缺席`},
			onlyOnLeft: true,
		},
		{
			// 两侧各写一个同名 let：取键的时点必须在右侧还没登记的时候，
			// 否则两侧都会被解析成后登记的那张表（见 compile.go 的注释）。
			name: "两侧各写同名的 let 但表不同",
			source: `(let price = {"prompt": 1.5, "completion": 7.5}; price)
				?? (let price = {"prompt": 3.0}; price)`,
			sentences:  []string{`键 "completion" 只在左侧里出现，右侧里缺席`},
			onlyOnLeft: true,
		},
		{
			// 连着写解析成左结合的嵌套 `??`：最外层比较的是 `(a ?? b)` 与 c，
			// 左侧取键要走得进内层那个 BinaryNode。
			name:   "链式 ?? 的末段键集不同",
			source: `{"prompt": 1.0} ?? {"prompt": 2.0} ?? {"completion": 3.0}`,
			sentences: []string{
				`键 "prompt" 只在左侧里出现，右侧里缺席`,
				`键 "completion" 只在右侧里出现，左侧里缺席`,
			},
			onlyOnLeft:  true,
			onlyOnRight: true,
		},
		{
			// 三元的一支里再分 `??`：遗漏落在内层那一对上，报错也用它那一套叫法。
			name:   "三元分支里的 ??",
			source: `c > 100 ? {"prompt": 1.0} : ({"prompt": 2.0} ?? {"completion": 3.0})`,
			sentences: []string{
				`键 "prompt" 只在左侧里出现，右侧里缺席`,
				`键 "completion" 只在右侧里出现，左侧里缺席`,
			},
			onlyOnLeft:  true,
			onlyOnRight: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := compileErr(t, tc.source)
			if err.Reason != ReasonShape {
				t.Errorf("原因 = %s，期望 %s（%v）", err.Reason, ReasonShape, err.Err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "根位置的 ?? 两侧必须给同一组 token 计价项") {
				t.Errorf("错误里应当点明这是 ?? 两侧的判据，得到 %q", msg)
			}
			for _, sentence := range tc.sentences {
				if !strings.Contains(msg, sentence) {
					t.Errorf("错误里应当逐字出现 %q，得到 %q", sentence, msg)
				}
			}
			// 有独有键的那一侧既要点名「只在…里出现」，也要说清另一侧缺席；
			// 没有独有键的那一侧，这两句一个字都不许出现。
			if tc.onlyOnLeft != strings.Contains(msg, "只在左侧里出现") {
				t.Errorf("onlyOnLeft = %v，但错误里「只在左侧里出现」的出现与否不符，得到 %q",
					tc.onlyOnLeft, msg)
			}
			if tc.onlyOnLeft != strings.Contains(msg, "右侧里缺席") {
				t.Errorf("onlyOnLeft = %v，但错误里「右侧里缺席」的出现与否不符，得到 %q",
					tc.onlyOnLeft, msg)
			}
			if tc.onlyOnRight != strings.Contains(msg, "只在右侧里出现") {
				t.Errorf("onlyOnRight = %v，但错误里「只在右侧里出现」的出现与否不符，得到 %q",
					tc.onlyOnRight, msg)
			}
			if tc.onlyOnRight != strings.Contains(msg, "左侧里缺席") {
				t.Errorf("onlyOnRight = %v，但错误里「左侧里缺席」的出现与否不符，得到 %q",
					tc.onlyOnRight, msg)
			}
			// `??` 的报错不借用三元的叫法：两个词都不该出现。
			if strings.Contains(msg, "真分支") || strings.Contains(msg, "假分支") {
				t.Errorf("?? 的报错不该出现三元的「真分支/假分支」，得到 %q", msg)
			}
			// 只说「不一致」没法照着改。
			if !strings.Contains(msg, "显式写 0") {
				t.Errorf("错误里应当说清不用的那一项怎么写，得到 %q", msg)
			}
		})
	}
}

// TestCoalesceBranchKeepsTernaryWording 验证三元嵌在 `??` 一侧时，
// 报错仍是三元那一套叫法：两种判据共用同一个比较函数，但措辞要各说各的。
func TestCoalesceBranchKeepsTernaryWording(t *testing.T) {
	err := compileErr(t, `(c > 100 ? {"prompt": 1.0} : {"prompt": 2.0, "completion": 3.0})
		?? {"prompt": 4.0}`)
	if err.Reason != ReasonShape {
		t.Errorf("原因 = %s，期望 %s（%v）", err.Reason, ReasonShape, err.Err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `键 "completion" 只在假分支里出现，真分支里缺席`) {
		t.Errorf("错误里应当逐字出现三元那一句，得到 %q", msg)
	}
	if !strings.Contains(msg, "根位置的三元两支必须给同一组 token 计价项") {
		t.Errorf("错误里应当点明这是三元两支的判据，得到 %q", msg)
	}
	if strings.Contains(msg, "?? 两侧") {
		t.Errorf("三元自己的报错不该出现 ?? 那一套措辞，得到 %q", msg)
	}
}

// TestCompileAcceptsCoalesceTokenKeyUniformity 验证 `??` 两侧键集一致时照常
// 编译，并且与三元同一条边界：units 与一次性费用可以只在一侧。
func TestCompileAcceptsCoalesceTokenKeyUniformity(t *testing.T) {
	for name, source := range map[string]string{
		"键集一致的对照规则":   `{"prompt": 1.5, "completion": 7.5} ?? {"prompt": 3.0, "completion": 15.0}`,
		"两侧单价完全相同":    `{"prompt": 1.5, "completion": 7.5} ?? {"prompt": 1.5, "completion": 7.5}`,
		"显式写 0 也算写了":  `{"prompt": 1.5, "completion": 0} ?? {"prompt": 1.5, "completion": 7.5}`,
		"链式 ?? 每段都一致": `{"prompt": 1.0} ?? {"prompt": 2.0} ?? {"prompt": 3.0}`,
		"两侧各写同名的 let": `(let price = {"prompt": 1.5, "completion": 0}; price)
			?? (let price = {"prompt": 3.0, "completion": 15.0}; price)`,
		"三元与 ?? 嵌套且键集一致": `c > 100
			? ({"prompt": 1.0, "completion": 2.0} ?? {"prompt": 3.0, "completion": 4.0})
			: {"prompt": 4.0, "completion": 5.0}`,
		// 边界：与三元一样，units 与 fixed/minimum 不在判据内。这条用例守着
		// 「不要顺手收紧」——真收紧了，这里会红。
		"计量项与固定费只在一侧": `{"prompt": 1.0, "units": {"img": 0.02}, "fixed": fixed(0.01)}
			?? {"prompt": 2.0}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile(source); err != nil {
				t.Errorf("%q 应当编得过: %v", source, err)
			}
		})
	}
}

// TestNonRootCoalesceUnchanged 守着「判据只针对根位置」：槽里的 `??` 选的是
// 一个数值，两侧没有键集可比，形状检查不该多出任何约束。
func TestNonRootCoalesceUnchanged(t *testing.T) {
	for name, source := range map[string]string{
		"槽里的 ??":      `{"prompt": (1.0 ?? 2.0)}`,
		"算术里的 ??":     `{"prompt": (1.0 ?? 2.0) + 1.0}`,
		"槽里的 ?? 包着条件": `{"prompt": isPeak("08:00-22:00") ? (1.0 ?? 2.0) : 1.0}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile(source); err != nil {
				t.Errorf("%q 应当编得过: %v", source, err)
			}
		})
	}
}

// TestCompileAcceptsBranchTokenKeyUniformity 验证键集一致的分支照常编译，
// 而且只收紧 token 计价项——units 与一次性费用可以只在一边。
func TestCompileAcceptsBranchTokenKeyUniformity(t *testing.T) {
	for name, source := range map[string]string{
		"键集一致的对照规则": `len <= 32000
			? {"prompt": 1.5, "completion": 7.5}
			: {"prompt": 3.0, "completion": 15.0}`,
		"两支单价完全相同":   `c > 100 ? {"prompt": 1.5, "completion": 7.5} : {"prompt": 1.5, "completion": 7.5}`,
		"显式写 0 也算写了": `c > 100 ? {"prompt": 1.5, "completion": 0} : {"prompt": 1.5, "completion": 7.5}`,
		"嵌套三元每层都一致": `len <= 1000
			? (len <= 100
				? {"prompt": 1.0, "completion": 2.0}
				: {"prompt": 3.0, "completion": 4.0})
			: {"prompt": 5.0, "completion": 6.0}`,
		"tier 包住的一致分支": `len <= 100
			? tier("standard", {"prompt": 1.0, "completion": 2.0})
			: tier("long_context", {"prompt": 3.0, "completion": 4.0})`,
		// 下面两条是边界：units 与 fixed/minimum 不在判据内。这条用例守着
		// 「不要顺手收紧」——真收紧了，这里会红。
		"计量项只在一边": `units["img"] > 0
			? {"prompt": 1.0, "units": {"img": 0.02, "search": 0.01}}
			: {"prompt": 1.0, "units": {"search": 0.01}}`,
		"分支是 let 包住的表": `c > 100
			? (let x = 1.0; {"prompt": x})
			: {"prompt": 2.0}`,
		"分支是嵌套 let": `c > 100
			? (let x = 1.0; let y = 2.0; {"prompt": x + y})
			: {"prompt": 2.0}`,
		"分支是语句序列": `c > 100
			? (1.0; {"prompt": 1.0, "completion": 2.0})
			: {"prompt": 1.0, "completion": 2.0}`,
		"分支是绑定的表": `let t = {"prompt": 1.0, "completion": 2.0};
			c > 100 ? t : {"prompt": 3.0, "completion": 4.0}`,
		// 同名 let 不是问题：只要各自绑的那张表键集一致，同名反而更好读。
		"两支各写同名的 let": `len <= 100
			? (let price = {"prompt": 1.5, "completion": 0}; price)
			: (let price = {"prompt": 3.0, "completion": 15.0}; price)`,
		"固定费与保底只在一边": `len <= 100
			? {"prompt": 1.0, "fixed": fixed(0.01), "minimum": minimum(0.5)}
			: {"prompt": 1.0}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile(source); err != nil {
				t.Errorf("%q 应当编得过: %v", source, err)
			}
		})
	}
}

// TestBranchTokenKeysEqualDeclaredOnEveryBranch 验证分支键集一致的规则，
// 在任意用量下（两个分支都会被走到）Program.Keys 与实际求值出来的
// Prices.Declared 相等。
//
// 相等在这里是**字面**的：规则不写 units 与一次性费用时，Keys 里只有
// token 计价项，与 Declared 的键集一一对应。这条性质正是收紧编译期要换来的
// 东西——否则 price list 按并集说「两项都定了价」，而结算按命中那一支说
// 「这项没定价」。
func TestBranchTokenKeysEqualDeclaredOnEveryBranch(t *testing.T) {
	source := `len <= 32000
		? {"prompt": 1.5, "completion": 7.5}
		: {"prompt": 3.0, "completion": 15.0}`
	p := mustCompile(t, source)
	keys := p.Keys()

	for _, usage := range []billing.Usage{
		{},
		{PromptTokens: 10, CompletionTokens: 5}, // 短上下文：真分支
		{PromptTokens: 32000, CompletionTokens: 5}, // 边界：真分支
		{PromptTokens: 32001, CompletionTokens: 5}, // 长上下文：假分支
		// 输出多不改变分档：len 只算输入侧，这一组仍走真分支。
		{PromptTokens: 100, CompletionTokens: 9000},
	} {
		res, err := p.Eval(Env{Usage: usage, At: testAt, Region: "CN", Calendar: newTestCalendar()})
		if err != nil {
			t.Fatalf("%+v: 求值失败: %v", usage, err)
		}

		declared := make([]string, 0, len(res.Prices.Declared))
		for name := range res.Prices.Declared {
			declared = append(declared, name)
		}
		sort.Strings(declared)
		if strings.Join(declared, ",") != strings.Join(keys, ",") {
			t.Errorf("%+v: Declared = %v，Keys = %v，两者必须相等", usage, declared, keys)
		}

		// 这是上面那条相等在结算侧的意义：用量带上了任何一项都不报缺价。
		if err := billing.Validate(usage, res.Prices); err != nil {
			t.Errorf("%+v: 形状一致的规则不该报缺价: %v", usage, err)
		}
	}
}

// TestZeroPriceBranchStillDeclares 验证「显式写 0」确实被算作定了价：
// 命中那一支时 Declared 里要有这个键，否则 Validate 会把它当成漏写。
//
// 这条与键集判据配套：判据要求两支写同一组键，而「不用的那一项写 0」
// 是给作者的修法；修法要真的生效，结算侧就得把 0 当成声明。
func TestZeroPriceBranchStillDeclares(t *testing.T) {
	source := `len <= 32000
		? {"prompt": 1.5, "completion": 0}
		: {"prompt": 3.0, "completion": 15.0}`
	p := mustCompile(t, source)

	for _, usage := range []billing.Usage{
		{PromptTokens: 10, CompletionTokens: 5},    // 真分支：completion 单价是 0
		{PromptTokens: 32001, CompletionTokens: 5}, // 假分支：completion 有单价
	} {
		res, err := p.Eval(Env{Usage: usage, At: testAt, Region: "CN", Calendar: newTestCalendar()})
		if err != nil {
			t.Fatalf("%+v: 求值失败: %v", usage, err)
		}
		if !res.Prices.Declared["completion"] {
			t.Errorf("%+v: 写了 0 也算定了价，Declared 里必须有 completion: %v",
				usage, res.Prices.Declared)
		}
		if err := billing.Validate(usage, res.Prices); err != nil {
			t.Errorf("%+v: 显式写 0 不该报缺价: %v", usage, err)
		}
	}
}

// TestCompileBranchKeyErrorIsShapeReason 确认失败包在 CompileError 里、
// 大类是 ReasonShape：调用方按类别分支处置，类别错了它就走不到那条路上。
func TestCompileBranchKeyErrorIsShapeReason(t *testing.T) {
	_, err := Compile(`c > 100 ? {"prompt": 1.5, "completion": 7.5} : {"prompt": 1.5}`)
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("应当是 *CompileError，得到 %T: %v", err, err)
	}
	if ce.Reason != ReasonShape {
		t.Errorf("原因 = %s，期望 %s", ce.Reason, ReasonShape)
	}
	if ce.Source == "" {
		t.Error("CompileError.Source 应当带上原始源码，便于定位到出错的那条规则")
	}
}
