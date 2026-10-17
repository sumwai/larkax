package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/tokens"
)

// 这个文件钉住 onStream / onUsage 两个挂载点的**效果**。
//
// 与 plugin_test.go 同一个理由：效果只有接上真的 gateway 才看得到。换成假
// Chatter 就只测得到「入口把闭包交给了谁」——那测的是调用发生，不是结果。
// 这两条 hook 各自的接线有两段（gateway 的分片循环 → 入口的闭包 → 沙箱），
// 断在任何一段上，调用方看到的都是一条**正常的流**与一份**正常的账单**。

// streamBody1000 与 streamBody 只差 max_tokens：onUsage 上报的用量要过
// 「客户端给的 max_tokens」那道输出上界（见 plugin.UsageBound），上界比
// 上报值小的时候一次合法上报也会被拒——那会把用例的判据变成上界而不是接线。
const streamBody1000 = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":1000}`

// ── onStream ──────────────────────────────────────────────────────────

// 逐块决策要在**调用方收到的流**上看得出来。
//
// 断言落在 SSE 正文上，不落在 gateway 的内部状态：这一条要证明的是
// 「插件说了算」，而只有正文能证明它——决策被算出来却没落到发出去的那一块上
// （例如在渲染之后才处置），内部状态一切正常，调用方看到的仍是原文。
func TestOnStreamDecisionsReachTheCaller(t *testing.T) {
	const hooks = `onStream(ctx) {
			if (ctx.chunk.kind !== "delta") return;
			if (ctx.chunk.text === "删掉") return { drop: true };
			return { replace: "改过:" + ctx.chunk.text };
		}`
	h := newPluginHarness(t, pluginRegistry(t, "", hooks))
	h.executor.stream = []channel.StreamEvent{
		{Kind: "delta", Text: "删掉"},
		{Kind: "delta", Text: "留着"},
		{Kind: "done"},
	}

	rec := post(h.handler, PathChatCompletions, streamBody1000)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "删掉") {
		t.Errorf("被 drop 的那一块不该出现在调用方收到的流里: %s", body)
	}
	if !strings.Contains(body, "改过:留着") {
		t.Errorf("替换后的正文没出现在流里（replace 没落到发出去的那一块上）: %s", body)
	}
}

// Terminate 走的是与「上游结束 / 客户端中途断开」**同一条**收尾路径：
// 流当场截断，而用量照样结算，冻住的钱也照样解冻。
//
// # 这一条钉的是哪个坑
//
// 把 Terminate 实现成分片循环里的一个 `break`，它就绕过了结算那一整段：
// 调用方收到半截流、账上一分钱没有、冻住的钱永远冻着——而这次请求是 200，
// 日志里也没有任何错误。所以断言必须同时落在**流被截断**与**钱已经了结**
// 两件事上，只看其中一件都分不出「接错了」与「接对了」。
//
// 「了结」那一半由两个数一起说：结算落库一次、释放零次。少了后者，
// 「结算了但钱还冻着」这种一半的收尾就漏过去了。
func TestOnStreamTerminateTruncatesButStillBills(t *testing.T) {
	// 第二块上终止：第一块已经发出去了，所以这是一次「发到一半」的截断，
	// 而不是「一个字都没发」。
	const hooks = `onStream(ctx) { if (ctx.index === 1) return { terminate: true }; }`
	h := newPluginHarness(t, pluginRegistry(t, "", hooks))
	h.executor.stream = []channel.StreamEvent{
		{Kind: "delta", Text: "第一块"},
		{Kind: "delta", Text: "第二块"},
		{Kind: "done"},
	}

	rec := post(h.handler, PathChatCompletions, streamBody1000)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "第一块") {
		t.Errorf("终止之前的那一块该已经发出去了: %s", body)
	}
	if strings.Contains(body, "第二块") {
		t.Errorf("终止之后的那一块不该发出去: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("截断的流不该有结束标记（那会让客户端以为回答完整）: %s", body)
	}

	if len(h.settlements.saved) != 1 {
		t.Fatalf("结算落库 %d 次，期望 1 次（终止走的是与上游结束同一条路，要结算）",
			len(h.settlements.saved))
	}
	if h.settlements.saved[0].ClientCanceled {
		t.Errorf("这次不是客户端断开，ClientCanceled 不该为真——两者在对账上是两回事")
	}
	// 释放次数为 0：钱是被**结算那一步**解冻的，不该再释放一次（那是对同一笔
	// 预留的第二次解冻）。这一条同时是上面那个坑的另一半——终止被写成循环里
	// 一个 break 时，收尾会落到「没结算过」的分支上：那里既走不到结算落库，
	// 又必然释放一次，于是这两条断言一起红。
	if len(h.reservations.released) != 0 {
		t.Errorf("预留释放 %d 次，期望 0 次（结算已经解冻了这笔钱）",
			len(h.reservations.released))
	}
}

