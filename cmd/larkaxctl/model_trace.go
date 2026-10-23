package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/pricing"
)

// 本文件是 `model trace`：把一条请求的名字链路一次打出来。
//
// 链路上有三个名字，它们分属三张表：调用方写的（可能是一条入站别名）、解析
// 出来的规范名、选中渠道上发给上游的名字。名字对不上时人只能在三处之间手工
// 比对，而这三处的错法各自指向不同的下一步——别名没建、模型没建、这个渠道没
// 配出站映射、没有生效的计价规则。

// traceSource 是诊断要读的那几处，抽成函数值是为了让判据不依赖数据库。
//
// 生产装配见 newTraceSource。它们全是只读的，且都不碰凭证。
type traceSource struct {
	resolveAlias  func(ctx context.Context, name string) (string, error)
	shadowedAlias func(ctx context.Context, name string) (bool, error)
	model         func(ctx context.Context, name string) (catalog.Model, error)
	upstreams     func(ctx context.Context, name string) ([]catalog.Alias, error)
	aliasFor      func(ctx context.Context, channelID int64, name string) (string, error)
	supplies      func(ctx context.Context, name string) ([]channels.Supply, error)
	channelRef    func(ctx context.Context, id int64) (channels.ChannelRef, error)
	explain       func(ctx context.Context, q pricing.ExplainQuery) ([]pricing.RuleInfo, error)
}

// newTraceSource 把诊断接到真实的数据库上。
//
// channels.New 的第二个参数是信封解密器，这里传 nil：`Supplies` 与
// `ChannelRef` 都不解凭证，而为了让诊断显示一个渠道名去取主密钥，会把一条
// 只读命令变成需要密钥的操作。
func newTraceSource(db *sql.DB) traceSource {
	cat := catalog.New(db)
	chs := channels.New(db, nil)
	pr := pricing.NewAdmin(db)
	return traceSource{
		resolveAlias: cat.ResolveInbound,
		shadowedAlias: func(ctx context.Context, name string) (bool, error) {
			return cat.HasShadowedInboundAlias(ctx, name)
		},
		model:     cat.Model,
		upstreams: cat.UpstreamModels,
		aliasFor: func(ctx context.Context, channelID int64, name string) (string, error) {
			upstream, _, err := cat.Alias(ctx, channelID, name)
			return upstream, err
		},
		supplies: chs.SuppliesFor,
		channelRef: func(ctx context.Context, id int64) (channels.ChannelRef, error) {
			return chs.ChannelRef(ctx, id)
		},
		explain: pr.Explain,
	}
}

// 链路上四段的标签。断点行与 JSON 的 broken_at 用同一份字面量——两处各写
// 一遍的话，屏幕上说「上游名」、json 里说「upstream」就会让按 json 排查的
// 脚本对不上文档。
const (
	traceSegmentAlias    = "入站别名"
	traceSegmentModel    = "规范模型"
	traceSegmentUpstream = "上游名"
	traceSegmentPricing  = "计价规则"
)

// 出站名是从哪一处来的。
const (
	traceFromChannel  = "渠道级"
	traceFromPlatform = "平台级"
	traceFromIdentity = "无映射，按规范名"
)

// traceUpstream 是一条出站名。
//
// ChannelID 为 0 表示平台级映射；带 --channel 时它装的是被问的那条渠道，
// From 说明这个名字是从哪一处取的。
type traceUpstream struct {
	ChannelID int64
	Upstream  string
	From      string
	// Merchant 是渠道所属商家的用户名，拿不到时为空串。它只影响显示：
	// 渠道名要主密钥才读得到，而诊断不该要主密钥。
	Merchant string
}

// traceRule 是一条胜出的计价规则，连同它是在哪个渠道上胜出的。
type traceRule struct {
	ChannelID int64
	ID        int64
	Scope     string
	Name      string
}

// modelTrace 是一次诊断的结果。
type modelTrace struct {
	Requested string
	// IsAlias 为 true 表示调用方写的名字命中了入站别名。
	IsAlias bool
	// ShadowedAlias 为 true 表示这个名字同时是一条入站别名，但它是一个
	// 规范名，解析时规范名优先、别名被遮蔽。
	ShadowedAlias   bool
	Canonical       string
	CanonicalExists bool

	Upstreams []traceUpstream
	Rules     []traceRule

	// BrokenAt 为空表示链路完整；非空时是断掉的那一段（见上面四个常量），
	// BrokenReason 是给那一段的一句话。
	BrokenAt     string
	BrokenReason string
}

