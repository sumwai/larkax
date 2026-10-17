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

// Pipeline 是完整的计费流程。
//
// 它由 gateway **单实例并发共用**，所以 Reserve 与 Settle 会同时在多个
// goroutine 上跑。构造完就不要再改它的字段：字段是给装配点填的，
// 装配发生在任何请求之前。
type Pipeline struct {
	Rules RuleSource
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
	UpstreamModel string

	// Candidates 是这次请求**可能**选中的渠道集合，供预留取上界。
	//
	// 预留发生在选路之前，那时还不知道会选中谁（见 ChannelID），所以
	// 上界只能按候选集取。空集合算不出上界，Reserve 会拒绝——见那里的
	// 注释。规则匹配之外没有别处读它。
	Candidates []RuleCandidate
	// ChannelID 与 OwnerUserID 是**实际选中**的渠道与商家：选路之前
	// 为 0，结算时由调用方填上实际的那个。规则匹配之外，账本还要按
	// ChannelID 记下这次请求走的是哪条渠道。
	ChannelID   int64
	OwnerUserID int64

	// InputTokens 是请求的输入 token 估算，由分词层给出。
	InputTokens int64
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
	// 取值只有 channel.ProtocolOpenAI 与 channel.ProtocolAnthropic 两种；
	// 落库层拒绝其他取值（含空值），见 internal/ledger/store 的 SaveReservation。
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
	// RuleMatchedAt 是**受理时刻**：这次请求的价格就是按它在规则里选中的
	// 那一档定下来的。
	//
	// 结算复用它，而不是重新读一次钟（见 Settle）。它是「价格在受理那一刻
	// 定下来」这条口径的载体：跟着预留一起持久化下来，事后才回答得了
	// 「这次是按哪一刻的档位算的」，也才谈得上复算。
	RuleMatchedAt    time.Time
	BillingRequestID int64
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

	// UpstreamStatus 与 ErrorCode 是失败时的依据，供事后核对
	// 「这个错误码当时是不是真的不该计费」。
	//
	// 它们最后落进 billing_requests 的同名两列（见 internal/ledger/store 的
	// Release——不计费的结局只有那一条写库的路）。两者都只在**上游这一侧**
	// 的失败上有值：
	//
	//   - UpstreamStatus 是上游那次 HTTP 响应的状态码。上游没给出状态码时是 0，
	//     落库成 NULL——HTTP 里没有 0 这个状态码，写 0 会读成「上游回了 0 号」。
	//   - ErrorCode 的取值口径见下面那组常量。
	//
	// 不是上游的失败（插件阻断、别名没配）两列都留空：这两列记的是
	// **上游失败**的依据，写进去会让「这一行是不是上游挂的」答错。
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

	// UpstreamCost 是上游侧的成本，供对账。
	UpstreamCost money.Amount
}

