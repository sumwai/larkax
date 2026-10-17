package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/snapshotwire"
)

// ── 参数与输出：不碰库 ──────────────────────────────────────────────────

func TestParsePartitionMonthsDefaultsToThree(t *testing.T) {
	var months int
	var err error
	_, _ = captureStreams(t, func() { months, err = parsePartitionMonths(nil, "partition check") })
	if err != nil {
		t.Fatalf("不给 --months 时应当用默认值: %v", err)
	}
	if months != partitionMonthsDefault {
		t.Errorf("默认 months = %d，期望 %d", months, partitionMonthsDefault)
	}
	// 默认值是对外契约的一部分：调度器按它算提前量，改它要连文档一起改。
	if partitionMonthsDefault != 3 {
		t.Errorf("默认值变成了 %d，文档 docs/partitions.md 里写的是 3", partitionMonthsDefault)
	}
}

func TestParsePartitionMonthsAcceptsPositive(t *testing.T) {
	var months int
	var err error
	_, _ = captureStreams(t, func() {
		months, err = parsePartitionMonths([]string{"--months", "6"}, "partition check")
	})
	if err != nil {
		t.Fatalf("--months 6 应当被接受: %v", err)
	}
	if months != 6 {
		t.Errorf("months = %d，期望 6", months)
	}
}

// 参数错必须是 exitUsage（2），不是通用的 1。
//
// 两者的处置不同：一个去改命令，一个去看库。混成一个码，调度器只能去匹配
// 错误文本，而错误文本是给人看的。
func TestParsePartitionMonthsRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"零个月", []string{"--months", "0"}},
		{"负数", []string{"--months", "-2"}},
		{"位置参数", []string{"extra"}},
		{"认不出的开关", []string{"--bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			_, _ = captureStreams(t, func() {
				_, err = parsePartitionMonths(tc.args, "partition check")
			})
			if err == nil {
				t.Fatalf("%v 应当报错", tc.args)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", got, exitUsage)
			}
		})
	}
}

// 一个都没建时也要有明确输出。
//
// 「没有缺失的分区」与「命令根本没跑成」在安静的终端里长得一样
// （LESSONS.md 第 16 条），所以断言的是**有一行说明**，不只是「没报错」。
func TestPrintCreatedPartitionsEmptyIsExplicit(t *testing.T) {
	out := captureStdout(t, func() { printCreatedPartitions(nil) })
	if strings.TrimSpace(out) == "" {
		t.Fatal("一个都没建时不该什么都不输出")
	}
	if !strings.Contains(out, "没有缺失的分区") {
		t.Errorf("空清单的输出应当说明是没有缺失的分区，实际为 %q", out)
	}
}

func TestPrintCreatedPartitionsListsEveryName(t *testing.T) {
	names := []string{"usage_events_2026_11", "usage_events_2026_12"}
	out := captureStdout(t, func() { printCreatedPartitions(names) })
	for _, n := range names {
		if !strings.Contains(out, n) {
			t.Errorf("输出里应当列出 %s，实际为 %q", n, out)
		}
	}
	if !strings.Contains(out, "2") {
		t.Errorf("输出里应当给出条数，实际为 %q", out)
	}
}

// 体检通过时也要说清「分区都在」，而不是只打两行兜底分区的行数。
//
// 缺分区那一路会列出缺了哪些；「一个都不缺」必须同样有正向证据，
// 否则「都查过了、都齐」与「根本没查」在输出上分不开。
func TestPrintMissingPartitionsEmptyIsExplicit(t *testing.T) {
	out := captureStdout(t, func() { printMissingPartitions(nil) })
	if !strings.Contains(out, "都在") {
		t.Errorf("没有缺失时的输出应当明说分区都在，实际为 %q", out)
	}
}

func TestPrintMissingPartitionsListsEveryName(t *testing.T) {
	names := []string{"usage_events_2026_12"}
	out := captureStdout(t, func() { printMissingPartitions(names) })
	if !strings.Contains(out, "usage_events_2026_12") {
		t.Errorf("输出里应当列出缺的分区名，实际为 %q", out)
	}
}

