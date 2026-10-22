package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// 这一层是「用户级履约操作」的正式入口：user plan / wallet / key / usage。
//
// 每个动作的实现仍然只有一份，在 plan.go / wallet.go / apikey.go / usage.go 里；
// 这里只把位置参数（用户、套餐、订阅 ID、API Key ID）翻成那几条命令认的旗标形状，
// 再交给同一个函数。翻而不是复制，是因为输出契约与退出码都长在那些函数里，复制
// 一份就等于让「新旧两条路径说的是同一件事」变成一句要靠人守着的约定。
//
// 顶层 plan / wallet / apikey / usage 的用户级写法在这一版仍可用（见 main.go 的
// 各分发函数），命中时在 stderr 打一行指向这里。

// userPlanHelp 是 user plan 的两版帮助文本（见 helpText）。
var userPlanHelp = helpText{short: userPlanShortUsage, long: userPlanUsage}

// userPlanShortUsage 是 user plan 的短版：`-h` / `--help` 与参数写错时打的那一份。
const userPlanShortUsage = `larkaxctl user plan 管理一个用户持有的订阅与扣费顺序。
用法：
  larkaxctl user plan grant <用户> <套餐> --ref <发放单号> --expires-at <RFC3339> [--dry-run]
  larkaxctl user plan list <用户>
  larkaxctl user plan void <订阅 ID> --ref <作废单号> [--status <expired|canceled>] [--dry-run]
  larkaxctl user plan order show <用户>
  larkaxctl user plan order set <用户> [--order <顺序> | --clear]

--ref 是幂等键；order 的三档与 --clear 见长版。
grant 与 void 的 --dry-run 打印将写入的订阅与分录后回滚。
更细的边界：larkaxctl user plan help。
`

// userPlanUsage 是 user plan 的长版：`larkaxctl user plan help`。
const userPlanUsage = `larkaxctl user plan 管理一个用户持有的订阅与扣费顺序。

用法：
  larkaxctl user plan grant <用户> <套餐> --ref <发放单号> --expires-at <RFC3339> [--dry-run]
  larkaxctl user plan list <用户>
  larkaxctl user plan void <订阅 ID> --ref <作废单号> [--status <expired|canceled>] [--dry-run]
  larkaxctl user plan order show <用户>
  larkaxctl user plan order set <用户> [--order <顺序> | --clear]

grant 建一条订阅，并把面值记进 prepaid:<订阅 ID>。--ref 是发放单号，也是幂等键：
同一个单号重跑只会发一次，第二次打「已经发过」。--expires-at 必须晚于现在。

void 作废一条订阅，把剩余面值转回平台收入。默认按人工作废（canceled）收尾；
--status expired 表示到期作废。--ref 是作废单号，同样是幂等键。

grant 与 void 都接受 --dry-run：走与真跑同一条写路径，把订阅与分录打出来之后回滚，
user_plans 与账本一条都不落，作废时的剩余面值仍然从账本现算。**除序列号（订阅 id）外，
预览与真跑逐字相等**：分录的科目、金额、借贷方向与余额前后完全相同。订阅 id 是这次
事务从序列取到的值，事务回滚不退号，所以真跑会拿到下一个。

list 列出该用户的全部订阅（有效的、到期的、已作废的都在）；「剩余」那一栏是
prepaid 科目的余额。

order show / order set 管扣费顺序的三档：plan_first（先额度包后余额，平台默认）、
balance_first（先余额后额度包）、balance_only（只走余额）。用户上的取值覆盖平台默认；
--clear 清掉覆盖，回到跟随平台默认。平台默认在 settings 的 plans.charge_order 上
（larkaxctl settings set plans.charge_order ...）。

更细的约定见 docs/cli-reference.md 的「user」一节。
`

// userWalletHelp 是 user wallet 的两版帮助文本（见 helpText）。
var userWalletHelp = helpText{short: userWalletShortUsage, long: userWalletUsage}

// userWalletShortUsage 是 user wallet 的短版。
const userWalletShortUsage = `larkaxctl user wallet 管理一个用户的钱包。
用法：
  larkaxctl user wallet show <用户>
  larkaxctl user wallet credit <用户> --amount <金额> --ref <外部单据号> [--asset <资产>] [--gateway <来源>] [--dry-run]
  larkaxctl user wallet adjust <用户> --amount <带符号金额> --ref <外部单据号> --memo <原因> --by <操作人> [--dry-run]
  larkaxctl user wallet refund <用户> --amount <金额> --ref <外部单据号> [--memo <原因>] [--asset <资产>]
                        [--merchant <ID 或用户名> --merchant-share <金额>] [--upstream-cost <金额>] [--dry-run]

credit / adjust / refund 要求钱包已存在；单据号必填且是幂等键。
三条都支持 --dry-run：打印将写入的分录后回滚，不动库。
更细的边界：larkaxctl user wallet help。
`

