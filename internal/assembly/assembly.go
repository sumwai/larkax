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

	// RedisDSN 是共享计数（限流）用的 Redis 连接串，来自 LARKAX_REDIS_DSN。
	//
	// 空表示这个部署不启用 Redis：限流退回进程内计数，也就是只有单实例
	// 正确。解析不了的值会让 Build 失败（那是配置错误），而解析得了但连不上
	// 只会降级（那是故障）——两者的处置不同，见 buildRateLimiter。
	RedisDSN string

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

	// RateLimiter 是按 key 的 RPM 限流器。
	//
	// 导出它有两个用处，都在**内部监听**那一侧：只读的用量查询
	// （RegisterInternalOps），以及进程退出时释放共享存储的连接池（Close）。
	// 它**不**该被公共入口之外的东西拿去改行为——这条路径上只有读。
	//
	// 为 nil 时表示这个 App 没有限流器（worker 角色：它不接推理流量）。
	RateLimiter *ratelimit.Limiter
}

// RegisterInternalOps 把本包在**内部监听**上的只读端点挂到 mux 上。
//
// 由 cmd/larkax 在装内部端口时调用。为什么不是本包自己起一个 mux：那些端点
// 要与 /healthz、/readyz、/metrics 共用同一个监听，而监听怎么起、绑在哪个
// 地址上属于命令行的职责（见 cmd/larkax 的 newInternalHandler）。
//
// 没有限流器时什么都不挂：worker 角色不接推理流量，它答不出用量（它连
// 限流器都没有）。挂一个「永远说不清」的端点比不挂更坏——它会让查询的人
// 以为那个实例上就是 0。
func (a *App) RegisterInternalOps(mux *http.ServeMux) {
	if a.RateLimiter == nil {
		return
	}
	api.RegisterRateLimitUsage(mux, a.RateLimiter)
}

// Close 释放装配期建立的连接池。
//
// 今天只有限流器的共享存储那一个。它没有排空语义（没有在途的写要等），
// 所以进程退出时调一次就够。
func (a *App) Close() error {
	if a.RateLimiter == nil {
		return nil
	}
	return a.RateLimiter.Close()
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
	// maxOutputTokensKey 是单次输出上限的全局默认值（token）。
	maxOutputTokensKey = "billing.max_output_tokens"
	// streamFreezeStepKey 是流式请求递进冻结的阈值段（token）。
	streamFreezeStepKey = "billing.stream_freeze_step"
)

// 单次输出上限允许的范围（token）。
//
// 下界 1：0 与负数在语义上不成立。这一条判据说的是「一次请求最多冻住多少
// 余额」，非正的数会让它要么拒掉一切、要么什么都不判。
//
// 上界一百万：**这不是一个调参旋钮，而是「这个数还算不算一个界」**。单次
// 输出一百万 token 在任何意义上都不是一次调用（它比任何模型的上下文窗口都
// 大），配到那个量级上等于把这条判据关掉——而「关掉」与「配得很宽」在请求上
// 同形，那正是这条判据要避免的东西。
const (
	minMaxOutputTokens = 1
	maxMaxOutputTokens = 1 << 20
)

