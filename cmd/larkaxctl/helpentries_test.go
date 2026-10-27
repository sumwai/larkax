package main

import (
	"slices"
	"strings"
	"testing"
)

// ── 帮助入口：清单从注册表派生，不手抄 ──────────────────────────────────
//
// 这一组门替掉原来那张手抄的 subGroupHelpCases。手抄的代价是实测过的：17 条
// 三级命令只抄了 10 条，漏掉的 9 条里含 4 条帮助入口根本没接的命令，而 CI
// 全绿——「测试表不派生自注册表，就必然会漏」。
//
// 清单唯一的来源是 completionActionTable：它已经在做「路径 → 动作常量」的接线
// （补全要用同一份），所以新加一个命令组时不可能只改一处。

// groupPathsDerived 是注册表里所有**命令组**的路径（有下级的那些），按补全表派生。
//
// 顺序排过，只为让失败输出稳定可读。
func groupPathsDerived(t *testing.T) []string {
	t.Helper()

	paths := make([]string, 0, len(completionActionTable))
	for path := range completionActionTable {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	if len(paths) < 10 {
		t.Fatalf("只派生出 %d 条命令组路径：补全表或它的形状变了，这个扫描器坏了", len(paths))
	}
	return paths
}

// allPathsDerived 是注册表里的全部路径：顶层命令 + 每个命令组 + 组下每个动作。
//
// 与 groupPathsDerived 同一个来源（顶层注册表与 completionActionTable），所以
// 新加一条命令时这两道门自动管到它。
func allPathsDerived(t *testing.T) []string {
	t.Helper()

	seen := map[string]bool{}
	var paths []string
	add := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}

	for _, g := range commandGroups {
		for _, c := range g.commands {
			add(c.name)
		}
	}
	for _, c := range legacyCommands {
		add(c.name)
	}
	for path, verbs := range completionActionTable {
		add(path)
		for _, verb := range verbs {
			add(path + " " + verb)
		}
	}

	slices.Sort(paths)
	if len(paths) < 100 {
		t.Fatalf("只派生出 %d 条路径：注册表的形状变了，这个扫描器坏了", len(paths))
	}
	return paths
}

// runHelpForm 跑一种帮助入口，返回 rc 是否为 0 与打出来的文本。
//
// 一组命令的 -h 若落到 flag 解析上，回来的是 usageErr(flag.ErrHelp)：用法已经
// 打过了，那不是失败（main 也照这个判据给退出码 0，见 helpRequested）。
func runHelpForm(t *testing.T, path, form string) (bool, string) {
	t.Helper()

	var err error
	stdout, stderr := captureStreams(t, func() {
		err = dispatch(append(strings.Fields(path), form))
	})
	if err != nil && !helpRequested(err) {
		return false, stdout + stderr
	}
	return true, stdout + stderr
}

// TestGroupPathsAnswerBothHelpForms 是门一：注册表派生的每个命令组，
// `help` 与 `-h` 两条入口都要打得出来，而且各自打得是对的那一版。
//
// 这一条要防的是这样一类命令组：`runXxx` 的 switch 里没有 help 分支，于是
// `help` 直接落进「未知的子命令」那一档——用法明明写在自己的长版里，人照着
// 敲一次却得到一句参数错。加了新命令组却忘了接入口时，这条会红。
//
// 两版的判据同时用了两条互补的：
//
//   - 长版里有末尾那条去向（`docs/cli-reference.md 的「…」一节`），短版里没有
//     ——它是长版独有的**结构**特征，不像行数那样改一次断行就变。
//   - 短版不比长版长、两版不逐字相同。
//
// 有两条**不在这里**断言，各有原因：
//
//   - **首行点出「自己是谁」**：有 8 条组的 `-h` 首行不是自己的名字，分三类——
//     ① 与父组的短版**逐字相同**（4 条：`admin token`、`user delete`、`user key
//     plans`、`user plan order`）；② 父组短版**加一行搬迁提示**（2 条：`plan key`、
//     `plan order`，那行提示由 `runLegacyUserPath` 追加）；③ 顶层别名，短版本来就
//     属于另一条完整路径（2 条：`calendar` 打的是 `price calendar` 的，`idempotency`
//     打的是 `maintenance idempotency` 的）。三类都是「一条文案一处来源」的直接
//     结果，为它们各写一份副本才是要避免的。门只要求「打得出来且是那一版」。
//   - **短版一屏之内（18 行）**：`wallet payout` 的短版现在 25 行，是先于这道门
//     的既有问题（它原先不在这张手抄表里，所以从来没被量过）。把它写进门会让
//     这条从第一天起就是红的，而改文案不在本条范围内；一条长期红着的门与没有
//     这条门是一回事。一级命令的一屏预算不受影响，仍由 checkShortUsage 对着
//     每个顶层命令跑。
func TestGroupPathsAnswerBothHelpForms(t *testing.T) {
	for _, path := range groupPathsDerived(t) {
		t.Run(path, func(t *testing.T) {
			longOK, long := runHelpForm(t, path, "help")
			if !longOK {
				t.Fatalf("%s help 报错了，打出来的是：%q", path, long)
			}
			shortOK, short := runHelpForm(t, path, "-h")
			if !shortOK {
				t.Fatalf("%s -h 报错了，打出来的是：%q", path, short)
			}

			if !strings.Contains(long, "larkaxctl ") {
				t.Errorf("%s help 什么都没打出来", path)
			}
			if !strings.Contains(short, "larkaxctl ") {
				t.Errorf("%s -h 什么都没打出来", path)
			}
			if !docPointerRE.MatchString(long) {
				t.Errorf("%s 的长版末尾没有给出去向（应写「见 docs/cli-reference.md 的「<节名>」一节」）：%q", path, long)
			}
			if docPointerRE.MatchString(short) {
				t.Errorf("%s 的短版里出现了长版的去向：两版装反了吗？%q", path, short)
			}
			if strings.TrimSpace(long) == strings.TrimSpace(short) {
				t.Errorf("%s 的长版与短版逐字相同：分层没有发生", path)
			}

			shortLines := len(strings.Split(strings.TrimSpace(short), "\n"))
			longLines := len(strings.Split(strings.TrimSpace(long), "\n"))
			if shortLines > longLines {
				t.Errorf("%s 的短版比长版还长：两版装反了（短 %d 行，长 %d 行）", path, shortLines, longLines)
			}
		})
	}
}

// TestEveryDerivedPathAnswersHelpFlag 是门二：注册表派生的**每一条**路径
// （命令组与叶子都算）都要认得 `-h`。
//
// 与门一的区别在覆盖面：门一管「有下级的那些」，这条管到叶子。位置参数把 -h
// 摘走的那一类病全在叶子上，门一一条都看不见。
//
// 叶子只要求打得出来，不要求「自己的那一版」：很多叶子与它的父组共用一份短版
// （`channel list -h` 给的就是 channel 的短版，而那份短版里确实写着
// `larkaxctl channel list [--all]`），换一份文案不在本条的范围内。
func TestEveryDerivedPathAnswersHelpFlag(t *testing.T) {
	for _, path := range allPathsDerived(t) {
		t.Run(path, func(t *testing.T) {
			ok, out := runHelpForm(t, path, "-h")
			if !ok {
				t.Fatalf("%s -h 报错了，打出来的是：%q", path, out)
			}
			if !strings.Contains(out, "larkaxctl ") {
				t.Errorf("%s -h 没有打出帮助：%q", path, out)
			}
		})
	}
}
