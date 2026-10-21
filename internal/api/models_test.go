package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pricing"
)

// fakeSupply 是公共模型清单的供给来源。
//
// 真实现是 channels.Source.Supplies，那条查询要真库才能跑；这里只回答
// 「哪些模型在哪条渠道上、归谁」，所以入口那一层的判据（谁该出现、谁不该）
// 可以脱离数据库钉住。供给判据本身由 internal/channels 的集成用例钉着。
type fakeSupply struct {
	supplies []channels.Supply
	err      error
	calls    int
}

func (f *fakeSupply) Supplies(context.Context) ([]channels.Supply, error) {
	f.calls++
	return f.supplies, f.err
}

// suppliesOf 给每个模型造一条平台自营渠道，供大多数不关心归属的用例用。
func suppliesOf(names ...string) []channels.Supply {
	out := make([]channels.Supply, 0, len(names))
	for i, name := range names {
		out = append(out, channels.Supply{Model: name, ChannelID: int64(i + 1)})
	}
	return out
}

// fakeOffers 是报价来源的替身。
//
// base 是默认价；byOwner 按归属覆盖（用来造「两个商家不同价」）；skip 里的
// 模型一律不报价（用来钉「没有报价的模型不列」）。
type fakeOffers struct {
	base    money.Amount
	byOwner map[int64]money.Amount
	skip    map[string]bool
	err     error
	calls   int
	seen    []pricing.Candidate
}

func (f *fakeOffers) Prices(_ context.Context, _ time.Time, candidates []pricing.Candidate) ([]pricing.CandidatePrice, error) {
	f.calls++
	f.seen = candidates
	if f.err != nil {
		return nil, f.err
	}
	out := []pricing.CandidatePrice{}
	for _, candidate := range candidates {
		if f.skip[candidate.Model] {
			continue
		}
		price := f.base
		if override, ok := f.byOwner[candidate.OwnerUserID]; ok {
			price = override
		}
		out = append(out, pricing.CandidatePrice{
			Model:       candidate.Model,
			ChannelID:   candidate.ChannelID,
			OwnerUserID: candidate.OwnerUserID,
			Merchant:    candidate.Merchant,
			Price:       price,
		})
	}
	return out, nil
}

// fakeModelCatalog 是目录那一侧的读点，只提供 vendor 与规范名。
type fakeModelCatalog struct {
	models []catalog.Model
	err    error
	calls  int
}

func (f *fakeModelCatalog) Models(context.Context) ([]catalog.Model, error) {
	f.calls++
	return f.models, f.err
}

// newModelsHandler 装一个带齐三份读点的 handler。
func newModelsHandler(supply *fakeSupply, cat *fakeModelCatalog) (*Handler, *fakeAuth) {
	h, authStub, _ := newTestHandler()
	h.Models = supply
	h.Catalog = cat
	h.Offers = &fakeOffers{base: money.MustParse("0.00350000")}
	return h, authStub
}

// get 发一个只读请求。
func get(h *Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// modelsBody 是清单响应里本文件要断言的部分。
type modelsBody struct {
	Object string      `json:"object"`
	Data   []modelJSON `json:"data"`
}

// 清单只列**有渠道在供**的模型，形状按 OpenAI 给。
//
// 目录里有的没在供的（gamma）不能出现：列出来调用方一用就是失败，比不列
// 更糟。owned_by 取 vendor，没填时回 larkax；created 固定 0。
func TestModelsListOnlyListsSuppliedModels(t *testing.T) {
	supply := &fakeSupply{supplies: suppliesOf("alpha", "beta")}
	cat := &fakeModelCatalog{models: []catalog.Model{
		{CanonicalName: "alpha", Vendor: "acme"},
		{CanonicalName: "beta"},
		// 目录里有、没有任何渠道在供：不该进清单。
		{CanonicalName: "gamma", Vendor: "acme"},
	}}
	h, _ := newModelsHandler(supply, cat)

	rec := get(h, PathModels)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	var body modelsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v（%s）", err, rec.Body)
	}
	if body.Object != "list" {
		t.Errorf("object = %q，期望 list", body.Object)
	}
	platformOffer := []offerJSON{{Price: "0.00350000"}}
	want := []modelJSON{
		{ID: "alpha", Object: "model", Created: 0, OwnedBy: "acme", Offers: platformOffer},
		// 目录里 vendor 是空串：回 larkax，而不是一个空的 owned_by。
		{ID: "beta", Object: "model", Created: 0, OwnedBy: "larkax", Offers: platformOffer},
	}
	if !reflect.DeepEqual(body.Data, want) {
		t.Errorf("清单 = %+v，期望 %+v", body.Data, want)
	}
	for _, item := range body.Data {
		if item.ID == "gamma" {
			t.Errorf("目录里有、没有渠道在供的模型出现在清单里: %+v", item)
		}
	}
}

