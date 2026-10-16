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
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/settings"
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
  larkaxctl reconcile                      跑一轮对账（开关与口径见 settings 表）
  larkaxctl snapshot cleanup               按留存期删除到期的快照分区
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
	case "reconcile":
		return runReconcile(os.Args[2:])
	case "snapshot":
		return runSnapshot(os.Args[2:])
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

// runReconcile 跑一轮对账任务。
//
// # 为什么它对接到命令行而不是做成后台定时任务
//
// 定时任务需要选主与「上一轮还没跑完怎么办」这两件事，而它们与对账本身的
// 逻辑无关。挂到命令行之后，跑它的是外部的调度器（cron、Kubernetes 的
// CronJob、运维的手），那两件事由**那个**调度器负责——它本来就更擅长。
//
// 开关与口径全部来自 settings 表，命令行不带参数：这样「线上跑的是哪一套
// 口径」只有一个答案（库里的值），而不是「命令行里带了什么」。
func runReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("reconcile 不接受位置参数，得到 %q：开关与口径在 settings 表里", fs.Arg(0))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("打开数据库连接失败: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
	defer cancel()
	if pingErr := db.PingContext(ctx); pingErr != nil {
		return fmt.Errorf("连接数据库失败: %w", pingErr)
	}

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		return err
	}
	rc, err := store.ConfigFromSettings(values)
	if err != nil {
		return err
	}

	logger := observe.NewLogger(cfg.LogLevel, "", cfg.IsProduction())
	report, err := store.New(db).Reconcile(ctx, rc, store.LogAlerts(logger))
	// 报告先打，再处理错误：跑了一半的结果比一句「失败了」有用得多——
	// 它至少说明比到了哪里、已经发现了什么。
	printReconcileReport(os.Stdout, report)
	if err != nil {
		return err
	}
	// 发现不一致时以非零退出码收尾：调度器与告警系统本来就盯着退出码，
	// 而「对账跑完了但有 3 条不一致」与「对账跑完了一切正常」是两件事，
	// 让它们落在同一个退出码上等于把告警交给「有没有人去看日志」。
	if len(report.Findings) > 0 {
		return fmt.Errorf("发现 %d 条不一致（见上面的清单与 run_id=%d）", len(report.Findings), report.RunID)
	}
	return nil
}

// reconcileTimeout 是单轮对账的时间上限。
//
// 命令行要有上限，否则一次卡住的查询会挂到运维发现为止。默认值给得宽：
// 全量扫描在大库上本来就慢，而把上限设小会让「跑不完」看起来像
// 「跑完了什么都没有」。
const reconcileTimeout = 30 * time.Minute

// printReconcileReport 把一轮对账的结果打到 w。
//
// 每处写入的返回值都显式丢弃（`_, _ =`）：目标是 stdout，写不进去只可能是
// 下游管道断了（`larkaxctl reconcile | head` 就是最常见的一种），
// 那种情况下重试与报错都没有意义，而它对这一轮对账的结论也毫无影响。
func printReconcileReport(w io.Writer, r *store.Report) {
	if r == nil {
		return
	}
	scope := "增量"
	if r.FullScan {
		scope = "全量"
	}
	_, _ = fmt.Fprintf(w, "对账 run_id=%d %s 起点=%s 比对=%d 行\n",
		r.RunID, scope, r.Watermark.Format(time.RFC3339), r.CheckedWallets)

	if len(r.Findings) == 0 {
		_, _ = fmt.Fprintln(w, "没有不一致。")
		return
	}
	_, _ = fmt.Fprintf(w, "不一致 %d 条（余额 %d、冻结 %d、畸形科目 %d）：\n",
		len(r.Findings), r.BalanceMismatches(), r.FrozenMismatches(), r.MalformedAccounts())
	for _, f := range r.Findings {
		_, _ = fmt.Fprintf(w, "  %-18s user=%d asset=%s 快照=%s 账本=%s 差=%s 已修=%v\n",
			f.Kind, f.UserID, f.Asset, f.Snapshot, f.Ledger, f.Diff(), f.Repaired)
		// 原因与补充说明各占一行：两者都可能很长，挤在同一行里会让人
		// 分不清哪一段是「为什么没修」、哪一段是「具体是什么情况」。
		if f.Unrepairable != "" {
			_, _ = fmt.Fprintf(w, "      未修原因：%s\n", f.Unrepairable)
		}
		if f.Detail != "" {
			_, _ = fmt.Fprintf(w, "      详情：%s\n", f.Detail)
		}
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
