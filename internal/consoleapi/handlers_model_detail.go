package consoleapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是控制台的**单条模型**读接口：模型页 `/app/models/[model-id]` 要的
// 信息一次给齐——模型目录里能编辑的全部字段，加上供应它的商家与报价。
//
// 它公开只读、不要求会话，理由与 handleModels 相同：模型与价格本来就是公开
// 信息。与 handleModels 的区别是回答的问题：那一份列「有哪些模型、参考价
// 多少」，这一份回答「这一个模型是什么、谁在供、各要多少钱」。

// modelDetailResponse 是单条模型的响应体。
//
// 模型字段包在 model 里而不是铺在顶层：currency 与 reference_at 说的是
// **报价**，不是模型目录的属性；三层并列时前端容易把参考时刻读成模型的
// 更新时间。
type modelDetailResponse struct {
	Currency    string          `json:"currency"`
	ReferenceAt string          `json:"reference_at"`
	Model       modelDetailJSON `json:"model"`
}

// modelDetailJSON 是模型目录里一条规范模型的全部展示字段。
//
// 与 modelJSON（清单里的一项）不是同一个形状：清单只要页面上那几格，而这一份
// 要把管理页能编辑的信息全给出来（除了采集来的 raw——那是运维排查用的原始
// 元信息，不适合公开）。两者刻意不合并：合并的结果是清单多背十几个没有消费
// 者的字段，或者某一页少了一格却没人发现。
type modelDetailJSON struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Vendor      string `json:"vendor"`
	Family      string `json:"family"`

	// 三个上限为 0 表示目录里没填（见 catalog.Model 的注释），不是「上限是 0」。
	ContextLimit int `json:"context_limit"`
	InputLimit   int `json:"input_limit"`
	OutputLimit  int `json:"output_limit"`

	Capabilities     modelCapabilitiesJSON `json:"capabilities"`
	ModalitiesInput  []string              `json:"modalities_input"`
	ModalitiesOutput []string              `json:"modalities_output"`
	ReasoningOptions []any                 `json:"reasoning_options"`
	CanonicalModelID string                `json:"canonical_model_id"`
	// 两个日期是 YYYY-MM-DD；目录里没填时为 null。
	KnowledgeCutoff *string `json:"knowledge_cutoff"`
	ReleaseDate     *string `json:"release_date"`
	UpdatedAt       string  `json:"updated_at"`

	// Suppliers 是供应这个模型、且能算出价的商家报价；同一商家可能有多套
	// 维度价，各占一行。按价格升序。
	//
	// 一条报价都算不出来的模型仍然返回 200（可见性只看供给），这时整个字段
	// 省略：空数组会被读成「问了，但没有人报价」，而实际是「还没有定价」。
	Suppliers []supplierJSON `json:"suppliers,omitempty"`
	// UnpricedChannels 是在供、但算不出价的渠道条数。
	//
	// 总是输出（包括 0）：这是模型级的统计，不是某一条报价的属性；省略它
	// 会让「全部渠道都有价」与「这个字段还没上线」分不出来。
	UnpricedChannels int `json:"unpriced_channels"`
}

// modelCapabilitiesJSON 是六项能力与端点声明的**三态**形状。
//
// 六项用指针、不加 omitempty：目录里没采集过是 null，明确不支持是 false。
// 把两者都写成 false 会让一个「不知道」看起来像「确认没有」，而这正是
// catalog.Capabilities 存在的理由。
type modelCapabilitiesJSON struct {
	Reasoning *bool `json:"reasoning"`
	Tools     *bool `json:"tools"`
	Vision    *bool `json:"vision"`
	Audio     *bool `json:"audio"`
	Cache     *bool `json:"cache"`
	Batch     *bool `json:"batch"`
	// Endpoints 同样是三态：null 未知、[] 明确一条都不支持、非空支持这几条。
	// nil 切片编码成 null、空切片编码成 []，所以这里不能把 nil 折成空切片。
	Endpoints []string `json:"endpoints"`
}

