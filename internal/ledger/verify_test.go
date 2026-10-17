package ledger

import (
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// 从数据库读回的一组分录是校验真正的用武之地：库里的行可能来自
// 任何写入路径、一次人工修数、或者一个已经下线的旧版本。
// 下面这一组用例全部按「外部构造」的形态搭，不走构造函数。
func externalEntries(t *testing.T, txn TxnID, spec []struct {
	account string
	amount  string
	kind    EntryType
}) []Entry {
	t.Helper()
	out := make([]Entry, 0, len(spec))
	for _, s := range spec {
		acct, err := ParseAccount(s.account)
		if err != nil {
			t.Fatalf("测试用的科目 %q 不合法: %v", s.account, err)
		}
		out = append(out, Entry{
			TxnID:     txn,
			Account:   acct,
			Asset:     AssetCNY,
			Amount:    amount(t, s.amount),
			EntryType: s.kind,
		})
	}
	return out
}

func TestVerifyAcceptsReadBackEntries(t *testing.T) {
	txn := newTxnID(t, txnA)
	entries := externalEntries(t, txn, []struct {
		account string
		amount  string
		kind    EntryType
	}{
		{"user:42", "-10", EntryConsume},
		{"merchant_payable:7", "6", EntryConsume},
		{"upstream_cost", "3", EntryConsume},
		{"platform_revenue", "1", EntryConsume},
	})
	tx := Transaction{TxnID: txn, Entries: entries}
	if err := tx.Verify(); err != nil {
		t.Fatalf("自洽的分录不该被拒: %v", err)
	}
	if err := VerifyEntries(entries); err != nil {
		t.Fatalf("VerifyEntries 不该拒: %v", err)
	}
}

// 不平是核心不变量，必须能查出来——而且要报出差额本身，
// 因为差额是排查时唯一能对上「哪一环算错了」的线索。
func TestVerifyRejectsUnbalanced(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"user:42", "-10", EntryConsume},
			{"platform_revenue", "9", EntryConsume}, // 差 1
		}),
	}
	err := tx.Verify()
	if err == nil {
		t.Fatal("不平的交易应当被拒")
	}
	if !strings.Contains(err.Error(), "不平") {
		t.Errorf("报错要说清是不平，得到: %v", err)
	}
	// 差额要出现在报错里，且要提示不能补分录。
	for _, want := range []string{"1.00000000", "不能补"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里少了 %q: %v", want, err)
		}
	}
}

// 不平必须是错误，**不是**自动补一笔调平分录。
// 补一笔会让代码继续跑、账面继续错，只是错的地方从「不平」变成
// 「一笔来路不明的分录」，而后者更查不出来。
func TestVerifyNeverAutoBalances(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"user:42", "-10", EntryConsume},
		}),
	}
	before := len(tx.Entries)
	if err := tx.Verify(); err == nil {
		t.Fatal("单边分录应当被拒")
	}
	if len(tx.Entries) != before {
		t.Errorf("Verify 改动了分录条数：%d → %d", before, len(tx.Entries))
	}
}

// 方向反了但两边一起反，求和仍然是零。求和的校验看不见它，
// 所以必须有一条按交易类型的方向检查。
func TestVerifyRejectsInvertedDirection(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"user:42", "10", EntryConsume},           // 消费应当扣钱（贷），这里是借
			{"platform_revenue", "-10", EntryConsume}, // 对家也跟着反，和为零
		}),
	}
	err := tx.Verify()
	if err == nil {
		t.Fatal("方向反了的交易应当被拒，尽管它求和为零")
	}
	if !strings.Contains(err.Error(), "贷方") {
		t.Errorf("报错要说清期望的方向，得到: %v", err)
	}
}

// 在某一类交易里出现不该有的科目：那是一处方向约定没定义过的组合，
// 报错比放行保守。
func TestVerifyRejectsUnexpectedAccountForType(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"user:42", "-10", EntryConsume},
			{"payout:1", "10", EntryConsume}, // payout 不该出现在消费里
		}),
	}
	if err := tx.Verify(); err == nil {
		t.Fatal("不该出现的科目应当被拒")
	}
}

// 方向检查要能区分「科目合法」与「方向合法」：同样是 payout_paid，
// 正确的记法是 payout 贷、通道借；两边一起反过来虽然求和为零，
// 但意思完全相反（那是「收钱进来」而不是「打款出去」）。
func TestVerifyRejectsWrongSignForPaidPayout(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"payout:1", "5", EntryPayoutPaid}, // 打款时 payout 应当核销（贷）
			{"payment_gateway:stripe", "-5", EntryPayoutPaid},
		}),
	}
	if err := tx.Verify(); err == nil {
		t.Fatal("打款方向写反应当被拒，尽管求和为零")
	}
}

func TestVerifyRejectsEmptyAndZeroAmount(t *testing.T) {
	txn := newTxnID(t, txnA)

	empty := Transaction{TxnID: txn}
	if err := empty.Verify(); err == nil {
		t.Error("空交易应当被拒")
	}

	zero := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"user:42", "0", EntryConsume},
		}),
	}
	if err := zero.Verify(); err == nil {
		t.Error("零金额分录应当被拒")
	}
}