// evaluateModelTrace 走一遍链路，收集每一段的结果。
//
// 返回的 error 只表示**读不出来**（数据库故障之类），链路断掉不是 error——
// 它是一种诊断结论，装在结果里，由调用方决定退出码。
func evaluateModelTrace(ctx context.Context, src traceSource, requested string, channelID int64) (modelTrace, error) {
	tr := modelTrace{Requested: requested}

	canonical, err := src.resolveAlias(ctx, requested)
	if err != nil {
		return tr, err
	}
	tr.Canonical = canonical
	tr.IsAlias = canonical != requested
	if !tr.IsAlias {
		shadowed, shadowErr := src.shadowedAlias(ctx, requested)
		if shadowErr != nil {
			return tr, shadowErr
		}
		tr.ShadowedAlias = shadowed
	}

	if _, modelErr := src.model(ctx, canonical); modelErr != nil {
		if errors.Is(modelErr, catalog.ErrModelNotFound) {
			// 这一段在正常部署里**不该断**：写入侧校验规范名存在，删模型那一侧
			// 也数入了入站别名（见 catalog.Admin.countReferences），所以别名
			// 悬空只可能来自绕过写入路径的数据。它仍然是必须报的一段——不报的话
			// 那种数据的现场是「查无此模型」，而这句话指不到别名。
			tr.BrokenAt = traceSegmentModel
			tr.BrokenReason = fmt.Sprintf("规范名 %q 不在模型目录里", canonical)
			return tr, nil
		}
		return tr, modelErr
	}
	tr.CanonicalExists = true

	rows, err := src.upstreams(ctx, canonical)
	if err != nil {
		return tr, err
	}

	// 渠道名与商家名只为显示：拿不到不影响诊断，只影响一栏的写法。
	supplies, err := src.supplies(ctx, canonical)
	if err != nil {
		return tr, err
	}
	merchantOf := make(map[int64]string, len(supplies))
	ownerOf := make(map[int64]int64, len(supplies))
	for _, s := range supplies {
		merchantOf[s.ChannelID] = s.Merchant
		ownerOf[s.ChannelID] = s.OwnerUserID
	}

	if channelID != 0 {
		ref, refErr := src.channelRef(ctx, channelID)
		if errors.Is(refErr, channels.ErrChannelNotFound) {
			tr.BrokenAt = traceSegmentUpstream
			tr.BrokenReason = fmt.Sprintf("渠道 %d 不存在", channelID)
			return tr, nil
		}
		if refErr != nil {
			return tr, refErr
		}

		upstream, upstreamErr := src.aliasFor(ctx, channelID, canonical)
		if errors.Is(upstreamErr, catalog.ErrAliasNotFound) {
			tr.BrokenAt = traceSegmentUpstream
			tr.BrokenReason = fmt.Sprintf(
				"渠道 %d（%s）上没有 %q 的上游名：既没有渠道级映射，也没有平台级映射",
				channelID, ref.Name, canonical)
			return tr, nil
		}
		if upstreamErr != nil {
			return tr, upstreamErr
		}

		tr.Upstreams = traceUpstreams(rows, supplies, channelID, upstream)
	} else {
		tr.Upstreams = traceUpstreams(rows, supplies, 0, canonical)
	}

	targets := traceTargets(supplies, channelID)
	for _, t := range targets {
		rules, explainErr := src.explain(ctx, pricing.ExplainQuery{
			CanonicalModel: canonical,
			OwnerUserID:    ownerOf[t],
			ChannelID:      t,
		})
		if errors.Is(explainErr, pipeline.ErrNoRule) || (explainErr == nil && len(rules) == 0) {
			tr.BrokenAt = traceSegmentPricing
			tr.BrokenReason = traceNoRuleReason(canonical, t)
			return tr, nil
		}
		if explainErr != nil {
			return tr, explainErr
		}
		top := rules[0]
		tr.Rules = append(tr.Rules, traceRule{
			ChannelID: t,
			ID:        top.ID,
			Scope:     top.Scope,
			Name:      top.Name,
		})
	}

	return tr, nil
}

