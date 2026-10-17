package gateway

// 本文件是流式那条路：与非流式同样的顺序（预留 → 选路 → 调上游 → 结算），
// 中间换成一串事件。
//
// # 与非流式的三处不同
//
//  1. **收尾在后台**。用量只有读完流才知道，而 ChatStream 必须在上游接受
//     请求之后**立刻**返回——否则调用方拿不到事件流，也就没法边收边发给
//     客户端，流式就退化成了攒完再发。所以结算与释放都放在转发事件的那个
//     goroutine 里，并且在**关闭事件通道之前**完成：调用方读完流这件事
//     本身就等于「账已经记好了」，不必再等一个信号，测试也不必靠 sleep。
//  2. **失败分两种**。上游在接受请求之前失败 → 一个字都没吐，可以换渠道，
//     也可以像非流式那样回一个错误响应，不计费；接受之后失败 → 已经吐字，
//     不能换渠道（route.ErrOutputStarted），已产生的部分要计费。
//  3. **客户端断开要计费**。已经产生的 token 上游是算了钱的，用量由
//     tokens.StreamEstimator 当场估算。
//
// # 三条规则仍然只有一份
//
// 「预留必须在调上游之前」「失败就释放预留」「结算用实际选中的渠道」都
// 复用 begin / finish / finishCharge / settleAndFail——本文件不重新实现
// 一遍顺序。多一份实现就多一处会漏掉释放的地方，而漏掉的那一处是一笔
// 永久冻着的钱，且不会有任何东西报出来。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/tokens"
)

// ChatStream 处理一次流式请求。
//
// 返回的 Result.Events 是给客户端的事件流。**调用方必须读到它关闭为止**：
// 结算与释放发生在关闭之前，不读到底，转发 goroutine 会阻塞在发送上，
// 那笔预留就一直冻着（见 Result.Events 的注释）。
//
// 返回错误只发生在**上游接受请求之前**（没有可用渠道、连不上、非 2xx）。
// 那时还没有任何内容发给客户端，调用方可以照常回一个错误响应。一旦返回
// 了事件流，之后的一切失败都只能作为流里的事件表达。
func (g *Gateway) ChatStream(ctx context.Context, req pipeline.Request, upstream *channel.Request) (*Result, error) {
	if g.Executor == nil {
		return nil, ErrNoExecutor
	}

	h, err := g.begin(ctx, req, upstream)
	if err != nil {
		return nil, err
	}

	routed, err := g.Router.ChatStream(ctx, g.Executor, upstream, h.candidates, req.CanonicalModel)

	// 换渠道过程中失败的那些尝试照记——无论这次路由最后成没成，
	// 那些调用都已经发生过。成功接受的那一个不在这里记：它的结局只有
	// 读完流才知道（见 recordFailedAttempts）。
	g.recordFailedAttempts(ctx, streamAttempts(routed))

	if err != nil {
		// 上游在接受请求之前就失败了：路由层已经换过渠道，剩下的处置与
		// 非流式完全一样——不计费。
		//
		// settleAndFail 只结算、不释放，非流式那条路靠 defer res.finish
		// 兜住释放那一步；这里没有 defer 可挂（成功路径的收尾在后台），
		// 所以显式补一次 finish。
		_, cause := h.settleAndFail(ctx, err, pipeline.Outcome{UpstreamFailed: true})
		h.finish(ctx)
		return nil, cause
	}

	out := make(chan channel.StreamEvent)
	res := &Result{
		Events:   out,
		Channel:  routed.Channel,
		Attempts: routed.Attempts,
	}
	go g.pumpStream(ctx, h, res, out, routed.Events, upstream)
	return res, nil
}

