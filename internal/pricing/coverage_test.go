package pricing

import (
	"strings"
	"testing"
)

// 这一组是 coverage.go 的包内用例。
//
// 它必须留在本包：同一个函数的断言若只写在调用方（cmd/larkaxctl）的包里，
// 变异门按包收窄到 internal/pricing 时看不到任何覆盖，两个错误分支会被报成
// NOT COVERED——那不是「没有缺口」，是尺子伸不到那里。

func TestUncoveredTokenItemsFullCoverage(t *testing.T) {
	missing, err := UncoveredTokenItems(`{"prompt": 0.0000015, "completion": 0.0000075, "cache_read": 0.00000015, "cache_write": 0.000001875, "reasoning": 0.0000075}`)
	if err != nil {
		t.Fatalf("五项齐全的规则不该报错：%v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("五项齐全时缺项应为空，得到 %v", missing)
	}
}

func TestUncoveredTokenItemsNamesMissingInSlotOrder(t *testing.T) {
	missing, err := UncoveredTokenItems(`{"prompt": 0.0000015, "completion": 0.0000075}`)
	if err != nil {
		t.Fatalf("两条单价的常量表不该报错：%v", err)
	}
	want := []string{"cache_read", "cache_write", "reasoning"}
	if len(missing) != len(want) {
		t.Fatalf("缺项应为 %v，得到 %v", want, missing)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("缺项顺序应照 billing.TokenSlots（%v），得到 %v", want, missing)
		}
	}
}

func TestUncoveredTokenItemsRejectsInvalidExpression(t *testing.T) {
	missing, err := UncoveredTokenItems("not-an-expression")
	if err == nil {
		t.Fatalf("编译不过的规则必须报错，得到缺项 %v", missing)
	}
	if missing != nil {
		t.Fatalf("报错时不该给出缺项，得到 %v", missing)
	}
}

func TestUncoveredScheduleTokenItemsRejectsInvalidSchedule(t *testing.T) {
	cases := map[string]*Schedule{
		"base 为空": {TZ: "Asia/Shanghai"},
		"键名不认":    {TZ: "Asia/Shanghai", Base: map[string]float64{"bogus": 1}},
	}
	for name, schedule := range cases {
		t.Run(name, func(t *testing.T) {
			missing, err := UncoveredScheduleTokenItems(schedule)
			if err == nil {
				t.Fatalf("无效的定价表必须报错，得到缺项 %v", missing)
			}
			// 断言错误来自表本身，而不是「表编译出来的源码又编译失败」那条兜底：
			// 认错了错误来源，两条路径就分不开了。
			if !strings.Contains(err.Error(), "定价表的 base") && !strings.Contains(err.Error(), "不是计价项") {
				t.Fatalf("错误应点出表本身的问题，得到 %q", err.Error())
			}
		})
	}
}

func TestUncoveredScheduleTokenItemsNamesMissing(t *testing.T) {
	schedule := &Schedule{
		TZ:   "Asia/Shanghai",
		Base: map[string]float64{"prompt": 0.0000015, "completion": 0.0000075},
	}
	missing, err := UncoveredScheduleTokenItems(schedule)
	if err != nil {
		t.Fatalf("合法的定价表不该报错：%v", err)
	}
	want := []string{"cache_read", "cache_write", "reasoning"}
	if len(missing) != len(want) {
		t.Fatalf("缺项应为 %v，得到 %v", want, missing)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("缺项应为 %v，得到 %v", want, missing)
		}
	}
}