// userWalletUsage 是 user wallet 的长版。
const userWalletUsage = `larkaxctl user wallet 管理一个用户的钱包。

用法：
  larkaxctl user wallet show <用户>
  larkaxctl user wallet credit <用户> --amount <金额> --ref <外部单据号>
                                  [--asset <资产>] [--gateway <来源>] [--dry-run]
  larkaxctl user wallet adjust <用户> --amount <带符号金额> --ref <外部单据号>
                                  --memo <原因> --by <操作人> [--dry-run]
  larkaxctl user wallet refund <用户> --amount <金额> --ref <外部单据号>
                                  [--asset <资产>] [--memo <原因>]
                                  [--merchant <ID 或用户名> --merchant-share <金额>]
                                  [--upstream-cost <金额>] [--dry-run]

show 列出这个用户的钱包与余额快照，一栏一个字段：用户 ID、用户名、资产、余额、
冻结。它与顶层的 wallet list --user 是同一份清单、同一份输出。

credit / adjust / refund 都走账本写分录，并在同一个事务里更新余额快照。
--ref 是外部单据号，必填且是幂等键：同一个单号重跑只会入账一次，配不同的金额、
用户或原因则是冲突（退出 4），一笔都不写。三条命令都要求钱包已经存在。

adjust 的 --amount 带符号；--memo 与 --by 必填。refund 的 --merchant 与
--merchant-share 要一起给。

credit / adjust / refund 都接受 --dry-run：走与真跑同一条写路径（同一份校验、同一份
幂等判据），把这一笔要写的分录逐条打出来之后回滚，余额、账本行与钱包快照都不变。
**预览与真跑逐字相等**：分录的科目、金额、借贷方向与余额前后完全相同。
末尾一句「干跑：以上内容没有写进数据库。」把两者分开。
参数错与业务拒绝在干跑下同样报错、同样退出码。

开通钱包、商户提现与支付通道那几组不在这一档：它们仍在 larkaxctl wallet 下
（create / payout / payment）。

更细的约定见 docs/cli-reference.md 的「user」一节。
`

// userKeyHelp 是 user key 的两版帮助文本（见 helpText）。
var userKeyHelp = helpText{short: userKeyShortUsage, long: userKeyUsage}

// userKeyShortUsage 是 user key 的短版。
const userKeyShortUsage = `larkaxctl user key 管理一个用户的 API Key。
用法：
  larkaxctl user key list <用户>
  larkaxctl user key create <用户> [--name <备注>] [--expires-at <RFC3339>]
                             [--rpm <整数>] [--quota <金额>]
  larkaxctl user key plans show <API Key ID>
  larkaxctl user key plans set <API Key ID> [--plans <订阅 ID,...> | --all]

create 的明文只打印这一次；改状态、备注与额度用 larkaxctl apikey update。
更细的边界：larkaxctl user key help。
`

// userKeyUsage 是 user key 的长版。
const userKeyUsage = `larkaxctl user key 管理一个用户的 API Key。

用法：
  larkaxctl user key list <用户>
  larkaxctl user key create <用户> [--name <备注>] [--expires-at <RFC3339>]
                             [--rpm <整数>] [--quota <金额>]
  larkaxctl user key plans show <API Key ID>
  larkaxctl user key plans set <API Key ID> [--plans <订阅 ID,...> | --all]

list 与 create 以用户为核心宾语，用户写成位置参数。create 生成一把新 key，
只把哈希入库：**明文只打印这一次**，库里存的是 SHA-256 哈希，丢了没有任何办法
取回，只能重建一把。明文单独打在 stdout 上，其余信息走 stderr。

--rpm 与 --quota 都按明文给：0 不是「不限」，是「一个都不放行」；负数一律拒绝。

plans 管这把 Key 能用这个用户的哪些订阅。没有配过名单（或用 --all 清空之后）
表示可以用该用户名下全部订阅；名单里每一条都必须是这把 Key 所属用户的订阅，
否则当场报错、一行都不写。

改状态、备注、额度与过期时刻，以及按 Key ID 读一把 Key 的现状，仍在
larkaxctl apikey 下（update / get）：那两条的核心宾语是 Key 本身，不是用户。

更细的约定见 docs/cli-reference.md 的「user」一节。
`

// userUsageHelp 是 user usage 的两版帮助文本（见 helpText）。
var userUsageHelp = helpText{short: userUsageShortUsage, long: userUsageUsage}

// userUsageShortUsage 是 user usage 的短版。
const userUsageShortUsage = `larkaxctl user usage 读一个用户的用量与计费。
用法：
  larkaxctl user usage events <用户> [--from <时刻>] [--to <时刻>] [--limit <条数>]
  larkaxctl user usage requests <用户> [--from <时刻>] [--to <时刻>] [--limit <条数>]

两条都只读，不改任何一行；看全量用 larkaxctl usage。
更细的边界：larkaxctl user usage help。
`

