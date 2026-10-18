// Command larkax 是 larkax 的主服务进程。
//
// 同一个二进制以 LARKAX_ROLE 环境变量区分数据面与控制面：
//   - gateway：承接用户推理请求。它的可用性直接等于 larkax 的可用性。
//   - control：控制台、账户、计费规则等管理面。它挂掉不影响推理。
//   - worker：后台任务（日历同步、额度探测、对账）。
//
// 最高原则是数据面不依赖控制面在线，因此三者共用一套依赖装配代码，
// 但启动的组件按角色裁剪，而不是让 gateway 顺带把控制面的依赖也拉起来。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sumwai/larkax/internal/api"
	"github.com/sumwai/larkax/internal/assembly"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/db"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/tokens"
	"github.com/sumwai/larkax/internal/upstream"
)

// version 由构建时注入：-ldflags "-X main.version=v0.1.0"。
// 未注入时为 dev，便于区分本地构建与正式发布。
var version = "dev"

// shutdownTimeout 是收到退出信号后等待在途请求结束的上限。
//
// 硬编码而不做成环境变量：它属于「改错了也不会造成事故」的那一类，
// 而且长流场景下几乎总该排空到这个长度。真需要调整时改代码，
// 比多一个谁都不知道该填多少的环境变量更清楚。
const shutdownTimeout = 30 * time.Second

// upstreamTimeout 是「上游回响应头」的时间上限。
//
// 它必须能容下**一次完整的生成**，而不是「首字节要多久」：非流式请求的
// 上游要等整段回复生成完才回响应头，一次长回复在这个上限之内才算正常。
// 定小了的表现是长回复被我们自己掐断，而调用方看到的是「上游失败」——
// 排查方向指向对方，实际是我们的超时。
//
// 硬编码而不做成环境变量，与 shutdownTimeout 同理：它属于「改错了也不会
// 造成事故」的那一类（改小了会失败得很显眼，改大了只是慢），真需要调整时
// 改代码比多一个没人知道该填多少的变量更清楚。
const upstreamTimeout = 10 * time.Minute

func main() {
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "larkax: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := observe.NewLogger(cfg.LogLevel, "", cfg.IsProduction())
	// 设为包级默认，因为 observe.FromContext 取的就是它——请求链路里
	// 每一处日志都走这条路。不设的话它们会落到 slog 自己的默认配置上，
	// 生产环境里表现为日志格式与级别都不是这里配的那一套。
	observe.AdoptAsDefault(logger)
	// 启动摘要直接打 Config 本身，由它的 LogValue 负责脱敏：DSN 里的口令抹掉、
	// 主密钥只留把数。摘要里是运维真会问的那几项（环境、角色、两个监听地址、
	// 有没有配 Redis），而不是把每个字段都塞进来。
	//
	// 这也是这条脱敏链的**触发点**：在此之前没有任何一处打过 Config，
	// 于是「打出来是脱敏的」只是一个没有调用方走过的性质，而不是被守住的。
	logger.Info("启动",
		"version", version,
		"config", cfg,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 数据库与配置在启动时一次性装好，任一步失败就不启动。
	// 带着半截依赖跑起来，故障会推迟到第一个真实请求才出现。
	handle, err := db.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	// 进程退出时的关闭错误无处可报：此刻已经在返回的路上，日志器可能已经
	// 随之结束，而连接池的关闭失败不影响进程已经做完的事。
	defer func() { _ = handle.Close() }()

	// settings 必须在服务对外之前读出来且校验通过：配置与代码声明的清单不一致
	// 属于部署错误，应该在启动时就暴露，而不是让某个请求读到不存在的配置项。
	values, err := settings.Load(ctx, settings.NewPgSource(handle))
	if err != nil {
		return err
	}
	logger.Info("配置已加载", "keys", strings.Join(values.Keys(), ","))

	// 内部端点（健康检查、指标、限流用量的只读查询）与控制台分端口暴露：
	// 它们面向编排系统与抓取器，不应与业务入口共用同一个监听地址。
	//
	// 它在公共入口装好之后才建，因为其中那条查询要读的是限流器的状态，
	// 而限流器由装配层建（见 newInternalHandler 的第二个参数）。
	app, err := publicHandler(ctx, cfg, handle, values, logger)
	if err != nil {
		return err
	}
	// 共享计数的连接池在进程退出时释放。这里没有在途的写要等，关一次就够；
	// 位置与数据库连接池那一处相同，都在 run 的收尾上。
	defer func() { _ = app.Close() }()

	publicSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.Public,
		ReadHeaderTimeout: 10 * time.Second,
	}

	internalSrv := &http.Server{
		Addr: cfg.InternalAddr,
		Handler: newInternalHandler([]observe.ReadinessCheck{
			{
				Name: "postgres",
				Check: func(ctx context.Context) error {
					return handle.PingContext(ctx)
				},
			},
		}, app),
		ReadHeaderTimeout: 5 * time.Second,
	}

	servers := []namedServer{
		{name: "internal", srv: internalSrv},
		{name: "public", srv: publicSrv},
	}

	return serveUntilSignal(ctx, logger, shutdownTimeout, servers)
}

