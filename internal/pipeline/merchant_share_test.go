package pipeline

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/tokens"
)

// shareOf 造一个比例取自固定函数的策略。
func shareOf(get func() (int64, error)) *MerchantShare {
	return &MerchantShare{BasisPoints: get}
}

// fixedShare 造一个比例固定的策略。
func fixedShare(bp int64) *MerchantShare {
	return shareOf(func() (int64, error) { return bp, nil })
}

func TestMerchantShareBasisPointsFor(t *testing.T) {
	m := fixedShare(9500)

	cases := []struct {
		name       string
		merchantID int64
		want       int64
	}{
		{"渠道商户拿配置的比例", 9, 9500},
		// 平台自营渠道：钱本来就留在平台，记一笔 merchant_payable
		// 会让账本多出一个不存在的债权人。
		{"平台自营不分账", 0, 0},
		{"非正 id 不分账", -3, 0},
	}
	for _, tc := range cases {
		if got := m.BasisPointsFor(context.Background(), tc.merchantID); got != tc.want {
			t.Errorf("%s：BasisPointsFor(%d) = %d，期望 %d", tc.name, tc.merchantID, got, tc.want)
		}
	}
}

// 没有配置来源时一律不分账，不 panic、不告警。
func TestMerchantShareWithoutSourceSharesNothing(t *testing.T) {
	m := &MerchantShare{}
	if got := m.BasisPointsFor(context.Background(), 9); got != 0 {
		t.Errorf("没有取值来源时应返回 0，得到 %d", got)
	}
}

// 读不出来与越界都按 0 处理，并且各留一条告警：分成恒为 0 与「还没配置」
// 在账面上长得一样，日志是唯一能分开两者的地方。
func TestMerchantShareFallsBackToZeroOnInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		get  func() (int64, error)
	}{
		{"比例读不出来", func() (int64, error) { return 0, errors.New("settings.billing.merchant_share_bp 不存在") }},
		{"比例是负数", func() (int64, error) { return -1, nil }},
		{"比例超过 10000", func() (int64, error) { return 10001, nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			adoptTestLogger(t, &logs)

			if got := shareOf(tc.get).BasisPointsFor(context.Background(), 9); got != 0 {
				t.Errorf("非法配置应按 0 处理，得到 %d", got)
			}
			if !strings.Contains(logs.String(), "商户分成比例") {
				t.Errorf("应写一条告警，实际日志:\n%s", logs.String())
			}
		})
	}
}

// 告警按值去重：同一个非法值只报一次，值变了再报，回到合法之后
// 同一个值再变坏要重新报。
func TestMerchantShareWarnsOncePerValue(t *testing.T) {
	var logs bytes.Buffer
	adoptTestLogger(t, &logs)

	bp := int64(20000)
	m := shareOf(func() (int64, error) { return bp, nil })
	ctx := context.Background()

	count := func() int { return strings.Count(logs.String(), "商户分成比例") }

	m.BasisPointsFor(ctx, 9)
	m.BasisPointsFor(ctx, 9)
	m.BasisPointsFor(ctx, 7)
	if got := count(); got != 1 {
		t.Fatalf("同一个非法值应只报一条，实际 %d 条:\n%s", got, logs.String())
	}

	bp = 30000
	m.BasisPointsFor(ctx, 9)
	if got := count(); got != 2 {
		t.Fatalf("值变了应再报一条，实际 %d 条:\n%s", got, logs.String())
	}

	// 合法值不告警
	bp = 9500
	if got := m.BasisPointsFor(ctx, 9); got != 9500 {
		t.Fatalf("合法值应原样返回，得到 %d", got)
	}
	if got := count(); got != 2 {
		t.Fatalf("合法值不该告警，实际 %d 条:\n%s", got, logs.String())
	}

	// 回到同一个非法值：中间好过一段，应当重新报
	bp = 20000
	m.BasisPointsFor(ctx, 9)
	if got := count(); got != 3 {
		t.Fatalf("回到非法值应再报一条，实际 %d 条:\n%s", got, logs.String())
	}
}

