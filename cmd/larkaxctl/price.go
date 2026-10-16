package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/pricing"
)

const priceUsage = `larkaxctl price 管理计价规则。

用法：
  larkaxctl price add --scope <platform|merchant|channel> --name <规则名> --expr '<表达式>'
                      [--owner <用户 ID 或用户名>] [--channel <渠道 ID>]
                      [--models <规范模型名,...>] [--priority <优先级>]
                      [--effective-from <RFC3339>] [--effective-to <RFC3339>]
                      [--disabled]
  larkaxctl price list

作用域决定哪两栏 id 有效：platform 两栏都留空；merchant 必须给 --owner；
channel 必须给 --channel。写了读取路径不看的那一栏会被拒绝，因为那是一条
「配了却不生效」的规则。

--models 留空表示适用于全部模型（兜底规则）；给了就按元素逐字匹配，
不支持通配符——带 * 或 ? 的名字会被拒绝。

表达式在写入前会编译一次，编译不过的命令直接失败：一条编译不过的规则进了库，
表现是每个用到它的请求都失败，而失败点在请求路径上。

计价项一共五个：prompt、completion、cache_read、cache_write、reasoning。
上游报了哪一项，规则里就必须有哪一项的单价——少一项不是少收，是这次请求整个
失败（结算报「用量里有规则未定价的计量项」）。会报缓存的上游很常见，所以新规则
把这五项都写上、用不到的填 0，比漏掉再被拦住省事。

list 按**裁决次序**排列：适用于某次请求的第一条就是会被选中的那条。
它不打印表达式源码，只打印规则声明了哪几项单价。
`

func runPrice(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, priceUsage)
		return usageErr(errors.New("price 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runPriceAdd(rest)
	case "list":
		return runPriceList(rest)
	case "help", "-h", "--help":
		fmt.Print(priceUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, priceUsage)
		return usageErr(fmt.Errorf("未知的 price 子命令 %q", action))
	}
}

func runPriceAdd(args []string) error {
	req, ownerRef, err := parsePriceAdd(args)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if ownerRef != "" {
		ownerID, resolveErr := resolveOwnerID(ctx, db, ownerRef)
		if resolveErr != nil {
			return resolveErr
		}
		req.OwnerUserID = ownerID
	}

	id, err := pricing.NewAdmin(db).Create(ctx, req)
	if err != nil {
		return err
	}
	printCreatedRule(os.Stdout, os.Stderr, id, req)
	return nil
}

// parsePriceAdd 解析 add 的参数。
//
// 它多带一个 ownerRef 返回值：--owner 要连库才能解析成用户 ID，而参数解析
// 必须在连库之前完成。所以这里只做「有没有给、给了什么」，解析留给调用方。
func parsePriceAdd(args []string) (pricing.CreateRequest, string, error) {
	fs := flag.NewFlagSet("price add", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, priceUsage) }
	scope := fs.String("scope", "", "作用域：platform、merchant、channel")
	name := fs.String("name", "", "规则名，列举与告警都靠它指认一条规则")
	expression := fs.String("expr", "", "规则表达式源码")
	owner := fs.String("owner", "", "商家的用户 ID 或用户名，merchant 作用域必填")
	channelID := fs.Int64("channel", 0, "渠道 ID，channel 作用域必填")
	models := fs.String("models", "", "适用的规范模型名，逗号分隔，留空表示全部")
	priority := fs.Int("priority", 0, "优先级，同一作用域里越大越优先")
	effectiveFrom := fs.String("effective-from", "", "生效时刻（RFC3339），留空表示从现在起")
	effectiveTo := fs.String("effective-to", "", "失效时刻（RFC3339），留空表示不过期")
	disabled := fs.Bool("disabled", false, "建一条停用的规则")
	if err := fs.Parse(args); err != nil {
		return pricing.CreateRequest{}, "", err
	}
	if fs.NArg() > 0 {
		return pricing.CreateRequest{}, "", fmt.Errorf(
			"add 不接受位置参数，得到 %q：表达式用 --expr 指定（记得给 shell 加引号）", fs.Arg(0))
	}

	if strings.TrimSpace(*scope) == "" {
		fs.Usage()
		return pricing.CreateRequest{}, "", errors.New("缺少 --scope（platform / merchant / channel）")
	}
	if strings.TrimSpace(*name) == "" {
		fs.Usage()
		return pricing.CreateRequest{}, "", errors.New("缺少 --name")
	}
	if strings.TrimSpace(*expression) == "" {
		fs.Usage()
		return pricing.CreateRequest{}, "", errors.New("缺少 --expr")
	}
	if *channelID < 0 {
		return pricing.CreateRequest{}, "", fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID)
	}
	from, err := parseTimeFlag("--effective-from", *effectiveFrom)
	if err != nil {
		return pricing.CreateRequest{}, "", err
	}
	to, err := parseTimeFlag("--effective-to", *effectiveTo)
	if err != nil {
		return pricing.CreateRequest{}, "", err
	}

	req := pricing.CreateRequest{
		Scope:         *scope,
		ChannelID:     *channelID,
		Name:          *name,
		ModelPatterns: splitOptionalList(*models),
		Expr:          *expression,
		Priority:      *priority,
		EffectiveTo:   to,
	}
	if from != nil {
		req.EffectiveFrom = *from
	}
	// --disabled 是「明确要一条停用的规则」。不给这个标志时留 nil，
	// 让包按它的默认走（启用）——nil 与 &false 在那里是两件事，
	// 而「忘了给标志」不该变成一条静默停用的规则。
	if *disabled {
		enabled := false
		req.Enabled = &enabled
	}
	return req, *owner, nil
}

