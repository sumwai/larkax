package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/money"
)

// apiKeyHelp 是 apikey 的两版帮助文本（见 helpText）。
var apiKeyHelp = helpText{short: apiKeyShortUsage, long: apiKeyUsage}

// apiKeyShortUsage 是 apikey 的短版：`-h` / `--help` 与参数写错时打的那一份。
const apiKeyShortUsage = `larkaxctl apikey 管理 API Key。

用法：
  larkaxctl apikey create --user <ID 或用户名> [--name <备注>]
                          [--expires-at <RFC3339>] [--rpm <整数>] [--quota <金额>]
  larkaxctl apikey update --id <ID> [--status <active|disabled|revoked>]
                                  [--name <备注>] [--rpm <整数> | --clear-rpm]
                                  [--quota <金额> | --clear-quota]
                                  [--expires-at <RFC3339> | --clear-expires-at]
  larkaxctl apikey get --id <ID>
  larkaxctl apikey list [--user <ID 或用户名>]

create 的明文只打印这一次（库里存的是哈希），它单独打在 stdout 上。

--rpm 与 --quota 都按明文给：0 不是「不限」，是「一个都不放行」；负数一律拒绝。

更细的边界：larkaxctl help apikey。
`

// apiKeyUsage 是 apikey 的长版：`larkaxctl help apikey` 与 `apikey help`。
const apiKeyUsage = `larkaxctl apikey 管理 API Key。

用法：
  larkaxctl apikey create --user <ID 或用户名> [--name <备注>] [--expires-at <RFC3339>]
                          [--rpm <整数>] [--quota <金额>]
  larkaxctl apikey update --id <ID> [--status <active|disabled|revoked>]
                                  [--name <备注>] [--rpm <整数> | --clear-rpm]
                                  [--quota <金额> | --clear-quota]
                                  [--expires-at <RFC3339> | --clear-expires-at]
  larkaxctl apikey get --id <ID>
  larkaxctl apikey list [--user <ID 或用户名>]

create 生成一把新 key，只把哈希入库。

**明文只打印这一次**：库里存的是 SHA-256 哈希，它不可逆，
丢了没有任何办法取回，只能重建一把。

明文单独打到 stdout，其余信息打到 stderr，所以

  larkaxctl apikey create --user alice > key.txt

保存下来的文件里只有明文，不会混进前缀、id 与提示语。

get 打一把 key 的现状：状态、备注、两列额度、过期与最近使用时刻。它读的是
list 那一行的同一份数据，只是收成一把。四处没设过的值打成「继承」「不过期」
「从未」（每分钟与额度都打「继承」），与 list 一致。

# 额度

两列额度都按**明文**给，不填表示 NULL：

  --rpm              每分钟请求额度。**0 不是「不限」，是「一个都不放行」。**
                     不填（或 update 时给 --clear-rpm）写回 NULL，
                     即「继承用户默认」——users 表上目前没有这样一个默认值，
                     所以它等价于不限。
  --quota            这把 key 累计可花费的金额上限，单位是**该用户钱包的
                     资产品种**（见 user_wallets.asset）。最多 8 位小数。
                     0 与不填的区别同上。

负数一律拒绝：限流那一侧把负数也当成「一个都不放行」，而写下 -60 的人
几乎总是想写 60。

update 一次可以改多项，只改命令行上**真的给出**的那几项：没给的列连写都
不写一次。一项都没给时报参数错——一条什么都没改的命令与一条改成功的命令
在屏幕上一模一样。

# 状态与有效期

  --status           取 active、disabled、revoked 之一。disabled 与 revoked
                     在鉴权那一侧都会被拒绝，区别只记在原因上（观测里分别
                     计数）。一把停用的 key 可以用 --status active 恢复。

  --expires-at       一个 RFC3339 时刻，到点之后这把 key 自动不可用。
  --clear-expires-at 写回「不过期」。

过去的时刻会被拒绝：要立刻停掉一把 key 就改 --status，那样它在鉴权侧记下的
原因是「被停用」，而设一个已经过去的过期时刻记下的是「已过期」——两种情形
要分的是下一步该找谁。

--name 给空串表示「没有备注」，与 create 不给 --name 是同一个意思：列是
NOT NULL，空串就是它的「没有」。

# 权限

这四个子命令都不需要 LARKAX_MASTER_KEYS：它们不碰渠道凭证，也就不碰
信封加密。主密钥是 rotate 与 channels 那一组的事。

# 别名

下面几个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -u <ID 或用户名>         同 --user，create 与 list 都认
  --rate-limit-rpm <整数>  同 --rpm
  --clear-rate-limit-rpm   同 --clear-rpm
  --quota-limit <金额>     同 --quota
  --clear-quota-limit      同 --clear-quota

两个额度**不开简写**：它们是限额，写短了容易看错也容易按错，要的是完整写出来。
apikey 下唯一的简写是 -u（--user 的别名，create 与 list 认它）。

更细的约定见 docs/cli-reference.md 的「apikey」一节。
`

