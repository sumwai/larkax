package route

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// ── 目录判据：能力 ──────────────────────────────────────────────────────
//
// 这一组用例钉的是「请求要的能力与目录声明对不上时，这条候选出局」。
// 判据的输入有两半：候选上的 Profile（模型级声明）与这次请求的 Needs。
// 两半缺一个都不成——只有 Profile 会判不出「这次请求到底用没用到」，
// 只有 Needs 则无从知道模型支持什么。
//
// 能力那一栏是三态，「没配过」与「配了说不支持」处置相反：前者放行、
// 后者出局。所以这一组里两种情形各有用例，而且都能写出来（declared）。

func toolRequest() *channel.Request {
	return &channel.Request{
		Model:     "m",
		MaxTokens: 100,
		Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:     []channel.Tool{{Name: "get_weather"}},
	}
}

func TestSelectRejectsCandidatesMissingDeclaredCapability(t *testing.T) {
	cases := []struct {
		name    string
		profile *ModelProfile
		needs   Needs
		want    string
	}{
		{"请求带工具而目录说不支持", &ModelProfile{Tools: declared(false)}, Needs{Tools: true}, "不支持工具调用"},
		{"请求带图片而目录说不支持", &ModelProfile{Vision: declared(false)}, Needs{Vision: true}, "不支持图片"},
		{"请求要推理而目录说不支持", &ModelProfile{Reasoning: declared(false)}, Needs{Reasoning: true}, "不支持推理"},
		{"目录说支持工具", &ModelProfile{Tools: declared(true)}, Needs{Tools: true}, ""},
		{"请求没用到任何能力", &ModelProfile{}, Needs{}, ""},
		// 「有记录、但这一项没配过」= 未知，放行；「有记录、写了 false」
		// = 不支持，出局。这两条并排摆着：把 nil 当 false 处理会让一条只填了
		// 名字的记录选不到所有带工具的请求，而那正是本条要修掉的缺陷（#485）。
		{"目录里那一项没配过", &ModelProfile{}, Needs{Tools: true, Vision: true, Reasoning: true}, ""},
		{"一项不支持、另一项没配过",
			&ModelProfile{Tools: declared(false)}, Needs{Tools: true, Vision: true}, "不支持工具调用"},
		// 目录里没有这个模型的记录：未知，一律放行。这一条与上面几条的
		// 差别是**没有依据**，而不是「依据说可以」。
		{"目录里没有这个模型", nil, Needs{Tools: true, Vision: true, Reasoning: true}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := seededRouter(1).Select(
				[]Candidate{candidate(1, "m", withProfile(tc.profile))}, "m", tc.needs)

			if tc.want == "" {
				if len(plan.Candidates) != 1 {
					t.Fatalf("应当放行，实际排除原因 %+v", plan.Excluded)
				}
				return
			}
			if len(plan.Candidates) != 0 || len(plan.Excluded) != 1 {
				t.Fatalf("应当排除，实际候选 %d 个、排除 %d 条",
					len(plan.Candidates), len(plan.Excluded))
			}
			if got := plan.Excluded[0]; !strings.Contains(got.Reason, tc.want) || !got.Unsupported {
				t.Errorf("排除记录 = %+v，期望理由含 %q 且标为目录判据", got, tc.want)
			}
		})
	}
}

// ── 目录判据：上限 ──────────────────────────────────────────────────────
//
// 两个上限列允许为空（读出来是 0），**0 是未知而不是零额度**——一次采集
// 失败不该让整批模型用不了，所以 0 一律放行。能力那三项同一个口径，只是
// 那里用 nil 表示未知（布尔列没有多余的取值可用，见 ModelProfile）。

