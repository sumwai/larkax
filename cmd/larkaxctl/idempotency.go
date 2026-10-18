package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/settings"
)

// maintenanceIdempotencyHelp 是 maintenance idempotency 的两版帮助文本（见 helpText）。
var maintenanceIdempotencyHelp = helpText{
	short: maintenanceIdempotencyShortUsage,
	long:  maintenanceIdempotencyUsage,
}

// maintenanceIdempotencyShortUsage 是 maintenance idempotency 的短版：
// `-h` / `--help` 与参数写错时打的那一份。
const maintenanceIdempotencyShortUsage = `larkaxctl maintenance idempotency 按留存期清理请求级幂等记录。

用法：
  larkaxctl maintenance idempotency               按 settings 里的留存期删除到期的记录
  larkaxctl maintenance idempotency --dry-run     只数一遍会被删除的条数，不删

留存期取自 settings 表的 idempotency.retention_hours，不在命令行上另设参数。

更细的边界：larkaxctl maintenance idempotency help。
`

// maintenanceIdempotencyUsage 是 maintenance idempotency 的长版：
// `maintenance idempotency help`（旧写法 `idempotency cleanup help` 也走这一份）。
//
// 它原先是一个顶层命令组（larkaxctl idempotency cleanup）的帮助，#475 把它
// 收进维护域：删的是到期数据，与「幂等」这个概念本身无关。旧路径仍然可用
// （见 main.go 的 legacyCommands），接的是同一个清理函数。
const maintenanceIdempotencyUsage = `larkaxctl maintenance idempotency 按留存期清理请求级幂等记录。

用法：
  larkaxctl maintenance idempotency               按 settings 里的留存期删除到期的记录
  larkaxctl maintenance idempotency --dry-run     只数一遍会被删除的条数，不删

留存期取自 settings 表的 idempotency.retention_hours，
不在命令行上另设参数：同一件事有两个来源时，命令行的那个会悄悄赢，
而「留存期到底是几小时」会变成一件要看是谁执行的才知道的事。

旧路径：larkaxctl idempotency cleanup 仍然可用，与 larkaxctl maintenance idempotency
逐字相同。

更细的约定见 docs/cli-reference.md 的「maintenance idempotency」一节。
`

// runIdempotency 处理顶层的 idempotency 子命令。
//
// 这是一个旧路径：这一组已经搬进 maintenance 域（#475），这里只为已经在跑的
// 脚本与部署流程留着，不列进根帮助页。它把 cleanup 与 help 原样接给搬过去
// 的那一套，所以新旧两条路径的输出逐字相同。
func runIdempotency(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, maintenanceIdempotencyShortUsage)
		return usageErr(errors.New("idempotency 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "cleanup":
		return runIdempotencyCleanup(rest)
	case "help":
		printUsage(os.Stdout, maintenanceIdempotencyUsage)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, maintenanceIdempotencyShortUsage)
		return nil
	default:
		printUsage(os.Stderr, maintenanceIdempotencyShortUsage)
		return usageErr(fmt.Errorf("未知的 idempotency 子命令 %q", action))
	}
}

// cleanupExpiredPredicate 是「哪些行算到期」这个判断，dry-run 与真删共用。
//
// 它与 idempotency.Store.Cleanup 里那条 DELETE 的 WHERE **逐字相同**，
// 而这是本命令最容易出错的地方：dry-run 与真删是两条 SQL，条件一旦漂移
// （把 `<` 写成 `<=`、把 interval 的单位从 second 换成 hour），干跑报出的
// 条数就与真删的条数对不上——而使用者正是拿前者决定要不要跑后者。
//
// 为什么不能只留一份：Store 只导出 Cleanup（那是唯一的删除入口），没有
// 「只数一遍」的方法，而 dry-run 不能真的删。所以判据只能写两遍，
// 靠 cmd/larkaxctl/idempotency_test.go 里那条集成用例钉住它们相等
// （先 dry-run 再真删，断言两个数相等）。
const cleanupExpiredPredicate = `created_at < now() - ($1::bigint * interval '1 second')`

// runIdempotencyCleanup 删除到期幂等记录。
//
// 它是留存承诺的兑现方式，所以输出的是**删了多少条**而不是一句「完成」：
// 一条都没到期时，命令的输出必须与「根本没跑成」区分得开。
func runIdempotencyCleanup(args []string) error {
	if err := jsonUnsupported("maintenance idempotency"); err != nil {
		return err
	}

	// `maintenance idempotency help`（以及旧写法 `idempotency cleanup help`）
	// 给长版；`-h` / `--help` 归下面的 flag 解析，给短版。
	if len(args) > 0 && args[0] == "help" {
		printUsage(os.Stdout, maintenanceIdempotencyUsage)
		return nil
	}

	fs := flag.NewFlagSet("maintenance idempotency", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, maintenanceIdempotencyShortUsage) }
	dryRun := fs.Bool("dry-run", false, "只数一遍会被删除的条数，不执行删除")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		return err
	}
	hours, err := values.Int("idempotency.retention_hours")
	if err != nil {
		return err
	}
	retention := idempotency.Retention(hours)

	// 留存时长不是正数时，真删会拒绝（见 idempotency.Store.Cleanup）。dry-run
	// 必须同样拒绝，否则它会照着一条「删掉全部」的判据数出一个很大的数，
	// 而真删一个都不删——一个说「将删除 12000 条」的干跑比不跑更坏。
	if retention <= 0 {
		return fmt.Errorf("settings.idempotency.retention_hours=%d 换算出的留存时长不是正数，拒绝清理", hours)
	}

	if *dryRun {
		expired, countErr := countExpired(ctx, db, retention)
		if countErr != nil {
			return countErr
		}
		printCleanup("将删除", expired)
		return nil
	}

	deleted, err := idempotency.New(db).Cleanup(ctx, retention)
	if err != nil {
		return err
	}
	printCleanup("已删除", deleted)
	return nil
}

// countExpired 数一遍到期记录，供 --dry-run 使用。
//
// 判据来自 cleanupExpiredPredicate，见那里的注释：它与 Store.Cleanup 的
// WHERE 必须逐字相同，否则干跑报的数会骗人。
func countExpired(ctx context.Context, db *sql.DB, retention time.Duration) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM idempotency_keys WHERE `+cleanupExpiredPredicate,
		int64(retention.Seconds())).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计到期幂等记录失败: %w", err)
	}
	return n, nil
}

// printCleanup 打印一条清理结果。
//
// 一条都没删时也打印一行明确的说明，而不是什么都不输出：
// 「没有到期记录」与「命令因为某个原因没跑起来」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。
func printCleanup(verb string, n int64) {
	if n == 0 {
		_, _ = fmt.Fprintf(humanStdout(), "%s：0 条（没有到期记录）\n", verb)
		return
	}
	_, _ = fmt.Fprintf(humanStdout(), "%s %d 条\n", verb, n)
}
