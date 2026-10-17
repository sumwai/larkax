// Package route 在多个上游渠道之间选择，并在失败时换下一个。
//
// 它做的事只有两件：**排序**与**重试**。协议转换在 internal/channel，
// 计费在 internal/billing。把选路单独放一层，是因为它的变化原因与那两者
// 都不同——换一家上游是加适配器，改计价是改规则，而改选路策略是改这里。
//
// 分层：
//
//	gateway ──► route ──► channel.Adapter ──► 上游
//	              │              │
//	              │              └── 只做协议转换
//	              └── 排序、重试、换渠道
//
// # 两条不可动摇的约束
//
//  1. **已经吐字给客户端的流不能换渠道。** 换了会让客户端收到两段拼接的
//     回答，而它无法察觉。边界是「适配器是否已经接受请求」——一旦拿到
//     事件流就不再换（见 stream.go 的 ChatStream）。至于失败发生在吐字
//     之前还是之后，由消费事件流的那一层判断——目前只有 gateway 一处
//     （见 gateway/stream.go 里 started 的注释）。
//  2. **失败的调用不计费。** 换渠道之后成功的那次才计费，前面失败的不计。
//     这意味着本包要把「哪些尝试已经花掉了费用」如实报出来——
//     上游可能在断开前已经产生了费用，只是我们没有拿到回执。
package route

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/quota"
)

// Candidate 是一个可选渠道连同它的实时状态。
//
// 状态与渠道本身分开传：channel.Channel 是「这个渠道是什么」（来自配置），
// 而这里的是「它现在怎么样」（来自健康度、配额、最近的表现）。
// 混在一起会让「配置改了」与「状态变了」在缓存里分不清。
type Candidate struct {
	Channel channel.Channel

	// OwnerUserID 是渠道所属的商家。0 表示平台自营。
	//
	// 它不参与打分，但结算需要它：商家级计价规则按这个字段匹配，
	// 而结算必须用与预留同一个 owner——否则两次调用可能匹配到不同的
	// 规则，算出一个与预留无缘由的差额。
	OwnerUserID int64

	// Priority 数值大者先选。商家用它表达「这个渠道是主力」。
	Priority int
	// Weight 是同优先级内的分流比例。<= 0 视为 1。
	Weight int

	// Health 是渠道健康度，由 channel_health_rollup 汇总而来。
	Health Health
	// Quota 是额度快照，直接取自 channel_quota_state 那一行。
	//
	// 类型是 quota.State 而不是更窄的形态：**判断**「这个额度状态能不能用」
	// 的判据在 internal/quota（Routable / Usable），而 Routable 要区分
	// 「从未观测过」与「观测过却没拿到数值」——那需要 LastProbeAt 与
	// LastErrorCode 两个字段。少带一个，判据就退化成「有没有数值」，
	// 而那是本包早先自己重写的那一遍（见 issue #204）。
	//
	// 读侧只填本包用得上的列（Remaining / Unit / Confidence / ResetsAt /
	// Kind / LastProbeAt / LastErrorCode）；窗口那几列不填，本包不看它们。
	//
	// 余量只在**同一单位**下与其他候选可比（见 scoreQuota）：Remaining 是
	// 裸数字，Unit 说明它按什么计，Confidence 说明这个数有多可信——本包按
	// 后者把分数往中性分拉，所以它必须由读侧填上，否则每个候选都退成
	// 「没有意见」，额度这一维等于关掉。
	//
	// Estimated **不在**上面那张单子里：本包不再看它。它想说的「这个数是
	// 推算的」，Confidence 已经说了（自记账的置信度上限就是 0.8，永远达不到
	// 实测的 1），两处一起看等于把同一件事扣两次。它的读者只剩插件契约
	// 那一处（见 internal/plugin 的 RouteContextFrom）。
	Quota quota.State

	// CostPerUnit 是最近观察到的单位成本，用于「成本」这一维度。
	// 为 0 表示未知——未知不该被当成「免费」。
	CostPerUnit float64
	// CacheHitRate 是近期缓存命中率，0..1。为 0 且 SampleCount 为 0 表示未知。
	CacheHitRate float64
	// SampleCount 是上述统计的样本数。为 0 时那些比率不可信。
	SampleCount int64
}

