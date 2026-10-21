// Package adminapi 是内部监听上的管理面：一组调用现有 admin 类型的薄 HTTP
// 端点，外加它自己的鉴权。
//
// # 为什么单独一套凭证
//
// 公共监听用的是用户 API Key，它的语义是「以某个用户的名义发推理请求」。
// 管理面改的是渠道、模型目录与计价规则——权限范围与计费身份都不是同一件事。
// 复用用户 Key 会让「一把能发请求的 Key」与「能改价的 Key」在库里长得一样，
// 而这两者该由不同的人持有、按不同的节奏轮换。所以这里是一套独立的令牌。
//
// # 它不是「私有网络即可信」
//
// 内部监听确实绑在私有网络上，但鉴权**不依赖网络位置**：缺令牌或令牌不对
// 一律 401，没配置令牌一律 503。网络边界会随部署变化（端口被转发、sidecar
// 暴露、误配 ingress），把安全性质押在它上面，出事时没有任何一处会报错。
//
// # 它只是薄层
//
// 端点不写 SQL、不改写入语义：渠道走 channels.Admin，模型与别名走
// catalog.Admin，售价与成本走 pricing.Admin / pricing.CostAdmin。同一套写入
// 逻辑被 larkaxctl 与这里共用，「CLI 能写的字段、API 写不了」于是不存在。
package adminapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/api"
	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/pricing"
)

// PathPrefix 是管理面在内部监听上的挂载点。所有管理端点都在它下面，鉴权
// 中间件也只包住这一片。
//
// 导出是给装配层用的（见 cmd/larkax）：挂载点与路由表各写一份字面量时，改
// 一处就是「路由都在、却一条也到不了」，而那种错在单测里两边用同一个常量
// 配替身时抓不到。
const PathPrefix = "/admin/v1/"

// maxBodyBytes 是请求体的上限。
//
// 管理面的请求都很小（一条渠道、一条规则），而一个没有上限的 body 会让一次
// 误发的超大请求把内存吃掉。1 MiB 对最长的渠道 setting 也足够。
const maxBodyBytes = 1 << 20

// Config 是装配一次管理面所需的依赖。
//
// DB 与 Sealer、Registry 必须由调用方给：它们来自环境里的主密钥与内置适配器
// 清单，而这一层不该自己去读环境变量。Sealer 与 Registry 只有写渠道时用到
// （封凭证、校验渠道类型），为 nil 时那两条端点会报错而不是 panic。
type Config struct {
	DB       *sql.DB
	Sealer   *credential.Sealer
	Registry *channel.Registry
	// Logger 记录被吞掉的内部错误（库报错、读不出 verifier）。为 nil 时用
	// slog.Default()：错误体只能说一句固定的话，真正的原因只有日志里有。
	Logger *slog.Logger

	// Prefix 是这组路由挂在哪。为空时用 PathPrefix（内部监听上的默认挂载点）。
	//
	// 控制台把它挂到 `/api/v1/merchant/` 与 `/api/v1/admin/` 上。前缀必须是
	// `/` 开头、`/` 结尾的子树（见 New 的校验）：少一个斜杠会让
	// `/api/v1/merchant/channels` 落进别的路由，而那种错只表现成 404。
	Prefix string

	// Authorize 包住整组路由。为 nil 时用本包自己的 Bearer 鉴权（内部监听）。
	//
	// 控制台传一个放行的中间件：它的会话角色判据在挂载处（见
	// internal/consoleapi），这里再来一道 Bearer 只会把浏览器挡在外面。
	// 传什么都要理解成「整组路由的前提」，不能只包一部分。
	Authorize func(http.Handler) http.Handler

	// Groups 限定注册哪几组资源；为空表示全部。
	//
	// 生产的两处装配都**显式列出**：内部监听用 InternalGroups（不含 users），
	// 控制台的 admin 组在 consoleapi 里列出（含 users）。留空只在测试与
	// 兼容旧默认时出现。
	//
	// 取值见 GroupChannels / GroupModels / GroupAliases / GroupPricing /
	// GroupCosts / GroupUsers / GroupOverview。控制台的 merchant 组给
	// channels + pricing + overview：它管的是自己名下的渠道、自己定的价，
	// 以及自己那一页的概览。模型目录与别名没有「归属」可以过滤，放给
	// merchant 等于把全局写入权交出去，所以不在这两个组里。
	//
	// `pricing/costs`（上游成本）是单独一组（GroupCosts），不随 pricing 注册：
	// 那是渠道侧的成本规则，没有商家归属。
	//
	// users 组（GroupUsers）只挂在控制台的 admin 组：它要求「操作者是会话里的
	// 某个管理员」——改自己的角色、降最后一个管理员这两条判据在内部管理面上
	// 无法定义（那里的调用者是运维令牌，没有「自己」）。所以内部管理面必须
	// 显式列出资源组，不能留空（见 InternalGroups）。
	//
	// overview 组（GroupOverview）正相反：它**只**随显式列名注册，不随空
	// Groups 出现——它的每条读都建立在归属上下文上，内部管理面没有那层上下文。
	Groups []string

	// Currency 是用户详情里「余额」用哪个资产品种。为空时用 CNY。
	// 它只影响读：钱包里没有这一品种时余额显示 0（与 /api/v1/wallet 同口径）。
	Currency string

	// Timezone 是站点时区的 IANA 名（如 `Asia/Shanghai`），概览的「今天」
	// 按它切日。为空或解析失败时按 UTC——没有站点时区时，UTC 是唯一一个
	// 不需要额外假设的答案。
	Timezone string
}

