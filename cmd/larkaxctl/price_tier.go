package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/pricing"
)

// priceTierShortUsage 是 price tier 的短版：`-h` / `--help` 与参数写错时打的那一份。
const priceTierShortUsage = `larkaxctl price tier 管理一条计价规则的阶梯档位。

用法：
  larkaxctl price tier add <规则名 或 ID> --above <tokens> [单价旗标] --by <用户>
  larkaxctl price tier list <规则名 或 ID>
  larkaxctl price tier remove <规则名 或 ID> <段号> --by <用户>

--above 是这一档的门槛：这次请求的输入上下文（含缓存读写）**达到**该值即命中
（>=）。门槛高的自动先命中，不必按顺序录入，也不用给 position。单价旗标与
price add 同一组，默认每百万 token，只写要改的键；没写的键沿用 base。

只有带结构化定价表（--schedule-file）的规则才有阶梯档位。

更细的边界：larkaxctl price tier help。
`

// priceTierUsage 是 price tier 的长版：`price tier help`。
const priceTierUsage = `larkaxctl price tier 管理一条计价规则的阶梯档位。

用法：
  larkaxctl price tier add <规则名 或 ID> --above <tokens>
                          [--in <单价>] [--out <单价>] [--cr <单价>]
                          [--cw <单价>] [--reasoning <单价>]
                          [--unit mtok|token] --by <用户 ID 或用户名>
  larkaxctl price tier list <规则名 或 ID>
  larkaxctl price tier remove <规则名 或 ID> <段号> --by <用户 ID 或用户名>

阶梯档位落在结构化定价表（--schedule-file）的 overrides 里，一条 add 追加一段：
门槛是 --above，单价是这一档改写的键。规则没有表时报错——手写表达式里的档位
读不出来，也就无从增删。

--above 的口径是**达到即命中**：输入上下文（prompt + 缓存读 + 缓存写）等于该
值时就走这一档。门槛之间不必按顺序录入，也不必给 position：门槛高的自动先
命中，200000 那一档排在 100000 前面。

单价旗标与 price add 是同一组，单位默认每百万 token（--in 0.2 即每百万 0.2），
--unit token 表示给的就是每 token 的价。**只写要改的键**：没写的键沿用 base，
不是写成 0。给了表里 base 没有的键会被拒——那一档没有兜底价可沿用。

段号从 1 开始，是这条规则 overrides 数组里的位置；price tier list 会打出来，
remove 按它删。同一门槛写两次会被拒（分不出哪一档该命中）。带门槛的覆盖都会
列在这里，包括同时带时段或星期条件的组合档（峰段 + 门槛）；remove 删的是整段
覆盖。

price tier list 打出来的单价是**每 token**，与 price show 看到的是同一份；--in
那一侧给的是每百万，换算在写入时完成。

更细的约定见 docs/cli-reference.md 的「price tier」一节。
`

// priceTierActions 是 price tier 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var priceTierActions = []string{"add", "list", "remove"}

// priceSegmentLocatorOperand 是 price tier / price peak 的位置参数：规则名或
// 规则 ID。
//
// 这些命令没有同名的旗标，定位符只能写成位置参数，所以 flag 留空；称呼沿用
// price update 那一份（见 priceLocatorOperand），几处定位的是同一件事。
var priceSegmentLocatorOperand = operandSpec{
	what: "定位符",
}

// runPriceTier 处理 price 下的 tier 子命令。
func runPriceTier(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, priceTierShortUsage)
		return usageErr(errors.New("price tier 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runPriceTierAdd(rest)
	case "list":
		return runPriceTierList(rest)
	case "remove":
		return runPriceTierRemove(rest)
	case "help":
		printUsage(os.Stdout, priceTierUsage)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, priceTierShortUsage)
		return nil
	default:
		printUsage(os.Stderr, priceTierShortUsage)
		return usageErr(fmt.Errorf("未知的 price tier 子命令 %q", action))
	}
}

