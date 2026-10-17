package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// Module 是编译、链接并求值过一次的插件。
//
// 它是**不可变**的：moejs 的 Module 可被任意多 runtime 并发 Load，
// 而每个请求新建一个 runtime。所以一份 Module 服务所有并发请求，
// 每次请求付的是「新建 runtime + 求值模块顶层」的代价。
type Module struct {
	// Name 是插件标识（编译时给的模块名），只用于错误与日志。
	Name string
	mod  *moejs.Module
	// hooks 是这个插件导出的 hook 名，按字母序。
	hooks []string
	// meta 是 `export const meta` 的值，编译期读出来的一份拷贝。
	meta map[string]any
}

// HookNames 是插件契约里的五个 hook，顺序即生命周期顺序。
//
// 用切片而不是集合：分发时要按这个顺序走，而 map 的遍历顺序是随机的。
var HookNames = []string{"onRequest", "onRoute", "onStream", "onResponse", "onUsage"}

// Compile 编译、链接并**求值一次**插件源码。
//
// 源码按 JavaScript 读。**不收 TypeScript**：插件契约与 `larkaxctl plugin
// publish` 都只说 JS（读文件之后直接进这里），没有一处声明 TypeScript 是
// 支持的语言。「收不收」要先在契约里定下来，再同时改这条路径与文档；
// 在那之前留一个没人调的 TypeScript 入口，只会让作者以为它已经生效。
//
// 链接这一步就把插件能导入的东西**封死**了：只有虚拟 SDK "larkax" 能解析成功，
// 别的说明符一律失败。于是 `import fs from "node:fs"` 在编译期就过不去，
// 而不是等到运行时才发现它拿到的是一个 undefined。
//
// 求值这一次是为了读出两样只有执行过才知道的东西：`export const meta` 的值，
// 以及**插件到底导出了哪几个 hook**。后者读不出来只能靠猜，而猜错的两种方向
// 都有代价：以为有 onRoute 会让每次请求白跑一次跨语言调用，
// 以为没有会让插件的一个 hook 永远不被调用。两样都缓存在 Module 上，
// 因为 Module 不可变——求值一次与求值一千次得到同样的答案。
func Compile(name, source string) (*Module, error) {
	mod, err := moejs.Compile(name, source)
	if err != nil {
		return nil, fmt.Errorf("编译插件 %s 失败: %w", name, err)
	}
	return link(name, mod)
}

func link(name string, mod *moejs.Module) (*Module, error) {
	sdkMod, err := sdk()
	if err != nil {
		return nil, fmt.Errorf("虚拟 SDK 不可用: %w", err)
	}

	linked, err := moejs.Link(mod, func(_ moejs.Referrer, specifier string) (*moejs.Module, error) {
		if specifier == sdkModuleName {
			return sdkMod, nil
		}
		// **只放行虚拟 SDK。** 插件不能从别处导入任何东西：
		// 没有文件系统、没有网络、没有宿主内部模块。
		return nil, fmt.Errorf("插件只能从 %q 导入，收到 %q", sdkModuleName, specifier)
	})
	if err != nil {
		return nil, fmt.Errorf("链接插件 %s 失败: %w", name, err)
	}

	m := &Module{Name: name, mod: linked}
	if err := m.inspect(context.Background()); err != nil {
		return nil, err
	}
	return m, nil
}

// ErrNoDefaultExport 表示插件没有默认导出，也就没有 hook 可调。
var ErrNoDefaultExport = errors.New("插件没有默认导出")

// inspectTimeout 是「求值一次模块顶层」的时限。
//
// 必须有，而且不能指望 ctx：`Compile` 的调用方（上传接口、命令行）
// 通常只给一个 context.Background()，那样 AfterFunc 永远不会触发，
// 而顶层写了一个死循环的插件会让上传请求永远挂着——表现是「界面上
// 一直转圈」，没人会想到是插件的内容问题。
const inspectTimeout = 2 * time.Second

