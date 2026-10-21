package consoleapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/testdb"
)

// TestMain 给本包一个独立的测试库（见 internal/testdb）。
//
// 只有模型页那一条用例连库：它要跑真的 pricing.References，才能证明
// 「一条平台规则取不出价」不会让整页清单变成错误。其余用例只用替身。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// referenceTestDB 返回本包独立测试库的连接。
//
// 没有 LARKAX_DB_DSN 时跳过而不是失败。**注意「跳过」与「通过」在输出里
// 长得一样**，所以 CI 里有一道专门确认它真的跑了的门。
func referenceTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// 每个用例用独立的模型名，避免同一个库上跑第二次时撞上上一次留下的规则。
var (
	referenceSeqMu   sync.Mutex
	referenceSeqN    int64
	referenceSeqBase = time.Now().UnixNano() % 1_000_000_000
)

func uniqueReferenceName(prefix string) string {
	referenceSeqMu.Lock()
	defer referenceSeqMu.Unlock()
	referenceSeqN++
	return fmt.Sprintf("%s-%d-%d", prefix, referenceSeqBase, referenceSeqN)
}

// insertPlatformRule 直接往库里插一条平台计价规则，返回规则 id。
//
// 绕过 pricing 的写入路径：Create 会先编译表达式，而这里要的正是**编译
// 不过**的那一行——历史数据、直接写库、编译器收紧之后的旧表达式都属于
// 这一类（见 #952）。
func insertPlatformRule(t *testing.T, ctx context.Context, db *sql.DB, model, expr string) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO pricing_rules
			(scope, name, model_patterns, expr, priority, effective_from)
		VALUES ('platform', $1, to_jsonb(ARRAY[$2]::text[]), $3, 0, '1970-01-01T00:00:00Z')
		RETURNING id`,
		uniqueReferenceName("console-ref-rule"), model, expr,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插平台计价规则失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM pricing_rules WHERE id = $1`, id)
	})
	return id
}

// 一条平台规则取不出价，不该让整页模型清单变成错误。
//
// 这条用例走的是真的 pricing.References（模型目录、供给与报价仍是替身）：
// 坏规则那个模型的 pricing 必须是 null，正常模型照常给出每百万 token 的
// 价格，响应码必须是 200。这正是 #952 要钉住的对外行为。
func TestModelsPageSurvivesBrokenReferenceRule(t *testing.T) {
	db := referenceTestDB(t)
	ctx := context.Background()
	good := uniqueReferenceName("console-ref-good")
	broken := uniqueReferenceName("console-ref-broken")

	insertPlatformRule(t, ctx, db, good, `{"prompt": 0.000002}`)
	insertPlatformRule(t, ctx, db, broken, `{"prompt": notAThing + }`)

	handler := newAPI(Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		KDF:      testKDF,
		SiteName: "larkax",
		Currency: "CNY",
		Now:      func() time.Time { return testNow },
	}, deps{
		sessions: newFakeSessions(),
		accounts: newFakeAccounts(),
		keys:     newFakeKeys(),
		usage:    &fakeUsage{},
		models:   &fakeModels{names: []string{good, broken}},
		supplies: &fakeSupplies{supplies: []channels.Supply{
			{Model: good, ChannelID: 1},
			{Model: broken, ChannelID: 2},
		}},
		catalog: &fakeCatalog{models: []catalog.Model{
			{CanonicalName: good},
			{CanonicalName: broken},
		}},
		prices:        pricing.NewReferences(db, "Asia/Shanghai", nil),
		offers:        &fakeOffers{base: money.MustParse("0.00200000")},
		merchantAdmin: stubAdmin(new(int), new(int64), nil),
		adminAdmin:    stubAdmin(new(int), new(int64), nil),
		spent: func(context.Context, []int64) (map[int64]money.Amount, error) {
			return map[int64]money.Amount{}, nil
		},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, PathPrefix+"models", nil)
	req.RemoteAddr = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("一条规则取不出价就让模型页变成 %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}

	var body struct {
		Items []struct {
			Name    string          `json:"name"`
			Pricing json.RawMessage `json:"pricing"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body)
	}
	byName := make(map[string]json.RawMessage, len(body.Items))
	for _, item := range body.Items {
		byName[item.Name] = item.Pricing
	}
	if pricing := byName[broken]; string(pricing) != "null" {
		t.Errorf("%s 的 pricing = %s，期望 null", broken, pricing)
	}
	var clean struct {
		InputPerMTok string `json:"input_per_mtok"`
	}
	if err := json.Unmarshal(byName[good], &clean); err != nil {
		t.Fatalf("%s 的 pricing 不是对象: %v", good, err)
	}
	if clean.InputPerMTok != "2.00000000" {
		t.Errorf("%s 的 input_per_mtok = %s，期望 2.00000000", good, clean.InputPerMTok)
	}
}