// apiKeyTimeout 是这条命令连库与写库的时间上限。
//
// 命令行要有上限，否则一次卡住的连接会挂到人发现为止。
// 30 秒对一条单行插入来说宽得离谱，取这个值只是为了让「卡住了」
// 有个终点，而不是把等待当成正常。
const apiKeyTimeout = 30 * time.Second

func runAPIKey(args []string) error {
	fs := flag.NewFlagSet("apikey", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, apiKeyShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageErr(errors.New("apikey 需要子命令"))
	}
	action, rest := rest[0], rest[1:]

	switch action {
	case "create":
		return runAPIKeyCreate(rest)
	case "update":
		return runAPIKeyUpdate(rest)
	case "get":
		return runAPIKeyGet(rest)
	case "list":
		return runAPIKeyList(rest)
	default:
		return usageErr(fmt.Errorf("未知的 apikey 子命令 %q", action))
	}
}

func runAPIKeyCreate(args []string) error {
	// 在**函数体第一行**拦下来，早于 flag 解析、更早于连库：写类命令在 json
	// 模式下必须一个字节都不写库。放在写库之后就成了「先把 key 建出来，再报
	// 不支持」——而调用方只看到一句参数错，于是重跑一次（见 format.go）。
	if err := jsonUnsupported("apikey create"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, apiKeyShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	name := fs.String("name", "", "")
	expiresAt := fs.String("expires-at", "", "")
	rpm := legacyStringFlag(fs, "rpm", "rate-limit-rpm", "")
	quota := legacyStringFlag(fs, "quota", "quota-limit", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("create 不接受位置参数，得到 %q：用户用 --user 指定", fs.Arg(0)))
	}
	if *user == "" {
		// 不打整段用法：人已经会用这条命令了，只是漏了一个必填项。用法留给
		// 「还没有开始用」的那几处（缺子命令 / 未知子命令 / flag 解析失败）。
		return usageErr(errors.New("缺少 --user"))
	}

	var expires *time.Time
	if *expiresAt != "" {
		// 已过去的时刻直接拒绝，而不是建一把立刻不可用的 key：那种情况几乎
		// 只可能是写错了年份（2025 写成 2024），而它的表现是「刚建好的 key
		// 就说无效」——最费时的一类排查。auth.Create 本身不拦：库层允许建
		// 一把已过期的 key（造测试数据、补历史数据都要用到），拦在交互式
		// 命令这一层才合适。
		t, err := parseFutureExpiresAt(*expiresAt, "这样的 key 建出来就是不可用的")
		if err != nil {
			return usageErr(err)
		}
		expires = &t
	}

	limits, err := parseLimits(fs, *rpm, *quota)
	if err != nil {
		return usageErr(err)
	}

	return withDB(apiKeyTimeout, func(ctx context.Context, db *sql.DB) error {
		store := auth.New(db)
		// 先解析用户再建 key：外键约束也会挡下不存在的用户，但那时错误信息
		// 来自数据库（一句外键冲突），而这里能说清是哪个标识没对上。
		owner, err := store.ResolveUser(ctx, *user)
		if err != nil {
			return err
		}

		created, err := store.Create(ctx, auth.CreateParams{
			UserID:       owner.ID,
			Name:         *name,
			ExpiresAt:    expires,
			RateLimitRPM: limits.rpm,
			QuotaLimit:   limits.quota,
		})
		if err != nil {
			return err
		}

		printCreatedKey(humanStdout(), humanStderr(), owner, created, expires, limits)
		return nil
	})
}

