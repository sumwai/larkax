package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// 本文件钉限额：调用超时、内存上限、入参出参的字节上限，
// 以及「宿主函数阻塞时 Interrupt 无效」这条实测出来的边界。
//
// 注意：宿主函数跑在独立 goroutine 里（runtimeState.call 把 rt.Call 放在
// 另一个 goroutine 上跑，卡住的调用才不会拖住请求），所以宿主函数写、
// 测试读的每个变量都必须用 channel 或 atomic 同步。

// 死循环必须被 CallTimeout 拉回来，且在时限附近返回（不是永远挂住）。
// 这里的余量给得很宽（2 倍 + 1 秒），因为要认证的是「它确实会回来」，
// 而不是精确的调度延迟。
// 死循环被调用超时拉回来。
func TestInfiniteLoopIsCutOffByCallTimeout(t *testing.T) {
	const 超时 = 40 * time.Millisecond
	m, err := Compile("p", `export default { onRequest() { while (true) {} } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{CallTimeout: 超时})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil)
	花了 := time.Since(开始)

	if f == nil {
		t.Fatal("死循环应当返回失败")
	}
	if !errors.Is(f, ErrCallTimeout) {
		t.Errorf("Kind = %v，想要 ErrCallTimeout", f.Kind)
	}
	if f.Plugin != "p" || f.Hook != "onRequest" {
		t.Errorf("失败信息应带上插件与 hook，实际 Plugin=%q Hook=%q", f.Plugin, f.Hook)
	}
	// 关键：不能早于时限返回（那说明不是超时路径），也不能晚太多（那说明拉不回来）。
	if 花了 < 超时 {
		t.Errorf("耗时 %v 早于超时 %v，这条走的不是超时路径", 花了, 超时)
	}
	if 花了 > 超时*2+time.Second {
		t.Errorf("耗时 %v 远超超时 %v，中断没能及时生效", 花了, 超时)
	}
	if !rt.Unusable() {
		t.Error("超时之后 runtime 应当是不可用的")
	}
}

// 超时之后同一个 runtime 不再被使用：它的调用栈可能还在跑。
// 超时后的runtime不再被调用。
func TestRuntimeIsNotUsedAfterTimeout(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { while (true) {} }, onUsage() { return {}; } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{CallTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil); f == nil {
		t.Fatal("死循环应当超时")
	}
	f := rt.Call(context.Background(), "onUsage", map[string]any{"a": 1}, nil)
	if f == nil {
		t.Fatal("超时之后的调用应当失败")
	}
	if !errors.Is(f, ErrRunaway) {
		t.Errorf("Kind = %v，想要 ErrRunaway", f.Kind)
	}
}

// 内存上限超了走 Interrupt 路径，**脚本的 try/catch 拦不住它**。
// 这条用一个「catch 包住的分配循环」来钉：如果 catch 能拦住，
// 插件就能把内存错误吃掉、照常返回结果，宿主永远不知道它超了。
// 内存超限时 try/catch 拦不住。
func TestMemoryOverrunIsNotCatchableByTryCatch(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    try {
      const 一堆 = [];
      while (true) { 一堆.push(new Array(1000).fill("占位占位占位占位占位占位占位占位")); }
    } catch (e) {
      // 这条路径**不该**被走到。
      return { 抓到了: String(e) };
    }
    return { 抓到了: false };
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{MemoryBytes: 1 << 20, CallTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	花了 := time.Since(开始)

	if f == nil {
		t.Fatalf("内存超限应当返回失败，实际出参 = %v（说明 catch 把它吃掉了）", out)
	}
	if !errors.Is(f, ErrMemoryLimit) {
		t.Errorf("Kind = %v，想要 ErrMemoryLimit", f.Kind)
	}
	// errors.Is 能穿过 Failure 一直认到 moejs 的那一类，排查时有用。
	if !errors.Is(f, moejs.ErrMemoryLimit) {
		t.Errorf("errors.Is(f, moejs.ErrMemoryLimit) = false，Cause = %v", f.Cause)
	}
	if _, 抓到了 := out["抓到了"]; 抓到了 {
		t.Errorf("脚本的 catch 吃掉了内存错误: %v", out)
	}
	// 时间上不能拖到调用超时：内存超限是分配时就发现，不是等超时。
	if 花了 >= 5*time.Second {
		t.Errorf("耗时 %v 说明它是被 CallTimeout 拉回来的，不是内存超限", 花了)
	}
	if !rt.Unusable() {
		t.Error("内存超限之后 runtime 应当是不可用的")
	}
}

// 内存上限要真的在限额附近停下，而不是一路吃到进程 OOM。
// 这条钉住「它确实是被内存上限挡住的」：错误信息里带上了具体上限值，
// 而且耗时远小于调用预算。
// 内存上限的错误信息带上上限值。
func TestMemoryLimitErrorCarriesTheLimitValue(t *testing.T) {
	const 上限 = 1 << 20
	m, err := Compile("p", `
export default {
  onRequest() {
    const 一堆 = [];
    while (true) { 一堆.push(new Array(1000).fill("占位占位占位占位占位占位占位占位")); }
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{MemoryBytes: 上限, CallTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil)
	花了 := time.Since(开始)
	if f == nil {
		t.Fatal("内存超限应当返回失败")
	}
	if !errors.Is(f, ErrMemoryLimit) {
		t.Fatalf("Kind = %v，想要 ErrMemoryLimit", f.Kind)
	}
	if f.Cause == nil {
		t.Fatal("Cause 不该为空")
	}
	文本 := f.Cause.Error()
	if !strings.Contains(文本, "memory limit exceeded") {
		t.Errorf("Cause 应说明是内存超限，实际 %q", 文本)
	}
	if !strings.Contains(文本, "1048576") {
		t.Errorf("Cause 应带上上限值 1048576，实际 %q", 文本)
	}
	if 花了 > time.Second {
		t.Errorf("耗时 %v，说明它是被 CallTimeout 拉回来的而不是内存超限", 花了)
	}
}

// 入参超过 MaxInputBytes：报 ErrTooLarge，**而且插件根本没被调用**。
// 用一个会改模块变量的插件来断言这一点——如果插件跑过，
// 下一次调用就能看到计数变了。
// 入参超限时插件完全没被调用。
func TestInputOverLimitNeverCallsPlugin(t *testing.T) {
	m, err := Compile("p", `
let 调用次数 = 0;
export default {
  onRequest(ctx) { 调用次数++; return { 调用次数: 调用次数 }; },
  onUsage() { return { 调用次数: 调用次数 }; },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	大入参 := map[string]any{"内容": string(make([]byte, 200))}
	payload, err := json.Marshal(大入参)
	if err != nil {
		t.Fatal(err)
	}
	上限 := len(payload) - 1 // 比实际差一个字节，确保越界

	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{MaxInputBytes: 上限})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	f := rt.Call(context.Background(), "onRequest", 大入参, nil)
	if f == nil {
		t.Fatal("入参超限应当返回失败")
	}
	if !errors.Is(f, ErrTooLarge) {
		t.Errorf("Kind = %v，想要 ErrTooLarge", f.Kind)
	}
	// Cause 里带上实际字节数与上限，排查时才知道超了多少。
	if f.Cause == nil || !strings.Contains(f.Cause.Error(), "入参") {
		t.Errorf("Cause 应说明是入参超限，实际 %v", f.Cause)
	}

	// 插件侧的证据：计数器仍是 0。
	var out map[string]any
	if f := rt.Call(context.Background(), "onUsage", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("onUsage 不该失败: %v", f)
	}
	if out["调用次数"] != float64(0) {
		t.Errorf("调用次数 = %v，说明插件被跑过了（入参超限应当在进插件之前就挡住）", out["调用次数"])
	}
}

// 入参的判据是「字节数 > 上限」：恰好等于上限要放行，少一个字节就拒。
// 这条钉住比较的方向，别把边界判反。
// 入参字节上限的边界是严格大于。
func TestInputByteLimitBoundaryIsStrictlyGreater(t *testing.T) {
	m, err := Compile("p", `export default { onRequest(ctx) { return { 字段数: Object.keys(ctx).length }; } };`)
	if err != nil {
		t.Fatal(err)
	}
	入参 := map[string]string{"k": "v"}
	payload, err := json.Marshal(入参)
	if err != nil {
		t.Fatal(err)
	}

	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{MaxInputBytes: len(payload)})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", 入参, &out); f != nil {
		t.Errorf("恰好等于上限应当放行，实际 %v", f)
	}
	if out["字段数"] != float64(1) {
		t.Errorf("插件没拿到入参: %v", out)
	}

	rt2, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{MaxInputBytes: len(payload) - 1})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	f := rt2.Call(context.Background(), "onRequest", 入参, nil)
	if f == nil || !errors.Is(f, ErrTooLarge) {
		t.Errorf("少一个字节就应当拒绝，实际 %v", f)
	}
}

// 无法序列化的入参同样算 ErrTooLarge（它进不了边界）。
// 无法序列化的入参报ErrTooLarge。
func TestUnserializableInputReturnsErrTooLarge(t *testing.T) {
	m, err := Compile("p", `export default { onRequest(ctx) { return {}; } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	f := rt.Call(context.Background(), "onRequest", func() {}, nil)
	if f == nil {
		t.Fatal("函数没法序列化成 JSON，应当失败")
	}
	if !errors.Is(f, ErrTooLarge) {
		t.Errorf("Kind = %v，想要 ErrTooLarge", f.Kind)
	}
	if f.Cause == nil || !strings.Contains(f.Cause.Error(), "无法序列化") {
		t.Errorf("Cause 应说明是序列化失败，实际 %v", f.Cause)
	}
}

// 出参超过 MaxOutputBytes：ErrTooLarge，并且**出参保持原样**——
// 半截写入比不写更危险。
// 出参超限时报ErrTooLarge且不写出参。
func TestOutputOverLimitReturnsErrTooLargeWithoutWritingOutput(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { return { 内容: "x".repeat(4000) }; } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{MaxOutputBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	out := struct {
		Kept int    `json:"旧值"`
		Body string `json:"内容"`
	}{Kept: 9, Body: "保留"}
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	if f == nil {
		t.Fatal("出参超限应当返回失败")
	}
	if !errors.Is(f, ErrTooLarge) {
		t.Errorf("Kind = %v，想要 ErrTooLarge", f.Kind)
	}
	if out.Kept != 9 || out.Body != "保留" {
		t.Errorf("出参被改动了: %+v", out)
	}
	// 字节超限是「这次的数据太大」，不是 runtime 坏了：它仍然可用。
	if rt.Unusable() {
		t.Error("字节超限不该把 runtime 弄成不可用")
	}
	var 第二次 map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &第二次); f == nil || !errors.Is(f, ErrTooLarge) {
		t.Errorf("第二次调用应当同样报 ErrTooLarge（runtime 仍可用），实际 %v", f)
	}
}

// **宿主函数阻塞时 Interrupt 无效。**
//
// 这条在钉什么：CallTimeout 是 50ms，而宿主函数要跑 200ms。
// 超时路径已经调了 Interrupt、Call 也已经返回，但那个宿主函数**仍在跑**，
// 跑满它自己的 200ms，一秒都没少。（实测过：宿主 sleep 300ms 配 50ms deadline
// 时跑满 301ms。这不是本包的问题，是协作式中断的固有性质——插不进去
// 一段正在执行的 Go 代码。）
//
// 由此得到三条结论，也就是这条测试的三个断言：
//
//  1. Call 在超时附近返回，**不等**那个卡住的宿主函数；
//  2. 宿主函数没有被 Interrupt 打断，它跑满了自己那 200ms；
//  3. 它跑完之后，插件再调宿主能力时**被宿主层拒绝了**——
//     因为那时调用预算已经用尽。所以「宿主函数自带 deadline」不是优化，
//     而是唯一能让插件在超时之后不再做事的机制：Interrupt 靠不住，
//     只能由每个宿主函数自己看 ctx 然后收手。
//
// 第 3 条是这一版实现有意做的：超时之后不再执行任何宿主能力调用。
// 没有它的话，插件在超时后继续查日历、写 KV，而请求那边早就返回了。
//
// 宿主函数写的东西全部走 channel，因为它跑在另一个 goroutine 上。
// 宿主函数阻塞时Interrupt无效。
func TestInterruptDoesNotStopBlockedHostFunc(t *testing.T) {
	const (
		宿主耗时 = 200 * time.Millisecond
		调用超时 = 50 * time.Millisecond
	)
	m, err := Compile("p", `
export default {
  onRequest() {
    globalThis.__larkax.慢();
    globalThis.__larkax.之后的语句();
    return {};
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	慢函数结束 := make(chan struct{})
	之后被调用 := make(chan struct{}, 1)
	host := Host{Capabilities: map[string]any{
		"慢": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			// 这段**故意**不看 ctx：它在模拟一个卡住的宿主函数。
			// 看 ctx 的版本由另外两条测试覆盖。
			time.Sleep(宿主耗时)
			close(慢函数结束)
			return moejs.Undefined(), nil
		}),
		"之后的语句": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			select {
			case 之后被调用 <- struct{}{}:
			default:
			}
			return moejs.Undefined(), nil
		}),
	}}
	rt, err := m.NewRuntime(context.Background(), host, []string{"慢", "之后的语句"}, Limits{CallTimeout: 调用超时})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil)
	返回用时 := time.Since(开始)

	if f == nil || !errors.Is(f, ErrCallTimeout) {
		t.Fatalf("应当按调用超时返回，实际 %v", f)
	}
	if !rt.Unusable() {
		t.Error("宿主函数卡住导致的超时之后，runtime 应当是不可用的")
	}
	// Call 是在超时附近返回的：它不等宿主函数。
	if 返回用时 > 调用超时*2+time.Second {
		t.Errorf("Call 返回用了 %v，远超超时 %v", 返回用时, 调用超时)
	}

	// 关键断言一：宿主函数跑满了自己那 200ms，**没有被 Interrupt 打断**。
	select {
	case <-慢函数结束:
		if 花了 := time.Since(开始); 花了 < 宿主耗时 {
			t.Errorf("宿主函数在 %v 就结束了，早于它自己的 %v——它被打断了？", 花了, 宿主耗时)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("宿主函数两秒内没结束")
	}

	// 关键断言二：它跑完之后，插件再调宿主能力**被拒绝了**。
	//
	// 这一条是「宿主函数自带 deadline」的价值所在。延时到超时之后，
	// 插件那句 `globalThis.__larkax.之后的语句()` 照旧会执行到宿主这一侧，
	// 但 wrapFunc 先看 ctx 就返回错误——实现本体一次都没被进入。
	if len(之后被调用) != 0 {
		t.Error("超时之后宿主函数的实现本体仍被进入了：插件在超时后还能做实事")
	}
}

// 与上一条相对：宿主函数**自己尊重 ctx** 时不会拖住调用。
// 但它的 ctx 来自 NewRuntime（请求 ctx），不是来自 CallTimeout——
// 见下一条测试。这里让请求 ctx 带上 deadline，宿主函数便能在预算内返回。
// 宿主函数尊重请求ctx时能及时返回。
func TestHostFuncRespectingRequestCtxReturnsInTime(t *testing.T) {
	const 宿主耗时 = 200 * time.Millisecond
	m, err := Compile("p", `
export default {
  onRequest() {
    const out = {};
    try { globalThis.__larkax.慢(); out.慢 = "正常返回"; } catch (e) { out.慢 = e.message; }
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{
		"慢": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			select {
			case <-ctx.Done():
				return moejs.Undefined(), context.Cause(ctx)
			case <-time.After(宿主耗时):
				return moejs.Undefined(), nil
			}
		}),
	}}

	// 请求 ctx 带上 50ms 的预算；Call 用的是另一个从不取消的 ctx，
	// 这样「谁先返回」只取决于宿主函数，不会与 Call 自己的 ctx.Done() 抢。
	请求ctx, 停请求 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer 停请求()
	rt, err := m.NewRuntime(请求ctx, host, []string{"慢"}, Limits{CallTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	花了 := time.Since(开始)

	// 插件自己 catch 住了，所以 Call 本身不算失败——这正是「宿主函数自己返回」
	// 的形态：插件有机会收拾，而不是被硬中断。
	if f != nil {
		t.Fatalf("插件 catch 了，Call 不该失败: %v", f)
	}
	if out["慢"] == "正常返回" {
		t.Error("宿主函数不该跑满 200ms")
	}
	if 花了 >= 宿主耗时 {
		t.Errorf("Call 用了 %v，宿主函数没有自己提前返回", 花了)
	}
}

// **CallTimeout 的预算会传到宿主函数的 ctx 里。**
//
// 这是「每个宿主函数自带 deadline」这条硬约束在沙箱侧的落点：
// 宿主阻塞时 Interrupt 无效，宿主函数唯一能靠的就是自己的 ctx 会结束。
// 如果这里给的是 NewRuntime 时那个 ctx，而调用方传了
// context.Background()（最常见的写法），宿主函数就拿到一个永不结束的
// ctx——那时没有任何一道防线，一次慢查询能把请求挂到上游超时。
// CallTimeout进入宿主函数的ctx。
func TestCallTimeoutReachesHostFuncCtx(t *testing.T) {
	const 调用超时 = 30 * time.Millisecond
	m, err := Compile("p", `export default { onRequest() { globalThis.__larkax.看(); return {}; } };`)
	if err != nil {
		t.Fatal(err)
	}
	type 观察 struct {
		有deadline bool
		ctx结束     bool
	}
	结果 := make(chan 观察, 1)
	host := Host{Capabilities: map[string]any{
		"看": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			var o 观察
			_, o.有deadline = ctx.Deadline()
			select {
			case <-ctx.Done():
				o.ctx结束 = true
			case <-time.After(150 * time.Millisecond):
			}
			结果 <- o
			return moejs.Undefined(), nil
		}),
	}}
	// 请求 ctx 没有 deadline，只有 CallTimeout。
	rt, err := m.NewRuntime(context.Background(), host, []string{"看"}, Limits{CallTimeout: 调用超时})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil)
	返回用时 := time.Since(开始)

	// **这条测试不钉 Call 的结局，只钉宿主函数拿到的 ctx。**
	//
	// 两个 30ms 的定时器（cctx 的 deadline 与 select 的 time.After）在同一个
	// 量级上赛跑，宿主函数又立刻响应 ctx 结束，于是 Call 是「超时失败」
	// 还是「宿主自己收手后正常完成」取决于调度——-race 下它会翻转。
	// 把其中一个结局写成断言，得到的就是一条按机器负载时红时绿的测试
	// （LESSONS 第 15 条：解释不通的现象不要靠调参数去糊）。
	//
	// 真正要断言的是下面那个观察：宿主函数拿到的 ctx 有 deadline、并且会结束。
	if f != nil && !errors.Is(f, ErrCallTimeout) {
		t.Fatalf("失败时只该是调用超时，实际 %v", f)
	}
	// 无论哪种结局，都不该远超预算：宿主函数不该跑满它自己那 150ms。
	if 返回用时 > 100*time.Millisecond {
		t.Errorf("Call 用了 %v，超过调用超时 %v 太多", 返回用时, 调用超时)
	}
	select {
	case o := <-结果:
		// **宿主函数的 ctx 必须带上调用超时。** 这一条是本包的核心保证：
		// 宿主阻塞时 Interrupt 无效（见上面那条），所以宿主函数唯一能靠的
		// 就是自己的 ctx 会结束。若这里拿到的是「永不结束的 ctx」，
		// 「每个宿主函数自带 deadline」就退化成调用方的义务——
		// 而调用方传 context.Background() 是最常见的写法。
		if !o.有deadline {
			t.Error("宿主函数的 ctx 没有 deadline：调用超时没有传下去，宿主只能靠 Interrupt，而它对阻塞的宿主无效")
		}
		if !o.ctx结束 {
			t.Error("宿主函数的 ctx 没有结束：它拿不到「该收手了」的信号")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("宿主函数没给出观察结果")
	}
}

// 客户端断开：ctx 被取消后 Call 返回 ErrClientCanceled，
// 与「插件自己超时」是两种失败，排查方向完全不同。
// 客户端取消时返回ErrClientCanceled。
func TestClientCancelReturnsErrClientCanceled(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { while (true) {} } };`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, 取消请求 := context.WithCancel(context.Background())
	// 调用超时给得很宽，确保它不会先于取消触发。
	rt, err := m.NewRuntime(ctx, Host{}, nil, Limits{CallTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	go func() { time.Sleep(50 * time.Millisecond); 取消请求() }()
	开始 := time.Now()
	f := rt.Call(ctx, "onRequest", map[string]any{"a": 1}, nil)
	花了 := time.Since(开始)

	if f == nil {
		t.Fatal("取消应当返回失败")
	}
	if !errors.Is(f, ErrClientCanceled) {
		t.Errorf("Kind = %v，想要 ErrClientCanceled", f.Kind)
	}
	if errors.Is(f, ErrCallTimeout) {
		t.Error("客户端取消不该被当成调用超时")
	}
	if f.Cause == nil || !errors.Is(f.Cause, context.Canceled) {
		t.Errorf("Cause 应当是 context.Canceled，实际 %v", f.Cause)
	}
	// 是「取消」把它拉回来的，不是超时（超时是 30s）。
	if 花了 > 5*time.Second {
		t.Errorf("耗时 %v 说明是别的原因返回的", 花了)
	}
	if !rt.Unusable() {
		t.Error("取消之后 runtime 应当是不可用的")
	}
}

// ctx 在**建 runtime 之前**就取消了：NewRuntime 直接失败，插件代码一次都不跑。
//
// 这条与上面那条是一对：上面是「跑到一半客户端断开」，这里是「请求早就没了」。
// 顶层求值也在限额之内、用的是同一个 ctx（见 loadTopLevel），所以这种情形在
// 更早的一步就被发现——比「先把顶层跑起来、到 Call 才发现」更省事。
//
// 顶层写了一段忙等：ctx 已经结束，而 select 里 `ctx.Done()` 那一支是**立刻**
// 可选的，忙等保证 `Load` 不会抢在它前面跑完。没有这一段的话，这条用例会变成
// 「谁先到」的竞态（LESSONS 第 15 条）。
// 调用前ctx已取消时不建runtime。
func TestCanceledCtxBeforeCallSkipsRuntimeCreation(t *testing.T) {
	m, err := Compile("p", `
if (typeof globalThis.__larkax !== "undefined") { const t = Date.now(); while (Date.now() - t < 200) {} }
export default {
  onRequest() { globalThis.__larkax.不该被调到(); return {}; },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, 取消请求 := context.WithCancel(context.Background())
	取消请求()
	var 调到 atomic.Int64
	host := Host{Capabilities: map[string]any{
		"不该被调到": HostFunc(func(c context.Context, args []moejs.Value) (moejs.Value, error) {
			调到.Add(1)
			return moejs.Undefined(), nil
		}),
	}}
	rt, err := m.NewRuntime(ctx, host, []string{"不该被调到"}, Limits{})
	if err == nil {
		rt.Close()
		t.Fatal("ctx 已取消时不该建出 runtime")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("错误 = %v，想要包住 context.Canceled", err)
	}
	if got := 调到.Load(); got != 0 {
		t.Errorf("ctx 已取消，宿主函数却被调了 %d 次", got)
	}
}

// Limits 的零值与负值都当作「没设」，落到 DefaultLimits。
// 这条钉住「忘了设限额 ≠ 没有限额」——后者是无限循环吃满 CPU 的那一种。
// 限额的零值与负值都落到默认值。
func TestZeroAndNegativeLimitsFallBackToDefaults(t *testing.T) {
	for _, 输入 := range []Limits{
		{},
		{CallTimeout: -1, MemoryBytes: -1, MaxInputBytes: -1, MaxOutputBytes: -1},
	} {
		got := 输入.normalize()
		if got != DefaultLimits {
			t.Errorf("normalize(%+v) = %+v，想要 DefaultLimits %+v", 输入, got, DefaultLimits)
		}
	}
	// 显式设的值不能被改掉。
	显式 := Limits{CallTimeout: 7 * time.Second, MemoryBytes: 1 << 20, MaxInputBytes: 5, MaxOutputBytes: 6}
	if got := 显式.normalize(); got != 显式 {
		t.Errorf("normalize 改动了显式设的值: %+v", got)
	}
	// 只设一项时，其余三项补默认。
	got := (Limits{CallTimeout: 7 * time.Second}).normalize()
	if got.CallTimeout != 7*time.Second {
		t.Errorf("CallTimeout = %v，想要 7s", got.CallTimeout)
	}
	if got.MemoryBytes != DefaultLimits.MemoryBytes || got.MaxInputBytes != DefaultLimits.MaxInputBytes || got.MaxOutputBytes != DefaultLimits.MaxOutputBytes {
		t.Errorf("只设一项时其余应补默认，实际 %+v", got)
	}
}

// newRuntime 是 runtimeState 的唯一构造点，它一定把限额规范化，
// 于是 Call 里读到的 MaxInputBytes 恒大于 0。
//
// 这条钉的是一个前提，不是行为：Call 里 `limit > 0` 这一半判据
// （「0 表示不限」）在现有构造路径上不可达——`Limits` 的零值与负值
// 都在 newRuntime 里变成了默认上限。所以要改那一处判据，
// 先得有一条能构造出零上限 runtimeState 的路径，而今天没有。
func TestNewRuntimeAlwaysNormalizesInputLimit(t *testing.T) {
	for _, 输入 := range []int{0, -1, -1024} {
		rs := newRuntime("p", Limits{MaxInputBytes: 输入})
		if got := rs.limits.MaxInputBytes; got != DefaultLimits.MaxInputBytes {
			t.Errorf("MaxInputBytes=%d 时 newRuntime 得到 %d，想要默认上限 %d", 输入, got, DefaultLimits.MaxInputBytes)
		}
	}
	// 显式给的正值原样保留，不被默认值盖掉。
	if got := newRuntime("p", Limits{MaxInputBytes: 5}).limits.MaxInputBytes; got != 5 {
		t.Errorf("显式的 MaxInputBytes=5 变成了 %d", got)
	}
}

// runtimeState.injectCtx 恒非 nil。
//
// 这条同样钉一个前提：runtimeState.call 里 `parent != nil && parent != ctx`
// 的前一半恒真（newRuntime 置 background，inject 只在收到非 nil 时覆盖，
// 而生产代码里 inject 的调用方只有 NewRuntime）。于是那段判据的实际内容
// 只剩 `parent != ctx`；两者相等时它做的事——先兜一次取消、再登记一次
// AfterFunc——是 merged 本来就从 ctx 继承取消的重复。
func TestInjectCtxIsNeverNil(t *testing.T) {
	rs := newRuntime("p", Limits{})
	if rs.injectCtx == nil {
		t.Fatal("newRuntime 之后的 injectCtx 是 nil")
	}
	// 注入时传 nil 不覆盖已有的 ctx：NewRuntime 之外的调用方不该把槽清空。
	// staticcheck 默认禁止传 nil ctx（SA1012），而这一格测的正是收到 nil 时的行为。
	if err := rs.inject(nil, Host{}, nil); err != nil { //nolint:staticcheck // 见上：这里测的就是 nil 这一格
		t.Fatalf("inject(nil) 不该失败: %v", err)
	}
	if rs.injectCtx == nil {
		t.Error("inject(nil) 之后 injectCtx 变成了 nil")
	}
	// 「非 nil 会覆盖」那一半同样要钉：用一个可区分的 ctx，
	// 断言槽里装的就是它，而不是原来那个 DefaultBackground。
	注入ctx := context.WithValue(context.Background(), 注入标记{}, "注入")
	if err := rs.inject(注入ctx, Host{}, nil); err != nil {
		t.Fatalf("inject 不该失败: %v", err)
	}
	if rs.injectCtx != 注入ctx {
		t.Error("inject 收到非 nil ctx 之后没有覆盖 injectCtx")
	}
}

// 注入标记是上面那条用例里 WithValue 的键，只用来让 ctx 可区分。
type 注入标记 struct{}

// 注入时的父 ctx 还活着、而这次调用的 ctx 是另一个时，
// 合并出来的调用 ctx 必须还是活的。
//
// 这条钉 runtimeState.call 里 context.Cause(parent) 的判向：判反了会在
// 父 ctx 活着时走 cancelMerged(nil)，于是 cctx 一出生就是已取消的，
// wrapFunc 在进入实现之前就拒绝它——宿主函数一次都进不去。
//
// 两个 ctx 必须不同：相同时那段合并不进入（parent != ctx 为假）。
// 父 ctx 在这里始终不取消，所以判据与调度顺序无关：变异版从第一刻起
// 就拒绝宿主能力，不存在「谁先到」的窗口。
func TestAliveParentCtxKeepsCallCtxAlive(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { globalThis.__larkax.看(); return {}; } };`)
	if err != nil {
		t.Fatal(err)
	}
	父ctx, 停父 := context.WithCancel(context.Background())
	defer 停父()

	var 进入 atomic.Int64
	看到 := make(chan error, 1)
	host := Host{Capabilities: map[string]any{
		"看": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			进入.Add(1)
			// 非阻塞发送：万一实现被进入不止一次，第二次不能把宿主调用
			// 卡在这里——卡住的宿主函数收不到 Interrupt，用例会白等满超时。
			select {
			case 看到 <- context.Cause(ctx):
			default:
			}
			return moejs.Undefined(), nil
		}),
	}}
	rt, err := m.NewRuntime(父ctx, host, []string{"看"}, Limits{CallTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// 调用的 ctx 与注入时的父 ctx 不同——正常调用方就是这样传的。
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil)

	// 先看宿主函数有没有被进入：变异版无论被中断在哪一步，这个计数都是 0。
	if got := 进入.Load(); got != 1 {
		t.Fatalf("宿主函数被进入 %d 次，想要 1：父 ctx 活着时这次调用的 ctx 不该被取消", got)
	}
	if f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	select {
	case cause := <-看到:
		if cause != nil {
			t.Errorf("宿主函数拿到的 ctx 已经有 cause（%v），想要没有", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("宿主函数没有回报它拿到的 ctx 状态")
	}
}

// DefaultLimits 的取值本身就是契约：改它是一次行为变更。
// 五秒是「一次请求的预算大部分要给上游」的结果；
// 内存 64 MiB 相对一份请求体是宽的，但足以挡住一个吃光进程内存的循环。
// 默认限额的取值。
func TestDefaultLimitsValues(t *testing.T) {
	if DefaultLimits.CallTimeout != 5*time.Second {
		t.Errorf("CallTimeout = %v，想要 5s", DefaultLimits.CallTimeout)
	}
	if DefaultLimits.MemoryBytes != 64<<20 {
		t.Errorf("MemoryBytes = %d，想要 64 MiB", DefaultLimits.MemoryBytes)
	}
	if DefaultLimits.MaxInputBytes != 4<<20 || DefaultLimits.MaxOutputBytes != 4<<20 {
		t.Errorf("字节上限 = %d/%d，想要都是 4 MiB", DefaultLimits.MaxInputBytes, DefaultLimits.MaxOutputBytes)
	}
}

// TestZeroInputLimitMeansNoLimit 钉住「入参上限为 0 表示不设上限」。
//
// newRuntime 会把 0 归一成默认值，所以正常构造走不到这一格；这里直接改
// state（同包用例）模拟一个没有上限的 runtime。两版只在 limit=0 且负载非空
// 时分得开：`> 0` 放行，`>= 0` 把 0 当成上限，任何非空入参都判超限。
func TestZeroInputLimitMeansNoLimit(t *testing.T) {
	m, err := Compile("p", `export default { onRequest(ctx) { return { 字段数: Object.keys(ctx).length }; } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.state.limits.MaxInputBytes = 0

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]string{"k": "v"}, &out); f != nil {
		t.Fatalf("上限为 0 时不该按体积拒: %v", f)
	}
	if out["字段数"] != float64(1) {
		t.Errorf("插件没拿到入参: %v", out)
	}
}

// TestCallWithoutInjectedContext 钉住「没有父 ctx 也能调」。
//
// injectCtx 由 inject 置上；这里直接清掉，模拟一个没有注入过宿主能力的
// runtime。两版只在 parent 为 nil 时分得开：`&&` 跳过合并，`||` 会去读
// context.Cause(nil)。没有父 ctx 是允许的状态，不是每次调用都经过 inject。
func TestCallWithoutInjectedContext(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { return { 有: true }; } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.state.injectCtx = nil

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("没有父 ctx 时不该失败: %v", f)
	}
	if out["有"] != true {
		t.Errorf("插件没有被调到: %v", out)
	}
}
