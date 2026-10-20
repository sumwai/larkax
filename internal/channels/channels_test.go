package channels

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/route"
)

// testKey 造一把 32 字节的测试主密钥。
func testKey(id string, fill byte) config.MasterKey {
	k := make([]byte, config.KeySize)
	for i := range k {
		k[i] = fill
	}
	return config.MasterKey{ID: id, Key: k}
}

func mustSealer(t *testing.T, keys []config.MasterKey) *credential.Sealer {
	t.Helper()
	s, err := credential.NewSealer(keys)
	if err != nil {
		t.Fatalf("NewSealer 出错: %v", err)
	}
	return s
}

// ── 凭证解密 ────────────────────────────────────────────────────────────

// 明文的往返是这一层唯一真正接上 internal/credential 的地方，
// 走通它才说明「取出来的凭证能交给适配器」。
func TestDecryptRoundTrip(t *testing.T) {
	sealer := mustSealer(t, []config.MasterKey{testKey("current", 0x11)})
	src := New(nil, sealer)

	const plaintext = "test-key-placeholder"
	envelope, err := sealer.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("Seal 出错: %v", err)
	}

	got, err := src.decrypt(envelope)
	if err != nil {
		t.Fatalf("decrypt 出错: %v", err)
	}
	if got != plaintext {
		t.Errorf("decrypt = %q，期望 %q", got, plaintext)
	}
}

// 没有主密钥是**部署问题**，错误必须指向它，而不是笼统的「解不开」。
func TestDecryptWithoutSealerReportsMissingKey(t *testing.T) {
	src := New(nil, nil)
	_, err := src.decrypt([]byte{0x01, 0x01})
	if !errors.Is(err, ErrNoMasterKey) {
		t.Errorf("err = %v，期望 ErrNoMasterKey", err)
	}
	if errors.Is(err, ErrCredentialCorrupt) {
		t.Error("没配主密钥不该被报成密文损坏")
	}
}

// 密文是用别的钥匙包的：同样是缺钥匙，不是数据坏了。
func TestDecryptWithWrongKeyReportsMissingKey(t *testing.T) {
	writer := mustSealer(t, []config.MasterKey{testKey("other", 0x22)})
	envelope, err := writer.Seal([]byte("test-key-placeholder"))
	if err != nil {
		t.Fatalf("Seal 出错: %v", err)
	}

	src := New(nil, mustSealer(t, []config.MasterKey{testKey("current", 0x11)}))
	_, err = src.decrypt(envelope)
	if !errors.Is(err, ErrNoMasterKey) {
		t.Errorf("err = %v，期望 ErrNoMasterKey", err)
	}
}

// 密文本身不合法是**数据问题**，与缺钥匙分开报。
func TestDecryptMalformedReportsCorrupt(t *testing.T) {
	src := New(nil, mustSealer(t, []config.MasterKey{testKey("current", 0x11)}))
	for name, envelope := range map[string][]byte{
		"太短":     {0x01},
		"版本未知":   {0x99, 0x01},
		"副本数为零":  {0x01, 0x00},
		"被截断的正文": {0x01, 0x01, 0x01, 'k', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := src.decrypt(envelope)
			if !errors.Is(err, ErrCredentialCorrupt) {
				t.Errorf("err = %v，期望 ErrCredentialCorrupt", err)
			}
			if errors.Is(err, ErrNoMasterKey) {
				t.Error("密文损坏不该被报成缺主密钥")
			}
		})
	}
}

// 聚合错误要能穿到内层，否则调用方只能看到一个笼统的「都解不开」。
func TestDecryptErrorsUnwrap(t *testing.T) {
	e := &DecryptErrors{Failures: []DecryptFailure{
		{ChannelID: 1, Name: "a", Err: ErrNoMasterKey},
		{ChannelID: 2, Name: "b", Err: ErrCredentialCorrupt},
	}}
	if !errors.Is(e, ErrNoMasterKey) {
		t.Error("errors.Is(ErrNoMasterKey) 应为真")
	}
	if !errors.Is(e, ErrCredentialCorrupt) {
		t.Error("errors.Is(ErrCredentialCorrupt) 应为真")
	}
	if e.Error() == "" {
		t.Error("Error() 不应为空")
	}
}

