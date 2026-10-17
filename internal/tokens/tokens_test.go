package tokens

import (
	"context"
	"strings"
	"testing"
)

// 计数类测试要显式加载词表：不加载时 OpenAI 族会退到估算，
// 而那时断言精确值必然失败，且失败信息会误导人。
func newPrepared(t *testing.T) *Counter {
	t.Helper()
	c := NewCounter()
	if _, err := c.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	return c
}

func TestFamilyOf(t *testing.T) {
	cases := []struct {
		model string
		want  Family
	}{
		{"claude-sonnet-4-6", FamilyClaude},
		{"claude-3-5-haiku-latest", FamilyClaude},
		{"anthropic/claude-opus-4-7", FamilyClaude},
		{"gpt-4o", FamilyOpenAI},
		{"gpt-4o-2024-11-20", FamilyOpenAI},
		{"gpt-3.5-turbo", FamilyOpenAI},
		{"o3-mini", FamilyOpenAI},
		{"text-embedding-3-small", FamilyOpenAI},
		{"deepseek-chat", FamilyDeepSeek},
		{"deepseek-reasoner", FamilyDeepSeek},
		{"qwen-max", FamilyQwen},
		{"qwen2.5-72b-instruct", FamilyQwen},
		{"kimi-k2", FamilyKimi},
		{"moonshot-v1-8k", FamilyKimi},
		{"glm-4-plus", FamilyGLM},
		{"chatglm3", FamilyGLM},
		{"some-vendor/mystery-model", FamilyUnknown},
		{"", FamilyUnknown},
	}
	for _, tc := range cases {
		if got := FamilyOf(tc.model); got != tc.want {
			t.Errorf("FamilyOf(%q) = %v, 期望 %v", tc.model, got, tc.want)
		}
	}
}

func TestEncodingForModel(t *testing.T) {
	// 老一代用 cl100k，新一代用 o200k。猜错会让精确计数变成「精确地算错」，
	// 所以这几个边界值必须有测试固定。
	if got := encodingForModel("gpt-4o"); string(got) != "o200k_base" {
		t.Errorf("gpt-4o 应走 o200k_base，得到 %s", got)
	}
	if got := encodingForModel("gpt-4-turbo"); string(got) != "cl100k_base" {
		t.Errorf("gpt-4-turbo 应走 cl100k_base，得到 %s", got)
	}
	if got := encodingForModel("gpt-3.5-turbo"); string(got) != "cl100k_base" {
		t.Errorf("gpt-3.5-turbo 应走 cl100k_base，得到 %s", got)
	}
	if got := encodingForModel("gpt-5"); string(got) != "o200k_base" {
		t.Errorf("gpt-5 应走 o200k_base，得到 %s", got)
	}
	if got := encodingForModel("text-davinci-003"); string(got) != "cl100k_base" {
		t.Errorf("text-davinci-003 应走 cl100k_base，得到 %s", got)
	}
	if got := encodingForModel("davinci"); string(got) != "r50k_base" {
		t.Errorf("davinci 应走 r50k_base，得到 %s", got)
	}
}

func TestCountTextExactForOpenAI(t *testing.T) {
	c := newPrepared(t)
	cases := []struct {
		model string
		text  string
		want  int64
	}{
		{"gpt-4o", "hello", 1},
		{"gpt-4o", "The quick brown fox jumps over the lazy dog.", 10},
		{"gpt-4o", "你好世界", 2},
		{"gpt-4o", "func main() {}", 4},
		{"gpt-4o", `{"a":1}`, 5},
		{"gpt-4o", "https://api.example.com/v1/messages", 8},
	}
	for _, tc := range cases {
		got, err := c.CountText(context.Background(), tc.model, tc.text)
		if err != nil {
			t.Fatalf("CountText(%q) 出错: %v", tc.text, err)
		}
		if got.Tokens != tc.want {
			t.Errorf("CountText(%q) = %d, 期望 %d", tc.text, got.Tokens, tc.want)
		}
		if got.Source != SourceTokenized {
			t.Errorf("CountText(%q) 来源 = %v, 期望 tokenized", tc.text, got.Source)
		}
	}
}

