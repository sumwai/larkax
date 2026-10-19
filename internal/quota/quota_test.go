package quota

import (
	"fmt"
	"math"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// testStaleAfter 是这一组用例喂给 Merge 的陈旧阈值。
//
// 写成**字面量**而不是拿 SelfAccountingStaleAfter 算一个：那几条曲线正是
// 要钉住的东西，让被测函数给自己出题，把阈值改小十倍也照样通过。
// 15 分钟与出厂配置一致（探测间隔 5 分钟 × 3 倍），所以这里断言的曲线
// 就是出厂配置下那条。导出规则本身另有一条用例。
const testStaleAfter = 15 * time.Minute

func f(v float64) *float64 { return &v }

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

// 陈旧阈值由探测间隔导出：探测间隔改小，阈值跟着变小。
//
// 这一条钉的是「那个键真的被读了」。原先阈值是写死的 15 分钟，探测间隔
// 从 5 分钟改成 1 分钟它一动不动，而读代码的人会以为它跟着走。
func TestSelfAccountingStaleAfterFollowsProbeInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{5 * time.Minute, 15 * time.Minute},
		{time.Minute, 3 * time.Minute},
		{30 * time.Second, 90 * time.Second},
	}
	for _, tc := range cases {
		got, err := SelfAccountingStaleAfter(tc.interval)
		if err != nil {
			t.Errorf("间隔 %v 不该被拒: %v", tc.interval, err)
			continue
		}
		if got != tc.want {
			t.Errorf("间隔 %v 的陈旧阈值 = %v，期望 %v", tc.interval, got, tc.want)
		}
	}

	// 非正数报错而不回落：阈值非正时任何一次自记账都立刻被判成陈旧，
	// 那是配置写成 0 之后一个安静的行为改变。
	for _, bad := range []time.Duration{0, -time.Minute} {
		got, err := SelfAccountingStaleAfter(bad)
		if err == nil {
			t.Errorf("间隔 %v 应被拒，却得到 %v", bad, got)
			continue
		}
		if !contains(err.Error(), "探测间隔") {
			t.Errorf("错误信息该说清是哪个配置项: %v", err)
		}
	}
}

func TestValidateAcceptsGoodObservations(t *testing.T) {
	cases := []Observation{
		{Kind: KindAPIQuery, Remaining: f(100), ObservedAt: base},
		{Kind: KindAPIQuery, Exhausted: true, ObservedAt: base},
		{Kind: KindSelfAccounting, Remaining: f(0), Exhausted: true, ObservedAt: base},
		{Kind: KindErrorObserved, ErrorCode: "insufficient_quota", ObservedAt: base},
	}
	for i, obs := range cases {
		if err := Validate(obs, base); err != nil {
			t.Errorf("第 %d 个观测不该被拒: %v", i, err)
		}
	}
}

// 把「消耗」当成「剩余」传进来会得到一个负值。它不会让程序崩，
// 只会让额度慢慢偏掉，而偏掉之后要很久才看得出来。
func TestValidateRejectsNegativeRemaining(t *testing.T) {
	err := Validate(Observation{Kind: KindSelfAccounting, Remaining: f(-5), ObservedAt: base}, base)
	if err == nil {
		t.Fatal("负余量应被拒绝")
	}
	if !contains(err.Error(), "Exhausted") {
		t.Errorf("错误信息应指出正确做法: %v", err)
	}
}

func TestValidateRejectsBadKind(t *testing.T) {
	if err := Validate(Observation{Kind: "guessed", ObservedAt: base}, base); err == nil {
		t.Error("未知观测方式应被拒绝")
	}
	if err := Validate(Observation{ObservedAt: base}, base); err == nil {
		t.Error("空观测方式应被拒绝")
	}
}

// 一次「实测」既没给出数值也没说耗尽，等于什么都没观测到。
// 放过去会让状态被标成「已实测」而实际毫无信息。
func TestValidateRejectsEmptyAPIQuery(t *testing.T) {
	err := Validate(Observation{Kind: KindAPIQuery, ObservedAt: base}, base)
	if err == nil {
		t.Fatal("空的实测观测应被拒绝")
	}
}

