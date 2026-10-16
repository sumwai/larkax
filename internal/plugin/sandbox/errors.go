// Package sandbox 把 moejs 封装成「跑一个插件的一次 hook 调用」这件事。
//
// 这一层只认三样东西：编译好的插件、每次请求的一个 runtime、以及一组宿主能力。
// 插件注册、hook 的类型与失败策略在上一层（internal/plugin）。
//
// # 三条实测出来的边界，整个设计围着它们转
//
//  1. **宿主函数阻塞时 Interrupt 无效。** 实测：宿主函数里 time.Sleep(300ms)
//     配 50ms 的 deadline，实际跑满 301ms。所以每个宿主函数自己带 deadline
//     （见 internal/plugin/host），不能指望 Interrupt 兜住。
//
//  2. **内存上限走 Interrupt 路径，不是可捕获的异常。** 脚本的 try/catch
//     拦不住它（实测确认）。但它管不住宿主用 ParseJSONString 塞进去的请求体
//     （实测：16 MiB 的 JSON 在 4 MiB 上限下毫无反应，分配计数只加了 768 字节）。
//     所以入参另有 MaxInputBytes 这一道，出参用 UnmarshalWith 的 MaxBytes。
//
//  3. **runtime 必须单 goroutine 独占。** Module 不可变、可并发 Load；
//     Runtime 不是。所以 Runtime 不做池化、不加锁：跨 goroutine 共享它是错的，
//     加锁只会把错误藏起来。
//
// # 为什么每请求新建 runtime
//
// 新建 runtime 约 1.4 µs，池化省不下什么。而池化要付代价：实测插件把 ctx
// 存进模块变量后，还池再租出去的下一个请求能读到上一个请求的数据。
// 每请求新建把这类泄漏限制在**同一个请求之内**。
package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/Calcium-Ion/moejs"
)

// 插件失败的类型。
//
// 分成这几种而不是一个笼统的 error：它们的处置不同（断开不必告警、
// 超时要看看插件写了什么、内存超限说明插件有问题、内部错误必须换掉 runtime），
// 而全都表现为「这次调用出错了」。
var (
	// ErrPluginThrew 表示插件自己抛了异常。
	ErrPluginThrew = errors.New("插件抛出异常")
	// ErrAsyncHook 表示 hook 返回了一个 Promise。
	//
	// 与 ErrBadResult 分开：那一个是「返回的形状不对」，这个是「用了不支持
	// 的写法」。前者要插件改返回值，后者要把 async 去掉——处置不同。
	ErrAsyncHook = errors.New("插件用了异步 hook")
	// ErrBadResult 表示插件返回的值解不成约定的结构。
	//
	// 与 ErrPluginThrew 分开：抛出异常是插件内部出了问题，
	// 而返回一个不合契约的对象是**契约违规**——它要么理解错了返回值形状，
	// 要么在试着给一个不该给的字段。上层的处置不同（前者按失败丢掉，
	// 后者要显式报出来）。
	ErrBadResult = errors.New("插件返回的值不符合契约")
	// ErrCallTimeout 表示这次 hook 调用超过了时限。
	ErrCallTimeout = errors.New("插件调用超时")
	// ErrHostTimeout 表示插件调用的宿主能力超时。
	//
	// 与 ErrCallTimeout 分开：那个是插件写得慢（死循环之类），
	// 这个是宿主慢（查日历、查 KV），排查方向完全不同。
	ErrHostTimeout = errors.New("宿主能力超时")
	// ErrMemoryLimit 表示插件用超了内存上限。
	ErrMemoryLimit = errors.New("插件超出内存上限")
	// ErrTooLarge 表示入参或出参超过了字节上限。
	ErrTooLarge = errors.New("插件数据超出字节上限")
	// ErrRunaway 表示 runtime 已经不可信（被中断过、或引擎内部出错），
	// 这次请求里不再调用它。
	ErrRunaway = errors.New("插件运行时已不可用")
	// ErrClientCancelled 表示客户端断开了。它不是插件的问题。
	ErrClientCancelled = errors.New("客户端已断开")
)

// Failure 是一次插件调用的失败，带上「是哪一种」。
//
// 带上 Hook 与 Plugin 是因为日志里只有一句「插件失败」时，
// 排查要从头翻一遍才能知道是谁、在哪一步。
type Failure struct {
	Plugin string
	Hook   string
	Kind   error
	// Cause 是底层错误，可能是 *moejs.Exception 等。
	Cause error
	// Stack 是插件侧的调用栈，有则带上——插件作者最需要的就是它。
	Stack string
}

func (f *Failure) Error() string {
	if f.Cause == nil {
		return fmt.Sprintf("插件 %s 的 %s 失败: %v", f.Plugin, f.Hook, f.Kind)
	}
	return fmt.Sprintf("插件 %s 的 %s 失败: %v: %v", f.Plugin, f.Hook, f.Kind, f.Cause)
}

func (f *Failure) Unwrap() error { return f.Cause }

// Is 让 errors.Is(err, ErrCallTimeout) 这类判断穿过 Failure。
//
// 用 errors.Is 而不是指针相等：Kind 可以是一组错误的 Join（例如
// 「越界」同时是「返回不符合契约」与具体哪个字段越界），
// 而指针相等认不出 Join 里的成员。
func (f *Failure) Is(target error) bool { return errors.Is(f.Kind, target) }

// classify 把一个 moejs 错误映射成上面那几种。
//
// 顺序重要：中断要先按 cause 分（客户端断开 / 我们的 deadline / 内存上限），
// 因为这三者都是 *InterruptedError，而处置完全不同。
func classify(ctx context.Context, plugin, hook string, err error) *Failure {
	if err == nil {
		return nil
	}
	f := &Failure{Plugin: plugin, Hook: hook, Cause: err}

	var exc *moejs.Exception
	var interrupted *moejs.InterruptedError
	switch {
	case errors.As(err, &interrupted):
		switch {
		case errors.Is(err, moejs.ErrMemoryLimit):
			f.Kind = ErrMemoryLimit
		case errors.Is(err, context.Canceled), ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
			// 客户端断开。先判它：请求 ctx 被取消时我们的 AfterFunc 会用同一个
			// cause 去 Interrupt，两条路径都会走到这里。
			f.Kind = ErrClientCancelled
		case errors.Is(err, ErrHostTimeout):
			f.Kind = ErrHostTimeout
		default:
			f.Kind = ErrCallTimeout
		}
	case errors.Is(err, ErrAsyncHook):
		f.Kind = ErrAsyncHook
	case errors.As(err, &exc):
		f.Kind = ErrPluginThrew
		f.Stack = exc.Stack
		// 宿主函数返回的错误会被包成 Error 再抛出，errors.Is 能穿过它。
		// 于是「插件没 catch 宿主超时」也能认出来，而不只是「插件抛了异常」。
		if errors.Is(err, ErrHostTimeout) {
			f.Kind = ErrHostTimeout
		}
	default:
		f.Kind = ErrPluginThrew
	}
	return f
}

// isRunaway 判断这个错误是否让 runtime 不再可信。
//
// 中断与内部错误之后都要丢掉 runtime：中断会丢弃排队中的 job
// （半执行的 Promise 反应、微任务），而内部错误是宿主函数 panic
// 或引擎自身的 bug，调用状态虽然恢复了，但没人能保证插件的状态是完整的。
func isRunaway(err error) bool {
	var interrupted *moejs.InterruptedError
	var internal *moejs.InternalError
	return errors.As(err, &interrupted) || errors.As(err, &internal)
}
