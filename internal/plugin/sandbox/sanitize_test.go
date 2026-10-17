package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// 本文件钉宿主与插件之间的**错误出口**。
//
// 宿主函数返回的错误会变成插件 `catch` 到的 `e.message`，而宿主依赖的错误
// 原文可能带连接串、驱动细节、上游地址——那些既对插件无用，又不该离开宿主。
// 所以这条边界上只有分类后的固定文案能过去，原文只进 Host.OnError。
//
// 探针用的是一串**一眼看得出不该外泄**的记号，而不是像真凭证的东西：
// 断言的是「它没出现」，用真形态的取值反而会让红线扫描器报警。

// 不该跨到插件侧的记号。它在宿主日志里必须出现，在插件侧必须不出现。
const 不该外泄 = "dsn-fragment-MUST-NOT-LEAK"

// 未分类的宿主错误：插件只拿到固定文案，原文进宿主日志。
func Test宿主错误原文不回显给插件(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    try { globalThis.__larkax.查(); return { 看到: "没抛" }; }
    catch (e) { return { 看到: String(e && e.message) }; }
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}

	原文 := make(chan error, 4)
	host := Host{
		Capabilities: map[string]any{
			"查": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
				// 形态就是真实实现会给出的那一种：依赖错误被 %w 包了一层。
				return moejs.Undefined(), fmt.Errorf("查询失败: %w", errors.New(不该外泄))
			}),
		},
		OnError: func(name string, err error) { 原文 <- err },
	}
	rt, err := m.NewRuntime(context.Background(), host, []string{"查"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("插件 catch 了，Call 不该失败: %v", f)
	}
	看到 := fmt.Sprint(out["看到"])
	if strings.Contains(看到, 不该外泄) {
		t.Errorf("插件看到了宿主错误的原文: %q", 看到)
	}
	// 分类码必须还在：插件要能据此分支，否则消毒就等于把信息全丢了。
	if !strings.Contains(看到, CodeUnclassified) {
		t.Errorf("插件应看到分类码 %s，实际 %q", CodeUnclassified, 看到)
	}
	// 而原文必须留在宿主这一侧——不然排查就没有依据了。
	select {
	case err := <-原文:
		if !strings.Contains(err.Error(), 不该外泄) {
			t.Errorf("宿主日志里应当有原文，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("原文没有交给 OnError")
	}
}

// 宿主显式分过类的错误原样过去，但**原文仍然只进宿主日志**。
//
// 这一条与上一条相对：上一条是「没分类的默认拒绝」，这一条是
// 「分过类的按分类给」，两者合起来才是完整的规则。
func Test已分类的宿主错误按分类给插件(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    try { globalThis.__larkax.查(); return { 看到: "没抛" }; }
    catch (e) { return { 看到: String(e && e.message) }; }
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}

	原文 := make(chan error, 4)
	host := Host{
		Capabilities: map[string]any{
			"查": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
				return moejs.Undefined(), &CapabilityError{
					Code:    CodeUnavailable,
					Message: "读取插件 KV 失败",
					Cause:   errors.New(不该外泄),
				}
			}),
		},
		OnError: func(name string, err error) { 原文 <- err },
	}
	rt, err := m.NewRuntime(context.Background(), host, []string{"查"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("插件 catch 了，Call 不该失败: %v", f)
	}
	看到 := fmt.Sprint(out["看到"])
	if strings.Contains(看到, 不该外泄) {
		t.Errorf("插件看到了宿主错误的原文: %q", 看到)
	}
	if !strings.Contains(看到, CodeUnavailable) || !strings.Contains(看到, "读取插件 KV 失败") {
		t.Errorf("插件应看到 %s 与它的固定文案，实际 %q", CodeUnavailable, 看到)
	}
	select {
	case err := <-原文:
		if !strings.Contains(err.Error(), 不该外泄) {
			t.Errorf("宿主日志里应当有原文，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("原文没有交给 OnError")
	}
}

// 能力表配错（某个成员不是函数）的文案同样要过去：那是集成方写错了，
// 而插件作者看到的应该是一句能指向问题的话。
func Test能力不是函数时文案保留(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    try { globalThis.__larkax.整块(); return { 看到: "没抛" }; }
    catch (e) { return { 看到: String(e && e.message) }; }
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{Capabilities: map[string]any{"整块": 42}},
		[]string{"整块"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("插件 catch 了，Call 不该失败: %v", f)
	}
	看到 := fmt.Sprint(out["看到"])
	if !strings.Contains(看到, CodeMisconfigured) || !strings.Contains(看到, "不是一个函数") {
		t.Errorf("插件应看到 %s 与它的说明，实际 %q", CodeMisconfigured, 看到)
	}
}

// 调用预算用尽时宿主函数被拒，宿主日志里要能看出「是超时」而不是别的。
//
// 插件那一侧在超时路径上拿不到结果（Call 会失败并丢掉出参），所以这里
// 只钉宿主日志。分类信息必须穿过 CapabilityError 的包装仍然可见——
// 它决定上层把这次失败记成 ErrHostTimeout 还是 ErrCallTimeout，
// 而两者的排查方向完全不同。
func Test超时拒绝宿主调用时原文报给宿主日志(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    globalThis.__larkax.慢();
    const out = {};
    try { globalThis.__larkax.之后的语句(); out.之后 = "没抛"; }
    catch (e) { out.之后 = String(e && e.message); }
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	原文 := make(chan error, 4)
	host := Host{
		Capabilities: map[string]any{
			"慢": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
				// 故意不看 ctx：模拟一个卡住的宿主函数。
				time.Sleep(200 * time.Millisecond)
				return moejs.Undefined(), nil
			}),
			"之后的语句": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
				return moejs.Undefined(), nil
			}),
		},
		OnError: func(name string, err error) { 原文 <- err },
	}
	rt, err := m.NewRuntime(context.Background(), host, []string{"慢", "之后的语句"}, Limits{CallTimeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// 超时之后 Call 会失败并丢掉出参，所以插件看到的文案要从宿主日志那一侧
	// 间接验证：这里只断言 Call 的结局与宿主日志里的原文。
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil); f == nil {
		t.Fatal("宿主函数卡住应当让 Call 超时")
	} else if !errors.Is(f, ErrCallTimeout) {
		t.Errorf("Kind = %v，想要 ErrCallTimeout", f.Kind)
	}
	select {
	case err := <-原文:
		// 拒绝的理由是**预算用尽**（ErrCallTimeout），不是客户端断开——
		// 两者都会让宿主函数在进门处被挡下，而处置完全不同。
		if !errors.Is(err, ErrCallTimeout) {
			t.Errorf("宿主日志里的原文 = %v，想要包住 ErrCallTimeout", err)
		}
		if errors.Is(err, ErrClientCancelled) {
			t.Error("预算用尽不该被记成客户端断开")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超时之后的宿主调用没有报给 OnError")
	}
}

// 宿主那一侧（host.expired）返回的是**未分类**的错误，分类只在 pluginFacing
// 这里做一次。这条把那张映射表钉住：同一件事在两条路径上必须是同一个码，
// 否则插件按码分支时会看到两个答案。
//
// 「宿主超时」这一条尤其重要：expired 报的是 ErrHostTimeout，而不是在那一层
// 自己拼一个码——拼的话，wrapFunc 进门那一次判断会给出 CodeTimeout，
// 两处就不一致了。
func Test宿主侧的超时与取消按同一套码分类(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"宿主超时", fmt.Errorf("%w: %w", ErrHostTimeout, context.DeadlineExceeded), CodeTimeout},
		{"调用超时", ErrCallTimeout, CodeTimeout},
		{"预算耗尽", context.DeadlineExceeded, CodeTimeout},
		{"请求结束", context.Canceled, CodeCanceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pluginFacing(tc.err)
			if got.Code != tc.want {
				t.Errorf("码 = %q，想要 %q", got.Code, tc.want)
			}
			// 分类不是把原文丢掉：它要能进宿主日志。
			if got.Cause == nil {
				t.Error("Cause 不能为空：原文要能交给 Host.OnError")
			}
		})
	}
}
