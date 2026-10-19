package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/route"
)

// 这个文件钉住 onRoute 这条接线**端到端**的效果：一个真的 JS 插件在一个
// 真的沙箱里选了一个渠道，请求就真的发去了那个渠道。
//
// # 为什么必须走完整条链
//
// 拆开测的话，「入口把决策交给了网关」与「网关按决策选路」各自都能过，
// 而两者之间的那一处（入口交出去的是不是网关真正读的那个字段）没有任何
// 东西看着。那正是这个项目反复撞到的那类缝：两处各自有测试，缺陷长在
// 两处之间。
//
// # 为什么断言落在执行器收到的渠道上
//
// 「插件被调过」太弱：插件跑过、请求 200、账单也对，三条同时成立而决策
// 被丢掉——现场是「插件选了一个渠道，请求发去了另一个」，从响应、日志和
// 账上都看不出来。所以下面断言的是**执行器实际收到哪个渠道**，而插件给的
// 那个恰恰是内核按优先级不会选的那一个。
//
// 夹具（pluginHarness 等）与 onRequest / onResponse 那两条共用一份，
// 见 plugin_test.go。

// pinnedCandidates 是「内核先选 1 号」的两个候选。
//
// priority 差着量级，于是「不装插件时先走 1 号」这件事在夹具里是确定的，
// 而它正是钉生效与否的对照。
func pinnedCandidates() []route.Candidate {
	c1 := pluginCandidate(1, "m-1")
	c1.Priority = 10
	c2 := pluginCandidate(2, "m-1")
	c2.Priority = 1
	return []route.Candidate{c1, c2}
}

const pinOnRoute = `onRoute(ctx) { return { channelId: 2, reason: "钉住 2 号" }; }`

