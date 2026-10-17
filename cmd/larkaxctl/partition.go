package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/snapshotwire"
)

// partitionUsage 是 partition 子命令的帮助。
const partitionUsage = `larkaxctl partition 维护 usage_events 的月分区。

用法：
  larkaxctl partition ensure [--months N]   建出缺失的月分区
  larkaxctl partition check  [--months N]   只读体检，有问题时非零退出

usage_events 按 created_at 按月分区（见 docs/partitions.md）。分区不会自己长
出来：没有预建时，过了迁移里种子覆盖的月份之后，每一行用量事件都落进兜底
分区 usage_events_default——不报错、查询照常返回正确的行，只是这张表退化成
「所有数据堆在一个分区里」。而兜底分区里的行会挡着那个月的分区建不出来，
要人工先把行搬走。

ensure 与 check 是给外部调度器用的：命令的退出码就是告警信号，0 表示这一次
没有问题。怎么接调度器见 docs/partitions.md。

--months 是「从当月起算几个月」，默认 3（含当月）。它决定的是提前量：给 1 只
保证当月，预建任务停摆超过一个月，下一个月的分区就不存在了；给 3 留出两个月
的缓冲，代价只是多几张空表。

check 只读，它不建任何东西。写入路径自己会保证当月分区存在（见
internal/ledger/store 的 ensureUsageEventPartition），所以 check 报出「缺月
分区」说明预建任务漏跑了，而不是写入路径坏了——这两件事的处置不同，所以
check 不去替 ensure 把分区补上。
`

// partitionMonthsDefault 是 --months 的默认值。
//
// 3 而不是 1：预建任务的失效方式是「停摆一段时间」，而 1 意味着停摆跨过
// 一次月末就会出现缺口。3 让调度器有一整个月的余量去修。
const partitionMonthsDefault = 3

// partitionTimeout 是单次 ensure / check 的时间上限。
//
// 命令行要有上限，否则一次卡住的 DDL 会挂到运维发现为止。给得宽：建分区
// 要拿父表的 ACCESS EXCLUSIVE 锁，排队等一个长事务是正常的。
const partitionTimeout = 2 * time.Minute

// runPartition 处理 partition 子命令。
func runPartition(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, partitionUsage)
		return usageErr(errors.New("partition 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "ensure":
		return runPartitionEnsure(rest)
	case "check":
		return runPartitionCheck(rest)
	case "help", "-h", "--help":
		fmt.Print(partitionUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, partitionUsage)
		return usageErr(fmt.Errorf("未知的 partition 子命令 %q", action))
	}
}

// parsePartitionMonths 解析 --months 并校验取值。
//
// 校验放在这里而不是只靠 store 那一层的错误：`--months 0` 是**参数**问题
// （调度器里的变量没填上），脚本对它的处置是「去改命令」，与「库里出了状况」
// 不是同一件事，退出码也该不同（见 exit.go）。
func parsePartitionMonths(args []string, name string) (int, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, partitionUsage) }
	months := fs.Int("months", partitionMonthsDefault, "从当月起算的月份数（含当月）")
	if err := fs.Parse(args); err != nil {
		return 0, usageErr(err)
	}
	if fs.NArg() > 0 {
		return 0, usageErr(fmt.Errorf("%s 不接受位置参数，得到 %q", name, fs.Arg(0)))
	}
	if *months <= 0 {
		return 0, usageErr(fmt.Errorf("--months 必须为正数，当前为 %d", *months))
	}
	return *months, nil
}

