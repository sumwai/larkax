package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/payment"
)

// TestWalletPaymentRejectsBadArgs 验参数错发生在连库之前。
func TestWalletPaymentRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
		hint string
	}{
		{"create 缺 --ref", func() error {
			return runPaymentCreate([]string{"--user", "1", "--amount", "1.00"})
		}, "--ref"},
		{"create 位置参数", func() error {
			return runPaymentCreate([]string{"bogus"})
		}, "位置参数"},
		{"create 金额非法", func() error {
			return runPaymentCreate([]string{"--user", "1", "--amount", "abc", "--ref", "ORD-1"})
		}, "--amount"},
		{"status 缺 --order", func() error { return runPaymentStatus(nil) }, "--order"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.run)
			if err == nil {
				t.Fatal("应当报参数错")
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（%v）", got, exitUsage, err)
			}
			if !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("错误里没有 %q：%v", tc.hint, err)
			}
		})
	}
}

// setPaymentFake 打开或关闭假支付通道的开关。
func setPaymentFake(t *testing.T, ctx context.Context, db *sql.DB, enabled bool) {
	t.Helper()
	value := "false"
	if enabled {
		value = "true"
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE settings SET value = $1 WHERE key = 'payment.fake_enabled'`, value); err != nil {
		t.Fatalf("设置假支付通道开关失败: %v", err)
	}
}

// TestWalletPaymentLifecycle 从关着的拒绝走到下单、查单与重放。
func TestWalletPaymentLifecycle(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	setupEnv(t)

	userID := createTestUserID(t)
	if err := runCaptured(t, func() error {
		return runWalletCreate([]string{"--user", userID})
	}); err != nil {
		t.Fatalf("wallet create 失败: %v", err)
	}

	// 默认关：两条子命令都拒绝，并说清去哪打开。
	err := runCaptured(t, func() error {
		return runPaymentCreate([]string{"--user", userID, "--amount", "66.00", "--ref", "ORD-CLI-1"})
	})
	if err == nil || !strings.Contains(err.Error(), "没有启用") {
		t.Fatalf("通道关着时应当拒绝，得到 %v", err)
	}
	if got := exitCode(err); got != exitFailure {
		t.Errorf("通道关着时的退出码 = %d，期望 %d", got, exitFailure)
	}

	setPaymentFake(t, ctx, db, true)
	t.Cleanup(func() { setPaymentFake(t, ctx, db, false) })

	stdout, _ := captureStreams(t, func() {
		if createErr := runPaymentCreate([]string{
			"--user", userID, "--amount", "66.00", "--ref", "ORD-CLI-1",
		}); createErr != nil {
			t.Fatalf("下单失败: %v", createErr)
		}
	})
	if !strings.Contains(stdout, "已下单") {
		t.Errorf("下单回执 = %q，期望含「已下单」", stdout)
	}

	replay, _ := captureStreams(t, func() {
		if replayErr := runPaymentCreate([]string{
			"--user", userID, "--amount", "66.00", "--ref", "ORD-CLI-1",
		}); replayErr != nil {
			t.Fatalf("同内容重跑失败: %v", replayErr)
		}
	})
	if !strings.Contains(replay, "已经下过单") {
		t.Errorf("重跑回执 = %q，期望含「已经下过单」", replay)
	}

	status, _ := captureStreams(t, func() {
		if statusErr := runPaymentStatus([]string{"--order", "ORD-CLI-1"}); statusErr != nil {
			t.Fatalf("查单失败: %v", statusErr)
		}
	})
	if !strings.Contains(status, "ORD-CLI-1") || !strings.Contains(status, "待支付") {
		t.Errorf("查单回执 = %q，期望含单号与「待支付」", status)
	}

	// 同一个单号配不同金额：冲突，退出 4。
	conflictErr := runCaptured(t, func() error {
		return runPaymentCreate([]string{"--user", userID, "--amount", "67.00", "--ref", "ORD-CLI-1"})
	})
	if !errors.Is(conflictErr, payment.ErrOrderConflict) {
		t.Fatalf("错误 = %v，期望 ErrOrderConflict", conflictErr)
	}
	if got := exitCode(conflictErr); got != exitConflict {
		t.Errorf("冲突退出码 = %d，期望 %d", got, exitConflict)
	}

	// 查一张不存在的单子：退出 4。
	missingErr := runCaptured(t, func() error {
		return runPaymentStatus([]string{"--order", "ORD-NOT-THERE"})
	})
	if !errors.Is(missingErr, payment.ErrOrderNotFound) {
		t.Fatalf("错误 = %v，期望 ErrOrderNotFound", missingErr)
	}
	if got := exitCode(missingErr); got != exitConflict {
		t.Errorf("缺单退出码 = %d，期望 %d", got, exitConflict)
	}

	// 没有钱包的用户下单：与 wallet credit 同一档。
	noWallet := createTestUserID(t)
	walletErr := runCaptured(t, func() error {
		return runPaymentCreate([]string{"--user", noWallet, "--amount", "1.00", "--ref", "ORD-CLI-2"})
	})
	if !errors.Is(walletErr, store.ErrNoWallet) {
		t.Fatalf("错误 = %v，期望 ErrNoWallet", walletErr)
	}
	if got := exitCode(walletErr); got != exitConflict {
		t.Errorf("缺钱包退出码 = %d，期望 %d", got, exitConflict)
	}
}
