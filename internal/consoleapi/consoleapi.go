// Package consoleapi 是公共监听上的用户控制台接口：登录、会话、密钥、用量、
// 钱包与模型参考价。
//
// # 三套凭证，三个面
//
//   - `/v1/*`：推理入口，凭证是用户的 API Key（见 internal/auth 的 Authenticate）。
//   - `/admin/v1/*`：内部监听上的管理面，凭证是运维令牌（见 internal/adminapi）。
//   - `/api/v1/*`：本包。凭证是用户名 + 口令换来的**服务端会话**，装在
//     HttpOnly Cookie 里。
//
// 三套凭证不共用，理由与 adminapi 的包注释相同：权限范围不同的东西不该在
// 库里长得一样。用 API Key 登录控制台尤其不行——那要求用户把长期凭证贴进
// 浏览器，而浏览器缓存、前端日志都比命令行更容易把它漏出去。
//
// # 它只是 HTTP 层
//
// 本包不写 SQL、不解释金额、不判断「口令对不对」：
//
//   - 会话与口令在 internal/auth；
//   - 密钥的读写复用 auth.Store 的 Create / UpdateKey（与 larkaxctl 同一份）；
//   - 用量聚合、钱包分录在 internal/ledger/store；
//   - 参考价在 internal/pricing。
//
// 它负责的是：路由、会话中间件、CSRF、传输校验、参数解析、JSON 形状。
//
// # 错误体不复用 /v1/*
//
// `/v1/*` 的错误是 OpenAI 的协议形状，推理 SDK 按既定格式解析它；控制台
// 客户端是我们自己的 SPA。把两者合成一个形状，等于让改一个前端字段的
// 配色去动推理协议。所以这里有一套 `{"error":{"code","message","request_id"}}`。
//
// # 金额一律是字符串
//
// 金额在 JSON 里是固定 8 位小数的**字符串**，从不让客户端用 double 解析钱。
// 时间用 RFC3339 UTC。
package consoleapi

import (
	"context"
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

	"github.com/sumwai/larkax/internal/adminapi"
	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/keyquota"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pricing"
)

// PathPrefix 是控制台接口在公共监听上的挂载点。
const PathPrefix = "/api/v1/"

// maxBodyBytes 是请求体的上限。
//
// 控制台的请求体都很小（一次登录、一次改字段），64 KiB 对最长的字段
// 也绰绰有余；而没有上限的 body 会让一次误发的超大请求把内存吃掉。
const maxBodyBytes = 64 << 10

// 错误码。取值稳定，供前端分支与本地化；文案可以改，码不能。
const (
	codeInvalidRequest    = "invalid_request"
	codePayloadTooLarge   = "payload_too_large"
	codeInsecureTransport = "insecure_transport"
	codeWeakPassword      = "weak_password"
	codeInvalidCredential = "invalid_credentials"
	codeUnauthenticated   = "unauthenticated"
	codeCSRFFailed        = "csrf_failed"
	codeForbidden         = "forbidden"
	codeAccountDisabled   = "account_disabled"
	codeAccountNotReady   = "account_not_ready"
	codeNotFound          = "not_found"
	codeConflict          = "conflict"
	codeRateLimited       = "rate_limited"
	codeInternal          = "internal"
	codeUnavailable       = "unavailable"
)

