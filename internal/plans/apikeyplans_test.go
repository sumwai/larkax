package plans

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedAPIKey 建一把 key，返回 id。归属与哈希都只求形态合法：这一层不碰鉴权。
func seedAPIKey(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	n := nextID()
	var id int64
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO api_keys (user_id, key_hash, key_prefix) VALUES ($1, $2, $3) RETURNING id`,
		userID, "hash-"+itoa(n), "larkax_"+itoa(n)).Scan(&id); err != nil {
		t.Fatalf("建 API Key 失败: %v", err)
	}
	return id
}

// grantPlan 给一个用户发一份订阅，返回订阅行。
func grantPlan(t *testing.T, ctx context.Context, svc *Service, userID int64, plan Plan) UserPlan {
	t.Helper()
	up, _, err := svc.Grant(ctx, GrantInput{
		TxnID:     newTxnID(),
		Ref:       "grant-" + itoa(nextID()),
		UserID:    userID,
		PlanKey:   plan.Key,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("发放失败: %v", err)
	}
	return up
}

// planIDs 取一组订阅的 id，排序后返回，便于整体比对。
func planIDs(plans []UserPlan) []int64 {
	ids := make([]int64, 0, len(plans))
	for _, p := range plans {
		ids = append(ids, p.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 没有关联行 = 全部：这座用户自己的订阅全在，别人用户的订阅一个都不在。
func TestAPIKeyPlansEmptyMeansAll(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userA := seedUser(t, db)
	userB := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")

	first := grantPlan(t, ctx, svc, userA, plan)
	second := grantPlan(t, ctx, svc, userA, plan)
	foreign := grantPlan(t, ctx, svc, userB, plan)
	keyA := seedAPIKey(t, db, userA)

	got, err := svc.APIKeyPlans(ctx, keyA)
	if err != nil {
		t.Fatalf("读 key 的订阅失败: %v", err)
	}
	if ids := planIDs(got); !equalIDs(ids, []int64{first.ID, second.ID}) {
		t.Errorf("没有关联行时应当返回用户全部订阅 %v，得到 %v", []int64{first.ID, second.ID}, ids)
	}
	for _, up := range got {
		if up.ID == foreign.ID {
			t.Errorf("别的用户的订阅 %d 出现在了清单里", foreign.ID)
		}
	}

	// 「没有行」与「有行但为空」是同一件事：名单原文也读出空切片。
	raw, err := svc.APIKeyPlanIDs(ctx, keyA)
	if err != nil {
		t.Fatalf("读名单原文失败: %v", err)
	}
	if len(raw) != 0 {
		t.Errorf("没有配过名单时原文应当是空的，得到 %v", raw)
	}
}

// 指定集合时只列出指定的那些。
func TestSetAPIKeyPlansRestrictsToNamed(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")

	first := grantPlan(t, ctx, svc, userID, plan)
	second := grantPlan(t, ctx, svc, userID, plan)
	key := seedAPIKey(t, db, userID)

	got, err := svc.SetAPIKeyPlans(ctx, key, []int64{second.ID})
	if err != nil {
		t.Fatalf("设名单失败: %v", err)
	}
	if ids := planIDs(got); !equalIDs(ids, []int64{second.ID}) {
		t.Errorf("设完名单后返回 %v，期望只有 %d", ids, second.ID)
	}

	got, err = svc.APIKeyPlans(ctx, key)
	if err != nil {
		t.Fatalf("读 key 的订阅失败: %v", err)
	}
	if ids := planIDs(got); !equalIDs(ids, []int64{second.ID}) {
		t.Errorf("读回的允许集 = %v，期望只有 %d（%d 不在名单里）", ids, second.ID, first.ID)
	}

	raw, err := svc.APIKeyPlanIDs(ctx, key)
	if err != nil {
		t.Fatalf("读名单原文失败: %v", err)
	}
	if !equalIDs(raw, []int64{second.ID}) {
		t.Errorf("名单原文 = %v，期望 [%d]", raw, second.ID)
	}
}

// 再设一次是整体替换，不是增量；空名单回到「全部」。
func TestSetAPIKeyPlansReplacesAndClears(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")

	first := grantPlan(t, ctx, svc, userID, plan)
	second := grantPlan(t, ctx, svc, userID, plan)
	key := seedAPIKey(t, db, userID)

	if _, err := svc.SetAPIKeyPlans(ctx, key, []int64{first.ID}); err != nil {
		t.Fatalf("第一次设名单失败: %v", err)
	}
	got, err := svc.SetAPIKeyPlans(ctx, key, []int64{second.ID})
	if err != nil {
		t.Fatalf("第二次设名单失败: %v", err)
	}
	if ids := planIDs(got); !equalIDs(ids, []int64{second.ID}) {
		t.Errorf("第二次设完之后 = %v，期望只有 %d：这是替换，不是合并", ids, second.ID)
	}

	// 空名单 = 全部：删掉关联行，读侧展开成用户名下全部订阅。
	got, err = svc.SetAPIKeyPlans(ctx, key, nil)
	if err != nil {
		t.Fatalf("清空名单失败: %v", err)
	}
	if ids := planIDs(got); !equalIDs(ids, []int64{first.ID, second.ID}) {
		t.Errorf("清空名单后应当回到全部订阅 [%d %d]，得到 %v", first.ID, second.ID, ids)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM api_key_plans WHERE api_key_id = $1`, key).Scan(&n); err != nil {
		t.Fatalf("数关联行失败: %v", err)
	}
	if n != 0 {
		t.Errorf("清空名单后还剩 %d 行关联行，期望 0", n)
	}
}

