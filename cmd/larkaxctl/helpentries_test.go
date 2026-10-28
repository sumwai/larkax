package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
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
	if err != nil && !cli.HelpRequested(err) {
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

// helpSaysItself 判断 `larkaxctl <path> help`（以及单层的 `help <path>`）打出来的
// 首行是不是点出了这条路径自己。
//
// 判据与 TestSubGroupShortUsageStaysWithinBudget 的 selfNaming 同源：
// `strings.Contains(首行, "larkaxctl "+path)`。不判 rc——深路径的 `help` 一律 rc=0，
// 包括打错内容的那些，拿 rc 判等于什么都没判。
func helpSaysItself(t *testing.T, path string) bool {
	t.Helper()
	var err error
	out, errOut := captureStreams(t, func() {
		err = dispatch(append(strings.Fields(path), "help"))
	})
	if err != nil {
		return false
	}
	first := out + errOut
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	return strings.Contains(first, "larkaxctl "+path)
}

// TestHelpRejectsDeepPath 钉住 `larkaxctl help <深路径>` 报错，而不是静默丢段。
//
// 这一条要防的是一类**看不出来**的错：`help wallet payout` 曾经把末尾那段扔掉，
// 打出来的是 `help wallet` 那一页——两页逐字节相同，人照着根帮助页末行的写法
// 敲一个深路径，拿到的是父组的说明，而没有任何线索能发现。
//
// 只认一层的代价是这条命令从 rc=0 变成 rc=2。判据落在 rc 上，因为那正是这次
// 变更要钉的东西——「静默」与「报错」在输出上可能只差一行，在退出码上不会。
//
// **不给它做逐层下钻**：那要把整条路径喂回分派器（`help a b` → `a b help`），
// 于是 `help migrate create` 变成 `migrate create help`——而 `help` 在 create
// 那里是合法的迁移名。深路径本来就有自己的一条路：`larkaxctl <深路径> help`。
func TestHelpRejectsDeepPath(t *testing.T) {
	// 从注册表派生，不手抄：手抄的路径名一旦写错，这条用例会去测一条不存在的
	// 命令（那也会 rc=2，于是照样绿）。
	var deep []string
	for _, path := range groupPathsDerived(t) {
		if strings.Contains(path, " ") {
			deep = append(deep, path)
		}
	}
	if len(deep) < 10 {
		t.Fatalf("只派生出 %d 条深路径：注册表扫描坏了，这条用例会安静地通过", len(deep))
	}

	for _, path := range deep {
		t.Run(path, func(t *testing.T) {
			var err error
			out, errOut := captureStreams(t, func() {
				err = dispatch(append([]string{"help"}, strings.Fields(path)...))
			})
			if err == nil {
				t.Fatalf("help %s 应当报参数错，却成功了：%q",
					path, out+errOut)
			}
			if code := cli.ExitCode(err); code != 2 {
				t.Errorf("help %s 的退出码是 %d，想要 2（参数错）：%v", path, code, err)
			}

			// 报错里给的两条出路，**至少一条真的走得通**。
			//
			// 这一条是这条用例的重点。文案里的指引没人验过，正是
			// payout_test.go:852 那条注释用了一年的坑：它说「笼统的 help 只认
			// 第一层」，而没有人跑过那条路。这一次换了个位置——报错文案推荐
			// `larkaxctl <深路径> help`，而 18 条深路径里有 6 条照着敲拿到的是
			// **父组**的说明。
			//
			// 判据是「那条路打出来的首行点出它自己」，不是「rc=0」：深路径的
			// `<路径> help` 一律 rc=0，包括那 6 条打错内容的。
			if !strings.Contains(err.Error(), path+" help") {
				t.Errorf("help %s 的报错没有给出深路径那条写法：%v", path, err)
			}
			parent := strings.Fields(path)[0]
			if !strings.Contains(err.Error(), "larkaxctl help "+parent) {
				t.Errorf("help %s 的报错没有给出父组那条写法：%v", path, err)
			}

			deepWorks := helpSaysItself(t, path)
			parentWorks := helpSaysItself(t, parent)
			if !deepWorks && !parentWorks {
				t.Errorf("help %s 的两条出路都走不通：`%s help` 与 `help %s` "+
					"打出来的都不是自己的说明", path, path, parent)
			}
		})
	}

	// 反向锚：单层路径**不能**被这条判据波及。少了它，把 dispatch 的 help 分支
	// 改成「一律报错」也能让上面的全部通过。
	for _, one := range []string{"wallet", "user", "migrate"} {
		t.Run("单层_"+one, func(t *testing.T) {
			ok, out := runHelpForm(t, one, "help")
			if !ok {
				t.Fatalf("help %s 不该报错，得到：%q", one, out)
			}
			if !strings.Contains(out, "larkaxctl "+one) {
				t.Errorf("help %s 打出来的不是它自己的帮助：%q", one, out)
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

// TestLeadingHelpFlagIgnoresTrailingArgs 钉住「求助旗标打头之后，尾巴上的参数不影响结果」。
//
// 这条恒等式**就是** HelpOnly 那一笔的语义定义：`<路径> -h extra` 与 `<路径> -h`
// 必须给出逐字节相同的 rc、stdout、stderr。改之前它不是恒等式——**27 条命令、
// 54 组（命令 × 两种写法）**上两者不同；其中 8 组（`migrate up/down/status/version`
// × 两种写法）在基线版本上是 rc=1，因为打帮助之前先去连库，连不上就直接失败。
//
// 判据是**两者相同**，不是「rc==0 且输出非空」：后者对「两条路都打错同一份东西」
// 同样成立。这一条问的是「尾巴被忽略了没有」，而忽略的定义就是「结果一样」。
//
// # 进门必须先清 DSN
//
// 这条门在全部路径上真敲命令，理由与 #1280 那次相同：门为了断言「有没有帮助」
// 自己成了这些命令的调用方。清空之后那 8 组 `migrate …` 也在连库之前就把帮助
// 打出来，与其余路径一致。不清的话它们会因为连不上库而红，而红的原因与这条门
// 要问的事无关。
//
// # 反向锚
//
// 光断言「-h extra == -h」在「两者都执行了命令」时也成立。所以另比一组：
// `<路径> extra`（第一个参数不是旗标）必须与 `<路径> -h extra` **不同**——
// 证明这一层真的在看第一个参数，而不是把所有带 extra 的调用都归一了。
func TestLeadingHelpFlagIgnoresTrailingArgs(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")

	paths := allPathsDerived(t)
	if len(paths) < 100 {
		t.Fatalf("只派生出 %d 条路径：扫描器坏了，这条门会安静地通过", len(paths))
	}

	compared := 0
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			fields := strings.Fields(path)
			for _, form := range helpForms {
				withFlag := append(append([]string{}, fields...), form, "extra")
				bare := append(append([]string{}, fields...), form)

				var flagErr, bareErr error
				flagOut, flagErrOut := captureStreams(t, func() { flagErr = dispatch(withFlag) })
				bareOut, bareErrOut := captureStreams(t, func() { bareErr = dispatch(bare) })

				if got, want := processExitCode(flagErr), processExitCode(bareErr); got != want {
					t.Errorf("%s %s extra 的退出码是 %d，而 %s %s 是 %d",
						path, form, got, path, form, want)
				}
				if flagOut != bareOut {
					t.Errorf("%s %s extra 的 stdout 与 %s %s 不同：\n带尾巴：%q\n不带：  %q",
						path, form, path, form, flagOut, bareOut)
				}
				if flagErrOut != bareErrOut {
					t.Errorf("%s %s extra 的 stderr 与 %s %s 不同：\n带尾巴：%q\n不带：  %q",
						path, form, path, form, flagErrOut, bareErrOut)
				}
				compared++

				// 反向锚：**不带旗标**的 `<路径> extra` 必须与 `<路径> -h extra`
				// 不同。少了它，这一层要是把「所有带 extra 的调用都归一成帮助」，
				// 上面那些「相同」照样全绿，而命令其实已经无法执行了。
				//
				// 这里比的是**无旗标**，不是 `extra -h`：位置参数之后跟 `-h` 时，
				// 30/199 条路径仍然打帮助。机制是 `TakeLeadingOperand` 先把位置
				// 参数摘走、剩下的 `-h` 落到叶子的 `fs.Parse` 头上（`flag` 包返回
				// ErrHelp，`exitWithFailure` 把它当成功）。那是各叶子自己的既有
				// 行为，不是这一层该管的事；把它并进反向锚，这条门会红在别人的
				// 地盘上。
				if form != helpForms[0] {
					continue
				}
				var tailErr error
				tailOut, tailErrOut := captureStreams(t, func() {
					tailErr = dispatch(append(append([]string{}, fields...), "extra"))
				})
				sameAsHelp := processExitCode(tailErr) == processExitCode(flagErr) &&
					tailOut == flagOut && tailErrOut == flagErrOut
				if sameAsHelp {
					t.Errorf("%s extra 与 %s -h extra 结果完全相同（rc、stdout、stderr 三者"+
						"都比了）：这一层把所有带 extra 的调用都归一了，那上面那些"+
						"「相同」什么也证明不了", path, path)
				}
			}
		})
	}

	// 对账三件事，各自的失效方式不同，缺一条就有一种「少扫」漏过去：
	//
	//  1. **跑过的路径数**要与独立来源相符。这条挡的是 `allPathsDerived` 自己
	//     少返回几条——只比 `compared` 与 `len(paths)*len(helpForms)` 挡不住，
	//     两个数都来自 paths，截短了会一起变小、仍然相等。
	//  2. **比过的组数**要与 `len(paths) * len(helpForms)` 相符。这条挡的是循环
	//     体少走几趟（`continue` 前漏了累加、内层只取了第一种写法）。
	//  3. 组数不为 0，挡住「两份注册表都空」。
	//
	// 三条都是「期望值不由被测量本身推出」。原先只有第 3 条，它的形状与
	// #1280 修掉的 `checked == 0` 一模一样：只判「非零」等于允许少扫。
	if got, want := len(paths), independentlyCountedPaths(); got != want {
		t.Fatalf("派生出 %d 条路径，而三份注册表展开取并集是 %d 条："+
			"allPathsDerived 少返回了 %d 条，这条门会安静地漏掉它们",
			got, want, want-got)
	}
	if want := len(paths) * len(helpForms); compared != want {
		t.Fatalf("只比了 %d 组，应有 %d 组（%d 条路径 × %d 种写法）："+
			"某一趟循环没有跑到", compared, want, len(paths), len(helpForms))
	}
	if compared == 0 {
		t.Fatal("一组都没比到：派生链断了，这条门什么也没证明")
	}
}

// independentlyCountedPaths 是 allPathsDerived 的**对账来源**：读同样的三份
// 注册表，在这里再展开一次并去重。
//
// 它与 allPathsDerived 刻意是两份代码。合成一份就只剩「自己与自己相等」——
// 那边少返回几条时，这边跟着少，对账永远成立。两份都读注册表，所以增删命令
// 两边一起动，不需要维护；只有 allPathsDerived 的**口径**变了才要改这里。
func independentlyCountedPaths() int {
	seen := map[string]bool{}
	for _, g := range commandGroups {
		for _, c := range g.commands {
			seen[c.name] = true
		}
	}
	for _, c := range legacyCommands {
		seen[c.name] = true
	}
	for path, verbs := range completionActionTable {
		seen[path] = true
		for _, verb := range verbs {
			seen[path+" "+verb] = true
		}
	}
	return len(seen)
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
