package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	ledgerstore "github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/testdb"
)

// TestMain 给这个包建一个独立的库（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// stubName 是用例里那条通道的名字。
const stubName = "stub"

// stubChannel 把用例给的通知原样交回来。
//
// 这一层要测的是 Service 的顺序与判据，不是签名或 HTTP 形状，所以通道只需
// 满足 Channel 接口。真实的假通道由 fakechannel 自己的用例覆盖。
type stubChannel struct {
	created CreatedOrder
	// omitOrderNo 让通道在建单回执里不给单号：Service 应当拿请求里的那一个补上。
	omitOrderNo bool
}

func (c *stubChannel) Name() string { return stubName }

func (c *stubChannel) CreateOrder(_ context.Context, req CreateOrderRequest) (CreatedOrder, error) {
	out := c.created
	if !c.omitOrderNo && out.OrderNo == "" {
		out.OrderNo = req.OrderNo
	}
	return out, nil
}

func (c *stubChannel) ParseNotification(context.Context, NotificationRequest) (Notification, error) {
	return Notification{}, errors.New("stub 通道不解析通知")
}

func (c *stubChannel) QueryOrder(_ context.Context, orderNo string) (ChannelOrder, error) {
	return ChannelOrder{OrderNo: orderNo}, nil
}

// harness 是一组连上真库的 Service 与它的依赖。
type harness struct {
	db      *sql.DB
	service *Service
	store   *Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithChannel(t, &stubChannel{})
}

// newHarnessWithChannel 用一条指定的通道装出 harness。
func newHarnessWithChannel(t *testing.T, channel Channel) *harness {
	t.Helper()
	db := testdb.Open(t)
	orders := NewStore(db)
	return &harness{
		db:      db,
		service: NewService(orders, ledgerstore.New(db), map[string]Channel{stubName: channel}),
		store:   orders,
	}
}

// 每个用例用独立的用户 id；测试库每次运行都重建，所以一个进程内计数器就够。
var userSeq atomic.Int64

// seedUser 建一个用户与一个 0 余额的钱包行。
//
// 钱包行走一遍 INSERT 而不是调 wallet.Create：本包要的是「账本能写进这个用户」，
// 而那条命令自己会写快照、还会要求别的权限面。0 余额与「账本上还没有分录」
// 是同一种状态，不变量从一开始就成立。
func seedUser(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	id := seedUserWithoutWallet(t, ctx, db)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_wallets (user_id, asset, balance, frozen) VALUES ($1, 'CNY', 0, 0)`,
		id); err != nil {
		t.Fatalf("建钱包失败: %v", err)
	}
	return id
}

// seedUserWithoutWallet 只建用户，不给他开钱包。
func seedUserWithoutWallet(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	id := 700000 + userSeq.Add(1)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "pay"+strconv.FormatInt(id, 10), "pay"+strconv.FormatInt(id, 10)+"@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// createOrder 走一遍 Service.Create 并返回落库的那一行。
func (h *harness) createOrder(t *testing.T, ctx context.Context, userID int64, orderNo, amount string) Order {
	t.Helper()
	result, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: orderNo,
		UserID:  userID,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse(amount),
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	return result.Order
}

// notify 发一份已支付通知。
func (h *harness) notify(ctx context.Context, orderNo string, paidAt time.Time) (NotificationResult, error) {
	return h.service.Notify(ctx, stubName, Notification{
		OrderNo:      orderNo,
		GatewayTxnID: "TRADE-" + orderNo,
		Status:       StatusPaid,
		Amount:       money.MustParse("66.00"),
		Asset:        ledger.AssetCNY,
		Fee:          money.MustParse("0.66"),
		PaidAt:       paidAt,
	})
}

// entriesOf 读一笔交易的分录，按科目排好再返回。
func entriesOf(t *testing.T, ctx context.Context, db *sql.DB, txnID ledger.TxnID) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT account, amount::text FROM ledger_entries
		 WHERE txn_id = $1 ORDER BY account`, string(txnID))
	if err != nil {
		t.Fatalf("读分录失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var account, amount string
		if err := rows.Scan(&account, &amount); err != nil {
			t.Fatalf("读分录失败: %v", err)
		}
		out[account] = amount
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读分录失败: %v", err)
	}
	return out
}

