package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

// ledgerHelp 是 ledger 的两版帮助文本（见 helpText）。
var ledgerHelp = helpText{short: ledgerShortUsage, long: ledgerUsage}

// ledgerShortUsage 是 ledger 的短版：`-h` / `--help` 与参数写错时打的那一份。
const ledgerShortUsage = `larkaxctl ledger 管财务账本：读余额、校验分录，跑一轮对账，以及看对账的历史。

用法：
  larkaxctl ledger balance --account <科目> [--asset <资产>]
  larkaxctl ledger balance --all [--asset <资产>]
  larkaxctl ledger verify [--from <时刻>] [--to <时刻>] [--max-txns <笔数>]
  larkaxctl ledger reconcile
  larkaxctl ledger runs [--limit <条数>]

balance、verify 与 runs 只读；reconcile 会写库，口径来自 settings 表。
balance 打的是从分录现算的权威值，不是 user_wallets 里的那个快照。

更细的边界：larkaxctl help ledger。
`

// ledgerUsage 是 ledger 的长版：`larkaxctl help ledger` 与 `ledger help`。
//
// balance 与 verify 只读这件事写在第一行：这两条命令出现在事故现场（余额对
// 不上、导入之后要复核），而那时候最要紧的一件事是**不要**再动账本。reconcile
// 是这一组里唯一会写库的那条，它写什么写在它自己那一段里。
const ledgerUsage = `larkaxctl ledger 管财务账本：读余额、校验分录，跑一轮对账，以及看对账的历史。

用法：
  larkaxctl ledger balance --account <科目> [--asset <资产>]
        查一个科目的余额
  larkaxctl ledger balance --all [--asset <资产>]
        按科目汇总全库，一个科目一行
  larkaxctl ledger verify [--from <时刻>] [--to <时刻>] [--max-txns <笔数>]
        逐笔校验分录（和为零、科目与类型合法、借贷方向与交易类型相符）
  larkaxctl ledger reconcile
        跑一轮对账（开关与口径见 settings 表）
  larkaxctl ledger runs [--limit <条数>]
        列出对账的历史运行：什么时候跑的、起点在哪、比出多少差额、修了几条

balance、verify 与 runs 只读：不改任何一行、不写任何快照。reconcile 不是只读的：
它往 reconcile_runs 记一行，并在 settings 的 reconcile.auto_repair 为真时按口径修差额。

runs 读的就是 reconcile 写下的那张表，所以两件事对得上：清单里最新那一行就是上一轮。
读它同样是管理员动作——跑得起 larkaxctl 的人本来就能连上这个库，所以这条命令不额外
要求别的授权，也不带角色参数。清单新的在前，--limit 默认 20、0 表示不限；被截住时
末行会说明只打了多少条。

balance 打出来的余额是从分录**现算**的权威值，不是 user_wallets 里那个快照。
两者是否一致由 reconcile 比对，它还会把差额归集到 reconcile_difference；
balance 只回答「账本上是多少」。

科目按账本的写法给，例如 user:42、merchant_payable:7、payout:3、
payment_gateway:<通道名>、platform_revenue、upstream_cost、reconcile_difference。

资产默认 CNY（目前只有这一种）。

时刻用 RFC3339（2026-10-17T00:00:00Z）或只写日期（2026-10-17，按 UTC 零点算）。
区间左闭右开：--from 含、--to 不含。

verify 的范围按「在这个区间里出现过的交易」圈定，而每一笔交易都会**整笔**
读回来校验。

退出码见 larkaxctl --help。verify 发现不合法、或区间里一条分录都没有时是 1；
reconcile 发现不一致时也是 1。

旧路径：larkaxctl reconcile 仍然可用，与 larkaxctl ledger reconcile 逐字相同。

更细的约定见 docs/cli-reference.md 的「ledger」一节。
`

// ledgerReadTimeout 是余额查询的时间上限。
//
// 给一分钟：它读的是一部分分录。读不完通常意味着谓词没走对索引、或者表已经
// 大到该换做法了，两种都不该让命令无限挂下去。
const ledgerReadTimeout = time.Minute