// 有失败项时，报文必须点出第一个是谁、为什么：笼统的「都解不开」会把排查
// 引向「主密钥配了吗」，而真正坏的往往只是某一条渠道的密文。
//
// 没有失败项时退回笼统文案，不去索引空切片——这个聚合类型是为「有失败项」
// 造的，但 Error() 本身不该在空值上崩掉。
func TestDecryptErrorsMessage(t *testing.T) {
	one := &DecryptErrors{Failures: []DecryptFailure{
		{ChannelID: 7, Name: "ch-7", Err: ErrCredentialCorrupt},
	}}
	for _, want := range []string{"1 个候选渠道", "渠道 7", "ch-7", ErrCredentialCorrupt.Error()} {
		if !strings.Contains(one.Error(), want) {
			t.Errorf("Error() = %q，期望包含 %q", one.Error(), want)
		}
	}

	if got, want := (&DecryptErrors{}).Error(), "候选渠道的凭证都解不开"; got != want {
		t.Errorf("没有失败项时 Error() = %q，期望 %q", got, want)
	}
}

// ── 健康度 ──────────────────────────────────────────────────────────────

// 新渠道还没有任何健康数据。此时必须表现为「未知」——
// 填成一个「很差」的值会让它永远选不上。
func TestHealthUnknownWhenNoRollup(t *testing.T) {
	var row candidateRow
	got := row.health(time.Now())

	if got.SuccessRate != 0 || got.LatencyP95 != nil || got.ConsecutiveFailures != 0 {
		t.Errorf("没有数据时应是零值（未知），实际 SuccessRate=%v LatencyP95=%s ConsecutiveFailures=%d",
			got.SuccessRate, showPtr(got.LatencyP95), got.ConsecutiveFailures)
	}
}

// 有数据时用窗口内的汇总：成功率与最新的 p95。
func TestHealthFromRollup(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		requests: 10, successes: 8,
		latencyP95: sqlNullInt(250),
	}

	got := row.health(now)
	if got.SuccessRate != 0.8 {
		t.Errorf("SuccessRate = %v，期望 0.8", got.SuccessRate)
	}
	if got.LatencyP95 == nil || *got.LatencyP95 != 250*time.Millisecond {
		t.Errorf("LatencyP95 = %s，期望 250ms", showPtr(got.LatencyP95))
	}
}

// 连续失败取自 channels 的列：它是一个逐次维护的真计数，不是从桶里估的。
func TestConsecutiveFailuresFromColumn(t *testing.T) {
	now := time.Now()
	row := candidateRow{streak: 5, lastFailureAt: sqlNullTime(now.Add(-5 * time.Minute))}

	if got := row.consecutiveFailures(now); got != 5 {
		t.Errorf("consecutiveFailures = %d，期望 5", got)
	}
}

// 计数为零就是零，时间戳无所谓——成功清零之后它不该再被判成失败。
func TestConsecutiveFailuresZeroWithoutStreak(t *testing.T) {
	now := time.Now()
	row := candidateRow{lastFailureAt: sqlNullTime(now.Add(-time.Minute))}

	if got := row.consecutiveFailures(now); got != 0 {
		t.Errorf("consecutiveFailures = %d，期望 0", got)
	}
}

// 冷却期是这条判据唯一的出口：被摘掉的渠道没有流量，计数不会自己降回去。
//
// 两个时间点都写字面量而不是引用 streakCooldown——用常量当输入的话，
// 把阈值从 15 分钟改成 15 秒，用例照样通过（见 LESSONS.md 第 17 条）。
func TestConsecutiveFailuresExpiresAfterCooldown(t *testing.T) {
	now := time.Now()

	fresh := candidateRow{streak: 5, lastFailureAt: sqlNullTime(now.Add(-14 * time.Minute))}
	if got := fresh.consecutiveFailures(now); got != 5 {
		t.Errorf("14 分钟前失败过：consecutiveFailures = %d，期望 5（冷却期是 15 分钟）", got)
	}

	stale := candidateRow{streak: 5, lastFailureAt: sqlNullTime(now.Add(-16 * time.Minute))}
	if got := stale.consecutiveFailures(now); got != 0 {
		t.Errorf("16 分钟前失败过：consecutiveFailures = %d，期望 0（冷却期已过，放它进来试一次）", got)
	}
}

