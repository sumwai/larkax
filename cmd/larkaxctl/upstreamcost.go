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

	"github.com/sumwai/larkax/internal/pricing"
)

// upstreamCostHelp 是 upstream-cost 的两版帮助文本（见 helpText）。
var upstreamCostHelp = helpText{short: upstreamCostShortUsage, long: upstreamCostUsage}

// upstreamCostShortUsage 是 upstream-cost 的短版：`-h` / `--help` 与参数写错时打的那一份。
const upstreamCostShortUsage = `larkaxctl upstream-cost 管理上游成本规则。

用法：
  larkaxctl upstream-cost create --channel <渠道 ID> --expr '<表达式>'
                                 [--models <规范模型名,...>] [--priority <优先级>]
                                 [--effective-from <RFC3339>] [--effective-to <RFC3339>]
  larkaxctl upstream-cost list [--channel <渠道 ID>]
                               [--model <规范模型名>] [--at <RFC3339>]
  larkaxctl upstream-cost update --id <规则 ID> [与 create 相同的字段]
                                 [--clear-effective-to]
  larkaxctl upstream-cost disable <规则 ID>

一条规则说的是「这条渠道上，这个模型的上游成本按哪个表达式算」。
list 带 --model（要同时给 --channel）时按裁决次序列出适用的规则，
标着「选中」的那条就是现在按它算成本的那条。

更细的边界：larkaxctl help upstream-cost。
`

// upstreamCostUsage 是 upstream-cost 的长版：`larkaxctl help upstream-cost` 与
// `upstream-cost help`。
const upstreamCostUsage = `larkaxctl upstream-cost 管理上游成本规则。

用法：
  larkaxctl upstream-cost create --channel <渠道 ID> --expr '<表达式>'
                                 [--models <规范模型名,...>] [--priority <优先级>]
                                 [--effective-from <RFC3339>] [--effective-to <RFC3339>]
  larkaxctl upstream-cost list [--channel <渠道 ID>]
                               [--model <规范模型名>] [--at <RFC3339>]
  larkaxctl upstream-cost update --id <规则 ID> [--channel <渠道 ID>]
                                 [--models <规范模型名,...>] [--expr '<表达式>']
                                 [--priority <优先级>] [--effective-from <RFC3339>]
                                 [--effective-to <RFC3339>] [--clear-effective-to]
  larkaxctl upstream-cost disable <规则 ID>

一条成本规则记的是「这条渠道上这个模型的上游成本按哪个表达式算」，字段与
upstream_costs 的列一一对应。--channel 必填：同一个模型在两条渠道上是两个上游、
两个价。

--models 留空表示适用于全部模型（兜底规则）；给了就按元素逐字匹配，不支持通配符
——带 * 或 ? 的名字会被拒绝。模型名用规范模型名，与 price 同一套键。

表达式与售价规则同一个方言，写入前会编译一次，编译不过的命令直接失败。

裁决次序：渠道 → priority 大的优先 → 模型名单非空的优先于兜底 → id 大的优先。
一条规则要被选中，还要满足此刻在它的生效区间内（左闭右开：生效自 <= 此刻 <
生效至）。

list 不带 --model 时列出规则清单，同一条渠道内按裁决次序排列。带 --model 时
（要同时给 --channel）只列适用的规则，按同一个次序，标着「选中」的那条就是现在
按它算成本的那条，排在它后面的每一条都在说明它落选的原因。一条都不适用时不是
失败：命令明说「没有适用的成本规则。」并以 0 退出——库里这一刻没有这条规则，
不是命令或数据出了问题。--at 用来问过去某一刻，留空表示现在。

update 只改真的给了的那几项，其余保持库里的值。「不再过期」用
--clear-effective-to，不要用 --effective-to 填一个很远的未来：那会在库里留一个
假的日期。

disable <规则 ID> 把生效区间收在当下：它从现在起不再适用。已经失效的规则、以及
还没生效的规则会被拒绝（后者会写出一个空区间），两种情况都说清原因。

更细的约定见 docs/cli-reference.md 的「upstream-cost」一节。
`

func runUpstreamCost(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, upstreamCostShortUsage)
		return usageErr(errors.New("upstream-cost 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create":
		return runUpstreamCostCreate(rest)
	case "list":
		return runUpstreamCostList(rest)
	case "update":
		return runUpstreamCostUpdate(rest)
	case "disable":
		return runUpstreamCostDisable(rest)
	case "-h", "--help":
		printUsage(os.Stdout, upstreamCostShortUsage)
		return nil
	default:
		printUsage(os.Stderr, upstreamCostShortUsage)
		return usageErr(fmt.Errorf("未知的 upstream-cost 子命令 %q", action))
	}
}

