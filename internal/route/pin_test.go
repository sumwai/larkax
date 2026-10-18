package route

import (
	"context"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 这个文件钉住 ChatPlan / ChatStreamPlan 上的那一个参数：pinChannelID。
//
// # 为什么断言落在「谁被调到」而不是「顺序对不对」
//
// 顺序是一个中间量，而它要影响的是**请求发去了哪个渠道**。两者在多数
// 情况下一致，分开的正是出错的那一处：把 pin 排进了 plan 却没交给执行器、
// 或者执行器拿到的还是原来那份——那时顺序看起来是对的，请求却发去了别处。

func req() *channel.Request { return &channel.Request{Model: "m"} }

// 钉住的渠道被**真的选中**，即使内核按优先级不会先选它。
//
// 两个候选的 priority 差着量级：不钉时先试的是 1 号（priority 10），
// 钉了 2 号之后先试的必须是 2 号。断言落在执行器收到的调用序列上——
// 只看 res.Channel 的话，「钉了但没生效、恰好 1 号也失败了」会蒙混过去。
func TestChatPlanPinsChannelToFront(t *testing.T) {
	cands := []Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}

	// 对照组：不钉，内核按 priority 先选 1 号。
	plain := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{UpstreamModel: "m"}}
	if _, err := seededRouter(1).Chat(context.Background(), plain, req(), cands, "m"); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(plain.calls) != 1 || plain.calls[0] != 1 {
		t.Fatalf("不钉时该先试 1 号，实际 %v", plain.calls)
	}

	pinned := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{UpstreamModel: "m"}}
	r := seededRouter(1)
	plan := r.Select(cands, "m", Needs{})
	res, err := r.ChatPlan(context.Background(), pinned, req(), plan, 2)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(pinned.calls) != 1 || pinned.calls[0] != 2 {
		t.Errorf("钉了 2 号，执行器却按 %v 调用", pinned.calls)
	}
	if res.Channel.ID != 2 {
		t.Errorf("结果渠道 = %d，要的是 2", res.Channel.ID)
	}
}

// pinChannelID 为 0 表示不钉：顺序与 Select 排出来的完全一致。
func TestChatPlanZeroPinKeepsKernelOrder(t *testing.T) {
	cands := []Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}
	r := seededRouter(1)

	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{UpstreamModel: "m"}}
	if _, err := r.ChatPlan(context.Background(), exec, req(), r.Select(cands, "m", Needs{}), 0); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(exec.calls) != 1 || exec.calls[0] != 1 {
		t.Errorf("pin 为 0 时该按内核顺序先试 1 号，实际 %v", exec.calls)
	}
}

// 钉住的渠道失败了照旧换下一个，剩下的候选一个不少、保持原顺序。
//
// 「钉」是排在最前，不是「只留它」——只留它会让一次上游抖动变成一次
// 请求失败，而钉住它的那个决策方只看得到候选集，看不到上游的状态。
func TestChatPlanPinnedChannelStillFailsOver(t *testing.T) {
	cands := []Candidate{
		candidate(1, "m", withPriority(10)),
		candidate(2, "m", withPriority(5)),
		candidate(3, "m", withPriority(1)),
	}
	exec := &fakeExecutor{
		results: map[int64]error{3: retryableErr()},
		resp:    &channel.Response{UpstreamModel: "m"},
	}
	r := seededRouter(1)

	res, err := r.ChatPlan(context.Background(), exec, req(), r.Select(cands, "m", Needs{}), 3)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 3 号先试并失败，随后按内核顺序走 1、2，最终成功的是 1 号。
	if got := exec.calls; len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Errorf("调用序列 = %v，要的是 [3 1]", got)
	}
	if res.Channel.ID != 1 {
		t.Errorf("结果渠道 = %d，要的是 1", res.Channel.ID)
	}
}

// 钉住的渠道**不在候选集里**时，顺序原样不变，而且不是错误。
//
// 这一条是「调用方传错了 plan」的兜底：决策者那一侧已经挡下了候选集之外的
// 决策（见 internal/plugin 的 pickChannel），走不到这里；真的走到这里时，
// 把一次传参失误升级成一次请求失败是更坏的处置。
func TestChatPlanPinOutsideCandidatesLeavesOrderAlone(t *testing.T) {
	cands := []Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}
	r := seededRouter(1)

	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{UpstreamModel: "m"}}
	res, err := r.ChatPlan(context.Background(), exec, req(), r.Select(cands, "m", Needs{}), 99)
	if err != nil {
		t.Fatalf("候选集之外的 pin 不该让请求失败: %v", err)
	}
	if len(exec.calls) != 1 || exec.calls[0] != 1 {
		t.Errorf("调用序列 = %v，要的是内核顺序的 [1]", exec.calls)
	}
	if res.Channel.ID != 1 {
		t.Errorf("结果渠道 = %d，要的是 1", res.Channel.ID)
	}
}

