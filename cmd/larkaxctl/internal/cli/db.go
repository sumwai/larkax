package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/config"
)

// errFlagIntrospection 是收集 flag 名期间 OpenDB 的返回值。
//
// 生成补全要把每条命令路径按 `-h` 走一遍（见 collect.go 的 CollectFlags）。
// 正常路径在解析参数时就停了，走不到这里；这一道是兜底：万一哪条命令在解析
// 之前就要连库，那次遍历也只拿到一个错误，不会真的碰到数据库。
var errFlagIntrospection = errors.New("收集补全数据期间不连库")

// OpenDB 打开一条到 larkax 数据库的连接，并确认它真的能用。
//
// 打开与探活合在一处，是因为两者失败时的处置相同（去看环境、去看 DSN），
// 而且分开写时「忘了探活」的表现是一个连不上的库拖到第一次查询才报错，
// 那时错误信息已经离原因很远了。两者都标成 exitDBUnreach：脚本对它们的
// 处置是同一种。
//
// 失败时自己关掉句柄，调用方不必为「没拿到连接」再去关一次。
func OpenDB(ctx context.Context, cfg *config.OpsConfig) (*sql.DB, error) {
	// 收集期**不连库**：那一趟遍历要的是「各命令注册了哪些选项」这件副作用，
	// 它一条记录都还没打算读。真去连的话，生成补全就要求这台机器上有可用的
	// 数据库与有效配置，而补全脚本是装机时就要装好的东西——没有库的机器也得
	// 生成得出来。所以这里直接返回，不碰 cfg.DBDSN。
	if CollectingFlags() {
		return nil, errFlagIntrospection
	}
	db, err := sql.Open("pgx", cfg.DBDSN)
	if err != nil {
		return nil, DBErr(fmt.Errorf("打开数据库连接失败: %w", err))
	}
	// 变量名不复用 err：这里再 `err :=` 是遮蔽，govet 的 shadow 检查器会报。
	if pingErr := db.PingContext(ctx); pingErr != nil {
		_ = db.Close()
		return nil, DBErr(fmt.Errorf("连接数据库失败: %w", pingErr))
	}
	return db, nil
}
