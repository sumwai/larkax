package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pricing"
)

// PathModels 是模型清单的入口，单条是它加 `/{id}`。
//
// 这两条是 OpenAI 兼容表面里**只读**的那一半：形状照 OpenAI 给，SDK 直接
// 能吃（见 docs/api.md 的「模型清单」一节）。它们与推理那几条路径的区别不
// 只是方法：不读请求体、不限流、不判额度、不产生用量——所以它们不经过
// admit，只共用其中的鉴权那一段（见 authenticate）。
const PathModels = "/v1/models"

// pathModelPrefix 是单条模型路径的前缀。
//
// 单独提出来而不是每处写 PathModels + "/"：两处的拼法一旦不一致，症状是
// 「清单里有、单条却 501」，而这种不一致没有任何东西会报出来。
const pathModelPrefix = PathModels + "/"

// ModelSupply 回答「哪些规范模型现在有渠道在供、分别是谁在供」。
//
// 它直接复用 internal/channels 的 Supplies：那条查询与选路共用同一处
// 谓词（suppliesModel——渠道启用、未删除、models 数组含这个规范名），本包
// 不另写一份 SQL。另写一份的那天，就是「清单里列着、一发请求却说没有可用
// 渠道」开始的那天；而那种分歧只会在调用方那里现形。
//
// 它只判**供给**：健康与额度不在这条判据里。一个连续失败的渠道仍然在供这个
// 模型，与选路之外的其余读点（控制台的模型页）同一个口径——把健康塞进来会
// 让「调得通、清单里却没有」成为另一类不一致。
type ModelSupply interface {
	Supplies(ctx context.Context) ([]channels.Supply, error)
}

// ModelOffers 给每个「模型 × 渠道」候选取一条适用的规则并求出价格。
//
// 取规则与求价的判据在 internal/pricing：本包只负责把候选凑出来、把价格
// 折成响应形状。不在这里乘一遍用量——那会让接口上的价与账单上的价成为
// 两份各自的算术（见 internal/pricing/offers.go 的包注释）。
type ModelOffers interface {
	Prices(ctx context.Context, at time.Time, candidates []pricing.Candidate) ([]pricing.CandidatePrice, error)
}

// ModelCatalog 读模型目录，这里只用它的规范名与 vendor（响应的 owned_by）。
//
// 与 ModelSupply 分成两个接口，是因为那两个方法分别属于两个包
// （internal/channels 与 internal/catalog）：合成一个就要装一个只做转发的
// 适配器，而适配器本身没有第二种行为可测。
type ModelCatalog interface {
	Models(ctx context.Context) ([]catalog.Model, error)
}

// offerJSON 是一个模型的一条报价。
//
// Merchant 为 null 表示这条报价来自平台自营渠道（channels.owner_user_id
// 为 NULL）。它不能写成空串：空串是一个字符串，客户端会把它当成一个叫作
// 空串的商家。
type offerJSON struct {
	Merchant *string `json:"merchant"`
	// Price 是 1000 个输入 token 的金额，币种沿用结算用的资产维度。
	// 字符串而不是数字：它是定点数，用 JSON 数字过一遍 float64 会在
	// 展示与对账之间引入一个没用任何东西报出来的舍入。
	Price string `json:"price"`
	// Prices 是各 token 维度的千 token 金额，键是 prompt / completion /
	// cache_read / cache_write / reasoning；只含规则写过的维度。
	//
	// 单独给一份而不只给 price：缓存读与推理的单价与输入价不是倍数关系，
	// 调用方用 price 估算一笔缓存为主的请求会差出好几倍。
	Prices map[string]string `json:"prices,omitempty"`
	// Request 是每请求的固定费用（元）。为 0 时省略这个键：规则没写固定费
	// 与写了 0 元都不收费，给一个 "0.00000000" 会被读成「确认免费」。
	Request *string `json:"request,omitempty"`
	// Minimum 是每次请求的最低消费（元）。为 0 时省略。
	Minimum *string `json:"minimum,omitempty"`
	// Units 是非 token 计量项的单价（每个计量单位多少元），键是规则里
	// 写过的计量项名。没有计量项时省略这个字段。
	Units map[string]string `json:"units,omitempty"`
}

