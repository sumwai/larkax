package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/channels"
)

// accountHelp 是 account 的两版帮助文本（见 helpText）。
var accountHelp = helpText{short: accountShortUsage, long: accountUsage}

// accountShortUsage 是 account 的短版：`-h` / `--help` 与参数写错时打的那一份。
const accountShortUsage = `larkaxctl account 管理上游凭证账号。

用法：
  larkaxctl account add [<名称>] [--channel <渠道>] [--weight <权重>] [--credential-env <变量名>]
  larkaxctl account list [--channel <渠道>]
  larkaxctl account update <名称> [--credential-env <变量名>]
  larkaxctl account update <名称> --channel <渠道> --weight <权重>
  larkaxctl account remove <名称> [--force]

凭证默认从标准输入读；明文凭证不能写在命令行参数里。
add 的 stdout 只有账号名；没给 <名称> 时生成 account- 加六位随机串。
一条渠道可以挂零个或多个账号，零账号的渠道不参与选路。
update 换凭证，或者改这个账号在某条渠道上的权重；权重 0 表示先别用它。
remove 在账号还挂在多条渠道上时要加 --force。
更细的边界：larkaxctl help account。
`

// accountUsage 是 account 的长版：`larkaxctl help account` 与 `account help`。
const accountUsage = `larkaxctl account 管理上游凭证账号。

账号是一份凭证的独立实体。一条渠道通过关联挂零个、一个或多个账号：
同一家上游开多条渠道、用的又是同一把 key 时，改一次账号，所有挂着它的
渠道一起生效。零账号是合法的中间态——渠道建得出来、在 channel list 里
看得见，选路跳过它。

用法：
  larkaxctl account add [<名称>] [--channel <渠道>] [--weight <权重>] [--credential-env <变量名>]
  larkaxctl account list [--channel <渠道>]
  larkaxctl account update <名称> [--credential-env <变量名>]
  larkaxctl account update <名称> --channel <渠道> --weight <权重>
  larkaxctl account remove <名称> [--force]

**明文凭证只能从标准输入或环境变量读，不能写在命令行参数里。**命令行参数
会出现在 ps 的输出与 shell 历史里。

  --credential-env <变量名> 从名为 <变量名> 的环境变量读
  不给 --credential-env 时      从标准输入读到文件结束（末尾的换行会被去掉）。
                                在终端里跑时会先提示「粘贴凭证，Ctrl-D 结束」。

add 不给 <名称> 时生成 account- 加六位随机 [a-z0-9]，撞名自动重试。
**stdout 只有账号名**，其余信息走 stderr，于是可以这样接力：

  larkaxctl account add --channel deepseek-official < secret.txt

--channel 收渠道 ID 或未删除渠道的名字；同名渠道多于一条时报错，要求改用 ID。
它把新账号一步挂进那条渠道的凭证池。已有账号可以用 channel update 的
--credential-* 换 key，效果相同。

--weight 是池内权重：同一条渠道上，权重大的账号被选中的次数多。默认 1，
0 表示先别用它（权重 0 的账号不参与选路）。它挂在渠道与账号的关联上，
所以必须与 --channel 一起给；同一个账号挂在两条渠道上时，两条渠道上的权重
各算各的。没给 --weight 时是 1。

**暂停一个账号用权重 0，不要 remove 再 add**：remove 删的是账号本身，它的
健康记录（连败次数、最近结论）跟着一起消失；改权重只动那一条关联，账号、
凭证与它在别的渠道上的权重都不动。恢复就是把权重改回一个非 0 的数：

  larkaxctl account update deepseek-backup --channel deepseek-official --weight 0

一条渠道上多个账号时的选法：按权重轮询，跳过被上游拒绝过的账号与权重 0 的。
被摘掉的账号在冷却期之后会再被试一次，成功就回到池子里；换掉凭证
（account update）也会让它立刻回到池子里。轮到的那个账号密文解不开时，这一次
选路会接着试池里的下一个。

list 不给 --channel 时列出全部账号；给了 --channel 就只列挂在那条渠道上的
（渠道 ID 或未删除渠道的名字）。**它不打印凭证密文**，只打印挂在哪些主密钥下
（那不需要任何密钥材料）与末四位提示。没有「挂在哪条渠道上」的账号也列出来
——它可能正准备挂上去。

update 换凭证：挂在同一条账号上的渠道一个都不用动，下一次请求就用到新 key。
账号名与关联都不变，updated_at 记下换 key 的时刻。换 key 会把这个账号的健康
状态清回「没用过」，先前凭证被上游拒过、已经摘出池子的，改完立刻重新参与选路。

update 改权重：要同时给 --channel（渠道 ID 或未删除渠道的名字）与 --weight。
改的是那一条关联，同一个账号在别的渠道上的权重不受影响；健康状态不动——它是
一次「先别用」，不是一次故障。账号没挂在那条渠道上时报参数错（退出码 2），并
把它实际挂着的渠道列出来。两项都给时先改权重再换凭证：前者是唯一会因命令写错
而失败的一步，先做它就不会留下「凭证已经换了、这一步才报错」的半截状态。

remove 删掉账号本身，连同它在各条渠道上的关联。只挂在一条渠道上时直接删；
挂在多于一条渠道上时要加 --force——删一行会让每一条渠道各少一个账号，
而其中某条渠道可能因此一个账号都不剩（它会退出选路，直到挂上新账号）。
正在进行的请求不受影响：它们的凭证在选路时已经解密；新的请求不会再选到它。
账号行与它的健康状态一起消失，库里不留空壳。

更细的约定见 docs/cli-reference.md 的「account」一节。
`

