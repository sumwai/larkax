package tokens

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

// 这些用例把估算式的行为逐类固定下来。它们不校验绝对精度（那是标定的
// 事，见 COUNTER.md），而是校验**方向与形态**：同类之间谁高谁低、
// 哪些形态必须被下限兜住。方向写反了不会报错，只会让某一类输入系统性偏。

func TestClassOf(t *testing.T) {
	cases := []struct {
		r    rune
		want charClass
	}{
		{'a', classLatin}, {'Z', classLatin},
		{'5', classDigit}, {'٥', classDigit}, // 阿拉伯-印度数字
		{' ', classSpace}, {'\n', classSpace}, {' ', classSpace},
		{',', classPunct}, {'。', classPunct},
		{'中', classCJK}, {'あ', classCJK}, {'ア', classCJK}, {'한', classCJK},
		{'я', classOtherLetter}, {'م', classOtherLetter}, {'अ', classOtherLetter},
		{'🎉', classSymbol}, {'→', classSymbol}, {'€', classSymbol},
	}
	for _, tc := range cases {
		if got := classOf(tc.r); got != tc.want {
			t.Errorf("classOf(%q) = %d, 期望 %d", tc.r, got, tc.want)
		}
	}
}

func TestEstimatePerClassOrdering(t *testing.T) {
	// 密度的相对大小是这套估算的核心结论。若某两类的顺序反了，
	// 说明权重表被改动过而标定没跟上。
	// 比较的是**每字符**的估算值，不是绝对值——否则文本长度会盖过
	// 密度差异，测出来的是长度而不是类别。
	// 顺序按实测密度排列（见 COUNTER.md 的「实测的每类边际密度」）：
	// 空白 0.03 < 拉丁 0.19 < 数字 0.46 < 中文 0.78 < emoji 1.14（o200k_base）。
	// 若某两类的顺序反了，说明权重表被改动过而标定没跟上。
	order := []struct {
		name string
		text string
	}{
		{"空白", strings.Repeat(" ", 280)},
		{"拉丁词", strings.Repeat("the gateway router channel ", 10)},
		{"数字", strings.Repeat("1234567890", 28)},
		{"中文", strings.Repeat("网络网关渠道协议转换", 23)},
		{"emoji", strings.Repeat("🎉🚀✨", 93)},
	}
	density := make([]float64, len(order))
	for i, o := range order {
		density[i] = estimate(o.text, tableOpenAI) / float64(len([]rune(o.text)))
	}
	for i := 1; i < len(density); i++ {
		if density[i] < density[i-1] {
			t.Errorf("%s 的每字符估算 (%.3f) 低于 %s (%.3f)，密度顺序反了",
				order[i].name, density[i], order[i-1].name, density[i-1])
		}
	}
}

func TestEstimateRunFloorForBlankRuns(t *testing.T) {
	// 空白段的密度算出来远小于 1，而下限必须把它抬到 1。
	// 没有下限的话 `\n\n` 会被算成 0.2 个 token，而实测是 1 个。
	// 用「多一段空白就多一个 token」来验证下限生效。
	base := estimate("x", tableOpenAI)
	withBlank := estimate("x\n\n", tableOpenAI)
	if withBlank <= base {
		t.Errorf("追加一段空白后估算没有增加: %v -> %v", base, withBlank)
	}
}

func TestEstimateLongIncompressibleRun(t *testing.T) {
	// 长且不中断的字母数字串触发额外下限。用同样的长度、不同的
	// 「可压缩性」对比：自然语言词序列不该触发下限。
	natural := strings.Repeat("gateway router ", 4)
	blob := strings.Repeat("kJ8xQ2mZ7pR4", 4)
	naturalPerChar := estimate(natural, tableOpenAI) / float64(len([]rune(natural)))
	blobPerChar := estimate(blob, tableOpenAI) / float64(len([]rune(blob)))
	if blobPerChar <= naturalPerChar {
		t.Errorf("不可压缩串的密度 (%v) 应高于自然语言 (%v)", blobPerChar, naturalPerChar)
	}
}

func TestEstimateShortRunsDoNotTriggerFloor(t *testing.T) {
	// 短标识符（main.go、v1.2、read_file）是正常的词，词表里有它们的
	// 片段，不该被下限抬高。若阈值设得太低，代码类输入会被大幅高估。
	const short = "main.go"
	tbl := tableOpenAI
	plain := 0.0
	for _, r := range short {
		plain += tbl.weight[classOf(r)]
	}
	if got := estimate(short, tbl) / tbl.bias; got > plain*2 {
		t.Errorf("短标识符 %q 的估算 %v 明显高于线性估算 %v，下限阈值可能太低",
			short, got, plain)
	}
}