// Health 是渠道的健康度。
type Health struct {
	// ConsecutiveFailures 是连续失败次数。它在打分里权重最高——
	// 一个正在连续失败的渠道，无论多便宜都不该被选中。
	//
	// 它是一个**逐次维护的真计数**（成功清零、失败加一，见
	// internal/channels 的 RecordCall），不是从小时桶里估出来的：
	// 桶内有过成功时，那段连续长度落在 0 与 failures 之间，估不出来。
	//
	// 读取侧会把冷却期已过的计数当作 0（见 internal/channels 的
	// streakCooldown）：否则被摘掉的渠道没有流量，计数永远不会降回去。
	ConsecutiveFailures int
	// SuccessRate 是近期成功率，0..1。
	SuccessRate float64
	// LatencyP95 是近期响应时间分位，0 表示未知。
	LatencyP95 time.Duration
}

// Router 在候选集中选择渠道并执行请求。
type Router struct {
	// Weights 是打分的权重。零值用 DefaultWeights。
	Weights Weights
	// Rand 是可注入的随机源，便于测试得到确定结果。nil 用全局源。
	Rand *rand.Rand
	// Now 可注入，便于测试时间相关的逻辑。
	Now func() time.Time
}

// Weights 是各维度在总分里的权重。
//
// 做成可配置而不是写死：权重取决于这台机器要优化什么——
// 一个成本敏感的部署与一个延迟敏感的部署不该用同一组数字。
// 但**默认值必须存在且保守**，否则第一次部署的人无从下手。
type Weights struct {
	// Health 是最重的一项：一个正在失败的渠道不该因为便宜而被选中。
	Health float64
	// Quota 反映「还有多少可用」。见 scoreQuota 的注释。
	Quota float64
	// Cost 是单位成本，越小越好。
	Cost float64
	// Latency 是延迟，越小越好。
	Latency float64
	// Cache 是缓存命中率，越高越好。
	Cache float64
}

// DefaultWeights 是默认权重。
//
// 健康度占大头是有意的：其余维度都是「更好或更差」，而健康度是
// 「能用或不能用」。把它压到与其他维度同量级，会让一个正在连续失败的
// 便宜渠道仍然被选中。
var DefaultWeights = Weights{
	Health:  1.0,
	Quota:   0.6,
	Cost:    0.5,
	Latency: 0.3,
	Cache:   0.2,
}

// Eligible 判断一个候选是否**根本不可用**。
//
// 这是硬性条件，不是偏好：不满足就完全排除，而不是扣分。
// 判据必须能一眼看出对错——把这些也做成打分的话，
// 一个额度已经耗尽的渠道会以「分数略低」的形式留在候选集里，
// 而它一旦被选中就是一次注定失败的调用。
func (c Candidate) Eligible() (bool, string) {
	if c.Channel.ID == 0 {
		return false, "渠道缺少 ID"
	}
	if c.Channel.BaseURL == "" {
		return false, "渠道缺少 base_url"
	}
	if c.Channel.Credential == "" {
		return false, "渠道缺少凭证"
	}
	return true, ""
}

// DrainReason 返回「现在不该往这个渠道发请求」的原因，没有则返回空。
//
// 与 Eligible 分开：Eligible 是配置层面的、不会自己变的事实；
// 这里是运行状态。两者的处置也不同——配置错误要告警，
// 而状态不佳是正常现象，等一会儿自然会好。
//
// 额度那两道**判断**只有一处实现：internal/quota 的 Routable 与 Usable。
// 本包不自己读 Remaining 再判一遍——同一个语义两处实现，迟早分叉，
// 而分叉的表现是「读侧说能用、写侧说不能用」这类查不出原因的行为。
func (c Candidate) DrainReason() string {
	if c.Health.ConsecutiveFailures >= consecutiveFailureCutoff {
		return fmt.Sprintf("连续失败 %d 次", c.Health.ConsecutiveFailures)
	}
	// 状态不足以支撑决策：观测过，却既没有数值也没有别的可用信息。
	// 「从未观测过」（上游不支持查询额度）**不算**这一档——把它一起挡掉
	// 等于默认停用所有查不到额度的渠道（见 quota.State.Routable 的注释）。
	if !c.Quota.Routable() {
		return "额度观测过但没有可用信息"
	}
	// 有数值且已经见底：这是事实，不是偏好，必须挡住。
	// 未知（Remaining 为 nil）不挡——那说明这个渠道查不到额度，
	// 不是它没额度了。
	if !c.Quota.Usable() {
		return "额度已耗尽"
	}
	return ""
}

