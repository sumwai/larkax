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
  larkaxctl model create --model <规范名> --display <展示名>
                         [--vendor <厂商>] [--family <家族>]
                         [--context-limit <tokens>] [--input-limit <tokens>]
                         [--output-limit <tokens>]
                         [--reasoning[=false]] [--tools[=false]] [--vision[=false]]
                         [--audio[=false]] [--cache[=false]] [--batch[=false]]
                         [--input-modalities <模态,...>] [--output-modalities <模态,...>]
                         [--knowledge-cutoff <YYYY-MM-DD>] [--release-date <YYYY-MM-DD>]
  larkaxctl model list
  larkaxctl model get --model <规范名>
  larkaxctl model update --model <规范名>
                         [--display <展示名>] [--vendor <厂商>] [--family <家族>]
                         [--context-limit <tokens>] [--input-limit <tokens>]
                         [--output-limit <tokens>]
                         [--reasoning[=false]] [--tools[=false]] [--vision[=false]]
                         [--audio[=false]] [--cache[=false]] [--batch[=false]]
                         [--input-modalities <模态,...>] [--output-modalities <模态,...>]
                         [--knowledge-cutoff <YYYY-MM-DD>] [--release-date <YYYY-MM-DD>]
  larkaxctl model delete --model <规范名>

**规范名是选路与计费共用的键**：请求里的模型名按它匹配，别名与计价规则也挂在
它上面。所以它不能带空白字符，也不能改（要换名字就建新名、把别名与规则挪过去、
再删旧名）。update 与 delete 都按 --model 定位，它本身不可改。

create 的 stdout 只有新模型的 ID，其余信息走 stderr。
三个 limit 留空表示未知，负数会被拒绝。--display 必填，不会回落到规范名。

**六个能力标志各有三态**：不给 = 未知（库里是 NULL）、给 = 支持、
--tools=false = 明确声明不支持。三者在选路时处置不同：未知与支持放行，
不支持会让用到那一项的请求选不到这个模型。所以 create 时没给的开关落库是 NULL
而不是 false——那是「还没配过」，不是「配了说不支持」。

**判断一条记录配没配过，看 list / get 的那一栏**：没配过的项在 list 里不列
（六项都没配过时打 -），get 里逐项打出「未知 / 支持 / 不支持」。

update **只改真的给了的那几项**，其余保持库里的值。空串与 0 是几种有含义的写法：
--vendor '' 与 --context-limit 0 都是「把这一栏清空」（库里存 NULL），
--knowledge-cutoff '' 与 --release-date '' 同理，--input-modalities '' 表示
这个模型没有这类模态。--display 清不掉——那一栏非空，它正是给人看的那一栏。

六个能力标志**成组**写入（写入路径对那六列是一次全量覆盖），所以给了其中任何
一个，命令会先读出当前那一行，再把这次给出的盖上去：只给 --tools 不会把另外
五个能力抹掉。要关掉一个能力写 --tools=false。

delete 只在**无人引用**时允许：还有别名或计价规则挂着这个规范名时它会被拒，
并报出各有多少条。别名要先删（渠道的 models 白名单里还留着这个名字，删别名
不等于删那条配置），计价规则要先改掉 model_patterns——直接删规则不会报错，
只会让请求落到别的规则上。那条错误原样打出来，不要绕过它去库里删。

get 打印一条规范模型：ID、命令能写的那些字段、采集源那边的 ID 与更新时间。
reasoning_options 与 raw 是采集来的原始元信息，命令行不写它们，也不在这里打。

**旧名仍可用**：model add 与 model show 分别等同于 model create 与
model get，--canonical 等同于 --model。它们保留是为了不让已经在跑的脚本与
部署流程坏掉，行为与新名字逐字相同；新写的命令请用新名字。

# 别名