// Config 是装配一次控制台接口所需的依赖。
type Config struct {
	// DB 是必须给的那一项：本包的每一个读方法都经过它背后的包。
	DB *sql.DB
	// Logger 记录被吞掉的内部错误。为 nil 时用 slog.Default()。
	Logger *slog.Logger
	// KDF 是口令哈希的当前参数。零值时用 auth.DefaultKDFParams（生产 60 万轮）；
	// 测试注入一个小的，否则每一条登录用例都要几百毫秒。
	KDF auth.KDFParams
	// SiteName / BaseURL / Currency / Timezone 是 meta 端点返回的部署常量。
	SiteName string
	// BaseURL 是推理入口的 base URL（例如 https://api.example.com/v1）。
	// 为空时按请求的 Host 推出来——部署换了域名不必改代码。
	BaseURL  string
	Currency string
	Timezone string
	// Region 是日历的地区（规则可能用 isWorkday / isPeak 定价参考价）。
	Region string
	// Calendar 供参考价求值使用；为 nil 时用到日历的规则取不到参考价。
	Calendar expr.Calendar
	// Sealer 与 Registry 是 merchant / admin 两组里的渠道写入要用的两件
	// （见 internal/adminapi）。为零值时创建渠道会报错而不是 panic；读路径
	// 不受影响。它们由装配层注入，本包不读环境变量、不碰主密钥。
	Sealer   *credential.Sealer
	Registry *channel.Registry
	// TrustProxy 强制信任转发头（`X-Forwarded-Proto` / `-For` / `-Host`）。
	//
	// 它为 true 时，无论直连对端是谁都读这几个头。为 false 时按**直连对端**
	// 判断（见 forwardedTrusted）：对端是 loopback 或私有地址才算受信——
	// 公开地址的客户端可以随意伪造这些头，而那是唯一一种需要挡的情形。
	//
	// 允许显式打开是给「反代在公网另一头、通过隧道进来」这类部署留的口子；
	// 默认的启发式覆盖同一台机器与同一内网这两种常见拓扑。
	TrustProxy bool
	// Now 可注入，便于测试会话过期与时间窗。
	Now func() time.Time
}

// sessionStore 是本包对会话存储的全部要求。
//
// 抽成接口只有一个目的：中间件的判据（没有 Cookie、令牌无效、会话过期、
// 用户被封禁）要能脱离数据库测。生产实现只有 auth.Store 一个。
type sessionStore interface {
	CreateSession(ctx context.Context, userID int64, userAgent string, ip net.IP) (auth.Session, string, error)
	SessionByToken(ctx context.Context, token string) (auth.Session, error)
	TouchSession(ctx context.Context, session auth.Session, now time.Time) error
	RevokeSession(ctx context.Context, sessionID int64) error
}

// accountStore 是登录与账户资料要的那几个方法。
type accountStore interface {
	LoginUserByName(ctx context.Context, username string) (auth.LoginUser, error)
	LoginUserByID(ctx context.Context, userID int64) (auth.LoginUser, error)
	RecordLoginFailure(ctx context.Context, userID int64, now time.Time) (int, *time.Time, error)
	ClearLoginFailures(ctx context.Context, userID int64) error
	RehashPassword(ctx context.Context, userID int64, encodedHash string) error
	SetPassword(ctx context.Context, userID int64, encodedHash string, keep *int64) error
	UserByRef(ctx context.Context, ref string) (auth.UserInfo, error)
	UpdateUser(ctx context.Context, ref string, p auth.UpdateUserParams) (auth.UserInfo, error)
}

// keyStore 是密钥的读写。
type keyStore interface {
	Create(ctx context.Context, p auth.CreateParams) (auth.Created, error)
	UpdateKey(ctx context.Context, keyID int64, p auth.KeyParams) (auth.Key, error)
	KeyForUser(ctx context.Context, userID, keyID int64) (auth.Key, error)
	ListKeysPage(ctx context.Context, q auth.KeyListQuery) (auth.KeyListPage, error)
}

// usageStore 是用量与钱包的聚合读。
type usageStore interface {
	UsageSummary(ctx context.Context, w store.UsageWindow) (store.UsageSummary, error)
	UsageSeries(ctx context.Context, w store.UsageWindow, interval string, tz *time.Location) ([]store.UsagePoint, error)
	UsageBreakdown(ctx context.Context, w store.UsageWindow, groupBy string, limit int) (store.UsageBreakdown, error)
	UsageRequests(ctx context.Context, q store.UsageRequestQuery) (store.UsageRequestPage, error)
	Wallets(ctx context.Context, userID *int64) ([]store.Wallet, error)
	UserEntries(ctx context.Context, q store.UserEntryQuery) (store.UserEntryPage, error)
}

// modelSource 回答「哪些模型在供」。
type modelSource interface {
	SuppliedModels(ctx context.Context) ([]string, error)
}

// modelSupply 回答「每个模型由哪些渠道在供、渠道归谁」。
//
// 与 modelSource 分开是因为两个问题不同：一份只列名字（参考价按名字求），
// 另一份要逐渠道的归属（报价按渠道的商家取规则）。
type modelSupply interface {
	Supplies(ctx context.Context) ([]channels.Supply, error)
}

