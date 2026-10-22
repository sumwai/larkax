package consoleapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pricing"
)

// 单条模型：不在供与目录里没有必须是同一个 404——否则调用方能靠状态码
// 分辨某个名字在不在目录里。
func TestModelDetailNotFoundUnlessSupplied(t *testing.T) {
	f := newFixture(t)
	// hidden 在目录里，但没有任何渠道在供；absent 连目录都没有。
	f.catalog.models = []catalog.Model{{CanonicalName: "hidden"}}

	// 同一个 request id 才比得出「正文逐字相同」：错误体里带着它。
	headers := map[string]string{"x-request-id": "fixed-request-id"}
	hidden := f.requestAsWith(t, "127.0.0.1:8080", headers, http.MethodGet, PathPrefix+"models/hidden", "")
	absent := f.requestAsWith(t, "127.0.0.1:8080", headers, http.MethodGet, PathPrefix+"models/absent", "")

	if hidden.Code != http.StatusNotFound || absent.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d / %d，期望都是 404", hidden.Code, absent.Code)
	}
	if hidden.Body.String() != absent.Body.String() {
		t.Errorf("两种 404 的正文不同：\n在目录里但不在供: %s\n目录里也没有: %s",
			hidden.Body, absent.Body)
	}

	empty := f.request(t, http.MethodGet, PathPrefix+"models/", "")
	if empty.Code != http.StatusNotFound {
		t.Errorf("空名字的状态码 = %d，期望 404（响应 %s）", empty.Code, empty.Body)
	}
	// 状态码分不出「handler 判的 404」与「路由没挂上时的默认 404」：后者是
	// Go 自带的纯文本页。断言错误码，路由整片丢掉时这条才红。
	if got := errorCode(t, empty); got != codeNotFound {
		t.Errorf("空名字的错误码 = %q，期望 %q（响应 %s）", got, codeNotFound, empty.Body)
	}
}

