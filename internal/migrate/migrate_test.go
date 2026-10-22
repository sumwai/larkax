package migrate_test

// 迁移 00056 的集成测试：它把 billing.stream_freeze_step 的默认值从 4096
// 改成 1024，但**只改仍停在旧默认值的行**。
//
// # 为什么非要在真库上跑
//
// 这一条要的是「UPDATE 的 WHERE 守卫生效」——值仍是 4096 的改、手工调过的
// 不动。判据是库里那一行的值，单测里没有任何东西能替它回答；把 WHERE 写错时，
// 迁移照样跑完、版本号照样前进，而手工配的值会被悄悄抹掉。
//
// 没有 LARKAX_DB_DSN 时跳过（见 testdb.Open），CI 的集成测试门禁里会跑。
//
// # 回滚指名版本
//
// 退到**本迁移的前一条**，而不是「退回最近一次」：再添一条迁移时，「最近一次」
// 撤的就是别人了，这条用例会以为自己验过回滚（见 LESSONS.md 第 37 条）。

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/testdb"
)

// streamFreezeStepMigration 是改默认值的那条迁移。版本号由文件名算出来，
// 不在这里另写一个数字——重编号时两处不会分叉。
const streamFreezeStepMigration = "00056_stream_freeze_step_default.sql"

// streamFreezeStepKey 是被改的那个配置键。
const streamFreezeStepKey = "billing.stream_freeze_step"

