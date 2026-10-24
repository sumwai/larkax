package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/manpage"
)

// 本文件是手册生成器的**驱动**：把命令注册表交给 internal/manpage，再把产物写出去。
//
// # 为什么是「一条测试」而不是一个 main 包
//
// 注册表（commandGroups）住在 package main 里，别的包引不到——它就在上面几十行处，
// 与分派表是同一份数据。要让生成器与注册表共处一个二进制，只有三条路：把注册表
// 搬进一个可导入的包（那会把它与 `run` 字段一起搬走，而 run 是本包的函数）、
// 新开一个 main 包再抄一份命令清单（**多了一个会漂的第二源**，正是这一件事要
// 避免的）、或者在这里当一条测试跑。
//
// 于是产物由 `go test` 生成，落盘的去向由环境变量 LARKAX_MAN_OUT 给：
//
//	go test -count=1 -run '^TestGenerateManPages$' ./cmd/larkaxctl/   # 只核对，不落盘
//	LARKAX_MAN_OUT=man/man1 go test -count=1 -run '^TestGenerateManPages$' ./cmd/larkaxctl/
//
// 同步由 scripts/check-man-pages.sh 管（重新生成 → 与 man/ 比对 → 变了就红），
// 与 cargo 的 validate-man.sh 是同一个形状。LARKAX_MAN_OUT 给相对路径时按**仓库根**
// 算，不按本包目录（见下面那个 filepath.IsAbs 分支的理由）。
//
// # 没设 LARKAX_MAN_OUT 时也在干活
//
// 这一条不设环境变量就跳过的话，「跳过」与「通过」的输出一模一样
//（LESSONS.md 第 16 条）。所以不设时它照样解析源文件、照样渲染，只是不落盘：
// 注册表与 docs/cli-reference.md 对不上时，`go test ./cmd/larkaxctl/` 就会红。

// manOutEnv 是产物写到哪儿去的环境变量。设了才落盘，目录不存在时建。
const manOutEnv = "LARKAX_MAN_OUT"

// referenceDoc 是长文本那一份源文件，路径相对本包目录。
var referenceDoc = filepath.Join("..", "..", "docs", "cli-reference.md")

// manGroups 把命令注册表翻成生成器认的形状。
//
// 这一段是「手册的名字与一句话来自注册表」的落点，也是全仓唯一一处看得见
// commandGroups 又看得见 manpage 的代码。
func manGroups() []manpage.Group {
	groups := make([]manpage.Group, 0, len(commandGroups))
	for _, g := range commandGroups {
		cmds := make([]manpage.Command, 0, len(g.commands))
		for _, c := range g.commands {
			cmds = append(cmds, manpage.Command{Name: c.name, Summary: c.summary})
		}
		groups = append(groups, manpage.Group{Title: g.title, Commands: cmds})
	}
	return groups
}

