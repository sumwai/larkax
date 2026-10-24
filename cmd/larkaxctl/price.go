package main

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/sumwai/larkax/internal/pricing"
)

// priceHelp 是 price 的两版帮助文本（见 helpText）。
var priceHelp = helpText{short: priceShortUsage, long: priceUsage}

// priceShortUsage 是 price 的短版：`-h` / `--help` 与参数写错时打的那一份。
const priceShortUsage = `larkaxctl price 管理计价规则。

用法：
  larkaxctl price add <规则名> --scope <…> --by <ID 或用户名>
                        (--expr '<表达式>' | --schedule-file <文件> | --in <单价> …)
  larkaxctl price list
  larkaxctl price show <规则 ID>
  larkaxctl price update <规则名 或 ID> --by <ID 或用户名> [与 add 相同的字段]
  larkaxctl price enable <规则 ID> | price disable <规则 ID>
  larkaxctl price explain --model <规范模型名> [--user <ID 或用户名>] [--at <…>]
  larkaxctl price tier add <规则名> --above <tokens> [单价旗标] --by <用户>
  larkaxctl price tier list <规则名> | price tier remove <规则名> <段号>
  larkaxctl price calendar import <文件> | price calendar sync [--year <年>] | price calendar list

作用域决定哪两栏 id 有效；--by 必填。单价按项给，默认每百万 token（--in 1.5 即
每百万 1.5）：五个计价项都会写进表达式，没给的项显式写 0。--unit token 可改单位。

更细的边界：larkaxctl help price。
`

