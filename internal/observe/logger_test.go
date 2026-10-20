package observe

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"DEBUG":    slog.LevelDebug,
		" Debug ":  slog.LevelDebug,
		"info":     slog.LevelInfo,
		"warn":     slog.LevelWarn,
		"warning":  slog.LevelWarn,
		"error":    slog.LevelError,
		"":         slog.LevelInfo,
		"nonsense": slog.LevelInfo,
		"VERBOSE":  slog.LevelInfo,
		"trace":    slog.LevelInfo,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestLoggerFormatSelection(t *testing.T) {
	cases := []struct {
		name         string
		format       string
		isProduction bool
		wantJSON     bool
	}{
		{"生产环境默认 JSON", "", true, true},
		{"开发环境默认文本", "", false, false},
		{"显式 JSON 覆盖开发默认", "json", false, true},
		{"显式文本覆盖生产默认", "text", true, false},
		{"大小写不敏感", "JSON", false, true},
		{"两侧空白可容忍", " json ", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := logOne(t, "info", tc.format, tc.isProduction)
			gotJSON := strings.HasPrefix(strings.TrimSpace(out), "{")
			if gotJSON != tc.wantJSON {
				t.Errorf("format=%q production=%v 输出 %q，JSON=%v，期望 %v",
					tc.format, tc.isProduction, out, gotJSON, tc.wantJSON)
			}
		})
	}
}

// logOne 用给定参数记录一条日志并返回渲染结果。
func logOne(t *testing.T, level, format string, isProduction bool) string {
	t.Helper()
	var sb strings.Builder
	l := NewLoggerWithWriter(&sb, level, format, isProduction)
	l.Info("probe", "key", "value")
	return sb.String()
}

func TestLoggerLevelFilters(t *testing.T) {
	var sb strings.Builder
	l := NewLoggerWithWriter(&sb, "warn", "json", true)

	if l.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("warn 级别下 info 应被过滤")
	}
	if !l.Enabled(context.Background(), slog.LevelError) {
		t.Error("warn 级别下 error 应放行")
	}

	l.Info("should not appear")
	if sb.Len() != 0 {
		t.Errorf("被过滤的日志不应有输出，实际为 %q", sb.String())
	}
}

func TestLoggerJSONShape(t *testing.T) {
	out := logOne(t, "info", "json", true)

	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	if rec["msg"] != "probe" {
		t.Errorf("msg = %v，期望 probe", rec["msg"])
	}
	if rec["key"] != "value" {
		t.Errorf("附加字段丢失: %v", rec)
	}
}

// FromContext 的日志器来自 slog 的包级默认，由进程启动时的 AdoptAsDefault
// 装好——没有任何一处会把日志器放进 context，所以这条路必须自己站得住。
func TestContextLoggerComesFromDefault(t *testing.T) {
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })

	var sb strings.Builder
	custom := NewLoggerWithWriter(&sb, "debug", "text", false)
	AdoptAsDefault(custom)

	if got := FromContext(context.Background()); got != custom {
		t.Error("应取回 AdoptAsDefault 装的那个日志器")
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHealthHandlers(mux, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("/healthz 返回 %d，期望 200", rec.Code)
	}
}

// 没有需要检查的依赖就是已就绪。若返回 503，尚未注册探针的实例
// 会被编排系统永久摘除，现场表现是「起来了但不接流量」。
func TestReadyzOKWithoutChecks(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHealthHandlers(mux, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("无依赖检查时 /readyz 返回 %d，期望 200", rec.Code)
	}

	var body struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Ready {
		t.Error("无依赖检查时 ready 应为 true")
	}
}

func TestReadyzReflectsCheckResults(t *testing.T) {
	// wantChecks 是 checks 这个映射**完整**的期望内容，按检查项的名字索引。
	//
	// 写成整张表而不是「某一项的明细」：按名字挑一项来断言的话，那个名字
	// 必须是这个 case 里真实存在的检查项，否则断言一次都不会执行——而
	// 一次都没执行的断言与「通过了」在输出里长得一样（见 LESSONS 第 16 条）。
	// 整张表还顺带钉住了成功分支写进去的那个字面量。
	cases := []struct {
		name       string
		checks     []ReadinessCheck
		wantCode   int
		wantReady  bool
		wantChecks map[string]string
	}{
		{
			name:       "全部通过",
			checks:     []ReadinessCheck{{Name: "db", Check: func(context.Context) error { return nil }}},
			wantCode:   http.StatusOK,
			wantReady:  true,
			wantChecks: map[string]string{"db": "ok"},
		},
		{
			name: "其中一个失败",
			checks: []ReadinessCheck{
				{Name: "db", Check: func(context.Context) error { return nil }},
				{Name: "redis", Check: func(context.Context) error { return errProbe }},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantReady:  false,
			wantChecks: map[string]string{"db": "ok", "redis": "probe failed"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			RegisterHealthHandlers(mux, tc.checks)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Errorf("返回 %d，期望 %d", rec.Code, tc.wantCode)
			}
			var body struct {
				Ready  bool              `json:"ready"`
				Checks map[string]string `json:"checks"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是合法 JSON: %v", err)
			}
			if body.Ready != tc.wantReady {
				t.Errorf("ready = %v，期望 %v", body.Ready, tc.wantReady)
			}
			for name, want := range tc.wantChecks {
				if got := body.Checks[name]; got != want {
					t.Errorf("checks.%s = %q，期望 %q", name, got, want)
				}
			}
			if len(body.Checks) != len(tc.wantChecks) {
				t.Errorf("checks 有 %d 项，期望 %d 项：%v", len(body.Checks), len(tc.wantChecks), body.Checks)
			}
		})
	}
}

// 就绪检查必须拿到一个**尚未到期**的截止时间：依赖探针靠它决定还要不要
// 继续等（见 internal/tokens 的 VocabReady，它按 ctx 放弃已经超时的词表加载）。
// 超时被写成 0 时（readyzTimeout 的 `*` 换成 `/` 就是这个结果），检查项一进来
// 就看见 ctx 已取消，会把「有依赖可用」误报成失败。
//
// 断言里用字面量 2 秒而不是 readyzTimeout：拿常量当判据的话，常量被一起
// 改掉时用例照样通过。
func TestReadyzPassesUnexpiredDeadlineToChecks(t *testing.T) {
	const wantTimeout = 2 * time.Second

	var (
		hasDeadline bool
		remaining   time.Duration
		ctxErr      error
	)
	mux := http.NewServeMux()
	RegisterHealthHandlers(mux, []ReadinessCheck{{
		Name: "probe",
		Check: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			hasDeadline = ok
			if !ok {
				return errors.New("context 没有截止时间")
			}
			remaining = time.Until(deadline)
			ctxErr = ctx.Err()
			return ctxErr
		},
	}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if !hasDeadline {
		t.Fatal("检查项拿到的 context 没有截止时间")
	}
	if ctxErr != nil {
		t.Fatalf("检查项开始时 context 已结束：%v（剩余 %v）", ctxErr, remaining)
	}
	if remaining <= 0 || remaining > wantTimeout {
		t.Errorf("剩余超时 = %v，期望落在 (0, %v] 之内", remaining, wantTimeout)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("/readyz 返回 %d，期望 200：%s", rec.Code, rec.Body.String())
	}
}

var errProbe = probeError("probe failed")

type probeError string

func (e probeError) Error() string { return string(e) }