// consecutiveFailureCutoff 是连续失败多少次之后暂时不再选它。
//
// 取 3 而不是 1：上游偶发的单次抖动很常见，一次失败就摘掉渠道会让
// 候选集频繁跳变，表现为「流量在渠道之间无规律地漂移」。
// 也不取更大：连续三次失败已经说明它此刻确实不可用。
const consecutiveFailureCutoff = 3

// Plan 是一次请求的选路结果：按顺序排好的候选。
type Plan struct {
	// Candidates 是按尝试顺序排好的候选。
	Candidates []Candidate
	// Excluded 是被排除的候选及其原因，用于排查「为什么没选那个渠道」。
	// 不记录它的话，商家问「我配了三个渠道为什么只用一个」时无从回答。
	Excluded []Exclusion
}

// Exclusion 记录一个候选为什么没进 Plan。
type Exclusion struct {
	ChannelID int64
	Name      string
	Reason    string
}

// pinFirst 返回一份把 channelID 那个候选提到最前的副本。
//
// # 为什么是「提到最前」而不是「只留它」
//
// 钉住的只是一个**顺序**：它失败了照旧换下一个，剩下的候选一个不少。
// 只留它等于把「优先用这个」变成「只能用这个」，而那个决定不该由一个
// 只看到候选集、看不到上游结果的调用方来做。
//
// # 找不到时为什么原样返回而不是报错
//
// 传进来的 channelID 不在候选集里只有一种来源：调用方把**另一份** plan
// 交给了决策者。它不是一个能在这里判对错的错——本函数看不到决策者那一侧，
// 也就分不出「调用方传错了 plan」与「这个渠道本来就不该被选中」。
// 而「插件选了候选集之外的渠道」在决策者那一侧就已经被挡下并记成一次失败
// （见 internal/plugin 的 pickChannel），走不到这里。
//
// 所以这里选的是**不改变任何东西**：顺序照旧，请求照常发出去。反过来
// （报错、或把整个候选集清空）会让一次调用方的传参失误变成一次请求失败。
//
// # 不改动入参
//
// 返回的是新切片：plan 还可能被调用方拿着（它要把 Excluded 记进观测），
// 就地排序会静默改掉那边看到的东西。同 internal/plugin 的 sortedByChannelID。
//
// # 按 channelId 找，不按下标
//
// 决策者看到的那份候选集与这里的这份**顺序不同**：交给它之前按 channelId
// 重排过（见 internal/plugin 的 sortedByChannelID）。所以它只能给出一个
// channelId，下标在两边不可比——`pinChannelID` 是 ID 而不是位置，正是为此。
func (p Plan) pinFirst(channelID int64) Plan {
	if channelID == 0 {
		return p
	}
	idx := -1
	for i, c := range p.Candidates {
		if c.Channel.ID == channelID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		// -1：不在候选集里；0：本来就在最前。两种都不必动。
		return p
	}
	out := make([]Candidate, 0, len(p.Candidates))
	out = append(out, p.Candidates[idx])
	out = append(out, p.Candidates[:idx]...)
	out = append(out, p.Candidates[idx+1:]...)
	p.Candidates = out
	return p
}

