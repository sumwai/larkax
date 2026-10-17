package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/tokens"
)

// adoptTestLogger 把日志器装成进程默认，并在用例结束时还原。
//
// 结算里的告警走 observe.FromContext，而它的日志器**只**来自 slog 的包级
// 默认（见 observe.AdoptAsDefault）——往 context 里塞日志器那条路已经没有了。
func adoptTestLogger(t *testing.T, w io.Writer) {
	t.Helper()
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	observe.AdoptAsDefault(observe.NewLoggerWithWriter(w, "debug", "json", false))
}

// ruleSource 是测试用的规则来源：一条固定规则，或一个错误。
//
// 两个方法与真实实现（pricing）同形：Match 给结算那一次，返回**一条**
// 规则；MatchAll 给预留那一次，按候选逐条返回。分开是因为两者的输入
// 本来就不一样——结算只有一个已选定的渠道，预留是一整个候选集。
type ruleSource struct {
	// rule 是没在 byChannel 里指定的渠道所适用的那一条（平台规则）。
	rule Rule
	err  error
	// byChannel 按渠道指定规则，让结算那一次按**实际选中的渠道**取到
	// 与别的候选不同的规则。真实实现就是这个形状：渠道级规则盖过平台
	// 规则，而结算必须按选定的那个渠道算。
	byChannel map[int64]Rule
	// all 直接指定 MatchAll 的返回值，用于「一个候选都匹配不上」这类
	// 与具体渠道无关的情形。没配时按候选逐个取。
	all []Rule
	// queries 记录每次匹配的条件（两个方法都记），用于断言
	// 「结算用的是预留时的时刻」以及「预留拿到了整个候选集」。
	queries []RuleQuery
}

// ruleFor 取某个渠道适用的规则。
func (r *ruleSource) ruleFor(channelID int64) Rule {
	if rule, ok := r.byChannel[channelID]; ok {
		return rule
	}
	return r.rule
}

func (r *ruleSource) Match(_ context.Context, q RuleQuery) (Rule, error) {
	r.queries = append(r.queries, q)
	if r.err != nil {
		return Rule{}, r.err
	}
	if len(q.Candidates) != 1 {
		return Rule{}, fmt.Errorf("测试替身只接受恰好一个候选，得到 %d 个", len(q.Candidates))
	}
	return r.ruleFor(q.Candidates[0].ChannelID), nil
}

func (r *ruleSource) MatchAll(_ context.Context, q RuleQuery) ([]Rule, error) {
	r.queries = append(r.queries, q)
	if r.err != nil {
		return nil, r.err
	}
	if r.all != nil {
		return r.all, nil
	}
	out := make([]Rule, 0, len(q.Candidates))
	for _, c := range q.Candidates {
		out = append(out, r.ruleFor(c.ChannelID))
	}
	return out, nil
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
		// 资产品种由入口从运行期配置填（见 Request.Asset），本包不再兜默认值。
		Asset:       "CNY",
		Candidates:  []RuleCandidate{{ChannelID: 7, OwnerUserID: 9}},
		ChannelID:   7,
		OwnerUserID: 9,
		InputTokens: 1000,
		MaxTokens:   500,
		// 协议由入口填（见 Request.Protocol）。夹具给一条具体的：capped
		// 计数器按它分序列，空值会造出一条生产上不该有的序列。
		Protocol: channel.ProtocolOpenAI,
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

// ── 预留的上界要覆盖整个候选集 ──────────────────────────────────────────
//
// 预留发生在选路之前，所以「会按哪条规则计价」还没有答案，手上只有一个
// 候选集。只按平台规则算会让预留小于结算，而结算撞上 Capped 之后实际
// 金额被预留截住、差额由平台吃掉，**账目仍然是平的**——没有任何东西
// 会报出来。下面几条钉的就是这个。

// platformRule 便宜、候选里的渠道规则贵：预留必须按贵的那条算。
//
// 这一条是本文件里最要紧的断言。把它按老口径（只问平台规则）跑一遍，
// 预留会算成 0.002 而结算要 1.4，Capped 触发、实际金额被截成 0.002。
func TestReserveTakesUpperBoundAcrossCandidateRules(t *testing.T) {
	platformRule := Rule{ID: 10, Version: 1, Source: `{"prompt": 0.000001, "completion": 0.000002}`}
	channelRule := Rule{ID: 20, Version: 1, Source: `{"prompt": 0.001, "completion": 0.002}`}

	// 两个候选：渠道 7 有自己的渠道规则，渠道 8 退到平台规则。
	// 结算会选渠道 7，所以 Match 给的也是渠道规则。
	src := &ruleSource{
		rule:      platformRule,
		byChannel: map[int64]Rule{7: channelRule},
	}
	p := newPipeline(t, src)

	req := baseRequest()
	req.Candidates = []RuleCandidate{{ChannelID: 7, OwnerUserID: 9}, {ChannelID: 8, OwnerUserID: 9}}

	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 输入 1000 × 1e-3 = 1；输出 500 × 2e-3 = 1 → 2
	if got := res.Amount.String(); got != "2.00000000" {
		t.Errorf("预留 = %s，期望 2.00000000（按候选里最贵的那条规则算，而不是平台价）", got)
	}
	// 记下来的依据要是**给出最大金额的那一条**：界面靠它解释这笔钱怎么估的。
	if res.RuleID != channelRule.ID {
		t.Errorf("规则依据 = %d，期望给出最大金额的 %d（平台那条是 %d）",
			res.RuleID, channelRule.ID, platformRule.ID)
	}
	if res.PricingExprSnap != channelRule.Source {
		t.Errorf("表达式快照 = %q，期望最贵那条的 %q", res.PricingExprSnap, channelRule.Source)
	}

	// 结算按实际选中的渠道 7：1000 × 1e-3 + 200 × 2e-3 = 1.4。
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 200},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}
	if got := charge.Settlement.Actual.String(); got != "1.40000000" {
		t.Errorf("实际金额 = %s，期望 1.40000000", got)
	}
	// 这一条就是「预留是上界」：撞上 Capped 说明上界算小了。
	if charge.Settlement.Capped {
		t.Errorf("撞上 Capped：预留 %s 小于结算，差额被平台吃掉且账目仍然平",
			res.Amount)
	}
}

