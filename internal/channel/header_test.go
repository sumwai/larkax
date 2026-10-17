package channel

import (
	"strings"
	"testing"
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