// 流式递进冻结的阈值段允许的范围（token）。
//
// 下界 1：0 与负数在语义上不成立（见 pipeline.ErrNoFreezeStep）。配成那样时
// 网关会拒掉**每一个**流式请求，而现场看到的是一批 5xx——那种失败与代码故障
// 同形，所以在启动时判掉。
//
// 上界复用单次输出上限那个数，理由也一样：段比声明上界还大时，第一段就把整笔
// 冻满，递进冻结整条不发生——那是把这条判据关掉，而「关掉」与「配得很宽」在
// 请求上长得一模一样。
const (
	minStreamFreezeStep = 1
	maxStreamFreezeStep = maxMaxOutputTokens
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

	// 单次输出上限的全局默认值。判据在请求那条路上（见 gateway.checkOutputCap），
	// 而它**只在目录里没登记 output_limit 的模型上生效**——所以配错了不会有
	// 任何别的东西报出来，只表现为一批请求多冻了余额或者突然开始 400。
	// 那一类错在启动时判掉最省事，于是范围校验放在这里。
	maxOutputTokens, err := cfg.Settings.Int(maxOutputTokensKey)
	if err != nil {
		return nil, fmt.Errorf("读取单次输出上限失败: %w", err)
	}
	if maxOutputTokens < minMaxOutputTokens || maxOutputTokens > maxMaxOutputTokens {
		return nil, fmt.Errorf("settings.%s = %d 超出允许范围 [%d, %d] token",
			maxOutputTokensKey, maxOutputTokens, minMaxOutputTokens, maxMaxOutputTokens)
	}

	// 流式递进冻结的阈值段。它同时是「多久检查一次」与「冻结额最多领先已产出量
	// 多少」——一个数担两个角色是有意的（见 gateway.Gateway.StreamFreezeStep）。
	//
	// 与其他键一样在这里判范围：配错的表现是流式那条路回一批 5xx（网关拒掉
	// 每一个流式请求），而「配置错」与「代码坏」在那一批 5xx 里分不出来。
	streamFreezeStep, err := cfg.Settings.Int(streamFreezeStepKey)
	if err != nil {
		return nil, fmt.Errorf("读取流式递进冻结的阈值段失败: %w", err)
	}
	if streamFreezeStep < minStreamFreezeStep || streamFreezeStep > maxStreamFreezeStep {
		return nil, fmt.Errorf("settings.%s = %d 超出允许范围 [%d, %d] token",
			streamFreezeStepKey, streamFreezeStep, minStreamFreezeStep, maxStreamFreezeStep)
	}

	cal, calendarEnv, err := buildCalendar(cfg.DB)
	if err != nil {
		return nil, err
	}
	// 插件那一路的宿主能力。日历与计价那一路共用同一个 *calendar.Calendar
	// （它不带请求状态，见 buildCalendar 的注释），KV 是插件专有的。
	//
	// **这份 wiring 是「允许申请哪些能力」的唯一来源**（见它的 provided）：
	// 注册校验不再读一份手写的清单，而是问这里真的接上了什么。
	wiring := pluginWiring{
		logger:   logger,
		now:      cfg.Now,
		calendar: pluginCalendar{cal: cal, peak: pluginPeakSchedule},
		kv:       pluginstore.NewKV(cfg.DB),
	}

	plugins, pluginLimits, err := buildPlugins(cfg, logger, wiring)
	if err != nil {
		return nil, err
	}

	// 账本存储同时满足预留与结算两个接口。它的方法名与签名与 gateway
	// 声明的逐字一致，所以这里没有适配器——那正是两边刻意对齐的地方。
	ledgerStore := store.New(cfg.DB)

	// 别名的两个读点共用这一个实例：选路按选中的渠道取覆盖，预留按候选集
	// 取输出上界（见 gateway.outputBound），两处读的是同一张表。分成两个
	// 实例不会有任何东西报错，但「选路时用的覆盖」与「算上界时用的覆盖」
	// 会来自两次不同的读。
	aliasCatalog := catalog.New(cfg.DB)

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
		Router: &route.Router{Now: cfg.Now, Aliases: aliasCatalog},
		// AliasOverrides 是预留要用的那一半：别名上的 max_tokens 覆盖顶掉
		// 调用方给的那个数，所以预留的输出上界要按覆盖之后的算。不接这一项
		// 请求会失败（见 gateway.ErrNoAliasOverrideSource）——上界算错的表现
		// 是少收，而那个错没有任何东西会报出来。
		AliasOverrides: aliasCatalog,
		// MaxOutputTokens 是单次输出上限的全局默认值：调用方声明的
		// max_tokens 超过它（目录里没登记 output_limit 时）会被拒。
		// 它必须在装配期就是正的（见下面那次校验）——不接或接成 0 的表现
		// 是这条判据整条失效，而「没配上限」与「上限配得很宽」在请求上
		// 长得一模一样。
		MaxOutputTokens: maxOutputTokens,
		// StreamFreezeStep 是流式请求递进冻结的阈值段：流式那条路不按声明的
		// max_tokens 冻满，而是先冻一段、之后随流补冻（见 gateway/stream.go
		// 的 streamFreeze 与 #498）。它必须在装配期就是正的（见上面那次校验）
		// ——不接或接成 0 的表现是每一个流式请求都失败，而不是安静地退回到
		// 某个默认行为。
		StreamFreezeStep: streamFreezeStep,
		Candidates:       channels.New(cfg.DB, cfg.Sealer),
		Reservations:     ledgerStore,
		Settlements:      ledgerStore,
		// 写入侧与读取侧共用同一个包：两边读写的列是同一批，
		// 分开放会让「写进去的值读出来不对」这类错没有唯一的归属。
		Recorder:  channels.NewRecorder(cfg.DB, staleAfter),
		Redactor:  snapshots.Redactor(),
		Snapshots: snapshots.Sink(),
		// 决策那条表面的脱敏器与上面两个同源、不同表：两条表面的字段
		// 没有一个共用的（见 internal/snapshot 的 decision.go）。
		// 它与 Snapshots 各自独立地为 nil——装配层少接一个字段时的表现
		// 应当是「这条路径不采快照」，而不是请求失败。
		DecisionRedactor: snapshots.DecisionRedactor(),
		Executor:         cfg.Executor,
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
	// 限流的计数放在哪，由 LARKAX_REDIS_DSN 决定；三种形态各有一条启动日志。
	//
	// 这件事必须说出来。「设了额度」与「真的限住了」在配置、响应码和请求
	// 日志上都一模一样，唯一能把它们分开的就是这句话——而默认沉默的话，
	// 一个按 60 RPM 配了容量、实际被打到 120 RPM 的部署没有任何提示。
	limiter, err := buildRateLimiter(cfg, logger)
	if err != nil {
		return nil, err
	}
	handler.RateLimiter = limiter
	// 额度这道早拒绝与账本共用同一个 Store：它读的是 billing_requests，
	// 而那正是账本存储管的表。权威判定在预留那个事务里（见 api.QuotaChecker），
	// 两处共用 internal/keyquota 里同一份口径。
	handler.Quota = ledgerStore
	// 插件链路：注册表与限额来自启动期（见 buildPlugins），宿主能力按请求
	// 现拼。三者一起给，缺一个的后果不同——注册表为 nil 时插件根本不跑
	// （与关掉开关同形），宿主构造为 nil 时插件拿不到任何宿主能力。
	//
	// 限额为零值**不是**「没有上限」，也不是立刻超时：`sandbox.Limits.normalize`
	// 会把零与负数当成「没设」，补成 `DefaultLimits`（5s 调用上限、64MB 内存）。
	// 也就是说 settings 里那四个键都没配时，插件跑在默认限额下——**能跑**。
	//
	// 日历与 KV 这一版都接上了（见 wiring）：接上之后插件不再需要自己折算时区，
	// 也有了自己的持久化存储；接不上时那两项能力**不注入**，而「能不能注入」
	// 与「注册时允许申请哪些」读的是同一份 wiring（见 pluginWiring.provided）。
	handler.Plugins = plugins
	handler.PluginLimits = pluginLimits
	handler.PluginHost = wiring.host
	// 幂等记录的读与写走同一个 Store：入口读它、落库层在预留那个事务里写它，
	// 两边读写的列是同一批（见 internal/idempotency 的包注释）。
	//
	// 建立记录那一步不在这里接线：它必须与预留同一个事务，所以入口拿到的
	// 只有「读 / 写回结局」这三个方法（见 api.IdempotencyStore）。
	handler.Idempotency = idempotency.New(cfg.DB)

	return &App{
		Public:       observe.Middleware(handler),
		Gateway:      gw,
		Snapshots:    snapshots,
		Plugins:      plugins,
		PluginLimits: pluginLimits,
		RateLimiter:  limiter,
	}, nil
}

