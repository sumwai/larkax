package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
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

	"github.com/sumwai/larkax/internal/migrate"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 这一层测的是 SQL，不是映射
//
// 本包的判断几乎全落在 SQL 上：偏唯一索引拦不拦得住、外键认不认一个
// 不存在的规范名、jsonb 与 date 写进去再读出来还是不是同一个值。
// 拿内存假装这些，测的是「我以为数据库会怎么做」。
//
// 没有 LARKAX_DB_DSN 时跳过而不是失败：本机未必起着库。
// **注意「跳过」与「通过」在输出里长得一样**，CI 里另有一道门禁确认
// 这个包真的跑了（见 .github/actions/check-integration-tests）。
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

// uniqueName 给规范名与上游名加本次运行的唯一后缀。
//
// 平台级别名与 Model 的清单是**全局**的（channel_id IS NULL 的那些行不分渠道），
// 共用一个库时，别的用例留下的行会混进结果里。所以断言一律按名字找，
// 不数总数。
func uniqueName(prefix string) string { return prefix + "-" + itoa(nextID()) }

// insertChannel 直接插一条渠道行并返回 id。
//
// 不走 channels.Admin：这里要的只是 model_aliases 那条外键指向的一行，
// 而那个管理面需要主密钥与适配器注册表——它们与本包要测的东西无关。
// 凭证密文用 `'\x00'::bytea` 占位：本包从不读它。
func insertChannel(t *testing.T, db *sql.DB, model string) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO channels (name, type, base_url, credential_enc, models)
		VALUES ($1, 'openai-compatible', 'https://api.example.com/v1', '\x00'::bytea, ARRAY[$2])
		RETURNING id`, "ch-"+itoa(nextID()), model).Scan(&id)
	if err != nil {
		t.Fatalf("插入渠道失败: %v", err)
	}
	return id
}

// insertAlias 插一条别名。channelID 传 nil 表示平台级别名。
func insertAlias(t *testing.T, db *sql.DB, channelID any, upstream, canonical string) {
	t.Helper()
	if err := insertAliasErr(db, channelID, upstream, canonical); err != nil {
		t.Fatalf("插入别名失败: %v", err)
	}
}

// insertAliasErr 与 insertAlias 相同，但把错误交回调用方。
//
// 「写入被拦下」正是下面几条用例要断言的东西，所以不能在这里 t.Fatalf。
func insertAliasErr(db *sql.DB, channelID any, upstream, canonical string) error {
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
		VALUES ($1, $2, $3)`, channelID, upstream, canonical)
	return err
}

// insertPricingRule 插一条按规范名精确匹配的计价规则。
//
// 表达式本身是假的（`1`）：本包不看它，要测的是「这个名字还挂在一条规则上」。
//
// model_patterns 收窄到这一个模型名，用完再删：pricing_rules 是全局表，而测试库
// 由同包与别的包的用例共用（`go test ./...` 并行跑包）。一条不带 model_patterns
// 的平台规则匹配**任何**模型，会让「一条规则都匹配不上」这个状态在整库范围内
// 消失，而那条分支是别的用例要测的（见 internal/pricing 的
// TestMatchReturnsErrNoRule）。
func insertPricingRule(t *testing.T, db *sql.DB, canonicalName string) {
	t.Helper()

	patterns, err := json.Marshal([]string{canonicalName})
	if err != nil {
		t.Fatalf("编码 model_patterns 失败: %v", err)
	}
	var id int64
	err = db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules (scope, name, expr, model_patterns)
		VALUES ('platform', $1, '1', $2::jsonb) RETURNING id`,
		"rule-"+itoa(nextID()), string(patterns)).Scan(&id)
	if err != nil {
		t.Fatalf("插入计价规则失败: %v", err)
	}

	// 清理失败必须报出来：被忽略的失败与清理成功在输出里长得一样，而这一条
	// 清理掉队就会让规则在共享的库里累积。本包不产生指向 pricing_rules 的账单行
	// （billing_requests.pricing_rule_id 是指向它的外键，且没有 ON DELETE），
	// 所以删得掉。
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM pricing_rules WHERE id = $1`, id); err != nil {
			t.Errorf("清理计价规则失败: %v", err)
		}
	})
}

// findAlias 按上游名从清单里找一条，找不到就失败。
//
// 按名字找而不是按下标取：平台级别名的清单是全局的，共用一个库时
// 别的用例的行也在里面。
func findAlias(t *testing.T, aliases []Alias, upstream string) Alias {
	t.Helper()
	for _, alias := range aliases {
		if alias.UpstreamModel == upstream {
			return alias
		}
	}
	t.Fatalf("清单里没有上游名 %q（共 %d 条）", upstream, len(aliases))
	return Alias{}
}

