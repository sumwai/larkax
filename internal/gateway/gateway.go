// Package gateway 把一次请求的完整顺序串起来。
//
// 前面各包各自做一件事，这一层是**唯一知道完整顺序**的地方：
//
//	收请求 → 预留 → 选渠道 → 调上游 → 结算 → 交回分录
//
// 它不实现任何一件事，只决定它们的先后与失败时的走向。
//
// 协议解析不在这一层：调用方先经 internal/relay 把**调用方的**协议翻成统一
// 请求，再把翻好的请求交给本包。上游那套协议由适配器在更后面翻出去。
//
// # 为什么顺序本身需要一个包
//
// 有几条约束**只有站在这个位置才看得见**：
//
//   - 预留必须在调上游**之前**，否则上游已经花了钱而我们没冻住余额
//   - 上游失败要换渠道重试，而**已经吐字的流不能换**——那是客户端
//     能看到两段拼接答案的唯一原因
//   - 快照与用量落库在任何情况下都**不能影响请求本身**
//
// 上面第二条目前还只是约定：本包只接了非流式入口 Chat，流式入口尚未接入
// （见 Result.Events 与 Result.Estimated）。
//
// 把它们散在 HTTP handler 里，这三条就变成了三处注释；放在这里，
// 它们是可以被测试的执行顺序。
//
// # 这一层仍然不碰数据库
//
// 预留、结算、分录都通过接口交回调用方。理由是那些动作需要事务，
// 而事务的边界由调用方决定——本包不知道账单与账本是不是同一个库，
// 也不该猜。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/tokens"
)

// CandidateSource 提供某个模型的可选渠道。
//
// 与 route 的关系：route 决定「选谁」，这里决定「候选从哪来」。
// 分开是因为候选来自数据库（渠道配置 + 健康度 + 额度），
// 而那是本包不该知道的事。
type CandidateSource interface {
	Candidates(ctx context.Context, canonicalModel string) ([]route.Candidate, error)
}

// ReservationStore 负责把预留落库、并在不计费的结局里把它放开。
//
// 为什么必须落库而不是留在内存里：预留与结算之间隔着一次可能数分钟的
// 上游调用。进程在那期间重启，内存里那份冻结金额就没了——
// 而钱已经冻在账上，没人记得它为什么冻着。
//
// # 方法名与签名跟存储层逐字一致
//
// 这里的两个方法（SaveReservation / Release）与 internal/ledger/store 的
// Store 完全同名同签名，于是那个实现**直接满足**本接口，中间不需要一层
// 装配适配器——而适配器正是「签名对不上」这类错误最容易藏身的地方：
// 它把两边的差异从编译期挪到了运行期，还多一处没人读的转发代码。
type ReservationStore interface {
	// SaveReservation 落一条预留记录，返回它的标识。
	SaveReservation(ctx context.Context, req pipeline.Request, r *pipeline.Reservation) (int64, error)
	// Release 释放一次预留。用于**不计费的结局**：上游失败、客户端中途断开。
	//
	// 它必须是幂等的：同一次请求释放两次不该报错，也不该把冻结额减两次。
	Release(ctx context.Context, req pipeline.Request) error
}

// SettlementStore 负责把结算结果落库（账本分录 + 用量事件）。
//
// 两者在同一个事务里由实现保证——本包只交回结果，
// 事务边界是存储层的事。
type SettlementStore interface {
	Save(ctx context.Context, req pipeline.Request, charge *pipeline.Charge) error
}

// SnapshotSink 接收脱敏后的结构快照。
//
// # 它必须是可以「不实现」的
//
// 快照是排障辅助，整条链路挂掉时计费不该受任何影响。所以这里的方法
// 由本包在后台调用，而本包**不等它的结果**——见 snapshotAsync。
type SnapshotSink interface {
	Save(ctx context.Context, snap *Snapshot) error
}

