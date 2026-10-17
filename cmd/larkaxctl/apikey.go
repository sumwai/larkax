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

const apiKeyUsage = `larkaxctl apikey 管理 API Key。

用法：
  larkaxctl apikey create --user <ID 或用户名> [--name <备注>] [--expires-at <RFC3339>]
                          [--rate-limit-rpm <整数>] [--quota-limit <金额>]
  larkaxctl apikey update --id <ID> [--rate-limit-rpm <整数> | --clear-rate-limit-rpm]
                                  [--quota-limit <金额> | --clear-quota-limit]
  larkaxctl apikey list [--user <ID 或用户名>]

create 生成一把新 key，只把哈希入库。

**明文只打印这一次**：库里存的是 SHA-256 哈希，它不可逆，
丢了没有任何办法取回，只能重建一把。

明文单独打到 stdout，其余信息打到 stderr，所以

  larkaxctl apikey create --user alice > key.txt

保存下来的文件里只有明文，不会混进前缀、id 与提示语。

# 额度

两列额度都按**明文**给，不填表示 NULL：

  --rate-limit-rpm   每分钟请求额度。**0 不是「不限」，是「一个都不放行」。**
                     不填（或 update 时给 --clear-rate-limit-rpm）写回 NULL，
                     即「继承用户默认」——users 表上目前没有这样一个默认值，
                     所以它等价于不限。
  --quota-limit      这把 key 累计可花费的金额上限，单位是**该用户钱包的
                     资产品种**（见 user_wallets.asset）。最多 8 位小数。
                     0 与不填的区别同上。

负数一律拒绝：限流那一侧把负数也当成「一个都不放行」，而写下 -60 的人
几乎总是想写 60。让他当场改掉，比让它变成一个静默的封禁好。

update 只改额度，不改备注与过期时间——那两样目前没有入口，只能直接改库。
这个限制写在帮助里，而不是让它安静地什么都不做。

# 权限

这三条命令都不需要 LARKAX_MASTER_KEYS：它们不碰渠道凭证，也就不碰
信封加密。主密钥是 rotate 与 channels 那一组的事。
`

// apiKeyTimeout 是这条命令连库与写库的时间上限。
//
// 命令行要有上限，否则一次卡住的连接会挂到人发现为止。
// 30 秒对一条单行插入来说宽得离谱，取这个值只是为了让「卡住了」
// 有个终点，而不是把等待当成正常。
const apiKeyTimeout = 30 * time.Second

func runAPIKey(args []string) error {
	fs := flag.NewFlagSet("apikey", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, apiKeyUsage) }
	if err := fs.Parse(args); err != nil {
		return err
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
	case "list":
		return runAPIKeyList(rest)
	default:
		return usageErr(fmt.Errorf("未知的 apikey 子命令 %q", action))
	}
}

func runAPIKeyCreate(args []string) error {
	fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, apiKeyUsage) }
	user := fs.String("user", "", "key 的归属用户：用户 ID 或用户名")
	name := fs.String("name", "", "备注名，用于日后在列表里认出这把 key")
	expiresAt := fs.String("expires-at", "", "过期时间（RFC3339），留空表示不过期")
	rpm := fs.String("rate-limit-rpm", "", "每分钟请求额度；0 表示一个都不放行，不填表示 NULL")
	quota := fs.String("quota-limit", "", "累计可花费的金额上限；0 表示一个都不放行，不填表示 NULL")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("create 不接受位置参数，得到 %q：用户用 --user 指定", fs.Arg(0)))
	}
	if *user == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --user"))
	}

	var expires *time.Time
	if *expiresAt != "" {
		t, err := time.Parse(time.RFC3339, *expiresAt)
		if err != nil {
			return usageErr(fmt.Errorf("--expires-at 不是 RFC3339 时间（例如 2026-12-31T00:00:00Z）: %w", err))
		}
		// 已过去的时刻直接拒绝，而不是建一把立刻不可用的 key。
		// 那种情况几乎只可能是写错了年份（2025 写成 2024），
		// 而它的表现是「刚建好的 key 就说无效」——最费时的一类排查。
		// auth.Create 本身不拦：库层允许建一把已过期的 key（造测试数据、
		// 补历史数据都要用到），拦在交互式命令这一层才合适。
		if !t.After(time.Now()) {
			return usageErr(fmt.Errorf("--expires-at %s 已经过去了：这样的 key 建出来就是不可用的",
				t.Format(time.RFC3339)))
		}
		expires = &t
	}

	limits, err := parseLimits(fs, *rpm, *quota)
	if err != nil {
		return err
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

		printCreatedKey(os.Stdout, os.Stderr, owner, created, expires, limits)
		return nil
	})
}

