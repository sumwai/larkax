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
  larkaxctl user update --user <ID 或用户名> [--status <状态>] [--email <邮箱>]
                        [--display-name <展示名>] [--merchant|--no-merchant]
                        [--admin|--no-admin]

create 只把新用户的 ID 打到 stdout；用户名已被占用时直接报错，不改已有的用户。
用户名的形状与邮箱的格式都在连库之前判一遍，不合格报参数错。
list 与 get 只读；update 只改显式给出的字段，改了什么打到 stdout。这四条都不碰
渠道凭证，也都不需要 LARKAX_MASTER_KEYS。

更细的边界：larkaxctl help user。
`

// userUsage 是 user 的长版：`larkaxctl help user` 与 `user help`。
const userUsage = `larkaxctl user 管理用户。

用法：
  larkaxctl user create --name <用户名> [--email <邮箱>] [--role <角色>]
  larkaxctl user list [--status <状态>] [--role <角色>]
  larkaxctl user get --user <ID 或用户名>
  larkaxctl user update --user <ID 或用户名> [--status <状态>] [--email <邮箱>]
                        [--display-name <展示名>] [--merchant|--no-merchant]
                        [--admin|--no-admin]

create 建一个新用户，只把新用户的 ID 打到 stdout。

角色取 user、merchant、admin 之一，默认 user。三者都是用户，merchant 与 admin
只是 users 表上的标记（admin 同时也是 merchant）。

**角色今天只是记下来，不改任何一处行为**：在后端的鉴权接上之前，--role merchant
不会让这个用户多出什么权限，--role admin 也不代表他能进管理接口。update 的那几
个角色旗标（--merchant、--admin 那四条）改的同样是这个称呼，不是权限。别把它们
当成访问控制。

**不做幂等**：用户名已被占用时直接报错，不会改到已有的那个用户。

用户名的形状也有限制：**整串是一个十进制整数的名字建不出来**（--name 123、
--name 007 都是，带一个正负号的 +7 也算），报参数错。--user 收「ID 或用户名」，
纯数字一律按 ID 解释（见下面 get）——那样一个名字建出来之后就够不着了，
user get 与 user update 都按 ID 去找它。名字里带一个非数字字符即可
（1a、a1 都收，1.0、1e3 这类也不是整数）。

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

状态是鉴权时真正被检查的那一列（suspended 与 banned 都让他手里的 key 全部失效），
**命令行上改它只有下面 update 这一条路**：直接改库也一样生效，但那样既没有校验
也没有回显，事后说不清是谁改的。

get 按 ID 或用户名读一个用户的全部字段：除上面那几栏，还有展示名、创建时间与
更新时间。标识怎么解释与别的命令一致：纯数字按 ID，否则按用户名。

update 只改**显式给出**的字段，一个都没给就报参数错（不做静默空操作）：

  --status       取 active、suspended、banned 之一。
  --email        新邮箱，写法要过上面那条格式校验。给空串表示清掉这一列
                 （写回 NULL），不是写成一个空串，也不是格式错。
  --display-name 新展示名。空串同上，表示清掉。
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

四条命令都只碰 users 表（不碰渠道凭证），都不需要 LARKAX_MASTER_KEYS。

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
	entry["created_at"] = jsonTime(u.CreatedAt)
	entry["updated_at"] = jsonTime(u.UpdatedAt)
	return entry
}
