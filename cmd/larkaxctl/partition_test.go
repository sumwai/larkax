package main

import (
	"context"
	"database/sql"
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
}

// request_snapshots_default 非空同样是非零退出。
//
// 它的注释写着「必须告警其非空：落进来的数据没有到期日，留存承诺对它不成立」。
// 两张表的处置不同（一张写入侧补建、一张靠预建任务），但「非空即告警」是同一条。
func TestPartitionCheckFailsWhenSnapshotDefaultPartitionHasRows(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()
	ensureCurrentMonthPartition(t)

	blocker := uniqueName("partition-check-snapshot")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO request_snapshots (request_id, user_id, direction, skeleton, created_at)
		VALUES ($1, 1, 'request', '{}'::jsonb, $2)`,
		blocker, time.Date(2030, 1, 15, 3, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("往快照兜底分区插一行失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM `+snapshotwire.DefaultPartition+` WHERE request_id = $1`, blocker)
	})
	if n := countTableRows(ctx, t, db, snapshotwire.DefaultPartition); n != 1 {
		t.Fatalf("%s 里有 %d 行，期望 1；这条用例的前提不成立",
			snapshotwire.DefaultPartition, n)
	}

	var err error
	out, _ := captureStreams(t, func() {
		err = runPartitionCheck([]string{"--months", "1"})
	})
	if err == nil {
		t.Fatal("快照兜底分区非空时应当非零退出")
	}
	if !strings.Contains(out, snapshotwire.DefaultPartition) {
		t.Errorf("输出里应当带上表名 %s，实际为 %q", snapshotwire.DefaultPartition, out)
	}
	if !strings.Contains(err.Error(), snapshotwire.DefaultPartition) {
		t.Errorf("错误信息里应当带上表名，实际为 %q", err)
	}
}

// 干净时 check 通过，退出码为 0。
//
// 没有这条用例，上面三条「非零退出」都可以靠「永远非零」满足。
func TestPartitionCheckPassesWhenClean(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()
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
// 用当月：这条用例要断言「建出来之后真的存在」，而挑一个远期的月份会把
// 分区留在库里，给别的用例添麻烦。当月的分区本来就会存在。
func TestPartitionEnsureReportsWhatItCreated(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	// 先保证它存在，再把它删掉——这样第一次 ensure 一定有东西可建。
	ensureCurrentMonthPartition(t)
	month := store.UsageEventPartitionName(time.Now().UTC())
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+month); err != nil {
		t.Fatalf("删掉当月分区失败: %v", err)
	}
	// 无论用例怎么结束，当月分区都要回到存在状态：它后面还有别的用例要写
	// 用量事件，而缺了它那些行会落进兜底分区。
	t.Cleanup(func() {
		if err := runCaptured(t, func() error {
			return runPartitionEnsure([]string{"--months", "1"})
		}); err != nil {
			t.Errorf("恢复当月分区失败: %v", err)
		}
	})

	var err error
	out, _ := captureStreams(t, func() {
		err = runPartitionEnsure([]string{"--months", "1"})
	})
	if err != nil {
		t.Fatalf("ensure 失败: %v", err)
	}
	if !strings.Contains(out, month) {
		t.Errorf("输出里应当列出真正建出来的分区 %s，实际为 %q", month, out)
	}

	// 第二次：没有缺失的分区，输出必须与「没跑成」分得开。
	out, _ = captureStreams(t, func() {
		err = runPartitionEnsure([]string{"--months", "1"})
	})
	if err != nil {
		t.Fatalf("第二次 ensure 失败: %v", err)
	}
	if !strings.Contains(out, "没有缺失的分区") {
		t.Errorf("第二次 ensure 应当明说没有缺失的分区，实际为 %q", out)
	}
}
