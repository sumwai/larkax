package manpage

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 本文件是 cli-reference.md 里那一小片 markdown 的读法，以及把它写成 roff 的写法。
//
// # 为什么只认这几种块
//
// 只认标题、段落、列表、表格四种——源文件里今天就只用这四种。遇到别的
// （围栏代码块、引用块、嵌套列表）**报错**，不跳过：跳过的话那一段说明会从手页上
// 消失，而「少了一段」与「本来就没写」在产物里长得一模一样（LESSONS.md 第 2 条）。
// 报错的位置带上行号，改起来才知道去哪儿改。

// Block 是源文件里的一个块。
type Block interface{ block() }

// Heading 是标题。Level 是 # 的个数（1..4）。
type Heading struct {
	Level int
	Text  string
}

// Paragraph 是一段文字（源文件里连着写的几行合成一段）。
type Paragraph struct{ Text string }

// List 是一个无序列表。
type List struct{ Items []string }

// Table 是一张表：Head 是表头，Rows 是其余各行。
//
// 表头的分隔行（`|---|---|`）在解析时就被吃掉，不进这里。
type Table struct {
	Head []string
	Rows [][]string
}

func (Heading) block()   {}
func (Paragraph) block() {}
func (List) block()      {}
func (Table) block()     {}

// parseBlocks 把源文件切成块。
//
// 段落与列表项内部的换行按 joinWrapped 的规则接回去：源文件是按列宽硬换行
// 的，直接留着会让 roff 在 CJK 文本里找不到断点，接的时候插一个空格又会在中文
// 句子中间多出一个空格。
func parseBlocks(md string) ([]Block, error) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")

	var (
		blocks []Block
		para   []string
		items  []string
	)
	flushPara := func() {
		if len(para) > 0 {
			blocks = append(blocks, Paragraph{Text: joinWrapped(para)})
			para = nil
		}
	}
	flushList := func() {
		if len(items) > 0 {
			blocks = append(blocks, List{Items: items})
			items = nil
		}
	}
	flush := func() {
		flushPara()
		flushList()
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			continue
		}
		if strings.HasPrefix(trimmed, "```") {
			return nil, fmt.Errorf("第 %d 行：围栏代码块不在本生成器认识的范围里", i+1)
		}
		if strings.HasPrefix(trimmed, ">") {
			return nil, fmt.Errorf("第 %d 行：引用块不在本生成器认识的范围里", i+1)
		}
		if strings.HasPrefix(trimmed, "|") {
			flush()
			table, next, err := parseTable(lines, i)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, table)
			i = next - 1
			continue
		}
		if level, text, ok := heading(trimmed); ok {
			flush()
			blocks = append(blocks, Heading{Level: level, Text: text})
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			// 认不出层级（五级及以上，或者 `#` 后面没空格）时不悄悄当成一段正文：
			// 那样它会进产物，只是不是一个标题，页面上看起来像漏了一段。
			return nil, fmt.Errorf("第 %d 行：这里像标题又认不出层级（只认 1~4 级，`#` 后面要有空格）：%s",
				i+1, trimmed)
		}
		if strings.HasPrefix(trimmed, "- ") {
			flushPara()
			items = append(items, strings.TrimSpace(trimmed[2:]))
			continue
		}
		// 以空白开头的行是上一段或上一条的续行（源文件按列宽硬换行）。
		if line != trimmed {
			switch {
			case len(items) > 0:
				items[len(items)-1] = joinWrapped([]string{items[len(items)-1], trimmed})
			case len(para) > 0:
				para = append(para, trimmed)
			default:
				return nil, fmt.Errorf("第 %d 行：缩进了一块不属于任何段落或列表项的正文", i+1)
			}
			continue
		}
		flushList()
		para = append(para, trimmed)
	}
	flush()
	return blocks, nil
}

// heading 认 `#` 开头的标题行，返回层级与标题文本。
func heading(trimmed string) (int, string, bool) {
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 4 {
		return 0, "", false
	}
	rest := trimmed[level:]
	if rest != "" && !strings.HasPrefix(rest, " ") {
		return 0, "", false
	}
	return level, strings.TrimSpace(rest), true
}

// parseTable 从第 i 行起读一张表，返回它与下一行的下标。
func parseTable(lines []string, i int) (Table, int, error) {
	var rows [][]string
	for ; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "|") {
			break
		}
		rows = append(rows, splitRow(trimmed))
	}
	if len(rows) == 0 {
		return Table{}, i, fmt.Errorf("第 %d 行：表格一行都没有", i+1)
	}
	if len(rows) < 2 || !isSeparator(rows[1]) {
		return Table{}, i, fmt.Errorf("第 %d 行：表格第二行不是分隔行（|---|---|）", i+2)
	}
	table := Table{Head: rows[0], Rows: rows[2:]}
	for n, row := range table.Rows {
		if len(row) != len(table.Head) {
			return Table{}, i, fmt.Errorf("第 %d 行：这一行有 %d 栏，表头有 %d 栏",
				i-len(table.Rows)+n+1, len(row), len(table.Head))
		}
	}
	return table, i, nil
}

