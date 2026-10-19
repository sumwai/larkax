// Package quota 跟踪上游账号的剩余额度。
//
// 上游能查额度的少，能查的也各家一个私有端点。所以这里分三层：
//
//	探测层（Prober）    能问就问——上游提供端点时拿到的是实测值
//	自记账层（Tracker）  问不到就自己算——用本地用量累计推算余量
//	调度层（Scheduler）  决定什么时候问谁——探测要花钱花配额，不能无脑轮询
//
// 为什么必须分三层而不是一个「查额度」函数：三类信息源的**可信度不同**，
// 而路由要按可信度区别对待。把估算值当实测值用，会让路由在一个其实已经
// 耗尽的渠道上反复失败；把实测值当估算值用，则浪费了一个准确的信号。
//
// 本包不碰数据库也不发 HTTP：数据来源与持久化都由调用方注入，
// 于是「探测结果怎么合并」这件事可以脱离网络与存储单独测试。
// 合并逻辑是这里最容易出错的地方——它决定「一个 404 之后额度的可信度变成多少」。
package quota

import (
	"fmt"
	"math"
	"time"
)

// Kind 是额度观测方式，取值与数据库的 CHECK 约束一致。
type Kind string

const (
	// KindAPIQuery 是向上游查到的实测值。
	KindAPIQuery Kind = "api_query"
	// KindSelfAccounting 是本地累计推算的估算值。
	KindSelfAccounting Kind = "self_accounting"
	// KindErrorObserved 是从上游错误里观察到的信号，例如「余额不足」。
	// 它不是一次数值测量，而是一次**定性**观察。
	KindErrorObserved Kind = "error_observed"
)

func (k Kind) valid() bool {
	switch k {
	case KindAPIQuery, KindSelfAccounting, KindErrorObserved:
		return true
	}
	return false
}

// 剩余量的单位。这几个值是**同一口径的判据**，不是展示用的标签：
// 路由只对单位完全相同的渠道比较余量（见 internal/route 的 scoreQuota），
// 所以换一种写法不会算错，只是把两个本可比的渠道判成不可比——偏保守。
//
// 这里只列已知的三种。写进来的单位不在其中也合法（上游的口径五花八门），
// 它只是与别人都不可比而已。空字符串表示未知。
const (
	// UnitTokens 是剩余 token 数（按 token 计的套餐）。
	UnitTokens = "tokens"
	// UnitRequests 是剩余请求次数（按次计的配额）。
	UnitRequests = "requests"
	// UnitMoney 是剩余金额，以本部署的记账单位计（见 internal/money）。
	UnitMoney = "money"
)

// State 是一个渠道的额度状态。字段与 channel_quota_state 表对应。
type State struct {
	ChannelID int64

	// Remaining 是剩余量。nil 表示未知——与「是 0」必须分开。
	Remaining *float64
	// Unit 是剩余量的单位（「元」「次」「token」）。
	// 不同渠道的单位不同，混用会算出一个无意义的数。
	Unit string

	// Confidence 是 0..1 的可信度。
	//
	// 路由按它给额度那一项加权：分数是「这次观测说出的分」与中性分 0.5 的
	// 凸组合，可信度越低越靠近中性分（见 internal/route 的 scoreQuota）。
	// 所以它不是展示用的标签——改这里的衰减速度会直接改候选的排序。
	Confidence float64
	// Kind 是最近一次观测的方式。
	Kind Kind

	// Estimated 为 true 表示当前值来自推算而非实测。
	//
	// 读者是插件契约（internal/plugin 的 RouteContextFrom 按它给可信度打折）。
	// 路由打分**不看它**：「推算不如实测可信」这件事 Confidence 已经说了
	// （自记账的上限是 0.8，永远达不到实测的 1），两处一起看等于把同一件事
	// 扣两次（见 internal/route 的 scoreQuota）。
	Estimated bool

	// ResetsAt 是额度重置的时刻，nil 表示不知道或不会重置。
	//
	// 它描述的是**按周期给量**的上游（「每天 1000 次」那类）：额度到点回满。
	// 路由按它给「越接近到期越该先用掉」的分（见 internal/route 的 scoreQuota），
	// 所以它是活的。取值只能来自上游实测或人工录入——自记账推不出这个时刻。
	//
	// 这里早先还有 WindowKind / WindowBudget / WindowUsed 三个字段描述窗口的
	// 形状。它们全仓没有一处赋值、也没有一处读，对应的四列已由
	// 00014_drop_quota_window_columns.sql 从 channel_quota_state 上删掉。
	// 真要接按周期给量的上游时，那几列该按当时的口径重新定，而不是照着
	// 一个从没被验证过的形状填。
	ResetsAt *time.Time

	// LastErrorCode / LastErrorAt 是上游最近一次给出的定性信号。
	LastErrorCode string
	LastErrorAt   *time.Time

	// LastProbeAt 是最近一次**实测**（KindAPIQuery）的时刻。自记账与错误
	// 观测都不动它——它是自记账可信度的衰减基准（见 Merge）。
	LastProbeAt *time.Time

	// LastProbeAttemptAt 是最近一次**尝试**探测的时刻，不论结果如何：拿到
	// 实测值、上游没有查询接口（ErrUnsupported）、探测失败都算。
	//
	// 它与 LastProbeAt 是两列，因为两者回答的问题不同。LastProbeAt 只记
	// 成功的实测，而且是 Routable 的判据（余量为 NULL 而它有值时，这个渠道
	// 被当成「观测过却没拿到数值」排除）；LastProbeAttemptAt 只服务于触发：
	// 按它与探测间隔判断该不该再探一次。
	//
	// 合成一列的话，「查不到」的上游会永远停在「从未探测过」，于是每一条
	// 命中它的请求都再问一次上游——而这类上游是多数。
	LastProbeAttemptAt *time.Time

	// LastProbeStatus 是最近一次探测的结论：ok / unsupported / error。
	//
	// 空字符串表示还没有探测过（自记账写出来的行就是这一档）。它不参与
	// 路由判据，只用来分清「查不到」与「探测失败」——两者都会更新
	// LastProbeAttemptAt，但处置不同：前者是常态，后者要在排障时看得见。
	LastProbeStatus string

	// UpdatedAt 是最近一次 Merge 的时刻，任何种类的观测都会更新它。
	UpdatedAt time.Time
}

