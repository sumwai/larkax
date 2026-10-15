package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/tokens"
)

// ruleSource 是测试用的规则来源：一条固定规则，或一个错误。
type ruleSource struct {
	rule Rule
	err  error
	// queries 记录每次匹配的条件，用于断言「结算用的是预留时的时刻」。
	queries []RuleQuery
}

func (r *ruleSource) Match(_ context.Context, q RuleQuery) (Rule, error) {
	r.queries = append(r.queries, q)
	if r.err != nil {
		return Rule{}, r.err
	}
	return r.rule, nil
}

// 每 token 1e-6 元、输出 2e-6 元的简单规则。
const simpleRule = `{"prompt": 0.000001, "completion": 0.000002}`

var fixedAt = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func newPipeline(t *testing.T, src RuleSource, opts ...func(*Pipeline)) *Pipeline {
	t.Helper()
	p := &Pipeline{
		Rules: src,
		Now:   func() time.Time { return fixedAt },
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func baseRequest() Request {
	return Request{
		RequestID:      "req-1",
		UserID:         42,
		CanonicalModel: "gpt-4o",
		ChannelID:      7,
		OwnerUserID:    9,
		InputTokens:    1000,
		MaxTokens:      500,
	}
}

func TestReserveComputesUpperBound(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	res, err := p.Reserve(context.Background(), baseRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 输入 1000 * 1e-6 = 0.001；输出 500 * 2e-6 = 0.001
	if res.Amount.String() != "0.00200000" {
		t.Errorf("预留 = %s，期望 0.002", res.Amount)
	}
	if res.RuleID != 1 || res.PricingExprSnap != simpleRule {
		t.Errorf("规则依据没带上: %+v", res)
	}
	if res.RuleMatchedAt.IsZero() {
		t.Error("应记录求值时刻，供事后复算")
	}
}

// 预留的记录里必须带规则的表达式快照：规则事后可能被改，
// 而账单要能解释「当时为什么是这个价」。
func TestReserveSnapshotsTheExpression(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 3, Version: 7, Source: simpleRule}}
	p := newPipeline(t, src)

	res, err := p.Reserve(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.PricingExprSnap != simpleRule {
		t.Errorf("表达式快照 = %q", res.PricingExprSnap)
	}
}

func TestReserveFailsWithoutRule(t *testing.T) {
	p := newPipeline(t, &ruleSource{err: ErrNoRule})
	_, err := p.Reserve(context.Background(), baseRequest())
	if !errors.Is(err, ErrNoRule) {
		t.Errorf("期望 ErrNoRule，得到 %v", err)
	}
}

func TestReserveFailsOnBrokenRule(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: `这不是表达式(`}}
	p := newPipeline(t, src)

	_, err := p.Reserve(context.Background(), baseRequest())
	if err == nil {
		t.Fatal("编译不过的规则应报错")
	}
	if !errors.Is(err, ErrRuleBroken) {
		t.Errorf("期望 ErrRuleBroken，得到 %v", err)
	}
}

// 没有 max_tokens 就算不出上界，而预留必须有上界。
func TestReserveRequiresMaxTokens(t *testing.T) {
	req := baseRequest()
	req.MaxTokens = 0

	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})
	_, err := p.Reserve(context.Background(), req)
	if err == nil {
		t.Fatal("缺少 max_tokens 应报错")
	}
	if !errors.Is(err, billing.ErrNoUpperBound) {
		t.Errorf("期望 ErrNoUpperBound，得到 %v", err)
	}
}

func TestReserveValidatesRequest(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})

	cases := map[string]func(*Request){
		"缺请求标识": func(r *Request) { r.RequestID = "" },
		"缺用户":   func(r *Request) { r.UserID = 0 },
		"缺模型":   func(r *Request) { r.CanonicalModel = "" },
		"负输入":   func(r *Request) { r.InputTokens = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := baseRequest()
			mutate(&req)
			if _, err := p.Reserve(context.Background(), req); err == nil {
				t.Error("应报错")
			}
		})
	}
}

// ── 结算 ────────────────────────────────────────────────────────────────

func TestSettleProducesChargeAndEntries(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 200},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	// 1000 * 1e-6 + 200 * 2e-6 = 0.001 + 0.0004
	if charge.Settlement.Actual.String() != "0.00140000" {
		t.Errorf("实际金额 = %s", charge.Settlement.Actual)
	}
	if charge.Settlement.Capped {
		t.Error("不该撞 Capped：预留是 0.002，实际 0.0014")
	}
	// 差额退回
	if charge.Settlement.Delta.String() != "-0.00060000" {
		t.Errorf("差额 = %s，期望 -0.0006", charge.Settlement.Delta)
	}
	if len(charge.Entries) == 0 {
		t.Fatal("应生成账本分录")
	}
	if charge.UsageSource != tokens.SourceMeasured {
		t.Errorf("用量来源 = %v", charge.UsageSource)
	}
}

// 分录必须平衡：组内求和为零。这是账本的核心不变量。
func TestSettleEntriesBalance(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, _ := p.Reserve(context.Background(), req)
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 1000, CompletionTokens: 200},
	})
	if err != nil {
		t.Fatal(err)
	}

	sum := money.Zero
	for _, e := range charge.Entries {
		sum = sum.Add(e.Amount)
	}
	if !sum.IsZero() {
		t.Errorf("分录不平：合计 %s（分录 %+v）", sum, charge.Entries)
	}
}

