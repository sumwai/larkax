package pricing

import (
	"context"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 报价折叠是纯函数，不连库。
//
// 它的输入是「每个候选各自的价」，输出是「给调用方看的一行行报价」；
// 这里要钉住的是折叠与排序的判据，SQL 取规则那一半在集成用例里。

func candidate(model string, channelID, ownerID int64, merchant string) Candidate {
	return Candidate{Model: model, ChannelID: channelID, OwnerUserID: ownerID, Merchant: merchant}
}

func candidatePrice(model string, channelID, ownerID int64, merchant string, prices map[string]money.Amount) CandidatePrice {
	price := CandidatePrice{
		Model:       model,
		ChannelID:   channelID,
		OwnerUserID: ownerID,
		Merchant:    merchant,
		Prices:      prices,
	}
	price.Price = prices["prompt"]
	return price
}

// 同一商家、同一整套维度价的多条渠道折成一条，ChannelCount 记条数。
func TestGroupByModelAggregatesSamePrices(t *testing.T) {
	model := "alpha"
	prices := map[string]money.Amount{"prompt": money.MustParse("0.00300000")}
	got := GroupByModel(
		[]CandidatePrice{
			candidatePrice(model, 1, 7, "acme", prices),
			candidatePrice(model, 2, 7, "acme", prices),
			candidatePrice(model, 3, 8, "globex", prices),
		},
		[]Candidate{
			candidate(model, 1, 7, "acme"),
			candidate(model, 2, 7, "acme"),
			candidate(model, 3, 8, "globex"),
		},
	)[model]

	if len(got.Offers) != 2 {
		t.Fatalf("报价条数 = %d，期望 2：%+v", len(got.Offers), got.Offers)
	}
	if got.UnpricedChannels != 0 {
		t.Errorf("unpriced_channels = %d，期望 0", got.UnpricedChannels)
	}
	if got.Offers[0].ChannelCount != 2 {
		t.Errorf("acme 的 channel_count = %d，期望 2", got.Offers[0].ChannelCount)
	}
	if got.Offers[1].ChannelCount != 1 {
		t.Errorf("globex 的 channel_count = %d，期望 1", got.Offers[1].ChannelCount)
	}
}

// 同一商家、输入价相同但缓存价不同：这是两套报价，不能按 prompt 折成一条。
func TestGroupByModelSplitsByWholePriceSet(t *testing.T) {
	model := "alpha"
	got := GroupByModel(
		[]CandidatePrice{
			candidatePrice(model, 1, 7, "acme", map[string]money.Amount{
				"prompt": money.MustParse("0.00300000"), "cache_read": money.MustParse("0.00060000"),
			}),
			candidatePrice(model, 2, 7, "acme", map[string]money.Amount{
				"prompt": money.MustParse("0.00300000"), "cache_read": money.MustParse("0.00120000"),
			}),
		},
		[]Candidate{
			candidate(model, 1, 7, "acme"),
			candidate(model, 2, 7, "acme"),
		},
	)[model]

	if len(got.Offers) != 2 {
		t.Fatalf("报价条数 = %d，期望 2（整套价不同）：%+v", len(got.Offers), got.Offers)
	}
	for _, offer := range got.Offers {
		if offer.ChannelCount != 1 {
			t.Errorf("每套价应当各一条渠道，得到 %d", offer.ChannelCount)
		}
	}
}

// 没有价的渠道不进 offers，但要在 unpriced_channels 里被数到。
func TestGroupByModelCountsUnpricedChannels(t *testing.T) {
	model := "alpha"
	got := GroupByModel(
		[]CandidatePrice{
			candidatePrice(model, 1, 7, "acme", map[string]money.Amount{
				"prompt": money.MustParse("0.00300000"),
			}),
		},
		[]Candidate{
			candidate(model, 1, 7, "acme"),
			candidate(model, 2, 8, "globex"),
			candidate(model, 3, 8, "globex"),
		},
	)[model]

	if len(got.Offers) != 1 {
		t.Fatalf("报价条数 = %d，期望 1：%+v", len(got.Offers), got.Offers)
	}
	if got.UnpricedChannels != 2 {
		t.Errorf("unpriced_channels = %d，期望 2", got.UnpricedChannels)
	}
}

// 一条有价的都没有的模型也要出现在结果里：它的 unpriced_channels 是
// 「有几条供给」，不是 0。
func TestGroupByModelKeepsFullyUnpricedModels(t *testing.T) {
	got := GroupByModel(nil, []Candidate{
		candidate("alpha", 1, 0, ""),
		candidate("alpha", 2, 0, ""),
	})["alpha"]

	if len(got.Offers) != 0 {
		t.Errorf("没有价却有 offers：%+v", got.Offers)
	}
	if got.UnpricedChannels != 2 {
		t.Errorf("unpriced_channels = %d，期望 2", got.UnpricedChannels)
	}
}

// 同价时平台自营排最前，再按商家名；价格是主键。
func TestGroupByModelSortsByPromptThenPlatformThenMerchant(t *testing.T) {
	model := "alpha"
	cheap := map[string]money.Amount{"prompt": money.MustParse("0.00200000")}
	expensive := map[string]money.Amount{"prompt": money.MustParse("0.00400000")}
	tied := map[string]money.Amount{"prompt": money.MustParse("0.00300000")}
	got := GroupByModel(
		[]CandidatePrice{
			candidatePrice(model, 1, 7, "acme", tied),
			candidatePrice(model, 2, 0, "", tied),
			candidatePrice(model, 3, 8, "globex", cheap),
			candidatePrice(model, 4, 9, "zeta", expensive),
		},
		[]Candidate{
			candidate(model, 1, 7, "acme"),
			candidate(model, 2, 0, ""),
			candidate(model, 3, 8, "globex"),
			candidate(model, 4, 9, "zeta"),
		},
	)[model]

	if len(got.Offers) != 4 {
		t.Fatalf("报价条数 = %d，期望 4：%+v", len(got.Offers), got.Offers)
	}
	if got.Offers[0].Merchant == nil || *got.Offers[0].Merchant != "globex" {
		t.Errorf("最便宜的应当是 globex，得到 %+v", got.Offers[0])
	}
	if got.Offers[1].Merchant != nil {
		t.Errorf("同价时平台自营应当排第二，得到 %+v", got.Offers[1])
	}
	if got.Offers[2].Merchant == nil || *got.Offers[2].Merchant != "acme" {
		t.Errorf("同价时商家按名字排，acme 应当在 globex 之后，得到 %+v", got.Offers[2])
	}
	if got.Offers[3].Merchant == nil || *got.Offers[3].Merchant != "zeta" {
		t.Errorf("最贵的应当是 zeta，得到 %+v", got.Offers[3])
	}
}

// 规则只写 completion 时，排序用第一个存在的维度；Price 仍是 prompt
// （没写就是 0，Prices 里也没有这个键）。
func TestGroupByModelFallsBackToFirstDimensionForSorting(t *testing.T) {
	model := "alpha"
	got := GroupByModel(
		[]CandidatePrice{
			candidatePrice(model, 1, 7, "acme", map[string]money.Amount{
				"completion": money.MustParse("0.00900000"),
			}),
			candidatePrice(model, 2, 8, "globex", map[string]money.Amount{
				"completion": money.MustParse("0.00300000"),
			}),
		},
		[]Candidate{
			candidate(model, 1, 7, "acme"),
			candidate(model, 2, 8, "globex"),
		},
	)[model]

	if len(got.Offers) != 2 {
		t.Fatalf("报价条数 = %d，期望 2：%+v", len(got.Offers), got.Offers)
	}
	if got.Offers[0].Merchant == nil || *got.Offers[0].Merchant != "globex" {
		t.Errorf("completion 更低的 globex 应当排第一，得到 %+v", got.Offers[0])
	}
	if got.Offers[0].Price != money.Zero {
		t.Errorf("没有 prompt 时 Price = %s，期望 0", got.Offers[0].Price)
	}
	if _, ok := got.Offers[0].Prices["prompt"]; ok {
		t.Errorf("规则没写 prompt，报价里却出现了它：%+v", got.Offers[0].Prices)
	}
}

// 空候选不连库直接返回空结果；模型名为空是调用方的错，不是「没有报价」。
func TestPricesHandlesEmptyAndInvalidCandidates(t *testing.T) {
	offers := NewOffers(nil, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), time.Now().UTC(), nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("空候选 = %+v, %v，期望非 nil 的空切片与 nil 错误", got, err)
	}
	if _, err := offers.Prices(context.Background(), time.Now().UTC(), []Candidate{
		{Model: "", ChannelID: 1},
	}); err == nil {
		t.Error("模型名为空时没有报错")
	}
}