func runAPIKeyUpdate(args []string) error {
	// 与 create 同一条：必须在改任何一列之前拦下来。
	if err := jsonUnsupported("apikey update"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("apikey update", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, apiKeyShortUsage) }
	id := fs.Int64("id", 0, "要改的 API Key 的 id（apikey list 里能看到）")
	status := fs.String("status", "", "")
	name := fs.String("name", "", "")
	expiresAt := fs.String("expires-at", "", "")
	clearExpiresAt := fs.Bool("clear-expires-at", false, "")
	rpm := legacyStringFlag(fs, "rpm", "rate-limit-rpm", "")
	clearRPM := fs.Bool("clear-rpm", false, "")
	boolAlias(fs, clearRPM, "clear-rate-limit-rpm")
	quota := legacyStringFlag(fs, "quota", "quota-limit", "")
	clearQuota := fs.Bool("clear-quota", false, "")
	boolAlias(fs, clearQuota, "clear-quota-limit")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("update 不接受位置参数，得到 %q：要改哪一把 key 用 --id 指定", fs.Arg(0)))
	}

	seen := providedFlags(fs)
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id：要改哪一把 key"))
	}

	params, err := updateParams(seen,
		limitFlag{value: *rpm, clear: *clearRPM},
		limitFlag{value: *quota, clear: *clearQuota},
		metaFlags{status: *status, name: *name, expiresAt: *expiresAt, clearExpires: *clearExpiresAt})
	if err != nil {
		// 这几条错误（负数、小数位过多、两个旗标打架、状态取值不认识、没有要改
		// 的东西）都在信息里点了名，已经够看懂，不再附带整段用法——与 create /
		// user 一样，用法只在「还没开始用」时打。
		return usageErr(err)
	}

	return withDB(apiKeyTimeout, func(ctx context.Context, db *sql.DB) error {
		store := auth.New(db)

		// 先读一次，为的是回显里那个「从什么」。它不是这次写的一部分：写由
		// 下面那一条语句独立完成，认的是 id 与显式给出的字段。两次读之间若有
		// 别人插进来改同一把 key，回显的**原值**会旧一格；新值取自写语句本身，
		// 不会。为了这一句回显去开一个事务不值得，而这条路径上没有并发写它的人。
		before, err := store.GetKey(ctx, *id)
		if err != nil {
			return err
		}

		after, err := store.UpdateKey(ctx, *id, params)
		if err != nil {
			return err
		}

		printUpdatedKey(humanStderr(), before, after, params)
		return nil
	})
}

func runAPIKeyGet(args []string) error {
	fs := flag.NewFlagSet("apikey get", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, apiKeyShortUsage) }
	id := fs.Int64("id", 0, "要看的 API Key 的 id（apikey list 里能看到）")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("get 不接受位置参数，得到 %q：要看哪一把 key 用 --id 指定", fs.Arg(0)))
	}
	if *id <= 0 {
		return usageErr(errors.New("缺少 --id：要看哪一把 key"))
	}

	return withDB(apiKeyTimeout, func(ctx context.Context, db *sql.DB) error {
		key, err := auth.New(db).GetKey(ctx, *id)
		if err != nil {
			return err
		}
		// 与 list 同一条约定：JSON 是另一条渲染路径，不是把上面那张表转成 JSON
		// （见 format.go 开头）。
		if jsonOutput() {
			return emitJSON(os.Stdout, apiKeyGetDoc(key), nil)
		}
		printKeyDetail(humanStdout(), key)
		return nil
	})
}

func runAPIKeyList(args []string) error {
	fs := flag.NewFlagSet("apikey list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, apiKeyShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q：按用户过滤用 --user", fs.Arg(0)))
	}

	return withDB(apiKeyTimeout, func(ctx context.Context, db *sql.DB) error {
		store := auth.New(db)

		// --user 先解析成 ID 再查：让「按用户名查不到」与「按 ID 查不到」
		// 都停在同一步，而不是一个走解析、一个走 SQL 的 WHERE。
		var userID *int64
		if *user != "" {
			owner, err := store.ResolveUser(ctx, *user)
			if err != nil {
				return err
			}
			userID = &owner.ID
		}

		keys, err := store.ListKeys(ctx, userID)
		if err != nil {
			return fmt.Errorf("列出 API Key 失败: %w", err)
		}
		// JSON 是**另一条渲染路径**，不是把上面那张表转成 JSON：表格那一档
		// 用 `-`、「继承」铺空栏，转出来的 JSON 里金额会变成字符串、空值会
		// 变成占位符（见 format.go 开头）。
		if jsonOutput() {
			return emitJSON(os.Stdout, apiKeyListDoc(keys), nil)
		}
		printKeyList(humanStdout(), keys)
		return nil
	})
}

// limitFlag 是某一项额度在命令行上给的那一对旗标：一个设值、一个清空。
//
// 收成一个结构体而不是四个平铺的参数：updateParams 要收两组这样的东西，
// 平铺开就是 rpm 字符串、rpm 布尔、quota 字符串、quota 布尔四个相邻的
// 位置参数，而其中两个是字符串、两个是布尔——调用点写反了顺序照样能编过。
// 这不是假设：写下这段注释的时候，用例里就刚写反过一次。
type limitFlag struct {
	value string
	clear bool
}

