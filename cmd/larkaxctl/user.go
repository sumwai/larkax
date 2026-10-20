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
  larkaxctl user create --name <用户名> [--email <邮箱>] [--role <角色>] [--generate-password]
  larkaxctl user list [--status <状态>] [--role <角色>]
  larkaxctl user get --user <ID 或用户名>
  larkaxctl user update --user <ID 或用户名> [--status <状态>] [--email <邮箱>]
                        [--display-name <展示名>] [--merchant|--no-merchant] [--admin|--no-admin]
  larkaxctl user password <ID 或用户名> --generate|--stdin
  larkaxctl user unlock <ID 或用户名>
  larkaxctl user delete request --user <ID 或用户名>
  larkaxctl user delete revert --user <ID 或用户名>
  larkaxctl user delete purge --user <ID 或用户名> --confirm <用户名>

create 只把新用户的 ID 打到 stdout；用户名与邮箱在连库之前判形状，不合格报参数错。
list 与 get 只读；update 只改显式给出的字段，改了什么打到 stdout。
delete request 打标（可撤），revert 撤回，purge 匿名化（不可逆，要 --confirm）；余额先清到 0。
更细的边界：larkaxctl help user。
`

// userUsage 是 user 的长版：`larkaxctl help user` 与 `user help`。
const userUsage = `larkaxctl user 管理用户。

用法：
  larkaxctl user create --name <用户名> [--email <邮箱>] [--role <角色>]
                        [--generate-password]
  larkaxctl user list [--status <状态>] [--role <角色>]
  larkaxctl user get --user <ID 或用户名>
  larkaxctl user update --user <ID 或用户名> [--status <状态>] [--email <邮箱>]
                        [--display-name <展示名>] [--merchant|--no-merchant]
                        [--admin|--no-admin]
  larkaxctl user password <ID 或用户名> --generate|--stdin
  larkaxctl user unlock <ID 或用户名>

create 建一个新用户，只把新用户的 ID 打到 stdout。

角色取 user、merchant、admin 之一，默认 user。三者都是用户，merchant 与 admin
只是 users 表上的标记（admin 同时也是 merchant）。

**角色今天只是记下来，不改任何一处行为**：在后端的鉴权接上之前，--role merchant
不会让这个用户多出什么权限，--role admin 也不代表他能进管理接口。update 的那几
个角色旗标（--merchant、--admin 那四条）改的同样是这个称呼，不是权限。别把它们
当成访问控制。

**不做幂等**：用户名已被占用时直接报错，不会改到已有的那个用户。

用户名的形状有两条限制，都报参数错。**整串是一个十进制整数的名字建不出来**
（--name 123、--name 007 都是，带一个正负号的 +7 也算）。--user 收「ID 或用户名」，
纯数字一律按 ID 解释（见下面 get）——那样一个名字建出来之后就够不着了，
user get 与 user update 都按 ID 去找它。名字里带一个非数字字符即可
（1a、a1 都收，1.0、1e3 这类也不是整数）。

**名字里也不能有控制字符**（换行、制表符、ESC 这些）：凡是要打这个名字的地方都按
「一条记录一行、一个字段一栏」排版，而控制字符会把那一栏折出续行、把转义序列送进
终端。报错会点名是哪一个字符；点、连字符、空格、中文都照收。

邮箱的写法也判，收的是**地址本身**那一种，五条：一个 @、@ 两边都不为空；
域名里有一个点；两半都不以点开头或结尾、也不出现连着两个点；域名里用点分开的
每一段都不以连字符开头或结尾；字符都在允许之列——本地部分只收字母、数字与一组
常见符号（非 ASCII 的字母与数字也收），域名只收字母、数字、连字符与点，完整的
那一组写在报错里。

user+tag@example.com、first.last@example.com、a@b.example.com 都收；
not-an-email、@example.com、first@second@example.com、a b@example.com、
a@example.com.、a@example（域名没有点）、a@-b.example.com 都报参数错。带显示名、
注释或引号的写法（Bob <bob@example.com>、bob@example.com (comment)）也报错——
这一列存的是地址，不是一封信的收件人那一栏。