func TestValidateRejectsFutureAndNonFinite(t *testing.T) {
	future := base.Add(time.Hour)
	if err := Validate(Observation{Kind: KindSelfAccounting, Remaining: f(1), ObservedAt: future}, base); err == nil {
		t.Error("将来的观测时间应被拒绝")
	}
	nan := f(math.NaN())
	if err := Validate(Observation{Kind: KindSelfAccounting, Remaining: nan, ObservedAt: base}, base); err == nil {
		t.Error("NaN 应被拒绝")
	}
	inf := f(math.Inf(1))
	if err := Validate(Observation{Kind: KindSelfAccounting, Remaining: inf, ObservedAt: base}, base); err == nil {
		t.Error("Inf 应被拒绝")
	}
}

// ── 合并 ────────────────────────────────────────────────────────────────

// 实测值无条件覆盖：它是我们能拿到的最好的信息。
func TestMergeAPIQueryOverrides(t *testing.T) {
	prev := State{ChannelID: 1, Remaining: f(50), Kind: KindSelfAccounting,
		Estimated: true, Confidence: 0.5, LastErrorCode: "429"}

	next := Merge(prev, Observation{Kind: KindAPIQuery, Remaining: f(999), ObservedAt: base}, base, testStaleAfter)

	if next.Remaining == nil || *next.Remaining != 999 {
		t.Errorf("Remaining = %s，期望 999", showPtr(next.Remaining))
	}
	if next.Confidence != confidenceAPIQuery {
		t.Errorf("Confidence = %v", next.Confidence)
	}
	if next.Estimated {
		t.Error("实测值不该标记为估算")
	}
	if next.LastProbeAt == nil {
		t.Error("应记录探测时间")
	}
	// 实测成功即清掉之前的错误信号：它已被更新的信息取代
	if next.LastErrorCode != "" {
		t.Errorf("实测成功后应清掉旧错误信号，实际 %q", next.LastErrorCode)
	}
}

// 重置时刻跟着实测走：带回来就换成新的，没带回来就保留旧的。
//
// 「这次没提」不等于「确认不会重置」——用 NULL 覆盖会让 scoreQuota 的
// 紧迫度那一半悄悄退回中性分。
func TestMergeAPIQueryResetsAt(t *testing.T) {
	oldResets := base.Add(24 * time.Hour)
	prev := State{ChannelID: 1, Remaining: f(50), Kind: KindAPIQuery, ResetsAt: &oldResets}

	newResets := base.Add(48 * time.Hour)
	next := Merge(prev, Observation{
		Kind: KindAPIQuery, Remaining: f(60), ResetsAt: &newResets, ObservedAt: base,
	}, base, testStaleAfter)
	if next.ResetsAt == nil || !next.ResetsAt.Equal(newResets) {
		t.Errorf("带回来时 ResetsAt = %v，期望 %v", next.ResetsAt, newResets)
	}

	next = Merge(prev, Observation{
		Kind: KindAPIQuery, Remaining: f(60), ObservedAt: base,
	}, base, testStaleAfter)
	if next.ResetsAt == nil || !next.ResetsAt.Equal(oldResets) {
		t.Errorf("没带回来时 ResetsAt = %v，期望保留 %v", next.ResetsAt, oldResets)
	}
}

// 从错误码推断「额度不足」是定性但确定的信号——
// 它比任何推算都更能说明「现在选它会失败」。
func TestMergeExhaustedFromError(t *testing.T) {
	prev := State{ChannelID: 1, Remaining: f(1000), Kind: KindAPIQuery, Confidence: 1.0}

	next := Merge(prev, Observation{
		Kind: KindErrorObserved, Exhausted: true, ErrorCode: "insufficient_quota", ObservedAt: base,
	}, base, testStaleAfter)

	if next.Remaining == nil || *next.Remaining != 0 {
		t.Errorf("Remaining = %s，期望 0", showPtr(next.Remaining))
	}
	if next.LastErrorCode != "insufficient_quota" {
		t.Errorf("LastErrorCode = %q", next.LastErrorCode)
	}
	if next.Confidence != confidenceErrorObserved {
		t.Errorf("Confidence = %v", next.Confidence)
	}
}

// 一个与额度无关的错误（例如 429 限流）不该动 Remaining。
// 这里最容易写错的是顺手把可信度也改了——那会让一个准确的实测值被降级。
func TestMergeUnrelatedErrorDoesNotTouchRemaining(t *testing.T) {
	prev := State{ChannelID: 1, Remaining: f(1000), Kind: KindAPIQuery,
		Confidence: confidenceAPIQuery, LastProbeAt: &base}

	next := Merge(prev, Observation{
		Kind: KindErrorObserved, ErrorCode: "rate_limit_exceeded", ObservedAt: base,
	}, base, testStaleAfter)

	if next.Remaining == nil || *next.Remaining != 1000 {
		t.Errorf("Remaining 被无关错误改动了: %s", showPtr(next.Remaining))
	}
	if next.Confidence != confidenceAPIQuery {
		t.Errorf("无关错误不该降低可信度: %v", next.Confidence)
	}
	if next.LastErrorCode != "rate_limit_exceeded" {
		t.Errorf("错误码应被记下: %q", next.LastErrorCode)
	}
}

