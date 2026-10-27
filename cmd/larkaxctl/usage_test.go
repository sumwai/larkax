package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// ── 根帮助页：分组、层级与「每条命令都有话说」 ──────────────────────────
//
// 根页面原先是一张三十来行的用法表：三级、四级的标志（--amount、
// --idempotency-key）直接堆在上面，而一级命令只占其中几行，看的人要在里面
// 找自己要的那一条。这一组用例把「收敛」这件事钉住——它是一件会**悄悄退回
// 去**的事：新加一条命令时，最省事的写法就是把它的参数也塞进根页面。

// usageGroup 是从根帮助页文本里解析出来的一组命令。
type usageGroup struct {
	title string
	names []string
}

// parseUsageGroups 按**渲染出来的文本**切出根页面上的分组与命令名。
//
// 解析的对象是文本而不是注册表：这一组用例要证明的正是「页面上写着的就是
// 那几条」，从注册表里再读一遍等于什么都没验。分组的开头按注册表里的标题
// 认（标题后面跟一个全角冒号），所以 "用法：" 这类小节标题不会被当成一组。
//
// 已知边界：**没在注册表里的分组标题会被整段跳过**。它挡不住「页面上多打了
// 一组」，但那一组的名字同样会被下面按组比的用例发现——除非那一组里一条
// 命令都没有，而那种排版看不出意义。
func parseUsageGroups(t *testing.T, text string) []usageGroup {
	t.Helper()

	known := map[string]bool{}
	for _, g := range commandGroups {
		known[g.title+"："] = true
	}

	var groups []usageGroup
	var current *usageGroup
	for _, line := range strings.Split(text, "\n") {
		if known[line] {
			groups = append(groups, usageGroup{title: strings.TrimSuffix(line, "：")})
			current = &groups[len(groups)-1]
			continue
		}
		if current == nil {
			continue
		}
		if strings.TrimSpace(line) == "" {
			// 组与组之间是空行：一组到空行为止，后面的正文（全局选项、
			// 退出码）不会被当成命令名收进来。
			current = nil
			continue
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "  "), "  ")
		if !ok {
			t.Fatalf("分组 %s 里的这一行没有「命令名 + 一句话」的形状：%q", current.title, line)
		}
		current.names = append(current.names, strings.TrimSpace(name))
	}
	return groups
}

// TestRootUsageListsExactlyTheRegisteredCommands 断言根页面上的命令清单就是
// 注册表里的那一份：分组、顺序、组内成员都要对得上。
func TestRootUsageListsExactlyTheRegisteredCommands(t *testing.T) {
	got := parseUsageGroups(t, usage)
	if len(got) != len(commandGroups) {
		t.Fatalf("根页面上的分组 %d 个，注册表里 %d 个", len(got), len(commandGroups))
	}
	for i, want := range commandGroups {
		if got[i].title != want.title {
			t.Errorf("第 %d 组的标题 = %q，想要 %q", i+1, got[i].title, want.title)
		}
		names := make([]string, 0, len(want.commands))
		for _, c := range want.commands {
			names = append(names, c.name)
		}
		if !slices.Equal(got[i].names, names) {
			t.Errorf("第 %d 组（%s）列出的命令 = %v，想要 %v", i+1, want.title, got[i].names, names)
		}
	}
}

// TestRootUsageHasNoSubcommandFlags 断言根页面上不出现子命令的参数。
//
// 判据是参数名本身：三级、四级的标志出现在根页面上，就说明某条命令的具体
// 动作又爬回了顶层。--env-file 与 --format 不在其列——它们对每条子命令都
// 生效，本来就该写在这里。
//
// 清单里那几条**已经改过名**的（--order、--canonical、--owner 之类）是刻意
// 留着的：它们更容易以一种「顺手补一句旧写法」的姿态爬回来。
func TestRootUsageHasNoSubcommandFlags(t *testing.T) {
	subcommandFlags := []string{
		"--account", "--actor", "--amount", "--apply", "--asset", "--base-url",
		"--batch-size", "--by", "--canonical", "--channel", "--credential-stdin",
		"--direction", "--disable", "--dry-run", "--enable", "--expr", "--id",
		"--idempotency-key", "--key", "--live", "--models", "--months", "--order",
		"--owner", "--priority", "--reason", "--scope", "--surface", "--value",
		"--verify",
	}

	for _, name := range subcommandFlags {
		if strings.Contains(usage, name) {
			t.Errorf("根页面上不该出现子命令的参数 %s：它属于某条子命令的 --help", name)
		}
	}
}

