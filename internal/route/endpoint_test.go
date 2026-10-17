package route

import (
	"context"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// withEndpoint 摆上这条候选把请求发到哪条路径。
//
// 空串（不经过这个助手）是「不知道」——库里那个渠道类型当前版本的二进制
// 不认识，端点判据不判它。要写「不知道」就别调这个助手：
// 用例里必须能把两者分开写出来，否则证明不了它们没被合并成一个零值。
func withEndpoint(e channel.Endpoint) func(*Candidate) {
	return func(c *Candidate) { c.Endpoint = e }
}

// ── 目录判据：端点 ──────────────────────────────────────────────────────
//
// 这一组钉的是「这条候选把请求发到哪条路径」与「目录说这个模型在哪几条
// 路径上被服务」对不对得上。
//
// 三态与能力那一栏同一套口径，而且**三档的处置各不相同**：
//
//	nil   没配过       → 放行（不知道，不判）
//	{}    一条都不支持 → 出局（任何候选都服务不了它）
//	非空  按集合判     → 候选不在里面就出局
//
// 「没配过」与「一条都不支持」并排摆着是这一组的重点：把 nil 读成空集合
// （或者反过来）会让一档悄悄变成另一档，而两者在选路里的结论正好相反。

func TestSelectRejectsCandidatesOnUndeclaredEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		profile  *ModelProfile
		endpoint channel.Endpoint
		want     string
	}{
		{
			name:     "目录里没配过端点",
			profile:  &ModelProfile{},
			endpoint: channel.EndpointMessages,
			want:     "",
		},
		{
			name:     "目录里声明一条都不支持",
			profile:  &ModelProfile{Endpoints: []channel.Endpoint{}},
			endpoint: channel.EndpointMessages,
			want:     "一条端点都不支持",
		},
		{
			name: "候选的端点就在目录里",
			profile: &ModelProfile{Endpoints: []channel.Endpoint{
				channel.EndpointChatCompletions, channel.EndpointMessages}},
			endpoint: channel.EndpointMessages,
			want:     "",
		},
		{
			name: "候选的端点不在目录里",
			profile: &ModelProfile{Endpoints: []channel.Endpoint{
				channel.EndpointChatCompletions}},
			endpoint: channel.EndpointMessages,
			want:     "只在这些端点上服务：/v1/chat/completions",
		},
		{
			name:     "候选没有端点",
			profile:  &ModelProfile{Endpoints: []channel.Endpoint{channel.EndpointChatCompletions}},
			endpoint: "",
			want:     "",
		},
		{
			name:     "目录里没有这个模型的记录",
			profile:  nil,
			endpoint: channel.EndpointMessages,
			want:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := seededRouter(1).Select(
				[]Candidate{candidate(1, "m", withProfile(tc.profile), withEndpoint(tc.endpoint))},
				"m", Needs{})

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
			got := plan.Excluded[0]
			if !strings.Contains(got.Reason, tc.want) {
				t.Errorf("排除理由 = %q，期望含 %q", got.Reason, tc.want)
			}
			// 端点不匹配是**目录判据**：与能力不匹配一样，一个候选都没试过，
			// 错误文本要说「这些候选做不了这次请求」而不是「都失败了」——
			// 后者会让人去查上游（见 Exclusion.Unsupported）。
			if !got.Unsupported {
				t.Errorf("端点不匹配应当标为目录判据: %+v", got)
			}
			// 理由要同时说清两侧：只报「这个模型只在那几条端点上服务」的话，
			// 看的人还得自己算出这条候选走的是哪条路径。
			if tc.endpoint != "" && !strings.Contains(got.Reason, string(tc.endpoint)) {
				t.Errorf("排除理由里没提这条候选走的端点 %q: %q", tc.endpoint, got.Reason)
			}
		})
	}
}

// 判据排在能力之后、上限之前：一条候选只记第一个命中的理由。
//
// 这次请求带工具（模型声明的工具能力是 false）而端点也对不上，报出来的
// 应当是能力那一条——顺序反了的话，一条「这个模型根本做不了工具调用」的
// 配置会被说成「端点不对」，而人照着去改端点，改完还是不通。
func TestEndpointRejectionComesAfterCapability(t *testing.T) {
	plan := seededRouter(1).Select(
		[]Candidate{candidate(1, "m",
			withEndpoint(channel.EndpointMessages),
			withProfile(&ModelProfile{
				Tools:     declared(false),
				Endpoints: []channel.Endpoint{channel.EndpointChatCompletions},
			}))},
		"m", Needs{Tools: true})

	if len(plan.Excluded) != 1 {
		t.Fatalf("排除 %d 条，期望 1 条: %+v", len(plan.Excluded), plan.Excluded)
	}
	if got := plan.Excluded[0].Reason; !strings.Contains(got, "不支持工具调用") {
		t.Errorf("排除理由 = %q，期望先报能力那一条", got)
	}
}

// 端点不匹配的候选不出现在尝试序列里——不是「排在后面」，是根本没进去。
//
// 与能力那一条同一条理由：进去就会多一次注定失败的上游调用，而那次调用
// 会真的花掉费用（上游按 unsupported_model 之外的东西计费，或者干脆记一次
// 请求）。
func TestChatSkipsEndpointRejectedCandidate(t *testing.T) {
	r := seededRouter(1)
	exec := &fakeExecutor{results: map[int64]error{}}
	cands := []Candidate{
		candidate(1, "m", withPriority(10),
			withEndpoint(channel.EndpointMessages),
			withProfile(&ModelProfile{Endpoints: []channel.Endpoint{channel.EndpointChatCompletions}})),
		candidate(2, "m", withPriority(1),
			withEndpoint(channel.EndpointChatCompletions),
			withProfile(&ModelProfile{Endpoints: []channel.Endpoint{channel.EndpointChatCompletions}})),
	}

	res, err := r.Chat(context.Background(), exec, toolRequest(), cands, "m")
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if res.Channel.ID != 2 {
		t.Errorf("选中的渠道 = %d，期望 2（1 号端点对不上）", res.Channel.ID)
	}
	if len(exec.calls) != 1 || exec.calls[0] != 2 {
		t.Errorf("执行器调用 = %v，期望只调 2 号一次", exec.calls)
	}
}

// 一个候选都不剩、且全部排除都来自端点这一条时，错误文本与「试过、上游
// 都说自己不支持」同一句——判据来自目录声明，但结论是同一件事。
func TestChatReportsEndpointRejectionWithoutCallingUpstream(t *testing.T) {
	r := seededRouter(1)
	exec := &fakeExecutor{results: map[int64]error{}}
	cands := []Candidate{candidate(1, "m",
		withEndpoint(channel.EndpointMessages),
		withProfile(&ModelProfile{Endpoints: []channel.Endpoint{channel.EndpointChatCompletions}}))}

	_, err := r.Chat(context.Background(), exec, toolRequest(), cands, "m")

	if err == nil {
		t.Fatal("应当报错")
	}
	if got, want := err.Error(), "1 个候选渠道都不支持这次请求"; got != want {
		t.Errorf("错误文本 = %q，期望 %q", got, want)
	}
	if len(exec.calls) != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次", len(exec.calls))
	}
}
