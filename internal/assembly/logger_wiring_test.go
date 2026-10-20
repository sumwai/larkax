package assembly

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/settings"
)

// Config.Logger 为 nil 时必须回落到 slog.Default()，不是把 nil 传下去。
//
// 这一条看的是 Build 开头那次回落。漏掉它的后果不会当场暴露：装配本身
// 不去调用 logger 也能走完大半，返回的对象照样能用。真正出事的是之后
// 每一条日志——nil 的 *slog.Logger 一被调用就 panic，于是第一个写日志
// 的地方成了崩溃点，现场看起来像那一条日志自己的问题。
//
// 观测量选的是**启动日志落在了哪个 handler 里**：把默认 logger 换成
// 一个记账的 handler，装配照常完成、那条启动日志出现在它里面，同时
// 说明了两件事——回落发生了，且用的是 slog.Default()。日志按字面量
// 断言，理由与别处一样：拿被测代码里的常量来出题，改掉它用例照样通过。
//
// 「配了 Logger 就只用它」那一半也在：少了它，一个永远用 slog.Default()、
// 根本不看 cfg.Logger 的实现也能让第一条通过。两条一起才说明回落只在未
// 配置时发生。
func TestBuildLoggerFallback(t *testing.T) {
	db := testDB(t)

	values, err := settings.Load(context.Background(), settings.NewPgSource(db))
	if err != nil {
		t.Fatalf("加载运行期配置失败: %v", err)
	}

	var defaultLogs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&defaultLogs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// buildRateLimiter 在每一条分支上都会写一行启动日志（见那里「三种形态，
	// 三条不同的日志」）。`testConfig` 不设 `Config.RedisDSN`，落的是进程内
	// 计数那一条。选它是为了**不依赖被测行为**：它证明的是「这次装配确实写过
	// 日志」，而不是哪一条日志必须写。
	const startupLine = "RPM 限流的计数在进程内"

	t.Run("未配置时回落到默认 logger", func(t *testing.T) {
		defaultLogs.Reset()

		cfg := testConfig(t, db, values)
		cfg.Logger = nil
		buildConfig(t, cfg)

		if !strings.Contains(defaultLogs.String(), startupLine) {
			t.Errorf("默认 logger 里没有那条启动日志：%q", defaultLogs.String())
		}
	})

	t.Run("配了 Logger 就只用它", func(t *testing.T) {
		defaultLogs.Reset()
		var ownLogs bytes.Buffer

		cfg := testConfig(t, db, values)
		cfg.Logger = slog.New(slog.NewTextHandler(&ownLogs, nil))
		buildConfig(t, cfg)

		if !strings.Contains(ownLogs.String(), startupLine) {
			t.Errorf("配下的 Logger 里没有那条启动日志：%q", ownLogs.String())
		}
		if strings.Contains(defaultLogs.String(), startupLine) {
			t.Errorf("显式配了 Logger，启动日志却进了默认 logger：%q", defaultLogs.String())
		}
	})
}

// buildConfig 装配一次，panic 也折成一次普通失败。
//
// 一个 nil 的 *slog.Logger 第一次被调用就会 panic，所以「Logger 为 nil 时
// 没回落到默认值」这一类漏法会以 panic 的形式出现。直接让它冲出去的话，
// 栈停在某个调用 slog 的深处，看上去像那里自己的问题；在这里接住，按一条
// 普通的失败报出来。
func buildConfig(t *testing.T, cfg Config) {
	t.Helper()

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("装配 panic 了：%v", r)
			}
		}()
		_, err = Build(cfg)
	}()
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
}