// 多个候选、规则价各不相同时取的是**最大**，不是第一个。
//
// 最贵的那条故意排在最后：按第一个候选算只会拿到 0.001。
func TestReserveTakesMaxNotFirstCandidate(t *testing.T) {
	cheap := Rule{ID: 1, Version: 1, Source: `{"prompt": 0.000001}`}
	middle := Rule{ID: 2, Version: 1, Source: `{"prompt": 0.000002}`}
	dear := Rule{ID: 3, Version: 1, Source: `{"prompt": 0.000005}`}

	src := &ruleSource{byChannel: map[int64]Rule{7: cheap, 8: middle, 9: dear}}
	p := newPipeline(t, src)

	req := baseRequest()
	req.Candidates = []RuleCandidate{
		{ChannelID: 7, OwnerUserID: 9},
		{ChannelID: 8, OwnerUserID: 9},
		{ChannelID: 9, OwnerUserID: 9},
	}

	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 输入 1000 × 5e-6 = 0.005
	if got := res.Amount.String(); got != "0.00500000" {
		t.Errorf("预留 = %s，期望 0.00500000（三条里最贵的那条）", got)
	}
	if res.RuleID != dear.ID {
		t.Errorf("规则依据 = %d，期望最贵的 %d", res.RuleID, dear.ID)
	}
}

// 预留要把**整个候选集**交给规则来源，每个候选带自己的商家。
//
// 商家级规则按 owner 匹配，只给渠道 id 是不够的——漏掉一条更贵的商家价
// 正是预留算小的来源。
func TestReservePassesCandidatesWithOwners(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	req.Candidates = []RuleCandidate{{ChannelID: 7, OwnerUserID: 11}, {ChannelID: 8, OwnerUserID: 22}}

	if _, err := p.Reserve(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(src.queries) != 1 {
		t.Fatalf("规则匹配 %d 次，期望 1 次", len(src.queries))
	}
	got := src.queries[0].Candidates
	if len(got) != 2 {
		t.Fatalf("候选 = %+v，期望 2 个", got)
	}
	if got[0].ChannelID != 7 || got[0].OwnerUserID != 11 {
		t.Errorf("第一个候选 = %+v，期望渠道 7 商家 11", got[0])
	}
	if got[1].ChannelID != 8 || got[1].OwnerUserID != 22 {
		t.Errorf("第二个候选 = %+v，期望渠道 8 商家 22", got[1])
	}
}

// 候选集为空时**拒绝**，而不是退回去按平台规则算。
//
// 空候选集算不出上界：候选里的规则可能一条比一条贵，按空集算出来的任何
// 数都是下界。退回平台规则正是「静默少收」那个毛病。
func TestReserveRejectsEmptyCandidates(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	req.Candidates = nil

	_, err := p.Reserve(context.Background(), req)
	if !errors.Is(err, ErrNoRule) {
		t.Fatalf("err = %v，期望 ErrNoRule", err)
	}
	// 连规则来源都不该问：问了也只会得到「平台那条」，而那正是要避免的。
	if len(src.queries) != 0 {
		t.Errorf("候选集为空时不该去匹配规则，实际匹配 %d 次", len(src.queries))
	}
}

// 候选都在，但一条适用的规则都没有：同样是算不出上界，不是零价。
func TestReserveRejectsWhenNoCandidateHasRule(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}, all: []Rule{}})

	_, err := p.Reserve(context.Background(), baseRequest())
	if !errors.Is(err, ErrNoRule) {
		t.Errorf("err = %v，期望 ErrNoRule", err)
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
	// 预留 0.002，实收 0.0014，差额 0.0006 退回
	if charge.Settlement.Reserved.String() != "0.00200000" {
		t.Errorf("预留 = %s，期望 0.002", charge.Settlement.Reserved)
	}
	if len(charge.Entries) == 0 {
		t.Fatal("应生成账本分录")
	}
	if charge.UsageSource != tokens.SourceMeasured {
		t.Errorf("用量来源 = %v", charge.UsageSource)
	}
}

