// Package snapshotwire 把请求结构快照的三段接起来：读配置、脱敏、落库。
//
// 它存在的原因是两侧的接口不能直接对上：
//
//   - internal/snapshot 的脱敏器**只管一个方向**（构造时就定了是请求还是响应），
//     而 internal/gateway 要求一个能同时处理两个方向的对象；
//   - snapshot 的产出是它自己的 Result，而网关要的是 gateway.Snapshot。
//
// 于是本包是唯一同时 import 两者的地方：它持有两个方向的脱敏器，
// 把 gateway 的两个接口实现出来，并把落库与留存期清理放在一起。
// 网关因此不必知道快照是怎么存的，snapshot 也不必知道有一个网关。
//
// # 快照链路不能影响请求路径
//
// 这条约束决定了本包里的每一次取舍，落到代码上是四处：
//
//  1. 落库是**异步**的（网关起 goroutine，不等结果），本包只保证单次调用够快。
//  2. 并发写有上限，满了就丢快照而不是排队等——见 Save。
//  3. 任何失败只记日志，绝不向请求路径抛错误。
//  4. 分区不存在时当场补建，而不是让行落进兜底分区——那条路会让留存承诺失效。
//
// # 配置从哪来
//
// 开关、字节上限与留存期都来自 settings 表（见 config.go）。
// 关掉开关时 Redactor 与 Sink 都返回 nil，网关据此完全不采集——
// 「关掉」是零成本，而不是「采集了再丢掉」。
//
// 而落库层照建：清理存量分区只用得上它，与写不写新快照无关（见 New）。
package snapshotwire

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/snapshot"
)

// maxConcurrentWrites 是同时进行的快照写入数上限。
//
// # 它管住的是什么
//
// 网关为每条请求起一个 goroutine 去调 Save（那是它的既定行为，见
// gateway.snapshotAsync）。这里限住的是**同时真的在写库的条数**：
// 拿不到令牌的那条立刻返回并丢弃，不排队。于是过载时堆积的只有
// 「正在调 Save 但还没被处理」的那一小段，而不是一个无界的待办队列；
// 每个被丢弃的 goroutine 都是立刻结束的，不会持着骨架等下去。
//
// 不这么做的话，突发流量下同时涌现的是「每条请求一个写连接」：
// 数据库连接池是 32 条（见 internal/db），会被快照占满，
// 而那是计费查询要用的同一批连接。
//
// 取 8 而不是 1：单条写入是毫秒级的插入，串行会让突发流量下的丢弃率
// 无谓地高，而 8 仍然远小于连接池，够不着计费。
const maxConcurrentWrites = 8

// ErrCapacityExceeded 表示并发写已满，这条快照被主动丢弃。
//
// 它不是故障：丢快照是本包在过载时的既定行为，丢掉的那条不重试、不排队。
// 单独一个错误类型是为了让日志里能一眼区分「满了丢了」与「库写失败了」——
// 前者说明上限或流量需要调整，后者说明存储有问题。
var ErrCapacityExceeded = errors.New("快照并发写已满，本条被丢弃")

// ConsentSource 回答「这次请求此刻有没有有效的原样采集授权」。
//
// # 它只回答这一个问题
//
// 授权记录本身（capture_consents 表：范围、同意文案与版本、到期与撤销）
// 不归这一层管，这一层只要一个是/否。这么分是为了让判定**关不上时朝安全
// 那边倒**：查不到、说不清、出错，实现都应当返回 false——那时落下来的是
// 骨架，而不是原文。
//
// # 为什么 ctx 一起给它
//
// 它可能要查一次库，而这是请求路径上的调用：客户端走掉时那次查库不该接着等。
//
// # 为什么 userID 与 requestID 分开给
//
// 两者对应授权记录上的两种范围：按一段时间（看 user_id）与按单条请求
// （看 user_id + request_id）。哪个匹配得上由实现判，这一层不拼条件——
// 拼在这里的话，表上的 shape 约束与判定就会各说各话。
type ConsentSource interface {
	AllowsRawCapture(ctx context.Context, userID int64, requestID string) bool
}

