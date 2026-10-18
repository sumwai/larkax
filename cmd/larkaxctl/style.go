package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// 这一层是**面向终端的输出层**：样式（状态符、加粗、置灰）与几条固定的文案，
// 那些文案是「同一类命令说出同一句话」的落点。机器可读那一档在 format.go。
//
// 它是**自研的薄样式层**，不是 github.com/charmbracelet/lipgloss。理由两条：
//
//  1. 本包真正需要它的只有两件事：终端显示宽度对齐与 ANSI 控制码剥离。前者
//     table.go 的 displayWidth 已经有了（按东亚宽算，中英混排本来就是对的），
//     后者在这里就是「算宽度时跳过转义序列」那几行。lipgloss 的字宽能力与
//     它重复。
//  2. 本仓的直接依赖只有五个（见 go.mod）。为样式引入 lipgloss 会带进七个
//     间接模块，而换来的能力与上面那几十行重复。
//
// 同理不引 golang.org/x/term：判断标准输出是不是交互终端只需要
// os.Stdout.Stat() 的 os.ModeCharDevice，标准库足够。

// escByte 是转义序列的起始字节（ESC）。
//
// 输出里出现它只可能是一段 ANSI 控制码；按显示宽度算宽度时整段跳过
// （见 table.go 的 ansiSeqLen）。
const escByte = 0x1b

// SGR 参数。写成常量而不是在调用点内联拼 "\x1b[1m"：那种字符串拼错一个字节
// 不报任何错，只会让终端上多出一段乱码，而这里改一处就全都跟着改。
const (
	sgrReset  = "0"
	sgrBold   = "1"
	sgrDim    = "2"
	sgrRed    = "31"
	sgrGreen  = "32"
	sgrYellow = "33"
)

// ── 状态符 ──────────────────────────────────────────────────────────────

// glyphKind 是一个状态符表达的意思。
//
// 四档而不是两档：「跳过」（依赖没满足，这一项没查成）与「没跑/不适用」
// （这一项本来就不在这次范围内）是两件事，用同一个符号表达会让一次什么都
// 没查的运行看起来像一次查过且没问题的运行——这正是本仓反复踩的那个坑
// （见 LESSONS.md 第 16 条）。
type glyphKind int

const (
	glyphPass glyphKind = iota // 通过
	glyphFail                  // 失败
	glyphSkip                  // 跳过：依赖没满足
	glyphNone                  // 没跑 / 不适用
)

// symbol 返回这一档的状态符。
func (g glyphKind) symbol() string {
	switch g {
	case glyphPass:
		return "✓"
	case glyphFail:
		return "✗"
	case glyphSkip:
		return "!"
	default:
		return "-"
	}
}

// ── 着色开关 ────────────────────────────────────────────────────────────