// 名单里引用别的用户的订阅时当场报错，且一行都不写。
func TestSetAPIKeyPlansRejectsForeignSubscription(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userA := seedUser(t, db)
	userB := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")

	mine := grantPlan(t, ctx, svc, userA, plan)
	foreign := grantPlan(t, ctx, svc, userB, plan)
	key := seedAPIKey(t, db, userA)

	_, err := svc.SetAPIKeyPlans(ctx, key, []int64{mine.ID, foreign.ID})
	if !errors.Is(err, ErrUserPlanNotOwned) {
		t.Fatalf("引用别的用户的订阅应当返回 ErrUserPlanNotOwned，得到 %v", err)
	}
	if got := planIDs(mustAPIKeyPlans(t, ctx, svc, key)); !equalIDs(got, []int64{mine.ID}) {
		t.Errorf("报错之后允许集变了：%v，期望仍是全部（只有 %d）", got, mine.ID)
	}
}

// 名单里引用一条不存在的订阅也当场报错。
func TestSetAPIKeyPlansRejectsMissingSubscription(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")
	mine := grantPlan(t, ctx, svc, userID, plan)
	key := seedAPIKey(t, db, userID)

	_, err := svc.SetAPIKeyPlans(ctx, key, []int64{mine.ID, 987654321})
	if !errors.Is(err, ErrUserPlanNotOwned) {
		t.Fatalf("引用不存在的订阅应当返回 ErrUserPlanNotOwned，得到 %v", err)
	}
	if got := planIDs(mustAPIKeyPlans(t, ctx, svc, key)); !equalIDs(got, []int64{mine.ID}) {
		t.Errorf("报错之后允许集变了：%v", got)
	}
}

// 重复的 id 去重，不撞主键。
func TestSetAPIKeyPlansDedupes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")
	up := grantPlan(t, ctx, svc, userID, plan)
	key := seedAPIKey(t, db, userID)

	got, err := svc.SetAPIKeyPlans(ctx, key, []int64{up.ID, up.ID, up.ID})
	if err != nil {
		t.Fatalf("名单里重复的 id 应当去重而不是撞主键: %v", err)
	}
	if ids := planIDs(got); !equalIDs(ids, []int64{up.ID}) {
		t.Errorf("去重后 = %v，期望 [%d]", ids, up.ID)
	}

	if _, err := svc.SetAPIKeyPlans(ctx, key, []int64{up.ID, 0}); err == nil {
		t.Error("名单里有非法 id 应当报错")
	}
}

// 删掉 key 之后关联行跟着没（级联），而且不会留下读侧读得到的悬空行。
func TestAPIKeyPlansCascadeOnKeyDelete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")
	up := grantPlan(t, ctx, svc, userID, plan)
	key := seedAPIKey(t, db, userID)

	if _, err := svc.SetAPIKeyPlans(ctx, key, []int64{up.ID}); err != nil {
		t.Fatalf("设名单失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = $1`, key); err != nil {
		t.Fatalf("删 key 失败: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM api_key_plans WHERE api_key_id = $1`, key).Scan(&n); err != nil {
		t.Fatalf("数关联行失败: %v", err)
	}
	if n != 0 {
		t.Errorf("删 key 之后还剩 %d 行关联行，期望 0", n)
	}
	if _, err := svc.APIKeyPlans(ctx, key); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("删掉 key 之后读它应当返回 ErrNoAPIKey，得到 %v", err)
	}
}