// TestRootUsageCommandsAreDispatchable 断言根页面上列出的每条命令都能被分派。
//
// 根页面与分派共用一张注册表，正常不会对不上。这条用例防的是有人在页面上
// 把名字改了半处（settings 写成 setting 之类）：那种错在页面上看不出来，
// 要等真的有人照着敲。
func TestRootUsageCommandsAreDispatchable(t *testing.T) {
	groups := parseUsageGroups(t, usage)
	if len(groups) == 0 {
		t.Fatal("根页面上一个命令都没解析出来：这一条会安静地通过，所以先在这里拦住")
	}

	for _, g := range groups {
		for _, name := range g.names {
			if _, ok := lookupCommand(name); !ok {
				t.Errorf("根页面列出了 %q（%s 组），但分派不认这个名字", name, g.title)
			}
		}
	}
}

// TestCommandNamesAreUnique 断言正式命令与旧路径之间不重名。
//
// 重名的表现是其中一条永远轮不到，而哪一条赢取决于查表的顺序——排查之前
// 得先知道那个顺序。
func TestCommandNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range commandGroups {
		for _, c := range g.commands {
			if seen[c.name] {
				t.Errorf("命令名 %q 在注册表里出现了两次", c.name)
			}
			seen[c.name] = true
		}
	}

	if len(legacyCommands) == 0 {
		t.Error("旧路径一条都没有：搬过家的那几条命令的脚本会全部失效")
	}
	for _, c := range legacyCommands {
		if seen[c.name] {
			t.Errorf("旧路径 %q 与正式命令重名", c.name)
		}
		seen[c.name] = true
	}
}

// TestEveryFirstLevelCommandHasSomethingToSay 断言每条一级命令都有两句话：
// 根页面上的那一句，以及它自己的帮助正文。
//
// 漏掉的是新加一条命令时最容易被忘的那一件：注册表里能跑（run 有值）、
// 页面上有一行（summary 有值），一敲 `larkaxctl <新命令> --help` 却什么都
// 打不出来。
func TestEveryFirstLevelCommandHasSomethingToSay(t *testing.T) {
	for _, g := range commandGroups {
		for _, c := range g.commands {
			if strings.TrimSpace(c.summary) == "" {
				t.Errorf("%s（%s 组）在根页面上没有说明", c.name, g.title)
			}
			if strings.TrimSpace(c.help.long) == "" {
				t.Errorf("%s 没有帮助正文", c.name)
				continue
			}
			// 第一行要点出它自己是谁：一份没头没尾的用法文本，多半是从别处
			// 抄来忘了改的。
			if first := strings.SplitN(c.help.long, "\n", 2)[0]; !strings.Contains(first, "larkaxctl "+c.name) {
				t.Errorf("%s 的帮助正文第一行没有点出这条命令：%q", c.name, first)
			}
		}
	}
}