// NoConsentSource 是「这条链路上永远不存在有效授权」。
//
// 它是**如实**的实现，不是占位：一条没有授权入口的链路就是这个形状，
// 每一条落下来的都是骨架。
//
// 今天用它的是 `larkaxctl snapshot cleanup`（那条路只删分区、一条也不采，
// 所以没有「这次请求有没有授权」可问）。**请求链路已经不用它了**：装配层接的
// 是查 capture_consents 的那一个来源，授权记录由 `larkaxctl consent grant` 写
// （见 internal/assembly 的 buildSnapshots 与 consent.go 的 DBConsentSource）。
type NoConsentSource struct{}

// AllowsRawCapture 永远回答「没有」。
func (NoConsentSource) AllowsRawCapture(context.Context, int64, string) bool { return false }

// Collector 是快照链路的装配结果：两个方向的脱敏器 + 一个落库层。
//
// 它同时实现 gateway.Redactor 与 gateway.SnapshotSink。
type Collector struct {
	requestRedactor  *snapshot.Redactor
	responseRedactor *snapshot.Redactor

	// consent 判「这一次要不要留原文」。**不接受 nil**（见 New）：
	// 它在请求路径上被调用，nil 会在那里 panic。
	consent ConsentSource

	// 决策那条表面（见 internal/gateway 的 DecisionRedactor）的两个脱敏器。
	//
	// 它们与上面那两个**不复用**：允许表按线格式建，而 chat 与决策两条表面
	// 没有任何一个字段是共用的（见 internal/snapshot 的 decision.go）。
	// 复用一张表意味着两条表面互相污染彼此的兜底规则。
	decisionRequestRedactor  *snapshot.Redactor
	decisionResponseRedactor *snapshot.Redactor

	// store 是落库层。**关掉采集时它也照样建**：清理存量分区只用得上它，
	// 与「要不要写新快照」无关（见 New）。
	store *Store

	// collecting 表示这条链路要不要写新快照。
	//
	// 它只管**写**。不要拿它去判「这条链路能不能清理存量分区」——那个判据
	// 曾经就是它（当时的名字叫 Enabled），后果是关掉采集时清理被一起废掉，
	// 而命令照样打印成功、退出码 0。清理没有开关：留存承诺管的是已经存下来
	// 的数据，与「还要不要写新的」无关。
	collecting bool

	// retentionDays 只被清理动作读，请求路径不碰它。
	retentionDays int

	// sem 是并发写的令牌桶。缓冲通道在这里的用途是计数而不是传值，
	// 空结构体不占内存。
	sem chan struct{}

	logger *slog.Logger

	written atomic.Int64
	dropped atomic.Int64
	failed  atomic.Int64
}