// supplierJSON 是一个商家（或平台自营）在这个模型上的一套报价。
//
// 不含渠道 id、base_url、凭证、上游地址与上游模型名：这一页是公开的，
// 上游名能把调用方引去绕过网关。渠道之间靠「商家 + 整套维度价」区分。
type supplierJSON struct {
	// Merchant 为 null 表示平台自营渠道。
	Merchant *string `json:"merchant"`
	// Price 是 1000 个输入 token 的金额，字符串，与结算同源。
	Price string `json:"price"`
	// Prices 是各 token 维度的千 token 金额，键是 prompt / completion /
	// cache_read / cache_write / reasoning；只含规则写过的维度。
	Prices map[string]string `json:"prices,omitempty"`
	// Request / Minimum 是每请求的固定费用与最低消费（每次请求多少元），
	// Units 是计量项单价（每个计量单位多少元）。为 0 或没有计量项时省略。
	Request *string           `json:"request,omitempty"`
	Minimum *string           `json:"minimum,omitempty"`
	Units   map[string]string `json:"units,omitempty"`
	// Rule 说明这套价出自哪条规则；口径见 ruleJSON。
	Rule *ruleJSON `json:"rule,omitempty"`
	// ChannelCount 是这套价对应的渠道条数（同一商家同价的多条供给折成一行）。
	ChannelCount int `json:"channel_count"`
}

// ruleJSON 是一条规则的对外元信息：编号、作用域、优先级与生效窗口。
//
// 不含表达式源码，也不含 owner id 与 channel id：这一页是公开的，前两者
// 会把定价逻辑与「谁定的价」暴露出去，渠道 id 还能关联到运营面的记录。
// 作用域只说这一档价是按渠道、按商家还是平台兜底定的。
type ruleJSON struct {
	ID            int64  `json:"id"`
	Scope         string `json:"scope"`
	Priority      int    `json:"priority"`
	EffectiveFrom string `json:"effective_from"`
	// EffectiveTo 为 null 表示不过期。
	EffectiveTo *string `json:"effective_to"`
}

// handleModelDetail 返回一个在供模型的全部展示字段与供应商报价。
//
// 「在供」复用 channels.Source.Supplies 那一条判据（与清单同一处 SQL），
// 不在供与目录里没有故意回同一个 404：让调用方能靠状态码探目录，等于把
// 「某名字在不在」告诉一个只是写错名字的请求。
func (a *API) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		a.notFound(w, r)
		return
	}

	ctx := r.Context()
	supplies, err := a.deps.supplies.Supplies(ctx)
	if err != nil {
		a.unavailable(w, r, err, "读取在供渠道失败")
		return
	}

	// 一次把「在供判断」与「候选集」都做掉：同一份 Supplies 里既有可见性
	// 判据，也有报价要的渠道归属。
	candidates := make([]pricing.Candidate, 0, 2)
	for _, supply := range supplies {
		if supply.Model != name {
			continue
		}
		candidates = append(candidates, pricing.Candidate{
			Model:       supply.Model,
			ChannelID:   supply.ChannelID,
			OwnerUserID: supply.OwnerUserID,
			Merchant:    supply.Merchant,
		})
	}
	if len(candidates) == 0 {
		a.notFound(w, r)
		return
	}

	model, err := a.deps.catalog.Model(ctx, name)
	if errors.Is(err, catalog.ErrModelNotFound) {
		// 供给查询与目录读取之间目录被删了。与「不在供」同一个正文：对
		// 调用方来说这两件事都是「这个模型现在拿不到」。
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.unavailable(w, r, err, "读取模型目录失败")
		return
	}

	now := a.now().UTC()
	quotes, err := a.deps.offers.Prices(ctx, now, candidates)
	if err != nil {
		a.unavailable(w, r, err, "求报价失败")
		return
	}

	grouped := pricing.GroupByModel(quotes, candidates)[name]
	detail := newModelDetailJSON(model)
	detail.UnpricedChannels = grouped.UnpricedChannels
	detail.Suppliers = newSuppliers(grouped.Offers)

	// 这条响应只依赖目录与计价规则，两者都不是按用户变化的：60 秒的公共
	// 缓存能让翻模型页时不重复打三条查询，又短到改价后一分钟内可见。
	// 它不走 a.ok——那里统一写 no-store，是给需要会话的响应用的。
	w.Header().Set("Cache-Control", "public, max-age=60")
	a.writeJSON(w, http.StatusOK, modelDetailResponse{
		Currency:    a.currency,
		ReferenceAt: formatTime(now),
		Model:       detail,
	})
}