// 耗时从 Outcome 原样搬到 Charge：它是 usage_events.duration_ms 的唯一来源。
//
// # 为什么这一条值得单独钉
//
// 耗时与金额无关，漏搬不会让任何计费用例变红：账单金额照旧正确、分录照旧
// 平衡，只是渠道健康度的延迟那一维永远没有样本。那正是「三列恒为 0、
// 权重写着在起作用而实际是常数」这条缺陷的形状。
//
// 零值也要原样搬：它到落库层才变成 NULL，本层不能把「没测到」改成 0。
func TestSettleCarriesOutcomeDuration(t *testing.T) {
	for _, want := range []time.Duration{1500 * time.Millisecond, 0} {
		t.Run(want.String(), func(t *testing.T) {
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
				Duration:    want,
			})
			if err != nil {
				t.Fatalf("结算出错: %v", err)
			}
			if charge.Duration != want {
				t.Errorf("Charge.Duration = %s，期望 %s", charge.Duration, want)
			}
		})
	}
}

// 五项 token 单价各给一个不同的价，再加一个非 token 计量项。
//
// 五项都不一样，是为了让「抄串了哪一项」在断言里看得出来；非 token 那一项
// 走的是另一条路（Prices.Units），单独一个价才能证明它也被搬过来了。
const fiveSlotRule = `{"prompt": 0.000001, "completion": 0.000002, ` +
	`"cache_read": 0.0000001, "cache_write": 0.00000125, "reasoning": 0.000004, ` +
	`"units": {"searches": 0.01}}`

// 单价从这次求值的结果搬到 Charge：它是 usage_events.unit_prices 唯一的来源。
//
// # 为什么这一条值得单独钉
//
// 单价与金额无关，漏搬不会让任何计费用例变红：账单金额照旧正确、分录照旧
// 平衡，只是「这笔钱是按什么单价算出来的」事后无从查起——正是
// TestSettleCarriesOutcomeDuration 那一条的形状（少一个字段，而所有金额类
// 断言都不动）。
//
// 只写规则**显式写过**的那几项也一并钉住（Declared）：没写的槽位单价是零值，
// 而「写了 0」（这一项免费）与「没写」在 money.Amount 上分不出来，落库层靠
// Declared 区分（见 Charge.Prices 与 insertUsageEventSQL）。
func TestSettleCarriesPricesFromEvaluation(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: fiveSlotRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{
			PromptTokens:     1000,
			CompletionTokens: 200,
			CacheReadTokens:  10,
			CacheWriteTokens: 20,
			ReasoningTokens:  5,
			UnitCounts:       map[string]int64{"searches": 2},
		},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	// 逐项比字面量，不拿 Breakdown 反推：反推要除、要猜零用量那一项，
	// 而这条路径要的是「求值给出的价原样到了这里」。
	for _, tc := range []struct {
		slot string
		got  string
		want string
	}{
		{"prompt", charge.Prices.Prompt.String(), "0.00000100"},
		{"completion", charge.Prices.Completion.String(), "0.00000200"},
		{"cache_read", charge.Prices.CacheRead.String(), "0.00000010"},
		{"cache_write", charge.Prices.CacheWrite.String(), "0.00000125"},
		{"reasoning", charge.Prices.Reasoning.String(), "0.00000400"},
		{"units.searches", charge.Prices.Units["searches"].String(), "0.01000000"},
	} {
		if tc.got != tc.want {
			t.Errorf("Prices[%s] = %s，期望 %s", tc.slot, tc.got, tc.want)
		}
	}

	// 这五项规则都写过，所以五个槽位都该是「声明过」的。落库层据此决定把哪些
	// 键写进 unit_prices——少一个声明就少一个键，而账上看起来像「当时没给这
	// 个价」。
	for _, slot := range billing.TokenSlots {
		if !charge.Prices.Declared[slot.Name] {
			t.Errorf("槽位 %q 没有被标成「规则写过」——它的单价不会进 unit_prices", slot.Name)
		}
	}
}