// Snapshot 是交给落库层的一条快照。
//
// 字段与 request_snapshots 表对应。本包只负责把脱敏器与方向、
// 请求标识组装起来，不做脱敏本身。
type Snapshot struct {
	RequestID string
	UserID    int64
	// Direction 取值 request / response。
	Direction string
	// Skeleton 是脱敏后的骨架。它是**结构**而不是内容。
	Skeleton map[string]any
	// RedactedPaths 是被替换掉的字段路径，供确认白名单有没有漏放行项。
	RedactedPaths []string
	// Truncated 表示骨架超限后降级成了摘要层。
	Truncated bool
	// Version 是骨架格式版本。
	Version int
}

// Redactor 把请求或响应变成骨架。
//
// 它由 internal/snapshotwire 适配 internal/snapshot 实现。抽成接口是为了让
// 本包不依赖那个包的具体类型——那些类型会随白名单演进变化，而顺序不该跟着变。
type Redactor interface {
	Redact(req *channel.Request) (*Snapshot, error)
	RedactResponse(resp *channel.Response) (*Snapshot, error)
}

// Gateway 是一次请求的编排。
type Gateway struct {
	Pipeline     *pipeline.Pipeline
	Router       *route.Router
	Candidates   CandidateSource
	Reservations ReservationStore
	Settlements  SettlementStore

	// Redactor 与 Snapshots 可以都为空——那样就不采集快照。
	//
	// 这是刻意的默认：快照是可选能力，而不是「配错了就报错」的必填项。
	Redactor  Redactor
	Snapshots SnapshotSink

	// Executor 是真正调上游的东西。适配器实现它。
	Executor Executor

	// Now 可注入，便于测试时间相关的逻辑。
	Now func() time.Time
}

// Executor 是调上游的能力，由适配器组合而成。
//
// 与 route.Executor 分开：那个只要求非流式，而网关还要流式。
// 合起来会让「只想测选路」的地方也要实现流式。
type Executor interface {
	route.Executor
	route.StreamExecutor
}

// ErrNoExecutor 表示没有配置调上游的能力。
var ErrNoExecutor = errors.New("未配置上游执行器")

// Result 是一次请求的结果。
type Result struct {
	// Response 是非流式请求的结果；流式请求为 nil。
	Response *channel.Response
	// Events 是流式请求的事件流；非流式请求为 nil。
	// **流式入口尚未接入**，本字段目前不会被填充。
	Events <-chan channel.StreamEvent

	Channel channel.Channel
	Charge  *pipeline.Charge

	// Attempts 记录了换渠道过程中的每一次尝试，含失败的。
	Attempts []route.Attempt

	// Estimated 为 true 表示用量是估算的（客户端中途断开）。
	// 与 Events 一样，要等流式入口接入后才有值。
	Estimated bool
}

// Chat 处理一次非流式请求。
func (g *Gateway) Chat(ctx context.Context, req pipeline.Request, upstream *channel.Request) (*Result, error) {
	if g.Executor == nil {
		return nil, ErrNoExecutor
	}

	res, err := g.begin(ctx, req, upstream)
	if err != nil {
		return nil, err
	}
	defer res.finish(ctx)

	routed, err := g.Router.Chat(ctx, g.Executor, upstream, res.candidates, req.CanonicalModel)
	if err != nil {
		// 没有可用的渠道，或者全部失败。**不计费**。
		//
		// 这里要显式走一次结算而不是直接返回：计费与否的结论只由
		// Settle 给出，把「失败不计费」放在这里，它就还是一条能被
		// 测试的执行顺序，而不是散在调用方的一句 if。钱那一侧由 defer
		// 的 finish 释放——「失败就释放」这条规则如果散在调用方，
		// 漏一处就是一笔永久冻着的钱。
		return res.settleAndFail(ctx, err, pipeline.Outcome{UpstreamFailed: true})
	}

	out := pipeline.Outcome{
		Usage:       usageOf(routed.Response),
		UsageSource: tokens.SourceMeasured,
	}
	charge, err := g.finishCharge(ctx, req, res, routed.Channel, out)
	if err != nil {
		return nil, err
	}

	return &Result{
		Response: routed.Response,
		Channel:  routed.Channel,
		Charge:   charge,
		Attempts: routed.Attempts,
	}, nil
}

