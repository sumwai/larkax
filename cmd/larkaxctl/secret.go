package main

import (
	"errors"
	"fmt"
	"os"
)

// secretHelp 是 secret 的两版帮助文本（见 helpText）。
var secretHelp = helpText{short: secretShortUsage, long: secretUsage}

// secretShortUsage 是 secret 的短版：`-h` / `--help` 与参数写错时打的那一份。
const secretShortUsage = `larkaxctl secret 维护安全凭证的主密钥。

用法：
  larkaxctl secret rotate [--apply|--verify] [--batch-size <N>]
        轮换渠道凭证的主密钥（默认干跑）

密钥从环境变量 LARKAX_MASTER_KEYS 来，这里没有「设置密钥」这个动作。

更细的边界：larkaxctl help secret。
`

// secretUsage 是 secret 的长版：`larkaxctl help secret` 与 `secret help`。
//
// 与其余各组同一个模板：第一行一句话说这组命令管什么，然后是用法，最后是
// 需要人知道的那几件事。
const secretUsage = `larkaxctl secret 维护安全凭证的主密钥。

用法：
  larkaxctl secret rotate [--apply|--verify] [--batch-size <N>]
        轮换渠道凭证的主密钥（默认干跑，逐条说明见 --help）

密钥本身从环境变量（LARKAX_MASTER_KEYS）来，这里没有「设置密钥」这个动作：
写进命令行或库里的密钥会跟着 shell 历史与备份一起流传出去。这一组做的是
**换**密钥——把已有的凭证重新包一份新的，全程不需要明文凭证。

旧路径：larkaxctl rotate 仍然可用，与 larkaxctl secret rotate 逐字相同。

更细的约定见 docs/cli-reference.md 的「secret」一节。
`

// runSecret 处理 secret 子命令。
func runSecret(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, secretShortUsage)
		return usageErr(errors.New("secret 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "rotate":
		// 轮换的实现与它的用法文本都在 rotate.go，旧路径接的也是同一个函数
		// （见 main.go 的 legacyCommands）。
		return runRotate(rest)
	case "-h", "--help":
		printUsage(os.Stdout, secretShortUsage)
		return nil
	default:
		printUsage(os.Stderr, secretShortUsage)
		return usageErr(fmt.Errorf("未知的 secret 子命令 %q", action))
	}
}
