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
	"time"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/config"
)

const modelUsage = `larkaxctl model 管理模型目录里的规范模型。

用法：
  larkaxctl model add --canonical <规范名> --display <展示名>
                      [--vendor <厂商>] [--family <家族>]
                      [--context-limit <tokens>] [--input-limit <tokens>]
                      [--output-limit <tokens>]
                      [--reasoning] [--tools] [--vision] [--audio] [--cache] [--batch]
                      [--modalities-input <模态,...>] [--modalities-output <模态,...>]
                      [--knowledge-cutoff <YYYY-MM-DD>] [--release-date <YYYY-MM-DD>]
  larkaxctl model list
  larkaxctl model show --canonical <规范名>
  larkaxctl model update --canonical <规范名>
                        [--display <展示名>] [--vendor <厂商>] [--family <家族>]
                        [--context-limit <tokens>] [--input-limit <tokens>]
                        [--output-limit <tokens>]
                        [--reasoning[=false]] [--tools[=false]] [--vision[=false]]
                        [--audio[=false]] [--cache[=false]] [--batch[=false]]
                        [--modalities-input <模态,...>] [--modalities-output <模态,...>]
                        [--knowledge-cutoff <YYYY-MM-DD>] [--release-date <YYYY-MM-DD>]
  larkaxctl model delete --canonical <规范名>

**规范名是选路与计费共用的键**：请求里的模型名按它匹配，别名与计价规则也挂在
它上面。所以它不能带空白字符，也不能改（要换名字就建新名、把别名与规则挪过去、
再删旧名）。update 与 delete 都按 --canonical 定位，它本身不可改。

add 的 stdout 只有新模型的 ID，其余信息走 stderr。
三个 limit 留空表示未知，负数会被拒绝。--display 必填，不会回落到规范名。

update **只改真的给了的那几项**，其余保持库里的值。空串与 0 是几种有含义的写法：
--vendor '' 与 --context-limit 0 都是「把这一栏清空」（库里存 NULL），
--knowledge-cutoff '' 与 --release-date '' 同理，--modalities-input '' 表示
这个模型没有这类模态。--display 清不掉——那一栏非空，它正是给人看的那一栏。

六个能力标志**成组**写入（写入路径对那六列是一次全量覆盖），所以给了其中任何
一个，命令会先读出当前那一行，再把这次给出的盖上去：只给 --tools 不会把另外
五个能力抹掉。要关掉一个能力写 --tools=false。

delete 只在**无人引用**时允许：还有别名或计价规则挂着这个规范名时它会被拒，
并报出各有多少条。别名要先删（渠道的 models 白名单里还留着这个名字，删别名
不等于删那条配置），计价规则要先改掉 model_patterns——直接删规则不会报错，
只会让请求落到别的规则上。那条错误原样打出来，不要绕过它去库里删。

show 打印一条规范模型：ID、命令能写的那些字段、采集源那边的 ID 与更新时间。
reasoning_options 与 raw 是采集来的原始元信息，命令行不写它们，也不在这里打。
`

func runModel(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, modelUsage)
		return usageErr(errors.New("model 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "add":
		return runModelAdd(rest)
	case "list":
		return runModelList(rest)
	case "show":
		return runModelShow(rest)
	case "update":
		return runModelUpdate(rest)
	case "delete":
		return runModelDelete(rest)
	case "help", "-h", "--help":
		fmt.Print(modelUsage)
		return nil
	default:
		fmt.Fprint(os.Stderr, modelUsage)
		return usageErr(fmt.Errorf("未知的 model 子命令 %q", action))
	}
}

func runModelAdd(args []string) error {
	req, err := parseModelAdd(args)
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

	id, err := catalog.NewAdmin(db).Create(ctx, req)
	if err != nil {
		return err
	}
	printCreatedModel(os.Stdout, os.Stderr, id, req.CanonicalName)
	return nil
}

