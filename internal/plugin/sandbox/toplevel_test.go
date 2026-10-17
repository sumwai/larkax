package sandbox

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// 本文件钉「插件代码不止 rt.Call 那一段」这件事。
//
// 另外两段同样会跑插件代码：请求期的**顶层求值**（NewRuntime 里的 Load）
// 与**出参解码**（UnmarshalWith 会为 toJSON / getter / proxy 跑 JavaScript）。
// 两段都曾经在 CallTimeout 之外，于是一个死循环或者一次慢宿主调用可以
// 挂在那里，而「插件调用超时」这条限额完全管不到它们。
//
// 每条用例都同时钉两件事：**在时限附近返回**（不是永远挂着），
// 以及**返回的是失败**（不是「跑完了没报错」）。

// 顶层死循环必须被 CallTimeout 拉回来。
//
// 顶层代码在编译期也会被求值一次（inspect），所以探针必须写成
// 「只有宿主能力注入之后才发作」——编译期没有注入能力，
// 那一句被跳过，模块编译得过；请求期注入之后再求值就会挂住。
// 这个形态本身就是真实的：插件完全可以按「能力在不在」决定做什么。
func Test顶层死循环被调用超时拉回来(t *testing.T) {
	const 超时 = 40 * time.Millisecond
	m, err := Compile("p", `
if (typeof globalThis.__larkax !== "undefined") { while (true) {} }
export default { onRequest() { return {}; } };
`)
	if err != nil {
		t.Fatalf("这个模块应当编译得过（编译期没有注入能力，那一句被跳过）: %v", err)
	}
	host := Host{Capabilities: map[string]any{"探针": noopHostFunc()}}

	// 在被测代码挂住时，用例自己要能失败而不是跟着一起挂——
	// 所以它在另一个 goroutine 里跑，这里只等一个有界的窗口。
	type 结果 struct {
		rt  *Runtime
		err error
	}
	done := make(chan 结果, 1)
	开始 := time.Now()
	go func() {
		rt, err := m.NewRuntime(context.Background(), host, []string{"探针"}, Limits{CallTimeout: 超时})
		done <- 结果{rt: rt, err: err}
	}()

	select {
	case r := <-done:
		花了 := time.Since(开始)
		if r.err == nil {
			r.rt.Close()
			t.Fatal("顶层死循环应当让 NewRuntime 失败")
		}
		if !errors.Is(r.err, ErrCallTimeout) {
			t.Errorf("错误 = %v，想要 ErrCallTimeout", r.err)
		}
		// 走了超时路径，而不是别的原因提前返回。
		if 花了 < 超时 {
			t.Errorf("耗时 %v 早于超时 %v，这条走的不是超时路径", 花了, 超时)
		}
		if 花了 > 超时*2+time.Second {
			t.Errorf("耗时 %v 远超超时 %v，中断没能及时生效", 花了, 超时)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("NewRuntime 在 3 秒内没有返回：顶层求值不受 CallTimeout 约束")
	}
}

// 顶层求值用的就是调用方传进来的那个 ctx：请求已经结束，就没有必要再花时间
// 求值顶层。所以取消要能**立刻**把它拉回来，而不是等 CallTimeout 到点。
//
// 这条钉的是「ctx 有没有真的传下去」。把 loadTopLevel 的父 ctx 换回
// context.Background() 之后，下面这个探针会一直转到 CallTimeout（10s）才返回，
// 于是这条用例挂——这正是它存在的意义。
func Test顶层求值被请求ctx取消拉回来(t *testing.T) {
	const 调用超时 = 10 * time.Second
	m, err := Compile("p", `
if (typeof globalThis.__larkax !== "undefined") { while (true) {} }
export default { onRequest() { return {}; } };
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{"探针": noopHostFunc()}}

	ctx, 取消请求 := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); 取消请求() }()

	type 结果 struct {
		rt  *Runtime
		err error
	}
	done := make(chan 结果, 1)
	开始 := time.Now()
	go func() {
		rt, err := m.NewRuntime(ctx, host, []string{"探针"}, Limits{CallTimeout: 调用超时})
		done <- 结果{rt: rt, err: err}
	}()

	select {
	case r := <-done:
		花了 := time.Since(开始)
		if r.err == nil {
			r.rt.Close()
			t.Fatal("顶层死循环 + 请求已取消，NewRuntime 应当失败")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("错误 = %v，想要包住 context.Canceled", r.err)
		}
		// 是「取消」把它拉回来的，不是超时（超时是 10s）。
		if 花了 > 3*time.Second {
			t.Errorf("耗时 %v 说明是别的原因返回的（CallTimeout 是 %v）", 花了, 调用超时)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消没能把顶层求值拉回来：ctx 没有传进 loadTopLevel")
	}
}

// 顶层代码调宿主能力时，宿主函数拿到的 ctx 必须带 deadline。
//
// 「每个宿主函数自带 deadline」是宿主阻塞时唯一可靠的机制（Interrupt 对
// 阻塞的宿主函数无效）。顶层这一段曾经拿到的是注入时那个 ctx，
// 而调用方传 context.Background() 是最常见的写法——那时它永不结束。
func Test顶层求值的宿主函数拿到deadline(t *testing.T) {
	const 超时 = 300 * time.Millisecond
	m, err := Compile("p", `
if (typeof globalThis.__larkax !== "undefined") { globalThis.__larkax.看(); }
export default { onRequest() { return {}; } };
`)
	if err != nil {
		t.Fatal(err)
	}

	type 观察 struct {
		有deadline bool
		剩余        time.Duration
	}
	结果 := make(chan 观察, 1)
	host := Host{Capabilities: map[string]any{
		"看": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			// 立刻返回，不阻塞：这条用例钉的是「ctx 长什么样」，
			// 让宿主函数卡住会把「谁先到」变成一场竞态（LESSONS 第 15 条）。
			var o 观察
			dl, ok := ctx.Deadline()
			o.有deadline = ok
			if ok {
				o.剩余 = time.Until(dl)
			}
			结果 <- o
			return moejs.Undefined(), nil
		}),
	}}

	rt, err := m.NewRuntime(context.Background(), host, []string{"看"}, Limits{CallTimeout: 超时})
	if err != nil {
		t.Fatalf("宿主函数立刻返回，顶层不该失败: %v", err)
	}
	defer rt.Close()

	select {
	case o := <-结果:
		if !o.有deadline {
			t.Error("顶层调用宿主能力时 ctx 没有 deadline：它拿不到「该收手了」的信号")
		}
		// 有 deadline 还不够，它得是**这次求值**的那个预算，
		// 而不是某个更远的时刻（例如调用方自己的超时）。
		if o.剩余 <= 0 || o.剩余 > 超时 {
			t.Errorf("deadline 剩余 %v，想要 (0, %v]", o.剩余, 超时)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("顶层没有调到宿主函数")
	}
}

// 顶层超时之后，被抛弃的那段插件代码**仍会继续跑**——Interrupt 拦不住一个
// 阻塞的宿主函数（它只在引擎的检查点生效，而调一个原生函数不是检查点）。
// 所以它随后调宿主能力时必须被兜住：要么在门口被拒，要么进去时 ctx 已经结束。
//
// 这条钉的是「callCtx 挂在哪」。写成调用方 goroutine 上的 defer 时，超时返回
// 的那一刻 callCtx 就被清掉，而 Load 还在另一个 goroutine 里跑：残余代码随后
// 调的宿主函数落到 hostCtx 的 fallback 上，拿到注入时那个请求 ctx——这里传的
// 是 context.Background()，于是它永不结束，宿主函数照常执行。
//
// 探针形状：顶层先调一个睡 300ms、不看 ctx 的宿主函数（把超时逼出来），
// 再调一个记录自己 ctx 的宿主函数。第二次调用的结局只有两种可能，两种都算
// 被兜住了；**放行且 ctx 还能用**才是这条要抓的那一种。
func Test顶层超时后残余代码的宿主调用被兜住(t *testing.T) {
	const 超时 = 50 * time.Millisecond
	m, err := Compile("p", `
if (typeof globalThis.__larkax !== "undefined") {
  globalThis.__larkax.慢();    // 不看 ctx，睡 300ms，把这次求值拖过超时
  globalThis.__larkax.之后();  // 残余代码走到这里
}
export default { onRequest() { return {}; } };
`)
	if err != nil {
		t.Fatal(err)
	}

	type 观察 struct {
		有deadline bool
		剩余        time.Duration
	}
	被进入 := make(chan 观察, 1)
	被拒 := make(chan error, 4)

	host := Host{
		Capabilities: map[string]any{
			"慢": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
				time.Sleep(300 * time.Millisecond)
				return moejs.Undefined(), nil
			}),
			"之后": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
				var o 观察
				dl, ok := ctx.Deadline()
				o.有deadline = ok
				if ok {
					o.剩余 = time.Until(dl)
				}
				被进入 <- o
				return moejs.Undefined(), nil
			}),
		},
		OnError: func(name string, err error) {
			if name == "之后" {
				被拒 <- err
			}
		},
	}

	_, err = m.NewRuntime(context.Background(), host, []string{"慢", "之后"}, Limits{CallTimeout: 超时})
	if err == nil {
		t.Fatal("顶层被宿主函数拖过超时，NewRuntime 应当失败")
	}
	if !errors.Is(err, ErrCallTimeout) {
		t.Errorf("错误 = %v，想要 ErrCallTimeout", err)
	}

	// 残余代码在 ~300ms 处才走到第二次调用，这里只等一个有界的窗口。
	select {
	case o := <-被进入:
		if !o.有deadline || o.剩余 > 0 {
			t.Errorf("残余代码的宿主调用被放行，而且 ctx 还能用：有 deadline=%v 剩余=%v",
				o.有deadline, o.剩余)
		}
	case err := <-被拒:
		// 在门口被拒：这是修好之后的正常路径。
		if !errors.Is(err, ErrCallTimeout) && !errors.Is(err, context.Canceled) {
			t.Errorf("拒绝的理由 = %v，想要「这次调用的预算已经结束」", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("残余代码在 3 秒内没有走到第二次宿主调用。" +
			"如果 moejs 的 Interrupt 已经能打断宿主函数，这条用例就观察不到东西了，得换个形态")
	}
}

// 出参解码里写的死循环同样要被 CallTimeout 拉回来。
//
// UnmarshalWith 会执行插件代码：值带 toJSON 时它走 AppendJSON 那条路，
// 而那条路是跑 JavaScript。解码曾经发生在 call() 返回之后——那时
// deadline 已经撤销、callCtx 也清空了，于是这段代码既跑不完也没有超时。
func Test出参解码的死循环被调用超时拉回来(t *testing.T) {
	const 超时 = 40 * time.Millisecond
	m, err := Compile("p", `
export default {
  onRequest() { return { text: "ok", toJSON() { while (true) {} } }; },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{CallTimeout: 超时})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	花了 := time.Since(开始)

	if f == nil {
		t.Fatalf("解码死循环应当返回失败，实际出参 = %v", out)
	}
	if !errors.Is(f, ErrCallTimeout) {
		t.Errorf("Kind = %v，想要 ErrCallTimeout", f.Kind)
	}
	if 花了 < 超时 {
		t.Errorf("耗时 %v 早于超时 %v，这条走的不是超时路径", 花了, 超时)
	}
	if 花了 > 超时*2+time.Second {
		t.Errorf("耗时 %v 远超超时 %v，解码没有被限额管住", 花了, 超时)
	}
	if !rt.Unusable() {
		t.Error("超时之后 runtime 应当是不可用的")
	}
}

// 出参解码**超预算**（不是死循环）时也要被拉回来。
//
// 与上一条分开：死循环钉的是「中断能不能插进去」，这条钉的是
// 「一次很慢但会结束的解码同样受同一个预算约束」——它才是更常见的那一种。
func Test出参解码超预算被调用超时拉回来(t *testing.T) {
	const (
		超时   = 30 * time.Millisecond
		解码耗时 = 400 * time.Millisecond
	)
	m, err := Compile("p", `
export default {
  onRequest() {
    return { text: "ok", toJSON() {
      const t = Date.now();
      while (Date.now() - t < `+strconv.FormatInt(解码耗时.Milliseconds(), 10)+`) {}
      return { text: "ok" };
    } };
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{CallTimeout: 超时})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	开始 := time.Now()
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	花了 := time.Since(开始)

	if f == nil {
		t.Fatalf("解码跑满 %v（预算 %v）应当返回失败，实际出参 = %v", 解码耗时, 超时, out)
	}
	if !errors.Is(f, ErrCallTimeout) {
		t.Errorf("Kind = %v，想要 ErrCallTimeout", f.Kind)
	}
	// 关键：不能等它自己跑完那 400ms 才算数。
	if 花了 >= 解码耗时 {
		t.Errorf("耗时 %v 跑满了解码自己的 %v，说明它不在限额之内", 花了, 解码耗时)
	}
	if 花了 > 超时*2+time.Second {
		t.Errorf("耗时 %v 远超超时 %v", 花了, 超时)
	}
}

// 解码期间调宿主能力时，宿主函数拿到的 ctx 同样要带 deadline。
//
// 这一条与顶层那条是一对：两段都曾经落在「没有 deadline 的窗口」里。
func Test出参解码期间宿主函数拿到deadline(t *testing.T) {
	const 超时 = 500 * time.Millisecond
	m, err := Compile("p", `
export default {
  onRequest() {
    return { text: "ok", toJSON() {
      globalThis.__larkax.看();
      return { text: "ok" };
    } };
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}

	type 观察 struct {
		有deadline bool
		剩余        time.Duration
	}
	结果 := make(chan 观察, 1)
	host := Host{Capabilities: map[string]any{
		"看": HostFunc(func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			var o 观察
			dl, ok := ctx.Deadline()
			o.有deadline = ok
			if ok {
				o.剩余 = time.Until(dl)
			}
			结果 <- o
			return moejs.Undefined(), nil
		}),
	}}

	rt, err := m.NewRuntime(context.Background(), host, []string{"看"}, Limits{CallTimeout: 超时})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}

	select {
	case o := <-结果:
		if !o.有deadline {
			t.Error("解码期间宿主函数的 ctx 没有 deadline：那时 callCtx 已经被清空了")
		}
		if o.剩余 <= 0 || o.剩余 > 超时 {
			t.Errorf("deadline 剩余 %v，想要 (0, %v]", o.剩余, 超时)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("toJSON 里没有调到宿主函数（这条用例没验到东西）")
	}
}