// 计数非零但没有时间戳：按「冷却期已过」处理。
//
// 写入方总是把两列一起写，所以这种行只可能来自手工写入。两个方向不对称：
// 按已过处理最多是少摘一次，按新鲜处理会让渠道永远摘不掉——而那正是
// 这一列要消除的状态。
func TestConsecutiveFailuresWithoutTimestampIsTreatedAsExpired(t *testing.T) {
	row := candidateRow{streak: 5}

	if got := row.consecutiveFailures(time.Now()); got != 0 {
		t.Errorf("没有时间戳时 consecutiveFailures = %d，期望 0", got)
	}
}

// 恰好等于冷却期就算「已过」：边界落在放行这一侧，而不是再压一个周期。
//
// 这一条按 streakCooldown 构造输入，与上面两条用字面量不同：它管的是比较符
// 的边界（`>=` 写成 `>` 时这一条才会红），时长本身由 14 / 16 分钟那两条负责。
func TestConsecutiveFailuresAtExactlyCooldown(t *testing.T) {
	now := time.Now()
	row := candidateRow{streak: 5, lastFailureAt: sqlNullTime(now.Add(-streakCooldown))}

	if got := row.consecutiveFailures(now); got != 0 {
		t.Errorf("恰好 %v 前失败过：consecutiveFailures = %d，期望 0", streakCooldown, got)
	}
}

// ── 变异测试的等价体 ────────────────────────────────────────────────────
//
// 这一段不是用例，是给变异测试留的说明：channels.go 的 consecutiveFailures
// 里那处 `row.streak <= 0`（第 655 行）被改成 `<` 后没有用例变红，但两版
// **在任何输入下同值**，所以区分不出：
//
//   - streak > 0：都不进这一支，往下走同一段逻辑；
//   - streak < 0：都进这一支，都返回 0；
//   - streak == 0：唯一有差异的输入——原文直接返回 0，改写后继续往下，但无论
//     走哪个出口，最后返回的都是 row.streak，也就是 0。
//
// 想靠「换一个输入」杀掉它是不可能的；上面三条用例覆盖的是这条函数真正会
// 分叉的输入（零、未过期、已过期）。

// ── 额度 ────────────────────────────────────────────────────────────────

// 没有额度行、或 remaining_estimate 为 NULL，都是「查不到额度」。
// 两者都不能变成「额度是 0」——route 会把 0 当成耗尽并排除该渠道。
func TestQuotaUnknown(t *testing.T) {
	cases := map[string]candidateRow{
		"没有额度行": {remaining: sql.NullString{}},
		"额度为 NULL": {
			remaining: sql.NullString{},
			probeKind: sql.NullString{String: "api_query", Valid: true},
		},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			q, err := row.quota()
			if err != nil {
				t.Fatalf("quota 出错: %v", err)
			}
			if q.Remaining != nil {
				t.Errorf("Remaining = %s，期望 nil（未知）；实际 %+v", showPtr(q.Remaining), q)
			}
			// 没有数值就既不是实测也不是推算：Estimated 说的是「这个数
			// 是怎么来的」，而这里根本没有数。
			if q.Estimated {
				t.Errorf("没有数值的状态不该标成推算值；实际 %+v", q)
			}
		})
	}
}

// 有值就是已知，且要区分实测与推算。
func TestQuotaKnownAndEstimated(t *testing.T) {
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	row := candidateRow{
		remaining: sql.NullString{String: "12.5", Valid: true},
		probeKind: sql.NullString{String: probeKindAPIQuery, Valid: true},
		resetsAt:  sql.NullTime{Time: expires, Valid: true},
	}

	q, err := row.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Remaining == nil || *q.Remaining != 12.5 {
		t.Errorf("quota = %+v，期望有值且为 12.5", q)
	}
	if q.Estimated {
		t.Error("api_query 是上游实测，不该标成推算值")
	}
	if q.ResetsAt == nil || !q.ResetsAt.Equal(expires) {
		t.Errorf("ResetsAt = %v，期望 %v", q.ResetsAt, expires)
	}
}

