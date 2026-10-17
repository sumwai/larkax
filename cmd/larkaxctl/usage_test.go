package main

import (
	"slices"
	"strings"
	"testing"
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
// 根页面上的那一句，以及它自己的 --help 正文。
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
			if strings.TrimSpace(c.help) == "" {
				t.Errorf("%s 没有 --help 正文", c.name)
				continue
			}
			// 第一行要点出它自己是谁：一份没头没尾的用法文本，多半是从别处
			// 抄来忘了改的。
			if first := strings.SplitN(c.help, "\n", 2)[0]; !strings.Contains(first, "larkaxctl "+c.name) {
				t.Errorf("%s 的 --help 第一行没有点出这条命令：%q", c.name, first)
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
				if err != nil && !helpRequested(err) {
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