// assertViolation 断言这次写入被**指定的那条约束**拦下了，并且错误链上带着
// 调用方要判断的那个分类错误。
//
// 只断言「报错了」不够：连不上库、外键不存在、列名写错都会报错，
// 而它们与「重复被拦下」是两回事。所以既认 SQLSTATE，也认约束名——
// 约束名才说明是这条规则在起作用。
//
// wantSentinel 传 nil 表示这次不检查分类（直接走 SQL 的那几条用例，
// 那里没有经过本包的翻译层）。
func assertViolation(t *testing.T, err error, wantCode, wantConstraint string, wantSentinel error) {
	t.Helper()

	if err == nil {
		t.Fatalf("应当被 %s 拦下，实际写进去了", wantConstraint)
	}
	if wantSentinel != nil && !errors.Is(err, wantSentinel) {
		t.Errorf("err = %v，期望 errors.Is 到 %v", err, wantSentinel)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v，期望错误链上有 *pgconn.PgError", err)
	}
	if pgErr.Code != wantCode {
		t.Errorf("SQLSTATE = %s（%s），期望 %s", pgErr.Code, pgErr.Message, wantCode)
	}
	if pgErr.ConstraintName != wantConstraint {
		t.Errorf("被违反的约束是 %q，期望 %q", pgErr.ConstraintName, wantConstraint)
	}
}

// ── 选路要的那条读取路径（从 internal/channels 搬来） ────────────────────

func TestUpstreamModelPrefersChannelSpecific(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)

	// 平台级别名对所有渠道生效，渠道级的应该盖过它。
	insertAlias(t, db, nil, "platform-"+model, model)
	insertAlias(t, db, channel, "channel-"+model, model)

	got, err := New(db).UpstreamModel(context.Background(), channel, model)
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
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	insertAlias(t, db, nil, "platform-"+model, model)

	got, err := New(db).UpstreamModel(context.Background(), channel, model)
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
	model := uniqueName("model")
	mine := insertChannel(t, db, model)
	other := insertChannel(t, db, uniqueName("model"))
	seedModel(t, db, model)
	insertAlias(t, db, other, "other-"+model, model)

	_, err := New(db).UpstreamModel(context.Background(), mine, model)
	if !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("err = %v，期望 ErrAliasNotFound", err)
	}
}

func TestUpstreamModelNotFound(t *testing.T) {
	db := testDB(t)

	_, err := New(db).UpstreamModel(context.Background(), 1, uniqueName("nonexistent"))
	if !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("err = %v，期望 ErrAliasNotFound", err)
	}
}

// channelID 传 0 是「不针对具体渠道」，只走平台级别名那条。
func TestUpstreamModelChannelZeroUsesPlatformAlias(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	seedModel(t, db, model)
	insertAlias(t, db, nil, "platform-"+model, model)

	got, err := New(db).UpstreamModel(context.Background(), 0, model)
	if err != nil {
		t.Fatalf("UpstreamModel 出错: %v", err)
	}
	if got != "platform-"+model {
		t.Errorf("UpstreamModel = %q，期望平台级别名", got)
	}
}

// ── 数据库层的约束（与写入路径无关，直接走 SQL） ────────────────────────

// 两条偏索引的名字（aliasChannelCanonicalUnique 等）定义在 admin.go 里，
// 它们是写入路径翻译错误时要认的东西——所以下面的断言与生产代码认的是
// **同一个常量**，改名时不会有一边忘了改。它们硬编码自
// 00007_model_alias_unique.sql，失效的方向是失败，不是静默通过。

// 约束所在的迁移文件，也是「已有重复数据会被拒绝」那条用例要重放的那一份。
const aliasUniqueMigration = "00007_model_alias_unique.sql"

// 同一个渠道下两个上游名映射到同一个规范名：写入侧就该被拦下。
//
// 这正是那条歧义——约束建立之前这种数据能写进来，而查询侧只能按
// 上游名排序取第一条，选错就是差价。
func TestAliasRejectsChannelDuplicateCanonical(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)

	insertAlias(t, db, channel, "up-a-"+model, model)
	err := insertAliasErr(db, channel, "up-b-"+model, model)
	assertViolation(t, err, pgUniqueViolation, aliasChannelCanonicalUnique, nil)
}

// 平台级那一批要单独一条偏索引才拦得住：SQL 里 NULL 不等于 NULL，
// 普通唯一键对 channel_id IS NULL 的行一行都不管。
func TestAliasRejectsPlatformDuplicateCanonical(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	seedModel(t, db, model)

	insertAlias(t, db, nil, "up-a-"+model, model)
	err := insertAliasErr(db, nil, "up-b-"+model, model)
	assertViolation(t, err, pgUniqueViolation, aliasPlatformCanonicalUnique, nil)
}

