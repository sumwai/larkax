package snapshotwire

import (
	"sort"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/gateway"
)

// fullDecisionCandidate 是一个**每一项都有数据**的候选，用来钉住适配层摊出来的
// 全部键。
//
// 缺数据的情形另有一条用例（TestRedactDecisionOmitsMissingValues）：两者分开，
// 是因为「键少了」与「值是 0」在断言里都很容易写成同一个样子，而它们说的
// 是两件事。
func fullDecisionCandidate(id int64) gateway.DecisionCandidate {
	latency := 120 * time.Millisecond
	remaining := 4096.0
	cost := 0.000002
	hit := 0.5
	return gateway.DecisionCandidate{
		ChannelID:           id,
		Priority:            3,
		Weight:              2,
		ConsecutiveFailures: 0,
		SuccessRate:         0.98,
		LatencyP95:          &latency,
		QuotaUnit:           "tokens",
		QuotaRemaining:      &remaining,
		QuotaConfidence:     0.8,
		QuotaKind:           "self_accounting",
		CostPerUnit:         &cost,
		CacheHitRate:        &hit,
		SampleCount:         42,
	}
}

// assertKeys 断言一个骨架对象的键集合**恰好**是这些。
//
// 逐字段扫而不是只挑几个键看：适配层多摊出一个键，就等于那个键的取值有可能
// 落库（前提是允许表里也有一行），而「多了一个键」这件事在只查关键几个键的
// 断言里完全看不见。
func assertKeys(t *testing.T, got map[string]any, want ...string) {
	t.Helper()
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if len(keys) != len(sorted) {
		t.Fatalf("键集合 = %v，期望 %v", keys, sorted)
	}
	for i := range keys {
		if keys[i] != sorted[i] {
			t.Fatalf("键集合 = %v，期望 %v", keys, sorted)
		}
	}
}

// assertNoMarker 在整份骨架里逐层找一个标记，找不到才算过。
//
// 只查某一个键的断言在字段挪到第二层之后就会静默失效，所以这里走全树。
func assertNoMarker(t *testing.T, v any, marker string) {
	t.Helper()
	if containsString(v, marker) {
		t.Errorf("骨架里出现了 %q：%#v", marker, v)
	}
}

// 决策请求方向摊出来的键集合。
//
// 它与 docs/request-retention.md 里那张清单、以及 internal/snapshot 的允许表
// 是同一件事的三处写法：这里钉住「适配层摊了什么」，那条用例钉住「文档与表
// 对得上」，而表钉住「摊出来的东西里哪些能落库」。三处缺一处，改动就会从
// 那个缺口溜进去。
func TestDecisionRequestMapKeys(t *testing.T) {
	m := decisionRequestMap(&gateway.DecisionTrace{
		Model:      "probe-model",
		Candidates: []gateway.DecisionCandidate{fullDecisionCandidate(7)},
	})
	assertKeys(t, m, "surface", "model", "candidates_count", "candidates")

	if got := m["surface"]; got != surfaceDecision {
		t.Errorf("surface = %v，期望 %q", got, surfaceDecision)
	}
	if got := m["candidates_count"]; got != 1 {
		t.Errorf("candidates_count = %v，期望 1", got)
	}
	cands, ok := m["candidates"].([]any)
	if !ok || len(cands) != 1 {
		t.Fatalf("candidates = %#v，期望一个元素", m["candidates"])
	}
	cand, ok := cands[0].(map[string]any)
	if !ok {
		t.Fatalf("候选不是对象: %#v", cands[0])
	}
	// 请求方向**不带** selected：那时还没有选中项，一个全 false 的字段
	// 只会让人以为「一个都没选上」。
	assertKeys(t, cand,
		"channel_id", "priority", "weight", "consecutive_failures", "success_rate",
		"latency_p95_ms", "quota_unit", "quota_remaining", "quota_confidence",
		"quota_kind", "cost_per_unit", "cache_hit_rate", "sample_count")
}

