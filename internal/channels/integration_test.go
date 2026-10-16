package channels

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/migrate"
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

	dsn := os.Getenv("LARKAX_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}

	handle, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if err := handle.PingContext(context.Background()); err != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", err)
	}

	// goose 自己记录版本，重复调用是幂等的。
	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.UpContext(context.Background(), handle, "."); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return handle
}

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

func insertQuota(t *testing.T, db *sql.DB, channelID int64, remaining any, probeKind, health string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_quota_state (channel_id, remaining_estimate, probe_kind, health)
		VALUES ($1, $2::numeric, $3, $4)`, channelID, remaining, probeKind, health)
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
	insertQuota(t, db, id, "12.5", "api_query", "healthy")

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
	if !c.Quota.Known || c.Quota.Remaining != 12.5 {
		t.Errorf("Quota = %+v，期望已知且为 12.5", c.Quota)
	}
	if c.Quota.Estimated {
		t.Error("api_query 是实测，不该标成推算值")
	}
	// 单位成本 = 窗口内总额 / 请求数 = 3.0 / 15。
	if want := 3.0 / 15.0; c.CostPerUnit != want {
		t.Errorf("CostPerUnit = %v，期望 %v", c.CostPerUnit, want)
	}
}

// 最新桶全部失败：连续失败数取该桶的失败数，足以触发 route 的摘除阈值。
func TestCandidatesConsecutiveFailuresFromLatestBucket(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})

	insertRollup(t, db, id, hourBucket(1), 20, 20, 0, 150, "1.0")
	insertRollup(t, db, id, hourBucket(0), 4, 0, 4, 900, "0")

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Health.ConsecutiveFailures != 4 {
		t.Errorf("ConsecutiveFailures = %d，期望 4", got[0].Health.ConsecutiveFailures)
	}
	if reason := got[0].DrainReason(); reason == "" {
		t.Error("连续失败 4 次的渠道应被判为暂时不该使用")
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
	if c.Quota.Known {
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

	insertQuota(t, db, id, nil, "self_accounting", "unknown")

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Quota.Known {
		t.Errorf("remaining_estimate 为 NULL 时应是未知，实际 %+v", got[0].Quota)
	}
}

// health='drained' 是上游明说的信号，即使 remaining_estimate 为 NULL
// 也要映射成「已知且为 0」，让 route 把它排除。
//
// 这条同时证明查询里真的取回了 q.health——少扫一列会在这里露出来。
func TestCandidatesDrainedQuotaIsKnownZero(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})

	insertQuota(t, db, id, nil, "error_observed", "drained")

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	q := got[0].Quota
	if !q.Known || q.Remaining != 0 {
		t.Fatalf("Quota = %+v，期望 Known=true、Remaining=0", q)
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

// ── 模型别名 ────────────────────────────────────────────────────────────

// seedModelCatalog 往 model_catalog 里插一条规范模型。
//
// model_aliases.canonical_name 有外键指向它，所以别名得先有这个模型。
func seedModelCatalog(t *testing.T, db *sql.DB, canonical string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO model_catalog (canonical_name, display_name) VALUES ($1, $1)`,
		canonical); err != nil {
		t.Fatalf("插入模型目录失败: %v", err)
	}
}