// 同商家、同 prompt、不同整套价：顺序由整套价的签名决定，而不是 map 的
// 迭代顺序。跑多轮是因为 grouped 是 map，单轮可能恰好撞上「没排序也对」。
func TestGroupByModelOrdersSameMerchantByWholePriceSet(t *testing.T) {
	const model = "alpha"
	low := map[string]money.Amount{
		"prompt":     money.MustParse("0.00300000"),
		"cache_read": money.MustParse("0.00060000"),
	}
	high := map[string]money.Amount{
		"prompt":     money.MustParse("0.00300000"),
		"cache_read": money.MustParse("0.00120000"),
	}
	prices := []CandidatePrice{
		candidatePrice(model, 1, 7, "acme", low),
		candidatePrice(model, 2, 7, "acme", high),
	}
	candidates := []Candidate{
		candidate(model, 1, 7, "acme"),
		candidate(model, 2, 7, "acme"),
	}

	for round := 0; round < 100; round++ {
		got := GroupByModel(prices, candidates)[model]
		if len(got.Offers) != 2 {
			t.Fatalf("第 %d 轮报价条数 = %d，期望 2", round, len(got.Offers))
		}
		if value := got.Offers[0].Prices["cache_read"]; value != low["cache_read"] {
			t.Fatalf("第 %d 轮第一条的 cache_read = %s，期望签名较小的 %s",
				round, value, low["cache_read"])
		}
	}
}

