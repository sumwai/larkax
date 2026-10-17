package main

import (
	"io"
	"os"
	"strings"
)

// 这一层是**自研的薄样式层**，不是 github.com/charmbracelet/lipgloss。理由两条：
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
