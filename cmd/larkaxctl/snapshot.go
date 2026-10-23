package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/snapshot"
	"github.com/sumwai/larkax/internal/snapshotwire"
	"github.com/sumwai/larkax/internal/textshape"
)

// snapshotUsage 是 snapshot 子命令的帮助。
//
// 几个动词服务同一件事的不同侧面：allowlist 回答「允许表里有什么」，
// redact 回答「这段报文交给它会长成什么样」，get 回答「这次请求当时存下了什么」，
// cleanup 负责把到期的删掉。中间两条是同一张表的两侧——redact 走脱敏的
// **输入**，get 读脱敏的**产出**。
// snapshotHelp 是 snapshot 的两版帮助文本（见 helpText）。
var snapshotHelp = helpText{short: snapshotShortUsage, long: snapshotUsage}

// snapshotShortUsage 是 snapshot 的短版：`-h` / `--help` 与参数写错时打的那一份。
const snapshotShortUsage = `larkaxctl snapshot 维护与查看请求结构快照。

用法：
  larkaxctl snapshot cleanup [--dry-run]   按 settings 里的留存期删除到期分区
  larkaxctl snapshot allowlist [--direction request|response] [--surface chat|decision]
        打印允许表：哪些字段放行、哪些替换成占位
  larkaxctl snapshot get <request_id> --actor <用户 ID 或用户名> --reason <文本>
        读出一条请求存下来的结构快照
  larkaxctl snapshot access-log [--request-id <id>] [--user <用户 ID 或用户名>]
        [--from <时刻>] [--to <时刻>] [--limit <条数>]
        列出访问记录：谁在什么时候读了哪一条快照
  larkaxctl snapshot redact --direction request|response
        从标准输入读一段 JSON 报文，按允许表脱敏

get 的 --actor 与 --reason 都是必填，读一次就写一行访问日志；授权失效之后原文不再展示。
redact 的骨架走 stdout，方向、字节上限与那句提醒走 stderr。

更细的边界：larkaxctl help snapshot。
`

// snapshotUsage 是 snapshot 的长版：`larkaxctl help snapshot` 与 `snapshot help`。
const snapshotUsage = `larkaxctl snapshot 维护与查看请求结构快照。

用法：
  larkaxctl snapshot cleanup               按 settings 里的留存期删除到期分区
  larkaxctl snapshot cleanup --dry-run     只列出会被删除的分区，不删
  larkaxctl snapshot allowlist [--direction request|response] [--surface chat|decision]
        打印允许表：哪些字段放行、哪些替换成占位，以及每一行的依据
  larkaxctl snapshot get <request_id> --actor <用户 ID 或用户名> --reason <文本>
        [--direction request|response]
        读出一条请求存下来的结构快照
  larkaxctl snapshot access-log [--request-id <id>] [--user <用户 ID 或用户名>]
        [--from <时刻>] [--to <时刻>] [--limit <条数>]
        列出访问记录：谁在什么时候读了哪一条快照
  larkaxctl snapshot redact --direction request|response [--surface chat|decision]
        [--max-bytes <字节数>]
        从标准输入读一段 JSON 报文，按允许表脱敏，骨架打到标准输出

留存期取自 settings 表的 request_snapshot.retention_days，
不在命令行上另设参数：同一件事有两个来源时，命令行的那个会悄悄赢，
而「留存期到底是几天」会变成一件要看是谁执行的才知道的事。

allowlist 不需要连库：它只读代码里那张允许表。所以它能在没有数据库、没有
配置的机器上跑——而「我们到底存了什么」这个问题，恰恰经常是在一个连不上
生产库的地方被问出来的。

get 的 --actor 与 --reason 都是必填。读一条别人的请求结构是读敏感数据的
动作，所以每一次读都要留痕：snapshot_access_log 里会写下一行，记着谁、
什么时候、以什么理由看了哪一条。表里 reason 允许为空，那是给系统自己写的行
留的余地；人手读一次，理由必须留下。两个参数都不给默认值——「我写了
--reason」与「我忘了写」在库里必须分得开。写日志与读快照在同一个事务里。

同一个 request_id 可能有多行（请求与响应各一行，重试也可能各一行），
get 全部按 created_at 升序打出来。只取一行会让「这次请求还有另一半」看不见。
request_id 一条都没读到的时候非零退出，并且不写访问日志：没读到东西不算一次访问。

原样采集的行要再问一次授权：正文能不能打出来，按这一行的 user_id 与 request_id
在**读的这一刻**重新查一次授权记录。授权到期或被撤销之后，那一行照常列出来，
正文不再打出——原样采集的承诺只在授权有效期间成立。查库失败时按「没有授权」处理，
方向与采集侧一致：扣下正文，不放行原文。

access-log 把 get 写下的那些行读回来：谁、什么时候、以什么理由读了哪一条快照。
它按 request_id、读取者（--user 收用户 ID 或用户名）与时间窗筛，三者可以一起给，
也可以一个都不给（那就是最近的那些）。--user 筛的是**读快照的那个人**，不是快照属于
谁：这张表记的是读取动作，快照属于谁是快照那一侧的事。理由那一栏是 get 的 --reason
原文；系统自己写的行没有理由，那一栏打一个短横。

access-log 也是管理员动作：跑得起 larkaxctl 的人本来就能连上这个库，所以这条命令
不额外要求别的授权，也不带角色参数——与 get 把读取者记成 admin 是同一条口径。
窗口是左闭右开的（--from 含、--to 不含，时刻按 UTC 算），--limit 默认 50、0 表示
不限；被截住时末行会说明只打了多少条。

redact 看到的结果不是库里那条快照。它把一段报文交给脱敏器，回答的是
「这段报文会长成什么样」；库里那条骨架来自**解析之后**的请求/响应，
字段集合与这里不完全一样——不转发的字段不会进库，而解析会补上它自己带的几个。
内容层面的结论一致：允许表之外的一律变成占位。

redact 把骨架打到标准输出（可以直接接给 jq），方向、骨架版本、字节上限、
被替换的路径与那句提醒打到标准错误。--max-bytes 默认 65536；线上是按
settings.request_snapshot.max_bytes 配的，要知道某段报文在**这个部署**里
会不会被降级成摘要层，就把那个值填进来。

报文从标准输入读，上限 64 MiB。这条命令是拿一段**单次请求**的报文来试，
不是拿整份日志：把一份很大的日志整个喂进来会超出上限并报错。要试整份日志
请先拆开，一次喂一段。

request 与 response 各有一张允许表，所以 redact 必须指明方向；
allowlist 与 get 不填时两个方向都处理。

**旧名仍可用**：snapshot show 等同于 snapshot get。它保留是为了不让已经在跑的
脚本与部署流程坏掉，行为与新名字逐字相同；新写的命令请用新名字。

更细的约定见 docs/cli-reference.md 的「snapshot」一节。
`

// snapshotActions 是 snapshot 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var snapshotActions = []string{"cleanup", "allowlist", "get", "access-log", "redact"}

// runSnapshot 处理 snapshot 子命令。
func runSnapshot(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, snapshotShortUsage)
		return usageErr(errors.New("snapshot 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "cleanup":
		return runSnapshotCleanup(rest)
	case "allowlist":
		return runSnapshotAllowlist(rest)
	case "get", "show":
		return runSnapshotGet(rest)
	case "access-log":
		return runSnapshotAccessLog(rest)
	case "redact":
		return runSnapshotRedact(rest)
	case "-h", "--help":
		printUsage(os.Stdout, snapshotShortUsage)
		return nil
	default:
		printUsage(os.Stderr, snapshotShortUsage)
		return usageErr(fmt.Errorf("未知的 snapshot 子命令 %q", action))
	}
}