// 不同商家、完全相同的价：按商家名排，而不是按 map 的迭代顺序。
// 只比整套价的话两套价一模一样，比较器必须落到「商家名」这一档。
func TestGroupByModelOrdersMerchantsByNameOnEqualPrices(t *testing.T) {
	const model = "alpha"
	prices := map[string]money.Amount{"prompt": money.MustParse("0.00300000")}
	names := []string{"zeta", "acme", "globex"}
	candidates := make([]Candidate, 0, len(names))
	quotes := make([]CandidatePrice, 0, len(names))
	for i, name := range names {
		candidates = append(candidates, candidate(model, int64(i+1), int64(10+i), name))
		quotes = append(quotes, candidatePrice(model, int64(i+1), int64(10+i), name, prices))
	}

	for round := 0; round < 100; round++ {
		got := GroupByModel(quotes, candidates)[model]
		if len(got.Offers) != len(names) {
			t.Fatalf("第 %d 轮报价条数 = %d，期望 %d", round, len(got.Offers), len(names))
		}
		want := []string{"acme", "globex", "zeta"}
		for i, name := range want {
			if got.Offers[i].Merchant == nil || *got.Offers[i].Merchant != name {
				t.Fatalf("第 %d 轮第 %d 条 = %+v，期望 %s", round, i, got.Offers[i], name)
			}
		}
	}
}

