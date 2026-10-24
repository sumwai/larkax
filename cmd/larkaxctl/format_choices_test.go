package main

import (
	"regexp"
	"strings"
	"testing"
)

// 这一组用例钉住 `--format` 的取值清单：表里的条目、由表渲染出来的句子，
// 以及帮助文本里那份手写的副本。
//
// 期望值一律写字面量，不从 formatSpecs 拼：拼出来的期望值会跟着表一起变，
// 表被改坏（少一档、顺序颠倒）时用例们一起乖乖变绿，等于没钉。
// 唯一按表拼的是帮助那一条——那里要的正是「两处是否一致」。

// TestFormatSpecsList 钉住表的条目与顺序。
func TestFormatSpecsList(t *testing.T) {
	want := []string{"table", "json"}
	got := formatNames()
	if len(got) != len(want) {
		t.Fatalf("--format 的取值有 %d 项，想要 %d 项：%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项取值 = %q，想要 %q（顺序也是契约：报错里按这个顺序列）",
				i+1, got[i], want[i])
		}
	}
}

// TestFormatChoicesRendering 钉住报错里那一串的逐字形态。
//
// 「 或 」两侧各一个空格：两档时顿号根本用不上，分隔规则退化成一个「或」，
// 空格写错在终端里几乎看不出来，只有逐字比才抓得住。
func TestFormatChoicesRendering(t *testing.T) {
	const want = "取值 table 或 json"
	if got := formatChoices(); got != want {
		t.Errorf("formatChoices() = %q，想要 %q", got, want)
	}
}

// TestFormatFlagErrorMessages 钉住两句报错，四个拼法各来一遍。
//
// 取值清单是同一份，但「哪个名字被漏接」只有分别断言才看得出来：
// isFormatFlag 认 --format、-format、--o、-o，少接一个时
// 这里对应的那条会红，而不是等使用者踩到「未知子命令」。
func TestFormatFlagErrorMessages(t *testing.T) {
	t.Run("取值不认识", func(t *testing.T) {
		cases := []struct {
			name string
			args []string
		}{
			{"--format", []string{"doctor", "--format", "x"}},
			{"-o", []string{"doctor", "-o", "x"}},
		}
		for _, c := range cases {
			_, _, err := takeFormatFlag(c.args)
			if err == nil {
				t.Fatalf("%s 给了不认识的取值时应当报错", c.name)
			}
			const want = `--format 的取值 "x" 不认识：取值 table 或 json`
			if err.Error() != want {
				t.Errorf("%s 的报错 = %q，想要 %q", c.name, err.Error(), want)
			}
		}
	})

	t.Run("取值缺失", func(t *testing.T) {
		cases := []struct {
			name string
			args []string
			want string
		}{
			{"--format", []string{"doctor", "--format"}, "--format 后面要跟 取值 table 或 json"},
			{"-format", []string{"doctor", "-format"}, "-format 后面要跟 取值 table 或 json"},
			{"--o", []string{"doctor", "--o"}, "--o 后面要跟 取值 table 或 json"},
			{"-o", []string{"doctor", "-o"}, "-o 后面要跟 取值 table 或 json"},
		}
		for _, c := range cases {
			_, _, err := takeFormatFlag(c.args)
			if err == nil {
				t.Fatalf("%s 后面没跟取值时应当报错", c.name)
			}
			if err.Error() != c.want {
				t.Errorf("%s 的报错 = %q，想要 %q", c.name, err.Error(), c.want)
			}
		}
	})
}

// usageFormatEnumRE 认根帮助页里 `--format <table|json>` 的枚举。
var usageFormatEnumRE = regexp.MustCompile(`--format <([^>]+)>`)

// TestUsageFormatEnumMatchesTable 让帮助文本与取值表互相钉住。
//
// main.go 的 usageNotes 里那串枚举是手写的第二份（表不是它的来源，改帮助要
// 顺手改表，反之亦然），没有这条用例时「给表加了一档、帮助没改」不会红任何东西。
// 期望值由表拼出来，实际值必须原样出现在帮助里，且恰好一处。
func TestUsageFormatEnumMatchesTable(t *testing.T) {
	want := strings.Join(formatNames(), "|")
	matches := usageFormatEnumRE.FindAllStringSubmatch(usageNotes, -1)
	if len(matches) != 1 {
		t.Fatalf("帮助里 --format 的枚举写法应恰好一处，找到 %d 处", len(matches))
	}
	if got := matches[0][1]; got != want {
		t.Errorf("帮助里 --format 的取值 = %q，取值表是 %q：只改一处时这里必须红",
			got, want)
	}
}
