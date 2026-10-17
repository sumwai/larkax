package channels

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/testdb"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 这一层测的是 SQL，不是映射
//
// 纯映射（健康度、额度、成本）在 channels_test.go 里用构造出来的行测，
// 不需要数据库。这里要测的是那些**只有真库才能证明**的东西：
//
//   - text[] 的包含查询（models @> ARRAY[...]）真的按元素匹配，不是按字符串
//   - 三张表 LEFT JOIN 之后缺失的行真的变成 NULL，而不是把渠道整行丢掉
//   - jsonb / numeric / timestamptz 扫进 Go 类型的结果与预期一致
//
// 拿内存假装这些，测的是「我以为数据库会怎么做」。
//
// 没有 LARKAX_DB_DSN 时跳过而不是失败：本机未必起着库。
// **注意「跳过」与「通过」在输出里长得一样**，报数时要把它们分开。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// 每个用例用独立的标识，避免彼此干扰。
//
// 用「时间戳 + 进程内计数」而不是纯计数：纯计数在同一个库上跑第二次
// `go test` 时会从头开始，撞上上一次留下的行。
var seq struct {
	sync.Mutex
	n int64
}

var base = time.Now().UnixNano() % 1_000_000_000

func nextID() int64 {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return base*1000 + seq.n
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// 模型名与渠道名都带本次运行的唯一后缀：候选是按**模型名**查的，
// 共用一个库时，别的用例留下的渠道会混进结果里。
func uniqueModel() string { return "model-" + itoa(nextID()) }

func testSealer(t *testing.T) *credential.Sealer {
	t.Helper()
	key := make([]byte, config.KeySize)
	for i := range key {
		key[i] = 0x42
	}
	sealer, err := credential.NewSealer([]config.MasterKey{{ID: "test", Key: key}})
	if err != nil {
		t.Fatalf("构造 Sealer 失败: %v", err)
	}
	return sealer
}

// seedMerchant 建一个商家用户并返回 id。
func seedMerchant(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id := 100000 + nextID()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, email, is_merchant) VALUES ($1, $2, $3, true)`,
		id, "m"+itoa(id), "m"+itoa(id)+"@example.com")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// channelSpec 描述一条待插入的渠道，零值表示用默认。
type channelSpec struct {
	ownerUserID int64
	model       string
	status      string
	priority    int
	weight      int
	credential  []byte
	extraModels []string
}

// insertChannel 插入一条渠道并返回它的 id。
//
// 凭证密文默认用 testSealer 现封一份，于是取出来能解开——
// 只有需要「密文坏了」的用例才自己传 credential。
func insertChannel(t *testing.T, db *sql.DB, sealer *credential.Sealer, spec channelSpec) int64 {
	t.Helper()

	if spec.status == "" {
		spec.status = "enabled"
	}
	if spec.credential == nil {
		sealed, err := sealer.Seal([]byte("test-key-placeholder"))
		if err != nil {
			t.Fatalf("封装凭证失败: %v", err)
		}
		spec.credential = sealed
	}
	models := append([]string{spec.model}, spec.extraModels...)

	var owner any
	if spec.ownerUserID != 0 {
		owner = spec.ownerUserID
	}

	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO channels
			(owner_user_id, name, type, base_url, credential_enc,
			 status, models, weight, priority, setting)
		VALUES ($1, $2, 'openai-compatible', 'https://api.example.com/v1', $3,
		        $4, $5, $6, $7, '{"supports_tools":true}'::jsonb)
		RETURNING id`,
		owner, "ch-"+itoa(nextID()), spec.credential, spec.status,
		models, spec.weight, spec.priority).Scan(&id)
	if err != nil {
		t.Fatalf("插入渠道失败: %v", err)
	}
	return id
}

// hourBucket 返回距今最近的那个整点桶，n 为回退的小时数。
func hourBucket(n int) time.Time {
	return time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(n) * time.Hour)
}