// priceUsage 是 price 的长版：`larkaxctl help price` 与 `price help`。
const priceUsage = `larkaxctl price 管理计价规则。

用法：
  larkaxctl price add <规则名> --scope <platform|merchant|channel>
                        (--expr '<表达式>' | --schedule-file <文件> |
                         --in <单价> [--out <单价>] [...] [--unit mtok|token])
                        --by <用户 ID 或用户名>
                        [--user <用户 ID 或用户名>] [--channel <渠道 ID>]
                        [--models <规范模型名,...>] [--priority <优先级>]
                        [--effective-from <RFC3339>] [--effective-to <RFC3339>]
                        [--disabled]
  larkaxctl price list
  larkaxctl price show <规则 ID>
  larkaxctl price update <规则名 或 ID> --by <用户 ID 或用户名>
                        [--scope <platform|merchant|channel>] [--name <规则名>]
                        [--user <用户 ID 或用户名>] [--channel <渠道 ID>]
                        [--models <规范模型名,...>]
                        (--expr '<表达式>' | --schedule-file <文件> |
                         --in <单价> [...] [--unit mtok|token])
                        [--clear-schedule]
                        [--priority <优先级>] [--effective-from <RFC3339>]
                        [--effective-to <RFC3339>] [--clear-effective-to]
  larkaxctl price enable <规则 ID>
  larkaxctl price disable <规则 ID>
  larkaxctl price explain --model <规范模型名>
                          [--user <用户 ID 或用户名>] [--channel <渠道 ID>]
                          [--at <RFC3339>]
  larkaxctl price tier add <规则名 或 ID> --above <tokens>
                           [--in <单价>] [--out <单价>] [--cr <单价>]
                           [--cw <单价>] [--reasoning <单价>]
                           [--unit mtok|token] --by <用户 ID 或用户名>
  larkaxctl price tier list <规则名 或 ID>
  larkaxctl price tier remove <规则名 或 ID> <段号> --by <用户 ID 或用户名>
  larkaxctl price calendar import <文件>
        导入某一年的日历数据（分时计价要用它判断哪天是休息日，
        文件格式见 larkaxctl price calendar --help）
  larkaxctl price calendar sync [--year <年>]
        从 holiday-cn 拉取一年的放假安排并写库（缺省当年；还没发布就记
        pending，退出码 0）
  larkaxctl price calendar list
        列出导入过的年份、状态与天数

作用域决定哪两栏 id 有效：platform 两栏都留空；merchant 必须给 --user；
channel 必须给 --channel。写了读取路径不看的那一栏会被拒绝。

**规则名是 add 的第一个位置参数**，位置参数必须写在旗标前面：
  larkaxctl price add example --scope platform --by alice --in 1.5
--name 仍然认，与位置参数同值时取位置参数，两处写的不是同一个名字时报错。

update 的定位符也是位置参数，收规则名或规则 ID：纯数字按 ID，其余按名字查。
按名字查命中多条时报错并列出候选（ID、作用域、商家、渠道、优先级），此时改用 ID
或加 --scope 收窄。--id 仍然认，与位置参数只能给一个。

--by 必填：它记的是这一次是谁做的，落库记的是那个 users.id。add 把它写进
pricing_rules.created_by（这条价是谁定的），update 把它写进版本历史的
changed_by（这一版是谁改的，只在表达式真的变了、留了一版时才有这一行）。
两个都收 users.id 或用户名，与其余命令同一个解析路径。它**不算**一次修改：只给
--by 而不给别的字段时，这次调用没有要改的东西，命令会拒绝。

--models 留空表示适用于全部模型（兜底规则）；给了就按元素逐字匹配，
不支持通配符——带 * 或 ? 的名字会被拒绝。

单价按项给，单位默认是**每百万 token**（供应商报价单就是这么写的）：

  larkaxctl price add example --scope platform --by alice \
    --in 0.15 --out 0.6 --cr 0.003

五个旗标与五个计价项一一对应：--in 是输入（未命中缓存）、--out 是输出、
--cr 是输入·命中缓存、--cw 是输入·缓存写、--reasoning 是推理。

存库前换成每 token 的价（1.5 → 0.0000015），库里存的仍是表达式；
--unit token 表示给的就是每 token 的价。--expr、--schedule-file 与这组单价
旗标三选一：手写表达式、结构化定价表、逐项单价。

**这组旗标会把五个计价项全写进表达式**，没给的项显式写 0。写 0 是「免费」，
与「没写」不是一回事：上游报了哪一项，规则里就必须有哪一项的单价，少一项不是
少收，是这一笔的结算失败。--in 与 --out 至少要给一项——只给缓存价或推理价时
主价会按 0 算，那条规则在正常请求上白送。

表达式在写入前会编译一次，编译不过的命令直接失败。

结构化定价表用 --schedule-file 给一份 JSON：base 是兜底单价，overrides 按
weekdays / window / day_kind / holidays / min_context_tokens 分行改写单价，
跨维度的两条用 position 裁决（大的优先）。--expr 与 --schedule-file 二选一：
给了表时表达式由它编译得出，读取与求值仍然走表达式那一份。

门槛（min_context_tokens）的口径是**达到即命中**（>=）：输入上下文（含缓存
读写）等于该值时就走那一档。带阶梯的规则不必手改整张表，用 price tier 增删
档位即可；多档逐条 add，门槛高的自动先命中，不用给 position，细节见
larkaxctl price tier help。

表里有 day_kind 或 holidays 时，实例必须接着日历，否则保存会被拒绝：日历
没有数据时整笔计价失败，不回落到底价。price show 对带表的规则打那张表，
对手写表达式的规则打源码。

带表的规则要改成手写表达式，必须同时给 --clear-schedule：库里存的是表编译
出来的那一份，直接换源码会让表与实际价格分家。

计价项一共五个：prompt、completion、cache_read、cache_write、reasoning。
上游报了哪一项，规则里就必须有哪一项的单价——少一项不是少收，是这一笔的结算
失败：非流式请求调用方拿到 500，流式请求只把「用量里有规则未定价的计量项」
写进服务端日志。用单价旗标写的规则五项齐全，不受这一条影响；手写表达式与
定价表仍可能漏项，写完会提示漏了哪几项。

create 与 update 写成功之后会提示这条规则漏了哪几项。提示看的是规则实际
定过价的键，与漏掉时结算报的那一项对得上。它只是提示：写入已经完成，命令
照样成功退出，-q 会连同它一起压掉。

少一项的后果分两条路：非流式请求结算失败时调用方拿到 500；流式请求的流
已经推出去，那次失败只落在服务端日志里。

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
改属性的命令不带状态开关。

**旧名仍可用**：price create 等同于 price add，--name 与 --id 仍可作为规则名与
定位符，--prompt / --completion / --cache-read / --cache-write 分别是
--in / --out / --cr / --cw 的旧写法，--enable 与 --disable 是启停的旧形态，
--owner 等同于 --user。它们保留是为了不让已经在跑的脚本与部署流程坏掉，行为与
新名字逐字相同；新写的命令请用新名字。

explain 回答「这次请求按哪条规则计价」：它把**全部适用**的规则按裁决次序
列出来，第一条就是会被选中的那条，排在它后面的每一条都在说明它落选的原因。
--at 用来问过去某一刻（对账时问「这一笔当时按哪条算的」），留空表示
现在。--user 与 --channel 是这次请求的商家与渠道，都不给时按平台级规则算。
一条适用的都没有时非零退出，与请求路径上的处置一致（那是拒绝服务，不是
「按 0 元计」）。

# 别名

下面几个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -u <用户 ID 或用户名>   同 --user，add / update / explain 都认
  --owner                 同 --user
  --from <RFC3339>        同 --effective-from
  --to <RFC3339>          同 --effective-to
  --no-expire             同 --clear-effective-to
  --prompt <单价>         同 --in
  --completion <单价>     同 --out
  --cache-read <单价>     同 --cr
  --cache-write <单价>    同 --cw
  --name <规则名>         同第一个位置参数
  --id <规则 ID>          同 update 的位置参数

**--channel 与 --effective-to 不开简写**：-c 在别的命令行里既可能是 channel
也可能是 config、count，挑错作用域会把一条规则挂到另一个渠道上；--effective-to
是边界参数，写短了容易看错。

--from / --to 在这一组命令上表示**生效区间的起止**。ledger verify 上的
--from / --to 是它自己的规范名（查账区间），与这里不冲突：两条命令不重名，
也没有一条命令同时认这两种含义。

更细的约定见 docs/cli-reference.md 的「price」一节。
`

// priceActions 是 price 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var priceActions = []string{"add", "list", "show", "update", "enable", "disable", "explain", "tier", "calendar"}

