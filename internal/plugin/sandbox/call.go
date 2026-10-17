package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Calcium-Ion/moejs"
)

// Call 在插件上调用一个 hook。
//
// 入参出参都走 JSON：插件看到的是一个普通对象，返回的也是一个普通对象。
// 用 JSON 而不是把 Go 结构直接塞进去，是因为**跨语言的边界要窄且可解释**——
// 一份 JSON 就是这次调用里插件能看到、能改的全部内容，多一个字段都能看出来。
//
// in 为 nil 表示这个 hook 不收参数（当前用不到，但留着比事后加一个变体便宜）。
// out 为 nil 表示不关心返回值。
//
// **失败时 out 的内容是未定义的，调用方不得读它。** 解码与调用跑在同一个
// goroutine 里，而超时返回发生在父 goroutine 上：那之后解码仍可能继续跑完
// 并写 out。现在没有调用方在失败时读出参，所以这条缝观察不到；把它写成
// 契约而不是留给调用方自觉，是因为将来加一个「失败时也看看出参」的调用方，
// 它就是一个只有 -race 抓得到的真竞态。
//
// 没有改成「解码到局部变量、成功才拷回」：那要在热路径上加一次反射
// （out 是 any，要按它的类型新建一个目标，还要处理不是指针的入参），
// 换来的是一条已经写在契约里的、当前观察不到的窗口。
func (r *Runtime) Call(ctx context.Context, hook string, in, out any) *Failure {
	if r.state.dead.Load() {
		// 这次请求里它已经出过一次中断或超时。再用它是错的：
		// 它的调用栈可能还在跑，而且中断会丢弃排队中的 job。
		return &Failure{Plugin: r.mod.Name, Hook: hook, Kind: ErrRunaway}
	}
	if !r.mod.Has(hook) {
		// 插件没导出这个 hook 是正常的，不是失败。
		return nil
	}
	handle, err := r.mod.mod.Hook("default", hook)
	if err != nil {
		return &Failure{Plugin: r.mod.Name, Hook: hook, Kind: ErrPluginThrew, Cause: err}
	}

	var args []moejs.Value
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return &Failure{Plugin: r.mod.Name, Hook: hook, Kind: ErrTooLarge, Cause: fmt.Errorf("入参无法序列化: %w", err)}
		}
		limit := r.state.limits.MaxInputBytes
		if limit > 0 && len(payload) > limit {
			// 字节上限先于解析判：内存上限管不住宿主塞进去的值，这一道才管得住。
			return &Failure{Plugin: r.mod.Name, Hook: hook, Kind: ErrTooLarge,
				Cause: fmt.Errorf("入参 %d 字节，上限 %d", len(payload), limit)}
		}
		// 用 ParseJSON 而不是 ParseJSONString：前者会把这批字节记进
		// runtime 的内存预算（实测确认），于是「宿主塞进去的请求体」
		// 也受内存上限约束。
		arg, err := r.state.rt.ParseJSON(payload)
		if err != nil {
			return classify(ctx, r.mod.Name, hook, err)
		}
		args = append(args, arg)
	}

	// 解码与调用在**同一个** deadline 之内：UnmarshalWith 会执行插件代码
	// （值带 toJSON / getter / proxy 时按 AppendJSON 的文本解码，见 moejs 的
	// unmarshal.go），放在 call() 返回之后做的话，那段代码既没有超时、
	// callCtx 也已经被清空了——它既跑不完，调宿主能力时也拿不到 deadline。
	_, failure := r.state.call(ctx, handle, hook, args, func(res moejs.Value) *Failure {
		return r.decode(hook, res, out)
	})
	return failure
}

// decode 把 hook 的返回值解进出参。
//
// 它由 runtimeState.call 在**受 deadline 约束的那个 goroutine 里**调用，
// 因为它可能执行插件代码：toJSON、getter、proxy 都会让 moejs 走
// AppendJSON 那条路，而那条路是跑 JavaScript。
func (r *Runtime) decode(hook string, res moejs.Value, out any) *Failure {
	if out == nil {
		return nil
	}
	if res.IsUndefined() || res.IsNull() {
		// 什么都没返回是合法的：大多数 hook 只在自己的条件下才给出补丁。
		return nil
	}
	// 先判 IsObject 再取 AsObject：AsObject 是裸的类型转换，
	// 对一个数字调用它拿到的是无效指针。
	if res.IsObject() {
		if _, _, isPromise := res.AsObject().PromiseResult(); isPromise {
			// **Promise 要显式报出来。** 插件把 hook 写成 `async` 时，
			// moejs 的 Call 返回的是一个 Promise 对象，而它会走正常的解组路径
			// 解成空对象——插件作者看到的是「我 return 了东西，宿主说没给」。
			// 那种静默是排查里最费时间的一类：插件自己测不出问题，
			// 宿主日志里也没有任何异常。
			//
			// 契约里顶层 await 与异步 hook 都是不支持的，所以这里直接拒绝，
			// 而不是等那个 Promise 落地（宿主不驱动 job 队列，等也等不到）。
			return &Failure{Plugin: r.mod.Name, Hook: hook, Kind: ErrAsyncHook,
				Cause: errors.New("hook 返回了 Promise：插件契约不支持 async / await")}
		}
	}
	// 出参也限字节，并且**限在解析之前**：插件可以把结果放大很多倍。
	opts := moejs.DecodeOptions{MaxBytes: r.state.limits.MaxOutputBytes}
	if err := r.state.rt.UnmarshalWith(res, out, opts); err != nil {
		kind := ErrBadResult
		if errors.Is(err, moejs.ErrTooLarge) {
			kind = ErrTooLarge
		}
		// 契约违规（例如补丁里出现了白名单之外的字段）由解码目标自己的
		// UnmarshalJSON 报出来，它带的信息比这里的包装有用得多，
		// 所以原样留在 Cause 里。
		return &Failure{Plugin: r.mod.Name, Hook: hook, Kind: kind, Cause: err}
	}
	return nil
}
