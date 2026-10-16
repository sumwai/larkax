package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sumwai/larkax/internal/config"
)

// openDB 打开一条到 larkax 数据库的连接，并确认它真的能用。
//
// 打开与探活合在一处，是因为两者失败时的处置相同（去看环境、去看 DSN），
// 而且分开写时「忘了探活」的表现是一个连不上的库拖到第一次查询才报错，
// 那时错误信息已经离原因很远了。两者都标成 exitDBUnreach：脚本对它们的
// 处置是同一种。
//
// 失败时自己关掉句柄，调用方不必为「没拿到连接」再去关一次。
func openDB(ctx context.Context, cfg *config.Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DBDSN)
	if err != nil {
		return nil, dbErr(fmt.Errorf("打开数据库连接失败: %w", err))
	}
	// 变量名不复用 err：这里再 `err :=` 是遮蔽，govet 的 shadow 检查器会报。
	if pingErr := db.PingContext(ctx); pingErr != nil {
		_ = db.Close()
		return nil, dbErr(fmt.Errorf("连接数据库失败: %w", pingErr))
	}
	return db, nil
}