func TestCountTextEstimatesWithoutVocab(t *testing.T) {
	c := newPrepared(t)
	// Claude 没有公开词表：只能估算，而且必须如实标注。
	for _, model := range []string{"claude-sonnet-4-6", "mystery-vendor/model-x"} {
		got, err := c.CountText(context.Background(), model, "这是一段用于测试的中文文本。")
		if err != nil {
			t.Fatalf("CountText(%q) 出错: %v", model, err)
		}
		if got.Source != SourceEstimated {
			t.Errorf("%s 的来源 = %v, 期望 estimated", model, got.Source)
		}
		if got.Tokens < 1 {
			t.Errorf("%s 的估算值 = %d, 非空文本必须 >= 1", model, got.Tokens)
		}
	}
}

func TestCountTextWithoutVocabFallsBackToEstimate(t *testing.T) {
	// 零值 Counter（vocab 为 nil）等价于「这台机器上没有可用词表」。
	// 此时不能报错——报错会把「词表不可用」升级成「服务不可用」，
	// 而估算值虽有偏差但仍然可用，且 Source 会如实标注。
	var c Counter
	got, err := c.CountText(context.Background(), "gpt-4o", "hello world")
	if err != nil {
		t.Fatalf("词表不可用时应退到估算而不是报错: %v", err)
	}
	if got.Source != SourceEstimated {
		t.Errorf("来源 = %v, 期望 estimated", got.Source)
	}
	if got.Tokens < 1 {
		t.Errorf("估算值 = %d, 必须 >= 1", got.Tokens)
	}
}

