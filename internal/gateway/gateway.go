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
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/overrides"
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

// AliasOverrideSource 给出别名上配的参数覆盖。
//
// 声明在本包、实现在 internal/catalog——与 CandidateSource 同一个做法：
// 本包不认识 model_aliases 那张表，只要求一个答案。
//
// # 它为什么在网关层
//
// 参数覆盖改得了 max_tokens，而那是**预留上界**的关键。预留发生在选路之前
// （见 begin），那时还不知道会走哪个候选，所以上界要按整个候选集算——这件事
// 只有本包做得了：route 拿得到选中的那个渠道，但它已经是预留之后了。
//
// # 必填
//
// 与 route 的别名来源同一个理由：缺了它，预留会按调用方给的 max_tokens 算
// 上界，而别名上的覆盖可以比那个数更大——那时实际输出超过上界，结算撞上界
// 变成少收，且没有任何地方说得出原因。所以 nil 时请求失败（见
// ErrNoAliasOverrideSource），不退回一个「看起来能用」的默认值。
type AliasOverrideSource interface {
	// OverridesFor 返回这个规范名在给定渠道上各自生效的参数覆盖。
	//
	// 返回的 map 只含真有覆盖的渠道（见 internal/catalog 的 OverridesFor）；
	// 没有覆盖的渠道取值时得到零值，调用方按原值算。
	OverridesFor(ctx context.Context, canonicalModel string, channelIDs []int64) (map[int64]map[string]any, error)
}

// ErrNoAliasOverrideSource 表示没有配置别名覆盖的来源。
//
// 与 route.ErrNoAliasSource 分开：那是「规范名翻不出上游名」，这是「预留上界
// 算不出覆盖之后的输出上限」，两件事要对两行装配代码，报同一句话会让人去改
// 错地方。
var ErrNoAliasOverrideSource = errors.New("未配置别名覆盖来源：预留上界算不出覆盖之后的输出上限")

// ErrNoOutputCap 表示没有配置单次输出上限的全局默认值。
//
// 与 ErrNoAliasOverrideSource 同一条理由：缺了它那条判据会**静默消失**——
// 超限的请求照旧走原来的路（冻住一大笔余额，填得离谱时报成「算不出预留上界」），
// 而「没配上限」与「上限配得很宽」在请求上完全同形。装配层因此把它当必填项。
var ErrNoOutputCap = errors.New("未配置单次输出上限：目录里没登记输出上限的模型判不出超限")

// OutputCapExceeded 表示调用方声明的 max_tokens 超过了单次输出上限。
//
// 它是**调用方请求本身的问题**：把这个字段改小就能成功，所以 internal/api
// 把它映射成 400 而不是 500（见 httpStatusOfUpstream）。三个字段一路带到
// 那句话里：只说「超限」而不说允许到多少，调用方只能猜着改。
//
// 用它的是一个具体的类型而不是哨兵错误，因为给调用方的那句话需要这几个数，
// 而错误信息本身是中文的内部措辞（与仓库里其余错误一致）。
type OutputCapExceeded struct {
	// Model 是调用方要的那个规范模型名。
	Model string
	// Declared 是调用方在请求体里声明的 max_tokens。
	Declared int64
	// Limit 是这次请求允许声明的最大值。
	Limit int64
}