// 自记账是一次**推进**，不是一次测量：它以最近一次已知值为基准减去消耗。
// 所以即便刚探测过，它也必须生效——否则两次探测之间额度永远停在实测值上，
// 路由会以为一个已经被用掉的渠道还有满额。
func TestSelfAccountingAdvancesPastFreshAPIQuery(t *testing.T) {
	probedAt := base.Add(-time.Minute)
	prev := State{ChannelID: 1, Remaining: f(1000), Kind: KindAPIQuery,
		Confidence: confidenceAPIQuery, LastProbeAt: &probedAt}

	next := Merge(prev, Observation{Kind: KindSelfAccounting, Remaining: f(900), ObservedAt: base}, base, testStaleAfter)

	if *next.Remaining != 900 {
		t.Errorf("自记账应在实测值之上推进: %v", *next.Remaining)
	}
	if !next.Estimated {
		t.Error("推进后的值应标记为估算")
	}
}

// 刚探测过时，自记账的可信度是基线（0.8）；离探测越远越低。
// 它永远达不到实测的水平：累积误差是自记账的固有代价。
func TestSelfAccountingConfidenceDecays(t *testing.T) {
	mk := func(probeAgo time.Duration) float64 {
		probedAt := base.Add(-probeAgo)
		prev := State{Remaining: f(1000), Kind: KindAPIQuery, LastProbeAt: &probedAt}
		next := Merge(prev, Observation{Kind: KindSelfAccounting, Remaining: f(900), ObservedAt: base}, base, testStaleAfter)
		return next.Confidence
	}

	fresh := mk(time.Minute)
	stale := mk(time.Hour)
	ancient := mk(48 * time.Hour)

	if fresh != confidenceSelfAccountingBase {
		t.Errorf("刚探测过的可信度 = %v，期望基线 %v", fresh, confidenceSelfAccountingBase)
	}
	if !(fresh > stale && stale > ancient) {
		t.Errorf("可信度应随探测陈旧程度递减: fresh=%v stale=%v ancient=%v", fresh, stale, ancient)
	}
	if fresh >= confidenceAPIQuery {
		t.Error("自记账的可信度不该达到实测水平")
	}
	if ancient <= 0 {
		t.Error("可信度不该降到零：旧估计仍有信息量")
	}
}

// 实测已经过时之后，让自记账接管——一个陈旧的数不该继续被当真。
func TestSelfAccountingTakesOverWhenProbeIsStale(t *testing.T) {
	staleProbe := base.Add(-testStaleAfter - time.Hour)
	prev := State{ChannelID: 1, Remaining: f(1000), Kind: KindAPIQuery,
		Confidence: confidenceAPIQuery, LastProbeAt: &staleProbe}

	next := Merge(prev, Observation{Kind: KindSelfAccounting, Remaining: f(800), ObservedAt: base}, base, testStaleAfter)

	if *next.Remaining != 800 {
		t.Errorf("陈旧的实测值应被自记账接管: %v", *next.Remaining)
	}
	if !next.Estimated {
		t.Error("应标记为估算")
	}
	if next.Confidence >= confidenceAPIQuery {
		t.Errorf("接管后的可信度应低于实测: %v", next.Confidence)
	}
}

// 没有任何实测值时，自记账是唯一的信息来源。
func TestSelfAccountingUsedWhenNoMeasure(t *testing.T) {
	prev := State{ChannelID: 1}
	next := Merge(prev, Observation{Kind: KindSelfAccounting, Remaining: f(500), ObservedAt: base}, base, testStaleAfter)

	if next.Remaining == nil || *next.Remaining != 500 {
		t.Errorf("Remaining = %s", showPtr(next.Remaining))
	}
	if !next.Estimated {
		t.Error("应标记为估算")
	}
	if next.Confidence != confidenceSelfAccountingBase {
		t.Errorf("Confidence = %v，期望 %v", next.Confidence, confidenceSelfAccountingBase)
	}
}

