package consoleapi

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/money"
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

	// Suppliers 是供应这个模型、且能算出价的渠道，按价格升序。
	//
	// 一条报价都算不出来的模型仍然返回 200（可见性只看供给），这时整个字段
	// 省略：空数组会被读成「问了，但没有人报价」，而实际是「还没有定价」。
	Suppliers []supplierJSON `json:"suppliers,omitempty"`
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

// supplierJSON 是一个在供渠道的报价。
//
// 不含渠道 id、base_url、凭证与任何上游地址：这一页是公开的。渠道之间靠
// 「商家 + 上游模型名 + 价格」区分，足以回答「谁在供、多少钱」。
type supplierJSON struct {
	// Merchant 为 null 表示平台自营渠道。
	Merchant *string `json:"merchant"`
	// UpstreamModel 是走这条渠道时发给上游的模型名（别名解析后的结果）。
	UpstreamModel string `json:"upstream_model"`
	// Price 是 1000 个输入 token 的金额，字符串，与结算同源。
	Price string `json:"price"`
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
	onSupply := make([]channels.Supply, 0, 2)
	candidates := make([]pricing.Candidate, 0, 2)
	for _, supply := range supplies {
		if supply.Model != name {
			continue
		}
		onSupply = append(onSupply, supply)
		candidates = append(candidates, pricing.Candidate{
			Model:       supply.Model,
			ChannelID:   supply.ChannelID,
			OwnerUserID: supply.OwnerUserID,
			Merchant:    supply.Merchant,
		})
	}
	if len(onSupply) == 0 {
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

	aliases, err := a.deps.catalog.UpstreamModels(ctx, name)
	if err != nil {
		a.unavailable(w, r, err, "读取模型别名失败")
		return
	}

	now := a.now().UTC()
	quotes, err := a.deps.offers.Prices(ctx, now, candidates)
	if err != nil {
		a.unavailable(w, r, err, "求报价失败")
		return
	}

	detail := newModelDetailJSON(model)
	detail.Suppliers = newSuppliers(onSupply, quotes, aliases)
	a.ok(w, http.StatusOK, modelDetailResponse{
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

// newSuppliers 把「在供渠道 + 各渠道的报价 + 别名」折成响应里的供应商列表。
//
// # 上游名怎么定
//
// 与路由的别名解析同一套优先级：渠道级别名 > 平台级别名 > 规范名本身。
// 别名按规范名一次查回（见 catalog.UpstreamModels），不按渠道往返。
//
// # 没有报价的渠道不出现
//
// 报价回答「多少钱」，一条没有适用规则的渠道给不出这个数——编一个 0 会让
// 页面显示「免费」，写 null 又会让 price 变成两种类型。所以这里只列能算出
// 价的渠道；一条都算不出来的模型由调用方看到省略的 suppliers（见
// modelDetailJSON 的注释）。
//
// # 排序与去重
//
// 按价格升序；同价平台自营在前，再按商家名、上游名。渠道 id 不对外，
// 所以「同一商家、同一上游名、同一价格」的两条渠道在响应里无法区分，
// 折成一条——否则页面上会出现两行逐字相同的报价。
func newSuppliers(supplies []channels.Supply, quotes []pricing.CandidatePrice, aliases []catalog.Alias) []supplierJSON {
	priceByChannel := make(map[int64]money.Amount, len(quotes))
	for _, quote := range quotes {
		priceByChannel[quote.ChannelID] = quote.Price
	}

	upstreamByChannel := make(map[int64]string, len(aliases))
	platformUpstream := ""
	for _, alias := range aliases {
		if alias.ChannelID == 0 {
			platformUpstream = alias.UpstreamModel
			continue
		}
		upstreamByChannel[alias.ChannelID] = alias.UpstreamModel
	}

	type supplierRow struct {
		merchant    *string
		ownerUserID int64
		upstream    string
		price       money.Amount
	}

	rows := make([]supplierRow, 0, len(supplies))
	seen := make(map[string]bool, len(supplies))
	for _, supply := range supplies {
		price, ok := priceByChannel[supply.ChannelID]
		if !ok {
			continue
		}
		upstream := upstreamByChannel[supply.ChannelID]
		if upstream == "" {
			upstream = platformUpstream
		}
		if upstream == "" {
			upstream = supply.Model
		}

		var merchant *string
		merchantKey := ""
		if supply.OwnerUserID != 0 {
			name := supply.Merchant
			merchant = &name
			merchantKey = name
		}

		key := merchantKey + "\x00" + upstream + "\x00" + price.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, supplierRow{
			merchant:    merchant,
			ownerUserID: supply.OwnerUserID,
			upstream:    upstream,
			price:       price,
		})
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if cmp := rows[i].price.Cmp(rows[j].price); cmp != 0 {
			return cmp < 0
		}
		if rows[i].ownerUserID == 0 || rows[j].ownerUserID == 0 {
			return rows[i].ownerUserID == 0 && rows[j].ownerUserID != 0
		}
		if rows[i].merchant != nil && rows[j].merchant != nil && *rows[i].merchant != *rows[j].merchant {
			return *rows[i].merchant < *rows[j].merchant
		}
		return rows[i].upstream < rows[j].upstream
	})

	out := make([]supplierJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, supplierJSON{
			Merchant:      row.merchant,
			UpstreamModel: row.upstream,
			Price:         row.price.String(),
		})
	}
	return out
}
