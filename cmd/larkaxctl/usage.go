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
	"github.com/sumwai/larkax/internal/calendar"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/settings"
)

// usageHelp 是 usage 的两版帮助文本（见 helpText）。
var usageHelp = helpText{short: usageShortUsage, long: usageUsage}

// usageShortUsage 是 usage 的短版：`-h` / `--help` 与参数写错时打的那一份。
const usageShortUsage = `larkaxctl usage 读用量与计费：这段时间用了多少、各自结成了什么。

用法：
  larkaxctl usage events [--user <用户 ID 或用户名>] [--from <时刻>] [--to <时刻>]
        [--limit <条数>]
        列出用量事件：每次请求用了多少 token、按哪条口径算的、收了多少钱
  larkaxctl usage requests [--user <用户 ID 或用户名>] [--from <时刻>] [--to <时刻>]
        [--limit <条数>]
        列出计费请求：每次请求的钱包预留与额度包面值、状态与结算金额
  larkaxctl usage summary --user <用户> [--today | --this-month]
        [--from <时刻>] [--to <时刻>]
        汇总某个用户在一段时间里的用量与金额，按计费口径分档

看某一个用户的用量在 larkaxctl user usage（events / requests <用户>）。
三条都只读，不改任何一行、不写任何快照。

更细的边界：larkaxctl help usage。
`

// usageUsage 是 usage 的长版：`larkaxctl help usage` 与 `usage help`。
const usageUsage = `larkaxctl usage 读用量与计费：这段时间用了多少、各自结成了什么。

用法：
  larkaxctl usage events [--user <用户 ID 或用户名>] [--from <时刻>] [--to <时刻>]
        [--limit <条数>]
        列出用量事件：每次请求用了多少 token、按哪条口径算的、收了多少钱
  larkaxctl usage requests [--user <用户 ID 或用户名>] [--from <时刻>] [--to <时刻>]
        [--limit <条数>]
        列出计费请求：每次请求的钱包预留与额度包面值、状态与结算金额
  larkaxctl usage summary --user <用户> [--today | --this-month]
        [--from <时刻>] [--to <时刻>]
        汇总某个用户在一段时间里的用量与金额，按计费口径分档

按用户看这两张表已经搬到 larkaxctl user usage，核心宾语写成位置参数：

  larkaxctl user usage events   <用户> [--from <时刻>] [--to <时刻>] [--limit <条数>]
  larkaxctl user usage requests <用户> [--from <时刻>] [--to <时刻>] [--limit <条数>]

两条路径的输出逐字相同。旧写法（usage events --user …）仍可用，命中时在 stderr
提示新路径，至少保留一个发布周期。不带 --user 时这两条回答的是全量，没有对应的
新路径，也就不提示。

三条都只读：不改任何一行、不写任何快照。读用量与读计费记录是管理员动作——
跑得起 larkaxctl 的人本来就能连上这个库，所以这几条不额外要求别的授权，也不带
角色参数。

events 与 requests 各读一张表，回答的问题不同：events 读 usage_events，是**计量**的
底稿（用了多少、金额按哪条口径算出来的；billing_path 为 measured 的才是一次实测，
estimated 是按估算给的、fallback 是算不出来时按退路口径给的）；requests 读
billing_requests，是**每次请求**的预留与结算（一笔请求从预扣到结清的状态与金额）。
预留分两栏看：钱包预留是钱从账户余额冻住的那部分，包预留是额度包占着的面值——
额度包覆盖的请求在钱包那一栏是 0，两个数并列、不合并。

summary 是这段时间的合计，不是清单：一行合计，下面按计费口径分三档
（measured / estimated / fallback，没有数据的那一档金额与请求数是 0）。
金额与额度包面值并排两栏、不相加：前者是实收，后者是预留时占住的面值上界。
五组 token 与合计在同一行。

summary 的窗口三选一：--today、--this-month，或者 --from 与 --to 一起给。
相对窗口与绝对窗口一起给、--today 与 --this-month 一起给、只给 --from 或只给
--to，都按参数错处理（退出 2）。都不给时按 --today 算。--today 是站点时区下的
当日 00:00 到次日 00:00；--this-month 是当月 1 日 00:00 到现在。站点时区取自
settings 的 calendar.default_region。summary 的输出首行总是打出这次的实际窗口
（起止与站点时区），默认窗口不会隐形。--from / --to 的写法与下面 events、
requests 那一幅相同；要与控制台的 /usage/summary 对数，两边给同两个 RFC3339 时刻。

--user 收用户 ID 或用户名，两种写法都认。events 与 requests 不给它就是不限用户；
summary 必填，汇总只做用户维度。

events 与 requests 的时刻用 RFC3339（2026-10-17T00:00:00Z）或只写日期
（2026-10-17，按 UTC 零点算）。区间左闭右开：--from 含、--to 不含；这两个命令
两个都不给就是不限时间，那时每个月分区都会被扫到。events 按落库时刻圈定，而那一列
正是这张表的分区键，所以给了窗口时范围之外的那些月份不会被扫到；requests 按请求的
受理时刻圈定。

--limit 是一次最多打几行，默认 50，0 表示不限。两张清单都是新的在前；被截住时
末行会说明只打了多少条——一份少了后文的清单与一份完整的清单在屏幕上长得一样。

退出码见 larkaxctl --help。区间里一条都没有是正常结果，退出 0。

更细的约定见 docs/cli-reference.md 的「usage」一节。
`

