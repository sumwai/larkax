package adminapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是各资源的请求/响应形状与处理函数。每个处理函数只做三件事：解请求、
// 调对应的 admin 类型、把结果翻成 JSON。写入语义与校验全部留在那些类型里。
//
// # 字段名用 snake_case
//
// 与 CLI 的 --flag 一一对应（base-url ↔ base_url），两边读的是同一组列。
// 这里不直接复用各 admin 类型的请求结构体：它们的字段没有 json tag，序列化
// 出来是 Go 名字；而且「没提到这个字段」与「值给了零」在这两种场合的表示
// 也不一样（CLI 用 flag 的 Changed，HTTP 用指针）。

// ── 渠道 ────────────────────────────────────────────────────────────────

type channelCreateRequest struct {
	OwnerUserID int64          `json:"owner_user_id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	BaseURL     string         `json:"base_url"`
	Credential  string         `json:"credential"`
	Models      []string       `json:"models"`
	Weight      int            `json:"weight"`
	Priority    int            `json:"priority"`
	Setting     map[string]any `json:"setting"`
}

type channelUpdateRequest struct {
	Name       *string         `json:"name"`
	Type       *string         `json:"type"`
	BaseURL    *string         `json:"base_url"`
	Credential *string         `json:"credential"`
	Models     *[]string       `json:"models"`
	Weight     *int            `json:"weight"`
	Priority   *int            `json:"priority"`
	Setting    *map[string]any `json:"setting"`
	Status     *string         `json:"status"`
}

type channelView struct {
	ID             int64          `json:"id"`
	OwnerUserID    int64          `json:"owner_user_id"`
	Name           string         `json:"name"`
	Type           string         `json:"type"`
	BaseURL        string         `json:"base_url"`
	Status         string         `json:"status"`
	Models         []string       `json:"models"`
	Weight         int            `json:"weight"`
	Priority       int            `json:"priority"`
	Setting        map[string]any `json:"setting"`
	CredentialKeys []string       `json:"credential_keys"`
	CredentialHint string         `json:"credential_hint"`
	CredentialErr  string         `json:"credential_error,omitempty"`
	DeletedAt      *time.Time     `json:"deleted_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

func channelViewOf(info channels.ChannelInfo) channelView {
	view := channelView{
		ID:             info.ID,
		OwnerUserID:    info.OwnerUserID,
		Name:           info.Name,
		Type:           info.Type,
		BaseURL:        info.BaseURL,
		Status:         info.Status,
		Models:         info.Models,
		Weight:         info.Weight,
		Priority:       info.Priority,
		Setting:        info.Setting,
		CredentialKeys: info.CredentialKeys,
		CredentialHint: info.Hint,
		CreatedAt:      info.CreatedAt,
		UpdatedAt:      info.UpdatedAt,
	}
	if info.CredentialErr != nil {
		view.CredentialErr = info.CredentialErr.Error()
	}
	if !info.DeletedAt.IsZero() {
		deletedAt := info.DeletedAt
		view.DeletedAt = &deletedAt
	}
	return view
}

func (a *API) listChannels(w http.ResponseWriter, r *http.Request) {
	ownerID, scoped := OwnerFromContext(r.Context())
	var (
		infos []channels.ChannelInfo
		err   error
	)
	if scoped {
		infos, err = a.channels.ListOwned(r.Context(), ownerID)
	} else {
		infos, err = a.channels.List(r.Context())
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	views := make([]channelView, 0, len(infos))
	for _, info := range infos {
		views = append(views, channelViewOf(info))
	}
	a.writeJSON(w, http.StatusOK, views)
}

func (a *API) getChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ownerID, scoped := OwnerFromContext(r.Context())
	var info channels.ChannelInfo
	if scoped {
		info, err = a.channels.GetOwned(r.Context(), ownerID, id)
	} else {
		info, err = a.channels.Get(r.Context(), id)
	}
	if err != nil {
		a.failScoped(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, channelViewOf(info))
}

func (a *API) createChannel(w http.ResponseWriter, r *http.Request) {
	var req channelCreateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	// merchant 建立渠道时归属就是他自己：请求体里的 owner_user_id 被覆盖，
	// 而不是「校验它等于自己」。后者要在两处维护同一句判断，且请求体里本来
	// 就不该出现这个字段——归属是服务端从会话里取的。
	if ownerID, scoped := OwnerFromContext(r.Context()); scoped {
		req.OwnerUserID = ownerID
	}
	id, err := a.channels.Create(r.Context(), channels.CreateRequest{
		OwnerUserID: req.OwnerUserID,
		Name:        req.Name,
		Type:        req.Type,
		BaseURL:     req.BaseURL,
		Credential:  req.Credential,
		Models:      req.Models,
		Weight:      req.Weight,
		Priority:    req.Priority,
		Setting:     req.Setting,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (a *API) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req channelUpdateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	updateReq := channels.UpdateRequest{
		ID:         id,
		Name:       req.Name,
		Type:       req.Type,
		BaseURL:    req.BaseURL,
		Models:     req.Models,
		Weight:     req.Weight,
		Priority:   req.Priority,
		Setting:    req.Setting,
		Credential: req.Credential,
		Status:     req.Status,
	}
	if ownerID, scoped := OwnerFromContext(r.Context()); scoped {
		err = a.channels.UpdateOwned(r.Context(), ownerID, updateReq)
	} else {
		err = a.channels.Update(r.Context(), updateReq)
	}
	if err != nil {
		a.failScoped(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── 模型目录 ─────────────────────────────────────────────────────────────

// capabilitiesRequest 是那六个能力开关与端点清单。
//
// 每一项都是指针：nil 表示「未知」（库里是 NULL），指针指向 false 表示
// 「明确不支持」。端点用 *[]string 是为了保住同一组三态：nil = 未知、
// `[]` = 一条都不支持、非空 = 这几条。
type capabilitiesRequest struct {
	Reasoning *bool     `json:"reasoning"`
	Tools     *bool     `json:"tools"`
	Vision    *bool     `json:"vision"`
	Audio     *bool     `json:"audio"`
	Cache     *bool     `json:"cache"`
	Batch     *bool     `json:"batch"`
	Endpoints *[]string `json:"endpoints"`
}

func (c *capabilitiesRequest) toCatalog() (catalog.Capabilities, error) {
	out := catalog.Capabilities{
		Reasoning: c.Reasoning,
		Tools:     c.Tools,
		Vision:    c.Vision,
		Audio:     c.Audio,
		Cache:     c.Cache,
		Batch:     c.Batch,
	}
	if c.Endpoints != nil {
		endpoints := make([]channel.Endpoint, 0, len(*c.Endpoints))
		for _, raw := range *c.Endpoints {
			endpoint, err := channel.ParseEndpoint(raw)
			if err != nil {
				return catalog.Capabilities{}, err
			}
			endpoints = append(endpoints, endpoint)
		}
		out.Endpoints = endpoints
	}
	return out, nil
}

type modelCreateRequest struct {
	CanonicalName    string              `json:"canonical_name"`
	DisplayName      string              `json:"display_name"`
	Vendor           string              `json:"vendor"`
	Family           string              `json:"family"`
	ContextLimit     int                 `json:"context_limit"`
	InputLimit       int                 `json:"input_limit"`
	OutputLimit      int                 `json:"output_limit"`
	Capabilities     capabilitiesRequest `json:"capabilities"`
	ModalitiesInput  []string            `json:"modalities_input"`
	ModalitiesOutput []string            `json:"modalities_output"`
	ReasoningOptions []any               `json:"reasoning_options"`
	CanonicalModelID string              `json:"canonical_model_id"`
	KnowledgeCutoff  *string             `json:"knowledge_cutoff"`
	ReleaseDate      *string             `json:"release_date"`
	Raw              map[string]any      `json:"raw"`
}

type modelUpdateRequest struct {
	DisplayName      *string              `json:"display_name"`
	Vendor           *string              `json:"vendor"`
	Family           *string              `json:"family"`
	ContextLimit     *int                 `json:"context_limit"`
	InputLimit       *int                 `json:"input_limit"`
	OutputLimit      *int                 `json:"output_limit"`
	Capabilities     *capabilitiesRequest `json:"capabilities"`
	ModalitiesInput  *[]string            `json:"modalities_input"`
	ModalitiesOutput *[]string            `json:"modalities_output"`
	ReasoningOptions *[]any               `json:"reasoning_options"`
	CanonicalModelID *string              `json:"canonical_model_id"`
	// KnowledgeCutoff 与 ReleaseDate：nil 表示不改，空串表示清空。
	KnowledgeCutoff *string         `json:"knowledge_cutoff"`
	ReleaseDate     *string         `json:"release_date"`
	Raw             *map[string]any `json:"raw"`
}

type capabilitiesView struct {
	Reasoning *bool    `json:"reasoning"`
	Tools     *bool    `json:"tools"`
	Vision    *bool    `json:"vision"`
	Audio     *bool    `json:"audio"`
	Cache     *bool    `json:"cache"`
	Batch     *bool    `json:"batch"`
	Endpoints []string `json:"endpoints"`
}

type modelView struct {
	ID               int64            `json:"id"`
	CanonicalName    string           `json:"canonical_name"`
	DisplayName      string           `json:"display_name"`
	Vendor           string           `json:"vendor"`
	Family           string           `json:"family"`
	ContextLimit     int              `json:"context_limit"`
	InputLimit       int              `json:"input_limit"`
	OutputLimit      int              `json:"output_limit"`
	Capabilities     capabilitiesView `json:"capabilities"`
	ModalitiesInput  []string         `json:"modalities_input"`
	ModalitiesOutput []string         `json:"modalities_output"`
	ReasoningOptions []any            `json:"reasoning_options"`
	CanonicalModelID string           `json:"canonical_model_id"`
	KnowledgeCutoff  *string          `json:"knowledge_cutoff"`
	ReleaseDate      *string          `json:"release_date"`
	Raw              map[string]any   `json:"raw"`
	UpdatedAt        time.Time        `json:"updated_at"`
}

func modelViewOf(model catalog.Model) modelView {
	endpoints := make([]string, 0, len(model.Capabilities.Endpoints))
	for _, endpoint := range model.Capabilities.Endpoints {
		endpoints = append(endpoints, string(endpoint))
	}
	view := modelView{
		ID:            model.ID,
		CanonicalName: model.CanonicalName,
		DisplayName:   model.DisplayName,
		Vendor:        model.Vendor,
		Family:        model.Family,
		ContextLimit:  model.ContextLimit,
		InputLimit:    model.InputLimit,
		OutputLimit:   model.OutputLimit,
		Capabilities: capabilitiesView{
			Reasoning: model.Capabilities.Reasoning,
			Tools:     model.Capabilities.Tools,
			Vision:    model.Capabilities.Vision,
			Audio:     model.Capabilities.Audio,
			Cache:     model.Capabilities.Cache,
			Batch:     model.Capabilities.Batch,
		},
		ModalitiesInput:  model.ModalitiesInput,
		ModalitiesOutput: model.ModalitiesOutput,
		ReasoningOptions: model.ReasoningOptions,
		CanonicalModelID: model.CanonicalModelID,
		KnowledgeCutoff:  dateString(model.KnowledgeCutoff),
		ReleaseDate:      dateString(model.ReleaseDate),
		Raw:              model.Raw,
		UpdatedAt:        model.UpdatedAt,
	}
	// 端点那一列的三态要在响应里保住：NULL 读出来是 nil 切片，编码成 null；
	// [] 编码成 []。用 make 造出来的空切片会把前者变成后者。
	if model.Capabilities.Endpoints != nil {
		view.Capabilities.Endpoints = endpoints
	}
	return view
}

// dateString 把一个可空日期编码成 YYYY-MM-DD；nil 保持 nil（JSON null）。
func dateString(value *time.Time) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	formatted := value.Format("2006-01-02")
	return &formatted
}

// parseDate 解一个 YYYY-MM-DD 的日期。空串与 nil 都回 nil（这一栏没有值）。
//
// nil, nil 是结论而不是「出错了但没报」：没有给日期与给了一个坏日期是两件事，
// 前者合法。调用方看第一个返回值判断有没有值。
func parseDate(field string, raw *string) (*time.Time, error) {
	if raw == nil || *raw == "" {
		return nil, nil //nolint:nilnil // 见上：没有日期是合法输入
	}
	parsed, err := time.Parse("2006-01-02", *raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be YYYY-MM-DD: %w", field, err)
	}
	return &parsed, nil
}

func (a *API) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := a.catalog.Models(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	views := make([]modelView, 0, len(models))
	for _, model := range models {
		views = append(views, modelViewOf(model))
	}
	a.writeJSON(w, http.StatusOK, views)
}

func (a *API) createModel(w http.ResponseWriter, r *http.Request) {
	var req modelCreateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	capabilities, err := req.Capabilities.toCatalog()
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cutoff, err := parseDate("knowledge_cutoff", req.KnowledgeCutoff)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	release, err := parseDate("release_date", req.ReleaseDate)
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id, err := a.catalogAdmin.Create(r.Context(), catalog.CreateModelRequest{
		CanonicalName:    req.CanonicalName,
		DisplayName:      req.DisplayName,
		Vendor:           req.Vendor,
		Family:           req.Family,
		ContextLimit:     req.ContextLimit,
		InputLimit:       req.InputLimit,
		OutputLimit:      req.OutputLimit,
		Capabilities:     capabilities,
		ModalitiesInput:  req.ModalitiesInput,
		ModalitiesOutput: req.ModalitiesOutput,
		ReasoningOptions: req.ReasoningOptions,
		CanonicalModelID: req.CanonicalModelID,
		KnowledgeCutoff:  cutoff,
		ReleaseDate:      release,
		Raw:              req.Raw,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (a *API) updateModel(w http.ResponseWriter, r *http.Request) {
	var req modelUpdateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}

	update := catalog.UpdateModelRequest{
		CanonicalName:    r.PathValue("name"),
		DisplayName:      req.DisplayName,
		Vendor:           req.Vendor,
		Family:           req.Family,
		ContextLimit:     req.ContextLimit,
		InputLimit:       req.InputLimit,
		OutputLimit:      req.OutputLimit,
		ModalitiesInput:  req.ModalitiesInput,
		ModalitiesOutput: req.ModalitiesOutput,
		ReasoningOptions: req.ReasoningOptions,
		CanonicalModelID: req.CanonicalModelID,
		Raw:              req.Raw,
	}

	if req.Capabilities != nil {
		capabilities, err := req.Capabilities.toCatalog()
		if err != nil {
			a.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		update.Capabilities = &capabilities
	}

	// 「不改」是 nil，「清空」是空串。catalog 用指向零值的指针表示清空
	// （见 UpdateModelRequest 的注释），所以这里要把空串翻成零值时刻。
	if req.KnowledgeCutoff != nil {
		cutoff, err := parseDate("knowledge_cutoff", req.KnowledgeCutoff)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		cleared := time.Time{}
		if cutoff != nil {
			cleared = *cutoff
		}
		update.KnowledgeCutoff = &cleared
	}
	if req.ReleaseDate != nil {
		release, err := parseDate("release_date", req.ReleaseDate)
		if err != nil {
			a.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		cleared := time.Time{}
		if release != nil {
			cleared = *release
		}
		update.ReleaseDate = &cleared
	}

	if err := a.catalogAdmin.Update(r.Context(), update); err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) deleteModel(w http.ResponseWriter, r *http.Request) {
	if err := a.catalogAdmin.Delete(r.Context(), r.PathValue("name")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── 别名 ─────────────────────────────────────────────────────────────────

type aliasCreateRequest struct {
	ChannelID     int64          `json:"channel_id"`
	UpstreamModel string         `json:"upstream_model"`
	CanonicalName string         `json:"canonical_name"`
	Overrides     map[string]any `json:"overrides"`
}

type aliasUpdateRequest struct {
	UpstreamModel *string         `json:"upstream_model"`
	Overrides     *map[string]any `json:"overrides"`
}

type aliasView struct {
	ID            int64          `json:"id"`
	ChannelID     int64          `json:"channel_id"`
	UpstreamModel string         `json:"upstream_model"`
	CanonicalName string         `json:"canonical_name"`
	Overrides     map[string]any `json:"overrides"`
	CreatedAt     time.Time      `json:"created_at"`
}

func aliasViewOf(alias catalog.Alias) aliasView {
	return aliasView{
		ID:            alias.ID,
		ChannelID:     alias.ChannelID,
		UpstreamModel: alias.UpstreamModel,
		CanonicalName: alias.CanonicalName,
		Overrides:     alias.Overrides,
		CreatedAt:     alias.CreatedAt,
	}
}

func (a *API) listAliases(w http.ResponseWriter, r *http.Request) {
	channelID, err := queryID(r, "channel_id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	aliases, err := a.catalog.Aliases(r.Context(), channelID)
	if err != nil {
		a.fail(w, err)
		return
	}
	views := make([]aliasView, 0, len(aliases))
	for _, alias := range aliases {
		views = append(views, aliasViewOf(alias))
	}
	a.writeJSON(w, http.StatusOK, views)
}

func (a *API) createAlias(w http.ResponseWriter, r *http.Request) {
	var req aliasCreateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	id, err := a.catalogAdmin.CreateAlias(r.Context(), catalog.CreateAliasRequest{
		ChannelID:     req.ChannelID,
		UpstreamModel: req.UpstreamModel,
		CanonicalName: req.CanonicalName,
		Overrides:     req.Overrides,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (a *API) updateAlias(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req aliasUpdateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	err = a.catalogAdmin.UpdateAlias(r.Context(), catalog.UpdateAliasRequest{
		ID:            id,
		UpstreamModel: req.UpstreamModel,
		Overrides:     req.Overrides,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) deleteAlias(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.catalogAdmin.DeleteAlias(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── 售价规则 ─────────────────────────────────────────────────────────────

type ruleCreateRequest struct {
	Scope         string     `json:"scope"`
	OwnerUserID   int64      `json:"owner_user_id"`
	ChannelID     int64      `json:"channel_id"`
	Name          string     `json:"name"`
	ModelPatterns []string   `json:"model_patterns"`
	Expr          string     `json:"expr"`
	Priority      int        `json:"priority"`
	EffectiveFrom *time.Time `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	Enabled       *bool      `json:"enabled"`
	CreatedBy     int64      `json:"created_by"`
}

type ruleUpdateRequest struct {
	Scope            *string    `json:"scope"`
	OwnerUserID      *int64     `json:"owner_user_id"`
	ChannelID        *int64     `json:"channel_id"`
	Name             *string    `json:"name"`
	ModelPatterns    *[]string  `json:"model_patterns"`
	Expr             *string    `json:"expr"`
	Priority         *int       `json:"priority"`
	EffectiveFrom    *time.Time `json:"effective_from"`
	EffectiveTo      *time.Time `json:"effective_to"`
	ClearEffectiveTo bool       `json:"clear_effective_to"`
	Enabled          *bool      `json:"enabled"`
	ChangedBy        int64      `json:"changed_by"`
}

type ruleView struct {
	ID            int64      `json:"id"`
	Scope         string     `json:"scope"`
	OwnerUserID   int64      `json:"owner_user_id"`
	ChannelID     int64      `json:"channel_id"`
	Name          string     `json:"name"`
	ModelPatterns []string   `json:"model_patterns"`
	Expr          string     `json:"expr"`
	Priority      int        `json:"priority"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	Enabled       bool       `json:"enabled"`
	CreatedBy     int64      `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeclaredKeys  []string   `json:"declared_keys"`
	ExprErr       string     `json:"expr_error,omitempty"`
	ScheduleErr   string     `json:"schedule_error,omitempty"`
}

func ruleViewOf(info pricing.RuleInfo) ruleView {
	view := ruleView{
		ID:            info.ID,
		Scope:         info.Scope,
		OwnerUserID:   info.OwnerUserID,
		ChannelID:     info.ChannelID,
		Name:          info.Name,
		ModelPatterns: info.ModelPatterns,
		Expr:          info.Expr,
		Priority:      info.Priority,
		EffectiveFrom: info.EffectiveFrom,
		EffectiveTo:   info.EffectiveTo,
		Enabled:       info.Enabled,
		CreatedBy:     info.CreatedBy,
		CreatedAt:     info.CreatedAt,
		UpdatedAt:     info.UpdatedAt,
		DeclaredKeys:  info.DeclaredKeys,
	}
	if info.ExprErr != nil {
		view.ExprErr = info.ExprErr.Error()
	}
	if info.ScheduleErr != nil {
		view.ScheduleErr = info.ScheduleErr.Error()
	}
	return view
}

func (a *API) listRules(w http.ResponseWriter, r *http.Request) {
	ownerID, scoped := OwnerFromContext(r.Context())
	var (
		rules []pricing.RuleInfo
		err   error
	)
	if scoped {
		rules, err = a.rules.ListOwned(r.Context(), ownerID)
	} else {
		rules, err = a.rules.List(r.Context())
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	views := make([]ruleView, 0, len(rules))
	for _, rule := range rules {
		views = append(views, ruleViewOf(rule))
	}
	a.writeJSON(w, http.StatusOK, views)
}

func (a *API) createRule(w http.ResponseWriter, r *http.Request) {
	var req ruleCreateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	// 商家建规则时作用域与归属由会话决定：请求体里写 platform 或别人的
	// owner 都不作数。与渠道的归属同一做法——归属是服务端的事实，不是
	// 客户端提交的一项。
	if ownerID, scoped := OwnerFromContext(r.Context()); scoped {
		req.Scope = pricing.ScopeMerchant
		req.OwnerUserID = ownerID
		req.ChannelID = 0
		req.CreatedBy = ownerID
	}
	var from time.Time
	if req.EffectiveFrom != nil {
		from = *req.EffectiveFrom
	}
	id, err := a.rules.Create(r.Context(), pricing.CreateRequest{
		Scope:         req.Scope,
		OwnerUserID:   req.OwnerUserID,
		ChannelID:     req.ChannelID,
		Name:          req.Name,
		ModelPatterns: req.ModelPatterns,
		Expr:          req.Expr,
		Priority:      req.Priority,
		EffectiveFrom: from,
		EffectiveTo:   req.EffectiveTo,
		Enabled:       req.Enabled,
		CreatedBy:     req.CreatedBy,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (a *API) getRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ownerID, scoped := OwnerFromContext(r.Context())
	var info pricing.RuleInfo
	if scoped {
		info, err = a.rules.GetOwned(r.Context(), ownerID, id)
	} else {
		info, err = a.rules.Get(r.Context(), id)
	}
	if err != nil {
		a.failScoped(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, ruleViewOf(info))
}

func (a *API) updateRule(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req ruleUpdateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	ownerID, scoped := OwnerFromContext(r.Context())
	updateReq := pricing.UpdateRequest{
		ID:               id,
		Scope:            req.Scope,
		OwnerUserID:      req.OwnerUserID,
		ChannelID:        req.ChannelID,
		Name:             req.Name,
		ModelPatterns:    req.ModelPatterns,
		Expr:             req.Expr,
		Priority:         req.Priority,
		EffectiveFrom:    req.EffectiveFrom,
		EffectiveTo:      req.EffectiveTo,
		ClearEffectiveTo: req.ClearEffectiveTo,
		Enabled:          req.Enabled,
		ChangedBy:        req.ChangedBy,
	}
	if scoped {
		// 改价的人就是会话用户：请求体里的 changed_by 不作数。
		updateReq.ChangedBy = ownerID
		err = a.rules.UpdateOwned(r.Context(), ownerID, updateReq)
	} else {
		err = a.rules.Update(r.Context(), updateReq)
	}
	if err != nil {
		a.failScoped(w, r, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── 成本规则 ─────────────────────────────────────────────────────────────

type costCreateRequest struct {
	ChannelID     int64      `json:"channel_id"`
	ModelPatterns []string   `json:"model_patterns"`
	Expr          string     `json:"expr"`
	Priority      int        `json:"priority"`
	EffectiveFrom *time.Time `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
}

type costUpdateRequest struct {
	ChannelID        *int64     `json:"channel_id"`
	ModelPatterns    *[]string  `json:"model_patterns"`
	Expr             *string    `json:"expr"`
	Priority         *int       `json:"priority"`
	EffectiveFrom    *time.Time `json:"effective_from"`
	EffectiveTo      *time.Time `json:"effective_to"`
	ClearEffectiveTo bool       `json:"clear_effective_to"`
}

type costView struct {
	ID            int64      `json:"id"`
	ChannelID     int64      `json:"channel_id"`
	ModelPatterns []string   `json:"model_patterns"`
	Expr          string     `json:"expr"`
	Priority      int        `json:"priority"`
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
	CreatedAt     time.Time  `json:"created_at"`
	DeclaredKeys  []string   `json:"declared_keys"`
	ExprErr       string     `json:"expr_error,omitempty"`
}

func costViewOf(info pricing.CostInfo) costView {
	view := costView{
		ID:            info.ID,
		ChannelID:     info.ChannelID,
		ModelPatterns: info.ModelPatterns,
		Expr:          info.Expr,
		Priority:      info.Priority,
		EffectiveFrom: info.EffectiveFrom,
		EffectiveTo:   info.EffectiveTo,
		CreatedAt:     info.CreatedAt,
		DeclaredKeys:  info.DeclaredKeys,
	}
	if info.ExprErr != nil {
		view.ExprErr = info.ExprErr.Error()
	}
	return view
}

func (a *API) listCosts(w http.ResponseWriter, r *http.Request) {
	costs, err := a.costs.List(r.Context(), pricing.CostListQuery{})
	if err != nil {
		a.fail(w, err)
		return
	}
	views := make([]costView, 0, len(costs))
	for _, cost := range costs {
		views = append(views, costViewOf(cost))
	}
	a.writeJSON(w, http.StatusOK, views)
}

func (a *API) createCost(w http.ResponseWriter, r *http.Request) {
	var req costCreateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	var from time.Time
	if req.EffectiveFrom != nil {
		from = *req.EffectiveFrom
	}
	id, err := a.costs.Create(r.Context(), pricing.CostCreateRequest{
		ChannelID:     req.ChannelID,
		ModelPatterns: req.ModelPatterns,
		Expr:          req.Expr,
		Priority:      req.Priority,
		EffectiveFrom: from,
		EffectiveTo:   req.EffectiveTo,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (a *API) updateCost(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		a.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req costUpdateRequest
	if !a.decodeBody(w, r, &req) {
		return
	}
	err = a.costs.Update(r.Context(), pricing.CostUpdateRequest{
		ID:               id,
		ChannelID:        req.ChannelID,
		ModelPatterns:    req.ModelPatterns,
		Expr:             req.Expr,
		Priority:         req.Priority,
		EffectiveFrom:    req.EffectiveFrom,
		EffectiveTo:      req.EffectiveTo,
		ClearEffectiveTo: req.ClearEffectiveTo,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