// tokenSource 提供当前令牌的 verifier。
//
// 抽成接口只有一个目的：鉴权那几条判据（缺令牌、错令牌、未配置）要能脱离
// 数据库测。生产实现只有 TokenStore 一个，它每次请求都读库——见那里的注释。
type tokenSource interface {
	Hash(ctx context.Context) ([sha256.Size]byte, bool, error)
}

// API 是管理面的 handler 集合。
type API struct {
	tokens tokenSource
	log    *slog.Logger

	channels     *channels.Admin
	catalog      *catalog.Catalog
	catalogAdmin *catalog.Admin
	rules        *pricing.Admin
	costs        *pricing.CostAdmin
	users        *auth.Store
	ledger       *store.Store
	currency     string

	// loc 是概览切「今天」用的时区；见 Config.Timezone。
	loc *time.Location
}

// New 装配管理面。返回的 handler **已经带上鉴权中间件**：调用方把它整片挂到
// 内部监听的 `/admin/v1/` 上即可，不存在「只挂了业务 handler」的装配错误。
//
// 注册哪些资源由 Config.Groups 决定；默认（空）是全部。
func New(cfg Config) http.Handler {
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = PathPrefix
	}
	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
		// 前缀是挂载点的**形状**，不是数据：写错时每一条路由都会落到别处，
		// 而症状只是 404。在装配时就说出来，比等第一个请求好。
		panic(fmt.Sprintf("adminapi: Prefix %q 必须以 / 开头并以 / 结尾", cfg.Prefix))
	}

	a := &API{
		tokens:       NewTokenStore(cfg.DB),
		log:          cfg.Logger,
		channels:     channels.NewAdmin(cfg.DB, cfg.Sealer, cfg.Registry),
		catalog:      catalog.New(cfg.DB),
		catalogAdmin: catalog.NewAdmin(cfg.DB),
		rules:        pricing.NewAdmin(cfg.DB),
		costs:        pricing.NewCostAdmin(cfg.DB),
		users:        auth.New(cfg.DB),
		ledger:       store.New(cfg.DB),
		currency:     cfg.Currency,
		loc:          overviewLocation(cfg.Timezone, cfg.Logger),
	}
	if a.currency == "" {
		a.currency = "CNY"
	}

	mux := http.NewServeMux()

	groups, all := groupSet(cfg.Groups)
	if all || groups[GroupChannels] {
		mux.HandleFunc("GET "+prefix+"channels", a.listChannels)
		mux.HandleFunc("POST "+prefix+"channels", a.createChannel)
		mux.HandleFunc("GET "+prefix+"channels/{id}", a.getChannel)
		mux.HandleFunc("PATCH "+prefix+"channels/{id}", a.updateChannel)
	}
	if all || groups[GroupModels] {
		mux.HandleFunc("GET "+prefix+"models", a.listModels)
		mux.HandleFunc("POST "+prefix+"models", a.createModel)
		mux.HandleFunc("PATCH "+prefix+"models/{name}", a.updateModel)
		mux.HandleFunc("DELETE "+prefix+"models/{name}", a.deleteModel)
	}
	if all || groups[GroupAliases] {
		mux.HandleFunc("GET "+prefix+"aliases", a.listAliases)
		mux.HandleFunc("POST "+prefix+"aliases", a.createAlias)
		mux.HandleFunc("PATCH "+prefix+"aliases/{id}", a.updateAlias)
		mux.HandleFunc("DELETE "+prefix+"aliases/{id}", a.deleteAlias)
	}
	if all || groups[GroupPricing] {
		mux.HandleFunc("GET "+prefix+"pricing/rules", a.listRules)
		mux.HandleFunc("POST "+prefix+"pricing/rules", a.createRule)
		mux.HandleFunc("GET "+prefix+"pricing/rules/{id}", a.getRule)
		mux.HandleFunc("PATCH "+prefix+"pricing/rules/{id}", a.updateRule)
	}
	if all || groups[GroupCosts] {
		mux.HandleFunc("GET "+prefix+"pricing/costs", a.listCosts)
		mux.HandleFunc("POST "+prefix+"pricing/costs", a.createCost)
		mux.HandleFunc("PATCH "+prefix+"pricing/costs/{id}", a.updateCost)
	}
	if all || groups[GroupUsers] {
		mux.HandleFunc("GET "+prefix+"users", a.listUsers)
		mux.HandleFunc("GET "+prefix+"users/{id}", a.getUser)
		mux.HandleFunc("PATCH "+prefix+"users/{id}", a.updateUser)
	}
	// 概览与上游**只**随显式组名注册，不跟 all 走（见 Config.Groups）：
	// 它们的每条读都建立在归属上下文上，而「全部资源」那条默认路径
	// （内部管理面）没有这层上下文。写在这里而不是 `all ||` 里，是刻意的。
	if groups[GroupOverview] {
		mux.HandleFunc("GET "+prefix+"overview", a.merchantOverview)
		mux.HandleFunc("GET "+prefix+"upstreams", a.merchantUpstreams)
	}

	authorize := cfg.Authorize
	if authorize == nil {
		authorize = a.authorize
	}
	return authorize(mux)
}