// 上游失败不计费。这是硬规则，且它比任何计算都优先。
func TestSettleUpstreamFailureIsNotBilled(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, _ := p.Reserve(context.Background(), req)

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		UpstreamFailed: true,
		UpstreamStatus: 503,
		ErrorCode:      "upstream_unavailable",
		// 即便带了用量也不计费
		Usage: channel.Usage{PromptTokens: 1000, CompletionTokens: 200},
	})
	if err == nil {
		t.Fatal("上游失败应返回错误而不是账单")
	}
	if charge != nil {
		t.Error("上游失败不该产生账单")
	}
	if !IsUpstreamFailure(err) {
		t.Errorf("应是 UpstreamFailure，得到 %T", err)
	}
	var uf *UpstreamFailure
	errors.As(err, &uf)
	if uf.Status != 503 || uf.Code != "upstream_unavailable" {
		t.Errorf("失败依据没带上: %+v", uf)
	}
}

// 客户端在流式响应中途断开**要计费**：已经产生的 token 上游是算了钱的。
func TestSettleClientCancelStillBills(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, _ := p.Reserve(context.Background(), req)

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		ClientCancelled: true,
		// 拿不到实测值，用量是估算的
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 50},
		UsageSource: tokens.SourceEstimated,
	})
	if err != nil {
		t.Fatalf("客户端取消仍应计费: %v", err)
	}
	if charge.Settlement.Actual.IsZero() {
		t.Error("客户端取消应产生金额")
	}
	// 估算这个事实要落到账单上，供事后看出来
	if charge.UsageSource != tokens.SourceEstimated {
		t.Errorf("用量来源 = %v，应标明是估算", charge.UsageSource)
	}
}

// 结算必须基于一次真实的预留。
func TestSettleRequiresReservation(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})
	req := baseRequest()

	if _, err := p.Settle(context.Background(), req, nil, Outcome{}); err == nil {
		t.Error("没有预留时应报错")
	}
}

// 预留与请求对不上时必须拒绝：那意味着把两次请求的钱算混了。
func TestSettleRejectsMismatchedReservation(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})
	req := baseRequest()
	res, _ := p.Reserve(context.Background(), req)

	other := req
	other.RequestID = "req-2"
	if _, err := p.Settle(context.Background(), other, res, Outcome{}); err == nil {
		t.Error("预留与请求不符时应报错")
	}
}

// 结算要用**预留时的时刻**取规则：规则在请求在途期间被改过的话，
// 用新规则结算会算出与预留无缘由的差额。
func TestSettleMatchesRuleAtReservationTime(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 100, CompletionTokens: 10},
	}); err != nil {
		t.Fatal(err)
	}

	if len(src.queries) != 2 {
		t.Fatalf("应有两次规则匹配（预留与结算各一次），实际 %d", len(src.queries))
	}
	if !src.queries[0].At.Equal(src.queries[1].At) {
		t.Errorf("结算用的时刻 %v 与预留的 %v 不同", src.queries[1].At, src.queries[0].At)
	}
}

// 金额为零时不写分录：大量零金额分录会让「有分录就说明发生过消费」
// 这个对账判据失效。
func TestZeroAmountProducesNoEntries(t *testing.T) {
	// 全零单价的规则
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: `{"prompt": 0.0}`}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Amount.IsZero() {
		t.Fatalf("全零单价的预留应为零，得到 %s", res.Amount)
	}

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(charge.Entries) != 0 {
		t.Errorf("零金额不该产生分录，实际 %d 条", len(charge.Entries))
	}
}

// ── 与日历、分时接起来 ──────────────────────────────────────────────────

// 一条按时间分档的规则：走最坏情况求值时，预留要取更贵的那一档。
func TestReserveUsesWorstCaseAcrossTimeBranches(t *testing.T) {
	// 白天贵、夜里便宜
	src := &ruleSource{rule: Rule{ID: 1, Version: 1,
		Source: `hour("Asia/Shanghai") >= 8 && hour("Asia/Shanghai") < 20 ? {"prompt": 0.000002} : {"prompt": 0.000001}`}}
	p := newPipeline(t, src)

	// 让求值发生在夜里（UTC 00:00 = 上海 08:00，正好是边界，取 02:00 更稳）
	night := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC) // 上海 02:00
	p.Now = func() time.Time { return night }

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	// 预留必须按**贵的那一档**（白天 2e-6），而不是当前时刻那一档
	// 输入 1000 * 2e-6 = 0.002；输出 500 * 2e-6… 输出没有分支，取规则里的值
	// 规则只给了 prompt，所以输出单价为 0
	if res.Amount.String() != "0.00200000" {
		t.Errorf("预留 = %s，期望按贵的一档算的 0.002", res.Amount)
	}

	// 白天结算不会撞 Capped
	day := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC) // 上海 12:00
	p.Now = func() time.Time { return day }
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if charge.Settlement.Capped {
		t.Errorf("白天结算撞上了 Capped：预留 %s 实际 %s",
			res.Amount, charge.Settlement.Breakdown.Total)
	}
}

func TestDefaultAsset(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Asset != "CNY" {
		t.Errorf("默认资产 = %q", res.Asset)
	}

	req.Asset = "USD"
	res, err = p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Asset != "USD" {
		t.Errorf("指定的资产应生效，得到 %q", res.Asset)
	}
}

// 编译缓存要复用：同一条规则走两次不该编两次。
func TestProgramCacheIsReused(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	for i := 0; i < 3; i++ {
		if _, err := p.Reserve(context.Background(), baseRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if got := p.Programs.Len(); got != 1 {
		t.Errorf("编译缓存里有 %d 条，期望 1", got)
	}
}