func runPrice(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, priceShortUsage)
		return usageErr(errors.New("price 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add", "create":
		// create 是 add 的旧名（见 priceUsage 的「旧名仍可用」）。两个名字走同一条
		// 路径，命令自己的名字按 add 报，免得在不支持 json 时说出旧名字。
		return runPriceCreate(rest)
	case "list":
		return runPriceList(rest)
	case "show":
		return runPriceShow(rest)
	case "update":
		return runPriceUpdate(rest)
	case "enable":
		return runPriceSetEnabled(rest, true)
	case "disable":
		return runPriceSetEnabled(rest, false)
	case "explain":
		return runPriceExplain(rest)
	case "tier":
		// 阶梯档位（按输入上下文分档）是定价表里的一行覆盖，增删走独立的
		// 二级命令组：手改整张 JSON 才能调一档价，改错一处就是整表不成立。
		return runPriceTier(rest)
	case "calendar":
		// 分时计价的日历原先是一个顶层命令组（larkaxctl calendar），#475 把它
		// 收进 price：这些数据是计价规则的输入，离开规则没有别的用处。
		return runPriceCalendar(rest)
	case "-h", "--help":
		printUsage(os.Stdout, priceShortUsage)
		return nil
	default:
		printUsage(os.Stderr, priceShortUsage)
		return usageErr(fmt.Errorf("未知的 price 子命令 %q", action))
	}
}

