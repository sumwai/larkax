package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
)

// 这个文件钉住几个挂载点**在顺序里的位置**与失败时的走向。
//
// 位置为什么值得单测：它排在 `routed.Response` 之后、`finishCharge` 之前。
// 靠后的那一步写的是账，靠前的那一步写的是要发给调用方的正文——顺序反了
// 不会报错，只会让「账单对应的那份响应」与「调用方拿到的那份」不是同一个，
// 而那种错在对账上表现为一笔对不上的差额。
//
// onStream 与 onUsage 同理，见文件后半。

// hookOrderSettlements 在结算落库那一刻问一次「钩子跑过没有」。
//
// 问的是**那一刻**的状态而不是「跑过没有」：钩子跑了但排在结算之后时，
// 后一种记法照样是 true，而顺序恰恰是这里要钉住的东西。
type hookOrderSettlements struct {
	probe   func() bool
	called  bool
	sawHook bool
}

func (f *hookOrderSettlements) Save(_ context.Context, _ pipeline.Request, _ *pipeline.Charge) error {
	f.called = true
	f.sawHook = f.probe()
	return nil
}

// 钩子在结算**之前**跑：结算落库的那一刻它已经跑过了。
func TestOnResponseRunsBeforeSettlement(t *testing.T) {
	h := newHarness(t)

	hookRan := false
	settlements := &hookOrderSettlements{probe: func() bool { return hookRan }}
	h.gw.Settlements = settlements

	res, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{
		OnResponse: func(context.Context, *channel.Response) error {
			hookRan = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if !settlements.called {
		t.Fatal("结算该被调用")
	}
	if !settlements.sawHook {
		t.Error("结算落库时钩子还没跑：它必须排在 finishCharge 之前")
	}
	if res == nil || res.Charge == nil {
		t.Fatal("成功时该有结算结果")
	}
}

// 钩子拿到的是**上游返回的那份响应**（同一个对象），不是副本。
//
// 它改的就是要发给调用方的那份：改副本的话插件看起来生效了，而调用方
// 拿到的仍是原文——这类错在日志里完全看不出来。
func TestOnResponseHookGetsUpstreamResponse(t *testing.T) {
	h := newHarness(t)

	var seen *channel.Response
	res, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{
		OnResponse: func(_ context.Context, resp *channel.Response) error {
			seen = resp
			return nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if seen == nil {
		t.Fatal("钩子该被调用")
	}
	if seen != res.Response {
		t.Error("钩子拿到的该是要交回调用方的那一份（同一个对象）")
	}
	if seen != h.executor.resp {
		t.Error("钩子拿到的该是上游返回的那一份")
	}
}

// 阻断型插件失败：这次请求失败、**不计费**、预留放开。
//
// 上游其实成功了、钱也花掉了，但调用方拿到的是错误而不是那份回答——
// 把一次没送出去的回答记成消费，在对账上是一笔解释不通的扣费。
func TestBlockingOnResponseFailureIsNotBilled(t *testing.T) {
	h := newHarness(t)

	hookErr := errors.New("插件声明为阻断型，它的 onResponse 失败")
	res, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{
		OnResponse: func(context.Context, *channel.Response) error { return hookErr },
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("错误该是钩子给的那个，实际 %v", err)
	}
	if res != nil {
		t.Errorf("失败时不该有结果，实际 %+v", res)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("结算落库 %d 次，期望 0 次", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("预留释放 %d 次，期望 1 次", len(h.reservations.released))
	}
	// 上游确实被调过：这一条钉住「不计费的理由不是没调上游」。
	if len(h.executor.calls) != 1 {
		t.Errorf("上游被调用 %d 次，期望 1 次", len(h.executor.calls))
	}
}

// 阻断型插件失败时那次结算走的是**不计费**那条路，不只是「没落库」。
//
// # 为什么这条非写不可
//
// 上面那条用例（结算落库 0 次）钉不住这件事：settleAndFail 从来不落库，
// 所以「按不计费结算」与「按一笔正常结算算出来、然后把结果丢掉」在它眼里
// 一模一样——把 settleAndFail 里的 UpstreamFailed 去掉，它照样绿。
//
// # 为什么只能读全局计数器
//
// 这条路上唯一剩下的观测点是 observe.Metrics.UnbilledSettlements 的自增
// （pipeline.Settle 里那一行）。网关这一层的夹具换不掉它：Pipeline 上
// 没有可注入的注册表，增量点直接读包级的 observe.Metrics。所以这里比的是
// **增量**而不是取值，而包内用例默认串行，两次读取之间没有别的请求在跑。
//
// 这也正是 internal/observe 那条「要断言具体数值的测试应当自己 NewRegistry()」
// 的告诫管不到的情形：注册表不在本包的构造点手里。
func TestBlockingOnResponseFailureCountsAsUnbilled(t *testing.T) {
	h := newHarness(t)

	before := observe.Metrics.UnbilledSettlements.Value()
	_, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{
		OnResponse: func(context.Context, *channel.Response) error {
			return errors.New("插件拦下了这份响应")
		},
	})
	if err == nil {
		t.Fatal("阻断型插件失败该让这次请求失败")
	}

	if got := observe.Metrics.UnbilledSettlements.Value(); got != before+1 {
		t.Errorf("未计费结算 %d → %d，期望恰好加一：插件阻断的那次结算必须按「不计费」走。"+
			"少了它，账上会多出一笔零元结算，而调用方手里是一个错误——"+
			"一次没送出去的回答不该留下任何一笔消费记录", before, got)
	}
}

// 没有钩子时（零值）行为与钩子出现之前完全一样。
func TestOnResponseHookOptional(t *testing.T) {
	h := newHarness(t)

	res, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if res == nil || res.Response == nil {
		t.Fatal("成功时该有响应")
	}
	if len(h.settlements.saved) != 1 {
		t.Errorf("结算落库 %d 次，期望 1 次", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("成功时不该释放预留，实际 %d 次", len(h.reservations.released))
	}
}

// ── onStream ───────────────────────────────────────────────────────────

// 插件替换掉的正文进的是**计费依据**：进分词器的是替换后的文本，不是原文。
//
// # 这条钉的是什么
//
// 挂载点排在 est.ObserveDelta 之前还是之后，从响应、日志和账上都看不出来
// ——除非那条流没读完。所以这里让流**不带** usage（估算于是成为唯一的依据），
// 并让插件把每一块正文都换成空串：
//
//   - 顺序对：进分词器的是空串，输出 token 为 0
//   - 顺序错：原文已经数过了，输出 token 大于 0
//
// 对照组（同一份流、不替换）证明这条流确实带着正文——少了它，「0」也可能
// 只是因为一个字都没产生，而那是这条用例最容易骗过自己的地方。
func TestOnStreamReplaceIsWhatGetsCounted(t *testing.T) {
	script := map[int64][]channel.StreamEvent{
		1: {delta("原始正文，一段不短的文字"), {Kind: "done"}},
	}

	t.Run("插件把正文换成空串", func(t *testing.T) {
		h := newHarness(t)
		useStreamExecutor(h, &scriptedExecutor{script: script})

		empty := ""
		res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
			OnStream: func(context.Context, channel.StreamEvent, StreamChunk) (StreamDecision, error) {
				return StreamDecision{Replace: &empty}, nil
			},
		})
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		forwarded := collect(t, res)

		// 调用方看到的也是替换后的文本：替换不是只改账。
		for _, ev := range forwarded {
			if ev.Kind == "delta" && ev.Text != "" {
				t.Errorf("转发出去的正文 = %q，期望空串（插件替换过）", ev.Text)
			}
		}
		if got := res.Charge.Usage.CompletionTokens; got != 0 {
			t.Errorf("结算用的输出 token = %d，期望 0：替换后的正文才是计费依据，"+
				"进分词器的该是空串而不是原文（挂载点必须排在 est.ObserveDelta 之前）", got)
		}
	})

	t.Run("插件不替换", func(t *testing.T) {
		h := newHarness(t)
		useStreamExecutor(h, &scriptedExecutor{script: script})

		res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{})
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		collect(t, res)

		if got := res.Charge.Usage.CompletionTokens; got == 0 {
			t.Error("对照组：不替换时输出 token 该大于 0，否则上面那个 0 说明不了任何事")
		}
	})
}

// 交给插件的只有内容块，序号也只数内容块。
//
// # 为什么这条是权限边界而不是洁癖
//
// usage 是这次流的计费依据：允许插件丢掉它，就等于让插件绕过 onUsage 的
// 那道上界（计费退回内核估算）。done / error 是框架：丢掉 done 会让一次
// 正常结束看起来像被截断，还会把渠道健康度判成失败。所以那几种事件根本
// 不该走到插件面前——这一条因此要在**网关这一侧**钉住。
func TestOnStreamSeesContentChunksOnly(t *testing.T) {
	h := newHarness(t)
	usage := channel.Usage{PromptTokens: 5, CompletionTokens: 6}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("一"), {Kind: "usage", Usage: &usage}, delta("二"), {Kind: "done"}},
	}})

	var seen []string
	var texts []string
	res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
		OnStream: func(_ context.Context, ev channel.StreamEvent, in StreamChunk) (StreamDecision, error) {
			seen = append(seen, fmt.Sprintf("%d:%s", in.Index, ev.Kind))
			texts = append(texts, in.AccumulatedText)
			return StreamDecision{}, nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	collect(t, res)

	want := []string{"0:delta", "1:delta"}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("插件看到的块 = %v，期望 %v（usage / done 不进插件的手，序号也不数它们）", seen, want)
	}
	// 累计正文是**这一块之前**的那些，不含当前块：第一块看到空，第二块看到
	// 第一块的正文。插件要看全文自己拼（当前块在 chunk 里）。
	if want := []string{"", "一"}; fmt.Sprint(texts) != fmt.Sprint(want) {
		t.Errorf("各块看到的累计正文 = %q，期望 %q", texts, want)
	}
}

