package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 这一组门禁管的是**「同一类命令说出同一句话」**：一个类里的约定是不是只落在
// 点名的那两条命令上，其余同类命令悄悄各写各的。
//
// 判据一律按**渲染路径 / 调用点**计数，不按函数名逐条罗列：
//
//   - 「扫到什么」来自 AST，不是人在这份清单里抄一遍。新加一条同类命令而没跟上
//     约定时，源码里就多出一个没登记的调用点，这里当场红。
//   - 登记表里的条目**少一个也红**。少了说明某条路径不再走共享渲染器——那不是
//     一次无害的重构，是把这条路径从「数得出来」变成「数不出来」。
//
// 只扫非测试文件：测试自己构造输出、不面向使用者，把它算进来会让「用例里也调了
// 一次」看起来像多了一条命令。
//
// 已知边界（都不是这里能判的）：
//
//   - 只认字面量与被调名字符串。间接调用（把 printStatusLines 存进变量再调、
//     或经 reflect 调）扫不到。
//   - 空状态那一档只钉「暂无」的一处出处，**不禁止**别的措辞。插件里那句
//     「它还没有生效」说的不是空清单，禁掉「还没有」这个词会误伤它。
//   - 用法文本（`*Usage` 常量）在文案与符号两道门上豁免：它举例子解释输出
//     长什么样，自己不渲染。豁免按常量名给，见 usageTextLiterals。

// statusRenderPaths 是诊断体检类命令的单行状态渲染器的调用点。
//
// 这一组共用 printStatusLines（见 style.go）：状态符、按最宽标题对齐、默认一屏
// 只留结论、细节挪进 -v。多一个调用点说明有新命令做了体检（它得先想清楚自己是
// 不是该类）；少一个说明有命令不再走这条路，两种都要人来看一眼。
var statusRenderPaths = []string{
	"printDoctorReport",       // doctor
	"printLedgerVerifyReport", // ledger verify
	"printPartitionCheck",     // partition check
	"printVerifyReport",       // secret rotate --verify
}

// tableRenderPaths 是走了 tableWriter（固定列宽的清单表）的调用点。
//
// 「清单表」这一类指的是**会随数据变长的那种清单**：表头加粗、主键加粗、其余
// 不上样式，列按显示宽度对齐。doctor 的 -v 明细表也在里面：它同样是清单表，
// 只是外壳是诊断报告。
var tableRenderPaths = []string{
	"doctorTable.lines",   // doctor -v 的逐项明细
	"printAccountList",    // account list
	"printAdjustedWallet", // wallet adjust 的回显：一栏一行，与 user update 同一个形状
	"printAliasList",
	"printAllowlists", // snapshot allowlist
	"printCalendarList",
	"printChannelAdapters",       // channel adapters
	"printChannelDetail",         // channel get
	"printChannelHealthAccounts", // channel health --id 的逐账号明细
	"printChannelHealthDetail",   // channel health --id 的读数
	"printChannelHealthHours",    // channel health --id 的逐小时明细
	"printChannelHealthList",     // channel health 的汇总表
	"printChannelList",
	"printChannelUpstreamModels", // channel upstream-models
	"printConsentList",           // consent list
	"printCostTable",
	"printDeletionReceipt", // user delete 的回执：一栏一行
	"printKeyDetail",       // apikey get
	"printKeyList",
	"printLedgerBalances", // ledger balance --all
	"printLedgerRuns",     // ledger runs
	"printModelAliasList", // model alias list
	"printModelDetail",
	"printModelList",
	"printModelTrace",    // model trace
	"printPayment",       // wallet payment 的回显：一栏一行，与 wallet payout 同一个形状
	"printPayout",        // wallet payout 的回显：一栏一行，与 wallet adjust 同一个形状
	"printPeakList",      // price peak list
	"printPlanDetail",    // plan get
	"printPlanList",      // plan list
	"printPluginDetail",  // plugin get
	"printPluginKVEntry", // plugin kv get
	"printPluginList",
	"printRuleTable",
	"printSettingsDetail",    // settings get
	"printSnapshotAccessLog", // snapshot access-log
	"printTierList",          // price tier list
	"printUsageEvents",       // usage events
	"printUsageRequests",     // usage requests
	"printUsageSummary",      // usage summary
	"printUserDetail",        // user get
	"printUserList",          // user list
	"printUserPlans",         // plan subscriptions 与 plan key show
	"printUserUpdate",        // user update 的改动回显：一栏一行，与 get 那一片同一个形状
	"printWalletList",        // wallet list
}

