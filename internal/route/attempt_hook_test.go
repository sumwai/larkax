package route

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// OnAttempt 在每一次真的发起上游调用之前被调用，参数是那一次尝试的候选。
// 两条执行路径（非流式与流式）的触发点必须一致：额度探测只挂在其中一条上的
// 话，另一条路径的渠道永远不会被探测，而现场只是「额度一直是旧的」。
func TestOnAttemptFiresForEachAttemptedCandidate(t *testing.T) {
	var seen []int64
	r := seededRouter(1)
	r.OnAttempt = func(c Candidate) { seen = append(seen, c.Channel.ID) }

	exec := &fakeExecutor{results: map[int64]error{1: retryableErr()}}
	if _, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{
			candidate(1, "m", withPriority(10)),
			candidate(2, "m", withPriority(1)),
		}, "m"); err != nil {
		t.Fatalf("请求失败: %v", err)
	}

	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("OnAttempt 看到的渠道 = %v，期望 [1 2]（第一个失败后换下一个）", seen)
	}
	if len(exec.calls) != 2 {
		t.Errorf("实际发起的调用 = %v，期望两条", exec.calls)
	}
}

// 流式那条路同样每次尝试都触发。
func TestOnAttemptFiresOnStreamAttempts(t *testing.T) {
	var seen []int64
	r := seededRouter(1)
	r.OnAttempt = func(c Candidate) { seen = append(seen, c.Channel.ID) }

	exec := &fakeStreamExecutor{
		results: map[int64]error{1: retryableErr()},
	}
	if _, err := r.ChatStream(context.Background(), exec, &channel.Request{Model: "m"},
		[]Candidate{
			candidate(1, "m", withPriority(10)),
			candidate(2, "m", withPriority(1)),
		}, "m"); err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}

	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("流式 OnAttempt 看到的渠道 = %v，期望 [1 2]", seen)
	}
	if len(exec.calls) != 2 {
		t.Errorf("流式实际发起的调用 = %v，期望两条", exec.calls)
	}
}

// 别名翻不出来的尝试不触发 OnAttempt：那一次请求没有发出去，也就谈不上
// 「用到了这个渠道」。少这一条，一副失败的别名会让额度探测去查一个刚刚
// 根本没被使用的渠道。
func TestOnAttemptSkipsUnresolvedAlias(t *testing.T) {
	var seen []int64
	r := seededRouter(1)
	r.Aliases = failingAliases{}
	r.OnAttempt = func(c Candidate) { seen = append(seen, c.Channel.ID) }

	if _, err := r.Chat(context.Background(), &fakeExecutor{}, &channel.Request{Model: "m"},
		[]Candidate{candidate(1, "m")}, "m"); err == nil {
		t.Fatal("别名翻不出来时请求应当失败")
	}
	if len(seen) != 0 {
		t.Errorf("OnAttempt 看到的渠道 = %v，期望为空（别名失败，请求没有发出去）", seen)
	}
}

// failingAliases 让每次别名翻译都失败。
type failingAliases struct{}

func (failingAliases) Alias(context.Context, int64, string) (string, map[string]any, error) {
	return "", nil, errors.New("别名翻译失败（占位）")
}
