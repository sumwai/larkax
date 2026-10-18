package gateway

// 本文件测**流式递进冻结**（#498）：流式那条路不再按调用方声明的 max_tokens
// 一次冻满，而是先冻第一段，之后每产出一个阈值段就补冻一次；补冻够不着余额时
// 把这条流切断。
//
// 五个结局各有一条用例，外加两条「配置不对时不静默」的：
//
//	一条长输出跑完        / 余额耗尽被切断 / 切断不算渠道的失败
//	补冻写库失败放行      / 阈值段缺失时拒绝受理
//
// 断言落在钱上（冻了多少、结算多少、有没有撞上界），因为这条路上最容易错的
// 恰好是钱：冻少了结算撞 Capped（差额由平台吃掉，账目仍然是平的），冻多了把
// 用户的余额白白占住。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/tokens"
)

// 本文件用的几个常量，与夹具里的规则对得上。
//
// simpleRule 是 {prompt: 1e-6, completion: 2e-6}，而递进冻结按
// max(completion, reasoning) 取输出侧的价（见 billing.OutputPrice）——这条规则
// 里没有 reasoning，所以输出侧的价就是 2e-6。
const (
	freezeStep      = 1000         // 阈值段（token）
	freezeInputCost = "0.00100000" // 夹具的输入侧：1000 token × 1e-6
	freezeSegment   = "0.00200000" // 一个阈值段的输出侧：1000 token × 2e-6
	freezeFirstSeg  = "0.00300000" // 第一段的预留总额（输入侧 + 一个段）
	freezeOutRate   = "0.000002"   // 输出侧单价（max(completion, reasoning) = 2e-6）
)

// freezeRequest 是一条**流式**请求：夹具默认那条是非流式的（Streamed 为假），
// 而两条路在 begin 里分岔（见 begin 里那一段）——不置这一位，用例测的还是
// 「按声明上界冻满」那条老路。
//
// MaxTokens 取得远大于阈值段：这正是要修的那个形状（调用方把上界填得很大）。
func freezeRequest() pipeline.Request {
	req := request()
	req.Streamed = true
	req.MaxTokens = 100000
	return req
}

// withStreamFreezeStep 把阈值段压到夹具的 max_tokens 以下。
//
// 夹具默认给 4096（与 settings 的默认值一致），而 request() 声明的上界是 500
// ——那时第一段就把整笔冻满、一次补冻都不发生（见 harness.streamFreezeStep）。
func withStreamFreezeStep(n int64) func(*harness) {
	return func(h *harness) { h.streamFreezeStep = n }
}

// chunk 造一段大约 n 个 token 的正文。
//
// 大约而不是精确：折算成 token 靠的是估算器（见 producedOf），用例不自己另算
// 一个数——那样断言的会是一个与生产无关的量。
func chunk(n int) string { return strings.Repeat("token ", n) }

// producedOf 把一串正文按**估算器同一个来源**折算成输出 token 数。
//
// 夹具里的网关没有接词表（Gateway.Counter 是零值），估算器因此走启发式那一条
// 路（见 tokens.EstimatedTokens）。用例要回答「到这一刻已经产出了多少 token」，
// 就只能用同一个函数。
func producedOf(texts ...string) int64 {
	var n int64
	for _, s := range texts {
		n += tokens.EstimatedTokens("gpt-4o", s)
	}
	return n
}

// mustAmount 解析一个金额字面量。
func mustAmount(t *testing.T, s string) money.Amount {
	t.Helper()
	amt, err := money.Parse(s)
	if err != nil {
		t.Fatalf("解析金额 %q 失败: %v", s, err)
	}
	return amt
}

// topUpBudget 造一个**按可用余额**决定成败的 onAdd。
//
// 它模拟的是落库层那条准入谓词（`balance - frozen >= delta`）：余额按总额给，
// 初始预留先扣掉，此后每次补冻再从里面扣——够不着就回
// pipeline.ErrInsufficientAvailable，与真实实现逐字相同的那个错误值。
//
// 用「余额」而不是「第几次调用会失败」来配：用例里那个数于是有明确的意思
// （这个用户账上还剩多少可以用），而「第几次」是一个会随实现里检查点位置变
// 的数——那种用例会在一次无害的重构之后开始测别的东西。
func topUpBudget(initial, balance money.Amount) (func(pipeline.Request, money.Amount) error, *[]money.Amount) {
	available := balance.Sub(initial)
	granted := &[]money.Amount{}
	return func(_ pipeline.Request, delta money.Amount) error {
		if delta.Cmp(available) > 0 {
			return fmt.Errorf("%w: 可用 %s，要冻 %s", pipeline.ErrInsufficientAvailable, available, delta)
		}
		available = available.Sub(delta)
		*granted = append(*granted, delta)
		return nil
	}, granted
}