// TestEveryFirstLevelCommandAnswersHelp 断言 `larkaxctl <命令> --help` 真的
// 打得出一句话来。
//
// 上一条比的是注册表里那份文本，这一条比的是**命令真的被这么跑时**打出来的
// 东西：有的命令会把 --help 当普通参数收下然后接着干自己的事（version 原先
// 就是这样，`version --help` 打出来的是一个版本号），页面上那份文本对不对
// 说明不了这件事。
func TestEveryFirstLevelCommandAnswersHelp(t *testing.T) {
	for _, g := range commandGroups {
		for _, c := range g.commands {
			t.Run(c.name, func(t *testing.T) {
				var err error
				stdout, stderr := captureStreams(t, func() { err = c.run([]string{"--help"}) })
				// flag 解析到 --help 时返回 ErrHelp，而那时用法已经打过了：
				// 那不是失败，main 也照这个判据收尾（见 helpRequested）。
				if err != nil && !cli.HelpRequested(err) {
					t.Fatalf("%s --help 报错了: %v", c.name, err)
				}
				if out := stdout + stderr; !strings.Contains(out, "larkaxctl "+c.name) {
					t.Errorf("%s --help 没有打出自己的用法：stdout=%q stderr=%q", c.name, stdout, stderr)
				}
			})
		}
	}
}

// TestLegacyCommandsStayOutOfRootUsage 断言旧路径不出现在根页面的分组清单里。
//
// 它们仍然可用（有用例逐条钉着），但不该被列成正式写法：列出来等于同时给了
// 两套推荐做法，而其中一套是错的。
func TestLegacyCommandsStayOutOfRootUsage(t *testing.T) {
	listed := map[string]bool{}
	for _, g := range parseUsageGroups(t, usage) {
		for _, name := range g.names {
			listed[name] = true
		}
	}
	for _, c := range legacyCommands {
		if listed[c.name] {
			t.Errorf("旧路径 %q 出现在根页面的分组清单里", c.name)
		}
	}
}

// ── 根帮助页：一屏之内、不给用户看设计理由 ──────────────────────────────
//
// 上面那组用例管的是「页面上写着哪些命令」，这一组管的是**页面本身有多长、
// 写的是什么口吻**。根页面原先 96 行，其中约 40 行是「为什么这么设计」的长句；
// 它们对跑命令的人没用，却挡在命令清单后面。挪进 docs/cli-reference.md 之后
// 会**悄悄爬回来**——新加一个选项时，最省事的写法就是顺手在这里补一句理由——
// 所以行数与口吻各要一道门钉着。

// TestRootUsageFitsOneScreen 断言根帮助页去掉末尾空行后不超过 45 行，且每行
// 显示宽度不超过 100 列。
//
// 宽度用 displayWidth（见 table.go）量，不用 len：中文、全角标点与 ANSI 转义
// 序列按字节数算都会偏，而偏的方向（把一行看得比实际窄）正好放过真正超宽的
// 那一行。
func TestRootUsageFitsOneScreen(t *testing.T) {
	const maxLines, maxWidth = 45, 100

	lines := strings.Split(strings.TrimRight(usage, "\n"), "\n")
	if len(lines) > maxLines {
		t.Errorf("根帮助页 %d 行，超过一屏的 %d 行", len(lines), maxLines)
	}
	for i, line := range lines {
		if w := cli.DisplayWidth(line); w > maxWidth {
			t.Errorf("第 %d 行宽 %d 列，超过 %d：%q", i+1, w, maxWidth, line)
		}
	}
}

// rootUsageDesignReasonMarkers 是「设计理由」的标记词：根帮助页上出现它们，
// 就说明某段「为什么这么设计」的说明又爬回了用户面前。
//
// 判据取词而不是整句：理由换个句式就能绕过去，而这些词在「是什么 / 怎么用」的
// 口吻里本来就用不上（「见 internal/xxx」「见 xxx_test.go」是两个内部的落脚点，
// 同样不该出现在给用户的页面上）。
var rootUsageDesignReasonMarkers = []string{
	"行业惯例", "惯例", "取舍", "理由是", "为什么", "因为",
	"多数工具", "internal/", "test.go",
}

// TestRootUsageHasNoDesignRationale 断言根帮助页不含设计理由。
//
// 反例是把「-o 是行业惯例（kubectl、gh）」这类句子搬回根页面：它是写这套程序的
// 人关心的取舍，不是跑命令的人当下要的东西，归 docs/cli-reference.md。
func TestRootUsageHasNoDesignRationale(t *testing.T) {
	for _, marker := range rootUsageDesignReasonMarkers {
		if strings.Contains(usage, marker) {
			t.Errorf("根帮助页出现了设计理由标记词 %q", marker)
		}
	}
}