// TestGenerateManPages 渲染全部手册，核对几条渲染不出来就看不见的性质，
// 并按 LARKAX_MAN_OUT 决定要不要落盘。
func TestGenerateManPages(t *testing.T) {
	raw, err := os.ReadFile(referenceDoc)
	if err != nil {
		t.Fatalf("读不到 %s：%v", referenceDoc, err)
	}
	ref, err := manpage.ParseReference(string(raw))
	if err != nil {
		t.Fatalf("%s 读不动：%v", referenceDoc, err)
	}
	groups := manGroups()
	files, err := manpage.Render(ref, groups)
	if err != nil {
		t.Fatalf("渲染不动：%v", err)
	}

	// ── 不变量 ──────────────────────────────────────────────────────────
	//
	// Render 已经核对过「注册表与文档对得上」（分组顺序、每条命令都有小节）。
	// 这里补的是**产物**那一侧：一页空文件、一页忘了写 NAME、页数对不上，
	// 都能让上面那一步通过，却让阅读的人拿到一页什么都没有的「手册」。

	total := 0
	for _, g := range groups {
		total += len(g.Commands)
	}
	if len(files) != total+1 {
		t.Fatalf("产出 %d 页，期望 %d 页（每条命令一页 + 首页）", len(files), total+1)
	}
	if files[0].Name != manpage.IndexName {
		t.Errorf("第一页是 %q，期望首页 %q", files[0].Name, manpage.IndexName)
	}

	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.Name] {
			t.Errorf("产物里 %q 出现了两次", f.Name)
		}
		seen[f.Name] = true
		if !strings.HasSuffix(f.Name, ".1") {
			t.Errorf("%q 不是一页手册（手册的文件名以 .1 结尾）", f.Name)
		}
		if len(f.Content) == 0 {
			t.Errorf("%q 是空的", f.Name)
			continue
		}
		// 第一行必须是 .TH：少了它，`man -l` 不认这一页，而它在文件里
		// 什么都不显示——只有翻页的人发现自己看到的是一堆宏名字。
		if first, _, _ := strings.Cut(string(f.Content), "\n"); !strings.HasPrefix(first, ".TH ") {
			t.Errorf("%q 的第一行是 %q，期望以 .TH 开头", f.Name, first)
		}
	}

	// 一句话来自注册表：把 CJK 断点（roffText 插的 \:）去掉之后，注册表里
	// 那一句话要原样出现在它那一页上。手册里另抄一句话时，改注册表不会有
	// 任何一步失败，而两句不一样的话只在有人对着敲命令时才暴露。
	for _, g := range groups {
		for _, c := range g.Commands {
			if c.Summary == "" {
				t.Errorf("命令 %q 在注册表里没有一句话", c.Name)
				continue
			}
			name := "larkaxctl-" + c.Name + ".1"
			page := ""
			for _, f := range files {
				if f.Name == name {
					page = string(f.Content)
				}
			}
			if page == "" {
				t.Errorf("命令 %q 没有它自己那一页（%s）", c.Name, name)
				continue
			}
			if !strings.Contains(strings.ReplaceAll(page, `\:`, ""), c.Summary) {
				t.Errorf("%s 上没有注册表里那一句话 %q", name, c.Summary)
			}
		}
	}

	// 源文件那张「旧路径 / 新路径」表要与代码对得上：表的旧路径必须是
	// legacyCommands 里真有一条（那条旧路真的还能跑），新路径必须落在注册表里
	// （那条路真的在）。任一处改名而表没跟上时，照着这张表敲的人会敲到一条
	// 不存在的命令——而手册上白纸黑字印着它。
	legacyNames := map[string]bool{}
	for _, c := range legacyCommands {
		legacyNames[c.name] = true
	}
	known := map[string]bool{}
	for _, g := range groups {
		for _, c := range g.Commands {
			known[c.Name] = true
		}
	}
	if len(ref.Legacy) == 0 {
		t.Fatal("源文件里那张「旧路径 / 新路径」表一行都没有")
	}
	for _, l := range ref.Legacy {
		if old := manpage.SectionCommand(l.Old); !legacyNames[old] {
			t.Errorf("表里把 %q 写成 %q 的旧路径，而 legacyCommands 里没有 %q 这条命令",
				l.New, l.Old, old)
		}
		if new := manpage.SectionCommand(l.New); !known[new] {
			t.Errorf("表里把 %q 的旧路径写成 %q，而注册表里没有 %q 这条命令",
				l.Old, l.New, new)
		}
	}

	out := os.Getenv(manOutEnv)
	if out == "" {
		t.Logf("已渲染 %d 页；%s 没设，没有落盘（只核对了源文件与注册表）", len(files), manOutEnv)
		return
	}
	// 相对路径按**仓库根**算，不按本包目录：人写 LARKAX_MAN_OUT=man/man1 时想的是
	// 仓库根下那个 man/man1，而 go test 跑测试用的当前目录是包目录（cmd/larkaxctl），
	// 那样会凭空多出一个 cmd/larkaxctl/man/man1。
	if !filepath.IsAbs(out) {
		out = filepath.Join(moduleRoot(t), out)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("建不了目录 %s：%v", out, err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(out, f.Name), f.Content, 0o644); err != nil {
			t.Fatalf("写 %s 失败：%v", filepath.Join(out, f.Name), err)
		}
	}
	t.Logf("已把 %d 页写到 %s", len(files), out)
}

// moduleRoot 从本包目录往上找 go.mod，返回模块根。
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取不到当前目录：%v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("从 %s 往上找不到 go.mod", dir)
		}
		dir = parent
	}
}