func TestSelectRejectsOverLimitOnlyWhenDeclared(t *testing.T) {
	cases := []struct {
		name    string
		profile *ModelProfile
		needs   Needs
		want    string
	}{
		{"输入加输出超出上下文", &ModelProfile{ContextLimit: 1000},
			Needs{InputTokens: 900, MaxOutputTokens: 200}, "上下文上限 1000"},
		{"恰好填满上下文不算超", &ModelProfile{ContextLimit: 1000},
			Needs{InputTokens: 800, MaxOutputTokens: 200}, ""},
		{"上下文未知时不判", &ModelProfile{ContextLimit: 0},
			Needs{InputTokens: 1 << 40, MaxOutputTokens: 1 << 40}, ""},
		{"输出超出单次上限", &ModelProfile{OutputLimit: 100},
			Needs{InputTokens: 1, MaxOutputTokens: 101}, "输出上限 100"},
		{"输出恰好等于上限不算超", &ModelProfile{OutputLimit: 100},
			Needs{InputTokens: 1, MaxOutputTokens: 100}, ""},
		{"输出上限未知时不判", &ModelProfile{OutputLimit: 0},
			Needs{InputTokens: 1, MaxOutputTokens: 1 << 40}, ""},
		{"目录里没有这个模型", nil,
			Needs{InputTokens: 1 << 40, MaxOutputTokens: 1 << 40}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := seededRouter(1).Select(
				[]Candidate{candidate(1, "m", withProfile(tc.profile))}, "m", tc.needs)

			if tc.want == "" {
				if len(plan.Candidates) != 1 {
					t.Fatalf("应当放行，实际排除原因 %+v", plan.Excluded)
				}
				return
			}
			if len(plan.Excluded) != 1 || !strings.Contains(plan.Excluded[0].Reason, tc.want) {
				t.Fatalf("排除记录 = %+v，期望理由含 %q", plan.Excluded, tc.want)
			}
			if len(plan.Candidates) != 0 {
				t.Errorf("被目录挡下的候选不该留在候选集里: %+v", plan.Candidates)
			}
		})
	}
}

// ── 出口 ────────────────────────────────────────────────────────────────
//
// 全部候选都被目录判据挡掉时，处置与「试过、上游一个个都说自己不支持」
// 相同：报同一种错误、一个上游调用都不发。这里断言的是**具体哪个错误**，
// 因为「报错了」在两种结论下都成立，而它们的排查方向完全相反——
// 一个要去查模型目录，一个要去查上游。

func TestChatReportsProfileRejectionWithoutCallingUpstream(t *testing.T) {
	r := seededRouter(1)
	exec := &fakeExecutor{results: map[int64]error{}}
	cands := []Candidate{candidate(1, "m", withProfile(&ModelProfile{Tools: declared(false)}))}

	res, err := r.Chat(context.Background(), exec, toolRequest(), cands, "m")

	if res != nil {
		t.Errorf("一个候选都没试过时不该交回结果: %+v", res)
	}
	var all *Errors
	if !errors.As(err, &all) {
		t.Fatalf("错误类型 = %T，期望 *route.Errors: %v", err, err)
	}
	if all.Unsupported != 1 {
		t.Errorf("Unsupported = %d，期望 1", all.Unsupported)
	}
	if all.Untried != 0 {
		t.Errorf("Untried = %d，期望 0：这些候选不是「没来得及试」，是压根不该试", all.Untried)
	}
	const want = "1 个候选渠道都不支持这次请求"
	if got := err.Error(); got != want {
		t.Errorf("错误文本 = %q，期望 %q", got, want)
	}
	// 判据来自目录声明而不是上游响应，但结论是同一件事：这条候选做不了
	// 这次请求。所以对上游错误类型敏感的调用方要认得出来（见 Errors.Unwrap）。
	if !errors.Is(err, channel.ErrUnsupported) {
		t.Error("应能被 errors.Is 认成 channel.ErrUnsupported")
	}
	if len(exec.calls) != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", len(exec.calls))
	}
}

// 流式那条路的出口与非流式同一处：一个候选都不剩时同样不发上游调用。
func TestChatStreamReportsProfileRejection(t *testing.T) {
	r := seededRouter(1)
	exec := &fakeStreamExecutor{results: map[int64]error{}}
	cands := []Candidate{candidate(1, "m", withProfile(&ModelProfile{Vision: declared(false)}))}
	req := toolRequest()
	req.Messages[0].Content = append(req.Messages[0].Content,
		channel.ContentBlock{Type: "image", ImageURL: "https://example.com/a.png"})

	_, err := r.ChatStream(context.Background(), exec, req, cands, "m")

	if got := err.Error(); got != "1 个候选渠道都不支持这次请求" {
		t.Errorf("错误文本 = %q", got)
	}
	if len(exec.calls) != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", len(exec.calls))
	}
}