// TestRootUsagePointsToReference 断言根帮助页末尾给出了去向：查一条命令用
// larkaxctl help <命令>，更细的约定在 docs/cli-reference.md。
func TestRootUsagePointsToReference(t *testing.T) {
	for _, want := range []string{"larkaxctl help", "docs/cli-reference.md"} {
		if !strings.Contains(usage, want) {
			t.Errorf("根帮助页没有给出 %q 这条去向", want)
		}
	}
}

// TestRootUsagePointerTargetsWork 钉住「根帮助页给的那条去向真的走得通」。
//
// 指针指向一个不存在的写法比不写更坏：人照着敲一次，得到的是刚才那一页，
// 而他会以为自己敲错了。所以 `larkaxctl help <命令>` 必须打出**那条命令**的
// 帮助正文，而不是根帮助页。
func TestRootUsagePointerTargetsWork(t *testing.T) {
	for _, name := range []string{"doctor", "channel", "price"} {
		c, ok := lookupCommand(name)
		if !ok {
			t.Fatalf("命令 %q 不在注册表里，用例的前提就不成立", name)
		}
		var err error
		stdout, stderr := captureStreams(t, func() { err = dispatch([]string{"help", name}) })
		if err != nil {
			t.Fatalf("larkaxctl help %s 出错: %v（stderr=%q）", name, err, stderr)
		}
		if !strings.Contains(stdout, c.help.long) {
			t.Errorf("larkaxctl help %s 没打出那条命令的帮助正文，stdout=%q", name, stdout)
		}
		if strings.TrimSpace(stdout) == strings.TrimSpace(usage) {
			t.Errorf("larkaxctl help %s 打的是根帮助页——指针指向了自己", name)
		}
	}
}

// ── 子命令帮助：两版各自要守的东西 ──────────────────────────────────────
//
// 一条命令的帮助原先只有一份：`-h` 打的就是那一整份，doctor 77 行、price 84 行，
// 其中过半是设计说明。现在按**要帮助的方式**分成两版——短版回答「有哪些动作、
// 参数怎么给」，长版再把边界说完，设计说明则搬去 docs/cli-reference.md。
//
// 这一组用例钉的正是「分开了，而且各自守住了本分」：预算（短版一屏之内）、
// 首行（点出自己是谁）、指针（短版要指向长版）、两级可达（两种写法都打长版），
// 以及两版都不给用户看设计理由。分开之后**悄悄退回一份**是最省事的走法：新加
// 一条命令时把长版复制给短版，谁也看不出来。

// shortUsageMaxLines 是短版的行数上限：一屏之内。
const shortUsageMaxLines = 18

// checkShortUsage 检查一份短版该有的三件事：非空、一屏之内、首行点出自己是谁。
//
// 抽出来是因为一级命令与三个二级命令组共用同一套判据——两处各写一遍的话，
// 其中一处松掉在输出上看不出来。
func checkShortUsage(t *testing.T, what, name, short string) {
	t.Helper()

	if strings.TrimSpace(short) == "" {
		t.Fatalf("%s 没有短版：`-h` 会什么都打不出来", what)
	}
	lines := strings.Split(strings.TrimSpace(short), "\n")
	if len(lines) > shortUsageMaxLines {
		t.Errorf("%s 的短版 %d 行，超过一屏的 %d 行", what, len(lines), shortUsageMaxLines)
	}
	if !strings.Contains(lines[0], "larkaxctl "+name) {
		t.Errorf("%s 的短版首行没有点出这条命令：%q", what, lines[0])
	}
}

