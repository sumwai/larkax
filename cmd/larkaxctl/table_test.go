package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sumwai/larkax/internal/catalog"
)

// ── 判宽 ────────────────────────────────────────────────────────────

// TestDisplayWidth 钉住判宽本身：东亚宽与全角记 2 列，其余记 1 列。
//
// 期望值一律写**字面量**，不用 displayWidth 自己算一遍——拿被测函数算期望值，
// 改坏了也照样通过（见 LESSONS.md 第 21 条：用常量测常量等于没测）。
func TestDisplayWidth(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"空标签", "", 0},
		{"单个 ASCII 字符", "x", 1},
		{"单个汉字", "模", 2},
		{"全 ASCII 标签", "GENERATION", 10},
		{"全中文标签", "采集源", 6},
		{"中英混排标签", "采集源 ID", 9},
		{"中文里夹数字", "渠道 12", 7},
		{"全角标点", "（无法显示）", 12},
		{"全角空格", "状态　值", 8},
		{"半角标点按一列", "tools,vision", 12},
		{"箭头按一列", "text→text", 9},
		{"省略号按一列", "…", 1},
		{"谚文音节", "모델", 4},
		{"片假名", "モデル", 6},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayWidth(tc.in); got != tc.want {
				t.Errorf("displayWidth(%q) = %d，期望 %d", tc.in, got, tc.want)
			}
		})
	}
}

// ── 对齐 ────────────────────────────────────────────────────────────

// terminalCols 是**测试自己的**宽度折算：ASCII 记 1 列，其余记 2 列。
//
// 刻意不复用被测的 displayWidth。两边用同一个函数时，把它改坏（CJK 记 1）会让
// 渲染与折算一起错、彼此仍然对得上，用例照样绿——那正是「守卫没人钉住」
// （见 LESSONS.md 第 23 条）。这里只用得到 ASCII 与汉字两类，自己数就够了。
func terminalCols(s string) int {
	n := 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			n++
			continue
		}
		n += 2
	}
	return n
}

// columnStarts 把一行按**显示宽度**折算成每一栏的起点列号。
//
// cells 是这一行开头若干栏的原文（**不要给到最后那一栏**）；函数按 terminalCols
// 逐栏吃掉它们、跳过栏间空格，返回每一栏的起点，外加再下一栏的起点。
func columnStarts(t *testing.T, line string, cells []string) []int {
	t.Helper()

	var starts []int
	col := 0
	rest := line
	for _, cell := range cells {
		if !strings.HasPrefix(rest, cell) {
			t.Fatalf("栏 %q 不在 %q 的第 %d 列处", cell, line, col)
		}
		starts = append(starts, col)
		col += terminalCols(cell)
		rest = rest[len(cell):]

		gap := len(rest) - len(strings.TrimLeft(rest, " "))
		col += gap
		rest = rest[gap:]
	}
	if rest == "" {
		t.Fatalf("%q 在吃掉 %v 之后没有下一栏", line, cells)
	}
	return append(starts, col)
}

// assertColumnStarts 逐行折算栏起点，断言每一行都等于 want。
//
// 断言的是**绝对列号**，不是「各行彼此一致」：被测实现按同一个（可能是错的）
// 规则渲染所有行时，各行照样一致——那种断言分辨不出「中文算了两列没有」。
func assertColumnStarts(t *testing.T, out string, rows [][]string, want []int) {
	t.Helper()

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("打了 %d 行，期望 %d 行：\n%s", len(lines), len(rows), out)
	}
	for i, line := range lines {
		got := columnStarts(t, line, rows[i])
		if len(got) != len(want) {
			t.Fatalf("第 %d 行折算出 %d 栏，期望 %d 栏：%q", i, len(got), len(want), line)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Errorf("第 %d 行第 %d 栏起点 = %d，期望 %d（整行 %q）", i, j, got[j], want[j], line)
			}
		}
	}
}