// Select 按模型筛出候选并排序。
//
// 返回 Plan 而不是单个候选人：换渠道重试是调用方驱动的，它需要知道
// 下一个是谁。返回一个切片也让「为什么选了这个」可被完整解释。
func (r *Router) Select(candidates []Candidate, canonicalModel string) Plan {
	var (
		eligible []Candidate
		plan     = Plan{}
	)

	for _, c := range candidates {
		if ok, reason := c.Eligible(); !ok {
			plan.Excluded = append(plan.Excluded, Exclusion{c.Channel.ID, c.Channel.Name, reason})
			continue
		}
		if !c.Channel.Supports(canonicalModel) {
			plan.Excluded = append(plan.Excluded, Exclusion{c.Channel.ID, c.Channel.Name,
				fmt.Sprintf("不支持模型 %q", canonicalModel)})
			continue
		}
		if reason := c.DrainReason(); reason != "" {
			plan.Excluded = append(plan.Excluded, Exclusion{c.Channel.ID, c.Channel.Name, reason})
			continue
		}
		eligible = append(eligible, c)
	}

	plan.Candidates = r.order(eligible)
	return plan
}

// order 把候选排成尝试顺序。
//
// 排序规则：
//
//  1. priority 降序——这是商家的显式意图，不该被任何打分盖过。
//  2. 同 priority 内按分数降序。分数是各维度的加权和。
//  3. 同分之间按 weight 加权随机——**不是**严格顺序。
//
// 为什么第 3 步是加权随机而不是固定顺序：固定顺序会让所有流量压在
// 排序第一的那个渠道上，于是
//
//   - 一台坏渠道影响的是全部流量，而不是一部分；
//   - 其余渠道长期空转，健康数据陈旧，等主力渠道挂了才第一次被使用，
//     而那时才发现它也不行。
//
// 加权随机让每个渠道都在持续被使用。代价是同一条请求两次走不同渠道，
// 因此对上游缓存的命中率会下降——这一点在缓存亲和那一层另议。
func (r *Router) order(candidates []Candidate) []Candidate {
	if len(candidates) == 0 {
		return nil
	}
	type scored struct {
		c     Candidate
		score float64
		// tie 是组内随机排序用的键，避免了排序不稳定带来的偏差。
		tie float64
	}
	// 额度能不能比，是**整个候选集**的性质：两个余量只有在同一单位下才有
	// 大小之分。所以这个判断在循环外算一次，再交给每个候选打分。
	// 插件契约用的是同一个函数（见 MixedQuotaUnits）。
	mixedUnits := MixedQuotaUnits(candidates)

	grouped := make(map[int][]scored)
	for _, c := range candidates {
		grouped[c.Priority] = append(grouped[c.Priority], scored{
			c:     c,
			score: r.score(c, mixedUnits),
			tie:   r.float64(),
		})
	}

	priorities := make([]int, 0, len(grouped))
	for p := range grouped {
		priorities = append(priorities, p)
	}
	// priority 大者先选。
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	out := make([]Candidate, 0, len(candidates))
	for _, p := range priorities {
		group := grouped[p]
		// 同 priority 内：分数降序。同分之间用 tie 打散——
		// 本轮请求内不再考虑 weight，因为 weight 表达的是**长期比例**，
		// 而这里排的是「这一条请求的尝试顺序」。
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].score != group[j].score {
				return group[i].score > group[j].score
			}
			return group[i].tie < group[j].tie
		})
		for _, s := range group {
			out = append(out, s.c)
		}
	}
	return out
}

// Score 计算一个候选的分数，越大越先选。
//
// 每一项都归一到 0..1 再乘权重，而不是用原始量纲相加：
// 成本与延迟的量纲差着几个数量级，直接相加会让成本完全主导结果，
// 而那多半不是使用者的本意。
//
// 它按「所有候选的额度单位相同」来算：跨单位的判断需要看到整个候选集，
// 而这里只有一个候选。真正排序时走的是 score（由 order 传进那个判断），
// 所以排序结果与直接调本函数得到的分数**可能不同**——差别只在额度那一项，
// 而且只在候选集里存在多种单位时出现（见 scoreQuota）。
func (r *Router) Score(c Candidate) float64 {
	return r.score(c, false)
}