// snapshotRootsFor 把 --direction 的取值翻成一组方向。
//
// 取值从 snapshot.Roots() 里找，而不是在这里再列一遍 request / response：
// 允许表里加了新方向时，这里跟着就能用；在这里手抄一份的表现是
// 「表里有的方向，命令行说它不认识」。
//
// 空串表示「全部」；调用方自己决定它是不是合法输入（redact 必须指明方向）。
func snapshotRootsFor(direction string) ([]snapshot.Root, error) {
	all := snapshot.Roots()
	if direction == "" {
		return all, nil
	}
	for _, root := range all {
		if string(root) == direction {
			return []snapshot.Root{root}, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, root := range all {
		names = append(names, string(root))
	}
	return nil, fmt.Errorf("方向 %q 不认识（可选：%s）", direction, strings.Join(names, "、"))
}

// surfaceFor 把 --surface 的取值翻成一个已知表面。
//
// 取值从 snapshot.Surfaces() 里找，而不是在这里再列一遍 chat / decision：
// 允许表加了新表面时这里跟着就能用；手抄一份的表现是「表里有的表面，
// 命令行说它不认识」。
//
// 空串表示「默认的那个」：allowlist 打印全部，redact 用 chat——
// 「不写就是对话那条路」是使用者的默认预期，而决策那条路是一次明确的指定。
func surfaceFor(name string, fallback snapshot.Surface) (snapshot.Surface, error) {
	all := snapshot.Surfaces()
	if name == "" {
		return fallback, nil
	}
	for _, s := range all {
		if string(s) == name {
			return s, nil
		}
	}
	names := make([]string, 0, len(all))
	for _, s := range all {
		names = append(names, string(s))
	}
	return "", fmt.Errorf("表面 %q 不认识（可选：%s）", name, strings.Join(names, "、"))
}

// runSnapshotAllowlist 打印允许表。
//
// 它不连库、不读配置：允许表是代码里的数据，而这条命令的用途正是
// 在一个连不上库的地方回答「你们存了我请求里的哪些字段」。
func runSnapshotAllowlist(args []string) error {
	fs := newFlagSet("snapshot allowlist", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, snapshotShortUsage) }
	direction := fs.String("direction", "", "")
	surfaceName := fs.String("surface", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("snapshot allowlist 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	roots, err := snapshotRootsFor(*direction)
	if err != nil {
		return usageErr(err)
	}
	// 不填 --surface 时逐张表都打：只打一张的话，「有没有决策那张表」这件事
	// 就变成要看帮助文本才知道的事，而这条命令的用途正是回答那个问题。
	surface, err := surfaceFor(*surfaceName, "")
	if err != nil {
		return usageErr(err)
	}
	surfaces := snapshot.Surfaces()
	if surface != "" {
		surfaces = []snapshot.Surface{surface}
	}
	if jsonOutput() {
		return emitJSON(os.Stdout, allowlistDoc(surfaces, roots), nil)
	}
	printAllowlists(humanStdout(), surfaces, roots)
	return nil
}

// allowlistDoc 把一组「表面 × 方向」的允许表折成机器可读输出。
//
// 行式而不是「表面 → 方向 → 规则」的嵌套：打印的顺序本来就是逐对打一遍，
// 拍平之后读的人不必先知道有哪几个表面，也保住了那一层顺序。
//
// Note 与表格那一档一样要完整带出来，这一条命令存在的理由就是它：省略或
// 截断它，这条命令就退化成一个字段名清单。
func allowlistDoc(surfaces []snapshot.Surface, roots []snapshot.Root) map[string]any {
	groups := make([]map[string]any, 0, len(surfaces)*len(roots))
	rules := 0
	for _, s := range surfaces {
		for _, root := range roots {
			entries := snapshot.AllowlistFor(s, root)
			rules += len(entries)

			items := make([]map[string]any, 0, len(entries))
			for _, rule := range entries {
				items = append(items, map[string]any{
					"path":           rule.Path,
					"kind":           rule.Kind,
					"upstream_value": rule.UpstreamValue,
					"note":           rule.Note,
				})
			}
			groups = append(groups, map[string]any{
				"surface":   string(s),
				"direction": string(root),
				"rules":     items,
			})
		}
	}

	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"groups": len(groups), "rules": rules}
	doc["allowlist"] = groups
	return doc
}

// printAllowlists 打印一组表面 × 方向的允许表。
//
// Note 必须完整打出来，这一条命令存在的理由就是它。允许表本身在源码里，
// 能读源码的人不需要这条命令；而「为什么这一行放行、为什么那一行脱敏」
// 正是使用者问「你们到底存了我什么」时唯一能回答他的东西。
// 省略或截断它，这条命令就退化成一个字段名清单。
//
// 走 snapshot.AllowlistFor 而不是去读包内的表：它返回的是**副本**
// （见那个函数的注释），所以打印它不会影响脱敏行为——这一点让这条命令
// 可以放心地在任何地方跑，包括生产库旁边。
//
// 规则走固定列宽的表格（与其余清单同一条约定）：一条规则的三个字段原先拼在
// 一行 `Path=… Kind=… UpstreamValue=…` 里，路径长的时候这一行会折行，而折出来
// 的行与下一条规则分不开。
//
// **Note 单独列在下面，不进表的第四栏**：那是一整句「这一行为什么放行、为什么
// 脱敏」，进栏会把宽度撑到一屏之外。它按规则原来的顺序排在表下面，键是路径，
// 于是「哪一段说明属于哪一条」仍然对得上——这份说明正是这条命令存在的理由，
// 不能因为排版把它与规则拆散。
func printAllowlists(w io.Writer, surfaces []snapshot.Surface, roots []snapshot.Root) {
	p := newPalette(w)
	for i, s := range surfaces {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		for j, root := range roots {
			if j > 0 {
				_, _ = fmt.Fprintln(w)
			}
			rules := snapshot.AllowlistFor(s, root)
			// 条数一起打：0 条与「这一步没跑」在安静的终端里长得一样
			// （见 LESSONS.md 第 16 条）。
			_, _ = fmt.Fprintf(w, "表面 %s 方向 %s：%d 条\n", s, root, len(rules))
			if len(rules) == 0 {
				continue
			}

			tw := newTableWriter(w)
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
				p.bold("路径"), p.bold("类型"), p.bold("上游值"))
			for _, rule := range rules {
				// 路径是这张表的主键，加粗。
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%t\n",
					p.bold(rule.Path), rule.Kind, rule.UpstreamValue)
			}
			_ = tw.Flush()

			_, _ = fmt.Fprintln(w, "  说明：")
			for _, rule := range rules {
				_, _ = fmt.Fprintf(w, "    %s：%s\n", p.bold(rule.Path), rule.Note)
			}
		}
	}
}