// New 按配置建立快照链路。
//
// # 关掉采集时仍然建 Store
//
// 开关只决定「要不要写新快照」：关掉时 Redactor 与 Sink 都返回 nil，
// 调用方据此完全不采集，代价是零而不是「脱敏一遍再丢掉」。
//
// 而落库层照建。清理存量分区只需要 DDL 能力，与写不写新快照无关；
// 把两者捆在一个开关上的后果是「关掉采集」这个动作顺带废掉了留存承诺，
// 而关掉采集的动机通常正是「不想让快照再涨」，那时已有的分区反而更需要被清掉。
// settings 里这个开关自己的描述也是这么写的：「关闭后不再写入新快照，
// 已有的按留存期清理」。
//
// 于是「写」与「清」各有各的判据：写看 collecting，清不看任何开关。
// 别用 collecting 去判后者——那正是这里修掉的缺陷。
//
// max_bytes 非法（非正）时返回错误而不回落到默认值：上限配错会导致
// 每条骨架都降级成摘要层，静默回落会让这种配置错误永远不显形。
//
// # consent 不接受 nil
//
// 它在请求路径上被调用，nil 会在那里 panic——那既不是「不采集」也不是
// 「不授权」，是一次故障。没有授权入口时传 NoConsentSource：那是**一个
// 答案**（永远没有授权），与「忘了接」不是同一件事，而两者的后果也不同。
func New(cfg Config, db DB, consent ConsentSource, logger *slog.Logger) (*Collector, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if consent == nil {
		return nil, errors.New("授权来源为 nil：没有授权入口时传 NoConsentSource")
	}
	c := &Collector{
		logger:        logger,
		consent:       consent,
		sem:           make(chan struct{}, maxConcurrentWrites),
		store:         NewStore(db, logger),
		collecting:    cfg.Enabled,
		retentionDays: cfg.RetentionDays,
	}
	if !cfg.Enabled {
		return c, nil
	}

	req, err := snapshot.New(snapshot.RootRequest, snapshot.WithMaxBytes(cfg.MaxBytes))
	if err != nil {
		return nil, err
	}
	resp, err := snapshot.New(snapshot.RootResponse, snapshot.WithMaxBytes(cfg.MaxBytes))
	if err != nil {
		return nil, err
	}
	// 决策表面同样两个方向各一个。表不同，方向取值相同——方向是落库那一列
	// 的取值，而它只有 request / response 两个（见 internal/snapshot 的
	// Surface 注释）。少建一个的话，那条路径会在运行期解引用 nil。
	decReq, err := snapshot.NewFor(snapshot.SurfaceDecision, snapshot.RootRequest,
		snapshot.WithMaxBytes(cfg.MaxBytes))
	if err != nil {
		return nil, err
	}
	decResp, err := snapshot.NewFor(snapshot.SurfaceDecision, snapshot.RootResponse,
		snapshot.WithMaxBytes(cfg.MaxBytes))
	if err != nil {
		return nil, err
	}
	c.requestRedactor = req
	c.responseRedactor = resp
	c.decisionRequestRedactor = decReq
	c.decisionResponseRedactor = decResp
	return c, nil
}

// Collecting 表示这条链路是否采集（写新快照）。
//
// 它与「能不能清理存量分区」是两件事：清理只看落库层在不在，而落库层在采集
// 关掉时也建好了（见 New）。所以这个方法不该被用来判清理路径能不能跑——
// 关掉采集之后旧分区照样要按留存期删掉。
func (c *Collector) Collecting() bool { return c.collecting }

// DropExpired 按留存期删除到期分区。
//
// 它不在请求路径上：由运维命令或后台任务调用（见 cmd/larkaxctl 的
// snapshot 子命令）。留存期的承诺靠的是「这个动作真的被执行」，
// 所以它的返回值是被删掉的分区名清单，而不是一个 bool——
// 调用方要能看出「删了哪些」，而不只是「没报错」。
//
// # 采集关掉时照样删
//
// 这里不看 Collecting()：留存承诺管的是**已经存下来的**数据，与「还要不要
// 写新的」无关。关掉采集之后不再清理的后果是旧分区永久留在库里——而清掉
// 它们恰恰是关掉采集的人想要的第一件事。
func (c *Collector) DropExpired(ctx context.Context, at time.Time) ([]string, error) {
	return c.store.DropExpired(ctx, c.retentionDays, at)
}

// ExpiredPartitions 列出到期分区但不动它们，供演练使用。
//
// 与 DropExpired 同源判据，「演练说会删哪些」因此等于「执行真的删了哪些」。
// 与它一样不看采集开关：关掉采集时存量分区照样按留存期到期，
// 演练也必须照实列出来，否则「演练说没有到期的」又变成一句无从核对的话。
func (c *Collector) ExpiredPartitions(ctx context.Context, at time.Time) ([]string, error) {
	return c.store.ExpiredPartitions(ctx, c.retentionDays, at)
}

