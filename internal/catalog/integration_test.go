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

	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/testdb"
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

func TestAliasPrefersChannelSpecific(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)

	// 平台级别名对所有渠道生效，渠道级的应该盖过它。
	insertAlias(t, db, nil, "platform-"+model, model)
	insertAlias(t, db, channel, "channel-"+model, model)

	got, _, err := New(db).Alias(context.Background(), channel, model)
	if err != nil {
		t.Fatalf("Alias 出错: %v", err)
	}
	if got != "channel-"+model {
		t.Errorf("Alias = %q，期望渠道级别名优先", got)
	}
}

// 渠道级没有时回落到平台级。
func TestAliasFallsBackToPlatform(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	insertAlias(t, db, nil, "platform-"+model, model)

	got, _, err := New(db).Alias(context.Background(), channel, model)
	if err != nil {
		t.Fatalf("Alias 出错: %v", err)
	}
	if got != "platform-"+model {
		t.Errorf("Alias = %q，期望回落到平台级别名", got)
	}
}

// 别的渠道的别名不该被用上——这是「配了一半」那种情况：这个规范名**配过**
// 别名，只是没有一条覆盖当前渠道。此时报错，不回落成规范名。
//
// 回落的表现是上游回一句「这个模型不存在」，排查方向指向对方；而真正要改的
// 是我们这一侧的别名配置。判据是「这个规范名有没有配过别名」，不是「表里有没有
// 任何一行」——后者会让配过别的模型名的部署一起失败（见下面那条）。
func TestAliasIgnoresOtherChannels(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	mine := insertChannel(t, db, model)
	other := insertChannel(t, db, uniqueName("model"))
	seedModel(t, db, model)
	insertAlias(t, db, other, "other-"+model, model)

	_, _, err := New(db).Alias(context.Background(), mine, model)
	if !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("err = %v，期望 ErrAliasNotFound", err)
	}
}

// 一条别名都没配过的规范名按同名处理——这是「一条别名都没配」的部署，
// 大多数部署都是这样，它们必须照旧能打通。
//
// 判据只落在**这个规范名**上：表里有没有别的行不参与判断。所以下面先插一条
// 别的规范名的别名，再断言这个没配过的名字照样按同名返回——「表里有行就报错」
// 那种写法会在这里失败。
func TestAliasFallsBackToIdentityWhenNoAliasForThisName(t *testing.T) {
	db := testDB(t)
	otherModel := uniqueName("model")
	seedModel(t, db, otherModel)
	insertAlias(t, db, nil, "platform-"+otherModel, otherModel)

	model := uniqueName("nonexistent")
	got, _, err := New(db).Alias(context.Background(), 1, model)
	if err != nil {
		t.Fatalf("一条别名都没配过的规范名不该报错: %v", err)
	}
	if got != model {
		t.Errorf("Alias = %q，期望按同名返回 %q", got, model)
	}
}

// channelID 传 0 是「不针对具体渠道」，只走平台级别名那条。
func TestAliasChannelZeroUsesPlatformAlias(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	seedModel(t, db, model)
	insertAlias(t, db, nil, "platform-"+model, model)

	got, _, err := New(db).Alias(context.Background(), 0, model)
	if err != nil {
		t.Fatalf("Alias 出错: %v", err)
	}
	if got != "platform-"+model {
		t.Errorf("Alias = %q，期望平台级别名", got)
	}
}

// ── 数据库层的约束（与写入路径无关，直接走 SQL） ────────────────────────

// 两条偏索引的名字（aliasChannelCanonicalUnique 等）定义在 admin.go 里，
// 它们是写入路径翻译错误时要认的东西——所以下面的断言与生产代码认的是
// **同一个常量**，改名时不会有一边忘了改。它们硬编码自
// 00007_model_alias_unique.sql，失效的方向是失败，不是静默通过。

// 约束所在的迁移文件，也是「已有重复数据会被拒绝」那条用例要重放的那一份。
const aliasUniqueMigration = "00007_model_alias_unique.sql"

