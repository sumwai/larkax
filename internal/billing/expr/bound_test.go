package expr

import (
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
)

// TestBoundBranchShapeMismatch 验证「根位置上的裸数值」在编译期被拦住。
//
// 这条不是空想的：`len <= 100 ? {"prompt": 1.0} : 2.0` 两支都合法、
// 类型检查也过，但最坏情况求值会把两支喂给 __worst，
// 而「一张表与一个数」无法逐槽比较——报错会发生在预留阶段，
// 那时请求已经发不出去了。必须在编译期拦。
func TestBoundBranchShapeMismatch(t *testing.T) {
	for _, source := range []string{
		`len <= 100 ? {"prompt": 1.0} : 2.0`,
		`{"prompt": {"completion": 1.0}}`,
		`{"units": 1.5}`,
	} {
		_, err := Compile(source)
		if err == nil {
			t.Errorf("%q 应当编不过", source)
			continue
		}
		var ce *CompileError
		if !errors.As(err, &ce) || ce.Reason != ReasonShape {
			t.Errorf("%q 的原因应当是 %s，得到 %v", source, ReasonShape, err)
		}
	}
}

// TestBoundNumberConditionalInSlotIsFine 说明槽里的数值条件照常工作。
//
// 槽里的三元两侧都是数值，__worst 拿到两个数取较大者即可；
// 根位置的三元两侧是两张表，必须逐槽取最大值。两种情形由同一个
// 位置信息区分，规则作者不需要知道这件事。
func TestBoundNumberConditionalInSlotIsFine(t *testing.T) {
	p := mustCompile(t, `{"prompt": len <= 100 ? 0.0000010 : 0.0000040}`)
	for _, tc := range []struct {
		length int64
		want   string
	}{
		{50, "0.00000100"},
		{500, "0.00000400"},
	} {
		env := Env{Usage: billing.Usage{PromptTokens: tc.length}, At: testAt}
		a, err := p.Eval(env)
		if err != nil {
			t.Fatal(err)
		}
		w, err := p.EvalWorstCase(env)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Prices.Prompt.String(); got != tc.want {
			t.Errorf("len=%d: 实际 %s，期望 %s", tc.length, got, tc.want)
		}
		if got := w.Prices.Prompt.String(); got != "0.00000400" {
			t.Errorf("len=%d: 上界 %s，期望两侧的较大者 0.00000400", tc.length, got)
		}
	}
}

// TestBoundAlsoHoldsWhenBothBranchesAreTables 验证形状一致时照常工作，
// 且上界在两种分支上都覆盖得住。
func TestBoundAlsoHoldsWhenBothBranchesAreTables(t *testing.T) {
	p := mustCompile(t, `len <= 100
		? {"prompt": 0.0000010, "completion": 0.0000050}
		: {"prompt": 0.0000040, "completion": 0.0000200}`)
	for _, prompt := range []int64{0, 50, 100, 101, 1000} {
		env := Env{Usage: billing.Usage{PromptTokens: prompt, CompletionTokens: 10}, At: testAt}
		a, err := p.Eval(env)
		if err != nil {
			t.Fatal(err)
		}
		w, err := p.EvalWorstCase(env)
		if err != nil {
			t.Fatal(err)
		}
		if w.Prices.Prompt.Cmp(a.Prices.Prompt) < 0 || w.Prices.Completion.Cmp(a.Prices.Completion) < 0 {
			t.Errorf("len=%d: 上界 %+v 不大于实际 %+v", prompt, w.Prices, a.Prices)
		}
	}
}

// TestBoundUnitsConditionsCoverBothBranches 验证 units 按条件选一张表时，
// 上界把两张表的键并起来——缺了哪一边都可能让结算超出去。
func TestBoundUnitsConditionsCoverBothBranches(t *testing.T) {
	p := mustCompile(t, `{
		"prompt": 0.0000015,
		"units": units["img"] > 0
			? {"img": 0.02, "search": 0.01}
			: {"search": 0.03}
	}`)

	// 用量里没有 img：实际走假分支，但上界里两张表的键都该在。
	env := Env{Usage: billing.Usage{PromptTokens: 10}, At: testAt}
	w, err := p.EvalWorstCase(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"img", "search"} {
		if _, ok := w.Prices.Units[name]; !ok {
			t.Errorf("上界里缺了计量项 %q: %v", name, w.Prices.Units)
		}
	}
	// search 两边都有，取贵的那个（0.03）。
	if got := w.Prices.Units["search"].String(); got != "0.03000000" {
		t.Errorf("search 的上界 = %s，期望 0.03000000", got)
	}
	// img 只在一边有，直接带过来。
	if got := w.Prices.Units["img"].String(); got != "0.02000000" {
		t.Errorf("img 的上界 = %s，期望 0.02000000", got)
	}
}