// ledgerVerifyTimeout 是全量校验的时间上限。
//
// 与 reconcile 同一个量级（30 分钟）：它同样要扫全表，而把上限设小会让
// 「没跑完」看起来像「跑完了什么都没发现」（LESSONS 第 16 条）。
const ledgerVerifyTimeout = 30 * time.Minute

// ledgerActions 是 ledger 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var ledgerActions = []string{"balance", "verify", "reconcile", "runs"}

// runLedger 处理 ledger 子命令。
func runLedger(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, ledgerShortUsage)
		return usageErr(errors.New("ledger 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "balance":
		return runLedgerBalance(rest)
	case "verify":
		return runLedgerVerify(rest)
	case "reconcile":
		// 对账原先是一个顶层命令（larkaxctl reconcile），#475 把它收进账本域：
		// 它比的是账本与快照，属于同一个域。旧路径留着，接的是同一个函数。
		return runReconcile(rest)
	case "runs":
		return runLedgerRuns(rest)
	case "-h", "--help":
		printUsage(os.Stdout, ledgerShortUsage)
		return nil
	default:
		printUsage(os.Stderr, ledgerShortUsage)
		return usageErr(fmt.Errorf("未知的 ledger 子命令 %q", action))
	}
}

// runLedgerBalance 查余额。
//
// 两个模式（一个科目 / 全部科目）互斥且必须给一个，没有默认值：默认成其中
// 一个，会让「我写了 --account」与「我忘了写」在输出上分不出来——而后者会
// 打出一份全库汇总，看起来像一次成功的查询。
func runLedgerBalance(args []string) error {
	fs := newFlagSet("ledger balance", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, ledgerShortUsage) }
	account := fs.String("account", "", "")
	all := fs.Bool("all", false, "")
	asset := fs.String("asset", string(ledger.AssetCNY), "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("ledger balance 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	switch {
	case *account == "" && !*all:
		return usageErr(errors.New("ledger balance 需要 --account <科目> 或 --all"))
	case *account != "" && *all:
		return usageErr(errors.New("ledger balance 的 --account 与 --all 只能给一个"))
	}

	assetValue := ledger.Asset(*asset)
	if !assetValue.Valid() {
		return usageErr(fmt.Errorf("资产 %q 不受支持（目前只有 %s）", *asset, ledger.AssetCNY))
	}
	var acct ledger.Account
	if *account != "" {
		parsed, err := ledger.ParseAccount(*account)
		if err != nil {
			return usageErr(fmt.Errorf("科目 %q 不合法: %w", *account, err))
		}
		acct = parsed
	}

	return withDB(ledgerReadTimeout, func(ctx context.Context, db *sql.DB) error {
		s := store.New(db)
		if *all {
			report, err := s.AccountBalances(ctx, assetValue)
			if err != nil {
				return err
			}
			// JSON 是**另一条渲染路径**（见 format.go 开头）：表格那一档把
			// 「没有任何分录」折成一句话，这一档给两个空数组。
			if jsonOutput() {
				return emitJSON(os.Stdout, ledgerBalancesDoc(assetValue, report), nil)
			}
			printLedgerBalances(humanStdout(), assetValue, report)
			return nil
		}
		balance, err := s.AccountBalance(ctx, acct, assetValue)
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, ledgerBalanceDoc(acct, assetValue, balance), nil)
		}
		printLedgerBalance(humanStdout(), acct, assetValue, balance)
		return nil
	})
}

// printLedgerBalance 打一个科目的余额。
func printLedgerBalance(w io.Writer, acct ledger.Account, asset ledger.Asset, balance money.Amount) {
	_, _ = fmt.Fprintf(w, "科目=%s 资产=%s 余额=%s\n", acct, asset, balance)
}