func (e *OutputCapExceeded) Error() string {
	return fmt.Sprintf("模型 %q 的单次输出上限是 %d，这次请求声明了 max_tokens=%d",
		e.Model, e.Limit, e.Declared)
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
	// AddReservation 给一次**已经落库**的预留追加冻结额。
	//
	// 它只服务于流式那条路：那条路不按声明的 max_tokens 冻满，而是边转发边
	// 按本地累计的真实产出补冻（见 stream.go 的 streamFreeze 与 #498）。
	// 一次补冻要把钱包的冻结额与账单行上的预留额**一起**加上——「冻了多少」
	// 的两个读者各读一处（准入判据读钱包，释放读账单行），只加一处会破掉
	// 对账的不变量（见实现里的说明）。
	//
	// 增量必须为正：非正的数在那里是一个真值错误的信号，静默当成 0 会让
	// 「补冻了但没冻上」看起来像成功。
	//
	// 余额够不着时返回的错误必须满足 errors.Is(err, pipeline.ErrInsufficientAvailable)
	// ——调用方据此切断那条流。其余错误（写库失败、钱包行不在）按「我们这边的
	// 故障」处置：放行，把这次请求标成未充分冻结。两类错误混成一个的话，一次
	// 入库超时会被当成「用户没钱」而切断一条本该跑完的流。
	AddReservation(ctx context.Context, req pipeline.Request, delta money.Amount) error
	// Release 释放一次预留。用于**不计费的结局**：上游失败、客户端中途断开。
	//
	// failure 是这次不计费的依据，由落库层写进账单的两列（上游状态码与
	// 归一化后的失败分类）；nil 表示没有「因上游失败而不计费」的依据。
	// 落库层要能接受 nil：「结算之前就返回了」那条路（客户端断开、
	// context 取消）走到这里时，上游那一次调用可能压根没发生，编不出依据来。
	//
	// 它必须是幂等的：同一次请求释放两次不该报错，也不该把冻结额减两次。
	Release(ctx context.Context, req pipeline.Request, failure *pipeline.UpstreamFailure) error
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
//
// # 两个方向回答的不是同一件事
//
// **请求方向**（direction = "request"）采于选路之后、那一次上游调用有了结局
// 之后（见 Chat 与 ChatStream 的 snapshotAsync 调用）。那类行上的
// UpstreamStatus 与 ErrorKind 恒为 nil——它读作「这一行不适用」，不是
// 「上游没回状态码」：这两个字段说的是**上游回来的什么**，而请求方向那一行
// 记的是发出去的东西（见 snapshotRequest）。
//
// **响应方向**（direction = "response"）采于上游那次调用有了结局之后：
// 非流式在 routeChat 返回之后、流式在那条流读完（或中断）之后。它回答
// 「回来的是什么」，两个字段因此有值（见 responseSnapshotAsync）。
// 两份是排查时配对读的：一份说发出去的是什么，一份说回来的是什么。
//
// # 与 billing_requests 那两列的关系
//
// 同一个事实，**各记各的**。取值口径与 derivation 是同一套（见
// upstreamEvidence、streamResponseEvidence 与 pipeline 的那组常量），但两列
// 回答的问题不同：快照的 ErrorKind 答的是「这次响应怎么了」，不看计不计费；
// 账单上的 error_code 在两种行上答两个问题——计费的行上它答的是
// 「钱收了，回答是不是在调用方不知情的情况下被截短」，不计费的行上答的是
// 「这次没向上游收钱，是不是因为上游这一侧出了事」（见 pipeline.Outcome）。
// 两列出现分歧时，以账单为准的是「钱为什么没扣」，以快照为准的是
// 「上游当时回了什么」。
//
// upstream_stream_truncated 是唯一在两表上都写的分类：那一次回答计了费（上游
// 已经产生了 token），而「回答只写了一半」是需要能事后读出来的——只写在快照上
// 的话，按账单核对金额的人看不到它（快照另有留存期，见
// docs/request-retention.md）。两边判据仍是同一个 streamTruncated，落点分别是
// 快照的 error_kind 与 billing_requests 的两列（经 pipeline.Settle 搬进
// Charge，由 store 的 markSettled 写入）。
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
	// Skeleton 是这条快照的正文。**它的形态由 CaptureMode 决定**：
	// skeleton 模式下是脱敏后的骨架（结构而不是内容），full_consented
	// 模式下是用户原文。读它的人必须先看 CaptureMode。
	Skeleton map[string]any
	// RedactedPaths 是被替换掉的字段路径，供确认白名单有没有漏放行项。
	// full_consented 模式下必然为空——那一档一个字段都没替换。
	RedactedPaths []string
	// Truncated 表示正文超限后降级成了摘要层。
	Truncated bool
	// Version 是骨架格式版本。
	Version int
	// CaptureMode 是这条快照按哪一档采的，取值与 request_snapshots.capture_mode
	// 的 CHECK 一致（skeleton / full_consented）；空串表示默认那一档。
	//
	// 它由脱敏那一侧（internal/snapshotwire）填，本包只搬运：这一档取决于
	// 「这次请求此刻有没有有效的原样采集授权」，而那个判定在脱敏那一刻做
	// （见 Redactor 的接口注释）。本包不判它——判定要查库，而那是数据面的事。
	CaptureMode string

	// UpstreamStatus 是这次调用**真的拿到过**的上游 HTTP 状态码，2xx 也填。
	//
	// nil 是唯一表示「没有上游状态码」的形态，含义有两层，且必须分开读：
	//   - 请求方向的行永远是 nil——这一路收不到状态码（它记的是发出去的
	//     那份请求，见类型注释），不是「上游没回」；
	//   - 响应方向的行上 nil 表示这次调用**一个 HTTP 响应都没拿到**：
	//     连不上、TLS 失败、超时、DNS 失败。它不是「忘了填」，也不是
	//     「上游回了 0 号」——HTTP 里没有 0 这个状态码。
	//
	// 指针而不是 0：request_snapshots.upstream_status 可空，而 0 会在库里
	// 落成一个真实存在的整数，把「没拿到」读成「上游回了 0」。
	UpstreamStatus *int

	// ErrorKind 是这次响应以上游失败告终时的分类，取值见 pipeline 的
	// ErrorUpstream* 常量（与 billing_requests.error_code 同一套词汇）。
	//
	// nil 表示「没有上游失败可归」：成功、客户端断开、插件阻断，以及
	// 一切不是上游那一侧出的错。两列的关系见类型注释。
	//
	// 取值与账单**同源**：同一个事件在两表上必须给同一个词，所以判据只有
	// 一处——快照侧是 streamResponseEvidence / upstreamEvidence，账单侧那条
	// 经 billedStreamEvidence 落到同一个 streamTruncated。两边各推一次的话，
	// 「快照说 A、账单说 B」会静默地发生——那正是 #461 修掉的形状。
	ErrorKind *string
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

// CaptureIdentity 是「这次请求是谁发的」，原样采集的授权判定要它。
//
// # 它只用于判定，不写进快照
//
// 一条快照属于谁仍由本包在脱敏**之后**补（见 snapshotAsync）。脱敏器不决定
// 归属这条理由没有变——但它现在必须**知道**归属，因为「要不要留原文」正是
// 按人授权的：授权记在 user_id（+ 某个 request_id 或一段窗口）上。
//
// # 两个字段都可能是零值
//
// 那时判定必须落到「不采集」：拿不到身份就不留原文，方向只能是这一个。
// 特别是 RequestID——本包今天取到的请求标识**永远是空串**（网关读的是自己
// 的 context 键，而标识是观测层放进另一个键的，两者的关系见 internal/snapshotwire
// 的 requestIDOr），所以「按单条请求授权的那个范围」在采集侧今天匹配不上。
// 那是一条已知缺陷、不在本档修复；留在这里是为了让接上它的那一天只改一处。
type CaptureIdentity struct {
	UserID    int64
	RequestID string
}

// Redactor 把请求或响应变成一条快照的正文。
//
// 它由 internal/snapshotwire 适配 internal/snapshot 实现。抽成接口是为了让
// 本包不依赖那个包的具体类型——那些类型会随白名单演进变化，而顺序不该跟着变。
//
// # 它按身份决定「留骨架还是留原文」
//
// 正文有两种形态（见 Snapshot.CaptureMode）：默认那一档是脱敏后的骨架，
// 另一档是用户明确授权后的原文。判定要看授权记录，所以两个方法都收
// CaptureIdentity——这正是把身份挪到脱敏**之前**的原因：脱敏怎么写取决于
// 这次判定，落库之后再挑就晚了（那时原文已经被丢掉）。
type Redactor interface {
	// Redact 脱敏一次请求。
	//
	// ctx 是这次请求的 context：判定可能要查一次库，客户端走掉时那次查库
	// 不必接着等。
	Redact(ctx context.Context, req *channel.Request, id CaptureIdentity) (*Snapshot, error)
	// RedactResponse 脱敏一次上游响应。
	//
	// resp 为 nil 表示这次调用**没有可脱敏的响应体**（连不上、超时，或者
	// 上游回了非 2xx、正文没有归一化成 channel.Response）。那时实现应当
	// 返回一条空骨架而不是错误：这一行仍然要落下来，它的内容是
	// UpstreamStatus 与 ErrorKind 两列，而不是骨架。
	RedactResponse(ctx context.Context, resp *channel.Response, id CaptureIdentity) (*Snapshot, error)
}

// Gateway 是一次请求的编排。
type Gateway struct {
	Pipeline     *pipeline.Pipeline
	Router       *route.Router
	Candidates   CandidateSource
	Reservations ReservationStore
	Settlements  SettlementStore

	// AliasOverrides 给出别名上的参数覆盖，**必填**——见
	// ErrNoAliasOverrideSource。装配层把它与 Router.Aliases 填成同一个
	// *catalog.Catalog：两处读的是同一张表，两个实例会让「选路时用的覆盖」
	// 与「算上界时用的覆盖」来自两次不同的读。
	AliasOverrides AliasOverrideSource

	// MaxOutputTokens 是单次输出上限的**全局默认值**（token）：模型目录里
	// 没登记 output_limit 的模型按它判，登记了的按目录那个数判
	//（见 checkOutputCap）。
	//
	// 必填且必须为正：零值不是「不限」。那样的话一个从没被填过的字段会静默
	// 放行一切，而它与「上限配得很宽」在请求上完全同形（见 ErrNoOutputCap）。
	//
	// 它是**我们的**界线，不是上游的能力：上游的输出上限是上游的事，这一条
	// 说的是我们愿意单次承担多少（一次请求最多冻住多少余额）。
	MaxOutputTokens int64

	// StreamFreezeStep 是流式请求**递进冻结**的阈值段（token）。
	//
	// # 它同时是两个量
	//
	// 流式那条路不按调用方声明的 max_tokens 冻满，而是每产出一个段就补冻一次
	// （见 stream.go 的 streamFreeze 与 #498）。所以这个数说的是「多久检查一次
	// 补冻」，同时就是「已冻额度最多领先已产出量多少」——调大它，数据库往返少、
	// 冻住的余量多；调小反之。**只有一个来源**：两个旋钮迟早会被配成互相矛盾的
	// 一对，而那种配置不报错，只是让「多久查一次」与「冻住多少」不再对得上。
	//
	// 流式请求必填且必须为正：零值不是「不限」——它要么让补冻整条不发生（于是
	// 冻结额不再跟着真实敞口走），要么把数据库往返摊到每一个事件上，两种都不会
	// 报错。非流式那条路用不到它（见 begin）。
	//
	// 装配期从 settings 的 billing.stream_freeze_step 读出来并校验（见
	// internal/assembly）：0 与负数在装配那一步就被拒。
	StreamFreezeStep int64

	// Redactor 与 Snapshots 可以都为空——那样就不采集快照。
	//
	// 这是刻意的默认：快照是可选能力，而不是「配错了就报错」的必填项。
	Redactor  Redactor
	Snapshots SnapshotSink

	// DecisionRedactor 是决策那条表面（见 decide.go）的脱敏器，可以为空。
	//
	// 与 Redactor 分开而不是合并：两者收的类型完全不同（channel.Request 与
	// systemone 的形状），合成一个接口会让每一个只服务其中一条路的实现
	// 都多出两个用不上的方法。见 DecisionRedactor 的注释。
	//
	// 它与 Snapshots **各自独立地为 nil**：留痕是排障辅助，装配层少接一个
	// 字段时的表现应当是「这条路径不采快照」，而不是请求失败。
	DecisionRedactor DecisionRedactor

	// ReleaseFailures 可以为空——那样释放失败不留痕（见 ReleaseFailureSink）。
	ReleaseFailures ReleaseFailureSink

	// Recorder 接收每一次上游调用的结局：渠道健康度与额度自记账。
	//
	// 可以为空——那样什么都不记。它是**可选**的接线，但装上之后它就不再是
	// 排障辅助：一个正在连续失败的渠道靠它被挡在候选集之外（见 recordCall）。
	Recorder CallRecorder

	// Executor 是真正调上游的东西。适配器实现它。
	Executor Executor

	// Decider 是调上游**决策接口**的能力（见 decide.go）。为 nil 时决策那条
	// 路径回 ErrNoExecutor，其余路径不受影响。
	//
	// 与 Executor 分开而不是合并：两者的签名不同（一个收 *channel.Request，
	// 一个收 *systemone.Request），合并只能靠再包一层类型断言或者往
	// channel.Request 里塞字段——前者把编译期的事推到运行期，后者正是
	// internal/systemone 要避免的。装配层通常把同一个 *upstream.Executor
	// 填进两个字段。
	Decider DecideExecutor

	// Counter 是流式中断时估算用量用的分词计数器。
	//
	// 可以为 nil——那时估算退到启发式（见 tokens.StreamEstimator），
	// 结果仍然可用，只是来源标成 SourceEstimated 而不是 SourceTokenized。
	// 非流式那条路用不到它：那里不估算——用量要么来自上游响应体，
	// 要么上游整块没报（记成 SourceUnreported，金额为零）。
	Counter *tokens.Counter

	// Now 可注入，便于测试时间相关的逻辑。
	Now func() time.Time

	// SettleTimeout 覆盖流式收尾（结算 + 释放）的时间预算。
	//
	// 零值表示用 streamSettleTimeout 那个常量——**不是**「不限」：收尾那一步
	// 写的是账本，没有上限时一次卡住的写库会把这笔钱吊在半空。
	//
	// 可注入，理由与 Now 相同：那一段预算要靠「一条跑得比它久的流」才验得到，
	// 而常量是十秒。用例把它压到毫秒级，形状不变（见 stream.go 的 settleTimeout）。
	//
	// 它只管**收尾**：流中间的每一次补冻各自配 5 秒（见 streamFreezeTimeout），
	// 与这个字段无关。
	SettleTimeout time.Duration
}

// Executor 是调上游的能力，由适配器组合而成。
//
// 与 route.Executor 分开：那个只要求非流式，而网关还要流式。
// 合起来会让「只想测选路」的地方也要实现流式。
type Executor interface {
	route.Executor
	route.StreamExecutor
}

// Hooks 是一次请求挂上来的插件钩子。零值表示这次请求不跑插件。
//
// # 形状为什么在这里、实现为什么在别处
//
// 声明在本包、实现在 internal/plugin：那个包不认识本包，所以由本包声明
// 它要调什么，装配处把两边对上（编译期就对上——api 把它当参数传进来）。
// 反过来让本包 import 插件包也能编译，但那样网关就依赖了插件包的注册表、
// 沙箱与配置，而它只该知道「有一个函数要在这一步被调」。
//
// # 为什么是参数而不是本包的字段
//
// 会话是**一次请求一个**的（它持有 moejs 运行时，且必须单 goroutine 用完），
// 而 Gateway 是长驻对象、被所有请求共用。挂在字段上就等于让并发请求共用
// 一个会话——那是数据竞争，而且是那种只在有插件时出现的竞争。
//
// # ChatStream 为什么也收它
//
// onRoute 与 onResponse 不一样：onResponse 只可能发生在非流式那条路上
// （流式是边生成边发的，没有「整份响应」这个时刻），而 onRoute 与选渠道是
// 同一件事，两条路都要走。只给非流式入口挂上，插件会对一半请求静默不生效
// ——那件事从响应、日志和账上都看不出来。
//
// 流式那条路上**独有**的挂载点是 onStream（逐块决定），它在非流式那条路上
// 不会被调到——非流式没有「块」可言。两个入口收的是同一份 Hooks，各自只调
// 自己那条路上会发生的那些。
type Hooks struct {
	// OnRoute 在候选**筛完之后、执行之前**调用一次，返回要钉在尝试顺序
	// 第一位的渠道。
	//
	// # 它拿到的是筛过的那一份，而且就是被执行的那一份
	//
	// 传进去的 candidates 与 route 接下来要执行的是同一个 route.Plan
	// （见 route.ChatPlan）。这件事有两个后果，都是要的：
	//
	//   - 决策者拿不到一个已经耗尽额度、被摘除或配置不全的渠道去选；
	//   - 它选的渠道一定在执行的那份候选里，于是「它选了一个渠道，请求却
	//     发去了另一个」这种静默分叉不可能发生。
	//
	// # 返回值
	//
	// 0 表示不决策，沿用内核排出来的顺序；非 0 表示把那个渠道钉在第一位，
	// 其余候选保持原顺序跟在后面（钉住 ≠ 只用它：它失败照旧换下一个）。
	//
	// 返回错误表示**决策方声明了阻断**（插件契约里的 meta.blocking）：
	// 这次请求失败。走的是与上游失败同一条路——一个候选都没试，不计费。
	//
	// 候选集**之外的渠道**轮不到这里处置：决策方在给出它之前就该挡下
	// （见 internal/plugin 的 pickChannel），因为那一侧才看得到「候选集是什么」。
	OnRoute func(ctx context.Context, candidates []route.Candidate) (int64, error)

	// OnStream 在流式响应的**每一个内容块**上调用一次，返回这一块怎么处置。
	//
	// # 只有内容块会走到这里
	//
	// 交给插件的只有 delta 与 tool_use 两种事件。usage 与 done / error 是
	// 这次流的**计费依据与框架**，不进插件的手：允许它丢掉 usage 就等于让
	// 插件绕过 onUsage 的那道上界（计费退回内核估算），丢掉 done 则会让一次
	// 正常结束看起来像被截断。这一条是**结构性**的——那几种事件根本不会被
	// 传进来（见 isContentChunk），不是靠插件自觉。
	//
	// # 每个块一次跨语言调用
	//
	// 延迟敏感：这条路上每一块都要进一次沙箱。所以本包不采样（采样会让
	// 「丢掉第 3 块」这类逐块决定失效），而插件没注册 onStream 时一次调用
	// 都不会发生——那个判断在 Session.OnStream 里，它先查 hook 名单。
	//
	// # 决策怎么落地
	//
	// Drop 的那一块不转发，也不进累计正文与分词计数；Replace 换掉的是这一块
	// 的正文，而累计正文与分词计数跟着换后的走（见 StreamChunk）；Terminate
	// 提前结束这条流。三者的收尾走的是**同一条**路径——与「上游结束 / 客户端
	// 断开」一起并进结局判定与计费分支，不另开一条绕过结算的出口。
	//
	// 返回错误表示决策方声明了阻断：这条流到此为止（已经吐给调用方的部分
	// 收不回来），收尾与 Terminate 相同。
	OnStream func(ctx context.Context, ev channel.StreamEvent, in StreamChunk) (StreamDecision, error)

	// OnResponse 在非流式响应拿到之后、结算之前调用。
	//
	// 返回错误表示插件声明了阻断：**这次请求失败**。契约里唯一的阻断旋钮是
	// 插件自己声明的 meta.blocking，不为任何 hook 位置开特例。
	//
	// 改的是要发给调用方的那份响应（补丁就地应用）。结算读的用量与它无关
	// ——ResponsePatch 里没有 usage。
	OnResponse func(ctx context.Context, resp *channel.Response) error

	// OnUsage 在用量成形之后、结算之前调用一次，返回结算该用的那一份用量。
	//
	// # 两条路上它都在同一个位置
	//
	// 非流式：上游响应的用量取出来之后、finishCharge 之前（排在 onResponse
	// 之后——那一步改的是要发给调用方的响应，与用量无关）。
	// 流式：读完事件流、streamUsage 之后、billStream 之前。
	//
	// 判据是同一句：**用量已经定了，钱还没算**。排在结算之后的话，插件上报
	// 的用量改不动这次账单——而请求照样成功、响应照样正确、日志里插件照样
	// 跑了，只有那笔钱是按内核的估算收的。
	//
	// # 返回值与错误
	//
	// 返回的用量就是结算用的那一份：插件没报的项保留内核的值（见
	// internal/plugin 的 MergeUsage），所以「没上报」不会被当成「归零」。
	//
	// 返回错误表示插件声明了阻断。**两条路的处置不同**，因为可表达的东西
	// 不同：非流式那条路上响应还没发出去，这次请求失败（与 onResponse 阻断
	// 同一条路，不计费）；流式那条路上整个回答已经送到调用方手里了，没有
	// 「让请求失败」这个对象，失败记进会话（由入口记成日志），用量按内核
	// 那份算——按不计费结算等于把一次已经送出去的回答白送。
	//
	// 出错时**返回的用量整个忽略**（两条路都一样）：让一个出错的决策方决定
	// 金额，方向正好反了——它多半交回来的就是零值。
	//
	// # outbound 是这次请求实际发出去的那份
	//
	// 它与 route.Result.OutboundRequest / route.StreamResult.OutboundRequest
	// 同一个口径：最后一次尝试真的交出去的那个请求对象（含别名上应用过的
	// 参数覆盖），一次都没发出去时是 nil。两条路给的都是**没退回**的那一份
	// ——退回调用方那份是采快照用的（见 snapshotRequest），不是这里。
	//
	// 它在这里交出去，是因为「这次调用真的允许生成多少」这件事只有网关在
	// 这一刻答得出来：入口层手上那份是调用方原样发来的，覆盖还没应用；而那
	// 份请求要等选路之后才存在。用量与它一起给，调用方就不必再去别处拼一份
	// ——拼第二份就是第二处，两处迟早不一致。
	OnUsage func(ctx context.Context, usage channel.Usage, outbound *channel.Request) (channel.Usage, error)
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
	// UpstreamModel 是这次请求**实际发出去**的上游模型名（规范名经别名翻过
	// 之后的那一个），取自选路的结果。
	//
	// 它与 Channel 同一个来源、同一类事实：选路之后才知道。它也是
	// billing_requests.upstream_model 的值——导出它是为了让测试与排查能看到
	// 「发出去的是什么」，而不必从库里反推。
	UpstreamModel string
	// Charge 是这次请求的结算结果。流式请求在事件流**关闭之后**才有值。
	Charge *pipeline.Charge

	// Attempts 记录了换渠道过程中的每一次尝试，含失败的。
	Attempts []route.Attempt

	// Estimated 为 true 表示用量**不是上游实测值**：客户端中途断开、
	// 上游在流中途失败、或上游正常结束却没带 usage。
	//
	// 它与 pipeline.Outcome.UsageSource 是同一个口径（见 tokens.Source）：
	// 只有 SourceMeasured 时它为 false。
	//
	// **非流式请求也可能为 true。** 那条路上用量不可能是本地估出来的，
	// 但上游**可以不报**：响应体里没有 usage 块时 usageOf 返回零值，
	// 而来源记成 SourceUnreported（见 usageSourceOf）。那两个零是占位，
	// 不是上游说的 0——这条判据要回答的正是「这个数是不是上游给的」。
	Estimated bool
}

// Chat 处理一次非流式请求。
//
// 顺序与 ChatStream 完全相同，差别只在用量从哪来：这里来自上游的响应体，
// 那里来自读完的整条事件流。两条路共用 begin / finish / finishCharge。
//
// hooks 为这次请求的插件挂载点，零值表示不跑插件（见 Hooks）。
func (g *Gateway) Chat(ctx context.Context, req pipeline.Request, upstream *channel.Request, hooks Hooks) (*Result, error) {
	if g.Executor == nil {
		return nil, ErrNoExecutor
	}

	res, err := g.begin(ctx, req)
	if err != nil {
		return nil, err
	}
	// 选路返回之前没有正常返回路径——只有 panic 会走到这里——所以这个兜底
	// 按零值 placement 收尾：那时确实还没有「打到了哪儿」。它守的是
	// 「预留一定被放开」这条不变量（见 handle.abort）。
	defer res.abort(ctx)

	// 请求快照要等选路返回才采得到：它记的是**发出去的那一份**请求，而上游名
	// 与别名上的参数覆盖都是逐渠道的，选路之前算不出来（见 snapshotRequest）。
	routed, placed, err := g.routeChat(ctx, upstream, res.candidates, req.CanonicalModel, hooks)
	// 选路一返回就把「打到了哪儿」拿到手，成功失败都一样：下面每一条
	// 收尾路径（结算与释放）都要收它，漏传编不过（见 placement）。
	// 它排在兜底那个 defer 之后注册，所以正常路径上先跑的是这一个。
	defer res.finish(ctx, placed)

	// 请求快照在这里采，用的是**真正发出去的那份请求**（见 snapshotRequest）：
	// 选路一返回才知道上游收到了什么——上游名与别名上的参数覆盖都是逐渠道的，
	// 在选路之前算不出来。
	//
	// 代价是这一行跟着那次上游调用一起晚落：上游挂住时它也不会先出现
	// （旧位置在调上游之前采，所以那时它已经开始写了）。换来的是快照记的
	// 确实是发出去的那份请求——旧位置记的是调用方发来的那份。
	g.snapshotAsync(ctx, snapshotRequest(routed, upstream), "request", req.UserID)

	// 响应方向的快照也在**这里**采：上游那次调用已经有了结局（成功或失败），
	// 而插件还没碰过响应——它记录的是上游回来的那份东西。失败也要采，
	// 那一条正是排查时最该看的。
	var routedResp *channel.Response
	if routed != nil {
		routedResp = routed.Response
	}
	respBody, respStatus, respKind := responseEvidence(routedResp, err)
	g.responseSnapshotAsync(ctx, respBody, req.UserID, respStatus, respKind)
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
		return nil, res.settleAndFail(ctx, placed, err, pipeline.Outcome{UpstreamFailed: true})
	}

	out := pipeline.Outcome{
		Usage:       usageOf(routed.Response),
		UsageSource: usageSourceOf(routed.Response),
		// 耗时取**最后一次尝试**的量：前面失败的那几次各是一次独立的调用，
		// 把它们的耗时加起来得到的不是「这个渠道多快」，而是「这次请求
		// 因为重试耽误了多久」。
		Duration: attemptDuration(routed.Attempts),
	}
	// 内核的用量留给下面记健康度用：out.Usage 在 onUsage 之后就未必是它了。
	kernelUsage := out.Usage

	// 插件在**结算之前**跑：它改的是要交给调用方的那份正文，而结算读的
	// 用量与它无关（ResponsePatch 里没有 usage）。位置选在这里而不是更靠后，
	// 是为了让「响应最终长什么样」在交给结算之前就定下来——结算之后再改，
	// 账单对应的那份响应就与调用方拿到的不是同一个了。
	if hooks.OnResponse != nil {
		if hookErr := hooks.OnResponse(ctx, routed.Response); hookErr != nil {
			// 阻断型插件失败：这次请求失败。与上游失败走同一条路——
			// 不计费、预留由 defer 的 finish 释放。
			//
			// 上游其实成功了、钱也花掉了，但仍然不向调用方收：他拿到的
			// 是一个错误，而不是那份回答。把一次没送出去的回答记成消费，
			// 在对账上表现为一笔解释不通的扣费。
			return nil, res.settleAndFail(ctx, placed, hookErr, pipeline.Outcome{UpstreamFailed: true})
		}
	}

	// onUsage 排在 onResponse **之后**、结算之前：用量已经定了，钱还没算
	// （五个挂载点的先后见 docs/plugin-contract.md 的「接线」一节）。
	//
	// 它改的是结算读的那份用量，而不是响应——所以位置反了不会有任何报错：
	// 请求成功、响应正确、日志里插件也跑了，只有那笔钱是按内核的估算收的。
	//
	// 记给渠道的那一份（recordAttempts，下面）取的是**内核的**用量：这一路
	// 记的是这个渠道自己消耗了什么（健康度与额度自记账），而插件上报的是
	// 计费口径上的说法。两者不一致时，以谁为准是插件契约里明确划过的：
	// 上报只改计费，不改上游实际发生了什么。
	if hooks.OnUsage != nil {
		usage, hookErr := hooks.OnUsage(ctx, out.Usage, routed.OutboundRequest)
		if hookErr != nil {
			// 阻断型插件失败：这次请求失败。与非流式那条路上的 onResponse
			// 同一个处置——响应还没发出去，所以「失败」有可表达的对象。
			return nil, res.settleAndFail(ctx, placed, hookErr, pipeline.Outcome{UpstreamFailed: true})
		}
		out.Usage = usage
	}

	charge, err := g.finishCharge(ctx, res, placed, out)
	if err != nil {
		// 结算没成，用量事件也就没落库。健康度仍然要记：这些调用真的发生过，
		// 成功率的分子分母不该因为一次结算失败而少掉一次。
		//
		// 这里传给写入方的是 out.Usage 而不是内核那份：这一次没有用量事件，
		// 缓存与延迟两列不会被重算，而自记账用的是**计费口径**的用量——
		// 与结算失败时我们能给出的唯一一份数一致。
		g.recordAttempts(ctx, routed.Attempts, &out.Usage)
		return nil, err
	}

	// 健康度排在结算**之后**：延迟那一维的样本来自 usage_events.duration_ms，
	// 而那一行是结算事务写的。提前记的话，这个桶的分位数里不含这一次
	// ——它要等下一次调用才被带进去，于是那个数总落后一个请求。
	//
	// 用量取内核那份（out.Usage 在 onUsage 之后可能已被插件改过，理由同上）。
	g.recordAttempts(ctx, routed.Attempts, &kernelUsage)

	return &Result{
		Response:      routed.Response,
		Channel:       routed.Channel,
		UpstreamModel: routed.UpstreamModel,
		Charge:        charge,
		Attempts:      routed.Attempts,
		Estimated:     out.UsageSource != tokens.SourceMeasured,
	}, nil
}