// 撞上预留上界的结算：单价不受影响，它本来就是价格侧的数。
//
// # 这一条防的是什么
//
// SettleWithCharges 撞上界时把 Settlement.Actual 截成预留额，而 Breakdown 留
// 的是截之前的金额（有意如此，见 internal/billing 的 TestSettleCapsOvershoot）。
// 于是同一次结算里出现两个金额，而**都不该变成单价**：从金额反推出来的价会
// 随 Actual 一起被截，复算时算出的金额小于 cost，看起来却像个正常的价。
//
// 这里把三个数摆在一起：Actual 是被预留截住之后的值（等于 Reserved），
// Breakdown.Prompt 是没截的 0.1，而 Prices.Prompt 是规则给的 0.000001
// （与两者都不是一回事）。
func TestSettleUnitPricesAreUnaffectedByCappedSettlement(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: fiveSlotRule}}
	p := newPipeline(t, src)

	res, err := p.Reserve(context.Background(), baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	// 上游报 100000 个输入 token，按 1e-6 算是 0.1——远超按最坏情况算出的预留。
	capped := baseRequest()
	charge, err := p.Settle(context.Background(), capped, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 100_000},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	if !charge.Settlement.Capped {
		t.Fatalf("这次结算没有撞上界（按用量算是 %s，预留 %s）——用例的前提不成立",
			charge.Settlement.Breakdown.Total, charge.Settlement.Reserved)
	}
	// 截住的意思是「按预留收」，所以 Actual 与 Reserved 是同一个数。不写死
	// 那个字面量：它是 Reserve 的最坏情况算术，由另一条用例管。
	if got := charge.Settlement.Actual; got.Cmp(charge.Settlement.Reserved) != 0 {
		t.Errorf("Actual = %s，期望等于预留 %s——撞上界时按预留收", got, charge.Settlement.Reserved)
	}
	if got := charge.Settlement.Breakdown.Prompt.String(); got != "0.10000000" {
		t.Errorf("Breakdown.Prompt = %s，期望 0.1（明细留的是截之前的金额）", got)
	}
	if got := charge.Prices.Prompt.String(); got != "0.00000100" {
		t.Errorf("Prices.Prompt = %s，期望 0.000001（规则给的单价，与上面两个金额都不是一回事）", got)
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
	var uf *UpstreamFailure
	if !errors.As(err, &uf) {
		t.Fatalf("应是 UpstreamFailure，得到 %T", err)
	}
	if uf.Status != 503 || uf.Code != "upstream_unavailable" {
		t.Errorf("失败依据没带上: %+v", uf)
	}
}

// 撞上预留上界时，那条按协议分的观测必须真的被记一次——它是「撞了」在账目
// 之外唯一的信号（账目仍然是平的，见 observe.CappedSettlements 的注释）。
//
// 断言落在**增量**上而不是绝对值：observe.Metrics 是进程级的一份，别的用例
// 也在往上面加数。四条协议各取一次差，同时钉住两件事：
//
//   - 「记了」：撞上界的那条协议的计数 +1；
//   - 「记对了序列」：没撞的协议一个都不动。只断言总数的话，把标签写死成
//     任何一条协议、或者干脆不按协议分，用例都照样通过——而那正是这一轮
//     要防的（各条入口的上界来源不同，混在一起分不出是哪一条在漏）。
func TestSettleCountsCappedSettlementPerProtocol(t *testing.T) {
	protocols := []channel.Protocol{
		channel.ProtocolOpenAI,
		channel.ProtocolAnthropic,
		channel.ProtocolOpenAIResponses,
		channel.ProtocolSystemOne,
	}
	// 先把四条序列都取一遍（值为 0 也会建出来），之后才读得到干净的起点。
	before := map[channel.Protocol]uint64{}
	for _, protocol := range protocols {
		before[protocol] = observe.Metrics.CappedSettlements.With(string(protocol)).Value()
	}

	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	// 一次**撞上界**的结算：预留按 1000 输入 + 500 输出的最坏情况算出
	// 0.002，而上游报 100000 个输入 token，按 1e-6 算是 0.1。
	capped := baseRequest()
	capped.RequestID = "req-capped-systemone"
	capped.Protocol = channel.ProtocolSystemOne
	res, err := p.Reserve(context.Background(), capped)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := p.Settle(context.Background(), capped, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 100_000},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}
	if !charge.Settlement.Capped {
		t.Fatalf("这次结算没有撞上界（按用量算是 %s，预留 %s）——用例的前提不成立",
			charge.Settlement.Breakdown.Total, charge.Settlement.Reserved)
	}

	// 一次**没撞**的结算，走另一条协议：它不该动任何 capped 序列。
	plain := baseRequest()
	plain.RequestID = "req-plain-openai"
	plain.Protocol = channel.ProtocolOpenAI
	res, err = p.Reserve(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Settle(context.Background(), plain, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 200},
		UsageSource: tokens.SourceMeasured,
	}); err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	wantSystemOne := before[channel.ProtocolSystemOne] + 1
	if got := observe.Metrics.CappedSettlements.With(string(channel.ProtocolSystemOne)).Value(); got != wantSystemOne {
		t.Errorf("systemone 的撞上界计数 = %d，期望 %d——撞了却没记，事后从账上看不出来",
			got, wantSystemOne)
	}
	for _, protocol := range protocols {
		if protocol == channel.ProtocolSystemOne {
			continue
		}
		if got := observe.Metrics.CappedSettlements.With(string(protocol)).Value(); got != before[protocol] {
			t.Errorf("%s 的撞上界计数 = %d，期望不变（%d）——没撞上界的协议不该被记，也不该被记到别的协议上",
				protocol, got, before[protocol])
		}
	}

	// 渲染一遍，证明这一条序列真的会出现在 /metrics 上，而不是只活在进程里。
	var buf bytes.Buffer
	if err := observe.Metrics.Render(&buf); err != nil {
		t.Fatalf("渲染指标失败: %v", err)
	}
	want := fmt.Sprintf(`larkax_billing_capped_settlements_total{protocol="systemone"} %d`, wantSystemOne)
	if !strings.Contains(buf.String(), want) {
		t.Errorf("/metrics 的输出里没有 %s\n实际输出:\n%s", want, buf.String())
	}
	t.Logf("撞上界被记下的那一条: %s", want)
}

