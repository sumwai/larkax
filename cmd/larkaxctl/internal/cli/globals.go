package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// 这一层管**全局控制选项**里那两个开关：--verbose / -v 与 --quiet / -q。
//
// 它们与 --format / -o 同一档：写在子命令**前或后**都认，所以都在最外层从参数
// 里摘掉再分派（见 cmd/larkaxctl 的 main.go 的 run），而不是逐条子命令各注册一遍——二十来条
// 子命令各注册一遍时，漏掉一条的表现是那条命令认不出这个选项。
//
// 两个都是布尔开关，取值的拼法也一致：`--quiet`、`-quiet`、`-q`，以及
// `--quiet=false` 这种显式给值的写法（与 flag 包对布尔选项的处理一致）。

// VerboseOutput 是本次运行要不要打细节。
//
// 目前只有 doctor 用它（每项多打一段检查范围、读到的数、表，以及每条问题的
// 完整原文）；其余命令接受这个参数、输出不变，--help 里那份全局选项说明把
// 这件事写出来了。
var VerboseOutput bool

// QuietOutput 是本次运行要不要压掉「给人看」那一档的输出。
var QuietOutput bool

// HumanStdout 是「给人看」那一档写 stdout 的地方：--quiet 之下它丢弃一切。
//
// -q 说的是「没有人看着，成功就一声不吭，只在出错的时候说话」——脚本与守护
// 调用要的正是这个。所以压掉的是**整档**人读输出，而不是挑几行提示：挑着压
// 的表现是「以为安静了，其实还在刷屏」。
//
// 另外两条大家伙不走这里：
//
//   - **机器可读那一档**（--format json）照常打。脚本要的正是那份，压掉它等于
//     把 -q 变成「什么都拿不到」。emitJSON 的调用点一律写 os.Stdout。
//   - **致命错误**照常打。它由 main 直接写到 os.Stderr，与 -q 无关——「只在
//     存在致命错误时输出」这句话里的那个输出，指的就是它。
//
// 用法那一档（fs.Usage、--help）也照常打：那是使用者主动要的，不是刷屏。
func HumanStdout() io.Writer { return HumanOut(os.Stdout) }

// HumanStderr 是提示与警告写 stderr 的地方：--quiet 之下它丢弃一切。
//
// 与 humanStdout 分开成两个函数，是为了让调用点写出来的那句话就能读出它属于
// 哪一档：`printCreatedKey(humanStdout(), humanStderr(), ...)` 一眼看得出前者
// 是结果、后者是附注。
func HumanStderr() io.Writer { return HumanOut(os.Stderr) }

// HumanOut 是上面两个的共用部分。
func HumanOut(w io.Writer) io.Writer {
	if QuietOutput {
		return io.Discard
	}
	return w
}

// verboseFlagNames 与 quietFlagNames 是两个开关认的写法。
//
// 单横线的长写法一并认：本仓其余选项由 flag 包解析，而它同时接受 -name 与
// --name。只有全局选项不认的话，`larkaxctl -quiet doctor` 会以「未知子命令」
// 收场——那不是使用者能自己看明白的错（与 --env-file、--format 同一条理由）。
var (
	verboseFlagNames = []string{"--verbose", "-verbose", "-v"}
	quietFlagNames   = []string{"--quiet", "-quiet", "-q"}
)

// TakeVerboseFlag 从参数里摘出全局开关 --verbose，返回它这次是不是开着。
func TakeVerboseFlag(args []string) (bool, []string, error) {
	_, on, rest, err := TakeBoolFlag(args, verboseFlagNames, "--verbose")
	return on, rest, err
}

// TakeQuietFlag 从参数里摘出全局开关 --quiet，返回它这次是不是开着。
func TakeQuietFlag(args []string) (bool, []string, error) {
	_, on, rest, err := TakeBoolFlag(args, quietFlagNames, "--quiet")
	return on, rest, err
}

// TakeBoolFlag 从参数里摘出一个布尔开关，返回它有没有出现过、最终的取值，
// 以及剩下的参数。
//
// 「有没有出现过」与「取值是不是 true」是两件事：`migrate down --allow-missing=false`
// 里那个选项的取值是关的，但它仍然是用错了子命令——这一层要报的是后者。
//
// 它扫全表，所以写在子命令前后都认；出现多次时**后写的赢**，同样与 flag 包
// 一致（`-q --quiet=false` 的净效果是关着）。
//
// 认 `=取值` 那一支而不是把整段当名字：`--quiet=false` 落到「剩下的参数」里会
// 被子命令的 flag 集当成「flag provided but not defined」，而使用者写的是这个
// 选项的显式取值——报「不认识 --quiet=false」会把人引到选项名上去，问题却在
// 取值的拼法。取值不是布尔时报参数错，不静默当成 true。
func TakeBoolFlag(args []string, names []string, canonical string) (bool, bool, []string, error) {
	present, on := false, false
	rest := make([]string, 0, len(args))

	for _, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		if !slices.Contains(names, name) {
			rest = append(rest, arg)
			continue
		}

		present = true
		if !hasValue {
			on = true
			continue
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return false, false, nil, UsageErr(fmt.Errorf(
				"%s 的取值 %q 不是布尔值：%s 要 true 或 false", canonical, value, canonical))
		}
		on = parsed
	}

	return present, on, rest, nil
}
