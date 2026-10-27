package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// ── 全局开关 --verbose / -v 与 --quiet / -q ──────────────────────────────
//
// 这两个与 --format / -o 同一档：在最外层从参数里摘掉再分派，所以写在子命令
// **前或后**都认。这一组钉三件事：
//
//  1. 两种拼法都能摘出来，且不吞掉旁边的参数；取值写法与 flag 包一致；
//  2. -V 打版本、-v 是详细模式——这是**行为变化**（`larkaxctl -v` 以前打版本
//     号），变了就得有东西钉着；
//  3. --quiet 压掉的是「给人看」那一档的**全部**输出，而致命错误与
//     --format json 那一份不受影响。

// ── 解析 ────────────────────────────────────────────────────────────────

func TestTakeGlobalBoolAcceptsSpellings(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
		rest []string
	}{
		{"长写法", []string{"--quiet", "doctor"}, true, []string{"doctor"}},
		{"单横线长写法", []string{"-quiet", "doctor"}, true, []string{"doctor"}},
		{"简写", []string{"-q", "doctor"}, true, []string{"doctor"}},
		{"等号写法", []string{"--quiet=true", "doctor"}, true, []string{"doctor"}},
		{"显式关掉", []string{"--quiet=false", "doctor"}, false, []string{"doctor"}},
		{"不给", []string{"doctor"}, false, []string{"doctor"}},
		// 写在子命令之后：人补一个开关时习惯写在最后。
		{"写在后面", []string{"doctor", "-q"}, true, []string{"doctor"}},
		// 后写的赢：与 flag 包对同一个选项写两遍的处置一致。
		{"出现两次", []string{"-q", "doctor", "--quiet=false"}, false, []string{"doctor"}},
		{"出现两次反过来", []string{"--quiet=false", "doctor", "-q"}, true, []string{"doctor"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rest, err := cli.TakeQuietFlag(tc.args)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("开关 = %v，想要 %v", got, tc.want)
			}
			if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
				t.Errorf("剩下的参数 = %q，想要 %q", rest, tc.rest)
			}
		})
	}
}

