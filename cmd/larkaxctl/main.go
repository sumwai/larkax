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
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/migrate"
)

// version 由构建时注入，与主服务用**同一个变量名**：
// 两个二进制的版本必须能对上，否则「命令行跑通了但服务起不来」这类
// 错配又多出一个来源。
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "larkaxctl: %v\n", err)
		os.Exit(1)
	}
}

const usage = `larkaxctl 是 larkax 的运维命令行。

用法：
  larkaxctl migrate up                     应用全部未执行的迁移
  larkaxctl migrate down                   回滚最近一次迁移
  larkaxctl migrate down-to <版本>          回滚到指定版本（0 表示全部回滚）
  larkaxctl migrate up-to <版本>            前进到指定版本
  larkaxctl migrate status                 显示每个迁移的应用状态
  larkaxctl migrate version                显示当前数据库版本
  larkaxctl migrate create <名称>           生成迁移文件
  larkaxctl version                        打印版本号

环境变量与主服务一致，见 internal/config。

`

// migrationsDir 是迁移文件所在目录，相对于仓库根目录。
const migrationsDir = "internal/migrate/sql"

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("缺少子命令")
	}

	switch os.Args[1] {
	case "migrate":
		return runMigrate(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(version)
		return nil
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
	action, rest := rest[0], rest[1:]

	if action == "create" {
		if len(rest) < 1 {
			return errors.New("用法：larkaxctl migrate create <名称>")
		}
		// 第二个参数固定为 sql。goose 默认生成 .go 迁移，而内嵌规则是
		// sql/*.sql，生成的 Go 文件既不会被执行，又会在迁移目录里多出一个包。
		//
		// create 只写文件，不碰数据库（第二个参数为 nil），所以这里没有可传的
		// context；RunContext 在 db 为 nil 时同样只做文件操作。
		if err := goose.RunContext(context.Background(), "create", nil, migrationsDir, rest[0], "sql"); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "已生成到 %s/\n", migrationsDir)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// 变量名不复用 err：这里的任何一次重新声明都会让后面读代码的人
	// 停下来判断「看到的是哪个 err」，而包一层名字的成本是零。
	dialectErr := migrate.SetDialect("postgres")
	if dialectErr != nil {
		return fmt.Errorf("设置迁移方言失败: %w", dialectErr)
	}

	db, err := sql.Open("pgx", cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("打开数据库连接失败: %w", err)
	}
	// 关闭错误无处可报：此时函数已经在返回的路上，命令行该打印的错误
	// 已经打印了，而关闭失败不会改变命令的结果。
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}

	goose.SetBaseFS(migrate.FS())

	switch action {
	case "up", "down", "status", "version":
		return gooseRun(ctx, db, action)

	case "down-to":
		ver, err := parseVersion(action, rest)
		if err != nil {
			return err
		}
		return stepTo(ctx, db, *ver, false)

	case "up-to":
		ver, err := parseVersion(action, rest)
		if err != nil {
			return err
		}
		return stepTo(ctx, db, *ver, true)

	default:
		return fmt.Errorf("未知的 migrate 子命令 %q", action)
	}
}

// stepTo 一步一步地前进或回滚到目标版本。
//
// 不用 goose 自带的 up-to / down-to：实测它们会停在目标版本的相邻位置
// （从 v2 执行 down-to 0 只退到 v1），差一位的表现比不支持更危险——
// 使用者会以为已经回滚干净了。自己循环，行为就是「版本号到目标为止」，
// 与直觉一致，也好测。
func stepTo(ctx context.Context, db *sql.DB, target int64, forward bool) error {
	for {
		current, err := goose.GetDBVersionContext(ctx, db)
		if err != nil {
			return fmt.Errorf("读取当前版本失败: %w", err)
		}

		if forward {
			if current >= target {
				return nil
			}
			if err := goose.UpByOneContext(ctx, db, "."); err != nil {
				return fmt.Errorf("前进到 %d 时失败: %w", target, err)
			}
			continue
		}

		if current <= target {
			return nil
		}
		if err := goose.DownContext(ctx, db, "."); err != nil {
			return fmt.Errorf("回滚到 %d 时失败: %w", target, err)
		}
	}
}

func gooseRun(ctx context.Context, db *sql.DB, action string) error {
	if err := goose.RunContext(ctx, action, db, "."); err != nil {
		return fmt.Errorf("执行 %s 失败: %w", action, err)
	}
	return nil
}

func parseVersion(action string, args []string) (*int64, error) {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return nil, fmt.Errorf("用法：larkaxctl migrate %s <版本>", action)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(args[0]), 10, 64)
	if err != nil || v < 0 {
		return nil, fmt.Errorf("%s 的版本号必须是非负整数，当前为 %q", action, args[0])
	}
	return &v, nil
}
