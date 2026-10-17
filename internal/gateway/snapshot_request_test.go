package gateway

import (
	"context"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// 这个文件测的是**请求快照采的是哪一份请求**。
//
// # 为什么这件事需要一个判据
//
// 快照回答的是「当时发出去的是什么」，而发出去的那份与调用方发来的那份**不是
// 同一份**：上游名按选中的渠道翻过，别名上配的参数覆盖也应用上去了（见
// internal/route 的 upstreamRequest）。采错那一份不会有任何报错，只会让事后
// 照着快照复现的人看到一组与实物不同的参数——而在覆盖这类「配了就是要生效」
// 的功能上，这种偏差正好把人引向「覆盖是不是没生效」。
//
// 判据落在**交给脱敏器的那个对象**上（fakeRedactor 记的指针），不是「内容相等」：
// 复制一份再算一遍覆盖也会让内容相等，那正是要挡的实现。
//
// # 采集点为什么因此落在选路之后
//
// 请求快照原先采在调上游**之前**（gateway_test.go 与 stream_test.go 各有一条
// 用例数着「调上游时的脱敏次数」钉住这个次序），那时采的就是调用方发来的那份，
// 所以采得早与采得对是同一件事。覆盖与上游名都只有选路之后才知道，两者不再
// 是同一件事，那两条次序用例因此撤掉，判据由本文件接着钉：记下来的仍然必须是
// 发出去的那份实物——成功、失败、换渠道之后的最后一次尝试、一份都没发出去，
// 以及流式那条路，逐条都有。
//
// 次序本身不再是要求，也不需要是：快照的落库本来就在请求结束之后才写完
// （见 saveSnapshotAsync 用不带取消的 context）。

// overridingAliases 是「别名按同名回落，但配了参数覆盖」的部署：同一个对象既是
// route.ModelAliases（选路要上游名）又是 AliasOverrideSource（预留上界要覆盖），
// 真实组装里 internal/catalog 的 Catalog 就是这样同时接上两条线的。
//
// 与 overrides_test.go 的 fixedOverrides 分开：那个只接覆盖来源，钉的是上界那
// 一路；这里的判据是「覆盖真的走到了发出去的那份请求上」，只接上界那一路的话，
// 发出去的请求里根本没有覆盖，用例会红在一个与判据无关的地方。
type overridingAliases struct {
	byChannel map[int64]map[string]any
}

func (o overridingAliases) Alias(_ context.Context, channelID int64, canonical string) (string, map[string]any, error) {
	return canonical, o.byChannel[channelID], nil
}

func (o overridingAliases) OverridesFor(_ context.Context, _ string, channelIDs []int64) (map[int64]map[string]any, error) {
	out := map[int64]map[string]any{}
	for _, id := range channelIDs {
		if v, ok := o.byChannel[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// withAliasOverrides 在组装夹具时把这一个对象接到两条线上。
//
// 必须走选项：网关与它里面的路由器都在组装那一刻把接口值抄进自己的字段，之后
// 再改夹具上的字段对它们没有任何影响（见 overrides_test.go 的 withOverrides）。
func withAliasOverrides(byChannel map[int64]map[string]any) func(*harness) {
	src := overridingAliases{byChannel: byChannel}
	return func(h *harness) {
		h.aliases = src
		h.aliasOverrides = src
	}
}

// 配了覆盖的别名上，快照采的是**发出去的那一份**。
func TestRequestSnapshotUsesTheRequestThatWentOut(t *testing.T) {
	h := newHarness(t, withAliasOverrides(map[int64]map[string]any{
		1: {"temperature": 0.2, "max_tokens": 2048},
	}))

	// 调用方没给 temperature，给的是 64——所以「采哪一份」在快照内容上分得开。
	caller := upstreamRequest()
	caller.MaxTokens = 64
	req := request()
	req.MaxTokens = 64

	if _, err := h.gw.Chat(context.Background(), req, caller, Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	if len(h.redactor.requests) != 1 {
		t.Fatalf("交给脱敏器的请求 = %d 份，期望 1 份（请求方向一次）", len(h.redactor.requests))
	}
	got := h.redactor.requests[0]
	if len(h.executor.sentRefs) != 1 {
		t.Fatalf("执行器被调用 %d 次，期望 1 次", len(h.executor.sentRefs))
	}
	if got != h.executor.sentRefs[0] {
		t.Error("快照采的不是执行器收到的那一份请求")
	}
	if got == caller {
		t.Error("快照采的是调用方发来的那一份——它没有别名上的覆盖")
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("快照里那份请求的 temperature = %v，期望覆盖之后的 0.2", got.Temperature)
	}
	if got.MaxTokens != 2048 {
		t.Errorf("快照里那份请求的 max_tokens = %d，期望覆盖之后的 2048（调用方给的是 64）", got.MaxTokens)
	}
}

// 上游那次调用**失败**时同样采发出去的那一份。
//
// 失败路径正是快照最该有用的时候，而「失败的那次也发出去过」这条口径在三处
// 是同一个（渠道、上游名、这份请求，见 route.Result 的注释）。采调用方那份的
// 话，一次配错覆盖导致的失败在快照里看不出覆盖。
func TestRequestSnapshotOnFailureUsesTheAttemptThatWasSent(t *testing.T) {
	h := newHarness(t, withAliasOverrides(map[int64]map[string]any{
		1: {"max_tokens": 2048},
	}))
	// 不可重试的失败：换渠道也是同一个错，只会有一次尝试。
	h.executor.err = &channel.ErrUpstream{StatusCode: 400, GotResponse: true}

	caller := upstreamRequest()
	caller.MaxTokens = 64
	req := request()
	req.MaxTokens = 64

	if _, err := h.gw.Chat(context.Background(), req, caller, Hooks{}); err == nil {
		t.Fatal("上游失败时 Chat 应报错")
	}

	if len(h.redactor.requests) != 1 {
		t.Fatalf("交给脱敏器的请求 = %d 份，期望 1 份", len(h.redactor.requests))
	}
	got := h.redactor.requests[0]
	if got == caller {
		t.Fatal("失败时采的是调用方发来的那一份——那一次调用真的发出去过")
	}
	if got.MaxTokens != 2048 {
		t.Errorf("快照里那份请求的 max_tokens = %d，期望覆盖之后的 2048", got.MaxTokens)
	}
}

// 换过渠道时采的是**最后一次尝试**发出去的那一份，而且只留一条。
//
// 「最后一次尝试」这条口径在 route.Result 上写死了：一次请求可能打过好几个渠道，
// 快照只有一行，得说清它是哪一次。按渠道各配一份覆盖之后，这件事在快照内容上
// 直接看得出来——采错一次就是一组别的渠道的参数。
//
// 候选之间的先后由选路层决定（那条用例在 internal/route），这里不去钉顺序：
// 问执行器最后打的是哪个渠道，再按那个渠道的覆盖核对快照内容。
func TestRequestSnapshotOnFailoverUsesTheLastAttempt(t *testing.T) {
	byChannel := map[int64]map[string]any{
		1: {"max_tokens": 1024},
		2: {"max_tokens": 4096},
	}
	h := newHarness(t, withAliasOverrides(byChannel), func(h *harness) {
		h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}
	})
	// 渠道 1 可重试地失败：请求会落到另一个候选上。
	h.executor.failFor = map[int64]error{1: &channel.ErrUpstream{StatusCode: 500, Retryable: true}}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("换渠道之后应当成功: %v", err)
	}

	if len(h.executor.calls) != 2 {
		t.Fatalf("执行器被调用 %d 次，期望 2 次（换渠道一次）", len(h.executor.calls))
	}
	if len(h.redactor.requests) != 1 {
		t.Fatalf("交给脱敏器的请求 = %d 份，期望 1 份（一行快照，不是一次尝试一行）",
			len(h.redactor.requests))
	}

	got := h.redactor.requests[0]
	if got != h.executor.sentRefs[len(h.executor.sentRefs)-1] {
		t.Error("快照采的不是最后一次尝试发出去的那一份")
	}
	last := h.executor.calls[len(h.executor.calls)-1]
	want, ok := byChannel[last]["max_tokens"].(int)
	if !ok {
		t.Fatalf("夹具里渠道 %d 的覆盖值不是 int: %v", last, byChannel[last]["max_tokens"])
	}
	if got.MaxTokens != want {
		t.Errorf("快照里那份请求的 max_tokens = %d，期望最后那次尝试（渠道 %d）覆盖之后的 %d",
			got.MaxTokens, last, want)
	}
}

// 流式那条路同样采发出去的那一份。
//
// 与非流式分开写而不是共用一个判据：两条路的收尾差得很远（流式的收尾在后台的
// pumpStream 里），采集点也各写各的。
func TestStreamRequestSnapshotUsesTheRequestThatWentOut(t *testing.T) {
	h := newHarness(t, withAliasOverrides(map[int64]map[string]any{
		1: {"temperature": 0.2, "max_tokens": 2048},
	}))
	exec := useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {{Kind: "done"}},
	}})

	caller := upstreamRequest()
	caller.MaxTokens = 64
	req := request()
	req.MaxTokens = 64

	res, err := h.gw.ChatStream(context.Background(), req, caller, Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	if len(h.redactor.requests) != 1 {
		t.Fatalf("交给脱敏器的请求 = %d 份，期望 1 份（请求方向一次）", len(h.redactor.requests))
	}
	got := h.redactor.requests[0]
	if len(exec.sentRefs) != 1 {
		t.Fatalf("执行器被调用 %d 次，期望 1 次", len(exec.sentRefs))
	}
	if got != exec.sentRefs[0] {
		t.Error("快照采的不是执行器收到的那一份请求")
	}
	if got == caller {
		t.Error("快照采的是调用方发来的那一份——它没有别名上的覆盖")
	}
	if got.Temperature == nil || *got.Temperature != 0.2 {
		t.Errorf("快照里那份请求的 temperature = %v，期望覆盖之后的 0.2", got.Temperature)
	}
	if got.MaxTokens != 2048 {
		t.Errorf("快照里那份请求的 max_tokens = %d，期望覆盖之后的 2048（调用方给的是 64）", got.MaxTokens)
	}
}

// 一份都没发出去时退回**调用方发来的那一份**。
//
// 那种情况下没有「发出去的那份」，而记录调用方那份仍然有用：它就是要发的那份，
// 而这一类失败（候选全被筛掉、别名翻不出来）恰恰最需要复现。判据是调用方拿到的
// 事实——route.Result.OutboundRequest 为 nil（见那里的注释）。
func TestRequestSnapshotFallsBackToTheCallerRequest(t *testing.T) {
	h := newHarness(t)
	// 候选只支持另一个模型名，于是选路把它整个筛掉：一个上游调用都不会发生。
	h.candidates.cands = []route.Candidate{candidate(1, "another-model")}

	caller := upstreamRequest()
	caller.MaxTokens = 64

	if _, err := h.gw.Chat(context.Background(), request(), caller, Hooks{}); err == nil {
		t.Fatal("没有可用候选时 Chat 应报错")
	}
	if len(h.executor.calls) != 0 {
		t.Fatalf("一个上游调用都不该发生，实际调用了 %v", h.executor.calls)
	}

	if len(h.redactor.requests) != 1 {
		t.Fatalf("交给脱敏器的请求 = %d 份，期望 1 份（这次请求仍然要留一条）",
			len(h.redactor.requests))
	}
	if got := h.redactor.requests[0]; got != caller {
		t.Errorf("一份都没发出去时该采调用方发来的那份，实际 %+v", got)
	}
}