func runPriceCreate(args []string) error {
	// 命令名写正式名 add：jsonUnsupported 那句会把命令名打给人看，而 create
	// 是旧写法（见 priceUsage 的「旧名仍可用」）。
	if err := jsonUnsupported("price add"); err != nil {
		return err
	}

	req, refs, err := parsePriceCreate(args)
	if err != nil {
		return usageErr(err)
	}
	if refs.scheduleFile != "" {
		sched, schedErr := readScheduleFile(refs.scheduleFile)
		if schedErr != nil {
			return schedErr
		}
		req.Schedule = sched
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

	// 先解析 --by 再解析 --user：前者必填，缺了上面就报错了；而已写入的
	// created_by 认的是一个真的存在的 users.id（那一列上有外键）。
	actor, err := auth.New(db).ResolveUser(ctx, refs.by)
	if err != nil {
		return fmt.Errorf("解析 --by 失败: %w", err)
	}
	req.CreatedBy = actor.ID

	if refs.owner != "" {
		ownerID, resolveErr := resolveOwnerID(ctx, db, refs.owner)
		if resolveErr != nil {
			return resolveErr
		}
		req.OwnerUserID = ownerID
	}

	id, err := pricing.NewAdmin(db).Create(ctx, req)
	if err != nil {
		return err
	}
	printCreatedRule(humanStdout(), humanStderr(), id, req)
	printPriceCoverageHint(humanStderr(), req.Expr, req.Schedule)
	return nil
}

// priceRefs 是 price add / update 上那几个「连库才解析得成」的旗标原文。
//
// 两个都要连库才解析得成 users.id，而参数解析必须在连库之前完成，所以这里
// 只收原文，解析留给命令那一层（见 runPriceCreate / runPriceUpdate）。
type priceRefs struct {
	// owner 是 --user：这条规则挂在哪个商家名下。空串表示没给这一项。
	owner string
	// by 是 --by：这一次是谁做的。必填，所以走完参数解析之后它一定非空。
	by string
	// scheduleFile 是 --schedule-file 的路径原文。空串表示没给。
	//
	// 文件在连库之前读：读不出来时不该先建立一次连接，而失败信息里要带
	// 文件路径，留给命令那一层拼。
	scheduleFile string
	// locator 是 update 上「改哪一条」的原文：位置参数或 --id，空串表示没给。
	//
	// 两条路都要连库才定得下来——纯数字按 ID，其余要读规则表按名字查（见
	// resolvePriceRuleLocator）。参数解析那一层只判「给没给、给了几个」。
	locator string
}

// parsePriceCreate 解析 add 的参数。
//
// 它多带一个 priceRefs 返回值：--user 与 --by 都要连库才能解析成用户 ID，
// 而参数解析必须在连库之前完成。所以这里只做「有没有给、给了什么」，
// 解析留给调用方。
func parsePriceCreate(args []string) (pricing.CreateRequest, priceRefs, error) {
	// 规则名是第一个位置参数，且必须写在旗标前面：flag 包遇到第一个非旗标词就
	// 停止解析，写在后面的一整段都会落进 NArg（下面按「多给了参数」报）。
	operand, rest, takeErr := takeLeadingOperand(args, priceRuleNameOperand)
	if takeErr != nil {
		return pricing.CreateRequest{}, priceRefs{}, takeErr
	}
	args = rest

	fs := newFlagSet("price add", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceShortUsage) }
	scope := fs.String("scope", "", "")
	name := fs.String("name", "", "")
	expression := fs.String("expr", "", "")
	scheduleFile := fs.String("schedule-file", "", "")
	owner := legacyStringFlag(fs, "user", "owner", "")
	stringAlias(fs, owner, "u")
	channelID := fs.Int64("channel", 0, "渠道 ID，channel 作用域必填")
	models := fs.String("models", "", "")
	priority := fs.Int("priority", 0, "")
	effectiveFrom := fs.String("effective-from", "", "")
	stringAlias(fs, effectiveFrom, "from")
	effectiveTo := fs.String("effective-to", "", "")
	stringAlias(fs, effectiveTo, "to")
	disabled := fs.Bool("disabled", false, "")
	by := fs.String("by", "", "")
	slots := newPriceSlotFlags(fs)
	if err := fs.Parse(args); err != nil {
		return pricing.CreateRequest{}, priceRefs{}, err
	}
	if fs.NArg() > 0 {
		return pricing.CreateRequest{}, priceRefs{}, fmt.Errorf(
			"add 不接受多余的参数，得到 %q：规则名要写成第一个位置参数，排在旗标前面", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if strings.TrimSpace(*scope) == "" {
		return pricing.CreateRequest{}, priceRefs{}, errors.New("缺少 --scope（platform / merchant / channel）")
	}
	ruleName, nameErr := resolveOperand(operand, *name, given["name"], priceRuleNameOperand)
	if nameErr != nil {
		return pricing.CreateRequest{}, priceRefs{}, nameErr
	}
	// 规则名与模型名单进 price list / explain 的表格：控制字符在连库之前
	// 拒掉（判据在 pricing 的写入路径，见 normalizeName / normalizePatterns）。
	if err := checkDisplayText("--name（规则名）", ruleName); err != nil {
		return pricing.CreateRequest{}, priceRefs{}, err
	}
	if err := checkDisplayList("--models", splitOptionalList(*models)); err != nil {
		return pricing.CreateRequest{}, priceRefs{}, err
	}
	exprGiven := strings.TrimSpace(*expression) != ""
	scheduleGiven := strings.TrimSpace(*scheduleFile) != ""
	slotsGiven := slots.anyGiven(given)
	if exprGiven && scheduleGiven {
		return pricing.CreateRequest{}, priceRefs{}, errors.New(
			"--expr 与 --schedule-file 只能给一种：给了定价表时表达式由它编译得出")
	}
	if slotsGiven && (exprGiven || scheduleGiven) {
		return pricing.CreateRequest{}, priceRefs{}, errors.New(
			"--expr、--schedule-file 与单价旗标（--in 等）只能给一种：" +
				"手写表达式、结构化定价表、逐项单价三者选一")
	}
	if slots.unitGiven(given) && !slotsGiven {
		return pricing.CreateRequest{}, priceRefs{}, errors.New(
			"--unit 只与单价旗标一起用：--expr 与 --schedule-file 自带单位")
	}
	if !exprGiven && !scheduleGiven && !slotsGiven {
		return pricing.CreateRequest{}, priceRefs{}, errors.New(
			"缺少 --expr、--schedule-file 或单价旗标：手写表达式、结构化定价表、" +
				"逐项单价三者选一")
	}

	// 三种给法在这里收拢成一条表达式：下面的写入路径只认 Expr 这一栏。
	expressionText := *expression
	if slotsGiven {
		built, buildErr := slots.expression(given)
		if buildErr != nil {
			return pricing.CreateRequest{}, priceRefs{}, buildErr
		}
		expressionText = built
	}
	if strings.TrimSpace(*by) == "" {
		return pricing.CreateRequest{}, priceRefs{}, errors.New("缺少 --by：这条命令要记下这条价是谁定的")
	}
	if *channelID < 0 {
		return pricing.CreateRequest{}, priceRefs{}, fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID)
	}
	from, err := parseTimeFlag("--effective-from", *effectiveFrom)
	if err != nil {
		return pricing.CreateRequest{}, priceRefs{}, err
	}
	to, err := parseTimeFlag("--effective-to", *effectiveTo)
	if err != nil {
		return pricing.CreateRequest{}, priceRefs{}, err
	}

	req := pricing.CreateRequest{
		Scope:         *scope,
		ChannelID:     *channelID,
		Name:          ruleName,
		ModelPatterns: splitOptionalList(*models),
		Expr:          expressionText,
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
	return req, priceRefs{owner: *owner, by: *by, scheduleFile: *scheduleFile}, nil
}

// priceLocatorOperand 是 price update 的位置参数：规则名或规则 ID。
var priceLocatorOperand = operandSpec{
	what:  "定位符",
	flag:  "id",
	usage: "写成第一个位置参数（larkaxctl price update <规则名 或 ID> …）",
}

// priceRuleNameOperand 是 price add 的位置参数：规则名。
//
// 与 user / plan / model 的 add 同一个形状（见 operand.go）：第一个位置参数是
// 正式写法，--name 是旧写法。
var priceRuleNameOperand = operandSpec{
	what:  "规则名",
	flag:  "name",
	usage: "写成第一个位置参数（larkaxctl price add <规则名> …）",
}

func runPriceUpdate(args []string) error {
	if err := jsonUnsupported("price update"); err != nil {
		return err
	}

	req, refs, err := parsePriceUpdate(args)
	if err != nil {
		return usageErr(err)
	}
	if refs.scheduleFile != "" {
		sched, schedErr := readScheduleFile(refs.scheduleFile)
		if schedErr != nil {
			return schedErr
		}
		req.Schedule = sched
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

	// --by 记在版本历史那一行上（pricing_rule_versions.changed_by），而那一列
	// 认的是一个真的存在的 users.id。解析放在前面：标识写错了要在改库之前
	// 报出来。
	actor, err := auth.New(db).ResolveUser(ctx, refs.by)
	if err != nil {
		return fmt.Errorf("解析 --by 失败: %w", err)
	}
	req.ChangedBy = actor.ID

	if refs.owner != "" {
		ownerID, resolveErr := resolveOwnerID(ctx, db, refs.owner)
		if resolveErr != nil {
			return resolveErr
		}
		req.OwnerUserID = &ownerID
	}

	// 定位符要连库才解析得成 ID（按名字查那一路要读规则表），所以留到这里。
	// 放在 --by 之后：写错的操作人要报在「改哪一条」之前，那是同一类错误里更
	// 基本的一件。
	if refs.locator != "" {
		id, resolveErr := resolvePriceRuleLocator(ctx, db, refs.locator, req.Scope)
		if resolveErr != nil {
			return usageErr(resolveErr)
		}
		req.ID = id
	}

	if err := pricing.NewAdmin(db).Update(ctx, req); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(humanStderr(), "已更新计价规则 %d。\n", req.ID)
	// 只在这次真的写了计价时提示：只改优先级、名字或只清表的那次更新没有
	// 新的单价可看，提示会变成一句与本次操作无关的话。
	expression := ""
	if req.Expr != nil {
		expression = *req.Expr
	}
	printPriceCoverageHint(humanStderr(), expression, req.Schedule)
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

	fs := newFlagSet("price "+verb, flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
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
		_, _ = fmt.Fprintf(humanStderr(), "计价规则 %d %s。\n", id, done)
		return nil
	})
}

// parsePriceUpdate 解析 update 的参数。
//
// 这几项各有别名，判「这次给没给」时要把别名一起查（见 flagGiven）。
var (
	effectiveFromFlagNames = []string{"effective-from", "from"}
	effectiveToFlagNames   = []string{"effective-to", "to"}
)

// 与 parsePriceCreate 同一个形状，多带的 priceRefs 里放着两个用户标识的原文：
// --user 与 --by 都要连库才能解析成用户 ID，而参数解析必须在连库之前完成。
// 这里只做「有没有给、给了什么」。
func parsePriceUpdate(args []string) (pricing.UpdateRequest, priceRefs, error) {
	// 定位符是第一个位置参数，规则名或规则 ID。与 add 的规则名同一个形状：
	// flag 包遇到第一个非旗标词就停，所以它必须写在旗标前面。
	operand, rest, takeErr := takeLeadingOperand(args, priceLocatorOperand)
	if takeErr != nil {
		return pricing.UpdateRequest{}, priceRefs{}, takeErr
	}
	args = rest

	fs := newFlagSet("price update", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceShortUsage) }
	id := fs.Int64("id", 0, "要改的规则 ID（与位置参数只能给一个）")
	scope := fs.String("scope", "", "")
	name := fs.String("name", "", "")
	owner := legacyStringFlag(fs, "user", "owner", "")
	stringAlias(fs, owner, "u")
	channelID := fs.Int64("channel", 0, "渠道 ID；0 表示改成不挂渠道")
	models := fs.String("models", "", "")
	expression := fs.String("expr", "", "")
	scheduleFile := fs.String("schedule-file", "", "")
	clearSchedule := fs.Bool("clear-schedule", false, "")
	priority := fs.Int("priority", 0, "")
	effectiveFrom := fs.String("effective-from", "", "")
	stringAlias(fs, effectiveFrom, "from")
	effectiveTo := fs.String("effective-to", "", "")
	stringAlias(fs, effectiveTo, "to")
	clearEffectiveTo := fs.Bool("clear-effective-to", false, "")
	boolAlias(fs, clearEffectiveTo, "no-expire")
	by := fs.String("by", "", "")
	// --enable / --disable 是**旧形态**（见 priceUsage 的「启停是独立的二级
	// 动词」那一节）：留着是为了不让已经在跑的脚本坏掉，新写的命令用
	// price enable / price disable。它们不出现在用法行里。
	enable := fs.Bool("enable", false, "")
	disable := fs.Bool("disable", false, "")
	slots := newPriceSlotFlags(fs)
	if err := fs.Parse(args); err != nil {
		return pricing.UpdateRequest{}, priceRefs{}, err
	}
	if fs.NArg() > 0 {
		return pricing.UpdateRequest{}, priceRefs{}, fmt.Errorf(
			"update 不接受多余的参数，得到 %q：要改的规则写成第一个位置参数（规则名或 ID），排在旗标前面",
			fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	// 定位符只能给一处：位置参数与 --id 同时给时，两个值不同就是一次
	// 「改错了规则」的风险，而静默取其中一个会把它变成一次成功的写入。
	//
	// 定位符**不在这里解析成 ID**：按名字查那一路要读规则表（见
	// resolvePriceRuleLocator），而参数解析不连库。req.ID 因此留空，由
	// runPriceUpdate 填——这里只保证它非空，并把 --id 与位置参数的冲突判掉。
	locator := operand
	if given["id"] {
		if *id <= 0 {
			return pricing.UpdateRequest{}, priceRefs{}, fmt.Errorf(
				"--id %d 无效：规则 ID 是正整数；按名字定位就把它写成位置参数", *id)
		}
		if locator != "" && locator != strconv.FormatInt(*id, 10) {
			return pricing.UpdateRequest{}, priceRefs{}, fmt.Errorf(
				"要改的规则给了两个不同的值：位置参数 %q，--id %d；只能写一处", locator, *id)
		}
		locator = strconv.FormatInt(*id, 10)
	}
	if locator == "" {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"缺少要改的规则：写成第一个位置参数（规则名或 ID），或改用 --id")
	}
	// 纯数字按 ID 在这里就定下来：它不需要连库，而 short_flags_test 那一组比的是
	// 「两种写法解析出同一个请求」，ID 留在 refs 里会让两条解析结果都是零值 ID。
	// 名字那一路仍留给 runPriceUpdate。
	numericID := int64(0)
	if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(locator), 10, 64); parseErr == nil && parsed > 0 {
		numericID = parsed
	}
	if strings.TrimSpace(*by) == "" {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New("缺少 --by：这条命令要记下是谁改的这条价")
	}
	if *enable && *disable {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New("--enable 与 --disable 只能给一个")
	}
	if *clearEffectiveTo && *effectiveTo != "" {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"--clear-effective-to 与 --effective-to 只能给一个：前者是清成不过期，" +
				"后者是改成某个时刻")
	}
	if *clearSchedule && strings.TrimSpace(*scheduleFile) != "" {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"--clear-schedule 与 --schedule-file 只能给一个：前者是清掉已有的表，" +
				"后者是换成另一张表")
	}
	if *channelID < 0 {
		return pricing.UpdateRequest{}, priceRefs{}, fmt.Errorf(
			"--channel %d 无效：渠道 ID 从 1 开始，0 表示不挂渠道", *channelID)
	}

	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if given["expr"] && given["schedule-file"] {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"--expr 与 --schedule-file 只能给一种：给了定价表时表达式由它编译得出")
	}
	slotsGiven := slots.anyGiven(given)
	if slotsGiven && (given["expr"] || given["schedule-file"]) {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"--expr、--schedule-file 与单价旗标（--in 等）只能给一种：" +
				"手写表达式、结构化定价表、逐项单价三者选一")
	}
	if slots.unitGiven(given) && !slotsGiven {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"--unit 只与单价旗标一起用：--expr 与 --schedule-file 自带单位")
	}

	// --user 给了空串就拒，而不是当成 0：它多半是一个没展开的 shell 变量，
	// 而 0 在这里是有含义的（改成不挂商家）——静默地按 0 处理会把
	// 「我本来想挂某个商家」变成一条平台价，而那条规则照样匹配得上。
	if flagGiven(given, userFlagNames...) && strings.TrimSpace(*owner) == "" {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"--user 不能是空串（多半是变量没展开）：要改成不挂商家写 --user 0")
	}

	// ID 只在定位符是纯数字时定得下来；按名字那一路要读规则表（见
	// resolvePriceRuleLocator），留给 runPriceUpdate。
	req := pricing.UpdateRequest{ID: numericID}
	changes := 0
	ownerRef := ""
	if given["scope"] {
		req.Scope = scope
		changes++
	}
	if flagGiven(given, userFlagNames...) {
		ownerRef = *owner
		changes++
	}
	if given["channel"] {
		req.ChannelID = channelID
		changes++
	}
	if given["name"] {
		if err := checkDisplayText("--name（规则名）", strings.TrimSpace(*name)); err != nil {
			return pricing.UpdateRequest{}, priceRefs{}, err
		}
		req.Name = name
		changes++
	}
	if given["models"] {
		list := splitOptionalList(*models)
		if err := checkDisplayList("--models", list); err != nil {
			return pricing.UpdateRequest{}, priceRefs{}, err
		}
		req.ModelPatterns = &list
		changes++
	}
	if given["expr"] {
		if strings.TrimSpace(*expression) == "" {
			return pricing.UpdateRequest{}, priceRefs{}, errors.New(
				"--expr 不能为空（表上那一栏非空）：要停用这条规则用 price disable")
		}
		req.Expr = expression
		changes++
	}
	if given["schedule-file"] {
		if strings.TrimSpace(*scheduleFile) == "" {
			return pricing.UpdateRequest{}, priceRefs{}, errors.New(
				"--schedule-file 不能为空：要给定价表就写文件路径")
		}
		changes++
	}
	if slotsGiven {
		built, buildErr := slots.expression(given)
		if buildErr != nil {
			return pricing.UpdateRequest{}, priceRefs{}, buildErr
		}
		req.Expr = &built
		changes++
	}
	// 清表只动结构化那一栏，表达式留在库里（它本来就是表编译出来的结果）。
	if *clearSchedule {
		req.ClearSchedule = true
		changes++
	}
	if given["priority"] {
		req.Priority = priority
		changes++
	}
	// 生效下界清不掉（那一栏非空），所以给了空串要当场拒：静默地当成
	// 「不改这一项」会让一次只想改下界的调用变成「什么都没改」，
	// 而它看起来像一次成功的修改。
	if flagGiven(given, effectiveFromFlagNames...) {
		if strings.TrimSpace(*effectiveFrom) == "" {
			return pricing.UpdateRequest{}, priceRefs{}, errors.New(
				"--effective-from 不能为空（表上那一栏非空）：这一栏清不掉")
		}
		from, err := parseTimeFlag("--effective-from", *effectiveFrom)
		if err != nil {
			return pricing.UpdateRequest{}, priceRefs{}, err
		}
		req.EffectiveFrom = from
		changes++
	}
	if flagGiven(given, effectiveToFlagNames...) {
		if strings.TrimSpace(*effectiveTo) == "" {
			return pricing.UpdateRequest{}, priceRefs{}, errors.New(
				"--effective-to 不能为空：要清成不过期用 --clear-effective-to")
		}
		to, err := parseTimeFlag("--effective-to", *effectiveTo)
		if err != nil {
			return pricing.UpdateRequest{}, priceRefs{}, err
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
	// --by 不算「改了一项」：它记的是这一次是谁做的，而规则本身一个字段都没动。
	// 把它算进去的话，`price update --id 5 --by ops` 会静默地变成一次
	// 「改完了」的成功调用，而库里什么都没变。
	if changes == 0 {
		return pricing.UpdateRequest{}, priceRefs{}, errors.New(
			"update 至少要改一项：给出 --scope / --name / --user / --channel / " +
				"--models / --expr / --in 等单价旗标 / --priority / --effective-from / " +
				"--effective-to / --clear-effective-to 之一" +
				"（启用或停用规则用 price enable / price disable）")
	}

	return req, priceRefs{
		owner: ownerRef, by: *by, scheduleFile: *scheduleFile, locator: locator,
	}, nil
}

// resolvePriceRuleLocator 把 update 的定位符解析成规则 ID。
//
// 纯数字按 ID，其余按名字查：规则名是给人看的（`ds-flash-2026q4`），而 ID 是
// 列表里第一栏，两种写法在日常里都顺手，所以两种都认。判据是「整串是正整数」，
// 不按「像不像名字」猜——名字允许是纯数字，那种名字要定位就得写 --id。
//
// # 按名字查命中多条时报错，不猜
//
// 规则名在库里不唯一（pricing_rules 上没有唯一约束：同一个名字可以出现在不同
// 作用域、不同商家、不同优先级上）。挑一条改等于在三条同名规则里随便选一条
// 改价，而命令照样成功退出。所以命中多条时列出候选并要求改用 ID。
//
// scope 是这次给了 --scope 的取值（没给是空串）：给了就只在那一个作用域里找，
// 因为「把 merchant 的那条改成 platform」正是同时给 --scope 与名字的用法。
func resolvePriceRuleLocator(ctx context.Context, db *sql.DB, locator string, scope *string) (int64, error) {
	locator = strings.TrimSpace(locator)
	if id, err := strconv.ParseInt(locator, 10, 64); err == nil {
		if id <= 0 {
			return 0, fmt.Errorf("规则 ID 必须是正整数，当前为 %q", locator)
		}
		return id, nil
	}

	rules, err := pricing.NewAdmin(db).List(ctx)
	if err != nil {
		return 0, fmt.Errorf("按名字查计价规则失败: %w", err)
	}
	wantScope := ""
	if scope != nil {
		wantScope = strings.TrimSpace(*scope)
	}
	matches := make([]pricing.RuleInfo, 0, 1)
	for _, rule := range rules {
		if rule.Name != locator {
			continue
		}
		if wantScope != "" && rule.Scope != wantScope {
			continue
		}
		matches = append(matches, rule)
	}

	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		if wantScope != "" {
			return 0, fmt.Errorf("作用域 %s 里没有叫 %q 的计价规则", wantScope, locator)
		}
		return 0, fmt.Errorf("没有叫 %q 的计价规则：用 price list 看现有的规则名", locator)
	}

	lines := make([]string, 0, len(matches))
	for _, rule := range matches {
		lines = append(lines, fmt.Sprintf("  ID=%d 作用域=%s 商家=%s 渠道=%s 优先级=%d",
			rule.ID, rule.Scope, priceOwnerLabel(rule.OwnerUserID),
			priceChannelLabel(rule.ChannelID), rule.Priority))
	}
	return 0, fmt.Errorf(
		"有 %d 条计价规则叫 %q，改用 ID 指定要改哪一条（或加 --scope 收窄）：\n%s",
		len(matches), locator, strings.Join(lines, "\n"))
}

