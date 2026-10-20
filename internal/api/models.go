package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
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

// ModelSupply 回答「哪些规范模型现在有渠道在供」。
//
// 它直接复用 internal/channels 的 SuppliedModels：那条查询与选路共用同一处
// 谓词（suppliesModel——渠道启用、未删除、models 数组含这个规范名），本包
// 不另写一份 SQL。另写一份的那天，就是「清单里列着、一发请求却说没有可用
// 渠道」开始的那天；而那种分歧只会在调用方那里现形。
//
// 它只判**供给**：健康与额度不在这条判据里。一个连续失败的渠道仍然在供这个
// 模型，与选路之外的其余读点（控制台的模型页）同一个口径——把健康塞进来会
// 让「调得通、清单里却没有」成为另一类不一致。
type ModelSupply interface {
	SuppliedModels(ctx context.Context) ([]string, error)
}

// ModelCatalog 读模型目录，这里只用它的规范名与 vendor（响应的 owned_by）。
//
// 与 ModelSupply 分成两个接口，是因为那两个方法分别属于两个包
// （internal/channels 与 internal/catalog）：合成一个就要装一个只做转发的
// 适配器，而适配器本身没有第二种行为可测。
type ModelCatalog interface {
	Models(ctx context.Context) ([]catalog.Model, error)
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
}

// modelListJSON 是清单的响应体。
//
// Data 从空切片开始构造（见 serveModels）：目录空或无渠道在供是正常结果，
// 序列化成 `null` 会让一部分 SDK 的解码直接失败。
type modelListJSON struct {
	Object string      `json:"object"`
	Data   []modelJSON `json:"data"`
}

// serveModels 处理 GET /v1/models。
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request) {
	if !h.admitModelRead(w, r) {
		return
	}

	names, vendors, err := h.availableModels(r.Context())
	if err != nil {
		observe.FromContext(r.Context()).Error("读取模型清单失败", "err", err)
		writeError(w, channel.ProtocolOpenAI, http.StatusInternalServerError, "internal error")
		return
	}

	items := make([]modelJSON, 0, len(names))
	for _, name := range names {
		items = append(items, newModelJSON(name, vendors[name]))
	}
	writeJSON(w, http.StatusOK, jsonBody(modelListJSON{Object: "list", Data: items}))
}

// serveModel 处理 GET /v1/models/{id}。
//
// 不可用与不存在回同一个 404：「目录里有、但没有渠道在供」与「目录里根本
// 没有」对调用方是同一件事，都是换一个模型。分开的话，调用方就能靠两种
// 404 去分辨某个名字在不在目录里，而我们没有理由把目录内容告诉一个只是
// 写错了名字的调用方。
func (h *Handler) serveModel(w http.ResponseWriter, r *http.Request, id string) {
	if !h.admitModelRead(w, r) {
		return
	}

	names, vendors, err := h.availableModels(r.Context())
	if err != nil {
		observe.FromContext(r.Context()).Error("读取模型清单失败", "err", err)
		writeError(w, channel.ProtocolOpenAI, http.StatusInternalServerError, "internal error")
		return
	}

	if !slices.Contains(names, id) {
		writeModelNotFound(w, id)
		return
	}
	writeJSON(w, http.StatusOK, jsonBody(newModelJSON(id, vendors[id])))
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
// 三份都要，包括 Catalog：缺它就只能把 owned_by 一律写成 larkax，而那是把
// 一个我们不知道的事实说成确定的——与「目录里那一列本来就是空的，用 larkax
// 补上」不是一回事（后者见 newModelJSON）。
func (h *Handler) modelsReady() bool {
	return h.Auth != nil && h.Models != nil && h.Catalog != nil
}

// availableModels 一次取回可用模型名与它们的 vendor。
//
// 两次读而不是一条 JOIN：供给的判据属于 internal/channels，目录的展示字段属于
// internal/catalog，本包不把两者拼成第三条 SQL——那条 SQL 会成为供给判据的
// 第二份字面量。清单规模小（这条路不做分页），两次读的代价可以接受。
func (h *Handler) availableModels(ctx context.Context) ([]string, map[string]string, error) {
	names, err := h.Models.SuppliedModels(ctx)
	if err != nil {
		return nil, nil, err
	}

	models, err := h.Catalog.Models(ctx)
	if err != nil {
		return nil, nil, err
	}
	vendors := make(map[string]string, len(models))
	for _, model := range models {
		vendors[model.CanonicalName] = model.Vendor
	}
	return names, vendors, nil
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