// Terminate 走的是与「上游结束 / 客户端断开」**同一条**收尾路径。
//
// # 为什么不能单独 break
//
// 单独一个 break（或直接 return）会让那条路绕开结算：已经吐给调用方的
// 正文一分钱没记，而预留被当成「不计费」放开——上游那次调用白花，账上
// 表现为「有请求无消费」。所以这里断言的是**结算真的落了库**，不是
// 「流停了」。
//
// ClientCanceled 必须是 false：插件终止不是客户端断的，而那个字段在对账
// 与告警上是另一件事（见 pipeline.Outcome.ClientCanceled）。
func TestOnStreamTerminateEndsThroughTheCommonPath(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("第一块"), delta("第二块"), {Kind: "done"}},
	}})

	res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
		OnStream: func(_ context.Context, _ channel.StreamEvent, in StreamChunk) (StreamDecision, error) {
			if in.Index == 1 {
				return StreamDecision{Terminate: true}, nil
			}
			return StreamDecision{}, nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	forwarded := collect(t, res)

	// 终止那一块自己也不转发：插件要表达「这一块先发出去再停」，是在
	// 下一块上终止（它每块都看得到）。
	var got []string
	for _, ev := range forwarded {
		if ev.Kind == "delta" {
			got = append(got, ev.Text)
		}
	}
	if want := []string{"第一块"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("转发出去的正文 = %v，期望 %v（终止的那一块与它之后的都不转发）", got, want)
	}
	if len(h.settlements.saved) != 1 {
		t.Fatalf("结算落库 %d 次，期望 1 次——插件终止也必须走结算（单独 break 会让它绕开）",
			len(h.settlements.saved))
	}
	if res.Charge == nil {
		t.Fatal("终止的流也该有结算结果")
	}
	if res.Charge.ClientCanceled {
		t.Error("插件终止不是客户端断的，ClientCanceled 该为 false")
	}
	if got := res.Charge.Usage.CompletionTokens; got == 0 {
		t.Error("已经吐给调用方的那一块要计费，输出 token 不该是 0")
	}
	// 渠道健康度：不是这个渠道的错，也不是一次成功。
	if got := h.recorder.only(t).Verdict; got != route.VerdictUnattributed {
		t.Errorf("渠道健康度判定 = %v，期望 unattributed（插件按自己的策略截的，与渠道无关）", got)
	}
}