// traceUpstreams 把出站映射排成要打的那几行。
//
// 带 --channel 时只打**一行**：那条渠道上实际会发出去的名字，以及它是从
// 哪一处取的（渠道级映射、平台级映射、还是一条都没有）。平台级在这里仍然
// 算数——选路时渠道级没有就落回它，所以「这条渠道上发什么」的答案可能是
// 平台级给的。不打两行：两行都写着同一个名字时，人分不清哪一行是答案。
//
// 不给 --channel 时列出全部映射；一条都没有时打一行「按规范名原样发送」
// ——那是大多数部署的样子，不是断链。
func traceUpstreams(rows []catalog.Alias, supplies []channels.Supply, channelID int64, upstream string) []traceUpstream {
	merchantOf := make(map[int64]string, len(supplies))
	for _, s := range supplies {
		merchantOf[s.ChannelID] = s.Merchant
	}

	if channelID != 0 {
		from := traceFromIdentity
		switch {
		case upstream == "":
			from = traceFromIdentity
		case hasChannelRow(rows, channelID, upstream):
			from = traceFromChannel
		case hasPlatformRow(rows, upstream):
			from = traceFromPlatform
		}
		return []traceUpstream{{
			ChannelID: channelID,
			Upstream:  upstream,
			From:      from,
			Merchant:  merchantOf[channelID],
		}}
	}

	out := []traceUpstream{}
	for _, row := range rows {
		from := traceFromChannel
		if row.ChannelID == 0 {
			from = traceFromPlatform
		}
		out = append(out, traceUpstream{
			ChannelID: row.ChannelID,
			Upstream:  row.UpstreamModel,
			From:      from,
			Merchant:  merchantOf[row.ChannelID],
		})
	}
	if len(out) == 0 {
		out = append(out, traceUpstream{Upstream: upstream, From: traceFromIdentity})
	}
	return out
}

// hasChannelRow 判断 rows 里有没有这条渠道上、指向这个名字的映射。
func hasChannelRow(rows []catalog.Alias, channelID int64, upstream string) bool {
	for _, row := range rows {
		if row.ChannelID == channelID && row.UpstreamModel == upstream {
			return true
		}
	}
	return false
}

// hasPlatformRow 判断 rows 里有没有指向这个名字的平台级映射。
func hasPlatformRow(rows []catalog.Alias, upstream string) bool {
	for _, row := range rows {
		if row.ChannelID == 0 && row.UpstreamModel == upstream {
			return true
		}
	}
	return false
}

// traceTargets 是「在哪些渠道上问计价规则」。
//
// 给了 --channel 就问那一条；否则每个在供渠道各问一次——同一个规范名在商家
// 与渠道两个作用域上都可能有规则，只问一次会漏掉另外那几条。一条渠道都不在
// 供时问平台范围（渠道 0），至少能把「有没有一条兜底规则」答出来。
func traceTargets(supplies []channels.Supply, channelID int64) []int64 {
	if channelID != 0 {
		return []int64{channelID}
	}
	if len(supplies) == 0 {
		return []int64{0}
	}
	out := make([]int64, 0, len(supplies))
	for _, s := range supplies {
		out = append(out, s.ChannelID)
	}
	return out
}

func traceNoRuleReason(canonical string, channelID int64) string {
	if channelID == 0 {
		return fmt.Sprintf("模型 %q 没有生效的计价规则（平台范围）", canonical)
	}
	return fmt.Sprintf("模型 %q 在渠道 %d 下没有生效的计价规则", canonical, channelID)
}

// runModelTrace 打一条名字链路。
func runModelTrace(args []string) error {
	fs := newFlagSet("model trace", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, modelShortUsage) }
	channelID := fs.Int64("channel", 0, "只诊断这条渠道（平台级映射仍算数）")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() != 1 {
		return usageErr(fmt.Errorf("trace 需要一个位置参数（要诊断的模型名），得到 %d 个", fs.NArg()))
	}
	if *channelID < 0 {
		return usageErr(fmt.Errorf("--channel %d 无效：渠道 ID 从 1 开始", *channelID))
	}
	name := fs.Arg(0)

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		tr, err := evaluateModelTrace(ctx, newTraceSource(db), name, *channelID)
		if err != nil {
			return err
		}

		cause := traceCause(tr)
		if jsonOutput() {
			return emitJSON(os.Stdout, modelTraceDoc(tr), cause)
		}
		printModelTrace(humanStdout(), tr)
		return cause
	})
}

