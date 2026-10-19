package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestParseMetaRejectsControlChars 钉住插件元数据里那几个会进表格的字段。
//
// key、name 与 version 都会进 `plugin list` 与 `plugin get`：换行会把一行
// 拆成两行，ESC 这类转义序列会原样进终端。key 上已经有一条更早的空白判据，
// 这里补的是其余 C0 与 DEL；name 与 version 没有别的形状判据。
func TestParseMetaRejectsControlChars(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"key":        "acme-vision",
			"name":       "Acme",
			"version":    "1.0.0",
			"apiVersion": float64(SupportedAPIVersion),
		}
	}

	for _, field := range []string{"name", "version"} {
		for _, bad := range []string{"a\nb", "a\tb", "a\x1b[31mb"} {
			raw := base()
			raw[field] = bad
			if _, err := ParseMeta(raw); !errors.Is(err, textshape.ErrControlChar) {
				t.Errorf("ParseMeta(%s=%q) = %v，期望 ErrControlChar", field, bad, err)
			}
		}
	}

	// key 上已经有一条更早的空白判据：换行与制表符先被它拒掉，其余的 C0
	// （ESC 这类）由形状判据接住。
	raw := base()
	raw["key"] = "a\nb"
	if _, err := ParseMeta(raw); err == nil || !strings.Contains(err.Error(), "空白") {
		t.Errorf("key 里的换行应报空白那一条，得到 %v", err)
	}
	raw = base()
	raw["key"] = "a\x1b[31mb"
	if _, err := ParseMeta(raw); !errors.Is(err, textshape.ErrControlChar) {
		t.Errorf("key 里的 ESC = %v，期望 ErrControlChar", err)
	}

	// 正常值照收：名字里有空格与中文是常见的。
	raw = base()
	raw["name"] = "Acme 视觉插件"
	if _, err := ParseMeta(raw); err != nil {
		t.Errorf("合法元数据被拒: %v", err)
	}
}
