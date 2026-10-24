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

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/pricing"
)

// pricePeakShortUsage 是 price peak 的短版：`-h` / `--help` 与参数写错时打的那一份。
const pricePeakShortUsage = `larkaxctl price peak 管理一条计价规则的峰段。

用法：
  larkaxctl price peak add <规则名 或 ID> --tz <时区> --window <时段> [旗标] --by <用户>
  larkaxctl price peak list <规则名 或 ID>
  larkaxctl price peak remove <规则名 或 ID> <段号> --by <用户>

--window 写 HH:MM-HH:MM，多段用逗号，可跨零点。--weekdays 收 mon,tue 或
mon-fri，缺省每天；--exclude-holidays 表示只在工作日算。--above 给了就与阶梯
合成「峰段且超过门槛」的一条覆盖。单价旗标与 price add 同一组，只写要改的键。

只有带结构化定价表（--schedule-file）的规则才有峰段。

更细的边界：larkaxctl price peak help。
`

// pricePeakUsage 是 price peak 的长版：`price peak help`。
const pricePeakUsage = `larkaxctl price peak 管理一条计价规则的峰段。

用法：
  larkaxctl price peak add <规则名 或 ID> --tz <时区> --window <时段>
                            [--weekdays <星期>] [--exclude-holidays]
                            [--above <tokens>]
                            [--in <单价>] [--out <单价>] [--cr <单价>]
                            [--cw <单价>] [--reasoning <单价>]
                            [--unit mtok|token] --by <用户 ID 或用户名>
  larkaxctl price peak list <规则名 或 ID>
  larkaxctl price peak remove <规则名 或 ID> <段号> --by <用户 ID 或用户名>

峰段落在结构化定价表（--schedule-file）的 overrides 里，一条 add 追加一段：
时段是 --window，单价是这一段改写的键。规则没有表时报错——手写表达式里的峰段
读不出来，也就无从增删。

--tz 必填，写 IANA 时区名（例如 Asia/Shanghai）。一张表只有一个时区：--tz 与
这张表已有的 tz 不一致时报参数错。

--window 写 HH:MM-HH:MM，多个时段用逗号（"01:00-04:00,06:00-10:00"），两端
必须是整点，可以跨零点（"22:00-08:00"）。落库时整串写进一条覆盖的 window。

--weekdays 收逗号分隔的缩写（mon,tue）或闭区间（mon-fri，按 sun/mon/tue/
wed/thu/fri/sat 的次序展开，不跨周）。缺省表示每天。--exclude-holidays 表示只
按工作日算（落库时写 day_kind=work）；它要读日历数据，那一年没有数据时求值失败。

--above 给了就与 price tier 的阶梯合成同一条覆盖（window 与
min_context_tokens 同时出现）：命中条件是「峰段且输入上下文达到门槛」，两个
维度是且。这一条在 price peak list 与 price tier list 里是同一个段号。

单价旗标与 price add 是同一组，单位默认每百万 token（--in 0.3 即每百万 0.3），
--unit token 表示给的就是每 token 的价。**只写要改的键**：没写的键沿用 base，
不是写成 0。给 base 里没有的键会被拒。

段号从 1 开始，是这条规则 overrides 数组里的位置；price peak list 打出来的
段号可以直接给 remove。list 只列 window 非空的覆盖——没有时段的阶梯只出现在
price tier list 里；带 --above 的组合档两个清单都列，段号相同。

remove 只删峰段：选中的那段没有 window 时报错，其余覆盖用
price update --schedule-file 改写整张表。

add 与 remove 都要 --by：这几次写入会重新编译表达式、留一版
pricing_rule_versions，changed_by 记的就是它。

更细的约定见 docs/cli-reference.md 的「price peak」一节。
`

// pricePeakActions 是 price peak 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var pricePeakActions = []string{"add", "list", "remove"}

