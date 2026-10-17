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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
//
// hooks 是这次请求的插件挂载点，零值表示不跑插件（见 Hooks）。
//
// onRoute 在这条路上与在 Chat 上一样跑：**选渠道这件事流式与非流式是同一件**
// ——候选集、筛选与排序都在 route 里，与响应怎么发出去无关。少了它，
// 一个「把这个模型的流量钉到某个渠道」的插件会对流式请求静默不生效，
// 而那正是接了一半最难发现的样子。
//
// 这条路上**独有**的是 onStream（逐块决定）。onUsage 两条路都有，位置也相同
// （用量成形之后、结算之前）。onResponse 则相反，它只可能发生在非流式那条路上：
// 这里的响应是边生成边发的，没有「整份响应」这个时刻。
func (g *Gateway) ChatStream(ctx context.Context, req pipeline.Request, upstream *channel.Request, hooks Hooks) (*Result, error) {
	if g.Executor == nil {
		return nil, ErrNoExecutor
	}

	h, err := g.begin(ctx, req)
	if err != nil {
		return nil, err
	}

	// 请求快照在**调上游之前**采集，与非流式那条路同一个位置与理由
	// （见 begin 的注释）。
	g.snapshotAsync(ctx, upstream, "request", req.UserID)

	// 与 Chat 同一个位置、同一个理由：选路一返回就拿到「打到了哪儿」
	// （见 placement）。这里尤其不能省——成功那条路的收尾在后台的
	// pumpStream 里，而中途断流那一次走的是释放，两条路都要收这个值。
	routed, placed, err := g.routeStream(ctx, upstream, h.candidates, req.CanonicalModel, hooks)

	// 换渠道过程中失败的那些尝试照记——无论这次路由最后成没成，
	// 那些调用都已经发生过。成功接受的那一个不在这里记：它的结局只有
	// 读完流才知道（见 recordFailedAttempts）。
	g.recordFailedAttempts(ctx, streamAttempts(routed))

	if err != nil {
		// 上游在接受请求之前就失败了：路由层已经换过渠道，剩下的处置与
		// 非流式完全一样——不计费。
		//
		// 响应方向的快照在这里采：上游有过调用（或至少有过一次尝试），
		// 只是没拿到可用的响应。失败同样要留一条，那正是排查时最该看的。
		respBody, respStatus, respKind := responseEvidence(nil, err)
		g.responseSnapshotAsync(ctx, respBody, req.UserID, respStatus, respKind)

		// settleAndFail 只结算、不释放，非流式那条路靠 defer res.finish
		// 兜住释放那一步；这里没有 defer 可挂（成功路径的收尾在后台），
		// 所以显式补一次 finish。
		cause := h.settleAndFail(ctx, placed, err, pipeline.Outcome{UpstreamFailed: true})
		h.finish(ctx, placed)
		return nil, cause
	}

	out := make(chan channel.StreamEvent)
	res := &Result{
		Events:        out,
		Channel:       routed.Channel,
		UpstreamModel: routed.UpstreamModel,
		Attempts:      routed.Attempts,
	}
	go g.pumpStream(ctx, h, placed, res, out, routed.Events, upstream, hooks)
	return res, nil
}

// routeStream 选路并执行一次流式请求。顺序与 routeChat 完全相同，
// 差别只在执行器是流式的那个（见 routeChat），以及结果的类型。
//
// 它同样同时交回「打到了哪儿」（placement）：流式那条路的收尾在后台的
// pumpStream 里，这个值要一路带过去（见 placement）。
//
// 「已经吐字之后不能换渠道」那条边界不受钉首渠道影响：它由
// route.chatStreamWithPlan 保证，钉住的渠道排在第一位也一样受它约束。
func (g *Gateway) routeStream(ctx context.Context, upstream *channel.Request, candidates []route.Candidate, canonicalModel string, hooks Hooks) (*route.StreamResult, placement, error) {
	if hooks.OnRoute == nil {
		res, err := g.Router.ChatStream(ctx, g.Executor, upstream, candidates, canonicalModel)
		return res, streamPlacementOf(res), err
	}
	// 与 routeChat 同一处、同一个理由：这条路上本层自己筛。
	plan := g.Router.Select(candidates, canonicalModel, route.NeedsOf(upstream))
	pin, err := hooks.OnRoute(ctx, plan.Candidates)
	if err != nil {
		return nil, placement{}, err
	}
	res, err := g.Router.ChatStreamPlan(ctx, g.Executor, upstream, plan, pin)
	return res, streamPlacementOf(res), err
}