// traceCause 把一次诊断的结论翻成退出码。
//
// 断链的退出码是 1，而屏幕上已经由这份报告说清楚了：标成 reportOnly，让
// main 不再补打一行「larkaxctl: …」。链路完整时返回 nil。
//
// 抽成函数是为了让它能被直接断言：退出码若在 withDB 的闭包里就地算，
// 「把断点当成功」这种错法只能靠连库跑命令才发现。
func traceCause(tr modelTrace) error {
	if tr.BrokenAt == "" {
		return nil
	}
	return reportedErr(fmt.Errorf("断在 %s：%s", tr.BrokenAt, tr.BrokenReason))
}

// printModelTrace 打印一份诊断报告。
//
// 三段的名字排在同一张表里，一栏一个对象：调用方写的、解析成的、上游实际叫
// 的。断掉的那一段不留空行——报告末尾单独一句「断在 X」比一栏空白更容易被
// 人看见。
func printModelTrace(w io.Writer, tr modelTrace) {
	tw := newTableWriter(w)
	p := newPalette(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.bold("段"), p.bold("值"), p.bold("说明"))

	aliasNote := "不是别名，按原样"
	switch {
	case tr.IsAlias:
		aliasNote = "命中别名"
	case tr.ShadowedAlias:
		aliasNote = "不是别名；另有一条同名入站别名，被这个规范名遮蔽"
	}
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", traceSegmentAlias, p.bold(tr.Requested), aliasNote)

	if tr.CanonicalExists {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", traceSegmentModel, p.bold(tr.Canonical), "在目录里")
	}

	for _, u := range tr.Upstreams {
		note := traceUpstreamNote(u)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", traceSegmentUpstream, u.Upstream, note)
	}
	for _, r := range tr.Rules {
		note := "平台范围上胜出"
		if r.ChannelID != 0 {
			note = fmt.Sprintf("渠道 %d 上胜出", r.ChannelID)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", traceSegmentPricing,
			p.bold(fmt.Sprintf("#%d %s %s", r.ID, r.Scope, r.Name)), note)
	}
	_ = tw.Flush()

	if tr.BrokenAt != "" {
		_, _ = fmt.Fprintf(w, "断在 %s：%s\n", tr.BrokenAt, tr.BrokenReason)
	}
}

func traceUpstreamNote(u traceUpstream) string {
	parts := []string{u.From}
	if u.ChannelID != 0 {
		parts = append(parts, fmt.Sprintf("渠道 %d", u.ChannelID))
	}
	if u.Merchant != "" {
		parts = append(parts, "商家 "+u.Merchant)
	}
	return strings.Join(parts, " · ")
}

// modelTraceDoc 是机器读的那一档。
//
// 键名与 human 表逐项对得上：requested / canonical / upstream_model / rule_id
// 都是这条命令自己起的名字，见 docs/cli-reference.md 的 model 一节。
func modelTraceDoc(tr modelTrace) map[string]any {
	status := jsonOK
	if tr.BrokenAt != "" {
		status = jsonFailed
	}
	doc := jsonDoc(status)
	doc["requested"] = tr.Requested
	doc["alias_matched"] = tr.IsAlias
	doc["shadowed_alias"] = tr.ShadowedAlias
	doc["canonical"] = tr.Canonical
	doc["canonical_exists"] = tr.CanonicalExists

	upstreams := make([]map[string]any, 0, len(tr.Upstreams))
	for _, u := range tr.Upstreams {
		upstreams = append(upstreams, map[string]any{
			"upstream_model": u.Upstream,
			"channel_id":     u.ChannelID,
			"from":           u.From,
			"merchant":       u.Merchant,
		})
	}
	doc["upstreams"] = upstreams

	rules := make([]map[string]any, 0, len(tr.Rules))
	for _, r := range tr.Rules {
		rules = append(rules, map[string]any{
			"rule_id":    r.ID,
			"scope":      r.Scope,
			"name":       r.Name,
			"channel_id": r.ChannelID,
		})
	}
	doc["rules"] = rules

	if tr.BrokenAt != "" {
		doc["broken_at"] = tr.BrokenAt
		doc["broken_reason"] = tr.BrokenReason
	}
	return doc
}
