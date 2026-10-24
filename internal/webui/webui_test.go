package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture 造一份最小控制台产物：一份 index.html、一份带哈希的脚本、一份图标。
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("建目录失败：%v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s 失败：%v", name, err)
		}
	}
	write("index.html", "<!doctype html><title>控制台</title>")
	write("assets/app-abc123.js", "console.log('app')")
	write("favicon.ico", "ico")
	return dir
}

// apiStub 记下一次委托，回一个可辨认的响应体。
func apiStub(called *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*called++
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("inference"))
	})
}

func newHandler(t *testing.T) (http.Handler, *int) {
	t.Helper()
	called := 0
	h, err := Handler(fixture(t), apiStub(&called))
	if err != nil {
		t.Fatalf("Handler 出错：%v", err)
	}
	return h, &called
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	return rec
}

// TestServesIndexAtRoot 钉住根路径给出控制台入口，且入口不缓存。
func TestServesIndexAtRoot(t *testing.T) {
	h, called := newHandler(t)
	rec := get(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q，期望 text/html", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q，期望 no-cache：index.html 认的是这一批哈希文件名", got)
	}
	if !strings.Contains(rec.Body.String(), "控制台") {
		t.Errorf("响应体 = %q，不像 index.html", rec.Body.String())
	}
	if *called != 0 {
		t.Errorf("推理入口被调用了 %d 次，根路径不该走它", *called)
	}
}

// TestServesHashedAssetWithLongCache 钉住 assets/ 下的文件长缓存。
func TestServesHashedAssetWithLongCache(t *testing.T) {
	h, _ := newHandler(t)
	rec := get(t, h, http.MethodGet, "/assets/app-abc123.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q，期望 immutable 长缓存", got)
	}
	if !strings.Contains(rec.Body.String(), "console.log") {
		t.Errorf("响应体 = %q，不是脚本内容", rec.Body.String())
	}
}

// TestFallsBackToIndexForClientRoutes 钉住前端路由：没有扩展名的未知路径给入口。
func TestFallsBackToIndexForClientRoutes(t *testing.T) {
	h, called := newHandler(t)
	rec := get(t, h, http.MethodGet, "/console/channels/42")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（前端路由回退）", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "控制台") {
		t.Errorf("响应体 = %q，不像 index.html", rec.Body.String())
	}
	if *called != 0 {
		t.Errorf("推理入口被调用了 %d 次", *called)
	}
}

// TestMissingAssetIsNotFound 钉住带扩展名的路径不回落：缺脚本要报 404。
func TestMissingAssetIsNotFound(t *testing.T) {
	h, _ := newHandler(t)
	rec := get(t, h, http.MethodGet, "/assets/app-gone.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "控制台") {
		t.Errorf("响应体回落到 index.html：缺产物会被当成前端坏了")
	}
}

// TestRejectsWriteMethods 钉住写请求不落进前端路由回退。
func TestRejectsWriteMethods(t *testing.T) {
	h, _ := newHandler(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := get(t, h, method, "/console/channels")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 状态码 = %d，期望 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s Allow = %q，期望 \"GET, HEAD\"", method, got)
		}
	}
}

// TestDelegatesInferencePaths 钉住 /v1/ 仍然交给推理入口。
//
// 这是整套改动的支点：静态目录与推理入口都挂在 "/"，分流判错时
// /v1/chat/completions 会拿到一份 HTML，而症状看起来像上游坏了。
func TestDelegatesInferencePaths(t *testing.T) {
	h, called := newHandler(t)
	for _, target := range []string{"/v1/chat/completions", "/v1/models", "/v1/models/claude-x"} {
		rec := get(t, h, http.MethodPost, target)
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s 状态码 = %d，期望进入推理入口（418）", target, rec.Code)
		}
	}
	if *called != 3 {
		t.Errorf("推理入口被调用了 %d 次，期望 3", *called)
	}
}

// TestHeadHasNoBody 钉住 HEAD 与 GET 同一套响应头而没有响应体。
func TestHeadHasNoBody(t *testing.T) {
	h, _ := newHandler(t)
	rec := get(t, h, http.MethodHead, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD 带回了 %d 字节响应体", rec.Body.Len())
	}
}

// TestFaviconGetsShortCache 钉住非哈希的根文件不拿到 immutable 缓存。
func TestFaviconGetsShortCache(t *testing.T) {
	h, _ := newHandler(t)
	rec := get(t, h, http.MethodGet, "/favicon.ico")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Errorf("Cache-Control = %q，期望一小时缓存", got)
	}
}

// TestRejectsDirectoryWithoutIndex 钉住缺 index.html 时构造就失败。
//
// 起得来但每个页面 404 的状态最难查：服务正常、日志正常，只有前端白屏。
func TestRejectsDirectoryWithoutIndex(t *testing.T) {
	if _, err := Handler(t.TempDir(), apiStub(new(int))); err == nil {
		t.Fatal("空目录也构造成功了，期望报错")
	}
}

// TestTraversalStaysInsideRoot 钉住路径穿越拿不到根目录之外的文件。
func TestTraversalStaysInsideRoot(t *testing.T) {
	dir := fixture(t)
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("写根外文件失败：%v", err)
	}
	called := 0
	h, err := Handler(dir, apiStub(&called))
	if err != nil {
		t.Fatalf("Handler 出错：%v", err)
	}
	for _, target := range []string{
		"/../outside.txt",
		"/assets/../../outside.txt",
		"/%2e%2e/outside.txt",
	} {
		rec := get(t, h, http.MethodGet, target)
		if strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%s 返回了根目录外的文件内容", target)
		}
	}
}
