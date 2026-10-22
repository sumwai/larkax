package consoleapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是两条公开端点：部署常量与模型参考价。
//
// 它们不要求会话。模型与价格本来就是公开信息（落地页也要用），而 meta 是
// 前端启动时第一件要知道的事——要它先登录，等于让登录页不知道自己在哪个站。

// metaResponse 是部署常量。
type metaResponse struct {
	SiteName   string       `json:"site_name"`
	APIBaseURL string       `json:"api_base_url"`
	Currency   string       `json:"currency"`
	Timezone   string       `json:"timezone"`
	Locales    []string     `json:"locales"`
	Features   metaFeatures `json:"features"`
}

// metaFeatures 是给界面做分支用的能力开关。
//
// v1 里三项都是固定值：没有注册入口、找回口令走运维、不能在线充值。
// 写成响应字段而不是让前端写死，是因为它们将来会变，而前端不该为一次
// 策略变更发版。
type metaFeatures struct {
	Registration  bool   `json:"registration"`
	PasswordReset string `json:"password_reset"`
	Topup         bool   `json:"topup"`
}

// handleMeta 返回部署常量。
func (a *API) handleMeta(w http.ResponseWriter, r *http.Request) {
	baseURL := a.baseURL
	if baseURL == "" {
		// 没配固定值时按这次请求推。生产里前面有 Caddy，Host 是用户看到的
		// 那个域名；X-Forwarded-* 只在受信代理时读（见 origin）。
		baseURL = a.origin(r) + "/v1"
	}
	a.ok(w, http.StatusOK, metaResponse{
		SiteName:   a.siteName,
		APIBaseURL: baseURL,
		Currency:   a.currency,
		Timezone:   a.loc.String(),
		Locales:    []string{"en", "zh"},
		Features: metaFeatures{
			Registration:  false,
			PasswordReset: "admin",
			Topup:         false,
		},
	})
}

// modalitiesJSON 是输入与输出的模态。
type modalitiesJSON struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// pricingJSON 是一个模型的参考价，单位是每百万 token。
//
// 缓存两项与推理一项可为 null：规则里没声明就是「没配」，与 0（免费）不是
// 一回事。一次性费用与计量项单价不成「每百万 token」的倍数，原样给出：
// request / minimum 是每请求多少元，units 是每个计量单位多少元；没写时
// 省略这个键。
type pricingJSON struct {
	InputPerMTok      string            `json:"input_per_mtok"`
	OutputPerMTok     string            `json:"output_per_mtok"`
	CacheReadPerMTok  *string           `json:"cache_read_per_mtok"`
	CacheWritePerMTok *string           `json:"cache_write_per_mtok"`
	ReasoningPerMTok  *string           `json:"reasoning_per_mtok"`
	Request           *string           `json:"request,omitempty"`
	Minimum           *string           `json:"minimum,omitempty"`
	Units             map[string]string `json:"units,omitempty"`
}

// modelJSON 是模型清单里的一项。
type modelJSON struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Vendor      string `json:"vendor"`
	// Family 是厂商下的系列（如 `claude`、`gpt`）。目录里没填时省略，
	// 不输出空串：界面按「有没有」决定显不显示这一段。
	Family       string          `json:"family,omitempty"`
	ContextLimit int             `json:"context_limit"`
	MaxOutput    int             `json:"max_output"`
	Capabilities map[string]bool `json:"capabilities"`
	Modalities   modalitiesJSON  `json:"modalities"`
	// Pricing 为 nil 表示没有平台规则——界面显示「—」，不伪造一个起价。
	Pricing *pricingJSON `json:"pricing"`
	// Offers 是按渠道归属折出来的报价，排序见 pricing.GroupByModel（有 token
	// 价的在前）；没有报价时省略这个
	// 字段而不是给一个空数组（理由见 api.modelJSON 的同名字段）。
	Offers []offerJSON `json:"offers,omitempty"`
}

