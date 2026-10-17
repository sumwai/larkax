package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// sessionFor 编译一个插件并建好会话。hooks 与 meta 是原样插入的 JS 片段。
func sessionFor(t *testing.T, meta, hooks string, host sandbox.Host, limits sandbox.Limits) *Session {
	t.Helper()
	reg := mustPublish(t, Registration{Key: "p1", Source: src(meta, hooks)})
	s, err := reg.NewSession(t.Context(), SessionRequest{CanonicalModel: "m", Protocol: "openai"}, host, limits)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// ── onRequest ─────────────────────────────────────────────────────────

func TestOnRequestReturnsPatch(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onRequest(ctx){
			return { body: { temperature: 0.1, max_tokens: 64 },
			         headers: { "x-larkax-tenant": ctx.user.id === 7 ? "t7" : "?" } };
		}`, sandbox.Host{}, sandbox.Limits{})

	res := s.OnRequest(t.Context(), RequestContext{RequestID: "r1", User: RequestUser{ID: 7}})
	if err := res.Err(); err != nil {
		t.Fatalf("出错: %v", err)
	}
	patch := res.Patch
	if patch == nil || patch.Body == nil {
		t.Fatal("该返回补丁")
	}
	if patch.Body.Temperature == nil || *patch.Body.Temperature != 0.1 {
		t.Errorf("temperature = %s", showPtr(patch.Body.Temperature))
	}
	if patch.Body.MaxTokens == nil || *patch.Body.MaxTokens != 64 {
		t.Errorf("max_tokens = %s", showPtr(patch.Body.MaxTokens))
	}
	if patch.Headers["x-larkax-tenant"] != "t7" {
		t.Errorf("headers = %v", patch.Headers)
	}
}

// 插件改模型的尝试到不了任何地方：RequestBodyPatch 里没有 model 字段，
// 严格解析会拒掉整份补丁。
//
// 这条钉住「不能碰钱」在实现上的落点——**不是过滤，是表达不出来**。
//
// 越界按插件失败处置（非阻断时请求继续、补丁整个丢掉），而不是让请求失败：
// 硬约束写的是「插件出错不能让请求失败，除非显式声明为阻断型」，
// 而越界是一种插件出错。让它在阻断型插件上生效见下面那条。
func TestOnRequestCannotChangeModel(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onRequest(){ return { body: { model: "更贵的模型" } }; }`, sandbox.Host{}, sandbox.Limits{})

	res := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"})
	if err := res.Err(); err != nil {
		t.Fatalf("非阻断型插件越界不该让请求失败，实际 %v", err)
	}
	if res.Patch != nil {
		t.Fatalf("越界时整份补丁都该丢掉（不是只丢掉越界的那一项），实际 %+v", res.Patch)
	}
	f := res.Failure
	if f == nil {
		t.Fatal("该记一条失败")
	}
	if !errors.Is(f, sandbox.ErrBadResult) {
		t.Errorf("该认出是「返回值不符合契约」，实际 kind=%v", f.Kind)
	}
	if !errors.Is(f, ErrPatchScope) {
		t.Errorf("该认出具体是越界，实际 cause=%v", f.Cause)
	}
	if f.Plugin != "p1" || f.Hook != HookRequest {
		t.Errorf("失败里该带上是哪个插件的哪个 hook，实际 %+v", f)
	}
}

// 阻断型插件越界时请求失败：作者主动要了「我的判断必须成立」这个语义。
func TestOnRequestOutOfScopeIsBlockingWhenDeclared(t *testing.T) {
	s := sessionFor(t, `key:"p1", apiVersion:1, blocking:true,`,
		`onRequest(){ return { body: { model: "更贵的模型" } }; }`, sandbox.Host{}, sandbox.Limits{})
	err := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"}).Err()
	if err == nil {
		t.Fatal("阻断型插件越界时请求该失败")
	}
	if !errors.Is(err, ErrPatchScope) {
		t.Errorf("错误里该能认出越界，实际 %v", err)
	}
}

