// Package assembly 把公共入口所需的各个包装配成一个可服务的对象。
//
// # 为什么单独一个包，而不是写在 cmd/larkax 里
//
// 端到端测试必须跑**生产用的那条装配路径**，否则测的是测试自己接的线。
// 装配写在 package main 里，测试就只能跑 cmd/larkax 的测试——那要连数据库、
// 要监听端口、要读环境变量，都不是命令行的职责。装成一个 internal 包之后，
// 测试 import 它、把上游执行器换成指向本地假上游的那一个，就能把
// 「HTTP 请求 → 鉴权 → 预留 → 选路 → 上游 → 结算 → 落库」整条链跑一遍。
//
// # 它替调用方做的判断
//
// 装出来的东西**要么能用，要么启动就失败**：缺执行器、缺规则来源、缺数据库
// 都在 Build 里报错，而不是等到第一个请求。理由是这几种缺失在运行期的表现
// 都一样——每个请求回一个 5xx 或 501，而现场看不出是「没装配」还是
// 「装配错了」。启动失败则指向明确。
//
// # 哪些东西是参数
//
// 上游执行器与计价规则来源**由调用方传进来**，本包不 new：
//
//   - 执行器是唯一会发出外部请求的东西，测试要能把它指向一个本地假上游。
//     生产侧它由 internal/upstream 装配（各协议适配器的注册表）。
//   - 规则来源连的是数据库，但它的匹配口径（作用域、优先级）属于计价，
//     不属于接线。放在参数里，这一层就不必知道规则长什么样。
//
// 其余依赖（候选来源、账本存储、快照链路、鉴权）都是本包直接构造的：
// 它们没有可替换的实现，多一层参数只会让调用方多写几行没有信息的代码。
package assembly

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/api"
	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/ratelimit"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/snapshotwire"
)

// Config 是装配一次服务所需的全部输入。
type Config struct {
	// DB 是已打开并验证过连通的连接池。
	DB *sql.DB

	// Sealer 解开渠道凭证。它来自环境变量里的主密钥，因而只有需要接触
	// 上游身份的角色才有——worker 角色没有，也就装不出候选来源。
	Sealer *credential.Sealer

	// Settings 是已加载并校验过的运行期配置（数据库的 settings 表）。
	// 装配期要读的是快照链路的开关与留存期、分时计价的默认地区。
	Settings *settings.Values

	// Logger 为 nil 时用 slog.Default()。
	Logger *slog.Logger

	// Version 是发布版本，写进每个响应的 x-larkax-version 头。
	Version string

	// Executor 是调上游的能力，由调用方构造。
	Executor gateway.Executor

	// Rules 是计价规则的来源，由调用方构造。
	Rules pipeline.RuleSource

	// Calendar 提供分时计价要用的工作日与节假日。可以为 nil——
	// 那时不用日历的规则照常计价，用到日历的规则求值失败并报错
	// （见 internal/billing/expr 的 ErrCalendarRequired）。
	Calendar expr.Calendar

	// Now 可注入，便于测试时间相关的逻辑。为 nil 时用 time.Now。
	Now func() time.Time
}

// App 是装配结果。
type App struct {
	// Public 是公共监听端口的 handler。
	//
	// 它已经带上 observe.Middleware：关联标识必须在进入业务 handler
	// **之前**放进 context，否则计费落库用的幂等键与快照的 request_id
	// 都会退化成 handler 自己现编的一个值，而那些日志与记录拼不回同一次请求。
	Public http.Handler

	// Gateway 是入口编排。导出它是为了让测试能直接看一次请求的结局
	// （用了哪个渠道、扣了多少），而不必从响应体里反推。
	Gateway *gateway.Gateway

	// Snapshots 是快照链路。导出它的理由与 Gateway 相同：它带着写入与
	// 丢弃的计数，而「一条快照都没写进去」与「没有请求」在日志里长得一样。
	Snapshots *snapshotwire.Collector
}

// cacheSize 是计价表达式编译缓存的容量。
//
// 显式传一个缓存，而不是让 pipeline 在第一次求值时自己建：它那一步是
// 无锁的懒赋值，两个并发请求同时走到就会写同一个字段——那是数据竞争，
// 而竞争窗口只在启动后的头几条请求上，现场表现为偶发的告警，很难查。
const cacheSize = 256

// regionKey 是分时计价默认地区的配置键，与 settings.Registry 里的一致。
const regionKey = "calendar.default_region"