// routeChat 选路并执行一次非流式请求。
//
// 它同时交回「打到了哪儿」（placement）——那是每条收尾路径的必填参数
// （见 placement）。结果与它在同一处取出，于是两者不会分叉。
//
// 有插件挂载点时走的是两步：**先筛**（Select），把筛过的候选交给插件，
// 再拿它给的渠道去执行（ChatPlan）。这两步必须由 route 自己做——本包
// 复现一遍筛选就是第二份实现，两份迟早不一致（见 route.ChatPlan）。
//
// 没有挂载点时走的还是原来那一条路（Chat），一步到位。这个分支不是优化：
// 插件默认是关的，绝大多数部署永远走这一条，让它与插件系统出现之前
// 逐字相同，是「插件关着时行为不变」最省事的证明方式。
//
// # 交给插件的顺序不是这里的顺序
//
// 插件拿到的候选集**按 channelId 重排过**（`internal/plugin` 的
// `sortedByChannelID`，排的是副本），所以它那边的 `candidates[0]` 不是内核
// 评分最高的那个。这一层不依赖两份顺序一致：插件给回来的只是一个 channelId，
// 由 ChatPlan 按 ID 在**内核自己那份** plan 上找（见 route.Plan.pinFirst）。
// 反过来说，任何「插件看到的下标 == 内核的下标」的假设都不成立。
func (g *Gateway) routeChat(ctx context.Context, upstream *channel.Request, candidates []route.Candidate, canonicalModel string, hooks Hooks) (*route.Result, placement, error) {
	if hooks.OnRoute == nil {
		res, err := g.Router.Chat(ctx, g.Executor, upstream, candidates, canonicalModel)
		return res, placementOf(res), err
	}
	// 这条路上本层自己筛（不走 Router.Chat），所以判据要显式给一遍：
	// 与另一条路上的 route.Chat 用的是同一个 NeedsOf，插件看到的就是
	// 内核会执行的那一份候选。
	plan := g.Router.Select(candidates, canonicalModel, route.NeedsOf(upstream))
	pin, err := hooks.OnRoute(ctx, plan.Candidates)
	if err != nil {
		// 阻断型插件失败：这次请求失败。一个候选都没试过，也没有任何
		// 上游调用发生过，所以不计费（调用方按「上游失败」处置）。
		return nil, placement{}, err
	}
	res, err := g.Router.ChatPlan(ctx, g.Executor, upstream, plan, pin)
	return res, placementOf(res), err
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

	// finished 表示收尾（结算 + 释放）已经跑过。
	//
	// 它让 abort 与 finish 两条 defer 可以同时挂着而不重复收尾：选路之前
	// 那个兜底在正常路径上也会执行，而那时 finish 已经把该做的做完了。
	finished bool

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

	// failure 是这次不计费的依据，跟着 release 一起落库。
	//
	// 它由 settleFailure 从结算回给的那份结局里取，而不是由各个失败分支
	// 各自记：分支有四处（上游非 2xx、连不上、流中途断、插件阻断），
	// 散着记就是四处都要记得写一次，而漏掉的那一处表现为「这一类失败在
	// 账单上仍然没有依据」——与修之前一模一样，且不会有任何东西报出来。
	failure *pipeline.UpstreamFailure
}

