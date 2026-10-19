package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/textshape"
)

// 本文件是 users 表的**写入**路径：建一个用户、改一个已有用户的若干列，以及
// 「角色」这个称呼到表上两个布尔标记的对应关系；同样的表也有**读**路径（列用户、
// 按标识读一个），它们与写入路径落在同一个文件里，因为两者共用角色称呼与标记
// 之间那一套映射（见 Role.Flags 与 roleOf）——分到两个文件里，这套映射就会长成
// 两份。
//
// 它与 store.go 的分工按**表**来分，不按操作来分：store.go 管 api_keys
// 的鉴权与创建，这里管 users 的创建与读取。两边共用 User 与同一个 *sql.DB，
// 所以拆文件只是为了让改动落在不重叠的地方，不是拆包。

// PostgreSQL 唯一约束冲突的错误码，以及 users 表上两个唯一约束的名字。
//
// 约束名硬编码自迁移 00001_init.sql 里 users 的列定义
// （`username citext NOT NULL UNIQUE` 与 `email citext UNIQUE` 生成的
// 就是这两个名字）。**改那张表就要同步改这里**，否则下面的识别失效。
// 失效的方向是安全的：认不出来就按原错误上抛，不会静默当成功，
// 只是少一层分类，报错从「用户名已被占用」退化成一句驱动原文。
//
// 只认错误码不够——23505 的含义是「有某个唯一约束被违反」，
// 分不出撞的是用户名还是邮箱，而这两者给人的下一步动作不同。
const (
	pgUniqueViolation   = "23505"
	usersUsernameUnique = "users_username_key"
	usersEmailUnique    = "users_email_key"
)

// 建用户时「这个名字不能用」的两类判断：撞上唯一约束，以及名字的形状不行。
//
// 第三类（写入本身失败，例如连不上库）没有哨兵，原样包一层上抛，见
// createUserFailure。
//
// 前两类必须分得开：它们都是「改个名字就能继续」的错误，而第三类是环境问题。
// 混在一条消息里，第一次建库的人会去改用户名，而真正的问题是 DSN 写错了。
var (
	// ErrUsernameTaken 表示这个用户名已经有人用了。
	ErrUsernameTaken = errors.New("用户名已被占用")
	// ErrEmailTaken 表示这个邮箱已经挂在另一个用户上。
	ErrEmailTaken = errors.New("邮箱已被占用")
	// ErrUsernameIsIDRef 表示这个用户名会被 `--user` 按 ID 解释（整串是一个十进制
	// 整数），于是落库之后按名字读不回来。
	//
	// 与上面两个分开放：它们说的是「这个名字被占了」，下一步是换一个名字；
	// 这一条说的是「这个形状的名字不能用」，下一步同样是换一个名字，但它与库里
	// 现在有什么无关。单独一个哨兵是为了让命令行把它归到「参数错」（退出码 2），
	// 与 ErrNoChanges 同一档——重发一次就能过。
	//
	// 措辞只说十进制整数，不说「一个数」：`1.0`、`1e3`、`0x10` 与 `1_0` 在
	// strconv 那一套里都不是整数（见 userRefIsID），它们是名字。
	ErrUsernameIsIDRef = errors.New("用户名不能整个是一个十进制整数")
	// ErrUsernameHasControlChar 表示这个名字里有控制字符（C0 与 DEL）。
	//
	// 与上面两个分开放的理由与 ErrUsernameIsIDRef 一样：它说的是「这个形状的名字
	// 不能用」，与库里现在有什么无关，命令行把它归到「参数错」（退出码 2）。
	// 与 ErrUsernameIsIDRef 分成两条而不是合成一句「名字不合法」，是因为被拒的人
	// 要做的动作不同：这一条是去掉某个字符，那一条是换一个名字。
	ErrUsernameHasControlChar = errors.New("用户名不能含控制字符")
	// ErrInvalidEmail 表示这个串不是一个邮箱地址的写法。
	//
	// 与 ErrEmailTaken 分开放：那一条说的是「这个邮箱挂在别人名下」，下一步是换
	// 一个邮箱；这一条说的是「这个串不像一个邮箱地址」，下一步同样是换一个串，
	// 但它与库里现在有什么无关。单独一个哨兵是为了让命令行把它归到「参数错」
	// （退出码 2），与 ErrUsernameIsIDRef 同一档——重发一次就能过。
	ErrInvalidEmail = errors.New("不是一个邮箱地址")
)

// ValidateNewUsername 校验一个**要建**的用户名。
//
// 两条判据，都只看名字本身：
//
//  1. 不能是 `--user` 会按 ID 解释的写法。名字进库之后怎么解释由 userRefWhere
//     决定，而它把这类串一律当 ID——那样一个名字建得出来、读不回来：`user get`
//     与 `user update` 都够不着它，只能改库。拦在建的时候，是为了不让这样一行
//     落进库里。
//
//  2. 不能含控制字符（C0 与 DEL）。这个形状的名字会把打它的地方打坏：名字是
//     外部输入，而所有打名字的地方都按「一条记录一行、一个字段一栏」排版——
//     `a\nb` 折出来是一条没有栏名的裸续行（`\r`、`\t` 同形），ESC 序列则原样进
//     执行者的终端，改标题、改色、清屏都能做。同一条路上还有第二处不一致：这个
//     名字进账本分录的备注时会过 ledger 的 sanitizeMemo，而它只把 `\r`、`\n`、
//     `\t` 换成空格（ESC 这类原样留在备注里），于是回显里写的与库里记的不是同一
//     个串，而没有任何提示。
//
// 两条都在**去掉两头空白之后**的名字上判，与 CreateUser 落库的那个值一致（它先
// 去空白再插）：`--name " 123 "` 存进去的是 `123`，那正是读不回来的那一个；而
// `--name $'\ta\t'` 存进去的是 `a`，那是个不带控制字符的名字，这里就不该拦。
//
// 第 1 条的判据与 userRefWhere 共用 userRefIsID，两边认的是同一批串；有一条用例
// 钉住这条一致性（见 TestValidateNewUsernameAgreesWithUserRefWhere）。两处各判
// 一遍的症状很隐蔽：写入路径放过去一个名字，解析路径读不回来，而两边各自的用例
// 都绿。第 2 条与 userRefWhere 无关——控制字符不会让一个串变成一个十进制整数，
// 所以它只往下加拒的那一侧，两条判据互不遮蔽。
//
// 第 2 条**不是**一张字符白名单：`.`、`-`、空格、中文照收，收窄成「只允许
// `[A-Za-z0-9_-]`」是另一条判据要谈的事（名字的字符集今天没有明文规定），与
// 「这个名字会不会把清单打坏」是两件事。C1（U+0080 起）与零宽字符同样不在
// 这一条里：要堵的是折行与转义序列这两类。
//
// 它管不到库里已经有的行——那些名字本来就够不着、本来就可能带控制字符，改解释
// 规则或清理存量是另一件事（见 ResolveUser）。改名的路也不存在：`user update`
// 不收 `--name`，UpdateUserParams 里也没有这一列。
func ValidateNewUsername(name string) error {
	trimmed := strings.TrimSpace(name)
	if _, ok := userRefIsID(trimmed); ok {
		return fmt.Errorf("%w：%q 会被 --user 按 ID 解释，"+
			"建出来之后 user get 与 user update 都找不到它", ErrUsernameIsIDRef, trimmed)
	}
	if r, ok := firstControlChar(trimmed); ok {
		return fmt.Errorf("%w：%q 里有%s。凡是打这个名字的地方（清单与写命令的回显）"+
			"都按「一条记录一行、一个字段一栏」排版，而这类字符会让那一栏错位、或"+
			"折出没有栏名的续行；ESC 这类转义序列还会原样进终端。--name 里去掉它",
			ErrUsernameHasControlChar, trimmed, describeControlChar(r))
	}
	return nil
}

