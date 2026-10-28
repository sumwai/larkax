package main

import (
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// legacyAlias 是「旧路径 → 它现在接在哪个正式命令上」，以及两者**是否同一层级**。
//
// 这四条（reconcile、rotate、idempotency、calendar）是搬过家、旧名字必须继续
// 能用的命令。它们不进根帮助页，但真的生效——`main.go` 的 legacyCommands
// 让它们直接接在同一个处理函数上。
//
// sameLevel 决定「流」比不比：`-h` 打哪个流在本仓按层级分档，一级命令与组
// 不是一个档。四对今天**全是跨层级**，所以这一栏现在全 false；留着它是因为
// 判据本身要能表达「同层级时必须连流一起比」，而今天没有这样的配对。
var legacyAlias = []struct {
	// from 是旧名字（一级命令）。
	from string
	// to 是新路径（可能是组，也可能是组下一个动作）。
	to string
	// sameLevel 为 true 时连 stdout/stderr 的归属一起比。
	sameLevel bool
}{
	{from: "reconcile", to: "ledger", sameLevel: false},
	{from: "rotate", to: "secret rotate", sameLevel: false},
	{from: "idempotency", to: "maintenance idempotency", sameLevel: false},
	{from: "calendar", to: "price calendar", sameLevel: false},
}

// TestLegacyCommandsShareTheirTargetsHelp 钉住旧路径打出来的**正文**就是新路径那一份。
//
// 判据是**比较运行时输出**，不是比较两个 helpText 值：那两处本来就是同一个变量
// （`main.go` 里 `{name: "ledger", help: ledgerHelp}` 与 `{name: "reconcile",
// help: ledgerHelp}`），比常量等于自己跟自己比，两个相同或两个不同，什么也证
// 不了。那是 LESSONS.md 第 58 条「同构期望」的另一个形态——期望值不是从被测
// 函数取的，而是**两条被测项本身就是同一份数据**。
//
// # 为什么跨层级只比正文、不比流
//
// `-h` 打哪个流由**层级**决定，这是既有约定而不是缺陷：
//
//   - 一级命令进 `c.run(rest)` 后由各命令自己的 `fs.Usage` 处理，那几处写的是
//     `PrintUsage(os.Stderr, …)`（见 runReconcile、runIdempotency、runCalendar）
//   - 组下的动作同理，走各自命令的 flag 集
//
// 所以 `reconcile`（一级）与 `ledger`（组）的流本来就不该相同，拿它判红等于
// 把约定记成缺陷。**但正文必须逐字相同**——旧名字留着的前提就是它打出的仍是
// 同一份说明。`rotate` 是这四条里唯一自己判 `-h` 并打 stdout 的（见
// rotate.go），所以它恰好与新路径一致；这一条差异由 sameLevel 那一栏表达，
// 不是判据的漏洞。
func TestLegacyCommandsShareTheirTargetsHelp(t *testing.T) {
	for _, alias := range legacyAlias {
		t.Run(alias.from, func(t *testing.T) {
			for _, form := range []string{"-h", "help"} {
				var fromErr error
				fromOut, fromErrOut := captureStreams(t, func() {
					fromErr = dispatch(append([]string{alias.from}, form))
				})
				var toErr error
				toOut, toErrOut := captureStreams(t, func() {
					toErr = dispatch(append(strings.Fields(alias.to), form))
				})

				// 退出码按**进程出口**那一档算，不是 cli.ExitCode(err)：
				// `-h` 让 flag 返回 ErrHelp，而 exitWithFailure 把「要帮助」
				// 映射成 0（见 main.go 的 exitWithFailure）。直接拿
				// cli.ExitCode 会把 `reconcile -h` 算成 2，而它从命令行跑
				// 出来是 0——判据比错了层，两边就对不上。
				if got, want := processExitCode(fromErr), processExitCode(toErr); got != want {
					t.Errorf("%s %s 的退出码是 %d，而 %q 是 %d",
						alias.from, form, got, alias.to, want)
				}

				// 正文：两条流合起来比，逐字节。
				if got, want := fromOut+fromErrOut, toOut+toErrOut; got != want {
					t.Errorf("%s %s 打出来的正文与 %q 不同：\n旧：%q\n新：%q\n"+
						"旧名字留着的前提是它仍打出同一份说明",
						alias.from, form, alias.to, got, want)
				}

				if !alias.sameLevel {
					continue
				}
				// 同层级时流也要一样。今天没有这样的配对，这一支不会跑到——
				// 留着是因为判据要能表达它，而不是等哪天有人补配对时才想起。
				if fromOut != toOut {
					t.Errorf("%s %s 的 stdout 归属与 %q 不同（两者同层级，流该一样）：\n旧：%q\n新：%q",
						alias.from, form, alias.to, fromOut, toOut)
				}
				if fromErrOut != toErrOut {
					t.Errorf("%s %s 的 stderr 归属与 %q 不同（两者同层级，流该一样）：\n旧：%q\n新：%q",
						alias.from, form, alias.to, fromErrOut, toErrOut)
				}
			}
		})
	}
}

// processExitCode 把 dispatch 返回的错误换算成**进程真实退出码**。
//
// 与 cli.ExitCode 的差别只有一处，但那一处正好落在这条门的判据上：`-h` 让
// flag 返回 ErrHelp，而 exitWithFailure（main.go）把「要帮助」映射成 0——
// 拿 cli.ExitCode 直接算会得到 2，与从命令行跑出来的结果不一致。门的判据必须
// 与使用者看到的一致，否则它断言的是一件没人观察到的事。
func processExitCode(err error) int {
	if err == nil || cli.HelpRequested(err) {
		return cli.ExitOK
	}
	return cli.ExitCode(err)
}