// runPricePeak 处理 price 下的 peak 子命令。
func runPricePeak(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, pricePeakShortUsage)
		return usageErr(errors.New("price peak 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runPricePeakAdd(rest)
	case "list":
		return runPricePeakList(rest)
	case "remove":
		return runPricePeakRemove(rest)
	case "help":
		printUsage(os.Stdout, pricePeakUsage)
		return nil
	case "-h", "--help":
		printUsage(os.Stdout, pricePeakShortUsage)
		return nil
	default:
		printUsage(os.Stderr, pricePeakShortUsage)
		return usageErr(fmt.Errorf("未知的 price peak 子命令 %q", action))
	}
}

// pricePeakRequest 是 add 解析出来的这一次写入。
type pricePeakRequest struct {
	// locator 是规则名或规则 ID 的原文：连库之后才定得下来是哪一条。
	locator string
	// tz 是这张表解释钟点与星期用的时区，必须与表里已有的那个一致。
	tz string
	// window 是适用时段，整串写进一条覆盖的 window。
	window string
	// weekdays 是适用星期（已展开成缩写）；空表示每天。
	weekdays []string
	// excludeHolidays 为 true 时这条覆盖只按工作日算（DayKind 记 work）。
	excludeHolidays bool
	// above 非 nil 时这一条与阶梯组合：输入上下文达到它才命中。
	above *int64
	// prices 是这一段改写的单价（每 token）。只含给了的键。
	prices map[string]float64
	// by 是这一次是谁做的，连库之后解析成 users.id。
	by string
}

func runPricePeakAdd(args []string) error {
	if err := jsonUnsupported("price peak add"); err != nil {
		return err
	}
	req, err := parsePricePeakAdd(args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := loadScheduleRule(ctx, db, req.locator, "峰段")
		if err != nil {
			return err
		}
		if tzErr := checkScheduleTZ(info.Schedule, req.tz); tzErr != nil {
			return usageErr(tzErr)
		}
		actor, err := auth.New(db).ResolveUser(ctx, req.by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}

		schedule := info.Schedule
		override := pricing.ScheduleOverride{
			Window:           req.window,
			Weekdays:         req.weekdays,
			MinContextTokens: req.above,
			Prices:           req.prices,
		}
		if req.excludeHolidays {
			override.DayKind = "work"
		}
		schedule.Overrides = append(schedule.Overrides, override)
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
			"已为计价规则 %d 加上第 %d 段峰段：%s %s%s，改写 %s。\n",
			info.ID, segment, peakWeekdaysLabel(req.weekdays), req.window,
			peakExtraCondition(req.excludeHolidays, req.above), slotPricesLabel(req.prices))
		_, _ = fmt.Fprintln(humanStdout(), segment)
		return nil
	})
}

// peakExtraCondition 把 add 里时段与星期之外的条件写成一句附加说明（可能为空）。
//
// 与列表里「条件」那一栏分开拼：那一栏在没有附加条件时打 `-`，而这句话在没有
// 附加条件时应当什么都不写。
func peakExtraCondition(excludeHolidays bool, above *int64) string {
	parts := make([]string, 0, 2)
	if excludeHolidays {
		parts = append(parts, "，只按工作日")
	}
	if above != nil {
		parts = append(parts, fmt.Sprintf("，输入上下文达到 %d token", *above))
	}
	return strings.Join(parts, "")
}

