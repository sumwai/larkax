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
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// planReservedMigration 是给用量事件补额度包面值的那条迁移。版本号由文件名
// 算出来，重编号时两处不会分叉。
const planReservedMigration = "00058_usage_event_plan_reserved.sql"

// TestUsageEventsPlanReservedMigration 钉住 00058 在父表与**每个已有分区**上的
// 结构，以及历史行的回填。
//
// usage_events 是分区表：只在父表上加列、分区上没跟上的话，读侧按分区扫时才会
// 报「列不存在」，而那时离迁移已经很远。所以这里逐个分区查 pg_attribute，不
// 只看父表。
//
// 判据：
//
//   - down 之后父表与每个分区都没有这一列；
//   - up 之后父表与每个分区都有；
//   - 回填按账单搬值：账单表里有同名请求的老行拿到那个面值，没有的保持 0
//     （两条一起才说明是「按请求搬」而不是「全表刷一个数」）；
//   - 再 down 一次，这一列从所有分区上一起消失；再 up 一次，回填重跑仍成立。
func TestUsageEventsPlanReservedMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	version, err := goose.NumericComponent(planReservedMigration)
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

	// usage_events 的分区：两个预建的月份与兜底分区。它们由 00001 建好，
	// 本迁移不该改变这张清单。
	relations := []string{
		"usage_events",
		"usage_events_2026_10",
		"usage_events_2026_11",
		"usage_events_default",
	}
	hasColumn := func(rel string) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_attribute
			 WHERE attrelid = $1::regclass
			   AND attname = 'plan_reserved_amount'
			   AND attnum > 0
			   AND NOT attisdropped`, rel).Scan(&n); err != nil {
			t.Fatalf("读 %s 的列失败: %v", rel, err)
		}
		return n > 0
	}
	assertColumn := func(want bool, when string) {
		t.Helper()
		for _, rel := range relations {
			if got := hasColumn(rel); got != want {
				t.Errorf("%s：%s 上这一列在不在 = %v，期望 %v", when, rel, got, want)
			}
		}
	}

	// 收尾：把库推回最新并删掉夹具行。中途红掉时，本包其它用例不该被留在
	// 旧结构上。
	//
	// 两条老用量事件各钉一件事：requestID 在账单表里有同名请求，回填要把它
	// 搬过来；orphanID 没有，回填后仍是 0。
	const (
		requestID = "migr-00058"
		orphanID  = "migr-00058-orphan"
		userName  = "migr-00058-user"
	)
	dropFixtures := func(cleanupCtx context.Context) error {
		if _, err := db.ExecContext(cleanupCtx,
			`DELETE FROM usage_events WHERE request_id IN ($1, $2)`,
			requestID, orphanID); err != nil {
			return err
		}
		if _, err := db.ExecContext(cleanupCtx,
			`DELETE FROM billing_requests WHERE request_id = $1`, requestID); err != nil {
			return err
		}
		_, err := db.ExecContext(cleanupCtx,
			`DELETE FROM users WHERE username = $1`, userName)
		return err
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := goose.UpContext(cleanupCtx, db, "."); err != nil {
			t.Errorf("收尾时把库推回最新失败: %v", err)
		}
		if err := dropFixtures(cleanupCtx); err != nil {
			t.Errorf("收尾时删夹具行失败: %v", err)
		}
	})

	// 1. 回到迁移前，写两条「老数据」。
	down()
	assertColumn(false, "回滚之后")

	// 上一次中途失败可能留下夹具行；先清掉再写，重跑才不会撞唯一键。
	if err := dropFixtures(ctx); err != nil {
		t.Fatalf("清理旧的夹具行失败: %v", err)
	}
	var userID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO users (username) VALUES ($1) RETURNING id`,
		userName).Scan(&userID); err != nil {
		t.Fatalf("写迁移前的用户失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_requests
			(request_id, user_id, canonical_model, protocol, plan_reserved_amount, status)
		VALUES ($1, $2, 'm', 'openai', 2.50, 'settled')`,
		requestID, userID); err != nil {
		t.Fatalf("写迁移前的账单行失败: %v", err)
	}
	for _, id := range []string{requestID, orphanID} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO usage_events
				(request_id, user_id, canonical_model, billing_path, cost, created_at)
			VALUES ($1, $2, 'm', 'measured', 1.00, '2026-10-15T00:00:00Z')`,
			id, userID); err != nil {
			t.Fatalf("写迁移前的用量事件 %s 失败: %v", id, err)
		}
	}

	planReservedOf := func(id string) string {
		t.Helper()
		var raw string
		if err := db.QueryRowContext(ctx, `
			SELECT plan_reserved_amount::text FROM usage_events WHERE request_id = $1`,
			id).Scan(&raw); err != nil {
			t.Fatalf("读 %s 的面值失败: %v", id, err)
		}
		return raw
	}
	assertBackfilled := func(when string) {
		t.Helper()
		if got := planReservedOf(requestID); got != "2.50000000" {
			t.Errorf("%s：账单表里有同名请求的那条读出 %s，期望 2.50000000", when, got)
		}
		if got := planReservedOf(orphanID); got != "0.00000000" {
			t.Errorf("%s：账单表里没有的老行读出 %s，期望 0（没有可搬的值）", when, got)
		}
	}

	// 2. up 之后，父表与每个分区都有这一列；回填按账单搬值，缺账单的仍是 0。
	up()
	assertColumn(true, "前进之后")
	assertBackfilled("前进之后")

	// 3. 再 down 一次：这一列从父表与所有分区上一起消失。
	down()
	assertColumn(false, "再次回滚之后")

	// 4. 再 up 一次：列删掉后回填重跑，两条老行的结果与第一次一致。
	up()
	assertColumn(true, "第二次前进之后")
	assertBackfilled("第二次前进之后")
}