// 单位要原样带出去：Remaining 是个裸数字，没有单位 route 就不知道它能不能
// 与别的候选比较（见 route 的 scoreQuota）。
func TestQuotaCarriesUnit(t *testing.T) {
	row := candidateRow{
		remaining: sql.NullString{String: "12.5", Valid: true},
		unit:      sql.NullString{String: "tokens", Valid: true},
	}
	q, err := row.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Unit != "tokens" {
		t.Errorf("Unit = %q，期望 tokens", q.Unit)
	}

	// unit 为 NULL 时是空字符串，**不是**某个默认单位：读侧与 route 都把
	// 空单位当成「不知道按什么计」，它与任何已知单位都不可比。
	noUnit := candidateRow{remaining: sql.NullString{String: "12.5", Valid: true}}
	q, err = noUnit.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Unit != "" {
		t.Errorf("unit 为 NULL 时 Unit = %q，期望空字符串", q.Unit)
	}
}

// 可信度要原样带出去，因为它决定 route 怎么给这一项打分。
//
// 这一列早先没有进 State（issue #233）。漏掉的后果不是报错，而是 route 那边
// 每个候选的额度项都退成中性分——排序照旧排得出来，只是与额度无关。
func TestQuotaCarriesConfidence(t *testing.T) {
	row := candidateRow{
		remaining:  sql.NullString{String: "12.5", Valid: true},
		probeKind:  sql.NullString{String: "self_accounting", Valid: true},
		confidence: sql.NullString{String: "0.40", Valid: true},
	}
	q, err := row.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	// numeric(3,2) 落库时是 "0.40"，读回来是 0.4。
	if q.Confidence != 0.4 {
		t.Errorf("Confidence = %v，期望 0.4", q.Confidence)
	}

	// 没有数值就没有可信度可言：置信度描述的是「这个余量有多可信」。
	// 额度行为 NULL 时（LEFT JOIN 没连上）不该凭空带出一个 0 之外的值，
	// 但也不该让整行读失败。
	noValue := candidateRow{
		probeKind:  sql.NullString{String: "api_query", Valid: true},
		confidence: sql.NullString{String: "1.00", Valid: true},
	}
	q, err = noValue.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Remaining != nil {
		t.Fatalf("Remaining = %v，期望 nil", q.Remaining)
	}
	if q.Confidence != 0 {
		t.Errorf("没有余量时 Confidence = %v，期望 0（可信度跟着余量走）", q.Confidence)
	}
}

// confidence 不是数字说明这一行的类型与表定义对不上（列是 numeric(3,2)
// NOT NULL），与 remaining_estimate 同样处置：报错，不静默当成 0。
// 静默的后果是那个渠道的额度项悄悄变成「不可信」，而库里那一行看起来正常。
func TestQuotaRejectsMalformedConfidence(t *testing.T) {
	row := candidateRow{
		remaining:  sql.NullString{String: "12.5", Valid: true},
		confidence: sql.NullString{String: "不是数字", Valid: true},
	}
	if _, err := row.quota(); err == nil {
		t.Fatal("confidence 不是数字时应当报错")
	}
}

// confidence 是空字符串时按「没有可信度」处理，不是「解析失败」。
//
// 空字符串不是库里的形态：这一列是 numeric(3,2) NOT NULL，扫描出来要么是数字，
// 要么是 NULL（LEFT JOIN 没连上）。它挡住的是直接构造出来的 candidateRow——
// 本包内拿构造值调用 candidateRow.quota / withValue 的路径。少了 `!= ""`，
// 那一格会掉进 ParseFloat 的错误分支，把「这一格没填」报成「这一行的类型与
// 表定义对不上」。两种口径不冲突：库里不会有空串，函数级输入可以有。
func TestQuotaEmptyConfidenceIsAbsent(t *testing.T) {
	row := candidateRow{
		remaining:  sql.NullString{String: "12.5", Valid: true},
		confidence: sql.NullString{String: "", Valid: true},
	}
	q, err := row.quota()
	if err != nil {
		t.Fatalf("confidence 为空时不该报错，得到 %v", err)
	}
	if q.Confidence != 0 {
		t.Errorf("Confidence = %v，期望 0", q.Confidence)
	}
}