func runAPIKeyUpdate(args []string) error {
	fs := flag.NewFlagSet("apikey update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, apiKeyUsage) }
	id := fs.Int64("id", 0, "要改的 API Key 的 id（apikey list 里能看到）")
	rpm := fs.String("rate-limit-rpm", "", "每分钟请求额度；0 表示一个都不放行")
	clearRPM := fs.Bool("clear-rate-limit-rpm", false, "把每分钟额度清空（写回 NULL）")
	quota := fs.String("quota-limit", "", "累计可花费的金额上限；0 表示一个都不放行")
	clearQuota := fs.Bool("clear-quota-limit", false, "把金额上限清空（写回 NULL）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("update 不接受位置参数，得到 %q：要改哪一把 key 用 --id 指定", fs.Arg(0))
	}

	seen := providedFlags(fs)
	if *id <= 0 {
		fs.Usage()
		return errors.New("缺少 --id：要改哪一把 key")
	}

	params, err := updateLimits(seen,
		limitFlag{value: *rpm, clear: *clearRPM},
		limitFlag{value: *quota, clear: *clearQuota})
	if err != nil {
		// 不在这里打一遍 usage：这几条错误（负数、小数位过多、两个旗标
		// 打架）都在信息里点了名，而「缺 --id」那条已经打过了。
		// 与 create / user 一样，usage 只在缺必填项时打。
		return err
	}

	return withDB(apiKeyTimeout, func(ctx context.Context, db *sql.DB) error {
		if err := auth.New(db).UpdateLimits(ctx, *id, params); err != nil {
			return err
		}
		printUpdatedLimits(os.Stderr, *id, params)
		return nil
	})
}

func runAPIKeyList(args []string) error {
	fs := flag.NewFlagSet("apikey list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, apiKeyUsage) }
	user := fs.String("user", "", "只看这个用户的 key：用户 ID 或用户名；留空表示全部")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("list 不接受位置参数，得到 %q：按用户过滤用 --user", fs.Arg(0))
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
		printKeyList(os.Stdout, keys)
		return nil
	})
}

// limitFlag 是某一项额度在命令行上给的那一对旗标：一个设值、一个清空。
//
// 收成一个结构体而不是四个平铺的参数：updateLimits 要收两组这样的东西，
// 平铺开就是 rpm 字符串、rpm 布尔、quota 字符串、quota 布尔四个相邻的
// 位置参数，而其中两个是字符串、两个是布尔——调用点写反了顺序照样能编过。
// 这不是假设：写下这段注释的时候，用例里就刚写反过一次。
type limitFlag struct {
	value string
	clear bool
}

// updateLimits 把 update 的四个旗标读成一次修改意图。
//
// seen 是「命令行上真的出现过哪些旗标」（见 providedFlags）。每对旗标有三种
// 状态——不改 / 设值 / 写回 NULL——而判据要**同时**看 seen 与 --clear-* 的值：
//
//   - 只看值：「--rate-limit-rpm 0」与「没给这个旗标」的字符串都是 "0" 或 ""，
//     分不开，而那两件事正相反（一个都不放行 / 不改）
//   - 只看 seen：`--clear-rate-limit-rpm=false` 会被当成「要清空」，
//     而它其实是「不要清空」，与不给它是同一件事
//
// 抽成一个不碰数据库的函数，是因为这三态正是最容易写错、也最该被单测钉住的
// 地方：把「清空」实现成「写成 0」，表现是每一次「恢复默认」都变成一次静默的
// 封禁，而它不报任何错。
func updateLimits(seen map[string]bool, rpm, quota limitFlag) (auth.LimitParams, error) {
	var params auth.LimitParams

	switch rpmGiven := seen["rate-limit-rpm"]; {
	case rpm.clear && rpmGiven:
		return auth.LimitParams{}, errors.New("--rate-limit-rpm 与 --clear-rate-limit-rpm 不能同时给：" +
			"一个是设值，一个是写回 NULL")
	case rpm.clear:
		// RateLimitRPM 留 nil：SetRateLimit 为真而指针为 nil 就是「写回 NULL」。
		params.SetRateLimit = true
	case rpmGiven:
		parsed, err := parseRateLimitRPM(rpm.value)
		if err != nil {
			return auth.LimitParams{}, err
		}
		params.SetRateLimit, params.RateLimitRPM = true, parsed
	}

	switch quotaGiven := seen["quota-limit"]; {
	case quota.clear && quotaGiven:
		return auth.LimitParams{}, errors.New("--quota-limit 与 --clear-quota-limit 不能同时给：" +
			"一个是设值，一个是写回 NULL")
	case quota.clear:
		params.SetQuota = true
	case quotaGiven:
		parsed, err := parseQuotaLimit(quota.value)
		if err != nil {
			return auth.LimitParams{}, err
		}
		params.SetQuota, params.QuotaLimit = true, parsed
	}

	// 一条什么都没要求的 update 是「看起来做了、其实没做」的命令，而它的
	// 表现与成功一模一样。零值在这里的意思是「不改」，所以这个检查不能省。
	if !params.SetRateLimit && !params.SetQuota {
		return auth.LimitParams{}, errors.New("没有要改的东西：--rate-limit-rpm、" +
			"--clear-rate-limit-rpm、--quota-limit、--clear-quota-limit 至少要给一个")
	}

	return params, nil
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

	if seen["rate-limit-rpm"] {
		parsed, err := parseRateLimitRPM(rpm)
		if err != nil {
			return keyLimits{}, err
		}
		out.rpm = parsed
	}

	if seen["quota-limit"] {
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
		return nil, errors.New("--rate-limit-rpm 需要一个整数（0 表示一个都不放行），得到空值")
	}

	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return nil, fmt.Errorf("--rate-limit-rpm 需要整数，得到 %q", plain)
	}
	// 列是 integer（32 位）。超出上界时数据库会报一句 numeric out of range，
	// 那句话没说清是哪个旗标，而这条命令上只有这一个整数旗标之外的东西……
	// 所以在这里拦下来，把话说在旗标的名字上。
	if n > math.MaxInt32 {
		return nil, fmt.Errorf("--rate-limit-rpm 最大 %d，得到 %d：列是 32 位整数",
			int64(math.MaxInt32), n)
	}
	if n < 0 {
		return nil, fmt.Errorf("--rate-limit-rpm 不能是负数，得到 %d："+
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
		return nil, errors.New("--quota-limit 需要一个金额（0 表示一个都不放行），得到空值")
	}

	// 先自己数一遍小数位。money.Parse 会把多出来的位四舍五入，而额度是一个
	// 会被拿去比较的边界值——「存进去的数与敲进去的数不一样」在这里是不该
	// 发生的事，哪怕差的只是最后一位。
	if _, frac, hasDot := strings.Cut(trimmed, "."); hasDot && len(frac) > quotaScale {
		return nil, fmt.Errorf("--quota-limit 最多 %d 位小数，得到 %q："+
			"列是 numeric(20, %d)，多出来的位存不进去", quotaScale, plain, quotaScale)
	}

	amount, err := money.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("--quota-limit 不是金额（例如 12.34）: %w", err)
	}
	if amount.IsNegative() {
		return nil, fmt.Errorf("--quota-limit 不能是负数，得到 %s：金额上限没有负数这一档",
			amount)
	}
	return &amount, nil
}