// 插件什么都不返回时不给补丁，也不算失败。
func TestOnRequestWithoutPatch(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRequest(){ return; }`, sandbox.Host{}, sandbox.Limits{})
	res := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"})
	if res.Failed() || res.Patch != nil {
		t.Fatalf("res=%+v，期望「没有补丁、没有错误」", res)
	}
}

// 空补丁（字段全为 nil）归一成「没给补丁」，调用方不必判两次。
func TestOnRequestEmptyPatchNormalized(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRequest(){ return { body: {} }; }`, sandbox.Host{}, sandbox.Limits{})
	res := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"})
	if res.Failed() || res.Patch != nil {
		t.Fatalf("res=%+v，期望归一成没有补丁", res)
	}
}

// ── 失败策略 ──────────────────────────────────────────────────────────

// 插件抛异常时请求**不失败**，插件给的改动**也不生效**。
//
// 「丢掉整份补丁」而不是「用部分改动」：部分应用过的补丁是没法解释的状态，
// 插件作者会看到「有的字段生效了、有的没有」——比整个不生效难查得多。
func TestThrowingPluginDoesNotFailRequestByDefault(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRequest(){ throw new Error("插件自己的毛病"); }`,
		sandbox.Host{}, sandbox.Limits{})

	res := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"})
	if err := res.Err(); err != nil {
		t.Fatalf("非阻断型插件失败不该让请求失败，实际 %v", err)
	}
	if res.Patch != nil {
		t.Errorf("失败时不该有补丁，实际 %v", res.Patch)
	}
	// 但失败必须被看见。
	fails := s.Failures()
	if len(fails) != 1 || !errors.Is(fails[0], sandbox.ErrPluginThrew) {
		t.Fatalf("失败该被记录下来，实际 %v", fails)
	}
	if fails[0].Plugin != "p1" || fails[0].Hook != HookRequest {
		t.Errorf("失败里该带上是哪个插件的哪个 hook，实际 %+v", fails[0])
	}
	if fails[0].Hook != HookRequest {
		t.Errorf("hook 名不该带上 default. 前缀，实际 %q", fails[0].Hook)
	}
}

// 显式声明 blocking 的插件失败时请求**失败**。
//
// 「显式声明为阻断型」是硬约束里唯一的例外，所以它必须是**作者主动要的**，
// 而不是宿主猜的。
func TestBlockingPluginFailsRequest(t *testing.T) {
	s := sessionFor(t, `key:"p1", apiVersion:1, blocking: true,`,
		`onRequest(){ throw new Error("阻断型"); }`, sandbox.Host{}, sandbox.Limits{})

	err := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"}).Err()
	if err == nil {
		t.Fatal("阻断型插件失败时请求该失败")
	}
	if !errors.Is(err, sandbox.ErrPluginThrew) {
		t.Errorf("错误里该能认出底层原因，实际 %v", err)
	}
}

// 阻断型失败之后剩下的 hook 不再调：runtime 已经不可信。
func TestFailureStopsLaterHooks(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onRequest(){ throw new Error("x"); },
		 onUsage(){ return { promptTokens: 999 }; }`,
		sandbox.Host{}, sandbox.Limits{})

	if err := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"}).Err(); err != nil {
		t.Fatalf("非阻断失败不该报错: %v", err)
	}
	// 抛异常之后 runtime 还能用吗？可以用——异常不会丢弃 job，只有中断会。
	// 所以这里应当照常调用，并拿到它报的用量。
	res := s.OnUsage(t.Context(), UsageContext{RequestID: "r1"})
	if err := res.Err(); err != nil {
		t.Fatalf("onUsage 出错: %v", err)
	}
	if res.Patch == nil || res.Patch.PromptTokens == nil || *res.Patch.PromptTokens != 999 {
		t.Errorf("onUsage 该照常返回，实际 %+v", res.Patch)
	}
	if len(s.Failures()) != 1 {
		t.Errorf("失败记录该只有 onRequest 那一条，实际 %v", s.Failures())
	}
}

