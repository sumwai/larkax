package main

import (
	"flag"
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// updateCompletionGoldens 重写 testdata 下的 zsh / fish 快照。
//
//	go test ./cmd/larkaxctl/ -run TestCompletionScriptsAreStable -update
var updateCompletionGoldens = flag.Bool("update", false, "重写 testdata 下的补全快照")

// ── 三 shell 可解析 ────────────────────────────────────────────────────
//
// bash 一定在：本仓库的脚本都是它写的，`bash -n` 是真语法检查。
// zsh 与 fish 不一定装着——本机就没有。没有的时候退化成**逐字快照比对**
// （testdata/completion.{zsh,fish}）：它证明输出没变，不证明那个 shell 认它。
// 「没被真 shell 执行」这件事记在用例日志里，不写成 Skip——跳过与通过
// 在输出里长得一样（见 LESSONS.md 第 16 条）。

// TestCompletionScriptsParse 断言三种脚本各自能被解析。
func TestCompletionScriptsParse(t *testing.T) {
	data := buildCompletionData()
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			script := generateCompletionScript(t, data, shell)
			checkCompletionGolden(t, shell, script)

			args := map[string][]string{
				"bash": {"-n"},
				"zsh":  {"-n"},
				"fish": {"--no-execute"},
			}[shell]
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Logf("这一格没被真 shell 执行：环境里没有 %s，只做了逐字快照比对", shell)
				return
			}
			file := filepath.Join(t.TempDir(), "completion."+shell)
			if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
				t.Fatalf("写临时脚本失败：%v", writeErr)
			}
			if out, runErr := exec.CommandContext(t.Context(), path, append(args, file)...).CombinedOutput(); runErr != nil {
				t.Fatalf("%s 语法检查失败：%v\n%s", shell, runErr, out)
			}
		})
	}
}

// generateCompletionScript 按 shell 生成一份脚本，与 runCompletion 走同一个生成器。
func generateCompletionScript(t *testing.T, data completionData, shell string) string {
	t.Helper()
	switch shell {
	case "bash":
		return bashCompletionScript(data)
	case "zsh":
		return zshCompletionScript(data)
	case "fish":
		return fishCompletionScript(data)
	}
	t.Fatalf("没有 %q 的生成器", shell)
	return ""
}

// checkCompletionGolden 与 testdata 下的快照逐字节比对。
func checkCompletionGolden(t *testing.T, shell, script string) {
	t.Helper()
	if shell == "bash" {
		// bash 有真语法检查，不再留一份快照：两份都在时，改生成器要记得改两处，
		// 而忘了改快照的那一次会把「输出变了」报成语法问题。
		return
	}
	path := filepath.Join("testdata", "completion."+shell)
	if *updateCompletionGoldens {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("建 testdata 失败：%v", err)
		}
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatalf("写快照失败：%v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到快照 %s（用 -update 生成）：%v", path, err)
	}
	if script != string(want) {
		t.Errorf("%s 脚本与快照 %s 不一致：生成器改过就带 -update 重跑一次", shell, path)
	}
}

// ── 补全集合与注册表对拍 ──────────────────────────────────────────────

// TestCompletionListsExactlyRegisteredCommands 断言三种脚本补出来的一级命令
// 就是注册表里的那一份（分组顺序 + 旧路径），不手抄名单。
func TestCompletionListsExactlyRegisteredCommands(t *testing.T) {
	want := make([]string, 0, 32)
	for _, g := range commandGroups {
		for _, c := range g.commands {
			want = append(want, c.name)
		}
	}
	for _, c := range legacyCommands {
		want = append(want, c.name)
	}

	data := buildCompletionData()
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			script := generateCompletionScript(t, data, shell)
			got := topLevelWords(t, shell, script)
			if !slices.Equal(got, want) {
				t.Errorf("脚本里的一级命令 = %v，注册表里的是 %v", got, want)
			}
		})
	}
}

