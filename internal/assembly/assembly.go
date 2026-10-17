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
// 其余依赖（候选来源、账本存储、快照链路、鉴权、日历）都是本包直接构造的：
// 它们没有第二种实现，多一层参数只会让调用方多写几行没有信息的代码。
// 日历尤其如此——它的数据源只有「库里的 calendar_days / calendar_years」
// 这一条路，把它做成可传可不传的参数，接不接得上就没有任何一步会失败。
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
	"github.com/sumwai/larkax/internal/billing/calendaradapter"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/calendar"
	calendarstore "github.com/sumwai/larkax/internal/calendar/store"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/plugin/host"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/ratelimit"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/snapshotwire"
	"github.com/sumwai/larkax/internal/tokens"
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

	// Executor 是调上游的能力，由调用方构造。两条路径（chat 与决策）都要能走，
	// 见 Executor 那个类型的注释。
	Executor Executor

	// Rules 是计价规则的来源，由调用方构造。
	Rules pipeline.RuleSource

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

	// Plugins 是启动时从 plugins 表加载出来的注册表，plugin.enabled 为
	// false 时为 nil。
	//
	// 它已经挂在请求路径上：五个挂载点都接在入口与网关里（onRequest 见
	// internal/api 的 runRequestHook，其余四个见同文件的 pluginHooks 与
	// gateway.Hooks）。两条路各自只跑自己那条路上会发生的那几个——onResponse
	// 只在非流式那条路上跑，onStream 只在流式那条路上跑。
	Plugins *plugin.Registry

	// PluginLimits 是插件每次调用的限额，来自 settings 表。
	//
	// 它与 Plugins 一起导出，是为了让接线那一片**只有一处**把配置翻成
	// sandbox.Limits：让调用方自己去读那四个键的话，两份映射迟早不一致，
	// 而那种不一致的表现是「插件跑在另一套限额下」——不报错，只是行为不同。
	PluginLimits sandbox.Limits
}

// pluginLoadTimeout 是启动期读 plugins 表的时间上限。
//
// Build 没有 context（它做的是装配，不是请求），而启动期的读表不该无限等：
// 一个连不上的库会让进程停在那里不报错，现场看起来像「服务起来了但什么都不接」
// ——那比启动失败难查得多。
const pluginLoadTimeout = 30 * time.Second

// cacheSize 是计价表达式编译缓存的容量。
//
// 显式传一个缓存，而不是让 pipeline 在第一次求值时自己建：它那一步是
// 无锁的懒赋值，两个并发请求同时走到就会写同一个字段——那是数据竞争，
// 而竞争窗口只在启动后的头几条请求上，现场表现为偶发的告警，很难查。
const cacheSize = 256

// 本包要读的运行期配置键。
//
// 写成常量而不是散落的字面量：键名写错的表现是「启动就失败」，而那样
// 一眼看不出是拼写问题。三个都与 settings.Registry 里声明的一致。
const (
	// regionKey 是分时计价默认地区。
	regionKey = "calendar.default_region"
	// assetKey 是结算用的资产品种。
	assetKey = "billing.default_asset"
	// probeIntervalKey 是额度探测的默认间隔（秒）。
	probeIntervalKey = "quota.probe_interval_seconds"
)

// 探测间隔允许的范围（秒）。
//
// 两个界都在**换算之前**判。time.Duration 以纳秒计，秒数乘 1e9 会溢出，
// 而 Go 的整数溢出不报错——它绕回一个看起来正常的数。实测两个绕法：
//
//	-9223372036854775807 秒 → 换算后 +1s，再乘 staleAfterFactor 得 +3s
//	-3074457345618258603 秒 → 换算后 +1708031h…，再乘得 -1s
//
// 两个都会装配成功、服务照常启动，而阈值是一个谁也没配过的值。只判上界
// 挡不住它们——它们都「小于」上界——所以下界必须一起判，判的是配置里那个
// 秒数本身，而不是换算之后的结果。
//
// 上界一年：远超任何合理的探测间隔。下界 1 秒：0 与负数在语义上就不成立
// （见 quota.SelfAccountingStaleAfter），在这里一并挡掉，顺带把「负数绕成
// 正数」这条整个堵死。
const (
	minProbeIntervalSeconds = 1
	maxProbeIntervalSeconds = 365 * 24 * 60 * 60
)

