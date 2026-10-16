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
// 上面第二条在流式那条路上是**可执行**的：上游一旦接受请求就不再换渠道，
// 判据与处置见 stream.go。
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
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
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

// CallRecorder 接收每一次上游调用的结局（健康度与额度自记账）。
//
// 与 CandidateSource 同一个做法：接口在这里声明、参数类型由 route 定义，
// 于是满足它的包（internal/channels）不必 import 本包——gateway 是入口层，
// 数据面依赖它会把整条入口链路拖进去（见 scripts/check-layering.sh）。
//
// 它与 SnapshotSink 的取舍相反：快照可以丢，而健康度是**路由的输入**，
// 所以这一路是同步的、错误要留痕（见 recordCall）。
type CallRecorder interface {
	RecordCall(ctx context.Context, rec route.CallOutcome) error
}

// Snapshot 是交给落库层的一条快照。
//
// 字段与 request_snapshots 表对应。本包只负责把脱敏器与方向、
// 请求标识组装起来，不做脱敏本身。
type Snapshot struct {
	RequestID string
	// UserID 是**发起这次请求的用户**，不是渠道所属的商家。
	//
	// 两者混起来不报错：这一列没有外键，写谁的 id 都插得进去，而快照是按
	// 用户查的（见 request_snapshots_user_idx）——记到商家名下等于让这份
	// 排障数据挂到了另一个人身上。
	UserID int64
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

// ReleaseFailureSink 接收「释放预留失败」的记录。
//
// # 它必须是可以「不实现」的
//
// 与 Snapshots 同理：留痕是排障辅助，计费链路上不能因此多出一个必填依赖。
// 字段为 nil 时这一条不留痕——那正是今天的形状，接口只是给它一个出口。
//
// # 它补的是对账补不了的那一半
//
// 释放失败之后那笔钱照旧冻着，而 billing_requests 那一行仍是 pending：
// 对账的 stale_reservation 那类 finding 会在阈值（一天）之后把它报出来。
// 那是**结果**上的兜底，它看不到**当时**发生了什么——这一路补的正是那个：
// 失败的时刻与 Release 返回的错误。
//
// 两者都要：对账保证「挂了很久的钱总会被人看见」，这里保证「第一次失败
// 当时就留下原因」。只留痕而不报，进程重启或日志轮转之后就什么都没有了。
type ReleaseFailureSink interface {
	Save(ctx context.Context, f ReleaseFailure) error
}

// ReleaseFailure 是一次释放失败的记录。
type ReleaseFailure struct {
	// RequestID 是这次请求的标识，与 billing_requests.request_id 对应。
	RequestID string
	// ReservationID 是预留落库时拿到的 id。
	ReservationID int64
	// UserID 是预留归属的用户。
	UserID int64
	// At 是失败发生的时刻，取自 Gateway.Now。
	At time.Time
	// Err 是 Release 返回的错误。
	Err error
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

	// ReleaseFailures 可以为空——那样释放失败不留痕（见 ReleaseFailureSink）。
	ReleaseFailures ReleaseFailureSink

	// Recorder 接收每一次上游调用的结局：渠道健康度与额度自记账。
	//
	// 可以为空——那样什么都不记。它是**可选**的接线，但装上之后它就不再是
	// 排障辅助：一个正在连续失败的渠道靠它被挡在候选集之外（见 recordCall）。
	Recorder CallRecorder

	// Executor 是真正调上游的东西。适配器实现它。
	Executor Executor

	// Counter 是流式中断时估算用量用的分词计数器。
	//
	// 可以为 nil——那时估算退到启发式（见 tokens.StreamEstimator），
	// 结果仍然可用，只是来源标成 SourceEstimated 而不是 SourceTokenized。
	// 非流式那条路用不到它：那里的用量只可能来自上游的响应体。
	Counter *tokens.Counter

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
	//
	// **调用方必须读到它关闭为止。** 网关的转发 goroutine 在关闭它**之前**
	// 完成结算与释放；不读到底，那条 goroutine 会阻塞在发送上，而「钱冻着」
	// 这件事不会有任何东西报出来。
	//
	// 关闭之后 Charge 与 Estimated 才有值——它们也是在那之前写好的，
	// 所以读完流再读这两个字段是安全的，不需要额外的同步。
	Events <-chan channel.StreamEvent

	Channel channel.Channel
	// Charge 是这次请求的结算结果。流式请求在事件流**关闭之后**才有值。
	Charge *pipeline.Charge

	// Attempts 记录了换渠道过程中的每一次尝试，含失败的。
	Attempts []route.Attempt

	// Estimated 为 true 表示用量**不是上游实测值**：客户端中途断开、
	// 上游在流中途失败、或上游正常结束却没带 usage。
	//
	// 它与 pipeline.Outcome.UsageSource 是同一个口径（见 tokens.Source）：
	// 只有 SourceMeasured 时它为 false。非流式请求恒为 false——那条路上
	// 用量只可能来自上游的响应体。
	Estimated bool
}

// Chat 处理一次非流式请求。
//
// 顺序与 ChatStream 完全相同，差别只在用量从哪来：这里来自上游的响应体，
// 那里来自读完的整条事件流。两条路共用 begin / finish / finishCharge。
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
		//
		// 健康度先记：失败的调用正是它要记的那些，而下面那次结算
		// 只动钱。
		g.recordAttempts(ctx, attemptsOf(routed), nil)
		return res.settleAndFail(ctx, err, pipeline.Outcome{UpstreamFailed: true})
	}

	out := pipeline.Outcome{
		Usage:       usageOf(routed.Response),
		UsageSource: tokens.SourceMeasured,
	}
	g.recordAttempts(ctx, routed.Attempts, &out.Usage)
	charge, err := g.finishCharge(ctx, res, routed.Channel, out)
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
	h := &handle{g: g, req: req}

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

	// 预留按**整个候选集**取上界：每个候选带它自己的商家，规则那一侧对
	// 每个候选各取一条，取其中金额最大的那个（见 pipeline.Reserve）。
	// 只给渠道 id 是不够的——商家级规则按 owner 匹配。
	//
	// 这里还不能收窄成一个渠道：选路发生在预留之后。结算时才按实际选中
	// 的那一个重新匹配——见 finishCharge。
	req.Candidates = ruleCandidates(cands)

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
		// 没有预留层时它保持 0：那本来就没有账单行可指。
		req.BillingRequestID = id
	}
	// 这一行在 if **外面**：无论有没有预留层，h.req 都该是「已经开始的那份
	// 请求」。结算以它为基准（见 finishCharge），而它少了 BillingRequestID，
	// 账本分录就挂不回这次账单——两边只剩时间上的巧合。
	h.req = req

	// 请求快照在**调上游之前**采集：出错要能复现「当时发出去的是什么」。
	g.snapshotAsync(ctx, upstream, "request", req.UserID)

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
// # 失败为什么不把错误交回调用方
//
// 这里已经在一个失败的收尾路径上，而调用方拿到的错误必须是**最初那个**
// （上游失败、结算失败），不能被释放的错误盖掉——那会让「上游 500」
// 变成「释放失败」，排查的人会朝错的方向找。
//
// 代价要说清楚：释放失败时那笔钱**照旧冻着**——billing_requests 那一行
// 仍是 pending，而对账把 pending 行之和当成「本来就该冻住的量」（见
// ledger/store 的 readPendingReservations），于是它对得平，不报错。
//
// 现在有两条互补的路让它最终被看见：失败当时交给 ReleaseFailures
// （可选，见 reportReleaseFailure），以及一天之后由对账的
// stale_reservation 报出来。两条都不改这里的行为：钱该冻着还是冻着，
// 处置由人决定。
func (h *handle) release(ctx context.Context) {
	if h.charged || h.storeID == 0 || h.g.Reservations == nil {
		return
	}
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := h.g.Reservations.Release(relCtx, h.req); err != nil {
		h.reportReleaseFailure(ctx, err)
	}
}

