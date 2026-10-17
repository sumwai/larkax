package plugin

import (
	"context"
	"errors"
	"sort"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// Session 是一次请求与一个插件的会话。
//
// 它持有这次请求的 runtime，所以**必须由同一个 goroutine 使用**：
// moejs 的 Runtime 必须单 goroutine 独占（只有 Interrupt 例外）。
//
// 每个 hook 的调用结果由**返回值**给出（Result[T]），不留在 Session 上：
// 留一份副本意味着「调用方拿到的」与「Session 记着的」可能不一致，
// 而那种不一致只在排查时会暴露出来，那时最需要的就是可信的记录。
// Session 只留下两样跨 hook 的东西：失败清单与让位的插件。
type Session struct {
	Plugin *Plugin
	rt     *sandbox.Runtime

	// Shadowed 是这次匹配上、但因为「一个请求一个插件」而没有参与的插件。
	//
	// 记录它是为了回答「我明明配了三个插件，为什么只跑了一个」——
	// 没有这个字段，那个问题只能靠读代码回答。
	Shadowed []*Plugin

	// failures 按发生顺序记录所有失败，含被吞掉的非阻断失败。
	failures []*sandbox.Failure
}

// Failures 返回这次请求里插件出过的全部错，含非阻断的。
//
// **被吞掉的失败也要被看见**：一个每次都超时但「不影响请求」的插件，
// 只从请求结果上是完全看不出来的。
func (s *Session) Failures() []*sandbox.Failure {
	if s == nil {
		return nil
	}
	return append([]*sandbox.Failure(nil), s.failures...)
}

// Close 释放这次请求的 runtime。
func (s *Session) Close() {
	if s == nil || s.rt == nil {
		return
	}
	s.rt.Close()
}

// blocking 判断这个插件失败时该不该让请求失败。
func (s *Session) blocking() bool { return s.Plugin.Meta.Blocking }

// Next 是某个 hook 是否还要跑。
//
// 一次失败之后就不再调这个 runtime：中断会丢弃排队中的 job
// （半执行的 Promise 反应、微任务），而超时意味着它的调用栈可能还在跑。
// 继续调它得到的是不可解释的结果。
func (s *Session) Next() bool { return !s.rt.Unusable() }

// record 统一处置一次调用的结果。
func (s *Session) record(f *sandbox.Failure) bool {
	if f == nil {
		return false
	}
	s.failures = append(s.failures, f)
	return s.blocking()
}

// OnRequest 调 onRequest。
//
// 返回的补丁**尚未应用**：应用由调用方做（见 ApplyRequestBodyPatch），
// 于是「插件给了什么」与「最终发出去的是什么」是两个可分别检查的东西。
func (s *Session) OnRequest(ctx context.Context, rc RequestContext) Result[RequestPatch] {
	if s == nil || !s.Next() || !s.Plugin.Module.Has(HookRequest) {
		return Result[RequestPatch]{}
	}
	var patch RequestPatch
	f := s.rt.Call(ctx, HookRequest, rc, &patch)
	if f != nil {
		// 失败时整份补丁都丢掉，不给「部分生效」这种没法解释的状态。
		return patchOf[RequestPatch](nil, f, s.record(f))
	}
	// 补丁里的字段全部为 nil 时归一成「没给补丁」：调用方不必判两次。
	if patch.Body.empty() && len(patch.Headers) == 0 {
		return Result[RequestPatch]{}
	}
	return patchOf(&patch, nil, false)
}

// OnRoute 调 onRoute。
//
// 插件选中的渠道**必须在候选集之内**，否则按插件失败处置。这一条在这里强制，
// 而不是在调用方：允许插件选候选集之外的渠道等于让插件决定流量去哪。
//
// 注意这里的候选集是**内核已经筛过一轮的**（Eligible / DrainReason 都过了），
// 所以插件拿不到一个已经耗尽额度的渠道去选。
func (s *Session) OnRoute(ctx context.Context, rc RouteContext) Result[RouteDecision] {
	if s == nil || !s.Next() || !s.Plugin.Module.Has(HookRoute) {
		return Result[RouteDecision]{}
	}
	// 传给插件之前按 channelId 排一次序：让输入与「内核给的顺序」脱钩，
	// 于是插件写 `candidates[0]` 不会意外表达「内核评分最高的那个」——
	// 那是一种依赖内核实现细节的隐式偏好。
	//
	// **排的是副本，不是调用方那份。** sort.Slice 排的是底层数组本身，
	// 直接排 rc.Candidates 会把调用方手上的顺序一起改掉，而路由层之后
	// 还要拿这份切片重试、记账、把「当时有哪些候选」记进快照——
	// 那时它已经不是内核筛出来的那个顺序了。
	// 同 patch.go 的合并函数：返回新的，不改动入参。
	rc.Candidates = sortedByChannelID(rc.Candidates)

	var decision RouteDecision
	f := s.rt.Call(ctx, HookRoute, rc, &decision)
	if f != nil {
		return patchOf[RouteDecision](nil, f, s.record(f))
	}

	if decision.ChannelID == 0 {
		// JS 里 `return {}` 解出来就是 0，而 0 不是一个合法渠道 ID，
		// 所以它归一成「没给决策」。这一步必须在越界校验**之前**：
		// 否则「没给决策」会被当成一次越界。
		return Result[RouteDecision]{}
	}
	if err := pickChannel(&decision, rc.Candidates); err != nil {
		// 越界不是「插件跑挂了」，而是**契约违规**：它要么理解错了候选集的含义，
		// 要么在猜。两种都该在观测里显式出现，所以这里合成一个 Failure 记下来。
		//
		// Kind 用 errors.Join：errors.Is 先问 Failure.Is（它比的是 Kind），
		// 于是这一条能同时认 ErrChannelOutsideCandidates 与它包住的底层错误。
		failure := &sandbox.Failure{Plugin: s.Plugin.Key, Hook: HookRoute, Kind: errors.Join(ErrChannelOutsideCandidates, err)}
		return patchOf[RouteDecision](nil, failure, s.record(failure))
	}
	return patchOf(&decision, nil, false)
}

// sortedByChannelID 返回按 channelId 升序排好的副本，不改动入参。
func sortedByChannelID(cands []RouteCandidate) []RouteCandidate {
	if len(cands) == 0 {
		// 没有元素就没有「排了谁」的问题，也就不必复制。
		// 直接返回入参同时保住了 nil 与空切片在序列化上的区别
		// （null 与 []）——插件看到的是哪一种，修这一处时不该顺手改掉。
		return cands
	}
	out := make([]RouteCandidate, len(cands))
	copy(out, cands)
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}

// StreamInput 是 onStream 的输入。
type StreamInput struct {
	// Chunk 是当前这一块的原始数据。
	Chunk any
	Index int
	// AccumulatedText 是至此累积的正文。
	//
	// 它是**流式中断计费的依据**：客户端取消时内核用对应模型的分词器对它计数。
	// 所以插件替换正文时改的正是这个值，而不是只改「客户端看到的文本」。
	AccumulatedText string
	// AccumulatedTokensApprox 是内核按分词器增量估算的 token 数。
	AccumulatedTokensApprox int64
}

// OnStream 调 onStream。
//
// **每个块都调一次。** 采样（每 N 块调一次）会让「drop 第 3 块」这类
// 逐块决定失效，而那是这个 hook 的主要用途。逐块的代价是每块一次
// 跨语言调用，所以这里不对块大小做任何猜测——由内核决定怎么切块。
func (s *Session) OnStream(ctx context.Context, rc StreamContext, in StreamInput) Result[StreamDecision] {
	if s == nil || !s.Next() || !s.Plugin.Module.Has(HookStream) {
		return Result[StreamDecision]{}
	}
	ctxPayload := rc
	ctxPayload.Chunk = in.Chunk
	ctxPayload.Index = in.Index
	ctxPayload.Text = in.AccumulatedText
	ctxPayload.Tokens = in.AccumulatedTokensApprox

	var decision StreamDecision
	f := s.rt.Call(ctx, HookStream, ctxPayload, &decision)
	if f != nil {
		return patchOf[StreamDecision](nil, f, s.record(f))
	}
	return patchOf(&decision, nil, false)
}

// OnResponse 调 onResponse。
//
// 传给插件的 usage 是**上游给的真实用量**，只读。插件要改用量只能走 onUsage，
// 而那条通道只收数量不收金额。
func (s *Session) OnResponse(ctx context.Context, rc ResponseContext) Result[ResponsePatch] {
	if s == nil || !s.Next() || !s.Plugin.Module.Has(HookResponse) {
		return Result[ResponsePatch]{}
	}
	var patch ResponsePatch
	f := s.rt.Call(ctx, HookResponse, rc, &patch)
	if f != nil {
		return patchOf[ResponsePatch](nil, f, s.record(f))
	}
	if patch.Text == nil && len(patch.Metadata) == 0 {
		return Result[ResponsePatch]{}
	}
	return patchOf(&patch, nil, false)
}

// OnUsage 调 onUsage。
//
// 返回的用量**只报数量**：UsageFacts 里没有金额字段，所以「插件改金额」
// 在这条路径上不是「被拦住」而是「表达不出来」。
//
// 拿不到上游 usage 时内核会自己估算，而插件上报的字段会**覆盖**对应的估算项。
// 这是有意的：插件比内核更懂它的上游，它说「这次是 123 个 completion token」
// 比内核按字符数估的准。但只覆盖它报了的项——没报的仍用内核的值，
// 而不是归零（归零会让一次漏报变成一次少收钱）。
func (s *Session) OnUsage(ctx context.Context, rc UsageContext) Result[UsageFacts] {
	if s == nil || !s.Next() || !s.Plugin.Module.Has(HookUsage) {
		return Result[UsageFacts]{}
	}
	var facts UsageFacts
	f := s.rt.Call(ctx, HookUsage, rc, &facts)
	if f != nil {
		return patchOf[UsageFacts](nil, f, s.record(f))
	}
	if facts.empty() {
		return Result[UsageFacts]{}
	}
	return patchOf(&facts, nil, false)
}

// empty 判断插件有没有真的上报任何一项。
//
// 只声明口径（CompletionIncludesReasoning）也算上报：它不改数量但改金额，
// 当成「没上报」丢掉的话，一个代理 OpenAI 系上游的插件把口径说对了也没用，
// 那批推理 token 照样被收两次。
func (f UsageFacts) empty() bool {
	switch {
	case f.PromptTokens != nil, f.CompletionTokens != nil, f.CacheReadTokens != nil,
		f.CacheWriteTokens != nil, f.ReasoningTokens != nil,
		f.CompletionIncludesReasoning != nil:
		return false
	}
	return len(f.Units) == 0
}

// MergeUsage 把插件上报的用量叠加到内核的事实上。
//
// 「叠加」而不是「替换」：没报的项保留内核的值。替换会让一次漏报
// 变成一次少收钱——而漏报是很可能发生的（插件只在特定条件下才报表某个字段）。
//
// 口径（CompletionIncludesReasoning）同样按这个规则叠加：插件声明了就以它
// 为准，没声明就保留内核的。它必须一起并进来——合并出来的用量随后要被
// 重建回协议层用量去结算，而这一步丢了口径就等于把同一批 token 收两次。
//
// **目前还没有生产调用方**：那条「合并 → 重建回 channel.Usage → 结算」的
// 链路还没接上，现在只有本包的测试在调它（同 ChannelUsage，见
// LESSONS.md 第 22 条）。
func MergeUsage(core UsageFacts, reported *UsageFacts) UsageFacts {
	if reported == nil {
		return core
	}
	out := core
	over := func(dst **int64, src *int64) {
		if src != nil {
			v := *src
			*dst = &v
		}
	}
	over(&out.PromptTokens, reported.PromptTokens)
	over(&out.CompletionTokens, reported.CompletionTokens)
	over(&out.CacheReadTokens, reported.CacheReadTokens)
	over(&out.CacheWriteTokens, reported.CacheWriteTokens)
	over(&out.ReasoningTokens, reported.ReasoningTokens)
	if reported.CompletionIncludesReasoning != nil {
		v := *reported.CompletionIncludesReasoning
		out.CompletionIncludesReasoning = &v
	}
	if len(reported.Units) > 0 {
		units := make(map[string]int64, len(out.Units)+len(reported.Units))
		for k, v := range out.Units {
			units[k] = v
		}
		for k, v := range reported.Units {
			// 非 token 计量按**相加**：一次请求可能调用两次搜索，
			// 插件报的是它观察到的那部分。
			units[k] += v
		}
		out.Units = units
	}
	return out
}
