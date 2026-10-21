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

// 渠道声明「推理单独报」时，consumedTokens 跟着声明走。
//
// 这条数的口径完全来自 channel.Usage.CompletionIncludesReasoning——它由适配器
// 按渠道 setting 填进来（见 channel.UsageCompletionIncludesReasoning）。声明
// 为 false 的渠道（上游把完成与推理分列）上，推理没被算进输出，必须另加；
// 声明为 true 时它已经含在完成里，再加一遍就是多扣。两种声明算出的数**必须
// 不同**，否则说明这一位根本没有被读。
//
// 用一组推理多于可见输出的数（completion=18、reasoning=30）：这正是互斥口径
// 上游的常见形状，而声明 true 时计费那一侧会把输出钳到 0——在那一格上，
// 「跟着声明走」与「照旧按已含算」相差整整一段推理，不是几个 token 的舍入。
func TestConsumedTokensFollowsChannelDeclaration(t *testing.T) {
	base := channel.Usage{PromptTokens: 28621, CompletionTokens: 18, ReasoningTokens: 30}

	separate := base
	separate.CompletionIncludesReasoning = false
	if got, want := consumedTokens(separate), int64(28621+18+30); got != want {
		t.Errorf("声明 false 时 consumedTokens = %d，期望 %d", got, want)
	}
	// 与计费那一侧同一个总数：那里拆开是为了按不同单价算钱，加起来就是这里
	// 要的数。
	if want := billing.FromChannelUsage(separate).TotalTokens(); consumedTokens(separate) != want {
		t.Errorf("声明 false 时 consumedTokens = %d，计费口径 = %d",
			consumedTokens(separate), want)
	}

	included := base
	included.CompletionIncludesReasoning = true
	if got, want := consumedTokens(included), int64(28621+18); got != want {
		t.Errorf("声明 true 时 consumedTokens = %d，期望 %d", got, want)
	}

	if consumedTokens(separate) == consumedTokens(included) {
		t.Error("两种声明算出了同一个数：CompletionIncludesReasoning 没有被读")
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