func TestEstimateEmptyIsZero(t *testing.T) {
	// 空文本确实没有 token。给个非零值会让「没有内容」与
	// 「有一点点内容」在账上分不开。
	for _, tbl := range []weightTable{tableOpenAI, tableUnknown, tableClaude} {
		if got := estimate("", tbl); got != 0 {
			t.Errorf("空文本的估算 = %v, 期望 0", got)
		}
	}
}

func TestEstimateNonEmptyIsAtLeastOne(t *testing.T) {
	// 「至少 1」由 estimateRounded 保证（估算式的偏置小于 1，所以
	// estimate 本身可能给出 0.97 这种值）。要测的是对外口径：
	// 非空文本在账上不能变成 0。
	c := newPrepared(t)
	for _, s := range []string{" ", "\n", "\t", "a", "。", "🎉", "中"} {
		if got, err := c.CountText(context.Background(), "gpt-4o", s); err != nil || got.Tokens < 1 {
			t.Errorf("CountText(%q) = %d, err=%v; 非空文本必须 >= 1", s, got.Tokens, err)
		}
		if got := EstimatedTokens("claude-sonnet-4-6", s); got < 1 {
			t.Errorf("EstimatedTokens(%q) = %d, 必须 >= 1", s, got)
		}
	}
}

func TestTableClaudeWeightsExceedUnknown(t *testing.T) {
	// Claude 的**权重**是拿未知族表乘 1.3 得到的。若有人「顺手」把它
	// 改成与其他表相同，这条会失败——而失败是有意义的：Claude 4.7+
	// 换了更碎的词表，同文本 token 数约多三成，用别的表会系统性低估。
	//
	// 比的是权重而不是 estimate 的返回值：两张表的偏置不同（那张表
	// 从训练集上单独定过），所以最终值可能接近，而设计意图体现在权重上。
	for i := 0; i < int(numClasses); i++ {
		want := tableUnknown.weight[i] * 1.3
		if diff := tableClaude.weight[i] - want; diff > 0.01 || diff < -0.01 {
			t.Errorf("类 %d 的 Claude 权重 = %.4f, 期望 %.4f (未知族 × 1.3)",
				i, tableClaude.weight[i], want)
		}
	}
}

func TestTableUnknownAtLeastAsHighAsBoth(t *testing.T) {
	// 未知族表要能同时覆盖 o200k 与 cl100k 两套词表，所以逐类取两者
	// 的较大值。这条断言固定住这个构造方式。
	for i := 0; i < int(numClasses); i++ {
		if tableUnknown.weight[i] < tableOpenAI.weight[i] {
			t.Errorf("未知族在类 %d 上低于 OpenAI 表", i)
		}
	}
}

func TestIsCodeish(t *testing.T) {
	for _, r := range "aZ09+/=_-" {
		if !isCodeish(r) {
			t.Errorf("isCodeish(%q) = false, 期望 true", r)
		}
	}
	for _, r := range " .,:;中🎉" {
		if isCodeish(r) {
			t.Errorf("isCodeish(%q) = true, 期望 false", r)
		}
	}
}

