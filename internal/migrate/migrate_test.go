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
	"testing"

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