// pumpStream 把上游的事件流转发给调用方，并在流结束时结算。
//
// 它是流式路径上**唯一**的收尾点，地位与非流式的 defer res.finish 相同：
// 每一个结局都在这里落地，没有第二条返回路径。
//
// out 单独传而不是用 res.Events：那个字段是只收的（<-chan），写不了也关不了，
// 而这里要做的正是这两件事。
func (g *Gateway) pumpStream(ctx context.Context, h *handle, res *Result, out chan channel.StreamEvent, events <-chan channel.StreamEvent, upstream *channel.Request) {
	// 关流放在最后：它一旦执行，调用方就会读到 Charge / Estimated，
	// 而那两个字段在下面才写好。
	defer close(out)

	// 结算与释放都用**不带取消**的 ctx：最需要计费的那一刻，正是客户端
	// 断开、请求 ctx 已经 Done 的那一刻（理由与 handle.release 相同）。
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), streamSettleTimeout)
	defer cancel()

	est := tokens.NewStreamEstimator(g.Counter, upstream.Model)
	// 估算器的方法不收 ctx，下面三处也不给它传一个——这是有意的，不是漏了：
	// 它们全是纯内存累加，唯一的 I/O 是词表加载，而那发生在 Counter 构造与
	// Prepare 阶段，不在这条路径上（见 tokens.StreamEstimator 的注释）。
	//
	// 硬塞一个 ctx 反而有害：这里手上只有请求 ctx，而它恰恰是**客户端断开时
	// 被取消**的那个，让估算可取消就等于「断开时少记一笔账」——方向正好反了。
	//
	// 输入侧在这里一次性算好。拼的是**整个请求的正文**，只传最后一条消息
	// 是这一层最常见的错误——一个 agent 循环里输入是输出的几十倍。
	//nolint:contextcheck // 估算不碰 ctx，理由见上
	est.ObserveRequest(upstream.PromptText(), len(upstream.Messages), len(upstream.Tools))

	var (
		// measured 是上游在流里带过的实测用量。
		//
		// 这里**不用 est.ObserveMeasured**：那个入口收的是一个总数（整次
		// 请求的 token 量），而流里的 usage 分着输入与输出两个数——计费对
		// 两者用不同的单价，压成一个总数就再也拆不回来，账会算错。
		measured *channel.Usage
		// started 表示已经向客户端吐过内容。它决定中途失败是「换个渠道
		// 重来」还是「如实告诉客户端被截断」。
		//
		// 判据（delta 与 tool_use 都算「已经吐过字」）**只有这一处**：
		// 本文件是唯一消费路由事件流的地方，而它本来就在这个 for/switch 里
		// 做别的事（累加估算、记 measured、判 done），判据留在这里不需要
		// 多一层通道与 goroutine。
		//
		// 将来若出现第二个消费点，要抽的是**一个共享的谓词函数**
		// （「这个事件算不算已经吐字」），不是包一层通道——包一层只是把
		// 「每个调用点都数一遍」换成「每个调用点都包一层」，
		// 漏一处的后果一模一样。
		started bool
		// done 表示上游给了结束标记（[DONE] / message_stop）。
		done bool
		// streamErr 是上游在流里报的错误。它决定这次调用记成渠道的失败
		// 还是成功——上游接受请求之后中途断了，是渠道此刻不行。
		streamErr error
		// clientGone 表示调用方已经断开。
		clientGone bool
	)

	for ev := range events {
		switch ev.Kind {
		case "delta":
			started = true
			//nolint:contextcheck // 估算不碰 ctx，理由见本函数开头
			est.ObserveDelta(ev.Text)
		case "tool_use":
			started = true
			if ev.ToolCall != nil {
				//nolint:contextcheck // 估算不碰 ctx，理由见本函数开头
				est.ObserveToolCall(ev.ToolCall.ID, ev.ToolCall.Name, ev.ToolCall.Arguments)
			}
		case "usage":
			if ev.Usage != nil {
				u := *ev.Usage
				measured = &u
			}
		case "done":
			done = true
			est.ObserveDone()
		case "error":
			if ev.Err != nil {
				streamErr = ev.Err
			}
			if started && ev.Err != nil {
				// 已经吐字之后失败：**不能换渠道**——重试会让客户端看到
				// 两段拼接的回答，而它无法察觉。把这个判断做成一个错误值
				// 交给调用方，它据此告诉客户端「回答被截断了」，而不是
				// 「服务暂时不可用，请重试」。
				//
				// ev 是值拷贝，改写它只影响下面转发的这一份。
				ev.Err = fmt.Errorf("%w: %w", route.ErrOutputStarted, ev.Err)
			}
		}

		if !forward(ctx, out, ev) {
			clientGone = true
			break
		}
	}

	// 客户端断开有两种表现：转发时被 ctx.Done 打断（上面那个 select），
	// 或者我们正等上游的下一个事件时那条流自己结束了——上游看到我们这边
	// 的 ctx 取消，于是收工。两种都是「客户端断开」，都要按要计费处理。
	//
	// done 之后不再看 ctx：流正常结束之后调用方才断开，那是正常结束。
	clientGone = clientGone || (!done && ctx.Err() != nil)

	if clientGone {
		// 我们不再转发，但上游那条流可能还没读完：适配器的泵阻塞在发送上，
		// 不读空它就永远不退出，它持有的上游连接也不会被释放。这里在后台
		// 读掉——上游的 ctx 就是被取消的这个，所以它很快就会结束。
		go func() {
			for range events {
			}
		}()
	}

	// 这次调用在渠道健康度上算成什么，三种结局的判定只有这里做得出来：
	//
	//   - 客户端断开不是渠道的失败（是我们这边断的），但它也没有正常结束，
	//     所以既不加连续失败也不清零
	//   - 上游接受请求之后报错、或者没吐字就断了，是渠道此刻不行
	//   - 正常结束（收到结束标记且流里没报错）才是成功
	verdict := route.VerdictFailure
	switch {
	case clientGone:
		verdict = route.VerdictUnattributed
	case done && streamErr == nil:
		verdict = route.VerdictSuccess
	}

	if !clientGone && !done && !started {
		// 上游在接受请求之后、吐出任何内容之前就结束了（报错或直接断开）。
		// 这等于「没有成功返回」：不计费，预留由 finish 释放。
		//
		// 注意这与上面那条分支的差别：客户端断开时，上游很可能已经为
		// 输入计了费，所以那条要计费，这条不用。
		//
		// 没有用量可记（一个字都没产生），所以只记结论。
		g.recordCall(ctx, route.CallOutcome{
			ChannelID: res.Channel.ID,
			At:        g.now(),
			Verdict:   verdict,
		})
		_, _ = h.settleAndFail(settleCtx, errStreamAborted,
			pipeline.Outcome{UpstreamFailed: true})
		h.finish(settleCtx)
		return
	}

	// 其余结局都要计费：正常结束、客户端中途断开、以及已经吐字之后
	// 上游失败或截断——那几种情况下上游都真的产生了 token。
	usage, source := streamUsage(est, measured)
	// Estimated 与 tokens.Source 是同一个口径（见 Result.Estimated）。
	res.Estimated = source != tokens.SourceMeasured
	g.billStream(settleCtx, h, res, usage, source, clientGone)

	// 用量照记，即便这次算「不是渠道的失败」：客户端断开时上游已经为
	// 输入计了费，额度自记账要跟着扣。
	g.recordCall(ctx, route.CallOutcome{
		ChannelID: res.Channel.ID,
		At:        g.now(),
		Verdict:   verdict,
		Usage:     usage,
	})
}