// StreamChunk 是逐块决策里交给插件的那一块的上下文。
//
// 它与 internal/plugin 的 StreamInput 是同一个形状的两处声明，理由与 Hooks
// 相同：本包不认识插件包，只声明「它要调什么」，装配处把两边对上。
//
// 两个累积量说的都是**这一块之前**的数，不含当前块——当前块在 ev 里，
// 而它是不是要算进去，取决于插件下面返回什么（见 Hooks.OnStream）。
type StreamChunk struct {
	// Index 是这一块在本次流里的序号，从 0 起，只数**内容块**。
	//
	// 数内容块而不是数全部事件：插件看到的就是这些块，序号里混进它看不见的
	// 事件（usage / done）会让「第几块」变成一个它无从验证的数。
	Index int
	// AccumulatedText 是已经发给调用方的正文，也就是分词器已经数过的那些。
	AccumulatedText string
	// AccumulatedTokensApprox 是上面那段正文的 token 数，由内核按分词器
	// 增量估算。
	AccumulatedTokensApprox int64
}

// StreamDecision 是插件对一块的处置。
//
// 零值是「什么都不做」：这一块原样转发。
type StreamDecision struct {
	// Drop 表示这一块不发给调用方，也不算进累计正文与分词计数。
	Drop bool
	// Replace 是替换后的正文。只对正文块（delta）有效；工具调用块上没有
	// 可作用的对象，按无效果处理（见 plugin.StreamDecision 的注释）。
	Replace *string
	// Terminate 表示提前结束这条流。已经发出去的部分收不回来，所以它与
	// 「客户端断开」走同一条收尾路径（见 pumpStream 的结局判定）。
	Terminate bool
}

// isContentChunk 判断一块是不是「内容块」——只有内容块交给插件。
//
// 它同时是**权限边界**：usage 是这次流的计费依据（插件丢掉它就能绕过
// onUsage 的上界，计费退回内核估算），done / error 是这次流的框架（丢掉
// done 会让一次正常结束看起来像被截断，还会把渠道健康度判成失败）。
// 这三样因此根本不进插件的手。
//
// 判据写成白名单而不是「不是 usage/done/error 就算内容」：将来事件类型
// 变多时，新类型默认**不给**插件，而不是默认给——能改的比能看的少。
//
// 引用标注（annotation）也在白名单之外：插件今天能改的只有正文与工具参数
// （见 StreamDecision），标注既没有可作用的对象、又描述「正文在哪引用了
// 什么」，交给它只会让「插件看到的块」与「它改得动的东西」之间多出一样
// 改不动的。它照旧转发给调用方，只是不经过插件。
func isContentChunk(ev channel.StreamEvent) bool {
	switch ev.Kind {
	case "delta", "tool_use":
		return true
	}
	return false
}