// entryCount 数一笔交易写了几条分录。
func entryCount(t *testing.T, ctx context.Context, db *sql.DB, txnID ledger.TxnID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1`, string(txnID)).Scan(&n); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	return n
}

// walletBalance 读 user_wallets 里的快照余额。
func walletBalance(t *testing.T, ctx context.Context, db *sql.DB, userID int64) string {
	t.Helper()
	var balance string
	if err := db.QueryRowContext(ctx,
		`SELECT balance::text FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		userID).Scan(&balance); err != nil {
		t.Fatalf("读钱包余额失败: %v", err)
	}
	return balance
}

// TestPaidNotificationPostsTopupAndMarksPaid 是这条链的主路径。
func TestPaidNotificationPostsTopupAndMarksPaid(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-PAID-1", "66.00")

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	result, err := h.notify(ctx, "ORD-PAID-1", paidAt)
	if err != nil {
		t.Fatalf("处理通知失败: %v", err)
	}
	if !result.Applied {
		t.Error("首次通知应当写入账本")
	}
	if result.Ignored {
		t.Error("首次通知不该被当成「已放过」")
	}
	if result.Order.Status != StatusPaid {
		t.Errorf("支付单状态 = %q，期望 %q", result.Order.Status, StatusPaid)
	}
	if result.Order.GatewayTxnID != "TRADE-ORD-PAID-1" {
		t.Errorf("通道流水 = %q，期望 %q", result.Order.GatewayTxnID, "TRADE-ORD-PAID-1")
	}
	if result.Order.Fee.Cmp(money.MustParse("0.66")) != 0 {
		t.Errorf("手续费 = %s，期望 0.66", result.Order.Fee)
	}
	if result.Order.PaidAt == nil || !result.Order.PaidAt.Equal(paidAt) {
		t.Errorf("支付时刻 = %v，期望 %v", result.Order.PaidAt, paidAt)
	}

	amount := money.MustParse("66.00")
	fee := money.MustParse("0.66")
	want := map[string]string{
		ledger.UserAccount(userID).String():             amount.String(),
		ledger.PaymentGatewayAccount(stubName).String(): amount.Neg().String(),
		ledger.UpstreamCostAccount().String():           fee.String(),
		ledger.PlatformRevenueAccount().String():        fee.Neg().String(),
	}
	got := entriesOf(t, ctx, h.db, TxnIDForOrder("ORD-PAID-1"))
	if len(got) != len(want) {
		t.Fatalf("分录 %d 条，期望 %d 条：%v", len(got), len(want), got)
	}
	for account, wantAmount := range want {
		if got[account] != wantAmount {
			t.Errorf("科目 %s 的金额 = %q，期望 %q", account, got[account], wantAmount)
		}
	}
	if balance := walletBalance(t, ctx, h.db, userID); balance != amount.String() {
		t.Errorf("钱包余额 = %s，期望 %s", balance, amount)
	}
}

// TestReplayedNotificationChangesNothing 验「同一份通知再来一次」。
func TestReplayedNotificationChangesNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-REPLAY-1", "66.00")

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	if _, err := h.notify(ctx, "ORD-REPLAY-1", paidAt); err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}
	txnID := TxnIDForOrder("ORD-REPLAY-1")
	before := entriesOf(t, ctx, h.db, txnID)

	result, err := h.notify(ctx, "ORD-REPLAY-1", paidAt)
	if err != nil {
		t.Fatalf("重放通知失败: %v", err)
	}
	if result.Applied {
		t.Error("重放不该再写分录")
	}
	if result.Order.Status != StatusPaid {
		t.Errorf("状态 = %q，期望仍是 paid", result.Order.Status)
	}

	after := entriesOf(t, ctx, h.db, txnID)
	if len(after) != len(before) {
		t.Fatalf("重放后分录 %d 条，重放前 %d 条", len(after), len(before))
	}
	for account, amount := range before {
		if after[account] != amount {
			t.Errorf("重放改动了科目 %s：%q → %q", account, amount, after[account])
		}
	}
	if balance := walletBalance(t, ctx, h.db, userID); balance != money.MustParse("66.00").String() {
		t.Errorf("重放后余额 = %s，期望 66.00", balance)
	}
}

