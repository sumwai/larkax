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
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

// ledgerUsage 是 ledger 子命令的帮助。
//
// balance 与 verify 只读这件事写在第一行：这两条命令出现在事故现场（余额对
// 不上、导入之后要复核），而那时候最要紧的一件事是**不要**再动账本。reconcile
// 是这一组里唯一会写库的那条，它写什么写在它自己那一段里。
const ledgerUsage = `larkaxctl ledger 管财务账本：读余额、校验分录，以及跑一轮对账。

用法：
  larkaxctl ledger balance --account <科目> [--asset <资产>]
        查一个科目的余额
  larkaxctl ledger balance --all [--asset <资产>]
        按科目汇总全库，一个科目一行
  larkaxctl ledger verify [--from <时刻>] [--to <时刻>] [--max-txns <笔数>]
        逐笔校验分录（和为零、科目与类型合法、借贷方向与交易类型相符）
  larkaxctl ledger reconcile
        跑一轮对账（开关与口径见 settings 表）

balance 与 verify 只读：不改任何一行、不写任何快照，任何写入都走各自的写入
路径，这里不提供。reconcile 不是只读的：它往 reconcile_runs 记一行，并在
settings 的 reconcile.auto_repair 为真时按口径修差额——要不要动手由那个开关
决定，命令行上不带参数，于是「线上跑的是哪一套口径」只有一个答案（库里的值）。

balance 打出来的余额是从分录**现算**的权威值，不是 user_wallets 里那个快照。
两者是否一致由 reconcile 比对，它还会把差额归集到 reconcile_difference；
balance 只回答「账本上是多少」。

科目按账本的写法给，例如 user:42、merchant_payable:7、payout:3、
payment_gateway:<通道名>、platform_revenue、upstream_cost、reconcile_difference。

资产默认 CNY（目前只有这一种，见 internal/ledger 的 Asset）。

时刻用 RFC3339（2026-10-17T00:00:00Z）或只写日期（2026-10-17，按 UTC 零点算）。
区间左闭右开：--from 含、--to 不含。

verify 的范围按「在这个区间里出现过的交易」圈定，而每一笔交易都会**整笔**
读回来校验：只按行过滤会把横跨区间边界的交易切两半，而半笔可能看起来是平的。

退出码见 larkaxctl --help。verify 发现不合法、或区间里一条分录都没有时是 1；
reconcile 发现不一致时也是 1。

旧路径：larkaxctl reconcile 仍然可用，与 larkaxctl ledger reconcile 逐字相同。
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

// runLedger 处理 ledger 子命令。
func runLedger(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, ledgerUsage)
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
	case "help", "-h", "--help":
		fmt.Print(ledgerUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, ledgerUsage)
		return usageErr(fmt.Errorf("未知的 ledger 子命令 %q", action))
	}
}

// runLedgerBalance 查余额。
//
// 两个模式（一个科目 / 全部科目）互斥且必须给一个，没有默认值：默认成其中
// 一个，会让「我写了 --account」与「我忘了写」在输出上分不出来——而后者会
// 打出一份全库汇总，看起来像一次成功的查询。
func runLedgerBalance(args []string) error {
	fs := flag.NewFlagSet("ledger balance", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, ledgerUsage) }
	account := fs.String("account", "", "要查的科目，例如 user:42")
	all := fs.Bool("all", false, "按科目汇总全库")
	asset := fs.String("asset", string(ledger.AssetCNY), "资产种类")
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
func printLedgerBalances(w io.Writer, asset ledger.Asset, report *store.AccountBalancesReport) {
	byAccount := report.ByAccount[asset]
	if len(byAccount) == 0 {
		// 一行都不打的话，「这种资产上没有分录」与「命令没跑成」看起来一样
		// （LESSONS 第 16 条）。
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
		for _, acct := range accounts {
			_, _ = fmt.Fprintf(w, "  %-28s %s\n", acct, byAccount[acct])
		}
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
	fs := flag.NewFlagSet("ledger verify", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, ledgerUsage) }
	from := fs.String("from", "", "区间起点（含），RFC3339 或 2006-01-02")
	to := fs.String("to", "", "区间终点（不含）")
	maxTxns := fs.Int("max-txns", 0, "最多校验多少笔交易，0 表示不限")
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
		// 三种情况都会走到这里，而它们的输出与「校验完了一切正常」
		// 必须不一样（LESSONS 第 16 条）。
		return errors.New("区间里一条分录都没有，没有可校验的东西：--from / --to 是不是写错了")
	}
	if len(report.Findings) > 0 {
		return fmt.Errorf("发现 %d 笔不合法的交易（见上面的清单）", len(report.Findings))
	}
	return nil
}

// printLedgerVerifyReport 把一次校验的结果打到 w。
//
// runErr 是 Verify 返回的错误，非空表示**没跑完**：报告里只有读到失败点为止
// 的那部分。它不能由报告本身推出来——「校验了 0 笔、没有不合法」既是
// 「读第一笔就失败了」，也是「区间里确实一笔都没有」，后者是跑完的正常结果。
// 两条路径的 stdout 因此不能同形：失败时汇总行点明「没跑完」，并且不出现
// 「全部自洽。」或那句计数之后就收尾、读起来像跑完了的样子。
//
// 传 err 而不是别的做法，理由与 printReconcileReport 相同：塞进
// store.VerifyReport 会让同一个事实有两个来源；失败时不打汇总行又丢掉了
// 失败前已经校验出来的那部分，而它正是「报告先打」要保住的东西。
//
// 每处写入的返回值都显式丢弃（`_, _ =`）：目标是 stdout，写不进去只可能是
// 下游管道断了（`larkaxctl ledger verify | head`），那种情况下重试与报错
// 都没有意义，也不影响这一轮的结论。
func printLedgerVerifyReport(w io.Writer, r *store.VerifyReport, runErr error) {
	if r == nil {
		return
	}
	if runErr != nil {
		// 计数照打（失败前校验过多少是真的），但紧跟一句「没跑完」：
		// 后面还有没读到的交易，这一轮的结论不完整。
		_, _ = fmt.Fprintf(w, "校验 %d 笔交易（%d 条分录），这一轮没跑完：只到失败时为止，后面的交易没有校验（原因见 stderr）。\n",
			r.CheckedTxns, r.CheckedEntries)
		if len(r.Findings) > 0 {
			printLedgerVerifyFindings(w, r)
		}
		return
	}
	// 一笔都没读到时不打「全部自洽」：那正是「跳过与通过长得一样」的形状。
	_, _ = fmt.Fprintf(w, "校验 %d 笔交易（%d 条分录）。\n", r.CheckedTxns, r.CheckedEntries)
	if r.Truncated {
		_, _ = fmt.Fprintf(w, "达到 --max-txns 上限，后面还有没校验的交易。\n")
	}
	if len(r.Findings) == 0 {
		if r.CheckedTxns > 0 {
			_, _ = fmt.Fprintln(w, "全部自洽。")
		}
		return
	}
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