// Observation 是一次观测的结果。
type Observation struct {
	Kind Kind

	// Remaining 是这次观测到的剩余量。nil 表示这次观测没有给出数值
	//（例如从错误码推断「余额不足」时，只知道它接近 0，不知道具体是多少）。
	Remaining *float64
	Unit      string

	// Exhausted 表示这次观测确认了额度已耗尽。
	//
	// 它与「Remaining 为 0」不同：后者是一次数值测量，
	// 而前者可以是定性的——上游返回「余额不足」时我们并不知道余额是多少，
	// 但知道它不足以完成这次请求。
	Exhausted bool

	// ErrorCode 是上游给的错误码，用于事后统计哪类错误最常见。
	ErrorCode string

	// ResetsAt 是上游给出的额度重置时刻。nil 表示这次观测没有给出这个信息，
	// 合并时保留上一状态的值——「没提」与「确认不会重置」是两件事，后者今天
	// 没有表达方式，所以空值只按「没提」处理。
	ResetsAt *time.Time

	// ObservedAt 是这次观测发生的时间。
	ObservedAt time.Time
}

// Confidence 是各类观测的可信度基线。
//
// 数值不是调出来的，是「按定义排个序」的结果：实测最可信，
// 从错误推断最不可信（它只说明「不够」，不说明「还有多少」）。
const (
	confidenceAPIQuery      = 1.0
	confidenceErrorObserved = 0.6
	// confidenceSelfAccountingBase 是自记账的**上限**，实际值按时长衰减。
	// 它永远达不到 1：自记账建立在「我们记全了每一次消耗」这个假设上，
	// 而任何一次漏记（进程崩溃、请求超时、上游多算）都会让它偏高。
	confidenceSelfAccountingBase = 0.8
)