func parsePricePeakAdd(args []string) (pricePeakRequest, error) {
	locator, rest, err := takeLeadingOperand(args, priceSegmentLocatorOperand)
	if err != nil {
		return pricePeakRequest{}, err
	}

	fs := newFlagSet("price peak add", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pricePeakShortUsage) }
	tz := fs.String("tz", "", "")
	window := fs.String("window", "", "")
	weekdays := fs.String("weekdays", "", "")
	excludeHolidays := fs.Bool("exclude-holidays", false, "")
	above := fs.Int64("above", 0, "")
	by := fs.String("by", "", "")
	slots := newPriceSlotFlags(fs)
	if parseErr := fs.Parse(rest); parseErr != nil {
		return pricePeakRequest{}, parseErr
	}
	if fs.NArg() > 0 {
		return pricePeakRequest{}, fmt.Errorf(
			"add 不接受多余的参数，得到 %q：规则名要写成第一个位置参数，排在旗标前面", fs.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	if strings.TrimSpace(locator) == "" {
		return pricePeakRequest{}, errors.New(
			"缺少规则名或规则 ID：写成第一个位置参数（larkaxctl price peak add <规则名> --tz <时区> --window <时段> …）")
	}
	if strings.TrimSpace(*tz) == "" {
		return pricePeakRequest{}, errors.New(
			"缺少 --tz：这张表按哪个时区解释钟点与星期（IANA 名，例如 Asia/Shanghai）")
	}
	if strings.TrimSpace(*window) == "" {
		return pricePeakRequest{}, errors.New(
			"缺少 --window：峰段的时段，写法 HH:MM-HH:MM，多段用逗号")
	}
	if given["weekdays"] && strings.TrimSpace(*weekdays) == "" {
		return pricePeakRequest{}, errors.New(
			"--weekdays 是空串（多半是一个没展开的变量）；缺省每天请不写这个旗标")
	}
	days, err := parsePeakWeekdays(*weekdays)
	if err != nil {
		return pricePeakRequest{}, err
	}
	var threshold *int64
	if given["above"] {
		if *above <= 0 {
			return pricePeakRequest{}, fmt.Errorf(
				"--above %d 无效：门槛是正整数 token 数，这一条从该值起与阶梯组合", *above)
		}
		value := *above
		threshold = &value
	}
	if !slots.anyGiven(given) {
		return pricePeakRequest{}, errors.New(
			"缺少单价旗标：这一段改写哪几项单价（--in / --out / --cr / --cw / --reasoning 至少给一项）")
	}
	prices, priceErr := slots.prices(given)
	if priceErr != nil {
		return pricePeakRequest{}, priceErr
	}
	if strings.TrimSpace(*by) == "" {
		return pricePeakRequest{}, errors.New("缺少 --by：这条命令要记下这一段峰是谁定的")
	}
	return pricePeakRequest{
		locator:         locator,
		tz:              strings.TrimSpace(*tz),
		window:          strings.TrimSpace(*window),
		weekdays:        days,
		excludeHolidays: *excludeHolidays,
		above:           threshold,
		prices:          prices,
		by:              *by,
	}, nil
}

// peakWeekdayOrder 是星期的展开次序：周日打头，与 Go 的 time.Weekday 一致。
//
// 与 internal/pricing 的 weekdayNames 是同一组名字，但这里要的是**次序**：
// 区间（mon-fri）按它展开，落库之后归一化也会排成这个次序，两处于是同一种写法。
var peakWeekdayOrder = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// parsePeakWeekdays 把 --weekdays 的取值折成星期缩写清单，空串表示每天。
//
// 收两种写法：逗号分隔的缩写（mon,tue）与闭区间（mon-fri）。区间按
// peakWeekdayOrder 的次序展开，不跨周：fri-mon 报错而不是悄悄绕一圈——绕一圈
// 会把「周五到周一」写成四天，而写的人可能想的是「周五、周一」两天。
//
// 七个都给时折成空清单（每天）：两者的命中条件相同，留七个名字只是让清单里
// 多一栏读不出信息的字。
func parsePeakWeekdays(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var picked [7]bool
	mark := func(items ...int) {
		for _, i := range items {
			picked[i] = true
		}
	}
	for _, part := range strings.Split(raw, ",") {
		item := strings.ToLower(strings.TrimSpace(part))
		if item == "" {
			return nil, fmt.Errorf("--weekdays %q 里有空项：多项之间只用逗号分隔", raw)
		}
		start, end, isRange := strings.Cut(item, "-")
		if !isRange {
			index, ok := peakWeekdayIndex(item)
			if !ok {
				return nil, peakWeekdayError(item, raw)
			}
			mark(index)
			continue
		}
		from, ok := peakWeekdayIndex(start)
		if !ok {
			return nil, peakWeekdayError(start, raw)
		}
		to, ok := peakWeekdayIndex(end)
		if !ok {
			return nil, peakWeekdayError(end, raw)
		}
		if from > to {
			return nil, fmt.Errorf("--weekdays %q 里的区间 %q 起点晚于终点："+
				"区间按 sun/mon/tue/wed/thu/fri/sat 的次序展开，不跨周", raw, item)
		}
		for i := from; i <= to; i++ {
			mark(i)
		}
	}

	out := make([]string, 0, len(peakWeekdayOrder))
	for i, name := range peakWeekdayOrder {
		if picked[i] {
			out = append(out, name)
		}
	}
	if len(out) == len(peakWeekdayOrder) {
		return nil, nil
	}
	return out, nil
}

// peakWeekdayIndex 查一个缩写是星期几。
func peakWeekdayIndex(name string) (int, bool) {
	for i, candidate := range peakWeekdayOrder {
		if candidate == name {
			return i, true
		}
	}
	return 0, false
}

// peakWeekdayError 是「--weekdays 里有个不认识的词」那一句。
func peakWeekdayError(name, raw string) error {
	return fmt.Errorf("--weekdays %q 里的 %q 不是星期：用 sun/mon/tue/wed/thu/fri/sat，"+
		"多项用逗号或 mon-fri 这样的区间", raw, name)
}

// peakWeekdaysLabel 打星期一栏：没给星期时是「每天」。
func peakWeekdaysLabel(days []string) string {
	if len(days) == 0 {
		return "每天"
	}
	return strings.Join(days, ",")
}

// peakConditionLabel 打「条件」那一栏：时段与星期之外的维度。
func peakConditionLabel(dayKind string, holidays []string, above *int64) string {
	parts := make([]string, 0, 3)
	switch dayKind {
	case "work":
		parts = append(parts, "工作日")
	case "rest":
		parts = append(parts, "休息日")
	}
	if len(holidays) > 0 {
		parts = append(parts, "节假日 "+strings.Join(holidays, "、"))
	}
	if above != nil {
		parts = append(parts, fmt.Sprintf("%d token 起", *above))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "，")
}

// checkScheduleTZ 判 --tz 与这张表已有的时区是否一致。
//
// 一张表只有一个时区：时段与星期都按它解释，两条覆盖各说一个时区，同一份表里
// 的「上午 9 点」就成了两个时刻。
func checkScheduleTZ(schedule *pricing.Schedule, tz string) error {
	want := strings.TrimSpace(tz)
	have := strings.TrimSpace(schedule.TZ)
	if have == "" {
		// 表里没有时区（解出来的旧表）时以这次给的为准，交给 Validate 落库。
		schedule.TZ = want
		return nil
	}
	if have != want {
		return fmt.Errorf("--tz %s 与这张表的时区 %s 不一致：一张表只有一个时区，"+
			"时段与星期都按它解释", want, have)
	}
	return nil
}

func runPricePeakList(args []string) error {
	fs := newFlagSet("price peak list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, pricePeakShortUsage) }
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		return usageErr(fmt.Errorf(
			"list 需要一个规则名或规则 ID，得到 %d 个参数：用法 larkaxctl price peak list <规则名 或 ID>",
			fs.NArg()))
	}
	locator := fs.Arg(0)

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := loadScheduleRule(ctx, db, locator, "峰段")
		if err != nil {
			return err
		}
		peaks := collectPricePeaks(info.Schedule)
		if jsonOutput() {
			return emitJSON(os.Stdout, pricePeakListDoc(info, peaks), nil)
		}
		printPeakList(humanStdout(), peaks)
		return nil
	})
}