// 被目录挡下的候选不出现在尝试序列里——不是「排在后面」，是根本没进去。
//
// 判据是执行器收到的那几次调用：换渠道重试一旦轮到它，这里就会多一条记录，
// 而请求本身会带着一次注定失败的调用与它的费用。
func TestChatSkipsProfileRejectedCandidate(t *testing.T) {
	r := seededRouter(1)
	exec := &fakeExecutor{results: map[int64]error{}}
	cands := []Candidate{
		candidate(1, "m", withPriority(10), withProfile(&ModelProfile{Tools: declared(false)})),
		candidate(2, "m", withPriority(1), withProfile(&ModelProfile{Tools: declared(true)})),
	}

	res, err := r.Chat(context.Background(), exec, toolRequest(), cands, "m")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Channel.ID != 2 {
		t.Errorf("选中的渠道 = %d，期望 2（1 号被目录挡下）", res.Channel.ID)
	}
	if len(exec.calls) != 1 || exec.calls[0] != 2 {
		t.Errorf("执行器调用 = %v，期望只调 2 号一次", exec.calls)
	}
}

// 排除理由不全是目录判据时，不说「都不支持这次请求」。
//
// 那句话会让人去查模型目录，而这里真正要改的是某个渠道的额度。
func TestChatKeepsGenericMessageWhenOtherReasonsExist(t *testing.T) {
	r := seededRouter(1)
	exec := &fakeExecutor{results: map[int64]error{}}
	cands := []Candidate{
		candidate(1, "m", withProfile(&ModelProfile{Tools: declared(false)})),
		candidate(2, "m", withQuota(quotaLeft(0))),
	}

	_, err := r.Chat(context.Background(), exec, toolRequest(), cands, "m")

	if got := err.Error(); got != "没有可用的渠道" {
		t.Errorf("错误文本 = %q，期望「没有可用的渠道」", got)
	}
}

// ── Needs ───────────────────────────────────────────────────────────────

// NeedsOf 从请求里读出用到了哪些能力。
//
// 参数形态的信号（工具定义、推理参数）与内容形态的信号（工具调用块、
// 图片块、思维链块）都算：它们在做同一件事，只是写法不同。
func TestNeedsOfReadsRequest(t *testing.T) {
	cases := []struct {
		name  string
		req   *channel.Request
		check func(t *testing.T, got Needs)
	}{
		{
			name: "工具定义",
			req:  &channel.Request{Tools: []channel.Tool{{Name: "t"}}},
			check: func(t *testing.T, got Needs) {
				if !got.Tools {
					t.Error("Tools 应为真")
				}
			},
		},
		{
			name: "工具调用与结果块",
			req: &channel.Request{Messages: []channel.Message{{Role: "assistant",
				Content: []channel.ContentBlock{{Type: "tool_use", ToolName: "t"}}}}},
			check: func(t *testing.T, got Needs) {
				if !got.Tools {
					t.Error("Tools 应为真")
				}
			},
		},
		{
			name: "图片块",
			req: &channel.Request{Messages: []channel.Message{{Role: "user",
				Content: []channel.ContentBlock{{Type: "image", ImageBase64: "aGk="}}}}},
			check: func(t *testing.T, got Needs) {
				if !got.Vision {
					t.Error("Vision 应为真")
				}
			},
		},
		{
			name: "推理参数",
			req:  &channel.Request{ReasoningEffort: "high"},
			check: func(t *testing.T, got Needs) {
				if !got.Reasoning {
					t.Error("Reasoning 应为真")
				}
			},
		},
		{
			name: "思维链块",
			req: &channel.Request{Messages: []channel.Message{{Role: "assistant",
				Content: []channel.ContentBlock{{Type: "thinking", Text: "想一下"}}}}},
			check: func(t *testing.T, got Needs) {
				if !got.Reasoning {
					t.Error("Reasoning 应为真")
				}
			},
		},
		{
			name: "纯文本",
			req: &channel.Request{Model: "m", MaxTokens: 321,
				Messages: []channel.Message{{Role: "user",
					Content: []channel.ContentBlock{{Type: "text", Text: "你好，这是一段话。"}}}}},
			check: func(t *testing.T, got Needs) {
				if got.Tools || got.Vision || got.Reasoning {
					t.Errorf("纯文本请求不该要任何能力: %+v", got)
				}
				if got.InputTokens <= 0 {
					t.Errorf("InputTokens = %d，期望正数", got.InputTokens)
				}
				if got.MaxOutputTokens != 321 {
					t.Errorf("MaxOutputTokens = %d，期望 321", got.MaxOutputTokens)
				}
			},
		},
		{
			name: "空请求",
			req:  &channel.Request{Model: "m"},
			check: func(t *testing.T, got Needs) {
				if got.InputTokens != 0 || got.MaxOutputTokens != 0 {
					t.Errorf("空请求的 token 数应为 0: %+v", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.check(t, NeedsOf(tc.req)) })
	}
}
