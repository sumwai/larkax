package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/config"
)

// rotateHelp 是 secret rotate 的两版帮助文本（见 helpText）。
var rotateHelp = helpText{short: rotateShortUsage, long: rotateUsage}

// rotateShortUsage 是 secret rotate 的短版：`-h` / `--help` 与参数写错时打的那一份。
const rotateShortUsage = `larkaxctl secret rotate 轮换凭证账号的主密钥。

用法：
  larkaxctl secret rotate                     干跑：列出会被改动的行，不写库（默认）
  larkaxctl secret rotate --apply             轮换：逐批补上当前主密钥的数据密钥副本
  larkaxctl secret rotate --verify            验证：每一行是否都含当前主密钥
  larkaxctl secret rotate --batch-size <N>    每批处理的行数（默认 500）

顺序：先把新主密钥放进 LARKAX_MASTER_KEYS 的首位、重启服务，再干跑、--apply、
--verify 各走一遍，**最后**才去掉旧主密钥。跑 --apply 之前先备份 accounts 表。

更细的边界：larkaxctl secret rotate help。
`

// rotateUsage 是 secret rotate 的长版：`secret rotate help`（旧写法
// `rotate help` 也走这一份）。
//
// 它原先是一个顶层命令（larkaxctl rotate）的帮助，#475 把这个动词收进
// secret 域：轮换的是渠道凭证的主密钥，与渠道管理是两件事，所以不挂在
// channel 下面。旧路径留着，见末尾那一行。
const rotateUsage = `larkaxctl secret rotate 轮换凭证账号的主密钥。

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

跑 --apply 之前先备份 accounts 表（至少备份 credential_enc 这一列）：轮换改的
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

更细的约定见 docs/cli-reference.md 的「secret rotate」一节。
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
	if err := cli.JSONUnsupported("secret rotate"); err != nil {
		return err
	}

	// `secret rotate help`（以及旧写法 `rotate help`）给长版：那一份完整的
	// 顺序说明才是这条命令的正文，短版只是一屏摘要。
	if len(args) > 0 && args[0] == "help" {
		cli.PrintUsage(os.Stdout, rotateUsage)
		return nil
	}

	fs := cli.NewFlagSet("secret rotate", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, rotateShortUsage) }
	apply := fs.Bool("apply", false, "")
	verify := fs.Bool("verify", false, "")
	batchSize := fs.Int("batch-size", channels.DefaultRotateBatch, "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf(
			"secret rotate 不接受位置参数，得到 %q：模式用 --apply 或 --verify 指定", fs.Arg(0)))
	}
	if *apply && *verify {
		return cli.UsageErr(errors.New("--apply 与 --verify 不能同时用：一个改写密文，一个只读"))
	}
	if *batchSize <= 0 {
		return cli.UsageErr(fmt.Errorf("--batch-size 必须是正整数，当前为 %d", *batchSize))
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

	ctx, cancel := context.WithTimeout(context.Background(), rotateTimeout)
	defer cancel()
	// 走 openDB 而不是自己 sql.Open + Ping：打开与探活失败时的处置相同
	// （都归 exitDBUnreach），各写一遍的表现是同一类失败在两条命令上给出
	// 不同的退出码，而脚本对它们的处置是不一样的。
	db, err := cli.OpenDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

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
	printRotateReport(cli.HumanStdout(), cli.HumanStderr(), report)
	return nil
}

// runRotateApply 逐批轮换。
//
// # 备份提醒为什么只在 -v 下打
//
// 「先备份」是这条命令唯一一个出了事还能回头的动作，但**每一次 --apply 都会
// 出现**——而一句每次成功运行都在的话，读的人第二遍就不再读它了。真正需要它
// 的时刻是第一次跑，而那时用法文本（rotateUsage 的「跑 --apply 之前先备份
// accounts 表」那一节）正摆在眼前。所以它跟着 -v 走：要动手的人按需多问一句。
func runRotateApply(ctx context.Context, rotator *channels.Rotator) error {
	if cli.VerboseOutput {
		_, _ = fmt.Fprintln(cli.HumanStderr(),
			"提示：本命令会改写 accounts.credential_enc，开始前请确认已经备份。")
	}

	report, err := rotator.Apply(ctx, rotateProgressPrinter(cli.HumanStdout()))
	printRotateReport(cli.HumanStdout(), cli.HumanStderr(), report)
	if report != nil && len(report.Problems) > 0 {
		_, _ = fmt.Fprintf(cli.HumanStderr(), "有 %d 行没能轮换，见上面的清单：它们要么密文坏了、要么缺一把主密钥，再跑一次也还是这样。\n",
			len(report.Problems))
	}
	if err != nil {
		return err
	}
	// 下一步走 stderr：它是附注，不是这一轮的结果——结果在上面那份报告里。
	_, _ = fmt.Fprintln(cli.HumanStderr(), "跑 larkaxctl secret rotate --verify 确认全库都补上了，再退役旧主密钥。")
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
	printVerifyReport(cli.HumanStdout(), cli.HumanStderr(), report)
	return rotateVerifyErr(report)
}

// rotateVerifyErr 把一次验证的结论折成退出码。
//
// 结论已经在 printVerifyReport 打出来的那一行状态与清单里了，所以它标成
// reportedErr：main 不再补打一行 `larkaxctl: …`——那一行与清单说的是同一
// 件事，而退出码照旧非零，给盯它的调度器。
func rotateVerifyErr(report *channels.VerifyReport) error {
	if report == nil || report.OK() {
		return nil
	}
	return cli.ReportedErr(fmt.Errorf("有 %d 行不含当前主密钥 %q：现在还不能退役旧主密钥（见上面的清单）",
		len(report.Missing), report.PrimaryKeyID))
}

// printRotateReport 打一次干跑或轮换的结果。
//
// 每一处写入的返回值都显式丢弃（`_, _ =`）：目标是 stdout，写不进去只可能是
// 下游管道断了（`larkaxctl secret rotate | head` 就是最常见的一种），
// 那种情况下重试与报错都没有意义，而它对这一轮的结论也毫无影响。
//
// out 与 note 分开：清单与数字是**结果**（下游 `| head` 要接的正是它），
// 「另有一处轮换在跑」与「这次什么都没改」是**附注**——只在条件成立时出现，
// 走 stderr（见 docs/cli-output.md 的「提示与警告」）。
func printRotateReport(out, note io.Writer, r *channels.RotateReport) {
	if r == nil {
		return
	}

	what := "已补上"
	if r.DryRun {
		what = "会被补上"
	}
	_, _ = fmt.Fprintf(out, "扫过 %d 行：%s主密钥 %q 的副本 %d 行，本来就含它的 %d 行。\n",
		r.Scanned, what, r.PrimaryKeyID, r.Rewrapped, r.Current)

	if r.RotationRunning {
		_, _ = fmt.Fprintln(note, "注意：扫描时另有一处轮换持着互斥锁，上面的数字可能不是终态。")
	}
	if len(r.Problems) > 0 {
		_, _ = fmt.Fprintf(out, "轮换不了的行 %d 行：\n", len(r.Problems))
		printRotateRows(out, r.Problems)
	}
	if len(r.Concurrent) > 0 {
		_, _ = fmt.Fprintf(out, "轮换期间被别的写入改掉、本轮跳过的行 %d 行（它们不一定还是旧的）：\n", len(r.Concurrent))
		printRotateRows(out, r.Concurrent)
	}

	if r.DryRun {
		// 「这次什么都没改」是干跑的结论，不是排版：一次干跑之后以为已经改过，
		// 与「跑成了」在终端上长得一样。它只在干跑那一档出现，所以是附注。
		_, _ = fmt.Fprintln(note, "现在什么都没改。要真改就加 --apply。")
	}
}

// printVerifyReport 打一次验证的结果。
//
// 结论一行由 printStatusLines（style.go）渲染，与其余诊断体检类命令共用同一条
// 路径。状态符那一档取自 channels.VerifyReport.OK()——与退出码用的是同一个
// 判据（见 runRotateVerify）：两处各判一遍时会出现「屏幕上一行 ✓、退出码却
// 是 1」，而调这条命令的正是盯退出码的调度器。
//
// 「可以退役旧主密钥了」那句原先单独打一行，现在收进结论里：它与 `✓` 说的是
// 同一件事，各打一遍时两句话会随改动漂移开。
//
// note 收的是附注（另一处轮换还在跑），走 stderr；结论与清单走 out。
func printVerifyReport(out, note io.Writer, r *channels.VerifyReport) {
	if r == nil {
		return
	}

	line := cli.StatusLine{Kind: cli.GlyphPass, Title: "主密钥覆盖"}
	if r.OK() {
		line.Conclusion = fmt.Sprintf("扫过 %d 行，每一行都含当前主密钥 %q，可以退役旧主密钥了",
			r.Scanned, r.PrimaryKeyID)
	} else {
		line.Kind = cli.GlyphFail
		line.Conclusion = fmt.Sprintf(
			"扫过 %d 行，含当前主密钥 %q 的 %d 行、不含的 %d 行；现在还不能退役旧主密钥（清单见下）",
			r.Scanned, r.PrimaryKeyID, r.Wrapped, len(r.Missing))
	}
	cli.PrintStatusLines(out, []cli.StatusLine{line}, false)

	if r.RotationRunning {
		// 这一句必须默认就打（只是改走 stderr）：它改的是「上面那个结论怎么读」
		// ——另一处轮换还没轮到那些行时，缺的那几行可能只是暂时的。
		_, _ = fmt.Fprintln(note, "注意：扫描时另有一处轮换持着互斥锁，清单里的行可能只是还没轮到。")
	}
	if len(r.Missing) > 0 {
		_, _ = fmt.Fprintln(out, "不含当前主密钥的行：")
		printRotateRows(out, r.Missing)
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