非 ASCII 那一条是有代价的：这一列的大小写不敏感只覆盖 ASCII 的字母，全角的 Ａ 与
半角的 A、西里尔文的同形字与拉丁字母都是**两个不同的值**，同一个人写两遍会并存成
两条。

**空邮箱不是格式错**：create 的 --email 给空串是不填这一列（库里是 NULL），
update 的 --email 给空串是把它清回 NULL（见下面 update）。

list 按 id 从小到大列出用户，一栏一个字段：ID、用户名、状态、角色、邮箱。

两个筛选项都可以同时给，也可以都不给（不给就是列全部）：

  --status  取 active、suspended、banned 之一。
  --role    取 user、merchant、admin 之一，按**称呼**逐字匹配，与 create 写进去的
            那一个对应：--role merchant 列出的是称呼为 merchant 的用户，
            --role admin 列的是 admin（admin 同时也是 merchant，但两者在这里
            是两个不同的取值）。

状态是鉴权时真正被检查的那一列（suspended 与 banned 都让用户手里的 key 全部失效）。
命令行上动它有两条路：update --status 改前三个取值，下面 delete 那一族打上并
撤回 deletion_requested。直接改库也一样生效，但那样既没有校验也没有回显，事后
说不清是谁改的。

get 按 ID 或用户名读一个用户的全部字段：除上面那几栏，还有展示名、删除时间、
创建时间与更新时间。标识怎么解释与别的命令一致：纯数字按 ID，否则按用户名。

update 只改**显式给出**的字段，一个都没给就报参数错（不做静默空操作）：

  --status       取 active、suspended、banned 之一。
  --email        新邮箱，写法要过上面那条格式校验。给空串表示清掉这一列
                 （写回 NULL），不是写成一个空串，也不是格式错。
  --display-name 新展示名。空串同上，表示清掉。取值不能有控制字符（换行、
                 制表符、ESC 这些），理由同用户名那一条。
  --merchant     把 is_merchant 置真。
  --no-merchant  把 is_merchant 置假（admin 上不能单独给，见下）。
  --admin        把 is_admin 置真，**并且**把 is_merchant 一起置真：admin 天然
                 也是 merchant（见上面角色那一节）。
  --no-admin     只把 is_admin 置假，merchant 不动。

每一条旗标说的都是**它自己那一列**变成什么，所以下面三组是互相打架的输入，
当场报参数错、不挑一个落库：--merchant 与 --no-merchant、--admin 与 --no-admin、
--admin 与 --no-merchant（理由同上：--admin 会把 merchant 一起置真）。

另有一条挡的不是输入，而是改完**那一行**读出来是什么：--no-merchant 打在 admin
上同样当场报参数错。它只把 is_merchant 置假，而这一档是按 is_admin 读的，那一行
读出来仍然是 admin——回显里角色那一栏没变，屏幕上只剩首行，看起来像什么都没做。
出路有两条，一条命令都不必分两次发：先 --no-admin（is_admin 置假、merchant 不动），
或者 --no-admin --no-merchant 一起给，一步落到普通用户。

改完把**真的变了**的字段按「旧值 → 新值」逐行打到 stdout，首行是「已改用户
<用户名>」。旧值取自改之前读的那一行，新值取自写语句 RETURNING 回来的那一行，
所以两栏是同一条语句前后的两次读数；给出的旗标没带来变化时（例如对本来就是
merchant 的用户再 --merchant），那一行不会出现。用户不存在时报「找不到用户」
并退出 4；邮箱撞上别的用户时报「邮箱已被占用」并带上那个值，也退出 4。

--email 与 --display-name 的取值两头空白会被去掉，去掉之后是空串就清回 NULL；
--status 也是先去两头空白再比对，但空串不认。

create、list、get 与 update 只碰 users 表（不碰渠道凭证）；delete 那一族会写到
api_keys、plugin_kv 与 capture_consents，钱包只读加锁。除了 plugin_kv 里属于
这个用户的行，别的行都不删。它们都不需要 LARKAX_MASTER_KEYS。

