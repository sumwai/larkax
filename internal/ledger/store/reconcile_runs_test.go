package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// 本文件是 reconcile_runs 只读一侧的用例。
//
// 写入方在 reconcile.go（每一轮开一行、收尾时把结果填进同一行），它的用例在
// reconcile_test.go。这里问的是另一半：`ReconcileRuns` 把那一行读回来时，
// 十三列里**每一列**都对得上吗、新的在前吗、被 limit 截住时说得清楚吗。
//
// # 为什么逐列比对，而不是只比几个要紧的
//
// 这几列的名字与类型挨得很近（四个计数列、两个时刻列），而读侧的列名与 Scan 的
// 顺序各写一遍。把两列弄反时，命令行那一档只看得出被断言过的那一两列——其余的
// 会安静地错着。所以这里把**整行**与库里的那一行并起来比：造一行值各不相同的
// 记录，弄反一列就会红。

// runRow 是 reconcile_runs 里读出来的一整行，字段与库里的列一一对应。
type runRow struct {
	StartedAt         time.Time
	FinishedAt        sql.NullTime
	Watermark         time.Time
	FullScan          bool
	CheckedWallets    int
	BalanceMismatches int
	FrozenMismatches  int
	Repaired          int
	MalformedAccounts int
	StaleReservations int
	Status            string
	Error             sql.NullString
}

// readRunRow 从库里读回某一行，供与读侧交出来的那一行对比。
//
// 走裸 SQL 而不是走 ReconcileRuns：那一个正是被测对象，用它取期望值等于拿被测
// 对象验它自己。
func readRunRow(t *testing.T, db *sql.DB, id int64) runRow {
	t.Helper()

	var row runRow
	if err := db.QueryRowContext(context.Background(), `
		SELECT started_at, finished_at, watermark, full_scan, checked_wallets,
		       balance_mismatches, frozen_mismatches, repaired, malformed_accounts,
		       stale_reservations, status, error
		  FROM reconcile_runs WHERE id = $1`, id).Scan(
		&row.StartedAt, &row.FinishedAt, &row.Watermark, &row.FullScan,
		&row.CheckedWallets, &row.BalanceMismatches, &row.FrozenMismatches,
		&row.Repaired, &row.MalformedAccounts, &row.StaleReservations,
		&row.Status, &row.Error); err != nil {
		t.Fatalf("读运行记录 %d 失败: %v", id, err)
	}
	return row
}

// reconcileInterval 是「距上一次全量超过多久就再来一次全量」。
//
// 给一个远大于用例运行时间的值：上一轮刚全量过，于是下一轮是增量。那个分界在
// reconcile.go 的 reconcileStart 里。
const reconcileInterval = 168 * time.Hour

// TestReconcileRunsReadsEveryColumn 跑真两轮（一次全量、一次增量），
// 再断言读回来的每一列都与库里那一行相同。
func TestReconcileRunsReadsEveryColumn(t *testing.T) {
	db := testDB(t)
	// 有一个钱包才有「比对了多少行」可言；这一列不这么造就是恒 0。
	seed(t, db, "100")

	full := reconcileWithInterval(t, db, 0)
	incremental := reconcileWithInterval(t, db, reconcileInterval)

	page, err := New(db).ReconcileRuns(context.Background(), 0)
	if err != nil {
		t.Fatalf("读对账运行记录失败: %v", err)
	}

	seen := make(map[int64]bool, len(page.Runs))
	for _, run := range page.Runs {
		seen[run.ID] = true
		want := readRunRow(t, db, run.ID)

		if !run.StartedAt.Equal(want.StartedAt) {
			t.Errorf("run %d 的 started_at = %s，库里是 %s", run.ID, run.StartedAt, want.StartedAt)
		}
		if run.FinishedAt.Valid != want.FinishedAt.Valid {
			t.Errorf("run %d 的 finished_at 可空性对不上：%v / %v",
				run.ID, run.FinishedAt, want.FinishedAt)
		} else if run.FinishedAt.Valid && !run.FinishedAt.Time.Equal(want.FinishedAt.Time) {
			t.Errorf("run %d 的 finished_at = %s，库里是 %s",
				run.ID, run.FinishedAt.Time, want.FinishedAt.Time)
		}
		if !run.Watermark.Equal(want.Watermark) {
			t.Errorf("run %d 的 watermark = %s，库里是 %s", run.ID, run.Watermark, want.Watermark)
		}
		if run.FullScan != want.FullScan {
			t.Errorf("run %d 的 full_scan = %v，库里是 %v", run.ID, run.FullScan, want.FullScan)
		}
		// 四个计数列与两个单独归集的计数列：弄反任意两列都会在这里露出来。
		got := [6]int{run.CheckedWallets, run.BalanceMismatches, run.FrozenMismatches,
			run.Repaired, run.MalformedAccounts, run.StaleReservations}
		wantCounts := [6]int{want.CheckedWallets, want.BalanceMismatches, want.FrozenMismatches,
			want.Repaired, want.MalformedAccounts, want.StaleReservations}
		if got != wantCounts {
			t.Errorf("run %d 的六个计数列是 %v，库里是 %v", run.ID, got, wantCounts)
		}
		if run.Status != want.Status {
			t.Errorf("run %d 的 status = %q，库里是 %q", run.ID, run.Status, want.Status)
		}
		if run.Error.Valid != want.Error.Valid {
			t.Errorf("run %d 的 error 可空性对不上：%v / %v", run.ID, run.Error, want.Error)
		} else if run.Error.Valid && run.Error.String != want.Error.String {
			t.Errorf("run %d 的 error = %q，库里是 %q", run.ID, run.Error.String, want.Error.String)
		}
	}

	// 刚跑的两轮都在清单里，且读回来的是它们各自的档：写侧刚断言过这两轮的
	// full_scan，读侧要是把这一列打反了，这里就对不上。
	for _, want := range []struct {
		id       int64
		fullScan bool
	}{{full.RunID, true}, {incremental.RunID, false}} {
		if !seen[want.id] {
			t.Fatalf("清单里没有刚跑的那一轮 run %d", want.id)
		}
	}
	for _, run := range page.Runs {
		if run.ID == full.RunID && !run.FullScan {
			t.Errorf("全量那一轮（run %d）读回来却是增量", run.ID)
		}
		if run.ID == incremental.RunID && run.FullScan {
			t.Errorf("增量那一轮（run %d）读回来却是全量", run.ID)
		}
	}

	// 本轮第二轮是接着第一轮的起点扫的：那个值来自库里上一轮的开始时刻。
	// 这一条把「起点」这一列的语义钉在读写两侧之间。
	first := readRunRow(t, db, full.RunID)
	second := readRunRow(t, db, incremental.RunID)
	if !second.Watermark.Equal(first.StartedAt) {
		t.Errorf("增量那一轮的起点是 %s，上一轮开始于 %s", second.Watermark, first.StartedAt)
	}
}

