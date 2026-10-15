package tokens

import "testing"

func TestAvailableEncodingsCoversMainstream(t *testing.T) {
	// 能力上限里必须包含覆盖主流流量的两种；否则「能精确计数」这件事
	// 与 Prepare 加载的范围会产生矛盾。
	got := AvailableEncodings()
	seen := map[string]bool{}
	for _, e := range got {
		seen[e] = true
	}
	for _, want := range []string{"o200k_base", "cl100k_base", "r50k_base", "p50k_base", "p50k_edit", "gpt2"} {
		if !seen[want] {
			t.Errorf("能力列表里缺少 %s, 得到 %v", want, got)
		}
	}
}