delete 这一族把账号从「要求删除」走到终态，分两步，中间可以撤回。

request 打标：把账号置为 deletion_requested，并记下它原来的状态。从这个时刻起，
用户手里的 API Key 全部失效——鉴权只认 status = active，打标之后这些
key 在任何一条路上都过不去。key 一行都不改，撤销之后原本有效的 key 恢复有效
（此前停用的仍然是停用）。

打标之前先看钱包：余额与冻结额都必须是 0。不是 0 时拒绝，并给出当前的两个数与
下一步（先用 wallet adjust 调账，或等退款与还没结算完的请求处理完）。

revert 撤掉打标：状态回到 request 之前的那一个——原来 active 就回 active，原来
suspended 就回 suspended，不会被顺手放回 active。key 仍然一行都不动。

purge 匿名化（不可逆）：只接已经处于 deletion_requested 的账号，并且要 --confirm
逐字写出当前用户名。没有 --confirm 或写错时什么都不写。它在**一个事务里**做这几
件事：

  - API Key 里状态还是 active 的全部置为 revoked，行保留；
  - plugin_kv 里属于这个用户的键全部删除（按插件与用户隔离的那一份）；
  - capture_consents 里还没撤销的授权全部撤销（写 revoked_at），行保留，到期仍按
    各自的 expires_at；
  - users 行置为 deleted、写下 deleted_at，并把用户名、邮箱、展示名换成不可逆的
    占位：deleted-user-<ID>、deleted-user-<ID>@example.com、已注销用户 #<ID>；
  - 钱包行与账本分录不动：财务记录不随账号删除清除（见 docs/request-retention.md）；
  - 请求快照不动：它们按各自的留存期由 snapshot cleanup 删除。

余额与冻结额在 purge 时再判一次：打标之后仍可能有退款或调账进来，而匿名化之后
就没有账号去收这笔钱了。

销户的重心是**别人还认不认得出这个账号**，不是删行。users、api_keys、钱包、
账本与授权记录的行都留着；能核对的是 status、deleted_at 与那几个占位值。
到了 deleted 之后，update 也不再收这个账号：改邮箱、展示名会被拒（退出 4），
否则占位身份就有一条命令能写回真身份。

password 设或重置控制台口令。用户标识可以是 --user，也可以直接写成一个位置参数
（两种混用会报错）。

  --generate  由本命令生成一个 24 字符的随机口令。
  --stdin     从标准输入读，不落 shell 历史。

两档必须给且只给一个。stdout 恰好一行，就是新口令，只显示这一次；其余说明走
stderr。口令从不作为命令行参数出现（进程列表可见），也不写日志。设置成功后这个
用户的全部控制台会话被吊销——运维重置的场景是「不知道谁还开着控制台」，留下任何
一个旧会话都让这次重置只对下一次登录生效。

口令策略与登录侧同一份判据：12–128 个字符、不能等于用户名、不能有控制字符。
从标准输入读时行尾的换行会被去掉（echo 管道最自然的写法就是带一个）。

create --generate-password 在建号的同时设一个随机初始口令：stdout 那一行因此是
**口令**而不是用户 ID，ID 与用户名走 stderr。要接走 ID 的脚本不要带这个旗标。