// Executor 是装配层要求的上游执行器：chat 与决策两条路都要能走。
//
// 两个接口嵌在一起，而不是在 Config 上分成两个字段：生产里它们永远是同一个
// 对象（*upstream.Executor，它按渠道类型查表，两条路共用同一份注册表）。
// 分成两个字段能装出「只接上一个」的状态，而那种状态的现场是一半路径回 501，
// 看代码的人却会以为它们本来就是一回事。
type Executor interface {
	gateway.Executor
	gateway.DecideExecutor
}

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

	// 地区只在这里读一次，之后顺着 pipeline.Region → expr.Env.Region 走到
	// 日历上：求值时由日历按它选时区、查那一天的安排（见 internal/billing/expr
	// 的 dayInfo 与 internal/billing/calendaradapter）。
	//
	// 所以日历对象本身不带地区，装配期也不需要拿它去查什么。同一个进程要能
	// 回答任何地区的日期问题，而「这次请求按哪个地区算」是求值的入参，
	// 不是装配期的常量。
	region, err := cfg.Settings.String(regionKey)
	if err != nil {
		return nil, fmt.Errorf("读取分时计价地区失败: %w", err)
	}

	// 结算用的资产品种。它是运行期配置，而计费层是纯编排、不读配置，
	// 所以在这里读出来、经 api 的 Handler 填进每个请求
	//（见 api.Handler.DefaultAsset 与 pipeline.Request.Asset）。
	//
	// 顺带校验它是不是账本认得出来的品种（见 ledger.Asset.Valid）。
	// 不校验的话，配错的表现是每个请求都在结算那一步失败，而错误信息里
	// 看不出是配置问题——「装出来的东西要么能用，要么启动就失败」这条
	// 在这里同样成立。
	asset, err := cfg.Settings.String(assetKey)
	if err != nil {
		return nil, fmt.Errorf("读取结算资产品种失败: %w", err)
	}
	if !ledger.Asset(asset).Valid() {
		return nil, fmt.Errorf("settings.%s = %q 不是账本支持的资产品种：目前只认 %q",
			assetKey, asset, ledger.AssetCNY)
	}

	staleAfter, err := quotaStaleAfter(cfg.Settings)
	if err != nil {
		return nil, err
	}

	calendarEnv, err := buildCalendar(cfg.DB)
	if err != nil {
		return nil, err
	}

	plugins, pluginLimits, err := buildPlugins(cfg, logger)
	if err != nil {
		return nil, err
	}

	// 账本存储同时满足预留与结算两个接口。它的方法名与签名与 gateway
	// 声明的逐字一致，所以这里没有适配器——那正是两边刻意对齐的地方。
	ledgerStore := store.New(cfg.DB)

	gw := &gateway.Gateway{
		Pipeline: &pipeline.Pipeline{
			Rules:    cfg.Rules,
			Programs: expr.NewCache(cacheSize),
			Calendar: calendarEnv,
			Region:   region,
			Now:      cfg.Now,
		},
		// Aliases 是选路要用的那一半：选路之后、编码请求之前，按选中的渠道
		// 把规范名翻成上游名。不接这一项的表现是**别名配了不生效**——上游
		// 收到的永远是规范名，只要两者不同就必然打不通，而现场看到的只是
		// 上游说这个模型不存在。
		Router:       &route.Router{Now: cfg.Now, Aliases: catalog.New(cfg.DB)},
		Candidates:   channels.New(cfg.DB, cfg.Sealer),
		Reservations: ledgerStore,
		Settlements:  ledgerStore,
		// 写入侧与读取侧共用同一个包：两边读写的列是同一批，
		// 分开放会让「写进去的值读出来不对」这类错没有唯一的归属。
		Recorder:  channels.NewRecorder(cfg.DB, staleAfter),
		Redactor:  snapshots.Redactor(),
		Snapshots: snapshots.Sink(),
		Executor:  cfg.Executor,
		// 决策那条路径与 chat 共用同一个执行器：它按渠道类型查表，
		// 而「这个类型支不支持决策」由适配器有没有实现 systemone.Decider
		// 决定（见 upstream.Executor.Decide）。填同一个值而不是再要一个
		// cfg 字段：两个执行器分开装配时，「其中一个没接上」的表现是
		// 一半路径回 501，而那时看代码的人会以为它们本来就是一回事。
		Decider: cfg.Executor,
		// 计数器按需加载词表（首次精确计数约 25ms），构造本身是廉价的，
		// 所以这里直接建一个而不是做成参数：它没有第二种实现，而做成
		// 参数还多一个坏处——调用方传 nil 时网关会安静地退到启发式估算
		// （Counter 允许为 nil），「忘了接」就没有任何一步会失败。
		//
		// 预热不在这里做：那一步不能失败，而 Build 的契约是「要么能用，
		// 要么启动就失败」。调用方拿到 App 之后自己预热（见 cmd/larkax
		// 的 warmVocab）。
		Counter: tokens.NewCounter(),
		Now:     cfg.Now,
	}

	handler := api.New(authenticator{store: auth.New(cfg.DB)}, gw)
	// 决策入口的协作者与 Chatter 是同一个对象（*gateway.Gateway 两条路都实现）。
	// 分开赋值而不是让 api.New 收两个：那样每个构造点都要多传一个参数，
	// 而这两个值在生产里永远是同一个。
	handler.Decider = gw
	handler.Version = cfg.Version
	handler.DefaultAsset = asset
	handler.Now = cfg.Now
	handler.RateLimiter = ratelimit.New(cfg.Now)
	// 额度这道早拒绝与账本共用同一个 Store：它读的是 billing_requests，
	// 而那正是账本存储管的表。权威判定在预留那个事务里（见 api.QuotaChecker），
	// 两处共用 internal/keyquota 里同一份口径。
	handler.Quota = ledgerStore
	// 插件链路：注册表与限额来自启动期（见 buildPlugins），宿主能力按请求
	// 现拼。三者一起给，缺一个的后果不同——注册表为 nil 时插件根本不跑
	// （与关掉开关同形），宿主构造为 nil 时插件拿不到 log/calendar/kv。
	//
	// 限额为零值**不是**「没有上限」，也不是立刻超时：`sandbox.Limits.normalize`
	// 会把零与负数当成「没设」，补成 `DefaultLimits`（5s 调用上限、64MB 内存）。
	// 也就是说 settings 里那四个键都没配时，插件跑在默认限额下——**能跑**。
	//
	// 宿主能力里的日历与 KV 这一版**都没有接**：KV 还没有生产实现
	// （只有插件树里的测试桩），日历要先有一个 host.CalendarSource 适配
	// internal/calendar——两件事各自是另一片。没接的能力不注入，插件侧
	// 收到的是「能力未启用」而不是一个永远失败的空壳（见 host.Build）。
	handler.Plugins = plugins
	handler.PluginLimits = pluginLimits
	handler.PluginHost = func(p *plugin.Plugin, requestID string, userID int64) sandbox.Host {
		return host.Build(host.Env{
			Logger:    logger,
			Plugin:    p.Key,
			RequestID: requestID,
			UserID:    userID,
			Now:       cfg.Now,
		})
	}
	// 幂等记录的读与写走同一个 Store：入口读它、落库层在预留那个事务里写它，
	// 两边读写的列是同一批（见 internal/idempotency 的包注释）。
	//
	// 建立记录那一步不在这里接线：它必须与预留同一个事务，所以入口拿到的
	// 只有「读 / 写回结局」这三个方法（见 api.IdempotencyStore）。
	handler.Idempotency = idempotency.New(cfg.DB)

	// 限流按 key 的额度生效了，但计数在**进程内**：多实例部署时每个实例
	// 各计一份，一把 60 RPM 的 key 在 N 个实例上最多能打到 60×N RPM。
	//
	// 这件事必须说出来。「设了额度」与「真的限住了」在配置、响应码和请求
	// 日志上都一模一样，唯一能把它们分开的就是这句话——而默认沉默的话，
	// 一个按 60 RPM 配了容量、实际被打到 120 RPM 的部署没有任何提示。
	// 共享计数要有一个所有实例都连得上的存储，那是另一件事。
	logger.Warn("RPM 限流按进程内计数：多实例部署时各实例独立计数，额度不共享")

	return &App{
		Public:       observe.Middleware(handler),
		Gateway:      gw,
		Snapshots:    snapshots,
		Plugins:      plugins,
		PluginLimits: pluginLimits,
	}, nil
}

