// Package manpage 把命令注册表与 docs/cli-reference.md 渲染成 roff 手页。
//
// # 单源
//
// 名字、分组与一句话来自命令注册表（cmd/larkaxctl 的 commandGroups），长文本来自
// docs/cli-reference.md。两边不各抄一份：改了注册表而没同步手页时，
// scripts/check-man-pages.sh 会红——没有那道门，手页会在第一次改动之后就开始撒谎。
//
// 生成器本身只认这两份输入，不知道仓库长什么样：输入是文本，输出是文件内容，
// 路径由调用方给（见 cmd/larkaxctl 的 man_test.go）。于是它可以用夹具测透，
// 不必先起一个仓库。
//
// # 只用标准库
//
// 渲染这件事不值得引一个依赖：本仓的直接依赖本来就少（见 go.mod），而这里要的
// roff 是一小片宏，花在认识它上面的时间远少于多一个依赖要付的代价。
//
// # 标题文字取自源文件
//
// 首页那几个分节的标题（全局约定、按命令、搬过家的命令）是从源文件读回来的，
// 不是在这里另写一遍：另写一遍的话，源文件里改了标题，手页上还是旧的那一个，
// 而两处都是中文短句，看的人不会觉得哪里不对。
//
// # 不认识的结构要报错
//
// 见 document.go 的开头。少一段文字与「本来就没写」在产物里长得一样。
package manpage

import (
	"fmt"
	"strings"
)

// Command 是一条一级命令：名字与根帮助页上那一句话。
type Command struct {
	Name    string
	Summary string
}

// Group 是根帮助页上的一个分组。顺序就是根帮助页上的顺序。
type Group struct {
	Title    string
	Commands []Command
}

// File 是一个产物：文件名（不含目录）与内容。
type File struct {
	Name    string
	Content []byte
}

// IndexName 是手册首页的文件名，其余页是 `larkaxctl-<命令>.1`。
const IndexName = "larkaxctl.1"

// Legacy 是一条搬过家的命令：旧路径与新路径。
type Legacy struct {
	Old string
	New string
}

// Reference 是 docs/cli-reference.md 的结构化结果。
//
// 字段名与源文件的层级一一对应：Preamble 是一级标题之后那几段，Global 是
// 「全局约定」那一节，Commands 那个前缀是「按命令」，Legacy 是「搬过家的命令」。
// 三节的标题一并收着（GlobalTitle 这几个），渲染时用它们，不另写一遍
// （见包注释「标题文字取自源文件」）。
type Reference struct {
	// Title 是一级标题。
	Title string
	// Preamble 是标题之后、「全局约定」之前的块。
	Preamble []Block
	// GlobalTitle 是「全局约定」那一节的标题，Global 是它的块（含它的小节）。
	GlobalTitle string
	Global      []Block
	// CommandsTitle 是「按命令」那一节的标题，CommandsIntro 是它到第一个分组
	// 之间的块。
	CommandsTitle string
	CommandsIntro []Block
	// GroupTitles 是各分组的标题，按出现顺序。
	//
	// 它与注册表的分组顺序要一致（见 Render）：两处不一致时首页会把命令列在
	// 错的分组下，而那看起来只是排版不同。
	GroupTitles []string
	// GroupIntros 是各分组标题之后、第一条命令之前的块（今天一条都没有）。
	GroupIntros map[string][]Block
	// Sections 是各命令那一节的正文，键是标题里那一个名字。
	//
	// 里面既有 `doctor` 这样的一级命令，也有 `price calendar` 这样的二级动作
	// （见 splitSection）。
	Sections map[string][]Block
	// LegacyTitle 是「搬过家的命令」那一节的标题，LegacyIntro 是它的块
	// （含那张表），Legacy 是那张表解析出来的行。
	LegacyTitle string
	LegacyIntro []Block
	Legacy      []Legacy
}

// 解析器当时走到源文件的哪一节。同一个块落在哪一节，全看它在源文件里的位置：
// 段落、列表与表格在三节里的读法都一样，区别只在收进哪一个字段。
const (
	modeStart = iota // 一级标题之前
	modePreamble
	modeGlobal
	modeCommands
	modeGroup
	modeSection
	modeLegacy
)

