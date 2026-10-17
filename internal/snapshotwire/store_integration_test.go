package snapshotwire

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/testdb"
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
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// ownDay 是这条用例专用的那一天。
//
// 取一个**未来**的日期，而不是「真正的当天」：未来那天一定没有分区（没人预建
// 到那么远），删掉它也不影响任何别的进程。判据是问一句「这个窗口里别的进程
// 会不会往它里面写」——不会。
//
// 按当天写用例就等于删当天分区，而分区是全库共享的状态：`go test ./...` 把包
// 并行跑在同一个库上，别的包刚写的行会跟着被删掉，而在那段窗口里写进来的行会
// 落进兜底分区（见 LESSONS.md 第 26 条）。取正午而不是零点，是为了让「属于哪
// 一天」不受边界口径影响。
var ownDay = time.Date(2037, 6, 15, 12, 0, 0, 0, time.UTC)

// currentSnapshotPartition 是「真正的当天分区」的名字。
func currentSnapshotPartition() string {
	return PartitionName(time.Now().UTC())
}

// dropRefusal 是「这个分区名能不能删」的判据，不合法时给出理由。
//
// # 绝不许删当天分区
//
// 删掉它之后、它被建回来之前的那一段窗口里：别的包刚写的快照会跟着被删掉
// （它们按 request_id 查不到自己那一条，报出来的是它们的失败），而并发的写入
// 会落进 request_snapshots_default——那里的行没有到期日，留存承诺对它不成立；
// 兜底分区一有当天范围内的行，当天分区就再也建不回来（SQLSTATE 23514）。
//
// 判据与动作分开：`t.Fatal` 本身没法在不弄挂用例的情况下断言，而「什么名字该
// 被拒」可以——见 TestDropRefusalRejectsOnlyCurrentDay。
func dropRefusal(name string) error {
	if name == currentSnapshotPartition() {
		return fmt.Errorf("不许删当天分区 %s：删掉它的窗口里别的包刚写的快照会跟着"+
			"被删掉，而并发的写入会落进 %s——那里的行没有到期日，留存承诺对它不成立；"+
			"改用一个别人碰不到的日期",
			name, DefaultPartition)
	}
	return nil
}

// dropRequestSnapshotPartition 删掉一个测试造出来的分区。
func dropRequestSnapshotPartition(ctx context.Context, t *testing.T, db *sql.DB, name string) {
	t.Helper()
	// 判据在 DDL 之前：拦住之后当天分区必须原封不动。
	if err := dropRefusal(name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("清理分区 %s 失败: %v", name, err)
	}
}

// 护栏本身要有判据：传当天分区进去必须被拒。
//
// 不连库：判的是那个纯函数，所以没设 LARKAX_DB_DSN 时这条也会跑。
func TestDropRefusalRejectsOnlyCurrentDay(t *testing.T) {
	if err := dropRefusal(currentSnapshotPartition()); err == nil {
		t.Error("护栏没拦住「删当天分区」——那道判断没在工作")
	}

	allowed := []struct {
		name string
		at   time.Time
	}{
		{"合成的未来日期", ownDay},
		{"昨天", time.Now().UTC().AddDate(0, 0, -1)},
	}
	for _, tc := range allowed {
		if err := dropRefusal(PartitionName(tc.at)); err != nil {
			t.Errorf("%s（%s）应当放行，实际被拒: %v",
				tc.name, PartitionName(tc.at), err)
		}
	}
}

