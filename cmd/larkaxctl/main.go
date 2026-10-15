// Command larkaxctl 是运维命令行：迁移、状态检查与后续的运维任务。
//
// 与主服务共用同一份配置加载与迁移代码，避免「命令行跑通了但服务起不来」这类错配。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "larkaxctl: %v\n", err)
		os.Exit(1)
	}
}

const usage = `larkaxctl 是 larkax 的运维命令行。

用法：
  larkaxctl migrate up             应用全部未执行的迁移
  larkaxctl migrate down           回滚最近一次迁移
  larkaxctl migrate status         显示迁移状态
  larkaxctl migrate version        显示当前版本
  larkaxctl migrate create <名称>   生成一个新的迁移文件

环境变量与主服务一致，见 internal/config。
`

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("缺少子命令")
	}

	switch os.Args[1] {
	case "migrate":
		return runMigrate(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("未知子命令 %q", os.Args[1])
	}
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("migrate 需要子命令")
	}
	action := rest[0]

	if action == "create" {
		if len(rest) < 2 {
			return errors.New("用法：larkaxctl migrate create <名称>")
		}
		return goose.Run("create", nil, ".", rest[1])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if err := migrate.SetDialect("postgres"); err != nil {
		return fmt.Errorf("设置迁移方言失败: %w", err)
	}

	db, err := sql.Open("pgx", cfg.DB.DSN)
	if err != nil {
		return fmt.Errorf("打开数据库连接失败: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}

	goose.SetBaseFS(migrate.FS())

	switch action {
	case "up", "down", "status", "version", "reset", "redo":
		if err := goose.RunContext(ctx, action, db, "."); err != nil {
			return fmt.Errorf("执行 %s 失败: %w", action, err)
		}
		return nil
	default:
		return fmt.Errorf("未知的 migrate 子命令 %q", action)
	}
}