// newInternalHandler 装配内部监听端口的 handler：健康检查、指标，以及那条
// 只读的限流用量查询。
//
// 抽成函数是为了让测试拿到**生产用的那一份**装配。写一份只给测试用的
// 装配，测到的就只是那份替身，而「指标端点到底挂在哪个监听上」正是最该
// 被钉住的性质之一——挂错了不会 404（公共端口的路径是全捕获的），
// 只会安静地从另一个端口回一份 501。限流用量那条同理，而且它更敏感：
// 挂到公共端口上等于把内部状态交给每一个能发推理请求的人。
//
// app 可以为 nil（测试、以及 worker 角色）：那时限流那条路径不注册。
func newInternalHandler(checks []observe.ReadinessCheck, app *assembly.App) http.Handler {
	mux := http.NewServeMux()
	observe.RegisterHealthHandlers(mux, checks)
	observe.RegisterMetricsHandler(mux)
	if app != nil {
		app.RegisterInternalOps(mux)
	}
	return mux
}

// publicHandler 装配公共监听端口的 handler。
//
// 装配本身在 internal/assembly 里：端到端测试要跑**同一条**装配路径，
// 而写在 package main 里的装配只有 cmd/larkax 自己的测试够得着，那要求
// 连库、监听端口——都不是命令行的职责。这里只负责把环境里的东西读出来
// 交给它。
//
// 它返回整个 App 而不是只返回那个 handler：内部端口上有一条只读查询要读
// 限流器的状态（见 newInternalHandler），而限流器装在 App 里。worker 角色
// 只带回一个占位 handler，其余字段都是零值——它没有限流器，那条查询在它
// 身上也不该存在。
//
// 注意它捕获一切路径，包括 /healthz。健康检查要配到 internal 端口；
// 配到这里的实例会拿到 501 并被编排系统摘除。上了负载均衡后这是真实的
// 误配陷阱，所以健康检查只注册在 internalMux 上。
//
// 它在返回之前把词表预热掉（见 warmVocab）：调用方拿到 handler 之后才去
// 监听端口，所以这个位置就等于「端口可用之前」。预热放在这里而不是
// run 里，是因为 worker 角色在上面就返回了——它不接推理流量，也不该付
// 这份启动成本。
func publicHandler(ctx context.Context, cfg *config.Config, handle *sql.DB, values *settings.Values, logger *slog.Logger) (*assembly.App, error) {
	// 公共端口的形状由**角色**决定，而不是从「有没有主密钥」反推角色。
	// 两者今天恰好等价，但把判据写成角色本身，才不会在改动 NeedsMasterKey
	// 或增删角色时静默地换掉某个角色的行为。
	if cfg.Role == config.RoleWorker {
		// worker 解不开任何渠道凭证，因而装不出候选来源。它本来就不承接推理
		// 流量，所以公共端口保持占位响应——回一个明确的 501 比让整个进程起不来
		// 更贴合这个角色，也让部署验证仍然能确认「端口通、TLS 通、负载均衡转发通」。
		//
		// 也没有限流器：它不接推理流量，就没有请求要按 key 计数。这同时意味着
		// 这个实例上没有那条只读查询（见 newInternalHandler）。
		return &assembly.App{Public: workerPlaceholder()}, nil
	}

	if len(cfg.MasterKeys) == 0 {
		// 角色本该接推理流量，却一把主密钥都没有：这是部署错误，不能退化成
		// 占位响应——那样一个接不到任何请求的实例在编排系统看来是「起来了」，
		// 要等到第一波真实流量才暴露，而现场只是一个与「未实现」无从区分的 501。
		// config.Load 已经拦下这一种；这里是同一判据的第二道防线，覆盖 Config
		// 不是经由环境变量构造出来的路径。
		return nil, fmt.Errorf("角色 %q 需要主密钥才能解开渠道凭证，但一把都没配置", cfg.Role)
	}

	sealer, err := credential.NewSealer(cfg.MasterKeys)
	if err != nil {
		return nil, fmt.Errorf("构造凭证封装器失败: %w", err)
	}
	// 只记标识不记材料：标识是「现在写进去的密文挂在哪把钥匙下」，
	// 轮换时靠它对账；而密钥本身进日志就是泄露。
	logger.Info("主密钥已加载", "ids", strings.Join(sealer.KeyIDs(), ","))

	executor, err := upstream.New(upstreamTimeout)
	if err != nil {
		return nil, fmt.Errorf("构造上游执行器失败: %w", err)
	}
	// 「注册了哪些适配器」要写进启动日志：「一个都没注册上」与「注册上了」
	// 在日志里长得一样，而前者的现场是每个请求都报未知渠道类型。
	logger.Info("上游适配器已注册", "types", strings.Join(executor.Types(), ","))

	//nolint:contextcheck // 这个 ctx 是给下面 warmVocab 的：Build 有意不收 context（见 assembly 里 pluginLoadTimeout 的注释）
	app, err := assembly.Build(assembly.Config{
		DB:       handle,
		Sealer:   sealer,
		Settings: values,
		Logger:   logger,
		Version:  version,
		Executor: executor,
		Rules:    pricing.New(handle),
		// 共享计数（限流）用的 Redis。留空表示这个部署不启用它，那时限流
		// 只在单实例下正确——装配层会打一条 warn 把这件事说出来。
		RedisDSN: cfg.RedisDSN,
	})
	if err != nil {
		return nil, err
	}
	// 计数器由装配层建好（它没有第二种实现），这里只是拿到它并预热。
	warmVocab(ctx, app.Gateway.Counter, logger)
	logger.Info("公共入口已装配", "snapshots", app.Snapshots.Collecting())
	return app, nil
}

