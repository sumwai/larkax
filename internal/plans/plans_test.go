package plans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/testdb"
)

// 本包的每一条用例都要真库：订阅行、账本分录与唯一索引是同一件事的三面。
//
// 拿内存实现假装，测的是「我以为数据库会怎么做」——「同一个单号只能发一次」
// 靠的是唯一索引，不是 Go 里的 map。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// 每个用例用独立的用户与额度包 key，避免彼此干扰。
var seq struct {
	sync.Mutex
	n int64
}

var base = time.Now().UnixNano() % 1_000_000_000

func nextID() int64 {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return base*1000 + seq.n
}

func itoa(v int64) string { return fmt.Sprintf("%d", v) }

// seedUser 建一个用户，返回 id。发放的 user_id 有外键指向 users。
func seedUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id := 100000 + nextID()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "u"+itoa(id), "u"+itoa(id)+"@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// amount 解析一个测试金额。
func amount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatalf("测试金额 %q 不合法: %v", s, err)
	}
	return a
}

// ledgerBalance 读一个科目在 CNY 上的余额（分录之和）。
//
// 直接查库而不是走 store.AccountBalance：用例要断言的是「账本里写进去的那个和」，
// 用被测的读路径去验被测的写路径，读错与写错会互相抵消。
func ledgerBalance(t *testing.T, db *sql.DB, acct ledger.Account) money.Amount {
	t.Helper()
	var raw string
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		   FROM ledger_entries WHERE account = $1 AND asset = 'CNY'`,
		acct.String()).Scan(&raw); err != nil {
		t.Fatalf("读科目 %s 的余额失败: %v", acct, err)
	}
	bal, err := money.Parse(raw)
	if err != nil {
		t.Fatalf("解析科目 %s 的余额 %q 失败: %v", acct, raw, err)
	}
	return bal
}

// countEntries 数某个交易号写了几条分录。重放不该多出来。
func countEntries(t *testing.T, db *sql.DB, txn ledger.TxnID) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1`, string(txn)).Scan(&n); err != nil {
		t.Fatalf("数交易 %s 的分录失败: %v", txn, err)
	}
	return n
}