// score 是 Score 的内部形态：mixedUnits 由调用方给出。
func (r *Router) score(c Candidate, mixedUnits bool) float64 {
	w := r.Weights
	if w == (Weights{}) {
		w = DefaultWeights
	}
	return w.Health*scoreHealth(c.Health) +
		w.Quota*scoreQuota(c.Quota, mixedUnits) +
		w.Cost*scoreCost(c.CostPerUnit) +
		w.Latency*scoreLatency(c.Health.LatencyP95) +
		w.Cache*scoreCache(c.CacheHitRate, c.SampleCount)
}

// MixedQuotaUnits 判断候选集里是不是存在多种额度单位。
//
// 单位未知（空）也算一种：一个「不知道按什么计」的渠道与一个「按元计」的
// 渠道之间同样不可比，所以它与任何已知单位放在一起都算混合。
//
// 只统计**有数值**的候选：没有数值的渠道那一项本来就是中性分，
// 与单位无关，把它算进来会让「一个查不到额度的渠道」把其余渠道的额度
// 全部降成中性分——那等于让「不知道」污染「知道」。
//
// # 这是这条规则的唯一一处实现
//
// 「跨单位不可比」有两个消费者，它们必须看到同一件事：
//
//   - route 自己的打分：混合时 scoreQuota 给中性分（order 调用）
//   - 插件契约：混合时 plugin.RouteContextFrom 把 Remaining 全部置 null，
//     让插件看到「这里没有可用的信息」，而不是一组不可比的数
//
// 两处各写一遍的话，插件的行为会与内部打分悄悄分叉，而那时没有人能解释
// 「插件为什么这么选」——解释的人会拿「内核是怎么算的」去对，然后对不上。
// 所以它是导出的：插件侧调它，而不是照着抄一遍。
func MixedQuotaUnits(candidates []Candidate) bool {
	var first string
	seen := false
	for _, c := range candidates {
		if c.Quota.Remaining == nil {
			continue
		}
		if !seen {
			first, seen = c.Quota.Unit, true
			continue
		}
		if c.Quota.Unit != first {
			return true
		}
	}
	return false
}

// scoreHealth 把健康度映射到 0..1。
//
// 成功率的权重比连续失败低：成功率是历史，而连续失败是**当下**。
// 一个历史上很稳但现在连续失败的渠道，比一个一直不太稳的渠道更危险。
func scoreHealth(h Health) float64 {
	// 连续失败直接扣到很低，但不为零——为零会让它与「刚上线、无数据」
	// 无法区分，而后者应该得到中性分数。
	streak := 1.0 / (1.0 + float64(h.ConsecutiveFailures))
	base := h.SuccessRate
	if base <= 0 {
		// 没有样本时给中性值，而不是 0：新品没有历史不等于它不好。
		base = 0.5
	}
	return 0.4*base + 0.6*streak
}