// 供应商按价格升序；平台自营的 merchant 是 null；每条带整套维度价与
// 渠道条数。响应里不得出现上游模型名——那会把调用方引去绕过网关。
func TestModelDetailListsSuppliersByPrice(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{
		{Model: "alpha", ChannelID: 3, OwnerUserID: 12, Merchant: "beta"},
		{Model: "alpha", ChannelID: 1, OwnerUserID: 11, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2},
	}
	f.offers.base = mustAmount(t, "0.00300000")
	f.offers.byOwner = map[int64]money.Amount{
		11: mustAmount(t, "0.00200000"),
		12: mustAmount(t, "0.00400000"),
	}

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}
	// 只依赖目录与规则的响应可以公共缓存一分钟：翻模型页不必重复打三条查询。
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q，期望 public, max-age=60", got)
	}
	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	want := []supplierJSON{
		{Merchant: ptrString("acme"), Price: "0.00200000",
			Prices: map[string]string{"prompt": "0.00200000"}, ChannelCount: 1},
		{Price: "0.00300000", Prices: map[string]string{"prompt": "0.00300000"}, ChannelCount: 1},
		{Merchant: ptrString("beta"), Price: "0.00400000",
			Prices: map[string]string{"prompt": "0.00400000"}, ChannelCount: 1},
	}
	if !reflect.DeepEqual(body.Model.Suppliers, want) {
		t.Errorf("suppliers = %+v，期望 %+v", body.Model.Suppliers, want)
	}
	if body.Model.UnpricedChannels != 0 {
		t.Errorf("unpriced_channels = %d，期望 0", body.Model.UnpricedChannels)
	}
	if body.Currency != "CNY" || body.ReferenceAt == "" {
		t.Errorf("currency/reference_at = %q/%q，期望 CNY 与一个时刻", body.Currency, body.ReferenceAt)
	}

	// 公开响应里 model 与每条 supplier 的键集合都必须正好是约定的那些键。
	// 断言形状而不是 grep 某个旧字符串：未来任何人加一个指向上游的字段，
	// 这条都会红。
	var rawBody struct {
		Model map[string]json.RawMessage `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawBody); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	wantModelKeys := map[string]bool{
		"name": true, "display_name": true, "vendor": true, "family": true,
		"context_limit": true, "input_limit": true, "output_limit": true,
		"capabilities": true, "modalities_input": true, "modalities_output": true,
		"reasoning_options": true, "canonical_model_id": true,
		"knowledge_cutoff": true, "release_date": true, "updated_at": true,
		"suppliers": true, "unpriced_channels": true,
	}
	if len(rawBody.Model) != len(wantModelKeys) {
		t.Errorf("model 有 %d 个字段，期望正好 %d 个：%v",
			len(rawBody.Model), len(wantModelKeys), rawBody.Model)
	}
	for key := range wantModelKeys {
		if _, ok := rawBody.Model[key]; !ok {
			t.Errorf("model 缺字段 %q：%v", key, rawBody.Model)
		}
	}
	for key := range rawBody.Model {
		if !wantModelKeys[key] {
			t.Errorf("model 多出字段 %q：%v", key, rawBody.Model)
		}
	}

	var suppliers []map[string]json.RawMessage
	if err := json.Unmarshal(rawBody.Model["suppliers"], &suppliers); err != nil {
		t.Fatalf("suppliers 不是数组: %v", err)
	}
	wantKeys := map[string]bool{
		"merchant": true, "price": true, "prices": true, "channel_count": true,
	}
	for i, supplier := range suppliers {
		if len(supplier) != len(wantKeys) {
			t.Errorf("第 %d 条 supplier 有 %d 个字段，期望正好 %d 个：%v",
				i, len(supplier), len(wantKeys), supplier)
		}
		for key := range wantKeys {
			if _, ok := supplier[key]; !ok {
				t.Errorf("第 %d 条 supplier 缺字段 %q：%v", i, key, supplier)
			}
		}
		for key := range supplier {
			if !wantKeys[key] {
				t.Errorf("第 %d 条 supplier 多出字段 %q：%v", i, key, supplier)
			}
		}
	}
}

// 同一商家、同一整套维度价的多条渠道折成一行，channel_count 记条数；
// 同一商家、任一维度不同则是两行。
func TestModelDetailAggregatesSuppliersByMerchantAndPrices(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{
		{Model: "alpha", ChannelID: 1, OwnerUserID: 11, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2, OwnerUserID: 11, Merchant: "acme"},
		{Model: "alpha", ChannelID: 3, OwnerUserID: 11, Merchant: "acme"},
	}
	f.offers.byChannelPrices = map[int64]map[string]money.Amount{
		1: {
			"prompt":      mustAmount(t, "0.00300000"),
			"completion":  mustAmount(t, "0.00900000"),
			"cache_read":  mustAmount(t, "0.00060000"),
			"cache_write": mustAmount(t, "0.00360000"),
			"reasoning":   mustAmount(t, "0.01200000"),
		},
		2: {
			"prompt":      mustAmount(t, "0.00300000"),
			"completion":  mustAmount(t, "0.00900000"),
			"cache_read":  mustAmount(t, "0.00060000"),
			"cache_write": mustAmount(t, "0.00360000"),
			"reasoning":   mustAmount(t, "0.01200000"),
		},
		// 输入价相同、缓存读价不同：是另一套报价。
		3: {
			"prompt":     mustAmount(t, "0.00300000"),
			"cache_read": mustAmount(t, "0.00120000"),
		},
	}

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}
	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	acme := "acme"
	want := []supplierJSON{
		// prompt 同价、同商家时按整套价的签名排：只有 prompt + cache_read
		// 的那套排在五项俱全的前面（签名逐字比较的结果，与价格无关）。
		{Merchant: &acme, Price: "0.00300000", ChannelCount: 1, Prices: map[string]string{
			"prompt": "0.00300000", "cache_read": "0.00120000"}},
		{Merchant: &acme, Price: "0.00300000", ChannelCount: 2, Prices: map[string]string{
			"prompt": "0.00300000", "completion": "0.00900000", "cache_read": "0.00060000",
			"cache_write": "0.00360000", "reasoning": "0.01200000"}},
	}
	if !reflect.DeepEqual(body.Model.Suppliers, want) {
		t.Errorf("suppliers = %+v，期望 %+v", body.Model.Suppliers, want)
	}
}

// 同商家、整套价相同、规则有效期窗口不同时，两条渠道折成一行，报的是
// 渠道编号最小的那条渠道适用的规则：报价层按 (模型, 渠道) 返回，折行时
// 保留先插入的那条。窗口不属于折行的判据——它不影响价格，只影响哪条规则
// 在这次求值里适用。
func TestModelDetailKeepsLowestChannelRuleOnPriceTie(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	// 故意让编号大的渠道先出现在供给里：结果只该取决于渠道编号，与供给的
	// 先后无关。
	f.supplies.supplies = []channels.Supply{
		{Model: "alpha", ChannelID: 2, OwnerUserID: 11, Merchant: "acme"},
		{Model: "alpha", ChannelID: 1, OwnerUserID: 11, Merchant: "acme"},
	}
	price := map[string]money.Amount{"prompt": mustAmount(t, "0.00300000")}
	f.offers.byChannelPrices = map[int64]map[string]money.Amount{1: price, 2: price}
	f.offers.byChannelRule = map[int64]pricing.RuleRef{
		1: {ID: 41, Scope: "channel", Priority: 10,
			EffectiveFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		2: {ID: 42, Scope: "channel", Priority: 10,
			EffectiveFrom: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
	}

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}
	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Model.Suppliers) != 1 {
		t.Fatalf("suppliers = %+v，期望折成一条", body.Model.Suppliers)
	}
	supplier := body.Model.Suppliers[0]
	if supplier.ChannelCount != 2 {
		t.Errorf("channel_count = %d，期望 2", supplier.ChannelCount)
	}
	if supplier.Rule == nil {
		t.Fatal("缺少 rule 段")
	}
	if supplier.Rule.ID != 41 {
		t.Errorf("rule.id = %d，期望 41（渠道编号最小的那条）", supplier.Rule.ID)
	}
	if supplier.Rule.EffectiveFrom != "2026-01-01T00:00:00Z" {
		t.Errorf("rule.effective_from = %q，期望 2026-01-01T00:00:00Z",
			supplier.Rule.EffectiveFrom)
	}
}

// 平台自营与商家同价时平台排在最前（与清单的排序一致）。
func TestModelDetailPlatformFirstOnTie(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{
		{Model: "alpha", ChannelID: 1, OwnerUserID: 11, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2},
	}
	f.offers.base = mustAmount(t, "0.00300000")

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Model.Suppliers) != 2 || body.Model.Suppliers[0].Merchant != nil {
		t.Errorf("suppliers = %+v，期望平台自营（merchant=null）排在第一", body.Model.Suppliers)
	}
}

// 部分渠道没有价：suppliers 只列有价的，unpriced_channels 说出剩下的条数。
func TestModelDetailCountsUnpricedChannels(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{
		{Model: "alpha", ChannelID: 1, OwnerUserID: 11, Merchant: "acme"},
		{Model: "alpha", ChannelID: 2, OwnerUserID: 12, Merchant: "beta"},
		{Model: "alpha", ChannelID: 3},
	}
	f.offers.base = mustAmount(t, "0.00300000")
	f.offers.skipChannels = map[int64]bool{2: true, 3: true}

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}
	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Model.Suppliers) != 1 || body.Model.Suppliers[0].Merchant == nil ||
		*body.Model.Suppliers[0].Merchant != "acme" {
		t.Errorf("suppliers = %+v，期望只列有价的 acme", body.Model.Suppliers)
	}
	if body.Model.UnpricedChannels != 2 {
		t.Errorf("unpriced_channels = %d，期望 2", body.Model.UnpricedChannels)
	}
}

// 五个维度的价都要原样给出来，不是一个价的倍数。
func TestModelDetailCarriesDimensionPrices(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{{Model: "alpha", ChannelID: 1}}
	f.offers.byChannelPrices = map[int64]map[string]money.Amount{
		1: {
			"prompt":      mustAmount(t, "0.00300000"),
			"completion":  mustAmount(t, "0.00900000"),
			"cache_read":  mustAmount(t, "0.00060000"),
			"cache_write": mustAmount(t, "0.00360000"),
			"reasoning":   mustAmount(t, "0.01200000"),
		},
	}

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Model.Suppliers) != 1 {
		t.Fatalf("suppliers = %+v，期望一条", body.Model.Suppliers)
	}
	want := map[string]string{
		"prompt":      "0.00300000",
		"completion":  "0.00900000",
		"cache_read":  "0.00060000",
		"cache_write": "0.00360000",
		"reasoning":   "0.01200000",
	}
	if !reflect.DeepEqual(body.Model.Suppliers[0].Prices, want) {
		t.Errorf("prices = %+v，期望 %+v", body.Model.Suppliers[0].Prices, want)
	}
	if body.Model.Suppliers[0].Price != "0.00300000" {
		t.Errorf("price = %q，期望 prompt 维度的 0.00300000", body.Model.Suppliers[0].Price)
	}
}

// 六项能力与端点都要保住三态：false 是 false，没采集是 null。
func TestModelDetailCarriesCapabilityTriState(t *testing.T) {
	yes, no := true, false
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{
		CanonicalName: "alpha",
		DisplayName:   "Alpha",
		Vendor:        "example",
		Family:        "alpha-family",
		ContextLimit:  100,
		InputLimit:    80,
		OutputLimit:   20,
		Capabilities: catalog.Capabilities{
			Reasoning: &yes,
			Tools:     &no,
			// Vision / Batch 没采集过（nil），必须编码成 null 而不是 false。
			Audio: &yes,
			Cache: &no,
		},
		ModalitiesInput:  []string{"text"},
		ModalitiesOutput: []string{"text", "image"},
		ReasoningOptions: []any{map[string]any{"type": "toggle"}},
		CanonicalModelID: "alpha-canonical",
		KnowledgeCutoff:  datePtr(t, "2024-03-01"),
		UpdatedAt:        testNow,
	}}
	f.supplies.supplies = []channels.Supply{{Model: "alpha", ChannelID: 1}}
	f.offers.base = mustAmount(t, "0.00300000")

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}
	var raw struct {
		Model struct {
			Name             string   `json:"name"`
			DisplayName      string   `json:"display_name"`
			Vendor           string   `json:"vendor"`
			Family           string   `json:"family"`
			ContextLimit     int      `json:"context_limit"`
			InputLimit       int      `json:"input_limit"`
			OutputLimit      int      `json:"output_limit"`
			CanonicalModelID string   `json:"canonical_model_id"`
			KnowledgeCutoff  *string  `json:"knowledge_cutoff"`
			ReleaseDate      *string  `json:"release_date"`
			UpdatedAt        string   `json:"updated_at"`
			ModalitiesInput  []string `json:"modalities_input"`
			ModalitiesOutput []string `json:"modalities_output"`
			ReasoningOptions []any    `json:"reasoning_options"`
			Capabilities     struct {
				Reasoning *bool    `json:"reasoning"`
				Tools     *bool    `json:"tools"`
				Vision    *bool    `json:"vision"`
				Audio     *bool    `json:"audio"`
				Cache     *bool    `json:"cache"`
				Batch     *bool    `json:"batch"`
				Endpoints []string `json:"endpoints"`
			} `json:"capabilities"`
		} `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	model := raw.Model
	if model.Name != "alpha" || model.DisplayName != "Alpha" || model.Vendor != "example" ||
		model.Family != "alpha-family" || model.ContextLimit != 100 ||
		model.InputLimit != 80 || model.OutputLimit != 20 ||
		model.CanonicalModelID != "alpha-canonical" {
		t.Errorf("模型字段没有原样透传：%+v", model)
	}
	if model.KnowledgeCutoff == nil || *model.KnowledgeCutoff != "2024-03-01" {
		t.Errorf("knowledge_cutoff = %v，期望 2024-03-01", model.KnowledgeCutoff)
	}
	if model.ReleaseDate != nil {
		t.Errorf("release_date = %v，期望 null", *model.ReleaseDate)
	}
	if want := "2026-10-21T12:00:00Z"; model.UpdatedAt != want {
		t.Errorf("updated_at = %q，期望 %q", model.UpdatedAt, want)
	}
	if !reflect.DeepEqual(model.ModalitiesInput, []string{"text"}) ||
		!reflect.DeepEqual(model.ModalitiesOutput, []string{"text", "image"}) {
		t.Errorf("模态没有原样透传：%v / %v", model.ModalitiesInput, model.ModalitiesOutput)
	}
	if len(model.ReasoningOptions) != 1 {
		t.Errorf("reasoning_options = %v，期望一项", model.ReasoningOptions)
	}

	caps := model.Capabilities
	if caps.Reasoning == nil || !*caps.Reasoning {
		t.Errorf("reasoning = %v，期望 true", caps.Reasoning)
	}
	if caps.Tools == nil || *caps.Tools {
		t.Errorf("tools = %v，期望 false（而不是 null）", caps.Tools)
	}
	if caps.Audio == nil || !*caps.Audio {
		t.Errorf("audio = %v，期望 true", caps.Audio)
	}
	if caps.Cache == nil || *caps.Cache {
		t.Errorf("cache = %v，期望 false（而不是 null）", caps.Cache)
	}
	if caps.Vision != nil {
		t.Errorf("vision = %v，期望 null（没采集过）", *caps.Vision)
	}
	if caps.Batch != nil {
		t.Errorf("batch = %v，期望 null（没采集过）", *caps.Batch)
	}
	if caps.Endpoints != nil {
		t.Errorf("endpoints = %v，期望 null（没采集过）", caps.Endpoints)
	}
}

