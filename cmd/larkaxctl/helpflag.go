package main

import "os"

// helpOnly 判断这次调用是不是「只要帮助」：参数恰好是一个 -h 或 --help。
//
// **只认这两种写法**，尽管 `flag` 包也认 `-help` 与 `--h`。理由是基线版本对这两种
// 写法本身就不一致：`plan -help` 退出码 0、`wallet -help` 却是 2。这一层若把四种
// 一起收下，20 条叶子会变成「四种都通」，而其余命令仍是两种——不是统一，是又多出
// 一种组合。要收就整仓一起定，那属于另一件事。
//
// 为什么需要它：好几条命令先把**第一个位置参数**摘掉，再交给下一层解析
// （见 takeOperand 与 operand.go 的 takeLeadingOperand）。`user wallet credit -h`
// 里那个 -h 会被当成用户 ID 摘走，于是报的是「缺少 <用户>」——人要的是用法，
// 拿到的是一句参数错。这些包装层因此必须先看一眼：**参数只有一个，而且它
// 就是求助旗标**。
//
// 只认「单独出现」这一种形态，是刻意的：
//
//   - `user wallet credit 123 -h` 不动：那条路本来就该把 -h 交给下面的 flag 集，
//     由它统一打帮助（现在就是这么做的），改了会动到带位置参数的既有行为。
//   - `credit -h --dry-run` 这种混写也不认：helpOnly 要的是「没别的参数」，
//     混写时仍按原路解析，行为与今天逐字相同。
//
// **收集补全数据期间一律返回 false**：那时 `collectCommandFlags` 正是拿
// `dispatch(<路径> -h)` 去把各命令的旗标集注册出来的（见 completion.go），
// 在这里短路等于让那些命令一个旗标都记不下——补全脚本会安静地少掉一片旗标。
// 与 takeOperand / takeLeadingOperand 在同一个开关上让路，是同一件事。
//
// 于是这条判据的影响面正好是「在基线版本上会报参数错的那 20 条叶子」。
func helpOnly(args []string) bool {
	if collectingCommandFlags {
		return false
	}
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help")
}

// answerHelpFlag 处理「参数只有一个 -h 或 --help」：把短版打到 stderr 并返回
// true（调用方直接 return nil，收尾与 `flag` 返回 ErrHelp 时一致）。
//
// 打 stderr 而不打 stdout，是跟着**叶子**的既有做法：走 flag 集的那一批
// （`wallet credit -h`、`channel list -h`）本来就把用法打在 stderr 上，而这一层
// 的 20 条全是没有自己 flag 集的叶子。两处打不同的流，会让人在 `cmd -h | less`
// 上看到同类命令两种结果。有下级的组是另一档：它们大多打 stdout（41 条里 32 条），
// 因为那一份是「接下来还有哪些子命令」的导览，会被人往下翻。
//
// 走 printUsage 而不是自己 Fprint，是为了让「帮助只有一个出口」这条仍然成立。
func answerHelpFlag(args []string, short string) bool {
	if !helpOnly(args) {
		return false
	}
	printUsage(os.Stderr, short)
	return true
}
