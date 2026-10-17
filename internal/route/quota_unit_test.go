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

	plan := seededRouter(1).Select([]Candidate{expensive, cheap}, "m")
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

	plan := seededRouter(1).Select([]Candidate{plenty, scarce}, "m")
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
			if reason := c.DrainReason(); reason != "额度已耗尽" {
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
	if reason := never.DrainReason(); reason != "" {
		t.Errorf("从未观测过不该被挡下，实际原因: %q", reason)
	}

	observed := candidate(2, "m", withQuota(quotaObservedNoValue()))
	if reason := observed.DrainReason(); reason != "额度观测过但没有可用信息" {
		t.Errorf("观测过却没有数值应当被挡下，实际原因: %q", reason)
	}

	// 同一个判据的另一半：从错误里看到的信号也算「观测过」。
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	errored := candidate(3, "m", withQuota(quota.State{LastErrorCode: "402", LastErrorAt: &at}))
	if reason := errored.DrainReason(); reason != "额度观测过但没有可用信息" {
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

	plan := seededRouter(1).Select([]Candidate{observed, healthy}, "m")
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

	plan := seededRouter(1).Select([]Candidate{never, money}, "m")
	if len(plan.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2——查不到额度不等于没有额度", len(plan.Candidates))
	}
	if len(plan.Excluded) != 0 {
		t.Errorf("Excluded = %v，期望空", plan.Excluded)
	}
}