// Merge 把一次新的观测合并进当前状态，返回新状态。
//
// 合并不是「用新值覆盖旧值」，而是按观测类型分情况——这是本包最需要
// 想清楚的一处，因为错误的合并策略会让额度推断在两种方向上出错：
//
//   - 太激进（无条件取最新）：一次 404 会把一个健康的渠道标成「未知」，
//     于是路由不再优先选它，而它其实完全可用。
//   - 太保守（只信实测）：一个查不到额度的渠道永远停在「未知」，
//     而自记账明明已经能给出可用的估计。
//
// 具体规则见下面每一步的注释。
//
// staleAfter 是实测值开始显得陈旧的时长，由调用方从探测间隔导出
// （见 SelfAccountingStaleAfter）。它跟着配置走而不是写死在这里：写死的话
// 改探测间隔对自记账的可信度毫无影响，而那条链路看上去是通的。
func Merge(prev State, obs Observation, now time.Time, staleAfter time.Duration) State {
	next := prev
	next.ChannelID = prev.ChannelID
	next.UpdatedAt = now

	if obs.Unit != "" {
		next.Unit = obs.Unit
	}

	switch obs.Kind {
	case KindAPIQuery:
		// 实测值无条件覆盖：它是我们能拿到的最好的信息。
		next.Remaining = obs.Remaining
		next.Confidence = confidenceAPIQuery
		next.Kind = obs.Kind
		next.Estimated = false
		next.LastProbeAt = &obs.ObservedAt
		if obs.ResetsAt != nil {
			// 这次实测带回了重置时刻。没带回时保留旧值：上游这次没提，
			// 不等于它确认了「不会重置」。
			next.ResetsAt = obs.ResetsAt
		}
		// 实测成功即清掉之前的错误信号：那个信号已经被更新的信息取代了。
		next.LastErrorCode = ""
		next.LastErrorAt = nil

	case KindSelfAccounting:
		// 自记账是一次**推进**，不是一次测量：它以「上一次已知的值」为基准，
		// 减去这段时间的消耗。所以它总是生效——即便上一次是刚拿到的实测值。
		//
		// 早先的写法是「新鲜的实测值不被覆盖」，那是错的：实测给出的是基准，
		// 而基准之上的消耗必须被扣掉，否则两次探测之间额度永远停在实测值上，
		// 路由会以为一个已经被用掉的渠道还有满额。
		//
		// 代价是它会累积误差（漏记一次消耗就偏高），所以可信度按时长衰减，
		// 且永远达不到实测的水平。
		next.Remaining = obs.Remaining
		next.Kind = obs.Kind
		next.Estimated = true
		next.Confidence = selfAccountingConfidence(prev.LastProbeAt, now, staleAfter)

	case KindErrorObserved:
		next.LastErrorCode = obs.ErrorCode
		next.LastErrorAt = &obs.ObservedAt
		next.Kind = obs.Kind

		if obs.Exhausted {
			// 上游明确说了不够用。这是定性但确定的信号——
			// 它比任何推算都更能说明「现在选它会失败」。
			zero := 0.0
			next.Remaining = &zero
			next.Confidence = confidenceErrorObserved
			next.Estimated = false
			return next
		}

		if obs.Remaining != nil {
			next.Remaining = obs.Remaining
			next.Confidence = confidenceErrorObserved
			next.Estimated = false
			return next
		}

		// 只是观察到一个错误码，没有数值信息。
		// **不动 Remaining**：一个限流错误不代表余额变了。
		// 这里最容易写错的是顺手把可信度也改了——那会让一个与额度无关的
		// 错误（例如 429 限流）把一个准确的实测值降级成不可信。
	}

	return next
}

// staleAfterFactor 是「实测值开始显得陈旧」相对于探测间隔的倍数。
//
// 取三倍：连着三个探测间隔都没拿到新的实测值，说明探测本身出了问题，
// 此时自记账的累积误差已经不小。
const staleAfterFactor = 3

// SelfAccountingStaleAfter 由探测间隔导出「实测值开始显得陈旧」的时长。
//
// 探测间隔是运行期配置（settings 的 quota.probe_interval_seconds），所以阈值
// 必须由它算出来而不是写死——写死的话，把探测间隔从 5 分钟改成 1 分钟，阈值
// 一动不动，而改配置的人以为自己改了。
//
// 今天只有这一份全局默认值，**按渠道覆盖还没有**：channels.setting 那一列能
// 存任意键值，但生产代码一个键都没读过，也没有按间隔发起探测的调度器。这句
// 写在这里是为了别把「本该如此」当成「已经是这样」——真要按渠道配不同的探测
// 间隔，先要有那个调度器。
//
// 非正数返回错误而不回落：阈值也非正时，任何一次自记账都会立刻被判成陈旧，
// 可信度从第一次扣减起就在衰减。那是配置写成 0 之后一个安静的行为改变，
// 而它在库里与「配置生效了」长得一样。
func SelfAccountingStaleAfter(probeInterval time.Duration) (time.Duration, error) {
	if probeInterval <= 0 {
		return 0, fmt.Errorf("额度探测间隔必须为正，得到 %v", probeInterval)
	}
	return staleAfterFactor * probeInterval, nil
}

