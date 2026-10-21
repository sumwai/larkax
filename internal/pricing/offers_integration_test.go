package pricing

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
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

// 一条规则取不出价，只跳过命中它的候选，其余候选照常报价。
//
// 这是 #938 的判据：某个商家配错一条规则，不该让所有调用方的模型清单都
// 变成 500。跳过要留一条日志（规则 id + 原因），否则外部只剩下「这个模型
// 没有报价」——那与「没有适用规则」长得一模一样。
func TestOffersSkipCandidatesWithBrokenRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ownerOK := seedUser(t, db)
	ownerBroken := seedUser(t, db)
	channelOK := seedChannel(t, db, ownerOK)
	channelBroken := seedChannel(t, db, ownerBroken)
	model := unique("offers-broken")
	at := time.Now().UTC().Add(time.Minute)

	createRule(t, NewAdmin(db), CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: ownerOK, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.000002}`,
	})
	// 绕过写入路径直接插一条编译不过的规则：Create 会先编译，所以这类行
	// 只能来自历史数据、直接写库，或编译器收紧之后的旧表达式。
	brokenID := insertRule(t, ctx, db, ruleSpec{
		scope: "merchant", ownerID: ownerBroken, patterns: []string{model},
		expr: `{"prompt": notAThing + }`,
	})

	logs := captureLogs(t)

	got, err := NewOffers(db, "Asia/Shanghai", nil).Prices(ctx, at, []Candidate{
		{Model: model, ChannelID: channelBroken, OwnerUserID: ownerBroken},
		{Model: model, ChannelID: channelOK, OwnerUserID: ownerOK},
	})
	if err != nil {
		t.Fatalf("一条规则取价失败就让整次取价失败: %v", err)
	}
	if len(got) != 1 || got[0].ChannelID != channelOK {
		t.Fatalf("报价 = %+v，期望只有渠道 %d 那一条", got, channelOK)
	}
	if want := money.MustParse("0.00200000"); got[0].Price != want {
		t.Errorf("正常渠道的报价 = %s，期望 %s", got[0].Price, want)
	}

	logged := logs.String()
	if !strings.Contains(logged, "rule_id="+strconv.FormatInt(brokenID, 10)) {
		t.Errorf("日志里没有规则 id %d：%s", brokenID, logged)
	}
	if !strings.Contains(logged, "规则取价失败") {
		t.Errorf("日志里没有取价失败的说明：%s", logged)
	}
	// 表达式全文可能很长，日志只该留错误的第一行。
	if strings.Contains(logged, "notAThing") {
		t.Errorf("日志里带上了表达式全文：%s", logged)
	}
}

// 全部候选都取价失败时返回空结果而不是错误：清单仍然列得出来，只是不带
// offers（见 internal/api 的 TestModelsOffersOmittedWhenAllRulesBroken）。
//
// 同一条规则命中多个候选只记一条日志：一条兜底规则写坏时，逐候选记会把
// 同一件事刷几十遍。
func TestOffersBrokenRulesYieldEmptyResult(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	ownerA := seedUser(t, db)
	ownerB := seedUser(t, db)
	channelA1 := seedChannel(t, db, ownerA)
	channelA2 := seedChannel(t, db, ownerA)
	channelB := seedChannel(t, db, ownerB)
	model := unique("offers-all-broken")
	at := time.Now().UTC().Add(time.Minute)

	ruleA := insertRule(t, ctx, db, ruleSpec{
		scope: "merchant", ownerID: ownerA, patterns: []string{model},
		expr: `{"prompt": notAThing + }`,
	})
	ruleB := insertRule(t, ctx, db, ruleSpec{
		scope: "merchant", ownerID: ownerB, patterns: []string{model},
		expr: `{"prompt": "x" + }`,
	})

	logs := captureLogs(t)

	got, err := NewOffers(db, "Asia/Shanghai", nil).Prices(ctx, at, []Candidate{
		{Model: model, ChannelID: channelA1, OwnerUserID: ownerA},
		{Model: model, ChannelID: channelA2, OwnerUserID: ownerA},
		{Model: model, ChannelID: channelB, OwnerUserID: ownerB},
	})
	if err != nil {
		t.Fatalf("全部候选都取价失败时返回了错误: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("报价 = %+v，期望非 nil 的空切片", got)
	}

	logged := logs.String()
	if n := strings.Count(logged, "规则取价失败"); n != 2 {
		t.Errorf("日志条数 = %d，期望 2（每条坏规则一条，命中它的候选不重复记）：%s", n, logged)
	}
	for _, id := range []int64{ruleA, ruleB} {
		if !strings.Contains(logged, "rule_id="+strconv.FormatInt(id, 10)) {
			t.Errorf("日志里没有规则 id %d：%s", id, logged)
		}
	}
	if strings.Contains(logged, "notAThing") {
		t.Errorf("日志里带上了表达式全文：%s", logged)
	}
}

// captureLogs 把这条用例期间打的日志收进缓冲区。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(observe.NewLoggerWithWriter(&buf, "debug", "text", false))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}