// printLedgerBalances 打按科目汇总的全库余额。
//
// 按**科目字符串**排序，不按金额：map 的遍历顺序是随机的，而同一个库两次
// 跑出来的清单必须逐行相同——否则比对两次输出就变成一件只能靠肉眼做的事。
//
// 科目与余额走固定列宽的表格（与其余清单同一条约定，见 docs/cli-output.md）：
// 科目名长短不一，用 `%-28s` 那样按**字节**补空格的写法在名字里出现非 ASCII
// 字符时会错位，而 tableWriter 补的是显示宽度（见 table.go）。
func printLedgerBalances(w io.Writer, asset ledger.Asset, report *store.AccountBalancesReport) {
	byAccount := report.ByAccount[asset]
	if len(byAccount) == 0 {
		// 一行都不打的话，「这种资产上没有分录」与「命令没跑成」看起来一样
		// （LESSONS 第 16 条）。这一条**不**套清单空状态那一句（style.go 的
		// emptyListNote）：科目不是被「添加」出来的，它们由分录产生，给不出
		// 那句话后半截要的那个命令。
		_, _ = fmt.Fprintf(w, "资产=%s 没有任何分录，汇总为空。\n", asset)
	} else {
		accounts := make([]ledger.Account, 0, len(byAccount))
		for acct := range byAccount {
			accounts = append(accounts, acct)
		}
		sort.Slice(accounts, func(i, j int) bool { return accounts[i].String() < accounts[j].String() })

		// 「随分录数增长」写进输出：这份汇总是一次全表读之后才算得出来的，
		// 而看输出的人（可能正在判断要不要把它接进定时任务）应当知道这件事。
		_, _ = fmt.Fprintf(w, "资产=%s 科目数=%d（全表汇总，随分录数增长）\n", asset, len(accounts))

		p := newPalette(w)
		tw := newTableWriter(w)
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold("科目"), p.bold("余额"))
		for _, acct := range accounts {
			// 科目是这张表的主键，加粗；余额是数据本身，不上样式。
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(acct.String()), byAccount[acct])
		}
		_ = tw.Flush()
	}

	// 少算的那些行必须当场说出来：一份少了几行的汇总与一份完整的汇总
	// 在输出上长得一模一样，而这几行恰恰是账本里最该被看见的东西。
	if len(report.Unreadable) > 0 {
		_, _ = fmt.Fprintf(w, "另有 %d 种科目读不出来，没有计进上面的汇总：%s\n",
			len(report.Unreadable), strings.Join(report.Unreadable, "、"))
		_, _ = fmt.Fprintln(w, "  这些行由对账的畸形科目报告负责，先跑 larkaxctl reconcile。")
	}
}

// runLedgerVerify 校验账本完整性。
func runLedgerVerify(args []string) error {
	fs := newFlagSet("ledger verify", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, ledgerShortUsage) }
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	maxTxns := fs.Int("max-txns", 0, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("ledger verify 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *maxTxns < 0 {
		return usageErr(fmt.Errorf("--max-txns 不能为负，得到 %d", *maxTxns))
	}

	fromAt, err := parseLedgerTime("--from", *from)
	if err != nil {
		return usageErr(err)
	}
	toAt, err := parseLedgerTime("--to", *to)
	if err != nil {
		return usageErr(err)
	}
	if !fromAt.IsZero() && !toAt.IsZero() && !fromAt.Before(toAt) {
		return usageErr(fmt.Errorf("--from 必须早于 --to（区间左闭右开），得到 %s 与 %s",
			fromAt.Format(time.RFC3339), toAt.Format(time.RFC3339)))
	}

	return withDB(ledgerVerifyTimeout, func(ctx context.Context, db *sql.DB) error {
		report, err := store.New(db).Verify(ctx, store.VerifyScope{
			From: fromAt, To: toAt, MaxTxns: *maxTxns,
		})

		// 结论先定出来，再决定往 stdout 打什么。两条渲染路径共用同一个错误：
		// emitJSON 拿它同时定 status 与 exit_code，table 那一档返回的也是它
		// ——两处各判一遍的表现是同一轮校验在两条路径上给出不同的退出码。
		cause := ledgerVerifyCause(report, err)

		if !jsonOutput() {
			// 报告先打，再处理错误：跑了一半的结果比一句「失败了」有用得多
			// ——它至少说明比到了哪里、已经发现了什么（与 reconcile 同一个理由）。
			//
			// err 一并交给打印函数，理由与 reconcile 相同：报告本身分不出
			// 「校验完了一笔都没问题」与「校验到一半读不下去了」。
			printLedgerVerifyReport(humanStdout(), report, err)
			return cause
		}

		// 查询就没跑起来时还没有任何结果（Verify 返回 nil 报告加错误）。这时
		// stdout 上一个字节都不打，错误照旧返回、由 main 打到 stderr：打一份
		// 全零的文档更坏——它读起来正是「校验了 0 笔、一笔都没问题」。
		if report == nil {
			return cause
		}
		return emitJSON(os.Stdout, ledgerVerifyDoc(report, cause), cause)
	})
}

