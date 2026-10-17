package route

import (
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/quota"
)

// 中性分是 0.5，与「查不到额度」同一档。用字面量钉住它，而不是与
// scoreQuota(quota.State{}, …) 相比——那样改掉中性值，两个用例会一起变。
func TestScoreQuotaMixedUnitsIsNeutral(t *testing.T) {
	q := quotaIn(3, "money")

	if got := scoreQuota(q, true); got != 0.5 {
		t.Errorf("跨单位时 = %v，期望中性分 0.5", got)
	}
	// 单位一致时它按余量算，**不是**中性分：这一条证明上面那个 0.5
	// 来自单位判断，而不是额度那一项恒为中性。
	// 3 的紧迫度是 1/(1+3) = 0.25，无到期时间按 0.5 算，各占一半。
	if got := scoreQuota(q, false); got != 0.375 {
		t.Errorf("同单位时 = %v，期望 0.375", got)
	}
}

// 余量为零是**单位无关**的一档：不管候选集里有几种单位，
// 「这个渠道没额度了」都该是最低分。
func TestScoreQuotaZeroIsLowestRegardlessOfUnits(t *testing.T) {
	q := quotaIn(0, "money")

	if got := scoreQuota(q, true); got != 0 {
		t.Errorf("跨单位时余量为零 = %v，期望 0", got)
	}
	if got := scoreQuota(q, false); got != 0 {
		t.Errorf("同单位时余量为零 = %v，期望 0", got)
	}
}

// 额度未知时是中性分，与单位无关：它既不该因为「不知道」被优先，
// 也不该被排到最后。
func TestScoreQuotaUnknownIsNeutral(t *testing.T) {
	if got := scoreQuota(quota.State{}, true); got != 0.5 {
		t.Errorf("跨单位时未知额度 = %v，期望 0.5", got)
	}
	if got := scoreQuota(quota.State{}, false); got != 0.5 {
		t.Errorf("同单位时未知额度 = %v，期望 0.5", got)
	}
}

// ── 可信度：权重，不是闸门 ──────────────────────────────────────────────
//
// State.Confidence 说的是「这个余量数字有多可信」。它早先在本包里没有读者
// （issue #233），于是「很久以前探测到的余量」与「刚刚探测到的」在打分里
// 完全等价——探测间隔那个配置改了，库里那一列跟着变，而排序一个字都不变。
//
// 现在的处置是把它当**权重**：分数是「这次观测说出的分」与中性分 0.5 的
// 凸组合，可信度越低越靠近中性分。**不是**低于阈值就排除：按间隔自动探测
// 上游的调度器还没有实现，一次陈旧的观测可能永远不会被刷新，排除等于让
// 一个渠道因为一个没人会再看的旧数字永久不可用。

// 置信度为 1（上游实测）时，分数与接上可信度之前**逐字节相同**。
//
// 这是「实测行为一字不变」的证明：下面每个期望值都是接上可信度之前那次
// 运行的真实输出，不是照着公式重算的。改坏了加权形状（例如把凸组合写成
// 乘法、或者顺手给实测值也打折），这几条会立刻变红。
func TestScoreQuotaFullConfidenceIsUnchanged(t *testing.T) {
	soon := time.Now().Add(30 * time.Minute)
	days3 := time.Now().Add(3 * 24 * time.Hour)
	days10 := time.Now().Add(10 * 24 * time.Hour)

	cases := []struct {
		name string
		q    quota.State
		want float64
	}{
		// 余量 3：紧迫度 1/(1+3) = 0.25，无到期时间按 0.5 算，各占一半。
		{"余量 3、无到期", quotaIn(3, "money"), 0.375},
		// 余量 1：紧迫度 0.5，与中性档的 0.5 恰好相等。
		{"余量 1、无到期", quotaIn(1, "money"), 0.5},
		// 余量 0.25：紧迫度 1/1.25 = 0.8，加中性档 0.5 → 0.65。
		{"余量 0.25、无到期", quotaIn(0.25, "money"), 0.65},
		{"余量 10、半小时后到期", quotaExpiring(10, soon), 0.5204545454545454},
		{"余量 10、三天后到期", quotaExpiring(10, days3), 0.34545454545454546},
		{"余量 2、十天后到期", quotaExpiring(2, days10), 0.3666666666666667},
		{"余量 0.5、半小时后到期", quotaExpiring(0.5, soon), 0.8083333333333333},
		{"余量 1000、十天后到期", quotaExpiring(1000, days10), 0.20049950049950052},
		{"余量 1000、无到期", quotaIn(1000, "money"), 0.2504995004995005},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.q.Confidence != 1 {
				t.Fatalf("这条用例的前提是实测（Confidence 1），实际 %v", tc.q.Confidence)
			}
			if got := scoreQuota(tc.q, false); got != tc.want {
				t.Errorf("scoreQuota = %v，期望 %v（接上可信度之前的值）", got, tc.want)
			}
		})
	}
}