// 响应方向多两段：selected 与 attempts，候选上多一个 selected 布尔。
func TestDecisionResponseMapKeys(t *testing.T) {
	status := 503
	kind := "upstream_http_error"
	m := decisionResponseMap(&gateway.DecisionOutcome{
		Model:             "probe-model",
		Candidates:        []gateway.DecisionCandidate{fullDecisionCandidate(7)},
		Succeeded:         true,
		SelectedChannelID: 7,
		UpstreamModel:     "vendor/probe",
		Attempts: []gateway.DecisionAttempt{
			{ChannelID: 7, Succeeded: false, Retryable: true, UpstreamStatus: &status, ErrorKind: &kind},
		},
	})
	assertKeys(t, m, "surface", "model", "candidates_count", "candidates", "attempts", "selected")
	assertKeys(t, m["selected"].(map[string]any), "channel_id", "upstream_model")

	cand := m["candidates"].([]any)[0].(map[string]any)
	if got := cand["selected"]; got != true {
		t.Errorf("候选上的 selected = %v，期望 true（它就是被选中的那个）", got)
	}
	attempt := m["attempts"].([]any)[0].(map[string]any)
	assertKeys(t, attempt, "channel_id", "succeeded", "retryable", "duration_ms",
		"upstream_status", "error_kind")
}

// 没有选中项时 **selected 整段不写**，而不是写一个空对象。
//
// 空对象读起来像「选了一个没有名字的渠道」，而实际情况是全军覆没。
// 全部失败时 route.Result.Channel 记的是最后试过的那个（见 route 的注释），
// 照抄进 selected 会把一次失败读成一次成功选路。
func TestDecisionResponseWithoutSelectionHasNoSelectedBlock(t *testing.T) {
	m := decisionResponseMap(&gateway.DecisionOutcome{
		Candidates: []gateway.DecisionCandidate{fullDecisionCandidate(7)},
	})
	if _, ok := m["selected"]; ok {
		t.Errorf("没有成功选路时不该有 selected 段：%#v", m["selected"])
	}
	cand := m["candidates"].([]any)[0].(map[string]any)
	if got := cand["selected"]; got != false {
		t.Errorf("候选上的 selected = %v，期望 false", got)
	}
}

// 没有数据的项**整个键不写**，与「是 0」分开。
//
// 写成 0 会把「这个渠道没有成本数据」读成「它的成本是 0」——那正是路由那一侧
// 修过的一条缺陷（见 route.Candidate.CostPerUnit 的注释），快照不该重新引入。
func TestDecisionOmitsMissingValues(t *testing.T) {
	m := decisionRequestMap(&gateway.DecisionTrace{
		// 模型名为空、除 id 之外什么都不填。
		Candidates: []gateway.DecisionCandidate{{ChannelID: 7}},
	})
	if _, ok := m["model"]; ok {
		t.Error("模型名为空时不该写这个键：一个长度 0 的文本占位看起来像「这里被脱敏了」")
	}
	cand := m["candidates"].([]any)[0].(map[string]any)
	for _, key := range []string{
		"latency_p95_ms", "quota_unit", "quota_remaining", "quota_kind",
		"cost_per_unit", "cache_hit_rate",
	} {
		if _, ok := cand[key]; ok {
			t.Errorf("没有数据的 %s 不该出现在骨架里：%#v", key, cand[key])
		}
	}
	// 非指针的那些 0 是**数据**，留着。
	for _, key := range []string{"priority", "weight", "consecutive_failures", "sample_count"} {
		if _, ok := cand[key]; !ok {
			t.Errorf("%s 的 0 是一个正常取值，不该被省掉", key)
		}
	}
}

// 每一次尝试都要带「成没成」，而只有失败的那些才带「为什么」。
//
// 一次成功的尝试没有失败分类，写一个 retryable=false 进去会让人以为它失败过
// 但没重试——而降级原因正是靠这两格读出来的。
func TestDecisionAttemptsCarryReasonOnlyOnFailure(t *testing.T) {
	status := 503
	kind := "upstream_http_error"
	m := decisionResponseMap(&gateway.DecisionOutcome{
		Attempts: []gateway.DecisionAttempt{
			{ChannelID: 1, Succeeded: false, Retryable: true, UpstreamStatus: &status, ErrorKind: &kind},
			{ChannelID: 2, Succeeded: true},
		},
	})
	attempts := m["attempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("尝试数 = %d，期望 2", len(attempts))
	}
	failed := attempts[0].(map[string]any)
	if failed["retryable"] != true || failed["upstream_status"] != 503 ||
		failed["error_kind"] != "upstream_http_error" {
		t.Errorf("失败那一次的降级原因不全: %#v", failed)
	}
	ok := attempts[1].(map[string]any)
	assertKeys(t, ok, "channel_id", "succeeded", "duration_ms")
}