// buildPlugins 按 settings 里的开关加载插件。
//
// # 关掉时不读另外三个键
//
// plugin.enabled 为 false 时**不构造注册表，也不去读另外三个键**（见
// plugin.LoadConfig）：没开的功能不该因为一个坏配置把启动搞挂。判据是
// 「enabled=false 时把一个非法值配进 call_timeout_ms，服务照样起得来」——
// 那是唯一一个把「读了」与「没读」分开的观测量（见 settings_wiring_test.go）。
//
// 同一条理由在 internal/snapshotwire 也用过一次：那里是关掉时连留存期都不校验。
//
// # 加载失败是启动失败
//
// 与「一个坏插件」不同：读表失败（连接断了、表不在）说明这一份配置整体不可信，
// 装出来的服务会带着一个空注册表跑，而「插件全都没跑」与「没有插件」在日志里
// 长得一样。所以那一种在这里直接失败。
func buildPlugins(cfg Config, logger *slog.Logger) (*plugin.Registry, sandbox.Limits, error) {
	pluginCfg, err := plugin.LoadConfig(cfg.Settings)
	if err != nil {
		return nil, sandbox.Limits{}, fmt.Errorf("读取插件配置失败: %w", err)
	}
	if !pluginCfg.Enabled {
		return nil, pluginCfg.Limits, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), pluginLoadTimeout)
	defer cancel()

	registry, err := pluginstore.New(cfg.DB).LoadRegistry(ctx, logger)
	if err != nil {
		return nil, sandbox.Limits{}, fmt.Errorf("加载插件失败: %w", err)
	}
	return registry, pluginCfg.Limits, nil
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

