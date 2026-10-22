package channels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/migrate"
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
func insertChannel(ctx context.Context, t *testing.T, db *sql.DB, sealer *credential.Sealer, spec channelSpec) int64 {
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
	err := db.QueryRowContext(ctx, `
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
// ——延迟两列在「整桶没有耗时样本」时正是 NULL（见 record.go 的 rollupSignalsSQL）。
//
// 两个延迟分位数一起给：写入侧两列出自同一条语句、同一个样本集，同一桶里
// 要么都有值、要么都是 NULL；分开给的话夹具能造出写入侧写不出来的行。
func insertRollupSignals(ctx context.Context, t *testing.T, db *sql.DB, channelID int64, bucket time.Time,
	requests, successes, failures int, p50, p95, cost, cacheHit, cacheMiss any) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO channel_health_rollup
			(channel_id, bucket, requests, successes, failures,
			 latency_p50_ms, latency_p95_ms, cost, cache_hit_tokens, cache_miss_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10)`,
		channelID, bucket, requests, successes, failures, p50, p95, cost, cacheHit, cacheMiss)
	if err != nil {
		t.Fatalf("插入健康度失败: %v", err)
	}
}

// insertRollupModelCost 插一行**模型桶的成本**。
//
// 健康列只读渠道级合并行（model 为空串），所以这里只写成本与成本事件数、
// 计数留 0：夹具表达的是「这个模型在这个小时的单价」，不是「这个渠道的
// 健康度」。cost_events 是读侧算单位成本的分母。
func insertRollupModelCost(ctx context.Context, t *testing.T, db *sql.DB,
	channelID int64, model string, bucket time.Time, cost string, costEvents int) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO channel_health_rollup (channel_id, model, bucket, cost, cost_events)
		VALUES ($1, $2, $3, $4::numeric, $5)`,
		channelID, model, bucket, cost, costEvents)
	if err != nil {
		t.Fatalf("插入模型桶成本失败: %v", err)
	}
}

func insertRollup(ctx context.Context, t *testing.T, db *sql.DB, channelID int64, bucket time.Time, requests, successes, failures, p95 int, cost string) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
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

	id := insertChannel(t.Context(), t, db, sealer, channelSpec{
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

	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

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

	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model, status: "disabled"})
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model, status: "drained"})
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: uniqueModel()})

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

	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model + "-suffix"})

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
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	// 两个桶：成功率是两者之和，p95 取**最新**那个非空值。
	insertRollup(t.Context(), t, db, id, hourBucket(1), 10, 9, 1, 300, "2.0")
	insertRollup(t.Context(), t, db, id, hourBucket(0), 5, 5, 0, 200, "1.0")
	// 成本按模型取：同一批金额各写一条模型行，分母是带成本的事件数。
	insertRollupModelCost(t.Context(), t, db, id, model, hourBucket(1), "2.0", 10)
	insertRollupModelCost(t.Context(), t, db, id, model, hourBucket(0), "1.0", 5)
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
	// 单位成本 = 窗口内总额 / 有成本事件的请求数 = 3.0 / 15。
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

	withData := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	withoutData := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	// 两边同一个健康度：20 次请求、2 次成功。没有额度行（未知）。
	// 有数据的那一个：p95 50ms、20 次请求共 0.02（每次 0.001）、
	// 缓存命中 80 / 未命中 20。选路读的是 p95，p50 按写入侧的样子成对给上。
	insertRollupSignals(t.Context(), t, db, withData, hourBucket(0), 20, 2, 18,
		20, 50, "0.02", 80, 20)
	// 成本按模型取：0.02 / 20 条成本事件 = 每次 0.001。
	insertRollupModelCost(t.Context(), t, db, withData, model, hourBucket(0), "0.02", 20)
	// 没有数据的那一个：桶在、健康度一样，而延迟与缓存那几列就是「一条样本
	// 都没有」的样子。
	insertRollupSignals(t.Context(), t, db, withoutData, hourBucket(0), 20, 2, 18,
		nil, nil, "0", 0, 0)

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

	plan := r.Select(got, model, route.Needs{})
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
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	insertRollup(t.Context(), t, db, id, hourBucket(0), 4, 0, 4, 900, "0")
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
	if _, reason := got[0].DrainReason(time.Time{}, 0); reason != "" {
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
	if _, reason := got[0].DrainReason(time.Time{}, 0); reason == "" {
		t.Error("连续失败 4 次的渠道应被判为暂时不该使用")
	}
}

// 冷却期已过的计数按 0 处理——被摘掉的渠道没有流量，计数不会自己降回去。
func TestCandidatesStreakExpiresAfterCooldown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

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
	if _, reason := got[0].DrainReason(time.Time{}, 0); reason != "" {
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

	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

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
	if _, reason := c.DrainReason(time.Time{}, 0); reason != "" {
		t.Errorf("新渠道不该被判为不该使用：%s", reason)
	}
}

// 额度行为 NULL 表示查不到额度，映射成未知而不是 0。
func TestCandidatesNullRemainingIsUnknown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

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
	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

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
	if _, reason := got[0].DrainReason(time.Time{}, 0); reason == "" {
		t.Error("drained 的渠道应被 DrainReason 排除")
	}
}

// 一个渠道的密文坏了，不该让同一模型下别的渠道一起不可用。
func TestCandidatesSurvivesBrokenCredential(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model, credential: []byte{0x99}})
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

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

	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model, credential: []byte{0x99}})
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model, credential: []byte{0x98}})

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

	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

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
	insertChannel(t.Context(), t, db, other, channelSpec{model: model})

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

// ── 软删 ────────────────────────────────────────────────────────────────
//
// 软删之后，**每一条读 channels 的路径**都不能再看见那一行。这一组用例按读点
// 逐条钉住，而不是只钉「清单里看不到」：漏掉一处不会报错，只会让一条已删的
// 渠道继续参与选路、继续出现在管理面或继续被轮换统计进去。
//
// 每一条自建自己的夹具（模型名带本次运行的唯一后缀），所以它们之间不共享状态。

// softDeleteRow 把一行标记成已删，并顺手把 status 置成 disabled
// （与 channels.Admin.SoftDelete 同一个口径：后者是生产里唯一的删除入口，
// 这里直接写库是为了让夹具不依赖管理面）。
func softDeleteRow(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE channels SET deleted_at = now(), status = 'disabled' WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("软删渠道 %d 失败: %v", id, err)
	}
}

// softDeleteKeepingEnabled 只置 deleted_at，不动 status。
//
// 生产里的删除入口（channels.Admin.SoftDelete）会同时把 status 改成 disabled，
// 于是选路那条 `status = 'enabled'` 顺带也挡住了已删的行——两条判据都在时，
// 少写哪一条都看不出来。这个夹具把 status 留在 enabled，让 channelAlive 成为
// 唯一挡得住它的东西（schema 允许这个状态：status 与 deleted_at 是两个独立的列）。
func softDeleteKeepingEnabled(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE channels SET deleted_at = now() WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("软删渠道 %d 失败: %v", id, err)
	}
}

// 读点一：选路。已删的渠道不进候选集。
//
// 两个已删的夹具停的状态不同：deleted 是删除路径留下的样子（deleted_at 有值
// 且 status = disabled），deletedEnabled 是只有 deleted_at 有值。后者专门钉住
// candidateQuery 里的 channelAlive——去掉那一句，它会以候选身份冒出来。
func TestCandidatesSkipsDeletedChannel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	deleted := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	deletedEnabled := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	live := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	softDeleteRow(t, db, deleted)
	softDeleteKeepingEnabled(t, db, deletedEnabled)

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1（两个已删的都不该进来）", len(got))
	}
	if got[0].Channel.ID != live {
		t.Errorf("候选是渠道 %d，期望 %d", got[0].Channel.ID, live)
	}
}

// 读点三：控制台的模型页。在供清单与选路共用 suppliesModel 这一条判据，
// 所以这里的四个夹具把两侧的分歧点各钉一个。
//
// 它只判**供给**：一个连续失败很多的渠道仍然在供这个模型（参考价不该因为
// 它今天不舒服而消失）。健康与额度是选路下一层的事，不在这条 SQL 里。
func TestSuppliedModelsFollowsTheSameSupplyPredicate(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := t.Context()

	supplied := uniqueModel()
	insertModelCatalog(t, db, supplied, nil, nil, nil, nil, nil)
	unhealthy := insertChannel(ctx, t, db, sealer, channelSpec{model: supplied})
	if _, err := db.ExecContext(ctx,
		`UPDATE channels SET consecutive_failures = 99 WHERE id = $1`, unhealthy); err != nil {
		t.Fatalf("改连续失败数失败: %v", err)
	}

	disabledModel := uniqueModel()
	insertModelCatalog(t, db, disabledModel, nil, nil, nil, nil, nil)
	insertChannel(ctx, t, db, sealer, channelSpec{model: disabledModel, status: "disabled"})

	deletedModel := uniqueModel()
	insertModelCatalog(t, db, deletedModel, nil, nil, nil, nil, nil)
	deleted := insertChannel(ctx, t, db, sealer, channelSpec{model: deletedModel})
	// status 留在 enabled，让 channelAlive 成为唯一挡得住它的东西。
	softDeleteKeepingEnabled(t, db, deleted)

	orphan := uniqueModel()
	insertModelCatalog(t, db, orphan, nil, nil, nil, nil, nil)

	got, err := New(db, sealer).SuppliedModels(ctx)
	if err != nil {
		t.Fatalf("SuppliedModels 出错: %v", err)
	}
	if !slices.Contains(got, supplied) {
		t.Errorf("%q 有启用的渠道在供，却不在清单里: %v", supplied, got)
	}
	for _, absent := range []string{disabledModel, deletedModel, orphan} {
		if slices.Contains(got, absent) {
			t.Errorf("%q 不该出现在在供清单里: %v", absent, got)
		}
	}
}

// Supplies 是报价那一侧要的供给行：模型 + 渠道 + 归属，判据与
// SuppliedModels 同一条。
//
// 平台自营在库里是 owner_user_id = NULL，映射成 0 与空用户名；商家渠道
// 带出用户名。禁用的渠道与没人供的模型都不出现。
func TestSuppliesCarryChannelOwner(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := t.Context()

	merchant := seedMerchant(t, db)
	model := uniqueModel()
	insertModelCatalog(t, db, model, nil, nil, nil, nil, nil)
	merchantChannel := insertChannel(ctx, t, db, sealer, channelSpec{ownerUserID: merchant, model: model})
	platformChannel := insertChannel(ctx, t, db, sealer, channelSpec{model: model})

	disabledModel := uniqueModel()
	insertModelCatalog(t, db, disabledModel, nil, nil, nil, nil, nil)
	insertChannel(ctx, t, db, sealer, channelSpec{model: disabledModel, status: "disabled"})

	orphan := uniqueModel()
	insertModelCatalog(t, db, orphan, nil, nil, nil, nil, nil)

	got, err := New(db, sealer).Supplies(ctx)
	if err != nil {
		t.Fatalf("Supplies 出错: %v", err)
	}

	byChannel := make(map[int64]Supply, len(got))
	for _, supply := range got {
		byChannel[supply.ChannelID] = supply
	}
	merchantSupply, ok := byChannel[merchantChannel]
	if !ok {
		t.Fatalf("商家渠道 %d 不在供给清单里", merchantChannel)
	}
	if merchantSupply.Model != model || merchantSupply.OwnerUserID != merchant ||
		merchantSupply.Merchant != "m"+itoa(merchant) {
		t.Errorf("商家渠道供给 = %+v，期望 model=%q owner=%d merchant=m%d",
			merchantSupply, model, merchant, merchant)
	}
	platformSupply, ok := byChannel[platformChannel]
	if !ok {
		t.Fatalf("平台渠道 %d 不在供给清单里", platformChannel)
	}
	if platformSupply.OwnerUserID != 0 || platformSupply.Merchant != "" {
		t.Errorf("平台渠道供给 = %+v，期望 owner=0、用户名空", platformSupply)
	}

	for _, supply := range got {
		if supply.Model == disabledModel || supply.Model == orphan {
			t.Errorf("%q 不该出现在供给清单里：%+v", supply.Model, supply)
		}
	}
}

// 读点三的补一条：**别名不进清单**。
//
// 清单里的名字是规范名——调用方请求时写的就是它。别名只说「这家上游把它
// 叫作什么」，所以两个方向都不该出现：上游名本身不在清单里，而只有别名、
// 没有渠道在供的模型也不在（别名不是供给）。
func TestSuppliedModelsIgnoresAliases(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := t.Context()

	supplied := uniqueModel()
	upstreamName := "upstream-" + itoa(nextID())
	insertModelCatalog(t, db, supplied, nil, nil, nil, nil, nil)
	channelID := insertChannel(ctx, t, db, sealer, channelSpec{model: supplied})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
		 VALUES ($1, $2, $3)`, channelID, upstreamName, supplied); err != nil {
		t.Fatalf("插入渠道级别名失败: %v", err)
	}

	// 只有一条平台级别名、没有任何渠道在供。
	aliasOnly := uniqueModel()
	insertModelCatalog(t, db, aliasOnly, nil, nil, nil, nil, nil)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO model_aliases (upstream_model, canonical_name)
		 VALUES ($1, $2)`, "upstream-"+itoa(nextID()), aliasOnly); err != nil {
		t.Fatalf("插入平台级别名失败: %v", err)
	}

	got, err := New(db, sealer).SuppliedModels(ctx)
	if err != nil {
		t.Fatalf("SuppliedModels 出错: %v", err)
	}
	if !slices.Contains(got, supplied) {
		t.Errorf("%q 有启用的渠道在供，却不在清单里: %v", supplied, got)
	}
	for _, absent := range []string{upstreamName, aliasOnly} {
		if slices.Contains(got, absent) {
			t.Errorf("%q 不该出现在在供清单里: %v", absent, got)
		}
	}
}

// 读点二：管理面清单。默认看不见，ListAll（--all）看得见且带上删除时刻。
func TestListHidesDeletedChannelUnlessAll(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	admin := newAdmin(t, db, sealer)
	model := uniqueModel()

	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	// 走生产里那个删除入口，不是直接写库：这一组要钉的正是「删掉之后清单
	// 怎么表现」，夹具自己拼一遍 deleted_at 的话，删除入口本身改坏了它照样通过。
	if _, err := admin.SoftDelete(context.Background(), id); err != nil {
		t.Fatalf("删除渠道 %d 失败: %v", id, err)
	}

	infos, err := admin.List(context.Background())
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	for _, info := range infos {
		if info.ID == id {
			t.Fatalf("默认清单里出现了已删的渠道 %d", id)
		}
	}

	// 每一条都要是零值：默认清单里不该有非零的 DeletedAt。
	for _, info := range infos {
		if !info.DeletedAt.IsZero() {
			t.Errorf("默认清单里出现了已删的行：%+v", info)
		}
	}

	all, err := admin.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll 出错: %v", err)
	}
	var found *ChannelInfo
	for i := range all {
		if all[i].ID == id {
			found = &all[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ListAll 里没有已删的渠道 %d", id)
	}
	if found.DeletedAt.IsZero() {
		t.Error("ListAll 里已删的行 DeletedAt 是零值")
	}
	// 删除时把状态一并置成 disabled：那是给漏网读路径的兜底。
	if found.Status != StatusDisabled {
		t.Errorf("已删渠道的 status = %q，期望 %q", found.Status, StatusDisabled)
	}
}

// 读点三：凭证轮换。已删的行不扫、也不要求它含当前主密钥。
func TestRotateSkipsDeletedChannels(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	deleted := insertChannel(t.Context(), t, db, sealer, channelSpec{
		model: model, credential: []byte{0x99}, // 解析不了的信封
	})
	live := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	softDeleteRow(t, db, deleted)

	rot := NewRotator(db, sealer)
	plan, err := rot.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan 出错: %v", err)
	}
	for _, p := range plan.Problems {
		if p.ID == deleted {
			t.Errorf("已删的渠道 %d 不该出现在轮换问题清单里：%v", deleted, p.Err)
		}
	}

	verify, err := rot.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify 出错: %v", err)
	}
	for _, m := range verify.Missing {
		if m.ID == deleted {
			t.Errorf("已删的渠道 %d 不该让 Verify 判成「漏网」", deleted)
		}
		if m.ID == live {
			t.Errorf("在用的渠道 %d 不该缺席：%v", live, m.Err)
		}
	}
}

// 读点四：健康汇总的写入侧。一次调用记到一条已删的渠道上时，不该再给它的
// 连续失败计数加一——那行数字最后会被谁读？没有读者了，但它会一直涨。
func TestRecordCallIgnoresDeletedChannel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()

	id := insertChannel(t.Context(), t, db, sealer, channelSpec{model: model, status: "disabled"})
	softDeleteRow(t, db, id)

	rec := NewRecorder(db, time.Minute)
	err := rec.RecordCall(context.Background(), route.CallOutcome{
		ChannelID: id,
		At:        time.Now(),
		Verdict:   route.VerdictFailure,
	})
	if err != nil {
		t.Fatalf("RecordCall 出错: %v", err)
	}

	var streak int
	if err := db.QueryRowContext(context.Background(),
		`SELECT consecutive_failures FROM channels WHERE id = $1`, id).Scan(&streak); err != nil {
		t.Fatalf("读回连续失败计数失败: %v", err)
	}
	if streak != 0 {
		t.Errorf("已删渠道的连续失败计数 = %d，期望 0（不该再给它记）", streak)
	}
}

// 迁移 up → down → up 往返：00024 加的那一列与它的部分索引都要能被干净地
// 退回去再装回来。
//
// 它跑在本包自己的测试库上（internal/testdb 按包路径建的那个），而且是**顺序**
// 执行的：本包没有 t.Parallel，所以中间那一小段没有 deleted_at 的窗口碰不到
// 别的用例。既有的行不受影响——down 只丢这一列，up 装回来时它是 NULL，与
// 建表时的状态一致。
//
// # 回滚指名版本，不说「退回最近一次」
//
// 00024 之后还会有别的迁移，那时「退回最近一次」撤的是那一条，这一列根本
// 不动——而用例会以为自己验过回滚了。所以退到哪一版写在常量上（见
// softDeleteDownTo），而不是跟着「最新」走。
func TestMigrateSoftDeleteRoundTrips(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// softDeleteDownTo 是回滚的目标版本：00024 的**前一条**。goose 的 DownTo
	// 一直退到「当前版本 <= 目标」为止，所以要去掉 00024 的效果，目标得写在
	// 它下面那一版上。
	const softDeleteDownTo = 23

	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}

	columnExists := func() bool {
		t.Helper()
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				 WHERE table_name = 'channels' AND column_name = 'deleted_at')`).Scan(&exists); err != nil {
			t.Fatalf("查列是否存在失败: %v", err)
		}
		return exists
	}
	indexExists := func() bool {
		t.Helper()
		var exists bool
		if err := db.QueryRowContext(ctx,
			`SELECT to_regclass('channels_live_idx') IS NOT NULL`).Scan(&exists); err != nil {
			t.Fatalf("查索引是否存在失败: %v", err)
		}
		return exists
	}

	// 正向证据：开始之前那一列必须在，否则下面的 down 什么都没撤、而 up 也
	// 看不出装回来了什么（LESSONS.md 第 16 条）。
	if !columnExists() {
		t.Fatal("迁移之后 channels 没有 deleted_at 列：up 这一侧就没成立")
	}
	if !indexExists() {
		t.Fatal("迁移之后没有 channels_live_idx 索引")
	}

	before, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("读当前迁移版本失败: %v", err)
	}

	if migErr := goose.DownToContext(ctx, db, ".", softDeleteDownTo); migErr != nil {
		t.Fatalf("回滚 00024 失败: %v", migErr)
	}
	if columnExists() {
		t.Error("回滚之后 deleted_at 列还在")
	}
	if indexExists() {
		t.Error("回滚之后 channels_live_idx 索引还在")
	}
	down, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("读回滚之后的版本失败: %v", err)
	}
	if down >= before {
		t.Errorf("回滚之后版本 = %d，期望小于 %d", down, before)
	}

	if migErr := goose.UpContext(ctx, db, "."); migErr != nil {
		t.Fatalf("重新前进失败: %v", migErr)
	}
	if !columnExists() {
		t.Error("重新前进之后 deleted_at 列没有回来")
	}
	if !indexExists() {
		t.Error("重新前进之后 channels_live_idx 索引没有回来")
	}
	up, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		t.Fatalf("读重新前进之后的版本失败: %v", err)
	}
	if up != before {
		t.Errorf("往返之后的版本 = %d，期望回到 %d", up, before)
	}
}

