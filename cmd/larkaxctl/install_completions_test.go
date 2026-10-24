package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 本文件钉住 make install 与三个 shell 的补全之间的关系。
//
// 补全装到哪儿、文件叫什么名字，是各个 shell 自己的约定：放错目录不会有任何一步
// 失败，文件躺在磁盘上，Tab 一个候选也不给。所以这里跑一次真的 `make install`，
// 再让三个 shell 从**装好的位置**把脚本找出来补一次——目录约定与生成内容一起验。
//
// 下面的三个落点在这里重写了一遍。它不是实现的抄写，而是这一条用例的契约：
// 值来自 docs/versioning.md 的「从源码树安装」一节与 Makefile 顶部那几个变量，
// 三处任意一处改了而这里没改，用例就红。

// installTestVersion 是这条用例注入的版本号。取一个不是 dev 的值：`make install`
// 会把装出来的二进制跑一次 --version 比对，那个比对只有在这里才走真实路径。
const installTestVersion = "install-test"

// TestMakeInstallWritesShellCompletions 端到端跑一遍 make install，验三件事：
// 三个补全文件落在各 shell 认的位置、内容与 `larkaxctl completion <shell>` 逐字节
// 相同、真 shell 能从那个位置找出来补出候选。最后跑一遍 make uninstall，确认装的
// 东西都收得回去。
func TestMakeInstallWritesShellCompletions(t *testing.T) {
	prefix := t.TempDir()
	runMake(t, "install", "PREFIX="+prefix, "VERSION="+installTestVersion)

	// 每个 shell 的落点，键是 completionShells 里的名字。
	files := map[string]string{
		"bash": filepath.Join(prefix, "share", "bash-completion", "completions", "larkaxctl"),
		"zsh":  filepath.Join(prefix, "share", "zsh", "site-functions", "_larkaxctl"),
		"fish": filepath.Join(prefix, "share", "fish", "vendor_completions.d", "larkaxctl.fish"),
	}

	data := buildCompletionData()
	for _, shell := range completionShells {
		t.Run("文件/"+shell, func(t *testing.T) {
			path := files[shell]
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("make install 没把 %s 补全写到 %s：%v", shell, path, err)
			}
			if want := generateCompletionScript(t, data, shell); string(got) != want {
				t.Errorf("%s 补全与 `larkaxctl completion %s` 的输出不一致：装进去的是旧版本？",
					shell, shell)
			}
		})
	}

	// 真 shell 那一格：候选集合与 cmd/larkaxctl 已有的行为用例同源，只是加载
	// 路径换成了「装好的目录」，而不是把脚本内容另存一份再 source。
	behavior := []struct {
		name  string
		words []string
		check func(t *testing.T, shell string, got []string)
	}{
		{"顶层补一级命令", nil, func(t *testing.T, _ string, got []string) {
			requireNoFormatValues(t, got)
			if !slices.Contains(got, "channel") {
				t.Errorf("候选 = %v，缺一级命令 channel", got)
			}
		}},
		{"--format 补取值", []string{"--format"}, func(t *testing.T, shell string, got []string) {
			requireEnumCandidates(t, shell, got, []string{"table", "json"})
		}},
	}

	t.Run("真 shell/bash", func(t *testing.T) {
		bash, ok := requireCompletionShell(t, "bash")
		if !ok {
			return
		}
		for _, tc := range behavior {
			t.Run(tc.name, func(t *testing.T) {
				tc.check(t, "bash", installedBashCompletion(t, bash, prefix, tc.words))
			})
		}
	})

	t.Run("真 shell/zsh", func(t *testing.T) {
		zsh, ok := requireCompletionShell(t, "zsh")
		if !ok {
			return
		}
		for _, tc := range behavior {
			t.Run(tc.name, func(t *testing.T) {
				tc.check(t, "zsh", installedZshCompletion(t, zsh, filepath.Dir(files["zsh"]), tc.words))
			})
		}
	})

	t.Run("真 shell/fish", func(t *testing.T) {
		fish, ok := requireCompletionShell(t, "fish")
		if !ok {
			return
		}
		for _, tc := range behavior {
			t.Run(tc.name, func(t *testing.T) {
				tc.check(t, "fish", installedFishCompletion(t, fish,
					filepath.Dir(files["fish"]), filepath.Join(prefix, "bin"), tc.words))
			})
		}
	})

	t.Run("uninstall", func(t *testing.T) {
		runMake(t, "uninstall", "PREFIX="+prefix)

		leftovers := []string{
			filepath.Join(prefix, "bin", "larkax"),
			filepath.Join(prefix, "bin", "larkaxctl"),
			files["bash"], files["zsh"], files["fish"],
		}
		for _, path := range leftovers {
			if _, err := os.Stat(path); err == nil {
				t.Errorf("make uninstall 之后 %s 还在", path)
			}
		}
	})
}

// runMake 在仓库根跑一个 make 目标。前缀都显式给出来：用例不许碰真实的 HOME。
func runMake(t *testing.T, target string, args ...string) {
	t.Helper()
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Fatalf("找不到 make：install 的落点由它决定，没有它这条用例什么也证明不了（%v）", err)
	}
	argv := append([]string{"-C", filepath.Join("..", ".."), target}, args...)
	cmd := exec.CommandContext(t.Context(), makePath, argv...)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("make %s 失败：%v\n%s", target, runErr, out)
	}
}

// bashCompletionLoader 找 bash-completion 的入口脚本。
//
// 只有它能证明「文件放在 $BASH_COMPLETION_USER_DIR 下就会按需加载」——直接 source
// 装好的那一份只证明内容可用，不证明目录约定。
func bashCompletionLoader(t *testing.T) string {
	t.Helper()
	for _, path := range []string{
		"/usr/share/bash-completion/bash_completion",
		"/etc/bash_completion",
	} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	if os.Getenv(completionShellsRequiredEnv) != "" {
		t.Fatalf("%s 已设，但找不到 bash-completion 的入口脚本：CI 的装壳那一步应当装它",
			completionShellsRequiredEnv)
	}
	return ""
}

