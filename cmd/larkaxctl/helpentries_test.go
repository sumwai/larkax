package main

import (
	"slices"
	"strings"
	"testing"
)

// ── 帮助入口：清单从注册表派生，不手抄 ──────────────────────────────────
//
// 这一组门替掉原来那张手抄的 subGroupHelpCases。手抄的代价是实测过的：
// completionActionTable 里带下级的键有 18 条，那张表只抄了 7 条，漏掉的 11 条里
// 含 4 条帮助入口根本没接的命令，而 CI 全绿——「测试表不派生自注册表，就必然会漏」。
//
// 清单唯一的来源是 completionActionTable：它已经在做「路径 → 动作常量」的接线
// （补全要用同一份）。但**单靠它不够**：删掉表里一行，那整棵子树就退出管辖，
// 而失败模式恰好就是这几道门要治的病。所以下面另有一条
// TestEveryActionConstantHasAPath 把 41 个 xxxActions 常量与表键双向对上——
// 少一行、多一行、改名成对不上的写法，都会在那里红。

// TestEveryActionConstantHasAPath 是派生源自身的门：41 个 xxxActions 常量与
// completionActionTable 的键必须双向一一对上。
//
// 上面两道门都从 completionActionTable 派生路径。这带来一个洞：**表里少一行，
// 那棵子树就整棵退出管辖**，而漏掉正是这几道门要治的病本身。实测过删一行
// （外加把它对应的 help 入口与 -h 守卫一起删掉），整套 go test 全绿——门看不见
// 它。下面这条补上另一半：常量还在、表里却没有对应路径，就报出来。
//
// 映射规则是路径按空格/短横分段后拼成驼峰（`user key plans` → userKeyPlans），
// 与仓里的命名一致。**这里刻意不做模糊匹配**：对不上就报，正好逼着新加的命令
// 同时补常量与表项两处。
func TestEveryActionConstantHasAPath(t *testing.T) {
	consts := actionConstants(t) // 与 TestActionConstantsMatchDispatch 共用同一份 AST 扫描
	if len(consts) < 30 {
		t.Fatalf("只扫到 %d 个 xxxActions 常量：扫描器坏了，这条门会安静地通过", len(consts))
	}

	byCamel := make(map[string]string, len(completionActionTable))
	for path := range completionActionTable {
		byCamel[strings.ToLower(pathToCamel(path))] = path
	}

	found := 0
	for name := range consts {
		base := strings.ToLower(strings.TrimSuffix(name, "Actions"))
		path, ok := byCamel[base]
		if !ok {
			t.Errorf("%s 没有对应的路径：completionActionTable 里少了一行？"+
				"那棵子树会整棵退出帮助入口门的管辖", name)
			continue
		}
		found++

		// 反过来也核一遍：表里那一条挂的确实是这个常量，而不是复制粘贴到别人的。
		if got := completionActionTable[path]; !sameStrings(got, consts[name]) {
			t.Errorf("%s 的动作是 %v，但 %s 常量是 %v：表项挂错了常量",
				path, got, name, consts[name])
		}
	}
	if found == 0 {
		t.Fatal("一个常量都没对上路径：这条门会安静地通过，所以先在这里拦住")
	}
}

// pathToCamel 把命令路径转成仓里 xxxActions 常量的驼峰形式。
func pathToCamel(path string) string {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == ' ' || r == '-' })
	out := parts[0]
	for _, part := range parts[1:] {
		out += strings.ToUpper(part[:1]) + part[1:]
	}
	return out
}

