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
	// 有两处不同（见下面两格的注释），同一条判据在那两处要分叉，但期望值只有
	// 一份，不抄第二张表。
	cases := []struct {
		name  string
		words []string // `larkaxctl` 之后、正在输入的那个词之前的部分
		check func(t *testing.T, shell string, got []string)
	}{
		{"--format 补取值", []string{"--format"}, func(t *testing.T, shell string, got []string) {
			want := []string{"table", "json"}
			gotCmp, wantCmp := got, want
			if shell == "fish" {
				// fish 的 `complete -C` 会把候选按字典序排过再返回：顺序不归我们
				// 定，也不钉成契约，所以这一格按排序后的副本比较。生成物里
				// `-a 'table json'` 的书写顺序仍由 TestCompletionWiresEnums 的
				// 字面串钉住，bash 与 zsh 照旧按顺序比。
				//
				// 本例有两格在 fish 上放宽，原因不同：这一格是候选顺序；
				// `--id` 那格是生成物缺 `-r`（见 #1171）。比较用副本，报错仍按
				// 契约顺序打期望值——否则失败信息会把 `[json table]` 说成期望。
				gotCmp = slices.Clone(got)
				slices.Sort(gotCmp)
				wantCmp = slices.Clone(want)
				slices.Sort(wantCmp)
			}
			if !slices.Equal(gotCmp, wantCmp) {
				t.Errorf("候选 = %v，期望 %v", got, want)
			}
		}},
		{"顶层补命令", nil, func(t *testing.T, _ string, got []string) {
			requireNoFormatValues(t, got)
			if !slices.Contains(got, "channel") {
				t.Errorf("候选 = %v，缺一级命令 channel", got)
			}
		}},
		{"channel 补动作", []string{"channel"}, func(t *testing.T, _ string, got []string) {
			requireNoFormatValues(t, got)
			if !slices.Contains(got, "get") {
				t.Errorf("候选 = %v，缺动作 get", got)
			}
		}},
		{"--id 后面不补取值", []string{"channel", "get", "--id"}, func(t *testing.T, shell string, got []string) {
			if shell == "fish" {
				// 已知缺陷，见 #1171：生成物没给吃取值的选项声明 `-r`，fish 就
				// 把 `--id` 当开关，下一个词按位置补出 channel 的动作；bash /
				// zsh 的手写分派遇到选项就 break，所以那边是空。
				//
				// 本格只判候选里不出现 `--format` 的取值：通配分支一旦吃掉所有
				// 键，`--id` 后面就会冒 table/json，这一格照样红。
				requireNoFormatValues(t, got)
				return
			}
			if len(got) != 0 {
				t.Errorf("候选 = %v，期望空", got)
			}
		}},
		{"-o 不补取值", []string{"-o"}, func(t *testing.T, _ string, got []string) {
			requireNoFormatValues(t, got)
		}},
	}

	t.Run("bash", func(t *testing.T) {
		bash, ok := requireCompletionShell(t, "bash")
		if !ok {
			return
		}
		script := bashCompletionScript(data)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tc.check(t, "bash", runBashCompletion(t, bash, script, tc.words))
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
				tc.check(t, "zsh", runZshCompletion(t, zsh, script, tc.words))
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
				tc.check(t, "fish", runFishCompletion(t, fish, script, tc.words))
			})
		}
	})
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
// 返回 COMPREPLY。words 是 `larkaxctl` 之后的部分，正在输入的那个词按空串补。
func runBashCompletion(t *testing.T, bash, script string, words []string) []string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "completion.bash")
	if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
		t.Fatalf("写补全脚本失败：%v", writeErr)
	}
	argv := append(append([]string{"larkaxctl"}, words...), "")
	quoted := make([]string, 0, len(argv))
	for _, word := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(word, "'", `'\''`)+"'")
	}
	driver := "source \"$1\"\n" +
		"COMP_WORDS=(" + strings.Join(quoted, " ") + ")\n" +
		"COMP_CWORD=" + strconv.Itoa(len(argv)-1) + "\n" +
		"_larkaxctl\n" +
		"printf '%s\\n' \"${COMPREPLY[@]}\"\n"
	out, runErr := exec.CommandContext(t.Context(), bash, "-c", driver, "larkaxctl-test", file).CombinedOutput()
	if runErr != nil {
		t.Fatalf("bash 跑补全失败：%v\n%s", runErr, out)
	}
	return strings.Fields(string(out))
}

// runZshCompletion 在真 zsh 里 source 生成的脚本，跑一次 _larkaxctl，
// 返回 compadd 收到的候选。
//
// 生成脚本里的 `compadd -- a b c` 换成把参数打到 stdout 的替身：真 compadd
// 只能在补全上下文里调，直接调用会报错。替身只接手「候选给了谁」，case 的
// 匹配语义仍由生成脚本自己决定。一行一次 compadd，去掉打头的 `--` 就是候选。
func runZshCompletion(t *testing.T, zsh, script string, words []string) []string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "completion.zsh")
	if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
		t.Fatalf("写补全脚本失败：%v", writeErr)
	}
	argv := append(append([]string{"larkaxctl"}, words...), "")
	quoted := make([]string, 0, len(argv))
	for _, word := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(word, "'", `'\''`)+"'")
	}
	// 中途出错有两类，分别由两样东西兜住：
	//   - 进程提前终止（`exit`、整段没跑到最后一行）：结尾那行标记不见了；
	//   - 生成脚本里某条命令失败：只往 stderr 写，驱动照旧往下走、标记照旧打印。
	//     所以成功路径上再断言 stderr 为空（见下面的检查）。
	// --id 那种该给空候选的格子本来就没有 stdout 输出，没有这两样，「静默失败」
	// 与「通过」在输出里长得一样。
	const done = "__completion_driver_done__"
	driver := "compadd() { print -r -- \"$@\" }\n" +
		"compdef() { : }\n" +
		"source \"$1\"\n" +
		"words=(" + strings.Join(quoted, " ") + ")\n" +
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

// runFishCompletion 用 fish 的 `complete -C` 非交互地取候选。
//
// 每条候选可能带一段描述（制表符分隔），这里只要名字。
func runFishCompletion(t *testing.T, fish, script string, words []string) []string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "completion.fish")
	if writeErr := os.WriteFile(file, []byte(script), 0o600); writeErr != nil {
		t.Fatalf("写补全脚本失败：%v", writeErr)
	}
	line := "larkaxctl " + strings.Join(words, " ") + " "
	driver := "source " + fishQuote(file) + "; complete -C " + fishQuote(line)
	out, runErr := exec.CommandContext(t.Context(), fish, "--no-config", "-c", driver).CombinedOutput()
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