// 插件钉住的渠道**就是**这次请求走的渠道，即使内核按优先级不会先选它。
func TestOnRoutePluginPinsChannelEndToEnd(t *testing.T) {
	// 对照组：不装插件时先走 1 号。没有它，下面那条在「实现根本没用
	// 决策」时也可能成立（例如两个候选的顺序恰好一样）。
	plain := newPluginHarness(t, nil, pinnedCandidates()...)
	if rec := post(plain.handler, PathChatCompletions, validBody); rec.Code != http.StatusOK {
		t.Fatalf("对照组状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if got := plain.executor.channels; len(got) != 1 || got[0] != 1 {
		t.Fatalf("不装插件时该先走 1 号，实际 %v", got)
	}

	h := newPluginHarness(t, pluginRegistry(t, "", pinOnRoute), pinnedCandidates()...)

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if got := h.executor.channels; len(got) != 1 || got[0] != 2 {
		t.Errorf("插件钉了 2 号，执行器却按 %v 调用", got)
	}
	if len(h.settlements.saved) != 1 {
		t.Errorf("账单 %d 条，期望 1 条：钉住渠道不影响计费", len(h.settlements.saved))
	}
}

// 流式那条路上同样生效——同一个插件，同样的钉。
//
// 与非流式分开断言是有必要的：入口那两个方法各有一条路，只测一边时另一边
// 「收下了挂载点却没用」不会有任何东西报出来，而现场是「插件对流式请求
// 静默不生效」。
func TestOnRoutePluginPinsChannelOnStream(t *testing.T) {
	h := newPluginHarness(t, pluginRegistry(t, "", pinOnRoute), pinnedCandidates()...)

	body := strings.Replace(validBody, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1)
	rec := post(h.handler, PathChatCompletions, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if got := h.executor.channels; len(got) != 1 || got[0] != 2 {
		t.Errorf("流式请求里插件钉了 2 号，执行器却按 %v 调用", got)
	}
}

// 插件选了候选集之外的渠道时，**非阻断**的失败按「没给决策」处置：
// 请求照常成功，走的是内核排序。
//
// 这一条与 session_test 里那条（越界被记成一次失败）是两个落点：那条钉的
// 是「记下来了」，这条钉的是「记下来之后请求怎么办」——把它升级成请求失败
// 是另一种做法，而契约选的是不阻断。
func TestOnRouteOutsideCandidatesFallsBackToKernelOrder(t *testing.T) {
	h := newPluginHarness(t,
		pluginRegistry(t, "", `onRoute(ctx) { return { channelId: 99 }; }`),
		pinnedCandidates()...)

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("非阻断的越界决策不该让请求失败，状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if got := h.executor.channels; len(got) != 1 || got[0] != 1 {
		t.Errorf("越界决策该被丢掉、沿用内核排序（1 号），实际 %v", got)
	}
}

// 声明了 blocking 的插件越界时，**这次请求失败**，而且一次上游都没调。
//
// 「一次上游都没调」与「不计费」要一起断言：只断言金额的话，一个「先调了
// 上游再按失败结算」的实现也能过——而那是白花的钱。
func TestOnRouteBlockingOutsideCandidatesFailsRequest(t *testing.T) {
	h := newPluginHarness(t,
		pluginRegistry(t, `, blocking: true`, `onRoute(ctx) { return { channelId: 99 }; }`),
		pinnedCandidates()...)

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code == http.StatusOK {
		t.Fatalf("阻断型插件的越界决策该让请求失败，实际 200：%s", rec.Body)
	}
	if len(h.executor.channels) != 0 {
		t.Errorf("阻断时不该调上游，实际调了 %v", h.executor.channels)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("阻断时不该有账单，实际 %d 条", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("预留释放 %d 次，期望 1 次", len(h.reservations.released))
	}
}

// ── 挂载点本身 ────────────────────────────────────────────────────────

// 两个入口都拿到了挂载点；插件系统没接上时两个都是零值。
//
// 这一条钉的是**接线**而不是效果（效果在上面那几条里）：它分辨的是
// 「流式那条路忘了传」——那种漏法在上面的端到端用例里会表现成
// 「流式请求没被钉」，而这里能直接指出来是哪一个入口漏了。
//
// 「插件系统没接上时是零值」这一半同样要紧：关掉插件的行为不变必须是
// **结构性**的（零值 Hooks 两个字段都是 nil，网关各自走不碰插件的那条
// 分支），而不是靠某个开关在运行时判断。
func TestOnRouteHookReachesBothEntryPoints(t *testing.T) {
	reg := pluginRegistry(t, "", `onRoute(ctx) { return undefined; }`)

	withPlugin, _, chatter := newTestHandler()
	withPlugin.Plugins = plugin.NewHolder(reg)
	if rec := post(withPlugin, PathChatCompletions, validBody); rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if chatter.hooks.OnRoute == nil {
		t.Error("非流式入口没有把 onRoute 挂载点传给网关")
	}

	withPlugin, _, chatter = newTestHandler()
	withPlugin.Plugins = plugin.NewHolder(reg)
	chatter.streamResult = &gateway.Result{Events: streamOf(channel.StreamEvent{Kind: "done"})}
	if rec := post(withPlugin, PathChatCompletions, streamBody); rec.Code != http.StatusOK {
		t.Fatalf("流式状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if chatter.streamHooks.OnRoute == nil {
		t.Error("流式入口没有把 onRoute 挂载点传给网关")
	}

	noPlugin, _, chatter := newTestHandler()
	if rec := post(noPlugin, PathChatCompletions, validBody); rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if chatter.hooks.OnRoute != nil {
		t.Error("插件系统没接上时不该有 onRoute 挂载点")
	}
}

// onRoute 与别的 hook 出自**同一个会话**。
//
// 分两次建会话是这条接线上最容易走上的岔路：两个挂载点各自都对、各自都有
// 测试，而插件作者眼里的一次请求在沙箱里变成了两次互不相干的状态。
//
// # 判据为什么是模块级的 JS 变量
//
// 会话与运行时是一对一的（见 plugin.Registry.NewSession 与
// sandbox.Module.NewRuntime：每个运行时都会把模块顶层重新求值一遍），所以
// **模块级的变量就是「同一个会话」的探针**。两个会话时 onRoute 那个运行时
// 里 picked 是初始值 0，而 channelId 为 0 等于「不决策」，请求会走内核顺序
// 的 1 号——于是这条用例失败。
//
// 断言落在执行器收到的渠道上，与上面几条同一个理由：从响应、日志和账上
// 都看不出决策被丢掉。
func TestOnRouteSharesSessionWithOtherHooks(t *testing.T) {
	// picked 只在 onRequest 里被赋值，onRoute 只读它、不写任何字面量渠道号。
	const hooks = `
		onRequest(ctx) { picked = 2; },
		onRoute(ctx) { return { channelId: picked }; }`

	reg := pluginRegistry(t, "", hooks, "let picked = 0;\n")
	h := newPluginHarness(t, reg, pinnedCandidates()...)

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if got := h.executor.channels; len(got) != 1 || got[0] != 2 {
		t.Errorf("onRoute 没读到 onRequest 写下的模块级变量，两个 hook 背后不是同一个会话：执行器按 %v 调用", got)
	}
}