// splitRow 把 `| a | b |` 拆成两栏。
func splitRow(trimmed string) []string {
	body := strings.TrimSuffix(strings.TrimPrefix(trimmed, "|"), "|")
	parts := strings.Split(body, "|")
	for n := range parts {
		parts[n] = strings.TrimSpace(parts[n])
	}
	return parts
}

// isSeparator 认 `|---|---|` 那一行。
func isSeparator(cells []string) bool {
	for _, c := range cells {
		if c == "" {
			return false
		}
		if strings.Trim(c, "-:") != "" {
			return false
		}
	}
	return true
}

// joinWrapped 把硬换行接回来。
//
// 两侧都是 ASCII 词字符时插一个空格，其余情况直接接上：源文件是在中文句子中间
// 换行的，插空格会在页面上多出一个空格；而英文单词被换行切开时（源文件不会这么
// 换，但下一个写的人可能会）不插空格会把两个词粘起来。判据取「会不会粘住词」，
// 不取「是不是中文」——中文与标点之间本来就不需要空格。
func joinWrapped(lines []string) string {
	var b strings.Builder
	for n, line := range lines {
		if n > 0 && needsSpace(lastRune(lines[n-1]), firstRune(line)) {
			b.WriteByte(' ')
		}
		b.WriteString(line)
	}
	return b.String()
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	var last rune
	for _, r := range s {
		last = r
	}
	return last
}

// needsSpace 判两侧要不要插一个空格：两侧都是 ASCII 词字符时才插。
//
// `)` `]` `}` 也算词的收尾（`…（见 ps)` 后面接一个英文词时要空格），但开括号不算
// 词首——`(ps` 这种写法左侧本来就不该有空格。
func needsSpace(prev, next rune) bool {
	return isWordEnd(prev) && isASCIIAlnum(next)
}