func runUpstreamCostCreate(args []string) error {
	if err := jsonUnsupported("upstream-cost create"); err != nil {
		return err
	}

	req, err := parseUpstreamCostCreate(args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		id, createErr := pricing.NewCostAdmin(db).Create(ctx, req)
		if createErr != nil {
			return createErr
		}
		printCreatedCostRule(humanStdout(), humanStderr(), id, req)
		return nil
	})
}

// parseUpstreamCostCreate 解析 create 的参数。
//
// 它只做「有没有给、给了什么」，校验放在包那一侧（validateCost）：读路径按
// 渠道与生效区间匹配，那条判据只有一份。
func parseUpstreamCostCreate(args []string) (pricing.CostCreateRequest, error) {
	fs := flag.NewFlagSet("upstream-cost create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, upstreamCostShortUsage) }
	channelID := fs.Int64("channel", 0, "这条规则挂在哪个渠道上")
	models := fs.String("models", "", "")
	expression := fs.String("expr", "", "")
	priority := fs.Int("priority", 0, "")
	effectiveFrom := fs.String("effective-from", "", "")
	effectiveTo := fs.String("effective-to", "", "")
	if err := fs.Parse(args); err != nil {
		return pricing.CostCreateRequest{}, err
	}
	if fs.NArg() > 0 {
		return pricing.CostCreateRequest{}, fmt.Errorf(
			"create 不接受位置参数，得到 %q：表达式用 --expr 指定（记得给 shell 加引号）", fs.Arg(0))
	}

	if *channelID == 0 {
		return pricing.CostCreateRequest{}, errors.New(
			"缺少 --channel：成本规则挂在一条渠道上")
	}
	if *channelID < 0 {
		return pricing.CostCreateRequest{}, fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID)
	}
	if strings.TrimSpace(*expression) == "" {
		return pricing.CostCreateRequest{}, errors.New("缺少 --expr")
	}
	from, err := parseTimeFlag("--effective-from", *effectiveFrom)
	if err != nil {
		return pricing.CostCreateRequest{}, err
	}
	to, err := parseTimeFlag("--effective-to", *effectiveTo)
	if err != nil {
		return pricing.CostCreateRequest{}, err
	}

	req := pricing.CostCreateRequest{
		ChannelID:     *channelID,
		ModelPatterns: splitOptionalList(*models),
		Expr:          *expression,
		Priority:      *priority,
		EffectiveTo:   to,
	}
	if from != nil {
		req.EffectiveFrom = *from
	}
	return req, nil
}

func runUpstreamCostUpdate(args []string) error {
	if err := jsonUnsupported("upstream-cost update"); err != nil {
		return err
	}

	req, err := parseUpstreamCostUpdate(args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		if updateErr := pricing.NewCostAdmin(db).Update(ctx, req); updateErr != nil {
			return updateErr
		}
		_, _ = fmt.Fprintf(humanStderr(), "已更新成本规则 %d。\n", req.ID)
		return nil
	})
}

