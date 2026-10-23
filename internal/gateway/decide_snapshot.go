package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/sumwai/larkax/internal/route"
)

// DecisionRedactor 脱敏一次决策请求的判据足迹，两个方向各一个方法。
//
// # 为什么与 Redactor 分开
//
// Redactor 收的是 *channel.Request / *channel.Response，而决策这条表面不共用
// 那两个类型（见 internal/systemone 的包注释）。把两个方法加到 Redactor 上，
// 等于要求每一个实现都同时能处理两套形状——包括那些只服务 chat 的替身，
// 而它们多出来的两个方法只会返回空或 panic。
//
// 与 Snapshots 同理：留痕是排障辅助，请求与计费链路上不能因此多出一个必填
// 依赖。字段为 nil 时这条路径完全不采集，请求照走（见 decisionSnapshotAsync）。
type DecisionRedactor interface {
	// RedactDecisionRequest 脱敏一次决策请求**发出之前**的判据。
	//
	// 它记的是「我们打算怎么处理」：候选集合里有哪些渠道、各自什么状态。
	RedactDecisionRequest(t *DecisionTrace) (*Snapshot, error)
	// RedactDecisionResponse 脱敏一次决策请求的**结局**。
	//
	// 它记的是「实际选了什么、为什么」：选中的渠道与上游名、每一次尝试的
	// 结果、候选集合本身（哪几个没被选中），以及被选路筛掉的那几个与原因。
	RedactDecisionResponse(o *DecisionOutcome) (*Snapshot, error)
}

// DecisionTrace 是决策请求**发出之前**的判据。
//
// # 它为什么不是那份 systemone.Request
//
// 那条请求里的 state 是用户原文、questions 是要模型回答的问题，两者都是
// 内容。把整份请求交给脱敏层，就是把「它们会不会被脱敏」交给一张允许表的
// 完备性；而这条链路口径更强：**内容根本不进这条链路**。
// 少了它们，交给脱敏层的就只有判据，脱敏器连看一眼原文的机会都没有。
//
// 新增字段时的判据因此是「它是判据还是内容」，而不是「它该配哪一档脱敏」——
// 后者的默认是写进表里就会落库，前者默认不写。
type DecisionTrace struct {
	// Model 是**入站解析之后的规范名**（入口那一侧的 CanonicalModel），
	// 不是选路之后翻出来的上游名，也不是调用方写的原名。
	Model string
	// Candidates 是预留之前取到的候选集合，与 begin 交给选路的是同一份。
	//
	// 它必须是**这一份**：选路之后能拿到的是「试过哪些」，而「当时一共配了
	// 几个候选」只有这一份答得出来——被筛掉的是哪些、为什么，记在结果那一侧
	// （见 DecisionOutcome.Excluded），两边的计数对照才说得出「几个里筛掉了
	// 几个」。
	Candidates []DecisionCandidate
}

