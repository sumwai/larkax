package cli

import "strings"

// 一次「收集各命令注册了哪些选项」的会话。
//
// 补全脚本要列出「这条命令能写哪些长选项」，而选项名只存在于各命令自己构造的
// flag 集里（见 FlagSet.record）。所以生成补全时把每条命令路径按
// `<路径> -h` 走一遍，让各命令走一遍注册，把这期间注册的名字记下来。
//
// # 为什么做成会话而不是一个包级 bool
//
// 收集期间**有五个地方要改变行为**：record 才记账，OpenDB 不连库，
// TakeLeadingOperand 与 takeOperand 不摘位置参数，HelpOnly 一律返回 false。
// 一个漂浮的包级 bool 让这五个地方各自去读「现在是不是收集期」，谁都能置位，
// 也没有一个地方说得清「什么时候算收集」。改成会话之后：置位、按路径重放、
// 复位、交出结果都在 CollectFlags 一处，五个读点只问「会话在不在进行中」。
//
// 会话仍然是一个进程级的状态：收集期间同一个进程里不能有并发的收集，
// 也不能有并发的正常调用。生成补全那一条命令是串行的，这条前提成立；
// 以后若要把命令也搬进这个包，「只许问会话、不许摸状态」这条跟着搬。
//
// # 会话状态怎么被读到
//
// 读点分布在 flagset.go（record）、db.go（OpenDB）、operand.go
// （TakeLeadingOperand）、helpflag.go（HelpOnly）与 main 包的 user_ops.go
// （takeOperand；它还没搬过来，问的是本包的 CollectingFlags）。记录的结果由
// FlagSet.record 写进来。它们都只在同一个 goroutine 里被调用——生成补全
// 是串行遍历，没有任何一处在收集期间起 goroutine。
var collectSession *collectState

// collectState 是一次收集会话的暂存。
type collectState struct {
	// path 是当前正在重放的那条命令路径，形如 "user wallet"。
	path string
	// flags 以命令路径为键，值是那条路径注册过的选项。
	flags map[string][]CollectedFlag
}

// CollectedFlag 是收集到的一个选项：名字，以及它注册时判出来的「吃不吃取值」。
type CollectedFlag struct {
	// Name 是选项名，不带前导的 `--`、也不带别名用的单字母简写之外的前缀。
	Name string
	// TakesValue 为 true 时选项后面跟一个取值（fish 生成物据此加 `-r`）。
	TakesValue bool
}

// CollectFlags 把每条命令路径重放一遍，收集它们注册的选项。
//
// 每次调用给 fn 一条路径（路径是一个词一片的切片），由 fn 去分派
// `<路径> -h`——「怎么按路径 dispatch」留在调用方，因为分派表属于命令注册表，
// 不在这个包里。`-h` 让 flag 在解析阶段返回 ErrHelp，命令走不到连库与写库；
// 万一有哪条路径在解析之前就要连库，OpenDB 在收集期间直接失败（见 db.go），
// 不会真的碰到数据库。
//
// 返回值按命令路径分组。**必须**在调用方的分派跑完之后才读它：会话结束之后
// 指针就被清掉了，之后任何一次注册都不再记账。
func CollectFlags(paths [][]string, fn func(path []string)) map[string][]CollectedFlag {
	state := &collectState{flags: make(map[string][]CollectedFlag, len(paths))}
	collectSession = state
	defer func() { collectSession = nil }()

	for _, path := range paths {
		state.path = strings.Join(path, " ")
		fn(path)
	}
	return state.flags
}

// CollectingFlags 报告现在是不是在一次收集会话里。
//
// 五个读点用它让路，除此之外不要在命令代码里用：它不是「要不要打输出」这类
// 开关，而是「这次调用不是真的在跑命令」的标记。以后迁进来的命令只许问会话，
// 不许自己置位。
func CollectingFlags() bool { return collectSession != nil }

// recordCollectedFlag 记下一个选项名。会话之外调用是空操作。
func recordCollectedFlag(name string, takesValue bool) {
	if collectSession == nil {
		return
	}
	collectSession.flags[collectSession.path] = append(
		collectSession.flags[collectSession.path],
		CollectedFlag{Name: name, TakesValue: takesValue})
}