// terminalProbe 判断一个写入目标是不是交互终端。
//
// 是变量而不是直接调 os.Stdout.Stat()：用例要能注入一个「永远是终端」的
// 假实现来覆盖上色那一支，而真实的 TTY 在 `go test` 里拿不到。
//
// 真实实现看的是 **w 自己**，不是 os.Stdout：输出写到哪就按哪判断，
// 于是「命令写文件、日志写终端」这类调用点也不会错判。
var terminalProbe = func(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// newPalette 决定这一次输出要不要上色。
//
// 三个条件里任何一个成立就**不上色**：
//
//   - 输出目标不是字符设备：重定向到文件、接进管道（`| grep`）、被别的
//     程序读走。这一条是重定向输出仍然可解析的前提。
//   - NO_COLOR 非空。规范（https://no-color.org）说的是「只要设了、且不是
//     空串」就尊重它，所以这里判的是非空而不是某个具体的值。
//   - TERM=dumb。
//
// 不上色时输出里**一个转义序列都没有**，而不只是「颜色变成透明」——这两者
// 在终端上看着一样，在 `| grep` 与文件里差很远。
func newPalette(w io.Writer) palette {
	if !terminalProbe(w) {
		return palette{}
	}
	if os.Getenv("NO_COLOR") != "" {
		return palette{}
	}
	if os.Getenv("TERM") == "dumb" {
		return palette{}
	}
	return palette{on: true}
}

// palette 是这一次输出的着色开关，外加几个具名样式。
//
// 零值表示不上色，每一个方法都退化成「原样返回」，所以调用点不必自己判。
type palette struct{ on bool }

// paint 给 s 套上一段 SGR 参数。
//
// 空串原样返回：给空串套两个转义序列会在输出里留下一段看不见的东西——
// 它在终端上零宽、在对齐里零宽，却在 `grep` 与文件里实实在在存在。
func (p palette) paint(params, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + params + "m" + s + "\x1b[" + sgrReset + "m"
}

// dim 把辅助描述（说明、版本号、时间戳）置灰。
func (p palette) dim(s string) string { return p.paint(sgrDim, s) }

// bold 突出关键实体：表头、主键、出错的那个对象。
func (p palette) bold(s string) string { return p.paint(sgrBold, s) }

// glyph 返回状态符，并按那一档上色。
//
// 失败那一档用「加粗 + 红」：只加粗在浅色主题下与普通文字分不开，只上色在
// 被截图或转到纯文本时又全丢了。
func (p palette) glyph(g glyphKind) string {
	switch g {
	case glyphPass:
		return p.paint(sgrGreen, g.symbol())
	case glyphFail:
		return p.paint(sgrBold+";"+sgrRed, g.symbol())
	case glyphSkip:
		return p.paint(sgrYellow, g.symbol())
	default:
		return p.paint(sgrDim, g.symbol())
	}
}

// conclusion 按结论的档位处理那一行结论本身。
//
// 通过项与跳过项整体置灰：一屏里最重要的信息是「哪几项不正常」，而它们不是。
func (p palette) conclusion(g glyphKind, s string) string {
	if g == glyphFail {
		return p.bold(s)
	}
	return p.dim(s)
}

// title 是那一行的标题。
//
// 失败项加粗：它是这次运行里要看的对象。其余置灰，与结论一起退到背景里。
func (p palette) title(g glyphKind, s string) string {
	if g == glyphFail {
		return p.bold(s)
	}
	return p.dim(s)
}

// ── 按显示宽度补空格 ────────────────────────────────────────────────────

// padRight 把 s 补到 width 列。
//
// 补的是**显示宽度**（displayWidth 会跳过样式码，见 table.go），所以
// 先上色再补空格与先补空格再上色结果一样——调用点不必挑顺序。
func padRight(s string, width int) string {
	if gap := width - displayWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// ── 帮助文本上色 ────────────────────────────────────────────────────────

// paintHelp 给一份帮助文本上色：只加样式码，一个字符都不增删。
//
// # 为什么是「逐行插样式码」而不是「渲染时拼」
//
// 帮助文本是四十来个手写的常量（`*Usage`），里面没有结构信息可用。所以判据
// 一律取**形状**，而形状判对了也只做一件事：在原本那几段的**外面**套上样式码。
// 于是「把样式码剥掉之后与不上色那一份逐字相同」是构造上成立的，不是拼出来的
// ——那一列也就不会因为上色而错位。
//
// # 三条形状判据
//
//   - **段标题与组标题**：顶格、以「：」结尾，且它后面第一个非空行是缩进的。
//     后半条不能省：说明文字里也有以冒号结尾的长句（「文件是一个 JSON 对象，
//     只认下面这些字段（键名写错会直接报错）：」），只按冒号结尾认会把一整句
//     话突出来；加上后半条就分得开——那一句后面是顶格的示例，不是缩进块。
//     组标题（`核心与诊断 (Core & Diagnostics)：`）由同一条判据覆盖，不必再
//     查注册表：它引出的正是紧跟着的那串命令。
//   - **两栏行**：缩进的行里，两个及以上空格分出的两栏是「名字栏 + 说明栏」。
//     两个而不是一个：名字栏自己含单空格（`larkaxctl migrate down-to <版本>`），
//     按一个空格切会把命令名切开。名字栏加粗、说明栏置灰。
//   - **其余**：顶格的是说明文字与指针行，整行置灰；缩进而以 `larkaxctl` 开头的
//     是用法那一栏（这一行没有说明），整行加粗；再其余（说明的续行、换行下来的
//     选项、示例里那段 JSON）原样不动——它们已经是背景了，置灰反而会把示例里的
//     花括号也一起调暗。
//
// 已知落在判据之外的一处：`--credential-env <变量名> 从名为 <变量名> 的环境变量
// 读` 那一行与它上面那一栏之间只隔**一个**空格（名字栏比上一行长），于是它按
// 「其余」处理、不加粗。它没有被上错色，只是没上色；要让它也成一行，得把那一栏
// 的空格补齐——那是改帮助文本的排版，与这一条不是一件事，没有混在一起做。
func (p palette) paintHelp(text string) string {
	if !p.on {
		// 不上色那一档逐字返回原文：它是「重定向、NO_COLOR、TERM=dumb、
		// --format json」四条判据共同的结果（见 newPalette 与 printUsage）。
		return text
	}

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = p.paintHelpLine(line, nextNonBlankIsIndented(lines, i))
	}
	return strings.Join(lines, "\n")
}

// paintHelpLine 上色一行。nextIndented 是「它后面第一个非空行缩进吗」，
// 只有段标题那一条判据用得到它。
func (p palette) paintHelpLine(line string, nextIndented bool) string {
	indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
	body := line[len(indent):]
	if body == "" {
		return line
	}

	if indent == "" {
		if strings.HasSuffix(body, "：") && nextIndented {
			return p.bold(body)
		}
		if head, rest, ok := helpCommandPrefix(body); ok {
			return p.bold(head) + p.dim(rest)
		}
		return p.dim(body)
	}

	if name, desc, col, ok := splitHelpRow(body); ok {
		// 说明栏补到原文里那一列。补空格走 padRight（按 displayWidth 算）而不是
		// 照抄原文那几个空格：名字栏里若含宽字符，字节数与列数不是一回事，
		// 按字节补会把说明栏往后推。
		return indent + padRight(p.bold(name), col) + p.dim(desc)
	}
	if helpInvocationRow(body) {
		return indent + p.bold(body)
	}
	return line
}

// nextNonBlankIsIndented 报告第 i 行之后第一个非空行是不是缩进的。
func nextNonBlankIsIndented(lines []string, i int) bool {
	for _, line := range lines[i+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return strings.HasPrefix(line, " ")
	}
	return false
}

// helpCommandPrefix 把「larkaxctl <命令>」那一段从一行里切出来。
//
// 认的是**命令名的形状**（小写字母开头，其余是小写字母、数字、连字符），不是
// 注册表：帮助的打印路径挂在 `commandGroups` 上（每条命令的 run 都在那里），
// 而注册表又要用到那几十条命令的处理函数——从注册表那一头查名字，会让命令组
// 的初始化反过来依赖 printUsage。**Go 的初始化环分析是穿过函数体的**，查一下
// 就编译不过。
//
// 形状判据在这里够用，而且落在同一批行上：整行以 `larkaxctl` 开头的本来就是
// 每份帮助的首行，以及说明里插的那句 `larkaxctl price calendar import <文件>`；
// `larkaxctl 是 larkax 的运维命令行。` 里的下一个词是中文，认不出来，整行置灰。
func helpCommandPrefix(body string) (head, rest string, ok bool) {
	after, found := strings.CutPrefix(body, "larkaxctl ")
	if !found {
		return "", "", false
	}
	name := after
	if i := strings.IndexByte(after, ' '); i >= 0 {
		name = after[:i]
	}
	if !isCommandNameShape(name) {
		return "", "", false
	}
	return "larkaxctl " + name, after[len(name):], true
}

// isCommandNameShape 报告一个词是不是命令名的形状：小写字母开头，后面只跟
// 小写字母、数字与连字符。
func isCommandNameShape(word string) bool {
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		case i > 0 && c == '-':
		default:
			return false
		}
	}
	return word != ""
}