// scoreQuota 把额度映射到 0..1。
//
// # 这里只回答「怎么排」
//
// 「这个额度状态能不能用」是**判断**，它只有一处实现：internal/quota 的
// Routable 与 Usable，由 DrainReason 调用。本函数回答的是另一半问题——
// 在都能用的候选里，谁的额度更该先用掉。两者不互相替代：判断是布尔的、
// 硬性的，排序是连续的、可权衡的。
//
// 两道兜底仍然留着，因为 Score 是导出 API，调用方可以绕过 DrainReason
// 直接调它。它们**不是**第二处判断，而是把同一个判断的结果映射成分数：
//
//   - 见底的候选给最低分（Usable 为假）。给 0 而不是负分：负分会让权重
//     之和的意义变得难解释。这一档与单位无关，所以排在下面那道跨单位的
//     判断之前——余量见底就是见底，与它按什么计无关。
//   - 没有数值的候选给中性分。走到这里的是「从未观测过额度」那一档
//     （上游不支持查询额度）；「观测过却没有数值」已经被 DrainReason
//     挡在候选集之外了。
//
// # 这里的数是什么单位
//
// Remaining 的单位由 State.Unit 说明，而候选渠道来自不同上游，
// 口径可能不同：按 token 计的套餐（百万量级）、按金额计的账户（个位到百位）、
// 按请求数计的配额（千量级）。把「剩余 800000 token」与「剩余 5 元」放进
// 同一个 1/(1+x) 里，前者永远得 0 分、后者永远接近满分——**分差来自单位，
// 而不是来自余量**，排序被一个与「还剩多少」无关的维度支配。
//
// 所以 mixedUnits 为 true（候选集里存在多种单位）时这一项给中性分，
// 与「没有数值」同一档：不比，而不是乱比。
//
// 为什么不归一化折算：那需要一个「1 元值多少 token」的假设，而那个假设
// 没有依据——把它写进代码等于把一个错换成另一个错，还多了一处没人能验证
// 的换算。**单位相同就可以比**，那时 1/(1+x) 的语义是明确的。
//
// # 可信度是权重，不是闸门
//
// 这个数有多可信由 State.Confidence 说明：实测是 1，自记账按距上次实测的
// 时长从 0.8 衰减到 0.2，从错误里推断是 0.6（见 internal/quota）。这里把它
// 当**权重**：算出来的分是「这次观测说出的分」与中性分 0.5 的凸组合，
// 可信度越低越靠近中性分。
//
// 为什么不按阈值排除：按间隔自动探测上游的调度器**还没有实现**，一次陈旧的
// 观测可能永远不会被刷新，排除等于让一个渠道因为一个没人会再看的旧数字
// 永久不可用。降权保留了「它仍然可能被选中」，只是不再因为一个陈旧数字被
// 优先或被排斥。
//
// 为什么退到 0.5 而不是别的档：0.5 就是上面那道兜底里「额度未知」的分。
// 可信度低意味着**没有意见**，与「不知道」是同一件事，不该为它另立一档。
//
// 两处刻意不参与加权：
//
//   - **见底**（Usable 为假）给 0。它是事实而不是偏好，与它有多可信无关；
//     而且这一档的处置在 DrainReason（判断）里，本函数只是把那个判断映射成
//     分数。让低可信度把「已经用完了」抬回中性，等于让一个旧数字重新放行
//     一个见底的渠道。
//   - **没有数值**与**跨单位**本来就是中性分，往中性分靠是它自己。
func scoreQuota(q quota.State, mixedUnits bool) float64 {
	if !q.Usable() {
		return 0
	}
	if q.Remaining == nil {
		return 0.5
	}
	if mixedUnits {
		return 0.5
	}

	// 「用掉它」的紧迫度：剩余越少越紧迫。用 1/(1+remaining) 之类的
	// 反比会让「剩余 0.5 与剩余 0.6」的差别大于「1000 与 2000」，
	// 这正是想要的——要到见底时才真的着急。
	urgency := 1.0 / (1.0 + *q.Remaining)

	// 到期越近越紧迫。没有到期时间时按中性处理。
	expiry := 0.5
	if q.ResetsAt != nil {
		remaining := time.Until(*q.ResetsAt)
		switch {
		case remaining <= 0:
			expiry = 1.0
		case remaining < time.Hour:
			expiry = 0.95
		case remaining < 24*time.Hour:
			expiry = 0.8
		case remaining < 7*24*time.Hour:
			expiry = 0.6
		default:
			expiry = 0.4
		}
	}

	// 这次观测说出的分：紧迫度与到期各占一半。
	raw := 0.5*urgency + 0.5*expiry

	// 再按可信度把它往中性分拉。写成凸组合而不是 0.5 + c*(raw-0.5)，
	// 是为了让两个端点**精确**落在 raw 与 0.5 上：后者在 raw 很小时
	// （例如余量 1000、十天后再到期，raw 约 0.2）会因两次舍入差最后一个
	// ulp，而「实测值的行为一字不变」是这条判据的硬要求。
	const neutral = 0.5
	c := confidenceWeight(q.Confidence)
	return raw*c + neutral*(1-c)
}

// confidenceWeight 把 State.Confidence 收进 0..1。
//
// 越界的值只可能来自本包之外（库里的列有 CHECK 0..1，quota.Merge 写的是
// 三个常量）。仍然要收：大于 1 会让一个候选拿到比实测还高的分，负值会把
// 「分数向中性分靠拢」反过来——余量多的反而更该先用。
func confidenceWeight(c float64) float64 {
	switch {
	case c <= 0:
		// 零值也走这里。State 的零值 Confidence 是 0，于是「没填可信度」
		// 与「可信度为 0」是同一个结果：退成中性分。这是有意的——一个
		// 不知道有多可信的数，不该比一个已知不可信的数更受优待。
		return 0
	case c >= 1:
		return 1
	default:
		return c
	}
}