// offerSource 给每个「模型 × 渠道」候选取一条适用的规则并求价。
type offerSource interface {
	Prices(ctx context.Context, at time.Time, candidates []pricing.Candidate) ([]pricing.CandidatePrice, error)
}

// catalogSource 读模型目录的展示字段。
//
// 单条模型要读一条目录行；清单要读全部。两件事都在 internal/catalog，
// 本包不另写 SQL。
type catalogSource interface {
	Models(ctx context.Context) ([]catalog.Model, error)
	Model(ctx context.Context, canonicalName string) (catalog.Model, error)
}

// priceSource 求平台规则下的参考价。
type priceSource interface {
	ReferencePrices(ctx context.Context, models []string, at time.Time) (map[string]*pricing.ReferencePrice, error)
}

// spentFunc 一次读一页 key 的累计花费（见 keyquota.SpentMany）。
//
// 用函数类型而不是接口：它只有一个方法，而参数里那个 keyquota.MultiQuerier
// 是数据库句柄，接口化之后测试反而要多写一层包装。
type spentFunc func(ctx context.Context, keyIDs []int64) (map[int64]money.Amount, error)

// deps 是本包用到的全部下层能力。
//
// 单开一个结构体是为了让测试能只替换一件东西：New 从 *sql.DB 装出生产的
// 一份，测试可以造零个数据库。
type deps struct {
	// merchantAdmin 与 adminAdmin 是挂在两个角色组下面的管理资源 handler。
	//
	// merchant 那份只注册渠道路由、并且每条都按会话用户的归属过滤；
	// admin 那份注册全部资源。两份都由 newAPI 按 Config 现建，
	// 测试可以注入替身（用例要验的是角色中间件，不是资源 handler）。
	merchantAdmin http.Handler
	adminAdmin    http.Handler

	sessions sessionStore
	accounts accountStore
	keys     keyStore
	usage    usageStore
	models   modelSource
	supplies modelSupply
	catalog  catalogSource
	prices   priceSource
	offers   offerSource
	spent    spentFunc
}

// API 是控制台接口的 handler 集合。
type API struct {
	log      *slog.Logger
	now      func() time.Time
	loc      *time.Location
	kdf      auth.KDFParams
	siteName string
	baseURL  string
	currency string
	trust    bool
	deps     deps

	// dummyHash 是「用户不存在」时也要跑一次的那条哈希。
	//
	// 不跑的话，一条不存在的用户名会在几微秒内返回 401，而存在的用户要等
	// 一次 KDF——响应时间本身就是一张用户名清单。哈希在装配时算一次，
	// 之后每次失败都拿它跑一次验证，耗时与真实用户相当。
	dummyHash string

	loginLimiter *ipLimiter
}

// New 装配控制台接口。返回的 handler 已经带上会话中间件：
// 调用方把它整片挂到公共监听的 `/api/v1/` 上即可。
func New(cfg Config) http.Handler {
	if cfg.DB == nil {
		panic("consoleapi: Config.DB 不能为空")
	}
	authStore := auth.New(cfg.DB)
	channelSource := channels.New(cfg.DB, nil)
	return newAPI(cfg, deps{
		sessions: authStore,
		accounts: authStore,
		keys:     authStore,
		usage:    store.New(cfg.DB),
		models:   channelSource,
		supplies: channelSource,
		catalog:  catalog.New(cfg.DB),
		prices:   pricing.NewReferences(cfg.DB, cfg.Region, cfg.Calendar),
		offers:   pricing.NewOffers(cfg.DB, cfg.Region, cfg.Calendar),
		spent: func(ctx context.Context, keyIDs []int64) (map[int64]money.Amount, error) {
			return keyquota.SpentMany(ctx, cfg.DB, keyIDs)
		},
	})
}

