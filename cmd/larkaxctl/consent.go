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
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/snapshotwire"
)

// consentHelp 是 consent 的两版帮助文本（见 helpText）。
var consentHelp = helpText{short: consentShortUsage, long: consentUsage}

// consentShortUsage 是 consent 的短版：`-h` / `--help` 与参数写错时打的那一份。
const consentShortUsage = `larkaxctl consent 管理「原样采集请求正文」的授权。

用法：
  larkaxctl consent grant --user <用户 ID 或用户名> --scope single_request
        --request-id <请求 ID> --expires-at <RFC3339 时刻> --agree
  larkaxctl consent grant --user <用户 ID 或用户名> --scope time_window
        --from <RFC3339 时刻> --to <RFC3339 时刻> --agree
  larkaxctl consent list [--user <用户 ID 或用户名>]
  larkaxctl consent revoke <授权 ID>

grant 先把同意文案原样打出来，再传 --agree 才落库。
时长上限取自 settings 的 capture.consent_max_window_hours，超了直接拒绝。

更细的边界：larkaxctl help consent。
`

// consentUsage 是 consent 的长版：`larkaxctl help consent` 与 `consent help`。
const consentUsage = `larkaxctl consent 管理「原样采集请求正文」的授权。

用法：
  larkaxctl consent grant --user <用户 ID 或用户名> --scope single_request
        --request-id <请求 ID> --expires-at <RFC3339 时刻> --agree
  larkaxctl consent grant --user <用户 ID 或用户名> --scope time_window
        --from <RFC3339 时刻> --to <RFC3339 时刻> --agree
  larkaxctl consent list [--user <用户 ID 或用户名>]
  larkaxctl consent revoke <授权 ID>

两个范围二选一，字段不能混着给：

  single_request  只对 --request-id 那一次请求生效，到 --expires-at 为止
  time_window     对 --from 到 --to 之间的全部请求生效，到点即失效

两种范围都要过 --agree。不带它时命令只把同意文案打出来，什么都不写：
同意文案与它的版本号要一起落库（capture_consents 的 consent_text 与
consent_version），所以落库之前你得先看见自己同意的是什么。

时长上限是 settings 的 capture.consent_max_window_hours：single_request 比的是
从此刻到 --expires-at，time_window 比的是 --from 到 --to 的长度。超过上限报错
拒绝，不夹到上限——夹一下会让申请的那段时长看起来生效了，而实际生效的更短。

授权到期、被撤销、时间窗没开始或已经过去，这四种情况在**读取的那一刻**重新
校验一次，所以撤销立即生效，不需要等任何后台任务。没有有效授权时，采集侧落
下来的是脱敏骨架而不是原文。

list 不带 --user 时列出全部用户的授权，含已经撤销与过期的那些。读原文那条路
（snapshot get）另算：它自己会写访问日志。

# 别名

下面这个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -u <用户 ID 或用户名>     同 --user，grant 与 list 都认

revoke 的位置参数是授权 ID，它没有简写：把它与 --user 写成同一个字母会让
「撤销哪一条」与「谁的授权」在命令行上分不出来。

更细的约定见 docs/cli-reference.md 的「consent」一节。
`

// consentTimeout 是这几条命令连库的时间上限。
//
// 与 settingsTimeout 同一条理由：命令行要有上限，否则一次卡住的连接会挂到人
// 发现为止。各写各的常量，是因为它们将来可能各调各的。
const consentTimeout = 30 * time.Second

// consentMaxWindowKey 是申请时长上限所在的配置键。
//
// 键名写在这里而不是从别处取：它是**这条命令读的那一个键**，而
// `larkaxctl settings list` 上那一行「未接线」的说法跟着它一起消失。
const consentMaxWindowKey = "capture.consent_max_window_hours"

func runConsent(args []string) error {
	fs := flag.NewFlagSet("consent", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, consentShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageErr(errors.New("consent 需要子命令"))
	}
	action, rest := rest[0], rest[1:]

	switch action {
	case "grant":
		return runConsentGrant(rest)
	case "list":
		return runConsentList(rest)
	case "revoke":
		return runConsentRevoke(rest)
	case "-h", "--help":
		printUsage(os.Stdout, consentShortUsage)
		return nil
	default:
		printUsage(os.Stderr, consentShortUsage)
		return usageErr(fmt.Errorf("未知的 consent 子命令 %q", action))
	}
}

