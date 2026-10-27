package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/migrate"
)

// 这一组用例钉的是 `migrate up --allow-missing` 的两半：
//
//	1. 开关只接在 up 上，其余动作带着它一律按参数错报（退出码 2），且拦在连库之前；
//	2. 库停在「当前版本 50、49 缺失」时，不打开关仍然报原来那条错，打开关才补上 49，
//	   再应用所有更大的未应用迁移，补了哪几条的文件名出现在输出里。
//
// 第 2 条走真库：版本表的形状、goose 的补应用顺序、00049 的 SQL 能不能在 50 之后
// 补跑，都只有真的 PostgreSQL 答得上来。没有 DSN 时整段跳过（CI 里会跑）。

// useEmbeddedMigrations 把 goose 指向二进制里内嵌的那份迁移。
func useEmbeddedMigrations(t *testing.T) {
	t.Helper()
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置迁移方言失败: %v", err)
	}
	goose.SetBaseFS(migrate.FS())
}

// TestMigrateAllowMissingOnlyOnUp 断言开关只属于 up。
//
// 特意把 DSN 清空：这条错误若发生在连库之后，报出来的会是配置无效或连不上库
// （退出码 1 / 3），而不是参数错——脚本按退出码分派时正会走错分支。
func TestMigrateAllowMissingOnlyOnUp(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")

	cases := [][]string{
		{"down", "--allow-missing"},
		{"down-to", "0", "--allow-missing"},
		{"up-to", "0", "--allow-missing"},
		{"status", "--allow-missing"},
		{"version", "--allow-missing"},
		{"create", "--allow-missing"},
		// 取值是关的也算「带了它」：用错的是子命令，不是取值。
		{"down", "--allow-missing=false"},
	}

	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := runMigrate(args)
			if err == nil {
				t.Fatalf("migrate %s 应当被挡成参数错", strings.Join(args, " "))
			}
			if code := cli.ExitCode(err); code != cli.ExitUsage {
				t.Errorf("退出码 = %d，想要 %d（参数错）: %v", code, cli.ExitUsage, err)
			}
			if !strings.Contains(err.Error(), "--allow-missing") {
				t.Errorf("错误信息要点名这个开关: %v", err)
			}
		})
	}
}

// TestMigrateUpAllowMissingOnUpToDateDatabase 钉住「正常库上加开关不改变结果」。
//
// 同时验证开关写在子命令之前也认——它与全局选项同一套摘取规则。
func TestMigrateUpAllowMissingOnUpToDateDatabase(t *testing.T) {
	db := testDB(t)
	useEmbeddedMigrations(t)

	before := currentVersion(t, db)
	if err := runCaptured(t, func() error {
		return runMigrate([]string{"--allow-missing", "up"})
	}); err != nil {
		t.Fatalf("已经是最新的库上加 --allow-missing 不该失败: %v", err)
	}
	if after := currentVersion(t, db); after != before {
		t.Errorf("版本从 %d 变成了 %d：没有可补的迁移时不该动库", before, after)
	}
}

// TestMigrateUpAllowMissingAppliesOutOfOrderMigrations 复现 issue 里的现场：
// 库停在 50，二进制里有 49，但库里没有 49 的记录。
//
// 顺序：先把库推到 48，再单独应用 50（模拟「50 先合入并被部署」）。此时二进制里
// 还有比 50 大的未应用迁移，但 49 缺失，goose 默认拒绝前进。
func TestMigrateUpAllowMissingAppliesOutOfOrderMigrations(t *testing.T) {
	dsn := createScratchDB(t)
	t.Setenv("LARKAX_DB_DSN", dsn)
	t.Setenv("LARKAX_ROLE", "")
	t.Setenv("LARKAX_MASTER_KEYS", "")

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	prepareVersion50Without49(t, db)

	// 不加开关：仍然是原来那条错，库停在 50。
	upErr := runCaptured(t, func() error { return runMigrate([]string{"up"}) })
	if upErr == nil {
		t.Fatal("49 缺失时 migrate up 不加开关应当失败")
	}
	if !strings.Contains(upErr.Error(), "found 1 missing migrations before current version 50") {
		t.Errorf("错误信息应当点明缺的是哪一类迁移: %v", upErr)
	}
	if !strings.Contains(upErr.Error(), "00049_prepaid_plans.sql") {
		t.Errorf("错误信息应当点名缺失的迁移文件: %v", upErr)
	}
	if got := currentVersion(t, db); got != 50 {
		t.Fatalf("失败的那次 up 不该改版本，当前是 %d", got)
	}

	// 加开关：补应用 49，再应用所有比 50 大的未应用迁移。
	_, stderr := captureStreams(t, func() { upErr = runMigrate([]string{"up", "--allow-missing"}) })
	if upErr != nil {
		t.Fatalf("加 --allow-missing 后 up 应当成功: %v", upErr)
	}
	if !strings.Contains(stderr, "00049_prepaid_plans.sql") {
		t.Errorf("补应用的迁移文件名应当打在输出里，stderr 是 %q", stderr)
	}
	// 期望值从本树的内嵌迁移里取，不写死：补完之后版本应当一直走到本树、
	// 也就是这个二进制里的最大迁移号。
	useEmbeddedMigrations(t)
	wantVersion, err := latestEmbeddedMigration()
	if err != nil {
		t.Fatalf("读本树内嵌迁移的最大版本失败: %v", err)
	}
	if got := currentVersion(t, db); got != wantVersion {
		t.Errorf("补完之后版本 = %d，想要 %d（本树内嵌迁移的最大版本）", got, wantVersion)
	}
	if !tableExists(t, db, "plan_models") {
		t.Error("00049 建的表 plan_models 不在库里：那条迁移没有被补应用")
	}
	var chargeOrder string
	if err := db.QueryRowContext(context.Background(),
		`SELECT value FROM settings WHERE key = 'plans.charge_order'`).Scan(&chargeOrder); err != nil {
		t.Errorf("settings 里没有 plans.charge_order：%v", err)
	}
}

// prepareVersion50Without49 造出「当前版本 50、49 缺失」的库。
func prepareVersion50Without49(t *testing.T, db *sql.DB) {
	t.Helper()
	useEmbeddedMigrations(t)
	ctx := context.Background()

	if err := goose.UpToContext(ctx, db, ".", 48); err != nil {
		t.Fatalf("先把库推到 48 失败: %v", err)
	}

	migrations, err := goose.CollectMigrations(".", 0, goose.MaxVersion)
	if err != nil {
		t.Fatalf("读内嵌迁移清单失败: %v", err)
	}
	for _, m := range migrations {
		if m.Version != 50 {
			continue
		}
		if err := m.UpContext(ctx, db); err != nil {
			t.Fatalf("单独应用 %s 失败: %v", m.Source, err)
		}
		if got := currentVersion(t, db); got != 50 {
			t.Fatalf("造出来的版本 = %d，想要 50", got)
		}
		return
	}
	t.Fatal("内嵌迁移里找不到版本 50")
}

// currentVersion 读指定库当前的迁移版本。
func currentVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	version, err := goose.GetDBVersionContext(context.Background(), db)
	if err != nil {
		t.Fatalf("读迁移版本失败: %v", err)
	}
	return version
}

// tableExists 判断一张表在不在。
func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(context.Background(),
		`SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists); err != nil {
		t.Fatalf("查表 %s 是否存在失败: %v", table, err)
	}
	return exists
}