// ParseReference 读 docs/cli-reference.md。
//
// 认得的顶层分节只有三个：全局约定、按命令、搬过家的命令。多出一个就报错——
// 不报的话，新加的那一节会从手页上消失，而删除与「本来就没写」长得一样。
func ParseReference(md string) (*Reference, error) {
	blocks, err := parseBlocks(md)
	if err != nil {
		return nil, err
	}

	ref := &Reference{
		GroupIntros: map[string][]Block{},
		Sections:    map[string][]Block{},
	}
	mode := modeStart
	group := ""
	section := ""

	for _, blk := range blocks {
		if h, ok := blk.(Heading); ok {
			switch h.Level {
			case 1:
				if ref.Title != "" {
					return nil, fmt.Errorf("有两个一级标题：%q 与 %q", ref.Title, h.Text)
				}
				if mode != modeStart {
					return nil, fmt.Errorf("一级标题 %q 出现在正文之后", h.Text)
				}
				ref.Title = strings.TrimSpace(h.Text)
				mode = modePreamble
				continue
			case 2:
				title := strings.TrimSpace(h.Text)
				switch title {
				case "全局约定":
					ref.GlobalTitle = title
					mode = modeGlobal
				case "按命令":
					ref.CommandsTitle = title
					mode = modeCommands
				case "搬过家的命令":
					ref.LegacyTitle = title
					mode = modeLegacy
				default:
					return nil, fmt.Errorf("不认识的顶层分节 %q；"+
						"它要么并进现有的一节，要么在这里教生成器怎么渲染", title)
				}
				continue
			case 3:
				// 三级标题在「按命令」下面是分组，在「全局约定」里是小节——
				// 两者在源文件里长得一样，靠位置区分。
				switch mode {
				case modeCommands, modeGroup, modeSection:
					group = strings.TrimSpace(h.Text)
					ref.GroupTitles = append(ref.GroupTitles, group)
					ref.GroupIntros[group] = nil
					mode = modeGroup
				case modeGlobal, modePreamble:
					if err := appendBlock(ref, mode, group, section, blk); err != nil {
						return nil, err
					}
				default:
					return nil, fmt.Errorf("三级标题 %q 只许出现在「全局约定」"+
						"（那里是小节）或「按命令」（那里是分组）里面", h.Text)
				}
				continue
			case 4:
				if mode != modeGroup && mode != modeSection {
					return nil, fmt.Errorf("命令小节 %q 只许出现在某个分组下面", h.Text)
				}
				name := strings.TrimSpace(h.Text)
				if !strings.HasPrefix(name, "`") || !strings.HasSuffix(name, "`") || len(name) < 3 {
					return nil, fmt.Errorf("命令小节要写成 `#### `命令名`` 的形式，实际是 %q", h.Text)
				}
				section = strings.Trim(name, "`")
				if _, dup := ref.Sections[section]; dup {
					return nil, fmt.Errorf("命令小节 %q 出现了两次", section)
				}
				ref.Sections[section] = nil
				mode = modeSection
				continue
			}
			return nil, fmt.Errorf("标题层级超过 4 级：%q", h.Text)
		}

		if err := appendBlock(ref, mode, group, section, blk); err != nil {
			return nil, err
		}
	}

	if ref.Title == "" {
		return nil, fmt.Errorf("没有一级标题")
	}
	if len(ref.Preamble) == 0 {
		return nil, fmt.Errorf("标题与「全局约定」之间一段话都没有")
	}
	if len(ref.Global) == 0 {
		return nil, fmt.Errorf("「全局约定」一节是空的")
	}
	if len(ref.GroupTitles) == 0 {
		return nil, fmt.Errorf("「按命令」下面一个分组都没有")
	}
	if len(ref.Sections) == 0 {
		return nil, fmt.Errorf("一条命令的说明都没有")
	}
	if len(ref.Legacy) == 0 {
		return nil, fmt.Errorf("「搬过家的命令」一节里没有那张表")
	}
	return ref, nil
}