// 自记账与「从错误里推断」都是推算，插件契约会对它们打折。
//
// 注意这条判据与 internal/quota 的 Merge 对同一情形**不一致**：那边
// error_observed 带数值时置 Estimated = false，这边按 probe_kind 推出 true，
// 而 Estimated 并不落库（表里没有这一列），所以同一个状态在内存里与从库里
// 读回来会不一样。本包以库里那一列为准；两处要统一是另一件事。
func TestQuotaSelfAccountingIsEstimated(t *testing.T) {
	for _, kind := range []string{"self_accounting", "error_observed"} {
		t.Run(kind, func(t *testing.T) {
			row := candidateRow{
				remaining: sql.NullString{String: "3", Valid: true},
				probeKind: sql.NullString{String: kind, Valid: true},
			}
			q, err := row.quota()
			if err != nil {
				t.Fatalf("quota 出错: %v", err)
			}
			if !q.Estimated {
				t.Errorf("probe_kind=%q 应标为推算值", kind)
			}
		})
	}
}

// 上游明说 drained 时，即使 remaining_estimate 是 NULL 也要映射成
// 「已知且为 0」：它是有信号的一态，不是「查不到」。
//
// 这条用例钉住的是**判断顺序**——把 drained 的分支放到 NULL 判断之后，
// 这一行会落进「未知」，route 照旧会选它。改回旧映射时它必须变红。
func TestQuotaDrainedWithNullRemainingIsZero(t *testing.T) {
	row := candidateRow{
		remaining:   sql.NullString{},
		quotaHealth: sql.NullString{String: quotaHealthDrained, Valid: true},
	}

	q, err := row.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Remaining == nil || *q.Remaining != 0 {
		t.Fatalf("quota = %+v，期望有值且为 0", q)
	}

	// 这条映射存在的唯一目的就是让 route 排除它，所以直接断言结果。
	if _, reason := (&route.Candidate{Quota: q}).DrainReason(time.Time{}, 0); reason == "" {
		t.Error("drained 的渠道应被 DrainReason 排除")
	}
}

// 其余三态仍按原映射走。degraded 是「不太顺」，不是「用完了」，
// 把它也当成 0 会让渠道被无谓地排除。
func TestQuotaOtherHealthsKeepRemaining(t *testing.T) {
	for _, health := range []string{"healthy", "degraded", "unknown"} {
		t.Run(health, func(t *testing.T) {
			row := candidateRow{
				remaining:   sql.NullString{String: "4", Valid: true},
				quotaHealth: sql.NullString{String: health, Valid: true},
			}
			q, err := row.quota()
			if err != nil {
				t.Fatalf("quota 出错: %v", err)
			}
			if q.Remaining == nil || *q.Remaining != 4 {
				t.Errorf("quota = %+v，期望有值且为 4", q)
			}
			if _, reason := (&route.Candidate{Quota: q}).DrainReason(time.Time{}, 0); reason != "" {
				t.Errorf("health=%q 不该被排除：%s", health, reason)
			}
		})
	}

	// 没有额度行（也就没有 health）时仍是未知。
	var empty candidateRow
	q, err := empty.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Remaining != nil {
		t.Errorf("没有额度行时应是未知，实际 %+v", q)
	}
}

// 剩余量为 0 与「查不到」必须分开：前者 route 会排除该渠道。
func TestQuotaZeroIsKnown(t *testing.T) {
	row := candidateRow{remaining: sql.NullString{String: "0", Valid: true}}
	q, err := row.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Remaining == nil || *q.Remaining != 0 {
		t.Errorf("quota = %+v，期望有值且为 0", q)
	}
}

// 观测痕迹要原样带出去。
//
// 「余量未知」有两种：从来没观测过（上游不支持查询额度），与观测过却没
// 拿到数值（探针跑了没结果、或只从错误里看到信号）。两者在路由侧是相反的
// 结论——前者照旧参与，后者不足以支撑决策——而区分它们靠的就是这两列。
// 早先这两列没有进 State，判据在路由侧退化成「有没有数值」。
func TestQuotaCarriesObservationTraces(t *testing.T) {
	probed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := candidateRow{
		probeKind:     sql.NullString{String: probeKindAPIQuery, Valid: true},
		lastProbeAt:   sql.NullTime{Time: probed, Valid: true},
		lastErrorCode: sql.NullString{String: "402", Valid: true},
	}

	q, err := row.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if q.Remaining != nil {
		t.Errorf("Remaining = %s，期望 nil", showPtr(q.Remaining))
	}
	if q.LastProbeAt == nil || !q.LastProbeAt.Equal(probed) {
		t.Errorf("LastProbeAt = %v，期望 %v", q.LastProbeAt, probed)
	}
	if q.LastErrorCode != "402" {
		t.Errorf("LastErrorCode = %q，期望 402", q.LastErrorCode)
	}

	// 这一行在 route 眼里是不可路由的：观测过，却没有可用信息。
	// 直接断言结果，因为「带出这两列」的唯一目的就是这个。
	if _, reason := (&route.Candidate{Quota: q}).DrainReason(time.Time{}, 0); reason == "" {
		t.Error("观测过却没有数值的渠道应被 DrainReason 排除")
	}
}