// TestNotificationWithDifferentFeeConflicts 验「同键不同内容」。
//
// 已支付的单子再收到费用不同的通知时，账本那一层会把它认成「同一个交易号、
// 内容不一样」，本包把它归成冲突。这句判据只在账本那一层有，所以这条用例
// 同时证明服务端确实把重放也送进了 PostTransaction。
func TestNotificationWithDifferentFeeConflicts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-CONFLICT-FEE", "66.00")

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	if _, err := h.notify(ctx, "ORD-CONFLICT-FEE", paidAt); err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}

	_, err := h.service.Notify(ctx, stubName, Notification{
		OrderNo:      "ORD-CONFLICT-FEE",
		GatewayTxnID: "TRADE-ORD-CONFLICT-FEE",
		Status:       StatusPaid,
		Amount:       money.MustParse("66.00"),
		Asset:        ledger.AssetCNY,
		Fee:          money.MustParse("1.00"),
		PaidAt:       paidAt,
	})
	if !errors.Is(err, ErrOrderConflict) {
		t.Fatalf("错误 = %v，期望 ErrOrderConflict", err)
	}
	const phrase = "已经存在，但内容与这次要写的不同"
	if !strings.Contains(err.Error(), phrase) {
		t.Errorf("错误里应当带着账本那一层的冲突原话 %q，实际是 %q", phrase, err)
	}
	if n := entryCount(t, ctx, h.db, TxnIDForOrder("ORD-CONFLICT-FEE")); n != 4 {
		t.Errorf("冲突后分录 %d 条，期望仍是 4 条", n)
	}
}

// TestTerminalOrderNotificationLeavesLedgerAlone 验状态机只往前走。
func TestTerminalOrderNotificationLeavesLedgerAlone(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-TERMINAL-1", "66.00")

	failed, err := h.service.Notify(ctx, stubName, Notification{
		OrderNo: "ORD-TERMINAL-1",
		Status:  StatusFailed,
		Amount:  money.MustParse("66.00"),
		Asset:   ledger.AssetCNY,
	})
	if err != nil {
		t.Fatalf("处理失败通知出错: %v", err)
	}
	if failed.Order.Status != StatusFailed {
		t.Errorf("状态 = %q，期望 failed", failed.Order.Status)
	}
	txnID := TxnIDForOrder("ORD-TERMINAL-1")
	if n := entryCount(t, ctx, h.db, txnID); n != 0 {
		t.Fatalf("失败通知写了 %d 条分录，期望 0 条", n)
	}

	paid, err := h.notify(ctx, "ORD-TERMINAL-1", time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("终态后收到支付通知出错: %v", err)
	}
	if !paid.Ignored {
		t.Error("终态后收到的支付通知应当被放过")
	}
	if paid.Order.Status != StatusFailed {
		t.Errorf("状态 = %q，期望仍是 failed", paid.Order.Status)
	}
	if n := entryCount(t, ctx, h.db, txnID); n != 0 {
		t.Errorf("终态后收到支付通知写了 %d 条分录，期望 0 条", n)
	}
	if balance := walletBalance(t, ctx, h.db, userID); balance != money.Zero.String() {
		t.Errorf("余额 = %s，期望 0", balance)
	}
}

// TestCreateReplaysOrConflictsOnTheSameOrderNo 验下单侧的幂等判据。
func TestCreateReplaysOrConflictsOnTheSameOrderNo(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-CREATE-1", "66.00")

	again, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: "ORD-CREATE-1",
		UserID:  userID,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse("66.00"),
	})
	if err != nil {
		t.Fatalf("同内容重跑下单失败: %v", err)
	}
	if again.Applied {
		t.Error("同内容重跑不该新写一行")
	}
	if again.Order.CreatedAt.IsZero() {
		t.Error("读回的那一行带着零值 created_at")
	}

	_, err = h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: "ORD-CREATE-1",
		UserID:  userID,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse("67.00"),
	})
	if !errors.Is(err, ErrOrderConflict) {
		t.Fatalf("不同金额重跑的错误 = %v，期望 ErrOrderConflict", err)
	}
}

// TestNotificationForUnknownOrderIsNotFound 验「单号必须先下过单」。
func TestNotificationForUnknownOrderIsNotFound(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	_, err := h.notify(ctx, "ORD-MISSING-1", time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC))
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("错误 = %v，期望 ErrOrderNotFound", err)
	}
}

// TestNotificationAmountMustMatchOrder 验「通知金额与单子对不上」。
func TestNotificationAmountMustMatchOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-AMOUNT-1", "66.00")

	_, err := h.service.Notify(ctx, stubName, Notification{
		OrderNo: "ORD-AMOUNT-1",
		Status:  StatusPaid,
		Amount:  money.MustParse("1.00"),
		Asset:   ledger.AssetCNY,
	})
	if !errors.Is(err, ErrOrderConflict) {
		t.Fatalf("错误 = %v，期望 ErrOrderConflict", err)
	}
	if n := entryCount(t, ctx, h.db, TxnIDForOrder("ORD-AMOUNT-1")); n != 0 {
		t.Errorf("金额对不上时写了 %d 条分录，期望 0 条", n)
	}
}