// redisProbeTimeout 是启动期那一次探活的时间上限。
//
// 它在启动路径上，而一个黑洞地址（连不上也不拒绝）会让这一行一直等下去：
// 取一个「运维能接受启动慢两秒」的量级，而不是热路径那个几十毫秒。
const redisProbeTimeout = 2 * time.Second

// buildRateLimiter 装出 RPM 限流器，并把「计数放在哪」写进启动日志。
//
// # 三种形态，三条不同的日志
//
//	配了 LARKAX_REDIS_DSN、且探活通过 → 精确：所有实例共用一份计数
//	配了、但连不上                   → 近似：退回进程内计数，之后自动重试
//	没配                             → 近似：只有进程内计数，单实例正确
//
// 三种都要说话，而且措辞不同：这一层最坏的情形不是「限流没生效」，是
// 「看起来生效了」。
//
// # 连不上为什么不拒绝启动
//
// 这与「装出来的东西要么能用，要么启动就失败」不冲突。拒绝启动换来的是
// **整个数据面不可用**，而这一类要保的恰恰是「共享存储挂了不能影响正常用户」
// （见 internal/ratelimit 的降级那一段）：连不上 Redis 的实例仍然能正确服务
// 请求，只是那一分钟里拦得松一点。把它拦在启动之外，代价的方向反了。
//
// 探活只是为了让日志说真话——不探的话只能写「配了 Redis」，而「配了」与
// 「能用」正是这里要说清的两件事。
func buildRateLimiter(cfg Config, logger *slog.Logger) (*ratelimit.Limiter, error) {
	// 探活用 Background 而不是某个请求的 context：此刻还没有请求。
	//
	// 两档都要探。不探的话，那个「精确还是近似」的信号要等到第一个请求才
	// 出现，而「还没来过请求」与「这个实例上根本没接限流器」在 /metrics 上
	// 都没有这一行——两件事在那上面长得一样。
	ctx, cancel := context.WithTimeout(context.Background(), redisProbeTimeout)
	defer cancel()

	if cfg.RedisDSN == "" {
		limiter := ratelimit.New(cfg.Now)
		logger.Warn("RPM 限流的计数在进程内：没有配置 LARKAX_REDIS_DSN，" +
			"多实例部署时各实例独立计数、额度不共享（总放行量最高到配置值 × 实例数）")
		// 没有共享存储时这一次探测不会失败（它只把信号记成近似），所以
		// 不接它的返回值。
		_ = limiter.Probe(ctx)
		return limiter, nil
	}

	store, err := ratelimit.OpenRedis(cfg.RedisDSN)
	if err != nil {
		return nil, fmt.Errorf("构造 RPM 限流器的共享计数失败: %w", err)
	}
	limiter := ratelimit.New(cfg.Now, ratelimit.WithStore(store))

	if err := limiter.Probe(ctx); err != nil {
		logger.Warn("RPM 限流的共享计数当前连不上，暂时按进程内计数（判定偏松，请求不受影响）",
			"err", err)
		return limiter, nil
	}
	logger.Info("RPM 限流的计数在共享存储里：各实例共用一份，放行总量按配置值生效")
	return limiter, nil
}