// warmVocab 预热分词词表，把词表加载的成本从第一个请求挪到启动期。
//
// # 为什么放在监听端口之前
//
// 词表是惰性加载的（见 tokens.NewCounter）：不预热时，第一次走到精确计数
// 那条路要多花约 25ms，而那一次恰好是某个用户第一次调用某个模型——落在
// 最不该慢的那一次上。两种放法都有代价：放前面，端口晚约 43ms 可用；
// 放后面（起个 goroutine），则「预热还没完就有请求进来」时那个请求会在
// tokens 的互斥锁上等到加载结束，成本原样推回给用户，只是窗口小了几十
// 毫秒。只有放前面能保证「没有任何请求为词表加载付过时间」，所以选前面。
//
// # 为什么失败只记日志
//
// 不预热只是慢，不是不能用：词表会惰性加载，计数结果一样，只是那一次请求
// 多花约 25ms。把它升级成启动失败，等于用「实例起不来」去换「第一个请求
// 慢一点」——代价的方向反了。所以这里只记一条日志，让「这台机器上预热没
// 成功」在启动日志里留下痕迹，而不是安静地退化。
//
// # 为什么不写进 assembly.Build
//
// Build 的契约是「要么能用，要么启动就失败」（见它的包注释），而这一步
// 恰恰**不能**失败；Build 也没有 context（理由见它里面 pluginLoadTimeout
// 的注释），而 Prepare 需要一个放弃点（见它的注释）。启动期的取舍与日志
// 本来就属于 cmd/larkax：放在这里，与「开始监听」在同一条时间线上，
// 顺序一眼可见。
func warmVocab(ctx context.Context, counter *tokens.Counter, logger *slog.Logger) {
	durations, err := counter.Prepare(ctx)
	if err != nil {
		logger.Warn("词表预热失败，退到首次使用时惰性加载", "err", err)
		return
	}
	// 逐种编码的耗时，排序后打：日志里 map 的顺序不固定，同一个部署两次
	// 启动打出来的两行会不一样，而这类日志正是拿来前后对比的。
	parts := make([]string, 0, len(durations))
	for enc, d := range durations {
		parts = append(parts, fmt.Sprintf("%s=%s", enc, d.Round(time.Millisecond)))
	}
	sort.Strings(parts)
	// 两个字段是同一个结论的两次独立观测：durations 是 Prepare 自己记的
	// 加载耗时，encodings 是词表集合的实际内容。只打「预热完成」的话，
	// 「一种都没解开」与「都解开了」在日志里长得一样；而两者若哪天真对不上
	// （Prepare 说加载了、集合里没有），这一行会把它摆出来。
	logger.Info("词表已预热",
		"encodings", strings.Join(counter.Loaded(), ","),
		"durations", strings.Join(parts, " "),
	)
}