// 0 是合法比例：不分账，但**不是**配置出错，不该留告警。
//
// 越界判据是 `bp < 0 || bp > 10000`。把它写成 `bp <= 0` 时 0 会被当成越界，
// 分成金额不变（两版都返回 0），差别只在日志里多一条「超出 0…10000 基点」
// ——而 0 是默认值，每个请求都报一条越界，日志里真正该看的越界反而找不到了。
// 所以这一格不能放过，得有输入能把它分开。
func TestMerchantShareZeroIsLegal(t *testing.T) {
	var logs bytes.Buffer
	adoptTestLogger(t, &logs)

	if got := fixedShare(0).BasisPointsFor(context.Background(), 9); got != 0 {
		t.Errorf("比例为 0 时应返回 0，得到 %d", got)
	}
	if logs.Len() != 0 {
		t.Errorf("比例为 0 是合法配置，不该写告警，实际日志:\n%s", logs.String())
	}
}

// settleWithShare 预留并结算一次请求，输入 token 数取自 req；不给输出，
// 所以夹具里的规则只需要写输入单价。
func settleWithShare(t *testing.T, p *Pipeline, req Request) *Charge {
	t.Helper()

	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: req.InputTokens},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}
	return charge
}

// ruleWithoutOutput 只给输入单价：这次请求没有输出，结算时不需要输出价。
const ruleWithoutOutput = `{"prompt": 0.0001}`

func pipelineForShare(t *testing.T, bp func() (int64, error)) *Pipeline {
	t.Helper()
	return newPipeline(t,
		&ruleSource{rule: Rule{ID: 1, Version: 1, Source: ruleWithoutOutput}},
		func(p *Pipeline) { p.MerchantShare = shareOf(bp) })
}

// 平台抽 5%（商户 95%）时，0.1 元消费里商户应付增加 0.095 元，
// 用户被扣 0.1 元，平台留下 0.005 元。
func TestSettleSplitsPriceWithMerchant(t *testing.T) {
	p := pipelineForShare(t, func() (int64, error) { return 9500, nil })

	req := baseRequest()
	charge := settleWithShare(t, p, req)

	if got := charge.Settlement.Actual.String(); got != "0.10000000" {
		t.Fatalf("售价 = %s，期望 0.1", got)
	}

	merchant := amountOfAccount(t, charge.Entries, ledger.MerchantPayableAccount(9))
	if want := money.MustParse("0.095"); merchant.Cmp(want) != 0 {
		t.Errorf("商户分成 = %s，期望 %s", merchant, want)
	}

	// 三边平：用户出钱、商户应付、平台收入。
	if got := amountOfAccount(t, charge.Entries, ledger.UserAccount(req.UserID)); got.Cmp(money.MustParse("-0.1")) != 0 {
		t.Errorf("用户分录 = %s，期望 -0.1", got)
	}
	if got := amountOfAccount(t, charge.Entries, ledger.PlatformRevenueAccount()); got.Cmp(money.MustParse("0.005")) != 0 {
		t.Errorf("平台收入 = %s，期望 0.005", got)
	}

	sum := money.Zero
	for _, e := range charge.Entries {
		sum = sum.Add(e.Amount)
	}
	if !sum.IsZero() {
		t.Errorf("分录不平：合计 %s（分录 %v）", sum, charge.Entries)
	}
}

// 需要舍入时向下取整，零头归平台：售价 3 个最小单位、分成 50%，
// 商户拿 1 个，平台拿 2 个。
func TestSettleRoundsMerchantShareDown(t *testing.T) {
	p := newPipeline(t,
		&ruleSource{rule: Rule{ID: 1, Version: 1, Source: `{"prompt": 0.00000003}`}},
		func(p *Pipeline) { p.MerchantShare = fixedShare(5000) })

	req := baseRequest()
	req.InputTokens = 1
	req.MaxTokens = 1
	charge := settleWithShare(t, p, req)

	if got := charge.Settlement.Actual.String(); got != "0.00000003" {
		t.Fatalf("售价 = %s，期望 0.00000003", got)
	}
	if got := amountOfAccount(t, charge.Entries, ledger.MerchantPayableAccount(9)); got.String() != "0.00000001" {
		t.Errorf("商户分成 = %s，期望 0.00000001（向下取整）", got)
	}
	if got := amountOfAccount(t, charge.Entries, ledger.PlatformRevenueAccount()); got.String() != "0.00000002" {
		t.Errorf("平台收入 = %s，期望 0.00000002（零头归平台）", got)
	}
}

