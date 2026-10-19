package adminapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/testdb"
	"github.com/sumwai/larkax/internal/upstream"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 这一层测的是「接线」，不是映射
//
// 中间件与错误映射在 adminapi_test.go 里用替身测过了。这里要证明的是只有
// 真库与真 handler 才说得清的那些：令牌写进 settings 的那一列是什么、
// 撤销之后下一次请求是什么状态、每个资源从建到读回来是不是走通了同一条路径。
//
// 没有 LARKAX_DB_DSN 时跳过而不是失败：本机未必起着库。**注意「跳过」与
// 「通过」在输出里长得一样**，CI 里另有一道门禁确认这个包真的跑了
// （见 .github/actions/check-integration-tests）。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// 每个用例用独立的标识，避免彼此干扰。
var seq struct {
	sync.Mutex
	n int64
}

var base = time.Now().UnixNano() % 1_000_000_000

func nextID() int64 {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return base*1000 + seq.n
}

func uniqueName(prefix string) string { return fmt.Sprintf("%s-%d", prefix, nextID()) }

// newTestAPI 按生产那条装配路径建一个 handler（见 New），只把数据库换成
// 本包的测试库。
func newTestAPI(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	db := testDB(t)

	key := make([]byte, config.KeySize)
	for i := range key {
		key[i] = 0x42
	}
	sealer, err := credential.NewSealer([]config.MasterKey{{ID: "test", Key: key}})
	if err != nil {
		t.Fatalf("构造凭证封装器失败: %v", err)
	}
	registry, err := upstream.Registry()
	if err != nil {
		t.Fatalf("构造渠道类型注册表失败: %v", err)
	}

	return New(Config{DB: db, Sealer: sealer, Registry: registry}), db
}

// resetToken 把令牌那一行写回迁移里的默认值（空串 = 从没配置过）。
//
// 包内各用例共用一个库，直接写这一行比「按执行顺序互相依赖」干净：每条用例
// 都从同一个已知状态开始。
func resetToken(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`UPDATE settings SET value = '""'::jsonb WHERE key = $1`, settings.KeyAdminToken); err != nil {
		t.Fatalf("重置令牌那一行失败: %v", err)
	}
}

// issueToken 生成一枚令牌并把它的明文交给调用方。
func issueToken(t *testing.T, db *sql.DB) string {
	t.Helper()
	token, err := NewTokenStore(db).Create(t.Context())
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	return token
}