// runSnapshotRedact 把一段报文交给脱敏器，打印它会变成什么。
//
// 这是 Decode 唯一的调用方，也是「这段报文我们存下来会是什么样」这个问题的
// 答案。它不连库：脱敏器是纯函数，报文从标准输入来。
func runSnapshotRedact(args []string) error {
	// redact 有自己的 stdout 契约，而且它**已经是**机器可读的：产物是脱敏后
	// 那段骨架的 JSON，直接接给 jq（见 printRedaction）。它不是一份带
	// status / exit_code 的信封，所以照 `-o json` 那一档的约定改不了它，
	// 也不该改——那段骨架就是这个命令的结果本身。这里拒掉，免得让人以为
	// `-o json` 能给它套上一层信封。
	if err := jsonUnsupported("snapshot redact"); err != nil {
		return err
	}

	fs := newFlagSet("snapshot redact", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, snapshotShortUsage) }
	direction := fs.String("direction", "", "")
	surfaceName := fs.String("surface", "", "")
	maxBytes := fs.Int("max-bytes", snapshot.DefaultMaxBytes,
		"")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"snapshot redact 不接受位置参数，得到 %q：报文从标准输入读", fs.Arg(0)))
	}
	if strings.TrimSpace(*direction) == "" {
		return usageErr(errors.New("缺少 --direction：request 与 response 各有一张允许表，必须指明"))
	}
	if *maxBytes <= 0 {
		// 在构造脱敏器之前拦下：WithMaxBytes 也会拒，但那个错误的措辞是
		// 「选项无效」，而这里的问题出在命令行参数上，退出码应当是 2。
		return usageErr(fmt.Errorf("--max-bytes 必须为正数，得到 %d", *maxBytes))
	}

	roots, err := snapshotRootsFor(*direction)
	if err != nil {
		return usageErr(err)
	}
	surface, err := surfaceFor(*surfaceName, snapshot.SurfaceChat)
	if err != nil {
		return usageErr(err)
	}
	redactor, err := snapshot.NewFor(surface, roots[0], snapshot.WithMaxBytes(*maxBytes))
	if err != nil {
		// 走到这里说明允许表本身有问题（重复路径、空 Note 之类），
		// 那不是参数错，是这份代码或这个二进制有问题。
		return internalErr(fmt.Errorf("构造脱敏器失败: %w", err))
	}

	data, err := readBoundedInput(os.Stdin, snapshotRedactMaxInput)
	if err != nil {
		return err
	}
	// Decode 而不是 json.Unmarshal：它用 json.Number 接住数字（大整数被静默
	// 转成浮点后与原值不一致），并拒绝「首个对象之后还有内容」的报文。
	// 那两件事只有「外部拿一段报文进来」时才起作用，而这里正是那个场景。
	payload, err := snapshot.Decode(data)
	if err != nil {
		// 按这条命令的用法，喂进来的是一段**已经采下来的**报文；它解不开说明
		// 这一段本身坏了，不是使用者写错了参数——归「内部错误」。
		return internalErr(fmt.Errorf("解析报文失败: %w", err))
	}
	res, err := redactor.RedactRaw(payload)
	if err != nil {
		return internalErr(fmt.Errorf("脱敏失败: %w", err))
	}
	return printRedaction(humanStdout(), humanStderr(), redactor, res, verboseOutput)
}

// readBoundedInput 把 r 整个读进内存，超过 limit 字节就报错。
//
// 多读一个字节再判断，而不是把 LimitReader 直接读到 limit：那样读到的长度最多
// 等于 limit，「恰好 limit 字节」与「远不止 limit 字节」就分不出来了，而后者
// 正是要拦的那一种。多读的那一个字节还让报错说得清「确实超了」，不用去猜。
//
// 上限由调用方给（而不是在这里读常量），边界因此能用一个小数值精确地测：
// 喂 limit 字节要通过、limit+1 字节要拒，两件事都不必真造出 64 MiB。
func readBoundedInput(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取标准输入失败: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf(
			"标准输入超过 %d MiB（%d 字节）的上限：snapshot redact 是拿一段**单次请求**"+
				"的报文来试，不是拿整份日志。要试整份日志请先拆开，一次喂一段。",
			limit>>20, limit)
	}
	return data, nil
}

// printRedaction 打印一次脱敏的结果。
//
// 骨架本身走 stdout，其余走 stderr：产物要能直接接给 jq，而方向、字节上限
// 这些是「读这个产物时的条件」，不是产物的一部分。截断与否取决于字节上限，
// 而线上那个上限来自 settings——把它一起打出来，读的人才能判断那一行
// truncated 说的到底是不是他关心的那件事。
//
// 「这是脱敏器对这段报文的结果、不是库里那条快照」只在 verbose 下打：它在
// **每一次**运行里都会出现，而每次都出现的那句解释读第二遍就没有信息了
// （见 docs/cli-output.md 的「提示与警告」）。要问「库里那条长什么样」的人
// 加 -v。
func printRedaction(out, info io.Writer, redactor *snapshot.Redactor, res *snapshot.Result,
	verbose bool,
) error {
	_, _ = fmt.Fprintf(info, "表面=%s 方向=%s 骨架版本=%d 字节上限=%d 被截断=%t\n",
		redactor.Surface(), redactor.Root(), res.Version, redactor.MaxBytes(), res.Truncated)
	if res.Truncated {
		_, _ = fmt.Fprintln(info,
			"注意：骨架超限，已降级成摘要层（只留顶层标量），逐条消息与工具定义的细节都丢了。")
	}

	// MarshalIndent 对 map 的键是排序的，所以同一段报文两次跑出来逐行相同。
	raw, err := json.MarshalIndent(res.Skeleton, "", "  ")
	if err != nil {
		return internalErr(fmt.Errorf("骨架无法序列化: %w", err))
	}
	_, _ = fmt.Fprintf(out, "%s\n", raw)

	printRedactedPaths(info, "", res.Redacted)
	if verbose {
		_, _ = fmt.Fprintln(info, "注意：这是脱敏器对这段报文的结果，不是库里那条快照——")
		_, _ = fmt.Fprintln(info, "      库里那条来自解析之后的请求/响应，字段集合与这里不完全一样。")
	}
	return nil
}

// snapshotReadTimeout 是读一条快照的时间上限。
//
// 与 ledger balance 同一个量级（一分钟）：它按 request_id 走索引读几行，
// 读不完通常意味着索引没被用上、或者表已经大到该换做法了。
// 给得再宽只会让一次卡住的查询挂到运维发现为止。
const snapshotReadTimeout = time.Minute

// snapshotActorKindAdmin 是这条命令写进 snapshot_access_log.actor_kind 的取值。
//
// 固定 admin：larkaxctl 是管理入口，能跑它的人本来就是管理员。
// 表上的 CHECK 还认 owner 与 system，那两个留给控制台与系统自己写的行。
const snapshotActorKindAdmin = "admin"

// snapshotRedactMaxInput 是 snapshot redact 从标准输入读入的字节上限。
//
// 不设界的读入等于让调用方决定这条命令要为它花多少内存。这不是「会不会被恶意
// 输入打挂」的问题，是**误用**的问题：`cat 2GB.log | larkaxctl snapshot redact`
// 是一条会顺手打出来的命令，而它的后果是把跑这条命令的机器吃光。运维手工跑
// 不等于不会误用。同 internal/plugin/host/log.go 那句「不设界的转换等于让插件
// 决定宿主要为此花多少内存」。
//
// 上限给得比任何**单次**报文都宽：它拦的是「整份日志喂进来」这类误用，
// 不是某一条特别大的请求。不配开关——现在没有需要调它的场景，而多一个会漂移的
// 配置就多一处「线上到底是哪个值」。
const snapshotRedactMaxInput = 64 << 20

// selectSnapshotsSQL 按 request_id 读快照。
//
// 谓词与排序都对着 request_snapshots_request_idx (request_id, created_at)：
// 表按天分区，走索引是「读一条快照」不变成一次全表扫的前提。
// 排序补一个 id 是为了稳定——同一个 request_id 的两行可能落在同一微秒上，
// 而两次跑出来的行序不同会让比对变成一件只能靠肉眼做的事。
//
// redacted_paths 是 text[]，database/sql 不认这个类型，所以查询侧用
// array_to_json 转成文本再解（与 internal/channels、internal/catalog 同一个做法）。
//
// user_id 也要读：原样采集的行在打正文之前要按「这一行属于谁」重新问一次
// 授权（见 withholdInactiveRaw），而这一列是判定要用的那两个身份之一。
const selectSnapshotsSQL = `
	SELECT id, user_id, direction, skeleton_version, capture_mode, skeleton,
	       skeleton_truncated, upstream_status, error_kind,
	       array_to_json(redacted_paths), created_at
	  FROM request_snapshots
	 WHERE request_id = $1
	   AND direction = ANY($2)
	 ORDER BY created_at, id`

// insertSnapshotAccessSQL 是 snapshot_access_log 的写入方。
//
// 这张表在初始 schema 里就建好了，为的正是这一刻：读快照这个动作要留痕。
// actor_kind 与 actor_user_id 都显式给值，不用 DEFAULT——一张审计表上的
// 「谁」如果可以是空的，那张表就回答不了它唯一要回答的问题。
const insertSnapshotAccessSQL = `
	INSERT INTO snapshot_access_log (snapshot_id, request_id, actor_user_id, actor_kind, reason)
	VALUES ($1, $2, $3, $4, $5)`

