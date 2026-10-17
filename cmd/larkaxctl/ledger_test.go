package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

func TestLedgerUnknownSubcommand(t *testing.T) {
	err := runLedger(nil)
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runLedger([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 ledger 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// 参数错在**连库之前**就报出来，所以这些用例不需要库。
//
// 一个写错的命令不该先连上生产库再报错——而「连上库之后才发现参数不对」
// 还有一个更坏的可能：它没报错，而是照着错的参数查了点什么出来。
func TestLedgerBalanceRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"两个模式都没给", nil, "需要 --account"},
		{"两个模式都给了", []string{"--account", "user:42", "--all"}, "只能给一个"},
		{"科目不是规范写法", []string{"--account", "user:042"}, "规范写法"},
		{"科目大类不认识", []string{"--account", "nonsense:1"}, "未知"},
		{"资产不认识", []string{"--all", "--asset", "USDT"}, "不受支持"},
		{"位置参数", []string{"--all", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runLedgerBalance(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, exitUsage)
			}
		})
	}
}

func TestLedgerVerifyRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"时刻写法不认识", []string{"--from", "昨天"}, "既不是 RFC3339"},
		{"起点晚于终点", []string{"--from", "2026-10-17T00:00:00Z", "--to", "2026-10-16T00:00:00Z"},
			"必须早于"},
		{"起点等于终点", []string{"--from", "2026-10-17", "--to", "2026-10-17"}, "必须早于"},
		{"上限为负", []string{"--max-txns", "-1"}, "不能为负"},
		{"位置参数", []string{"extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runLedgerVerify(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, exitUsage)
			}
		})
	}
}

func TestPrintLedgerBalance(t *testing.T) {
	var buf bytes.Buffer
	printLedgerBalance(&buf, ledger.UserAccount(42), ledger.AssetCNY, money.MustParse("80"))
	want := "科目=user:42 资产=CNY 余额=80.00000000\n"
	if got := buf.String(); got != want {
		t.Errorf("输出 = %q，期望 %q", got, want)
	}
}