// newModelDetailJSON 把目录行映射成响应里的模型对象。
func newModelDetailJSON(model catalog.Model) modelDetailJSON {
	displayName := model.DisplayName
	if displayName == "" {
		// 与清单那一侧同一个兜底：页面不会出现一行没有标题的模型。
		displayName = model.CanonicalName
	}
	reasoning := model.ReasoningOptions
	if reasoning == nil {
		reasoning = []any{}
	}
	return modelDetailJSON{
		Name:             model.CanonicalName,
		DisplayName:      displayName,
		Vendor:           model.Vendor,
		Family:           model.Family,
		ContextLimit:     model.ContextLimit,
		InputLimit:       model.InputLimit,
		OutputLimit:      model.OutputLimit,
		Capabilities:     newModelCapabilitiesJSON(model.Capabilities),
		ModalitiesInput:  nonNilStrings(model.ModalitiesInput),
		ModalitiesOutput: nonNilStrings(model.ModalitiesOutput),
		ReasoningOptions: reasoning,
		CanonicalModelID: model.CanonicalModelID,
		KnowledgeCutoff:  dateValue(model.KnowledgeCutoff),
		ReleaseDate:      dateValue(model.ReleaseDate),
		UpdatedAt:        formatTime(model.UpdatedAt),
	}
}

// newModelCapabilitiesJSON 原样透传三态。
func newModelCapabilitiesJSON(catalogCaps catalog.Capabilities) modelCapabilitiesJSON {
	view := modelCapabilitiesJSON{
		Reasoning: catalogCaps.Reasoning,
		Tools:     catalogCaps.Tools,
		Vision:    catalogCaps.Vision,
		Audio:     catalogCaps.Audio,
		Cache:     catalogCaps.Cache,
		Batch:     catalogCaps.Batch,
	}
	// 只有 nil 才保持 null；空切片必须编码成 []。用 make 无条件造一个空切片
	// 会把「没采集过」变成「确认一条都不支持」。
	if catalogCaps.Endpoints != nil {
		endpoints := make([]string, 0, len(catalogCaps.Endpoints))
		for _, endpoint := range catalogCaps.Endpoints {
			endpoints = append(endpoints, string(endpoint))
		}
		view.Endpoints = endpoints
	}
	return view
}

// dateValue 把可空日期编码成 YYYY-MM-DD；nil 保持 null。
func dateValue(value *time.Time) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	formatted := value.Format("2006-01-02")
	return &formatted
}

// newSuppliers 把折叠好的报价映射成响应里的供应商列表。
//
// 聚合已经由 pricing.GroupByModel 做完：一组 = 商家（平台自营单列）×
// 一整套维度价，ChannelCount 是这套价的渠道条数。这里只做形状转换，不再
// 按价格或上游名去重——折叠规则属于报价层，两个 HTTP 表面共用同一份。
//
// 没有报价的渠道不出现在 suppliers 里；它们由模型级的 unpriced_channels
// 计数回答（见 modelDetailJSON 的注释）。
func newSuppliers(offers []pricing.Offer) []supplierJSON {
	out := make([]supplierJSON, 0, len(offers))
	for _, offer := range offers {
		entry := supplierJSON{
			Merchant:     offer.Merchant,
			Price:        offer.Price.String(),
			Prices:       dimensionPricesJSON(offer.Prices),
			Request:      amountOrNil(offer.Request),
			Minimum:      amountOrNil(offer.Minimum),
			Units:        dimensionPricesJSON(offer.Units),
			ChannelCount: offer.ChannelCount,
		}
		// 有价的报价一定来自一条规则；编号为 0 只可能来自没有规则来源的
		// 构造，那时不给一个空壳 rule——它会让页面显示一条不存在的规则。
		if offer.Rule.ID != 0 {
			entry.Rule = newRuleJSON(offer.Rule)
		}
		out = append(out, entry)
	}
	return out
}

// newRuleJSON 把规则的元信息映射成响应形状。时刻统一成 RFC3339 UTC，
// 与 reference_at 同一个格式。
func newRuleJSON(rule pricing.RuleRef) *ruleJSON {
	return &ruleJSON{
		ID:            rule.ID,
		Scope:         rule.Scope,
		Priority:      rule.Priority,
		EffectiveFrom: formatTime(rule.EffectiveFrom),
		EffectiveTo:   optionalTime(rule.EffectiveTo),
	}
}