// 阻断型插件在逐块决策里失败：这条流到此为止，收尾与 Terminate 相同。
//
// 「原样转发」是另一个方向（非阻断失败的处置），而它在这里正好是最坏的
// 那个：插件声明了 blocking（含义是「这个模型的请求必须经过我的检查」），
// 它挂掉之后未经检查的内容照旧流到调用方——声明了却静默不生效。
func TestBlockingOnStreamFailureEndsTheStream(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("第一块"), delta("第二块"), delta("第三块"), {Kind: "done"}},
	}})

	hookErr := errors.New("插件声明为阻断型，它的 onStream 失败")
	res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
		OnStream: func(_ context.Context, _ channel.StreamEvent, in StreamChunk) (StreamDecision, error) {
			if in.Index == 1 {
				return StreamDecision{}, hookErr
			}
			return StreamDecision{}, nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	forwarded := collect(t, res)

	var got []string
	for _, ev := range forwarded {
		if ev.Kind == "delta" {
			got = append(got, ev.Text)
		}
	}
	if want := []string{"第一块"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("转发出去的正文 = %v，期望 %v（失败之后一块都不该再出去）", got, want)
	}
	if len(h.settlements.saved) != 1 {
		t.Fatalf("结算落库 %d 次，期望 1 次（失败也要走结算，已经吐出去的要计费）",
			len(h.settlements.saved))
	}
	if res.Charge == nil || res.Charge.ClientCanceled {
		t.Errorf("阻断失败不是客户端断的，Charge = %+v", res.Charge)
	}
}

