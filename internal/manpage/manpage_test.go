package manpage

import (
	"strings"
	"testing"
)

// 本文件用手写的夹具测生成器：源文件是一小片 markdown 的子集，注册表是几行字。
// 真仓库的那两份输入另有用例（cmd/larkaxctl 的 man_test.go），这里判的是**规则**
// ——认哪种结构、不认哪种、每种结构渲染成什么。

// fixture 是一份最小的源文件：四节都在，两个分组、三条命令、一条二级动作、一张表。
//
// 用 strings.Join 一行一行写，不用反引号的原生字符串：命令小节的标题本身就带
// 反引号（`#### \`doctor\“），原生字符串里写不出来。
var fixture = strings.Join([]string{
	"# 某某 命令参考",
	"",
	"`某某` 是一个用来测生成器的命令行。第二句不参与 NAME 那一行。",
	"",
	"## 全局约定",
	"",
	"这一段对所有命令都成立。",
	"",
	"### 环境变量",
	"",
	"| 环境变量 | 用途 |",
	"|---|---|",
	"| `AAA` | 第一项 |",
	"| `BBB` | 第二项 |",
	"",
	"- 第一条约定",
	"- 第二条约定",
	"",
	"## 按命令",
	"",
	"下面按分组列出。",
	"",
	"### 甲组",
	"",
	"#### `doctor`",
	"",
	"体检。`doctor` 是**粗体**与反引号混着写的。",
	"",
	"#### `price`",
	"",
	"计价。",
	"",
	"#### `price calendar`",
	"",
	"日历那一块。",
	"",
	"### 乙组",
	"",
	"#### `snapshot`",
	"",
	"快照。",
	"",
	"## 搬过家的命令",
	"",
	"旧路径还能用：",
	"",
	"| 旧路径 | 新路径 |",
	"|---|---|",
	"| `old` | `doctor` |",
	"",
	"旧路径不进根帮助页。",
}, "\n")

// groupsFor 是夹具对应的注册表。
func groupsFor() []Group {
	return []Group{
		{Title: "甲组", Commands: []Command{
			{Name: "doctor", Summary: "体检"},
			{Name: "price", Summary: "计价"},
		}},
		{Title: "乙组", Commands: []Command{
			{Name: "snapshot", Summary: "快照"},
		}},
	}
}

func TestParseReferenceReadsTheFourSections(t *testing.T) {
	ref, err := ParseReference(fixture)
	if err != nil {
		t.Fatalf("解析夹具失败：%v", err)
	}

	if ref.Title != "某某 命令参考" {
		t.Errorf("标题是 %q", ref.Title)
	}
	if ref.GlobalTitle != "全局约定" || ref.CommandsTitle != "按命令" || ref.LegacyTitle != "搬过家的命令" {
		t.Errorf("三节的标题读成了 %q / %q / %q",
			ref.GlobalTitle, ref.CommandsTitle, ref.LegacyTitle)
	}
	// 小节标题原样留着：渲染时用的是它，不是包内另写的一份。
	// 四块：一段、一个三级标题、一张表、一个两条的列表。
	if got := len(ref.Global); got != 4 {
		t.Errorf("「全局约定」读到 %d 个块，期望 4 个：%#v", got, ref.Global)
	}
	if got := strings.Join(ref.GroupTitles, ","); got != "甲组,乙组" {
		t.Errorf("分组是 %q", got)
	}
	if len(ref.CommandsIntro) == 0 {
		t.Error("「按命令」到第一个分组之间那段前言没读到")
	}
	want := []string{"doctor", "price", "price calendar", "snapshot"}
	got := make([]string, 0, len(ref.Sections))
	for name := range ref.Sections {
		got = append(got, name)
	}
	sortStrings(got)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("小节读到 %v，期望 %v", got, want)
		}
	}
	if len(ref.Legacy) != 1 || ref.Legacy[0].Old != "old" || ref.Legacy[0].New != "doctor" {
		t.Errorf("搬过家的表读成了 %#v", ref.Legacy)
	}
}

