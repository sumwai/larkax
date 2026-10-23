// Package pipeline 把计费的各个环节串成一次请求的完整流程。
//
// 它串起来的是：
//
//	取规则 → 求最坏情况 → 预留 → （上游调用）→ 按实际用量结算 → 生成账本分录
//
// 这一层**不查库、不发 HTTP、不改余额**：规则从 RuleSource 来，
// 账本分录交回调用方去持久化。于是整条流程可以脱离数据库与网络测试，
// 而那恰好是这套系统里最贵的一类 bug 的所在——**算错了不会报错，
// 只会让账目悄悄错掉**。
//
// 唯一的例外是告警：上游报的用量自相矛盾时写一条日志（见 Settle）。
// 它走 context 上的日志器，不引入任何具体依赖，也不改变任何返回值——
// 没有日志器的调用方（测试、未来的别的入口）拿到的仍是 slog 的兜底。
//
// # 为什么分两个阶段而不是一个函数
//
// 预留发生在请求发出**之前**，结算发生在响应回来**之后**，中间隔着一次
// 可能持续数分钟的上游调用。合成一个函数意味着它必须持有一个跨越整个
// 上游调用的状态，而那个状态里放着已经冻结的金额——它一旦丢（进程重启、
// panic），钱就少收或多收了。
//
// 分开之后，「预留」的返回值是可以被调用方落库的完整信息，
// 进程重启也能从库里捡回来。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/tokens"
)

// RuleSource 提供一次请求适用的计价规则。
//
// 抽成接口而不是直接查库：规则的匹配与优先级裁决是 SQL 的活，
// 而这一层只关心「拿到一条能求值的规则」。
//
// 两个方法分别服务计费的两个阶段：结算时渠道已经选定，只需要**一条**规则；
// 预留时只有一个候选集，需要**每个候选各一条**（见 Reserve）。
type RuleSource interface {
	// Match 返回这次请求实际会用的那一条规则：用于结算，那时渠道已经
	// 选定，req.Candidates 恰好一个元素。
	//
	// 没有匹配时返回 ErrNoRule——**不要返回一条默认的零价规则**：
	// 那会让「没配价」表现为「免费」，而免费是最不容易被发现的错账。
	Match(ctx context.Context, req RuleQuery) (Rule, error)

	// MatchAll 返回候选集里**每个候选各自**适用的那一条规则：用于预留。
	//
	// 返回的规则集对每个候选至多一条（裁决次序与 Match 相同）。一个候选
	// 都匹配不上时返回空集——**不要给它编一条零价规则**，理由同上。
	//
	// 调用方对空集要有明确处置：空集意味着算不出上界，而不是零价
	// （见 Reserve）。
	MatchAll(ctx context.Context, req RuleQuery) ([]Rule, error)
}

// Rule 是一条待求值的计价规则。
type Rule struct {
	ID int64
	// ChannelID 是这条规则服务的那条候选渠道，由 RuleSource 填。
	//
	// 预留要按候选**各自**查缓存记录（见 billing.CacheStore），而规则是
	// 按候选匹配出来的；没有这个归属，就只能拿整批候选查一次再用到每条
	// 规则上——那会把一条渠道的命中算到另一条头上，方向是漏收。
	//
	// 结算那一次（RuleSource.Match）也会填，但结算不用：那时渠道已经选定，
	// 命中与否已经不影响任何金额。
	ChannelID int64
	// Source 是表达式源码。
	Source string
	// Version 是规则的版本号，用于编译缓存的键。
	//
	// 它必须参与缓存键：同一 ID 的规则被改过之后，用旧版本号取到的
	// 编译产物仍是旧表达式，而那会让「改了规则但没生效」表现为
	// 「规则写错了」——排查方向完全错。
	Version int64
}

// CacheKey 是这条规则在编译缓存里的键。
func (r Rule) CacheKey() string {
	return fmt.Sprintf("%d:%d", r.ID, r.Version)
}

// RuleQuery 是匹配规则的条件。
type RuleQuery struct {
	// CanonicalModel 是规范模型名（不是上游名）。
	CanonicalModel string
	// Candidates 是这次请求可能用到的渠道，每个候选带它自己的商家。
	//
	// 两个阶段给的是不同大小的集合：
	//
	//   - 预留在选路**之前**，给的是全部候选。每个候选取它自己适用的
	//     那一条规则，预留按其中金额最大的算——只有一个候选会真正服务
	//     这次请求，所以是取最大，不是求和（见 Reserve）。
	//   - 结算在选路**之后**，给的是**恰好一个**候选：实际选中的渠道。
	//
	// 两个阶段共用一个字段，是因为它们问的是同一件事——「这次请求涉及
	// 哪些渠道」。给结算单开一对 channel/owner 字段会让同一份信息有两个
	// 表示，而它们迟早会不一致。
	Candidates []RuleCandidate
	// At 是匹配用的时刻。生效时间窗口按它判。
	At time.Time
}

// RuleCandidate 是匹配规则时的一个候选渠道。
type RuleCandidate struct {
	// ChannelID 是渠道标识。
	ChannelID int64
	// OwnerUserID 是渠道所属的商家，0 表示平台自营。
	//
	// 它必须跟着渠道一起来：商家级规则按 owner 匹配，只给渠道 id
	// 匹配不到商家价，而漏掉一条更贵的商家价会让预留小于结算。
	OwnerUserID int64
}

// ErrNoRule 表示没有匹配的计价规则。
//
// 单独一个错误类型是为了让调用方能把它与「规则求值失败」分开：
// 前者是配置缺失（该拒绝服务并告警），后者是规则本身有问题。
var ErrNoRule = errors.New("没有匹配的计价规则")

// CostSource 提供一次请求适用的上游成本规则。
//
// 与 RuleSource 分开，而不是给它加两个方法：成本规则只在结算时问一次，
// 问的是「这条渠道上这个模型的上游单价是多少」；它不参与预留（上游成本
// 不进任何金额的上界），也就没有「每个候选各一条」那个阶段。
//
// 方法名是 MatchCost 而不是 Match：pricing 的 CostSource 已经用 Match
// 提供了「问某一次请求按哪条成本规则算」这个形状（命令行的 explain 用），
// 两个签名不同，同一个类型无法同时满足。结算要的那一份多带一个版本号，
// 用来做编译缓存的键。
type CostSource interface {
	// MatchCost 返回这条渠道这个模型此刻适用的成本规则。
	//
	// 没有规则时返回 ErrNoCostRule——**不要返回一条零成本的兜底规则**：
	// 那会让「没配成本价」表现为「上游不要钱」，而毛利会因此虚高。
	MatchCost(ctx context.Context, q CostQuery) (CostRule, error)
}

// CostQuery 是「这条渠道这个模型的上游成本按哪条规则算」这一次提问。
type CostQuery struct {
	// ChannelID 是这次请求实际落到的渠道。成本规则按渠道挂，所以它没有
	// 「未选定」（0）这一档。
	ChannelID int64
	// CanonicalModel 是规范模型名。与售价规则同一套键。
	CanonicalModel string
	// At 是匹配用的时刻，取受理时刻：生效窗口按它判，与这次计费用的是
	// 同一个时刻（见 Settle）。
	At time.Time
}

// CostRule 是一条待求值的上游成本规则。
type CostRule struct {
	ID int64
	// Source 是表达式源码。成本与售价用同一个表达式方言，只是它写的是
	// 「我们付给上游多少」。
	Source string
	// Version 是用于编译缓存的版本号。upstream_costs 没有版本列，这个值
	// 由来源按表达式算出来（与售价侧的 pricing.Source 同一个做法）。
	Version int64
}

// CacheKey 是这条成本规则在编译缓存里的键。
//
// 带 cost: 前缀：成本规则与售价规则是两张表，id 各自从 1 开始，裸的
// 「id:version」会让两条不相干的规则撞同一个键，而症状是**用另一条规则的
// 表达式算成本**——算得出数来，只是数不对。
func (r CostRule) CacheKey() string {
	return fmt.Sprintf("cost:%d:%d", r.ID, r.Version)
}

// ErrNoCostRule 表示这条渠道这个模型没有生效的成本规则。
//
// 与 ErrNoRule 分开：那一条在请求路径上是拒绝服务（没配价就不能算钱），
// 而成本这一侧不参与任何拒绝。上游成本是观测值：算不出来时结算照走，
// 只是这一列留 NULL（见 Charge.UpstreamCost）。所以拿着这个错误的调用方
// 该做的是「记下没有成本数据」，不是把请求打回去。
var ErrNoCostRule = errors.New("没有生效的成本规则")

