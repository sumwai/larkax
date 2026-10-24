package main

import (
	"flag"
	"fmt"
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
// zsh 与 fish 不一定装着。没有的时候退化成**逐字快照比对**
// （testdata/completion.{zsh,fish}）：它证明输出没变，不证明那个 shell 认它。
// 「没被真 shell 执行」这件事记在用例日志里，不写成 Skip——跳过与通过
// 在输出里长得一样（见 LESSONS.md 第 16 条）。CI 的 Go 门禁 job 会装这两个壳，
// 并设 LARKAX_REQUIRE_COMPLETION_SHELLS=1：那时候找不到壳是 `t.Fatalf`，
// 装壳失败不会静默变成绿灯。

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
			path, ok := requireCompletionShell(t, shell)
			if !ok {
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
//
// fish 还多断一条：一级命令那条 `-n` 条件里列的命令名也要与注册表逐字相等。
// 这一条与上面那条各取各的来源——`topLevelWords` 抠的是 `-a` 行，条件名直读
// `-n`，两句期望都从注册表独立派生。两边同源时，「条件漏一条」与「`-a` 行
// 漏一条」会一起漏一起绿。
func TestCompletionListsExactlyRegisteredCommands(t *testing.T) {
	want := registeredTopLevelNames()

	data := buildCompletionData()
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			script := generateCompletionScript(t, data, shell)
			got := topLevelWords(t, shell, script)
			if !slices.Equal(got, want) {
				t.Errorf("脚本里的一级命令 = %v，注册表里的是 %v", got, want)
			}
			if shell == "fish" {
				if cond := fishTopLevelConditionNames(t, script); !slices.Equal(cond, want) {
					t.Errorf("fish 顶层条件里列的命令名 = %v，注册表里的是 %v", cond, want)
				}
			}
		})
	}
}