// recordFailedAttempts 记下换渠道过程中失败的那些尝试。
//
// # 成功的那一次为什么不在这里记
//
// 流式那条路上「上游接受了请求」不等于成功：它之后还可能中途断掉。
// 真正的结局只有读完流才知道，所以那一次由 pumpStream 在流结束时记。
// 在这里把它一起记了的话，同一个渠道每次流式请求会被记两遍——
// 一遍按「接受请求」算成功、一遍按真实结局算，成功率与连续失败都会偏。
//
// 与 recordAttempts 分开而不是加一个开关：两边的规则本来就不同，
// 一个记全部（非流式的成败在调用返回时就定了），一个只记失败的。
func (g *Gateway) recordFailedAttempts(ctx context.Context, attempts []route.Attempt) {
	if g.Recorder == nil {
		return
	}
	for _, a := range attempts {
		if a.Err == nil {
			continue
		}
		g.recordCall(ctx, route.CallOutcome{
			ChannelID: a.ChannelID,
			At:        attemptTime(a, g.now()),
			Verdict:   verdictOf(a.Err),
		})
	}
}

// streamAttempts 取一次流式路由结果的尝试列表。
//
// 与 attemptsOf 分开是因为返回类型不同（StreamResult 与 Result），
// 而两边都要处理「路由失败时结果可能为 nil」这一件事。
func streamAttempts(r *route.StreamResult) []route.Attempt {
	if r == nil {
		return nil
	}
	return r.Attempts
}

