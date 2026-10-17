package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/settings"
)

const settingsUsage = `larkaxctl settings 查看与修改运行期配置。

用法：
  larkaxctl settings list
  larkaxctl settings set --key <键> --value <值> --by <用户 ID 或用户名>

配置项存在数据库的 settings 表里，与数据同生共死：备份恢复、换机器时它
跟着数据一起走，不会像环境变量那样漏掉一个就整体不可用。

键与类型定义在 internal/settings 的 Registry 里，每个键的说明存在库中
settings.description 那一列（由迁移写入，list 打的就是它）。**未声明的
键写不进去**——库里出现一个代码不认识的键，表现是配置被静默忽略，而写它的
人当场看不到任何反馈。

set 的值按明文给，不用写成 JSON：

  larkaxctl settings set --key request_snapshot.retention_days --value 7 --by ops
  larkaxctl settings set --key plugin.enabled --value true --by ops
  larkaxctl settings set --key reconcile.auto_repair --value false --by ops

整数直接写数字，布尔只认 true / false，字符串直接写、不用加引号。

--by 必填。settings.updated_by 记的是谁改的配置，而一条不记名的写入等于
没有审计——允许留空的话，最省事的用法就是永远不填。

有些键不接受这条路径的修改（settings.editable 为 false）：它们改错会造成
严重后果，只允许由迁移写入。命令会报错并指出是哪一个。

另有一些键**写得进去、但改了不会有任何效果**：代码里还没有读它们的路径
（Registry 里标了 Unwired 的那几个）。命令照常写入，并在输出里指出这一点——
不指出来的话，它与一个真的生效的键在这条命令的输出上完全一样。

密钥类配置不放这里（明文落盘并进备份），它们走 channels 表的信封加密。
这条由上面的白名单保证：写入只认 Registry 里声明过的键。
`

// settingsTimeout 是这两条命令连库的时间上限。
//
// 与 apiKeyTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。各写各的常量，是因为它们将来可能各调各的——共用一个是
// 「碰巧现在一样」的耦合。
const settingsTimeout = 30 * time.Second

func runSettings(args []string) error {
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, settingsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("settings 需要子命令")
	}
	action, rest := rest[0], rest[1:]

	switch action {
	case "list":
		return runSettingsList(rest)
	case "set":
		return runSettingsSet(rest)
	default:
		return fmt.Errorf("未知的 settings 子命令 %q", action)
	}
}

func runSettingsList(args []string) error {
	fs := flag.NewFlagSet("settings list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, settingsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0))
	}

	return withSettingsDB(func(ctx context.Context, _ *sql.DB, src *settings.PgSource) error {
		entries, err := src.List(ctx)
		if err != nil {
			return fmt.Errorf("读取 settings 失败: %w", err)
		}
		// 表先打出来。json 那一档在这里什么都不打，它打的是下面那份文档。
		if !jsonOutput() {
			printSettingsList(humanStdout(), entries)
		}

		// 列完之后再让 Load 校验一遍。它跑的是**服务启动时那一份**校验
		// （缺键、未声明的键、类型不对、JSON 不合法），所以这一步回答的是
		// 运维真正要问的那个问题：这套配置服务能不能加载起来。
		//
		// 结果收在 loadErr 里而不是直接返回：配置有问题的时候，恰恰是最需要
		// 看到内容的时候，而一个「校验不过就什么都不列」的清单等于在最需要它
		// 的时候闭嘴。json 那一档同一条：清单照打，结论由 exit_code 说。
		_, loadErr := settings.Load(ctx, src)
		if loadErr != nil {
			// 警告走 stderr，两档都是：stdout 是给管道下游的
			// （`larkaxctl settings list -o json | jq`），混进一行提示就会让
			// jq 报错——而它报的那句话离原因很远。
			_, _ = fmt.Fprintf(humanStderr(), "警告：这套配置服务加载不起来——%v\n", loadErr)
		}

		if jsonOutput() {
			return emitJSON(os.Stdout, settingsListDoc(entries), loadErr)
		}
		return loadErr
	})
}

