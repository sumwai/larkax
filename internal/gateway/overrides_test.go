package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/route"
)

// 这个文件测的是**预留上界**那一路：别名上的 max_tokens 覆盖会顶掉调用方给的
// 那个数，所以预留的输出侧上界要按覆盖之后的算（见 Gateway.outputBound）。
//
// 判据落在**落库的那笔预留金额**上，而不是「网关读到了覆盖」：上界算错的后果
// 是结算撞上界、差额被截掉，而那件事从响应、日志和账单行上都看不出来。金额是
// 唯一会对不上的地方。
//
// 覆盖的语义（白名单、谁赢）在 internal/overrides 与 internal/route 各有用例，
// 这里只钉「按哪个数算上界」。

// fixedOverrides 按渠道给出覆盖，代表**逐渠道**配的别名覆盖。
//
// 逐渠道而不是全局一个开关：上界要按候选集取最大，而「取的是覆盖之后那个
// 更大的数」只有在候选之间大小不同时才分辨得出来。
type fixedOverrides struct {
	byChannel map[int64]map[string]any
	err       error
	// asked 记下被问到的渠道，用来断言「候选集里每个渠道都问过」。
	asked [][]int64
}

// withOverrides 在组装夹具时接上覆盖来源。
//
// 必须走选项而不是 newHarness 返回之后再改 h.aliasOverrides：网关在组装那一刻
// 就把那个接口值抄进了自己的字段，之后改夹具上的字段对它没有任何影响——而
// 那种错的表现是「用例静默按调用方的数算」，断言会红在一个看不出原因的地方。
func withOverrides(src AliasOverrideSource) func(*harness) {
	return func(h *harness) { h.aliasOverrides = src }
}