// Pipeline 是完整的计费流程。
//
// 它由 gateway **单实例并发共用**，所以 Reserve 与 Settle 会同时在多个
// goroutine 上跑。构造完就不要再改它的字段：字段是给装配点填的，
// 装配发生在任何请求之前。
type Pipeline struct {
	Rules RuleSource
	// Costs 是上游成本规则的来源。
	//
	// 与 Rules 分开：它只在结算时被问一次，且算不出来不影响任何金额。
	// 为 nil 时结算不写成本（落库层写 NULL）——这只对**直接构造 Pipeline 的
	// 测试**有意义：生产装配必须接上，assembly.Config 的 validate 会拒绝 nil
	// （不接的表现是成本这一维永远没有数据，而路由照样排得出来）。
	Costs CostSource
	// MerchantShare 决定一笔消费里分给渠道商户多少。为 nil 表示不分账，
	// 全部留作平台收入——这只对**直接构造 Pipeline 的测试**有意义：
	// 生产装配必须接上，不接的表现是 merchant_payable 永远是 0，而账面
	// 照样是平的，没有任何东西会报出来。
	MerchantShare *MerchantShare
	// Programs 是规则表达式的编译缓存。
	//
	// 不给时由 programs 兜底建一个（容量 defaultCacheSize）。要换容量
	// 就在**构造时**给——它是共享状态，构造之后再写会与并发调用者竞争。
	Programs *expr.Cache
	// Calendar 在规则用到日历时必须给，否则求值会失败。
	Calendar expr.Calendar
	// Region 是日历的地区。空则用规则所在地的默认值由调用方决定。
	Region string
	// Now 可注入，便于测试时间相关的逻辑。
	Now func() time.Time

	// cache 是缓存前缀记录的进程内存储（见 billing.CacheStore）。
	//
	// 零值可用：没有记录时预留退回按整段输入估，也就是这条特性出现之前的
	// 行为。它是值字段而不是指针/接口，正因为「不配置也能跑」是这个特性
	// 的既定退化方向——多实例之间不共享记录也一样，各实例只看得到自己写的那几条。
	//
	// 不放进装配配置：它没有需要调的参数，生命周期与 Pipeline 实例相同。
	cache billing.CacheStore

	// programsOnce 让 Programs 的兜底初始化只发生一次，且对并发的
	// 调用者可见（见 programs）。
	programsOnce sync.Once
}

// Request 是一次请求的计费输入。
//
// 它只带**计费需要知道的东西**，不带你调上游要用的参数——
// 那些与计费无关，混进来会让「改一个上游参数」看起来像「改了计费」。
//
// 唯一的例外是几个「这次请求在持久化那一侧是谁」的字段（RequestID、
// BillingRequestID、Protocol、Streamed、Idempotency）：它们不参与任何金额
// 计算，但落库层要靠它们把这一行写对。理由与取舍各自写在字段上。
type Request struct {
	// RequestID 是幂等键。同一次请求重复计费必须只扣一次。
	RequestID string
	UserID    int64

	// APIKeyID 是这次请求用的那把 API Key（api_keys.id），落进
	// billing_requests.api_key_id。
	//
	// 与 UserID 不是一回事：一个用户可以有多个 key，而按 key 归因用量、
	// 排查「是哪把 key 在异常调用」只能靠这一列——退到按用户的话，同一个
	// 用户名下的几把 key 就分不开了，而那正是要区分它们的原因。
	//
	// 由**入口**设置：鉴权那一步解析出用的是哪把 key（见 internal/auth 的
	// User.KeyID），计费这一层看不出请求带的是哪把。落库层拒绝 0
	// （见 internal/ledger/store 的 ErrMissingAPIKey）：这一列的历史行
	// 本来就是空的，于是「漏设」与「历史行」在库里长得一样，
	// 而写进去的那一刻是这两者唯一还分得清的地方。
	APIKeyID int64

	CanonicalModel string

	// UpstreamModel 是这次请求**实际发给上游**的模型名，由网关在选路之后
	// 填上（见 internal/route 的 Result.UpstreamModel）。
	//
	// 它与 CanonicalModel 是两个不同的东西，别混：**计价按规范名**
	// （pricing_rules.model_patterns 匹配的是它），而发出去的名字是某个渠道
	// 上这家上游的叫法。别名没配时两者相同。
	//
	// 它落进 billing_requests.upstream_model——那一列存在的意义正是记下
	// 「这次实际用了哪个上游名」，而事后重查一次别名答不出这个问题：
	// 配置可能已经改过，而上游改名正是要配别名的那类事。
	//
	// 预留时它还是空的：那时还没选路，也就不知道会走哪个渠道（见 ChannelID
	// 的注释，两者在同一个位置被填上）。结算时**必须有**，落库层拒绝空值
	// （见 internal/ledger/store 的 ErrMissingUpstreamModel）。
	//
	// 失败路径上它同样有值（上游回了 4xx 的那一次确实发出去了），只是落库的
	// 那一处不同：结算走 markSettled、不计费走 Release，两处读的是同一个
	// 字段。一次调用都没发生时它保持空——那时没有上游名可记。
	UpstreamModel string

	// Candidates 是这次请求**可能**选中的渠道集合，供预留取上界。
	//
	// 预留发生在选路之前，那时还不知道会选中谁（见 ChannelID），所以
	// 上界只能按候选集取。空集合算不出上界，Reserve 会拒绝——见那里的
	// 注释。规则匹配之外没有别处读它。
	Candidates []RuleCandidate
	// ChannelID 与 OwnerUserID 是**实际选中**的渠道与商家：选路之前
	// 为 0，选路一返回就由网关填上实际的那个（见 internal/gateway 的
	// handle.routedTo）。规则匹配之外，账本还要按 ChannelID 记下这次请求
	// 走的是哪条渠道——**成功与失败两条路都要**，所以它填在结算之前。
	//
	// OwnerUserID 只有结算读（规则按商家匹配），失败路径上留着零值不影响
	// 任何判据。
	ChannelID   int64
	OwnerUserID int64

	// InputTokens 是请求的输入 token 估算，由分词层给出。
	InputTokens int64
	// CachePrefix 是这次请求前缀的块序列，由入口按请求算出（见 CachePrefixOf）。
	//
	// 预留拿它去比对同一条渠道上一次结算留下的记录，判断下次会不会命中上游
	// 缓存（见 billing.CacheStore）：命中那一段按缓存价估，上界跟着往下压。
	//
	// **为空表示算不出来，不表示「没有前缀」**：那时不查记录，预留按整段输入
	// 估——即这条特性出现之前的行为。它是只读的计费依据，不落库、不参与结算。
	CachePrefix []billing.BlockHash
	// MaxTokens 是请求允许的最大输出。它是预留上界的关键。
	MaxTokens int64

	// Usage 是计费口径的用量（缓存读写等）。预留时用它估缓存侧的消耗。
	Usage billing.Usage

	// Asset 是结算用的资产品种。**必填**，空值会被 validate 拒掉。
	//
	// 它由**入口**从运行期配置里填（settings 的 billing.default_asset），
	// 见 internal/api。本包不再兜一个默认值：兜底意味着同一个概念有两处
	// 取值，而库里那一份改了之后，代码里那一份会安静地继续生效。
	Asset string

	// BillingRequestID 是 billing_requests 表里那一行的 id，
	// 由调用方在落预留时拿到。它是账本分录与请求记录的关联点。
	//
	// 为 0 表示尚未落库——此时分录也构造得出来，只是追溯时少一层关联。
	BillingRequestID int64

	// Streamed 说明这次请求走的是流式入口，落进 billing_requests.streamed。
	//
	// 两条路的失败方式与用量来源都不同（流式的用量可能是估算的），而事后
	// 只看金额分不出这笔账是哪条路产生的。它由入口设置：计费这一层看不出
	// 请求是不是流式的，也不该去猜。
	Streamed bool

	// Protocol 是这次请求走的入口协议，落进 billing_requests.protocol。
	//
	// 与 Streamed 同一个来源：由**入口**设置——调用方走哪条路径，在
	// 分派时就定了（见 internal/api 的 ServeHTTP）。计费这一层看不出
	// 请求是从哪条路径进来的，也不该去猜：写死一个默认值会让另一条入口
	// 的账单全部记错，而那张表正是事后按协议统计的依据。
	//
	// 取值是 channel.Protocol 的取值集合：ProtocolOpenAI、ProtocolAnthropic、
	// ProtocolOpenAIResponses 与 ProtocolSystemOne 四种；落库层拒绝其他取值
	// （含空值），见 internal/ledger/store 的 checkProtocol。
	//
	// 这一份清单是 channel.Protocol.valid 的**副本**，三处（这里、那里、
	// 落库层）要一起改——漏了落库层那一处的表现是「入口收得下这个请求，
	// 却在预留那一步失败」。
	Protocol channel.Protocol

	// Idempotency 是这次请求的幂等声明，由入口按请求头 Idempotency-Key 设置；
	// nil 表示调用方没带这个头，那时落库层不写幂等记录。
	//
	// # 它为什么在这里
	//
	// 幂等记录必须与预留写在**同一个事务**里（只记了幂等而没预留，重试会
	// 被告知「已处理」而钱没扣；只预留而没记幂等，重试就重复扣费），而
	// 预留由落库层写。声明跟着请求一起走到那里，就不必再给 SaveReservation
	// 加一个参数、也不必让编排层转手一次——那条链上每多一个环节，
	// 就多一个「有人忘了往下传」的位置，而漏传的表现是幂等静默失效。
	//
	// 它和 RequestID 一样属于「一次请求在持久化那一侧的身份」，
	// 不是计费口径：它不参与任何金额的计算。
	Idempotency *idempotency.Claim

	// UserAgent 是调用方请求头里的 User-Agent，由**入口**原样带来，
	// 落进 billing_requests.user_agent。
	//
	// 不截断、不归一化：客户端标识符五花八门，而排查「哪个版本的客户端
	// 出的兼容性问题」要看的正是版本号那一段。空串表示调用方没带这个头，
	// 落库层写 NULL。
	UserAgent string

	// ToolResultTail 是「这次提交的最后一句是工具结果回填」：由入口按
	// channel.Request.EndsWithToolResult 判定，落进
	// billing_requests.tool_result_tail。
	//
	// 它把「用户在对话」与「agent 在跑工具循环」分开——请求日志上同一行
	// 两种读法，而预留侧（工具回填的请求几乎必然命中同一前缀）将来压
	// 上界也要用它。systemone 的决策表面没有消息序列，恒为否。
	ToolResultTail bool
}