// selfAccountingConfidence 给出自记账在当前情况下的可信度。
//
// 刚探测过时给基线（0.8），此后随时间衰减到一个下限。
// 下限不为零是有意的：一个很旧的估计仍然比「完全不知道」有信息量，
// 而 0 会被下游解释成「等同于未知」。
//
// staleAfter 由 SelfAccountingStaleAfter 从探测间隔导出，必须是正数——
// 它由装配层校验过一次（见 internal/assembly 读 quota.probe_interval_seconds
// 的那一处），本函数不再回落。
func selfAccountingConfidence(lastProbe *time.Time, now time.Time, staleAfter time.Duration) float64 {
	const floor = 0.2
	if lastProbe == nil {
		// 从未探测过：自记账是唯一的信息来源，给基线而不衰减——
		// 没有实测可以对照，衰减没有意义。
		return confidenceSelfAccountingBase
	}

	elapsed := now.Sub(*lastProbe)
	if elapsed <= staleAfter {
		return confidenceSelfAccountingBase
	}

	const fadeOver = 6 * time.Hour
	ratio := float64(elapsed-staleAfter) / float64(fadeOver)
	if ratio >= 1 {
		return floor
	}
	return confidenceSelfAccountingBase - (confidenceSelfAccountingBase-floor)*ratio
}

// Project 用本次消耗推进自记账。
//
// 它是自记账的核心：知道上次的余量，减去这次的消耗，得到新的估计。
// 余量未知时返回未知——凭消耗量推不出总量。
func Project(prev State, consumed float64, now time.Time) Observation {
	obs := Observation{
		Kind:       KindSelfAccounting,
		Unit:       prev.Unit,
		ObservedAt: now,
	}
	if prev.Remaining == nil {
		return obs
	}

	left := *prev.Remaining - consumed
	if left < 0 {
		// 消耗超过了记录的余量。这说明自记账有漏（例如在别处也用了这个账号），
		// 记成 0 而不是负数：负余量在路由与界面上都没有意义，
		// 而「已经见底」才是真实情况。
		left = 0
	}
	obs.Remaining = &left
	if left == 0 {
		obs.Exhausted = true
	}
	return obs
}

// Routable 判断一个额度状态是否足以支撑路由决策。
//
// 三种情况都算「够用」：
//
//   - 有实测值
//   - 有自记账的估算值
//   - 明确知道查不到（Remaining 为 nil 且从未观测过）
//
// 第三种之所以算够用：把它排除会让所有不支持查询额度的上游都不能被路由。
// 路由需要的是「知道还是不知道」，而不是「必须知道具体数字」。
func (s State) Routable() bool {
	if s.Remaining != nil {
		return true
	}
	// 从没观测过：视为未知但可用。
	return s.LastProbeAt == nil && s.LastErrorCode == ""
}

// Usable 判断额度是否足以承接一次请求。
//
// 与 Routable 分开：Routable 是「这个状态能不能用来做决策」，
// Usable 是「现在还能不能用这个渠道」。未知一律视为可用——
// 把未知当不可用等于默认停用所有查不到额度的渠道。
func (s State) Usable() bool {
	if s.Remaining == nil {
		return true
	}
	return *s.Remaining > 0
}

// Validate 检查一次观测是否自洽。
//
// 拦的是调用方的笔误：把「消耗」当成「剩余」传进来（那个值会是负的，
// 而负余量在这里被拒）、余量不是有限数、观测时间落在将来。
// 这些错不会让程序崩，只会让额度慢慢偏掉，而偏掉之后要很久才看得出来。
//
// **它不检查单位，这是刻意的。** 空单位是合法的——那表示「不知道按什么计」，
// 而 Merge 对空单位的处置是保留上一状态的单位（见 Unit 的注释）。
// 加一条「必须带单位」会与那条语义冲突，也会把自记账那条路堵死。
func Validate(obs Observation, now time.Time) error {
	if !obs.Kind.valid() {
		return fmt.Errorf("观测方式 %q 未知", obs.Kind)
	}
	if obs.Remaining != nil {
		if math.IsNaN(*obs.Remaining) || math.IsInf(*obs.Remaining, 0) {
			return fmt.Errorf("剩余量不是有限数: %v", *obs.Remaining)
		}
		if *obs.Remaining < 0 {
			return fmt.Errorf("剩余量为负: %v；余量见底应记为 0 并置 Exhausted", *obs.Remaining)
		}
	}
	if obs.Kind == KindAPIQuery && obs.Remaining == nil && !obs.Exhausted {
		// 一次「实测」既没给出数值也没说耗尽，那它等于什么都没观测到。
		// 放过去会让状态被标成「已实测」而实际毫无信息。
		return fmt.Errorf("实测观测必须给出剩余量或明确标记耗尽")
	}
	if obs.ObservedAt.After(now.Add(time.Minute)) {
		return fmt.Errorf("观测时间 %v 在将来", obs.ObservedAt)
	}
	return nil
}