// emptyListPaths 是清单空状态文案的调用点。
//
// 这一句（emptyListNote）把「这一栏是空的」与「下一步敲哪条命令」说在一起。
// 没有「添加」这个动作的清单**不该**登记进来——给不出可执行的下一步时硬造一个
// 命令名，比留一句「现在是空的」更坏（ledger balance --all 就是这一类）。
var emptyListPaths = []string{
	"checkDoctorWallets", // doctor 的钱包体检：一个用户都没有
	"printAccountList",   // account list：一个账号都没有
	"printAliasList",
	"printCalendarList",
	"printChannelHealthList", // channel health：一条渠道都没有
	"printChannelList",
	"printChannelUpstreamModels", // channel upstream-models：从没同步过
	"printConsentList",           // consent list
	"printCostList",
	"printKeyList",
	"printModelAliasList", // model alias list
	"printModelList",
	"printPeakList", // price peak list
	"printPlanList",
	"printPluginList",
	"printRuleList",
	"printTierList",   // price tier list
	"printUserList",   // user list
	"printUserPlans",  // plan subscriptions 与 plan key show
	"printWalletList", // wallet list
}

// TestEveryStatusRenderPathIsRegistered 钉住诊断体检类的调用点集合。
func TestEveryStatusRenderPathIsRegistered(t *testing.T) {
	checkCallSiteRegistry(t, "PrintStatusLines", statusRenderPaths)
}

// TestEveryTableRenderPathIsRegistered 钉住清单表的调用点集合。
func TestEveryTableRenderPathIsRegistered(t *testing.T) {
	checkCallSiteRegistry(t, "NewTableWriter", tableRenderPaths)
}

// TestEveryEmptyListNoteIsRegistered 钉住空状态文案的调用点集合。
func TestEveryEmptyListNoteIsRegistered(t *testing.T) {
	checkCallSiteRegistry(t, "EmptyListNote", emptyListPaths)
}

// TestEmptyStateWordingHasASingleSource 钉住「暂无」只有一个出处。
//
// 空清单的文案原先六条命令各写各的（「还没有渠道。」「目录里还没有模型。」
// 「没有 API Key。」），读的人分不清它们是同一件事还是三个不同的状态。收成一句
// 之后，**模板只许有一份**：谁再自己拼一句「暂无…」，这一栏里就会出现两种说法，
// 而两种说法并存时没人知道该以哪一句为准。
func TestEmptyStateWordingHasASingleSource(t *testing.T) {
	const note = "暂无"

	var hits []string
	forEachNonTestFileBoth(t, func(name string, file *ast.File) {
		exempt := usageTextLiterals(file)
		forEachStringLiteral(file, func(lit *ast.BasicLit, value string) {
			if exempt[lit.Pos()] {
				return
			}
			// 比的是**解释之后的值**：写成 "✓" 一样是那个符号，而源码
			// 文本里找不到它。
			if strings.Contains(value, note) {
				hits = append(hits, name+": "+lit.Value)
			}
		})
	})

	if len(hits) == 1 && strings.HasPrefix(hits[0], frameworkStylePath+":") {
		return
	}
	t.Errorf("「%s」在非测试源码的字面量里出现 %d 次，期望正好 1 次、且在 %s 里：\n%s\n"+
		"空清单走 EmptyListNote，别自己拼一句。", note, len(hits), frameworkStylePath,
		strings.Join(hits, "\n"))
}

// TestOnlyThePalettePaints 钉住「转义序列只在 style.go 拼得出来」。
//
// 比的是**解释之后的字面量值**而不是源码文本：写成 "\u001b[1m" 一样能拼出控制码，
// 而文本匹配看不见它。
//
// 与 TestNoStyleWhenNotATerminal 不重复：那条走的是「非终端下不许出现转义序列」
// 这一条行为，它只覆盖跑到的那几条渲染路径；这一条覆盖所有文件的字面量，包括
// 还没有用例走过的路径。两处都留着。
func TestOnlyThePalettePaints(t *testing.T) {
	var hits []string
	forEachNonTestFileBoth(t, func(name string, file *ast.File) {
		forEachStringLiteral(file, func(lit *ast.BasicLit, value string) {
			if strings.Contains(value, "\x1b[") {
				hits = append(hits, name+": "+lit.Value)
			}
		})
	})

	if len(hits) == 0 {
		t.Fatal("一处转义序列都没扫到：palette 被拆了，或者这个扫描器坏了——两种都不算通过")
	}
	for _, hit := range hits {
		if !strings.HasPrefix(hit, frameworkStylePath+":") {
			t.Errorf("转义序列出现在 %s 之外：%s\n上色只有 palette 一个出口，"+
				"调用点自己拼控制码的地方会绕过 NO_COLOR 与重定向判断。",
				frameworkStylePath, hit)
		}
	}
}