// quotaStaleAfter 从 settings 读出额度探测的默认间隔，再导出「实测值开始
// 显得陈旧」的时长（见 quota.SelfAccountingStaleAfter）。
//
// 那个时长决定自记账写回库里的 confidence 衰减得多快——基准越旧，写回去的
// 数越低。它必须跟着配置走：写死的话，把探测间隔从 5 分钟改成 1 分钟，
// 阈值一动不动，而改配置的人以为自己改了（见 internal/channels 的 Recorder）。
//
// 间隔配成 0 或负数时拒绝启动而不是回落到某个默认值：阈值也非正时，任何
// 一次自记账都会立刻被判成陈旧，而它在库里与「配置生效了」长得一样。
//
// 范围检查在**换算之前**：换算本身会溢出，而溢出绕出来的数看着是正常的
// （见 minProbeIntervalSeconds / maxProbeIntervalSeconds 上的说明）。
func quotaStaleAfter(v *settings.Values) (time.Duration, error) {
	seconds, err := v.Int(probeIntervalKey)
	if err != nil {
		return 0, fmt.Errorf("读取额度探测间隔失败: %w", err)
	}
	if seconds < minProbeIntervalSeconds || seconds > maxProbeIntervalSeconds {
		return 0, fmt.Errorf("settings.%s = %d 秒，超出允许范围 [%d, %d] 秒",
			probeIntervalKey, seconds, minProbeIntervalSeconds, maxProbeIntervalSeconds)
	}
	staleAfter, err := quota.SelfAccountingStaleAfter(time.Duration(seconds) * time.Second)
	if err != nil {
		return 0, fmt.Errorf("settings.%s 不合法: %w", probeIntervalKey, err)
	}
	return staleAfter, nil
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

// buildCalendar 把日历数据源接到计价引擎要的那个接口上。
//
// # 三跳，只有这一层知道它们拼得起来
//
//	calendarstore.NewSource(db)  —— 读 calendar_days / calendar_years 两张表
//	calendar.New(src)            —— 判定「工作日 / 休息日 / 未知」
//	calendaradapter.New(cal)     —— 把判定结果转成 expr.Calendar 的形状
//
// 三段分属三个包，彼此不 import（理由写在各自的包文档里），所以只有装配层
// 知道它们能拼起来。少接任何一跳，用到日历的计价规则都会在运行期报
// ErrCalendarRequired——而那在响应上只是又一个 5xx，现场分不出「没接」
// 与「接错了」。
//
// # 一个进程一个对象，可以并发共享
//
// 返回的对象只构造这一次，之后被所有请求共用：
//
//   - *calendar.Calendar 只有 src 一个字段；
//   - *calendaradapter.Adapter 只有 cal 一个字段；
//   - src 是 *sql.DB 上的两次只读查询，而 *sql.DB 本身并发安全。
//
// 也就是说它**不带请求状态**，不必按请求构造——按请求构造只是多一次分配，
// 换不来任何隔离。
//
// # 装配期一次也不查
//
// 这里只把对象拼起来，不读日历数据。日历是逐年补的，未来年份查不到属于
// 正常状态（见 docs/calendar-data.md），拿它当启动条件会让跨年时的一次例行
// 重启起不来，而那时未必有任何规则用得到下一年的数据。缺数据要等到规则真的
// 调用 isWorkday() 时才暴露，那时报出的错误会说明这是配置缺口而非服务故障
// （见 internal/billing/calendaradapter）。
func buildCalendar(db *sql.DB) (*calendaradapter.Adapter, error) {
	cal, err := calendar.New(calendarstore.NewSource(db))
	if err != nil {
		return nil, fmt.Errorf("构造日历失败: %w", err)
	}
	adapter, err := calendaradapter.New(cal)
	if err != nil {
		return nil, fmt.Errorf("构造日历适配器失败: %w", err)
	}
	return adapter, nil
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
	return api.User{
		ID:           user.ID,
		KeyID:        user.KeyID,
		RateLimitRPM: user.RateLimitRPM,
		QuotaLimit:   user.QuotaLimit,
	}, nil
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

// 编译期断言：渠道健康度写入方要满足网关对它的要求。
//
// 同上：channels 与 gateway 互不 import（数据面不依赖入口层），接不上
// 只会在装配时暴露。它同时满足读取侧的接口（见 channels 包里的断言），
// 所以这一处接错会让候选集取不出来或者健康度写不进去。
var _ gateway.CallRecorder = (*channels.Recorder)(nil)

// 编译期断言：幂等存储要满足公共入口对它的要求。
//
// 同上：api 不该 import 具体实现，而「两边对得上」必须有人检查——
// 放在这里，三个方法的签名一改就在改的那一次编译失败。少了它，
// 接不上的表现是「幂等静默不生效」，而那与「没人带这个头」长得一样。
var _ api.IdempotencyStore = (*idempotency.Store)(nil)

// 编译期断言：别名来源要满足选路对它的要求。
//
// 同上：catalog 与 route 互不 import，接不上只会在装配时暴露。这一条尤其
// 要写在装配层——选路在 Aliases 为 nil 时会拒绝每个请求（见
// route.ErrNoAliasSource），而那是一个**运行期**才看得见的失败。
var _ route.ModelAliases = (*catalog.Catalog)(nil)

// 编译期断言：限流实现要满足公共入口对它的要求。
//
// 同上，写在装配层：api 不该 import 具体实现，而「两边对得上」必须有人检查
// ——放在这里，Allow 的签名一改就在改的那一次编译失败。
var _ api.RateLimiter = (*ratelimit.Limiter)(nil)
