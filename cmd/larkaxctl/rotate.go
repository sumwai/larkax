package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/config"
)

// rotateUsage 是 secret rotate 子命令的帮助。
//
// 它原先是一个顶层命令（larkaxctl rotate）的帮助，#475 把这个动词收进
// secret 域：轮换的是渠道凭证的主密钥，与渠道管理是两件事，所以不挂在
// channel 下面。旧路径留着，见末尾那一行。
const rotateUsage = `larkaxctl secret rotate 轮换渠道凭证的主密钥。

用法：
  larkaxctl secret rotate                     干跑：列出会被改动的行，不写库（默认）
  larkaxctl secret rotate --apply             轮换：逐批补上当前主密钥的数据密钥副本
  larkaxctl secret rotate --verify            验证：每一行是否都含当前主密钥
  larkaxctl secret rotate --batch-size <N>    每批处理的行数（默认 500）

轮换补的是「当前主密钥的一份数据密钥副本」，**不解密凭证正文**：每个信封
本来就被旧主密钥包着，本命令给它再包一份新的。全程不需要明文凭证。

顺序（重要）：本命令只保证新主密钥的副本存在，**不退役旧主密钥**。

  1. 把新主密钥放进 LARKAX_MASTER_KEYS 的**首位**，旧密钥留在后面，重启服务；
  2. larkaxctl secret rotate           看清有多少行要改；
  3. larkaxctl secret rotate --apply   轮换（可中断、可重跑，重跑是幂等的）；
  4. larkaxctl secret rotate --verify  退出码为 0 才说明全库每一行都含新主密钥；
  5. 到这一步才能把旧主密钥从 LARKAX_MASTER_KEYS 里去掉。

第 4 步之前去掉旧主密钥，那些还没轮换到的行就再也解不开了。

跑 --apply 之前先备份 channels 表（至少备份 credential_enc 这一列）：轮换改的
就是这一列。中断不会让数据不可用——每一行要么是旧的（仍被旧主密钥包着），
要么是新的（新旧都能开），不存在解不开的中间态；没跑完的部分 --verify 看得见。

--apply 会取一把 PostgreSQL 会话级互斥锁，同一时刻只允许一处轮换，已有轮换
在跑时直接失败而不排队。干跑与验证是只读的、不取锁，但会在锁被占用时提示
「读到的可能不是终态」。

退出码：--verify 的退出码回答「有没有漏网的行」，有就是非零。--apply 的退出码
只回答「这一轮跑完了没有」——轮换不了的行会被逐行打出来，但那一行坏掉不该
让整次轮换报失败（它再跑一次也还是坏的，由 --verify 与人工处置）。

它需要读到主密钥，所以要设好 LARKAX_MASTER_KEYS。这与 LARKAX_ROLE 无关：
运维命令不属于任何角色，主密钥直接从这个变量读。

旧路径：larkaxctl rotate 仍然可用，与 larkaxctl secret rotate 逐字相同。
`

// rotateTimeout 是这条命令连库与跑完整轮换的时间上限。
//
// 与 reconcileTimeout 同一条理由：命令行要有上限，否则一次卡住的查询会挂到
// 运维发现为止。取值也放宽到 30 分钟——轮换要逐行读写全表，在大库上本来就慢，
// 而把上限设小会让「没跑完」看起来像「跑完了」。
const rotateTimeout = 30 * time.Minute

// rotateRowLimit 是逐行清单一次最多打印的行数。
//
// 条数永远完整打印，只有清单会被截断：一行密文坏掉是异常，几千行同时坏掉
// 说明的是另一回事，那时把终端刷满反而看不清结论。
const rotateRowLimit = 100