// ── 走真脱敏器的那一层 ──────────────────────────────────────────────────
//
// 上面几条钉的是「适配层摊了什么」，下面这几条把摊出来的东西真的交给
// 允许表过一遍：判据的取值要留下，且**一条都不该被脱敏**。
//
// 后者是个强断言：允许表里没有、或者档位配错时，那一格会变成描述符并出现在
// redacted_paths 里。空清单因此等于「这一份骨架里每一个键都被允许表认下来了」。

func newDecisionCollector(t *testing.T) *Collector {
	t.Helper()
	return newCollector(t, testConfig(), &fakeDB{})
}

func TestRedactDecisionRequestKeepsCriteria(t *testing.T) {
	c := newDecisionCollector(t)
	snap, err := c.RedactDecisionRequest(&gateway.DecisionTrace{
		Model:      "probe-model",
		Candidates: []gateway.DecisionCandidate{fullDecisionCandidate(7)},
	})
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("skeleton_version = %d，期望 1（这条链路此前不采，没有旧口径的行）", snap.Version)
	}
	if got := snap.Skeleton["surface"]; got != "decision" {
		t.Errorf("骨架里的 surface = %v，期望 decision", got)
	}
	if got := snap.Skeleton["model"]; got != "probe-model" {
		t.Errorf("model = %v，判据的取值应当留下", got)
	}
	cand := snap.Skeleton["candidates"].([]any)[0].(map[string]any)
	if got := cand["channel_id"]; got != int64(7) {
		t.Errorf("channel_id = %#v，期望 7", got)
	}
	if got := cand["latency_p95_ms"]; got == nil {
		t.Error("有数据的响应时间分位应当留下")
	}
	if len(snap.RedactedPaths) != 0 {
		t.Errorf("判据被脱敏了 %v——允许表里少了它，或者档位对不上它的取值",
			snap.RedactedPaths)
	}
}

func TestRedactDecisionResponseKeepsSelectionAndAttempts(t *testing.T) {
	c := newDecisionCollector(t)
	status := 503
	kind := "upstream_http_error"
	snap, err := c.RedactDecisionResponse(&gateway.DecisionOutcome{
		Model:             "probe-model",
		Candidates:        []gateway.DecisionCandidate{fullDecisionCandidate(7), fullDecisionCandidate(8)},
		Succeeded:         true,
		SelectedChannelID: 8,
		UpstreamModel:     "vendor/probe-model",
		Attempts: []gateway.DecisionAttempt{
			{ChannelID: 7, Succeeded: false, Retryable: true, UpstreamStatus: &status, ErrorKind: &kind},
			{ChannelID: 8, Succeeded: true},
		},
	})
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	selected := snap.Skeleton["selected"].(map[string]any)
	if selected["channel_id"] != int64(8) {
		t.Errorf("选中的渠道 = %#v，期望 8", selected["channel_id"])
	}
	if selected["upstream_model"] != "vendor/probe-model" {
		t.Errorf("上游模型名 = %#v", selected["upstream_model"])
	}
	cands := snap.Skeleton["candidates"].([]any)
	if cands[0].(map[string]any)["selected"] != false {
		t.Error("第一个候选没被选中，selected 该是 false")
	}
	if cands[1].(map[string]any)["selected"] != true {
		t.Error("第二个候选被选中了，selected 该是 true")
	}
	attempts := snap.Skeleton["attempts"].([]any)
	if attempts[0].(map[string]any)["error_kind"] != "upstream_http_error" {
		t.Errorf("失败分类没留下: %#v", attempts[0])
	}
	if len(snap.RedactedPaths) != 0 {
		t.Errorf("判据被脱敏了 %v", snap.RedactedPaths)
	}
}

// 这一份骨架里只可能出现判据：**内容那一侧的字符串一个都到不了这里**。
//
// 这一条不是「脱敏器挡住了内容」，而是「内容根本没进来」：DecisionTrace /
// DecisionOutcome 上没有 state / questions / answers 的字段，候选也是
// DecisionCandidate（没有 channel.Channel 的凭证与地址）。用例把这些标记
// 放在别处能放的地方，断言它们不出现在骨架里。
func TestRedactDecisionCarriesNoContent(t *testing.T) {
	c := newDecisionCollector(t)
	const marker = "工单: 用户说付款后没到账"

	snap, err := c.RedactDecisionRequest(&gateway.DecisionTrace{
		Model:      "probe-model",
		Candidates: []gateway.DecisionCandidate{fullDecisionCandidate(7)},
	})
	if err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}
	assertNoMarker(t, snap.Skeleton, marker)
	assertNoMarker(t, snap.Skeleton, "这是账单问题吗")
	assertNoMarker(t, snap.Skeleton, "placeholder")
}