// newAPI 装配 handler 本身。
//
// 与 New 分开是为了让测试只替换下层，不替换这一层要验的东西（会话中间件、
// CSRF、参数解析、响应形状）。测试造零个数据库。
func newAPI(cfg Config, d deps) http.Handler {
	a := &API{
		log:          cfg.Logger,
		now:          cfg.Now,
		kdf:          cfg.KDF,
		siteName:     cfg.SiteName,
		baseURL:      strings.TrimSuffix(cfg.BaseURL, "/"),
		currency:     cfg.Currency,
		trust:        cfg.TrustProxy,
		loginLimiter: newIPLimiter(),
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.kdf == (auth.KDFParams{}) {
		a.kdf = auth.DefaultKDFParams
	}
	if err := a.kdf.Validate(); err != nil {
		panic(fmt.Sprintf("consoleapi: 口令哈希参数非法: %v", err))
	}
	if a.siteName == "" {
		a.siteName = "larkax"
	}
	if a.currency == "" {
		a.currency = "CNY"
	}
	tz := cfg.Timezone
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		panic(fmt.Sprintf("consoleapi: 无法解析站点时区 %q: %v", tz, err))
	}
	a.loc = loc

	hash, err := auth.HashPassword(a.kdf, "console-login-dummy-password")
	if err != nil {
		panic(fmt.Sprintf("consoleapi: 生成假哈希失败: %v", err))
	}
	a.dummyHash = hash
	a.deps = d

	// 角色组的资源 handler。放行中间件在这里传：会话与角色的判据属于本包的
	// 路由层（见 roleGroup），adminapi 自己的 Bearer 鉴权是内部监听那一侧的事。
	if a.deps.merchantAdmin == nil || a.deps.adminAdmin == nil {
		passThrough := func(next http.Handler) http.Handler { return next }
		build := func(prefix string, groups []string) http.Handler {
			return adminapi.New(adminapi.Config{
				DB:        cfg.DB,
				Sealer:    cfg.Sealer,
				Registry:  cfg.Registry,
				Logger:    a.log,
				Prefix:    prefix,
				Authorize: passThrough,
				Groups:    groups,
				Currency:  a.currency,
			})
		}
		if a.deps.merchantAdmin == nil {
			// 商家管自己的渠道、自己的价。模型目录与别名没有归属可过滤。
			a.deps.merchantAdmin = build(PathPrefix+"merchant/",
				[]string{adminapi.GroupChannels, adminapi.GroupPricing})
		}
		if a.deps.adminAdmin == nil {
			// 显式列出，不留空（空 = 全部）：users 组只属于这一组——它的
			// 判据依赖会话里的操作者，内部管理面那一侧没有这个前提。
			a.deps.adminAdmin = build(PathPrefix+"admin/", []string{
				adminapi.GroupChannels,
				adminapi.GroupModels,
				adminapi.GroupAliases,
				adminapi.GroupPricing,
				adminapi.GroupCosts,
				adminapi.GroupUsers,
			})
		}
	}

	if !a.trust {
		a.log.Info("控制台只信任本机与内网来的转发头：直连公网的请求，X-Forwarded-* 一律忽略")
	}
	return observe.Middleware(a.routes())
}