// ── 成本与缓存 ──────────────────────────────────────────────────────────

// 表里只有总额与请求数，所以单位成本是「每次请求的平均成本」。
//
// 没有请求、没有成本合计、成本合计为 0 三种情况都返回 **nil**（没有数据），
// 而不是 0：0 在 route 眼里是「免费」，而一个从没记过成本的渠道不是免费。
func TestCostPerUnit(t *testing.T) {
	cases := []struct {
		name string
		row  candidateRow
		want *float64
	}{
		{"没有请求", candidateRow{cost: sqlNullString("1.5")}, nil},
		{"没有成本列（窗口内没有桶）", candidateRow{requests: 10}, nil},
		{"成本合计为 0（写入侧从没写过）", candidateRow{requests: 10, cost: sqlNullString("0")}, nil},
		{"平均值", candidateRow{requests: 10, cost: sqlNullString("1.5")}, ptr(0.15)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.row.costPerUnit()
			if err != nil {
				t.Fatalf("costPerUnit 出错: %v", err)
			}
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("costPerUnit = %s，期望 %s", showPtr(got), showPtr(tc.want))
			}
			if got != nil && *got != *tc.want {
				t.Errorf("costPerUnit = %v，期望 %v", *got, *tc.want)
			}
		})
	}
}

// 成本合计不是数字是**数据问题**，要整次调用报错，不静默当成「没有数据」。
func TestCostPerUnitRejectsNonNumeric(t *testing.T) {
	row := candidateRow{id: 7, requests: 10, cost: sqlNullString("abc")}
	if _, err := row.costPerUnit(); err == nil {
		t.Error("cost 不是数字时应当报错")
	}
}

// 不报缓存明细的渠道两个计数都是 0（或没有桶、两列都是 NULL）：
// 命中率返回 nil，样本数为 0，route 把这一维整个剔除，
// 而不是把它当成「命中率 0」。
func TestCacheUnknownWhenNoTokens(t *testing.T) {
	var row candidateRow
	if got := row.cacheHitRate(); got != nil {
		t.Errorf("cacheHitRate = %s，期望 nil", showPtr(got))
	}
	if row.cacheSamples() != 0 {
		t.Error("没有缓存明细时样本数应为 0")
	}
}

// 命中率**恰好为 0** 是有数据的：有样本、且一次都没命中。
// 它必须与「没有数据」分得开，否则缓存最差的渠道会被当成「这一维不适用」。
func TestCacheHitRate(t *testing.T) {
	row := candidateRow{cacheHit: sqlNullInt(75), cacheMiss: sqlNullInt(25)}
	got := row.cacheHitRate()
	if got == nil || *got != 0.75 {
		t.Errorf("cacheHitRate = %s，期望 0.75", showPtr(got))
	}
	if row.cacheSamples() != 100 {
		t.Errorf("样本数 = %d，期望 100", row.cacheSamples())
	}

	none := candidateRow{cacheHit: sqlNullInt(0), cacheMiss: sqlNullInt(100)}
	if got := none.cacheHitRate(); got == nil || *got != 0 {
		t.Errorf("一次都没命中时 cacheHitRate = %s，期望 0（而不是 nil）", showPtr(got))
	}
}

// ── 整行映射 ────────────────────────────────────────────────────────────

