package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// Session 是一次请求与一个插件的会话。
//
// 它持有这次请求的 runtime，所以**同一时刻只能有一个 goroutine 用它**：
// moejs 的 Runtime 必须单 goroutine 独占（只有 Interrupt 例外）。
//
// 「同一时刻」而不是「同一个 goroutine」：流式那条路上，onStream 与 onUsage
// 跑在网关的转发 goroutine 里，而 onRequest / onRoute 跑在请求自己的 goroutine
// 里。两者之间隔着事件通道的发送与关闭，有明确的先后关系，不会重叠——但
// 谁要是把 onStream 挪到另一个 goroutine 里与它并发，那条独占就不成立了。
//
// 每个 hook 的调用结果由**返回值**给出（Result[T]），不留在 Session 上：
// 留一份副本意味着「调用方拿到的」与「Session 记着的」可能不一致，
// 而那种不一致只在排查时会暴露出来，那时最需要的就是可信的记录。
// Session 只留下三样跨 hook 的东西：失败清单、让位的插件，以及用量上报的
// 上界（见 usageBound）。
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

	// usageBound 是这次请求给插件上报用量划的那条线，由 ApplyRequest 在
	// **补丁写回之前**记下（见那里的注释与 UsageBound）。
	usageBound UsageBound
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

// ApplyRequest 调 onRequest 并把补丁应用到统一请求上。
//
// 返回被拒的请求头名与错误。**错误只在阻断型插件失败时非 nil**——调用方
// 据此让这次请求失败；非阻断的失败已经记进 Failures()，请求照常继续，
// 只是这次补丁整个不生效。
//
// # 为什么应用补丁这一步在包内
//
// 失败要记进会话（record 与 blocking 都未导出），而契约里明写着「被吞掉的
// 失败也要被看见」。在包外接一条「应用补丁 + 打日志」的路，等于把那条落在
// 地上：一个每次都失败的插件，从请求的结果上完全看不出来。
//
// 被拒的头要报出去，理由见 ApplyHeaderPatch。
//
// **必须在预留之前调用**：补丁能改 max_tokens，而它是计费预留上界的关键
// （见 patch.go 的 errMaxTokensRaised）。排在预留之后，插件调小它就不影响
// 上界，于是上界与上报值由两个来源决定。
func (s *Session) ApplyRequest(ctx context.Context, requestID string, req *channel.Request, canonicalModel, protocol string) ([]string, error) {
	if s == nil || req == nil {
		return nil, nil
	}
	// 上界在这里、**在补丁写回之前**记下来：req.MaxTokens 此刻还是客户端
	// 请求里的那个数。记在补丁之后等于把上界交回插件手里——插件把 max_tokens
	// 调小之后，上界与上报值又由同一方决定（见 UsageBound）。
	//
	// 记在这一步而不是让调用方传进来：调用方手上那份请求已经被补丁**就地**
	// 改过（见 internal/api 的 serveOnce），它拿不到原值。而「谁应用补丁、
	// 谁就记得补丁之前是什么」本来就是一件事，分开写迟早有一处忘了。
	s.usageBound = UsageBound{MaxOutputTokens: int64(req.MaxTokens)}

	res := s.OnRequest(ctx, RequestContextFrom(requestID, req, canonicalModel, protocol))
	if err := res.Err(); err != nil {
		return nil, err
	}
	if res.Patch == nil {
		return nil, nil
	}
	rejected, err := ApplyRequestPatch(req, res.Patch)
	if err == nil {
		return rejected, nil
	}
	// 补丁被拒是**契约违规**（ErrPatchScope），与「插件跑挂了」不是同一件事，
	// 但处置一样：这次补丁整个不生效。它必须记成一次失败，否则表现是
	// 「插件改了但没生效」——契约里最难查的一类静默。
	//
	// 没有「部分生效」要收拾：ApplyRequestPatch 只在写下去**之前**校验，
	// 所以走到这里时 req 一个字段都没被改过。
	f := &sandbox.Failure{Plugin: s.Plugin.Key, Hook: HookRequest, Kind: err}
	if s.record(f) {
		return nil, fmt.Errorf("插件声明为阻断型，它的 %s 补丁被拒: %w", HookRequest, err)
	}
	return nil, nil
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
	// AccumulatedText 是**在这一块之前**累积的正文，不含当前块。
	//
	// 它是**流式中断计费的依据**：客户端取消时内核用对应模型的分词器对它计数。
	// 所以插件替换正文时改的正是这个值，而不是只改「客户端看到的文本」——
	// 替换后的文本会进分词器，这一块的原文不会（见 StreamDecision）。
	AccumulatedText string
	// AccumulatedTokensApprox 是上面那段正文的 token 数，由内核按分词器
	// 增量估算。它与 AccumulatedText 是同一个累加器出来的两个数。
	AccumulatedTokensApprox int64
}