// Reservation 是预留的结果，调用方需要把它落库。
//
// 它带着**求值时的全部依据**（规则标识、表达式快照、求值时刻），
// 这样事后复算不需要再查规则——规则可能在事后被改过，
// 而账单必须能解释当时为什么是这个价。
type Reservation struct {
	RequestID string
	UserID    int64
	Asset     string

	// Amount 是冻结的金额。它是**上界**：结算不会超过它。
	Amount money.Amount

	// RuleID 与 PricingExprSnap 是**算这个上界用的那条**规则。
	//
	// 预留时候选不止一个，没有「唯一适用的一条」，所以这里存的不是
	// 「这次请求会用的规则」，而是候选规则里**给出最大金额的那一条**
	// （见 Reserve）。它的用途是让界面能解释「这笔钱是怎么估出来的」：
	// 把这条规则的表达式摊开，上界里的每一项是按什么价算的就一目了然。
	//
	// 它**不代表**结算会用的规则：结算按实际选中的渠道重新匹配，
	// 结果可能不是这一条（见 Settle）。
	RuleID          int64
	PricingExprSnap string
	// OutputTokens 是这份预留**已经覆盖了**多少个输出 token。
	//
	// 非流式那条路上它等于声明的上界（一次冻满，不必再冻）；流式那条路上它是
	// 第一段，此后每补冻一次就加上那一段——那条路上 Amount 与它同进同退，两者
	// 是同一笔冻结的两个说法（见 internal/gateway 的 streamFreeze）。
	//
	// 它不参与落库：billing_requests 没有这一列。它是「还差多少才盖得住已产出
	// 量」这个判断的输入，而账单要回答的不是这个问题。
	OutputTokens int64
	// RuleMatchedAt 是**受理时刻**：这次请求的价格就是按它在规则里选中的
	// 那一档定下来的。
	//
	// 结算复用它，而不是重新读一次钟（见 Settle）。它是「价格在受理那一刻
	// 定下来」这条口径的载体：跟着预留一起持久化下来，事后才回答得了
	// 「这次是按哪一刻的档位算的」，也才谈得上复算。
	RuleMatchedAt    time.Time
	BillingRequestID int64

	// UserPlanID 是这次预留在**额度包**里选中的那条订阅；0 表示从账户余额扣。
	//
	// 它由落库层在预留事务里填（见 internal/ledger/store 的 SaveReservation），
	// 随预留行一起持久化。结算读的就是这个值，**不重新选**：在途期间订阅到期、
	// 用户新增一个优先级更高的包，都不改变这一笔的账面。
	//
	// 它也是分录走哪个科目的开关：非 0 时消费那条借的是
	// `prepaid:<订阅 id>`，其余三条（平台收入、商户应付）形状不变
	// （见 internal/ledger 的 Consume）。
	UserPlanID int64
}

// Outcome 是上游调用的结果，决定怎么结算。
//
// **失败不计费**，而「失败」有几种，处置不同。用一个结构体显式表达
// 而不是靠调用方传一个 bool：那几种情形在计费上的差别很大，
// 而它们在上游调用那一侧都表现为「出错了」。
type Outcome struct {
	// UpstreamFailed 为 true 表示**这次结算不计费**。
	//
	// 今天有两个来源：上游根本没成功返回；以及上游成功了、响应也拿到了，
	// 但它在交给调用方之前被插件拦下（上游那笔钱花了，却不向调用方收——
	// 他拿到的是一个错误，而不是那份回答）。两者在计费上的处置完全一样，
	// 所以共用一个开关；在这一层分不出是哪一个（见 observe 的
	// UnbilledSettlements）。
	//
	// 不计费，但仍然要落一条记录——「这个请求失败了」本身是排查的依据。
	UpstreamFailed bool

	// UpstreamStatus 与 ErrorCode 是**上游这一侧**这次的依据，供事后核对
	// 「这个错误码当时是不是真的不该计费」。同一对值有两处落点，按这次计不
	// 计费分开：不计费的走 internal/ledger/store 的 Release，计费的走
	// markSettled（见下）。
	//
	// 两者都只在**上游这一侧**的失败与截断上有值：
	//
	//   - UpstreamStatus 是上游那次 HTTP 响应的状态码。**拿到过响应就有值**，
	//     包括上游回 2xx、却把错误写在体里的那些（通常是 200）；一个响应都
	//     没拿到时是 0，落库成 NULL——HTTP 里没有 0 这个状态码，写 0 会读成
	//     「上游回了 0 号」。
	//   - ErrorCode 的取值口径见下面那组常量。
	//
	// # 计费的行上只有一种值会到这里
	//
	// 上游吐了字之后出的问题不改变计费（token 已经产生，上游算了钱），而其中
	// 只有**静默截断**（ErrorUpstreamStreamTruncated）会记在账单上：那一次
	// **没有错误帧**，调用方要靠「帧序列里没有结束标记」自己发现被截短了
	// （见 docs/api.md 的「没有结束标记就是被截断」一节）。按账单核对金额的人
	// 看不到它，账单因此是唯一能说明「这次回答只写了一半」的地方，而快照又有
	// 留存期（见 docs/request-retention.md）。上游在流里报错的那一次，调用方
	// 当场就拿到错误帧了，账单不必再讲一遍——它的两列留 NULL（见
	// stream_e2e_test.go 的 TestStreamUpstreamErrorMidStream）。
	//
	// 所以 `error_code IS NOT NULL` 仍然不是「上游出过问题的请求」的判据，
	// 而且它在两种行上答的是两个问题：计费的行上答的是
	// 「钱收了，回答是不是在调用方不知情的情况下被截短」，不计费的行上答的是
	// 「这次没向上游收钱，是不是因为上游这一侧出了事」。两种读法都得连着
	// status 一起看。
	//
	// 不是上游的失败（插件阻断、别名没配）两列一律留空：它们不是上游出的
	// 事，写进去会让上面那个问题答错。
	UpstreamStatus int
	ErrorCode      string

	// ClientCanceled 为 true 表示客户端在流式响应中途断开。
	//
	// **要计费**：已经产生的 token 上游是算了钱的。用量由 Usage 给，
	// 而它是网关按收到的内容当场估算的——见 tokens 包。
	ClientCanceled bool

	// Usage 是实际的用量。上游失败时忽略它。
	//
	// 它应当是**实测值**；只有在客户端中途断开这类拿不到实测值的
	// 情形下才是估算值，而那时 UsageSource 会标明。
	Usage channel.Usage
	// UsageSource 说明用量是实测还是估算。结算时会落进账本，
	// 因为「这笔钱是按估的收的」是需要能被事后看出来的。
	UsageSource tokens.Source

	// Duration 是这次上游调用**实际花了多久**，只用于统计，不参与金额计算。
	//
	// 它由网关量：非流式取最后一次尝试的耗时（route.Attempt.Duration），
	// 流式再加上读完那条流的时间（那次尝试只覆盖到上游接受请求）。记账层
	// 把它写进 usage_events.duration_ms，渠道健康度的延迟那一维再从那里
	// 按小时桶取分位数。
	//
	// 零值表示**没有测到**（调用没有发生，或者调用方没给），不是「耗时 0」。
	// 与 charge.Usage 的零值一样，落库层要靠这一条把它写成 NULL——
	// 0 毫秒的样本会把聚合出来的分位数拉低，而那个数看起来完全正常。
	Duration time.Duration

	// FirstToken 是从受理到**第一个内容块**到达网关的时长，只有流式请求
	// 量得出：非流式的响应整份一次到达，TTFT 没有意义。
	//
	// 它由网关在流式循环里量（第一个让「已向客户端吐字」成立的事件），
	// 随结算落进 billing_requests.first_token_ms。零值表示没测到
	// （非流式、上游一个字没吐就失败），落库层写 NULL——与 Duration
	// 同一个口径：0 毫秒是「快得看不见」，不是「没量」。
	FirstToken time.Duration
}