// modelJSON 是清单里的一项，字段与 OpenAI 的 Model 对象逐字对应。
//
// Created 固定 0：目录里没有「模型创建时间」这一列，而编一个看起来像时间戳
// 的值会让调用方拿它做时间比较——那个比较没有意义，还会让人以为我们真的
// 记着这件事。
type modelJSON struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Offers 是供应这个模型的所有报价，按价格升序。
	//
	// 可以为空：清单回答「能不能用」，offers 回答「谁在供、多少钱」。一个
	// 有渠道在供、却没有任何适用规则的模型照样在清单里（它能调通，只是
	// 报价算不出来），这时省略这个字段——输出 `"offers": []` 会让客户端
	// 把它当成「问了，但没人报价」，而实际发生的是「这个模型还没有定价」。
	Offers []offerJSON `json:"offers,omitempty"`
}

// modelListJSON 是清单的响应体。
//
// Data 从空切片开始构造（见 serveModels）：目录空或无渠道在供是正常结果，
// 序列化成 `null` 会让一部分 SDK 的解码直接失败。
//
// Currency 是这里多加的一项，与 offers 一样是向后兼容的加法：OpenAI 的
// Model 列表本来没有它，不读这个字段的客户端不受影响。
type modelListJSON struct {
	Object string `json:"object"`
	// Currency 是清单里所有金额（`offers` 的 price / prices / request /
	// minimum / units）的币种。
	//
	// 单币种部署：值与控制台的 currency 同源，都是装配层从 settings 的
	// billing.default_asset 读出来的那一个（见 Handler.currency）。不给的话
	// 调用方拿到一串金额却不知道是人民币还是美元，只能去猜。
	Currency string      `json:"currency"`
	Data     []modelJSON `json:"data"`
}

// currency 是只读清单里金额的币种。
//
// 取自 Handler.DefaultAsset：装配层从 settings 读一次，这个值同时喂控制台的
// currency 与结算资产。本包不第二次读 settings——两份读取在配置改动后可能
// 短暂不一致，而「控制台说人民币、接口说美元」是一类查不出来的分歧。
//
// 空值回落到 CNY，与控制台那一处的回落相同（见 consoleapi.New）。这与结算
// 路径有意不同：那里空值会让每个请求失败，因为记错品种会写坏账；而这里只
// 是展示，两个响应在同一个装配下不该给出两种币种。
func (h *Handler) currency() string {
	if h.DefaultAsset == "" {
		return string(ledger.AssetCNY)
	}
	return h.DefaultAsset
}