func TestCandidateMapsChannelFields(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		id: 7, name: "示例渠道", channelType: "openai-compatible",
		baseURL: "https://api.example.com/v1",
		models:  []string{"model-a", "model-b"},
		setting: []byte(`{"supports_tools":true}`),
		weight:  3, priority: 5, ownerUserID: 42,
	}

	got, err := row.candidate("test-key-placeholder", now)
	if err != nil {
		t.Fatalf("candidate 出错: %v", err)
	}

	if got.Channel.ID != 7 || got.Channel.Name != "示例渠道" ||
		got.Channel.Type != "openai-compatible" ||
		got.Channel.BaseURL != "https://api.example.com/v1" {
		t.Errorf("Channel 映射不完整: %+v", got.Channel)
	}
	if got.Channel.Credential != "test-key-placeholder" {
		t.Errorf("Credential = %q，期望明文凭证", got.Channel.Credential)
	}
	if !got.Channel.Supports("model-a") {
		t.Error("Models 未映射")
	}
	if got.Channel.Setting["supports_tools"] != true {
		t.Errorf("Setting = %v，期望解开 jsonb", got.Channel.Setting)
	}
	// 结算要按这个字段匹配商家级规则，且必须与预留同一个 owner。
	if got.OwnerUserID != 42 {
		t.Errorf("OwnerUserID = %d，期望 42", got.OwnerUserID)
	}
	if got.Priority != 5 || got.Weight != 3 {
		t.Errorf("Priority/Weight = %d/%d，期望 5/3", got.Priority, got.Weight)
	}

	// 这一行通过了 route 的硬性条件，说明映射出来的候选确实可选。
	if ok, reason := got.Eligible(); !ok {
		t.Errorf("Eligible 为假：%s", reason)
	}
}

// setting 为空（理论上不会，列是 NOT NULL）时给一个空 map，而不是让
// 整次取候选失败。
func TestDecodeSettingEmpty(t *testing.T) {
	got, err := decodeSetting(nil)
	if err != nil {
		t.Fatalf("decodeSetting 出错: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("decodeSetting(nil) = %v，期望空 map", got)
	}

	if _, err := decodeSetting([]byte("不是 JSON")); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// 候选要能当 route.Candidate 用：这里做一次最小的端到端，
// 确认映射出来的东西真的进得了选路。
func TestCandidateSelectable(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		id: 1, name: "c", channelType: "openai-compatible",
		baseURL: "https://api.example.com/v1",
		models:  []string{"model-a"},
	}
	cand, err := row.candidate("test-key-placeholder", now)
	if err != nil {
		t.Fatalf("candidate 出错: %v", err)
	}

	plan := (&route.Router{}).Select([]route.Candidate{cand}, "model-a", route.Needs{})
	if len(plan.Candidates) != 1 {
		t.Errorf("选路结果 %d 个，期望 1；排除原因 %+v", len(plan.Candidates), plan.Excluded)
	}
}

// 候选上的端点由这条渠道的 type 指向的协议算出来。
//
// 它是选路那侧端点评据的一半输入（另一半是目录里的声明）：算错了会让一条
// 本来配好的渠道被判成「端点对不上」——而现场看到的是「没有可用的渠道」，
// 与「这些渠道都挂了」分不开。
func TestCandidateEndpointFromChannelType(t *testing.T) {
	cases := map[string]channel.Endpoint{
		"openai":           channel.EndpointChatCompletions,
		"anthropic":        channel.EndpointMessages,
		"openai-responses": channel.EndpointResponses,
		"systemone":        channel.EndpointSystemOne,
		// 库里写着当前版本的二进制不认识的类型：空串 = 不知道。端点判据
		// 不判它，而这条候选本来就选不中（执行器会报 ErrUnknownType）。
		"some-vendor": "",
	}
	for channelType, want := range cases {
		t.Run(channelType, func(t *testing.T) {
			row := candidateRow{
				id: 1, name: "c", channelType: channelType,
				baseURL: "https://api.example.com/v1",
				models:  []string{"model-a"},
			}
			got, err := row.candidate("test-key-placeholder", time.Now())
			if err != nil {
				t.Fatalf("candidate 出错: %v", err)
			}
			if got.Endpoint != want {
				t.Errorf("Endpoint = %q，期望 %q", got.Endpoint, want)
			}
		})
	}
}

func sqlNullInt(v int64) sql.NullInt64      { return sql.NullInt64{Int64: v, Valid: true} }
func sqlNullTime(v time.Time) sql.NullTime  { return sql.NullTime{Time: v, Valid: true} }
func sqlNullString(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
func ptr[T any](v T) *T                     { return &v }

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
