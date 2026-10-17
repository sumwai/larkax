package main

import (
	"bytes"
	"context"
	"database/sql"
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
	// 表头 + 四个科目 + 读不出来的那一行 + 指路的那一行。
	if len(lines) != 7 {
		t.Fatalf("打了 %d 行，期望 7 行：\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "科目数=4") || !strings.Contains(lines[0], "随分录数增长") {
		t.Errorf("表头没有说明这是什么、以及它随什么增长：%q", lines[0])
	}
	want := []string{"payout:3", "platform_revenue", "upstream_cost", "user:7"}
	for i, name := range want {
		if !strings.Contains(lines[1+i], name) {
			t.Errorf("第 %d 行应当是 %s：%q", i+1, name, lines[1+i])
		}
	}
	if !strings.Contains(lines[5], "nonsense:1") || !strings.Contains(lines[5], "没有计进") {
		t.Errorf("读不出来的科目没有被说出来：%q", lines[5])
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
	})
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
	printLedgerVerifyReport(&clean, &store.VerifyReport{CheckedTxns: 3, CheckedEntries: 8})
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
	printLedgerVerifyReport(&none, &store.VerifyReport{})
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
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
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
