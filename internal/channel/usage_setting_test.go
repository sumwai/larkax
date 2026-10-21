package channel

import "testing"

// UsageCompletionIncludesReasoning 的判据：缺声明沿用「已含」，显式写 false
// 才改成互斥。
//
// 「缺声明」与「写了 true」是同一件事，而它们与「写了 false」必须分开——
// 一条渠道没有声明时不该被当成互斥口径（那会让 OpenAI 系上游少收一次），
// 写坏的值也不该把整条渠道的请求都变成失败。
func TestUsageCompletionIncludesReasoning(t *testing.T) {
	cases := []struct {
		name    string
		setting map[string]any
		want    bool
	}{
		{"没有 setting", nil, true},
		{"空的 setting", map[string]any{}, true},
		{"没有 usage 块", map[string]any{"reasoning_effort_map": map[string]any{"none": "off"}}, true},
		{"声明 false", usageSetting(false), false},
		{"声明 true", usageSetting(true), true},
		{
			// 块写成了别的形状：按「没声明」处理。
			name:    "usage 块不是对象",
			setting: map[string]any{"usage": "completion_includes_reasoning=false"},
			want:    true,
		},
		{
			name:    "声明值不是布尔",
			setting: map[string]any{"usage": map[string]any{"completion_includes_reasoning": "false"}},
			want:    true,
		},
		{
			// 键写错了：没有读者，同样是「没声明」。
			name:    "键名拼错",
			setting: map[string]any{"usage": map[string]any{"completion_includes_reasonings": false}},
			want:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UsageCompletionIncludesReasoning(tc.setting); got != tc.want {
				t.Errorf("UsageCompletionIncludesReasoning(%v) = %v，期望 %v", tc.setting, got, tc.want)
			}
		})
	}
}

// usageSetting 拼一块 {"usage": {"completion_includes_reasoning": v}} 形状的
// 渠道配置。键名在用例里独立写一遍：它是这条判据的输入，引用常量的话源与
// 用例一起改而口径可以悄悄变。
func usageSetting(v bool) map[string]any {
	return map[string]any{
		"usage": map[string]any{"completion_includes_reasoning": v},
	}
}
