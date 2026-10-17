package gateway

import (
	"context"
	"fmt"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/systemone"
)

// DecideExecutor 是调上游决策接口的能力，由适配器组合而成。
//
// 声明在本包、实现在 internal/upstream：与 Executor 同一个做法，本包不认识
// 具体的适配器，只要求一个「给定渠道与请求，回一份响应」的能力。
type DecideExecutor interface {
	Decide(ctx context.Context, ch channel.Channel, req *systemone.Request) (*systemone.Response, error)
}

// DecideResult 是一次决策请求走完之后的产出。
//
// 与 Result 分成两个类型而不是给 Result 加字段：两者的 Response 类型不同
// （*channel.Response 与 *systemone.Response），合成一个会让「哪条路径填了
// 哪个字段」变成要读调用点才知道的事，而读错的表现是一个静默的 nil。
type DecideResult struct {
	// Response 是要交给调用方的那份决策响应（上游原样，未经过任何 chat 形状的投影）。
	Response *systemone.Response
	// Channel 是实际选中的渠道，UpstreamModel 是这次实际发出去的上游模型名。
	Channel       channel.Channel
	UpstreamModel string
	Charge        *pipeline.Charge
	Attempts      []route.Attempt
}

// Decide 走一次决策请求：预留 → 选路 → 调上游 → 结算。
//
// # 它与 Chat 的关系
//
// 顺序、失败分支与钱的处置**完全相同**，差别只有两处：上游调用收的是
// *systemone.Request（这条表面不是 chat 形状，见 internal/systemone 的包注释），
// 以及没有插件挂载点。
//
// 所以它复用 begin / finish / settleAndFail / finishCharge 这四个方法，
// 而不是自己再串一遍：那四个方法里放着「预留一定有上界」「每一个返回路径都
// 经过结算」「只有真的交回账单才解冻」这些不变量，抄一份出来就是两份会分叉的
// 实现（LESSONS.md 第 24 条）。
//
// # 为什么没有插件挂载点
//
// 插件契约里的 onRequest / onResponse 都作用在 channel.Request /
// channel.Response 上，而这条表面不共用那两个类型；onRoute 与 onUsage 虽然
// 与协议无关，但会话是按请求建的，只接一半会让「插件在这条表面上不生效」
// 变成一件要靠读代码才发现的事。这一轮**整条路径都不跑插件**，装配层不给
// 它建会话——这是有意的取舍，不是漏了。
//
// 后果要说清楚：一个「把这个模型的流量钉到某个渠道」的插件对决策请求不生效。
// 它不会报错，也不会在响应里留下痕迹。
func (g *Gateway) Decide(ctx context.Context, req pipeline.Request, decision *systemone.Request) (*DecideResult, error) {
	if g.Decider == nil {
		return nil, ErrNoExecutor
	}

	h, err := g.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	// 与 Chat 同一个位置、同一个理由：选路之前的兜底只管「预留一定被放开」，
	// 那时还没有「打到了哪儿」可记（见 handle.abort）。
	defer h.abort(ctx)

	// 这条路径**不采请求快照**：脱敏器的允许表按 channel.Request 的形状建
	// （见 internal/snapshot 的 allowlist.go），这条表面没有对应的根。
	// 快照是排障辅助、可以整条丢，所以缺了它不影响计费；接上它需要在
	// gateway 与 snapshotwire 之间再加一层按通用 map 脱敏的接口，那是另一片。

	call := &decideCall{exec: g.Decider, req: decision}
	routed, placed, err := g.routeDecide(ctx, call, req.CanonicalModel, h.candidates)
	// 与 Chat 同一个位置、同一个理由：选路一返回就拿到「打到了哪儿」，
	// 下面每一条收尾路径都要收它（见 placement）。它排在兜底之后注册，
	// 所以正常路径上先跑的是这一个。
	defer h.finish(ctx, placed)
	if err != nil {
		// 与 Chat 同一处置：健康度先记（失败的调用正是它要记的那些），
		// 再走一次结算得出「不计费」的结论，预留由 defer 的 finish 释放。
		g.recordAttempts(ctx, attemptsOf(routed), nil)
		return nil, h.settleAndFail(ctx, placed, err, pipeline.Outcome{UpstreamFailed: true})
	}

	out := pipeline.Outcome{
		Usage: usageOf(routed.Response),
		// 来源按「上游报没报 usage 块」判，与 chat 那条路同一个入口
		// （见 usageSourceOf）：systemone.Usage 的 Reported 由解析那一层填，
		// 再经 ChannelUsage 折进这里。一个不报 usage 的决策上游因此落成
		// Unreported，而不是「实测 0」。
		UsageSource: usageSourceOf(routed.Response),
	}
	g.recordAttempts(ctx, routed.Attempts, &out.Usage)

	charge, err := g.finishCharge(ctx, h, placed, out)
	if err != nil {
		return nil, err
	}

	return &DecideResult{
		Response:      call.response,
		Channel:       routed.Channel,
		UpstreamModel: routed.UpstreamModel,
		Charge:        charge,
		Attempts:      routed.Attempts,
	}, nil
}

