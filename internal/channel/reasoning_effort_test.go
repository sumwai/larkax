package channel

import "testing"

// effortMap 拼一张 {"reasoning_effort_map": 表} 形状的渠道配置。
func effortMap(table map[string]any) map[string]any {
	return map[string]any{reasoningEffortMapKey: table}
}

func TestReasoningEffortDialect(t *testing.T) {
	cases := []struct {
		name    string
		setting map[string]any
		effort  string
		want    string
	}{
		{
			name:   "没有 setting 时原样返回",
			effort: "none",
			want:   "none",
		},
		{
			name:    "配了别的键但没配映射表",
			setting: map[string]any{"supports_optional_param": true},
			effort:  "none",
			want:    "none",
		},
		{
			name:    "表里没有这个取值时原样返回",
			setting: effortMap(map[string]any{"high": "max"}),
			effort:  "none",
			want:    "none",
		},
		{
			name:    "映射成该渠道的方言",
			setting: effortMap(map[string]any{"none": "off"}),
			effort:  "none",
			want:    "off",
		},
		{
			name:    "映射成空串表示剔掉这个字段",
			setting: effortMap(map[string]any{"none": ""}),
			effort:  "none",
			want:    "",
		},
		{
			name:    "请求没带这个参数时不去查表",
			setting: effortMap(map[string]any{"": "off"}),
			effort:  "",
			want:    "",
		},
		{
			name:    "映射表本身不是对象时按没配处理",
			setting: map[string]any{reasoningEffortMapKey: "off"},
			effort:  "none",
			want:    "none",
		},
		{
			name:    "映射表是 null 时按没配处理",
			setting: map[string]any{reasoningEffortMapKey: nil},
			effort:  "none",
			want:    "none",
		},
		{
			name:    "某一项的值不是字符串时跳过该项",
			setting: effortMap(map[string]any{"none": true}),
			effort:  "none",
			want:    "none",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReasoningEffortDialect(tc.setting, tc.effort); got != tc.want {
				t.Errorf("ReasoningEffortDialect(%v, %q) = %q，期望 %q",
					tc.setting, tc.effort, got, tc.want)
			}
		})
	}
}