// TestCreateRequiresWallet 验「下单时钱包必须已经存在」。
//
// 不在这里拦住的话，那份失败会推迟到回调：通道已经收了钱，入账却因为缺
// 钱包行失败，只能靠重发通知碰运气。
func TestCreateRequiresWallet(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUserWithoutWallet(t, ctx, h.db)

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: "ORD-NO-WALLET-1",
		UserID:  userID,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse("66.00"),
	})
	if !errors.Is(err, ledgerstore.ErrNoWallet) {
		t.Fatalf("错误 = %v，期望 ErrNoWallet", err)
	}
	if _, getErr := h.store.Get(ctx, "ORD-NO-WALLET-1"); !errors.Is(getErr, ErrOrderNotFound) {
		t.Errorf("缺钱包时不该落下支付单，查询得到 %v", getErr)
	}
}

// TestNotificationRecoversOrderLeftInCreated 验「分录写了、单子还是 created」
// 这种状态下再收到通知会收敛。
//
// 分录与状态现在在同一个事务里，这个中间态不会再由崩溃或半途失败产生；
// 用例保留为防御性检查：状态被手工改回 created 之后，重复通知必须把它补成
// paid，且不能再写一组分录。
func TestNotificationRecoversOrderLeftInCreated(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-RECOVER-1", "66.00")

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	if _, err := h.notify(ctx, "ORD-RECOVER-1", paidAt); err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}
	if _, err := h.db.ExecContext(ctx,
		`UPDATE payment_orders SET status = 'created' WHERE order_no = $1`, "ORD-RECOVER-1"); err != nil {
		t.Fatalf("把状态改回 created 失败: %v", err)
	}

	result, err := h.notify(ctx, "ORD-RECOVER-1", paidAt)
	if err != nil {
		t.Fatalf("恢复通知失败: %v", err)
	}
	if result.Order.Status != StatusPaid {
		t.Errorf("状态 = %q，期望补成 paid", result.Order.Status)
	}
	if result.Applied {
		t.Error("恢复通知不该再写一成分录")
	}
	if n := entryCount(t, ctx, h.db, TxnIDForOrder("ORD-RECOVER-1")); n != 4 {
		t.Errorf("分录 %d 条，期望仍是 4 条", n)
	}
}

// TestConflictingNotificationsConverge 让「失败」与「已支付」两份通知并发到达。
//
// 两种先后都合法：支付先到则入账并停在 paid（失败通知随后被放过），失败先到
// 则一分钱都不记。**不合法的是中间态**——分录写了，单子却是 failed：那笔钱
// 永远补不上，因为终态会放过之后的每一份通知。这里只断言那条不变量。
func TestConflictingNotificationsConverge(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-RACE-1", "66.00")

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	var paidResult NotificationResult
	var paidErr error
	go func() {
		defer wg.Done()
		<-start
		paidResult, paidErr = h.notify(ctx, "ORD-RACE-1", time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC))
	}()

	var failedErr error
	go func() {
		defer wg.Done()
		<-start
		_, failedErr = h.service.Notify(ctx, stubName, Notification{
			OrderNo: "ORD-RACE-1",
			Status:  StatusFailed,
			Amount:  money.MustParse("66.00"),
			Asset:   ledger.AssetCNY,
		})
	}()

	close(start)
	wg.Wait()

	if paidErr != nil {
		t.Fatalf("支付通知失败: %v", paidErr)
	}
	if failedErr != nil {
		t.Fatalf("失败通知失败: %v", failedErr)
	}

	order, err := h.store.Get(ctx, "ORD-RACE-1")
	if err != nil {
		t.Fatalf("读支付单失败: %v", err)
	}
	entries := entryCount(t, ctx, h.db, TxnIDForOrder("ORD-RACE-1"))
	switch order.Status {
	case StatusPaid:
		if entries != 4 {
			t.Errorf("单子是 paid，分录却有 %d 条，期望 4 条", entries)
		}
		if !paidResult.Applied {
			t.Error("单子停在 paid，第一次支付通知应当写入了账本")
		}
	case StatusFailed:
		if entries != 0 {
			t.Errorf("单子是 failed，分录却有 %d 条——这笔钱永远补不上", entries)
		}
		if !paidResult.Ignored {
			t.Error("单子停在 failed 时，支付通知应当被放过")
		}
	default:
		t.Fatalf("并发通知后状态 = %q，期望 paid 或 failed", order.Status)
	}
}

