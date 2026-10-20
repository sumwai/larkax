package credential

import "testing"

// Hint 取明文凭证的末几位。
//
// 断言写的是**具体字符**而不是「长度是 4」：取前四位、取末五位、取末三位，
// 长度断言全都过得去，而清单上显示的是另一个凭证的尾巴——它看起来完全正常，
// 于是运维会照它去比对，得出一个错的结论。
func TestHintTakesTrailingRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"典型的凭证", "test-key-abcdefghijklmnop", "mnop"},
		{"恰好四位", "abcd", "abcd"},
		{"不足四位", "abc", "abc"},
		{"一位", "x", "x"},
		{"空串", "", ""},
		// 多字节必须按字符切：按字节切会从中间切开一个字，打出来是乱码。
		// "甲乙丙丁戊" 是 5 个字符、15 字节，取末 4 **字节**会得到半个「戊」。
		{"多字节按字符切", "甲乙丙丁戊", "乙丙丁戊"},
		{"多字节恰好四位", "甲乙丙丁", "甲乙丙丁"},
		{"多字节不足四位", "令牌甲", "令牌甲"},
		// 恰好四位、且原文不是合法 UTF-8：`<=` 改成 `<` 时这一档会落进
		// 「取末四位」分支，而 []rune 重新编码会把每个非法字节换成 U+FFFD，
		// 结果与原文不同。合法 UTF-8 上两个分支给出同一串（末四位就是全部），
		// 所以只有这一档能区分它们。「短到没有末几位时原样返回」要按字面理解：
		// 凭证是 Go 字符串，不保证是合法 UTF-8，原样返回就不能重编码。这类值
		// 写不进 credential_hint 那个 text 列，所以这一档守的是包内契约，
		// 不是存储路径上的行为。
		{"非法 UTF-8 恰好四位", "\xff\xfe\xfd\xfc", "\xff\xfe\xfd\xfc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Hint(c.in); got != c.want {
				t.Errorf("Hint(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// Hint 与凭证的长度无关地取尾部：前缀一样的两个凭证必须给出不同的提示，
// 否则界面拿它辨识时会把两条渠道认成同一条。
func TestHintDistinguishesCredentialsSharingPrefix(t *testing.T) {
	a := Hint("test-key-000000000000aaaa")
	b := Hint("test-key-000000000000bbbb")
	if a == b {
		t.Fatalf("两条尾部不同的凭证给出同一个提示 %q", a)
	}
	if a != "aaaa" || b != "bbbb" {
		t.Errorf("提示 = %q / %q，期望 aaaa / bbbb", a, b)
	}
}