// 反方向——一个上游名对应两个规范名——原本就被 (channel_id, upstream_model)
// 拦着。这条用例把那个约束也钉住：删掉它，镜像的歧义会重新出现。
func TestAliasRejectsDuplicateUpstreamModel(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	other := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	seedModel(t, db, other)

	insertAlias(t, db, channel, "up-"+model, model)
	err := insertAliasErr(db, channel, "up-"+model, other)
	assertViolation(t, err, pgUniqueViolation, "model_aliases_channel_id_upstream_model_key", nil)
}

// 约束只该拦「同一个范围里的同一个规范名」，别的一律不受影响——
// 拦宽了的话，「两个渠道各配一个上游名指向同一个规范名」这种正常配置
// 就会写不进去。
//
// 这条同时是查询侧简化的前提：允许写进去的这几条里，同一个范围下指向 model
// 的**各自只有一条**，所以 UpstreamModel 命中什么是确定的。
func TestAliasAllowsDistinctScopesAndNames(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	other := uniqueName("model")
	mine := insertChannel(t, db, model)
	theirs := insertChannel(t, db, model)
	seedModel(t, db, model)
	seedModel(t, db, other)

	insertAlias(t, db, mine, "up-a-"+model, model)
	insertAlias(t, db, theirs, "up-b-"+model, model) // 另一个渠道：允许
	insertAlias(t, db, mine, "up-c-"+model, other)   // 同一渠道、另一个规范名：允许
	insertAlias(t, db, nil, "up-d-"+model, model)    // 平台级与渠道级并存：允许
	insertAlias(t, db, nil, "up-e-"+model, other)    // 平台级的另一个规范名：允许

	got, err := New(db).UpstreamModel(context.Background(), mine, model)
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
	model := uniqueName("model")
	channel := insertChannel(t, db, model)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, idx := range []string{aliasChannelCanonicalUnique, aliasPlatformCanonicalUnique} {
		if _, err = tx.ExecContext(ctx, `DROP INDEX `+idx); err != nil {
			t.Fatalf("删掉索引 %s 失败: %v", idx, err)
		}
	}

	// 两类重复各造一组：同一个规范名在同一个渠道下有两个上游名，
	// 以及平台级下同样的一对。
	if _, err = tx.ExecContext(ctx,
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
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
			VALUES ($1, $2, $3)`, dup.channelID, dup.upstream, model); err != nil {
			t.Fatalf("造重复数据失败: %v", err)
		}
	}

	// 迁移报错之后整个事务会被 PostgreSQL 置为 aborted，后面什么都查不了，
	// 所以先打一个保存点：报错后回到这里，事务恢复可用，造的重复数据还在。
	if _, err = tx.ExecContext(ctx, `SAVEPOINT before_migration`); err != nil {
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

// ── 规范模型的写入路径 ──────────────────────────────────────────────────

// seedModel 直接插一条规范模型。**只用于别的用例的前置数据**：
// 写入路径本身的用例走 Admin.Create，这样「写进去的读得出来」才有意义。
func seedModel(t *testing.T, db *sql.DB, canonical string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO model_catalog (canonical_name, display_name) VALUES ($1, $1)`,
		canonical); err != nil {
		t.Fatalf("插入模型目录失败: %v", err)
	}
}

