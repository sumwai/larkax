package billing

import (
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// 确认量为 0 时，预留结果与拆分之前**逐位相同**。
//
// 用例把拆分之前那几条口径的金额写成字面量：基本形态、额外缓存估计量、
// 一次性费用、非 token 计量。改坏了拆分里的默认分支（例如把 hit 的默认值
// 兜成非零、或者 miss 少减一次），这里就会红。
func TestReserveDefaultPathUnchanged(t *testing.T) {
	// 非 token 计量的单价挂在 Prices.Units 上，单独备一份价目。
	withUnits := prices()
	withUnits.Units = map[string]money.Amount{
		"searches": money.MustParse("0.01"),
		"images":   money.MustParse("0.02"),
	}

	cases := []struct {
		name string
		req  ReserveRequest
		want string
	}{
		{
			name: "输入输出两侧都按最贵单价",
			req:  ReserveRequest{Prices: prices(), InputTokens: 1000, OutputTokens: 4000},
			want: "0.04250000",
		},
		{
			name: "在整段输入之外再加缓存读写的估计量",
			req: ReserveRequest{
				Prices: prices(), InputTokens: 1000,
				CacheReadTokens: 500, CacheWriteTokens: 200, OutputTokens: 4000,
			},
			want: "0.04400000",
		},
		{
			name: "最低消费与固定费",
			req: ReserveRequest{
				Prices: prices(), InputTokens: 1000, OutputTokens: 100,
				Charges: Charges{Minimum: money.MustParse("0.5"), Fixed: money.MustParse("0.2")},
			},
			want: "0.70000000",
		},
		{
			name: "非 token 计量各留一份",
			req: ReserveRequest{
				Prices: withUnits, InputTokens: 1000, OutputTokens: 1,
			},
			want: "0.03251000",
		},
	}

	for _, tc := range cases {
		if tc.req.ConfirmedCacheTokens != 0 {
			t.Fatalf("%s：这条用例的确认量必须是 0", tc.name)
		}
		got, err := Reserve(tc.req)
		if err != nil {
			t.Errorf("%s：出错: %v", tc.name, err)
			continue
		}
		if want := money.MustParse(tc.want); got.Cmp(want) != 0 {
			t.Errorf("%s：预留 = %s，期望 %s（确认量为 0 时口径不该变）", tc.name, got, want)
		}
	}
}

// 确认命中的那一段按 cache_read 估，其余仍按最贵单价。
func TestReserveSplitsConfirmedCacheHit(t *testing.T) {
	// prices()：输入 2e-6、缓存读 2e-7、缓存写 2.5e-6，所以最贵是 2.5e-6。
	// 输入 1500 里确认命中 1200：300 × 2.5e-6 + 1200 × 2e-7 = 0.00075 + 0.00024。
	// 输出 1000 × max(8e-6, 1e-5) = 0.01。
	got, err := Reserve(ReserveRequest{
		Prices: prices(), InputTokens: 1500, ConfirmedCacheTokens: 1200, OutputTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := money.MustParse("0.01099000")
	if got.Cmp(want) != 0 {
		t.Errorf("预留 = %s，期望 %s", got, want)
	}

	unchanged, err := Reserve(ReserveRequest{
		Prices: prices(), InputTokens: 1500, OutputTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(unchanged) >= 0 {
		t.Errorf("确认命中后的预留 %s 没有低于未确认的 %s——拆出去的那段没按缓存价算", got, unchanged)
	}
}

// 确认量钳进 [0, InputTokens]：超出输入总量按输入总量算，负数按 0 算。
func TestReserveClampsConfirmedCache(t *testing.T) {
	all, err := Reserve(ReserveRequest{
		Prices: prices(), InputTokens: 1500, ConfirmedCacheTokens: 1500, OutputTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 整段命中：1500 × 2e-7 + 0.01 = 0.0003 + 0.01。
	if want := money.MustParse("0.01030000"); all.Cmp(want) != 0 {
		t.Errorf("整段命中时预留 = %s，期望 %s", all, want)
	}

	over, err := Reserve(ReserveRequest{
		Prices: prices(), InputTokens: 1500, ConfirmedCacheTokens: 5000, OutputTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if over.Cmp(all) != 0 {
		t.Errorf("确认量超过输入总量时预留 = %s，期望与钳到输入总量相同 %s", over, all)
	}

	negative, err := Reserve(ReserveRequest{
		Prices: prices(), InputTokens: 1500, ConfirmedCacheTokens: -100, OutputTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if negative.Cmp(money.MustParse("0.01375000")) != 0 {
		t.Errorf("确认量为负时预留 = %s，期望按 0 算（0.01375）", negative)
	}
}

// 规模型号的分账示例：上次整段 28,693 命中、本次新增 1,200、输出冻 4096。
//
// 单价是「输入 1e-6、缓存读 2e-8、输出 4e-6」那一档。两个数都是这条改动
// 要带来的效果：旧口径 0.046277，确认命中之后 0.01815786。
func TestReserveConfirmedCacheExample(t *testing.T) {
	p := Prices{
		Prompt:     money.MustParse("0.000001"),
		Completion: money.MustParse("0.000004"),
		CacheRead:  money.MustParse("0.00000002"),
	}

	old, err := Reserve(ReserveRequest{Prices: p, InputTokens: 29893, OutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if want := money.MustParse("0.046277"); old.Cmp(want) != 0 {
		t.Errorf("旧口径预留 = %s，期望 %s", old, want)
	}

	got, err := Reserve(ReserveRequest{
		Prices: p, InputTokens: 29893, ConfirmedCacheTokens: 28693, OutputTokens: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := money.MustParse("0.01815786"); got.Cmp(want) != 0 {
		t.Errorf("确认命中后预留 = %s，期望 %s", got, want)
	}
}

// 确认量越大，预留越小或相等——它只会把上界往下压，任何一格都不该反过来。
func TestReserveConfirmedCacheNeverIncreasesBound(t *testing.T) {
	prev := money.Amount{}
	for hit := int64(0); hit <= 1500; hit += 100 {
		got, err := Reserve(ReserveRequest{
			Prices: prices(), InputTokens: 1500, ConfirmedCacheTokens: hit, OutputTokens: 1000,
		})
		if err != nil {
			t.Fatalf("hit = %d：%v", hit, err)
		}
		if hit > 0 && got.Cmp(prev) > 0 {
			t.Errorf("hit = %d 时预留 %s 大于 hit = %d 时的 %s", hit, got, hit-100, prev)
		}
		prev = got
	}
}

// 拆分出来的两项合起来溢出时拒绝，而不是交回一个夹住的数。
//
// 两项各自 = 2^62 个单位，和 = 2^63 放不进 int64。末尾那次 SumChecked 判不出来
// （它看到的是夹住之后的 input），所以这一条守的是输入侧那个显式判据。
func TestReserveRejectsConfirmedCacheSumOverflow(t *testing.T) {
	p62 := money.MustParse("46116860184.73879040")
	_, err := Reserve(ReserveRequest{
		Prices:               Prices{Prompt: p62, CacheRead: p62},
		InputTokens:          2,
		ConfirmedCacheTokens: 1,
		OutputTokens:         1,
	})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("期望溢出错误，得到 %v", err)
	}
}
