package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/pricing"
)

// errOnboardExists 表示向导的某一步发现目标已经在库里，而这次没给 --update。
//
// 它是「一个已经配了一半」的信号，不是唯一键撞出来的：渠道白名单里有这个
// 名字、计价规则已经覆盖这个模型，库里都没有唯一约束会拒绝第二次写入。
// 归进 conflictErrors（exit.go）：下一步都是「先去库里看现在是什么，再决定
// 要不要覆盖」，与撞唯一键同一档。
var errOnboardExists = errors.New("已经存在")

// onboardOptions 是 model onboard 解析出来的那一次请求。
//
// DisplaySet 与 Display 分开：不给 --display 与给一个空串在这次命令里是同一
// 件事（展示名回落成规范名 / 不改展示名），所以不需要第三种状态。
type onboardOptions struct {
	Canonical string
	ChannelID int64
	Upstream  string
	PriceExpr string
	CostExpr  string
	Display   string
	Update    bool
	DryRun    bool
}

// onboardReport 是每一步实际做了什么。
//
// 只在整笔成功之后才打印：中途失败时打印「做到哪一步了」会让人以为前面几处
// 已经落地，而那些写入已经随事务回滚。
type onboardReport struct {
	Model   string
	Channel string
	Alias   string
	Price   string
	Cost    string
}

// runModelOnboard 在一个事务里把一个模型配到「能调通、能计价、有成本」。
//
// 它不另写校验：五处各自的合法性判据在 catalog / channels / pricing 里，
// 这里只负责参数解析、顺序与事务边界。
func runModelOnboard(args []string) error {
	if err := jsonUnsupported("model onboard"); err != nil {
		return err
	}

	opts, err := parseModelOnboard(args)
	if err != nil {
		return usageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		var report onboardReport
		apply := func(ctx context.Context, tx *sql.Tx) error {
			var applyErr error
			report, applyErr = onboardInTx(ctx, db, tx, opts)
			return applyErr
		}

		if opts.DryRun {
			// 与真跑同一条写路径：同一份判重、同一份写入，全部成功之后回滚。
			if err := inRolledBackTx(ctx, db, apply); err != nil {
				return err
			}
		} else {
			tx, beginErr := db.BeginTx(ctx, nil)
			if beginErr != nil {
				return fmt.Errorf("开启上架事务失败: %w", beginErr)
			}
			defer func() { _ = tx.Rollback() }()

			if err := apply(ctx, tx); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("提交上架事务失败: %w", err)
			}
		}

		printOnboardReport(humanStdout(), opts, report)
		return nil
	})
}

// parseModelOnboard 解析规范名（位置参数）与其余旗标。
//
// 位置参数排在旗标前面，与 channel map 那一族同一个形状：takeOperand
// 先摘走第一个非选项参数，剩下的交给 flag 集。
func parseModelOnboard(args []string) (onboardOptions, error) {
	canonical, rest, err := takeOperand(args, "<规范名>")
	if err != nil {
		return onboardOptions{}, err
	}

	fs := newFlagSet("model onboard", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, modelShortUsage) }
	channelID := fs.Int64("channel", 0, "")
	upstream := fs.String("upstream", "", "")
	priceExpr := fs.String("price-expr", "", "")
	costExpr := fs.String("cost-expr", "", "")
	display := fs.String("display", "", "")
	update := fs.Bool("update", false, "")
	dryRun := fs.Bool("dry-run", false, "")
	if err := fs.Parse(rest); err != nil {
		return onboardOptions{}, err
	}
	if fs.NArg() > 0 {
		return onboardOptions{}, fmt.Errorf(
			"onboard 只收一个位置参数（规范名），多出来的是 %q", fs.Arg(0))
	}

	switch {
	case *channelID == 0:
		return onboardOptions{}, errors.New("缺少 --channel：五处配置都挂在这条渠道上")
	case *channelID < 0:
		return onboardOptions{}, fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID)
	case strings.TrimSpace(*upstream) == "":
		return onboardOptions{}, errors.New("缺少 --upstream：这家上游把这个模型叫什么")
	case strings.TrimSpace(*priceExpr) == "":
		return onboardOptions{}, errors.New("缺少 --price-expr：售价表达式（记得给 shell 加引号）")
	case strings.TrimSpace(*costExpr) == "":
		return onboardOptions{}, errors.New("缺少 --cost-expr：成本表达式（记得给 shell 加引号）")
	}

	// 这几个值会进清单与单条回显：控制字符在连库之前拒掉。
	if err := checkDisplayText("--<规范名>", strings.TrimSpace(canonical)); err != nil {
		return onboardOptions{}, err
	}
	if err := checkDisplayText("--upstream（上游名）", strings.TrimSpace(*upstream)); err != nil {
		return onboardOptions{}, err
	}
	if err := checkDisplayText("--display（展示名）", strings.TrimSpace(*display)); err != nil {
		return onboardOptions{}, err
	}

	return onboardOptions{
		Canonical: strings.TrimSpace(canonical),
		ChannelID: *channelID,
		Upstream:  strings.TrimSpace(*upstream),
		PriceExpr: *priceExpr,
		CostExpr:  *costExpr,
		Display:   strings.TrimSpace(*display),
		Update:    *update,
		DryRun:    *dryRun,
	}, nil
}

