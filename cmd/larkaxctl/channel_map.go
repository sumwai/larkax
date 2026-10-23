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
	"strings"

	"github.com/sumwai/larkax/internal/catalog"
)

// channelMapHelp 是 channel map 的两版帮助文本（见 helpText）。
var channelMapHelp = helpText{short: channelMapShortUsage, long: channelMapUsage}

// channelMapShortUsage 是 channel map 的短版：`-h` / `--help` 与参数写错时打的一份。
const channelMapShortUsage = `larkaxctl channel map 管理出站映射：某个渠道把规范名发给上游时用什么名字。

用法：
  larkaxctl channel map add <渠道> <规范名> <上游模型名> [--overrides <JSON 对象>]
  larkaxctl channel map add --all-channels <规范名> <上游模型名> [--overrides <JSON 对象>]
  larkaxctl channel map list [<渠道>] [--all-channels] [--model <规范名>]
  larkaxctl channel map remove <渠道> <规范名>
  larkaxctl channel map remove --all-channels <规范名>

--all-channels 表示平台级映射，对所有渠道生效；渠道级映射盖过它。
不带 <渠道> 的 list 列平台级映射；--model 列一个规范名在所有范围下的映射。
更细的边界：larkaxctl channel map help。
`

// channelMapUsage 是 channel map 的长版：`larkaxctl channel map help` 与
// `channel map help`。
const channelMapUsage = `larkaxctl channel map 管理出站映射。

映射的键是规范名（调用方请求里那个名字），值是这家上游对同一个模型的叫法。
请求进来时按渠道查一条：渠道级映射盖过平台级那条；两者都没有时，规范名原样
发给上游。

用法：
  larkaxctl channel map add <渠道> <规范名> <上游模型名> [--overrides <JSON 对象>]
  larkaxctl channel map add --all-channels <规范名> <上游模型名> [--overrides <JSON 对象>]
  larkaxctl channel map list [<渠道>] [--all-channels] [--model <规范名>]
  larkaxctl channel map remove <渠道> <规范名>
  larkaxctl channel map remove --all-channels <规范名>

**--all-channels 表示平台级映射**：它对所有渠道生效，位置参数里不再给渠道。
不带它时第一个位置参数是渠道 ID，这条映射只对那个渠道生效。两种范围不会同时
给出——一条映射只属于一个范围。

add 的 stdout 只有新映射的 ID，其余信息走 stderr。

list 不带参数时列平台级映射；给 <渠道> 列那个渠道的渠道级映射，--all-channels
是平台级的显式写法。<渠道> 与 --all-channels 只能给一个。--model <规范名> 列出
这个名字在**所有范围**下的映射，不能与 <渠道> / --all-channels 同时给。平台级
与渠道级分在两处列：混在一条清单里会让人以为渠道级那条对所有渠道生效。

remove 按「范围 + 规范名」定位：一个范围里一个规范名最多一条映射，这两个值
就够确定要删哪一条，不必先 list 一遍拿 ID。

--overrides 是一组**上游请求参数**的覆盖，只允许三个键：temperature、top_p、
max_tokens。别的键在写入时就被拒绝——它们要么改的是请求的形状（model、
messages、tools、stream），要么根本不是这个请求的参数。与调用方冲突时的口径：
调用方**给了**的采样参数以调用方为准，没给的取覆盖值；max_tokens 反过来，
一律以覆盖值为准。

**旧写法仍可用**：alias create / add、alias list、alias delete 会继续按原来的
方式写入与读取，并在 stderr 上提示这里的新路径；它们保留一个发布周期。改一条
映射的属性仍用 alias update，解析某个渠道实际会发什么名字仍用 alias resolve。

更细的约定见 docs/cli-reference.md 的「channel map」一节。
`

// channelMapActions 是 channel map 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var channelMapActions = []string{"add", "list", "remove"}

func runChannelMap(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, channelMapShortUsage)
		return usageErr(errors.New("channel map 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runChannelMapAdd(rest)
	case "list":
		return runChannelMapList(rest)
	case "remove":
		return runChannelMapRemove(rest)
	case "help":
		printUsage(os.Stdout, channelMapHelp.long)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, channelMapHelp.short)
		return nil
	default:
		printUsage(os.Stderr, channelMapShortUsage)
		return usageErr(fmt.Errorf("未知的 channel map 子命令 %q", action))
	}
}