// installedBashCompletion 让 bash 从装好的目录按需加载补全，再跑一次 _larkaxctl。
//
// 加载走 bash-completion 的 `_completion_loader`，不是 source 文件本身：
// 这条用例要验的正是「文件放在这个目录下就够」。机器上没有 bash-completion 时
// 退化成 source 装好的那一份，并记一行日志说明目录约定这一格没验——
// 静默降级与验证通过在输出里长得一样（LESSONS.md 第 16 条）。
func installedBashCompletion(t *testing.T, bash, prefix string, words []string) []string {
	t.Helper()
	userDir := filepath.Join(prefix, "share", "bash-completion")

	var load string
	if loader := bashCompletionLoader(t); loader != "" {
		load = "BASH_COMPLETION_USER_DIR=" + shellQuote(userDir) + "\n" +
			"export BASH_COMPLETION_USER_DIR\n" +
			"source " + shellQuote(loader) + "\n" +
			"_completion_loader larkaxctl\n"
	} else {
		t.Logf("这台机器没有 bash-completion：这一格只 source 装好的文件，目录约定没验")
		load = "source " + shellQuote(filepath.Join(userDir, "completions", "larkaxctl")) + "\n"
	}

	argv := append(append([]string{"larkaxctl"}, words...), "")
	driver := load +
		"declare -F _larkaxctl >/dev/null || { echo '加载不到 _larkaxctl'; exit 3; }\n" +
		"COMP_WORDS=(" + shellQuoteAll(argv) + ")\n" +
		"COMP_CWORD=" + strconv.Itoa(len(argv)-1) + "\n" +
		"_larkaxctl\n" +
		"printf '%s\\n' \"${COMPREPLY[@]}\"\n"
	out, runErr := exec.CommandContext(t.Context(), bash, "-c", driver, "larkaxctl-test").CombinedOutput()
	if runErr != nil {
		t.Fatalf("bash 从装好的目录取候选失败：%v\n%s", runErr, out)
	}
	return strings.Fields(string(out))
}

// installedZshCompletion 把装好的 site-functions 目录放进 fpath，靠 autoload
// 取到 _larkaxctl 再跑一次。autoload 才是 zsh 的目录约定：source 一个已知路径
// 证明不了那个目录被认。
//
// compadd 与 _files 照现有行为用例的做法换成替身（真 compadd 只能在补全上下文里
// 调用）。结尾那行标记与 stderr 检查同样照抄：中途失败时标记不见了，而失败的命令
// 只往 stderr 写。
func installedZshCompletion(t *testing.T, zsh, dir string, words []string) []string {
	t.Helper()
	const done = "__installed_zsh_done__"
	argv := append(append([]string{"larkaxctl"}, words...), "")
	driver := "fpath=(" + shellQuote(dir) + " $fpath)\n" +
		"autoload -Uz _larkaxctl\n" +
		"(( $+functions[_larkaxctl] )) || { print -u2 'fpath 里找不到 _larkaxctl'; exit 3 }\n" +
		"compadd() { print -r -- \"$@\" }\n" +
		"_files() { print -r -- \"-- " + filesStubCandidate + "\" }\n" +
		"compdef() { : }\n" +
		"words=(" + shellQuoteAll(argv) + ")\n" +
		"CURRENT=" + strconv.Itoa(len(argv)) + "\n" +
		"_larkaxctl\n" +
		"print -r -- " + done + "\n"

	var stderr strings.Builder
	cmd := exec.CommandContext(t.Context(), zsh, "-f", "-c", driver, "larkaxctl-test")
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if runErr != nil {
		t.Fatalf("zsh 从 fpath 取候选失败：%v\n%s", runErr, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if lines[len(lines)-1] != done {
		t.Fatalf("zsh 没跑到结尾（补全函数中途出错？）\nstdout=%q\nstderr=%s", out, stderr.String())
	}
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

// installedFishCompletion 把装好的 vendor_completions.d 加进 fish_complete_path，
// 再用 `complete -C` 取候选。
//
// PATH 里也要有装好的 bin 目录：fish **只为 PATH 里找得到的命令**按需加载补全
// （否则一个名字就能拉一个文件进来）。真实使用里二进制就在 PATH 里；用例不补这
// 一句，换来的是「文件装对了而一条候选也没有」——那是另一种失败，与这里要验的
// 目录约定无关，而且它看起来像生成器坏了。
//
// 工作目录是空的临时目录：吃取值的选项带 -r，fish 会给它补文件名，在仓库目录里跑
// 会把「没有候选」那几格填满。
func installedFishCompletion(t *testing.T, fish, dir, binDir string, words []string) []string {
	t.Helper()
	line := "larkaxctl " + strings.Join(words, " ") + " "
	driver := "set -p fish_complete_path " + fishQuote(dir) + "; " +
		"set -p PATH " + fishQuote(binDir) + "; " +
		"complete -C " + fishQuote(line)
	cmd := exec.CommandContext(t.Context(), fish, "--no-config", "-c", driver)
	cmd.Dir = t.TempDir()
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("fish 从 vendor_completions.d 取候选失败：%v\n%s", runErr, out)
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

// shellQuote 把一段文本包成单引号形式，给 bash / zsh 驱动脚本用。
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// shellQuoteAll 把一串词包成 shell 数组里的样子。
func shellQuoteAll(words []string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, shellQuote(word))
	}
	return strings.Join(quoted, " ")
}