// ── 目录判据 ────────────────────────────────────────────────────────────
//
// 选路要按模型目录声明的能力与上限筛候选（见 route.Candidate.Profile）。
// 这一组用例钉的是**那份数据真的从库里出来了**，以及它与「没有这条记录」
// 分得开——后者是放行，前者才是判据。

// insertModelCatalog 插一条规范模型。
//
// 三个能力与两个上限都收 any：nil 表示库里是 NULL，也就是「未知」。上限那
// 两侧 NULL 折成 0（查询里套了 COALESCE），能力那三列 NULL 原样读成 nil
// ——两种表达同一个意思：不参与判据（见 catalog.Capabilities）。
func insertModelCatalog(t *testing.T, db *sql.DB, name string,
	tools, vision, reasoning, contextLimit, outputLimit any) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO model_catalog
			(canonical_name, display_name, supports_tools, supports_vision,
			 supports_reasoning, context_limit, output_limit)
		VALUES ($1, $1, $2, $3, $4, $5, $6)`,
		name, tools, vision, reasoning, contextLimit, outputLimit)
	if err != nil {
		t.Fatalf("插入规范模型失败: %v", err)
	}
}

// fakeCatalog 记下读目录的次数，并可以造出「读不出来」与「没有这条记录」。
// 正常路径转给真实现，所以它证明的仍然是真库上跑出来的结果。
type fakeCatalog struct {
	inner  modelCatalog
	calls  int
	models []string
	err    error
}

func (f *fakeCatalog) Constraints(ctx context.Context, canonicalName string) (catalog.Constraints, error) {
	f.calls++
	f.models = append(f.models, canonicalName)
	if f.err != nil {
		return catalog.Constraints{}, f.err
	}
	return f.inner.Constraints(ctx, canonicalName)
}

func TestCandidatesCarryCatalogProfile(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	insertModelCatalog(t, db, model, true, false, true, 200000, 8192)

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}

	p := got[0].Profile
	if p == nil {
		t.Fatal("目录里有这条记录，Profile 不该是 nil")
	}
	if p.Tools == nil || !*p.Tools || p.Vision == nil || *p.Vision || p.Reasoning == nil || !*p.Reasoning {
		t.Errorf("能力 = tools:%s vision:%s reasoning:%s，期望 支持/不支持/支持",
			capabilityText(p.Tools), capabilityText(p.Vision), capabilityText(p.Reasoning))
	}
	if p.ContextLimit != 200000 || p.OutputLimit != 8192 {
		t.Errorf("上限 = %d/%d，期望 200000/8192", p.ContextLimit, p.OutputLimit)
	}
}

// capabilityText 把三态打成一句话，供断言失败时的信息用。
//
// 失败时最需要知道的正是「是没配过（nil）还是配了说不支持」——这两者在
// 判据里处置相反，而 `%v` 打一个 *bool 出来给的是地址。
func capabilityText(item *bool) string {
	switch {
	case item == nil:
		return "未知"
	case *item:
		return "支持"
	default:
		return "不支持"
	}
}

// 目录里没有这个模型 = 未知 = 放行，而不是「什么都不支持」。
//
// 这是最常见的开始状态：渠道先接上了，采集还没跑到。按「不支持」处理会让
// 整批模型在那一刻用不了。
func TestCandidatesWithoutCatalogRowAreUnknown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	if got[0].Profile != nil {
		t.Errorf("目录里没有这条记录时 Profile 应是 nil，实际 %+v", got[0].Profile)
	}
}

// 读目录失败时整次报错，不降级成「未知」——降级会让一次查询故障看起来
// 与「这个部署没有目录判据」一模一样。
func TestCandidatesFailWhenCatalogReadFails(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	src := New(db, sealer)
	src.catalog = &fakeCatalog{inner: src.catalog, err: errors.New("库挂了")}

	got, err := src.Candidates(context.Background(), model)
	if err == nil {
		t.Fatal("读目录失败时应报错")
	}
	if got != nil {
		t.Errorf("报错时不该交回候选: %+v", got)
	}
	if !strings.Contains(err.Error(), model) {
		t.Errorf("错误里要带上模型名，实际 %v", err)
	}
}

// 「没有这条记录」是数据层的一个正常答案，不是错误：它整条链路上都要
// 变成 nil（未知）而不是向上冒。
func TestCandidatesTreatMissingModelAsUnknown(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	src := New(db, sealer)
	src.catalog = &fakeCatalog{inner: src.catalog,
		err: fmt.Errorf("%w：%q", catalog.ErrModelNotFound, model)}

	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(got) != 1 || got[0].Profile != nil {
		t.Errorf("候选 = %+v，期望一条、Profile 为 nil", got)
	}
}

// 目录声明是模型级的：一次取候选只读它一次，与候选数无关；一个候选都没有
// 时压根不读。
//
// 这条路径在每条请求上：多出来的往返必须是有界的，而下界是零。
func TestCandidatesReadCatalogOncePerCall(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model, extraModels: []string{"x-" + model}})
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	insertModelCatalog(t, db, model, true, true, true, nil, nil)

	src := New(db, sealer)
	fake := &fakeCatalog{inner: src.catalog}
	src.catalog = fake

	got, err := src.Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("候选数 = %d，期望 2", len(got))
	}
	if fake.calls != 1 {
		t.Errorf("两个候选时读了 %d 次目录，期望 1 次", fake.calls)
	}

	// 没有候选的模型：这次请求接下来要报「没有可用的渠道」，多读一次
	// 只会多一个失败点。
	if _, err := src.Candidates(context.Background(), uniqueModel()); err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("没有候选时也读了目录（共 %d 次），期望仍是 1 次", fake.calls)
	}
}

// 从库到选路的整条链：目录说这个模型不支持工具，带工具的请求就选不到它，
// 而且理由是「模型目录」。
func TestCatalogProfileFiltersCandidateInRoute(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})
	insertModelCatalog(t, db, model, false, false, false, 1000, nil)

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}

	r := &route.Router{}
	// 不带工具的请求照旧可走：筛的是「这次请求要什么」，不是「这个模型不好」。
	if plan := r.Select(got, model, route.Needs{}); len(plan.Candidates) != 1 {
		t.Errorf("不带工具时应当能选到，排除原因 %+v", plan.Excluded)
	}

	plan := r.Select(got, model, route.Needs{Tools: true, InputTokens: 900, MaxOutputTokens: 200})
	if len(plan.Candidates) != 0 {
		t.Fatalf("带工具的请求不该选到它，实际 %d 个候选", len(plan.Candidates))
	}
	if len(plan.Excluded) != 1 || !plan.Excluded[0].Unsupported {
		t.Fatalf("排除记录 = %+v，期望一条目录判据", plan.Excluded)
	}
	if !strings.Contains(plan.Excluded[0].Reason, "模型目录") {
		t.Errorf("排除理由要指向模型目录，实际 %q", plan.Excluded[0].Reason)
	}
}

// 目录里那条记录**没配过**能力（六列都是 NULL）时，带工具的请求照样选得到它。
//
// 这是 #485 的核心：`larkaxctl model create` 不勾那六个开关时，库里落的是 NULL
// 而不是 false。落成 false 的话，一条只填了名字的记录会让所有带工具的请求
// 选不到它——哪怕那个上游其实支持工具。上一组用例钉的是另一半（写了 false
// 就是不支持），两条并排才有意义。
func TestUnknownCapabilityDoesNotFilterCandidate(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	model := uniqueModel()
	insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

	// 走写入路径且不给任何能力开关：与 `larkaxctl model create --model …
	// --display …`（不带那六个标志）是同一条路——命令行那一层只把参数搬进
	// 这个请求，判据不重写第二遍。
	if _, err := catalog.NewAdmin(db).Create(context.Background(), catalog.CreateModelRequest{
		CanonicalName: model,
		DisplayName:   model,
	}); err != nil {
		t.Fatalf("建规范模型失败: %v", err)
	}

	got, err := New(db, sealer).Candidates(context.Background(), model)
	if err != nil {
		t.Fatalf("Candidates 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("候选数 = %d，期望 1", len(got))
	}
	p := got[0].Profile
	if p == nil {
		t.Fatal("目录里有这条记录，Profile 不该是 nil")
	}
	if p.Tools != nil || p.Vision != nil || p.Reasoning != nil {
		t.Errorf("没配过的三项该是未知（nil），得到 tools:%s vision:%s reasoning:%s",
			capabilityText(p.Tools), capabilityText(p.Vision), capabilityText(p.Reasoning))
	}

	// 三项都用到的请求：一项都没配过，那就一项都不判——放行。
	plan := (&route.Router{}).Select(got, model,
		route.Needs{Tools: true, Vision: true, Reasoning: true})
	if len(plan.Candidates) != 1 {
		t.Errorf("没配过任何能力的模型应当接得住带工具的请求，排除原因 %+v", plan.Excluded)
	}
}