// 自记账的可信度永远达不到 1：它建立在「我们记全了每一次消耗」这个假设上，
// 而任何一次漏记都会让它偏高。
func TestSelfAccountingNeverFullyConfident(t *testing.T) {
	prev := State{}
	for i := 0; i < 5; i++ {
		prev = Merge(prev, Observation{Kind: KindSelfAccounting, Remaining: f(100), ObservedAt: base}, base, testStaleAfter)
	}
	if prev.Confidence >= confidenceAPIQuery {
		t.Errorf("自记账可信度不该达到实测水平: %v", prev.Confidence)
	}
}

func TestMergeKeepsUnitWhenNotGiven(t *testing.T) {
	prev := State{Unit: "CNY"}
	next := Merge(prev, Observation{Kind: KindSelfAccounting, Remaining: f(1), ObservedAt: base}, base, testStaleAfter)
	if next.Unit != "CNY" {
		t.Errorf("Unit = %q", next.Unit)
	}
	next = Merge(next, Observation{Kind: KindAPIQuery, Remaining: f(1), Unit: "USD", ObservedAt: base}, base, testStaleAfter)
	if next.Unit != "USD" {
		t.Errorf("新观测带了单位时应更新: %q", next.Unit)
	}
}

// ── 自记账 ──────────────────────────────────────────────────────────────

func TestProjectSubtractsConsumption(t *testing.T) {
	prev := State{Remaining: f(100), Unit: "CNY"}
	obs := Project(prev, 30, base)

	if obs.Remaining == nil || *obs.Remaining != 70 {
		t.Errorf("Remaining = %s，期望 70", showPtr(obs.Remaining))
	}
	if obs.Kind != KindSelfAccounting {
		t.Errorf("Kind = %q", obs.Kind)
	}
	if obs.Exhausted {
		t.Error("还有余量时不该标记耗尽")
	}
}

// 消耗超过记录的余量说明自记账有漏（例如在别处也用了这个账号）。
// 记成 0 而不是负数：负余量在路由与界面上都没有意义。
func TestProjectClampsAtZero(t *testing.T) {
	prev := State{Remaining: f(10)}
	obs := Project(prev, 999, base)

	if obs.Remaining == nil || *obs.Remaining != 0 {
		t.Errorf("Remaining = %s，期望 0", showPtr(obs.Remaining))
	}
	if !obs.Exhausted {
		t.Error("见底应标记耗尽")
	}
}

// 消耗正好等于余量：新的余量是 0，并且标记耗尽。
//
// 等价变异（gremlins CONDITIONALS_BOUNDARY，把 `if left < 0` 的 `<` 改成
// `<= 0`）：恰好为 0 那一格也进夹取分支，而那里 `left = 0` 是恒等变换
// （值本来就是 0），后面的 `Exhausted` 又由同一个 `left == 0` 判据给出——
// 两版对任何输入都同值。这一格就是本条用例。
func TestProjectExhaustsExactlyToZero(t *testing.T) {
	prev := State{Remaining: f(10)}
	obs := Project(prev, 10, base)
	if obs.Remaining == nil || *obs.Remaining != 0 || !obs.Exhausted {
		t.Errorf("正好用尽应标记耗尽: %+v", obs)
	}
}

// 余量未知时推不出新值——凭消耗量算不出总量。
func TestProjectUnknownStaysUnknown(t *testing.T) {
	obs := Project(State{}, 5, base)
	if obs.Remaining != nil {
		t.Errorf("余量未知时应保持未知: %s", showPtr(obs.Remaining))
	}
}

// 自记账在「先记账再探测」的循环里不该丢信息。
func TestProjectThenMergeKeepsEstimate(t *testing.T) {
	state := State{ChannelID: 1, Remaining: f(1000), Unit: "CNY", Kind: KindAPIQuery,
		Confidence: confidenceAPIQuery, LastProbeAt: &base}

	// 消耗 200
	state = Merge(state, Project(state, 200, base), base, testStaleAfter)
	if state.Remaining == nil || *state.Remaining != 800 {
		t.Fatalf("推进后 Remaining = %s，期望 800", showPtr(state.Remaining))
	}

	// 再探测，拿到实测的 750（说明自记账略偏高，这很常见）
	state = Merge(state, Observation{Kind: KindAPIQuery, Remaining: f(750), ObservedAt: base}, base, testStaleAfter)
	if *state.Remaining != 750 {
		t.Errorf("实测应覆盖估算: %v", *state.Remaining)
	}
	if state.Estimated {
		t.Error("实测之后不该仍标记为估算")
	}
}

// ── 可用性 ──────────────────────────────────────────────────────────────