// frozenAtSettle 是结算那一刻一共冻住了多少。
//
// 从 Charge.Settlement 上取而不是从预留对象上取：那个对象在流中间被就地推进
// 过（见 fakeReservations.SaveReservation 的注释），而结算读的就是推进之后的
// 那个值——它才是这条路上真正生效的冻结额。
func frozenAtSettle(t *testing.T, res *Result) money.Amount {
	t.Helper()
	if res.Charge == nil {
		t.Fatal("没有结算，读不到冻结额")
	}
	return res.Charge.Settlement.Reserved
}

// grantedTotal 是补冻记录里那些成功追加的金额合计。
func grantedTotal(granted []money.Amount) money.Amount {
	total := money.Zero
	for _, g := range granted {
		total = total.Add(g)
	}
	return total
}

// longScript 造一串增量，总量按 token 计远大于一个阈值段。
//
// 用增量而不是 usage 事件来推进：真实上游（OpenAI 那套）的 usage 只在流的最
// 后一片里出现，中途的产出量只能由本地估算给出——而「中途知道产出了多少」
// 正是递进冻结的前提。
func longScript(parts int, size int) []channel.StreamEvent {
	events := make([]channel.StreamEvent, 0, parts)
	for i := 0; i < parts; i++ {
		events = append(events, delta(chunk(size)))
	}
	return events
}

// ── 一条长输出跑完 ──────────────────────────────────────────────────────

// 余额够付实际消耗的流式请求**跑得起来**：开始之前只冻第一段，不按声明的
// max_tokens 冻满。
//
// 这是 #498 要修的那个问题本身：上界由调用方填，填大是常态，而按它冻满会让
// 一条实际只产几千个 token、余额完全够付的请求在开始之前就被 429 拒掉。
func TestChatStreamFreezesOnlyTheFirstSegmentUpFront(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(freezeStep))
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: append(longScript(8, 200), channel.StreamEvent{Kind: "done", FinishReason: "stop"}),
	}})

	res, err := h.gw.ChatStream(context.Background(), freezeRequest(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留记录数 = %d，期望 1", len(h.reservations.saved))
	}
	first := h.reservations.saved[0]
	// 声明的上界是 100000 个 token，按它冻满会是 0.201——这条断言就是那条
	// 修复：开始之前冻住的只有一段。
	if first.OutputTokens != freezeStep {
		t.Errorf("初始预留覆盖 %d 个输出 token，期望 %d（只冻第一段）", first.OutputTokens, freezeStep)
	}
	if got := first.Amount.String(); got != freezeFirstSeg {
		t.Errorf("初始预留 = %s，期望 %s（输入侧 + 一段）", got, freezeFirstSeg)
	}
}