// countUserPlansByRef 数某个发放单号建了几条订阅。
func countUserPlansByRef(t *testing.T, db *sql.DB, ref string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM user_plans WHERE ref = $1`, ref).Scan(&n); err != nil {
		t.Fatalf("数发放单号 %q 的订阅失败: %v", ref, err)
	}
	return n
}

// newPlan 建一个金额包（最常用的夹具）。
func newPlan(t *testing.T, ctx context.Context, svc *Service, face string) Plan {
	t.Helper()
	p, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key:        "plan-" + itoa(nextID()),
		Name:       "测试金额包",
		Asset:      ledger.AssetCNY,
		Unit:       UnitAmount,
		QuotaTotal: amount(t, face),
	})
	if err != nil {
		t.Fatalf("建额度包失败: %v", err)
	}
	return p
}

// newTxnID 生成一个合法的交易号。
func newTxnID() ledger.TxnID { return ledger.NewTxnID() }

// token 包与次数包没有模型范围时建不出来；金额包可以（空 = 不限模型）。
func TestCreatePlanRequiresModelsForQuotaUnits(t *testing.T) {
	ctx := context.Background()
	svc := New(testDB(t))

	for _, unit := range []PlanUnit{UnitToken, UnitRequest} {
		_, err := svc.CreatePlan(ctx, CreatePlanInput{
			Key: "plan-" + itoa(nextID()), Name: "无范围的包", Asset: ledger.AssetCNY,
			Unit: unit, QuotaTotal: amount(t, "100"), FaceValue: amount(t, "10"),
		})
		if !errors.Is(err, ErrPlanScope) {
			t.Errorf("%s 包没有模型范围应当被拒（ErrPlanScope），得到: %v", unit, err)
		}
	}

	p, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "不限模型的金额包", Asset: ledger.AssetCNY,
		Unit: UnitAmount, QuotaTotal: amount(t, "100"),
	})
	if err != nil {
		t.Fatalf("金额包允许模型范围为空: %v", err)
	}
	if len(p.Models) != 0 {
		t.Errorf("金额包的模型范围应当为空，得到 %v", p.Models)
	}
}

// 金额包的可用量与面值是同一个数：只填一个时补齐，两个都给时必须相等。
func TestCreatePlanFillsAmountPackage(t *testing.T) {
	ctx := context.Background()
	svc := New(testDB(t))

	onlyTotal, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "只填可用量", Asset: ledger.AssetCNY,
		Unit: UnitAmount, QuotaTotal: amount(t, "100"),
	})
	if err != nil {
		t.Fatalf("只填可用量的金额包应当能建: %v", err)
	}
	if onlyTotal.FaceValue.Cmp(amount(t, "100")) != 0 {
		t.Errorf("面值 = %s，期望补齐成 100", onlyTotal.FaceValue)
	}

	onlyFace, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "只填面值", Asset: ledger.AssetCNY,
		Unit: UnitAmount, FaceValue: amount(t, "50"),
	})
	if err != nil {
		t.Fatalf("只填面值的金额包应当能建: %v", err)
	}
	if onlyFace.QuotaTotal.Cmp(amount(t, "50")) != 0 {
		t.Errorf("可用量 = %s，期望补齐成 50", onlyFace.QuotaTotal)
	}

	_, err = svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "两个数不一致", Asset: ledger.AssetCNY,
		Unit: UnitAmount, QuotaTotal: amount(t, "100"), FaceValue: amount(t, "80"),
	})
	if err == nil {
		t.Fatal("金额包的可用量与面值不一致应当被拒")
	}
	// 两个数不等时，这一句必须由这里挡下来。库里还有一道
	// plans_amount_quota_matches_face_value 的 CHECK，不一致的 INSERT 同样会失败，
	// 所以只看 err != nil 会把两道守卫混成一道：拆掉这一句，错误变成约束冲突，
	// 用例仍然绿——钉住文案才分得清是谁拦的。
	if !strings.Contains(err.Error(), "必须相等") {
		t.Errorf("两个数不一致的错误 = %q，期望点出「必须相等」", err)
	}
}

// 「两个都不给」不能落到「可用量必须为正数」那一句上：那会把「没填」说成
// 「填了非正数」，调用方照着改也改不对。金额包在这一档里两个数本来是一回事，
// 缺的是值本身，所以错误要指向「必须给出可用量（或面值）」。
func TestCreatePlanRejectsAmountPackageWithoutAnyValue(t *testing.T) {
	ctx := context.Background()
	svc := New(testDB(t))

	_, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "两个都不给", Asset: ledger.AssetCNY,
		Unit: UnitAmount,
	})
	if err == nil {
		t.Fatal("金额包两个数都不给应当被拒")
	}
	if !strings.Contains(err.Error(), "必须给出可用量") {
		t.Errorf("两个数都不给的错误 = %q，期望点出「必须给出可用量」", err)
	}
	if strings.Contains(err.Error(), "必须为正数") {
		t.Errorf("两个数都不给的错误 = %q；这里没有「填了个非正数」，不该走正数校验那一条", err)
	}
}

// 模型范围去重并按字典序落库：同一个包在两次创建里应当得到同一份范围。
func TestCreatePlanNormalizesModels(t *testing.T) {
	ctx := context.Background()
	svc := New(testDB(t))

	p, err := svc.CreatePlan(ctx, CreatePlanInput{
		Key: "plan-" + itoa(nextID()), Name: "token 包", Asset: ledger.AssetCNY,
		Unit: UnitToken, QuotaTotal: amount(t, "1000000"), FaceValue: amount(t, "20"),
		Models: []string{"vendor/b", "vendor/a", "vendor/b"},
	})
	if err != nil {
		t.Fatalf("建 token 包失败: %v", err)
	}
	want := []string{"vendor/a", "vendor/b"}
	if len(p.Models) != len(want) || p.Models[0] != want[0] || p.Models[1] != want[1] {
		t.Errorf("模型范围 = %v，期望 %v（去重并按字典序）", p.Models, want)
	}

	got, err := svc.PlanByKey(ctx, p.Key)
	if err != nil {
		t.Fatalf("按 key 读回额度包失败: %v", err)
	}
	if len(got.Models) != len(want) || got.Models[0] != want[0] || got.Models[1] != want[1] {
		t.Errorf("读回的模型范围 = %v，期望 %v", got.Models, want)
	}
	if got.Unit != UnitToken || got.QuotaTotal.Cmp(amount(t, "1000000")) != 0 ||
		got.FaceValue.Cmp(amount(t, "20")) != 0 {
		t.Errorf("读回的额度包与写入的不一致: %+v", got)
	}
}

// 发放：一行订阅 + 一笔授予分录，面值进 prepaid:<订阅 id>。
func TestGrantBooksFaceValue(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	ref := "grant-" + itoa(nextID())
	txn := newTxnID()
	// 平台收入是全局科目，同包的别的用例也往上面记；判据只能看这一条用例
	// 写进去的变化，不能看整库的余额。
	before := ledgerBalance(t, db, ledger.PlatformRevenueAccount())
	up, applied, err := svc.Grant(ctx, GrantInput{
		TxnID: txn, Ref: ref, UserID: userID, PlanKey: plan.Key,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	if !applied {
		t.Error("第一次发放应当 applied=true")
	}
	if up.Status != UserPlanActive || up.Ref != ref || up.UserID != userID || up.PlanID != plan.ID {
		t.Errorf("订阅行不对: %+v", up)
	}
	if bal := ledgerBalance(t, db, ledger.PrepaidAccount(up.ID)); bal.Cmp(amount(t, "100")) != 0 {
		t.Errorf("prepaid:%d 余额 = %s，期望 100", up.ID, bal)
	}
	after := ledgerBalance(t, db, ledger.PlatformRevenueAccount())
	if delta := after.Sub(before); delta.Cmp(amount(t, "-100")) != 0 {
		t.Errorf("发放让平台收入变化 %s，期望 -100（让出去的额度）", delta)
	}
	if got := countEntries(t, db, txn); got != 2 {
		t.Errorf("授予分录 %d 条，期望 2（prepaid + platform_revenue）", got)
	}
	if up.FaceValueRemaining.Cmp(amount(t, "100")) != 0 {
		t.Errorf("读回的剩余面值 = %s，期望 100", up.FaceValueRemaining)
	}
}

// 同一个发放单号重放：不写新订阅、不重复记账，返回同一个订阅。
func TestGrantIsIdempotentByRef(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	ref := "grant-" + itoa(nextID())
	txn := newTxnID()
	expires := time.Now().Add(time.Hour)
	first, _, err := svc.Grant(ctx, GrantInput{
		TxnID: txn, Ref: ref, UserID: userID, PlanKey: plan.Key, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("首次发放失败: %v", err)
	}

	// 重放：同一个单号、同一个交易号、同样的关键字段。
	again, applied, err := svc.Grant(ctx, GrantInput{
		TxnID: txn, Ref: ref, UserID: userID, PlanKey: plan.Key, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if applied {
		t.Error("重放应当 applied=false")
	}
	if again.ID != first.ID {
		t.Errorf("重放返回的订阅 id = %d，期望 %d", again.ID, first.ID)
	}
	if got := countUserPlansByRef(t, db, ref); got != 1 {
		t.Errorf("发放单号 %q 建了 %d 条订阅，期望 1", ref, got)
	}
	if got := countEntries(t, db, txn); got != 2 {
		t.Errorf("重放后授予分录 %d 条，期望仍是 2", got)
	}
	if bal := ledgerBalance(t, db, ledger.PrepaidAccount(first.ID)); bal.Cmp(amount(t, "100")) != 0 {
		t.Errorf("重放后 prepaid 余额 = %s，期望仍为 100", bal)
	}

	// 同一个单号配不同的到期时刻：不是重放，是冲突。
	_, _, err = svc.Grant(ctx, GrantInput{
		TxnID: txn, Ref: ref, UserID: userID, PlanKey: plan.Key,
		ExpiresAt: expires.Add(time.Hour),
	})
	if !errors.Is(err, store.ErrTxnConflict) {
		t.Errorf("同一单号配不同到期时刻应当是冲突，得到: %v", err)
	}
}

// 同一个发放单号配不同的用户或额度包：不是重放，是冲突——那说明调用方把两个
// 不同的事件映射到了同一个单号上。
func TestGrantRejectsSameRefDifferentUserOrPlan(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	otherUser := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")
	otherPlan := newPlan(t, ctx, svc, "50")

	ref := "grant-" + itoa(nextID())
	expires := time.Now().Add(time.Hour)
	if _, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref, UserID: userID, PlanKey: plan.Key, ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("首次发放失败: %v", err)
	}

	if _, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref, UserID: otherUser, PlanKey: plan.Key, ExpiresAt: expires,
	}); !errors.Is(err, store.ErrTxnConflict) {
		t.Errorf("同一单号换用户应当是冲突，得到: %v", err)
	}
	if _, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref, UserID: userID, PlanKey: otherPlan.Key, ExpiresAt: expires,
	}); !errors.Is(err, store.ErrTxnConflict) {
		t.Errorf("同一单号换额度包应当是冲突，得到: %v", err)
	}
	if got := countUserPlansByRef(t, db, ref); got != 1 {
		t.Errorf("冲突之后该单号下有 %d 条订阅，期望仍是 1", got)
	}
}

// 重放到达时那份额度可能已经过期了，它仍然是同一次发放——「有效期必须在将来」
// 只对新发放成立，否则一次迟到重放会被判成「发出去就已经到期」，调用方拿不到
// 「已经发过」这个答案。
func TestGrantReplayAfterExpirySucceeds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	ref := "grant-" + itoa(nextID())
	expires := time.Now().Add(-time.Hour)
	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO user_plans (user_id, plan_id, status, period_start, period_end, ref)
		VALUES ($1, $2, 'expired', $3, $4, $5)
		RETURNING id`,
		userID, plan.ID, expires.Add(-time.Hour), expires, ref).Scan(&id); err != nil {
		t.Fatalf("手工写订阅失败: %v", err)
	}

	row, applied, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref, UserID: userID, PlanKey: plan.Key, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("过期之后的重放不该报错: %v", err)
	}
	if applied {
		t.Error("重放应当 applied=false")
	}
	if row.ID != id {
		t.Errorf("重放返回的订阅 id = %d，期望 %d", row.ID, id)
	}
	if got := countUserPlansByRef(t, db, ref); got != 1 {
		t.Errorf("重放又建了一条订阅，该单号下有 %d 条", got)
	}
}