// registeredTopLevelNames 从注册表独立派生一级命令清单（分组顺序 + 旧路径）。
func registeredTopLevelNames() []string {
	want := make([]string, 0, 32)
	for _, g := range commandGroups {
		for _, c := range g.commands {
			want = append(want, c.name)
		}
	}
	for _, c := range legacyCommands {
		want = append(want, c.name)
	}
	return want
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
		// 一级命令那一行的条件来自 fishTopLevelCondition；只认这种条件，
		// 才不会把子命令动词、选项取值那些 `-a` 行一起抠进来。
		re = regexp.MustCompile(`(?m)^complete -c larkaxctl -n 'not __fish_seen_subcommand_from [^']*' -a '([^']*)'`)
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

// fishTopLevelConditionNames 从生成物的一级命令 `-n` 条件里抠出命令名。
//
// 与 topLevelWords 读的不是同一处：那个读 `-a` 行的值，这个读条件串。两条断言
// 各取各的来源，才能分别红在「`-a` 行漏一条」与「条件漏一条」上。
func fishTopLevelConditionNames(t *testing.T, script string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^complete -c larkaxctl -n 'not __fish_seen_subcommand_from ([^']*)' -a '[^']*'`)
	matches := re.FindAllStringSubmatch(script, -1)
	if len(matches) == 0 {
		t.Fatalf("fish 脚本里找不到一级命令那一行：生成器的格式变了，或者脚本坏了")
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
	}
}

// TestFishCompletionMarksValueFlags 断言 fish 生成物给吃取值的选项加了 `-r`（自有的
// 与全局的），布尔开关与有枚举的选项没加。
//
// 逐行比字面串，不比「包含某个选项名」：`-r` 在行尾，只查名字的话加了没加都能过。
func TestFishCompletionMarksValueFlags(t *testing.T) {
	lines := map[string]bool{}
	for _, line := range strings.Split(fishCompletionScript(buildCompletionData()), "\n") {
		lines[line] = true
	}

	cases := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "吃取值的选项带 -r",
			line: "complete -c larkaxctl -n '__fish_seen_subcommand_from price; and __fish_seen_subcommand_from create' -l schedule-file -r",
			want: true,
		},
		{
			// 这一格走的是另一个注册入口：`--model` 经 legacyStringFlag 用
			// fs.Var 注册，吃不吃取值由标准库的 IsBoolFlag 约定判；上一格的
			// `--schedule-file` 走 fs.String。两个入口各钉一格，否则把
			// takesValue 改坏时只有走 fs.String 的那一格会红。
			name: "经 flag.Value 注册的选项也带 -r",
			line: "complete -c larkaxctl -n '__fish_seen_subcommand_from alias; and __fish_seen_subcommand_from create' -l model -r",
			want: true,
		},
		{
			name: "布尔开关不带 -r",
			line: "complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l credential-stdin",
			want: true,
		},
		{
			name: "布尔开关（--all）也不带 -r",
			line: "complete -c larkaxctl -n '__fish_seen_subcommand_from ledger; and __fish_seen_subcommand_from balance' -l all",
			want: true,
		},
		{
			name: "有枚举的选项，普通行不加 -r",
			line: "complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l endpoint",
			want: true,
		},
		{
			name: "全局选项 --format 不加 -r",
			line: "complete -c larkaxctl -l format",
			want: true,
		},
		{
			// 全局选项里吃取值的那一条：与枚举不同，它加的是 `-r`——取值是
			// 任意路径，`-x` 会把文件名补全关掉。
			name: "全局取值选项 --env-file 带 -r",
			line: "complete -c larkaxctl -l env-file -r",
			want: true,
		},
		{
			name: "全局取值选项不带 -x（文件补全要留着）",
			line: "complete -c larkaxctl -l env-file -x",
			want: false,
		},
		{
			name: "布尔开关带上 -r 的那一行不存在",
			line: "complete -c larkaxctl -n '__fish_seen_subcommand_from doctor' -l credential-stdin -r",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lines[tc.line]; got != tc.want {
				t.Errorf("这一行在脚本里存在 = %v，期望 %v：\n%s", got, tc.want, tc.line)
			}
		})
	}
}

// boolFlagValue 是只实现标准库约定的假 flag.Value：IsBoolFlag 的值由它自己决定。
type boolFlagValue bool

func (b boolFlagValue) String() string   { return "false" }
func (b boolFlagValue) Set(string) error { return nil }
func (b boolFlagValue) IsBoolFlag() bool { return bool(b) }

// plainFlagValue 不实现 IsBoolFlag，与 String / Int 这类一样吃取值。
type plainFlagValue struct{}

func (plainFlagValue) String() string   { return "" }
func (plainFlagValue) Set(string) error { return nil }

// TestTakesValue 逐格钉住「一种 flag.Value 吃不吃取值」的判定。
//
// 三个方向各一格。生产里 `fs.Var` 目前只注册不实现 IsBoolFlag 的值，
// 「IsBoolFlag 返回 true」那一支没有别的用例经过——把它删掉（`return !ok`）
// 在整包里不会红，这一格就是为它留的。
func TestTakesValue(t *testing.T) {
	cases := []struct {
		name  string
		value flag.Value
		want  bool
	}{
		{"IsBoolFlag 返回 true 的按开关处理", boolFlagValue(true), false},
		{"IsBoolFlag 返回 false 的吃取值", boolFlagValue(false), true},
		{"没实现 IsBoolFlag 的吃取值", plainFlagValue{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := takesValue(tc.value); got != tc.want {
				t.Errorf("takesValue(%T) = %v，期望 %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestGlobalValueFlagsMatchUsage 交叉检查两份清单：usageNotes 的「全局选项」一节里
// 凡是带取值占位的（`--xxx <…>`），必须已经接上——枚举进 globalEnums，其余进
// globalValueFlags；反过来 globalValueFlags 的每一条都必须真在那节里、且带占位。
//
// 两份清单分不开：usageNotes 是给人看的，生成器要的是按语义分好的那一张。漏接的
// 表现是那个选项在 fish 里按 Tab 补出动词，而根帮助页看着一切正常——#1171 修的就是
// 这个症状，所以这条护栏值一条用例。
func TestGlobalValueFlagsMatchUsage(t *testing.T) {
	section := usageNotes
	if i := strings.Index(section, "全局选项"); i >= 0 {
		section = section[i:]
	}
	if i := strings.Index(section, "\n\n"); i >= 0 {
		section = section[:i]
	}
	// 一行一个全局选项：`  --name <占位>  说明` 或 `  --name  说明`。
	lineRE := regexp.MustCompile(`(?m)^\s*(--[a-z][a-z0-9-]*)(\s+<[^>]*>)?`)
	declared := map[string]bool{} // 名字 → 是否带取值占位
	for _, m := range lineRE.FindAllStringSubmatch(section, -1) {
		declared[m[1]] = m[2] != ""
	}
	if len(declared) == 0 {
		t.Fatalf("没从 usageNotes 里解析出全局选项：解析规则与帮助文本的格式脱节了")
	}

	enums := completionGlobalEnumValues()
	for name, takesValue := range declared {
		if !takesValue {
			continue
		}
		_, wiredEnum := enums[name]
		wiredValue := slices.Contains(globalValueFlags, name)
		switch {
		case wiredEnum && wiredValue:
			t.Errorf("全局选项 %s 同时出现在 globalEnums 与 globalValueFlags："+
				"`-x -a` 与 `-r` 是两种语义，只能选一种", name)
		case !wiredEnum && !wiredValue:
			t.Errorf("全局选项 %s 在帮助文本里有取值占位，却没接上补全："+
				"枚举加进 globalEnums，自由取值加进 globalValueFlags", name)
		}
	}
	for _, name := range globalValueFlags {
		takesValue, ok := declared[name]
		if !ok {
			t.Errorf("globalValueFlags 里的 %s 不在 usageNotes 的全局选项一节里：两份清单分岔了", name)
			continue
		}
		if !takesValue {
			t.Errorf("globalValueFlags 里的 %s 在帮助文本里没有取值占位：它不该按吃取值处理", name)
		}
	}
}

// TestCompletionBehaviorInRealShells 在真 shell 里跑一遍生成出来的补全。
//
// 只比生成物里的字面串挡不住 case 模式里的元字符：`|` 在 case 里是多模式
// 分隔符，`*|--format` 实际是 `*` 与 `--format` 两条模式，而 `*` 匹配任意键，
// 于是每个 Tab 都返回 format 的取值、后面的分支全被截断。这一条按行为钉。
//
// 三种壳都能非交互地取候选。bash 走 COMP_WORDS + `_larkaxctl`，fish 走
// `complete -C`；zsh 的 `compadd` 只能在真正的补全上下文里调，所以先把
// `compadd` 换成一个把参数打到 stdout 的替身，再调生成脚本里的函数——
// case 的匹配语义照样被验到，替身只接手「候选给了谁」，不参与判断。
//
// 缺壳时记一行日志并跳过这一格，不写成 Skip（跳过与通过在输出里长得一样，
// 见 LESSONS.md 第 16 条）；设了 LARKAX_REQUIRE_COMPLETION_SHELLS 时改成
// `t.Fatalf`——CI 装了壳，装失败不该静默变绿。
func TestCompletionBehaviorInRealShells(t *testing.T) {
	data := buildCompletionData()
	// 每格一张判据，shell 名传进去：fish 的补全模型与 bash / zsh 的手写分派
	// 有一处不同（fish 会把候选排过序，见 `--format` 那格的注释），同一条判据
	// 在那里要分叉，但期望值只有一份，不抄第二张表。
	cases := []struct {
		name  string
		words []string // `larkaxctl` 之后、正在输入的那个词之前的部分
		check func(t *testing.T, shell string, got []string)
		cur   string // 正在输入的那个词；空串是「还没敲」
	}{
		{"--format 补取值", []string{"--format"}, func(t *testing.T, shell string, got []string) {
			requireEnumCandidates(t, shell, got, []string{"table", "json"})
		}, ""},
		{"--scope 补取值", []string{"price", "create", "--scope"}, func(t *testing.T, shell string, got []string) {
			requireEnumCandidates(t, shell, got, []string{"platform", "merchant", "channel"})
		}, ""},
		{"顶层补命令", nil, func(t *testing.T, _ string, got []string) {
			requireNoFormatValues(t, got)
			if !slices.Contains(got, "channel") {
				t.Errorf("候选 = %v，缺一级命令 channel", got)
			}
		}, ""},
		// 全局选项的取值吃掉一个词之后，一级命令仍要补。这两格是本次 fish 改动的
		// 证据：`__fish_use_subcommand` 把取值当成位置词，改前一条候选都没有；
		// bash 与 zsh 的路径取到第一个 `-` 就停，改前改后都是这一批。
		{"--env-file 取值后仍补一级命令", []string{"--env-file", "x"}, func(t *testing.T, shell string, got []string) {
			requireRegisteredTopLevel(t, shell, got)
		}, ""},
		{"--format 取值后仍补一级命令", []string{"--format", "json"}, func(t *testing.T, shell string, got []string) {
			requireRegisteredTopLevel(t, shell, got)
		}, ""},
		// 取值恰好等于一级命令名，也就是 docs/cli-reference.md 的 completion 一节
		// 写明的边界，按 shell 分行断：
		//   - fish 按「词里有没有一级命令名」判断，那个词被当成子命令，候选退成
		//     channel 那一级的动作，一条一级命令都没有；
		//   - bash 与 zsh 的路径在第一个以 `-` 开头的词处停，路径是空的，给的还是
		//     那 27 条一级命令。
		// 两种结果都来自既有规则，不是本次改动引入的。fish 这一格是已知边界，
		// 判定方式一变，这里与文档要一起更新。
		{"取值恰好是命令名", []string{"--env-file", "channel"}, func(t *testing.T, shell string, got []string) {
			if shell == "fish" {
				requireFishNamedValueBoundary(t, got)
				return
			}
			requireRegisteredTopLevel(t, shell, got)
		}, ""},
		{"channel 补动作", []string{"channel"}, func(t *testing.T, _ string, got []string) {
			requireNoFormatValues(t, got)
			if !slices.Contains(got, "get") {
				t.Errorf("候选 = %v，缺动作 get", got)
			}
		}, ""},
		// 三个取值位各一格：顶层全局选项（路径是空串，最容易落到「空路径 ⇒ 列
		// 一级命令」那条）、子命令里的文件路径选项、子命令里的非路径参数。
		{"顶层 --env-file 取值位", []string{"--env-file"}, func(t *testing.T, shell string, got []string) {
			requireFileFallback(t, shell, got)
		}, ""},
		{"子命令 --schedule-file 取值位", []string{"price", "create", "--schedule-file"},
			func(t *testing.T, shell string, got []string) {
				requireFileFallback(t, shell, got)
			}, ""},
		{"--id 后面不补取值", []string{"channel", "get", "--id"}, func(t *testing.T, shell string, got []string) {
			requireFileFallback(t, shell, got)
		}, ""},
		{"-o 不补取值", []string{"-o"}, func(t *testing.T, _ string, got []string) {
			requireNoFormatValues(t, got)
		}, ""},
		// 取值位上已经敲了一个 `-`：敲到这里的人下一步想要的是另一个选项，不是
		// 文件名，所以这一格要落回选项名。三壳的候选同为全局长选项（已按字典序），
		// fish 那边的排序在这里本来就是有序的，不必分叉。
		//
		// fish 这一格不是本次改动的证据：fish 生成器没动、也没有这道守卫，改前改后
		// 都给选项名。守卫只由 bash 与 zsh 两格在验。
		{"--env-file 后跟 - 仍补选项名", []string{"--env-file"}, func(t *testing.T, _ string, got []string) {
			want := []string{"--env-file", "--format", "--quiet", "--verbose"}
			if !slices.Equal(got, want) {
				t.Errorf("候选 = %v，期望 %v：取值位上的 `-` 应当落回选项名", got, want)
			}
		}, "-"},
	}

	t.Run("bash", func(t *testing.T) {
		bash, ok := requireCompletionShell(t, "bash")
		if !ok {
			return
		}
		script := bashCompletionScript(data)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tc.check(t, "bash", runBashCompletion(t, bash, script, tc.words, tc.cur))
			})
		}
	})

	t.Run("zsh", func(t *testing.T) {
		zsh, ok := requireCompletionShell(t, "zsh")
		if !ok {
			return
		}
		script := zshCompletionScript(data)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tc.check(t, "zsh", runZshCompletion(t, zsh, script, tc.words, tc.cur))
			})
		}
	})

	t.Run("fish", func(t *testing.T) {
		fish, ok := requireCompletionShell(t, "fish")
		if !ok {
			return
		}
		script := fishCompletionScript(data)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tc.check(t, "fish", runFishCompletion(t, fish, script, tc.words, tc.cur))
			})
		}
	})
}

// TestFishMultiSegmentConditions 钉住 fish 多段路径的条件：路径上每一段都出现过、
// 且它的直接子命令名一个都没出现时才成立。
//
// 两条规则各修过一次。先是 `and` 的连接位置：fishSeenCondition 曾把各段用空格
// 隔开的 `and` 连起来，而 fish 只在命令分隔位置把 `and` 当连接符；整串于是成了
// `__fish_seen_subcommand_from` 的参数，判定退化成「任一段出现过」，深层路径的
// 动作漏到父层——`channel map` 的 `add` / `remove` 出现在 `channel ` 那一级
// （见 #1182）。再是父层条件在子层仍为真：只判「这一段出现过」时，`channel map `
// 里 `channel` 出现过，父层动作全留在子层（见 #1183）。现在的条件多了 `not
// __fish_seen_subcommand_from <直接子命令名>`；子命令一敲出来，父层那条就不再成立。
//
// 期望从 completionActionTable 与注册表独立派生（见 fishExpectedCandidates），
// 不读生成物：从生成物取期望等于把同一个错误抄两遍。fish 的 `complete -C` 按
// 字典序返回候选，比较前两边都排序。
func TestFishMultiSegmentConditions(t *testing.T) {
	fish, ok := requireCompletionShell(t, "fish")
	if !ok {
		return
	}
	data := buildCompletionData()
	script := fishCompletionScript(data)
	cells := [][]string{
		nil, // 一级命令：不受这两条规则影响，作为对照
		{"channel"}, {"channel", "map"}, {"channel", "map", "add"}, {"channel", "map", "remove"},
		{"channel", "adapters"},
		{"model"}, {"model", "alias"},
		{"price"}, {"price", "calendar"}, {"price", "show"},
		{"admin"}, {"admin", "token"},
		{"user"}, {"user", "plan"}, {"user", "plan", "order"}, {"user", "inspect"},
		{"wallet"}, {"wallet", "payout"},
		{"migrate"}, {"migrate", "up"},
		{"plugin", "delete"}, {"settings", "list"},
		{"consent", "revoke"}, {"ledger", "reconcile"},
	}
	for _, words := range cells {
		name := "动作: 顶层"
		if len(words) > 0 {
			name = "动作: " + strings.Join(words, " ") + " "
		}
		t.Run(name, func(t *testing.T) {
			got := runFishCompletion(t, fish, script, words, "")
			gotCmp := slices.Clone(got)
			slices.Sort(gotCmp)
			want := fishExpectedCandidates(words)
			if !slices.Equal(gotCmp, want) {
				t.Errorf("`larkaxctl %s` 的候选 = %v，期望 %v（按动作表派生）",
					strings.Join(words, " "), got, want)
			}
		})
	}
}

// TestFishChildLevelActionsEqualTheirOwnTable 是对上面那条规则的反向等式：在这几
// 格里，候选恰好是**那条路径自己的动作表**，一条父层动词都不许有。
//
// 与 TestFishMultiSegmentConditions 的期望算法不同：那边算的是「所有成立路径的
// 并集」，这里只认这一条路径的常量。两者同时成立，才说明子层的候选既没有多
// （父层漏下来）也没有少（自己的被父层那条否定误伤）。
//
// `model alias ` 不在这张表里：一级命令 `alias` 的动作会因为同名词串扰一起补出来
// （见 #1183 的「不做的事」），那格的期望由上面的并集那条负责。`user plan order `
// 同名的 `plan order` 也成立，只是两边的动作恰好是同一份（`show` / `set`）。
func TestFishChildLevelActionsEqualTheirOwnTable(t *testing.T) {
	fish, ok := requireCompletionShell(t, "fish")
	if !ok {
		return
	}
	script := fishCompletionScript(buildCompletionData())
	// 下面几条路径在动作表里都没有条目：它们是吃位置的叶子，动作数为 0。
	// `migrate up` 与 `channel map remove` 是不同类型的叶子——前者属于整组不遍历
	// 选项的 `migrate`，后者是三级动词；两种都曾在否定名单里缺位（见 #1183）。
	for _, path := range []string{
		"channel map", "channel map add", "channel map remove", "channel adapters",
		"migrate up", "price show", "user inspect", "user plan order", "wallet payout",
	} {
		t.Run(path+" ", func(t *testing.T) {
			got := runFishCompletion(t, fish, script, strings.Fields(path), "")
			want := slices.Clone(completionActionTable[path])
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("`larkaxctl %s ` 的候选 = %v，期望这条路径自己的动作表 %v：子层不该再有父层动词",
					path, got, want)
			}
		})
	}
}