// firstControlChar 找出名字里第一个控制字符（C0 或 DEL），没有就报 false。
//
// 判据与 cmd/larkaxctl 的 checkOrderKey 同一形态（那边管幂等键）：C0 是
// U+0000–U+001F，DEL 是 U+007F。C1 与零宽字符不收，理由见 ValidateNewUsername。
//
// 只报第一个就够：报错要说清的是「哪个字符要改」，而一个名字里出现控制字符时，
// 它后面往往还跟着一串（`\x1b[31m` 就是一整个转义序列）。
func firstControlChar(name string) (rune, bool) {
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return r, true
		}
	}
	return 0, false
}

// ValidateDisplayName 校验 users.display_name 的新值。
//
// 空串（含只有空白的）不是形状错误：那是「清掉这一列」，落库的是 NULL
// （见 UpdateUser 的 addText）。判在去掉两头空白之后的值上，与落库的那个串
// 一致——与 ValidateNewUsername 对 `--name $'\ta\t'` 的处置同一条口径。
//
// 这一列会进 `user list` 的表格与 `user get` 的单条回显，而两处都按「一个
// 字段一栏」排版：换行会把那一栏折出没有栏名的续行，ESC 这类转义序列会原样
// 进终端。控制字符的判据在 textshape，与别的展示字段共用一份。
func ValidateDisplayName(name string) error {
	return textshape.Validate("--display-name", strings.TrimSpace(name))
}

// describeControlChar 把报错里点名的那个字符说成能照着改的样子：码位，以及那几个
// 在命令行上敲得出来的字符的名字。
//
// 名字不是装饰：`$'a\tb'` 里的那个字符，在报错里写成 U+0009 对不上使用者眼里的
// 东西——他敲的是一个 `\t`。其余 C0 只报码位：它们没有一个人人会随手敲出来的写法。
func describeControlChar(r rune) string {
	switch r {
	case '\n':
		return "换行（U+000A）"
	case '\r':
		return "回车（U+000D）"
	case '\t':
		return "制表符（U+0009）"
	case 0x1b:
		return "ESC（U+001B）"
	case 0x00:
		return "NUL（U+0000）"
	case 0x7f:
		return "DEL（U+007F）"
	default:
		return fmt.Sprintf("U+%04X", r)
	}
}

// 邮箱地址两半各自允许出现的符号。
//
// 字母与数字由 isASCIILetterDigit 认，点由 badEmailDots 判位置，所以下面两个
// 串里只有别的符号。本地部分那一组是标准里不带引号的写法允许的那些；域名的只有
// 连字符——域名实际只用字母、数字、点与连字符。
const (
	emailLocalSymbols  = "!#$%&'*+-/=?^_`{|}~"
	emailDomainSymbols = "-"
	// emailCharsHint 是「哪些字符收」那一句，符号表由上面那个常量拼出来：
	// 手抄一遍的话，往表里加一个符号而忘了改这句话，报错就少说一个——而少说的
	// 那一个正是使用者照着改的那一个。
	//
	// 表后面不跟 ASCII 的句点：`.` 本身就在这张表里，紧跟一个句号的话，读的人
	// 分不出那个点是表的一部分还是句子的结束。
	emailCharsHint = "本地部分只收字母、数字与 " + emailLocalSymbols + " 这几个符号，" +
		"域名只收字母、数字、连字符与点"
	// 下面每一句各由一处判据用，一句一件事，不合成一句总述：`alice@gmail` 这种
	// 漏了半截的写法，合成一句时会读到两条与它无关的条件，而他要改的只有一处。
	// `@` 那一条（数量与两边非空）落在两个分支里，其余各落在一个条件上。
	shapeReason        = "里的 @ 应当恰好一个，而且两边都不为空"
	dotsReason         = "的域名里要有一个点"
	dotPlacementReason = "两半都不能以点开头或结尾，也不能连着两个点"
	hyphenReason       = "的域名里，用点分开的每一段都不能以连字符开头或结尾"
)