// 被筛掉的候选不会因为被钉而回来。
//
// 这是「由 route 自己完成筛选」那句要求的落点：钉只改**顺序**，不改
// **集合**。一个额度已耗尽的渠道被钉住时，它照样不在 plan 里，于是 pin
// 找不到它、顺序不变——请求不会发给一个注定失败的渠道。
func TestChatPlanCannotPinAnExcludedCandidate(t *testing.T) {
	cands := []Candidate{
		candidate(1, "m", withPriority(1)),
		candidate(2, "m", withPriority(10), withQuota(quotaLeft(0))),
	}
	r := seededRouter(1)
	plan := r.Select(cands, "m", Needs{})
	if len(plan.Candidates) != 1 || plan.Candidates[0].Channel.ID != 1 {
		t.Fatalf("夹具不对：额度耗尽的 2 号该被筛掉，plan = %+v", plan.Candidates)
	}

	exec := &fakeExecutor{results: map[int64]error{}, resp: &channel.Response{UpstreamModel: "m"}}
	if _, err := r.ChatPlan(context.Background(), exec, req(), plan, 2); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(exec.calls) != 1 || exec.calls[0] != 1 {
		t.Errorf("调用序列 = %v：被筛掉的渠道不该因为被钉就回来", exec.calls)
	}
}

// pinFirst 返回的是副本：调用方手上那份 plan 的顺序不该被改掉。
//
// 它拿着的是一份要被记进观测、还要在重试时复用的东西（见 Plan 的注释）。
// 就地排序不报错，只是让那边看到的东西静默变了——而那种变化在
// 「顺序恰好一样」时完全看不出来。
func TestPlanPinFirstDoesNotMutateInput(t *testing.T) {
	cands := []Candidate{candidate(1, "m"), candidate(2, "m"), candidate(3, "m")}
	plan := Plan{Candidates: cands}

	pinned := plan.pinFirst(3)
	if len(pinned.Candidates) != 3 || pinned.Candidates[0].Channel.ID != 3 {
		t.Fatalf("钉之后 = %+v", pinned.Candidates)
	}
	// 剩下的两个保持原顺序。
	if pinned.Candidates[1].Channel.ID != 1 || pinned.Candidates[2].Channel.ID != 2 {
		t.Errorf("其余候选的顺序变了: %+v", pinned.Candidates)
	}
	for i, want := range []int64{1, 2, 3} {
		if plan.Candidates[i].Channel.ID != want {
			t.Fatalf("入参被改动了: %v", []int64{
				plan.Candidates[0].Channel.ID,
				plan.Candidates[1].Channel.ID,
				plan.Candidates[2].Channel.ID,
			})
		}
	}
}

// 已经在最前的候选被钉住时，plan 原样返回（连切片都不必重建）。
//
// 变异：`if idx <= 0` 改成 `< 0`。这一处是**等价变异**：`idx == 0` 时下面
// 那段重建只是把同一份列表重新拼了一遍——`[Candidates[0]]` 接上
// `Candidates[:0]`（空的）再接上 `Candidates[1:]`，逐项、个数、顺序都与
// 入参相同，所以「本来就在最前」这一格上两版交回的候选列表一模一样。
//
// 这一格分不开的是**切片本身**：原版把入参那个切片原样交回，变异版交回
// 一个新建的（元素相同、cap 不同）。本仓没有读切片身份或 cap 的地方，
// 而且两种写法都满足 pinFirst 的契约「不改动入参」——所以没有任何输入
// 能让两版在**可观察的行为**上分开。（同类：容量提示变异。）
func TestPlanPinFirstOnLeadingCandidateIsNoop(t *testing.T) {
	plan := Plan{Candidates: []Candidate{candidate(1, "m"), candidate(2, "m")}}
	if got := plan.pinFirst(1); got.Candidates[0].Channel.ID != 1 || len(got.Candidates) != 2 {
		t.Errorf("钉已经在最前的那个候选之后 = %+v", got.Candidates)
	}
}

// 流式那条路上同样生效。
//
// 与非流式分开断言是有必要的：两个入口各有一份实现，只测一边时另一边
// 「收下了 pin 却没用」不会有任何东西报出来，而现场是「插件对流式请求
// 静默不生效」——插件作者在日志里看不到任何异常。
func TestChatStreamPlanPinsChannelToFront(t *testing.T) {
	cands := []Candidate{candidate(1, "m", withPriority(10)), candidate(2, "m", withPriority(1))}
	r := seededRouter(1)

	plain := &fakeStreamExecutor{results: map[int64]error{}}
	if _, err := r.ChatStream(context.Background(), plain, req(), cands, "m"); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(plain.calls) != 1 || plain.calls[0] != 1 {
		t.Fatalf("不钉时该先试 1 号，实际 %v", plain.calls)
	}

	pinned := &fakeStreamExecutor{results: map[int64]error{}}
	res, err := r.ChatStreamPlan(context.Background(), pinned, req(), r.Select(cands, "m", Needs{}), 2)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(pinned.calls) != 1 || pinned.calls[0] != 2 {
		t.Errorf("钉了 2 号，执行器却按 %v 调用", pinned.calls)
	}
	if res.Channel.ID != 2 {
		t.Errorf("结果渠道 = %d，要的是 2", res.Channel.ID)
	}
}