// TestSubcommandShortUsageFitsOneScreen 断言每条一级命令的短版非空、首行点出
// 自己是谁、不超过 18 行，并且没有退化成与长版逐字相同的一份。
func TestSubcommandShortUsageFitsOneScreen(t *testing.T) {
	for _, g := range commandGroups {
		for _, c := range g.commands {
			t.Run(c.name, func(t *testing.T) {
				checkShortUsage(t, c.name, c.name, c.help.short)

				if strings.TrimSpace(c.help.short) == strings.TrimSpace(c.help.long) {
					t.Errorf("%s 的长版与短版逐字相同：分层没有发生", c.name)
				}
			})
		}
	}
}

// TestSubcommandShortUsagePointsToLongVersion 断言每条短版都给出了长版的去向：
// 一份摘要的最后一行如果说不出「其余的在哪」，读的人只能猜。
func TestSubcommandShortUsagePointsToLongVersion(t *testing.T) {
	for _, g := range commandGroups {
		for _, c := range g.commands {
			if pointer := "larkaxctl help " + c.name; !strings.Contains(c.help.short, pointer) {
				t.Errorf("%s 的短版没有指向长版（应含 %q）", c.name, pointer)
			}
		}
	}
}

// TestSubcommandHelpHasTwoLevels 断言两级都走得通，且走的是各自该走的那一版：
//
//   - `larkaxctl help <命令>` 与 `<命令> help` 打长版；
//   - `-h` / `--help` 打短版。
//
// 分开两条路而不是只比注册表里那两份文本，是因为「哪条路给哪一版」是分派与
// 各命令的参数解析合起来决定的：注册表里两份都在，路上却可能两个写法都打短版。
func TestSubcommandHelpHasTwoLevels(t *testing.T) {
	for _, g := range commandGroups {
		for _, c := range g.commands {
			t.Run(c.name, func(t *testing.T) {
				for _, args := range [][]string{{"help", c.name}, {c.name, "help"}} {
					var err error
					stdout, stderr := captureStreams(t, func() { err = dispatch(args) })
					if err != nil {
						t.Fatalf("%v 出错: %v（stderr=%q）", args, err, stderr)
					}
					if !strings.Contains(stdout, c.help.long) {
						t.Errorf("%v 打的不是长版，stdout=%q", args, stdout)
					}
				}

				var err error
				stdout, stderr := captureStreams(t, func() { err = c.run([]string{"--help"}) })
				if err != nil && !cli.HelpRequested(err) {
					t.Fatalf("%s --help 报错了: %v", c.name, err)
				}
				if out := stdout + stderr; !strings.Contains(out, c.help.short) {
					t.Errorf("%s --help 打的不是短版：stdout=%q stderr=%q", c.name, stdout, stderr)
				}
			})
		}
	}
}

// TestSubcommandHelpHasNoDesignRationale 断言两版帮助都不含设计理由标记词。
//
// 词表与根帮助页共用一份（见 rootUsageDesignReasonMarkers）：某个字段为什么必填、
// 别家工具怎么做这类话，对跑命令的人当下没用；它们的家是 docs/cli-reference.md。
// 短版里出现它们说明这一版又长回去了，长版里出现说明说明文字没搬走。
func TestSubcommandHelpHasNoDesignRationale(t *testing.T) {
	for _, tc := range helpTextsForGate(t) {
		for _, marker := range rootUsageDesignReasonMarkers {
			if strings.Contains(tc.text, marker) {
				t.Errorf("%s 出现了设计理由标记词 %q", tc.what, marker)
			}
		}
	}
}