// buildPlugins 按 settings 里的开关加载插件。
//
// # 去哪张表、装了哪些能力
//
// wiring 一路传到 store.LoadRegistry 与装配出来的宿主构造：注册时按它**真的
// 提供**的能力收窄（见 pluginWiring.provided），请求期按它注入。两处读同一份，
// 于是不会出现「申请到了、运行期是 undefined」。
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
func buildPlugins(cfg Config, logger *slog.Logger, wiring pluginWiring) (*plugin.Registry, sandbox.Limits, error) {
	pluginCfg, err := plugin.LoadConfig(cfg.Settings)
	if err != nil {
		return nil, sandbox.Limits{}, fmt.Errorf("读取插件配置失败: %w", err)
	}
	if !pluginCfg.Enabled {
		return nil, pluginCfg.Limits, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), pluginLoadTimeout)
	defer cancel()

	registry, err := pluginstore.New(cfg.DB).LoadRegistry(ctx, logger, wiring.provided())
	if err != nil {
		return nil, sandbox.Limits{}, fmt.Errorf("加载插件失败: %w", err)
	}
	return registry, pluginCfg.Limits, nil
}

// pluginWiring 是这次装配**给插件链路接上的东西**。
//
// # 它为什么是一个结构体，而不是几个参数
//
// 三个用处读的是同一份东西：注册期算「允许申请哪些能力」、请求期拼宿主能力、
// 以及将来那句「这次装配给了什么」的自述。分开传的话，同一个 nil 判断会在
// 三处各写一遍，而漏掉的那一处不报错——表现为「注册时说有 kv，运行期拿到
// undefined」，也就是这一片要修的那个坑。
//
// # 字段可空，「空」是有意义的
//
// calendar / kv 为 nil 表示这次装配没接那一项：它既不出现在允许集里（于是
// 申请它的插件注册失败），也不注入（于是运行期不会有一个永远失败的空壳）。
// 生产路径上两者都非 nil（Build 里现造），而**测试与将来的部分装配**正是
// 靠给一个 nil 的字段来构造「宿主没提供某个能力」那一种情形。
type pluginWiring struct {
	logger   *slog.Logger
	now      func() time.Time
	calendar host.CalendarSource
	kv       host.KVStore
}