// 两个开关各认各的名字：--quiet 不该被 --verbose 的解析当成自己人，
// 反过来也一样。
func TestGlobalBoolsDoNotSwallowEachOther(t *testing.T) {
	verbose, rest, err := cli.TakeVerboseFlag([]string{"--quiet", "doctor"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if verbose {
		t.Error("--verbose 的解析不该把 --quiet 当成自己")
	}
	if strings.Join(rest, " ") != "--quiet doctor" {
		t.Errorf("--quiet 该原样留下，得到 %q", rest)
	}

	quiet, rest, err := cli.TakeQuietFlag([]string{"-v", "doctor"})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if quiet {
		t.Error("--quiet 的解析不该把 -v 当成自己")
	}
	if strings.Join(rest, " ") != "-v doctor" {
		t.Errorf("-v 该原样留下，得到 %q", rest)
	}
}

// 取值不是布尔时报参数错（退出码 2），不静默当成 true。
func TestTakeGlobalBoolRejectsNonBooleanValue(t *testing.T) {
	for _, raw := range []string{"yes", "1x", "", "silent"} {
		_, rest, err := cli.TakeQuietFlag([]string{"doctor", "--quiet=" + raw})
		if err == nil {
			t.Fatalf("--quiet=%q 应当报错", raw)
		}
		if rest != nil {
			t.Errorf("报错时不该返回剩下的参数，得到 %q", rest)
		}
		if code := cli.ExitCode(err); code != cli.ExitUsage {
			t.Errorf("--quiet=%q 的退出码 = %d，想要 %d（参数错）", raw, code, cli.ExitUsage)
		}
		// 错误信息要点出是哪个开关的取值不对，否则使用者会去查子命令的参数。
		if !strings.Contains(err.Error(), "--quiet") {
			t.Errorf("错误信息里应当点名 --quiet：%v", err)
		}
	}
}

// ── 过 run() 的那条路 ───────────────────────────────────────────────────

// runWithArgs 按给定的命令行跑一遍 run()，把两个流接回来。
//
// run() 会按参数改写三个包级开关（outputFormat / verboseOutput / quietOutput），
// 所以跑完必须还原：不还原的话，后面那些用例会跟着跑在 json 档或静默档上。
func runWithArgs(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	t.Cleanup(func() {
		cli.OutputFormat = cli.FormatTable
		cli.VerboseOutput = false
		cli.QuietOutput = false
	})

	saved := os.Args
	os.Args = append([]string{"larkaxctl"}, args...)
	t.Cleanup(func() { os.Args = saved })

	var err error
	stdout, stderr := captureStreams(t, func() { err = run() })
	return stdout, stderr, err
}

// -V 打版本；-v 不再是版本，而是详细模式。
//
// 这是一次**行为变化**：`larkaxctl -v` 以前打版本号。脚本里写 `-v` 取版本号的
// 那一类要改成 `-V`，所以这一条必须有东西钉住——它是这次改动里唯一会悄悄改掉
// 既有行为的开关。
func TestVersionTakesUpperVAndVerboseTakesLowerV(t *testing.T) {
	t.Run("-V 打版本", func(t *testing.T) {
		stdout, _, err := runWithArgs(t, "-V")
		if err != nil {
			t.Fatalf("跑失败: %v", err)
		}
		if strings.TrimSpace(stdout) != version {
			t.Errorf("stdout = %q，想要版本号 %q", stdout, version)
		}
	})

	t.Run("--version 仍然打版本", func(t *testing.T) {
		stdout, _, err := runWithArgs(t, "--version")
		if err != nil {
			t.Fatalf("跑失败: %v", err)
		}
		if strings.TrimSpace(stdout) != version {
			t.Errorf("stdout = %q，想要版本号 %q", stdout, version)
		}
	})

	t.Run("-v 是详细模式，不再是版本", func(t *testing.T) {
		stdout, _, err := runWithArgs(t, "-v")
		// 没有子命令：-v 被摘走之后什么都不剩，于是报「缺少子命令」。
		// 这一条同时钉住「它没有再去打版本号」——stdout 上一个字节都没有。
		if err == nil {
			t.Fatal("-v 之后没有子命令，应当报参数错")
		}
		if stdout != "" {
			t.Errorf("-v 不该再打版本号，stdout 得到 %q", stdout)
		}
		if !cli.VerboseOutput {
			t.Error("-v 应当把详细模式打开")
		}
	})

	t.Run("--verbose 也打开详细模式", func(t *testing.T) {
		if _, _, err := runWithArgs(t, "--verbose"); err == nil {
			t.Fatal("没有子命令时应当报参数错")
		}
		if !cli.VerboseOutput {
			t.Error("--verbose 应当把详细模式打开")
		}
	})
}

// 两个开关一起给是参数错，不是「后者赢」：它们要的是相反的东西。
func TestVerboseAndQuietAreMutuallyExclusive(t *testing.T) {
	_, _, err := runWithArgs(t, "doctor", "-q", "-v")
	if err == nil {
		t.Fatal("--quiet 与 --verbose 一起给时应当报参数错")
	}
	if code := cli.ExitCode(err); code != cli.ExitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错）", code, cli.ExitUsage)
	}
	for _, want := range []string{"--verbose", "--quiet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里应当提到 %s：%v", want, err)
		}
	}
}

// 写在子命令前或后都认——全局选项的那条约定，对这两个开关同样成立。
func TestQuietIsAcceptedBeforeAndAfterSubcommand(t *testing.T) {
	setupEnv(t)
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	for _, args := range [][]string{
		{"-q", "doctor"},
		{"doctor", "-q"},
	} {
		stdout, _, err := runWithArgs(t, args...)
		if err == nil {
			t.Fatalf("%v：连不上库时应当报错", args)
		}
		if stdout != "" {
			t.Errorf("%v：静默模式下 stdout 应当为空，得到 %q", args, stdout)
		}
	}
}

// ── --quiet 压掉什么、留下什么 ──────────────────────────────────────────

