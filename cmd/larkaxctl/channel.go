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
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/upstream"
)

// channelHelp 是 channel 的两版帮助文本（见 helpText）。
var channelHelp = helpText{short: channelShortUsage, long: channelUsage}

// channelShortUsage 是 channel 的短版：`-h` / `--help` 与参数写错时打的那一份。
const channelShortUsage = `larkaxctl channel 管理上游渠道。
用法：
  larkaxctl channel create --name <名称> --base-url <URL> --models <模型名,...>
                           (--type <类型> | --protocols <协议名,...>)
                           [--credential-stdin | --credential-env <变量名>]
  larkaxctl channel list [--all]
  larkaxctl channel get --id <渠道 ID>
  larkaxctl channel health [--id <渠道 ID>] [--since <时长或时刻>] [--all]
  larkaxctl channel update --id <渠道 ID> [字段与 create 相同，另可换凭证]
  larkaxctl channel enable|disable|delete|restore --id <渠道 ID>
  larkaxctl channel sync-models|upstream-models --id <渠道 ID>
  larkaxctl channel adapters
  larkaxctl channel probe --type <类型> --base-url <URL>
                          [--credential-stdin | --credential-env <变量名>]
  larkaxctl channel map add|list|remove（出站映射，见 larkaxctl channel map help）
明文凭证只能从标准输入或环境变量读，不能写在命令行参数里。
create 的 stdout 只有新渠道的 ID；delete 是软删，restore 是它的逆操作。
更细的边界：larkaxctl help channel。
`

// channelUsage 是 channel 的长版：`larkaxctl help channel` 与 `channel help`。
const channelUsage = `larkaxctl channel 管理上游渠道。

用法：
  larkaxctl channel create --name <名称> --base-url <URL> --models <模型名,...>
                           (--type <类型> | --protocols <协议名,...>)
                           [--credential-stdin | --credential-env <变量名>]
                           [--user <用户 ID 或用户名>] [--weight <权重>]
                           [--priority <优先级>] [--setting <JSON 对象>]
  larkaxctl channel list [--all]
  larkaxctl channel get --id <渠道 ID>
  larkaxctl channel health [--id <渠道 ID>] [--since <时长或时刻>] [--all]
  larkaxctl channel update --id <渠道 ID>
                           [--name <名称>] [--type <类型>]
                           [--protocols <协议名,...>] [--base-url <URL>]
                           [--models <模型名,...>] [--weight <权重>]
                           [--priority <优先级>] [--setting <JSON 对象>]
                           [--credential-stdin | --credential-env <变量名>]
  larkaxctl channel enable --id <渠道 ID>
  larkaxctl channel disable --id <渠道 ID>
  larkaxctl channel delete --id <渠道 ID>
  larkaxctl channel restore --id <渠道 ID>
  larkaxctl channel sync-models --id <渠道 ID>
  larkaxctl channel upstream-models --id <渠道 ID>
  larkaxctl channel adapters
  larkaxctl channel probe --type <类型> --base-url <URL>
                          [--credential-stdin | --credential-env <变量名>]
  larkaxctl channel map add <渠道> <规范名> <上游模型名> [--overrides <JSON 对象>]
  larkaxctl channel map add --all-channels <规范名> <上游模型名>
  larkaxctl channel map list [<渠道>] [--all-channels] [--model <规范名>]
  larkaxctl channel map remove <渠道> <规范名>
  larkaxctl channel map remove --all-channels <规范名>

**明文凭证只能从标准输入或环境变量读，不能写在命令行参数里。**命令行参数会
出现在 ps 的输出与 shell 历史里。

  --credential-stdin        从标准输入读到文件结束（末尾的换行会被去掉）
  --credential-env <变量名> 从名为 <变量名> 的环境变量读

**create 不给凭证是合法的**：渠道建出来但一个账号都不挂（零账号），
channel list 看得见它，选路会跳过。之后用 account add --channel <渠道>
接上，或者 channel update --credential-* 直接补一把。给了凭证时 create 会
建一个匿名账号并挂上——账号是凭证的单位（见 larkaxctl help account）。

例（凭证从标准输入读）：

  cat secret.txt | larkaxctl channel create --name c1 --type openai \
      --base-url https://api.example.com/v1 --models <规范模型名> --credential-stdin

例（凭证从环境变量读）：

  LARKAX_CHANNEL_CREDENTIAL=<CHANNEL_CREDENTIAL> larkaxctl channel create ... \
      --credential-env LARKAX_CHANNEL_CREDENTIAL

create 的 stdout 只有新渠道的 ID，其余信息走 stderr。
list **不打印凭证密文**，只打印它挂在哪些主密钥下（那不需要任何密钥材料）。
list 默认不列已删的渠道，--all 才列，并在「已删」那一栏标出删除时刻。

get 打一条渠道的全部配置，一栏一行。它与 list 的分工是形状上的：list 一张表，
get 一条一行。**已软删的渠道 get 也打得出来**，并在「已删」那一栏标出它——行还
在库里，单条读取的目的是核对配置。

health 打渠道的健康。health 那一栏是 channel_quota_state.health 的取值
（healthy / degraded / drained / unknown），请求数、失败数、失败率、p50、p95、
成本与缓存命中 / 未命中（以 token 计，口径与选路读的那两列相同）来自
channel_health_rollup 在窗口里的那些小时桶。窗口由 --since 给：

  --since 24h                     从 24 小时前到现在（默认）
  --since 90m                     从 90 分钟前到现在
  --since 2026-10-17T00:00:00Z    从这个时刻到现在

默认打一张按渠道汇总的表：上面那些列都在，**成功数只在 --id 的详情里打**。
带 --id 时改打那一条渠道的详情，并把它在这个窗口里的**逐小时**明细一起列出来。
默认不列已软删的渠道，--all 才带上；带 --id 时那一条已删的也照打
（--all 只影响清单要不要变长）。

分位数每一列各取窗口内**自己**最近一个有值的桶，不是整个窗口的分位数——分位数
不可累加，p50 与 p95 因此可能来自不同的小时。汇总行里的请求数是整个窗口的合计，
要逐小时看就用 --id。

成本那一列是窗口内各小时桶的上游成本合计（我们付给上游多少，不是售价）。
- 表示这一列没有可用的数：窗口里没有一条带成本的事件，或者合计恰好为 0——
别把它读成「免费」。

缓存命中 / 未命中也是窗口内各小时之和（未命中算的是 prompt + 缓存写入）。
0 表示这段时间一个缓存 token 都没有：那两列与成本不同，它们有真的写入方，
所以 0 是正常读数而不是「没人记过账」。

delete 是**软删**：置 deleted_at 并把状态改成 disabled，行与它的凭证密文都留在
库里。历史账单（billing_requests / usage_events）仍然按 channel_id 引用它。
已删的渠道不能再 update / enable / disable，再删一次不报错（打印「已经是删除
状态」）。

update 的 --credential-* 改的是这条渠道**池里 id 最小的主账号**（选路读的那
一个）；账号是凭证的单位，同一账号若还挂在别的渠道上，改一次那些渠道一起生效
（见 larkaxctl help account）。零账号的渠道走这条路会补一个匿名账号。

restore 是 delete 的逆操作：清掉 deleted_at、把状态置回 enabled，并把这条渠道的
健康结论置回 unknown——它上一次的结论是**软删之前**的，不能直接沿用，恢复之后
报 unknown 的意思是「现在好不好还没人说得出来」。没有处于删除状态的渠道再
restore 一次不报错（打印「没有处于删除状态」）。

sync-models 从上游拉一次这条渠道的模型清单并落库，需要主密钥（要解出凭证）。
它拉的是**上游自己的模型名**，存在单独的表里，不写进 channels.models——那一栏
是选路用的规范名，两者之间隔着出站映射（channel map）。upstream-models 只读库里那一份，不碰
网络、不要主密钥。

上游不提供模型列表接口是常态：适配器回「不支持」时 sync-models 照样成功返回
（退出码 0），只把状态记成 unsupported，**已有清单一行不动**。同一个渠道连续
同步两次是幂等的：模型名按渠道唯一，第二次不会多出行，只把每行的「最近见到」
刷新到这一次；上游下线了某个模型时，再同步一次会把它从清单里删掉。

adapters 打印已注册的适配器类型，以及每种类型对应的协议与端点。它与控制台的
GET /admin/v1/adapters 读同一份注册表：--type 认得哪些类型，这里就列哪些。

probe 在落库之前向上游问一次模型清单：给 --type 与 --base-url，凭证可选
（无鉴权的自建上游不传）。它不连库、不需要主密钥，凭证只能从标准输入或环境
变量读。拿到清单时 stdout 第一行是条数，下面一行一个模型名（升序、不编号）；
清单超过上限时只列前一部分，首行写明一共有多少个。

上游不提供模型列表接口时 probe 退出 0，只打一句「这家上游（<类型>）不提供
模型列表」，不打空清单——「没有这个接口」与「清单是空的」不是同一件事。
探测失败时退出非零，错误里带上游的状态码与响应体摘要，摘要里的凭证已换成
<redacted>。

渠道类型必须是已注册的适配器类型；类型写错时错误信息会列出已知的类型。
--protocols 是这条渠道声明支持的协议集合，至少一个，取值同样是已注册的适配器
类型；写错一个名字时错误信息会列出已知的协议。一条渠道一套凭证：声明两套协议
表示同一个上游地址、同一把凭证上两种端点都能用，请求按它走的那条入口分派到
对应协议。

--type 等价于只写一个元素的 --protocols，两者都还是单协议的写法。同时给时
--type 必须在 --protocols 里（它就是主协议，落在渠道的类型上）；不在里面会报错。
只给 --protocols 时主协议取第一个。update 给 --type 会把协议集合改成只含它
一个——要扩成多协议请给 --protocols。

**单协议渠道的行为与加 --protocols 之前逐字相同**：请求照旧按类型分派，
channel get 也不多一栏（协议集合暂时不出现在 get / list 的输出里）。

--models 至少要有一个：读取路径把 models 当白名单用，空数组的渠道永远选不中。
每个模型还要能被这条渠道服务：模型目录里可以声明一个模型只在某几条端点上被
服务，而渠道声明的协议决定它把请求发到哪几条。一条都对不上时 create / update
会被拒绝，并说出「模型在目录里声明的端点」与「这条渠道打的端点」各是什么；
多协议渠道只要有其中一条对得上就放行。目录里没有这个模型、或者那一项没配过时
不拦。
--setting 是渠道私有配置（自由形态的 JSON 对象），键由该渠道 type 指向的
适配器解释，选路层不认，管理面也不校验键名。今天有两个键有读者。

reasoning_effort_map 由 openai 与 openai-responses 两个适配器读：它把
reasoning_effort 的取值翻成该渠道认的写法（映射到空字符串表示把这个字段从
出站报文里去掉，表里没有的取值照原样发）。例如只认 off 的上游这样写：
  {"reasoning_effort_map":{"none":"off"}}
要剔掉这个字段就写成 {"reasoning_effort_map":{"none":""}}。

usage 由 openai、anthropic 与 openai-responses 三个适配器读。它的
completion_includes_reasoning 这一位决定上游报的输出 token 里含不含推理：
不写时按「含」算——OpenAI 的 completion_tokens 与 Anthropic 的 output_tokens
都这样；只有把可见输出与推理分列报出的上游要显式写成
  {"usage":{"completion_includes_reasoning":false}}
不写的话，计费前减掉推理会让输出那一侧少收；推理比可见输出多时，整段输出
都收不到钱。两种口径靠数字分不出来，所以按渠道声明。

三个适配器都读它，但只有把推理分列报出的上游需要配：anthropic 渠道上这一位
没有数值效果——Claude 的用量不单列推理，ReasoningTokens 恒为 0，两种取值
算出的金额相同。

内层键名与顶层键一样会点名，写法是点号连起来：usage 里的键写成
usage.completion_includes_reasoning。这一位在钱路上——少一个 s 就等于没写，
会静默按「含推理」处理。反过来，在本来就含推理的口径上写 false，同一批推理
会被收两次：输出整段按输出价算一次，推理那一项再加一次。

写进去的键这条渠道的 type 不认时（拼错的、别的 type 的），照样不报错、
不挡写入，但 create，以及给了 --setting 或 --type 的 update，成功之后会逐个
点名打到 stderr 上，channel list 也把这样的渠道点出来。

**选项的旧名仍可用**：--owner 等同于 --user。它保留是为了不让已经在跑的脚本
与部署流程坏掉，行为一致；新写的命令请用 --user。

渠道上的出站映射（规范名 → 这家上游的叫法）在 channel map 下：add / list /
remove，平台级映射用 --all-channels 表示。完整边界见 larkaxctl channel map help。

# 别名

下面几个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -u <用户 ID 或用户名>     同 --user
  --stdin                   同 --credential-stdin
  --env <变量名>            同 --credential-env。它与全局的 --env-file 只差一个
                            词，含义却不同：**这里是环境变量的名字**（从那个
                            变量里读凭证），--env-file 要的是 .env 文件的**路径**。
  --owner                   同 --user

**--channel 不存在、--type / --base-url / --model… 也都不开简写**：这条命令上
挑错的是「凭证写给哪条渠道」，而 -c 这类单字母在别的命令行里既可能是
channel 也可能是 config。纯长参数那一条有用例钉着。

更细的约定见 docs/cli-reference.md 的「channel」一节。
`