// userUsageUsage 是 user usage 的长版。
const userUsageUsage = `larkaxctl user usage 读一个用户的用量与计费。

用法：
  larkaxctl user usage events <用户> [--from <时刻>] [--to <时刻>] [--limit <条数>]
  larkaxctl user usage requests <用户> [--from <时刻>] [--to <时刻>] [--limit <条数>]

两条都只读，不改任何一行、不写任何快照。events 读 usage_events，是计量的底稿
（用了多少 token、金额按哪条口径算出来的）；requests 读 billing_requests，是每次
请求的预留与结算。时刻用 RFC3339 或只写日期（按 UTC 零点算），区间左闭右开；
--limit 默认 50，0 表示不限。

不给 --from / --to 就是不限时间，那时每个月分区都会被扫到。要看不限用户的全量，
仍用 larkaxctl usage events|requests（不带 --user）。

更细的约定见 docs/cli-reference.md 的「user」一节。
`

// runUserPlan 分发 user plan 的子命令。
func runUserPlan(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, userPlanHelp.short)
		return usageErr(errors.New("user plan 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "grant":
		return runUserPlanGrant(rest)
	case "list":
		return runUserPlanList(rest)
	case "void":
		return runUserPlanVoid(rest)
	case "order":
		return runUserPlanOrder(rest)
	case "-h", "--help":
		printUsage(os.Stdout, userPlanHelp.short)
		return nil
	case "help":
		printUsage(os.Stdout, userPlanHelp.long)
		return nil
	default:
		printUsage(os.Stderr, userPlanHelp.short)
		return usageErr(fmt.Errorf("未知的 user plan 子命令 %q", action))
	}
}

func runUserPlanGrant(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	plan, rest, err := takeOperand(rest, "<套餐>")
	if err != nil {
		return err
	}
	return runPlanGrant(append([]string{"--user", user, "--plan", plan}, rest...))
}

func runUserPlanList(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runPlanSubscriptions(append([]string{"--user", user}, rest...))
}

func runUserPlanVoid(args []string) error {
	id, rest, err := takeOperand(args, "<订阅 ID>")
	if err != nil {
		return err
	}
	return runPlanVoid(append([]string{"--id", id}, rest...))
}

func runUserPlanOrder(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, userPlanHelp.short)
		return usageErr(errors.New("user plan order 需要 show 或 set"))
	}
	action, rest := args[0], args[1:]
	switch action {
	case "show":
		return runUserPlanOrderShow(rest)
	case "set":
		return runUserPlanOrderSet(rest)
	case "-h", "--help":
		printUsage(os.Stdout, userPlanHelp.short)
		return nil
	case "help":
		printUsage(os.Stdout, userPlanHelp.long)
		return nil
	default:
		return usageErr(fmt.Errorf("未知的 user plan order 子命令 %q", action))
	}
}

func runUserPlanOrderShow(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runPlanOrderShow(append([]string{"--user", user}, rest...))
}

func runUserPlanOrderSet(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runPlanOrderSet(append([]string{"--user", user}, rest...))
}

// runUserWallet 分发 user wallet 的子命令。
func runUserWallet(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, userWalletHelp.short)
		return usageErr(errors.New("user wallet 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "show":
		return runUserWalletShow(rest)
	case "credit":
		return runUserWalletCredit(rest)
	case "adjust":
		return runUserWalletAdjust(rest)
	case "refund":
		return runUserWalletRefund(rest)
	case "-h", "--help":
		printUsage(os.Stdout, userWalletHelp.short)
		return nil
	case "help":
		printUsage(os.Stdout, userWalletHelp.long)
		return nil
	default:
		printUsage(os.Stderr, userWalletHelp.short)
		return usageErr(fmt.Errorf("未知的 user wallet 子命令 %q", action))
	}
}

func runUserWalletShow(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runWalletList(append([]string{"--user", user}, rest...))
}

func runUserWalletCredit(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runWalletCredit(append([]string{"--user", user}, rest...))
}

func runUserWalletAdjust(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runWalletAdjust(append([]string{"--user", user}, rest...))
}

func runUserWalletRefund(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runWalletRefund(append([]string{"--user", user}, rest...))
}

// runUserKey 分发 user key 的子命令。
func runUserKey(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, userKeyHelp.short)
		return usageErr(errors.New("user key 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "list":
		return runUserKeyList(rest)
	case "create":
		return runUserKeyCreate(rest)
	case "plans":
		return runUserKeyPlans(rest)
	case "-h", "--help":
		printUsage(os.Stdout, userKeyHelp.short)
		return nil
	case "help":
		printUsage(os.Stdout, userKeyHelp.long)
		return nil
	default:
		printUsage(os.Stderr, userKeyHelp.short)
		return usageErr(fmt.Errorf("未知的 user key 子命令 %q", action))
	}
}