// Outcome.ErrorCode 的取值。它们是**我们自己归一化过的分类**，不是上游错误
// 码的原文。
//
// 为什么不用原文：各家上游对同一件事叫的名字不一样，而这一列的用途是事后
// 统计「哪类失败最常见」——按原文统计时，两个名字会把同一件事算成两件，
// 三个部署之间也拼不起来。上游自己那个机器可读的数字没有丢：它在
// UpstreamStatus 里。两列合起来既看得出「哪一类」，也看得出「上游说的几号」。
//
// 要取原文得改 internal/channel：channel.ErrUpstream 今天只带状态码与响应体
// 原文，错误码要从体里解出来才拿得到，而解它的是适配器。
//
// 取值只有下面三个，**都属于「上游这一侧出了事」**。我们这边的失败（插件
// 阻断、别名没配、结算自己出错）不进这一列：这两列是上游失败的依据，
// 把它们混进来，「这一行是不是上游挂的」就答错了。
const (
	// ErrorUpstreamRejected 表示上游回了非 2xx。状态码在 UpstreamStatus 里。
	ErrorUpstreamRejected = "upstream_rejected"

	// ErrorUpstreamNoAnswer 表示上游这一侧没给出可用的回答。它下面其实混着
	// 好几种情形：连不上、TLS 失败、超时、响应体解不开、以及上游回了 2xx
	// 但把错误写在体里（见适配器的 extractEmbeddedError）。
	//
	// 它们合成一档，是因为**在这一层分不开**：channel.ErrUpstream 只带状态码
	// 与响应体原文，上面几种的状态码都是 0，体在「解不开」时也拿不到。要拆开
	// 得让适配器把「拿到过响应吗」记进 ErrUpstream——那要改 internal/channel，
	// 不在这次改动的范围内。宁可给一档诚实的粗分类，也不要按猜的拆细。
	//
	// 此时 UpstreamStatus 是 0（落库成 NULL）：上游没有给出过一个状态码，
	// 编一个出来比留空更坏。
	ErrorUpstreamNoAnswer = "upstream_no_answer"

	// ErrorUpstreamStreamAborted 表示上游接受了请求，但一个 token 都没吐就
	// 结束了（流式）。它发生在响应头之后，所以状态码拿不到——那时只知道它是
	// 2xx，具体是几号没有别的地方记着，UpstreamStatus 因此也是 0。
	ErrorUpstreamStreamAborted = "upstream_stream_aborted"
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

	// ClientCanceled 为 true 表示这次请求是客户端在流式响应中途断开的。
	//
	// 落进 billing_requests.client_canceled_at（见 internal/ledger/store
	// 的 markSettled）。它与 UsageSource 描述的是**两件事**：客户端断开时
	// 用量确实是估的，但上游中途报错时用量同样是估的，而那不是客户端断的
	// ——两者在对账与告警上是两回事（见 Outcome.ClientCanceled 的注释）。
	ClientCanceled bool

	// Tier 是规则记下的档位名，供账单解释「为什么按这一档算」。
	Tier string
	// PricedAt 是这次求值用的时刻，它记的是**受理时刻**
	// （Reservation.RuleMatchedAt），不是结算这一刻：档位按受理那一刻选，
	// 任何事后复算都必须从同一个时刻出发。拿结算时刻去复算会算出另一个
	// 档位的价——两个价看起来都合理，差别只在用的是哪一刻，所以靠账单
	// 本身分不出来。
	//
	// **下面三个字段里只有 PricedAt 会落库。** 它落进 usage_events.priced_at
	// （见 internal/ledger/store 的 Save），那是「这笔钱是按哪一刻的档位算的」
	// 唯一的落点——复算入口本身还没写，但复算要的输入从这一刻起留痕了。
	// LocalHour 与 IsOffDay 目前仍只在内存里：结算路径从不填它们（见下），
	// 落库层因此把对应的两列写成 NULL——「不知道」——而不是把零值写成
	// 「当地 0 点、不是休息日」。
	PricedAt time.Time
	// LocalHour 与 IsOffDay 是同一次求值里用到的分时依据，为复算预留。
	//
	// 它们与 PricedAt 描述的是**同一次求值**，所以三个字段必须取自同一个
	// 时刻：把结算时刻的钟点配上受理时刻的 PricedAt，是一个从未发生过的
	// 组合，而它照样算得出数来——照它对账会得到一个错的结论，且看不出错。
	//
	// 结算路径**从不填这两个字段**（保持零值）：
	//
	//   - 「当地钟点」在规则里不是唯一的值——hour(tz) 的时区由规则作者
	//     逐次给出，同一条规则里可以出现多个时区，也可以一次都不问钟点。
	//     按 Env.Region 的时区填一个上去，会得到一个看起来权威、却可能
	//     与规则实际用的那个不同的数，那比留空更糟。
	//   - IsOffDay 由 Env.Region 与日历唯一确定，但「At 折成当地哪一天」
	//     这一步在 expr 的求值运行时里。在这里重写一遍等于把一条计费判据
	//     抄成两份，而两份迟早会不一致。
	//
	// 要填它们，得先让求值把「用了哪个时区、算的是哪一天」报回来。
	LocalHour int
	IsOffDay  bool
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
func (p *Pipeline) Reserve(ctx context.Context, req Request) (*Reservation, error) {
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
	var best *candidateBound
	for _, rule := range rules {
		bound, err := p.boundOf(rule, evalEnv, req)
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
		RuleMatchedAt:   best.at,
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
func (p *Pipeline) boundOf(rule Rule, env expr.Env, req Request) (candidateBound, error) {
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
		// 缓存读与缓存写各按什么价估，见 billing.Reserve 的注释——
		// 上界的构成写在那里，这里只负责把两个量传进去。
		CacheReadTokens:  req.Usage.CacheReadTokens,
		CacheWriteTokens: req.Usage.CacheWriteTokens,
		MaxTokens:        req.MaxTokens,
	})
	if err != nil {
		return candidateBound{}, fmt.Errorf("计算预留失败: %w", err)
	}

	return candidateBound{rule: rule, amount: amount, at: worst.At}, nil
}

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

	entries, err := p.entries(req, val, settlement)
	if err != nil {
		return nil, err
	}

	// 结算真的走完了。它是下面那条 Capped 的分母——只有这个数同时在涨，
	// 「Capped 一直是 0」才说明真的没撞上，而不是计数器没接上。
	observe.Metrics.Settlements.With(out.UsageSource.String()).Inc()
	// 撞上预留上界：按设计不该发生，一旦成为常态就是静默少收，而账目仍然是平的。
	if settlement.Capped {
		observe.Metrics.CappedSettlements.Inc()
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
		ClientCanceled:  out.ClientCanceled,
		Tier:            val.Tier,
		// val.At 是这次求值用的时刻，也就是受理时刻（见 env）。
		PricedAt: val.At,
	}, nil
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
// 一次消费的账面形状：用户出钱、商户应付增加、平台留收入、上游成本记账。
// 中间隔着一次上游调用，所以「平台先垫付了上游成本」这件事在分录里
// 是显式的——不显式的话，平台的实际毛利只能靠事后减出来。
func (p *Pipeline) entries(req Request, val expr.Result, s billing.Settlement) ([]ledger.Entry, error) {
	// 金额为零时不写分录。**不是省事，是避免污染账本**：
	// 大量零金额分录会让对账时的「有分录就说明发生过消费」这个判据失效。
	if s.Actual.IsZero() {
		return nil, nil
	}

	// 这里只构造「用户被扣了多少」这一侧的完整分录。
	// 分成与上游成本的拆分需要商家分成规则，那属于另一个环节；
	// 留一个显式的 TODO 比在这里猜一个比例好。
	//
	// 猜错的表现是商户拿到的钱不对，而那要等到结算日才发现。
	consume, err := ledger.Consume(ledger.ConsumeParams{
		TxnID:  ledger.NewTxnID(),
		UserID: req.UserID,
		Asset:  ledger.Asset(req.Asset),
		Amount: s.Actual,
		// 分成与上游成本由另一个环节填：那需要商家的分成规则，
		// 而在这里猜一个比例的表现是「商户拿到的钱不对」，
		// 那要等到结算日才发现。这里只记「用户被扣了多少」这一侧，
		// 未拆分的部分留在平台科目上，事后可查。
		MerchantID:       req.OwnerUserID,
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