func runPricePeakRemove(args []string) error {
	if err := jsonUnsupported("price peak remove"); err != nil {
		return err
	}
	locator, segment, by, err := parsePriceSegmentRemove("price peak", pricePeakShortUsage, args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := loadScheduleRule(ctx, db, locator, "峰段")
		if err != nil {
			return err
		}
		actor, err := auth.New(db).ResolveUser(ctx, by)
		if err != nil {
			return fmt.Errorf("解析 --by 失败: %w", err)
		}

		schedule := info.Schedule
		if segment > len(schedule.Overrides) {
			return fmt.Errorf("段号 %d 超出范围：计价规则 %d 只有 %d 段覆盖（price peak list 看段号）",
				segment, info.ID, len(schedule.Overrides))
		}
		target := schedule.Overrides[segment-1]
		if target.Window == "" {
			return fmt.Errorf("第 %d 段不是峰段（没有时段）：price peak remove 只删峰段，"+
				"其余覆盖用 price update --schedule-file 改写整张表", segment)
		}
		removed := target.Window
		schedule.Overrides = append(schedule.Overrides[:segment-1], schedule.Overrides[segment:]...)

		if err := pricing.NewAdmin(db).Update(ctx, pricing.UpdateRequest{
			ID:        info.ID,
			Schedule:  schedule,
			ChangedBy: actor.ID,
		}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(humanStderr(), "已删除计价规则 %d 的第 %d 段峰段（时段 %s）。\n",
			info.ID, segment, removed)
		return nil
	})
}