func parseModelAdd(args []string) (catalog.CreateModelRequest, error) {
	fs := flag.NewFlagSet("model add", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := fs.String("canonical", "", "规范模型名，选路与计费共用的键")
	display := fs.String("display", "", "给人看的展示名")
	vendor := fs.String("vendor", "", "厂商")
	family := fs.String("family", "", "家族")
	contextLimit := fs.Int("context-limit", 0, "上下文窗口上限（tokens），留空表示未知")
	inputLimit := fs.Int("input-limit", 0, "输入上限（tokens），留空表示未知")
	outputLimit := fs.Int("output-limit", 0, "输出上限（tokens），留空表示未知")
	reasoning := fs.Bool("reasoning", false, "支持推理")
	tools := fs.Bool("tools", false, "支持工具调用")
	vision := fs.Bool("vision", false, "支持图片")
	audio := fs.Bool("audio", false, "支持音频")
	cache := fs.Bool("cache", false, "支持提示缓存")
	batch := fs.Bool("batch", false, "支持批量接口")
	modalitiesIn := fs.String("modalities-input", "", "输入模态，逗号分隔")
	modalitiesOut := fs.String("modalities-output", "", "输出模态，逗号分隔")
	knowledgeCutoff := fs.String("knowledge-cutoff", "", "知识截止日期 YYYY-MM-DD")
	releaseDate := fs.String("release-date", "", "发布日期 YYYY-MM-DD")
	if err := fs.Parse(args); err != nil {
		return catalog.CreateModelRequest{}, err
	}
	if fs.NArg() > 0 {
		return catalog.CreateModelRequest{}, fmt.Errorf(
			"add 不接受位置参数，得到 %q：规范名用 --canonical 指定", fs.Arg(0))
	}

	if strings.TrimSpace(*canonical) == "" {
		fs.Usage()
		return catalog.CreateModelRequest{}, errors.New("缺少 --canonical")
	}
	if strings.TrimSpace(*display) == "" {
		fs.Usage()
		return catalog.CreateModelRequest{}, errors.New("缺少 --display")
	}
	cutoff, err := parseDateFlag("--knowledge-cutoff", *knowledgeCutoff)
	if err != nil {
		return catalog.CreateModelRequest{}, err
	}
	released, err := parseDateFlag("--release-date", *releaseDate)
	if err != nil {
		return catalog.CreateModelRequest{}, err
	}

	return catalog.CreateModelRequest{
		CanonicalName: *canonical,
		DisplayName:   *display,
		Vendor:        *vendor,
		Family:        *family,
		ContextLimit:  *contextLimit,
		InputLimit:    *inputLimit,
		OutputLimit:   *outputLimit,
		Capabilities: catalog.Capabilities{
			Reasoning: *reasoning,
			Tools:     *tools,
			Vision:    *vision,
			Audio:     *audio,
			Cache:     *cache,
			Batch:     *batch,
		},
		ModalitiesInput:  splitOptionalList(*modalitiesIn),
		ModalitiesOutput: splitOptionalList(*modalitiesOut),
		KnowledgeCutoff:  cutoff,
		ReleaseDate:      released,
	}, nil
}

// modelCapabilityFlag 是一个能力标志：名字、这次解析出来的值，以及它落在
// catalog.Capabilities 的哪一栏。
//
// given 在 Parse 之后由 fs.Visit 填。`--tools` 与 `--tools=false` 都算「给了」
// ——前者打开、后者关掉——而「没提 --tools」是「别动它」。flag 包的零值分不出
// 后两者，所以判据是 Visit 记下的清单，不是这个布尔的取值。
type modelCapabilityFlag struct {
	name  string
	value *bool
	dst   func(*catalog.Capabilities) *bool
	given bool
}

// newCapabilityFlag 注册一个能力标志。
//
// 名字只写一次：它同时是 flag 包里的标志名与下面用来查「给没给」的键。
// 两处各写一个字符串时，写歪的那一处不会报错——那个标志会静默地永远算
// 「没给」，而它的表现是「关不掉一个能力」。
func newCapabilityFlag(fs *flag.FlagSet, name, usage string,
	dst func(*catalog.Capabilities) *bool) modelCapabilityFlag {
	return modelCapabilityFlag{name: name, value: fs.Bool(name, false, usage), dst: dst}
}

// anyCapabilityGiven 判断这六个标志里有没有给过。
func anyCapabilityGiven(flags []modelCapabilityFlag) bool {
	for _, f := range flags {
		if f.given {
			return true
		}
	}
	return false
}

// applyCapabilityFlags 把这次给出的能力标志盖到 base 上。
//
// # 为什么要一个 base
//
// 写入路径对那六列是**一次全量覆盖**（见 catalog.modelUpdateSets）。所以
// 「只给 --tools」不能拼一个只有 Tools 为真的结构体交出去——那会把另外五个
// 能力一起抹掉，而使用者说的是「加一个能力」。调用方因此先读出当前那一行，
// 把这里改完的结果整体交回去。
func applyCapabilityFlags(base catalog.Capabilities, flags []modelCapabilityFlag) catalog.Capabilities {
	for _, f := range flags {
		if !f.given {
			continue
		}
		*f.dst(&base) = *f.value
	}
	return base
}

// runModelUpdate 改一条规范模型。
//
// 它比别的 update 多一步：给过能力标志时先读出当前那一行（见
// applyCapabilityFlags）。那一步与后面的写入不在一个事务里，所以两次
// `model update` 并发时，能力那一栏是后写的那次说了算。这是刻意的取舍——
// 另一个做法是把六个标志当成「一组必须给全」，那会让「加一个能力」变成
// 每次都要把另外五个也抄一遍，而抄漏一个就是静默地关掉一个能力。
func runModelUpdate(args []string) error {
	req, capFlags, err := parseModelUpdate(args)
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

	if anyCapabilityGiven(capFlags) {
		current, readErr := catalog.New(db).Model(ctx, req.CanonicalName)
		if readErr != nil {
			return readErr
		}
		merged := applyCapabilityFlags(current.Capabilities, capFlags)
		req.Capabilities = &merged
	}

	if err := catalog.NewAdmin(db).Update(ctx, req); err != nil {
		return err
	}
	fmt.Printf("已更新规范模型 %s。\n", req.CanonicalName)
	return nil
}

// parseModelUpdate 解析 update 的参数。
//
// 除了请求本身，它还把六个能力标志单独交出去：那六个要跟库里当前的值合并
// （见 applyCapabilityFlags），而合并要连库，参数解析必须在连库之前完成。
func parseModelUpdate(args []string) (catalog.UpdateModelRequest, []modelCapabilityFlag, error) {
	fs := flag.NewFlagSet("model update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := fs.String("canonical", "", "要改的规范模型名（它本身不可改）")
	display := fs.String("display", "", "展示名")
	vendor := fs.String("vendor", "", "厂商，给空串表示清空")
	family := fs.String("family", "", "家族，给空串表示清空")
	contextLimit := fs.Int("context-limit", 0, "上下文窗口上限，0 表示未知（清空）")
	inputLimit := fs.Int("input-limit", 0, "输入上限，0 表示未知（清空）")
	outputLimit := fs.Int("output-limit", 0, "输出上限，0 表示未知（清空）")
	modalitiesIn := fs.String("modalities-input", "", "输入模态，逗号分隔，给空串表示清空")
	modalitiesOut := fs.String("modalities-output", "", "输出模态，逗号分隔，给空串表示清空")
	knowledgeCutoff := fs.String("knowledge-cutoff", "", "知识截止日期 YYYY-MM-DD，给空串表示清空")
	releaseDate := fs.String("release-date", "", "发布日期 YYYY-MM-DD，给空串表示清空")
	capFlags := []modelCapabilityFlag{
		newCapabilityFlag(fs, "reasoning", "支持推理", func(c *catalog.Capabilities) *bool { return &c.Reasoning }),
		newCapabilityFlag(fs, "tools", "支持工具调用", func(c *catalog.Capabilities) *bool { return &c.Tools }),
		newCapabilityFlag(fs, "vision", "支持图片", func(c *catalog.Capabilities) *bool { return &c.Vision }),
		newCapabilityFlag(fs, "audio", "支持音频", func(c *catalog.Capabilities) *bool { return &c.Audio }),
		newCapabilityFlag(fs, "cache", "支持提示缓存", func(c *catalog.Capabilities) *bool { return &c.Cache }),
		newCapabilityFlag(fs, "batch", "支持批量接口", func(c *catalog.Capabilities) *bool { return &c.Batch }),
	}
	if err := fs.Parse(args); err != nil {
		return catalog.UpdateModelRequest{}, nil, err
	}
	if fs.NArg() > 0 {
		return catalog.UpdateModelRequest{}, nil, fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的模型用 --canonical 指定", fs.Arg(0))
	}
	if strings.TrimSpace(*canonical) == "" {
		fs.Usage()
		return catalog.UpdateModelRequest{}, nil, errors.New("缺少 --canonical（要改的规范模型名）")
	}

	// 只有**真的给了**的标志才进请求。--context-limit 0 与「没提这一项」是
	// 两件事，而 flag 包的零值分不出它们，所以这里看 fs.Visit 记下的清单。
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for i := range capFlags {
		capFlags[i].given = given[capFlags[i].name]
	}

	req := catalog.UpdateModelRequest{CanonicalName: *canonical}
	changes := 0
	if given["display"] {
		req.DisplayName = display
		changes++
	}
	if given["vendor"] {
		req.Vendor = vendor
		changes++
	}
	if given["family"] {
		req.Family = family
		changes++
	}
	if given["context-limit"] {
		req.ContextLimit = contextLimit
		changes++
	}
	if given["input-limit"] {
		req.InputLimit = inputLimit
		changes++
	}
	if given["output-limit"] {
		req.OutputLimit = outputLimit
		changes++
	}
	if given["modalities-input"] {
		list := splitOptionalList(*modalitiesIn)
		req.ModalitiesInput = &list
		changes++
	}
	if given["modalities-output"] {
		list := splitOptionalList(*modalitiesOut)
		req.ModalitiesOutput = &list
		changes++
	}
	if given["knowledge-cutoff"] {
		cutoff, err := parseClearableDateFlag("--knowledge-cutoff", *knowledgeCutoff)
		if err != nil {
			return catalog.UpdateModelRequest{}, nil, err
		}
		req.KnowledgeCutoff = cutoff
		changes++
	}
	if given["release-date"] {
		released, err := parseClearableDateFlag("--release-date", *releaseDate)
		if err != nil {
			return catalog.UpdateModelRequest{}, nil, err
		}
		req.ReleaseDate = released
		changes++
	}
	if anyCapabilityGiven(capFlags) {
		changes++
	}
	if changes == 0 {
		fs.Usage()
		return catalog.UpdateModelRequest{}, nil, errors.New(
			"update 至少要改一项：给出 --display / --vendor / --family / " +
				"--context-limit / --input-limit / --output-limit / 六个能力标志 / " +
				"--modalities-input / --modalities-output / --knowledge-cutoff / --release-date 之一")
	}

	return req, capFlags, nil
}

// runModelDelete 删掉一条规范模型。
//
// 引用检查在写入路径里（见 catalog.Admin.Delete），这里不重做一遍：那份
// 判据要是两处各写一份，删掉的就可能是一条其实还在被用的名字。被引用时
// 那条错误里带着「各有多少条」，原样打出来。
func runModelDelete(args []string) error {
	fs := flag.NewFlagSet("model delete", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := fs.String("canonical", "", "要删的规范模型名")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"delete 不接受位置参数，得到 %q：要删的模型用 --canonical 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*canonical) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --canonical（要删的规范模型名）"))
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

	if err := catalog.NewAdmin(db).Delete(ctx, *canonical); err != nil {
		return err
	}
	fmt.Printf("已删除规范模型 %s。\n", *canonical)
	return nil
}

// runModelShow 打印一条规范模型。
//
// 走的是 catalog.Catalog 那条读取路径（选路与列举读的同一套列），不是
// 另写一条 SQL：写入与读取对同一套列的假设是否一致，只有「写进去再读出来」
// 能证明，而这里正是那个读回来的一端。
func runModelShow(args []string) error {
	fs := flag.NewFlagSet("model show", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := fs.String("canonical", "", "要看的规范模型名")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"show 不接受位置参数，得到 %q：要看的模型用 --canonical 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*canonical) == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --canonical（要看的规范模型名）"))
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

	model, err := catalog.New(db).Model(ctx, *canonical)
	if err != nil {
		return err
	}
	printModelDetail(os.Stdout, model)
	return nil
}