// routes 注册全部端点。
//
// 鉴权不在路由层「猜」：每一条受保护的路径都显式经过 requireSession，
// 写方法再经过一次 CSRF 校验。漏挂一层时，它没有默认放行的分支——路径
// 压根不会出现在表里。
func (a *API) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+PathPrefix+"meta", a.handleMeta)
	mux.HandleFunc("POST "+PathPrefix+"auth/login", a.handleLogin)
	mux.HandleFunc("POST "+PathPrefix+"auth/logout", a.handleLogout)
	mux.HandleFunc("GET "+PathPrefix+"auth/session", a.handleSession)
	mux.HandleFunc("GET "+PathPrefix+"models", a.handleModels)
	// {name...} 而不是 {name}：规范名里可能有 "/"（`vendor/model` 这种写法），
	// 按单段匹配会把它们判成未知路径。与 /v1/models/{id} 是同一个理由。
	mux.HandleFunc("GET "+PathPrefix+"models/{name...}", a.handleModelDetail)

	mux.Handle("GET "+PathPrefix+"me", a.requireSession(false, a.handleMe))
	mux.Handle("PATCH "+PathPrefix+"me", a.requireSession(true, a.handlePatchMe))
	mux.Handle("POST "+PathPrefix+"me/password", a.requireSession(true, a.handleChangePassword))

	mux.Handle("GET "+PathPrefix+"keys", a.requireSession(false, a.handleListKeys))
	mux.Handle("POST "+PathPrefix+"keys", a.requireSession(true, a.handleCreateKey))
	mux.Handle("GET "+PathPrefix+"keys/{id}", a.requireSession(false, a.handleGetKey))
	mux.Handle("PATCH "+PathPrefix+"keys/{id}", a.requireSession(true, a.handlePatchKey))
	mux.Handle("POST "+PathPrefix+"keys/{id}/revoke", a.requireSession(true, a.handleRevokeKey))

	mux.Handle("GET "+PathPrefix+"usage/summary", a.requireSession(false, a.handleUsageSummary))
	mux.Handle("GET "+PathPrefix+"usage/series", a.requireSession(false, a.handleUsageSeries))
	mux.Handle("GET "+PathPrefix+"usage/breakdown", a.requireSession(false, a.handleUsageBreakdown))
	mux.Handle("GET "+PathPrefix+"usage/requests", a.requireSession(false, a.handleUsageRequests))

	mux.Handle("GET "+PathPrefix+"wallet", a.requireSession(false, a.handleWallet))
	mux.Handle("GET "+PathPrefix+"wallet/entries", a.requireSession(false, a.handleWalletEntries))

	// 角色组挂的是子树：组内所有路径先过角色会话中间件，再交给管理资源
	// handler。前缀与 internal/adminapi 里注册的逐字一致，改一处就是「路由
	// 都在、却一条也到不了」——两份都从这里取，不各写一份字面量。
	mux.Handle(PathPrefix+"merchant/", a.roleGroup(auth.RoleMerchant, a.deps.merchantAdmin))
	mux.Handle(PathPrefix+"admin/", a.roleGroup(auth.RoleAdmin, a.deps.adminAdmin))

	return mux
}

// authedHandler 是拿到会话之后的处理器。
type authedHandler func(w http.ResponseWriter, r *http.Request, s auth.Session)

// requireSession 是会话中间件；write 为 true 时再校验 CSRF。
func (a *API) requireSession(write bool, next authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.currentSession(w, r)
		if !ok {
			return
		}
		if write && !a.csrfValid(r, session) {
			a.fail(w, r, http.StatusForbidden, codeCSRFFailed,
				"The CSRF token is missing or does not match.")
			return
		}

		a.touchSession(r, session)
		next(w, r, session)
	})
}

// roleGroup 是 `/merchant/` 与 `/admin/` 两组共用的中间件。
//
// 顺序：会话（401、账号停用 403）→ 角色（403，正文固定）→ 写方法的 CSRF →
// 注入归属 → 资源 handler。角色判据只看服务端会话里那一列（见 auth.Session.
// Role），请求里带的「当前视图」不参与判定。
//
// 归属只注入给**merchant 组里的 merchant**：admin 在这一组里看全部，与它进
// admin 组时一样；不注入的话 adminapi 会把 admin 当成某个用户，于是它在
// merchant 组里也只能看到自己的渠道——那不是「merchant 或 admin」的意思。
func (a *API) roleGroup(required auth.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := a.currentSession(w, r)
		if !ok {
			return
		}
		if !session.Role.Allows(required) {
			a.fail(w, r, http.StatusForbidden, codeForbidden, msgForbidden)
			return
		}
		if !isSafeMethod(r.Method) && !a.csrfValid(r, session) {
			a.fail(w, r, http.StatusForbidden, codeCSRFFailed,
				"The CSRF token is missing or does not match.")
			return
		}
		a.touchSession(r, session)
		if required == auth.RoleMerchant && session.Role == auth.RoleMerchant {
			r = r.WithContext(adminapi.WithOwner(r.Context(), session.UserID))
		}
		// admin 组总带操作者：用户管理要判「是不是在改自己」，审计日志也要
		// 记下是谁。它只来自这里（服务端会话），请求头与 body 不参与。
		if required == auth.RoleAdmin {
			r = r.WithContext(adminapi.WithActor(r.Context(), session.UserID))
		}
		next.ServeHTTP(w, r)
	})
}

