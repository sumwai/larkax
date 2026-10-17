package assembly

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/observe"
)

// captureLogs 把装配期的日志收下来，供用例断言。
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// restoreExactSignal 把包级注册表里那个信号改回去。
//
// 装配层用的是包级注册表（限流器没有别的构造点），而往它上面写会让同一次
// go test 里别的断言看到不该有的初值。
func restoreExactSignal(t *testing.T) {
	t.Helper()
	before := observe.Metrics.RateLimitExact.Value()
	t.Cleanup(func() { observe.Metrics.RateLimitExact.Set(before) })
}

// TestBuildRateLimiterWithoutRedisWarns 钉住「没配 Redis」那一档。
//
// 它必须说出来，而且是 warn：这一档在单实例下完全正确，所以默认沉默看起来
// 无懈可击——而它在多实例下就是额度放大到实例数倍，且没有一处能看出来
// （响应码、配置、请求日志全都一样）。
func TestBuildRateLimiterWithoutRedisWarns(t *testing.T) {
	restoreExactSignal(t)

	logger, logs := captureLogs()
	limiter, err := buildRateLimiter(Config{}, logger)
	if err != nil {
		t.Fatalf("没有 Redis 不该装配失败: %v", err)
	}
	if limiter == nil {
		t.Fatal("没有 Redis 时也必须有限流器：退回进程内计数，而不是不做限流")
	}
	if got := logs.String(); !strings.Contains(got, "进程内") || !strings.Contains(got, "LARKAX_REDIS_DSN") {
		t.Errorf("启动日志没说清计数在哪: %s", got)
	}
	// 信号从一开始就要落在「近似」上，而不是等到第一个请求才出现：
	// 「还没来过请求」与「这个实例上没接限流器」在 /metrics 上都没有这一行。
	if v := observe.Metrics.RateLimitExact.Value(); v != 0 {
		t.Errorf("没有共享存储时 larkax_ratelimit_exact = %d，期望 0（近似）", v)
	}
}

// TestBuildRateLimiterRejectsUnparsableDSN 钉住两种「Redis 有问题」的分界。
//
// 解析不了的 DSN 是**配置错误**，拒绝启动：那种值重试多少次都不会变好，
// 而带着它跑起来的表现是「每个请求都降级」——日志里只有一行 warn，看起来
// 与「Redis 临时挂了」一模一样。
func TestBuildRateLimiterRejectsUnparsableDSN(t *testing.T) {
	logger, _ := captureLogs()
	// scheme 在 internal/config 那一层已经挡过一遍（只放 redis/rediss/unix），
	// 这里造一个过得了那一关、但客户端解析不了的：unix socket 的路径为空。
	if _, err := buildRateLimiter(Config{RedisDSN: "unix://"}, logger); err == nil {
		t.Fatal("解析不了的 DSN 应当让装配失败，而不是降级")
	}
}

// TestBuildRateLimiterDegradesWhenRedisIsDown 钉住「配了但连不上」那一档。
//
// 与上一档相对：这是**故障**，不能拒绝启动。拒绝启动换来的是整个数据面
// 不可用，而这一类要保的恰恰是「共享存储挂了不能影响正常用户」。
//
// 指向一个没人监听的端口：走的是真的客户端、真的连接失败。
func TestBuildRateLimiterDegradesWhenRedisIsDown(t *testing.T) {
	restoreExactSignal(t)

	logger, logs := captureLogs()
	limiter, err := buildRateLimiter(Config{
		RedisDSN: "redis://127.0.0.1:1/0",
		Now:      time.Now,
	}, logger)
	if err != nil {
		t.Fatalf("连不上 Redis 不该让启动失败: %v", err)
	}
	if limiter == nil {
		t.Fatal("降级也必须有限流器")
	}
	t.Cleanup(func() { _ = limiter.Close() })

	if got := logs.String(); !strings.Contains(got, "连不上") {
		t.Errorf("启动日志没说清共享计数当前不可用: %s", got)
	}
	if v := observe.Metrics.RateLimitExact.Value(); v != 0 {
		t.Errorf("连不上时 larkax_ratelimit_exact = %d，期望 0", v)
	}
}

// TestBuildRateLimiterWithRedisIsExact 钉住连得上那一档。
//
// 需要真的 Redis（LARKAX_REDIS_DSN），没有时跳过——与仓库里连库那些用例
// 同一个做法。判据是「探活之后那个信号是精确」，而不只是「没报错」：
// 一个探活没接上指标的实现同样是「没报错」。
func TestBuildRateLimiterWithRedisIsExact(t *testing.T) {
	dsn := os.Getenv("LARKAX_REDIS_DSN")
	if dsn == "" {
		t.Skip("没有 LARKAX_REDIS_DSN，跳过需要真 Redis 的用例")
	}
	restoreExactSignal(t)

	logger, logs := captureLogs()
	limiter, err := buildRateLimiter(Config{RedisDSN: dsn, Now: time.Now}, logger)
	if err != nil {
		t.Fatalf("配了可用的 Redis 却装配失败: %v", err)
	}
	t.Cleanup(func() { _ = limiter.Close() })

	if got := logs.String(); !strings.Contains(got, "共享存储") {
		t.Errorf("启动日志没说清计数在共享存储里: %s", got)
	}
	if v := observe.Metrics.RateLimitExact.Value(); v != 1 {
		t.Errorf("探活之后 larkax_ratelimit_exact = %d，期望 1", v)
	}
}