// OnStream 调 onStream。
//
// **每个内容块都调一次。** 采样（每 N 块调一次）会让「drop 第 3 块」这类
// 逐块决定失效，而那是这个 hook 的主要用途。逐块的代价是每块一次
// 跨语言调用，所以这里不对块大小做任何猜测——由内核决定怎么切块。
//
// 哪些块算内容块由**调用方**定：网关只把 delta 与 tool_use 交给它，
// usage 与 done / error 不进插件的手（见 gateway.Hooks.OnStream）。
// 这条边界放在网关那一侧，因为只有它看得见事件流；本函数只负责
// 「有就调、没有就跳过」。
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

// ApplyResponse 调 onResponse 并把补丁应用到响应上。
//
// 返回插件给的元数据（可能为 nil）与错误。**错误只在阻断型插件失败时非
// nil**——调用方据此让这次请求失败。非阻断的失败已经记进 Failures()，
// 响应照常发出去，只是这次补丁整个不生效。
//
// # 元数据为什么是返回值
//
// 它在协议层没有落点（见 ApplyResponsePatch），所以本包不能替调用方决定
// 往哪写。交回调用方之后，「元数据只进日志与快照」这条才有一处**看得见**
// 的实现，而不是一句文档承诺。
//
// # 流式请求拿不到它
//
// 那一条路上的响应是边生成边发的，没有「整份响应」这个时刻——调用方也
// 就没有东西可以传进来（见 docs/plugin-contract.md）。
//
// 返回的 nil 元数据是**合法结局**（这次没有元数据），不是「出错了但没报」
// ——所以下面两处显式 nolint，而不是造一个哨兵错误让每个调用方多判一次。
func (s *Session) ApplyResponse(ctx context.Context, requestID string, resp *channel.Response) (map[string]any, error) {
	if s == nil || resp == nil {
		return nil, nil //nolint:nilnil // 见上
	}
	res := s.OnResponse(ctx, ResponseContextFrom(requestID, resp))
	if err := res.Err(); err != nil {
		return nil, err
	}
	if res.Patch == nil {
		return nil, nil //nolint:nilnil // 见上
	}
	ApplyResponsePatch(resp, res.Patch)
	return res.Patch.Metadata, nil
}