unlock 清掉一个账号的失败计数与锁定（连续失败 5 次后按指数锁定，上限 15 分钟）。
它不动口令、不动会话：解锁只把「现在能不能试」恢复成默认，不改变谁能登录。

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
	case "update":
		return runUserUpdate(rest)
	case "password":
		return runUserPassword(rest)
	case "unlock":
		return runUserUnlock(rest)
	case "delete":
		return runUserDelete(rest)
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
	generateInitialPassword := fs.Bool("generate-password", false, "")
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
	// 名字的形状也在这里判（判据在 auth，命令行与包认的是同一处，见
	// auth.ValidateNewUsername）：纯数字的名字建得出来、读不回来，而这一条
	// 不必连库就知道。放在连库之前还有一层：它挡的是一次注定要回滚的写入。
	if err := auth.ValidateNewUsername(*name); err != nil {
		return usageErr(err)
	}
	parsedRole, err := auth.ParseRole(*role)
	if err != nil {
		return usageErr(err)
	}
	// 邮箱的形状同样在连库之前判（判据在 auth，见 auth.ValidateEmail）。空串是
	// 「不填这一列」，不是格式错——判据放行它，这里也就不必分两种写法。
	// 复用上面那个 err（这里不 `:=`）：新起一个会把外层这个名字遮掉，而外层那个
	// 紧接着还要读（下面 cfg 那一处），遮出的是同一个名字的两个作用域。
	if err = auth.ValidateEmail(*email); err != nil {
		return usageErr(err)
	}

	// 初始口令在连库之前生成：熵源坏了是一次「这次命令根本没开始」的失败，
	// 不该在库里留下一个建了一半的账号。
	var initialPassword string
	if *generateInitialPassword {
		initialPassword, err = generatePassword()
		if err != nil {
			return err
		}
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

	store := auth.New(db)
	created, err := store.CreateUser(ctx, auth.CreateUserParams{
		Username: *name,
		Email:    *email,
		Role:     parsedRole,
	})
	if err != nil {
		return err
	}

	if *generateInitialPassword {
		hash, err := auth.HashPassword(auth.DefaultKDFParams, initialPassword)
		if err != nil {
			return fmt.Errorf("用户 %s（ID %d）已创建，但计算口令哈希失败: %w",
				created.Username, created.ID, err)
		}
		if err := store.SetPassword(ctx, created.ID, hash, nil); err != nil {
			return fmt.Errorf("用户 %s（ID %d）已创建，但设置初始口令失败: %w",
				created.Username, created.ID, err)
		}
		printCreatedUserWithPassword(humanStdout(), humanStderr(), created, initialPassword)
		return nil
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

// printCreatedUserWithPassword 打印「建号 + 初始口令」的结果。
//
// 与 printCreatedUser 的分工：带 --generate-password 时 stdout 那一行是
// **口令**——它只出现这一次，要能被直接接走或复制；ID 与用户名走 stderr。
// 两种输出形状不能混：stdout 一行里既放 ID 又放口令，接的人分不出哪一半是哪个。
func printCreatedUserWithPassword(stdout, stderr io.Writer, u auth.User, password string) {
	_, _ = fmt.Fprintf(stderr, "已创建用户 %s（ID %d），初始口令只显示这一次\n",
		u.Username, u.ID)
	_, _ = fmt.Fprintln(stdout, password)
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

// runUserUpdate 改一个用户的若干列。
func runUserUpdate(args []string) error {
	// 与 create 同一条：写类命令今天没有机器可读那一档，必须在改任何一列
	// 之前拦下来。
	if err := jsonUnsupported("user update"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("user update", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	status := fs.String("status", "", "")
	email := fs.String("email", "", "")
	displayName := fs.String("display-name", "", "")
	merchant := fs.Bool("merchant", false, "")
	noMerchant := fs.Bool("no-merchant", false, "")
	admin := fs.Bool("admin", false, "")
	noAdmin := fs.Bool("no-admin", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user（用户的 ID 或用户名）"))
	}

	// 参数全部在连库之前校验完（与 create / list 同一条约定）。「一个字段都
	// 没给」也在这里挡下来：那一次的语句只会动 updated_at，命令照样退出 0，
	// 而使用者以为改动生效了。
	params, err := userUpdateParams(providedFlags(fs), userUpdateFlags{
		status:      *status,
		email:       *email,
		displayName: *displayName,
		merchant:    *merchant,
		noMerchant:  *noMerchant,
		admin:       *admin,
		noAdmin:     *noAdmin,
	})
	if err != nil {
		return usageErr(err)
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		store := auth.New(db)

		// 改之前先读一行：回显要打「从什么变成什么」，而改完再读回来的只有
		// 「变成什么」。这一读同时把「用户不存在」提到写任何东西之前。
		before, err := store.UserByRef(ctx, *user)
		if err != nil {
			return err
		}

		// 这一条校验**做不到「连库之前」**，是这份文件里唯一的一条：它说的是
		// 「改完之后那一行会不会变成一个反例」，而那要看库里现在是什么。上面那
		// 三组挡的是输入本身，这一条挡的是结果——--no-merchant 打在 admin 上只把
		// is_merchant 置假，留下 (is_merchant=false, is_admin=true)，而 roleOf 仍
		// 把那一行读成 admin：真改了库的那一次在回显里看起来像什么都没做。
		if isMerchant, isAdmin := userUpdateResultMarkers(before.Role, params); isAdmin && !isMerchant {
			return usageErr(errors.New(
				"--no-merchant 会把 admin 的 is_merchant 置假，而 admin 天然也是 merchant：" +
					"先 --no-admin（is_admin 置假、merchant 不动），" +
					"或 --no-admin --no-merchant 一起给（一步落到普通用户）"))
		}

		after, err := store.UpdateUser(ctx, *user, params)
		if err != nil {
			return err
		}
		printUserUpdate(humanStdout(), before, after)
		return nil
	})
}

// runUserDelete 分派 delete 的三个动作。
//
// 三个动作各自一条命令，不合成一条带 --step 的命令：它们要的东西不一样
// （purge 要 --confirm），而「同一个入口按旗标决定做哪一步」会让一次少写一个
// 旗标的调用落到另一档上——那一步可能是不可逆的那一步。
func runUserDelete(args []string) error {
	if len(args) == 0 {
		return usageErr(errors.New("delete 需要子命令：request、revert 或 purge"))
	}
	action, rest := args[0], args[1:]
	switch action {
	case "request":
		return runUserDeleteRequest(rest)
	case "revert":
		return runUserDeleteRevert(rest)
	case "purge":
		return runUserDeletePurge(rest)
	default:
		return usageErr(fmt.Errorf("未知的 user delete 子命令 %q", action))
	}
}

// runUserDeleteRequest 给账号打上「要求删除」的标记。
func runUserDeleteRequest(args []string) error {
	if err := jsonUnsupported("user delete request"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("user delete request", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"delete request 不接受位置参数，得到 %q：要标记的用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user（用户的 ID 或用户名）"))
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		mark, err := auth.New(db).RequestDeletion(ctx, *user)
		if err != nil {
			return err
		}
		printDeletionReceipt(humanStdout(), deletionRequestReceipt(mark))
		return nil
	})
}

// runUserDeleteRevert 撤掉「要求删除」的标记。
func runUserDeleteRevert(args []string) error {
	if err := jsonUnsupported("user delete revert"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("user delete revert", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"delete revert 不接受位置参数，得到 %q：要撤回的用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user（用户的 ID 或用户名）"))
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		mark, err := auth.New(db).RevertDeletion(ctx, *user)
		if err != nil {
			return err
		}
		printDeletionReceipt(humanStdout(), deletionRevertReceipt(mark))
		return nil
	})
}