// 超时之后剩下的 hook 不再调：中断会丢弃排队中的 job，runtime 不可信。
func TestTimeoutStopsLaterHooks(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onStream(){ while(true){} },
		 onUsage(){ return { promptTokens: 999 }; }`,
		sandbox.Host{}, sandbox.Limits{CallTimeout: 30 * time.Millisecond})

	// onStream 不存在于这个插件里？不，它导出了。触发超时。
	if err := s.OnStream(t.Context(), StreamContext{RequestID: "r1"}, StreamInput{Index: 0}).Err(); err != nil {
		t.Fatalf("非阻断超时不该报错: %v", err)
	}
	if s.Next() {
		t.Fatal("超时之后 runtime 该被判为不可用")
	}
	res := s.OnUsage(t.Context(), UsageContext{RequestID: "r1"})
	if res.Failed() || res.Patch != nil {
		t.Fatalf("超时之后的 hook 该直接跳过，实际 %+v", res)
	}
}

// ── onRoute ───────────────────────────────────────────────────────────

func TestOnRouteSelectsWithinCandidates(t *testing.T) {
	// 插件把「它看到的候选顺序」原样写进 reason，好让这一层能断言
	// 插件侧看到的到底是什么——候选集是内核构造的，从外面看不出来。
	s := sessionFor(t, minimalMeta,
		`onRoute(ctx){
			return { channelId: 42, reason: "" + ctx.candidates[0].channelId + "," +
			         ctx.candidates[1].channelId + "," + ctx.candidates[2].channelId };
		}`,
		sandbox.Host{}, sandbox.Limits{})

	cands := []RouteCandidate{{ChannelID: 7}, {ChannelID: 42}, {ChannelID: 3}}
	res := s.OnRoute(t.Context(), RouteContext{RequestID: "r1", Candidates: cands})
	if err := res.Err(); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Patch == nil || res.Patch.ChannelID != 42 {
		t.Fatalf("决策 = %+v", res.Patch)
	}
	// 插件看到的候选集按 channelId 排序，与内核的尝试顺序脱钩。
	// 于是插件写 candidates[0] 不会意外表达「内核评分最高的那个」。
	if res.Patch.Reason != "3,7,42" {
		t.Errorf("插件看到的候选集该按 channelId 排序（3,7,42），实际 %q", res.Patch.Reason)
	}
}

// 排序**只对插件生效**：调用方手上那份切片的顺序不变。
//
// 排的是副本。sort.Slice 排的是底层数组本身，直接排 rc.Candidates
// 会把内核拿到的候选顺序改掉，而路由层之后还要拿它重试、记账、
// 把「当时有哪些候选」记进快照。
func TestOnRouteLeavesCallerCandidatesUntouched(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRoute(){ return { channelId: 42 }; }`,
		sandbox.Host{}, sandbox.Limits{})

	cands := []RouteCandidate{{ChannelID: 7}, {ChannelID: 42}, {ChannelID: 3}}
	if err := s.OnRoute(t.Context(), RouteContext{RequestID: "r1", Candidates: cands}).Err(); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if cands[0].ChannelID != 7 || cands[1].ChannelID != 42 || cands[2].ChannelID != 3 {
		t.Errorf("调用方的候选顺序不该被插件层改动（期望 7,42,3），实际 %d,%d,%d",
			cands[0].ChannelID, cands[1].ChannelID, cands[2].ChannelID)
	}
}

// 选了候选集之外的渠道按插件失败处置，不给决策。
//
// 允许它选候选集之外的渠道等于让插件决定流量去哪，而候选集是配额、
// 健康度与商家配置共同算出来的。
func TestOnRouteRejectsChannelOutsideCandidates(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRoute(){ return { channelId: 999 }; }`,
		sandbox.Host{}, sandbox.Limits{})

	res := s.OnRoute(t.Context(), RouteContext{RequestID: "r1", Candidates: []RouteCandidate{{ChannelID: 7}}})
	if err := res.Err(); err != nil {
		t.Fatalf("非阻断插件失败不该让请求失败，实际 %v", err)
	}
	if res.Patch != nil {
		t.Fatalf("越界时不该给决策，实际 %+v", res.Patch)
	}
	fails := s.Failures()
	if len(fails) != 1 || !errors.Is(fails[0], ErrChannelOutsideCandidates) {
		t.Fatalf("越界该被记录成契约违规，实际 %v", fails)
	}
}

// 越界的阻断型插件让请求失败。
func TestOnRouteOutsideCandidatesIsBlockingWhenDeclared(t *testing.T) {
	s := sessionFor(t, `key:"p1", apiVersion:1, blocking:true,`,
		`onRoute(){ return { channelId: 999 }; }`, sandbox.Host{}, sandbox.Limits{})
	if err := s.OnRoute(t.Context(), RouteContext{RequestID: "r1", Candidates: []RouteCandidate{{ChannelID: 7}}}).Err(); err == nil {
		t.Fatal("阻断型插件越界时请求该失败")
	}
}

// 返回 channelId 0 归一成「没给决策」——JS 里 `{}` 解出来的就是 0，
// 而 0 不是一个合法渠道 ID。
func TestOnRouteZeroMeansNoDecision(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRoute(){ return {}; }`, sandbox.Host{}, sandbox.Limits{})
	res := s.OnRoute(t.Context(), RouteContext{RequestID: "r1", Candidates: []RouteCandidate{{ChannelID: 7}}})
	if res.Failed() || res.Patch != nil {
		t.Fatalf("res=%+v，期望「没有决策」", res)
	}
}

