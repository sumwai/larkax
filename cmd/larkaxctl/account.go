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

	"github.com/sumwai/larkax/internal/channels"
)

// accountHelp 是 account 的两版帮助文本（见 helpText）。
var accountHelp = helpText{short: accountShortUsage, long: accountUsage}

// accountShortUsage 是 account 的短版：`-h` / `--help` 与参数写错时打的那一份。
const accountShortUsage = `larkaxctl account 管理上游凭证账号。

用法：
  larkaxctl account add [<名称>] [--channel <渠道>] [--credential-env <变量名>]
  larkaxctl account list
  larkaxctl account update <名称> [--credential-env <变量名>]

凭证默认从标准输入读；明文凭证不能写在命令行参数里。
add 的 stdout 只有账号名；没给 <名称> 时生成 account- 加六位随机串。
一条渠道可以挂零个或多个账号，零账号的渠道不参与选路。
更细的边界：larkaxctl help account。
`

// accountUsage 是 account 的长版：`larkaxctl help account` 与 `account help`。
const accountUsage = `larkaxctl account 管理上游凭证账号。

账号是一份凭证的独立实体。一条渠道通过关联挂零个、一个或多个账号：
同一家上游开多条渠道、用的又是同一把 key 时，改一次账号，所有挂着它的
渠道一起生效。零账号是合法的中间态——渠道建得出来、在 channel list 里
看得见，选路跳过它。

用法：
  larkaxctl account add [<名称>] [--channel <渠道>] [--credential-env <变量名>]
  larkaxctl account list
  larkaxctl account update <名称> [--credential-env <变量名>]

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

update 只换凭证：挂在同一条账号上的渠道一个都不用动，下一次请求就用到新 key。
账号名与关联都不变，updated_at 记下换 key 的时刻。

list **不打印凭证密文**，只打印它挂在哪些主密钥下（那不需要任何密钥材料）
与末四位提示。没有「挂在哪条渠道上」的账号也列出来——它可能正准备挂上去。

更细的约定见 docs/cli-reference.md 的「account」一节。
`

// accountActions 是 account 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var accountActions = []string{"add", "list", "update"}

func runAccount(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, accountShortUsage)
		return usageErr(errors.New("account 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runAccountAdd(rest)
	case "list":
		return runAccountList(rest)
	case "update":
		return runAccountUpdate(rest)
	case "help":
		printUsage(os.Stdout, accountHelp.long)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, accountHelp.short)
		return nil
	default:
		printUsage(os.Stderr, accountShortUsage)
		return usageErr(fmt.Errorf("未知的 account 子命令 %q", action))
	}
}

func runAccountAdd(args []string) error {
	if err := jsonUnsupported("account add"); err != nil {
		return err
	}
	name, channelRef, plaintext, err := parseAccountAdd(args)
	if err != nil {
		return usageErr(err)
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
		_, _ = fmt.Fprintln(humanStdout(), info.Name)
		// 其余信息走 stderr。挂在哪把主密钥下要当场说：轮换时「新写进去的
		// 密文挂在旧钥匙下」是一个需要立刻看见的事实。
		if err := info.CredentialErr; err != nil {
			_, _ = fmt.Fprintf(humanStderr(), "注意：账号 %s 的凭证密文不合法：%v\n", info.Name, err)
		}
		_, _ = fmt.Fprintf(humanStderr(), "已创建账号 %s（主密钥 %s）。\n",
			info.Name, strings.Join(info.CredentialKeys, ","))
		for _, ch := range info.Channels {
			_, _ = fmt.Fprintf(humanStderr(), "已挂到渠道 %d（%s）。\n", ch.ID, ch.Name)
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
func parseAccountAdd(args []string) (name, channelRef, plaintext string, err error) {
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}

	fs := newFlagSet("account add", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, accountShortUsage) }
	channel := fs.String("channel", "", "")
	credentialEnv := fs.String("credential-env", "", "")
	if err = fs.Parse(args); err != nil {
		return "", "", "", err
	}
	if fs.NArg() > 0 {
		if positional != "" {
			return "", "", "", fmt.Errorf(
				"add 只收一个账号名，多出来的 %q 没有去处", fs.Arg(0))
		}
		if fs.NArg() > 1 {
			return "", "", "", fmt.Errorf(
				"add 只收一个账号名，多出来的 %q 没有去处", fs.Arg(1))
		}
		positional = fs.Arg(0)
	}

	name = strings.TrimSpace(positional)
	if name != "" {
		if err = checkDisplayText("账号名", name); err != nil {
			return "", "", "", err
		}
	}
	plaintext, err = readAccountCredential(*credentialEnv)
	if err != nil {
		return "", "", "", err
	}
	return name, strings.TrimSpace(*channel), plaintext, nil
}

func runAccountList(args []string) error {
	fs := newFlagSet("account list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, accountShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		// 列举只解析信封头部，不解密正文：一台没有主密钥的只读机器也能核对配置。
		infos, err := channels.NewAdmin(db, nil, nil).ListAccounts(ctx)
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, accountListDoc(infos), nil)
		}
		printAccountList(humanStdout(), infos)
		return nil
	})
}

