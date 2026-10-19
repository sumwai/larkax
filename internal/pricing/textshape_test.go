package pricing

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestRuleTextFieldsRejectControlChars 钉住规则名与模型名单的形状判据。
//
// 规则名进 `price list` / `explain` 的「名称」那一栏，模型名单进同一张表的
// 「模型」那一栏；成本规则那一侧共用同一个名单判据。换行会把一栏拆成两行，
// ESC 这类转义序列会原样进终端。
func TestRuleTextFieldsRejectControlChars(t *testing.T) {
	for _, bad := range []string{"a\nb", "a\tb", "a\x1b[31mb"} {
		if _, err := normalizeName(bad); !errors.Is(err, textshape.ErrControlChar) {
			t.Errorf("normalizeName(%q) = %v，期望 ErrControlChar", bad, err)
		}
		if _, err := normalizePatterns([]string{"claude-3", bad}); !errors.Is(err, textshape.ErrControlChar) {
			t.Errorf("normalizePatterns(%q) = %v，期望 ErrControlChar", bad, err)
		}
	}

	for _, good := range []string{"平台价", "platform 2026"} {
		if _, err := normalizeName(good); err != nil {
			t.Errorf("normalizeName(%q) = %v，期望通过", good, err)
		}
	}
	if _, err := normalizePatterns([]string{" claude-3-opus ", "gpt-4o"}); err != nil {
		t.Errorf("正常名单被拒: %v", err)
	}
}

// TestNormalizePatternsNamesTheItem 钉住名单报错点名是第几项。
func TestNormalizePatternsNamesTheItem(t *testing.T) {
	_, err := normalizePatterns([]string{"claude-3", "gpt\n4o"})
	if !errors.Is(err, textshape.ErrControlChar) {
		t.Fatalf("normalizePatterns = %v，期望 ErrControlChar", err)
	}
	if want := "第 2 个"; !strings.Contains(err.Error(), want) {
		t.Errorf("报错 %q 里没有 %q", err.Error(), want)
	}
}
