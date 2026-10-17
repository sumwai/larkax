package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sumwai/larkax/internal/config"
)

// 这一组用例钉的是同一件事的四个面：**运维命令的配置面里没有「服务角色」**。
//
//	1. 角色与主密钥都不设时，migrate 必须成功，且表真的建出来；
//	2. 与本次操作无关的变量（哪怕设成非法值）不许让命令失败；
//	3. 真需要主密钥的命令（rotate）必须继续要求它；
//	4. 服务那一侧照旧校验，一条都没放松（在 cmd/larkax 里）。
//
// 四条缺一条都不够：只留第 1 条，「顺手把校验全删掉」也能过；只留第 4 条，
// 则回到 issue #343 的现状。

// wantTables 是 migrate up 之后必须存在的几张表。
//
// 取的是**不同迁移建出来的**几张：初始化那一个建的表、后面的迁移建的
// 分区表与业务表都占一个，这样「迁移只跑了一半」也会被认出来。
var wantTables = []string{"goose_db_version", "users", "channels", "usage_events"}

// TestMigrateUpWithoutRoleOrMasterKey 钉住 issue #343 的现象：
//
//	$ larkaxctl migrate up                       → 失败，要主密钥
//	$ LARKAX_ROLE=worker larkaxctl migrate up    → 成功
//
// **显式设成 worker 反而比不设更能跑**，而建表与角色没有任何关系。
//
// 判据是可观察的结果，不是「某个内部函数被调了」：命令的退出码，以及表
// **真的建出来了**。
//
// # 为什么要一个全新的库
//
// 共用的那个库里表早就在了，在它上面跑，「建出了表」与「命令根本没跑」
// 分不开（见 LESSONS.md 第 16 条）。临时库的名字带唯一后缀、跑完就删，
// 所以别的进程碰不到它（第 26 条）。
func TestMigrateUpWithoutRoleOrMasterKey(t *testing.T) {
	dsn := createScratchDB(t)

	t.Setenv("LARKAX_DB_DSN", dsn)
	// 角色与主密钥都清掉，而不是设成 worker：设成 worker 恰好是绕开这个
	// 缺陷的那个动作，而用例要钉的正是「不设也能跑」。
	t.Setenv("LARKAX_ROLE", "")
	t.Setenv("LARKAX_MASTER_KEYS", "")

	// 先确认这个库确实是空的，否则下面「建出了表」那条断言是空的。
	if before := existingTables(t, dsn); len(before) != 0 {
		t.Fatalf("临时库应当是空的，却已经有这些表: %v", before)
	}

	err := runCaptured(t, func() error { return runMigrate([]string{"up"}) })
	if err != nil {
		t.Fatalf("不设 LARKAX_ROLE / LARKAX_MASTER_KEYS 时 migrate up 应当成功，"+
			"却报错（退出码 %d）: %v", exitCode(err), err)
	}
	if got := exitCode(err); got != exitOK {
		t.Errorf("退出码 = %d，期望 %d", got, exitOK)
	}

	if after := existingTables(t, dsn); len(after) != len(wantTables) {
		t.Errorf("migrate up 之后缺少这些表: %v", missing(wantTables, after))
	}
}

// 与本次操作无关的变量不许让命令失败——上一条是它在 migrate 上的一个实例，
// 这一条是通则。
//
// 三个变量都设成**非法值**，而不是空值：设成空值只是没走到那条校验分支，
// 而这里要证明的是「命令根本没读它们」。角色用一个不存在的取值同理——
// 运维命令不属于任何角色，那个值对不对与它无关。
func TestOpsCommandsIgnoreUnrelatedVariables(t *testing.T) {
	testDB(t) // 连上库、跑好迁移

	t.Setenv("LARKAX_ROLE", "router")
	t.Setenv("LARKAX_MASTER_KEYS", "not base64!!")
	t.Setenv("LARKAX_REDIS_DSN", "localhost:6379")

	err := runCaptured(t, func() error { return runMigrate([]string{"version"}) })
	if err != nil {
		t.Fatalf("这三个变量与 migrate 无关，命令不该失败（退出码 %d）: %v", exitCode(err), err)
	}
}