// 边转发边补冻：每跨过一个段就补一笔，每笔都是**整段**的价。
//
// 按段对齐是必须的：不对齐的话每个事件都会算出一个新的目标值，补冻就从
// 「每段一次」退化成「每个事件一次」，一次几 token 的流会打出上百次数据库往返。
//
// 变异记录：pending 里两条守卫上的边界变异是**等价变异**，不必为它们写用例
// ——`if want > f.max` 改成 `>=` 时，相等那一步赋的还是 f.max 本身；`if want <=
// f.reserved.OutputTokens` 改成 `<` 时，相等那一步会走到
// `want - f.reserved.OutputTokens`，算出来同样是 0。
func TestChatStreamTopsUpByWholeSegments(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(freezeStep))
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: append(longScript(40, 200), channel.StreamEvent{Kind: "done", FinishReason: "stop"}),
	}})

	res, err := h.gw.ChatStream(context.Background(), freezeRequest(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	events := collect(t, res)

	if len(h.reservations.added) == 0 {
		t.Fatal("产出远超一个段时应当有补冻，实际一次都没有")
	}

	// 前提：一个检查窗口（一片增量）产出的量小于一个段。敞口那条判据正是在
	// 这个前提下成立的——一片就能越过一整个段时，任何冻结方案都来不及反应。
	perChunk := producedOf(chunk(200))
	if perChunk >= freezeStep {
		t.Fatalf("夹具的一片是 %d 个 token，不小于阈值段 %d——用例的前提不成立",
			perChunk, freezeStep)
	}

	// 产出越多，冻得越多：补冻的笔数应当与「跨过了几个段」对得上。数目是
	// 「跨过的段边界数」**加一**——那多出来的一笔在流的开头，它把领先量从 0
	// 补到一个段（初始那一笔只冻了一段，而设计要保持「领先一段」）。
	produced := producedOf(forwardedTexts(events)...)
	wantAdds := int(produced/freezeStep) + 1
	if got := len(h.reservations.added); got != wantAdds {
		t.Errorf("补冻 %d 笔，期望 %d 笔（已产出 %d 个 token，段长 %d）",
			got, wantAdds, produced, freezeStep)
	}

	for i, call := range h.reservations.added {
		// 每一笔都是整段的价：段与段之间不重复冻、也不冻半个段。
		if got := call.delta.Cmp(mustAmount(t, freezeSegment)); got != 0 {
			t.Errorf("第 %d 笔补冻 = %s，期望一个整段 %s", i+1, call.delta, freezeSegment)
		}
		// 补冻用的是**结算口径**的那份请求：选中渠道与商家已经填上。用受理时
		// 那份（一整个候选集）会让补冻按另一条规则取价——冻出来的数与结算
		// 算的数从此不是同一把尺子量的。
		if call.req.ChannelID != 1 {
			t.Errorf("第 %d 笔补冻的请求没有选中渠道：ChannelID = %d", i+1, call.req.ChannelID)
		}
		if len(call.req.Candidates) != 1 {
			t.Errorf("第 %d 笔补冻的候选集有 %d 个，期望收窄成选中的那一个",
				i+1, len(call.req.Candidates))
		}
	}

	// 跑完之后：结算按实际用量收，且**没撞上界**。
	if res.Charge == nil {
		t.Fatal("正常结束必须结算")
	}
	if res.Charge.Settlement.Capped {
		t.Errorf("撞上 Capped：冻结额 %s 没盖住实际用量 %s",
			frozenAtSettle(t, res), res.Charge.Settlement.Actual)
	}
	if !res.Charge.Settlement.Actual.IsPositive() {
		t.Error("结算金额应当是正的")
	}
	// 结算那一刻冻住的 = 落库的那一刻冻住的 + 所有成功的补冻。三者对不上说明
	// 有一笔补冻「冻在库里、没进结算读的那个数」（或者反过来），而那种偏差在
	// 账上只有一个表现：撞 Capped。
	want := h.reservations.saved[0].Amount.Add(grantedTotal(grantedFrom(h.reservations.added)))
	if got := frozenAtSettle(t, res); got.Cmp(want) != 0 {
		t.Errorf("结算用的冻结额 = %s，期望 %s（初始 %s + 补冻合计）",
			got, want, h.reservations.saved[0].Amount)
	}
}

// forwardedTexts 取出一串转发事件里的正文。
func forwardedTexts(events []channel.StreamEvent) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == "delta" {
			out = append(out, ev.Text)
		}
	}
	return out
}

// grantedFrom 把补冻记录折算成金额。
//
// 它含**每一次调用**，包括被余额挡下的那一次（夹具在决定成败之前就记下了
// 调用，与真实实现一样：那条 UPDATE 试过了才算数）。要「成功的那些」，用
// 用例自己那份 granted 切片（由 topUpBudget 给）。
func grantedFrom(adds []addCall) []money.Amount {
	out := make([]money.Amount, 0, len(adds))
	for _, a := range adds {
		out = append(out, a.delta)
	}
	return out
}

// ── 余额耗尽，切断这条流 ────────────────────────────────────────────────