// TestCompletionPathSegmentsExistInParentWords 断言注册表里的每条路径都能沿
// 「父路径的词表」逐段走通。
//
// 否定名单取的是 data.words[路径] 的值，所以一条路径在父层词表里缺席，就等于
// 那条子命令不会被否定——父层动作会漏到它后面（#1183 评审 P1）。这条从另一头
// 守同一件事：路径枚举（动作表的路径、data.flags 的键）里只要长出一段父层词表
// 没有的名字，这里先红。
func TestCompletionPathSegmentsExistInParentWords(t *testing.T) {
	data := buildCompletionData()
	paths := make([]string, 0, len(data.flags)+len(completionActionTable))
	for key := range data.flags {
		paths = append(paths, key)
	}
	for path := range completionActionTable {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		fields := strings.Fields(path)
		for i := 1; i < len(fields); i++ {
			parent := strings.Join(fields[:i], " ")
			if !slices.Contains(data.words[parent], fields[i]) {
				t.Errorf("路径 %q 的第 %d 段 %q 不在父路径 %q 的词表里（data.words[%q] = %v）：否定名单会漏掉它",
					path, i+1, fields[i], parent, parent, data.words[parent])
			}
		}
	}
}

// fishExpectedCandidates 按动作表独立派生某一格应当出现的候选。
//
// fish 的路径条件是「路径上每段都出现过、且直接子命令名一个都没出现」
// （见 fishPathHolds），所以期望是动作表里所有成立路径的动作并集；一级命令那一
// 格没有路径，条件是「还没出现过一级命令名」，另算。只从 completionActionTable
// 与注册表取，不读生成物。
func fishExpectedCandidates(words []string) []string {
	seen := make(map[string]bool, len(words))
	for _, word := range words {
		seen[word] = true
	}
	var want []string
	for path, verbs := range completionActionTable {
		if fishPathHolds(path, seen) {
			want = append(want, verbs...)
		}
	}
	topLevelSeen := false
	for _, name := range registeredTopLevelNames() {
		if seen[name] {
			topLevelSeen = true
			break
		}
	}
	if !topLevelSeen {
		want = append(want, registeredTopLevelNames()...)
	}
	slices.Sort(want)
	return slices.Compact(want)
}