// TestCreateFillsOrderNoWhenChannelOmitsIt 验通道不回单号时用请求里的那一个。
//
// 单号是我们给通道的，通道按它做幂等；回执里没有单号时补上请求里的，
// 不能让空单号落库——那样之后的任何通知都认不出这张单子。
func TestCreateFillsOrderNoWhenChannelOmitsIt(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithChannel(t, &stubChannel{omitOrderNo: true})
	userID := seedUser(t, ctx, h.db)

	result, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: "ORD-OMIT-1",
		UserID:  userID,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse("66.00"),
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if result.Created.OrderNo != "ORD-OMIT-1" {
		t.Errorf("通道回执里的单号 = %q，期望补成 ORD-OMIT-1", result.Created.OrderNo)
	}
	if result.Order.OrderNo != "ORD-OMIT-1" {
		t.Errorf("落库的单号 = %q，期望 ORD-OMIT-1", result.Order.OrderNo)
	}
}

// TestCreateRejectsOrderNoRewrittenByChannel 验通道改写单号时拒绝。
//
// 改写之后付款人看到的单号与库里那一行对不上，通道发来的通知也指不到这张单子。
func TestCreateRejectsOrderNoRewrittenByChannel(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithChannel(t, &stubChannel{created: CreatedOrder{OrderNo: "ORD-OTHER-1"}})
	userID := seedUser(t, ctx, h.db)

	_, err := h.service.Create(ctx, stubName, CreateOrderRequest{
		OrderNo: "ORD-REWRITE-1",
		UserID:  userID,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse("66.00"),
	})
	if err == nil {
		t.Fatal("通道改写了单号，下单却成功了")
	}
	if _, getErr := h.store.Get(ctx, "ORD-REWRITE-1"); !errors.Is(getErr, ErrOrderNotFound) {
		t.Errorf("被拒的下单不该落库，查询得到 %v", getErr)
	}
	if _, getErr := h.store.Get(ctx, "ORD-OTHER-1"); !errors.Is(getErr, ErrOrderNotFound) {
		t.Errorf("被拒的下单不该按通道的单号落库，查询得到 %v", getErr)
	}
}

// TestCreateConflictsWhenAnyComparedFieldDiffers 验下单幂等的判据逐项成立。
//
// 同一单号的四栏（用户、通道、资产、金额）任一对不上都不是重放：那说明
// 这个单号已经被另一张单子占用了。
func TestCreateConflictsWhenAnyComparedFieldDiffers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	original := Order{
		OrderNo: "ORD-CONTENT-1",
		UserID:  userID,
		Gateway: stubName,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse("66.00"),
	}
	if _, applied, err := h.store.Create(ctx, original); err != nil || !applied {
		t.Fatalf("首次写入 = (applied %v, err %v)", applied, err)
	}

	cases := []struct {
		name string
		mut  func(*Order)
	}{
		{"用户不同", func(o *Order) { o.UserID = userID + 1 }},
		{"通道不同", func(o *Order) { o.Gateway = "another" }},
		{"资产不同", func(o *Order) { o.Asset = ledger.Asset("USD") }},
		{"金额不同", func(o *Order) { o.Amount = money.MustParse("67.00") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := original
			tc.mut(&o)
			_, _, err := h.store.Create(ctx, o)
			if !errors.Is(err, ErrOrderConflict) {
				t.Errorf("错误 = %v，期望 ErrOrderConflict", err)
			}
		})
	}

	same, applied, err := h.store.Create(ctx, original)
	if err != nil {
		t.Fatalf("同内容重跑失败: %v", err)
	}
	if applied {
		t.Error("同内容重跑不该新写一行")
	}
	if same.OrderNo != original.OrderNo || same.UserID != original.UserID {
		t.Errorf("同内容重跑读回的行 = %+v，期望与首次写入一致", same)
	}
}