// errLedgerVerifyEmpty 是「一笔都没校验到」那一个判据的错误。
//
// 单独一个变量而不是就地构造：结论那一行与退出码要说的是**同一件事**，
// 而它们分属两个渲染处。抄一份措辞的那天就是两份开始分叉的那天——屏幕上一行
// 写着「区间里一条分录都没有」，退出码却是 0 的那种不一致。
var errLedgerVerifyEmpty = errors.New(
	"区间里一条分录都没有，没有可校验的东西：--from / --to 是不是写错了")

// ledgerVerifyCause 给一次校验定下要返回的那个错误。
//
// 抽成函数是因为它有**两个**调用点（table 与 json 两条渲染路径）。判据在这里
// 只写一遍：抄第二份的那天就是两份开始分叉的那天，而分叉的表现是同一个库在
// 两条路径上给出不同的退出码。
func ledgerVerifyCause(report *store.VerifyReport, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if report == nil || report.CheckedTxns == 0 {
		// 「一条都没校验到」不是成功。范围写错、库是空的、连错了环境，
		// 三种情况都会走到这里，而它们的输出与「校验完了一笔都没问题」
		// 必须不一样（LESSONS 第 16 条）。
		//
		// 标成 reportedErr：结论已经在屏幕那一行状态里打出来了，main 不再
		// 补打一行 `larkaxctl: …`，只留退出码。
		return reportedErr(errLedgerVerifyEmpty)
	}
	if len(report.Findings) > 0 {
		return reportedErr(fmt.Errorf("发现 %d 笔不合法的交易（见上面的清单）", len(report.Findings)))
	}
	return nil
}

// ledgerVerifyLine 把一次校验折成一行状态行。
//
// 状态符的档位与退出码取的是**同一个判据**（ledgerVerifyCause 是不是非空，
// 以及那之后的截断那一档），所以「屏幕上一行 ✓、退出码却是 1」这种自相矛盾的
// 输出不会有。措辞各自写：那一句给脚本看，这一行给人看。
//
// 截断那一档是 `!` 而不是 `✓`：被截断的一轮与跑完的一轮，findings 与两个计数
// 可以逐字相同，差别只在那一个布尔（LESSONS 第 16 条）——所以这一行既不能打
// 「全部自洽」，也不能用通过的那个符号。它退出 0：上限是操作者用 --max-txns
// 要的，不是失败，只是这一轮的结论不完整（与 ledgerVerifyDoc 的 skipped 同）。
//
// 细节（不合法的那几笔与它们的分录）不打进 detail：那是这条命令默认就该看见的
// 内容，不是 -v 才展开的推导过程。它们由 printLedgerVerifyFindings 接着打。
func ledgerVerifyLine(r *store.VerifyReport, runErr error) statusLine {
	counts := fmt.Sprintf("校验 %d 笔交易（%d 条分录）", r.CheckedTxns, r.CheckedEntries)
	line := statusLine{kind: glyphPass, title: "分录校验", conclusion: counts + "，全部自洽"}

	switch {
	case runErr != nil:
		// 计数照打（失败前校验过多少是真的），但明说这一轮没跑完：后面还有
		// 没读到的交易，这一轮的结论不完整。
		line.kind = glyphFail
		line.conclusion = counts + "，这一轮没跑完：只到失败时为止，后面的交易没有校验（原因见 stderr）"
	case ledgerVerifyCause(r, nil) != nil:
		line.kind = glyphFail
		if len(r.Findings) > 0 {
			line.conclusion = fmt.Sprintf("%s，发现 %d 笔不合法的交易（见下面的清单）",
				counts, len(r.Findings))
		} else {
			line.conclusion = counts + "，" + errLedgerVerifyEmpty.Error()
		}
	case r.Truncated:
		line.kind = glyphSkip
		line.conclusion = counts
	}

	// 截断那一句挂在**任何一种**结论后面，而且挂在最后：有 findings 时那一行
	// 整行都换成了失败的说法，而「后面还有没校验的交易」并不会因此变得不成立
	// ——把它并进上面某一支里，另一支就会把它漏掉。
	if r.Truncated {
		line.conclusion += "；达到 --max-txns 上限，后面还有没校验的交易"
	}
	return line
}