// snapshotRead 是一次 get 的输入。
type snapshotRead struct {
	// RequestID 是要读的那次请求。
	RequestID string
	// Directions 是这次要读的方向，取值来自 snapshot.Roots()。
	Directions []string
	// Filtered 表示 Directions 是用户点名要的，而不是「全部」。
	//
	// 单独记一个标记，是为了让「一条都没读到」的报错能说清是哪种情况：
	// 库里确实有这次请求、但只有另一个方向那一行时，指向的是一个写错的
	// --direction，而不是一次写错的采集。
	Filtered bool
	// Actor 是读这条快照的人，用户 ID 或用户名。
	Actor string
	// Reason 是读它的理由，原样落进 snapshot_access_log.reason。
	Reason string
}

// snapshotRow 是从 request_snapshots 读回来的一行。
//
// 只带命令要展示的列，不写成 SELECT *：这张表的列会随 schema 演进，
// 而 `*` 在演进时会静默改变列序与含义（与 snapshotwire 的 insertSQL 同一条理由）。
type snapshotRow struct {
	ID              int64
	UserID          int64
	Direction       string
	SkeletonVersion int
	CaptureMode     string
	Skeleton        map[string]any
	Truncated       bool
	UpstreamStatus  sql.NullInt64
	ErrorKind       sql.NullString
	RedactedPaths   []string
	CreatedAt       time.Time

	// RawWithheld 表示这一行采的是原文，而授权在**读的这一刻**已经不作数，
	// 所以正文没有读出来（见 withholdInactiveRaw）。Skeleton 此时为空。
	//
	// 行本身照常保留：读的人要看得见「这里本来有一条原文、现在不能读了」，
	// 而不是以为输出被截断了；访问日志也照旧，因为这一行确实被读过。
	RawWithheld bool
}

// runSnapshotGet 读出一条请求存下来的结构快照。
//
// # 为什么分两趟解析参数
//
// Go 的 flag 包在遇到**第一个非旗标参数时就停止解析**，而这条命令的形状是
// `get <request_id> --actor ... --reason ...`——request_id 在旗标前面。
// 直接 Parse 会把 --actor 及其取值当成第二、第三个位置参数，报出来的错是
// 「收到 5 个位置参数」，那句话把人引向参数个数，而问题在参数顺序
// （settings set 的注释里记着同一个陷阱）。
//
// 所以先切出 request_id，再把剩下的交给同一个 FlagSet 解一遍。
// 于是 `get <id> --actor X` 与 `get --actor X <id>` 两种写法都能用，
// 而多余的位置参数照样会被拒——它们落在第二趟的 fs.Args() 里。
func runSnapshotGet(args []string) error {
	fs := newFlagSet("snapshot get", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, snapshotShortUsage) }
	actor := fs.String("actor", "", "")
	reason := fs.String("reason", "", "")
	direction := fs.String("direction", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	// 第一趟 Parse 之后，fs.Args() 是「第一个位置参数 + 它后面的一切」。
	rest := fs.Args()
	if len(rest) == 0 {
		return usageErr(errors.New(
			"用法：larkaxctl snapshot get <request_id> --actor <用户> --reason <文本>"))
	}
	requestID := strings.TrimSpace(rest[0])
	if requestID == "" {
		return usageErr(errors.New("request_id 不能为空"))
	}
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return usageErr(err)
		}
		if fs.NArg() > 0 {
			return usageErr(fmt.Errorf(
				"snapshot get 只接受一个位置参数（request_id），多余的：%q", fs.Arg(0)))
		}
	}
	// 两个都不给默认值。理由见 snapshotUsage：「我写了 --reason」与
	// 「我忘了写」在库里必须分得开，而一个默认值恰好让这两件事在库里长得一样。
	if strings.TrimSpace(*actor) == "" {
		return usageErr(errors.New("缺少 --actor：读一条快照要记下是谁读的"))
	}
	if strings.TrimSpace(*reason) == "" {
		return usageErr(errors.New("缺少 --reason：读一条别人的请求结构，理由必须留下"))
	}
	// 理由原样落进 snapshot_access_log，随后由 `snapshot access-log` 打成表格的
	// 最后一栏。这条写入路径就在本文件里（readSnapshotsWithAudit），没有别的
	// 包可以承载判据，所以在连库之前判一次，写入那一步与这里用同一个函数。
	if err := checkDisplayText("--reason", *reason); err != nil {
		return err
	}

	roots, err := snapshotRootsFor(*direction)
	if err != nil {
		return usageErr(err)
	}
	dirs := make([]string, 0, len(roots))
	for _, root := range roots {
		dirs = append(dirs, string(root))
	}
	req := snapshotRead{
		RequestID:  requestID,
		Directions: dirs,
		Filtered:   *direction != "",
		Actor:      *actor,
		Reason:     *reason,
	}

	return withDB(snapshotReadTimeout, func(ctx context.Context, db *sql.DB) error {
		rows, err := readSnapshotsWithAudit(ctx, db, req)
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, snapshotsDoc(req.RequestID, rows), nil)
		}
		printSnapshots(humanStdout(), humanStderr(), req.RequestID, rows)
		return nil
	})
}

// snapshotsDoc 把读回来的快照折成机器可读输出。
//
// # 两栏 NULL 在 JSON 里就是 null
//
// 表格那一档给 upstream_status / error_kind 的三段文本（<不适用（请求方向
// 快照）>、<无上游响应>、<无>）在 JSON 里一律是 null：占位符进 JSON 比留空
// 更坏——按时间或数字解析它的一方会拿到一个解析不了的值。两个方向上的差别
// 由 direction 推得出来（请求方向的采集点在选路之前，那两列**本来就不适用**），
// 不需要再给一栏布尔去复述它。
//
// 有值的一栏照原样给：连请求方向上出现状态码也一样（那说明有写入路径绕过了
// internal/gateway 的采集点，把一个异常藏起来比显示出来更坏，见
// upstreamStatusCell）。
func snapshotsDoc(requestID string, rows []snapshotRow) map[string]any {
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"id":        row.ID,
			"direction": row.Direction,
			// created_at 走 format.go 的 jsonTime，不在这里手工 Format：机器读
			// 的时刻只有那一条出口（恒 UTC、完整精度），多一处手工 Format 就多
			// 一处要记得带 .UTC() 的地方（见 docs/cli-output.md 的「时刻与时区」）。
			"created_at":       jsonTime(row.CreatedAt),
			"skeleton_version": row.SkeletonVersion,
			"capture_mode":     row.CaptureMode,
			// 被降级成摘要层的骨架与一份完整的骨架在肉眼下长得一样，所以这个
			// 布尔必须带出来：没有它，读的人会把「这里只有这些」读成
			// 「上游只发了这些」（见 printSnapshot）。
			"skeleton_truncated": row.Truncated,
			// 正文被扣下时 skeleton 是 null。没有这个布尔，null 与「这次什么
			// 都没发」分不开，而两者要读的人做的事完全不同。
			"raw_withheld":   row.RawWithheld,
			"redacted_paths": jsonList(row.RedactedPaths),
			"skeleton":       row.Skeleton,
		}
		if row.UpstreamStatus.Valid {
			item["upstream_status"] = row.UpstreamStatus.Int64
		} else {
			item["upstream_status"] = nil
		}
		if row.ErrorKind.Valid {
			item["error_kind"] = row.ErrorKind.String
		} else {
			item["error_kind"] = nil
		}
		items = append(items, item)
	}

	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(items)}
	doc["request_id"] = requestID
	doc["snapshots"] = items
	return doc
}