// onboardInTx 按顺序写完五处。任一步出错就原样返回，由调用方回滚。
//
// 顺序是依赖决定的：渠道的 models 白名单与出站映射都指向规范名，规范名得先
// 在目录里；成本规则挂在渠道上，渠道得先存在（调用方给的就是已有渠道，这里
// 的读会先确认它在）。
func onboardInTx(ctx context.Context, db *sql.DB, tx *sql.Tx, opts onboardOptions) (onboardReport, error) {
	var report onboardReport

	// 判据读与写入走同一个 tx：在别处读看不到本事务刚写的行，五处判重就
	// 落到了事务外（见各包的 NewInTx / NewAdminInTx）。
	cat := catalog.NewInTx(tx)
	modelAdmin := catalog.NewAdminInTx(tx)

	// ① 模型目录
	if _, err := cat.Model(ctx, opts.Canonical); err != nil {
		if !errors.Is(err, catalog.ErrModelNotFound) {
			return report, err
		}
		// 没给 --display 时取规范名：向导的参数要一次给全能跑，而展示名在
		// 这条命令里是可选的。model add 同一条约定（缺省取规范名）。
		display := opts.Canonical
		if opts.Display != "" {
			display = opts.Display
		}
		id, createErr := modelAdmin.Create(ctx, catalog.CreateModelRequest{
			CanonicalName: opts.Canonical,
			DisplayName:   display,
		})
		if createErr != nil {
			return report, createErr
		}
		report.Model = fmt.Sprintf("新建 id=%d（展示名 %s）", id, display)
	} else {
		if !opts.Update {
			return report, fmt.Errorf(
				"%w：模型目录里已经有规范名 %q；确认要覆盖就加 --update",
				errOnboardExists, opts.Canonical)
		}
		if opts.Display != "" {
			display := opts.Display
			if err := modelAdmin.Update(ctx, catalog.UpdateModelRequest{
				CanonicalName: opts.Canonical,
				DisplayName:   &display,
			}); err != nil {
				return report, err
			}
			report.Model = "已存在，更新展示名"
		} else {
			report.Model = "已存在，保留（没给 --display）"
		}
	}

	// ② 渠道 models 白名单：先读回来，把规范名并进去，再整体写回。
	//
	// 不另写一条 UPDATE channels 的原因：白名单的归一化、端点校验（新挂进来的
	// 模型是否服务得了这条渠道的协议）都在 channels.Admin.Update 里，绕过去
	// 等于把那些判据漏掉。
	// sealer 与 registry 传 nil：本命令只改 models，channels.Update 只会走
	// 白名单那一支——type / protocols 与凭证才需要它们，而 checkModelEndpoints
	// 读的是内嵌的 catalog（已指向同一个 tx）。以后给 onboard 加 --type 或
	// --credential-* 时，这里要按那一支补齐，否则会撞上 nil。
	chAdmin := channels.NewAdminInTx(tx, nil, nil)
	channel, err := chAdmin.Get(ctx, opts.ChannelID)
	if err != nil {
		return report, err
	}
	if slices.Contains(channel.Models, opts.Canonical) {
		if !opts.Update {
			return report, fmt.Errorf(
				"%w：渠道 %d 的 models 白名单里已经有 %q；确认要覆盖就加 --update",
				errOnboardExists, opts.ChannelID, opts.Canonical)
		}
		report.Channel = "已包含"
	} else {
		models := append(slices.Clone(channel.Models), opts.Canonical)
		if updateErr := chAdmin.Update(ctx, channels.UpdateRequest{
			ID:     opts.ChannelID,
			Models: &models,
		}); updateErr != nil {
			return report, updateErr
		}
		report.Channel = fmt.Sprintf("加入（现在共 %d 个）", len(models))
	}

	// ③ 出站映射：渠道级，规范名 → 上游名。
	aliases, err := cat.Aliases(ctx, opts.ChannelID)
	if err != nil {
		return report, err
	}
	existing := slices.IndexFunc(aliases, func(alias catalog.Alias) bool {
		return alias.CanonicalName == opts.Canonical
	})
	if existing >= 0 {
		alias := aliases[existing]
		if !opts.Update {
			return report, fmt.Errorf(
				"%w：渠道 %d 已经有规范名 %q 的出站映射（上游名 %s）；确认要覆盖就加 --update",
				errOnboardExists, opts.ChannelID, opts.Canonical, alias.UpstreamModel)
		}
		if alias.UpstreamModel == opts.Upstream {
			report.Alias = fmt.Sprintf("已存在 id=%d，未变", alias.ID)
		} else {
			upstream := opts.Upstream
			if updateErr := modelAdmin.UpdateAlias(ctx, catalog.UpdateAliasRequest{
				ID:            alias.ID,
				UpstreamModel: &upstream,
			}); updateErr != nil {
				return report, updateErr
			}
			report.Alias = fmt.Sprintf("更新 id=%d（%s → %s）", alias.ID, alias.UpstreamModel, opts.Upstream)
		}
	} else {
		id, createErr := modelAdmin.CreateAlias(ctx, catalog.CreateAliasRequest{
			ChannelID:     opts.ChannelID,
			UpstreamModel: opts.Upstream,
			CanonicalName: opts.Canonical,
		})
		if createErr != nil {
			return report, createErr
		}
		report.Alias = fmt.Sprintf("新建 id=%d（%s → %s）", id, opts.Canonical, opts.Upstream)
	}

	// ④ 计价规则：channel 作用域，模型名单只有这个规范名。
	priceAdmin := pricing.NewAdminInTx(tx, pricing.CalendarOf(db))
	rules, err := priceAdmin.List(ctx)
	if err != nil {
		return report, err
	}
	priceHits := make([]pricing.RuleInfo, 0, 1)
	for _, rule := range rules {
		if rule.Scope == pricing.ScopeChannel && rule.ChannelID == opts.ChannelID &&
			slices.Contains(rule.ModelPatterns, opts.Canonical) {
			priceHits = append(priceHits, rule)
		}
	}
	switch len(priceHits) {
	case 0:
		id, createErr := priceAdmin.Create(ctx, pricing.CreateRequest{
			Scope:         pricing.ScopeChannel,
			ChannelID:     opts.ChannelID,
			Name:          onboardRuleName(opts.Canonical, opts.ChannelID),
			ModelPatterns: []string{opts.Canonical},
			Expr:          opts.PriceExpr,
		})
		if createErr != nil {
			return report, createErr
		}
		report.Price = fmt.Sprintf("新建 id=%d", id)
	case 1:
		if !opts.Update {
			return report, fmt.Errorf(
				"%w：计价规则 id=%d 已经覆盖渠道 %d 的 %q；确认要覆盖就加 --update",
				errOnboardExists, priceHits[0].ID, opts.ChannelID, opts.Canonical)
		}
		expr := opts.PriceExpr
		if updateErr := priceAdmin.Update(ctx, pricing.UpdateRequest{
			ID:   priceHits[0].ID,
			Expr: &expr,
		}); updateErr != nil {
			return report, updateErr
		}
		report.Price = fmt.Sprintf("更新 id=%d", priceHits[0].ID)
	default:
		return report, fmt.Errorf(
			"%w：计价规则里有 %d 条覆盖渠道 %d 的 %q（id：%s）；不猜要改哪一条，"+
				"先跑 larkaxctl price list 逐条看清",
			errOnboardExists, len(priceHits), opts.ChannelID, opts.Canonical,
			priceRuleIDs(priceHits))
	}

	// ⑤ 成本规则：同一个渠道、同一个规范名。
	costAdmin := pricing.NewCostAdminInTx(tx)
	costs, err := costAdmin.List(ctx, pricing.CostListQuery{ChannelID: opts.ChannelID})
	if err != nil {
		return report, err
	}
	costHits := make([]pricing.CostInfo, 0, 1)
	for _, rule := range costs {
		if slices.Contains(rule.ModelPatterns, opts.Canonical) {
			costHits = append(costHits, rule)
		}
	}
	switch len(costHits) {
	case 0:
		id, createErr := costAdmin.Create(ctx, pricing.CostCreateRequest{
			ChannelID:     opts.ChannelID,
			ModelPatterns: []string{opts.Canonical},
			Expr:          opts.CostExpr,
		})
		if createErr != nil {
			return report, createErr
		}
		report.Cost = fmt.Sprintf("新建 id=%d", id)
	case 1:
		if !opts.Update {
			return report, fmt.Errorf(
				"%w：成本规则 id=%d 已经覆盖渠道 %d 的 %q；确认要覆盖就加 --update",
				errOnboardExists, costHits[0].ID, opts.ChannelID, opts.Canonical)
		}
		expr := opts.CostExpr
		if updateErr := costAdmin.Update(ctx, pricing.CostUpdateRequest{
			ID:   costHits[0].ID,
			Expr: &expr,
		}); updateErr != nil {
			return report, updateErr
		}
		report.Cost = fmt.Sprintf("更新 id=%d", costHits[0].ID)
	default:
		return report, fmt.Errorf(
			"%w：成本规则里有 %d 条覆盖渠道 %d 的 %q（id：%s）；不猜要改哪一条，"+
				"先跑 larkaxctl upstream-cost list --channel %d 逐条看清",
			errOnboardExists, len(costHits), opts.ChannelID, opts.Canonical,
			costRuleIDs(costHits), opts.ChannelID)
	}

	return report, nil
}