// ── 真库 ────────────────────────────────────────────────────────────────

// insertUsageEventIntoDefault 往 usage_events 的兜底分区里塞一行用量事件。
//
// 走父表而不是直接写 usage_events_default：PostgreSQL 按 created_at 把它路由
// 到兜底分区，所以传一个没有分区的月份，落点就是兜底分区。
func insertUsageEventIntoDefault(ctx context.Context, t *testing.T, db *sql.DB, requestID string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_events (
			request_id, user_id, canonical_model, billing_path, cost, asset, priced_at, created_at
		) VALUES ($1, 1, 'gpt-4o', 'measured', 0, 'CNY', $2, $2)`,
		requestID, at); err != nil {
		t.Fatalf("往兜底分区插一行失败: %v", err)
	}
}

// countTableRows 数一张表的行数。
func countTableRows(ctx context.Context, t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("数 %s 的行失败: %v", table, err)
	}
	return n
}

// relationExists 用 to_regclass 判断一个关系在不在。
func relationExists(ctx context.Context, t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, name).Scan(&got); err != nil {
		t.Fatalf("查关系 %s 是否存在失败: %v", name, err)
	}
	return got.Valid && got.String != ""
}

// currentUsageEventPartition 是「真正的当月分区」的名字。
func currentUsageEventPartition() string {
	return store.UsageEventPartitionName(time.Now().UTC())
}

// dropRefusal 是「这个分区名能不能删」的判据，不合法时给出理由。
//
// # 绝不许删当月分区
//
// 删掉它之后，到它被建回来之前的那一段窗口里，**任何并发的写入都会落进
// usage_events_default**——而 `go test ./...` 是把包并行跑起来的，那些写入
// 来自别的包。兜底分区一旦有行，当月分区就再也建不回来（实测 SQLSTATE
// 23514），于是这一次 run 里**后面所有**碰分区的用例都会挂。
//
// 这道坑真的踩过：这条用例原本删的就是当月分区，门禁挂在这里，而报出来的
// 却是 `internal/ledger/store` 的用例失败——现场离原因很远。
//
// 判据与动作分开：`t.Fatal` 本身没法在不弄挂用例的情况下断言，而「什么名字
// 该被拒」可以——见 TestDropRefusalRejectsOnlyCurrentMonth。
func dropRefusal(name string) error {
	if name == currentUsageEventPartition() {
		return fmt.Errorf("不许删当月分区 %s：删掉它的窗口里并发的写入会落进 %s，"+
			"而兜底分区一有行当月分区就再也建不回来（SQLSTATE 23514）——"+
			"那会毁掉这次 run 里后面所有碰分区的用例；改用一个未来的月份",
			name, store.UsageEventDefaultPartition)
	}
	return nil
}

// dropUsageEventPartition 删掉一个测试造出来的分区。
func dropUsageEventPartition(ctx context.Context, t *testing.T, db *sql.DB, name string) {
	t.Helper()
	// 判据在 DDL 之前：拦住之后那个分区必须原封不动。
	if err := dropRefusal(name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("清理分区 %s 失败: %v", name, err)
	}
}

// partitionStateLockKey 是「正在动 usage_events 分区全局状态」的跨进程互斥键。
//
// # 为什么需要它
//
// `go test ./...` 把包并行跑起来，而分区的状态是**全库共享**的。一个包往兜底
// 分区里塞行（为了造出「建不出分区」），另一个包正好在断言兜底分区为空——
// 两边各自都对，撞在一起就是一次假的失败。
//
// 键值是任取的常量，只要 `internal/ledger/store` 与 `cmd/larkaxctl` 两处用
// **同一个**。改了这里要连那边一起改，否则锁形同虚设。
const partitionStateLockKey = 247001

// lockPartitionState 拿一把跨进程的排它锁，直到用例结束。
//
// 用 session 级的 advisory lock 绑在一条独占连接上：连接一关锁就自动释放，
// 所以用例中途挂掉也不会把锁留在库里，下一个用例照样能跑。
func lockPartitionState(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("取一条独占连接失败: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", partitionStateLockKey); err != nil {
		_ = conn.Close()
		t.Fatalf("拿分区状态锁失败: %v", err)
	}
	// 用 WithoutCancel 而不是 Background：清理由用例的 ctx 派生（保留它的值），
	// 但用例结束时那个 ctx 可能已经被取消，而解锁必须发得出去。
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			"SELECT pg_advisory_unlock($1)", partitionStateLockKey)
		_ = conn.Close()
	})
}

// 护栏本身要被钉住：只有当月分区该被拒。
//
// 没有这条用例，`dropRefusal` 里那道判断只是「写在 helper 里」，不是「被证明
// 在工作」（LESSONS.md 第 23 条）。反向的一半同样要紧——判据写宽了（例如把
// 所有分区都拒掉），真要删分区的用例会一起挂，而现场看起来像别的问题。
func TestDropRefusalRejectsOnlyCurrentMonth(t *testing.T) {
	if err := dropRefusal(currentUsageEventPartition()); err == nil {
		t.Error("护栏没拦住「删当月分区」——那道判断没在工作")
	}

	future := store.UsageEventPartitionName(time.Now().UTC().AddDate(0, 2, 0))
	if err := dropRefusal(future); err != nil {
		t.Errorf("未来月份 %s 应当放行，实际被拒: %v", future, err)
	}
}

// ensureCurrentMonthPartition 先把当月分区补上。
//
// 几条 check 用例都需要「缺分区」这一项不成立，才能保证它们失败的原因是
// 自己造的那一个。不先补上的话，这些用例会在「当月本来就没有分区」的月份里
// 因为另一个原因失败，而两者的退出码一样（LESSONS.md 第 23 条）。
func ensureCurrentMonthPartition(t *testing.T) {
	t.Helper()
	if err := runCaptured(t, func() error {
		return runPartitionEnsure([]string{"--months", "1"})
	}); err != nil {
		t.Fatalf("先补上当月分区失败: %v", err)
	}
}

// 缺月分区时非零退出。
//
// --months 给一个没人会真的铺满的数：这条命令是只读的，用「要求的区间里
// 必然有缺的」来造条件，就不必去删任何分区。删分区是有副作用的，而这条
// 用例要测的只是退出码。
func TestPartitionCheckFailsOnMissingMonth(t *testing.T) {
	testDB(t)
	setupEnv(t)

	var err error
	out, _ := captureStreams(t, func() {
		err = runPartitionCheck([]string{"--months", "120"})
	})
	if err == nil {
		t.Fatal("缺月分区时应当非零退出")
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", got, exitFailure)
	}
	if !strings.Contains(out, "usage_events_") {
		t.Errorf("输出里应当列出缺的分区名，实际为 %q", out)
	}
}

// 兜底分区非空时非零退出，且输出里要带上表名与行数。
//
// 「有问题」三个字不够：看的人下一步要判断的是「哪张表、有多少行」。
func TestPartitionCheckFailsWhenUsageDefaultPartitionHasRows(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()
	lockPartitionState(ctx, t, db)
	ensureCurrentMonthPartition(t)

	// 挑一个没有分区的月份，PostgreSQL 才会把它路由到兜底分区。
	blocker := uniqueName("partition-check")
	insertUsageEventIntoDefault(ctx, t, db, blocker, time.Date(2030, 1, 15, 0, 0, 0, 0, time.UTC))
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM `+store.UsageEventDefaultPartition+` WHERE request_id = $1`, blocker)
	})
	// 前提：那一行确实落进了兜底分区，否则这条用例失败的原因是别的。
	if n := countTableRows(ctx, t, db, store.UsageEventDefaultPartition); n != 1 {
		t.Fatalf("%s 里有 %d 行，期望 1；这条用例的前提不成立",
			store.UsageEventDefaultPartition, n)
	}

	var err error
	out, _ := captureStreams(t, func() {
		err = runPartitionCheck([]string{"--months", "1"})
	})
	if err == nil {
		t.Fatal("兜底分区非空时应当非零退出")
	}
	if !strings.Contains(out, store.UsageEventDefaultPartition) {
		t.Errorf("输出里应当带上表名 %s，实际为 %q", store.UsageEventDefaultPartition, out)
	}
	if !strings.Contains(out, "1 行") {
		t.Errorf("输出里应当带上行数，实际为 %q", out)
	}
	if !strings.Contains(err.Error(), store.UsageEventDefaultPartition) {
		t.Errorf("错误信息里应当带上表名，实际为 %q", err)
	}

	// 两张兜底分区互不串味：这一条只让 usage 那张有行，快照那张必须仍读作
	// 0 行。判据那一层由 TestPartitionProblemsFlagsEachCondition 钉住，这一
	// 句钉的是**取值**——两处把表名接错的话，会在这里现形（一个数被用了两遍
	// 时，快照那一行会跟着变成 1）。
	if !strings.Contains(out, snapshotwire.DefaultPartition+"：0 行") {
		t.Errorf("快照兜底分区应当仍读作 0 行，实际输出为 %q", out)
	}
}

