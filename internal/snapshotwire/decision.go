package snapshotwire

import (
	"errors"

	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/snapshot"
)

// surfaceDecision 是决策那条表面写在骨架里的标记。
//
// # 为什么是一个骨架里的键，而不是一个新方向
//
// request_snapshots.direction 上是 `CHECK (direction IN ('request','response'))`，
// 而决策这条表面同样有请求与响应两个方向。加一个取值就要一条迁移，而这条
// 链路是排障辅助、可以整条丢——不值得为它动 schema（见 internal/snapshot 的
// Surface 注释）。
//
// 它落在**顶层标量**这一格：骨架超限降级成摘要层时（见 snapshot 的
// topScalars），留下来的正是顶层标量。分辨不出是哪条表面的摘要行等于没有。
const surfaceDecision = string(snapshot.SurfaceDecision)

// RedactDecisionRequest 脱敏一次决策请求的判据足迹。
//
// # 适配层为什么在本包
//
// 脱敏器吃的是 map[string]any（见 snapshot.RedactRaw），而网关手上是
// DecisionTrace / DecisionOutcome。这一层把前者摊成后者要的形状。
// 它放在本包而不是 gateway：gateway 的职责是编排请求，它多知道一处
// 「骨架长什么样」就多一处要跟着允许表改的地方。本包是唯一同时看见两侧的
// 地方，摊平是它该做的事。
func (c *Collector) RedactDecisionRequest(t *gateway.DecisionTrace) (*gateway.Snapshot, error) {
	if t == nil {
		return nil, errors.New("决策足迹为空")
	}
	res, err := c.decisionRequestRedactor.RedactRaw(decisionRequestMap(t))
	return toSnapshot(res, err)
}

// RedactDecisionResponse 脱敏一次决策请求的结局。
func (c *Collector) RedactDecisionResponse(o *gateway.DecisionOutcome) (*gateway.Snapshot, error) {
	if o == nil {
		return nil, errors.New("决策结局为空")
	}
	res, err := c.decisionResponseRedactor.RedactRaw(decisionResponseMap(o))
	return toSnapshot(res, err)
}

// decisionRequestMap 摊出请求方向的判据。
//
// # 这里决定了「哪些字段可能落库」
//
// 允许表是第二道闸，这里是第一道，而且是更强的那一道：**没有摊进这个 map
// 的东西，脱敏器连看一眼的机会都没有。** 这条表面上的内容（state、questions、
// answers）因此不是「进了脱敏器又被替换掉」，是压根没到过。
// 新增字段时的判据是「它是判据还是内容」，而不是「给它配哪一档脱敏」。
func decisionRequestMap(t *gateway.DecisionTrace) map[string]any {
	m := map[string]any{
		"surface":          surfaceDecision,
		"candidates_count": len(t.Candidates),
		"candidates":       decisionCandidatesOf(t.Candidates, 0, false),
	}
	putEnumWord(m, "model", t.Model)
	return m
}

// decisionResponseMap 摊出响应方向的判据。
//
// 它比请求方向多两部分：selected（只有成功时才有）与每段候选上的 selected
// 布尔。两者一起才回答得了「为什么是它」——只看选中项看不出还有谁在争。
//
// 被排除的候选是第三部分（见 decisionExclusionsOf），它只在这一条上。
func decisionResponseMap(o *gateway.DecisionOutcome) map[string]any {
	m := map[string]any{
		"surface":          surfaceDecision,
		"candidates_count": len(o.Candidates),
		"candidates":       decisionCandidatesOf(o.Candidates, o.SelectedChannelID, true),
		"attempts":         decisionAttemptsOf(o.Attempts),
	}
	putEnumWord(m, "model", o.Model)
	// 一个候选都没被筛掉时**这两个键都不写**：那时这一行与加这条留痕之前
	// 逐字相同，而「多一个空数组」也算变——排障的人是拿着两份快照对比的，
	// 凭空多出来的 `"excluded": []` 会让人以为「这次有过排除，只是没记下」。
	//
	// excluded_count 与 excluded 一起写、一起不写，而不是像 candidates_count
	// 那样常驻：candidates_count 说的是「当时有几个候选」，那是一个永远成立
	// 的事实；零个排除不是一个事实，它是「没有这件事」。它的用途在另一个
	// 场合——骨架超限降级成摘要层时留下的只有顶层标量（见 snapshot 的
	// topScalars），那时 excluded 整段会消失，而「3 个候选里有 1 个被筛掉」
	// 这件事必须还看得见。
	if n := len(o.Excluded); n > 0 {
		m["excluded_count"] = n
		m["excluded"] = decisionExclusionsOf(o.Excluded)
	}
	if o.Succeeded {
		if selected := decisionSelectedOf(o); selected != nil {
			m["selected"] = selected
		}
	}
	return m
}