func runChannelMapAdd(args []string) error {
	if err := jsonUnsupported("channel map add"); err != nil {
		return err
	}

	req, err := parseChannelMapAdd(args)
	if err != nil {
		return usageErr(err)
	}

	var id int64
	err = withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		created, createErr := catalog.NewAdmin(db).CreateAlias(ctx, req)
		if createErr != nil {
			return createErr
		}
		id = created
		return nil
	})
	if err != nil {
		return err
	}
	printCreatedChannelMap(humanStdout(), humanStderr(), id, req)
	return nil
}

// parseChannelMapAdd 解析 add 的三个位置参数与 --overrides。
//
// 位置参数排在旗标前面（与 user 那一族的位置参数命令同一个形状）：
// `--overrides` 写在三个名字之后。--all-channels 是开关，前后都能写。
func parseChannelMapAdd(args []string) (catalog.CreateAliasRequest, error) {
	allChannels, args, err := takeAllChannelsFlag(args)
	if err != nil {
		return catalog.CreateAliasRequest{}, err
	}

	var channelRef, canonical, upstream string
	if allChannels {
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return catalog.CreateAliasRequest{}, err
		}
		if upstream, args, err = takeOperand(args, "<上游模型名>"); err != nil {
			return catalog.CreateAliasRequest{}, err
		}
	} else {
		if channelRef, args, err = takeOperand(args, "<渠道>"); err != nil {
			return catalog.CreateAliasRequest{}, err
		}
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return catalog.CreateAliasRequest{}, err
		}
		if upstream, args, err = takeOperand(args, "<上游模型名>"); err != nil {
			return catalog.CreateAliasRequest{}, err
		}
	}

	fs := newFlagSet("channel map add", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelMapShortUsage) }
	overrides := fs.String("overrides", "", "")
	if parseErr := fs.Parse(args); parseErr != nil {
		return catalog.CreateAliasRequest{}, parseErr
	}
	if fs.NArg() > 0 {
		if allChannels {
			return catalog.CreateAliasRequest{}, fmt.Errorf(
				"带 --all-channels 时只给两个位置参数：<规范名> <上游模型名>；多出来的 %q 是渠道，平台级映射不挂渠道",
				fs.Arg(0))
		}
		return catalog.CreateAliasRequest{}, fmt.Errorf(
			"add 不接受多余的参数，得到 %q：渠道、规范名与上游模型名依次写成三个位置参数",
			fs.Arg(0))
	}

	var channelID int64
	if !allChannels {
		channelID, err = parseChannelOperand(channelRef)
		if err != nil {
			return catalog.CreateAliasRequest{}, err
		}
	}
	// 上游模型名进 list 的「上游模型名」那一栏：控制字符在连库之前拒掉
	// （判据在 catalog 的 normalizeUpstreamModel，与旧路径同一处）。
	if checkErr := checkDisplayText("<上游模型名>", strings.TrimSpace(upstream)); checkErr != nil {
		return catalog.CreateAliasRequest{}, checkErr
	}
	parsedOverrides, err := parseJSONObject("--overrides", *overrides)
	if err != nil {
		return catalog.CreateAliasRequest{}, err
	}

	return catalog.CreateAliasRequest{
		ChannelID:     channelID,
		UpstreamModel: upstream,
		CanonicalName: canonical,
		Overrides:     parsedOverrides,
	}, nil
}

func runChannelMapList(args []string) error {
	channelID, canonical, err := parseChannelMapList(args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		store := catalog.New(db)
		var aliases []catalog.Alias
		if strings.TrimSpace(canonical) != "" {
			aliases, err = store.UpstreamModels(ctx, canonical)
		} else {
			aliases, err = store.Aliases(ctx, channelID)
		}
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, aliasListDoc(aliases), nil)
		}
		printAliasList(humanStdout(), aliases, "映射", "channel map add")
		return nil
	})
}

