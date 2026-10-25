package plans

import (
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// TestPurchasePlanMismatchPinsEachBranch 逐个分支钉 purchasePlanMismatch。
//
// 为什么要有纯函数这一层：五条分支里**币种那条在真库上走不到**——
// `ledger.Asset.Valid()` 只认 CNY，包上写了别的币种时 scanPlan 在读的时候
// 就报「资产 ... 不受支持」，根本到不了这里。少了这条用例，币种分支删掉
// 也不会有人发现（真库那几条 `TestPlanPurchaseRejects*` 全绿）。
//
// 每条断言都钉**这一条分支自己的文案**：只断言「有错」的话，前面那条分支
// 先报也能让用例绿，红的地方就不对了。
func TestPurchasePlanMismatchPinsEachBranch(t *testing.T) {
	days := 30
	base := Plan{
		Key:          "mismatch-probe",
		FaceValue:    money.MustParse("88"),
		Asset:        ledger.AssetCNY,
		Enabled:      true,
		ValidityDays: &days,
	}
	owner := int64(4242)
	longer := 365
	cases := []struct {
		name string
		edit func(*Plan)
		want []string
	}{
		{
			name: "面值在售出之后改过",
			edit: func(p *Plan) { p.FaceValue = money.MustParse("99") },
			want: []string{"面值在售出之后改过", "88", "99"},
		},
		{
			name: "币种在售出之后改过",
			edit: func(p *Plan) { p.Asset = ledger.Asset("USDT") },
			want: []string{"币种在售出之后改过", string(ledger.AssetCNY), "USDT"},
		},
		{
			name: "在售出之后被停用",
			edit: func(p *Plan) { p.Enabled = false },
			want: []string{"在售出之后被停用"},
		},
		{
			name: "在售出之后被改成归属商户",
			edit: func(p *Plan) { p.OwnerUserID = &owner },
			want: []string{"归属商户", "4242"},
		},
		{
			// 改长：平台亏。用户按 30 天付钱，拿到 365 天。
			name: "售卖有效期在售出之后改长",
			edit: func(p *Plan) { p.ValidityDays = &longer },
			want: []string{"售卖有效期在售出之后改过", "30", "365"},
		},
		{
			// 改短：用户亏，而且更难举证。单独一条，别与改长合成一条。
			name: "售卖有效期在售出之后改短",
			edit: func(p *Plan) {
				short := 7
				p.ValidityDays = &short
			},
			want: []string{"售卖有效期在售出之后改过", "30", "7"},
		},
		{
			// 被清空：包从「可售」变成「不出售」。这一格不能漏——漏了的话
			// 运维会从别处读到「没有设置售卖有效期」，像是这包从来没设过。
			name: "售卖有效期在售出之后被清空",
			edit: func(p *Plan) { p.ValidityDays = nil },
			want: []string{"被改成不出售", "30"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := base
			tc.edit(&plan)
			err := purchasePlanMismatch(plan, &PurchaseInput{
				ExpectedAmount: money.MustParse("88"),
				ExpectedAsset:  ledger.AssetCNY,
				ValidityDays:   &days,
			})
			if err == nil {
				t.Fatalf("%s 应当被拒", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("报错里没有 %q：%v", want, err)
				}
			}
		})
	}

	// 五样都没变时不能报错：否则正常售卖全被拦住。
	if err := purchasePlanMismatch(base, &PurchaseInput{
		ExpectedAmount: money.MustParse("88"),
		ExpectedAsset:  ledger.AssetCNY,
		ValidityDays:   &days,
	}); err != nil {
		t.Errorf("包没变时不该报错：%v", err)
	}
}
