package snapshotwire

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 为什么这一段不能用假实现测
//
// 本包要保证的性质里有两条**只在真库上存在**：
//
//   - 分区 DDL 的边界与表定义是否真的匹配，写进去的行到底落在哪个分区；
//   - 目录查询是否真的只列出快照表下的分区。
//
// 假实现验证的是「我以为 PostgreSQL 会怎么做」。上面两条恰好是这份代码
// 里判错就不可逆的部分（删错分区），值得用真库钉一次。
//
// 没有 LARKAX_DB_DSN 时跳过，而不是失败：本机的开发环境未必起着库。
// CI 里那个环境变量一直在，并有专门的检查确认集成测试真的跑了。
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

	if pingErr := handle.PingContext(context.Background()); pingErr != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", pingErr)
	}

	// 表必须已经存在：迁移由 larkaxctl 或测试环境准备，本包不负责建表。
	var exists bool
	err = handle.QueryRowContext(context.Background(),
		`SELECT to_regclass('public.request_snapshots') IS NOT NULL`).Scan(&exists)
	if err != nil {
		t.Fatalf("检查快照表失败: %v", err)
	}
	if !exists {
		t.Skip("request_snapshots 不存在，先跑 larkaxctl migrate up")
	}
	return handle
}

// 写入的行必须真的落在当天分区里，而不是兜底分区。
//
// 这是整条留存承诺的落点：落进兜底分区的行没有到期日，清理永远删不掉它。
func TestIntegrationInsertLandsInDayPartition(t *testing.T) {
	handle := testDB(t)
	store := NewStore(Wrap(handle), quietLogger())
	ctx := context.Background()

	// 清掉今天的分区，让补建这一步真的被执行一次。
	day := PartitionName(time.Now().UTC())
	if _, err := handle.ExecContext(ctx, "DROP TABLE IF EXISTS "+day); err != nil {
		t.Fatalf("预清理分区失败: %v", err)
	}
	// 退出时也清掉，避免污染后续运行。
	t.Cleanup(func() {
		_, _ = handle.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+day)
	})

	requestID := "wire-it-" + time.Now().UTC().Format("20060102150405.000000000")
	snap := newSnap()
	snap.RequestID = requestID
	if err := store.Insert(ctx, snap); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	var landed string
	err := handle.QueryRowContext(ctx, `
		SELECT tableoid::regclass::text
		  FROM request_snapshots
		 WHERE request_id = $1`, requestID).Scan(&landed)
	if err != nil {
		t.Fatalf("查询落点失败: %v", err)
	}
	if landed != day {
		t.Errorf("落进了 %q，期望 %q；落进兜底分区意味着留存期对它不成立", landed, day)
	}

	// 兜底分区必须仍然为空——它是告警项，不该被正常写入填满。
	var fallback int
	if err := handle.QueryRowContext(ctx,
		`SELECT count(*) FROM request_snapshots_default`).Scan(&fallback); err != nil {
		t.Fatalf("查询兜底分区失败: %v", err)
	}
	if fallback != 0 {
		t.Errorf("兜底分区有 %d 行，说明有写入绕过了补建", fallback)
	}
}

// 清理必须真的能删掉一个已存在的分区，且只删到期的那些。
func TestIntegrationDropExpiredDropsRealPartitions(t *testing.T) {
	handle := testDB(t)
	store := NewStore(Wrap(handle), quietLogger())
	ctx := context.Background()

	// 造两个自己的分区：一个明显过期，一个在留存窗口内。
	// 名字带上固定前缀后面的日期，形状必须与 PartitionName 一致。
	expired := "request_snapshots_1999_01_01"
	fresh := "request_snapshots_" + time.Now().UTC().AddDate(0, 0, -1).Format("2006_01_02")
	for _, name := range []string{expired, fresh} {
		if _, err := handle.ExecContext(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
			t.Fatalf("预清理 %s 失败: %v", name, err)
		}
		from, to := partitionBounds(t, name)
		stmt := "CREATE TABLE " + name + " PARTITION OF request_snapshots " +
			"FOR VALUES FROM ('" + from + "') TO ('" + to + "')"
		if _, err := handle.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("建分区 %s 失败: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range []string{expired, fresh} {
			_, _ = handle.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+name)
		}
	})

	dropped, err := store.DropExpired(ctx, 30, time.Now().UTC())
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	// 过期那个必须被删掉。
	if _, err := handle.ExecContext(ctx, "SELECT 1 FROM "+expired); err == nil {
		t.Errorf("%s 仍在，清理没有真的删掉它", expired)
	}
	// 窗口内的那个必须还在。
	if _, err := handle.ExecContext(ctx, "SELECT 1 FROM "+fresh); err != nil {
		t.Errorf("%s 被误删了: %v", fresh, err)
	}
	// 兜底分区绝不能被删：删掉它，之后所有越界写入都会直接报错。
	if _, err := handle.ExecContext(ctx, "SELECT 1 FROM request_snapshots_default"); err != nil {
		t.Errorf("兜底分区被删了: %v", err)
	}

	found := false
	for _, name := range dropped {
		if name == expired {
			found = true
		}
	}
	if !found {
		t.Errorf("返回的清单 %v 里没有 %s；「删了哪些」必须交回调用方", dropped, expired)
	}
}

// 演练与执行在真库上也要给出同一份清单。
//
// 测试自己造一个到期分区，而不是「有就比、没有就跳过」：本机与 CI 库里
// 默认的种子分区都在留存窗口内，靠库里碰巧有到期分区来触发比对的话，
// 这条用例实际上永远不跑，而它的输出与通过长得一样（LESSONS.md 第 16 条）。
func TestIntegrationDryRunMatchesRealDrop(t *testing.T) {
	handle := testDB(t)
	store := NewStore(Wrap(handle), quietLogger())
	ctx := context.Background()

	expired := "request_snapshots_1999_03_03"
	if _, err := handle.ExecContext(ctx, "DROP TABLE IF EXISTS "+expired); err != nil {
		t.Fatalf("预清理失败: %v", err)
	}
	from, to := partitionBounds(t, expired)
	if _, err := handle.ExecContext(ctx,
		"CREATE TABLE "+expired+" PARTITION OF request_snapshots FOR VALUES FROM ('"+
			from+"') TO ('"+to+"')"); err != nil {
		t.Fatalf("造到期分区失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = handle.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+expired)
	})

	at := time.Now().UTC()
	listed, err := store.ExpiredPartitions(ctx, 30, at)
	if err != nil {
		t.Fatalf("演练失败: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("刚造出来的分区没被列为到期——判据不对")
	}
	dropped, err := store.DropExpired(ctx, 30, at)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if len(listed) != len(dropped) {
		t.Errorf("演练列出 %d 个、实际删了 %d 个：%v vs %v", len(listed), len(dropped), listed, dropped)
	}
	for i := range listed {
		if i < len(dropped) && listed[i] != dropped[i] {
			t.Errorf("第 %d 项不一致：演练 %q、实际 %q", i, listed[i], dropped[i])
		}
	}
}

// partitionBounds 由分区名反推 FOR VALUES 的上下界。
//
// 只用于测试里造分区，用的就是本包自己的解析与边界函数——这样测试造出来的
// 分区与生产路径产出的分区边界一致，两处不会各写一份格式而悄悄错开。
func partitionBounds(t *testing.T, name string) (string, string) {
	t.Helper()
	day, ok := PartitionDate(name)
	if !ok {
		t.Fatalf("测试用的分区名 %q 形状不对", name)
	}
	from, to := DayBounds(day)
	return from.Format(time.RFC3339), to.Format(time.RFC3339)
}
