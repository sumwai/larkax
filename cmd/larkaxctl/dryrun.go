package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
)

// 这个文件装 --dry-run 的两件共用东西：一条「事务里跑完就回滚」的辅助，
// 与干跑输出的固定尾注。
//
// **干跑不另写一份读侧模拟**：预览与真跑走同一条写路径（同一份 SQL、同一份
// 校验、同一份幂等判据），唯一的差别是事务不提交。两份实现迟早分叉，而分叉的
// 表现是「预览说行、执行报错」——那正好毁掉预览的全部意义。

// dryRunNote 是干跑输出末尾那一行。
//
// 预览与真跑共用同一个渲染函数，前半段因此长得几乎一样；一句固定尾注说明
// 「这些都没有写进数据库」，两种结果才不会混（LESSONS 第 16 条：跳过与通过、
// 空操作与真写入，在输出上不能长得一样）。
const dryRunNote = "干跑：以上内容没有写进数据库。"

// inRolledBackTx 在一条事务里执行 fn，无论成败结束时都回滚。
//
// 干跑的全部保证都落在这个 deferred Rollback 上：fn 里走的是与真跑同一条写路径，
// 写进去的行看得见、读得回（预览里的 id、时刻、余额前后都由此而来），
// 但一条都不会落库。
func inRolledBackTx(ctx context.Context, db *sql.DB, fn func(context.Context, *sql.Tx) error) error {
	tx, beginErr := db.BeginTx(ctx, nil)
	if beginErr != nil {
		return fmt.Errorf("开启干跑事务失败: %w", beginErr)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(ctx, tx)
}

// printDryRunNote 打出干跑尾注。
func printDryRunNote(w io.Writer) {
	_, _ = fmt.Fprintln(w, dryRunNote)
}