// parseUpstreamCostUpdate 解析 update 的参数。
//
// 这一组旗标没有别名（与 price 那几条不同：那里留着的 --from / --to / --owner
// 是改名前就在跑的写法），所以判「给没给」看 flag 自己的名字就够。
func parseUpstreamCostUpdate(args []string) (pricing.CostUpdateRequest, error) {
	fs := flag.NewFlagSet("upstream-cost update", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, upstreamCostShortUsage) }
	id := fs.Int64("id", 0, "要改的成本规则 ID")
	channelID := fs.Int64("channel", 0, "改挂到哪条渠道上")
	models := fs.String("models", "", "")
	expression := fs.String("expr", "", "")
	priority := fs.Int("priority", 0, "")
	effectiveFrom := fs.String("effective-from", "", "")
	effectiveTo := fs.String("effective-to", "", "")
	clearEffectiveTo := fs.Bool("clear-effective-to", false, "")
	if err := fs.Parse(args); err != nil {
		return pricing.CostUpdateRequest{}, err
	}
	if fs.NArg() > 0 {
		return pricing.CostUpdateRequest{}, fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的规则用 --id 指定", fs.Arg(0))
	}
	if *id <= 0 {
		return pricing.CostUpdateRequest{}, errors.New("缺少 --id（要改的成本规则 ID）")
	}
	if *channelID < 0 {
		return pricing.CostUpdateRequest{}, fmt.Errorf(
			"--channel %d 无效：渠道 ID 从 1 开始", *channelID)
	}
	if *clearEffectiveTo && *effectiveTo != "" {
		return pricing.CostUpdateRequest{}, errors.New(
			"--clear-effective-to 与 --effective-to 只能给一个：前者是清成不过期，" +
				"后者是改成某个时刻")
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	req := pricing.CostUpdateRequest{ID: *id}
	changes := 0
	if given["channel"] {
		if *channelID == 0 {
			return pricing.CostUpdateRequest{}, errors.New(
				"--channel 不能是 0：成本规则必须挂在一条渠道上（渠道 ID 从 1 开始）")
		}
		req.ChannelID = channelID
		changes++
	}
	if given["models"] {
		list := splitOptionalList(*models)
		req.ModelPatterns = &list
		changes++
	}
	if given["expr"] {
		if strings.TrimSpace(*expression) == "" {
			return pricing.CostUpdateRequest{}, errors.New(
				"--expr 不能为空（表上那一栏非空）：要停用这条规则用 upstream-cost disable")
		}
		req.Expr = expression
		changes++
	}
	if given["priority"] {
		req.Priority = priority
		changes++
	}
	// 生效下界清不掉（那一栏非空），所以给了空串要当场拒：静默地当成
	// 「不改这一项」会让一次只想改下界的调用变成「什么都没改」，
	// 而它看起来像一次成功的修改。
	if given["effective-from"] {
		if strings.TrimSpace(*effectiveFrom) == "" {
			return pricing.CostUpdateRequest{}, errors.New(
				"--effective-from 不能为空（表上那一栏非空）：这一栏清不掉")
		}
		from, err := parseTimeFlag("--effective-from", *effectiveFrom)
		if err != nil {
			return pricing.CostUpdateRequest{}, err
		}
		req.EffectiveFrom = from
		changes++
	}
	if given["effective-to"] {
		if strings.TrimSpace(*effectiveTo) == "" {
			return pricing.CostUpdateRequest{}, errors.New(
				"--effective-to 不能为空：要清成不过期用 --clear-effective-to")
		}
		to, err := parseTimeFlag("--effective-to", *effectiveTo)
		if err != nil {
			return pricing.CostUpdateRequest{}, err
		}
		req.EffectiveTo = to
		changes++
	}
	// 这一项看的是取值而不是「给没给」：--clear-effective-to=false 是
	// 「不要这么做」，不是一次修改。
	if *clearEffectiveTo {
		req.ClearEffectiveTo = true
		changes++
	}
	if changes == 0 {
		return pricing.CostUpdateRequest{}, errors.New(
			"update 至少要改一项：给出 --channel / --models / --expr / --priority / " +
				"--effective-from / --effective-to / --clear-effective-to 之一" +
				"（停用规则用 upstream-cost disable）")
	}
	return req, nil
}

