package main

import (
	"errors"
	"fmt"
	"os"
)

// secretUsage 是 secret 子命令的帮助。
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

它挂在 secret 而不是 channel 下面：轮换读写的确实是 channels 那一列，但它管
的是密钥本身的生命周期，与渠道怎么配是两件事。

旧路径：larkaxctl rotate 仍然可用，与 larkaxctl secret rotate 逐字相同。
`

// runSecret 处理 secret 子命令。
func runSecret(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, secretUsage)
		return usageErr(errors.New("secret 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "rotate":
		// 轮换的实现与它的用法文本都在 rotate.go，旧路径接的也是同一个函数
		// （见 main.go 的 legacyCommands）。
		return runRotate(rest)
	case "help", "-h", "--help":
		fmt.Print(secretUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, secretUsage)
		return usageErr(fmt.Errorf("未知的 secret 子命令 %q", action))
	}
}
