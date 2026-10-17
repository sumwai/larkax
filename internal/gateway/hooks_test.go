package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 这个文件钉住 onResponse 这个挂载点**在顺序里的位置**与失败时的走向。
//
// 位置为什么值得单测：它排在 `routed.Response` 之后、`finishCharge` 之前。
// 靠后的那一步写的是账，靠前的那一步写的是要发给调用方的正文——顺序反了
// 不会报错，只会让「账单对应的那份响应」与「调用方拿到的那份」不是同一个，
// 而那种错在对账上表现为一笔对不上的差额。

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