// BenchmarkCandidatePath 量热路径上这次改动加了多少：目录声明那条查询。
//
// 它不是 CI 的一部分（`go test` 默认不跑基准），改动这条路径时手动跑：
//
//	go test -run XXX -bench BenchmarkCandidatePath -benchtime 200x ./internal/channels/
func BenchmarkCandidatePath(b *testing.B) {
	db := testdb.Open(b)
	key := make([]byte, config.KeySize)
	for i := range key {
		key[i] = 0x42
	}
	sealer, err := credential.NewSealer([]config.MasterKey{{ID: "bench", Key: key}})
	if err != nil {
		b.Fatalf("构造 Sealer 失败: %v", err)
	}

	model := "bench-" + itoa(nextID())
	insertChannelB(b, db, sealer, model)
	insertModelCatalogB(b, db, model)

	src := New(db, sealer)
	ctx := context.Background()

	b.Run("取候选（含目录声明）", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := src.Candidates(ctx, model); err != nil {
				b.Fatalf("Candidates 出错: %v", err)
			}
		}
	})
	b.Run("目录声明", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := src.catalog.Constraints(ctx, model); err != nil {
				b.Fatalf("Constraints 出错: %v", err)
			}
		}
	})
}

func insertChannelB(b *testing.B, db *sql.DB, sealer *credential.Sealer, model string) {
	b.Helper()
	sealed, err := sealer.Seal([]byte("test-key-placeholder"))
	if err != nil {
		b.Fatalf("封装凭证失败: %v", err)
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO channels
			(owner_user_id, name, type, base_url, credential_enc,
			 status, models, weight, priority, setting)
		VALUES (NULL, $1, 'openai-compatible', 'https://api.example.com/v1', $2,
		        'enabled', $3, 1, 0, '{}'::jsonb)`,
		"bench-"+itoa(nextID()), sealed, []string{model})
	if err != nil {
		b.Fatalf("插入渠道失败: %v", err)
	}
}

func insertModelCatalogB(b *testing.B, db *sql.DB, model string) {
	b.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO model_catalog
			(canonical_name, display_name, supports_tools, supports_vision,
			 supports_reasoning, context_limit, output_limit)
		VALUES ($1, $1, true, true, true, 200000, 8192)`, model)
	if err != nil {
		b.Fatalf("插入规范模型失败: %v", err)
	}
}