// call 发一次管理面请求。token 为空时不带 Authorization。
func call(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("编码请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decode 把响应体解进 target，失败时把正文带上。
func decode(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("响应体不是 JSON: %v\n%s", err, rec.Body)
	}
}

// TestTokenLifecycle 钉住整套判据：未配置 503、正确 200、缺失/错误 401、
// 重新生成让旧的失效、撤销之后同一枚 401，以及**明文不进库**。
func TestTokenLifecycle(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	store := NewTokenStore(db)

	// 一、从没配置过：匿名请求也是 503，而不是「给了令牌就能进」的 401。
	rec := call(t, h, http.MethodGet, PathPrefix+"channels", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置令牌时状态码 = %d，期望 503；正文：%s", rec.Code, rec.Body)
	}

	// 二、生成之后：明文只出现在返回值里，库里是它的 SHA-256。
	token := issueToken(t, db)
	var stored string
	if err := db.QueryRowContext(t.Context(),
		`SELECT value #>> '{}' FROM settings WHERE key = $1`, settings.KeyAdminToken).Scan(&stored); err != nil {
		t.Fatalf("读令牌那一行失败: %v", err)
	}
	if len(stored) != hashHexLen {
		t.Errorf("库里那一列是 %d 个字符，期望 %d 位十六进制：%q", len(stored), hashHexLen, stored)
	}
	if _, err := hex.DecodeString(stored); err != nil {
		t.Errorf("库里那一列不是十六进制：%v", err)
	}
	sum := sha256.Sum256([]byte(token))
	if stored != hex.EncodeToString(sum[:]) {
		t.Errorf("库里那一列不是这枚令牌的 SHA-256")
	}
	if stored == token {
		t.Errorf("库里存了明文令牌")
	}

	// 三、正确令牌放行；缺的与错的都是 401，且正文逐字相同。
	if rec := call(t, h, http.MethodGet, PathPrefix+"channels", token, nil); rec.Code != http.StatusOK {
		t.Fatalf("正确令牌的状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
	noHeader := call(t, h, http.MethodGet, PathPrefix+"channels", "", nil)
	wrong := call(t, h, http.MethodGet, PathPrefix+"channels", "wrong-token", nil)
	if noHeader.Code != http.StatusUnauthorized || wrong.Code != http.StatusUnauthorized {
		t.Errorf("缺令牌 = %d、错令牌 = %d，都期望 401", noHeader.Code, wrong.Code)
	}
	if noHeader.Body.String() != wrong.Body.String() {
		t.Errorf("缺令牌与错令牌的正文不一样：\n%s\n%s", noHeader.Body, wrong.Body)
	}

	// 四、再生成一枚：旧的立刻失效，新的立刻能用。
	second := issueToken(t, db)
	if rec := call(t, h, http.MethodGet, PathPrefix+"channels", token, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("旧令牌的状态码 = %d，期望 401（新令牌生成后旧令牌应立刻失效）", rec.Code)
	}
	if rec := call(t, h, http.MethodGet, PathPrefix+"channels", second, nil); rec.Code != http.StatusOK {
		t.Errorf("新令牌的状态码 = %d，期望 200", rec.Code)
	}

	// 五、撤销：同一枚令牌回 401，而不是 503——「换一枚」与「先去创建一枚」
	// 是两种不同的处置，撤销属于前者。库里那一刻是全零的哨兵，所以随便哪一枚
	// 都进不去，且与「给错了」逐字相同。
	if err := store.Revoke(t.Context()); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	revoked := call(t, h, http.MethodGet, PathPrefix+"channels", second, nil)
	if revoked.Code != http.StatusUnauthorized {
		t.Errorf("撤销之后同一令牌的状态码 = %d，期望 401", revoked.Code)
	}
	for _, token := range []string{"", second, "any-other-token"} {
		rec := call(t, h, http.MethodGet, PathPrefix+"channels", token, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("撤销之后用 %q 的状态码 = %d，期望 401", token, rec.Code)
		}
		if rec.Body.String() != wrong.Body.String() {
			t.Errorf("撤销之后的正文与「令牌不对」不一样：\n%s\n%s", rec.Body, wrong.Body)
		}
	}

	resetToken(t, db)
}

// TestChannelEndpoints 走一遍渠道的建 → 列 → 改 → 读。
func TestChannelEndpoints(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	token := issueToken(t, db)

	name := uniqueName("channel")
	rec := call(t, h, http.MethodPost, PathPrefix+"channels", token, map[string]any{
		"name":       name,
		"type":       "openai",
		"base_url":   "https://api.example.com/v1",
		"credential": "sk-placeholder",
		"models":     []string{"model-x"},
		"priority":   1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建渠道的状态码 = %d，期望 201；正文：%s", rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &created)
	if created.ID <= 0 {
		t.Fatalf("创建返回的 id = %d", created.ID)
	}

	var list []channelView
	decode(t, call(t, h, http.MethodGet, PathPrefix+"channels", token, nil), &list)
	if !containsChannel(list, created.ID, name) {
		t.Fatalf("列表里看不到刚建的渠道 %d", created.ID)
	}

	rec = call(t, h, http.MethodPatch, fmt.Sprintf("%schannels/%d", PathPrefix, created.ID), token,
		map[string]any{"priority": 7})
	if rec.Code != http.StatusOK {
		t.Fatalf("改渠道的状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}

	var got channelView
	decode(t, call(t, h, http.MethodGet, fmt.Sprintf("%schannels/%d", PathPrefix, created.ID), token, nil), &got)
	if got.Priority != 7 {
		t.Errorf("读回来的 priority = %d，期望 7", got.Priority)
	}
	if got.CredentialHint == "" {
		t.Errorf("读回来的 credential_hint 是空的：提示应当来自写入路径")
	}

	// 按 id 找不到：404，而不是一个「成功但没有内容」的响应。
	if rec := call(t, h, http.MethodGet, PathPrefix+"channels/999999999", token, nil); rec.Code != http.StatusNotFound {
		t.Errorf("不存在的渠道状态码 = %d，期望 404", rec.Code)
	}
}

func containsChannel(list []channelView, id int64, name string) bool {
	for _, item := range list {
		if item.ID == id && item.Name == name {
			return true
		}
	}
	return false
}

// TestModelEndpoints 走一遍模型的建 → 列 → 改 → 读 → 删。
func TestModelEndpoints(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	token := issueToken(t, db)

	name := uniqueName("model")
	rec := call(t, h, http.MethodPost, PathPrefix+"models", token, map[string]any{
		"canonical_name": name,
		"display_name":   "示例模型",
		"context_limit":  200000,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建模型的状态码 = %d，期望 201；正文：%s", rec.Code, rec.Body)
	}

	var list []modelView
	decode(t, call(t, h, http.MethodGet, PathPrefix+"models", token, nil), &list)
	before, ok := findModel(list, name)
	if !ok {
		t.Fatalf("列表里看不到刚建的模型 %q", name)
	}
	if before.DisplayName != "示例模型" {
		t.Errorf("display_name = %q", before.DisplayName)
	}

	rec = call(t, h, http.MethodPatch, PathPrefix+"models/"+name, token,
		map[string]any{"display_name": "改过的模型"})
	if rec.Code != http.StatusOK {
		t.Fatalf("改模型的状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}

	decode(t, call(t, h, http.MethodGet, PathPrefix+"models", token, nil), &list)
	after, ok := findModel(list, name)
	if !ok {
		t.Fatalf("改完之后列表里找不到 %q", name)
	}
	if after.DisplayName != "改过的模型" {
		t.Errorf("改完之后 display_name = %q，期望「改过的模型」", after.DisplayName)
	}

	if rec := call(t, h, http.MethodDelete, PathPrefix+"models/"+name, token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("删模型的状态码 = %d，期望 204；正文：%s", rec.Code, rec.Body)
	}
	decode(t, call(t, h, http.MethodGet, PathPrefix+"models", token, nil), &list)
	if _, ok := findModel(list, name); ok {
		t.Errorf("删完之后列表里还有 %q", name)
	}
}

func findModel(list []modelView, name string) (modelView, bool) {
	for _, item := range list {
		if item.CanonicalName == name {
			return item, true
		}
	}
	return modelView{}, false
}

// TestAliasEndpoints 走一遍别名的建 → 列 → 改 → 读 → 删。
func TestAliasEndpoints(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	token := issueToken(t, db)

	canonical := uniqueName("model")
	rec := call(t, h, http.MethodPost, PathPrefix+"models", token, map[string]any{
		"canonical_name": canonical,
		"display_name":   "示例模型",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("准备模型失败: %d %s", rec.Code, rec.Body)
	}

	upstreamModel := uniqueName("upstream")
	rec = call(t, h, http.MethodPost, PathPrefix+"aliases", token, map[string]any{
		"upstream_model": upstreamModel,
		"canonical_name": canonical,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建别名的状态码 = %d，期望 201；正文：%s", rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &created)

	var list []aliasView
	decode(t, call(t, h, http.MethodGet, PathPrefix+"aliases", token, nil), &list)
	if !containsAlias(list, created.ID, upstreamModel) {
		t.Fatalf("平台级别名清单里看不到刚建的别名 %d", created.ID)
	}
	// `?channel_id=0` 是「平台级」的显式写法，与不带给的是同一段。
	var explicit []aliasView
	decode(t, call(t, h, http.MethodGet, PathPrefix+"aliases?channel_id=0", token, nil), &explicit)
	if !containsAlias(explicit, created.ID, upstreamModel) {
		t.Errorf("?channel_id=0 的清单里看不到别名 %d：0 被当成了非法或别的口径", created.ID)
	}

	renamed := upstreamModel + "-v2"
	rec = call(t, h, http.MethodPatch, fmt.Sprintf("%saliases/%d", PathPrefix, created.ID), token,
		map[string]any{"upstream_model": renamed})
	if rec.Code != http.StatusOK {
		t.Fatalf("改别名的状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}

	decode(t, call(t, h, http.MethodGet, PathPrefix+"aliases", token, nil), &list)
	if !containsAlias(list, created.ID, renamed) {
		t.Errorf("改完之后别名清单里没有 %q", renamed)
	}

	if rec := call(t, h, http.MethodDelete, fmt.Sprintf("%saliases/%d", PathPrefix, created.ID), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("删别名的状态码 = %d，期望 204；正文：%s", rec.Code, rec.Body)
	}
}

func containsAlias(list []aliasView, id int64, upstreamModel string) bool {
	for _, item := range list {
		if item.ID == id && item.UpstreamModel == upstreamModel {
			return true
		}
	}
	return false
}

// TestPricingRuleEndpoints 走一遍售价规则的建 → 列 → 改 → 读。
func TestPricingRuleEndpoints(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	token := issueToken(t, db)

	name := uniqueName("rule")
	rec := call(t, h, http.MethodPost, PathPrefix+"pricing/rules", token, map[string]any{
		"scope":          "platform",
		"name":           name,
		"model_patterns": []string{"model-x"},
		"expr":           `{"prompt": 0.000001}`,
		"priority":       1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建计价规则的状态码 = %d，期望 201；正文：%s", rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &created)

	var list []ruleView
	decode(t, call(t, h, http.MethodGet, PathPrefix+"pricing/rules", token, nil), &list)
	if !containsRule(list, created.ID, name) {
		t.Fatalf("规则清单里看不到刚建的规则 %d", created.ID)
	}

	rec = call(t, h, http.MethodPatch, fmt.Sprintf("%spricing/rules/%d", PathPrefix, created.ID), token,
		map[string]any{"priority": 9, "expr": `{"prompt": 0.000002}`})
	if rec.Code != http.StatusOK {
		t.Fatalf("改计价规则的状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}

	decode(t, call(t, h, http.MethodGet, PathPrefix+"pricing/rules", token, nil), &list)
	if !containsRule(list, created.ID, name) {
		t.Fatalf("改完之后规则清单里找不到 %d", created.ID)
	}
	for _, item := range list {
		if item.ID != created.ID {
			continue
		}
		if item.Priority != 9 {
			t.Errorf("priority = %d，期望 9", item.Priority)
		}
		if item.Expr != `{"prompt": 0.000002}` {
			t.Errorf("expr = %q，期望改过的那一份", item.Expr)
		}
	}
}

func containsRule(list []ruleView, id int64, name string) bool {
	for _, item := range list {
		if item.ID == id && item.Name == name {
			return true
		}
	}
	return false
}

// TestCostRuleEndpoints 走一遍成本规则的建 → 列 → 改 → 读。
func TestCostRuleEndpoints(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	token := issueToken(t, db)

	// 成本规则挂在渠道上（外键），所以先建一条渠道。
	rec := call(t, h, http.MethodPost, PathPrefix+"channels", token, map[string]any{
		"name":       uniqueName("channel"),
		"type":       "openai",
		"base_url":   "https://api.example.com/v1",
		"credential": "sk-placeholder",
		"models":     []string{"model-x"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("准备渠道失败: %d %s", rec.Code, rec.Body)
	}
	var channel struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &channel)

	rec = call(t, h, http.MethodPost, PathPrefix+"pricing/costs", token, map[string]any{
		"channel_id":     channel.ID,
		"model_patterns": []string{"model-x"},
		"expr":           `{"prompt": 0.0000005}`,
		"priority":       1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建成本规则的状态码 = %d，期望 201；正文：%s", rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &created)

	var list []costView
	decode(t, call(t, h, http.MethodGet, PathPrefix+"pricing/costs", token, nil), &list)
	if !containsCost(list, created.ID, channel.ID) {
		t.Fatalf("成本规则清单里看不到刚建的规则 %d", created.ID)
	}

	rec = call(t, h, http.MethodPatch, fmt.Sprintf("%spricing/costs/%d", PathPrefix, created.ID), token,
		map[string]any{"priority": 4})
	if rec.Code != http.StatusOK {
		t.Fatalf("改成本规则的状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}

	decode(t, call(t, h, http.MethodGet, PathPrefix+"pricing/costs", token, nil), &list)
	for _, item := range list {
		if item.ID == created.ID && item.Priority != 4 {
			t.Errorf("priority = %d，期望 4", item.Priority)
		}
	}
}

func containsCost(list []costView, id, channelID int64) bool {
	for _, item := range list {
		if item.ID == id && item.ChannelID == channelID {
			return true
		}
	}
	return false
}
