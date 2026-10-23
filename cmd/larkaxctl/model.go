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
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/config"
)

// modelHelp 是 model 的两版帮助文本（见 helpText）。
var modelHelp = helpText{short: modelShortUsage, long: modelUsage}

// modelShortUsage 是 model 的短版：`-h` / `--help` 与参数写错时打的那一份。
const modelShortUsage = `larkaxctl model 管理模型目录里的规范模型。

用法：
  larkaxctl model create --model <规范名> --display <展示名> [字段见下]
  larkaxctl model list
  larkaxctl model get --model <规范名>
  larkaxctl model update --model <规范名> [与 create 相同的字段]
  larkaxctl model delete --model <规范名>

create / update 的字段：--vendor、--family、三个 limit、六个能力标志
（--reasoning … --batch，各有三态）、--input-modalities 等。

规范名是选路与计费共用的键，建了就不能改；delete 只在无人引用时允许。

更细的边界：larkaxctl help model。
`

// modelUsage 是 model 的长版：`larkaxctl help model` 与 `model help`。
//
// 它不是常量：端点那一串取值由 internal/channel 的清单拼进来（见
// channel.EndpointValueList）。这里手抄一遍的话，加一种端点时它会留着老的那几条
// ——而「用法文本里少列一条」没有任何东西会报出来，写的人只能靠试。
var modelUsage = `larkaxctl model 管理模型目录里的规范模型。

用法：
  larkaxctl model create --model <规范名> --display <展示名>
                         [--vendor <厂商>] [--family <家族>]
                         [--context-limit <tokens>] [--input-limit <tokens>]
                         [--output-limit <tokens>]
                         [--reasoning[=false]] [--tools[=false]] [--vision[=false]]
                         [--audio[=false]] [--cache[=false]] [--batch[=false]]
                         [--endpoints <端点路径,...>]
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
                         [--endpoints <端点路径,...>]
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
不支持会让用到那一项的请求选不到这个模型。

**判断一条记录配没配过，看 list / get 的那一栏**：没配过的项在 list 里不列
（六项都没配过时打 -），get 里逐项打出「未知 / 支持 / 不支持」。端点清单只在
get 里打，list 那张表不列它。

**--endpoints 收的是端点路径**，逗号分隔，取值只有下面几条
（写别的当场报错，并把可选项原样列出来）：

  ` + channel.EndpointValueList + `

它与能力那六项是同一组声明，三态也同一套：不给 = 未知（这一项不参与选路判据）、
给空串 = 明确声明一条都不支持、给路径 = 支持这几条。「未知」与「明确没有」在这里
的结论相反——后者会让所有候选都落选。

update **只改真的给了的那几项**，其余保持库里的值。空串与 0 是几种有含义的写法：
--vendor '' 与 --context-limit 0 都是「把这一栏清空」（库里存 NULL），
--knowledge-cutoff '' 与 --release-date '' 同理，--input-modalities '' 表示
这个模型没有这类模态。--display 清不掉——那一栏非空，它正是给人看的那一栏。

六个能力标志与端点清单**成组**写入（写入路径对这一组是一次全量覆盖），所以给了
其中任何一项，命令会先读出当前那一行，再把这次给出的盖上去：只给 --tools 不会把
另外五个能力与端点清单抹掉，只给 --endpoints 也不会把六个能力抹掉。要关掉一个
能力写 --tools=false，要把端点清成「一条都不支持」写 --endpoints ''。

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
更容易看混，而这两件事在选路上的处置相反。--ctx / --in / --out / --im / --om
是纯粹的两三个词缩成一节，没有第二种读法。

更细的约定见 docs/cli-reference.md 的「model」一节。
`

// modelActions 是 model 的动作清单，补全从这里取。
// 分派仍是下面 switch 的 case——新增动作时两处都要加。
var modelActions = []string{"create", "list", "get", "update", "delete"}

