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

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/pricing"
)

const priceUsage = `larkaxctl price 管理计价规则。

用法：
  larkaxctl price create --scope <platform|merchant|channel> --name <规则名> --expr '<表达式>'
                         [--user <用户 ID 或用户名>] [--channel <渠道 ID>]
                         [--models <规范模型名,...>] [--priority <优先级>]
                         [--effective-from <RFC3339>] [--effective-to <RFC3339>]
                         [--disabled]
  larkaxctl price list
  larkaxctl price update --id <规则 ID>
                         [--scope <platform|merchant|channel>] [--name <规则名>]
                         [--user <用户 ID 或用户名>] [--channel <渠道 ID>]
                         [--models <规范模型名,...>] [--expr '<表达式>']
                         [--priority <优先级>] [--effective-from <RFC3339>]
                         [--effective-to <RFC3339>] [--clear-effective-to]
  larkaxctl price enable <规则 ID>
  larkaxctl price disable <规则 ID>
  larkaxctl price explain --model <规范模型名>
                          [--user <用户 ID 或用户名>] [--channel <渠道 ID>]
                          [--at <RFC3339>]
  larkaxctl price calendar import <文件>
        导入某一年的日历数据（分时计价要用它判断哪天是休息日，
        文件格式见 larkaxctl price calendar --help）

作用域决定哪两栏 id 有效：platform 两栏都留空；merchant 必须给 --user；
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
裁决次序是作用域（channel > merchant > platform）→ priority 大的优先 →
模型名单非空的优先于兜底 → id 大的优先。所以同作用域同优先级下，后加一条
兜底不会盖掉已有的模型专属价；要让兜底赢，把它的 --priority 调高。
它不打印表达式源码，只打印规则声明了哪几项单价。

update **只改真的给了的那几项**，其余保持库里的值。校验的对象是**改完之后
的那一行**，所以换作用域时要把该清的那一栏一起清掉：改成 platform 用
--user 0 与 --channel 0，改成 merchant 要给 --user，改成 channel 要给
--channel。给空串的 --user 会被拒——那多半是一个没展开的变量，而它读起来
像「不挂商家」。「不再过期」用 --clear-effective-to，不要用 --effective-to
填一个很远的未来：那会在库里留一个假的日期。

**启停是独立的二级动词**：price enable <规则 ID> / price disable <规则 ID>。
改属性的命令不带状态开关——「顺便改个状态」与「启用这条规则」是两种意图，
混在一个命令里时，那条 update 的最后一行日志说不清是哪一种。

**旧名仍可用**：price update 上的 --enable 与 --disable 是这次改名的旧形态，
--owner 等同于 --user，price add 等同于 price create。它们保留是为了不让已经
在跑的脚本与部署流程坏掉，行为与新名字逐字相同；新写的命令请用新名字。

explain 回答「这次请求按哪条规则计价」：它把**全部适用**的规则按裁决次序
列出来，第一条就是会被选中的那条，排在它后面的每一条都在说明「为什么不是
它」。--at 用来问过去某一刻（对账时问「这一笔当时按哪条算的」），留空表示
现在。--user 与 --channel 是这次请求的商家与渠道，都不给时按平台级规则算。
一条适用的都没有时非零退出，与请求路径上的处置一致（那是拒绝服务，不是
「按 0 元计」）。
`

func runPrice(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, priceUsage)
		return usageErr(errors.New("price 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create", "add":
		return runPriceCreate(rest)
	case "list":
		return runPriceList(rest)
	case "update":
		return runPriceUpdate(rest)
	case "enable":
		return runPriceSetEnabled(rest, true)
	case "disable":
		return runPriceSetEnabled(rest, false)
	case "explain":
		return runPriceExplain(rest)
	case "calendar":
		// 分时计价的日历原先是一个顶层命令组（larkaxctl calendar），#475 把它
		// 收进 price：这些数据是计价规则的输入，离开规则没有别的用处。
		return runPriceCalendar(rest)
	case "help", "-h", "--help":
		fmt.Print(priceUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, priceUsage)
		return usageErr(fmt.Errorf("未知的 price 子命令 %q", action))
	}
}