// runUpstreamCostDisable 停用一条成本规则。
//
// 规则 ID 是位置参数，与 `price disable <规则 ID>` 同形：同一个动作有两种写法时，
// 帮助文本要说两遍，而人多半会猜错一遍。
func runUpstreamCostDisable(args []string) error {
	if err := jsonUnsupported("upstream-cost disable"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("upstream-cost disable", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, upstreamCostShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		return usageErr(fmt.Errorf(
			"disable 需要一个规则 ID，得到 %d 个参数：用法 larkaxctl upstream-cost disable <规则 ID>",
			fs.NArg()))
	}
	id, err := strconv.ParseInt(strings.TrimSpace(fs.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		return usageErr(fmt.Errorf("disable 的规则 ID 必须是正整数，当前为 %q", fs.Arg(0)))
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		if disableErr := pricing.NewCostAdmin(db).Disable(ctx, id); disableErr != nil {
			return disableErr
		}
		_, _ = fmt.Fprintf(humanStderr(), "成本规则 %d 已停用：从现在起不再适用。\n", id)
		return nil
	})
}

// runUpstreamCostList 列出成本规则，或回答「这条渠道这个模型按哪条算成本」。
//
// # 为什么这两件事同一条命令
//
// 它问的是同一张表、同一套次序，差别只在「要不要按模型裁决」：不带 --model 时
// 列的是一批规则，带 --model 时列的是这批规则里适用于这一次的那些，而顺序本身
// 就是裁决依据。分成两条命令会让「有哪些规则」与「用哪条」各排一次序，那两次
// 排序分岔时的表现是「清单里第一条不是它」，而清单正是用来回答这个问题的。
func runUpstreamCostList(args []string) error {
	fs := flag.NewFlagSet("upstream-cost list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, upstreamCostShortUsage) }
	channelID := fs.Int64("channel", 0, "只看这条渠道的规则")
	model := fs.String("model", "", "这条渠道上的哪个模型（规范模型名）")
	at := fs.String("at", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("list 不接受位置参数，得到 %q", fs.Arg(0)))
	}
	if *channelID < 0 {
		return usageErr(fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID))
	}
	modelName := strings.TrimSpace(*model)
	if modelName != "" && *channelID == 0 {
		return usageErr(errors.New(
			"--model 要同时给 --channel：成本规则挂在渠道上，只给模型问不出按哪条算"))
	}
	when, err := parseTimeFlag("--at", *at)
	if err != nil {
		return usageErr(err)
	}
	if when != nil && modelName == "" {
		return usageErr(errors.New("--at 只在给了 --model 时才有意义：它问的是某一次请求"))
	}
	// 时刻在这里定下来再交给包：包在零值时会自己取当前时刻，而那样打出来的
	// 表头与真正用的那一刻是两个数——「问的是哪一刻」正是这一栏要回答的问题。
	if modelName != "" && when == nil {
		now := time.Now()
		when = &now
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		q := pricing.CostListQuery{ChannelID: *channelID, CanonicalModel: modelName}
		if when != nil {
			q.At = *when
		}
		rules, listErr := pricing.NewCostAdmin(db).List(ctx, q)
		if listErr != nil {
			return listErr
		}

		// 给了模型就问一次选择器：标着「选中」的那一行由它说了算，而不是由
		// 「表里排第一的那一行」推出来。两处不一致时那一行不会被标上——那是
		// 看得见的错，而不是一张悄悄排错序的表。
		var selectedID int64
		if modelName != "" {
			rule, matchErr := pricing.NewCostSource(db).Match(ctx, pricing.CostQuery{
				ChannelID:      *channelID,
				CanonicalModel: modelName,
				At:             q.At,
			})
			switch {
			case errors.Is(matchErr, pricing.ErrNoCostRule):
				// 一条都不适用：下面那张表本来也是空的，由它把话说清楚。
			case matchErr != nil:
				return matchErr
			default:
				selectedID = rule.ID
			}
		}

		if jsonOutput() {
			return emitJSON(os.Stdout, costListDoc(q, rules, selectedID), nil)
		}
		printCostList(humanStdout(), q, rules, selectedID)
		return nil
	})
}

// printCreatedCostRule 打印新规则。
//
// 与 printCreatedRule 同一条契约：stdout 恰好一行，只放能被直接接走的值。
func printCreatedCostRule(stdout, stderr io.Writer, id int64, req pricing.CostCreateRequest) {
	_, _ = fmt.Fprintf(stderr, "已加入成本规则 id=%d：渠道 %d。\n", id, req.ChannelID)
	_, _ = fmt.Fprintln(stdout, id)
}

// printCostList 打印成本规则清单，或一次裁决说明。
//
// 带模型的那一问多打两行：问的是哪条渠道哪个模型哪一刻，以及那张表怎么读。
// 两张表共用 printCostTable——两处各拼一份格式串的话，加一栏时改了一处、
// 另一处还是老样子，而它们说的是同一批规则。
func printCostList(w io.Writer, q pricing.CostListQuery, rules []pricing.CostInfo, selectedID int64) {
	if q.CanonicalModel != "" {
		_, _ = fmt.Fprintf(w, "渠道 %d，模型 %s，时刻 %s：\n",
			q.ChannelID, q.CanonicalModel, q.At.Format(time.RFC3339))
	}
	if len(rules) == 0 {
		if q.CanonicalModel != "" {
			// 与 price explain 不同，这一条不是失败：请求路径上成本那一维不
			// 参与拒绝（见 pricing.ErrNoCostRule），所以这里给结论、给 0 退出码。
			_, _ = fmt.Fprintln(w, "没有适用的成本规则。")
			return
		}
		_, _ = fmt.Fprintln(w, emptyListNote("成本规则", "upstream-cost create"))
		return
	}

	var verdictOf func(int) string
	if q.CanonicalModel != "" {
		verdictOf = func(i int) string {
			if rules[i].ID == selectedID {
				return "选中"
			}
			return "被压住"
		}
	}
	printCostTable(w, rules, verdictOf)
	if q.CanonicalModel != "" {
		_, _ = fmt.Fprintln(w, "「选中」那条就是这条渠道这个模型现在按它算成本的那条；"+
			"排在它后面的每一条都在说明它落选的原因。")
	}
}