// handle 是内部的一次请求状态。
//
// 它把「预留已经落了库」这件事与请求绑在一起，于是 begin/finish 成对
// 出现时不会漏掉释放。**没有它的话，每个失败分支都要自己记得结算**，
// 而漏掉的那一处就是永久冻结的余额。
type handle struct {
	g          *Gateway
	req        pipeline.Request
	reserved   *pipeline.Reservation
	storeID    int64
	candidates []route.Candidate
	startedAt  time.Time
	// settled 记录这次请求有没有结算过。
	//
	// 它存在的理由只有一个：让**每一个**返回路径都经过结算。
	// 否则「预留了但没结算」的那些分支会留下一笔永久冻着的钱，
	// 而那种余额在界面上表现为「用户说他的钱不见了」。
	settled bool

	// charged 表示账单已经**成功交回调用方**，即结算事务真的跑过。
	//
	// 只有它为 true 时，冻住的钱才由结算那一步解冻。其余一切结局——
	// 上游失败、结算本身失败、结算落库失败、客户端中途断开，以及
	// **根本没配结算落库层**——都只有一条出路：走 Release 把钱放开。
	//
	// 它记的是「结算事务真的跑过」，不是「Settle 算出来了」：两者在没有
	// 落库层时会分叉——那次 Save 与它事务里的解冻都不会发生，此时若把
	// 它当成结算过，Release 会被挡下，两条路都没走，钱就永久冻着。
	charged bool
}

func (g *Gateway) begin(ctx context.Context, req pipeline.Request, upstream *channel.Request) (*handle, error) {
	h := &handle{g: g, req: req, startedAt: g.now()}

	// 候选必须在预留**之前**取：没有候选就没有可用的渠道，
	// 那种情况下不该冻住用户的钱。
	cands, err := g.Candidates.Candidates(ctx, req.CanonicalModel)
	if err != nil {
		return nil, fmt.Errorf("取可用渠道失败: %w", err)
	}
	if len(cands) == 0 {
		return nil, route.ErrNoCandidates
	}
	h.candidates = cands
	// 预留时还不知道会选中哪个渠道，所以先用请求里给的那个（可能是 0）。
	// 结算时会按**实际选中的渠道**重取 owner——见 finishCharge。

	reserved, err := g.Pipeline.Reserve(ctx, req)
	if err != nil {
		return nil, err
	}
	h.reserved = reserved

	if g.Reservations != nil {
		id, err := g.Reservations.SaveReservation(ctx, req, reserved)
		if err != nil {
			return nil, fmt.Errorf("预留落库失败: %w", err)
		}
		h.storeID = id
		req.BillingRequestID = id
		h.req = req
	}

	// 请求快照在**调上游之前**采集：出错要能复现「当时发出去的是什么」。
	g.snapshotAsync(ctx, upstream, "request")

	return h, nil
}

// finish 是 handle 的收尾。它总是被 defer 调用，负责把还没结算的那次
// 补上——正常路径已经结算过，这里会发现并跳过。
//
// 它也是**唯一**释放预留的地方。放在这里而不是各个失败分支上，理由与
// handle 本身存在的理由相同：散在分支上的规则，漏一处就是一笔永久冻着
// 的钱，而漏掉的那一处不会有任何东西报出来。
func (h *handle) finish(ctx context.Context) {
	if !h.settled {
		// 走到这里说明在结算之前就返回了（客户端断开、context 取消等）。
		// 按「上游失败」结算：不计费。
		_, _ = h.g.Pipeline.Settle(ctx, h.req, h.reserved,
			pipeline.Outcome{UpstreamFailed: true})
	}
	h.release(ctx)
}