// 三项检查各自都能单独把体检判成不通过。
//
// # 为什么这一条不连库
//
// `request_snapshots_default` 非空这一项**没法**用「往库里真写一行」来造：
// 那张表的行数是全库共享的，而 `internal/snapshotwire` 的集成测试断言它是
// 空的（`TestIntegrationInsertLandsInDayPartition`：兜底分区有行就说明有写入
// 绕过了补建）。`go test ./...` 把包并行跑起来，于是「我塞一行」与「它数一遍」
// 会撞在一起——两边各自都对，撞在一起就是一次假的失败。
//
// 这是同一个坑的另一种形状：上一条是**删**共享状态踩到别人，这一条是**写**
// 共享状态踩到别人。所以判据从取值里拆了出来（见 partitionProblems），这一层
// 不必连库就能把三种条件各测一遍。
//
// 代价是「countRows 取的是哪张表」不由这条用例钉住。它由另外两条钉：
// TestPartitionCheckPassesWhenClean 断言输出里两张表的名字与行数都在，
// TestPartitionCheckFailsWhenUsageDefaultPartitionHasRows 断言 usage 那张
// 非空时快照那张仍读作 0 行——两处接错表都会在那两条上现形。
func TestPartitionProblemsFlagsEachCondition(t *testing.T) {
	missing := []string{"usage_events_2026_12"}

	cases := []struct {
		name         string
		missing      []string
		usageRows    int64
		snapshotRows int64
		wantProblem  bool
		wantTable    string // 出问题时该在清单里出现的表名；空表示只要求「没问题」
	}{
		{"都正常", nil, 0, 0, false, ""},
		{"缺月分区", missing, 0, 0, true, ""},
		{"usage 兜底分区非空", nil, 3, 0, true, store.UsageEventDefaultPartition},
		{"快照兜底分区非空", nil, 0, 7, true, snapshotwire.DefaultPartition},
		{"三项都不满足", missing, 1, 2, true, store.UsageEventDefaultPartition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := partitionProblems(tc.missing, tc.usageRows, tc.snapshotRows)
			if tc.wantProblem != (len(got) > 0) {
				t.Fatalf("问题清单为 %v，期望「有问题=%v」", got, tc.wantProblem)
			}
			if tc.wantTable != "" {
				joined := strings.Join(got, "；")
				if !strings.Contains(joined, tc.wantTable) {
					t.Errorf("清单里应当带上表名 %s，实际为 %q", tc.wantTable, joined)
				}
			}
			// 「三项都不满足」要三项都在，不能只报第一项。
			if tc.name == "三项都不满足" && len(got) != 3 {
				t.Errorf("三项都不满足时应当报 3 条，实际 %d 条：%v", len(got), got)
			}
		})
	}
}