// 写进去的每一个字段都要读得回来：写入与读取对同一套列的假设是否一致，
// 只有「写一遍再读一遍」能证明。少扫一列、多包一层 jsonb、日期差一天，
// 都会在这里露出来。
func TestCreateModelRoundTrip(t *testing.T) {
	db := testDB(t)
	name := uniqueName("model")
	cutoff := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	released := time.Date(2025, 2, 3, 0, 0, 0, 0, time.UTC)

	id, err := NewAdmin(db).Create(context.Background(), CreateModelRequest{
		CanonicalName: name,
		DisplayName:   "显示名 " + name,
		Vendor:        "acme",
		Family:        "acme-1",
		ContextLimit:  200000,
		InputLimit:    180000,
		OutputLimit:   8192,
		Capabilities: Capabilities{
			Reasoning: true, Tools: true, Vision: false, Audio: false, Cache: true, Batch: false,
		},
		ModalitiesInput:  []string{"text", "image"},
		ModalitiesOutput: []string{"text"},
		ReasoningOptions: []any{
			map[string]any{"type": "effort", "values": []any{"low", "high"}},
		},
		CanonicalModelID: "acme/1",
		KnowledgeCutoff:  &cutoff,
		ReleaseDate:      &released,
		Raw:              map[string]any{"source": "fixture", "n": float64(3)},
	})
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Create 返回的 id = %d，期望正数", id)
	}

	got, err := New(db).Model(context.Background(), name)
	if err != nil {
		t.Fatalf("读回规范模型失败: %v", err)
	}

	if got.ID != id {
		t.Errorf("ID = %d，期望 %d", got.ID, id)
	}
	if got.DisplayName != "显示名 "+name {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.Vendor != "acme" || got.Family != "acme-1" {
		t.Errorf("Vendor/Family = %q/%q", got.Vendor, got.Family)
	}
	if got.ContextLimit != 200000 || got.InputLimit != 180000 || got.OutputLimit != 8192 {
		t.Errorf("limits = %d/%d/%d", got.ContextLimit, got.InputLimit, got.OutputLimit)
	}
	wantCaps := Capabilities{Reasoning: true, Tools: true, Cache: true}
	if got.Capabilities != wantCaps {
		t.Errorf("Capabilities = %+v，期望 %+v", got.Capabilities, wantCaps)
	}
	if strings.Join(got.ModalitiesInput, ",") != "text,image" {
		t.Errorf("ModalitiesInput = %v", got.ModalitiesInput)
	}
	if strings.Join(got.ModalitiesOutput, ",") != "text" {
		t.Errorf("ModalitiesOutput = %v", got.ModalitiesOutput)
	}
	// reasoning_options 原样存取：写进去的是数组，读出来也必须是数组，
	// 不能是一个被当成字符串的 JSON。
	if len(got.ReasoningOptions) != 1 {
		t.Fatalf("ReasoningOptions = %v，期望 1 项", got.ReasoningOptions)
	}
	option, ok := got.ReasoningOptions[0].(map[string]any)
	if !ok || option["type"] != "effort" {
		t.Errorf("ReasoningOptions[0] = %v，期望 {type: effort}", got.ReasoningOptions[0])
	}
	if got.CanonicalModelID != "acme/1" {
		t.Errorf("CanonicalModelID = %q", got.CanonicalModelID)
	}
	if got.KnowledgeCutoff == nil || !got.KnowledgeCutoff.Equal(cutoff) {
		t.Errorf("KnowledgeCutoff = %v，期望 %v", got.KnowledgeCutoff, cutoff)
	}
	if got.ReleaseDate == nil || !got.ReleaseDate.Equal(released) {
		t.Errorf("ReleaseDate = %v，期望 %v", got.ReleaseDate, released)
	}
	if got.Raw["source"] != "fixture" {
		t.Errorf("Raw = %v，期望解回对象", got.Raw)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt 为零值：那一列有默认值，读出来不该是零")
	}
}

// 没填的字段要落成 NULL 而不是零值：读取侧把 NULL 折成 0 与空串，
// 存 0 会让「没填」看起来像一个测出来的值。
func TestCreateModelLeavesUnknownFieldsNull(t *testing.T) {
	db := testDB(t)
	name := uniqueName("model")

	if _, err := NewAdmin(db).Create(context.Background(), CreateModelRequest{
		CanonicalName: name,
		DisplayName:   name,
	}); err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	var (
		vendor       sql.NullString
		contextLimit sql.NullInt64
		cutoff       sql.NullTime
		raw          []byte
	)
	err := db.QueryRowContext(context.Background(), `
		SELECT vendor, context_limit, knowledge_cutoff, raw
		  FROM model_catalog WHERE canonical_name = $1`, name).
		Scan(&vendor, &contextLimit, &cutoff, &raw)
	if err != nil {
		t.Fatalf("回查失败: %v", err)
	}
	if vendor.Valid {
		t.Errorf("vendor = %q，期望 NULL", vendor.String)
	}
	if contextLimit.Valid {
		t.Errorf("context_limit = %d，期望 NULL", contextLimit.Int64)
	}
	if cutoff.Valid {
		t.Errorf("knowledge_cutoff = %v，期望 NULL", cutoff.Time)
	}
	if string(raw) != "{}" {
		t.Errorf("raw = %s，期望 {}", raw)
	}

	got, err := New(db).Model(context.Background(), name)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Vendor != "" || got.ContextLimit != 0 || got.KnowledgeCutoff != nil {
		t.Errorf("读回 = %+v，期望空值", got)
	}
	if got.ModalitiesInput == nil || len(got.ModalitiesInput) != 0 {
		t.Errorf("ModalitiesInput = %v，期望空切片（NOT NULL 的列）", got.ModalitiesInput)
	}
	if got.ReasoningOptions == nil || len(got.ReasoningOptions) != 0 {
		t.Errorf("ReasoningOptions = %v，期望空数组", got.ReasoningOptions)
	}
}