func TestCountTextLazilyLoadsVocab(t *testing.T) {
	// 没调 Prepare 也能拿到精确值：惰性加载发生在第一次用到精确路径时。
	// 这是有意的取舍——忘了 Prepare 就得到一个更差的答案，比直接失败更难排查。
	// 想要「启动时就加载好」，调 Prepare；两者的区别由 Loaded() 可观测。
	c := NewCounter()
	if len(c.Loaded()) != 0 {
		t.Fatal("构造时不该加载任何词表")
	}
	got, err := c.CountText(context.Background(), "gpt-4o", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceTokenized {
		t.Errorf("惰性加载后来源 = %v, 期望 tokenized", got.Source)
	}
	if len(c.Loaded()) == 0 {
		t.Error("用过一次之后应能看到已加载的编码")
	}
}

func TestCountTextOnNilCounterDoesNotPanic(t *testing.T) {
	// 生产代码里一个漏初始化的 Counter 不该让请求 panic。
	var c *Counter
	got, err := c.CountText(context.Background(), "gpt-4o", "hello")
	if err != nil {
		t.Fatalf("nil Counter 应退到估算: %v", err)
	}
	if got.Tokens < 1 {
		t.Errorf("估算值 = %d, 必须 >= 1", got.Tokens)
	}
}

func TestCountTextEmpty(t *testing.T) {
	c := newPrepared(t)
	got, err := c.CountText(context.Background(), "gpt-4o", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens != 0 {
		t.Errorf("空文本应计 0, 得到 %d", got.Tokens)
	}
}

func TestCountTextRespectsCanceledContext(t *testing.T) {
	c := newPrepared(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CountText(ctx, "gpt-4o", "hello"); err == nil {
		t.Error("已取消的 context 上应返回错误")
	}
}

func TestObserveRequestNeedsContextButNotCancelable(t *testing.T) {
	// ObserveRequest 内部用 Background：分词不可取消，这里只是确认它
	// 不会因为外部没有 context 而 panic。
	e := NewStreamEstimator(newPrepared(t), "gpt-4o")
	e.ObserveRequest("hello world", 1, 0)
	if e.input.Tokens < 1 {
		t.Errorf("输入 = %d, 期望 >= 1", e.input.Tokens)
	}
}

func TestEstimatedTokensMonotonic(t *testing.T) {
	// 估算必须是单调的：更长的文本不能算出更少的 token。
	// 单调性是最低要求——破坏它意味着「加内容反而更便宜」。
	prev := int64(0)
	base := "The gateway receives a request and selects an upstream channel. "
	for i := 1; i <= 40; i++ {
		got := EstimatedTokens("claude-sonnet-4-6", strings.Repeat(base, i))
		if got < prev {
			t.Fatalf("长度 %d 段的估算值 %d 小于前一个 %d", i, got, prev)
		}
		prev = got
	}
}

func TestEstimatedCJKHigherThanLatin(t *testing.T) {
	// 中文的 token 密度明显高于英文。用「字符数 ÷ 4」的粗估会把这一点
	// 抹掉，从而对中文系统性低估——而中文正是最需要保守的那一类。
	zh := EstimatedTokens("claude-sonnet-4-6", strings.Repeat("网关渠道协议转换", 10))
	en := EstimatedTokens("claude-sonnet-4-6", strings.Repeat("gateway channel protocol", 10))
	if zh <= en {
		t.Errorf("同字符数的中文估算 (%d) 应高于英文 (%d)", zh, en)
	}
}

func TestEstimateMinimumOnePerNonEmpty(t *testing.T) {
	// 非空文本必须至少 1 个 token。算出 0 会让这段内容在账上消失。
	for _, s := range []string{"a", " ", "\n", "。", "🎉", "\t"} {
		if got := EstimatedTokens("claude-sonnet-4-6", s); got < 1 {
			t.Errorf("EstimatedTokens(%q) = %d, 必须 >= 1", s, got)
		}
	}
}

func TestEstimateHandlesLongIncompressibleRun(t *testing.T) {
	// 密钥/摘要/base64 这类串 BPE 压不动，实测接近逐字符。
	// 线性估算只给 0.23~0.61，会低估三成——run 下限就是为这个加的。
	plain := EstimatedTokens("claude-sonnet-4-6", strings.Repeat("ab", 40))
	// 用同长度的随机串对比：两者的估算不应差出量级
	blob := EstimatedTokens("claude-sonnet-4-6",
		"sk-"+strings.Repeat("aB3xK9mQ2zR7wT5yU8iO", 2))
	if blob < 20 {
		t.Errorf("长不可压缩串的估算 %d 偏低", blob)
	}
	if plain < 1 {
		t.Error("普通长串的估算必须 >= 1")
	}
}

func TestCountAddTakesLessTrustedSource(t *testing.T) {
	measured := Count{Tokens: 10, Source: SourceMeasured, Model: "m"}
	estimated := Count{Tokens: 5, Source: SourceEstimated, Model: "m"}
	if got := measured.Add(estimated); got.Source != SourceEstimated {
		t.Errorf("精确 + 估算 = %v, 期望 estimated", got.Source)
	}
	if got := measured.Add(estimated); got.Tokens != 15 {
		t.Errorf("合计 = %d, 期望 15", got.Tokens)
	}
}

func TestRecordMeasured(t *testing.T) {
	// 上游实测值是权威数字，本包只负责打对来源标记——
	// 让调用方自己构造 Count 等于把「别忘了标来源」交给每个调用点。
	got, err := RecordMeasured("gpt-4o", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceMeasured || got.Tokens != 42 {
		t.Errorf("RecordMeasured = %+v", got)
	}
	if _, err := RecordMeasured("gpt-4o", -1); err == nil {
		t.Error("负的实测值应报错")
	}
}

func TestSourceIsExact(t *testing.T) {
	if !SourceMeasured.IsExact() {
		t.Error("实测值应可与上游对账")
	}
	if SourceTokenized.IsExact() {
		t.Error("本地精确值不可对账（差的正是框架与特殊 token）")
	}
	if SourceEstimated.IsExact() {
		t.Error("估算值不可对账")
	}
}

func TestSourceStringStable(t *testing.T) {
	// 这几个字符串会进数据库与 API，改名等于一次不兼容变更。
	for src, want := range map[Source]string{
		SourceMeasured:  "measured",
		SourceTokenized: "tokenized",
		SourceEstimated: "estimated",
	} {
		if got := src.String(); got != want {
			t.Errorf("Source(%d).String() = %q, 期望 %q", src, got, want)
		}
	}
}

func TestPrepareIsIdempotentAndConcurrent(t *testing.T) {
	c := NewCounter()
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := c.Prepare(context.Background())
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发 Prepare 出错: %v", err)
		}
	}
	if got := c.Loaded(); len(got) == 0 {
		t.Error("Prepare 之后应至少有一种编码已加载")
	}
}

func TestPrepareOnZeroCounter(t *testing.T) {
	var c *Counter
	if _, err := c.Prepare(context.Background()); err == nil {
		t.Error("nil Counter 应报错，而不是 panic 或静默通过")
	}
}

func TestPrepareReportsLoadDuration(t *testing.T) {
	// 这组数字用来回答「16MB 是启动时解的还是第一个请求解的」，
	// 所以要能取到，且必须为正。
	c := NewCounter()
	d, err := c.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d) == 0 {
		t.Fatal("应报告每种编码的加载耗时")
	}
	for enc, dur := range d {
		if dur <= 0 {
			t.Errorf("编码 %s 的加载耗时 = %v, 必须为正", enc, dur)
		}
	}
}

func TestPrepareHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewCounter()
	if _, err := c.Prepare(ctx); err == nil {
		t.Error("已取消的 context 上 Prepare 应返回错误")
	}
}