// 比例边界：0 不分账，10000 全部分给商户、平台收入为零。
//
// 变异：`entries` 里 `if bp := …; bp > 0` 改成 `>= 0`。这一处是**等价变异**：
// BasisPointsFor 的返回值只可能是 0…10000（读不出来与越界都在它里面折成 0，
// 负数商户直接返回 0），所以 `bp` 不会为负，`>= 0` 与 `> 0` 只在 `bp == 0`
// 那一格分岔。那一格变异版会进分支，但 `MulBasisPoints(0)` 在 bp 为零时
// 早退返回 Zero（见 money 里同一个判据的说明），`share` 仍是零。
// 下面的「0 不分账」就是那一格：两版算出来是同一组分录。
func TestSettleMerchantShareBoundaries(t *testing.T) {
	t.Run("0 不分账", func(t *testing.T) {
		p := pipelineForShare(t, func() (int64, error) { return 0, nil })
		charge := settleWithShare(t, p, baseRequest())

		if hasAccount(charge.Entries, ledger.MerchantPayableAccount(9)) {
			t.Errorf("比例为 0 时不该有商户分录：%v", charge.Entries)
		}
		if len(charge.Entries) != 2 {
			t.Errorf("分录应为用户与平台两条，实际 %d 条：%v", len(charge.Entries), charge.Entries)
		}
	})

	t.Run("10000 全部归商户", func(t *testing.T) {
		p := pipelineForShare(t, func() (int64, error) { return 10000, nil })
		charge := settleWithShare(t, p, baseRequest())

		if hasAccount(charge.Entries, ledger.PlatformRevenueAccount()) {
			t.Errorf("全部分给商户时平台收入应为零，不该有那条分录：%v", charge.Entries)
		}
		if got := amountOfAccount(t, charge.Entries, ledger.MerchantPayableAccount(9)); got.Cmp(money.MustParse("0.1")) != 0 {
			t.Errorf("商户分成 = %s，期望全额 0.1", got)
		}
	})
}

// 平台自营渠道（商户 id 为 0）即使配了比例也不分账：没有商户可记。
func TestSettlePlatformChannelKeepsEverything(t *testing.T) {
	p := pipelineForShare(t, func() (int64, error) { return 9500, nil })

	req := baseRequest()
	req.OwnerUserID = 0
	req.Candidates = []RuleCandidate{{ChannelID: 7, OwnerUserID: 0}}
	charge := settleWithShare(t, p, req)

	for _, e := range charge.Entries {
		if e.Account.Type() == ledger.AccountMerchantPayable {
			t.Errorf("平台自营渠道不该有商户分录：%v", e)
		}
	}
	if got := amountOfAccount(t, charge.Entries, ledger.PlatformRevenueAccount()); got.Cmp(money.MustParse("0.1")) != 0 {
		t.Errorf("平台收入 = %s，期望全额 0.1", got)
	}
}

// 配置读不出来时结算照常完成，只是不分账——并且这条告警不会每个请求
// 刷一条。
func TestSettleSurvivesUnreadableShareConfig(t *testing.T) {
	var logs bytes.Buffer
	adoptTestLogger(t, &logs)

	p := pipelineForShare(t, func() (int64, error) {
		return 0, errors.New("settings.billing.merchant_share_bp 不存在")
	})

	for i := 0; i < 2; i++ {
		charge := settleWithShare(t, p, baseRequest())
		if hasAccount(charge.Entries, ledger.MerchantPayableAccount(9)) {
			t.Errorf("配置读不出来时不该有商户分录：%v", charge.Entries)
		}
	}

	if got := strings.Count(logs.String(), "商户分成比例"); got != 1 {
		t.Errorf("同一个非法值应只报一条告警，实际 %d 条:\n%s", got, logs.String())
	}
}

// amountOfAccount 取某个科目在分录里的合计。找不到时直接失败：
// 返回零值会让「没有这条分录」表现为「金额不对」，排查方向整个偏掉。
func amountOfAccount(t *testing.T, entries []ledger.Entry, account ledger.Account) money.Amount {
	t.Helper()
	if !hasAccount(entries, account) {
		t.Fatalf("分录里没有 %s：%v", account, entries)
	}
	total := money.Zero
	for _, e := range entries {
		if e.Account == account {
			total = total.Add(e.Amount)
		}
	}
	return total
}

func hasAccount(entries []ledger.Entry, account ledger.Account) bool {
	for _, e := range entries {
		if e.Account == account {
			return true
		}
	}
	return false
}