// runUserDeletePurge 匿名化一个已经打过标的账号。
func runUserDeletePurge(args []string) error {
	if err := jsonUnsupported("user delete purge"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("user delete purge", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	confirm := fs.String("confirm", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"delete purge 不接受位置参数，得到 %q：要匿名化的用户用 --user 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少 --user（用户的 ID 或用户名）"))
	}
	// 空 --confirm 在连库之前就挡下：它是这次调用的形状，不是库里的数据。
	// 写错名字的那一档要比对用户名，只能等读到那一行之后再判。
	if strings.TrimSpace(*confirm) == "" {
		return usageErr(errors.New("缺少 --confirm（要逐字写出当前用户名）"))
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		result, err := auth.New(db).PurgeDeletion(ctx, *user, *confirm)
		if err != nil {
			return err
		}
		printDeletionReceipt(humanStdout(), deletionPurgeReceipt(result))
		return nil
	})
}

// deletionReceipt 是销户命令的一张回执：首行说这是什么，下面一栏一行。
//
// 三条命令共用一张表：它们的形状是同一个（一次状态改动 + 逐项结果），而各打
// 各的版式会让「核对销户之后是什么样」这件事在三条命令上要读三次。
type deletionReceipt struct {
	headline string
	rows     [][2]string
}

// deletionRequestReceipt 是打标的回执：改了什么，以及下一步敲什么。
func deletionRequestReceipt(m auth.DeletionMark) deletionReceipt {
	return deletionReceipt{
		headline: fmt.Sprintf("已标记用户 %s 要求删除。", m.Username),
		rows: [][2]string{
			{"状态", m.FromStatus + " → " + m.ToStatus},
			{"撤销", "larkaxctl user delete revert --user " + shellArg(m.Username)},
			{"匿名化", "larkaxctl user delete purge --user " + shellArg(m.Username) +
				" --confirm " + shellArg(m.Username)},
		},
	}
}