// 汇总清单按科目排序，且读不出来的那些行要**当场说出来**。
//
// 排序不是排版偏好：map 的顺序是随机的，同一个库两次跑出来的清单必须逐行
// 相同，否则比对两次输出只能靠肉眼。少算的行同理——一份少了几行的汇总与
// 一份完整的汇总长得一模一样，而那几行恰恰是最该被看见的。
func TestPrintLedgerBalances(t *testing.T) {
	report := &store.AccountBalancesReport{
		ByAccount: map[ledger.Asset]map[ledger.Account]money.Amount{
			ledger.AssetCNY: {
				ledger.UserAccount(7):           money.MustParse("12"),
				ledger.PlatformRevenueAccount(): money.MustParse("-30"),
				ledger.UpstreamCostAccount():    money.MustParse("4"),
				ledger.PayoutAccount(3):         money.MustParse("-1"),
			},
		},
		Unreadable: []string{"nonsense:1"},
	}

	var buf bytes.Buffer
	printLedgerBalances(&buf, ledger.AssetCNY, report)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// 抬头那一行 + 表头 + 四个科目 + 读不出来的那一行 + 指路的那一行。
	if len(lines) != 8 {
		t.Fatalf("打了 %d 行，期望 8 行：\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "科目数=4") || !strings.Contains(lines[0], "随分录数增长") {
		t.Errorf("抬头没有说明这是什么、以及它随什么增长：%q", lines[0])
	}
	// 这是一张表：表头必须有自己的名字，只有值的那一栏读的人得回去看代码。
	// 两栏之间的空格数取决于最宽的科目名（tableWriter 按显示宽度补齐），
	// 所以比的是「切出来的两栏是不是那两个列名」。
	if got := strings.Fields(lines[1]); len(got) != 2 || got[0] != "科目" || got[1] != "余额" {
		t.Errorf("表头应当是「科目 余额」两栏，实际为 %q", lines[1])
	}
	want := []string{"payout:3", "platform_revenue", "upstream_cost", "user:7"}
	for i, name := range want {
		// 科目与余额在同一行、之间按显示宽度对齐（两列以上的空格）。
		if !strings.HasPrefix(lines[2+i], name+"  ") {
			t.Errorf("第 %d 行应当是 %s 开头：%q", i+2, name, lines[2+i])
		}
	}
	if !strings.Contains(lines[6], "nonsense:1") || !strings.Contains(lines[6], "没有计进") {
		t.Errorf("读不出来的科目没有被说出来：%q", lines[6])
	}

	// 连着打二十次必须逐行相同。比一次是不够的：map 的遍历顺序随机，
	// 一个没排序的实现有 1/4! 的机会恰好也是这个顺序，一次比对放过它。
	// 二十次里每次都恰好排对，概率小到可以当它不存在。
	for i := 0; i < 20; i++ {
		var again bytes.Buffer
		printLedgerBalances(&again, ledger.AssetCNY, report)
		if again.String() != buf.String() {
			t.Fatalf("第 %d 次打的清单与第一次不一样：\n%s\n---\n%s",
				i+2, buf.String(), again.String())
		}
	}
}

// 汇总为空时也要有一行明确的输出：「这种资产上没有分录」与「命令没跑成」
// 在安静的终端里长得一样（LESSONS 第 16 条）。
func TestPrintLedgerBalancesEmpty(t *testing.T) {
	var buf bytes.Buffer
	printLedgerBalances(&buf, ledger.AssetCNY, &store.AccountBalancesReport{})
	if !strings.Contains(buf.String(), "没有任何分录") {
		t.Errorf("空汇总的输出应当说明是空的，实际为 %q", buf.String())
	}
}

// 校验报告要同时说清「校验了多少」「有没有截断」「哪一笔的哪一条不合法」。
func TestPrintLedgerVerifyReport(t *testing.T) {
	txn := ledger.NewTxnID()
	entries := []ledger.Entry{
		{
			TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
			Amount: money.MustParse("30"), EntryType: ledger.EntryAdjustment,
		},
		{
			TxnID: txn, Account: ledger.UpstreamCostAccount(), Asset: ledger.AssetCNY,
			Amount: money.Zero, EntryType: ledger.EntryAdjustment,
		},
	}

	var bad bytes.Buffer
	printLedgerVerifyReport(&bad, &store.VerifyReport{
		CheckedTxns: 2, CheckedEntries: 5, Truncated: true,
		Findings: []store.VerifyFinding{{
			TxnID: txn, Entries: entries, Err: errors.New("第 1 条分录: 科目 upstream_cost 的金额为零"),
		}},
	}, nil)
	out := bad.String()
	for _, want := range []string{
		"校验 2 笔交易（5 条分录）",
		"达到 --max-txns 上限",
		"不合法 1 笔",
		string(txn),
		"第 1 条分录",
		"受支持的分录类型：topup",
		"adjustment",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
	// 出问题那一笔的分录要逐条列出来：错误里的「第 N 条」是组内下标，
	// 不把这一组按同一个顺序打出来，那句话对不上具体是哪一行。
	if !strings.Contains(out, "upstream_cost") {
		t.Errorf("没有把出问题那一笔的分录列出来：\n%s", out)
	}

	var clean bytes.Buffer
	printLedgerVerifyReport(&clean, &store.VerifyReport{CheckedTxns: 3, CheckedEntries: 8}, nil)
	if !strings.Contains(clean.String(), "全部自洽") {
		t.Errorf("干净的一轮应当明说全部自洽：%q", clean.String())
	}
	if strings.Contains(clean.String(), "不合法") {
		t.Errorf("干净的一轮不该出现「不合法」：%q", clean.String())
	}
	if strings.Contains(clean.String(), "受支持的分录类型") {
		t.Errorf("没有问题时不该列类型清单：%q", clean.String())
	}

	// 一笔都没校验到时**不能**说「全部自洽」——那正是「跳过与通过长得一样」。
	var none bytes.Buffer
	printLedgerVerifyReport(&none, &store.VerifyReport{}, nil)
	if strings.Contains(none.String(), "全部自洽") {
		t.Errorf("一笔都没校验到时不该说全部自洽：%q", none.String())
	}
	if !strings.Contains(none.String(), "0 笔") {
		t.Errorf("一笔都没校验到时要明说：%q", none.String())
	}
}

// ── 以下要连库 ──────────────────────────────────────────────────────────

// ledgerSlot 给这次用例一个独占的时刻，以及一个只覆盖它的区间。
//
// 集成测试的库是**所有包共用**的（CI 里一个 job 一个库），账本里因此会有
// 别的包留下的分录，包括它们故意造的坏账。按时间窗圈住自己插进去的那几条，
// 断言才不会被别人的数据左右。窗口取 [at, at+1s)，相邻两次调用相隔 2 秒，
// 于是两两不相交；时刻落在整秒上，命令行上写成 RFC3339 也不丢精度。
func ledgerSlot() (at, from, to time.Time) {
	seq.Lock()
	seq.n++
	n := seq.n
	seq.Unlock()
	at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration((base%100000)+2*n) * time.Second)
	return at, at, at.Add(time.Second)
}

// insertLedgerEntries 写一组分录，created_at 由调用方给（见 ledgerSlot）。
func insertLedgerEntries(t *testing.T, db *sql.DB, at time.Time, entries []ledger.Entry) {
	t.Helper()
	for _, e := range entries {
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type, created_at)
			VALUES ($1, $2, $3, $4::numeric, $5, $6)`,
			string(e.TxnID), e.Account.String(), string(e.Asset),
			e.Amount.String(), string(e.EntryType), at); err != nil {
			t.Fatalf("写分录失败: %v", err)
		}
	}
}

// adjustment 造一条人工调账分录：科目与方向都不设限，于是「平不平」只由
// 金额决定，用例的意图不会被方向的约定盖住。
func adjustment(t *testing.T, txn ledger.TxnID, account ledger.Account, amount string) ledger.Entry {
	t.Helper()
	return ledger.Entry{
		TxnID: txn, Account: account, Asset: ledger.AssetCNY,
		Amount: money.MustParse(amount), EntryType: ledger.EntryAdjustment,
	}
}

// balance 查一个科目：命令的输出与退出码一起断言。
func runBalanceCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out, _ := captureStreams(t, func() { err = runLedgerBalance(args) })
	return out, err
}

// 余额超出 money 的表示范围时，命令要报错，而不是打印一个偏小的数。
//
// 这是这条命令撞到过的现场：库里两笔的和是 92233720513.54775808，而命令
// 打印 92233720368.54775807（夹到 MaxInt64）并退出 0——比真实和小 145 元，
// 输出里没有任何迹象。一个会静默变小的余额比一个报错的余额坏得多：
// 看输出的人会拿它去核对，而那个数是错的。
func TestLedgerBalanceReportsOverflowInsteadOfClamping(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	account := ledgerAccount()
	txn := ledger.NewTxnID()
	insertLedgerEntries(t, db, time.Now(), []ledger.Entry{
		adjustment(t, txn, account, "92233720368.54775807"),
		adjustment(t, txn, account, "145.00000001"),
	})
	// 用完必须删掉：本包的库是全包共用的，留着这两笔会让后面每一个扫全表的
	// 用例（`--all` 汇总、对账）都撞上这个溢出。
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM ledger_entries WHERE txn_id = $1`, string(txn)); err != nil {
			t.Fatalf("清理用例的分录失败: %v", err)
		}
	})

	// 单科目。
	out, err := runBalanceCmd(t, "--account", account.String())
	if err == nil {
		t.Fatalf("余额算不出来时命令应当失败，实际输出 %q", out)
	}
	if code := exitCode(err); code != exitFailure {
		t.Errorf("退出码 = %d，期望 %d（其他失败）", code, exitFailure)
	}
	// 关键的一条：不许把被夹住的那个数打出去。打出去了就是一次静默的错账，
	// 而退出码再对也救不回来。
	if strings.Contains(out, "92233720368.54775807") {
		t.Errorf("不该打印被夹住的余额：%q", out)
	}
	if !strings.Contains(err.Error(), account.String()) {
		t.Errorf("报错里应当指明是哪个科目：%v", err)
	}

	// 全库汇总（`--all`）同样要报错，而不是少算一个科目继续打。
	out, err = runBalanceCmd(t, "--all")
	if err == nil {
		t.Fatalf("汇总里有科目算不出来时命令应当失败，实际输出 %q", out)
	}
	if !strings.Contains(err.Error(), account.String()) {
		t.Errorf("报错里应当指明是哪个科目：%v", err)
	}
}

