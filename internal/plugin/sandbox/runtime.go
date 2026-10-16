package sandbox

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// Limits 是一次调用的限额。
//
// 三项都有默认值（见 DefaultLimits），因为「没设限额」与「限额很宽」
// 在运行时的表现完全一样，而前者是无限循环吃满 CPU 的那一种。
type Limits struct {
	// CallTimeout 是单次 hook 调用的上限。
	CallTimeout time.Duration
	// MemoryBytes 是这个 runtime 的 JS 分配上限。<= 0 表示不设。
	MemoryBytes int64
	// MaxInputBytes 是入参 JSON 的字节上限。
	//
	// **它必须单独存在**：内存上限管不住宿主塞进去的值——
	// 实测 16 MiB 的 ParseJSONString 在 4 MiB 上限下，分配计数只加了 768 字节。
	MaxInputBytes int
	// MaxOutputBytes 是出参 JSON 的字节上限。插件可以把结果放大很多倍，
	// 而上游请求体与响应体都有实际的体积限制。
	MaxOutputBytes int
}

// DefaultLimits 是默认限额。
//
// 5 秒是契约默认值：一次请求的预算大部分要给上游，插件夹在中间，
// 它超时就不该继续等。内存 64 MiB 相对于「一份请求体 + 一点中间数据」
// 是宽的，但足以挡住「一个循环吃光进程内存」。
var DefaultLimits = Limits{
	CallTimeout:    5 * time.Second,
	MemoryBytes:    64 << 20,
	MaxInputBytes:  4 << 20,
	MaxOutputBytes: 4 << 20,
}