// 资源组名。取值是稳定的：装配参数里写的是它们。
const (
	GroupChannels = "channels"
	GroupModels   = "models"
	GroupAliases  = "aliases"
	GroupPricing  = "pricing"
	GroupCosts    = "costs"
	// GroupUsers 是控制台的用户管理页。它**不在**内部管理面的清单里，
	// 理由见 Config.Groups 与 InternalGroups。
	GroupUsers = "users"
	// GroupOverview 是商家运营页的概览与上游清单。它不随空 Groups 出现。
	GroupOverview = "overview"
)

// InternalGroups 是内部监听（`/admin/v1/*`）上注册的资源组。
//
// 显式列出而不是留空（空 = 全部）：users 组的判据依赖「操作者是会话里的
// 管理员」，内部管理面那一侧没有这个前提。留空的话，加一个组就会顺手把
// 它暴露在运维令牌下面，而那种暴露在请求到达之前看不出来（见 New）。
func InternalGroups() []string {
	return []string{
		GroupChannels, GroupModels, GroupAliases, GroupPricing, GroupCosts,
	}
}

// groupSet 把 Groups 翻成集合；第二个返回值表示「全部资源」。
//
// 未知组名直接 panic：装配参数写错时，症状是「这一组路由一条都不存在」，
// 那在请求到达之前就能发现，比一个安静少挂的 API 面好。
func groupSet(groups []string) (map[string]bool, bool) {
	if len(groups) == 0 {
		return nil, true
	}
	set := make(map[string]bool, len(groups))
	for _, group := range groups {
		switch group {
		case GroupChannels, GroupModels, GroupAliases, GroupPricing, GroupCosts,
			GroupUsers, GroupOverview:
			set[group] = true
		default:
			panic("adminapi: 未知的资源组 " + group)
		}
	}
	return set, false
}

// ownerKey 是归属上下文的键。
type ownerKey struct{}

// WithOwner 把「这次请求只代表这个用户」放进 context，用于控制台的 merchant 组。
//
// 它是**授权判定之后**才该调用的一步：本包拿到这个值就把渠道读写的范围收窄到
// 这个用户名下，而不会再去问「调用者是谁」。所以传进来的 id 必须来自服务端
// 会话，不能来自请求体或查询串。
func WithOwner(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, ownerKey{}, userID)
}

// OwnerFromContext 读 WithOwner 放进去的归属。第二个返回值为 false 表示
// 「这组路由不做归属限制」（admin 组与内部管理面）。
func OwnerFromContext(ctx context.Context) (int64, bool) {
	ownerID, ok := ctx.Value(ownerKey{}).(int64)
	if !ok || ownerID <= 0 {
		return 0, false
	}
	return ownerID, true
}

// actorKey 是「这次请求的管理员是谁」的键。
type actorKey struct{}

// WithActor 把操作者的用户 id 放进 context，供 users 组使用。
//
// 与 WithOwner 的分别：owner 是**受限范围**（merchant 只能看自己名下的行），
// actor 是**操作者身份**（改自己的角色/状态要被拒绝、审计日志要记下是谁）。
// 两者都只该由路由层在拿到服务端会话之后注入，调用方传进来的 id 不能来自
// body 或请求头。
func WithActor(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, actorKey{}, userID)
}