// runPartitionEnsure 建出缺失的 usage_events 月分区。
//
// 它打印**真正建出来的**分区名，而不是一句「完成」：一个都没建时也要有一行
// 明确输出——「没有缺失的分区」与「命令根本没跑成」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。判据与 snapshot cleanup 相同。
func runPartitionEnsure(args []string) error {
	if err := jsonUnsupported("partition ensure"); err != nil {
		return err
	}

	months, err := parsePartitionMonths(args, "partition ensure")
	if err != nil {
		return err
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), partitionTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	from := time.Now().UTC()
	created, ensureErr := store.EnsureUsageEventPartitions(ctx, db, from, months)

	// 先打结果再处理错误：跑了一半的结果比一句「失败了」有用得多——
	// 它至少说明建成了哪几个、卡在哪一个。与 reconcile 的处置一致。
	printPartitionSpan(from, months)
	printCreatedPartitions(created)
	if ensureErr != nil {
		return ensureErr
	}
	return nil
}

// runPartitionCheck 只读体检，有问题时非零退出。
//
// 检查三件事：缺哪些应当存在的月分区；usage_events_default 的行数；
// request_snapshots_default 的行数。三者都是「预建漏跑了」的表现，而它们的
// 现场不同——缺分区是还没写进去，兜底分区非空是已经写进去了。输出里带上
// 表名与行数，让人不必再去连一次库。
//
// 它**不建**任何东西：体检与处置分开，调度器才能在「只是看一眼」与「动手
// 改库」之间自己选。写入路径已经保证当月分区存在（见 store 的
// ensureUsageEventPartition），所以这里报出来的缺口是预建任务的，不是写入
// 路径的。
func runPartitionCheck(args []string) error {
	months, err := parsePartitionMonths(args, "partition check")
	if err != nil {
		return err
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), partitionTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	from := time.Now().UTC()
	missing, err := store.MissingUsageEventPartitions(ctx, db, from, months)
	if err != nil {
		return err
	}

	usageDefaultRows, err := countRows(ctx, db, store.UsageEventDefaultPartition)
	if err != nil {
		return err
	}
	snapshotDefaultRows, err := countRows(ctx, db, snapshotwire.DefaultPartition)
	if err != nil {
		return err
	}

	problems := partitionProblems(missing, usageDefaultRows, snapshotDefaultRows)
	cause := partitionCheckErr(problems)

	if jsonOutput() {
		return emitJSON(os.Stdout, partitionCheckDoc(
			from, months, missing, usageDefaultRows, snapshotDefaultRows, problems), cause)
	}

	printPartitionSpan(from, months)
	printMissingPartitions(missing)
	fmt.Printf("兜底分区 %s：%d 行\n", store.UsageEventDefaultPartition, usageDefaultRows)
	fmt.Printf("兜底分区 %s：%d 行\n", snapshotwire.DefaultPartition, snapshotDefaultRows)
	return cause
}

// partitionCheckErr 把体检的问题清单折成退出码。
//
// 抽出来是因为两条渲染路径都要用它，而「有几条问题才算不通过」只有一处判据：
// 各写一遍的表现是 json 模式下的退出码与交互模式不同，而脚本正是按退出码
// 分派的（见 docs/cli-output.md 的「退出码」）。
func partitionCheckErr(problems []string) error {
	if len(problems) > 0 {
		return fmt.Errorf("usage_events 月分区体检不通过：%s", strings.Join(problems, "；"))
	}
	return nil
}

// partitionCheckDoc 把一次体检折成机器可读输出。
//
// 三条判据各自的**取值**都给出来（缺几个分区、两个兜底分区各几行），不只给
// 一个「过没过」：那句结论由它们算出来，而「有 3 行落进兜底分区」与
// 「有 3000 行」要做的处置完全不同。
//
// 月份区间三个键一起给：缺分区时最要紧的上下文就是「这一段区间本来该有几个
// 分区」，而它只存在于命令行参数里（见 printPartitionSpan）。
func partitionCheckDoc(
	from time.Time, months int, missing []string,
	usageDefaultRows, snapshotDefaultRows int64, problems []string,
) map[string]any {
	start, _ := store.UsageEventMonthBounds(from)
	end, _ := store.UsageEventMonthBounds(start.AddDate(0, months-1, 0))

	status := jsonOK
	if len(problems) > 0 {
		status = jsonFailed
	}
	doc := jsonDoc(status)
	doc["summary"] = map[string]any{"months": months, "missing": len(missing)}
	doc["from_month"] = start.Format("2006-01")
	doc["to_month"] = end.Format("2006-01")
	doc["missing"] = jsonList(missing)
	doc["problems"] = jsonList(problems)
	// 两个兜底分区的**行数**按表名给：它们是库里真实的表名，脚本要按它去查
	// 那几行是什么，翻译一遍就多一层对应关系。行数不进 summary——同一个事实
	// 有两个键时，改了其中一处另一处就开始说谎。
	doc["default_partitions"] = map[string]any{
		store.UsageEventDefaultPartition: usageDefaultRows,
		snapshotwire.DefaultPartition:    snapshotDefaultRows,
	}
	return doc
}

