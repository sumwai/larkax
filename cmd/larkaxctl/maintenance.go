package main

import (
	"errors"
	"fmt"
	"os"
)

// maintenanceUsage 是 maintenance 子命令的帮助。
//
// 与其余各组同一个模板：第一行一句话说这组命令管什么，然后是用法，最后是
// 需要人知道的那几件事。
const maintenanceUsage = `larkaxctl maintenance 维护运行期数据的留存与过期清理。

用法：
  larkaxctl maintenance idempotency [--dry-run]
        按 settings 里的留存期删除到期的幂等记录（--dry-run 只数一遍，不删）

这一组的共同点是「按留存承诺删掉过期的行」：留存期到了就删，与业务上发生了
什么无关，所以它既不挂账本域也不挂某个业务对象。第三方数据（例如请求快照）
的留存各有自己的分区维护命令，见 larkaxctl partition 与 larkaxctl snapshot。

留存期本身不在命令行上给（见 larkaxctl maintenance idempotency --help）：
同一件事有两个来源时，命令行的那个会悄悄赢，而「留存期到底是几小时」会变成
一件要看是谁执行的才知道的事。

旧路径：larkaxctl idempotency cleanup 仍然可用，与 larkaxctl maintenance idempotency
逐字相同。
`

// runMaintenance 处理 maintenance 子命令。
func runMaintenance(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, maintenanceUsage)
		return usageErr(errors.New("maintenance 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "idempotency":
		// 清理的实现与它的用法文本都在 idempotency.go，旧路径
		// （larkaxctl idempotency cleanup）接的也是同一个函数。
		return runIdempotencyCleanup(rest)
	case "help", "-h", "--help":
		fmt.Print(maintenanceUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, maintenanceUsage)
		return usageErr(fmt.Errorf("未知的 maintenance 子命令 %q", action))
	}
}