// readSnapshotsWithAudit 在一个事务里读出快照，并为读到的每一行写下访问日志。
//
// # 为什么两件事必须在同一个事务里
//
// 这条命令是读快照的入口，而 snapshot_access_log 存在的理由就是让每一次读
// 留下痕迹。拆成两步的话，中间那一步失败会留下一个**完全正常**的成功返回：
// 数据打出来了，日志里没有那一行。那种失败不报错，只让审计承诺静默失效，
// 而这正是这条链路上最坏的失败方式——它看起来完全正常。
// 所以这里先记再返回，一起提交、一起回滚。
//
// # 没读到就不记
//
// 0 行时直接返回错误，不写日志。这不是为了省一行，而是让日志里的每一行
// 都对应一次真的读过：把「没找到」也记进去，日志就会混进一批
// 「有人查过这个不存在的 ID」的行，而那不是审计要回答的问题。
//
// # 为什么一行日志一条快照
//
// snapshot_access_log.snapshot_id 是 NOT NULL 的，一条日志对应一行快照。
// 一次 get 打出几行就写几行，于是「看了请求那一半还是响应那一半」
// 在日志里分得开。
func readSnapshotsWithAudit(ctx context.Context, db *sql.DB, req snapshotRead) ([]snapshotRow, error) {
	// 理由会原样落进 snapshot_access_log，随后由 `snapshot access-log` 打进
	// 表格最后一栏。这里再判一次，而不是只靠命令行的 pre-check：写入路径自己
	// 守住，别的调用方从这一头同样进不来（判据与命令行那一处是同一个函数）。
	if err := textshape.Validate("--reason", req.Reason); err != nil {
		return nil, err
	}
	// 先解析 actor：actor_user_id 上有外键，写一个不存在的用户会失败，
	// 而那时快照已经读出来了。把它放在读之前，「谁在读」这件事不成立时
	// 根本不会有读发生（与 settings set 的 --by 同一个做法）。
	actor, err := auth.New(db).ResolveUser(ctx, req.Actor)
	if err != nil {
		return nil, fmt.Errorf("解析 --actor 失败: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	// 任何没走到 Commit 的返回路径都回滚。漏掉一处的表现是连接被占着，
	// 直到它超时——那时现场离这里已经很远。提交之后再回滚是空操作
	//（返回 ErrTxDone），所以这里不必分情况。
	defer func() { _ = tx.Rollback() }()

	rows, err := querySnapshots(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, snapshotNotFound(req)
	}

	// 正文该不该交出去，在写访问日志之前定下来：日志记的是「这一行被读过」，
	// 而扣下正文的读同样是一次读。放在这里也让人类可读与 JSON 两档共用同一个
	// 结论——两路各判一次，迟早会出现「终端里看不到、jq 里拿得到」。
	//
	// request_id 取命令行那一个，不取行里读出来的：查询是 `WHERE request_id = $1`
	// 精确匹配，所以两者恒等。哪天这条查询改成前缀或模糊匹配，这里就要换成
	// rows[i].RequestID——拿命令行那个值去问授权，会把这次请求的判定套到别的请求上。
	withholdInactiveRaw(ctx, db, req.RequestID, rows)

	for _, row := range rows {
		if _, execErr := tx.ExecContext(ctx, insertSnapshotAccessSQL,
			row.ID, req.RequestID, actor.ID, snapshotActorKindAdmin, req.Reason,
		); execErr != nil {
			// 写不进去就整条命令失败，一行数据都不返回：审计记不下来的那次读，
			// 不该把内容交出去。
			return nil, fmt.Errorf("写快照访问日志失败: %w", execErr)
		}
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return nil, fmt.Errorf("提交事务失败: %w", commitErr)
	}
	return rows, nil
}

// withholdInactiveRaw 把授权已经不作数的原文行扣下来。
//
// # 为什么读的时候还要再问一次授权
//
// 采集那一刻的判定回答的是「这一次要不要留原文」，它管不到「存下来之后还能不能
// 读」。授权到期或被撤销之后，那一行仍然是 full_consented，而原样采集的承诺只在
// 授权有效期间成立——所以读的这一刻要按这一行的 user_id 与 request_id 再问一次。
// 判据与采集侧是同一个来源（见 snapshotwire.DBConsentSource）：每次都查库、
// 没有缓存，撤销立即生效靠的就是这一点；任何缓存都会留下一段「已经撤销、
// 却还能读到原文」的窗口。
//
// # 扣下的是什么
//
// 只把正文置空，行本身留着：读的人要看得见「这里本来有一条原文、现在不能读了」，
// 而不是以为输出被截断了。查库失败时那个来源按「没有授权」回答，所以失败的方向
// 是扣下正文，不是放行原文。到了渲染那一层，「没有授权」与「判断不了」已经分不开，
// 所以那一句提示同时盖住两种情形（见 printSnapshot）。
func withholdInactiveRaw(ctx context.Context, db *sql.DB, requestID string, rows []snapshotRow) {
	src := snapshotwire.NewDBConsentSource(snapshotwire.Wrap(db), nil)
	for i := range rows {
		// 骨架行不用问：它们本来就不含原文，查一次库只会把判定点散成两处。
		if !snapshotwire.CaptureModeIsRaw(rows[i].CaptureMode) {
			continue
		}
		if src.AllowsRawCapture(ctx, rows[i].UserID, requestID) {
			continue
		}
		rows[i].Skeleton = nil
		rows[i].RawWithheld = true
	}
}

// errSnapshotNotFound 是读路径上「要读的那一条快照不在库里」的哨兵。
//
// 它登记在 conflictErrors（exit.go）里，落 exitConflict：与「要改的那一行不
// 在」是同一个形状——下一步都是先去看数据里现在是什么，而不是改一条命令重发。
var errSnapshotNotFound = errors.New("要读的快照不存在")

// snapshotNotFoundError 是「一条都没读到」时返回的具体错误。
//
// 单独一个类型而不是直接 fmt.Errorf：那句话要带上 request id 与方向（给读的人
// 看），而 conflictErrors 是按哨兵分类的（给脚本看退出码）。一个纯哨兵带不了
// 前者，一个纯错误串进不了后者，所以这里让类型自己认作那个哨兵。
type snapshotNotFoundError struct {
	requestID  string
	directions []string
	filtered   bool
}

func (e *snapshotNotFoundError) Error() string {
	if e.filtered {
		return fmt.Sprintf("请求 %q 没有 %s 方向的快照（去掉 --direction 或换一个再试）",
			e.requestID, strings.Join(e.directions, "、"))
	}
	return fmt.Sprintf("请求 %q 没有存下来的快照", e.requestID)
}

// Is 让它等于哨兵 errSnapshotNotFound：conflictErrors 按哨兵分类，退出码 4。
func (e *snapshotNotFoundError) Is(target error) bool { return target == errSnapshotNotFound }

// snapshotNotFound 说明一条都没读到。
func snapshotNotFound(req snapshotRead) error {
	return &snapshotNotFoundError{
		requestID:  req.RequestID,
		directions: req.Directions,
		filtered:   req.Filtered,
	}
}

// querySnapshots 读出一行行快照。
//
// 收 *sql.Tx 而不是 *sql.DB：它只该在 readSnapshotsWithAudit 的事务里被调用，
// 类型上写成事务能让「换个人在别处顺手调一下」当场编译不过——而那正是
// 「读了但没记」会溜进来的地方。
func querySnapshots(ctx context.Context, tx *sql.Tx, req snapshotRead) ([]snapshotRow, error) {
	rows, err := tx.QueryContext(ctx, selectSnapshotsSQL, req.RequestID, req.Directions)
	if err != nil {
		return nil, fmt.Errorf("查询快照失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []snapshotRow
	for rows.Next() {
		var (
			row      snapshotRow
			skeleton []byte
			redacted string
		)
		if scanErr := rows.Scan(
			&row.ID, &row.UserID, &row.Direction, &row.SkeletonVersion, &row.CaptureMode,
			&skeleton, &row.Truncated, &row.UpstreamStatus, &row.ErrorKind,
			&redacted, &row.CreatedAt,
		); scanErr != nil {
			return nil, fmt.Errorf("读取快照行失败: %w", scanErr)
		}

		decoded, decodeErr := decodeSkeleton(skeleton)
		if decodeErr != nil {
			return nil, fmt.Errorf("快照 %d 的骨架不是合法的 JSON 对象: %w", row.ID, decodeErr)
		}
		row.Skeleton = decoded

		if pathsErr := json.Unmarshal([]byte(redacted), &row.RedactedPaths); pathsErr != nil {
			return nil, fmt.Errorf("快照 %d 的 redacted_paths 不是合法的 JSON 数组: %w", row.ID, pathsErr)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历快照行: %w", err)
	}
	return out, nil
}

// decodeSkeleton 把 jsonb 列里的骨架解成 map。
//
// 用 json.Number 接住数字，与 snapshot.Decode 同一个理由：大整数被静默转成
// 浮点后与原值不一致，而**错的那一份会被当成「当时存的就是这个」**。
// 顺带让渲染稳定——json.Number 原样保留字面量（1.0 不会变成 1）。
func decodeSkeleton(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		// 列上是 NOT NULL，但存成 JSON null 照样能落进去。那种行读出来
		// 是一张空骨架，与「这次请求什么都没发」分不开，所以不当成正常值。
		return nil, errors.New("骨架是 JSON null")
	}
	return out, nil
}

// printSnapshots 把读回来的快照打到 out，附注（哪几条被降级了）打到 note。
//
// 两个流分开的理由与 printSnapshot 同一条：那几句是「这一行该怎么读」的
// 附注，不是快照本身，而 stdout 上那份是要能接给 jq 的。
func printSnapshots(out, note io.Writer, requestID string, rows []snapshotRow) {
	_, _ = fmt.Fprintf(out, "请求 %s：%d 条快照\n", requestID, len(rows))
	for i, row := range rows {
		printSnapshot(out, note, i+1, row)
	}
}

// printSnapshot 打一条快照。
//
// # 为什么这些列要逐行标出来
//
// 读快照的人是来复现一次故障的，他要知道的第一件事是「这条是什么」：
// 哪个方向、什么时候落的、上游当时什么状态、有没有报错。这些都在列上，
// 不在骨架里，而骨架本身不解释自己。
//
// # upstream_status 与 error_kind 按方向读
//
// 两列在**请求方向**的行上不适用：那一行回答的是「发出去的是什么」，
// 不记这次调用的结局。在**响应方向**的行上它们有值，而 NULL 也有确定的含义：
// 状态码 NULL 是「一个 HTTP 响应都没拿到」，错误分类 NULL 是「没有上游失败
// 可归」（见 upstreamStatusCell / errorKindCell）。
//
// 三个文本不能合并：<不适用> 说这一列在这个方向上没有意义，<无上游响应>
// 说上游没回过状态码，<无> 说这次没有上游失败。共用一个文本就会把其中
// 两种读成第三种。
//
// # 为什么截断必须显式说出来
//
// 被降级成摘要层的骨架与一份完整的骨架在肉眼下长得一样，而前者会让人以为
// 「上游就是只发了这些」。这一行不是排版，是这个数字唯一的出口。
//
// 它走 note（stderr）而不是 out：它是「这一行该怎么读」的附注，只在条件成立
// （这一条真的降级了）时出现；而 out 上那份是快照本身，要能接给 jq。
//
// # 为什么这几行不改成表格
//
// 这段输出的主体是一段 JSON 骨架（见下面 skeleton 那几行），它进不了任何一张表。
// 而上面那几栏本来就已经是「一栏一行」，不存在这一节要治的那个形态——超长单行
// 键值拼接。所以这里按清单那一条约定只做**视觉重心**：方向是这条快照的身份，
// 加粗；时刻置灰。
func printSnapshot(out, note io.Writer, seq int, row snapshotRow) {
	p := newPalette(out)
	_, _ = fmt.Fprintf(out, "\n第 %d 条\n", seq)
	_, _ = fmt.Fprintf(out, "  direction=%s\n", p.bold(row.Direction))
	_, _ = fmt.Fprintf(out, "  created_at=%s\n",
		p.dim(row.CreatedAt.UTC().Format(time.RFC3339Nano)))
	_, _ = fmt.Fprintf(out, "  skeleton_version=%d\n", row.SkeletonVersion)
	_, _ = fmt.Fprintf(out, "  capture_mode=%s\n", row.CaptureMode)
	_, _ = fmt.Fprintf(out, "  upstream_status=%s\n", upstreamStatusCell(row.Direction, row.UpstreamStatus))
	_, _ = fmt.Fprintf(out, "  error_kind=%s\n", errorKindCell(row.Direction, row.ErrorKind))
	_, _ = fmt.Fprintf(out, "  skeleton_truncated=%t\n", row.Truncated)
	printRedactedPaths(out, "  ", row.RedactedPaths)

	// 正文的标题按采集档分岔：两档在终端上都是「一段 JSON」，而读法完全不同
	// ——默认那一档是脱敏后的结构（内容已经被替换成占位描述符），另一档是
	// 用户原文。同一个标题会让「这份是不是原文」只能靠 capture_mode 那一行
	// 自己看出来，而它离正文有好几行。
	//
	// 判据只取一次：下面「降级了」那两句附注也要按同一档分岔，两处各判一遍
	// 迟早会出现「标题说原文、附注说骨架」。
	rawCapture := snapshotwire.CaptureModeIsRaw(row.CaptureMode)
	bodyLabel := "skeleton"
	if rawCapture {
		bodyLabel = "原文（用户已授权原样采集）"
	}

	// 授权已经不作数、或者这一行当场判断不了时，正文没有读出来。这一句打在
	// out 而不是 note，因为它回答的是「这一行的正文是什么」——与正文同一个
	// 出口；写在 note 上，一份保存下来的 stdout 里就会只剩一行没有正文的快照，
	// 看不出为什么。
	//
	// 措辞要把「没有授权」与「判断不了」一起盖住：查库失败时判定那一层按
	// 未授权回答（方向是安全的），而渲染这一层手上只有「正文被扣下了」这一个
	// 事实，说不出是到期、撤销，还是那次查询根本没成功。
	if row.RawWithheld {
		_, _ = fmt.Fprintf(out, "  %s：不显示——授权已失效或无法确认，原文不再可读\n", bodyLabel)
		return
	}

	if row.Truncated {
		if rawCapture {
			// 原文被降级比骨架被降级更该说重一句：授权采集的用途就是看原文，
			// 而这一份里的内容已经被摘要层丢掉了——它**不是**完整的原文。
			_, _ = fmt.Fprintln(note,
				"注意：这条是授权采集的原文，但它超限被降级成摘要层了——逐条消息的内容都丢了。")
			_, _ = fmt.Fprintln(note,
				"      授权采集就是为了看原文，所以这一份不是完整原文；要完整的得调大 request_snapshot.max_bytes 之后重发一次。")
		} else {
			_, _ = fmt.Fprintln(note,
				"注意：这条骨架被降级成摘要层了——超限时只留顶层标量，逐条消息与工具定义的细节都丢了。")
			_, _ = fmt.Fprintln(note,
				"      它与一份完整的骨架在肉眼下长得一样，别把「这里只有这些」读成「上游只发了这些」。")
		}
	}

	raw, err := json.MarshalIndent(row.Skeleton, "    ", "  ")
	if err != nil {
		// 正文是从 jsonb 里解出来的，正常情况下序列化不了是不可能的。
		// 不 panic：这条命令的价值在于把库里那条读出来，一格读不出来
		// 不该把整条命令带走。
		_, _ = fmt.Fprintf(out, "  %s：无法序列化（%v）\n", bodyLabel, err)
		return
	}
	_, _ = fmt.Fprintf(out, "  %s:\n    %s\n", bodyLabel, raw)
}

// printRedactedPaths 打印被替换掉的路径清单。
//
// 一条都没有时也打一行：空清单与「这一步没跑」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。这份清单的用途是回答「白名单是不是漏了该留的字段」，
// 所以它必须逐条看得见，不能只打一个数。
func printRedactedPaths(w io.Writer, indent string, paths []string) {
	if len(paths) == 0 {
		_, _ = fmt.Fprintf(w, "%sredacted_paths：0 条（没有任何字段被替换）\n", indent)
		return
	}
	_, _ = fmt.Fprintf(w, "%sredacted_paths：%d 条\n", indent, len(paths))
	for _, p := range paths {
		_, _ = fmt.Fprintf(w, "%s  %s\n", indent, p)
	}
}

const (
	// notApplicableOnRequest 是 upstream_status / error_kind 在请求方向行上的
	// 渲染文本。
	//
	// 请求方向的那一行回答的是「发出去的是什么」，上游状态码不在它回答的
	// 范围里。打成另一个文本会让读命令的人以为漏记了。
	notApplicableOnRequest = "<不适用（请求方向快照）>"

	// noUpstreamResponse 是响应方向快照上 upstream_status 为 NULL 的渲染文本。
	//
	// 它读作「上游一个 HTTP 响应都没给过」：连不上、超时、DNS 失败。
	// 不能写成 <未记录>——那读作「应该有值却漏记了」，而这一列在那几种
	// 结局里**本来就没有值**。同一条快照的 error_kind 会是
	// upstream_no_answer，两列一起读才能把「没拿到」与「拿到了但不是 2xx」
	// 分开。
	noUpstreamResponse = "<无上游响应>"

	// noUpstreamError 是响应方向快照上 error_kind 为 NULL 的渲染文本。
	//
	// 它表示「这次响应没有以上游失败告终」：正常结束、客户端断开、
	// 插件阻断都归这一档。写成 <未记录> 会把一次成功的请求说成漏记。
	noUpstreamError = "<无>"
)

// upstreamStatusCell 按快照方向渲染 upstream_status。
//
// 非 NULL 的值原样显示，哪怕它出现在请求方向的行上——那说明有写入路径
// 绕过了 internal/gateway 的采集点，把一个异常藏起来比显示出来更坏。
func upstreamStatusCell(direction string, v sql.NullInt64) string {
	if v.Valid {
		return strconv.FormatInt(v.Int64, 10)
	}
	if direction == string(snapshot.RootRequest) {
		return notApplicableOnRequest
	}
	return noUpstreamResponse
}

// errorKindCell 按快照方向渲染 error_kind。
//
// 与 upstreamStatusCell 同一个做法：有值照原样显示，NULL 按方向解释。
func errorKindCell(direction string, v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	if direction == string(snapshot.RootRequest) {
		return notApplicableOnRequest
	}
	return noUpstreamError
}

// snapshotAccessLogDefaultLimit 是 access-log 默认打几行。
//
// 这张表只追加，每一次 `snapshot get` 都写一行，不给上限时一次就是整表读回来；
// 看的人要的通常只是最近那一段。被截住时末行会说明，所以默认值不会让人把一份
// 截过的清单当成完整的。
const snapshotAccessLogDefaultLimit = 50

// snapshotAccessLogFilter 描述一次访问记录的读取范围。零值表示三个筛选条件
// 都不给、不限条数。
type snapshotAccessLogFilter struct {
	// RequestID 为空表示不限请求。
	RequestID string
	// ActorUserID 为 nil 表示不限读取者。
	ActorUserID *int64
	// From 是窗口起点（含），零值表示不限。
	From time.Time
	// To 是窗口终点（**不含**），零值表示不限。
	To time.Time
	// Limit 是一次最多读几行，0 表示不限。与其余清单一样多读一行判「后面还有没有」。
	Limit int
}

// snapshotAccessRow 是 snapshot_access_log 里读出来的一行。
//
// ActorUserID 可空：actor_kind 为 system 的那些行没有用户（表上该列可空）。
// Reason 可空：系统自己写的行没有理由，而人手读一次的理由是必填的（见
// runSnapshotGet）。
type snapshotAccessRow struct {
	ID          int64
	SnapshotID  int64
	RequestID   string
	ActorUserID sql.NullInt64
	ActorKind   string
	Reason      sql.NullString
	AccessedAt  time.Time
}

// runSnapshotAccessLog 列出快照的访问记录。
//
// 它读的就是 `snapshot get` 写下的那张表：写入那一侧在 readSnapshotsWithAudit
// （读快照与记日志在同一个事务里，所以日志里的每一行都对应一次真的读过）。
// 只写不读的话，「谁把快照读走了」这个问题在命令行上回答不了。
func runSnapshotAccessLog(args []string) error {
	fs := newFlagSet("snapshot access-log", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, snapshotShortUsage) }
	requestID := fs.String("request-id", "", "")
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	limit := fs.Int("limit", snapshotAccessLogDefaultLimit, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"snapshot access-log 不接受位置参数，得到 %q：按请求筛用 --request-id", fs.Arg(0)))
	}

	fromAt, err := parseLedgerTime("--from", *from)
	if err != nil {
		return usageErr(err)
	}
	toAt, err := parseLedgerTime("--to", *to)
	if err != nil {
		return usageErr(err)
	}
	if !fromAt.IsZero() && !toAt.IsZero() && !fromAt.Before(toAt) {
		return usageErr(fmt.Errorf(
			"--from 必须早于 --to（区间左闭右开），得到 %s 与 %s",
			fromAt.Format(time.RFC3339), toAt.Format(time.RFC3339)))
	}
	if *limit < 0 {
		return usageErr(fmt.Errorf("--limit 不能为负，得到 %d（0 表示不限）", *limit))
	}

	return withDB(snapshotReadTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		filter := snapshotAccessLogFilter{
			RequestID: strings.TrimSpace(*requestID),
			From:      fromAt,
			To:        toAt,
			Limit:     *limit,
		}
		if *user != "" {
			// 与 get 的 --actor 走同一个解析：两种写法（ID / 用户名）停在同一处。
			actor, err := authStore.ResolveUser(ctx, *user)
			if err != nil {
				return err
			}
			filter.ActorUserID = &actor.ID
		}

		rows, more, err := readSnapshotAccessLog(ctx, db, filter)
		if err != nil {
			return err
		}
		names, err := userNames(ctx, authStore)
		if err != nil {
			return err
		}

		if jsonOutput() {
			return emitJSON(os.Stdout, snapshotAccessLogDoc(rows, names, more, filter), nil)
		}
		printSnapshotAccessLog(humanStdout(), rows, names, more)
		return nil
	})
}