// msgForbidden 是角色不足时唯一的正文。
//
// 它不说缺哪个角色，也不说这条路径叫什么：两组路由的存在本身不是秘密，但
// 「你差的是 merchant 还是 admin」会把内部拓扑说给一个已经登录的人，
// 而那对排查没有帮助——他要做的是找管理员。
const msgForbidden = "You do not have permission to perform this action."

// isSafeMethod 判断一个方法是不是只读。
//
// 只读方法不校验 CSRF（浏览器的跨站 GET 不携带副作用的前提是服务端不把它
// 当写）。其余一律当作写：未知方法放过去，会比漏校验一次 CSRF 更坏。
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// touchSession 刷新「最后活跃」与空闲窗。
//
// 它每分钟至多写一次（见 auth.TouchSession），错误显式丢弃：一个本来有效的
// 请求不该因为一列观测值写不进去而失败。用 WithoutCancel：响应写完（或客户端
// 断开）之后这次写仍要落地，否则「最后活跃」会在请求被取消的那些路径上停住。
func (a *API) touchSession(r *http.Request, session auth.Session) {
	if err := a.deps.sessions.TouchSession(
		context.WithoutCancel(r.Context()), session, a.now().UTC()); err != nil {
		a.log.Warn("刷新会话失败", "error", err)
	}
}

// currentSession 读 Cookie、校验会话，失败时已经写好响应。
func (a *API) currentSession(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil || cookie.Value == "" {
		a.fail(w, r, http.StatusUnauthorized, codeUnauthenticated, "Sign in to continue.")
		return auth.Session{}, false
	}

	session, err := a.deps.sessions.SessionByToken(r.Context(), cookie.Value)
	if errors.Is(err, auth.ErrSessionNotFound) {
		a.fail(w, r, http.StatusUnauthorized, codeUnauthenticated, "Sign in to continue.")
		return auth.Session{}, false
	}
	if err != nil {
		// 查询失败不是「会话无效」：把库故障显示成登录过期，用户重登也进不去，
		// 而真正的下一步是看服务端日志。
		a.log.Error("查询会话失败", "error", err)
		a.fail(w, r, http.StatusServiceUnavailable, codeUnavailable,
			"The console is temporarily unavailable.")
		return auth.Session{}, false
	}

	now := a.now().UTC()
	if session.RevokedAt != nil || !now.Before(session.ExpiresAt) {
		a.fail(w, r, http.StatusUnauthorized, codeUnauthenticated, "Sign in to continue.")
		return auth.Session{}, false
	}
	if session.UserStatus != auth.UserStatusActive {
		a.fail(w, r, http.StatusForbidden, codeAccountDisabled, "This account is not active.")
		return auth.Session{}, false
	}
	return session, true
}

// csrfValid 校验同步令牌与来源。
//
// 两层都要过：SameSite=Lax 已经挡住跨站 POST，但那是浏览器的行为，不是
// 服务端的判据；一个不把 SameSite 当回事的旧浏览器、或一个被挂进 iframe 的
// 页面，都不在那一层的保护范围里。
func (a *API) csrfValid(r *http.Request, session auth.Session) bool {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(session.CSRFToken)) != 1 {
		return false
	}
	return a.sameOrigin(r)
}

// sameOrigin 判断这次请求是不是从本站发出的。
//
// 它只看两个现代浏览器都会带、而跨站页面改不掉的头：
//
//   - Origin 存在时必须等于本站。没有 Origin 的请求（curl、非浏览器客户端）
//     放行——它们不是 CSRF 的目标，也没有一个「本站」可以比。
//   - Sec-Fetch-Site 存在时必须同源。跨站表单会带 cross-site；用户直接在
//     地址栏敲入是 none，而那种请求不会是写方法。
//
// 登录也走这一条（见 handleLogin）：那里没有 CSRF 令牌可比，但跨站登录
// 同样有后果——受害者会被登进攻击者的账号。
func (a *API) sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		if !strings.EqualFold(strings.TrimSuffix(origin, "/"), a.origin(r)) {
			return false
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && !strings.EqualFold(site, "same-origin") {
		return false
	}
	return true
}

