// Package observe 提供日志、贯穿请求全链路的关联标识（request ID）与
// 健康检查的基础设施。
//
// 这里刻意不引入第三方日志库：标准库的 log/slog 已经够用，
// 少一个依赖就少一处需要跟进的升级与安全公告。
package observe

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger 按级别与格式构造日志器，输出到标准输出。
func NewLogger(level, format string, isProduction bool) *slog.Logger {
	return NewLoggerWithWriter(os.Stdout, level, format, isProduction)
}

// NewLoggerWithWriter 与 NewLogger 相同，但输出到指定位置。
// 测试用它把日志写进缓冲区。
func NewLoggerWithWriter(w io.Writer, level, format string, isProduction bool) *slog.Logger {
	if format == "" {
		format = defaultFormat(isProduction)
	}
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	return slog.New(newHandler(w, format, opts))
}

// defaultFormat 按环境选择默认格式：生产用 JSON（便于采集与检索），
// 开发用文本（人读得顺眼）。
func defaultFormat(isProduction bool) string {
	if isProduction {
		return "json"
	}
	return "text"
}

func newHandler(w io.Writer, format string, opts *slog.HandlerOptions) slog.Handler {
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// ParseLevel 把文本级别映射为 slog 级别。
//
// 无法识别的值一律回落到 info 而不报错：日志级别写错不应该让服务起不来，
// 而 info 是既能看见异常、又不会淹没在调试输出里的安全选择。
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// FromContext 取回本进程的日志器，并把请求标识附成字段。
//
// 日志器来自 slog.Default()，由进程启动时的 AdoptAsDefault 装好。
// **没有「把日志器放进 context」这条路**：日志器是进程级的，逐请求要变的
// 只是请求标识，而它由 Middleware 装进 context。
//
// 若 context 里带着请求标识，会作为字段附在日志器上。这是有意的：
// 让调用点不必在每个 log 语句里手写 "request_id", id——
// 那种写法漏一处就断一条链路，而漏掉的地方往往正是出错的那一处。
func FromContext(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if id := RequestIDFromContext(ctx); id != "" {
		l = l.With("request_id", string(id))
	}
	return l
}

// AdoptAsDefault 把 logger 设为 slog 的包级默认。
//
// 只在进程启动时调用一次。**它是 FromContext 取到日志器的唯一来源**：
// 不设的话，请求链路里的每一处日志都会走 slog 自己的默认配置，
// 生产环境里表现为日志格式与级别都不是这里配的那一套。
func AdoptAsDefault(l *slog.Logger) { slog.SetDefault(l) }