// 客户端在流式响应中途断开**要计费**：已经产生的 token 上游是算了钱的。
func TestSettleClientCancelStillBills(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, _ := p.Reserve(context.Background(), req)

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		ClientCanceled: true,
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
	// 「客户端断的」这件事本身也要跟着账单走：它是
	// billing_requests.client_canceled_at 的唯一来源，而那一列是事后回答
	// 「这个渠道是不是经常被客户端掐断」的依据。
	if !charge.ClientCanceled {
		t.Error("客户端取消没落进账单：client_canceled_at 会永远是 NULL")
	}
}

// 上游正常返回的结算**不能**标成客户端取消。
//
// 两个方向都要钉：只钉「取消时为真」的话，把这一列写成恒真也能通过，
// 而恒真会让每一笔账单都记上「客户端断的」——按渠道统计掐断率时，
// 一个健康的渠道看起来像是最差的那个。
func TestSettleUpstreamSuccessIsNotClientCanceled(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	res, _ := p.Reserve(context.Background(), req)

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 50},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("正常结算失败: %v", err)
	}
	if charge.ClientCanceled {
		t.Error("上游正常返回被标成了客户端取消：client_canceled_at 会被写成非 NULL")
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
	// 结算把候选集收窄成**实际选中的那一个渠道**：候选里挑一条规则是预留
	// 的事，结算只能按选定的那个渠道算。
	if got := src.queries[1].Candidates; len(got) != 1 || got[0].ChannelID != 7 || got[0].OwnerUserID != 9 {
		t.Errorf("结算的候选 = %+v，期望恰好一个：渠道 7 商家 9", got)
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

	// 结算按**受理那一刻**（上海 02:00，低谷档）算，而不是结算时的墙钟：
	// 这里把墙钟拨到白天，档位边界已经跨过去了。
	day := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC) // 上海 12:00
	p.Now = func() time.Time { return day }
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 1000 × 1e-6（低谷档）= 0.001，而不是白天档的 0.002
	if got := charge.Settlement.Actual.String(); got != "0.00100000" {
		t.Errorf("实际金额 = %s，期望 0.00100000（按受理那一刻的低谷档）", got)
	}
	if charge.Settlement.Capped {
		t.Errorf("结算撞上了 Capped：预留 %s 实际 %s",
			res.Amount, charge.Settlement.Breakdown.Total)
	}
}

// ── 价格在受理那一刻定下来 ──────────────────────────────────────────────
//
// 一次请求的价格由它**被受理的那一刻**决定：那一刻的钟点与日历日选出规则里
// 的哪一档，而档位决定单价。上游那次调用可能跑很久、跨过档位边界，但结算
// 复用的是预留记下的受理时刻，不重新读钟（见 pipeline.env）。
//
// 下面几条都让受理与结算分处规则的两个档位，看结算按哪一档算，每条都断言
// 具体金额。只断言「没撞 Capped」是不够的：预留取的是最坏情况，它本来就
// 大于任何一个档位的价，所以撞不撞 Capped 与用了哪一档无关。

// hourTierRule 按上海钟点分档：09:00 之前低谷，之后高峰。两档差四倍，
// 所以「用了哪一档」在金额上直接看得出来。档位名也带上，好让断言不必
// 靠金额反推。
const hourTierRule = `hour("Asia/Shanghai") < 9 ? tier("off_peak", {"prompt": 0.000001}) : tier("peak", {"prompt": 0.000004})`

// shanghaiAt 造一个上海当地时刻。
func shanghaiAt(t *testing.T, y int, mo time.Month, d, h, mi int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// 镜像里没装 tzdata 时它才会失败，而那时规则里的
		// hour("Asia/Shanghai") 同样会失败。报出来，别让用例静默跳过。
		t.Fatalf("加载时区失败: %v", err)
	}
	return time.Date(y, mo, d, h, mi, 0, 0, loc)
}

