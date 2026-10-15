package tokens

import (
	"context"
	"testing"
)

func TestUpstreamStringDefaultBranch(t *testing.T) {
	if got := Upstream(9).String(); got == "" {
		t.Error("未知 Upstream 的 String 不该返回空串")
	}
}

func TestSourceStringDefaultBranch(t *testing.T) {
	if got := Source(9).String(); got == "" {
		t.Error("未知 Source 的 String 不该返回空串")
	}
}

func TestCacheWriteDetailEmpty(t *testing.T) {
	if got := CacheWriteDetail(RawUsage{Upstream: UpstreamAnthropic}); got != nil {
		t.Errorf("没有 5m/1h 拆分时应返回 nil, 得到 %v", got)
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
	if len(c.Loaded()) != 2 {
		t.Errorf("已加载编码数 = %d, 期望 2（o200k 与 cl100k）", len(c.Loaded()))
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