// fishPathHolds 判断一条路径在给定的词序列下是否成立：路径上每段都出现过，
// 且它的直接子命令名一个都没出现。
//
// 两条都从注册表取：路径本身来自 completionActionTable / data.flags，子命令名
// 来自 fishExpectedChildren。
func fishPathHolds(path string, seen map[string]bool) bool {
	for _, token := range strings.Fields(path) {
		if !seen[token] {
			return false
		}
	}
	for _, child := range fishExpectedChildren(path) {
		if seen[child] {
			return false
		}
	}
	return true
}

// fishExpectedChildren 按注册表独立派生一条路径的直接子命令名：**这条路径的
// 动作表**（completionActionTable），空路径取一级命令清单。
//
// 与生成器不共用来源是这条用例的关键：生成器读 data.words，这里读动作常量，
// 两处只要有一处对「下一级有哪些词」的定义不全，这条就会红。曾经两边都从
// data.flags 的键派生，同一个缺口被抄了两遍、一起绿——没注册选项的叶子动词
// （`channel adapters`、`migrate up`）不在键里，否定名单漏掉它们（见 #1183
// 的评审 P1）。动作表是这些名字的源头，它不依赖某个动词有没有注册选项。
func fishExpectedChildren(path string) []string {
	var children []string
	if path == "" {
		children = registeredTopLevelNames()
	} else {
		children = slices.Clone(completionActionTable[path])
	}
	slices.Sort(children)
	return slices.Compact(children)
}