func insertRollup(t *testing.T, db *sql.DB, channelID int64, bucket time.Time, requests, successes, failures, p95 int, cost string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_health_rollup
			(channel_id, bucket, requests, successes, failures, latency_p95_ms, cost)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric)`,
		channelID, bucket, requests, successes, failures, p95, cost)
	if err != nil {
		t.Fatalf("插入健康度失败: %v", err)
	}
}

// insertQuota 插一行额度状态。
//
// unit 收 any：nil 表示这一列是 NULL（「不知道按什么计」），
// 与空字符串不是一件事。
//
// confidence 显式给，不吃列的默认值：默认值是 0，而 0 在打分里表示
// 「这个数不可信」，于是漏给它的夹具造出的是**不可信**的状态——与它想
// 表达的「一次实测」正好相反，而那种错在断言里看不出来。
func insertQuota(t *testing.T, db *sql.DB, channelID int64, remaining any, unit any, probeKind, health string, confidence float64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_quota_state (channel_id, remaining_estimate, unit, probe_kind, health, confidence)
		VALUES ($1, $2::numeric, $3, $4, $5, $6::numeric)`,
		channelID, remaining, unit, probeKind, health, confidence)
	if err != nil {
		t.Fatalf("插入额度失败: %v", err)
	}
}

// ── 用例 ────────────────────────────────────────────────────────────────

func TestCandidatesReturnsEnabledChannel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	merchant := seedMerchant(t, db)

	id := insertChannel(t, db, sealer, channelSpec{
		ownerUserID: merchant, model: model, priority: 7, weight: 3,
		extraModels: []string{"other-" + model},
	})

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}

	c := got[0]
	if c.Channel.ID != id {
		t.Errorf("Channel.ID = %d，期望 %d", c.Channel.ID, id)
	}
	// 这是全仓库第一次真正接上信封加密：取出来的必须是明文。
	if c.Channel.Credential != "test-key-placeholder" {
		t.Errorf("凭证没解开，得到 %q", c.Channel.Credential)
	}
	if c.OwnerUserID != merchant {
		t.Errorf("OwnerUserID = %d，期望 %d", c.OwnerUserID, merchant)
	}
	if c.Priority != 7 || c.Weight != 3 {
		t.Errorf("Priority/Weight = %d/%d，期望 7/3", c.Priority, c.Weight)
	}
	if c.Channel.Setting["supports_tools"] != true {
		t.Errorf("Setting = %v，期望解开 jsonb", c.Channel.Setting)
	}
	if ok, reason := c.Eligible(); !ok {
		t.Errorf("取出来的候选不可选：%s", reason)
	}
}

// 平台自营渠道的 owner_user_id 是 NULL，映射成 0（route 的约定）。
func TestCandidatesPlatformChannelHasZeroOwner(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t, db, sealer, channelSpec{model: model})

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].OwnerUserID != 0 {
		t.Errorf("OwnerUserID = %d，期望 0（平台自营）", got[0].OwnerUserID)
	}
}

// 停用的渠道与不支持该模型的渠道都不该出现。
func TestCandidatesFiltersStatusAndModel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t, db, sealer, channelSpec{model: model, status: "disabled"})
	insertChannel(t, db, sealer, channelSpec{model: model, status: "drained"})
	insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("候选数 = %d，期望 0（停用与不支持该模型的都要滤掉）", len(got))
	}
}

// models 是 text[]，包含查询必须按元素匹配。这里用一个模型名是另一个
// 前缀的样本：按字符串匹配会把它误判成命中。
func TestCandidatesMatchesModelExactly(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t, db, sealer, channelSpec{model: model + "-suffix"})

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("候选数 = %d，期望 0（模型名要整段相等，不是前缀）", len(got))
	}
}