func runPriceCreate(args []string) error {
	if err := jsonUnsupported("price create"); err != nil {
		return err
	}

	req, ownerRef, err := parsePriceCreate(args)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
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

// parsePriceCreate 解析 create 的参数。
//
// 它多带一个 ownerRef 返回值：--user 要连库才能解析成用户 ID，而参数解析
// 必须在连库之前完成。所以这里只做「有没有给、给了什么」，解析留给调用方。
func parsePriceCreate(args []string) (pricing.CreateRequest, string, error) {
	fs := flag.NewFlagSet("price create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, priceUsage) }
	scope := fs.String("scope", "", "作用域：platform、merchant、channel")
	name := fs.String("name", "", "规则名，列举与告警都靠它指认一条规则")
	expression := fs.String("expr", "", "规则表达式源码")
	owner := legacyStringFlag(fs, "user", "owner", "", "商家的用户 ID 或用户名，merchant 作用域必填")
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
			"create 不接受位置参数，得到 %q：表达式用 --expr 指定（记得给 shell 加引号）", fs.Arg(0))
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

func runPriceUpdate(args []string) error {
	if err := jsonUnsupported("price update"); err != nil {
		return err
	}

	req, ownerRef, err := parsePriceUpdate(args)
	if err != nil {
		return usageErr(err)
	}

	cfg, err := config.LoadOps()
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
		req.OwnerUserID = &ownerID
	}

	if err := pricing.NewAdmin(db).Update(ctx, req); err != nil {
		return err
	}
	fmt.Printf("已更新计价规则 %d。\n", req.ID)
	return nil
}

// runPriceSetEnabled 启用或停用一条计价规则。
//
// # 为什么是独立的二级动词，而不是 update 上的一个开关
//
// 原来写成 `price update --id 1 --enable`：状态开关挂在「改属性」的命令上，
// 于是「顺手把状态也改一下」与「启用这条规则」是同一行命令。而 channel 与
// plugin 早就是 `enable|disable <对象>` 的形态（见 issue #477 的规范表）。
//
// 旧写法没有删：`--enable` / `--disable` 仍然能在 update 上给（见
// parsePriceUpdate），已经在跑的脚本不该因为这次改名坏掉。
//
// # 规则 ID 是位置参数
//
// 与 `plugin enable <key>` 同形，也与规范表里的示例 `price enable <id>` 一致。
// 这里不另收 `--id`：同一个动作有两种写法时，帮助文本要说两遍，而人多半会
// 猜错一遍。
func runPriceSetEnabled(args []string, enable bool) error {
	verb, done := "enable", "已启用"
	if !enable {
		verb, done = "disable", "已停用"
	}
	// 命令名按 enable 算出来：这两条命令共用这一个入口，写死一个会让其中一条
	// 在不支持 json 时报出兄弟命令的名字。拦在**函数体第一行**、连库之前——
	// 它会改 rules.enabled，做完再报不支持就成了一次静默的启停。
	if err := jsonUnsupported("price " + verb); err != nil {
		return err
	}

	fs := flag.NewFlagSet("price "+verb, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, priceUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return usageErr(fmt.Errorf(
			"%s 需要一个规则 ID，得到 %d 个参数：用法 larkaxctl price %s <规则 ID>",
			verb, fs.NArg(), verb))
	}
	id, err := strconv.ParseInt(strings.TrimSpace(fs.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		return usageErr(fmt.Errorf("%s 的规则 ID 必须是正整数，当前为 %q", verb, fs.Arg(0)))
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		// 只把 Enabled 那一栏写进请求：applyTo 只改请求里提到（非 nil）的字段，
		// 其余保持库里的值，所以这一条命令不会顺手改掉别的属性。
		enabled := enable
		if err := pricing.NewAdmin(db).Update(ctx, pricing.UpdateRequest{
			ID:      id,
			Enabled: &enabled,
		}); err != nil {
			return err
		}
		fmt.Printf("计价规则 %d %s。\n", id, done)
		return nil
	})
}

// parsePriceUpdate 解析 update 的参数。
//
// 与 parsePriceCreate 同一个形状，多带一个 ownerRef 返回值：--user 要连库才能
// 解析成用户 ID，而参数解析必须在连库之前完成。这里只做「有没有给、给了什么」。
func parsePriceUpdate(args []string) (pricing.UpdateRequest, string, error) {
	fs := flag.NewFlagSet("price update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, priceUsage) }
	id := fs.Int64("id", 0, "要改的规则 ID")
	scope := fs.String("scope", "", "作用域：platform、merchant、channel")
	name := fs.String("name", "", "规则名")
	owner := legacyStringFlag(fs, "user", "owner", "", "商家的用户 ID 或用户名；0 表示改成不挂商家")
	channelID := fs.Int64("channel", 0, "渠道 ID；0 表示改成不挂渠道")
	models := fs.String("models", "", "适用的规范模型名，逗号分隔，给空串表示全部")
	expression := fs.String("expr", "", "规则表达式源码")
	priority := fs.Int("priority", 0, "优先级，同一作用域里越大越优先")
	effectiveFrom := fs.String("effective-from", "", "生效时刻（RFC3339）")
	effectiveTo := fs.String("effective-to", "", "失效时刻（RFC3339）")
	clearEffectiveTo := fs.Bool("clear-effective-to", false, "把失效时刻清成不过期")
	// --enable / --disable 是**旧形态**（见 priceUsage 的「启停是独立的二级
	// 动词」那一节）：留着是为了不让已经在跑的脚本坏掉，新写的命令用
	// price enable / price disable。它们不出现在用法行里。
	enable := fs.Bool("enable", false, "启用这条规则（旧形态，新写法是 price enable）")
	disable := fs.Bool("disable", false, "停用这条规则（旧形态，新写法是 price disable）")
	if err := fs.Parse(args); err != nil {
		return pricing.UpdateRequest{}, "", err
	}
	if fs.NArg() > 0 {
		return pricing.UpdateRequest{}, "", fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的规则用 --id 指定", fs.Arg(0))
	}
	if *id <= 0 {
		fs.Usage()
		return pricing.UpdateRequest{}, "", errors.New("缺少 --id（要改的规则 ID）")
	}
	if *enable && *disable {
		fs.Usage()
		return pricing.UpdateRequest{}, "", errors.New("--enable 与 --disable 只能给一个")
	}
	if *clearEffectiveTo && *effectiveTo != "" {
		fs.Usage()
		return pricing.UpdateRequest{}, "", errors.New(
			"--clear-effective-to 与 --effective-to 只能给一个：前者是清成不过期，" +
				"后者是改成某个时刻")
	}
	if *channelID < 0 {
		return pricing.UpdateRequest{}, "", fmt.Errorf(
			"--channel %d 无效：渠道 ID 从 1 开始，0 表示不挂渠道", *channelID)
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	// --user 给了空串就拒，而不是当成 0：它多半是一个没展开的 shell 变量，
	// 而 0 在这里是有含义的（改成不挂商家）——静默地按 0 处理会把
	// 「我本来想挂某个商家」变成一条平台价，而那条规则照样匹配得上。
	if legacyGiven(given, "user", "owner") && strings.TrimSpace(*owner) == "" {
		fs.Usage()
		return pricing.UpdateRequest{}, "", errors.New(
			"--user 不能是空串（多半是变量没展开）：要改成不挂商家写 --user 0")
	}

	req := pricing.UpdateRequest{ID: *id}
	changes := 0
	ownerRef := ""
	if given["scope"] {
		req.Scope = scope
		changes++
	}
	if legacyGiven(given, "user", "owner") {
		ownerRef = *owner
		changes++
	}
	if given["channel"] {
		req.ChannelID = channelID
		changes++
	}
	if given["name"] {
		req.Name = name
		changes++
	}
	if given["models"] {
		list := splitOptionalList(*models)
		req.ModelPatterns = &list
		changes++
	}
	if given["expr"] {
		if strings.TrimSpace(*expression) == "" {
			fs.Usage()
			return pricing.UpdateRequest{}, "", errors.New(
				"--expr 不能为空（表上那一栏非空）：要停用这条规则用 price disable")
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
			fs.Usage()
			return pricing.UpdateRequest{}, "", errors.New(
				"--effective-from 不能为空（表上那一栏非空）：这一栏清不掉")
		}
		from, err := parseTimeFlag("--effective-from", *effectiveFrom)
		if err != nil {
			return pricing.UpdateRequest{}, "", err
		}
		req.EffectiveFrom = from
		changes++
	}
	if given["effective-to"] {
		if strings.TrimSpace(*effectiveTo) == "" {
			fs.Usage()
			return pricing.UpdateRequest{}, "", errors.New(
				"--effective-to 不能为空：要清成不过期用 --clear-effective-to")
		}
		to, err := parseTimeFlag("--effective-to", *effectiveTo)
		if err != nil {
			return pricing.UpdateRequest{}, "", err
		}
		req.EffectiveTo = to
		changes++
	}
	// 这两个看的是取值而不是「给没给」：--clear-effective-to=false 与
	// --enable=false 都是「不要这么做」，不是一次修改。
	if *clearEffectiveTo {
		req.ClearEffectiveTo = true
		changes++
	}
	switch {
	case *enable:
		enabled := true
		req.Enabled = &enabled
		changes++
	case *disable:
		enabled := false
		req.Enabled = &enabled
		changes++
	}
	if changes == 0 {
		fs.Usage()
		return pricing.UpdateRequest{}, "", errors.New(
			"update 至少要改一项：给出 --scope / --name / --user / --channel / " +
				"--models / --expr / --priority / --effective-from / --effective-to / " +
				"--clear-effective-to 之一（启用或停用规则用 price enable / price disable）")
	}

	return req, ownerRef, nil
}

// runPriceExplain 回答「这次请求按哪条规则计价」。
func runPriceExplain(args []string) error {
	fs := flag.NewFlagSet("price explain", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, priceUsage) }
	model := fs.String("model", "", "这次请求的规范模型名")
	owner := legacyStringFlag(fs, "user", "owner", "", "这次请求的商家（用户 ID 或用户名），留空表示未选定")
	channelID := fs.Int64("channel", 0, "这次请求的渠道 ID，留空表示未选定")
	at := fs.String("at", "", "问的是哪一刻（RFC3339），留空表示现在")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"explain 不接受位置参数，得到 %q：模型用 --model 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*model) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --model（这次请求的规范模型名）"))
	}
	if *channelID < 0 {
		return usageErr(fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID))
	}
	when, err := parseTimeFlag("--at", *at)
	if err != nil {
		return usageErr(err)
	}
	// 时刻在这里定下来再交给包：包在零值时会自己取当前时刻，而那样打出来的
	// 表头与真正用的那一刻是两个数（差几毫秒，但「问的是哪一刻」正是这张表
	// 要回答的问题之一，不该有两个答案）。
	if when == nil {
		now := time.Now()
		when = &now
	}

	cfg, err := config.LoadOps()
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

	q := pricing.ExplainQuery{CanonicalModel: *model, ChannelID: *channelID, At: *when}
	if strings.TrimSpace(*owner) != "" {
		ownerID, resolveErr := resolveOwnerID(ctx, db, *owner)
		if resolveErr != nil {
			return resolveErr
		}
		q.OwnerUserID = ownerID
	}

	rules, err := pricing.NewAdmin(db).Explain(ctx, q)
	if err != nil {
		return err
	}
	if jsonOutput() {
		return emitJSON(os.Stdout, priceExplainDoc(q, rules), nil)
	}
	printRuleExplain(os.Stdout, q, rules)
	return nil
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

	cfg, err := config.LoadOps()
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
	if jsonOutput() {
		return emitJSON(os.Stdout, priceListDoc(rules), nil)
	}
	printRuleList(os.Stdout, rules)
	return nil
}