// onboardRuleName 是向导建出来的计价规则的名字。
//
// 名字要能在一列里认出「这条是哪一次上架配的」：带上渠道 ID，因为同一个模型
// 在不同渠道上各有一条 channel 作用域的价，两条同名规则在 `price list` 里分不出
// 归属。
func onboardRuleName(canonical string, channelID int64) string {
	return fmt.Sprintf("onboard %s ch%d", canonical, channelID)
}

func priceRuleIDs(rules []pricing.RuleInfo) string {
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		ids = append(ids, strconv.FormatInt(rule.ID, 10))
	}
	return strings.Join(ids, ", ")
}

func costRuleIDs(rules []pricing.CostInfo) string {
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		ids = append(ids, strconv.FormatInt(rule.ID, 10))
	}
	return strings.Join(ids, ", ")
}

// printOnboardReport 打印每一步实际做了什么。
//
// 干跑与真跑打同一份报告，只在末尾多一句共用的干跑尾注（printDryRunNote）：
// 两者走的是同一条写路径，报告若长得不一样，读的人就分不清「这次会这么改」与
// 「已经这么改了」。
func printOnboardReport(w io.Writer, opts onboardOptions, report onboardReport) {
	_, _ = fmt.Fprintf(w, "模型目录：%s\n", report.Model)
	_, _ = fmt.Fprintf(w, "渠道 %d 的 models：%s\n", opts.ChannelID, report.Channel)
	_, _ = fmt.Fprintf(w, "出站映射：%s\n", report.Alias)
	_, _ = fmt.Fprintf(w, "计价规则：%s\n", report.Price)
	_, _ = fmt.Fprintf(w, "成本规则：%s\n", report.Cost)
	if opts.DryRun {
		// 尾注只有 dryrun.go 那一份：同一句话写两遍，改的时候就会漏掉一处。
		printDryRunNote(w)
	}
}