// 干净时 check 通过，退出码为 0。
//
// 没有这条用例，上面三条「非零退出」都可以靠「永远非零」满足。
func TestPartitionCheckPassesWhenClean(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()
	// 这条用例断言的是「两个兜底分区都是空的」这个**全局**状态，而并发的包
	// 会往兜底分区里塞行造它们自己的现场。锁把「动分区状态」与「读分区状态」
	// 分开，否则它会偶发地因为别人的行而失败。
	lockPartitionState(ctx, t, db)
	ensureCurrentMonthPartition(t)

	// 前提：两个兜底分区都是空的。不空的话这条用例会因为别的用例留下的数据
	// 而失败，而那时它看起来像「check 坏了」——所以先说清楚前提不成立。
	for _, table := range []string{store.UsageEventDefaultPartition, snapshotwire.DefaultPartition} {
		if n := countTableRows(ctx, t, db, table); n != 0 {
			t.Fatalf("%s 有 %d 行（别的用例留下的），这条用例的前提不成立", table, n)
		}
	}

	var err error
	out, _ := captureStreams(t, func() {
		err = runPartitionCheck([]string{"--months", "1"})
	})
	if err != nil {
		t.Fatalf("干净时 check 不该失败: %v\n输出：%s", err, out)
	}
	if !strings.Contains(out, store.UsageEventDefaultPartition) ||
		!strings.Contains(out, snapshotwire.DefaultPartition) {
		t.Errorf("通过时也要把两张兜底分区的行数打出来，实际为 %q", out)
	}
}

