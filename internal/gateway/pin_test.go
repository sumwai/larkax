package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// 这个文件钉住 onRoute 这个挂载点的**效果**：钩子给的那个渠道要真的成为
// 这次请求走的渠道。
//
// # 为什么「钩子被调过」不算
//
// 那是一个比它看起来弱得多的断言：钩子跑过、请求 200、账单也对，三条同时
// 成立而钩子给的渠道被丢掉——现场是「插件选了一个渠道，请求发去了另一个」，
// 从响应、日志和账上都看不出来。所以下面每条都断言**执行器实际收到的渠道**。
//
// # 夹具为什么把顺序钉死
//
// 内核顺序由 priority 决定（见 route.order 的第 1 条规则），而 priority
// 是**硬性**的排序键，不受打分与随机打散影响。所以「不钉时先走 1 号」
// 这件事在夹具里是确定的，而它正是钉生效与否的对照。

// pinCandidate 造一个候选，用 priority 决定内核顺序（大者先）。
func pinCandidate(id int64, priority int) route.Candidate {
	c := candidate(id, "gpt-4o")
	c.Priority = priority
	return c
}

// pinHarness 造一个「两个候选、内核先选 1 号」的场景。
func pinHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(h *harness) {
		h.candidates = &fakeCandidates{cands: []route.Candidate{
			pinCandidate(1, 10),
			pinCandidate(2, 1),
		}}
	})
}

// 钩子钉住的渠道**就是**这次请求走的渠道，即使内核按优先级不会先选它。
func TestOnRoutePinDecidesWhichChannelIsCalled(t *testing.T) {
	// 对照组：不给挂载点时先走 1 号。没有它，下面那条在「实现根本没用
	// pin」时也可能成立（例如两个候选的顺序恰好一样）。
	plain := pinHarness(t)
	if _, err := plain.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got := plain.executor.calls; len(got) != 1 || got[0] != 1 {
		t.Fatalf("不钉时该先走 1 号，实际 %v", got)
	}

	h := pinHarness(t)
	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(context.Context, []route.Candidate) (int64, error) { return 2, nil },
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got := h.executor.calls; len(got) != 1 || got[0] != 2 {
		t.Errorf("钉了 2 号，执行器却按 %v 调用", got)
	}
	if res.Channel.ID != 2 {
		t.Errorf("结果渠道 = %d，要的是 2", res.Channel.ID)
	}
}

// 钩子拿到的候选是**筛过的那一份**，而且就是被执行的那一份。
//
// 两件事一起断言：
//
//   - 筛过：连续失败三次的渠道不该出现在钩子看到的清单里（否则它可能去
//     选一个注定失败的渠道，而那正是「候选集」这个说法要挡住的事）；
//   - 同一份：钩子看到的 ID 与这次请求真正尝试过的 ID 是同一个集合——
//     「它选了一个渠道，请求却发去了另一个」只可能发生在两者分叉时。
func TestOnRouteSeesFilteredCandidatesItWillExecute(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.candidates = &fakeCandidates{cands: []route.Candidate{
			pinCandidate(1, 10),
			pinCandidate(2, 1),
			// 连续失败三次：route 会把它筛掉（见 route.consecutiveFailureCutoff）。
			func() route.Candidate {
				c := pinCandidate(3, 99)
				c.Health.ConsecutiveFailures = 3
				return c
			}(),
		}}
	})

	var seen []int64
	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(_ context.Context, cands []route.Candidate) (int64, error) {
			for _, c := range cands {
				seen = append(seen, c.Channel.ID)
			}
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("钩子看到的候选 = %v，要的是筛过之后的 [1 2]", seen)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].ChannelID != 1 {
		t.Fatalf("尝试记录 = %+v", res.Attempts)
	}
	// 被执行的那个必须就在钩子看到的那份里。
	inSeen := false
	for _, id := range seen {
		if id == res.Attempts[0].ChannelID {
			inSeen = true
		}
	}
	if !inSeen {
		t.Errorf("执行的渠道 %d 不在钩子看到的 %v 里：两者不是同一份候选",
			res.Attempts[0].ChannelID, seen)
	}
}

// 钩子返回 0 表示不决策：顺序与插件系统出现之前完全一样。
func TestOnRouteZeroMeansNoDecision(t *testing.T) {
	h := pinHarness(t)

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(context.Context, []route.Candidate) (int64, error) { return 0, nil },
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got := h.executor.calls; len(got) != 1 || got[0] != 1 {
		t.Errorf("不决策时该按内核顺序走 1 号，实际 %v", got)
	}
	if res.Channel.ID != 1 {
		t.Errorf("结果渠道 = %d，要的是 1", res.Channel.ID)
	}
}

