package idempotency

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 本文件钉的是 NativeResidue：直通模式下「原文里没进 hashInput 的那部分」
// 怎么进指纹。
//
// # 每条钉都要说清它证明的是哪一类
//
// 残差的意义只在一种情况下成立：两份原文在 hashInput 的**已知部分**
// （Protocol / Model / Messages / MaxTokens / Temperature / TopP /
// ReasoningEffort / Tools / Stream / Extra）上逐字段相同，差别只落在
// 残差覆盖的那些键上。否则指纹不同本来就有别的解释，这条钉证明不到残差。
//
// 于是分裂钉一律先断言前提（见 requireSameKnownPart），再断言指纹不同。
// 前提本身也是一条钉：将来有人改了 hashInput 或解析结果，让某条用例不再
// 满足前提，它会立刻红在这里，而不是静默变成一条什么也证明不了的绿用例。
//
// 等价钉反过来：它要证明「两份原文确实不同」（!bytes.Equal），而指纹相同。
// 两份原文一模一样时指纹当然相同，那是废话。
//
// # 为什么不 import 协议转换层
//
// 本包的测试也只认 channel 的类型：幂等包是存储口径，不该把协议转换层
// 拖成依赖。所以下面每条用例的**已知部分**都是手工构造的 channel.Request，
// 注释里写明它对应哪种原文写法。那侧解析出来的结果由解析层与入口层自己的
// 用例钉住，不在这里重复。

// requestHash 算一次请求的指纹，失败即终止用例。
func requestHash(t *testing.T, protocol channel.Protocol, req *channel.Request) [32]byte {
	t.Helper()
	h, err := HashRequest(protocol, req)
	if err != nil {
		t.Fatalf("算指纹失败: %v", err)
	}
	return h
}

// residueOf 取出这次请求算出来的残差。
func residueOf(t *testing.T, protocol channel.Protocol, req *channel.Request) map[string]any {
	t.Helper()
	in, err := newHashInput(protocol, req)
	if err != nil {
		t.Fatalf("构造 hashInput 失败: %v", err)
	}
	return in.NativeResidue
}

// knownPartOf 取出 hashInput 的已知部分（残差置空）。
func knownPartOf(t *testing.T, protocol channel.Protocol, req *channel.Request) hashInput {
	t.Helper()
	in, err := newHashInput(protocol, req)
	if err != nil {
		t.Fatalf("构造 hashInput 失败: %v", err)
	}
	in.NativeResidue = nil
	return in
}

// requireSameKnownPart 断言两份请求在残差之外的字段上逐字段相同。
//
// 这是分裂钉的前提：不同就说明差异本来就在已知部分里，指纹分裂与残差无关，
// 那条钉什么也证明不了。
func requireSameKnownPart(t *testing.T, protocol channel.Protocol, a, b *channel.Request) {
	t.Helper()
	ka := knownPartOf(t, protocol, a)
	kb := knownPartOf(t, protocol, b)
	if !reflect.DeepEqual(ka, kb) {
		t.Fatalf("前提不成立：两条原文的 hashInput 已知部分不同，这条钉证明不了 residue\n  A %+v\n  B %+v", ka, kb)
	}
}

// requireDifferentBodies 断言两份原文确实不同。
//
// 等价钉的前提：两份原文完全一样时残差当然相同，那样的用例不证明任何事。
func requireDifferentBodies(t *testing.T, a, b string) {
	t.Helper()
	if bytes.Equal([]byte(a), []byte(b)) {
		t.Fatalf("前提不成立：两份原文逐字节相同，这条钉只能证明「自己等于自己」")
	}
}

// ── 零值不变 ───────────────────────────────────────────────────────────