// 编译期钉不住「这个签名与 api.Handler 那个字段一致」——方法值这一层由
// `handler.PluginHost = wiring.host` 那一行管：签名漂了那里直接编译不过。

// provided 返回这套装配**真的能提供**的能力名，按字典序。
//
// **这是允许集的唯一来源。** 判据在 host.Available 里，与请求期注入读的是
// 同一张定义表：能申请的与能提供的从此同源，而不是两份各自维护的清单
// （那两份曾经让它申请 kv 注册成功、运行期拿到 undefined）。
//
// 只把三个字段里非 nil 的那些交出去：Env 里其余字段（Plugin / RequestID /
// UserID）是请求期才有的，判定不看它们，见 host.Available 的注释。
func (w pluginWiring) provided() []string {
	return host.Available(host.Env{Calendar: w.calendar, KV: w.kv})
}

// host 按请求构造插件看到的宿主能力。
//
// 插件 key 由注册表选中之后回填（见 plugin.HostBuilder）；一块接的日历与 KV
// 是进程级的对象，可以并发共享（见 buildCalendar 的注释与 store.KV）。
func (w pluginWiring) host(p *plugin.Plugin, requestID string, userID int64) sandbox.Host {
	return host.Build(host.Env{
		Logger:    w.logger,
		Plugin:    p.Key,
		RequestID: requestID,
		UserID:    userID,
		Calendar:  w.calendar,
		KV:        w.kv,
		Now:       w.now,
	})
}

// pluginCalendar 把 internal/calendar 的判定接到 host.CalendarSource 上。
//
// # 为什么适配器在这一层
//
// host 那个接口是窄接口（不 import internal/calendar，理由见 host.CalendarSource），
// 于是把两边连起来是装配的事：只有这一层同时知道「日历库怎么判」与「宿主函数
// 要什么形状」。
//
// # 它调的是什么
//
//   - Day 走 calendar.Calendar.At：按地区时区把时刻折成当地日期、再查那一天的
//     判定。当地钟点与星期从同一个时区的时刻取（JS 的 getDay() 是 0=周日，
//     time.Weekday 也是 0=周日，直接转）。
//   - Peak 走 calendar.Calendar.PeakAt，窗口取自下面那个排班常量。
type pluginCalendar struct {
	cal  *calendar.Calendar
	peak calendar.PeakSchedule
}

var _ host.CalendarSource = pluginCalendar{}

// pluginPeakSchedule 是插件 calendar 能力用的高峰排班：**空的**。
//
// 空不是「还没写」，而是这份部署此刻的真实答案：高峰窗口从哪来（库里还是配置
// 里）还没有定（见 internal/calendar 的包注释与 docs/calendar-data.md），所以
// 这里没有任何窗口可传。空排班在 calendar.PeakAt 里的语义是明确的：已知的
// 日子里一律 off_peak，日子未知时 unknown——不会凭星期几猜一天。
//
// 将来定了「高峰窗口从哪来」，改的是这一个值（或者由装配从配置里读），而不是
// 在适配器里重写一遍判定。
var pluginPeakSchedule = calendar.PeakSchedule{}