// doctor 打不出一份报告（连不上库）时：静默模式下 stdout 与 stderr 都空，
// 错误照旧返回。
//
// # 为什么「致命错误仍然输出」这一条落在「错误照旧返回」上
//
// 那一句话是 main 打的（`larkaxctl: %v`），而 main 里有 os.Exit，用例没法
// 直接调它。这里能钉住的是它**拿得到那个错误**——只要 err 非空、又不是
// 「要帮助」，main 就一定会把那行打到 stderr 上。这是这一条的边界：真正
// 打那行字的代码只有人工验过。
func TestQuietKeepsFatalErrorsAndDropsTheReport(t *testing.T) {
	setupEnv(t)
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	// 先确认不静默时确实有一份报告：不然下面那条空 stdout 可能只是
	// 「这份报告本来就打不出来」。
	loudOut, _, loudErr := runWithArgs(t, "doctor")
	if loudErr == nil {
		t.Fatal("连不上库时应当报错")
	}
	if !strings.Contains(loudOut, "数据库") {
		t.Fatalf("不静默时该打出一份报告，stdout 得到 %q", loudOut)
	}

	quietOut, quietErrOut, err := runWithArgs(t, "doctor", "-q")
	if err == nil {
		t.Fatal("连不上库时应当报错——静默模式不该把致命错误也吞掉")
	}
	if cli.HelpRequested(err) {
		t.Fatalf("这不是一次「要帮助」，main 会把它打到 stderr：%v", err)
	}
	if code := cli.ExitCode(err); code != cli.ExitDBUnreach {
		t.Errorf("静默模式不该改变退出码：得到 %d，想要 %d", code, cli.ExitDBUnreach)
	}
	if quietOut != "" {
		t.Errorf("静默模式下 stdout 应当为空，得到 %q", quietOut)
	}
	if quietErrOut != "" {
		t.Errorf("静默模式下提示与警告也该压掉，stderr 得到 %q", quietErrOut)
	}
}

// 机器可读那一档不受 --quiet 影响：脚本要的正是那份。
func TestQuietLeavesJSONOutputAlone(t *testing.T) {
	setupEnv(t)
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	stdout, _, err := runWithArgs(t, "doctor", "-q", "-o", "json")
	if err == nil {
		t.Fatal("连不上库时应当报错")
	}
	var got struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &got); jsonErr != nil {
		t.Fatalf("静默模式下 json 那一份也该照打，stdout 解不回来: %v\n%s", jsonErr, stdout)
	}
	if got.Status != cli.JSONSkipped || got.ExitCode != cli.ExitDBUnreach {
		t.Errorf("status=%q exit_code=%d，想要 %q / %d",
			got.Status, got.ExitCode, cli.JSONSkipped, cli.ExitDBUnreach)
	}
}

// 成功的一次读命令：静默模式下 stdout 上一个字节都没有，而它本来是会打一张表的。
func TestQuietSuppressesTableOutputOnSuccess(t *testing.T) {
	testDB(t)
	setupEnv(t)

	canonical := uniqueName("model")
	if r := runOnce(t, runModel, []string{"create", "--model", canonical, "--display", "展示名"}); r.err != nil {
		t.Fatalf("model create 失败: %v", r.err)
	}

	loudOut, _, err := runWithArgs(t, "model", "list")
	if err != nil {
		t.Fatalf("model list 失败: %v", err)
	}
	if !strings.Contains(loudOut, canonical) {
		t.Fatalf("不静默时该列出刚建的模型，stdout 得到 %q", loudOut)
	}

	quietOut, quietErrOut, err := runWithArgs(t, "model", "list", "-q")
	if err != nil {
		t.Fatalf("静默模式下命令仍然要跑成功: %v", err)
	}
	if quietOut != "" {
		t.Errorf("静默模式下 stdout 应当为空，得到 %q", quietOut)
	}
	if quietErrOut != "" {
		t.Errorf("静默模式下 stderr 应当为空，得到 %q", quietErrOut)
	}

	// 同一台机器上再确认一次：json 那一档照打同一份清单。
	jsonOut, _, err := runWithArgs(t, "model", "list", "-q", "-o", "json")
	if err != nil {
		t.Fatalf("静默 + json 应当跑成功: %v", err)
	}
	assertOnlyJSON(t, jsonOut)
	if !strings.Contains(jsonOut, canonical) {
		t.Errorf("json 那一份里该有刚建的模型：\n%s", jsonOut)
	}
}