// printLedgerVerifyReport 把一次校验的结果打到 w。
//
// 结论那一行由 printStatusLines（style.go）渲染，与其余诊断体检类命令共用
// 同一条路径。
//
// runErr 是 Verify 返回的错误，非空表示**没跑完**：报告里只有读到失败点为止
// 的那部分。它不能由报告本身推出来——「校验了 0 笔、没有不合法」既是
// 「读第一笔就失败了」，也是「区间里确实一笔都没有」，后者是跑完的正常结果。
// 两条路径的 stdout 因此不能同形：失败时那一行点明「没跑完」，并且不出现
// 「全部自洽」——那句只有跑完才成立。
//
// 传 err 而不是别的做法，理由与 printReconcileReport 相同：塞进
// store.VerifyReport 会让同一个事实有两个来源；失败时不打结论又丢掉了
// 失败前已经校验出来的那部分，而它正是「报告先打」要保住的东西。
//
// 每处写入的返回值都显式丢弃（`_, _ =`）：目标是 stdout，写不进去只可能是
// 下游管道断了（`larkaxctl ledger verify | head`），那种情况下重试与报错
// 都没有意义，也不影响这一轮的结论。
func printLedgerVerifyReport(w io.Writer, r *store.VerifyReport, runErr error) {
	if r == nil {
		return
	}
	printStatusLines(w, []statusLine{ledgerVerifyLine(r, runErr)}, false)

	if len(r.Findings) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w)
	printLedgerVerifyFindings(w, r)
}

// printLedgerVerifyFindings 打不合法的那几笔，连同受支持的分录类型。
func printLedgerVerifyFindings(w io.Writer, r *store.VerifyReport) {
	_, _ = fmt.Fprintf(w, "不合法 %d 笔：\n", len(r.Findings))
	for _, f := range r.Findings {
		printLedgerVerifyFinding(w, f)
	}
	// 受支持的分录类型取自 ledger.EntryTypes()，不在这里手抄一份：校验失败时
	// 那句「分录类型 %q 未知」正是拿这份清单判的，抄一份意味着两处迟早不一致，
	// 而不一致的表现是提示里列的类型与实际认的对不上——看提示的人会照着改出
	// 一个仍然报错的值。
	_, _ = fmt.Fprintf(w, "受支持的分录类型：%s。\n", strings.Join(entryTypeNames(), "、"))
}

// printLedgerVerifyFinding 打一笔校验不过的交易。
//
// 分录逐条列出来（带组内下标），而不是只打一句错误：错误里的「第 N 条分录」
// 是**组内**下标，只有把这一组按同一个顺序打出来，那句话才对得上具体是哪一行；
// 而「和不为零」那类错误根本不点名任何一条，整组打出来是唯一的线索。
func printLedgerVerifyFinding(w io.Writer, f store.VerifyFinding) {
	_, _ = fmt.Fprintf(w, "交易 %s：%v\n", f.TxnID, f.Err)
	for i, e := range f.Entries {
		_, _ = fmt.Fprintf(w, "  %d  %-15s %-24s %s %s\n",
			i, e.EntryType, e.Account, e.Amount, e.Asset)
	}
}

