// Package observe 提供日志与健康检查的基础设施。
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

// contextKey 是本包在 context 中存放日志器的私有键类型。
// 用空结构体而非字符串，避免与其他包的键冲突。
type contextKey struct{}

// WithLogger 返回带日志器的 context。
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

// FromContext 取出 context 里的日志器；没有就返回默认的。
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