func TestVerifyRejectsEmptyAccount(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: []Entry{
			{TxnID: txn, Asset: AssetCNY, Amount: amount(t, "1"), EntryType: EntryAdjustment},
			{TxnID: txn, Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "-1"), EntryType: EntryAdjustment},
		},
	}
	err := tx.Verify()
	if err == nil {
		t.Fatal("空科目应当被拒")
	}
	// 空科目的报错不该是「大类 "" 未知」——那会让人以为取值写错了，
	// 而实际是整列漏填。
	if !strings.Contains(err.Error(), "科目为空") {
		t.Errorf("报错要说清是科目为空，得到: %v", err)
	}
}

// 非法科目进不了 Verify 的入口：Account 的字段不导出，唯一的读回通道是
// UnmarshalText，而它在解析时就拦住了。这条用例钉住那个通道。
func TestMalformedAccountCannotBeReadBack(t *testing.T) {
	var bad Account
	if err := bad.UnmarshalText([]byte("user")); err == nil {
		t.Fatal("缺 id 后缀的科目不该读回来")
	}
	if err := bad.UnmarshalText([]byte("user:1")); err != nil {
		t.Fatalf("合法科目应当读得回来: %v", err)
	}
}

func TestVerifyRejectsBadAssetAndType(t *testing.T) {
	txn := newTxnID(t, txnA)
	base := func() Transaction {
		return Transaction{
			TxnID: txn,
			Entries: []Entry{
				{TxnID: txn, Account: PlatformRevenueAccount(), Asset: AssetCNY, Amount: amount(t, "1"), EntryType: EntryConsume},
				{TxnID: txn, Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "-1"), EntryType: EntryConsume},
			},
		}
	}

	badAsset := base()
	badAsset.Entries[0].Asset = Asset("BTC")
	badAsset.Entries[1].Asset = Asset("BTC")
	if err := badAsset.Verify(); err == nil {
		t.Error("未知资产应当被拒")
	}

	badType := base()
	badType.Entries[0].EntryType = EntryType("gift")
	badType.Entries[1].EntryType = EntryType("gift")
	if err := badType.Verify(); err == nil {
		t.Error("未知分录类型应当被拒")
	}
}

// 同一交易里出现两条完全相同的分录，几乎只有「同一条被写了两遍」
// 这一种来路。它一定该被看见。
func TestVerifyRejectsDuplicateEntries(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"user:42", "-10", EntryConsume},
			{"user:42", "-10", EntryConsume}, // 重复
			{"platform_revenue", "20", EntryConsume},
		}),
	}
	err := tx.Verify()
	if err == nil {
		t.Fatal("完全重复的分录应当被拒")
	}
	if !strings.Contains(err.Error(), "重复") {
		t.Errorf("报错要说清是重复，得到: %v", err)
	}
}

// 同一个科目出现两条**金额不同**的分录是合法的：它来自两笔独立的事件
// （例如一次分账加一次调整）。
func TestVerifyAllowsSameAccountDifferentAmounts(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: externalEntries(t, txn, []struct {
			account string
			amount  string
			kind    EntryType
		}{
			{"user:42", "-10", EntryAdjustment},
			{"user:42", "-5", EntryAdjustment},
			{"platform_revenue", "15", EntryAdjustment},
		}),
	}
	if err := tx.Verify(); err != nil {
		t.Errorf("金额不同的同科目分录不该被拒: %v", err)
	}
}

// VerifyEntries 按 txn_id 分组，一组不平就要报出来。
func TestVerifyEntriesGroupsByTxn(t *testing.T) {
	a := newTxnID(t, txnA)
	b := newTxnID(t, txnB)
	entries := externalEntries(t, a, []struct {
		account string
		amount  string
		kind    EntryType
	}{
		{"user:1", "-1", EntryConsume},
		{"platform_revenue", "1", EntryConsume},
	})
	entries = append(entries, externalEntries(t, b, []struct {
		account string
		amount  string
		kind    EntryType
	}{
		{"user:2", "-2", EntryConsume},
		{"platform_revenue", "1", EntryConsume}, // 第二组不平
	})...)

	err := VerifyEntries(entries)
	if err == nil {
		t.Fatal("有一组不平就该报出来")
	}
	if !strings.Contains(err.Error(), string(b)) {
		t.Errorf("报错要指明是哪一组，得到: %v", err)
	}
}

func TestVerifyEntriesRejectsEmptyInput(t *testing.T) {
	if err := VerifyEntries(nil); err == nil {
		t.Error("空输入应当报错，而不是静默通过")
	}
}

