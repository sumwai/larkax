package plans

import (
	"context"
	"strings"
	"testing"
)

// 每个对外的 id 守卫都必须在**碰库之前**拒掉 0。
//
// 这一组用例给 Service 一个 nil 句柄：守卫一旦被绕过，方法会在 s.db 上解引用并
// panic——把这一点接住，报出来的是「0 号 id 没被拦住」，而不是一串栈。这同时就是
// 「不碰库」的判据：真去查一次库，nil 句柄先崩。
//
// 断言到具体那句文案，而不是「错误非 nil」。只判非 nil 挡不住这一类变异：守卫
// 改成 `<` 之后，0 号 id 会走到查询，再以「这一行不存在」的形式报错——错误类别与
// 排查方向都变了，而用例看起来照样绿（见 #912）。
func TestZeroIDsRejectedBeforeTouchingDB(t *testing.T) {
	ctx := context.Background()
	svc := New(nil)

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"APIKeyPlans(0)", func() error {
			_, err := svc.APIKeyPlans(ctx, 0)
			return err
		}, "API Key id 非法: 0"},
		{"SetAPIKeyPlans(0)", func() error {
			_, err := svc.SetAPIKeyPlans(ctx, 0, nil)
			return err
		}, "API Key id 非法: 0"},
		{"APIKeyPlanIDs(0)", func() error {
			_, err := svc.APIKeyPlanIDs(ctx, 0)
			return err
		}, "API Key id 非法: 0"},
		{"EffectiveChargeOrder(0)", func() error {
			_, _, err := svc.EffectiveChargeOrder(ctx, 0)
			return err
		}, "用户 id 非法: 0"},
		{"SetUserChargeOrder(0)", func() error {
			return svc.SetUserChargeOrder(ctx, 0, ChargeOrderPlanFirst)
		}, "用户 id 非法: 0"},
		{"ClearUserChargeOrder(0)", func() error {
			return svc.ClearUserChargeOrder(ctx, 0)
		}, "用户 id 非法: 0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s 在守卫之后碰到了库（panic: %v）：0 号 id 必须在查库之前拒掉", tc.name, r)
				}
			}()

			err := tc.call()
			if err == nil {
				t.Fatalf("%s 没报错", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s 的报错 = %v，期望含 %q（不是「查不到这一行」）", tc.name, err, tc.want)
			}
		})
	}
}

// normalizeUserPlanIDs 是 SetAPIKeyPlans 的入参规范化，纯函数：0 与负数都在这里拒。
//
// 它是名单里那一条守卫，与上面几个入口的 id 守卫是同一类缺口（#912）。
func TestNormalizeUserPlanIDsRejectsNonPositive(t *testing.T) {
	for _, id := range []int64{0, -1} {
		ids, err := normalizeUserPlanIDs([]int64{id})
		if err == nil {
			t.Errorf("normalizeUserPlanIDs([%d]) 返回 %v 与 nil 错误", id, ids)
			continue
		}
		if !strings.Contains(err.Error(), "额度包订阅 id 非法") {
			t.Errorf("normalizeUserPlanIDs([%d]) 的报错 = %v，期望点出「额度包订阅 id 非法」", id, err)
		}
	}

	// 合法 id 照旧通过，且去重、升序：这条用例不把规范化本身的行为钉丢了。
	got, err := normalizeUserPlanIDs([]int64{3, 1, 3})
	if err != nil {
		t.Fatalf("normalizeUserPlanIDs([3 1 3]) 失败: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("normalizeUserPlanIDs([3 1 3]) = %v，期望 [1 3]", got)
	}
}

// 全包变异复核（#912）之后还剩两条 `CONDITIONALS_BOUNDARY` 存活，都不是 id 守卫，
// 也都杀不掉——理由写在这里，免得下次复核时又被当成缺口：
//
//   - `apikeyplans.go:122` 的 `len(ids) > 0` 改成 `>= 0`：`len` 不会是负数，改完
//     只是让空名单也走一次 `WHERE id = ANY($1)`。空数组查不出行，`owned` 与
//     `missing` 的取值不变，后面每一步的判断与原来完全一样。要「杀」它只能断言
//     查询次数，那是把实现钉死在调用形状上。
//   - `apikeyplans.go:253` 的 `out[i] < out[j]` 改成 `out[i] <= out[j]`：进排序
//     之前已经去重，比较的两项不会相等，`<` 与 `<=` 在每一对上结果相同。它的
//     `NEGATION` 那一份（改成 `>=`）会把顺序反过来，已经被上面那条升序断言杀掉。