// Build 装配一次服务。
//
// 它不启动任何监听器，也不接管进程的生命周期：装出来的对象交给调用方，
// 由它决定在哪监听、什么时候退出。这样同一份装配既能被命令行使用，
// 也能被测试在一个进程内跑起来。
func Build(cfg Config) (*App, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	snapshots, err := buildSnapshots(cfg, logger)
	if err != nil {
		return nil, err
	}

	region, err := cfg.Settings.String(regionKey)
	if err != nil {
		return nil, fmt.Errorf("读取分时计价地区失败: %w", err)
	}

	// 账本存储同时满足预留与结算两个接口。它的方法名与签名与 gateway
	// 声明的逐字一致，所以这里没有适配器——那正是两边刻意对齐的地方。
	ledgerStore := store.New(cfg.DB)

	gw := &gateway.Gateway{
		Pipeline: &pipeline.Pipeline{
			Rules:    cfg.Rules,
			Programs: expr.NewCache(cacheSize),
			Calendar: cfg.Calendar,
			Region:   region,
			Now:      cfg.Now,
		},
		Router:       &route.Router{Now: cfg.Now},
		Candidates:   channels.New(cfg.DB, cfg.Sealer),
		Reservations: ledgerStore,
		Settlements:  ledgerStore,
		Redactor:     snapshots.Redactor(),
		Snapshots:    snapshots.Sink(),
		Executor:     cfg.Executor,
		Now:          cfg.Now,
	}

	handler := api.New(authenticator{store: auth.New(cfg.DB)}, gw)
	handler.Version = cfg.Version
	handler.Now = cfg.Now
	handler.RateLimiter = ratelimit.New(cfg.Now)

	// 限流按 key 的额度生效了，但计数在**进程内**：多实例部署时每个实例
	// 各计一份，一把 60 RPM 的 key 在 N 个实例上最多能打到 60×N RPM。
	//
	// 这件事必须说出来。「设了额度」与「真的限住了」在配置、响应码和请求
	// 日志上都一模一样，唯一能把它们分开的就是这句话——而默认沉默的话，
	// 一个按 60 RPM 配了容量、实际被打到 120 RPM 的部署没有任何提示。
	// 共享计数要有一个所有实例都连得上的存储，那是另一件事。
	logger.Warn("RPM 限流按进程内计数：多实例部署时各实例独立计数，额度不共享")

	return &App{
		Public:    observe.Middleware(handler),
		Gateway:   gw,
		Snapshots: snapshots,
	}, nil
}

// validate 把「装不出能用的服务」这件事一次报全。
//
// 一次报全而不是撞上第一个就返回：缺项往往是一起出现的（换一台机器部署
// 时少配了一批东西），逐个报会让人重启好几遍。
func (c Config) validate() error {
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if c.DB == nil {
		fail("缺少数据库句柄")
	}
	if c.Sealer == nil {
		fail("缺少主密钥封装器（渠道凭证解不开）")
	}
	if c.Settings == nil {
		fail("缺少已加载的运行期配置")
	}
	if c.Executor == nil {
		fail("缺少上游执行器（每个请求都会回 501）")
	}
	if c.Rules == nil {
		fail("缺少计价规则来源（无法计费）")
	}

	if len(errs) > 0 {
		return fmt.Errorf("装配失败:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// buildSnapshots 按 settings 里的开关与上限建立快照链路。
//
// 关掉时它返回一个 Redactor 与 Sink 都是 nil 的 Collector，网关据此
// 完全不采集——「关掉」的代价是零，而不是「脱敏一遍再丢掉」。
func buildSnapshots(cfg Config, logger *slog.Logger) (*snapshotwire.Collector, error) {
	snapCfg, err := snapshotwire.LoadConfig(cfg.Settings)
	if err != nil {
		return nil, fmt.Errorf("读取快照配置失败: %w", err)
	}
	// PgDB 那层适配是必须的：snapshotwire 为了让分区名筛选可测，把
	// QueryContext 的返回类型收窄成了它自己的 Rows 接口。
	collector, err := snapshotwire.New(snapCfg, snapshotwire.Wrap(cfg.DB), logger)
	if err != nil {
		return nil, fmt.Errorf("建立快照链路失败: %w", err)
	}
	return collector, nil
}

// authenticator 把 auth.Store 适配成公共入口要的那份接口。
//
// # 为什么是两个类型之间的几行转换，而不是合并它们
//
// auth.User 带 ID、Username、KeyID 与限流额度，api.User 只带入口真正用到的
// 那几个。两者不合并是有意的：公共入口的注释写明，套餐、分组这些属于鉴权
// 自己的类型，不该顺着入口的结构漏出去——入口一旦带上它们，改鉴权就变成了
// 改入口的对外契约。
//
// 这里搬的是**有消费者**的字段：ID 用于计费，KeyID 与 RateLimitRPM 用于
// 限流（见 api.RateLimiter）。其余字段（例如 Username）没有消费者就不搬，
// 要加时在这里显式加一行，而不是在两个包里各有一份悄悄同步的字段表。
type authenticator struct {
	store *auth.Store
}

// Authenticate 解析请求里的 API Key。
//
// 错误原样往上抛，不做包装：公共入口只按「是不是未认证」分两类
// （401 与 5xx），而那个区分由 auth.Error 表达，包一层反而会把它盖掉。
func (a authenticator) Authenticate(ctx context.Context, r *http.Request) (api.User, error) {
	user, err := a.store.Authenticate(ctx, r)
	if err != nil {
		return api.User{}, err
	}
	return api.User{ID: user.ID, KeyID: user.KeyID, RateLimitRPM: user.RateLimitRPM}, nil
}

// 编译期断言：适配器要满足公共入口对鉴权的要求。
//
// 写在装配层而不是 auth 包：auth 不该 import 入口，而「两边对得上」
// 这件事必须有人检查——放在这里，签名一改就在改的那一次编译失败。
var _ api.Authenticator = authenticator{}

// 编译期断言：账本存储要同时满足网关的两个落库接口。
//
// 同上，写在装配层：store 与 gateway 互不 import，接不上只会在装配时
// 暴露，而那时已经是运行期了。
var (
	_ gateway.ReservationStore = (*store.Store)(nil)
	_ gateway.SettlementStore  = (*store.Store)(nil)
)

// 编译期断言：限流实现要满足公共入口对它的要求。
//
// 同上，写在装配层：api 不该 import 具体实现，而「两边对得上」必须有人检查
// ——放在这里，Allow 的签名一改就在改的那一次编译失败。
var _ api.RateLimiter = (*ratelimit.Limiter)(nil)
