package observe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRequestIDIsUniqueAndWellFormed(t *testing.T) {
	seen := make(map[RequestID]bool)
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if id == "" {
			t.Fatal("生成了空的请求标识")
		}
		if len(string(id)) != requestIDBytes*2 {
			t.Fatalf("标识 %q 长度为 %d，期望 %d", id, len(id), requestIDBytes*2)
		}
		if seen[id] {
			t.Fatalf("重复的标识 %q", id)
		}
		seen[id] = true
	}
}

func TestRequestIDFromHeader(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantIn  bool // 期望原样保留
		wantBad bool // 期望被替换成新生成的
	}{
		{name: "正常值原样保留", in: "abc-123_XYZ", wantIn: true},
		{name: "空值生成新的", in: "", wantBad: true},
		{name: "只有空白生成新的", in: "   ", wantBad: true},
		{name: "带换行被拒绝", in: "abc\ndef", wantBad: true},
		{name: "带制表符被拒绝", in: "abc\tdef", wantBad: true},
		{name: "带空格被拒绝", in: "abc def", wantBad: true},
		{name: "超长被拒绝", in: strings.Repeat("a", 129), wantBad: true},
		{name: "非 ASCII 被拒绝", in: "请求标识", wantBad: true},
		{name: "上限长度可接受", in: strings.Repeat("a", 128), wantIn: true},
		{name: "两侧空白被裁掉", in: "  abc123  ", wantIn: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RequestIDFromHeader(tc.in)
			trimmed := strings.TrimSpace(tc.in)
			switch {
			case tc.wantIn && string(got) != trimmed:
				t.Errorf("得到 %q，期望原样保留 %q", got, trimmed)
			case tc.wantBad:
				if string(got) == tc.in || got == "" {
					t.Errorf("得到 %q，期望替换为新生成的标识", got)
				}
				if len(string(got)) != requestIDBytes*2 {
					t.Errorf("替换后的标识 %q 不是新生成的样子", got)
				}
			}
		})
	}
}

// 带换行的标识若被原样写进日志，就能伪造出额外的日志行。
// 日志注入容易被忽略，因为日志被认为「只是输出」。
func TestRequestIDRejectsLogInjection(t *testing.T) {
	evil := "abc\nlevel=ERROR msg=伪造的告警"
	if got := RequestIDFromHeader(evil); string(got) == evil {
		t.Error("带换行的标识不应被接受")
	}
}

func TestRequestIDContextRoundTrip(t *testing.T) {
	if RequestIDFromContext(context.Background()) != "" {
		t.Error("无标识时应返回空，而不是凭空生成")
	}

	id := RequestID("abc123")
	ctx := WithRequestID(context.Background(), id)
	if got := RequestIDFromContext(ctx); got != id {
		t.Errorf("取回 %q，期望 %q", got, id)
	}
}

func TestMiddlewareSetsHeaderAndContext(t *testing.T) {
	var fromCtx RequestID
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromCtx = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set(HeaderRequestID, "client-supplied")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderRequestID); got != "client-supplied" {
		t.Errorf("响应头 %q，期望回显 client-supplied", got)
	}
	if fromCtx != "client-supplied" {
		t.Errorf("context 里是 %q，期望 client-supplied", fromCtx)
	}
}

// 客户端没带标识时也要有一个，否则那条日志无法与别的关联起来。
func TestMiddlewareGeneratesWhenAbsent(t *testing.T) {
	var fromCtx RequestID
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromCtx = RequestIDFromContext(r.Context())
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if fromCtx == "" {
		t.Error("context 里应有自动生成的标识")
	}
	if got := rec.Header().Get(HeaderRequestID); got != string(fromCtx) {
		t.Errorf("响应头 %q 与 context 里的 %q 应一致", got, fromCtx)
	}
}

// 带上请求标识的日志器应自动附上该字段，调用点不必手写。
func TestFromContextAttachesRequestID(t *testing.T) {
	var sb strings.Builder
	l := NewLoggerWithWriter(&sb, "info", "json", true)

	ctx := WithLoggerAndRequest(context.Background(), l, RequestID("trace-me"))
	FromContext(ctx).Info("probe", "key", "value")

	var rec map[string]any
	if err := json.Unmarshal([]byte(sb.String()), &rec); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, sb.String())
	}
	if rec["request_id"] != "trace-me" {
		t.Errorf("request_id = %v，期望 trace-me", rec["request_id"])
	}
	if rec["key"] != "value" {
		t.Errorf("原有字段丢失: %v", rec)
	}
}

// 没有标识时不应凭空多出一个空字段，那会让日志看起来像是漏了埋点。
func TestFromContextWithoutRequestIDAddsNothing(t *testing.T) {
	var sb strings.Builder
	l := NewLoggerWithWriter(&sb, "info", "json", true)

	ctx := WithLogger(context.Background(), l)
	FromContext(ctx).Info("probe")

	if strings.Contains(sb.String(), "request_id") {
		t.Errorf("无标识时不应出现 request_id 字段: %s", sb.String())
	}
}

// 反复取用同一个 context 不应叠加字段：每次取值都从原始日志器派生，
// 而不是在被派生的那个上面继续派生。
func TestFromContextIsIdempotent(t *testing.T) {
	var sb strings.Builder
	l := NewLoggerWithWriter(&sb, "info", "json", true)
	ctx := WithLoggerAndRequest(context.Background(), l, RequestID("once"))

	FromContext(ctx).Info("first")
	FromContext(ctx).Info("second")

	if n := strings.Count(sb.String(), "request_id"); n != 2 {
		t.Errorf("两条日志各应有一次 request_id，共 2 次，实际 %d 次：%s", n, sb.String())
	}
}

// 放 nil 日志器不应让后续取值 panic。
func TestWithLoggerAndRequestNilLogger(t *testing.T) {
	ctx := WithLoggerAndRequest(context.Background(), nil, RequestID("x"))
	if FromContext(ctx) == nil {
		t.Error("取出不应为 nil")
	}
}