// 规范名撞车要报成 ErrModelExists，并且带上具体的约束名——
// 光说「写入失败」的话，人分不清是重名还是别的什么问题。
func TestCreateModelRejectsDuplicateName(t *testing.T) {
	db := testDB(t)
	name := uniqueName("model")

	if _, err := NewAdmin(db).Create(context.Background(), CreateModelRequest{
		CanonicalName: name, DisplayName: name,
	}); err != nil {
		t.Fatalf("第一次 Create 出错: %v", err)
	}

	_, err := NewAdmin(db).Create(context.Background(), CreateModelRequest{
		CanonicalName: name, DisplayName: name,
	})
	assertViolation(t, err, pgUniqueViolation, "model_catalog_canonical_name_key", ErrModelExists)
}

// 一次修改之后，改过的与没改过的都要读回来核对。
func TestUpdateModelRoundTrip(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	name := uniqueName("model")
	cutoff := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	if _, err := admin.Create(context.Background(), CreateModelRequest{
		CanonicalName: name, DisplayName: name,
		Vendor: "old-vendor", ContextLimit: 1000, KnowledgeCutoff: &cutoff,
		Raw: map[string]any{"keep": "me"},
	}); err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	newDisplay := "新显示名"
	newLimit := 2000
	caps := Capabilities{Tools: true}
	modalities := []string{"text"}
	options := []any{map[string]any{"type": "toggle"}}
	empty := ""
	err := admin.Update(context.Background(), UpdateModelRequest{
		CanonicalName:    name,
		DisplayName:      &newDisplay,
		Vendor:           &empty,      // 指向零值 = 清空
		ContextLimit:     &newLimit,   // 改成新值
		Capabilities:     &caps,       // 整体替换能力矩阵
		ModalitiesInput:  &modalities, // 空数组之外的替换
		ReasoningOptions: &options,
		KnowledgeCutoff:  &time.Time{}, // 指向零值 = 清空
	})
	if err != nil {
		t.Fatalf("Update 出错: %v", err)
	}

	got, err := New(db).Model(context.Background(), name)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.DisplayName != newDisplay {
		t.Errorf("DisplayName = %q，期望 %q", got.DisplayName, newDisplay)
	}
	if got.Vendor != "" {
		t.Errorf("Vendor = %q，期望被清空", got.Vendor)
	}
	if got.ContextLimit != 2000 {
		t.Errorf("ContextLimit = %d，期望 2000", got.ContextLimit)
	}
	if got.Capabilities != caps {
		t.Errorf("Capabilities = %+v，期望 %+v", got.Capabilities, caps)
	}
	if strings.Join(got.ModalitiesInput, ",") != "text" {
		t.Errorf("ModalitiesInput = %v", got.ModalitiesInput)
	}
	if len(got.ReasoningOptions) != 1 {
		t.Errorf("ReasoningOptions = %v，期望 1 项", got.ReasoningOptions)
	}
	if got.KnowledgeCutoff != nil {
		t.Errorf("KnowledgeCutoff = %v，期望被清空", got.KnowledgeCutoff)
	}
	// 没提到的字段一个都不许动。
	if got.Raw["keep"] != "me" {
		t.Errorf("Raw = %v，没提到的字段被改掉了", got.Raw)
	}
	if got.InputLimit != 0 || got.OutputLimit != 0 {
		t.Errorf("limits = %d/%d，没提到的字段被改掉了", got.InputLimit, got.OutputLimit)
	}
}

// 只给规范名等于没有要改的字段：**规范名本身不可改**（见 Admin 的注释），
// 所以这里必须是 ErrNoChanges，而不是一条悄悄改了 updated_at 的 UPDATE。
func TestUpdateModelRejectsEmptyRequest(t *testing.T) {
	db := testDB(t)
	name := uniqueName("model")
	seedModel(t, db, name)

	err := NewAdmin(db).Update(context.Background(), UpdateModelRequest{CanonicalName: name})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v，期望 ErrNoChanges", err)
	}

	// 库里那一行还在，而且没有被当成一次成功的修改。
	var display string
	if err := db.QueryRowContext(context.Background(),
		`SELECT display_name FROM model_catalog WHERE canonical_name = $1`, name).Scan(&display); err != nil {
		t.Fatalf("回查失败: %v", err)
	}
	if display != name {
		t.Errorf("display_name = %q，期望没被动过", display)
	}
}