// runRotate 跑主密钥轮换，命令是 larkaxctl secret rotate。
//
// 旧路径 larkaxctl rotate 接的也是这个函数（见 main.go 的 legacyCommands）：
// 两条路径走的是同一段代码，输出因此逐字相同。
func runRotate(args []string) error {
	if err := jsonUnsupported("secret rotate"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("secret rotate", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, rotateUsage) }
	apply := fs.Bool("apply", false, "真正改写 credential_enc（不加这个参数只干跑）")
	verify := fs.Bool("verify", false, "只验证每一行是否都含当前主密钥，不写库")
	batchSize := fs.Int("batch-size", channels.DefaultRotateBatch, "每批处理的行数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("secret rotate 不接受位置参数，得到 %q：模式用 --apply 或 --verify 指定", fs.Arg(0))
	}
	if *apply && *verify {
		return errors.New("--apply 与 --verify 不能同时用：一个改写密文，一个只读")
	}
	if *batchSize <= 0 {
		return fmt.Errorf("--batch-size 必须是正整数，当前为 %d", *batchSize)
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	// 主密钥从环境变量直接读，与 channel 的写入命令走同一个函数：轮换需要
	// 主密钥，而「要不要主密钥」由**服务角色**决定，运维命令不属于任何角色。
	// 绑到角色上的表现是 `LARKAX_ROLE=worker larkaxctl rotate` 明明设好了
	// LARKAX_MASTER_KEYS 却报「没有读到主密钥」。
	sealer, err := masterKeySealer()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("打开数据库连接失败: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()
	// 变量名不复用 err：这里的每一次重新声明都会让后面读代码的人停下来
	// 判断「看到的是哪个 err」，而包一层名字的成本是零。
	if pingErr := db.PingContext(ctx); pingErr != nil {
		return fmt.Errorf("连接数据库失败: %w", pingErr)
	}

	rotator := channels.NewRotator(db, sealer)
	rotator.BatchSize = *batchSize

	switch {
	case *verify:
		return runRotateVerify(ctx, rotator)
	case *apply:
		return runRotateApply(ctx, rotator)
	default:
		return runRotatePlan(ctx, rotator)
	}
}

// runRotatePlan 干跑，不写库。
func runRotatePlan(ctx context.Context, rotator *channels.Rotator) error {
	report, err := rotator.Plan(ctx)
	if err != nil {
		return err
	}
	printRotateReport(humanStdout(), report)
	return nil
}

// runRotateApply 逐批轮换。
//
// 它先打一句备份提示再动手：这条命令改的是全库的凭证密文，
// 而「先备份」是唯一一个出了事还能回头的动作。
func runRotateApply(ctx context.Context, rotator *channels.Rotator) error {
	_, _ = fmt.Fprintln(humanStderr(), "提示：本命令会改写 channels.credential_enc，开始前请确认已经备份。")

	report, err := rotator.Apply(ctx, rotateProgressPrinter(humanStdout()))
	printRotateReport(humanStdout(), report)
	if report != nil && len(report.Problems) > 0 {
		_, _ = fmt.Fprintf(humanStderr(), "有 %d 行没能轮换，见上面的清单：它们要么密文坏了、要么缺一把主密钥，再跑一次也还是这样。\n",
			len(report.Problems))
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(humanStdout(), "跑 larkaxctl secret rotate --verify 确认全库都补上了，再退役旧主密钥。")
	return nil
}

// runRotateVerify 扫一遍全表并给出结论。
//
// 非零退出码是这条命令存在的意义：调度器与运维盯的是退出码，而「命令跑完了」
// 与「全库每一行都补上了」是两件事（见 LESSONS.md 第 16 条）。
func runRotateVerify(ctx context.Context, rotator *channels.Rotator) error {
	report, err := rotator.Verify(ctx)
	if err != nil {
		return err
	}
	printVerifyReport(humanStdout(), report)
	if !report.OK() {
		return fmt.Errorf("有 %d 行不含当前主密钥 %q：现在还不能退役旧主密钥（见上面的清单）",
			len(report.Missing), report.PrimaryKeyID)
	}
	_, _ = fmt.Fprintf(humanStdout(), "全库每一行都含当前主密钥 %q，可以退役旧主密钥了。\n", report.PrimaryKeyID)
	return nil
}

// printRotateReport 打一次干跑或轮换的结果。
//
// 每一处写入的返回值都显式丢弃（`_, _ =`）：目标是 stdout，写不进去只可能是
// 下游管道断了（`larkaxctl secret rotate | head` 就是最常见的一种），
// 那种情况下重试与报错都没有意义，而它对这一轮的结论也毫无影响。
func printRotateReport(w io.Writer, r *channels.RotateReport) {
	if r == nil {
		return
	}

	what := "已补上"
	if r.DryRun {
		what = "会被补上"
	}
	_, _ = fmt.Fprintf(w, "扫过 %d 行：%s主密钥 %q 的副本 %d 行，本来就含它的 %d 行。\n",
		r.Scanned, what, r.PrimaryKeyID, r.Rewrapped, r.Current)

	if r.RotationRunning {
		_, _ = fmt.Fprintln(w, "注意：扫描时另有一处轮换持着互斥锁，上面的数字可能不是终态。")
	}
	if len(r.Problems) > 0 {
		_, _ = fmt.Fprintf(w, "轮换不了的行 %d 行：\n", len(r.Problems))
		printRotateRows(w, r.Problems)
	}
	if len(r.Concurrent) > 0 {
		_, _ = fmt.Fprintf(w, "轮换期间被别的写入改掉、本轮跳过的行 %d 行（它们不一定还是旧的）：\n", len(r.Concurrent))
		printRotateRows(w, r.Concurrent)
	}

	if r.DryRun {
		_, _ = fmt.Fprintln(w, "现在什么都没改。要真改就加 --apply（先备份 channels 表）。")
	}
}

// printVerifyReport 打一次验证的结果。
func printVerifyReport(w io.Writer, r *channels.VerifyReport) {
	if r == nil {
		return
	}

	_, _ = fmt.Fprintf(w, "扫过 %d 行：含当前主密钥 %q 的 %d 行，不含的 %d 行。\n",
		r.Scanned, r.PrimaryKeyID, r.Wrapped, len(r.Missing))
	if r.RotationRunning {
		_, _ = fmt.Fprintln(w, "注意：扫描时另有一处轮换持着互斥锁，清单里的行可能只是还没轮到。")
	}
	if len(r.Missing) > 0 {
		_, _ = fmt.Fprintln(w, "不含当前主密钥的行：")
		printRotateRows(w, r.Missing)
	}
}

// printRotateRows 逐行打一份清单，超过上限时明确说明还剩多少行没列。
func printRotateRows(w io.Writer, rows []channels.RotateRow) {
	for i, row := range rows {
		if i == rotateRowLimit {
			_, _ = fmt.Fprintf(w, "    ……其余 %d 行未列出\n", len(rows)-rotateRowLimit)
			return
		}
		if len(row.Keys) == 0 {
			// 解析不出标识与「一个标识都没有」是同一件事：这一行的信封读不了。
			_, _ = fmt.Fprintf(w, "    id=%d 主密钥标识解析不出", row.ID)
		} else {
			_, _ = fmt.Fprintf(w, "    id=%d 挂在主密钥 %s 下", row.ID, strings.Join(row.Keys, " "))
		}
		if row.Err != nil {
			_, _ = fmt.Fprintf(w, "：%v", row.Err)
		}
		_, _ = fmt.Fprintln(w)
	}
}

// rotateProgressPrinter 返回一个把每批进度打到 w 的回调。
func rotateProgressPrinter(w io.Writer) func(channels.RotateProgress) {
	return func(p channels.RotateProgress) {
		_, _ = fmt.Fprintf(w, "处理到 id=%d：本批 %d 行（轮换 %d、无需改动 %d、并发改写 %d、失败 %d），累计 %d/%d\n",
			p.LastID, p.Batch, p.Rewrapped, p.Current, p.Concurrent, p.Failed, p.Done, p.Total)
	}
}