// helpTextsForGate 是这几道门管辖的全部文本：一级命令的短版与长版，加上每个
// 命令组的短版与长版（按注册表派生，不手抄）。
//
// 抽成一份是因为「管哪些文本」这件事本身就是判据的一部分：另写几份清单的话，
// 新加一条命令或一个命令组时，漏掉的那一份不会报错，只会安静地少管一段文本。
//
// 组的文本走 dispatch 取实际打出来的那一份，而不是从某个常量变量读：很多组与
// 父组共用短版（见 TestGroupPathsAnswerBothHelpForms 的说明），按常量读会漏掉
// 其中一半，也会把「哪一份真的被打出来」这件事绕过。
func helpTextsForGate(t *testing.T) []struct{ what, text string } {
	t.Helper()

	var texts []struct{ what, text string }
	for _, g := range commandGroups {
		for _, c := range g.commands {
			texts = append(texts,
				struct{ what, text string }{c.name + " 的短版", c.help.short},
				struct{ what, text string }{c.name + " 的长版", c.help.long},
			)
		}
	}
	for _, path := range groupPathsDerived(t) {
		parts := strings.Fields(path)
		for _, form := range []struct {
			what string
			args []string
		}{
			{"短版", append(slices.Clone(parts), "-h")},
			{"长版", append(slices.Clone(parts), "help")},
		} {
			var err error
			stdout, stderr := captureStreams(t, func() { err = dispatch(form.args) })
			if err != nil && !cli.HelpRequested(err) {
				t.Fatalf("%s 的%s取不到：%v（stderr=%q）", path, form.what, err, stderr)
			}
			texts = append(texts,
				struct{ what, text string }{path + " 的" + form.what, stdout + stderr})
		}
	}
	return texts
}

// docPointerRE 认长版末尾那条去向：`docs/cli-reference.md 的「xxx」一节`。
var docPointerRE = regexp.MustCompile(`docs/cli-reference\.md 的「([^」]+)」一节`)

// TestHelpPointersTargetRealDocSections 钉住长版末尾那条去向真的落在文档里。
//
// 帮助里写着「更细的约定见 docs/cli-reference.md 的「doctor」一节」，而那一节的名字
// 是手抄进帮助文本的：文档里改了标题、或者抄错一个词，指针就指向空气——比不写更坏，
// 因为照它去找的人会以为自己找错了地方。
func TestHelpPointersTargetRealDocSections(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli-reference.md"))
	if err != nil {
		t.Fatalf("读不到 docs/cli-reference.md：%v", err)
	}
	doc := string(raw)

	checked := 0
	for _, tc := range helpTextsForGate(t) {
		if !strings.HasSuffix(tc.what, "的长版") {
			continue
		}
		m := docPointerRE.FindStringSubmatch(tc.text)
		if m == nil {
			t.Errorf("%s 末尾没有给出去向（应写「见 docs/cli-reference.md 的「<节名>」一节」）", tc.what)
			continue
		}
		if section := m[1]; !strings.Contains(doc, "#### `"+section+"`") {
			t.Errorf("%s 指向文档里的「%s」一节，而文档里没有这一节", tc.what, section)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("一条去向都没检查到：这一条会安静地通过，所以先在这里拦住")
	}
}

// TestQuietDoesNotSuppressHelp 钉住既有行为：`-q` 压的是「给人看」那一档的
// 输出，而帮助是使用者主动要的，短版长版都照打。
//
// 这一条容易在收口时丢掉：帮助文本一旦改走 humanStdout()，`larkaxctl -q help
// doctor` 就会安静地什么都不打——而那正是最需要它的人（脚本作者）会遇到的情形。
func TestQuietDoesNotSuppressHelp(t *testing.T) {
	cli.QuietOutput = true
	t.Cleanup(func() { cli.QuietOutput = false })

	for _, tc := range []struct {
		what     string
		args     []string
		contains string
	}{
		{"长版", []string{"help", "channel"}, channelUsage},
		{"短版", []string{"channel", "--help"}, channelShortUsage},
	} {
		var err error
		stdout, stderr := captureStreams(t, func() { err = dispatch(tc.args) })
		if err != nil && !cli.HelpRequested(err) {
			t.Fatalf("%v 报错了: %v", tc.args, err)
		}
		if out := stdout + stderr; !strings.Contains(out, tc.contains) {
			t.Errorf("--quiet 把 %s 帮助压掉了：stdout=%q stderr=%q", tc.what, stdout, stderr)
		}
	}
}
