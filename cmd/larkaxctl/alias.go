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

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/config"
)

const aliasUsage = `larkaxctl alias 管理模型别名。

别名说的是「某家上游对同一个模型叫什么名字」：一个上游模型名对应一个规范名。

用法：
  larkaxctl alias create --model <规范名> --upstream <上游模型名>
                         [--channel <渠道 ID>] [--overrides <JSON 对象>]
  larkaxctl alias list [--channel <渠道 ID>] [--model <规范名>]
  larkaxctl alias update --id <别名 ID>
                         [--upstream <上游模型名>] [--overrides <JSON 对象>]
  larkaxctl alias delete --id <别名 ID>
  larkaxctl alias resolve --model <规范名> [--channel <渠道 ID>]

不带 --channel 的别名是**平台级**，对所有渠道生效；带 --channel 的是渠道级，
只对那个渠道生效，并盖过同一条平台级别名。同一个规范名在每个范围里最多
只能有一个上游名——两个上游名指向同一个规范名时，选路无从取舍。

list 默认列平台级别名；--channel <ID> 列那个渠道的渠道级别名；
--model <名称> 列出这个规范名在**所有范围**下对应的上游名（两种范围一起）。
后两者只能给一个。

create 的 stdout 只有新别名的 ID，其余信息走 stderr。

--overrides 是一组**上游请求参数**的覆盖，只允许三个键：temperature、top_p、
max_tokens。别的键在写入时就被拒绝——它们要么改的是请求的形状（model、
messages、tools、stream），要么根本不是这个请求的参数。与调用方冲突时的口径：
调用方**给了**的采样参数以调用方为准，没给的取覆盖值；max_tokens 反过来，
一律以覆盖值为准。

update 按 --id 定位，**渠道范围与规范名都不可改**：改渠道范围是换了一条别名，
改规范名等于把这条别名改挂到另一个计费键上，而那样同一次请求的选路与计费会
指向两个不同的模型。要改就删掉重建，两次改动各自看得见。--overrides '' 表示
清空这一栏。

resolve 回答的是选路那个问题：**这个规范名在这个渠道上实际会发给上游什么
名字**。它走的是请求热路径上同一条查询，所以渠道级别名盖过平台级那条这件事
在这里看得见（list 只是把两种范围各列一遍，不回答谁赢）。stdout 只有那个
上游模型名，其余信息走 stderr；--channel 留空表示不针对具体渠道，只看平台级
那条。一条都没配上时非零退出。

**旧名仍可用**：alias add 等同于 alias create，--canonical 等同于 --model。
它们保留是为了不让已经在跑的脚本与部署流程坏掉，行为与新名字逐字相同；
新写的命令请用新名字。

# 别名

下面两个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -m <规范名>            同 --model，create / list / resolve 三条都认
  --canonical <规范名>   同 --model

**--channel 不开简写**：-c 在别的命令行里既可能是 channel 也可能是 config、
count，而这条命令上挑错渠道范围的后果是别名挂到另一个渠道上——那类错误要等到
某次请求选路上游时才会现形。这个约束有用例钉着（见 short_flags_test.go）。
`

func runAlias(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, aliasUsage)
		return usageErr(errors.New("alias 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create", "add":
		return runAliasCreate(rest)
	case "list":
		return runAliasList(rest)
	case "update":
		return runAliasUpdate(rest)
	case "delete":
		return runAliasDelete(rest)
	case "resolve":
		return runAliasResolve(rest)
	case "help", "-h", "--help":
		fmt.Print(aliasUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, aliasUsage)
		return usageErr(fmt.Errorf("未知的 alias 子命令 %q", action))
	}
}

func runAliasCreate(args []string) error {
	if err := jsonUnsupported("alias create"); err != nil {
		return err
	}

	req, err := parseAliasCreate(args)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
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
	printCreatedAlias(humanStdout(), humanStderr(), id, req)
	return nil
}