// readSnapshotAccessLog 读一页访问记录，新的在前。
//
// 排序补一个 id 是为了稳定：同一次 get 打出的多行、以及同一微秒写下的两行，只按
// accessed_at 排时两次读出来的顺序可以不同。
//
// 谓词一律按「调用方给了什么条件」现编（与 ledger/store 的只读查询同一条）：
// 写成 `($1 IS NULL OR request_id = $1)` 会把「这次有没有这个条件」搬进 SQL，
// 而一个恒真的分支足以让查询放弃走索引。
func readSnapshotAccessLog(ctx context.Context, db *sql.DB,
	f snapshotAccessLogFilter) ([]snapshotAccessRow, bool, error) {
	var (
		conds []string
		args  []any
	)
	param := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.RequestID != "" {
		conds = append(conds, "request_id = "+param(f.RequestID))
	}
	if f.ActorUserID != nil {
		conds = append(conds, "actor_user_id = "+param(*f.ActorUserID))
	}
	if !f.From.IsZero() {
		conds = append(conds, "accessed_at >= "+param(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "accessed_at < "+param(f.To))
	}

	query := `SELECT id, snapshot_id, request_id, actor_user_id, actor_kind, reason, accessed_at
	            FROM snapshot_access_log`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY accessed_at DESC, id DESC"
	if f.Limit > 0 {
		query += " LIMIT " + strconv.Itoa(f.Limit+1)
	}

	result, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("查询快照访问记录失败: %w", err)
	}
	defer func() { _ = result.Close() }()

	var rows []snapshotAccessRow
	for result.Next() {
		var row snapshotAccessRow
		if err := result.Scan(&row.ID, &row.SnapshotID, &row.RequestID,
			&row.ActorUserID, &row.ActorKind, &row.Reason, &row.AccessedAt); err != nil {
			return nil, false, fmt.Errorf("读快照访问记录失败: %w", err)
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, false, fmt.Errorf("读快照访问记录失败: %w", err)
	}

	if f.Limit > 0 && len(rows) > f.Limit {
		return rows[:f.Limit], true, nil
	}
	return rows, false, nil
}

// printSnapshotAccessLog 打一张访问记录清单。
//
// 空清单也说一句话：一行都不打时，「这个范围内没人读过快照」与「命令没跑成」在
// 安静的终端里长得一样（见 LESSONS.md 第 16 条）。这一句不套 style.go 的
// emptyListNote——访问记录由 get 写出来，没有「添加」这个动作。
func printSnapshotAccessLog(w io.Writer, rows []snapshotAccessRow,
	names map[int64]string, more bool) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "这个范围内没有快照访问记录。")
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("时刻"), p.bold("request_id"), p.bold("快照 ID"), p.bold("用户 ID"),
		p.bold("用户名"), p.bold("身份"), p.bold("理由"))
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.dim(r.AccessedAt.Format(longStampLayout)), p.bold(r.RequestID),
			strconv.FormatInt(r.SnapshotID, 10), actorIDCell(r.ActorUserID),
			names[r.ActorUserID.Int64], r.ActorKind, reasonCell(r.Reason))
	}
	_ = tw.Flush()
	printTruncatedListNote(w, more, len(rows))
}