// 目录里声明的端点清单要**原样**落到候选上，三态一个不少。
//
// 这是选路那侧端点评据的一半输入（另一半是候选自己那条端点，见
// channels_test.go 的 TestCandidateEndpointFromChannelType）。投影里折平任意
// 两档都会让「没配过」与「一条都不支持」重新合成一个值，而它们的结论相反
// ——那种错不会报错，只会让一批模型静默地选不到或选得到。
func TestCandidatesCarriesDeclaredEndpoints(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	cases := []struct {
		name     string
		seed     func(model string)
		want     []channel.Endpoint
		wantProf bool // 期望候选上有一个非 nil 的 Profile
	}{
		{
			// 目录里没有这个模型：Profile 为 nil（没有依据），而不是
			// 一份「六项都没配过」的空声明。
			name: "目录里没有这个模型",
			seed: func(string) {},
		},
		{
			name:     "端点那一项没配过",
			seed:     func(model string) { seedCatalogEndpoints(t, db, model, nil) },
			wantProf: true,
		},
		{
			name:     "明确一条都不支持",
			seed:     func(model string) { seedCatalogEndpoints(t, db, model, []channel.Endpoint{}) },
			want:     []channel.Endpoint{},
			wantProf: true,
		},
		{
			name: "声明了两条",
			seed: func(model string) {
				seedCatalogEndpoints(t, db, model, []channel.Endpoint{
					channel.EndpointChatCompletions, channel.EndpointMessages})
			},
			want:     []channel.Endpoint{channel.EndpointChatCompletions, channel.EndpointMessages},
			wantProf: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := uniqueModel()
			tc.seed(model)
			insertChannel(t.Context(), t, db, sealer, channelSpec{model: model})

			// ctx 在这个用例里内联着写：insertChannel 是夹具，它自己起一个
			// context.Background()，本文件其余用例也是这么用的（contextcheck
			// 会因为作用域里有一个 ctx 而要求把夹具也改成收 ctx——那要动它
			// 几十个调用点，换不来任何东西）。
			got, err := New(db, sealer).Candidates(context.Background(), model)
			if err != nil {
				t.Fatalf("Candidates 出错: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("候选数 = %d，期望 1", len(got))
			}
			profile := got[0].Profile
			if !tc.wantProf {
				if profile != nil {
					t.Fatalf("目录里没有这个模型时 Profile 应为 nil，得到 %+v", profile)
				}
				return
			}
			if profile == nil {
				t.Fatal("目录里有这条记录时 Profile 不该是 nil")
			}
			if !slices.Equal(profile.Endpoints, tc.want) {
				t.Errorf("Endpoints = %v，期望 %v", profile.Endpoints, tc.want)
			}
			// nil 与空切片必须分得开：两者在 route 的端点评据里处置相反
			// （放行 vs 出局）。
			if (tc.want == nil) != (profile.Endpoints == nil) {
				t.Errorf("nil 与空切片被混了：期望 %v，得到 %v", tc.want, profile.Endpoints)
			}
		})
	}
}

