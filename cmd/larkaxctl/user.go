package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/config"
)

const userUsage = `larkaxctl user 管理用户。

用法：
  larkaxctl user create --name <用户名> [--email <邮箱>] [--role <角色>]

create 建一个新用户，只把新用户的 ID 打到 stdout。

角色取 user、merchant、admin 之一，默认 user。三者都是用户，
merchant 与 admin 只是 users 表上的标记（admin 同时也是 merchant）。

**角色今天只是记下来，不改任何一处行为**：全仓没有代码读那两个标记，
所以 --role merchant 不会让这个用户多出什么权限，--role admin 也不代表
他能进管理接口（管理面还没做）。它们留给管理面的鉴权用，接上之前别把它
当成一条访问控制。

**不做幂等**：用户名已被占用时直接报错，不会改到已有的那个用户。

`

// userTimeout 是这条命令连库与写库的时间上限。
//
// 与 apiKeyTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。两个常量各写各的，是因为它们将来可能各调各的——
// 共用一个是「碰巧现在一样」的耦合。
const userTimeout = 30 * time.Second

func runUser(args []string) error {
	fs := flag.NewFlagSet("user", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, userUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageErr(errors.New("user 需要子命令"))
	}
	action, rest := rest[0], rest[1:]

	switch action {
	case "create":
		return runUserCreate(rest)
	default:
		return usageErr(fmt.Errorf("未知的 user 子命令 %q", action))
	}
}

func runUserCreate(args []string) error {
	if err := jsonUnsupported("user create"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, userUsage) }
	name := fs.String("name", "", "用户名，唯一（大小写不敏感）")
	email := fs.String("email", "", "邮箱，留空表示不填")
	role := fs.String("role", string(auth.RoleUser), "角色：user、merchant、admin")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("create 不接受位置参数，得到 %q：用户名用 --name 指定", fs.Arg(0)))
	}

	// 参数检查全部发生在连库之前（与 apikey create 同一条约定）：
	// 一个写错的命令不该先连上生产库再报错，而「连上库之后才发现参数不对」
	// 还有一个更坏的可能——它没报错。
	if strings.TrimSpace(*name) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --name"))
	}
	parsedRole, err := auth.ParseRole(*role)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), userTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	created, err := auth.New(db).CreateUser(ctx, auth.CreateUserParams{
		Username: *name,
		Email:    *email,
		Role:     parsedRole,
	})
	if err != nil {
		return err
	}

	printCreatedUser(os.Stdout, os.Stderr, created)
	return nil
}

// printCreatedUser 打印新用户。
//
// 与 printCreatedKey 同一条契约：**stdout 恰好一行，只放能被直接接走的那个值**，
// 其余信息走 stderr。这里要接走的是 ID——
//
//	larkaxctl apikey create --user "$(larkaxctl user create --name alice)"
//
// 是造完用户之后最自然的下一步。那一行里多一个前缀或一句提示语，这条链就断了，
// 而断了的表现是 apikey create 收到一个不是用户标识的字符串，报「找不到用户」。
func printCreatedUser(stdout, stderr io.Writer, u auth.User) {
	_, _ = fmt.Fprintf(stderr, "已创建用户 %s(id=%d)\n", u.Username, u.ID)
	_, _ = fmt.Fprintln(stdout, u.ID)
}