func runModel(args []string) error {
	if len(args) == 0 {
		printUsage(os.Stderr, modelShortUsage)
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
	case "-h", "--help":
		printUsage(os.Stdout, modelShortUsage)
		return nil
	default:
		printUsage(os.Stderr, modelShortUsage)
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
	fs := newFlagSet("model create", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, modelShortUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "")
	stringAlias(fs, canonical, "m")
	display := fs.String("display", "", "")
	vendor := fs.String("vendor", "", "")
	family := fs.String("family", "", "")
	contextLimit := fs.Int("context-limit", 0, "")
	intAlias(fs, contextLimit, "ctx")
	inputLimit := fs.Int("input-limit", 0, "")
	intAlias(fs, inputLimit, "in")
	outputLimit := fs.Int("output-limit", 0, "")
	intAlias(fs, outputLimit, "out")
	capFlags := newCapabilityFlags(fs)
	endpoints := fs.String("endpoints", "", "")
	modalitiesIn := legacyStringFlag(fs, "input-modalities", "modalities-input", "")
	stringAlias(fs, modalitiesIn, "im")
	modalitiesOut := legacyStringFlag(fs, "output-modalities", "modalities-output", "")
	stringAlias(fs, modalitiesOut, "om")
	knowledgeCutoff := fs.String("knowledge-cutoff", "", "")
	releaseDate := fs.String("release-date", "", "")
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
	// 这些值都会进 model list / get 的表格：控制字符在连库之前拒掉（判据在
	// catalog 的写入路径，见 normalizeCanonicalName 一组）。
	if err := checkDisplayText("--model（规范模型名）", strings.TrimSpace(*canonical)); err != nil {
		return catalog.CreateModelRequest{}, err
	}
	if err := checkDisplayText("--display（展示名）", strings.TrimSpace(*display)); err != nil {
		return catalog.CreateModelRequest{}, err
	}
	if err := checkDisplayText("--vendor（厂商）", strings.TrimSpace(*vendor)); err != nil {
		return catalog.CreateModelRequest{}, err
	}
	if err := checkDisplayText("--family（家族）", strings.TrimSpace(*family)); err != nil {
		return catalog.CreateModelRequest{}, err
	}
	for i, value := range splitOptionalList(*modalitiesIn) {
		if err := checkDisplayText(
			fmt.Sprintf("--input-modalities（输入模态） 里第 %d 个", i+1), value); err != nil {
			return catalog.CreateModelRequest{}, err
		}
	}
	for i, value := range splitOptionalList(*modalitiesOut) {
		if err := checkDisplayText(
			fmt.Sprintf("--output-modalities（输出模态） 里第 %d 个", i+1), value); err != nil {
			return catalog.CreateModelRequest{}, err
		}
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
	// 判据是 fs.Visit 记下的清单（见 modelCapabilityFlag）。端点清单与那六项
	// 同一组、同一套三态，所以一起收（见 capabilityEdit）。
	given := visitedFlags(fs)
	markGivenCapabilityFlags(given, capFlags)
	edit, err := newCapabilityEdit(given, capFlags, *endpoints)
	if err != nil {
		return catalog.CreateModelRequest{}, err
	}

	return catalog.CreateModelRequest{
		CanonicalName:    *canonical,
		DisplayName:      *display,
		Vendor:           *vendor,
		Family:           *family,
		ContextLimit:     *contextLimit,
		InputLimit:       *inputLimit,
		OutputLimit:      *outputLimit,
		Capabilities:     edit.apply(catalog.Capabilities{}),
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

// newCapabilityFlags 注册六个能力标志。
//
// 名字只写一次：它同时是 flag 包里的标志名与下面用来查「给没给」的键。
// 两处各写一个字符串时，写歪的那一处不会报错——那个标志会静默地永远算
// 「没给」，而它的表现是「关不掉一个能力」。
func newCapabilityFlags(fs *flagSet) []modelCapabilityFlag {
	return []modelCapabilityFlag{
		newCapabilityFlag(fs, "reasoning",
			func(c *catalog.Capabilities, v *bool) { c.Reasoning = v }),
		newCapabilityFlag(fs, "tools",
			func(c *catalog.Capabilities, v *bool) { c.Tools = v }),
		newCapabilityFlag(fs, "vision",
			func(c *catalog.Capabilities, v *bool) { c.Vision = v }),
		newCapabilityFlag(fs, "audio",
			func(c *catalog.Capabilities, v *bool) { c.Audio = v }),
		newCapabilityFlag(fs, "cache",
			func(c *catalog.Capabilities, v *bool) { c.Cache = v }),
		newCapabilityFlag(fs, "batch",
			func(c *catalog.Capabilities, v *bool) { c.Batch = v }),
	}
}

func newCapabilityFlag(fs *flagSet, name string,
	set func(*catalog.Capabilities, *bool)) modelCapabilityFlag {
	return modelCapabilityFlag{name: name, value: fs.Bool(name, false, ""), set: set}
}

// visitedFlags 列出这次真的设置过的标志名——「给了 --x=false」与「没提 --x」
// 在这里分得开，而 flag 包的零值分不出它们。
//
// 必须在 Parse 之后调用：Visit 只遍历**被设置过**的标志。
func visitedFlags(fs *flagSet) map[string]bool {
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

// capabilityEdit 是这次给过的那一组声明：六个能力标志，外加端点清单。
//
// # 为什么它不直接进请求
//
// 这一组在写入路径上是**一次全量覆盖**（见 catalog.modelUpdateSets）：给出
// 其中任何一项，整组都要一起交出去。update 光有这次给的那几项不够——它得先
// 读出库里当前那一行再合并，否则「加一个能力」会静默地关掉另外五项与端点
// 清单。而合并要连库，参数解析必须在连库之前完成（参数错要当场报出来），
// 所以这一组由解析函数单独交出去。create 没有「当前那一行」，apply 的 base
// 就是零值。
type capabilityEdit struct {
	flags []modelCapabilityFlag

	// endpoints 只在 endpointsGiven 为真时有意义。
	endpoints []channel.Endpoint
	// endpointsGiven 分开「没给 --endpoints」与「--endpoints ''」：后者是
	// 明确声明一条都不支持（空切片），前者是不改这一栏（库里原样留着）。
	endpointsGiven bool
}

// newCapabilityEdit 按 fs.Visit 记下的清单，把这次给过的能力那一组收起来。
//
// 取值当场解析：写错的端点要在连库之前报出来，而报错里把可选项列全了
// （见 parseEndpointsFlag）。
func newCapabilityEdit(given map[string]bool, flags []modelCapabilityFlag, endpointsRaw string) (capabilityEdit, error) {
	edit := capabilityEdit{flags: flags, endpointsGiven: given["endpoints"]}
	if !edit.endpointsGiven {
		return edit, nil
	}
	parsed, err := parseEndpointsFlag("--endpoints", endpointsRaw)
	if err != nil {
		return capabilityEdit{}, err
	}
	edit.endpoints = parsed
	return edit, nil
}

// given 报告这一组这次有没有被动过。update 用它判断要不要先读出当前那一行。
func (e capabilityEdit) given() bool {
	return anyCapabilityGiven(e.flags) || e.endpointsGiven
}

// apply 把这次给过的那几项盖到 base 上。base 是 create 的零值，或者 update
// 时库里当前那一行。
func (e capabilityEdit) apply(base catalog.Capabilities) catalog.Capabilities {
	base = applyCapabilityFlags(base, e.flags)
	if e.endpointsGiven {
		base.Endpoints = e.endpoints
	}
	return base
}

// runModelUpdate 改一条规范模型。
//
// 它比别的 update 多一步：动过能力那一组（六个标志或端点清单）时，先读出
// 当前那一行再合并（见 capabilityEdit）。那一步与后面的写入不在一个事务里，
// 所以两次 `model update` 并发时，那一组是后写的那次说了算。这是刻意的取舍——
// 另一个做法是把这一组当成「必须给全」，那会让「加一个能力」变成每次都要把
// 另外五项与端点清单也抄一遍，而抄漏一个就是静默地关掉一个能力或一条端点。
func runModelUpdate(args []string) error {
	if err := jsonUnsupported("model update"); err != nil {
		return err
	}

	req, edit, err := parseModelUpdate(args)
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

	if edit.given() {
		current, readErr := catalog.New(db).Model(ctx, req.CanonicalName)
		if readErr != nil {
			return readErr
		}
		merged := edit.apply(current.Capabilities)
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
// 除了请求本身，它还把能力那一组单独交出去（见 capabilityEdit）：那几项要跟
// 库里当前的值合并，而合并要连库，参数解析必须在连库之前完成。
// 这几项各有别名，判「这次给没给」时要把别名一起查（见 flagGiven）。
// 漏掉一种的表现是「`--ctx 0` 被当成没提过这一项」，也就是清空静默失效。
var (
	contextLimitFlagNames  = []string{"context-limit", "ctx"}
	inputLimitFlagNames    = []string{"input-limit", "in"}
	outputLimitFlagNames   = []string{"output-limit", "out"}
	modalitiesInFlagNames  = []string{"input-modalities", "modalities-input", "im"}
	modalitiesOutFlagNames = []string{"output-modalities", "modalities-output", "om"}
)

func parseModelUpdate(args []string) (catalog.UpdateModelRequest, capabilityEdit, error) {
	fs := newFlagSet("model update", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, modelShortUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "")
	stringAlias(fs, canonical, "m")
	display := fs.String("display", "", "")
	vendor := fs.String("vendor", "", "")
	family := fs.String("family", "", "")
	contextLimit := fs.Int("context-limit", 0, "")
	intAlias(fs, contextLimit, "ctx")
	inputLimit := fs.Int("input-limit", 0, "")
	intAlias(fs, inputLimit, "in")
	outputLimit := fs.Int("output-limit", 0, "")
	intAlias(fs, outputLimit, "out")
	modalitiesIn := legacyStringFlag(fs, "input-modalities", "modalities-input", "")
	stringAlias(fs, modalitiesIn, "im")
	modalitiesOut := legacyStringFlag(fs, "output-modalities", "modalities-output", "")
	stringAlias(fs, modalitiesOut, "om")
	knowledgeCutoff := fs.String("knowledge-cutoff", "", "")
	releaseDate := fs.String("release-date", "", "")
	capFlags := newCapabilityFlags(fs)
	endpoints := fs.String("endpoints", "", "")
	if err := fs.Parse(args); err != nil {
		return catalog.UpdateModelRequest{}, capabilityEdit{}, err
	}
	if fs.NArg() > 0 {
		return catalog.UpdateModelRequest{}, capabilityEdit{}, fmt.Errorf(
			"update 不接受位置参数，得到 %q：要改的模型用 --model 指定", fs.Arg(0))
	}
	if strings.TrimSpace(*canonical) == "" {
		return catalog.UpdateModelRequest{}, capabilityEdit{}, errors.New("缺少 --model（要改的规范模型名）")
	}
	// 与 create 同一条：这些值会进 model list / get 的表格，控制字符在连库
	// 之前拒掉（判据在 catalog 的写入路径）。
	if err := checkDisplayText("--model（规范模型名）", strings.TrimSpace(*canonical)); err != nil {
		return catalog.UpdateModelRequest{}, capabilityEdit{}, err
	}

	// 只有**真的给了**的标志才进请求。--context-limit 0 与「没提这一项」是
	// 两件事，而 flag 包的零值分不出它们，所以这里看 fs.Visit 记下的清单。
	// 能力与端点同理：--tools=false 与「没提 --tools」、--endpoints ''（明确
	// 一条都不支持）与「没提 --endpoints」都必须分得开。
	//
	// 有别名的那几项要连别名一起查（见 flagGiven）：fs.Visit 记的是**实际
	// 写出来的那个名字**，只查规范名的表现是 `--ctx 0` 被当成「没提这一项」，
	// 于是清空静默失效——命令报了成功，库里那一栏没动。
	given := visitedFlags(fs)
	markGivenCapabilityFlags(given, capFlags)

	edit, err := newCapabilityEdit(given, capFlags, *endpoints)
	if err != nil {
		return catalog.UpdateModelRequest{}, capabilityEdit{}, err
	}

	req := catalog.UpdateModelRequest{CanonicalName: *canonical}
	changes := 0
	if given["display"] {
		if err := checkDisplayText("--display（展示名）", strings.TrimSpace(*display)); err != nil {
			return catalog.UpdateModelRequest{}, capabilityEdit{}, err
		}
		req.DisplayName = display
		changes++
	}
	if given["vendor"] {
		if err := checkDisplayText("--vendor（厂商）", strings.TrimSpace(*vendor)); err != nil {
			return catalog.UpdateModelRequest{}, capabilityEdit{}, err
		}
		req.Vendor = vendor
		changes++
	}
	if given["family"] {
		if err := checkDisplayText("--family（家族）", strings.TrimSpace(*family)); err != nil {
			return catalog.UpdateModelRequest{}, capabilityEdit{}, err
		}
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
		for i, value := range list {
			if err := checkDisplayText(
				fmt.Sprintf("--input-modalities（输入模态） 里第 %d 个", i+1), value); err != nil {
				return catalog.UpdateModelRequest{}, capabilityEdit{}, err
			}
		}
		req.ModalitiesInput = &list
		changes++
	}
	if flagGiven(given, modalitiesOutFlagNames...) {
		list := splitOptionalList(*modalitiesOut)
		for i, value := range list {
			if err := checkDisplayText(
				fmt.Sprintf("--output-modalities（输出模态） 里第 %d 个", i+1), value); err != nil {
				return catalog.UpdateModelRequest{}, capabilityEdit{}, err
			}
		}
		req.ModalitiesOutput = &list
		changes++
	}
	if given["knowledge-cutoff"] {
		cutoff, err := parseClearableDateFlag("--knowledge-cutoff", *knowledgeCutoff)
		if err != nil {
			return catalog.UpdateModelRequest{}, capabilityEdit{}, err
		}
		req.KnowledgeCutoff = cutoff
		changes++
	}
	if given["release-date"] {
		released, err := parseClearableDateFlag("--release-date", *releaseDate)
		if err != nil {
			return catalog.UpdateModelRequest{}, capabilityEdit{}, err
		}
		req.ReleaseDate = released
		changes++
	}
	if edit.given() {
		changes++
	}
	if changes == 0 {
		return catalog.UpdateModelRequest{}, capabilityEdit{}, errors.New(
			"update 至少要改一项：给出 --display / --vendor / --family / " +
				"--context-limit / --input-limit / --output-limit / 六个能力标志 / " +
				"--endpoints / --input-modalities / --output-modalities / " +
				"--knowledge-cutoff / --release-date 之一")
	}

	return req, edit, nil
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

	fs := newFlagSet("model delete", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, modelShortUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "")
	stringAlias(fs, canonical, "m")
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
	fs := newFlagSet("model get", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, modelShortUsage) }
	canonical := legacyStringFlag(fs, "model", "canonical", "")
	stringAlias(fs, canonical, "m")
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
	fs := newFlagSet("model list", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, modelShortUsage) }
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
//
// endpoints 打在 capabilities 旁边而不是它里面：capabilities 那一档是六个
// 三态开关的对象（见 capabilityEntry），而端点是一张清单，它的三态落在
// null / `[]` / 非空数组上，与那六个布尔键不同一种形状。
func modelDetailEntry(m catalog.Model) map[string]any {
	entry := modelEntry(m)
	entry["endpoints"] = nullableEndpoints(m.Capabilities.Endpoints)
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
	row("端点", endpointDetail(m.Capabilities.Endpoints))
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

// endpointDetail 把端点那一栏打出来，用于明细。
//
// 三态都要看得见，理由与能力那六项相同：「没配过」不参与选路判据，而
// 「明确一条都不支持」筛掉所有候选，两者结论相反。空清单不能打成 -：- 在
// 这一栏里已经表示「没配过」了（见 capabilityState 的同一条理由）。
//
// 分隔符用逗号不留空格：值就是命令行上那串路径，照着打出来的这一行抄回去
// 正是同一条命令。
func endpointDetail(endpoints []channel.Endpoint) string {
	switch {
	case endpoints == nil:
		return "未知"
	case len(endpoints) == 0:
		return "无（明确一条都不支持）"
	default:
		parts := make([]string, 0, len(endpoints))
		for _, e := range endpoints {
			parts = append(parts, string(e))
		}
		return strings.Join(parts, ",")
	}
}

// nullableEndpoints 把端点那一栏打成 JSON 的三态。
//
// 不套 jsonList：它把 nil 折成 `[]`，而这一列上 nil（没配过）与空数组
// （明确一条都不支持）的结论正好相反（见 catalog.Capabilities.Endpoints）。
func nullableEndpoints(endpoints []channel.Endpoint) any {
	if endpoints == nil {
		return nil
	}
	out := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		out = append(out, string(e))
	}
	return out
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

// parseEndpointsFlag 解析 --endpoints 的值。
//
// # 三态
//
//	没给这个标志   调用方不写这一栏，库里的值原样留着
//	--endpoints '' 空切片：明确声明一条都不支持
//	一串路径       这几条
//
// 空串落空切片而不是 nil，与 --input-modalities 那两项同一条口径；区别在于
// 这一列可空，所以「没配过」由**没给标志**表达，不由空串表达。
//
// # 每一项都过 channel.ParseEndpoint
//
// 取值就是适配器实际发的那几条路径，判据用 channel 包里那一个，不在这里另写
// 一遍 switch：两侧各判各的时，从命令行写进去的值可能读不出来，而那要等到
// 有人读那一行才发现。报错里会把这四条路径列全（见 channel.ParseEndpoint）。
func parseEndpointsFlag(name, raw string) ([]channel.Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []channel.Endpoint{}, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]channel.Endpoint, 0, len(parts))
	for _, part := range parts {
		endpoint, err := channel.ParseEndpoint(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%s 不是端点路径: %w", name, err)
		}
		out = append(out, endpoint)
	}
	return out, nil
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