// Outcome.ErrorCode 的取值。它们是**我们自己归一化过的分类**，不是上游错误
// 码的原文。
//
// 为什么不用原文：各家上游对同一件事叫的名字不一样，而这一列的用途是事后
// 统计「哪类失败最常见」——按原文统计时，两个名字会把同一件事算成两件，
// 三个部署之间也拼不起来。上游自己那个机器可读的数字没有丢：它在
// UpstreamStatus 里。两列合起来既看得出「哪一类」，也看得出「上游说的几号」。
//
// 要取原文得改 internal/channel：channel.ErrUpstream 今天带状态码、「拿到过
// 响应吗」与响应体原文，上游自己那个错误码要从体里解出来才拿得到，而解它的
// 是适配器。
//
// 取值只有下面五个，**都属于「上游这一侧出了事」**。我们这边的失败（插件
// 阻断、别名没配、结算自己出错）不进这一列：混进来之后，
// `error_code IS NOT NULL` 连「是不是上游挂的」都答不了了。
//
// 这一列有两处落点。**不计费**的行上是前四个（记的是「这次为什么没计费」），
// 它们由不计费那一侧的落点写入（见 internal/ledger/store 的 Release）；
// **已计费**的行上只有第五个（ErrorUpstreamStreamTruncated）：上游吐了字之后
// 把连接掐断，那些 token 已经花了钱。所以 `error_code IS NOT NULL` 既不等价
// 于「这次没向上游收钱」，也不等价于「上游没挂」——要回答钱的问题得连着
// status 一起读。
const (
	// ErrorUpstreamHTTPError 表示上游回了个错误状态码（非 2xx）。状态码本身
	// 在 UpstreamStatus 里，两列成对读：`upstream_http_error` + 500。
	//
	// 名字不叫 rejected：那读起来像「上游拒绝了这次请求」，也就是 4xx，而这条
	// 口径覆盖的是**任意非 2xx**——httpx.CheckStatus 把 500、503、429 一样
	// 变成带 StatusCode 的 ErrUpstream。值会进库，而库里没有注释可读，所以
	// 名字得自己说得清。
	ErrorUpstreamHTTPError = "upstream_http_error"

	// ErrorUpstreamUnusableResponse 表示上游回了响应，但那份响应不是一个
	// 可用的回答。两种情形并在这里：响应体解不开（解析失败、结构不符），
	// 以及上游回了 2xx 却把错误写在体里（见适配器的 extractEmbeddedError）。
	//
	// 它与 ErrorUpstreamNoAnswer 的分界是**拿到过响应吗**，由
	// channel.ErrUpstream.GotResponse 显式给出，不从 Body 反推。拿到过响应
	// 就有一个真实的状态码，所以 UpstreamStatus 此时有值（通常是 200）：
	// 把它丢掉会让库里那一行读成「没有记录到状态码」，而它其实是一个确定的
	// 事实。
	ErrorUpstreamUnusableResponse = "upstream_unusable_response"

	// ErrorUpstreamNoAnswer 表示上游这一侧**一个 HTTP 响应都没有给过**：
	// 连不上、TLS 失败、超时。响应头都没收到，所以 UpstreamStatus 是 0
	// （落库成 NULL）：上游没有给出过一个状态码，编一个出来比留空更坏。
	//
	// 「拿到了响应但读不懂」不在这里，见 ErrorUpstreamUnusableResponse。
	ErrorUpstreamNoAnswer = "upstream_no_answer"

	// ErrorUpstreamStreamAborted 表示上游接受了请求，但一个 token 都没吐就
	// 结束了（流式）。它发生在响应头之后：适配器拿得到那个 2xx，并由流的
	// 第一条事件把它交下来，所以 UpstreamStatus 记的是那个真实状态码——
	// 「拿到过响应」这个事实成立。这次的**结局**（一个字都没产生）由本分类
	// 表达，两列各司其职。
	ErrorUpstreamStreamAborted = "upstream_stream_aborted"

	// ErrorUpstreamStreamTruncated 表示上游吐过内容之后，连接在没有结束标记
	// （[DONE] / message_stop）、也没有 error 帧的情况下就断了（流式）。
	//
	// 它与 ErrorUpstreamStreamAborted 的差别是**有没有产生 token**：那一个
	// 一个字都没吐，所以不计费；这一个已经吐出去的部分要计费，上游是算了
	// 钱的。两者的共同点是「上游没把话说完整」，所以它们并在这组「上游这
	// 一侧出了事」的分类里。
	//
	// 它回答的问题与前三个不同：前三个只出现在**不计费**的账单行上（记的是
	// 「这次为什么没计费」），而这一档写的是**已计费**的行——`error_code IS
	// NOT NULL` 因此不再等价于「没向上游收钱」。要回答「是不是这次没计费」，
	// 得连着 status 一起读。
	//
	// 快照的 error_kind 用同一个值：同一件事在两表里取同一个词，读的人不必
	// 再翻译一次。
	//
	// 已计费那条路的落点就是 markSettled：这个值随 Outcome 进 Settle，由
	// Settle 搬进 Charge.ErrorCode，落库时写进 billing_requests.error_code
	// （见 internal/ledger/store）。所以它现在是五个取值里唯一**会在已计费
	// 的行上**出现的那个。
	ErrorUpstreamStreamTruncated = "upstream_stream_truncated"
)

// Charge 是结算的结果，调用方据此写账本。
type Charge struct {
	RequestID string
	UserID    int64
	Asset     string

	Settlement billing.Settlement

	// Entries 是要写入账本的分录。它是平衡的（组内求和为零），
	// 由 ledger 包构造并校验过。
	Entries []ledger.Entry

	RuleID          int64
	PricingExprSnap string

	// UsageSource 与 Usage 是这次结算依据的用量与它的来源。
	//
	// 它们由落库层写进 usage_events（见 internal/ledger/store 的 Save）：
	// 账本分录只说动了多少钱，这张表说那笔钱是按什么用量算出来的，
	// 对账要拿这两边对。来源决定 billing_path 那一列——本地计出来的数
	// 不能记成实测值，理由见 tokens.Source.IsExact。
	UsageSource tokens.Source
	Usage       billing.Usage

	// Prices 是这次结算**按哪些单价算的**，由 Settle 从求值结果搬过来。
	//
	// 它唯一的下游是 usage_events.unit_prices（见 internal/ledger/store 的
	// insertUsageEvent）：账本只记动了多少钱，而「那笔钱是按什么单价乘出来的」
	// 只有这一处留痕。取的是**价格侧**，不是 Settlement.Breakdown——
	// 后者是金额（单价 × 用量），撞上界时还被有意留成截之前的值（见
	// billing.SettleWithCharges），拿金额去反推单价要除、要猜零用量那一项。
	//
	// 这是**结算实测到的那一组价**，不是预留用的上界（Reserve 走
	// EvalWorstCase，逐槽取各分支最高价）：两者在分档规则下本来就不是一组
	// 数，而账单要解释的是前者。
	//
	// 只由 Settle 填。别的调用方（测试直接构造 Charge）留零值，落库层据此
	// 写空对象。落库时也不是五项照抄：只有 Declared 标过的那几项会进 unit_prices
	// ——「写了 0」（这一项免费）与「没写」（规则漏了）在 money.Amount 上分不
	// 出来，详见 insertUsageEventSQL 里 unit_prices 那一节。
	Prices billing.Prices

	// UpstreamCost 是这次请求在**上游一侧**的成本，由 Settle 按成本规则
	// （upstream_costs）算出：用这条渠道上这个模型适用的表达式求值，
	// 再按这次的实际用量乘出来。
	//
	// 它是「我们付给上游多少」，不是售价——售价在 Settlement.Actual 里。
	// 两者混起来毛利永远是零，而账目仍然是平的。
	//
	// nil 表示**算不出来**，落库层据此写 NULL：没有生效的成本规则是正常
	// 状态；规则存在却编译不过、求值失败或覆盖不了这次用量时另外写一条
	// 告警。**不要用零值顶替 nil**——0 在库里读作「上游不要钱」，而毛利会
	// 因此虚高，且没有任何东西会报出来。
	UpstreamCost *money.Amount

	// ClientCanceled 为 true 表示这次请求是客户端在流式响应中途断开的。
	//
	// 落进 billing_requests.client_canceled_at（见 internal/ledger/store
	// 的 markSettled）。它与 UsageSource 描述的是**两件事**：客户端断开时
	// 用量确实是估的，但上游中途报错时用量同样是估的，而那不是客户端断的
	// ——两者在对账与告警上是两回事（见 Outcome.ClientCanceled 的注释）。
	ClientCanceled bool

	// UpstreamStatus 与 ErrorCode 是这次响应的上游侧依据，由 Settle 从
	// Outcome 原样搬过来，落进 billing_requests 的同名两列（见
	// internal/ledger/store 的 markSettled）。
	//
	// 已计费的行上只可能是 ErrorUpstreamStreamTruncated 那一种（见
	// Outcome 的注释）：正常结束、客户端断开、插件终止都不带依据。零值与
	// 空串在这里的意思就是「这次没有依据可写」，落库时被 NULLIF 折成
	// NULL——与不计费那条路（Release）的口径逐字一致。
	UpstreamStatus int
	ErrorCode      string

	// Duration 是这次上游调用的耗时，由 Settle 从 Outcome 搬过来。
	//
	// 它唯一的下游是 usage_events.duration_ms（见 internal/ledger/store 的
	// insertUsageEvent）：渠道健康度的延迟那一维从那张表按小时桶取分位数，
	// 于是「一次请求花了多久」只在账本的用量底稿上留一份。零值表示没测到，
	// 落库层写 NULL——理由见 Outcome.Duration。
	Duration time.Duration

	// FirstToken 是「受理到第一个内容块」的时长，由 Settle 从 Outcome
	// 原样搬过来，落进 billing_requests.first_token_ms。只有流式请求
	// 有值；零值表示没测到，落库层写 NULL——理由见 Outcome.FirstToken。
	FirstToken time.Duration

	// Tier 是规则记下的档位名，供账单解释「为什么按这一档算」。
	Tier string
	// PricedAt 是这次求值用的时刻，它记的是**受理时刻**
	// （Reservation.RuleMatchedAt），不是结算这一刻：档位按受理那一刻选，
	// 任何事后复算都必须从同一个时刻出发。拿结算时刻去复算会算出另一个
	// 档位的价——两个价看起来都合理，差别只在用的是哪一刻，所以靠账单
	// 本身分不出来。
	//
	// 它与下面的 LocalTime 一起落进 usage_events（priced_at 与
	// local_hour / is_off_day，见 internal/ledger/store 的 Save）：「这笔钱
	// 是按哪一刻、哪一档算的」事后读得出来，不必再猜。复算入口本身还没写，
	// 但复算要的输入从这一刻起留痕了。
	PricedAt time.Time
	// LocalTime 是这次求值用到的「当地」口径：地区时区的钟点与那一天算不算
	// 休息日，为复算预留。
	//
	// 它与 PricedAt 描述的是**同一次求值**，取值也来自同一次求值的结果：
	// Settle 从 val.LocalTime 搬过来，与 Prices、Tier、PricedAt 出自同一个
	// expr.Result。所以「写进库的当地钟点」与「规则里 isPeak / isWorkday
	// 当时判的那一档」是同一个值，而不是在这里按 Region 另算一遍凑出来的
	// ——「At 折成当地哪一天」这一步在 expr 的求值运行时里，在这里重写一遍
	// 等于把一条计费判据抄成两份，而两份迟早会不一致。把结算时刻的钟点配上
	// 受理时刻的 PricedAt 同理：那是一个从未发生过的组合，而它照样算得出
	// 数来，照它对账会得到一个错的结论，且看不出错。
	//
	// 规则没有用到地区那一套口径（isWorkday / isHoliday / isPeak 一个都没走）
	// 时为 nil，落库层写 NULL——「不知道」，而不是把零值写成「当地 0 点、
	// 不是休息日」（那种值同样算得出数来）。判据在 expr.Result.LocalTime 上。
	LocalTime *expr.LocalTime
}

