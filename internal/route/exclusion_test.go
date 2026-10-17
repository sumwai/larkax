package route

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 每一条排除都要带上机器可读的分类：留痕那一侧记的就是它（见 Exclusion.Code）。
//
// 分类的取值是**对外的一层契约**——那几个字面量写在 route.ExclusionCode 里，
// docs/request-retention.md 给的是释义与字面量两栏，排障的人按它聚合。所以这里
// 逐档钉死：只断言「不为空」的话，把 health 改成 quota 不会有任何东西报出来，
// 而那一格正是「上次是不是额度问题」这类判断的唯一依据。
//
// 同一档里再钉一句「给人看的原因不为空」：分类说的是哪一档，现场（模型名、
// 上限、连续失败次数）在那句话里（见 Exclusion.Reason）。
func TestSelectRecordsExclusionCode(t *testing.T) {
	cases := []struct {
		name  string
		cand  Candidate
		needs Needs
		want  ExclusionCode
	}{
		{
			name: "渠道配置不全",
			cand: Candidate{Channel: channel.Channel{ID: 1}},
			want: CodeIneligible,
		},
		{
			name: "渠道没声明支持这个模型",
			cand: candidate(2, "claude-sonnet"),
			want: CodeModelNotDeclared,
		},
		{
			name:  "目录说这个模型做不了工具调用",
			cand:  candidate(3, "m", withProfile(&ModelProfile{Tools: declared(false)})),
			needs: Needs{Tools: true},
			want:  CodeCapability,
		},
		{
			name: "目录说这个模型不在候选走的那条端点上",
			cand: candidate(4, "m",
				withProfile(&ModelProfile{Endpoints: []channel.Endpoint{channel.EndpointMessages}}),
				withEndpoint(channel.EndpointChatCompletions)),
			want: CodeCapability,
		},
		{
			name:  "超出目录声明的输出上限",
			cand:  candidate(5, "m", withProfile(&ModelProfile{OutputLimit: 4096})),
			needs: Needs{MaxOutputTokens: 8192},
			want:  CodeLimit,
		},
		{
			name: "健康度",
			cand: candidate(6, "m", withHealth(Health{ConsecutiveFailures: consecutiveFailureCutoff})),
			want: CodeHealth,
		},
		{
			name: "额度",
			cand: candidate(7, "m", withQuota(quotaLeft(0))),
			want: CodeQuota,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := seededRouter(1).Select([]Candidate{tc.cand}, "m", tc.needs)
			if len(plan.Excluded) != 1 {
				t.Fatalf("排除记录 = %+v，期望一条", plan.Excluded)
			}
			got := plan.Excluded[0]
			if got.Code != tc.want {
				t.Errorf("分类 = %q，期望 %q（它是留痕里的取值）", got.Code, tc.want)
			}
			if got.Reason == "" {
				t.Error("给人的那句原因不该为空——现场在那句话里，分类只说明是哪一档")
			}
		})
	}
}

// 分类是**枚举**：取值必须能过留痕那一侧的枚举词检查。
//
// 上游那些「看起来像枚举词」的字符串会被逐字符检查（见 internal/snapshot 的
// isEnumWord：只认 ASCII 字母数字与 _ - . / :）。分类里混进一个中文词或一段
// 用户输入，它在骨架里就会退成一个文本占位符——记录还在，但读不出是哪一档，
// 而这种失效不会有任何报错。
func TestExclusionCodeIsEnumWord(t *testing.T) {
	codes := []ExclusionCode{
		CodeIneligible,
		CodeModelNotDeclared,
		CodeCapability,
		CodeLimit,
		CodeHealth,
		CodeQuota,
	}
	seen := map[ExclusionCode]bool{}
	for _, c := range codes {
		if c == "" {
			t.Error("分类不该是空值：空值经留痕那一档会落成一个长度为 0 的占位符")
			continue
		}
		if seen[c] {
			t.Errorf("分类 %q 出现了两次——两个判据会被读成同一个", c)
		}
		seen[c] = true
		for _, r := range string(c) {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
			if !ok {
				t.Errorf("分类 %q 里有 %q：留痕那一侧只认小写字母、数字与下划线", c, r)
			}
		}
	}
}

// 被筛掉的候选要跟着结果一起交回调用方——留痕就是从这里读它。
//
// 少了这一步，网关手上只剩 Attempts（真的发出去过的那几个），而「三个候选
// 为什么只用一个」的答案恰好落在两者之间。
func TestResultCarriesExcluded(t *testing.T) {
	exec := &fakeExecutor{resp: &channel.Response{UpstreamModel: "m"}}

	r := seededRouter(1)
	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"}, []Candidate{
		candidate(1, "m"),
		candidate(2, "m", withHealth(Health{ConsecutiveFailures: consecutiveFailureCutoff})),
	}, "m")
	if err != nil {
		t.Fatalf("Chat 出错: %v", err)
	}
	if len(exec.calls) != 1 || exec.calls[0] != 1 {
		t.Fatalf("执行器收到的调用 = %v，期望只有渠道 1", exec.calls)
	}
	if res.Channel.ID != 1 {
		t.Fatalf("选中的渠道 = %d，期望 1", res.Channel.ID)
	}
	if len(res.Excluded) != 1 || res.Excluded[0].ChannelID != 2 {
		t.Fatalf("结果里的排除记录 = %+v，期望记下渠道 2", res.Excluded)
	}
	if res.Excluded[0].Code != CodeHealth {
		t.Errorf("分类 = %q，期望 %q", res.Excluded[0].Code, CodeHealth)
	}
}

// 一个候选都没剩下时，被筛掉的那些只能挂在**错误**上交出去。
//
// 那一支里选路直接返回错误、不进 Result，所以「三条渠道一条都没用上」这条
// 最该被看见的记录，如果只挂在 Result 上，恰好是唯一看不见的。
func TestErrorsCarryExcludedWhenNothingLeft(t *testing.T) {
	exec := &fakeExecutor{}

	r := seededRouter(1)
	res, err := r.Chat(context.Background(), exec, &channel.Request{Model: "m"}, []Candidate{
		candidate(1, "m", withHealth(Health{ConsecutiveFailures: consecutiveFailureCutoff})),
		candidate(2, "m", withQuota(quotaLeft(0))),
	}, "m")
	if err == nil {
		t.Fatal("一个候选都不剩时应当报错")
	}
	if res != nil {
		t.Errorf("这一支不该交回结果，实际 %+v", res)
	}
	if len(exec.calls) != 0 {
		t.Errorf("一个候选都不剩时不该调用执行器，实际调用了 %v", exec.calls)
	}

	var routeErr *Errors
	if !errors.As(err, &routeErr) {
		t.Fatalf("错误类型 = %T，期望 *route.Errors", err)
	}
	if len(routeErr.Excluded) != 2 {
		t.Fatalf("错误上的排除记录 = %+v，期望两条", routeErr.Excluded)
	}
	want := []ExclusionCode{CodeHealth, CodeQuota}
	for i, w := range want {
		if routeErr.Excluded[i].Code != w {
			t.Errorf("第 %d 条的分类 = %q，期望 %q", i+1, routeErr.Excluded[i].Code, w)
		}
	}
	// 排除依据一个都不来自模型目录时，错误文本仍是原来那一句。
	if got := err.Error(); got != "没有可用的渠道" {
		t.Errorf("错误文本 = %q，期望「没有可用的渠道」", got)
	}
}
