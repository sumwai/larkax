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
	"github.com/sumwai/larkax/internal/route"
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

// insertRollupSignals 插一行**带全部信号列**的桶。
//
// 与 insertRollup 分开：那三列正是选路打分的三个维度，「这个用例到底写没写
// 它们」在调用处要一眼看得出来。收 any 是为了让「这一列是 NULL」也能表达
// ——写入侧今天写的正是 NULL（latency）与 0（cost / cache）。
func insertRollupSignals(t *testing.T, db *sql.DB, channelID int64, bucket time.Time,
	requests, successes, failures int, p95, cost, cacheHit, cacheMiss any) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_health_rollup
			(channel_id, bucket, requests, successes, failures,
			 latency_p95_ms, cost, cache_hit_tokens, cache_miss_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9)`,
		channelID, bucket, requests, successes, failures, p95, cost, cacheHit, cacheMiss)
	if err != nil {
		t.Fatalf("插入健康度失败: %v", err)
	}
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
	if c.Health.LatencyP95 == nil || *c.Health.LatencyP95 != 200*time.Millisecond {
		t.Errorf("LatencyP95 = %s，期望最新的 200ms", showPtr(c.Health.LatencyP95))
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
	if c.CostPerUnit == nil {
		t.Fatal("CostPerUnit 是 nil，期望 3.0/15")
	}
	if want := 3.0 / 15.0; *c.CostPerUnit != want {
		t.Errorf("CostPerUnit = %v，期望 %v", *c.CostPerUnit, want)
	}
}

// 桶里真的写上了成本 / 延迟 / 缓存三列之后，它们必须真的影响排序。
//
// # 这一条钉的是「接线之后会不会生效」
//
// 写入侧今天不写那三列（见 record.go），所以生产里它们是 0 / NULL，
// 打分时整条退出加权——这条路径由 route 的
// TestOrderingUnchangedWithoutRollupData 钉住。而**接线那一天**会怎样，
// 只有把数据真的写进桶里才证明得了：查询要把值取出来、映射要原样带出去、
// 打分要按权重算进去、排序要跟着变。
//
// # 两个候选只差那三列
//
// 健康度与额度刻意造得一模一样（同样的请求数、成功数、没有额度行），
// 于是「谁排前面」只可能由那三列决定。末尾那个对照把这三列抹掉，
// 两个候选必须**同分**——否则说明差别来自别处，这条用例证明的东西就不成立。
func TestCandidatesRollupSignalsAffectOrdering(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	withData := insertChannel(t, db, sealer, channelSpec{model: model})
	withoutData := insertChannel(t, db, sealer, channelSpec{model: model})

	// 两边同一个健康度：20 次请求、2 次成功。没有额度行（未知）。
	// 有数据的那一个：p95 50ms、20 次请求共 0.02（每次 0.001）、
	// 缓存命中 80 / 未命中 20。
	insertRollupSignals(t, db, withData, hourBucket(0), 20, 2, 18,
		50, "0.02", 80, 20)
	// 没有数据的那一个：桶在、健康度一样，而那三列就是写入侧今天的样子。
	insertRollupSignals(t, db, withoutData, hourBucket(0), 20, 2, 18,
		nil, "0", 0, 0)

	src := New(db, sealer)
	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(got))
	}
	byID := map[int64]route.Candidate{}
	for _, c := range got {
		byID[c.Channel.ID] = c
	}
	a, b := byID[withData], byID[withoutData]

	// 读侧要原样带出三个值。少带一个，route 那一维就整条不参与，
	// 而这件事在分数上只表现为「少了一点」，不会报错。
	if a.CostPerUnit == nil || *a.CostPerUnit != 0.001 {
		t.Errorf("CostPerUnit = %s，期望 0.001（0.02 / 20 次请求）", showPtr(a.CostPerUnit))
	}
	if a.Health.LatencyP95 == nil || *a.Health.LatencyP95 != 50*time.Millisecond {
		t.Errorf("LatencyP95 = %s，期望 50ms", showPtr(a.Health.LatencyP95))
	}
	if a.CacheHitRate == nil || *a.CacheHitRate != 0.8 || a.SampleCount != 100 {
		t.Errorf("缓存 = %s / 样本 %d，期望 0.8 / 100",
			showPtr(a.CacheHitRate), a.SampleCount)
	}

	// 另一边必须是「没有数据」而不是「数据是 0」：0 在打分里是免费、
	// 最快、一次都没命中，与没有数据是相反的结论。
	if b.CostPerUnit != nil || b.Health.LatencyP95 != nil || b.CacheHitRate != nil {
		t.Errorf("没写过那三列的渠道应是未知，实际 cost=%s latency=%s cache=%s",
			showPtr(b.CostPerUnit), showPtr(b.Health.LatencyP95), showPtr(b.CacheHitRate))
	}

	// 除了那三项，两个候选必须完全一样，否则下面证明不了什么。
	if a.Health.SuccessRate != b.Health.SuccessRate ||
		a.Health.ConsecutiveFailures != b.Health.ConsecutiveFailures ||
		a.Quota.Remaining != nil || b.Quota.Remaining != nil {
		t.Fatalf("两个候选的健康度/额度应当一样：a=%+v b=%+v", a.Health, b.Health)
	}

	r := &route.Router{}
	if r.Score(a) <= r.Score(b) {
		t.Errorf("有数据的候选应更高：%v vs %v", r.Score(a), r.Score(b))
	}

	plan := r.Select(got, model)
	if len(plan.Candidates) != 2 {
		t.Fatalf("选路结果 %d 个，期望 2；排除原因 %+v", len(plan.Candidates), plan.Excluded)
	}
	if plan.Candidates[0].Channel.ID != withData {
		t.Errorf("排序应把有数据的那个放前面，实际第一个是渠道 %d", plan.Candidates[0].Channel.ID)
	}

	// 对照：把三项抹掉，两者必须同分。
	blank := a
	blank.CostPerUnit, blank.CacheHitRate, blank.SampleCount, blank.Health.LatencyP95 = nil, nil, 0, nil
	if r.Score(blank) != r.Score(b) {
		t.Errorf("抹掉三项数据后应同分，实际 %v vs %v——差别不止来自那三列",
			r.Score(blank), r.Score(b))
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
	if c.Health.SuccessRate != 0 || c.Health.LatencyP95 != nil || c.Health.ConsecutiveFailures != 0 {
		t.Errorf("健康度应为零值（未知），实际 SuccessRate=%v LatencyP95=%s ConsecutiveFailures=%d",
			c.Health.SuccessRate, showPtr(c.Health.LatencyP95), c.Health.ConsecutiveFailures)
	}
	// 三列没有数据时映射成 nil，而不是 0：0 在打分里是「免费」「最快」
	// 「一次都没命中」，与「没有数据」是相反的结论。
	if c.CostPerUnit != nil || c.CacheHitRate != nil || c.SampleCount != 0 {
		t.Errorf("没有桶时成本与缓存都该是未知：cost=%s cache=%s samples=%d",
			showPtr(c.CostPerUnit), showPtr(c.CacheHitRate), c.SampleCount)
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