// runPriceExplain 回答「这次请求按哪条规则计价」。
func runPriceExplain(args []string) error {
	fs := newFlagSet("price explain", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceShortUsage) }
	model := fs.String("model", "", "")
	stringAlias(fs, model, "m")
	owner := legacyStringFlag(fs, "user", "owner", "")
	stringAlias(fs, owner, "u")
	channelID := fs.Int64("channel", 0, "这次请求的渠道 ID，留空表示未选定")
	at := fs.String("at", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"explain 不接受位置参数，得到 %q：模型用 --model 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*model) == "" {
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
	printRuleExplain(humanStdout(), q, rules)
	return nil
}

func runPriceList(args []string) error {
	fs := newFlagSet("price list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceShortUsage) }
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
	printRuleList(humanStdout(), rules)
	return nil
}

// readScheduleFile 读一份结构化定价表。
//
// 读文件、解析与校验都在连库之前：文件不成立时不该先建立一次连接，而报错
// 要带路径——不然一个相对路径写错，看到的只是一句「JSON 解析失败」。
// 校验在这里多跑一遍是有意的：写入路径还会再跑一次，但只有这一次能把
// 「第几条覆盖」的报错和文件名放在同一句里。
func readScheduleFile(path string) (*pricing.Schedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读定价表文件 %s 失败: %w", path, err)
	}
	schedule, err := pricing.ParseSchedule(data)
	if err != nil {
		return nil, fmt.Errorf("定价表文件 %s: %w", path, err)
	}
	if _, err := schedule.Validate(); err != nil {
		return nil, fmt.Errorf("定价表文件 %s: %w", path, err)
	}
	return schedule, nil
}

