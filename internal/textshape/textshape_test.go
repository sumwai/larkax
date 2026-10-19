package textshape

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateRejectsControlChars 钉住控制字符的判据与报错的形状。
//
// 三样都要一起断言：这一句是参数错那一档的哨兵（errors.Is）、报错点名了
// 是哪一个字符（换行 / 制表符 / ESC），以及它说清了该改哪个字段。
// 只断言「报错了」不够——把值静默丢掉、或者报一句「取值不合法」，同样能让
// 一条只看 err != nil 的用例通过。
func TestValidateRejectsControlChars(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"换行", "a\nb", "换行（U+000A）"},
		{"回车", "a\rb", "回车（U+000D）"},
		{"制表符", "a\tb", "制表符（U+0009）"},
		{"ESC 序列", "a\x1b[31mb", "ESC（U+001B）"},
		{"NUL", "a\x00b", "NUL（U+0000）"},
		{"DEL", "a\x7fb", "DEL（U+007F）"},
		{"其余 C0 只报码位", "a\x07b", "U+0007"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate("--field", tc.value)
			if err == nil {
				t.Fatal("带控制字符的值应当被拒")
			}
			if !errors.Is(err, ErrControlChar) {
				t.Errorf("错误 %v 不是 ErrControlChar：命令行会把它归到别的退出码", err)
			}
			for _, want := range []string{"控制字符", tc.want, "--field"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("报错 %q 里没有 %q", err.Error(), want)
				}
			}
		})
	}
}

// TestValidateAcceptsOrdinaryText 钉住「拦的是控制字符，不是一张字符白名单」。
//
// 空格、标点、中文与空串都要照收：收窄成 `[A-Za-z0-9_-]` 会挡掉真实存在的
// 展示名，而那种收窄在「只断言坏值被拒」的用例里看不出来。
func TestValidateAcceptsOrdinaryText(t *testing.T) {
	for _, value := range []string{
		"",
		"普通名字",
		"claude-3.5 sonnet",
		"name (test) #1",
		"emoji 😀",
		"全角，标点。",
	} {
		if err := Validate("--field", value); err != nil {
			t.Errorf("Validate(%q) = %v，期望通过", value, err)
		}
	}
}