// 端点声明的空切片必须与 null 分开：[] 是「明确一条都不支持」。
func TestModelDetailKeepsEmptyEndpoints(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{
		CanonicalName: "alpha",
		Capabilities:  catalog.Capabilities{Endpoints: []channel.Endpoint{}},
	}}
	f.supplies.supplies = []channels.Supply{{Model: "alpha", ChannelID: 1}}
	f.offers.base = mustAmount(t, "0.00300000")

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	var raw struct {
		Model struct {
			Capabilities struct {
				Endpoints []string `json:"endpoints"`
			} `json:"capabilities"`
		} `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if raw.Model.Capabilities.Endpoints == nil {
		t.Errorf("端点的空声明被编码成了 null：%s", rec.Body)
	}
}

// 一条报价都算不出来的模型：仍然 200，suppliers 整个省略。
func TestModelDetailWithoutQuotesStillOK(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{{Model: "alpha", ChannelID: 1}}
	f.offers.skipChannels = map[int64]bool{1: true}

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	var model map[string]json.RawMessage
	if err := json.Unmarshal(raw["model"], &model); err != nil {
		t.Fatalf("model 不是对象: %v", err)
	}
	if _, present := model["suppliers"]; present {
		t.Errorf("suppliers 应当整个省略，得到 %s", model["suppliers"])
	}
	// 没有价不等于没有供给：这一条渠道仍然在供，只是算不出价。
	if got := string(model["unpriced_channels"]); got != "1" {
		t.Errorf("unpriced_channels = %s，期望 1", got)
	}
}

// 规范名里可能有 "/"（`vendor/model` 这种写法），{name...} 必须整个接住。
func TestModelDetailAcceptsSlashInName(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "vendor/alpha"}}
	f.supplies.supplies = []channels.Supply{{Model: "vendor/alpha", ChannelID: 1}}
	f.offers.base = mustAmount(t, "0.00300000")

	rec := f.request(t, http.MethodGet, PathPrefix+"models/vendor/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}
	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Model.Name != "vendor/alpha" {
		t.Errorf("name = %q，期望 vendor/alpha", body.Model.Name)
	}
}

// 大小写变体回与「目录里没有」逐字相同的 404。两层都是字面比较：供给匹配
// 与目录查找各自按名字精确比较，任一层放宽都会让这里的行为变掉。
func TestModelDetailIsCaseSensitive(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{{Model: "alpha", ChannelID: 1}}
	f.offers.base = mustAmount(t, "0.00300000")

	// 同一个 request id 才比得出「正文逐字相同」：错误体里带着它。
	headers := map[string]string{"x-request-id": "fixed-request-id"}
	variant := f.requestAsWith(t, "127.0.0.1:8080", headers,
		http.MethodGet, PathPrefix+"models/Alpha", "")
	absent := f.requestAsWith(t, "127.0.0.1:8080", headers,
		http.MethodGet, PathPrefix+"models/absent", "")

	if variant.Code != http.StatusNotFound || absent.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d / %d，期望都是 404", variant.Code, absent.Code)
	}
	if got := errorCode(t, variant); got != codeNotFound {
		t.Errorf("大小写变体的错误码 = %q，期望 %q", got, codeNotFound)
	}
	if variant.Body.String() != absent.Body.String() {
		t.Errorf("大小写变体与「目录里没有」的正文不同：\n变体: %s\n不存在: %s",
			variant.Body, absent.Body)
	}
}

// 多段通配符按路径段反转义：编码过的 "/" 与字面写出的 "/" 是同一个名字。
func TestModelDetailDecodesEscapedName(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{
		{CanonicalName: "vendor/alpha"},
		{CanonicalName: "vendor%2Falpha"},
	}
	f.supplies.supplies = []channels.Supply{
		{Model: "vendor/alpha", ChannelID: 1},
		{Model: "vendor%2Falpha", ChannelID: 2},
	}
	f.offers.base = mustAmount(t, "0.00300000")

	// %2F 在单个路径段里反转义成 "/"，与字面写出的两段同名。
	escapedSlash := f.request(t, http.MethodGet, PathPrefix+"models/vendor%2Falpha", "")
	if escapedSlash.Code != http.StatusOK {
		t.Fatalf("%%2F 的状态码 = %d，期望 200（响应 %s）",
			escapedSlash.Code, escapedSlash.Body)
	}
	var slashBody modelDetailResponse
	if err := json.Unmarshal(escapedSlash.Body.Bytes(), &slashBody); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if slashBody.Model.Name != "vendor/alpha" {
		t.Errorf("%%2F 解出的 name = %q，期望 vendor/alpha", slashBody.Model.Name)
	}

	// %25 反转义成字面量 "%"，只解一层：不会再把 %2F 解成 "/"。
	escapedPercent := f.request(t, http.MethodGet, PathPrefix+"models/vendor%252Falpha", "")
	if escapedPercent.Code != http.StatusOK {
		t.Fatalf("%%25 的状态码 = %d，期望 200（响应 %s）",
			escapedPercent.Code, escapedPercent.Body)
	}
	var percentBody modelDetailResponse
	if err := json.Unmarshal(escapedPercent.Body.Bytes(), &percentBody); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if percentBody.Model.Name != "vendor%2Falpha" {
		t.Errorf("%%25 解出的 name = %q，期望 vendor%%2Falpha", percentBody.Model.Name)
	}
}

// datePtr 造一个 YYYY-MM-DD 的日期指针。
func datePtr(t *testing.T, raw string) *time.Time {
	t.Helper()
	value, err := time.Parse("2006-01-02", raw)
	if err != nil {
		t.Fatalf("解析日期 %q 失败: %v", raw, err)
	}
	return &value
}

// 供应商报价带上每请求的固定费用、最低消费、计量项单价与规则来源。
//
// 规则段只说「哪条规则、什么作用域、什么优先级、生效窗口」，不得出现表达式
// 源码、owner id、channel id 与 base_url——这一页是公开的。
func TestModelDetailCarriesChargesUnitsAndRule(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{{Model: "alpha", ChannelID: 1}}
	f.offers.byChannelPrices = map[int64]map[string]money.Amount{
		1: {"prompt": mustAmount(t, "0.00300000")},
	}
	f.offers.byChannelRequest = map[int64]money.Amount{1: mustAmount(t, "0.05000000")}
	f.offers.byChannelMinimum = map[int64]money.Amount{1: mustAmount(t, "1.00000000")}
	f.offers.byChannelUnits = map[int64]map[string]money.Amount{
		1: {"cache_write_5m": mustAmount(t, "0.00030000")},
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.offers.byChannelRule = map[int64]pricing.RuleRef{
		1: {ID: 42, Scope: "channel", Priority: 10, EffectiveFrom: from},
	}

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", rec.Code, rec.Body)
	}

	var body modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(body.Model.Suppliers) != 1 {
		t.Fatalf("suppliers = %+v，期望一条", body.Model.Suppliers)
	}
	supplier := body.Model.Suppliers[0]
	if supplier.Request == nil || *supplier.Request != "0.05000000" {
		t.Errorf("request = %v，期望 0.05000000", supplier.Request)
	}
	if supplier.Minimum == nil || *supplier.Minimum != "1.00000000" {
		t.Errorf("minimum = %v，期望 1.00000000", supplier.Minimum)
	}
	if got := supplier.Units["cache_write_5m"]; got != "0.00030000" {
		t.Errorf("units = %+v，期望 cache_write_5m = 0.00030000", supplier.Units)
	}
	if supplier.Rule == nil {
		t.Fatal("缺少 rule 段")
	}
	if supplier.Rule.ID != 42 || supplier.Rule.Scope != "channel" || supplier.Rule.Priority != 10 {
		t.Errorf("rule = %+v，期望 id/scope/priority 原样带出", supplier.Rule)
	}
	if supplier.Rule.EffectiveFrom != "2026-01-01T00:00:00Z" {
		t.Errorf("rule.effective_from = %q，期望 RFC3339 UTC", supplier.Rule.EffectiveFrom)
	}
	if supplier.Rule.EffectiveTo != nil {
		t.Errorf("rule.effective_to = %v，期望 null（不过期）", supplier.Rule.EffectiveTo)
	}

	// 键集合必须正好是这几项：新增字段不能顺带把表达式源码、owner/channel id
	// 或 base_url 带出去。
	var rawBody struct {
		Model struct {
			Suppliers []map[string]json.RawMessage `json:"suppliers"`
		} `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawBody); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	wantKeys := map[string]bool{
		"merchant": true, "price": true, "prices": true, "request": true,
		"minimum": true, "units": true, "rule": true, "channel_count": true,
	}
	rawSupplier := rawBody.Model.Suppliers[0]
	if len(rawSupplier) != len(wantKeys) {
		t.Errorf("supplier 有 %d 个字段，期望正好 %d 个：%v",
			len(rawSupplier), len(wantKeys), rawSupplier)
	}
	for key := range wantKeys {
		if _, ok := rawSupplier[key]; !ok {
			t.Errorf("supplier 缺字段 %q：%v", key, rawSupplier)
		}
	}
	for key := range rawSupplier {
		if !wantKeys[key] {
			t.Errorf("supplier 多出字段 %q：%v", key, rawSupplier)
		}
	}

	var rawRule map[string]json.RawMessage
	if err := json.Unmarshal(rawSupplier["rule"], &rawRule); err != nil {
		t.Fatalf("rule 不是 JSON 对象: %v", err)
	}
	wantRuleKeys := map[string]bool{
		"id": true, "scope": true, "priority": true,
		"effective_from": true, "effective_to": true,
	}
	if len(rawRule) != len(wantRuleKeys) {
		t.Errorf("rule 有 %d 个字段，期望正好 %d 个：%v",
			len(rawRule), len(wantRuleKeys), rawRule)
	}
	for key := range wantRuleKeys {
		if _, ok := rawRule[key]; !ok {
			t.Errorf("rule 缺字段 %q：%v", key, rawRule)
		}
	}
}

// 规则没写固定费、最低消费、计量项，也没有规则来源时，这三个键与 rule
// 都不出现：省略比给一个 0 或空壳 rule 更少被误读。
func TestModelDetailOmitsAbsentChargesAndRule(t *testing.T) {
	f := newFixture(t)
	f.catalog.models = []catalog.Model{{CanonicalName: "alpha"}}
	f.supplies.supplies = []channels.Supply{{Model: "alpha", ChannelID: 1}}
	f.offers.base = mustAmount(t, "0.00300000")

	rec := f.request(t, http.MethodGet, PathPrefix+"models/alpha", "")
	var rawBody struct {
		Model struct {
			Suppliers []map[string]json.RawMessage `json:"suppliers"`
		} `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawBody); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	rawSupplier := rawBody.Model.Suppliers[0]
	for _, absent := range []string{"request", "minimum", "units", "rule"} {
		if _, ok := rawSupplier[absent]; ok {
			t.Errorf("supplier 多出字段 %q（规则没写，应为省略）：%v", absent, rawSupplier)
		}
	}
}