// parseChannelMapList 解析 list 的范围。
//
// 三种范围互斥：平台级（不给参数或 --all-channels）、某个渠道（<渠道>），
// 以及一个规范名的全部范围（--model）。返回值里 channelID 为 0 表示平台级。
func parseChannelMapList(args []string) (int64, string, error) {
	allChannels, args, err := takeAllChannelsFlag(args)
	if err != nil {
		return 0, "", err
	}

	var channelID int64
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		channelRef, rest, err := takeOperand(args, "<渠道>")
		if err != nil {
			return 0, "", err
		}
		args = rest
		if allChannels {
			return 0, "", errors.New("<渠道> 与 --all-channels 只能给一个：" +
				"前者列那个渠道的映射，后者列平台级映射")
		}
		if channelID, err = parseChannelOperand(channelRef); err != nil {
			return 0, "", err
		}
	}

	fs := newFlagSet("channel map list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelMapShortUsage) }
	canonical := fs.String("model", "", "")
	if err := fs.Parse(args); err != nil {
		return 0, "", err
	}
	if fs.NArg() > 0 {
		return 0, "", fmt.Errorf("list 不接受多余的参数，得到 %q：渠道写成位置参数，规范名用 --model",
			fs.Arg(0))
	}

	if strings.TrimSpace(*canonical) != "" {
		if channelID != 0 || allChannels {
			return 0, "", errors.New("--model 与 <渠道> / --all-channels 只能给一个：" +
				"前者列出这个名字在所有范围下的映射")
		}
	}
	return channelID, *canonical, nil
}

func runChannelMapRemove(args []string) error {
	if err := jsonUnsupported("channel map remove"); err != nil {
		return err
	}

	channelID, canonical, err := parseChannelMapRemove(args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		store := catalog.New(db)
		aliases, err := store.UpstreamModels(ctx, canonical)
		if err != nil {
			return err
		}

		// 同一个规范名在一个范围里最多一条（两条偏唯一索引挡着），所以这里
		// 按范围挑出来的就是那一条，不需要再排个序。
		var found catalog.Alias
		matched := false
		for _, alias := range aliases {
			if alias.ChannelID == channelID {
				found, matched = alias, true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w：%s 的规范名 %q", catalog.ErrAliasNotFound,
				aliasScope(channelID), canonical)
		}
		if err := catalog.NewAdmin(db).DeleteAlias(ctx, found.ID); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(humanStderr(),
			"已删除映射 id=%d：%s 规范名 %s（上游名 %s）。\n",
			found.ID, aliasScope(channelID), found.CanonicalName, found.UpstreamModel)
		return nil
	})
}

// parseChannelMapRemove 解析 remove 的范围与规范名。
func parseChannelMapRemove(args []string) (int64, string, error) {
	allChannels, args, err := takeAllChannelsFlag(args)
	if err != nil {
		return 0, "", err
	}

	var channelRef, canonical string
	if allChannels {
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return 0, "", err
		}
	} else {
		if channelRef, args, err = takeOperand(args, "<渠道>"); err != nil {
			return 0, "", err
		}
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return 0, "", err
		}
	}
	if len(args) > 0 {
		return 0, "", fmt.Errorf("remove 不接受多余的参数，得到 %q", args[0])
	}

	var channelID int64
	if !allChannels {
		if channelID, err = parseChannelOperand(channelRef); err != nil {
			return 0, "", err
		}
	}
	return channelID, canonical, nil
}

// takeAllChannelsFlag 从参数里摘出 --all-channels。
//
// 它是开关、不带取值，所以与位置参数没有先后关系：取位置参数之前先摘掉它，
// `add --all-channels <规范名> <上游模型名>` 与
// `add <规范名> <上游模型名> --all-channels` 都认。
func takeAllChannelsFlag(args []string) (bool, []string, error) {
	all := false
	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "--all-channels" || arg == "-all-channels":
			all = true
		case strings.HasPrefix(arg, "--all-channels=") || strings.HasPrefix(arg, "-all-channels="):
			return false, nil, errors.New("--all-channels 是开关，不带取值：写成 --all-channels")
		default:
			out = append(out, arg)
		}
	}
	return all, out, nil
}

// parseChannelOperand 把位置参数里的渠道 ID 解析成正整数。
func parseChannelOperand(ref string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(ref), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("<渠道> 必须是正整数 ID，得到 %q", ref)
	}
	return id, nil
}

// printCreatedChannelMap 打印新映射。
//
// 与 printCreatedAlias 同一条契约：stdout 恰好一行，只放能被直接接走的值；
// stderr 那一行把两个名字各自标出来。措辞用「映射」——这条命令管的就是出站
// 映射，不再借用「别名」那个词。
func printCreatedChannelMap(stdout, stderr io.Writer, id int64, req catalog.CreateAliasRequest) {
	_, _ = fmt.Fprintf(stderr, "已加入映射 id=%d：%s 上游名 %s ← 规范名 %s。\n",
		id, aliasScope(req.ChannelID), req.UpstreamModel, req.CanonicalName)
	_, _ = fmt.Fprintln(stdout, id)
}