// ── onUsage ───────────────────────────────────────────────────────────

// 流式那条路上，插件上报的用量要**一路走到结算**。
//
// 对照组是必须的：没有插件时结算用的是上游在流里带的实测用量（10/5），
// 所以「结算里是 250」只可能来自插件。少了对照组，一个把用量写死成 250 的
// 接线错误也会绿。
func TestOnUsageStreamReportIsWhatGetsBilled(t *testing.T) {
	// 流里带实测用量，于是内核那份不是估的——这样才分得清
	// 「插件上报覆盖了内核」与「内核本来就在估」。
	const measured = `onRequest(){}, onRoute(){}`
	stream := []channel.StreamEvent{
		{Kind: "delta", Text: "一"},
		{Kind: "usage", Usage: &channel.Usage{PromptTokens: 10, CompletionTokens: 5}},
		{Kind: "done"},
	}

	t.Run("没有插件：按上游报的实测用量结算", func(t *testing.T) {
		h := newPluginHarness(t, nil)
		h.executor.stream = stream

		rec := post(h.handler, PathChatCompletions, streamBody1000)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		if len(h.settlements.saved) != 1 {
			t.Fatalf("结算落库 %d 次，期望 1 次", len(h.settlements.saved))
		}
		if got := h.settlements.saved[0].Usage.CompletionTokens; got != 5 {
			t.Errorf("输出 token = %d，期望 5（上游在流里报的实测值）", got)
		}
	})

	t.Run("插件上报：结算用的是上报值", func(t *testing.T) {
		h := newPluginHarness(t, pluginRegistry(t, "",
			measured+`, onUsage(){ return { completionTokens: 250 }; }`))
		h.executor.stream = stream

		rec := post(h.handler, PathChatCompletions, streamBody1000)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		if len(h.settlements.saved) != 1 {
			t.Fatalf("结算落库 %d 次，期望 1 次", len(h.settlements.saved))
		}
		charge := h.settlements.saved[0]
		if got := charge.Usage.CompletionTokens; got != 250 {
			t.Errorf("输出 token = %d，期望 250（onUsage 的上报要走到结算）", got)
		}
		// 没报的项保留内核的值：合并是叠加而不是替换，一次漏报不该变成少收钱。
		if got := charge.Usage.PromptTokens; got != 10 {
			t.Errorf("输入 token = %d，期望 10（插件没报这一项，该保留内核的值）", got)
		}
		// 上报不改**来源**：用量是上游实测的，这一点不因插件参与而变。
		if charge.UsageSource != tokens.SourceMeasured {
			t.Errorf("用量来源 = %v，期望 %v（插件上报不该把它变成估算）",
				charge.UsageSource, tokens.SourceMeasured)
		}
	})
}