// 删掉订阅之后关联行跟着没，读侧只返回还在的那些。
func TestAPIKeyPlansCascadeOnSubscriptionDelete(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")

	first := grantPlan(t, ctx, svc, userID, plan)
	second := grantPlan(t, ctx, svc, userID, plan)
	key := seedAPIKey(t, db, userID)

	if _, err := svc.SetAPIKeyPlans(ctx, key, []int64{first.ID, second.ID}); err != nil {
		t.Fatalf("设名单失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM user_plans WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("删订阅失败: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM api_key_plans WHERE user_plan_id = $1`, first.ID).Scan(&n); err != nil {
		t.Fatalf("数关联行失败: %v", err)
	}
	if n != 0 {
		t.Errorf("删订阅之后还剩 %d 行指向它的关联行，期望 0", n)
	}
	if ids := planIDs(mustAPIKeyPlans(t, ctx, svc, key)); !equalIDs(ids, []int64{second.ID}) {
		t.Errorf("删掉一条订阅后允许集 = %v，期望只剩 %d", ids, second.ID)
	}
}

// 不存在的 key、非法 id 都在读侧报错。
func TestAPIKeyPlansRejectsUnknownOrZeroKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)

	if _, err := svc.APIKeyPlans(ctx, 987654321); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("不存在的 key 应当返回 ErrNoAPIKey，得到 %v", err)
	}
	if _, err := svc.APIKeyPlanIDs(ctx, 987654321); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("不存在的 key 读名单原文应当返回 ErrNoAPIKey，得到 %v", err)
	}
	if _, err := svc.APIKeyPlans(ctx, 0); err == nil {
		t.Error("key id 为 0 应当报错")
	}
	if _, err := svc.SetAPIKeyPlans(ctx, 0, nil); err == nil {
		t.Error("设名单时 key id 为 0 应当报错")
	}
	if _, err := svc.SetAPIKeyPlans(ctx, 987654321, nil); !errors.Is(err, ErrNoAPIKey) {
		t.Error("给不存在的 key 设名单应当返回 ErrNoAPIKey")
	}
}

func mustAPIKeyPlans(t *testing.T, ctx context.Context, svc *Service, key int64) []UserPlan {
	t.Helper()
	got, err := svc.APIKeyPlans(ctx, key)
	if err != nil {
		t.Fatalf("读 key 的订阅失败: %v", err)
	}
	return got
}

// 回读发生在提交之前：读不回来时，这一轮一行都没写。
//
// 故障靠把那两处读**用到的表**改名造出来：写入前的归属核对只读 user_plans，
// 照常通过；回读要 JOIN plans，于是失败。
//
// 这条用例钉的是两条实现的分界：先提交再回读的写法在失败时库里已经是新名单
// （调用方以为没写），先回读再提交的写法会回滚，库里的名单还是原来那一份。
func TestSetAPIKeyPlansReadFailureLeavesNothingWritten(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := New(db)
	userID := seedUser(t, db)
	plan := newPlan(t, ctx, svc, "10")

	first := grantPlan(t, ctx, svc, userID, plan)
	second := grantPlan(t, ctx, svc, userID, plan)
	key := seedAPIKey(t, db, userID)

	// 先成功设一次，作为「原来那一份」。
	if _, err := svc.SetAPIKeyPlans(ctx, key, []int64{first.ID}); err != nil {
		t.Fatalf("第一次设名单失败: %v", err)
	}

	renameTable(t, db, "plans", "plans_903_readback_fault")

	// 换一份名单：删除与写入都会执行，回读在提交之前失败。
	_, err := svc.SetAPIKeyPlans(ctx, key, []int64{second.ID})
	if err == nil {
		t.Fatal("回读失败时应当报错")
	}
	if !strings.Contains(err.Error(), "读 API Key") {
		t.Errorf("报错应当点出是读回失败，得到 %v", err)
	}

	raw, readErr := svc.apiKeyPlanIDs(ctx, db, key)
	if readErr != nil {
		t.Fatalf("读名单原文失败: %v", readErr)
	}
	if !equalIDs(raw, []int64{first.ID}) {
		t.Errorf("回读失败的那一轮不该改动名单：期望还是 %v，得到 %v", []int64{first.ID}, raw)
	}
}

// renameTable 把一张表改名，用例结束时改回去。
//
// 用来把某一步读打坏：改名之后指向它的查询会报「表不存在」，而别的查询照常。
func renameTable(t *testing.T, db *sql.DB, from, to string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`ALTER TABLE `+from+` RENAME TO `+to); err != nil {
		t.Fatalf("把表 %s 改名为 %s 失败: %v", from, to, err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`ALTER TABLE `+to+` RENAME TO `+from); err != nil {
			t.Errorf("把表 %s 改回 %s 失败: %v", to, from, err)
		}
	})
}