func runModelList(args []string) error {
	fs := flag.NewFlagSet("model list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
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

	models, err := catalog.New(db).Models(ctx)
	if err != nil {
		return err
	}
	printModelList(os.Stdout, models)
	return nil
}

// printCreatedModel 打印新模型。
//
// 与 printCreatedUser 同一条契约：stdout 恰好一行，只放能被直接接走的值。
func printCreatedModel(stdout, stderr io.Writer, id int64, canonicalName string) {
	_, _ = fmt.Fprintf(stderr, "已加入规范模型 %s(id=%d)。\n", canonicalName, id)
	_, _ = fmt.Fprintln(stdout, id)
}

// printModelList 打印目录里的规范模型。
func printModelList(w io.Writer, models []catalog.Model) {
	if len(models) == 0 {
		_, _ = fmt.Fprintln(w, "暂无模型。可使用 larkaxctl model add 添加。")
		return
	}

	tw := newTableWriter(w)
	_, _ = fmt.Fprintln(tw, "ID\t规范名\t展示名\t厂商\t家族\t上下文\t输入\t输出\t能力\t模态")
	for _, m := range models {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.ID, m.CanonicalName, m.DisplayName, orDash(m.Vendor), orDash(m.Family),
			limitLabel(m.ContextLimit), limitLabel(m.InputLimit), limitLabel(m.OutputLimit),
			capabilityLabel(m.Capabilities), modalityLabel(m.ModalitiesInput, m.ModalitiesOutput))
	}
	_ = tw.Flush()
}