// TestTableWriterAlignsByDisplayWidth 是 issue 里那张最小的复现表。
//
// 三行第一栏的 rune 数分别是 2 / 7 / 2，按 rune 数补空格时终端里对不齐；
// 按显示宽度（2 / 9 / 4 列）补才对得上。第二栏的起点写死成 11：
// 「采集源 ID」= 3 个汉字（6 列）+ 空格（1 列）+「ID」（2 列）= 9 列，加两列栏间距。
func TestTableWriterAlignsByDisplayWidth(t *testing.T) {
	var buf bytes.Buffer
	tw := newTableWriter(&buf)
	_, _ = fmt.Fprintln(tw, "ID\t7")
	_, _ = fmt.Fprintln(tw, "采集源 ID\t7")
	_, _ = fmt.Fprintln(tw, "状态\tenabled")
	if err := tw.Flush(); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}

	rows := [][]string{{"ID"}, {"采集源 ID"}, {"状态"}}
	assertColumnStarts(t, buf.String(), rows, []int{0, 11})

	// 逐字节钉一遍排版：列号对得上、行尾却多出一串空格，也算没对齐。
	want := "ID         7\n" +
		"采集源 ID  7\n" +
		"状态       enabled\n"
	if got := buf.String(); got != want {
		t.Errorf("输出 =\n%q\n期望 =\n%q", got, want)
	}
}

// TestTableWriterLabelEdgeCases 钉住几种极端标签：空、单字符、全中文。
//
// 空标签那一行的第一栏补出来全是空格，最容易露馅：按 rune 数补时它与其他行
// 的列起点分得开，按显示宽度补才对得上。
func TestTableWriterLabelEdgeCases(t *testing.T) {
	var buf bytes.Buffer
	tw := newTableWriter(&buf)
	_, _ = fmt.Fprintln(tw, "\t空标签")
	_, _ = fmt.Fprintln(tw, "x\t单字符")
	_, _ = fmt.Fprintln(tw, "模型\t全中文")
	if err := tw.Flush(); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}

	// 最宽的是「模型」：2 个汉字 = 4 列，加两列间距，第二栏从第 6 列起。
	rows := [][]string{{""}, {"x"}, {"模型"}}
	assertColumnStarts(t, buf.String(), rows, []int{0, 6})

	want := "      空标签\n" +
		"x     单字符\n" +
		"模型  全中文\n"
	if got := buf.String(); got != want {
		t.Errorf("输出 =\n%q\n期望 =\n%q", got, want)
	}
}

// TestModelDetailColumnsLineUp 钉住 `model show` 那张表。
//
// 选它是因为它同一栏里 ASCII 标签（`ID`）与中文标签（`采集源 ID`、`上下文上限`）
// 混在一起——正是 issue 里说的那种「最明显」的情形。标签名写死在下面：
// 它们是这张表的排版契约，改了一个就该让这条用例红。
func TestModelDetailColumnsLineUp(t *testing.T) {
	var buf bytes.Buffer
	printModelDetail(&buf, catalog.Model{
		ID:               7,
		CanonicalName:    "claude-x",
		DisplayName:      "示例模型",
		Vendor:           "example",
		ContextLimit:     200000,
		ModalitiesInput:  []string{"text"},
		ModalitiesOutput: []string{"text"},
		UpdatedAt:        time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	})

	labels := []string{
		"ID", "规范名", "展示名", "厂商", "家族",
		"上下文上限", "输入上限", "输出上限", "能力",
		"输入模态", "输出模态", "知识截止", "发布日期", "采集源 ID", "更新时间",
	}

	// 先钉行数：遍历一堆行的断言在一条都没有时会全部静默通过
	//（见 LESSONS.md 第 30 条），而这里恰好是「表里少打了一行」最难发现。
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != len(labels) {
		t.Fatalf("打了 %d 行，期望 %d 行：\n%s", len(lines), len(labels), buf.String())
	}

	// 最宽的标签是「上下文上限」：5 个汉字 = 10 列，加两列间距 → 第二栏从第 12 列起。
	rows := make([][]string, 0, len(labels))
	for _, label := range labels {
		rows = append(rows, []string{label})
	}
	assertColumnStarts(t, buf.String(), rows, []int{0, 12})

	// 哨兵：这张表必须真的中英混排。全是 ASCII 标签时上面那条断言照样通过，
	// 而它要证明的正是「中文标签占两列」。
	var ascii, cjk int
	for _, label := range labels {
		if terminalCols(label) == len(label) {
			ascii++
		} else {
			cjk++
		}
	}
	if ascii == 0 || cjk == 0 {
		t.Fatalf("这张表要同时有 ASCII 与中文标签才证明得了判宽，得到 %d / %d", ascii, cjk)
	}
}