// runPriceShow 打一条规则的定价依据。
//
// 带结构化定价表的规则打那张表：它是可枚举的那一份，也是运维要改的那一份。
// 手写表达式的规则打源码——没有表可打，源码就是全部。
func runPriceShow(args []string) error {
	fs := newFlagSet("price show", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		return usageErr(fmt.Errorf(
			"show 需要一个规则 ID，得到 %d 个参数：用法 larkaxctl price show <规则 ID>", fs.NArg()))
	}
	id, err := strconv.ParseInt(strings.TrimSpace(fs.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		return usageErr(fmt.Errorf("show 的规则 ID 必须是正整数，当前为 %q", fs.Arg(0)))
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := pricing.NewAdmin(db).Get(ctx, id)
		if err != nil {
			return err
		}
		if jsonOutput() {
			return emitJSON(os.Stdout, priceShowDoc(info), nil)
		}
		printRuleShow(os.Stdout, info)
		return nil
	})
}

// priceShowDoc 组装 price show 的机器可读输出。
//
// expr 与 schedule 都给：表是给人改的那一份，表达式是求值真正用的那一份，
// 让脚本能同时核对两者（不变式是 schedule 非 null 时 expr 等于它编译的结果）。
func priceShowDoc(r pricing.RuleInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["rule"] = ruleEntry(r)
	doc["expr"] = r.Expr
	doc["schedule"] = r.Schedule
	doc["schedule_error"] = nullableError(r.ScheduleErr)
	return doc
}