// ── onStream ──────────────────────────────────────────────────────────

func TestOnStreamDecision(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onStream(ctx){
			return { drop: ctx.index === 0, replace: ctx.index === 1 ? "改过" : undefined,
			         terminate: ctx.index === 2 };
		}`, sandbox.Host{}, sandbox.Limits{})

	for _, tc := range []struct {
		index     int
		drop      bool
		replace   string
		hasRepl   bool
		terminate bool
	}{
		{0, true, "", false, false},
		{1, false, "改过", true, false},
		{2, false, "", false, true},
	} {
		res := s.OnStream(t.Context(), StreamContext{RequestID: "r1"}, StreamInput{Index: tc.index})
		if err := res.Err(); err != nil {
			t.Fatalf("index %d 出错: %v", tc.index, err)
		}
		d := res.Patch
		if d == nil {
			t.Fatalf("index %d 该有决策", tc.index)
		}
		if d.Drop != tc.drop || d.Terminate != tc.terminate {
			t.Errorf("index %d: drop=%v terminate=%v，期望 %v/%v", tc.index, d.Drop, d.Terminate, tc.drop, tc.terminate)
		}
		if (d.Replace != nil) != tc.hasRepl {
			t.Errorf("index %d: replace=%s，期望有值=%v", tc.index, showPtr(d.Replace), tc.hasRepl)
		}
		if tc.hasRepl && *d.Replace != tc.replace {
			t.Errorf("index %d: replace=%q", tc.index, *d.Replace)
		}
	}
}

// **`replace: ""` 与「没有 replace」是两件事**：前者是「把这一块替换成空」。
// 用指针就是为了这个，零值 string 让两者无法区分。
func TestOnStreamEmptyReplaceIsNotAbsent(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onStream(){ return { replace: "" }; }`, sandbox.Host{}, sandbox.Limits{})
	res := s.OnStream(t.Context(), StreamContext{RequestID: "r1"}, StreamInput{})
	if err := res.Err(); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Patch == nil || res.Patch.Replace == nil || *res.Patch.Replace != "" {
		t.Fatalf("空字符串替换该被当成「有替换」，实际 %+v", res.Patch)
	}
}

// 累积正文一路传给插件：它是流式中断计费的依据。
func TestOnStreamSeesAccumulatedText(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onStream(ctx){ return { replace: ctx.accumulatedText + "!" }; }`,
		sandbox.Host{}, sandbox.Limits{})
	res := s.OnStream(t.Context(), StreamContext{RequestID: "r1"},
		StreamInput{Index: 3, AccumulatedText: "已经吐出来的", AccumulatedTokensApprox: 12})
	if err := res.Err(); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Patch == nil || res.Patch.Replace == nil || *res.Patch.Replace != "已经吐出来的!" {
		t.Fatalf("累积正文没有传到插件，实际 %+v", res.Patch)
	}
}

// ── onResponse ────────────────────────────────────────────────────────

func TestOnResponsePatch(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onResponse(ctx){ return { text: "改写过的正文", metadata: { source: "p1" } }; }`,
		sandbox.Host{}, sandbox.Limits{})
	res := s.OnResponse(t.Context(), ResponseContext{RequestID: "r1", Status: 200})
	if err := res.Err(); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Patch == nil || res.Patch.Text == nil || *res.Patch.Text != "改写过的正文" {
		t.Fatalf("patch = %+v", res.Patch)
	}
	if res.Patch.Metadata["source"] != "p1" {
		t.Errorf("metadata = %v", res.Patch.Metadata)
	}
}