func TestUpdateModelNotFound(t *testing.T) {
	db := testDB(t)
	display := "x"

	err := NewAdmin(db).Update(context.Background(), UpdateModelRequest{
		CanonicalName: uniqueName("missing"), DisplayName: &display,
	})
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v，期望 ErrModelNotFound", err)
	}
}

// 校验失败发生在碰到数据库之前：拿一个 nil 句柄也应当报出字段问题，
// 而不是 panic 或者一句「连接为空」。
func TestCreateModelValidation(t *testing.T) {
	cases := []struct {
		name string
		req  CreateModelRequest
	}{
		{"规范名为空", CreateModelRequest{DisplayName: "d"}},
		{"规范名只有空白", CreateModelRequest{CanonicalName: "   ", DisplayName: "d"}},
		{"规范名里有空白", CreateModelRequest{CanonicalName: "acme vision", DisplayName: "d"}},
		{"展示名为空", CreateModelRequest{CanonicalName: "acme-vision"}},
		{"context_limit 为负", CreateModelRequest{CanonicalName: "acme-vision", DisplayName: "d", ContextLimit: -1}},
		{"output_limit 为负", CreateModelRequest{CanonicalName: "acme-vision", DisplayName: "d", OutputLimit: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAdmin(nil).Create(context.Background(), tc.req); err == nil {
				t.Fatal("应当被拒绝，实际通过了校验")
			}
		})
	}
}

// 别名写进去之后，两个方向的列举都要能看见它。
func TestCreateAliasRoundTrip(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	upstream := "up-" + model

	id, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID:     channel,
		UpstreamModel: upstream,
		CanonicalName: model,
		Overrides:     map[string]any{"context_limit": float64(4096)},
	})
	if err != nil {
		t.Fatalf("CreateAlias 出错: %v", err)
	}

	byChannel, err := New(db).Aliases(context.Background(), channel)
	if err != nil {
		t.Fatalf("Aliases 出错: %v", err)
	}
	got := findAlias(t, byChannel, upstream)
	if got.ID != id || got.CanonicalName != model || got.ChannelID != channel {
		t.Errorf("别名 = %+v，期望 id=%d、规范名 %q、渠道 %d", got, id, model, channel)
	}
	if got.Overrides["context_limit"] != float64(4096) {
		t.Errorf("Overrides = %v，期望解回对象", got.Overrides)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt 为零值")
	}

	byModel, err := New(db).UpstreamModels(context.Background(), model)
	if err != nil {
		t.Fatalf("UpstreamModels 出错: %v", err)
	}
	if len(byModel) != 1 || byModel[0].UpstreamModel != upstream {
		t.Errorf("UpstreamModels = %+v，期望只有 %q", byModel, upstream)
	}
}

// 平台级别名（channel_id IS NULL）单独一套：写的时候传 0，读的时候也只从
// 平台级那一批里取——两个范围混起来的话，「对所有渠道生效」与
// 「只对这一个渠道生效」就分不清了。
func TestCreateAliasPlatformScope(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	upstream := "platform-" + model

	if _, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID:     0,
		UpstreamModel: upstream,
		CanonicalName: model,
	}); err != nil {
		t.Fatalf("CreateAlias 出错: %v", err)
	}

	platform, err := New(db).Aliases(context.Background(), 0)
	if err != nil {
		t.Fatalf("Aliases(0) 出错: %v", err)
	}
	if got := findAlias(t, platform, upstream); got.ChannelID != 0 {
		t.Errorf("ChannelID = %d，期望 0（平台级）", got.ChannelID)
	}

	// 渠道级的清单里不该有它。
	byChannel, err := New(db).Aliases(context.Background(), channel)
	if err != nil {
		t.Fatalf("Aliases(channel) 出错: %v", err)
	}
	for _, alias := range byChannel {
		if alias.UpstreamModel == upstream {
			t.Errorf("平台级别名 %q 出现在了渠道 %d 的清单里", upstream, channel)
		}
	}

	// 但任何渠道查这个名字都要回落到它。
	got, err := New(db).UpstreamModel(context.Background(), channel, model)
	if err != nil {
		t.Fatalf("UpstreamModel 出错: %v", err)
	}
	if got != upstream {
		t.Errorf("UpstreamModel = %q，期望 %q", got, upstream)
	}
}