// pricePeakInfo 是 list / remove 看到的一段峰。
type pricePeakInfo struct {
	// segment 是这一段在 overrides 数组里的位置，从 1 开始。
	segment int
	// window 是适用时段，可能是一串逗号分开的多段。
	window string
	// weekdays 是适用星期；空表示每天。
	weekdays []string
	// dayKind 是 work / rest，空表示不限。
	dayKind string
	// holidays 是适用节假日名。
	holidays []string
	// above 非 nil 时这一条与阶梯组合。
	above *int64
	// prices 是这一段改写的单价（每 token）。
	prices map[string]float64
}

// collectPricePeaks 挑出表里带时段的覆盖，按段号次序排列。
//
// 判据是 window 非空：没有时段的阶梯不是峰段（它只出现在 price tier list 里）；
// 带门槛的组合档两个清单都列，数组位置相同，于是段号也相同。
//
// 不重排：段号就是数组位置，打印次序与段号一致，读的人不必在两张表之间对号。
func collectPricePeaks(schedule *pricing.Schedule) []pricePeakInfo {
	out := make([]pricePeakInfo, 0, len(schedule.Overrides))
	for i, override := range schedule.Overrides {
		if override.Window == "" {
			continue
		}
		out = append(out, pricePeakInfo{
			segment:  i + 1,
			window:   override.Window,
			weekdays: override.Weekdays,
			dayKind:  override.DayKind,
			holidays: override.Holidays,
			above:    override.MinContextTokens,
			prices:   override.Prices,
		})
	}
	return out
}

// printPeakList 打印一张峰段表。
func printPeakList(w io.Writer, peaks []pricePeakInfo) {
	if len(peaks) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("峰段", "price peak add"))
		return
	}
	tw := newTableWriter(w)
	p := newPalette(w)
	headers := []string{"段号", "时段", "星期", "条件", "单价（每 token）"}
	bolded := make([]string, len(headers))
	for i, h := range headers {
		bolded[i] = p.bold(h)
	}
	_, _ = fmt.Fprintln(tw, strings.Join(bolded, "\t"))
	for _, peak := range peaks {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			p.bold(strconv.Itoa(peak.segment)), peak.window, peakWeekdaysLabel(peak.weekdays),
			peakConditionLabel(peak.dayKind, peak.holidays, peak.above),
			slotPricesLabel(peak.prices))
	}
	_ = tw.Flush()
}

// pricePeakListDoc 组装 price peak list 的机器可读输出。
func pricePeakListDoc(rule pricing.RuleInfo, peaks []pricePeakInfo) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["rule"] = map[string]any{
		"id":    rule.ID,
		"name":  rule.Name,
		"scope": rule.Scope,
	}
	doc["summary"] = map[string]any{"total": len(peaks)}
	entries := make([]map[string]any, 0, len(peaks))
	for _, peak := range peaks {
		entry := map[string]any{
			"segment": peak.segment,
			"window":  peak.window,
			"prices":  peak.prices,
		}
		if len(peak.weekdays) > 0 {
			entry["weekdays"] = peak.weekdays
		}
		if peak.dayKind != "" {
			entry["day_kind"] = peak.dayKind
		}
		if len(peak.holidays) > 0 {
			entry["holidays"] = peak.holidays
		}
		if peak.above != nil {
			entry["above"] = *peak.above
		}
		entries = append(entries, entry)
	}
	doc["peaks"] = entries
	return doc
}