// 报价排序先分量纲：有 token 价的按 token 价升序在前，只有一次性费用的
// 排在它们之后，组内按每请求固定费、再按最低消费升序。
//
// 四种情形各来一条：只有固定费、只有最低消费、两者都有、token 价 + 固定费。
// 关键判据是「只有固定费的不再排在最前」——它的 offerSortPrice 是零值，
// 按那个键升序会让「按次收费」看起来像最便宜的一档。
func TestGroupByModelSortsTokenPricesBeforeFixedOnly(t *testing.T) {
	const model = "alpha"
	fixed := func(channelID, ownerID int64, merchant string, request, minimum string) CandidatePrice {
		price := candidatePrice(model, channelID, ownerID, merchant, nil)
		price.Request = money.MustParse(request)
		price.Minimum = money.MustParse(minimum)
		return price
	}
	quotes := []CandidatePrice{
		fixed(1, 8, "globex", "0.20000000", "0.00000000"),    // 只有固定费
		fixed(2, 11, "hooli", "0.00000000", "5.00000000"),    // 只有最低消费
		fixed(3, 10, "umbrella", "0.01000000", "2.00000000"), // 两者都有
		fixed(4, 9, "initech", "0.01000000", "0.50000000"),   // 同上，最低消费更小
		candidatePrice(model, 5, 7, "acme", map[string]money.Amount{ // token 价 + 固定费
			"prompt": money.MustParse("0.00200000"),
		}),
		candidatePrice(model, 6, 0, "", map[string]money.Amount{ // 平台自营，只有 token 价
			"prompt": money.MustParse("0.00400000"),
		}),
	}
	quotes[4].Request = money.MustParse("0.05000000")
	candidates := make([]Candidate, 0, len(quotes))
	for _, quote := range quotes {
		candidates = append(candidates, candidate(model, quote.ChannelID, quote.OwnerUserID, quote.Merchant))
	}

	got := GroupByModel(quotes, candidates)[model]
	if len(got.Offers) != len(quotes) {
		t.Fatalf("报价条数 = %d，期望 %d：%+v", len(got.Offers), len(quotes), got.Offers)
	}
	want := []struct {
		merchant string // 空串表示平台自营
		hasToken bool
	}{
		{"acme", true},      // token 价最低
		{"", true},          // 平台自营的 token 价
		{"hooli", false},    // 只有最低消费：固定费是 0，排在这一组最前
		{"initech", false},  // 固定费 0.01、最低消费 0.5
		{"umbrella", false}, // 固定费相同，最低消费更大
		{"globex", false},   // 只有固定费，且最大
	}
	for i, expected := range want {
		if i >= len(got.Offers) {
			break
		}
		offer := got.Offers[i]
		merchant := ""
		if offer.Merchant != nil {
			merchant = *offer.Merchant
		}
		if merchant != expected.merchant {
			t.Fatalf("第 %d 条 = %q，期望 %q：%+v", i, merchant, expected.merchant, got.Offers)
		}
		if hasToken := len(offer.Prices) > 0; hasToken != expected.hasToken {
			t.Fatalf("第 %d 条（%s）有 token 价 = %v，期望 %v", i, merchant, hasToken, expected.hasToken)
		}
	}
}

// 三个排序键完全相同的两笔报价，末位由渠道 id 分开——其中渠道 id 小的在前。
//
// 这个输入在库里造不出来（users.username 唯一，同名即同一商家，整套价相同
// 会被折成一条），但比较器不能依赖这个前提：一旦落到 map 的迭代顺序上，
// 同一份数据两次请求就可能给出不同的行序，而照行序读页面的人看不出这是随机的。
func TestGroupByModelBreaksTiesByChannelID(t *testing.T) {
	const model = "alpha"
	prices := map[string]money.Amount{"prompt": money.MustParse("0.00300000")}
	base := CandidatePrice{Model: model, Prices: prices, Price: prices["prompt"],
		Request: money.MustParse("0.05000000"), Minimum: money.MustParse("1.00000000")}
	higher := base
	higher.ChannelID, higher.OwnerUserID, higher.Merchant = 7, 11, "acme"
	higher.Rule = RuleRef{ID: 71}
	lower := base
	lower.ChannelID, lower.OwnerUserID, lower.Merchant = 3, 12, "acme"
	lower.Rule = RuleRef{ID: 32}

	candidates := []Candidate{candidate(model, 7, 11, "acme"), candidate(model, 3, 12, "acme")}
	for round := 0; round < 100; round++ {
		got := GroupByModel([]CandidatePrice{higher, lower}, candidates)[model]
		if len(got.Offers) != 2 {
			t.Fatalf("第 %d 轮报价条数 = %d，期望 2", round, len(got.Offers))
		}
		if got.Offers[0].Rule.ID != 32 {
			t.Fatalf("第 %d 轮第一条的规则 = %d，期望渠道 id 较小的 32",
				round, got.Offers[0].Rule.ID)
		}
	}
}