// usageReadTimeout 是这几条只读命令（events、requests 与 summary）的时间上限。
//
// 与 ledger balance 同一个量级（一分钟）：它们按索引读一段窗口里的行，读不完
// 通常意味着谓词没走对索引、或者窗口给得太宽，两种都不该让命令无限挂下去。
const usageReadTimeout = time.Minute

// usageDefaultLimit 是清单默认打几行。
//
// 两张表都只追加，不给上限时「这个用户这个月花了多少」会一次把整月甚至整库的行
// 读回来，而看的人要的通常只是最近那一段。被截住时末行会说明，所以这个默认值
// 不会让人把一份截过的清单当成完整的。
const usageDefaultLimit = 50

// usageActions 是 usage 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var usageActions = []string{"events", "requests", "summary"}

// runUsage 处理 usage 子命令。
func runUsage(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, usageShortUsage)
		return usageErr(errors.New("usage 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "events":
		if usesUserFlag(rest) {
			return runLegacyUserPath("usage events --user 已搬到 larkaxctl user usage events <用户>",
				func() error { return runUsageEvents(rest) })
		}
		return runUsageEvents(rest)
	case "requests":
		if usesUserFlag(rest) {
			return runLegacyUserPath("usage requests --user 已搬到 larkaxctl user usage requests <用户>",
				func() error { return runUsageRequests(rest) })
		}
		return runUsageRequests(rest)
	case "summary":
		return runUsageSummary(rest)
	case "-h", "--help":
		printUsage(os.Stdout, usageShortUsage)
		return nil
	default:
		printUsage(os.Stderr, usageShortUsage)
		return usageErr(fmt.Errorf("未知的 usage 子命令 %q", action))
	}
}

// usageReadFlags 是这两条子命令共用的筛选项。
//
// 收成一处是因为它们**逐字相同**：同一个 --user 在两条命令上语义不同的话，拿
// 一个用户的两次输出对不上时会先怀疑数据，而问题在参数怎么解释。
type usageReadFlags struct {
	user  *string
	from  *string
	to    *string
	limit *int
}

// newUsageReadFlags 注册这组筛选项。
func newUsageReadFlags(fs *flagSet) usageReadFlags {
	f := usageReadFlags{
		user:  fs.String("user", "", ""),
		from:  fs.String("from", "", ""),
		to:    fs.String("to", "", ""),
		limit: fs.Int("limit", usageDefaultLimit, ""),
	}
	stringAlias(fs, f.user, "u")
	return f
}

// window 把 --from / --to / --limit 翻成查询要的形状。
//
// what 是命令名，用在报错里：同一个解析函数服务两条命令，报出来的那句话要能
// 指出是哪一条给了错的取值（与 parseLedgerTime 收 flagName 同一个做法）。
func (f usageReadFlags) window(what string) (from, to time.Time, limit int, err error) {
	from, err = parseLedgerTime("--from", *f.from)
	if err != nil {
		return time.Time{}, time.Time{}, 0, usageErr(err)
	}
	to, err = parseLedgerTime("--to", *f.to)
	if err != nil {
		return time.Time{}, time.Time{}, 0, usageErr(err)
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return time.Time{}, time.Time{}, 0, usageErr(fmt.Errorf(
			"%s 的 --from 必须早于 --to（区间左闭右开），得到 %s 与 %s",
			what, from.Format(time.RFC3339), to.Format(time.RFC3339)))
	}
	if *f.limit < 0 {
		return time.Time{}, time.Time{}, 0, usageErr(fmt.Errorf(
			"%s 的 --limit 不能为负，得到 %d（0 表示不限）", what, *f.limit))
	}
	return from, to, *f.limit, nil
}