// printCreatedKey 打印新 key。
//
// 明文与其余信息分走两个流：`> key.txt` 是保存一把 key 最自然的动作，
// 而元信息混进去会让那个文件没法被程序直接读走（粘贴到客户端配置里时
// 多一行就少一个能用的 key）。
func printCreatedKey(stdout, stderr io.Writer, owner auth.User, c auth.Created,
	expires *time.Time, limits keyLimits,
) {
	_, _ = fmt.Fprintf(stderr, "已创建 API Key id=%d prefix=%s 用户=%s(id=%d) 过期=%s 每分钟=%s 额度=%s\n",
		c.ID, c.Prefix, owner.Username, owner.ID,
		formatTime(expires, "不过期"), formatRPM(limits.rpm), formatQuota(limits.quota))
	_, _ = fmt.Fprintln(stderr, "明文只打印这一次，请立即保存：库里只有哈希，无法再取回。")
	_, _ = fmt.Fprintln(stdout, c.Plaintext)
}

// printUpdatedLimits 回显这次改了哪两项。
//
// 回显是必要的：update 的参数里「不改」与「清空」在命令行长得很像
// （一个是不给旗标，一个是给 --clear-*），把结果打出来才能让人当场
// 确认自己做的正是想做的那一件事。
func printUpdatedLimits(w io.Writer, id int64, p auth.LimitParams) {
	var parts []string
	if p.SetRateLimit {
		parts = append(parts, "每分钟="+formatRPM(p.RateLimitRPM))
	}
	if p.SetQuota {
		parts = append(parts, "额度="+formatQuota(p.QuotaLimit))
	}
	_, _ = fmt.Fprintf(w, "已改 API Key id=%d %s\n", id, strings.Join(parts, " "))
}

// printKeyList 打印 key 清单。
//
// 每把一行、键=值的形式，与 printCreatedKey 同一套：可 grep，也能直接
// 看出哪一栏是空的。NULL 打成「继承」而不是留空——留空与「这一栏不存在」
// 长得一样，而这一列恰恰是「没设过」的意思。
//
// 三个时刻（创建、过期、最近使用）连着排：按时间先后读下来正好是一把 key 的
// 一生，所以创建时刻排在另外两个之前。
func printKeyList(w io.Writer, keys []auth.Key) {
	if len(keys) == 0 {
		_, _ = fmt.Fprintln(w, "没有 API Key。")
		return
	}

	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "id=%d prefix=%s 用户=%s(id=%d) 备注=%s 状态=%s 每分钟=%s 额度=%s 创建=%s 过期=%s 最近使用=%s\n",
			k.ID, k.Prefix, k.Username, k.UserID, orDash(k.Name), k.Status,
			formatRPM(k.RateLimitRPM), formatQuota(k.QuotaLimit),
			formatTimeValue(k.CreatedAt),
			formatTime(k.ExpiresAt, "不过期"), formatTime(k.LastUsedAt, "从未"))
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
func formatTimeValue(t time.Time) string {
	return t.Format(time.RFC3339)
}