// 置信度为 0 时退成中性分 0.5：与「额度未知」同一档。
//
// 两边都要钉：一个高于中性分的分数与一个低于中性分的分数，都要落到 0.5，
// 而不是各自按比例缩到 0 或缩到某个别的地方。
func TestScoreQuotaZeroConfidenceIsNeutral(t *testing.T) {
	high := withConfidence(quotaIn(0.25, "money"), 0)   // 实测时是 0.65
	low := withConfidence(quotaIn(1000, "money"), 0)    // 实测时是 0.2504995004995005
	exhausted := withConfidence(quotaIn(0, "money"), 0) // 见底
	unknown := quota.State{}                            // 从未观测过

	for name, q := range map[string]quota.State{"高于中性": high, "低于中性": low} {
		t.Run(name, func(t *testing.T) {
			if got := scoreQuota(q, false); got != 0.5 {
				t.Errorf("置信度为 0 时 = %v，期望中性分 0.5", got)
			}
		})
	}

	// 两个反例，说明上面那两条不是「什么都给 0.5」：
	//
	// 见底是**事实**，不参与可信度加权。让低可信度把它抬回中性，等于让一个
	// 旧数字重新放行一个已经用完的渠道——而它是否见底由 Usable 判，
	// 与这个数有多可信无关。
	if got := scoreQuota(exhausted, false); got != 0 {
		t.Errorf("见底且置信度为 0 时 = %v，期望 0", got)
	}
	// 从未观测过同样是 0.5，但走的是另一条分支（Remaining 为 nil）。
	if got := scoreQuota(unknown, false); got != 0.5 {
		t.Errorf("未知额度 = %v，期望 0.5", got)
	}
}

// 可信度下降时分数单调地向 0.5 靠拢，且**方向不反转**。
//
// 两个方向都要钉：一个高于 0.5 的分数降下来之后仍然 >= 0.5，一个低于 0.5
// 的分数升上去之后仍然 <= 0.5。只钉一个方向的话，把凸组合写成
// 0.5 - c*(raw-0.5) 之类的写法仍然能过——而那会让「越不可信越极端」，
// 与这条判据正好相反。
func TestScoreQuotaConfidenceMovesTowardNeutral(t *testing.T) {
	const (
		neutral = 0.5
		above   = 0.65               // 余量 0.25，无到期
		below   = 0.2504995004995005 // 余量 1000，无到期
	)
	// 1 → 0：实测、刚探测过的自记账（0.8）、衰减一截的自记账（0.6）、
	// 快到底的自记账（0.3）、不可信。
	steps := []float64{1, 0.8, 0.6, 0.3, 0}

	for name, tc := range map[string]struct {
		q   quota.State
		raw float64
	}{
		"高于中性分": {quotaIn(0.25, "money"), above},
		"低于中性分": {quotaIn(1000, "money"), below},
	} {
		t.Run(name, func(t *testing.T) {
			prev := tc.raw
			for _, c := range steps {
				got := scoreQuota(withConfidence(tc.q, c), false)

				// 落在 0.5 与 raw 之间（含两端）。
				lo, hi := neutral, tc.raw
				if lo > hi {
					lo, hi = hi, lo
				}
				if got < lo || got > hi {
					t.Fatalf("置信度 %v 时 = %v，落在 [%v, %v] 之外", c, got, lo, hi)
				}
				// 方向不反转。
				if tc.raw >= neutral && got < neutral {
					t.Fatalf("置信度 %v 时 = %v，低于中性分 0.5（方向反了）", c, got)
				}
				if tc.raw <= neutral && got > neutral {
					t.Fatalf("置信度 %v 时 = %v，高于中性分 0.5（方向反了）", c, got)
				}
				// 单调：每一步都更靠近中性分，而不是来回摆。
				if dist(got, neutral) > dist(prev, neutral) {
					t.Fatalf("置信度 %v 时 = %v，比上一档 %v 离中性分更远（不单调）", c, got, prev)
				}
				prev = got
			}
			// 最后一档必须真的到中性分，不能只是「靠近」。
			if got := scoreQuota(withConfidence(tc.q, 0), false); got != neutral {
				t.Errorf("置信度 0 时 = %v，期望 %v", got, neutral)
			}
		})
	}
}