// TestStatusGlyphsHaveASingleSource 钉住 ✓ 与 ✗ 只从 glyphKind.symbol 出来。
//
// 状态符与它的颜色是同一档意思的两半（见 cli 包的 style.go 的 glyphKind）：谁在调用点
// 直接写一个 ✓，那一处的颜色就与别的处不在一条路径上，而「这一屏各档状态符是
// 什么」正是诊断体检类命令唯一的共同语言。
//
// 用法文本（包内那几个 `*Usage` 常量）豁免：doctor --help 要举一个 ✓ 出来解释
// 这一档是什么，那是给读的人看的例子，不打印任何一行状态。豁免按**常量名**给，
// 是明写的，不是按「长得像帮助文本」猜的。
func TestStatusGlyphsHaveASingleSource(t *testing.T) {
	glyphs := []string{"✓", "✗"}

	for _, glyph := range glyphs {
		var hits []string
		forEachNonTestFileBoth(t, func(name string, file *ast.File) {
			exempt := usageTextLiterals(file)
			forEachStringLiteral(file, func(lit *ast.BasicLit, value string) {
				if exempt[lit.Pos()] {
					return
				}
				if strings.Contains(value, glyph) {
					hits = append(hits, name+": "+lit.Value)
				}
			})
		})
		if len(hits) != 1 || !strings.HasPrefix(hits[0], frameworkStylePath+":") {
			t.Errorf("%s 在非测试源码里出现 %d 次，期望正好 1 次、且在 %s 里：%q",
				glyph, len(hits), frameworkStylePath, hits)
		}
	}
}

// TestNoParenthesisedIDInHumanOutput 钉住「实体只点一次身份」。
//
// 输出里曾经出现过「用户 alice(id=7)」这种把 ID 挂在名字后面的写法：同一行里
// 名字已经认出这条记录是谁，括号里的 ID 是为读的人多准备的一份，而按名字读的
// 人每次都要跳过它。ID 该出现的地方是它自己的那一栏（`id=7` 这种独立字段），
// 用在脚本里接下去。
func TestNoParenthesisedIDInHumanOutput(t *testing.T) {
	// 两个写法都扫：全角括号是中文句子里更自然的那一种，只扫半角会漏掉它。
	brackets := []string{"(id=", "（id="}

	for _, opening := range brackets {
		var hits []string
		forEachNonTestFileBoth(t, func(name string, file *ast.File) {
			forEachStringLiteral(file, func(lit *ast.BasicLit, value string) {
				if strings.Contains(value, opening) {
					hits = append(hits, name+": "+lit.Value)
				}
			})
		})
		if len(hits) != 0 {
			t.Errorf("输出文案里有 %q：\n%s\n实体只点一次身份——名字认得出就写名字，"+
				"ID 单独成一个 id= 字段。", opening, strings.Join(hits, "\n"))
		}
	}
}

// ── 扫描器 ──────────────────────────────────────────────────────────────

// checkCallSiteRegistry 比对「扫出来的调用点」与登记表。
//
// 两侧都要比：多出来的调用点（新命令没登记）与少掉的（登记了但没人调）都是
// 要人看一眼的事，而只比「登记表的每一项都还在」会让它退化成一张只增不减的清单。
func checkCallSiteRegistry(t *testing.T, callee string, want []string) {
	t.Helper()

	if !slices.IsSorted(want) {
		t.Fatalf("登记表没排序，比对结果不可读：%q", want)
	}
	got := nonTestCallSites(t, callee)
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("%s 的调用点与登记表不一致：\n扫到：%q\n登记：%q\n"+
		"新加一条同类命令：先想清楚它属不属于这一类，属于就把调用点登记进来、"+
		"并补上它在那张样式用例表里的条目；这一条路径改掉了，登记项要跟着删。",
		callee, got, want)
}

// nonTestCallSites 返回包内非测试源码里每个 callee 调用点所在的函数名。
//
// 调用点按所在函数名排序；同一个函数里调两次就出现两次——重复也是信息，
// 它说明这一条路径里有两张表（这一个函数里的两处渲染要一起看）。
func nonTestCallSites(t *testing.T, callee string) []string {
	t.Helper()

	var found []string
	forEachNonTestFileBoth(t, func(name string, file *ast.File) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			where := declaredName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if calledName(call.Fun) == callee {
					found = append(found, where)
				}
				return true
			})
		}
	})

	if len(found) == 0 {
		// 「约定被废掉了」与「这个扫描器坏了」在输出上长得一样（见 LESSONS.md
		// 第 16 条），两种都不能算通过。
		t.Fatalf("一处 %s 调用点都没扫到：要么约定废了，要么扫描器坏了", callee)
	}
	sort.Strings(found)
	return found
}