// entryTypeNames 把账本认的分录类型列成一串。
func entryTypeNames() []string {
	types := ledger.EntryTypes()
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, string(t))
	}
	return names
}

// ── 机器可读输出 ────────────────────────────────────────────────────────
//
// 下面这几个与上面的 printXxx 是同一份数据的另一条渲染路径（见 format.go
// 开头）。它们排在文件末尾，是为了让表格那一档的几段读起来是连着的。

// ledgerBalanceDoc 组装 ledger balance --account 的机器可读输出。
//
// 不给 summary：这一档要数的东西只有余额本身，再给一个 {total: 1} 只是背景音。
func ledgerBalanceDoc(acct ledger.Account, asset ledger.Asset, balance money.Amount) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["account"] = acct.String()
	doc["asset"] = string(asset)
	// 金额走 jsonAmount（十进制定点字符串）：直接塞 money.Amount 编出来是 `{}`，
	// 而它的字段不导出（见 format.go）。
	doc["balance"] = jsonAmount(balance)
	return doc
}

// ledgerBalancesDoc 组装 ledger balance --all 的机器可读输出。
//
// 排序与 printLedgerBalances 一样按**科目字符串**，不按 map 的遍历顺序：同一个
// 库两次跑出来的 JSON 必须逐字节相同。这一档多半会被接进定时任务，逐字节相同
// 才让 `diff` 两次输出成为一件有意义的事。
func ledgerBalancesDoc(asset ledger.Asset, report *store.AccountBalancesReport) map[string]any {
	byAccount := report.ByAccount[asset]
	accounts := make([]ledger.Account, 0, len(byAccount))
	for acct := range byAccount {
		accounts = append(accounts, acct)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].String() < accounts[j].String() })

	balances := make([]map[string]any, 0, len(accounts))
	for _, acct := range accounts {
		balances = append(balances, map[string]any{
			"account": acct.String(),
			"balance": jsonAmount(byAccount[acct]),
		})
	}

	doc := jsonDoc(jsonOK)
	doc["asset"] = string(asset)
	doc["summary"] = map[string]any{
		"accounts": len(balances),
		// 读不出来的那几个科目单独计数，与表格那一档同一条理由：一份少了几行
		// 的汇总与一份完整的汇总长得一模一样，而这几行恰恰是账本里最该被看见
		// 的东西——钱在库里，却归不到任何科目上。计数在这里，原文在 unreadable。
		"unreadable": len(report.Unreadable),
	}
	doc["accounts"] = balances
	// 空时打 `[]` 而不是 null（见 format.go 的 jsonList）：一栏有、一条都没有，
	// 与「这一栏没有」是两件事。
	doc["unreadable"] = jsonList(report.Unreadable)
	return doc
}

// ledgerVerifyDoc 组装 ledger verify 的机器可读输出。
//
// cause 是这一轮最终要返回的那个错误（见 ledgerVerifyCause），status 由它定：
//
//	ok       校验跑完、一笔问题都没有
//	failed   查出问题，或者根本没跑完
//	skipped  没查出问题，但这一轮**不是**全量：达到 --max-txns 上限，后面还有
//	         没校验的交易
//
// skipped 那一档不是修辞。被截断的一轮与跑完的一轮，findings 与两个计数可以
// 逐字相同，差别只在 truncated 那一个布尔——这正是「没跑成与通过不能落进同一个
// 字符串」那条约定（LESSONS 第 16 条）在机器可读输出里的落点。退出码仍是
// 0：被截断是操作者用 --max-txns 要的，不是失败，只是这一轮的结论不完整。
//
// 前两档优先于第三档：有 findings 时即使也被截断，status 也是 failed。
func ledgerVerifyDoc(report *store.VerifyReport, cause error) map[string]any {
	status := jsonOK
	switch {
	case cause != nil:
		status = jsonFailed
	case report.Truncated:
		status = jsonSkipped
	}

	doc := jsonDoc(status)
	doc["summary"] = map[string]any{
		"checked_txns":    report.CheckedTxns,
		"checked_entries": report.CheckedEntries,
		"truncated":       report.Truncated,
	}

	findings := make([]map[string]any, 0, len(report.Findings))
	for _, f := range report.Findings {
		findings = append(findings, verifyFindingDoc(f))
	}
	doc["findings"] = findings
	// 受支持的分录类型取自 ledger.EntryTypes()（见 entryTypeNames），与表格那
	// 一档同一个来源：校验那句「分录类型 %q 未知」正是拿这份清单判的，这里抄
	// 一份意味着两处迟早不一致，而不一致的表现是提示里列的类型与实际认的对不上
	// ——照着改的人会改出一个仍然报错的值。
	doc["supported_entry_types"] = entryTypeNames()
	return doc
}