// metaFlags 是 update 上「状态 / 备注 / 过期」三项在命令行上给的值。
//
// 同样收成结构体：updateParams 里额度那两项已经是两个同形状的位置参数，
// 再平铺四个（其中还有一个布尔）的话，把 name 与 status 写反只是又一次
// 「编得过、跑出来是另一件事」。
type metaFlags struct {
	status       string
	name         string
	expiresAt    string
	clearExpires bool
}

// updateParams 把 update 的旗标读成一次修改意图。
//
// seen 是「命令行上真的出现过哪些旗标」（见 providedFlags）。每对旗标有三种
// 状态——不改 / 设值 / 写回 NULL——而判据要**同时**看 seen 与 --clear-* 的值：
//
//   - 只看值：「--rate-limit-rpm 0」与「没给这个旗标」的字符串都是 "0" 或 ""，
//     分不开，而那两件事正相反（一个都不放行 / 不改）
//   - 只看 seen：`--clear-rate-limit-rpm=false` 会被当成「要清空」，
//     而它其实是「不要清空」，与不给它是同一件事
//
// 抽成一个不碰数据库的函数，是因为这几态正是最容易写错、也最该被单测钉住的
// 地方：把「清空」实现成「写成 0」，表现是每一次「恢复默认」都变成一次静默的
// 封禁，而它不报任何错。
var (
	// rpmFlagNames / quotaFlagNames 是这两项额度在命令行上的全部写法。
	//
	// 判「这次到底给没给」时要把每一种写法都查（见 flagGiven）。漏掉一种的表现
	// 是「--rpm 60 --clear-rpm 不报冲突」：两个写着相反意思的旗标一起生效，
	// 后一个安静地赢，所以名字组与注册那一段要一起看。
	rpmFlagNames   = []string{"rpm", "rate-limit-rpm"}
	quotaFlagNames = []string{"quota", "quota-limit"}
)

func updateParams(seen map[string]bool, rpm, quota limitFlag, meta metaFlags) (auth.KeyParams, error) {
	var params auth.KeyParams

	switch rpmGiven := flagGiven(seen, rpmFlagNames...); {
	case rpm.clear && rpmGiven:
		return auth.KeyParams{}, errors.New("--rpm 与 --clear-rpm 不能同时给：" +
			"一个是设值，一个是写回 NULL")
	case rpm.clear:
		// RateLimitRPM 留 nil：SetRateLimit 为真而指针为 nil 就是「写回 NULL」。
		params.SetRateLimit = true
	case rpmGiven:
		parsed, err := parseRateLimitRPM(rpm.value)
		if err != nil {
			return auth.KeyParams{}, err
		}
		params.SetRateLimit, params.RateLimitRPM = true, parsed
	}

	switch quotaGiven := flagGiven(seen, quotaFlagNames...); {
	case quota.clear && quotaGiven:
		return auth.KeyParams{}, errors.New("--quota 与 --clear-quota 不能同时给：" +
			"一个是设值，一个是写回 NULL")
	case quota.clear:
		params.SetQuota = true
	case quotaGiven:
		parsed, err := parseQuotaLimit(quota.value)
		if err != nil {
			return auth.KeyParams{}, err
		}
		params.SetQuota, params.QuotaLimit = true, parsed
	}

	switch expiresGiven := flagGiven(seen, "expires-at"); {
	case meta.clearExpires && expiresGiven:
		return auth.KeyParams{}, errors.New("--expires-at 与 --clear-expires-at 不能同时给：" +
			"一个是设值，一个是写回「不过期」")
	case meta.clearExpires:
		// ExpiresAt 留 nil：SetExpiresAt 为真而指针为 nil 就是「写回 NULL」。
		params.SetExpiresAt = true
	case expiresGiven:
		parsed, err := parseFutureExpiresAt(meta.expiresAt, "改完就不可用了")
		if err != nil {
			return auth.KeyParams{}, err
		}
		params.SetExpiresAt, params.ExpiresAt = true, &parsed
	}

	if flagGiven(seen, "status") {
		status, err := validateKeyStatus(meta.status)
		if err != nil {
			return auth.KeyParams{}, err
		}
		params.SetStatus, params.Status = true, status
	}

	if flagGiven(seen, "name") {
		// 空串照收：它表示「没有备注」，与 create 不给 --name 是同一件事。
		// 把它当成「没给」会让清空备注变成一条改不动任何东西的命令。
		params.SetName, params.Name = true, meta.name
	}

	// 一条什么都没要求的 update 是「看起来做了、其实没做」的命令，而它的
	// 表现与成功一模一样。零值在这里的意思是「不改」，所以这个检查不能省。
	if !params.SetRateLimit && !params.SetQuota && !params.SetStatus &&
		!params.SetName && !params.SetExpiresAt {
		return auth.KeyParams{}, errors.New("没有要改的东西：--status、--name、" +
			"--expires-at、--rpm、--quota 至少要给一个（要清空的给对应的 --clear-*）")
	}

	return params, nil
}