// ActorFromContext 读操作者。第二个返回值为 false 表示这条请求没有被
// 控制台的 admin 组包过——users 组只挂在那一组下面，所以它等于装配错误。
func ActorFromContext(ctx context.Context) (int64, bool) {
	actorID, ok := ctx.Value(actorKey{}).(int64)
	if !ok || actorID <= 0 {
		return 0, false
	}
	return actorID, true
}

// 管理面自己的错误文案。
//
// 401 那一句对「没给令牌」与「给错了」逐字相同：两者都能被中间人构造，
// 而区分它们等于告诉猜令牌的人「这一枚接近了」。503 那一句要说清该去做什么，
// 但**不透露配置细节**：只说管理面没启用，不说是哪一项没配。
const (
	msgUnauthorized = "missing or invalid admin token"
	msgNoToken      = "the admin API is not enabled; run `larkaxctl admin token create` on the server host"
	// msgUnavailable 与 msgDatabase 是内部故障那一侧的话：原因只写日志，
	// 因为 err.Error() 可能带着连接串、主机名与 settings 键名。
	msgUnavailable = "the admin API is temporarily unavailable"
	msgDatabase    = "the database rejected the request; see the server log"
	// msgNotFound 是归属受限（控制台 merchant）那一侧 404 的固定正文。
	//
	// 「不存在」与「不是你的」必须逐字相同：后者一旦能分辨，404 就变成了
	// 「这一行存在」的确认。
	msgNotFound = "not found"
)

// logger 返回装配时给的日志器；没给时用进程级的默认日志器。
func (a *API) logger() *slog.Logger {
	if a.log == nil {
		return slog.Default()
	}
	return a.log
}