// TestParseReferenceRejects 钉住「不认识的形状要报错」。
//
// 这一条是这份生成器的立场：跳过看不懂的结构时，少掉的那一段与「本来就没写」
// 在产物里长得一样，而手册是给人照着的（LESSONS.md 第 2 条）。所以每种形状都要
// 有一个用例——少一个，下一个写文档的人就能悄悄把它写进去。
func TestParseReferenceRejects(t *testing.T) {
	cases := []struct {
		what string
		md   string
	}{
		{"围栏代码块", "# T\n\n```\nx\n```\n"},
		{"引用块", "# T\n\n> 提示\n"},
		{"一级标题之前就有正文", "前言\n\n# T\n"},
		{"第二个一级标题", "# T\n\n# T2\n"},
		{"不认识的顶层分节", fixture + "\n## 另开一节\n\n内容\n"},
		{"命令小节标题没写反引号", strings.Replace(fixture, "#### `doctor`", "#### doctor", 1)},
		{"命令小节出现了两次", strings.Replace(fixture, "#### `snapshot`", "#### `doctor`", 1)},
		{"三级标题出现在搬过家的命令里", strings.Replace(fixture, "## 搬过家的命令", "### 搬过家的命令", 1)},
		{"表格撞上表头栏数", strings.Replace(fixture, "| `BBB` | 第二项 |", "| `BBB` |", 1)},
		{"缩进了一块没人要的正文", strings.Replace(fixture, "## 全局约定", "  悬空的一行\n\n## 全局约定", 1)},
		{"缩进了一块没人要的正文（标题层级超过四级）", strings.Replace(fixture, "## 全局约定", "##### 太深\n\n## 全局约定", 1)},
	}
	for _, tc := range cases {
		if _, err := ParseReference(tc.md); err == nil {
			t.Errorf("%s：解析通过了，而它应当报错", tc.what)
		}
	}
}

// TestRenderChecksRegistryAgainstSource 钉住「对不上就报错」的三条。
func TestRenderChecksRegistryAgainstSource(t *testing.T) {
	ref, err := ParseReference(fixture)
	if err != nil {
		t.Fatalf("解析夹具失败：%v", err)
	}

	cases := []struct {
		what   string
		groups []Group
	}{
		{"分组顺序对不上", []Group{
			{Title: "乙组", Commands: []Command{{Name: "snapshot", Summary: "快照"}}},
			{Title: "甲组", Commands: []Command{
				{Name: "doctor", Summary: "体检"},
				{Name: "price", Summary: "计价"},
			}},
		}},
		{"分组名字对不上", []Group{
			{Title: "丙组", Commands: []Command{{Name: "doctor", Summary: "体检"}}},
		}},
		{"注册表里的命令没有小节", []Group{
			{Title: "甲组", Commands: []Command{{Name: "wallet", Summary: "钱包"}}},
			{Title: "乙组", Commands: []Command{{Name: "snapshot", Summary: "快照"}}},
		}},
		{"注册表里命令重名", []Group{
			{Title: "甲组", Commands: []Command{
				{Name: "doctor", Summary: "体检"},
				{Name: "doctor", Summary: "又一遍"},
			}},
			{Title: "乙组", Commands: []Command{{Name: "snapshot", Summary: "快照"}}},
		}},
	}
	for _, tc := range cases {
		if _, err := Render(ref, tc.groups); err == nil {
			t.Errorf("%s：渲染通过了，而它应当报错", tc.what)
		}
	}

	if _, err := Render(ref, groupsFor()); err != nil {
		t.Fatalf("夹具与它自己的注册表应当能渲染，却报错：%v", err)
	}
}