// reportReleaseFailure 把一次释放失败交给上报口。
//
// # 为什么是异步的
//
// 它在收尾路径上，而调用方正在等这一次请求返回：留痕写不进去不该再拖住
// 他。取舍与快照落库相同（见 snapshotAsync），连「用不带取消的 context」
// 也一样——走到这一步时请求的 ctx 多半已经 Done，而那正是最该留下这条痕
// 的时刻。
//
// 异步的代价要说清楚：进程紧接着退出时这条痕会丢。可以接受，因为它是
// **补充**而不是保证——那笔钱冻着这件事本身由对账兜底，丢掉的只是当时
// 那个错误。
//
// 与快照一样只丢不报：调用方拿到的错误必须是**最初那个**，被释放的错误
// 盖掉会让排查朝错的方向找（见 release 的注释）。
func (h *handle) reportReleaseFailure(ctx context.Context, cause error) {
	if h.g.ReleaseFailures == nil {
		return
	}
	rec := ReleaseFailure{
		RequestID:     h.req.RequestID,
		ReservationID: h.storeID,
		UserID:        h.req.UserID,
		At:            h.g.now(),
		Err:           cause,
	}

	base := context.WithoutCancel(ctx)
	go func() {
		// 上限与释放本身共用 releaseTimeout：两者写的都是「按主键定位的
		// 单行」这个量级的东西，而这一步的全部意义同样是不许挂住。
		recCtx, cancel := context.WithTimeout(base, releaseTimeout)
		defer cancel()
		_ = h.g.ReleaseFailures.Save(recCtx, rec)
	}()
}