// TestModelListColumnsLineUp 钉住 `model list` 那张最宽的表：十栏全都要对齐。
//
// 表头全是中文、数据行全是 ASCII，所以它同时钉住两件事：中文表头不把数据行
// 挤歪，以及多栏时每一栏的起点都算对。
func TestModelListColumnsLineUp(t *testing.T) {
	var buf bytes.Buffer
	printModelList(&buf, []catalog.Model{{
		ID:               7,
		CanonicalName:    "claude-x",
		DisplayName:      "示例模型",
		Family:           "example",
		ContextLimit:     200000,
		Capabilities:     catalog.Capabilities{Tools: true, Vision: true},
		ModalitiesInput:  []string{"text"},
		ModalitiesOutput: []string{"text"},
		UpdatedAt:        time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
	}})

	header := []string{"ID", "规范名", "展示名", "厂商", "家族", "上下文", "输入", "输出", "能力", "模态"}
	row := []string{"7", "claude-x", "示例模型", "-", "example", "200000", "-", "-", "tools,vision", "text→text"}

	// 每一栏的起点：栏宽取「表头与数据行里较宽的那个」+ 两列间距。
	// 第二栏的起点 4 来自 `ID`/`7`（2 列），第三栏的 14 来自 `claude-x`（8 列）……
	// 最后一个 73 是末栏 `模态` 的起点——它自己不补空格，但起点要对。
	assertColumnStarts(t, buf.String(), [][]string{header[:9], row[:9]},
		[]int{0, 4, 14, 24, 30, 39, 47, 53, 59, 73})
}

// TestTableWriterKeepsEmptyAndShortLists 钉住没有数据行时的输出。
//
// 这三条命令各自在空清单时另打一句话（「还没有渠道。」一类），不走表格；
// 这里钉的是表格自身在零行、一行时的行为，免得它把空表打成一个空行。
func TestTableWriterKeepsEmptyAndShortLists(t *testing.T) {
	var buf bytes.Buffer
	tw := newTableWriter(&buf)
	_, _ = fmt.Fprintln(tw, "ID\t名称")
	if err := tw.Flush(); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	if got, want := buf.String(), "ID  名称\n"; got != want {
		t.Errorf("只有表头时输出 = %q，期望 %q", got, want)
	}

	buf.Reset()
	tw = newTableWriter(&buf)
	if err := tw.Flush(); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	if got := buf.String(); got != "" {
		t.Errorf("一行都没写时输出 = %q，期望空", got)
	}
}

// TestTableWriterFlushesOneTableAtATime 钉住「Flush 之前什么都不出去」与
// 「两次 Flush 是两张独立的表」。
//
// 对齐要先知道每一栏有多宽，所以行是攒着的。攒的行为本身也要钉：攒漏了，
// 表会在第一行到齐时就定型，后面更宽的标签再也撑不开它。
func TestTableWriterFlushesOneTableAtATime(t *testing.T) {
	var buf bytes.Buffer
	tw := newTableWriter(&buf)
	_, _ = fmt.Fprintln(tw, "长标签\t1")
	_, _ = fmt.Fprintln(tw, "短\t2")
	if got := buf.String(); got != "" {
		t.Fatalf("Flush 之前就写出去了：%q", got)
	}
	if err := tw.Flush(); err != nil {
		t.Fatalf("Flush 失败: %v", err)
	}
	want := "长标签  1\n短      2\n"
	if got := buf.String(); got != want {
		t.Errorf("输出 = %q，期望 %q", got, want)
	}

	// 第二张表的栏宽不与第一张串味：`短` 在上一张里被撑到 6 列，这里不该。
	_, _ = fmt.Fprintln(tw, "短\t3")
	_, _ = fmt.Fprintln(tw, "长一些的标签\t4")
	if err := tw.Flush(); err != nil {
		t.Fatalf("第二次 Flush 失败: %v", err)
	}
	want += "短            3\n长一些的标签  4\n"
	if got := buf.String(); got != want {
		t.Errorf("两次 Flush 之后输出 = %q，期望 %q", got, want)
	}
}