下面几个写法仍然可用，只是不再是正式写法（用法行里只写规范名）：

  -m <规范名>              同 --model，create / get / update / delete 都认
  --canonical <规范名>     同 --model
  --ctx <tokens>           同 --context-limit
  --in <tokens>            同 --input-limit
  --out <tokens>           同 --output-limit
  --im <模态,...>          同 --input-modalities
  --om <模态,...>          同 --output-modalities
  --modalities-input       同 --input-modalities
  --modalities-output      同 --output-modalities

**六个能力标志（--reasoning 那六个）不开简写**：它们各有三态（不给 = 未知、
给 = 支持、=false = 明确不支持），缩成一个字母之后「没给」与「给了 false」
更容易看混，而这两件事在选路上的处置相反。--ctx / --in / --out / --im / --om 是纯粹的两三个词缩成一节，没有第二种读法。
`

func runModel(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, modelUsage)
		return usageErr(errors.New("model 需要子命令"))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "create", "add":
		return runModelCreate(rest)
	case "list":
		return runModelList(rest)
	case "get", "show":
		return runModelGet(rest)
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

func runModelCreate(args []string) error {
	if err := jsonUnsupported("model create"); err != nil {
		return err
	}

	req, err := parseModelCreate(args)
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
	printCreatedModel(humanStdout(), humanStderr(), id, req.CanonicalName)
	return nil
}

func parseModelCreate(args []string) (catalog.CreateModelRequest, error) {
	fs := flag.NewFlagSet("model create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "", "规范模型名，选路与计费共用的键")
	stringAlias(fs, canonical, "m", sameAs("model"))
	display := fs.String("display", "", "给人看的展示名")
	vendor := fs.String("vendor", "", "厂商")
	family := fs.String("family", "", "家族")
	contextLimit := fs.Int("context-limit", 0, "上下文窗口上限（tokens），留空表示未知")
	intAlias(fs, contextLimit, "ctx", sameAs("context-limit"))
	inputLimit := fs.Int("input-limit", 0, "输入上限（tokens），留空表示未知")
	intAlias(fs, inputLimit, "in", sameAs("input-limit"))
	outputLimit := fs.Int("output-limit", 0, "输出上限（tokens），留空表示未知")
	intAlias(fs, outputLimit, "out", sameAs("output-limit"))
	capFlags := newCapabilityFlags(fs)
	modalitiesIn := legacyStringFlag(fs, "input-modalities", "modalities-input", "", "输入模态，逗号分隔")
	stringAlias(fs, modalitiesIn, "im", sameAs("input-modalities"))
	modalitiesOut := legacyStringFlag(fs, "output-modalities", "modalities-output", "", "输出模态，逗号分隔")
	stringAlias(fs, modalitiesOut, "om", sameAs("output-modalities"))
	knowledgeCutoff := fs.String("knowledge-cutoff", "", "知识截止日期 YYYY-MM-DD")
	releaseDate := fs.String("release-date", "", "发布日期 YYYY-MM-DD")
	if err := fs.Parse(args); err != nil {
		return catalog.CreateModelRequest{}, err
	}
	if fs.NArg() > 0 {
		return catalog.CreateModelRequest{}, fmt.Errorf(
			"create 不接受位置参数，得到 %q：规范名用 --model 指定", fs.Arg(0))
	}

	if strings.TrimSpace(*canonical) == "" {
		return catalog.CreateModelRequest{}, errors.New("缺少 --model")
	}
	if strings.TrimSpace(*display) == "" {
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

	// 只把**给了的**开关写进请求。没给的那几项留在零值 nil 上，落库是 NULL
	// = 未知——不是 false。「没配过」与「配了说不支持」的区分就从这一行开始，
	// 判据是 fs.Visit 记下的清单（见 modelCapabilityFlag）。
	markGivenCapabilityFlags(visitedFlags(fs), capFlags)

	return catalog.CreateModelRequest{
		CanonicalName:    *canonical,
		DisplayName:      *display,
		Vendor:           *vendor,
		Family:           *family,
		ContextLimit:     *contextLimit,
		InputLimit:       *inputLimit,
		OutputLimit:      *outputLimit,
		Capabilities:     applyCapabilityFlags(catalog.Capabilities{}, capFlags),
		ModalitiesInput:  splitOptionalList(*modalitiesIn),
		ModalitiesOutput: splitOptionalList(*modalitiesOut),
		KnowledgeCutoff:  cutoff,
		ReleaseDate:      released,
	}, nil
}

// modelCapabilityFlag 是一个能力标志：名字、这次解析出来的值，以及它落在
// catalog.Capabilities 的哪一栏。
//
// # 三态由 fs.Visit 与指针共同表达
//
//	没提 --tools        given=false，请求里那一栏是 nil（落库 NULL，未知）
//	--tools             given=true，值是指向 true 的指针（落库 true，支持）
//	--tools=false       given=true，值是指向 false 的指针（落库 false，不支持）
//
// flag 包的零值分不出后两者，所以判据是 Visit 记下的清单，不是那个布尔的
// 取值。缺了 given 这一半，「--tools=false」与「没提 --tools」就落到同一个
// 结论上——表现是「关不掉一个能力」。
type modelCapabilityFlag struct {
	name  string
	value *bool
	set   func(*catalog.Capabilities, *bool)
	given bool
}

// capabilityFlagUsage 是一句统一的帮助文本。六个标志的语义一模一样，
// 差别只在能力名，所以文本拼出来而不是抄六遍。
func capabilityFlagUsage(capability string) string {
	return capability + "：给 = 声明支持，=false = 明确不支持，不给 = 未知"
}

// newCapabilityFlags 注册六个能力标志。
//
// 名字只写一次：它同时是 flag 包里的标志名与下面用来查「给没给」的键。
// 两处各写一个字符串时，写歪的那一处不会报错——那个标志会静默地永远算
// 「没给」，而它的表现是「关不掉一个能力」。
func newCapabilityFlags(fs *flag.FlagSet) []modelCapabilityFlag {
	return []modelCapabilityFlag{
		newCapabilityFlag(fs, "reasoning", capabilityFlagUsage("推理"),
			func(c *catalog.Capabilities, v *bool) { c.Reasoning = v }),
		newCapabilityFlag(fs, "tools", capabilityFlagUsage("工具调用"),
			func(c *catalog.Capabilities, v *bool) { c.Tools = v }),
		newCapabilityFlag(fs, "vision", capabilityFlagUsage("图片"),
			func(c *catalog.Capabilities, v *bool) { c.Vision = v }),
		newCapabilityFlag(fs, "audio", capabilityFlagUsage("音频"),
			func(c *catalog.Capabilities, v *bool) { c.Audio = v }),
		newCapabilityFlag(fs, "cache", capabilityFlagUsage("提示缓存"),
			func(c *catalog.Capabilities, v *bool) { c.Cache = v }),
		newCapabilityFlag(fs, "batch", capabilityFlagUsage("批量接口"),
			func(c *catalog.Capabilities, v *bool) { c.Batch = v }),
	}
}

func newCapabilityFlag(fs *flag.FlagSet, name, usage string,
	set func(*catalog.Capabilities, *bool)) modelCapabilityFlag {
	return modelCapabilityFlag{name: name, value: fs.Bool(name, false, usage), set: set}
}

// visitedFlags 列出这次真的设置过的标志名——「给了 --x=false」与「没提 --x」
// 在这里分得开，而 flag 包的零值分不出它们。
//
// 必须在 Parse 之后调用：Visit 只遍历**被设置过**的标志。
func visitedFlags(fs *flag.FlagSet) map[string]bool {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	return given
}

// markGivenCapabilityFlags 按 visitedFlags 的结果标出哪些能力标志这次真的给了。
func markGivenCapabilityFlags(given map[string]bool, flags []modelCapabilityFlag) {
	for i := range flags {
		flags[i].given = given[flags[i].name]
	}
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
// 没给的项原样留着 base 里那一栏——create 时 base 是零值（六项都是 nil = 未知），
// update 时 base 是库里当前那一行。
//
// # update 为什么要一个 base
//
// 写入路径对那六列是**一次全量覆盖**（见 catalog.modelUpdateSets）。所以
// 「只给 --tools」不能拼一个只有 Tools 为真的结构体交出去——那会把另外五个
// 能力一起改成未知，而使用者说的是「加一个能力」。调用方因此先读出当前那一行，
// 把这里改完的结果整体交回去。
func applyCapabilityFlags(base catalog.Capabilities, flags []modelCapabilityFlag) catalog.Capabilities {
	for _, f := range flags {
		if !f.given {
			continue
		}
		f.set(&base, f.value)
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
	if err := jsonUnsupported("model update"); err != nil {
		return err
	}

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
	_, _ = fmt.Fprintf(humanStderr(), "已更新规范模型 %s。\n", req.CanonicalName)
	return nil
}

// parseModelUpdate 解析 update 的参数。
//
// 除了请求本身，它还把六个能力标志单独交出去：那六个要跟库里当前的值合并
// （见 applyCapabilityFlags），而合并要连库，参数解析必须在连库之前完成。
// 这几项各有别名，判「这次给没给」时要把别名一起查（见 flagGiven）。
// 漏掉一种的表现是「`--ctx 0` 被当成没提过这一项」，也就是清空静默失效。
var (
	contextLimitFlagNames  = []string{"context-limit", "ctx"}
	inputLimitFlagNames    = []string{"input-limit", "in"}
	outputLimitFlagNames   = []string{"output-limit", "out"}
	modalitiesInFlagNames  = []string{"input-modalities", "modalities-input", "im"}
	modalitiesOutFlagNames = []string{"output-modalities", "modalities-output", "om"}
)

func parseModelUpdate(args []string) (catalog.UpdateModelRequest, []modelCapabilityFlag, error) {
	fs := flag.NewFlagSet("model update", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "", "要改的规范模型名（它本身不可改）")
	stringAlias(fs, canonical, "m", sameAs("model"))
	display := fs.String("display", "", "展示名")
	vendor := fs.String("vendor", "", "厂商，给空串表示清空")
	family := fs.String("family", "", "家族，给空串表示清空")
	contextLimit := fs.Int("context-limit", 0, "上下文窗口上限，0 表示未知（清空）")
	intAlias(fs, contextLimit, "ctx", sameAs("context-limit"))
	inputLimit := fs.Int("input-limit", 0, "输入上限，0 表示未知（清空）")
	intAlias(fs, inputLimit, "in", sameAs("input-limit"))
	outputLimit := fs.Int("output-limit", 0, "输出上限，0 表示未知（清空）")
	intAlias(fs, outputLimit, "out", sameAs("output-limit"))
	modalitiesIn := legacyStringFlag(fs, "input-modalities", "modalities-input", "", "输入模态，逗号分隔，给空串表示清空")
	stringAlias(fs, modalitiesIn, "im", sameAs("input-modalities"))
	modalitiesOut := legacyStringFlag(fs, "output-modalities", "modalities-output", "", "输出模态，逗号分隔，给空串表示清空")
	stringAlias(fs, modalitiesOut, "om", sameAs("output-modalities"))
	knowledgeCutoff := fs.String("knowledge-cutoff", "", "知识截止日期 YYYY-MM-DD，给空串表示清空")
	releaseDate := fs.String("release-date", "", "发布日期 YYYY-MM-DD，给空串表示清空")
	capFlags := newCapabilityFlags(fs)
	if err := fs.Parse(args); err != nil {
		return catalog.UpdateModelRequest{}, nil, err
	}
	if fs.NArg() > 0 {
		return catalog.UpdateModelRequest{}, nil, fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的模型用 --model 指定", fs.Arg(0))
	}
	if strings.TrimSpace(*canonical) == "" {
		return catalog.UpdateModelRequest{}, nil, errors.New("缺少 --model（要改的规范模型名）")
	}

	// 只有**真的给了**的标志才进请求。--context-limit 0 与「没提这一项」是
	// 两件事，而 flag 包的零值分不出它们，所以这里看 fs.Visit 记下的清单。
	// 能力那六个同理：--tools=false 与「没提 --tools」也必须分得开。
	//
	// 有别名的那几项要连别名一起查（见 flagGiven）：fs.Visit 记的是**实际
	// 写出来的那个名字**，只查规范名的表现是 `--ctx 0` 被当成「没提这一项」，
	// 于是清空静默失效——命令报了成功，库里那一栏没动。
	given := visitedFlags(fs)
	markGivenCapabilityFlags(given, capFlags)

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
	if flagGiven(given, contextLimitFlagNames...) {
		req.ContextLimit = contextLimit
		changes++
	}
	if flagGiven(given, inputLimitFlagNames...) {
		req.InputLimit = inputLimit
		changes++
	}
	if flagGiven(given, outputLimitFlagNames...) {
		req.OutputLimit = outputLimit
		changes++
	}
	if flagGiven(given, modalitiesInFlagNames...) {
		list := splitOptionalList(*modalitiesIn)
		req.ModalitiesInput = &list
		changes++
	}
	if flagGiven(given, modalitiesOutFlagNames...) {
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
		return catalog.UpdateModelRequest{}, nil, errors.New(
			"update 至少要改一项：给出 --display / --vendor / --family / " +
				"--context-limit / --input-limit / --output-limit / 六个能力标志 / " +
				"--input-modalities / --output-modalities / --knowledge-cutoff / --release-date 之一")
	}

	return req, capFlags, nil
}

// runModelDelete 删掉一条规范模型。
//
// 引用检查在写入路径里（见 catalog.Admin.Delete），这里不重做一遍：那份
// 判据要是两处各写一份，删掉的就可能是一条其实还在被用的名字。被引用时
// 那条错误里带着「各有多少条」，原样打出来。
func runModelDelete(args []string) error {
	if err := jsonUnsupported("model delete"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("model delete", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "", "要删的规范模型名")
	stringAlias(fs, canonical, "m", sameAs("model"))
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"delete 不接受位置参数，得到 %q：要删的模型用 --model 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*canonical) == "" {
		return usageErr(errors.New("缺少 --model（要删的规范模型名）"))
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
	_, _ = fmt.Fprintf(humanStderr(), "已删除规范模型 %s。\n", *canonical)
	return nil
}

// runModelGet 打印一条规范模型。
//
// 走的是 catalog.Catalog 那条读取路径（选路与列举读的同一套列），不是
// 另写一条 SQL：写入与读取对同一套列的假设是否一致，只有「写进去再读出来」
// 能证明，而这里正是那个读回来的一端。
func runModelGet(args []string) error {
	fs := flag.NewFlagSet("model get", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "", "要看的规范模型名")
	stringAlias(fs, canonical, "m", sameAs("model"))
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf(
			"get 不接受位置参数，得到 %q：要看的模型用 --model 指定", fs.Arg(0)))
	}
	if strings.TrimSpace(*canonical) == "" {
		return usageErr(errors.New("缺少 --model（要看的规范模型名）"))
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
	if jsonOutput() {
		return emitJSON(os.Stdout, modelGetDoc(model), nil)
	}
	printModelDetail(humanStdout(), model)
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
	if jsonOutput() {
		return emitJSON(os.Stdout, modelListDoc(models), nil)
	}
	printModelList(humanStdout(), models)
	return nil
}

// printCreatedModel 打印新模型。
//
// 与 printCreatedUser 同一条契约：stdout 恰好一行，只放能被直接接走的值。
// 规范名的后面不带 `(id=N)`：名字已经认出这一条了，而 ID 就是 stdout 上那一个
// （见 docs/cli-output.md 的「实体只点一次身份」）。
func printCreatedModel(stdout, stderr io.Writer, id int64, canonicalName string) {
	_, _ = fmt.Fprintf(stderr, "已加入规范模型 %s。\n", canonicalName)
	_, _ = fmt.Fprintln(stdout, id)
}

// modelListDoc 组装 model list 的机器可读输出。
//
// 与 printModelList 是同一份数据的另一条渲染路径：那一档打表、把没设的上限
// 打成 `-`，这一档给原始值与 null。
func modelListDoc(models []catalog.Model) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["summary"] = map[string]any{"total": len(models)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entries = append(entries, modelEntry(m))
	}
	doc["models"] = entries
	return doc
}

// modelGetDoc 组装 model get 的机器可读输出。
//
// 只有一条，所以不打 summary（没有可数的东西），形状是 `{"model": {...}}`：
// 一眼能看出这一条是「哪个东西」的详情。
func modelGetDoc(m catalog.Model) map[string]any {
	doc := jsonDoc(jsonOK)
	doc["model"] = modelDetailEntry(m)
	return doc
}

// modelEntry 打清单里的一条规范模型：表格那十栏对应的**原始值**。
//
// 三个 limit 在库里可空、Go 侧用 0 表示没设，这里换成 null。表格那一档的
// `-` 是给人扫的，机器读的一方要能分清「没设」与「设成了 0」——而 0 不是
// 任何模型的合法上限，所以 0 只可能是前者（见 catalog.Model 的注释）。
func modelEntry(m catalog.Model) map[string]any {
	return map[string]any{
		"id":             m.ID,
		"canonical_name": m.CanonicalName,
		"display_name":   m.DisplayName,
		// 厂商与家族是 text 可空列，空串就是「没填」（update 的 --vendor ''
		// 正是把它清成 NULL）。
		"vendor":            nullableString(m.Vendor),
		"family":            nullableString(m.Family),
		"context_limit":     nullableInt(m.ContextLimit),
		"input_limit":       nullableInt(m.InputLimit),
		"output_limit":      nullableInt(m.OutputLimit),
		"capabilities":      capabilityEntry(m.Capabilities),
		"modalities_input":  jsonList(m.ModalitiesInput),
		"modalities_output": jsonList(m.ModalitiesOutput),
	}
}

// modelDetailEntry 打 model get 的那一条：清单里的那十栏，外加采集源那边
// 的几栏。表格那一档也是这个口径（见 printModelDetail）。
//
// reasoning_options 与 raw 不打：它们不参与任何判断，而 raw 可能是一大段
// 嵌套 JSON。要它们的一方直接查库，而不是让每条 get 都拖着这一坨。
func modelDetailEntry(m catalog.Model) map[string]any {
	entry := modelEntry(m)
	entry["knowledge_cutoff"] = jsonDate(m.KnowledgeCutoff)
	entry["release_date"] = jsonDate(m.ReleaseDate)
	entry["canonical_model_id"] = nullableString(m.CanonicalModelID)
	entry["updated_at"] = jsonTime(m.UpdatedAt)
	return entry
}

// capabilityEntry 打六个能力。
//
// 做成一个对象而不是拼成一栏文本：表格那一档把打开的几个逗号连起来
// （`tools,vision`）是排版，机器读的一方要能直接问 `capabilities.tools`
// 而不必去切字符串。
func capabilityEntry(c catalog.Capabilities) map[string]any {
	return map[string]any{
		"reasoning": c.Reasoning,
		"tools":     c.Tools,
		"vision":    c.Vision,
		"audio":     c.Audio,
		"cache":     c.Cache,
		"batch":     c.Batch,
	}
}

// printModelList 打印目录里的规范模型。
func printModelList(w io.Writer, models []catalog.Model) {
	if len(models) == 0 {
		_, _ = fmt.Fprintln(w, emptyListNote("模型", "model create"))
		return
	}

	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		p.bold("ID"), p.bold("规范名"), p.bold("展示名"), p.bold("厂商"),
		p.bold("家族"), p.bold("上下文"), p.bold("输入"), p.bold("输出"),
		p.bold("能力"), p.bold("模态"))
	for _, m := range models {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.bold(strconv.FormatInt(m.ID, 10)), p.bold(m.CanonicalName),
			m.DisplayName, orDash(m.Vendor), orDash(m.Family),
			limitLabel(m.ContextLimit), limitLabel(m.InputLimit), limitLabel(m.OutputLimit),
			capabilitySummary(m.Capabilities), modalityLabel(m.ModalitiesInput, m.ModalitiesOutput))
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
	p := newPalette(w)
	row := func(label, value string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold(label), value)
	}
	row("ID", strconv.FormatInt(m.ID, 10))
	row("规范名", m.CanonicalName)
	row("展示名", m.DisplayName)
	row("厂商", orDash(m.Vendor))
	row("家族", orDash(m.Family))
	row("上下文上限", limitLabel(m.ContextLimit))
	row("输入上限", limitLabel(m.InputLimit))
	row("输出上限", limitLabel(m.OutputLimit))
	row("能力", capabilityDetail(m.Capabilities))
	row("输入模态", joinOrDash(m.ModalitiesInput))
	row("输出模态", joinOrDash(m.ModalitiesOutput))
	row("知识截止", dateLabel(m.KnowledgeCutoff))
	row("发布日期", dateLabel(m.ReleaseDate))
	row("采集源 ID", orDash(m.CanonicalModelID))
	row("更新时间", p.dim(m.UpdatedAt.Format(time.RFC3339)))
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

// capabilitySummary 把六个三态能力打成一栏，用于清单。
//
// 只列**已声明**的项：没配过的项在这里省略，六项都没配过时打 -。清单是一张
// 十栏的表，把六项「未知」逐条铺开会让这一栏比其余九栏加起来还宽，而它要
// 回答的问题（这条记录配过什么）本来就只看已声明的那些。要逐项看三态用
// get（见 capabilityDetail）。
func capabilitySummary(c catalog.Capabilities) string {
	var declared []string
	for _, cap := range capabilityFields(c) {
		if cap.declared == nil {
			continue
		}
		declared = append(declared, cap.name+"="+capabilityState(cap.declared))
	}
	if len(declared) == 0 {
		return "-"
	}
	return strings.Join(declared, " ")
}

// capabilityDetail 把六个三态能力打成一栏，用于明细。
//
// 六项都打，未知的也打出来：「没配过」是一个要看得见的状态——它的处置
// （不参与选路判据）与「配了说不支持」（筛掉候选）正好相反。
func capabilityDetail(c catalog.Capabilities) string {
	parts := make([]string, 0, len(capabilityFields(c)))
	for _, cap := range capabilityFields(c) {
		parts = append(parts, cap.name+"="+capabilityState(cap.declared))
	}
	return strings.Join(parts, " ")
}

// capabilityField 是一项能力在输出里的样子：名字与三态值。
type capabilityField struct {
	name     string
	declared *bool
}

// capabilityFields 把六个能力按固定顺序摆出来。
//
// 顺序写死在这里：清单与明细两处各写一遍顺序时，它们会在下一次加列时分岔。
func capabilityFields(c catalog.Capabilities) []capabilityField {
	return []capabilityField{
		{"reasoning", c.Reasoning},
		{"tools", c.Tools},
		{"vision", c.Vision},
		{"audio", c.Audio},
		{"cache", c.Cache},
		{"batch", c.Batch},
	}
}

// capabilityState 把三态打成给人看的词。
//
// 打「未知」而不是 -：- 在这一栏里已经表示「这一项没出现在清单里」，
// 两个意思用同一个记号就分不出来了。
func capabilityState(declared *bool) string {
	switch {
	case declared == nil:
		return "未知"
	case *declared:
		return "支持"
	default:
		return "不支持"
	}
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