// 非阻断的失败**不改变这一块**：它原样转发（零值决策），流继续。
//
// 网关这一侧只认得「返回了错误」= 阻断，理由见 internal/api 的 pluginHooks：
// 非阻断的失败在那里已经记进会话、决策被丢掉。
func TestOnStreamFailureWithoutErrorKeepsTheChunk(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("第一块"), delta("第二块"), {Kind: "done"}},
	}})

	res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
		OnStream: func(context.Context, channel.StreamEvent, StreamChunk) (StreamDecision, error) {
			// 没有决策、也没有错误：这一块按原样走。
			return StreamDecision{}, nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	forwarded := collect(t, res)

	var got []string
	for _, ev := range forwarded {
		if ev.Kind == "delta" {
			got = append(got, ev.Text)
		}
	}
	if want := []string{"第一块", "第二块"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("转发出去的正文 = %v，期望 %v", got, want)
	}
}

// Drop 掉的那一块既不转发，也不进累计正文与分词计数。
//
// 它与 `replace: ""` 在计费上是同一件事——契约里那一条（见
// plugin.StreamDecision）就是在这里落地的。
func TestOnStreamDropLeavesNeitherTextNorTokens(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("会被丢掉的一整块"), {Kind: "done"}},
	}})

	res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
		OnStream: func(context.Context, channel.StreamEvent, StreamChunk) (StreamDecision, error) {
			return StreamDecision{Drop: true}, nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	forwarded := collect(t, res)

	for _, ev := range forwarded {
		if ev.Kind == "delta" {
			t.Errorf("被丢掉的块不该转发，实际转发了 %q", ev.Text)
		}
	}
	if got := res.Charge.Usage.CompletionTokens; got != 0 {
		t.Errorf("结算用的输出 token = %d，期望 0（丢掉的块连累计也不进）", got)
	}
}

// ── onUsage ────────────────────────────────────────────────────────────

// 非流式：插件交回来的用量就是**结算**用的那一份。
//
// 它改的是结算读的用量而不是响应，所以位置反了不会有任何报错：请求成功、
// 响应正确、日志里插件也跑了，只有那笔钱是按内核的估算收的。断言因此落在
// 结算结果上，而且是字面量。
func TestOnUsageReportIsWhatGetsBilled(t *testing.T) {
	const reported = 250

	// 对照组：没有挂载点时结算用内核那份（1000/100）。
	t.Run("没有插件", func(t *testing.T) {
		h := newHarness(t)
		res, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{})
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		if got := res.Charge.Usage.CompletionTokens; got != 100 {
			t.Errorf("结算用的输出 token = %d，期望 100（内核那份）", got)
		}
	})

	t.Run("插件报了别的数", func(t *testing.T) {
		h := newHarness(t)
		res, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{
			OnUsage: func(_ context.Context, core channel.Usage) (channel.Usage, error) {
				// 插件那侧做的是同一件事：在内核那份上覆盖它报了的项
				// （见 plugin.Session.ApplyUsage）。
				if core.CompletionTokens != 100 {
					t.Errorf("交给插件的内核用量 = %d，期望 100", core.CompletionTokens)
				}
				core.CompletionTokens = reported
				return core, nil
			},
		})
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		if got := res.Charge.Usage.CompletionTokens; got != reported {
			t.Errorf("结算用的输出 token = %d，期望 %d（插件报的那个数才是结算依据）", got, reported)
		}
		// 没报的项保留内核的值，而不是归零——归零会让一次漏报变成一次少收钱。
		if got := res.Charge.Usage.PromptTokens; got != 1000 {
			t.Errorf("结算用的输入 token = %d，期望 1000（插件没报这一项）", got)
		}
	})
}