// validateKeyStatus 检查 --status 的取值，返回归一后的（去掉首尾空白的）那个。
//
// 取值来自 auth.KeyStatuses：库里那条 CHECK 约束、鉴权那一侧的判定与命令行
// 收不收这个值，说的是同一件事。这里自己抄一份的话，改了一处不会报错——表现
// 是命令行收下一个鉴权侧不认的状态，而它要等到有人拿这把 key 打请求才露出来。
func validateKeyStatus(value string) (string, error) {
	allowed := auth.KeyStatuses()
	status := strings.TrimSpace(value)
	for _, ok := range allowed {
		if status == ok {
			return status, nil
		}
	}
	return "", fmt.Errorf("--status 取 %s 之一，得到 %q",
		strings.Join(allowed, "、"), value)
}

// parseFutureExpiresAt 解析 --expires-at：RFC3339，且必须在将来。
//
// consequence 是「给了一个过去的时刻会怎样」那半句，由调用方补：create 上是
// 「建出来就不可用」，update 上是「改完就不可用」。判据（认 RFC3339、必须在
// 将来）只有这一份——两处各写一遍时，放宽的那一处不会有任何东西报出来，而它
// 表现为「刚建好的 key 就说无效」这类最费时的排查。
func parseFutureExpiresAt(plain, consequence string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, plain)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"--expires-at 不是 RFC3339 时间（例如 2026-12-31T00:00:00Z）: %w", err)
	}
	if !t.After(time.Now()) {
		return time.Time{}, fmt.Errorf("--expires-at %s 已经过去了：%s",
			t.Format(time.RFC3339), consequence)
	}
	return t, nil
}

// keyLimits 是一次解析出来的两列额度，nil 表示 NULL。
type keyLimits struct {
	rpm   *int
	quota *money.Amount
}

// parseLimits 解析 create 的两个额度旗标。
//
// create 与 update 对「不填」的解释不同：create 不填就是 NULL（新行本来
// 也没有值），update 不填是「不改这一项」。所以这里只处理前者，后者的
// 三态判断留在 runAPIKeyUpdate 里——把两套语义塞进同一个函数，读的人
// 就得先想清楚自己在看哪一套。
func parseLimits(fs *flag.FlagSet, rpm, quota string) (keyLimits, error) {
	var out keyLimits
	seen := providedFlags(fs)

	if flagGiven(seen, rpmFlagNames...) {
		parsed, err := parseRateLimitRPM(rpm)
		if err != nil {
			return keyLimits{}, err
		}
		out.rpm = parsed
	}

	if flagGiven(seen, quotaFlagNames...) {
		parsed, err := parseQuotaLimit(quota)
		if err != nil {
			return keyLimits{}, err
		}
		out.quota = parsed
	}

	return out, nil
}

// providedFlags 返回命令行上**真的出现过**的旗标名。
//
// 不能靠「值等于零值」来判断：--rate-limit-rpm 0 是一个有意义的取值
// （一个都不放行），它与「没给这个旗标」必须分得开，而两者的字符串表示
// 分别是 "0" 与 ""——后者又与「给了空串」撞在一起。flag 包自己记着谁
// 出现过，问它比猜可靠。
func providedFlags(fs *flag.FlagSet) map[string]bool {
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	return seen
}

// parseRateLimitRPM 解析每分钟请求额度的明文值。
//
// 返回 nil 表示写 NULL（继承用户默认）。
func parseRateLimitRPM(plain string) (*int, error) {
	trimmed := strings.TrimSpace(plain)
	if trimmed == "" {
		return nil, errors.New("--rpm 需要一个整数（0 表示一个都不放行），得到空值")
	}

	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return nil, fmt.Errorf("--rpm 需要整数，得到 %q", plain)
	}
	// 列是 integer（32 位）。超出上界时数据库会报一句 numeric out of range，
	// 那句话没说清是哪个旗标，而这条命令上只有这一个整数旗标之外的东西……
	// 所以在这里拦下来，把话说在旗标的名字上。
	if n > math.MaxInt32 {
		return nil, fmt.Errorf("--rpm 最大 %d，得到 %d：列是 32 位整数",
			int64(math.MaxInt32), n)
	}
	if n < 0 {
		return nil, fmt.Errorf("--rpm 不能是负数，得到 %d："+
			"限流那一侧把负数与 0 一样当成「一个都不放行」，而写下负数的人几乎总是漏了一个减号",
			n)
	}
	return &n, nil
}

