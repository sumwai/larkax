// Command larkax 是 larkax 的主服务进程。
//
// 同一个二进制以 --role 区分数据面与控制面：
//   - gateway：承接用户推理请求。它的可用性直接等于 larkax 的可用性。
//   - control：控制台、账户、计费规则等管理面。它挂掉不影响推理。
//   - worker：后台任务（日历同步、额度探测、对账）。
//
// 最高原则是数据面不依赖控制面在线，因此三者共用一套依赖装配代码，
// 但启动的组件按角色裁剪，而不是让 gateway 顺带把控制面的依赖也拉起来。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/db"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/settings"
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
	// 同时设为包级默认，让 observe.FromContext 的兜底与本进程真正在用的
	// 日志器一致。不设的话，漏了 WithLogger 的调用点会拿到另一套配置，
	// 生产环境里表现为部分日志是文本、部分是 JSON。
	observe.AdoptAsDefault(logger)
	logger.Info("启动",
		"version", version,
		"env", cfg.Env,
		"role", string(cfg.Role),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 数据库与配置在启动时一次性装好，任一步失败就不启动。
	// 带着半截依赖跑起来，故障会推迟到第一个真实请求才出现。
	handle, err := db.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer handle.Close()

	// settings 必须在服务对外之前读出来且校验通过：配置与代码声明的清单不一致
	// 属于部署错误，应该在启动时就暴露，而不是让某个请求读到不存在的配置项。
	values, err := settings.Load(ctx, settings.NewPgSource(handle))
	if err != nil {
		return err
	}
	logger.Info("配置已加载", "keys", strings.Join(values.Keys(), ","))

	// 内部端点（健康检查与指标）与控制台分端口暴露：
	// 它们面向编排系统与抓取器，不应与业务入口共用同一个监听地址。
	internalMux := http.NewServeMux()
	observe.RegisterHealthHandlers(internalMux, []observe.ReadinessCheck{
		{
			Name: "postgres",
			Check: func(ctx context.Context) error {
				return handle.PingContext(ctx)
			},
		},
	})

	internalSrv := &http.Server{
		Addr:              cfg.InternalAddr,
		Handler:           internalMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	servers := []namedServer{{name: "internal", srv: internalSrv}}

	// 还没有业务路由，公共监听端口先只回一个明确的占位响应。
	// 这样部署验证能确认端口通、TLS 通、负载均衡转发通，
	// 同时任何真实流量都会立刻得到「尚未实现」而不是超时或 404 误导。
	//
	// 注意 "/" 捕获一切路径，包括 /healthz。健康检查要配到 internal 端口；
	// 配到这里的实例会拿到 501 并被编排系统摘除。上了负载均衡后这是真实的
	// 误配陷阱，所以健康检查只注册在 internalMux 上。
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("x-larkax-version", version)
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":{"type":"not_implemented","message":"inference API is not available in this build"}}`))
	})
	publicSrv := &http.Server{
		Addr: cfg.HTTPAddr,
		// 最外层是关联标识：放在其他中间件之前，
		// 这样被后续中间件拒绝掉的请求也带着 id，能被排查到。
		Handler:           observe.Middleware(publicMux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	servers = append(servers, namedServer{name: "public", srv: publicSrv})

	return serveUntilSignal(ctx, logger, shutdownTimeout, servers)
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
// 已知取舍：某个监听器起不来时直接返回错误，不会去排空其余监听器。
// 此时进程随即退出，已建立的连接是被切断而非排空的。理由是「监听器起不来」
// 属于启动期错误，此刻还没有真实流量值得保护——启动失败就快速失败。
func serveUntilSignal(ctx context.Context, logger *slog.Logger, shutdownTimeout time.Duration, servers []namedServer) error {
	errCh := make(chan error, len(servers))

	for _, ns := range servers {
		go func(ns namedServer) {
			logger.Info("开始监听", "server", ns.name, "addr", ns.srv.Addr)
			if err := ns.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s 监听失败: %w", ns.name, err)
			}
		}(ns)
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，开始排空")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var failed bool
	for _, ns := range servers {
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