// Reserve 计算并返回这次请求的预留。
//
// # 上界按**候选集**取
//
// 预留发生在选路之前，所以「这次请求会按哪条规则计价」还没有答案——
// 手上只有一个候选集。此时取上界的口径是：对**每个候选**取它自己适用
// 的那一条规则，各求一次最坏情况，取其中**最大**的金额。
//
// 只有一个候选会真正服务这次请求，所以是取最大，不是求和。
//
// 不能只按平台规则算：渠道级与商家级规则可以比平台规则贵，而按平台
// 规则预留会让预留小于结算。那时结算撞上 Capped，实际金额被预留截住，
// 差额由平台吃掉——账目仍然是平的，所以这是**静默少收**。
//
// 候选集为空时**拒绝**，而不是退回去按平台规则算：候选里的规则可能
// 一条比一条贵，按空集算出来的任何数都是下界而不是上界。这条路径真实
// 可达——还没选路就调到这里，或者上游一个可用渠道都没有（网关在更前面
// 就会拦下空候选，见 gateway.begin，但本函数是公开的，不能假设调用方
// 一定拦过）。
//
// # 输出侧按声明的**整个**上界冻
//
// 这是非流式那条路的形状：调用方声明了多少就冻多少。流式那条路不这样，
// 见 ReserveStream。
func (p *Pipeline) Reserve(ctx context.Context, req Request) (*Reservation, error) {
	return p.reserve(ctx, req, req.MaxTokens)
}

// ReserveStream 计算一次**流式**请求的初始预留。
//
// # 与非流式只差输出侧那一项
//
// 输出侧只按**第一段**（step 个 token，不超过声明的 max_tokens）冻，之后边
// 转发边按本地累计的真实产出补冻（见 ReserveMore 与 #498）。输入侧一字不改：
// 它本来就不按 max_tokens 估，而是按请求前算得出来的输入 token 数
// （见 billing.Reserve 里输入侧那一段）。
//
// 要解决的是：max_tokens 是**调用方给的值**，填大是常态（不少 SDK 的默认值
// 就很大）。按它冻满时，一个余额够付实际消耗的账户发一条 max_tokens=100K、
// 实际只产出几十个 token 的流式请求，光预留就冻掉一大笔——单发一条就被
// 「可用余额不足」拒掉，而拒绝的理由与真实原因被抹成同一句话。
//
// # 上界的性质变了，说清楚
//
// 非流式的预留是**整笔结算的上界**。流式的初始预留不再是：已产出那部分的
// 上界由「初始段 + 随流补冻的那些段」共同撑着，而每一步都按同一份规则的最坏
// 情况估（见 ReserveMore）。不变的是「余额不为负」——补冻与初始预留走**同一条**
// 准入谓词（见 internal/ledger/store 的 AddReservation），够不着就中断这条流。
func (p *Pipeline) ReserveStream(ctx context.Context, req Request, step int64) (*Reservation, error) {
	if step <= 0 {
		return nil, fmt.Errorf("%w: 递进冻结的阈值段必须为正，当前 %d", ErrNoFreezeStep, step)
	}
	// 第一段不超过声明的上界：max_tokens 比一个段还小时（例如 100），冻满它
	// 就是冻满整笔请求——那种请求本来也撞不上这条要修的问题（一百个 token 的
	// 预留拦不住任何人）。
	return p.reserve(ctx, req, min(req.MaxTokens, step))
}

// reserve 是 Reserve 与 ReserveStream 共用的那一段，差别只有输出侧按多少
// token 估（见上面两个函数的说明）。
//
// outputTokens 由调用方决定，而两条路各自的口径写在它们自己的注释里：
// 非流式传声明的上界，流式传第一段。**它不是「可以随手传个数」的旋钮**——
// 只许来自那两处，别处传一个更小的数就是在收窄上界。
func (p *Pipeline) reserve(ctx context.Context, req Request, outputTokens int64) (*Reservation, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	if len(req.Candidates) == 0 {
		return nil, fmt.Errorf("%w: 候选渠道集合为空，算不出预留上界", ErrNoRule)
	}

	// 求值环境只造一次：所有候选要在**同一个时刻**下比较，否则跨整点的
	// 请求会拿两个时刻去算同一笔钱，而取最大就不再是在同一个条件下取的。
	//
	// 时刻取受理这一刻，并随 RuleMatchedAt 一起交给结算——预留与结算
	// 用同一个时刻，是「一次请求的价格在受理时就定了」这句话的落点。
	evalEnv := p.env(req.Usage, p.now())

	rules, err := p.Rules.MatchAll(ctx, RuleQuery{
		CanonicalModel: req.CanonicalModel,
		Candidates:     req.Candidates,
		At:             evalEnv.At,
	})
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%w: 候选渠道 %d 个，但没有一条适用的计价规则",
			ErrNoRule, len(req.Candidates))
	}

	// 逐个候选求上界，留最大的那一个。留下的是**整条**候选而不只是金额：
	// Reservation 要带走给出这个上界的那条规则——见 Reservation.RuleID。
	//
	// 缓存记录按候选**各自**查：不同渠道的缓存是各自独立的（见
	// billing.CacheStore 的隔离口径），拿其中一条的命中量给所有候选共用，
	// 就会把别的渠道的命中算到没有命中的渠道头上。
	var best *candidateBound
	for _, rule := range rules {
		hit := p.cache.Resolve(req.APIKeyID, rule.ChannelID, req.CanonicalModel, req.CachePrefix, evalEnv.At)
		bound, err := p.boundOf(rule, evalEnv, req, outputTokens, hit)
		if err != nil {
			return nil, err
		}
		if best == nil || bound.amount.Cmp(best.amount) > 0 {
			best = &bound
		}
	}

	return &Reservation{
		RequestID:       req.RequestID,
		UserID:          req.UserID,
		Asset:           req.Asset,
		Amount:          best.amount,
		RuleID:          best.rule.ID,
		PricingExprSnap: best.rule.Source,
		// 冻住了多少个输出 token（见字段的注释）：两条路上它都是「按这个数
		// 估出来的」，所以由这里统一填，调用方不必自己再算一遍 min。
		OutputTokens:  outputTokens,
		RuleMatchedAt: best.at,
	}, nil
}

// candidateBound 是**一条**候选规则给出的上界。
type candidateBound struct {
	rule   Rule
	amount money.Amount
	at     time.Time
}

// boundOf 对一条候选规则求最坏情况，算出它对应的预留金额。
//
// 预留用**最坏情况**求值：每个价格位置取所有分支里的最高价。
//
// 不用「按当前时刻求一次」：规则可以按时间、按缓存命中与否分档，
// 而一次普通求值只给出其中一支的价。用那一支去预留，
// 结算走到更贵的那一支时就会超过预留。
//
// outputTokens 是这次要冻的输出 token 数，由调用方按各自的口径传
// （见 reserve 的注释）——它只影响金额，不影响规则怎么选。
//
// confirmedCacheTokens 是这条候选上**已确认**会命中缓存的输入 token 数
// （见 billing.CacheStore.Resolve）——它只从输入侧最贵的那一段里拆出
// 一部分按缓存价估，同样不影响规则怎么选。
func (p *Pipeline) boundOf(rule Rule, env expr.Env, req Request, outputTokens, confirmedCacheTokens int64) (candidateBound, error) {
	program, err := p.programs().Get(rule.CacheKey(), rule.Source)
	if err != nil {
		return candidateBound{}, fmt.Errorf("%w: 规则 %d 无法编译: %w", ErrRuleBroken, rule.ID, err)
	}

	worst, err := program.EvalWorstCase(env)
	if err != nil {
		return candidateBound{}, fmt.Errorf("%w: %w", ErrRuleBroken, err)
	}

	amount, err := billing.Reserve(billing.ReserveRequest{
		Prices:      worst.Prices,
		Charges:     worst.Charges,
		InputTokens: req.InputTokens,
		// 已确认命中的那一段从输入侧最贵单价里拆出来按缓存价估，见
		// billing.Reserve 里输入侧那一段。为 0 时结果与拆分之前逐位相同。
		ConfirmedCacheTokens: confirmedCacheTokens,
		// 缓存读与缓存写各按什么价估，见 billing.Reserve 的注释——
		// 上界的构成写在那里，这里只负责把两个量传进去。
		CacheReadTokens:  req.Usage.CacheReadTokens,
		CacheWriteTokens: req.Usage.CacheWriteTokens,
		OutputTokens:     outputTokens,
	})
	if err != nil {
		return candidateBound{}, fmt.Errorf("计算预留失败: %w", err)
	}

	return candidateBound{rule: rule, amount: amount, at: worst.At}, nil
}

