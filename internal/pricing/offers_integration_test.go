package pricing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/money"
)

// 报价要按**实际选中的那条渠道的商家**取价，并与结算对得上。
//
// 这里的对拍不是「再算一遍」：结算那一侧走的是 Source.Match（结算阶段
// 拿到的恰好一个候选），本文件用它 + 同一个 ComputeWithCharges 算出
// 结算价，再与 Offers.Prices 的结果逐字比较。两条路径共用规则判据与求值
// 函数，差开只可能来自「报价那条整批 SQL 挑错了规则」——那正是要钉住的。
func TestOffersFollowChannelOwnership(t *testing.T) {
	db := testDB(t)
	source := New(db)
	admin := NewAdmin(db)
	ownerA := seedUser(t, db)
	ownerB := seedUser(t, db)
	channelA := seedChannel(t, db, ownerA)
	channelB := seedChannel(t, db, ownerB)
	model := unique("offers-model")
	at := time.Now().UTC().Add(time.Minute)

	createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: ownerA, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000002}`,
	})
	createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: ownerB, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000003}`,
	})

	offers := NewOffers(db, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), at, []Candidate{
		{Model: model, ChannelID: channelA, OwnerUserID: ownerA},
		{Model: model, ChannelID: channelB, OwnerUserID: ownerB},
	})
	if err != nil {
		t.Fatalf("Prices 出错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("报价条数 = %d，期望 2：%+v", len(got), got)
	}

	want := map[int64]money.Amount{
		channelA: money.MustParse("0.00200000"),
		channelB: money.MustParse("0.00300000"),
	}
	for _, price := range got {
		expected, ok := want[price.ChannelID]
		if !ok {
			t.Fatalf("多出渠道 %d 的报价", price.ChannelID)
		}
		if price.Price != expected {
			t.Errorf("渠道 %d 的报价 = %s，期望 %s", price.ChannelID, price.Price, expected)
		}
		settled := settlementPromptPrice(t, source, model, price.ChannelID, price.OwnerUserID, at)
		if price.Price != settled {
			t.Errorf("渠道 %d 的报价 %s 与结算价 %s 不一致", price.ChannelID, price.Price, settled)
		}
	}
}

// 平台自营渠道与商家渠道并存：各自按自己的作用域取价，平台那条不会
// 因为某个商家配了价而拿到商家价。
func TestOffersMixPlatformAndMerchant(t *testing.T) {
	db := testDB(t)
	source := New(db)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	merchantChannel := seedChannel(t, db, owner)
	platformChannel := seedPlatformChannel(t, db)
	model := unique("offers-mix")
	at := time.Now().UTC().Add(time.Minute)

	createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000004}`,
	})
	createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000002}`,
	})

	offers := NewOffers(db, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), at, []Candidate{
		{Model: model, ChannelID: merchantChannel, OwnerUserID: owner},
		{Model: model, ChannelID: platformChannel},
	})
	if err != nil {
		t.Fatalf("Prices 出错: %v", err)
	}
	byChannel := make(map[int64]money.Amount, len(got))
	for _, price := range got {
		byChannel[price.ChannelID] = price.Price
	}
	if got := byChannel[merchantChannel]; got != money.MustParse("0.00200000") {
		t.Errorf("商家渠道的报价 = %s，期望 0.00200000", got)
	}
	if got := byChannel[platformChannel]; got != money.MustParse("0.00400000") {
		t.Errorf("平台渠道的报价 = %s，期望 0.00400000", got)
	}
	for _, price := range got {
		settled := settlementPromptPrice(t, source, model, price.ChannelID, price.OwnerUserID, at)
		if price.Price != settled {
			t.Errorf("渠道 %d 的报价 %s 与结算价 %s 不一致", price.ChannelID, price.Price, settled)
		}
	}
}

// 渠道级规则压过商家级规则：报价与结算选中同一条。
func TestOffersFollowChannelScope(t *testing.T) {
	db := testDB(t)
	source := New(db)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	channel := seedChannel(t, db, owner)
	model := unique("offers-scope")
	at := time.Now().UTC().Add(time.Minute)

	// 商家价更低；渠道自己定的价更高。报价必须是渠道价——否则调用方看到
	// 一个永远结算不到的价。
	createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000002}`,
	})
	createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: channel, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000005}`,
	})

	offers := NewOffers(db, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), at, []Candidate{
		{Model: model, ChannelID: channel, OwnerUserID: owner},
	})
	if err != nil {
		t.Fatalf("Prices 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("报价条数 = %d，期望 1：%+v", len(got), got)
	}
	if want := money.MustParse("0.00500000"); got[0].Price != want {
		t.Errorf("报价 = %s，期望渠道价 %s", got[0].Price, want)
	}
	settled := settlementPromptPrice(t, source, model, channel, owner, at)
	if got[0].Price != settled {
		t.Errorf("报价 %s 与结算价 %s 不一致", got[0].Price, settled)
	}
}