// Day 判定某个时刻落在哪一天、那天是工作日还是休息日。
func (a pluginCalendar) Day(ctx context.Context, region string, instant time.Time) (host.DayInfo, error) {
	// 地区名由插件给出，日历库按它查表并给出时区：认不出这个地区时它会报错，
	// 而这里原样往上传——插件侧收到「能力不可用」比收到一个猜出来的判定好。
	st, err := a.cal.At(ctx, calendar.Region(region), instant)
	if err != nil {
		return host.DayInfo{}, err
	}
	loc, err := calendar.Timezone(calendar.Region(region))
	if err != nil {
		return host.DayInfo{}, err
	}
	local := instant.In(loc)
	return host.DayInfo{
		// Kind 直接取自日历库的三态判定，于是 host 那一侧那三个字面量
		// （workday / rest_day / unknown）与 internal/calendar 的取值对不上时，
		// 这里不会静默降级——它们本来就是同一份字符串。
		Kind:          string(st.Kind),
		Name:          st.Name,
		MakeupWorkday: st.MakeupWorkday,
		LocalHour:     local.Hour(),
		LocalWeekday:  int(local.Weekday()),
	}, nil
}

// Peak 判定某个时刻是高峰还是低谷。
//
// 返回值就是 calendar.PeakKind 的取值：peak / off_peak / unknown（三态，
// 未知不会被压成低谷）。**当前没有插件能调到它**：SDK 的 calendar 命名空间里
// 没有 peak 这个函数（见 internal/plugin/sandbox 的 sdkSource），接上它要先定
// 「高峰窗口从哪来」——那是一条独立的决定（见 pluginPeakSchedule 与本文件在
// scripts/deadmethods-allowlist.txt 里的那一条）。
func (a pluginCalendar) Peak(ctx context.Context, region string, instant time.Time) (string, error) {
	st, err := a.cal.PeakAt(ctx, calendar.Region(region), instant, a.peak)
	if err != nil {
		return "", err
	}
	return string(st.Peak), nil
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
//
// # 为什么把 *calendar.Calendar 也交出去
//
// 它有两个消费者：计价那一半走 calendaradapter（上面那条路），插件那一半走
// pluginCalendar（宿主能力的窄接口）。两者共用同一个对象是有意的——它不带
// 请求状态，也**顺手让「两个消费者按不同口径折算时区」这件事不可能发生**：
// 同一个时刻在两条路径上落到同一天。
func buildCalendar(db *sql.DB) (*calendar.Calendar, *calendaradapter.Adapter, error) {
	cal, err := calendar.New(calendarstore.NewSource(db))
	if err != nil {
		return nil, nil, fmt.Errorf("构造日历失败: %w", err)
	}
	adapter, err := calendaradapter.New(cal)
	if err != nil {
		return nil, nil, fmt.Errorf("构造日历适配器失败: %w", err)
	}
	return cal, adapter, nil
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

// 编译期断言：别名覆盖来源要满足网关对它的要求。
//
// 与上一条同一个理由，而这一条更容易漏：Route 与 AliasOverrides 是**两个**
// 字段，两个都实现自同一个 *catalog.Catalog，只接上一个时编译、启动、选路
// 全都正常，只有预留那条路会失败（见 gateway.ErrNoAliasOverrideSource）。
var _ gateway.AliasOverrideSource = (*catalog.Catalog)(nil)

// 编译期断言：限流实现要满足公共入口对它的要求。
//
// 同上，写在装配层：api 不该 import 具体实现，而「两边对得上」必须有人检查
// ——放在这里，Allow 的签名一改就在改的那一次编译失败。
var _ api.RateLimiter = (*ratelimit.Limiter)(nil)

// 编译期断言：限流实现要满足那条只读查询对它的要求（见 RegisterInternalOps）。
//
// 与上一条同一个理由：那个接口由 api 声明（它不该 import 具体实现），
// 所以「实现接得上」只能在装配层检查。
var _ api.RateLimitUsageSource = (*ratelimit.Limiter)(nil)