// 补冻够不着余额时这条流被切断，而且切断的那一刻**已产出部分全都冻住了**。
//
// 这是这套设计最要紧的一条：检查排在转发之后，中断意味着这一次补冻没成功
// ——若冻结额只是追平已产出量，那一刻就会留下一小段没冻住的产出。结算到它时
// 实际金额超过预留额、撞上 Capped，于是「上游不守信」这个信号被「用户没钱了」
// 这件事污染（差额由平台承担，而账目仍然是平的，对账看不出来）。
//
// 领先一个段之后，中断时已产出量始终落在上一次冻住的额度之内：结算按实际用量
// 收、不撞上界，余额也不会变负（本轮不引入透支）。
func TestChatStreamCutsOffWhenTopUpRunsOutOfBalance(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(freezeStep))
	initial := mustAmount(t, freezeFirstSeg)
	// 余额 0.0075：第一段冻掉 0.003，剩下 0.0045 够两次补冻（0.002 × 2），
	// 第三次要的那一段就够不着了。
	onAdd, granted := topUpBudget(initial, mustAmount(t, "0.0075"))
	h.reservations.onAdd = onAdd
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: longScript(40, 200),
	}})

	before := observeStreamBalanceExhausted(t)

	res, err := h.gw.ChatStream(context.Background(), freezeRequest(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	events := collect(t, res)

	// 1. **不补正常结束标记**：伪造一个 done 会让调用方把半截回答当成完整
	// 回答（答到一半的内容可能与完整的答案意思相反）。
	if len(events) == 0 {
		t.Fatal("一条事件都没有")
	}
	for _, ev := range events {
		if ev.Kind == "done" {
			t.Error("被切断的流里出现了 done——那会让「回答被截断」看起来像「答完了」")
		}
	}

	// 2. 调用方看到一条流内错误帧，而且它是最后一条事件
	last := events[len(events)-1]
	if last.Kind != "error" {
		t.Fatalf("最后一条事件是 %q，期望一条 error（中断必须告诉调用方）", last.Kind)
	}
	if !errors.Is(last.Err, pipeline.ErrInsufficientAvailable) {
		t.Errorf("错误帧 = %v，期望 ErrInsufficientAvailable", last.Err)
	}

	// 3. 敞口为零：切断时已产出的部分全部落在冻结额之内
	produced := producedOf(forwardedTexts(events)...)
	if produced == 0 {
		t.Fatal("中断之前一片正文都没转发出去，这条用例没测到要测的事")
	}
	perChunk := producedOf(chunk(200))
	if perChunk >= freezeStep {
		t.Fatalf("夹具的一片是 %d 个 token，不小于阈值段 %d——用例的前提不成立", perChunk, freezeStep)
	}
	// 已产出那部分按输出侧的价折算，再加上输入侧那一笔（初始预留里就冻住了）。
	need := mustAmount(t, freezeInputCost).
		Add(mustAmount(t, freezeOutRate).MulInt(produced))
	frozen := frozenAtSettle(t, res)
	if frozen.Cmp(need) < 0 {
		t.Errorf("切断时冻住的 %s 盖不住已产出的部分（按输出侧的价算 %s）——"+
			"这段尾巴会在结算时撞上 Capped", frozen, need)
	}
	// 领先量不超过两个段：冻太多等于把用户的钱白占住。
	lead := frozen.Sub(need)
	if lead.Cmp(mustAmount(t, freezeSegment).MulInt(2)) >= 0 {
		t.Errorf("领先量 %s 达到或超过两个阈值段——冻得太多", lead)
	}

	// 4. 已产出的部分**按实际用量结算**，不撞上界
	if res.Charge == nil {
		t.Fatal("中断之前已经产出的部分要计费")
	}
	if res.Charge.Settlement.Capped {
		t.Errorf("中断那次结算撞上了 Capped（实际 %s，预留 %s）——"+
			"中断不该让平台来承担差额",
			res.Charge.Settlement.Breakdown.Total, res.Charge.Settlement.Reserved)
	}
	if res.Charge.Settlement.Actual.IsZero() {
		t.Error("结算金额为零：已经产出的部分白送了")
	}

	// 5. 计数留痕，日志里也指得到是哪一次（见 StreamBalanceExhausted 的注释）
	if got := observeStreamBalanceExhausted(t); got != before+1 {
		t.Errorf("被切断的流式请求计数 %d → %d，期望恰好加一", before, got)
	}

	// 6. 断在**第一次付不起的补冻**上：余额付得起的那两笔都冻上了，第三笔没
	//    冻上就把这条流停了——不多跑一段，也不提前断。
	if len(*granted) != 2 {
		t.Errorf("成功补冻 %d 笔，期望 2 笔（余额付得起两段）", len(*granted))
	}
	if got := len(h.reservations.added); got != len(*granted)+1 {
		t.Errorf("补冻尝试 %d 笔、成功 %d 笔，期望恰好差一笔（付不起的那一笔就是最后一次尝试）",
			got, len(*granted))
	}
}

// observeStreamBalanceExhausted 读「因余额不足被切断」那个计数器的当前值。
//
// 与 hooks_test 里 UnbilledSettlements 那条同一个理由：增量点直接读包级的
// observe.Metrics，夹具换不掉它，所以用例比的是**增量**。
func observeStreamBalanceExhausted(t *testing.T) uint64 {
	t.Helper()
	return observe.Metrics.StreamBalanceExhausted.Value()
}

// 我们主动切断的流**不算渠道的失败**：既不加连续失败也不清零。
//
// 把一次「我们没钱了」记成渠道挂了，会让一个完全正常的渠道被降权——那正是
// #498 里说的「不污染渠道健康度」。它与「客户端断开」同路（都不归因于上游），
// 而与「上游截断」不同路（那是渠道此刻不行）。
func TestChatStreamInterruptIsNotChannelFailure(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(freezeStep))
	onAdd, _ := topUpBudget(mustAmount(t, freezeFirstSeg), mustAmount(t, freezeFirstSeg))
	h.reservations.onAdd = onAdd
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: longScript(40, 200),
	}})

	res, err := h.gw.ChatStream(context.Background(), freezeRequest(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)

	rec := h.recorder.only(t)
	if rec.Verdict != route.VerdictUnattributed {
		t.Errorf("结局 = %v，期望 Unattributed（不是渠道的失败，也不是成功）", rec.Verdict)
	}
	if rec.ChannelID != 1 {
		t.Errorf("记的渠道 = %d，期望 1（这一次真的打到了它）", rec.ChannelID)
	}
	// 对照：同一条流的**上游截断**要记成失败。只钉上面那条的话，把判据写成
	// 恒为 Unattributed 一样能过，而那样渠道健康度就再也读不出问题了。
	if !channelFailureOnTruncation(t) {
		t.Error("上游截断没有被记成渠道失败——健康度那一维对真正的问题失明了")
	}
}