// placement 是这次请求**实际打到了哪儿**：哪一个渠道、用哪个上游模型名。
//
// # 它为什么是一个值，而不是选路之后的一行赋值
//
// 这两个值要同时写进成功与失败的账单行：成功走 finishCharge，失败走
// settleAndFail 与 finish（释放）。早先它们由选路返回后调一次 routedTo
// 记进 h.req，再由那三处去读——那是一条**位置约定**。新增一个入口时忘了
// 那一行，它那条路的失败账单行又是两列 NULL，而且不会有任何东西报出来：
// 失败行上 NULL 与「确实没有渠道可记」在库里逐列相同。
//
// 现在它是那三处的**必填参数**，并且由选路的包装交回（见 routeChat /
// routeStream / routeDecide）。忘了把它交给任何一条收尾路径，就编不过——
// 「漏记」不再是一种可表达的写法。
//
// # 零值
//
// 零值表示「没有打到任何渠道」：候选集为空、别名翻不出来、context 已取消。
// 那时两列留 NULL 是正确答案，不是漏记（见
// TestReleaseWithoutUpstreamCallCarriesNoEvidence）。
//
// # 新增入口时
//
// 新的 Chat / ChatStream / Decide 形态要照三件事做：
//
//  1. 选路经一个同时交回 placement 的包装（routeChat / routeStream /
//     routeDecide），不要直接调 g.Router 再把结果自己拼起来；
//  2. 把这个值交给入口的**每一条**收尾路径：成功结算（finishCharge）、
//     不计费的结算（settleAndFail）与释放（finish）。
//  3. 第 2 步漏了会编不过；第 1 步漏了只会得到零值，而那在「一个候选中
//     都没选中」时也是正确答案——所以新入口至少要有一条用例覆盖「上游
//     失败」，断言释放时带的渠道与上游名不是零值（照
//     TestUpstreamFailureCarriesEvidenceToRelease 写）。
type placement struct {
	channelID     int64
	upstreamModel string
}