// decisionSelectedOf 摊出选中的那一个，没有可用取值时返回 nil。
//
// 两个字段都空时整段不写，而不是写一个空对象：空对象读起来像「选了一个
// 没有名字的渠道」，而实际情况是我们没拿到它们。
func decisionSelectedOf(o *gateway.DecisionOutcome) map[string]any {
	if o.SelectedChannelID == 0 && o.UpstreamModel == "" {
		return nil
	}
	out := make(map[string]any, 2)
	if o.SelectedChannelID != 0 {
		out["channel_id"] = o.SelectedChannelID
	}
	putEnumWord(out, "upstream_model", o.UpstreamModel)
	return out
}

// decisionCandidatesOf 把候选集合摊成骨架里的数组。
//
// selectedID 与 withSelected 一起决定每段要不要带那个布尔。请求方向不带：
// 那时还没有选中项，一个全 false 的字段只会让人以为「一个都没选上」。
//
// # 没有数据的字段整个键都不写
//
// 指针为 nil 表示**没有数据**，与「是 0」必须分得开（见 route.Candidate 上
// 那几个字段的注释）。写成 0 会把「这个渠道没有成本数据」读成「它的成本是
// 0」——那正是路由那一侧修过的一条缺陷，快照不该把它重新引入一遍。
func decisionCandidatesOf(cands []gateway.DecisionCandidate, selectedID int64, withSelected bool) []any {
	out := make([]any, 0, len(cands))
	for _, c := range cands {
		m := map[string]any{
			"channel_id":           c.ChannelID,
			"priority":             c.Priority,
			"weight":               c.Weight,
			"consecutive_failures": c.ConsecutiveFailures,
			"success_rate":         c.SuccessRate,
			"quota_confidence":     c.QuotaConfidence,
			"sample_count":         c.SampleCount,
		}
		if withSelected {
			m["selected"] = selectedID != 0 && c.ChannelID == selectedID
		}
		if c.LatencyP95 != nil {
			m["latency_p95_ms"] = c.LatencyP95.Milliseconds()
		}
		putEnumWord(m, "quota_unit", c.QuotaUnit)
		if c.QuotaRemaining != nil {
			m["quota_remaining"] = *c.QuotaRemaining
		}
		putEnumWord(m, "quota_kind", c.QuotaKind)
		if c.CostPerUnit != nil {
			m["cost_per_unit"] = *c.CostPerUnit
		}
		if c.CacheHitRate != nil {
			m["cache_hit_rate"] = *c.CacheHitRate
		}
		out = append(out, m)
	}
	return out
}

// decisionExclusionsOf 把被筛掉的候选摊成骨架里的数组。
//
// 只有两格：身份与分类。与 decisionCandidatesOf 同一条判据——**没有摊进这个
// map 的东西，脱敏器连看一眼的机会都没有**，所以渠道名、上游地址这些不摆上来
// 比摆上来再指望某一档脱敏挡住它可靠。
//
// 分类那一格的键叫 code：它跟着 route.Exclusion.Code 与
// gateway.DecisionExclusion.Code 走，而不是 route.Exclusion.Reason——那个词在
// route 那边指给人看的那句话，两处叫反了读的人会把这一格当成原文。
//
// 取值走枚举词那一档（见 putEnumWord）：它是 route 里那几个常量之一，
// 本来就是 ASCII 词；万一哪天有人把一句自由文本塞进这个位置，取值检查会让它
// 退成占位符而不是原样落库。
func decisionExclusionsOf(excluded []gateway.DecisionExclusion) []any {
	out := make([]any, 0, len(excluded))
	for _, e := range excluded {
		m := map[string]any{"channel_id": e.ChannelID}
		putEnumWord(m, "code", e.Code)
		out = append(out, m)
	}
	return out
}

// decisionAttemptsOf 摊出每一次尝试。
//
// 状态码与分类在网关那一侧就算好了（见 gateway.DecisionAttempt）：判据是
// upstreamEvidence，与账单上那两列同源。本层只搬，不重新解释——
// 两处各推一遍的话，同一个失败在快照与账单上会有两个说法。
func decisionAttemptsOf(attempts []gateway.DecisionAttempt) []any {
	out := make([]any, 0, len(attempts))
	for _, a := range attempts {
		m := map[string]any{
			"channel_id":  a.ChannelID,
			"succeeded":   a.Succeeded,
			"duration_ms": a.Duration.Milliseconds(),
		}
		if !a.Succeeded {
			// 只有失败的那几次才带「为什么」：一次成功的尝试没有失败分类，
			// 写一个 retryable=false 进骨架会让人以为它失败过但不重试。
			m["retryable"] = a.Retryable
		}
		if a.UpstreamStatus != nil {
			m["upstream_status"] = *a.UpstreamStatus
		}
		if a.ErrorKind != nil {
			m["error_kind"] = *a.ErrorKind
		}
		out = append(out, m)
	}
	return out
}

// putEnumWord 只在有取值时写入一个字符串键。
//
// 空串整个键不写：它经 allowEnumWord 会落成一个长度 0 的文本占位
// （`{"_t":"text","chars":0}`），读起来像「这里本来有段内容被脱敏了」，
// 而实际情况是**没有这项数据**。少一个键比一个会说谎的占位好。
func putEnumWord(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}
