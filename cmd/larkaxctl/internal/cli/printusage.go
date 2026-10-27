package cli

import (
	"fmt"
	"io"
)

// PrintUsage 打一份帮助文本，是**帮助文本的唯一出口**。
//
// 收成一处之前，帮助散在几十处（各命令 help 分支里的 fmt.Print、各 fs.Usage
// 闭包里的 fmt.Fprint），于是「打哪一版、打在哪条流上」得挨处看一遍才知道，
// 而两处写法不一致在页面上看不出来。现在只剩两条规则：
//
//   - **打哪一版**看这一次要的是哪一版：`larkaxctl help <命令>` 与
//     `<命令> help` 给长版，`-h` / `--help` 与参数写错时给短版。
//
//   - **打在哪条流**：`help <命令>` 与 `<命令> help`（要来的长版）走 stdout——
//     它是这一次运行的结果，要能进管道（`larkaxctl help channel | less`）。
//     `-h` / `--help` 与参数写错时分两种情况：**叶子**把用法打在 stderr 上
//     （`wallet credit -h`、`channel list -h` 历来如此，199 条路径里 162 条），
//     **有下级的组**多数打在 stdout 上（41 条里 32 条），因为那一份是「接下来
//     还有哪些子命令」的导览，会被人往下翻。跟着错误打的那一份一律走 stderr：
//     那时 stdout 上可能正要落一份 JSON（`--format json`），把用法页混进去下游
//     的 jq 就解析不了；而且它本来就是那条错误的附注。
//
//     **两种流并存是既有事实，不是本函数引入的**：这里只负责「打哪一版」，
//     打哪条流由调用点按上面的实际做法传 os.Stdout / os.Stderr。要统一得先
//     逐个命令定方向，那是另一件事。
//
// w 收 os.Stdout / os.Stderr 本身，不是 humanStdout()：`-q` 压的是「给人看」那一
// 档的输出，而帮助是使用者主动要的，不该被它压掉。
//
// 上色在这一处做，不在那几十个调用点各做一遍：只给**结构**上色（段标题与组标题
// 加粗、命令名与选项名加粗、说明与指针行置灰），规则见 style.go 的 paintHelp。
// `--format json` 是个例外——它压掉上色：那一档是给管道下游的纯文本，混进控制码
// 只会让下游多剥一层壳。`-q` 不在例外里：帮助照打、照上色。
func PrintUsage(w io.Writer, text string) {
	p := NewPalette(w)
	if JSONOutput() {
		p = Palette{}
	}
	_, _ = fmt.Fprint(w, p.PaintHelp(text))
}
