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
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
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
        列出计费请求：每次请求的预留、状态与结算金额

两条都只读，不改任何一行、不写任何快照。

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
        列出计费请求：每次请求的预留、状态与结算金额

两条都只读：不改任何一行、不写任何快照。读用量与读计费记录是管理员动作——
跑得起 larkaxctl 的人本来就能连上这个库，所以这两条不额外要求别的授权，也不带
角色参数。

两条各读一张表，回答的问题不同：events 读 usage_events，是**计量**的底稿（用了
多少、金额按哪条口径算出来的；billing_path 为 measured 的才是一次实测，estimated
是按估算给的、fallback 是算不出来时按退路口径给的）；requests 读 billing_requests，
是**每次请求**的预留与结算（一笔请求从预扣到结清的状态与金额）。

--user 收用户 ID 或用户名，两种写法都认。不给它就是不限用户。

时刻用 RFC3339（2026-10-17T00:00:00Z）或只写日期（2026-10-17，按 UTC 零点算）。
区间左闭右开：--from 含、--to 不含；两个都不给就是不限时间，那时每个月分区都会被
扫到。events 按落库时刻圈定，而那一列正是这张表的分区键，所以给了窗口时范围之外的
那些月份不会被扫到；requests 按请求的受理时刻圈定。

--limit 是一次最多打几行，默认 50，0 表示不限。两张清单都是新的在前；被截住时
末行会说明只打了多少条——一份少了后文的清单与一份完整的清单在屏幕上长得一样。

退出码见 larkaxctl --help。区间里一条都没有是正常结果，退出 0。

更细的约定见 docs/cli-reference.md 的「usage」一节。
`

// usageReadTimeout 是这两条只读命令的时间上限。
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

// runUsage 处理 usage 子命令。
func runUsage(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, usageShortUsage)
		return usageErr(errors.New("usage 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "events":
		return runUsageEvents(rest)
	case "requests":
		return runUsageRequests(rest)
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
func newUsageReadFlags(fs *flag.FlagSet) usageReadFlags {
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
func (f usageReadFlags) resolveUser(ctx context.Context, authStore *auth.Store) (*int64, error) {
	if *f.user == "" {
		return nil, nil
	}
	owner, err := authStore.ResolveUser(ctx, *f.user)
	if err != nil {
		return nil, err
	}
	return &owner.ID, nil
}

// runUsageEvents 列出用量事件。
func runUsageEvents(args []string) error {
	fs := flag.NewFlagSet("usage events", flag.ContinueOnError)
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
	fs := flag.NewFlagSet("usage requests", flag.ContinueOnError)
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
				FinalAmount:    r.FinalAmount,
				Asset:          r.Asset,
				UpstreamModel:  r.UpstreamModel,
				Protocol:       r.Protocol,
				Streamed:       r.Streamed,
				ErrorCode:      r.ErrorCode,
				SettledAt:      r.SettledAt,
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
type usageRequestRow struct {
	StartedAt      time.Time
	RequestID      string
	UserID         int64
	Username       string
	CanonicalModel string
	// Status 是 pending / settled / released / failed / refunded 之一
	// （表上有 CHECK）。
	Status         string
	ReservedAmount money.Amount
	// FinalAmount 是库里那一列的十进制文本（numeric(20,8)::text），这一列可空：
	// 「还没结算」与「结算出来是 0」在零值上分不开，所以用 sql.NullString 带着
	// 「有没有值」一起读回来。文本的形状与 money.Amount 的 String() 一致
	// （定点十进制、小数保留到 8 位），两处打出来的是同一种写法。
	FinalAmount sql.NullString

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
// 结算金额那一栏对没结算的行打 `-`：pending 的行结出来是 0，与「还没结算」在
// 数字上分不开，而这两件事的处置完全不同（那一栏进 JSON 是 null，见
// usageRequestsDoc）。
func printUsageRequests(w io.Writer, rows []usageRequestRow, more bool) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "这个范围内没有计费请求。")
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("时刻"), p.bold("request_id"), p.bold("用户 ID"), p.bold("用户名"),
		p.bold("模型"), p.bold("状态"), p.bold("预留"), p.bold("结算"))
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.dim(r.StartedAt.Format(longStampLayout)), p.bold(r.RequestID),
			p.bold(strconv.FormatInt(r.UserID, 10)), p.bold(r.Username),
			r.CanonicalModel, r.Status, r.ReservedAmount.String(), r.settledCell())
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
// 结算金额这一档没结算就是 null，不打 `-`：占位符进 JSON 比留空更坏，按数字解析
// 它的一方会拿到一个解析不了的值（见 format.go 上面那几段）。
func usageRequestsDoc(rows []usageRequestRow, more bool, filters usageFilters) map[string]any {
	summary := filters.summary()
	summary["total"] = len(rows)
	summary["truncated"] = more

	requests := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		requests = append(requests, map[string]any{
			"started_at":      jsonTime(r.StartedAt),
			"settled_at":      jsonTimePtr(nullTimeToPtr(r.SettledAt)),
			"request_id":      r.RequestID,
			"user_id":         r.UserID,
			"username":        nullableString(r.Username),
			"canonical_model": r.CanonicalModel,
			"upstream_model":  nullableString(r.UpstreamModel.String),
			"protocol":        r.Protocol,
			"streamed":        r.Streamed,
			"status":          r.Status,
			"reserved_amount": jsonAmount(r.ReservedAmount),
			"final_amount":    nullableString(r.FinalAmount.String),
			"asset":           r.Asset,
			"error_code":      nullableString(r.ErrorCode.String),
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