// Redactor 返回给网关用的脱敏器；不采集时返回 nil。
//
// 返回 nil 而不是一个「什么都不做」的实现：网关对 nil 的处理是
// 「根本不采集」，而对空实现的处理是「采集了一条空快照再落库」。
func (c *Collector) Redactor() gateway.Redactor {
	if !c.Collecting() {
		return nil
	}
	return c
}

// Sink 返回给网关用的落库层；不采集时返回 nil。
func (c *Collector) Sink() gateway.SnapshotSink {
	if !c.Collecting() {
		return nil
	}
	return c
}

// DecisionRedactor 返回给网关用的决策表面脱敏器；不采集时返回 nil。
//
// 与 Redactor 分开一个入口，是因为网关那两个字段是两个独立的接口
// （见 gateway.DecisionRedactor）：它们任一个为 nil 都只关掉对应的那条路，
// 而不是把两条一起关掉。返回 nil 而不是空实现的理由同 Redactor。
func (c *Collector) DecisionRedactor() gateway.DecisionRedactor {
	if !c.Collecting() {
		return nil
	}
	return c
}

// Redact 脱敏一次请求。
//
// # 两种正文
//
// 有有效授权时返回**原文**（落库时 capture_mode = full_consented），
// 否则返回骨架（默认那一档）。判在这里而不是网关那一侧：网关不认识授权
// 记录，而这一层已经拿着配置与库；判定要查库，所以它拿到 ctx。
//
// 判据只有这一个入口，两个方向共用它——「同一个用户、同一次请求，请求方向
// 留了原文而响应方向没留」这种分裂不该发生，而两处各判一次就会发生。
func (c *Collector) Redact(ctx context.Context, req *channel.Request, id gateway.CaptureIdentity) (*gateway.Snapshot, error) {
	raw := c.rawCaptureAllowed(ctx, id)
	mode := captureModeFor(raw)
	if raw {
		res, err := c.requestRedactor.RedactRequestVerbatim(req)
		return toSnapshot(res, err, mode)
	}
	res, err := c.requestRedactor.RedactRequest(req)
	return toSnapshot(res, err, mode)
}

// RedactResponse 脱敏一次响应。
//
// 注意它只有归一化字段那部分：上游原样返回的 Response.Raw 与 Usage.Raw
// 不在其中，见 snapshot.RedactResponse 的注释。要采集它们需要单独传
// RedactRaw，而那是一个需要单独决定的问题。「原样采集那一档」也一样——
// 它指的是归一化字段里的内容不被替换，不是上游字节的副本。
//
// # resp 为 nil 时返回空骨架
//
// nil 表示这次调用没有可脱敏的响应体：连不上、超时，或者上游回了非 2xx、
// 正文只留在 channel.ErrUpstream.Body 里而没有归一化。那一行仍然要落——
// 它的内容是 upstream_status 与 error_kind 两列，不是骨架（见
// gateway.Redactor 的接口注释）。
//
// 采集档照记：那一行的正文是空的，但「这一次是开着授权的」仍然是事实，
// 读的人要能把它与「没授权可开」分开。
//
// 这里不把 nil 交给 snapshot.Redactor：那个包把 nil 读作调用方的 bug，
// 而在这里它是一个正常结局。
func (c *Collector) RedactResponse(ctx context.Context, resp *channel.Response, id gateway.CaptureIdentity) (*gateway.Snapshot, error) {
	raw := c.rawCaptureAllowed(ctx, id)
	mode := captureModeFor(raw)
	if resp == nil {
		return &gateway.Snapshot{
			Skeleton:    map[string]any{},
			Version:     snapshot.SkeletonVersion,
			CaptureMode: mode,
		}, nil
	}
	if raw {
		res, err := c.responseRedactor.RedactResponseVerbatim(resp)
		return toSnapshot(res, err, mode)
	}
	res, err := c.responseRedactor.RedactResponse(resp)
	return toSnapshot(res, err, mode)
}

