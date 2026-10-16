package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

func reservation(t *testing.T, userID int64, requestID, amount string, asset string) *pipeline.Reservation {
	t.Helper()
	amt, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析金额失败: %v", err)
	}
	if asset == "" {
		asset = "CNY"
	}
	return &pipeline.Reservation{
		RequestID: requestID,
		UserID:    userID,
		Asset:     asset,
		Amount:    amt,
	}
}

// reservationWithRule 造一条**带真实规则 id** 的预留。
//
// 默认那条不记规则（RuleID 为 0，落库成 NULL）：绝大多数用例关心的是
// 钱怎么动，不该为了满足外键先造一条规则。但规则快照是账单可解释性的
// 依据，所以单独有一条用例覆盖它能正确落库。
func reservationWithRule(t *testing.T, userID int64, requestID, amount string, ruleID int64) *pipeline.Reservation {
	t.Helper()
	r := reservation(t, userID, requestID, amount, "CNY")
	r.RuleID = ruleID
	r.PricingExprSnap = `{"prompt": 0.000001}`
	return r
}

func testRequest(userID int64, requestID string) pipeline.Request {
	return pipeline.Request{
		RequestID:      requestID,
		UserID:         userID,
		CanonicalModel: "gpt-4o",
		InputTokens:    1000,
		MaxTokens:      500,
	}
}

// ── 预留 ────────────────────────────────────────────────────────────────

func TestSaveReservationFreezesFunds(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	res, err := s.SaveReservation(ctx, testRequest(userID, reqID(t)), reservation(t, userID, reqID(t), "30", "CNY"))
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if res == 0 {
		t.Error("应返回预留记录 id")
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("余额被改动了: %s", balance)
	}
	if frozen != "30.00000000" {
		t.Errorf("冻结 = %s，期望 30", frozen)
	}
}

// 预留**不动余额**，只动冻结。余额与冻结分开是「可用 = 余额 − 冻结」
// 这个判断的前提。
func TestReservationDoesNotTouchBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	userID := seed(t, db, "50")

	req := testRequest(userID, reqID(t))
	if _, err := s.SaveReservation(context.Background(), req, reservation(t, userID, req.RequestID, "20", "CNY")); err != nil {
		t.Fatal(err)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "20.00000000" {
		t.Errorf("冻结 = %s", frozen)
	}
}

// 可用余额不足时预留失败，且**什么都没改**。
//
// 这里用的是「更新影响 0 行」这条判据，所以它同时验证了那条 SQL 的条件。
func TestReservationInsufficientBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	// 余额 10，已经冻了 8，再想冻 5 —— 可用只有 2
	userID := seed(t, db, "10")
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET frozen = 8 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	_, err := s.SaveReservation(ctx, testRequest(userID, reqID(t)), reservation(t, userID, reqID(t), "5", "CNY"))
	if err == nil {
		t.Fatal("可用余额不足应当失败")
	}
	if !errors.Is(err, ErrInsufficientAvailable) {
		t.Errorf("期望 ErrInsufficientAvailable，得到 %v", err)
	}

	// 冻结额没变
	if _, frozen := walletOf(t, db, userID); frozen != "8.00000000" {
		t.Errorf("失败时不该改动冻结额，得到 %s", frozen)
	}
	// 也没留下预留记录
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_requests WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("失败时不该留下预留记录，实际 %d 条", n)
	}
}

// 恰好够用应当成功——边界。
func TestReservationExactBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	userID := seed(t, db, "10")

	req := testRequest(userID, reqID(t))
	if _, err := s.SaveReservation(context.Background(), req, reservation(t, userID, req.RequestID, "10", "CNY")); err != nil {
		t.Errorf("恰好够用应当成功: %v", err)
	}
}