// quotaScale 是 api_keys.quota_limit 的小数位数，与迁移里的 numeric(20, 8) 一致。
const quotaScale = 8

// parseQuotaLimit 解析金额上限的明文值。
//
// 返回 nil 表示写 NULL（继承用户默认）。
func parseQuotaLimit(plain string) (*money.Amount, error) {
	trimmed := strings.TrimSpace(plain)
	if trimmed == "" {
		return nil, errors.New("--quota 需要一个金额（0 表示一个都不放行），得到空值")
	}

	// 先自己数一遍小数位。money.Parse 会把多出来的位四舍五入，而额度是一个
	// 会被拿去比较的边界值——「存进去的数与敲进去的数不一样」在这里是不该
	// 发生的事，哪怕差的只是最后一位。
	if _, frac, hasDot := strings.Cut(trimmed, "."); hasDot && len(frac) > quotaScale {
		return nil, fmt.Errorf("--quota 最多 %d 位小数，得到 %q："+
			"列是 numeric(20, %d)，多出来的位存不进去", quotaScale, plain, quotaScale)
	}

	amount, err := money.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("--quota 不是金额（例如 12.34）: %w", err)
	}
	if amount.IsNegative() {
		return nil, fmt.Errorf("--quota 不能是负数，得到 %s：金额上限没有负数这一档",
			amount)
	}
	return &amount, nil
}

// printCreatedKey 打印新 key。
//
// 明文与其余信息分走两个流：`> key.txt` 是保存一把 key 最自然的动作，
// 而元信息混进去会让那个文件没法被程序直接读走（粘贴到客户端配置里时
// 多一行就少一个能用的 key）。
//
// 归属人只打名字，不带 `(id=N)`：同一行里 `用户=` 已经把「这把 key 是谁的」
// 说清楚了，再挂一个用户 ID 是把身份说两遍（见 docs/cli-output.md 的
// 「实体只点一次身份」）。要用户 ID 就在造用户那一步接走——那正是
// `larkaxctl user create` 往 stdout 上只打一个 ID 的用途。
func printCreatedKey(stdout, stderr io.Writer, owner auth.User, c auth.Created,
	expires *time.Time, limits keyLimits,
) {
	_, _ = fmt.Fprintf(stderr, "已创建 API Key id=%d prefix=%s 用户=%s 过期=%s 每分钟=%s 额度=%s\n",
		c.ID, c.Prefix, owner.Username,
		formatTime(expires, "不过期"), formatRPM(limits.rpm), formatQuota(limits.quota))
	_, _ = fmt.Fprintln(stderr, "明文只打印这一次，请立即保存：库里只有哈希，无法再取回。")
	_, _ = fmt.Fprintln(stdout, c.Plaintext)
}

// printUpdatedKey 回显这次改了哪几项、各从什么变成什么。
//
// 回显是必要的：update 的参数里「不改」与「清空」在命令行长得很像
// （一个是不给旗标，一个是给 --clear-*），把结果打出来才能让人当场
// 确认自己做的正是想做的那一件事。
//
// 打「从什么变成什么」而不是一句「已更新」，是因为要确认的是**那几项的值**：
// 一句「已更新」在把 --name 敲成 --status 时照样是它。列出的是**命令行上
// 给出**的那几项，不是「两次读出来不一样」的那几项——后者在给出一个与库里
// 相同的值时什么都不打，读的人分不清「没生效」与「本来就是它」。
//
// 走 stderr：「写成功之后只回一句确认」的那一档统一在这里，stdout 上只留能被
// 脚本直接接走的值（见 streams_test.go 的 TestWriteEchoesAppearOnlyOnStderr）。
func printUpdatedKey(w io.Writer, before, after auth.Key, p auth.KeyParams) {
	var parts []string
	if p.SetStatus {
		parts = append(parts, fmt.Sprintf("状态 从 %s 改为 %s", before.Status, after.Status))
	}
	if p.SetName {
		parts = append(parts, fmt.Sprintf("备注 从 %s 改为 %s",
			orDash(before.Name), orDash(after.Name)))
	}
	if p.SetRateLimit {
		parts = append(parts, fmt.Sprintf("每分钟 从 %s 改为 %s",
			formatRPM(before.RateLimitRPM), formatRPM(after.RateLimitRPM)))
	}
	if p.SetQuota {
		parts = append(parts, fmt.Sprintf("额度 从 %s 改为 %s",
			formatQuota(before.QuotaLimit), formatQuota(after.QuotaLimit)))
	}
	if p.SetExpiresAt {
		parts = append(parts, fmt.Sprintf("过期 从 %s 改为 %s",
			formatTime(before.ExpiresAt, "不过期"), formatTime(after.ExpiresAt, "不过期")))
	}
	_, _ = fmt.Fprintf(w, "已改 API Key id=%d：%s\n", after.ID, strings.Join(parts, "；"))
}