// 钩子报错表示**阻断**：这次请求失败，而且不计费。
//
// 「不计费」这一条要连着「一次上游都没调」一起看：只断言金额为零的话，
// 一个「先调了上游、再按失败结算」的实现也能过——而那是白花的钱，
// 只是账单上看不出来。
func TestOnRouteBlockingFailureFailsWithoutCallingUpstream(t *testing.T) {
	h := pinHarness(t)
	boom := errors.New("插件声明了阻断")

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(context.Context, []route.Candidate) (int64, error) { return 0, boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，要的是钩子那个", err)
	}
	if len(h.executor.calls) != 0 {
		t.Errorf("阻断时不该调上游，实际调了 %v", h.executor.calls)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("阻断时不该有账单，实际 %d 条", len(h.settlements.saved))
	}
	// 预留照旧要放开：冻着的钱与账单是两件事，漏掉释放就是一笔永久冻结。
	if len(h.reservations.released) != 1 {
		t.Errorf("预留释放 %d 次，期望 1 次", len(h.reservations.released))
	}
}

// 钩子在**调上游之前**跑，而且那时候选已经筛好了。
//
// 顺序反了不会报错：钩子排在执行之后时它照样被调到、返回的渠道照样合法，
// 只是这次请求已经发去别处了。所以断言落在「钩子跑的那一刻，执行器被调过
// 几次」——那是唯一能把两条顺序分开的量。
func TestOnRouteRunsBeforeUpstreamCall(t *testing.T) {
	h := pinHarness(t)

	callsAtHook := -1
	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(context.Context, []route.Candidate) (int64, error) {
			callsAtHook = len(h.executor.calls)
			return 0, nil
		},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if callsAtHook != 0 {
		t.Errorf("钩子跑的时候执行器已经被调过 %d 次，它必须排在调上游之前", callsAtHook)
	}
}

// 流式那条路上同样生效。
//
// 与非流式分开断言是有必要的：两个入口各有一份实现，只测一边时另一边
// 「收下了钩子却没用」不会有任何东西报出来，而现场是「插件对流式请求
// 静默不生效」——插件作者在日志里看不到任何异常。
func TestOnRoutePinAppliesToStream(t *testing.T) {
	plain := pinHarness(t)
	res, err := plain.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	<-res.Events
	if got := plain.executor.calls; len(got) != 1 || got[0] != 1 {
		t.Fatalf("不钉时该先走 1 号，实际 %v", got)
	}

	h := pinHarness(t)
	res, err = h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(context.Context, []route.Candidate) (int64, error) { return 2, nil },
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	<-res.Events
	if got := h.executor.calls; len(got) != 1 || got[0] != 2 {
		t.Errorf("钉了 2 号，执行器却按 %v 调用", got)
	}
	if res.Channel.ID != 2 {
		t.Errorf("结果渠道 = %d，要的是 2", res.Channel.ID)
	}
}

// 流式那条路上的阻断失败同样不计费，而且一个字都没发出去。
func TestOnRouteBlockingFailureOnStreamFailsBeforeAnyEvent(t *testing.T) {
	h := pinHarness(t)
	boom := errors.New("插件声明了阻断")

	res, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(context.Context, []route.Candidate) (int64, error) { return 0, boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，要的是钩子那个", err)
	}
	if res != nil {
		t.Errorf("阻断时不该有结果，实际 %+v", res)
	}
	if len(h.executor.calls) != 0 {
		t.Errorf("阻断时不该调上游，实际调了 %v", h.executor.calls)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("阻断时不该有账单，实际 %d 条", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("预留释放 %d 次，期望 1 次", len(h.reservations.released))
	}
}

// 钉住的渠道失败之后，剩下的候选按内核顺序继续——钉改的是顺序，不是集合。
func TestOnRoutePinFallsBackToKernelOrderOnRetryableFailure(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.candidates = &fakeCandidates{cands: []route.Candidate{
			pinCandidate(1, 10),
			pinCandidate(2, 1),
			pinCandidate(3, 5),
		}}
	})
	h.executor.failFor = map[int64]error{3: &channel.ErrUpstream{StatusCode: 500, Retryable: true}}

	res, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{
		OnRoute: func(context.Context, []route.Candidate) (int64, error) { return 3, nil },
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 3 号先试并失败，随后按内核顺序（1 号 priority 10 在前）走 1 号。
	if got := h.executor.calls; len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Errorf("调用序列 = %v，要的是 [3 1]", got)
	}
	if res.Channel.ID != 1 {
		t.Errorf("结果渠道 = %d，要的是 1", res.Channel.ID)
	}
}