// 未知一律视为可用——把未知当不可用等于默认停用所有查不到额度的渠道。
func TestUsableTreatsUnknownAsUsable(t *testing.T) {
	if !(State{}).Usable() {
		t.Error("未知余量应视为可用")
	}
	if !(State{Remaining: f(0.01)}).Usable() {
		t.Error("还有余量应视为可用")
	}
	if (State{Remaining: f(0)}).Usable() {
		t.Error("余量为 0 应视为不可用")
	}
}

func TestRoutableTreatsNeverObservedAsRoutable(t *testing.T) {
	if !(State{}).Routable() {
		t.Error("从未观测过应视为可路由")
	}
	if !(State{Remaining: f(0)}).Routable() {
		t.Error("余量为 0 仍然是「知道」的状态，可路由")
	}
	if !(State{Remaining: f(1)}).Routable() {
		t.Error("有值可路由")
	}
}

func TestRoutableWithObservedButUnknownValue(t *testing.T) {
	at := base
	s := State{LastProbeAt: &at, Remaining: nil}
	if s.Routable() {
		t.Error("观测过却没拿到数值，视为不可路由才对")
	}
}

// 「观测过」有两条来源：探针跑过（last_probe_at），与从上游错误里看到过
// 信号（last_error_code）。两条都算——判据问的是「有没有观测过」，
// 而不是「探针有没有跑过」。
func TestRoutableWithObservedErrorCode(t *testing.T) {
	at := base
	s := State{LastErrorCode: "insufficient_quota", LastErrorAt: &at}
	if s.Routable() {
		t.Error("看到过错误码却没拿到数值，视为不可路由才对")
	}
}

// 有数值时观测痕迹不参与判断：那两列只在「没有数值」时才有话说。
// 否则一次与额度无关的错误码会把一个有余量的渠道判成不可路由。
func TestRoutableWithValueIgnoresTraces(t *testing.T) {
	at := base
	s := State{Remaining: f(3), LastProbeAt: &at, LastErrorCode: "429", LastErrorAt: &at}
	if !s.Routable() {
		t.Error("有余量就是「知道」，可路由")
	}
}

// 余量为负算不可用。它只可能来自手工写入——写入路径上 Validate 会拒绝
// 负值——但对路由来说与「见底」是同一个结论。
func TestUsableTreatsNegativeAsUnusable(t *testing.T) {
	if (State{Remaining: f(-0.01)}).Usable() {
		t.Error("负余量应视为不可用")
	}
}