// UsageBound.MaxOutputTokens 必须是**客户端原始的** max_tokens，即 onRequest
// 补丁**之前**的那个值。
//
// # 为什么这一条最要紧
//
// 上界与上报值由同一方控制时，两者一起放大就等于没有上界（见 UsageBound）。
// 补丁之后的值是**插件自己挑的**，把它当上界等于把这道检查交回插件手里：
// 插件把 max_tokens 调到 10、再照着自己的口径报一个数，报多少这道检查都点头。
//
// 这个坑的形状很特别——**两个方向都不会报错**。传补丁前的值，一次越界上报
// 被拒、账单退回内核的值；传补丁后的值，一次合法上报被拒、账单同样退回内核
// 的值。两次请求都是 200，日志里都只有一条「上报被拒」，差别只在**哪一次**
// 被拒上。所以判据必须用字面量把「哪一份上报被接受了」钉住，不能只断言
// 「出过一条拒绝」。
func TestUsageBoundIsTheClientsOriginalMaxTokens(t *testing.T) {
	// 客户端给 1000，插件把它调到 10——于是两个候选上界之间差了 100 倍，
	// 而上报值 100 正好落在中间：按客户端那个值判是界内，按补丁后的值判是越界。
	const body = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`
	const patch = `onRequest(){ return { body: { max_tokens: 10 } }; }, `

	t.Run("落在两个上界之间：按客户端的值判，接受", func(t *testing.T) {
		h := newPluginHarness(t, pluginRegistry(t, "",
			patch+`onUsage(){ return { completionTokens: 100 }; }`))

		rec := post(h.handler, PathChatCompletions, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		if got := h.executor.reqs[0].MaxTokens; got != 10 {
			t.Fatalf("上游收到的 max_tokens = %d，期望 10（补丁要先生效，否则这条用例测的不是上界）", got)
		}
		if len(h.settlements.saved) != 1 {
			t.Fatalf("结算落库 %d 次，期望 1 次", len(h.settlements.saved))
		}
		// 100 而不是内核的 5：只有把补丁**之前**的 1000 当上界，这份上报才进得来。
		// 传补丁后的 10 时这里是 5——用例会红，而请求仍然是 200。
		if got := h.settlements.saved[0].Usage.CompletionTokens; got != 100 {
			t.Errorf("输出 token = %d，期望 100（上界该是客户端原始的 max_tokens=1000，"+
				"不是插件补丁之后的 10；是 5 就说明上界被交回插件手里了）", got)
		}
	})

	t.Run("超过客户端的值：拒绝，退回内核的用量", func(t *testing.T) {
		h := newPluginHarness(t, pluginRegistry(t, "",
			patch+`onUsage(){ return { completionTokens: 2000 }; }`))

		rec := post(h.handler, PathChatCompletions, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("非阻断型插件的越界上报不该让请求失败，状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		if got := h.settlements.saved[0].Usage.CompletionTokens; got != 5 {
			t.Errorf("输出 token = %d，期望 5（越界的上报整份丢掉，按内核的值计费）", got)
		}
	})
}

// ── 接线本身 ──────────────────────────────────────────────────────────

// 两个入口都要把**五个** hook 全部交出去，一个都不能漏。
//
// 这条看着像废话，但它钉的是一件真实发生过的事：onStream 与 onUsage 是
// 后接上的两个挂载点，而它们的接线点在两个不同的入口函数里（流式与非流式）。
// 只接一个入口时，另一个入口上插件**静默不生效**——请求 200、响应正确、
// 账单也对，只是插件没跑。
//
// 与效果无关的那一半同样要钉：插件系统没接上时（Plugins 为零值），
// 交出去的必须是**零值 Hooks**。那不是「碰巧没有插件」，而是「插件关掉之后
// 行为与插件出现之前完全一样」这条承诺的落点——一个空壳闭包会让插件关掉
// 时仍然多走一遍沙箱。
// 五个 hook 在两个入口上都要接上。
//
// 分两半查，因为 onRequest 走的不是同一条路：它要在**预留之前**生效，所以
// 由入口自己调（见 plugin_test.go 的 TestOnRequestShrinksReservation），
// 不经过 gateway.Hooks。所以下面这张清单是四个字段，而 onRequest 由
// 「上游收到的 max_tokens 被改过」来证明——那是它唯一的效果出口。
func TestEveryHookIsWiredOnBothEntryPoints(t *testing.T) {
	// onRequest 把 max_tokens 从 16 调到 7，于是「它跑过」有一个能断言的
	// 落点：交出去的上游请求。
	const hooks = `onRequest(){ return { body: { max_tokens: 7 } }; },
		onRoute(){}, onStream(){}, onResponse(){}, onUsage(){}`

	// want 为 false 时查的是另一半：插件系统没接上时交出去的必须是**零值**。
	// 那不是「碰巧没有插件」，而是「插件关掉之后行为与插件出现之前完全一样」
	// 这条承诺的落点——一个空壳闭包会让插件关掉时照样进沙箱。
	all := func(t *testing.T, where string, h gateway.Hooks, want bool) {
		t.Helper()
		for _, item := range []struct {
			name string
			set  bool
		}{
			{"OnRoute", h.OnRoute != nil},
			{"OnStream", h.OnStream != nil},
			{"OnResponse", h.OnResponse != nil},
			{"OnUsage", h.OnUsage != nil},
		} {
			switch {
			case want && !item.set:
				t.Errorf("%s 上 %s 没接上", where, item.name)
			case !want && item.set:
				t.Errorf("%s 上 %s 不该接上", where, item.name)
			}
		}
	}
	patched := func(t *testing.T, where string, up *channel.Request) {
		t.Helper()
		if up == nil {
			t.Fatalf("%s 没把上游请求交出去", where)
		}
		if got := up.MaxTokens; got != 7 {
			t.Errorf("%s 上游收到的 max_tokens = %d，期望 7（onRequest 在这条路上没跑）", where, got)
		}
	}

	t.Run("非流式入口", func(t *testing.T) {
		h, _, chatter := newTestHandler()
		h.Plugins = pluginRegistry(t, "", hooks)

		if rec := post(h, PathChatCompletions, validBody); rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		all(t, "非流式入口", chatter.hooks, true)
		patched(t, "非流式入口", chatter.upstream)
	})

	t.Run("流式入口", func(t *testing.T) {
		h, _, chatter := newTestHandler()
		h.Plugins = pluginRegistry(t, "", hooks)
		chatter.streamResult = &gateway.Result{Events: streamOf(channel.StreamEvent{Kind: "done"})}

		if rec := post(h, PathChatCompletions, streamBody); rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		all(t, "流式入口", chatter.streamHooks, true)
		patched(t, "流式入口", chatter.upstream)
	})

	t.Run("插件系统没接上：交出去的是零值", func(t *testing.T) {
		h, _, chatter := newTestHandler()

		if rec := post(h, PathChatCompletions, validBody); rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		all(t, "没有插件时", chatter.hooks, false)
	})
}