func TestRenderProducesOnePagePerCommand(t *testing.T) {
	ref, err := ParseReference(fixture)
	if err != nil {
		t.Fatalf("解析夹具失败：%v", err)
	}
	files, err := Render(ref, groupsFor())
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	if len(files) != 4 {
		t.Fatalf("产出 %d 页，期望 4 页（3 条命令 + 首页）", len(files))
	}
	if files[0].Name != IndexName {
		t.Errorf("第一页是 %q", files[0].Name)
	}
	for _, f := range files {
		body := string(f.Content)
		if !strings.HasPrefix(body, ".TH ") {
			t.Errorf("%s 不是以 .TH 开头", f.Name)
		}
		if !strings.Contains(body, ".SH NAME") || !strings.Contains(body, ".SH SYNOPSIS") {
			t.Errorf("%s 少了 NAME 或 SYNOPSIS 那一节", f.Name)
		}
	}

	// 首页那份清单由注册表填：每条命令都要有它自己的名字、那句话与指向它那一页的引用。
	index := plain(string(files[0].Content))
	for _, g := range groupsFor() {
		if !strings.Contains(index, g.Title) {
			t.Errorf("首页上没有分组 %q", g.Title)
		}
		for _, c := range g.Commands {
			if !strings.Contains(index, ".B "+c.Name) {
				t.Errorf("首页上没列 %q", c.Name)
			}
			if !strings.Contains(index, "larkaxctl\\-"+c.Name+" (1)") {
				t.Errorf("首页上没指向 %q 那一页", c.Name)
			}
		}
	}

	// 二级动作落在它那条命令那一页上，作为一个小节。
	for _, f := range files {
		if f.Name != "larkaxctl-price.1" {
			continue
		}
		if !strings.Contains(plain(string(f.Content)), ".SS calendar") {
			t.Error("`price calendar` 那一节没有落在 price 那一页上")
		}
	}
}

func TestRoffTextEscapesAndBreakPoints(t *testing.T) {
	cases := []struct{ what, in, want string }{
		{"连字符", "a-b", `a\-b`},
		{"反斜杠", `a\b`, `a\eb`},
		{"行首的点", ".env", `\&.env`},
		{"和号（裸着就是字面量）", "a&b", "a&b"},
		{"CJK 之间插断点", "中文", `中\:文`},
		{"收尾标点前不插（行首不能是句号）", "说明。", `说\:明。`},
		{"收尾标点前不插（行首不能是冒号）", "一句话：完", `一\:句\:话：\:完`},
		{"起始标点后不插（行尾不能是左括号）", "（见", "（见"},
		{"ASCII 与 CJK 之间不插", "见 ps 的", `见 ps 的`},
	}
	for _, tc := range cases {
		if got := roffText(tc.in); got != tc.want {
			t.Errorf("%s：roffText(%q) = %q，期望 %q", tc.what, tc.in, got, tc.want)
		}
	}
}

func TestRoffInlineHandlesNestedMarkup(t *testing.T) {
	cases := []struct{ what, in, want string }{
		{"反引号变粗体", "见 `doctor` 那一节", `见 \fBdoctor\fR 那\:一\:节`},
		{"星号变粗体", "这是**重点**", `这\:是\fB重\:点\fR`},
		// 套着写时里层不再切字体：里层的 \fR 会把外层的粗体一起关掉。
		{"粗体里套着反引号", "**看 `x` 一眼**", `\fB看 x 一\:眼\fR`},
		{"反引号里套着星号", "`a **b** c`", `\fBa b c\fR`},
		{"链接写成文字加目标", "见 [文档](docs/x.md)", `见 文\:档（docs/x\&.md）`},
		{"链接文字与目标相同时只留文字", "[docs/x.md](docs/x.md)", `docs/x\&.md`},
		// 断点插在没被标记圈住的那一段里，所以「中文`code`中文」两侧各自成段。
		{"一段里的 CJK 有断点", "中文", `中\:文`},
	}
	for _, tc := range cases {
		if got := roffInline(tc.in); got != tc.want {
			t.Errorf("%s：roffInline(%q) = %q，期望 %q", tc.what, tc.in, got, tc.want)
		}
	}
}

// plain 去掉 roff 的 CJK 断点，好让用例按「读起来是什么」比对产物。
func plain(s string) string { return strings.ReplaceAll(s, `\:`, "") }