// accountActions 是 account 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var accountActions = []string{"add", "list", "update", "remove"}

func runAccount(args []string) error {
	if len(args) == 0 {
		cli.PrintUsage(os.Stderr, accountShortUsage)
		return cli.UsageErr(errors.New("account 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runAccountAdd(rest)
	case "list":
		return runAccountList(rest)
	case "update":
		return runAccountUpdate(rest)
	case "remove":
		return runAccountRemove(rest)
	case "help":
		cli.PrintUsage(os.Stdout, accountHelp.long)
		return nil
	case "-h", "--help":
		cli.PrintUsage(os.Stdout, accountHelp.short)
		return nil
	default:
		cli.PrintUsage(os.Stderr, accountShortUsage)
		return cli.UsageErr(fmt.Errorf("未知的 account 子命令 %q", action))
	}
}

func runAccountAdd(args []string) error {
	if err := cli.JSONUnsupported("account add"); err != nil {
		return err
	}
	name, channelRef, weight, plaintext, err := parseAccountAdd(args)
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		sealer, err := masterKeySealer()
		if err != nil {
			return err
		}
		admin := channels.NewAdmin(db, sealer, nil)

		req := channels.CreateAccountRequest{
			Name:       name,
			Credential: plaintext,
			Weight:     weight,
		}
		if channelRef != "" {
			req.ChannelID, err = admin.ChannelIDByRef(ctx, channelRef)
			if err != nil {
				return err
			}
		}
		info, err := admin.CreateAccount(ctx, req)
		if err != nil {
			return err
		}

		// stdout 恰好一行、只放能被直接接走的值，与 channel create 同一条契约：
		//   larkaxctl account update "$(larkaxctl account add ...)"
		_, _ = fmt.Fprintln(cli.HumanStdout(), info.Name)
		// 其余信息走 stderr。挂在哪把主密钥下要当场说：轮换时「新写进去的
		// 密文挂在旧钥匙下」是一个需要立刻看见的事实。
		if err := info.CredentialErr; err != nil {
			_, _ = fmt.Fprintf(cli.HumanStderr(), "注意：账号 %s 的凭证密文不合法：%v\n", info.Name, err)
		}
		_, _ = fmt.Fprintf(cli.HumanStderr(), "已创建账号 %s（主密钥 %s）。\n",
			info.Name, strings.Join(info.CredentialKeys, ","))
		for _, ch := range info.Channels {
			_, _ = fmt.Fprintf(cli.HumanStderr(), "已挂到渠道 %d（%s）。\n", ch.ID, ch.Name)
		}
		// 与 account remove 的「这条渠道因此没有账号了」对称：权重 0 不会让
		// 渠道下线，但它同样意味着这次挂载不给池子增加任何可用账号。不说
		// 这一句，人会以为它已经在分担流量。
		if weight != nil && *weight == 0 {
			_, _ = fmt.Fprintln(cli.HumanStderr(),
				"注意：权重 0 表示先别用这个账号——关联已经建好，但选路会跳过它。")
		}
		return nil
	})
}