// 三张表联起来之后，健康度与额度都要落到候选上。
func TestCandidatesIncludesHealthAndQuota(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})

	// 两个桶：成功率是两者之和，p95 取**最新**那个非空值。
	insertRollup(t, db, id, hourBucket(1), 10, 9, 1, 300, "2.0")
	insertRollup(t, db, id, hourBucket(0), 5, 5, 0, 200, "1.0")
	insertQuota(t, db, id, "12.5", "money", "api_query", "healthy", 1)

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	c := got[0]

	if want := 14.0 / 15.0; c.Health.SuccessRate != want {
		t.Errorf("SuccessRate = %v，期望 %v", c.Health.SuccessRate, want)
	}
	if c.Health.LatencyP95 != 200*time.Millisecond {
		t.Errorf("LatencyP95 = %v，期望最新的 200ms", c.Health.LatencyP95)
	}
	// 最新桶里有成功，连续失败长度为 0。
	if c.Health.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d，期望 0", c.Health.ConsecutiveFailures)
	}
	if c.Quota.Remaining == nil || *c.Quota.Remaining != 12.5 {
		t.Errorf("Quota = %+v，期望有值且为 12.5", c.Quota)
	}
	// 单位要一起带出来：Remaining 是裸数字，没有单位 route 就不知道它
	// 能不能与别的候选比较。
	if c.Quota.Unit != "money" {
		t.Errorf("Quota.Unit = %q，期望 money", c.Quota.Unit)
	}
	if c.Quota.Estimated {
		t.Error("api_query 是实测，不该标成推算值")
	}
	// 可信度必须一起出来：route 按它把额度那一项往中性分拉（见 route 的
	// scoreQuota）。漏掉这一列不会报错，只会让每个候选的额度项都退成中性分
	// ——额度这一维静默地关掉，而排序照旧排得出来。查询里少取一列、
	// 或者少扫一个变量，都会在这里露出来。
	if c.Quota.Confidence != 1 {
		t.Errorf("Quota.Confidence = %v，期望 1（api_query 是实测）", c.Quota.Confidence)
	}
	// 单位成本 = 窗口内总额 / 请求数 = 3.0 / 15。
	if want := 3.0 / 15.0; c.CostPerUnit != want {
		t.Errorf("CostPerUnit = %v，期望 %v", c.CostPerUnit, want)
	}
}

// 连续失败取自 channels 的列，**不是**从小时桶里算的。
//
// 这一条与健康度桶刻意错开：桶里全是失败、而列里是 0，期望取列里的 0。
// 反过来（桶里全是成功、列里非零）也一样——桶重建不出「连续」这个量，
// 桶内有过成功时那段长度落在 0 与 failures 之间。
func TestCandidatesConsecutiveFailuresFromColumn(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})

	insertRollup(t, db, id, hourBucket(0), 4, 0, 4, 900, "0")
	setStreak(t, db, id, 0, nil)

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Health.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d，期望 0（桶里全是失败，但计数是 0）",
			got[0].Health.ConsecutiveFailures)
	}
	if reason := got[0].DrainReason(); reason != "" {
		t.Errorf("计数为 0 的渠道不该被摘除：%s", reason)
	}

	// 列里非零时按它走，并足以触发 route 的摘除阈值。
	at := time.Now().UTC().Add(-time.Minute)
	setStreak(t, db, id, 4, &at)

	got, err = src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if got[0].Health.ConsecutiveFailures != 4 {
		t.Errorf("ConsecutiveFailures = %d，期望 4", got[0].Health.ConsecutiveFailures)
	}
	if reason := got[0].DrainReason(); reason == "" {
		t.Error("连续失败 4 次的渠道应被判为暂时不该使用")
	}
}

// 冷却期已过的计数按 0 处理——被摘掉的渠道没有流量，计数不会自己降回去。
func TestCandidatesStreakExpiresAfterCooldown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})

	// 16 分钟前失败过：冷却期（15 分钟）已经过了。
	old := time.Now().UTC().Add(-16 * time.Minute)
	setStreak(t, db, id, 4, &old)

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Health.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d，期望 0（冷却期已过）", got[0].Health.ConsecutiveFailures)
	}
	if reason := got[0].DrainReason(); reason != "" {
		t.Errorf("冷却期已过的渠道不该被摘除：%s", reason)
	}
}

// setStreak 直接改连续失败那两列，模拟写入侧留下的状态。
func setStreak(t *testing.T, db *sql.DB, channelID int64, count int, at *time.Time) {
	t.Helper()
	var last any
	if at != nil {
		last = *at
	}
	_, err := db.ExecContext(context.Background(),
		`UPDATE channels SET consecutive_failures = $2, last_failure_at = $3 WHERE id = $1`,
		channelID, count, last)
	if err != nil {
		t.Fatalf("写连续失败失败: %v", err)
	}
}

// 没有健康度与额度行是**正常**的（新渠道）：要表现为未知，
// 而不是「最差」——否则新渠道永远选不上。
func TestCandidatesWithoutHealthOrQuotaIsUnknown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t, db, sealer, channelSpec{model: model})

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1（缺少健康度不该让渠道消失）", len(got))
	}
	c := got[0]
	if c.Health.SuccessRate != 0 || c.Health.LatencyP95 != 0 || c.Health.ConsecutiveFailures != 0 {
		t.Errorf("健康度应为零值（未知），实际 %+v", c.Health)
	}
	if c.Quota.Remaining != nil {
		t.Errorf("没有额度行时应是未知，实际 %+v", c.Quota)
	}
	if reason := c.DrainReason(); reason != "" {
		t.Errorf("新渠道不该被判为不该使用：%s", reason)
	}
}