// resolveUser 把 --user 解析成用户 ID，不给时返回 nil。
//
// 先解析成 ID 再查，两种写法（ID / 用户名）就停在同一处：与 apikey list、
// wallet list 同一条约定。
//
// 「没给 --user」与「查不到这个用户」是两回事：前者返回 nil、不报错——它是这一栏
// 的合法取值，不是「出错了但没报」，所以这里显式 nolint。
func (f usageReadFlags) resolveUser(ctx context.Context, authStore *auth.Store) (*int64, error) {
	if *f.user == "" {
		return nil, nil //nolint:nilnil // 见上
	}
	owner, err := authStore.ResolveUser(ctx, *f.user)
	if err != nil {
		return nil, err
	}
	return &owner.ID, nil
}

// runUsageEvents 列出用量事件。
func runUsageEvents(args []string) error {
	fs := newFlagSet("usage events", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, usageShortUsage) }
	flags := newUsageReadFlags(fs)
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("usage events 不接受位置参数，得到 %q：按用户筛用 --user", fs.Arg(0)))
	}

	from, to, limit, err := flags.window("usage events")
	if err != nil {
		return err
	}

	return withDB(usageReadTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		userID, err := flags.resolveUser(ctx, authStore)
		if err != nil {
			return err
		}

		page, err := store.New(db).UsageEvents(ctx, store.UsageEventFilter{
			UserID: userID, From: from, To: to, Limit: limit,
		})
		if err != nil {
			return err
		}

		names, err := userNames(ctx, authStore)
		if err != nil {
			return err
		}
		rows := make([]usageEventRow, 0, len(page.Events))
		for _, e := range page.Events {
			rows = append(rows, usageEventRow{
				CreatedAt:        e.CreatedAt,
				UserID:           e.UserID,
				Username:         names[e.UserID],
				CanonicalModel:   e.CanonicalModel,
				PromptTokens:     e.PromptTokens,
				CompletionTokens: e.CompletionTokens,
				CacheReadTokens:  e.CacheReadTokens,
				CacheWriteTokens: e.CacheWriteTokens,
				BillingPath:      e.BillingPath,
				Cost:             e.Cost,
				RequestID:        e.RequestID,
				ChannelID:        e.ChannelID.Int64,
				ReasoningTokens:  e.ReasoningTokens,
				Asset:            e.Asset,
				PricedAt:         e.PricedAt,
			})
		}

		filters := usageFilters{userID: userID, from: from, to: to, limit: limit}
		if jsonOutput() {
			return emitJSON(os.Stdout, usageEventsDoc(rows, page.More, filters), nil)
		}
		printUsageEvents(humanStdout(), rows, page.More)
		return nil
	})
}

// runUsageRequests 列出计费请求。
func runUsageRequests(args []string) error {
	fs := newFlagSet("usage requests", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, usageShortUsage) }
	flags := newUsageReadFlags(fs)
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("usage requests 不接受位置参数，得到 %q：按用户筛用 --user", fs.Arg(0)))
	}

	from, to, limit, err := flags.window("usage requests")
	if err != nil {
		return err
	}

	return withDB(usageReadTimeout, func(ctx context.Context, db *sql.DB) error {
		authStore := auth.New(db)
		userID, err := flags.resolveUser(ctx, authStore)
		if err != nil {
			return err
		}

		page, err := store.New(db).BillingRequests(ctx, store.BillingRequestFilter{
			UserID: userID, From: from, To: to, Limit: limit,
		})
		if err != nil {
			return err
		}

		names, err := userNames(ctx, authStore)
		if err != nil {
			return err
		}
		rows := make([]usageRequestRow, 0, len(page.Requests))
		for _, r := range page.Requests {
			rows = append(rows, usageRequestRow{
				StartedAt:      r.StartedAt,
				RequestID:      r.RequestID,
				UserID:         r.UserID,
				Username:       names[r.UserID],
				CanonicalModel: r.CanonicalModel,
				Status:         r.Status,
				ReservedAmount: r.ReservedAmount,
				// 两笔预留分开带过去：打印层把它们摆成并列的两栏。
				PlanReservedAmount: r.PlanReservedAmount,
				FinalAmount:        r.FinalAmount,
				PricingRuleID:      r.PricingRuleID,
				Asset:              r.Asset,
				UpstreamModel:      r.UpstreamModel,
				Protocol:           r.Protocol,
				Streamed:           r.Streamed,
				ErrorCode:          r.ErrorCode,
				SettledAt:          r.SettledAt,
			})
		}

		filters := usageFilters{userID: userID, from: from, to: to, limit: limit}
		if jsonOutput() {
			return emitJSON(os.Stdout, usageRequestsDoc(rows, page.More, filters), nil)
		}
		printUsageRequests(humanStdout(), rows, page.More)
		return nil
	})
}

