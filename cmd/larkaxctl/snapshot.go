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
)

// snapshotUsage 是 snapshot 子命令的帮助。
//
// 几个动词服务同一件事的不同侧面：allowlist 回答「允许表里有什么」，
// redact 回答「这段报文交给它会长成什么样」，show 回答「这次请求当时存下了什么」，
// cleanup 负责把到期的删掉。中间两条是同一张表的两侧——redact 走脱敏的
// **输入**，show 读脱敏的**产出**。
const snapshotUsage = `larkaxctl snapshot 维护与查看请求结构快照。

用法：
  larkaxctl snapshot cleanup               按 settings 里的留存期删除到期分区
  larkaxctl snapshot cleanup --dry-run     只列出会被删除的分区，不删
  larkaxctl snapshot allowlist [--direction request|response] [--surface chat|decision]
        打印允许表：哪些字段放行、哪些替换成占位、每一行为什么这么定
  larkaxctl snapshot show <request_id> --actor <用户 ID 或用户名> --reason <文本>
        [--direction request|response]
        读出一条请求存下来的结构快照
  larkaxctl snapshot redact --direction request|response [--surface chat|decision]
        [--max-bytes <字节数>]
        从标准输入读一段 JSON 报文，按允许表脱敏，骨架打到标准输出

留存期取自 settings 表的 request_snapshot.retention_days，
不在命令行上另设参数：同一件事有两个来源时，命令行的那个会悄悄赢，
而「留存期到底是几天」会变成一件要看是谁执行的才知道的事。

allowlist 不需要连库。它只读 internal/snapshot 里那张允许表，那是纯代码里的
数据。所以它能在没有数据库、没有配置的机器上跑——而「我们到底存了什么」
这个问题，恰恰经常是在一个连不上生产库的地方被问出来的。

show 的 --actor 与 --reason 都是必填。读一条别人的请求结构是读敏感数据的
动作，所以每一次读都要留痕：snapshot_access_log 里会写下一行，记着谁、
什么时候、以什么理由看了哪一条。表里 reason 允许为空，那是给系统自己写的行
留的余地；人手读一次，理由必须留下。两个参数都不给默认值——「我写了
--reason」与「我忘了写」在库里必须分得开。写日志与读快照在同一个事务里：
一个「读了但没记」的成功返回看起来完全正常，而它意味着这张表存在的理由落空了。

同一个 request_id 可能有多行（请求与响应各一行，重试也可能各一行），
show 全部按 created_at 升序打出来。只取一行会让「这次请求还有另一半」看不见。
request_id 一条都没读到的时候非零退出，并且不写访问日志：没读到东西不算一次访问。

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
allowlist 与 show 不填时两个方向都处理。
`

// runSnapshot 处理 snapshot 子命令。
func runSnapshot(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, snapshotUsage)
		return usageErr(errors.New("snapshot 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "cleanup":
		return runSnapshotCleanup(rest)
	case "allowlist":
		return runSnapshotAllowlist(rest)
	case "show":
		return runSnapshotShow(rest)
	case "redact":
		return runSnapshotRedact(rest)
	case "help", "-h", "--help":
		fmt.Print(snapshotUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, snapshotUsage)
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
	fs := flag.NewFlagSet("snapshot allowlist", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, snapshotUsage) }
	direction := fs.String("direction", "", "只打某个方向（request / response），不填则两个方向都打")
	surfaceName := fs.String("surface", "", "只打某个表面（chat / decision），不填则每个表面都打")
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
	printAllowlists(os.Stdout, surfaces, roots)
	return nil
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
func printAllowlists(w io.Writer, surfaces []snapshot.Surface, roots []snapshot.Root) {
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
			for _, rule := range rules {
				_, _ = fmt.Fprintf(w, "  Path=%s Kind=%s UpstreamValue=%t\n",
					rule.Path, rule.Kind, rule.UpstreamValue)
				_, _ = fmt.Fprintf(w, "    Note=%s\n", rule.Note)
			}
		}
	}
}

