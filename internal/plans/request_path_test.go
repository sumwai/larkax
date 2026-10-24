package plans

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
)

// 本文件是请求路径与作废那一侧的两条约定（#918）：
//
//   - 扣费顺序的读点**不因为配置读不出来而拒绝服务**：用户级读失败跟随平台
//     默认，平台默认再失败用兜底值，两次都留日志；
//   - 作废只核销「面值余额 − 在途预留」，给已经在飞的那几笔留下它们占着的部分。

func TestRequestChargeOrderFallsBackInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)

	// 平台默认先改成 balance_first，再做「用户级读失败」那一档：若它停在
	// plan_first，那与兜底常量 DefaultChargeOrder 同值——实现即使跳过平台那一读、
	// 直接返回常量，断言也照样是绿的，钉住的就不是「跟随平台默认」。
	var (
		beforeValue       string
		beforeDescription string
		beforeEditable    bool
	)
	if err := db.QueryRowContext(ctx,
		`SELECT value::text, description, editable FROM settings WHERE key = $1`,
		PlatformChargeOrderKey).Scan(&beforeValue, &beforeDescription, &beforeEditable); err != nil {
		t.Fatalf("读平台默认扣费顺序那一行失败: %v", err)
	}
	// 这一行是同一个测试库上其它用例的前提，而下面会把它删掉。还原必须连
	// 「行不在」也救得回来，所以用 upsert 而不是 UPDATE。
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO settings (key, value, description, editable)
			VALUES ($1, $2::jsonb, $3, $4)
			ON CONFLICT (key) DO UPDATE
			   SET value = EXCLUDED.value, description = EXCLUDED.description,
			       editable = EXCLUDED.editable`,
			PlatformChargeOrderKey, beforeValue, beforeDescription, beforeEditable); err != nil {
			t.Errorf("还原平台默认扣费顺序失败: %v", err)
		}
	})
	setPlatformChargeOrder(t, svc, `"balance_first"`)

	// 不存在的用户：EffectiveChargeOrder 读用户那一步就失败，请求路径要按
	// 平台默认继续，而不是把错误交给调用方。
	if got := svc.RequestChargeOrder(ctx, -1); got != ChargeOrderBalanceFirst.String() {
		t.Errorf("用户级读失败时应跟随平台默认 %q，得到 %q", ChargeOrderBalanceFirst, got)
	}

	// 平台默认那一行也删掉：再退一档到 DefaultChargeOrder，同样不报错。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM settings WHERE key = $1`, PlatformChargeOrderKey); err != nil {
		t.Fatalf("删平台默认失败: %v", err)
	}
	userID := seedUser(t, ctx, db)
	if got := svc.RequestChargeOrder(ctx, userID); got != DefaultChargeOrder.String() {
		t.Errorf("两级都读不到时应落到兜底值 %q，得到 %q", DefaultChargeOrder, got)
	}

	// 用户自己的覆盖仍然是第一优先级（此刻平台那一行不在，所以只有它能让
	// 结果是 balance_first）。
	if err := svc.SetUserChargeOrder(ctx, userID, ChargeOrderBalanceFirst); err != nil {
		t.Fatalf("设用户扣费顺序失败: %v", err)
	}
	if got := svc.RequestChargeOrder(ctx, userID); got != ChargeOrderBalanceFirst.String() {
		t.Errorf("用户覆盖 = %q，期望 %q", got, ChargeOrderBalanceFirst)
	}
}

// seedPendingPlanReservation 造一条额度包覆盖的 pending 预留，返回 request_id。
func seedPendingPlanReservation(t *testing.T, ctx context.Context, db *sql.DB, userID, userPlanID int64, amount string) string {
	t.Helper()
	var keyID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id, name, key_hash, key_prefix)
		VALUES ($1, 'plan-void-test', $2, 'placeholder') RETURNING id`,
		userID, "hash-"+itoa(nextID())).Scan(&keyID); err != nil {
		t.Fatalf("建 API Key 失败: %v", err)
	}
	requestID := "req-" + itoa(nextID())
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_requests
			(request_id, user_id, api_key_id, canonical_model, protocol, streamed,
			 reserved_amount, reserved_asset, status, started_at,
			 user_plan_id, reserved_quota, plan_reserved_amount)
		VALUES ($1, $2, $3, 'gpt-4o', 'openai', false,
		        0, 'CNY', 'pending', now(), $4, 0, $5::numeric)`,
		requestID, userID, keyID, userPlanID, amount); err != nil {
		t.Fatalf("写 pending 预留失败: %v", err)
	}
	return requestID
}

// 作废不能把在途预留已经承诺出去的那部分面值也核销掉：那会让在途请求
// 结算时把 prepaid 扣成负数。
func TestVoidLeavesPendingReservationOnPrepaid(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	userID := seedUser(t, ctx, db)

	plan := newPlan(t, ctx, svc, "50")
	up, applied, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil || !applied {
		t.Fatalf("发放失败: applied=%v err=%v", applied, err)
	}

	seedPendingPlanReservation(t, ctx, db, userID, up.ID, "30")

	before := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(up.ID))
	if before.Cmp(amount(t, "50")) != 0 {
		t.Fatalf("作废前面值余额 = %s，期望 50", before)
	}

	voided, applied, err := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: up.ID, Ref: "void-" + itoa(nextID()),
		Status: UserPlanExpired,
	})
	if err != nil || !applied {
		t.Fatalf("作废失败: applied=%v err=%v", applied, err)
	}
	if voided.Status != UserPlanExpired {
		t.Errorf("状态 = %s，期望 %s", voided.Status, UserPlanExpired)
	}

	after := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(up.ID))
	if after.Cmp(amount(t, "30")) != 0 {
		t.Errorf("作废后面值余额 = %s，期望 30（在途占用留给那几笔请求）", after)
	}
}