// 渠道级撞车要报成 ErrCanonicalTaken，而且话里要说清是**渠道级**——
// 平台级撞的是另一条索引，处置也不同。
func TestCreateAliasTranslatesChannelConflict(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)

	if _, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-a-" + model, CanonicalName: model,
	}); err != nil {
		t.Fatalf("第一次 CreateAlias 出错: %v", err)
	}

	_, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-b-" + model, CanonicalName: model,
	})
	assertViolation(t, err, pgUniqueViolation, aliasChannelCanonicalUnique, ErrCanonicalTaken)
	// 报错要提这个名字，否则人不知道是哪个规范名撞了。
	if !strings.Contains(err.Error(), model) {
		t.Errorf("报错里没提规范名 %q：%s", model, err)
	}
	// 而且要说清是**渠道级**：平台级撞的是另一条索引，处置也不同
	//（一个是去改这个渠道，一个是去改兜底那条）。两句写反了在这里挂。
	if !strings.Contains(err.Error(), "渠道 "+itoa(channel)) {
		t.Errorf("报错里没说是哪个渠道撞的：%s", err)
	}
}

func TestCreateAliasTranslatesPlatformConflict(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	seedModel(t, db, model)

	if _, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		UpstreamModel: "up-a-" + model, CanonicalName: model,
	}); err != nil {
		t.Fatalf("第一次 CreateAlias 出错: %v", err)
	}

	_, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		UpstreamModel: "up-b-" + model, CanonicalName: model,
	})
	assertViolation(t, err, pgUniqueViolation, aliasPlatformCanonicalUnique, ErrCanonicalTaken)
	// 平台级撞了要说成平台级：这句给人的下一步是「去改兜底那条」，
	// 而渠道级那句是「去改这个渠道」。
	if !strings.Contains(err.Error(), "平台级") {
		t.Errorf("报错里没说是平台级撞的：%s", err)
	}
}

// 反方向：同一个渠道里一个上游名只能对应一个规范名。
func TestCreateAliasTranslatesUpstreamConflict(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	other := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	seedModel(t, db, other)
	upstream := "up-" + model

	if _, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: upstream, CanonicalName: model,
	}); err != nil {
		t.Fatalf("第一次 CreateAlias 出错: %v", err)
	}

	_, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: upstream, CanonicalName: other,
	})
	assertViolation(t, err, pgUniqueViolation, "model_aliases_channel_id_upstream_model_key", ErrUpstreamTaken)
}

// 规范名不在目录里：外键会拒，但报出来的应当是「先建模型」，
// 而不是一句 SQLSTATE。
func TestCreateAliasRejectsUnknownCanonical(t *testing.T) {
	db := testDB(t)
	channel := insertChannel(t, db, uniqueName("model"))

	_, err := NewAdmin(db).CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: uniqueName("up"), CanonicalName: uniqueName("missing"),
	})
	assertViolation(t, err, pgForeignKeyViolation, aliasCanonicalFK, ErrModelNotFound)
}

func TestCreateAliasRejectsUnknownChannel(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	seedModel(t, db, model)

	_, err := NewAdmin(db).CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: nextID(), UpstreamModel: uniqueName("up"), CanonicalName: model,
	})
	assertViolation(t, err, pgForeignKeyViolation, aliasChannelFK, ErrChannelNotFound)
}

func TestCreateAliasValidation(t *testing.T) {
	cases := []struct {
		name string
		req  CreateAliasRequest
	}{
		{"渠道 id 为负", CreateAliasRequest{ChannelID: -1, UpstreamModel: "u", CanonicalName: "c"}},
		{"规范名为空", CreateAliasRequest{UpstreamModel: "u"}},
		{"上游名为空", CreateAliasRequest{CanonicalName: "c"}},
		{"上游名只有空白", CreateAliasRequest{UpstreamModel: "  ", CanonicalName: "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewAdmin(nil).CreateAlias(context.Background(), tc.req); err == nil {
				t.Fatal("应当被拒绝，实际通过了校验")
			}
		})
	}
}

// 改别名：上游名与 overrides 要能改，改完读回来核对。
func TestUpdateAliasRoundTrip(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)

	id, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-old-" + model, CanonicalName: model,
	})
	if err != nil {
		t.Fatalf("CreateAlias 出错: %v", err)
	}

	newUpstream := "up-new-" + model
	overrides := map[string]any{"note": "changed"}
	err = admin.UpdateAlias(context.Background(), UpdateAliasRequest{
		ID: id, UpstreamModel: &newUpstream, Overrides: &overrides,
	})
	if err != nil {
		t.Fatalf("UpdateAlias 出错: %v", err)
	}

	aliases, err := New(db).Aliases(context.Background(), channel)
	if err != nil {
		t.Fatalf("Aliases 出错: %v", err)
	}
	got := findAlias(t, aliases, newUpstream)
	if got.ID != id || got.CanonicalName != model {
		t.Errorf("别名 = %+v，期望 id 与规范名不变", got)
	}
	if got.Overrides["note"] != "changed" {
		t.Errorf("Overrides = %v，期望 %v", got.Overrides, overrides)
	}
}