// ReserveMore 算出「又产出了 outputTokens 个输出 token」要追加冻结多少。
//
// # 单价与结算**同源**，而且必须是同一份
//
// 补冻的全部意义是让冻结额跟着真实敞口走。用一份与结算无关的价，冻出来的数
// 既可能盖不住结算（撞上 Capped、平台吃掉差额），也可能过量（把 #498 要修的
// 那个问题换个地方复发）。所以这里按**实际选中的那个渠道**取规则，查询条件与
// 受理时刻与 Settle 里那次 Match 逐字相同——结算用的是哪条规则，补冻就用哪条。
//
// 不能拿 Reservation.RuleID 那条规则来算：那是**受理时给出最大金额的那条**
// 候选规则，而结算按实际选中的渠道重新匹配，两者可能不是同一条（渠道级与
// 商家级规则都算数，见 Reservation.RuleID 的说明）。用它会得到「补冻用一份、
// 结算用另一份」。
//
// # 为什么求最坏情况而不是按当前用量求一次
//
// 与预留同一个理由：规则可以按用量或缓存分档，而一次普通求值只给出其中一支
// 的价。最坏情况（每个价格位置取各支最高价）才是与「结算走哪一支」无关的上界
// ——单价不随用量变化是本包强制的（见 expr.EvalWorstCase）。
//
// # 它只算输出侧那一项
//
// 输入侧、固定费、最低消费与非 token 计量都是一次请求只发生一次的量，初始
// 预留里已经各算过一份。按段重复加会让预留越滚越大，而多冻的正是这条要修的
// 东西（见 billing.ReserveOutput）。
func (p *Pipeline) ReserveMore(ctx context.Context, req Request, reserved *Reservation, outputTokens int64) (money.Amount, error) {
	if reserved == nil {
		return money.Zero, errors.New("缺少预留信息：补冻必须基于一次真实的预留")
	}
	if reserved.RequestID != req.RequestID {
		return money.Zero, fmt.Errorf("预留属于请求 %q，与当前的 %q 不符", reserved.RequestID, req.RequestID)
	}
	// 与 Settle 里那条同样的防御：受理时刻是定价基准，缺了它就只能退回读墙钟，
	// 而那正是「一次请求的价格在受理时就定了」这条口径要消除的东西。
	if reserved.RuleMatchedAt.IsZero() {
		return money.Zero, errors.New("预留缺少受理时刻：补冻要按受理那一刻的档位定价")
	}
	if outputTokens <= 0 {
		// 调用方只在「确实要补」时才调它。传 0 进来是一个真值错误的信号，
		// 静默返回 0 会让「补冻了但金额是零」看起来像成功。
		return money.Zero, fmt.Errorf("要补冻的输出 token 数必须为正，当前 %d", outputTokens)
	}
	if err := req.validate(); err != nil {
		return money.Zero, err
	}

	// 候选集收窄成**实际选中的那一个渠道**，与 Settle 逐字相同：req.ChannelID
	// 与 req.OwnerUserID 由调用方在选路之后填上（见 gateway 的 settledRequest）。
	rule, err := p.Rules.Match(ctx, RuleQuery{
		CanonicalModel: req.CanonicalModel,
		Candidates: []RuleCandidate{{
			ChannelID:   req.ChannelID,
			OwnerUserID: req.OwnerUserID,
		}},
		At: reserved.RuleMatchedAt,
	})
	if err != nil {
		return money.Zero, fmt.Errorf("补冻时重新取规则失败: %w", err)
	}

	program, err := p.programs().Get(rule.CacheKey(), rule.Source)
	if err != nil {
		return money.Zero, fmt.Errorf("%w: 规则 %d 无法编译: %w", ErrRuleBroken, rule.ID, err)
	}
	worst, err := program.EvalWorstCase(p.env(req.Usage, reserved.RuleMatchedAt))
	if err != nil {
		return money.Zero, fmt.Errorf("%w: %w", ErrRuleBroken, err)
	}

	amount, err := billing.ReserveOutput(worst.Prices, outputTokens)
	if err != nil {
		return money.Zero, fmt.Errorf("计算补冻金额失败: %w", err)
	}
	return amount, nil
}

// ErrNoFreezeStep 表示流式递进冻结的阈值段不是一个可用的数（非正数）。
//
// 阈值段同时是「多久检查一次」与「冻结额最多领先已产出量多少」（见
// gateway.Gateway.StreamFreezeStep）。零值不是「不限」也不是「每次事件都查」：
// 前者让补冻整条不发生（流式又退回按声明上界冻，或者干脆不冻），后者把数据库
// 往返摊到每一个事件上——两种都不会报错，只会安静地跑错，所以在这里挡掉。
var ErrNoFreezeStep = errors.New("递进冻结的阈值段不可用")

// ErrInsufficientAvailable 表示「余额 − 已冻结」不够这一次冻结。
//
// # 它为什么在这里，而不在落库层
//
// 它的产生点是落库层的冻结语句（见 internal/ledger/store 的 SaveReservation
// 与 AddReservation：更新影响 0 行就是余额不够），但**读**它的有两层：入口层
// （把一个预留在开始之前就失败的请求回成 429，见 internal/api）与网关（流式
// 补冻失败时据此中断这条流，见 internal/gateway/stream.go）。
//
// 网关不认识落库层——它只声明一个 ReservationStore 接口（见 gateway 的注释：
// 数据面依赖网关会把整条入口链路拖进去，见 scripts/check-layering.sh）。所以
// 这个错误值只能住在两边都认识的包：本包。落库层把它重新导出一次（值相同），
// 两处的 errors.Is 才都成立。
//
// 它与 ErrRuleBroken 那一类不同：那是**我们这边**坏了（配置错误、编译不过），
// 这个是账户状态，处置是让人去充值，不是重试。
var ErrInsufficientAvailable = errors.New("可用余额不足")