// topLevelWords 从生成出来的脚本里抠出一级命令。
//
// 三种 shell 的写法各不同，但都在同一行里列出全部命令名：认那一行的格式，
// 而不是去读 Go 里的数据——这一条要证明的正是「脚本里有」。
func topLevelWords(t *testing.T, shell, script string) []string {
	t.Helper()
	var re *regexp.Regexp
	switch shell {
	case "bash":
		re = regexp.MustCompile(`(?m)^\t\t""\) COMPREPLY=\( \$\(compgen -W "([^"]*)"`)
	case "zsh":
		re = regexp.MustCompile(`(?m)^\t\t""\) compadd -- ([^;]*);`)
	case "fish":
		re = regexp.MustCompile(`(?m)^complete -c larkaxctl -n '__fish_use_subcommand' -a '([^']*)'`)
	}
	matches := re.FindAllStringSubmatch(script, -1)
	if len(matches) == 0 {
		t.Fatalf("%s 脚本里找不到一级命令那一行：生成器的格式变了，或者脚本坏了", shell)
	}
	if shell == "fish" {
		// fish 一条命令一行，逐行收集。
		var names []string
		for _, m := range matches {
			names = append(names, m[1])
		}
		return names
	}
	return strings.Fields(matches[0][1])
}

// TestCompletionIncludesItself 断言 completion 也在一级命令里。
func TestCompletionIncludesItself(t *testing.T) {
	data := buildCompletionData()
	for _, shell := range completionShells {
		got := topLevelWords(t, shell, generateCompletionScript(t, data, shell))
		if !slices.Contains(got, "completion") {
			t.Errorf("%s 的补全里没有 completion 自己", shell)
		}
	}
}

// TestCompletionActionsMatchConstants 断言脚本里每个命令路径的动词就是它的
// 动作常量，逐项相等。
func TestCompletionActionsMatchConstants(t *testing.T) {
	data := buildCompletionData()
	script := bashCompletionScript(data)
	for path, want := range completionActionTable {
		got := caseWords(t, script, path)
		if !slices.Equal(got, want) {
			t.Errorf("%s 补出来的动词 = %v，动作常量是 %v", path, got, want)
		}
	}
}

// caseWords 抠出 bash 脚本里 `"path") COMPREPLY=( ... )` 那一行的词表。
func caseWords(t *testing.T, script, path string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\t\t"` + regexp.QuoteMeta(path) + `"\) COMPREPLY=\( \$\(compgen -W "([^"]*)"`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("脚本里找不到路径 %q 那一行", path)
	}
	return strings.Fields(m[1])
}

// TestCompletionOptionsComeFromFlagSets 断言长选项来自命令自己的 flag 集：
// 几个只存在于注册代码里的选项要在，而旧名与短选项不在。
func TestCompletionOptionsComeFromFlagSets(t *testing.T) {
	data := buildCompletionData()
	options := completionOptionNames(data, "channel create")
	for _, want := range []string{"--credential-stdin", "--credential-env", "--base-url"} {
		if !slices.Contains(options, want) {
			t.Errorf("channel create 的补全里没有 %s：%v", want, options)
		}
	}
	for _, unwanted := range []string{"--owner", "--stdin", "--env"} {
		if slices.Contains(options, unwanted) {
			t.Errorf("channel create 的补全里出现了旧名 %s：旧名不占 Tab 的位置", unwanted)
		}
	}
}

// TestCompletionWiresEnums 断言已接线的枚举真的出现在生成物里，取值按来源顺序。
//
// 期望值一律写字面量，不从 doctorEndpointNames() / formatNames() 拼：拼出来的
// 期望值会跟着来源一起变，钉不住「生成物里出现的是这一串」。取值顺序也钉住——
// 三种 shell 都按给的顺序补，只比「两个词都在」等于没钉。
func TestCompletionWiresEnums(t *testing.T) {
	cases := map[string][]string{
		"bash": {
			"\t\t\"doctor|--endpoint\")\n\t\t\tCOMPREPLY=( $(compgen -W \"chat messages responses\" -- \"$cur\") )",
			"\t\t*\"|--format\")\n\t\t\tCOMPREPLY=( $(compgen -W \"table json\" -- \"$cur\") )",
		},
		"zsh": {
			"\t\t\"doctor|--endpoint\") compadd -- chat messages responses; return ;;",
			"\t\t*\"|--format\") compadd -- table json; return ;;",
		},
		"fish": {
			"complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l endpoint -x -a 'chat messages responses'",
			"complete -c larkaxctl -l format -x -a 'table json'",
		},
	}
	data := buildCompletionData()
	for _, shell := range completionShells {
		want := cases[shell]
		if len(want) == 0 {
			t.Fatalf("没有 %q 的期望片段", shell)
		}
		script := generateCompletionScript(t, data, shell)
		for _, fragment := range want {
			if !strings.Contains(script, fragment) {
				t.Errorf("%s 脚本里没有这一段：%q", shell, fragment)
			}
		}
		// 短拼法不占 Tab 的位置：`-o` / `--o` 不该有取值分支。
		for _, short := range []string{"|-o)", "|--o)"} {
			if strings.Contains(script, short) {
				t.Errorf("%s 脚本里出现了短拼法的取值分支 %q：短选项不占 Tab 的位置", shell, short)
			}
		}
		if regexp.MustCompile(`(?m)^complete -c larkaxctl .*-l o .*-x`).MatchString(script) {
			t.Errorf("%s 脚本里给短拼法 -o 挂了取值：短选项不占 Tab 的位置", shell)
		}
	}
}