// inspect 求值一次模块顶层，读出 meta 与导出的 hook 名。
//
// **这一次求值用的是临时 runtime，而且没有注入任何宿主能力**：
// 编译期拿不到用户身份，也不该往任何人的命名空间里写东西，所以这里
// 即使插件写了 `log("...")` 也只会拿到「能力未启用」。
//
// 注意范围：这句话说的是**编译期这一次**求值。请求期的 NewRuntime 会把
// 能力注入之后再求值顶层（KV 要按用户命名空间隔离，只能每请求注一遍），
// 所以请求期的顶层代码**能**调宿主能力——它受的是 CallTimeout 的约束，
// 而不是「拿不到能力」。别把这两处混为一谈。
func (m *Module) inspect(parent context.Context) error {
	// 没有默认导出时先把它挑出来：逐个 hook 去查只会得到「找不到 onRequest」，
	// 而真正的问题是「你没有 export default」。
	if _, err := m.mod.Hook("default"); err != nil {
		return fmt.Errorf("%w: 插件 %s 必须 export default 一个对象", ErrNoDefaultExport, m.Name)
	}

	ctx, cancel := context.WithTimeoutCause(parent, inspectTimeout,
		fmt.Errorf("%w: 求值插件 %s 的顶层超过 %v", ErrCallTimeout, m.Name, inspectTimeout))
	defer cancel()

	rt := moejs.NewRuntime(moejs.Options{DisableDynamicCode: true, MemoryLimit: inspectMemoryLimit})
	stop := context.AfterFunc(ctx, func() { rt.Interrupt(context.Cause(ctx)) })
	defer stop()

	// Load 与 Has 都可能因为有宿主函数卡住而不返回（这里没有注入宿主函数，
	// 但顶层仍然可以写一个很慢的循环），所以这里也按「不阻塞地等」处理。
	if err := loadWithin(ctx, rt, m.mod, inspectTimeout, nil); err != nil {
		// 顶层失败的**原因分类要看得到**：上传期最常见的两种错法是
		// 「顶层就吃掉几十兆」与「顶层死循环」，而它们的处置不同
		// （前者要作者删代码，后者要作者把重活挪进 hook）。
		// 不分类的话，上层只能拿到一句「求值失败」。
		f := classify(parent, m.Name, compileHook, err)
		return fmt.Errorf("%w: 求值插件 %s 的顶层失败: %w", f.Kind, m.Name, err)
	}

	// 下面这一段（读 meta、问 hook）**同样在跑插件代码**：meta 可以是一个
	// 带 getter 的对象，hook 可以是一个取值器，两者都能写死循环。
	// 它跑在调用方 goroutine 上，不在 loadWithin 的 select 里，界来自
	// **上面那个 AfterFunc 还武装着**——inspectTimeout 到点时它 Interrupt，
	// 而纯 JS 的循环会被打断（实测：meta 里写 `toJSON(){while(true){}}`，
	// Compile 在 2.004s 返回）。宿主函数在这里面不存在（这一路没注入能力），
	// 所以「宿主阻塞时 Interrupt 无效」那条边界够不到它。
	//
	// 界既然来自 AfterFunc，失败文案就得按它来分（见 afterLoad）。
	if v, ok := rt.Export("meta"); ok {
		var meta map[string]any
		if err := rt.Unmarshal(v, &meta); err != nil {
			return m.afterLoad(ctx, fmt.Errorf("插件 %s 的 meta 不是一个对象: %w", m.Name, err))
		}
		// meta 是 null 时 rt.Export 给的是 null 值，而 null 解进一个 map
		// 会被当成「一个空对象」（json.Unmarshal 的语义）。那样「没有 meta」
		// 与「meta 是空对象」就分不开了，而前者是缺字段、后者是有效声明。
		if v.IsNull() || v.IsUndefined() {
			m.meta = nil
		} else if c, ok := deepCopyJSON(meta).(map[string]any); ok {
			m.meta = c
		}
	}

	// hook 名只能靠 Has 问出来：Hook() 只解析导出名与成员键，**不检查成员存不存在**，
	// 所以它对任何一个成员名都会成功。Has 才是「这个名字现在是不是一个函数」。
	for _, name := range HookNames {
		h, err := m.mod.Hook("default", name)
		if err != nil {
			// 走到这里说明连 default 都没有，上面已经拦过了。
			continue
		}
		ok, err := rt.Has(h)
		if err != nil {
			return m.afterLoad(ctx, fmt.Errorf("插件 %s 的 %s 无法解析: %w", m.Name, name, err))
		}
		if ok {
			m.hooks = append(m.hooks, name)
		}
	}
	if len(m.hooks) == 0 {
		// 一个 hook 都没有的插件是写错了，而不是「什么都不做」——
		// 它会被注册进来、每次请求跑一遍、然后什么都不做，没人看得出来。
		return fmt.Errorf("插件 %s 没有导出任何 hook", m.Name)
	}
	sort.Strings(m.hooks)
	return nil
}