// DecisionOutcome 是一次决策请求的结局。
//
// 四个部分缺一不可：选中的是谁、试过谁、候选集合本身，以及**被筛掉的那几个
// 与为什么**。少了候选集合，「为什么不是另外那个渠道」就要去 join 请求方向
// 那一行才答得出来——而排障时最容易被漏掉的一步正是「还有第二行」；少了排除
// 记录，**那两个没发出去的候选为什么不在候选里**就答不出来——从 Candidates 与
// Attempts 的差额只看得出「有两/三个没被用」，看不出它们是选路筛掉的还是
// 压根没轮到（见 DecisionOutcome.Excluded）。
type DecisionOutcome struct {
	// Model 与请求方向是同一个规范模型名。
	//
	// 响应方向也带它，因为骨架被截断降级成摘要层时留下的只有顶层标量
	// （见 snapshot 的 topScalars）——一条分不出「这是哪个模型的哪次决策」
	// 的摘要行等于没有。
	Model string
	// Candidates 与请求方向是同一份候选集合（见 DecisionTrace.Candidates）。
	Candidates []DecisionCandidate

	// Succeeded 表示这次决策请求拿到了上游的响应。
	//
	// 它与「SelectedChannelID 是不是零」分开：零值确实能表达「没选中」，
	// 但读代码的人得先知道渠道 id 不会是 0。更要紧的是全部失败时
	// route.Result.Channel 记的是**最后试过**的那个（见那里的注释），
	// 照抄进骨架会让一次全军覆没读成一次成功选路。
	Succeeded bool
	// SelectedChannelID 与 UpstreamModel 是成功时选中的渠道与**实际发出去**的
	// 上游名。失败时两者不进骨架。
	SelectedChannelID int64
	UpstreamModel     string

	// Attempts 按顺序记下每一次尝试，含最后成功的那次（见 route.Result.Attempts）。
	Attempts []DecisionAttempt

	// Excluded 是被选路筛掉的候选与原因（见 route.Result.Excluded）。
	//
	// 它补上的是候选集合与尝试列表之间的那一段：Candidates 记的是**选路之前**
	// 有几个候选，Attempts 记的是**真的发出去过**哪几个，而前者里没被发出去的
	// 那些**为什么**不在候选里，从前没有任何一处说得出来。
	//
	// 它不是一份穷尽的划分：差额里还可能有「没轮到就被提前放弃」的候选——
	// 别名翻不出来、客户端断开、不可重试的失败后仍有未试候选（见
	// route.Chat 的三条提前返回）。那几支不在 Excluded 里，Attempts 也照不到
	// 它们（除了 `retryable=false` 那一支）。
	Excluded []DecisionExclusion

	// UpstreamStatus 与 ErrorKind 是整次请求以失败告终时的上游证据，
	// 与响应快照的那两列同源（同样出自 upstreamEvidence）。
	//
	// **成功时两者是 nil**，这与 chat 那条路不同：这条表面回给我们的响应里
	// 没有 HTTP 状态码（internal/systemone.Response 不带那个字段），
	// 所以一个成功的决策请求在库里那两列是 NULL——而 NULL 的既有含义是
	// 「一个 HTTP 响应都没拿到」。这是一条**已知的偏差**：逐次尝试的
	// 状态码记在 attempts[].upstream_status 里，见
	// docs/request-retention.md。
	UpstreamStatus *int
	ErrorKind      *string
}

// DecisionCandidate 是一个候选渠道的**判据**，只有判据。
//
// # 它为什么不是 route.Candidate
//
// 那个类型里带着 channel.Channel——里面有凭证密文与 BaseURL。整份传下去的
// 后果是「快照这一层能不能看到凭证」取决于适配层有没有读它，而那是一条只靠
// 约定维持的边界：加一个字段、抄一行赋值，凭证就进了某条骨架，且不报错。
//
// 这里把用得上的一格一格列出来，于是「凭证与上游地址进不了骨架」在编译期
// 就成立——它们根本没有那条路。改动时要照着同一个判据问一句：
// 这一格是**判据**还是**内容**。
type DecisionCandidate struct {
	// ChannelID 是候选身份，也是与账单对上的那个数。
	ChannelID int64
	// Priority 数值大者先试。Weight 不参与排序——它记在这一段里，是为了
	// 让「当时渠道上配的是什么」可查（见 route.Candidate.Weight）。
	Priority int
	Weight   int

	// 下面四格是健康度，路由打分里权重最高的一维是连续失败次数。
	ConsecutiveFailures int
	SuccessRate         float64
	// LatencyP95 为 nil 表示**没有数据**，不是「恰好 0」。
	LatencyP95 *time.Duration

	// 下面四格是额度。余量只在单位相同时与其他候选可比，所以 Unit 必须
	// 与 Remaining 成对读；Confidence 说明这个数有多可信。
	QuotaUnit       string
	QuotaRemaining  *float64
	QuotaConfidence float64
	QuotaKind       string

	// CostPerUnit 与 CacheHitRate 为 nil 表示没有数据，与「是 0」分开。
	CostPerUnit  *float64
	CacheHitRate *float64
	// SampleCount 是上面两条统计的样本数。
	SampleCount int64
}

// decisionCandidatesOf 从路由的候选集里取出判据。
//
// 这是 route.Candidate → DecisionCandidate 的唯一转换点，字段逐个写出来而不是
// 整份搬：见 DecisionCandidate 的注释，那条编译期的保证就建立在这一行一行。
func decisionCandidatesOf(cands []route.Candidate) []DecisionCandidate {
	out := make([]DecisionCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, DecisionCandidate{
			ChannelID:           c.Channel.ID,
			Priority:            c.Priority,
			Weight:              c.Weight,
			ConsecutiveFailures: c.Health.ConsecutiveFailures,
			SuccessRate:         c.Health.SuccessRate,
			LatencyP95:          c.Health.LatencyP95,
			QuotaUnit:           c.Quota.Unit,
			QuotaRemaining:      c.Quota.Remaining,
			QuotaConfidence:     c.Quota.Confidence,
			QuotaKind:           string(c.Quota.Kind),
			CostPerUnit:         c.CostPerUnit,
			CacheHitRate:        c.CacheHitRate,
			SampleCount:         c.SampleCount,
		})
	}
	return out
}