func isWordEnd(r rune) bool {
	return isASCIIAlnum(r) || r == ')' || r == ']' || r == '}'
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// ── roff ─────────────────────────────────────────────────────────────────

// roffText 把一段行内文本翻成 roff。
//
// 三件事：
//
//   - **行内标记**：反引号包住的 `x` 与 `**x**` 都写成粗体。两者在 roff 里没有各自的对应物，
//     而粗体是约定俗成的「这是代码/这是强调」。
//   - **转义**：反斜杠是 roff 的转义入口；连字符要写成 `\-`，否则渲染成排版用的
//     短横（`--format` 会印成 `–format`）。
//   - **CJK 断点**：roff 只在空格处断行，而中文句子里没有空格，整段会溢出页宽。
//     `\:` 是一个零宽的断点，插在相邻的两个 CJK 字符之间（插哪儿见 breakable），
//     让它能断在那里。
//
// 链接写成「文字（目标）」，目标与文字相同时只留文字：手页在无网机器上翻，而
// 源文件里的目标多半是仓库内路径，印出来至少能让人知道去哪儿找。
func roffText(s string) string {
	var b strings.Builder
	var prev rune
	for _, r := range s {
		if breakable(prev, r) {
			b.WriteString("\\:")
		}
		b.WriteString(escapeRoffRune(r))
		prev = r
	}
	return b.String()
}

// breakable 判两个相邻字符之间要不要插一个断点。
//
// 两侧都是 CJK 才插（英文靠空格断，标点之间不该断），而且**避开行首标点**：
// 中文排版里句号、逗号、右括号这些不能落在下一行的行首（「完整说明」换行、
// 下一行只有一个「。」），左括号之类不能落在上一行的行尾。这两条是中文排版的
// 通行规则，也是 `\:` 想插哪儿就插哪儿时唯一会把页面弄难看的地方。
func breakable(prev, next rune) bool {
	if !isCJK(prev) || !isCJK(next) {
		return false
	}
	return !isClosingPunct(next) && !isOpeningPunct(prev)
}

// isClosingPunct 判一个字符是不是**不能出现在行首**的收尾标点。
func isClosingPunct(r rune) bool {
	return strings.ContainsRune("。、，．；：？！）］｝」』〉》】〕｠・", r)
}

// isOpeningPunct 判一个字符是不是**不能出现在行尾**的起始标点。
func isOpeningPunct(r rune) bool {
	return strings.ContainsRune("（［｛「『〈《【〔｟", r)
}

// escapeRoffRune 转义一个字符。
//
// `.` 与 `'` 只在**行首**有特殊含义，而这里不知道最终会不会落在行首（roff 会自己
// 折行），所以统一写成 `\&.`：`\&` 是零宽，行首之外没有影响。`&` 不用管——它裸着
// 就是字面量，写成 `\&` 反而变成零宽、把字符吃掉了。
func escapeRoffRune(r rune) string {
	switch r {
	case '\\':
		return "\\e"
	case '-':
		return "\\-"
	case '.', '\'':
		return "\\&" + string(r)
	default:
		return string(r)
	}
}

// roffInline 渲染一段行内标记，写出带字体的 roff。
//
// 顺序不能反：先认标记（它们靠反引号与 `*` 定位），再逐**字符**转义。逐字符而不是
// 逐字节：一个字节拿去当字符就是 `string(byte(0xE7))`，Go 会把它当成 U+00E7 重新
// 编码成两个字节——整段中文会变成乱码，而它仍然是一份合法的 UTF-8，页面上只是
// 内容是错的。标记那几个判断按字节比是对的：它们全是 ASCII，而 ASCII 字节不会
// 出现在某个多字节字符的中间（UTF-8 的性质）。
func roffInline(s string) string { return roffMarkup(s, true) }

// roffSpan 渲染一段**已经处在某个字体里**的文字：标记照认，但不再切字体。
//
// 源文件里套着写的标记是常事（`**` 那一句里就嵌着几个单反引号），而 roff 的字体
// 只有一层：里层再来一个 `\fR` 会把外层的粗体一起关掉，后面半句就没有强调。所以
// 里层只把标记去掉，文字交给外层的字体。
//
// 用在两处：`.TP` 条目的第一栏（外层字体由 `.B` 给），以及标记里套着的标记。
func roffSpan(s string) string { return roffMarkup(s, false) }

// roffMarkup 是上面两个的共同实现。
//
// wrapper 决定遇到标记时切不切字体：切的时候里层不再套一层（见 roffSpan），
// 不切的时候把标记去掉即可。
//
// **没被标记圈住的字符攒着一起交给 roffText**，不一个字一个字地转：CJK 断点是插在
// 相邻两个字符之间的，一个字一个字地转时那个函数每次只看到当前这一个字符，断点
// 永远插不进去——页面上表现为一整段中文溢出右边距，而且只在中文上出问题
// （groff 只在空格处折行，英文句子本来就有空格）。
func roffMarkup(s string, wrapper bool) string {
	var b, plain strings.Builder
	flush := func() {
		b.WriteString(roffText(plain.String()))
		plain.Reset()
	}
	for i := 0; i < len(s); {
		if inner, n, ok := markupSpan(s[i:]); ok {
			flush()
			if wrapper {
				b.WriteString("\\fB" + roffMarkup(inner, false) + "\\fR")
			} else {
				b.WriteString(roffMarkup(inner, false))
			}
			i += n
			continue
		}
		if text, target, n, ok := linkSpan(s[i:]); ok {
			flush()
			b.WriteString(roffMarkup(text, false))
			if text != target {
				b.WriteString(roffText("（" + target + "）"))
			}
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		plain.WriteString(string(r))
		i += size
	}
	flush()
	return b.String()
}

// markupSpan 认一段行内标记（`**强调**` 或反引号包住的 `代码`），返回里面的文字与
// 吃掉的字节数。认不出时 ok 为 false，调用方按普通字符处理。
func markupSpan(s string) (inner string, n int, ok bool) {
	if strings.HasPrefix(s, "**") {
		if end := strings.Index(s[2:], "**"); end >= 0 {
			return s[2 : 2+end], 2 + end + 2, true
		}
		return "", 0, false
	}
	if strings.HasPrefix(s, "`") {
		if end := strings.IndexByte(s[1:], '`'); end >= 0 {
			return s[1 : 1+end], 1 + end + 1, true
		}
	}
	return "", 0, false
}

// linkSpan 认一条链接 `[文字](目标)`，返回文字、目标与吃掉的字节数。
//
// 链接不带字体：文字按正文字体写，目标跟在后面的括号里——手页在无网机器上翻，而
// 源文件里的目标多半是仓库内路径，印出来至少能让人知道去哪儿找。
func linkSpan(s string) (text, target string, n int, ok bool) {
	if !strings.HasPrefix(s, "[") {
		return "", "", 0, false
	}
	return parseLink(s)
}

// parseLink 认 `[文字](目标)`，返回文字、目标与吃掉的字节数。
func parseLink(s string) (text, target string, n int, ok bool) {
	close := strings.IndexByte(s, ']')
	if close < 0 || close+1 >= len(s) || s[close+1] != '(' {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[close+2:], ')')
	if end < 0 {
		return "", "", 0, false
	}
	return s[1:close], s[close+2 : close+2+end], close + 2 + end + 1, true
}

// isCJK 判一个字符是不是中日韩文字或全角标点。
//
// 只用来决定要不要插断点，所以范围取宽一点没有坏处：多插的断点只影响换行位置。
func isCJK(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // 部首、假名、CJK 统一表意文字
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 兼容表意文字
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // 全角形式与标点
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK 标点
		return true
	case r >= 0x20000 && r <= 0x2FA1F: // 扩展区
		return true
	}
	return false
}