// normalize 补上零值。负数与零都当作「没设」，用默认值。
func (l Limits) normalize() Limits {
	d := DefaultLimits
	if l.CallTimeout <= 0 {
		l.CallTimeout = d.CallTimeout
	}
	if l.MemoryBytes <= 0 {
		l.MemoryBytes = d.MemoryBytes
	}
	if l.MaxInputBytes <= 0 {
		l.MaxInputBytes = d.MaxInputBytes
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	return l
}

// HostFunc 是一个宿主函数的实现。
//
// 第一个参数是请求的 context——**每个宿主函数都必须尊重它**。
// 宿主阻塞时 Interrupt 无效（实测：宿主 sleep 300ms 配 50ms deadline
// 实际跑满 301ms），所以超时只能由宿主函数自己提前返回来实现。
type HostFunc func(ctx context.Context, args []moejs.Value) (moejs.Value, error)

// Host 是一组宿主能力。
//
// 这是**权限的落点**：不给 fetch，插件就没有网络；渠道密钥不放进来，
// 它就看不到密钥。契约里「context 里根本没有这个字段」靠的是这里不注入，
// 而不是插件层的检查——检查会漏，不注入不会。
type Host struct {
	// Capabilities 是注入到 globalThis.__larkax 上的命名空间。
	// 每个值是 map[string]any（内含 HostFunc）或单个 HostFunc。
	Capabilities map[string]any

	// OnError 让宿主记录能力自身的失败（例如 KV 查询报错）。
	// 为 nil 时错误只通过返回值传给插件。
	OnError func(name string, err error)
}

// runtimeState 是一个请求内的 runtime 连同它的注入状态。
//
// 它**不是**并发安全的，也不打算是：moejs 的 Runtime 必须单 goroutine 独占。
// 例外只有 Interrupt 与这里的几个原子标志——超时路径上会从别的 goroutine 读它们。
type runtimeState struct {
	rt     *moejs.Runtime
	plugin string
	limits Limits

	// injected 记录注入了哪些能力。能力是每请求注一遍的（KV 要按用户
	// 命名空间隔离），所以每个请求都要重来一次。
	injected map[string]bool

	// dead 表示这个 runtime 已经不可用。一旦置上就永不复用。
	dead atomic.Bool

	// injectCtx 是注入宿主能力时的那个 ctx（也就是请求的 ctx）。
	injectCtx context.Context

	// callCtx 是**当前这次调用**的 ctx，带这次调用的 deadline，
	// 并且继承 injectCtx 的取消。宿主函数读它。
	//
	// 「每个宿主函数自带 deadline」如果只依赖调用方在 NewRuntime 时传一个
	// 带 deadline 的 ctx，那它就不是本层的保证而是调用方的义务——
	// 而调用方传 context.Background() 是最常见的写法，那时宿主函数
	// 拿到的 ctx 永不结束，超时只能靠 Interrupt，偏偏宿主阻塞时它无效。
	// 所以这里由本层把两个父 ctx 合起来，宿主函数拿到的是结果。
	//
	// 用 atomic.Pointer 而不是普通字段：写入发生在调用 goroutine 里，
	// 读取也发生在同一个 goroutine 上（宿主函数由 rt.Call 同步调起），
	// 但中断路径可能从别的 goroutine 观察它，所以按可并发读来声明。
	callCtx atomic.Pointer[context.Context]
}

// markUnusable 把 runtime 标成不可用。可以从别的 goroutine 调。
func (r *runtimeState) markUnusable() { r.dead.Store(true) }

// hostCtx 返回宿主函数该用的 ctx：优先用当前这次调用的（带这次调用的
// deadline），没有则在调用之外，用注入时的那个。
func (r *runtimeState) hostCtx(fallback context.Context) context.Context {
	if p := r.callCtx.Load(); p != nil {
		return *p
	}
	return fallback
}

// usable 判断还能不能再用它。
func (r *runtimeState) usable() bool { return !r.dead.Load() }

// newRuntime 按限额建一个 runtime。
func newRuntime(plugin string, limits Limits) *runtimeState {
	limits = limits.normalize()
	return &runtimeState{
		rt: moejs.NewRuntime(moejs.Options{
			// 插件源码在编译期就解析完了，运行时不需要 eval / new Function。
			// 关掉它让「动态执行一段字符串」直接抛 EvalError（实测确认）。
			DisableDynamicCode: true,
			MemoryLimit:        limits.MemoryBytes,
		}),
		plugin:    plugin,
		limits:    limits,
		injected:  make(map[string]bool),
		injectCtx: context.Background(),
	}
}

// inject 注入宿主能力。
//
// 白名单而不是黑名单：不在 allowed 里的名字不注入。这样后面加了新能力
// 而忘了改策略时，插件拿到的是「未启用」，而不是悄悄拿到了它。
func (r *runtimeState) inject(ctx context.Context, host Host, allowed map[string]bool) error {
	if ctx != nil {
		r.injectCtx = ctx
	}
	if len(host.Capabilities) == 0 {
		return nil
	}
	ns := make(map[string]any, len(host.Capabilities))
	for name, capability := range host.Capabilities {
		if !allowed[name] {
			continue
		}
		ns[name] = wrapCapability(r, ctx, name, capability, host.OnError)
	}
	if len(ns) == 0 {
		return nil
	}
	if err := r.rt.SetGlobal("__larkax", ns); err != nil {
		return fmt.Errorf("注入宿主能力失败: %w", err)
	}
	for name := range ns {
		r.injected[name] = true
	}
	return nil
}

// wrapCapability 把一个能力对象（map 或单个函数）里的每个函数包一层。
func wrapCapability(r *runtimeState, ctx context.Context, name string, capability any, onError func(string, error)) any {
	switch v := capability.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for fn, impl := range v {
			out[fn] = wrapFunc(r, ctx, name+"."+fn, impl, onError)
		}
		return out
	default:
		return wrapFunc(r, ctx, name, capability, onError)
	}
}

func wrapFunc(r *runtimeState, ctx context.Context, name string, impl any, onError func(string, error)) moejs.NativeFunc {
	fn, ok := impl.(HostFunc)
	if !ok {
		// 能力表写错了。返回一个总是报错的函数，而不是 panic——
		// panic 会变成 *InternalError，把整个 runtime 一起拖下水。
		return moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			return moejs.Undefined(), fmt.Errorf("宿主能力 %s 不是一个函数", name)
		})
	}
	return moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		// 这是「宿主函数自带 deadline」的落点。宿主阻塞时 Interrupt 无效，
		// 所以每一步都先在这里判一次 ctx：已经超时就立刻返回。
		//
		// 用 hostCtx 而不是闭包里的注入 ctx：前者带着**这次调用**的 deadline，
		// 于是宿主函数拿到的是一个真的会结束的 ctx，它的 I/O 也能跟着结束。
		callCtx := r.hostCtx(ctx)
		if cause := context.Cause(callCtx); cause != nil {
			if onError != nil {
				// 报给宿主：这次能力调用被拒绝了，而原因不是能力本身出错。
				// 不报的话，一次超时里的所有能力调用都静默消失，
				// 排查时看不出「插件到底调没调它」。
				onError(name, cause)
			}
			return moejs.Undefined(), cause
		}
		val, err := fn(callCtx, args)
		if err != nil {
			if onError != nil {
				onError(name, err)
			}
			return moejs.Undefined(), err
		}
		return val, nil
	})
}

