package cli

import "os"

// HelpOnly 判断这次调用是不是「只要帮助」：第一个参数是 -h 或 --help。
//
// **只认这两种写法**，尽管 `flag` 包也认 `-help` 与 `--h`。理由是基线版本对这两种
// 写法本身就不一致：`plan -help` 退出码 0、`wallet -help` 却是 2。这一层若把四种
// 一起收下，20 条叶子会变成「四种都通」，而其余命令仍是两种——不是统一，是又多出
// 一种组合。要收就整仓一起定，那属于另一件事。
//
// 为什么需要它：好几条命令先把**第一个位置参数**摘掉，再交给下一层解析
// （见 takeOperand 与 operand.go 的 takeLeadingOperand）。`user wallet credit -h`
// 里那个 -h 会被当成用户 ID 摘走，于是报的是「缺少 <用户>」——人要的是用法，
// 拿到的是一句参数错。这些包装层因此必须先看一眼：**第一个参数是不是求助旗标**。
//
// # 为什么是「第一个」而不是「恰好一个」
//
// 原先判的是 `len(args) == 1`。那个形状挡不住尾巴上多带一个词：
// `migrate create -h extra` 落进「两个词」，这一层让路，`-h` 一路走到 goose
// 的写入点——snakeCase 把它洗成 `h`，建出 `*_h.sql`。名字看起来完全合法，而
// 那个目录是 //go:embed sql/*.sql。
//
// 放宽到「第一个是求助旗标」之后，`-h` 后面跟什么都归这一层收掉，不再往下走。
// 代价是 `credit -h --dry-run` 这类混写也从「按原路解析」变成「打帮助」——
// 两者今天都打出帮助（混写那批走的是 flag 集的 ErrHelp），所以用户看到的变化
// 只在 rc 与流上，不在「有没有帮助」上。
//
// `user wallet credit 123 -h` 仍然不动：第一个参数是 `123`，不是求助旗标。
//
// **收集补全数据期间一律返回 false**：那时 CollectFlags 正是拿
// `dispatch(<路径> -h)` 去把各命令的旗标集注册出来的（见 collect.go），
// 在这里短路等于让那些命令一个旗标都记不下——补全脚本会安静地少掉一片旗标。
// 与 takeOperand / TakeLeadingOperand 在同一个会话状态上让路，是同一件事。
func HelpOnly(args []string) bool {
	if CollectingFlags() {
		return false
	}
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

// AnswerHelpFlag 处理「第一个参数是 -h 或 --help」：把短版打到 stderr 并返回
// true（调用方直接 return nil，收尾与 `flag` 返回 ErrHelp 时一致）。
//
// 尾巴上还有什么都不影响：判据在 HelpOnly 里，`-h` 打头即收。
//
// 打 stderr 而不打 stdout，是跟着**叶子**的既有做法：走 flag 集的那一批
// （`wallet credit -h`、`channel list -h`）本来就把用法打在 stderr 上，而这一层
// 的 20 条全是没有自己 flag 集的叶子。两处打不同的流，会让人在 `cmd -h | less`
// 上看到同类命令两种结果。有下级的组是另一档：它们大多打 stdout（41 条里 32 条），
// 因为那一份是「接下来还有哪些子命令」的导览，会被人往下翻。
//
// 走 printUsage 而不是自己 Fprint，是为了让「帮助只有一个出口」这条仍然成立。
func AnswerHelpFlag(args []string, short string) bool {
	if !HelpOnly(args) {
		return false
	}
	PrintUsage(os.Stderr, short)
	return true
}
