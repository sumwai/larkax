package keyquota

import (
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// TestOver 钉住判据的边界。
//
// 断言用字面量，不用被测常量拼出来：拿 over 自己算出来的东西当期望值，
// 把 `>` 改成 `>=` 时两边一起变，用例照样通过（见 docs/development.md 的「用常量
// 测常量等于没测」）。
//
// 边界只在这一处：`>` 与 `>=` 的差别恰好是「把最后一分钱用满」那一格。
// 它是额度唯一会被追问的地方——用户会拿着账单问「我正好花到上限，为什么
// 最后一笔被拒了」。
func TestOver(t *testing.T) {
	cases := []struct {
		name       string
		spent      string
		additional string
		limit      string
		want       bool
	}{
		// 上限 100，一笔 10：合计 100 恰好用满，放行。
		{"恰好用满放行", "90", "10", "100", false},
		// 再多一个最小单位就越界。这一格是 `>` 与 `>=` 的唯一分界。
		{"越一个最小单位就拒", "90", "10.00000001", "100", true},
		{"已花超过上限、这一笔为零也拒", "100.00000001", "0", "100", true},
		{"已花恰好等于上限、这一笔为零放行", "100", "0", "100", false},
		{"一笔就把上限填满放行", "0", "100", "100", false},
		{"一笔就超过上限拒", "0", "100.00000001", "100", true},
		// 上限 0 是「一分钱都不放行」，不是「不限」（NULL 才是不限，那一支
		// 在 CheckInTx 里根本走不到这里）。
		{"上限为零时零元一笔放行", "0", "0", "0", false},
		{"上限为零时任何一笔都拒", "0", "0.00000001", "0", true},
		{"没有消费、没有上限约束时放行", "0", "0", "0", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := over(
				money.MustParse(tc.spent),
				money.MustParse(tc.additional),
				money.MustParse(tc.limit),
			)
			if got != tc.want {
				t.Errorf("over(已花 %s, 这一笔 %s, 上限 %s) = %v，期望 %v",
					tc.spent, tc.additional, tc.limit, got, tc.want)
			}
		})
	}
}