// ApplyUsage 调 onUsage 并把上报的用量合并进内核的用量，返回结算该用的那一份。
//
// 返回的错误只在**阻断型插件失败**时非 nil——调用方据此让这次请求失败。
// 非阻断的失败已经记进 Failures()，用量照内核那份算。
//
// # 为什么合并这一步在包内
//
// 与 ApplyRequest / ApplyResponse 同一条理由，而这里更要紧：上报越界时
// 那份上报**整份丢掉**，而丢掉必须被看见——看不见的表现是「插件报了但没
// 生效」，契约里最难查的一类。记这次失败要用未导出的 record / blocking，
// 所以只能在包内做；在包外接一条「合并 + 打日志」的路，等于把「一次被丢掉
// 的上报必须看得见」留在纸上（见 MergeUsage 的注释）。
//
// # 上界从哪来
//
// 取 s.usageBound，也就是 ApplyRequest 在补丁之前记下的那个 max_tokens。
// 调用方传不进来：它手上那份请求已经被补丁就地改过（见 ApplyRequest）。
//
// # 没有上界时拒收
//
// 调用方没给 max_tokens、或这个会话根本没走过 ApplyRequest 时上界是 0，
// 那时任何正数上报都算越界。读不出上界就拒收是唯一安全的方向——把「没有
// 上界」当成「不设上界」，会让漏传一个参数变成关掉整道检查（见 UsageBound）。
func (s *Session) ApplyUsage(ctx context.Context, requestID, protocol string, core channel.Usage) (channel.Usage, error) {
	if s == nil {
		return core, nil
	}
	// raw 就是上游原样的那个 usage 对象。流式那条路上它只在流里带过 usage
	// 时才有——本地估算出来的用量没有「上游原样」可言（见 docs/plugin-contract.md）。
	res := s.OnUsage(ctx, UsageContextFrom(requestID, protocol, core.Raw))
	if err := res.Err(); err != nil {
		return core, err
	}
	if res.Patch == nil {
		// 插件没注册 onUsage，或者这次什么都没报。两条都是「用量照内核算」。
		return core, nil
	}
	merged, err := MergeUsage(*UsageFactsFrom(core), res.Patch, s.usageBound)
	if err != nil {
		// 越界是**运行期信号**而不是契约违规：同一份插件在不同请求上可能
		// 一次越界一次不越界（见 ErrUsageAboveBound）。整份上报已经由
		// MergeUsage 丢掉了，下面交回去的仍是内核那份。
		f := &sandbox.Failure{Plugin: s.Plugin.Key, Hook: HookUsage, Kind: err}
		if s.record(f) {
			return core, fmt.Errorf("插件声明为阻断型，它的 %s 上报被拒: %w", HookUsage, err)
		}
		return core, nil
	}
	// 重建回协议层用量时带上**内核那份**的口径：事实里自己声明了口径就
	// 以它为准，没声明就沿用内核的（见 UsageFacts.ChannelUsage）。
	return merged.ChannelUsage(core.CompletionIncludesReasoning), nil
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
//
// 覆盖不等于照单全收：上报要过一道上界，见 UsageBound 与 MergeUsage。
// 越界的那一份整份丢掉并按内核的值计费。
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

// UsageBound 是这次请求给插件上报用量划的那条线。
//
// # 它只能由内核给出
//
// 上界与上报值由同一方控制时，两者一起放大就等于没有上界：插件把上界抬高、
// 再照着抬高后的上界报用量，报多少就收多少。所以这里的每个数都必须来自
// **插件改不动**的来源：
//
//   - MaxOutputTokens 取客户端在请求里给的 max_tokens。插件只许把它调小
//     （见 ApplyRequestBodyPatch 与 applyBodyToChannel），所以客户端的原值
//     仍是上界，而补丁之后的值不是——那是插件自己挑的。传补丁后的值进来
//     等于把上界交回插件手里，这个类型拦不住，只能靠调用方取对。
//
// 输入侧（promptTokens / cacheReadTokens / cacheWriteTokens）**不在这里划**：
// 内核手上的输入量是启发式估算（见 internal/tokens），它的设计方向是
// 「宁可偏高」而不是「一定是上界」——拿它当上界会把上游报的合法
// prompt_tokens 拒掉，而退回估算的表现是少收。输入侧超报撞的是预留
// （结算时 Capped），不是无限计费。
//
// # 它是**一致性**信号，不是最后一道钱闸
//
// 拦住金额的是另外两层：「max_tokens 只许调小」（上界因此不会被插件抬高）
// 与「结算钳到预留」（billing 的 Capped）。这一条拦的是「插件的账与请求
// 自带的上限对不上」——那通常说明插件算错了或上游的报文不对。
//
// 所以**它拦不住一个想把账单顶到上限的插件**：客户端给 max_tokens=1000、
// 插件调到 10、再上报 completionTokens=900，这个值不超过 1000，判定界内；
// 而结算按调小后的 10 去钳，账单不会超过预留。别把这条当成金额防线——
// 金额防线是上面那两层，它们与插件无关。
type UsageBound struct {
	// MaxOutputTokens 是输出侧的上界：客户端请求里的 max_tokens。
	//
	// 非正表示调用方没给出上界。**此时任何正数上报都算越界**：这个字段的
	// 作用就是拦住抬高账单的上报，读不出上界时拒收是唯一安全的方向，
	// 而不是把「没有上界」当成「不设上界」——后者会让漏传一个参数
	// 变成关掉整道检查。
	MaxOutputTokens int64
}

// ErrUsageAboveBound 表示插件上报的用量超过了这次请求的上界。
//
// 与 ErrPatchScope 分开：那是**契约违规**（补丁的写法不对，作者回去改代码，
// 上传期就该被发现），而这是**这一次上报的数据越界**——同一份插件在不同请求上
// 可能一次越界一次不越界，所以它是一条运行期信号，要记成一次失败。
var ErrUsageAboveBound = errors.New("插件上报的用量超过了这次请求的上界")

// check 检查一份上报有没有越过上界。
//
// 只查输出侧，理由见 UsageBound。
//
// 逐项查而不是查总和：口径声明为「completion 含推理」时推理本来就在
// completion 里，两项相加会把同一批 token 数两遍，于是合法的上报被判越界。
// 代价是口径声明为「不含」时，两项各自贴在上界上（合起来是两倍）不算越界——
// 那一侧撞的是预留，不会无限计费。
//
// 想收紧成「口径为『已含』时逐项判、为『不含』时判总和」得先改 UsageBound 的
// 形状：这个检查跑在**合并之前**，而内核那份口径（UsageFacts 的
// CompletionIncludesReasoning）也可能为 nil，所以这里现在表达不出「已含」。
// 这是**没做**，不是漏了——别把它当成一个可以顺手补上的判断。
func (b UsageBound) check(reported UsageFacts) error {
	for _, item := range []struct {
		name string
		v    *int64
	}{
		{"completionTokens", reported.CompletionTokens},
		{"reasoningTokens", reported.ReasoningTokens},
	} {
		if item.v != nil && *item.v > b.MaxOutputTokens {
			return fmt.Errorf("%w: %s=%d 超过输出上界 %d（客户端请求里的 max_tokens）",
				ErrUsageAboveBound, item.name, *item.v, b.MaxOutputTokens)
		}
	}
	return nil
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
// # 越界的上报整份丢掉
//
// 任何一项超过 bound 时返回的是**内核那份原样**与 ErrUsageAboveBound：
// 于是即使调用方忽略了错误，也拿不到被夸大的用量，账单不会因此变高。
// 调用方要做的另一件事是把它记成一条失败——一次被丢掉的上报必须看得见，
// 否则它表现为「插件报了但没生效」。
//
// **那件事只有包内调用方做得到**：`Session.record` 与 `Session.blocking`
// 都未导出，`Session.Failures()` 只读。所以「调这个函数 + 记这次失败」
// 被收成了一个导出的入口——`Session.ApplyUsage`，请求路径调的是它。
// **不要**在包外另接一条只打日志的路，然后当文档里那句已经落地了。
//
// 上界由调用方给出，且必须是插件改不动的那个（见 UsageBound）。请求路径上
// 那个值由 `Session.ApplyRequest` 在补丁之前记下、由 ApplyUsage 取用，
// 于是「上界是补丁前的 max_tokens」这件事只有一处实现。
func MergeUsage(core UsageFacts, reported *UsageFacts, bound UsageBound) (UsageFacts, error) {
	if reported == nil {
		return core, nil
	}
	if err := bound.check(*reported); err != nil {
		return core, err
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
	return out, nil
}