// splitHelpRow 把一行缩进的两栏切成名字栏与说明栏，col 是说明栏的起点列。
//
// col 取的是原文里名字栏与那一串空格的**显示宽度**：它就是要补到的列，
// 调用点拿它交给 padRight。
func splitHelpRow(body string) (name, desc string, col int, ok bool) {
	// 从 1 起：body 已经去掉行首缩进，第 0 列不可能是栏间空格。
	for i := 1; i+1 < len(body); i++ {
		if body[i] != ' ' || body[i+1] != ' ' {
			continue
		}
		j := i
		for j < len(body) && body[j] == ' ' {
			j++
		}
		if j == len(body) {
			// 行尾的一串空格后面没有说明栏，这一行不是两栏行。
			return "", "", 0, false
		}
		return body[:i], body[j:], displayWidth(body[:j]), true
	}
	return "", "", 0, false
}

// helpInvocationRow 报告一行缩进的一栏文字是不是一条用法行（这一行没有说明）。
//
// 只有 `larkaxctl` 开头的那一种：用法行里没有说明的（`larkaxctl alias delete
// --id <别名 ID>`）与多行用法的续行各是一行，它们都是**命令名**，加粗。
//
// 按开头那一个字符认，`-`、`[`、`(` 开头的**不**算：说明的续行里也会在换行处
// 以一个选项名起头（doctor 的帮助里就有一句 `--env-file 要的是 …`），加粗它
// 会让半句话从一段置灰的说明里跳出来。换行下来的选项保持原样，宁可少加粗一处。
func helpInvocationRow(body string) bool {
	return strings.HasPrefix(body, "larkaxctl ")
}

// ── 诊断体检类命令的单行状态 ────────────────────────────────────────────

// statusLine 是诊断体检类命令默认输出里的一行：状态符、标题、一句结论，
// 外加 -v 才打的细节。
//
// 先收成切片再打，是因为标题那一栏要按**最宽的那个**补空格，而宽度得等所有行
// 到齐才知道（与 tableWriter 同一个理由，见 table.go）。
//
// 这一组命令（doctor、partition check、ledger verify、secret rotate --verify）
// 共用这一条渲染路径，不是各写一遍：共用的地方才数得出来。调用点登记在
// renderpaths_test.go 的 TestEveryStatusRenderPathIsRegistered 里，多一个
// 没登记的、少一个登记过的都会红。
type statusLine struct {
	kind       glyphKind
	title      string
	conclusion string
	detail     []string
}

