package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/settings"
)

// maintenanceSessionHelp 是 maintenance session 的两版帮助文本（见 helpText）。
var maintenanceSessionHelp = helpText{
	short: maintenanceSessionShortUsage,
	long:  maintenanceSessionUsage,
}

// maintenanceSessionShortUsage 是 maintenance session 的短版：
// `-h` / `--help` 与参数写错时打的那一份。
const maintenanceSessionShortUsage = `larkaxctl maintenance session 按留存期清理过期与已吊销的控制台会话。

用法：
  larkaxctl maintenance session               删掉过期会话与吊销超过留存期的会话
  larkaxctl maintenance session --dry-run     只数一遍会被删除的条数，不删

留存期取自 settings 表的 console.session_retention_days，不在命令行上另设参数。

更细的边界：larkaxctl maintenance session help。
`

// maintenanceSessionUsage 是 maintenance session 的长版：
// `maintenance session help`。
const maintenanceSessionUsage = `larkaxctl maintenance session 按留存期清理过期与已吊销的控制台会话。

用法：
  larkaxctl maintenance session               删掉过期会话与吊销超过留存期的会话
  larkaxctl maintenance session --dry-run     只数一遍会被删除的条数，不删

留存期取自 settings 表的 console.session_retention_days，
不在命令行上另设参数：同一件事有两个来源时，命令行的那个会悄悄赢，
而「留存期到底是几天」会变成一件要看是谁执行的才知道的事。

删两类行：expires_at 已经过去的（到点即删，与留存期无关），以及已吊销且
revoked_at 早于「现在减去留存期」的。窗口内被吊销的会话留着：排查「这个会话
是什么时候被踢的」要看它们。

重复跑是安全的：第二次没有可删的行，输出 0 条并退出 0。

更细的约定见 docs/cli-reference.md 的「maintenance session」一节。
`

// runMaintenanceSession 删除到期与已过留存窗口的会话。
//
// 输出的是**删了多少条**而不是一句「完成」：一条都没删时，命令的输出必须与
// 「根本没跑成」区分得开（见 LESSONS.md 第 16 条）。
func runMaintenanceSession(args []string) error {
	if err := cli.JSONUnsupported("maintenance session"); err != nil {
		return err
	}

	// `maintenance session help` 给长版；`-h` / `--help` 归下面的 flag
	// 解析，给短版。
	if len(args) > 0 && args[0] == "help" {
		cli.PrintUsage(os.Stdout, maintenanceSessionHelp.long)
		return nil
	}

	fs := cli.NewFlagSet("maintenance session", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, maintenanceSessionHelp.short) }
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	// 位置参数一律拒掉：`dry-run`（漏了横线）会被 flag 当成位置参数留在
	// fs.Args() 里，不拦的话它就成了一条**真删**命令——而写它的人以为在干跑。
	if fs.NArg() > 0 {
		fs.Usage()
		return cli.UsageErr(fmt.Errorf("maintenance session 不接收位置参数，多余的 %q（干跑请写 --dry-run）", fs.Arg(0)))
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := cli.OpenDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		return err
	}
	days, err := values.Int("console.session_retention_days")
	if err != nil {
		return err
	}

	// 留存期不是正数时真删会拒绝（见 auth.PurgeSessions）。干跑必须同样拒绝：
	// 否则它会照着一条「删掉全部已吊销行」的判据数出一个很大的数，而真删一个
	// 都不删——一个说「将删除 12000 条」的干跑比不跑更坏。
	if days <= 0 {
		return fmt.Errorf("settings.console.session_retention_days=%d 不是正数，拒绝清理", days)
	}
	// 上界按「换算成 Duration 不溢出」算：再大一点，days*24h 会回绕成一个
	// 很小的正数（213505 天回绕成 +24h25m26s），下面那条 `retention <= 0`
	// 放行，等于拿一天的窗口去删 21 万年的会话。
	const maxSessionRetentionDays = int64(math.MaxInt64 / int64(24*time.Hour))
	if int64(days) > maxSessionRetentionDays {
		return fmt.Errorf("settings.console.session_retention_days=%d 超出可换算的上限（最多 %d 天），拒绝清理",
			days, maxSessionRetentionDays)
	}
	retention := time.Duration(days) * 24 * time.Hour

	store := auth.New(db)
	if *dryRun {
		purgeable, countErr := store.CountPurgeableSessions(ctx, retention)
		if countErr != nil {
			return countErr
		}
		printCleanup("将删除", purgeable)
		return nil
	}

	deleted, err := store.PurgeSessions(ctx, retention)
	if err != nil {
		return err
	}
	printCleanup("已删除", deleted)
	return nil
}
