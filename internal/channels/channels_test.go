package channels

import (
	"database/sql"
	"errors"
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

// ── 健康度 ──────────────────────────────────────────────────────────────

// 新渠道还没有任何健康数据。此时必须表现为「未知」——
// 填成一个「很差」的值会让它永远选不上。
func TestHealthUnknownWhenNoRollup(t *testing.T) {
	var row candidateRow
	got := row.health(time.Now())

	if got.SuccessRate != 0 || got.LatencyP95 != 0 || got.ConsecutiveFailures != 0 {
		t.Errorf("没有数据时应全为零值（未知），实际 %+v", got)
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
	if got.LatencyP95 != 250*time.Millisecond {
		t.Errorf("LatencyP95 = %v，期望 250ms", got.LatencyP95)
	}
}

// 最新一个桶全部失败：连续失败数取这一桶的失败数（此时是精确值）。
func TestConsecutiveFailuresFromFailedLatestBucket(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		latestBucket:  sqlNullTime(now.Add(-30 * time.Minute)),
		latestReq:     sqlNullInt(5),
		latestSuccess: sqlNullInt(0),
		latestFailure: sqlNullInt(5),
	}
	if got := row.consecutiveFailures(now); got != 5 {
		t.Errorf("consecutiveFailures = %d，期望 5", got)
	}
}

// 最新桶里有过成功：连续长度不可知，返回 0 而不是猜一个。
func TestConsecutiveFailuresZeroWhenLatestBucketHadSuccess(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		latestBucket:  sqlNullTime(now.Add(-30 * time.Minute)),
		latestReq:     sqlNullInt(9),
		latestSuccess: sqlNullInt(1),
		latestFailure: sqlNullInt(8),
	}
	if got := row.consecutiveFailures(now); got != 0 {
		t.Errorf("consecutiveFailures = %d，期望 0（桶内有过成功，连续长度不可知）", got)
	}
}

// 数据太旧就不代表「当下」。不加这道判断会形成死循环：
// 失败 → 被摘掉 → 没有流量 → 数据不更新 → 永远摘不掉。
func TestConsecutiveFailuresZeroWhenStale(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		latestBucket:  sqlNullTime(now.Add(-6 * time.Hour)),
		latestReq:     sqlNullInt(5),
		latestSuccess: sqlNullInt(0),
		latestFailure: sqlNullInt(5),
	}
	if got := row.consecutiveFailures(now); got != 0 {
		t.Errorf("consecutiveFailures = %d，期望 0（数据已过期）", got)
	}
}

// 空桶（有桶行但没有请求）不算失败。
func TestConsecutiveFailuresZeroWhenLatestBucketEmpty(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		latestBucket:  sqlNullTime(now.Add(-10 * time.Minute)),
		latestReq:     sqlNullInt(0),
		latestSuccess: sqlNullInt(0),
		latestFailure: sqlNullInt(0),
	}
	if got := row.consecutiveFailures(now); got != 0 {
		t.Errorf("consecutiveFailures = %d，期望 0", got)
	}
}

// ── 额度 ────────────────────────────────────────────────────────────────

// 没有额度行、或 remaining_estimate 为 NULL，都是「查不到额度」。
// 两者都不能变成「额度是 0」——route 会把 0 当成耗尽并永久排除该渠道。
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
			if q.Known {
				t.Errorf("Known = true，期望 false（未知）；实际 %+v", q)
			}
			if q.Remaining != 0 {
				t.Errorf("Remaining = %v，期望零值", q.Remaining)
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
	if !q.Known || q.Remaining != 12.5 {
		t.Errorf("quota = %+v，期望已知且为 12.5", q)
	}
	if q.Estimated {
		t.Error("api_query 是上游实测，不该标成推算值")
	}
	if q.ExpiresAt == nil || !q.ExpiresAt.Equal(expires) {
		t.Errorf("ExpiresAt = %v，期望 %v", q.ExpiresAt, expires)
	}
}