// Settle 按实际结果结算并生成账本分录。
//
// reserved 来自同一个 Request 的 Reserve，**必须是它返回的那个金额**。
// 传一个自己算的数会让「预留是上界」这条不变量失效，而失效的表现是
// 结算超过预留、余额变负。
func (p *Pipeline) Settle(ctx context.Context, req Request, reserved *Reservation, out Outcome) (*Charge, error) {
	if reserved == nil {
		return nil, errors.New("缺少预留信息：结算必须基于一次真实的预留")
	}
	if reserved.RequestID != req.RequestID {
		return nil, fmt.Errorf("预留属于请求 %q，与当前的 %q 不符", reserved.RequestID, req.RequestID)
	}
	if err := req.validate(); err != nil {
		return nil, err
	}

	// 上游失败不计费。这是硬规则，放在结算的最前面：
	// 后面任何一处计算都不该有机会改变这个结论。
	if out.UpstreamFailed {
		// 不计费的结局也要有数：它是「上游在失败」与「计数没接上」之间
		// 唯一的区别（见 observe.Metrics 的注释）。
		observe.Metrics.UnbilledSettlements.Inc()
		return nil, &UpstreamFailure{
			RequestID: req.RequestID,
			Status:    out.UpstreamStatus,
			Code:      out.ErrorCode,
		}
	}

	// 结算的定价基准就是它（见下面求值那一步），没有它就只能退回去读墙钟，
	// 而那正是本次要消除的口径。报出来而不是兜一个 p.now()：一个自己编的
	// 时刻会让跨档位的请求按另一档计价，而账单上看不出用了哪个时刻。
	//
	// 它排在「上游失败不计费」**之后**，因为那条是硬规则：一次不计费的请求，
	// 定价基准缺不缺都不该改变结论，更不该把一次上游失败降级成一句普通错误
	// （调用方要靠 `*UpstreamFailure` 认出「这笔钱放回去而不是收掉」）。
	//
	// 这条分支是**防御性的**，当前真实路径造不出来：Reservation 只有一个构造点，
	// RuleMatchedAt 取自 p.now()；预留也不落库、不从库里读回（SaveReservation
	// 只把它写进 billing_requests.started_at，没有反向的加载路径）。只有测试
	// 能手工把它置零（见 TestSettleRejectsReservationWithoutInstant）。
	if reserved.RuleMatchedAt.IsZero() {
		return nil, errors.New("预留缺少受理时刻：结算要按受理那一刻的档位定价")
	}

	// 结算用规则**当时**的那份。预留与结算必须用同一个规则版本——
	// 规则在请求在途期间被改过的话，用新规则结算会算出与预留无缘由的差额。
	//
	// 候选集在这里收窄成**实际选中的那一个渠道**：结算按它匹配，
	// 而不是按预留时的那一整个候选集。实际选中的渠道必是候选之一，
	// 而它的规则在预留取上界时算过，所以结算不会超过预留。
	rule, err := p.Rules.Match(ctx, RuleQuery{
		CanonicalModel: req.CanonicalModel,
		Candidates: []RuleCandidate{{
			ChannelID:   req.ChannelID,
			OwnerUserID: req.OwnerUserID,
		}},
		At: reserved.RuleMatchedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("结算时重新取规则失败: %w", err)
	}

	program, err := p.programs().Get(rule.CacheKey(), rule.Source)
	if err != nil {
		return nil, fmt.Errorf("%w: 规则 %d 无法编译: %w", ErrRuleBroken, rule.ID, err)
	}

	// 拆分会把「推理量大于输出总量」这种自相矛盾钳掉，钳过之后就看不出
	// 上游报的数对不上——所以要在拆分**之前**问一次。
	//
	// 只告警，不让这次请求失败：这是运行时来自上游的数据状况，不是几分钟能
	// 修好的配置错误。让它失败会把整条渠道的请求都变成失败，而上游那次调用
	// 已经成功、token 已经花掉，客户端还会看到错误——白服务一次还赔上体验。
	// 计费照走，按钳过的值算。
	if billing.ReasoningExceedsCompletion(out.Usage) {
		observe.FromContext(ctx).Warn("上游报的推理 token 多于输出总量，而用量声明推理含在输出里",
			"channel_id", req.ChannelID,
			"completion_tokens", out.Usage.CompletionTokens,
			"reasoning_tokens", out.Usage.ReasoningTokens,
		)
	}

	usage := billing.FromChannelUsage(out.Usage)
	// 求值时刻取**受理那一刻**，不取现在：这次请求的价格在受理时就已经
	// 定下来了（见 env）。上游那次调用可能跑了好几分钟，跨过了规则的档位
	// 边界——按结算这一刻的档位算，价格就取决于上游快不快：同一个请求，
	// 上游答得快与答得慢会落到两个档位上，而两个价看起来都合理。
	val, err := program.Eval(p.env(usage, reserved.RuleMatchedAt))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRuleBroken, err)
	}

	settlement, err := billing.SettleWithCharges(usage, val.Prices, val.Charges, reserved.Amount)
	if err != nil {
		return nil, fmt.Errorf("结算失败: %w", err)
	}

	// 上游成本与这笔售价是两件事：它由另一条规则线（upstream_costs）算出，
	// 而且**算不出来不影响这次结算**——那是一次观测缺口，不是计费错误。
	// 求值时刻与售价那一侧相同（val.At，也就是受理时刻），于是同一个请求
	// 的售价与成本问的是同一刻的档位。
	upstreamCost := p.upstreamCost(ctx, req, usage, val.At)

	entries, err := p.entries(ctx, req, val, settlement, reserved.UserPlanID)
	if err != nil {
		return nil, err
	}

	// 结算真的走完了。它是下面那条 Capped 的分母——只有这个数同时在涨，
	// 「Capped 一直是 0」才说明真的没撞上，而不是计数器没接上。
	observe.Metrics.Settlements.With(out.UsageSource.String()).Inc()
	// 撞上预留上界：按设计不该发生，一旦成为常态就是静默少收，而账目仍然是平的。
	//
	// 按协议分：各条入口的上界来源不同（chat 那几条按 max_tokens 与正文
	// 估算，决策表面按 internal/systemone 的两个常量），混成一条序列时
	// 分不出是哪一条协议在漏。协议由入口填在 Request.Protocol 上，这里
	// 只负责把它带进标签——不在这里猜。
	//
	// 同一次判断里再记一条**金额侧**的：撞了几次答不了「少收了多少钱」，
	// 而后者才是平台真正承担掉的那笔钱。差额是 Breakdown.Total − Actual
	// （未截断的按用量金额 − 实际收的金额），也就是落库时 computed_amount −
	// final_amount 的同一个差——两处都从 Settlement 自己的字段减，不在这里
	// 另存一份，见 billing.Settlement 的类型注释。
	//
	// 差额在 Capped 为真时必然为正：Capped 的定义就是 Breakdown.Total >
	// Reserved，而 Actual 此时等于 Reserved。所以这一条序列被创建出来时
	// 必然带着一个正数——不会出现「有一条序列，但它是 0」这种读不出意思的行。
	//
	// 金额这条也不按请求分标签（那是无界集合），要查具体是哪一次请求撞的、
	// 撞了多少，去库里看 billing_requests 的 computed_amount。
	if settlement.Capped {
		observe.Metrics.CappedSettlements.With(string(req.Protocol)).Inc()
		observe.Metrics.CappedAmount.
			With(string(req.Protocol), req.Asset).
			Add(uint64(settlement.Breakdown.Total.Sub(settlement.Actual).Units()))
	}

	// 记下这次请求的块序列与**实测缓存量**，供下一次预留判断缓存前缀。
	//
	// 只记实测用量（tokens.SourceMeasured）：估算值不是上游那一段落了多少
	// token，拿它当命中量会把输入侧的上界压到实际之下——结算撞 Capped，
	// 差额由平台承担。没实测就等于没有依据，宁可下次多冻一点。
	//
	// 记的是上游报出的缓存量（cache_read + cache_write），不是整段输入：
	// 没有缓存活动的上游（两项都是 0）不能记成「整段都会命中」——那类渠道的
	// 规则通常把缓存价填 0，下一次预留的输入侧会直接归零。这个 0 由
	// CacheStore.Record 自己的判据挡掉（缓存量不为正就不记）。
	//
	// 写在这里而不是更早：上面任何一处失败都意味着这次请求没有结算，
	// 而记录的口径是「上一次**已结算**的请求」。
	if out.UsageSource.IsExact() {
		settledAt := p.now()
		p.cache.Record(req.APIKeyID, req.ChannelID, req.CanonicalModel, billing.CacheRecord{
			Blocks:       req.CachePrefix,
			CachedTokens: usage.CacheReadTokens + usage.CacheWriteTokens,
			SettledAt:    settledAt,
		}, settledAt)
	}

	return &Charge{
		RequestID:       req.RequestID,
		UserID:          req.UserID,
		Asset:           req.Asset,
		Settlement:      settlement,
		Entries:         entries,
		RuleID:          rule.ID,
		PricingExprSnap: rule.Source,
		UsageSource:     out.UsageSource,
		Usage:           usage,
		// 单价从这次求值的结果原样搬过来，是 usage_events.unit_prices 唯一的
		// 来源。取 val.Prices（这次结算**按哪一组价**算的），不取
		// settlement.Breakdown：后者是金额（单价 × 用量），撞上界时还留着
		// 截之前的值。理由与边界见 Charge.Prices 的注释。
		Prices: val.Prices,
		// 上游成本另算一次，是 usage_events.upstream_cost 唯一的来源。
		// nil 表示算不出来（没有规则，或规则坏了），落库层写 NULL。
		UpstreamCost:   upstreamCost,
		ClientCanceled: out.ClientCanceled,
		// 上游侧的依据原样带过去，落库层照着写 billing_requests 那两列。
		// 已计费的行上它只可能是截断那一种——取舍在网关那一侧做，见
		// Outcome 的注释；这里不判，判两次就成了两份会各自漂移的规矩。
		UpstreamStatus: out.UpstreamStatus,
		ErrorCode:      out.ErrorCode,
		// 耗时与金额无关，原样带过去：它只在落库时变成 usage_events 的一列，
		// 延迟那一维的输入就从那里按桶聚出来。
		Duration: out.Duration,
		// TTFT 同样与金额无关，落的是 billing_requests 那一列：它是
		// 「一次请求」的形状，不是一次用量。非流式是零值，落库层写 NULL。
		FirstToken: out.FirstToken,
		Tier:       val.Tier,
		// val.At 是这次求值用的时刻，也就是受理时刻（见 env）。
		PricedAt: val.At,
		// 当地那两个值同样从这次求值原样搬过来：写进 usage_events 的两格
		// （local_hour / is_off_day）因此与上面那组价、那个档位出自同一次
		// 求值，而不是结算这一侧按 Region 重算的。规则没用上地区那一套时
		// 是 nil，落库层写 NULL——理由见 Charge.LocalTime。
		LocalTime: val.LocalTime,
	}, nil
}

// upstreamCost 算这次请求在上游一侧的成本，算不出来时返回 nil。
//
// # 算不出来不影响结算
//
// 成本是**观测值**，不是收费依据：算不出来时这次请求照常结算，只是
// usage_events.upstream_cost 留 NULL。这不是兜底，而是两类「没有数」的
// 明确区分——NULL 读作「不知道」，0 读作「上游不要钱」。用 0 顶替会让
// 毛利虚高，而账面仍然是平的，事后看不出来。
//
// # 两种来路的处置不同
//
//   - 没有生效的成本规则：正常的配置状态（多数渠道不配成本价），静默留 NULL。
//   - 规则存在却算不出来：编译不过、求值失败、或者规则漏了这次用量的某个
//     单价（billing.ValidateCharges 会拒）。写一条告警——否则它在库里与
//     「没配」长得一模一样，而配了规则正是有人要看见它的信号。
//
// 与售价那一侧共用同一个表达式方言、同一个编译缓存与同一份用量口径：
// 成本规则写的也是「一组单价（可含一次性费用）」，由这里乘实际用量。
func (p *Pipeline) upstreamCost(ctx context.Context, req Request, usage billing.Usage, at time.Time) *money.Amount {
	if p.Costs == nil {
		return nil
	}

	rule, err := p.Costs.MatchCost(ctx, CostQuery{
		ChannelID:      req.ChannelID,
		CanonicalModel: req.CanonicalModel,
		At:             at,
	})
	if errors.Is(err, ErrNoCostRule) {
		return nil
	}
	if err != nil {
		p.warnCost(ctx, req, CostRule{}, err)
		return nil
	}

	program, err := p.programs().Get(rule.CacheKey(), rule.Source)
	if err != nil {
		p.warnCost(ctx, req, rule, fmt.Errorf("%w: 规则 %d 无法编译: %w", ErrRuleBroken, rule.ID, err))
		return nil
	}
	val, err := program.Eval(p.env(usage, at))
	if err != nil {
		p.warnCost(ctx, req, rule, fmt.Errorf("%w: %w", ErrRuleBroken, err))
		return nil
	}

	// 与售价那一侧同一条校验：用量里有、规则却没给价的那一项，乘出来是 0，
	// 成本因此被低估。那是一个「算得出来但算得不对」的数，比 NULL 更坏。
	if err := billing.ValidateCharges(usage, val.Prices, val.Charges); err != nil {
		p.warnCost(ctx, req, rule, err)
		return nil
	}

	total := billing.ComputeWithCharges(usage, val.Prices, val.Charges).Total
	return &total
}