// 越界的置信度要收进 0..1。
//
// 库里的列有 CHECK、quota.Merge 只写三个常量，所以越界值只可能来自本包
// 之外（Score 是导出 API，Candidate.Quota 由调用方给）。大于 1 会让一个
// 候选拿到比实测还高的分，负值会把「向中性分靠拢」整个反过来。
func TestScoreQuotaClampsOutOfRangeConfidence(t *testing.T) {
	q := quotaIn(0.25, "money") // 实测时 0.65
	if got := scoreQuota(withConfidence(q, 1.5), false); got != 0.65 {
		t.Errorf("置信度 1.5 时 = %v，期望与置信度 1 相同（0.65）", got)
	}
	if got := scoreQuota(withConfidence(q, -1), false); got != 0.5 {
		t.Errorf("置信度 -1 时 = %v，期望与置信度 0 相同（0.5）", got)
	}
}

// issue #233 的原始诉求，在 Select 这一层钉住：**很久以前探测到的余量，
// 与刚刚探测到的，在打分里不再等价**。
//
// 两个候选只有可信度不同（余量、单位、优先级、权重、健康度都一样），
// 所以排序的差别只能来自可信度那一项。
//
// 差别往哪个方向走，取决于这次观测说出的分落在中性分的哪一侧：
//
//   - 余量 0.5（实测时 0.5833，高于中性分）：「快用完了，先用掉它」这个
//     判断越可信越该照做，所以刚探测到的排在前面。
//   - 余量 1000（实测时 0.2505，低于中性分）：陈旧的观测退向「没有意见」，
//     于是它反而排到了前面——一个「余量还很充裕」的旧数字不再被当成
//     「确实很充裕」。这一条看起来反直觉，但它是降权语义的必然结果，
//     也正是「不再等价」的证明：两者若还相等，这条会失败。
//
// 两档都必须留在候选集里：降权不是排除。
func TestSelectNoLongerTreatsStaleQuotaAsFresh(t *testing.T) {
	const (
		fresh = 1   // 刚刚探测到
		stale = 0.2 // 很久以前探测到（自记账衰减到底）
	)
	cases := []struct {
		name      string
		q         quota.State
		wantFirst int64
	}{
		{"余量 0.5：刚探测到的先选", quotaIn(0.5, "money"), 1},
		{"余量 1000：陈旧的退向中性分，反而先选", quotaIn(1000, "money"), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			justProbed := candidate(1, "m", withQuota(withConfidence(tc.q, fresh)))
			longAgo := candidate(2, "m", withQuota(withConfidence(tc.q, stale)))

			plan := seededRouter(1).Select([]Candidate{longAgo, justProbed}, "m", Needs{})
			if len(plan.Candidates) != 2 {
				t.Fatalf("候选数 = %d，期望 2——降权不是排除", len(plan.Candidates))
			}
			if len(plan.Excluded) != 0 {
				t.Errorf("Excluded = %v，期望空", plan.Excluded)
			}
			if got := plan.Candidates[0].Channel.ID; got != tc.wantFirst {
				t.Errorf("排在第一的是渠道 %d，期望 %d", got, tc.wantFirst)
			}
			// 分数确实不同——「不再等价」就是这一条。
			if a, b := seededRouter(1).Score(justProbed), seededRouter(1).Score(longAgo); a == b {
				t.Errorf("两个候选的分数都是 %v，可信度没有生效", a)
			}
		})
	}
}