// 不平的报错文本必须稳定：求和走的是 map，遍历顺序随机，两次运行的
// 报错不同就没法断言、也没法按文本搜日志。所以报错前先把键排一遍。
func TestVerifyErrorIsDeterministic(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx := Transaction{
		TxnID: txn,
		Entries: []Entry{
			{TxnID: txn, Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "2"), EntryType: EntryAdjustment},
		},
	}
	first := tx.Verify()
	if first == nil {
		t.Fatal("应当不平")
	}
	for i := 0; i < 50; i++ {
		if got := tx.Verify(); got == nil || got.Error() != first.Error() {
			t.Fatalf("第 %d 次的报错与首次数不一致: %v vs %v", i, got, first)
		}
	}
}

// sortedAssets 是「报错文本稳定」的实现手段。它按字典序返回键，
// 与 map 的随机遍历顺序无关——接第二种资产之后不平的报错才仍然可比。
func TestSortedAssetsIsDeterministic(t *testing.T) {
	m := map[Asset]money.Amount{"USDT": money.One, AssetCNY: money.One}
	for i := 0; i < 20; i++ {
		got := sortedAssets(m)
		if len(got) != 2 || got[0] != AssetCNY || got[1] != "USDT" {
			t.Fatalf("排序结果 = %v，期望 [CNY USDT]", got)
		}
	}
	if got := sortedAssets(nil); len(got) != 0 {
		t.Errorf("空 map 应当返回空切片，得到 %v", got)
	}
}

// ── 余额推导 ────────────────────────────────────────────────────────────

func TestBalanceOfIsPureSummation(t *testing.T) {
	txn := newTxnID(t, txnA)
	entries := externalEntries(t, txn, []struct {
		account string
		amount  string
		kind    EntryType
	}{
		{"user:42", "100", EntryTopup},
		{"payment_gateway:stripe", "-100", EntryTopup},
		{"user:42", "-30", EntryConsume},
		{"platform_revenue", "30", EntryConsume},
		{"user:42", "10", EntryRefund},
		{"platform_revenue", "-10", EntryRefund},
	})

	if got := BalanceOf(entries, UserAccount(42), AssetCNY); got.Cmp(amount(t, "80")) != 0 {
		t.Errorf("用户余额 = %s，期望 80", got)
	}
	if got := BalanceOf(entries, PaymentGatewayAccount("stripe"), AssetCNY); got.Cmp(amount(t, "-100")) != 0 {
		t.Errorf("通道余额 = %s，期望 -100", got)
	}
	// 没有分录的科目余额是零，而不是报错。
	if got := BalanceOf(entries, UserAccount(7), AssetCNY); !got.IsZero() {
		t.Errorf("没有分录的科目余额 = %s，期望 0", got)
	}
	// 资产不匹配时也不参与。
	if got := BalanceOf(entries, UserAccount(42), "USDT"); !got.IsZero() {
		t.Errorf("别的资产上的余额 = %s，期望 0", got)
	}
}

func TestBalanceOfEmptyInput(t *testing.T) {
	if got := BalanceOf(nil, UserAccount(1), AssetCNY); !got.IsZero() {
		t.Errorf("空分录的余额 = %s，期望 0", got)
	}
}

func TestBalancesGroupsByAssetAndAccount(t *testing.T) {
	txn := newTxnID(t, txnA)
	entries := []Entry{
		{TxnID: txn, Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "5"), EntryType: EntryAdjustment},
		{TxnID: txn, Account: UserAccount(1), Asset: AssetCNY, Amount: amount(t, "-2"), EntryType: EntryAdjustment},
		{TxnID: txn, Account: PlatformRevenueAccount(), Asset: AssetCNY, Amount: amount(t, "-3"), EntryType: EntryAdjustment},
	}

	got := Balances(entries)
	if bal := got[AssetCNY][UserAccount(1)]; bal.Cmp(amount(t, "3")) != 0 {
		t.Errorf("user:1 的余额 = %s，期望 3", bal)
	}
	if bal := got[AssetCNY][PlatformRevenueAccount()]; bal.Cmp(amount(t, "-3")) != 0 {
		t.Errorf("platform_revenue 的余额 = %s，期望 -3", bal)
	}
	if _, ok := got["USDT"]; ok {
		t.Error("没有分录的资产不该出现在结果里")
	}
	if len(Balances(nil)) != 0 {
		t.Error("空输入的汇总应当是空的")
	}
}

// 所有构造出来的交易都应当两两配对为零，且每个科目的余额
// 与逐条相加的结果一致——这是「账本能自圆其说」的最朴素形式。
func TestConstructedTransactionBalancesToZero(t *testing.T) {
	txn := newTxnID(t, txnA)
	tx, err := Consume(ConsumeParams{
		TxnID: txn, UserID: 42, Amount: amount(t, "10"), Asset: AssetCNY,
		MerchantID: 7, MerchantShare: amount(t, "6"), UpstreamCost: amount(t, "3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Verify(); err != nil {
		t.Errorf("构造出的交易应当自洽: %v", err)
	}
	total := money.Zero
	for _, bal := range Balances(tx.Entries)[AssetCNY] {
		total = total.Add(bal)
	}
	if !total.IsZero() {
		t.Errorf("各科目余额之和 = %s，期望 0", total)
	}
}