// 额度行为 NULL 表示查不到额度，映射成未知而不是 0。
func TestCandidatesNullRemainingIsUnknown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})

	insertQuota(t, db, id, nil, nil, "self_accounting", "unknown", 0.2)

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Quota.Remaining != nil {
		t.Errorf("remaining_estimate 为 NULL 时应是未知，实际 %+v", got[0].Quota)
	}
}

// health='drained' 是上游明说的信号，即使 remaining_estimate 为 NULL
// 也要映射成「已知且为 0」，让 route 把它排除。
//
// 这条同时证明查询里真的取回了 q.health——少扫一列会在这里露出来。
func TestCandidatesDrainedQuotaIsZero(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})

	insertQuota(t, db, id, nil, nil, "error_observed", "drained", 0.6)

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	q := got[0].Quota
	if q.Remaining == nil || *q.Remaining != 0 {
		t.Fatalf("Quota = %+v，期望有值且为 0", q)
	}
	if reason := got[0].DrainReason(); reason == "" {
		t.Error("drained 的渠道应被 DrainReason 排除")
	}
}

// 一个渠道的密文坏了，不该让同一模型下别的渠道一起不可用。
func TestCandidatesSurvivesBrokenCredential(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t, db, sealer, channelSpec{model: model, credential: []byte{0x99}})
	insertChannel(t, db, sealer, channelSpec{model: model})

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1（好的那个仍要返回）", len(got))
	}
	if got[0].Channel.Credential != "test-key-placeholder" {
		t.Errorf("返回的应是能解开的那一个，得到 %q", got[0].Channel.Credential)
	}
}

// 一份都解不开时要把原因报出来，且能区分「密文坏了」与「缺主密钥」。
func TestCandidatesAllBrokenCredentialsReportsCorrupt(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t, db, sealer, channelSpec{model: model, credential: []byte{0x99}})
	insertChannel(t, db, sealer, channelSpec{model: model, credential: []byte{0x98}})

	src := New(db, sealer)
	_, err := src.Candidates(context.Background(), model)
	if err == nil {
		t.Fatal("一份都解不开时应报错")
	}
	if !errors.Is(err, ErrCredentialCorrupt) {
		t.Errorf("err = %v，期望 ErrCredentialCorrupt（数据问题）", err)
	}
	if errors.Is(err, ErrNoMasterKey) {
		t.Error("密文坏了不该被报成缺主密钥（那是部署问题）")
	}
}

// 进程没有主密钥时，每一个渠道都会解不开：这是部署问题，
// 错误要指向它，而不是报一个空的候选集。
func TestCandidatesWithoutSealerReportsMissingKey(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t, db, sealer, channelSpec{model: model})

	src := New(db, nil)
	_, err := src.Candidates(context.Background(), model)
	if err == nil {
		t.Fatal("没有主密钥时应报错")
	}
	if !errors.Is(err, ErrNoMasterKey) {
		t.Errorf("err = %v，期望 ErrNoMasterKey", err)
	}
}

// 用别的钥匙封的密文，同样是缺钥匙。
func TestCandidatesForeignKeyReportsMissingKey(t *testing.T) {
	db := testDB(t)
	otherKey := make([]byte, config.KeySize)
	for i := range otherKey {
		otherKey[i] = 0x7f
	}
	other, err := credential.NewSealer([]config.MasterKey{{ID: "other", Key: otherKey}})
	if err != nil {
		t.Fatalf("构造 Sealer 失败: %v", err)
	}
	model := uniqueModel()
	insertChannel(t, db, other, channelSpec{model: model})

	src := New(db, testSealer(t))
	_, err = src.Candidates(context.Background(), model)
	if err == nil {
		t.Fatal("钥匙对不上时应报错")
	}
	if !errors.Is(err, ErrNoMasterKey) {
		t.Errorf("err = %v，期望 ErrNoMasterKey", err)
	}
}

// 空模型名直接拒绝，不落到数据库上。
func TestCandidatesRejectsEmptyModel(t *testing.T) {
	src := New(nil, nil)
	if _, err := src.Candidates(context.Background(), ""); err == nil {
		t.Error("空模型名应报错")
	}
}
