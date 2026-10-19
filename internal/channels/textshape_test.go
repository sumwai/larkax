package channels

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestNormalizeNameRejectsControlChars 钉住渠道名的形状判据。
//
// 渠道名进 `channel list` 与 `channel health` 的「名称」那一栏。判在 trim
// 之后的值上，与落库的那个串一致。
func TestNormalizeNameRejectsControlChars(t *testing.T) {
	for _, value := range []string{"a\nb", "a\tb", "a\x1b[31mb", " a\nb "} {
		if _, err := normalizeName(value); !errors.Is(err, textshape.ErrControlChar) {
			t.Errorf("normalizeName(%q) = %v，期望 ErrControlChar", value, err)
		}
	}

	for _, value := range []string{"主渠道", "backup 1", "  backup 1  "} {
		if _, err := normalizeName(value); err != nil {
			t.Errorf("normalizeName(%q) = %v，期望通过", value, err)
		}
	}

	// 空值仍然报它原来那一句：形状判据不该把「没填」说成「含控制字符」。
	for _, value := range []string{"", "   "} {
		_, err := normalizeName(value)
		if err == nil || err.Error() != "渠道名不能为空" {
			t.Errorf("normalizeName(%q) = %v，期望「渠道名不能为空」", value, err)
		}
	}
}

// TestNormalizeModelsRejectsControlChars 钉住模型名单里每一项的形状判据。
//
// 这一栏在 `channel list` 里按逗号连起来打，一个带换行的模型名会在表里
// 折出续行。报错要点名是第几项——名单可能很长，只说「--models 不合法」
// 等于让人一行一行去找。
func TestNormalizeModelsRejectsControlChars(t *testing.T) {
	_, err := normalizeModels([]string{"claude-3-opus", "gpt\n4o"})
	if !errors.Is(err, textshape.ErrControlChar) {
		t.Fatalf("normalizeModels = %v，期望 ErrControlChar", err)
	}
	if want := "第 2 个"; !strings.Contains(err.Error(), want) {
		t.Errorf("报错 %q 里没有 %q", err.Error(), want)
	}

	got, err := normalizeModels([]string{" claude-3-opus ", "gpt-4o"})
	if err != nil {
		t.Fatalf("正常名单被拒: %v", err)
	}
	if len(got) != 2 || got[0] != "claude-3-opus" {
		t.Errorf("normalizeModels = %v，期望去掉两头空白后的两项", got)
	}
}
