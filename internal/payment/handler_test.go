package payment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCallbackRejectsWrongMethod 只收 POST。
func TestCallbackRejectsWrongMethod(t *testing.T) {
	h := NewHandler(nil, stubName, "")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, CallbackPath(stubName), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q，期望 POST", got)
	}
}

// TestCallbackRejectsNonJSONContentType 请求体必须是 JSON。
func TestCallbackRejectsNonJSONContentType(t *testing.T) {
	h := NewHandler(nil, stubName, "")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, CallbackPath(stubName), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("状态码 = %d，期望 415", rec.Code)
	}
}

// TestCallbackWithoutServiceIsNotFound 验「通道没装」时的响应。
func TestCallbackWithoutServiceIsNotFound(t *testing.T) {
	h := NewHandler(nil, stubName, "v-test")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, CallbackPath(stubName),
		strings.NewReader(`{"order_no":"ORD-1"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if got := rec.Header().Get(HeaderVersion); got != "v-test" {
		t.Errorf("%s = %q，期望 v-test", HeaderVersion, got)
	}
}

// TestJSONContentTypeMatching 验 Content-Type 的比较口径。
func TestJSONContentTypeMatching(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"application/json", true},
		{"Application/JSON", true},
		{"application/json; charset=utf-8", true},
		{" application/json ", true},
		{"application/jsonx", false},
		{"text/json", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isJSON(tc.value); got != tc.want {
			t.Errorf("isJSON(%q) = %v，期望 %v", tc.value, got, tc.want)
		}
	}
}

// TestCallbackErrorMapping 验本包的几类错误各落到哪个状态码。
func TestCallbackErrorMapping(t *testing.T) {
	h := NewHandler(nil, stubName, "")
	cases := []struct {
		name string
		err  error
		want int
		// wantBody 非空时逐字钉住响应体里的那句文案；留空只要求有 "error" 字段。
		wantBody string
	}{
		{"签名不对", ErrBadSignature, http.StatusUnauthorized, ""},
		{"通知不合法", fmt.Errorf("%w：缺少 sign", ErrInvalidNotification), http.StatusBadRequest, ""},
		{"没有这张支付单", fmt.Errorf("%w: ORD-X", ErrOrderNotFound), http.StatusNotFound, ""},
		{"单号内容不同", fmt.Errorf("%w: ORD-X", ErrOrderConflict), http.StatusConflict, ""},
		{"流水号已被另一张单占用", &GatewayTxnConflictError{
			OrderNo: "ORD-B", Gateway: stubName, GatewayTxnID: "TRADE-X", OtherOrderNo: "ORD-A",
		}, http.StatusConflict, ""},
		// 没归类的那一类：包被改过、通知送晚都落在这里。它们要人工处理，所以是
		// 500——4xx 会让通道当成已处理而停止重试。正文是给通道看的固定文案，
		// 不带单号（单号只在服务端日志里），所以这里逐字钉住，不钉 err 的内容。
		{"没归类的（包被改过、通知送晚）",
			errors.New("支付单 ORD-X: 售卖有效期在售出之后改过"),
			http.StatusInternalServerError, "failed to process payment notification"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				CallbackPath(stubName), nil)
			rec := httptest.NewRecorder()
			h.writeServiceError(rec, req, tc.err)
			if rec.Code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d（响应体 %s）", rec.Code, tc.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("响应体不是错误体: %s", rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("响应体里没有那句通用文案 %q：%s", tc.wantBody, rec.Body.String())
			}
		})
	}
}

// TestCallbackGatewayTxnConflictBody 验 409 的说明里点得出两张单。
//
// 状态码对只说明「拒了」，而这条冲突要人去核两张单：响应体里得写出当前单号、
// 流水号与对方单号，否则通道那一侧的运营只看到一句「冲突」。
func TestCallbackGatewayTxnConflictBody(t *testing.T) {
	h := NewHandler(nil, stubName, "")
	err := &GatewayTxnConflictError{
		OrderNo: "ORD-B", Gateway: stubName, GatewayTxnID: "TRADE-X", OtherOrderNo: "ORD-A",
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		CallbackPath(stubName), nil)
	rec := httptest.NewRecorder()
	h.writeServiceError(rec, req, err)

	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（响应体 %s）", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"ORD-A", "ORD-B", "TRADE-X"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("响应体里应当出现 %q：%s", want, rec.Body.String())
		}
	}
}

// TestCallbackRejectsOversizedBody 验 64 KiB 的上限在解析之前生效。
func TestCallbackRejectsOversizedBody(t *testing.T) {
	service := NewService(NewStore(nil), nil, nil, map[string]Channel{stubName: &stubChannel{}})
	h := NewHandler(service, stubName, "")

	body := strings.Repeat("a", maxCallbackBodyBytes+1)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		CallbackPath(stubName), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", rec.Code)
	}
}

// failingWriter 让响应体写入失败，用来走到「编码失败只记日志」那一支。
type failingWriter struct {
	header http.Header
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingWriter) WriteHeader(int) {}

func (w *failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("写入失败")
}

// TestWriteCallbackJSONLogsEncodeFailure 验编码失败时留下一条日志。
//
// 响应头已经发出去了，此时再写一个错误体只会拼出半截 JSON；能做的只有记下来。
func TestWriteCallbackJSONLogsEncodeFailure(t *testing.T) {
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })

	writeCallbackJSON(&failingWriter{}, http.StatusOK, map[string]string{"ok": "true"})

	if !strings.Contains(buf.String(), "写支付回调响应失败") {
		t.Errorf("日志里没有编码失败那一句：%q", buf.String())
	}
}
