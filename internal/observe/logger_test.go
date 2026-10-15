package observe

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestContextLogger(t *testing.T) {
	if FromContext(context.Background()) == nil {
		t.Fatal("无日志器时应返回默认日志器而非 nil")
	}

	var sb strings.Builder
	custom := NewLoggerWithWriter(&sb, "debug", "text", false)
	ctx := WithLogger(context.Background(), custom)
	if FromContext(ctx) != custom {
		t.Error("应取回放入的日志器")
	}
}

// nil 日志器放进 context 不应导致取出时得到 nil。
func TestContextLoggerRejectsNil(t *testing.T) {
	ctx := WithLogger(context.Background(), nil)
	if FromContext(ctx) == nil {
		t.Error("放入 nil 后取出不应为 nil")
	}
}

func TestHealthzAlwaysOK(t *testing.T) {
	mux := http.NewServeMux()
	RegisterHealthHandlers(mux, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
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
	cases := []struct {
		name       string
		checks     []ReadinessCheck
		wantCode   int
		wantReady  bool
		wantDetail string
	}{
		{
			name:       "全部通过",
			checks:     []ReadinessCheck{{Name: "db", Check: func(context.Context) error { return nil }}},
			wantCode:   http.StatusOK,
			wantReady:  true,
			wantDetail: "ok",
		},
		{
			name: "其中一个失败",
			checks: []ReadinessCheck{
				{Name: "db", Check: func(context.Context) error { return nil }},
				{Name: "redis", Check: func(context.Context) error { return errProbe }},
			},
			wantCode:   http.StatusServiceUnavailable,
			wantReady:  false,
			wantDetail: "probe failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			RegisterHealthHandlers(mux, tc.checks)

			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
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
			for _, c := range tc.checks {
				if c.Name == "redis" && body.Checks["redis"] != tc.wantDetail {
					t.Errorf("checks.redis = %q，期望 %q", body.Checks["redis"], tc.wantDetail)
				}
			}
		})
	}
}

var errProbe = probeError("probe failed")

type probeError string

func (e probeError) Error() string { return string(e) }
