package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/config"
)

const aliasUsage = `larkaxctl alias 管理模型别名。

别名说的是「某家上游对同一个模型叫什么名字」：一个上游模型名对应一个规范名。

用法：
  larkaxctl alias add --canonical <规范名> --upstream <上游模型名>
                      [--channel <渠道 ID>] [--overrides <JSON 对象>]
  larkaxctl alias list [--channel <渠道 ID>] [--canonical <规范名>]

不带 --channel 的别名是**平台级**，对所有渠道生效；带 --channel 的是渠道级，
只对那个渠道生效，并盖过同一条平台级别名。同一个规范名在每个范围里最多
只能有一个上游名——两个上游名指向同一个规范名时，选路无从取舍。

list 默认列平台级别名；--channel <ID> 列那个渠道的渠道级别名；
--canonical <名称> 列出这个规范名在**所有范围**下对应的上游名（两种范围一起）。
后两者只能给一个。

add 的 stdout 只有新别名的 ID，其余信息走 stderr。
`

func runAlias(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, aliasUsage)
		return usageErr(errors.New("alias 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runAliasAdd(rest)
	case "list":
		return runAliasList(rest)
	case "help", "-h", "--help":
		fmt.Print(aliasUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, aliasUsage)
		return usageErr(fmt.Errorf("未知的 alias 子命令 %q", action))
	}
}

func runAliasAdd(args []string) error {
	req, err := parseAliasAdd(args)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	id, err := catalog.NewAdmin(db).CreateAlias(ctx, req)
	if err != nil {
		return err
	}
	printCreatedAlias(os.Stdout, os.Stderr, id, req)
	return nil
}

func parseAliasAdd(args []string) (catalog.CreateAliasRequest, error) {
	fs := flag.NewFlagSet("alias add", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, aliasUsage) }
	canonical := fs.String("canonical", "", "规范模型名，必须已经在目录里")
	upstream := fs.String("upstream", "", "这家上游对这个模型的叫法")
	channelID := fs.Int64("channel", 0, "渠道 ID，留空表示平台级别名")
	overrides := fs.String("overrides", "", "对上游模型名的静态覆盖，JSON 对象")
	if err := fs.Parse(args); err != nil {
		return catalog.CreateAliasRequest{}, err
	}
	if fs.NArg() > 0 {
		return catalog.CreateAliasRequest{}, fmt.Errorf(
			"add 不接受位置参数，得到 %q：规范名用 --canonical 指定", fs.Arg(0))
	}

	if strings.TrimSpace(*canonical) == "" {
		fs.Usage()
		return catalog.CreateAliasRequest{}, errors.New("缺少 --canonical")
	}
	if strings.TrimSpace(*upstream) == "" {
		fs.Usage()
		return catalog.CreateAliasRequest{}, errors.New("缺少 --upstream")
	}
	if *channelID < 0 {
		return catalog.CreateAliasRequest{}, fmt.Errorf(
			"--channel %d 无效：渠道 ID 从 1 开始，留空表示平台级别名", *channelID)
	}
	parsedOverrides, err := parseJSONObject("--overrides", *overrides)
	if err != nil {
		return catalog.CreateAliasRequest{}, err
	}

	return catalog.CreateAliasRequest{
		ChannelID:     *channelID,
		UpstreamModel: *upstream,
		CanonicalName: *canonical,
		Overrides:     parsedOverrides,
	}, nil
}

func runAliasList(args []string) error {
	fs := flag.NewFlagSet("alias list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, aliasUsage) }
	channelID := fs.Int64("channel", 0, "渠道 ID，留空表示平台级别名")
	canonical := fs.String("canonical", "", "按规范名列，两种范围一起列出")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["channel"] && given["canonical"] {
		return usageErr(errors.New("--channel 与 --canonical 只能给一个：" +
			"前者按渠道列，后者按规范名列（它的结果里两种范围都有）"))
	}
	if *channelID < 0 {
		return usageErr(fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	store := catalog.New(db)
	var aliases []catalog.Alias
	if strings.TrimSpace(*canonical) != "" {
		aliases, err = store.UpstreamModels(ctx, *canonical)
	} else {
		aliases, err = store.Aliases(ctx, *channelID)
	}
	if err != nil {
		return err
	}
	printAliasList(os.Stdout, aliases)
	return nil
}

// printCreatedAlias 打印新别名。
//
// 与 printCreatedUser 同一条契约：stdout 恰好一行，只放能被直接接走的值。
func printCreatedAlias(stdout, stderr io.Writer, id int64, req catalog.CreateAliasRequest) {
	_, _ = fmt.Fprintf(stderr, "已加入别名 id=%d：%s 的 %s 对应 %s。\n",
		id, aliasScope(req.ChannelID), req.UpstreamModel, req.CanonicalName)
	_, _ = fmt.Fprintln(stdout, id)
}

// printAliasList 打印别名清单。
func printAliasList(w io.Writer, aliases []catalog.Alias) {
	if len(aliases) == 0 {
		_, _ = fmt.Fprintln(w, "没有别名。")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\t范围\t上游模型名\t规范名\t覆盖")
	for _, a := range aliases {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			a.ID, aliasScope(a.ChannelID), a.UpstreamModel, a.CanonicalName,
			overridesLabel(a.Overrides))
	}
	_ = tw.Flush()
}

// aliasScope 把渠道 ID 打成「平台」或「渠道 N」。
//
// 0 表示平台级（库里 channel_id 为 NULL），见 catalog.Alias 的注释。
// 直接打 0 会让人以为那是一个真实的渠道。
func aliasScope(channelID int64) string {
	if channelID == 0 {
		return "平台"
	}
	return "渠道 " + strconv.FormatInt(channelID, 10)
}

// overridesLabel 打别名的覆盖对象。
func overridesLabel(overrides map[string]any) string {
	if len(overrides) == 0 {
		return "-"
	}
	raw, err := json.Marshal(overrides)
	if err != nil {
		// 它刚从 jsonb 解出来，再编回去编不动只可能是内存之类的问题。
		// 列举不该因为一栏打不出来就整次失败，所以这里退化成一句说明。
		return "（无法显示）"
	}
	return string(raw)
}
