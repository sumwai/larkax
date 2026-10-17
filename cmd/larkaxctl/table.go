package main

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

// columnGap 是相邻两栏之间的空格数。
//
// 取 2 是为了与 `tabwriter` 打出来的排版一致——它的参数是
// `NewWriter(w, 0, 0, 2, ' ', 0)`。改的是宽度怎么算，不是看起来长什么样。
const columnGap = 2

// tableWriter 是按**显示宽度**对齐的表格写入器，取代了标准库的 `tabwriter`。
//
// `tabwriter` 按 **rune 数**补空格，而中文标签在终端里占两列：同一栏里既有
// `ID` 又有 `采集源 ID` 时，两行补出来的 rune 数一样，看起来却差好几列。
// 它没有宽度钩子，所以「换一个参数」解决不了，只能自己算。
//
// 用法与 `tabwriter` 一样：先把若干行写进来，最后 Flush。栏之间用 \t 分隔。
//
//	tw := newTableWriter(w)
//	_, _ = fmt.Fprintln(tw, "ID\t规范名")
//	_ = tw.Flush()
//
// 与 `tabwriter` 有两处不同，都是刻意的：
//
//   - 每一栏的宽度取**所有行**里该栏的最大显示宽度，不像 `tabwriter` 那样按
//     「栏块」分段。本包的表都是矩形的，全表一个宽度更好预测。
//   - 行尾那一栏不补空格（`tabwriter` 在没有尾随 \t 时也不补）。补了会在行尾
//     留下一串看不见的空格，跟着复制粘贴与 `| cat` 一起跑出去。
type tableWriter struct {
	w    io.Writer
	buf  []byte
	rows [][]string
}

// newTableWriter 建一个按显示宽度对齐的表格写入器。
func newTableWriter(w io.Writer) *tableWriter {
	return &tableWriter{w: w}
}

// Write 收下 p，把其中已经完整的行切成栏存起来。
//
// 真正的输出发生在 Flush：对齐要先知道每一栏有多宽，而宽度要等所有行到齐。
// 所以这里与 bufio 一样先攒着，返回写入的字节数而不报错。
func (tw *tableWriter) Write(p []byte) (int, error) {
	tw.buf = append(tw.buf, p...)
	for {
		i := bytes.IndexByte(tw.buf, '\n')
		if i < 0 {
			break
		}
		tw.addLine(string(tw.buf[:i]))
		tw.buf = tw.buf[i+1:]
	}
	return len(p), nil
}

// Flush 把收下的行按显示宽度对齐后写出去，返回底层写入的错误。
//
// 调用方一律写 `_ = tw.Flush()`，不看这个返回值——这张表是给人看的清单，
// 没有任何处置挂在它的成败上。留着 error 是为了真要分派时不必改签名。
func (tw *tableWriter) Flush() error {
	// 最后一行可能没有换行符结尾，收尾时一并处理。
	if len(tw.buf) > 0 {
		tw.addLine(string(tw.buf))
		tw.buf = tw.buf[:0]
	}

	widths := tw.columnWidths()
	var sb strings.Builder
	for _, cells := range tw.rows {
		for i, cell := range cells {
			if i > 0 {
				sb.WriteString(strings.Repeat(" ", columnGap))
			}
			sb.WriteString(cell)
			if i < len(cells)-1 {
				sb.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)))
			}
		}
		sb.WriteString("\n")
	}
	tw.rows = nil

	_, err := io.WriteString(tw.w, sb.String())
	return err
}

// addLine 把一行按制表符切成栏存起来。
func (tw *tableWriter) addLine(line string) {
	tw.rows = append(tw.rows, strings.Split(line, "\t"))
}