// NativeBody 为 nil 时，指纹与加 NativeResidue 之前**逐字节**相同。
//
// 断言的是字面量而不是「跑两次一样」：跑两次一样只能证明确定，证明不了没变。
// 这个值写在 docs/api.md 的公开契约旁边（同一份请求两次得到同一个指纹），
// 一个恒为 null 的新字段也会把它改掉，所以必须钉死。
//
// 变异：把 omitempty 去掉，或让 nativeResidueOf 在 nil 时返回空 map 而不是
// nil，这一条当场红。
func TestHashRequestWithoutNativeBodyKeepsFingerprint(t *testing.T) {
	req := baseRequest()
	if req.NativeBody != nil {
		t.Fatal("precondition: baseRequest 不该带 NativeBody")
	}

	got := requestHash(t, channel.ProtocolOpenAI, req)
	const want = "79bfa4259003d8deb8030c0f0f33f0cb0be401c497d4bc938334ef61cd31ced5"
	if h := hexString(got[:]); h != want {
		t.Errorf("NativeBody 为 nil 时指纹变了：\n  得到 %s\n  期望 %s\n"+
			"没有原文时残差必须留零值：指纹是公开契约，加一个恒为 null 的字段也会改掉它", h, want)
	}
	if residue := residueOf(t, channel.ProtocolOpenAI, req); residue != nil {
		t.Errorf("NativeBody 为 nil 时残差 = %v，期望 nil", residue)
	}

	// 序列化结果里不能出现新键。哈希值相同已经蕴含这一点，但直接断言
	// 键名能让失败信息指向「多了一个键」，而不是一串对不上的十六进制。
	in, err := newHashInput(channel.ProtocolOpenAI, req)
	if err != nil {
		t.Fatalf("构造 hashInput 失败: %v", err)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化 hashInput 失败: %v", err)
	}
	if bytes.Contains(raw, []byte("native_residue")) {
		t.Errorf("没有原文时序列化结果里出现了 native_residue: %s", raw)
	}
}

// 原文非法时报错，不 panic、不落回空残差。
//
// # 为什么是报错而不是退回 nil 残差
//
// 落回 nil 残差意味着「这份原文里没有任何影响上游的字段」——一个我们并不知道
// 的事实。它与「原文解析不出来」在指纹上完全一样，于是两份不同的、都解析不
// 出来的原文会共用指纹。报错的方向与 HashRequest 的既有取舍一致：宁可不做
// 幂等，也不要让两个不同的请求共用一个指纹。
//
// 生产路径走不到这里：NativeBody 由 relay.ParseRequest 在解析成功之后才挂上，
// 能挂上就说明它是合法 JSON。这条钉守的是「将来有其他填充点」。
func TestHashRequestRejectsMalformedNativeBody(t *testing.T) {
	req := baseRequest()
	req.NativeBody = []byte(`{"model":`)

	got, err := HashRequest(channel.ProtocolOpenAI, req)
	if err == nil {
		t.Fatalf("原文不是合法 JSON 时应当报错，却返回了指纹 %x", got)
	}
	if got != ([32]byte{}) {
		t.Errorf("报错时不该返回非零指纹: %x", got)
	}
}

// ── 等价：差异被扣掉了 ─────────────────────────────────────────────────