// 08:59 受理、09:01 结算：按受理那一刻的档位计价。
//
// 把 Settle 那一步换回读墙钟（p.env(usage, p.now())），这里拿到的会是
// 高峰价 0.004 而不是低谷价 0.001，用例即挂。
func TestSettlePricesAtAcceptanceInstant(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: hourTierRule}})

	accepted := shanghaiAt(t, 2026, 10, 16, 8, 59)
	p.Now = func() time.Time { return accepted }

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}
	// 预留取最坏情况：两档里贵的 4e-6 × 1000 = 0.004
	if got := res.Amount.String(); got != "0.00400000" {
		t.Fatalf("预留 = %s，期望 0.00400000（最坏情况取高峰档）", got)
	}
	if !res.RuleMatchedAt.Equal(accepted) {
		t.Fatalf("受理时刻 = %v，期望 %v", res.RuleMatchedAt, accepted)
	}

	// 结算时墙钟已经走过 09:00 这个档位边界。
	p.Now = func() time.Time { return shanghaiAt(t, 2026, 10, 16, 9, 1) }

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	// 按受理那一刻（09:00 之前）的档位：1000 × 1e-6 = 0.001
	if got := charge.Settlement.Actual.String(); got != "0.00100000" {
		t.Errorf("实际金额 = %s，期望 0.00100000（受理那一刻的低谷档，而不是结算时刻的高峰档 0.004）", got)
	}
	if charge.Tier != "off_peak" {
		t.Errorf("档位 = %q，期望 off_peak", charge.Tier)
	}
	if !charge.PricedAt.Equal(accepted) {
		t.Errorf("PricedAt = %v，期望受理时刻 %v", charge.PricedAt, accepted)
	}

	// 不变式：预留（最坏情况）≥ 结算（受理那一刻那一档）。
	if res.Amount.Cmp(charge.Settlement.Actual) < 0 {
		t.Errorf("预留 %s 小于结算 %s", res.Amount, charge.Settlement.Actual)
	}
	if charge.Settlement.Capped {
		t.Errorf("撞上 Capped：预留 %s 实际 %s", res.Amount, charge.Settlement.Actual)
	}
}

// 跨日：23:59 受理、次日 00:01 结算。受理那一刻是 23 点（高峰），
// 而结算那一刻是次日 0 点（低谷）——按前者算。
func TestSettlePricesAtAcceptanceAcrossMidnight(t *testing.T) {
	const rule = `hour("Asia/Shanghai") >= 8 ? tier("peak", {"prompt": 0.000004}) : tier("off_peak", {"prompt": 0.000001})`
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: rule}})

	accepted := shanghaiAt(t, 2026, 10, 16, 23, 59)
	p.Now = func() time.Time { return accepted }

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}
	if got := res.Amount.String(); got != "0.00400000" {
		t.Fatalf("预留 = %s，期望 0.00400000", got)
	}

	p.Now = func() time.Time { return shanghaiAt(t, 2026, 10, 17, 0, 1) }

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 1000},
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}
	if got := charge.Settlement.Actual.String(); got != "0.00400000" {
		t.Errorf("实际金额 = %s，期望 0.00400000（受理那一刻的 23 点档，而不是次日 0 点档的 0.001）", got)
	}
	if charge.Tier != "peak" {
		t.Errorf("档位 = %q，期望 peak", charge.Tier)
	}
	if !charge.PricedAt.Equal(accepted) {
		t.Errorf("PricedAt = %v，期望受理时刻 %v", charge.PricedAt, accepted)
	}
	if res.Amount.Cmp(charge.Settlement.Actual) < 0 {
		t.Errorf("预留 %s 小于结算 %s", res.Amount, charge.Settlement.Actual)
	}
}

// fakeCalendar 是一份写死的日历：时区固定，休息日由 offDays 指定。
//
// 用写死的表而不是真日历数据：这条用例要钉的是「按哪一天判档」，
// 而真日历会随节假日安排变。
type fakeCalendar struct {
	tz      string
	offDays map[string]bool
}

func (c *fakeCalendar) TimeZone(string) (string, error) { return c.tz, nil }

func (c *fakeCalendar) DayInfo(_ string, day time.Time) (expr.DayInfo, error) {
	return expr.DayInfo{OffDay: c.offDays[day.Format("2006-01-02")]}, nil
}

// 跨「工作日 / 休息日」边界：周五 23:59 受理、周六 00:01 结算，
// 按受理那一刻（周五，工作日）的档位算。
//
// 这一条与上一条不同：换的是**日历日**，而档位由 isWorkday() 决定——
// 同一段墙钟时间里，判档依据从「哪一天」来，而不只是「几点」。
func TestSettlePricesAtAcceptanceAcrossOffDayBoundary(t *testing.T) {
	const rule = `isWorkday() ? tier("workday", {"prompt": 0.000001}) : tier("rest_day", {"prompt": 0.000004})`
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: rule}},
		func(p *Pipeline) {
			p.Region = "CN"
			p.Calendar = &fakeCalendar{
				tz: "Asia/Shanghai",
				// 2026-10-16 是周五，10-17 是周六。
				offDays: map[string]bool{"2026-10-17": true},
			}
		})

	accepted := shanghaiAt(t, 2026, 10, 16, 23, 59)
	p.Now = func() time.Time { return accepted }

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}
	// 最坏情况：两档里贵的 4e-6 × 1000 = 0.004
	if got := res.Amount.String(); got != "0.00400000" {
		t.Fatalf("预留 = %s，期望 0.00400000", got)
	}

	// 结算时已经是周六（休息日）。
	p.Now = func() time.Time { return shanghaiAt(t, 2026, 10, 17, 0, 1) }

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 1000},
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}
	// 按受理那一刻（周五，工作日）：1000 × 1e-6 = 0.001
	if got := charge.Settlement.Actual.String(); got != "0.00100000" {
		t.Errorf("实际金额 = %s，期望 0.00100000（受理那一刻是工作日，而不是结算时的休息日 0.004）", got)
	}
	if charge.Tier != "workday" {
		t.Errorf("档位 = %q，期望 workday", charge.Tier)
	}
	if !charge.PricedAt.Equal(accepted) {
		t.Errorf("PricedAt = %v，期望受理时刻 %v", charge.PricedAt, accepted)
	}
	if res.Amount.Cmp(charge.Settlement.Actual) < 0 {
		t.Errorf("预留 %s 小于结算 %s", res.Amount, charge.Settlement.Actual)
	}
}