// insertAlias 插一条别名。channelID 传 nil 表示平台级别名。
func insertAlias(t *testing.T, db *sql.DB, channelID any, upstream, canonical string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
		VALUES ($1, $2, $3)`, channelID, upstream, canonical); err != nil {
		t.Fatalf("插入别名失败: %v", err)
	}
}

func TestUpstreamModelPrefersChannelSpecific(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})
	seedModelCatalog(t, db, model)

	// 平台级别名对所有渠道生效，渠道级的应该盖过它。
	insertAlias(t, db, nil, "platform-"+model, model)
	insertAlias(t, db, id, "channel-"+model, model)

	src := New(db, sealer)
	got, err := src.UpstreamModel(context.Background(), id, model)
	if err != nil {
		t.Fatalf("UpstreamModel 出错: %v", err)
	}
	if got != "channel-"+model {
		t.Errorf("UpstreamModel = %q，期望渠道级别名优先", got)
	}
}

// 渠道级没有时回落到平台级。
func TestUpstreamModelFallsBackToPlatform(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t, db, sealer, channelSpec{model: model})
	seedModelCatalog(t, db, model)
	insertAlias(t, db, nil, "platform-"+model, model)

	src := New(db, sealer)
	got, err := src.UpstreamModel(context.Background(), id, model)
	if err != nil {
		t.Fatalf("UpstreamModel 出错: %v", err)
	}
	if got != "platform-"+model {
		t.Errorf("UpstreamModel = %q，期望回落到平台级别名", got)
	}
}

// 别的渠道的别名不该被用上。
func TestUpstreamModelIgnoresOtherChannels(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	mine := insertChannel(t, db, sealer, channelSpec{model: model})
	other := insertChannel(t, db, sealer, channelSpec{model: uniqueModel()})
	seedModelCatalog(t, db, model)
	insertAlias(t, db, other, "other-"+model, model)

	src := New(db, sealer)
	_, err := src.UpstreamModel(context.Background(), mine, model)
	if !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("err = %v，期望 ErrAliasNotFound", err)
	}
}

func TestUpstreamModelNotFound(t *testing.T) {
	db := testDB(t)
	src := New(db, testSealer(t))

	_, err := src.UpstreamModel(context.Background(), 1, "nonexistent-"+itoa(nextID()))
	if !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("err = %v，期望 ErrAliasNotFound", err)
	}
}

// channelID 传 0 是「不针对具体渠道」，只走平台级别名那条。
func TestUpstreamModelChannelZeroUsesPlatformAlias(t *testing.T) {
	db := testDB(t)
	model := uniqueModel()
	seedModelCatalog(t, db, model)
	insertAlias(t, db, nil, "platform-"+model, model)

	src := New(db, testSealer(t))
	got, err := src.UpstreamModel(context.Background(), 0, model)
	if err != nil {
		t.Fatalf("UpstreamModel 出错: %v", err)
	}
	if got != "platform-"+model {
		t.Errorf("UpstreamModel = %q，期望平台级别名", got)
	}
}

// ── 别名的唯一约束 ──────────────────────────────────────────────────────

// 两条偏索引的名字硬编码自 00007_model_alias_unique.sql。
// **改那个文件就要同步改这里**，否则下面的断言会认不出来；
// 失效的方向是失败，不是静默通过。
const (
	aliasChannelCanonicalUnique  = "model_aliases_channel_canonical_idx"
	aliasPlatformCanonicalUnique = "model_aliases_platform_canonical_idx"
)

// 约束所在的迁移文件，也是「已有重复数据会被拒绝」那条用例要重放的那一份。
const aliasUniqueMigration = "00007_model_alias_unique.sql"

// insertAliasErr 与 insertAlias 相同，但把错误交回调用方。
//
// 「写入被拦下」正是下面几条用例要断言的东西，所以不能在这里 t.Fatalf。
func insertAliasErr(db *sql.DB, channelID any, upstream, canonical string) error {
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
		VALUES ($1, $2, $3)`, channelID, upstream, canonical)
	return err
}

// assertUniqueViolation 断言这次写入被**指定的那条唯一索引**拦下了。
//
// 只断言「报错了」不够：连不上库、外键不存在、列名写错都会报错，
// 而它们与「重复被拦下」是两回事。所以既认 SQLSTATE 23505，也认约束名——
// 约束名才说明是这条规则在起作用。
func assertUniqueViolation(t *testing.T, err error, wantIndex string) {
	t.Helper()

	if err == nil {
		t.Fatal("重复的别名应当被拒，实际写进去了")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v，期望 *pgconn.PgError", err)
	}
	if pgErr.Code != "23505" {
		t.Fatalf("SQLSTATE = %s（%s），期望 23505 unique_violation", pgErr.Code, pgErr.Message)
	}
	if pgErr.ConstraintName != wantIndex {
		t.Errorf("被违反的索引是 %q，期望 %q", pgErr.ConstraintName, wantIndex)
	}
}