// settleAndFail 结算一次失败的请求并返回原始错误。
//
// 失败不计费，所以这里用 UpstreamFailed。预留的释放交给 finish——
// 它是**每一个**返回路径的收尾点（见 finish 的注释），在这里再调一次
// 只会多一次数据库往返。
func (h *handle) settleAndFail(ctx context.Context, cause error, out pipeline.Outcome) (*Result, error) {
	h.settled = true
	_, _ = h.g.Pipeline.Settle(ctx, h.req, h.reserved, out)
	return nil, cause
}

// release 把这次请求冻住的钱放开。
//
// # 它为什么不跟着请求的 ctx 走
//
// 最需要释放的时刻，恰恰是客户端断开、请求 ctx 已经 Done 的那一刻：
// 拿一个已经取消的 ctx 去释放，释放本身就会失败，而那笔钱于是照旧冻着。
// 所以这里去掉取消与期限，另配一个独立的上限（releaseTimeout）——
// 与 internal/ledger/store 里「把对账结局写回去」那一步同一个做法
// （reconcile.go 的 finishTimeout），理由也一样。
//
// # 失败为什么不报
//
// 这里已经在一个失败的收尾路径上，而调用方拿到的错误必须是**最初那个**
// （上游失败、结算失败），不能被释放的错误盖掉——那会让「上游 500」
// 变成「释放失败」，排查的人会朝错的方向找。
//
// 代价要说清楚：释放失败时那笔钱**照旧冻着**，而这一条没有任何东西会
// 报出来——billing_requests 那一行仍是 pending，而对账把 pending 行之和
// 当成「本来就该冻住的量」（见 ledger/store 的 readPendingReservations），
// 于是它对得平。要发现它得另加一条上报，而本包没有日志层。
func (h *handle) release(ctx context.Context) {
	if h.charged || h.storeID == 0 || h.g.Reservations == nil {
		return
	}
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	_ = h.g.Reservations.Release(relCtx, h.req)
}

func (g *Gateway) finishCharge(ctx context.Context, req pipeline.Request, h *handle, ch channel.Channel, out pipeline.Outcome) (*pipeline.Charge, error) {
	h.settled = true

	// 结算用**实际选中的渠道**：渠道级与商家级规则按它匹配。
	// 预留时还不知道会选中谁，所以那时用的是请求里给的（通常是 0）——
	// 那意味着预留按所有候选里**最贵的**那个规则算（见 Pipeline.Reserve 的注释）。
	settleReq := req
	settleReq.ChannelID = ch.ID
	settleReq.OwnerUserID = ownerOf(h.candidates, ch.ID)

	charge, err := g.Pipeline.Settle(ctx, settleReq, h.reserved, out)
	if err != nil {
		return nil, err
	}

	if g.Settlements != nil {
		if err := g.Settlements.Save(ctx, settleReq, charge); err != nil {
			// 结算落库失败是**必须报出来**的：账已经算出来了但没记下，
			// 而钱是真金白银。这里不吞错误。
			//
			// 没走到下面那行，所以这次请求仍算「没成功交回账单」——
			// finish 会把预留释放掉。对**真正的**落库失败而言那是对的：
			// 事务已经回滚，一分钱都没扣，冻着的那笔钱没有理由留着。
			//
			// 例外是 ErrAlreadySettled：那是重放，上一次结算已经扣过钱，
			// 事务并没有回滚。这里不必为它分支——释放会被存储层按
			// status='settled' 挡下，重复解冻不会发生。
			return nil, fmt.Errorf("结算落库失败: %w", err)
		}
		// 账单已经交回调用方：冻住的钱由结算那一步解冻，这里不再释放。
		//
		// 置位放在这个 if **里面**是有意的。没有落库层时上面那次 Save
		// 不会跑，它事务里的解冻也就不会发生；此时若把 charged 置成
		// true，release 会被挡下，那笔钱两条路都没走，永久冻着。
		h.charged = true
	}
	return charge, nil
}