// priceListDoc 组装 price list 的机器可读输出。
//
// 与 printRuleList 是同一份数据的另一条渲染路径：那一档把「生效/停用」、
// 「不过期」、商家栏的 `-` 铺成给人扫的表，这一档给布尔、null 与原始值。
func priceListDoc(rules []pricing.RuleInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(rules)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		entries = append(entries, ruleEntry(r))
	}
	doc["rules"] = entries
	return doc
}

// priceExplainDoc 组装 price explain 的机器可读输出。
//
// 它比 list 多两样：query（问的是哪一次请求）与每条规则上的 verdict。
// query 里那两个外键同样用 null 表示「这次没选定」，与请求路径上
// OwnerUserID / ChannelID 为 0 的含义一致（见 pricing.ExplainQuery）。
func priceExplainDoc(q pricing.ExplainQuery, rules []pricing.RuleInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["query"] = map[string]any{
		"canonical_model": q.CanonicalModel,
		"owner_user_id":   jsonID(q.OwnerUserID),
		"channel_id":      jsonID(q.ChannelID),
		"at":              jsonTime(q.At),
	}

	entries := make([]map[string]any, 0, len(rules))
	for i, r := range rules {
		entry := ruleEntry(r)
		entry["verdict"] = ruleVerdict(i)
		entries = append(entries, entry)
	}
	doc["rules"] = entries
	return doc
}