// verifyFindingDoc 打一笔校验不过的交易。
//
// entries 逐条列出来（带组内下标），与表格那一档同一个理由：错误里的「第 N 条
// 分录」是**组内**下标，只有把这一组按同一个顺序打出来，那句话才对得上具体是哪
// 一行；而「和不为零」那类错误根本不点名任何一条，整组打出来是唯一的线索。
//
// index 与表格那一栏一样从 0 起：它就是组内下标本身，从这里再减一层只会让两处
// 的数字对不上，而对着错误信息数行数的人正是拿它当坐标的。
func verifyFindingDoc(f store.VerifyFinding) map[string]any {
	entries := make([]map[string]any, 0, len(f.Entries))
	for i, e := range f.Entries {
		entries = append(entries, map[string]any{
			"index":      i,
			"entry_type": string(e.EntryType),
			"account":    e.Account.String(),
			// 金额**保持符号**：账本里借为正、贷为负，这个符号是数据的一部分
			// （见 internal/ledger/entry.go），取绝对值会把方向这件事抹掉。
			"amount": jsonAmount(e.Amount),
			"asset":  string(e.Asset),
		})
	}

	return map[string]any{
		"txn_id": string(f.TxnID),
		// 错误那句话由 internal/ledger 给，原样转述、不在这里改写：改写会让看
		// JSON 的人与看终端的人拿到两种说法，而排查时两边的说法要能对上。
		"error":   nullableError(f.Err),
		"entries": entries,
	}
}

// ledgerRunsDefaultLimit 是 ledger runs 默认打几行。
//
// reconcile_runs 只追加、不清扫，而看这份清单通常是为了回答「上一次扫到哪」——
// 最近那几行。被截住时末行会说明，所以默认值不会让人把一份截过的清单当成完整的。
const ledgerRunsDefaultLimit = 20

// runLedgerRuns 列出对账的历史运行。
//
// 它读的就是 reconcile 写下的那张表（reconcile_runs）。这条命令与 reconcile 是
// 一写一读的两半：只跑不读的话，「跑过几次、上次扫到哪、差额怎么处理的」只能连库看，
// 而那张表同时还是下一轮增量扫描的起点。
func runLedgerRuns(args []string) error {
	fs := newFlagSet("ledger runs", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, ledgerShortUsage) }
	limit := fs.Int("limit", ledgerRunsDefaultLimit, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("ledger runs 不接受位置参数，得到 %q：要少看几行用 --limit", fs.Arg(0)))
	}
	if *limit < 0 {
		return usageErr(fmt.Errorf("--limit 不能为负，得到 %d（0 表示不限）", *limit))
	}

	return withDB(ledgerReadTimeout, func(ctx context.Context, db *sql.DB) error {
		page, err := store.New(db).ReconcileRuns(ctx, *limit)
		if err != nil {
			return err
		}

		if jsonOutput() {
			return emitJSON(os.Stdout, ledgerRunsDoc(page.Runs, page.More, *limit), nil)
		}
		printLedgerRuns(humanStdout(), page.Runs, page.More)
		return nil
	})
}