// ── usage summary：一段时间的合计 ──────────────────────────────────────
//
// 与上面两条清单共用同一批表，但走的是 internal/ledger/store 的聚合读
// （UsageSummary）——金额求和的口径必须与账本、额度判定落在同一个包里，
// 在命令行这一侧另写一条 SUM 就会成为第二个口径。

// usageNow 取当前时刻，供相对窗口落成两个时刻。
//
// 是变量而不是直接调 time.Now()：用例注入一个固定时刻，才能把「跨零点」
// 「跨月」这两格钉住（与 style.go 的 terminalProbe 同一个做法）。
var usageNow = time.Now

// usageWindowMode 是这次窗口的来路：两档相对窗口，或者 --from/--to 给的绝对窗口。
type usageWindowMode string

const (
	usageWindowToday     usageWindowMode = "today"
	usageWindowThisMonth usageWindowMode = "this_month"
	usageWindowAbsolute  usageWindowMode = "absolute"
)

// usageSummaryFlags 是 usage summary 的筛选项。
//
// 与 usageReadFlags 分开：这里没有 --limit（汇总只有一行合计），多出两档相对窗口。
type usageSummaryFlags struct {
	user      *string
	today     *bool
	thisMonth *bool
	from      *string
	to        *string
}

// newUsageSummaryFlags 注册这组筛选项。
func newUsageSummaryFlags(fs *flagSet) usageSummaryFlags {
	f := usageSummaryFlags{
		user:      fs.String("user", "", ""),
		today:     fs.Bool("today", false, ""),
		thisMonth: fs.Bool("this-month", false, ""),
		from:      fs.String("from", "", ""),
		to:        fs.String("to", "", ""),
	}
	stringAlias(fs, f.user, "u")
	return f
}

// usageWindowChoice 是解析好的窗口选择。
//
// 相对窗口此刻只知道是哪一档：落到具体时刻要站点时区，而站点时区要连库读
// settings。绝对窗口在参数这一层就能验完（两个时刻都在，且起点早于终点）。
type usageWindowChoice struct {
	mode usageWindowMode
	from time.Time
	to   time.Time
}

// choice 把参数翻成窗口选择，参数错在这里全部报出来。
//
// 这一段**不连库**：参数错必须先于「连不上库」被报出来，否则一台连不上库的
// 机器上这几个错都看不出来（与 usage 另外两条的 --from/--to 同一个位置）。
func (f usageSummaryFlags) choice(what string) (usageWindowChoice, error) {
	if *f.today && *f.thisMonth {
		return usageWindowChoice{}, usageErr(fmt.Errorf(
			"%s 的 --today 与 --this-month 只能给一个", what))
	}

	relative := ""
	if *f.today {
		relative = "--today"
	}
	if *f.thisMonth {
		relative = "--this-month"
	}

	fromRaw, toRaw := strings.TrimSpace(*f.from), strings.TrimSpace(*f.to)
	if relative != "" && (fromRaw != "" || toRaw != "") {
		return usageWindowChoice{}, usageErr(fmt.Errorf(
			"%s 的相对窗口（--today / --this-month）与绝对窗口（--from / --to）只能给一种："+
				"相对窗口是快捷方式，对拍请用绝对窗口", what))
	}

	if fromRaw != "" || toRaw != "" {
		if fromRaw == "" {
			return usageWindowChoice{}, usageErr(fmt.Errorf(
				"%s 给了 --to 却缺 --from：两个要一起给", what))
		}
		if toRaw == "" {
			return usageWindowChoice{}, usageErr(fmt.Errorf(
				"%s 给了 --from 却缺 --to：两个要一起给", what))
		}
		from, err := parseLedgerTime("--from", fromRaw)
		if err != nil {
			return usageWindowChoice{}, usageErr(err)
		}
		to, err := parseLedgerTime("--to", toRaw)
		if err != nil {
			return usageWindowChoice{}, usageErr(err)
		}
		if !from.Before(to) {
			return usageWindowChoice{}, usageErr(fmt.Errorf(
				"%s 的 --from 必须早于 --to（区间左闭右开），得到 %s 与 %s",
				what, from.Format(time.RFC3339), to.Format(time.RFC3339)))
		}
		return usageWindowChoice{mode: usageWindowAbsolute, from: from, to: to}, nil
	}

	if relative == "--this-month" {
		return usageWindowChoice{mode: usageWindowThisMonth}, nil
	}
	return usageWindowChoice{mode: usageWindowToday}, nil
}