// 写入的行必须真的落在它自己那一天的分区里，而不是兜底分区。
//
// 这是整条留存承诺的落点：落进兜底分区的行没有到期日，清理永远删不掉它。
//
// 用例把写入指向 ownDay（一个合成的未来日期），并删掉那一天的分区，让补建
// 这一步真的被执行一次。删掉的那张分区只属于本用例，所以它不依赖「别人此刻
// 在不在动当天的分区」这个前提——原来按当天写，断言的东西与它真正依赖的东西
// 是两回事（见 LESSONS.md 第 26 条）。
func TestIntegrationInsertLandsInDayPartition(t *testing.T) {
	handle := testDB(t)
	store := NewStore(Wrap(handle), quietLogger())
	store.now = func() time.Time { return ownDay }
	ctx := context.Background()

	// 清掉这一天自己的分区，让补建这一步真的被执行一次。
	day := PartitionName(ownDay)
	dropRequestSnapshotPartition(ctx, t, handle, day)
	// 退出时也清掉：这个分区只属于本用例。
	t.Cleanup(func() {
		dropRequestSnapshotPartition(context.Background(), t, handle, day)
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

	// 兜底分区里不许有这一行。
	//
	// 判据按 request_id 收窄到本用例写进去的那一行，不是整张兜底分区的行数：
	// 兜底分区是全库共享的，别的进程在同一个库里跑，全局计数会把它们的行算到
	// 这条用例头上。收窄之后它钉住的是「补建真的生效了，这一行没有绕过它掉进
	// 兜底分区」——也就是 `landed` 那句话在告警自己的口径上的复述（兜底分区
	// 非空即告警），留着它，失败信息才会指向告警而不是一个内部分区名。
	var fallback int
	if err := handle.QueryRowContext(ctx,
		`SELECT count(*) FROM `+DefaultPartition+` WHERE request_id = $1`,
		requestID).Scan(&fallback); err != nil {
		t.Fatalf("查询兜底分区失败: %v", err)
	}
	if fallback != 0 {
		t.Errorf("兜底分区里有 %d 行属于这次写入，说明它绕过了补建", fallback)
	}
}

// 清理必须真的能删掉一个已存在的分区，且只删到期的那些。
func TestIntegrationDropExpiredDropsRealPartitions(t *testing.T) {
	handle := testDB(t)
	store := NewStore(Wrap(handle), quietLogger())
	ctx := context.Background()

	// 造两个自己的分区：一个明显过期，一个在留存窗口内。名字一律由
	// PartitionName 产出，形状就不必再对一遍。
	//
	// 「窗口内」那个取 15 天前，而不是「昨天」：昨天那张分区是迁移铺下的**种子
	// 分区**，它不属于本用例——用例删掉它、`t.Cleanup` 再删一次，它在这一轮里
	// 就再也回不来了（见 LESSONS.md 第 26 条）。15 天前稳稳落在 30 天的窗口内，
	// 又是别人碰不到的。
	expired := "request_snapshots_1999_01_01"
	fresh := PartitionName(time.Now().UTC().AddDate(0, 0, -15))
	for _, name := range []string{expired, fresh} {
		dropRequestSnapshotPartition(ctx, t, handle, name)
		from, to := partitionBounds(t, name)
		stmt := "CREATE TABLE " + name + " PARTITION OF request_snapshots " +
			"FOR VALUES FROM ('" + from + "') TO ('" + to + "')"
		if _, err := handle.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("建分区 %s 失败: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range []string{expired, fresh} {
			dropRequestSnapshotPartition(context.Background(), t, handle, name)
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

// 采集关掉时清理必须真的跑：写新快照与清存量是两件事，而它们曾经共用一个谓词。
//
// 走 Collector 而不是 Store：这条判据挂在 Collector 上，真库要证明的是
// 「关掉采集之后，那张分区真的没了」。假实现能证明调用到达了 Store，
// 证明不了 DDL 在真库上发生了。
func TestIntegrationCleanupRunsWhenCollectingDisabled(t *testing.T) {
	handle := testDB(t)
	ctx := context.Background()

	expired := "request_snapshots_1999_05_05"
	dropRequestSnapshotPartition(ctx, t, handle, expired)
	from, to := partitionBounds(t, expired)
	if _, err := handle.ExecContext(ctx,
		"CREATE TABLE "+expired+" PARTITION OF request_snapshots FOR VALUES FROM ('"+
			from+"') TO ('"+to+"')"); err != nil {
		t.Fatalf("造到期分区失败: %v", err)
	}
	t.Cleanup(func() {
		dropRequestSnapshotPartition(context.Background(), t, handle, expired)
	})

	c := newCollector(t, Config{Enabled: false, RetentionDays: 30}, Wrap(handle))
	if c.Collecting() {
		t.Fatal("这条用例的前提是采集关着")
	}

	dropped, err := c.DropExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	found := false
	for _, name := range dropped {
		if name == expired {
			found = true
		}
	}
	if !found {
		t.Errorf("返回的清单 %v 里没有 %s", dropped, expired)
	}
	if _, err := handle.ExecContext(ctx, "SELECT 1 FROM "+expired); err == nil {
		t.Errorf("%s 仍在：关掉采集把清理一起废掉了", expired)
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
	dropRequestSnapshotPartition(ctx, t, handle, expired)
	from, to := partitionBounds(t, expired)
	if _, err := handle.ExecContext(ctx,
		"CREATE TABLE "+expired+" PARTITION OF request_snapshots FOR VALUES FROM ('"+
			from+"') TO ('"+to+"')"); err != nil {
		t.Fatalf("造到期分区失败: %v", err)
	}
	t.Cleanup(func() {
		dropRequestSnapshotPartition(context.Background(), t, handle, expired)
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
