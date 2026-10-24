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

// TestUncoveredTokenItemsIgnoresUnreferencedLetTable 检查 #1153：提示读的是
// 结果位置能到达的键，不是「遍历到的每张表」。
//
// m 里没被结果引用的 completion 没有单价，结算真的会失败，所以提示必须把
// 它算成缺项——#1153 之前它被算作已覆盖，于是提示说「齐全」，事故只在数据面出现。
func TestUncoveredTokenItemsIgnoresUnreferencedLetTable(t *testing.T) {
	missing, err := UncoveredTokenItems(
		`let m = {"prompt": 0.0000015, "completion": 0.0000075}; {"prompt": m["prompt"]}`)
	if err != nil {
		t.Fatalf("规则应当能编译：%v", err)
	}
	want := []string{"completion", "cache_read", "cache_write", "reasoning"}
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Fatalf("缺项 = %v，期望 %v（prompt 被引用了，completion 只躺在绑定里）", missing, want)
	}
}

// TestUncoveredTokenItemsFollowsReferencedBinding 是上一条的另一半。
//
// 绑定的表被结果**整张引用**时，里面的键照样算覆盖——收窄的是「没人引用的
// 表」，不是「所有 let 里的表」。只检查收窄那一半的话，一个「见 let 就跳过」
// 的实现也能通过。
func TestUncoveredTokenItemsFollowsReferencedBinding(t *testing.T) {
	missing, err := UncoveredTokenItems(
		`let m = {"prompt": 0.0000015, "completion": 0.0000075}; m`)
	if err != nil {
		t.Fatalf("规则应当能编译：%v", err)
	}
	want := []string{"cache_read", "cache_write", "reasoning"}
	if strings.Join(missing, ",") != strings.Join(want, ",") {
		t.Fatalf("缺项 = %v，期望 %v（绑定的表就是结果，两项都写了）", missing, want)
	}
}