// resolveUsageWindow 把相对窗口落成两个时刻，按站点时区切分。
//
// --today 是站点时区下的当日 00:00 到次日 00:00（左闭右开）；--this-month 是
// 当月 1 日 00:00 到现在。now 由调用方给，所以「跨零点」「跨月」这两格能脱离
// 系统时钟测。
func resolveUsageWindow(mode usageWindowMode, now time.Time, loc *time.Location) (from, to time.Time) {
	local := now.In(loc)
	from = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	if mode == usageWindowThisMonth {
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc), local
	}
	return from, from.AddDate(0, 0, 1)
}

// usageSiteTimezone 读站点时区。
//
// 与装配层给控制台的 siteTimezone 同源：settings 的 calendar.default_region
// 经 calendar.Timezone 翻成 IANA 时区。这里不另立一个默认值——读不到就报错，
// 免得同一套设置在两个入口落成两个时区。
func usageSiteTimezone(ctx context.Context, db *sql.DB) (*time.Location, error) {
	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		return nil, err
	}
	region, err := values.String("calendar.default_region")
	if err != nil {
		return nil, err
	}
	loc, err := calendar.Timezone(calendar.Region(region))
	if err != nil {
		return nil, fmt.Errorf("站点时区（settings.calendar.default_region = %q）不可用: %w", region, err)
	}
	return loc, nil
}

// runUsageSummary 汇总一个用户在一段时间里的用量与金额。
func runUsageSummary(args []string) error {
	fs := newFlagSet("usage summary", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, usageShortUsage) }
	flags := newUsageSummaryFlags(fs)
	if err := fs.Parse(args); err != nil {
		// -h / --help 不是失败：flag 在返回 ErrHelp 之前已经把用法打过了。
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("usage summary 不接受位置参数，得到 %q：按用户汇总用 --user", fs.Arg(0)))
	}
	if strings.TrimSpace(*flags.user) == "" {
		return usageErr(errors.New("usage summary 需要 --user <用户 ID 或用户名>：汇总只做用户维度"))
	}

	choice, err := flags.choice("usage summary")
	if err != nil {
		return err
	}

	return withDB(usageReadTimeout, func(ctx context.Context, db *sql.DB) error {
		owner, err := auth.New(db).ResolveUser(ctx, *flags.user)
		if err != nil {
			return err
		}

		loc, err := usageSiteTimezone(ctx, db)
		if err != nil {
			return err
		}

		from, to := choice.from, choice.to
		if choice.mode != usageWindowAbsolute {
			from, to = resolveUsageWindow(choice.mode, usageNow(), loc)
		}
		if !from.Before(to) {
			// 只可能发生在「现在正好是本月起点」那一瞬间：窗口宽度为 0。
			// 换一句清楚的说明，而不是把 store 的「起点不早于终点」端给使用者。
			return usageErr(fmt.Errorf(
				"usage summary 的窗口是空的（%s 至 %s）：现在正好在窗口起点，稍后再跑或改用绝对窗口",
				from.In(loc).Format(time.RFC3339), to.In(loc).Format(time.RFC3339)))
		}

		summary, err := store.New(db).UsageSummary(ctx, store.UsageWindow{
			UserID: owner.ID, From: from, To: to,
		})
		if err != nil {
			return err
		}

		if jsonOutput() {
			return emitJSON(os.Stdout, usageSummaryDoc(owner.ID, choice.mode, from, to, loc, summary), nil)
		}
		printUsageSummary(humanStdout(), from, to, loc, summary)
		return nil
	})
}

