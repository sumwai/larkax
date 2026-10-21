package plans

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"
)

// captureLogs 把这一层的日志引到一个缓冲区，返回日志句柄与读缓冲区的函数。
//
// 「按默认值走」这条判据的另一半是「记了一条日志」：只断言回落的取值，实现
// 静默吞掉未知值也照样绿，而运维就再没有第二条路知道库里那一行写坏了。
func captureLogs(t *testing.T, svc *Service) func() string {
	t.Helper()
	var buf bytes.Buffer
	svc.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func() string { return buf.String() }
}

// setPlatformChargeOrder 直接改 settings 里那一行，模拟运维改平台默认。
//
// 走 CLI 的 `settings set` 要一个记名的操作人，这里要的是「库里的值是什么」。
func setPlatformChargeOrder(t *testing.T, svc *Service, value string) {
	t.Helper()
	if _, err := svc.db.ExecContext(context.Background(),
		`UPDATE settings SET value = $1::jsonb WHERE key = $2`,
		value, PlatformChargeOrderKey); err != nil {
		t.Fatalf("改平台默认扣费顺序失败: %v", err)
	}
}

// 没有任何覆盖时，有效顺序就是平台默认，出处是 platform。
func TestEffectiveChargeOrderFollowsPlatformDefault(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)

	order, source, err := svc.EffectiveChargeOrder(ctx, userID)
	if err != nil {
		t.Fatalf("读扣费顺序失败: %v", err)
	}
	if order != ChargeOrderPlanFirst {
		t.Errorf("有效顺序 = %q，期望平台默认 %q", order, ChargeOrderPlanFirst)
	}
	if source != ChargeOrderSourcePlatform {
		t.Errorf("出处 = %q，期望 %q", source, ChargeOrderSourcePlatform)
	}
}

// 用户级覆盖生效，且出处报成 user。
func TestEffectiveChargeOrderUsesUserOverride(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)

	if err := svc.SetUserChargeOrder(ctx, userID, ChargeOrderBalanceFirst); err != nil {
		t.Fatalf("设用户扣费顺序失败: %v", err)
	}
	order, source, err := svc.EffectiveChargeOrder(ctx, userID)
	if err != nil {
		t.Fatalf("读扣费顺序失败: %v", err)
	}
	if order != ChargeOrderBalanceFirst {
		t.Errorf("有效顺序 = %q，期望 %q", order, ChargeOrderBalanceFirst)
	}
	if source != ChargeOrderSourceUser {
		t.Errorf("出处 = %q，期望 %q", source, ChargeOrderSourceUser)
	}

	// 清掉覆盖之后回到跟随平台默认。
	if clearErr := svc.ClearUserChargeOrder(ctx, userID); clearErr != nil {
		t.Fatalf("清用户扣费顺序失败: %v", clearErr)
	}
	order, source, err = svc.EffectiveChargeOrder(ctx, userID)
	if err != nil {
		t.Fatalf("清掉覆盖后读扣费顺序失败: %v", err)
	}
	if order != ChargeOrderPlanFirst || source != ChargeOrderSourcePlatform {
		t.Errorf("清掉覆盖后 = %q/%q，期望回到平台默认 %q", order, source, ChargeOrderPlanFirst)
	}
}

// 平台默认改成另一个取值时，有效顺序跟着变。
func TestEffectiveChargeOrderFollowsChangedPlatformDefault(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)

	before := platformChargeOrderRow(t, svc)
	setPlatformChargeOrder(t, svc, `"balance_only"`)
	t.Cleanup(func() { restorePlatformChargeOrder(t, svc, before) })

	order, source, err := svc.EffectiveChargeOrder(ctx, userID)
	if err != nil {
		t.Fatalf("读扣费顺序失败: %v", err)
	}
	if order != ChargeOrderBalanceOnly {
		t.Errorf("有效顺序 = %q，期望 %q", order, ChargeOrderBalanceOnly)
	}
	if source != ChargeOrderSourcePlatform {
		t.Errorf("出处 = %q，期望 %q", source, ChargeOrderSourcePlatform)
	}
}

// 平台默认是一个认不出的取值时按兜底值走，并记一条日志。
func TestEffectiveChargeOrderFallsBackOnUnknownPlatformValue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	logs := captureLogs(t, svc)
	userID := seedUser(t, db)

	before := platformChargeOrderRow(t, svc)
	setPlatformChargeOrder(t, svc, `"future_only"`)
	t.Cleanup(func() { restorePlatformChargeOrder(t, svc, before) })

	order, source, err := svc.EffectiveChargeOrder(ctx, userID)
	if err != nil {
		t.Fatalf("读扣费顺序失败: %v", err)
	}
	if order != DefaultChargeOrder {
		t.Errorf("有效顺序 = %q，期望兜底值 %q", order, DefaultChargeOrder)
	}
	if source != ChargeOrderSourcePlatform {
		t.Errorf("出处 = %q，期望 %q", source, ChargeOrderSourcePlatform)
	}
	if !strings.Contains(logs(), "平台默认的扣费顺序取值认不出") {
		t.Errorf("未知取值没有记日志，日志是:\n%s", logs())
	}
}