// offerJSON 是一个模型的一条报价，形状与 /v1/models 的 offers 项相同。
//
// Merchant 为 null 表示平台自营。Price 是 1000 个输入 token 的金额——
// 与同一条目里的 pricing（每百万 token）不是同一个单位，界面必须标出来。
// 规则没声明输入价时 Price 是零值；要分辨「输入免费」与「没写输入价」，
// 看 Prices 里有没有 prompt 这个键。Prices 是各 token 维度的千 token 金额，
// 只含规则写过的维度。
//
// Request / Minimum / Units 的量纲与 Prices 不同：前两项是每请求多少元，
// units 是每个计量单位多少元。为 0（或没有计量项）时省略这个键。
type offerJSON struct {
	Merchant *string           `json:"merchant"`
	Price    string            `json:"price"`
	Prices   map[string]string `json:"prices,omitempty"`
	Request  *string           `json:"request,omitempty"`
	Minimum  *string           `json:"minimum,omitempty"`
	Units    map[string]string `json:"units,omitempty"`
}

// modelsResponse 是模型清单。
type modelsResponse struct {
	Currency    string      `json:"currency"`
	ReferenceAt string      `json:"reference_at"`
	Items       []modelJSON `json:"items"`
}

// handleModels 返回在供的模型与参考价。
//
// 「在供」的判据来自 channels.Source.SuppliedModels——它复用选路那一侧的
// 谓词（渠道启用且有货），本包不抄一份 SQL。抄一份的那天，就是「页面上
// 有、实际调不通」开始的那天。
func (a *API) handleModels(w http.ResponseWriter, r *http.Request) {
	names, err := a.deps.models.SuppliedModels(r.Context())
	if err != nil {
		a.unavailable(w, r, err, "读取在供模型失败")
		return
	}

	models, err := a.deps.catalog.Models(r.Context())
	if err != nil {
		a.unavailable(w, r, err, "读取模型目录失败")
		return
	}
	byName := make(map[string]catalog.Model, len(models))
	for _, model := range models {
		byName[model.CanonicalName] = model
	}

	now := a.now().UTC()
	prices, err := a.deps.prices.ReferencePrices(r.Context(), names, now)
	if err != nil {
		a.unavailable(w, r, err, "求参考价失败")
		return
	}

	supplies, err := a.deps.supplies.Supplies(r.Context())
	if err != nil {
		a.unavailable(w, r, err, "读取在供渠道失败")
		return
	}
	candidates := make([]pricing.Candidate, 0, len(supplies))
	for _, supply := range supplies {
		candidates = append(candidates, pricing.Candidate{
			Model:       supply.Model,
			ChannelID:   supply.ChannelID,
			OwnerUserID: supply.OwnerUserID,
			Merchant:    supply.Merchant,
		})
	}
	quotes, err := a.deps.offers.Prices(r.Context(), now, candidates)
	if err != nil {
		a.unavailable(w, r, err, "求报价失败")
		return
	}
	offers := pricing.GroupByModel(quotes, candidates)

	items := make([]modelJSON, 0, len(names))
	for _, name := range names {
		model, ok := byName[name]
		if !ok {
			// 供应侧只 JOIN 了 model_catalog，正常到不了这里；真到了说明两份
			// 读之间目录被改过。跳过而不是造一个空模型：一个没有展示字段的
			// 条目会让前端显示一行空白。
			continue
		}
		items = append(items, newModelJSON(model, prices[name], offers[name].Offers))
	}

	// ETag 只对「模型 + 价格」求值：reference_at 是这次求值的时刻，每次请求
	// 都不同。把它算进去，同一个 ETag 永远对不上，304 也就永远不会发生。
	// 正文仍然带它——它说明这些价格是什么时候算出来的。
	tagBody, err := json.Marshal(struct {
		Currency string      `json:"currency"`
		Items    []modelJSON `json:"items"`
	}{Currency: a.currency, Items: items})
	if err != nil {
		a.internal(w, r, err, "序列化模型清单失败")
		return
	}
	sum := sha256.Sum256(tagBody)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`

	body, err := json.Marshal(modelsResponse{
		Currency:    a.currency,
		ReferenceAt: formatTime(now),
		Items:       items,
	})
	if err != nil {
		a.internal(w, r, err, "序列化模型清单失败")
		return
	}

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// matchesETag 判断 If-None-Match 是否命中。
//
// 只按整串比较（允许一个逗号分隔的清单）：客户端与 CDN 送回来的通常是它们
// 上次拿到的那个值，弱比较（W/ 前缀）在静态内容上没有意义——这里没有
// 语义等价而字节不同的两个版本。
func matchesETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

// newModelJSON 把目录行与参考价折成一项。
func newModelJSON(model catalog.Model, price *pricing.ReferencePrice, offers []pricing.Offer) modelJSON {
	out := modelJSON{
		Name:         model.CanonicalName,
		DisplayName:  model.DisplayName,
		Vendor:       model.Vendor,
		Family:       model.Family,
		ContextLimit: model.ContextLimit,
		MaxOutput:    model.OutputLimit,
		Capabilities: capabilitiesJSON(model.Capabilities),
		Modalities: modalitiesJSON{
			Input:  nonNilStrings(model.ModalitiesInput),
			Output: nonNilStrings(model.ModalitiesOutput),
		},
	}
	if out.DisplayName == "" {
		out.DisplayName = model.CanonicalName
	}
	for _, offer := range offers {
		out.Offers = append(out.Offers, offerJSON{
			Merchant: offer.Merchant,
			Price:    offer.Price.String(),
			Prices:   dimensionPricesJSON(offer.Prices),
			Request:  amountOrNil(offer.Request),
			Minimum:  amountOrNil(offer.Minimum),
			Units:    dimensionPricesJSON(offer.Units),
		})
	}
	if price != nil {
		out.Pricing = &pricingJSON{
			InputPerMTok:  price.InputPerMTok.String(),
			OutputPerMTok: price.OutputPerMTok.String(),
			Request:       amountOrNil(price.Request),
			Minimum:       amountOrNil(price.Minimum),
			Units:         dimensionPricesJSON(price.Units),
		}
		if price.CacheReadPerMTok != nil {
			value := price.CacheReadPerMTok.String()
			out.Pricing.CacheReadPerMTok = &value
		}
		if price.CacheWritePerMTok != nil {
			value := price.CacheWritePerMTok.String()
			out.Pricing.CacheWritePerMTok = &value
		}
		if price.ReasoningPerMTok != nil {
			value := price.ReasoningPerMTok.String()
			out.Pricing.ReasoningPerMTok = &value
		}
	}
	return out
}

// amountOrNil 把 0 金额编码成「省略这个键」。
//
// 固定费与最低消费的 0 值同时表示「规则没写」与「写了 0 元」，两者都是一分
// 钱不收；给一个 "0.00000000" 会让客户端把它当成「确认免费」而与「没配」
// 混在一起。量纲是每请求的金额，不能与 token 单价相加。
func amountOrNil(amount money.Amount) *string {
	if amount == money.Zero {
		return nil
	}
	value := amount.String()
	return &value
}

// dimensionPricesJSON 把维度价映射成响应里的字符串 map。
//
// 空 map 返回 nil：omitempty 把「规则只有固定费、没有 token 维度」写成
// 省略字段，而不是 `"prices": {}`——后者看起来像「查过了，五个维度都没有」。
func dimensionPricesJSON(prices map[string]money.Amount) map[string]string {
	if len(prices) == 0 {
		return nil
	}
	out := make(map[string]string, len(prices))
	for name, price := range prices {
		out[name] = price.String()
	}
	return out
}

// capabilitiesJSON 把三态能力列折成「只出现有值的那几项」。
//
// 「未采集」不出现在响应里，界面显示「未知」而不是「不支持」——把两者合并
// 会让一份没采集全的目录看起来像「这个模型什么都不支持」。
func capabilitiesJSON(c catalog.Capabilities) map[string]bool {
	out := make(map[string]bool, 4)
	if c.Tools != nil {
		out["tools"] = *c.Tools
	}
	if c.Vision != nil {
		out["vision"] = *c.Vision
	}
	if c.Reasoning != nil {
		out["reasoning"] = *c.Reasoning
	}
	if c.Cache != nil {
		out["caching"] = *c.Cache
	}
	return out
}

// nonNilStrings 把 nil 切片折成空切片，让 JSON 输出 `[]` 而不是 `null`。
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