// printStatusLines 打一组状态行：`状态符 标题 结论`，标题那一栏按最宽的对齐，
// verbose 时每行后面缩进跟它的细节。
//
// 收敛掉的不是信息量，是**默认那一屏**：一次全通过的运行原先要刷三十多行，
// 而其中没有一行需要动作。原来的长句一条都没删，只是挪到了 -v。
func printStatusLines(w io.Writer, lines []statusLine, verbose bool) {
	p := newPalette(w)
	width := 0
	for _, l := range lines {
		if n := displayWidth(l.title); n > width {
			width = n
		}
	}
	for _, l := range lines {
		_, _ = fmt.Fprintf(w, "  %s %s  %s\n",
			p.glyph(l.kind), p.title(l.kind, padRight(l.title, width)),
			p.conclusion(l.kind, l.conclusion))
		if !verbose {
			continue
		}
		for _, line := range l.detail {
			_, _ = fmt.Fprintf(w, "      %s\n", p.dim(line))
		}
	}
}

// countStatusLines 数一组状态行里各档各有多少项。
//
// 从**打出去的那些行**数，而不是从上游的数据结构数：汇总行说的是「上面这一屏
// 里有几项」，两处各数一遍时，多打或少打一行都会让那句汇总说谎。
func countStatusLines(lines []statusLine) (passed, failed, skipped int) {
	for _, l := range lines {
		switch l.kind {
		case glyphPass:
			passed++
		case glyphFail:
			failed++
		case glyphSkip:
			skipped++
		}
	}
	return passed, failed, skipped
}

// ── 清单空状态的文案 ────────────────────────────────────────────────────

// emptyListNoteFmt 是所有清单空状态共用的模板。
//
// 「暂无<实体>。可使用 larkaxctl <子命令> 添加。」这一句把两件事说在一起：
// 这一栏现在是空的，以及下一步该敲哪条命令。拆成两句会让人在两个地方找那个
// 命令名，而「还没有…」与「没有…」两套说法并存时，读的人分不清它们是同一件事
// 还是两个不同的状态（见 docs/cli-output.md 的「加一个清单命令」）。
//
// 它是包内**唯一**出现「暂无」的地方，且由 renderpaths_test.go 按字面量计数
// 钉住：新加一条清单命令、空状态自己另写一句，那道门会红。
const emptyListNoteFmt = "暂无%s。可使用 larkaxctl %s 添加。"

// emptyListNote 返回一句清单空状态。
//
// entity 是实体名（「API Key」「渠道」），action 是添加它的子命令
// （「apikey create」）。没有「添加」这个动作的清单不走这里：这一句的后半截
// 是它的一半，给不出可执行的下一步时不该硬造一个。
//
// 实体名以 ASCII 字符开头时前面补一个空格（「暂无 API Key」而不是
// 「暂无API Key」）：中文与西文之间留一个空格是本仓一贯的写法，而这一点由这里
// 统一，调用点只写实体名本身。
//
// 补空格只能按 entity 的**首字节**判（`entity[0] < utf8.RuneSelf` 就是 ASCII），
// 本包这几条实体名都是纯 ASCII 或纯中文，没有混排的首字符边界要处理。
func emptyListNote(entity, action string) string {
	if entity != "" && entity[0] < utf8.RuneSelf {
		entity = " " + entity
	}
	return fmt.Sprintf(emptyListNoteFmt, entity, action)
}

// longStampLayout 是清单里时刻那一栏的**带年份**写法。
//
// 与 stampLayout（apikey.go，`01-02 15:04`）差一个年份。清单里默认省掉年份是
// 为了不把一栏撑宽（见 docs/cli-output.md 的「加一个清单命令」），但那几类数据
// 的跨度以天计；用量事件按月分区、对账运行记录长期留存，省掉年份之后
// 「10-31 23:00」是哪一年要靠上下文猜。
const longStampLayout = "2006-01-02 15:04"

// truncatedListNote 是清单被 --limit 截住时补的那一句。
//
// 理由与空状态那一句同源：一份**少了后文**的清单与一份完整的清单在屏幕上长得
// 一样，而看的人会照第一份下结论（见 LESSONS.md 第 16 条）。所以被截住时要在
// 同一份输出里说出来，并且给出两条能拿到后文的下一步——它收在 style.go 而不是
// 各命令自己拼，与 emptyListNote 是同一条：同类清单说同一句话。
func truncatedListNote(shown int) string {
	return fmt.Sprintf(
		"（以上是最近 %d 条，更早的没打出来：把 --limit 调大，或用筛选条件缩小范围）", shown)
}