// parseAccountAdd 解析 add 的参数。
//
// 位置参数（账号名）是可选的，且只许一个。flag 包在第一个非选项参数处停下，
// 所以先把**开头**那个位置参数摘掉，再把剩下的交给 fs.Parse：
// `account add 名字 --channel c1` 与 `account add --channel c1` 都走得通。
// `--channel c1 名字` 这种「选项之后才给位置参数」也认——fs.Parse 停下之后
// fs.Args() 里剩下的那一个就是名字。
//
// weight 用指针区分「没给」与「给了 0」：0 是合法值（先别用它），缺省是 1。
func parseAccountAdd(args []string) (name, channelRef string, weight *int, plaintext string, err error) {
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}

	fs := cli.NewFlagSet("account add", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, accountShortUsage) }
	channel := fs.String("channel", "", "")
	credentialEnv := fs.String("credential-env", "", "")
	weightFlag := fs.Int("weight", 0, "")
	if err = fs.Parse(args); err != nil {
		return "", "", nil, "", err
	}
	if fs.NArg() > 0 {
		if positional != "" {
			return "", "", nil, "", fmt.Errorf(
				"add 只收一个账号名，多出来的 %q 没有去处", fs.Arg(0))
		}
		if fs.NArg() > 1 {
			return "", "", nil, "", fmt.Errorf(
				"add 只收一个账号名，多出来的 %q 没有去处", fs.Arg(1))
		}
		positional = fs.Arg(0)
	}

	name = strings.TrimSpace(positional)
	if name != "" {
		if err = cli.CheckDisplayText("账号名", name); err != nil {
			return "", "", nil, "", err
		}
	}
	channelRef = strings.TrimSpace(*channel)

	// 显式给过的 flag 才进 seen：fs.Int 的零值分不出「--weight 0」与「没给」。
	seen := make(map[string]bool, 3)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["weight"] {
		if *weightFlag < 0 {
			return "", "", nil, "", fmt.Errorf("--weight 不能为负，得到 %d", *weightFlag)
		}
		// 权重挂在渠道与账号的关联上，没有渠道就没有地方放它。替调用方猜一条
		// 渠道，猜错就是给错误的对象设了权重。
		if channelRef == "" {
			return "", "", nil, "", errors.New("--weight 挂在渠道与账号的关联上：要同时给 --channel")
		}
		w := *weightFlag
		weight = &w
	}

	plaintext, err = readAccountCredential(*credentialEnv)
	if err != nil {
		return "", "", nil, "", err
	}
	return name, channelRef, weight, plaintext, nil
}

func runAccountList(args []string) error {
	fs := cli.NewFlagSet("account list", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, accountShortUsage) }
	channelRef := fs.String("channel", "", "")
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		// 列举只解析信封头部，不解密正文：一台没有主密钥的只读机器也能核对配置。
		admin := channels.NewAdmin(db, nil, nil)
		var channelID int64
		if ref := strings.TrimSpace(*channelRef); ref != "" {
			id, err := admin.ChannelIDByRef(ctx, ref)
			if err != nil {
				return err
			}
			channelID = id
		}
		// 0 表示不过滤：ListAccounts 把「没给 --channel」当作列全部。
		infos, err := admin.ListAccounts(ctx, channelID)
		if err != nil {
			return err
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, accountListDoc(infos), nil)
		}
		printAccountList(cli.HumanStdout(), infos)
		return nil
	})
}

