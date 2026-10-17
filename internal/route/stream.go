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
	// UpstreamModel 是这次请求**实际发出去**的上游模型名。
	//
	// 与非流式那条路上的 Result.UpstreamModel 同一件事：由本包给出来，因为
	// 只有这里知道最终用的是哪一次翻译（见 Router.upstreamRequest）。失败
	// 路径上它没有意义。
	UpstreamModel string
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
	return r.ChatStreamPlan(ctx, exec, req, r.Select(candidates, canonicalModel), 0)
}

// ChatStreamPlan 是流式那条路上的 ChatPlan：按一份已经算好的选路结果执行，
// 并把 pinChannelID 钉在第一位。
//
// 「已经吐字之后不能换渠道」这条边界由 chatStreamWithPlan 保证，与这里
// 有没有钉一个首渠道无关——钉住的渠道同样受那条边界约束。
//
// 其余语义（为什么需要这个入口、pinChannelID 为 0 与找不到时的行为）
// 见 ChatPlan。
func (r *Router) ChatStreamPlan(ctx context.Context, exec StreamExecutor, req *channel.Request, plan Plan, pinChannelID int64) (*StreamResult, error) {
	return r.chatStreamWithPlan(ctx, exec, req, plan.pinFirst(pinChannelID))
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

		attempt, aliasErr := r.upstreamRequest(ctx, req, c)
		if aliasErr != nil {
			// 与非流式那条路同一个处置：配置问题不换下一个候选（见
			// chatWithPlan）。已经试过的那几次照常带回去记健康度。
			return result, aliasErr
		}

		started := r.now()
		events, err := exec.ChatStream(ctx, c.Channel, attempt)
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
			result.UpstreamModel = attempt.Model
			return result, nil
		}
		if !worthNextCandidate(err) {
			// 与非流式那条路同一个处置：没试过的候选记进 Untried，
			// 见 Errors 的注释。
			return result, &Errors{
				Attempts: result.Attempts,
				Untried:  len(plan.Candidates) - len(result.Attempts),
			}
		}
	}
	return result, &Errors{Attempts: result.Attempts}
}