// partitionProblems 把三项检查的结果折成一张问题清单，空清单表示体检通过。
//
// 折成纯函数是为了让判据能**逐项**测。三项里有两项要靠往兜底分区里真的写行
// 才造得出来，而 `request_snapshots_default` 那一项不能那么造：那张表的行数
// 是**全库共享**的状态，本包的 TestPartitionCheckPassesWhenClean 断言它是空
// 的（见 TestPartitionProblemsFlagsEachCondition 的说明）。判据与取值分开之
// 后，这一层不必连库就能把三种条件各测一遍。
func partitionProblems(missing []string, usageDefaultRows, snapshotDefaultRows int64) []string {
	var problems []string
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("usage_events 缺 %d 个月分区", len(missing)))
	}
	if usageDefaultRows != 0 {
		problems = append(problems, fmt.Sprintf("兜底分区 %s 有 %d 行",
			store.UsageEventDefaultPartition, usageDefaultRows))
	}
	if snapshotDefaultRows != 0 {
		problems = append(problems, fmt.Sprintf("兜底分区 %s 有 %d 行",
			snapshotwire.DefaultPartition, snapshotDefaultRows))
	}
	return problems
}

// printPartitionSpan 打印这次体检 / 预建覆盖的月份区间。
//
// 它把「--months 是多少、从哪个月起」摆在输出里：缺分区时最要紧的上下文
// 就是「这一段区间本来该有几个分区」，而它只存在于命令行参数里。
func printPartitionSpan(from time.Time, months int) {
	start, _ := store.UsageEventMonthBounds(from)
	end, _ := store.UsageEventMonthBounds(start.AddDate(0, months-1, 0))
	fmt.Printf("起点 %s，共 %d 个月（%s ~ %s）\n",
		start.Format("2006-01"), months, start.Format("2006-01"), end.Format("2006-01"))
}

// printCreatedPartitions 打印本次真正建出来的分区名。
//
// 一个都没建时也打印一行明确的说明，理由见 runPartitionEnsure。
func printCreatedPartitions(created []string) {
	if len(created) == 0 {
		fmt.Println("没有缺失的分区，本次没有建任何东西。")
		return
	}
	fmt.Printf("已建立 %d 个分区：\n", len(created))
	fmt.Println("  " + strings.Join(created, "\n  "))
}

// printMissingPartitions 打印缺失的月分区名。
func printMissingPartitions(missing []string) {
	if len(missing) == 0 {
		fmt.Println("usage_events 应当存在的月分区都在。")
		return
	}
	fmt.Printf("usage_events 缺失 %d 个月分区：\n", len(missing))
	fmt.Println("  " + strings.Join(missing, "\n  "))
}

// countRows 统计一张表的行数。
//
// 表名是包里的常量（store.UsageEventDefaultPartition 与
// snapshotwire.DefaultPartition），没有任何外部输入，所以可以拼进 SQL——
// 标识符本来也不能参数化。
//
// 表不存在时这里会报错而不是回 0：兜底分区不存在意味着越界写入会直接失败，
// 那是另一件要人看的事，把它当成「0 行、一切正常」正好是这个命令要防的
// 那种静默。
func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计 %s 的行数失败: %w", table, err)
	}
	return n, nil
}
