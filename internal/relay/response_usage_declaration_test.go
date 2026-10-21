package relay

import (
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
)

// 推理是否已经算在输出里，**由渠道声明，不在这里推断**。
//
// 同一个 dto.Usage 在两种声明下只该差那一位：数字照抄，减法是计费层的事
// （见 billing.FromChannelUsage）。这一条钉住「传进来的声明原样落到
// channel.Usage 上」——把它写成常量的话，另一种口径的上游永远算不对。
func TestUsageFromRelaykitCarriesDeclaration(t *testing.T) {
	for _, declared := range []bool{true, false} {
		got := UsageFromRelaykit(deepSeekStyleUsage(), declared)
		if got.CompletionIncludesReasoning != declared {
			t.Errorf("声明 %v 时 CompletionIncludesReasoning = %v", declared, got.CompletionIncludesReasoning)
		}
		// 声明只影响这一位，不该顺手改动数字。
		if got.PromptTokens != 28621 || got.CompletionTokens != 18 || got.ReasoningTokens != 30 {
			t.Errorf("声明 %v 时数字被改动了: %+v", declared, got)
		}
	}
}

// 没声明（调用方传 true）时，换算结果要与改动前**逐位相同**。
//
// 不是「看着没变」：整个 channel.Usage 与一份写死的期望值比，包括 Raw 与
// Reported。这个函数是钱路上最容易改坏的一处，而「只在某个字段上少了一个
// 键」不会让任何断言变红——除非断言的是整份结果。
func TestUsageFromRelaykitDefaultPathUnchanged(t *testing.T) {
	got := UsageFromRelaykit(deepSeekStyleUsage(), true)

	want := channel.Usage{
		PromptTokens:                28621,
		CompletionTokens:            18,
		ReasoningTokens:             30,
		CompletionIncludesReasoning: true,
		Reported:                    true,
		Raw: map[string]any{
			"prompt_tokens":     28621,
			"completion_tokens": 18,
			"total_tokens":      28669,
			"source":            dto.BillingUsageSourceOAIChat,
			"semantic":          dto.BillingUsageSemanticOpenAI,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("默认口径的结果与改动前不一致：\n  得到 %#v\n  期望 %#v", got, want)
	}
}

// 一条实测记录在两种声明下的金额：声明 false 才是上游真正的口径。
//
// 数字取自一次推理多于可见输出的调用：prompt=28621、completion=18、
// reasoning=30。规则是 prompt 1.5e-6、completion 7.5e-6、reasoning 9e-6。
//
//	声明 false：1.5e-6×28621 + 7.5e-6×18 + 9e-6×30      = 0.04333650
//	声明 true ：1.5e-6×28621 + 7.5e-6×max(18−30,0) + 9e-6×30 = 0.04320150
//
// 差额正是 18×7.5e-6：输出侧被推理量吃光。断言的是具体金额而不是「不等」，
// 因为「两种口径确实不同」与「差多少」是两件事，前者不会发现系数抄错。
func TestDeepSeekUsageAmountUnderBothDeclarations(t *testing.T) {
	prices := billing.Prices{
		Prompt:     money.MustParse("0.0000015"),
		Completion: money.MustParse("0.0000075"),
		Reasoning:  money.MustParse("0.000009"),
	}

	separate := billing.ComputeWithCharges(
		billing.FromChannelUsage(UsageFromRelaykit(deepSeekStyleUsage(), false)),
		prices, billing.Charges{})
	included := billing.ComputeWithCharges(
		billing.FromChannelUsage(UsageFromRelaykit(deepSeekStyleUsage(), true)),
		prices, billing.Charges{})

	if want := money.MustParse("0.04333650"); separate.Total.Cmp(want) != 0 {
		t.Errorf("声明 false 时总额 = %s，期望 %s", separate.Total, want)
	}
	if want := money.MustParse("0.04320150"); included.Total.Cmp(want) != 0 {
		t.Errorf("声明 true 时总额 = %s，期望 %s", included.Total, want)
	}
	if separate.Total.Cmp(included.Total) <= 0 {
		t.Errorf("声明 false 的金额应更大：%s vs %s", separate.Total, included.Total)
	}
}

// 减法是声明带来的，不是协议层做的：同一组数字只在 true 时被减。
//
// 用一组不自相矛盾的数（completion=100、reasoning=30），这样「减没减」
// 可以直接从 CompletionTokens 读出来，不受钳到 0 那条兜底干扰。
func TestFromChannelUsageSubtractsOnlyWhenDeclared(t *testing.T) {
	u := &dto.Usage{
		PromptTokens:           10,
		CompletionTokens:       100,
		CompletionTokenDetails: dto.OutputTokenDetails{ReasoningTokens: 30},
	}

	separate := billing.FromChannelUsage(UsageFromRelaykit(u, false))
	if separate.CompletionTokens != 100 || separate.ReasoningTokens != 30 {
		t.Errorf("声明 false 时不该减：%+v", separate)
	}

	included := billing.FromChannelUsage(UsageFromRelaykit(u, true))
	if included.CompletionTokens != 70 || included.ReasoningTokens != 30 {
		t.Errorf("声明 true 时应减掉推理：%+v", included)
	}
}

// 三条响应形状都要把声明带到用量上：非流式的三个适配器都走
// ResponseFromRelaykit，漏掉其中一支时那条协议的渠道会一直按「已含」算。
func TestResponseFromRelaykitCarriesDeclaration(t *testing.T) {
	usage := dto.Usage{
		PromptTokens:           10,
		CompletionTokens:       100,
		CompletionTokenDetails: dto.OutputTokenDetails{ReasoningTokens: 30},
	}
	sources := map[string]any{
		"openai": &dto.OpenAITextResponse{Model: "m", Usage: usage},
		"claude": &dto.ClaudeResponse{
			Model: "m",
			Usage: &dto.ClaudeUsage{InputTokens: 10, OutputTokens: 100},
		},
		"responses": &dto.OpenAIResponsesResponse{Model: "m", Usage: &usage},
	}

	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			got, err := ResponseFromRelaykit(src, false)
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if got.Usage.CompletionIncludesReasoning {
				t.Error("声明 false，响应里的用量却仍是「已含」")
			}

			got, err = ResponseFromRelaykit(src, true)
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if !got.Usage.CompletionIncludesReasoning {
				t.Error("声明 true，响应里的用量却是「互斥」")
			}
		})
	}
}

// deepSeekStyleUsage 是一条「完成与推理分列」的上游报文的形状：
// completion_tokens 里不含 reasoning_tokens，两者各算各的。
func deepSeekStyleUsage() *dto.Usage {
	return &dto.Usage{
		PromptTokens:           28621,
		CompletionTokens:       18,
		TotalTokens:            28669,
		CompletionTokenDetails: dto.OutputTokenDetails{ReasoningTokens: 30},
		UsageSource:            dto.BillingUsageSourceOAIChat,
		UsageSemantic:          dto.BillingUsageSemanticOpenAI,
	}
}