func TestJoinWrappedRestoresSpacesAcrossCJKAndLatin(t *testing.T) {
	cases := []struct {
		what string
		in   []string
		want string
	}{
		// 中文中间换行：源文件本来就没有空格。
		{"中文之间直接接上", []string{"这句话在", "中间换行"}, "这句话在中间换行"},
		// 中英混排：换行吃掉了源文件里的空格，两个方向都要补回来。
		{"中文后接代码要空格", []string{"鉴权只认", "`status`"}, "鉴权只认 `status`"},
		{"代码后接中文要空格", []string{"所以下面 `revert`", "之后"}, "所以下面 `revert` 之后"},
		{"中文后接英文要空格", []string{"路径落到了 query", "后面"}, "路径落到了 query 后面"},
		{"英文后接中文要空格", []string{"路径落到了", "query 后面"}, "路径落到了 query 后面"},
		{"数字与中文之间也要空格", []string{"下限 4096", "个 token"}, "下限 4096 个 token"},
		// 标记不占位置：判据看的是标记里面那一个字。
		{"收尾的粗体标记按里面的字判", []string{"各取", "**自己**最近"}, "各取**自己**最近"},
		{"粗体里是英文时按英文判", []string{"仍然是 `admin`**", "and it"}, "仍然是 `admin`** and it"},
		{"开头的粗体标记按里面的字判", []string{"各取**", "自己**最近"}, "各取**自己**最近"},
		// 空格在标记**前面**时，剥完标记要先把它吃掉：屏幕上已经有一个空格了，
		// 再补就是两个。
		{"标记前有空格时不重复补", []string{"各取 **", "自己**最近"}, "各取 **自己**最近"},
		// 全角标点与相邻字符之间按中文排版都不加空格，两个方向一样。
		{"全角开括号后接英文不补", []string{"状态（", "status）"}, "状态（status）"},
		{"英文后接全角开括号不补", []string{"写成 `window`", "（`HH:MM`）"}, "写成 `window`（`HH:MM`）"},
		{"英文后接全角收标点不补", []string{"取值是 `status`", "，退出 4"}, "取值是 `status`，退出 4"},
		{"全角收标点后接英文不补", []string{"（见 ps）", "and it"}, "（见 ps）and it"},
		{"破折号两侧都不补", []string{"回到可用——", "分位数不能相加"}, "回到可用——分位数不能相加"},
		{"省略号后面不补", []string{"已为用户", "… 开通"}, "已为用户… 开通"},
		// 两侧都不是 CJK 时退回「收词 / 起词」判据：词与词补，被折断的词不补。
		{"英文单词之间补一个空格", []string{"见 ps", "and it"}, "见 ps and it"},
		{"右括号后面接英文要空格", []string{"（见 ps)", "and it"}, "（见 ps) and it"},
		{"右括号后面接中文要空格", []string{"（见 ps)", "与它"}, "（见 ps) 与它"},
		{"斜杠后面接英文不补空格（那是路径）", []string{"见 docs/", "cli-reference.md"}, "见 docs/cli-reference.md"},
		{"独立的分隔符后面接词要补空格", []string{"`list` / `get` /", "`health`"}, "`list` / `get` / `health`"},
	}
	for _, tc := range cases {
		if got := joinWrapped(tc.in); got != tc.want {
			t.Errorf("%s：joinWrapped(%q) = %q，期望 %q", tc.what, tc.in, got, tc.want)
		}
	}
}

func TestOneLineAboutDropsTheCommandName(t *testing.T) {
	ref := &Reference{Preamble: []Block{Paragraph{
		Text: "`" + manRefName(IndexName) + "` 是 larkax 的运维命令行。第二句不参与。",
	}}}
	if got := oneLineAbout(ref); got != "larkax 的运维命令行" {
		t.Errorf("首页 NAME 那一行是 %q；期望去掉开头的命令名与「是」、去掉句号，只留后面那截", got)
	}

	// 认不出「<命令名> 是 …」这个开头时原样用它：句子长一点，但不会漏掉内容。
	ref.Preamble = []Block{Paragraph{Text: "某某是另一个东西。第二句。"}}
	if got := oneLineAbout(ref); got != "某某是另一个东西" {
		t.Errorf("认不出开头时是 %q，期望原样用第一句", got)
	}
}