// printModelDetail 打印一条规范模型。
//
// 一栏一行而不是一行一表：一条模型的字段比清单里那十栏多，横着排会折行，
// 而折行之后「哪一栏是哪个值」就得数着看。
//
// 打的是命令能写的那些字段，外加 ID、采集源那边的 ID 与更新时间。
// reasoning_options 与 raw 不打：它们是采集来的原始元信息，命令行不写它们，
// 而 raw 可能是一大段嵌套 JSON——把它铺在这里会把上面那几行挤走。
func printModelDetail(w io.Writer, m catalog.Model) {
	tw := newTableWriter(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", label, value)
	}
	row("ID", strconv.FormatInt(m.ID, 10))
	row("规范名", m.CanonicalName)
	row("展示名", m.DisplayName)
	row("厂商", orDash(m.Vendor))
	row("家族", orDash(m.Family))
	row("上下文上限", limitLabel(m.ContextLimit))
	row("输入上限", limitLabel(m.InputLimit))
	row("输出上限", limitLabel(m.OutputLimit))
	row("能力", capabilityLabel(m.Capabilities))
	row("输入模态", joinOrDash(m.ModalitiesInput))
	row("输出模态", joinOrDash(m.ModalitiesOutput))
	row("知识截止", dateLabel(m.KnowledgeCutoff))
	row("发布日期", dateLabel(m.ReleaseDate))
	row("采集源 ID", orDash(m.CanonicalModelID))
	row("更新时间", m.UpdatedAt.Format(time.RFC3339))
	_ = tw.Flush()
}