// afterLoad 给「Load 之后那一段」的失败分类。那一段的界从哪来，见 inspect。
//
// 被 AfterFunc 拉回来时错误文本是「interrupted: 求值插件 X 的顶层超过 2s」，
// 而外层的包装会把它读成「meta 不是一个对象」或「hook 无法解析」——
// 真正的原因是超时，两者在上传期的处置不同：前者要作者改 meta，
// 后者要作者把重活挪进 hook。
func (m *Module) afterLoad(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	return fmt.Errorf("%w: 求值插件 %s 的顶层超过 %v", ErrCallTimeout, m.Name, inspectTimeout)
}

// loadWithin 求值模块顶层，但**不无限等它**。
//
// 与调用路径上的 invoke 同理：moejs 的 Runtime 必须单 goroutine 独占，
// 一个卡住的顶层会占住那个 goroutine，唯一能做的就是把 runtime 与
// goroutine 一起抛弃。超时之后这个 runtime 不再被碰。
//
// run 在**跑 Load 的那个 goroutine 里**执行，收到的是这次求值的 ctx。
// 需要把这次求值的 ctx 挂到 runtime 上的调用方用它——挂的位置不是随便定的：
// 超时返回之后 Load 还在那个 goroutine 里跑，而残余的插件代码随后会调宿主
// 能力；挂在调用方 goroutine 上的话，那时 ctx 已经被清掉，宿主函数就落到
// 「没有 deadline 的 fallback」上（见 loadTopLevel 的注释）。
//
// 这个位置**只对请求期那条路径有意义**，编译期的 inspect 不挂 ctx，传 nil。
func loadWithin(ctx context.Context, rt *moejs.Runtime, mod *moejs.Module, timeout time.Duration, run func(context.Context) error) error {
	if run == nil {
		run = func(context.Context) error { return rt.Load(mod) }
	}
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		rt.Interrupt(ErrCallTimeout)
		return fmt.Errorf("%w: 求值超过 %v", ErrCallTimeout, timeout)
	case <-ctx.Done():
		rt.Interrupt(context.Cause(ctx))
		return context.Cause(ctx)
	}
}

// deepCopyJSON 深拷贝一份 JSON 形状的值。
//
// 只在编译期用一次，不值得为它引入反射或第三方库：这里的数据形状是
// map / slice / 标量，手写一层递归就够，而且读起来比反射版直白。
func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, sub := range x {
			out[k] = deepCopyJSON(sub)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, sub := range x {
			out[i] = deepCopyJSON(sub)
		}
		return out
	default:
		return v
	}
}

// compileHook 是编译期那次求值在错误里的 hook 名。
//
// 它不是一个真的 hook：编译期求值的是模块顶层。用一个明显的名字，
// 免得日志里出现「onRequest 失败」而实际失败在顶层。
const compileHook = "compile"