// 同一个渠道下两个上游名映射到同一个规范名：写入侧就该被拦下。
//
// 这正是 issue 说的那条歧义——约束建立之前这种数据能写进来，而查询侧只能按
// 上游名排序取第一条，选错就是差价。
func TestAliasRejectsChannelDuplicateCanonical(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	channel := insertChannel(t, db, sealer, channelSpec{model: model})
	seedModelCatalog(t, db, model)

	insertAlias(t, db, channel, "up-a-"+model, model)
	assertUniqueViolation(t,
		insertAliasErr(db, channel, "up-b-"+model, model),
		aliasChannelCanonicalUnique)
}

// 平台级那一批要单独一条偏索引才拦得住：SQL 里 NULL 不等于 NULL，
// 普通唯一键对 channel_id IS NULL 的行一行都不管。
func TestAliasRejectsPlatformDuplicateCanonical(t *testing.T) {
	db := testDB(t)
	model := uniqueModel()
	seedModelCatalog(t, db, model)

	insertAlias(t, db, nil, "up-a-"+model, model)
	assertUniqueViolation(t,
		insertAliasErr(db, nil, "up-b-"+model, model),
		aliasPlatformCanonicalUnique)
}

// 反方向——一个上游名对应两个规范名——原本就被 (channel_id, upstream_model)
// 拦着。这条用例把那个约束也钉住：删掉它，镜像的歧义会重新出现。
func TestAliasRejectsDuplicateUpstreamModel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	other := uniqueModel()
	channel := insertChannel(t, db, sealer, channelSpec{model: model})
	seedModelCatalog(t, db, model)
	seedModelCatalog(t, db, other)

	insertAlias(t, db, channel, "up-"+model, model)
	assertUniqueViolation(t,
		insertAliasErr(db, channel, "up-"+model, other),
		"model_aliases_channel_id_upstream_model_key")
}

// 约束只该拦「同一个范围里的同一个规范名」，别的一律不受影响——
// 拦宽了的话，「两个渠道各配一个上游名指向同一个规范名」这种正常配置
// 就会写不进去。
//
// 这条同时是查询侧简化的前提：允许写进去的这几条里，同一个范围下指向 model
// 的**各自只有一条**，所以 UpstreamModel 命中什么是确定的。
func TestAliasAllowsDistinctScopesAndNames(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	other := uniqueModel()
	mine := insertChannel(t, db, sealer, channelSpec{model: model})
	theirs := insertChannel(t, db, sealer, channelSpec{model: model})
	seedModelCatalog(t, db, model)
	seedModelCatalog(t, db, other)

	insertAlias(t, db, mine, "up-a-"+model, model)
	insertAlias(t, db, theirs, "up-b-"+model, model) // 另一个渠道：允许
	insertAlias(t, db, mine, "up-c-"+model, other)   // 同一渠道、另一个规范名：允许
	insertAlias(t, db, nil, "up-d-"+model, model)    // 平台级与渠道级并存：允许
	insertAlias(t, db, nil, "up-e-"+model, other)    // 平台级的另一个规范名：允许

	src := New(db, sealer)
	got, err := src.UpstreamModel(context.Background(), mine, model)
	if err != nil {
		t.Fatalf("UpstreamModel 出错: %v", err)
	}
	if got != "up-a-"+model {
		t.Errorf("UpstreamModel = %q，期望渠道级那条 %q", got, "up-a-"+model)
	}
}