// priceTierRequest 是 add 解析出来的这一次写入。
type priceTierRequest struct {
	// locator 是规则名或规则 ID 的原文：连库之后才定得下来是哪一条。
	locator string
	// above 是这一档的门槛：输入上下文达到它即命中。
	above int64
	// prices 是这一档改写的单价（每 token）。只含给了的键。
	prices map[string]float64
	// by 是这一次是谁做的，连库之后解析成 users.id。
	by string
}

func runPriceTierAdd(args []string) error {
	if err := jsonUnsupported("price tier add"); err != nil {
		return err
	}
	req, err := parsePriceTierAdd(args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := loadScheduleRule(ctx, db, req.locator, "阶梯档位")
		if err != nil {
			return err
		}
		actor, err := auth.New(db).ResolveUser(ctx, req.by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}

		schedule := info.Schedule
		above := req.above
		schedule.Overrides = append(schedule.Overrides, pricing.ScheduleOverride{
			MinContextTokens: &above,
			Prices:           req.prices,
		})
		// 段号是这次追加在 overrides 里的位置。Validate 会归一化条件与单价，
		// 不会重排数组，所以它在落库之后仍然是这一段。
		segment := len(schedule.Overrides)

		if err := pricing.NewAdmin(db).Update(ctx, pricing.UpdateRequest{
			ID:        info.ID,
			Schedule:  schedule,
			ChangedBy: actor.ID,
		}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(humanStderr(),
			"已为计价规则 %d 加上第 %d 段阶梯：输入上下文达到 %d token 起，改写 %s。\n",
			info.ID, segment, above, slotPricesLabel(req.prices))
		_, _ = fmt.Fprintln(humanStdout(), segment)
		return nil
	})
}

func parsePriceTierAdd(args []string) (priceTierRequest, error) {
	locator, rest, err := takeLeadingOperand(args, priceSegmentLocatorOperand)
	if err != nil {
		return priceTierRequest{}, err
	}

	fs := newFlagSet("price tier add", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceTierShortUsage) }
	above := fs.Int64("above", 0, "")
	by := fs.String("by", "", "")
	slots := newPriceSlotFlags(fs)
	if err := fs.Parse(rest); err != nil {
		return priceTierRequest{}, err
	}
	if fs.NArg() > 0 {
		return priceTierRequest{}, fmt.Errorf(
			"add 不接受多余的参数，得到 %q：规则名要写成第一个位置参数，排在旗标前面", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if strings.TrimSpace(locator) == "" {
		return priceTierRequest{}, errors.New(
			"缺少规则名或规则 ID：写成第一个位置参数（larkaxctl price tier add <规则名> --above <tokens> …）")
	}
	if !given["above"] {
		return priceTierRequest{}, errors.New(
			"缺少 --above：这一档从多少个输入 token 起命中（达到即命中）")
	}
	if *above <= 0 {
		return priceTierRequest{}, fmt.Errorf(
			"--above %d 无效：门槛是正整数 token 数，这一档从该值起命中", *above)
	}
	if !slots.anyGiven(given) {
		return priceTierRequest{}, errors.New(
			"缺少单价旗标：这一档要改写哪几项单价（--in / --out / --cr / --cw / --reasoning 至少给一项）")
	}
	prices, priceErr := slots.prices(given)
	if priceErr != nil {
		return priceTierRequest{}, priceErr
	}
	if strings.TrimSpace(*by) == "" {
		return priceTierRequest{}, errors.New("缺少 --by：这条命令要记下这一档价是谁定的")
	}
	return priceTierRequest{locator: locator, above: *above, prices: prices, by: *by}, nil
}

func runPriceTierList(args []string) error {
	fs := newFlagSet("price tier list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, priceTierShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		return usageErr(fmt.Errorf(
			"list 需要一个规则名或规则 ID，得到 %d 个参数：用法 larkaxctl price tier list <规则名 或 ID>",
			fs.NArg()))
	}
	locator := fs.Arg(0)

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := loadScheduleRule(ctx, db, locator, "阶梯档位")
		if err != nil {
			return err
		}
		tiers := collectPriceTiers(info.Schedule)
		if jsonOutput() {
			return emitJSON(os.Stdout, priceTierListDoc(info, tiers), nil)
		}
		printTierList(humanStdout(), tiers)
		return nil
	})
}