// actorIDCell 打读取者的用户 ID，系统自己写的那一行没有用户。
//
// 没有用户时打 `-`，不打 0：0 不是一个合法的用户 ID，打出来会被读成「一个编号为 0
// 的用户读了它」。进 JSON 时那一栏是 null（见 snapshotAccessLogDoc）。
func actorIDCell(v sql.NullInt64) string {
	if !v.Valid {
		return "-"
	}
	return strconv.FormatInt(v.Int64, 10)
}

// reasonCell 打理由那一栏，没有理由时打 `-`。
func reasonCell(v sql.NullString) string {
	if !v.Valid || v.String == "" {
		return "-"
	}
	return v.String
}

// snapshotAccessLogDoc 组装 snapshot access-log 的机器可读输出。
//
// 理由原文照打（它是使用者写下的那句话），读取者的用户 ID 没有就是 null。
func snapshotAccessLogDoc(rows []snapshotAccessRow, names map[int64]string,
	more bool, f snapshotAccessLogFilter) map[string]any {
	summary := map[string]any{
		"total":     len(rows),
		"limit":     f.Limit,
		"truncated": more,
		// 这次筛的是什么，与其余清单的机器可读档一样带出来：一份只有总数的清单
		// 说不清这几十条是哪一段、哪一个请求的。
		"request_id": nullableString(f.RequestID),
		"from":       jsonTimeOrNull(f.From),
		"to":         jsonTimeOrNull(f.To),
	}
	if f.ActorUserID != nil {
		summary["user_id"] = *f.ActorUserID
	} else {
		summary["user_id"] = nil
	}

	entries := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, map[string]any{
			"id":            r.ID,
			"snapshot_id":   r.SnapshotID,
			"request_id":    r.RequestID,
			"actor_user_id": jsonNullInt64(r.ActorUserID),
			"username":      nullableString(names[r.ActorUserID.Int64]),
			"actor_kind":    r.ActorKind,
			"reason":        nullableString(r.Reason.String),
			"accessed_at":   jsonTime(r.AccessedAt),
		})
	}

	doc := jsonDoc(jsonOK)
	doc["summary"] = summary
	doc["accesses"] = entries
	return doc
}

