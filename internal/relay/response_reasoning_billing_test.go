package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// 本文件从「渠道声明」一路算到「账上落的那笔钱」，钉住口径对金额的影响。
//
// # 与同目录另一份用例的分工
//
// response_usage_declaration_test.go 也断金额（TestDeepSeekUsageAmountUnderBothDeclarations
// 就断 0.04333650 / 0.04320150），但它直接调 billing.ComputeWithCharges 算明细，
// 只看 Settlement 之外的产物。这一份走 **pipeline 的预留 → 结算**，多断两样
// 那一条碰不到的东西：Charge.Prices（usage_events.unit_prices 的唯一来源）与
// 逐槽位的 Breakdown（不只 Total）。
//
// 走两阶段而不是直接算明细，是因为库里的 cost 取的是 Settlement.Actual，而它
// 在撞上预留上界时会被截成预留额。直接算明细证明的是「明细对」，不是「账上
// 落的那笔钱对」——用例因此显式断言 Capped 为 false，把这条前提钉住。
//
// # 只比总额不够
//
// 两种错误可以互相抵消：输出侧多减了 x 个 token、推理侧少算了 x 个 token，
// 总额看着分毫不差。所以下面逐槽位比 Breakdown、逐项比 Prices，并且把期望值
// 写死在断言里——从被测函数取期望值等于把同一个错误抄两遍。

// reasoningRule 是 issue 里那一组单价：prompt 1.5e-6、completion 7.5e-6、
// reasoning 9e-6（每 token 多少元）。
//
// 三项都显式写出：规则里没写的槽位在用量非零时会被 Validate 拒掉，而漏写
// 一项在账单上表现为「这一项免费」——那正是这类测试要挡的错账形态。
const reasoningRule = `{"prompt": 0.0000015, "completion": 0.0000075, "reasoning": 0.000009}`

// reasoningFixedAt 是这些用例的受理时刻。固定值让求值时刻可重复。
var reasoningFixedAt = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// fixedRuleSource 对每一条候选都返回同一份规则。
//
// 这里只借用 pipeline 走一遍「预留 → 结算」，不按渠道分流；ChannelID 照真实
// 实现填上，预留会按它查缓存记录。
type fixedRuleSource struct {
	source string
}

func (f fixedRuleSource) Match(_ context.Context, q pipeline.RuleQuery) (pipeline.Rule, error) {
	if len(q.Candidates) != 1 {
		return pipeline.Rule{}, fmt.Errorf("结算只该有一个候选，得到 %d 个", len(q.Candidates))
	}
	return pipeline.Rule{ID: 1, Version: 1, Source: f.source, ChannelID: q.Candidates[0].ChannelID}, nil
}

func (f fixedRuleSource) MatchAll(_ context.Context, q pipeline.RuleQuery) ([]pipeline.Rule, error) {
	out := make([]pipeline.Rule, 0, len(q.Candidates))
	for _, c := range q.Candidates {
		out = append(out, pipeline.Rule{ID: 1, Version: 1, Source: f.source, ChannelID: c.ChannelID})
	}
	return out, nil
}

// reasoningUsage 造一次「输入 28621、输出 completion、推理 reasoning」的上游
// 用量，并按 declared 声明它属于哪种口径。
//
// 参数收 int64 而 dto.Usage 的字段是 int：两边本来就是两种类型——dto.Usage 是
// 上游报文里的数，channel.Usage 是计费口径的数（int64）。转换只在这一处发生，
// 换回去时不做截断判断，因为调用方给的都是小整数。
//
// 输入量与 completion=18 / reasoning=30 那一格取自 issue 里那条实测记录——
// 它已经写在 issue 正文里，且不含任何上游标识（域名、账号、模型清单都没有）。
// 其余形状是按 issue 的公式合成的，见 TestSettleCostForEveryUsageShape 的注释。
func reasoningUsage(completion, reasoning int64, declared bool) channel.Usage {
	return UsageFromRelaykit(&dto.Usage{
		PromptTokens:           28621,
		CompletionTokens:       int(completion),
		TotalTokens:            int(28621 + completion + reasoning),
		CompletionTokenDetails: dto.OutputTokenDetails{ReasoningTokens: int(reasoning)},
		UsageSource:            dto.BillingUsageSourceOAIChat,
		UsageSemantic:          dto.BillingUsageSemanticOpenAI,
	}, declared)
}