func runUserKeyList(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runAPIKeyList(append([]string{"--user", user}, rest...))
}

func runUserKeyCreate(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runAPIKeyCreate(append([]string{"--user", user}, rest...))
}

// runUserKeyPlans 分发 user key plans 的子命令（一把 Key 能用哪些订阅）。
func runUserKeyPlans(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, userKeyHelp.short)
		return usageErr(errors.New("user key plans 需要 show 或 set"))
	}
	action, rest := args[0], args[1:]
	switch action {
	case "show":
		return runUserKeyPlansShow(rest)
	case "set":
		return runUserKeyPlansSet(rest)
	case "-h", "--help":
		printUsage(os.Stdout, userKeyHelp.short)
		return nil
	case "help":
		printUsage(os.Stdout, userKeyHelp.long)
		return nil
	default:
		return usageErr(fmt.Errorf("未知的 user key plans 子命令 %q", action))
	}
}

func runUserKeyPlansShow(args []string) error {
	id, rest, err := takeOperand(args, "<API Key ID>")
	if err != nil {
		return err
	}
	return runPlanKeyShow(append([]string{"--id", id}, rest...))
}

func runUserKeyPlansSet(args []string) error {
	id, rest, err := takeOperand(args, "<API Key ID>")
	if err != nil {
		return err
	}
	return runPlanKeySet(append([]string{"--id", id}, rest...))
}

// runUserUsage 分发 user usage 的子命令。
func runUserUsage(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, userUsageHelp.short)
		return usageErr(errors.New("user usage 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "events":
		return runUserUsageEvents(rest)
	case "requests":
		return runUserUsageRequests(rest)
	case "-h", "--help":
		printUsage(os.Stdout, userUsageHelp.short)
		return nil
	case "help":
		printUsage(os.Stdout, userUsageHelp.long)
		return nil
	default:
		printUsage(os.Stderr, userUsageHelp.short)
		return usageErr(fmt.Errorf("未知的 user usage 子命令 %q", action))
	}
}

func runUserUsageEvents(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runUsageEvents(append([]string{"--user", user}, rest...))
}

func runUserUsageRequests(args []string) error {
	user, rest, err := takeOperand(args, "<用户>")
	if err != nil {
		return err
	}
	return runUsageRequests(append([]string{"--user", user}, rest...))
}

// takeOperand 取走一个位置参数（用户、套餐、订阅 ID 或 Key ID）。
//
// 以 - 开头的不当位置参数：那样报出来的是「不认识这个旗标」，而人写的是少了一个
// 宾语；把两件事混成一句，读的人会去查旗标名。
func takeOperand(args []string, name string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || strings.TrimSpace(args[0]) == "" {
		return "", nil, usageErr(fmt.Errorf("缺少 %s：把它写成位置参数", name))
	}
	return args[0], args[1:], nil
}

// noteLegacyUserPath 打一行指向新路径。
//
// 走 stderr 的「提示」那一档：它是附注，不是这一次的结果（见 docs/cli-output.md）；
// -q 之下跟着一起压掉，--format json 的 stdout 不受影响。
func noteLegacyUserPath(message string) {
	_, _ = fmt.Fprintf(humanStderr(),
		"提示：%s；旧路径至少保留一个发布周期。\n", message)
}

// runLegacyUserPath 跑一条搬过家的命令，随后补一句新路径在哪。
//
// **成功与失败都打**：提示说的是「这条写法搬去哪了」，与这一次跑没跑成无关。
// 失败时更要说——「Key 不存在」这类结果只说明数据不在，而人手上那条命令可能
// 早就该换了；只在成功时提示，等于让最需要知道的人最后一个知道。
//
// **参数错（退出码 2）不打**：那说明命令本身还没写成形（缺 --id、多了个位置
// 参数），此时 stderr 上已经是一屏用法，再补一句搬家提示只会把「先把参数写对」
// 挤到后面。
//
// 提示排在命令之后，而不是之前：读类命令的清单已经打到 stdout 上了，提示必须
// 落在后面才不会被当成结果的一部分；写在前面还得在两处各判一次「跑成没有」。
func runLegacyUserPath(message string, run func() error) error {
	err := run()
	if exitCode(err) != exitUsage {
		noteLegacyUserPath(message)
	}
	return err
}

// usesUserFlag 判断参数里有没有 --user / -u（含 --user=值 的写法）。
//
// 只给「仍然有两种用法」的那几条旧路径用：带 --user 时那一句是在说用户级视图，
// 不带时命令回答的是全量，没有可以指过去的新路径。
func usesUserFlag(args []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--user", "-user", "-u":
			return true
		}
	}
	return false
}