// runSnapshotRedact 把一段报文交给脱敏器，打印它会变成什么。
//
// 这是 Decode 唯一的调用方，也是「这段报文我们存下来会是什么样」这个问题的
// 答案。它不连库：脱敏器是纯函数，报文从标准输入来。
func runSnapshotRedact(args []string) error {
	fs := flag.NewFlagSet("snapshot redact", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, snapshotUsage) }
	direction := fs.String("direction", "", "报文的方向：request 或 response（必填）")
	surfaceName := fs.String("surface", "", "报文的表面：chat（默认）或 decision")
	maxBytes := fs.Int("max-bytes", snapshot.DefaultMaxBytes,
		"骨架的字节上限，超过就降级为摘要层")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"snapshot redact 不接受位置参数，得到 %q：报文从标准输入读", fs.Arg(0)))
	}
	if strings.TrimSpace(*direction) == "" {
		fs.Usage()
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
		return fmt.Errorf("构造脱敏器失败: %w", err)
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
		return fmt.Errorf("解析报文失败: %w", err)
	}
	res, err := redactor.RedactRaw(payload)
	if err != nil {
		return fmt.Errorf("脱敏失败: %w", err)
	}
	return printRedaction(os.Stdout, os.Stderr, redactor, res)
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
func printRedaction(out, info io.Writer, redactor *snapshot.Redactor, res *snapshot.Result) error {
	_, _ = fmt.Fprintf(info, "表面=%s 方向=%s 骨架版本=%d 字节上限=%d 被截断=%t\n",
		redactor.Surface(), redactor.Root(), res.Version, redactor.MaxBytes(), res.Truncated)
	if res.Truncated {
		_, _ = fmt.Fprintln(info,
			"注意：骨架超限，已降级成摘要层（只留顶层标量），逐条消息与工具定义的细节都丢了。")
	}

	// MarshalIndent 对 map 的键是排序的，所以同一段报文两次跑出来逐行相同。
	raw, err := json.MarshalIndent(res.Skeleton, "", "  ")
	if err != nil {
		return fmt.Errorf("骨架无法序列化: %w", err)
	}
	_, _ = fmt.Fprintf(out, "%s\n", raw)

	printRedactedPaths(info, "", res.Redacted)
	_, _ = fmt.Fprintln(info, "注意：这是脱敏器对这段报文的结果，不是库里那条快照——")
	_, _ = fmt.Fprintln(info, "      库里那条来自解析之后的请求/响应，字段集合与这里不完全一样。")
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
const selectSnapshotsSQL = `
	SELECT id, direction, skeleton_version, capture_mode, skeleton,
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

// snapshotRead 是一次 show 的输入。
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
	Direction       string
	SkeletonVersion int
	CaptureMode     string
	Skeleton        map[string]any
	Truncated       bool
	UpstreamStatus  sql.NullInt64
	ErrorKind       sql.NullString
	RedactedPaths   []string
	CreatedAt       time.Time
}

// runSnapshotShow 读出一条请求存下来的结构快照。
//
// # 为什么分两趟解析参数
//
// Go 的 flag 包在遇到**第一个非旗标参数时就停止解析**，而这条命令的形状是
// `show <request_id> --actor ... --reason ...`——request_id 在旗标前面。
// 直接 Parse 会把 --actor 及其取值当成第二、第三个位置参数，报出来的错是
// 「收到 5 个位置参数」，那句话把人引向参数个数，而问题在参数顺序
// （settings set 的注释里记着同一个陷阱）。
//
// 所以先切出 request_id，再把剩下的交给同一个 FlagSet 解一遍。
// 于是 `show <id> --actor X` 与 `show --actor X <id>` 两种写法都能用，
// 而多余的位置参数照样会被拒——它们落在第二趟的 fs.Args() 里。
func runSnapshotShow(args []string) error {
	fs := flag.NewFlagSet("snapshot show", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, snapshotUsage) }
	actor := fs.String("actor", "", "读这条快照的人：用户 ID 或用户名（必填）")
	reason := fs.String("reason", "", "读它的理由，会落进 snapshot_access_log（必填）")
	direction := fs.String("direction", "", "只读某个方向（request / response），不填则都读")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	// 第一趟 Parse 之后，fs.Args() 是「第一个位置参数 + 它后面的一切」。
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageErr(errors.New(
			"用法：larkaxctl snapshot show <request_id> --actor <用户> --reason <文本>"))
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
				"snapshot show 只接受一个位置参数（request_id），多余的：%q", fs.Arg(0)))
		}
	}
	// 两个都不给默认值。理由见 snapshotUsage：「我写了 --reason」与
	// 「我忘了写」在库里必须分得开，而一个默认值恰好让这两件事在库里长得一样。
	if strings.TrimSpace(*actor) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --actor：读一条快照要记下是谁读的"))
	}
	if strings.TrimSpace(*reason) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --reason：读一条别人的请求结构，理由必须留下"))
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
		printSnapshots(os.Stdout, req.RequestID, rows)
		return nil
	})
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
// 一次 show 打出几行就写几行，于是「看了请求那一半还是响应那一半」
// 在日志里分得开。
func readSnapshotsWithAudit(ctx context.Context, db *sql.DB, req snapshotRead) ([]snapshotRow, error) {
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

// snapshotNotFound 说明一条都没读到。
func snapshotNotFound(req snapshotRead) error {
	if req.Filtered {
		return fmt.Errorf("请求 %q 没有 %s 方向的快照（去掉 --direction 或换一个再试）",
			req.RequestID, strings.Join(req.Directions, "、"))
	}
	return fmt.Errorf("请求 %q 没有存下来的快照", req.RequestID)
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
			&row.ID, &row.Direction, &row.SkeletonVersion, &row.CaptureMode,
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

// printSnapshots 把读回来的快照打到 w。
func printSnapshots(w io.Writer, requestID string, rows []snapshotRow) {
	_, _ = fmt.Fprintf(w, "请求 %s：%d 条快照\n", requestID, len(rows))
	for i, row := range rows {
		printSnapshot(w, i+1, row)
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
// 两列在**请求方向**的行上不适用：采集在选路与调上游之前，那时连会打到
// 哪个渠道都不知道。在**响应方向**的行上它们有值，而 NULL 也有确定的含义：
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
func printSnapshot(w io.Writer, seq int, row snapshotRow) {
	_, _ = fmt.Fprintf(w, "\n第 %d 条\n", seq)
	_, _ = fmt.Fprintf(w, "  direction=%s\n", row.Direction)
	_, _ = fmt.Fprintf(w, "  created_at=%s\n", row.CreatedAt.UTC().Format(time.RFC3339Nano))
	_, _ = fmt.Fprintf(w, "  skeleton_version=%d\n", row.SkeletonVersion)
	_, _ = fmt.Fprintf(w, "  capture_mode=%s\n", row.CaptureMode)
	_, _ = fmt.Fprintf(w, "  upstream_status=%s\n", upstreamStatusCell(row.Direction, row.UpstreamStatus))
	_, _ = fmt.Fprintf(w, "  error_kind=%s\n", errorKindCell(row.Direction, row.ErrorKind))
	_, _ = fmt.Fprintf(w, "  skeleton_truncated=%t\n", row.Truncated)
	printRedactedPaths(w, "  ", row.RedactedPaths)

	if row.Truncated {
		_, _ = fmt.Fprintln(w,
			"  注意：这条骨架被降级成摘要层了——超限时只留顶层标量，逐条消息与工具定义的细节都丢了。")
		_, _ = fmt.Fprintln(w,
			"        它与一份完整的骨架在肉眼下长得一样，别把「这里只有这些」读成「上游只发了这些」。")
	}

	raw, err := json.MarshalIndent(row.Skeleton, "    ", "  ")
	if err != nil {
		// 骨架是从 jsonb 里解出来的，正常情况下序列化不了是不可能的。
		// 不 panic：这条命令的价值在于把库里那条读出来，一格读不出来
		// 不该把整条命令带走。
		_, _ = fmt.Fprintf(w, "  skeleton：无法序列化（%v）\n", err)
		return
	}
	_, _ = fmt.Fprintf(w, "  skeleton:\n    %s\n", raw)
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
	// 请求方向的采集点在选路与调上游之前，那时没有上游状态码可记。
	// 打成另一个文本会让读命令的人以为漏记了。
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

// runSnapshotCleanup 删除到期分区。
//
// 它是留存承诺的兑现方式，所以输出的是**被删除的分区名**而不是一句
// 「完成」：一天都没到期时，命令的输出必须与「根本没跑成」区分得开。
func runSnapshotCleanup(args []string) error {
	fs := flag.NewFlagSet("snapshot cleanup", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, snapshotUsage) }
	dryRun := fs.Bool("dry-run", false, "只列出会被删除的分区，不执行删除")
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
	printCleanupHint(os.Stderr, snapCfg)

	collector, err := snapshotwire.New(snapCfg, snapshotwire.Wrap(db), nil)
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
		fmt.Printf("%s：0 个（留存期 %d 天，到期线 %s；没有到期的分区）\n",
			verb, retentionDays, cutoff.Format("2006-01-02"))
		return
	}
	fmt.Printf("%s %d 个分区：\n", verb, len(names))
	fmt.Println("  " + strings.Join(names, "\n  "))
}
