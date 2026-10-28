package main

import (
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// legacyAlias 是「旧路径 → 它现在接在哪个正式命令上」。
//
// 这四条（reconcile、rotate、idempotency、calendar）是搬过家、旧名字必须继续
// 能用的命令。它们不进根帮助页，但真的生效——`main.go` 的 legacyCommands
// 让它们直接接在同一个处理函数上。
var legacyAlias = []struct {
	// from 是旧名字（一级命令）。
	from string
	// to 是新路径（可能是组，也可能是组下一个动作）。
	to string
}{
	{from: "reconcile", to: "ledger"},
	{from: "rotate", to: "secret rotate"},
	{from: "idempotency", to: "maintenance idempotency"},
	{from: "calendar", to: "price calendar"},
}

// TestLegacyCommandsShareTheirTargetsHelp 钉住旧路径打出来的**正文**就是新路径那一份。
//
// 判据是**比较运行时输出**，不是比较两个 helpText 值：那两处本来就是同一个变量
// （`main.go` 里 `{name: "ledger", help: ledgerHelp}` 与 `{name: "reconcile",
// help: ledgerHelp}`），比常量等于自己跟自己比，两个相同或两个不同，什么也证
// 不了。那是 LESSONS.md 第 58 条「同构期望」的另一个形态——期望值不是从被测
// 函数取的，而是**两条被测项本身就是同一份数据**。
//
// # 为什么只比正文、不比流
//
// 旧名字留下的前提是**仍打出同一份说明**，不是「连 stdout/stderr 归属也一致」。
// 流在两个方向上都不一致，而且与新旧无关：
//
//   - 旧路径内部就不齐：`rotate`、`reconcile` 的 `-h` 走 `fs.Usage`，打
//     **stderr**；`idempotency`、`calendar` 手写了 `case "-h", "--help"`，打
//     **stdout**。四条旧命令本身分两派。
//   - 一级命令整体上也不齐：28 条一级命令（24 `commandGroups` + 4
//     `legacyCommands`）里，`-h` 打 stdout 的 19 条、打 stderr 的 9 条，
//     无一条两个流都非空（2026-10-28 实测）。九条 stderr 是
//     `apikey consent doctor migrate plan reconcile rotate settings user`。
//
// 所以「流由层级决定」这个说法是错的，拿它当判据会把约定记成缺陷。真正的
// 分界是**顺序**：在 `fs.Parse` 之前自己看第一个参数的打 stdout（`account`、
// `version` 这一派）；先交给 `fs.Parse` 的打 stderr。
//
// **`consent` 是这条分界的反例，也是它唯一需要说清的地方**：它手写了
// `case "-h", "--help"`（consent.go:123），却打 stderr——因为 `runConsent`
// 一进门就 `fs.Parse(args)`（consent.go:108），flag 包遇到 `-h` 当场返回
// `ErrHelp` 并调 `fs.Usage`，那一行 switch **根本到不了**。实测 `consent -h`
// 打 stderr 704 字节，而 `consent help` 走手写 case 打 stdout 2464 字节。
//
// 「手写 `-h` case」这个观察可以留着，但要读成**必要不充分**：19 条 stdout
// 确实都有手写 case，而它不保证流。写成「分界」会在 `consent` 上翻车，然后
// 读者不再信这段。
//
// 这是本仓一处既有不一致，与旧路径无关，P2 收口时一并看（记在
// `leaders/CLI.md`）。这条门要钉的是正文，流不归它管。
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
