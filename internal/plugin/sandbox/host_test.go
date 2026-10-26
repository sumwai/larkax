package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// 本文件钉「宿主能力怎么注入、插件能不能越权、runtime 的生命周期」。

// 空函数，只用来占一个能力位置。
func noopHostFunc() HostFunc {
	return func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
		return moejs.Undefined(), nil
	}
}

// 白名单是「没列出来就不给」：allowed 为空时插件调用任何能力都拿到
// 「未启用」，而**不是**一个能用的能力。这条钉住默认拒绝。
// 白名单为空时任何能力都不可用。
func TestEmptyAllowlistDeniesEveryCapability(t *testing.T) {
	m, err := Compile("p", `
import { log, calendar, kv } from "larkax";
export default {
  onRequest() {
    const out = {};
    try { log("info", "m"); out.log = "可用"; } catch (e) { out.log = e.message; }
    try { calendar.weekdayIn("CN"); out.calendar = "可用"; } catch (e) { out.calendar = e.message; }
    try { kv.get("k"); out.kv = "可用"; } catch (e) { out.kv = e.message; }
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{
		"log":      map[string]any{"log": noopHostFunc()},
		"calendar": map[string]any{"weekdayIn": noopHostFunc()},
		"kv":       map[string]any{"get": noopHostFunc()},
	}}
	rt, err := m.NewRuntime(context.Background(), host, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	for _, name := range []string{"log", "calendar", "kv"} {
		got := fmt.Sprint(out[name])
		if got == "可用" {
			t.Errorf("allowed 为空时 %s 仍可用", name)
		}
		// 一个能力都没注入时 SDK 报「未注入」，注入了一部分时报「未启用」，
		// 两者都说明这个能力没到手。
		if !strings.Contains(got, "未启用") && !strings.Contains(got, "未注入") {
			t.Errorf("%s 的错误信息应说明能力没被启用，实际 %q", name, got)
		}
	}
	if len(rt.state.injected) != 0 {
		t.Errorf("injected = %v，allowed 为空时不该注入任何能力", rt.state.injected)
	}
}

// 白名单只列了一部分时，列出来的可用、没列出来的不可用。
// 白名单只放行列出来的能力。
func TestAllowlistExposesOnlyListedCapabilities(t *testing.T) {
	m, err := Compile("p", `
import { log, calendar } from "larkax";
export default {
  onRequest() {
    const out = {};
    try { log("info", "m"); out.log = "可用"; } catch (e) { out.log = e.message; }
    try { calendar.weekdayIn("CN"); out.calendar = "可用"; } catch (e) { out.calendar = e.message; }
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{
		"log":      map[string]any{"log": noopHostFunc()},
		"calendar": map[string]any{"weekdayIn": noopHostFunc()},
	}}
	rt, err := m.NewRuntime(context.Background(), host, []string{"log"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if out["log"] != "可用" {
		t.Errorf("log 在白名单里，应当可用，实际 %v", out["log"])
	}
	if out["calendar"] == "可用" {
		t.Error("calendar 不在白名单里，不该可用")
	}
	if !strings.Contains(fmt.Sprint(out["calendar"]), "calendar") {
		t.Errorf("calendar 的报错应点名是哪个能力没启用，实际 %v", out["calendar"])
	}
	if got := rt.state.injected; len(got) != 1 || !got["log"] {
		t.Errorf("injected = %v，想要只有 log", got)
	}
}

// 注入进去的能力名必须与白名单一致：多一个就是越权。
// 注入的能力名与白名单一致。
func TestInjectedCapabilityNamesMatchAllowlist(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    return { names: Object.getOwnPropertyNames(globalThis.__larkax) };
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{
		"log":      map[string]any{"log": noopHostFunc()},
		"calendar": map[string]any{"weekdayIn": noopHostFunc()},
		"kv":       map[string]any{"get": noopHostFunc()},
	}}
	rt, err := m.NewRuntime(context.Background(), host, []string{"calendar", "kv"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	names := out["names"].([]any)
	if len(names) != 2 {
		t.Fatalf("__larkax 的键 = %v，想要两个（calendar、kv）", names)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n.(string)] = true
	}
	if !got["calendar"] || !got["kv"] || got["log"] {
		t.Errorf("__larkax 的键 = %v，想要只有 calendar 与 kv", names)
	}
	if len(rt.state.injected) != 2 || !rt.state.injected["calendar"] || !rt.state.injected["kv"] {
		t.Errorf("injected = %v，想要 calendar 与 kv", rt.state.injected)
	}
}

// 白名单里写了 host 没有的能力名：不该报错，也不该凭空出现一个能力。
// 白名单里的名字host没提供时不影响启动。
func TestAllowlistedNameMissingFromHostDoesNotBreakStartup(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    const out = {};
    try { globalThis.__larkax.不存在的能力(); out.调到了 = true; } catch (e) { out.错误 = e.message; }
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{Capabilities: map[string]any{"log": noopHostFunc()}},
		[]string{"log", "不存在的能力"}, Limits{})
	if err != nil {
		t.Fatalf("白名单里有个 host 没有的名字不该让 NewRuntime 失败: %v", err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if out["调到了"] == true {
		t.Error("host 没提供这个能力，插件不该调到它")
	}
	if got := rt.state.injected; len(got) != 1 || !got["log"] {
		t.Errorf("injected = %v，想要只有 log", got)
	}
}

// 宿主没有任何能力时，插件连 __larkax 都看不到。
// 这条与「白名单为空」是两回事：那次是能力被挡住，这次是压根没注入。
// 没有能力时插件看不到能力命名空间。
func TestNoCapabilitiesMeansNoCapabilityNamespace(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { return { t: typeof globalThis.__larkax }; } };`)
	if err != nil {
		t.Fatal(err)
	}
	for _, allowed := range [][]string{nil, {}, {"log"}} {
		rt, err := m.NewRuntime(context.Background(), Host{}, allowed, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
			t.Fatalf("allowed=%v: 调用不该失败: %v", allowed, f)
		}
		if out["t"] != "undefined" {
			t.Errorf("allowed=%v: typeof __larkax = %v，想要 undefined", allowed, out["t"])
		}
		if len(rt.state.injected) != 0 {
			t.Errorf("allowed=%v: injected = %v，想要空", allowed, rt.state.injected)
		}
		rt.Close()
	}
}

// 能力表里写了一个不是 HostFunc 的值时，插件调用它只拿到错误，
// **不能 panic**：宿主 panic 会变成 *moejs.InternalError，把整个 runtime 一起拖死。
// 能力不是函数时插件拿到错误且runtime不崩。
func TestNonFuncCapabilityReturnsErrorWithoutPanickingRuntime(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    const out = {};
    try { globalThis.__larkax.整块(1); out.整块 = "可用"; } catch (e) { out.整块 = e.name + ": " + e.message; }
    try { globalThis.__larkax.命名空间.成员(); out.成员 = "可用"; } catch (e) { out.成员 = e.name + ": " + e.message; }
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{
		"整块":   42,                                   // 整个能力就不是函数
		"命名空间": map[string]any{"成员": []int{1, 2, 3}}, // 成员不是函数
	}}
	rt, err := m.NewRuntime(context.Background(), host, []string{"整块", "命名空间"}, Limits{})
	if err != nil {
		t.Fatalf("能力表写错不该让 NewRuntime 失败: %v", err)
	}
	defer rt.Close()

	var out map[string]any
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	if f != nil {
		t.Fatalf("插件自己 catch 了，Call 不该失败: %v", f)
	}
	for _, k := range []string{"整块", "成员"} {
		got := fmt.Sprint(out[k])
		if got == "可用" {
			t.Errorf("%s 不是函数却调用成功了", k)
		}
		if !strings.Contains(got, "不是一个函数") {
			t.Errorf("%s 的错误信息应说明它不是函数，实际 %q", k, got)
		}
	}
	// 关键：runtime 仍然可用，没有被 panic 拖死。
	if rt.Unusable() {
		t.Error("能力表写错不该把 runtime 弄成不可用")
	}
	var nth map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &nth); f != nil {
		t.Errorf("第二次调用也不该失败: %v", f)
	}
}

// 宿主函数拿到的 ctx 已经取消时，**实现本体不会被进入**：
// wrapFunc 在调用实现之前先看一次 context.Cause。
// 这条钉住「宿主函数不做事就返回」，是宿主侧超时的第一道防线。
//
// 这里直接调 wrapFunc 的产物，**不经过 rt.Call**：经过它的话，取消同时会让
// 那个 AfterFunc 去 Interrupt 插件执行，两者赛跑——插件可能在调到宿主函数
// 之前就被中断了，用例于是变成「谁先到」，跑一次绿跑一次红（LESSONS 第 15 条）。
// 这条要钉的是 wrapFunc 那一处判断，直接调它才是它该测的东西。
//
// 插件那一侧看到什么，由 sanitize_test.go 那几条钉（它们不依赖取消时机）。
// 宿主函数在ctx已取消时不进入实现。
func TestHostFuncWithCanceledCtxNeverEntersImpl(t *testing.T) {
	// 宿主函数若被进入，这个计数从另一条路径被写，所以用 atomic。
	var entered atomic.Int64
	original := make(chan error, 4)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	rs := newRuntime("p", Limits{})
	probe := wrapFunc(rs, canceled, "probe",
		HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			entered.Add(1)
			return moejs.Undefined(), nil
		}),
		func(name string, err error) { original <- err })

	_, err := probe(nil, moejs.Undefined(), nil)
	if err == nil {
		t.Fatal("ctx 已取消时宿主函数应当报错，而不是照常返回")
	}
	var ce *CapabilityError
	if !errors.As(err, &ce) || ce.Code != CodeCanceled {
		t.Errorf("错误 = %v，想要 %s 那一类", err, CodeCanceled)
	}
	if n := entered.Load(); n != 0 {
		t.Errorf("宿主函数被进入了 %d 次，想要 0", n)
	}
	// 原文进了宿主日志——插件侧看不见它，排查只能靠这一份。
	select {
	case originalErr := <-original:
		if !errors.Is(originalErr, context.Canceled) {
			t.Errorf("宿主日志里的原文 = %v，想要 context.Canceled", originalErr)
		}
	case <-time.After(time.Second):
		t.Fatal("宿主函数被拒绝时没有把原因报给 OnError")
	}

	// 同一个宿主函数在 ctx 正常时会被进入——否则上一条可能是别的原因造成的。
	rs2 := newRuntime("p", Limits{})
	probe2 := wrapFunc(rs2, context.Background(), "probe",
		HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			entered.Add(1)
			return moejs.Undefined(), nil
		}), nil)
	if _, err := probe2(nil, moejs.Undefined(), nil); err != nil {
		t.Fatalf("ctx 正常时不该报错: %v", err)
	}
	if n := entered.Load(); n != 1 {
		t.Errorf("ctx 正常时宿主函数应被进入 1 次，实际共 %d 次", n)
	}
}

// ctx 在**调用进行中**被取消时，宿主函数能看到取消并自己返回。
// 这条是「宿主函数自带 deadline」在宿主侧的实现方式：
// 沙箱的 Interrupt 拉不回来一个阻塞的宿主函数，只能靠它自己看 ctx。
// 宿主函数能在调用途中看到ctx取消。
func TestHostFuncSeesCtxCanceledMidCall(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { globalThis.__larkax.等(); return {}; } };`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sawCancel := make(chan error, 1)
	host := Host{Capabilities: map[string]any{
		"等": HostFunc(func(c context.Context, args []moejs.Value) (moejs.Value, error) {
			select {
			case <-c.Done():
				sawCancel <- context.Cause(c)
				return moejs.Undefined(), context.Cause(c)
			case <-time.After(2 * time.Second):
				sawCancel <- errors.New("等超时了")
				return moejs.Undefined(), nil
			}
		}),
	}}
	rt, err := m.NewRuntime(ctx, host, []string{"等"}, Limits{CallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	f := rt.Call(ctx, "onRequest", map[string]any{"a": 1}, nil)
	if f == nil {
		t.Fatal("客户端取消应当让 Call 失败")
	}
	if !errors.Is(f, ErrClientCanceled) {
		t.Errorf("Kind = %v，想要 ErrClientCanceled", f.Kind)
	}
	select {
	case err := <-sawCancel:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("宿主函数看到的 cause = %v，想要 context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("宿主函数始终没看到 ctx 取消")
	}
}

// 渠道密钥不进插件：这一层不往 Host 里放，插件就看不到。
// 这里构造「宿主手里有一份 credential，但白名单没放行」的情形：
// 白名单是最终判据，没列出来就不会注入。
// 未放进白名单的密钥字段不会进插件。
func TestCredentialFieldNotAllowlistedNeverReachesPlugin(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    const ns = globalThis.__larkax ? Object.getOwnPropertyNames(globalThis.__larkax) : [];
    return { ns: ns, cred: typeof (globalThis.__larkax && globalThis.__larkax.credential) };
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{
		"log":        map[string]any{"log": noopHostFunc()},
		"credential": map[string]any{"value": noopHostFunc()},
	}}
	rt, err := m.NewRuntime(context.Background(), host, []string{"log"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if out["cred"] != "undefined" {
		t.Errorf("typeof __larkax.credential = %v，想要 undefined", out["cred"])
	}
	for _, n := range out["ns"].([]any) {
		if n.(string) == "credential" {
			t.Errorf("__larkax 的键 = %v，不该含 credential", out["ns"])
		}
	}
}

// 每次请求新建 runtime，是「插件把上一个请求的数据留在模块变量里」
// 这个泄漏的防线。这条钉住：同一个 Module、两个 runtime，
// 第二个读不到第一个存下的 ctx。
// 新建runtime读不到上一个的数据。
func TestNewRuntimeCannotReadPreviousRuntimeState(t *testing.T) {
	m, err := Compile("p", `
let 上次的 = null;
export default {
  onRequest(ctx) { 上次的 = ctx; return { tag: ctx.tag }; },
  onUsage() { return { 上次的: 上次的 === null ? "无" : 上次的.tag }; },
};
`)
	if err != nil {
		t.Fatal(err)
	}

	rt1, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]any
	if f := rt1.Call(context.Background(), "onRequest", map[string]any{"tag": "第一个请求"}, &first); f != nil {
		t.Fatal(f)
	}
	if first["tag"] != "第一个请求" {
		t.Fatalf("第一个 runtime 的 onRequest 没拿到入参: %v", first)
	}
	rt1.Close()

	rt2, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	var second map[string]any
	if f := rt2.Call(context.Background(), "onUsage", map[string]any{"a": 1}, &second); f != nil {
		t.Fatal(f)
	}
	if second["上次的"] != "无" {
		t.Errorf("新 runtime 读到了上一个 runtime 的数据: %v", second)
	}
}

// 同一个 runtime 里的连续两次 Call **能**看到彼此的状态。
// 这不是漏洞，是同一个请求内的合法行为——把几个 hook 串在一次请求上时，
// 插件需要能在 hook 之间记住东西。
// 单独钉一条，免得有人以为「每请求新建 runtime」能防住同一请求内的状态。
// 同一个runtime的连续调用共享模块状态。
func TestCallsInOneRuntimeShareModuleState(t *testing.T) {
	m, err := Compile("p", `
let 计数 = 0;
export default {
  onRequest() { 计数++; return { 计数: 计数 }; },
  onRoute()   { 计数++; return { 计数: 计数 }; },
  onUsage()   { return { 计数: 计数 }; },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	for i, want := range []float64{1, 2, 2} {
		hook := []string{"onRequest", "onRoute", "onUsage"}[i]
		var out map[string]any
		if f := rt.Call(context.Background(), hook, map[string]any{"a": 1}, &out); f != nil {
			t.Fatalf("%s 调用失败: %v", hook, f)
		}
		if out["计数"] != want {
			t.Errorf("第 %d 次调用后 计数 = %v，想要 %v（同请求内状态应当共享）", i+1, out["计数"], want)
		}
	}
}

// Close 之后的调用必须稳定地返回 ErrRunaway，
// 而且不该再去碰已经放开的宿主值（出参保持原样）。
// Close 还要幂等：它会在 defer 里被调，重复调不该 panic。
// Close之后的调用返回ErrRunaway且幂等。
func TestCallAfterCloseReturnsErrRunawayAndCloseIsIdempotent(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { return { ok: true }; } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Unusable() {
		t.Fatal("刚建好的 runtime 不该是不可用的")
	}
	rt.Close()
	if !rt.Unusable() {
		t.Error("Close 之后 Unusable 应当为 true")
	}
	rt.Close() // 幂等：重复 Close 不该 panic

	out := map[string]any{"旧值": 1}
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	if f == nil {
		t.Fatal("Close 之后的调用应当失败")
	}
	if !errors.Is(f, ErrRunaway) {
		t.Errorf("Kind = %v，想要 ErrRunaway", f.Kind)
	}
	if len(out) != 1 || out["旧值"] != 1 {
		t.Errorf("失败时不该改动出参，实际 %v", out)
	}
}

// 并发：同一个 Module 被多个 goroutine 各自 NewRuntime 并 Call，
// 结果各自正确、无数据竞争。这条配 -race 跑才有意义。
// 多个goroutine各自建runtime互不干扰。
func TestGoroutinesWithOwnRuntimesDoNotInterfere(t *testing.T) {
	m, err := Compile("p", `
import { definePlugin } from "larkax";
export default definePlugin({
  onRequest(ctx) {
    // 每个请求把自己看到的模型名存进模块变量，同一请求的下一次调用读回来。
    globalThis.__上次 = ctx.model;
    return { model: ctx.model, 上次: globalThis.__上次 };
  },
});
`)
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("模型-%d", i)
			rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
			if err != nil {
				errs <- err
				return
			}
			defer rt.Close()
			for whichAttempt := 0; whichAttempt < 3; whichAttempt++ {
				var out map[string]any
				f := rt.Call(context.Background(), "onRequest", map[string]any{"model": want}, &out)
				if f != nil {
					errs <- f
					return
				}
				if out["model"] != want || out["上次"] != want {
					errs <- fmt.Errorf("goroutine %d 第 %d 次: %v", i, whichAttempt, out)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发用例失败: %v", err)
	}
}