// 余额读的是**账本**，不是快照。
//
// 这个用例故意只加分录、不改 user_wallets，于是账本（30）与快照（不存在）
// 不一致——打出来的必须是账本那一侧的数。
func TestLedgerBalanceReadsTheLedger(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	account := ledgerAccount()
	txn := ledger.NewTxnID()
	insertLedgerEntries(t, db, time.Now(), []ledger.Entry{
		adjustment(t, txn, account, "50"),
		adjustment(t, ledger.NewTxnID(), ledger.PlatformRevenueAccount(), "-20"),
		adjustment(t, txn, account, "-20"),
	})

	out, err := runBalanceCmd(t, "--account", account.String())
	if err != nil {
		t.Fatalf("查余额失败: %v", err)
	}
	if want := "科目=" + account.String() + " 资产=CNY 余额=30.00000000\n"; out != want {
		t.Errorf("输出 = %q，期望 %q", out, want)
	}
	if code := exitCode(err); code != exitOK {
		t.Errorf("退出码 = %d，期望 %d", code, exitOK)
	}
}

// balanceAllLines 把 `--all` 的输出拆成「按输出顺序的科目名」与「科目 → 余额」。
//
// 金额那一栏解析不出十进制数就不是数据行——表头（`资产=…`）与末尾那句
// 「这些行由对账的畸形科目报告负责」都带前导空格，靠列宽切会误收。
func balanceAllLines(out string) ([]string, map[string]string) {
	var names []string
	byName := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		// 表格那一档的一行是「科目 + 两列以上空格 + 余额」（见 table.go），
		// 所以按空白切正好两栏；抬头（「资产=CNY 科目数=4…」）与读不出来的
		// 那一句都不是两栏，自然被跳过。
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] == "科目" {
			continue
		}
		name, value := fields[0], fields[1]
		if _, err := money.Parse(value); err != nil {
			continue
		}
		names = append(names, name)
		byName[name] = value
	}
	return names, byName
}

