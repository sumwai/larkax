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

**规范名是选路与计费共用的键**：请求里的模型名按它匹配，别名与计价规则也挂在
它上面。所以它不能带空白字符，也不能改（要换名字就建新名、把别名与规则挪过去、
再删旧名）。

add 的 stdout 只有新模型的 ID，其余信息走 stderr。
三个 limit 留空表示未知，负数会被拒绝。--display 必填，不会回落到规范名。
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

func runModelList(args []string) error {
	fs := flag.NewFlagSet("model list", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, modelUsage) }
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
		_, _ = fmt.Fprintln(w, "目录里还没有模型。")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\t规范名\t展示名\t厂商\t家族\t上下文\t输入\t输出\t能力\t模态")
	for _, m := range models {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.ID, m.CanonicalName, m.DisplayName, orDash(m.Vendor), orDash(m.Family),
			limitLabel(m.ContextLimit), limitLabel(m.InputLimit), limitLabel(m.OutputLimit),
			capabilityLabel(m.Capabilities), modalityLabel(m.ModalitiesInput, m.ModalitiesOutput))
	}
	_ = tw.Flush()
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
