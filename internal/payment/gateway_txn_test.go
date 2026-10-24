package payment

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// gatewayTxnUniqueName 是 00064 迁移建的那条部分唯一索引的名字。
//
// 写的是字面量而不是引用 paymentOrdersGatewayTxnUnique：这一格要证明「认出
// 的就是迁移里那个名字」，从被测常量取就等于把同一个错误抄两遍。迁移文件的
// 用例（internal/migrate）断的是同一个字面量，两边分叉时会有一边红。
const gatewayTxnUniqueName = "payment_orders_gateway_txn_uniq_idx"

// TestNotifyRejectsGatewayTxnTakenByAnotherOrder 钉住「两张单争同一个流水号」：
// 第二张被拒、错误指认得出两张单，且两张单的状态与流水号都不变（事务回滚）。
//
// 这一条不能靠单号上的锁：WithOrderLock 只把同一张单的通知串起来，两张不同的
// 单并发入账时谁也不挡谁——守住它的是库里的部分唯一索引。
func TestNotifyRejectsGatewayTxnTakenByAnotherOrder(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-TXN-A", "66.00")
	h.createOrder(t, ctx, userID, "ORD-TXN-B", "66.00")

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	notify := func(orderNo string) (NotificationResult, error) {
		return h.service.Notify(ctx, stubName, Notification{
			OrderNo:      orderNo,
			GatewayTxnID: "TRADE-TXN-797",
			Status:       StatusPaid,
			Amount:       money.MustParse("66.00"),
			Asset:        ledger.AssetCNY,
			Fee:          money.MustParse("0.66"),
			PaidAt:       paidAt,
		})
	}

	if _, err := notify("ORD-TXN-A"); err != nil {
		t.Fatalf("A 的通知失败: %v", err)
	}

	_, err := notify("ORD-TXN-B")
	if err == nil {
		t.Fatal("同一个流水号打到第二张单上应当被拒，实际成功了")
	}
	if !errors.Is(err, ErrGatewayTxnConflict) {
		t.Fatalf("错误 = %v，期望 ErrGatewayTxnConflict", err)
	}
	var conflict *GatewayTxnConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("错误类型 = %T，期望 *GatewayTxnConflictError", err)
	}
	if conflict.OrderNo != "ORD-TXN-B" || conflict.Gateway != stubName ||
		conflict.GatewayTxnID != "TRADE-TXN-797" || conflict.OtherOrderNo != "ORD-TXN-A" {
		t.Errorf("冲突错误 = 单号 %q / 通道 %q / 流水号 %q / 对方单 %q，"+
			"期望 ORD-TXN-B / %s / TRADE-TXN-797 / ORD-TXN-A",
			conflict.OrderNo, conflict.Gateway, conflict.GatewayTxnID, conflict.OtherOrderNo, stubName)
	}
	for _, want := range []string{"ORD-TXN-B", "ORD-TXN-A", "TRADE-TXN-797", stubName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案里应当出现 %q，实际是 %q", want, err.Error())
		}
	}

	// A 与 B 两张单都不变：A 仍是它原来那次入账的结果，B 停在 created、
	// 不记流水号、不记支付时刻与手续费。
	a, err := h.store.Get(ctx, "ORD-TXN-A")
	if err != nil {
		t.Fatalf("读 A 失败: %v", err)
	}
	if a.Status != StatusPaid || a.GatewayTxnID != "TRADE-TXN-797" {
		t.Errorf("A = 状态 %q / 流水号 %q，期望 paid / TRADE-TXN-797", a.Status, a.GatewayTxnID)
	}
	b, err := h.store.Get(ctx, "ORD-TXN-B")
	if err != nil {
		t.Fatalf("读 B 失败: %v", err)
	}
	if b.Status != StatusCreated {
		t.Errorf("B 的状态 = %q，期望仍是 created", b.Status)
	}
	if b.GatewayTxnID != "" {
		t.Errorf("B 的流水号 = %q，期望为空", b.GatewayTxnID)
	}
	if b.PaidAt != nil {
		t.Errorf("B 的支付时刻 = %v，期望为空", b.PaidAt)
	}
	if b.Fee.Cmp(money.Zero) != 0 {
		t.Errorf("B 的手续费 = %s，期望 0", b.Fee)
	}

	// B 的分录与余额也没留下：账本写入与状态改动在同一个事务里，一起回滚。
	if n := entryCount(t, ctx, h.db, TxnIDForOrder("ORD-TXN-B")); n != 0 {
		t.Errorf("被拒的 B 留下 %d 条分录，期望 0 条", n)
	}
	if balance := walletBalance(t, ctx, h.db, userID); balance != money.MustParse("66.00").String() {
		t.Errorf("钱包余额 = %s，期望只有 A 那一笔 66.00", balance)
	}
}