// ── 健康读数与恢复 ──────────────────────────────────────────────────────
//
// 这一组测的是**只有真库才能证明**的几件事：窗口筛选真的按 bucket 比大小、
// 分位数真的取的是最近一个非空桶、NULLIF 真的把「没记过成本」变成 NULL、
// 恢复真的在一个事务里把三处写完。拿内存假装这些，测的是「我以为 SQL 会怎么做」。

// seedProbeAt 把一行额度状态的「最近一次探测时刻」写上。
//
// insertQuota 不写这一列（那一批用例关心的是余量与可信度），而健康读数要把它
// 打出来——探测过与没探测过在输出上分得开，正是这一列要证明的事。
func seedProbeAt(t *testing.T, db *sql.DB, channelID int64, at time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE channel_quota_state SET last_probe_at = $2 WHERE channel_id = $1`, channelID, at)
	if err != nil {
		t.Fatalf("写渠道 %d 的探测时刻失败: %v", channelID, err)
	}
}

// TestHealthReadsWindowAndSignals 钉住健康读数的三件事：窗口筛选、分位数的口径、
// 以及「没有额度状态行」时的兜底。
func TestHealthReadsWindowAndSignals(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()
	admin := NewAdmin(db, nil, nil)

	id := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
	// 两行相隔 30 小时的桶：窗口一收，旧的那一行就不该再进汇总。
	// 缓存 token 两桶给不同的值：汇总里读到的是不是**窗口内那几桶之和**，
	// 靠它们分得出来（分位数那一栏做不到这件事，它取的是最近一个非空桶）。
	// 延迟两列按写入侧的样子成对给：同一桶里同时有值，且 p50 < p95。
	insertRollupSignals(ctx, t, db, id, hourBucket(1), 10, 9, 1, 120, 200, "1.50000000", 300, 700)
	insertRollupSignals(ctx, t, db, id, hourBucket(30), 100, 50, 50, 400, 900, "9.00000000", 1000, 1000)
	insertQuota(t, db, id, "12.5", "tokens", "api_query", "degraded", 0.9)
	probeAt := hourBucket(2)
	seedProbeAt(t, db, id, probeAt)

	t.Run("24 小时的窗口只算得进最近那一桶", func(t *testing.T) {
		got, buckets, err := admin.HealthOne(ctx, id, time.Now().Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("HealthOne 失败: %v", err)
		}
		if got.Health != "degraded" {
			t.Errorf("health = %q，期望 degraded", got.Health)
		}
		if got.LastProbeAt == nil || !got.LastProbeAt.Equal(probeAt) {
			t.Errorf("最近探测时刻 = %v，期望 %v", got.LastProbeAt, probeAt)
		}
		if got.Requests != 10 || got.Successes != 9 || got.Failures != 1 {
			t.Errorf("窗口内的计数 = %d/%d/%d，期望 10/9/1",
				got.Requests, got.Successes, got.Failures)
		}
		rate := got.FailureRate()
		if rate == nil || *rate != 0.1 {
			t.Errorf("失败率 = %v，期望 0.1", rate)
		}
		// 分位数取最近一个非空桶（那一桶是 120 / 200），不是最差的那一个（400 / 900）。
		if got.LatencyP50ms == nil || *got.LatencyP50ms != 120 {
			t.Errorf("p50 = %s，期望最近那一桶的 120", showPtr(got.LatencyP50ms))
		}
		if got.LatencyP95ms == nil || *got.LatencyP95ms != 200 {
			t.Errorf("p95 = %s，期望最近那一桶的 200", showPtr(got.LatencyP95ms))
		}
		// 两个分位数出自同一个桶、同一个样本集：中位数不可能大于 95 分位。
		// 这一行同时钉住 p50 有写入方——它接上写入方之前，这里断言的是
		// 「p50 必须是 nil」，于是谁把那一列接上都会先撞在这条用例上。
		if got.LatencyP50ms != nil && got.LatencyP95ms != nil && *got.LatencyP50ms > *got.LatencyP95ms {
			t.Errorf("p50 = %d 大于 p95 = %d，两个分位数写反了",
				*got.LatencyP50ms, *got.LatencyP95ms)
		}
		if got.Cost == nil || *got.Cost != "1.50000000" {
			t.Errorf("成本 = %v，期望 1.50000000", got.Cost)
		}
		// 缓存 token 是窗口内之和：只有最近那一桶（30 小时前那一桶在窗口外）。
		if got.CacheHitTokens != 300 || got.CacheMissTokens != 700 {
			t.Errorf("窗口内的缓存 token = %d/%d，期望 300/700",
				got.CacheHitTokens, got.CacheMissTokens)
		}
		if len(buckets) != 1 {
			t.Fatalf("明细 %d 行，期望只剩窗口内的那一行", len(buckets))
		}
		if !buckets[0].Bucket.Equal(hourBucket(1)) {
			t.Errorf("明细那一行的桶 = %v，期望 %v", buckets[0].Bucket, hourBucket(1))
		}
		if buckets[0].CacheHitTokens != 300 || buckets[0].CacheMissTokens != 700 {
			t.Errorf("明细那一行的缓存 token = %d/%d，期望 300/700",
				buckets[0].CacheHitTokens, buckets[0].CacheMissTokens)
		}
	})

	t.Run("窗口放宽到 40 小时时两桶都算进去", func(t *testing.T) {
		got, buckets, err := admin.HealthOne(ctx, id, time.Now().Add(-40*time.Hour))
		if err != nil {
			t.Fatalf("HealthOne 失败: %v", err)
		}
		if got.Requests != 110 || got.Failures != 51 {
			t.Errorf("窗口内的计数 = %d/%d，期望 110/51", got.Requests, got.Failures)
		}
		if got.Cost == nil || *got.Cost != "10.50000000" {
			t.Errorf("成本 = %v，期望两桶之和 10.50000000", got.Cost)
		}
		if got.CacheHitTokens != 1300 || got.CacheMissTokens != 1700 {
			t.Errorf("窗口内的缓存 token = %d/%d，期望两桶之和 1300/1700",
				got.CacheHitTokens, got.CacheMissTokens)
		}
		// 最近的一行在最上面：这条命令回答的是「它最近怎么了」。
		if len(buckets) != 2 {
			t.Fatalf("明细 %d 行，期望 2 行", len(buckets))
		}
		if !buckets[0].Bucket.After(buckets[1].Bucket) {
			t.Errorf("明细没有按时刻倒序：%v 在 %v 之前", buckets[0].Bucket, buckets[1].Bucket)
		}
		// 每一行各自的分位数都是**那一桶自己的**数，不是汇总行的。
		if buckets[0].LatencyP50ms == nil || *buckets[0].LatencyP50ms != 120 ||
			buckets[0].LatencyP95ms == nil || *buckets[0].LatencyP95ms != 200 {
			t.Errorf("最近那一行的分位数 = %s / %s，期望 120 / 200",
				showPtr(buckets[0].LatencyP50ms), showPtr(buckets[0].LatencyP95ms))
		}
		if buckets[1].LatencyP50ms == nil || *buckets[1].LatencyP50ms != 400 ||
			buckets[1].LatencyP95ms == nil || *buckets[1].LatencyP95ms != 900 {
			t.Errorf("旧的那一行的分位数 = %s / %s，期望 400 / 900",
				showPtr(buckets[1].LatencyP50ms), showPtr(buckets[1].LatencyP95ms))
		}
	})

	t.Run("没有额度状态行的渠道是未知，不是最差", func(t *testing.T) {
		bare := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
		got, buckets, err := admin.HealthOne(ctx, bare, time.Now().Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("HealthOne 失败: %v", err)
		}
		if got.Health != HealthUnknown {
			t.Errorf("health = %q，期望 %q", got.Health, HealthUnknown)
		}
		if got.LastProbeAt != nil {
			t.Errorf("从来没探测过时最近探测时刻应当是 nil，得到 %v", got.LastProbeAt)
		}
		if got.Requests != 0 || got.Failures != 0 {
			t.Errorf("没有桶时计数应当是 0，得到 %d/%d", got.Requests, got.Failures)
		}
		// 0/0 不是一个百分数：没有请求时失败率必须是「没有数据」。
		if rate := got.FailureRate(); rate != nil {
			t.Errorf("没有请求时失败率应当是 nil，得到 %v", *rate)
		}
		if got.LatencyP50ms != nil || got.LatencyP95ms != nil || got.Cost != nil {
			t.Errorf("没有桶时两个分位数与成本都应当是 nil，得到 %v / %v / %v",
				showPtr(got.LatencyP50ms), showPtr(got.LatencyP95ms), showPtr(got.Cost))
		}
		// 缓存 token 没有「没有数据」那一档：没有桶时是 0，与选路侧读 0 的口径一致。
		if got.CacheHitTokens != 0 || got.CacheMissTokens != 0 {
			t.Errorf("没有桶时缓存 token 应当是 0，得到 %d/%d",
				got.CacheHitTokens, got.CacheMissTokens)
		}
		if len(buckets) != 0 {
			t.Errorf("没有桶时明细应当是空的，得到 %d 行", len(buckets))
		}
	})

	t.Run("延迟那一列的 - 只出现在真的没有样本的桶上", func(t *testing.T) {
		// 有请求、有桶，但那一小时一条耗时样本都没有：两个分位数都读作
		// 「没有数据」。跑过流量的桶会读出分位数（见上面那条用例），于是
		// 「-」与「这一列没人接」这两件事在读侧分得开。
		noSamples := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
		insertRollupSignals(ctx, t, db, noSamples, hourBucket(1), 7, 7, 0, nil, nil, "0", 0, 0)

		// 对照：同一张表、同一个窗口，只有它那一桶有样本。少了它，读侧
		// 「整列都返回 nil」也会让上面那条通过。
		withSamples := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
		insertRollupSignals(ctx, t, db, withSamples, hourBucket(1), 7, 7, 0, 15, 30, "0", 40, 60)

		got, _, err := admin.HealthOne(ctx, noSamples, time.Now().Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("HealthOne 失败: %v", err)
		}
		if got.Requests != 7 {
			t.Errorf("请求数 = %d，期望 7（没有样本不等于没有请求）", got.Requests)
		}
		if got.LatencyP50ms != nil || got.LatencyP95ms != nil {
			t.Errorf("没有耗时样本时两个分位数都应当是 nil，得到 %v / %v",
				showPtr(got.LatencyP50ms), showPtr(got.LatencyP95ms))
		}

		control, _, err := admin.HealthOne(ctx, withSamples, time.Now().Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("HealthOne 失败: %v", err)
		}
		if control.LatencyP50ms == nil || *control.LatencyP50ms != 15 {
			t.Errorf("有样本时 p50 = %s，期望 15", showPtr(control.LatencyP50ms))
		}
		if control.LatencyP95ms == nil || *control.LatencyP95ms != 30 {
			t.Errorf("有样本时 p95 = %s，期望 30", showPtr(control.LatencyP95ms))
		}
	})

	t.Run("两列各自取自己最近一个有值的桶", func(t *testing.T) {
		// 这一列接上写入方之前留下的旧桶：p95 有值、p50 是 NULL。读侧对两列
		// 各取**自己**最近一个非空桶（见 health.go 的 healthFrom），所以 p50
		// 退到更早的那一小时，而不是跟着最近那一桶一起变成 -。
		//
		// 新写的桶里两列同时有值（见 record.go 的 rollupSignalsSQL），能造出
		// 这种行的只有旧数据——但它在每个已有流量的库里都存在，而「退到更早
		// 那一桶」正是这条命令对 `--window` 的既有承诺。
		legacy := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
		insertRollupSignals(ctx, t, db, legacy, hourBucket(1), 3, 3, 0, nil, 80, "0", 0, 0)
		insertRollupSignals(ctx, t, db, legacy, hourBucket(3), 4, 4, 0, 25, 60, "0", 0, 0)

		got, _, err := admin.HealthOne(ctx, legacy, time.Now().Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("HealthOne 失败: %v", err)
		}
		if got.LatencyP50ms == nil || *got.LatencyP50ms != 25 {
			t.Errorf("p50 = %s，期望退到更早那一桶的 25", showPtr(got.LatencyP50ms))
		}
		if got.LatencyP95ms == nil || *got.LatencyP95ms != 80 {
			t.Errorf("p95 = %s，期望最近那一桶的 80", showPtr(got.LatencyP95ms))
		}
	})

	t.Run("成本那一列的 0 读作没有数据", func(t *testing.T) {
		// 写入侧接上之前那一列是 NOT NULL DEFAULT 0，窗口里可能还留着那时
		// 写下的 0；而接上之后「合计为 0」也可能是真的免费。读侧两种都判成
		// 「没有数据」：把它当免费会让每一行都显示一个凭空的零成本。
		zero := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
		insertRollup(ctx, t, db, zero, hourBucket(1), 5, 5, 0, 120, "0.00000000")

		got, _, err := admin.HealthOne(ctx, zero, time.Now().Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("HealthOne 失败: %v", err)
		}
		if got.Cost != nil {
			t.Errorf("成本是 0 时应当读作没有数据，得到 %q", *got.Cost)
		}
		if got.Requests != 5 {
			t.Errorf("计数仍然要读出来：请求数 = %d，期望 5", got.Requests)
		}
	})

	t.Run("找不到那条渠道", func(t *testing.T) {
		if _, _, err := admin.HealthOne(ctx, 999999999, time.Now()); !errors.Is(err, ErrChannelNotFound) {
			t.Fatalf("期望 ErrChannelNotFound，得到 %v", err)
		}
	})
}

// TestHealthHidesDeletedUnlessAll 钉住读侧对已软删渠道的那条分工：
// 清单默认不列、--all 才列，而按 id 取一条一律读得出来。
func TestHealthHidesDeletedUnlessAll(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()
	admin := NewAdmin(db, nil, nil)
	since := time.Now().Add(-24 * time.Hour)

	alive := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
	deleted := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
	softDeleteRow(t, db, deleted)

	contains := func(rows []ChannelHealth, id int64) bool {
		for _, h := range rows {
			if h.ID == id {
				return true
			}
		}
		return false
	}

	rows, err := admin.Health(ctx, since, false)
	if err != nil {
		t.Fatalf("Health 失败: %v", err)
	}
	if !contains(rows, alive) {
		t.Error("默认清单里应当有那条没删的渠道")
	}
	if contains(rows, deleted) {
		t.Error("默认清单里不该有已删的渠道")
	}

	rows, err = admin.Health(ctx, since, true)
	if err != nil {
		t.Fatalf("Health --all 失败: %v", err)
	}
	if !contains(rows, deleted) {
		t.Error("ListAll 那一档应当带上已删的渠道")
	}

	// 按 id 取一条：已删的也读得出来，且 DeletedAt 非零（调用方据此标出它）。
	one, _, err := admin.HealthOne(ctx, deleted, since)
	if err != nil {
		t.Fatalf("按 id 读一条已删的渠道失败: %v", err)
	}
	if one.DeletedAt.IsZero() {
		t.Error("已删的那一条应当带上删除时刻")
	}
}

// TestRestoreBringsChannelBack 钉住恢复写的三处，以及「本来就没删」那一档。
func TestRestoreBringsChannelBack(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()
	admin := NewAdmin(db, nil, nil)

	id := insertChannel(ctx, t, db, sealer, channelSpec{model: uniqueModel()})
	// 造一个**不是** unknown 的结论：一条本来就是 unknown 的渠道被恢复之后还是
	// unknown，那样「回落成 unknown」这条断言什么都没钉住。
	insertQuota(t, db, id, "5", "tokens", "api_query", "drained", 0.9)
	// 连续失败计数也造上，证明恢复**不**动它（那个计数自带 15 分钟冷却期）。
	failureAt := time.Now().Add(-time.Minute)
	setStreak(t, db, id, 3, &failureAt)
	envelopeBefore := channelEnvelopeOf(t, db, id)

	softDeleteRow(t, db, id)

	restored, err := admin.Restore(ctx, id)
	if err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}
	if !restored {
		t.Fatal("Restore 应当报「这次才恢复的」")
	}

	var (
		deleted sql.NullTime
		status  string
		streak  int
		health  string
	)
	if scanErr := db.QueryRowContext(ctx,
		`SELECT deleted_at, status, consecutive_failures FROM channels WHERE id = $1`, id).
		Scan(&deleted, &status, &streak); scanErr != nil {
		t.Fatalf("读回渠道行失败: %v", scanErr)
	}
	if deleted.Valid {
		t.Error("恢复之后 deleted_at 应当是 NULL")
	}
	if status != StatusEnabled {
		// 「恢复回可用」是字面意思：选路要求 status = 'enabled'，恢复成 disabled
		// 的渠道会出现在清单上却选不中。
		t.Errorf("恢复之后 status = %q，期望 %q", status, StatusEnabled)
	}
	if streak != 3 {
		t.Errorf("恢复不该动连续失败计数：得到 %d，期望 3", streak)
	}
	if scanErr := db.QueryRowContext(ctx,
		`SELECT health FROM channel_quota_state WHERE channel_id = $1`, id).Scan(&health); scanErr != nil {
		t.Fatalf("读回额度状态失败: %v", scanErr)
	}
	if health != HealthUnknown {
		// 上一次的结论是软删之前的，直接沿用等于把一段已经过去的历史当成现在的判断。
		t.Errorf("恢复之后 health = %q，期望 %q", health, HealthUnknown)
	}
	if got := channelEnvelopeOf(t, db, id); string(got) != string(envelopeBefore) {
		t.Error("恢复不该重新封装凭证密文")
	}

	// 没删的再恢复一次：幂等成功，报 false，不再改写任何一列。
	again, err := admin.Restore(ctx, id)
	if err != nil {
		t.Fatalf("重复 Restore 不该报错，得到 %v", err)
	}
	if again {
		t.Error("本来就没删时 Restore 应当报 false")
	}

	// id 不存在仍然报错——幂等不能让一次打错的调用看起来像成功。
	if _, err := admin.Restore(ctx, 999999999); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("期望 ErrChannelNotFound，得到 %v", err)
	}
}

// channelEnvelopeOf 直接读出一行的凭证密文（清单里没有这一栏）。
func channelEnvelopeOf(t *testing.T, db *sql.DB, id int64) []byte {
	t.Helper()
	var envelope []byte
	if err := db.QueryRowContext(context.Background(),
		`SELECT credential_enc FROM channels WHERE id = $1`, id).Scan(&envelope); err != nil {
		t.Fatalf("读渠道 %d 的密文失败: %v", id, err)
	}
	return envelope
}

// TestOwnedChannelScope 钉住归属过滤的 SQL：列表、读、改三条路径都只认
// owner_user_id = 自己，平台自营（NULL）与别人的行一律看不穿。
//
// 「不属于自己」与「不存在」在 UpdateOwned / GetOwned 上是同一个
// ErrChannelNotFound，所以这里连错误类型一起判——只判非 nil 的话，把越权
// 误报成「已删除」也能过。
func TestOwnedChannelScope(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	sealer := testSealer(t)
	admin := NewAdmin(db, sealer, nil)

	owner := seedMerchant(t, db)
	other := seedMerchant(t, db)
	model := uniqueModel()

	ownID := insertChannel(ctx, t, db, sealer, channelSpec{ownerUserID: owner, model: model})
	otherID := insertChannel(ctx, t, db, sealer, channelSpec{ownerUserID: other, model: model})
	platformID := insertChannel(ctx, t, db, sealer, channelSpec{model: model})

	list, err := admin.ListOwned(ctx, owner)
	if err != nil {
		t.Fatalf("ListOwned 失败: %v", err)
	}
	seen := make(map[int64]bool, len(list))
	for _, info := range list {
		seen[info.ID] = true
	}
	if !seen[ownID] {
		t.Errorf("自己的渠道 %d 不在列表里", ownID)
	}
	if seen[otherID] {
		t.Errorf("列表里出现了别人的渠道 %d", otherID)
	}
	if seen[platformID] {
		t.Errorf("列表里出现了平台渠道 %d", platformID)
	}

	if _, getErr := admin.GetOwned(ctx, owner, ownID); getErr != nil {
		t.Errorf("读自己的渠道失败: %v", getErr)
	}
	for _, id := range []int64{otherID, platformID, 999999999} {
		if _, getErr := admin.GetOwned(ctx, owner, id); !errors.Is(getErr, ErrChannelNotFound) {
			t.Errorf("GetOwned(%d) 的错误 = %v，期望 ErrChannelNotFound", id, getErr)
		}
		if setErr := admin.UpdateOwned(ctx, owner, UpdateRequest{
			ID: id, Priority: func() *int { v := 9; return &v }(),
		}); !errors.Is(setErr, ErrChannelNotFound) {
			t.Errorf("UpdateOwned(%d) 的错误 = %v，期望 ErrChannelNotFound", id, setErr)
		}
	}

	// 自己的可以改。
	newName := "renamed-" + itoa(nextID())
	if setErr := admin.UpdateOwned(ctx, owner, UpdateRequest{ID: ownID, Name: &newName}); setErr != nil {
		t.Fatalf("改自己的渠道失败: %v", setErr)
	}
	got, err := admin.GetOwned(ctx, owner, ownID)
	if err != nil {
		t.Fatalf("改完再读失败: %v", err)
	}
	if got.Name != newName {
		t.Errorf("改完的名字 = %q，期望 %q", got.Name, newName)
	}

	// 别人来改同一条：404，而且确实没变。
	if err := admin.UpdateOwned(ctx, other, UpdateRequest{ID: ownID, Name: &newName}); !errors.Is(err, ErrChannelNotFound) {
		t.Errorf("别人改我的渠道的错误 = %v，期望 ErrChannelNotFound", err)
	}
}

// TestSuppliedModelsOwnedScopesAndCounts 钉住「我的模型」那条读的四件事：
// 归属只看本人、渠道数按渠道条数（同地址多账号各算一条）、姓名按在供判据
// 过滤（停用 / 软删 / 没有这个模型的渠道都不算）、展示名缺省时回落到规范名。
func TestSuppliedModelsOwnedScopesAndCounts(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	sealer := testSealer(t)
	admin := NewAdmin(db, nil, nil)

	owner := seedMerchant(t, db)
	other := seedMerchant(t, db)
	shared := uniqueModel()
	otherOnly := uniqueModel()
	disabledOnly := uniqueModel()
	deletedOnly := uniqueModel()
	for _, name := range []string{shared, otherOnly, disabledOnly, deletedOnly} {
		insertModelCatalog(t, db, name, true, true, true, 128000, 8192)
	}
	// 展示名为空时清单上不该出现一个空白的名字。
	if _, err := db.ExecContext(ctx,
		`UPDATE model_catalog SET display_name = '' WHERE canonical_name = $1`,
		otherOnly); err != nil {
		t.Fatalf("清空展示名失败: %v", err)
	}

	// 同一位商家的两条渠道在供同一个模型：各算一条（不同账号）。
	insertChannel(ctx, t, db, sealer, channelSpec{ownerUserID: owner, model: shared})
	insertChannel(ctx, t, db, sealer, channelSpec{ownerUserID: owner, model: shared})
	// 别人的渠道在供 shared 与 otherOnly：它们不该进本人的清单。
	insertChannel(ctx, t, db, sealer, channelSpec{ownerUserID: other, model: shared})
	insertChannel(ctx, t, db, sealer, channelSpec{ownerUserID: other, model: otherOnly})
	// 停用与软删的渠道即便挂着模型也不算在供。
	insertChannel(ctx, t, db, sealer, channelSpec{
		ownerUserID: owner, model: disabledOnly, status: "disabled",
	})
	deleted := insertChannel(ctx, t, db, sealer, channelSpec{
		ownerUserID: owner, model: deletedOnly,
	})
	if _, err := db.ExecContext(ctx,
		`UPDATE channels SET deleted_at = now() WHERE id = $1`, deleted); err != nil {
		t.Fatalf("软删渠道失败: %v", err)
	}

	own, err := admin.SuppliedModelsOwned(ctx, &owner)
	if err != nil {
		t.Fatalf("读本人的在供模型失败: %v", err)
	}
	if len(own) != 1 {
		t.Fatalf("本人的在供模型 = %+v，期望只有 %q 一条", own, shared)
	}
	if own[0].Model != shared || own[0].Channels != 2 {
		t.Errorf("本人的清单 = %+v，期望 %q 两条渠道", own[0], shared)
	}
	if own[0].DisplayName != shared {
		t.Errorf("展示名 = %q，期望规范名 %q", own[0].DisplayName, shared)
	}

	all, err := admin.SuppliedModelsOwned(ctx, nil)
	if err != nil {
		t.Fatalf("读全局在供模型失败: %v", err)
	}
	byName := make(map[string]SuppliedModel, len(all))
	names := make([]string, 0, len(all))
	for _, m := range all {
		byName[m.Model] = m
		names = append(names, m.Model)
	}
	if got := byName[shared]; got.Channels != 3 {
		t.Errorf("全局 %q 的渠道数 = %d，期望 3（本人 2 + 别人 1）", shared, got.Channels)
	}
	if got, ok := byName[otherOnly]; !ok {
		t.Errorf("全局清单里没有 %q：%+v", otherOnly, all)
	} else if got.DisplayName != otherOnly {
		t.Errorf("%q 的展示名 = %q，期望回落到规范名", otherOnly, got.DisplayName)
	}
	for _, absent := range []string{disabledOnly, deletedOnly} {
		if _, ok := byName[absent]; ok {
			t.Errorf("全局清单里出现了不该在供的 %q", absent)
		}
	}
	if !slices.IsSorted(names) {
		t.Errorf("清单没有按规范名升序：%v", names)
	}

	zero := int64(0)
	if _, err := admin.SuppliedModelsOwned(ctx, &zero); err == nil {
		t.Error("ownerID 为 0 应当报错")
	}
}