// migrationUpSQL 取出内嵌迁移文件的 Up 段原文。
//
// 用例要在「约束之前」的状态上重放这条迁移，所以需要它的原文。
// 切分用的是 goose 的标注格式；切不到就报错——失配时**不能**静默返回半段
// SQL，那样用例会退化成一条什么都没跑的测试。
func migrationUpSQL(t *testing.T, name string) string {
	t.Helper()

	raw, err := fs.ReadFile(migrate.FS(), name)
	if err != nil {
		t.Fatalf("读取迁移文件 %s 失败: %v", name, err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatalf("迁移文件 %s 里没有 -- +goose Down 标注，切分方式要跟着改", name)
	}
	up, ok = strings.CutPrefix(up, "-- +goose Up")
	if !ok {
		t.Fatalf("迁移文件 %s 里没有 -- +goose Up 标注", name)
	}
	if strings.TrimSpace(up) == "" || strings.TrimSpace(down) == "" {
		t.Fatalf("迁移文件 %s 的 Up 段或 Down 段是空的", name)
	}
	return up
}

// 已有重复数据时，迁移必须报错退出，而不是静默清理。
//
// 「约束之前」的状态用事务造出来：索引在事务里摘掉、重复行在事务里插进去，
// 跑完迁移再整体回滚（PostgreSQL 的 DDL 也在事务里，回滚会把索引带回来）。
// 这样即使用例中途失败，库也不会被留在「没有约束」的状态上——那种状态不报错，
// 只会让后面所有用例失去保护。
func TestMigrationRejectsExistingDuplicateAliases(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sealer := testSealer(t)
	model := uniqueModel()
	channel := insertChannel(t, db, sealer, channelSpec{model: model})

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, idx := range []string{aliasChannelCanonicalUnique, aliasPlatformCanonicalUnique} {
		if _, err := tx.ExecContext(ctx, `DROP INDEX `+idx); err != nil {
			t.Fatalf("删掉索引 %s 失败: %v", idx, err)
		}
	}

	// 两类重复各造一组：同一个规范名在同一个渠道下有两个上游名，
	// 以及平台级下同样的一对。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO model_catalog (canonical_name, display_name) VALUES ($1, $1)`,
		model); err != nil {
		t.Fatalf("插入模型目录失败: %v", err)
	}
	for _, dup := range []struct {
		channelID any
		upstream  string
	}{
		{channel, "up-a-" + model},
		{channel, "up-b-" + model},
		{nil, "up-c-" + model},
		{nil, "up-d-" + model},
	} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
			VALUES ($1, $2, $3)`, dup.channelID, dup.upstream, model); err != nil {
			t.Fatalf("造重复数据失败: %v", err)
		}
	}

	// 迁移报错之后整个事务会被 PostgreSQL 置为 aborted，后面什么都查不了，
	// 所以先打一个保存点：报错后回到这里，事务恢复可用，造的重复数据还在。
	if _, err := tx.ExecContext(ctx, `SAVEPOINT before_migration`); err != nil {
		t.Fatalf("打保存点失败: %v", err)
	}

	_, err = tx.ExecContext(ctx, migrationUpSQL(t, aliasUniqueMigration))
	if err == nil {
		t.Fatal("已有重复数据时迁移必须报错，而不是静默清理")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v，期望 *pgconn.PgError", err)
	}
	if pgErr.Code != "P0001" {
		t.Fatalf("SQLSTATE = %s（%s），期望 P0001（迁移自己抛的那个）", pgErr.Code, pgErr.Message)
	}
	// 报错要说清是哪几行重复，否则人拿到它不知道该改什么。
	if !strings.Contains(pgErr.Message, model) {
		t.Errorf("报错里没提重复的规范名 %q：%s", model, pgErr.Message)
	}

	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT before_migration`); err != nil {
		t.Fatalf("回滚到保存点失败: %v", err)
	}

	// 一条都不许被删掉：迁移的职责是拒绝，不是替数据挑一条。
	var left int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM model_aliases WHERE canonical_name = $1`, model).Scan(&left); err != nil {
		t.Fatalf("回查重复行失败: %v", err)
	}
	if left != 4 {
		t.Errorf("重复行还剩 %d 条，期望 4（迁移不该删数据）", left)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}

	// 回滚必须把索引带回来，否则这条用例把库留在了没有约束的状态上，
	// 而后面那些用例会照常通过——它们只是不再受保护。
	for _, idx := range []string{aliasChannelCanonicalUnique, aliasPlatformCanonicalUnique} {
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_indexes
			                WHERE schemaname = current_schema() AND indexname = $1)`,
			idx).Scan(&exists); err != nil {
			t.Fatalf("查索引 %s 失败: %v", idx, err)
		}
		if !exists {
			t.Errorf("回滚后索引 %s 不在了", idx)
		}
	}
}