// jsonNullInt64 打一个可空 bigint，没有就是 null。
func jsonNullInt64(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

// runSnapshotCleanup 删除到期分区。
//
// 它是留存承诺的兑现方式，所以输出的是**被删除的分区名**而不是一句
// 「完成」：一天都没到期时，命令的输出必须与「根本没跑成」区分得开。
func runSnapshotCleanup(args []string) error {
	if err := jsonUnsupported("snapshot cleanup"); err != nil {
		return err
	}

	fs := newFlagSet("snapshot cleanup", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, snapshotShortUsage) }
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		return err
	}
	snapCfg, err := snapshotwire.LoadConfig(values)
	if err != nil {
		return err
	}

	// 采集关掉时清理照旧执行：已有的分区还要按留存期清掉，
	// 「关掉」承诺的是不再写入新的，而不是把旧的留到永远。
	// 打不打提示由 printCleanupHint 自己判（采集开着时一个字都不打）。
	printCleanupHint(humanStderr(), snapCfg)

	// 授权来源在这一条命令上用不到：清理只删分区，一条也不采。传「永远没有
	// 授权」而不是 nil——nil 是「忘了接」，而这一个是一个明确的答案。
	collector, err := snapshotwire.New(snapCfg, snapshotwire.Wrap(db),
		snapshotwire.NoConsentSource{}, nil)
	if err != nil {
		return err
	}

	at := time.Now().UTC()
	if *dryRun {
		expired, listErr := collector.ExpiredPartitions(ctx, at)
		if listErr != nil {
			return listErr
		}
		printPartitions("将删除", expired, snapCfg.RetentionDays, at)
		return nil
	}

	dropped, err := collector.DropExpired(ctx, at)
	if err != nil {
		return err
	}
	printPartitions("已删除", dropped, snapCfg.RetentionDays, at)
	return nil
}

// printCleanupHint 说明这次清理是在采集关闭的情况下跑的。
//
// 单独一个函数是为了让它能被断言：这句提示的职责是把这次命令的前提说清楚，
// 而它曾经说过一件做不到的事——「本次只清理存量分区」，实现上却一步都没走，
// 因为「要不要写新快照」与「要不要清理存量」当时是同一个开关。
// 提示语与行为一致这件事，得有一条用例看着。
func printCleanupHint(w io.Writer, cfg snapshotwire.Config) {
	if cfg.Enabled {
		return
	}
	_, _ = fmt.Fprintf(w,
		"提示：快照采集已关闭，不再写入新快照；本次仍按留存期 %d 天清理存量分区。\n",
		cfg.RetentionDays)
}

// printPartitions 打印分区名清单。
//
// 一个都没删时也打印一行明确的说明，而不是什么都不输出：
// 「没有到期的分区」与「命令因为某个原因没跑起来」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。
//
// 那句说明里带上留存期与到期线，也就是这次清理用到的两个判据。只写一句
// 「没有到期分区」是不够的：它与「根本没查」还是分不开，而这条命令曾经正是
// 「说自己在清理、实际一步都没走，输出却完全正常」。把判据打出来，
// 「查过了、确实没有」才是可以核对的。
func printPartitions(verb string, names []string, retentionDays int, at time.Time) {
	if len(names) == 0 {
		cutoff := snapshotwire.RetentionCutoff(retentionDays, at)
		_, _ = fmt.Fprintf(humanStdout(), "%s：0 个（留存期 %d 天，到期线 %s；没有到期的分区）\n",
			verb, retentionDays, cutoff.Format("2006-01-02"))
		return
	}
	_, _ = fmt.Fprintf(humanStdout(), "%s %d 个分区：\n", verb, len(names))
	_, _ = fmt.Fprintln(humanStdout(), "  "+strings.Join(names, "\n  "))
}