// 全库汇总要能查到别的科目，且每一次跑出来的清单都按科目排序。
//
// **这条用例不比对「两次输出逐行相同」。** 这个包与别的包共用同一个库，
// `go test -race ./...` 会让它们同时跑，别处随时会写进新的分录。原来那条
// 断言实测在 CI 上挂过：第二次汇总多出一行 `user:7`，`platform_revenue`
// 也跟着变了——它测的是「有没有别的进程在写这个库」，不是这个命令的行为。
//
// 真正要钉住的是「排序不依赖 map 的遍历顺序」，而它在并发写入下是稳定的：
// **每一份快照内部都必须是升序**，多几行少几行都不影响。跑 8 次——真没排序时，
// 6 个科目一次跑出升序的概率约 1/720，8 次全中约 10⁻²³，所以「通过」不是撞运气。
func TestLedgerBalanceAllIsStable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	account := ledgerAccount()
	txn := ledger.NewTxnID()
	insertLedgerEntries(t, db, time.Now(), []ledger.Entry{
		adjustment(t, txn, account, "7"),
		adjustment(t, ledger.NewTxnID(), ledger.PlatformRevenueAccount(), "-7"),
	})

	const runs = 8
	for i := 1; i <= runs; i++ {
		out, err := runBalanceCmd(t, "--all")
		if err != nil {
			t.Fatalf("第 %d 次汇总失败: %v", i, err)
		}
		names, byName := balanceAllLines(out)

		if !sort.StringsAreSorted(names) {
			t.Fatalf("第 %d 次汇总没有按科目排序：\n%s", i, out)
		}
		if !slices.Contains(names, account.String()) {
			t.Fatalf("第 %d 次汇总里没有这个科目：\n%s", i, out)
		}
		if got := byName[account.String()]; got != "7.00000000" {
			t.Fatalf("第 %d 次里 %s 的余额 = %q，期望 7.00000000", i, account, got)
		}
		if _, ok := byName[ledger.PlatformRevenueAccount().String()]; !ok {
			t.Fatalf("第 %d 次汇总里没有平台科目：\n%s", i, out)
		}
	}
}