// placementOf 从一次非流式选路的结果里取出 placement。
//
// 结果为 nil 时返回零值：一个候选都没选中，确实没有渠道可记。
// 失败路径上结果通常不是 nil（route 会带上最后尝试的那个渠道），这正是
// 「上游回了 4xx 的那一次明明打到了某个确定的渠道」能写进账单的原因。
func placementOf(r *route.Result) placement {
	if r == nil {
		return placement{}
	}
	return placement{channelID: r.Channel.ID, upstreamModel: r.UpstreamModel}
}

// streamPlacementOf 与 placementOf 同一件事，只是流式结果的类型不同。
func streamPlacementOf(r *route.StreamResult) placement {
	if r == nil {
		return placement{}
	}
	return placement{channelID: r.Channel.ID, upstreamModel: r.UpstreamModel}
}

// routedTo 把 placement 记到这次请求上。
//
// 它只有三个调用点，都是**钱的出口**：settleAndFail、finishCharge、finish。
// 三个都要收 placement 才调得起来（见 placement 的注释），所以「忘了记」
// 在编译期就不成立，而不是靠「选路返回后记得调一次」这种位置约定。
// **新增一个入口时要做什么，见 placement 的「新增入口时」一节。**
//
// # 它为什么记在 req 上
//
// billing_requests 的那两列有两个写入点：计费走 markSettled，不计费走
// Release。两者读的都是**同一份请求**（前者是 finishCharge 从 h.req 拷出来
// 的那份，后者就是 h.req 本身），所以「这次打到了哪儿」只有记在 h.req 上
// 才对两条路同时生效。
//
// 不记的话，成功那一行两列有值而失败那一行恒为 NULL——恰恰是「某个渠道对
// 某批模型全在报错」时最需要它们的地方没有它们。**这不是「没选中渠道就不记」**：
// 上游回了 4xx 的那一次明明打到了某个确定的渠道。
func (h *handle) routedTo(p placement) {
	h.req.ChannelID = p.channelID
	h.req.UpstreamModel = p.upstreamModel
}

// # 它为什么不收上游请求
//
// 早先它收一个 *channel.Request，只为在最后采一次请求快照。那条表面之外的
// 调用方（决策，见 decide.go）没有 chat 形状的请求，而「为了采快照把一个
// 不适用的类型传进来」正是本包要避免的事。快照改由各条入口自己采——采集点
// 跟着各自那条路的选路走（见 snapshotRequest）。
func (g *Gateway) begin(ctx context.Context, req pipeline.Request) (*handle, error) {
	// 单次上限的兜底值没配时直接失败，而不是跳过后面的判据：见 ErrNoOutputCap
	// ——「判据没接上」与「这次请求没超限」在请求上长得一样，那是这一类改动
	// 最容易留下的东西。
	if g.MaxOutputTokens <= 0 {
		return nil, ErrNoOutputCap
	}

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

	// 单次上限判在**预留之前**：超限的请求一分钱都不该冻。排在候选之后是因为
	// 上限要按模型目录那条路径取（见 checkOutputCap 的 outputLimit），而候选
	// 里带着目录声明。
	// 变量名不复用外层那个 err：同一个块里再 `err :=` 是遮蔽，govet 的
	// shadow 检查器会报（那个 err 在下面还要用）。
	if capErr := g.checkOutputCap(req, cands); capErr != nil {
		return nil, capErr
	}

	// 预留按**整个候选集**取上界：每个候选带它自己的商家，规则那一侧对
	// 每个候选各取一条，取其中金额最大的那个（见 pipeline.Reserve）。
	// 只给渠道 id 是不够的——商家级规则按 owner 匹配。
	//
	// 这里还不能收窄成一个渠道：选路发生在预留之后。结算时才按实际选中
	// 的那一个重新匹配——见 finishCharge。
	req.Candidates = ruleCandidates(cands)

	// 别名上的参数覆盖**也**按候选集取上界，而且必须在预留之前——理由
	// 见 outputBound。
	//
	// 注意它对**决策表面**（/v1/systemone）同样生效：那条路上游收到的请求
	// 里根本没有 max_tokens 这个字段（见 route.ChatModel 与 decideCall），
	// 而这里会按别名上配的覆盖去算上界，于是那笔预留可能按一个这次请求根本
	// 不带的上界冻住余额。**不会少收**（结算按上游实际报的用量），只是可能
	// 多冻——余额紧的时候决策请求会因此撞上「余额不足」，而原因在那条请求上
	// 看不出来。要收窄的话，就是让决策路径不走这一段。
	req.MaxTokens, err = g.outputBound(ctx, req, cands)
	if err != nil {
		return nil, err
	}

	// 预留：流式按**第一段**冻，非流式按声明的整个上界冻。
	//
	// 收窄的只有「这次冻多少」这一个量，req.MaxTokens 本身**不动**：它表达的
	// 是这次请求允许输出到多少，结算（pipeline.Settle 的 req.validate）与
	// 上游那份请求都按它走。把它改成第一段等于顺手改掉了上界——而发出去的
	// 请求与上界是两个量（见 outputBound）。
	//
	// 流式那条路的第一段由 pipeline.ReserveStream 取 min(max_tokens, 阈值段)：
	// 这个 min 只许有那一处，网关照着 Reservation.OutputTokens 读回来就是
	// 「已经冻了多少个输出 token」（见 stream.go 的 streamFreeze）。
	var reserved *pipeline.Reservation
	if req.Streamed {
		// 非正的阈值段在装配期就被拒（见 assembly），这里是第二道：网关是
		// 公开类型，测试与装配都能自己造一个。零值不是「不限」，见字段的注释。
		if g.StreamFreezeStep <= 0 {
			return nil, pipeline.ErrNoFreezeStep
		}
		reserved, err = g.Pipeline.ReserveStream(ctx, req, g.StreamFreezeStep)
	} else {
		reserved, err = g.Pipeline.Reserve(ctx, req)
	}
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

	return h, nil
}

// checkOutputCap 判这次请求声明的输出上界有没有超过单次上限。
//
// # 判的是调用方声明的那个数，不是别名覆盖之后的那个数
//
// 别名上的 max_tokens 覆盖会**顶掉**调用方给的那个数（见 outputBound），于是
// 「调用方填 8K、别名覆盖成 100K」这种请求有两种判法。这里判**调用方声明的
// 那个数**，也就是说覆盖不参与这条判据。三条理由：
//
//   - 给调用方的那句话要能照做。超限时我们告诉他「你声明的 max_tokens 超过了
//     允许的 N」——那是他写的字段，改小就能成功。按覆盖之后的数判的话，他收到
//     的会是「100000 超过上限 8192」，而 100000 他从没写过：那句拒绝指向一个
//     不存在的输入。
//   - 覆盖是**商家配置**，配错了责任与后果都落在配置那一侧。按覆盖之后的数判
//     的话，别名上写一个超过上限的数会让该别名下的**每一个**请求都收到 400，
//     而调用方改请求是没用的——他做什么都通不过，只能等商家去改别名。
//   - 与选路那条目录判据同一口径：route 比的就是调用方给的
//     channel.Request.MaxTokens（见 route.NeedsOf），两处同进同出，不会出现
//     「一条说超了、另一条说没超」。
//
// 代价要说清楚：调用方在上限之内、而别名覆盖把它抬到上限之上时，这次请求
// **不会**在这里被拒，预留仍按覆盖之后那个数冻（见 outputBound）。那一笔多冻
// 出来的钱是商家配置的结果，要改的是那条别名——不是把调用方的请求拒掉。
//
// # 两个豁免
//
//   - 决策那条表面（ProtocolSystemOne）：它的 MaxTokens 是我们按上游预算写的
//     常量（见 internal/api 的 decideBillingRequest），调用方从来没有声明过它。
//     拿一个我们自己填的数去拒调用方的请求，他不知道该改什么。
//   - 流式：流式有中途观测点，钱的处置是随流递进冻结，与这条判据不是一回事
//     （见 stream.go 与 #498）。
//
// # 非正数的 max_tokens 不在这里判
//
// 它落在下面那条 `req.MaxTokens <= limit` 之外（非正数一定不大于一个正的
// limit），继续走 pipeline.Request.validate 与 billing.Reserve 那条路报
// ErrNoUpperBound：措辞与状态码都更准（「必须为正」比「超过上限」说的是同一件
// 事的两个方向）。
func (g *Gateway) checkOutputCap(req pipeline.Request, cands []route.Candidate) error {
	if req.Protocol == channel.ProtocolSystemOne || req.Streamed {
		return nil
	}
	limit := g.outputLimit(cands)
	if req.MaxTokens <= limit {
		return nil
	}
	return &OutputCapExceeded{Model: req.CanonicalModel, Declared: req.MaxTokens, Limit: limit}
}

// outputLimit 是这次请求的单次输出上限：模型目录里登记了就用它，否则用全局
// 默认值（见 Gateway.MaxOutputTokens）。
//
// 目录那一列是**模型级**的（同一规范名下的候选拿到同一份 Profile，见
// route.ModelProfile），所以候选之间取值相同。取其中**最小的那个已登记值**，
// 而不是随手取第一个：将来真改成逐候选时，这条判据会自然落到严的一侧。
//
// 0 表示**未知**（库里是 NULL），不是「零额度」——那与「目录里没有这个模型」
// 一样落到全局默认值（见 route.ModelProfile 对两个上限列的处置）。
func (g *Gateway) outputLimit(cands []route.Candidate) int64 {
	limit := int64(0)
	for _, c := range cands {
		if c.Profile == nil || c.Profile.OutputLimit <= 0 {
			continue
		}
		if limit == 0 || c.Profile.OutputLimit < limit {
			limit = c.Profile.OutputLimit
		}
	}
	if limit == 0 {
		return g.MaxOutputTokens
	}
	return limit
}

