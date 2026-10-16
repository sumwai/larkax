package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/snapshotwire"
)

// snapshotUsage 是 snapshot 子命令的帮助。
const snapshotUsage = `larkaxctl snapshot 维护请求结构快照。

用法：
  larkaxctl snapshot cleanup               按 settings 里的留存期删除到期分区
  larkaxctl snapshot cleanup --dry-run     只列出会被删除的分区，不删

留存期取自 settings 表的 request_snapshot.retention_days，
不在命令行上另设参数：同一件事有两个来源时，命令行的那个会悄悄赢，
而「留存期到底是几天」会变成一件要看是谁执行的才知道的事。
`

// runSnapshot 处理 snapshot 子命令。
func runSnapshot(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, snapshotUsage)
		return usageErr(errors.New("snapshot 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "cleanup":
		return runSnapshotCleanup(rest)
	case "help", "-h", "--help":
		fmt.Print(snapshotUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, snapshotUsage)
		return usageErr(fmt.Errorf("未知的 snapshot 子命令 %q", action))
	}
}

// runSnapshotCleanup 删除到期分区。
//
// 它是留存承诺的兑现方式，所以输出的是**被删除的分区名**而不是一句
// 「完成」：一天都没到期时，命令的输出必须与「根本没跑成」区分得开。
func runSnapshotCleanup(args []string) error {
	fs := flag.NewFlagSet("snapshot cleanup", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, snapshotUsage) }
	dryRun := fs.Bool("dry-run", false, "只列出会被删除的分区，不执行删除")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	cfg, err := config.Load()
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
	snapCfg, err := snapshotwire.LoadConfig(values)
	if err != nil {
		return err
	}

	if !snapCfg.Enabled {
		// 关掉采集时仍然清理：已有的分区还要按留存期清掉，
		// 「关掉」承诺的是不再写入新的，而不是把旧的留到永远。
		fmt.Fprintln(os.Stderr, "提示：快照采集已关闭，本次只清理存量分区。")
	}

	collector, err := snapshotwire.New(snapCfg, snapshotwire.Wrap(db), nil)
	if err != nil {
		return err
	}

	at := time.Now().UTC()
	if *dryRun {
		expired, listErr := collector.ExpiredPartitions(ctx, at)
		if listErr != nil {
			return listErr
		}
		printPartitions("将删除", expired)
		return nil
	}

	dropped, err := collector.DropExpired(ctx, at)
	if err != nil {
		return err
	}
	printPartitions("已删除", dropped)
	return nil
}

// printPartitions 打印分区名清单。
//
// 一个都没删时也打印一行明确的说明，而不是什么都不输出：
// 「没有到期分区」与「命令因为某个原因没跑起来」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。
func printPartitions(verb string, names []string) {
	if len(names) == 0 {
		fmt.Printf("%s：0 个（没有到期分区）\n", verb)
		return
	}
	fmt.Printf("%s %d 个分区：\n", verb, len(names))
	fmt.Println("  " + strings.Join(names, "\n  "))
}