// 没有钱包行时给出可区分的错误。
func TestReservationWithoutWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)

	// 建用户但不建钱包
	id := 900000 + nextID()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO users (id, username) VALUES ($1, $2)`, id, "u"+itoa(id)); err != nil {
		t.Fatal(err)
	}

	req := testRequest(id, reqID(t))
	_, err := s.SaveReservation(context.Background(), req, reservation(t, id, req.RequestID, "1", "CNY"))
	if err == nil {
		t.Fatal("没有钱包应当失败")
	}
	// 「用户不存在」与「钱不够」要能分开——处置完全不同
	if errors.Is(err, ErrInsufficientAvailable) {
		t.Errorf("应报告「没有钱包」而不是「余额不足」: %v", err)
	}
}

// 同一个 request_id 重复预留要被唯一键挡下。
func TestDuplicateReservationRejected(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	if _, err := s.SaveReservation(ctx, testRequest(userID, requestID), reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveReservation(ctx, testRequest(userID, requestID), reservation(t, userID, requestID, "10", "CNY"))
	if err == nil {
		t.Fatal("重复的 request_id 应当被唯一键挡下")
	}
}

// ── 结算 ────────────────────────────────────────────────────────────────

func TestSaveSettlementMovesMoney(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}

	// 实际只花了 12，比预留少
	entries := consumeEntries(t, userID, "12")
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: entries,
	}
	charge.Settlement.Actual = money.MustParse("12")

	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "88.00000000" {
		t.Errorf("余额 = %s，期望 88（100 − 12）", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("预留应当被释放，冻结 = %s", frozen)
	}

	// 钱包余额必须等于账本推出来的——这是「快照是账本的具体化物」
	if got, want := balance, ledgerBalance(t, db, userID).String(); got != want {
		t.Errorf("钱包 %s 与账本 %s 不一致", got, want)
	}
}

// # 幂等
//
// 上游超时、进程重启、队列重投都会让同一次结算到达两次，
// 而重复扣费是用户立刻能发现的错。
func TestSettlementIsIdempotent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: consumeEntries(t, userID, "12"),
	}
	charge.Settlement.Actual = money.MustParse("12")

	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	// 重放
	err := s.Save(ctx, req, charge)
	if !errors.Is(err, ErrAlreadySettled) {
		t.Errorf("重放应当返回 ErrAlreadySettled，得到 %v", err)
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "88.00000000" {
		t.Errorf("重放后余额 = %s，期望仍是 88（只扣一次）", balance)
	}
}

// 并发重放也只能扣一次。
//
// 这条与上一条不同：它压的是「幂等判断在加锁之后」这个顺序。
// 判断放在加锁之前的话，两个并发重放会各自看到「还没结算」。
func TestConcurrentSettlementChargesOnce(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "50", "CNY")); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		okCount  int
		dupCount int
		otherErr error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			charge := &pipeline.Charge{
				RequestID: requestID, UserID: userID, Asset: "CNY",
				Entries: consumeEntries(t, userID, "10"),
			}
			charge.Settlement.Actual = money.MustParse("10")

			err := s.Save(ctx, req, charge)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrAlreadySettled):
				dupCount++
			default:
				otherErr = err
			}
		}()
	}
	wg.Wait()

	if otherErr != nil {
		t.Fatalf("并发结算出错: %v", otherErr)
	}
	if okCount != 1 {
		t.Errorf("成功结算 %d 次，期望恰好 1 次", okCount)
	}
	if dupCount != n-1 {
		t.Errorf("重复结算 %d 次，期望 %d 次", dupCount, n-1)
	}

	balance, _ := walletOf(t, db, userID)
	if balance != "90.00000000" {
		t.Errorf("余额 = %s，期望 90（只扣一次）", balance)
	}
}

// 扣款会让余额变负时必须报错，而不是夹到零。
func TestSettlementRefusesNegativeBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	// 预留 10，但结算 150 —— 预留机制本该不让这事发生，
	// 真发生说明有别的路径动了钱包或预留算错了。
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: consumeEntries(t, userID, "150"),
	}
	charge.Settlement.Actual = money.MustParse("150")

	err := s.Save(ctx, req, charge)
	if err == nil {
		t.Fatal("扣款会让余额变负，应当失败")
	}
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Errorf("期望 ErrInsufficientBalance，得到 %v", err)
	}

	// 失败时整个事务回滚：余额与冻结都没变
	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" || frozen != "10.00000000" {
		t.Errorf("失败时应当整体回滚，得到 balance=%s frozen=%s", balance, frozen)
	}

	// 账本里不该留下那组分录。seed 造过一笔 100 的充值，所以期望值
	// 是**充值额**而不是零——按零断言会把「充值也没了」也当成通过。
	if got := ledgerBalance(t, db, userID); got.Cmp(money.MustParse("100")) != 0 {
		t.Errorf("失败时账本不该有变化，期望仍是充值的 100，得到 %s", got)
	}
}

// ── 释放 ────────────────────────────────────────────────────────────────

func TestReleaseUnfreezes(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("释放不该动余额: %s", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("冻结应当释放: %s", frozen)
	}
}

// 释放是幂等的，且**不写分录**——它没有发生资金流动。
func TestReleaseIsIdempotentAndWritesNoEntry(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Release(ctx, req); err != nil {
			t.Fatalf("第 %d 次释放失败: %v", i+1, err)
		}
	}

	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("重复释放不该把冻结扣成负数: %s", frozen)
	}
	// seed 造过一条充值分录，所以基线是 1 而不是 0
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("释放不该写分录（账本只记资金流动），期望仍只有 seed 的那 1 条，实际 %d 条", n)
	}
}

// 释放一个不存在的预留不是错误：目的已经达到了。
func TestReleaseUnknownRequest(t *testing.T) {
	db := testDB(t)
	s := New(db)
	if err := s.Release(context.Background(), testRequest(1, reqID(t))); err != nil {
		t.Errorf("释放不存在的预留不该报错: %v", err)
	}
}

// 已经结算过的请求不能再释放——那会把冻结额扣两次。
func TestReleaseAfterSettlementIsNoop(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: consumeEntries(t, userID, "12"),
	}
	charge.Settlement.Actual = money.MustParse("12")
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	if err := s.Release(ctx, req); err != nil {
		t.Fatalf("结算后释放不该报错: %v", err)
	}

	// 余额与冻结都保持结算后的状态
	balance, frozen := walletOf(t, db, userID)
	if balance != "88.00000000" || frozen != "0.00000000" {
		t.Errorf("结算后释放应当是空操作，得到 balance=%s frozen=%s", balance, frozen)
	}
}

// ── 对账 ────────────────────────────────────────────────────────────────

// 钱包快照必须等于账本推出来的值。
//
// 这是「快照是具体化物、账本是权威」这条关系的可执行检查。
func TestWalletMatchesLedger(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")

	for i := 0; i < 5; i++ {
		requestID := reqID(t)
		req := testRequest(userID, requestID)
		if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "100", "CNY")); err != nil {
			t.Fatal(err)
		}
		amount := money.FromInt(int64(10 + i))
		charge := &pipeline.Charge{
			RequestID: requestID, UserID: userID, Asset: "CNY",
			Entries: consumeEntries(t, userID, amount.String()),
		}
		charge.Settlement.Actual = amount
		if err := s.Save(ctx, req, charge); err != nil {
			t.Fatal(err)
		}
	}

	balance, _ := walletOf(t, db, userID)
	want := ledgerBalance(t, db, userID)
	if money.MustParse(balance).Cmp(want) != 0 {
		t.Errorf("钱包 %s 与账本 %s 不一致", balance, want)
	}
	// 1000 − (10+11+12+13+14) = 940
	if balance != "940.00000000" {
		t.Errorf("余额 = %s，期望 940", balance)
	}
}

// ── 死锁 ────────────────────────────────────────────────────────────────

// 两个并发的多用户交易，若加锁顺序不固定就会死锁。
//
// 这里让两个 goroutine 以**相反的用户顺序**造分录，而 store 内部会按
// 排序后的顺序拿锁。跑若干轮，任何一次死锁都会让 PostgreSQL 报错。
func TestConcurrentMultiWalletNoDeadlock(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	// 一批用户，每个都有余额
	users := make([]int64, 6)
	for i := range users {
		users[i] = seed(t, db, "1000")
	}

	const rounds = 12
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for r := 0; r < rounds; r++ {
		// 一轮里两个方向相反的交易
		for _, order := range [][]int64{users, reverse(users)} {
			wg.Add(1)
			go func(order []int64) {
				defer wg.Done()
				requestID := reqID(t)
				// 用第一个用户作为「请求主体」，但分录覆盖全部用户
				req := testRequest(order[0], requestID)
				if _, err := s.SaveReservation(ctx, req, reservation(t, order[0], requestID, "1", "CNY")); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				charge := &pipeline.Charge{
					RequestID: requestID, UserID: order[0], Asset: "CNY",
					Entries: multiUserEntries(t, order),
				}
				charge.Settlement.Actual = money.FromInt(1)
				if err := s.Save(ctx, req, charge); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}(order)
		}
	}
	wg.Wait()

	for _, err := range errs {
		if err == nil {
			continue
		}
		// 死锁的报错文本里带 "deadlock"
		if isDeadlock(err) {
			t.Fatalf("出现死锁——加锁顺序没有固定住: %v", err)
		}
		t.Errorf("并发交易出错: %v", err)
	}
}

// multiUserEntries 造一组「从每个用户各扣一点、汇总到平台」的分录。
//
// 用户按传入顺序出现，于是不同的调用方会给出不同的科目顺序——
// 这正是死锁的温床。
func multiUserEntries(t *testing.T, users []int64) []ledger.Entry {
	t.Helper()
	txn := ledger.NewTxnID()
	one := money.MustParse("0.1")

	var out []ledger.Entry
	total := money.Zero
	for _, id := range users {
		out = append(out, ledger.Entry{
			TxnID: txn, Account: ledger.UserAccount(id), Asset: ledger.AssetCNY,
			Amount: one.Neg(), EntryType: ledger.EntryConsume,
		})
		total = total.Add(one)
	}
	out = append(out, ledger.Entry{
		TxnID: txn, Account: ledger.PlatformRevenueAccount(), Asset: ledger.AssetCNY,
		Amount: total, EntryType: ledger.EntryConsume,
	})
	return out
}

func reverse(v []int64) []int64 {
	out := make([]int64, len(v))
	for i := range v {
		out[i] = v[len(v)-1-i]
	}
	return out
}

func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock") || strings.Contains(msg, "40p01")
}

// ── 精度 ────────────────────────────────────────────────────────────────

// 金额经 numeric(20,8) 往返之后必须一分不差。
//
// 这一层是 Go 的定点数与数据库的 numeric 之间唯一的转换点，
// 精度在这里丢了的话，账目会慢慢对不上而没有任何信号。
func TestAmountPrecisionRoundTrip(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")

	// 带满 8 位小数的金额
	const amount = "0.00000001"
	requestID := reqID(t)
	req := testRequest(userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, amount, "CNY")); err != nil {
		t.Fatal(err)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: consumeEntries(t, userID, amount),
	}
	charge.Settlement.Actual = money.MustParse(amount)
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	balance, _ := walletOf(t, db, userID)
	want := money.MustParse("1000").Sub(money.MustParse(amount))
	if money.MustParse(balance).Cmp(want) != 0 {
		t.Errorf("余额 = %s，期望 %s", balance, want)
	}
}

// 大额也要精确。
func TestLargeAmountPrecision(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	const start = "99999999.99999999"
	userID := seed(t, db, start)

	requestID := reqID(t)
	req := testRequest(userID, requestID)
	const amount = "12345678.87654321"
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, amount, "CNY")); err != nil {
		t.Fatal(err)
	}
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries: consumeEntries(t, userID, amount),
	}
	charge.Settlement.Actual = money.MustParse(amount)
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatal(err)
	}

	balance, _ := walletOf(t, db, userID)
	want := money.MustParse(start).Sub(money.MustParse(amount))
	if money.MustParse(balance).Cmp(want) != 0 {
		t.Errorf("余额 = %s，期望 %s", balance, want)
	}
}

// reqID 生成一个唯一的请求标识。
func reqID(t *testing.T) string {
	t.Helper()
	return "req-" + itoa(nextID())
}

// 规则快照要能正确落库——它是账单可解释性的依据。
//
// 规则事后可能被改，而账单必须能回答「当时按哪条规则算的」。
// 这条也顺带验证了 pricing_rule_id 的外键约束真的生效
// （前面那条用例就是因为这个约束才发现自己漏造了规则）。
func TestReservationRecordsPricingRule(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	// 造一条真规则
	var ruleID int64
	if ruleErr := db.QueryRowContext(ctx, `
		INSERT INTO pricing_rules (scope, name, expr, priority)
		VALUES ('platform', 'placeholder-rule', '{"prompt": 0.000001}', 0)
		RETURNING id`).Scan(&ruleID); ruleErr != nil {
		t.Fatalf("造规则失败: %v", ruleErr)
	}

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req,
		reservationWithRule(t, userID, requestID, "10", ruleID)); err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	var gotRule int64
	var gotExpr sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT pricing_rule_id, pricing_expr_snapshot FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&gotRule, &gotExpr)
	if err != nil {
		t.Fatal(err)
	}
	if gotRule != ruleID {
		t.Errorf("规则 id = %d，期望 %d", gotRule, ruleID)
	}
	if !gotExpr.Valid || gotExpr.String == "" {
		t.Error("表达式快照应当落库——规则事后可变，账单要能解释当时算的是什么")
	}
}

// 不记规则时该字段是 NULL，而不是 0。
//
// 0 会被读成「规则 0」——一个不存在的规则，而 NULL 才是「没有规则」。
func TestReservationWithoutRuleStoresNull(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(userID, requestID)

	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}

	var ruleID sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT pricing_rule_id FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	if ruleID.Valid {
		t.Errorf("没记规则时应当是 NULL，得到 %d", ruleID.Int64)
	}
}