// warnCost 把一次「配了成本规则却算不出来」报出来。
//
// 不报「没有规则」那一档：那是最常见也最正常的状态，逐请求打一条会把日志
// 淹没，而真正需要看见的那几条反而找不到了。
func (p *Pipeline) warnCost(ctx context.Context, req Request, rule CostRule, err error) {
	observe.FromContext(ctx).Warn("上游成本算不出来，这次记 NULL",
		"channel_id", req.ChannelID,
		"canonical_model", req.CanonicalModel,
		"cost_rule_id", rule.ID,
		"err", err,
	)
}

// env 构造求值环境。
//
// # 求值时刻是显式入参，**不读墙钟**
//
// 一次请求的价格在它**被受理的那一刻**就定下来了：那一刻的钟点与日历日
// 决定了规则里走哪一档，而档位决定单价。之后无论隔多久才结算、中间跨过
// 几次档位边界，都按那一刻的档位算。
//
// 两个调用点因此各传各的时刻，而它们拿到的是**同一个**：
//
//   - Reserve 传 p.now()——受理时刻就是「现在」，它同时被记进
//     Reservation.RuleMatchedAt；
//   - Settle 传 reserved.RuleMatchedAt——结算复用它，而不是重新读一次钟。
//
// 读墙钟会让同一次请求在预留与结算时拿到两个时刻，于是跨整点的请求按
// 低谷预留、按高峰结算——同一笔钱有两个答案，而它们本应是同一个。
// 这也是 expr 包把时间做成显式入参的原因（见 expr.Env.At）。
//
// 时刻为零值时求值会直接失败（见 expr.Env.validate）：一个按公元 1 年
// 选出来的档位，算出的价看不出错。
func (p *Pipeline) env(usage billing.Usage, at time.Time) expr.Env {
	return expr.Env{
		Usage:    usage,
		At:       at,
		Region:   p.Region,
		Calendar: p.Calendar,
	}
}

// programs 取编译缓存，没显式给时兜底建一个。
//
// 兜底那一次走 sync.Once，不用裸的 `if p.Programs == nil`：Pipeline 由
// gateway 单实例共用，两个 goroutine 同时进来时那个判断就是同一块内存上
// 的无同步读写。**它不报错**——表现是两个 goroutine 各建一份缓存，
// 于是编译缓存静默失效（编译一次约 130 µs，一次请求要跑两次求值），
// 而这类「没坏但没生效」正是最难发现的一种。
//
// 缓存本身是并发安全的（见 expr.Cache），这里要挡的只是**建它**这一步。
func (p *Pipeline) programs() *expr.Cache {
	p.programsOnce.Do(func() {
		if p.Programs == nil {
			p.Programs = expr.NewCache(defaultCacheSize)
		}
	})
	return p.Programs
}

// defaultCacheSize 是编译缓存的默认容量。
//
// 规则数远小于它，所以正常不会触发淘汰；给一个上限是为了防止
// 「商家反复改规则」把内存吃满。
const defaultCacheSize = 256

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// entries 把结算结果变成账本分录。
//
// 一次消费的账面形状：用户出钱（贷 user）、商户应付增加（借 merchant_payable）、
// 平台留收入（借 platform_revenue）。三边由 ledger.Consume 从「售价 − 分成」
// 推出来，所以天然平。
//
// 上游成本**不在**这里：它由另一条规则线算出，落在 usage_events 那一列上，
// 是观测值而不是分账依据（见 upstreamCost 的注释）。
func (p *Pipeline) entries(ctx context.Context, req Request, val expr.Result, s billing.Settlement, planID int64) ([]ledger.Entry, error) {
	// 金额为零时不写分录。**不是省事，是避免污染账本**：
	// 大量零金额分录会让对账时的「有分录就说明发生过消费」这个判据失效。
	if s.Actual.IsZero() {
		return nil, nil
	}

	// 商户分成从售价里出，按配置的比例、向下取整，零头归平台。
	//
	// 比例读不出来时不在这里报错，由 MerchantShare 记一条告警并按 0 处理：
	// 一笔已经服务完的请求不该因为一个配置项写错而收不了钱（见
	// MerchantShare.BasisPointsFor）。商户为平台自营（0）时它同样是 0。
	//
	// 上游成本**不**在这一层扣：它由另一条规则线算出，落在 usage_events
	// 那一列上是观测值，不是分账依据（见 upstreamCost 的注释）。
	share := money.Zero
	if p.MerchantShare != nil {
		if bp := p.MerchantShare.BasisPointsFor(ctx, req.OwnerUserID); bp > 0 {
			share = s.Actual.MulBasisPoints(bp)
		}
	}

	// 预留快照里有订阅时，这条消费从 prepaid:<订阅 id> 扣；没有时走用户
	// 钱包。两条路的借贷形状只差用户侧这一个科目（见 ledger.Consume）。
	//
	// 0 不能直接喂给 PrepaidAccount：它拼出来的是 `prepaid:0`，一个既非零值
	// 又不指向任何订阅的科目。留零值表示「走用户钱包」。
	prepaid := ledger.Account{}
	if planID > 0 {
		prepaid = ledger.PrepaidAccount(planID)
	}

	consume, err := ledger.Consume(ledger.ConsumeParams{
		TxnID:          ledger.NewTxnID(),
		UserID:         req.UserID,
		Asset:          ledger.Asset(req.Asset),
		Amount:         s.Actual,
		PrepaidAccount: prepaid,
		// 分成从售价里出，余下的全部记作平台收入——钱在账本上只有这一种
		// 分法（见 ledger.Consume 的注释），所以不会出现「分出去的和留下的
		// 加起来不等于售价」。
		MerchantID:       req.OwnerUserID,
		MerchantShare:    share,
		BillingRequestID: req.BillingRequestID,
		// At 取 val.At（受理时刻），与这次定价的依据是同一个时刻：
		// 一笔消费记的就是「哪一次请求、按哪一刻的价」。
		//
		// 它今天到不了库里：分录的 created_at 由写库那一侧用 SQL 的 now()
		// 填（见 ledger/store 的 INSERT），全仓没有读 Entry.CreatedAt 的地方。
		// 仍然传这个值而不是留零值，是因为分录在语义上属于受理那一刻——
		// 留零值的话，将来谁开始读它，读到的会是一个公元 1 年的时间戳
		// （见 LESSONS.md 第 19 条）。
		At: val.At,
	})
	if err != nil {
		return nil, fmt.Errorf("构造账本分录失败: %w", err)
	}
	return consume.Denormalize(), nil
}

// ErrRuleBroken 表示规则本身有问题（编译不过、求值失败）。
//
// 与 ErrNoRule 分开：那个是「没配价」，这个是「配了但坏了」。
// 前者要补一条规则，后者要商家改规则——处置完全不同。
var ErrRuleBroken = errors.New("计价规则不可用")

// UpstreamFailure 表示这次请求因上游失败而不计费。
//
// 它是**返回值**而不是普通错误：调用方需要区分「计费过程出错了」
// 与「这次本来就不该计费」，前者要告警，后者是正常结局。
//
// 它同时是这次失败的**依据**（Status 与 Code 取自 Outcome 的同名字段）。
// 不计费的结局不写账本，调用方要留下这份依据，只能从它这里拿——
// 落点是 billing_requests 的 upstream_status 与 error_code（见
// internal/gateway 的 handle.release 与 internal/ledger/store 的 Release）。
type UpstreamFailure struct {
	RequestID string
	Status    int
	Code      string
}

func (e *UpstreamFailure) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("上游失败（%d %s），本次不计费", e.Status, e.Code)
	}
	return fmt.Sprintf("上游失败（%d），本次不计费", e.Status)
}

func (r Request) validate() error {
	if r.Asset == "" {
		// 这里**没有**兜底值，是刻意的。原先有一个写死的 "CNY"，理由写着
		// 「配置没加载时的兜底」——而配置缺键时 settings.Load 直接拒绝启动，
		// 那个场景根本到不了这里。它实际的作用只是让「入口忘了填」表现为
		// 一笔人民币分录，与正常的完全一样。
		return errors.New("缺少资产品种：结算写进哪个钱包由它决定，入口要从配置里填进来")
	}
	if r.RequestID == "" {
		return errors.New("缺少请求标识：它是幂等键，重复计费必须只扣一次")
	}
	if r.UserID == 0 {
		return errors.New("缺少用户标识")
	}
	if r.CanonicalModel == "" {
		return errors.New("缺少规范模型名")
	}
	if r.MaxTokens <= 0 {
		// 没有 max_tokens 就算不出预留上界，而预留必须有上界。
		return fmt.Errorf("%w: max_tokens 必须为正，当前 %d", billing.ErrNoUpperBound, r.MaxTokens)
	}
	if r.InputTokens < 0 {
		return fmt.Errorf("输入 token 为负: %d", r.InputTokens)
	}
	return nil
}