// dateLabel 打一个可空日期。nil 表示目录里没填。
//
// 只打日期不打时刻：表上那两列是 date（无时区），而把无时区的日期按某个
// 时区渲染成时刻，会让同一个值在不同机器上打出来不一样。
func dateLabel(date *time.Time) string {
	if date == nil {
		return "-"
	}
	return date.Format("2006-01-02")
}

// limitLabel 打一个 token 上限。
//
// 0 表示目录里没填（库里存 NULL），见 catalog.Model 的注释。打 0 会让人
// 以为那是一个量出来的上限。
func limitLabel(limit int) string {
	if limit == 0 {
		return "-"
	}
	return strconv.Itoa(limit)
}

// capabilityLabel 把六个布尔打成一栏。
func capabilityLabel(c catalog.Capabilities) string {
	var on []string
	for _, cap := range []struct {
		name string
		on   bool
	}{
		{"reasoning", c.Reasoning},
		{"tools", c.Tools},
		{"vision", c.Vision},
		{"audio", c.Audio},
		{"cache", c.Cache},
		{"batch", c.Batch},
	} {
		if cap.on {
			on = append(on, cap.name)
		}
	}
	if len(on) == 0 {
		return "-"
	}
	return strings.Join(on, ",")
}

// modalityLabel 打「输入→输出」两栏模态。
func modalityLabel(in, out []string) string {
	return joinOrDash(in) + "→" + joinOrDash(out)
}

// splitOptionalList 与 splitList 相同，但空串给 nil。
//
// modalities 那两列是 NOT NULL：空数组才是「这个模型没有这类模态」，
// 而空串切出来的 [""] 是一个含空字符串的数组——那是一条谁都匹配不上的配置。
func splitOptionalList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// parseDateFlag 解析一个 YYYY-MM-DD 日期参数，空串表示不填。
//
// 只认日期不认时刻：表上那两列是 date（无时区），而带时区的时刻转成日期时
// 依赖会话时区——同一个瞬间在 UTC+8 与 UTC-5 下可能落在不同的日子上。
// 让使用者直接给出他想要的那个日子，比让我们去猜时区可靠。
//
// 空串返回 nil：那一栏在库里可空，nil 就是「没填」这个合法取值
// （见 catalog.Model），不是「出错了但没报」——所以这里显式 nolint。
func parseDateFlag(name, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil //nolint:nilnil // 见上
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, fmt.Errorf("%s 不是 YYYY-MM-DD 形式的日期（例如 2026-01-31）: %w", name, err)
	}
	return &parsed, nil
}

// parseClearableDateFlag 解析一个可清空的日期参数。
//
// 与 parseDateFlag 的区别只在空串：那个把空串当成「不填」，返回 nil，调用方
// 就不写这一栏；这里把空串当成「清空」，返回零值时刻的指针——写入路径的
// dateOrNull 把它落成 NULL。update 要能表达「把这一栏清掉」，而在那个位置上
// nil 已经被「不改这一项」占着了。
func parseClearableDateFlag(name, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &time.Time{}, nil
	}
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, fmt.Errorf("%s 不是 YYYY-MM-DD 形式的日期（例如 2026-01-31）: %w", name, err)
	}
	return &parsed, nil
}