func runAccountUpdate(args []string) error {
	if err := jsonUnsupported("account update"); err != nil {
		return err
	}
	name, envName, err := parseAccountUpdate(args)
	if err != nil {
		return usageErr(err)
	}
	plaintext, err := readAccountCredential(envName)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		sealer, err := masterKeySealer()
		if err != nil {
			return err
		}
		info, err := channels.NewAdmin(db, sealer, nil).UpdateAccount(ctx, name, plaintext)
		if err != nil {
			return err
		}
		// stdout 不放回显：要改的是哪一条由调用方自己给，没有新值要接走。
		_, _ = fmt.Fprintf(humanStderr(), "已更新账号 %s（挂在 %d 条渠道上）。\n",
			info.Name, len(info.Channels))
		return nil
	})
}

// parseAccountUpdate 解析 update 的参数：一个必填的账号名与可选的凭证来源。
func parseAccountUpdate(args []string) (name, envName string, err error) {
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}

	fs := newFlagSet("account update", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, accountShortUsage) }
	credentialEnv := fs.String("credential-env", "", "")
	if err = fs.Parse(args); err != nil {
		return "", "", err
	}
	switch {
	case fs.NArg() == 0:
	case fs.NArg() == 1 && positional == "":
		positional = fs.Arg(0)
	default:
		return "", "", fmt.Errorf("update 只收一个账号名，多出来的 %q 没有去处", fs.Arg(0))
	}
	name = strings.TrimSpace(positional)
	if name == "" {
		return "", "", errors.New("缺少账号名：把它写成位置参数")
	}
	if err = checkDisplayText("账号名", name); err != nil {
		return "", "", err
	}
	return name, strings.TrimSpace(*credentialEnv), nil
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
		_, _ = fmt.Fprint(humanStderr(), "粘贴凭证，Ctrl-D 结束：\n")
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
		_, _ = fmt.Fprintln(w, emptyListNote("账号", "account add"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		p.bold("名称"), p.bold("提示"), p.bold("主密钥"), p.bold("渠道"), p.bold("更新于"))
	for _, info := range infos {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.bold(info.Name),
			accountHintLabel(info),
			accountKeysLabel(info),
			accountChannelsLabel(info),
			p.dim(formatTimeValue(info.UpdatedAt)))
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

func accountChannelsLabel(info channels.AccountInfo) string {
	if len(info.Channels) == 0 {
		return "-"
	}
	names := make([]string, 0, len(info.Channels))
	for _, ch := range info.Channels {
		names = append(names, ch.Name)
	}
	return strings.Join(names, ",")
}

// accountListDoc 组装 account list 的机器可读输出。
//
// 与 printAccountList 是同一份数据的另一条渲染路径：那一档把缺值铺成「-」，
// 这一档给 null。**这一份里没有、也不会有凭证密文**：AccountInfo 根本没有
// 那一栏，这里能给的只有「挂在哪些主密钥下」与末四位提示。
func accountListDoc(infos []channels.AccountInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(infos)}

	// 从空切片起：一个账号都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
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
		"hint":             nullableString(info.Hint),
		"credential_keys":  jsonList(info.CredentialKeys),
		"credential_error": nullableError(info.CredentialErr),
		"channels":         channelsJSON,
		"created_at":       jsonTimeOrNull(info.CreatedAt),
		"updated_at":       jsonTimeOrNull(info.UpdatedAt),
	}
}