// rawCaptureAllowed 判这一次要不要留原文。
//
// 授权来源是装配进来的一个接口，本包不判它内部怎么算（见 ConsentSource）。
func (c *Collector) rawCaptureAllowed(ctx context.Context, id gateway.CaptureIdentity) bool {
	return c.consent.AllowsRawCapture(ctx, id.UserID, id.RequestID)
}

// captureModeFor 把判定结果落成落库用的取值。
//
// 两个取值与表上的 CHECK 一致（见 store.go 的两个常量）。
func captureModeFor(raw bool) string {
	if raw {
		return captureModeFullConsented
	}
	return captureModeSkeleton
}

// toSnapshot 把脱敏结果转成网关的快照类型。
//
// 只搬脱敏产出与采集档，不碰 request_id / user_id / direction 三个字段：
// 那些属于「这条快照属于谁」，由调用方（网关）填——脱敏器不写它们
// （它按这些字段判过授权，但那与「写进这一行」是两件事，见
// gateway.CaptureIdentity）。
//
// # 丢弃的计数为什么在这里
//
// 四个脱敏入口（两个方向 × 两条表面）都从这里出去，所以计数放在这一处就
// 不会有哪条表面漏记——decision.go 那两个入口也走它。
//
// 计数的是「连降级后的摘要层都装不下、整条被丢掉」这一种（ErrTooLarge），
// 而不是所有脱敏失败：其余失败各有各的原因（类型不认识、值没脱干净），
// 把它们并进同一个数会让「上限配错了」这件事在指标上失去形状。
//
// 必须有人记它：调用方（网关）拿到错误就直接返回，既不记日志也不上报
// （快照链路挂掉不该影响请求），于是「上限配小了」在 /metrics 与日志里都
// 看不见，而现场是「快照表里这一段时间是空的」——那看起来与「没有请求」
// 一模一样。
func toSnapshot(res *snapshot.Result, err error, mode string) (*gateway.Snapshot, error) {
	if err != nil {
		if errors.Is(err, snapshot.ErrTooLarge) {
			observe.Metrics.SnapshotOversized.Inc()
		}
		return nil, err
	}
	return &gateway.Snapshot{
		Skeleton:      res.Skeleton,
		RedactedPaths: res.Redacted,
		Truncated:     res.Truncated,
		Version:       res.Version,
		CaptureMode:   mode,
	}, nil
}

// Save 落一条快照。它是 gateway.SnapshotSink 的实现。
//
// # 并发满时丢弃，不排队
//
// 令牌拿不到就直接丢，日志里说明。**不阻塞等待**是刻意的：等的那个
// goroutine 会占着内存里的骨架、并与计费路径争同一批连接——过载时
// 「多救几条快照」的代价是拖慢真正在挣钱的那条路。
//
// 丢掉多少条是可观测的（Dropped），因为「静默地丢」与「没有请求」
// 在日志里长得一样。
//
// # 错误只给日志看
//
// 返回的错误供调用方记日志，网关**不看**它——快照链路挂掉不影响请求与计费。
// 两种失败分开报：容量不足是 ErrCapacityExceeded（既定行为，说明上限或流量
// 需要调整），写库失败是存储的问题。混成一句话会让日志里的「该调上限」
// 与「库坏了」分不开。
func (c *Collector) Save(ctx context.Context, snap *gateway.Snapshot) error {
	if !c.Collecting() || snap == nil {
		return nil
	}
	// request_id 为空时从 context 补上。
	//
	// 为什么由本包补：网关那一侧目前从自己的私有 context 键里取请求标识，
	// 而标识是由 observe 放进 context 的（两者不是同一个键），于是它取到的
	// 永远是空串。快照落库后 request_id 为空，等于「这条快照属于哪次请求」
	// 无从回答——而它正是快照的用途。
	//
	// 只补空值，不覆盖网关给的值：网关那边改好之后这一行自然不再生效，
	// 不需要两处同时改。
	//
	// 改的是一份副本而不是调用方的那个：骨架里的 map 是浅拷贝共享的，
	// 但这里只动 RequestID 这一个字段，调用方的快照不会被本包改写。
	local := *snap
	local.RequestID = requestIDOr(ctx, local.RequestID)

	select {
	case c.sem <- struct{}{}:
	default:
		c.dropped.Add(1)
		observe.Metrics.SnapshotDropped.Inc()
		c.loggerOrContext(ctx).Warn("快照并发已满，丢弃",
			"request_id", local.RequestID,
			"direction", local.Direction,
			"dropped_total", c.dropped.Load(),
			"limit", maxConcurrentWrites,
		)
		return ErrCapacityExceeded
	}
	defer func() { <-c.sem }()

	if err := c.store.Insert(ctx, &local); err != nil {
		c.failed.Add(1)
		observe.Metrics.SnapshotFailed.Inc()
		c.loggerOrContext(ctx).Error("快照落库失败",
			"request_id", local.RequestID,
			"direction", local.Direction,
			"err", err,
		)
		return err
	}
	c.written.Add(1)
	observe.Metrics.SnapshotWritten.Inc()
	return nil
}