// snapshotAsync 采集一条快照并交给落库层。
//
// # 采集是同步的，落库是异步的
//
// 这个分界不是随意的：
//
//   - **脱敏是同步的**。它是纯 CPU 计算、不查库不发请求，所以它够快，
//     而且必须**在调上游之前**完成——否则采集到的是「调用之后」的状态，
//     而请求快照要回答的正是「当时发出去的是什么」。
//   - **落库是异步的**。它是 I/O，可能慢、可能失败。快照是排障辅助，
//     整条链路挂掉时计费不该受任何影响，所以它跑在独立的 goroutine 里、
//     带自己的超时、错误只丢不报。
//
// 两者都做才对：全同步会让一次慢查库拖住一个请求，全异步会让「请求
// 快照」变成「请求发出后某一刻的快照」——后者在排查时序相关的
// 问题时正好错过关键状态。
func (g *Gateway) snapshotAsync(ctx context.Context, req *channel.Request, direction string) {
	if g.Redactor == nil || g.Snapshots == nil || req == nil {
		return
	}

	snap, err := g.Redactor.Redact(req)
	if err != nil {
		// 脱敏失败就丢快照——绝不回落成存原文。
		// 这里同步返回而不是在 goroutine 里判断：失败是**确定性**的，
		// 让它跑一趟后台再丢弃是白费。
		return
	}
	snap.RequestID = requestIDOf(ctx)
	snap.Direction = direction

	// 用请求的 context 值但**不带取消**：请求结束后快照仍该写完，
	// 而因为客户端断开就丢掉一条快照，恰恰丢掉了最该看的那条。
	base := context.WithoutCancel(ctx)

	go func() {
		snapCtx, cancel := context.WithTimeout(base, snapshotTimeout)
		defer cancel()
		_ = g.Snapshots.Save(snapCtx, snap)
	}()
}

// usageOf 从上游响应里取出用量。
//
// 上游没报 usage 时返回零值——那会让结算金额为零，**而不是静默按某个
// 默认价收钱**。零金额在账上是可见的（有请求无消费），而默认价不可见。
func usageOf(resp *channel.Response) channel.Usage {
	if resp == nil {
		return channel.Usage{}
	}
	return resp.Usage
}

// ownerOf 从候选里找出某个渠道的商家。
//
// 结算要用与预留同一个 owner，否则商家级规则在两次调用里可能匹配到
// 不同的规则，算出一个与预留无缘由的差额。
func ownerOf(cands []route.Candidate, channelID int64) int64 {
	for _, c := range cands {
		if c.Channel.ID == channelID {
			return c.OwnerUserID
		}
	}
	return 0
}

// releaseTimeout 是释放一次预留的时间上限。
//
// 与 snapshotTimeout 分开：那个管的是「丢掉一条快照」，这个管的是
// 「把用户的钱放开」，两者不该共用一个数。五秒的依据与对账那条一样
// （见 ledger/store 的 finishTimeout）：写的是按主键定位的单行 UPDATE，
// 量级是毫秒；连它都写不进去，说明库已经不接受写入了，把上限放宽也
// 救不回来，而这一步的全部意义就是不许挂住。
const releaseTimeout = 5 * time.Second

// snapshotTimeout 是单条快照的写入上限。
//
// 它是独立于请求的：快照再慢也不该拖住任何东西，所以宁可丢掉它。
const snapshotTimeout = 5 * time.Second

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func requestIDOf(ctx context.Context) string {
	// 不 import observe 而用一个窄接口，是为了让本包能在没有观测层时
	// 也能测。真实实现由调用方在 ctx 里放一个实现该接口的值。
	if v, ok := ctx.Value(requestIDKey{}).(interface{ String() string }); ok {
		return v.String()
	}
	return ""
}

type requestIDKey struct{}