// TestBashCompletionBehavior 在真 bash 里跑一遍生成出来的补全函数。
//
// 只比生成物里的字面串挡不住 case 模式里的元字符：`|` 在 case 里是多模式
// 分隔符，`*|--format` 实际是 `*` 与 `--format` 两条模式，而 `*` 匹配任意键，
// 于是每个 Tab 都返回 format 的取值、后面的分支全被截断。这一条按行为钉：
// source 之后直接调 _larkaxctl，看 COMPREPLY 里到底是什么。
func TestBashCompletionBehavior(t *testing.T) {
	script := bashCompletionScript(buildCompletionData())

	t.Run("--format 补取值", func(t *testing.T) {
		got := runBashCompletion(t, script, []string{"larkaxctl", "--format", ""})
		if want := []string{"table", "json"}; !slices.Equal(got, want) {
			t.Errorf("COMPREPLY = %v，期望 %v", got, want)
		}
	})

	// 下面三格只要不是 format 的取值：顶层补的是命令、--id 与 -o 后面没有
	// 枚举可取。旧生成器在这里全返回 table / json。
	noFormatValues := func(t *testing.T, got []string) {
		t.Helper()
		for _, unwanted := range []string{"table", "json"} {
			if slices.Contains(got, unwanted) {
				t.Errorf("COMPREPLY = %v，不该出现 %q：通配分支把别的键也截断了", got, unwanted)
			}
		}
	}

	t.Run("顶层补命令", func(t *testing.T) {
		got := runBashCompletion(t, script, []string{"larkaxctl", ""})
		noFormatValues(t, got)
		if !slices.Contains(got, "channel") {
			t.Errorf("COMPREPLY = %v，缺一级命令 channel", got)
		}
	})

	t.Run("--id 后面不补取值", func(t *testing.T) {
		noFormatValues(t, runBashCompletion(t, script, []string{"larkaxctl", "channel", "get", "--id", ""}))
	})

	t.Run("-o 不补取值", func(t *testing.T) {
		noFormatValues(t, runBashCompletion(t, script, []string{"larkaxctl", "-o", ""}))
	})
}

// runBashCompletion 在真 bash 里 source 生成的脚本，跑一次 _larkaxctl，
// 返回 COMPREPLY。words 的最后一项是正在输入的那个词（与 COMP_WORDS 的约定一致）。
func runBashCompletion(t *testing.T, script string, words []string) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("找不到 bash：%v", err)
	}
	file := filepath.Join(t.TempDir(), "completion.bash")
	if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
		t.Fatalf("写补全脚本失败：%v", writeErr)
	}
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, "'"+strings.ReplaceAll(word, "'", `'\''`)+"'")
	}
	driver := "source \"$1\"\n" +
		"COMP_WORDS=(" + strings.Join(quoted, " ") + ")\n" +
		"COMP_CWORD=" + strconv.Itoa(len(words)-1) + "\n" +
		"_larkaxctl\n" +
		"printf '%s\\n' \"${COMPREPLY[@]}\"\n"
	out, runErr := exec.CommandContext(t.Context(), bash, "-c", driver, "larkaxctl-test", file).CombinedOutput()
	if runErr != nil {
		t.Fatalf("bash 跑补全失败：%v\n%s", runErr, out)
	}
	return strings.Fields(string(out))
}

// ── 参数错、帮助 ──────────────────────────────────────────────────────

// TestCompletionRejectsUnknownShell 断言非法 shell 名按参数错处理（退出码 2），
// 并把三种可选项都列出来。
func TestCompletionRejectsUnknownShell(t *testing.T) {
	err := runCompletion([]string{"powershell"})
	if err == nil {
		t.Fatal("未知的 shell 名应当报错")
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d", got, exitUsage)
	}
	for _, shell := range completionShells {
		if !strings.Contains(err.Error(), shell) {
			t.Errorf("报错里没有列出 %s：%v", shell, err)
		}
	}
}