// 库里被手工写进来的「没有模型范围的 token 包」发不出去：写入时那道校验
// 挡不住绕过它的写路径，发放这一层要再查一次。
func TestGrantRejectsScopeViolationAtWriteTime(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)

	key := "plan-" + itoa(nextID())
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plans (key, name, asset, unit, quota_total, face_value)
		VALUES ($1, '手工写的 token 包', 'CNY', 'token', 1000, 10)`, key); err != nil {
		t.Fatalf("手工写额度包失败: %v", err)
	}

	_, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID, PlanKey: key,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrPlanScope) {
		t.Errorf("没有模型范围的 token 包发放应当被拒（ErrPlanScope），得到: %v", err)
	}
}

// 禁用的包不能再发；已经发出去的订阅不受影响（见 SetPlanEnabled 的注释）。
func TestGrantRejectsDisabledPlan(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	if _, err := svc.SetPlanEnabled(ctx, SetPlanEnabledInput{Key: plan.Key, Enabled: false}); err != nil {
		t.Fatalf("禁用额度包失败: %v", err)
	}
	_, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID, PlanKey: plan.Key,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Error("禁用的额度包发放应当被拒")
	}
}

// SetPlanEnabled 天然幂等：重复设成同一个值返回同一行，不会多出一次改动。
func TestSetPlanEnabledIsIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	plan := newPlan(t, ctx, svc, "100")

	first, err := svc.SetPlanEnabled(ctx, SetPlanEnabledInput{Key: plan.Key, Enabled: false})
	if err != nil {
		t.Fatalf("禁用额度包失败: %v", err)
	}
	second, err := svc.SetPlanEnabled(ctx, SetPlanEnabledInput{Key: plan.Key, Enabled: false})
	if err != nil {
		t.Fatalf("重复禁用不该报错: %v", err)
	}
	if first.ID != second.ID || first.Enabled || second.Enabled {
		t.Errorf("重复禁用的结果不一致: %+v / %+v", first, second)
	}

	again, err := svc.SetPlanEnabled(ctx, SetPlanEnabledInput{Key: plan.Key, Enabled: true})
	if err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}
	if !again.Enabled {
		t.Error("重新启用之后 Enabled 仍然是 false")
	}
}

// 发放 → 从额度包消费 → 作废：prepaid 依次是 100 / 70 / 0，平台收入走完归零。
func TestLifecycleBalances(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	// 平台收入是全局科目：只判这三条分录让它的净变化为零。
	before := ledgerBalance(t, db, ledger.PlatformRevenueAccount())

	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	prepaid := ledger.PrepaidAccount(up.ID)

	consumeTxn, err := ledger.Consume(ledger.ConsumeParams{
		TxnID: newTxnID(), UserID: userID, Amount: amount(t, "30"), Asset: ledger.AssetCNY,
		PrepaidAccount: prepaid,
	})
	if err != nil {
		t.Fatalf("构造额度包消费失败: %v", err)
	}
	if _, postErr := store.New(db).PostTransaction(ctx, consumeTxn); postErr != nil {
		t.Fatalf("记额度包消费失败: %v", postErr)
	}
	if bal := ledgerBalance(t, db, prepaid); bal.Cmp(amount(t, "70")) != 0 {
		t.Fatalf("消费后 prepaid 余额 = %s，期望 70", bal)
	}

	voidRef := "void-" + itoa(nextID())
	voided, applied, err := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: up.ID, Ref: voidRef, Status: UserPlanExpired,
	})
	if err != nil {
		t.Fatalf("作废失败: %v", err)
	}
	if !applied {
		t.Error("第一次作废应当 applied=true")
	}
	if voided.Status != UserPlanExpired || voided.VoidRef != voidRef {
		t.Errorf("作废后的订阅不对: %+v", voided)
	}
	if bal := ledgerBalance(t, db, prepaid); !bal.IsZero() {
		t.Errorf("作废后 prepaid 余额 = %s，期望 0", bal)
	}
	if bal := ledgerBalance(t, db, ledger.PlatformRevenueAccount()); bal.Sub(before) != money.Zero {
		t.Errorf("走完一轮后平台收入变化 %s，期望 0（消费记了 30、作废收回 70）",
			bal.Sub(before))
	}
	if voided.FaceValueRemaining.Cmp(money.Zero) != 0 {
		t.Errorf("读回的剩余面值 = %s，期望 0", voided.FaceValueRemaining)
	}
}

// 作废要先读余额再写分录，而消费会让余额变小：两条路必须排在同一把科目锁上。
// 没有这把锁时，作废会拿着读到的旧余额去核销，中间消费又扣一次，prepaid 成负数。
//
// 判据用「挡没挡住」而不是「两个 goroutine 各跑一次看结果」：结果那条要先制造
// 交错才看得出错，而这里只要消费事务没提交，作废就必须在锁上等着。
func TestVoidWaitsForConcurrentPrepaidWriter(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	prepaid := ledger.PrepaidAccount(up.ID)

	consumeTxn, err := ledger.Consume(ledger.ConsumeParams{
		TxnID: newTxnID(), UserID: userID, Amount: amount(t, "30"), Asset: ledger.AssetCNY,
		PrepaidAccount: prepaid,
	})
	if err != nil {
		t.Fatalf("构造额度包消费失败: %v", err)
	}

	// 消费写在一个未提交的事务里：科目锁随它一起被持有到提交。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启消费事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, postErr := store.New(db).PostTransactionTx(ctx, tx, consumeTxn); postErr != nil {
		t.Fatalf("在事务里记额度包消费失败: %v", postErr)
	}

	type voidResult struct {
		row     UserPlan
		applied bool
		err     error
	}
	done := make(chan voidResult, 1)
	go func() {
		row, applied, voidErr := svc.Void(ctx, VoidInput{
			TxnID: newTxnID(), UserPlanID: up.ID, Ref: "void-" + itoa(nextID()),
			Status: UserPlanExpired,
		})
		done <- voidResult{row: row, applied: applied, err: voidErr}
	}()

	select {
	case r := <-done:
		t.Fatalf("消费事务还没提交，作废就完成了（err=%v）——写分录时没有锁住 prepaid 科目", r.err)
	case <-time.After(300 * time.Millisecond):
	}

	if commitErr := tx.Commit(); commitErr != nil {
		t.Fatalf("提交消费事务失败: %v", commitErr)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("作废失败: %v", r.err)
		}
		if !r.applied {
			t.Error("第一次作废应当 applied=true")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("消费提交之后作废仍未完成，可能是锁序错了")
	}

	// 作废应当核销消费之后剩下的 70，而不是它读到的 100：余额归零，不为负。
	if bal := ledgerBalance(t, db, prepaid); !bal.IsZero() {
		t.Errorf("prepaid 余额 = %s，期望 0", bal)
	}
	rows, err := svc.UserPlans(ctx, userID)
	if err != nil {
		t.Fatalf("读订阅失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != UserPlanExpired {
		t.Fatalf("作废后的订阅状态不对: %+v", rows)
	}
}

// 同一个作废单号重放：不重复记账；同一单号配不同的终态是冲突。
func TestVoidIsIdempotentByRef(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}

	voidRef := "void-" + itoa(nextID())
	txn := newTxnID()
	if _, _, voidErr := svc.Void(ctx, VoidInput{
		TxnID: txn, UserPlanID: up.ID, Ref: voidRef, Status: UserPlanExpired,
	}); voidErr != nil {
		t.Fatalf("首次作废失败: %v", voidErr)
	}
	before := countEntries(t, db, txn)

	again, applied, err := svc.Void(ctx, VoidInput{
		TxnID: txn, UserPlanID: up.ID, Ref: voidRef, Status: UserPlanExpired,
	})
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if applied {
		t.Error("重放应当 applied=false")
	}
	if again.VoidRef != voidRef {
		t.Errorf("重放返回的作废单号 = %q，期望 %q", again.VoidRef, voidRef)
	}
	if got := countEntries(t, db, txn); got != before {
		t.Errorf("重放后分录 %d 条，期望仍是 %d", got, before)
	}
	if bal := ledgerBalance(t, db, ledger.PrepaidAccount(up.ID)); !bal.IsZero() {
		t.Errorf("重放后 prepaid 余额 = %s，期望 0", bal)
	}

	// 同一个单号、不同的终态：不是重放。
	_, _, err = svc.Void(ctx, VoidInput{
		TxnID: txn, UserPlanID: up.ID, Ref: voidRef, Status: UserPlanCancelled,
	})
	if !errors.Is(err, store.ErrTxnConflict) {
		t.Errorf("同一作废单号配不同终态应当是冲突，得到: %v", err)
	}
}

// 已经作废的订阅换一个新单号再作废：状态不允许。
func TestVoidRejectsAlreadyVoided(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	if _, _, voidErr := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: up.ID, Ref: "void-" + itoa(nextID()),
		Status: UserPlanCancelled,
	}); voidErr != nil {
		t.Fatalf("首次作废失败: %v", voidErr)
	}

	_, _, err = svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: up.ID, Ref: "void-" + itoa(nextID()),
		Status: UserPlanCancelled,
	})
	if !errors.Is(err, ErrUserPlanState) {
		t.Errorf("重复作废应当是状态不允许（ErrUserPlanState），得到: %v", err)
	}
}

// 同一个作废单号不能用在两条订阅上：它撞的是 user_plans.void_ref 的唯一索引，
// 到了库里才报错就分不清是哪一次作废重复了，所以在写之前先查。
func TestVoidRejectsRefUsedByAnotherSubscription(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	grant := func() UserPlan {
		up, _, err := svc.Grant(ctx, GrantInput{
			TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
			PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("发放失败: %v", err)
		}
		return up
	}
	first, second := grant(), grant()

	ref := "void-" + itoa(nextID())
	if _, _, err := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: first.ID, Ref: ref, Status: UserPlanCancelled,
	}); err != nil {
		t.Fatalf("首次作废失败: %v", err)
	}
	if _, _, err := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: second.ID, Ref: ref, Status: UserPlanCancelled,
	}); !errors.Is(err, store.ErrTxnConflict) {
		t.Errorf("同一个作废单号用在第二条订阅上应当是冲突，得到: %v", err)
	}
	if bal := ledgerBalance(t, db, ledger.PrepaidAccount(second.ID)); bal.Cmp(amount(t, "100")) != 0 {
		t.Errorf("冲突之后第二条订阅的面值余额 = %s，期望仍是 100", bal)
	}
}

// 作废时刻必须落库：剩余面值为零时连分录都不写，不记这个时间就只剩状态一个终态值，
// 事后无从知道是什么时候作废的。
func TestVoidRecordsVoidedAt(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	if up.VoidedAt != nil {
		t.Error("未作废的订阅不该有作废时刻")
	}

	before := time.Now().Add(-time.Second)
	voided, _, err := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: up.ID, Ref: "void-" + itoa(nextID()),
		Status: UserPlanCancelled,
	})
	if err != nil {
		t.Fatalf("作废失败: %v", err)
	}
	if voided.VoidedAt == nil {
		t.Fatal("作废之后没有记下作废时刻")
	}
	if voided.VoidedAt.Before(before) || voided.VoidedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("作废时刻 %s 不在这次调用的时间窗口里", voided.VoidedAt)
	}

	// 读回路径也要带上它：对账读的是 UserPlans 那一侧。
	rows, err := svc.UserPlans(ctx, userID)
	if err != nil {
		t.Fatalf("读订阅失败: %v", err)
	}
	if len(rows) != 1 || rows[0].VoidedAt == nil {
		t.Fatalf("读回的订阅没有作废时刻: %+v", rows)
	}
}

// 面值已经用完时作废：只改状态，不写零金额分录。
func TestVoidWithZeroRemainingWritesNoEntries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	consumeTxn, err := ledger.Consume(ledger.ConsumeParams{
		TxnID: newTxnID(), UserID: userID, Amount: amount(t, "100"), Asset: ledger.AssetCNY,
		PrepaidAccount: ledger.PrepaidAccount(up.ID),
	})
	if err != nil {
		t.Fatalf("构造额度包消费失败: %v", err)
	}
	if _, postErr := store.New(db).PostTransaction(ctx, consumeTxn); postErr != nil {
		t.Fatalf("记额度包消费失败: %v", postErr)
	}

	voidTxn := newTxnID()
	voided, applied, err := svc.Void(ctx, VoidInput{
		TxnID: voidTxn, UserPlanID: up.ID, Ref: "void-" + itoa(nextID()),
		Status: UserPlanExpired,
	})
	if err != nil {
		t.Fatalf("面值用完的作废不该报错: %v", err)
	}
	if !applied {
		t.Error("这次作废改动了状态，应当 applied=true")
	}
	if voided.Status != UserPlanExpired {
		t.Errorf("作废后的状态 = %s，期望 %s", voided.Status, UserPlanExpired)
	}
	if voided.VoidedAt == nil {
		t.Error("零剩余面值的作废没有写分录，作废时刻更不能丢")
	}
	if got := countEntries(t, db, voidTxn); got != 0 {
		t.Errorf("零剩余面值的作废写了 %d 条分录，期望 0", got)
	}
	if bal := ledgerBalance(t, db, ledger.PrepaidAccount(up.ID)); !bal.IsZero() {
		t.Errorf("prepaid 余额 = %s，期望 0", bal)
	}
}

// UserPlans 把「还剩多少」读出来：面值按 prepaid 余额。作废之后仍读得到
// （不按状态过滤）。
func TestUserPlansReportsRemaining(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "100")

	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: "grant-" + itoa(nextID()), UserID: userID,
		PlanKey: plan.Key, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	if _, _, voidErr := svc.Void(ctx, VoidInput{
		TxnID: newTxnID(), UserPlanID: up.ID, Ref: "void-" + itoa(nextID()),
		Status: UserPlanCancelled,
	}); voidErr != nil {
		t.Fatalf("作废失败: %v", voidErr)
	}

	rows, err := svc.UserPlans(ctx, userID)
	if err != nil {
		t.Fatalf("读订阅失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应当读回 1 条订阅，得到 %d 条", len(rows))
	}
	got := rows[0]
	if got.ID != up.ID || got.PlanKey != plan.Key || got.Unit != UnitAmount {
		t.Errorf("读回的订阅不对: %+v", got)
	}
	if got.QuotaTotal.Cmp(amount(t, "100")) != 0 {
		t.Errorf("可用量 = %s，期望 100", got.QuotaTotal)
	}
	if !got.FaceValueRemaining.IsZero() {
		t.Errorf("作废后剩余面值 = %s，期望 0", got.FaceValueRemaining)
	}
	if got.Status != UserPlanCancelled || got.VoidRef == "" {
		t.Errorf("作废后的订阅应当带终态与作废单号: %+v", got)
	}
}

// 0 号用户不存在：UserPlans(0) 必须报错，而不是静默返回空清单——空清单会被
// 调用方当成「这个用户没有订阅」，一个参数错误就这样变成了业务结论。
func TestUserPlansRejectsZeroUserID(t *testing.T) {
	svc := New(testDB(t))
	rows, err := svc.UserPlans(context.Background(), 0)
	if err == nil {
		t.Fatalf("UserPlans(0) 返回 %d 条订阅与 nil 错误；空清单会被当成「没有订阅」", len(rows))
	}
	if !strings.Contains(err.Error(), "用户 id 非法") {
		t.Errorf("错误应当是「用户 id 非法」，得到: %v", err)
	}
}

// Grant 的 UserID 在入口就拒掉：0 号用户不存在，放它进事务只会在插入时被外键
// 拒，错误类别与文案都变了，而且多了一次写尝试。
func TestGrantRejectsZeroUserID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	plan := newPlan(t, ctx, svc, "100")

	ref := "grant-" + itoa(nextID())
	_, _, err := svc.Grant(ctx, GrantInput{
		TxnID: newTxnID(), Ref: ref, UserID: 0, PlanKey: plan.Key,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Fatal("UserID=0 的发放应当被拒")
	}
	if !strings.Contains(err.Error(), "用户 id 非法") {
		t.Errorf("错误应当是「用户 id 非法」，得到: %v", err)
	}
	if got := countUserPlansByRef(t, db, ref); got != 0 {
		t.Errorf("被拒的发放留下了 %d 条订阅，期望 0", got)
	}
}

// Void 的 UserPlanID 同理：0 不是合法订阅 id，要报「订阅 id 非法」，
// 而不是拿 0 去查库、把「参数写错了」报成「这条订阅不存在」。
func TestVoidRejectsZeroUserPlanID(t *testing.T) {
	svc := New(testDB(t))
	_, _, err := svc.Void(context.Background(), VoidInput{
		TxnID: newTxnID(), UserPlanID: 0, Ref: "void-" + itoa(nextID()),
		Status: UserPlanCancelled,
	})
	if err == nil {
		t.Fatal("UserPlanID=0 的作废应当被拒")
	}
	if !strings.Contains(err.Error(), "订阅 id 非法") {
		t.Errorf("错误应当是「订阅 id 非法」，得到: %v", err)
	}
	if errors.Is(err, ErrNoUserPlan) {
		t.Error("参数错误被报成了「这条订阅不存在」")
	}
}
