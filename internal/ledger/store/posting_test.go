package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// topupTxn 造一笔充值交易，形状与命令行那条 credit 命令造出来的完全一致。
//
// 走 ledger.Topup 而不是手拼分录：这一层要证明的是「造好的分录能落库、
// 余额快照跟着动」，手拼的分录只能证明「我拼的那几条写得进去」。
func topupTxn(t *testing.T, txnID ledger.TxnID, userID int64, amount, gateway, order string) ledger.Transaction {
	t.Helper()
	amt, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析金额 %q 失败: %v", amount, err)
	}
	txn, err := ledger.Topup(ledger.TopupParams{
		TxnID:   txnID,
		UserID:  userID,
		Amount:  amt,
		Asset:   ledger.AssetCNY,
		Gateway: gateway,
		OrderNo: order,
	})
	if err != nil {
		t.Fatalf("构造充值交易失败: %v", err)
	}
	return txn
}

// entryRow 是一条分录在库里的样子，供逐字比对。
type entryRow struct {
	Account   string
	Asset     string
	Amount    string
	EntryType string
	Memo      string
}

// entriesOf 按科目读出某个交易写下的全部分录。
func entriesOf(t *testing.T, db *sql.DB, txnID ledger.TxnID) []entryRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT account, asset, amount::text, entry_type, coalesce(memo, '')
		  FROM ledger_entries WHERE txn_id = $1 ORDER BY account`, string(txnID))
	if err != nil {
		t.Fatalf("读分录失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []entryRow
	for rows.Next() {
		var r entryRow
		if err := rows.Scan(&r.Account, &r.Asset, &r.Amount, &r.EntryType, &r.Memo); err != nil {
			t.Fatalf("扫描分录失败: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历分录失败: %v", err)
	}
	return out
}

// 一次入账真的写下了那两条分录，并把净额加到钱包快照上。
//
// 金额与分录逐条钉死，不只断言「没报错」：这一层动钱，而「写成功了但写反了
// 方向」「余额加了两次」都不会报错。
func TestPostTransactionWritesEntriesAndBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	// 先给 5 元：入账后应当是 105，而不是 100——「加到了已有余额上」与
	// 「把余额改成了这次入账的金额」是两件不同的事。
	userID := seed(t, db, "5")
	txn := topupTxn(t, ledger.NewTxnID(), userID, "100", "manual", "ORD-1")

	applied, err := s.PostTransaction(ctx, txn)
	if err != nil {
		t.Fatalf("入账失败: %v", err)
	}
	if !applied {
		t.Fatal("第一次入账应当返回 applied=true")
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "105.00000000" {
		t.Errorf("余额 = %s，期望 105.00000000", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("冻结 = %s，期望 0.00000000（入账不该动冻结额）", frozen)
	}

	got := entriesOf(t, db, txn.TxnID)
	want := []entryRow{
		{
			Account: ledger.PaymentGatewayAccount("manual").String(), Asset: "CNY",
			Amount: "-100.00000000", EntryType: string(ledger.EntryTopup),
			Memo: "充值（单据 ORD-1）",
		},
		{
			Account: ledger.UserAccount(userID).String(), Asset: "CNY",
			Amount: "100.00000000", EntryType: string(ledger.EntryTopup),
		},
	}
	if len(got) != len(want) {
		t.Fatalf("写下 %d 条分录，期望 %d 条：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条分录 = %+v，期望 %+v", i, got[i], want[i])
		}
	}

	// 权威值（分录之和）与快照一致：seed 的 5 元加这次的 100 元。
	authoritative, err := s.AccountBalance(ctx, ledger.UserAccount(userID), ledger.AssetCNY)
	if err != nil {
		t.Fatalf("读权威余额失败: %v", err)
	}
	if authoritative.String() != "105.00000000" {
		t.Errorf("账本侧余额 = %s，期望 105.00000000（与快照对得上）", authoritative)
	}
}

// 同一个交易号第二次到达是重放：一笔都不写，余额一分不动。
func TestPostTransactionReplayWritesNothing(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "5")
	txn := topupTxn(t, ledger.NewTxnID(), userID, "100", "manual", "ORD-2")

	if _, err := s.PostTransaction(ctx, txn); err != nil {
		t.Fatalf("第一次入账失败: %v", err)
	}
	applied, err := s.PostTransaction(ctx, txn)
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if applied {
		t.Error("重放应当返回 applied=false")
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "105.00000000" {
		t.Errorf("重放之后余额 = %s，期望仍是 105.00000000", balance)
	}
	if got := entriesOf(t, db, txn.TxnID); len(got) != 2 {
		t.Errorf("重放之后这个交易有 %d 条分录，期望 2 条", len(got))
	}
}

// 同一个交易号配不同的金额不是重放，是冲突：报出来，且一笔都不写。
//
// 静默放过会让第二笔钱在账面上消失，而账本本身仍然平——事后没有线索。
func TestPostTransactionConflictOnDifferentContent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "5")
	txnID := ledger.NewTxnID()

	if _, err := s.PostTransaction(ctx, topupTxn(t, txnID, userID, "100", "manual", "ORD-3")); err != nil {
		t.Fatalf("第一次入账失败: %v", err)
	}

	_, err := s.PostTransaction(ctx, topupTxn(t, txnID, userID, "150", "manual", "ORD-3"))
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("内容不同的同一交易号应当返回 ErrTxnConflict，得到 %v", err)
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "105.00000000" {
		t.Errorf("冲突之后余额 = %s，期望仍是 105.00000000", balance)
	}
	got := entriesOf(t, db, txnID)
	if len(got) != 2 {
		t.Fatalf("冲突之后这个交易有 %d 条分录，期望 2 条：%+v", len(got), got)
	}
	if got[1].Amount != "100.00000000" {
		t.Errorf("库里那笔的金额 = %s，期望仍是 100.00000000（没被改写成 150）", got[1].Amount)
	}
}

// 同一个交易号、金额也一样，只换了**用户**：仍然是冲突，不是重放。
//
// 这一条钉的是幂等判据里的**科目**。把 entryFingerprint 里的科目拿掉之后
// （「科目反正由 txn_id 决定」那种重构），这条会变成一次重放：命令退出 0、
// 输出「已经入过账，本次未改动」，而第二个用户的钱一分没写——正是
// ErrTxnConflict 注释里说要防的那种「在账面上消失」。
//
// 只换金额的那条用例照不到它：两者的金额相同，指纹里只剩科目不同。
func TestPostTransactionConflictOnDifferentUser(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	alice := seed(t, db, "5")
	bob := seed(t, db, "5")
	txnID := ledger.NewTxnID()

	if _, err := s.PostTransaction(ctx, topupTxn(t, txnID, alice, "100", "manual", "ORD-7")); err != nil {
		t.Fatalf("第一次入账失败: %v", err)
	}

	_, err := s.PostTransaction(ctx, topupTxn(t, txnID, bob, "100", "manual", "ORD-7"))
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("只换用户的同一交易号应当返回 ErrTxnConflict，得到 %v", err)
	}

	if balance, _ := walletOf(t, db, bob); balance != "5.00000000" {
		t.Errorf("第二个用户的余额 = %s，期望仍是 5.00000000（这笔钱没写给他）", balance)
	}
	if balance, _ := walletOf(t, db, alice); balance != "105.00000000" {
		t.Errorf("第一个用户的余额 = %s，期望仍是 105.00000000", balance)
	}
	got := entriesOf(t, db, txnID)
	if len(got) != 2 {
		t.Fatalf("冲突之后这个交易有 %d 条分录，期望 2 条：%+v", len(got), got)
	}
	bobAccount := ledger.UserAccount(bob).String()
	for _, e := range got {
		if e.Account == bobAccount {
			t.Errorf("库里多了一条记在第二个用户名下的分录：%+v", e)
		}
	}
}

// 同一个交易号、金额与用户都一样，只换了**通道**：同样是冲突。
//
// 与上一条同一个形状：通道也是科目的一部分，而科目是指纹的一部分。
func TestPostTransactionConflictOnDifferentGateway(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "5")
	txnID := ledger.NewTxnID()

	if _, err := s.PostTransaction(ctx, topupTxn(t, txnID, userID, "100", "manual", "ORD-8")); err != nil {
		t.Fatalf("第一次入账失败: %v", err)
	}

	_, err := s.PostTransaction(ctx, topupTxn(t, txnID, userID, "100", "alipay", "ORD-8"))
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("只换通道的同一交易号应当返回 ErrTxnConflict，得到 %v", err)
	}

	if balance, _ := walletOf(t, db, userID); balance != "105.00000000" {
		t.Errorf("冲突之后余额 = %s，期望仍是 105.00000000", balance)
	}
	got := entriesOf(t, db, txnID)
	if len(got) != 2 {
		t.Fatalf("冲突之后这个交易有 %d 条分录，期望 2 条：%+v", len(got), got)
	}
	if got[0].Account != ledger.PaymentGatewayAccount("manual").String() {
		t.Errorf("库里那笔的通道科目 = %s，期望仍是 payment_gateway:manual", got[0].Account)
	}
}

// 没有钱包行时返回 ErrNoWallet，而不是「余额不足」。
//
// 一次入账的金额是正的，缺行会被 applyToWallets 的 WHERE 挡下并报成
// 「余额不足以扣 -100」——那是用户欠费，与「还没开户」的下一步动作完全不同。
func TestPostTransactionWithoutWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)
	txn := topupTxn(t, ledger.NewTxnID(), userID, "100", "manual", "ORD-4")

	_, err := s.PostTransaction(ctx, txn)
	if !errors.Is(err, ErrNoWallet) {
		t.Fatalf("没有钱包行时应当返回 ErrNoWallet，得到 %v", err)
	}
	if got := entriesOf(t, db, txn.TxnID); len(got) != 0 {
		t.Errorf("被拒的入账留下了 %d 条分录，期望 0 条：%+v", len(got), got)
	}
}

// 不自洽的交易在落库之前就被 Verify 挡下。
func TestPostTransactionRejectsUnbalanced(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "5")
	amt, err := money.Parse("100")
	if err != nil {
		t.Fatalf("解析金额失败: %v", err)
	}
	txnID := ledger.NewTxnID()
	// 只有借方、没有对家：账不平。
	txn := ledger.Transaction{
		TxnID: txnID,
		Entries: []ledger.Entry{{
			TxnID: txnID, Account: ledger.UserAccount(userID), Asset: ledger.AssetCNY,
			Amount: amt, EntryType: ledger.EntryTopup,
		}},
	}

	if _, err := s.PostTransaction(ctx, txn); err == nil {
		t.Fatal("不平的交易应当被拒")
	}
	balance, _ := walletOf(t, db, userID)
	if balance != "5.00000000" {
		t.Errorf("被拒的交易动了余额：%s，期望 5.00000000", balance)
	}
	if got := entriesOf(t, db, txnID); len(got) != 0 {
		t.Errorf("被拒的交易留下了 %d 条分录，期望 0 条", len(got))
	}
}

// 同一个交易号并发到达多次：只入账一次。
//
// ledger_entries 上没有 txn_id 的唯一约束，「先查再插」中间有窗口，所以
// 挡住它的是事务级 advisory lock。
//
// **它只是概率性的**：并发能不能真的重叠取决于调度（见 LESSONS 第 34 条），
// 而这里能做的只有同时起步、多来几个。实测去掉那把锁之后它多数运行会红
// （8 个 goroutine 时 5 次里红 3 次），但**不是每次都红**。真正确定性地
// 钉住那把锁的是下面 TestPostTransactionWaitsOnTxnLock。
func TestPostTransactionConcurrentSameTxn(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "5")
	txn := topupTxn(t, ledger.NewTxnID(), userID, "100", "manual", "ORD-5")

	const goroutines = 8
	applied := make([]bool, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	// 闸门：所有 goroutine 同时起步，让它们尽量真的重叠。
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			applied[i], errs[i] = s.PostTransaction(ctx, txn)
		}()
	}
	close(start)
	wg.Wait()

	appliedCount := 0
	for i := range goroutines {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发入账失败: %v", i, errs[i])
		}
		if applied[i] {
			appliedCount++
		}
	}
	if appliedCount != 1 {
		t.Errorf("applied=true 的有 %d 次，期望恰好 1 次", appliedCount)
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "105.00000000" {
		t.Errorf("余额 = %s，期望 105.00000000（只加一次）", balance)
	}
	if got := entriesOf(t, db, txn.TxnID); len(got) != 2 {
		t.Errorf("这个交易有 %d 条分录，期望 2 条", len(got))
	}
}

// 同一个交易号已经被别人锁着时，PostTransaction 会**等**，而不是照写。
//
// 与上面那条并发用例的区别是确定性：这里由用例自己在一个事务里先拿住同一把
// advisory lock，然后看 PostTransaction 会不会立刻完成。把锁那一句删掉之后，
// 它会立刻写完并返回，这条用例必红——而那条并发用例只**多半**会红。
//
// 它按 key 去拿锁，所以也钉住了「锁挂在哪个键上」：键换成别的东西（例如
// 进程内的 mutex），跨进程的那条路径就不再被覆盖，而这条用例会红。
func TestPostTransactionWaitsOnTxnLock(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "5")
	txn := topupTxn(t, ledger.NewTxnID(), userID, "100", "manual", "ORD-6")

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启持锁事务失败: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		string(txn.TxnID)); err != nil {
		t.Fatalf("用例自己拿锁失败: %v", err)
	}

	var (
		postErr error
		applied bool
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		applied, postErr = s.PostTransaction(ctx, txn)
	}()

	select {
	case <-done:
		t.Fatal("同一个交易号已经被锁着时，PostTransaction 不该完成：那把事务级 advisory lock 没生效")
	case <-time.After(200 * time.Millisecond):
	}

	// 放锁之后它应当接着跑完——等待而不是放弃，也不是报错。
	if err := holder.Rollback(); err != nil {
		t.Fatalf("释放持锁事务失败: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("放锁之后 PostTransaction 没有接着跑完")
	}
	if postErr != nil {
		t.Fatalf("入账失败: %v", postErr)
	}
	if !applied {
		t.Error("等到的这一次应当是真正入账的那一次（applied=true）")
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "105.00000000" {
		t.Errorf("余额 = %s，期望 105.00000000", balance)
	}
	if got := entriesOf(t, db, txn.TxnID); len(got) != 2 {
		t.Errorf("这个交易有 %d 条分录，期望 2 条", len(got))
	}
}