// otherIntKey 是另一格要碰的合法 int 键：它用来验证迁移不会顺手改别的键。
const otherIntKey = "billing.max_output_tokens"

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestStreamFreezeStepDefaultMigrationOnlyTouchesTheOldDefault 钉住这条迁移的
// 两侧守卫。
//
// 五个动作各有各的判据：
//
//   - 新库（全部迁移跑完）里是 1024：出厂值确实变了；
//   - down 把 1024 放回 4096：回滚方向到了旧默认值；
//   - up 把 4096 改成 1024：仍停在旧默认值的行跟着改；
//   - up / down 都不动 2048：手工调过的值不被覆盖；
//   - up 不动另一个合法的 int 键：迁移只碰自己那个键。
func TestStreamFreezeStepDefaultMigrationOnlyTouchesTheOldDefault(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	version, err := goose.NumericComponent(streamFreezeStepMigration)
	if err != nil {
		t.Fatalf("从迁移文件名读版本号失败: %v", err)
	}
	// down 的目标是**本迁移的前一条**：goose 的 DownTo 一直退到「当前版本
	// <= 目标」为止，所以目标写在本迁移下面那一版上。
	previous := version - 1

	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}

	value := func(key string) string {
		t.Helper()
		var raw string
		if err := db.QueryRowContext(ctx,
			`SELECT value::text FROM settings WHERE key = $1`,
			key).Scan(&raw); err != nil {
			t.Fatalf("读 %s 失败: %v", key, err)
		}
		return raw
	}
	setValue := func(key, raw string) {
		t.Helper()
		// 值按 string 传，不按 []byte：驱动会把 []byte 编码成 bytea，
		// 「column is of type jsonb but expression is of type bytea」不会
		// 告诉写代码的人该改哪里（见 internal/settings 的 PgSource.Set）。
		if _, err := db.ExecContext(ctx,
			`UPDATE settings SET value = $1 WHERE key = $2`,
			raw, key); err != nil {
			t.Fatalf("把 %s 改成 %q 失败: %v", key, raw, err)
		}
	}
	up := func() {
		t.Helper()
		if err := goose.UpContext(ctx, db, "."); err != nil {
			t.Fatalf("前进到最新失败: %v", err)
		}
	}
	down := func() {
		t.Helper()
		if err := goose.DownToContext(ctx, db, ".", previous); err != nil {
			t.Fatalf("回滚到 %d 失败: %v", previous, err)
		}
	}

	// 第 5 步要临时改另一个键，先把它的原值记下来，收尾一起还原。
	originalOther := value(otherIntKey)

	// 收尾要还原**两样**：版本与值。用例中途红掉会停在回滚态或人工值上，
	// 本包之后读这个键的用例就会跟着红——那是把一条用例的状态算到另一条头上。
	// 只推版本不够：第 4 步结束时库里是 version=55、value=2048，up 会被
	// `WHERE value = '4096'` 挡在门外，值原样留成 2048。
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := goose.UpContext(cleanupCtx, db, "."); err != nil {
			t.Errorf("收尾时把库推回最新失败: %v", err)
		}
		if _, err := db.ExecContext(cleanupCtx,
			`UPDATE settings SET value = $1 WHERE key = $2`,
			"1024", streamFreezeStepKey); err != nil {
			t.Errorf("收尾时把 %s 还原成 1024 失败: %v", streamFreezeStepKey, err)
		}
		if _, err := db.ExecContext(cleanupCtx,
			`UPDATE settings SET value = $1 WHERE key = $2`,
			originalOther, otherIntKey); err != nil {
			t.Errorf("收尾时把 %s 还原成 %s 失败: %v", otherIntKey, originalOther, err)
		}
	})

	// 1. 新库跑完全部迁移之后就是新默认值。
	if got := value(streamFreezeStepKey); got != "1024" {
		t.Fatalf("新库里的 %s = %s，期望 1024（新默认值）", streamFreezeStepKey, got)
	}

	// 2. down 回到旧默认值。
	down()
	if got := value(streamFreezeStepKey); got != "4096" {
		t.Fatalf("回滚之后 %s = %s，期望 4096（旧默认值）", streamFreezeStepKey, got)
	}

	// 3. 仍停在旧默认值的行跟着 up 改。
	setValue(streamFreezeStepKey, "4096")
	up()
	if got := value(streamFreezeStepKey); got != "1024" {
		t.Errorf("up 之后 %s = %s，期望 1024（值仍是旧默认值的行应当被改）",
			streamFreezeStepKey, got)
	}

	// 4. 手工调过的值两侧都不动。
	down() // 先回到 55：此时值是上一步写下的 1024，down 会把它放回 4096
	setValue(streamFreezeStepKey, "2048")
	up()
	if got := value(streamFreezeStepKey); got != "2048" {
		t.Errorf("up 之后 %s = %s，期望 2048（手工设置不该被覆盖）",
			streamFreezeStepKey, got)
	}
	down()
	if got := value(streamFreezeStepKey); got != "2048" {
		t.Errorf("down 之后 %s = %s，期望 2048（回滚同样不该覆盖手工设置）",
			streamFreezeStepKey, got)
	}

	// 5. 迁移只动它自己那个键：另一个合法的 int 键落在 4096 上时不该被顺手改掉。
	//
	// 防的是把 Up 的 `key = 'billing.stream_freeze_step'` 条件漏掉——少了它，
	// 上面四步照样全绿（它们只读这一个键），而别人的配置会被抹掉。
	setValue(otherIntKey, "4096")
	up()
	if got := value(otherIntKey); got != "4096" {
		t.Errorf("up 之后 %s = %s，期望 4096（迁移不该动别的键）", otherIntKey, got)
	}
	if got := value(streamFreezeStepKey); got != "2048" {
		t.Errorf("up 之后 %s = %s，期望 2048（第 4 步留下的手工值）",
			streamFreezeStepKey, got)
	}
}

// rollupModelMigration 是给健康度桶加模型维度的迁移。版本号同样由文件名
// 算出来，重编号时两处不会分叉。
const rollupModelMigration = "00057_channel_health_rollup_model.sql"

