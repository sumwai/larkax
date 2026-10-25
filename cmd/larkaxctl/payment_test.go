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

// TestWalletPaymentPlanIgnoresDefaultAsset 钉住「买额度包不读默认资产」。
//
// 把 settings.billing.default_asset 改成一个不受支持的取值，然后：
//   - 带 --plan 下单（不给 --asset）→ 成功，币种取自包；
//   - 不带 --plan 走充值 → 报「不是受支持的资产」。
//
// 这一对钉的是**行为**，不是实现结构：将来谁把 resolveWalletAsset 挪回买包
// 那条路，第二条会先红。它必须写在 cmd/larkaxctl 包：读 settings 的是 CLI 侧的
// resolveWalletAsset，服务层 Service.Create 根本不读它——写在 internal/payment
// 里断言那句话是假绿，那条错误路径在那一层不存在。
//
// **这两条用例必须读同一个 settings 行**（同一份 "XYZ"），否则「同一份 settings
// 下两条路分叉」这个语义就没了。取值要写成合法的 JSON 字符串（settings.Load
// 会校验 json.Valid），否则报的是「不是合法 JSON」，走不到资产那一句。
func TestWalletPaymentPlanIgnoresDefaultAsset(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	setupEnv(t)
	setPaymentFake(t, ctx, db, true)
	t.Cleanup(func() { setPaymentFake(t, ctx, db, false) })

	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (key, name, asset, unit, quota_total, face_value, validity_days)
		VALUES ('cli-plan-asset', '命令行售卖包', 'CNY', 'amount', 42, 42, 30)
		ON CONFLICT (key) DO NOTHING`); err != nil {
		t.Fatalf("建额度包失败: %v", err)
	}

	// 两个用户与钱包都在改配置**之前**建好：wallet create 自己也要读默认资产，
	// 放到改之后会先撞上同一个错误，用例就测不到 payment create 那条路了。
	planUser := createTestUserID(t)
	topupUser := createTestUserID(t)
	for _, id := range []string{planUser, topupUser} {
		if err := runCaptured(t, func() error {
			return runWalletCreate([]string{"--user", id})
		}); err != nil {
			t.Fatalf("wallet create 失败: %v", err)
		}
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE settings SET value = '"XYZ"' WHERE key = 'billing.default_asset'`); err != nil {
		t.Fatalf("改默认资产失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE settings SET value = '"CNY"' WHERE key = 'billing.default_asset'`); err != nil {
			t.Errorf("还原默认资产失败（后续用例会看到 XYZ）: %v", err)
		}
	})

	// 第一条路：买额度包，不读默认资产，币种由包决定。
	stdout, _ := captureStreams(t, func() {
		if err := runPaymentCreate([]string{
			"--user", planUser, "--plan", "cli-plan-asset", "--ref", "ORD-CLI-PLAN-ASSET",
		}); err != nil {
			t.Fatalf("默认资产不可用时买额度包不该失败: %v", err)
		}
	})
	if !strings.Contains(stdout, "CNY") {
		t.Errorf("下单回执里没有包决定的币种 CNY：%q", stdout)
	}
	// 回执要打出**单子上记的**天数：用户买的是「42 元 / 30 天」这件商品，
	// 只打金额不打天数等于只报了一半。钉单子上的值而不是包上的现值——包在
	// 下单之后被改过时，用户该看到的是自己买的那一档。
	if !strings.Contains(stdout, "30 天") {
		t.Errorf("下单回执里没有这张单的天数（30 天），用户看不到自己买了多久：%q", stdout)
	}

	// 第二条路：同一份 settings 下走充值，必须报那句。
	err := runCaptured(t, func() error {
		return runPaymentCreate([]string{"--user", topupUser, "--amount", "1.00", "--ref", "ORD-CLI-DEFAULT-ASSET"})
	})
	if err == nil {
		t.Fatal("默认资产不可用时充值应当报错")
	}
	if !strings.Contains(err.Error(), "不是受支持的资产") {
		t.Errorf("报错没有点名默认资产不可用：%v", err)
	}
}