// deletionRevertReceipt 是撤销的回执。
func deletionRevertReceipt(m auth.DeletionMark) deletionReceipt {
	return deletionReceipt{
		headline: fmt.Sprintf("已撤销用户 %s 的删除要求。", m.Username),
		rows: [][2]string{
			{"状态", m.FromStatus + " → " + m.ToStatus},
			{"API Key", "未改动，原本有效的 key 恢复有效"},
		},
	}
}

// deletionPurgeReceipt 是匿名化的回执：每一项处置一行。
//
// 回执读的是**写库那一次的前后两个值**（见 auth.DeletionResult），不是从参数
// 再拼一遍：要回答的是「库里现在是什么」，而参数只说「打算写什么」。
func deletionPurgeReceipt(r auth.DeletionResult) deletionReceipt {
	return deletionReceipt{
		headline: fmt.Sprintf("已匿名化用户 %s。", r.OldUsername),
		rows: [][2]string{
			{"ID", strconv.FormatInt(r.UserID, 10)},
			{"用户名", r.OldUsername + " → " + r.NewUsername},
			{"邮箱", orDash(r.OldEmail) + " → " + r.NewEmail},
			{"展示名", orDash(r.OldDisplayName) + " → " + r.NewDisplayName},
			{"状态", auth.UserStatusDeletionRequested + " → " + auth.UserStatusDeleted},
			{"删除时间", r.DeletedAt.Format(time.RFC3339)},
			{"API Key", fmt.Sprintf("%d 把置为 revoked（共 %d 把，行都保留）",
				r.KeysRevoked, r.KeysTotal)},
			{"插件 KV", fmt.Sprintf("%d 个键已删除", r.PluginKVDeleted)},
			{"采集授权", fmt.Sprintf("%d 条已撤销（行保留）", r.ConsentsRevoked)},
			{"钱包", deletionWalletLine(r)},
			{"账本", fmt.Sprintf("%d 条分录保留", r.LedgerEntries)},
		},
	}
}

// deletionWalletLine 说清钱包那一条处置。
//
// 「余额 0、冻结 0，行保留」只对有钱包行的账号成立：从没开过户的账号一行都
// 没有，回执说「行保留」会让人以为那里还有一条记录可查。
func deletionWalletLine(r auth.DeletionResult) string {
	if r.WalletRows == 0 {
		return "没有钱包行"
	}
	return fmt.Sprintf("余额 %s、冻结 %s，行保留", r.Balance, r.Frozen)
}

// shellArg 把一个值写成能直接粘进 POSIX shell 的形态。
//
// 用户名只挡了控制字符与「会被 --user 当成 ID」的写法，空格、引号、$ 都能
// 出现；回执里那句「下一步敲什么」要能原样执行，所以整体用单引号包住，内部的
// 单引号用 '\” 收尾再开头。
func shellArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printDeletionReceipt 打一张销户回执。
func printDeletionReceipt(w io.Writer, r deletionReceipt) {
	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintln(w, r.headline)
	for _, row := range r.rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(row[0]), row[1])
	}
	_ = tw.Flush()
}

