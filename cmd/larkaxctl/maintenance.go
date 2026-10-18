package main

import (
	"errors"
	"fmt"
	"os"
)

// maintenanceHelp 是 maintenance 的两版帮助文本（见 helpText）。
var maintenanceHelp = helpText{short: maintenanceShortUsage, long: maintenanceUsage}

// maintenanceShortUsage 是 maintenance 的短版：`-h` / `--help` 与参数写错时打的那一份。
const maintenanceShortUsage = `larkaxctl maintenance 维护运行期数据的留存与过期清理。

用法：
  larkaxctl maintenance idempotency [--dry-run]
        按 settings 里的留存期删除到期的幂等记录（--dry-run 只数一遍，不删）

留存期不在命令行上给：同一件事有两个来源时，命令行的那个会悄悄赢。

更细的边界：larkaxctl help maintenance。
`

// maintenanceUsage 是 maintenance 的长版：`larkaxctl help maintenance` 与
// `maintenance help`。
//
// 与其余各组同一个模板：第一行一句话说这组命令管什么，然后是用法，最后是
// 需要人知道的那几件事。
const maintenanceUsage = `larkaxctl maintenance 维护运行期数据的留存与过期清理。

用法：
  larkaxctl maintenance idempotency [--dry-run]
        按 settings 里的留存期删除到期的幂等记录（--dry-run 只数一遍，不删）

留存期不在命令行上给：同一件事有两个来源时，命令行的那个会悄悄赢，而
「留存期到底是几小时」会变成一件要看是谁执行的才知道的事。

第三方数据（请求快照、账本分区）的留存各有自己的命令，见 larkaxctl partition
与 larkaxctl snapshot。

旧路径：larkaxctl idempotency cleanup 仍然可用，与 larkaxctl maintenance idempotency
逐字相同。

更细的约定见 docs/cli-reference.md 的「maintenance」一节。
`

// runMaintenance 处理 maintenance 子命令。
func runMaintenance(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, maintenanceShortUsage)
		return usageErr(errors.New("maintenance 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "idempotency":
		// 清理的实现与它的用法文本都在 idempotency.go，旧路径
		// （larkaxctl idempotency cleanup）接的也是同一个函数。
		return runIdempotencyCleanup(rest)
	case "-h", "--help":
		printUsage(os.Stdout, maintenanceShortUsage)
		return nil
	default:
		printUsage(os.Stderr, maintenanceShortUsage)
		return usageErr(fmt.Errorf("未知的 maintenance 子命令 %q", action))
	}
}