// 目录空或无渠道在供时回 `"data": []`，不是 null。
//
// null 会让一部分 SDK 的解码直接失败，而「一个模型都不可用」恰恰是这条
// 路径最需要说清楚的时刻之一。
func TestModelsListEmptyDataIsArray(t *testing.T) {
	h, _ := newModelsHandler(&fakeSupply{}, &fakeModelCatalog{})
	rec := get(h, PathModels)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	var raw struct {
		Object string          `json:"object"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if string(raw.Data) != "[]" {
		t.Errorf("data = %s，期望 []", raw.Data)
	}
}

// 鉴权失败的两个方向与推理那几条路径一致：401 与 500，形状是 OpenAI 的。
//
// 401 那一支还要钉住「没有读供给」：没通过鉴权的请求不该碰库。
func TestModelsRequiresValidCredentials(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		status   int
		wantType string
		wantCode string
	}{
		{
			name:     "凭证问题回 401",
			err:      &auth.Error{Reason: auth.ReasonMissingCredential},
			status:   http.StatusUnauthorized,
			wantType: "invalid_request_error",
			wantCode: "invalid_api_key",
		},
		{
			name:     "查询失败回 500",
			err:      errors.New("数据库连不上"),
			status:   http.StatusInternalServerError,
			wantType: "api_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supply := &fakeSupply{supplies: suppliesOf("alpha")}
			h, authStub := newModelsHandler(supply, &fakeModelCatalog{})
			authStub.err = tc.err

			rec := get(h, PathModels)
			if rec.Code != tc.status {
				t.Fatalf("状态码 = %d，期望 %d（响应体 %s）", rec.Code, tc.status, rec.Body)
			}
			if supply.calls != 0 {
				t.Errorf("鉴权失败后仍然读了供给（%d 次）", supply.calls)
			}

			var body struct {
				Error struct {
					Type    string  `json:"type"`
					Message string  `json:"message"`
					Code    *string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是 JSON: %v（%s）", err, rec.Body)
			}
			if body.Error.Type != tc.wantType {
				t.Errorf("错误类型 = %q，期望 %q", body.Error.Type, tc.wantType)
			}
			if tc.wantCode == "" && body.Error.Code != nil {
				t.Errorf("不该带码，却带了 %q", *body.Error.Code)
			}
			if tc.wantCode != "" && (body.Error.Code == nil || *body.Error.Code != tc.wantCode) {
				t.Errorf("错误码 = %v，期望 %q", body.Error.Code, tc.wantCode)
			}
		})
	}
}

// 只读路径不接受 POST，而且 405 要带 Allow 头。
func TestModelsRejectsNonGet(t *testing.T) {
	h, _ := newModelsHandler(&fakeSupply{}, &fakeModelCatalog{})

	rec := post(h, PathModels, validBody)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405（响应体 %s）", rec.Code, rec.Body)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q，期望 %q", allow, http.MethodGet)
	}
}

// 装配漏了任何一份读点时回 501，而不是一份空清单。
//
// 空清单是一句「这把 key 一个模型都用不了」，而那时我们并不知道这件事。
func TestModelsNotWiredReturnsNotImplemented(t *testing.T) {
	full := &fakeSupply{supplies: suppliesOf("alpha")}
	cat := &fakeModelCatalog{models: []catalog.Model{{CanonicalName: "alpha"}}}

	cases := []struct {
		name  string
		build func() *Handler
	}{
		{"缺供给", func() *Handler {
			h, _ := newModelsHandler(full, cat)
			h.Models = nil
			return h
		}},
		{"缺目录", func() *Handler {
			h, _ := newModelsHandler(full, cat)
			h.Catalog = nil
			return h
		}},
		{"缺鉴权", func() *Handler {
			h, _ := newModelsHandler(full, cat)
			h.Auth = nil
			return h
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(tc.build(), PathModels)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("状态码 = %d，期望 501（响应体 %s）", rec.Code, rec.Body)
			}
		})
	}
}

// 单条路径：可用的模型回一条完整对象，路径里带 "/" 的规范名也认得。
func TestModelByIDReturnsAvailableModel(t *testing.T) {
	supply := &fakeSupply{supplies: suppliesOf("vendor/alpha", "beta")}
	cat := &fakeModelCatalog{models: []catalog.Model{
		{CanonicalName: "vendor/alpha", Vendor: "acme"},
		{CanonicalName: "beta"},
	}}
	h, _ := newModelsHandler(supply, cat)

	rec := get(h, PathModels+"/vendor/alpha")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	var got modelJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v（%s）", err, rec.Body)
	}
	want := modelJSON{ID: "vendor/alpha", Object: "model", Created: 0, OwnedBy: "acme",
		Offers: []offerJSON{{Price: "0.00350000"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("响应 = %+v，期望 %+v", got, want)
	}
}

// 单条路径对不可用的模型回 404，形状是 OpenAI 的 model_not_found。
//
// 三种「不可用」都要回同一个结果：目录里没有、目录里有但没有渠道在供、
// 以及空的 id（`/v1/models/` 走的是未知路径那一支，回 501）。
func TestModelByIDUnavailableReturnsNotFound(t *testing.T) {
	supply := &fakeSupply{supplies: suppliesOf("alpha")}
	cat := &fakeModelCatalog{models: []catalog.Model{
		{CanonicalName: "alpha", Vendor: "acme"},
		{CanonicalName: "beta", Vendor: "acme"},
	}}
	h, _ := newModelsHandler(supply, cat)

	for _, path := range []string{PathModels + "/beta", PathModels + "/nope"} {
		rec := get(h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 的状态码 = %d，期望 404（响应体 %s）", path, rec.Code, rec.Body)
			continue
		}
		var body struct {
			Error struct {
				Type    string  `json:"type"`
				Message string  `json:"message"`
				Code    *string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 的响应不是 JSON: %v（%s）", path, err, rec.Body)
		}
		if body.Error.Type != "invalid_request_error" {
			t.Errorf("%s 的错误类型 = %q，期望 invalid_request_error", path, body.Error.Type)
		}
		if body.Error.Code == nil || *body.Error.Code != "model_not_found" {
			t.Errorf("%s 的错误码 = %v，期望 model_not_found", path, body.Error.Code)
		}
	}
}

// 读库失败回 500：两类读点的失败都不能变成一份空清单或一个 404。
func TestModelsReadFailureIsInternalError(t *testing.T) {
	cases := []struct {
		name   string
		supply *fakeSupply
		cat    *fakeModelCatalog
	}{
		{"供给读不出来", &fakeSupply{err: errors.New("连不上库")}, &fakeModelCatalog{}},
		{"目录读不出来", &fakeSupply{supplies: suppliesOf("alpha")}, &fakeModelCatalog{err: errors.New("连不上库")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newModelsHandler(tc.supply, tc.cat)
			for _, path := range []string{PathModels, PathModels + "/alpha"} {
				rec := get(h, path)
				if rec.Code != http.StatusInternalServerError {
					t.Errorf("%s 的状态码 = %d，期望 500（响应体 %s）", path, rec.Code, rec.Body)
				}
			}
		})
	}
}

// 只读路径不经过限流与额度：它们不产生用量，也不该消耗任何人的额度。
func TestModelsDoNotTouchLimiterOrQuota(t *testing.T) {
	h, _ := newModelsHandler(&fakeSupply{supplies: suppliesOf("alpha")}, &fakeModelCatalog{
		models: []catalog.Model{{CanonicalName: "alpha"}},
	})
	limiter := &fakeLimiter{allowed: false}
	quota := &fakeQuota{err: errors.New("额度判定不该被调用")}
	h.RateLimiter = limiter
	h.Quota = quota

	for _, path := range []string{PathModels, PathModels + "/alpha"} {
		if rec := get(h, path); rec.Code != http.StatusOK {
			t.Fatalf("%s 的状态码 = %d，期望 200（响应体 %s）", path, rec.Code, rec.Body)
		}
	}
	if limiter.calls != 0 {
		t.Errorf("限流被调用 %d 次", limiter.calls)
	}
	if quota.calls != 0 {
		t.Errorf("额度判定被调用 %d 次", quota.calls)
	}
}

// 未知路径与 `{id}` 的切分：前缀之后整段就是 id，空 id 也算这条路径。
func TestModelIDFromPath(t *testing.T) {
	cases := []struct {
		path string
		id   string
		ok   bool
	}{
		{PathModels, "", false},
		// 前缀之后为空仍然算这条路径（id 是空串），由调用方回 404——
		// 见 modelIDFromPath 的注释。
		{PathModels + "/", "", true},
		{PathModels + "//", "/", true},
		{PathModels + "/alpha", "alpha", true},
		// 规范名里可能带 "/"，不按段数判。
		{PathModels + "/vendor/alpha", "vendor/alpha", true},
		// 前缀只是字面相同、不是一个路径段：不认。
		{PathModels + "x/alpha", "", false},
		{"/v1/model/alpha", "", false},
	}

	for _, tc := range cases {
		id, ok := modelIDFromPath(tc.path)
		if id != tc.id || ok != tc.ok {
			t.Errorf("modelIDFromPath(%q) = (%q, %v)，期望 (%q, %v)", tc.path, id, ok, tc.id, tc.ok)
		}
	}
}

// 尾斜杠落 404，而不是未知路径那一支的 501。
//
// `/v1/models/` 与 `/v1/models//` 是同一类输入（空 id 或一个以 "/" 开头的
// id），两条都该得到同一个故事：路径已经实现，没有这个模型。回 501 会说成
// 「这个部署没接模型清单」，而那是另一件事。
func TestModelsTrailingSlashIsNotFound(t *testing.T) {
	h, _ := newModelsHandler(&fakeSupply{supplies: suppliesOf("alpha")}, &fakeModelCatalog{
		models: []catalog.Model{{CanonicalName: "alpha"}},
	})

	for _, path := range []string{PathModels + "/", PathModels + "//"} {
		rec := get(h, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 的状态码 = %d，期望 404（响应体 %s）", path, rec.Code, rec.Body)
			continue
		}
		var body struct {
			Error struct {
				Code *string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 的响应不是 JSON: %v（%s）", path, err, rec.Body)
		}
		if body.Error.Code == nil || *body.Error.Code != "model_not_found" {
			t.Errorf("%s 的错误码 = %v，期望 model_not_found", path, body.Error.Code)
		}
	}
}

// 时间与版本头这些「每条路径都带」的东西，只读路径也有。
//
// 版本头是 docs/versioning.md 的承诺：用户报问题时第一个要确认的就是它。
func TestModelsCarryVersionHeader(t *testing.T) {
	h, _ := newModelsHandler(&fakeSupply{}, &fakeModelCatalog{})
	h.Version = "v0.1.0-test"
	rec := get(h, PathModels)
	if got := rec.Header().Get(HeaderVersion); got != "v0.1.0-test" {
		t.Errorf("%s = %q", HeaderVersion, got)
	}
}

// 同一个模型由两个商家供、价不同：offers 列出两条，按价格升序。
//
// 单位是每 1000 个输入 token（与 /v1/models 的约定一致）。归属来自渠道，
// 不是模型目录：同一个模型在不同渠道上可以是不同商家的货。
func TestModelsOffersListMerchantsByPrice(t *testing.T) {
	supply := &fakeSupply{supplies: []channels.Supply{
		{Model: "alpha", ChannelID: 1, OwnerUserID: 7, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2, OwnerUserID: 8, Merchant: "globex"},
	}}
	cat := &fakeModelCatalog{models: []catalog.Model{{CanonicalName: "alpha", Vendor: "acme"}}}
	h, _ := newModelsHandler(supply, cat)
	offers := &fakeOffers{
		base:    money.MustParse("0.00400000"),
		byOwner: map[int64]money.Amount{7: money.MustParse("0.00350000")},
	}
	h.Offers = offers

	rec := get(h, PathModels)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	var body modelsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("清单项数 = %d，期望 1（响应 %s）", len(body.Data), rec.Body)
	}
	acme, globex := "acme", "globex"
	want := []offerJSON{
		{Merchant: &acme, Price: "0.00350000"},
		{Merchant: &globex, Price: "0.00400000"},
	}
	if !reflect.DeepEqual(body.Data[0].Offers, want) {
		t.Errorf("offers = %+v，期望 %+v", body.Data[0].Offers, want)
	}

	// 一次报价查询覆盖全部候选，而不是每个模型问一次。
	if offers.calls != 1 {
		t.Errorf("报价查询调用了 %d 次，期望 1 次", offers.calls)
	}
	if len(offers.seen) != 2 {
		t.Errorf("报价候选数 = %d，期望 2（两个渠道各一条）", len(offers.seen))
	}
}

// 平台自营与商家并存：平台那条 merchant 是 null，不是空串。
func TestModelsOffersMarkPlatformAsNull(t *testing.T) {
	supply := &fakeSupply{supplies: []channels.Supply{
		{Model: "alpha", ChannelID: 1, OwnerUserID: 7, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2},
	}}
	cat := &fakeModelCatalog{models: []catalog.Model{{CanonicalName: "alpha"}}}
	h, _ := newModelsHandler(supply, cat)
	h.Offers = &fakeOffers{
		base:    money.MustParse("0.00400000"),
		byOwner: map[int64]money.Amount{7: money.MustParse("0.00350000")},
	}

	rec := get(h, PathModels)
	var body modelsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Data) != 1 || len(body.Data[0].Offers) != 2 {
		t.Fatalf("清单 = %s，期望一条模型、两条报价", rec.Body)
	}
	acme := "acme"
	want := []offerJSON{
		{Merchant: &acme, Price: "0.00350000"},
		{Price: "0.00400000"},
	}
	if !reflect.DeepEqual(body.Data[0].Offers, want) {
		t.Errorf("offers = %+v，期望 %+v", body.Data[0].Offers, want)
	}

	// JSON 里平台的 merchant 是字面量 null；omitempty 不能把它吞掉，
	// 因为 null 与「字段缺失」对客户端是两种不同的形状。
	var raw struct {
		Data []struct {
			Offers []map[string]json.RawMessage `json:"offers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	last := raw.Data[0].Offers[1]
	value, ok := last["merchant"]
	if !ok || string(value) != "null" {
		t.Errorf("平台报价的 merchant = %s（存在=%v），期望 null", value, ok)
	}
}

// 没有报价的模型不列；整份清单里不出现空 offers 数组。
func TestModelsDropsModelsWithoutOffers(t *testing.T) {
	supply := &fakeSupply{supplies: suppliesOf("alpha", "beta")}
	cat := &fakeModelCatalog{models: []catalog.Model{
		{CanonicalName: "alpha"},
		{CanonicalName: "beta"},
	}}
	h, _ := newModelsHandler(supply, cat)
	h.Offers = &fakeOffers{
		base: money.MustParse("0.00350000"),
		skip: map[string]bool{"beta": true},
	}

	rec := get(h, PathModels)
	var raw struct {
		Data []struct {
			ID     string          `json:"id"`
			Offers json.RawMessage `json:"offers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(raw.Data) != 1 || raw.Data[0].ID != "alpha" {
		t.Errorf("清单 = %s，期望只有 alpha", rec.Body)
	}
	if string(raw.Data[0].Offers) == "[]" || string(raw.Data[0].Offers) == "null" {
		t.Errorf("offers = %s，期望一个有内容的数组", raw.Data[0].Offers)
	}

	// 单条路径对「有供给却没报价」同样回 404：列表里没有它，单条就不该有。
	if rec := get(h, PathModels+"/beta"); rec.Code != http.StatusNotFound {
		t.Errorf("单条 beta 的状态码 = %d，期望 404（响应 %s）", rec.Code, rec.Body)
	}
}

// 同一个商家供两条渠道、同价只报一次；不同价则两条都在。
func TestModelsOffersDedupeByMerchantAndPrice(t *testing.T) {
	supply := &fakeSupply{supplies: []channels.Supply{
		{Model: "alpha", ChannelID: 1, OwnerUserID: 7, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2, OwnerUserID: 7, Merchant: "acme"},
		{Model: "alpha", ChannelID: 3, OwnerUserID: 8, Merchant: "globex"},
	}}
	cat := &fakeModelCatalog{models: []catalog.Model{{CanonicalName: "alpha"}}}
	h, _ := newModelsHandler(supply, cat)
	h.Offers = &fakeOffers{
		base:    money.MustParse("0.00350000"),
		byOwner: map[int64]money.Amount{7: money.MustParse("0.00350000"), 8: money.MustParse("0.00350000")},
	}

	rec := get(h, PathModels)
	var body modelsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("清单项数 = %d，期望 1", len(body.Data))
	}
	acme, globex := "acme", "globex"
	want := []offerJSON{
		{Merchant: &acme, Price: "0.00350000"},
		{Merchant: &globex, Price: "0.00350000"},
	}
	if !reflect.DeepEqual(body.Data[0].Offers, want) {
		t.Errorf("offers = %+v，期望同价的渠道折叠成一条、商家各一条", body.Data[0].Offers)
	}
}
