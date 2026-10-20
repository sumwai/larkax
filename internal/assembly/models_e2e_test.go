package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 模型清单走一次生产装配路径（assembly.Build）与真库。
//
// api 包的用例证明的是「拿到供给清单之后怎么输出」，供给判据本身由
// internal/channels 的集成用例钉着；这一条把两段接起来，断言的正是 issue
// 的完成判据：启用的渠道在供的模型出现，渠道停用或删除后消失，只有目录行
// 而没有渠道的模型不出现，owned_by 取目录里的 vendor。
func TestModelsListFollowsSuppliedChannelsEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	supplied := unique("e2e-model-supplied")
	disabled := unique("e2e-model-disabled")
	removed := unique("e2e-model-deleted")
	catalogOnly := unique("e2e-model-catalog-only")

	insertCatalogModel(t, db, supplied, "acme")
	insertCatalogModel(t, db, disabled, "")
	insertCatalogModel(t, db, removed, "")
	insertCatalogModel(t, db, catalogOnly, "")

	owner := seedUser(t, db, true, "0")
	seedChannel(t, db, sealer, owner, "https://upstream.example.com/v1", supplied)
	disabledID := seedChannel(t, db, sealer, owner, "https://upstream.example.com/v1", disabled)
	removedID := seedChannel(t, db, sealer, owner, "https://upstream.example.com/v1", removed)

	if _, err := db.ExecContext(ctx, `UPDATE channels SET status = 'disabled' WHERE id = $1`, disabledID); err != nil {
		t.Fatalf("停用渠道失败: %v", err)
	}
	// status 留在 enabled，让 deleted_at 成为唯一挡得住它的东西。
	if _, err := db.ExecContext(ctx, `UPDATE channels SET deleted_at = now() WHERE id = $1`, removedID); err != nil {
		t.Fatalf("删除渠道失败: %v", err)
	}

	user := seedUser(t, db, false, "10")
	key := issueKey(t, db, user)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := getJSON(ctx, t, public.URL+"/v1/models", key)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("响应不是 JSON: %v（%s）", err, body)
	}
	if list.Object != "list" {
		t.Errorf("object = %q，期望 list", list.Object)
	}

	got := make(map[string]string, len(list.Data))
	for _, item := range list.Data {
		if item.Object != "model" {
			t.Errorf("%q 的 object = %q，期望 model", item.ID, item.Object)
		}
		if item.Created != 0 {
			t.Errorf("%q 的 created = %d，期望 0", item.ID, item.Created)
		}
		got[item.ID] = item.OwnedBy
	}
	// 同一个库上还有别的用例留下的渠道，所以只断言这几个名字在不在，
	// 不断言整个清单恰好等于什么。
	if vendor, ok := got[supplied]; !ok {
		t.Errorf("%q 有启用的渠道在供，却不在清单里: %v", supplied, got)
	} else if vendor != "acme" {
		t.Errorf("%q 的 owned_by = %q，期望目录里的 vendor acme", supplied, vendor)
	}
	for _, absent := range []string{disabled, removed, catalogOnly} {
		if _, ok := got[absent]; ok {
			t.Errorf("%q 不该出现在清单里: %v", absent, got)
		}
	}

	// 单条路径：可用的一条回一个完整对象；停用渠道的那条回 404。
	status, body = getJSON(ctx, t, public.URL+"/v1/models/"+supplied, key)
	if status != http.StatusOK {
		t.Fatalf("单条状态码 = %d，期望 200。响应体：%s", status, body)
	}
	var one struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatalf("单条响应不是 JSON: %v（%s）", err, body)
	}
	if one.ID != supplied || one.Object != "model" || one.OwnedBy != "acme" {
		t.Errorf("单条响应 = %+v，期望 id=%q、object=model、owned_by=acme", one, supplied)
	}

	status, body = getJSON(ctx, t, public.URL+"/v1/models/"+disabled, key)
	if status != http.StatusNotFound {
		t.Fatalf("不可用模型的状态码 = %d，期望 404。响应体：%s", status, body)
	}
	var notFound struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &notFound); err != nil {
		t.Fatalf("404 响应不是 JSON: %v（%s）", err, body)
	}
	if notFound.Error.Code == nil || *notFound.Error.Code != "model_not_found" {
		t.Errorf("404 的 error.code = %v，期望 model_not_found", notFound.Error.Code)
	}

	// 不带凭证：401，且形状是 OpenAI 那一套（SDK 按 type / code 分支）。
	status, body = getJSON(ctx, t, public.URL+"/v1/models", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("未认证的状态码 = %d，期望 401。响应体：%s", status, body)
	}
	var unauthorized struct {
		Error struct {
			Type string  `json:"type"`
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &unauthorized); err != nil {
		t.Fatalf("401 响应不是 JSON: %v（%s）", err, body)
	}
	if unauthorized.Error.Type != "invalid_request_error" {
		t.Errorf("401 的 error.type = %q，期望 invalid_request_error", unauthorized.Error.Type)
	}
	if unauthorized.Error.Code == nil || *unauthorized.Error.Code != "invalid_api_key" {
		t.Errorf("401 的 error.code = %v，期望 invalid_api_key", unauthorized.Error.Code)
	}
}

// getJSON 打一次 GET，返回状态码与响应体。key 为空时不带凭证。
func getJSON(ctx context.Context, t *testing.T, url, key string) (int, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return resp.StatusCode, out
}

// insertCatalogModel 插一条目录行。vendor 为空串时写成 NULL——
// 与生产里「采集到的数据没有这一项」同一个形状。
func insertCatalogModel(t *testing.T, db *sql.DB, name, vendor string) {
	t.Helper()

	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO model_catalog (canonical_name, display_name, vendor)
		VALUES ($1, $1, NULLIF($2, ''))`, name, vendor); err != nil {
		t.Fatalf("插规范模型失败: %v", err)
	}
}