func (f *fixedOverrides) OverridesFor(_ context.Context, _ string, channelIDs []int64) (map[int64]map[string]any, error) {
	f.asked = append(f.asked, append([]int64(nil), channelIDs...))
	if f.err != nil {
		return nil, f.err
	}
	out := map[int64]map[string]any{}
	for _, id := range channelIDs {
		if v, ok := f.byChannel[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// 覆盖把 max_tokens 调大时，预留按**调大之后**的数算。
//
// 这一条是整个改动的核心：不这么算的话，商家把上限调到 2048 而预留仍按调用方
// 的 64 算，输出了 1000 个 token 的那一次就会被截成按 64 算的钱——少收，
// 而且账上那一行完全正常。
func TestReserveBoundUsesOverriddenMaxTokens(t *testing.T) {
	overrides := &fixedOverrides{byChannel: map[int64]map[string]any{
		1: {"max_tokens": 2048},
	}}
	h := newHarness(t, withOverrides(overrides))

	req := request()
	req.InputTokens = 0 // 让上界只剩输出侧那一项，金额好算
	req.MaxTokens = 64

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
	// 2048 × 2e-6 = 0.004096。调用方给的 64 会算出 0.000128，两者差 32 倍。
	if got := h.reservations.saved[0].Amount.String(); got != "0.00409600" {
		t.Errorf("预留金额 = %s，期望 0.00409600（2048 × 输出单价）；按调用方给的 64 算是 0.00012800", got)
	}
}

// 覆盖比调用方给的小时，上界跟着**变小**——不拿调用方那个数当保底。
//
// 「上界只多不少」听上去更保险，但它保的是错的数：真正发出去的请求带的是覆盖
// 值，而上界要跟着那份请求走。留着调用方的数当保底会让「商家把输出限制调小」
// 反而冻住调用方更多的钱。
func TestReserveBoundFollowsOverrideDown(t *testing.T) {
	h := newHarness(t, withOverrides(&fixedOverrides{byChannel: map[int64]map[string]any{
		1: {"max_tokens": 16},
	}}))

	req := request()
	req.InputTokens = 0
	req.MaxTokens = 64

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 16 × 2e-6 = 0.000032
	if got := h.reservations.saved[0].Amount.String(); got != "0.00003200" {
		t.Errorf("预留金额 = %s，期望 0.00003200（16 × 输出单价）；等于 0.00012800 说明拿调用方给的 64 当了保底", got)
	}
}

// 多个候选各配一份覆盖时，上界取**最大**的那个。
//
// 预留发生在选路之前，还不知道会走哪个候选，所以上界要盖得住每一个。取小的
// 那个会让「选中了贵的那条」变成一次撞上界的少收。
func TestReserveBoundTakesMaxOverCandidates(t *testing.T) {
	overrides := &fixedOverrides{byChannel: map[int64]map[string]any{
		1: {"max_tokens": 128},
		2: {"max_tokens": 1024},
	}}
	h := newHarness(t, withOverrides(overrides), func(h *harness) {
		h.candidates.cands = []route.Candidate{candidate(1, "gpt-4o"), candidate(2, "gpt-4o")}
	})

	req := request()
	req.InputTokens = 0
	req.MaxTokens = 64

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 1024 × 2e-6 = 0.002048
	if got := h.reservations.saved[0].Amount.String(); got != "0.00204800" {
		t.Errorf("预留金额 = %s，期望 0.00204800（候选里最大的那个上限）", got)
	}

	// 候选集里**每个**渠道都要问到：只问第一个的实现会漏掉后面那些更大的上限。
	if len(overrides.asked) != 1 {
		t.Fatalf("覆盖来源被问了 %d 次，期望 1 次（一次问完整个候选集）", len(overrides.asked))
	}
	if got := overrides.asked[0]; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("问到的渠道 = %v，期望候选集里的 [1 2]", got)
	}
}

// 别名没配覆盖时，上界就是调用方给的那个数——**一个字节都不变**。
//
// 这是回归判据：绝大多数部署一条覆盖都没配，而它们的行为必须与这个功能
// 出现之前逐字相同。
func TestReserveBoundWithoutOverridesIsCallersNumber(t *testing.T) {
	h := newHarness(t)
	// 默认夹具就是一个渠道、一条覆盖都没配。

	req := request()
	req.InputTokens = 0
	req.MaxTokens = 64

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
	// 64 × 2e-6 = 0.000128
	if got := h.reservations.saved[0].Amount.String(); got != "0.00012800" {
		t.Errorf("预留金额 = %s，期望 0.00012800（调用方给的 64 × 输出单价）", got)
	}
}

// 覆盖不合法时**请求失败**，而且一分钱都不冻。
//
// 写入路径校验过不代表这里不用管：库里可能有绕过写入路径写进去的历史行，
// 而静默按原值算上界的表现是「这条配置配了不生效」——正是这一列要消掉的
// 东西。这里同时钉「报错」与「没有预留」：报了错却已经冻住钱的话，那笔钱
// 要等到释放才回来。
func TestReserveFailsOnInvalidOverride(t *testing.T) {
	h := newHarness(t, withOverrides(&fixedOverrides{byChannel: map[int64]map[string]any{
		1: {"messages": []any{"改正文"}},
	}}))

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
	if err == nil {
		t.Fatal("覆盖里有不允许的键时请求应失败，而不是静默按原值算")
	}
	if !contains(err.Error(), `"messages"`) {
		t.Errorf("错误里要说得出是哪个键: %v", err)
	}
	if len(h.reservations.saved) != 0 {
		t.Errorf("算不出上界时不该冻钱，实际预留 %d 次", len(h.reservations.saved))
	}
}

// 覆盖来源读不出来时**请求失败**，不静默按调用方的数算。
//
// 静默回落的表现是「这次请求少冻了钱」，而那次失败在响应、日志和账单上都
// 看不出来——凡是算不出上界的路径都必须出声（与没有规则、候选集为空同类）。
func TestReserveFailsWhenOverridesUnreadable(t *testing.T) {
	h := newHarness(t, withOverrides(&fixedOverrides{err: errors.New("数据库连不上")}))

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
	if err == nil {
		t.Fatal("读覆盖失败时请求应失败，而不是按调用方给的数算上界")
	}
	if !contains(err.Error(), "数据库连不上") {
		t.Errorf("原始错误应被带上: %v", err)
	}
	if len(h.reservations.saved) != 0 {
		t.Errorf("算不出上界时不该冻钱，实际预留 %d 次", len(h.reservations.saved))
	}
}

// 没接覆盖来源时**每个请求都失败**，而不是静默按调用方给的数算。
//
// 与 route.ErrNoAliasSource 是同一个选择：忘接了的表现是「上界总是偏小」，
// 而它只在某天真的超了上限时才以少收的形式露出来——那时已经查不回是哪一次。
func TestChatWithoutOverrideSourceFails(t *testing.T) {
	h := newHarness(t)
	h.gw.AliasOverrides = nil

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
	if !errors.Is(err, ErrNoAliasOverrideSource) {
		t.Fatalf("错误 = %v，期望 ErrNoAliasOverrideSource", err)
	}
	if len(h.reservations.saved) != 0 {
		t.Errorf("没有覆盖来源时不该冻钱，实际预留 %d 次", len(h.reservations.saved))
	}
	// 更要紧的是**连上游都没碰**：上界是预留在调上游之前算的。
	if len(h.executor.calls) != 0 {
		t.Errorf("不该发起任何上游调用，实际调用了 %v", h.executor.calls)
	}
}

// contains 判断 err 文本里有没有这一段。
func contains(s, sub string) bool { return strings.Contains(s, sub) }