// TestFishOptionPositionConditions 钉住条件在选项位上的两件事。
//
// 一是 #1182 那条分隔符规则：`model alias -` 修前有 30 条候选，`model create`、
// `model alias create` 这些路径的选项全漏了进来。二是否定**用过了头**：
// `channel map add -` 该有它自己的 `--overrides` 加全局选项，少了它就说明把子层
// 自己的选项也一起否掉了。
//
// 它不是「否定本身有效」的反向证据：选项只从自己那条路径补，而带自有选项的
// 路径里没有一对是父子关系（按 data.flags 的键逐对核过），今天没有父层选项能漏
// 到子层——把生成器里那段否定删掉，这六格照样全绿。那份证据在动作位，见
// TestFishMultiSegmentConditions 与 TestFishChildLevelActionsEqualTheirOwnTable。
//
// 期望同样从注册表派生（见 fishExpectedOptions），不读生成物。
func TestFishOptionPositionConditions(t *testing.T) {
	fish, ok := requireCompletionShell(t, "fish")
	if !ok {
		return
	}
	data := buildCompletionData()
	script := fishCompletionScript(data)
	cells := [][]string{
		{"model", "alias"},
		{"alias", "resolve"},
		{"channel"},
		{"channel", "map"},
		{"channel", "map", "add"},
		{"price", "calendar"},
	}
	for _, words := range cells {
		t.Run("选项: "+strings.Join(words, " ")+" -", func(t *testing.T) {
			got := runFishCompletion(t, fish, script, words, "-")
			gotCmp := slices.Clone(got)
			slices.Sort(gotCmp)
			want := fishExpectedOptions(data, words)
			if !slices.Equal(gotCmp, want) {
				t.Errorf("`larkaxctl %s -` 的候选 = %v，期望 %v（按注册表派生）",
					strings.Join(words, " "), got, want)
			}
		})
	}
}

// fishExpectedOptions 按注册表独立派生选项位上应当出现的候选。
//
// 与 fishExpectedCandidates 同一条规则（见 fishPathHolds）：成立的路径，它的自有
// 选项都算数；全局选项无条件补，与路径无关。只从 data.flags 与 usageNotes 的
// 全局选项一节取。
func fishExpectedOptions(data completionData, words []string) []string {
	seen := make(map[string]bool, len(words))
	for _, word := range words {
		seen[word] = true
	}
	var want []string
	for _, path := range flaggedPaths(data) {
		if fishPathHolds(path, seen) {
			want = append(want, ownOptionNames(data, path)...)
		}
	}
	want = append(want, globalOptionNames()...)
	slices.Sort(want)
	return slices.Compact(want)
}

// requireEnumCandidates 断言候选恰是枚举的取值。
//
// fish 的 `complete -C` 会把候选按字典序排过再返回：顺序不归我们定，也不钉成
// 契约，所以这一格按排序后的副本比较。生成物里 `-a '…'` 的书写顺序仍由
// TestCompletionWiresEnums 的字面串钉住，bash 与 zsh 照旧按顺序比。比较用副本，
// 报错仍按契约顺序打期望值——否则失败信息会把 `[json table]` 说成期望。
func requireEnumCandidates(t *testing.T, shell string, got, want []string) {
	t.Helper()
	gotCmp, wantCmp := got, want
	if shell == "fish" {
		gotCmp = slices.Clone(got)
		slices.Sort(gotCmp)
		wantCmp = slices.Clone(want)
		slices.Sort(wantCmp)
	}
	if !slices.Equal(gotCmp, wantCmp) {
		t.Errorf("候选 = %v，期望 %v", got, want)
	}
}