// 空补丁归一成没有补丁。
func TestOnResponseEmptyPatchNormalized(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onResponse(){ return {}; }`, sandbox.Host{}, sandbox.Limits{})
	res := s.OnResponse(t.Context(), ResponseContext{RequestID: "r1"})
	if res.Failed() || res.Patch != nil {
		t.Fatalf("res=%+v", res)
	}
}

// ── onUsage ───────────────────────────────────────────────────────────

func TestOnUsageReportsCountsOnly(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onUsage(ctx){ return { promptTokens: 10, completionTokens: 20, units: { searches: 2 } }; }`,
		sandbox.Host{}, sandbox.Limits{})
	res := s.OnUsage(t.Context(), UsageContext{RequestID: "r1", Protocol: "openai"})
	if err := res.Err(); err != nil {
		t.Fatalf("出错: %v", err)
	}
	facts := res.Patch
	if facts == nil || *facts.PromptTokens != 10 || *facts.CompletionTokens != 20 {
		t.Fatalf("facts = %+v", facts)
	}
	// 没报的项保持 nil，而不是 0——见 MergeUsage。
	if facts.CacheReadTokens != nil {
		t.Errorf("没报的项该是 nil，实际 %v", *facts.CacheReadTokens)
	}
	if facts.Units["searches"] != 2 {
		t.Errorf("units = %v", facts.Units)
	}
}

// 空上报归一成没有上报：插件返回 `{}` 时不该覆盖内核的估算值。
func TestOnUsageEmptyIsNoReport(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onUsage(){ return {}; }`, sandbox.Host{}, sandbox.Limits{})
	res := s.OnUsage(t.Context(), UsageContext{RequestID: "r1"})
	if res.Failed() || res.Patch != nil {
		t.Fatalf("res=%+v，期望「没有上报」", res)
	}
}

// 只声明计费口径也算一次上报：它不改数量，但改金额。
//
// 被当成「没上报」丢掉时，插件把口径说对了也没有用——那批推理 token 照样
// 会被 completion 与 reasoning 各乘一次单价，而账单只是偏高，对账发现不了。
func TestOnUsageReasoningScopeAloneIsAReport(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onUsage(){ return { completionIncludesReasoning: true }; }`, sandbox.Host{}, sandbox.Limits{})
	res := s.OnUsage(t.Context(), UsageContext{RequestID: "r1"})
	if res.Failed() {
		t.Fatalf("不该失败: %v", res.Failure)
	}
	if res.Patch == nil || res.Patch.CompletionIncludesReasoning == nil || !*res.Patch.CompletionIncludesReasoning {
		t.Fatalf("只声明口径也是上报，实际 %+v", res.Patch)
	}
}

// 口径跟着数量一起合并：插件没声明时保留内核的，声明了就以插件为准。
func TestMergeUsageCarriesReasoningScope(t *testing.T) {
	// 插件只报数量时，内核那份口径不能丢——它是重建回协议层用量时的依据。
	core := UsageFacts{CompletionTokens: ptrInt64(100), CompletionIncludesReasoning: ptrBool(true)}
	if got := MergeUsage(core, &UsageFacts{CompletionTokens: ptrInt64(120)}); got.CompletionIncludesReasoning == nil || !*got.CompletionIncludesReasoning {
		t.Errorf("插件没声明口径，该保留内核的 true，实际 %s", showPtr(got.CompletionIncludesReasoning))
	}
	// 插件声明了就覆盖内核的。
	if got := MergeUsage(core, &UsageFacts{CompletionIncludesReasoning: ptrBool(false)}); got.CompletionIncludesReasoning == nil || *got.CompletionIncludesReasoning {
		t.Errorf("插件声明为 false，该覆盖内核的 true，实际 %s", showPtr(got.CompletionIncludesReasoning))
	}
	// 不改动入参。
	if core.CompletionIncludesReasoning == nil || !*core.CompletionIncludesReasoning {
		t.Error("入参被就地修改了")
	}
}