// Stats 是这条链路的计数，供测试与装配层的用例使用。
//
// 有了它，「一条都没写进去」与「没有请求」才分得开：只看日志的话，
// 两者都是没有输出。生产里的出口是 /metrics 上那一族（observe 的
// snapshot_written / dropped / failed / oversized），见下面 Stats 的注释。
type Stats struct {
	Written int64
	Dropped int64
	Failed  int64
}

// Stats 返回当前计数。
//
// 它是这个 Collector **自己**的计数，与 /metrics 上那一族（observe 的
// snapshot_*）是同一批事件的两份口径，不能合成一份：
//
//   - 指标是进程级的累计，跨实例不可分；而这里要回答的是「**这一个**
//     Collector 写了多少」——装配层的端到端用例靠它等一条快照落库，用全局
//     计数器会把别的用例写进去的数也算进来。
//   - 指标按调用点自增，进不了测试里的中间状态；而这里随时读得到。
//
// 两边的自增点在同一处（Save 与两个 Redact 方法），所以不存在「一边涨了
// 一边没涨」的漂移——若哪天真要合并，合并的方向是把这份计数删掉、测试改用
// 一个自己 NewRegistry 造的注册表，而不是让指标去读它。
func (c *Collector) Stats() Stats {
	return Stats{Written: c.written.Load(), Dropped: c.dropped.Load(), Failed: c.failed.Load()}
}

// 编译期断言：装配出来的对象要能满足网关对脱敏器与落库层的要求。
//
// 两个断言都写在这里而不是各自的实现处：本包的存在意义就是这个适配，
// 它不成立时应该在这一行编译失败。
var (
	_ gateway.Redactor         = (*Collector)(nil)
	_ gateway.SnapshotSink     = (*Collector)(nil)
	_ gateway.DecisionRedactor = (*Collector)(nil)
)

// loggerOrContext 取出请求链路里的日志器，没有就用本包自己的。
//
// 走 context 是为了让快照的日志与产生它的那次请求带上同一个 request_id——
// 这条链路本来就是给排障用的，日志拼不上等于白记。
func (c *Collector) loggerOrContext(ctx context.Context) *slog.Logger {
	if id := observe.RequestIDFromContext(ctx); id != "" {
		return c.logger.With("request_id", string(id))
	}
	return c.logger
}

// requestIDOr 在已有取值为空时，从 context 里取请求标识。
//
// 见 Save 里的说明：网关那一侧取的键与 observe 写进去的不是同一个，
// 所以它交给这里的标识是空的。这一层把缺口补上，取值只来自 observe——
// 本包与网关不同，它本来就依赖观测层（Logger 就是从那里来的）。
func requestIDOr(ctx context.Context, existing string) string {
	if existing != "" {
		return existing
	}
	return string(observe.RequestIDFromContext(ctx))
}