// TestCompletionRequiresShell 断言不给 shell 名是参数错。
func TestCompletionRequiresShell(t *testing.T) {
	stdout, stderr := captureStreams(t, func() {
		err := runCompletion(nil)
		if err == nil || exitCode(err) != exitUsage {
			t.Errorf("缺 shell 名应当按参数错报，得到 %v", err)
		}
	})
	if out := stdout + stderr; !strings.Contains(out, "larkaxctl completion") {
		t.Errorf("缺 shell 名时没有打出用法：%q", out)
	}
}

// ── 动作常量与分发表不漂 ──────────────────────────────────────────────

// TestActionConstantsMatchDispatch 断言每个 xxxActions 常量与 runXxx 里的
// case 标签是同一组动词。
//
// 常量是补全与「未知子命令」报错共用的那一份；分发表里的 case 才是真正执行
// 的地方。两边分叉的两种方向都要报：常量里有而 case 里没有——Tab 补出来敲下去
// 说不存在；case 里有而常量里没有——那条命令补不出来。
//
// 旧写法（model add / wallet open 这些）允许只出现在 case 里，清单复用
// opscommandtable_test.go 的 legacyOpsVerbs，不另抄一份。
func TestActionConstantsMatchDispatch(t *testing.T) {
	funcs := actionTestFuncs(t)
	legacy := map[string]bool{"help": true}
	for _, aliases := range legacyOpsVerbs {
		for alias := range aliases {
			legacy[alias] = true
		}
	}

	found := 0
	for name, constant := range actionConstants(t) {
		base := strings.TrimSuffix(name, "Actions")
		var decl *ast.FuncDecl
		for funcName, fn := range funcs {
			if strings.EqualFold(funcName, "run"+base) {
				decl = fn
				break
			}
		}
		if decl == nil {
			t.Errorf("常量 %s 找不到对应的 run%s 函数", name, base)
			continue
		}
		labels := switchCaseLabels(decl)
		found++

		for _, verb := range constant {
			if !labels[verb] {
				t.Errorf("%s 里的 %q 不在分发表里：Tab 补出来会报「未知子命令」", name, verb)
			}
		}
		for label := range labels {
			if label == "-h" || label == "--help" || slices.Contains(constant, label) || legacy[label] {
				continue
			}
			t.Errorf("分发表里的 %q 不在 %s 里：这个动词补不出来", label, name)
		}
	}
	if found == 0 {
		t.Fatal("一个动作常量都没对上：这条用例会安静地通过，所以先在这里拦住")
	}
}

// actionConstants 收集包目录下所有 `xxxActions` 形式的 []string 变量。
func actionConstants(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	forEachNonTestFile(t, func(file string, node *ast.File) {
		for _, decl := range node.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || !strings.HasSuffix(value.Names[0].Name, "Actions") {
					continue
				}
				lit, ok := value.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s：%s 不是复合字面量", file, value.Names[0].Name)
				}
				var verbs []string
				for _, elt := range lit.Elts {
					basic, ok := elt.(*ast.BasicLit)
					if !ok || basic.Kind != token.STRING {
						t.Fatalf("%s：%s 里有不是字符串字面量的元素", file, value.Names[0].Name)
					}
					verb, err := strconv.Unquote(basic.Value)
					if err != nil {
						t.Fatalf("%s：解析 %s 失败：%v", file, basic.Value, err)
					}
					verbs = append(verbs, verb)
				}
				out[value.Names[0].Name] = verbs
			}
		}
	})
	if len(out) == 0 {
		t.Fatal("一个动作常量都没扫到：扫描器坏了，或者命名约定变了")
	}
	return out
}

// actionTestFuncs 是包目录下全部顶层函数，按名字索引。
func actionTestFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	out := map[string]*ast.FuncDecl{}
	forEachNonTestFile(t, func(_ string, node *ast.File) {
		for _, decl := range node.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			out[fn.Name.Name] = fn
		}
	})
	return out
}

// switchCaseLabels 收集一个函数里 `switch action` 那种分派的 case 标签。
//
// 只看标签是标识符 action 的 switch：函数里还有别的 switch（按取值分档之类），
// 那些 case 不是动词。
func switchCaseLabels(fn *ast.FuncDecl) map[string]bool {
	labels := map[string]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		sw, ok := node.(*ast.SwitchStmt)
		if !ok || sw.Tag == nil {
			return true
		}
		tag, ok := sw.Tag.(*ast.Ident)
		if !ok || tag.Name != "action" {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				labels[value] = true
			}
		}
		return true
	})
	return labels
}
