package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// 本文件是 reconcile_runs 的只读一侧：列出对账跑过的那几轮。
//
// 写入方在 reconcile.go（每轮开一行、收尾时把结果填进同一行）。这里只把它读
// 回来，让「跑过几次、上次扫到哪、比出多少差额」在命令行上看得见——这张表同时
// 是下一轮增量扫描的起点（watermark），只写不读的话，那件事只能连库看。
//
// 它不读 reconcile_findings（尚未消除的不一致）：那张表是「现在还剩哪几条」，
// 渲染成一行一条的清单前要先定一处口径（同一 scope 多轮 last_seen_at 怎么归并），
// 而那属于对账报告本身，不属于运行历史。

// ReconcileRun 是 reconcile_runs 里的一行。
//
// FinishedAt 可空：status = running 的那一轮还没收尾。Error 可空：只有失败的那
// 一轮有值。两者都用 sql.Null* 而不是零值表示「没有」——零值时刻会打成一个
// 看起来正常的日期，而「这一轮还没跑完」与「这一轮在公元 1 年结束」是两件事。
type ReconcileRun struct {
	ID         int64
	StartedAt  time.Time
	FinishedAt sql.NullTime
	// Watermark 是这一轮比对的**起点**，也是下一轮增量扫描的开始位置
	// （口径见迁移 00004 里那一列的注释）。
	Watermark time.Time
	// FullScan 为真表示这一轮扫了全量。增量只看上次起点之后变动过的行，
	// 「两边都早就错着」的那种不一致只有全量能发现。
	FullScan bool
	// CheckedWallets 是这一轮比对过的钱包行数。
	CheckedWallets    int
	BalanceMismatches int
	FrozenMismatches  int
	Repaired          int
	// MalformedAccounts 是归不到任何用户头上的那些科目，单独一列
	// （它既不是余额差额也不是冻结差额）。
	MalformedAccounts int
	// StaleReservations 是陈旧的未结算预留。它与其余几档分开：它不是「两边
	// 数字对不上」，而是「这笔预留挂得太久了」。
	StaleReservations int
	Status            string
	Error             sql.NullString
}

// ReconcileRunPage 是一页对账运行记录，More 的含义同 UsageEventPage：
// 由「多读了一行」得出，不由 len(Runs) == limit 推断。
type ReconcileRunPage struct {
	Runs []ReconcileRun
	More bool
}

// reconcileRunColumns 是读一行运行记录要选的列，顺序与下面的 Scan 一致。
const reconcileRunColumns = `id, started_at, finished_at, watermark, full_scan,
	checked_wallets, balance_mismatches, frozen_mismatches, repaired,
	malformed_accounts, stale_reservations, status, error`

// ReconcileRuns 读最近的对账运行记录，新的在前。
//
// 排序补一个 id：同一微秒开的两轮（并发跑时可能发生）只按 started_at 排时
// 顺序不定，而「上一次跑的是哪一轮」正是看这份清单要回答的问题。
//
// limit 为 0 表示不限。上限存在的理由与其余清单命令相同：这张表只追加，
// 不清扫一次就是整表读回来。
func (s *Store) ReconcileRuns(ctx context.Context, limit int) (ReconcileRunPage, error) {
	query := "SELECT " + reconcileRunColumns + " FROM reconcile_runs" +
		" ORDER BY started_at DESC, id DESC"
	if limit > 0 {
		query += " LIMIT " + strconv.Itoa(limit+1)
	}

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return ReconcileRunPage{}, fmt.Errorf("查询对账运行记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page ReconcileRunPage
	for rows.Next() {
		var run ReconcileRun
		if err := rows.Scan(&run.ID, &run.StartedAt, &run.FinishedAt, &run.Watermark,
			&run.FullScan, &run.CheckedWallets, &run.BalanceMismatches,
			&run.FrozenMismatches, &run.Repaired, &run.MalformedAccounts,
			&run.StaleReservations, &run.Status, &run.Error); err != nil {
			return ReconcileRunPage{}, fmt.Errorf("读对账运行记录失败: %w", err)
		}
		page.Runs = append(page.Runs, run)
	}
	if err := rows.Err(); err != nil {
		return ReconcileRunPage{}, fmt.Errorf("读对账运行记录失败: %w", err)
	}

	if limit > 0 && len(page.Runs) > limit {
		page.Runs = page.Runs[:limit]
		page.More = true
	}
	return page, nil
}
