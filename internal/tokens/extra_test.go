package tokens

import (
	"context"
	"slices"
	"sort"
	"testing"
)

func TestSourceStringDefaultBranch(t *testing.T) {
	if got := Source(9).String(); got == "" {
		t.Error("未知 Source 的 String 不该返回空串")
	}
}

func TestPrepareContextCanceledMidway(t *testing.T) {
	// 在第一次加载之后再取消：确认循环里的取消检查生效，
	// 且不把已加载的编码丢掉（下一次 Prepare 不该重新加载）。
	c := NewCounter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Prepare(ctx); err == nil {
		t.Fatal("取消后的 context 应让 Prepare 返回错误")
	}
	// 取消发生在第一次 codecFor 之前，所以什么都没加载。
	if len(c.Loaded()) != 0 {
		t.Errorf("取消在加载前，不该有已加载编码, 得到 %v", c.Loaded())
	}
	// 之后正常 Prepare 应当成功。
	if _, err := c.Prepare(context.Background()); err != nil {
		t.Fatalf("正常 Prepare 应成功: %v", err)
	}
	// 判据从 preparedCodecs 派生，不写死数量：写死的话，改那个集合就要
	// 回来改这里，而漏改的表现是这条用例报一个与改动无关的数字。
	want := make([]string, 0, len(preparedCodecs))
	for _, enc := range preparedCodecs {
		want = append(want, string(enc))
	}
	sort.Strings(want)
	if got := c.Loaded(); !slices.Equal(got, want) {
		t.Errorf("已加载编码 = %v, 期望 %v（preparedCodecs 的全部）", got, want)
	}
}

// 预热之后，第一次精确计数不该再解开任何新词表——这正是 #259 要的性质：
// 那 25ms 落在启动期，而不是落在第一个用户的第一次调用上。
//
// 判据是**前后对比 Loaded()**，不是「Prepare 返回了 nil 错误」：后者只说明
// 加载那一步没报错，说明不了接下来那条请求路径用的是同一批词表。
func TestPreparedCounterCountsWithoutLoadingMore(t *testing.T) {
	c := NewCounter()
	if _, err := c.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	before := c.Loaded()

	// 两种编码各取一个模型名，把「预热的那两种」都走一遍。
	for _, model := range []string{"gpt-4o", "gpt-3.5-turbo"} {
		got, err := c.CountText(context.Background(), model, "hello world")
		if err != nil {
			t.Fatalf("CountText(%s) 失败: %v", model, err)
		}
		if got.Source != SourceTokenized {
			t.Errorf("模型 %s 的来源 = %s，期望 tokenized（预热之后应当走精确路径）",
				model, got.Source)
		}
	}

	if after := c.Loaded(); !slices.Equal(before, after) {
		t.Errorf("精确计数又解开了新词表: %v -> %v（预热没覆盖到它会走的那一种）", before, after)
	}
}

func TestEstimateRoundedZeroForEmpty(t *testing.T) {
	if got := estimateRounded("", tableOpenAI); got != 0 {
		t.Errorf("空文本 = %v, 期望 0", got)
	}
}

func TestCeil(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0, 0}, {0.1, 1}, {1, 1}, {1.0001, 2}, {2.5, 3}, {100, 100},
	} {
		if got := ceil(tc.in); got != tc.want {
			t.Errorf("ceil(%v) = %v, 期望 %v", tc.in, got, tc.want)
		}
	}
}

func TestMergeOutputSourceKeepsEstimate(t *testing.T) {
	// 先精确后估算：整体必须是估算。
	e := NewStreamEstimator(NewCounter(), "gpt-4o")
	e.mergeOutputSource(SourceTokenized)
	e.mergeOutputSource(SourceEstimated)
	if e.outputSource != SourceEstimated {
		t.Errorf("来源 = %v, 期望 estimated", e.outputSource)
	}
	// 反过来也一样。
	e2 := NewStreamEstimator(NewCounter(), "gpt-4o")
	e2.mergeOutputSource(SourceEstimated)
	e2.mergeOutputSource(SourceTokenized)
	if e2.outputSource != SourceEstimated {
		t.Errorf("来源 = %v, 期望 estimated", e2.outputSource)
	}
}