// dist 是到中性分的距离，用于判断「是否更靠近中性分」。
func dist(v, neutral float64) float64 {
	if v > neutral {
		return v - neutral
	}
	return neutral - v
}

// 候选集里有没有多种单位，是**整个集合**的性质。
func TestMixedQuotaUnits(t *testing.T) {
	known := func(unit string) Candidate {
		return candidate(1, "m", withQuota(quotaIn(1, unit)))
	}
	unknown := func() Candidate {
		return candidate(1, "m", withQuota(quota.State{}))
	}

	cases := []struct {
		name string
		set  []Candidate
		want bool
	}{
		{"一个候选都没有", nil, false},
		{"只有一个已知额度", []Candidate{known("money")}, false},
		{"两个同单位", []Candidate{known("money"), known("money")}, false},
		{"两个不同单位", []Candidate{known("money"), known("tokens")}, true},
		// 单位为空也算一种：一个「不知道按什么计」的渠道与一个「按元计」的
		// 渠道之间同样不可比。
		{"空单位与已知单位", []Candidate{known(""), known("money")}, true},
		// 额度未知的渠道那一项本来就是中性分，不该把别人也拖成中性分。
		{"已知与未知混在一起", []Candidate{known("money"), unknown()}, false},
		{"只有一个未知额度", []Candidate{unknown(), unknown()}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MixedQuotaUnits(tc.set); got != tc.want {
				t.Errorf("mixedQuotaUnits = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 排序真的用上了那个判断。
//
// # 这条钉住的是什么
//
// 上一条用例证明 mixedQuotaUnits 判对了，这一条证明 order 把它交给了
// scoreQuota。少了它，一个「order 永远传 false」的错误不会被任何用例发现。
//
// 构造：A 便宜但额度按 token 计（余量看着很大），B 贵但额度按元计
// （余量看着很小）。单位混在一起时，「余量小更紧迫」这个偏好会把 B 抬到
// A 前面——分差来自单位而不是余量。修好之后两者都是中性分，A 靠成本胜出。
func TestSelectIgnoresQuotaAcrossUnits(t *testing.T) {
	cheap := candidate(1, "m",
		withCost(0.0001),
		withQuota(quotaIn(1_000_000, "tokens")))
	expensive := candidate(2, "m",
		withCost(1),
		withQuota(quotaIn(0.1, "money")))

	plan := seededRouter(1).Select([]Candidate{expensive, cheap}, "m", Needs{})
	if len(plan.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(plan.Candidates))
	}
	if got := plan.Candidates[0].Channel.ID; got != 1 {
		t.Errorf("排在第一的是渠道 %d，期望 1（跨单位时额度不比，便宜的那个该先选）", got)
	}
}

// 单位一致时额度照旧参与排序：这条与上一条相对，说明上一条的结论不是
// 「额度永远不参与」。
//
// 两个候选只有余量不同，余量少的更紧迫，所以先选。
func TestSelectComparesQuotaWithinSameUnit(t *testing.T) {
	scarce := candidate(1, "m", withQuota(quotaIn(0.1, "money")))
	plenty := candidate(2, "m", withQuota(quotaIn(10_000, "money")))

	plan := seededRouter(1).Select([]Candidate{plenty, scarce}, "m", Needs{})
	if got := plan.Candidates[0].Channel.ID; got != 1 {
		t.Errorf("排在第一的是渠道 %d，期望 1（同单位时余量少的先选）", got)
	}
}

// ── 判断那一半：route 与 internal/quota 的语义差异 ──────────────────────
//
// route 早先自己重写了一遍额度判断（读 Known / Remaining 再判），
// 现在改成调 quota.State 的 Routable / Usable。下面几条把两处语义的
// 每一个格子都钉住——差异表见交付报告，这里是它的可执行形态。

// 余量为 0 与余量为负都算「不可用」：前者是见底，后者只可能来自手工写入
// （写入路径上 Validate 会拒绝负值），但对路由来说都是「别选它」。
func TestDrainReasonQuotaNotUsable(t *testing.T) {
	cases := []struct {
		name string
		q    quota.State
	}{
		{"余量恰好为零", quotaLeft(0)},
		{"余量为负", quotaLeft(-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := candidate(1, "m", withQuota(tc.q))
			if _, reason := c.DrainReason(); reason != "额度已耗尽" {
				t.Errorf("DrainReason = %q，期望「额度已耗尽」", reason)
			}
		})
	}
}

// 「从未观测过」与「观测过却没拿到数值」是两回事：
// 前者是上游不支持查询额度，不该被扣分；后者是状态不足以支撑决策，挡下。
//
// 这一格就是 route 与 quota 唯一的语义差异。早先 route 的输入类型是一次
// 有损投影（只有一个 Known 布尔），两者在它眼里长得一样。
func TestDrainReasonDistinguishesNeverObservedFromObservedWithoutValue(t *testing.T) {
	never := candidate(1, "m", withQuota(quota.State{}))
	if _, reason := never.DrainReason(); reason != "" {
		t.Errorf("从未观测过不该被挡下，实际原因: %q", reason)
	}

	observed := candidate(2, "m", withQuota(quotaObservedNoValue()))
	if _, reason := observed.DrainReason(); reason != "额度观测过但没有可用信息" {
		t.Errorf("观测过却没有数值应当被挡下，实际原因: %q", reason)
	}

	// 同一个判据的另一半：从错误里看到的信号也算「观测过」。
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	errored := candidate(3, "m", withQuota(quota.State{LastErrorCode: "402", LastErrorAt: &at}))
	if _, reason := errored.DrainReason(); reason != "额度观测过但没有可用信息" {
		t.Errorf("有错误码却没有数值应当被挡下，实际原因: %q", reason)
	}
}

// 被挡下的候选不进 Plan，而不是以「分数低」的形式留在里面。
//
// 判断与排序是两个角色：一个注定失败的候选不该靠分数排在最后——
// 候选集里只剩它的时候，它就会被选中。
func TestSelectExcludesObservedWithoutValue(t *testing.T) {
	observed := candidate(1, "m", withQuota(quotaObservedNoValue()))
	healthy := candidate(2, "m", withQuota(quotaLeft(100)))

	plan := seededRouter(1).Select([]Candidate{observed, healthy}, "m", Needs{})
	if len(plan.Candidates) != 1 || plan.Candidates[0].Channel.ID != 2 {
		t.Fatalf("候选集 = %v，期望只剩渠道 2", plan.Candidates)
	}
	if len(plan.Excluded) != 1 || plan.Excluded[0].ChannelID != 1 {
		t.Fatalf("Excluded = %v，期望记下渠道 1", plan.Excluded)
	}
	if plan.Excluded[0].Reason != "额度观测过但没有可用信息" {
		t.Errorf("Excluded 原因 = %q", plan.Excluded[0].Reason)
	}
}

// 余量未知但从未观测过：既不排除，也不进跨单位的比较集合。
func TestSelectKeepsNeverObservedQuota(t *testing.T) {
	never := candidate(1, "m", withQuota(quota.State{}))
	money := candidate(2, "m", withQuota(quotaIn(10, "money")))

	plan := seededRouter(1).Select([]Candidate{never, money}, "m", Needs{})
	if len(plan.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2——查不到额度不等于没有额度", len(plan.Candidates))
	}
	if len(plan.Excluded) != 0 {
		t.Errorf("Excluded = %v，期望空", plan.Excluded)
	}
}