// reasoningRequest 是一次能走完预留与结算的请求。
//
// InputTokens 取 28621 与结算用的输入量对齐：填小了会让每一次结算都撞上
// Capped，而 Capped 时 Actual 被截成预留额——金额就不再是按用量算出来的
// 那个数，断言会红在一条与口径无关的理由上。
func reasoningRequest(requestID string) pipeline.Request {
	return pipeline.Request{
		RequestID:      requestID,
		UserID:         42,
		CanonicalModel: "gpt-4o",
		Asset:          "CNY",
		Candidates:     []pipeline.RuleCandidate{{ChannelID: 7, OwnerUserID: 9}},
		ChannelID:      7,
		OwnerUserID:    9,
		InputTokens:    28621,
		MaxTokens:      500,
		Protocol:       channel.ProtocolOpenAI,
	}
}

// settleUsage 走一遍「预留 → 结算」，返回结算结果。
//
// 走真实的两阶段而不是直接调 billing.ComputeWithCharges：库里的 cost 取的是
// Settlement.Actual，而它在撞上界时会被截。直接算明细的话，用例证明的是
// 「明细对」，不是「账上落的那笔钱对」。
func settleUsage(ctx context.Context, t *testing.T, requestID string, u channel.Usage) *pipeline.Charge {
	t.Helper()

	p := &pipeline.Pipeline{
		Rules: fixedRuleSource{source: reasoningRule},
		Now:   func() time.Time { return reasoningFixedAt },
	}
	req := reasoningRequest(requestID)
	reserved, err := p.Reserve(ctx, req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}
	charge, err := p.Settle(ctx, req, reserved, pipeline.Outcome{
		Usage:       u,
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}
	if charge.Settlement.Capped {
		t.Fatalf("这次结算撞上了预留上界（按用量算是 %s，预留 %s）——用例的前提不成立："+
			"撞上界时 Actual 被截成预留额，金额就不再是按用量算出来的那个数",
			charge.Settlement.Breakdown.Total, charge.Settlement.Reserved)
	}
	return charge
}

// quietLogs 把进程默认日志器换成丢弃，用例结束时还原。
//
// 结算在上游报的推理多于输出总量时会告警（声明「已含」的渠道上那是矛盾数据）。
// 告警不影响金额，但会把默认日志器吵起来。observe.FromContext 的日志器只来自
// slog 的包级默认（见 observe.AdoptAsDefault），所以这里改的是它。
//
// # 每个顶层用例调一次，不在 settleUsage 里调
//
// 一次结算里调两次 settleUsage 时，第二次保存下来的「原值」已经是丢弃日志器；
// t.Cleanup 的 LIFO 顺序恰好还能还原对，但那是靠调用顺序撑着的——改一行就坏。
// 而且它是进程级状态：本包没有 t.Parallel() 的用例（否则会互相吞日志），
// 这条约束要跟着这一段留着。
func quietLogs(t *testing.T) {
	t.Helper()
	original := slog.Default()
	t.Cleanup(func() { slog.SetDefault(original) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// assertAmounts 逐项比一组金额，值写成 money 的定点字符串（保留 8 位小数）。
//
// 比字符串而不是比 Cmp：定点表示没有浮点误差，逐位相等就是同一个数；而 Cmp
// 只说明数值相等，写错一个槽位名字也会静默通过。
func assertAmounts(t *testing.T, what string, got map[string]money.Amount, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s 有 %d 个槽位，期望 %d 个", what, len(got), len(want))
	}
	for slot, expected := range want {
		amount, ok := got[slot]
		if !ok {
			t.Errorf("%s 少了槽位 %q", what, slot)
			continue
		}
		if amount.String() != expected {
			t.Errorf("%s[%s] = %s，期望 %s", what, slot, amount.String(), expected)
		}
	}
}

// 一条推理多于可见输出的用量，其金额必须与「单价 × 用量」逐位对上。
//
// 这是 issue 里那条实测记录：prompt=28621、completion=18、reasoning=30，
// 单价 prompt 1.5e-6、completion 7.5e-6、reasoning 9e-6。
//
//	28621 × 1.5e-6 = 0.04293150
//	   18 × 7.5e-6 = 0.00013500
//	   30 × 9e-6   = 0.00027000
//	合计            = 0.04333650
//
// 期望值全部写死，不拿 Prices 乘 Usage 反推：反推与实现在同一条错误上分岔
// 不了——FromChannelUsage 少减一次，用量与金额会一起变，反推照样成立。
func TestSettleDeepSeekUsageCostFromUnitPrices(t *testing.T) {
	quietLogs(t)
	charge := settleUsage(context.Background(), t, "req-1203-prices", reasoningUsage(18, 30, false))

	// 单价侧：规则给的三项原样落到 Charge.Prices。它唯一的下游是
	// usage_events.unit_prices，事后对账要拿它对。
	assertAmounts(t, "Prices", map[string]money.Amount{
		"prompt":     charge.Prices.Prompt,
		"completion": charge.Prices.Completion,
		"reasoning":  charge.Prices.Reasoning,
	}, map[string]string{
		"prompt":     "0.00000150",
		"completion": "0.00000750",
		"reasoning":  "0.00000900",
	})

	// 明细侧：五个槽位逐个比。缓存两项是 0 也写出来——漏掉一个槽位时，
	// 「它恰好是 0」与「它没被算」在总额上分不出来。
	assertAmounts(t, "Breakdown", map[string]money.Amount{
		"prompt":      charge.Settlement.Breakdown.Prompt,
		"completion":  charge.Settlement.Breakdown.Completion,
		"cache_read":  charge.Settlement.Breakdown.CacheRead,
		"cache_write": charge.Settlement.Breakdown.CacheWrite,
		"reasoning":   charge.Settlement.Breakdown.Reasoning,
		"total":       charge.Settlement.Breakdown.Total,
	}, map[string]string{
		"prompt":      "0.04293150",
		"completion":  "0.00013500",
		"cache_read":  "0.00000000",
		"cache_write": "0.00000000",
		"reasoning":   "0.00027000",
		"total":       "0.04333650",
	})

	// cost 那一列：usage_events.cost 取的是 Settlement.Actual（见
	// internal/ledger/store 的 insertUsageEvent）。不撞上界时它等于明细总额。
	if got := charge.Settlement.Actual.String(); got != "0.04333650" {
		t.Errorf("结算金额 = %s，期望 0.04333650（与明细总额同一个数）", got)
	}
}

// 同一组用量在「推理已算在输出里」的渠道上算出更小的金额，而少的那一笔
// 恰好是整段可见输出。
//
// 声明「已含」时 FromChannelUsage 会做 max(18−30, 0) = 0：输出侧一分钱收不到，
// 只剩推理那一项。这不是「差一点」，是整整 18 个输出 token 白送。
//
//	声明 false：0.04293150 + 0.00013500 + 0.00027000 = 0.04333650
//	声明 true ：0.04293150 + 0.00000000 + 0.00027000 = 0.04320150
func TestSettleAmountIsSmallerWhenReasoningCountedInOutput(t *testing.T) {
	quietLogs(t)
	ctx := context.Background()
	separate := settleUsage(ctx, t, "req-1203-separate", reasoningUsage(18, 30, false))
	included := settleUsage(ctx, t, "req-1203-included", reasoningUsage(18, 30, true))

	// 输出被推理吃光：声明「已含」时这一项是 0，不是「小一点」。
	if got := included.Settlement.Breakdown.Completion.String(); got != "0.00000000" {
		t.Errorf("声明 true 时输出侧金额 = %s，期望 0——18 个可见输出被 30 个推理吃光", got)
	}
	if got := separate.Settlement.Breakdown.Completion.String(); got != "0.00013500" {
		t.Errorf("声明 false 时输出侧金额 = %s，期望 0.00013500", got)
	}

	// 推理侧两种口径下都是同一个数：这一位只改输出侧，不该顺手动别的槽位。
	for _, tc := range []struct {
		name   string
		charge *pipeline.Charge
	}{
		{"声明 false", separate},
		{"声明 true", included},
	} {
		if got := tc.charge.Settlement.Breakdown.Reasoning.String(); got != "0.00027000" {
			t.Errorf("%s 时推理侧金额 = %s，期望 0.00027000", tc.name, got)
		}
	}

	if got := separate.Settlement.Actual.String(); got != "0.04333650" {
		t.Errorf("声明 false 时结算金额 = %s，期望 0.04333650", got)
	}
	if got := included.Settlement.Actual.String(); got != "0.04320150" {
		t.Errorf("声明 true 时结算金额 = %s，期望 0.04320150", got)
	}
	if separate.Settlement.Actual.Cmp(included.Settlement.Actual) <= 0 {
		t.Errorf("声明 false 的金额应更大：%s vs %s",
			separate.Settlement.Actual, included.Settlement.Actual)
	}
}

// shapeExpectation 是某一格在一种声明下应有的结果。
type shapeExpectation struct {
	// usageCompletion 是**计费口径**用量里的 CompletionTokens。负数说明减法
	// 没有被夹住——它进金额运算会算出一个负的输出项。
	usageCompletion int64
	// completion 与 reasoning 是两个槽位的金额。
	completion string
	reasoning  string
	// cost 是 Settlement.Actual，也就是库里的 usage_events.cost。
	cost string
}

// 三种用量形状 × 两种声明，每一格钉住一件不同的事。
//
// # 夹具是合成的
//
// 三个形状按 issue 里的公式（cost = prompt×P + completion×C + reasoning×R，
// 其中声明「已含」那一支的 completion 先按 max(c−r, 0) 折过）造出来，不是从
// 任何一次真实调用抄下来的。真实上游数据一条都不进仓库。
//
// 为什么不直接用「那批真实记录」：它们来自真实上游的账单，带着域名、账号与
// 模型清单，写进公开仓库就是泄露；而把数字匿名化之后，它们提供的也只剩
// 「形状」这一层信息——那正是下面三格在做的事。
//
// # 三格各自钉什么
//
//   - 推理少于输出（30/18）：钉住减法**确实生效**。声明「已含」时输出侧少收
//     r×C，而它不为 0——「减了」与「减到 0」在这里分得开。
//   - 推理多于输出（18/30）：钉住 max64 把负值**夹到 0**。去掉 max64 时
//     usageCompletion 会变成 −12，这一格当场红；而单看金额，−12×7.5e-6 只让
//     总额小一点点，很容易被当成「另一种口径」放过。
//   - 推理等于输出（18/18）：钉住**等号落在「减」这一侧**。max64 的边界上，
//     「夹到 0」与「保留原值」是同一个数的两种来历，只有这一格分得开。
//
// 每一格都跑两种声明。声明「互斥」那一半在三格里是同一个形状（c 原样乘价），
// 它钉的是「声明为 false 时输出侧不受影响」；声明「已含」那一半才是三格互不
// 相同的地方。
//
// # 等号那一格的鉴别力在 included 那一半
//
// 「相等」这一格的两个半边钉的不是同一件事，而**能把它与「多于」那格分开的
// 是 included**：
//
//   - included 半边：18/18 与 18/30 的期望值确实相同（都是 0 / 0.00000000），
//     但「相同」不等于「重复」——把 max64 换成「只在 c > r 时减」，18/30 与
//     18/18 会一起红；而在等号处特判保留原值（`if c == r { 原值 } else { max64 }`）
//     时，18/30 那格是**绿的**，只有这一格红。那种实现正是把判据写成严格大于
//     `if c > r` 的样子，而它只在这一格露出来。
//   - separate 半边：18/18 与 18/30 的期望值不同（18 与 30 都保留），它钉的是
//     「声明为 false 时不做任何折减」，对 max64 的边界没有鉴别力。
//
// 换句话说：只留「多于」那格，「相等时保留原值」这种错法会漏过去。
func TestSettleCostForEveryUsageShape(t *testing.T) {
	cases := []struct {
		name       string
		completion int64
		reasoning  int64
		whatItPins string
		separate   shapeExpectation
		included   shapeExpectation
	}{
		{
			name:       "推理少于输出",
			completion: 30,
			reasoning:  18,
			whatItPins: "减法生效：声明「已含」时输出侧少收 18×7.5e-6，且不为 0",
			separate: shapeExpectation{
				usageCompletion: 30,
				completion:      "0.00022500",
				reasoning:       "0.00016200",
				cost:            "0.04331850",
			},
			included: shapeExpectation{
				usageCompletion: 12,
				completion:      "0.00009000",
				reasoning:       "0.00016200",
				cost:            "0.04318350",
			},
		},
		{
			name:       "推理多于输出",
			completion: 18,
			reasoning:  30,
			whatItPins: "max64 夹到 0：减法本会得 −12，夹住之后输出侧一分钱收不到",
			separate: shapeExpectation{
				usageCompletion: 18,
				completion:      "0.00013500",
				reasoning:       "0.00027000",
				cost:            "0.04333650",
			},
			included: shapeExpectation{
				usageCompletion: 0,
				completion:      "0.00000000",
				reasoning:       "0.00027000",
				cost:            "0.04320150",
			},
		},
		{
			name:       "推理等于输出",
			completion: 18,
			reasoning:  18,
			whatItPins: "等号落在「减」这一侧：相等时归零，不是保留原值（判据写成严格大于时只在这一格露出来）",
			separate: shapeExpectation{
				usageCompletion: 18,
				completion:      "0.00013500",
				reasoning:       "0.00016200",
				cost:            "0.04322850",
			},
			included: shapeExpectation{
				usageCompletion: 0,
				completion:      "0.00000000",
				reasoning:       "0.00016200",
				cost:            "0.04309350",
			},
		},
	}

	quietLogs(t)
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, arm := range []struct {
				name     string
				declared bool
				want     shapeExpectation
			}{
				{"声明 false", false, tc.separate},
				{"声明 true", true, tc.included},
			} {
				t.Run(arm.name, func(t *testing.T) {
					u := reasoningUsage(tc.completion, tc.reasoning, arm.declared)

					// 先判**换算**：计费口径的 CompletionTokens 是不是期望的那个数。
					//
					// 这一步必须走 billing.FromChannelUsage 而不只是读
					// charge.Usage——夹住发生在那个函数里，而负数一旦漏出去，
					// 后面的结算会在 Validate 那一步就失败（"用量为负"），
					// 用例红在一句与口径无关的错误上。判据要长在产生它的那一步：
					// 去掉 max64 时，红的是这里，失败原文是「期望 0」而不是
					// 「结算出错」。用 Fatalf 而不是 Errorf：换算错了，后面
					// 拿它算出的金额就不是「另一个错误」，而是同一个错误的下游
					// ——再报一遍只会把红的落点糊掉。
					if got := billing.FromChannelUsage(u).CompletionTokens; got != arm.want.usageCompletion {
						t.Fatalf("计费口径的 CompletionTokens = %d，期望 %d（%s）",
							got, arm.want.usageCompletion, tc.whatItPins)
					}

					charge := settleUsage(ctx, t, "req-1203-shape", u)
					if got := charge.Usage.CompletionTokens; got != arm.want.usageCompletion {
						t.Errorf("结算用的计费口径 CompletionTokens = %d，期望 %d（%s）",
							got, arm.want.usageCompletion, tc.whatItPins)
					}
					assertAmounts(t, "Breakdown", map[string]money.Amount{
						"completion": charge.Settlement.Breakdown.Completion,
						"reasoning":  charge.Settlement.Breakdown.Reasoning,
					}, map[string]string{
						"completion": arm.want.completion,
						"reasoning":  arm.want.reasoning,
					})
					if got := charge.Settlement.Actual.String(); got != arm.want.cost {
						t.Errorf("结算金额 = %s，期望 %s", got, arm.want.cost)
					}
				})
			}
		})
	}
}

