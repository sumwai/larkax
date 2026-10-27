package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
)

// channelMapHelp 是 channel map 的两版帮助文本（见 helpText）。
var channelMapHelp = helpText{short: channelMapShortUsage, long: channelMapUsage}

// channelMapShortUsage 是 channel map 的短版：`-h` / `--help` 与参数写错时打的一份。
const channelMapShortUsage = `larkaxctl channel map 管理出站映射：某个渠道把规范名发给上游时用什么名字。

用法：
  larkaxctl channel map <渠道> <规范名> [<上游模型名>] [--overrides <JSON 对象>]
  larkaxctl channel map --all <规范名> <上游模型名> [--overrides <JSON 对象>]
  larkaxctl channel unmap <渠道> <规范名>
  larkaxctl channel unmap --all <规范名>
  larkaxctl channel map list [<渠道>] [--all] [--model <规范名>]

渠道级 map 同时把规范名写进该渠道的 models 白名单；unmap 只删映射，白名单保留。
<渠道> 认 ID，也认未软删渠道的名字。第一个位置参数是 add / list / remove / help
时按子命令解释，其余按「动词 + 渠道」。
更细的边界：larkaxctl channel map help。
`

// channelMapUsage 是 channel map 的长版：`larkaxctl channel map help` 与
// `channel map help`。
const channelMapUsage = `larkaxctl channel map 管理出站映射。

映射的键是规范名（入站解析之后的规范名），值是这家上游对同一个模型的叫法。
请求进来时按渠道查一条：渠道级映射盖过平台级那条；两者都没有时，规范名原样
发给上游。

用法：
  larkaxctl channel map <渠道> <规范名> [<上游模型名>] [--overrides <JSON 对象>]
  larkaxctl channel map --all <规范名> <上游模型名> [--overrides <JSON 对象>]
  larkaxctl channel unmap <渠道> <规范名>
  larkaxctl channel unmap --all <规范名>
  larkaxctl channel map list [<渠道>] [--all] [--model <规范名>]

**映射即上架**：渠道级 map 在同一个事务里做两件事——建这条映射，并把规范名写进
那个渠道的 models 白名单。渠道服务哪些模型就是它有哪些映射，白名单不必再手工
同步。任何一步失败都整笔回滚，所以不会留下「映射建了、渠道却服务不了这个模型」
的半截配置：白名单写入要过端点校验，模型在目录里声明的端点服务不了这条渠道时
会被拒。白名单里已经有这个规范名时不重复写，命令照样成功——重复跑同一条 map 是
幂等的。反过来，同一个范围里同一个规范名已有映射时报错，不会悄悄改掉它的上游名
（要改上游名用 alias update）。

它**不建模型目录条目**：规范名要先用 model add 建进目录（要一次配齐目录、白名单、
映射与两条价格规则时用 model onboard）。

**unmap 只删映射，不删白名单条目**：反义是「不再翻译」，不是「渠道不再服务这个
模型」。那个名字继续留在渠道的 models 里，由 channel update --models 管。

**--all 表示平台级映射**（--all-channels 是它的全写）：它对所有渠道生效，位置
参数里不再给渠道，只写映射表、**不碰任何渠道的白名单**——它是一层通用翻译，不是
把名字上架到每条渠道。不带它时第一个位置参数是渠道。两种范围不会同时给出。

**渠道级的 <上游模型名> 可以省略**：省了就用规范名本身，即直通形态。落库结果与
显式写一个同名的上游名逐字相同。带 --all 的那条不能省——平台级映射是通用翻译，
「规范名 → 同名」不改变任何翻译结果，只在 map list 里多一行，所以按参数错误拒掉。

**<渠道> 认 ID，也认名字**：纯数字按 ID，其余按名字在**未软删**的渠道里查。名字
在库里不唯一，命中多条时报错并列出候选的 id、类型与地址，改用 ID 指定。**名字恰好是
add / list / remove / help 之一的渠道只能用 ID 指代**——第一个位置参数落在这些
保留词上时按子命令解释（见下）。

**保留词优先，且只判第一个位置参数**：第一个位置参数是 add / list / remove /
help / -h / --help 时按子命令走（add 与 remove 是兼容别名），其余一律按「动词 +
渠道」走。第二个及之后的位置参数原样当名字，所以 channel map 42 list 里的 list
是规范名，不是子命令。

list 不带参数时列平台级映射；给 <渠道> 列那个渠道的渠道级映射，--all 是平台级
的显式写法。<渠道> 与 --all 只能给一个。--model <规范名> 列出这个名字在**所有
范围**下的映射，不能与 <渠道> / --all 同时给。平台级与渠道级分在两处列：混在
一条清单里会让人以为渠道级那条对所有渠道生效。

**兼容别名**：channel map add 等于 channel map，channel map remove 等于
channel unmap。两者保留一个发布周期，行为与动词语法逐字相同。

map 的 stdout 只有新映射的 ID，其余信息走 stderr；unmap 的 stdout 是空的。

unmap 按「范围 + 规范名」定位：一个范围里一个规范名最多一条映射，这两个值就够
确定要删哪一条，不必先 list 一遍拿 ID。

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

// channelMapActions 是 channel map 的子命令清单，补全从这里取。
//
// 它列的是**保留词**（兼容别名），不是唯一的写法：`channel map <渠道> …` 与
// `channel map --all …` 走动词入口，补全补不出渠道名，所以不在这里。分派仍是
// runChannelMap 的 switch——新增动作时两处都要加。
var channelMapActions = []string{"add", "list", "remove"}

// runChannelMap 分派 channel map 的两种写法：保留词子命令，以及「动词 + 渠道」。
//
// **保留词优先，且只判第一个位置参数**（见 channelMapUsage）：第一个是 add /
// list / remove / help / -h / --help 时按子命令解释，其余一律当动词入口。
// 第二个及之后原样当名字——`channel map 42 list` 里的 list 是规范名。代价是
// 「名字恰好是保留词的渠道」只能用 ID 指代，这是已知且可接受的。
func runChannelMap(args []string) error {
	if len(args) == 0 {
		cli.PrintUsage(os.Stderr, channelMapShortUsage)
		return cli.UsageErr(errors.New("channel map 需要渠道：larkaxctl channel map <渠道> <规范名> [<上游模型名>]"))
	}

	switch action := args[0]; action {
	case "add":
		// 兼容别名：与动词写法是同一条路径。
		return runChannelMapAdd(args[1:])
	case "list":
		return runChannelMapList(args[1:])
	case "remove":
		// 兼容别名：反义动词是 channel unmap。
		return runChannelMapRemove(args[1:])
	case "help":
		cli.PrintUsage(os.Stdout, channelMapHelp.long)
		return nil
	case "-h", "--help":
		cli.PrintUsage(os.Stdout, channelMapHelp.short)
		return nil
	default:
		return runChannelMapAdd(args)
	}
}

// runChannelMapUnmap 是反义动词的入口。
func runChannelUnmap(args []string) error {
	// -h 要在摘位置参数之前收掉（见 cli 包的 helpflag.go）。
	if cli.AnswerHelpFlag(args, channelMapShortUsage) {
		return nil
	}

	if err := cli.JSONUnsupported("channel unmap"); err != nil {
		return err
	}

	channelRef, canonical, err := parseChannelMapRemove(args, "unmap")
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		return deleteChannelMap(ctx, db, channelRef, canonical)
	})
}

// runChannelMapAdd 写一条映射：渠道级同时把规范名并进渠道白名单。
//
// 它是两个入口共用的实现：动词写法（第一个位置参数是渠道）与兼容别名
// `channel map add`。
func runChannelMapAdd(args []string) error {
	// -h 要在摘位置参数之前收掉（见 cli 包的 helpflag.go）。
	if cli.AnswerHelpFlag(args, channelMapShortUsage) {
		return nil
	}

	if err := cli.JSONUnsupported("channel map"); err != nil {
		return err
	}

	req, channelRef, err := parseChannelMapAdd(args)
	if err != nil {
		return cli.UsageErr(err)
	}

	var id int64
	err = withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		created, applyErr := createChannelMap(ctx, db, &req, channelRef)
		if applyErr != nil {
			return applyErr
		}
		id = created
		return nil
	})
	if err != nil {
		return err
	}
	printCreatedChannelMap(cli.HumanStdout(), cli.HumanStderr(), id, req)
	return nil
}

// createChannelMap 在**一个事务**里写完一条映射，渠道级还要写渠道白名单。
//
// 顺序是映射在前、白名单在后：白名单写入走 channels.Admin.Update 的既有校验
// （模型在目录里声明的端点服务不了这条渠道时拒），此时映射已经在同一个事务里
// 写过了——回滚让它变成一次干净的失败。若把两处拆成各自的事务，库里就会留下
// 一条指向「渠道不服务这个模型」的映射，选路会选中一条永远打不通的路。
//
// channelRef 为空表示平台级：只写 model_aliases，不碰任何渠道的 models。
// 渠道引用到这一步才解析：它认 ID 也认名字，按名字查要连库（见
// channels.Admin.ChannelIDByRef）。
func createChannelMap(ctx context.Context, db *sql.DB, req *catalog.CreateAliasRequest, channelRef string) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启映射事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	chAdmin := channels.NewAdminInTx(tx, nil, nil)
	if channelRef != "" {
		channelID, resolveErr := chAdmin.ChannelIDByRef(ctx, channelRef)
		if resolveErr != nil {
			return 0, resolveErr
		}
		req.ChannelID = channelID
	}

	id, err := catalog.NewAdminInTx(tx).CreateAlias(ctx, *req)
	if err != nil {
		return 0, err
	}

	if req.ChannelID != 0 {
		channel, getErr := chAdmin.Get(ctx, req.ChannelID)
		if getErr != nil {
			return 0, getErr
		}
		if !slices.Contains(channel.Models, req.CanonicalName) {
			models := append(slices.Clone(channel.Models), req.CanonicalName)
			updateErr := chAdmin.Update(ctx, channels.UpdateRequest{
				ID:     req.ChannelID,
				Models: &models,
			})
			if updateErr != nil {
				return 0, updateErr
			}
		}
	}

	commitErr := tx.Commit()
	if commitErr != nil {
		return 0, fmt.Errorf("提交映射事务失败: %w", commitErr)
	}
	return id, nil
}

// parseChannelMapAdd 解析动词写法（以及兼容别名 `map add`）的位置参数与 --overrides。
//
// 位置参数排在旗标前面（与 user 那一族的位置参数命令同一个形状）：
// `--overrides` 写在名字之后。--all 是开关，前后都能写。
//
// 渠道只在这里留下一份**原文**：它认 ID 也认名字，而按名字查要连库，所以解析
// 挪到写入那一步（见 createChannelMap）。返回的 channelRef 为空表示平台级。
func parseChannelMapAdd(args []string) (catalog.CreateAliasRequest, string, error) {
	allChannels, args, err := takeAllChannelsFlag(args)
	if err != nil {
		return catalog.CreateAliasRequest{}, "", err
	}

	var channelRef, canonical, upstream string
	if allChannels {
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return catalog.CreateAliasRequest{}, "", err
		}
	} else {
		if channelRef, args, err = takeOperand(args, "<渠道>"); err != nil {
			return catalog.CreateAliasRequest{}, "", err
		}
		if checkErr := checkChannelRefShape(channelRef); checkErr != nil {
			return catalog.CreateAliasRequest{}, "", checkErr
		}
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return catalog.CreateAliasRequest{}, "", err
		}
	}
	// 上游名在渠道级可以省略：直通形态就是「这家上游对这个模型叫同一个名字」。
	// 平台级不行：不带翻译内容的映射只在 list 里多一行，不改变任何结果。
	if upstream, args = takeOptionalOperand(args); strings.TrimSpace(upstream) == "" {
		if allChannels {
			return catalog.CreateAliasRequest{}, "", errors.New(
				"带 --all 时要写 <上游模型名>：平台级映射是通用翻译，与规范名同名的一条不改变任何结果")
		}
		upstream = canonical
	}

	fs := cli.NewFlagSet("channel map", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, channelMapShortUsage) }
	overrides := fs.String("overrides", "", "")
	if parseErr := fs.Parse(args); parseErr != nil {
		return catalog.CreateAliasRequest{}, "", parseErr
	}
	if fs.NArg() > 0 {
		if allChannels {
			return catalog.CreateAliasRequest{}, "", fmt.Errorf(
				"带 --all 时只给两个位置参数：<规范名> <上游模型名>；多出来的 %q 是渠道，平台级映射不挂渠道",
				fs.Arg(0))
		}
		return catalog.CreateAliasRequest{}, "", fmt.Errorf(
			"map 不接受多余的参数，得到 %q：渠道、规范名与上游模型名依次写成位置参数",
			fs.Arg(0))
	}

	// 上游模型名进 list 的「上游模型名」那一栏：控制字符在连库之前拒掉
	// （判据在 catalog 的 normalizeUpstreamModel，与旧路径同一处）。
	canonical = strings.TrimSpace(canonical)
	upstream = strings.TrimSpace(upstream)
	if checkErr := cli.CheckDisplayText("<上游模型名>", upstream); checkErr != nil {
		return catalog.CreateAliasRequest{}, "", checkErr
	}
	parsedOverrides, err := parseJSONObject("--overrides", *overrides)
	if err != nil {
		return catalog.CreateAliasRequest{}, "", err
	}

	return catalog.CreateAliasRequest{
		ChannelID:     0,
		UpstreamModel: upstream,
		CanonicalName: canonical,
		Overrides:     parsedOverrides,
	}, channelRef, nil
}

func runChannelMapList(args []string) error {
	channelRef, canonical, err := parseChannelMapList(args)
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		// 渠道引用到这一步才解析：它认 ID 也认名字，按名字查要连库。
		var channelID int64
		if channelRef != "" {
			resolved, resolveErr := channels.NewAdmin(db, nil, nil).ChannelIDByRef(ctx, channelRef)
			if resolveErr != nil {
				return resolveErr
			}
			channelID = resolved
		}

		store := catalog.New(db)
		var aliases []catalog.Alias
		var listErr error
		if strings.TrimSpace(canonical) != "" {
			aliases, listErr = store.UpstreamModels(ctx, canonical)
		} else {
			aliases, listErr = store.Aliases(ctx, channelID)
		}
		if listErr != nil {
			return listErr
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, aliasListDoc(aliases), nil)
		}
		printAliasList(cli.HumanStdout(), aliases, "映射", "channel map")
		return nil
	})
}

// parseChannelMapList 解析 list 的范围。
//
// 三种范围互斥：平台级（不给参数或 --all）、某个渠道（<渠道>），以及一个规范名
// 的全部范围（--model）。渠道只留下原文，解析在连库之后（见 runChannelMapList）。
func parseChannelMapList(args []string) (string, string, error) {
	allChannels, args, err := takeAllChannelsFlag(args)
	if err != nil {
		return "", "", err
	}

	var channelRef string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if channelRef, args, err = takeOperand(args, "<渠道>"); err != nil {
			return "", "", err
		}
		if allChannels {
			return "", "", errors.New("<渠道> 与 --all 只能给一个：" +
				"前者列那个渠道的映射，后者列平台级映射")
		}
		if checkErr := checkChannelRefShape(channelRef); checkErr != nil {
			return "", "", checkErr
		}
	}

	fs := cli.NewFlagSet("channel map list", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, channelMapShortUsage) }
	canonical := fs.String("model", "", "")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if fs.NArg() > 0 {
		return "", "", fmt.Errorf("list 不接受多余的参数，得到 %q：渠道写成位置参数，规范名用 --model",
			fs.Arg(0))
	}

	if strings.TrimSpace(*canonical) != "" {
		if channelRef != "" || allChannels {
			return "", "", errors.New("--model 与 <渠道> / --all 只能给一个：" +
				"前者列出这个名字在所有范围下的映射")
		}
	}
	return channelRef, *canonical, nil
}

// runChannelMapRemove 是 `channel map remove` 这条兼容别名的入口。
func runChannelMapRemove(args []string) error {
	// -h 要在摘位置参数之前收掉（见 cli 包的 helpflag.go）。
	if cli.AnswerHelpFlag(args, channelMapShortUsage) {
		return nil
	}

	if err := cli.JSONUnsupported("channel map remove"); err != nil {
		return err
	}

	channelRef, canonical, err := parseChannelMapRemove(args, "remove")
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		return deleteChannelMap(ctx, db, channelRef, canonical)
	})
}

// deleteChannelMap 删掉一个范围内的映射。
//
// 渠道引用到这一步才解析（ID 或未软删渠道的名字）。只删 model_aliases 这一行：
// 渠道 models 白名单里的名字**保留**——unmap 的反义是「不再翻译」，不是「渠道
// 不再服务这个模型」；白名单由 channel update --models 管。
func deleteChannelMap(ctx context.Context, db *sql.DB, channelRef, canonical string) error {
	var channelID int64
	if channelRef != "" {
		resolved, err := channels.NewAdmin(db, nil, nil).ChannelIDByRef(ctx, channelRef)
		if err != nil {
			return err
		}
		channelID = resolved
	}

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
	_, _ = fmt.Fprintf(cli.HumanStderr(),
		"已删除映射 id=%d：%s 规范名 %s（上游名 %s）。白名单里的名字保留。\n",
		found.ID, aliasScope(channelID), found.CanonicalName, found.UpstreamModel)
	return nil
}

// parseChannelMapRemove 解析删除一个映射的位置参数与 --all。
//
// verb 只影响报错里出现的命令名：`remove` 是兼容别名，`unmap` 是动词。
// 渠道只留下原文，解析在连库之后。
func parseChannelMapRemove(args []string, verb string) (string, string, error) {
	allChannels, args, err := takeAllChannelsFlag(args)
	if err != nil {
		return "", "", err
	}

	var channelRef, canonical string
	if allChannels {
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return "", "", err
		}
	} else {
		if channelRef, args, err = takeOperand(args, "<渠道>"); err != nil {
			return "", "", err
		}
		if checkErr := checkChannelRefShape(channelRef); checkErr != nil {
			return "", "", checkErr
		}
		if canonical, args, err = takeOperand(args, "<规范名>"); err != nil {
			return "", "", err
		}
	}
	if len(args) > 0 {
		return "", "", fmt.Errorf("%s 不接受多余的参数，得到 %q", verb, args[0])
	}
	return channelRef, strings.TrimSpace(canonical), nil
}

// takeAllChannelsFlag 从参数里摘出 --all / --all-channels。
//
// 它是开关、不带取值，所以与位置参数没有先后关系：取位置参数之前先摘掉它，
// `map --all <规范名> <上游模型名>` 与 `map <规范名> <上游模型名> --all` 都认。
func takeAllChannelsFlag(args []string) (bool, []string, error) {
	all := false
	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "--all-channels" || arg == "-all-channels" || arg == "--all" || arg == "-all":
			all = true
		case strings.HasPrefix(arg, "--all-channels=") || strings.HasPrefix(arg, "-all-channels=") ||
			strings.HasPrefix(arg, "--all=") || strings.HasPrefix(arg, "-all="):
			return false, nil, errors.New("--all 是开关，不带取值：写成 --all（--all-channels 是它的全写）")
		default:
			out = append(out, arg)
		}
	}
	return all, out, nil
}

// checkChannelRefShape 在解析层拒掉不像 ID 又不可能是名字前导的写法。
//
// 名字这一档要连库才认得出（见 channels.Admin.ChannelIDByRef），但纯数字且不是
// 正整数的引用在这一层就知道它不可能是任何一条渠道：0 与负数都不行。等连库再报
// 会把一个参数错拖成一次数据库往返，退出码也从头到尾不同（参数错 2，约束冲突 4）。
// 名字恰好是数字时按 ID 解释，与 ChannelIDByRef 同一条次序。
//
// 文案要带上「也可以写渠道名」：这一层只拦数字形态，别让人以为名字不被接受。
func checkChannelRefShape(ref string) error {
	id, parseErr := strconv.ParseInt(strings.TrimSpace(ref), 10, 64)
	if parseErr != nil || id > 0 {
		return nil
	}
	return fmt.Errorf("<渠道> %q 无效：渠道 ID 从 1 开始（也可以写渠道名）", ref)
}

// takeOptionalOperand 取一个可有可无的位置参数。
//
// 与 takeOperand 的区别只有「缺了不报错」：缺了返回空串、参数原样交回，由
// 调用方决定缺省值（map 的上游名缺省成规范名）。以 - 开头的一律当旗标，不吞。
func takeOptionalOperand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
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