// 非流式：阻断型插件在 onUsage 上失败 → 这次请求失败、不计费、预留放开。
//
// 与 onResponse 阻断同一个处置：响应还没发出去，所以「让这次请求失败」
// 在这里有可表达的对象。
func TestBlockingOnUsageFailureFailsTheRequest(t *testing.T) {
	h := newHarness(t)

	hookErr := errors.New("插件声明为阻断型，它的 onUsage 失败")
	res, err := h.gw.Chat(t.Context(), request(), upstreamRequest(), Hooks{
		OnUsage: func(context.Context, channel.Usage) (channel.Usage, error) {
			return channel.Usage{}, hookErr
		},
	})
	if !errors.Is(err, hookErr) {
		t.Fatalf("错误该是钩子给的那个，实际 %v", err)
	}
	if res != nil {
		t.Errorf("失败时不该有结果，实际 %+v", res)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("结算落库 %d 次，期望 0 次（没送出去的回答不计费）", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("预留释放 %d 次，期望 1 次", len(h.reservations.released))
	}
}

// 流式：插件交回来的用量同样进结算。
//
// 流里的实测用量是 12/7，插件报 250：结算按 250 走，而 Estimated 仍然是
// 上游那份口径（插件上报不是「上游实测」）。
func TestOnUsageOnStreamPathIsBilled(t *testing.T) {
	h := newHarness(t)
	measured := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("一"), {Kind: "usage", Usage: &measured}, {Kind: "done"}},
	}})

	const reported = 250
	res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
		OnUsage: func(_ context.Context, core channel.Usage) (channel.Usage, error) {
			if core.CompletionTokens != 7 {
				t.Errorf("交给插件的内核用量 = %d，期望 7（流里的实测值）", core.CompletionTokens)
			}
			core.CompletionTokens = reported
			return core, nil
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	collect(t, res)

	if got := res.Charge.Usage.CompletionTokens; got != reported {
		t.Errorf("结算用的输出 token = %d，期望 %d（onUsage 必须排在 billStream 之前）", got, reported)
	}
	if res.Estimated {
		t.Error("用量来源仍以上游那份为准：插件上报不是「上游实测」")
	}
	// 记给渠道的是**内核那份**：这一路记的是这个渠道自己消耗了什么。
	if got := h.recorder.only(t).Usage.CompletionTokens; got != 7 {
		t.Errorf("记给渠道的输出 token = %d，期望 7（内核那份，不是插件报的）", got)
	}
}

// 流式：阻断型插件在 onUsage 上失败**不改变任何东西**——整个回答已经送到
// 调用方手里了，没有「让请求失败」这个对象；按不计费结算则等于白送。
//
// 这里断言两件事：结算照走（按内核那份用量），以及失败不是静默的（网关
// 这一层留一条告警；会话里那条由入口记）。
func TestBlockingOnUsageFailureOnStreamStillBills(t *testing.T) {
	logs := adoptLogs(t)

	h := newHarness(t)
	measured := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("一"), {Kind: "usage", Usage: &measured}, {Kind: "done"}},
	}})

	res, err := h.gw.ChatStream(t.Context(), request(), upstreamRequest(), Hooks{
		OnUsage: func(context.Context, channel.Usage) (channel.Usage, error) {
			return channel.Usage{}, errors.New("插件声明为阻断型，它的 onUsage 失败")
		},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	collect(t, res)

	if len(h.settlements.saved) != 1 {
		t.Fatalf("结算落库 %d 次，期望 1 次（回答已经送出去了，不能不收钱）",
			len(h.settlements.saved))
	}
	if got := res.Charge.Usage.CompletionTokens; got != 7 {
		t.Errorf("结算用的输出 token = %d，期望 7（失败时按内核那份算）", got)
	}
	if !strings.Contains(logs.text(), "流式请求的 onUsage 失败") {
		t.Error("一次被吞掉的阻断失败必须留痕，否则它表现为「插件跑了但没用」")
	}
}

// ── 日志 ───────────────────────────────────────────────────────────────

// logBuffer 是并发安全的日志落点：网关的收尾跑在转发 goroutine 里。
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// adoptLogs 把进程默认日志器换成写进缓冲区的那个，并返回它。
//
// observe.FromContext 的日志器**只**来自 slog 的包级默认（见
// observe.AdoptAsDefault），所以捕获器得从那条路进去——与 internal/api
// 的 logCapture 同一个做法，只是这里要断言的只有一条告警。
func adoptLogs(t *testing.T) *logBuffer {
	t.Helper()
	b := &logBuffer{}
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	observe.AdoptAsDefault(observe.NewLoggerWithWriter(b, "debug", "text", false))
	return b
}