// ruleEntry 打一条计价规则。
//
// 它打的是**原始值**：表格那一档的打法（`生效/停用`、`不过期`、商家栏的
// `-`）一个都不进来。enabled 是布尔、effective_to 是 null 或时刻、
// quota_keys 是那一份原始清单而不是 declaredKeysLabel 渲染出来的那句
// 「（一项都没有）」——表达式编译不过时它是空数组，原因在 expr_error 上。
func ruleEntry(r pricing.RuleInfo) map[string]any {
	return map[string]any{
		"id": r.ID,
		// 0 表示不挂商家 / 不挂渠道（平台价），库里存的是 NULL。
		"scope":          r.Scope,
		"owner_user_id":  jsonID(r.OwnerUserID),
		"channel_id":     jsonID(r.ChannelID),
		"name":           r.Name,
		"model_patterns": jsonList(r.ModelPatterns),
		"priority":       r.Priority,
		"enabled":        r.Enabled,
		"effective_from": jsonTime(r.EffectiveFrom),
		"effective_to":   jsonTimePtr(r.EffectiveTo),
		"quota_keys":     jsonList(r.DeclaredKeys),
		"expr_error":     nullableError(r.ExprErr),
	}
}

// ruleVerdict 是 explain 里「裁决」那一栏在 JSON 里的取值。
//
// 给英文的两个词而不是表格那一档的「选中 / 被压住」：它是这个字段的取值域，
// 脚本按它分派（`select(.verdict == "selected")`），而中文渲染属于终端那一档。
// 语义与 printRuleExplain 的第一行同一条：排第一的就是会被计价的那条。
func ruleVerdict(index int) string {
	if index == 0 {
		return "selected"
	}
	return "shadowed"
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
		_, _ = fmt.Fprintln(w, "暂无计价规则。可使用 larkaxctl price create 添加。")
		return
	}
	printRuleTable(w, rules, nil)
}