// 渠道没有声明这一位时，金额必须与改动前逐位相同。
//
// 「没声明」有四种长相：setting 整个是 nil、是空 map、有 usage 块但没有这个
// 内层键、块写成了别的形状。四种都要落到「已含」——落到互斥那一侧的话，
// 所有没配过的渠道会集体改变金额，而那正是这一位最不该带来的后果。
//
// 比的是整份 Breakdown 与结算金额，不是「总额相同」：漏一个槽位、把某一项
// 算成 0 而另一项补上，总额可以分毫不差。
//
// 读的是 channel.UsageCompletionIncludesReasoning 本身，不是手写一个 true：
// 那一步才是适配器与计费之间唯一的口径入口，替它写死就绕过了被测的那一环。
func TestSettleAmountUnchangedWithoutDeclaration(t *testing.T) {
	quietLogs(t)
	ctx := context.Background()
	baseline := settleUsage(ctx, t, "req-1203-baseline", reasoningUsage(18, 30, true))

	for _, tc := range []struct {
		name    string
		setting map[string]any
	}{
		{"setting 为 nil", nil},
		{"setting 为空", map[string]any{}},
		{"有 usage 块但没有内层键", map[string]any{"usage": map[string]any{}}},
		{"usage 块写成别的形状", map[string]any{"usage": "completion_includes_reasoning=false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declared := channel.UsageCompletionIncludesReasoning(tc.setting)
			if !declared {
				t.Fatalf("%s 时读成了互斥口径——没声明必须沿用「已含」", tc.name)
			}

			got := settleUsage(ctx, t, "req-1203-baseline", reasoningUsage(18, 30, declared))
			if !reflect.DeepEqual(got.Settlement.Breakdown, baseline.Settlement.Breakdown) {
				t.Errorf("明细与改动前不一致：\n  得到 %#v\n  期望 %#v",
					got.Settlement.Breakdown, baseline.Settlement.Breakdown)
			}
			if got.Settlement.Actual.Cmp(baseline.Settlement.Actual) != 0 {
				t.Errorf("结算金额 = %s，期望与改动前相同（%s）",
					got.Settlement.Actual, baseline.Settlement.Actual)
			}
			if !reflect.DeepEqual(got.Usage, baseline.Usage) {
				t.Errorf("计费口径用量与改动前不一致：\n  得到 %#v\n  期望 %#v", got.Usage, baseline.Usage)
			}
		})
	}
}