// printUsageSummary 打这张汇总表。
//
// 首行是这次的实际窗口（起止与站点时区）：默认窗口（--today）不隐形，读的人
// 不用回看命令行就知道这些数属于哪一段。表里一行合计、三行计费口径；恒三档，
// 没有数据的那一档金额与请求数是 0（见 store 的 UsageSummary）。
func printUsageSummary(w io.Writer, from, to time.Time, loc *time.Location, summary store.UsageSummary) {
	_, _ = fmt.Fprintf(w, "窗口：%s 至 %s（站点时区 %s）\n",
		from.In(loc).Format(time.RFC3339), to.In(loc).Format(time.RFC3339), loc)

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("计费口径"), p.bold("请求数"), p.bold("金额"), p.bold("额度包面值"),
		p.bold("输入"), p.bold("输出"), p.bold("缓存读"), p.bold("缓存写"), p.bold("推理"))

	t := summary.Totals
	_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\t%d\t%d\t%d\t%d\n",
		p.bold("合计"), t.Requests, t.Cost.String(), t.PlanReservedAmount.String(),
		t.PromptTokens, t.CompletionTokens, t.CacheReadTokens, t.CacheWriteTokens,
		t.ReasoningTokens)

	// 计费口径那三行只有请求数与金额：store 的按口径聚合只给这两列，
	// 面值与 token 是合计上的数。没值的格子打 `-`，不打 0——0 会被读成
	// 「这一档的面值是 0」，与「这一档不按面值分」混成同一件事。
	for _, path := range summary.Paths {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			path.Path, path.Requests, path.Cost.String(),
			"-", "-", "-", "-", "-", "-")
	}
	_ = tw.Flush()
}

// usageSummaryDoc 组装 usage summary 的机器可读输出。
//
// 字段名与控制台 GET /usage/summary 一致（totals / billing_paths / tokens 的五个
// 槽位），两边同一个 store 函数、同一个形状，对拍时不必先翻译一层。
// cost 与 plan_reserved_amount 是两个独立字段，永不相加。
func usageSummaryDoc(userID int64, mode usageWindowMode, from, to time.Time,
	loc *time.Location, summary store.UsageSummary) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{
		"user_id":  userID,
		"window":   string(mode),
		"from":     jsonTime(from),
		"to":       jsonTime(to),
		"timezone": loc.String(),
	}

	t := summary.Totals
	doc["totals"] = map[string]any{
		"cost":                 jsonAmount(t.Cost),
		"plan_reserved_amount": jsonAmount(t.PlanReservedAmount),
		"requests":             t.Requests,
		"tokens": map[string]any{
			"input":       t.PromptTokens,
			"output":      t.CompletionTokens,
			"cache_read":  t.CacheReadTokens,
			"cache_write": t.CacheWriteTokens,
			"reasoning":   t.ReasoningTokens,
		},
	}

	paths := make([]map[string]any, 0, len(summary.Paths))
	for _, path := range summary.Paths {
		paths = append(paths, map[string]any{
			"path":     path.Path,
			"cost":     jsonAmount(path.Cost),
			"requests": path.Requests,
		})
	}
	doc["billing_paths"] = paths
	return doc
}

// usageEventRow 是 usage events 清单里的一行。
//
// 前十个字段进表格，后面几个只进 JSON：request_id 与 channel_id 能把这一行接回
// 别的命令与别的表，但它们有几十个字符，摆进表格会把每一行撑到折行（见
// docs/cli-output.md 的「加一个清单命令」）。
type usageEventRow struct {
	CreatedAt        time.Time
	UserID           int64
	Username         string
	CanonicalModel   string
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	BillingPath      string
	Cost             money.Amount

	RequestID       string
	ChannelID       int64
	ReasoningTokens int64
	Asset           string
	PricedAt        time.Time
}

// usageRequestRow 是 usage requests 清单里的一行。
//
// 前几个字段进表格，其余只进 JSON（与 usageEventRow 同一条排布）：upstream_model
// 是选路之后才知道的名字，protocol / streamed / error_code 只在排查某一条请求时才
// 用得上，摆进表格会把每一行撑到折行（见 docs/cli-output.md 的「加一个清单命令」）。
type usageRequestRow struct {
	StartedAt      time.Time
	RequestID      string
	UserID         int64
	Username       string
	CanonicalModel string
	// Status 是 pending / settled / released / failed / refunded 之一
	// （表上有 CHECK）。
	Status string
	// ReservedAmount 是钱包那一部分预留：额度包覆盖的请求在这一列是 0，
	// 钱一分没冻。表格里那一栏叫「钱包预留」。
	ReservedAmount money.Amount
	// PlanReservedAmount 是这笔请求占着的额度包面值。它与 ReservedAmount 并列，
	// 不合并成一个数：加在一起就答不了「钱包冻了多少」这个问题。表格里那一栏
	// 叫「包预留」，机器可读档里是 plan_reserved_amount。
	PlanReservedAmount money.Amount
	// FinalAmount 是库里那一列的十进制文本（numeric(20,8)::text），这一列可空：
	// 「还没结算」与「结算出来是 0」在零值上分不开，所以用 sql.NullString 带着
	// 「有没有值」一起读回来。文本的形状与 money.Amount 的 String() 一致
	// （定点十进制、小数保留到 8 位），两处打出来的是同一种写法。
	FinalAmount sql.NullString
	// PricingRuleID 是定这一单价的计价规则。可空：请求没有落到任何规则上时库里
	// 那一列就是 NULL（见 store.BillingRequestRow 的说明）。规则事后可变，而账单
	// 要能回答「当时按哪条规则算的」，所以它跟着这一行一起打出来。
	PricingRuleID sql.NullInt64

	Asset         string
	UpstreamModel sql.NullString
	Protocol      string
	Streamed      bool
	ErrorCode     sql.NullString
	SettledAt     sql.NullTime
}

