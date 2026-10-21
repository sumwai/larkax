package auth

import "testing"

// likeContains 把搜索框的原文包成 LIKE 模式。_ 与 % 必须转义：不转义时
// 搜一个 `%` 会列出全部用户，而搜 `_` 会把「任意一个字」当成条件。
func TestLikeContainsEscapesWildcards(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"alice", `%alice%`},
		{"50%", `%50\%%`},
		{"a_b", `%a\_b%`},
		// 反斜杠自己也要转义：它是 ESCAPE 字符。
		{`a\b`, `%a\\b%`},
		// 两个连在一起（`\%`）与「转义一个 %」是同一个输入片段；
		// 上面的替换器一次扫描，不会把刚加进去的反斜杠再转一遍。
		{`\%`, `%\\\%%`},
	}
	for _, tc := range cases {
		if got := likeContains(tc.in); got != tc.want {
			t.Errorf("likeContains(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}