// 预留里没有受理时刻时**拒绝**，而不是退回去读墙钟。
//
// 兜一个 p.now() 上去会让跨档位的请求按另一档计价，而账单上看不出
// 用的是哪个时刻——这正是本次要消除的口径。
func TestSettleRejectsReservationWithoutInstant(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})
	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	res.RuleMatchedAt = time.Time{}
	_, err = p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{PromptTokens: 100},
	})
	if err == nil {
		t.Fatal("预留缺少受理时刻时应报错")
	}
	var uf *UpstreamFailure
	if errors.As(err, &uf) {
		t.Errorf("这不该被当成「本次不计费」: %v", err)
	}
}

// 上面那条与「上游失败不计费」撞在一起时，后者胜出。
//
// 次序是那条硬规则的一部分：一次不计费的请求，定价基准缺不缺都不该改变结论。
// 两条检查反过来写的话，一次上游失败会被降级成一句普通错误，而调用方正是靠
// `*UpstreamFailure` 认出「这笔钱放回去还是收掉」——认不出来的后果是那笔
// 冻结的钱不再被当成「上游失败」来处理。
//
// 两个条件同时出现是**测试造出来的**（真实路径里 RuleMatchedAt 不可能为零，
// 见 Settle 的注释），所以这条用例钉的是次序本身，不是一条会走到的路径。
func TestSettleUpstreamFailureOutranksMissingInstant(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})
	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	res.RuleMatchedAt = time.Time{}
	_, err = p.Settle(context.Background(), req, res, Outcome{
		UpstreamFailed: true,
		UpstreamStatus: 503,
		ErrorCode:      "upstream_unavailable",
	})
	var uf *UpstreamFailure
	if !errors.As(err, &uf) {
		t.Fatalf("err = %v，期望仍被当成「上游失败不计费」", err)
	}
}

// 资产品种由调用方给，本包不兜默认值。
//
// 原先这里断言「不给就是 CNY」。那个默认值把「入口忘了填」伪装成一笔
// 人民币分录——与正常的完全一样，而库里那份配置改了它也不会跟着动。
// 现在不给就报错，给的照原样用。
func TestAssetIsRequiredAndPassedThrough(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	req.Asset = ""
	_, err := p.Reserve(context.Background(), req)
	if err == nil {
		t.Fatal("不给资产品种应当报错，而不是回落到某个默认值")
	}
	if !strings.Contains(err.Error(), "资产品种") {
		t.Errorf("错误信息该说清缺的是什么: %v", err)
	}

	req.Asset = "USD"
	res, err := p.Reserve(context.Background(), req)
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

// concurrentSource 是并发安全的规则来源：每次调用返回同一批规则，
// 不记录任何东西。
//
// 不复用 ruleSource：它把每次查询 append 进 queries，那个 append 自己
// 就是一处竞争。用它测并发，报出来的会是夹具的竞争而不是被测代码的。
type concurrentSource struct{ rules []Rule }

func (s concurrentSource) MatchAll(context.Context, RuleQuery) ([]Rule, error) {
	return s.rules, nil
}

func (s concurrentSource) Match(_ context.Context, _ RuleQuery) (Rule, error) {
	return s.rules[0], nil
}

// 同一个 Pipeline 被并发共用时不该有数据竞争。
//
// Pipeline 由 gateway 单实例共用，所以 Reserve 会在多个 goroutine 上
// 同时跑。编译缓存的**懒初始化**曾经在无锁情况下写 Programs 这个导出
// 字段：两个 goroutine 同时进来，就是同一块内存上的无同步读写。
//
// 判据是 `go test -race` 报不报 DATA RACE。竞争与「金额算得对不对」
// 是两件事——这里的断言只看调用成功，因为竞争恰恰不影响返回值。
func TestReserveConcurrent(t *testing.T) {
	p := &Pipeline{
		Rules: concurrentSource{rules: []Rule{{ID: 1, Version: 1, Source: simpleRule}}},
		Now:   func() time.Time { return fixedAt },
	}

	const callers = 2
	// start 让两个 goroutine 尽量同时进入 Reserve：竞争要两边都走到
	// 那一行才报得出来。
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := p.Reserve(context.Background(), baseRequest()); err != nil {
				t.Errorf("并发预留失败: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
}

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 来自变异测试（gremlins unleash）：把比较符改一下，测试仍然通过。
// 覆盖率看不出来——行被执行过了，但边界那一支没被走到。

// UpstreamFailure 的错误文本要能区分「有错误码」与「只有状态码」。
//
// 变异：把 `e.Code != ""` 改成 `== ""`，两者会互换，
// 而原先只断言了「错误不为空」。
func TestUpstreamFailureErrorText(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *UpstreamFailure
		want string
	}{
		{"有错误码", &UpstreamFailure{Status: 503, Code: "upstream_unavailable"}, "upstream_unavailable"},
		{"只有状态码", &UpstreamFailure{Status: 429}, "429"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			if !strings.Contains(got, tc.want) {
				t.Errorf("错误文本 %q 应含 %q", got, tc.want)
			}
			// 两种情况都必须说明「不计费」
			if !strings.Contains(got, "不计费") {
				t.Errorf("错误文本 %q 应说明不计费", got)
			}
		})
	}
}

// max_tokens 恰好为 0 与负数都要被拒——它是上界，必须是正的。
//
// 变异：`MaxTokens <= 0` 改成 `< 0`，0 就被放行，
// 而 0 意味着「最多写 0 个 token」，算不出预留上界。
func TestMaxTokensBoundary(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})

	for _, mt := range []int64{0, -1} {
		req := baseRequest()
		req.MaxTokens = mt
		if _, err := p.Reserve(context.Background(), req); err == nil {
			t.Errorf("max_tokens = %d 应被拒绝", mt)
		}
	}

	// 恰好为 1 应当被接受
	req := baseRequest()
	req.MaxTokens = 1
	if _, err := p.Reserve(context.Background(), req); err != nil {
		t.Errorf("max_tokens = 1 应被接受: %v", err)
	}
}