// **用量合并是「叠加」而不是「替换」**：没报的项保留内核的值。
//
// 替换会让一次漏报变成一次少收钱，而漏报很可能发生——
// 插件只在特定条件下才报某个字段。
func TestMergeUsageKeepsCoreForUnreportedFields(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	core := UsageFacts{
		PromptTokens:     n(100),
		CompletionTokens: n(200),
		CacheReadTokens:  n(30),
		Units:            map[string]int64{"searches": 1},
	}
	got := MergeUsage(core, &UsageFacts{CompletionTokens: n(250), Units: map[string]int64{"searches": 2}})

	if *got.PromptTokens != 100 {
		t.Errorf("插件没报 prompt，该保留内核的 100，实际 %d", *got.PromptTokens)
	}
	if *got.CompletionTokens != 250 {
		t.Errorf("插件报了 completion，该用 250，实际 %d", *got.CompletionTokens)
	}
	if *got.CacheReadTokens != 30 {
		t.Errorf("插件没报缓存读，该保留 30，实际 %d", *got.CacheReadTokens)
	}
	if got.Units["searches"] != 3 {
		t.Errorf("非 token 计量按相加，该是 3，实际 %d", got.Units["searches"])
	}
	// 不改动入参。
	if *core.CompletionTokens != 200 {
		t.Errorf("入参被就地修改了: %d", *core.CompletionTokens)
	}

	// nil 上报等于什么都没发生。
	if same := MergeUsage(core, nil); *same.CompletionTokens != 200 {
		t.Errorf("nil 上报该原样返回，实际 %d", *same.CompletionTokens)
	}
}

// ── 失败类型 ──────────────────────────────────────────────────────────

// 客户端断开与插件超时必须能分开：前者不是插件的错，不该告警也不该
// 让插件作者去改代码。
func TestClientCancellationIsDistinct(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRequest(){ while(true){} }`, sandbox.Host{}, sandbox.Limits{
		CallTimeout: 10 * time.Second, // 故意给长，让下面的 cancel 先到
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if err := s.OnRequest(ctx, RequestContext{RequestID: "r1"}).Err(); err != nil {
		t.Fatalf("客户端断开不该让请求失败: %v", err)
	}
	fails := s.Failures()
	if len(fails) != 1 {
		t.Fatalf("该记一条失败，实际 %v", fails)
	}
	if !errors.Is(fails[0], sandbox.ErrClientCancelled) {
		t.Fatalf("该认出是客户端断开，实际 kind=%v cause=%v", fails[0].Kind, fails[0].Cause)
	}
}

// 插件里死循环时，宿主不陪它等：请求侧不该被一个坏插件拖住。
func TestHangingPluginIsCutOff(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRequest(){ while(true){} }`, sandbox.Host{}, sandbox.Limits{
		CallTimeout: 40 * time.Millisecond,
	})
	start := time.Now()
	err := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"}).Err()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("非阻断超时不该让请求失败: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("死循环该被及时切断，实际耗时 %v", elapsed)
	}
	fails := s.Failures()
	if len(fails) != 1 || !errors.Is(fails[0], sandbox.ErrCallTimeout) {
		t.Fatalf("该记一条超时，实际 %v", fails)
	}
}

// 失败的错误信息里要能看出是哪个插件的哪个 hook——排查的第一个问题就是它。
func TestFailureNamesPluginAndHook(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onResponse(){ throw new Error("原因在此"); }`, sandbox.Host{}, sandbox.Limits{})
	if err := s.OnResponse(t.Context(), ResponseContext{RequestID: "r1"}).Err(); err != nil {
		t.Fatalf("非阻断失败不该报错: %v", err)
	}
	msg := s.Failures()[0].Error()
	for _, want := range []string{"p1", HookResponse, "原因在此"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 里该含 %q", msg, want)
		}
	}
}

// 没导出某个 hook 时直接跳过，不算失败。
func TestMissingHookIsNotFailure(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onUsage(){ return {}; }`, sandbox.Host{}, sandbox.Limits{})
	for _, call := range []func() error{
		func() error { return s.OnRequest(t.Context(), RequestContext{}).Err() },
		func() error { return s.OnRoute(t.Context(), RouteContext{}).Err() },
		func() error { return s.OnStream(t.Context(), StreamContext{}, StreamInput{}).Err() },
		func() error { return s.OnResponse(t.Context(), ResponseContext{}).Err() },
	} {
		if err := call(); err != nil {
			t.Errorf("没导出的 hook 该直接跳过，实际 %v", err)
		}
	}
	if len(s.Failures()) != 0 {
		t.Errorf("跳过不是失败，实际 %v", s.Failures())
	}
}