// requireFileFallback 断言取值位把补全交回给了 shell。
//
// 两个壳的「交回去」长得不一样：
//   - bash 是 `COMPREPLY` 保持为空——这是末尾 `complete -o default` 接手补文件的
//     **前提**，也是驱动里能看到的唯一一半（回落本身发生在函数之外）；
//   - zsh 没有回落，生成脚本自己调 `_files`，替身把它记成 filesStubCandidate。
//
// fish 走 `-r`，在空目录里同样没有候选。文件补全真的开着这件事由
// TestFishCompletionValueFlagOffersFiles 在有文件的目录里钉。
func requireFileFallback(t *testing.T, shell string, got []string) {
	t.Helper()
	if shell == "zsh" {
		if !slices.Equal(got, []string{filesStubCandidate}) {
			t.Errorf("候选 = %v，期望恰好 [%s]：取值位应当调 _files，把候选交给它",
				got, filesStubCandidate)
		}
		return
	}
	if len(got) != 0 {
		t.Errorf("候选 = %v，期望空：取值位不产出候选，「交回 shell」才成立", got)
	}
}

// TestCompletionValueFallbackPlacement 钉住取值位那一段的位置与份数，
// 每一条键都断（三百多条）。
//
// 行为用例只覆盖三格（`--env-file`、`--schedule-file`、`--id`，见
// requireFileFallback），漏掉一条自有键它看不见：那些键都在叶子路径上，没有下一级
// 词表可填，bash 的候选仍是空、zsh 只是从 `_files` 变成空。只有全局 `--env-file`
// 漏掉时才明显（顶层有词表，补出来的是一级命令）。
//
// 这一条按生成物逐键断：每条键那一份必须在顶层 `case "$cur"` 之前，且生成物里恰好
// 一份。挪到动词分派之后、挪进 `case "$cur" in -*` 的嵌套分支、漏掉某条键，都会红。
//
// 期望清单在测试里自己从注册表走一遍（见 fallbackKeysFromFlags），不调
// valueOptionKeys：两头同源的话，「函数漏了一条键」期望值跟着漏，这条断言与行为
// 用例会一起绿——金文件只覆盖 zsh / fish，bash 没有第二条守卫。
func TestCompletionValueFallbackPlacement(t *testing.T) {
	data := buildCompletionData()
	keys := fallbackKeysFromFlags(data)
	if len(keys) == 0 {
		t.Fatalf("注册表里一条取值位都没有：下面遍历它的循环会一条都不断言")
	}
	if got := valueOptionKeys(data); !slices.Equal(got, keys) {
		t.Errorf("valueOptionKeys 与注册表对不上——两份清单只改了一边：%s", keyDiff(got, keys))
	}
	// 枚举位的排除由上面那条相等断言与生成物本身覆盖（fallbackKeysFromFlags 与
	// valueOptionKeys 都跳过 data.enums，在这里再遍历一遍永远不成立）。

	globals := slices.Clone(globalValueFlags)
	slices.Sort(globals)
	// 每条分支的抬头：自有键带路径，全局键的路径是 `*` 通配。
	patterns := make([]string, 0, len(keys)+len(globals))
	for _, key := range keys {
		patterns = append(patterns, shellCasePattern(key)+")")
	}
	for _, name := range globals {
		patterns = append(patterns, "*"+shellCasePattern("|"+name)+")")
	}

	cases := []struct {
		shell  string
		script string
	}{
		{"bash", bashCompletionScript(data)},
		{"zsh", zshCompletionScript(data)},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			// 锚必须带上行首的那个制表符：守卫自己也是 `case "$cur" in`，只是缩进
			// 深一层；不带缩进的锚会落在第一段守卫上，位置断言恒假绿。
			at := strings.Index(tc.script, "\n\tcase \"$cur\" in\n")
			if at < 0 {
				t.Fatalf("生成物里没有顶层的 `case \"$cur\" in`：生成器结构变了，这条位置断言要重写")
			}
			var missing, moved []string
			for _, pattern := range patterns {
				switch n := strings.Count(tc.script, pattern+"\n"); n {
				case 0:
					missing = append(missing, pattern)
					continue
				case 1:
				default:
					t.Errorf("生成物里 %q 出现 %d 次：每条键应当恰好一份", pattern, n)
					continue
				}
				if strings.Index(tc.script, pattern+"\n") > at {
					moved = append(moved, pattern)
				}
			}
			// 整段被删或被挪会牵动几百条，逐条报会把结论淹掉，所以只摘前几条。
			if len(missing) > 0 {
				t.Errorf("生成物里没有这一段（%d 条）：%s", len(missing), samplePatterns(missing))
			}
			if len(moved) > 0 {
				t.Errorf("取值位分支挪到了顶层 `case \"$cur\"` 之后（%d 条）：%s",
					len(moved), samplePatterns(moved))
			}
		})
	}
	// bash 的回落靠这一行：分支只把 `COMPREPLY` 留空，接手补文件的是它。
	if !strings.HasSuffix(cases[0].script, "complete -o default -F _larkaxctl larkaxctl\n") {
		t.Errorf("bash 生成物末尾不再是 `complete -o default -F _larkaxctl larkaxctl`：" +
			"取值位留空的回落依赖它")
	}
}

// fallbackKeysFromFlags 在测试里自己走一遍注册表，得出取值位应当有的键。
//
// 与被测的 valueOptionKeys 是两份实现，这是故意的：两边同源时，函数漏一条键
// 期望值跟着漏，断言照样绿。
func fallbackKeysFromFlags(data completionData) []string {
	var keys []string
	for path := range data.flags {
		for _, option := range ownOptions(data, path) {
			if !option.takesValue {
				continue
			}
			if _, wired := data.enums[path+"|"+option.name]; wired {
				continue
			}
			keys = append(keys, path+"|"+option.name)
		}
	}
	slices.Sort(keys)
	return keys
}