// upstream_model 上那条「不能是空白」的 CHECK，以及它所在的迁移文件。
//
// 这两个常量**不**进 admin.go 那个常量块：那一块列的是「写入路径翻译错误时要
// 认的东西」，而写入路径在碰到 SQL 之前就把空白挡掉了（normalizeUpstreamModel），
// aliasWriteError 里没有这个码的分支。判据仍然硬编码自迁移文件，失效的方向是
// 失败，不是静默通过。
const (
	aliasBlankMigration        = "00022_model_alias_upstream_model_not_blank.sql"
	aliasUpstreamModelNotBlank = "model_aliases_upstream_model_not_blank"
)

// pgCheckViolation 是 CHECK 约束违反的 SQLSTATE，同样只在测试里用。
const pgCheckViolation = "23514"

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

	got, _, err := New(db).Alias(context.Background(), mine, model)
	if err != nil {
		t.Fatalf("Alias 出错: %v", err)
	}
	if got != "up-a-"+model {
		t.Errorf("Alias = %q，期望渠道级那条 %q", got, "up-a-"+model)
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

// 空串与纯空白都不许写进 upstream_model——直接走 SQL 也不行。
//
// 这一列是 text NOT NULL，而 NOT NULL 拦不住空串。它一旦是空的，别名查询就会
// 返回 ("", nil)，请求带着 "model": "" 发给上游；上游若照收，这次请求**已经
// 成功、钱已经花掉**，走到 internal/ledger/store 的 Save 才失败——账记不上而
// 钱花了。
//
// 断言落在**约束名**上而不是「报错了」：连不上库、列名写错、外键不认都会报错，
// 而它们与「这一列被这条 CHECK 挡住了」是两回事。
func TestAliasRejectsBlankUpstreamModelOnInsert(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	seedModel(t, db, model)

	for _, tc := range []struct{ name, blank string }{
		{"空串", ""},
		{"纯空白", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := insertAliasErr(db, nil, tc.blank, model)
			assertViolation(t, err, pgCheckViolation, aliasUpstreamModelNotBlank, nil)
		})
	}

	var left int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM model_aliases WHERE canonical_name = $1`, model).Scan(&left); err != nil {
		t.Fatalf("回查失败: %v", err)
	}
	if left != 0 {
		t.Errorf("被拒之后还剩 %d 行，期望一行都没写进去", left)
	}
}

// 改别名时同样拦得住：这条路径上原本只有 normalizeUpstreamModel 一道，
// 而它照不到直接走 SQL 的调用方。
func TestAliasRejectsBlankUpstreamModelOnUpdate(t *testing.T) {
	db := testDB(t)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	upstream := "up-" + model
	insertAlias(t, db, channel, upstream, model)

	var id int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT id FROM model_aliases WHERE channel_id = $1 AND upstream_model = $2`,
		channel, upstream).Scan(&id); err != nil {
		t.Fatalf("取别名 id 失败: %v", err)
	}

	for _, tc := range []struct{ name, blank string }{
		{"空串", ""},
		{"纯空白", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(context.Background(),
				`UPDATE model_aliases SET upstream_model = $1 WHERE id = $2`, tc.blank, id)
			assertViolation(t, err, pgCheckViolation, aliasUpstreamModelNotBlank, nil)
		})
	}

	// 被拒的那两次都不许动到这一行：UPDATE 报错时值必须原样。
	var got string
	if err := db.QueryRowContext(context.Background(),
		`SELECT upstream_model FROM model_aliases WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("回查失败: %v", err)
	}
	if got != upstream {
		t.Errorf("upstream_model = %q，期望没被动过（%q）", got, upstream)
	}
}

// 存量里已经有空白行时，迁移必须报错退出、并把那几行列出来，而不是静默清理
// （那是替数据选一个上游模型名）或静默装上约束（那时这条 CHECK 对已有的坏行
// 一点用都没有，而读的人会以为这一列从此有保证了）。
//
// 与 TestMigrationRejectsExistingDuplicateAliases 同一个形状：先造出「约束之前」
// 的状态——约束在事务里摘掉、坏行在事务里插进去，跑完迁移再整体回滚。
func TestMigrationRejectsExistingBlankUpstreamModel(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	model := uniqueName("model")
	seedModel(t, db, model)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(ctx,
		`ALTER TABLE model_aliases DROP CONSTRAINT `+aliasUpstreamModelNotBlank); err != nil {
		t.Fatalf("摘掉约束失败: %v", err)
	}

	// 用纯空白而不是空串：判据是 btrim(...) <> ''，这两种都该被同一条拦住，
	// 而只判 `<> ''` 的写法会在这里放走它。
	var badID int64
	if err = tx.QueryRowContext(ctx, `
		INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
		VALUES (NULL, '   ', $1) RETURNING id`, model).Scan(&badID); err != nil {
		t.Fatalf("造空白行失败: %v", err)
	}

	// 迁移报错之后事务会被 PostgreSQL 置为 aborted，后面什么都查不了，
	// 所以先打一个保存点：报错后回到这里，事务恢复可用，造的坏行还在。
	if _, err = tx.ExecContext(ctx, `SAVEPOINT before_migration`); err != nil {
		t.Fatalf("打保存点失败: %v", err)
	}

	_, err = tx.ExecContext(ctx, migrationUpSQL(t, aliasBlankMigration))
	if err == nil {
		t.Fatal("存量有空白行时迁移必须报错，而不是静默装上约束")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v，期望 *pgconn.PgError", err)
	}
	if pgErr.Code != "P0001" {
		t.Fatalf("SQLSTATE = %s（%s），期望 P0001（迁移自己抛的那个）", pgErr.Code, pgErr.Message)
	}
	// 报错要说清是**哪几行**：ALTER TABLE 自己的报错不指名任何行，也不说有几行，
	// 人拿到它还得自己再查一遍。
	if !strings.Contains(pgErr.Message, itoa(badID)) {
		t.Errorf("报错里没提空白行的 id %d：%s", badID, pgErr.Message)
	}

	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT before_migration`); err != nil {
		t.Fatalf("回滚到保存点失败: %v", err)
	}
	// 那一行不许被删掉或改写：迁移的职责是拒绝，不是替数据挑一个上游名。
	var upstream string
	if err := tx.QueryRowContext(ctx,
		`SELECT upstream_model FROM model_aliases WHERE id = $1`, badID).Scan(&upstream); err != nil {
		t.Fatalf("回查空白行失败: %v", err)
	}
	if upstream != "   " {
		t.Errorf("upstream_model = %q，期望原样留着（迁移不该改数据）", upstream)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}

	// 回滚必须把约束带回来，否则这条用例把库留在了没有约束的状态上，而后面
	// 那些用例会照常通过——它们只是不再受保护。
	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_constraint
		                WHERE conname = $1 AND conrelid = 'model_aliases'::regclass)`,
		aliasUpstreamModelNotBlank).Scan(&exists); err != nil {
		t.Fatalf("查约束失败: %v", err)
	}
	if !exists {
		t.Errorf("回滚后约束 %s 不在了", aliasUpstreamModelNotBlank)
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

// ── 三态能力的断言助手 ──────────────────────────────────────────────────

// declared 造一项**已声明**的能力：true 支持、false 明确不支持。
//
// 「没配过」是第三个状态，直接写 nil（不经过这个助手）——用例里必须能把
// 这两者分开写出来，否则证明不了它们没被合并成一个零值。
func declared(v bool) *bool { return &v }

// sameCapabilities 按**值**比较两份三态能力。
//
// 不能写 `a != b`：那比的是六个指针的地址，两份内容相同的声明会因为地址
// 不同而判不等——一条本该通过的对拍会红，而红的理由与真正要查的东西无关。
func sameCapabilities(a, b Capabilities) bool {
	for _, pair := range [][2]*bool{
		{a.Reasoning, b.Reasoning}, {a.Tools, b.Tools}, {a.Vision, b.Vision},
		{a.Audio, b.Audio}, {a.Cache, b.Cache}, {a.Batch, b.Batch},
	} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return false
		}
		if pair[0] != nil && *pair[0] != *pair[1] {
			return false
		}
	}
	return true
}

// capabilityText 把一项能力打成一句话，供断言失败时的信息用：nil 打成
// 「未知」。要这一句是因为失败时最需要知道的正是「是没配过还是配了说不支持」
// ——这两者在判据里处置相反。
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

// capabilitiesText 把六项一起打出来。
func capabilitiesText(c Capabilities) string {
	return "reasoning=" + capabilityText(c.Reasoning) +
		" tools=" + capabilityText(c.Tools) +
		" vision=" + capabilityText(c.Vision) +
		" audio=" + capabilityText(c.Audio) +
		" cache=" + capabilityText(c.Cache) +
		" batch=" + capabilityText(c.Batch)
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
		// 六项里四项显式声明（含一项 false）、两项不给：不给的那两项要走完
		// 「不写 → NULL → 读回来是未知」这一趟，否则这条用例证明不了写入侧
		// 没有把「没给」悄悄折成 false。
		Capabilities: Capabilities{
			Reasoning: declared(true), Tools: declared(false),
			Vision: declared(true), Cache: declared(true),
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
	wantCaps := Capabilities{
		Reasoning: declared(true), Tools: declared(false),
		Vision: declared(true), Cache: declared(true),
	}
	if !sameCapabilities(got.Capabilities, wantCaps) {
		t.Errorf("Capabilities = %s，期望 %s",
			capabilitiesText(got.Capabilities), capabilitiesText(wantCaps))
	}
	// 没给的两项读回来应当是**未知**（nil），不是 false。这一条与上面那行
	// 分开写：整份 struct 的比较里，某一个字段被判错也可能被别处的相等掩盖。
	if got.Capabilities.Audio != nil || got.Capabilities.Batch != nil {
		t.Errorf("没给的两项该是未知（NULL），得到 audio=%s batch=%s",
			capabilityText(got.Capabilities.Audio), capabilityText(got.Capabilities.Batch))
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
	caps := Capabilities{Tools: declared(true)}
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
	if !sameCapabilities(got.Capabilities, caps) {
		t.Errorf("Capabilities = %s，期望 %s",
			capabilitiesText(got.Capabilities), capabilitiesText(caps))
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
		Overrides:     map[string]any{"temperature": float64(0.2)},
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
	if got.Overrides["temperature"] != float64(0.2) {
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
	got, _, err := New(db).Alias(context.Background(), channel, model)
	if err != nil {
		t.Fatalf("Alias 出错: %v", err)
	}
	if got != upstream {
		t.Errorf("Alias = %q，期望 %q", got, upstream)
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

// 写入时拒绝改请求形状的那几个键，而且错误里**说得出是哪一个**。
//
// # 为什么拒绝发生在写入时
//
// 这几个键改的是请求的形状（发给哪个模型、正文是什么、是不是流式），让别名去
// 改它们等于让商家改写调用方的请求。判据放在写入时而不是热路径上：写进来的
// 那一刻是唯一还分得清「配错了」与「这一列本来就这么用」的地方，而热路径拒绝
// 的表现是那个别名上的每一个请求都在失败——那时看到的是故障，不是配置。
//
// # 为什么断言错误文本
//
// 只断言 err != nil 的话，任何一条拒绝都成立：键名写错、库连不上、外键不存在，
// 全都算「通过了」。而这里要钉的恰恰是**它拒绝的是这个键**，理由是那一句。
func TestCreateAliasRejectsForbiddenOverrides(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	// 逐个键单独试：一次写两个的话，错误里只会出现其中一个，另一个是不是也
	// 被拒了看不出来。
	cases := []struct {
		key   string
		value any
	}{
		{"model", "vendor/other-model"},
		{"messages", []any{map[string]any{"role": "user", "content": "改写过的正文"}}},
		{"stream", true},
		{"tools", []any{}},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			model := uniqueName("model")
			channel := insertChannel(t, db, model)
			seedModel(t, db, model)

			id, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
				ChannelID: channel, UpstreamModel: "up-" + model, CanonicalName: model,
				Overrides: map[string]any{tc.key: tc.value},
			})
			if err == nil {
				t.Fatalf("写入 %q 应当被拒绝，实际写成了 id=%d", tc.key, id)
			}
			// 错误里要说得出是哪个键，也要说得出为什么——「不在允许的字段里」
			// 与「它改了请求的形状」是两句不同的话，配这个键的人需要看的是后一句。
			if !strings.Contains(err.Error(), `"`+tc.key+`"`) {
				t.Errorf("错误文本里没有键名 %q: %v", tc.key, err)
			}
			if !strings.Contains(err.Error(), "temperature") {
				t.Errorf("错误文本里没有列出允许覆盖的键: %v", err)
			}

			// 更要紧的是**一行都没写进去**：报错而写进去了的话，那个别名上的
			// 请求会带着一份非法覆盖上路，而热路径上看到的是请求失败。
			aliases, err := New(db).Aliases(context.Background(), channel)
			if err != nil {
				t.Fatalf("Aliases 出错: %v", err)
			}
			if len(aliases) != 0 {
				t.Errorf("被拒的那条写进去了: %+v", aliases)
			}
		})
	}
}

// 白名单之外的键（多半是名字写错了）同样在写入时被拒，错误里列出能用的那几个。
func TestCreateAliasRejectsUnknownOverrideKey(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)

	_, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-" + model, CanonicalName: model,
		Overrides: map[string]any{"context_limit": float64(4096)},
	})
	if err == nil {
		t.Fatal("白名单之外的键应当被拒绝")
	}
	if !strings.Contains(err.Error(), `"context_limit"`) {
		t.Errorf("错误文本里没有键名: %v", err)
	}
	if !strings.Contains(err.Error(), "temperature") {
		t.Errorf("错误文本里应列出允许覆盖的键: %v", err)
	}
}

// 改别名时同样在写入时校验，而且**改不动原来的那一份**。
//
// 与 CreateAlias 那条分开：改是一条独立的写入路径（UPDATE），两条各自都要有
// 判据——只钉一条的话，另一条漏掉校验时没有任何东西会响，而它照样能把非法
// 覆盖写进库。
func TestUpdateAliasRejectsForbiddenOverrides(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)
	upstream := "up-" + model

	id, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: upstream, CanonicalName: model,
		Overrides: map[string]any{"temperature": float64(0.3)},
	})
	if err != nil {
		t.Fatalf("CreateAlias 出错: %v", err)
	}

	bad := map[string]any{"stream": true}
	err = admin.UpdateAlias(context.Background(), UpdateAliasRequest{ID: id, Overrides: &bad})
	if err == nil {
		t.Fatal("改成一个非法的覆盖应当被拒绝")
	}
	if !strings.Contains(err.Error(), `"stream"`) {
		t.Errorf("错误文本里没有键名: %v", err)
	}

	// 原来那份还在，没被这次失败的更新动过。
	aliases, err := New(db).Aliases(context.Background(), channel)
	if err != nil {
		t.Fatalf("Aliases 出错: %v", err)
	}
	got := findAlias(t, aliases, upstream)
	if got.Overrides["temperature"] != float64(0.3) {
		t.Errorf("Overrides = %v，期望原来那份 %.1f 还在", got.Overrides, 0.3)
	}
	if _, leaked := got.Overrides["stream"]; leaked {
		t.Errorf("被拒的那份覆盖写进去了: %v", got.Overrides)
	}
}

// 合法取值在写入时**照收**：校验不能宽到把正常配置也拦下来。
//
// 与上面几条成对：只有拒绝那几条的话，一个「什么键都拒」的实现能让它们全绿。
func TestCreateAliasAcceptsWhitelistedOverrides(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := uniqueName("model")
	channel := insertChannel(t, db, model)
	seedModel(t, db, model)

	// 值走一遍 JSON 之后一律是 float64，所以这里给的就是 float64——与从 HTTP
	// 请求体解出来的形状一致。
	id, err := admin.CreateAlias(context.Background(), CreateAliasRequest{
		ChannelID: channel, UpstreamModel: "up-" + model, CanonicalName: model,
		Overrides: map[string]any{
			"temperature": float64(0.2),
			"top_p":       float64(0.9),
			"max_tokens":  float64(2048),
		},
	})
	if err != nil {
		t.Fatalf("合法覆盖应当能写进去，得到 %v", err)
	}

	aliases, err := New(db).Aliases(context.Background(), channel)
	if err != nil {
		t.Fatalf("Aliases 出错: %v", err)
	}
	got := findAlias(t, aliases, "up-"+model)
	if got.ID != id || len(got.Overrides) != 3 {
		t.Errorf("别名 = %+v，期望三项覆盖都落库", got)
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
	overrides := map[string]any{"top_p": float64(0.9)}
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
	if got.Overrides["top_p"] != float64(0.9) {
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
	// 删过的那条再查一次：这个规范名**一条别名都不剩**了，于是回到「没配过
	// 别名」那个状态，按同名返回——不是报错，也不是拿到空串。
	//
	// 这里刻意不报 ErrAliasNotFound：那会让「删掉最后一条别名」把整个模型名
	// 变成不可用，而删别名的本意只是让这个名字恢复原样。
	if got, _, err := New(db).Alias(context.Background(), channel, model); err != nil {
		t.Errorf("删掉最后一条别名后应按同名返回，得到错误 %v", err)
	} else if got != model {
		t.Errorf("Alias = %q，期望按同名返回 %q", got, model)
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

// ── 选路判据的读面 ──────────────────────────────────────────────────────

// 热路径那条读面：能力矩阵与目录里判上限用的那两列。
//
// 用写入面（Admin.Create）造数据再读回来：两处对同一套列的假设是否一致，
// 只有「写一遍再读一遍」能证明。
func TestConstraintsRoundTrip(t *testing.T) {
	db := testDB(t)
	name := uniqueName("model")
	if _, err := NewAdmin(db).Create(context.Background(), CreateModelRequest{
		CanonicalName: name,
		DisplayName:   name,
		ContextLimit:  200000,
		OutputLimit:   8192,
		Capabilities: Capabilities{
			Tools: declared(true), Vision: declared(true), Reasoning: declared(false),
		},
	}); err != nil {
		t.Fatalf("Create 出错: %v", err)
	}

	got, err := New(db).Constraints(context.Background(), name)
	if err != nil {
		t.Fatalf("Constraints 出错: %v", err)
	}
	for _, tc := range []struct {
		name     string
		declared *bool
		want     string
	}{
		{"tools", got.Capabilities.Tools, "支持"},
		{"vision", got.Capabilities.Vision, "支持"},
		{"reasoning", got.Capabilities.Reasoning, "不支持"},
		// 没勾的三项：**未知**，不是 false。这一条是这次改动之前做不到的
		// ——那时它们在库里被 DEFAULT false 填成了「不支持」。
		{"audio", got.Capabilities.Audio, "未知"},
		{"cache", got.Capabilities.Cache, "未知"},
		{"batch", got.Capabilities.Batch, "未知"},
	} {
		if gotState := capabilityText(tc.declared); gotState != tc.want {
			t.Errorf("%s = %s，期望 %s", tc.name, gotState, tc.want)
		}
	}
	if got.ContextLimit != 200000 || got.OutputLimit != 8192 {
		t.Errorf("上限 = %d/%d，期望 200000/8192", got.ContextLimit, got.OutputLimit)
	}
}

// 没填的两个上限读出来是 0 = **未知**，没写过的能力列读出来是 nil = **未知**
// ——两者在选路里的处置相同（放行），表达方式不同（0 与 nil）。这一条把两者
// 钉在库里。
//
// # 这条用例在 00025 之前是反的
//
// 那时六个能力列是 `NOT NULL DEFAULT false`，seeded 行（只写了名字）读出来
// 是六个 false = 「六项都不支持」。同一个 insert、同一行，迁移前后读出来
// 是两个不同的结论——而库里那一行**一个字节都没变**。这正是这条迁移要改的
// 东西：把「从没配过」与「配成不支持」分开。
func TestConstraintsUnsetAreUnknown(t *testing.T) {
	db := testDB(t)
	name := uniqueName("model")
	seedModel(t, db, name)

	got, err := New(db).Constraints(context.Background(), name)
	if err != nil {
		t.Fatalf("Constraints 出错: %v", err)
	}
	if got.ContextLimit != 0 || got.OutputLimit != 0 {
		t.Errorf("没填的上限读出来 = %d/%d，期望 0/0", got.ContextLimit, got.OutputLimit)
	}
	if !sameCapabilities(got.Capabilities, Capabilities{}) {
		t.Errorf("没写过能力列的行应当六项都是未知，得到 %s",
			capabilitiesText(got.Capabilities))
	}
}

// 存量行（迁移之前写进去的 false）迁移之后仍是「明确不支持」。
//
// 这条迁移只有 DDL，一条 UPDATE 都没有——**不对存量做猜测**。把存量的
// false 猜成 NULL 会让所有已经声明过「不支持」的模型重新参与判据，而那是一次
// 静默的放行，方向正是「把请求发给一个声明做不了这件事的模型」。
//
// 造存量行的方式是裸 SQL 写死 false（那正是迁移之前每一行的样子），不走
// Admin.Create——它今天总把六项一起写下去，写出来的是新写法。
func TestExistingFalseCapabilityStaysDeclared(t *testing.T) {
	db := testDB(t)
	name := uniqueName("model")
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO model_catalog (canonical_name, display_name, supports_tools)
		VALUES ($1, $1, false)`, name); err != nil {
		t.Fatalf("插入存量行失败: %v", err)
	}

	got, err := New(db).Constraints(context.Background(), name)
	if err != nil {
		t.Fatalf("Constraints 出错: %v", err)
	}
	// 写了 false 的那一列还是 false；同一行里没写的列是 NULL。两者在同一行
	// 上并排，正是这条迁移要维持的区分。
	if got.Capabilities.Tools == nil || *got.Capabilities.Tools {
		t.Errorf("存量写的 false 应当读成「不支持」，得到 %s",
			capabilityText(got.Capabilities.Tools))
	}
	if got.Capabilities.Batch != nil {
		t.Errorf("存量行里没写的列该是未知（NULL），得到 %s",
			capabilityText(got.Capabilities.Batch))
	}

	// 管理面那条读面读到的必须是同一份结论：两处对同一套列的假设不一致时，
	// 「larkaxctl model show 说支持、选路说不支持」就是这么来的。
	model, err := New(db).Model(context.Background(), name)
	if err != nil {
		t.Fatalf("Model 出错: %v", err)
	}
	if !sameCapabilities(model.Capabilities, got.Capabilities) {
		t.Errorf("两个读面对同一行的结论不同：Model=%s Constraints=%s",
			capabilitiesText(model.Capabilities), capabilitiesText(got.Capabilities))
	}
}

// 目录里没有这个模型：ErrModelNotFound，与「查询失败」分开——调用方对前者
// 放行、对后者报错（见 internal/channels 的 profile）。
func TestConstraintsMissingModel(t *testing.T) {
	db := testDB(t)

	_, err := New(db).Constraints(context.Background(), uniqueName("model"))

	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v，期望 ErrModelNotFound", err)
	}
}