// authorize 是管理面的鉴权中间件。
//
// # 顺序要紧
//
// 先读令牌配置、再比对请求：没配置令牌时**任何**请求都得到 503，包括没有
// 带 Authorization 的那一种。反过来的话，未配置的部署会对匿名请求回 401，
// 让人以为「给了令牌就能进」——而实际上给了也进不去，因为根本没有令牌。
//
// # 常量时间比较
//
// 比对的是两个 SHA-256，用 subtle.ConstantTimeCompare。它不是「防远程计时
// 攻击」的银弹（网络抖动远大于比较耗时），但它让这条判据在代码上就是正确的，
// 不必靠「反正也测不出来」来辩护。
func (a *API) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want, configured, err := a.tokens.Hash(r.Context())
		if err != nil {
			// 读不出 verifier 说明库里那一行坏了或者库不可用。这时既不能放行，
			// 也不该说成 401：调用方手里的令牌可能是对的。
			//
			// 正文是一句固定的话：底层错误可能带出连接串、主机名与键名，而
			// 这些只该进日志（见 docs/admin-api.md 的「错误体」）。
			a.logger().Error("读取管理令牌失败", "error", err)
			a.writeError(w, http.StatusServiceUnavailable, msgUnavailable)
			return
		}
		if !configured {
			a.writeError(w, http.StatusServiceUnavailable, msgNoToken)
			return
		}

		token, ok := bearerToken(r)
		if !ok {
			a.writeError(w, http.StatusUnauthorized, msgUnauthorized)
			return
		}
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			a.writeError(w, http.StatusUnauthorized, msgUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken 从 Authorization 里取 Bearer 令牌。
//
// 方案名按 RFC 7235 不区分大小写，值不做任何 trim：令牌是 base64url，两端
// 不该有空白；顺手 trim 只会让「复制时多带了一个空格」也悄悄通过，而那种
// 请求出错时更难查。
func bearerToken(r *http.Request) (string, bool) {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || value == "" {
		return "", false
	}
	return value, true
}

// writeError 写出一个错误体，形状与公共监听相同（见 internal/api.WriteError）。
func (a *API) writeError(w http.ResponseWriter, status int, message string) {
	api.WriteError(w, status, message)
}

// writeJSON 写出一个成功响应。
//
// 序列化失败只可能来自我们自己丢进去的东西（时间、字符串、切片、map），
// 而那属于内部故障：回 500 并说清是哪一步，比回半截 JSON 好。
func (a *API) writeJSON(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		a.writeError(w, http.StatusInternalServerError, "failed to encode the response")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// decodeBody 解一个 JSON 请求体。
//
// 未知字段直接拒：管理面的字段名写错时，静默忽略会让调用方拿到一次「成功」
// 而库里什么都没改——那比一次 400 难查得多。
func (a *API) decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

// fail 把一次写路径的失败翻成状态码。
//
// # 为什么需要这一层
//
// 各 admin 类型返回的是**给人看的一句话**，没有统一的「这是输入错还是库坏
// 了」的分类。判据只能是错误链：
//
//   - 已知哨兵：那一行不在（404）、约束冲突（409）、没有要改的字段（400）
//   - 驱动错误：数据库服务端拒绝或连接断了，是 5xx
//   - 其余：写入路径的校验都在碰库之前，或者由约束拒绝，所以按输入错（400）
//
// 最后一条是**有意的偏向**：认不出来的错误当输入错，而不是当内部故障。
// 代价是某一类真正的 bug 会被报成 400；好处是「参数写错了」不会表现成
// 「服务器坏了」，而那才是调用方会去改的东西。
func (a *API) fail(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	var netErr net.Error
	switch {
	// 一、已知哨兵先判。catalog 与 dberr 的约束错误都把 *pgconn.PgError 留在
	// 错误链里（它们是在「驱动拒绝了这次写入」之上给一个语义名字），所以把
	// pgErr 放在前面会让 409 与那一类 404 全部变成 500——用例喂裸哨兵时是
	// 绿的，真库上却是错的。顺序本身就是判据，用例里另有一条「被 pg 包过」
	// 的那一格。
	case errors.Is(err, channels.ErrChannelNotFound),
		errors.Is(err, catalog.ErrModelNotFound),
		errors.Is(err, catalog.ErrAliasIDNotFound),
		errors.Is(err, pricing.ErrRuleNotFound),
		errors.Is(err, pricing.ErrCostNotFound),
		errors.Is(err, auth.ErrUserNotFound):
		a.writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, dberr.ErrConstraintViolation),
		errors.Is(err, catalog.ErrModelExists),
		errors.Is(err, catalog.ErrCanonicalTaken),
		errors.Is(err, catalog.ErrUpstreamTaken),
		errors.Is(err, catalog.ErrModelInUse),
		errors.Is(err, catalog.ErrChannelNotFound),
		// 用户管理那三条「数据不在可执行这个动作的状态上」：最后一个
		// active 管理员不能被撤，销户流程里的账号不归这一页管。
		errors.Is(err, auth.ErrLastAdmin),
		errors.Is(err, auth.ErrDeletionAlreadyRequested),
		errors.Is(err, auth.ErrDeletionAlreadyDeleted):
		a.writeError(w, http.StatusConflict, err.Error())

	// 二、驱动本身的错误。正文不转述它：它可能带出连接串、主机与表名，
	// 而那些只该进日志。
	case errors.As(err, &pgErr):
		a.logger().Error("管理面写库失败", "error", err)
		a.writeError(w, http.StatusInternalServerError, msgDatabase)
	case errors.As(err, &netErr),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		a.logger().Error("管理面访问数据库失败", "error", err)
		a.writeError(w, http.StatusServiceUnavailable, "database is unavailable")
	default:
		a.writeError(w, http.StatusBadRequest, err.Error())
	}
}

// failScoped 是 fail 的归属受限版本：404 一律用固定正文。
//
// 渠道与计价规则那几条路由用它（归属受限时「不存在」与「不是你的」逐字
// 相同）。不带归属的调用（内部管理面、admin 组）仍走 fail——那里的 404
// 本来就该说清是哪一条，调用者是运维。
func (a *API) failScoped(w http.ResponseWriter, r *http.Request, err error) {
	if _, scoped := OwnerFromContext(r.Context()); scoped &&
		(errors.Is(err, channels.ErrChannelNotFound) || errors.Is(err, pricing.ErrRuleNotFound)) {
		a.writeError(w, http.StatusNotFound, msgNotFound)
		return
	}
	a.fail(w, err)
}

// pathID 取路径里的数字 id。
func pathID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := parseInt64(raw)
	if err != nil {
		return 0, fmt.Errorf("path parameter %s = %q is not a positive integer", name, raw)
	}
	return id, nil
}

// queryID 取查询串里的数字 id；缺省时回 0。
//
// **0 是合法值**：`?channel_id=0` 指的是平台级那一段（别名清单的默认口径），
// 与 CLI 的 `--channel 0` 是同一个含义。这里拒绝 0 的话，那个口径就没有
// 第二种表达方式了。
func queryID(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("query parameter %s = %q is not an integer", name, raw)
	}
	if id < 0 {
		return 0, fmt.Errorf("query parameter %s = %q must not be negative", name, raw)
	}
	return id, nil
}

func parseInt64(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not an integer: %w", err)
	}
	if id <= 0 {
		return 0, errors.New("must be positive")
	}
	return id, nil
}