// billStream 结算一次要计费的流式请求。
func (g *Gateway) billStream(ctx context.Context, h *handle, res *Result, usage channel.Usage, source tokens.Source, canceled bool) {
	charge, err := g.finishCharge(ctx, h, res.Channel, pipeline.Outcome{
		Usage:       usage,
		UsageSource: source,
		// ClientCanceled 只描述「客户端中途断开」这一种结局，别拿它当
		// 「用量是估的」用：上游中途报错时用量同样是估的，但那不是客户端
		// 断的——两者在对账与告警上是两回事（见 pipeline.Outcome 的注释）。
		ClientCanceled: canceled,
	})
	if err != nil {
		// 结算落库失败：账算出来了但没记下，而钱是真金白银。调用方那边流
		// 已经结束（收尾在关流之前），所以这条错误只能留痕，交不回去。
		// 钱由下面的 finish 释放——与非流式那条「结算落库失败就释放」的
		// 规则一致（见 finishCharge 的注释）。
		observe.FromContext(ctx).Error("流式结算落库失败",
			"err", err,
			"request_id", h.req.RequestID,
			"channel_id", res.Channel.ID)
	} else {
		res.Charge = charge
	}
	// 与 Chat 的 defer res.finish 同一件事：已经结算过的会跳过再结算，
	// 释放总是走一遍（结算成功时被 charged 挡下，失败时真的放开）。
	h.finish(ctx)
}

// forward 把一个事件交给调用方。返回 false 表示调用方已经断开，不必再转发。
//
// ctx.Done 那一路是必须的：调用方断开之后没人再读这个通道，只靠
// `out <- ev` 会永远阻塞在这个 goroutine 里，而它一旦不往下走，结算与
// 释放就永远不会发生——**那笔钱会一直冻着**。
//
// 两路同时就绪时 select 随机取一路，所以断开之后还可能再发出去一两个
// 事件。那没有害处：多转发的分片只是没人收，而少转发一次结算就会漏。
func forward(ctx context.Context, out chan<- channel.StreamEvent, ev channel.StreamEvent) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// streamUsage 给出这次流式请求的用量与它的来源。
//
// 上游在流里带了实测 usage 时以它为准；没带时（客户端断开、上游中途报错、
// 或上游正常结束却没给 usage）用本地估算。估算的来源由 tokens 那一侧给出
// （词表精确计数或启发式），本函数不自己判断——来源是计费口径的一部分，
// 只该有一个产生点。
func streamUsage(est *tokens.StreamEstimator, measured *channel.Usage) (channel.Usage, tokens.Source) {
	if measured != nil {
		return *measured, tokens.SourceMeasured
	}
	r := est.Result()
	// 估算没有缓存的概念，所以输入直接放进 PromptTokens：本项目的
	// PromptTokens 含缓存部分（OpenAI 口径，见 channel.Usage 的注释），
	// 没有缓存时两者本来就是一个数。
	return channel.Usage{
		PromptTokens:     r.Input.Tokens,
		CompletionTokens: r.Output.Tokens,
	}, r.Total().Source
}

// errStreamAborted 表示上游在接受请求之后、吐出任何内容之前就结束了。
//
// 它不交给调用方（那时响应头可能已经发出去了），只用于结算：
// 「一个字都没产生」与「上游根本没成功返回」在计费上是同一件事。
var errStreamAborted = errors.New("上游在流式响应开始输出之前结束")

// streamSettleTimeout 是流式收尾（结算 + 释放）的时间上限。
//
// 与 releaseTimeout 分开：那个只管一次按主键定位的单行 UPDATE，这个还要
// 匹配规则、构造分录、写账本，是一次事务。取 10 秒的依据与它相同——
// 连事务都提交不进去时，说明库已经不接受写入了，放宽上限也救不回来，
// 而这一步的全部意义是不许挂住。
const streamSettleTimeout = 10 * time.Second
