// Package db 打开并管理 PostgreSQL 连接池。
//
// 连接参数全部来自 DSN，不在这里再拆出主机、端口、用户名等字段：
// 拆开就多出几处可以不同步的地方，而配置不同步的故障现场是
// 「连上了但是连错了」，很难发现。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// 连接池参数写死在代码里而不做成配置项。
//
// 它们属于「改错了也不会造成事故」的一类：池子小了顶多排队，
// 大了顶多占内存，都能通过指标看出来并按需调整。做成环境变量则会把它们
// 绑上「必须与数据一起迁移」的负担，而它们其实跟数据毫无关系。
const (
	maxOpenConns    = 32
	maxIdleConns    = 8
	connMaxLifetime = time.Hour
	connMaxIdleTime = 10 * time.Minute

	// pingTimeout 是打开连接池后验证连通性的上限。
	// 启动时的检查要快：连不上就该立刻失败，而不是让编排系统对着一个
	// 半天不响的实例做健康判定。
	pingTimeout = 5 * time.Second
)

// Open 打开连接池并验证一次连通性。
//
// 事先 Ping 一次是有意的：database/sql 是惰性连接，不 Ping 的话
// 「配置写错了」要等到第一个真实请求才暴露，那时现场是「服务起来了但一用就挂」。
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	handle, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库连接失败: %w", err)
	}

	handle.SetMaxOpenConns(maxOpenConns)
	handle.SetMaxIdleConns(maxIdleConns)
	handle.SetConnMaxLifetime(connMaxLifetime)
	handle.SetConnMaxIdleTime(connMaxIdleTime)

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := handle.PingContext(pingCtx); err != nil {
		handle.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return handle, nil
}