// samplePatterns 把一长串模式摘成前几条，截断时以「…」收尾。条数由调用方报，
// 这里不再写一遍——同一句里出现两个同样的数字，读的人会以为是两件事。
func samplePatterns(patterns []string) string {
	if len(patterns) == 0 {
		return "（无）"
	}
	sample := patterns[:min(5, len(patterns))]
	if len(patterns) == len(sample) {
		return strings.Join(sample, " ")
	}
	return strings.Join(sample, " ") + " …"
}

// keyDiff 列出两份清单各自独有的键。只摘前几条：整份清单一打出来，报错就没法读了。
func keyDiff(got, want []string) string {
	var extra, missing []string
	for _, key := range got {
		if !slices.Contains(want, key) {
			extra = append(extra, key)
		}
	}
	for _, key := range want {
		if !slices.Contains(got, key) {
			missing = append(missing, key)
		}
	}
	return fmt.Sprintf("函数多出 %d 条：%s；函数缺少 %d 条：%s",
		len(extra), samplePatterns(extra), len(missing), samplePatterns(missing))
}

// TestFishCompletionValueFlagOffersFiles 断言 `-r` 在取值位置保留文件补全，
// 而不是像 `-x` 那样把取值补全整个关掉。
//
// 别处的行为用例都在空目录里跑（见 runFishCompletion）。在空目录里 `-r` 与 `-x`
// 给出的候选都是空，只有这一格能把两者分开。自有选项与全局选项各一格：两者的
// `-r` 由不同分支生成（`ownOptions` 与 `globalValueFlags`），只钉一处时另一处
// 漏掉不会红。
func TestFishCompletionValueFlagOffersFiles(t *testing.T) {
	fish, ok := requireCompletionShell(t, "fish")
	if !ok {
		return
	}
	script := fishCompletionScript(buildCompletionData())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schedule.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("写候选文件失败：%v", err)
	}
	cases := []struct {
		name  string
		words []string
	}{
		{"自有选项 --schedule-file", []string{"price", "create", "--schedule-file"}},
		{"全局选项 --env-file", []string{"--env-file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runFishCompletionIn(t, fish, script, tc.words, "", dir)
			if !slices.Contains(got, "schedule.json") {
				t.Errorf("候选 = %v，期望有 schedule.json：`-r` 应当保留文件补全", got)
			}
		})
	}
}

// completionShellsRequiredEnv 设上之后，缺壳不再是「记一行日志就过」，而是失败。
//
// CI 的 Go 门禁 job 装了 fish 与 zsh 再跑单元测试（bash 由 runner 镜像自带）。
// 装壳那一步失败时用例仍然会走到这里——没有这个变量，它就退化成两行日志，
// 整条链全绿，正是本仓反复踩的「跳过与通过长得一样」（LESSONS.md 第 16 条）。
const completionShellsRequiredEnv = "LARKAX_REQUIRE_COMPLETION_SHELLS"

// requireCompletionShell 返回 shell 的可执行路径；没有时按上面那条口径处理。
func requireCompletionShell(t *testing.T, shell string) (string, bool) {
	t.Helper()
	path, err := exec.LookPath(shell)
	if err == nil {
		return path, true
	}
	if os.Getenv(completionShellsRequiredEnv) != "" {
		t.Fatalf("%s 已设，但环境里找不到 %s：CI 装壳那一步失败了吗（%v）",
			completionShellsRequiredEnv, shell, err)
	}
	t.Logf("这一格不做真执行：环境里没有 %s", shell)
	return "", false
}

// requireRegisteredTopLevel 断言候选就是注册表里的一级命令。
//
// 期望值从注册表独立派生（不读生成物）：读生成物等于把同一个错误抄两遍。
// fish 的 `complete -C` 会把候选按字典序排过再返回，比较用排好序的副本。
func requireRegisteredTopLevel(t *testing.T, shell string, got []string) {
	t.Helper()
	want := registeredTopLevelNames()
	gotCmp, wantCmp := got, want
	if shell == "fish" {
		gotCmp = slices.Clone(got)
		slices.Sort(gotCmp)
		wantCmp = slices.Clone(want)
		slices.Sort(wantCmp)
	}
	if !slices.Equal(gotCmp, wantCmp) {
		t.Errorf("候选 = %v，期望注册表里的一级命令 %v", got, want)
	}
}

// requireFishNamedValueBoundary 钉住 fish 在「取值恰好是命令名」那一格的边界。
//
// `--env-file channel ` 里的 `channel` 被 fish 当成子命令，一级命令「还没出现过
// 命令名」的条件因此不成立，候选退成 channel 那一级的动作；取值本身照常生效。
// 期望从动作表独立派生（见 fishExpectedCandidates），不写死条数：新增一个
// channel 动作时期望跟着变，不会出现「数字对、动词错」这种假绿。
func requireFishNamedValueBoundary(t *testing.T, got []string) {
	t.Helper()
	want := fishExpectedCandidates([]string{"--env-file", "channel"})
	gotCmp := slices.Clone(got)
	slices.Sort(gotCmp)
	if !slices.Equal(gotCmp, want) {
		t.Errorf("候选 = %v，期望 channel 那一级的动作 %v：取值被当成子命令，应当只剩 channel 的动作",
			got, want)
	}
}

// requireNoFormatValues 断言候选里没有 --format 的取值。
func requireNoFormatValues(t *testing.T, got []string) {
	t.Helper()
	for _, unwanted := range []string{"table", "json"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("候选 = %v，不该出现 %q：通配分支把别的键也截断了", got, unwanted)
		}
	}
}

