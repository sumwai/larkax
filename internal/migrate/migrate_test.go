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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
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
		INSERT INTO channels (name, type, base_url, models)
		VALUES ('migr-00057', 'openai-compatible', 'https://api.example.com/v1', ARRAY['m'])
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

// carryoverMigration 是期末结转那条迁移。
const carryoverMigration = "00065_plan_carryover.sql"

// 00065 的上下往返：四个列与配置键跟着走，分录类型的约束在两侧都对。
func TestPlanCarryoverMigrationRoundtrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	version, err := goose.NumericComponent(carryoverMigration)
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

	// 用例中途红掉会把库停在回滚态，本包之后的所有用例都会跟着红——收尾必须
	// 把版本推回最新。
	t.Cleanup(func() {
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Errorf("收尾时把库推回最新失败: %v", err)
		}
	})

	columns := func() map[string]bool {
		t.Helper()
		rows, err := db.QueryContext(ctx, `
			SELECT column_name FROM information_schema.columns
			 WHERE table_name = 'user_plans'`)
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
	settingValue := func() (string, error) {
		var raw string
		err := db.QueryRowContext(ctx,
			`SELECT value::text FROM settings WHERE key = 'plans.carryover_bp'`).Scan(&raw)
		return raw, err
	}
	// 试着写一条 plan_carry 分录：约束放行就成功，不放行就报错。写完删掉，
	// 否则 down 的守卫会拒绝回滚。
	canInsertCarryEntry := func() error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
			VALUES (gen_random_uuid(), 'carryover:1', 'CNY', 1, 'plan_carry')`)
		return err
	}

	// 1. 前进态：四列在、配置键在、新分录类型放行。
	cols := columns()
	for _, name := range []string{"carried_quota", "carried_amount", "carryout_quota", "carryout_amount"} {
		if !cols[name] {
			t.Errorf("迁移之后 user_plans 缺列 %s", name)
		}
	}
	if got, err := settingValue(); err != nil || got != "4000" {
		t.Errorf("plans.carryover_bp = %q, %v；期望 4000", got, err)
	}
	if err := canInsertCarryEntry(); err != nil {
		t.Errorf("迁移之后 plan_carry 分录应被约束放行: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM ledger_entries WHERE entry_type = 'plan_carry'`); err != nil {
		t.Fatalf("清理试写的分录失败: %v", err)
	}

	// 2. 回滚态：四列与配置键都没了，旧约束拒绝新类型。
	down()
	cols = columns()
	for _, name := range []string{"carried_quota", "carried_amount", "carryout_quota", "carryout_amount"} {
		if cols[name] {
			t.Errorf("回滚之后 user_plans 仍有列 %s", name)
		}
	}
	if _, err := settingValue(); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("回滚之后 plans.carryover_bp 应不存在，实际 err=%v", err)
	}
	if err := canInsertCarryEntry(); err == nil {
		t.Error("回滚之后 plan_carry 分录应被旧约束拒绝")
	}

	// 3. 再前进：恢复原样。
	up()
	cols = columns()
	if !cols["carried_quota"] || !cols["carryout_amount"] {
		t.Error("再次前进之后四列应当回来")
	}
}

// accountBelowFloorMigration 是科目余额下界那条迁移。
const accountBelowFloorMigration = "00066_account_below_floor.sql"