func runPriceTierRemove(args []string) error {
	if err := jsonUnsupported("price tier remove"); err != nil {
		return err
	}
	locator, segment, by, err := parsePriceSegmentRemove("price tier", priceTierShortUsage, args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := loadScheduleRule(ctx, db, locator, "阶梯档位")
		if err != nil {
			return err
		}
		actor, err := auth.New(db).ResolveUser(ctx, by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}

		schedule := info.Schedule
		if segment > len(schedule.Overrides) {
			return fmt.Errorf("段号 %d 超出范围：计价规则 %d 只有 %d 段覆盖（price tier list 看段号）",
				segment, info.ID, len(schedule.Overrides))
		}
		target := schedule.Overrides[segment-1]
		if target.MinContextTokens == nil {
			return fmt.Errorf("第 %d 段不是阶梯档（没有 --above 门槛）：price tier remove 只删阶梯档，"+
				"其余覆盖用 price update --schedule-file 改写整张表", segment)
		}
		removed := *target.MinContextTokens
		schedule.Overrides = append(schedule.Overrides[:segment-1], schedule.Overrides[segment:]...)

		if err := pricing.NewAdmin(db).Update(ctx, pricing.UpdateRequest{
			ID:        info.ID,
			Schedule:  schedule,
			ChangedBy: actor.ID,
		}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(humanStderr(), "已删除计价规则 %d 的第 %d 段阶梯（门槛 %d token）。\n",
			info.ID, segment, removed)
		return nil
	})
}

// parsePriceSegmentRemove 解析 remove 的两个位置参数与 --by，price tier 与
// price peak 共用：两者的段号是同一套（overrides 数组里的位置）。
//
// group 是命令组名（"price tier" / "price peak"），用在补全之外的三处：flag 集
// 的名字、那句「用法」与「段号见 … list」。short 是参数写错时要打的那一版帮助。
//
// 段号可以写在规则名后面，也可以写在旗标后面（`remove r1 --by alice 2`）：
// flag 包遇到第一个非旗标词就停，只认后一种写法时，`remove r1 2 --by alice`
// 里的 --by 会整段落进多余参数。两种写法都收，多给的一律报错。
func parsePriceSegmentRemove(group, short string, args []string) (locator string, segment int, by string, err error) {
	locator, rest, err := takeLeadingOperand(args, priceSegmentLocatorOperand)
	if err != nil {
		return "", 0, "", err
	}
	segmentRaw := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		segmentRaw = rest[0]
		rest = rest[1:]
	}

	fs := newFlagSet(group+" remove", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, short) }
	byFlag := fs.String("by", "", "")
	if err := fs.Parse(rest); err != nil {
		return "", 0, "", err
	}
	switch {
	case fs.NArg() > 1:
		return "", 0, "", fmt.Errorf("remove 不接受多余的参数：得到 %q 起", fs.Arg(0))
	case fs.NArg() == 1 && segmentRaw != "":
		return "", 0, "", fmt.Errorf(
			"段号给了两个值：%q 与 %q；只能写一处", segmentRaw, fs.Arg(0))
	case fs.NArg() == 1:
		segmentRaw = fs.Arg(0)
	}
	if strings.TrimSpace(locator) == "" {
		return "", 0, "", fmt.Errorf(
			"缺少规则名或规则 ID：写成第一个位置参数（larkaxctl %s remove <规则名> <段号> …）", group)
	}
	if strings.TrimSpace(segmentRaw) == "" {
		return "", 0, "", fmt.Errorf(
			"缺少段号：用法 larkaxctl %s remove <规则名 或 ID> <段号>（段号见 %s list）", group, group)
	}
	parsed, parseErr := strconv.Atoi(strings.TrimSpace(segmentRaw))
	if parseErr != nil || parsed <= 0 {
		return "", 0, "", fmt.Errorf("段号必须是正整数，当前为 %q", segmentRaw)
	}
	if strings.TrimSpace(*byFlag) == "" {
		return "", 0, "", errors.New("缺少 --by：这条命令要记下删掉这一段的是谁")
	}
	return locator, parsed, *byFlag, nil
}

