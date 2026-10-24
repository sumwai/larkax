package plans

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是额度包期末按比例结转（#1193）。要钉住的几件事：
//
//   - 只有真的到期才结转；canceled 与提前标 expired 整笔归平台；
//   - 面值与可用量两个维度各自算、各自向下取整，从一维推另一维会错；
//   - 结转出去的钱与归平台的钱严格等于处置前的剩余面值（取整的零头归平台）；
//   - 比例读不出来时整次作废失败，订阅与账本都不动；
//   - 领新包时并入，只并账本余额大于零的源；发放重放不会二次并入；
//   - 待并入的余额在账本汇总里看得见（不落到「认不出的科目」）。

// setCarryoverBP 改测试库里的结转比例，并在用例结束时还原。
//
// 用 UPDATE 而不是 INSERT：迁移 00065 已经写进这一行过，缺了它说明库里状态
// 本身不对，用例该红。还原走 upsert，因为「比例读不出来」那条用例会把它删掉，
// 而删掉之后 UPDATE 救不回来。
//
// ctx 由调用方传进来（理由见 seedUser）；收尾那个闭包用的也是它——自己再造一个
// context.Background() 就是 contextcheck 报的那个形状。
func setCarryoverBP(t *testing.T, ctx context.Context, db *sql.DB, bp int64) {
	t.Helper()

	var (
		value       string
		description string
		editable    bool
	)
	if err := db.QueryRowContext(ctx,
		`SELECT value::text, description, editable FROM settings WHERE key = $1`,
		carryoverBpKey).Scan(&value, &description, &editable); err != nil {
		t.Fatalf("读 %s 失败：迁移 00065 没跑到这个库上？%v", carryoverBpKey, err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO settings (key, value, description, editable)
			VALUES ($1, $2::jsonb, $3, $4)
			ON CONFLICT (key) DO UPDATE
			   SET value = EXCLUDED.value, description = EXCLUDED.description,
			       editable = EXCLUDED.editable`,
			carryoverBpKey, value, description, editable); err != nil {
			t.Errorf("还原 %s 失败: %v", carryoverBpKey, err)
		}
	})
	if _, err := db.ExecContext(ctx,
		`UPDATE settings SET value = $1::jsonb WHERE key = $2`,
		itoa(bp), carryoverBpKey); err != nil {
		t.Fatalf("把 %s 改成 %d 失败: %v", carryoverBpKey, bp, err)
	}
}

// expireSubscription 把一条订阅的到期时刻挪到过去，模拟「时间到了」。
//
// 发放要求到期时刻在将来，所以到期态只能事后改出来。走 UPDATE 而不是把
// 库里的时间伪造掉：判据用的是库的 now()，而用例要验证的正是「period_end
// 已经过去」这件事。
func expireSubscription(t *testing.T, ctx context.Context, db *sql.DB, userPlanID int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`UPDATE user_plans SET period_end = now() - interval '1 minute' WHERE id = $1`,
		userPlanID); err != nil {
		t.Fatalf("把订阅 %d 改成已到期失败: %v", userPlanID, err)
	}
}

// grantPlan 发一个包，返回订阅 id。
func grantFresh(t *testing.T, ctx context.Context, svc *Service, userID int64, planKey, ref string) int64 {
	t.Helper()
	up, applied, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref, UserID: userID, PlanKey: planKey,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil || !applied {
		t.Fatalf("发放 %s 失败: applied=%v err=%v", ref, applied, err)
	}
	return up.ID
}

// newTokenPlan 建一个 token 包（token 包必须有模型范围）。
func newTokenPlan(t *testing.T, ctx context.Context, svc *Service, quota, face string) Plan {
	t.Helper()
	p, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "测试 token 包", Asset: ledger.AssetCNY,
		Unit: UnitToken, QuotaTotal: amount(t, quota), FaceValue: amount(t, face),
		Models: []string{"gpt-4o"},
	})
	if err != nil {
		t.Fatalf("建 token 包失败: %v", err)
	}
	return p
}

// newRequestPlan 建一个次数包（与 token 包一样必须有模型范围）。
func newRequestPlan(t *testing.T, ctx context.Context, svc *Service, quota, face string) Plan {
	t.Helper()
	p, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "测试次数包", Asset: ledger.AssetCNY,
		Unit: UnitRequest, QuotaTotal: amount(t, quota), FaceValue: amount(t, face),
		Models: []string{"gpt-4o"},
	})
	if err != nil {
		t.Fatalf("建次数包失败: %v", err)
	}
	return p
}

// consumePrepaid 从一条订阅的面值里扣掉 amt（走账本，不经过请求路径）。
func consumePrepaid(t *testing.T, ctx context.Context, svc *Service, userID, userPlanID int64, amt money.Amount) {
	t.Helper()
	txn, err := ledger.Consume(ledger.ConsumeParams{
		TxnID: newTxnID(), UserID: userID, Amount: amt, Asset: ledger.AssetCNY,
		PrepaidAccount: ledger.PrepaidAccount(userPlanID),
	})
	if err != nil {
		t.Fatalf("构造消费分录失败: %v", err)
	}
	if _, err := svc.ledger.PostTransaction(ctx, txn); err != nil {
		t.Fatalf("写消费分录失败: %v", err)
	}
}

// voidPlan 作废一条订阅，断言调用本身成功。
func voidPlan(t *testing.T, ctx context.Context, svc *Service, upID int64, status UserPlanStatus, ref string) UserPlan {
	t.Helper()
	up, applied, err := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: upID, Ref: ref, Status: status,
	})
	if err != nil || !applied {
		t.Fatalf("作废订阅 %d 失败: applied=%v err=%v", upID, applied, err)
	}
	return up
}

// 比例三档：0 不结转、0.4 结转四成、1 全部结转。
func TestCarryoverRatios(t *testing.T) {
	cases := []struct {
		name string
		bp   int64
		// carry 是期望结转出去的面值。
		carry string
	}{
		{"不结转", 0, "0"},
		{"四成", 4000, "40"},
		{"全部结转", 10000, "100"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testDB(t)
			svc := New(db)
			setCarryoverBP(t, ctx, db, tc.bp)

			userID := seedUser(t, ctx, db)
			plan := newPlan(t, ctx, svc, "100")
			upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
			before := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(upID))
			expireSubscription(t, ctx, db, upID)

			up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

			carry := amount(t, tc.carry)
			if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); got.Cmp(carry) != 0 {
				t.Errorf("结转余额 = %s，期望 %s", got, carry)
			}
			if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(upID)); !got.IsZero() {
				t.Errorf("作废后面值余额 = %s，期望 0", got)
			}
			if got := up.CarryoutAmount; got.Cmp(carry) != 0 {
				t.Errorf("订阅记的结转出去面值 = %s，期望 %s", got, carry)
			}
			if got := up.CarryoutQuota; got.Cmp(carry) != 0 {
				t.Errorf("金额包的结转出去量 = %s，期望与面值同值 %s", got, carry)
			}
			if up.Status != UserPlanExpired {
				t.Errorf("状态 = %s，期望 %s", up.Status, UserPlanExpired)
			}

			// 守恒：结转 + 归平台 = 处置前的剩余面值。三个数都从账本读，
			// 期望值不取自被测代码算出的结转账面。
			stayed := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID))
			voided := planVoidedFrom(t, ctx, db, upID)
			if sum := stayed.Add(voided); sum.Cmp(before) != 0 {
				t.Errorf("结转 %s + 归平台 %s = %s，期望处置前的面值余额 %s",
					stayed, voided, sum, before)
			}
		})
	}
}

// 向下取整（token / 次数那一档）：剩余 7 个 token、比例 0.4 → 结转 2，
// 不是 2.8。这一维没结转的 5 留在终态订阅上、随订阅一起不可用，**不进平台
// 科目**——所以下面只断言面值那一维的归平台金额，别把两维的处置混在一起。
//
// 面值那一维在 token 包上也按整单位截断（与量同一档，见 carryShare）：7 元面值
// × 0.4 = 2.8 → 2，被截掉的 0.8 与没结转的 4 元一起归平台（共 5）。
func TestCarryoverFloors(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 4000)

	userID := seedUser(t, ctx, db)
	plan := newTokenPlan(t, ctx, svc, "7", "7")
	upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
	expireSubscription(t, ctx, db, upID)

	up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); got.Cmp(amount(t, "2")) != 0 {
		t.Errorf("结转余额 = %s，期望 2（7 × 0.4 向下取整到整单位）", got)
	}
	if got := up.CarryoutAmount; got.Cmp(amount(t, "2")) != 0 {
		t.Errorf("订阅记的结转出去面值 = %s，期望 2", got)
	}
	if got := up.CarryoutQuota; got.Cmp(amount(t, "2")) != 0 {
		t.Errorf("订阅记的结转出去量 = %s，期望 2（7 个 token × 0.4，不是 2.8）", got)
	}
	if got := planVoidedFrom(t, ctx, db, upID); got.Cmp(amount(t, "5")) != 0 {
		t.Errorf("面值归平台 = %s，期望 5（7 − 2）", got)
	}
}

// 金额包按 money 的最小单位（10^-8）取整，不是按整元。
//
// 取剩余 3 个最小单位（0.00000003）、比例 4000 bp：3 × 4000 / 10000 = 1.2，
// 向下取整得 1，平台侧得 2，三个数逐字断言。把这一档改回整元截断时，结转
// 会变成 0、平台拿走全部 3——这一格红。
//
// 它**不**区分向下与四舍五入：1.2 两种口径都得 1。正好落在半格上的那一格见
// TestCarryoverAmountRoundsDownNotHalfUp。
func TestCarryoverAmountKeepsSubYuanPrecision(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 4000)

	userID := seedUser(t, ctx, db)
	plan := newPlan(t, ctx, svc, "0.00000003")
	upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
	before := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(upID))
	expireSubscription(t, ctx, db, upID)

	up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); got.Cmp(amount(t, "0.00000001")) != 0 {
		t.Errorf("结转余额 = %s，期望 0.00000001（3 个单位 × 0.4 = 1.2，向下取整到 1）", got)
	}
	if got := up.CarryoutAmount; got.Cmp(amount(t, "0.00000001")) != 0 {
		t.Errorf("订阅记的结转出去面值 = %s，期望 0.00000001", got)
	}
	if got := up.CarryoutQuota; got.Cmp(amount(t, "0.00000001")) != 0 {
		t.Errorf("金额包的结转出去量 = %s，期望与面值同值 0.00000001", got)
	}
	if got := planVoidedFrom(t, ctx, db, upID); got.Cmp(amount(t, "0.00000002")) != 0 {
		t.Errorf("面值归平台 = %s，期望 0.00000002（3 − 1）", got)
	}
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)).Add(planVoidedFrom(t, ctx, db, upID)); got.Cmp(before) != 0 {
		t.Errorf("结转 + 归平台 = %s，期望处置前的面值余额 %s", got, before)
	}
}

// 金额包的取整方向落在正好半格上时见分晓：剩余 5 个最小单位、比例 5000 bp，
// 应得 2.5 个单位。向下取整得 2，四舍五入会得 3。这条是「向下」的那格判据。
func TestCarryoverAmountRoundsDownNotHalfUp(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 5000)

	userID := seedUser(t, ctx, db)
	plan := newPlan(t, ctx, svc, "0.00000005")
	upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
	before := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(upID))
	expireSubscription(t, ctx, db, upID)

	up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); got.Cmp(amount(t, "0.00000002")) != 0 {
		t.Errorf("结转余额 = %s，期望 0.00000002（5 个单位 × 0.5 = 2.5，向下取整；四舍五入会得 3）", got)
	}
	if got := up.CarryoutAmount; got.Cmp(amount(t, "0.00000002")) != 0 {
		t.Errorf("订阅记的结转出去面值 = %s，期望 0.00000002", got)
	}
	if got := up.CarryoutQuota; got.Cmp(amount(t, "0.00000002")) != 0 {
		t.Errorf("金额包的结转出去量 = %s，期望与面值同值 0.00000002", got)
	}
	if got := planVoidedFrom(t, ctx, db, upID); got.Cmp(amount(t, "0.00000003")) != 0 {
		t.Errorf("面值归平台 = %s，期望 0.00000003（5 − 2）", got)
	}
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)).Add(planVoidedFrom(t, ctx, db, upID)); got.Cmp(before) != 0 {
		t.Errorf("结转 + 归平台 = %s，期望处置前的面值余额 %s", got, before)
	}
}

// 金额包的非整元余额按 1e-8 结转，不往整元截：7.35 × 0.4 = 2.94，结转 2.94
// （不是 2），归平台 4.41。
//
// **这条用例只钉守恒，不钉取整方向**：2.94 是精确可表示的，一次都没有截到
// 东西，四舍五入与向下在这里给出同一个数。钉「向下」的是上面两条。
func TestCarryoverAmountKeepsSubYuan(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 4000)

	userID := seedUser(t, ctx, db)
	plan := newPlan(t, ctx, svc, "7.35")
	upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
	before := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(upID))
	expireSubscription(t, ctx, db, upID)

	up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); got.Cmp(amount(t, "2.94")) != 0 {
		t.Errorf("结转余额 = %s，期望 2.94（7.35 × 0.4，不是整元 2）", got)
	}
	if got := up.CarryoutAmount; got.Cmp(amount(t, "2.94")) != 0 {
		t.Errorf("订阅记的结转出去面值 = %s，期望 2.94", got)
	}
	if got := up.CarryoutQuota; got.Cmp(amount(t, "2.94")) != 0 {
		t.Errorf("金额包的结转出去量 = %s，期望与面值同值 2.94", got)
	}
	if got := planVoidedFrom(t, ctx, db, upID); got.Cmp(amount(t, "4.41")) != 0 {
		t.Errorf("面值归平台 = %s，期望 4.41（7.35 − 2.94）", got)
	}
	// 守恒：两笔分录之和 = 处置前的剩余面值，零头没有被吞掉。
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)).Add(planVoidedFrom(t, ctx, db, upID)); got.Cmp(before) != 0 {
		t.Errorf("结转 + 归平台 = %s，期望处置前的面值余额 %s", got, before)
	}
}

// 只有真的到期才结转。canceled、还没到期就标 expired、以及没有到期时刻的，
// 都整笔归平台。
func TestCarryoverRequiresRealExpiry(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 10000)

	userID := seedUser(t, ctx, db)
	plan := newPlan(t, ctx, svc, "100")

	grant := func() int64 {
		return grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
	}

	// 1. 运营提前收回：canceled 不结转，即使 period_end 已经过去。
	canceledID := grant()
	expireSubscription(t, ctx, db, canceledID)
	voidPlan(t, ctx, svc, canceledID, UserPlanCancelled, "void-"+itoa(nextID()))

	// 2. 还没到期就手工标 expired：到期是时间到了的事实，提前标是运营收回。
	earlyID := grant()
	voidPlan(t, ctx, svc, earlyID, UserPlanExpired, "void-"+itoa(nextID()))

	// 3. 没有到期时刻的订阅不存在「到期」这件事（发放时必填，这里模拟旧数据）。
	noEndID := grant()
	if _, err := db.ExecContext(ctx,
		`UPDATE user_plans SET period_end = NULL WHERE id = $1`, noEndID); err != nil {
		t.Fatalf("清掉到期时刻失败: %v", err)
	}
	voidPlan(t, ctx, svc, noEndID, UserPlanExpired, "void-"+itoa(nextID()))

	for _, id := range []int64{canceledID, earlyID, noEndID} {
		if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(id)); !got.IsZero() {
			t.Errorf("订阅 %d 不该结转，carryover 余额 = %s", id, got)
		}
		if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(id)); !got.IsZero() {
			t.Errorf("订阅 %d 的面值应整笔归平台，余额 = %s", id, got)
		}
	}
}

// 两维各自算：token 包的可用量余量与面值余量比例不同时，两个结转量各自
// 等于自己的 floor，谁也不是从另一维推出来的。
func TestCarryoverTwoDimensionsIndependent(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 4000)

	userID := seedUser(t, ctx, db)
	plan := newTokenPlan(t, ctx, svc, "1000", "100")
	upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))

	// 面值用掉 20 → R = 80；可用量用掉 500 → Q = 500。
	consumePrepaid(t, ctx, svc, userID, upID, amount(t, "20"))
	if _, err := db.ExecContext(ctx,
		`UPDATE user_plans SET quota_used = 500 WHERE id = $1`, upID); err != nil {
		t.Fatalf("写已用量失败: %v", err)
	}
	expireSubscription(t, ctx, db, upID)

	up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	// 面值：80 × 0.4 = 32；可用量：500 × 0.4 = 200。比例是同一个 0.4，不同的是
	// 两维各自的**基数**（R = 80、Q = 500）——从一维按比例推另一维会得到 32 或
	// 200 以外的数，因为那等于假设「1 元面值 = 固定多少 token」。
	if got := up.CarryoutAmount; got.Cmp(amount(t, "32")) != 0 {
		t.Errorf("结转出去的面值 = %s，期望 32（80 × 0.4）", got)
	}
	if got := up.CarryoutQuota; got.Cmp(amount(t, "200")) != 0 {
		t.Errorf("结转出去的量 = %s，期望 200（500 × 0.4）", got)
	}
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); got.Cmp(amount(t, "32")) != 0 {
		t.Errorf("账上结转余额 = %s，期望 32（量那一维不进账本）", got)
	}

	// 并入同单位的 token 包：量从 carryout_quota 读（账本没有 unit），
	// 面值从 carryover 科目余额读。
	newToken := newTokenPlan(t, ctx, svc, "1000", "100")
	newID := grantFresh(t, ctx, svc, userID, newToken.Key, "grant-"+itoa(nextID()))
	var carriedQuota, carriedAmount string
	if err := db.QueryRowContext(ctx,
		`SELECT carried_quota::text, carried_amount::text FROM user_plans WHERE id = $1`,
		newID).Scan(&carriedQuota, &carriedAmount); err != nil {
		t.Fatalf("读新订阅的 carried 两列失败: %v", err)
	}
	if carriedQuota != "200.00000000" || carriedAmount != "32.00000000" {
		t.Errorf("新订阅的 carried = (%s, %s)，期望 (200, 32)", carriedQuota, carriedAmount)
	}
	if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(newID)); got.Cmp(amount(t, "132")) != 0 {
		t.Errorf("新订阅面值 = %s，期望 132（发放 100 + 并入 32）", got)
	}
}

// 次数包（UnitRequest）与 token 包走同一档取整：剩余 1 次、比例 0.4 → 0.4 次，
// 向下取整到整次得 0。于是这一次也不结转，整份面值归平台。
//
// 全仓此前没有一条结转用例落在 request 上（`carryShare` 里它和 token 同分支，
// 但没人钉过这个边界）。取整到 0 的代价见 carrySplit 的注释：量跟着没了，
// 账上看不出来。
func TestCarryoverRequestUnitRoundsToZero(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 4000)

	userID := seedUser(t, ctx, db)
	plan := newRequestPlan(t, ctx, svc, "1", "1")
	upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
	expireSubscription(t, ctx, db, upID)

	up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	if got := up.CarryoutQuota; !got.IsZero() {
		t.Errorf("结转出去的量 = %s，期望 0（1 次 × 0.4 = 0.4，向下取整到整次）", got)
	}
	if got := up.CarryoutAmount; !got.IsZero() {
		t.Errorf("结转出去的面值 = %s，期望 0（量落零时面值也不结转）", got)
	}
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); !got.IsZero() {
		t.Errorf("结转余额 = %s，期望 0", got)
	}
	if got := planVoidedFrom(t, ctx, db, upID); got.Cmp(amount(t, "1")) != 0 {
		t.Errorf("面值归平台 = %s，期望 1（整份，没有结转）", got)
	}
}

// token 包的结转基数：上期结转进来 100、本期发放 100、本期用掉 50 →
// 量那一维结转 20，不是 0。
//
// 这一格钉的是「结转进来的量不计入本期可结转的基数」：可用量 = 总量 100 +
// 结转 100 − 已用 50 = 150，减掉结转进来的 100 得 50，× 0.4 = 20。基数若把
// carried_quota 多减一次（= 100 − 50 − 100 < 0），结转会落成 0，而它正是
// issue 举的那组数。
//
// 金额包走不到这一格（carrySplit 对 UnitAmount 提前返回），所以这里必须是
// token 包；现有其它用例的 carried 都是 0。
func TestCarryoverTokenQuotaBaseExcludesCarried(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)

	userID := seedUser(t, ctx, db)

	// 第一期末：全部结转（比例 1），留下量 100、面值 100。
	setCarryoverBP(t, ctx, db, 10000)
	source := newTokenPlan(t, ctx, svc, "100", "100")
	sourceID := grantFresh(t, ctx, svc, userID, source.Key, "grant-"+itoa(nextID()))
	expireSubscription(t, ctx, db, sourceID)
	voidPlan(t, ctx, svc, sourceID, UserPlanExpired, "void-"+itoa(nextID()))

	// 本期：领一个新包，100 的量与 100 的面值并进来。
	setCarryoverBP(t, ctx, db, 4000)
	fresh := newTokenPlan(t, ctx, svc, "100", "100")
	upID := grantFresh(t, ctx, svc, userID, fresh.Key, "grant-"+itoa(nextID()))
	var carriedQuota, carriedAmount string
	if err := db.QueryRowContext(ctx,
		`SELECT carried_quota::text, carried_amount::text FROM user_plans WHERE id = $1`,
		upID).Scan(&carriedQuota, &carriedAmount); err != nil {
		t.Fatalf("读新订阅的 carried 两列失败: %v", err)
	}
	if carriedQuota != "100.00000000" || carriedAmount != "100.00000000" {
		t.Fatalf("前置条件不成立：新订阅的 carried = (%s, %s)，期望 (100, 100)",
			carriedQuota, carriedAmount)
	}

	// 本期用掉 50：面值扣 50，已用量记 50。
	consumePrepaid(t, ctx, svc, userID, upID, amount(t, "50"))
	if _, err := db.ExecContext(ctx,
		`UPDATE user_plans SET quota_used = 50 WHERE id = $1`, upID); err != nil {
		t.Fatalf("写已用量失败: %v", err)
	}
	expireSubscription(t, ctx, db, upID)

	up := voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	// 面值：200 − 50 = 150，减掉结转进来的 100 得 50，× 0.4 = 20。
	if got := up.CarryoutAmount; got.Cmp(amount(t, "20")) != 0 {
		t.Errorf("结转出去的面值 = %s，期望 20（(150 − 100) × 0.4）", got)
	}
	// 量：100 + 100 − 50 = 150，减掉结转进来的 100 得 50，× 0.4 = 20。
	if got := up.CarryoutQuota; got.Cmp(amount(t, "20")) != 0 {
		t.Errorf("结转出去的量 = %s，期望 20（(100 + 100 − 50 − 100) × 0.4）", got)
	}
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); got.Cmp(amount(t, "20")) != 0 {
		t.Errorf("账上结转余额 = %s，期望 20", got)
	}
}

// 比例读不出来时整次作废失败，订阅与账本都不动——不回落成 0。
func TestCarryoverRateFailureLeavesStateUntouched(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	cases := []struct {
		name    string
		corrupt func(t *testing.T, db *sql.DB)
	}{
		{"行不存在", func(t *testing.T, db *sql.DB) {
			if _, err := db.ExecContext(context.Background(),
				`DELETE FROM settings WHERE key = $1`, carryoverBpKey); err != nil {
				t.Fatalf("删比例行失败: %v", err)
			}
		}},
		{"不是整数", func(t *testing.T, db *sql.DB) {
			if _, err := db.ExecContext(context.Background(),
				`UPDATE settings SET value = '"四成"'::jsonb WHERE key = $1`, carryoverBpKey); err != nil {
				t.Fatalf("改比例失败: %v", err)
			}
		}},
		{"超出上界", func(t *testing.T, db *sql.DB) {
			if _, err := db.ExecContext(context.Background(),
				`UPDATE settings SET value = '10001'::jsonb WHERE key = $1`, carryoverBpKey); err != nil {
				t.Fatalf("改比例失败: %v", err)
			}
		}},
		{"负数", func(t *testing.T, db *sql.DB) {
			if _, err := db.ExecContext(context.Background(),
				`UPDATE settings SET value = '-1'::jsonb WHERE key = $1`, carryoverBpKey); err != nil {
				t.Fatalf("改比例失败: %v", err)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子用例都从 4000 出发，结束时由 setCarryoverBP 的收尾还原。
			setCarryoverBP(t, ctx, db, 4000)
			svc := New(db)
			userID := seedUser(t, ctx, db)
			plan := newPlan(t, ctx, svc, "100")
			upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
			expireSubscription(t, ctx, db, upID)

			tc.corrupt(t, db)

			_, applied, err := svc.Void(ctx, VoidInput{
				TxnID: newTxnID(), UserPlanID: upID,
				Ref: "void-" + itoa(nextID()), Status: UserPlanExpired,
			})
			if err == nil {
				t.Fatal("比例读不出来时作废应当失败，实际成功")
			}
			if applied {
				t.Error("失败的作废不该返回 applied=true")
			}
			if !strings.Contains(err.Error(), carryoverBpKey) {
				t.Errorf("错误信息没有点名 %s: %v", carryoverBpKey, err)
			}
			if !strings.Contains(err.Error(), "本次作废未执行") {
				t.Errorf("错误信息没有说清这次作废没有执行: %v", err)
			}

			// 三样都没动：状态、面值、分录。
			var (
				status  string
				voidRef sql.NullString
			)
			if err := db.QueryRowContext(ctx,
				`SELECT status, void_ref FROM user_plans WHERE id = $1`, upID).Scan(&status, &voidRef); err != nil {
				t.Fatalf("读订阅失败: %v", err)
			}
			if status != string(UserPlanActive) || voidRef.Valid {
				t.Errorf("订阅状态 = %s、void_ref = %v；期望仍是 active 且没有作废单号",
					status, voidRef)
			}
			if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(upID)); got.Cmp(amount(t, "100")) != 0 {
				t.Errorf("面值余额 = %s，期望原样 100", got)
			}
			if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(upID)); !got.IsZero() {
				t.Errorf("失败时不该写结转余额，实际 %s", got)
			}
			var n int
			if err := db.QueryRowContext(ctx, `
				SELECT count(*) FROM ledger_entries
				 WHERE account = $1 AND entry_type IN ('plan_carry', 'plan_void')`,
				ledger.PrepaidAccount(upID).String()).Scan(&n); err != nil {
				t.Fatalf("数分录失败: %v", err)
			}
			if n != 0 {
				t.Errorf("失败的作废写了 %d 条结转 / 作废分录，期望 0", n)
			}
		})
	}
}

// TestWholeUnitUnits 按值钉住「一个整单位」。
//
// 期望值写字面量，不从 money.Scale 推：从被测来源算期望，等于把同一个错误抄两遍。
//
// 循环改成 `for range money.Scale` 之后，原先 `i < money.Scale` 上那两条边界变异
// （`<=` 会多乘一次得 1e9，`>=` 一次不乘得 1）连同那句死循环形态一起消失，而它们
// 此前是被用例杀掉的。这条值断言就是把那两格证据补回来。单独成一个用例，是为了
// 它能单跑、不连库，红证好贴。
func TestWholeUnitUnits(t *testing.T) {
	if wholeUnitUnits != 100_000_000 {
		t.Errorf("一个整单位在定点数里的值 = %d，期望 100000000（10^8，即 money.Scale=8）", wholeUnitUnits)
	}
}

// 结转 → 并入 → 二次期末，一条链走完；顺带钉住两件事：
//
//   - 上期结转进来的钱不再结转第二次（剩余正好等于结转进来的量时，结转恰好为 0）；
//   - 发放重放不二次并入，源余额已清零时第二次发放也不并。
//
// # 为什么 mergeCarryovers 开头那条 `err != nil || len(ids) == 0` 上的
// # `||` → `&&` 是等价变异
//
// 两半都要成立，缺一不可：
//
//   - ① 「`err != nil` 且 `ids` 非空」这一格不存在：pendingCarryoverIDs 的错误
//     路径全部 `return nil, err`——查询失败、逐行 Scan 失败，以及收完行之后
//     `rows.Err()` 那条；最后一条返回的也是 `nil`，而不是已经攒了一半的 `out`。
//   - ② 「`err == nil && len(ids) == 0`」这一格，`&&` 版会继续往下走，但
//     `ordered` 与 `sources` 都是空的，两段循环一次都不进（一个 DB 也不碰），
//     最后被 `len(sources) == 0` 那道返回接住，给出同一对零值与 nil 错误。
//
// **② 那道守卫是这条等价的前提**：把它删掉，变异体立刻变成真缺陷——空的 moves
// 会一路走到 PlanCarryMerge，构造出一笔没有分录的交易。
func TestCarryoverFullCycleAndIdempotency(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 10000)

	userID := seedUser(t, ctx, db)
	plan := newPlan(t, ctx, svc, "100")

	// 第一次期末：100 全部结转。
	ref1 := "grant-" + itoa(nextID())
	sub1 := grantFresh(t, ctx, svc, userID, plan.Key, ref1)
	expireSubscription(t, ctx, db, sub1)
	void1 := voidPlan(t, ctx, svc, sub1, UserPlanExpired, "void-"+itoa(nextID()))
	if void1.CarryoutAmount.Cmp(amount(t, "100")) != 0 {
		t.Fatalf("第一次期末结转 = %s，期望 100", void1.CarryoutAmount)
	}
	if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(sub1)); !got.IsZero() {
		t.Fatalf("第一次期末之后源订阅面值 = %s，期望 0", got)
	}

	// 领新包：100 发放 + 100 并入。
	ref2 := "grant-" + itoa(nextID())
	expires2 := time.Now().Add(time.Hour)
	up2, applied2, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref2, UserID: userID, PlanKey: plan.Key, ExpiresAt: expires2,
	})
	if err != nil || !applied2 {
		t.Fatalf("第二次发放失败: applied=%v err=%v", applied2, err)
	}
	sub2 := up2.ID
	if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(sub2)); got.Cmp(amount(t, "200")) != 0 {
		t.Fatalf("新订阅面值 = %s，期望 200（发放 100 + 并入 100）", got)
	}
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(sub1)); !got.IsZero() {
		t.Fatalf("并入之后源结转余额 = %s，期望 0", got)
	}
	mergeTxn := carryTxnID(carryMergeTxnDomain, ref2)
	if n := countEntries(t, db, mergeTxn); n != 2 {
		t.Fatalf("并入交易 %s 有 %d 条分录，期望 2（一个源一条贷方 + 目标一条借方）",
			mergeTxn, n)
	}

	// 把上期结转 100 对应的那份额度用掉：剩余 100，正好等于 carried_amount。
	consumePrepaid(t, ctx, svc, userID, sub2, amount(t, "100"))
	if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(sub2)); got.Cmp(amount(t, "100")) != 0 {
		t.Fatalf("用掉 100 后面值 = %s，期望 100", got)
	}

	// 发放重放：早返回，面值、carried 与分录都不变。
	before := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(sub2))
	up, applied, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref2, UserID: userID, PlanKey: plan.Key, ExpiresAt: expires2,
	})
	if err != nil || applied {
		t.Fatalf("重放发放应 applied=false 且无错，得到 applied=%v err=%v", applied, err)
	}
	if up.CarriedAmount.Cmp(amount(t, "100")) != 0 {
		t.Errorf("重放之后 carried_amount = %s，期望仍是 100（没有二次并入）", up.CarriedAmount)
	}
	if got := ledgerBalance(t, ctx, db, ledger.PrepaidAccount(sub2)); got.Cmp(before) != 0 {
		t.Errorf("重放改动面值：%s → %s", before, got)
	}
	if n := countEntries(t, db, mergeTxn); n != 2 {
		t.Errorf("重放之后并入交易有 %d 条分录，期望仍是 2", n)
	}

	// 第二次期末：结转恰好为 0——那 100 是结转进来的，不再结转第二次。
	expireSubscription(t, ctx, db, sub2)
	void2 := voidPlan(t, ctx, svc, sub2, UserPlanExpired, "void-"+itoa(nextID()))
	if !void2.CarryoutAmount.IsZero() {
		t.Errorf("第二次期末结转出去的面值 = %s，期望恰好 0", void2.CarryoutAmount)
	}
	if !void2.CarryoutQuota.IsZero() {
		t.Errorf("第二次期末结转出去的量 = %s，期望恰好 0", void2.CarryoutQuota)
	}
	if got := ledgerBalance(t, ctx, db, ledger.CarryoverAccount(sub2)); !got.IsZero() {
		t.Errorf("第二次期末之后结转余额 = %s，期望 0", got)
	}

	// 再领一个包：源（sub1）还有 carryout_amount = 100，但账本余额已清零，
	// 并入的闸门是余额——这次一条都不并，量也不并。
	ref3 := "grant-" + itoa(nextID())
	sub3 := grantFresh(t, ctx, svc, userID, plan.Key, ref3)
	var cq, ca string
	if err := db.QueryRowContext(ctx,
		`SELECT carried_quota::text, carried_amount::text FROM user_plans WHERE id = $1`,
		sub3).Scan(&cq, &ca); err != nil {
		t.Fatalf("读新订阅的 carried 两列失败: %v", err)
	}
	if cq != "0.00000000" || ca != "0.00000000" {
		t.Errorf("零余额的源不该并入，新订阅 carried = (%s, %s)", cq, ca)
	}
	if n := countEntries(t, db, carryTxnID(carryMergeTxnDomain, ref3)); n != 0 {
		t.Errorf("零余额的源写入了 %d 条并入分录，期望 0", n)
	}
}

// 待并入的余额要出现在账本汇总里，不能落进「认不出的科目」。
func TestCarryoverVisibleInAccountBalances(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	svc := New(db)
	setCarryoverBP(t, ctx, db, 4000)

	userID := seedUser(t, ctx, db)
	plan := newPlan(t, ctx, svc, "100")
	upID := grantFresh(t, ctx, svc, userID, plan.Key, "grant-"+itoa(nextID()))
	expireSubscription(t, ctx, db, upID)
	voidPlan(t, ctx, svc, upID, UserPlanExpired, "void-"+itoa(nextID()))

	report, err := store.New(db).AccountBalances(ctx, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("读账本汇总失败: %v", err)
	}
	acct := ledger.CarryoverAccount(upID)
	got, ok := report.ByAccount[ledger.AssetCNY][acct]
	if !ok {
		t.Fatalf("账本汇总里没有结转科目 %s", acct)
	}
	if got.Cmp(amount(t, "40")) != 0 {
		t.Errorf("汇总里 %s 的余额 = %s，期望 40", acct, got)
	}
	for _, raw := range report.Unreadable {
		if strings.Contains(raw, "carryover") {
			t.Errorf("结转科目落进了「认不出的科目」：%q", raw)
		}
	}
}

// planVoidedFrom 读一条订阅的 prepaid 上由 plan_void 核销掉的金额之和。
//
// 按订阅而不是按平台科目：平台科目是全局的，同一个包里别的用例也会往里写。
// ctx 由调用方传进来（理由见 seedUser）。
func planVoidedFrom(t *testing.T, ctx context.Context, db *sql.DB, userPlanID int64) money.Amount {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		  FROM ledger_entries
		 WHERE account = $1 AND asset = 'CNY' AND entry_type = 'plan_void'`,
		ledger.PrepaidAccount(userPlanID).String()).Scan(&raw); err != nil {
		t.Fatalf("读订阅 %d 的核销分录失败: %v", userPlanID, err)
	}
	got, err := money.Parse(raw)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", raw, err)
	}
	return got.Neg() // 贷方记的是负数
}