// channelFailureOnTruncation 跑一条「吐过字之后上游掐断」的流，返回它有没有
// 被记成渠道的失败。
func channelFailureOnTruncation(t *testing.T) bool {
	t.Helper()
	h := newHarness(t, withStreamFreezeStep(freezeStep))
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("半句")}, // 没有 done，也没有 error：上游静默掐断
	}})

	res, err := h.gw.ChatStream(context.Background(), freezeRequest(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	collect(t, res)
	return h.recorder.only(t).Verdict == route.VerdictFailure
}

// ── 补冻写库失败：放行，但记成未充分冻结 ────────────────────────────────

// 补冻那一步写库失败时**不切断这条流**：捂不住的那一侧是平台自己的问题，
// 不该让一次正常的请求失败。但这次请求从此是「未充分冻结」的——它必须**可观测**，
// 否则「冻结额没跟上产出」这件事没有任何东西会报出来。
//
// 代价如实写在断言里：之后产出的 token 没有对应的冻结额，结算到它们时实际金额
// 超过预留额、撞上 Capped，差额由平台承担（账目仍然是平的，对账看不出来——
// 所以那个计数器是唯一的即时信号）。
func TestChatStreamKeepsGoingWhenTopUpWriteFails(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(freezeStep))
	h.reservations.onAdd = func(pipeline.Request, money.Amount) error {
		return errors.New("库在报错")
	}
	logs := adoptLogs(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: append(longScript(40, 200), channel.StreamEvent{Kind: "done", FinishReason: "stop"}),
	}})

	before := observe.Metrics.StreamUnderfrozen.Value()

	res, err := h.gw.ChatStream(context.Background(), freezeRequest(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	events := collect(t, res)

	// 1. 流照常跑完：错误帧一条都不该有（那是给「钱不够」用的），done 照发
	for _, ev := range events {
		if ev.Kind == "error" {
			t.Errorf("补冻写库失败不该切断这条流，实际注入了错误帧: %v", ev.Err)
		}
	}
	if got := events[len(events)-1].Kind; got != "done" {
		t.Errorf("最后一条事件是 %q，期望 done（流照常结束）", got)
	}

	// 2. 计数与日志各留一次痕：指标答「多少次」，日志答「是哪一次」
	if got := observe.Metrics.StreamUnderfrozen.Value(); got != before+1 {
		t.Errorf("未充分冻结计数 %d → %d，期望恰好加一（每个请求只记一次）", before, got)
	}
	if !strings.Contains(logs.text(), "未充分冻结") {
		t.Errorf("日志里没有留下未充分冻结的痕迹:\n%s", logs.text())
	}

	// 3. 后果：结算撞上界——冻结额只有初始那一段，而产出远超它
	if res.Charge == nil {
		t.Fatal("正常结束必须结算")
	}
	if !res.Charge.Settlement.Capped {
		t.Error("补给没冻上，结算却设撞上界——那说明这条路上的实际金额被少算了")
	}
}

// ── 配置不对时不静默 ────────────────────────────────────────────────────

// 阈值段缺失（或非正）时**拒绝受理**，而且不碰上游。
//
// 零值不是「不限」也不是「每次事件都查」：前者让补冻整条不发生（流式又退回按
// 声明上界冻），后者把数据库往返摊到每一个事件上。两种都不会报错，只会安静地
// 跑错——所以它必须在开始之前就失败（见 Gateway.StreamFreezeStep）。
//
// 变异记录：`g.StreamFreezeStep <= 0` 改成 `< 0` 时，阈值段为 0 的这一条会漏过
// 网关这道，一路走到 pipeline.ReserveStream 才被挡下。两层用的是**同一个哨兵**
// （ErrNoFreezeStep），所以只比哨兵的话分不出是哪一层拒的；分得开的是**拒在哪
// 一层**：网关这一层把哨兵原样抛出，下面那层会在它外面包一句「当前 0」。
//
// 所以这条用例连错误的文本一起比。它钉的是「第二道防线自己就拒了，而没把这一步交给
// 下游」——网关是公开类型，装配之外还有人能造它出来，这道防线不生效时，拒绝
// 发生在哪一层就没人说得出。
func TestChatStreamRejectsMissingFreezeStep(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(0))
	exec := useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "done"}},
	}})

	_, err := h.gw.ChatStream(context.Background(), freezeRequest(), upstreamRequest(), Hooks{})
	if !errors.Is(err, pipeline.ErrNoFreezeStep) {
		t.Fatalf("err = %v，期望拒的是 %v 这一族", err, pipeline.ErrNoFreezeStep)
	}
	// 分得开「拒在哪一层」的是错误的文本，不是 errors.Is——下面那层在哨兵外面
	// 包了「当前 0」之后，errors.Is 照样为真，只有文本变了。
	if err.Error() != pipeline.ErrNoFreezeStep.Error() {
		t.Fatalf("err = %v，期望网关这一层原样抛出的 %v（不是下面那层包过的）",
			err, pipeline.ErrNoFreezeStep)
	}
	if calls := exec.calledChannels(); len(calls) != 0 {
		t.Errorf("拒绝发生在调上游之前，实际打了渠道 %v", calls)
	}
	if len(h.reservations.saved) != 0 {
		t.Errorf("算不出该冻多少就不该落预留，实际落了 %d 次", len(h.reservations.saved))
	}
}

