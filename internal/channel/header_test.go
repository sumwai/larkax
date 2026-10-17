package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ValidHeaderValue 是**两处共用的**判据：插件补丁层的校验（internal/plugin）
// 与适配器最后那道过滤（internal/channel/httpx）。这里逐个字符钉住它的边界——
// 它宽一格就放过一个能注入的头，窄一格就把合法的头拒了，
// 而两处共用意味着这两种错会同时出现在两条路径上。
func TestValidHeaderValue(t *testing.T) {
	合法 := []struct {
		name  string
		value string
	}{
		{"空值", ""},
		{"普通值", "text/plain; charset=utf-8"},
		{"水平制表符", "a\tb"},
		{"首尾空格", "  v  "},
		{"可见字符的两端", "!~"},
		{"obs-text", "\x80\xff"},
		{"含冒号的普通值", "a: b"},
	}
	for _, tc := range 合法 {
		t.Run("合法/"+tc.name, func(t *testing.T) {
			if !ValidHeaderValue(tc.value) {
				t.Errorf("%q 是合法的 field-value，却被拒了", tc.value)
			}
		})
	}

	非法 := []struct {
		name  string
		value string
	}{
		{"NUL", "a\x00b"},
		{"最高一位的 C0", "a\x1fb"},
		{"垂直制表符", "a\x0bb"},
		{"CR", "a\rb"},
		{"LF", "a\nb"},
		{"DEL", "a\x7fb"},
		{"换行注入", "值\r\nX-Injected: evil"},
		{"裸换行注入", "值\nX-Injected: evil"},
	}
	for _, tc := range 非法 {
		t.Run("非法/"+tc.name, func(t *testing.T) {
			if ValidHeaderValue(tc.value) {
				t.Errorf("%q 带控制字符，却被放行了", tc.value)
			}
		})
	}
}

// TestValidHeaderValueAgreesWithNetHTTP 是这条判据与**真正会报错的那一层**的差分。
//
// 上一条用例的 want 表达式是这条判据的重述：它能抓 off-by-one，
// 抓不到「整条判据与 net/http 不一致」。而两处共用这条判据的意义正是让
// 「插件写坏一个头」不表现为「上游失败」（后者会触发换渠道重试，
// 而真正的原因是插件写了坏头），所以要对齐的是 net/http 本身，
// 不是对 RFC 的转述。
//
// 逐字节：0x00–0xFF 每个字节构造成 `"a"+字节+"b"`，一边过判据，一边真发一次
// HTTP 请求到 httptest 服务器看 net/http 收不收，断言两边一致。loopback 上
// 没有别的失败来源，所以「请求失败」只可能是头值被判非法。
func TestValidHeaderValueAgreesWithNetHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := srv.Client()
	// 只在 loopback 上跑，但仍然给个上限：卡住会把整个测试二进制拖到超时。
	client.Timeout = 5 * time.Second

	for i := 0; i < 256; i++ {
		value := "a" + string([]byte{byte(i)}) + "b"

		// 直接写 Header map 而不是 Set：Set 会做规范化，而这里要测的是值的判据。
		// 显式给 ctx 是这一层的约定：这条用例没有取消语义，但请求要带着 ctx 构造。
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header["X-Probe"] = []string{value}

		resp, err := client.Do(req)
		nethttp := err == nil
		if resp != nil {
			resp.Body.Close()
		}
		if got := ValidHeaderValue(value); got != nethttp {
			t.Errorf("字节 0x%02x：ValidHeaderValue = %v，net/http = %v", i, got, nethttp)
		}
	}
}

// 判据必须逐个字节看，而不是只看开头几个：注入用的字符可以出现在值的任何位置。
// 这里把 0x00–0xFF 的每一个字节都放在中间过一遍，并钉住它该被放行还是拒绝。
func TestValidHeaderValueChecksEveryByte(t *testing.T) {
	for i := 0; i < 256; i++ {
		value := strings.Repeat("a", 40) + string([]byte{byte(i)}) + strings.Repeat("a", 40)
		want := i == ' ' || i == '\t' || (i >= 0x21 && i <= 0x7e) || i >= 0x80
		if got := ValidHeaderValue(value); got != want {
			t.Errorf("字节 0x%02x 在中间：ValidHeaderValue = %v，想要 %v", i, got, want)
		}
	}
}