// channelTimeout 是渠道命令连库与写库的时间上限。
//
// 与 apiKeyTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。
const channelTimeout = 30 * time.Second

// credentialInput 是读明文凭证的来源。
//
// 它是一个变量而不是直接写 os.Stdin，是为了让「凭证从标准输入读」这条路径
// 在测试里能走通——那条路径本身就是这条命令的一条契约，不能只靠人工敲一次。
var credentialInput io.Reader = os.Stdin

// channelActions 是 channel 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var channelActions = []string{"create", "list", "get", "health", "update", "enable", "disable", "delete", "restore", "sync-models", "upstream-models", "adapters", "probe", "map"}

func runChannel(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, channelShortUsage)
		return usageErr(errors.New("channel 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create":
		return runChannelCreate(rest)
	case "list":
		return runChannelList(rest)
	case "get":
		return runChannelGet(rest)
	case "health":
		return runChannelHealth(rest)
	case "update":
		return runChannelUpdate(rest)
	case "enable":
		return runChannelSetStatus(rest, channels.StatusEnabled)
	case "disable":
		return runChannelSetStatus(rest, channels.StatusDisabled)
	case "delete":
		return runChannelDelete(rest)
	case "restore":
		return runChannelRestore(rest)
	case "sync-models":
		return runChannelSyncModels(rest)
	case "upstream-models":
		return runChannelUpstreamModels(rest)
	case "adapters":
		return runChannelAdapters(rest)
	case "probe":
		return runChannelProbe(rest)
	case "map":
		return runChannelMap(rest)
	case "-h", "--help":
		printUsage(os.Stdout, channelShortUsage)
		return nil
	default:
		printUsage(os.Stderr, channelShortUsage)
		return usageErr(fmt.Errorf("未知的 channel 子命令 %q", action))
	}
}

func runChannelCreate(args []string) error {
	if err := jsonUnsupported("channel create"); err != nil {
		return err
	}

	req, ownerRef, err := parseChannelCreate(args)
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

	// 给了凭证才需要主密钥：零账号渠道不封装任何东西，一台没有
	// LARKAX_MASTER_KEYS 的机器也该建得出来（见 CreateRequest.Credential）。
	registry, err := upstream.Registry()
	if err != nil {
		return fmt.Errorf("装配渠道适配器注册表失败: %w", err)
	}
	var sealer *credential.Sealer
	if req.Credential != "" {
		sealer, err = masterKeySealer()
		if err != nil {
			return err
		}
	}
	admin := channels.NewAdmin(db, sealer, registry)

	if ownerRef != "" {
		ownerID, resolveErr := resolveOwnerID(ctx, db, ownerRef)
		if resolveErr != nil {
			return resolveErr
		}
		req.OwnerUserID = ownerID
	}

	id, err := admin.Create(ctx, req)
	if err != nil {
		return err
	}
	// setting 的键有没有读者，按**主协议**判：只给 --protocols 时它是列表的
	// 第一个（写入侧同一套规则，见 channels.protocolSetForCreate）。
	primaryType := req.Type
	if primaryType == "" && len(req.Protocols) > 0 {
		primaryType = req.Protocols[0]
	}
	primaryKeyID := ""
	if sealer != nil {
		primaryKeyID = sealer.PrimaryKeyID()
	}
	printCreatedChannel(humanStdout(), humanStderr(), id, primaryKeyID,
		channel.SettingUnreadNote(primaryType, req.Setting))
	return nil
}