// workerPlaceholderMessage 是 worker 角色的公共端口占位响应文案。
//
// 这一层只有文案：状态码与错误体的形状都由 internal/api 给出（见
// api.WriteNotImplemented）。在这里手写一份完整字面量的话，它与 api 那一侧
// 会各改各的——而两份看起来都对，除非有人把两处的字节对一遍。
const workerPlaceholderMessage = "this instance runs the worker role and does not serve inference traffic"

// workerPlaceholder 是 worker 角色的公共端口占位响应。
//
// 它按路径全捕获，任何真实流量都会立刻得到「这个实例不接推理流量」，
// 而不是超时或 404 误导——部署验证靠这个性质确认链路是通的。
//
// 措辞点名角色，而不是写「本构建没有推理接口」：501 也可能来自别的原因，
// 而两种情况的排查方向完全不同（一个是部署把 worker 暴露在了公共端口上，
// 另一个是功能还没接上）。让响应自己说清是哪一种，看到它的人不必先猜。
func workerPlaceholder() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(api.HeaderVersion, version)
		api.WriteNotImplemented(w, workerPlaceholderMessage)
	})
	// 最外层是关联标识：放在其他中间件之前，
	// 这样被后续中间件拒绝掉的请求也带着 id，能被排查到。
	return observe.Middleware(mux)
}

type namedServer struct {
	name string
	srv  *http.Server
}