// 自记账与「从错误里推断」都是推算，route 会对它们打折。
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
func TestQuotaDrainedWithNullRemainingIsKnownZero(t *testing.T) {
	row := candidateRow{
		remaining:   sql.NullString{},
		quotaHealth: sql.NullString{String: quotaHealthDrained, Valid: true},
	}

	q, err := row.quota()
	if err != nil {
		t.Fatalf("quota 出错: %v", err)
	}
	if !q.Known || q.Remaining != 0 {
		t.Fatalf("quota = %+v，期望 Known=true、Remaining=0", q)
	}

	// 这条映射存在的唯一目的就是让 route 排除它，所以直接断言结果。
	if reason := (&route.Candidate{Quota: q}).DrainReason(); reason == "" {
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
			if !q.Known || q.Remaining != 4 {
				t.Errorf("quota = %+v，期望已知且为 4", q)
			}
			if reason := (&route.Candidate{Quota: q}).DrainReason(); reason != "" {
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
	if q.Known {
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
	if !q.Known || q.Remaining != 0 {
		t.Errorf("quota = %+v，期望已知且为 0", q)
	}
}

// ── 成本与缓存 ──────────────────────────────────────────────────────────

// 表里只有总额与请求数，所以单位成本是「每次请求的平均成本」。
// 没有请求时是未知（0），不是免费。
func TestCostPerUnit(t *testing.T) {
	cases := []struct {
		name string
		row  candidateRow
		want float64
	}{
		{"没有请求", candidateRow{cost: "1.5"}, 0},
		{"没有成本", candidateRow{requests: 10, cost: "0"}, 0},
		{"平均值", candidateRow{requests: 10, cost: "1.5"}, 0.15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.row.costPerUnit()
			if err != nil {
				t.Fatalf("costPerUnit 出错: %v", err)
			}
			if got != tc.want {
				t.Errorf("costPerUnit = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 不报缓存明细的渠道两个计数都是 0：命中率与样本数都留 0，
// route 据此给中性分，而不是把它当成「命中率 0」。
func TestCacheUnknownWhenNoTokens(t *testing.T) {
	var row candidateRow
	if got := row.cacheHitRate(); got != 0 {
		t.Errorf("cacheHitRate = %v，期望 0", got)
	}
	if row.cacheHit+row.cacheMiss != 0 {
		t.Error("没有缓存明细时样本数应为 0")
	}
}

func TestCacheHitRate(t *testing.T) {
	row := candidateRow{cacheHit: 75, cacheMiss: 25}
	if got := row.cacheHitRate(); got != 0.75 {
		t.Errorf("cacheHitRate = %v，期望 0.75", got)
	}
	if row.cacheHit+row.cacheMiss != 100 {
		t.Errorf("样本数 = %d，期望 100", row.cacheHit+row.cacheMiss)
	}
}

// ── 整行映射 ────────────────────────────────────────────────────────────

func TestCandidateMapsChannelFields(t *testing.T) {
	now := time.Now()
	row := candidateRow{
		id: 7, name: "示例渠道", channelType: "openai-compatible",
		baseURL: "https://api.example.com/v1", protocol: "openai",
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
		got.Channel.BaseURL != "https://api.example.com/v1" ||
		got.Channel.Protocol != channel.ProtocolOpenAI {
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
		baseURL: "https://api.example.com/v1", protocol: "openai",
		models: []string{"model-a"},
	}
	cand, err := row.candidate("test-key-placeholder", now)
	if err != nil {
		t.Fatalf("candidate 出错: %v", err)
	}

	plan := (&route.Router{}).Select([]route.Candidate{cand}, "model-a")
	if len(plan.Candidates) != 1 {
		t.Errorf("选路结果 %d 个，期望 1；排除原因 %+v", len(plan.Candidates), plan.Excluded)
	}
}

func sqlNullInt(v int64) sql.NullInt64     { return sql.NullInt64{Int64: v, Valid: true} }
func sqlNullTime(v time.Time) sql.NullTime { return sql.NullTime{Time: v, Valid: true} }