// printKeyDetail 打一把 key 的现状。
//
// 竖排的「字段 / 值」两列，与 printModelDetail 同一种形状：一把 key 的字段
// 比 list 那十栏更值得铺开（备注可能很长，权限说明也要位置），而横排的一行
// 在窄终端里会自己折行，折出来的行与下一个字段分不开。
//
// 三个时刻打完整的 RFC3339（不截到分钟）：list 那一栏回答的是「哪一把最近
// 用过」，这里是「这一把到底什么时候建的、什么时候到期」，秒是它要回答的
// 问题的一部分。
func printKeyDetail(w io.Writer, k auth.Key) {
	tw := newTableWriter(w)
	p := newPalette(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("ID", strconv.FormatInt(k.ID, 10))
	row("KEY 前缀", k.Prefix)
	row("所属用户", k.Username)
	row("备注", orDash(k.Name))
	row("状态", k.Status)
	row("每分钟", formatRPM(k.RateLimitRPM))
	row("额度", formatQuota(k.QuotaLimit))
	row("创建", formatTimeValue(k.CreatedAt))
	row("过期", formatTime(k.ExpiresAt, "不过期"))
	row("最近使用", formatTime(k.LastUsedAt, "从未"))
	_ = tw.Flush()
}

// apiKeyGetDoc 组装 apikey get 的机器可读输出。
//
// 条目形状**逐字段**借用 list 的 apiKeyEntry：同一份数据的两条读路径给出两套
// 字段名时，对读它的脚本来说那就是两件事。这里是 list 收成一条，不是另一种
// 详情。
func apiKeyGetDoc(k auth.Key) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["key"] = apiKeyEntry(k)
	return doc
}

// printKeyList 打印 key 清单。
//
// 一张固定列宽的表格代替原先那条 150 字符的键=值长行：字段一多，长行在窄
// 终端里会自己折行，而折出来的几行与下一把 key 的行分不开——清单最要紧的
// 「一行一把」就那么没了。
//
// 三处取舍：
//
//   - 用户那一栏只打用户名。原先还附一个 `(id: x)`，而 id 已经单独一列，
//     重复一遍会把这一栏撑宽一倍。
//   - 三个时刻截到分钟（见 formatStampShort）。这一栏回答的是「哪一把最近
//     用过」，秒与年份在绝大多数行里带来的信息不抵它占的宽度。
//   - 三个时刻（创建、过期、最近使用）连着排：按时间先后读下来正好是一把
//     key 的一生，所以创建时刻排在另外两个之前。
//
// NULL 打成「继承」而不是留空——留空与「这一栏不存在」长得一样，而这一列
// 恰恰是「没设过」的意思。
func printKeyList(w io.Writer, keys []auth.Key) {
	p := newPalette(w)
	if len(keys) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("API Key", "apikey create"))
		return
	}

	tw := newTableWriter(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("ID"), p.bold("KEY 前缀"), p.bold("所属用户"), p.bold("备注"), p.bold("状态"),
		p.bold("每分钟"), p.bold("额度"), p.bold("创建"), p.bold("过期"), p.bold("最近使用"))
	for _, k := range keys {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.bold(strconv.FormatInt(k.ID, 10)), p.bold(k.Prefix),
			k.Username, orDash(k.Name), k.Status,
			formatRPM(k.RateLimitRPM), formatQuota(k.QuotaLimit),
			p.dim(formatStampShort(k.CreatedAt)),
			p.dim(formatTimeShort(k.ExpiresAt, "不过期")),
			p.dim(formatTimeShort(k.LastUsedAt, "从未")))
	}
	_ = tw.Flush()
}