// serveUntilSignal 启动全部监听器并在收到信号时优雅排空。
//
// 排空的超时不是形式主义：流式响应可能持续数分钟，
// 到点强行关闭会让正在生成的长回复断在半截。超时值的取舍是
// 「不能让旧实例永远不退」与「不能截断进行中的流」之间的折中。
//
// # 启动期：先绑上，说清了，再服务
//
// 绑定从各个监听器自己的 goroutine 里提到了这里，**顺序做完、一次收齐**，
// 然后才开始服务。换来的是两件事：
//
//   - 「开始监听」只在**所有**地址都绑上之后才打，于是它出现就是「这些端口
//     都在听」的正向证据。以前它打的是「我打算绑这个地址」——端口被占时
//     照样出现，排查「服务到底有没有起来」时它是**反向**的证据。
//   - 两个地址都起不来时（例如两个端口都被占），返回的错误里两条原因都在。
//     以前每个 goroutine 各绑各的、收到第一条就返回，第二条留在没人读的
//     channel 里，于是「另一个为什么也没起来」要再改一次端口、再跑一次才知道。
//
// 已知取舍：**有一个地址绑不上就整体退出**，已经绑上的那些直接关闭，不做排空
// （它们还没开始 Serve，也没有在途请求可排）。理由是「监听器起不来」属于启动期
// 错误，此刻还没有真实流量值得保护——启动失败就快速失败；而让一半端口在听的
// 进程更糟：它会按另一半的结果被编排系统收流量。
//
// 服务起来之后，Serve 出错仍然是「收到第一条就返回」：进程随即退出，其余
// 监听器跟着一起消失，它们的错误没有第二个读者。这一点与启动期不同——运行时
// 没有「等一会儿就能收齐」的窗口。
func serveUntilSignal(ctx context.Context, logger *slog.Logger, shutdownTimeout time.Duration, servers []namedServer) error {
	// 绑不上的先记下来、继续绑下一个，而不是就地返回：「另一个为什么也没起来」
	// 正是这条路径要一次答完的问题。
	listeners := make([]net.Listener, 0, len(servers))
	failures := make([]error, 0, len(servers))
	// 走 ListenConfig 而不是 net.Listen（noctx 要求带 context）。这里的 ctx
	// 只用于解析地址，**不影响返回的监听器**（见它的文档），所以收到退出信号
	// 不会顺手把已经绑好的端口关掉——那是下面 Shutdown 的活。
	var lc net.ListenConfig

	for _, ns := range servers {
		addr := ns.srv.Addr
		if addr == "" {
			// 与 http.Server.ListenAndServe 的默认值一致。自己绑就得自己认这条
			// 默认值，否则空地址会静默地从一个固定端口变成一个随机端口。
			addr = ":http"
		}
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s 监听失败: %w", ns.name, err))
			continue
		}
		listeners = append(listeners, ln)
	}
	if len(failures) > 0 {
		// 已经绑上的一个都不留：留着就是一个「半死」的进程——一个端口在听、
		// 另一个没起来，而进程马上就要退出。
		for _, ln := range listeners {
			_ = ln.Close()
		}
		// 拼成单行而不是 errors.Join：后者的错误文本是换行拼起来的，而这条要进
		// stderr（见 main），单行更好读——与 ledger/store、catalog 里那两处同。
		msgs := make([]string, 0, len(failures))
		for _, err := range failures {
			msgs = append(msgs, err.Error())
		}
		return fmt.Errorf("%d 个监听器起不来: %s", len(failures), strings.Join(msgs, "; "))
	}

	errCh := make(chan error, len(servers))

	// 走到这里说明一个都没绑失败，listeners 与 servers 一一对应。
	for i, ns := range servers {
		ln := listeners[i]
		// 报的是**实际绑上**的地址，而不是配置里的那一个：这一行要当证据用，
		// 就不能是「我打算绑哪儿」。两者在正常配置下相同，只有 :0 那种写法不同。
		logger.Info("开始监听", "server", ns.name, "addr", ln.Addr())
		go func(ns namedServer, ln net.Listener) {
			// Serve 拿的是上面那个已经绑好的监听器，而不是让 Server 自己去绑：
			// 绑定的结果与「开始监听」这行日志必须是同一个动作的产物。
			if err := ns.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s 服务中断: %w", ns.name, err)
			}
		}(ns, ln)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，开始排空")
	}

	// 排空用的 context 有意从 Background 起，而不是从上面那个 ctx：
	// ctx 在收到退出信号时已经 Done 了，从它派生出来的 context 一出生就是
	// 取消状态，Shutdown 会立刻返回，在途请求被直接切断——
	// 而排空要做的恰恰是「等它们结束」。这里要的是「独立于退出信号，
	// 但有自己的超时上限」的一条时间线，所以新起一个。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var failed bool
	for _, ns := range servers {
		//nolint:contextcheck // shutdownCtx 的来由见上，它不该继承退出信号
		if err := ns.srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("排空失败", "server", ns.name, "err", err)
			failed = true
		}
	}
	if failed {
		return errors.New("部分监听器未能正常排空")
	}
	logger.Info("已退出")
	return nil
}
