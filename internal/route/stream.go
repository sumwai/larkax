package route

import (
	"context"
	"errors"

	"github.com/sumwai/larkax/internal/channel"
)

// StreamExecutor 是流式执行器。与 Executor 分开是因为流式的失败边界不同：
// 非流式只有「成功/失败」两种结局，流式多了一个「已经吐字之后才失败」。
type StreamExecutor interface {
	ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error)
}

// StreamResult 是一次流式调用的结果。
type StreamResult struct {
	// Events 是发给客户端的事件流。
	Events <-chan channel.StreamEvent
	// Channel 是最终选中的渠道。
	Channel channel.Channel
	// Attempts 记录了换渠道过程中的每一次尝试。
	Attempts []Attempt
}

// ErrOutputStarted 表示流已经开始输出之后才失败，此时不能再换渠道。
//
// 单独一个错误类型是有用的：它与「全部候选失败」的处置完全不同——
// 后者可以告诉用户「服务暂时不可用，请重试」，而前者意味着
// **用户已经看到了一部分回答**，重试会让他看到两段拼接的内容。
var ErrOutputStarted = errors.New("流已开始输出，不能换渠道")

// ChatStream 按选路结果尝试流式请求。
//
// # 换渠道的边界
//
// 只在一个渠道**尚未向客户端输出任何内容**时才能换下一个。判据是
// 适配器的 ChatStream 返回值：
//
//   - 它返回 error → 上游在响应头阶段就拒绝了，还没有任何内容发出 → 可换渠道
//   - 它返回了 channel → 上游已接受请求，之后任何失败都属于「中途断了」 → 不能再换
//
// 所以本方法只在上游**接受请求之前**做重试，一旦拿到事件流就交给调用方。
// 这一点必须由这一层保证：适配器只看得到自己的那一次请求，
// 而「已经吐了多少字给客户端」只有这一层知道。
//
// # 为什么不在拿到第一个事件之后再决定
//
// 那看起来更精确（「第一个 delta 之前都还能换」），但会引入一个更糟的问题：
// 适配器在这段时间里已经读了一部分上游数据，换渠道意味着丢弃它们，
// 而上游可能已经为这些 token 计了费。以「上游是否接受请求」为界，
// 边界清晰且不产生已计费却被丢弃的数据。
func (r *Router) ChatStream(ctx context.Context, exec StreamExecutor, req *channel.Request, candidates []Candidate, canonicalModel string) (*StreamResult, error) {
	plan := r.Select(candidates, canonicalModel)
	return r.chatStreamWithPlan(ctx, exec, req, plan)
}

func (r *Router) chatStreamWithPlan(ctx context.Context, exec StreamExecutor, req *channel.Request, plan Plan) (*StreamResult, error) {
	if len(plan.Candidates) == 0 {
		return nil, &Errors{}
	}

	result := &StreamResult{}
	for _, c := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		started := r.now()
		events, err := exec.ChatStream(ctx, c.Channel, req)
		result.Attempts = append(result.Attempts, Attempt{
			ChannelID:   c.Channel.ID,
			ChannelName: c.Channel.Name,
			Err:         err,
			Retryable:   worthNextCandidate(err),
			StartedAt:   started,
			Duration:    r.now().Sub(started),
		})
		result.Channel = c.Channel

		if err == nil {
			result.Events = events
			return result, nil
		}
		if !worthNextCandidate(err) {
			return result, &Errors{Attempts: result.Attempts}
		}
	}
	return result, &Errors{Attempts: result.Attempts}
}