// 00066 的上下往返：kind 取值与运行记录上的计数列跟着走。
//
// 回滚那一侧有一条**守卫**要一起验：down 先把用了新 kind 的 findings 删掉，
// 再把旧约束加回去。少了那个 DELETE，约束加不回去（库里的行不满足新的取值
// 集合），down 直接失败——而「回滚失败」与「回滚成功」在只看版本号的时候
// 长得一模一样。
func TestAccountBelowFloorMigrationRoundtrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	version, err := goose.NumericComponent(accountBelowFloorMigration)
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

	// 用例中途红掉会把库停在回滚态，本包之后的所有用例都会跟着红——收尾必须
	// 把版本推回最新。
	t.Cleanup(func() {
		if err := goose.UpContext(context.Background(), db, "."); err != nil {
			t.Errorf("收尾时把库推回最新失败: %v", err)
		}
	})

	hasColumn := func() bool {
		t.Helper()
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				 WHERE table_name = 'reconcile_runs'
				   AND column_name = 'account_below_floors')`).Scan(&exists); err != nil {
			t.Fatalf("查列是否存在失败: %v", err)
		}
		return exists
	}

	// findings 的 last_run_id 有外键指向 reconcile_runs，先写一行当目标。
	var runID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO reconcile_runs (watermark) VALUES (now()) RETURNING id`).Scan(&runID); err != nil {
		t.Fatalf("写运行记录失败: %v", err)
	}
	// 写一条新 kind 的 finding：约束放行就成功，不放行就报错。
	insertFinding := func() error {
		_, insertErr := db.ExecContext(ctx, `
			INSERT INTO reconcile_findings (scope, kind, last_run_id)
			VALUES ('floor:carryover:1', 'account_below_floor', $1)`, runID)
		return insertErr
	}
	deleteFindings := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`DELETE FROM reconcile_findings WHERE kind = 'account_below_floor'`); err != nil {
			t.Fatalf("清理试写的 finding 失败: %v", err)
		}
	}

	// 1. 前进态：列在、新 kind 放行。
	if !hasColumn() {
		t.Error("迁移之后 reconcile_runs 缺列 account_below_floors")
	}
	if err := insertFinding(); err != nil {
		t.Errorf("迁移之后 account_below_floor 应被约束放行: %v", err)
	}

	// 2. 回滚态：down 把这一类 finding 删掉，列没了，旧约束拒绝新 kind。
	down()
	if hasColumn() {
		t.Error("回滚之后 reconcile_runs 仍有列 account_below_floors")
	}
	if err := insertFinding(); err == nil {
		t.Error("回滚之后 account_below_floor 应被旧约束拒绝")
		deleteFindings()
	}

	// 3. 再前进：列与取值都回来。
	up()
	if !hasColumn() {
		t.Error("再次前进之后 account_below_floors 应当回来")
	}
	if err := insertFinding(); err != nil {
		t.Errorf("再次前进之后 account_below_floor 应被放行: %v", err)
	}
	deleteFindings()
}

// accountsMigration 是把凭证从渠道行拆成账号实体的那条迁移。版本号由文件名
// 算出来，重编号时两处不会分叉。
const accountsMigration = "00067_accounts.sql"