func runConsentGrant(args []string) error {
	// 与别的写类子命令同一条：json 模式下必须在写库之前拦下来，否则调用方
	// 只看到一句参数错，而授权已经落了库（见 format.go 的 jsonUnsupported）。
	if err := jsonUnsupported("consent grant"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("consent grant", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, consentShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	scope := fs.String("scope", "", "")
	requestID := fs.String("request-id", "", "")
	expiresAt := fs.String("expires-at", "", "")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	agree := fs.Bool("agree", false, "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("grant 不接受位置参数，得到 %q：授权给谁用 --user 指定", fs.Arg(0)))
	}
	if *user == "" {
		return usageErr(errors.New("缺少 --user：这条授权给哪一个用户"))
	}
	if *scope == "" {
		return usageErr(fmt.Errorf("缺少 --scope：%s 或 %s",
			snapshotwire.ConsentScopeSingleRequest, snapshotwire.ConsentScopeTimeWindow))
	}

	req := snapshotwire.ConsentRequest{Scope: *scope, RequestID: *requestID}
	switch *scope {
	case snapshotwire.ConsentScopeSingleRequest:
		if *from != "" || *to != "" {
			return usageErr(errors.New("--scope single_request 不接受 --from / --to：按单条请求授权只用到 --request-id 与 --expires-at"))
		}
		// request_id 会进 grant 的回执与 consent list 的 RANGE 那一栏：控制
		// 字符在连库之前拒掉（判据也在 ConsentStore.Grant 的 validate 里）。
		if err := checkDisplayText("--request-id", *requestID); err != nil {
			return err
		}
		if *expiresAt == "" {
			return usageErr(errors.New("缺少 --expires-at：这条授权到什么时候为止"))
		}
		at, err := parseConsentTime("--expires-at", *expiresAt)
		if err != nil {
			return err
		}
		req.ExpiresAt = at
	case snapshotwire.ConsentScopeTimeWindow:
		if *requestID != "" || *expiresAt != "" {
			return usageErr(errors.New("--scope time_window 不接受 --request-id / --expires-at：时间窗的到期时间就是 --to"))
		}
		if *from == "" || *to == "" {
			return usageErr(errors.New("缺少 --from 或 --to：时间窗的两端都要给"))
		}
		start, err := parseConsentTime("--from", *from)
		if err != nil {
			return err
		}
		end, err := parseConsentTime("--to", *to)
		if err != nil {
			return err
		}
		req.WindowStart, req.WindowEnd = start, end
	default:
		return usageErr(fmt.Errorf("--scope 只能是 %s 或 %s，得到 %q",
			snapshotwire.ConsentScopeSingleRequest, snapshotwire.ConsentScopeTimeWindow, *scope))
	}

	// 文案先打，且打在**没同意**的那条路上也打：这句话是操作者要同意的东西，
	// 不带 --agree 时它正是这次运行的全部产出。
	//
	// 它写 os.Stderr 而不是 humanStderr()：`-q` 压的是「给人看」那一档的输出，
	// 而这段文案是落库的前提——压掉它就成了「没看见也同意过」（同 printUsage
	// 收 os.Stdout 的那条理由）。
	_, _ = fmt.Fprint(os.Stderr, consentDisclosure())

	if !*agree {
		return usageErr(errors.New(
			"未确认：读完上面那段同意文案，同意之后加 --agree 再执行一次"))
	}

	return withConsentStore(func(ctx context.Context, db *sql.DB, store *snapshotwire.ConsentStore) error {
		owner, err := auth.New(db).ResolveUser(ctx, *user)
		if err != nil {
			return err
		}

		vals, err := settings.Load(ctx, settings.NewPgSource(db))
		if err != nil {
			return err
		}
		maxWindow, err := consentMaxWindow(vals)
		if err != nil {
			return err
		}

		req.UserID = owner.ID
		id, err := store.Grant(ctx, req, maxWindow)
		if err != nil {
			// 申请不合法是参数错（改命令重发），写库失败不是。时长上限只有
			// 读出 settings 才知道，所以这一类校验发生在连库之后，只能按
			// 类型认它（见 ConsentInvalidError）。
			var bad *snapshotwire.ConsentInvalidError
			if errors.As(err, &bad) {
				return usageErr(err)
			}
			return err
		}

		printConsentGranted(humanStderr(), id, owner.Username, req)
		return nil
	})
}

func runConsentList(args []string) error {
	fs := flag.NewFlagSet("consent list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, consentShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q：按用户筛用 --user", fs.Arg(0)))
	}

	return withConsentStore(func(ctx context.Context, db *sql.DB, store *snapshotwire.ConsentStore) error {
		// 不给 --user 就是全部用户：授权记录是运维要一眼看全的东西，而
		// 「某个用户有没有开过原文采集」这个问题，问的人未必先知道用户是谁。
		var userID int64
		if *user != "" {
			owner, err := auth.New(db).ResolveUser(ctx, *user)
			if err != nil {
				return err
			}
			userID = owner.ID
		}

		rows, err := store.List(ctx, userID)
		if err != nil {
			return err
		}

		if jsonOutput() {
			return emitJSON(os.Stdout, consentListDoc(rows), nil)
		}
		printConsentList(humanStdout(), rows)
		return nil
	})
}