// 一次性费用与计量项单价随整套报价带出来，并且与逐维度价出自同一次求值：
// 最低消费不抬高每千 token 的单价，固定费也不进 Prices。
func TestPricesOfCarriesChargesAndUnits(t *testing.T) {
	offers := NewOffers(nil, "Asia/Shanghai", nil)
	at := time.Now().UTC()
	got, err := offers.pricesOf(1, `{
		"prompt": 0.000002,
		"fixed": 0.05,
		"minimum": 1.2,
		"units": {"cache_write_5m": 0.0003}
	}`, at)
	if err != nil {
		t.Fatalf("pricesOf 出错: %v", err)
	}
	if want := money.MustParse("0.00200000"); got.Prices["prompt"] != want {
		t.Errorf("prompt 千 token 价 = %s，期望 %s（固定费与最低消费不进单价）",
			got.Prices["prompt"], want)
	}
	if want := money.MustParse("0.05000000"); got.Request != want {
		t.Errorf("request = %s，期望 charges.Fixed 的 %s", got.Request, want)
	}
	if want := money.MustParse("1.20000000"); got.Minimum != want {
		t.Errorf("minimum = %s，期望 charges.Minimum 的 %s", got.Minimum, want)
	}
	if want := money.MustParse("0.00030000"); got.Units["cache_write_5m"] != want {
		t.Errorf("units[cache_write_5m] = %s，期望 %s", got.Units["cache_write_5m"], want)
	}
}

// 规则没写固定费、最低消费与计量项时，它们保持零值：调用方据此省略键，
// 而不是编一个 0。写了 0 与没写在报价里都不出键（两者都一分不收）。
func TestPricesOfOmitsAbsentChargesAndUnits(t *testing.T) {
	offers := NewOffers(nil, "Asia/Shanghai", nil)
	got, err := offers.pricesOf(2, `{"prompt": 0.000002, "fixed": 0}`, time.Now().UTC())
	if err != nil {
		t.Fatalf("pricesOf 出错: %v", err)
	}
	if got.Request != money.Zero {
		t.Errorf("request = %s，期望 0（规则写了 fixed: 0）", got.Request)
	}
	if got.Minimum != money.Zero {
		t.Errorf("minimum = %s，期望 0", got.Minimum)
	}
	if len(got.Units) != 0 {
		t.Errorf("units = %+v，期望空（规则没写计量项单价）", got.Units)
	}
}