func parseAliasCreate(args []string) (catalog.CreateAliasRequest, error) {
	fs := flag.NewFlagSet("alias create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, aliasUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "", "规范模型名，必须已经在目录里")
	stringAlias(fs, canonical, "m", sameAs("model"))
	upstream := fs.String("upstream", "", "这家上游对这个模型的叫法")
	channelID := fs.Int64("channel", 0, "渠道 ID，留空表示平台级别名")
	overrides := fs.String("overrides", "", "上游请求参数的覆盖，JSON 对象，只允许 temperature / top_p / max_tokens")
	if err := fs.Parse(args); err != nil {
		return catalog.CreateAliasRequest{}, err
	}
	if fs.NArg() > 0 {
		return catalog.CreateAliasRequest{}, fmt.Errorf(
			"create 不接受位置参数，得到 %q：规范名用 --model 指定", fs.Arg(0))
	}

	if strings.TrimSpace(*canonical) == "" {
		return catalog.CreateAliasRequest{}, errors.New("缺少 --model")
	}
	if strings.TrimSpace(*upstream) == "" {
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
	canonical := legacyStringFlag(fs, "model", "canonical", "", "按规范名列，两种范围一起列出")
	stringAlias(fs, canonical, "m", sameAs("model"))
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	// 判据要连旧名与简写一起看：写 `--canonical` 或 `-m` 时 Visit 记的是它们，
	// 不是 --model（见 flagGiven）。
	if given["channel"] && flagGiven(given, modelFlagNames...) {
		return usageErr(errors.New("--channel 与 --model 只能给一个：" +
			"前者按渠道列，后者按规范名列（它的结果里两种范围都有）"))
	}
	if *channelID < 0 {
		return usageErr(fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID))
	}

	cfg, err := config.LoadOps()
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
	if jsonOutput() {
		return emitJSON(os.Stdout, aliasListDoc(aliases), nil)
	}
	printAliasList(humanStdout(), aliases)
	return nil
}

func runAliasUpdate(args []string) error {
	if err := jsonUnsupported("alias update"); err != nil {
		return err
	}

	req, err := parseAliasUpdate(args)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
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

	if err := catalog.NewAdmin(db).UpdateAlias(ctx, req); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(humanStderr(), "已更新别名 %d。\n", req.ID)
	return nil
}

// parseAliasUpdate 解析 update 的参数。
//
// 没有 --channel 与 --model：那两栏不可改，见 catalog.UpdateAliasRequest。
// 把它们做成标志再拒绝，比根本没有这两个标志更容易让人以为改得动。
func parseAliasUpdate(args []string) (catalog.UpdateAliasRequest, error) {
	fs := flag.NewFlagSet("alias update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, aliasUsage) }
	id := fs.Int64("id", 0, "要改的别名 ID")
	upstream := fs.String("upstream", "", "这家上游对这个模型的叫法")
	overrides := fs.String("overrides", "", "上游请求参数的覆盖，JSON 对象（只允许 temperature / top_p / max_tokens），给空串表示清空")
	if err := fs.Parse(args); err != nil {
		return catalog.UpdateAliasRequest{}, err
	}
	if fs.NArg() > 0 {
		return catalog.UpdateAliasRequest{}, fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的别名用 --id 指定", fs.Arg(0))
	}
	if *id <= 0 {
		return catalog.UpdateAliasRequest{}, errors.New("缺少 --id（要改的别名 ID）")
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	req := catalog.UpdateAliasRequest{ID: *id}
	changes := 0
	if given["upstream"] {
		req.UpstreamModel = upstream
		changes++
	}
	if given["overrides"] {
		parsed, err := parseJSONObject("--overrides", *overrides)
		if err != nil {
			return catalog.UpdateAliasRequest{}, err
		}
		// 空串解析出来是 nil 映射，写入路径的 encodeJSONObject 把它编成 `{}`
		// ——那正是「这条别名没有覆盖」在库里的样子，所以清空不需要另一条路径。
		req.Overrides = &parsed
		changes++
	}
	if changes == 0 {
		return catalog.UpdateAliasRequest{}, errors.New(
			"update 至少要改一项：给出 --upstream 或 --overrides 之一")
	}

	return req, nil
}

func runAliasDelete(args []string) error {
	if err := jsonUnsupported("alias delete"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("alias delete", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, aliasUsage) }
	id := fs.Int64("id", 0, "要删的别名 ID")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"delete 不接受位置参数，得到 %q：要删的别名用 --id 指定", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id（要删的别名 ID）"))
	}

	cfg, err := config.LoadOps()
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

	if err := catalog.NewAdmin(db).DeleteAlias(ctx, *id); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(humanStderr(), "已删除别名 %d。\n", *id)
	return nil
}

// runAliasResolve 回答「这个规范名在这个渠道上实际会发给上游什么名字」。
//
// 它调的是选路用的那条查询（catalog.Catalog.UpstreamModel），不是另写一条：
// 「渠道级优先于平台级」这条判据只有一处，两处各写一份时，漂移的表现是
// 「同一次请求，选路与计费用了两个上游名」，而账单看上去完全正常。
func runAliasResolve(args []string) error {
	fs := flag.NewFlagSet("alias resolve", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, aliasUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "", "规范模型名")
	stringAlias(fs, canonical, "m", sameAs("model"))
	channelID := fs.Int64("channel", 0, "渠道 ID，留空表示不针对具体渠道（只看平台级那条）")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"resolve 不接受位置参数，得到 %q：规范名用 --model 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*canonical) == "" {
		return usageErr(errors.New("缺少 --model"))
	}
	if *channelID < 0 {
		return usageErr(fmt.Errorf(
			"--channel %d 无效：渠道 ID 从 1 开始，留空表示不针对具体渠道", *channelID))
	}

	cfg, err := config.LoadOps()
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

	// 覆盖在这里用不上（resolve 回答的是「叫什么名字」），但它与名字来自
	// 同一条别名，所以走的是同一个方法——分成两个方法的话，「按哪一条别名
	// 回答」这件事会有两份实现。
	upstream, _, err := catalog.New(db).Alias(ctx, *channelID, *canonical)
	if err != nil {
		return err
	}
	if jsonOutput() {
		return emitJSON(os.Stdout, aliasResolveDoc(*canonical, *channelID, upstream), nil)
	}
	printResolvedAlias(humanStdout(), humanStderr(), *canonical, *channelID, upstream)
	return nil
}

// printResolvedAlias 打印一条解析结果。
//
// 与 printCreatedUser 同一条契约：**stdout 恰好一行，只放能被直接接走的那个值**，
// 其余信息走 stderr。这里要接走的是上游模型名——
//
//	larkaxctl alias resolve --channel 3 --model m1
//
// 的输出可以直接喂给别的工具，而「用哪个范围的名字」这件事必须让人看见：
// 同一个规范名在渠道级与平台级各有一条时，只看名字分不出命中的是哪一条。
func printResolvedAlias(stdout, stderr io.Writer, canonical string, channelID int64, upstream string) {
	_, _ = fmt.Fprintf(stderr, "%s 在%s的上游模型名是 %s。\n",
		canonical, resolveScopeLabel(channelID), upstream)
	_, _ = fmt.Fprintln(stdout, upstream)
}

// resolveScopeLabel 把 resolve 的渠道参数打成一句范围说明。
func resolveScopeLabel(channelID int64) string {
	if channelID == 0 {
		return "平台级"
	}
	return "渠道 " + strconv.FormatInt(channelID, 10)
}

// printCreatedAlias 打印新别名。
//
// 与 printCreatedUser 同一条契约：stdout 恰好一行，只放能被直接接走的值。
//
// stderr 那一行把两个名字**各自标出来**，而不是写成「A 对应 B」：两个名字
// 风格相近时（都带连字符或斜杠），语序不说明哪个是哪个，要停下来想一下。
func printCreatedAlias(stdout, stderr io.Writer, id int64, req catalog.CreateAliasRequest) {
	_, _ = fmt.Fprintf(stderr, "已加入别名 id=%d：%s 上游名 %s ← 规范名 %s。\n",
		id, aliasScope(req.ChannelID), req.UpstreamModel, req.CanonicalName)
	_, _ = fmt.Fprintln(stdout, id)
}

// aliasListDoc 组装 alias list 的机器可读输出。
//
// 与 printAliasList 是同一份数据的另一条渲染路径：那一档把范围打成「平台」
// 「渠道 3」，这一档给 channel_id 的原始值（null 才是平台级）。
func aliasListDoc(aliases []catalog.Alias) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(aliases)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(aliases))
	for _, a := range aliases {
		entries = append(entries, map[string]any{
			"id": a.ID,
			// null 表示平台级别名（库里 channel_id 为 NULL），不是「渠道 0」。
			"channel_id":     jsonID(a.ChannelID),
			"upstream_model": a.UpstreamModel,
			"canonical_name": a.CanonicalName,
			"overrides":      jsonObject(a.Overrides),
		})
	}
	doc["aliases"] = entries
	return doc
}

// aliasResolveDoc 组装 alias resolve 的机器可读输出。
//
// 表格那一档（printResolvedAlias）在 stdout 上只放上游模型名，而这里要把
// 「问的是谁」一并带上：同一个规范名在渠道级与平台级各有一条时会给出不同的
// 上游名，只给结果的那份输出事后对不上是哪一次提问（见 resolveScopeLabel）。
func aliasResolveDoc(canonical string, channelID int64, upstream string) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["canonical"] = canonical
	// null 表示这次没针对具体渠道（只看平台级那条）。
	doc["channel_id"] = jsonID(channelID)
	doc["upstream_model"] = upstream
	return doc
}

// printAliasList 打印别名清单。
func printAliasList(w io.Writer, aliases []catalog.Alias) {
	if len(aliases) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("别名", "alias create"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		p.bold("ID"), p.bold("范围"), p.bold("上游模型名"), p.bold("规范名"), p.bold("覆盖"))
	for _, a := range aliases {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.bold(strconv.FormatInt(a.ID, 10)), aliasScope(a.ChannelID), p.bold(a.UpstreamModel),
			a.CanonicalName, overridesLabel(a.Overrides))
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