// Anthropic 的 system 三种写法是同一个请求。
//
// 三种写法的已知部分相同（顶层的 system 经枢纽折进 Messages 的首条）：
//
//	字符串            {"system":"你是助手", ...}
//	块数组            {"system":[{"type":"text","text":"你是助手"}], ...}
//	直接写在消息里    {"messages":[{"role":"system","content":"你是助手"}, ...]}
//
// 断言三件事：原文确实不同、残差被扣空、指纹相同。残差必须是空——只要有一种
// 写法没扣干净，它就非空，指纹也就分开了。
func TestHashRequestAnthropicSystemFormsShareFingerprint(t *testing.T) {
	bodies := []string{
		`{"model":"claude-x","max_tokens":4096,"system":"你是助手","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"claude-x","max_tokens":4096,"system":[{"type":"text","text":"你是助手"}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"claude-x","max_tokens":4096,"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"hi"}]}`,
	}
	// 这三种写法经枢纽都得到这份请求（见解析层的用例）。
	known := &channel.Request{
		Protocol: channel.ProtocolAnthropic,
		Model:    "claude-x",
		Messages: []channel.Message{
			{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "你是助手"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		MaxTokens: 4096,
	}

	var want [32]byte
	for i, body := range bodies {
		req := *known
		req.NativeBody = []byte(body)
		got := requestHash(t, channel.ProtocolAnthropic, &req)
		if residue := residueOf(t, channel.ProtocolAnthropic, &req); residue != nil {
			t.Errorf("第 %d 种写法的残差 = %v，期望 nil——没扣干净就会与别的写法分开", i+1, residue)
		}
		if i == 0 {
			want = got
			continue
		}
		requireDifferentBodies(t, bodies[0], body)
		if got != want {
			t.Errorf("第 %d 种写法与字符串写法指纹不同：\n  %x\n  %x\n"+
				"三种写法在枢纽那边是同一个请求，指纹必须相同", i+1, got, want)
		}
	}
}

// Responses 的 instructions 字符串与 input 里 role=system 的第一项是同一个请求。
func TestHashRequestResponsesInstructionsMatchSystemMessage(t *testing.T) {
	const withInstructions = `{"model":"gpt-x","instructions":"你是助手","input":"hi"}`
	const withSystemItem = `{"model":"gpt-x","input":[{"role":"system","content":"你是助手"},{"role":"user","content":"hi"}]}`
	requireDifferentBodies(t, withInstructions, withSystemItem)

	known := &channel.Request{
		Protocol: channel.ProtocolOpenAIResponses,
		Model:    "gpt-x",
		Messages: []channel.Message{
			{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "你是助手"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	a, b := *known, *known
	a.NativeBody = []byte(withInstructions)
	b.NativeBody = []byte(withSystemItem)
	requireSameKnownPart(t, channel.ProtocolOpenAIResponses, &a, &b)

	for name, req := range map[string]*channel.Request{"instructions": &a, "input 里的 system": &b} {
		if residue := residueOf(t, channel.ProtocolOpenAIResponses, req); residue != nil {
			t.Errorf("%s 写法的残差 = %v，期望 nil", name, residue)
		}
	}
	if ha, hb := requestHash(t, channel.ProtocolOpenAIResponses, &a), requestHash(t, channel.ProtocolOpenAIResponses, &b); ha != hb {
		t.Errorf("两种写法指纹不同：\n  %x\n  %x", ha, hb)
	}
}

// chat 的 max_tokens 与 max_completion_tokens 是同一个上界的两个名字。
//
// 两个键都落在 MaxTokens 上，所以残差里两个都不能留：留一个就说明扣键表
// 只认了一个名字，而调用方换一个名字重发时会被判成新请求。
func TestHashRequestChatTokenLimitAliasesShareFingerprint(t *testing.T) {
	const legacy = `{"model":"gpt-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	const current = `{"model":"gpt-x","max_completion_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	requireDifferentBodies(t, legacy, current)

	known := &channel.Request{
		Protocol:  channel.ProtocolOpenAI,
		Model:     "gpt-x",
		Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 100,
	}
	a, b := *known, *known
	a.NativeBody = []byte(legacy)
	b.NativeBody = []byte(current)
	requireSameKnownPart(t, channel.ProtocolOpenAI, &a, &b)

	for name, req := range map[string]*channel.Request{"max_tokens": &a, "max_completion_tokens": &b} {
		if residue := residueOf(t, channel.ProtocolOpenAI, req); residue != nil {
			t.Errorf("%s 写法的残差 = %v，期望 nil", name, residue)
		}
	}
	if ha, hb := requestHash(t, channel.ProtocolOpenAI, &a), requestHash(t, channel.ProtocolOpenAI, &b); ha != hb {
		t.Errorf("两个上界名字算出了不同指纹：\n  %x\n  %x", ha, hb)
	}
}

// ── 分裂：A 类（枢纽读且无损）—— 这一条不是 residue 钉 ────────────────

// Responses 的 reasoning.effort 不同时必须分裂。
//
// # 它属于 A 类，不是 residue 钉
//
// 枢纽把 effort 无损搬进 ReasoningEffort，所以两份的**已知部分本来就不同**，
// 指纹分裂由 hashInput 自己完成——与残差无关，把 effort 留在残差里也一样分裂。
//
// 那为什么还要写这一条：它守的是另一件事——「reasoning.effort 被扣掉了」。
// 这是 A 类扣键的正确性钉（扣了不改变指纹），也是下面「只扣 effort、留下
// budget_tokens」那条的对照：同一个 reasoning 对象里两个子键，一个该扣、
// 一个该留。
//
// 所以这里**不加**「已知部分相同」的前提——加了会假红：两者本来就不同，
// 那正是 A 类的定义。
func TestHashRequestResponsesReasoningEffortSplits(t *testing.T) {
	const low = `{"model":"gpt-x","input":"hi","reasoning":{"effort":"low"}}`
	const high = `{"model":"gpt-x","input":"hi","reasoning":{"effort":"high"}}`
	requireDifferentBodies(t, low, high)

	knownLow := channel.Request{
		Protocol:        channel.ProtocolOpenAIResponses,
		Model:           "gpt-x",
		Messages:        []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
		ReasoningEffort: "low",
	}
	knownHigh := knownLow
	knownHigh.ReasoningEffort = "high"
	a, b := knownLow, knownHigh
	a.NativeBody = []byte(low)
	b.NativeBody = []byte(high)

	// 扣键正确：effort 已经进了 ReasoningEffort，残差里不能还有它
	// （reasoning 对象删空之后整个键也不该留）。
	for name, req := range map[string]*channel.Request{"low": &a, "high": &b} {
		if residue := residueOf(t, channel.ProtocolOpenAIResponses, req); residue != nil {
			t.Errorf("%s 的残差 = %v，期望 nil——effort 已经由 ReasoningEffort 携带", name, residue)
		}
	}

	if ha, hb := requestHash(t, channel.ProtocolOpenAIResponses, &a), requestHash(t, channel.ProtocolOpenAIResponses, &b); ha == hb {
		t.Errorf("reasoning.effort 不同却算出了同一个指纹（%x）", ha)
	}
}

// ── 分裂：B 类（枢纽读到了，但读得粗） ─────────────────────────────────

// Anthropic thinking.budget_tokens 512 与 1024 必须分裂。
//
// # 为什么取这一对值
//
// 枢纽把预算折成四档 effort（≤1024 是 low、≤8192 是 medium、其余 high），
// 512 与 1024 落在**同一档**，算出来的 ReasoningEffort 都是 low，已知部分
// 逐字段相同——差别只有残差里的那个数字。
//
// 换成跨档的一对（1024 与 1025）就钉不住了：那时枢纽的 effort 是 low 与
// medium，指纹本来就由 ReasoningEffort 分开，哪怕有人把整个 thinking 键
// 扣掉，用例也照样绿。那样的绿证明的是「hashInput 里有 ReasoningEffort」，
// 不是「残差留住了预算」。
//
// 这一条是 B 类存在的唯一理由：扣掉就是假合并，两份不同预算的直通报文会
// 共用指纹。
func TestHashRequestAnthropicThinkingBudgetSplitsWithinEffortBand(t *testing.T) {
	const budget512 = `{"model":"claude-x","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":512},"messages":[{"role":"user","content":"hi"}]}`
	const budget1024 = `{"model":"claude-x","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
	requireDifferentBodies(t, budget512, budget1024)

	// 枢纽对这两个预算都给出 low（见上）。已知部分必须逐字段相同，
	// 否则这条钉证明不到残差。
	known := &channel.Request{
		Protocol:        channel.ProtocolAnthropic,
		Model:           "claude-x",
		Messages:        []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens:       4096,
		ReasoningEffort: "low",
	}
	a, b := *known, *known
	a.NativeBody = []byte(budget512)
	b.NativeBody = []byte(budget1024)
	requireSameKnownPart(t, channel.ProtocolAnthropic, &a, &b)

	residueA := residueOf(t, channel.ProtocolAnthropic, &a)
	thinking, ok := residueA["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("残差里没有 thinking 对象：%v——预算没有被留下来", residueA)
	}
	if got := thinking["budget_tokens"]; got != float64(512) {
		t.Errorf("残差里的 budget_tokens = %v，期望 512", got)
	}

	if ha, hb := requestHash(t, channel.ProtocolAnthropic, &a), requestHash(t, channel.ProtocolAnthropic, &b); ha == hb {
		t.Errorf("预算 512 与 1024 算出了同一个指纹（%x）：\n"+
			"枢纽把它们折成同一档 effort，只有残差能分开——扣掉预算就是让两份不同的报文共用指纹", ha)
	}
}

// Responses reasoning.budget_tokens 512 与 1024 必须分裂。
//
// 与 Anthropic 那条同源：走的是同一套预算折算，两个值落在同一档，已知部分
// （含 ReasoningEffort）逐字段相同。只扣 reasoning.effort、把 budget_tokens
// 留给残差，这一对才会分开。
func TestHashRequestResponsesReasoningBudgetSplitsWithinEffortBand(t *testing.T) {
	const budget512 = `{"model":"gpt-x","input":"hi","reasoning":{"budget_tokens":512}}`
	const budget1024 = `{"model":"gpt-x","input":"hi","reasoning":{"budget_tokens":1024}}`
	requireDifferentBodies(t, budget512, budget1024)

	// 只写 budget_tokens、不写 effort 时，枢纽算出的 ReasoningEffort 是空串
	// （两个预算都一样）。
	known := &channel.Request{
		Protocol: channel.ProtocolOpenAIResponses,
		Model:    "gpt-x",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
	a, b := *known, *known
	a.NativeBody = []byte(budget512)
	b.NativeBody = []byte(budget1024)
	requireSameKnownPart(t, channel.ProtocolOpenAIResponses, &a, &b)

	residueA := residueOf(t, channel.ProtocolOpenAIResponses, &a)
	reasoning, ok := residueA["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("残差里没有 reasoning 对象：%v——预算没有被留下来", residueA)
	}
	if got := reasoning["budget_tokens"]; got != float64(512) {
		t.Errorf("残差里的 budget_tokens = %v，期望 512", got)
	}

	if ha, hb := requestHash(t, channel.ProtocolOpenAIResponses, &a), requestHash(t, channel.ProtocolOpenAIResponses, &b); ha == hb {
		t.Errorf("预算 512 与 1024 算出了同一个指纹（%x）", ha)
	}
}

// ── 分裂：C 类（枢纽读不到） ───────────────────────────────────────────

// cache_control 落在 messages 的块内层时必须分裂。
//
// # 钉的是内层，不是顶层
//
// 顶层的 cache_control 与它不在一个位置：这条要证明的是「块内层的键也走到了
// 残差」。只钉顶层的话，一个「只处理顶层、块数组整段扣掉」的实现照样绿。
//
// 它是 C 类的典型：上游转换器确实读 cache_control（它决定提示缓存怎么切），
// 而 channel.ContentBlock 没有这个字段，hashInput 里也就没有。扣掉就是
// 丢信息——直通会把原文原样发上游，上游分得出加没加缓存标记。
func TestHashRequestCacheControlInsideMessageSplits(t *testing.T) {
	const plain = `{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	const cached = `{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`
	requireDifferentBodies(t, plain, cached)

	known := &channel.Request{
		Protocol:  channel.ProtocolAnthropic,
		Model:     "claude-x",
		Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
		MaxTokens: 4096,
	}
	a, b := *known, *known
	a.NativeBody = []byte(plain)
	b.NativeBody = []byte(cached)
	requireSameKnownPart(t, channel.ProtocolAnthropic, &a, &b)

	if residue := residueOf(t, channel.ProtocolAnthropic, &a); residue != nil {
		t.Errorf("不加 cache_control 的那一份残差 = %v，期望 nil（块里没有 leftover 键）", residue)
	}
	if residue := residueOf(t, channel.ProtocolAnthropic, &b); residue == nil {
		t.Error("加了 cache_control 的那一份残差为空——这个键指不到 hashInput 的任何字段，必须留下")
	}
	if ha, hb := requestHash(t, channel.ProtocolAnthropic, &a), requestHash(t, channel.ProtocolAnthropic, &b); ha == hb {
		t.Errorf("块内层的 cache_control 没有让指纹分裂（%x）：直通会把它发给上游", ha)
	}
}

// messages 里带/不带一个 type=thinking 的块必须分裂。
//
// 枢纽把这个块整块丢掉（它不是对话正文），所以已知部分完全相同；直通却会
// 原样发给上游。扣掉残差里这一块，就等于把「带思考历史」与「不带」算成
// 同一个请求。
func TestHashRequestThinkingBlockSplits(t *testing.T) {
	const plain = `{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"answer"}]}]}`
	const withThinking = `{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"text","text":"answer"}]}]}`
	requireDifferentBodies(t, plain, withThinking)

	known := &channel.Request{
		Protocol: channel.ProtocolAnthropic,
		Model:    "claude-x",
		Messages: []channel.Message{
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []channel.ContentBlock{{Type: "text", Text: "answer"}}},
		},
		MaxTokens: 4096,
	}
	a, b := *known, *known
	a.NativeBody = []byte(plain)
	b.NativeBody = []byte(withThinking)
	requireSameKnownPart(t, channel.ProtocolAnthropic, &a, &b)

	if residue := residueOf(t, channel.ProtocolAnthropic, &b); residue == nil {
		t.Error("带 thinking 块的那一份残差为空——枢纽丢掉的内容在直通里会发往上游，必须留下")
	}
	if ha, hb := requestHash(t, channel.ProtocolAnthropic, &a), requestHash(t, channel.ProtocolAnthropic, &b); ha == hb {
		t.Errorf("thinking 块没有让指纹分裂（%x）", ha)
	}
}

// Responses input[] 项上的 id / status 必须分裂。
//
// 两者枢纽读到并忽略（回传历史时它们对上游有意义），hashInput 里没有对应
// 字段，所以留在残差。这是保守方向：宁可多分裂，也不要让两份不同原文共用
// 指纹。
func TestHashRequestResponsesInputItemMetadataSplits(t *testing.T) {
	const base = `{"model":"gpt-x","input":[{"type":"message","role":"user","content":"hi"}]}`
	cases := []struct {
		name  string
		body  string
		extra string
	}{
		{
			name:  "id",
			body:  `{"model":"gpt-x","input":[{"type":"message","id":"msg_1","role":"user","content":"hi"}]}`,
			extra: "id",
		},
		{
			name:  "status",
			body:  `{"model":"gpt-x","input":[{"type":"message","status":"completed","role":"user","content":"hi"}]}`,
			extra: "status",
		},
	}

	known := &channel.Request{
		Protocol: channel.ProtocolOpenAIResponses,
		Model:    "gpt-x",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
	baseReq := *known
	baseReq.NativeBody = []byte(base)
	baseHash := requestHash(t, channel.ProtocolOpenAIResponses, &baseReq)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireDifferentBodies(t, base, tc.body)
			req := *known
			req.NativeBody = []byte(tc.body)
			requireSameKnownPart(t, channel.ProtocolOpenAIResponses, &baseReq, &req)

			if residue := residueOf(t, channel.ProtocolOpenAIResponses, &req); residue == nil {
				t.Fatalf("带 %s 的那一份残差为空——这个键指不到 hashInput 的任何字段，必须留下", tc.extra)
			}
			if got := requestHash(t, channel.ProtocolOpenAIResponses, &req); got == baseHash {
				t.Errorf("input[] 项上的 %s 没有让指纹分裂（%x）", tc.extra, got)
			}
		})
	}
}

// 枢纽读不到的顶层键必须各自分裂，且与不带该键的那一份也分裂。
//
// 这四组覆盖三条入口上「另一种协议的名字」与「本协议里枢纽不搬的参」：
//
//	Anthropic max_tokens_to_sample   —— 旧名字，枢纽只读 max_tokens
//	Anthropic top_k                  —— 采样参数，hashInput 里没有
//	Responses 顶层 reasoning_effort  —— chat 的形状，这条入口认的是 reasoning.effort
//	chat max_output_tokens           —— Responses 的名字，这条入口不认
//
// 每一组都先断言已知部分相同，再断言指纹不同。跨协议地拿这些键去扣，就是
// 把「枢纽没读」当成「上游不需要」，而直通把原文原样发出去。
func TestHashRequestUnreadTopLevelKeysSplit(t *testing.T) {
	cases := []struct {
		name     string
		protocol channel.Protocol
		known    *channel.Request
		base     string
		variant  string
	}{
		{
			name:     "Anthropic max_tokens_to_sample",
			protocol: channel.ProtocolAnthropic,
			known: &channel.Request{
				Protocol:  channel.ProtocolAnthropic,
				Model:     "claude-x",
				Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
				MaxTokens: 10,
			},
			base:    `{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			variant: `{"model":"claude-x","max_tokens":10,"max_tokens_to_sample":999,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "Anthropic top_k",
			protocol: channel.ProtocolAnthropic,
			known: &channel.Request{
				Protocol:  channel.ProtocolAnthropic,
				Model:     "claude-x",
				Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
				MaxTokens: 10,
			},
			base:    `{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			variant: `{"model":"claude-x","max_tokens":10,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "Responses 顶层 reasoning_effort",
			protocol: channel.ProtocolOpenAIResponses,
			known: &channel.Request{
				Protocol: channel.ProtocolOpenAIResponses,
				Model:    "gpt-x",
				Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
			},
			base:    `{"model":"gpt-x","input":"hi"}`,
			variant: `{"model":"gpt-x","input":"hi","reasoning_effort":"high"}`,
		},
		{
			name:     "chat max_output_tokens",
			protocol: channel.ProtocolOpenAI,
			known: &channel.Request{
				Protocol:  channel.ProtocolOpenAI,
				Model:     "gpt-x",
				Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
				MaxTokens: 10,
			},
			base:    `{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			variant: `{"model":"gpt-x","max_tokens":10,"max_output_tokens":999,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireDifferentBodies(t, tc.base, tc.variant)

			baseReq := *tc.known
			baseReq.NativeBody = []byte(tc.base)
			variantReq := *tc.known
			variantReq.NativeBody = []byte(tc.variant)
			requireSameKnownPart(t, tc.protocol, &baseReq, &variantReq)

			if residue := residueOf(t, tc.protocol, &baseReq); residue != nil {
				t.Errorf("不带该键的那一份残差 = %v，期望 nil", residue)
			}
			if residue := residueOf(t, tc.protocol, &variantReq); residue == nil {
				t.Errorf("带 %s 的那一份残差为空——枢纽读不到它，直通却会原样发上游", tc.name)
			}
			if hb, hv := requestHash(t, tc.protocol, &baseReq), requestHash(t, tc.protocol, &variantReq); hb == hv {
				t.Errorf("%s 没有让指纹分裂（%x）", tc.name, hv)
			}
		})
	}
}

// system 的块数组里带 cache_control 时不能扣。
//
// 这一条是等价钉的**反面**：system 的字符串写法与纯 text 块数组扣成同一个
// 指纹（上面那条），而块里多一个 cache_control 就必须分开——那个键指不到
// hashInput 的任何字段。两种写法同时存在，才说明扣键判据是「每块只有
// type+text」而不是「system 写了就扣」。
func TestHashRequestAnthropicSystemCacheControlSplits(t *testing.T) {
	const plain = `{"model":"claude-x","max_tokens":4096,"system":[{"type":"text","text":"你是助手"}],"messages":[{"role":"user","content":"hi"}]}`
	const cached = `{"model":"claude-x","max_tokens":4096,"system":[{"type":"text","text":"你是助手","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	requireDifferentBodies(t, plain, cached)

	known := &channel.Request{
		Protocol: channel.ProtocolAnthropic,
		Model:    "claude-x",
		Messages: []channel.Message{
			{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "你是助手"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		MaxTokens: 4096,
	}
	a, b := *known, *known
	a.NativeBody = []byte(plain)
	b.NativeBody = []byte(cached)
	requireSameKnownPart(t, channel.ProtocolAnthropic, &a, &b)

	if residue := residueOf(t, channel.ProtocolAnthropic, &b); residue == nil {
		t.Error("system 块里的 cache_control 没进残差——它指不到 hashInput 的任何字段")
	}
	if ha, hb := requestHash(t, channel.ProtocolAnthropic, &a), requestHash(t, channel.ProtocolAnthropic, &b); ha == hb {
		t.Errorf("system 块里的 cache_control 没有让指纹分裂（%x）", ha)
	}
}

// ── 边界：只认已知结构 ─────────────────────────────────────────────────

// 认不出的消息形状整条留残差，一个键都不删。
//
// 判据是「要么全扣、要么全不动」：只删认识的几个键、把剩下的当残差，等于
// 在这里另造一张「枢纽的解析器」；它一旦与真实解析器分岔，表现是假合并
// （两份不同报文共用指纹），而假合并看不出来。
//
// 这里用 Anthropic 的 document 块：它是管道表达不了的块类型，枢纽不转，
// 直通会原样发给上游。整条消息（含 role 与 content）都必须还在。
func TestHashRequestKeepsUnrecognizedMessageShapeWhole(t *testing.T) {
	const body = `{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}}]}]}`
	req := &channel.Request{
		Protocol:  channel.ProtocolAnthropic,
		Model:     "claude-x",
		Messages:  []channel.Message{{Role: "user", Content: nil}},
		MaxTokens: 4096,
	}
	req.NativeBody = []byte(body)

	residue := residueOf(t, channel.ProtocolAnthropic, req)
	messages, ok := residue["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("残差里的 messages = %v，期望整条留下", residue["messages"])
	}
	msg, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("残差里的消息不是对象: %T", messages[0])
	}
	if _, ok := msg["role"]; !ok {
		t.Error("整条留下的消息里不该删掉 role：形状认不出时一个键都不删")
	}
	if _, ok := msg["content"]; !ok {
		t.Error("整条留下的消息里不该删掉 content：块类型认不出时整条原样留")
	}
}

// hexString 把指纹转成十六进制串，只给断言与失败信息用。
func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}
