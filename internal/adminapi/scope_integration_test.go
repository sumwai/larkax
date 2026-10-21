package adminapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/upstream"
)

// 本文件测「装配可配」与「归属受限」这两件事。它们只有真库加真 handler
// 才说得清：前缀决定路由落不落得到，归属决定 WHERE 里多不多一个条件。

// testSurface 按测试需要的装配参数建一份 handler。
func testSurface(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	cfg.DB = testDB(t)
	if cfg.Sealer == nil {
		key := make([]byte, config.KeySize)
		for i := range key {
			key[i] = 0x42
		}
		sealer, err := credential.NewSealer([]config.MasterKey{{ID: "test", Key: key}})
		if err != nil {
			t.Fatalf("构造凭证封装器失败: %v", err)
		}
		cfg.Sealer = sealer
	}
	if cfg.Registry == nil {
		registry, err := upstream.Registry()
		if err != nil {
			t.Fatalf("构造渠道类型注册表失败: %v", err)
		}
		cfg.Registry = registry
	}
	return New(cfg)
}

// seedUser 建一个用户并返回 id。归属列有外键，所以渠道的主人必须先存在。
func seedUser(t *testing.T, db *sql.DB, prefix string) int64 {
	t.Helper()
	id := 900000 + nextID()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, prefix+itoa(id), prefix+itoa(id)+"@example.com")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// callAsOwner 在请求 context 里放一个归属，模拟控制台 merchant 组挂载之后的