// printRuleShow 打印一条规则的定价依据。
func printRuleShow(w io.Writer, r pricing.RuleInfo) {
	_, _ = fmt.Fprintf(w, "规则 %d：%s（作用域=%s，优先级=%d）\n",
		r.ID, r.Name, r.Scope, r.Priority)
	// 单条读取（Get）遇到坏表会当场报错，所以这一支今天走不到；留着是为了
	// 与清单那一档对齐——将来若 show 改走不报错的读法，它不该把坏表渲染成
	// «手写表达式»。
	if r.ScheduleErr != nil {
		_, _ = fmt.Fprintf(w, "结构化定价表解不开：%v\n", r.ScheduleErr)
	}
	if r.Schedule == nil {
		_, _ = fmt.Fprintln(w, "表达式：")
		_, _ = fmt.Fprintln(w, r.Expr)
		return
	}
	data, err := json.MarshalIndent(r.Schedule, "", "  ")
	if err != nil {
		// 表里只有数值、字符串与布尔，编码不会失败；真失败也不该让
		// 这条命令打半张表，于是把原因照原样打出来。
		_, _ = fmt.Fprintf(w, "结构化定价表（编码失败：%v）：\n", err)
		return
	}
	_, _ = fmt.Fprintln(w, "结构化定价表：")
	_, _ = fmt.Fprintln(w, string(data))
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
		"schedule_error": nullableError(r.ScheduleErr),
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

// priceCoverageHint 拼出「这条规则漏了哪几个计价项」的提示。
//
// subject 说的是被提示的是哪一份单价（一条表达式规则，还是一张定价表的
// base）；missing 是漏掉的计价项，调用方拿到的顺序已经按 billing.TokenSlots
// 排好。一个都不缺时返回空串：没有可提示的事，就不该有这一行。
func priceCoverageHint(subject string, missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("提示：%s没有覆盖 %d 个计价项的单价：%s。\n"+
		"上游报了这几项用量时，这一笔的结算会失败：非流式请求，调用方拿到 500；\n"+
		"流式请求的流已经推出去，失败只落在服务端日志里（「用量里有规则未定价的计量项」）。\n"+
		"用不到就把单价显式写成 0——写 0 是「免费」，与「没写」不是一回事。\n",
		subject, len(missing), strings.Join(missing, "、"))
}

// printPriceCoverageHint 在写成功之后提示这条规则漏了哪几个 token 计价项。
//
// 只提示，不拦写入：调用时写入已经完成，这里出任何问题都只是少打一段字。
// w 由调用方传 humanStderr()，所以 -q 下自动静默。
//
// expression 与 schedule 至多一个非空；都为空表示这次没有写入新的计价
// （例如 price update 只改了优先级），不提示。
func printPriceCoverageHint(w io.Writer, expression string, schedule *pricing.Schedule) {
	if schedule != nil {
		missing, err := pricing.UncoveredScheduleTokenItems(schedule)
		if err != nil {
			// 表在写入前校验过；走到这里说明两次校验之间出现了不一致。
			// 写入本身成功了，不能因为一句提示把它变成失败。
			return
		}
		// subject 末尾的空格落在 base 与「没有覆盖」之间：拉丁词挨着汉字
		// 挤在一起不好读，也过不了手册那条中英间距的判据。
		_, _ = io.WriteString(w, priceCoverageHint("这张定价表的 base ", missing))
		return
	}
	if expression == "" {
		return
	}
	missing, err := pricing.UncoveredTokenItems(expression)
	if err != nil {
		// 同一条边界：写入路径已经编译过一次，这里再编译失败不该改变结果。
		return
	}
	_, _ = io.WriteString(w, priceCoverageHint("这条规则", missing))
}

// printRuleList 打印规则清单。
//
// 排序用的是写入路径的裁决次序，所以「排在第一的那条」与「请求会被按它计价
// 的那条」是同一件事（见 pricing.Admin.List）。表达式源码不在这里：
// 这一栏要回答的是「有哪些规则、哪条会赢」，而源码在 add 的时候就在命令行上；
// 声明了哪几项单价由最后一栏给出。
func printRuleList(w io.Writer, rules []pricing.RuleInfo) {
	if len(rules) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("计价规则", "price add"))
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
		// 坏表与坏表达式同一个位置、同一个理由：不说明的话，这一行看起来
		// 就是一条手写规则，而它其实有一张解不开的表。
		if r.ScheduleErr != nil {
			_, _ = fmt.Fprintf(w, "规则 %d 的定价表解不开：%v\n", r.ID, r.ScheduleErr)
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