// runSettingsSet 改一个配置项。
//
// # 为什么键与值也用旗标而不是位置参数
//
// Go 的 flag 包在遇到**第一个非旗标参数时就停止解析**，于是
//
//	larkaxctl settings set <键> <值> --by <用户>
//
// 里的 --by 会被当成第三个位置参数，报出来的错是「得到 4 个位置参数」——
// 那句话把使用者引向参数个数，而问题在参数顺序。把它写成旗标就没有这个
// 陷阱，形状也与其余子命令一致。另外以减号开头的值（--value -1）走旗标
// 是安全的，走位置参数会被当成一个旗标。
func runSettingsSet(args []string) error {
	// 在**函数体第一行**拦下来，早于 flag 解析、更早于连库：这条命令会写进
	// settings 表，做完再报不支持就成了一次没人知道的写入（见 format.go）。
	if err := jsonUnsupported("settings set"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("settings set", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, settingsUsage) }
	keyFlag := fs.String("key", "", "要改的配置键，例如 plugin.enabled")
	valueFlag := fs.String("value", "", "新的值，按明文给（不用写成 JSON）")
	by := fs.String("by", "", "改这个配置的用户：用户 ID 或用户名（必填）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 参数检查全部发生在连库之前（与 apikey / user 同一条约定）：
	// 一个写错的命令不该先连上生产库再报错，而「连上库之后才发现参数不对」
	// 还有一个更坏的可能——它没报错。
	if fs.NArg() > 0 {
		return fmt.Errorf("set 不接受位置参数，得到 %q：键用 --key、值用 --value", fs.Arg(0))
	}
	key, plain := *keyFlag, *valueFlag

	if key == "" {
		fs.Usage()
		return errors.New("缺少 --key：要改哪一个配置项")
	}
	if *by == "" {
		fs.Usage()
		return errors.New("缺少 --by：这条命令要记下是谁改的配置")
	}

	// 键名与值的类型都在这里就校验掉（见 settings.EncodeValue）。
	value, err := settings.EncodeValue(key, plain)
	if err != nil {
		return err
	}

	return withSettingsDB(func(ctx context.Context, db *sql.DB, src *settings.PgSource) error {
		// 先解析 --by 再写：settings.updated_by 上没有外键（那一列故意不设
		// 约束，好让迁移与手工 SQL 能留空），所以写进去一个不存在的用户 ID
		// 不会报错，只会留下一条查不到人的审计记录。
		actor, err := auth.New(db).ResolveUser(ctx, *by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}

		if err := src.Set(ctx, key, value, actor.ID); err != nil {
			return err
		}

		printSettingsSetResult(humanStderr(), key, value, actor.Username)
		return nil
	})
}

// printSettingsSetResult 打印一次写入的结果。
//
// 改写人只打名字，不带 `(id=N)`：名字已经认出是谁了，再挂一个 ID 是把身份说
// 两遍（见 docs/cli-output.md 的「实体只点一次身份」）。审计要的是用户 ID 时
// 看 settings.updated_by 那一列——它记的就是那个 ID。
//
// # 为什么未接线的键照样写、只是多打一行
//
// 不拒绝：Unwired 是一句**声明**，不是权限。库里的值仍然要能改——接线之前
// 把开关摆到接线后要用的位置、把间隔调成将来那个数，都是合理的操作；而拒绝
// 之后，「这个键存不存在、现在是什么值」在命令行上就看不出来了，那比一句
// 提醒更难用。
//
// 但那一行必须在**这条输出里**，不能只写进日志：调用方是在命令行上看的，
// 写进 slog 等于没写。没有它，改 plugin.enabled 与改 request_snapshot.enabled
// 的输出一模一样——而前者什么都不会发生（见 #215）。
//
// 抽成函数是为了有一条不连库也能跑的判据：写入本身要连库，而「输出里有没有
// 说出这件事」正是要钉住的东西。
func printSettingsSetResult(w io.Writer, key string, value []byte, by string) {
	_, _ = fmt.Fprintf(w, "已写入 settings.%s = %s 改自 %s\n", key, value, by)
	if note := settings.UnwiredNote(key); note != "" {
		_, _ = fmt.Fprintf(w, "  尚未接线：%s\n", note)
	}
}

// printSettingsList 打印全部配置项。
//
// 每项两行：第一行是键、值、类型与元数据（可 grep），第二行是说明
// （给人读，长短不一，挤进第一行会把表格撑烂）。
//
// 值打印的是 jsonb 原文而不是解码后的样子：这一列里到底存了什么，
// 正是核对时要看的东西，而解码会把它盖住——一个漏了引号的字符串配置
// 解码后与一个正常的看起来一样，而它会让服务起不来。
func printSettingsList(w io.Writer, entries []settings.Entry) {
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "settings 表是空的。")
		return
	}

	p := newPalette(w)
	_, _ = fmt.Fprintln(w, "值打的是 settings.value 的 jsonb 原文：字符串带引号，布尔与整数不带。")
	for _, e := range entries {
		// 未声明的键单独标出来：它是「服务加载不起来」的直接原因，
		// 而它在类型那一列里只是一句话，不标出来就得靠人记得去比对 Registry。
		kind := "未声明"
		unwired := ""
		if spec, ok := settings.Lookup(e.Key); ok {
			kind = string(spec.Kind)
			unwired = spec.Unwired
		}

		editable := "否"
		if e.Editable {
			editable = "是"
		}

		by := "-"
		if e.UpdatedBy != nil {
			by = strconv.FormatInt(*e.UpdatedBy, 10)
		}

		_, _ = fmt.Fprintf(w, "%s = %s  [%s]  可改 %s  改自 %s  改于 %s\n",
			p.bold(e.Key), e.Value, kind, editable, by, p.dim(e.UpdatedAt.Format(time.RFC3339)))
		if e.Description != "" {
			_, _ = fmt.Fprintf(w, "  说明：%s\n", e.Description)
		}
		// 还没接线的键单独一行。这一行不是修辞：没有它，改一个没人读的
		// 键与改一个真的生效的键，在这份输出上完全一样——两边都是
		// 「值变了、改自谁、改于何时」，而前者什么都不会发生。
		//
		// 它取自 Registry 而不是库里那一行 description：接不接线是代码的
		// 事实，而接线的人改的正是 Registry 里那几行。set 那条路上打的是
		// 同一份说明（见 settings.UnwiredNote），措辞不会分叉。
		if unwired != "" {
			_, _ = fmt.Fprintf(w, "  尚未接线：%s\n", unwired)
		}
	}
}