// TestMarkMethodsMoveCreatedOnlyOnce 验两个「改状态」的方法只动 `created` 那一行。
//
// 第一次调用返回 true，同一张单子再调一次返回 false：调用方据此知道状态
// 已经被别的通知改过了。
func TestMarkMethodsMoveCreatedOnlyOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-MARK-PAID-1", "66.00")
	h.createOrder(t, ctx, userID, "ORD-MARK-FAILED-1", "66.00")

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	paidRows := make([]bool, 2)
	if err := h.store.WithOrderLock(ctx, "ORD-MARK-PAID-1", func(ctx context.Context, locked *LockedOrder) error {
		for i := range paidRows {
			changed, markErr := locked.MarkPaid(ctx, "ORD-MARK-PAID-1", stubName, "TRADE-1", nil, paidAt, money.Zero)
			if markErr != nil {
				return markErr
			}
			paidRows[i] = changed
		}
		return nil
	}); err != nil {
		t.Fatalf("MarkPaid 失败: %v", err)
	}
	if !paidRows[0] || paidRows[1] {
		t.Errorf("两次 MarkPaid = %v，期望 [true false]", paidRows)
	}

	failedRows := make([]bool, 2)
	if err := h.store.WithOrderLock(ctx, "ORD-MARK-FAILED-1", func(ctx context.Context, locked *LockedOrder) error {
		for i := range failedRows {
			changed, markErr := locked.MarkTerminal(ctx, "ORD-MARK-FAILED-1", StatusFailed, nil)
			if markErr != nil {
				return markErr
			}
			failedRows[i] = changed
		}
		return nil
	}); err != nil {
		t.Fatalf("MarkTerminal 失败: %v", err)
	}
	if !failedRows[0] || failedRows[1] {
		t.Errorf("两次 MarkTerminal = %v，期望 [true false]", failedRows)
	}
}

// TestMarkTerminalRejectsOtherTargets 验这里只收 failed 与 expired。
//
// paid 要走 MarkPaid（它还要记流水与手续费），refunded 是已支付之后的事，
// 都不能从 created 直接跳过去。
func TestMarkTerminalRejectsOtherTargets(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-MARK-TARGET-1", "66.00")

	for _, to := range []Status{StatusPaid, StatusCreated, StatusRefunded} {
		err := h.store.WithOrderLock(ctx, "ORD-MARK-TARGET-1", func(ctx context.Context, locked *LockedOrder) error {
			_, markErr := locked.MarkTerminal(ctx, "ORD-MARK-TARGET-1", to, nil)
			return markErr
		})
		if err == nil {
			t.Errorf("MarkTerminal(%q) 成功了，期望拒绝", to)
		}
	}

	order, err := h.store.Get(ctx, "ORD-MARK-TARGET-1")
	if err != nil {
		t.Fatalf("读支付单失败: %v", err)
	}
	if order.Status != StatusCreated {
		t.Errorf("被拒的转移改了状态：%q", order.Status)
	}
}

// TestConcurrentNotificationsHoldOneConnectionEach 让 6 条不同单号的通知同时跑。
//
// 测试池只有 4 条连接。分录与状态放进同一个事务之后，一条通知在处理期间只占
// 一条连接，6 路并发会在池上排队；若处理通知时的事务里再开一个事务去写账本，
// 一条通知就占两条，4 路并发时每条都握着第一条去等第二条，整池停摆。这条用例
// 就是对着那个形状写的：它没跑完，说明有人把「另开事务」写了回来。
func TestConcurrentNotificationsHoldOneConnectionEach(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	const n = 6
	orderNos := make([]string, n)
	for i := range orderNos {
		userID := seedUser(t, ctx, h.db)
		orderNos[i] = fmt.Sprintf("ORD-POOL-%d", i)
		h.createOrder(t, ctx, userID, orderNos[i], "1.00")
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i, orderNo := range orderNos {
		wg.Add(1)
		go func(i int, orderNo string) {
			defer wg.Done()
			<-start
			_, errs[i] = h.service.Notify(ctx, stubName, Notification{
				OrderNo:      orderNo,
				GatewayTxnID: "TRADE-" + orderNo,
				Status:       StatusPaid,
				Amount:       money.MustParse("1.00"),
				Asset:        ledger.AssetCNY,
				Fee:          money.MustParse("0.10"),
			})
		}(i, orderNo)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("%s 的通知失败: %v", orderNos[i], err)
		}
	}
	for _, orderNo := range orderNos {
		order, err := h.store.Get(ctx, orderNo)
		if err != nil {
			t.Fatalf("读支付单 %s 失败: %v", orderNo, err)
		}
		if order.Status != StatusPaid {
			t.Errorf("%s 的状态 = %q，期望 paid", orderNo, order.Status)
		}
		if entries := entryCount(t, ctx, h.db, TxnIDForOrder(orderNo)); entries != 4 {
			t.Errorf("%s 的分录 %d 条，期望 4 条", orderNo, entries)
		}
	}
}