// serveModels 处理 GET /v1/models。
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request) {
	if !h.admitModelRead(w, r) {
		return
	}

	items, err := h.availableModels(r.Context())
	if err != nil {
		observe.FromContext(r.Context()).Error("读取模型清单失败", "err", err)
		writeError(w, channel.ProtocolOpenAI, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, jsonBody(modelListJSON{
		Object:   "list",
		Currency: h.currency(),
		Data:     items,
	}))
}

// serveModel 处理 GET /v1/models/{id}。
//
// 不可用与不存在回同一个 404：「目录里有、但没有渠道在供」与「目录里根本
// 没有」对调用方是同一件事，都是换一个模型。分开的话，调用方就能靠两种
// 404 去分辨某个名字在不在目录里，而我们没有理由把目录内容告诉一个只是
// 写错了名字的调用方。
//
// 单条与清单走同一个构造函数：两者的对象形状必须逐字相同，各拼一份的那天
// 就是「清单里有 offers、单条却没有」开始的那天。
func (h *Handler) serveModel(w http.ResponseWriter, r *http.Request, id string) {
	if !h.admitModelRead(w, r) {
		return
	}

	items, err := h.availableModels(r.Context())
	if err != nil {
		observe.FromContext(r.Context()).Error("读取模型清单失败", "err", err)
		writeError(w, channel.ProtocolOpenAI, http.StatusInternalServerError, "internal error")
		return
	}

	for _, item := range items {
		if item.ID == id {
			writeJSON(w, http.StatusOK, jsonBody(item))
			return
		}
	}
	writeModelNotFound(w, id)
}

// admitModelRead 跑一次只读请求在写响应之前的检查：方法、装配、鉴权。
//
// # 它与 admit 有意不同
//
// 不读请求体、不限流、不判额度：这条路径只读，不产生任何用量，所以没有
// 「先冻住钱再拒掉」可言，也没有为它读进最多 maxRequestBodyBytes 的理由。
//
// # 顺序与 admit 相同
//
// 方法在前（一眼可判，而且 405 要带 Allow），装配检查在后，鉴权最后。
// 鉴权那一段与推理路径共用 authenticate：两条路径对同一把 key 的解释必须
// 一致，否则「推理请求能用、清单却说 key 无效」会成为一次查不出来的分歧。
func (h *Handler) admitModelRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		// Allow 头是 405 的必要部分，与 admit 里那条同一条理由。
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, channel.ProtocolOpenAI, http.StatusMethodNotAllowed, "method not allowed, use GET")
		return false
	}
	if !h.modelsReady() {
		// 与推理那条的 501 同一类：依赖没装配不是调用方的错，也不该在请求期
		// panic。一个 501 说明「这个构建没接上」，比一个栈回溯有用。
		writeError(w, channel.ProtocolOpenAI, http.StatusNotImplemented,
			"model listing is not wired in this build")
		return false
	}
	_, ok := h.authenticate(w, r, channel.ProtocolOpenAI)
	return ok
}

// modelsReady 报告这条路径要的协作者都装上了没有。
//
// 四份都要：Models 与 Offers 缺一条就列不出报价，而一份「只有模型名、
// 没有价」的清单会把调用方引到一个它查不到成本的模型上；Catalog 缺了
// 只能把 owned_by 一律写成 larkax，那是把一个我们不知道的事实说成确定的。
func (h *Handler) modelsReady() bool {
	return h.Auth != nil && h.Models != nil && h.Catalog != nil && h.Offers != nil
}

// availableModels 一次取回在供模型、vendor 与各自的价格。
//
// 三次读：供给（channels）、目录展示字段（catalog）、候选价格（pricing）。
// 每一份的判据都在它自己的包里，本包不拼一条把这些拼在一起的 SQL——那会
// 成为供给判据或取价判据的第二份字面量。三条查询都与模型数无关。
func (h *Handler) availableModels(ctx context.Context) ([]modelJSON, error) {
	supplies, err := h.Models.Supplies(ctx)
	if err != nil {
		return nil, err
	}

	models, err := h.Catalog.Models(ctx)
	if err != nil {
		return nil, err
	}
	vendors := make(map[string]string, len(models))
	for _, model := range models {
		vendors[model.CanonicalName] = model.Vendor
	}

	candidates := make([]pricing.Candidate, 0, len(supplies))
	order := make([]string, 0, len(supplies))
	seenModel := make(map[string]bool, len(supplies))
	for _, supply := range supplies {
		// 供给侧 JOIN 了目录，正常到不了这里；真到了说明两次读之间目录
		// 被改过。跳过而不是造一个没有展示字段的条目。
		if _, ok := vendors[supply.Model]; !ok {
			continue
		}
		if !seenModel[supply.Model] {
			seenModel[supply.Model] = true
			order = append(order, supply.Model)
		}
		candidates = append(candidates, pricing.Candidate{
			Model:       supply.Model,
			ChannelID:   supply.ChannelID,
			OwnerUserID: supply.OwnerUserID,
			Merchant:    supply.Merchant,
		})
	}

	quotes, err := h.Offers.Prices(ctx, h.now().UTC(), candidates)
	if err != nil {
		return nil, err
	}
	grouped := pricing.GroupByModel(quotes, candidates)

	items := make([]modelJSON, 0, len(order))
	for _, name := range order {
		item := newModelJSON(name, vendors[name])
		if offers := grouped[name].Offers; len(offers) > 0 {
			item.Offers = newOffersJSON(offers)
		}
		// 没有报价的模型仍然列出：它能不能用只取决于有没有渠道在供，与
		// 有没有配价无关。把「无价」实现成「不列」，调用方会以为模型不可
		// 用；而它其实能调通，只是账单上还没有价（契约用例用一条没有计价
		// 规则的渠道钉着这一点）。
		items = append(items, item)
	}
	return items, nil
}