// ValidateEmail 校验一个要写进 users.email 的邮箱地址。
//
// # 空串通过
//
// 空串（含只有空白的）不算格式不对：这一列「没填」在库里的样子是 NULL，而两条
// 写入路径都把空串落成 NULL（见 CreateUser 对空邮箱的处理）。把「没填」判成
// 格式错，使用者会被逼着编一个邮箱出来。
//
// # 收什么
//
// 去掉两头空白之后，判五件事：
//
//  1. `@` 恰好一个，两边都不为空；
//  2. 本地部分只由字母、数字与 `!#$%&'*+-/=?^_`{|}~.` 组成，域名只由字母、
//     数字、连字符与点组成。非 ASCII 的字母与数字也收（本地化的域名与本地部分
//     是真实存在的写法），别的非 ASCII 字符不收——全角逗号、emoji 这些出现在
//     地址里，几乎都是粘贴时一起带进来的；
//  3. 域名里至少有一个点；
//  4. 两半都不以点开头或结尾，也不出现连着两个点；
//  5. 域名里用点分开的每一段，都不以连字符开头或结尾。
//
// 这样收：`a@example.com`、`a@b.example.com`、`user+tag@example.com`、
// `first.last@example.com`、`A.B@Example.COM`、`用户@例え.jp`。这样拒：
// `not-an-email`、`@example.com`、`first@second@example.com`、`a b@example.com`、
// `a@example.com.`、`a@example`（域名没有点）、`a@-b.example.com`、
// `a,b@example.com`、`Bob <bob@example.com>`、`a@[127.0.0.1]`。用例里那两张表比
// 这里全，改判据要连着它们一起改。
//
// # 非 ASCII 收的是「任何 Unicode 字母与数字」
//
// 非 ASCII 的那一批只按 unicode.IsLetter / IsDigit 认，不查它属于哪张表，所以全角
// 的 `ａ`、西里尔与希腊的同形字都在这五条之内。要说清后果的是这一列的大小写不敏感
// （库里的类型是 citext，做的就是折叠大小写）：它只覆盖有大小写之分的那些，`Ａ` 与
// ASCII 的 `A`、西里尔 `а` 与拉丁 `a` 是**两个不同的值**，唯一约束分得开，两条会
// 并存。这不是这一版要改的行为，是要让人知道这一列实际收什么。
//
// # 长度不判
//
// 本地部分与整串的长度都不设上限（标准里本地部分到 64、整串到 254）：300 字的本地
// 部分照收。这是**有意留的边界**，不是漏了——这一版要挡住的是「写不成一个地址」
// 的那一类，长度是另一类问题（真实的地址会先被上游拒掉），要加就得单开一条判据、
// 单开一张用例表，并且说清超长那一次报的是哪一句。
//
// # 比标准窄的地方
//
// 这五条比标准里的地址语法窄，窄在四处，都是有意不收的：带引号的本地部分
// （`"a b"@example.com`）、方括号包起来的域名（`a@[127.0.0.1]`）、带尖括号或
// 注释的写法（`Bob <bob@example.com>`、`bob@example.com (comment)`）、没有点的
// 域名（`a@example`）。前三处是这一列不该出现的写法——它存的是地址本身，不是一个
// 信封上的收件人栏；存成带显示名的那一串，同一个人的另一种写法会被当成第二个邮箱
// （这一列上有唯一约束）。第四处的理由见下。
//
// 串里不能有空白不必单列一条：空格与制表符不在上面那张符号表里，不断行空格那种
// 非 ASCII 空白也不是字母数字，两条判据都到不了它们那儿。
//
// # 为什么不借 net/mail
//
// 它是现成的，但它那一套的宽严**跟着 Go 版本走**：同一台机器上，`a@[127.0.0.1]`
// 在 go1.22 那儿是拒的，go1.26 收了（而同一批输入里的其余几条没变）。这一列收什么
// 是对外行为——升级编译器顺手改了它，收严的那一天会拒掉使用者原本存得进去的邮箱，
// 放松的那一天会静默放行一批从前挡住的写法。所以允许哪些字符写死在上面那张表里，
// 改它要改的是这段注释、那张表，以及用例里那两张表。
//
// # 为什么要求域名里有一个点
//
// 没有点的域名几乎都是漏了半截（`alice@gmail`），而这一列今天只用来记联系方式
// 与查重。代价是**收不下真的本地域名**：`alice@corp` 会报参数错，而这一列没有
// 一条绕过校验的写法。两边都是坏结果，选了挡住的那一边——挡住会立刻报出来，
// 静默存下一个写错的，要等到真按这一列找人时才发现。报错里写着域名要有一个点，
// 所以被挡住的那一次说得清是为什么。
//
// 已经躺在库里的值不归它管：校验只在写之前做，读一行时再判一次格式，只会让
// 一行早就在那里的数据突然读不出来。
func ValidateEmail(email string) error {
	trimmed := strings.TrimSpace(email)
	if trimmed == "" {
		return nil
	}

	at := strings.Index(trimmed, "@")
	if at < 0 || at != strings.LastIndex(trimmed, "@") {
		return invalidEmail(trimmed, shapeReason)
	}
	local, domain := trimmed[:at], trimmed[at+1:]
	if local == "" || domain == "" {
		return invalidEmail(trimmed, shapeReason)
	}
	if !emailPartAllowed(local, emailLocalSymbols) ||
		!emailPartAllowed(domain, emailDomainSymbols) {
		return invalidEmail(trimmed, "里有不该出现在地址里的字符（"+emailCharsHint+"）")
	}
	if !strings.ContainsRune(domain, '.') {
		return invalidEmail(trimmed, dotsReason)
	}
	if badEmailDots(local) || badEmailDots(domain) {
		return invalidEmail(trimmed, dotPlacementReason)
	}
	if badEmailHyphens(domain) {
		return invalidEmail(trimmed, hyphenReason)
	}
	return nil
}

// invalidEmail 说清一次拒绝：哪个串、哪里不对。
//
// 每条判据各给自己那一句，而不是合成一句总述（「要写成 name@example.com 这样」）：
// 总述在恰好没碰坏被点名那一条的人眼里等于没说——他逐条对一遍全对，还是不知道
// 错在哪儿，只能猜着改。报的是去掉两头空白之后的那个串，与判据看的是同一个值。
func invalidEmail(email, reason string) error {
	return fmt.Errorf("%w：%q %s", ErrInvalidEmail, email, reason)
}

// emailPartAllowed 报告地址的某一半里，每个字符都在允许之列。
//
// 非 ASCII 只收字母与数字（见 ValidateEmail）：**任何**非 ASCII 字符都收的话，
// 全角逗号、emoji 这类东西会一起放进来。
func emailPartAllowed(part, symbols string) bool {
	for _, r := range part {
		if r > unicode.MaxASCII {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				return false
			}
			continue
		}
		if !isASCIILetterDigit(r) && r != '.' && !strings.ContainsRune(symbols, r) {
			return false
		}
	}
	return true
}