// 阈值段为 0 而请求又**不是流式**的（Streamed=false）时必须不 panic。
//
// pumpStream 无论 Streamed 真假都会造 streamFreeze（见它的调用点），而 begin
// 只在**流式**那一支挡了 StreamFreezeStep<=0（见 gateway.go）。于是「ChatStream
// + Streamed=false + 阈值段为 0」会让 pumpStream 拿着 step=0 一路跑到补冻那一步，
// 在 roundUpTo 的整数除零上 panic——而崩点离原因（一个没设的字段）很远。网关是
// 公开类型，一个忘了设这个字段的新夹具就能撞上。
//
// 零值的语义是「不递进冻结」：这条流该照常跑完，一次补冻都不发生，也不崩。
func TestChatStreamWithZeroFreezeStepDoesNotPanic(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(0))
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: append(longScript(8, 200), channel.StreamEvent{Kind: "done", FinishReason: "stop"}),
	}})

	// request() 的 Streamed 为假，走的是 begin 的非流式那一支——正是那条
	// 绕开了守卫的路。它的 max_tokens（500）在单次上限之内，于是一路走到
	// pumpStream，产出足够触发补冻检查。
	req := request()

	res, err := h.gw.ChatStream(context.Background(), req, upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	events := collect(t, res)

	if got := events[len(events)-1].Kind; got != "done" {
		t.Errorf("最后一条事件是 %q，期望 done（流该照常跑完）", got)
	}
	for _, ev := range events {
		if ev.Kind == "error" {
			t.Errorf("阈值段为 0 不该注入错误帧，实际有: %v", ev.Err)
		}
	}
	if got := len(h.reservations.added); got != 0 {
		t.Errorf("阈值段为 0 时不该补冻，实际补了 %d 次", got)
	}
}