// finishCharge 结算一次成功的请求。
//
// 基准是 h.req，不是调用方手上那份请求：BillingRequestID 是在 begin 里
// 赋给 h.req 的，外层那份看不到它——而少了它，账本分录就挂不回这次账单。
func (g *Gateway) finishCharge(ctx context.Context, h *handle, ch channel.Channel, out pipeline.Outcome) (*pipeline.Charge, error) {
	h.settled = true

	// 结算用**实际选中的渠道**：渠道级与商家级规则按它匹配。
	//
	// 预留时还不知道会选中谁，所以那时按整个候选集取上界——每个候选各取
	// 自己适用的一条规则，取其中金额最大的（见 pipeline.Reserve）。实际
	// 选中的渠道必是候选之一，而它的规则在那次取上界里算过，所以结算
	// 不会超过预留。EvalWorstCase 取的是**一条规则内部**的最坏分支，
	// 不跨规则；跨规则那一层由预留的「取最大」负责。
	settleReq := h.req
	settleReq.ChannelID = ch.ID
	settleReq.OwnerUserID = ownerOf(h.candidates, ch.ID)
	// 候选集也收窄成选中的那一个，让这份请求自洽：上面两个字段说的是
	// 「实际选中了谁」，而候选集若还留着整份，两者对不上。结算本身不读
	// 候选集（见 pipeline.Request），这一行是给后来读它的人留的。
	settleReq.Candidates = []pipeline.RuleCandidate{{
		ChannelID:   settleReq.ChannelID,
		OwnerUserID: settleReq.OwnerUserID,
	}}

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
//
// # 归属由本包填
//
// request_id / user_id / direction 说的是「这条快照属于谁」，脱敏器不知道
// 也不该知道（见 internal/snapshotwire 的 toSnapshot），所以三个字段在这里
// 补齐。userID 必须是**发起这次请求的用户**，见 Snapshot.UserID 的注释。
func (g *Gateway) snapshotAsync(ctx context.Context, req *channel.Request, direction string, userID int64) {
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
	snap.UserID = userID
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

// ruleCandidates 把候选渠道折成规则匹配用的集合。
//
// 每个候选要带上**它自己的**商家：商家级规则按 owner 匹配，只给渠道 id
// 会让候选里的商家价一条都匹配不上，而预留因此退回平台价——那正是
// 「预留小于结算」的来源（见 pipeline.Reserve）。
func ruleCandidates(cands []route.Candidate) []pipeline.RuleCandidate {
	out := make([]pipeline.RuleCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, pipeline.RuleCandidate{
			ChannelID:   c.Channel.ID,
			OwnerUserID: c.OwnerUserID,
		})
	}
	return out
}

// recordAttempts 把一次路由过程的每一次尝试记成渠道健康度。
//
// 逐次记而不是只记最后一次：换渠道之前那些失败的调用同样说明那个渠道
// 此刻不行，而「连续失败 N 次就别再试」这条保护正是靠它们累起来的。
//
// usage 非 nil 时附加给**成功的那一次**——只有它产生了用量。
func (g *Gateway) recordAttempts(ctx context.Context, attempts []route.Attempt, usage *channel.Usage) {
	if g.Recorder == nil || len(attempts) == 0 {
		return
	}
	for _, a := range attempts {
		rec := route.CallOutcome{
			ChannelID: a.ChannelID,
			At:        attemptTime(a, g.now()),
			Verdict:   verdictOf(a.Err),
		}
		if a.Err == nil && usage != nil {
			rec.Usage = *usage
		}
		g.recordCall(ctx, rec)
	}
}

// attemptTime 取一次尝试**结束**的时刻。
//
// 用结束时刻而不是记录时刻：健康度桶的归属与 last_failure_at 说的都是
// 「这件事什么时候发生的」，而记一条结局可能晚于它几个请求。
//
// 时间为零时回落到当前时刻：那种 Attempt 只可能来自没填 StartedAt 的
// 调用方，而按零值算会把它归进公元 1 年的那个桶，在那里留下一行
// 永远不会被任何窗口查到的垃圾数据。
func attemptTime(a route.Attempt, now time.Time) time.Time {
	if a.StartedAt.IsZero() {
		return now
	}
	return a.StartedAt.Add(a.Duration)
}

// verdictOf 把一次尝试的错误翻成调用结论。
func verdictOf(err error) route.Verdict {
	switch {
	case err == nil:
		return route.VerdictSuccess
	case channelAtFault(err):
		return route.VerdictFailure
	default:
		return route.VerdictUnattributed
	}
}

// channelAtFault 判断一次失败该不该记在渠道头上。
//
// 判据是「换一个渠道会不会不一样」：
//
//   - 适配器判为可重试的（429、5xx、网络层）——会，所以这是渠道此刻不行
//   - 上游拒绝了我们的身份或余额（401/402/403）——换谁都是同一个错，
//     但那正是**这个渠道**坏了，而且它会一直坏下去
//
// 其余 4xx（400 参数错、404 模型名不存在、422）是**这次请求**的问题：
// 三次参数错把一个健康渠道摘掉，比不摘它更糟——摘掉之后没有流量，
// 计数也不会自己降回去（见 internal/channels 的 streakCooldown）。
//
// ErrUnknownType（这条候选的配置本版本二进制不认识）同样不算：它是部署
// 与回滚留下的痕迹，不是渠道的运行状态。
func channelAtFault(err error) bool {
	if err == nil {
		return false
	}
	if channel.IsRetryable(err) {
		return true
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		return false
	}
	switch up.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
		return true
	}
	return false
}