func runPriceList(args []string) error {
	fs := flag.NewFlagSet("price list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, priceUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	rules, err := pricing.NewAdmin(db).List(ctx)
	if err != nil {
		return err
	}
	printRuleList(os.Stdout, rules)
	return nil
}

// printCreatedRule 打印新规则。
//
// 与 printCreatedUser 同一条契约：stdout 恰好一行，只放能被直接接走的值。
func printCreatedRule(stdout, stderr io.Writer, id int64, req pricing.CreateRequest) {
	_, _ = fmt.Fprintf(stderr, "已加入计价规则 id=%d：%s（作用域=%s）。\n", id, req.Name, req.Scope)
	_, _ = fmt.Fprintln(stdout, id)
}

// printRuleList 打印规则清单。
//
// 排序用的是写入路径的裁决次序，所以「排在第一的那条」与「请求会被按它计价
// 的那条」是同一件事（见 pricing.Admin.List）。表达式源码不在这里：
// 这一栏要回答的是「有哪些规则、哪条会赢」，而源码在 add 的时候就在命令行上；
// 声明了哪几项单价由最后一栏给出。
func printRuleList(w io.Writer, rules []pricing.RuleInfo) {
	if len(rules) == 0 {
		_, _ = fmt.Fprintln(w, "还没有计价规则。")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\t作用域\t商家\t渠道\t名称\t模型\t优先级\t状态\t生效自\t生效至\t单价键")
	for _, r := range rules {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			r.ID, r.Scope, priceOwnerLabel(r.OwnerUserID), priceChannelLabel(r.ChannelID),
			r.Name, joinOrDash(r.ModelPatterns), r.Priority, enabledLabel(r),
			r.EffectiveFrom.Format(time.RFC3339), effectiveToLabel(r.EffectiveTo),
			declaredKeysLabel(r))
	}
	_ = tw.Flush()

	// 表达式编译不过的行单独说明，不跳过也不整次报错（见 pricing.Admin.List）：
	// 库里有一条坏规则时整个清单一条都看不到，而那正是最需要看清单的时候。
	for _, r := range rules {
		if r.ExprErr != nil {
			_, _ = fmt.Fprintf(w, "规则 %d 的表达式编译不过：%v\n", r.ID, r.ExprErr)
		}
	}
}

// priceOwnerLabel 打商家那一栏。
//
// 0 表示不挂商家（平台价），见 pricing.RuleInfo 的注释。打 0 会让人以为
// 那是一个真实的用户。
func priceOwnerLabel(ownerUserID int64) string {
	if ownerUserID == 0 {
		return "-"
	}
	return strconv.FormatInt(ownerUserID, 10)
}

// priceChannelLabel 打渠道那一栏。0 表示不挂渠道。
func priceChannelLabel(channelID int64) string {
	if channelID == 0 {
		return "-"
	}
	return strconv.FormatInt(channelID, 10)
}

// enabledLabel 打状态那一栏。
func enabledLabel(r pricing.RuleInfo) string {
	if r.Enabled {
		return "启用"
	}
	return "停用"
}

// effectiveToLabel 打失效时刻，nil 表示不过期。
func effectiveToLabel(to *time.Time) string {
	if to == nil {
		return "不过期"
	}
	return to.Format(time.RFC3339)
}

// declaredKeysLabel 打这条规则声明了哪几项单价。
//
// 空数组意味着这条规则一项单价都没写——那时每一笔都按 0 元计，而结算那一侧
// 要到结算时才拦（见 pricing.RuleInfo.DeclaredKeys）。所以这里要打出来，
// 而且空的时候要显眼。
func declaredKeysLabel(r pricing.RuleInfo) string {
	if r.ExprErr != nil {
		return "（表达式编译不过）"
	}
	if len(r.DeclaredKeys) == 0 {
		return "（一项都没有）"
	}
	return strings.Join(r.DeclaredKeys, ",")
}

// parseTimeFlag 解析一个 RFC3339 时刻参数，空串表示不填。
//
// 空串返回 nil：生效区间那两栏在库里可空，nil 表示「不设边界」（见
// pricing.RuleInfo），是这一栏的合法取值，不是「出错了但没报」——
// 所以这里显式 nolint。
func parseTimeFlag(name, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil //nolint:nilnil // 见上
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%s 不是 RFC3339 时间（例如 2026-12-31T00:00:00Z）: %w", name, err)
	}
	return &parsed, nil
}
