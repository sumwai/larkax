package tokens

import (
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

func i64(v int64) *int64 { return &v }

// 三家口径的差别是「同一个数字代表的东西不同」，不是「字段名不同」。
// 这几条用例就是把这个差别固定下来——写反了会重复计费，而它不报错。
func TestNormalizeOpenAI(t *testing.T) {
	// OpenAI：prompt_tokens **已含**缓存，不能再加一次。
	// 假如把 cached 加进 prompt，输入会变成 130，多收 30 个 token 的钱。
	got, err := Normalize(RawUsage{
		Upstream:         UpstreamOpenAI,
		PromptTokens:     i64(100),
		CachedTokens:     i64(30),
		CompletionTokens: i64(20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, 期望 100（缓存是子集，不能相加）", got.PromptTokens)
	}
	if got.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d, 期望 30", got.CacheReadTokens)
	}
	if got.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d, 期望 20", got.CompletionTokens)
	}
}

func TestNormalizeAnthropic(t *testing.T) {
	// Anthropic：input_tokens **不含**缓存，三者相加才是输入总量。
	// 只取 input_tokens 会让每一次带缓存的请求都少计输入，
	// 而 Claude 的缓存命中率在 agent 循环里很高，少计的部分会很大。
	got, err := Normalize(RawUsage{
		Upstream:                 UpstreamAnthropic,
		InputTokens:              i64(100),
		OutputTokens:             i64(20),
		CacheReadInputTokens:     i64(30),
		CacheCreationInputTokens: i64(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 140 {
		t.Errorf("PromptTokens = %d, 期望 140 (100+30+10)", got.PromptTokens)
	}
	if got.CacheReadTokens != 30 || got.CacheWriteTokens != 10 {
		t.Errorf("缓存明细 = %d/%d, 期望 30/10", got.CacheReadTokens, got.CacheWriteTokens)
	}
	if got.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d, 期望 20", got.CompletionTokens)
	}
}

func TestNormalizeDeepSeek(t *testing.T) {
	// DeepSeek：命中与未命中分列，相加是输入总量。
	got, err := Normalize(RawUsage{
		Upstream:              UpstreamDeepSeek,
		PromptCacheHitTokens:  i64(30),
		PromptCacheMissTokens: i64(70),
		CompletionTokens:      i64(20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, 期望 100 (30+70)", got.PromptTokens)
	}
	if got.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d, 期望 30", got.CacheReadTokens)
	}
}

func TestNormalizeDeepSeekFallsBackToPromptTokens(t *testing.T) {
	// 有的源只报 prompt_tokens（其中已含命中）。此时按子集口径处理，
	// 不能再加一次 hit。
	got, err := Normalize(RawUsage{
		Upstream:             UpstreamDeepSeek,
		PromptTokens:         i64(100),
		PromptCacheHitTokens: i64(30),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, 期望 100", got.PromptTokens)
	}
}

func TestNormalizeOpenAIWithAnthropicFieldNames(t *testing.T) {
	// 少数兼容层把 Anthropic 的字段名直接搬过来用。此时按 Anthropic 口径
	// 处理，否则输入会被算漏。
	got, err := Normalize(RawUsage{
		Upstream:             UpstreamOpenAI,
		InputTokens:          i64(100),
		CacheReadInputTokens: i64(30),
		OutputTokens:         i64(20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 130 {
		t.Errorf("PromptTokens = %d, 期望 130", got.PromptTokens)
	}
	if got.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d, 期望 20", got.CompletionTokens)
	}
}

func TestNormalizeCacheCannotExceedPrompt(t *testing.T) {
	// 上游口径不一致时会出现「缓存量大于输入量」。压低而不是放大：
	// 放大输入会让金额变高，而这里没有依据说明该放大多少。
	got, err := Normalize(RawUsage{
		Upstream:         UpstreamOpenAI,
		PromptTokens:     i64(100),
		CachedTokens:     i64(300),
		CacheWriteTokens: i64(200),
		CompletionTokens: i64(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheReadTokens+got.CacheWriteTokens > got.PromptTokens {
		t.Errorf("缓存合计 %d 大于输入 %d", got.CacheReadTokens+got.CacheWriteTokens, got.PromptTokens)
	}
	// 压的时候先压写入——它更容易缺失，也更容易是上游多报的那一项。
	if got.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, 期望先被压到 0", got.CacheWriteTokens)
	}
	if got.CacheReadTokens != 100 {
		t.Errorf("CacheReadTokens = %d, 期望被压到 100", got.CacheReadTokens)
	}
}

func TestNormalizeAnthropicCacheNeverExceedsPrompt(t *testing.T) {
	// Anthropic 的输入总量 = input + cache_read + cache_write，
	// 所以缓存天然不可能超过输入。这条断言固定住这个不变量：
	// 一旦有人改写公式（例如只取 input_tokens），它就会失败。
	got, err := Normalize(RawUsage{
		Upstream:                 UpstreamAnthropic,
		InputTokens:              i64(100),
		CacheReadInputTokens:     i64(300),
		CacheCreationInputTokens: i64(200),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 600 {
		t.Errorf("PromptTokens = %d, 期望 600 (100+300+200)", got.PromptTokens)
	}
	if got.CacheReadTokens+got.CacheWriteTokens > got.PromptTokens {
		t.Error("Anthropic 口径下缓存不该超过输入总量")
	}
}

func TestNormalizeRejectsNegativeFields(t *testing.T) {
	// 负 token 只可能来自解析错误。放过去会算出一笔负数消费，
	// 即「上游付钱给用户」——而它不会让任何一处失败。
	_, err := Normalize(RawUsage{
		Upstream:     UpstreamOpenAI,
		PromptTokens: i64(-1),
	})
	if err == nil {
		t.Error("负字段应报错")
	}
}

func TestNormalizeMissingFieldsAreZero(t *testing.T) {
	// 「这家不报这个字段」是常态而非异常，不该报错。
	got, err := Normalize(RawUsage{Upstream: UpstreamOpenAI})
	if err != nil {
		t.Fatalf("字段全缺不该报错: %v", err)
	}
	if got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Errorf("全缺字段应得到零值用量, 得到 %+v", got)
	}
	if got.UnitCounts != nil || got.Raw != nil {
		t.Error("空字段应保持 nil，而不是空 map（两者在展示上含义不同）")
	}
}

// 上游报「输出 5、其中推理 500」是自相矛盾的（推理含在输出里）。
// 把输出抬到至少等于推理，让 channel.Usage 自身自洽——它会被序列化给
// 调用方看。抬这一下不改金额：计费层拆分时减掉的量与抬高的量相同。
func TestNormalizeReasoningDoesNotExceedCompletion(t *testing.T) {
	got, err := Normalize(RawUsage{
		Upstream:         UpstreamOpenAI,
		PromptTokens:     i64(10),
		CompletionTokens: i64(5),
		ReasoningTokens:  i64(500),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CompletionTokens < got.ReasoningTokens {
		t.Errorf("输出 %d 小于推理 %d", got.CompletionTokens, got.ReasoningTokens)
	}
}

// 三家的推理都含在输出里，声明必须显式给出。
//
// 漏了声明就会落到零值（「推理是单独报的」），于是同一批 token 在计费层
// 被按输出价与推理价各收一遍——账单只是偏高，对账发现不了。
func TestNormalizeDeclaresReasoningIncluded(t *testing.T) {
	cases := map[string]RawUsage{
		"openai": {
			Upstream:         UpstreamOpenAI,
			PromptTokens:     i64(10),
			CompletionTokens: i64(100),
			ReasoningTokens:  i64(60),
		},
		"anthropic": {
			Upstream:        UpstreamAnthropic,
			InputTokens:     i64(10),
			OutputTokens:    i64(100),
			ReasoningTokens: i64(60),
		},
		"deepseek": {
			Upstream:              UpstreamDeepSeek,
			PromptCacheHitTokens:  i64(10),
			PromptCacheMissTokens: i64(5),
			CompletionTokens:      i64(100),
			ReasoningTokens:       i64(60),
		},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Normalize(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !got.CompletionIncludesReasoning {
				t.Error("未声明「推理含在输出里」")
			}
		})
	}
}

func TestNormalizeResponseUsageReportsMissingInput(t *testing.T) {
	// 非流式响应里输入缺失通常是上游没报，而不是这次真的没有输入。
	// 用专门的错误类型让调用方决定是否去估算。
	_, err := NormalizeResponseUsage(RawUsage{
		Upstream:         UpstreamOpenAI,
		CompletionTokens: i64(20),
	})
	if !errors.Is(err, ErrNoInputTokens) {
		t.Errorf("err = %v, 期望 ErrNoInputTokens", err)
	}
}

func TestNormalizeResponseUsageOKWhenInputPresent(t *testing.T) {
	if _, err := NormalizeResponseUsage(RawUsage{
		Upstream:         UpstreamOpenAI,
		PromptTokens:     i64(10),
		CompletionTokens: i64(20),
	}); err != nil {
		t.Errorf("有输入时不该报错: %v", err)
	}
}

func TestCacheWriteDetailIsNotExtraVolume(t *testing.T) {
	// Anthropic 的 5m/1h 拆分**已经包含在** cache_creation_input_tokens 里。
	// 把它当额外的量相加会重复计费，所以它只走这个单独的函数。
	u := RawUsage{
		Upstream:                 UpstreamAnthropic,
		InputTokens:              i64(100),
		CacheCreationInputTokens: i64(50),
		CacheCreation5mTokens:    i64(30),
		CacheCreation1hTokens:    i64(20),
	}
	got, err := Normalize(u)
	if err != nil {
		t.Fatal(err)
	}
	// 输入总量是 100 + 50，不是 100 + 50 + 30 + 20。
	if got.PromptTokens != 150 {
		t.Errorf("PromptTokens = %d, 期望 150（5m/1h 是拆分，不是额外量）", got.PromptTokens)
	}
	detail := CacheWriteDetail(u)
	if detail["cache_write_5m"] != 30 || detail["cache_write_1h"] != 20 {
		t.Errorf("明细 = %v, 期望 5m=30 1h=20", detail)
	}
}

func TestNormalizePreservesUnitCountsAndRaw(t *testing.T) {
	raw := map[string]any{"source": "test"}
	units := map[string]int64{"images": 2}
	got, err := Normalize(RawUsage{
		Upstream:   UpstreamOpenAI,
		UnitCounts: units,
		Raw:        raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.UnitCounts["images"] != 2 {
		t.Error("UnitCounts 应原样保留")
	}
	if got.Raw["source"] != "test" {
		t.Error("Raw 应原样保留——它是排查口径变化的唯一依据")
	}
}

func TestUpstreamStringStable(t *testing.T) {
	for u, want := range map[Upstream]string{
		UpstreamOpenAI:    "openai",
		UpstreamAnthropic: "anthropic",
		UpstreamDeepSeek:  "deepseek",
	} {
		if got := u.String(); got != want {
			t.Errorf("Upstream(%d).String() = %q, 期望 %q", u, got, want)
		}
	}
}

// 编译期确认归一函数的输出就是 channel.Usage——本包不定义第二套口径。
var _ func(RawUsage) (channel.Usage, error) = Normalize

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

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 来自变异测试（gremlins unleash）：把比较符或分支方向改一下，测试仍然通过。
// 覆盖率看不出来——行被执行过了，但「两边都填」这种边界没被走到。
//
// 归一化层的每一个分支都直接决定收多少钱，所以这里刻意把
// 「两个字段都填了」的情形都过一遍——那才是分支真正起作用的场景。

func ptrInt(v int64) *int64 { return &v }

// Anthropic 口径下 OutputTokens 是原生字段，优先；CompletionTokens 只是
// 少数兼容层用的别名，在前者缺席时才回退。
//
// 变异：`if out.CompletionTokens == 0` 改成 `!= 0`，两者都填时会被
// 别名覆盖——而原有用例从没让两个字段**同时有值**，所以看不出差别。
func TestAnthropicOutputTokensTakesPrecedence(t *testing.T) {
	got, err := Normalize(RawUsage{
		Upstream:         UpstreamAnthropic,
		OutputTokens:     ptrInt(222),
		CompletionTokens: ptrInt(111),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CompletionTokens != 222 {
		t.Errorf("CompletionTokens = %d，期望 222（原生字段优先）", got.CompletionTokens)
	}

	// 只有别名时回退到它
	got, err = Normalize(RawUsage{
		Upstream:         UpstreamAnthropic,
		CompletionTokens: ptrInt(111),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CompletionTokens != 111 {
		t.Errorf("回退后 CompletionTokens = %d，期望 111", got.CompletionTokens)
	}
}

// OpenAI 口径：CompletionTokens 已填时不该被 OutputTokens 覆盖。
func TestOpenAICompletionNotOverridden(t *testing.T) {
	got, err := Normalize(RawUsage{
		Upstream:         UpstreamOpenAI,
		CompletionTokens: ptrInt(333),
		OutputTokens:     ptrInt(444),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CompletionTokens != 333 {
		t.Errorf("CompletionTokens = %d，期望 333", got.CompletionTokens)
	}
}

// CacheWriteTokens 与 CacheCreationInputTokens 都填时，前者优先。
func TestCacheWriteTakesPrecedence(t *testing.T) {
	got, err := Normalize(RawUsage{
		Upstream:                 UpstreamOpenAI,
		PromptTokens:             ptrInt(1000),
		CacheWriteTokens:         ptrInt(50),
		CacheCreationInputTokens: ptrInt(999),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheWriteTokens != 50 {
		t.Errorf("CacheWriteTokens = %d，期望 50", got.CacheWriteTokens)
	}
}

// 缓存读量与写入量**都填**时，两者相加不能超过输入总量。
//
// 变异：`total > out.PromptTokens` 改成 `>=`，恰好相等时也会触发压回，
// 把两个字段各减一份——而原有用例里没有「两个都填」的情形。
func TestBothCacheFieldsCountedTogether(t *testing.T) {
	got, err := Normalize(RawUsage{
		Upstream:                 UpstreamOpenAI,
		PromptTokens:             ptrInt(1000),
		CachedTokens:             ptrInt(300),
		CacheWriteTokens:         ptrInt(200),
		CacheCreationInputTokens: ptrInt(900),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheReadTokens != 300 || got.CacheWriteTokens != 200 {
		t.Fatalf("缓存明细 = 读 %d 写 %d，期望 300/200", got.CacheReadTokens, got.CacheWriteTokens)
	}
	// 两者之和 500 不超过输入 1000，不该被压
	if got.CacheReadTokens+got.CacheWriteTokens > got.PromptTokens {
		t.Errorf("缓存合计超过输入：%d > %d", got.CacheReadTokens+got.CacheWriteTokens, got.PromptTokens)
	}
}

// OpenAI 口径下 prompt_tokens 与 input_tokens 都填时，**不该相加**——
// prompt 已含缓存，再加一次就是重复计费。
//
// 变异：`out.PromptTokens == 0 && u.InputTokens != nil` 里的
// `== 0` 改成 `!= 0`，两个都填时会走 Anthropic 口径把三者相加，
// 账单悄悄变高。
func TestOpenAIDoesNotAddInputTokensWhenPromptPresent(t *testing.T) {
	got, err := Normalize(RawUsage{
		Upstream:                 UpstreamOpenAI,
		PromptTokens:             ptrInt(1000),
		InputTokens:              ptrInt(800),
		CacheReadInputTokens:     ptrInt(100),
		CacheCreationInputTokens: ptrInt(50),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PromptTokens != 1000 {
		t.Errorf("PromptTokens = %d，期望 1000（prompt 已含缓存，不该再加）", got.PromptTokens)
	}
}