// sameStrings 比两份词表，顺序也要一致——补全顺序是脚本里看得见的。
func sameStrings(a, b []string) bool { return slices.Equal(a, b) }

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
//     plans`、`user plan order`）；② stdout 上同样与父组短版逐字相同，只是
//     `runLegacyUserPath` 在返回后另往 stderr 补一行搬迁提示（2 条：`plan key`、
//     `plan order`）——判据要按**单条流**比，把两条流拼起来看会以为短版里多了一行；
//     ③ 顶层别名，短版本来就属于另一条完整路径（2 条：`calendar` 打的是
//     `price calendar` 的，`idempotency` 打的是 `maintenance idempotency` 的）。
//     三类都是「一条文案一处来源」的直接结果，为它们各写一份副本才是要避免的。
//     门只要求「打得出来且是那一版」。
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
			// 四种写法都要认，与 `flag` 包一致。只测 -h 会漏掉另一种：
			// `-help` 与 `--h` 在走 flag 集的叶子上一直是通的，靠位置参数
			// 摘取的那些若不认它们，同类命令会出现两种结果。
			for _, form := range helpForms {
				ok, out := runHelpForm(t, path, form)
				if !ok {
					t.Fatalf("%s %s 报错了，打出来的是：%q", path, form, out)
				}
				if !strings.Contains(out, "larkaxctl ") {
					t.Errorf("%s %s 没有打出帮助：%q", path, form, out)
				}
			}
		})
	}
}

// helpForms 是这道门要测的写法。**只有这两种**：`flag` 包虽然也认 `-help` 与
// `--h`，但基线版本对它们各命令不一致（`plan -help` 退出码 0、`wallet -help` 却
// 是 2），统一那件事不在本条的范围内，测了会红。
var helpForms = []string{"-h", "--help"}

// 二级组短版的两条租约，**只许减不许增**：每一条都是一句「现在确实不合规，
// 但不在这一条分支的范围里」。
//
// 上面两道门只管「打得出来、是那一版」，不量短版本身合不合规矩。手抄表退役时
// 这两件事一起消失了：一级命令仍由 checkShortUsage 对着注册表跑到，二级组没有
// 了。不量就等于放它长回去——所以在这里补回来，只是把**已经超了的那一条**记成
// 租约，而不是干脆不量。
var (
	// shortUsageOverBudget 是短版超过 shortUsageMaxLines 的二级组。
	// `wallet payout` 现在 25 行，是先于这道门的既有问题；改文案不在本条范围内。
	shortUsageOverBudget = map[string]bool{"wallet payout": true}

	// shortUsageNotSelfNaming 是短版首行不是自己的名字的二级组，分三类：
	// 与父组短版逐字相同 4 条、父组短版 + 搬迁提示 2 条、顶层别名 2 条（见门一的
	// 注释）。三类都是「一条文案一处来源」的直接结果，各写一份副本才是要避免的。
	shortUsageNotSelfNaming = map[string]bool{
		"admin token": true, "user delete": true, "user key plans": true, "user plan order": true,
		"plan key": true, "plan order": true,
		"calendar": true, "idempotency": true,
	}
)

// TestSubGroupShortUsageStaysWithinBudget 把一级命令已有的两条判据补到二级组：
// 短版不超过一屏、首行点出自己是谁。两份租约之外的组必须合规。
//
// 租约是**双向**的：名单里那条若已经合规，也报错——过期租约会让下一轮把一个
// 早就修好的缺陷当成还没修，清单也会慢慢失真。
func TestSubGroupShortUsageStaysWithinBudget(t *testing.T) {
	for _, path := range groupPathsDerived(t) {
		t.Run(path, func(t *testing.T) {
			ok, text := runHelpForm(t, path, "-h")
			if !ok {
				t.Fatalf("%s -h 报错了，打出来的是：%q", path, text)
			}
			trimmed := strings.TrimSpace(text)
			if trimmed == "" {
				t.Fatalf("%s 没有短版：`-h` 什么都打不出来", path)
			}
			lines := strings.Split(trimmed, "\n")

			if len(lines) > shortUsageMaxLines {
				if !shortUsageOverBudget[path] {
					t.Errorf("%s 的短版 %d 行，超过一屏的 %d 行", path, len(lines), shortUsageMaxLines)
				}
			} else if shortUsageOverBudget[path] {
				t.Errorf("%s 的短版已在 %d 行以内，把它从 shortUsageOverBudget 里删掉", path, shortUsageMaxLines)
			}

			selfNaming := strings.Contains(lines[0], "larkaxctl "+path)
			switch {
			case !selfNaming && !shortUsageNotSelfNaming[path]:
				t.Errorf("%s 的短版首行没有点出这条命令：%q", path, lines[0])
			case selfNaming && shortUsageNotSelfNaming[path]:
				t.Errorf("%s 的短版首行已经点出自己，把它从 shortUsageNotSelfNaming 里删掉", path)
			}
		})
	}
}