// 输入 token 恰好为 0 是合法的（例如只有工具调用的请求）。
//
// 变异：`InputTokens < 0` 改成 `<= 0`，输入为 0 会被误拒。
func TestInputTokensZeroIsAllowed(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}})
	req := baseRequest()
	req.InputTokens = 0

	if _, err := p.Reserve(context.Background(), req); err != nil {
		t.Errorf("输入 token 为 0 应当被接受: %v", err)
	}
	// 负数仍要拒
	req.InputTokens = -1
	if _, err := p.Reserve(context.Background(), req); err == nil {
		t.Error("负的输入 token 应被拒绝")
	}
}

// ── 上游用量自相矛盾时的告警 ────────────────────────────────────────────

// reasoningRule 给推理单独定价，这样「同一批 token 收两次」才在金额上看得出来。
const reasoningRule = `{"prompt": 0.000001, "completion": 0.000002, "reasoning": 0.00001}`

// 上游报「输出 50、其中推理 80」时：**照常计费，只写一条告警**。
//
// 不让它失败是有意的。这是运行时来自上游的数据状况，不是几分钟能修好的配置
// 错误：让它失败会把整条渠道的请求都变成失败，而上游那次调用已经成功、
// token 已经花掉，客户端还会拿到错误——白服务一次还赔上体验。
func TestSettleWarnsButSucceedsOnContradictoryReasoning(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: reasoningRule}})
	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	adoptTestLogger(t, &buf)

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage: channel.Usage{
			PromptTokens:                1000,
			CompletionTokens:            50,
			ReasoningTokens:             80,
			CompletionIncludesReasoning: true,
		},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("自相矛盾的用量不该让结算失败: %v", err)
	}

	// 拆出来为负，钳到 0：不能算出一笔负数消费。推理照上游报的算。
	// 1000*1e-6 + 0*2e-6 + 80*1e-5 = 0.001 + 0.0008
	if charge.Usage.CompletionTokens != 0 {
		t.Errorf("输出应被钳到 0，得到 %d", charge.Usage.CompletionTokens)
	}
	if charge.Usage.ReasoningTokens != 80 {
		t.Errorf("推理 = %d，期望 80", charge.Usage.ReasoningTokens)
	}
	if got := charge.Settlement.Actual.String(); got != "0.00180000" {
		t.Errorf("实际金额 = %s，期望 0.0018", got)
	}

	// 告警要带够上下文：哪条渠道、上游原样报的两个数。
	logged := buf.String()
	for _, want := range []string{"channel_id", "7", "completion_tokens", "50", "reasoning_tokens", "80"} {
		if !strings.Contains(logged, want) {
			t.Errorf("告警里缺少 %q: %s", want, logged)
		}
	}
}

// 正常的用量不该产生任何告警——否则那条告警等于噪声，
// 而噪声里的真信号没人会看。
func TestSettleDoesNotWarnOnConsistentReasoning(t *testing.T) {
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: reasoningRule}})
	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	adoptTestLogger(t, &buf)

	// 两种口径各一次：推理含在输出里，以及推理独立报。
	for _, u := range []channel.Usage{
		{CompletionTokens: 100, ReasoningTokens: 40, CompletionIncludesReasoning: true},
		{CompletionTokens: 100, ReasoningTokens: 40},
	} {
		if _, err := p.Settle(context.Background(), req, res, Outcome{Usage: u}); err != nil {
			t.Fatalf("结算出错: %v", err)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("自洽的用量不该产生日志: %s", buf.String())
	}
}