func runConsentRevoke(args []string) error {
	if err := jsonUnsupported("consent revoke"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("consent revoke", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, consentShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return usageErr(errors.New("缺少要撤销的授权 ID：larkaxctl consent revoke <授权 ID>"))
	}
	if len(rest) > 1 {
		return usageErr(fmt.Errorf("revoke 只接受一个授权 ID，多出来的是 %q", rest[1]))
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || id <= 0 {
		return usageErr(fmt.Errorf("授权 ID 要是一个正整数，得到 %q", rest[0]))
	}

	return withConsentStore(func(ctx context.Context, _ *sql.DB, store *snapshotwire.ConsentStore) error {
		revoked, err := store.Revoke(ctx, id)
		if err != nil {
			return err
		}
		if !revoked {
			// 两种情形合成一句话：那一条不在库里，或者已经撤销过。这一层
			// 能给出的事实只有「没有任何一行被改」，而两种情形的下一步动作
			// 是同一条——先跑 consent list 看现在是什么。
			return fmt.Errorf("%w：id=%d（larkaxctl consent list 能看到现有的授权）",
				snapshotwire.ErrConsentNotFound, id)
		}
		_, _ = fmt.Fprintf(humanStderr(), "已撤销授权 id=%d：这条授权从此刻起不再放行原文\n", id)
		return nil
	})
}

// withConsentStore 打开数据库，把授权记录读写层交给 fn。
//
// 只做连库这一件事，不顺手读 settings：list 与 revoke 用不上配置，而把
// settings.Load 塞进来会让「库里的 settings 有一行坏掉」变成这两条命令也跑不了
// ——它们要回答的问题与配置无关（见 runConsentGrant 里那一份单独的读取）。
func withConsentStore(fn func(context.Context, *sql.DB, *snapshotwire.ConsentStore) error) error {
	return withDB(consentTimeout, func(ctx context.Context, db *sql.DB) error {
		return fn(ctx, db, snapshotwire.NewConsentStore(snapshotwire.Wrap(db)))
	})
}

// consentMaxWindow 读出申请时长的上限。
//
// 非正数在这里就报错，不交给下面的比较去处理：上限是 0 或负数时，任何申请都
// 会被判成「超过上限」，而那句话会把责任指向操作者——实际要改的是配置。
func consentMaxWindow(vals *settings.Values) (time.Duration, error) {
	hours, err := vals.Int(consentMaxWindowKey)
	if err != nil {
		return 0, fmt.Errorf("读取 settings.%s 失败: %w", consentMaxWindowKey, err)
	}
	if hours <= 0 {
		return 0, fmt.Errorf("settings.%s = %d 不是正数：先把它改成正数，否则任何申请都会被拒",
			consentMaxWindowKey, hours)
	}
	return time.Duration(hours) * time.Hour, nil
}

// parseConsentTime 解析一个时刻参数。
//
// 只认 RFC3339（与 --expires-at 那类参数一致）：授权记录进的是合规材料，
// 而「2026-10-18 15:00」这种写法少一个时区，落库时会按进程所在时区补一个，
// 于是同一份记录在两台机器上读出来是两个时刻。
func parseConsentTime(name, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, usageErr(fmt.Errorf(
			"%s 不是 RFC3339 时刻（例如 2026-10-18T15:00:00+08:00）: %w", name, err))
	}
	return t, nil
}

// consentDisclosure 是 grant 打给操作者看的那段话。
//
// 末尾空一行：下面紧跟的是结果或那句「未确认」，挤在一起读起来像同一段。
func consentDisclosure() string {
	return fmt.Sprintf("同意文案（版本 %d，会连同这段文字一起落库）：\n\n%s\n\n",
		snapshotwire.ConsentVersion, snapshotwire.ConsentText)
}

// printConsentGranted 打印一次授权的结果。
//
// 打在 stderr 上，与别的写类命令一致：stdout 留给「这一次运行的产出」，
// 而 grant 的产出是一条记录已经落库这个事实，不是可以进管道的数据。
//
// 用户只打名字（见 docs/cli-output.md 的「实体只点一次身份」）；范围那一段
// 打成申请时给的那几个字段，让操作者能当场核对落库的是不是自己要的那一条。
func printConsentGranted(w io.Writer, id int64, username string, req snapshotwire.ConsentRequest) {
	switch req.Scope {
	case snapshotwire.ConsentScopeSingleRequest:
		_, _ = fmt.Fprintf(w, "已开启原样采集授权 id=%d 用户=%s 范围=单条请求 request_id=%s 到期=%s\n",
			id, username, req.RequestID, req.ExpiresAt.Format(time.RFC3339))
	default:
		_, _ = fmt.Fprintf(w, "已开启原样采集授权 id=%d 用户=%s 范围=时间窗 %s → %s\n",
			id, username, req.WindowStart.Format(time.RFC3339), req.WindowEnd.Format(time.RFC3339))
	}
}

// printConsentList 打出授权清单。
//
// 表头用库里的列名（ID / USER / SCOPE），状态那一栏用中文：它不是一个列，
// 而是由撤销、到期、窗口三件事算出来的显示口径，翻译过来读的人才知道
// 「这一条现在还能不能换来原文」。
func printConsentList(w io.Writer, rows []snapshotwire.Consent) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("授权记录", "consent grant"))
		return
	}

	now := time.Now()
	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("ID"), p.bold("USER"), p.bold("SCOPE"), p.bold("STATE"),
		p.bold("EXPIRES"), p.bold("RANGE"))
	for _, c := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n",
			p.bold(strconv.FormatInt(c.ID, 10)), c.UserID, c.Scope,
			consentStateLabel(c.StateAt(now)),
			p.dim(c.ExpiresAt.Format("01-02 15:04")), consentRange(c))
	}
	_ = tw.Flush()
}