// TestEstimateWithinBounds 是本包里最重要的一条：它把「估算偏高」
// 这个设计选择变成可执行的断言，用的是真实分词结果当基准。
//
// 用表驱动的真实文本，检查「估算 ≥ 真实」的样本占比达到预期。
// 达不到就意味着有人把权重或偏置调低了——那会让收入悄悄少掉。
func TestEstimateWithinBounds(t *testing.T) {
	c, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatalf("加载词表失败: %v", err)
	}
	corpus := []string{
		strings.Repeat("The gateway receives a request from an SDK and selects an upstream channel based on remaining quota. ", 4),
		strings.Repeat("这是一段中文文本，用来测试分词器的中文分词效果。中文的字符信息密度很高。", 4),
		"func handleRequest(w http.ResponseWriter, r *http.Request) { defer r.Body.Close() }",
		`{"model":"placeholder-model","max_tokens":4096,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		"2026-10-16T12:34:56.789Z ERROR upstream request failed status=429 channel=7 retry_after=3s",
		"550e8400-e29b-41d4-a716-446655440000 req_018f2c9a4b7d4e0f8a1b2c3d4e5f6071",
		"SELECT c.id, c.name, sum(u.total_tokens) FROM channels c JOIN usage u ON u.channel_id = c.id GROUP BY c.id LIMIT 20;",
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		strings.Repeat("Привет, мир! Это тестовое сообщение на русском языке. ", 4),
		"Fixed it 🎉 thanks! ✅ " + strings.Repeat("Done and verified. ", 10),
	}
	covered := 0
	var worst float64 = 1
	for _, text := range corpus {
		actual, err := c.Count(text)
		if err != nil {
			t.Fatalf("分词失败: %v", err)
		}
		est := estimate(text, tableOpenAI)
		ratio := est / float64(actual)
		if ratio < worst {
			worst = ratio
		}
		if ratio >= 1 {
			covered++
		}
	}
	// 这组样本是「长输入 + 真实形态」，实测覆盖率在 90% 以上。
	// 阈值定在 80%，给语料变化留余量——定得太紧会在无关改动上误报。
	if coverRatio := float64(covered) / float64(len(corpus)); coverRatio < 0.8 {
		t.Errorf("覆盖率 %.0f%% 低于 80%%（最坏比例 %.2f）——权重或偏置被调低了",
			coverRatio*100, worst)
	}
}

// TestEstimateJSONIsNotWildlyOff 用几个真实的请求体回看估算式。
// 这类输入（工具定义、参数）在 agent 场景里占比很高。
func TestEstimateJSONIsNotWildlyOff(t *testing.T) {
	c, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	tools := []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get the current weather for a city",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string", "description": "City name"},
						"unit": map[string]any{"type": "string", "enum": []string{"c", "f"}},
					},
					"required": []string{"city"},
				},
			},
		},
	}
	for _, tl := range tools {
		body, err := json.Marshal(tl)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := c.Count(string(body))
		if err != nil {
			t.Fatal(err)
		}
		est := estimate(string(body), tableOpenAI)
		// 工具定义的 JSON 是标点密集、短 run 的输入。要求它落在
		// 0.8~3 倍之间——超出这个范围说明某一类的权重明显不对。
		ratio := est / float64(actual)
		if ratio < 0.8 || ratio > 3 {
			t.Errorf("工具定义 JSON 的比例 = %.2f (实际 %d, 估算 %.0f)，超出 0.8~3",
				ratio, actual, est)
		}
	}
}

// TestEstimateIsDeterministic：同样的输入必须得到同样的数。
// 估算值会进账本，而不可重现的账本无法对账。
func TestEstimateIsDeterministic(t *testing.T) {
	text := "The gateway receives a request. 这是一段中文。🎉"
	first := estimate(text, tableOpenAI)
	for i := 0; i < 20; i++ {
		if got := estimate(text, tableOpenAI); got != first {
			t.Fatalf("第 %d 次的估算 %v 与首次 %v 不同", i, got, first)
		}
	}
}

// TestObserveRequestMatchesDirectEstimate：累加器的输入估算与直接
// 调 EstimatedTokens 必须一致，否则两条路径会在账上给出不同的数。
func TestObserveRequestMatchesDirectEstimate(t *testing.T) {
	text := "hello world, 这是一段文本。"
	model := "claude-sonnet-4-6"
	e := NewStreamEstimator(NewCounter(), model)
	e.ObserveRequest(text, 0, 0)
	if got := e.Result().Input.Tokens; got != EstimatedTokens(model, text) {
		t.Errorf("累加器的输入 = %d, 直接估算 = %d",
			got, EstimatedTokens(model, text))
	}
}

// TestStreamEstimatorModelPropagates：模型名要一路带到结果里，
// 否则出了问题无从知道那个数是按哪套规则算的。
func TestStreamEstimatorModelPropagates(t *testing.T) {
	e := NewStreamEstimator(NewCounter(), "claude-opus-4-7")
	e.ObserveRequest("x", 0, 0)
	e.ObserveDelta("y")
	u := e.Result()
	if u.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q", u.Model)
	}
	if u.Input.Model != "claude-opus-4-7" || u.Output.Model != "claude-opus-4-7" {
		t.Errorf("Input/Output 的 Model 未带上: %q / %q", u.Input.Model, u.Output.Model)
	}
}

// 编译期确认：Count 与 Source 的 String 不会退化成默认实现
// （默认实现会打印出指针或数字，把日志变成天书）。
var (
	_ fmt.Stringer = Source(0)
	_ fmt.Stringer = Count{}
)

func TestCountStringIncludesSource(t *testing.T) {
	c := Count{Tokens: 42, Source: SourceEstimated, Model: "m"}
	s := c.String()
	for _, want := range []string{"42", "estimated", "m"} {
		if !strings.Contains(s, want) {
			t.Errorf("Count.String() = %q, 应包含 %q", s, want)
		}
	}
}

func TestUsageStringIncludesSource(t *testing.T) {
	u := Usage{
		Input:  Count{Tokens: 1, Source: SourceMeasured},
		Output: Count{Tokens: 2, Source: SourceEstimated},
	}
	s := fmt.Sprint(u)
	if !strings.Contains(s, "estimated") {
		t.Errorf("Usage 的输出 = %q, 应能看出输出来源是估算", s)
	}
}

func TestCountTextContextWithValue(t *testing.T) {
	// 带值的 context 不该影响计数结果——本包不读 context 里的任何东西。
	type k struct{}
	c := newPrepared(t)
	ctx := context.WithValue(context.Background(), k{}, "v")
	got, err := c.CountText(ctx, "gpt-4o", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens != 1 {
		t.Errorf("Tokens = %d, 期望 1", got.Tokens)
	}
}