// 那一条路径。token 照给（默认装配仍是 Bearer）。
func callAsOwner(t *testing.T, h http.Handler, ownerID int64, method, path, token string, body any) *httptest.ResponseRecorder {
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
	ctx := WithOwner(context.Background(), ownerID)
	req := httptest.NewRequestWithContext(ctx, method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func createChannelAs(t *testing.T, h http.Handler, ownerID int64, token, name string, bodyOwner int64) int64 {
	t.Helper()
	rec := callAsOwner(t, h, ownerID, http.MethodPost, PathPrefix+"channels", token, map[string]any{
		"owner_user_id": bodyOwner,
		"name":          name,
		"type":          "openai",
		"base_url":      "https://api.example.com/v1",
		"credential":    "sk-placeholder",
		"models":        []string{"model-x"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建渠道 %q 的状态码 = %d，期望 201；正文：%s", name, rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &created)
	return created.ID
}

func channelNames(list []channelView) map[int64]string {
	out := make(map[int64]string, len(list))
	for _, item := range list {
		out[item.ID] = item.Name
	}
	return out
}

// TestChannelOwnershipScope 钉住 merchant 这一侧的归属：列表按 owner 过滤，
// 越权的单条读与改都是 404，且 404 正文不透露那一行是否存在。
func TestChannelOwnershipScope(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	token := issueToken(t, db)

	owner := seedUser(t, db, "owner-")
	other := seedUser(t, db, "other-")

	// 建渠道时请求体里写别人的 owner_user_id：merchant 的归属由会话决定，
	// 请求体里的那个值必须被覆盖。
	ownName := uniqueName("own")
	ownID := createChannelAs(t, h, owner, token, ownName, other)
	var storedOwner sql.NullInt64
	if err := db.QueryRowContext(t.Context(),
		`SELECT owner_user_id FROM channels WHERE id = $1`, ownID).Scan(&storedOwner); err != nil {
		t.Fatalf("读渠道归属失败: %v", err)
	}
	if !storedOwner.Valid || storedOwner.Int64 != owner {
		t.Fatalf("merchant 建的渠道归属 = %v，期望会话用户 %d", storedOwner, owner)
	}

	// 另外两条：别人的渠道、平台自营渠道（owner 为 NULL）。
	otherID := createChannelAs(t, h, 0, token, uniqueName("other"), other)
	platformID := createChannelAs(t, h, 0, token, uniqueName("platform"), 0)

	// 列表：只有自己的。
	var list []channelView
	decode(t, callAsOwner(t, h, owner, http.MethodGet, PathPrefix+"channels", token, nil), &list)
	names := channelNames(list)
	if names[ownID] != ownName {
		t.Errorf("merchant 的列表里没有自己的渠道 %d", ownID)
	}
	if _, ok := names[otherID]; ok {
		t.Errorf("merchant 的列表里出现了别人的渠道 %d", otherID)
	}
	if _, ok := names[platformID]; ok {
		t.Errorf("merchant 的列表里出现了平台渠道 %d", platformID)
	}

	// 读：自己的 200，别人的与平台的都是 404，且正文逐字相同。
	ownPath := PathPrefix + "channels/" + itoa(ownID)
	if rec := callAsOwner(t, h, owner, http.MethodGet, ownPath, token, nil); rec.Code != http.StatusOK {
		t.Errorf("读自己的渠道状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
	var forbiddenBodies []string
	for _, id := range []int64{otherID, platformID, 999999999} {
		rec := callAsOwner(t, h, owner, http.MethodGet, PathPrefix+"channels/"+itoa(id), token, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("读渠道 %d 的状态码 = %d，期望 404；正文：%s", id, rec.Code, rec.Body)
		}
		forbiddenBodies = append(forbiddenBodies, rec.Body.String())
	}
	for _, body := range forbiddenBodies[1:] {
		if body != forbiddenBodies[0] {
			t.Errorf("「别人的渠道」与「不存在」的正文不一样：\n%s\n%s", forbiddenBodies[0], body)
		}
	}
	if !strings.Contains(forbiddenBodies[0], msgNotFound) {
		t.Errorf("404 正文里没有固定那一句 %q：%s", msgNotFound, forbiddenBodies[0])
	}
	if strings.Contains(forbiddenBodies[0], itoa(otherID)) {
		t.Errorf("404 正文里回显了别人的渠道 id：%s", forbiddenBodies[0])
	}

	// 改：别人的与平台的都是 404，而且库里确实没变。
	otherBefore := channelName(t, db, otherID)
	rec := callAsOwner(t, h, owner, http.MethodPatch, PathPrefix+"channels/"+itoa(otherID), token,
		map[string]any{"name": "hijacked"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("改别人的渠道状态码 = %d，期望 404；正文：%s", rec.Code, rec.Body)
	}
	if got := channelName(t, db, otherID); got != otherBefore {
		t.Errorf("越权修改落库了：名字从 %q 变成了 %q", otherBefore, got)
	}
	if rec := callAsOwner(t, h, owner, http.MethodPatch, PathPrefix+"channels/"+itoa(platformID), token,
		map[string]any{"name": "hijacked"}); rec.Code != http.StatusNotFound {
		t.Errorf("改平台渠道状态码 = %d，期望 404；正文：%s", rec.Code, rec.Body)
	}

	// 改自己的：成功，并落库。
	newName := uniqueName("renamed")
	if rec := callAsOwner(t, h, owner, http.MethodPatch, ownPath, token,
		map[string]any{"name": newName}); rec.Code != http.StatusOK {
		t.Errorf("改自己的渠道状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
	if got := channelName(t, db, ownID); got != newName {
		t.Errorf("改自己的渠道之后名字 = %q，期望 %q", got, newName)
	}

	// 状态切换也要经过归属检查：改自己的可以，改别人的仍然 404。
	if rec := callAsOwner(t, h, owner, http.MethodPatch, ownPath, token,
		map[string]any{"status": "disabled"}); rec.Code != http.StatusOK {
		t.Errorf("停用自己的渠道状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
	var status string
	if err := db.QueryRowContext(t.Context(), `SELECT status FROM channels WHERE id = $1`, ownID).Scan(&status); err != nil {
		t.Fatalf("读渠道状态失败: %v", err)
	}
	if status != "disabled" {
		t.Errorf("停用之后库里的 status = %q，期望 disabled", status)
	}
	if rec := callAsOwner(t, h, owner, http.MethodPatch, PathPrefix+"channels/"+itoa(otherID), token,
		map[string]any{"status": "disabled"}); rec.Code != http.StatusNotFound {
		t.Errorf("停用别人的渠道状态码 = %d，期望 404；正文：%s", rec.Code, rec.Body)
	}
}

func channelName(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var name string
	if err := db.QueryRowContext(t.Context(), `SELECT name FROM channels WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatalf("读渠道 %d 的名字失败: %v", id, err)
	}
	return name
}

// TestPrefixAuthorizerAndChannelOnly 钉住可配的三项：前缀、授权中间件、
// 按组注册。默认装配那一条另外判，防的是「加了配置项之后默认值变了」。
func TestPrefixAuthorizerAndChannelOnly(t *testing.T) {
	const prefix = "/api/v1/merchant/"
	calls := 0
	passThrough := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			next.ServeHTTP(w, r)
		})
	}
	h := testSurface(t, Config{Prefix: prefix, Authorize: passThrough, Groups: []string{GroupChannels}})

	// 用一条「路径参数不合法」的请求证明路由在前缀下存在：它在碰库之前就回 400。
	rec := call(t, h, http.MethodGet, prefix+"channels/not-a-number", "", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("新前缀下的渠道路由状态码 = %d，期望 400（路径参数）；正文：%s", rec.Code, rec.Body)
	}

	// 只注册渠道：模型、别名与计价都不在。
	absent := []string{prefix + "models", prefix + "aliases", prefix + "pricing/rules"}
	for _, path := range absent {
		if rec := call(t, h, http.MethodGet, path, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s 的状态码 = %d，期望 404（这一组不该有它）", path, rec.Code)
		}
	}

	// 旧前缀在这份装配里不存在。
	if rec := call(t, h, http.MethodGet, PathPrefix+"channels/not-a-number", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("指定前缀之后旧路径的状态码 = %d，期望 404", rec.Code)
	}

	// 一条存在的路径、len(absent) 条不存在的、一条旧前缀，都该经过授权中间件。
	if want := 2 + len(absent); calls != want {
		t.Errorf("授权中间件被调用 %d 次，期望 %d（它应当包住整组，包括没匹配上的路径）", calls, want)
	}
}

// TestDefaultAssemblyStillBearer 钉住默认装配：不传 Prefix / Authorize /
// Groups 时，仍然是 /admin/v1/ + Bearer，且资源是全量的。
func TestDefaultAssemblyStillBearer(t *testing.T) {
	h := testSurface(t, Config{})

	// 没带令牌：401（说明默认授权中间件在跑，且路由表在默认前缀下）。
	rec := call(t, h, http.MethodGet, PathPrefix+"channels", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("默认装配缺令牌的状态码 = %d，期望 401；正文：%s", rec.Code, rec.Body)
	}

	// 默认装配注册全部资源：没带令牌时模型路由也是 401，而不是 404。
	if rec := call(t, h, http.MethodGet, PathPrefix+"models", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("默认装配的模型路由状态码 = %d，期望 401", rec.Code)
	}
}

// TestMerchantPricingScope 钉住计价规则的归属面：merchant 组能看到并改
// 自己的规则；平台规则与别人的规则都是固定正文的 404；创建时作用域与归属
// 由会话强制。
func TestMerchantPricingScope(t *testing.T) {
	h, db := newTestAPI(t)
	resetToken(t, db)
	token := issueToken(t, db)

	owner := seedUser(t, db, "pricing-owner-")
	other := seedUser(t, db, "pricing-other-")

	expr := `{"prompt": 0.1}`
	mk := func(scope string, ownerUserID int64) int64 {
		t.Helper()
		body := map[string]any{
			"scope": scope, "owner_user_id": ownerUserID,
			"name": uniqueName("rule"), "expr": expr,
		}
		rec := call(t, h, http.MethodPost, PathPrefix+"pricing/rules", token, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("建规则状态码 = %d，期望 201；正文：%s", rec.Code, rec.Body)
		}
		var created struct {
			ID int64 `json:"id"`
		}
		decode(t, rec, &created)
		return created.ID
	}
	ownID := mk("merchant", owner)
	otherID := mk("merchant", other)
	platformID := mk("platform", 0)

	// 商家建的规则：作用域与归属被强制成自己的。
	rec := callAsOwner(t, h, owner, http.MethodPost, PathPrefix+"pricing/rules", token, map[string]any{
		"scope": "platform", "owner_user_id": other,
		"name": uniqueName("forced"), "expr": expr,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("商家建规则状态码 = %d，期望 201；正文：%s", rec.Code, rec.Body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	decode(t, rec, &created)
	var scope string
	var storedOwner sql.NullInt64
	if err := db.QueryRowContext(t.Context(),
		`SELECT scope, owner_user_id FROM pricing_rules WHERE id = $1`, created.ID).
		Scan(&scope, &storedOwner); err != nil {
		t.Fatalf("读规则失败: %v", err)
	}
	if scope != "merchant" || !storedOwner.Valid || storedOwner.Int64 != owner {
		t.Errorf("商家建的规则落库为 scope=%q owner=%v，期望 merchant/%d", scope, storedOwner, owner)
	}

	// 列表只有自己的。
	var list []ruleView
	decode(t, callAsOwner(t, h, owner, http.MethodGet, PathPrefix+"pricing/rules", token, nil), &list)
	seen := make(map[int64]bool, len(list))
	for _, item := range list {
		seen[item.ID] = true
	}
	if !seen[ownID] || !seen[created.ID] {
		t.Errorf("商家清单里缺少自己的规则（own=%v created=%v）", seen[ownID], seen[created.ID])
	}
	if seen[otherID] || seen[platformID] {
		t.Errorf("商家清单里出现了别家或平台规则（other=%v platform=%v）", seen[otherID], seen[platformID])
	}

	// 单条读：自己的 200；别人的、平台的、不存在的都是固定正文 404，
	// 且三份正文逐字相同——能分辨「不存在」与「不是你的」的那个 404 等于
	// 告诉调用方「这一行存在」。
	if rec := callAsOwner(t, h, owner, http.MethodGet, PathPrefix+"pricing/rules/"+itoa(ownID), token, nil); rec.Code != http.StatusOK {
		t.Errorf("读自己的规则状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
	notFoundIDs := []int64{otherID, platformID, 999999999}
	bodies := make([]string, 0, len(notFoundIDs))
	for _, id := range notFoundIDs {
		rec := callAsOwner(t, h, owner, http.MethodGet, PathPrefix+"pricing/rules/"+itoa(id), token, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("读规则 %d 状态码 = %d，期望 404；正文：%s", id, rec.Code, rec.Body)
		}
		bodies = append(bodies, rec.Body.String())
	}
	if !strings.Contains(bodies[0], msgNotFound) {
		t.Errorf("越权 404 的正文不是固定那句：%s", bodies[0])
	}
	for i, body := range bodies[1:] {
		if body != bodies[0] {
			t.Errorf("读规则 %d 的 404 正文与越权那份不同：%s ≠ %s",
				notFoundIDs[i+1], body, bodies[0])
		}
	}

	// 改：别人的、平台的、不存在的都是 404；自己的 200。
	updateBodies := make([]string, 0, len(notFoundIDs))
	for _, id := range notFoundIDs {
		rec := callAsOwner(t, h, owner, http.MethodPatch, PathPrefix+"pricing/rules/"+itoa(id), token,
			map[string]any{"priority": 99})
		if rec.Code != http.StatusNotFound {
			t.Errorf("改规则 %d 状态码 = %d，期望 404；正文：%s", id, rec.Code, rec.Body)
		}
		updateBodies = append(updateBodies, rec.Body.String())
	}
	for i, body := range updateBodies[1:] {
		if body != updateBodies[0] {
			t.Errorf("改规则 %d 的 404 正文与越权那份不同：%s ≠ %s",
				notFoundIDs[i+1], body, updateBodies[0])
		}
	}
	if rec := callAsOwner(t, h, owner, http.MethodPatch, PathPrefix+"pricing/rules/"+itoa(ownID), token,
		map[string]any{"priority": 7}); rec.Code != http.StatusOK {
		t.Errorf("改自己的规则状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}

	// admin（不带归属）能改平台规则。
	if rec := call(t, h, http.MethodPatch, PathPrefix+"pricing/rules/"+itoa(platformID), token,
		map[string]any{"priority": 5}); rec.Code != http.StatusOK {
		t.Errorf("admin 改平台规则状态码 = %d，期望 200；正文：%s", rec.Code, rec.Body)
	}
}

// TestGroupSelectionControlsRoutes 钉住按组注册：channels+pricing 有渠道与
// 计价规则（含单条读），没有模型、别名与成本价；未知组名在装配时 panic。
func TestGroupSelectionControlsRoutes(t *testing.T) {
	const prefix = "/api/v1/merchant/"
	passThrough := func(next http.Handler) http.Handler { return next }
	h := testSurface(t, Config{
		Prefix:    prefix,
		Authorize: passThrough,
		Groups:    []string{GroupChannels, GroupPricing},
	})

	// 两条路径参数不合法的请求：都在碰库之前回 400，证明路由存在。
	for _, path := range []string{prefix + "channels/x", prefix + "pricing/rules/x"} {
		if rec := call(t, h, http.MethodGet, path, "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s 的状态码 = %d，期望 400", path, rec.Code)
		}
	}
	// 成本价是渠道侧的成本口径，没有商家归属可过滤，merchant 组不注册它。
	for _, path := range []string{
		prefix + "models", prefix + "aliases", prefix + "pricing/costs",
		// 用户管理只挂在控制台的 admin 组：它的判据依赖会话里的操作者。
		prefix + "users",
	} {
		if rec := call(t, h, http.MethodGet, path, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s 的状态码 = %d，期望 404（这一组不含它）", path, rec.Code)
		}
	}

	func() {
		var recovered any
		defer func() { recovered = recover() }()
		New(Config{Groups: []string{"nonsense"}})
		if recovered == nil {
			t.Error("未知资源组没有 panic")
		}
	}()
}