func TestBoundsBasisDistinguishesMeasuredFromInferred(t *testing.T) {
	// Claude 那一列是推理出来的，OpenAI 那一列是实测的。
	// 这个区别必须在数据里留着，不能只写在注释里。
	if !strings.Contains(OpenAIBounds.Basis, "实测") {
		t.Errorf("OpenAI 的误差依据应标明是实测: %q", OpenAIBounds.Basis)
	}
	if !strings.Contains(ClaudeBounds.Basis, "推理") {
		t.Errorf("Claude 的误差依据应标明是推理: %q", ClaudeBounds.Basis)
	}
}

func TestBoundsForUnknownFamilyFallsBackToWidest(t *testing.T) {
	got := BoundsFor(FamilyUnknown)
	if got.MaxRatio != UnknownModelBounds.MaxRatio {
		t.Errorf("未知族应落到兜底区间, 得到 %+v", got)
	}
	if BoundsOf("mystery-model").MaxRatio < OpenAIBounds.MaxRatio {
		t.Error("未知族的误差区间不该比 OpenAI 那一列更窄")
	}
}

func TestCountTextEstimatedForcesEstimate(t *testing.T) {
	// 上游换了词表而我们还没跟上时，调用方要能显式避开本地词表——
	// 一个基于过期词表的「精确」数字比明说是估算更危险。
	c := newPrepared(t)
	got := c.CountTextEstimated("gpt-4o", "hello world")
	if got.Source != SourceEstimated {
		t.Errorf("来源 = %v, 期望 estimated", got.Source)
	}
	if got.Tokens < 1 {
		t.Errorf("估算值 = %d, 必须 >= 1", got.Tokens)
	}
}

func TestBoundsForKnownFamilies(t *testing.T) {
	if got := BoundsFor(FamilyOpenAI); got.MaxRatio != OpenAIBounds.MaxRatio {
		t.Errorf("OpenAI 族 = %+v, 期望 OpenAIBounds", got)
	}
	if got := BoundsFor(FamilyClaude); got.MaxRatio != ClaudeBounds.MaxRatio {
		t.Errorf("Claude 族 = %+v, 期望 ClaudeBounds", got)
	}
	// 有开源词表但尚未接入的族落到兜底：拿 OpenAI 的区间去套是错的，
	// 因为 Qwen 的中文密度与 o200k 差得远。
	if got := BoundsFor(FamilyQwen); got.MaxRatio != UnknownModelBounds.MaxRatio {
		t.Errorf("Qwen 族 = %+v, 期望兜底区间", got)
	}
}

func TestLoadedEmptyOnFreshCounter(t *testing.T) {
	c := NewCounter()
	if got := c.Loaded(); len(got) != 0 {
		t.Errorf("新建的 Counter 不该有已加载编码, 得到 %v", got)
	}
	var nilC *Counter
	if got := nilC.Loaded(); got != nil {
		t.Errorf("nil Counter 的 Loaded 应为 nil, 得到 %v", got)
	}
}