// 无效的 max_tokens 仍然在**开始之前**被拒（4xx 那条判据在入口层，这里钉的是
// 它没有被递进冻结改掉）：先冻一段不等于「没有上界也能跑」。
func TestChatStreamRejectsInvalidMaxTokensBeforeStreaming(t *testing.T) {
	h := newHarness(t, withStreamFreezeStep(freezeStep))
	exec := useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "done"}},
	}})

	req := freezeRequest()
	req.MaxTokens = 0

	_, err := h.gw.ChatStream(context.Background(), req, upstreamRequest(), Hooks{})
	if !errors.Is(err, billing.ErrNoUpperBound) {
		t.Fatalf("err = %v，期望 ErrNoUpperBound", err)
	}
	if calls := exec.calledChannels(); len(calls) != 0 {
		t.Errorf("拒绝发生在调上游之前，实际打了渠道 %v", calls)
	}
}

// ── 判据本身：已产出量的口径与段的对齐 ──────────────────────────────────

// 已产出量取**估算量与上游实测量里大的那一侧**。
//
// 这里逐格钉住 produced 的判据。既有的流式用例全用增量推进（一个 usage 事件
// 都不发），而发 usage 的那些走的都是非流式请求——那条路上补冻的目标值被声明
// 的上界夹住、逐次算出来都是 0，produced 取多少根本不影响结果。所以「取大的
// 那一侧」这句话在此之前没有任何用例钉过。
//
// 反过来（取小的那一侧）的错法是静默的：冻结额会落后于真实产出，结算时实际
// 金额超过预留额、撞上 Capped，差额由平台承担，而账目仍然是平的——对账看不
// 出来，只有那个计数器会说。
//
// 变异记录：同一行上 `>` 改成 `>=` 是**等价变异**（相等时赋的就是 n 自己，
// 值不变），不必为它写用例。
func TestStreamFreezeProducedTakesTheLargerSide(t *testing.T) {
	f := &streamFreeze{}
	est := tokens.NewStreamEstimator(nil, "gpt-4o")
	est.ObserveDelta(chunk(200))
	estimated := est.Result().Output.Tokens
	if estimated <= 3 {
		t.Fatalf("夹具的估算量只有 %d 个 token，用例的前提不成立", estimated)
	}

	for _, tc := range []struct {
		name     string
		measured *channel.Usage
		want     int64
	}{
		{"实测更小：以本地估算为准", &channel.Usage{CompletionTokens: 3}, estimated},
		{"实测更大：以上游报的为准", &channel.Usage{CompletionTokens: estimated + 100}, estimated + 100},
		{"还没有实测：以本地估算为准", nil, estimated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.produced(est, tc.measured); got != tc.want {
				t.Errorf("produced = %d 个 token，期望 %d", got, tc.want)
			}
		})
	}
}

// roundUpTo 把已产出量向上对齐到段的整数倍，三个点各有各的意思。
//
// 「还差一个」与「正好落在边界上」都该只对到段本身，越过边界一个才是下一段的
// 起点。这条边界就是「多冻一段」的分界线：多进一段等于把用户的钱白占住，而少
// 冻一头会撞上 Capped——这套设计的两头都在这一个数上。
//
// 变异记录：`if n <= 0` 改成 `< 0` 是**等价变异**——n 为 0 时会去算
// ((0+step-1)/step)*step，而 step 为正时那个整除的商也是 0，两条路同值。
func TestRoundUpToSegmentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want int64
	}{
		{freezeStep - 1, freezeStep},
		{freezeStep, freezeStep},
		{freezeStep + 1, 2 * freezeStep},
	} {
		if got := roundUpTo(tc.n, freezeStep); got != tc.want {
			t.Errorf("roundUpTo(%d, %d) = %d，期望 %d", tc.n, freezeStep, got, tc.want)
		}
	}
}