// outputBound 算出这次请求的输出 token 上界：候选集里**覆盖之后**的
// max_tokens 的最大值。
//
// # 它为什么必须在这里
//
// 别名上的 max_tokens 覆盖会**顶掉调用方给的那个数**，而预留的输出部分正是
// mul(输出单价, max_tokens)（见 internal/billing）。上界按调用方那个数算，
// 而实际发出去的请求允许生成得更多时，结算会撞上预留的上界——差额被截掉，
// 表现为**少收**，且没有任何地方说得出原因：账上那一行看起来完全正常。
//
// 预留发生在选路之前，那时还不知道会走哪个候选，所以与规则那一侧同一个做法：
// 按整个候选集取最大（见 begin）。逐个候选算的收窄需要一个「预留时就知道选中
// 谁」的次序，而那与「按候选集取」是同一条既有约定，不在本次改动里翻。
//
// # 取值就是覆盖之后的那个数，不拿调用方的数当下限
//
// 覆盖比调用方给的小时，上界跟着变小——这是对的：上界要跟着**实际发出去
// 那份请求**走，而那份请求带的是覆盖值。保留调用方那个数当下限会让「商家把
// 输出限制调小」反而冻住调用方更多的钱，而那是这一列本不该有的副作用。
//
// 这条口径与插件那条路今天的处置**不同**（插件只许把 max_tokens 调小，因为
// 它的补丁改不到预留上界），差异是有意的：覆盖在预留之前就被读到了，所以它
// 不需要那条约束。谁把插件补丁的边界往这边对齐之前，要先想清楚这一点。
//
// # 与 route 的关系
//
// 真正发出去的那份请求上应用的覆盖在 route 里（upstreamRequest），它用的是
// **选中那个渠道**的覆盖——与这里的口径不同（这里只求上界）。两处都从
// internal/overrides 取值，语义只有一份。
func (g *Gateway) outputBound(ctx context.Context, req pipeline.Request, cands []route.Candidate) (int64, error) {
	if g.AliasOverrides == nil {
		return 0, ErrNoAliasOverrideSource
	}

	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Channel.ID)
	}
	byChannel, err := g.AliasOverrides.OverridesFor(ctx, req.CanonicalModel, ids)
	if err != nil {
		return 0, fmt.Errorf("读取别名的参数覆盖失败: %w", err)
	}

	// 初值取零而不是调用方那个数：下面每个候选都会给它一个「覆盖之后」的值
	// （没配覆盖时就是调用方那个数），所以零只在与候选集为空时取到——而那在
	// 更前面就被拦下了（见 begin）。这个零值一旦漏出去，表现是 MaxTokens = 0
	// 被下游拒绝，而不是一个静默的小上界。
	// 窄化成 int 是因为覆盖那一层按 int 收（它给的是请求体里的那个数）。
	// 今天所有入口的 max_tokens 都是从 int 来的，所以无损；32 位构建上会
	// 截断，而截断成负数的表现是下面给出 0，请求以「算不出上界」失败——
	// 不是静默的小上界，所以这里不做范围检查，只记一笔。
	var bound int64
	for _, id := range ids {
		effective, err := overrides.EffectiveMaxTokens(int(req.MaxTokens), byChannel[id])
		if err != nil {
			return 0, fmt.Errorf("渠道 %d 上的别名覆盖算不出输出上界: %w", id, err)
		}
		if int64(effective) > bound {
			bound = int64(effective)
		}
	}
	return bound, nil
}

// finish 是 handle 的收尾。它总是被 defer 调用，负责把还没结算的那次
// 补上——正常路径已经结算过，这里会发现并跳过。
//
// 它也是**唯一**释放预留的地方。放在这里而不是各个失败分支上，理由与
// handle 本身存在的理由相同：散在分支上的规则，漏一处就是一笔永久冻着
// 的钱，而漏掉的那一处不会有任何东西报出来。
//
// placed 是这次请求打到了哪儿，释放把它一起交给落库层（见 placement）。
// 它是必填参数：调用点从选路结果上拿到它，忘了传编不过。
func (h *handle) finish(ctx context.Context, placed placement) {
	if h.finished {
		return
	}
	h.finished = true
	h.routedTo(placed)
	if !h.settled {
		// 走到这里说明在结算之前就返回了（客户端断开、context 取消等）。
		// 按「上游失败」结算：不计费。
		//
		// 这条路上没有「因上游失败而不计费」的依据：走到这里说明还没到能
		// 判定上游结局的那一步（多半是上游那一次调用压根没发生），所以
		// Settle 回出来的那份结局两列都是零值，settleFailure 会把它丢成 nil。
		h.settleFailure(ctx, pipeline.Outcome{UpstreamFailed: true})
	}
	h.release(ctx)
}

// abort 是选路**之前**的兜底收尾。
//
// 选路返回之前没有正常返回路径（只有 panic 会走到这里），那时还没有
// 「打到了哪儿」可记，所以按零值 placement 收尾——与它取代的那条
// defer 是同一个位置，守的也是同一条不变量：预留一定要被放开。
//
// finish 跑过之后它是空操作（见 handle.finished）：正常路径上两条 defer
// 都会执行，重复释放不该发生。
func (h *handle) abort(ctx context.Context) {
	h.finish(ctx, placement{})
}

// settleAndFail 结算一次失败的请求，并把原始错误原样交回调用方。
//
// placed 是这次请求打到了哪儿（见 placement）：失败账单行上的渠道与上游名
// 靠它写进去。它是必填参数，调用点从选路结果上拿到它。
//
// 失败不计费，所以这里用 UpstreamFailed。预留的释放交给 finish——
// 它是**每一个**返回路径的收尾点（见 finish 的注释），在这里再调一次
// 只会多一次数据库往返。
//
// # 失败依据在这里判，不在调用点判
//
// 上游状态码与失败分类由 cause 推出来（见 upstreamEvidence），**调用点
// 传进来的 Outcome 里那两项会被覆盖**。这个函数有五个调用点
// （gateway.go 三处、stream.go 两处），各判一次的话判据就有五份，而漏判的
// 那一处表现为「这一类失败在账单上没有依据」——库里那一列是 NULL，与
// 「这一批恰好没有失败」长得一样，没有东西会报出来。
//
// 判据虽然只有一处，它要覆盖的是**四类失败**：上游非 2xx、连不上、
// 流中途断、插件阻断（见 upstreamEvidence 的分支与 handle.failure 的注释）。
//
// # 它为什么只回错误、不回结果
//
// 它从来只回 (nil, cause)。早先的签名是 (*Result, error)，于是调用方写成
// `return res.settleAndFail(...)`——读起来像「返回一个结果」，实际返回的
// 永远是 nil。决策那条路（见 decide.go）的产出是 *DecideResult，那个签名
// 直接把这件事变成了编译错误。去掉结果那一位之后，「失败没有结果」是
// 签名上看得见的事。
func (h *handle) settleAndFail(ctx context.Context, placed placement, cause error, out pipeline.Outcome) error {
	h.settled = true
	h.routedTo(placed)
	// 依据**无条件覆盖**，调用方传来的那两个字段在这里被丢掉：失败这一条路上
	// 依据只能来自 cause（这次结算认下的原因是它），让调用方自己填就有了两个
	// 来源。调用方设定本包这一对值的地方只有计费那条路（见 stream.go 的
	// billedStreamEvidence）。
	out.UpstreamStatus, out.ErrorCode = upstreamEvidence(cause)
	h.settleFailure(ctx, out)
	return cause
}

// settleFailure 结算一次不计费的请求，并把这次失败的依据记在 handle 上。
//
// 依据取自 Settle 回给的那份结局（*pipeline.UpstreamFailure），而不是在这里
// 另存一份 cause：Outcome 里那两个字段是 Settle 读进去的，它回出来的
// UpstreamFailure 就是「这次结算认下的依据」——两处各记一份，迟早对不上。
//
// 结算回别的错误时（规则坏了、缺少受理时刻）依据保持 nil：那是我们这边的
// 失败，不是上游的，两列该留空。
func (h *handle) settleFailure(ctx context.Context, out pipeline.Outcome) {
	_, err := h.g.Pipeline.Settle(ctx, h.req, h.reserved, out)
	var failure *pipeline.UpstreamFailure
	if !errors.As(err, &failure) {
		return
	}
	// 两列都没有值时不留下这个对象，而是留 nil。
	//
	// 落库层对两者一视同仁（都写成 NULL），但**接口上「有没有依据」只能有
	// 一个答案**：一个零值的 UpstreamFailure 与 nil 表示同一件事，却让每个
	// 读它的人（包括调用方与用例）都要记得两种都判一次，而漏判的那一处
	// 不会报错——它只是把「没有依据」当成「有依据，值是 0」。
	if failure.Status == 0 && failure.Code == "" {
		return
	}
	h.failure = failure
}