// DecisionAttempt 是一次尝试的判据足迹。
//
// 状态码与分类在**这里**算好，而不是交给脱敏那一侧现推：判据是
// upstreamEvidence，它与账单上那两列是同一个来源。两处各推一遍的话，
// 「为什么降级到下一个渠道」在快照与账单上会给出两个说法。
type DecisionAttempt struct {
	ChannelID int64
	// Succeeded 表示这一次拿到了响应。
	Succeeded bool
	// Retryable 只在失败时有意义：这一次的失败是否值得换下一个候选。
	// false 表示当场放弃，后面的候选连试都没试——这是降级原因的第一层。
	Retryable bool
	// UpstreamStatus 与 ErrorKind 同上，没有时是 nil。
	UpstreamStatus *int
	ErrorKind      *string
	// Duration 是这一次的耗时。负值归零：它由两次 now() 相减而来，
	// 注入的时钟回拨会给出负数，而负数经「非负整数」那一档会变成脱敏描述符
	// ——一条时钟抖动换来的噪音。
	Duration time.Duration
}

// DecisionExclusion 是一条被选路筛掉的候选的**判据**。
//
// # 它为什么不是 DecisionCandidate
//
// 被筛掉的候选没有「现在是什么状态」可言：它连打分都没进，Priority /
// 健康度 / 额度那一组输入对它一件都没用上。把两者合成一个类型，就得
// 让每一段候选回答「我是被筛掉的还是被选的」，而那个布尔答错了的表现是一段
// 看不出问题的现场。
//
// # 字段为什么只有两格
//
// ChannelID 是身份，Code 是那句枚举词（见 route.Exclusion.Code）。**渠道名
// 不上这条路**：它与 DecisionCandidate 同一条判据——那个类型里也没有名字，
// 于是「凭证与商家可读的名字进不了骨架」在编译期就成立。
//
// 它的名字跟着 route.Exclusion.Code 走，而**不叫** Reason：那两个词在
// route.Exclusion 上指的是相反的东西（Reason 是给人看的那句话，Code 是分类），
// 同一个类型里叫反了，读的人会把留痕里的那一格当成原文。
type DecisionExclusion struct {
	ChannelID int64
	// Code 是机器可读的排除分类，取值见 route.ExclusionCode。
	//
	// 不是那句给人看的话：那句话里带着模型名与数值（见 route.Exclusion.Reason），
	// 是一段自由文本，落进留痕只会被当成内容脱敏成一个占位符。
	Code string
}

// decisionExclusionsOf 把路由的排除记录摊成判据足迹。
//
// 与 decisionCandidatesOf 同一个做法、同一个理由：字段逐个写出来，于是
// 「哪些字段可能进骨架」在这一行一行上，而不是散在几个调用点。
func decisionExclusionsOf(excluded []route.Exclusion) []DecisionExclusion {
	out := make([]DecisionExclusion, 0, len(excluded))
	for _, e := range excluded {
		out = append(out, DecisionExclusion{
			ChannelID: e.ChannelID,
			Code:      string(e.Code),
		})
	}
	return out
}

// excludedOf 取出这次选路筛掉了哪些候选。
//
// # 两个来源
//
// 有结果时读结果上的那一份。**一个候选都没剩下时没有结果**：选路在这一步
// 就返回了错误，而那份 Plan 不进 Result——所以被筛掉的那些只在 route.Errors
// 上（见 route.Errors.Excluded）。少了这一支，「三个候选一个都没用上」这条
// 最该被看见的记录恰好是唯一看不见的。
//
// err 为 nil 而结果也为 nil 的组合不会出现（选路要么给结果要么给错误），
// 这里不为此多写一个分支：真出现时返回空清单，留痕照落，与「没有排除」
// 逐列相同——那是这条链路可以接受的降级。
func excludedOf(r *route.Result, err error) []route.Exclusion {
	if r != nil {
		return r.Excluded
	}
	var re *route.Errors
	if errors.As(err, &re) {
		return re.Excluded
	}
	return nil
}