// userUpdateFlags 是 update 那一组旗标的原始取值。
//
// 收成一个结构体而不是八个平铺的参数：为的是让「旗标名 → 哪一列」这张对应
// 关系在下面那个函数里一眼看得全，而不是散在参数表里对位。
type userUpdateFlags struct {
	status      string
	email       string
	displayName string
	merchant    bool
	noMerchant  bool
	admin       bool
	noAdmin     bool
}

// userUpdateParams 把旗标折成要写的列。
//
// 它只做校验与编码，不碰数据库——参数错要在连库之前报出来，这里正好是那条
// 分界线，于是它也是不连库就能单测走到的那一半。
//
// # 四个标记旗标的语义
//
// 每条旗标说的都是**它自己那一列**变成什么，而不是「把整个角色换掉」：
//
//	--merchant      is_merchant = true
//	--no-merchant   is_merchant = false
//	--admin         is_admin = true，并且 is_merchant = true
//	--no-admin      is_admin = false，merchant 不动
//
// --admin 连 merchant 一起写，取值直接问 auth（Role.Flags）——「admin 天然也是
// merchant」这条不变式的定义只有那一处。所以 --admin 配 --no-merchant 是一对
// 互相打架的输入，**当场拒掉**而不是挑一个落库：挑一个的话命令照样退出 0，而
// 库里那一行是不是想要的样子只能再读一遍才知道。
//
// 给没给按 seen 判，不按「取值等于零值」判：`--display-name ""` 是一次**清空**，
// 它与「没给这一项」是两件事，而两者的取值都是空串。
func userUpdateParams(seen map[string]bool, f userUpdateFlags) (auth.UpdateUserParams, error) {
	var p auth.UpdateUserParams

	if seen["status"] {
		// 取值不认识当场报错，不回落成「不改」——回落的表现是命令退出 0，
		// 而那一列没动过。
		parsed, err := auth.ParseUserStatus(f.status)
		if err != nil {
			return p, err
		}
		p.Status = &parsed
	}
	if seen["email"] {
		// 取值不是一个邮箱地址就当场报错（判据在 auth，见 ValidateEmail），
		// 不落库：这一列上还有唯一约束，一个写错的邮箱会占掉别人的那一格。
		// 空串过这一关——它是「清掉这一列」，由下面折成 NULL。
		if err := auth.ValidateEmail(f.email); err != nil {
			return p, err
		}
		p.Email = &f.email
	}
	if seen["display-name"] {
		// 与邮箱同一条：形状判据在 auth（见 ValidateDisplayName），在连库
		// 之前判。空串过这一关——它是「清掉这一列」。
		if err := auth.ValidateDisplayName(f.displayName); err != nil {
			return p, err
		}
		p.DisplayName = &f.displayName
	}

	switch {
	case f.merchant && f.noMerchant:
		return p, errors.New("--merchant 与 --no-merchant 不能同时给：一个要置真、一个要置假")
	case f.admin && f.noAdmin:
		return p, errors.New("--admin 与 --no-admin 不能同时给：一个要置真、一个要置假")
	case f.admin && f.noMerchant:
		return p, errors.New("--admin 与 --no-merchant 矛盾：admin 天然也是 merchant，" +
			"--admin 会把 is_merchant 一起置真（见 larkaxctl help user）")
	}

	// 四个标记旗标折成列。各有各的列，只有 --admin 写两列，所以它放最后——
	// 赋值是按指针覆盖，而互相打架的组合上面已经全挡掉了，先后不影响结果。
	if f.merchant {
		yes := true
		p.IsMerchant = &yes
	}
	if f.noMerchant {
		no := false
		p.IsMerchant = &no
	}
	if f.noAdmin {
		no := false
		p.IsAdmin = &no
	}
	if f.admin {
		// 两列一起从 auth 取，不在这里写 `true`：抄一份之后，改了 auth 那一处
		// 而没改这里，建出来的 admin 与改出来的 admin 就是两行不同的数据，
		// 而两边的用例都照样通过（见 Role.Flags 的注释）。
		merchant, admin := auth.RoleAdmin.Flags()
		p.IsMerchant, p.IsAdmin = &merchant, &admin
	}

	if p.Status == nil && p.Email == nil && p.DisplayName == nil &&
		p.IsMerchant == nil && p.IsAdmin == nil {
		return p, errors.New("一个要改的字段都没给：用 --status、--email、" +
			"--display-name、--merchant / --no-merchant、--admin / --no-admin 指定")
	}
	return p, nil
}