// call 在 runtime 上调用一次 hook。
//
// hookName 是给错误与日志用的**干净名字**（"onRequest"），不是 moejs 的
// Hook.Name()——后者是解析路径（"default.onRequest"），把它写进日志会让人
// 以为插件里有一个叫 default 的东西。
//
// 返回的 value 在 Failure 非空时无意义。
func (r *runtimeState) call(ctx context.Context, hook moejs.Hook, hookName string, args []moejs.Value) (moejs.Value, *Failure) {
	timeout := r.limits.CallTimeout

	type outcome struct {
		val moejs.Value
		err error
	}
	// 缓冲为 1：超时路径返回之后那个 goroutine 仍可能写入，不缓冲就会
	// 永久阻塞它——那是一个 goroutine 泄漏，而且是最难发现的那种。
	ch := make(chan outcome, 1)

	// ctx 是从参数与 r.injectCtx 两处继承来的，都是调用链上传下来的
	// 请求 ctx，只是 contextcheck 看不到结构体字段的来源。
	//nolint:contextcheck // 见上
	go func() {
		// 这个 goroutine 独占 runtime。它自己管 ctx 的取消与 Interrupt。
		//
		// 两个父 ctx 都要跟：这次调用的 ctx（带调用方的取消）与注入时那个
		// 请求 ctx。正常路径下它们本来就是同一个，但把它们显式合起来，
		// 「宿主函数拿到的 ctx 一定会在该结束时结束」就成了本层的保证，
		// 而不是「调用方记得两处传同一个 ctx」这个隐式约定。
		merged, cancelMerged := context.WithCancelCause(ctx)
		defer cancelMerged(nil)
		if parent := r.injectCtx; parent != nil && parent != ctx {
			// 先同步查一次：AfterFunc 对**已经取消**的 ctx 会在自己的
			// goroutine 里补跑，于是「父 ctx 已取消」与「宿主函数被调用」
			// 之间有一场竞态——而两者谁先到，决定了宿主函数会不会被进入。
			if cause := context.Cause(parent); cause != nil {
				cancelMerged(cause)
			} else {
				stopParent := context.AfterFunc(parent, func() { cancelMerged(context.Cause(parent)) })
				defer stopParent()
			}
		}
		cctx, cancel := context.WithTimeoutCause(merged, timeout, ErrCallTimeout)
		defer cancel()

		// 把这次调用的 ctx 挂出去，宿主函数从这里取。
		// 放在 Call 之前、取出在 Call 之后清掉：清晚了会让调用之外的
		// 能力使用（例如施工期的探针）拿到一个已经取消的 ctx。
		captured := cctx
		r.callCtx.Store(&captured)
		defer r.callCtx.Store(nil)

		stop := context.AfterFunc(cctx, func() {
			// Interrupt 是唯一允许跨 goroutine 调的方法。插件在 JS 里
			// 死循环时，这是唯一能把它拉回来的东西。
			r.rt.Interrupt(context.Cause(cctx))
		})
		defer stop()

		val, err := r.rt.Call(hook, args...)
		ch <- outcome{val: val, err: err}
	}()

	select {
	case out := <-ch:
		if out.err != nil && isRunaway(out.err) {
			// 中断与内部错误之后这个 runtime 不再可信：中断会丢弃排队中的
			// job（半执行的 Promise 反应、微任务）。不再复用它。
			r.markUnusable()
		}
		return out.val, classify(ctx, r.plugin, hookName, out.err)

	case <-time.After(timeout):
		// Call 没能在时限内返回。两种可能：插件在 JS 里死循环而 Interrupt
		// 没能及时生效，或者某个宿主函数卡住了。两种情况下这个 runtime
		// 都不能再用——它的调用栈还在跑。
		r.markUnusable()
		// 补一次 Interrupt，让那个还在跑的 Call 尽早退出，
		// 别把一个 goroutine 永久挂在那里。
		r.rt.Interrupt(ErrCallTimeout)
		return moejs.Undefined(), &Failure{Plugin: r.plugin, Hook: hookName, Kind: ErrCallTimeout}

	case <-ctx.Done():
		r.markUnusable()
		r.rt.Interrupt(context.Cause(ctx))
		return moejs.Undefined(), &Failure{Plugin: r.plugin, Hook: hookName, Kind: ErrClientCancelled, Cause: context.Cause(ctx)}
	}
}