// pumpStream 把上游的事件流转发给调用方，并在流结束时结算。
//
// 它是流式路径上**唯一**的收尾点，地位与非流式的 defer res.finish 相同：
// 每一个结局都在这里落地，没有第二条返回路径。
//
// out 单独传而不是用 res.Events：那个字段是只收的（<-chan），写不了也关不了，
// 而这里要做的正是这两件事。
func (g *Gateway) pumpStream(ctx context.Context, h *handle, placed placement, res *Result, out chan channel.StreamEvent, events <-chan channel.StreamEvent, upstream *channel.Request, hooks Hooks) {
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
		// 它由本文件持有而不是交给 `tokens.StreamEstimator`：那个累加器
		// 的结论是一个总数（Count），而流里的 usage 分着输入与输出、还带
		// 缓存与推理的明细——计费对它们用不同的单价，压成一个总数就再也
		// 拆不回来，账会算错。所以「实测优先」这条判据落在 streamUsage 上，
		// 估算器只负责「拿不到实测值」那一半。
		measured *channel.Usage
		// started 表示已经向客户端吐过内容。它决定中途失败是「换个渠道
		// 重来」还是「如实告诉客户端被截断」。
		//
		// 判据是「这条事件让客户端看见了东西」，参与者是 delta、tool_use
		// 与 annotation 三种 Kind（各自的 case 里置真）。它就落在下面这个
		// for/switch 里，**只有这一处**：本文件是唯一消费路由事件流的地方，
		// 而它本来就在这里做别的事（累加估算、记 measured、判 done），
		// 判据留在这里不需要多一层通道与 goroutine。
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
		// upstreamStatus 是这条流对应的上游 HTTP 状态码，取自适配器发出的
		// 第一条 start 事件（见 channel.StreamEvent.StatusCode）。
		//
		// 它只用于一种结局：上游接受了请求、却一个字都没吐就结束。那时
		// 事件流里可能一个 error 都没有，状态码只在这条事件里出现过；
		// 账单上那一列要照实记下它（上游确实回了一个 2xx）。
		upstreamStatus int
		// clientGone 表示调用方已经断开。
		clientGone bool
		// terminated 表示插件要求提前结束这条流（onStream 返回 terminate，
		// 或它作为阻断型插件在那里失败）。
		//
		// 它与 clientGone 分开记，因为两者在**一个**字段上有差别：
		// pipeline.Outcome.ClientCanceled 说的是「客户端断的」，而插件终止
		// 不是客户端断的。其余一切（结局判定、计费分支、读空上游）两者同路
		// ——见下面那一段。
		terminated bool
		// chunkIndex 是交给插件的块序号，只数内容块（见 StreamChunk）。
		chunkIndex int
		// accText 是已经发给调用方的正文。
		//
		// 它按**插件处置之后**的样子累加：drop 掉的块不进来，replace 过的
		// 块进来的是替换后的文本。累加器（est）与它是同一个口径，所以两者
		// 只能在同一处更新——错开就会出现「按原文计费、按替换后的文本发给
		// 调用方」这种账单与回答对不上的状态。
		accText string
		// upstreamResp 把这条流归一回整份响应，供响应方向的快照使用。
		//
		// 它与 accText 的口径相反：观察发生在插件的处置**之前**，记的是
		// 上游回来的那份内容。插件改的是发给调用方的东西，不是上游的事实。
		upstreamResp = newStreamResponseBuilder()
	)

	// 开始泵流的时刻。它只是延迟的**兜底起点**：真正发出去的时刻取自
	// 最后一次尝试的 StartedAt（见 streamDuration），这里兜的是脚本执行器
	// 那种不填时间的形状。
	start := g.now()

	// 响应方向的快照在流结束时采一次。
	//
	// 用 defer 而不是写在循环之后：本函数有两条返回路径（「一个字没吐」与
	// 「计费」），只挂在其中一条上就会漏掉另一半——那正是这张表想要的
	// 「上游出过什么事」。
	defer func() {
		status, kind := streamResponseEvidence(upstreamStatus, streamErr, started, done, clientGone || terminated)
		g.responseSnapshotAsync(ctx, upstreamResp.response(res.UpstreamModel), h.req.UserID,
			optionalInt(status), optionalString(kind))
	}()

	for ev := range events {
		// 归一看**插件事先**的事件：快照记的是上游回来的内容（见 upstreamResp）。
		upstreamResp.observe(ev)

		// onStream 排在**累加之前**，而且是这一块被决定之后才累加：插件
		// 替换掉的正文是这次流式中断时的计费依据（见 StreamChunk），
		// 排在 est.ObserveDelta 之后就变成按原文计费了。
		if hooks.OnStream != nil && isContentChunk(ev) {
			dec, hookErr := hooks.OnStream(ctx, ev, StreamChunk{
				Index:                   chunkIndex,
				AccumulatedText:         accText,
				AccumulatedTokensApprox: est.Result().Output.Tokens,
			})
			chunkIndex++
			if hookErr != nil {
				// 阻断型插件失败：这条流到此为止。已经吐给调用方的部分收不
				// 回来，所以「让这次请求失败」在这里没有可表达的对象——
				// 终止是唯一做得到的处置，而它走的是下面那条**共同的**收尾
				// 路径（结算照走），不是在这里另开一个出口。
				//
				// 非阻断的失败不会走到这里：那一侧网关拿到的是零值决策，
				// 这一块原样转发（见 internal/api 的 pluginHooks）。
				terminated = true
				break
			}
			if dec.Terminate {
				terminated = true
				break
			}
			if dec.Drop {
				// 丢掉的那一块连累计也不进：计费依据是调用方实际看到的
				// 正文，而这一块他没看到（见 StreamChunk 与契约里的
				// StreamDecision）。
				continue
			}
			if dec.Replace != nil {
				ev.Text = *dec.Replace
			}
		}

		switch ev.Kind {
		case "start":
			// 这条流的信封信息，不是正文：记下状态码，不转发给调用方
			// （调用方看到的 SSE 里没有这一帧）。
			upstreamStatus = ev.StatusCode
			continue
		case "delta":
			started = true
			//nolint:contextcheck // 估算不碰 ctx，理由见本函数开头
			est.ObserveDelta(ev.Text)
			accText += ev.Text
		case "annotation":
			started = true
			// 引用标注。它不是正文（不参与估算，也不进计费口径），但对调用方
			// 是**内容**：客户端已经看见了它，就无法再假装这次调用什么都没
			// 发生。所以按「已经吐过字」处置——那正是上面 started 的判据所要
			// 回答的问题（能否换渠道重试）。
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
		case "usage_input":
			// 输入侧的用量先到了。**这里什么都不做，是有意的**：它只有输入、
			// 没有输出，当成实测用量会把输出算成 0，而这条流很可能还在正常
			// 生成。结算等上面那个完整的 "usage"（见 channel.StreamEvent）。
			//
			// 写成一个显式的空分支而不是让它落到 switch 外面：这个 switch 是
			// 计费口径唯一的落点，读者要能一眼看出这个新事件**被看过了**。
		case "done":
			done = true
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

	// ended 表示这次流不是正常走完的，但也不是上游的错：客户端断开、或者
	// 插件要求提前结束。两者在**结局判定、计费分支、读空上游**这三件事上
	// 完全同路，所以下面只有这一个判据；差别只有 ClientCanceled 那一个字段
	// （它说的是「客户端断的」，而插件终止不是——见 pipeline.Outcome）。
	//
	// 插件终止之所以要并进这里而不是自己 break 出去，正是下面那两段决定的：
	// 它必须走同一条结算路径。单独一个 break 会让那条路绕开结算，表现是
	// 「已经吐出去的回答一分钱没记」。
	ended := clientGone || terminated

	if ended {
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
	//     所以既不加连续失败也不清零；插件终止同理——那是插件按自己的策略
	//     截的，与这个渠道好不好无关
	//   - 上游接受请求之后报错、或者没吐字就断了，是渠道此刻不行
	//   - 正常结束（收到结束标记且流里没报错）才是成功
	verdict := route.VerdictFailure
	switch {
	case ended:
		verdict = route.VerdictUnattributed
	case done && streamErr == nil:
		verdict = route.VerdictSuccess
	}

	if !ended && !done && !started {
		// 上游在接受请求之后、吐出任何内容之前就结束了（报错或直接断开）。
		// 这等于「没有成功返回」：不计费，预留由 finish 释放。
		//
		// 注意这与上面那条分支的差别：客户端断开时，上游很可能已经为
		// 输入计了费，所以那条要计费，这条不用。插件终止也一样——它是我们
		// 这边主动停的，而上游已经读了 prompt。
		//
		// 没有用量可记（一个字都没产生），所以只记结论。
		g.recordCall(ctx, route.CallOutcome{
			ChannelID: res.Channel.ID,
			At:        g.now(),
			Verdict:   verdict,
		})
		_ = h.settleAndFail(settleCtx, placed, &streamAbortedError{status: upstreamStatus},
			pipeline.Outcome{UpstreamFailed: true})
		h.finish(settleCtx, placed)
		return
	}

	// 其余结局都要计费：正常结束、客户端中途断开、插件提前终止，以及已经
	// 吐字之后上游失败或截断——那几种情况下上游都真的产生了 token。
	//
	// coreUsage 是内核算出来的那份，usage 是**结算**用的那份：两者只在
	// 插件上报过用量时不同（见下面 onUsage 那一段）。
	coreUsage, source := streamUsage(est, measured)
	usage := coreUsage
	// Estimated 与 tokens.Source 是同一个口径（见 Result.Estimated）。
	res.Estimated = source != tokens.SourceMeasured

	// onUsage 排在用量成形之后、结算之前：它改的是结算读的那份用量。
	// 排在结算之后就一点用都没有——请求成功、响应正确、日志里插件也跑了，
	// 只有那笔钱是按内核的估算收的。
	if hooks.OnUsage != nil {
		merged, hookErr := hooks.OnUsage(settleCtx, usage)
		if hookErr != nil {
			// 阻断型插件失败。这里**不改变控制流**：整个回答已经送到调用方
			// 手里了，没有「让这次请求失败」这个对象；而按不计费结算等于把
			// 一次已经送出去的回答白送（上游那笔钱是真的花掉了）。
			//
			// 失败本身已经记进会话，入口在请求结束时把它记成告警
			// （见 internal/api 的 logPluginFailures），所以它不是静默的。
			//
			// 出错时交回来的用量**整个忽略**，下面仍按内核那份结算：让一个
			// 出错的 hook 决定金额，方向正好反了——它多半交回来的就是零值。
			observe.FromContext(settleCtx).Warn("流式请求的 onUsage 失败，用量按内核的值计",
				"err", hookErr, "request_id", h.req.RequestID)
		} else {
			usage = merged
		}
	}

	// 这次调用花了多久。结算要用它（落进 usage_events.duration_ms），
	// 下面那次健康度记录也要用它——两处必须同一个数，否则桶里聚合出来的
	// 分位数与账本上那一行说的不是同一件事。
	elapsed := streamDuration(res.Attempts, start, g.now())

	g.billStream(settleCtx, h, placed, res, usage, source, clientGone, elapsed)

	// 用量照记，即便这次算「不是渠道的失败」：客户端断开时上游已经为
	// 输入计了费，额度自记账要跟着扣。
	//
	// 记的是**内核那份**（usage 上面可能已被插件改过）：这一路记的是这个
	// 渠道自己消耗了什么，而插件上报的是计费口径上的说法——与非流式那条
	// 路上 recordAttempts 取 out.Usage（插件跑之前）是同一个口径。
	//
	// 位置在 billStream **之后**：延迟那一维的样本来自 usage_events，
	// 而那一行是上面那次结算写的（见 route.CallOutcome 与 channels 的
	// rollupSignalsSQL）。
	g.recordCall(ctx, route.CallOutcome{
		ChannelID: res.Channel.ID,
		At:        g.now(),
		Verdict:   verdict,
		Usage:     coreUsage,
	})
}

// streamDuration 是一次流式调用的耗时。
//
// 起点取最后一次尝试的 StartedAt（请求真正发出去的那一刻）；它为零时
// （测试里的脚本执行器不会填）回落到开始泵流的时刻。终点是流结束。
//
// 终点早于起点时返回 0（写进库里是 NULL）：那意味着时钟回拨或两个钟
// 不是同一个来源，编一个正数出来只会让分位数里混进一个假样本。
func streamDuration(attempts []route.Attempt, fallbackStart, end time.Time) time.Duration {
	start := fallbackStart
	if n := len(attempts); n > 0 && !attempts[n-1].StartedAt.IsZero() {
		start = attempts[n-1].StartedAt
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
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
//
// elapsed 是这次调用从发出去到读完那条流的时间，落进 usage_events.duration_ms
// ——渠道健康度延迟那一维的唯一样本来源（见 pipeline.Outcome.Duration）。
func (g *Gateway) billStream(ctx context.Context, h *handle, placed placement, res *Result, usage channel.Usage, source tokens.Source, canceled bool, elapsed time.Duration) {
	charge, err := g.finishCharge(ctx, h, placed, pipeline.Outcome{
		Usage:       usage,
		UsageSource: source,
		Duration:    elapsed,
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
	h.finish(ctx, placed)
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

// streamAbortedError 给 errStreamAborted 带上上游那次响应的状态码。
//
// 适配器在流的第一条事件里交下了那个 2xx（见 channel.StreamEvent.StatusCode），
// 而这次请求**确实拿到过响应**——「拿到过响应」是 ErrUpstream.GotResponse
// 表达的那件事，账单上那一列要照实记下来。这次的结局由 error_code
// （upstream_stream_aborted）表达，两列各司其职。
//
// status 为 0 表示这条流没有 start 事件（例如测试里的脚本执行器）：
// 那时编不出状态码，与 errStreamAborted 单独出现时一样留空。
type streamAbortedError struct {
	status int
}

func (e *streamAbortedError) Error() string { return errStreamAborted.Error() }

// Unwrap 让 errors.Is(err, errStreamAborted) 继续成立：所有把这次结局
// 认成「流在接受之后断了」的调用方不必知道有没有状态码。
func (e *streamAbortedError) Unwrap() error { return errStreamAborted }

// streamResponseBuilder 把一条已消费的事件流归一回 channel.Response。
//
// # 为什么在网关里做
//
// 适配器已经把上游的协议形状翻译成了 channel.StreamEvent——那是本项目认的
// 唯一形状。这里只是把同一份事件重新拼成「整份响应」那个形状，供响应方向的
// 快照使用；它不引入第三种协议表示，也不改任何一条事件的语义。
//
// # 它是近似，近似在哪里
//
// 事件流里没有块边界：文本增量与工具增量按到达顺序给出。这里按到达顺序
// 聚块——连续文本归一个 text 块，同一个 Index 的工具增量归一个 tool_use 块。
// 还原不出的是零长度的块（上游发了一个空的文本块），因而块的数量可能比
// 上游原样响应少。快照记的是形状与长度，这个差别不影响计费，也不影响
// 「这次回答长什么样」的判读。
//
// # 谁不在这里
//
// 插件的 drop / replace 不在这里：那些是发给调用方的处置，不是上游回来的
// 内容。所以观察必须发生在插件处理**之前**（见 pumpStream 的循环）。
type streamResponseBuilder struct {
	status       int
	finishReason string
	usage        channel.Usage
	measured     bool
	blocks       []channel.ContentBlock
	// toolAt 把增量里的工具序号映射到 blocks 里的下标。
	toolAt map[int]int
	// toolArgs 按增量序号累计参数分片。参数在流里是分片给的，
	// 拼齐之后才是一个 JSON 对象。
	toolArgs map[int]*strings.Builder
	// pendingAnnotations 是在第一段正文出现之前就到了的引用标注。
	//
	// 上游的先后不保证（标注可能先于它还解释的那段正文），而快照要记的是
	// 「上游说了什么」：为一批标注凭空造一个空正文块，会让骨架里多出一项
	// 并不存在的正文，所以先攒着，等正文块出现时再挂上去（见 observe）。
	pendingAnnotations []map[string]any
}

func newStreamResponseBuilder() *streamResponseBuilder {
	return &streamResponseBuilder{
		toolAt:   make(map[int]int),
		toolArgs: make(map[int]*strings.Builder),
	}
}

// observe 看一条事件。
func (b *streamResponseBuilder) observe(ev channel.StreamEvent) {
	switch ev.Kind {
	case "start":
		// 第一条事件的信封信息，不是正文（见 channel.StreamEvent.StatusCode）。
		b.status = ev.StatusCode
	case "delta":
		if ev.Text == "" {
			return
		}
		if n := len(b.blocks); n > 0 && b.blocks[n-1].Type == "text" {
			b.blocks[n-1].Text += ev.Text
			return
		}
		b.blocks = append(b.blocks, channel.ContentBlock{Type: "text", Text: ev.Text})
		b.flushPendingAnnotations()
	case "tool_use":
		if ev.ToolCall == nil {
			return
		}
		i, ok := b.toolAt[ev.ToolCall.Index]
		if !ok {
			i = len(b.blocks)
			b.toolAt[ev.ToolCall.Index] = i
			b.blocks = append(b.blocks, channel.ContentBlock{Type: "tool_use"})
			b.toolArgs[ev.ToolCall.Index] = &strings.Builder{}
		}
		blk := &b.blocks[i]
		// id 与 name 只在第一个分片里给出（见 channel.ToolCallDelta）。
		if ev.ToolCall.ID != "" {
			blk.ToolUseID = ev.ToolCall.ID
		}
		if ev.ToolCall.Name != "" {
			blk.ToolName = ev.ToolCall.Name
		}
		b.toolArgs[ev.ToolCall.Index].WriteString(ev.ToolCall.Arguments)
	case "annotation":
		// 引用标注。挂在最后那个正文块上：标注描述的是「正文的哪一段引用了
		// 哪个来源」，挂在工具调用上没有意义。这里只要求「最后块是 text」，
		// 是因为事件流里没有块边界——连续文本本来就聚成一个块，块之间只由
		// tool_use 分隔（见上方「它是近似」那一段）。
		//
		// 与非流式的 relay.attachAnnotations **不是同一处挂载点**：那个在
		// 事后遍历整份 ContentBlock，挂在**第一个**带文字的 text 块上。差别
		// 在本条流式的真实形状下看不出来（正文只有一个正文块），但它不是
		// 「同一个口径」，别照着一边去改另一边。
		//
		// 还没有正文块时先攒着，等第一段正文出现再挂上去：上游可能先给标注
		// 再给正文，而快照要记的是「上游说了什么」——为一批标注凭空造一个
		// 空的正文块，会让骨架里多出一项并不存在的正文。
		if len(ev.Annotations) == 0 {
			return
		}
		if n := len(b.blocks); n > 0 && b.blocks[n-1].Type == "text" {
			b.blocks[n-1].Annotations = append(b.blocks[n-1].Annotations, ev.Annotations...)
			return
		}
		b.pendingAnnotations = append(b.pendingAnnotations, ev.Annotations...)
	case "usage":
		if ev.Usage != nil {
			b.usage = *ev.Usage
			b.measured = true
		}
	case "done":
		b.finishReason = ev.FinishReason
	}
}

// flushPendingAnnotations 把先于正文到达的标注挂到刚建好的正文块上。
func (b *streamResponseBuilder) flushPendingAnnotations() {
	if len(b.pendingAnnotations) == 0 {
		return
	}
	// 挂到最后那个正文块上（没有就什么都不做）。
	//
	// 找最后一个而不是只看最后一个块：标注可能与工具调用块交错到达，
	// 而它解释的仍然是正文（见 observe 里 "annotation" 那一支）。
	for i := len(b.blocks) - 1; i >= 0; i-- {
		if b.blocks[i].Type == "text" {
			b.blocks[i].Annotations = append(b.blocks[i].Annotations, b.pendingAnnotations...)
			b.pendingAnnotations = nil
			return
		}
	}
}

// response 产出一份归一化响应；什么都没观察到时返回 nil。
//
// 返回 nil 的条件是「没有内容、没有实测用量、没有结束原因」——那说明这条流
// 在给出任何东西之前就结束了。返回一条字段全是零值的 Response 会让骨架里
// 出现 `usage: {prompt_tokens: 0, completion_tokens: 0}`，读起来像「上游报了
// 零用量」，而事实是上游什么都没报。空骨架配状态码与错误分类，才是那次
// 调用的忠实记录。
func (b *streamResponseBuilder) response(upstreamModel string) *channel.Response {
	if len(b.blocks) == 0 && !b.measured && b.finishReason == "" {
		return nil
	}
	// 收尾时还没等来正文的标注挂到最后那个正文块上；一个正文块都没有时
	// 就没有可挂的地方——为它造一个空正文块，骨架里会多出一项并不存在的正文。
	b.flushPendingAnnotations()
	for idx, buf := range b.toolArgs {
		blk := &b.blocks[b.toolAt[idx]]
		// 拼不成一个 JSON 对象就留空：参数可能因为断流只到了一半，
		// 而快照不猜内容（见 internal/snapshot 的铁律）。
		var input map[string]any
		if s := buf.String(); s != "" && json.Unmarshal([]byte(s), &input) == nil {
			blk.ToolInput = input
		}
	}
	return &channel.Response{
		StatusCode:    b.status,
		UpstreamModel: upstreamModel,
		// 响应一定是 assistant 的；事件流里没有角色字段，因为上游的
		// 流式分片从不带它（协议决定的）。
		Message:      channel.Message{Role: "assistant", Content: b.blocks},
		FinishReason: b.finishReason,
		Usage:        b.usage,
	}
}

// streamResponseEvidence 推出响应方向快照要记的两列。
//
// 判据与 pumpStream 里那两段结局判定同源，**顺序也一致**：「一个字没吐就
// 结束」先判，不看 streamErr 的类型。这一点是必须的——账单那一侧
// （pumpStream 的 settleAndFail）已经把「没吐字」归一成 upstream_stream_aborted
// 了，快照若先看 streamErr，就会把同一次失败记成 upstream_unusable_response
// （流里那条错误帧恰好带着 *channel.ErrUpstream）。
//
// 一个 token 都没吐之后的分类是「流在接受之后断了」；吐过字之后上游把连接
// 掐断（没有结束标记、也没有错误帧）是「流被截断」，见 streamTruncated。
// 其余（正常结束、客户端断开、插件终止）没有上游失败可归。
func streamResponseEvidence(status int, streamErr error, started, done, ended bool) (int, string) {
	if !ended && !done && !started {
		return status, pipeline.ErrorUpstreamStreamAborted
	}
	if streamTruncated(streamErr, started, done, ended) {
		return status, pipeline.ErrorUpstreamStreamTruncated
	}
	if streamErr != nil {
		s, kind := upstreamEvidence(streamErr)
		if s == 0 {
			// 适配器没带上状态码时退回 start 事件里的那个（见
			// channel.StreamEvent.StatusCode）。
			s = status
		}
		return s, kind
	}
	return status, ""
}

// streamTruncated 判断这次流是不是「吐过内容之后被上游掐断」。
//
// 四个条件缺一不可，而且它们都是**已经发生过的事实**，不是推断：
//
//   - started：至少吐过一个内容块（正文、工具调用或标注）。一个字没吐的
//     是另一种结局（upstream_stream_aborted），不计费；
//   - !done：上游没给结束标记（[DONE] / message_stop）。给了就是正常结束；
//   - streamErr == nil：上游没有报错。报错是另一条路（已经吐字之后报错要
//     计费，但结局是「上游的响应不可用」而不是「静默截断」）；
//   - !ended：不是我们这边断的。客户端断开或插件终止时，上游那条连接多半
//     也跟着断了，把它记成上游截断等于把我们的行为算到上游头上。
//
// 它同时是快照与账单两侧的判据：两表对同一次截断必须给出同一个值，所以
// 判据只能有一处，两边都调这一个函数，不各判一次。账单那一侧的落点是
// store.Release 之外的另一处（已计费的行不走 Release），见 pipeline 的
// ErrorUpstreamStreamTruncated。
func streamTruncated(streamErr error, started, done, ended bool) bool {
	return !ended && !done && started && streamErr == nil
}

// streamSettleTimeout 是流式收尾（结算 + 释放）的时间上限。
//
// 与 releaseTimeout 分开：那个只管一次按主键定位的单行 UPDATE，这个还要
// 匹配规则、构造分录、写账本，是一次事务。取 10 秒的依据与它相同——
// 连事务都提交不进去时，说明库已经不接受写入了，放宽上限也救不回来，
// 而这一步的全部意义是不许挂住。
const streamSettleTimeout = 10 * time.Second