// columnWidths 算每一栏的显示宽度。
//
// 取的是**还有下一栏**的那些行里该栏的最大值：行尾那一栏不补空格（见 Flush），
// 把它算进来只会白白撑宽别的行。行数不齐时按各自有的栏算，短行不受影响。
func (tw *tableWriter) columnWidths() []int {
	var widths []int
	for _, cells := range tw.rows {
		for i := 0; i < len(cells)-1; i++ {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			if w := displayWidth(cells[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// displayWidth 返回 s 在终端里占的列数：东亚宽与全角字符记 2，其余记 1。
//
// **样式码不占列**：整段 ANSI 转义序列按 0 算（见 ansiSeqLen）。上色之后栏宽
// 仍然对得上，就是靠这一条——`\x1b[1m` 有 4 个字节、0 列，把它算进去会让
// 所有带样式的栏一起往右错开。
//
// 只判**东亚宽（W）与全角（F）**这两类，不引 golang.org/x/text/width：那要
// 一整张 Unicode 宽度表，而这里需要的只是「汉字、假名、谚文与全角标点占两列」
// 这一条，它覆盖了本包会打出来的全部标签。
//
// 已知边界（都按「占一列」算，真出现了要改的是这里，不是调用方）：
//
//   - **组合字符**：`e` + 声调符号算 2 列，实际渲染是 1 列。
//   - **零宽字符**：算 1 列，实际占 0 列。
//   - **emoji**：在 Unicode 里也是宽字符，但不收进来——本包的输出是运维清单，
//     不该出现 emoji；真出现了，那一栏会差一列，而不是被静默算对。
//
// 另有**歧义宽度（A）**一类，按窄的算。本包唯一用到的是省略号 `…`
// （见 channel.go 的 credentialKeysLabel）：它在东亚字体下可能占两列，而
// 绝大多数终端与字体按一列渲染——取多数的那一侧。
func displayWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == escByte {
			i += ansiSeqLen(s[i:])
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		n += runeWidth(r)
		i += size
	}
	return n
}

// ansiSeqLen 返回 s 开头那条 ANSI 转义序列占的字节数。
//
// 只认 CSI（`ESC [ 参数 终结字节`，终结字节在 0x40–0x7E）：本包自己产生的
// 只有 SGR 这一类。认不出形状时返回 1——只跳过那个 ESC，后面的字节按普通
// 字符算：宁可把一栏算宽一点（那一栏会宽出来，一眼看得见），也不要把一条
// 不认识的序列整个吞掉，那会让**后面每一栏**的宽度都算少。
func ansiSeqLen(s string) int {
	if len(s) < 2 || s[1] != '[' {
		return 1
	}
	for i := 2; i < len(s); i++ {
		if s[i] >= 0x40 && s[i] <= 0x7E {
			return i + 1
		}
	}
	return 1
}

// runeWidth 返回一个字符的显示列数。
func runeWidth(r rune) int {
	for _, rg := range wideRanges {
		if r < rg.lo {
			break
		}
		if r <= rg.hi {
			return 2
		}
	}
	return 1
}

// wideRanges 是东亚宽与全角的码点区间，按 lo 升序（runeWidth 靠这个顺序提前
// 退出）。取值来自 Unicode 的 EastAsianWidth.txt，只收 W 与 F 两类里中文、
// 日文、韩文与全角标点实际用得到的区间，未分配的码点不单独挖洞。
var wideRanges = []struct{ lo, hi rune }{
	{0x1100, 0x115F},   // 谚文字母
	{0x2E80, 0x303E},   // 中日韩部首、康熙部首、中日韩符号与标点（含全角空格）
	{0x3041, 0x33FF},   // 平假名、片假名、注音、谚文兼容字母、带圈与兼容的中日韩字符
	{0x3400, 0x4DBF},   // 中日韩统一表意文字扩展 A
	{0x4E00, 0x9FFF},   // 中日韩统一表意文字
	{0xA000, 0xA4CF},   // 彝文
	{0xAC00, 0xD7A3},   // 谚文音节
	{0xF900, 0xFAFF},   // 中日韩兼容表意文字
	{0xFE10, 0xFE19},   // 竖排形式
	{0xFE30, 0xFE4F},   // 中日韩兼容形式
	{0xFF00, 0xFF60},   // 全角 ASCII 与全角标点
	{0xFFE0, 0xFFE6},   // 全角符号（如全角人民币号）
	{0x20000, 0x2FFFD}, // 中日韩统一表意文字扩展 B 及以后
	{0x30000, 0x3FFFD}, // 同上，扩展 G 及以后
}