// 折叠时把一次性费用、计量项单价与规则来源原样带到 Offer 上，并且
// Offer.Price 仍等于 Prices 里的 prompt 一项。
func TestGroupByModelCarriesChargesUnitsAndRule(t *testing.T) {
	const model = "alpha"
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	price := CandidatePrice{
		Model:       model,
		ChannelID:   1,
		OwnerUserID: 7,
		Merchant:    "acme",
		Prices:      map[string]money.Amount{"prompt": money.MustParse("0.00300000")},
		Price:       money.MustParse("0.00300000"),
		Request:     money.MustParse("0.05000000"),
		Minimum:     money.MustParse("1.00000000"),
		Units:       map[string]money.Amount{"cache_write_5m": money.MustParse("0.00030000")},
		Rule:        RuleRef{ID: 42, Scope: "merchant", Priority: 10, EffectiveFrom: from, EffectiveTo: &to},
	}

	got := GroupByModel([]CandidatePrice{price}, []Candidate{candidate(model, 1, 7, "acme")})[model]
	if len(got.Offers) != 1 {
		t.Fatalf("报价条数 = %d，期望 1：%+v", len(got.Offers), got.Offers)
	}
	offer := got.Offers[0]
	if offer.Price != offer.Prices["prompt"] {
		t.Errorf("price = %s 与 prices.prompt = %s 不一致",
			offer.Price, offer.Prices["prompt"])
	}
	if offer.Request != price.Request || offer.Minimum != price.Minimum {
		t.Errorf("request/minimum = %s/%s，期望 %s/%s",
			offer.Request, offer.Minimum, price.Request, price.Minimum)
	}
	if offer.Units["cache_write_5m"] != price.Units["cache_write_5m"] {
		t.Errorf("units = %+v，期望 %+v", offer.Units, price.Units)
	}
	if offer.Rule.ID != 42 || offer.Rule.Scope != "merchant" || offer.Rule.Priority != 10 {
		t.Errorf("rule = %+v，期望原样带出规则元信息", offer.Rule)
	}
	if !offer.Rule.EffectiveFrom.Equal(from) {
		t.Errorf("rule.effective_from = %v，期望 %v", offer.Rule.EffectiveFrom, from)
	}
	if offer.Rule.EffectiveTo == nil || !offer.Rule.EffectiveTo.Equal(to) {
		t.Errorf("rule.effective_to = %v，期望 %v", offer.Rule.EffectiveTo, to)
	}
}

// token 价完全相同、只有固定费不同，是两套报价——不能按 token 价折成一条。
func TestGroupByModelSplitsByCharges(t *testing.T) {
	const model = "alpha"
	tokenPrices := map[string]money.Amount{"prompt": money.MustParse("0.00300000")}
	base := CandidatePrice{Model: model, OwnerUserID: 7, Merchant: "acme",
		Prices: tokenPrices, Price: tokenPrices["prompt"]}
	withFee := base
	withFee.ChannelID = 2
	withFee.Request = money.MustParse("0.05000000")

	got := GroupByModel(
		[]CandidatePrice{func() CandidatePrice { p := base; p.ChannelID = 1; return p }(), withFee},
		[]Candidate{candidate(model, 1, 7, "acme"), candidate(model, 2, 7, "acme")},
	)[model]
	if len(got.Offers) != 2 {
		t.Fatalf("报价条数 = %d，期望 2（固定费不同）：%+v", len(got.Offers), got.Offers)
	}
}

// 规则显式写 0 与「没写」是两件事：写了 0 的维度与计量项要出现在报价里，
// 值为 0；没写的维度仍然不出现。结算据 Prices.Declared 区分「免费」与
// 「漏配」，报价不能把「写了 0」丢掉。
func TestPricesOfKeepsExplicitZeroPrices(t *testing.T) {
	offers := NewOffers(nil, "Asia/Shanghai", nil)
	got, err := offers.pricesOf(3, `{
		"prompt": 0,
		"completion": 0.000003,
		"units": {"cache_write_5m": 0}
	}`, time.Now().UTC())
	if err != nil {
		t.Fatalf("pricesOf 出错: %v", err)
	}

	value, ok := got.Prices["prompt"]
	if !ok {
		t.Fatalf("规则写了 prompt: 0，报价里却没有这个维度：%+v", got.Prices)
	}
	if value != money.Zero {
		t.Errorf("prompt 报价 = %s，期望 0（显式免费）", value)
	}
	if _, declared := got.Prices["cache_read"]; declared {
		t.Errorf("规则没写 cache_read，报价里却有它：%+v", got.Prices)
	}
	unit, ok := got.Units["cache_write_5m"]
	if !ok {
		t.Fatalf("规则写了 cache_write_5m: 0，Units 里却没有这一项：%+v", got.Units)
	}
	if unit != money.Zero {
		t.Errorf("cache_write_5m 单价 = %s，期望 0", unit)
	}
}