func runAccountUpdate(args []string) error {
	if err := cli.JSONUnsupported("account update"); err != nil {
		return err
	}
	parsed, err := parseAccountUpdate(args)
	if err != nil {
		return cli.UsageErr(err)
	}
	// 只改权重时不读标准输入：这条命令的缺省凭证来源是标准输入，而
	// `... --weight 0` 后面根本没有人在等着粘一把 key。读了它就卡在那儿。
	var plaintext string
	if parsed.setCredential {
		plaintext, err = readAccountCredential(parsed.credentialEnv)
		if err != nil {
			return cli.UsageErr(err)
		}
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		// 先改权重：它是唯一会因为「命令写错」而失败的一步（账号没挂在那条
		// 渠道上），先做它就不会留下「凭证已经换了、这一步才报参数错」的半截
		// 状态。两项都给时它们仍然是两件事，没有跨表的原子性可言。
		if parsed.weight != nil {
			if err := updateAccountWeight(ctx, db, parsed); err != nil {
				return err
			}
		}
		if !parsed.setCredential {
			return nil
		}

		sealer, err := masterKeySealer()
		if err != nil {
			return err
		}
		info, err := channels.NewAdmin(db, sealer, nil).UpdateAccount(ctx, parsed.name, plaintext)
		if err != nil {
			return err
		}
		// stdout 不放回显：要改的是哪一条由调用方自己给，没有新值要接走。
		_, _ = fmt.Fprintf(cli.HumanStderr(), "已更新账号 %s（挂在 %d 条渠道上）。\n",
			info.Name, len(info.Channels))
		return nil
	})
}

// updateAccountWeight 把 --weight 写进「渠道 × 账号」那一条关联。
//
// 渠道引用在写入之前解析一次：认不出来时给的是渠道那一侧的错（名字多余一条、
// 不存在），与 add 走同一条路。
func updateAccountWeight(ctx context.Context, db *sql.DB, args accountUpdateArgs) error {
	admin := channels.NewAdmin(db, nil, nil)
	channelID, err := admin.ChannelIDByRef(ctx, args.channelRef)
	if err != nil {
		return err
	}
	channel, err := admin.SetAccountWeight(ctx, args.name, channelID, *args.weight)
	if err != nil {
		// 账号没挂在那条渠道上：补救动作是改命令（换一条渠道，或者换一个
		// 账号），与 remove 少写 --force 同一档。
		if errors.Is(err, channels.ErrAccountNotOnChannel) {
			return cli.UsageErr(err)
		}
		return err
	}
	_, _ = fmt.Fprintf(cli.HumanStderr(), "已把账号 %s 在渠道 %d（%s）上的权重设为 %d。\n",
		args.name, channel.ID, channel.Name, channel.Weight)
	if channel.Weight == 0 {
		_, _ = fmt.Fprintf(cli.HumanStderr(),
			"权重 0 表示先别用：这条渠道的选路会跳过它，账号、凭证与健康记录都留着。"+
				"要恢复就把权重改回非 0。\n")
	}
	return nil
}