// 「观测时间在将来」的容差是**恰好一分钟**：一分钟以内放行（容忍时钟抖动），
// 超过就拒绝。用两个相邻的字面量把边界钉住，而不是断言「将来的被拒」——
// 后者把容差改成一天也照样通过。
func TestValidateFutureToleranceIsExactlyOneMinute(t *testing.T) {
	atLimit := Observation{Kind: KindSelfAccounting, Remaining: f(1), ObservedAt: base.Add(time.Minute)}
	if err := Validate(atLimit, base); err != nil {
		t.Errorf("恰好一分钟不该被拒: %v", err)
	}
	overLimit := Observation{
		Kind: KindSelfAccounting, Remaining: f(1),
		ObservedAt: base.Add(time.Minute + time.Nanosecond),
	}
	if err := Validate(overLimit, base); err == nil {
		t.Error("超过一分钟应被拒绝")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 来自变异测试（scripts/check-mutation.sh，workers=1、系数 10）。
// 存活集中在自记账可信度的**衰减曲线**上——而那条曲线决定
// 「一个估算值还能不能用来做路由判断」。

// 衰减曲线要逐点精确：三个段（基线 / 线性衰减 / 触底）各自的边界。
//
// 这条曲线原本只被断言了「越久越低」这种相对关系，于是改常量、
// 改插值系数，只要单调性不变就测不出来。
//
// 等价变异（gremlins CONDITIONALS_BOUNDARY，把 `if elapsed <= staleAfter` 的
// `<=` 改成 `<`）：恰好等于阈值那一格不再走「直接给基线」，而是落到线性段上。
// 那一格的 ratio 恰好是 0，`confidenceSelfAccountingBase -
// (confidenceSelfAccountingBase-floor)*0` 与直接返回基线**逐位相同**——
// 乘 0 与减 0 在 float64 里都是精确的，那一格两版给出的是同一个数。
// 下面 `score(testStaleAfter)` 那一行钉的就是这一格。
func TestSelfAccountingConfidenceCurvePoints(t *testing.T) {
	const baseScore = 0.8 // confidenceSelfAccountingBase
	const floorScore = 0.2

	score := func(sinceProbe time.Duration) float64 {
		probedAt := base.Add(-sinceProbe)
		prev := State{Remaining: f(1000), Kind: KindAPIQuery, LastProbeAt: &probedAt}
		next := Merge(prev, Observation{Kind: KindSelfAccounting, Remaining: f(900), ObservedAt: base}, base, testStaleAfter)
		return next.Confidence
	}

	// 阈值用**字面量**钉住。段一那两条拿 `testStaleAfter`
	// 自己测自己：把它改成 15 秒，它们照样通过，
	// 于是测到的是「相对关系」而不是这个数。15 分钟与出厂配置一致
	// （探测间隔 5 分钟 × 3 倍，见 SelfAccountingStaleAfter），
	// 改那个倍数就该连带改这里的字面量。
	if got := score(14 * time.Minute); got != baseScore {
		t.Errorf("14 分钟陈旧应仍给基线 %v，得到 %v", baseScore, got)
	}
	if got := score(16 * time.Minute); got == baseScore {
		t.Errorf("16 分钟陈旧不该仍给基线 %v", got)
	}

	// 段一：陈旧阈值本身与之内一律给基线
	if got := score(testStaleAfter); got != baseScore {
		t.Errorf("恰好等于陈旧阈值时应给基线 %v，得到 %v", baseScore, got)
	}
	if got := score(testStaleAfter - time.Second); got != baseScore {
		t.Errorf("阈值之内应给基线，得到 %v", got)
	}

	// 段二：线性衰减的**中点**。过阈值后 3 小时（fadeOver 是 6 小时）
	// 应当恰好落在基线与下限的中间。
	mid := score(testStaleAfter + 3*time.Hour)
	wantMid := baseScore - (baseScore-floorScore)*0.5
	if diff := mid - wantMid; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("衰减中点的可信度 = %v，期望 %v", mid, wantMid)
	}

	// 段三：过了 fadeOver 之后触底，不再降
	if got := score(testStaleAfter + 6*time.Hour); got != floorScore {
		t.Errorf("恰好过 fadeOver 时应触底 %v，得到 %v", floorScore, got)
	}
	if got := score(testStaleAfter + 100*time.Hour); got != floorScore {
		t.Errorf("很久之后仍应是下限 %v，得到 %v", floorScore, got)
	}
}

// ExhaustedRetryDue 只对「有数值且见底」这一档回答「该不该放行」。
//
// 冷却期用字面量 15 分钟，时刻也写成相对它的具体偏移：这条判据的全部内容
// 就是「距上次尝试够不够久」，拿常量去测自己会把边界一起改掉。
func TestExhaustedRetryDue(t *testing.T) {
	cooldown := 15 * time.Minute
	now := base
	at := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}

	cases := []struct {
		name string
		q    State
		want bool
	}{
		{"从未观测过（余量未知）", State{}, false},
		{"余量为正", State{Remaining: f(1), LastProbeAttemptAt: at(-time.Hour)}, false},
		{"余量未知但观测过（Routable 为假）", State{LastProbeAt: at(-time.Hour), LastProbeAttemptAt: at(-time.Hour)}, false},
		{"见底且从未尝试过探测", State{Remaining: f(0)}, true},
		{"见底且恰好过了冷却期", State{Remaining: f(0), LastProbeAttemptAt: at(-cooldown)}, true},
		{"见底且差一秒没到冷却期", State{Remaining: f(0), LastProbeAttemptAt: at(-cooldown + time.Second)}, false},
		{"余量为负：同样算见底", State{Remaining: f(-1), LastProbeAttemptAt: at(-cooldown)}, true},
		{"尝试时刻在将来（时钟偏差）：不放行", State{Remaining: f(0), LastProbeAttemptAt: at(time.Minute)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.ExhaustedRetryDue(now, cooldown); got != tc.want {
				t.Errorf("ExhaustedRetryDue = %v，期望 %v", got, tc.want)
			}
		})
	}

	// 非正冷却期表示不启用恢复路径，而不是「零冷却」：后者会放行每一次请求。
	q := State{Remaining: f(0), LastProbeAttemptAt: at(-time.Hour)}
	for _, c := range []time.Duration{0, -time.Minute} {
		if q.ExhaustedRetryDue(now, c) {
			t.Errorf("冷却期 %v 时应表示不启用恢复路径，却放行了", c)
		}
	}
}