// TestAccountsEntityMigration 钉住 00067 的两头：前进时存量凭证原样搬进账号、
// 回滚时搬回渠道行且两张表**真的消失**、再前进时重新搬一遍。
//
// # 为什么回滚要断言「表不在了」而不是「语句没报错」
//
// 只调用一次 Down 再看到退出码是 0，证明不了它清掉了自己建的东西：漏写
// DROP 也能跑完。所以这里查 to_regclass 为 NULL，再前进一次确认库还能用。
//
// # 回滚态的门槛比 00001 松一格
//
// Down 把 credential_enc 加回来时是可空的：零账号渠道在旧模型里没法表示，
// 伪造一份密文或删掉这一行都比留 NULL 更坏（见迁移里的注释）。所以本用例
// 在回滚态插入的渠道自己带凭证，走的是与存量数据完全相同的那条搬移路径。
func TestAccountsEntityMigration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// 这一层不声明函数级 err：下面每个查询都用 `if err := ...` 自己收口，
	// 外层留一个 err 只会被逐条遮蔽（shadow），而遮蔽之后它仍被读到。
	version, versionErr := goose.NumericComponent(accountsMigration)
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
	down := func() {
		t.Helper()
		if err := goose.DownToContext(ctx, db, ".", previous); err != nil {
			t.Fatalf("回滚到 %d 失败: %v", previous, err)
		}
	}

	// 用例中途红掉会把库停在回滚态，本包之后的所有用例都会跟着红——收尾
	// 必须把版本推回最新，并清掉夹具。
	const (
		channelName = "migr-00067"
		probeName   = "migr-00067-probe"
	)
	// 迁移给存量渠道建的账号名是 `channel-<渠道 id>`。名字在 Go 侧拼好再
	// 传成 text：把 int64 参数直接丢给 `'channel-' || $1::text` 时 pgx 编不过。
	channelAccountName := func(id int64) string {
		return "channel-" + strconv.FormatInt(id, 10)
	}
	var channelID int64
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := goose.UpContext(cleanupCtx, db, "."); err != nil {
			t.Errorf("收尾时把库推回最新失败: %v", err)
		}
		if channelID != 0 {
			// 账号先删：channel_accounts 对两张表都是 CASCADE，谁先都行，
			// 但渠道删掉之后按名字找账号就只剩这一条路了。
			if _, err := db.ExecContext(cleanupCtx,
				`DELETE FROM accounts WHERE name = $1 OR name = $2`,
				channelAccountName(channelID), probeName); err != nil {
				t.Errorf("收尾时删测试账号失败: %v", err)
			}
			if _, err := db.ExecContext(cleanupCtx,
				`DELETE FROM channels WHERE id = $1`, channelID); err != nil {
				t.Errorf("收尾时删测试渠道失败: %v", err)
			}
		}
	})

	tableExists := func(name string) bool {
		t.Helper()
		var reg sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT to_regclass($1)::text`, name).Scan(&reg); err != nil {
			t.Fatalf("查表 %s 是否存在失败: %v", name, err)
		}
		return reg.Valid
	}
	columnExists := func(table, column string) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = $1 AND column_name = $2`,
			table, column).Scan(&n); err != nil {
			t.Fatalf("查列 %s.%s 是否存在失败: %v", table, column, err)
		}
		return n > 0
	}

	// 一份**真能被解开**的凭证：迁移只搬字节，所以迁过去的密文必须仍由
	// 原来的主密钥解得开。用随机字节当密文的话，这一步证明不了任何事。
	key := bytes.Repeat([]byte{0x5a}, config.KeySize)
	sealer, sealerErr := credential.NewSealer([]config.MasterKey{{ID: "mig-00067", Key: key}})
	if sealerErr != nil {
		t.Fatalf("构造封装器失败: %v", sealerErr)
	}
	const plaintext = "migration-fixture-credential-00067"
	envelope, envelopeErr := sealer.Seal([]byte(plaintext))
	if envelopeErr != nil {
		t.Fatalf("封装凭证失败: %v", envelopeErr)
	}
	hint := credential.Hint(plaintext)
	if hint != "0067" {
		t.Fatalf("提示 = %q，期望 0067（用例自己的前提就不成立）", hint)
	}

	// 1. 退回旧结构，按旧结构写一条带凭证的渠道。
	down()
	if tableExists("accounts") || tableExists("channel_accounts") {
		t.Fatal("回滚态仍有账号表：用例的前提不成立")
	}
	if !columnExists("channels", "credential_enc") {
		t.Fatal("回滚态 channels 缺 credential_enc 列：用例的前提不成立")
	}
	if err := db.QueryRowContext(ctx, `
		INSERT INTO channels (name, type, base_url, credential_enc, credential_hint, models)
		VALUES ($1, 'openai-compatible', 'https://api.example.com/v1', $2, $3, ARRAY['m'])
		RETURNING id`, channelName, envelope, hint).Scan(&channelID); err != nil {
		t.Fatalf("按旧结构插入渠道失败: %v", err)
	}

	// 2. 前进：凭证搬进账号、关联建好、渠道行不再有凭证列。
	up()
	for _, table := range []string{"accounts", "channel_accounts"} {
		if !tableExists(table) {
			t.Fatalf("前进之后缺表 %s", table)
		}
	}
	if columnExists("channels", "credential_enc") || columnExists("channels", "credential_hint") {
		t.Error("前进之后 channels 仍有凭证列：两条写路径会各自写一份")
	}

	migratedName := channelAccountName(channelID)
	var (
		migratedAccountID int64
		migratedEnc       []byte
		migratedHint      sql.NullString
	)
	if err := db.QueryRowContext(ctx, `
		SELECT id, credential_enc, credential_hint FROM accounts
		 WHERE name = $1`, migratedName,
	).Scan(&migratedAccountID, &migratedEnc, &migratedHint); err != nil {
		t.Fatalf("前进之后找不到账号 %s: %v", migratedName, err)
	}
	if !bytes.Equal(migratedEnc, envelope) {
		t.Error("搬进账号的密文与渠道行里那份不一致：迁移应当是原样复制")
	}
	if !migratedHint.Valid || migratedHint.String != hint {
		t.Errorf("账号提示 = %+v，期望 %q", migratedHint, hint)
	}
	opened, openErr := sealer.Open(migratedEnc)
	if openErr != nil {
		t.Fatalf("迁移后的密文解不开：%v", openErr)
	}
	if string(opened) != plaintext {
		t.Errorf("迁移后的明文 = %q，期望 %q", opened, plaintext)
	}

	var weight int
	if err := db.QueryRowContext(ctx, `
		SELECT weight FROM channel_accounts
		 WHERE channel_id = $1 AND account_id = $2`, channelID, migratedAccountID,
	).Scan(&weight); err != nil {
		t.Fatalf("前进之后没有渠道与账号的关联: %v", err)
	}
	if weight != 1 {
		t.Errorf("迁移建出来的关联权重 = %d，期望 1", weight)
	}

	// 两张表都能写：表在而不可写的话，前进态一样是坏的。
	var probeID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, credential_enc) VALUES ($1, $2) RETURNING id`,
		probeName, envelope).Scan(&probeID); err != nil {
		t.Fatalf("前进之后写 accounts 失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_accounts (channel_id, account_id) VALUES ($1, $2)`,
		channelID, probeID); err != nil {
		t.Fatalf("前进之后写 channel_accounts 失败: %v", err)
	}

	// 3. 回滚：两张表**从库里消失**，凭证回到渠道行且仍能解开。
	//
	//    探针账号此时还挂在渠道上：回滚按账号 id 最小者搬回，探针的 id 比
	//    迁移建出来的那个大，所以取到的是迁移那一份。
	down()
	for _, table := range []string{"accounts", "channel_accounts"} {
		if tableExists(table) {
			t.Errorf("回滚之后表 %s 仍然存在", table)
		}
	}
	if columnExists("accounts", "credential_enc") {
		t.Error("回滚之后 accounts 仍在 information_schema 里")
	}
	var restoredEnc []byte
	var restoredHint sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT credential_enc, credential_hint FROM channels WHERE id = $1`, channelID,
	).Scan(&restoredEnc, &restoredHint); err != nil {
		t.Fatalf("回滚之后读渠道凭证失败: %v", err)
	}
	if !bytes.Equal(restoredEnc, envelope) {
		t.Error("回滚之后渠道里的密文与写入时不一致")
	}
	if !restoredHint.Valid || restoredHint.String != hint {
		t.Errorf("回滚之后渠道提示 = %+v，期望 %q", restoredHint, hint)
	}
	opened, openErr = sealer.Open(restoredEnc)
	if openErr != nil {
		t.Fatalf("回滚之后渠道密文解不开：%v", openErr)
	}
	if string(opened) != plaintext {
		t.Errorf("回滚之后的明文 = %q，期望 %q", opened, plaintext)
	}

	// 4. 再前进：表与迁移生成的账号都回来，且两张表仍可写。
	up()
	for _, table := range []string{"accounts", "channel_accounts"} {
		if !tableExists(table) {
			t.Fatalf("再次前进之后缺表 %s", table)
		}
	}
	var (
		againID  int64
		againEnc []byte
	)
	if err := db.QueryRowContext(ctx, `
		SELECT id, credential_enc FROM accounts
		 WHERE name = $1`, channelAccountName(channelID),
	).Scan(&againID, &againEnc); err != nil {
		t.Fatalf("再次前进之后找不到迁移生成的账号: %v", err)
	}
	if !bytes.Equal(againEnc, envelope) {
		t.Error("再次前进之后搬过来的密文与渠道行里那份不一致")
	}
	opened, openErr = sealer.Open(againEnc)
	if openErr != nil {
		t.Fatalf("再次前进之后的密文解不开：%v", openErr)
	}
	if string(opened) != plaintext {
		t.Errorf("再次前进之后的明文 = %q，期望 %q", opened, plaintext)
	}
	var againChannelID int64
	if err := db.QueryRowContext(ctx, `
		SELECT channel_id FROM channel_accounts WHERE account_id = $1`, againID,
	).Scan(&againChannelID); err != nil {
		t.Fatalf("再次前进之后没有重建关联: %v", err)
	}
	if againChannelID != channelID {
		t.Errorf("再次建出来的关联指向渠道 %d，期望 %d", againChannelID, channelID)
	}
	// Down 丢掉的表内容不会跟着回来：探针账号是新表里从未存在过的那一行，
	// 再前进之后它仍然是「不存在」。
	var probeLeft int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM accounts WHERE name = $1`, probeName).Scan(&probeLeft); err != nil {
		t.Fatalf("数探针账号失败: %v", err)
	}
	if probeLeft != 0 {
		t.Errorf("再次前进之后探针账号还在（%d 行）：Down 没有真的清掉表", probeLeft)
	}
	// 可写：表在、关联能建。
	var recheckID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, credential_enc) VALUES ($1, $2) RETURNING id`,
		probeName, envelope).Scan(&recheckID); err != nil {
		t.Fatalf("再次前进之后写 accounts 失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO channel_accounts (channel_id, account_id) VALUES ($1, $2)`,
		channelID, recheckID); err != nil {
		t.Fatalf("再次前进之后写 channel_accounts 失败: %v", err)
	}
}