// upstreamEvidence 从一次失败的成因里取出写进账单的两列依据。
//
// 只有**上游这一侧**的失败才有依据，判据是错误链里有没有
// channel.ErrUpstream（适配器造它、路由层原样带回，见 route.Errors.Unwrap
// ——它给的是**结束这次请求**的那一次尝试的错误，也正是调用方看到的那个）。
//
// # 「拿到过响应吗」是显式的事实
//
// 它取自 ErrUpstream.GotResponse，**不从 Body 反推**：响应体有没有内容取决于
// 上游写没写、适配器截没截，它答不了这个问题。谁哪天给网络错误也塞上一个
// Body，按 Body 判的话分类就会静默翻面。
//
// 四种情形分开写，因为它们在两列上的落法不同：
//
//   - 非 2xx：状态码就是上游回的那个，分类是「上游回了错误状态码」
//   - 拿到了 2xx，但体里是错误 / 体解不开：状态码**照实记**（通常是 200），
//     分类是「上游的响应不可用」。这里不能把状态码丢掉——200 是这次调用
//     真实发生过的事实，丢掉会让库里那一行读成「没有记录到状态码」。
//   - 一个响应都没拿到（连不上、TLS 失败、超时）：状态码留空，分类是
//     「上游没给出可用的回答」。**不写 0**：HTTP 里没有 0 这个状态码，
//     写 0 会读成「上游回了 0 号」，而 0 是「没有这回事」
//   - 上游接受了请求却一个字都没吐（流式）：适配器在流的第一条事件里
//     交下了那个 2xx，状态码照实记；分类是「流在接受之后断了」。这次的
//     **结局**由分类表达，状态码表达「上游回过什么」——两列各司其职。
//
// 其余错误（插件阻断、别名没配、结算自己出错）返回空值：两列记的是上游
// 失败的依据，把它们写进去会让「这一行是不是上游挂的」答错。
func upstreamEvidence(err error) (status int, code string) {
	if errors.Is(err, errStreamAborted) {
		// 有状态码就记（streamAbortedError），没有就留空（裸哨兵）：
		// 后者的 status 是 0，编一个 200 出来就是假数据。
		var aborted *streamAbortedError
		if errors.As(err, &aborted) && aborted.status > 0 {
			return aborted.status, pipeline.ErrorUpstreamStreamAborted
		}
		return 0, pipeline.ErrorUpstreamStreamAborted
	}

	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		return 0, ""
	}
	if !up.GotResponse {
		return 0, pipeline.ErrorUpstreamNoAnswer
	}
	if up.StatusCode <= 0 {
		// 说了拿到响应却没给状态码，是构造方的 bug（见 ErrUpstream.GotResponse
		// 的不变量）。归到「没拿到可用的回答」而不是编一个状态码：这是唯一
		// 不会往库里写假数据的落法。
		return 0, pipeline.ErrorUpstreamNoAnswer
	}
	if up.StatusCode >= 200 && up.StatusCode < 300 {
		return up.StatusCode, pipeline.ErrorUpstreamUnusableResponse
	}
	return up.StatusCode, pipeline.ErrorUpstreamHTTPError
}