// 用户行上是未知取值时按平台默认走，并记一条日志。
//
// 库里那条 CHECK 让正常写入路径写不进未知值，所以这条用例先把约束摘掉再写：
// 它模拟的是「更新的版本写过这个值、老版本回读」——那时约束可能已经放宽、
// 而读的代码还不认识新取值。用例结束时把行改回去再补上约束。
//
// 平台默认在这里特意改成 balance_first：若平台那一行还停在 plan_first，这个
// 回落与兜底常量 DefaultChargeOrder 的取值相同，实现写成「认不出就返回常量」
// 也照样绿，用例就钉不住「用的是平台那一行」。
func TestEffectiveChargeOrderFallsBackOnUnknownUserValue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	logs := captureLogs(t, svc)
	userID := seedUser(t, db)

	before := platformChargeOrderRow(t, svc)
	setPlatformChargeOrder(t, svc, `"balance_first"`)
	t.Cleanup(func() { restorePlatformChargeOrder(t, svc, before) })

	dropChargeOrderCheck(t, db, userID)
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET charge_order = 'future_only' WHERE id = $1`, userID); err != nil {
		t.Fatalf("写未知取值失败: %v", err)
	}

	order, source, err := svc.EffectiveChargeOrder(ctx, userID)
	if err != nil {
		t.Fatalf("读扣费顺序失败: %v", err)
	}
	if order != ChargeOrderBalanceFirst {
		t.Errorf("有效顺序 = %q，期望回落到的平台默认 %q", order, ChargeOrderBalanceFirst)
	}
	if source != ChargeOrderSourcePlatform {
		t.Errorf("出处 = %q，期望 %q", source, ChargeOrderSourcePlatform)
	}
	if !strings.Contains(logs(), "用户上的扣费顺序取值认不出") {
		t.Errorf("未知取值没有记日志，日志是:\n%s", logs())
	}
}

// 写入侧：认不出的取值、非法用户都当场报错，且一个字都不写。
func TestSetUserChargeOrderRejectsUnknownValue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)

	err := svc.SetUserChargeOrder(ctx, userID, ChargeOrder("tokens_first"))
	if err == nil {
		t.Fatal("认不出的取值应当报错")
	}
	if !strings.Contains(err.Error(), "tokens_first") {
		t.Errorf("报错没有点出那个取值: %v", err)
	}

	if err := svc.SetUserChargeOrder(ctx, 0, ChargeOrderPlanFirst); err == nil {
		t.Error("用户 id 为 0 应当报错")
	}
	if err := svc.ClearUserChargeOrder(ctx, -1); err == nil {
		t.Error("清用户 id 为负应当报错")
	}
	if _, _, err := svc.EffectiveChargeOrder(ctx, 0); err == nil {
		t.Error("读扣费顺序时用户 id 为 0 应当报错")
	}
}

// 不存在的用户：写与读都报错，而不是静默成功。
func TestChargeOrderRejectsMissingUser(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)

	if err := svc.SetUserChargeOrder(ctx, 987654321, ChargeOrderBalanceFirst); err == nil {
		t.Error("给不存在的用户设扣费顺序应当报错")
	}
	if err := svc.ClearUserChargeOrder(ctx, 987654321); err == nil {
		t.Error("清不存在的用户的扣费顺序应当报错")
	}
	if _, _, err := svc.EffectiveChargeOrder(ctx, 987654321); err == nil {
		t.Error("读不存在的用户的扣费顺序应当报错")
	}
}

// platformChargeOrderRow 读回 settings 里那一行的原文，供用例结束时还原。
func platformChargeOrderRow(t *testing.T, svc *Service) string {
	t.Helper()
	var raw string
	if err := svc.db.QueryRowContext(context.Background(),
		`SELECT value::text FROM settings WHERE key = $1`,
		PlatformChargeOrderKey).Scan(&raw); err != nil {
		t.Fatalf("读平台默认扣费顺序失败: %v", err)
	}
	return raw
}

// restorePlatformChargeOrder 把 settings 那一行写回原来的值。
func restorePlatformChargeOrder(t *testing.T, svc *Service, raw string) {
	t.Helper()
	if _, err := svc.db.ExecContext(context.Background(),
		`UPDATE settings SET value = $1::jsonb WHERE key = $2`,
		raw, PlatformChargeOrderKey); err != nil {
		t.Errorf("还原平台默认扣费顺序失败: %v", err)
	}
}

// dropChargeOrderCheck 摘掉 users.charge_order 上的 CHECK，并在用例结束时补回去。
//
// 判据是「库里确实有未知取值时读侧怎么办」，而正常写入路径写不进它——不先摘
// 约束，这条用例就只能测一个不可能发生的状态。补回来之前先把这一行的值改回
// NULL，否则 ADD CONSTRAINT 会因为那行未知取值失败。
func dropChargeOrderCheck(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`ALTER TABLE users DROP CONSTRAINT users_charge_order_check`); err != nil {
		t.Fatalf("摘掉 users.charge_order 的 CHECK 失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`UPDATE users SET charge_order = NULL WHERE id = $1`, userID); err != nil {
			t.Errorf("还原用户的扣费顺序失败: %v", err)
		}
		if _, err := db.ExecContext(context.Background(),
			`ALTER TABLE users ADD CONSTRAINT users_charge_order_check
			   CHECK (charge_order IN ('plan_first', 'balance_first', 'balance_only'))`); err != nil {
			t.Errorf("补回 users.charge_order 的 CHECK 失败: %v", err)
		}
	})
}