// runAccountRemove 摘掉一个账号。
//
// 三件事在实现里分得开：
//   - **还挂在别的渠道上**：多于一条时要 --force（ErrAccountInUse）。少一条
//     渠道各少一个账号是调用方看得见的后果，不该由它悄悄发生。
//   - **正在进行的请求**：不拦。凭证在选路那一刻就解密并带在请求里，删行只
//     影响之后的新请求；这里既没有锁也没有等待。
//   - **渠道因此空了**：删完把那些渠道打出来。零账号是合法的中间态，但
//     「删掉一个账号」与「有一条渠道从此退出选路」不是同一个后果。
func runAccountRemove(args []string) error {
	if err := cli.JSONUnsupported("account remove"); err != nil {
		return err
	}
	name, force, err := parseAccountRemove(args)
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		res, err := channels.NewAdmin(db, nil, nil).RemoveAccount(ctx, name, force)
		if err != nil {
			// 还挂在多条渠道上时，补救动作是改命令加 --force：归 usageErr。
			if errors.Is(err, channels.ErrAccountInUse) {
				return cli.UsageErr(err)
			}
			return err
		}
		// stdout 不放东西：删掉的是调用方自己点的名字，没有新值要接走。
		_, _ = fmt.Fprintf(cli.HumanStderr(), "已删除账号 %s。\n", res.Name)
		if len(res.Channels) > 0 {
			_, _ = fmt.Fprintf(cli.HumanStderr(), "已从这 %d 条渠道的凭证池里摘掉：%s。\n",
				len(res.Channels), accountChannelList(res.Channels))
		}
		for _, ch := range res.EmptiedChannels {
			_, _ = fmt.Fprintf(cli.HumanStderr(),
				"渠道 %d（%s）现在没有账号了，选路会跳过它，直到挂上新账号。\n", ch.ID, ch.Name)
		}
		return nil
	})
}

// parseAccountRemove 解析 remove 的参数：一个必填的账号名与 --force。
func parseAccountRemove(args []string) (name string, force bool, err error) {
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}

	fs := cli.NewFlagSet("account remove", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, accountShortUsage) }
	forceFlag := fs.Bool("force", false, "")
	if err = fs.Parse(args); err != nil {
		return "", false, err
	}
	switch {
	case fs.NArg() == 0:
	case fs.NArg() == 1 && positional == "":
		positional = fs.Arg(0)
	default:
		return "", false, fmt.Errorf("remove 只收一个账号名，多出来的 %q 没有去处", fs.Arg(0))
	}
	name = strings.TrimSpace(positional)
	if name == "" {
		return "", false, errors.New("缺少账号名：把它写成位置参数")
	}
	if err = cli.CheckDisplayText("账号名", name); err != nil {
		return "", false, err
	}
	return name, *forceFlag, nil
}

// accountChannelList 把渠道清单拼成「12（主）、13（备用）」。
func accountChannelList(chs []channels.AccountChannel) string {
	parts := make([]string, 0, len(chs))
	for _, ch := range chs {
		parts = append(parts, fmt.Sprintf("%d（%s）", ch.ID, ch.Name))
	}
	return strings.Join(parts, "、")
}

// accountUpdateArgs 是 `account update` 解析出来的两件事：换凭证、改权重。
//
// 它们各自可以缺席，但不会同时缺席：一个都不给时按老行为从标准输入读凭证
// （见 setCredential）。
type accountUpdateArgs struct {
	name string
	// credentialEnv 是 --credential-env 的值；空串表示从标准输入读。
	credentialEnv string
	// setCredential 为 false 表示这次不换凭证，只改权重。
	setCredential bool
	// channelRef 与 weight 一起给：权重挂在关联上，没有渠道就没有地方放。
	channelRef string
	weight     *int
}