// badEmailDots 报告地址的某一半里，点号摆得不对：以点开头或结尾，或者连着两个点。
//
// 两半都判，不分开写：标准里本地部分与域名的点号摆放是同一条规矩（各段之间用
// 一个点隔开，每一段至少一个字符），分开写的话，只改了其中一处的那一天，两半的
// 规矩会不一样，而两边各自的用例都还是绿的。
func badEmailDots(part string) bool {
	return strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") ||
		strings.Contains(part, "..")
}

// badEmailHyphens 报告域名的某一段以连字符开头或结尾。
//
// 段的边界是点，与 badEmailDots 同一套分段：`a@b-c.example.com` 里的连字符在一段
// 中间，收；`a@-b.example.com`、`a@b-.example.com` 那样以连字符开头或结尾的，拒。
// 这一条与「域名里要有一个点」同一路——它挡住的几乎都是漏了半截的写法（少打了一个
// 字母），而真的长成那样的一段本来也不是能解析的主机名。
func badEmailHyphens(domain string) bool {
	for _, label := range strings.Split(domain, ".") {
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return true
		}
	}
	return false
}

// isASCIILetterDigit 报告 r 是不是 ASCII 的字母或数字。
//
// 不能拿 unicode.IsLetter 代替它：那个对非 ASCII 也返回真，而这里要的正是不把
// 那一类算进来（非 ASCII 由 emailPartAllowed 单独判）。
func isASCIILetterDigit(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

// Role 是用户角色的称呼。
//
// 迁移里写得很清楚：**所有角色都是用户**，merchant 与 admin 只是 users 表上
// 的布尔标记，不是独立实体。所以这里不建三张表，只把「称呼 → 标记」的
// 对应关系收在一处，免得命令行与将来别的调用方各写一份、又各自漂移。
//
// # 这两个标记今天还是**预留**，不是权限
//
// 全仓没有一处**鉴权判断**读 users.is_merchant / users.is_admin：写入点只有
// 建用户与 `user update` 两处（见 Flags），读方只有展示与筛选那一侧（`user
// list` 的角色栏与 `--role` 筛选，见 roleOf 与 rolePredicate），没有任何代码
// 按它放行或拒绝。于是 `--role` 今天只是**把称呼记下来**，它不改任何一处
// 行为——`channel create --user` 接受任何存在的用户，建完也不会去问这个人
// 是不是 merchant。
//
// 它们打算给管理面的鉴权用（#158）：那时「谁能挂渠道、谁能进管理接口」
// 才是被检查的。接上之前别把这里的注释读成「已经生效」——一句写下来却
// 没人执行的声明，比没有声明更容易让下一个人以为有人管。
type Role string

const (
	// RoleUser 是普通用户：能调推理、能被计费。
	RoleUser Role = "user"
	// RoleMerchant 是 merchant 角色的称呼：**预留**指「可以挂载上游渠道并获得分成」，
	// 今天不生效（没有一处代码按它放行或拒绝，见 Role 的注释）。
	RoleMerchant Role = "merchant"
	// RoleAdmin 是 admin 角色的称呼：**预留**指「可以进管理面」，今天不生效
	// （管理面本身还不存在，见 #158）。
	RoleAdmin Role = "admin"
)

// valid 判断 r 是不是三个已知角色之一。
func (r Role) valid() bool {
	switch r {
	case RoleUser, RoleMerchant, RoleAdmin:
		return true
	}
	return false
}

// ParseRole 把外部（命令行、配置文件）给的字符串解析成角色。
//
// 它不碰数据库，所以调用方能在连库之前就用它挡住写错的参数——
// 一个拼错的角色不该先连上库再报错。
func ParseRole(s string) (Role, error) {
	r := Role(strings.TrimSpace(s))
	if !r.valid() {
		return "", fmt.Errorf("未知的角色 %q：可取 %s、%s、%s",
			s, RoleUser, RoleMerchant, RoleAdmin)
	}
	return r, nil
}

// Flags 把角色翻译成 users 表上的两个标记。
//
// 读方只有展示与筛选那一侧（见 Role 的注释）：翻出来的值只是落库，没有任何
// 鉴权判断读它。所以它今天的作用是「把称呼记下来」，而把对应关系收在一处，
// 是为了将来接上时有唯一一处可改，不是因为它已经生效。
//
// # 为什么导出
//
// 写入侧有两处，都从这里取：CreateUser 整体按角色落值；`user update` 的
// --admin 也要把 is_merchant 一起置真（那条命令按旗标逐列写，只有这一格
// 需要「称呼 → 标记」这张表）。两处各写一份的话，改了一处之后，建出来的
// admin 与改出来的 admin 会是两行不同的数据，而两边各自的用例都照样通过
// ——这条不变式也写在迁移对 users.is_admin 的注释里。
func (r Role) Flags() (isMerchant, isAdmin bool) {
	switch r {
	case RoleMerchant:
		return true, false
	case RoleAdmin:
		// admin 同时也是 merchant：迁移里 users.is_admin 的注释这么写，而
		// 「admin 天然是 merchant」这条不变式只在**落库时**能保证——将来接上
		// 「谁能挂渠道」的判断时，两种建法（--role admin / --role merchant 再改）
		// 才不会给出不同答案。今天读它的只有展示与筛选，没有鉴权判断读它，
		// 所以这条也只是预留。
		return true, true
	default:
		return false, false
	}
}

// CreateUserParams 描述要创建的一个用户。
type CreateUserParams struct {
	// Username 是用户名，唯一（citext，大小写不敏感）。必填。
	// 不能整个是一个十进制整数：那样一个名字会被 `--user` 按 ID 解释，建出来就再也
	// 按名字读不回来（见 ValidateNewUsername）。
	Username string
	// Email 为空表示不填。填了要是一个邮箱地址的写法（见 ValidateEmail），
	// 且唯一（同样大小写不敏感）。
	Email string
	// Role 必须是三个已知角色之一，**没有默认值**：
	// 它是将来用于访问控制的那个字段（今天只落库、不参与鉴权判断，见 Role 的注释），
	// 让它随零值静默变成普通用户，等于把「忘了传」和「特意要普通用户」
	// 写成同一件事——而将来接上判断时，这个差别会变成权限的差别。
	Role Role
}

// CreateUser 建一个新用户，返回它的 ID 与用户名。
//
// # 为什么不幂等
//
// 重名时报错，而不是把已有用户改成这次的参数。静默改一个已有用户
// 比报错危险得多：用户名是人的标识，改掉它意味着**另一个人的**
// 归属、账单与渠道可能被悄悄挪走，而命令的退出码还是 0。
//
// # 错误分三类
//
//   - errors.Is(err, ErrUsernameTaken) / ErrEmailTaken：名字被占了，
//     换个名字就能继续，是使用者能处理的
//   - errors.Is(err, ErrUsernameIsIDRef)：这个名字的形状不能用（整串是一个十进制
//     整数），同样换个名字就能继续——它不连库就判得出来
//   - errors.Is(err, ErrUsernameHasControlChar)：这个名字的形状不能用（含控制
//     字符），去掉那个字符就能继续，同样不连库就判得出来
//   - errors.Is(err, ErrInvalidEmail)：这个邮箱的写法不认，换一个写法就能继续，
//     同样不连库就判得出来
//   - 其它：写入失败（连不上库、约束之外的错误），要人去查
func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) (User, error) {
	username := strings.TrimSpace(p.Username)
	if username == "" {
		return User{}, errors.New("用户名不能为空")
	}
	// 与「空用户名」同一条：这些校验在**自己这里**就挡住，不靠命令行那层拦着
	// （见 ValidateNewUsername 与 user_test.go 的 TestCreateUserRejectsEmptyInput）。
	// 落库之前挡住才拦得下这样一个用户——写进去之后它就在库里了。
	if err := ValidateNewUsername(username); err != nil {
		return User{}, err
	}
	if !p.Role.valid() {
		return User{}, fmt.Errorf("未知的角色 %q", p.Role)
	}
	// 邮箱的形状同样在落库之前判，判的是**真的要写进去的那个值**：空串表示
	// 不填这一列（下面落成 NULL），ValidateEmail 放行它。
	if err := ValidateEmail(p.Email); err != nil {
		return User{}, err
	}

	// 空邮箱存 NULL 而不是空串：email 上有唯一约束，而空串在 citext 里
	// 是一个**具体的值**——第二个不填邮箱的用户会撞上第一个的 ''。
	// NULL 不参与唯一性比较，所以「不填」必须真的落成 NULL。
	var email sql.NullString
	if e := strings.TrimSpace(p.Email); e != "" {
		email = sql.NullString{String: e, Valid: true}
	}

	// 这两列是**预留**：按角色写进去，读它的只有展示与筛选那一侧（见 Role 的注释）。
	isMerchant, isAdmin := p.Role.Flags()

	// status、created_at、updated_at 不写：它们都有默认值（active / now()），
	// 让默认值留在库里一处定义，命令行不必跟着抄一遍。
	var user User
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO users (username, email, is_merchant, is_admin)
		VALUES ($1, $2, $3, $4)
		RETURNING id, username`,
		username, email, isMerchant, isAdmin,
	).Scan(&user.ID, &user.Username)
	if err != nil {
		return User{}, createUserFailure(err, username, email.String)
	}
	return user, nil
}

// createUserFailure 把写用户语句的错误翻译成调用方认得出的错误。
//
// **它只翻译，不吞**：认不出来的错误原样包一层返回，绝不因为读不懂而
// 当成成功。
func createUserFailure(err error, username, email string) error {
	if mapped := usersUniqueFailure(err, username, email); mapped != nil {
		return mapped
	}
	return fmt.Errorf("写入用户失败: %w", err)
}

// usersUniqueFailure 把 users 表上的唯一约束冲突翻成调用方认得出的错误，
// 认不出来时返回 nil。
//
// 建与改两条路共用它：约束名与哨兵的对应关系只有一处，否则将来多一条唯一
// 约束时，漏改的那一侧会把冲突原样抛给使用者——而退出码表把「名字被占了」
// 算成使用者能处理的那一类，原文上抛会让它落回「其它失败」。
func usersUniqueFailure(err error, username, email string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		switch pgErr.ConstraintName {
		case usersUsernameUnique:
			return fmt.Errorf("%w: %q", ErrUsernameTaken, username)
		case usersEmailUnique:
			return fmt.Errorf("%w: %q", ErrEmailTaken, email)
		}
	}
	return nil
}

// ── 读侧 ────────────────────────────────────────────────────────────────
//
// 下面这一半与上面的写入路径服务同一张表上同一批列。分不开的两处是**角色**：
// `--role` 怎么写成两个标记（Role.Flags）与两个标记怎么读回一个称呼（roleOf）
// 是同一套映射的两半，它们必须一起改，所以放在同一个文件里。

// 用户状态，与迁移里 users.status 的 CHECK 约束一致。
const (
	// UserStatusActive 是正常状态：key 有效时就能调推理、能被计费。
	UserStatusActive = "active"
	// UserStatusSuspended 是暂停：他手里的 key 全部失效（见 store.go 的 lookup），
	// 但行还在、数据都留着，恢复只要把这一列改回去。
	UserStatusSuspended = "suspended"
	// UserStatusBanned 是封禁。它与暂停在今天的行为**完全一样**（都是
	// lookup 里那个 `!= active` 判据），分开只因为记录下来的原因不同：
	// 「临时停一下」与「不许再用」是两件事，而写进库里的东西要能区分它们。
	UserStatusBanned = "banned"
)

// UserStatuses 是三个用户状态，按 正常 → 暂停 → 封禁 排。
//
// 单开一个函数而不是在判据与报错信息里各写三遍：漏掉一处时，报错会说
// 「只认 active、suspended」，而 banned 其实也认——一句说错了自己边界的
// 错误信息比没有更坏。
func UserStatuses() []string {
	return []string{UserStatusActive, UserStatusSuspended, UserStatusBanned}
}

// ParseUserStatus 校验一个用户状态的取值。
//
// 两头空白先去掉，与 ParseRole 同一口径：取值是给人敲的，多一个空格不该判成不
// 认识，而两处不一致的表现是同一条命令上两种规矩（`--role " user "` 通过、
// `--status " active "` 报错）。校验走去掉空白之后那个串，报错里仍打调用方给的
// 原文——他该看到自己敲进去的东西。
//
// 取值不认识时**当场报错**，不静默回落成「不筛」。回落的那一面很坏：
// 一个写错的 --status 会让筛选少一层，命令照样退出 0，打出来的却是全部
// 用户，看起来像「原来这么多人是这个状态」。
func ParseUserStatus(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	for _, known := range UserStatuses() {
		if trimmed == known {
			return known, nil
		}
	}
	return "", fmt.Errorf("未知的用户状态 %q：只认 %s", s, strings.Join(UserStatuses(), "、"))
}

// roleOf 把 users 表上那两个标记反推成一个角色称呼。
//
// 它是 Role.Flags 在**读侧**的另一半：那一处按称呼写标记，这里按标记读称呼。
//
// 先看 is_admin：迁移里写着 admin 天然也是 merchant，所以一行只写了 is_admin
// 没写 is_merchant 的数据仍然算 admin。**造得出这样一行的路有两条**：直接改库，
// 以及绕过命令行直接调 UpdateUser——它按列写，不替调用方补那条不变式。读侧给不出
// 角色时，命令行那一栏就只能空着，而空着与「这一栏没填」在输出里分不出来。
func roleOf(isMerchant, isAdmin bool) Role {
	switch {
	case isAdmin:
		return RoleAdmin
	case isMerchant:
		return RoleMerchant
	default:
		return RoleUser
	}
}

// rolePredicate 把一个角色称呼折成「要挑哪些行」的两个标记条件。
//
// 它是 roleOf 的另一半，两者必须描述同一套映射：一个按标记读称呼，一个按称呼
// 挑标记。不一致的表现是清单里明明打着一个 admin，`--role admin` 却筛不出它。
//
// 返回的两个条件可以为 nil，nil 表示「这一列不参与筛选」。不能只返回一对 bool：
// merchant 的意思是「is_merchant 为真**且** is_admin 为假」，一个 bool 说不出来。
func rolePredicate(r Role) (isMerchant, isAdmin *bool) {
	yes, no := true, false
	switch r {
	case RoleAdmin:
		// 不要求 is_merchant 为真，理由见 roleOf。
		return nil, &yes
	case RoleMerchant:
		return &yes, &no
	default:
		return &no, &no
	}
}

// UserInfo 是一个用户在**读侧**的字段。
//
// 与 CreateUserParams 是两件事：那个是「要写成什么」，这个是「库里现在是什么」，
// 所以它带着 created_at / updated_at 这两个时刻——它们由库自己维护，命令行写不了，
// 只有读侧看得见（status 与两个角色标记都另有写入路径，见 UpdateUserParams 与
// Role.Flags）。
type UserInfo struct {
	// ID 是 users.id。
	ID int64
	// Username 是 users.username。
	Username string
	// Email 为空串表示这一列是 NULL：不填邮箱的用户存的是 NULL 而不是空串
	// （见 CreateUser），所以在这一侧「空串」只可能是「没填」。
	Email string
	// Status 取 UserStatuses 里的一个。
	Status string
	// Role 由 is_merchant / is_admin 两列反推（见 roleOf）。
	Role Role
	// DisplayName 为空串表示这一列是 NULL。
	DisplayName string
	// DeletedAt 是匿名化完成的时刻；nil 表示还没走到销户终态。
	//
	// 它与 Status 说的是两件事：Status 是鉴权读的那一列（deleted 之后账号不能
	// 再用），DeletedAt 回答「什么时候销的户」。users 行永远不删，所以这一列也是
	// 事后核对「这一行是什么时候变成终态的」的唯一来源。
	DeletedAt *time.Time
	// CreatedAt 与 UpdatedAt 是 users 表上那两列。
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListUsersFilter 是 ListUsers 的筛选条件。
//
// 两个字段各自「为空 = 不按它筛」：不给筛选条件与「筛一个空集」是两件事，
// 所以这里不用零值表示「一行都不要」。
type ListUsersFilter struct {
	// Status 为 "" 表示不按状态筛。
	Status string
	// Role 为 nil 表示不按角色筛。
	Role *Role
}

// userInfoColumns 是读侧要的那几列，ListUsers 与 UserByRef 共用。
//
// 两列可空文本在 SQL 里就折成空串：读侧只有「填了」与「没填」两种状态，而
// sql.NullString 那种「第三态」传出去以后每个调用点都要再判一次，判漏的
// 那一处会把 NULL 打成 `<nil>` 打给使用者看。
//
// is_merchant / is_admin 读的是**原始标记**，折成称呼的事只在 roleOf 里做一次。
const userInfoColumns = `id, username, COALESCE(email, ''), status,
	       is_merchant, is_admin, COALESCE(display_name, ''), deleted_at,
	       created_at, updated_at`

// ListUsers 列出用户，按 id 排序。
//
// 与 ListKeys 同一条约定：标识先由命令行解析、校验成具体取值再查，这里收的是
// 已经校验过的条件——两处各校验一遍的话，将来「--status 接受简写」这类改动
// 就得改两遍，而漏掉一处不会报错。
func (s *Store) ListUsers(ctx context.Context, f ListUsersFilter) ([]UserInfo, error) {
	var (
		status     *string
		isMerchant *bool
		isAdmin    *bool
	)
	if f.Status != "" {
		status = &f.Status
	}
	if f.Role != nil {
		isMerchant, isAdmin = rolePredicate(*f.Role)
	}

	// 三个条件都写成「为 NULL 就不过滤」，与 ListKeys 的
	// `$1::bigint IS NULL OR ...` 同一个写法：一条语句覆盖全部组合，
	// 而不是按有没有筛选拼字符串——拼出来的 SQL 每个分支都要各自看一遍。
	const query = `
		SELECT ` + userInfoColumns + `
		  FROM users
		 WHERE ($1::text IS NULL OR status = $1)
		   AND ($2::boolean IS NULL OR is_merchant = $2)
		   AND ($3::boolean IS NULL OR is_admin = $3)
		 ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query, status, isMerchant, isAdmin)
	if err != nil {
		return nil, fmt.Errorf("查询用户清单失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UserInfo
	for rows.Next() {
		info, err := scanUserInfo(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户行失败: %w", err)
	}
	return out, nil
}

// UserByRef 按「用户 ID 或用户名」读一个用户的全部读侧字段。
//
// 标识怎么解释与 ResolveUser 共用一处（见 userRefWhere），所以 `user get 12345`
// 与 `apikey create --user 12345` 认的是同一个人。
func (s *Store) UserByRef(ctx context.Context, ref string) (UserInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return UserInfo{}, errors.New("用户标识为空")
	}

	where, arg := userRefWhere(ref, 1)
	info, err := scanUserInfo(s.db.QueryRowContext(ctx,
		`SELECT `+userInfoColumns+` FROM users WHERE `+where, arg).Scan)

	if errors.Is(err, sql.ErrNoRows) {
		return UserInfo{}, fmt.Errorf("%w: %q", ErrUserNotFound, ref)
	}
	if err != nil {
		return UserInfo{}, fmt.Errorf("查询用户失败: %w", err)
	}
	return info, nil
}

// scanUserInfo 把一行用户读成 UserInfo。
//
// 收一个 scan 函数而不是 *sql.Rows，是因为 ListUsers 与 UserByRef 拿到的
// 是两个不同的东西（*sql.Rows 与 *sql.Row），而它们的 Scan 方法是同一个形状。
// 两处各扫一遍的话，下面是九列、手工对齐，漏一列的表现是后面所有列都串位。
func scanUserInfo(scan func(...any) error) (UserInfo, error) {
	var (
		info       UserInfo
		isMerchant bool
		isAdmin    bool
		deletedAt  sql.NullTime
	)
	if err := scan(&info.ID, &info.Username, &info.Email, &info.Status,
		&isMerchant, &isAdmin, &info.DisplayName, &deletedAt,
		&info.CreatedAt, &info.UpdatedAt); err != nil {
		return UserInfo{}, err
	}
	info.Role = roleOf(isMerchant, isAdmin)
	if deletedAt.Valid {
		info.DeletedAt = &deletedAt.Time
	}
	return info, nil
}

// ── 改一个已有用户 ──────────────────────────────────────────────────────
//
// 这一段两边都碰：定位用 userRefWhere（写侧），两个标记按 Role.Flags 与 roleOf
// 那一套映射写（读侧要用它读回来），返回的又是读侧那一份 UserInfo。所以它排在
// 文件末尾，而不是塞进上面某一半里。

// ErrNoChanges 表示这次修改没有任何字段要改。
//
// 单独报错而不是发一条只动 updated_at 的语句：后者会成功退出，而调用方以为
// 改动生效了（与 channels / catalog / pricing 的同名哨兵同一个理由）。命令行
// 在连库之前就挡住了这一档，这个哨兵留给绕过它的调用方；它同时让命令那一层
// 把这个错误归到「参数错」而不是「其他失败」——值不同，所以那边逐个登记
// （见 cmd/larkaxctl 的 exitCode）。
var ErrNoChanges = errors.New("没有要改的字段")

// UpdateUserParams 描述一次修改要动的列。
//
// 与 CreateUserParams 的分别在于**每一列都可以不改**：nil 表示「这一列不动」，
// 于是「没提到」与「写成一个零值」分得开——后者会把没打算碰的列一起改掉。
// 两个文本列还多一档：指向空串表示把这一列清成 NULL，那是「不填」在库里的样子
// （见 CreateUser 对空邮箱的处理）。
//
// 两个标记分开给，而不是收一个 Role：`--no-admin` 的意思是「只把 is_admin 置假，
// merchant 不动」，一个 Role 说不出来（见 Role.Flags 与 roleOf）。
type UpdateUserParams struct {
	// Status 为 nil 表示不改；给了必须是 UserStatuses() 里的一个。
	Status *string
	// DisplayName 为 nil 表示不改；指向空串（或只有空白）表示清成 NULL。
	DisplayName *string
	// Email 同上，只是这一列的写法要先过 ValidateEmail；空串（含只有空白的）
	// 表示清成 NULL，不是「一个写错的邮箱」。这一列有唯一约束，撞上别的用户时
	// 报 ErrEmailTaken。
	Email *string
	// IsMerchant 为 nil 表示不改。
	IsMerchant *bool
	// IsAdmin 为 nil 表示不改。
	IsAdmin *bool
}

// UpdateUser 按 ref 定位一个用户，改给出的那几列，返回改完之后的读侧字段。
//
// # 只碰给出的列
//
// SET 子句按「给没给」拼起来，没提到的列**在语句里根本不出现**。用
// `COALESCE($n, col)` 那类写法省掉拼接，就等于把 NULL 借去当「不改」的记号——
// 而 display_name 与 email 本来就可以是 NULL，两件事挤在同一个记号上，清空
// 这一档就再也表达不出来，只能靠再立一个约定去区分（那正是这一档想避免的）。
//
// # 定位与返回
//
// ref 的解释与 ResolveUser / UserByRef 共用 userRefWhere，三条路径认的是同一个人。
// 改完的那一行用 RETURNING 一次读回来（读侧九列与 ListUsers 共用一处定义），
// 于是调用方不必再查一次：再查一次会让「改完之后是什么」依赖两次查询之间世界
// 没有变，而调用方要拿它跟改之前比，比的必须是**这一条语句的**前后。
//
// 影响行数为 0 与「找不到这一行」是同一件事（RETURNING 一行都不吐），所以
// 落回 ErrUserNotFound，与按标识读一个用户时报的是同一个哨兵。
func (s *Store) UpdateUser(ctx context.Context, ref string, p UpdateUserParams) (UserInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return UserInfo{}, errors.New("用户标识为空")
	}

	sets, args, err := userUpdateSets(p)
	if err != nil {
		return UserInfo{}, err
	}
	if len(sets) == 0 {
		// 一列都没给。命令行那一侧在连库之前就挡住了，这里挡的是绕过它的
		// 调用方：发一条只动 updated_at 的语句会成功退出，而调用方以为改动
		// 生效了。
		return UserInfo{}, ErrNoChanges
	}
	// updated_at 每次都动：它回答的是「这一行最后一次被改是什么时候」，
	// 与改的是哪一列无关。也正因为每次都有它，这条语句永远不会变成空 SET。
	sets = append(sets, "updated_at = now()")

	where, arg := userRefWhere(ref, len(args)+1)
	args = append(args, arg)
	where += ` AND status <> '` + UserStatusDeleted + `'`
	if p.Status != nil {
		// 改状态不能把一行从「要求删除」里捞出来：那条路是 delete revert，
		// 它会把 deletion_prior_status 一起清掉。从 update 直接写 active，
		// 既跳过了「回到原来那一个状态」的判据，又把旧值留在那一列上。
		where += ` AND status <> '` + UserStatusDeletionRequested + `'`
	}

	info, err := scanUserInfo(s.db.QueryRowContext(ctx,
		`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE `+where+
			` RETURNING `+userInfoColumns, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return UserInfo{}, s.updateUserNoRows(ctx, ref)
	}
	if err != nil {
		return UserInfo{}, updateUserFailure(err, p.Email)
	}
	return info, nil
}

// updateUserNoRows 在 UPDATE 一条也没改到时判断是「没有这个人」还是「这个人已经
// 到终态」——两者在 RETURNING 上都表现为一行不出。
//
// 终态那一句写在 WHERE 里而不是先查后改：先查后改中间那道缝里正好能塞进一次
// 匿名化，而匿名化是不可逆的，漏过去的那一次会把占位身份写回成真身份。
// 命令行把 ErrDeletionAlreadyDeleted 归成冲突（退出 4），与 delete 家族的报错
// 同一档。
func (s *Store) updateUserNoRows(ctx context.Context, ref string) error {
	where, arg := userRefWhere(ref, 1)
	var (
		status   string
		username string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT status, username FROM users WHERE `+where, arg,
	).Scan(&status, &username)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %q", ErrUserNotFound, ref)
	}
	if err != nil {
		return fmt.Errorf("查询用户状态失败: %w", err)
	}
	switch status {
	case UserStatusDeleted:
		return fmt.Errorf(
			"%w：账号 %q 已经匿名化，终态不可逆，不能再改这一行的任何一列",
			ErrDeletionAlreadyDeleted, ref)
	case UserStatusDeletionRequested:
		return fmt.Errorf(
			"%w：要改状态请先用 larkaxctl user delete revert --user %s，它会把状态"+
				"回到打标之前的那一个",
			ErrDeletionAlreadyRequested, username)
	}
	return fmt.Errorf("%w: %q", ErrUserNotFound, ref)
}

// userUpdateSets 把一次修改编成 SET 片段与参数。
//
// 与 internal/catalog 的 modelUpdateSets 同一个写法，理由也相同：它只做校验与
// 编码、不碰数据库，可以单独读；而且它**没有函数作用域上的 err**，每个分支各自
// 声明，作用域互不重叠，也就不存在遮蔽（.golangci.yml 里 govet 开了 enable-all，
// 全写在一个函数里时最上面那次校验声明的 err 会被下面每个 `:=` 遮住）。
//
// 状态与邮箱在这里都再校验一次（命令行那侧已经校验过，见 cmd/larkaxctl 的
// user update）：两处走的是同一个 ParseUserStatus、同一个 ValidateEmail，所以
// 不存在「改一处漏一处」；换来的是绕过命令行的调用方拿到一句能读的报错，而不是
// CHECK 约束的原文。
func userUpdateSets(p UpdateUserParams) ([]string, []any, error) {
	var sets []string
	var args []any
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}
	// addText 把空串（含只有空白的）写成 NULL：那一列在库里的「不填」是 NULL，
	// 而读侧把 NULL 与空串都折成「没填」——再写进去一个空串只让「没填」多出
	// 一种存法，而对 email 尤其坏：空串在 citext 里是一个具体的值，唯一约束
	// 只容得下一个（见 CreateUser）。
	addText := func(column string, value *string) {
		if trimmed := strings.TrimSpace(*value); trimmed != "" {
			add(column, trimmed)
			return
		}
		add(column, nil)
	}

	if p.Status != nil {
		parsed, err := ParseUserStatus(*p.Status)
		if err != nil {
			return nil, nil, err
		}
		add("status", parsed)
	}
	if p.Email != nil {
		// 与状态同一处：命令行那侧已经判过一遍（见 cmd/larkaxctl 的
		// userUpdateParams），两处走的是同一个 ValidateEmail。绕过命令行的调用方
		// 从这里拿到的是同一句话，而不是让一个写错的邮箱进库、等到撞上唯一约束
		// 才发现。空串过它那一关，由 addText 落成 NULL——那是「清掉这一列」。
		if err := ValidateEmail(*p.Email); err != nil {
			return nil, nil, err
		}
		addText("email", p.Email)
	}
	if p.DisplayName != nil {
		// 与邮箱同一条：命令行那侧已经判过一遍（见 cmd/larkaxctl 的
		// userUpdateParams），这里挡的是绕过命令行的调用方——否则这一列
		// 里会进一个把表格与终端打坏的值。
		if err := ValidateDisplayName(*p.DisplayName); err != nil {
			return nil, nil, err
		}
		addText("display_name", p.DisplayName)
	}
	if p.IsMerchant != nil {
		add("is_merchant", *p.IsMerchant)
	}
	if p.IsAdmin != nil {
		add("is_admin", *p.IsAdmin)
	}
	return sets, args, nil
}

// updateUserFailure 把改用户语句的错误翻译成调用方认得出的错误。
//
// 只能撞上 email 那一条唯一约束（username 不在能改的列里，所以不传它，那一支
// 也永远碰不到），但仍然走共用的 usersUniqueFailure：认不出来的原样包一层返回，
// 绝不因为读不懂而当成成功。
func updateUserFailure(err error, email *string) error {
	if email != nil {
		if mapped := usersUniqueFailure(err, "", strings.TrimSpace(*email)); mapped != nil {
			return mapped
		}
	}
	return fmt.Errorf("修改用户失败: %w", err)
}