// userUpdateResultMarkers 算出这次改动之后那两个标记会是什么。
//
// 没提到的列按**折出来的称呼**推：读侧给的是 Role，而 roleOf 先看 is_admin，所以
// (merchant=false, admin=true) 与 (true, true) 都读成 admin。admin 这一档上称呼
// 与标记本来就是绑定的（Role.Flags 只按称呼写一次），所以「称呼是 admin」一律当
// merchant 也为真——那正是正常写法的结果（建与改都从那一处取）。
//
// runUserUpdate 里那条「改完之后不能是一个反例」的校验就靠它算：只看读回来的
// 那一行与这次给的参数，不与库打交道，所以能单独走一遍。
func userUpdateResultMarkers(before auth.Role, p auth.UpdateUserParams) (isMerchant, isAdmin bool) {
	isMerchant = before == auth.RoleMerchant || before == auth.RoleAdmin
	isAdmin = before == auth.RoleAdmin
	if p.IsMerchant != nil {
		isMerchant = *p.IsMerchant
	}
	if p.IsAdmin != nil {
		isAdmin = *p.IsAdmin
	}
	return isMerchant, isAdmin
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
	// 销户之后这一栏才有值；没走到终态时是 `-`，与「没填」的显示同一套。
	deletedAt := p.dim("-")
	if u.DeletedAt != nil {
		deletedAt = p.dim(u.DeletedAt.Format(time.RFC3339))
	}
	row("删除时间", deletedAt)
	row("创建时间", p.dim(u.CreatedAt.Format(time.RFC3339)))
	row("更新时间", p.dim(u.UpdatedAt.Format(time.RFC3339)))
	_ = tw.Flush()
}

// printUserUpdate 打印这一次改了什么。
//
// 一行一个**真的变了**的字段，第二栏是「旧值 → 新值」。打这个而不是一句
// 「已更新」：改身份这一类操作的下一步常常是核对（「刚才那条把谁封了？」），
// 只给终点时核对的人只能自己再读一遍，读到的还是改完之后的那个值，起点已经
// 没了。没变的字段不打——那几行既不是这次改动的结果，要看状态用 user get。
//
// 两栏都取自库：旧值来自改之前读的那一行，新值来自写语句 RETURNING 回来的
// 那一行。从命令行参数再拼一遍看着一样，但它记的是「我打算写什么」，而这一步
// 要回答的是「库里现在是什么」。
func printUserUpdate(w io.Writer, before, after auth.UserInfo) {
	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(w, "已改用户 %s\n", after.Username)

	row := func(label, old, next string) {
		if old == next {
			return
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), old+" → "+next)
	}
	row("状态", before.Status, after.Status)
	// 角色这一栏给的是称呼（user / merchant / admin），由那两个标记折回来。
	// 打原始标记的话，「--admin 把 merchant 一起置真」看起来像动了两列。
	row("角色", string(before.Role), string(after.Role))
	// 邮箱与展示名在库里可空，`-` 就是「没填」（与 user get 那一侧同一套渲染）。
	row("邮箱", orDash(before.Email), orDash(after.Email))
	row("展示名", orDash(before.DisplayName), orDash(after.DisplayName))
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
	entry["deleted_at"] = jsonTimePtr(u.DeletedAt)
	entry["created_at"] = jsonTime(u.CreatedAt)
	entry["updated_at"] = jsonTime(u.UpdatedAt)
	return entry
}