// TestReplayedGatewayTxnOnTheSameOrderStillSucceeds 钉住「同一张单的重放不受
// 新约束影响」：同一份通知再来一次仍回成功、只入账一次。
//
// 与上一条配对：那张索引判的是「同一个号码记在**两张**单上」，不是「这个号码
// 出现过」——写成后者的话，通道每一次重发都会被拒，而通道看到非 2xx 会继续重发。
func TestReplayedGatewayTxnOnTheSameOrderStillSucceeds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	userID := seedUser(t, ctx, h.db)
	h.createOrder(t, ctx, userID, "ORD-TXN-REPLAY", "66.00")

	paidAt := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	notify := func() (NotificationResult, error) {
		return h.service.Notify(ctx, stubName, Notification{
			OrderNo:      "ORD-TXN-REPLAY",
			GatewayTxnID: "TRADE-TXN-REPLAY",
			Status:       StatusPaid,
			Amount:       money.MustParse("66.00"),
			Asset:        ledger.AssetCNY,
			Fee:          money.MustParse("0.66"),
			PaidAt:       paidAt,
		})
	}

	first, err := notify()
	if err != nil {
		t.Fatalf("首次通知失败: %v", err)
	}
	if !first.Applied {
		t.Error("首次通知应当写入账本")
	}
	second, err := notify()
	if err != nil {
		t.Fatalf("重放通知失败: %v", err)
	}
	if second.Applied {
		t.Error("重放不该再写分录")
	}
	if second.Order.Status != StatusPaid || second.Order.GatewayTxnID != "TRADE-TXN-REPLAY" {
		t.Errorf("重放之后 = 状态 %q / 流水号 %q，期望 paid / TRADE-TXN-REPLAY",
			second.Order.Status, second.Order.GatewayTxnID)
	}
	if n := entryCount(t, ctx, h.db, TxnIDForOrder("ORD-TXN-REPLAY")); n != 4 {
		t.Errorf("重放之后分录 %d 条，期望仍是 4 条", n)
	}
	if balance := walletBalance(t, ctx, h.db, userID); balance != money.MustParse("66.00").String() {
		t.Errorf("重放之后余额 = %s，期望 66.00", balance)
	}
}

// TestGatewayTxnUniqueViolationDetection 钉住「只认这一条索引」。
//
// 最省事的写法是「只要 23505 就翻译成流水号冲突」，而单号上也有一条唯一键
// （payment_orders_order_no_key）：那样下单撞单号会被报成「流水号被别人占了」，
// 排障的人会去查一张根本不存在的单。所以错误码与索引名要同时对上。
func TestGatewayTxnUniqueViolationDetection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "撞的是流水号索引",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: gatewayTxnUniqueName},
			want: true,
		},
		{
			name: "包了一层也算",
			err:  errors.Join(errors.New("更新失败"), &pgconn.PgError{Code: "23505", ConstraintName: gatewayTxnUniqueName}),
			want: true,
		},
		{
			name: "23505 但撞的是单号",
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "payment_orders_order_no_key"},
			want: false,
		},
		{
			name: "别的错误码",
			err:  &pgconn.PgError{Code: "23503", ConstraintName: gatewayTxnUniqueName},
			want: false,
		},
		{
			name: "不是数据库错误",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayTxnUniqueViolated(tc.err); got != tc.want {
				t.Errorf("gatewayTxnUniqueViolated(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestGatewayTxnConflictLookupFailureKeepsTheSameKind 钉住「补查对方单号失败
// 不改变错误种类」。
//
// 与 TestNotifyRejectsGatewayTxnTakenByAnotherOrder 配对：那边是补查成功、
// 错误里带着对方单号；这里是补查失败，调用方拿到的仍是同一个
// ErrGatewayTxnConflict，差异只在文案少一个对方单号，并且留下一条日志。它不能
// 变成 5xx，也不能退化成驱动原文——前者会让通道重试到天荒，后者调用方认不出来。
//
// 失败只注入到补查这一处，不动共享的连接池：关池会让同包后面的用例跟着报连接
// 错误，而那种红看起来像被测代码坏了。
func TestGatewayTxnConflictLookupFailureKeepsTheSameKind(t *testing.T) {
	store := NewStore(nil)
	store.lookupConflict = func(context.Context, string, string) (string, error) {
		return "", errors.New("补查那一句查询失败（用例注入）")
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	conflict := &GatewayTxnConflictError{
		OrderNo:      "ORD-TXN-B",
		Gateway:      stubName,
		GatewayTxnID: "TRADE-TXN-797",
	}
	err := store.annotateGatewayTxnConflict(context.Background(), conflict)

	if !errors.Is(err, ErrGatewayTxnConflict) {
		t.Fatalf("错误 = %v，期望仍认得出 ErrGatewayTxnConflict", err)
	}
	var got *GatewayTxnConflictError
	if !errors.As(err, &got) {
		t.Fatalf("错误类型 = %T，期望 *GatewayTxnConflictError", err)
	}
	if got.OtherOrderNo != "" {
		t.Errorf("补查失败时对方单号 = %q，期望为空", got.OtherOrderNo)
	}
	for _, want := range []string{"ORD-TXN-B", "TRADE-TXN-797", stubName, "另一张支付单"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("错误文案里应当出现 %q，实际是 %q", want, got.Error())
		}
	}
	if !strings.Contains(logs.String(), "补查对方单号失败") {
		t.Errorf("补查失败应当记一条日志，实际日志是 %q", logs.String())
	}
}
