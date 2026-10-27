package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/catalog"
)

// modelAliasHelp 是 `model alias` 的两版帮助文本（见 helpText）。
var modelAliasHelp = helpText{short: modelAliasShortUsage, long: modelAliasUsage}

// modelAliasShortUsage 是 model alias 的短版。
const modelAliasShortUsage = `larkaxctl model alias 管理入站别名：调用方请求里写的名字 → 规范名。

用法：
  larkaxctl model alias add <别名> <规范名>
  larkaxctl model alias remove <别名>
  larkaxctl model alias list

解析只发生在 API 入口一次，之后选路、计价、记账都按规范名走。别名不能与
已存在的规范名同名（解析时规范名优先，那条别名会被遮蔽）。

更细的边界：larkaxctl help model。
`

// modelAliasUsage 是 model alias 的长版。
const modelAliasUsage = `larkaxctl model alias 管理入站别名。

入站别名回答「调用方请求体里写这个名字时，指的是哪个规范模型」。方向与
channel map 相反：那张表是出站（发给上游时叫什么），这张是入站。

解析只在 API 入口发生一次，之后全链路按规范名走：选路、计价、缓存键与用量
记录看到的都是规范名。所以同一个逻辑请求写短名还是写全名，用量不会分成两行。
每解析一次是一次主键查询，没有缓存。

**响应体里的 model 字段回显调用方写的名字**（别名原样），不是规范名：别名
存在的意义就是客户端有一个稳定的名字。要用规范名去查用量或诊断，用
larkaxctl model trace 或 usage events。

三条规则：

  - 别名不能与已存在的规范名同名。解析时规范名优先、别名被遮蔽，「这个名字
    指谁」于是没有唯一答案，所以写入时就拒绝。
  - 别名指向的规范名必须在目录里（写入时校验）。目录行事后被删是另一回事，
    那种断链由 model trace 报出来。
  - 同一个别名只有一条映射，已存在时不静默覆盖。要改指向先 remove 再 add。

用法：
  larkaxctl model alias add <别名> <规范名>
  larkaxctl model alias remove <别名>
  larkaxctl model alias list

add 的 stdout 只有别名本身，其余信息走 stderr；remove 的 stdout 是空的。

更细的约定见 docs/cli-reference.md 的「model」一节。
`

// modelAliasActions 是 model alias 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var modelAliasActions = []string{"add", "remove", "list"}

// runModelAlias 分发 model alias 的三个动作。
func runModelAlias(args []string) error {
	if len(args) == 0 {
		cli.PrintUsage(os.Stderr, modelAliasShortUsage)
		return cli.UsageErr(errors.New("model alias 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runModelAliasAdd(rest)
	case "remove":
		return runModelAliasRemove(rest)
	case "list":
		return runModelAliasList(rest)
	case "help":
		cli.PrintUsage(os.Stdout, modelAliasHelp.long)
		return nil
	case "-h", "--help":
		cli.PrintUsage(os.Stdout, modelAliasHelp.short)
		return nil
	default:
		cli.PrintUsage(os.Stderr, modelAliasShortUsage)
		return cli.UsageErr(fmt.Errorf("未知的 model alias 子命令 %q", action))
	}
}

// runModelAliasAdd 新建一条入站别名。
func runModelAliasAdd(args []string) error {
	if err := cli.JSONUnsupported("model alias add"); err != nil {
		return err
	}

	alias, canonical, err := parseModelAliasPair(args)
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		if err := catalog.NewAdmin(db).CreateInboundAlias(ctx, alias, canonical); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cli.HumanStderr(), "已加入入站别名 %s → %s。\n", alias, canonical)
		_, _ = fmt.Fprintln(cli.HumanStdout(), alias)
		return nil
	})
}

// runModelAliasRemove 删除一条入站别名。
func runModelAliasRemove(args []string) error {
	fs := cli.NewFlagSet("model alias remove", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, modelAliasShortUsage) }
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() != 1 {
		return cli.UsageErr(fmt.Errorf("remove 需要一个位置参数（别名），得到 %d 个", fs.NArg()))
	}
	alias := fs.Arg(0)

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		if err := catalog.NewAdmin(db).DeleteInboundAlias(ctx, alias); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cli.HumanStderr(), "已删除入站别名 %s。\n", alias)
		return nil
	})
}

// runModelAliasList 列出入站别名。
func runModelAliasList(args []string) error {
	fs := cli.NewFlagSet("model alias list", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, modelAliasShortUsage) }
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		aliases, err := catalog.New(db).InboundAliases(ctx)
		if err != nil {
			return err
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, modelAliasListDoc(aliases), nil)
		}
		printModelAliasList(cli.HumanStdout(), aliases)
		return nil
	})
}

// parseModelAliasPair 解析 add 的两个位置参数。
//
// 校验放在连库之前：少给一个参数是命令错（退出码 2），不该先连一次库再报。
func parseModelAliasPair(args []string) (alias, canonical string, err error) {
	fs := cli.NewFlagSet("model alias add", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, modelAliasShortUsage) }
	if err = fs.Parse(args); err != nil {
		return "", "", err
	}
	if fs.NArg() != 2 {
		return "", "", fmt.Errorf("add 需要两个位置参数（别名、规范名），得到 %d 个", fs.NArg())
	}
	return fs.Arg(0), fs.Arg(1), nil
}

// printModelAliasList 打印入站别名清单。
func printModelAliasList(w io.Writer, aliases []catalog.InboundAlias) {
	if len(aliases) == 0 {
		_, _ = fmt.Fprintln(w, cli.EmptyListNote("入站别名", "model alias add"))
		return
	}

	tw := cli.NewTableWriter(w)
	p := cli.NewPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Bold("别名"), p.Bold("规范名"), p.Bold("创建于"))
	for _, a := range aliases {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
			p.Bold(a.Alias), a.CanonicalName, formatStampShort(a.CreatedAt))
	}
	_ = tw.Flush()
}

func modelAliasListDoc(aliases []catalog.InboundAlias) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["summary"] = map[string]any{"total": len(aliases)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 cli 包的 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(aliases))
	for _, a := range aliases {
		entries = append(entries, map[string]any{
			"alias":          a.Alias,
			"canonical_name": a.CanonicalName,
			"created_at":     cli.JSONTime(a.CreatedAt),
		})
	}
	doc["aliases"] = entries
	return doc
}