// 一条规则都没有的模型在**报价**结果里不出现——不是 0 元。
//
// 它仍然会出现在模型清单里，只是不带 offers（见 internal/api 的
// TestModelsKeepModelsWithoutOffers）：报价这一层负责「算不出就别编」，
// 「列不列」是清单那一层的判据。
func TestOffersSkipCandidatesWithoutRule(t *testing.T) {
	db := testDB(t)
	owner := seedUser(t, db)
	channel := seedChannel(t, db, owner)
	model := unique("offers-none")

	offers := NewOffers(db, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), time.Now().UTC(), []Candidate{
		{Model: model, ChannelID: channel, OwnerUserID: owner},
	})
	if err != nil {
		t.Fatalf("Prices 出错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("没有规则的模型拿到了报价：%+v", got)
	}
}

// seedPlatformChannel 插一条平台自营渠道（owner_user_id 为 NULL）。
//
// seedChannel 把 owner 原样写进外键列，0 会撞上 users 的外键——平台自营
// 在库里是 NULL，不是 0。
func seedPlatformChannel(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO channels (owner_user_id, name, type, base_url, credential_enc)
		VALUES (NULL, $1, 'openai', 'http://127.0.0.1:1/v1', '\x00'::bytea)
		RETURNING id`, unique("offers-platform-channel")).Scan(&id)
	if err != nil {
		t.Fatalf("插平台渠道失败: %v", err)
	}
	return id
}

// settlementPromptPrice 用**结算那一条路径**算 1000 个输入 token 的价：
// Source.Match 选规则，再用与结算同一个 ComputeWithCharges 求金额。
//
// 它不调用 Offers 的任何代码，所以两次结果的比较是真正的对拍。
func settlementPromptPrice(t *testing.T, source *Source, model string, channelID, ownerID int64, at time.Time) money.Amount {
	t.Helper()

	rule, err := source.Match(context.Background(), singleCandidate(model, channelID, ownerID, at))
	if err != nil {
		t.Fatalf("结算取规则失败: %v", err)
	}
	program, err := expr.Compile(rule.Source)
	if err != nil {
		t.Fatalf("编译结算规则失败: %v", err)
	}
	usage := billing.Usage{PromptTokens: offerTokenCount}
	result, err := program.Eval(expr.Env{At: at, Region: "Asia/Shanghai", Usage: usage})
	if err != nil {
		t.Fatalf("求结算价失败: %v", err)
	}
	return billing.ComputeWithCharges(usage, result.Prices, result.Charges).Prompt
}

// 五个 token 维度各自报一个千 token 价，并与结算逐维度对得上。
func TestOffersReportAllTokenDimensions(t *testing.T) {
	db := testDB(t)
	source := New(db)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	channel := seedChannel(t, db, owner)
	model := unique("offers-dimensions")
	at := time.Now().UTC().Add(time.Minute)

	createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, ModelPatterns: []string{model},
		Expr: `{
			"prompt": 0.000002,
			"completion": 0.000006,
			"cache_read": 0.0000004,
			"cache_write": 0.0000024,
			"reasoning": 0.000010
		}`,
	})

	offers := NewOffers(db, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), at, []Candidate{
		{Model: model, ChannelID: channel, OwnerUserID: owner},
	})
	if err != nil {
		t.Fatalf("Prices 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("报价条数 = %d，期望 1：%+v", len(got), got)
	}
	want := map[string]money.Amount{
		"prompt":      money.MustParse("0.00200000"),
		"completion":  money.MustParse("0.00600000"),
		"cache_read":  money.MustParse("0.00040000"),
		"cache_write": money.MustParse("0.00240000"),
		"reasoning":   money.MustParse("0.01000000"),
	}
	if len(got[0].Prices) != len(want) {
		t.Errorf("维度数 = %d，期望 %d：%+v", len(got[0].Prices), len(want), got[0].Prices)
	}
	for name, expected := range want {
		if value := got[0].Prices[name]; value != expected {
			t.Errorf("%s 的报价 = %s，期望 %s", name, value, expected)
		}
		settled := settlementDimensionPrice(t, source, model, channel, owner, at, name)
		if value := got[0].Prices[name]; value != settled {
			t.Errorf("%s 的报价 %s 与结算价 %s 不一致", name, value, settled)
		}
	}
	// Price 始终是 prompt 那一项，保留给只关心输入价的调用方。
	if got[0].Price != want["prompt"] {
		t.Errorf("Price = %s，期望 prompt 的 %s", got[0].Price, want["prompt"])
	}
}

// 每个维度按各自 1000 个 token 求值：规则可以引用用量，用 prompt 的用量
// 求 completion 会得到另一个价。
func TestOffersEvaluateEachDimensionSeparately(t *testing.T) {
	db := testDB(t)
	source := New(db)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	channel := seedChannel(t, db, owner)
	model := unique("offers-per-dimension")
	at := time.Now().UTC().Add(time.Minute)

	// c > 0 时 completion 更贵。若整条规则只按「1000 个 prompt」求一次，
	// completion 会取到 0.000006——比结算低。
	createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000002, "completion": c > 0 ? 0.000008 : 0.000006}`,
	})

	offers := NewOffers(db, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), at, []Candidate{
		{Model: model, ChannelID: channel, OwnerUserID: owner},
	})
	if err != nil {
		t.Fatalf("Prices 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("报价条数 = %d，期望 1：%+v", len(got), got)
	}
	if want := money.MustParse("0.00800000"); got[0].Prices["completion"] != want {
		t.Errorf("completion 报价 = %s，期望 %s（按 1000 个 completion 求值）",
			got[0].Prices["completion"], want)
	}
	settled := settlementDimensionPrice(t, source, model, channel, owner, at, "completion")
	if got[0].Prices["completion"] != settled {
		t.Errorf("completion 报价 %s 与结算价 %s 不一致", got[0].Prices["completion"], settled)
	}
}