// reconcileWithInterval 跑一轮对账，并断言这一轮的档与给的间隔相符。
func reconcileWithInterval(t *testing.T, db *sql.DB, interval time.Duration) *Report {
	t.Helper()

	cfg := noRepair()
	cfg.FullScanInterval = interval
	report := reconcile(t, db, cfg)
	if report.FullScan != (interval <= 0) {
		t.Fatalf("这一轮的 full_scan = %v，与间隔 %v 对不上", report.FullScan, interval)
	}
	return report
}

// TestReconcileRunsOrderAndLimit 钉住顺序与 limit 的语义。
//
// 顺序：新的在前。判据按整页判（相邻两行不递增就红），不靠「刚跑的那一轮排在第
// 一行」——同一张表上还有别的用例留下的行，它们也会被读出来。
//
// limit：`More` 由**多读一行**得出，不由 `len(Runs) == limit` 推断。这两者在
// 「这一页刚好有 limit 条、后面没有了」这一格上分道扬镳：按行数推断会误报「后面
// 还有」，而那一句会让人去翻一份其实已经读完的清单。所以三种 limit 都要判：
// limit=1 时 More 为真；limit 足够大时 More 为假、条数等于库里的行数；limit 恰好
// 等于总行数时 More 同样为假——最后这一格正是上面说的那一格。
func TestReconcileRunsOrderAndLimit(t *testing.T) {
	db := testDB(t)
	seed(t, db, "100")

	// 两轮：桌面上的这张表在本包里一定不止一行（本包其它用例也跑对账），
	// 所以 limit=1 时一定有下一页。
	reconcileWithInterval(t, db, 0)
	reconcileWithInterval(t, db, reconcileInterval)

	var total int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM reconcile_runs`).Scan(&total); err != nil {
		t.Fatalf("数运行记录失败: %v", err)
	}

	all, err := New(db).ReconcileRuns(context.Background(), 0)
	if err != nil {
		t.Fatalf("读对账运行记录失败: %v", err)
	}
	if len(all.Runs) != total {
		t.Errorf("limit 为 0 时应当读回全部 %d 行，读回 %d 行", total, len(all.Runs))
	}
	if all.More {
		t.Errorf("limit 为 0 没有下一页，More 却是 true")
	}
	for i := 1; i < len(all.Runs); i++ {
		if all.Runs[i].StartedAt.After(all.Runs[i-1].StartedAt) {
			t.Errorf("第 %d 行（%s）比上一行（%s）新：清单要新的在前",
				i, all.Runs[i].StartedAt, all.Runs[i-1].StartedAt)
		}
	}

	if total < 2 {
		t.Fatalf("这一张表里应当不止一行（本次刚跑了两轮），得到 %d 行", total)
	}
	page, err := New(db).ReconcileRuns(context.Background(), 1)
	if err != nil {
		t.Fatalf("读对账运行记录失败: %v", err)
	}
	if len(page.Runs) != 1 {
		t.Fatalf("limit=1 时应当只读回一行，读回 %d 行", len(page.Runs))
	}
	if !page.More {
		t.Errorf("这一张表有 %d 行，limit=1 时 More 应当为 true", total)
	}
	// 只读回一行时那一行必须是最新的那一行。
	if !page.Runs[0].StartedAt.Equal(all.Runs[0].StartedAt) {
		t.Errorf("limit=1 读回的是 %s，最新的一行是 %s",
			page.Runs[0].StartedAt, all.Runs[0].StartedAt)
	}

	// 刚好装满一页：limit 与总行数相等，也就是「这一页刚好有 limit 条、后面没有
	// 了」。按行数推断会在这一格上报 true，而这正是本节要挡的那一件事。
	exact, err := New(db).ReconcileRuns(context.Background(), total)
	if err != nil {
		t.Fatalf("读对账运行记录失败: %v", err)
	}
	if len(exact.Runs) != total {
		t.Fatalf("limit=%d 时应当读回 %d 行，读回 %d 行", total, total, len(exact.Runs))
	}
	if exact.More {
		t.Errorf("limit=%d 恰好装满一页、后面没有下一页，More 却是 true", total)
	}
}