// runBashCompletion 在真 bash 里 source 生成的脚本，跑一次 _larkaxctl，
// 返回 COMPREPLY。words 是 `larkaxctl` 之后的部分，正在输入的那个词由 cur 给出
// （多数格子传空串；要验「已经敲了一个 `-`」就传它）。
func runBashCompletion(t *testing.T, bash, script string, words []string, cur string) []string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "completion.bash")
	if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
		t.Fatalf("写补全脚本失败：%v", writeErr)
	}
	argv := append(append([]string{"larkaxctl"}, words...), cur)
	driver := "source \"$1\"\n" +
		"COMP_WORDS=(" + shellQuoteAll(argv) + ")\n" +
		"COMP_CWORD=" + strconv.Itoa(len(argv)-1) + "\n" +
		"_larkaxctl\n" +
		"printf '%s\\n' \"${COMPREPLY[@]}\"\n"
	out, runErr := exec.CommandContext(t.Context(), bash, "-c", driver, "larkaxctl-test", file).CombinedOutput()
	if runErr != nil {
		t.Fatalf("bash 跑补全失败：%v\n%s", runErr, out)
	}
	return strings.Fields(string(out))
}

// filesStubCandidate 是 zsh 驱动里 `_files` 替身写出的标记，不是候选的名字：
// 它只表示生成脚本在这一格要求了文件补全。真 `_files` 会不会列出文件、列出哪些，
// 只能在真交互式 shell 里看，而那种用例带固定等待，不进 CI（见 #1178 的判据）。
const filesStubCandidate = "__files__"

// runZshCompletion 在真 zsh 里 source 生成的脚本，跑一次 _larkaxctl，
// 返回 compadd 收到的候选。
//
// 生成脚本里的 `compadd -- a b c` 换成把参数打到 stdout 的替身：真 compadd
// 只能在补全上下文里调，直接调用会报错。替身只接手「候选给了谁」，case 的
// 匹配语义仍由生成脚本自己决定。一行一次 compadd，去掉打头的 `--` 就是候选。
func runZshCompletion(t *testing.T, zsh, script string, words []string, cur string) []string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "completion.zsh")
	if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
		t.Fatalf("写补全脚本失败：%v", writeErr)
	}
	argv := append(append([]string{"larkaxctl"}, words...), cur)
	// 中途出错有两类，分别由两样东西兜住：
	//   - 进程提前终止（`exit`、整段没跑到最后一行）：结尾那行标记不见了；
	//   - 生成脚本里某条命令失败：只往 stderr 写，驱动照旧往下走、标记照旧打印。
	//     所以成功路径上再断言 stderr 为空（见下面的检查）。
	// --id 那种该给空候选的格子本来就没有 stdout 输出，没有这两样，「静默失败」
	// 与「通过」在输出里长得一样。
	const done = "__completion_driver_done__"
	driver := "compadd() { print -r -- \"$@\" }\n" +
		// `_files` 与 `compadd` 同理：真 `_files` 只能在补全上下文里调。替身写出的
		// 标记不是候选的名字，只表示「生成脚本在这一格要求了文件补全」。
		"_files() { print -r -- \"-- " + filesStubCandidate + "\" }\n" +
		"compdef() { : }\n" +
		"source \"$1\"\n" +
		"words=(" + shellQuoteAll(argv) + ")\n" +
		"CURRENT=" + strconv.Itoa(len(argv)) + "\n" +
		"_larkaxctl\n" +
		"print -r -- " + done + "\n"
	var stderr strings.Builder
	cmd := exec.CommandContext(t.Context(), zsh, "-f", "-c", driver, "larkaxctl-test", file)
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if runErr != nil {
		t.Fatalf("zsh 跑补全失败：%v\n%s", runErr, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if lines[len(lines)-1] != done {
		t.Fatalf("zsh 没跑到结尾（补全函数中途出错？）\nstdout=%q\nstderr=%s", out, stderr.String())
	}
	// 生成脚本在成功路径上不该往 stderr 写东西：一条失败的命令不会中止驱动，
	// 标记照样会打印，只有这一条能把那种情况分出来。
	if stderr.Len() > 0 {
		t.Fatalf("zsh 补全往 stderr 写了东西（生成脚本里有命令失败？）\nstdout=%q\nstderr=%s", out, stderr.String())
	}
	var candidates []string
	for _, line := range lines[:len(lines)-1] {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "--" {
			continue
		}
		candidates = append(candidates, fields[1:]...)
	}
	return candidates
}

// runFishCompletion 用 fish 的 `complete -C` 非交互地取候选，工作目录是一个
// 空目录。
//
// 目录不是随便挑的：吃取值的选项带 `-r`，fish 会给它补文件名。在仓库目录里跑，
// 「取值位置应当没有候选」那几格会被当前目录的文件填满——红的不是生成物，而是
// 工作目录。空的临时目录让「文件补全开着」退化成「没有候选」；要验文件补全的
// 那一格自己指定目录（见 runFishCompletionIn）。
//
// 每条候选可能带一段描述（制表符分隔），这里只要名字。
func runFishCompletion(t *testing.T, fish, script string, words []string, cur string) []string {
	t.Helper()
	return runFishCompletionIn(t, fish, script, words, cur, t.TempDir())
}

// runFishCompletionIn 与 runFishCompletion 相同，但把 fish 的工作目录设成 dir。
func runFishCompletionIn(t *testing.T, fish, script string, words []string, cur string, dir string) []string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "completion.fish")
	if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
		t.Fatalf("写补全脚本失败：%v", writeErr)
	}
	// 末尾那个空格是「当前词还没敲」：cur 为空的格子靠它，别省。
	line := "larkaxctl " + strings.Join(words, " ") + " " + cur
	driver := "source " + fishQuote(file) + "; complete -C " + fishQuote(line)
	cmd := exec.CommandContext(t.Context(), fish, "--no-config", "-c", driver)
	cmd.Dir = dir
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("fish 取候选失败：%v\n%s", runErr, out)
	}
	var candidates []string
	for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if raw == "" {
			continue
		}
		if i := strings.IndexByte(raw, '\t'); i >= 0 {
			raw = raw[:i]
		}
		candidates = append(candidates, raw)
	}
	return candidates
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