// parseAccountUpdate 解析 update 的参数：一个必填的账号名，加上换凭证或改权重。
//
// 三种组合各走各的路：
//   - 不给 --weight：老行为，换凭证，凭证从 --credential-env 或标准输入读；
//   - 给 --weight：必须同时给 --channel（见下），**不读标准输入**；
//   - 两个都给：两件事都做。
//
// --weight 用指针区分「没给」与「给了 0」：0 是合法值（先别用它）。给了
// --channel 却不给 --weight 是参数错而不是空操作——那多半是把两条命令拼错了，
// 悄悄放过它，人会以为权重已经改过了。
func parseAccountUpdate(args []string) (accountUpdateArgs, error) {
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}

	fs := cli.NewFlagSet("account update", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, accountShortUsage) }
	credentialEnv := fs.String("credential-env", "", "")
	channel := fs.String("channel", "", "")
	weightFlag := fs.Int("weight", 0, "")
	if err := fs.Parse(args); err != nil {
		return accountUpdateArgs{}, err
	}
	switch {
	case fs.NArg() == 0:
	case fs.NArg() == 1 && positional == "":
		positional = fs.Arg(0)
	default:
		return accountUpdateArgs{}, fmt.Errorf("update 只收一个账号名，多出来的 %q 没有去处", fs.Arg(0))
	}
	name := strings.TrimSpace(positional)
	if name == "" {
		return accountUpdateArgs{}, errors.New("缺少账号名：把它写成位置参数")
	}
	if err := cli.CheckDisplayText("账号名", name); err != nil {
		return accountUpdateArgs{}, err
	}

	// 显式给过的 flag 才进 seen：fs.Int 的零值分不出「--weight 0」与「没给」。
	seen := make(map[string]bool, 3)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })

	out := accountUpdateArgs{
		name:          name,
		credentialEnv: strings.TrimSpace(*credentialEnv),
		channelRef:    strings.TrimSpace(*channel),
	}
	if seen["weight"] {
		if *weightFlag < 0 {
			return accountUpdateArgs{}, fmt.Errorf("--weight 不能为负，得到 %d", *weightFlag)
		}
		// 权重挂在渠道与账号的关联上，没有渠道就没有地方放它。替调用方猜一条
		// 渠道，猜错就是给错误的对象设了权重。
		if out.channelRef == "" {
			return accountUpdateArgs{}, errors.New(
				"--weight 挂在渠道与账号的关联上：要同时给 --channel")
		}
		w := *weightFlag
		out.weight = &w
	} else if out.channelRef != "" {
		return accountUpdateArgs{}, errors.New("--channel 只与 --weight 一起用：单独给它没有要改的东西")
	}

	// 不给 --weight 时保持老行为：换凭证，凭证从标准输入读。不设这一条的话，
	// `account update <名称>` 会变成一次什么都没有做的成功。
	out.setCredential = !seen["weight"] || seen["credential-env"]
	return out, nil
}

// readAccountCredential 读明文凭证。
//
// 与 channel 路径的 readCredential 是两条入口、同一套规矩：没有命令行参数
// 这一条路，因为参数会出现在 ps 与 shell 历史里。区别只在**缺省来源**——
// 账号命令把标准输入当作缺省（issue 的用法就是 `... | larkaxctl account add`），
// 而 channel 需要显式给 --credential-stdin。
func readAccountCredential(envName string) (string, error) {
	if envName != "" {
		value := os.Getenv(envName)
		if value == "" {
			// 只说变量名，不说值——值本身就不该出现在任何输出里。
			return "", fmt.Errorf("环境变量 %s 未设置或为空", envName)
		}
		return value, nil
	}

	// 终端里要提示一句，否则命令看起来像卡住了；管道里不能提示，那一行会
	// 混进下游的输出。判的是 os.Stdin 本身，测试替换 credentialInput 之后
	// 就不该再提示。
	if credentialInput == os.Stdin && stdinIsTerminal() {
		_, _ = fmt.Fprint(cli.HumanStderr(), "粘贴凭证，Ctrl-D 结束：\n")
	}
	raw, err := io.ReadAll(credentialInput)
	if err != nil {
		return "", fmt.Errorf("读标准输入失败: %w", err)
	}
	// 只去掉末尾的换行：`echo "$SECRET" | ...` 会多带一个，而凭证本身几乎
	// 不可能以换行结尾。不去掉的话，写进去的密文与手里那把 key 差一个字节。
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", errors.New("标准输入里没有凭证")
	}
	return value, nil
}

