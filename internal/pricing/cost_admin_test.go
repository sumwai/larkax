package pricing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 这一组用例钉住的是「入参判断本身」：它们不改任何数据，只决定一次调用该不该
// 往下走。判据错了不会让哪一行数据变坏，只会让调用方拿到另一种结果——所以
// 变异测试里它们是最容易「改了也没人发现」的那一类。

// TestCostUpdateRejectsZeroID 钉住 id = 0 是入参错，而不是「找不到那一行」。
//
// 库上的主键从 1 起，0 一定查不到；但两种错的文案与退出码不同，调用方据此
// 决定是改参数还是换 id。把 `<=` 写成 `<` 时 0 会进到事务里，结果从「id 无效」
// 变成「成本规则不存在」。
func TestCostUpdateRejectsZeroID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	priority := 1
	err := admin.Update(ctx, CostUpdateRequest{ID: 0, Priority: &priority})
	if err == nil {
		t.Fatal("id = 0 的修改应当被拒")
	}
	if errors.Is(err, ErrCostNotFound) {
		t.Fatalf("id = 0 被当成「找不到那一行」，得到 %v：这是入参错，不是 id 不存在", err)
	}
	if !strings.Contains(err.Error(), "id 无效") {
		t.Errorf("错误信息 %q 没有点出 id 无效", err.Error())
	}
}

// TestCostDisableRejectsZeroID 与 Update 那条同一个判据：0 是入参错。
func TestCostDisableRejectsZeroID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	err := admin.Disable(ctx, 0)
	if err == nil {
		t.Fatal("id = 0 的停用应当被拒")
	}
	if errors.Is(err, ErrCostNotFound) {
		t.Fatalf("id = 0 被当成「找不到那一行」，得到 %v：这是入参错，不是 id 不存在", err)
	}
	if !strings.Contains(err.Error(), "id 无效") {
		t.Errorf("错误信息 %q 没有点出 id 无效", err.Error())
	}
}

// TestCostUpdateOneFieldIsAChange 钉住「只给一个字段也算有改动」。
//
// CostUpdateRequest 的每个字段都是指针 / bool，changesAnything 把它们串成一条
// 或链。链上任意一个 `||` 被写成 `&&`，对应那个字段单独出现时就会被判成
// 「没有要改的字段」——而那正是最常见的调用形状（只调优先级、只改模型清单）。
// 所以七个可改项各来一条，每个请求只提这一项。
func TestCostUpdateOneFieldIsAChange(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	// 渠道要换个真存在的：ChannelID 那一条会把它写进外键那一栏。
	otherChannel := seedCostChannel(t, db)
	baseChannel := seedCostChannel(t, db)
	newModel := unique("cost-model")

	cases := []struct {
		name string
		set  func(*CostUpdateRequest, int64)
	}{
		{"ChannelID", func(req *CostUpdateRequest, ch int64) { req.ChannelID = &ch }},
		{"ModelPatterns", func(req *CostUpdateRequest, _ int64) {
			patterns := []string{newModel}
			req.ModelPatterns = &patterns
		}},
		{"Expr", func(req *CostUpdateRequest, _ int64) {
			expr := `{"completion": 0.000002}`
			req.Expr = &expr
		}},
		{"Priority", func(req *CostUpdateRequest, _ int64) {
			priority := 9
			req.Priority = &priority
		}},
		{"EffectiveFrom", func(req *CostUpdateRequest, _ int64) {
			from := time.Now().Add(-2 * time.Hour)
			req.EffectiveFrom = &from
		}},
		{"EffectiveTo", func(req *CostUpdateRequest, _ int64) {
			to := time.Now().Add(24 * time.Hour)
			req.EffectiveTo = &to
		}},
		{"ClearEffectiveTo", func(req *CostUpdateRequest, _ int64) {
			req.ClearEffectiveTo = true
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := insertCost(t, ctx, db, costSpec{channelID: baseChannel, patterns: []string{newModel}})

			req := CostUpdateRequest{ID: id}
			tc.set(&req, otherChannel)

			err := admin.Update(ctx, req)
			if err == nil {
				return
			}
			if errors.Is(err, ErrCostNoChanges) {
				t.Fatalf("只给 %s 时被判成「没有要改的字段」", tc.name)
			}
			t.Fatalf("只给 %s 的修改失败: %v", tc.name, err)
		})
	}
}

// costExprOf 读回某条规则存在库里的表达式原文。
func costExprOf(t *testing.T, ctx context.Context, admin *CostAdmin, channelID, id int64) string {
	t.Helper()
	rules, err := admin.List(ctx, CostListQuery{ChannelID: channelID})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	for _, r := range rules {
		if r.ID == id {
			return r.Expr
		}
	}
	t.Fatalf("库里找不到规则 %d", id)
	return ""
}

// TestCostUpdateWritesCompiledExpr 钉住「编译通过之后真的写进库」。
//
// 编译这一步夹在校验与 UPDATE 之间：把它写成「编译通过就返回」，调用方拿到
// 一次成功而那一栏根本没变；编译失败时返回更是把坏表达式放进了库。所以这里
// 不看「报没报错」，而是把库里的值读回来对。
func TestCostUpdateWritesCompiledExpr(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	ch := seedCostChannel(t, db)
	id := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{unique("cost-model")}})

	newExpr := `{"completion": 0.000002}`
	if err := admin.Update(ctx, CostUpdateRequest{ID: id, Expr: &newExpr}); err != nil {
		t.Fatalf("改成一个能编译的表达式: %v", err)
	}
	if got := costExprOf(t, ctx, admin, ch, id); got != newExpr {
		t.Errorf("库里的表达式 = %q，期望 %q", got, newExpr)
	}
}

// TestCostUpdateRejectsUncompilableExprWithoutWriting 钉住「编译不过就整次拒绝，
// 一个字都不落库」。
func TestCostUpdateRejectsUncompilableExprWithoutWriting(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	ch := seedCostChannel(t, db)
	id := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{unique("cost-model")}})
	before := costExprOf(t, ctx, admin, ch, id)

	broken := `{"prompt":`
	err := admin.Update(ctx, CostUpdateRequest{ID: id, Expr: &broken})
	if err == nil {
		t.Fatal("编译不过的表达式应当被拒")
	}
	if got := costExprOf(t, ctx, admin, ch, id); got != before {
		t.Errorf("被拒之后库里的表达式 = %q，期望还是 %q", got, before)
	}
}