// printRuleExplain 打印一次「按哪条规则计价」的裁决说明。
//
// 与 printRuleList 共用同一张表，只多一栏「裁决」：两处各拼一份格式串的话，
// 加一栏时改了一处、另一处还是老样子，而两张表说的是同一批规则。
func printRuleExplain(w io.Writer, q pricing.ExplainQuery, rules []pricing.RuleInfo) {
	_, _ = fmt.Fprintf(w, "模型 %s，商家 %s，渠道 %s，时刻 %s：\n",
		q.CanonicalModel, priceOwnerLabel(q.OwnerUserID), priceChannelLabel(q.ChannelID),
		q.At.Format(time.RFC3339))
	if len(rules) == 0 {
		// 走到这里说明写入路径没按约定返回 pipeline.ErrNoRule——那条路径对
		// 「一条都不适用」是报错而不是空清单（见 pricing.Admin.Explain）。
		// 这一行仍然要有：一张没有表头的空表与「命令没跑起来」长得一样。
		_, _ = fmt.Fprintln(w, "没有适用的计价规则。")
		return
	}
	printRuleTable(w, rules, func(i int) string {
		if i == 0 {
			return "选中"
		}
		return "被压住"
	})
	_, _ = fmt.Fprintln(w, "第一条就是这次请求会被计价的那条；排在它后面的每一条都在说明「为什么不是它」。")
}

// printRuleTable 打印一张规则表。
//
// verdictOf 非 nil 时多一栏「裁决」，按行号取那一栏的值。
func printRuleTable(w io.Writer, rules []pricing.RuleInfo, verdictOf func(int) string) {
	tw := newTableWriter(w)
	p := newPalette(w)
	headers := []string{"ID", "作用域", "商家", "渠道", "名称", "模型", "优先级", "状态", "生效自", "生效至", "单价键"}
	if verdictOf != nil {
		headers = append(headers, "裁决")
	}
	bolded := make([]string, len(headers))
	for i, h := range headers {
		bolded[i] = p.bold(h)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(bolded, "\t"))

	for i, r := range rules {
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s",
			p.bold(strconv.FormatInt(r.ID, 10)), r.Scope,
			priceOwnerLabel(r.OwnerUserID), priceChannelLabel(r.ChannelID),
			p.bold(r.Name), joinOrDash(r.ModelPatterns), r.Priority, enabledLabel(r),
			p.dim(r.EffectiveFrom.Format(time.RFC3339)), p.dim(effectiveToLabel(r.EffectiveTo)),
			declaredKeysLabel(r))
		if verdictOf != nil {
			line += "\t" + verdictOf(i)
		}
		_, _ = fmt.Fprintln(tw, line)
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