// parseChannelCreate 解析 create 的参数。
//
// 它多带一个 ownerRef 返回值：--user 要连库才能解析成用户 ID，而参数解析
// 必须在连库之前完成。所以这里只做「有没有给、给了什么」，解析留给调用方。
func parseChannelCreate(args []string) (channels.CreateRequest, string, error) {
	fs := newFlagSet("channel create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	name := fs.String("name", "", "")
	channelType := fs.String("type", "", "")
	protocols := fs.String("protocols", "", "")
	baseURL := fs.String("base-url", "", "")
	models := fs.String("models", "", "")
	owner := legacyStringFlag(fs, "user", "owner", "")
	stringAlias(fs, owner, "u")
	weight := fs.Int("weight", 0, "")
	priority := fs.Int("priority", 0, "")
	setting := fs.String("setting", "", "")
	credentialStdin := fs.Bool("credential-stdin", false, "")
	boolAlias(fs, credentialStdin, "stdin")
	credentialEnv := fs.String("credential-env", "", "")
	stringAlias(fs, credentialEnv, "env")
	if err := fs.Parse(args); err != nil {
		return channels.CreateRequest{}, "", err
	}
	if fs.NArg() > 0 {
		return channels.CreateRequest{}, "", fmt.Errorf(
			"create 不接受位置参数，得到 %q：各字段用 --<名称> 指定", fs.Arg(0))
	}

	// 参数检查全部发生在连库之前（与 user / apikey 同一条约定）：
	// 一个写错的命令不该先连上生产库再报错，而「连上库之后才发现参数不对」
	// 还有一个更坏的可能——它没报错。
	if strings.TrimSpace(*name) == "" {
		return channels.CreateRequest{}, "", errors.New("缺少 --name")
	}
	// 名称与模型名单会进 channel list / health 的表格：控制字符在连库之前
	// 就拒掉（判据在 channels 的写入路径，见 normalizeName / normalizeModels）。
	if err := checkDisplayText("--name（渠道名）", strings.TrimSpace(*name)); err != nil {
		return channels.CreateRequest{}, "", err
	}
	// --type 与 --protocols 至少给一个：前者是单协议的旧写法，后者是新写法，
	// 两者都等于「这条渠道声明了哪些协议」（见 channels.CreateRequest）。
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var protocolList []string
	if given["protocols"] {
		if strings.TrimSpace(*protocols) == "" {
			return channels.CreateRequest{}, "", errors.New("--protocols 里没有协议名（至少一个）")
		}
		protocolList = splitList(*protocols)
	}
	if strings.TrimSpace(*channelType) == "" && len(protocolList) == 0 {
		return channels.CreateRequest{}, "", errors.New("缺少 --type 或 --protocols（至少给一个）")
	}
	if strings.TrimSpace(*baseURL) == "" {
		return channels.CreateRequest{}, "", errors.New("缺少 --base-url")
	}
	// 地址的写法在这里先过一遍（下面 update 那条路同理）：判据与写入路径共用
	// 同一个实现，区别只在**什么时候**报——写错地址是参数错，该走退出码 2、
	// 且不该为了知道地址写错了先连一次库。
	normalizedBaseURL, err := channels.NormalizeBaseURL(*baseURL)
	if err != nil {
		return channels.CreateRequest{}, "", err
	}
	if strings.TrimSpace(*models) == "" {
		return channels.CreateRequest{}, "", errors.New("缺少 --models（至少一个规范模型名）")
	}
	for i, model := range splitList(*models) {
		if shapeErr := checkDisplayText(
			fmt.Sprintf("--models 里第 %d 个模型名", i+1), strings.TrimSpace(model)); shapeErr != nil {
			return channels.CreateRequest{}, "", shapeErr
		}
	}
	// 凭证是可选的：不给就建一条零账号渠道（合法中间态，见
	// channels.CreateRequest.Credential）。给了才建匿名账号并挂上。
	plaintext, err := readCredential(*credentialStdin, *credentialEnv, false)
	if err != nil {
		return channels.CreateRequest{}, "", err
	}
	credentialValue := ""
	if plaintext != nil {
		credentialValue = *plaintext
	}
	parsedSetting, err := parseJSONObject("--setting", *setting)
	if err != nil {
		return channels.CreateRequest{}, "", err
	}

	return channels.CreateRequest{
		Name:       *name,
		Type:       *channelType,
		Protocols:  protocolList,
		BaseURL:    normalizedBaseURL,
		Credential: credentialValue,
		Models:     splitList(*models),
		Weight:     *weight,
		Priority:   *priority,
		Setting:    parsedSetting,
	}, *owner, nil
}

// parseChannelUpdate 解析 update 的参数。
//
// 没有 --user：归属（平台自营 / 某个商家）不在写入路径能改的字段里，
// 见 channels.UpdateRequest。要换归属是另一件事，不该混在一次「改配置」里。
func parseChannelUpdate(args []string) (channels.UpdateRequest, error) {
	fs := newFlagSet("channel update", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	id := fs.Int64("id", 0, "要改的渠道 ID")
	name := fs.String("name", "", "")
	channelType := fs.String("type", "", "")
	protocols := fs.String("protocols", "", "")
	baseURL := fs.String("base-url", "", "")
	models := fs.String("models", "", "")
	weight := fs.Int("weight", 0, "")
	priority := fs.Int("priority", 0, "")
	setting := fs.String("setting", "", "")
	credentialStdin := fs.Bool("credential-stdin", false, "")
	boolAlias(fs, credentialStdin, "stdin")
	credentialEnv := fs.String("credential-env", "", "")
	stringAlias(fs, credentialEnv, "env")
	if err := fs.Parse(args); err != nil {
		return channels.UpdateRequest{}, err
	}
	if fs.NArg() > 0 {
		return channels.UpdateRequest{}, fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的渠道用 --id 指定", fs.Arg(0))
	}
	if *id <= 0 {
		return channels.UpdateRequest{}, errors.New("缺少 --id（要改的渠道 ID）")
	}

	// 只有**真的给了**的标志才进请求。--priority 0 与「没提这一项」是两件事，
	// 而 flag 包的零值分不出它们，所以这里看 fs.Visit 记下的清单。
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	req := channels.UpdateRequest{ID: *id}
	changes := 0
	if given["name"] {
		if err := checkDisplayText("--name（渠道名）", strings.TrimSpace(*name)); err != nil {
			return channels.UpdateRequest{}, err
		}
		req.Name = name
		changes++
	}
	if given["type"] {
		req.Type = channelType
		changes++
	}
	if given["protocols"] {
		if strings.TrimSpace(*protocols) == "" {
			return channels.UpdateRequest{}, errors.New("--protocols 里没有协议名（至少一个）")
		}
		list := splitList(*protocols)
		req.Protocols = &list
		changes++
	}
	if given["base-url"] {
		// 与 create 同一条：地址的写法在连库之前判，判据与写入路径共用
		// channels.NormalizeBaseURL。
		normalized, err := channels.NormalizeBaseURL(*baseURL)
		if err != nil {
			return channels.UpdateRequest{}, err
		}
		req.BaseURL = &normalized
		changes++
	}
	if given["models"] {
		list := splitList(*models)
		for i, model := range list {
			if err := checkDisplayText(
				fmt.Sprintf("--models 里第 %d 个模型名", i+1), strings.TrimSpace(model)); err != nil {
				return channels.UpdateRequest{}, err
			}
		}
		req.Models = &list
		changes++
	}
	if given["weight"] {
		req.Weight = weight
		changes++
	}
	if given["priority"] {
		req.Priority = priority
		changes++
	}
	if given["setting"] {
		parsed, err := parseJSONObject("--setting", *setting)
		if err != nil {
			return channels.UpdateRequest{}, err
		}
		req.Setting = &parsed
		changes++
	}
	// 判据要连别名一起看（见 flagGiven）：写 `--env FOO` 时 Visit 记的是
	// "env"，只查规范名的表现是那次轮换被静默跳过——命令报成功，
	// 而库里那一列还是旧凭证。
	if flagGiven(given, credentialStdinFlagNames...) || flagGiven(given, credentialEnvFlagNames...) {
		plaintext, err := readCredential(*credentialStdin, *credentialEnv, true)
		if err != nil {
			return channels.UpdateRequest{}, err
		}
		req.Credential = plaintext
		changes++
	}
	if changes == 0 {
		return channels.UpdateRequest{}, errors.New(
			"update 至少要改一项：给出 --name / --type / --protocols / --base-url / " +
				"--models / --weight / --priority / --setting / --credential-* 之一")
	}

	return req, nil
}

func runChannelUpdate(args []string) error {
	if err := jsonUnsupported("channel update"); err != nil {
		return err
	}

	req, err := parseChannelUpdate(args)
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

	admin, _, err := channelWriteAdmin(db)
	if err != nil {
		return err
	}
	if err := admin.Update(ctx, req); err != nil {
		return err
	}
	// 写成功的回显走 stderr：stdout 只放能被下游脚本接走的值，而这条命令要改的
	// 那一条是调用方自己用 --id 指定的，没有一个新值要给出去
	// （见 docs/cli-output.md 的「提示与警告」）。
	_, _ = fmt.Fprintf(humanStderr(), "已更新渠道 %d。\n", req.ID)
	// 提示按**改完之后**的 type 与 setting 算：只改 --type 时，库里原有的
	// 键可能对新类型没有读者；只改 --setting 时，类型取自库里那一行。
	// 两样都没碰的 update 不打——那条渠道的 setting 有没有读者，`channel
	// list` 一直在说，每次改名都重复一遍只会把提示变成背景音。
	if req.Setting != nil || req.Type != nil {
		info, getErr := admin.Get(ctx, req.ID)
		if getErr != nil {
			// 更新已经落库了：这里回滚不了，也不该把这次调用报成「没改成功」。
			// 但「改成功了，却读不回来」本身是要人看的事实，所以照样给错误，
			// 只是明说前半句已经完成。
			return fmt.Errorf("渠道 %d 已更新，但读取更新后的配置失败: %w", req.ID, getErr)
		}
		printSettingUnreadNote(humanStderr(), info.Type, info.Setting)
	}
	return nil
}

// printSettingUnreadNote 说出 setting 里哪些键对该渠道类型没有读者。
//
// 打在写入**成功之后**：命令确实写进去了，所以不能报成错误；而「写进去
// 了、但没有人按它做判断」正是使用者当场该看见的事实——只写在文档里
// 的话没人会读，而它在库里、在清单上看起来都与一个生效的配置一模一样
// （见 #360）。
//
// 走 stderr，与 create 的其余信息同一处：stdout 只放能被下一段脚本接走的值。
// create 与 update 都没有新值要给出去（create 那个 ID 走 stdout 单独一行），
// 所以两条命令的「已…」都只在 stderr 上出现一次。
//
// 判据与措辞都取自 channel.SettingUnreadNote，不在这里另写一份：哪些键有
// 读者由渠道 type 决定，那份对应关系属于 channel 包，而两处各写一句的话，
// 一处改了一处不动，于是其中一处开始说谎。
func printSettingUnreadNote(w io.Writer, channelType string, setting map[string]any) {
	note := channel.SettingUnreadNote(channelType, setting)
	if note == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "注意：%s。\n", note)
}

func runChannelList(args []string) error {
	fs := newFlagSet("channel list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
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

	// 列举既不封装凭证也不校验渠道类型，所以既不需要主密钥也不需要适配器
	// 注册表：一台只读的机器（比如没有主密钥的运维跳板）也能核对配置。
	admin := channels.NewAdmin(db, nil, nil)
	var infos []channels.ChannelInfo
	if *all {
		infos, err = admin.ListAll(ctx)
	} else {
		infos, err = admin.List(ctx)
	}
	if err != nil {
		return err
	}
	if jsonOutput() {
		return emitJSON(os.Stdout, channelListDoc(infos, *all), nil)
	}
	printChannelList(humanStdout(), infos, *all)
	return nil
}

func runChannelSetStatus(args []string, status string) error {
	verb, done := "enable", "已启用"
	if status == channels.StatusDisabled {
		verb, done = "disable", "已停用"
	}
	// 命令名按 status 算出来：enable 与 disable 共用这一个入口，写死一个
	// 会让另一条命令在不支持 json 时报出兄弟命令的名字。
	if err := jsonUnsupported("channel " + verb); err != nil {
		return err
	}

	fs := newFlagSet("channel "+verb, flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	id := fs.Int64("id", 0, "渠道 ID")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("%s 不接受位置参数，得到 %q：渠道用 --id 指定", verb, fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id（渠道 ID）"))
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

	// 启停只写 status 那一列，不碰凭证，所以同样不需要主密钥。
	if err := channels.NewAdmin(db, nil, nil).SetStatus(ctx, *id, status); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(humanStderr(), "渠道 %d %s。\n", *id, done)
	return nil
}

// runChannelDelete 软删一条渠道。
//
// 它不需要主密钥也不需要适配器注册表：删除只改 deleted_at 与 status 两列，
// 不碰凭证密文（密文留着是为了让历史行仍能解释自己）。
//
// 两条输出口径：
//   - 这次真的删掉了 → 「已删除渠道 N。」；
//   - 它已经是删除状态 → 「渠道 N 已经是删除状态。」，退出码 0。
//
// 第二条不是错误：删除是幂等的，脚本重跑一次不该失败。而 id 不存在仍然报错
// （ErrChannelNotFound）——那说明调用方拿错了 id，报成功会让它以为删对了。
func runChannelDelete(args []string) error {
	if err := jsonUnsupported("channel delete"); err != nil {
		return err
	}

	fs := newFlagSet("channel delete", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	id := fs.Int64("id", 0, "要删的渠道 ID")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("delete 不接受位置参数，得到 %q：要删的渠道用 --id 指定", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id（要删的渠道 ID）"))
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

	deleted, err := channels.NewAdmin(db, nil, nil).SoftDelete(ctx, *id)
	if err != nil {
		return err
	}
	if deleted {
		_, _ = fmt.Fprintf(humanStderr(), "已删除渠道 %d。\n", *id)
		return nil
	}
	_, _ = fmt.Fprintf(humanStderr(), "渠道 %d 已经是删除状态。\n", *id)
	return nil
}

// runChannelGet 打一条渠道的全部配置。
//
// 它与 list 的差别只在形状：list 一张表，get 一栏一行。**已软删的渠道也打得出来**
// 并由那一栏标出——行还在库里，单条读取的目的是核对配置，藏起来反而让人打不开。
func runChannelGet(args []string) error {
	fs := newFlagSet("channel get", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	id := fs.Int64("id", 0, "要看的渠道 ID")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("get 不接受位置参数，得到 %q：要看哪条渠道用 --id 指定", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id（要看的渠道 ID）"))
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

	// 与 list 同一条：单条详情既不封装凭证也不校验渠道类型，所以不需要主密钥，
	// 也不需要适配器注册表。
	info, err := channels.NewAdmin(db, nil, nil).Get(ctx, *id)
	if err != nil {
		return err
	}
	if jsonOutput() {
		return emitJSON(os.Stdout, channelGetDoc(info), nil)
	}
	printChannelDetail(humanStdout(), info)
	return nil
}

// channelHealthWindow 是 health 的默认窗口。
//
// 它取 24 小时，与选路那个窗口（internal/channels 的 healthWindow）是同一个数，
// 但**不是同一个判据**：那一个决定「这条渠道最近够不够好、还选不选它」，这一个
// 决定「这张汇总表默认看多久」。两处各取各的，改一处不会悄悄改另一处的含义。
const channelHealthWindow = 24 * time.Hour

// parseSince 把 --since 的取值翻成窗口起点。
//
// 两种写法：一段时长（`24h`、`90m`，Go 的 time.ParseDuration 那一套），相对此刻；
// 或一个 RFC3339 时刻。空串是默认窗口。
//
// 时长必须为正，时刻不得晚于此刻：窗口的终点是现在，起点跑到终点之后就不再是一个
// 窗口了。放它过去的表现是一张全是 0 的表，而那与「这段时间它没被调用过」在输出上
// 一模一样——两者要处置的事情完全不同。
func parseSince(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Now().Add(-channelHealthWindow), nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--since 的时长必须为正，得到 %q", raw)
		}
		return time.Now().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"--since 既不是一段时长（如 24h、90m）也不是一个 RFC3339 时刻（如 2026-10-17T00:00:00Z）：%q",
			raw)
	}
	if t.After(time.Now()) {
		return time.Time{}, fmt.Errorf(
			"--since 给的时刻 %s 在将来：窗口的终点是现在", raw)
	}
	return t, nil
}

// runChannelHealth 打窗口内的渠道健康。
func runChannelHealth(args []string) error {
	fs := newFlagSet("channel health", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	id := fs.Int64("id", 0, "只看这一条渠道（渠道 ID）")
	since := fs.String("since", "", "窗口起点：一段时长或一个 RFC3339 时刻")
	all := fs.Bool("all", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("health 不接受位置参数，得到 %q：要看哪一条渠道用 --id 指定", fs.Arg(0)))
	}
	if *id < 0 {
		return usageErr(errors.New("--id 必须是正数（渠道 ID）"))
	}
	windowStart, err := parseSince(*since)
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

	// 与 list 同一条：健康读数既不封装凭证也不解密，所以不需要主密钥。
	admin := channels.NewAdmin(db, nil, nil)

	if *id > 0 {
		one, buckets, healthErr := admin.HealthOne(ctx, *id, windowStart)
		if healthErr != nil {
			return healthErr
		}
		if jsonOutput() {
			return emitJSON(os.Stdout,
				channelHealthDoc([]channels.ChannelHealth{one}, windowStart, false, buckets, true), nil)
		}
		printChannelHealthDetail(humanStdout(), one)
		printChannelHealthHours(humanStdout(), buckets)
		return nil
	}

	rows, err := admin.Health(ctx, windowStart, *all)
	if err != nil {
		return err
	}
	if jsonOutput() {
		return emitJSON(os.Stdout, channelHealthDoc(rows, windowStart, *all, nil, false), nil)
	}
	printChannelHealthList(humanStdout(), rows, *all)
	return nil
}

// runChannelRestore 把一条软删的渠道恢复回可用。
//
// 它**不需要主密钥**：恢复只清 deleted_at、把状态置回 enabled、把健康结论置回
// unknown，一处都不碰凭证密文。这与 delete / enable / disable 是同一条判据
// （见 internal/channels 的 Restore），而恢复正是 delete 的逆操作，所以三者的
// 环境要求也就该一样。
func runChannelRestore(args []string) error {
	if err := jsonUnsupported("channel restore"); err != nil {
		return err
	}

	fs := newFlagSet("channel restore", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	id := fs.Int64("id", 0, "要恢复的渠道 ID")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("restore 不接受位置参数，得到 %q：要恢复的渠道用 --id 指定", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id（要恢复的渠道 ID）"))
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

	restored, err := channels.NewAdmin(db, nil, nil).Restore(ctx, *id)
	if err != nil {
		return err
	}
	if !restored {
		// 与 delete 的「已经是删除状态」配成一对：两条命令都幂等，重跑一次
		// 不该失败，而回显要说清这一次什么都没改。
		_, _ = fmt.Fprintf(humanStderr(), "渠道 %d 没有处于删除状态。\n", *id)
		return nil
	}
	// 健康结论回落这件事**必须当场说**：恢复之后的第一次 health 会报 unknown，
	// 而 unknown 与「这条渠道没问题」在只看一眼时很容易被读成一回事。这一句
	// 只在真的恢复了的时候打——没恢复的那一次没有任何结论被改写。
	_, _ = fmt.Fprintf(humanStderr(),
		"已恢复渠道 %d。它的健康结论已回落为 %s：上一次的结论是软删之前的，不能直接沿用。\n",
		*id, channels.HealthUnknown)
	return nil
}

// channelWriteAdmin 装配渠道管理面的写入侧：主密钥用来封装凭证，
// 适配器注册表用来校验渠道类型。
func channelWriteAdmin(db *sql.DB) (*channels.Admin, *credential.Sealer, error) {
	sealer, err := masterKeySealer()
	if err != nil {
		return nil, nil, err
	}
	registry, err := upstream.Registry()
	if err != nil {
		return nil, nil, fmt.Errorf("装配渠道适配器注册表失败: %w", err)
	}
	return channels.NewAdmin(db, sealer, registry), sealer, nil
}

// masterKeySealer 用环境变量里的主密钥构造凭证封装器。
//
// 它读 LARKAX_MASTER_KEYS 本身，而不是 config.Load 解析好的那一份：那份只在
// 需要主密钥的**服务角色**下才存在（worker 角色不解析它），而运维命令不属于
// 任何角色——把「命令跑不跑得起来」绑到 LARKAX_ROLE 上，会让
// LARKAX_ROLE=worker 时 channel create 因为一个与本次操作无关的变量而失败。
//
// 这条判断是**通则**，不是这一处的特例：运维命令一律走 config.LoadOps
// （它的配置面里没有角色与主密钥）。要**封装凭证**的命令走这里——写渠道与
// 轮换都用它构造 sealer；只要「这台机器带着运维凭证」而不解密的那几条
// （plugin delete 与 plugin kv set / delete）只调 config.MasterKeysFromEnv
// 探一下，不必构造 sealer。反过来，服务那一侧照旧走 config.Load，gateway / control
// 缺主密钥仍然拒绝启动。
func masterKeySealer() (*credential.Sealer, error) {
	keys, err := config.MasterKeysFromEnv()
	if err != nil {
		return nil, fmt.Errorf("读取主密钥失败（LARKAX_MASTER_KEYS）: %w", err)
	}
	sealer, err := credential.NewSealer(keys)
	if err != nil {
		return nil, fmt.Errorf("构造凭证封装器失败: %w", err)
	}
	return sealer, nil
}

// readCredential 读明文凭证，返回 nil 表示这次调用没给凭证。
//
// # 它为什么只认这两个来源
//
// 命令行参数会出现在 ps 的输出与 shell 历史里，而凭证一旦到了那里就等于
// 交给了同机的其他进程和任何一个能翻历史的人。所以这里**没有**一条从参数
// 取值路径：想传凭证只有标准输入与环境变量两种办法，而这两处都能被
// 使用者自己控制（管道的内容不进历史，环境变量可以只在这一条命令前设）。
//
// required 为 false 时（update）两者都没给是合法的，表示「不改这一项」。
//
// 返回的 nil 就是「这次没给凭证」这个合法状态，不是「出错了但没报」——
// 所以这里显式 nolint 而不是造一个哨兵错误（与 catalog.normalizeLimit 同一条）。
func readCredential(fromStdin bool, envName string, required bool) (*string, error) {
	switch {
	case fromStdin && envName != "":
		return nil, errors.New("--credential-stdin 与 --credential-env 只能给一个")

	case fromStdin:
		raw, err := io.ReadAll(credentialInput)
		if err != nil {
			return nil, fmt.Errorf("读标准输入失败: %w", err)
		}
		// 只去掉末尾的换行：`echo "$SECRET" | ...` 会多带一个，而凭证本身
		// 几乎不可能以换行结尾（凭证里带换行的话，上游也认不了）。
		// 不去掉的话，写进去的密文与运维手里那把 key 就差一个字节。
		value := strings.TrimRight(string(raw), "\r\n")
		if value == "" {
			return nil, errors.New("标准输入里没有凭证")
		}
		return &value, nil

	case envName != "":
		value := os.Getenv(envName)
		if value == "" {
			// 只说变量名，不说值——值本身就不该出现在任何输出里。
			return nil, fmt.Errorf("环境变量 %s 未设置或为空", envName)
		}
		return &value, nil

	case required:
		return nil, errors.New(
			"缺少凭证：用 --credential-stdin 或 --credential-env <变量名> 传入" +
				"（凭证不能写在命令行参数里）")

	default:
		return nil, nil //nolint:nilnil // 见上
	}
}

// printCreatedChannel 打印新渠道。
//
// 与 printCreatedUser 同一条契约：**stdout 恰好一行，只放能被直接接走的那个值**，
// 其余信息走 stderr。这里要接走的是 ID——
//
//	larkaxctl channel disable --id "$(larkaxctl channel create ...)"
//
// 是造完渠道之后很自然的一步，而那一行里多一句提示语这条链就断了。
//
// 挂在哪把主密钥下要打出来：轮换时「新写进去的密文挂在旧钥匙下」是个
// 需要当场看见的事实，事后从库里翻不如建的时候就说清楚。
//
// settingNote 为 channel.SettingUnreadNote 算出来的那句话，空串表示这次写进
// 去的键对该渠道类型都有读者。它必须与**这次调用真的写了什么**挂钩——每次
// create 都打一遍的话，它很快会变成所有输出里的一行背景音，而真正配了没人
// 读的键的人反而不会看见。
func printCreatedChannel(stdout, stderr io.Writer, id int64, primaryKeyID, settingNote string) {
	if primaryKeyID == "" {
		// 零账号是合法中间态：说清楚这条渠道还选不中，读者才知道下一步
		// 该接一个账号（而不是以为凭证已经存进去了）。
		_, _ = fmt.Fprintf(stderr,
			"已创建渠道 id=%d，尚未挂账号（零账号渠道不参与选路）。\n", id)
	} else {
		_, _ = fmt.Fprintf(stderr, "已创建渠道 id=%d，凭证挂在主密钥 %s 下。\n", id, primaryKeyID)
	}
	if settingNote != "" {
		_, _ = fmt.Fprintf(stderr, "注意：%s。\n", settingNote)
	}
	_, _ = fmt.Fprintln(stdout, id)
}

// printChannelList 打印渠道清单。
//
// **它不打印凭证密文，也不可能打印**：ChannelInfo 里根本没有密文那一栏，
// 只有「这份密文挂在哪些主密钥下」（见 channels.ChannelInfo 的注释）。
// 列举的目的是核对配置，不是把密钥搬来搬去。
//
// withDeleted 为真时（list --all）多打一栏「已删」，值为删除时刻；那一栏
// 只在 --all 下出现，因为默认清单里每一行都是零值，空列只是把噪音铺满每一行。
func printChannelList(w io.Writer, infos []channels.ChannelInfo, withDeleted bool) {
	if len(infos) == 0 {
		// 明确打一句，而不是什么都不输出：「暂无渠道」与「命令因为某个
		// 原因没跑起来」在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
		// 带上添加的命令：空清单是「还没配」，读者下一步的动作就写在那一句里。
		_, _ = fmt.Fprintln(w, emptyListNote("渠道", "channel create"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	if withDeleted {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.bold("ID"), p.bold("状态"), p.bold("类型"), p.bold("名称"),
			p.bold("上游地址"), p.bold("模型"), p.bold("归属"), p.bold("权重"),
			p.bold("优先级"), p.bold("凭证"), p.bold("已删"))
	} else {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.bold("ID"), p.bold("状态"), p.bold("类型"), p.bold("名称"),
			p.bold("上游地址"), p.bold("模型"), p.bold("归属"), p.bold("权重"),
			p.bold("优先级"), p.bold("凭证"))
	}
	for _, info := range infos {
		if withDeleted {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
				p.bold(strconv.FormatInt(info.ID, 10)), info.Status, info.Type, p.bold(info.Name),
				info.BaseURL,
				strings.Join(info.Models, ","), ownerLabel(info.OwnerUserID),
				info.Weight, info.Priority, credentialKeysLabel(info),
				p.dim(deletedLabel(info)))
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			p.bold(strconv.FormatInt(info.ID, 10)), info.Status, info.Type, p.bold(info.Name),
			info.BaseURL,
			strings.Join(info.Models, ","), ownerLabel(info.OwnerUserID),
			info.Weight, info.Priority, credentialKeysLabel(info))
	}
	_ = tw.Flush()

	// 密文坏掉的行单独说明，不跳过也不整次报错（见 channels.Admin.List）：
	// 库里有一行坏数据时整个清单一条都看不到，而那正是最需要看清单的时候。
	for _, info := range infos {
		for _, acc := range info.Accounts {
			if acc.CredentialErr != nil {
				_, _ = fmt.Fprintf(w, "渠道 %d 的账号 %s 凭证密文不合法：%v\n",
					info.ID, acc.Name, acc.CredentialErr)
			}
		}
	}

	// 已删的行单独点一句：它们的 status 也是 disabled（删除时一并置的），
	// 而「停用」是运维的临时决定、随时能启用回来；两者只看状态那一栏分不出来。
	var deletedRows []string
	for _, info := range infos {
		if !info.DeletedAt.IsZero() {
			deletedRows = append(deletedRows, strconv.FormatInt(info.ID, 10))
		}
	}
	printDeletedChannelsNote(w, deletedRows)

	// 没有读者的键逐行点出来：那一栏在库里、在这张清单上都与一个生效的
	// 配置没有区别，而这样的键写进去不会生效（见 #360）。判据是**这一行
	// 自己的 type**——同一个键对 openai 是配置、对 anthropic 是噪音
	// （见 channel.SettingUnreadNote）。
	//
	// 不新开一列是同一条理由：空列只是把噪音铺满每一行，而这里要说的只有
	// 「这几行有键没人读」。一行都没这种情况时不打：那一句在每次 list 上
	// 都是背景音，反倒把真正该看的那几行淹掉。
	for _, info := range infos {
		if note := channel.SettingUnreadNote(info.Type, info.Setting); note != "" {
			_, _ = fmt.Fprintf(w, "渠道 %d 配了 setting：%s。\n", info.ID, note)
		}
	}
}

// channelListDoc 组装 channel list 的机器可读输出。
//
// 与 printChannelList 是同一份数据的另一条渲染路径（见 format.go 开头）：
// 那一档打表、用 `-` 与「平台」铺空栏，这一档给原始值与 null。
//
// withDeleted 是这次有没有 --all：默认清单按软删过滤（每一行的 deleted_at
// 都是 null），带上 --all 之后才可能出现非空的。summary 里那个 deleted 计数
// 只在 --all 下给：它的读者问的是「这条是删了还是只是停用」，而没带 --all 时
// 它恒为 0，给出来只是一栏背景音。
func channelListDoc(infos []channels.ChannelInfo, withDeleted bool) map[string]any {
	doc := jsonDoc(jsonOK)

	summary := map[string]any{"total": len(infos)}
	if withDeleted {
		deleted := 0
		for _, info := range infos {
			if !info.DeletedAt.IsZero() {
				deleted++
			}
		}
		summary["deleted"] = deleted
	}
	doc["summary"] = summary

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, channelEntry(info))
	}
	doc["channels"] = entries
	return doc
}

// channelEntry 打清单里的一条渠道。
//
// **这一份里没有、也不会有凭证密文**：ChannelInfo 根本没有那一栏（见它的
// 注释），这里能给的只有「挂在哪些主密钥下」与末四位提示。
func channelEntry(info channels.ChannelInfo) map[string]any {
	return map[string]any{
		"id":       info.ID,
		"status":   info.Status,
		"type":     info.Type,
		"name":     info.Name,
		"base_url": info.BaseURL,
		"models":   jsonList(info.Models),
		// 0 表示平台自营（库里存 NULL），不是「编号 0 的用户」。
		"owner_user_id": jsonID(info.OwnerUserID),
		"weight":        info.Weight,
		"priority":      info.Priority,
		// 密文与凭证明文都不在这里：这两栏一个是主密钥标识、一个是末四位提示，
		// 二者都不需要密钥材料就能算出来（见 credentialKeysLabel）。
		//
		// 三个平铺字段描述**主账号**（选路会用的那一个）；完整的池子在
		// accounts 里，同样只有名字、权重与信封头部的解析结果。
		"credential_keys":  jsonList(info.CredentialKeys),
		"credential_hint":  nullableString(info.Hint),
		"credential_error": nullableError(info.CredentialErr),
		"accounts":         accountRefsJSON(info.Accounts),
		"setting":          jsonObject(info.Setting),
		"deleted_at":       jsonTimeOrNull(info.DeletedAt),
	}
}

// accountRefsJSON 打一条渠道挂着的账号。
//
// 每个账号给的是名字、权重与「挂在哪些主密钥下」——与账号清单那一档同一套
// 口径，没有密文，也没有明文。
func accountRefsJSON(accounts []channels.ChannelAccount) []map[string]any {
	out := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		out = append(out, map[string]any{
			"id":               acc.ID,
			"name":             acc.Name,
			"weight":           acc.Weight,
			"credential_keys":  jsonList(acc.CredentialKeys),
			"credential_hint":  nullableString(acc.Hint),
			"credential_error": nullableError(acc.CredentialErr),
		})
	}
	return out
}

// printChannelDetail 打一条渠道的详情：一栏一行。
//
// 与 printChannelList 是同一份数据的另一种排版（一栏一行 vs 一张表），与
// user / apikey / model 的 get 是同一个形状。
func printChannelDetail(w io.Writer, info channels.ChannelInfo) {
	tw := newTableWriter(w)
	p := newPalette(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("ID", strconv.FormatInt(info.ID, 10))
	row("名称", info.Name)
	row("状态", info.Status)
	row("类型", info.Type)
	row("上游地址", info.BaseURL)
	row("模型", strings.Join(info.Models, ","))
	row("归属", ownerLabel(info.OwnerUserID))
	row("权重", strconv.Itoa(info.Weight))
	row("优先级", strconv.Itoa(info.Priority))
	row("凭证", credentialKeysLabel(info))
	row("账号", accountRefsLabel(info))
	row("setting", settingLabel(info.Setting))
	row("创建时间", p.dim(info.CreatedAt.Format(time.RFC3339)))
	row("更新时间", p.dim(info.UpdatedAt.Format(time.RFC3339)))
	// 「已删」整行只在真删了的时候出现：单条详情只有一行，打一个空的已删栏
	// 除了让人多找一次「哪儿写着我删没删」没有别的用处（与 list 那一栏不同，
	// 那里一列空值要能逐行分辨）。
	if !info.DeletedAt.IsZero() {
		row("已删", p.dim(deletedAtLabel(info.DeletedAt)))
	}
	_ = tw.Flush()

	if info.CredentialErr != nil {
		_, _ = fmt.Fprintf(w, "渠道 %d 的凭证密文不合法：%v\n", info.ID, info.CredentialErr)
	}
	for _, acc := range info.Accounts {
		if acc.CredentialErr != nil {
			_, _ = fmt.Fprintf(w, "渠道 %d 的账号 %s 凭证密文不合法：%v\n",
				info.ID, acc.Name, acc.CredentialErr)
		}
	}
}

// accountRefsLabel 打这条渠道挂着的账号名与权重。
//
// 权重为 1 时不写出来：那是默认值，铺在每一行只是噪音；改过权重才值得
// 在详情里看见。
func accountRefsLabel(info channels.ChannelInfo) string {
	if len(info.Accounts) == 0 {
		return "无账号"
	}
	parts := make([]string, 0, len(info.Accounts))
	for _, acc := range info.Accounts {
		if acc.Weight == 1 {
			parts = append(parts, acc.Name)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s（权重 %d）", acc.Name, acc.Weight))
	}
	return strings.Join(parts, ", ")
}

// settingLabel 打渠道私有配置。
//
// 空对象打 `{}` 而不是 `-`：库里那一列是 NOT NULL DEFAULT '{}'，「没有额外配置」
// 本身就是一个确定的值，与「这一栏没读出来」是两回事。
func settingLabel(setting map[string]any) string {
	if len(setting) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(setting)
	if err != nil {
		return "（不是可序列化的 JSON 对象）"
	}
	return string(raw)
}

// printChannelHealthList 打按渠道汇总的健康表。
//
// withDeleted 与 list --all 那一栏同义：默认清单里每一行都是零值，那一列只是把
// 噪音铺满每一行。
func printChannelHealthList(w io.Writer, rows []channels.ChannelHealth, withDeleted bool) {
	if len(rows) == 0 {
		// 与 list 共用同一句：空清单与「命令没跑起来」在安静的终端里长得一样
		// （见 LESSONS.md 第 16 条），而这里的下一步动作也确实是建一条渠道。
		_, _ = fmt.Fprintln(w, emptyListNote("渠道", "channel create"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	headers := []string{"ID", "名称", "health", "最近探测", "请求数", "失败数", "失败率",
		"缓存命中", "缓存未命中", "p50", "p95", "成本"}
	if withDeleted {
		headers = append(headers, "已删")
	}
	bolded := make([]string, len(headers))
	for i, h := range headers {
		bolded[i] = p.bold(h)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(bolded, "\t"))

	for _, h := range rows {
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%d\t%s\t%d\t%d\t%s\t%s\t%s",
			p.bold(strconv.FormatInt(h.ID, 10)), p.bold(h.Name), h.Health,
			p.dim(probeLabel(h.LastProbeAt)),
			h.Requests, h.Failures, rateLabel(h.FailureRate()),
			h.CacheHitTokens, h.CacheMissTokens,
			latencyLabel(h.LatencyP50ms), latencyLabel(h.LatencyP95ms), costLabel(h.Cost))
		if withDeleted {
			line += "\t" + p.dim(deletedAtLabel(h.DeletedAt))
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()

	// 与 list 同一句：已删的行不该只靠一个状态字去认，下一步该敲哪条命令也
	// 写在同一句里。
	var deleted []string
	for _, h := range rows {
		if !h.DeletedAt.IsZero() {
			deleted = append(deleted, strconv.FormatInt(h.ID, 10))
		}
	}
	printDeletedChannelsNote(w, deleted)
}

// printChannelHealthDetail 打一条渠道的健康详情：一栏一行。
//
// 与汇总表同一份数据、另一种排版。健康那一栏与几个计数在这里是逐项列出来的，
// 因为 --id 这一档的读者问的是「这条渠道怎么了」，而不是「哪条渠道最差」。
func printChannelHealthDetail(w io.Writer, h channels.ChannelHealth) {
	tw := newTableWriter(w)
	p := newPalette(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("ID", strconv.FormatInt(h.ID, 10))
	row("名称", h.Name)
	row("health", h.Health)
	row("最近探测", probeLabel(h.LastProbeAt))
	row("请求数", strconv.FormatInt(h.Requests, 10))
	row("成功数", strconv.FormatInt(h.Successes, 10))
	row("失败数", strconv.FormatInt(h.Failures, 10))
	row("失败率", rateLabel(h.FailureRate()))
	row("缓存命中", strconv.FormatInt(h.CacheHitTokens, 10))
	row("缓存未命中", strconv.FormatInt(h.CacheMissTokens, 10))
	row("p50", latencyLabel(h.LatencyP50ms))
	row("p95", latencyLabel(h.LatencyP95ms))
	row("成本", costLabel(h.Cost))
	if !h.DeletedAt.IsZero() {
		row("已删", p.dim(deletedAtLabel(h.DeletedAt)))
	}
	_ = tw.Flush()
}

// printChannelHealthHours 打一条渠道在这个窗口里的逐小时明细。
//
// 最近的一小时在最上面：这条命令回答的是「它最近怎么了」。
//
// 分位数在这里是**每个桶自己的值**，不跨桶合并。汇总行里每一列取的是它自己最近
// 一个有值的那一桶，所以最上面那一行的 p50 / p95 通常与汇总行一致（除非最近那一桶
// 在某一列上没有样本，那一列就落到更早的一桶）。
func printChannelHealthHours(w io.Writer, buckets []channels.HealthBucket) {
	if len(buckets) == 0 {
		// 这一句**不套**空清单那一句：这里缺的不是「还没有渠道」这个可以添加的
		// 东西，而是「这段时间它没被调用过」这个结论，给不出下一步命令。
		_, _ = fmt.Fprintln(w, "窗口内没有逐小时的数据：这段时间它一次都没被调用过。")
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	headers := []string{"小时", "请求数", "成功数", "失败数", "缓存命中", "缓存未命中", "p50", "p95", "成本"}
	bolded := make([]string, len(headers))
	for i, h := range headers {
		bolded[i] = p.bold(h)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(bolded, "\t"))

	for _, b := range buckets {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
			p.dim(b.Bucket.Format(time.RFC3339)), b.Requests, b.Successes, b.Failures,
			b.CacheHitTokens, b.CacheMissTokens,
			latencyLabel(b.LatencyP50ms), latencyLabel(b.LatencyP95ms), costLabel(b.Cost))
	}
	_ = tw.Flush()
}

// probeLabel 打最近一次额度探测的时刻。
//
// 没探测过打「从未」而不是 `-`：这张表里的 `-` 已经表示「这个窗口里没有数据」
// （没有失败、没有样本、没有成本），而「从来没探测过这条渠道」是另一件具体的事，
// 值一个自己的词。
func probeLabel(at *time.Time) string {
	if at == nil {
		return "从未"
	}
	return at.Format(time.RFC3339)
}

// rateLabel 打失败率。
//
// 窗口里没有请求时打 `-`：0/0 不是一个百分数，而打 `0.0%` 会与「请求过很多次、
// 一次都没失败」长得一样——那两件事的下一步动作正相反。
func rateLabel(rate *float64) string {
	if rate == nil {
		return "-"
	}
	return strconv.FormatFloat(*rate*100, 'f', 1, 64) + "%"
}

// latencyLabel 打分位数。
//
// nil 是「这个窗口里没有样本」，与「样本是 0 毫秒」分开：后者是一条很可疑的读数，
// 而前者只是没数据。用零值接住 nil 会让一条没有任何样本的渠道看起来快得离谱。
func latencyLabel(ms *int) string {
	if ms == nil {
		return "-"
	}
	return strconv.Itoa(*ms) + "ms"
}

// costLabel 打窗口内的成本合计。
//
// nil 是「没有数据」（见 channels.ChannelHealth.Cost）：读取侧用
// `NULLIF(SUM(cost), 0)` 把「没有带成本的事件」与「合计恰好为 0」都折成 nil，
// 所以 `0.00000000` 不会作为「这段窗口里没花钱」打出来——那正是这一列最容易
// 骗人的一种读数。
func costLabel(cost *string) string {
	if cost == nil {
		return "-"
	}
	return *cost
}

// printDeletedChannelsNote 点出清单里已删的那几行。
//
// 它们的 status 也是 disabled（删除时一并置的），而「停用」是运维的临时决定、
// 随时能启用回来；两者只看状态那一栏分不出来。list 与 health 共用这一句，
// 于是「怎么把它弄回来」这个下一步只有一处答案。
//
// 收的是 ID 清单而不是某一种行类型：两个调用方的行类型不同，而这一句要说的事情
// 只用到 ID。
func printDeletedChannelsNote(w io.Writer, deleted []string) {
	if len(deleted) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "渠道 %s 已删除：它们不再参与选路，也不能 update / enable / disable；"+
		"用 larkaxctl channel restore 可以恢复回来。\n", strings.Join(deleted, "、"))
}

// channelGetDoc 组装 channel get 的机器可读输出。
func channelGetDoc(info channels.ChannelInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["channel"] = channelDetailEntry(info)
	return doc
}

// channelDetailEntry 打一条渠道的详情：在清单那几栏之上多给创建与更新时刻。
func channelDetailEntry(info channels.ChannelInfo) map[string]any {
	entry := channelEntry(info)
	entry["created_at"] = jsonTime(info.CreatedAt)
	entry["updated_at"] = jsonTime(info.UpdatedAt)
	return entry
}

// channelHealthDoc 组装 channel health 的机器可读输出。
//
// since 是这次的窗口起点，它进 summary：不给的话 `"requests": 12` 说不清是哪段
// 时间里的 12，而这一档正是给脚本读的。
//
// detail 为真时（--id）多给一份逐小时明细。两条路共用同一个顶层形状（channels
// 数组，--id 那一档恰好一个元素），于是 `jq '.channels[]'` 在两种调用下都成立。
func channelHealthDoc(rows []channels.ChannelHealth, since time.Time, withDeleted bool,
	buckets []channels.HealthBucket, detail bool) map[string]any {
	doc := jsonDoc(jsonOK)

	summary := map[string]any{"total": len(rows), "since": jsonTime(since)}
	if withDeleted {
		deleted := 0
		for _, h := range rows {
			if !h.DeletedAt.IsZero() {
				deleted++
			}
		}
		summary["deleted"] = deleted
	}
	if detail {
		summary["buckets"] = len(buckets)
	}
	doc["summary"] = summary

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(rows))
	for _, h := range rows {
		entries = append(entries, healthEntry(h))
	}
	doc["channels"] = entries

	if detail {
		bs := make([]map[string]any, 0, len(buckets))
		for _, b := range buckets {
			bs = append(bs, healthBucketEntry(b))
		}
		doc["buckets"] = bs
	}
	return doc
}

// healthEntry 打一条渠道的健康读数：表格那几栏对应的原始值。
//
// 缓存那两个 token 数是**数字**而不是 null：与 requests 同一档，0 表示窗口里没有
// 样本（见 channels.ChannelHealth 上那两个字段）。
func healthEntry(h channels.ChannelHealth) map[string]any {
	return map[string]any{
		"id":                h.ID,
		"name":              h.Name,
		"health":            h.Health,
		"last_probe_at":     jsonTimePtr(h.LastProbeAt),
		"requests":          h.Requests,
		"successes":         h.Successes,
		"failures":          h.Failures,
		"failure_rate":      jsonOpt(h.FailureRate()),
		"cache_hit_tokens":  h.CacheHitTokens,
		"cache_miss_tokens": h.CacheMissTokens,
		"latency_p50_ms":    jsonOpt(h.LatencyP50ms),
		"latency_p95_ms":    jsonOpt(h.LatencyP95ms),
		"cost":              jsonOpt(h.Cost),
		"deleted_at":        jsonTimeOrNull(h.DeletedAt),
	}
}

// healthBucketEntry 打逐小时明细里的一行。
func healthBucketEntry(b channels.HealthBucket) map[string]any {
	return map[string]any{
		"bucket":            jsonTime(b.Bucket),
		"requests":          b.Requests,
		"successes":         b.Successes,
		"failures":          b.Failures,
		"cache_hit_tokens":  b.CacheHitTokens,
		"cache_miss_tokens": b.CacheMissTokens,
		"latency_p50_ms":    jsonOpt(b.LatencyP50ms),
		"latency_p95_ms":    jsonOpt(b.LatencyP95ms),
		"cost":              jsonOpt(b.Cost),
	}
}

// jsonOpt 把「可能没有值的指针」翻成 JSON：nil 是 null，否则是它指着的那个值。
//
// 表格那一档的 `-` 与「从未」一个都不进 JSON：`"latency_p95_ms": "-"` 会让按数字
// 解析它的一方拿到一个解析不了的值（见 docs/cli-output.md 的「值的约定」）。
func jsonOpt[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}

// deletedLabel 是 list --all 里「已删」那一栏。
//
// 没删的行留空，打一个占位符会让每一行都有东西、而读者要逐行分辨哪个占位符
// 表示「没有」；已删的行给出删除时刻，因为「什么时候删的」是事后核对时唯一
// 能把它与历史账单对上的一条线索。
func deletedLabel(info channels.ChannelInfo) string {
	return deletedAtLabel(info.DeletedAt)
}

// deletedAtLabel 把一个删除时刻打成那一栏里的值：零值（没删）留空。
func deletedAtLabel(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

// credentialKeysLabel 打清单里的「凭证」那一栏。
//
// 它是**账号侧**的一栏（见迁移 00067）：零账号时说「无账号」，而不是留空
// 或打 `-`——「没有凭证」与「凭证挂在没有的钥匙下」是两件事。
//
// 两样一起给：主密钥标识回答「挂在哪把钥匙下、轮换补到哪一步」，
// 末四位提示回答「挂的是我刚发的那个凭证吗」。它们是两个不同的问题，
// 只给其中一个都会让运维去别处再查一次。
//
// 多个账号时列主密钥的并集并标出账号数：轮换要问的是「还有没有哪一把 key
// 没补上新主密钥」，并集正好回答这个；末四位只在一个账号时才有意义，
// 多了就分不清是哪一个的尾巴。
func credentialKeysLabel(info channels.ChannelInfo) string {
	if len(info.Accounts) == 0 {
		return "无账号"
	}
	for _, acc := range info.Accounts {
		if acc.CredentialErr != nil {
			// 上面会另起一行说明原因，这里只留一个显眼的标记。
			return "密文不合法"
		}
	}

	var keys []string
	seen := make(map[string]bool, len(info.CredentialKeys))
	for _, acc := range info.Accounts {
		for _, key := range acc.CredentialKeys {
			if seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	label := "-"
	if len(keys) > 0 {
		label = strings.Join(keys, ",")
	}
	if len(info.Accounts) > 1 {
		return fmt.Sprintf("%d 个账号 %s", len(info.Accounts), label)
	}
	if info.Accounts[0].Hint == "" {
		// 空提示只可能来自「这一列接上之前建的账号」——接上之后每一次写入
		// 都会带上它。这里不补一个占位符，让「没有提示」与「提示是某几个
		// 字符」在清单上分得开。
		return label
	}
	return label + " …" + info.Accounts[0].Hint
}

// ownerLabel 把归属打成「平台」或用户 ID。
//
// 0 表示平台自营（库里存 NULL），见 channels.CreateRequest.OwnerUserID。
// 直接打 0 会让人以为那是一个真实的用户。
func ownerLabel(ownerUserID int64) string {
	if ownerUserID == 0 {
		return "平台"
	}
	return strconv.FormatInt(ownerUserID, 10)
}

// parseJSONObject 解析一个 JSON 对象参数。
//
// 空串是合法的，表示空对象。解析失败时报**哪个参数**不对，不回显内容——
// 与凭证字段共用同一套写法，免得后来把某个字段换成凭证时忘了改这一处。
//
// 空串返回 nil 映射：写入路径的 encodeSetting 把「长度为零」编成 `{}`，
// 所以 nil 与空映射在库里是同一个值（NULL 会破坏 jsonb 的非空约束）。
// 它是这一栏的合法取值，不是「出错了但没报」，所以这里显式 nolint。
func parseJSONObject(name, raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil //nolint:nilnil // 见上
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s 不是合法的 JSON 对象: %w", name, err)
	}
	return out, nil
}

// splitList 把逗号分隔的取值切成切片。
//
// 它只做切分，不去重也不丢空项：校验是写入路径的事（空的模型名会被它
// 按序号指出来），在这一层再做一遍就成了两处判据，改了一处另一处还在。
func splitList(raw string) []string {
	return strings.Split(raw, ",")
}

// ── adapters 与 probe ───────────────────────────────────────────────────
//
// 这两条是**落库之前**的读侧：一条列「有哪些适配器类型」，一条拿一份还没保存
// 的地址与凭证去问上游。它们都不连库、不需要主密钥——凭证直接从标准输入或
// 环境变量读，地址来自命令行。

// probeChannelRegistry 取探测用的适配器注册表。
//
// 做成变量只为用例能注入一张空表（空表在正常构建里造不出来）；生产路径永远
// 走 upstream.ProbeRegistry。与 credentialInput 同一类注入点。
var probeChannelRegistry = upstream.ProbeRegistry

// runChannelAdapters 打印已注册的适配器类型、协议与端点。
//
// 读的是 ProbeRegistry 而不是 Registry：控制台的 GET /admin/v1/adapters 用的
// 就是那一份（见 internal/adminapi/probe.go 的 listAdapters），两处同源，
// 「控制台列得出的类型」与「命令行列得出的类型」不会各说各的。
func runChannelAdapters(args []string) error {
	if err := jsonUnsupported("channel adapters"); err != nil {
		return err
	}

	fs := newFlagSet("channel adapters", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("adapters 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	registry, err := probeChannelRegistry()
	if err != nil {
		return fmt.Errorf("装配渠道适配器注册表失败: %w", err)
	}
	if len(registry.Types()) == 0 {
		// 注册表由内置清单装配，正常构建下不可能是空的。空表说的是「这个
		// 二进制不完整」，而不是「今天一个适配器都没有」——后者会被读成
		// 一次正常的查询结果，所以这里报内部错误（internalErr 在人读的那句
		// 前面另加「内部错误: 」）。也没有任何添加适配器的命令可指。
		return internalErr(errors.New("内置适配器注册表为空：这个二进制的装配不完整"))
	}
	printChannelAdapters(humanStdout(), registry)
	return nil
}

// printChannelAdapters 打一张「类型 / 协议 / 端点」的表。
//
// 三栏与控制台 adapterJSON 的三个字段一一对应（type / protocol / endpoint）。
// 顺序直接用 registry.Types()：它已经排好序，调用点不再排第二遍。
func printChannelAdapters(w io.Writer, registry *channel.Registry) {
	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
		p.bold("类型"), p.bold("协议"), p.bold("端点"))
	for _, name := range registry.Types() {
		adapter, err := registry.Lookup(name)
		if err != nil {
			// Types 与 Lookup 读的是同一张表，这里不可能落空；落空也不该让
			// 整张清单消失——少一行比一条都没有好（与控制台那一侧同一条）。
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
			p.bold(adapter.Type()), adapter.Protocol(), adapter.Endpoint())
	}
	_ = tw.Flush()
}

// channelProbeArgs 是 channel probe 校验过的输入。
type channelProbeArgs struct {
	channelType string
	baseURL     string
	credential  string
}

// parseChannelProbe 解析并校验 channel probe 的参数。
//
// 校验全部发生在发请求之前：写错的命令不该先打一次上游再报错，而这些错误
// 都是参数错（退出码 2）。
func parseChannelProbe(args []string) (channelProbeArgs, error) {
	fs := newFlagSet("channel probe", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, channelShortUsage) }
	channelType := fs.String("type", "", "")
	baseURL := fs.String("base-url", "", "")
	credentialStdin := fs.Bool("credential-stdin", false, "")
	boolAlias(fs, credentialStdin, "stdin")
	credentialEnv := fs.String("credential-env", "", "")
	stringAlias(fs, credentialEnv, "env")
	if err := fs.Parse(args); err != nil {
		return channelProbeArgs{}, err
	}
	if fs.NArg() > 0 {
		return channelProbeArgs{}, fmt.Errorf(
			"probe 不接受位置参数，得到 %q：上游地址用 --base-url 指定", fs.Arg(0))
	}
	if strings.TrimSpace(*channelType) == "" {
		return channelProbeArgs{}, errors.New(
			"缺少 --type（渠道类型，可用 larkaxctl channel adapters 查看）")
	}
	if strings.TrimSpace(*baseURL) == "" {
		return channelProbeArgs{}, errors.New("缺少 --base-url")
	}
	// 地址与写入路径共用同一个校验：原文（「base_url 不能带 query……」）直接
	// 往上报，包上 ErrInvalidBaseURL 这一层是为了让分类认得出这是地址不合法。
	normalized, err := channels.NormalizeBaseURL(*baseURL)
	if err != nil {
		return channelProbeArgs{}, fmt.Errorf("%w：%w", channels.ErrInvalidBaseURL, err)
	}
	plaintext, err := readCredential(*credentialStdin, *credentialEnv, false)
	if err != nil {
		return channelProbeArgs{}, err
	}
	out := channelProbeArgs{channelType: strings.TrimSpace(*channelType), baseURL: normalized}
	if plaintext != nil {
		out.credential = *plaintext
	}
	return out, nil
}

// runChannelProbe 拿一份还没落库的地址与凭证向上游问一次模型清单。
func runChannelProbe(args []string) error {
	if err := jsonUnsupported("channel probe"); err != nil {
		return err
	}

	parsed, err := parseChannelProbe(args)
	if err != nil {
		return usageErr(err)
	}

	registry, err := probeChannelRegistry()
	if err != nil {
		return fmt.Errorf("装配渠道适配器注册表失败: %w", err)
	}
	// 类型写错是命令错：在发请求之前判，按退出码 2 报。Lookup 的报错里带着
	// 注册表现有的类型（从 Types() 取，不在这里抄第二份）。
	if _, lookupErr := registry.Lookup(parsed.channelType); lookupErr != nil {
		return usageErr(fmt.Errorf("渠道类型无效: %w", lookupErr))
	}

	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	result, err := channels.NewModelProbe(nil, nil, registry).Probe(ctx, channels.ProbeRequest{
		Type:       parsed.channelType,
		BaseURL:    parsed.baseURL,
		Credential: parsed.credential,
	})
	if err != nil {
		return probeFailure(err)
	}
	if !result.Supported {
		// 「没有这个接口」不是失败，也不等于「清单是空的」：打一句说明、退出
		// 码 0，并且不打任何清单行——空清单与一次失败的输出在屏幕上分不开。
		_, _ = fmt.Fprintf(humanStdout(),
			"这家上游（%s）不提供模型列表，这不算错误。\n", parsed.channelType)
		return nil
	}
	printProbeModels(humanStdout(), result)
	return nil
}

// probeFailure 给上游的失败补上状态码与响应体摘要。
//
// ErrUpstream 的 Error() 只说「上游返回 401」，而探测要回答的正是「这把钥匙
// 为什么不行」——上游那句原文往往就是答案。摘要已经由 channels.ModelProbe
// 把这次用的凭证换成了 <redacted>（见 redactUpstream），这一层不再碰正文，
// 也不该在别处另拼一份原始 Body。
func probeFailure(err error) error {
	var upstreamErr *channel.ErrUpstream
	if !errors.As(err, &upstreamErr) || !upstreamErr.GotResponse {
		return err
	}
	body := doctorPreview([]byte(upstreamErr.Body))
	if body == "" {
		return err
	}
	return fmt.Errorf("%w：%s", err, body)
}

// printProbeModels 打一次探测拿到的清单。
//
// 第一行是条数，下面一行一个模型名：不编号（名字已经由 ModelProbe 去重并按
// 升序排好），这样 `| grep` 与 `| wc -l` 都直接可用。截断时首行写出上游
// 一共给了多少——Total 是截断前的条数，不能拿打出来的行数当总数。
func printProbeModels(w io.Writer, result channels.ProbeResult) {
	if result.Truncated {
		_, _ = fmt.Fprintf(w, "上游提供 %d 个模型，以下只列出前 %d 个。\n",
			result.Total, len(result.Models))
	} else {
		_, _ = fmt.Fprintf(w, "上游提供 %d 个模型。\n", result.Total)
	}
	for _, model := range result.Models {
		_, _ = fmt.Fprintln(w, model)
	}
}