// loadScheduleRule 按规则名或 ID 读一条**带结构化定价表**的规则。
//
// 阶梯档位与峰段都是表里的一行覆盖，手写表达式改不出可枚举的段，所以没有表时
// 直接拒绝，并指出该往哪条路走。thing 是那类段的名字（"阶梯档位" / "峰段"），
// 用在报错里。
func loadScheduleRule(ctx context.Context, db *sql.DB, locator, thing string) (pricing.RuleInfo, error) {
	id, err := resolvePriceRuleLocator(ctx, db, locator, nil)
	if err != nil {
		return pricing.RuleInfo{}, usageErr(err)
	}
	info, err := pricing.NewAdmin(db).Get(ctx, id)
	if err != nil {
		return pricing.RuleInfo{}, err
	}
	if info.Schedule == nil {
		return pricing.RuleInfo{}, fmt.Errorf(
			"计价规则 %d 没有结构化定价表：%s写在表的 overrides 里，手写表达式改不出来；"+
				"先用 --schedule-file 建一条带表的规则", info.ID, thing)
	}
	return info, nil
}

// priceTierInfo 是 list / remove 看到的一段阶梯。
type priceTierInfo struct {
	// segment 是这一段在 overrides 数组里的位置，从 1 开始。
	segment int
	// above 是门槛：输入上下文达到它即命中。
	above int64
	// prices 是这一段改写的单价（每 token）。
	prices map[string]float64
}

// collectPriceTiers 挑出表里带门槛的覆盖，按命中次序（门槛从高到低）排列。
//
// 数组里的位置不变，段号跟着走；排序只影响打印，不影响 remove 用哪个号。
func collectPriceTiers(schedule *pricing.Schedule) []priceTierInfo {
	out := make([]priceTierInfo, 0, len(schedule.Overrides))
	for i, override := range schedule.Overrides {
		if override.MinContextTokens == nil {
			continue
		}
		out = append(out, priceTierInfo{
			segment: i + 1,
			above:   *override.MinContextTokens,
			prices:  override.Prices,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].above > out[j].above })
	return out
}

// printTierList 打印一张阶梯表。
func printTierList(w io.Writer, tiers []priceTierInfo) {
	if len(tiers) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("阶梯档位", "price tier add"))
		return
	}
	tw := newTableWriter(w)
	p := newPalette(w)
	headers := []string{"段号", "门槛（输入上下文 token）", "单价（每 token）"}
	bolded := make([]string, len(headers))
	for i, h := range headers {
		bolded[i] = p.bold(h)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(bolded, "\t"))
	for _, tier := range tiers {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\n",
			p.bold(strconv.Itoa(tier.segment)), tier.above, slotPricesLabel(tier.prices))
	}
	_ = tw.Flush()
}

// slotPricesLabel 打一段覆盖改写了哪几项单价。
//
// 按键名字典序拼接，同一份表两次打出来是同一串；数值用最短可回读形式，
// 与 price show 里那一份逐字相同。
func slotPricesLabel(prices map[string]float64) string {
	keys := make([]string, 0, len(prices))
	for key := range prices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+strconv.FormatFloat(prices[key], 'g', -1, 64))
	}
	return strings.Join(parts, ", ")
}

// priceTierListDoc 组装 price tier list 的机器可读输出。
func priceTierListDoc(rule pricing.RuleInfo, tiers []priceTierInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["rule"] = map[string]any{
		"id":    rule.ID,
		"name":  rule.Name,
		"scope": rule.Scope,
	}
	doc["summary"] = map[string]any{"total": len(tiers)}
	entries := make([]map[string]any, 0, len(tiers))
	for _, tier := range tiers {
		entries = append(entries, map[string]any{
			"segment": tier.segment,
			"above":   tier.above,
			"prices":  tier.prices,
		})
	}
	doc["tiers"] = entries
	return doc
}