// printUsageEvents 打一张用量事件清单。
//
// 空清单也说一句话：一行都不打时，「这个窗口里没有用量事件」与「命令没跑成」在
// 安静的终端里长得一样（见 LESSONS.md 第 16 条）。这一句不套 style.go 的
// emptyListNote——那个模板的后半截是「用哪条命令添加」，而用量事件由网关在结算
// 时写，没有「添加」这个动作。
func printUsageEvents(w io.Writer, rows []usageEventRow, more bool) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "这个范围内没有用量事件。")
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("时刻"), p.bold("用户 ID"), p.bold("用户名"), p.bold("模型"),
		p.bold("输入"), p.bold("输出"), p.bold("缓存读"), p.bold("缓存写"),
		p.bold("计费口径"), p.bold("金额"))
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			p.dim(r.CreatedAt.Format(longStampLayout)),
			p.bold(strconv.FormatInt(r.UserID, 10)), p.bold(r.Username),
			r.CanonicalModel,
			r.PromptTokens, r.CompletionTokens, r.CacheReadTokens, r.CacheWriteTokens,
			r.BillingPath, r.Cost.String())
	}
	_ = tw.Flush()
	printTruncatedListNote(w, more, len(rows))
}

// printUsageRequests 打一张计费请求清单。
//
// 预留摆成并列的两栏：钱包预留与包预留。额度包覆盖的请求在钱包那一栏是 0，
// 它占着的面值在包那一栏；把两个数加在一起，「钱包冻了多少」就答不出来了。
//
// 结算金额那一栏对没结算的行打 `-`：pending 的行结出来是 0，与「还没结算」在
// 数字上分不开，而这两件事的处置完全不同（那一栏进 JSON 是 null，见
// usageRequestsDoc）。末尾的规则那一栏同一条：没落到任何规则上时打 `-`，不打 0。
func printUsageRequests(w io.Writer, rows []usageRequestRow, more bool) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "这个范围内没有计费请求。")
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("时刻"), p.bold("request_id"), p.bold("用户 ID"), p.bold("用户名"),
		p.bold("模型"), p.bold("状态"), p.bold("钱包预留"), p.bold("包预留"),
		p.bold("结算"), p.bold("规则"))
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.dim(r.StartedAt.Format(longStampLayout)), p.bold(r.RequestID),
			p.bold(strconv.FormatInt(r.UserID, 10)), p.bold(r.Username),
			r.CanonicalModel, r.Status, r.ReservedAmount.String(),
			r.PlanReservedAmount.String(), r.settledCell(), r.pricingRuleCell())
	}
	_ = tw.Flush()
	printTruncatedListNote(w, more, len(rows))
}

// settledCell 是结算金额那一栏的表格写法，没结算时打 `-`。
func (r usageRequestRow) settledCell() string {
	if !r.FinalAmount.Valid {
		return "-"
	}
	return r.FinalAmount.String
}

// pricingRuleCell 是规则那一栏的表格写法，没落到任何规则上时打 `-`。
//
// 打 `-` 而不是 0：规则 id 从 1 开始，0 在读数的一方看起来仍像「一条编号为 0 的
// 规则」。这一栏进 JSON 是 null（见 usageRequestsDoc）——两处都由 PricingRuleID
// 这一个字段推出来，改了判别「有没有值」的那条线，两边会一起动。
func (r usageRequestRow) pricingRuleCell() string {
	if !r.PricingRuleID.Valid {
		return "-"
	}
	return strconv.FormatInt(r.PricingRuleID.Int64, 10)
}