// release 把这次请求冻住的钱放开。
//
// 它是不计费那一侧**唯一的上游失败依据落点**：不计费的结局不写账本，账单
// 那一行上「上游回了什么、为什么不计费」两列只有这一次写入机会（见
// h.failure）。计费那一侧另有自己的落点（静默截断经 markSettled 写入，见
// internal/ledger/store）。把依据挂在这里而不是另开一次写库，是因为这一行
// 本来就只有这一条 UPDATE——两次写之间会有一个「已经 released 但没有理由」
// 的窗口。
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
	if err := h.g.Reservations.Release(relCtx, h.req, h.failure); err != nil {
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

// settledRequest 是**结算口径**的那份请求：把「这次请求实际打到了哪儿」记上，
// 再把候选集收窄成选中的那一个渠道。
//
// # 它为什么是一处而不是两处
//
// 有两个调用点要的就**是**「结算会用的那份请求」：
//
//   - finishCharge：结算按实际选中的渠道匹配规则（见那里的注释）；
//   - 流式的递进补冻（见 stream.go 的 streamFreeze）：补冻的单价必须与结算
//     同源（见 pipeline.ReserveMore），所以它要的输入与结算逐字相同。
//
// 两处各拼一份的后果是「补冻用一份单价、结算用另一份」——冻出来的数既可能
// 盖不住结算（撞上界、平台吃掉差额），也可能过量（把 #498 要修的问题换个
// 地方复发），而两种都不会报错。
//
// 它顺带把 placement 记进 h.req（见 routedTo）：三条收尾路径（结算、失败结算、
// 释放）本来就各自调一次，这里只是把第一次提前到了选路之后。
func (h *handle) settledRequest(placed placement) pipeline.Request {
	h.routedTo(placed)
	req := h.req
	// 规则按商家匹配，而商家由渠道推出来（见 ownerOf）。
	req.OwnerUserID = ownerOf(h.candidates, req.ChannelID)
	// 候选集收窄成选中的那一个，让这份请求自洽：上面那个字段说的是「实际选中了
	// 谁」，而候选集若还留着整份，两者对不上。结算与补冻都不读候选集（它们各自
	// 按收窄后的那一个渠道匹配，见 pipeline.Settle / ReserveMore）。
	req.Candidates = []pipeline.RuleCandidate{{
		ChannelID:   req.ChannelID,
		OwnerUserID: req.OwnerUserID,
	}}
	return req
}

// finishCharge 结算一次成功的请求。
//
// placed 是这次请求打到了哪儿（见 placement）：成功账单行上的渠道与上游名
// 靠它写进去，与 settleAndFail / finish 收的是同一个值。
//
// 基准是 h.req，不是调用方手上那份请求：BillingRequestID 是在 begin 里
// 赋给 h.req 的，外层那份看不到它——而少了它，账本分录就挂不回这次账单。
//
// 「实际选中了谁」也取自 h.req：渠道 id 与上游模型名由 placed 记上去
// （见 handle.routedTo），这里只是把它收窄成**一个**渠道——规则按实际选中的
// 那个匹配，而不是按预留时的一整个候选集（见 pipeline.Reserve）。
// 别名不在这里重查一次：事后重查答不出「当时发的是什么」——配置可能已经
// 改过，而那正是账单要能回答的问题。
func (g *Gateway) finishCharge(ctx context.Context, h *handle, placed placement, out pipeline.Outcome) (*pipeline.Charge, error) {
	h.settled = true
	h.routedTo(placed)

	// 结算用**实际选中的渠道**：渠道级与商家级规则按它匹配。
	//
	// 预留时还不知道会选中谁，所以那时按整个候选集取上界——每个候选各取
	// 自己适用的一条规则，取其中金额最大的（见 pipeline.Reserve）。实际
	// 选中的渠道必是候选之一，而它的规则在那次取上界里算过，所以结算
	// 不会超过预留。EvalWorstCase 取的是**一条规则内部**的最坏分支，
	// 不跨规则；跨规则那一层由预留的「取最大」负责。
	//
	// 流的递进补冻要的就是这一份（见 settledRequest）：两处拼出来的输入
	// 必须逐字相同，否则补冻与结算各按一份规则算钱。
	settleReq := h.settledRequest(placed)

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
//     而且在请求方向那次调用**回来之后**立刻做（见 snapshotRequest）——
//     脱敏的对象是那份不变的请求副本，什么时候读都是同一个结果，但早做
//     意味着它在响应处理之前就完成了。
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
// request_id / user_id / direction 说的是「这条快照属于谁」，脱敏器不写它
// （见 internal/snapshotwire 的 toSnapshot），所以三个字段在这里补齐。
// 而脱敏器现在**知道**归属——它按归属判「留骨架还是留原文」，判完就不再管
// 这三个字段（见 CaptureIdentity）。userID 必须是**发起这次请求的用户**，
// 见 Snapshot.UserID 的注释。
func (g *Gateway) snapshotAsync(ctx context.Context, req *channel.Request, direction string, userID int64) {
	if g.Redactor == nil || g.Snapshots == nil || req == nil {
		return
	}

	id := CaptureIdentity{UserID: userID, RequestID: requestIDOf(ctx)}
	snap, err := g.Redactor.Redact(ctx, req, id)
	if err != nil {
		// 脱敏失败就丢快照——绝不回落成存原文。
		// 这里同步返回而不是在 goroutine 里判断：失败是**确定性**的，
		// 让它跑一趟后台再丢弃是白费。
		return
	}
	snap.RequestID = id.RequestID
	snap.UserID = userID
	snap.Direction = direction

	g.saveSnapshotAsync(ctx, snap)
}

// snapshotRequest 挑出该采进请求快照的那份请求。
//
// # 优先用真正发出去的那份
//
// 请求快照回答的是「当时发出去的是什么」，而发出去的那份与调用方发来的那份
// **不是同一份**：上游名按选中的渠道翻过，别名上配的参数覆盖也应用上去了
// （见 internal/route 的 upstreamRequest）。所以有它就用它。
//
// 在选路之前采是采不到的：覆盖逐渠道配，而那时连选哪个渠道都不知道。
//
// # 一份都没发出去时退回调用方那份
//
// routed 为 nil（候选全被筛掉，选路没跑）或者它那份是 nil（别名翻不出来、
// context 已取消——见 route.Result.OutboundRequest）时，这一次请求压根没有
// 「发出去的那份」。那时记录调用方发来的那份仍然有用：它就是要发的那份，
// 而这一类失败恰恰是最需要复现的（别名配错、渠道全被摘除）。
//
// 退回是有判据的而不是猜的：调用方拿到的「什么都没有发出去」这个事实由
// OutboundRequest 为 nil 表达，快照里那一行只是当时能拿到的最好证据。
func snapshotRequest(routed *route.Result, caller *channel.Request) *channel.Request {
	if routed != nil && routed.OutboundRequest != nil {
		return routed.OutboundRequest
	}
	return caller
}

// responseSnapshotAsync 采集一条响应方向的快照。
//
// # 采集点与请求方向不同
//
// 请求方向采在**选路之后**：它记的是发出去的那份请求，而上游名与别名上的
// 参数覆盖都是逐渠道的，选路之前算不出来（见 snapshotRequest）。响应方向采在
// **那次调用有了结局之后**：非流式在 routeChat 返回之后、流式在事件流读完
// （或中断）之后。它回答的是「回来的是什么」，与请求方向配对读。
//
// # 三个入参都可以是零值
//
// resp 为 nil 表示没有可脱敏的响应体（连不上、上游回了非 2xx）；status 与
// kind 由 upstreamEvidence 推出。**三者全空时不采集**：那说明这次请求
// 根本没有走到上游（没有候选、别名翻不出来、插件在选路前就阻断了），
// 写一行方向为 response、内容为空的快照只会让人以为上游回了个空。
//
// 脱敏与落库的同步/异步分界与请求方向完全相同，见 snapshotAsync。
func (g *Gateway) responseSnapshotAsync(ctx context.Context, resp *channel.Response, userID int64, status *int, kind *string) {
	if g.Redactor == nil || g.Snapshots == nil {
		return
	}
	if resp == nil && status == nil && kind == nil {
		return
	}

	id := CaptureIdentity{UserID: userID, RequestID: requestIDOf(ctx)}
	snap, err := g.Redactor.RedactResponse(ctx, resp, id)
	if err != nil {
		return
	}
	snap.RequestID = id.RequestID
	snap.UserID = userID
	snap.Direction = "response"
	snap.UpstreamStatus = status
	snap.ErrorKind = kind

	g.saveSnapshotAsync(ctx, snap)
}

// saveSnapshotAsync 把一条已经组装好的快照交给落库层。
//
// # 用请求的 context 值但**不带取消**
//
// 请求结束后快照仍该写完，而因为客户端断开就丢掉一条快照，恰恰丢掉了最该
// 看的那条——响应方向尤其如此：断流正是这条快照要回答的事。
func (g *Gateway) saveSnapshotAsync(ctx context.Context, snap *Snapshot) {
	base := context.WithoutCancel(ctx)

	go func() {
		snapCtx, cancel := context.WithTimeout(base, snapshotTimeout)
		defer cancel()
		_ = g.Snapshots.Save(snapCtx, snap)
	}()
}

// responseEvidence 从一次请求的结局里取出响应方向的证据。
//
// 三个返回值对应响应快照要用的三样东西：脱敏的对象、状态码、错误分类。
// 上游成功时对象是那份响应，状态码取自它（适配器在拿到响应头后填的）；
// 失败时没有可脱敏的响应体，两列由 upstreamEvidence 推出——它与账单那
// 两列走的是**同一个**判据，见 upstreamEvidence。
func responseEvidence(resp *channel.Response, err error) (body *channel.Response, status *int, kind *string) {
	if err != nil {
		s, k := upstreamEvidence(err)
		return nil, optionalInt(s), optionalString(k)
	}
	if resp == nil {
		return nil, nil, nil
	}
	return resp, optionalInt(resp.StatusCode), nil
}

// optionalInt 把「0 表示没有」翻成快照的 nil 形态。
//
// 上游状态码 0 与「没有状态码」在库里必须落到同一个 NULL：
// HTTP 里没有 0 这个状态码，写进去会读成「上游回了 0 号」。
func optionalInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

// optionalString 把空串翻成快照的 nil 形态。
//
// 分类的零值是空串（见 upstreamEvidence），库里对应的无值是 NULL。
// 不把空串写进去：`error_kind IS NOT NULL` 是「有分类」的判据，
// 一个空串会让它按「有」来数。
func optionalString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// usageOf 从上游响应里取出用量。
//
// 上游没报 usage 时返回零值——那会让结算金额为零，**而不是静默按某个
// 默认价收钱**。零金额在账上是可见的（有请求无消费），而默认价不可见。
//
// 零值有两个来源：上游报了一个全 0 的 usage，或者响应体里根本没有 usage 块。
// 光看这个函数分不开它们，所以来源要另外问一次（见 usageSourceOf）。
func usageOf(resp *channel.Response) channel.Usage {
	if resp == nil {
		return channel.Usage{}
	}
	return resp.Usage
}

// usageSourceOf 判断这次非流式响应的用量是不是上游给的。
//
// # 判据是「有没有 usage 块」，不是「数字是不是 0」
//
// 上游可以报一个全 0 的 usage（字段都在，值都是 0），也可以整块不报。
// 两种情形下 usageOf 拿到的都是零值，**逐列相同**；而读账的人会把后者
// 读成「这个模型的用量全是 0」。真正的区别只有一处：响应体里有没有
// 那个块，而这一事实由 channel.Usage.Reported 显式给出。
//
// 不拿「用量等于零」反推：那会把上游报的 0 读成「没报」，方向正好反了
// ——本函数存在的理由就是不让这两件事混在一起。
//
// 也不读 Usage.Raw：那个字段的注释写着「排查用，不参与计费」，里面的
// 键名由 relay 与适配器约定着填。拿它当判据时，谁改了键名或新适配器
// 忘了填，这里不会报错，只会把来源静默判反。Reported 是计费路径上的
// 一等字段，漏填会落成 false（保守那一侧）。
//
// 判据宁严勿宽：拿不准（Reported 为 false）时记 Unreported，而不是声称
// 实测。把一个没报的 0 记成实测，正是要消除的那一种；反过来，把一个实测值
// 误记成「没报」只是让一组本来能对账的数落进不可对账那一档，金额不变。
func usageSourceOf(resp *channel.Response) tokens.Source {
	if resp == nil || !resp.Usage.Reported {
		return tokens.SourceUnreported
	}
	return tokens.SourceMeasured
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
//
// 它必须在成功请求的结算**之后**调用：延迟与缓存两列由写入方按桶从
// usage_events 重算，而那一行是结算事务写的（见 route.CallOutcome）。
// 失败的请求没有用量事件，也就不涉及这个顺序。
//
// # 请求 ctx 已经结束时，失败的尝试记成「不归因」
//
// 客户端点停止、LB 空闲超时、客户端自己超时，都会让请求 ctx 在调用中途
// 结束。那种失败不记在渠道头上：连续失败计数既不加一也不清零。
//
// 判据与 channelAtFault 是同一条——「换一个渠道会不会不一样」——而客户端
// 断开时不会。这里必须看 ctx，不能看错误的形态：internal/channel 下的四个
// 适配器都把 ctx 取消包成一个可重试的上游错误（见 openaiadapter、
// anthropicadapter、responsesadapter、systemoneadapter 各自调上游的那一处），
// 而 IsRetryable 为真的错误正是 channelAtFault 判为渠道故障的那一类。少了
// 这一道，一个健康渠道会在客户端反复放弃的几分钟里攒够连续失败三次，然后
// 按 streakCooldown 被摘掉十五分钟。
//
// 流式那条路早就是这么判的（见 pumpStream 的 clientGone）：两条路对同一件
// 事不该给出相反的答案。
//
// 成功的那一次不在改判之列：上游确实服务了这次请求，取消发生在它回来之后
// ——流式那边同样是「流正常走完之后调用方才断开」（clientGone 要求流没走完），
// 不算客户端断开。改判它还会连用量一起丢掉，而那份用量是额度自记账的依据。
//
// 代价说清楚：取消之前那几次失败也一并落成「不归因」，即使它们是渠道自己的
// 问题。不按时间分辨是因为我们手里没有「ctx 是什么时候结束的」这个时刻
// （Attempt 只有开始与时长），猜错的方向是把一次取消重新记成渠道故障——那
// 正是这一道要消除的东西。丢掉的那几次方向是**不摘渠道**，与三次参数错摘掉
// 一个健康渠道相比是轻的那一侧（见 channelAtFault）。
func (g *Gateway) recordAttempts(ctx context.Context, attempts []route.Attempt, usage *channel.Usage) {
	if g.Recorder == nil || len(attempts) == 0 {
		return
	}
	// 取一次就够：它是这一次请求的同一个事实。循环里每次重读的话，一次运行
	// 中途结束的 ctx 会让同一次请求里的两次尝试按两套判据记。
	canceled := ctx.Err() != nil
	for _, a := range attempts {
		verdict := verdictOf(a.Err)
		if canceled && a.Err != nil {
			verdict = route.VerdictUnattributed
		}
		rec := route.CallOutcome{
			ChannelID: a.ChannelID,
			At:        attemptTime(a, g.now()),
			Verdict:   verdict,
		}
		if a.Err == nil && usage != nil {
			rec.Usage = *usage
		}
		g.recordCall(ctx, rec)
	}
}

// attemptDuration 取一次路由过程里**最后一次尝试**的耗时。
//
// 取最后一次而不是把全部加起来：失败的那几次各是一次独立的调用，加起来
// 得到的是「这次请求因为重试耽误了多久」，而延迟那一维问的是「这个渠道
// 多快」。最后一次正是真正服务了这次请求的那一次。
//
// 没有尝试时返回 0（写入方把它落成 NULL，不是 0 毫秒）。
func attemptDuration(attempts []route.Attempt) time.Duration {
	if len(attempts) == 0 {
		return 0
	}
	return attempts[len(attempts)-1].Duration
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
		// request_id 那一栏由观测层从 ctx 附上来（见 observe.FromContext），
		// 这里不手写：手写的那一栏取不到值时会附上一个空串，而日志里同名键
		// 后者胜，真值会被那个空串盖掉。
		observe.FromContext(ctx).Error("写渠道健康度失败",
			"err", err, "channel_id", rec.ChannelID)
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

// requestIDOf 取这次请求的标识，没有则返回空串。
//
// 键用观测层那一个（observe.WithRequestID 放进去的），网关自己不再留一份：
// 留的那一份从来没有被写进过 context——往 context 里放标识的是观测层的中间件，
// 用的是观测层自己的键类型，两者**同名不同型**，看着像一份，取值却一直落到
// 空串上。本包已经 import 观测层，直接取即可。
func requestIDOf(ctx context.Context) string {
	return string(observe.RequestIDFromContext(ctx))
}
