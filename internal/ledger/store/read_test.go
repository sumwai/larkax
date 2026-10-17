package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件测的是账本的只读一侧：余额查询与完整性校验。
//
// 它跑在真库上（理由见 helpers_test.go 的 testDB）：这一侧要说清的正是
// 「从库里读回来的行算什么」，而拿内存假装的那些性质——uuid 的文本形态、
// numeric 的精度、`account` 是 text 因而能存下任何写法——都只在真库上存在。

// slotAt 给这次用例一个**独占**的时刻，以及一个只覆盖它的区间。
//
// 本包的其他用例会在同一个库里留下分录（seed 造的充值不清理），所以
// 「校验某个区间」的断言必须只圈到自己插进去的那几条，否则它测的是
// 别人留了什么。相邻两次调用相隔一秒，区间取 ±500 毫秒，于是两两不相交。
func slotAt() (at, from, to time.Time) {
	at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(nextID()%100000) * time.Second)
	return at, at.Add(-500 * time.Millisecond), at.Add(500 * time.Millisecond)
}

// insertAt 写一组分录，created_at 由调用方给。
//
// 时间显式给而不是让库取 now()：见 slotAt。
func insertAt(t *testing.T, db *sql.DB, at time.Time, entries []ledger.Entry) {
	t.Helper()
	ctx := context.Background()
	for _, e := range entries {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type, created_at)
			VALUES ($1, $2, $3, $4::numeric, $5, $6)`,
			string(e.TxnID), e.Account.String(), string(e.Asset),
			e.Amount.String(), string(e.EntryType), at); err != nil {
			t.Fatalf("写分录失败: %v", err)
		}
	}
}

// adjustmentEntry 造一条人工调账分录。
//
// 用它而不是 ledger.Topup 那一套构造器：本文件要测的是**读**，而调账分录
// 对科目与方向都不设限，于是「这一笔平不平」只由金额决定，用例里的意图
// 不会被方向的约定盖住。
func adjustmentEntry(t *testing.T, txn ledger.TxnID, account ledger.Account, amount string) ledger.Entry {
	t.Helper()
	amt, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析金额失败: %v", err)
	}
	return ledger.Entry{
		TxnID: txn, Account: account, Asset: ledger.AssetCNY,
		Amount: amt, EntryType: ledger.EntryAdjustment,
	}
}

// 一个科目有多条分录时，余额是它们逐条相加的结果。
//
// 同时钉住「读的是账本，不是快照」：这个用例故意只加分录、不改
// user_wallets，于是账本（70）与快照（100）不一致——返回 70 才说明
// 数是从分录现算的。这也正是这条命令存在的意义：快照是读优化，权威值是账本。
func TestAccountBalanceSumsEveryEntry(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")

	// 再来一笔消费：用户 -30、平台收入 +30。走 insertEntries（写路径用的
	// 就是它），而不是另拼一句 INSERT——读回来的东西要与生产写进去的一样。
	//
	// 分录不关联账单（billID 传 0）：这个用例的世界里没有 billing_requests
	// 行，生产也给不出一个不存在的 id。要断言「按账单找分录」的用例自己
	// 把预留拿到的 id 传进来（见 consumeEntries 的注释）。
	//
	// 变量名不复用 err：每次重新声明都会让读代码的人停下来判断「看到的是
	// 哪个 err」，而多一个名字的成本是零。
	tx, beginErr := db.BeginTx(ctx, nil)
	if beginErr != nil {
		t.Fatalf("开事务失败: %v", beginErr)
	}
	if writeErr := insertEntries(ctx, tx, consumeEntries(t, userID, "30", 0)); writeErr != nil {
		t.Fatalf("写消费分录失败: %v", writeErr)
	}
	if commitErr := tx.Commit(); commitErr != nil {
		t.Fatalf("提交失败: %v", commitErr)
	}

	got, err := s.AccountBalance(ctx, ledger.UserAccount(userID), ledger.AssetCNY)
	if err != nil {
		t.Fatalf("查余额失败: %v", err)
	}
	if want := money.MustParse("70"); got.Cmp(want) != 0 {
		t.Errorf("余额 = %s，期望 %s（充值 100 减消费 30）", got, want)
	}

	// 快照没跟着改（这个用例没有走改快照的那条路），所以它还是 100。
	// 上面那条断言因此分辨得出「读账本」与「读快照」两种实现。
	if balance, _ := walletOf(t, db, userID); balance != "100.00000000" {
		t.Fatalf("快照余额 = %s，期望还是 100.00000000——用例的前提是两者不一致", balance)
	}
}

// 旧写路径留下的别名写法要算进同一个用户的余额。
//
// 写路径当初把后缀读成整数，所以 `user:0<id>` 那样的行**当初是**算进这个
// 用户钱包的。读的时候逐字相等看不见它们，余额就偏小——而偏小的方向恰好是
// 「看起来少扣了钱」，那是最难查的一类。对账的 readLedgerSums 走的是同一条口径。
func TestAccountBalanceCountsLegacyAliasAccounts(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	alias := "user:0" + itoa(userID)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM ledger_entries WHERE account = $1`, alias)
	})

	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
		VALUES (gen_random_uuid(), $1, 'CNY', 7, 'adjustment')`, alias); err != nil {
		t.Fatalf("写别名分录失败: %v", err)
	}

	got, err := s.AccountBalance(ctx, ledger.UserAccount(userID), ledger.AssetCNY)
	if err != nil {
		t.Fatalf("查余额失败: %v", err)
	}
	if want := money.MustParse("107"); got.Cmp(want) != 0 {
		t.Errorf("余额 = %s，期望 %s（100 加上别名行里的 7）", got, want)
	}
}

// 全库汇总按科目分组，每个科目的数与该科目逐条相加的结果一致。
//
// 两个科目都取这个用例**独有**的：seed 用的那个通道科目是全包共用的，
// 断言落在它上面时，别的用例（包括本文件里前面两条）插的充值会一起算进来，
// 于是期望值成了「跑过哪些用例」的函数——那正是这条断言分辨不出来的东西。
func TestAccountBalancesGroupsByAccount(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	user := ledger.UserAccount(nextID())
	payout := ledger.PayoutAccount(nextID())
	txn := ledger.NewTxnID()
	insertAt(t, db, time.Now(), []ledger.Entry{
		adjustmentEntry(t, txn, user, "100"),
		adjustmentEntry(t, ledger.NewTxnID(), payout, "-100"),
	})

	report, err := s.AccountBalances(ctx, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	byAccount, ok := report.ByAccount[ledger.AssetCNY]
	if !ok {
		t.Fatalf("汇总里没有 CNY：%v", report.ByAccount)
	}

	// 断言落在**具体金额**上，而不是「两个数相等」：这一层要钉的是
	// 「哪个科目多少钱」，而相对关系在算错时照样成立。
	if got := byAccount[user]; got.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("%s 的余额 = %s，期望 100", user, got)
	}
	// 另一侧的科目必须出现在**同一份**汇总里。只挑一个科目断言的话，
	// 一个「只汇总了 user 科目」的实现也能过。
	if got := byAccount[payout]; got.Cmp(money.MustParse("-100")) != 0 {
		t.Errorf("%s 的余额 = %s，期望 -100", payout, got)
	}
	// 别的资产不该混进来（见 ledger.Balances 的注释）。
	if _, ok := report.ByAccount["USDT"]; ok {
		t.Error("汇总里不该出现没有分录的资产")
	}
}

// 读不出科目的行不算错误，但必须**带回去**，不能悄悄少算。
//
// 两条路是分开的：一条读不出来的科目不该让整份汇总打不开（账本脏的时候
// 正是最需要看一眼总览的时候），但它也不能被静默丢掉——一份少了几行的
// 汇总与一份完整的汇总在输出上长得一模一样。
func TestAccountBalancesReportsUnreadableAccounts(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")

	account := "nonsense:" + itoa(nextID())
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM ledger_entries WHERE account = $1`, account)
	})
	// 两行同一个坏科目：清单要按**种**去重，重复出现会让人以为行数不对。
	for i := 0; i < 2; i++ {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type)
			VALUES (gen_random_uuid(), $1, 'CNY', 1, 'adjustment')`, account); err != nil {
			t.Fatalf("写分录失败: %v", err)
		}
	}

	report, err := s.AccountBalances(ctx, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("一条读不出来的科目不该让汇总打不开: %v", err)
	}
	if len(report.Unreadable) != 1 || report.Unreadable[0] != account {
		t.Errorf("读不出来的科目清单 = %v，期望只有 %q 一项", report.Unreadable, account)
	}
	// 好的那些照旧算出来——上面的清单是补充，不是「整份作废」。
	if got := report.ByAccount[ledger.AssetCNY][ledger.UserAccount(userID)]; got.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("user:%d 的余额 = %s，期望 100", userID, got)
	}
}

// 自洽的账本校验下来没有 finding，且计数对得上。
func TestVerifyAcceptsBalancedTransactions(t *testing.T) {
	db := testDB(t)
	at, from, to := slotAt()

	first := ledger.NewTxnID()
	second := ledger.NewTxnID()
	insertAt(t, db, at, []ledger.Entry{
		adjustmentEntry(t, first, ledger.PlatformRevenueAccount(), "5"),
		adjustmentEntry(t, first, ledger.UpstreamCostAccount(), "-5"),
		adjustmentEntry(t, second, ledger.PlatformRevenueAccount(), "-2"),
		adjustmentEntry(t, second, ledger.UpstreamCostAccount(), "2"),
	})

	report, err := New(db).Verify(context.Background(), VerifyScope{From: from, To: to})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if report.CheckedTxns != 2 || report.CheckedEntries != 4 {
		t.Errorf("校验了 %d 笔（%d 条分录），期望 2 笔（4 条）",
			report.CheckedTxns, report.CheckedEntries)
	}
	if len(report.Findings) != 0 {
		t.Errorf("自洽的分录不该被报出来：%v", report.Findings)
	}
	if report.Truncated {
		t.Error("没有给上限时不该报成截断")
	}
}

// 不合法的分录要能指到**具体是哪一笔的哪一条**，而同一区间里合法的那些
// 一条都不许被带出来。
func TestVerifyNamesTheOffendingEntry(t *testing.T) {
	db := testDB(t)
	at, from, to := slotAt()

	good := ledger.NewTxnID()
	bad := ledger.NewTxnID()
	insertAt(t, db, at, []ledger.Entry{
		// 一笔正常的。
		adjustmentEntry(t, good, ledger.PlatformRevenueAccount(), "5"),
		adjustmentEntry(t, good, ledger.UpstreamCostAccount(), "-5"),
		// 一笔里面第 1 条是零金额。和仍然为零，所以「不平」那条判据看不见它
		// ——这正是要用单条分录的判据去抓的形状。
		adjustmentEntry(t, bad, ledger.PlatformRevenueAccount(), "30"),
		adjustmentEntry(t, bad, ledger.UpstreamCostAccount(), "0"),
		adjustmentEntry(t, bad, ledger.UpstreamCostAccount(), "-30"),
	})

	report, err := New(db).Verify(context.Background(), VerifyScope{From: from, To: to})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if report.CheckedTxns != 2 || report.CheckedEntries != 5 {
		t.Errorf("校验了 %d 笔（%d 条分录），期望 2 笔（5 条）",
			report.CheckedTxns, report.CheckedEntries)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("应当恰好报出 1 笔，得到 %d 笔：%v", len(report.Findings), report.Findings)
	}

	f := report.Findings[0]
	if f.TxnID != bad {
		t.Errorf("报出来的是 %s，期望 %s（那笔合法的也被报出来了？）", f.TxnID, bad)
	}
	// 校验错误里的下标是**组内**下标，所以它必须与这一组的分录条数对得上，
	// 而交易标识得单独带着——不然看错误的人回不到是哪一笔。
	if !strings.Contains(f.Err.Error(), "第 1 条分录") {
		t.Errorf("错误信息 %q 里没有指到第 1 条分录", f.Err.Error())
	}
	if !strings.Contains(f.Err.Error(), "金额为零") {
		t.Errorf("错误信息 %q 里没有说清是哪一类问题", f.Err.Error())
	}
	if len(f.Entries) != 3 {
		t.Errorf("带回来的分录有 %d 条，期望 3 条（「第 N 条」要对着它才数得出来）", len(f.Entries))
	}
}

// 上限按**交易**算，不按行算。
//
// 按行截断会把一笔交易切成两半，而半笔可能仍然是平的，于是截断本身会
// 制造出「查过了、没问题」的假象。这里的第一笔恰好是「切成两半就平了」
// 的形状（三条分录里任意一条单独拿出来都不平，但切法不当会剩下两条）。
func TestVerifyTruncatesByWholeTransaction(t *testing.T) {
	db := testDB(t)
	at, from, to := slotAt()

	for i := 0; i < 3; i++ {
		txn := ledger.NewTxnID()
		insertAt(t, db, at.Add(time.Duration(i)*time.Millisecond), []ledger.Entry{
			adjustmentEntry(t, txn, ledger.PlatformRevenueAccount(), "3"),
			adjustmentEntry(t, txn, ledger.UpstreamCostAccount(), "-3"),
		})
	}

	report, err := New(db).Verify(context.Background(), VerifyScope{From: from, To: to, MaxTxns: 1})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if report.CheckedTxns != 1 {
		t.Errorf("校验了 %d 笔，期望 1 笔", report.CheckedTxns)
	}
	if report.CheckedEntries != 2 {
		t.Errorf("参与校验的分录有 %d 条，期望 2 条（一笔要么整笔算，要么不算）",
			report.CheckedEntries)
	}
	if !report.Truncated {
		t.Error("到了上限就停下时必须说自己是截断的，否则它与跑完了一模一样")
	}
	if len(report.Findings) != 0 {
		t.Errorf("这三笔都是平的，不该报出来：%v", report.Findings)
	}
}

// 区间只覆盖一笔交易的一部分行时，这一笔仍然要**整笔**读回来校验。
// 直接按行过滤会把横跨区间边界的交易切两半，而半笔恰好可能看起来是平的
// （见 ledger.VerifyEntries 的注释）。这里的形状是「只读一行必然不平」：
// 一条 +25 配一条 -25，读一半就只剩一条，于是被截断的实现会报出一个假 finding。
func TestVerifyKeepsStraddlingTransactionWhole(t *testing.T) {
	db := testDB(t)
	at, _, _ := slotAt()

	txn := ledger.NewTxnID()
	insertAt(t, db, at, []ledger.Entry{adjustmentEntry(t, txn, ledger.PlatformRevenueAccount(), "25")})
	insertAt(t, db, at.Add(400*time.Millisecond),
		[]ledger.Entry{adjustmentEntry(t, txn, ledger.UpstreamCostAccount(), "-25")})

	// 区间只盖住第一行（To 落在两行之间）。
	from, to := at.Add(-500*time.Millisecond), at.Add(100*time.Millisecond)

	report, err := New(db).Verify(context.Background(), VerifyScope{From: from, To: to})
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if report.CheckedTxns != 1 || report.CheckedEntries != 2 {
		t.Errorf("校验了 %d 笔（%d 条分录），期望 1 笔（2 条）：区间切不开一笔交易",
			report.CheckedTxns, report.CheckedEntries)
	}
	if len(report.Findings) != 0 {
		t.Errorf("整笔读回来是平的，不该报出来：%v", report.Findings)
	}
}

// 校验遇到读不出来的科目要**停下报错**，而不是像汇总那样少算一行。
//
// 两处处置不同是有意的：校验的对象是整笔交易，少一行可能让它看起来是平的
// （见 ledger.VerifyEntries 的注释），于是这一轮的结果不再可信。这里同时
// 钉住「半份报告也交回去」——比到哪儿、发现了什么，比一句「失败了」有用。
func TestVerifyStopsOnUnreadableAccount(t *testing.T) {
	db := testDB(t)
	at, from, to := slotAt()

	account := "nonsense:" + itoa(nextID())
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM ledger_entries WHERE account = $1`, account)
	})
	insertAt(t, db, at, []ledger.Entry{
		adjustmentEntry(t, ledger.NewTxnID(), ledger.PlatformRevenueAccount(), "1"),
	})
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type, created_at)
		VALUES (gen_random_uuid(), $1, 'CNY', 1, 'adjustment', $2)`, account, at); err != nil {
		t.Fatalf("写分录失败: %v", err)
	}

	report, err := New(db).Verify(context.Background(), VerifyScope{From: from, To: to})
	if err == nil {
		t.Fatal("区间里有读不出来的科目时应当报错")
	}
	if !strings.Contains(err.Error(), account) {
		t.Errorf("错误信息 %q 里没有点出 %q", err.Error(), account)
	}
	if report == nil {
		t.Error("出错时也要把已经算出来的那半份报告交回来")
	}
}