// apiKeyListDoc 组装 apikey list 的机器可读输出。
//
// 与 printKeyList 是同一份数据的另一条渲染路径（见 format.go 开头）：那一档
// 打表、把缺值铺成「-」「继承」「不过期」「从未」，这一档给 null。
//
// **这一份里没有、也不会有 key 的明文或哈希**：auth.Key 里根本没有这两样
// （见 limits.go），所以这里能给的只有 prefix——它本来就够在列表里认出是
// 哪一把钥匙，而认出是哪一把正是这一栏的用途。
func apiKeyListDoc(keys []auth.Key) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(keys)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, apiKeyEntry(k))
	}
	doc["keys"] = entries
	return doc
}

// apiKeyEntry 打清单里的一把 key。
//
// 两列额度给的是**原样**的列值：null 表示这一列没设过（继承用户默认），
// 0 表示「一个都不放行」。表格那一档把 null 打成「继承」，正是为了不让这两
// 件事看起来一样——JSON 里靠 null 保住同一件事，而「继承」那个词不能进这里
// （它是个字符串，按金额解析它的一方会拿到一个解析不了的值）。
func apiKeyEntry(k auth.Key) map[string]any {
	var rpm any
	if k.RateLimitRPM != nil {
		rpm = *k.RateLimitRPM
	}
	// 金额走 jsonAmount：money.Amount 的字段不导出，直接塞进来编出来是 `{}`。
	var quota any
	if k.QuotaLimit != nil {
		quota = jsonAmount(*k.QuotaLimit)
	}

	return map[string]any{
		"id":       k.ID,
		"prefix":   k.Prefix,
		"username": k.Username,
		// 空串是「创建时没给 --name」（列是 NOT NULL DEFAULT ''），
		// 与表格那一栏打 `-` 是同一件事。
		"name":           nullableString(k.Name),
		"status":         k.Status,
		"rate_limit_rpm": rpm,
		"quota_limit":    quota,
		// 三个时刻都走 format.go 的 jsonTime/jsonTimePtr，**不**走上面那几个
		// formatXxx：那几档服务的是终端，按进程时区打（`+08:00`）并截到分钟。
		// 库里那几列是 timestamptz，驱动交回来的是带会话时区的 time.Time，
		// 直接 Format 出来的是「同一行在两台机器上是两个字符串」。机器读的
		// 那一份必须与机器无关（见 docs/cli-output.md 的「值的约定」）。
		"created_at":   jsonTime(k.CreatedAt),
		"expires_at":   jsonTimePtr(k.ExpiresAt),
		"last_used_at": jsonTimePtr(k.LastUsedAt),
	}
}

func formatRPM(v *int) string {
	if v == nil {
		return "继承"
	}
	return strconv.Itoa(*v)
}

func formatQuota(v *money.Amount) string {
	if v == nil {
		return "继承"
	}
	return v.String()
}

func formatTime(v *time.Time, empty string) string {
	if v == nil {
		return empty
	}
	return formatTimeValue(*v)
}

// formatTimeValue 格式化一个必然有值的时刻。
//
// created_at 是 NOT NULL DEFAULT now()，没有「没设过」这一档，所以它不走
// formatTime 的占位符参数——那一栏给不出有意义的占位语。共用一个 Format 布局
// 是为了让三个时刻写法一致：各写一遍 time.RFC3339 的话，改其中一处的布局，
// 另一处会安静地留着旧样子。
//
// formatTimeValue 与 formatTime 服务的是**终端那一档**，按进程时区打
// （`+08:00`）。机器读的那一档不要用它们：库里交回来的时刻带会话时区，
// 同一行在两台机器上会变成两个字符串（见 apikeyEntry 里那几条与 format.go
// 的 jsonTime/jsonTimePtr）。
func formatTimeValue(t time.Time) string {
	return t.Format(time.RFC3339)
}

// stampLayout 是清单里的时刻布局：月-日 时:分。
//
// 截到分钟，且不带年份：清单要回答的是「哪一把最近用过」，而整条 RFC3339
// （2026-10-17T14:59:09+08:00）会把一栏撑到 25 列，秒与年份在几乎所有行里
// 都一样。要精确到秒或要跨年比对时看单个实体的回显（它仍然打 RFC3339）。
const stampLayout = "01-02 15:04"

// formatStampShort 把必然有值的时刻截到分钟。
func formatStampShort(t time.Time) string {
	return t.Format(stampLayout)
}

// formatTimeShort 与 formatTime 同义，只是把时刻截到分钟。
func formatTimeShort(v *time.Time, empty string) string {
	if v == nil {
		return empty
	}
	return formatStampShort(*v)
}
