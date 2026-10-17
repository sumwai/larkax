package main

import (
	"context"
	"database/sql"
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

键的定义在 internal/settings 的 Registry 里（类型与一句说明）。**未声明的
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
		printSettingsList(os.Stdout, entries)

		// 列完之后再让 Load 校验一遍。它跑的是**服务启动时那一份**校验
		// （缺键、未声明的键、类型不对、JSON 不合法），所以这一步回答的是
		// 运维真正要问的那个问题：这套配置服务能不能加载起来。
		//
		// 放在列完之后：配置有问题的时候，恰恰是最需要看到内容的时候，
		// 而一个「校验不过就什么都不列」的清单等于在最需要它的时候闭嘴。
		if _, err := settings.Load(ctx, src); err != nil {
			fmt.Fprintf(os.Stderr, "警告：这套配置服务加载不起来——%v\n", err)
			return err
		}
		return nil
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

		_, _ = fmt.Fprintf(os.Stderr, "已写入 settings.%s = %s 改自 %s(id=%d)\n",
			key, value, actor.Username, actor.ID)
		return nil
	})
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
			e.Key, e.Value, kind, editable, by, e.UpdatedAt.Format(time.RFC3339))
		if e.Description != "" {
			_, _ = fmt.Fprintf(w, "  说明：%s\n", e.Description)
		}
		// 还没接线的键单独一行。这一行不是修辞：没有它，改一个没人读的
		// 键与改一个真的生效的键，在这份输出上完全一样——两边都是
		// 「值变了、改自谁、改于何时」，而前者什么都不会发生。
		//
		// 它取自 Registry 而不是库里那一行 description：接不接线是代码的
		// 事实，而接线的人改的正是 Registry 里那几行。
		if unwired != "" {
			_, _ = fmt.Fprintf(w, "  尚未接线：%s\n", unwired)
		}
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