// usageFilters 是这次查询用的那几个筛选条件，供机器可读输出复述。
type usageFilters struct {
	userID *int64
	from   time.Time
	to     time.Time
	limit  int
}

// summary 把筛选条件折进 summary。
//
// 一份只有 `{"total": 12}` 的清单说不清这 12 条是哪一段：两张表的行都随时间增长，
// 而看 JSON 的一方要能不看命令行就知道这次筛的是什么（与 channel health 把 since
// 放进 summary 是同一条）。
func (f usageFilters) summary() map[string]any {
	summary := map[string]any{
		"limit": f.limit,
		"from":  jsonTimeOrNull(f.from),
		"to":    jsonTimeOrNull(f.to),
	}
	if f.userID != nil {
		summary["user_id"] = *f.userID
	} else {
		summary["user_id"] = nil
	}
	return summary
}

// usageEventsDoc 组装 usage events 的机器可读输出。
//
// 金额走 jsonAmount（十进制定点字符串）：直接塞过一个 float64 会丢精度，而丢掉的
// 恰好是账单要精确到的那几位（见 format.go）。
func usageEventsDoc(rows []usageEventRow, more bool, filters usageFilters) map[string]any {
	summary := filters.summary()
	summary["total"] = len(rows)
	// 被截断的一轮与跑完的一轮，行内容可以逐字相同，差别只在这个布尔：没有它，
	// 「只打了 50 条」与「一共就这 50 条」在 JSON 里读起来一样（见 LESSONS.md
	// 第 16 条）。
	summary["truncated"] = more

	events := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		events = append(events, map[string]any{
			"created_at":         jsonTime(r.CreatedAt),
			"priced_at":          jsonTime(r.PricedAt),
			"request_id":         r.RequestID,
			"user_id":            r.UserID,
			"username":           nullableString(r.Username),
			"channel_id":         jsonID(r.ChannelID),
			"canonical_model":    r.CanonicalModel,
			"prompt_tokens":      r.PromptTokens,
			"completion_tokens":  r.CompletionTokens,
			"cache_read_tokens":  r.CacheReadTokens,
			"cache_write_tokens": r.CacheWriteTokens,
			"reasoning_tokens":   r.ReasoningTokens,
			"billing_path":       r.BillingPath,
			"cost":               jsonAmount(r.Cost),
			"asset":              r.Asset,
		})
	}

	doc := jsonDoc(jsonOK)
	doc["summary"] = summary
	doc["events"] = events
	return doc
}

// usageRequestsDoc 组装 usage requests 的机器可读输出。
//
// 结算金额与规则这两档没值时是 null，不打 `-`：占位符进 JSON 比留空更坏，按数字
// 解析它的一方会拿到一个解析不了的值（见 format.go 上面那几段）。字段的命名与库里
// 那几列对得上（reserved_amount、plan_reserved_amount、final_amount、pricing_rule_id）。
func usageRequestsDoc(rows []usageRequestRow, more bool, filters usageFilters) map[string]any {
	summary := filters.summary()
	summary["total"] = len(rows)
	summary["truncated"] = more

	requests := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		requests = append(requests, map[string]any{
			"started_at":           jsonTime(r.StartedAt),
			"settled_at":           jsonTimePtr(nullTimeToPtr(r.SettledAt)),
			"request_id":           r.RequestID,
			"user_id":              r.UserID,
			"username":             nullableString(r.Username),
			"canonical_model":      r.CanonicalModel,
			"upstream_model":       nullableString(r.UpstreamModel.String),
			"protocol":             r.Protocol,
			"streamed":             r.Streamed,
			"status":               r.Status,
			"reserved_amount":      jsonAmount(r.ReservedAmount),
			"plan_reserved_amount": jsonAmount(r.PlanReservedAmount),
			"final_amount":         nullableString(r.FinalAmount.String),
			"pricing_rule_id":      jsonNullInt64(r.PricingRuleID),
			"asset":                r.Asset,
			"error_code":           nullableString(r.ErrorCode.String),
		})
	}

	doc := jsonDoc(jsonOK)
	doc["summary"] = summary
	doc["requests"] = requests
	return doc
}

// printTruncatedListNote 在清单被 --limit 截住时补一句。
func printTruncatedListNote(w io.Writer, more bool, shown int) {
	if !more {
		return
	}
	_, _ = fmt.Fprintln(w, truncatedListNote(shown))
}