// attemptsOf 取一次路由结果的尝试列表。
//
// 路由失败时结果可能为 nil（一个可用候选都没有），那时没有尝试可记。
func attemptsOf(r *route.Result) []route.Attempt {
	if r == nil {
		return nil
	}
	return r.Attempts
}

// recordCall 把一次调用的结局交给写入方。
//
// # 为什么是同步的
//
// 健康度不是排障数据，而是路由的**输入**：一个正在连续失败的渠道靠它被
// 挡在候选集之外。放进后台 goroutine 的话，进程在写入之前退出就丢掉一条，
// 而丢掉的表现是「保护没生效」——它与「这个渠道没失败」在库里长得一样。
// 代价是每个请求多一次按主键定位的单行写，换的是这条保护真的会落地。
//
// # 为什么错误只记不报
//
// 调用方拿到的必须是**最初那个**结论（上游失败、结算失败），不能被一次
// 写健康度的错误盖掉——那会让「上游 500」变成「写健康度失败」，
// 排查的人会朝错的方向找。丢掉的后果要说清楚：这一次调用的结局不入库，
// 于是连续失败少记一次、桶少一个请求。
//
// # 为什么去掉取消
//
// 最需要记一笔的时刻，正是客户端断开、请求 ctx 已经 Done 的那一刻
// （理由与 handle.release 相同）。拿一个已取消的 ctx 去写，写本身就会失败。
func (g *Gateway) recordCall(ctx context.Context, rec route.CallOutcome) {
	if g.Recorder == nil {
		return
	}
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := g.Recorder.RecordCall(recCtx, rec); err != nil {
		observe.FromContext(ctx).Error("写渠道健康度失败",
			"err", err, "channel_id", rec.ChannelID, "request_id", requestIDOf(ctx))
	}
}

// recordTimeout 是写一条调用结局的时间上限。
//
// 与 releaseTimeout 同一个量级：写的都是按主键定位的单行，量级是毫秒；
// 连它都写不进去，说明库已经不接受写入了，放宽上限也救不回来，
// 而这一步不许挂住请求。
const recordTimeout = 5 * time.Second

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