// scoreCost 把单位成本映射到 0..1，越小越好。
//
// 未知成本给中性分而不是满分：把「不知道多少钱」当成「免费」，
// 会让所有未知成本的渠道排在已知便宜的前面，而那正是最容易超支的路径。
func scoreCost(cost float64) float64 {
	if cost <= 0 {
		return 0.5
	}
	// 1/(1+cost) 在有量纲的成本上不理想，但它的好处是**不需要知道全局范围**：
	// 归一化需要所有候选的成本，而那要求先收集全部候选才能算一个人的分。
	// 现在这样可以让每个候选独立打分，也便于测试。
	return 1.0 / (1.0 + cost)
}

// scoreLatency 把延迟映射到 0..1，越小越好。
func scoreLatency(p95 time.Duration) float64 {
	if p95 <= 0 {
		return 0.5
	}
	// 以秒为单位代入反比函数。1 秒以内接近满分，
	// 10 秒时降到约 0.09——足够把慢渠道压下去，但不会压成 0。
	return 1.0 / (1.0 + p95.Seconds())
}

// scoreCache 把缓存命中率映射到 0..1。
//
// 样本太少时给中性分：一两次请求的命中率不说明任何事，
// 用它去影响路由会让流量在两个渠道之间无意义地摇摆。
func scoreCache(hitRate float64, sampleCount int64) float64 {
	const minSamples = 20
	if sampleCount < minSamples {
		return 0.5
	}
	if hitRate < 0 {
		return 0
	}
	if hitRate > 1 {
		return 1
	}
	return hitRate
}

func (r *Router) float64() float64 {
	if r.Rand != nil {
		return r.Rand.Float64()
	}
	return rand.Float64()
}

// ── 执行 ────────────────────────────────────────────────────────────────

// Attempt 是一次尝试的记录。
type Attempt struct {
	ChannelID   int64
	ChannelName string
	Err         error
	// Retryable 是这次失败是否值得换下一个渠道。
	Retryable bool
	// StartedAt 用于统计与排查。
	StartedAt time.Time
	Duration  time.Duration
}

// Result 是一次 Chat 调用的结果。
type Result struct {
	Response *channel.Response
	// Channel 是最终成功（或最后失败）的那个渠道。
	Channel channel.Channel
	// Attempts 按顺序记录了每一次尝试，包括失败的。
	// 计费与排查都需要它：失败的调用不计费，但要知道发生过。
	Attempts []Attempt
}

// Errors 是全部候选都失败时返回的错误。
type Errors struct {
	Attempts []Attempt
}

func (e *Errors) Error() string {
	if len(e.Attempts) == 0 {
		return "没有可用的渠道"
	}
	return fmt.Sprintf("全部 %d 个候选渠道都失败，最后一个错误: %v",
		len(e.Attempts), e.Attempts[len(e.Attempts)-1].Err)
}

// Unwrap 返回最后一个错误，让 errors.Is/As 能对上游错误类型生效。
func (e *Errors) Unwrap() error {
	if len(e.Attempts) == 0 {
		return nil
	}
	return e.Attempts[len(e.Attempts)-1].Err
}

// ErrNoCandidates 表示没有可用候选。
var ErrNoCandidates = errors.New("没有可用的渠道")

// Executor 是真正执行请求的东西。抽成接口是为了让路由逻辑能脱离 HTTP 测试，
// 也为了让「重试到第几个为止」这类规则可以被单测覆盖。
type Executor interface {
	Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error)
}