// 规则没写的维度不出现在报价里：结算会拒绝这种用量，报价不能替它补 0。
func TestOffersOnlyReportDeclaredDimensions(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	channel := seedChannel(t, db, owner)
	model := unique("offers-declared")
	at := time.Now().UTC().Add(time.Minute)

	createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000002, "completion": 0.000006}`,
	})

	offers := NewOffers(db, "Asia/Shanghai", nil)
	got, err := offers.Prices(context.Background(), at, []Candidate{
		{Model: model, ChannelID: channel, OwnerUserID: owner},
	})
	if err != nil {
		t.Fatalf("Prices 出错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("报价条数 = %d，期望 1：%+v", len(got), got)
	}
	if len(got[0].Prices) != 2 {
		t.Errorf("维度 = %+v，期望只有 prompt 与 completion", got[0].Prices)
	}
	for _, absent := range []string{"cache_read", "cache_write", "reasoning"} {
		if _, ok := got[0].Prices[absent]; ok {
			t.Errorf("规则没写 %s，报价里却有它：%+v", absent, got[0].Prices)
		}
	}
}

// settlementDimensionPrice 用结算那一条路径算 1000 个某一维度 token 的价。
//
// Breakdown 的字段按测试自己的 switch 取，不复用 Offers 的求和：两边共用
// 同一个「哪一项落在哪个字段」的映射，就会一起错。
func settlementDimensionPrice(t *testing.T, source *Source, model string, channelID, ownerID int64, at time.Time, name string) money.Amount {
	t.Helper()

	rule, err := source.Match(context.Background(), singleCandidate(model, channelID, ownerID, at))
	if err != nil {
		t.Fatalf("结算取规则失败: %v", err)
	}
	program, err := expr.Compile(rule.Source)
	if err != nil {
		t.Fatalf("编译结算规则失败: %v", err)
	}
	var slot billing.TokenSlot
	found := false
	for _, candidate := range billing.TokenSlots {
		if candidate.Name == name {
			slot = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("未知的 token 维度 %q", name)
	}
	usage := billing.Usage{}
	slot.SetCount(&usage, offerTokenCount)
	result, err := program.Eval(expr.Env{At: at, Region: "Asia/Shanghai", Usage: usage})
	if err != nil {
		t.Fatalf("求结算价失败: %v", err)
	}
	breakdown := billing.ComputeWithCharges(usage, result.Prices, result.Charges)
	switch name {
	case "prompt":
		return breakdown.Prompt
	case "completion":
		return breakdown.Completion
	case "cache_read":
		return breakdown.CacheRead
	case "cache_write":
		return breakdown.CacheWrite
	case "reasoning":
		return breakdown.Reasoning
	default:
		t.Fatalf("维度 %q 没有对应的 Breakdown 字段", name)
		return money.Zero
	}
}
