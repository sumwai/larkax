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
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/config"
)

// userHelp 是 user 的两版帮助文本（见 helpText）。
var userHelp = helpText{short: userShortUsage, long: userUsage}

// userShortUsage 是 user 的短版：`-h` / `--help` 与参数写错时打的那一份。
const userShortUsage = `larkaxctl user 管理用户。

用法：
  larkaxctl user create --name <用户名> [--email <邮箱>] [--role <角色>]
  larkaxctl user list [--status <状态>] [--role <角色>]
  larkaxctl user get --user <ID 或用户名>

create 只把新用户的 ID 打到 stdout；用户名已被占用时直接报错，不改已有的用户。
list 与 get 只读，不需要 LARKAX_MASTER_KEYS。

更细的边界：larkaxctl help user。
`

// userUsage 是 user 的长版：`larkaxctl help user` 与 `user help`。
const userUsage = `larkaxctl user 管理用户。

用法：
  larkaxctl user create --name <用户名> [--email <邮箱>] [--role <角色>]
  larkaxctl user list [--status <状态>] [--role <角色>]
  larkaxctl user get --user <ID 或用户名>

create 建一个新用户，只把新用户的 ID 打到 stdout。

角色取 user、merchant、admin 之一，默认 user。三者都是用户，merchant 与 admin
只是 users 表上的标记（admin 同时也是 merchant）。

**角色今天只是记下来，不改任何一处行为**：在后端的鉴权接上之前，--role merchant
不会让这个用户多出什么权限，--role admin 也不代表他能进管理接口。别把 --role
当成一条访问控制。

**不做幂等**：用户名已被占用时直接报错，不会改到已有的那个用户。

list 按 id 从小到大列出用户，一栏一个字段：ID、用户名、状态、角色、邮箱。

两个筛选项都可以同时给，也可以都不给（不给就是列全部）：

  --status  取 active、suspended、banned 之一。
  --role    取 user、merchant、admin 之一，按**称呼**逐字匹配，与 create 写进去的
            那一个对应：--role merchant 列出的是称呼为 merchant 的用户，
            --role admin 列的是 admin（admin 同时也是 merchant，但两者在这里
            是两个不同的取值）。

状态是鉴权时真正被检查的那一列（suspended 与 banned 都让他手里的 key 全部失效），
**今天没有任何命令能改它**——这一侧只能读。

get 按 ID 或用户名读一个用户的全部字段：除上面那几栏，还有展示名、创建时间与
更新时间。标识怎么解释与别的命令一致：纯数字按 ID，否则按用户名。

这两条命令都不需要 LARKAX_MASTER_KEYS，也不改任何一行。

更细的约定见 docs/cli-reference.md 的「user」一节。
`

// userTimeout 是这条命令连库与写库的时间上限。
//
// 与 apiKeyTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到
// 人发现为止。两个常量各写各的，是因为它们将来可能各调各的——
// 共用一个是「碰巧现在一样」的耦合。
const userTimeout = 30 * time.Second

func runUser(args []string) error {
	fs := flag.NewFlagSet("user", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
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
	case "list":
		return runUserList(rest)
	case "get":
		return runUserGet(rest)
	default:
		return usageErr(fmt.Errorf("未知的 user 子命令 %q", action))
	}
}

func runUserCreate(args []string) error {
	if err := jsonUnsupported("user create"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	name := fs.String("name", "", "")
	email := fs.String("email", "", "")
	role := fs.String("role", string(auth.RoleUser), "")
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
		// 不打整段用法：人已经会用这条命令了，只是漏了一个必填项。
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

	printCreatedUser(humanStdout(), humanStderr(), created)
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
//
// stderr 那一行只说名字：ID 已经是被接走的那个值，再在名字后面挂一个同一件事
// 是把身份说两遍（见 docs/cli-output.md 的「实体只点一次身份」）。
func printCreatedUser(stdout, stderr io.Writer, u auth.User) {
	_, _ = fmt.Fprintf(stderr, "已创建用户 %s\n", u.Username)
	_, _ = fmt.Fprintln(stdout, u.ID)
}

// runUserList 列出用户。
func runUserList(args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	status := fs.String("status", "", "")
	role := fs.String("role", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"list 不接受位置参数，得到 %q：按状态筛用 --status，按角色筛用 --role", fs.Arg(0)))
	}

	// 筛选条件在连库之前校验完（与 create 同一条约定）：写错的取值当场报错，
	// 不回落成「不筛」——回落的表现是打出一份看起来正常、其实没筛过的清单。
	filter := auth.ListUsersFilter{}
	if *status != "" {
		parsed, err := auth.ParseUserStatus(*status)
		if err != nil {
			return usageErr(err)
		}
		filter.Status = parsed
	}
	if *role != "" {
		parsed, err := auth.ParseRole(*role)
		if err != nil {
			return usageErr(err)
		}
		filter.Role = &parsed
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		users, err := auth.New(db).ListUsers(ctx, filter)
		if err != nil {
			return fmt.Errorf("列出用户失败: %w", err)
		}
		// JSON 是另一条渲染路径，不是把上面那张表转成 JSON（见 format.go 开头）。
		if jsonOutput() {
			return emitJSON(os.Stdout, userListDoc(users), nil)
		}
		printUserList(humanStdout(), users)
		return nil
	})
}