// 自洽的区间校验下来退出码为 0。
func TestLedgerVerifyCleanRangeExitsZero(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	at, from, to := ledgerSlot()
	txn := ledger.NewTxnID()
	insertLedgerEntries(t, db, at, []ledger.Entry{
		adjustment(t, txn, ledger.PlatformRevenueAccount(), "5"),
		adjustment(t, txn, ledger.UpstreamCostAccount(), "-5"),
	})

	var err error
	out, _ := captureStreams(t, func() {
		err = runLedgerVerify([]string{"--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339)})
	})
	if err != nil {
		t.Fatalf("自洽的区间不该失败: %v", err)
	}
	if code := exitCode(err); code != exitOK {
		t.Errorf("退出码 = %d，期望 %d", code, exitOK)
	}
	for _, want := range []string{"校验 1 笔交易（2 条分录）", "全部自洽"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
}

// 区间里混进一条不合法的分录时，要指到具体是哪一笔的哪一条，退出码非零。
func TestLedgerVerifyFindsBadEntryAndExitsNonZero(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	at, from, to := ledgerSlot()
	good := ledger.NewTxnID()
	bad := ledger.NewTxnID()
	insertLedgerEntries(t, db, at, []ledger.Entry{
		adjustment(t, good, ledger.PlatformRevenueAccount(), "5"),
		adjustment(t, good, ledger.UpstreamCostAccount(), "-5"),
		// 和仍然为零，所以「不平」那条判据看不见它——这正是要用单条分录的
		// 判据去抓的形状。
		adjustment(t, bad, ledger.PlatformRevenueAccount(), "30"),
		adjustment(t, bad, ledger.UpstreamCostAccount(), "0"),
		adjustment(t, bad, ledger.UpstreamCostAccount(), "-30"),
	})

	var err error
	out, _ := captureStreams(t, func() {
		err = runLedgerVerify([]string{"--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339)})
	})
	if err == nil {
		t.Fatalf("发现不合法的分录时应当以非零退出码收尾，输出：\n%s", out)
	}
	if code := exitCode(err); code != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", code, exitFailure)
	}
	for _, want := range []string{
		"校验 2 笔交易（5 条分录）",
		"不合法 1 笔",
		string(bad),
		"第 1 条分录",
		"upstream_cost",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
	// 那一笔合法的（good）不该被带出来。
	if strings.Contains(out, string(good)) {
		t.Errorf("合法的那一笔被报出来了：\n%s", out)
	}
}

// 校验到一半读不下去时，stdout 不能说「全部自洽。」。
//
// 这是 issue 里复现的那条路径：所有交易都读不出来时 CheckedTxns == 0，
// 改动前那一行「校验 0 笔交易（0 条分录）。」与「区间里确实一笔都没有」
// 逐字相同，而它后面还跟着一条错误——读日志的人只会看见前面那句。
func TestLedgerVerifyMidwayFailureIsNotClean(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	at, from, to := ledgerSlot()
	// 金额超出 money 的表示范围：读回来解析不了，Verify 就此停下。
	// 直接写原始字符串——money.Amount 装不下这个数，用例要的正是库里
	// 存在一个读不回来的值。
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type, created_at)
		VALUES ($1, $2, 'CNY', $3::numeric, 'adjustment', $4)`,
		string(ledger.NewTxnID()), ledger.PlatformRevenueAccount().String(),
		"184467440736.00000000", at); err != nil {
		t.Fatalf("写溢出金额的分录失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM ledger_entries WHERE created_at >= $1 AND created_at < $2`, from, to); err != nil {
			t.Fatalf("清理用例的分录失败: %v", err)
		}
	})

	var err error
	out, _ := captureStreams(t, func() {
		err = runLedgerVerify([]string{"--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339)})
	})
	if err == nil {
		t.Fatalf("读不下去时应当以非零退出码收尾，输出：\n%s", out)
	}
	if code := exitCode(err); code != exitFailure {
		t.Errorf("退出码 = %d，期望 %d", code, exitFailure)
	}
	if strings.Contains(out, "全部自洽") {
		t.Errorf("没跑完的一轮不该说全部自洽：\n%s", out)
	}
	if !strings.Contains(out, "没跑完") {
		t.Errorf("没跑完的一轮要在 stdout 上说清楚：\n%s", out)
	}
}

// 区间里一条分录都没有时，退出码非零。
//
// 「一条都没校验到」与「校验完了一切正常」必须长得不一样：范围写错、连错了
// 环境、库是空的，三种情况都会走到这里，而它们都不是「账本没问题」。
func TestLedgerVerifyEmptyRangeIsNotSuccess(t *testing.T) {
	testDB(t)
	setupEnv(t)

	_, from, to := ledgerSlot()
	// 用一个**没有行**的窗口：这个窗口没人用过，所以它必然是空的。
	var err error
	out, _ := captureStreams(t, func() {
		err = runLedgerVerify([]string{"--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339)})
	})
	if err == nil {
		t.Fatalf("一条分录都没校验到时应当以非零退出码收尾，输出：\n%s", out)
	}
	if !strings.Contains(err.Error(), "没有可校验的东西") {
		t.Errorf("错误信息 %q 没说清是一条都没校验到", err.Error())
	}
	if strings.Contains(out, "全部自洽") {
		t.Errorf("一条都没校验到时不该说全部自洽：\n%s", out)
	}
}

// ledgerAccount 给这次用例一个独占的用户科目。
//
// 每次都换一个 id：复用同一个会让第二次跑（同一个库）看到上一次留下的分录，
// 于是「余额 = 30」这种断言变成了「跑了第几次」的函数。用户 id 从 10 亿起，
// 免得撞上别的用例建的真用户。
func ledgerAccount() ledger.Account {
	seq.Lock()
	seq.n++
	n := seq.n
	seq.Unlock()
	return ledger.UserAccount(1_000_000_000 + base%1_000_000_000 + n)
}

// ── 机器可读输出（--format json） ───────────────────────────────────────
//
// 这几条钉住的是「脚本读得到什么」：金额是十进制定点字符串、空清单是 `[]`、
// 汇总按科目排序（同一个库两次跑出来必须逐字节相同），以及一轮校验的结论落在
// status 的哪一档上。

func TestLedgerBalanceDocIsMachineReadable(t *testing.T) {
	doc := ledgerBalanceDoc(ledger.UserAccount(42), ledger.AssetCNY, money.MustParse("30"))

	var got struct {
		Status  string `json:"status"`
		Account string `json:"account"`
		Asset   string `json:"asset"`
		// 解进 string 能成功这件事本身就是判据：金额编成 JSON 数字时这里会失败。
		Balance string `json:"balance"`
	}
	raw := decodeDoc(t, doc, &got)

	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q", got.Status, jsonOK)
	}
	if got.Account != "user:42" || got.Asset != "CNY" {
		t.Errorf("account=%q asset=%q，想要 user:42 / CNY", got.Account, got.Asset)
	}
	if got.Balance != "30.00000000" {
		t.Errorf("balance = %q，想要 30.00000000", got.Balance)
	}
	// 表格那一档那句「科目=… 余额=…」不进这里。
	if strings.Contains(string(raw), "科目=") {
		t.Errorf("表格那一档的输出混进了 JSON：\n%s", raw)
	}

	var generic map[string]any
	decodeDoc(t, doc, &generic)
	// 这一档要数的东西只有余额本身，所以不打 summary。
	assertDocKeys(t, generic, []string{"status", "account", "asset", "balance"})
}

// 汇总按**科目字符串**排序，且两次跑出来逐字节相同；读不出来的那些行单独给出。
func TestLedgerBalancesDocIsStableAndMachineReadable(t *testing.T) {
	report := &store.AccountBalancesReport{
		ByAccount: map[ledger.Asset]map[ledger.Account]money.Amount{
			ledger.AssetCNY: {
				ledger.UserAccount(7):           money.MustParse("12"),
				ledger.PlatformRevenueAccount(): money.MustParse("-30"),
				ledger.UpstreamCostAccount():    money.MustParse("4"),
			},
		},
		Unreadable: []string{"nonsense:1"},
	}

	// 编 8 次逐字节比对：不排序时三个科目一次恰好跑成升序的概率是 1/6，
	// 8 次全中约 10⁻⁶，所以「通过」不是撞运气（与 --all 那条用例同一个理由）。
	var first string
	for i := 0; i < 8; i++ {
		var generic map[string]any
		raw := string(decodeDoc(t, ledgerBalancesDoc(ledger.AssetCNY, report), &generic))
		if i == 0 {
			first = raw
			continue
		}
		if raw != first {
			t.Fatalf("第 %d 次编出来的与第一次不同：\n%s\n---\n%s", i+1, first, raw)
		}
	}

	var got struct {
		Status  string `json:"status"`
		Asset   string `json:"asset"`
		Summary struct {
			Accounts   int `json:"accounts"`
			Unreadable int `json:"unreadable"`
		} `json:"summary"`
		Accounts []struct {
			Account string `json:"account"`
			Balance string `json:"balance"`
		} `json:"accounts"`
		Unreadable []string `json:"unreadable"`
	}
	decodeDoc(t, ledgerBalancesDoc(ledger.AssetCNY, report), &got)

	if got.Status != jsonOK || got.Asset != "CNY" {
		t.Errorf("status=%q asset=%q，想要 %q / CNY", got.Status, got.Asset, jsonOK)
	}
	if got.Summary.Accounts != 3 || len(got.Accounts) != 3 {
		t.Fatalf("summary.accounts = %d、accounts 有 %d 条，都想要 3",
			got.Summary.Accounts, len(got.Accounts))
	}
	// 顺序必须是科目字符串的升序——不是 map 的遍历顺序。
	wantOrder := []string{"platform_revenue", "upstream_cost", "user:7"}
	for i, want := range wantOrder {
		if got.Accounts[i].Account != want {
			t.Fatalf("第 %d 条是 %q，期望按科目字符串升序排（%v）",
				i, got.Accounts[i].Account, wantOrder)
		}
	}
	if got.Accounts[0].Balance != "-30.00000000" {
		t.Errorf("platform_revenue 的余额 = %q，想要 -30.00000000", got.Accounts[0].Balance)
	}
	// 少算的那些行必须当场说出来：一份少了几行的汇总与一份完整的汇总长得
	// 一模一样，而这几行恰恰是账本里最该被看见的东西。
	if got.Summary.Unreadable != 1 || len(got.Unreadable) != 1 || got.Unreadable[0] != "nonsense:1" {
		t.Errorf("读不出来的行没有如实带出来：summary=%+v unreadable=%v",
			got.Summary, got.Unreadable)
	}
}

// 空汇总：两个数组都在，且都是 `[]`。
func TestLedgerBalancesDocEmptyIsEmptyArrays(t *testing.T) {
	var got struct {
		Accounts   *[]map[string]any `json:"accounts"`
		Unreadable *[]string         `json:"unreadable"`
	}
	decodeDoc(t, ledgerBalancesDoc(ledger.AssetCNY, &store.AccountBalancesReport{}), &got)

	if got.Accounts == nil || len(*got.Accounts) != 0 {
		t.Errorf("空汇总的 accounts 应当是 []，得到 %v", got.Accounts)
	}
	if got.Unreadable == nil || len(*got.Unreadable) != 0 {
		t.Errorf("空汇总的 unreadable 应当是 []，得到 %v", got.Unreadable)
	}
}

// ledgerVerifyDoc 的三档结论逐个钉住。
//
// 「被截断」那一档是这一份输出最容易被写成 ok 的地方：被截断的一轮与跑完的
// 一轮，findings 与两个计数可以逐字相同，差别只在 truncated 那一个布尔。而
// 「没跑成」与「通过」落进同一个字符串，正是这个项目反复踩的坑
// （见 LESSONS.md 第 16 条）——机器读的输出更不能让步。
func TestLedgerVerifyDocStatuses(t *testing.T) {
	txn := ledger.NewTxnID()
	findings := []store.VerifyFinding{{
		TxnID: txn,
		Entries: []ledger.Entry{{
			TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
			Amount: money.MustParse("30"), EntryType: ledger.EntryAdjustment,
		}},
		Err: errors.New("第 0 条分录: 科目 platform_revenue 的金额为零"),
	}}
	badErr := errors.New("发现 1 笔不合法的交易（见上面的清单）")

	cases := []struct {
		name   string
		report *store.VerifyReport
		cause  error
		want   string
	}{
		{"跑完且自洽", &store.VerifyReport{CheckedTxns: 3, CheckedEntries: 6}, nil, jsonOK},
		{"查出问题", &store.VerifyReport{
			CheckedTxns: 3, CheckedEntries: 6, Findings: findings}, badErr, jsonFailed},
		{"读了一半读不下去", &store.VerifyReport{CheckedTxns: 1, CheckedEntries: 2},
			errors.New("读分录失败"), jsonFailed},
		{"被 --max-txns 截断", &store.VerifyReport{
			CheckedTxns: 1, CheckedEntries: 2, Truncated: true}, nil, jsonSkipped},
		{"既截断又查出问题", &store.VerifyReport{
			CheckedTxns: 1, Truncated: true, Findings: findings}, badErr, jsonFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Status  string `json:"status"`
				Summary struct {
					CheckedTxns    int  `json:"checked_txns"`
					CheckedEntries int  `json:"checked_entries"`
					Truncated      bool `json:"truncated"`
				} `json:"summary"`
				Findings            []map[string]any `json:"findings"`
				SupportedEntryTypes []string         `json:"supported_entry_types"`
			}
			decodeDoc(t, ledgerVerifyDoc(tc.report, tc.cause), &got)

			if got.Status != tc.want {
				t.Errorf("status = %q，想要 %q", got.Status, tc.want)
			}
			if got.Summary.CheckedTxns != tc.report.CheckedTxns ||
				got.Summary.CheckedEntries != tc.report.CheckedEntries {
				t.Errorf("summary 的计数对不上：%+v", got.Summary)
			}
			// truncated 是**布尔**，不是 "true" 字符串：解进 bool 成功即是判据。
			if got.Summary.Truncated != tc.report.Truncated {
				t.Errorf("truncated = %v，想要 %v", got.Summary.Truncated, tc.report.Truncated)
			}
			if got.Findings == nil {
				t.Error("findings 应当是数组（空时是 []），不是 null")
			}
			if len(got.Findings) != len(tc.report.Findings) {
				t.Errorf("findings 有 %d 条，想要 %d", len(got.Findings), len(tc.report.Findings))
			}
			// 受支持的分录类型取自 ledger.EntryTypes()：写死一份的话，校验那句
			// 「分录类型 %q 未知」与实际认的会慢慢对不上。
			for _, want := range []string{string(ledger.EntryAdjustment), string(ledger.EntryTopup)} {
				if !slices.Contains(got.SupportedEntryTypes, want) {
					t.Errorf("supported_entry_types 里没有 %q：%v", want, got.SupportedEntryTypes)
				}
			}
		})
	}
}

// 一笔不合法都没查出来时，findings 是 `[]` 而不是 null，也不是一句「全部自洽。」。
func TestLedgerVerifyDocEmptyFindingsIsEmptyArray(t *testing.T) {
	var got struct {
		Findings *[]map[string]any `json:"findings"`
	}
	decodeDoc(t, ledgerVerifyDoc(&store.VerifyReport{CheckedTxns: 1, CheckedEntries: 2}, nil), &got)

	if got.Findings == nil {
		t.Fatal("空 findings 编成了 null，要的是 []")
	}
	if len(*got.Findings) != 0 {
		t.Errorf("空 findings 里有 %d 条", len(*got.Findings))
	}
}

// 一笔不合法的那几条要能逐条读出来：错误里的「第 N 条分录」是组内下标，
// 分录清单是把它对回具体那一行的唯一线索。
func TestVerifyFindingDocIsMachineReadable(t *testing.T) {
	txn := ledger.NewTxnID()
	finding := store.VerifyFinding{
		TxnID: txn,
		Entries: []ledger.Entry{
			{
				TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
				Amount: money.MustParse("30"), EntryType: ledger.EntryAdjustment,
			},
			{
				TxnID: txn, Account: ledger.UpstreamCostAccount(), Asset: ledger.AssetCNY,
				Amount: money.MustParse("-30"), EntryType: ledger.EntryAdjustment,
			},
		},
		Err: errors.New("第 1 条分录: 科目 upstream_cost 的金额为零"),
	}

	var got struct {
		TxnID   string `json:"txn_id"`
		Err     string `json:"error"`
		Entries []struct {
			Index     int    `json:"index"`
			EntryType string `json:"entry_type"`
			Account   string `json:"account"`
			Amount    string `json:"amount"`
			Asset     string `json:"asset"`
		} `json:"entries"`
	}
	decodeDoc(t, verifyFindingDoc(finding), &got)

	if got.TxnID != string(txn) {
		t.Errorf("txn_id = %q，想要 %q", got.TxnID, txn)
	}
	if !strings.Contains(got.Err, "upstream_cost") {
		t.Errorf("error 应当是校验那一侧的原文，得到 %q", got.Err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries 有 %d 条，想要 2", len(got.Entries))
	}
	// 下标与表格那一栏一样从 0 起，且按读回的顺序。
	if got.Entries[0].Index != 0 || got.Entries[1].Index != 1 {
		t.Errorf("组内下标对不上：%d / %d", got.Entries[0].Index, got.Entries[1].Index)
	}
	if got.Entries[0].Account != "platform_revenue" || got.Entries[1].Account != "upstream_cost" {
		t.Errorf("分录的科目对不上：%+v", got.Entries)
	}
	// 金额保持符号：账本里借为正、贷为负，取绝对值会把方向抹掉。
	if got.Entries[0].Amount != "30.00000000" || got.Entries[1].Amount != "-30.00000000" {
		t.Errorf("分录金额对不上（符号要保留）：%+v", got.Entries)
	}
	if got.Entries[0].EntryType != "adjustment" || got.Entries[0].Asset != "CNY" {
		t.Errorf("分录的类型与资产对不上：%+v", got.Entries[0])
	}

	// 没有错误信息时 error 是 null，不是空串。
	var noErr struct {
		Err *string `json:"error"`
	}
	decodeDoc(t, verifyFindingDoc(store.VerifyFinding{TxnID: txn}), &noErr)
	if noErr.Err != nil {
		t.Errorf("没有错误时 error 应当是 null，得到 %q", *noErr.Err)
	}
}

// 命令级：`--all` 的 JSON 里能看到这个科目，金额是定点字符串。
func TestLedgerBalanceAllJSONStdoutIsMachineReadable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	account := ledgerAccount()
	txn := ledger.NewTxnID()
	insertLedgerEntries(t, db, time.Now(), []ledger.Entry{
		adjustment(t, txn, account, "7"),
		adjustment(t, ledger.NewTxnID(), ledger.PlatformRevenueAccount(), "-7"),
	})

	withFormat(t, formatJSON)
	out, err := runBalanceCmd(t, "--all")
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	assertOnlyJSON(t, out)

	var got struct {
		Status  string `json:"status"`
		Asset   string `json:"asset"`
		Summary struct {
			Accounts int `json:"accounts"`
		} `json:"summary"`
		Accounts []struct {
			Account string `json:"account"`
			Balance string `json:"balance"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, out)
	}
	if got.Status != jsonOK || got.Asset != "CNY" {
		t.Errorf("status=%q asset=%q，想要 %q / CNY", got.Status, got.Asset, jsonOK)
	}
	if got.Summary.Accounts != len(got.Accounts) {
		t.Errorf("summary.accounts = %d，清单里有 %d 条", got.Summary.Accounts, len(got.Accounts))
	}
	names := make([]string, 0, len(got.Accounts))
	for _, a := range got.Accounts {
		names = append(names, a.Account)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("清单没有按科目字符串排序：%v", names)
	}
	var found bool
	for _, a := range got.Accounts {
		if a.Account == account.String() {
			found = true
			if a.Balance != "7.00000000" {
				t.Errorf("%s 的余额 = %q，想要 7.00000000", account, a.Balance)
			}
		}
	}
	if !found {
		t.Fatalf("汇总里没有这个科目：\n%s", out)
	}
}

// 命令级：verify 的三条路径都要在 stdout 上留下**一份能解析的 JSON**。
//
// 三条都会走到：跑完且自洽（status ok、退出码 0）、被 --max-txns 截断
// （status skipped、退出码仍是 0）、以及区间里一条分录都没有（status failed、
// 退出码 1）。最后那条正是「结论先定出来、再用它填 exit_code」要保住的东西：
// 它在 JSON 模式下也必须打得出来，而不是只剩 stderr 上那句错误。
func TestLedgerVerifyJSONStdoutIsMachineReadable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	at, from, to := ledgerSlot()
	first := ledger.NewTxnID()
	second := ledger.NewTxnID()
	insertLedgerEntries(t, db, at, []ledger.Entry{
		adjustment(t, first, ledger.PlatformRevenueAccount(), "5"),
		adjustment(t, first, ledger.UpstreamCostAccount(), "-5"),
		adjustment(t, second, ledger.PlatformRevenueAccount(), "3"),
		adjustment(t, second, ledger.UpstreamCostAccount(), "-3"),
	})
	fromArg, toArg := from.Format(time.RFC3339), to.Format(time.RFC3339)

	run := func(t *testing.T, args ...string) (string, error) {
		t.Helper()
		var err error
		out, _ := captureStreams(t, func() { err = runLedgerVerify(args) })
		return out, err
	}

	type report struct {
		Status  string `json:"status"`
		Summary struct {
			CheckedTxns int  `json:"checked_txns"`
			Truncated   bool `json:"truncated"`
		} `json:"summary"`
		Findings []map[string]any `json:"findings"`
		ExitCode int              `json:"exit_code"`
	}
	decode := func(t *testing.T, out string) report {
		t.Helper()
		assertOnlyJSON(t, out)
		var got report
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("解不回来: %v\n%s", err, out)
		}
		return got
	}

	t.Run("跑完且自洽", func(t *testing.T) {
		withFormat(t, formatJSON)
		out, err := run(t, "--from", fromArg, "--to", toArg)
		if err != nil {
			t.Fatalf("自洽的区间不该失败: %v\n%s", err, out)
		}
		got := decode(t, out)
		if got.Status != jsonOK || got.ExitCode != exitOK {
			t.Errorf("status=%q exit_code=%d，想要 %q / 0", got.Status, got.ExitCode, jsonOK)
		}
		if got.Summary.CheckedTxns != 2 {
			t.Errorf("checked_txns = %d，想要 2", got.Summary.CheckedTxns)
		}
		if got.Summary.Truncated {
			t.Error("没给 --max-txns 时不该有 truncated")
		}
	})

	t.Run("被截断", func(t *testing.T) {
		withFormat(t, formatJSON)
		out, err := run(t, "--from", fromArg, "--to", toArg, "--max-txns", "1")
		if err != nil {
			t.Fatalf("截断不是失败: %v\n%s", err, out)
		}
		got := decode(t, out)
		// 没查出问题，但这一轮**不是**全量：这正是 skipped 那一档的定义。
		if got.Status != jsonSkipped {
			t.Errorf("status = %q，想要 %q（截断不是「全部通过」）", got.Status, jsonSkipped)
		}
		if !got.Summary.Truncated || got.Summary.CheckedTxns != 1 {
			t.Errorf("summary 对不上：%+v", got.Summary)
		}
		if got.ExitCode != exitOK {
			t.Errorf("exit_code = %d，想要 0：截断是用 --max-txns 要来的", got.ExitCode)
		}
	})

	t.Run("一条分录都没有", func(t *testing.T) {
		withFormat(t, formatJSON)
		// 另一个没放东西的时间窗（见 ledgerSlot）。
		_, emptyFrom, emptyTo := ledgerSlot()
		out, err := run(t, "--from", emptyFrom.Format(time.RFC3339), "--to", emptyTo.Format(time.RFC3339))
		if err == nil {
			t.Fatalf("一条都没校验到时应当以非零退出码收尾：\n%s", out)
		}
		got := decode(t, out)
		if got.Status != jsonFailed || got.ExitCode != exitFailure {
			t.Errorf("status=%q exit_code=%d，想要 %q / %d",
				got.Status, got.ExitCode, jsonFailed, exitFailure)
		}
		if got.Findings == nil {
			t.Error("findings 应当是 []，不是 null")
		}
	})
}
