package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/testdb"
)

// TestMain 给本包一个独立的测试库（见 internal/testdb）。
//
// 只有报价那两条用例连库：它们要跑真的 pricing.Offers，才能证明「一条规则
// 取不出价」这一侧的处置真的让清单返回 200。其余用例只用替身。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// testDB 返回本包独立测试库的连接。
//
// 没有 LARKAX_DB_DSN 时跳过而不是失败。**注意「跳过」与「通过」在输出里
// 长得一样**，所以 CI 里有一道专门确认它真的跑了的门。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// 每个用例用独立的标识，避免同一个库上跑第二次时撞上上一次留下的行。
var (
	seqMu   sync.Mutex
	seqN    int64
	seqBase = time.Now().UnixNano() % 1_000_000_000
)

func uniqueAPIName(prefix string) string {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqN++
	return fmt.Sprintf("%s-%d-%d", prefix, seqBase, seqN)
}

// seedRuleUser 建一个用户，供商家作用域的规则挂靠（外键要求它存在）。
func seedRuleUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (username) VALUES ($1) RETURNING id`, uniqueAPIName("api-user"),
	).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// insertOfferRule 直接往库里插一条计价规则，返回规则 id。
//
// 绕过 pricing 的写入路径：Create 会先编译表达式，而这里要的正是**编译
// 不过**的那一行——历史数据、直接写库、编译器收紧之后的旧表达式都属于
// 这一类（见 #938）。`effective_from` 显式写成很早的时刻，免得规则插入
// 与请求之间的时钟差把「规则没生效」混进「规则取不出价」。
func insertOfferRule(t *testing.T, ctx context.Context, db *sql.DB, scope string, owner any, model, expression string) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO pricing_rules
			(scope, owner_user_id, name, model_patterns, expr, priority, effective_from)
		VALUES ($1, $2, $3, to_jsonb(ARRAY[$4]::text[]), $5, 0, '1970-01-01T00:00:00Z')
		RETURNING id`,
		scope, owner, uniqueAPIName("api-rule"), model, expression,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插计价规则失败: %v", err)
	}
	return id
}

// 一条规则取不出价，不该让整份模型清单变成 500。
//
// 这条用例走的是真的 pricing.Offers（供给与目录仍是替身）：库里那行商家
// 规则编译不过，平台兜底规则正常，于是这个模型有一条报价、而两条候选里
// 只有平台那一条出现在结果里。响应码必须是 200——这正是 #938 要钉住的
// 对外行为，坏规则只该让**它自己**那几个候选没有报价。
func TestModelsOffersSkipBrokenRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	owner := seedRuleUser(t, db)
	model := uniqueAPIName("models-broken-rule")

	insertOfferRule(t, ctx, db, "platform", nil, model, `{"prompt": 0.000002}`)
	insertOfferRule(t, ctx, db, "merchant", owner, model, `{"prompt": notAThing + }`)

	supply := &fakeSupply{supplies: []channels.Supply{
		{Model: model, ChannelID: 1},
		{Model: model, ChannelID: 2, OwnerUserID: owner, Merchant: "acme"},
	}}
	cat := &fakeModelCatalog{models: []catalog.Model{{CanonicalName: model}}}
	h, _ := newModelsHandler(supply, cat)
	h.Offers = pricing.NewOffers(db, "Asia/Shanghai", nil)

	rec := get(h, PathModels)
	if rec.Code != http.StatusOK {
		t.Fatalf("一条规则取不出价就让清单变成 %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}

	var body modelsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body)
	}
	if len(body.Data) != 1 || body.Data[0].ID != model {
		t.Fatalf("清单 = %+v，期望只有 %q", body.Data, model)
	}
	offers := body.Data[0].Offers
	if len(offers) != 1 {
		t.Fatalf("offers = %+v，期望只有平台那条报价", offers)
	}
	if offers[0].Price != "0.00200000" {
		t.Errorf("报价 = %s，期望 0.00200000", offers[0].Price)
	}
	if offers[0].Merchant != nil {
		t.Errorf("报价的 merchant = %q，期望 null（平台自营）", *offers[0].Merchant)
	}
}

// 全部候选都取不出价时，清单仍是 200，只是省略 offers 字段。
//
// 省略而不是 `[]`：空数组会被读成「问了，但没有人报价」，而实际发生的是
// 「这个模型还没有可用的价格」——与「没有适用规则」同一处置。
func TestModelsOffersOmittedWhenAllRulesBroken(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	owner := seedRuleUser(t, db)
	model := uniqueAPIName("models-all-broken")

	insertOfferRule(t, ctx, db, "merchant", owner, model, `{"prompt": notAThing + }`)

	supply := &fakeSupply{supplies: []channels.Supply{
		{Model: model, ChannelID: 1, OwnerUserID: owner, Merchant: "acme"},
		{Model: model, ChannelID: 2, OwnerUserID: owner, Merchant: "acme"},
	}}
	cat := &fakeModelCatalog{models: []catalog.Model{{CanonicalName: model}}}
	h, _ := newModelsHandler(supply, cat)
	h.Offers = pricing.NewOffers(db, "Asia/Shanghai", nil)

	rec := get(h, PathModels)
	if rec.Code != http.StatusOK {
		t.Fatalf("全部候选都取不出价时清单变成 %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}

	var raw struct {
		Data []struct {
			ID     string          `json:"id"`
			Offers json.RawMessage `json:"offers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body)
	}
	if len(raw.Data) != 1 || raw.Data[0].ID != model {
		t.Fatalf("清单 = %s，期望只有 %q", rec.Body, model)
	}
	if offers := string(raw.Data[0].Offers); offers != "" {
		t.Errorf("offers = %s，期望整个字段省略", offers)
	}
}