// runUserGet 读一个用户的全部字段。
func runUserGet(args []string) error {
	fs := flag.NewFlagSet("user get", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"get 不接受位置参数，得到 %q：要看的用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user（用户的 ID 或用户名）"))
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := auth.New(db).UserByRef(ctx, *user)
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, userGetDoc(info), nil)
		}
		printUserDetail(humanStdout(), info)
		return nil
	})
}

// printUserList 打一张用户清单。
//
// 角色与状态两栏给的是**原始取值**（user / active），不折成中文词：这两列的值
// 是脚本与别的命令要按它匹配的东西（`--status`、`--role` 收的正是这两个词），
// 而一个只在这一处出现的译文会让照着帮助写命令的人对不上。
func printUserList(w io.Writer, users []auth.UserInfo) {
	if len(users) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("用户", "user create"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		p.bold("ID"), p.bold("用户名"), p.bold("状态"), p.bold("角色"), p.bold("邮箱"))
	for _, u := range users {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.bold(strconv.FormatInt(u.ID, 10)), p.bold(u.Username),
			u.Status, string(u.Role), orDash(u.Email))
	}
	_ = tw.Flush()
}

// printUserDetail 打一个用户。
//
// 一栏一行而不是一行一表：理由与 printModelDetail 相同——一条记录的字段比清单
// 里那几栏多，横着排会折行，折行之后哪一栏是哪个值就得数着看。
func printUserDetail(w io.Writer, u auth.UserInfo) {
	tw := newTableWriter(w)
	p := newPalette(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("ID", strconv.FormatInt(u.ID, 10))
	row("用户名", u.Username)
	row("状态", u.Status)
	row("角色", string(u.Role))
	row("邮箱", orDash(u.Email))
	row("展示名", orDash(u.DisplayName))
	row("创建时间", p.dim(u.CreatedAt.Format(time.RFC3339)))
	row("更新时间", p.dim(u.UpdatedAt.Format(time.RFC3339)))
	_ = tw.Flush()
}

// userListDoc 组装 user list 的机器可读输出。
//
// 与 printUserList 是同一份数据的另一条渲染路径：那一档打表、把没填的邮箱铺成
// `-`，这一档给 null（见 format.go 的 nullableString）。
func userListDoc(users []auth.UserInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(users)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(users))
	for _, u := range users {
		entries = append(entries, userEntry(u))
	}
	doc["users"] = entries
	return doc
}

// userGetDoc 组装 user get 的机器可读输出。
//
// 只有一条，所以不打 summary（没有可数的东西），形状是 `{"user": {...}}`。
func userGetDoc(u auth.UserInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["user"] = userDetailEntry(u)
	return doc
}

// userEntry 打清单里的一个用户：表格那五栏对应的原始值。
func userEntry(u auth.UserInfo) map[string]any {
	return map[string]any{
		"id":       u.ID,
		"username": u.Username,
		"status":   u.Status,
		"role":     string(u.Role),
		// 邮箱在库里可空，空串就是「没填」（见 CreateUser：不填存 NULL）。
		"email": nullableString(u.Email),
	}
}

// userDetailEntry 打一个用户的详情：在清单那几栏之上多给展示名与两个时刻。
func userDetailEntry(u auth.UserInfo) map[string]any {
	entry := userEntry(u)
	entry["display_name"] = nullableString(u.DisplayName)
	entry["created_at"] = jsonTime(u.CreatedAt)
	entry["updated_at"] = jsonTime(u.UpdatedAt)
	return entry
}