// 改成一个已经占用的上游名：同一套约束、同一套说法。
func TestUpdateAliasTranslatesUpstreamConflict(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	other := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	seedModel(t, db, other)
	taken := "up-taken-" + model

	if _, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: taken, CanonicalName: model,
	}); err != nil {
		t.Fatalf("第一次 CreateAlias 出错: %v", err)
	}
	id, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-mine-" + model, CanonicalName: other,
	})
	if err != nil {
		t.Fatalf("第二次 CreateAlias 出错: %v", err)
	}

	err = admin.UpdateAlias(context.Background(), UpdateAliasRequest{ID: id, UpstreamModel: &taken})
	assertViolation(t, err, pgUniqueViolation, "model_aliases_channel_id_upstream_model_key", ErrUpstreamTaken)
}

func TestUpdateAliasRejectsEmptyRequest(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	id, err := NewAdmin(db).CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-" + model, CanonicalName: model,
	})
	if err != nil {
		t.Fatalf("CreateAlias 出错: %v", err)
	}

	err = NewAdmin(db).UpdateAlias(context.Background(), UpdateAliasRequest{ID: id})
	if !errors.Is(err, ErrNoChanges) {
		t.Errorf("err = %v，期望 ErrNoChanges", err)
	}

	upstream := "up-" + model
	err = NewAdmin(db).UpdateAlias(context.Background(), UpdateAliasRequest{ID: nextID(), UpstreamModel: &upstream})
	if !errors.Is(err, ErrAliasIDNotFound) {
		t.Errorf("err = %v，期望 ErrAliasIDNotFound", err)
	}
}

func TestDeleteAlias(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	upstream := "up-" + model

	id, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: upstream, CanonicalName: model,
	})
	if err != nil {
		t.Fatalf("CreateAlias 出错: %v", err)
	}
	err = admin.DeleteAlias(context.Background(), id)
	if err != nil {
		t.Fatalf("DeleteAlias 出错: %v", err)
	}

	aliases, err := New(db).Aliases(context.Background(), channel)
	if err != nil {
		t.Fatalf("Aliases 出错: %v", err)
	}
	for _, alias := range aliases {
		if alias.ID == id {
			t.Fatalf("别名 %d 还在清单里", id)
		}
	}
	// 删过的那条再查一次：选路那条路径要报 ErrAliasNotFound，不是拿到空串。
	if _, err := New(db).UpstreamModel(context.Background(), channel, model); !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("err = %v，期望 ErrAliasNotFound", err)
	}
	if err := admin.DeleteAlias(context.Background(), id); !errors.Is(err, ErrAliasIDNotFound) {
		t.Errorf("err = %v，期望 ErrAliasIDNotFound", err)
	}
}

// ── 规范名的删除 ────────────────────────────────────────────────────────

// 还挂着别名时不许删：删掉之后渠道的 models 白名单里还留着这个名字，
// 那是一条谁都看不懂的废配置。
func TestDeleteModelRefusesWhenAliased(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	if _, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-" + model, CanonicalName: model,
	}); err != nil {
		t.Fatalf("CreateAlias 出错: %v", err)
	}

	err := admin.Delete(context.Background(), model)
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("err = %v，期望 ErrModelInUse", err)
	}
	// 报错要说清被什么挡住了：别名几条、规则几条。
	if !strings.Contains(err.Error(), model) {
		t.Errorf("报错里没提规范名 %q：%s", model, err)
	}

	if _, err := New(db).Model(context.Background(), model); err != nil {
		t.Errorf("被拒之后模型不该消失: %v", err)
	}
}

// 还挂在计价规则上时也不许删：那条规则是 jsonb 数组，删了不会报错，
// 只会让请求静默落到别的规则上。
func TestDeleteModelRefusesWhenPriced(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	seedModel(t, db, model)
	insertPricingRule(t, db, model)

	err := NewAdmin(db).Delete(context.Background(), model)
	if !errors.Is(err, ErrModelInUse) {
		t.Fatalf("err = %v，期望 ErrModelInUse", err)
	}

	if _, err := New(db).Model(context.Background(), model); err != nil {
		t.Errorf("被拒之后模型不该消失: %v", err)
	}
}

// 无人引用时删得掉，删完就查不到了。
func TestDeleteModelWhenUnreferenced(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	seedModel(t, db, model)

	if err := admin.Delete(context.Background(), model); err != nil {
		t.Fatalf("Delete 出错: %v", err)
	}
	if _, err := New(db).Model(context.Background(), model); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v，期望 ErrModelNotFound", err)
	}
	if err := admin.Delete(context.Background(), model); !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v，期望 ErrModelNotFound", err)
	}
}