// forEachNonTestFile 把包目录下每个非测试的 .go 文件交给 visit。
//
// 包目录就是测试的工作目录（go test 每个包在自己的目录里跑），所以这里直接读
// "."——不去找模块根：找到了也要再猜一次包的相对路径。
func forEachNonTestFile(t *testing.T, visit func(name string, file *ast.File)) {
	t.Helper()
	forEachNonTestFileIn(t, ".", visit)
}

// forEachFrameworkFile 把框架包（internal/cli）下每个非测试的 .go 文件交给 visit。
//
// 框架那一层（样式、表格、格式、退出码）已经搬进 internal/cli，但它仍然是
// 「输出只有一个出口」这条约定的一部分：上色只在 style.go 拼、状态符只有一处
// 出处、空状态文案只有一句。只扫 main 的话，这些字面量一条都扫不到——而
// 「扫不到」与「全都对」在输出上长得一样（见 LESSONS.md 第 16 条）。
func forEachFrameworkFile(t *testing.T, visit func(name string, file *ast.File)) {
	t.Helper()
	forEachNonTestFileIn(t, frameworkDir, visit)
}

// frameworkDir 是框架包的相对路径（测试的工作目录是 main 包目录）。
var frameworkDir = filepath.Join("internal", "cli")

// frameworkStylePath 是上色那一份的路径。两条判据（转义序列只有一处出处、
// 空状态文案只有一句）都钉在它上面——搬进 internal/cli 之前它就是 style.go。
const frameworkStylePath = "internal/cli/style.go"

// forEachNonTestFileBoth 扫 main 包与框架包两处。
//
// 输出层的约定横跨两处：风格与文案在 internal/cli，调用点在 main。只扫一处时
// 「另一处违规」与「全都对」在输出上长得一样。
func forEachNonTestFileBoth(t *testing.T, visit func(name string, file *ast.File)) {
	t.Helper()
	forEachNonTestFile(t, visit)
	forEachFrameworkFile(t, visit)
}

// forEachNonTestFileIn 是上面两个的共用部分：扫一个目录里的非测试 .go 文件。
func forEachNonTestFileIn(t *testing.T, dir string, visit func(name string, file *ast.File)) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读包目录 %s 失败：%v", dir, err)
	}

	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, rel, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", rel, err)
		}
		scanned++
		visit(rel, file)
	}

	if scanned == 0 {
		t.Fatal("一个非测试的 .go 文件都没扫到：这个扫描器坏了，不是通过")
	}
}

// usageTextLiterals 返回被 `*Usage` 常量包住的字符串字面量的位置。
//
// 用法文本是给人看的说明书，与渲染路径不是一回事（见 TestStatusGlyphsHaveASingleSource）。
func usageTextLiterals(file *ast.File) map[token.Pos]bool {
	exempt := make(map[token.Pos]bool)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok || !namesEndWith(values.Names, "Usage") {
				continue
			}
			for _, v := range values.Values {
				ast.Inspect(v, func(n ast.Node) bool {
					if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						exempt[lit.Pos()] = true
					}
					return true
				})
			}
		}
	}
	return exempt
}

// namesEndWith 判断一组标识符名是不是都以 suffix 结尾。
func namesEndWith(names []*ast.Ident, suffix string) bool {
	for _, name := range names {
		if !strings.HasSuffix(name.Name, suffix) {
			return false
		}
	}
	return len(names) > 0
}

// forEachStringLiteral 把文件里每个能解释出值的字符串字面量交给 visit。
//
// 传两份：value 是解释之后的值（`\x1b` 已经是一个字节），字面量本身带着源码里的
// 写法与位置——报错时照着它去找那一行，按位置排除豁免的那些也是靠它。
func forEachStringLiteral(file *ast.File, visit func(lit *ast.BasicLit, value string)) {
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			// 解析不了的字面量（不可能出现在能编译的代码里）跳过：这里不判
			// 「哪一处写坏了」，那是编译器的事。
			return true
		}
		visit(lit, value)
		return true
	})
}

// declaredName 返回函数声明在调用点清单里的名字：自由函数是函数名，
// 方法是「类型.方法」——`lines` 这种名字在多个类型上都可能出现，带上接收者
// 才指得清是哪一个。
func declaredName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// calledName 返回调用表达式的被调名字：`f(...)` 与 `pkg.F(...)`、`x.M(...)`
// 都取最后那一段。
func calledName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