// rotate 是**真的**需要主密钥的命令，所以它必须继续要求——这一轮放宽的是
// 「角色决定要不要主密钥」，不是「rotate 不需要主密钥」。
//
// 两个方向都要钉，因为修这条 issue 最容易的做法是把校验整体删掉：
//
//	主密钥没设 → 仍然报错（否则轮换连「当前主密钥是哪一把」都答不出来）
//	主密钥设了 → 角色不该再拦住它（那正是这条 issue 的另一半）
func TestRotateRequiresMasterKeyRegardlessOfRole(t *testing.T) {
	testDB(t)

	t.Setenv("LARKAX_MASTER_KEYS", "")
	t.Setenv("LARKAX_ROLE", string(config.RoleWorker))

	err := runCaptured(t, func() error { return runRotate(nil) })
	if err == nil {
		t.Fatal("没设主密钥时 rotate 应当报错")
	}
	if !strings.Contains(err.Error(), "LARKAX_MASTER_KEYS") {
		t.Errorf("错误信息应当点明缺的是主密钥: %v", err)
	}

	// 干跑不写库，所以这里拿 worker 角色再跑一次是安全的。
	t.Setenv("LARKAX_MASTER_KEYS", "test:"+testMasterKey())
	if err := runCaptured(t, func() error { return runRotate(nil) }); err != nil {
		t.Fatalf("主密钥设好之后，rotate 不该因为角色而失败: %v", err)
	}
}

// createScratchDB 建一个只属于本次用例的空库，返回连它的 DSN。
//
// 维护库（postgres）由 LARKAX_DB_DSN 推出来：换主机、换端口、换账号时
// 跟着走，不必再配一个变量。
func createScratchDB(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("LARKAX_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Fatalf("LARKAX_DB_DSN 要是 URL 形式才推得出维护库的连接串: %q", dsn)
	}

	adminDSN := *u
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("pgx", adminDSN.String())
	if err != nil {
		t.Fatalf("打开维护库失败: %v", err)
	}
	if err := admin.PingContext(context.Background()); err != nil {
		_ = admin.Close()
		t.Skipf("连不上维护库，跳过集成测试: %v", err)
	}

	name := fmt.Sprintf("larkax_scratch_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		_ = admin.Close()
		t.Fatalf("建临时库 %s 失败（这条用例需要 CREATEDB 权限）: %v", name, err)
	}
	t.Cleanup(func() {
		// WITH (FORCE) 断开还挂在上面的连接：删库时命令那一侧可能还有
		// 连接没回收，而 DROP DATABASE 会因为「还有连接」失败——那会留下
		// 一个谁也不知道该不该删的库。
		if _, err := admin.ExecContext(context.Background(),
			"DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("删除临时库 %s 失败: %v", name, err)
		}
		_ = admin.Close()
	})

	scratch := *u
	scratch.Path = "/" + name
	return scratch.String()
}

// existingTables 返回 wantTables 里在库中确实存在的那几张。
//
// 用 to_regclass 而不是查 information_schema：它对不存在的对象返回 NULL
// 而不报错，正是「不报错地告诉你没有」这种判据最容易读错的地方，所以这里
// 把它转成一个显式的清单。
func existingTables(t *testing.T, dsn string) []string {
	t.Helper()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer func() { _ = db.Close() }()

	var found []string
	for _, table := range wantTables {
		var reg *string
		if err := db.QueryRowContext(context.Background(),
			"SELECT to_regclass($1)::text", "public."+table).Scan(&reg); err != nil {
			t.Fatalf("查询表 %s 是否存在失败: %v", table, err)
		}
		if reg != nil {
			found = append(found, table)
		}
	}
	return found
}

// missing 返回 want 里不在 got 里的那些。断言失败时要打的是「少了哪几张」，
// 而不是两个清单让人自己对——差异正是排查的起点。
func missing(want, got []string) []string {
	have := make(map[string]bool, len(got))
	for _, t := range got {
		have[t] = true
	}
	var out []string
	for _, t := range want {
		if !have[t] {
			out = append(out, t)
		}
	}
	return out
}