// ensure 打印它真正建出来的分区名，第二次跑则明说没建任何东西。
//
// # 删的是未来月份，不是当月
//
// 这条用例要先造出一个「缺的分区」才能验预建。**那个分区不能是当月**：
// 删掉当月的窗口里，并发的写入（`go test ./...` 把包并行跑起来）会落进
// `usage_events_default`，而兜底分区一有行，当月分区就再也建不回来
// （实测 SQLSTATE 23514）——那会毁掉这一次 run 里后面所有碰分区的用例，
// 包括别的包的。这道坑踩过一次，`dropUsageEventPartition` 里留了护栏。
//
// 用「当月 +2」：--months 3 会把它建出来，而它离得够远，没有任何并发写入
// 会落到那里，删掉它也不影响当月。
func TestPartitionEnsureReportsWhatItCreated(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()
	lockPartitionState(ctx, t, db)

	// 先把「当月 +2」建出来——它可能本来就不存在（种子只铺了固定的两个月）。
	if err := runCaptured(t, func() error {
		return runPartitionEnsure([]string{"--months", "3"})
	}); err != nil {
		t.Fatalf("先把三个月铺出来失败: %v", err)
	}
	month := store.UsageEventPartitionName(time.Now().UTC().AddDate(0, 2, 0))
	dropUsageEventPartition(ctx, t, db, month)

	var err error
	out, _ := captureStreams(t, func() {
		err = runPartitionEnsure([]string{"--months", "3"})
	})
	if err != nil {
		t.Fatalf("ensure 失败: %v", err)
	}
	if !strings.Contains(out, month) {
		t.Errorf("输出里应当列出真正建出来的分区 %s，实际为 %q", month, out)
	}
	if !relationExists(ctx, t, db, month) {
		t.Errorf("%s 在 ensure 之后仍然不存在", month)
	}

	// 第二次：没有缺失的分区，输出必须与「没跑成」分得开。
	out, _ = captureStreams(t, func() {
		err = runPartitionEnsure([]string{"--months", "3"})
	})
	if err != nil {
		t.Fatalf("第二次 ensure 失败: %v", err)
	}
	if !strings.Contains(out, "没有缺失的分区") {
		t.Errorf("第二次 ensure 应当明说没有缺失的分区，实际为 %q", out)
	}
}