// inboundAliasesMigration 是建入站别名表的那条迁移。
const inboundAliasesMigration = "00060_inbound_model_aliases.sql"

// TestInboundModelAliasesMigration 钉住这张表的形状与回滚方向。
//
// 判据：
//
//   - up 之后表在，主键是 alias，canonical_name 非空；
//   - 同一个 alias 插两次被主键拒（唯一性靠主键，不靠另建的索引）；
//   - **没有**指向 model_catalog 的外键——有外键时「别名指向一个已删掉的规范名」
//     这种数据进不来，而 model trace 要报的正是这种断链；
//   - down 之后表消失，再 up 之后表与约束一起回来。
func TestInboundModelAliasesMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	version, err := goose.NumericComponent(inboundAliasesMigration)
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

	exists := func() bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = current_schema() AND table_name = 'inbound_model_aliases'`,
		).Scan(&n); err != nil {
			t.Fatalf("查表是否存在失败: %v", err)
		}
		return n > 0
	}
	// 外键数：这张表按设计一条都没有（见 00060 迁移的注释）。
	foreignKeys := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.table_constraints
			 WHERE table_schema = current_schema()
			   AND table_name = 'inbound_model_aliases'
			   AND constraint_type = 'FOREIGN KEY'`).Scan(&n); err != nil {
			t.Fatalf("数外键失败: %v", err)
		}
		return n
	}

	// 收尾：把库推回最新。中途红掉时，本包别的用例不该被留在旧结构上。
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := goose.UpContext(cleanupCtx, db, "."); err != nil {
			t.Logf("收尾时前进失败（库可能已经不在旧版本上）：%v", err)
		}
	})

	// 1. down 之后表不存在。
	down()
	if exists() {
		t.Fatal("回滚之后 inbound_model_aliases 还在")
	}

	// 2. up 之后表回来，外键为零。
	up()
	if !exists() {
		t.Fatal("前进之后 inbound_model_aliases 不存在")
	}
	if got := foreignKeys(); got != 0 {
		t.Errorf("这张表有 %d 条外键，期望 0：有外键时「别名指向一个已删掉的规范名」这种数据进不来，"+
			"而 model trace 要报的正是那种断链", got)
	}

	// 3. 别名是主键：同一个 alias 插两次被拒。
	alias := "migr-00060-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM inbound_model_aliases WHERE alias = $1`, alias)
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inbound_model_aliases (alias, canonical_name) VALUES ($1, $2)`,
		alias, "canonical-a"); err != nil {
		t.Fatalf("第一次插入失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inbound_model_aliases (alias, canonical_name) VALUES ($1, $2)`,
		alias, "canonical-b"); err == nil {
		t.Error("同一个别名插了两次却没被拒：alias 应当唯一")
	}

	// 4. 断链允许存在：规范名不在目录里也能插进去（这正是 trace 要报的状态）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inbound_model_aliases (alias, canonical_name) VALUES ($1, $2)`,
		alias+"-dangling", "no-such-model"); err != nil {
		t.Errorf("指向不存在规范名的别名插不进去（%v）：那会把一个可诊断的断链变成不可表达的数据", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM inbound_model_aliases WHERE alias = $1`, alias+"-dangling")
	})

	// 5. 再 down 一次、再 up 一次：表与约束一起回来。
	down()
	if exists() {
		t.Fatal("第二次回滚之后表还在")
	}
	up()
	if !exists() {
		t.Fatal("第二次前进之后表不存在")
	}
}

// channelsModelsGINMigration 是给渠道模型清单加 GIN 索引的那条迁移。
const channelsModelsGINMigration = "00063_channels_models_gin.sql"

// TestChannelsModelsGINMigration 钉住这把索引的形状与往返：up 之后它在、
// 方法为 GIN、建在 channels.models 上；down 之后它消失；再 up 又回来。
//
// # 为什么非要在真库上跑
//
// `CREATE INDEX … USING gin` 是 PostgreSQL 的语法与实现，单测里没有任何东西
// 能替它回答「插进去的是不是一把 GIN 索引」。判据是 pg_index 里那一行：
// 写成默认的 btree 或者建到别的列上，迁移照样跑完、版本号照样前进，而
// SuppliesFor 的 `models @> ARRAY[…]` 仍然整表扫。
//
// 没有 LARKAX_DB_DSN 时跳过（见 testdb.Open），CI 的集成测试门禁里会跑。
func TestChannelsModelsGINMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	version, err := goose.NumericComponent(channelsModelsGINMigration)
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

	// indexState 返回索引的访问方法、所在表与第一列；索引不存在时 ok 为 false。
	// 三样一起取，是为了让「存在但形状不对」也能被同一条断言报出来。
	indexState := func() (method, table, column string, ok bool) {
		t.Helper()
		err := db.QueryRowContext(ctx, `
			SELECT am.amname, tbl.relname, att.attname
			  FROM pg_class idx
			  JOIN pg_index i ON i.indexrelid = idx.oid
			  JOIN pg_class tbl ON tbl.oid = i.indrelid
			  JOIN pg_am am ON am.oid = idx.relam
			  JOIN pg_attribute att ON att.attrelid = tbl.oid AND att.attnum = i.indkey[0]
			 WHERE idx.relname = 'channels_models_gin_idx'`).
			Scan(&method, &table, &column)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", "", false
		}
		if err != nil {
			t.Fatalf("读索引定义失败: %v", err)
		}
		return method, table, column, true
	}

	// 收尾：把库推回最新。中途红掉时，本包别的用例不该被留在旧结构上。
	t.Cleanup(func() {
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Logf("收尾时前进失败（库可能已经不在旧版本上）：%v", err)
		}
	})

	// 1. 全量 up 之后索引在，是 GIN，建在 channels.models 上。
	up()
	method, table, column, ok := indexState()
	if !ok {
		t.Fatal("全量迁移之后 channels_models_gin_idx 不存在")
	}
	if method != "gin" {
		t.Errorf("channels_models_gin_idx 的访问方法 = %q，期望 gin（btree 索引支撑不了"+
			"`models @> ARRAY[…]`）", method)
	}
	if table != "channels" || column != "models" {
		t.Errorf("channels_models_gin_idx 建在 %s(%s) 上，期望 channels(models)", table, column)
	}

	// 2. 退到本迁移的前一条：索引随之消失。
	down()
	if _, _, _, stillThere := indexState(); stillThere {
		t.Fatal("回滚之后 channels_models_gin_idx 还在")
	}

	// 3. 再 up 一次：索引与它的形状一起回来。
	up()
	method, table, column, ok = indexState()
	if !ok {
		t.Fatal("第二次前进之后 channels_models_gin_idx 不存在")
	}
	if method != "gin" || table != "channels" || column != "models" {
		t.Errorf("第二次前进之后索引 = %s on %s(%s)，期望 gin on channels(models)",
			method, table, column)
	}
}

// paymentOrdersGatewayTxnMigration 是给通道流水号加部分唯一索引的那条迁移。
const paymentOrdersGatewayTxnMigration = "00064_payment_orders_gateway_txn_unique.sql"

// paymentOrdersGatewayTxnIndex 是那条迁移建出来的索引名。
//
// 这里写的是字面量而不是从别处引用：这是迁移文件里那一行的期望值，从被测代码
// 取就等于把同一个错误抄两遍。internal/payment 的翻译逻辑认的也是这个名字，
// 两边分叉时那一边的用例会红。
const paymentOrdersGatewayTxnIndex = "payment_orders_gateway_txn_uniq_idx"

// TestPaymentOrdersGatewayTxnUniqueMigration 钉住这条迁移的四件事：
// 同一通道的重复流水号插不进第二行、两行空流水号能并存、索引的形状与
// 被删掉的那条普通索引、以及 down / up 往返。
//
// # 为什么要断言约束名而不是只断 23505
//
// 23505 只说明「某个唯一约束被违反」：`order_no` 上那条唯一键也回 23505。
// 只断错误码的话，把唯一性建到别的列上、或者那条索引压根没建，用例照样可能
// 因为单号撞上而变绿（见 internal/catalog/admin.go 里对同一件事的说明）。
//
// 没有 LARKAX_DB_DSN 时跳过（见 testdb.Open），CI 的集成测试门禁里会跑。
func TestPaymentOrdersGatewayTxnUniqueMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	version, versionErr := goose.NumericComponent(paymentOrdersGatewayTxnMigration)
	if versionErr != nil {
		t.Fatalf("从迁移文件名读版本号失败: %v", versionErr)
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
	// down 退到本迁移的前一条。号再往后加时，它会把后面那些迁移一并撤掉；
	// 收尾那次 up 会把库推回最新，所以用例仍然成立。
	down := func() {
		t.Helper()
		if err := goose.DownToContext(ctx, db, ".", previous); err != nil {
			t.Fatalf("回滚到 %d 失败: %v", previous, err)
		}
	}

	// 收尾：先清掉本用例插的行，再把库推回最新。中途红掉时若留着重复行，
	// 收尾那一步重建唯一索引会失败，本包别的用例就被留在旧结构上了。
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM payment_orders WHERE order_no LIKE 'ORD-797-%'`)
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Logf("收尾时前进失败（库可能已经不在旧版本上）：%v", err)
		}
	})

	const userID = int64(797001)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		userID, "migrate797", "migrate797@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	// insert 落一行支付单。txn 为 nil 表示这一列写 NULL。
	insert := func(orderNo string, txn any) error {
		t.Helper()
		_, err := db.ExecContext(ctx, `
			INSERT INTO payment_orders (order_no, user_id, gateway, amount, gateway_txn_id)
			VALUES ($1, $2, 'stub', 66, $3)`, orderNo, userID, txn)
		return err
	}

	up()

	// 1. 同一个 (gateway, gateway_txn_id) 插不进第二行，且撞的是这条索引。
	if err := insert("ORD-797-DUP-1", "TXN-797-DUP"); err != nil {
		t.Fatalf("第一行重复流水号的支付单写不进去: %v", err)
	}
	dupErr := insert("ORD-797-DUP-2", "TXN-797-DUP")
	var pgErr *pgconn.PgError
	if !errors.As(dupErr, &pgErr) {
		t.Fatalf("第二行同流水号的插入错误 = %v，期望一个 PostgreSQL 错误（23505）", dupErr)
	}
	if pgErr.Code != "23505" {
		t.Errorf("第二行同流水号的插入错误码 = %q，期望 23505", pgErr.Code)
	}
	if pgErr.ConstraintName != paymentOrdersGatewayTxnIndex {
		t.Errorf("撞的约束 = %q，期望 %q", pgErr.ConstraintName, paymentOrdersGatewayTxnIndex)
	}

	// 2. 两行空流水号能并存：部分索引不覆盖 gateway_txn_id IS NULL 的行。
	if err := insert("ORD-797-NULL-1", nil); err != nil {
		t.Fatalf("第一行空流水号写不进去: %v", err)
	}
	if err := insert("ORD-797-NULL-2", nil); err != nil {
		t.Errorf("第二行空流水号应当能与第一行并存，实际写不进去: %v", err)
	}

	// 3. 索引的形状：唯一、带条件、列顺序是 (gateway, gateway_txn_id)；
	// 同时确认 00001 里那条同列的普通索引已经被删掉。
	var unique, hasPredicate bool
	var first, second string
	if err := db.QueryRowContext(ctx, `
		SELECT i.indisunique, i.indpred IS NOT NULL,
		       (SELECT attname FROM pg_attribute
		         WHERE attrelid = i.indrelid AND attnum = i.indkey[0]),
		       (SELECT attname FROM pg_attribute
		         WHERE attrelid = i.indrelid AND attnum = i.indkey[1])
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = $1`, paymentOrdersGatewayTxnIndex).
		Scan(&unique, &hasPredicate, &first, &second); err != nil {
		t.Fatalf("读索引 %s 的定义失败: %v", paymentOrdersGatewayTxnIndex, err)
	}
	if !unique {
		t.Errorf("%s 不是唯一索引", paymentOrdersGatewayTxnIndex)
	}
	if !hasPredicate {
		t.Errorf("%s 没有 WHERE 条件——那样空流水号的行会互相排斥", paymentOrdersGatewayTxnIndex)
	}
	if first != "gateway" || second != "gateway_txn_id" {
		t.Errorf("%s 的列 = (%s, %s)，期望 (gateway, gateway_txn_id)",
			paymentOrdersGatewayTxnIndex, first, second)
	}
	var oldIndex sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass('payment_orders_gateway_idx')::text`).Scan(&oldIndex); err != nil {
		t.Fatalf("查旧索引是否存在失败: %v", err)
	}
	if oldIndex.Valid {
		t.Errorf("payment_orders_gateway_idx 还在：同列的普通索引应当随这条迁移删掉")
	}

	// 4. 退到前一条：新索引消失，旧索引回来；再前进一次，两者对调。
	down()
	var newIndex sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass($1)::text`, paymentOrdersGatewayTxnIndex).Scan(&newIndex); err != nil {
		t.Fatalf("查新索引是否存在失败: %v", err)
	}
	if newIndex.Valid {
		t.Errorf("回滚之后 %s 还在", paymentOrdersGatewayTxnIndex)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass('payment_orders_gateway_idx')::text`).Scan(&oldIndex); err != nil {
		t.Fatalf("查旧索引是否存在失败: %v", err)
	}
	if !oldIndex.Valid {
		t.Errorf("回滚之后 payment_orders_gateway_idx 没有回来")
	}

	// 两行空流水号在回滚之后仍然并存，前进时不会挡住重建唯一的索引。
	up()
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass($1)::text`, paymentOrdersGatewayTxnIndex).Scan(&newIndex); err != nil {
		t.Fatalf("查新索引是否存在失败: %v", err)
	}
	if !newIndex.Valid {
		t.Errorf("第二次前进之后 %s 不存在", paymentOrdersGatewayTxnIndex)
	}
}