// settingsListDoc 组装 settings list 的机器可读输出。
//
// 与 printSettingsList 是同一份数据的另一条渲染路径（见 format.go 开头）。
// 表头那句「值打的是 settings.value 的 jsonb 原文」不进这里：它是给终端里看
// 表的人的一句说明，而 JSON 里值本身就是原文，没有第二种读法要交代。
func settingsListDoc(entries []settings.Entry) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(entries)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, settingsEntry(e))
	}
	doc["settings"] = rows
	return doc
}

// settingsEntry 打清单里的一条配置。
//
// value 走 json.RawMessage，也就是把库里那一列**原文**搬进 JSON。这一列里到底
// 存了什么，正是核对时要看的东西，而**解码再编码会把它盖住**：一个漏了引号的
// 字符串配置（类型写错，例如本该是 `"CNY"` 却写成 `7`）解码之后与一个正常的
// 值看起来差不多，可它会让服务起不来。原文还让大整数的位数原样经过，不借道
// float64——那一条正是这份输出的读者最不能接受的。
func settingsEntry(e settings.Entry) map[string]any {
	// kind 与 unwired 取自 Registry，不是库里那一列：类型与接不接线都是**代码**
	// 的事实（见 settings.UnwiredNote 的注释），而接线的人改的正是 Registry
	// 里那几行。Registry 里没有这个键时两栏都是 null——那正是「服务加载不起来」
	// 的直接原因，表格那一档把类型打成「未声明」，这里给 null，脚本按它筛得出来。
	var kind, unwired any
	if spec, ok := settings.Lookup(e.Key); ok {
		kind = string(spec.Kind)
		unwired = nullableString(spec.Unwired)
	}

	var updatedBy any
	if e.UpdatedBy != nil {
		updatedBy = *e.UpdatedBy
	}

	return map[string]any{
		"key":      e.Key,
		"value":    json.RawMessage(e.Value),
		"kind":     kind,
		"editable": e.Editable,
		// nil 是「没有记录」——迁移或手工 SQL 写下的行（库里那一列可空），
		// 与「改自用户 0」不是一回事。
		"updated_by": updatedBy,
		// 走 jsonTime（恒 UTC、完整精度），不走表格那一档的 Format：库里
		// 交回来的是带会话时区的时刻，直接打出来同一行在两台机器上不一样。
		"updated_at": jsonTime(e.UpdatedAt),
		// 空说明给 null 而不给空串：那一列是 NOT NULL DEFAULT ''，所以空串就是
		// 「这一行没有说明」，而 null 才是机器读得懂的那个「没有值」。
		"description": nullableString(e.Description),
		"unwired":     unwired,
	}
}

// withSettingsDB 打开数据库、确认连得上，再把句柄交给 fn。
//
// 收一个回调而不是把「打开」与「关闭」拆成两半：这两条命令各有一处提前
// 返回，而漏掉一处 defer 关闭的表现是连接慢慢漏光——那要等到线上跑了一阵
// 才现形。放在一个函数里，关闭与打开就是同一段代码。
func withSettingsDB(fn func(context.Context, *sql.DB, *settings.PgSource) error) error {
	return withDB(settingsTimeout, func(ctx context.Context, db *sql.DB) error {
		return fn(ctx, db, settings.NewPgSource(db))
	})
}