// appendBlock 把一个块收进当前那一节。
//
// 表在「搬过家的命令」那一节里多一件事：除了原样收着，还要解析成行——渲染首页
// 时用的是原文，跨页引用那几页用的是解析出来的行。
func appendBlock(ref *Reference, mode int, group, section string, blk Block) error {
	switch mode {
	case modeStart:
		return fmt.Errorf("一级标题之前就有正文：%s", summaryOf(blk))
	case modePreamble:
		ref.Preamble = append(ref.Preamble, blk)
	case modeGlobal:
		ref.Global = append(ref.Global, blk)
	case modeCommands:
		ref.CommandsIntro = append(ref.CommandsIntro, blk)
	case modeGroup:
		ref.GroupIntros[group] = append(ref.GroupIntros[group], blk)
	case modeSection:
		ref.Sections[section] = append(ref.Sections[section], blk)
	case modeLegacy:
		ref.LegacyIntro = append(ref.LegacyIntro, blk)
		if table, ok := blk.(Table); ok {
			rows, err := legacyRows(table)
			if err != nil {
				return err
			}
			ref.Legacy = rows
		}
	}
	return nil
}

// legacyRows 把那张「旧路径 / 新路径」表读成行。
func legacyRows(table Table) ([]Legacy, error) {
	if len(table.Head) != 2 {
		return nil, fmt.Errorf("搬过家的命令那张表有 %d 栏，期望 2 栏", len(table.Head))
	}
	var out []Legacy
	for _, row := range table.Rows {
		out = append(out, Legacy{
			Old: strings.Trim(row[0], "`"),
			New: strings.Trim(row[1], "`"),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("搬过家的命令那张表一行都没有")
	}
	return out, nil
}

// SectionCommand 是一个节点名所属的那条命令：`price calendar` 属于 `price`。
//
// 对外只有这一半有用（「这一节该落在哪一页上」）；拆出来的那半截动作名在包内用。
// 调用方另有 cmd/larkaxctl 的 man_test.go——它拿这个把源文件里那张「旧路径 /
// 新路径」表与代码里的 legacyCommands / 注册表对上。
func SectionCommand(name string) string {
	cmd, _ := splitSection(name)
	return cmd
}

// splitSection 把一节的名字拆成「一级命令」与「二级动作」。
//
// `price calendar` 这种节讲的是 `price` 的某个二级动作，所以它落在 `price` 那一页
// 上，作为那一节下面的一个小标题。拆不出来时返回整个名字与空动作。
func splitSection(name string) (command, action string) {
	if i := strings.IndexByte(name, ' '); i > 0 {
		return name[:i], strings.TrimSpace(name[i+1:])
	}
	return name, ""
}

// summaryOf 给报错信息用：块的头部一小段。
//
// 切的是**字符**而不是字节：这一段多半是中文，逐字节切会把最后一个字切成半个，
// 于是报错文本自己不是合法 UTF-8，终端上打出来是一串乱码——要给人指路的那句话
// 反倒成了最难读的一句（同一个坑，见 document.go 里 roffInline 的那段注释）。
// 四十这个数取的是字符数，中英文都读得过去。
func summaryOf(blk Block) string {
	var s string
	switch t := blk.(type) {
	case Paragraph:
		s = t.Text
	case List:
		s = strings.Join(t.Items, "；")
	case Table:
		s = strings.Join(t.Head, " | ")
	case Heading:
		s = t.Text
	}
	if r := []rune(s); len(r) > 40 {
		s = string(r[:40]) + "…"
	}
	return s
}

// ── 渲染 ─────────────────────────────────────────────────────────────────

// Render 产出首页与每条一级命令各一页。
//
// 顺序：首页在最前，其余按注册表的顺序——产物是提交进库的文件，顺序不定的话
// 每一次生成的目录内容一致而列出来的顺序不同，比对起来会多出无谓的噪声。
func Render(ref *Reference, groups []Group) ([]File, error) {
	if err := checkAgainst(ref, groups); err != nil {
		return nil, err
	}

	index, err := renderIndex(ref, groups)
	if err != nil {
		return nil, err
	}
	files := []File{{Name: IndexName, Content: index}}

	for _, g := range groups {
		for _, c := range g.Commands {
			page, err := renderCommand(ref, c)
			if err != nil {
				return nil, err
			}
			files = append(files, File{Name: commandPageName(c.Name), Content: page})
		}
	}
	return files, nil
}

// commandPageName 是一条命令那一页的文件名。
func commandPageName(name string) string { return "larkaxctl-" + name + ".1" }

// checkAgainst 核对源文件与注册表对不对得上。
//
// 这几条是这道生成器的全部价值所在：对不上的时候**报错**，而不是渲染出一页
// 少了半截内容的手册——后者在页面上看不出来，只在有人照着它敲命令时才暴露。
func checkAgainst(ref *Reference, groups []Group) error {
	var titles []string
	known := map[string]bool{}
	for _, g := range groups {
		if g.Title == "" {
			return fmt.Errorf("注册表里有一个分组没有标题")
		}
		titles = append(titles, g.Title)
		if _, ok := ref.GroupIntros[g.Title]; !ok {
			return fmt.Errorf("分组 %q 不在 docs/cli-reference.md 里", g.Title)
		}
		for _, c := range g.Commands {
			if known[c.Name] {
				return fmt.Errorf("注册表里命令 %q 出现了两次", c.Name)
			}
			known[c.Name] = true
		}
	}
	if strings.Join(titles, "\n") != strings.Join(ref.GroupTitles, "\n") {
		return fmt.Errorf("分组对不上：\n  注册表：%s\n  cli-reference.md：%s",
			strings.Join(titles, " / "), strings.Join(ref.GroupTitles, " / "))
	}

	for name := range ref.Sections {
		cmd, _ := splitSection(name)
		if !known[cmd] {
			return fmt.Errorf("cli-reference.md 里的 %q 没有对应的命令；"+
				"要么改小节标题，要么把它并进某条命令那一节", name)
		}
	}
	for name := range known {
		if _, ok := ref.Sections[name]; !ok {
			return fmt.Errorf("命令 %q 在 cli-reference.md 里没有小节；"+
				"每一条命令都要有它自己的长说明", name)
		}
	}
	return nil
}

// renderIndex 产出首页。
func renderIndex(ref *Reference, groups []Group) ([]byte, error) {
	var b strings.Builder
	header(&b, "LARKAXCTL", "larkaxctl")

	b.WriteString(".SH NAME\n")
	b.WriteString(roffText("larkaxctl") + " \\- " + roffText(oneLineAbout(ref)) + "\n")

	b.WriteString(".SH SYNOPSIS\n")
	b.WriteString(".B larkaxctl\n.I " + roffText("命令") + "\n.RI [ " + roffText("选项") + " ]\n")
	b.WriteString(".br\n")
	b.WriteString(roffInline("每条命令的二级动作与参数见 `larkaxctl <命令> --help`。") + "\n")

	// 标题之后那几段讲的是这份手册与这个工具是什么，在 roff 里归 DESCRIPTION
	// 那一节（源文件里它们没有自己的标题）。
	if len(ref.Preamble) > 0 {
		b.WriteString(".SH DESCRIPTION\n")
		seen := map[string]bool{}
		for _, blk := range ref.Preamble {
			if err := writeBlock(&b, blk, seen); err != nil {
				return nil, err
			}
		}
	}

	seen := map[string]bool{}
	b.WriteString(".SH " + roffText(ref.GlobalTitle) + "\n")
	for _, blk := range ref.Global {
		if err := writeBlock(&b, blk, seen); err != nil {
			return nil, err
		}
	}

	// 「按命令」那一节：文档自己的前言照抄，分组与命令清单由注册表填。
	b.WriteString(".SH " + roffText(ref.CommandsTitle) + "\n")
	seen = map[string]bool{}
	for _, blk := range ref.CommandsIntro {
		if err := writeBlock(&b, blk, seen); err != nil {
			return nil, err
		}
	}
	for _, g := range groups {
		b.WriteString(".SS " + roffInline(g.Title) + "\n")
		for _, blk := range ref.GroupIntros[g.Title] {
			if err := writeBlock(&b, blk, seen); err != nil {
				return nil, err
			}
		}
		for _, c := range g.Commands {
			// `.br` 在名字后面那一行不是多余的：`.TP` 的条目在够短时会与正文
			// 挤在同一行（doctor 挤、settings 不挤），清单看上去就参差不齐。
			b.WriteString(".TP\n")
			b.WriteString(".B " + roffText(c.Name) + "\n")
			b.WriteString(".br\n")
			b.WriteString(roffInline(c.Summary) + "\n")
			b.WriteString(".br\n")
			b.WriteString(".BR " + roffText(manRefName(commandPageName(c.Name))) + " (1)\n")
		}
	}

	// 「搬过家的命令」那一节原样照抄（含那张表）。
	b.WriteString(".SH " + roffText(ref.LegacyTitle) + "\n")
	seen = map[string]bool{}
	for _, blk := range ref.LegacyIntro {
		if err := writeBlock(&b, blk, seen); err != nil {
			return nil, err
		}
	}

	b.WriteString(".SH SEE ALSO\n")
	for _, g := range groups {
		for _, c := range g.Commands {
			b.WriteString(".BR " + roffText(manRefName(commandPageName(c.Name))) + " (1),\n")
		}
	}
	b.WriteString(".BR " + roffText("larkaxctl") + " (1)\n")
	return []byte(b.String()), nil
}

// renderCommand 产出某一条一级命令那一页。
//
// 旧路径（reconcile、rotate 这些）**不在这里另起一节**：源文件每条命令那一节里
// 已经写了它自己那条旧路径，而且写得更准（`larkaxctl calendar import <文件>`）。
// 再生成一节「旧路径」的话，同一页上会有两句意思一样的话。
func renderCommand(ref *Reference, c Command) ([]byte, error) {
	var b strings.Builder
	header(&b, strings.ToUpper(manRefName(commandPageName(c.Name))), "larkaxctl-"+c.Name)

	b.WriteString(".SH NAME\n")
	b.WriteString(roffText("larkaxctl-"+c.Name) + " \\- " + roffInline(c.Summary) + "\n")

	b.WriteString(".SH SYNOPSIS\n")
	b.WriteString(".B larkaxctl " + roffText(c.Name) + "\n.RI [ " + roffText("选项") + " ]\n")
	b.WriteString(".br\n")
	b.WriteString(roffInline(fmt.Sprintf(
		"二级动作与参数见 `larkaxctl %s --help`（短版）与 `larkaxctl help %s`（长版）。",
		c.Name, c.Name)) + "\n")

	// 正文：命令自己那一节，随后是它的二级动作（`price calendar` 这类）。
	var actions []string
	for name := range ref.Sections {
		if cmd, action := splitSection(name); cmd == c.Name && action != "" {
			actions = append(actions, name)
		}
	}
	sortStrings(actions)

	// SYNOPSIS 那两行与正文之间要断开：不断的话正文紧接着「（长版）。」往下排，
	// 页面上分不出哪儿是用法、哪儿是说明。
	body := ref.Sections[c.Name]
	if len(body) > 0 {
		if _, ok := body[0].(Paragraph); ok {
			b.WriteString(".PP\n")
		}
	}

	seen := map[string]bool{}
	for _, blk := range body {
		if err := writeBlock(&b, blk, seen); err != nil {
			return nil, err
		}
	}
	for _, name := range actions {
		_, action := splitSection(name)
		b.WriteString(".SS " + roffInline(action) + "\n")
		seen = map[string]bool{}
		for _, blk := range ref.Sections[name] {
			if err := writeBlock(&b, blk, seen); err != nil {
				return nil, err
			}
		}
	}

	b.WriteString(".SH SEE ALSO\n")
	b.WriteString(".BR " + roffText("larkaxctl") + " (1)\n")
	return []byte(b.String()), nil
}

// header 写 roff 的文件头。
//
// **不写生成日期**：日期一变，每次生成的产物都与上一次不同，同步门就再也分不出
// 「内容变了」与「今天又生成了一次」。手页的版本信息由包本身带着，不在这一行。
//
// 后面两行关掉默认的排版行为，因为本页的正文是中文：
//
//   - `.ds AD l` + `.ad l` 左对齐。默认是两端对齐，而中文行里只有几个空格，
//     为了把行拉满，groff 会把那几个空格拉成一大片（页面上看到的是词与词之间
//     一条条空白，还夹着 `cannot adjust line` 的警告）。两句都要：`.ad` 管当下，
//     `AD` 是 man 宏自己的那个寄存器，每个段落开头它都会拿它再设一遍（an.tmac
//     的 `an-write-paragraph-tag`），只写 `.ad` 的话，`.TP` 与 `.IP` 那些块会
//     退回两端对齐。
//   - `.nh` 不自动断词。默认允许，而它按英文的规则断，`larkaxctl` 这种词
//     会被腰斩；中文的断点由 `\:` 给（见 document.go 的 breakable）。
func header(b *strings.Builder, title, manual string) {
	b.WriteString(".TH " + roffText(title) + " 1 \"\" \"larkax\" \"" + roffText(manual) + "\"\n")
	b.WriteString(".ds AD l\n")
	b.WriteString(".ad l\n")
	b.WriteString(".nh\n")
}

// manRefName 把产物的文件名翻成手册里引用它时用的名字（去掉 .1）。
func manRefName(file string) string { return strings.TrimSuffix(file, ".1") }

// writeBlock 写一个块。
//
// seen 记住「这一节里已经写过块了」，只为决定段落要不要补一个 `.PP`：紧跟在标题
// 后面的那一段不补，否则标题与正文之间会多空一行。
func writeBlock(b *strings.Builder, blk Block, seen map[string]bool) error {
	switch t := blk.(type) {
	case Heading:
		switch t.Level {
		case 2:
			b.WriteString(".SH " + roffInline(t.Text) + "\n")
		case 3:
			b.WriteString(".SS " + roffInline(t.Text) + "\n")
		default:
			return fmt.Errorf("正文里出现了 %d 级标题：%q", t.Level, t.Text)
		}
	case Paragraph:
		if seen["block"] {
			b.WriteString(".PP\n")
		}
		b.WriteString(roffInline(t.Text) + "\n")
	case List:
		for _, item := range t.Items {
			b.WriteString(".IP \\(bu 2\n")
			b.WriteString(roffInline(item) + "\n")
		}
	case Table:
		// 表格写成 `.TP` 列表，不写 `.TS`：后者要 tbl 预处理器，而 man-db 与
		// groff 是否替我们跑那一步取决于发行版的配置。两栏的表（这里都是这种）
		// 写成 `.TP` 与 man 自己的选项列表是同一种样子，而且不依赖任何前置步骤。
		//
		// 第一栏用 `.B` 加粗（`.TP` 的条目惯例如此），所以它走 roffSpan——那一栏
		// 里的标记只去掉、不再切一次字体。第二栏是正文字体，走 roffInline。
		for _, row := range t.Rows {
			b.WriteString(".TP\n")
			b.WriteString(".B " + roffSpan(row[0]) + "\n")
			b.WriteString(roffInline(strings.Join(row[1:], "  ")) + "\n")
		}
	default:
		return fmt.Errorf("不认识的块类型 %T", blk)
	}
	seen["block"] = true
	return nil
}

// oneLineAbout 是首页 NAME 那一行的那一句话。
//
// 它取自源文件第一段的第一句（那句话本来就是「这个工具是什么」），去掉开头那个
// 命令名与「是」：man 的 NAME 行本身就是「名字 \- 一句话」，名字已经在破折号前头
// 了，再来一遍是赘述。去掉之后剩下的仍是源文件里的字，不是在这里另写的
// （源文件改了那句话，这里跟着变）。认不出那个开头时原样用它，句子长一点，但不错。
func oneLineAbout(ref *Reference) string {
	sentence := firstSentence(firstParagraph(ref))
	name := manRefName(IndexName)
	for _, prefix := range []string{"`" + name + "` 是 ", name + " 是 "} {
		if strings.HasPrefix(sentence, prefix) {
			return strings.TrimSuffix(strings.TrimPrefix(sentence, prefix), "。")
		}
	}
	return strings.TrimSuffix(sentence, "。")
}

// firstParagraph 取源文件第一段；一段都没有时退回标题。
func firstParagraph(ref *Reference) string {
	for _, blk := range ref.Preamble {
		if p, ok := blk.(Paragraph); ok {
			return p.Text
		}
	}
	return ref.Title
}

// firstSentence 取一句话里的第一句（按中文句号切）。
func firstSentence(s string) string {
	if i := strings.Index(s, "。"); i >= 0 {
		return s[:i+len("。")]
	}
	return s
}

// sortStrings 是 sort.Strings 的本地写法，避免为一个调用引 sort。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