// worthNextCandidate 判断一次失败是否值得换下一个候选渠道。
//
// 它比 channel.IsRetryable 多认一种错误：ErrUnknownType。那不是「上游坏了」，
// 而是「这一条候选的配置当前版本的二进制不认识」——它属于**这条候选**，
// 不属于**这次请求**。按不可重试处理会让它挡住后面的候选：库里只要有一条
// 回滚后残留的渠道行排在前面，整条请求就失败，而其余候选都是好的。
//
// 判据放在这里而不是适配器或执行器里：执行器只看得到自己那一次调用，
// 只有路由层知道后面还有谁，也就只有它能决定「值不值得换下一个」。
func worthNextCandidate(err error) bool {
	return channel.IsRetryable(err) || errors.Is(err, channel.ErrUnknownType)
}

// Chat 按选路结果依次尝试，直到成功或用尽候选。
//
// 重试的边界由 worthNextCandidate 决定：429、5xx、网络层失败换下一个；
// 400、401 这类换渠道也是同一个错，重试只会把费用与延迟乘以候选数。
// 唯一的例外是 ErrUnknownType——见 worthNextCandidate。
//
// 它是「筛 + 排 + 执行」一步到位的形态。要在这两步之间插一件事（今天只有
// 插件）的调用方走 ChatPlan。
func (r *Router) Chat(ctx context.Context, exec Executor, req *channel.Request, candidates []Candidate, canonicalModel string) (*Result, error) {
	return r.ChatPlan(ctx, exec, req, r.Select(candidates, canonicalModel), 0)
}

// ChatPlan 按一份**已经算好的**选路结果执行，并把 pinChannelID 钉在第一位。
//
// # 它为什么存在
//
// 选路要分两步做：先由 Select 筛出候选，再把「筛过的候选」交给一个外部
// 决策者，最后才执行它给出的顺序。中间那一步**必须在包外发生**——
// internal/plugin 已经 import 本包（见它的 adapter.go），本包反向 import
// 它就成环。
//
// 没有这个入口时，包外的调用方只有两条错路：自己重写一遍 Eligible /
// DrainReason 的筛选（那是第二份实现，两份迟早不一致），或者把筛选结果
// 丢掉、重新走一次 Chat（那样决策者看到的候选与真正执行的不是同一个东西，
// 而两者分叉的表现是「它选了一个渠道，请求却发去了另一个」）。
//
// # pinChannelID 的语义
//
// 0 表示不钉，顺序与 Select 排出来的完全一致；非 0 时把那个候选提到最前，
// 其余候选保持原顺序跟在后面（它失败了照旧换下一个，见 pinFirst）。
//
// **调用方应当把同一个 plan 先交给决策者、再拿回来执行**：那样「决策者看到
// 的候选」与「被执行的候选」是同一份，这一点由调用约定保证，本方法不重复
// 校验——决策者那一侧已经挡下了候选集之外的决策（见 internal/plugin 的
// pickChannel），而本方法看不到那一侧的上下文。
func (r *Router) ChatPlan(ctx context.Context, exec Executor, req *channel.Request, plan Plan, pinChannelID int64) (*Result, error) {
	return r.chatWithPlan(ctx, exec, req, plan.pinFirst(pinChannelID))
}

func (r *Router) chatWithPlan(ctx context.Context, exec Executor, req *channel.Request, plan Plan) (*Result, error) {
	if len(plan.Candidates) == 0 {
		return nil, &Errors{}
	}

	result := &Result{}
	for _, c := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			// 客户端断开：不再尝试剩余渠道。已经发生的尝试照常记录，
			// 因为其中可能有已经产生费用的。
			return nil, err
		}

		started := r.now()
		resp, err := exec.Chat(ctx, c.Channel, req)
		result.Attempts = append(result.Attempts, Attempt{
			ChannelID:   c.Channel.ID,
			ChannelName: c.Channel.Name,
			Err:         err,
			Retryable:   worthNextCandidate(err),
			StartedAt:   started,
			Duration:    r.now().Sub(started),
		})
		result.Channel = c.Channel

		if err == nil {
			result.Response = resp
			return result, nil
		}
		if !worthNextCandidate(err) {
			// 不可重试：换渠道也是同一个错，立刻返回。
			// 已经记录的尝试保留下来，调用方需要知道试过谁。
			return result, &Errors{Attempts: result.Attempts}
		}
	}
	return result, &Errors{Attempts: result.Attempts}
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