// stdinIsTerminal 判断标准输入是不是终端。
//
// 用 ModeCharDevice 而不是引入一个终端库：这里要的只是「该不该打那句提示」，
// 判错的代价是一行提示出现或消失，不是任何功能。
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// printAccountList 打账号清单。
func printAccountList(w io.Writer, infos []channels.AccountInfo) {
	if len(infos) == 0 {
		// 空清单与「命令没跑起来」在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
		_, _ = fmt.Fprintln(w, cli.EmptyListNote("账号", "account add"))
		return
	}

	tw := cli.NewTableWriter(w)
	p := cli.NewPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		p.Bold("名称"), p.Bold("提示"), p.Bold("主密钥"), p.Bold("渠道"), p.Bold("更新于"))
	for _, info := range infos {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.Bold(info.Name),
			accountHintLabel(info),
			accountKeysLabel(info),
			accountChannelsLabel(info),
			p.Dim(formatTimeValue(info.UpdatedAt)))
	}
	_ = tw.Flush()

	// 密文坏掉的账号单独说明，不跳过也不整次报错：与 channel list 同一处置。
	// 库里有一行坏数据时整个清单一条都看不到，而那正是最需要看清单的时候。
	for _, info := range infos {
		if info.CredentialErr != nil {
			_, _ = fmt.Fprintf(w, "账号 %s 的凭证密文不合法：%v\n", info.Name, info.CredentialErr)
		}
	}
}

func accountHintLabel(info channels.AccountInfo) string {
	if info.Hint == "" {
		return "-"
	}
	return "…" + info.Hint
}

func accountKeysLabel(info channels.AccountInfo) string {
	if info.CredentialErr != nil {
		return "密文不合法"
	}
	if len(info.CredentialKeys) == 0 {
		return "-"
	}
	return strings.Join(info.CredentialKeys, ",")
}

// accountChannelsLabel 打这一行挂着的渠道，每条都带上它在关联上的权重。
//
// 权重 0 是「这条渠道先别用这个账号」的暂停开关，人读表里要能一眼看见它，
// 不必切到 `--format json`。默认值 1 也照打：这一栏回答的是「这个账号在每条
// 渠道上是什么权重」，那个数就是答案本身，省掉 1 等于把「改完看不到结果」
// 从 0 挪到了 1。
//
// 形态与 channel.go 的 accountRefsLabel 相同，规则不同——那边回答「这条渠道
// 挂了谁」，权重只是附注，默认值可以不写；不要为了「看着统一」把两处改成
// 同一条规则，无论统一到哪边都会改坏一侧。
//
// 没有关联时打「-」：「没有这个数」与「这个数是 0」在选路里是相反的结论，
// 不能在这里合并成一个空值。
func accountChannelsLabel(info channels.AccountInfo) string {
	if len(info.Channels) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(info.Channels))
	for _, ch := range info.Channels {
		parts = append(parts, fmt.Sprintf("%s（权重 %d）", ch.Name, ch.Weight))
	}
	return strings.Join(parts, ", ")
}

// accountListDoc 组装 account list 的机器可读输出。
//
// 与 printAccountList 是同一份数据的另一条渲染路径：那一档把缺值铺成「-」，
// 这一档给 null。**这一份里没有、也不会有凭证密文**：AccountInfo 根本没有
// 那一栏，这里能给的只有「挂在哪些主密钥下」与末四位提示。
func accountListDoc(infos []channels.AccountInfo) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["summary"] = map[string]any{"total": len(infos)}

	// 从空切片起：一个账号都没有时打 `[]`，不是 `null`（见 cli 包的 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, accountEntry(info))
	}
	doc["accounts"] = entries
	return doc
}

func accountEntry(info channels.AccountInfo) map[string]any {
	channelsJSON := make([]map[string]any, 0, len(info.Channels))
	for _, ch := range info.Channels {
		channelsJSON = append(channelsJSON, map[string]any{
			"id":     ch.ID,
			"name":   ch.Name,
			"weight": ch.Weight,
		})
	}
	return map[string]any{
		"id":               info.ID,
		"name":             info.Name,
		"hint":             cli.NullableString(info.Hint),
		"credential_keys":  cli.JSONList(info.CredentialKeys),
		"credential_error": cli.NullableError(info.CredentialErr),
		"channels":         channelsJSON,
		"created_at":       cli.JSONTimeOrNull(info.CreatedAt),
		"updated_at":       cli.JSONTimeOrNull(info.UpdatedAt),
	}
}