// routeDecide 选路并执行一次决策请求。
//
// 它与 routeChat / routeStream 同一个形状：结果与「打到了哪儿」一起交回，
// 于是每条收尾路径拿到的 placement 都来自选路本身（见 placement）。
func (g *Gateway) routeDecide(ctx context.Context, call *decideCall, canonicalModel string, candidates []route.Candidate) (*route.Result, placement, error) {
	res, err := g.Router.ChatModel(ctx, call, canonicalModel, candidates)
	return res, placementOf(res), err
}

// decideCall 把一次决策调用适配成 route.Executor。
//
// # 为什么这里会出现一个 channel.Response
//
// 选路层（internal/route）与上面的编排都是围着 channel.Request /
// channel.Response 写的，而它们要做的事——按规范名筛候选、按渠道翻上游名、
// 失败换下一个、把用量交给计费——与协议无关。这条表面不共用那两个类型
// （见 internal/systemone 的包注释），但**共用这条执行顺序**，所以这里做一次
// 转换：把决策响应里计费真正要读的两个值（上游模型名与用量）放进一个
// channel.Response。
//
// 它是**有损**的，而且只在一处被读：上面的 usageOf 与 finishCharge 只读
// Usage 与 UpstreamModel，不读 Message。要交给调用方的那份完整响应由
// response 字段单独带出去，**不经过这个投影**——把决策结果渲染成 chat 的
// Message 才是真的把它塞进了错的形状。
type decideCall struct {
	exec DecideExecutor
	req  *systemone.Request

	// response 是**成功的那一次**调用的完整响应。
	//
	// route 在第一次成功时就返回，所以它对应的正是 routed.Response 那一次。
	// 失败的尝试不写它（那时 err 非 nil，route 也不会走到成功分支）。
	response *systemone.Response
}

// Chat 执行一次决策调用。
//
// carrier 是选路层自己合成的**载体**（见 route.Router.ChatModel）：除 Model
// 之外全是零值，而本方法也只读 Model。
func (c *decideCall) Chat(ctx context.Context, ch channel.Channel, carrier *channel.Request) (*channel.Response, error) {
	attempt := *c.req
	// 载体里的 Model 是路由层按**这个渠道**翻好的上游名（见
	// route.upstreamRequest）。决策请求自己的 Model 在入口那一侧是规范名，
	// 所以这里必须换掉——不换就是把规范名发给上游。
	attempt.Model = carrier.Model

	resp, err := c.exec.Decide(ctx, ch, &attempt)
	if err != nil {
		return nil, err
	}
	// 成功却交回空响应是**实现**的问题，不是上游的：唯一的实现有检查，所以
	// 今天不可达。留着这一行是因为解引用 nil 的现场是一次 panic，而调用方
	// 拿到的是断连而不是一个能查的失败（与 internal/api 里
	// `res == nil || res.Response == nil` 同一个处置）。
	//
	// 不包 ErrNoExecutor 一类的哨兵：那是「没接上」，这是「接上了但坏了」，
	// 报成 501 会把一个实现缺陷说成部署缺件，把排查方向带偏。
	if resp == nil {
		return nil, fmt.Errorf("渠道 %d: 决策执行器成功却没有交回响应", ch.ID)
	}
	c.response = resp

	return &channel.Response{
		UpstreamModel: resp.Model,
		Usage:         resp.Usage.ChannelUsage(),
	}, nil
}
