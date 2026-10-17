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

const channelUsage = `larkaxctl channel 管理上游渠道。

用法：
  larkaxctl channel create --name <名称> --type <类型> --base-url <URL>
                           --models <模型名,...>
                           (--credential-stdin | --credential-env <变量名>)
                           [--owner <用户 ID 或用户名>] [--weight <权重>]
                           [--priority <优先级>] [--setting <JSON 对象>]
  larkaxctl channel list [--all]
  larkaxctl channel update --id <渠道 ID>
                           [--name <名称>] [--type <类型>] [--base-url <URL>]
                           [--models <模型名,...>] [--weight <权重>]
                           [--priority <优先级>] [--setting <JSON 对象>]
                           [--credential-stdin | --credential-env <变量名>]
  larkaxctl channel enable --id <渠道 ID>
  larkaxctl channel disable --id <渠道 ID>
  larkaxctl channel delete --id <渠道 ID>

**明文凭证只能从标准输入或环境变量读，不能写在命令行参数里。**
命令行参数会出现在 ps 的输出与 shell 历史里，凭证到了那里就等于交给了
同机的其他进程和任何一个能翻历史的人。

  --credential-stdin        从标准输入读到文件结束（末尾的换行会被去掉）
  --credential-env <变量名> 从名为 <变量名> 的环境变量读

例（凭证从标准输入读）：

  cat secret.txt | larkaxctl channel create --name c1 --type openai \
      --base-url https://api.example.com/v1 --models <规范模型名> --credential-stdin

例（凭证从环境变量读）：

  LARKAX_CHANNEL_CREDENTIAL=<CHANNEL_CREDENTIAL> larkaxctl channel create ... \
      --credential-env LARKAX_CHANNEL_CREDENTIAL

create 的 stdout 只有新渠道的 ID，其余信息走 stderr。
list **不打印凭证密文**，只打印它挂在哪些主密钥下（那不需要任何密钥材料）。
list 默认不列已删的渠道，--all 才列，并在「已删」那一栏标出删除时刻。

delete 是**软删**：置 deleted_at 并把状态改成 disabled，行与它的凭证密文都留在
库里。历史账单（billing_requests / usage_events）仍然按 channel_id 引用它，所以
删除之后那些记录照样认得这条渠道。已删的渠道不能再 update / enable / disable，
再删一次不报错（打印「已经是删除状态」）。

渠道类型必须是已注册的适配器类型；类型写错时错误信息会列出已知的类型。
--models 至少要有一个：读取路径把 models 当白名单用，空数组的渠道永远选不中。
--setting 是渠道私有配置（自由形态的 JSON 对象），键由该渠道 type 指向的
适配器解释，选路层不认，管理面也不校验键名。**当前没有任何适配器读它**，
所以给了 --setting 时命令会把这一点说出来，见 internal/channel 的 Setting。
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

func runChannel(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, channelUsage)
		return usageErr(errors.New("channel 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create":
		return runChannelCreate(rest)
	case "list":
		return runChannelList(rest)
	case "update":
		return runChannelUpdate(rest)
	case "enable":
		return runChannelSetStatus(rest, channels.StatusEnabled)
	case "disable":
		return runChannelSetStatus(rest, channels.StatusDisabled)
	case "delete":
		return runChannelDelete(rest)
	case "help", "-h", "--help":
		fmt.Print(channelUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, channelUsage)
		return usageErr(fmt.Errorf("未知的 channel 子命令 %q", action))
	}
}

func runChannelCreate(args []string) error {
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

	admin, sealer, err := channelWriteAdmin(db)
	if err != nil {
		return err
	}
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
	printCreatedChannel(os.Stdout, os.Stderr, id, sealer.PrimaryKeyID(), len(req.Setting) > 0)
	return nil
}

// parseChannelCreate 解析 create 的参数。
//
// 它多带一个 ownerRef 返回值：--owner 要连库才能解析成用户 ID，而参数解析
// 必须在连库之前完成。所以这里只做「有没有给、给了什么」，解析留给调用方。
func parseChannelCreate(args []string) (channels.CreateRequest, string, error) {
	fs := flag.NewFlagSet("channel create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, channelUsage) }
	name := fs.String("name", "", "渠道名")
	channelType := fs.String("type", "", "适配器类型")
	baseURL := fs.String("base-url", "", "上游地址，必须以 http:// 或 https:// 开头")
	models := fs.String("models", "", "该渠道支持的规范模型名，逗号分隔，至少一个")
	owner := fs.String("owner", "", "归属用户（ID 或用户名），留空表示平台自营")
	weight := fs.Int("weight", 0, "分流权重，0 表示用默认值 1")
	priority := fs.Int("priority", 0, "优先级，越大越优先")
	setting := fs.String("setting", "", "渠道级 JSON 配置对象，留空表示空对象")
	credentialStdin := fs.Bool("credential-stdin", false, "从标准输入读明文凭证")
	credentialEnv := fs.String("credential-env", "", "从指定环境变量读明文凭证")
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
		fs.Usage()
		return channels.CreateRequest{}, "", errors.New("缺少 --name")
	}
	if strings.TrimSpace(*channelType) == "" {
		fs.Usage()
		return channels.CreateRequest{}, "", errors.New("缺少 --type")
	}
	if strings.TrimSpace(*baseURL) == "" {
		fs.Usage()
		return channels.CreateRequest{}, "", errors.New("缺少 --base-url")
	}
	if strings.TrimSpace(*models) == "" {
		fs.Usage()
		return channels.CreateRequest{}, "", errors.New("缺少 --models（至少一个规范模型名）")
	}
	plaintext, err := readCredential(*credentialStdin, *credentialEnv, true)
	if err != nil {
		return channels.CreateRequest{}, "", err
	}
	parsedSetting, err := parseJSONObject("--setting", *setting)
	if err != nil {
		return channels.CreateRequest{}, "", err
	}

	return channels.CreateRequest{
		Name:       *name,
		Type:       *channelType,
		BaseURL:    *baseURL,
		Credential: *plaintext,
		Models:     splitList(*models),
		Weight:     *weight,
		Priority:   *priority,
		Setting:    parsedSetting,
	}, *owner, nil
}

// parseChannelUpdate 解析 update 的参数。
//
// 没有 --owner：归属（平台自营 / 某个商家）不在写入路径能改的字段里，
// 见 channels.UpdateRequest。要换归属是另一件事，不该混在一次「改配置」里。
func parseChannelUpdate(args []string) (channels.UpdateRequest, error) {
	fs := flag.NewFlagSet("channel update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, channelUsage) }
	id := fs.Int64("id", 0, "要改的渠道 ID")
	name := fs.String("name", "", "渠道名")
	channelType := fs.String("type", "", "适配器类型")
	baseURL := fs.String("base-url", "", "上游地址")
	models := fs.String("models", "", "该渠道支持的规范模型名，逗号分隔")
	weight := fs.Int("weight", 0, "分流权重")
	priority := fs.Int("priority", 0, "优先级")
	setting := fs.String("setting", "", "渠道级 JSON 配置对象")
	credentialStdin := fs.Bool("credential-stdin", false, "从标准输入读新的明文凭证")
	credentialEnv := fs.String("credential-env", "", "从指定环境变量读新的明文凭证")
	if err := fs.Parse(args); err != nil {
		return channels.UpdateRequest{}, err
	}
	if fs.NArg() > 0 {
		return channels.UpdateRequest{}, fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的渠道用 --id 指定", fs.Arg(0))
	}
	if *id <= 0 {
		fs.Usage()
		return channels.UpdateRequest{}, errors.New("缺少 --id（要改的渠道 ID）")
	}

	// 只有**真的给了**的标志才进请求。--priority 0 与「没提这一项」是两件事，
	// 而 flag 包的零值分不出它们，所以这里看 fs.Visit 记下的清单。
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	req := channels.UpdateRequest{ID: *id}
	changes := 0
	if given["name"] {
		req.Name = name
		changes++
	}
	if given["type"] {
		req.Type = channelType
		changes++
	}
	if given["base-url"] {
		req.BaseURL = baseURL
		changes++
	}
	if given["models"] {
		list := splitList(*models)
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
	if given["credential-stdin"] || given["credential-env"] {
		plaintext, err := readCredential(*credentialStdin, *credentialEnv, true)
		if err != nil {
			return channels.UpdateRequest{}, err
		}
		req.Credential = plaintext
		changes++
	}
	if changes == 0 {
		fs.Usage()
		return channels.UpdateRequest{}, errors.New(
			"update 至少要改一项：给出 --name / --type / --base-url / --models / " +
				"--weight / --priority / --setting / --credential-* 之一")
	}

	return req, nil
}

func runChannelUpdate(args []string) error {
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
	fmt.Printf("已更新渠道 %d。\n", req.ID)
	if req.Setting != nil && len(*req.Setting) > 0 {
		printSettingUnreadNote(os.Stderr)
	}
	return nil
}

// printSettingUnreadNote 说出「配了 setting 也不会影响请求路径」。
//
// 打在写入**成功之后**：命令确实写进去了，所以不能报成错误；而「写进去
// 了、但没有任何东西按它做判断」正是使用者当场该看见的事实——只写在文档里
// 的话没人会读，而它在库里、在清单上看起来都与一个生效的配置一模一样
// （见 #360）。
//
// 走 stderr，与 create 的其余信息同一处：stdout 只放能被下一段脚本接走的
// 那个值（create 是 ID，update 是一行结果），一句提示混进去会把它弄脏。
//
// 措辞与「什么时候会变」取自 channel.SettingUnreadNote，不在这里另写一份：
// 那段说明属于 channel 包（键归适配器解释是那条契约的一部分），而两处各写
// 一句的话，一处改了一处不动，于是其中一处开始说谎。
func printSettingUnreadNote(w io.Writer) {
	note := channel.SettingUnreadNote()
	if note == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "注意：%s。\n", note)
}

func runChannelList(args []string) error {
	fs := flag.NewFlagSet("channel list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, channelUsage) }
	all := fs.Bool("all", false, "含已删除的渠道，并在「已删」一栏标出删除时刻")
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
	printChannelList(os.Stdout, infos, *all)
	return nil
}

func runChannelSetStatus(args []string, status string) error {
	verb, done := "enable", "已启用"
	if status == channels.StatusDisabled {
		verb, done = "disable", "已停用"
	}

	fs := flag.NewFlagSet("channel "+verb, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, channelUsage) }
	id := fs.Int64("id", 0, "渠道 ID")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("%s 不接受位置参数，得到 %q：渠道用 --id 指定", verb, fs.Arg(0)))
	}
	if *id <= 0 {
		fs.Usage()
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
	fmt.Printf("渠道 %d %s。\n", *id, done)
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
	fs := flag.NewFlagSet("channel delete", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, channelUsage) }
	id := fs.Int64("id", 0, "要删的渠道 ID")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("delete 不接受位置参数，得到 %q：要删的渠道用 --id 指定", fs.Arg(0)))
	}
	if *id <= 0 {
		fs.Usage()
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
		fmt.Printf("已删除渠道 %d。\n", *id)
		return nil
	}
	fmt.Printf("渠道 %d 已经是删除状态。\n", *id)
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
// （它的配置面里没有角色与主密钥），需要主密钥的命令调这里——写渠道与轮换
// 都走这一个函数。反过来，服务那一侧照旧走 config.Load，gateway / control
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
// hasSetting 为真时再补一句「setting 今天没有读者」：那句话必须与这次调用
// 是否真的写了这个键挂钩——每次 create 都打一遍的话，它很快会变成所有输出
// 里的一行背景音，而真正配了键的人反而不会看见。
func printCreatedChannel(stdout, stderr io.Writer, id int64, primaryKeyID string, hasSetting bool) {
	_, _ = fmt.Fprintf(stderr, "已创建渠道 id=%d，凭证挂在主密钥 %s 下。\n", id, primaryKeyID)
	if hasSetting {
		printSettingUnreadNote(stderr)
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
		// 明确打一句，而不是什么都不输出：「还没有渠道」与「命令因为某个
		// 原因没跑起来」在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
		_, _ = fmt.Fprintln(w, "还没有渠道。")
		return
	}

	tw := newTableWriter(w)
	if withDeleted {
		_, _ = fmt.Fprintln(tw, "ID\t状态\t类型\t名称\t上游地址\t模型\t归属\t权重\t优先级\t凭证\t已删")
	} else {
		_, _ = fmt.Fprintln(tw, "ID\t状态\t类型\t名称\t上游地址\t模型\t归属\t权重\t优先级\t凭证")
	}
	for _, info := range infos {
		if withDeleted {
			_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
				info.ID, info.Status, info.Type, info.Name, info.BaseURL,
				strings.Join(info.Models, ","), ownerLabel(info.OwnerUserID),
				info.Weight, info.Priority, credentialKeysLabel(info),
				deletedLabel(info))
			continue
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			info.ID, info.Status, info.Type, info.Name, info.BaseURL,
			strings.Join(info.Models, ","), ownerLabel(info.OwnerUserID),
			info.Weight, info.Priority, credentialKeysLabel(info))
	}
	_ = tw.Flush()

	// 密文坏掉的行单独说明，不跳过也不整次报错（见 channels.Admin.List）：
	// 库里有一行坏数据时整个清单一条都看不到，而那正是最需要看清单的时候。
	for _, info := range infos {
		if info.CredentialErr != nil {
			_, _ = fmt.Fprintf(w, "渠道 %d 的凭证密文不合法：%v\n", info.ID, info.CredentialErr)
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
	if len(deletedRows) > 0 {
		_, _ = fmt.Fprintf(w, "渠道 %s 已删除：它们不再参与选路，也不能 update / enable / disable。\n",
			strings.Join(deletedRows, "、"))
	}

	// 带着 setting 的行单独点出来：那一栏在库里、在这张清单上都与一个生效的
	// 配置没有区别，而今天没有任何适配器读它（见 #360）。不新开一列是同一条
	// 理由——空列只是把噪音铺满每一行，而这里要说的只有「这几行配过东西」。
	// 只在**至少一行**带 setting 时打：没有这种行时，这一句在每次 list 上
	// 都是背景音。
	if note := channel.SettingUnreadNote(); note != "" {
		var rows []string
		for _, info := range infos {
			if len(info.Setting) > 0 {
				rows = append(rows, strconv.FormatInt(info.ID, 10))
			}
		}
		if len(rows) > 0 {
			_, _ = fmt.Fprintf(w, "渠道 %s 配了 setting：%s。\n", strings.Join(rows, "、"), note)
		}
	}
}

// deletedLabel 是 list --all 里「已删」那一栏。
//
// 没删的行留空，打一个占位符会让每一行都有东西、而读者要逐行分辨哪个占位符
// 表示「没有」；已删的行给出删除时刻，因为「什么时候删的」是事后核对时唯一
// 能把它与历史账单对上的一条线索。
func deletedLabel(info channels.ChannelInfo) string {
	if info.DeletedAt.IsZero() {
		return ""
	}
	return info.DeletedAt.UTC().Format(time.RFC3339)
}

// credentialKeysLabel 是清单里的「凭证」那一栏。
//
// 两样一起给：主密钥标识回答「挂在哪把钥匙下、轮换补到哪一步」，
// 末四位提示回答「挂的是我刚发的那个凭证吗」。它们是两个不同的问题，
// 只给其中一个都会让运维去别处再查一次。
func credentialKeysLabel(info channels.ChannelInfo) string {
	if info.CredentialErr != nil {
		// 上面会另起一行说明原因，这里只留一个显眼的标记。
		return "密文不合法"
	}
	keys := "-"
	if len(info.CredentialKeys) > 0 {
		keys = strings.Join(info.CredentialKeys, ",")
	}
	if info.Hint == "" {
		// 空提示只可能来自「这一列接上之前建的渠道」——接上之后每一次写入
		// 都会带上它。这里不补一个占位符，让「没有提示」与「提示是某几个
		// 字符」在清单上分得开：补一个 `…-` 会让两种情形长得一样。
		return keys
	}
	return keys + " …" + info.Hint
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