// newOffersJSON 把折叠好的报价映射成响应形状。
func newOffersJSON(offers []pricing.Offer) []offerJSON {
	out := make([]offerJSON, 0, len(offers))
	for _, offer := range offers {
		out = append(out, offerJSON{
			Merchant: offer.Merchant,
			Price:    offer.Price.String(),
			Prices:   dimensionPricesJSON(offer.Prices),
			Request:  amountOrNil(offer.Request),
			Minimum:  amountOrNil(offer.Minimum),
			Units:    dimensionPricesJSON(offer.Units),
		})
	}
	return out
}

// amountOrNil 把 0 金额编码成「省略这个键」：固定费与最低消费的零值同时
// 表示「规则没写」与「写了 0 元」，两者都是一分钱不收，省略比给一个
// "0.00000000" 更少被误读成「确认免费」。
func amountOrNil(amount money.Amount) *string {
	if amount == money.Zero {
		return nil
	}
	value := amount.String()
	return &value
}

// dimensionPricesJSON 把维度价映射成响应里的字符串 map。
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

// newModelJSON 把一条规范模型映射成 OpenAI 的 Model 对象。
//
// 没填 vendor 时用 larkax：这个字段在 OpenAI 的形状里不是可空的，回空串
// 等于让调用方拿一个空值去做归属判断。
func newModelJSON(name, vendor string) modelJSON {
	if vendor == "" {
		vendor = "larkax"
	}
	return modelJSON{ID: name, Object: "model", Created: 0, OwnedBy: vendor}
}

// writeModelNotFound 写出「这个模型现在不可用」的 404。
//
// 码用 OpenAI 的 model_not_found：SDK 按码分支，而它比一句光秃秃的 404 更能
// 说清处置——换一个模型，重试不会有用。
func writeModelNotFound(w http.ResponseWriter, id string) {
	writeErrorCode(w, channel.ProtocolOpenAI, http.StatusNotFound, "model_not_found",
		fmt.Sprintf("the model %q is not available on this gateway", id))
}

// modelIDFromPath 从 /v1/models/{id} 里取出 id。
//
// 规范名里可能有 "/"（目录按 `vendor/model` 这种写法收集过），所以这里**不按
// 段数判**：只截掉前缀，剩下的整段就是 id。按段数判会把一部分合法名字判成
// 未知路径，而那种表现是 501——比 404 难排查得多。
//
// 前缀之后为空（`/v1/models/`）仍然算这条路径，id 是空串，于是落到 404：
// 这条路径**已经实现**，只是没有这个模型。落回未知路径那一支会给一个
// 「未实现」，而那句话在这里是错的，会把人引向「这个部署没接模型清单」。
//
// 不做反转义：r.URL.Path 已经解过一次码，再解一次会把名字里的 `%2F` 变成
// 一个真的分隔符。
func modelIDFromPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, pathModelPrefix)
	if !ok {
		return "", false
	}
	return rest, true
}