// printCostTable 打印一张成本规则表。verdictOf 非 nil 时多一栏「裁决」。
func printCostTable(w io.Writer, rules []pricing.CostInfo, verdictOf func(int) string) {
	tw := newTableWriter(w)
	p := newPalette(w)
	headers := []string{"ID", "渠道", "模型", "优先级", "生效自", "生效至", "单价键"}
	if verdictOf != nil {
		headers = append(headers, "裁决")
	}
	bolded := make([]string, len(headers))
	for i, h := range headers {
		bolded[i] = p.bold(h)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(bolded, "\t"))

	for i, r := range rules {
		line := fmt.Sprintf("%s\t%d\t%s\t%d\t%s\t%s\t%s",
			p.bold(strconv.FormatInt(r.ID, 10)), r.ChannelID,
			joinOrDash(r.ModelPatterns), r.Priority,
			p.dim(r.EffectiveFrom.Format(time.RFC3339)),
			p.dim(effectiveToLabel(r.EffectiveTo)),
			declaredCostKeysLabel(r))
		if verdictOf != nil {
			line += "\t" + verdictOf(i)
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()

	// 表达式编译不过的行单独说明，不跳过也不整次报错（见 pricing.CostAdmin.List）。
	for _, r := range rules {
		if r.ExprErr != nil {
			_, _ = fmt.Fprintf(w, "成本规则 %d 的表达式编译不过：%v\n", r.ID, r.ExprErr)
		}
	}
}

// declaredCostKeysLabel 打这条规则声明了哪几项单价。
//
// 空数组意味着这条规则一项单价都没写——那时每一笔的成本都算成 0，而毛利会因此
// 虚高。所以这里要打出来，而且空的时候要显眼。
func declaredCostKeysLabel(r pricing.CostInfo) string {
	if r.ExprErr != nil {
		return "（表达式编译不过）"
	}
	if len(r.DeclaredKeys) == 0 {
		return "（一项都没有）"
	}
	return strings.Join(r.DeclaredKeys, ",")
}

// costListDoc 组装 upstream-cost list 的机器可读输出。
//
// 与 printCostList 是同一份数据的另一条渲染路径：那一档把「不过期」与模型栏的
// `-` 铺成给人扫的表，这一档给布尔、null 与原始值。
func costListDoc(q pricing.CostListQuery, rules []pricing.CostInfo, selectedID int64) map[string]any {
	doc := jsonDoc(jsonOK)
	// query 里那三个字段说的是这一次问的是什么：没给 --channel 或 --model 时
	// 它们是 null，与表格那一档不去裁决同一件事。
	doc["query"] = map[string]any{
		"channel_id":      jsonID(q.ChannelID),
		"canonical_model": nullableString(q.CanonicalModel),
		"at":              jsonTimeOrNull(q.At),
	}
	doc["summary"] = map[string]any{"total": len(rules)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		entry := costEntry(r)
		entry["verdict"] = costVerdict(q.CanonicalModel, r.ID, selectedID)
		entries = append(entries, entry)
	}
	doc["costs"] = entries
	return doc
}

// costEntry 打一条成本规则的原始值。
//
// 与 ruleEntry 一样不打表达式源码：这一栏要回答的是「有哪些规则、哪条会赢」，
// 源码在写入的时候就在命令行上；声明了哪几项由 quota_keys 给出。
func costEntry(r pricing.CostInfo) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"channel_id":     r.ChannelID,
		"model_patterns": jsonList(r.ModelPatterns),
		"priority":       r.Priority,
		"effective_from": jsonTime(r.EffectiveFrom),
		"effective_to":   jsonTimePtr(r.EffectiveTo),
		"quota_keys":     jsonList(r.DeclaredKeys),
		"expr_error":     nullableError(r.ExprErr),
	}
}

// costVerdict 是「裁决」那一栏在 JSON 里的取值。
//
// 给英文的两个词而不是表格那一档的「选中 / 被压住」：它是这个字段的取值域，
// 脚本按它分派（`select(.verdict == "selected")`），而中文渲染属于终端那一档。
// 没给 --model 时是 null——那一次调用没有裁决可谈。
func costVerdict(model string, id, selectedID int64) any {
	if model == "" {
		return nil
	}
	if id == selectedID {
		return "selected"
	}
	return "shadowed"
}
