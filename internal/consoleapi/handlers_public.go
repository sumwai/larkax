package consoleapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sumwai/larkax/internal/catalog"
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
// 缓存两项可为 null：规则里没声明就是「没配」，与 0（免费）不是一回事。
type pricingJSON struct {
	InputPerMTok      string  `json:"input_per_mtok"`
	OutputPerMTok     string  `json:"output_per_mtok"`
	CacheReadPerMTok  *string `json:"cache_read_per_mtok"`
	CacheWritePerMTok *string `json:"cache_write_per_mtok"`
}

// modelJSON 是模型清单里的一项。
type modelJSON struct {
	Name         string          `json:"name"`
	DisplayName  string          `json:"display_name"`
	Vendor       string          `json:"vendor"`
	ContextLimit int             `json:"context_limit"`
	MaxOutput    int             `json:"max_output"`
	Capabilities map[string]bool `json:"capabilities"`
	Modalities   modalitiesJSON  `json:"modalities"`
	// Pricing 为 nil 表示没有平台规则——界面显示「—」，不伪造一个起价。
	Pricing *pricingJSON `json:"pricing"`
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

	items := make([]modelJSON, 0, len(names))
	for _, name := range names {
		model, ok := byName[name]
		if !ok {
			// 供应侧只 JOIN 了 model_catalog，正常到不了这里；真到了说明两份
			// 读之间目录被改过。跳过而不是造一个空模型：一个没有展示字段的
			// 条目会让前端显示一行空白。
			continue
		}
		items = append(items, newModelJSON(model, prices[name]))
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
func newModelJSON(model catalog.Model, price *pricing.ReferencePrice) modelJSON {
	out := modelJSON{
		Name:         model.CanonicalName,
		DisplayName:  model.DisplayName,
		Vendor:       model.Vendor,
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
	if price != nil {
		out.Pricing = &pricingJSON{
			InputPerMTok:  price.InputPerMTok.String(),
			OutputPerMTok: price.OutputPerMTok.String(),
		}
		if price.CacheReadPerMTok != nil {
			value := price.CacheReadPerMTok.String()
			out.Pricing.CacheReadPerMTok = &value
		}
		if price.CacheWritePerMTok != nil {
			value := price.CacheWritePerMTok.String()
			out.Pricing.CacheWritePerMTok = &value
		}
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