// decisionAttemptsOf 把路由的尝试列表摊成判据足迹。
//
// # Err 本身不带出去
//
// 错误文本是上游或我们自己的自由文本，可能回显用户输入。带出去的是它的
// 分类与状态码，原文一个字都不留——这一层就把它丢掉，比指望后面某一档
// 脱敏规则挡住它可靠。
func decisionAttemptsOf(attempts []route.Attempt) []DecisionAttempt {
	out := make([]DecisionAttempt, 0, len(attempts))
	for _, a := range attempts {
		item := DecisionAttempt{
			ChannelID: a.ChannelID,
			Succeeded: a.Err == nil,
			Duration:  a.Duration,
		}
		if item.Duration < 0 {
			item.Duration = 0
		}
		if a.Err != nil {
			item.Retryable = a.Retryable
			status, kind := upstreamEvidence(a.Err)
			item.UpstreamStatus = optionalInt(status)
			item.ErrorKind = optionalString(kind)
		}
		out = append(out, item)
	}
	return out
}

// decisionOutcomeOf 把一次决策选路的结局摊成响应方向的判据。
//
// 与 attemptsOf / placementOf 同一个做法：把「从结果里读哪几个字段」收在一处，
// 免得每个调用点各读一遍，而其中一处读错时的表现是一个静默的空字段。
func decisionOutcomeOf(r *route.Result, err error, candidates []route.Candidate, model string) *DecisionOutcome {
	out := &DecisionOutcome{
		Model:      model,
		Candidates: decisionCandidatesOf(candidates),
		Attempts:   decisionAttemptsOf(attemptsOf(r)),
		Excluded:   decisionExclusionsOf(excludedOf(r, err)),
	}
	if err == nil && r != nil {
		out.Succeeded = true
		out.SelectedChannelID = r.Channel.ID
		out.UpstreamModel = r.UpstreamModel
		return out
	}
	if err != nil {
		status, kind := upstreamEvidence(err)
		out.UpstreamStatus = optionalInt(status)
		out.ErrorKind = optionalString(kind)
	}
	return out
}

// decisionSnapshotAsync 采集一条决策请求方向的快照。
//
// # 位置与 chat 那条路相同
//
// 采在选路**之前**：那时还不知道会打到哪儿，而请求方向要回答的正是
// 「当时手上有什么」。与 snapshotAsync 的分工一样——采集同步、落库异步。
func (g *Gateway) decisionSnapshotAsync(ctx context.Context, t *DecisionTrace, userID int64) {
	if g.DecisionRedactor == nil || g.Snapshots == nil || t == nil {
		return
	}

	snap, err := g.DecisionRedactor.RedactDecisionRequest(t)
	if err != nil {
		// 与 chat 那条路同一个处置：脱敏失败就丢快照，绝不回落成存原文。
		return
	}
	snap.RequestID = requestIDOf(ctx)
	snap.UserID = userID
	// 方向是 request：这条表面与 chat 共用那两个取值，它是哪条表面由骨架里
	// 的 surface 标量回答（见 internal/snapshotwire 的适配层）。
	snap.Direction = "request"

	g.saveSnapshotAsync(ctx, snap)
}

// decisionSnapshotResponse 采集一条决策响应方向的快照。
//
// # 成功失败都采
//
// 失败那一条尤其重要：全部候选都失败时，「换渠道重试降级到了哪一步、
// 每一步为什么不成」只有这一条记着。与 chat 那条路同一个取舍（见
// responseSnapshotAsync）。
func (g *Gateway) decisionSnapshotResponse(ctx context.Context, o *DecisionOutcome, userID int64) {
	if g.DecisionRedactor == nil || g.Snapshots == nil || o == nil {
		return
	}

	snap, err := g.DecisionRedactor.RedactDecisionResponse(o)
	if err != nil {
		return
	}
	snap.RequestID = requestIDOf(ctx)
	snap.UserID = userID
	snap.Direction = "response"
	// 两列照实写：失败时它们是上游证据，成功时是 nil（见 DecisionOutcome
	// 上那条已知偏差）。
	snap.UpstreamStatus = o.UpstreamStatus
	snap.ErrorKind = o.ErrorKind

	g.saveSnapshotAsync(ctx, snap)
}