// printLedgerRuns 打一张对账运行清单。
//
// 「范围」那一栏把 full_scan 翻成全量 / 增量：它是这张清单里最要紧的一栏——增量
// 只看上次起点之后变动过的行，所以一份全是增量的清单说明「两边都早就错着」的那种
// 不一致还从来没被看过一眼（见迁移 00004 里 full_scan_interval_hours 的说明）。
//
// 空清单也说一句话：一行都不打时，「一轮对账都没跑过」与「命令没跑成」在安静的
// 终端里长得一样（见 LESSONS.md 第 16 条）。这一句不套 style.go 的 emptyListNote
// ——运行记录由 reconcile 跑出来，没有「添加」这个动作。
func printLedgerRuns(w io.Writer, runs []store.ReconcileRun, more bool) {
	if len(runs) == 0 {
		_, _ = fmt.Fprintln(w, "还没有跑过对账，一条运行记录都没有。")
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("run_id"), p.bold("开始"), p.bold("状态"), p.bold("范围"), p.bold("起点"),
		p.bold("比对"), p.bold("余额差"), p.bold("冻结差"), p.bold("畸形科目"),
		p.bold("陈旧预留"), p.bold("面值差"), p.bold("低于下界"), p.bold("修复"))
	for _, r := range runs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			p.bold(strconv.FormatInt(r.ID, 10)),
			p.dim(r.StartedAt.Format(longStampLayout)),
			r.Status, scanScopeCell(r.FullScan),
			p.dim(r.Watermark.Format(longStampLayout)),
			r.CheckedWallets, r.BalanceMismatches, r.FrozenMismatches,
			r.MalformedAccounts, r.StaleReservations, r.PlanFaceShortfalls,
			r.AccountBelowFloors, r.Repaired)
	}
	_ = tw.Flush()
	printTruncatedListNote(w, more, len(runs))
}

// scanScopeCell 把 full_scan 翻成「全量 / 增量」。
func scanScopeCell(fullScan bool) string {
	if fullScan {
		return "全量"
	}
	return "增量"
}

// ledgerRunsDoc 组装 ledger runs 的机器可读输出。
//
// 每一行的 status 是**那一轮对账**的状态（running / done / failed），与最外层那个
// status（这份输出本身成不成立）不是一回事。取库里那一列的原文，不在命令行这一侧
// 另翻一套词：对账与运维看的是同一个字符串。
func ledgerRunsDoc(runs []store.ReconcileRun, more bool, limit int) map[string]any {
	items := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		items = append(items, map[string]any{
			"id":         r.ID,
			"started_at": jsonTime(r.StartedAt),
			// finished_at 为空表示这一轮还没收尾（status = running）。
			"finished_at":          jsonTimePtr(nullTimeToPtr(r.FinishedAt)),
			"watermark":            jsonTime(r.Watermark),
			"full_scan":            r.FullScan,
			"checked_wallets":      r.CheckedWallets,
			"balance_mismatches":   r.BalanceMismatches,
			"frozen_mismatches":    r.FrozenMismatches,
			"repaired":             r.Repaired,
			"malformed_accounts":   r.MalformedAccounts,
			"stale_reservations":   r.StaleReservations,
			"plan_face_shortfalls": r.PlanFaceShortfalls,
			"account_below_floors": r.AccountBelowFloors,
			"status":               r.Status,
			"error":                nullableString(r.Error.String),
		})
	}

	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{
		"total":     len(items),
		"limit":     limit,
		"truncated": more,
	}
	doc["runs"] = items
	return doc
}

// parseLedgerTime 解析一个时刻参数，空串表示不限。
//
// 只有日期时按 **UTC** 零点算，不按本地时区：库里存的是 timestamptz，
// 而「今天」在一台时区没设对的机器上会悄悄差出几小时——那种偏差不报错，
// 只让区间少一段，而少掉的那一段恰好是边界上的交易。
func parseLedgerTime(flagName, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at, nil
	}
	if at, err := time.Parse("2006-01-02", value); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf(
		"%s 的取值 %q 既不是 RFC3339（2026-10-17T00:00:00Z）也不是日期（2026-10-17）",
		flagName, value)
}