// inspectMemoryLimit 是「读出元数据」这一次求值的分配上限。
//
// 给得比请求时的限额小：这一步只该声明常量，一个在顶层就吃掉几十兆的插件
// 是写错了，而它耗的是**每一次编译**的成本。
const inspectMemoryLimit = 8 << 20

// Hooks 返回插件导出的 hook 名，按字母序。
//
// 目前只有测试在调它：生产里逐个问的是 Has（「这个插件导出了某个 hook 吗」），
// 还没有哪一处要枚举全部 hook 名。留着是因为「这个插件到底导出了哪些 hook」
// 是自省该回答的问题，而这份名单是编译期求值出来的，不需要重算。
func (m *Module) Hooks() []string { return append([]string(nil), m.hooks...) }

// Has 判断插件是否导出了某个 hook。
func (m *Module) Has(hook string) bool {
	for _, h := range m.hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// Meta 返回插件在源码里声明的元数据，没有则返回 nil。
//
// 它是**编译期读出来的快照**，不再执行任何插件代码：`export const meta`
// 是常量，而每次请求都为了读它跑一遍顶层是白费。
func (m *Module) Meta() map[string]any {
	if m.meta == nil {
		return nil
	}
	out := make(map[string]any, len(m.meta))
	for k, v := range m.meta {
		out[k] = v
	}
	return out
}

// Runtime 是一次请求的插件运行时。
//
// 它必须由**同一个 goroutine** 使用：moejs 的 Runtime 必须单 goroutine 独占。
// 不要在多个 goroutine 之间传它——那不会有编译错误，只会有随机的错误结果。
type Runtime struct {
	state *runtimeState
	mod   *Module
}

// NewRuntime 为一次请求准备一个 runtime，并把宿主能力注入进去。
//
// **每请求新建**，不池化。池化省下的那点开销换不来一个真实的泄漏风险：
// 实测插件把 ctx 存进模块变量之后，还池再租出的下一个请求能读到上一个
// 请求的数据。每请求新建把这类泄漏限制在同一个请求之内。
//
// allowed 是这次允许注入的能力名清单，**空表示一个都不给**。
func (m *Module) NewRuntime(ctx context.Context, host Host, allowed []string, limits Limits) (*Runtime, error) {
	rs := newRuntime(m.Name, limits)
	if err := rs.inject(ctx, host, allowedSet(allowed)); err != nil {
		return nil, err
	}
	// 顶层求值**也在限额之内**。它同样是插件代码：写一个死循环就能让这次
	// 请求永远挂在这里，而 CallTimeout 只覆盖 Call。注入在它之前是有意的
	// ——顶层能调宿主能力（见 loadTopLevel 的注释），代价由 deadline 兜住。
	if err := rs.loadTopLevel(ctx, m.mod); err != nil {
		// 顶层没跑完的 runtime 不再可信：它的调用栈可能还在跑。
		rs.markUnusable()
		return nil, fmt.Errorf("求值插件 %s 失败: %w", m.Name, err)
	}
	return &Runtime{state: rs, mod: m}, nil
}

func allowedSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Close 释放这个 runtime 对宿主值的引用。
//
// 不池化也必须调：它同时会重置内存预算，并把这一轮请求引用的宿主数据放开。
// 调完之后这个 Runtime 不可再用。
func (r *Runtime) Close() {
	if r.state.dead.Load() {
		return
	}
	r.state.rt.ReleaseCallData()
	r.state.markUnusable()
}

// Unusable 表示这个 runtime 已经不可信，剩下的 hook 不再调它。
//
// 三种情况下变 true：这次请求里出过一次中断或引擎内部错误、调用超时了、
// 或者已经 Close 过。前两种意味着它的调用栈可能还在跑或状态不完整，
// 第三种是它已经被显式释放——三种都不该再用。
func (r *Runtime) Unusable() bool { return !r.state.usable() }
