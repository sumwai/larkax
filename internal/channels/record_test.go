package channels

import (
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// consumedTokens 的口径必须与计费那一侧一致。
//
// 两处算的是同一个总数：计费那边把缓存与推理拆开是为了按不同单价算钱，
// 加起来就是这里要的数。这一条用同一批输入同时喂给两边——两边漂移的症状
// 是「额度扣得比实际消耗多或少」，而那不会报错，只会让额度慢慢偏掉。
func TestConsumedTokensMatchesBillingTotal(t *testing.T) {
	cases := []struct {
		name string
		u    channel.Usage
	}{
		{"没有用量", channel.Usage{}},
		{"只有输入输出", channel.Usage{PromptTokens: 100, CompletionTokens: 50}},
		// PromptTokens 含缓存：再加一遍缓存读写就重复计了。
		{"带缓存", channel.Usage{
			PromptTokens: 100, CompletionTokens: 50,
			CacheReadTokens: 40, CacheWriteTokens: 10,
		}},
		// 推理已算在输出里：不能再加。
		{"推理含在输出里", channel.Usage{
			PromptTokens: 10, CompletionTokens: 100,
			ReasoningTokens: 50, CompletionIncludesReasoning: true,
		}},
		// 推理是单独报的：要加。
		{"推理单独报", channel.Usage{
			PromptTokens: 10, CompletionTokens: 100, ReasoningTokens: 50,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := billing.FromChannelUsage(tc.u).TotalTokens()
			if got := consumedTokens(tc.u); got != want {
				t.Errorf("consumedTokens = %d，计费口径 = %d（%+v）", got, want, tc.u)
			}
		})
	}
}

// 结论决定怎么动连续失败计数。三态收成两个布尔值，SQL 里就不必认识
// success / failure 这些字面量——那种字面量改一处名字不会编译失败，
// 只会让那条语句静默走进「什么都不改」的分支。
func TestStreakChange(t *testing.T) {
	cases := []struct {
		verdict   route.Verdict
		reset     bool
		increment bool
	}{
		{route.VerdictSuccess, true, false},
		{route.VerdictFailure, false, true},
		// 客户端断开、请求本身有错：不加，也不打断已有的连续失败。
		{route.VerdictUnattributed, false, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.verdict), func(t *testing.T) {
			reset, increment, err := streakChange(tc.verdict)
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if reset != tc.reset || increment != tc.increment {
				t.Errorf("reset = %v、increment = %v，期望 %v、%v",
					reset, increment, tc.reset, tc.increment)
			}
		})
	}
}

// 未知结论要报错，而不是静默当成「什么都不改」：后者会让「保护失效」
// 与「这个渠道没有失败」在库里长得一模一样。
func TestStreakChangeRejectsUnknownVerdict(t *testing.T) {
	if _, _, err := streakChange(route.Verdict("whatever")); err == nil {
		t.Error("未知结论该报错")
	}
}