// consentStateLabel 把状态翻成给人看的那一栏。
func consentStateLabel(state string) string {
	switch state {
	case snapshotwire.ConsentStateActive:
		return "生效中"
	case snapshotwire.ConsentStatePending:
		return "未开始"
	case snapshotwire.ConsentStateRevoked:
		return "已撤销"
	default:
		return "已过期"
	}
}

// consentRange 打出这条授权管的是哪一段。
//
// 单条请求打 request_id，时间窗打两端：两者是这张表上唯一的「范围」表达，
// 而 list 的读者要拿它去与手里那个 request_id 对。
func consentRange(c snapshotwire.Consent) string {
	if c.Scope == snapshotwire.ConsentScopeSingleRequest {
		return c.RequestID
	}
	if c.WindowStart == nil || c.WindowEnd == nil {
		// 表上的 shape 约束要求时间窗那一档两端非空，走到这里说明行是被
		// 别的手段写进去的。不猜一个时刻，如实说这一栏读不出来。
		return "（时间窗缺失）"
	}
	return c.WindowStart.Format("01-02 15:04") + " → " + c.WindowEnd.Format("01-02 15:04")
}

// consentListDoc 组装 consent list 的机器可读输出。
//
// 与 printConsentList 是同一份数据的另一条渲染路径（见 format.go 开头）。
func consentListDoc(rows []snapshotwire.Consent) map[string]any {
	now := time.Now()
	doc := jsonDoc(jsonOK)

	active := 0
	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		if c.StateAt(now) == snapshotwire.ConsentStateActive {
			active++
		}
		entries = append(entries, consentEntry(c, now))
	}
	doc["summary"] = map[string]any{"total": len(rows), "active": active}
	doc["consents"] = entries
	return doc
}

// consentEntry 打清单里的一条授权。
//
// consent_text 与 consent_version 照打：这两列是这条授权当初同意的是什么，
// 而「同意文案与版本真的落库、可查」正是这条命令要能回答的问题。它是代码里
// 的常量，不是用户内容。
func consentEntry(c snapshotwire.Consent, now time.Time) map[string]any {
	return map[string]any{
		"id":              c.ID,
		"user_id":         c.UserID,
		"scope":           c.Scope,
		"request_id":      nullableString(c.RequestID),
		"window_start":    jsonTimePtr(c.WindowStart),
		"window_end":      jsonTimePtr(c.WindowEnd),
		"consent_text":    c.ConsentText,
		"consent_version": c.ConsentVersion,
		"agreed_at":       jsonTime(c.AgreedAt),
		"revoked_at":      jsonTimePtr(c.RevokedAt),
		"expires_at":      jsonTime(c.ExpiresAt),
		"state":           c.StateAt(now),
	}
}