// TestChannelHealthRollupModelMigration 钉住 00057 的两侧结构。
//
// 五个判据：
//
//   - up 之后 model / cost_events 两列在，主键含 model；
//   - 迁移前的老行保留，model 为空串、cost_events = 0——不编造模型；
//   - down 之后两列不在，主键回到 (channel_id, bucket)；
//   - down 丢弃模型行、保留合并行（合并行每次调用都同时累加，已是合并结果；
//     对全部行求和会把计数算两遍）；
//   - 合并行缺失时，down 用模型行的计数补一条（推导列留 NULL）。
func TestChannelHealthRollupModelMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// 夹具渠道的 id：闭包里要用，先声明。
	var testChannelID int64

	version, err := goose.NumericComponent(rollupModelMigration)
	if err != nil {
		t.Fatalf("从迁移文件名读版本号失败: %v", err)
	}
	previous := version - 1

	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	up := func() {
		t.Helper()
		if err := goose.UpContext(ctx, db, "."); err != nil {
			t.Fatalf("前进到最新失败: %v", err)
		}
	}
	down := func() {
		t.Helper()
		if err := goose.DownToContext(ctx, db, ".", previous); err != nil {
			t.Fatalf("回滚到 %d 失败: %v", previous, err)
		}
	}
	columns := func() map[string]bool {
		t.Helper()
		rows, err := db.QueryContext(ctx, `
			SELECT column_name FROM information_schema.columns
			 WHERE table_name = 'channel_health_rollup'`)
		if err != nil {
			t.Fatalf("读列清单失败: %v", err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("读列名失败: %v", err)
			}
			out[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("遍历列清单失败: %v", err)
		}
		return out
	}
	primaryKey := func() string {
		t.Helper()
		var def string
		if err := db.QueryRowContext(ctx, `
			SELECT pg_get_constraintdef(oid) FROM pg_constraint
			 WHERE conname = 'channel_health_rollup_pkey'`).Scan(&def); err != nil {
			t.Fatalf("读主键定义失败: %v", err)
		}
		return def
	}
	rollup := func(channelID int64, model string, bucket time.Time, requests int, cost string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO channel_health_rollup
				(channel_id, model, bucket, requests, successes, failures, cost)
			VALUES ($1, $2, $3, $4, 0, 0, $5::numeric)`,
			channelID, model, bucket, requests, cost); err != nil {
			t.Fatalf("插入健康度行失败: %v", err)
		}
	}
	// rollupFull 另给两个分位数与缓存计数：用来验证 down 回填时哪些列能求和、
	// 哪些必须留空。
	rollupFull := func(channelID int64, model string, bucket time.Time, requests int,
		cost string, p50, p95, cacheHit, cacheMiss int) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO channel_health_rollup
				(channel_id, model, bucket, requests, successes, failures,
				 latency_p50_ms, latency_p95_ms, cost, cache_hit_tokens, cache_miss_tokens)
			VALUES ($1, $2, $3, $4, 0, 0, $5, $6, $7::numeric, $8, $9)`,
			channelID, model, bucket, requests, p50, p95, cost, cacheHit, cacheMiss); err != nil {
			t.Fatalf("插入带信号的健康度行失败: %v", err)
		}
	}
	rowCount := func(bucket time.Time) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM channel_health_rollup
			 WHERE channel_id = $1 AND bucket = $2`, testChannelID, bucket).Scan(&n); err != nil {
			t.Fatalf("数健康度行失败: %v", err)
		}
		return n
	}

	up()

	// 收尾推回最新并删掉夹具：中途红掉时，这个包的其它用例不该被留在
	// 旧结构上。
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := goose.UpContext(cleanupCtx, db, "."); err != nil {
			t.Errorf("收尾时把库推回最新失败: %v", err)
		}
		if _, err := db.ExecContext(cleanupCtx,
			`DELETE FROM channels WHERE id = $1`, testChannelID); err != nil {
			t.Errorf("收尾时删测试渠道失败: %v", err)
		}
	})

	// 夹一条渠道：健康度桶有指向 channels 的外键。
	//
	// 列名按**当前最新结构**写：channels.protocol 已在 00005 删掉，这里
	// 不能再带它（带上时用例在第一步就红，后面几条断言一条都跑不到）。
	if err := db.QueryRowContext(ctx, `
		INSERT INTO channels (name, type, base_url, credential_enc, models)
		VALUES ('migr-00057', 'openai-compatible', 'https://api.example.com/v1',
		        '\x00'::bytea, ARRAY['m'])
		RETURNING id`).Scan(&testChannelID); err != nil {
		t.Fatalf("插入测试渠道失败: %v", err)
	}

	// 1. 回到旧结构，写一行「迁移前」的老桶。
	down()
	if cols := columns(); cols["model"] || cols["cost_events"] {
		t.Fatalf("回滚之后仍有 model / cost_events 列：%v", cols)
	}
	legacyBucket := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_health_rollup
			(channel_id, bucket, requests, successes, failures, cost)
		VALUES ($1, $2, 7, 6, 1, 1.25)`, testChannelID, legacyBucket); err != nil {
		t.Fatalf("插入老行失败: %v", err)
	}

	// 2. up 之后新列与新主键生效，老行保留且 model 为空串。
	up()
	cols := columns()
	if !cols["model"] || !cols["cost_events"] {
		t.Fatalf("前进之后缺列：%v", cols)
	}
	if !strings.Contains(primaryKey(), "model") {
		t.Errorf("主键 = %q，期望包含 model", primaryKey())
	}
	var (
		legacyModel  string
		legacyEvents int64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT model, cost_events FROM channel_health_rollup
		 WHERE channel_id = $1 AND bucket = $2`, testChannelID, legacyBucket,
	).Scan(&legacyModel, &legacyEvents); err != nil {
		t.Fatalf("读老行失败: %v", err)
	}
	if legacyModel != "" || legacyEvents != 0 {
		t.Errorf("老行 = model %q / cost_events %d，期望空串 / 0", legacyModel, legacyEvents)
	}

	// 3. down：模型行丢弃、合并行保留；合并行缺失的桶用模型行补一条。
	//    回填要分清可加与不可加：计数、成本、缓存求和；分位数留 NULL。
	mergedBucket := legacyBucket.Add(time.Hour)
	orphanBucket := legacyBucket.Add(2 * time.Hour)
	rollup(testChannelID, "", mergedBucket, 2, "3.0")
	rollup(testChannelID, "a", mergedBucket, 1, "1.0")
	rollup(testChannelID, "b", mergedBucket, 1, "2.0")
	rollupFull(testChannelID, "a", orphanBucket, 3, "4.0", 100, 200, 10, 20)
	rollupFull(testChannelID, "b", orphanBucket, 2, "1.5", 300, 400, 5, 7)

	down()
	if cols := columns(); cols["model"] || cols["cost_events"] {
		t.Fatalf("回滚之后仍有 model / cost_events 列：%v", cols)
	}
	if pk := primaryKey(); !strings.Contains(pk, "channel_id") || strings.Contains(pk, "model") {
		t.Errorf("回滚之后主键 = %q，期望回到 (channel_id, bucket)", pk)
	}
	if n := rowCount(mergedBucket); n != 1 {
		t.Errorf("合并桶行数 = %d，期望 1（模型行被丢弃、合并行保留）", n)
	}
	var mergedRequests int
	var mergedCost sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT requests, cost::text FROM channel_health_rollup
		 WHERE channel_id = $1 AND bucket = $2`, testChannelID, mergedBucket,
	).Scan(&mergedRequests, &mergedCost); err != nil {
		t.Fatalf("读合并桶失败: %v", err)
	}
	if mergedRequests != 2 || !mergedCost.Valid || mergedCost.String != "3.00000000" {
		t.Errorf("合并桶 = requests %d / cost %+v，期望 2 / 3.0"+
			"（保留合并行，不把模型行再求和一遍）", mergedRequests, mergedCost)
	}
	if n := rowCount(orphanBucket); n != 1 {
		t.Errorf("只有模型行的桶补完之后行数 = %d，期望 1", n)
	}
	var (
		orphanRequests, orphanHit, orphanMiss int
		orphanCost                            sql.NullString
		orphanP50, orphanP95                  sql.NullInt64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT requests, cost::text, cache_hit_tokens, cache_miss_tokens,
		       latency_p50_ms, latency_p95_ms
		  FROM channel_health_rollup
		 WHERE channel_id = $1 AND bucket = $2`, testChannelID, orphanBucket,
	).Scan(&orphanRequests, &orphanCost, &orphanHit, &orphanMiss,
		&orphanP50, &orphanP95); err != nil {
		t.Fatalf("读补出来的合并行失败: %v", err)
	}
	// 可加列逐模型求和：3+2 次请求、4.0+1.5 成本、10+5 命中、20+7 未命中。
	if orphanRequests != 5 || !orphanCost.Valid || orphanCost.String != "5.50000000" ||
		orphanHit != 15 || orphanMiss != 27 {
		t.Errorf("补出来的合并行 = requests %d / cost %+v / cache %d+%d，"+
			"期望 5 / 5.5 / 15+27（可加列逐模型求和）",
			orphanRequests, orphanCost, orphanHit, orphanMiss)
	}
	// 分位数不可加：留 NULL，而不是把模型行的 p95 加起来或写成 0。
	if orphanP50.Valid || orphanP95.Valid {
		t.Errorf("补出来的合并行分位数 = p50 %+v / p95 %+v，期望 NULL（分位数不可加）",
			orphanP50, orphanP95)
	}
}