// origin 推出这次请求的「本站」地址。
//
// 生产里它是 https://<域名>；受信代理给的 X-Forwarded-Proto / Host 优先，
// 否则按连接本身推。它只与请求里带来的 Origin 比，不用于拼重定向。
func (a *API) origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if a.forwardedTrusted(r) {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = strings.ToLower(strings.TrimSpace(proto))
		}
	}
	host := r.Host
	if a.forwardedTrusted(r) {
		if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
			host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
	}
	return scheme + "://" + host
}

// forwardedTrusted 判断这次请求的转发头能不能信。
//
// 判据是**直连对端**的地址：loopback 与私有网段算受信，公开地址不算。
// 理由是攻击面：能连到这个进程的公开地址客户端可以随手写上
// `X-Forwarded-Proto: https`，而只有处在同一台机器或同一内网的反代才
// 真的终结过 TLS、真的知道来源 IP。
//
// 这是一个启发式，不是授权：它把「反代与本进程在同一信任域」当作部署的
// 默认形状。反代在别处时把 Config.TrustProxy 打开，那时信任是显式给的。
func (a *API) forwardedTrusted(r *http.Request) bool {
	if a.trust {
		return true
	}
	return peerIsLocalOrPrivate(r.RemoteAddr)
}

// peerIsLocalOrPrivate 判断 TCP 对端地址是不是 loopback 或私有地址。
func peerIsLocalOrPrivate(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// transportSecure 判断这次请求是不是走了 HTTPS。
//
// 它只用在口令端点上（登录、改口令）：口令的机密性由 TLS 兑现，而这两个
// 端点是口令唯一出现的地方。其余端点不判——它们不传口令，而在明文 HTTP
// 上拒绝整个控制台会让本地开发寸步难行。
func (a *API) transportSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if a.forwardedTrusted(r) {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" &&
			strings.EqualFold(strings.TrimSpace(proto), "https") {
			return true
		}
	}
	// loopback 例外只给本地开发：浏览器把 localhost 当作安全上下文，
	// 而 curl 在 127.0.0.1 上也不会把口令送到网线上。
	return isLoopback(r.RemoteAddr)
}

// isLoopback 判断请求的远端地址是不是本机。
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// remoteIP 取这次请求的来源地址。
//
// X-Forwarded-For 只在有受信代理时读，而且只取第一段：后面几段可能是
// 中间代理追加的，拿它们做按 IP 的节流等于把所有人算成同一个人。
func (a *API) remoteIP(r *http.Request) net.IP {
	if a.forwardedTrusted(r) {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

// setSessionCookie 写会话 Cookie。
func (a *API) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie 清会话 Cookie。
func (a *API) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// errorBody 是错误响应体。
type errorBody struct {
	Error errorDetail `json:"error"`
}

// errorDetail 是一条错误。
type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// fail 写一条错误响应。
func (a *API) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	// 带用户数据的响应都不许缓存。错误体里也可能有状态（例如账号被停用），
	// 所以这一条对错误也成立。
	w.Header().Set("Cache-Control", "no-store")
	id := observe.RequestIDFromContext(r.Context())
	if id == "" {
		id = observe.NewRequestID()
		w.Header().Set(observe.HeaderRequestID, string(id))
	}
	a.writeJSON(w, status, errorBody{Error: errorDetail{
		Code: code, Message: message, RequestID: string(id),
	}})
}

// writeJSON 写一个 JSON 响应。
func (a *API) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// ok 写一个成功响应。
func (a *API) ok(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	a.writeJSON(w, status, v)
}

// decodeJSON 解析请求体。
//
// 未知字段一律拒绝：一个拼错的字段名静默成功，会让前端以为设置生效了，
// 而服务端什么都没改。末尾多余的数据同样拒绝——它通常意味着发的是两段
// JSON，或者内容长度算错了。
func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("请求体里有多余的内容")
	}
	return nil
}

// parseLimit 解析分页的 limit。
func parseLimit(raw string, def, max int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, fmt.Errorf("limit 必须是 1..%d 之间的整数", max)
	}
	return n, nil
}

// pathID 解析路径里的 {id}。
func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("id 必须是正整数")
	}
	return id, nil
}

// rfc3339 把时刻格式化成 RFC3339 UTC。
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// optionalTime 格式化一个可空时刻。
func optionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	value := formatTime(*t)
	return &value
}
