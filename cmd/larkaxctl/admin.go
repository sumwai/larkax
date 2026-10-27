package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/adminapi"
	"github.com/sumwai/larkax/internal/config"
)

// adminHelp 是 admin 的两版帮助文本（见 helpText）。
var adminHelp = helpText{short: adminShortUsage, long: adminUsage}

// adminShortUsage 是 admin 的短版：`-h` / `--help` 与参数写错时打的那一份。
const adminShortUsage = `larkaxctl admin 管理内部监听上 admin API 的凭证。

用法：
  larkaxctl admin token create    生成一枚新令牌（明文只在这里打印一次）
  larkaxctl admin token revoke    撤销当前令牌

令牌单独一套，不复用用户 API Key。生成之后在请求里带
Authorization: Bearer <令牌>；没配置令牌时 admin API 一律回 503。

更细的边界：larkaxctl help admin。
`

// adminUsage 是 admin 的长版：`larkaxctl help admin` 与 `admin help`。
const adminUsage = `larkaxctl admin 管理内部监听上 admin API 的凭证。

用法：
  larkaxctl admin token create
  larkaxctl admin token revoke

管理面（/admin/v1/*）挂在**内部监听**上，改的是渠道、模型目录与计价规则。
它用的是单独一套令牌，不复用用户 API Key：那把 Key 的语义是「以某个用户
的名义发推理请求」，权限范围与持有者都不是同一件事。

create 生成 32 字节随机令牌（base64url），**只在这一次打印明文**。库里只存
它的 SHA-256，没有任何路径能从哈希反推回来；丢了就只能重新生成。新令牌
立刻替换旧令牌：不需要先撤销，也不会有一段两枚都有效的窗口。

撤销之后，任何令牌都匹配不上（请求得到 401）。它与「从来没配置过令牌」是
两件事：后者回 503，提示先去 create；那时 revoke 也不会把空串写成哨兵，
而是直接报错让你先创建一枚。那一行不会被删掉——settings 要求每个声明过的
键都有一行，删掉会让下一次启动报「缺少配置项」。

令牌在库里明文的只有哈希，所以 settings list 看得到那一行也无所谓——它不是
凭证。不要把它复制到别处当作令牌使用。

改完下一次请求就生效，不必重启服务。

更细的约定见 docs/admin-api.md 与 docs/cli-reference.md 的「admin」一节。
`

// adminTimeout 是这两条命令连库的时间上限。
//
// 与 settingsTimeout 分开写：它们将来可能各调各的，共用一个是「碰巧现在一样」
// 的耦合。
const adminTimeout = 30 * time.Second

// adminActions 是 admin 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var adminActions = []string{"token"}

func runAdmin(args []string) error {
	if len(args) == 0 {
		// 缺子命令才打整段用法：这时人还没开始用，用法正是他要的。
		cli.PrintUsage(os.Stderr, adminShortUsage)
		return cli.UsageErr(errors.New("admin 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "token":
		return runAdminToken(rest)
	case "-h", "--help":
		cli.PrintUsage(os.Stdout, adminShortUsage)
		return nil
	default:
		cli.PrintUsage(os.Stderr, adminShortUsage)
		return cli.UsageErr(fmt.Errorf("未知的 admin 子命令 %q", action))
	}
}

// adminTokenActions 是 admin token 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var adminTokenActions = []string{"create", "revoke"}

func runAdminToken(args []string) error {
	if len(args) == 0 {
		cli.PrintUsage(os.Stderr, adminShortUsage)
		return cli.UsageErr(errors.New("admin token 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create":
		return runAdminTokenCreate(rest)
	case "revoke":
		return runAdminTokenRevoke(rest)
	case "-h", "--help":
		cli.PrintUsage(os.Stdout, adminShortUsage)
		return nil
	case "help":
		// admin token 由 admin 拆出来，读者要的仍是 admin 那一份里关于令牌的
		// 那几行：这一组不另写文案，与「帮助文案只有一处来源」一致。
		cli.PrintUsage(os.Stdout, adminUsage)
		return nil
	default:
		cli.PrintUsage(os.Stderr, adminShortUsage)
		return cli.UsageErr(fmt.Errorf("未知的 admin token 子命令 %q", action))
	}
}

func runAdminTokenCreate(args []string) error {
	if err := cli.JSONUnsupported("admin token create"); err != nil {
		return err
	}
	fs := cli.NewFlagSet("admin token create", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, adminShortUsage) }
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("create 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	store, closeDB, err := openTokenStore(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	token, err := store.Create(ctx)
	if err != nil {
		return err
	}
	printAdminToken(cli.HumanStdout(), cli.HumanStderr(), token)
	return nil
}

func runAdminTokenRevoke(args []string) error {
	if err := cli.JSONUnsupported("admin token revoke"); err != nil {
		return err
	}
	fs := cli.NewFlagSet("admin token revoke", flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, adminShortUsage) }
	if err := fs.Parse(args); err != nil {
		return cli.UsageErr(err)
	}
	if fs.NArg() > 0 {
		return cli.UsageErr(fmt.Errorf("revoke 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	store, closeDB, err := openTokenStore(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	if err := store.Revoke(ctx); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cli.HumanStdout(), "已撤销管理令牌：从现在起，带旧令牌的请求一律 401。")
	return nil
}

// openTokenStore 连库并返回一个令牌存储；closeDB 由调用方 defer。
//
// 这一段在两条命令里逐字相同：抄两份的话，改超时或改错误包装时只有一处会
// 跟着改。
func openTokenStore(ctx context.Context) (*adminapi.TokenStore, func(), error) {
	cfg, err := config.LoadOps()
	if err != nil {
		return nil, nil, err
	}
	db, err := cli.OpenDB(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return adminapi.NewTokenStore(db), func() { _ = db.Close() }, nil
}

// printAdminToken 打印新令牌。
//
// stdout 只有一行：令牌本身，**不加任何前缀或说明**。它的用途是被接走，
// 例如 `larkaxctl admin token create | head -1` 之后交给别处；混进一句话
// 就得多剥一层。其余说明走 stderr，与 channel / user create 的回显同一条
// 契约（stdout 只放能被直接接走的值）。
//
// 令牌加粗、说明置灰：这是这一页唯一需要看清的那一行，而它只在这一次出现。
func printAdminToken(stdout, stderr io.Writer, token string) {
	p := cli.NewPalette(stdout)
	_, _ = fmt.Fprintln(stdout, p.Bold(token))
	_, _ = fmt.Fprintln(stderr, "这是这枚令牌唯一的明文：库里只存它的 SHA-256，丢了只能重新生成。")
	_, _ = fmt.Fprintln(stderr, "请求里带 Authorization: Bearer <令牌>；撤销用 larkaxctl admin token revoke。")
}
