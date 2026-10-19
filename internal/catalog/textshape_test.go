package catalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestModelTextFieldsRejectControlChars 钉住模型目录里那几个展示文本的形状判据。
//
// 这些值都会进 `model list` 的表格或 `model get` 的单条回显：换行会把一栏
// 拆成两行，ESC 这类转义序列会原样进终端。规范名与上游模型名本来就有别的
// 判据（空白、非空），这里只补控制字符那一类——两者的报错也各说各的，
// 因为下一步动作不同。
func TestModelTextFieldsRejectControlChars(t *testing.T) {
	cases := []struct {
		name string
		run  func(string) error
	}{
		{"展示名", func(v string) error { _, err := normalizeDisplayName(v); return err }},
		{"厂商", func(v string) error { _, err := optionalText("--vendor（厂商）", v); return err }},
		{"家族", func(v string) error { _, err := optionalText("--family（家族）", v); return err }},
		{"上游模型名", func(v string) error { _, err := normalizeUpstreamModel(v); return err }},
		{"模态", func(v string) error { return checkTexts("--input-modalities（输入模态）", []string{v}) }},
	}

	for _, tc := range cases {
		for _, bad := range []string{"a\nb", "a\tb", "a\x1b[31mb"} {
			if err := tc.run(bad); !errors.Is(err, textshape.ErrControlChar) {
				t.Errorf("%s: run(%q) = %v，期望 ErrControlChar", tc.name, bad, err)
			}
		}
	}

	// 规范名已经有一条更早的空白判据：换行与制表符先被它拒掉，剩下的 C0
	// （ESC 这类）由形状判据接住。两条各说各的，不合并成一句。
	if _, err := normalizeCanonicalName("a\nb"); err == nil ||
		!strings.Contains(err.Error(), "空白") {
		t.Errorf("规范名里的换行应报空白那一条，得到 %v", err)
	}
	if _, err := normalizeCanonicalName("a\x1b[31mb"); !errors.Is(err, textshape.ErrControlChar) {
		t.Errorf("规范名里的 ESC = %v，期望 ErrControlChar", err)
	}

	// 正常值照收：点、空格、中文都在展示文本里真实存在。
	for _, tc := range cases {
		if err := tc.run("Claude 3.5 中文版"); err != nil {
			t.Errorf("%s: 正常值被拒: %v", tc.name, err)
		}
	}
	if _, err := normalizeCanonicalName("claude-3-5-sonnet"); err != nil {
		t.Errorf("规范名的正常值被拒: %v", err)
	}
}

// TestCheckTextsNamesTheItem 钉住模态清单的报错点名是第几项。
//
// 名单里的元素不 trim（见 nonNilStrings），所以判的是原值；报错带序号，
// 否则一个长名单里有一项带换行，读的人不知道该改哪一个。
func TestCheckTextsNamesTheItem(t *testing.T) {
	err := checkTexts("--input-modalities（输入模态）", []string{"text", "a\nb"})
	if !errors.Is(err, textshape.ErrControlChar) {
		t.Fatalf("checkTexts = %v，期望 ErrControlChar", err)
	}
	if want := "第 2 个"; !strings.Contains(err.Error(), want) {
		t.Errorf("报错 %q 里没有 %q", err.Error(), want)
	}
}
