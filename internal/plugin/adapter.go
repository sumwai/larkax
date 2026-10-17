package plugin

import (
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// 这个文件是**入口层与插件层之间的翻译**：把内核类型翻成插件看得见的形状，
// 再把插件给的补丁翻回来。
//
// 为什么翻译单独放一层而不让插件直接用 channel.Request：
// 那些类型里有凭证、有渠道配置、有整份上游响应。共用类型意味着「给那个类型
// 加一个字段」会静默扩大插件的可见范围，而没有人会去检查插件层——
// 一次不小心的字段添加就成了一次权限扩张。翻译层让这件事必须显式发生。

// RequestContextFrom 把统一的请求翻成 onRequest 的上下文。
//
// **凭证不进这个函数**：它拿不到 channel.Channel，所以没有地方能读到密钥。
// 这不是「记得别传」，是「签名里没有」。
func RequestContextFrom(requestID string, req *channel.Request, canonicalModel, protocol string) RequestContext {
	rc := RequestContext{
		RequestID:      requestID,
		CanonicalModel: canonicalModel,
		UpstreamModel:  req.Model,
		Protocol:       protocol,
		Streamed:       req.Stream,
		Headers:        map[string]string{},
	}
	// Body 是重建出来的「协议中立」视图，而不是上游 JSON 原文：
	// 插件看到的是内核理解的请求，于是它的判断在内核与插件之间是同一件事。
	// 直接用原文会让插件按某一家的字段名写逻辑，而那些字段名会随适配器变化。
	rc.Body = map[string]any{
		"model":    req.Model,
		"messages": messagesToJSON(req.Messages),
	}
	if req.MaxTokens > 0 {
		rc.Body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		rc.Body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		rc.Body["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		rc.Body["tools"] = tools
	}
	return rc
}

func messagesToJSON(msgs []channel.Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]any, 0, len(m.Content))
		for _, b := range m.Content {
			block := map[string]any{"type": b.Type}
			if b.Text != "" {
				block["text"] = b.Text
			}
			if b.ImageURL != "" {
				block["imageUrl"] = b.ImageURL
			}
			if b.ToolUseID != "" {
				block["toolUseId"] = b.ToolUseID
			}
			if b.ToolName != "" {
				block["toolName"] = b.ToolName
			}
			if b.ToolInput != nil {
				block["toolInput"] = b.ToolInput
			}
			blocks = append(blocks, block)
		}
		out = append(out, map[string]any{"role": m.Role, "content": blocks})
	}
	return out
}

// ApplyRequestPatch 把 onRequest 的补丁应用到统一请求上。
//
// 返回被拒的请求头名与错误。**被拒的头要报出去**：插件作者写了一个被拒的头
// 却没有任何反馈，会以为是上游不支持。
//
// 补丁能改的字段是有限的那几个，所以这里不需要「记得跳过 model」这类判断——
// RequestBodyPatch 里没有 model。
func ApplyRequestPatch(req *channel.Request, patch *RequestPatch) (rejected []string, err error) {
	if patch == nil || req == nil {
		return nil, nil
	}
	if patch.Body != nil {
		if err := applyBodyToChannel(req, patch.Body); err != nil {
			return nil, err
		}
	}
	if len(patch.Headers) > 0 {
		merged, rejected := ApplyHeaderPatch(req.Headers, patch.Headers)
		req.Headers = merged
		return rejected, nil
	}
	return nil, nil
}

// applyBodyToChannel 把补丁里的字段写回统一请求。
//
// # 能改的只有三个标量
//
// 写下去的是 `max_tokens`、`temperature`、`top_p`（见 appliedBodyFields）。
// `messages` / `system` / `stop` / `tools` / `tool_choice` 这 5 个**一律拒绝**，
// 由 unappliedFields 判定，理由也写在那里：它们在上游请求体里是结构化数据，
// 而 channel.Request 用的是自己的类型（[]Message、[]Tool），从插件的 JSON
// 视图反解回那些类型需要一份「JSON → 结构化消息」的解析器，而那份解析器的
// 每条规则都是一个新的契约面（插件能不能改 tool_use 的 id？能不能插一条
// system 消息？）。那是权限边界，要单独拍板。
//
// **有意的是不应用，不是静默。** 这段注释早先写的是「结构化的那几项留在补丁里，
// 由调用方按自己的需要决定——这是有意留的口子」，而两条应用路径都不写它们、
// 也不报错：作者看到的是「我的插件没生效」，与字段名拼错一模一样。现在给了
// 这几个字段就返回 ErrPatchScope，与「写了契约之外的字段」同一个处置。
//
// 判定在**写下去之前**：走到错误那一支时 req 一个字段都没被改过，
// 于是「整份补丁不生效」是这里唯一可能的结局（见 Session.ApplyRequest）。
func applyBodyToChannel(req *channel.Request, body *RequestBodyPatch) error {
	// 与 patch.go 的 ApplyRequestBodyPatch 调同一个判据：两条应用路径对同一份
	// 补丁必须给出同一个判定，只拦一条的话，接线时用哪条就把洞开在哪条上。
	if fields := body.unappliedFields(); len(fields) > 0 {
		return errBodyFieldsNotApplied(fields)
	}
	if body.MaxTokens != nil {
		if *body.MaxTokens <= 0 {
			return errNonPositiveMaxTokens(*body.MaxTokens)
		}
		// 只许调小。这一条在 patch.go 的 ApplyRequestBodyPatch 里也有一份，
		// 因为那是**另一条**应用路径（协议中立视图）：两条都得拦，
		// 只拦一条的话，接线时用哪条就把洞开在哪条上。
		//
		// 原值取 channel.Request.MaxTokens，也就是客户端请求里那个数——
		// 补丁还没写回去。0 表示客户端没给，那时补丁给任何值都是新增一个
		// 上界，而「上界从哪来」正是这条规则要管的（见 UsageBound）。
		//
		// 「客户端没给」这个情形由下面这一个比较覆盖：那时 original ≤ 0，
		// 而上面刚保证补丁值 ≥ 1，所以 `> original` 必然成立。**不要再写成
		// `!hasOriginal || ...`**——它看起来像一道独立的门，删掉却不会放开
		// 任何东西（实测过，patch.go 那一处同形）。
		original, hasOriginal := int64(req.MaxTokens), req.MaxTokens > 0
		if *body.MaxTokens > original {
			return errMaxTokensRaised(original, hasOriginal, *body.MaxTokens)
		}
		req.MaxTokens = int(*body.MaxTokens)
	}
	if body.Temperature != nil {
		v := *body.Temperature
		req.Temperature = &v
	}
	if body.TopP != nil {
		v := *body.TopP
		req.TopP = &v
	}
	return nil
}

// ApplyResponsePatch 把 onResponse 的补丁应用到统一响应上。
//
// # 只有正文能落地
//
// `ResponsePatch.Metadata` **不进协议层**：channel.Response 上没有对应字段，
// 而它也不该有——那个类型是协议转换与渲染的输入，往里加一个插件专用的 map
// 会让它跟着响应一路走到调用方，而契约里它只是插件留给自己的一段附注。
// 所以元数据的落点是日志与快照，由调用方决定（见 Session.ApplyResponse）。
//
// # 正文怎么合并
//
// `text` 在插件那一侧是**拼接后的整段正文**（见 ResponseContextFrom 的
// body.text），所以写回来时也是整段替换：原有的文本块合成一个，位置取
// 第一块文本的位置，非文本的块（tool_use 等）原样留在原位。补丁里没有
// 工具调用，**不要顺手把别的块也改掉**——那是契约之外的东西。
//
// 一个都没有文本块时追加一块：上游只回了工具调用、而插件要补一句说明，
// 是这个字段说得通的用法。追加而不是拒绝，是因为「响应里没有正文」不是
// 补丁的问题。
//
// 不改 Usage 与 FinishReason：前者是计费依据、后者是「这次回答是否完整」
// 的判定，两者都不在 ResponsePatch 里（见它的注释）。
func ApplyResponsePatch(resp *channel.Response, patch *ResponsePatch) {
	if resp == nil || patch == nil || patch.Text == nil {
		return
	}
	resp.Message.Content = textBlocksReplaced(resp.Message.Content, *patch.Text)
}

// textBlocksReplaced 把一串内容块里的文本块换成单独一块。
//
// 非文本块保持原顺序与原位置，文本块只留第一处——插件给的是一整段文本，
// 留着后面那几块会让同一段正文出现两次。
func textBlocksReplaced(blocks []channel.ContentBlock, text string) []channel.ContentBlock {
	out := make([]channel.ContentBlock, 0, len(blocks)+1)
	placed := false
	for _, b := range blocks {
		if b.Type != "text" {
			out = append(out, b)
			continue
		}
		if !placed {
			out = append(out, channel.ContentBlock{Type: "text", Text: text})
			placed = true
		}
	}
	if !placed {
		out = append(out, channel.ContentBlock{Type: "text", Text: text})
	}
	return out
}

// RouteContextFrom 把候选集翻成 onRoute 的上下文。
//
// # 额度余量要不要给，是候选集的性质而不是候选的性质
//
// 「剩余 800000」与「剩余 5」只有在同一单位下才有大小之分，而单位由候选
// 自带的额度说明（route.Candidate.Quota.Unit），各渠道可以不同。插件对这一项
// 唯一说得通的用法是
// **跨候选比较**（挑一个先用掉），那是一个集合级的操作，所以判断必须在这里
// 做完，不能甩给插件：它跑在沙箱里，也看不到「这一组数可不可比」之外的上下文。
//
// 判据与内核打分共用一处实现（route.MixedQuotaUnits），于是插件看到的
// 「可不可比」与内核排序时的口径是同一件事。
func RouteContextFrom(requestID, canonicalModel string, cands []route.Candidate) RouteContext {
	rc := RouteContext{
		RequestID:      requestID,
		CanonicalModel: canonicalModel,
		Candidates:     make([]RouteCandidate, 0, len(cands)),
	}
	// 混合单位时**一个数都不给**，包括单位相同的那几个：只给一部分会让
	// 「有的有数、有的没数」看起来像那几家查额度失败了，而事实是这一组数
	// 整体不可比。
	mixedUnits := route.MixedQuotaUnits(cands)
	for _, c := range cands {
		cand := RouteCandidate{
			ChannelID:   c.Channel.ID,
			OwnerUserID: c.OwnerUserID,
			Weight:      c.Weight,
			Priority:    c.Priority,
			Health:      healthName(c.Health),
			Confidence:  1,
		}
		// 给数字要同时满足两件事：这个渠道有余量，且这一组余量可比。
		// 缺任何一件都是 Remaining 为 null、Confidence 为 0——「没有数」
		// 与「有一个可信的数」是这里仅有的两种状态。可信度跟着数字一起给，
		// 免得出现「可信度 1 而数字是 null」这种读不通的组合。
		if c.Quota.Remaining != nil && !mixedUnits {
			// **「没有数值」不进这个字段**：给 0 会让插件以为「额度为零」，
			// 而那与「这家不支持查询」是两件事。
			remaining := *c.Quota.Remaining
			cand.Remaining = &remaining
			if c.Quota.Estimated {
				// 估算值的可信度打折。插件据此决定要不要信它。
				cand.Confidence = 0.7
			}
		} else {
			cand.Confidence = 0
		}
		// 与 Remaining 同一条规矩：没有数据就不给数字。route 侧用 nil 表示
		// 「没有观测到成本」，指针本身就可以直接交出去——不给 0，
		// 因为 0 在插件眼里是「这个渠道免费」。
		if c.CostPerUnit != nil {
			cost := *c.CostPerUnit
			cand.UpstreamCost = &cost
		}
		if c.Quota.ResetsAt != nil {
			s := c.Quota.ResetsAt.UTC().Format("2006-01-02T15:04:05Z")
			cand.ResetsAt = &s
		}
		rc.Candidates = append(rc.Candidates, cand)
	}
	return rc
}

// healthName 把健康度翻成契约里的四档。
//
// 四档而不是原始数字：插件要做的是「还能不能用」这个判断，
// 而连续失败次数与成功率的组合没有唯一答案——把它留给内核，
// 插件拿到的是一个已经判过的结论。
func healthName(h route.Health) string {
	switch {
	case h.ConsecutiveFailures >= 3:
		return "drained"
	case h.ConsecutiveFailures > 0:
		return "degraded"
	case h.SuccessRate > 0 && h.SuccessRate < 0.9:
		return "degraded"
	case h.SuccessRate >= 0.9 && h.LatencyP95 != nil:
		return "healthy"
	default:
		// 没有样本时是「不知道」，而不是「健康」——
		// 后者会让插件用「优先选健康的」这类规则把一个从没跑过的渠道排前面。
		return "unknown"
	}
}

// UsageContextFrom 把上游原始的 usage 翻成 onUsage 的上下文。
//
// raw 是**上游原样的对象**：插件比内核更懂它的上游，而口径变化（上游改了
// 字段名、加了新的计量单位）在 raw 里才看得见。
//
// raw 为 nil 是**合法输入**，不是漏传：流式那条路上用量可能是内核本地估算的
// （客户端断开、上游没在流里带 usage），估算值没有「上游原样」可言。插件据此
// 能分辨「上游报了个零」与「这次根本没有上游的数」——前者是一个对象，各项为 0。
func UsageContextFrom(requestID, protocol string, raw map[string]any) UsageContext {
	return UsageContext{RequestID: requestID, Protocol: protocol, Raw: raw}
}

// StreamChunkFrom 把统一的流事件翻成插件看到的「当前块」。
//
// # 为什么不是 channel.StreamEvent 本身
//
// 与本文件开头同一条理由：共用一个类型意味着给那个类型加一个字段就静默
// 扩大了插件的可见范围。这里只挑出插件真正用得上的那几项，加一项必须显式发生。
//
// 形状由 kind 决定：
//
//	{ kind: "delta",    text: "..." }
//	{ kind: "tool_use", toolCall: { index, id, name, arguments } }
//
// 只有**内容块**会走到这里。usage 与 done / error 是这次流的计费依据与框架，
// 网关根本不会把它们交给插件（见 gateway.Hooks.OnStream），所以这里也不必
// 为它们留形状——插件看不到的东西，写进契约只会让人以为拿得到。
func StreamChunkFrom(ev channel.StreamEvent) map[string]any {
	out := map[string]any{"kind": ev.Kind}
	switch ev.Kind {
	case "delta":
		out["text"] = ev.Text
	case "tool_use":
		if ev.ToolCall != nil {
			out["toolCall"] = map[string]any{
				"index":     ev.ToolCall.Index,
				"id":        ev.ToolCall.ID,
				"name":      ev.ToolCall.Name,
				"arguments": ev.ToolCall.Arguments,
			}
		}
	}
	return out
}

// UsageFactsFrom 把内核的用量翻成插件看到的事实（onResponse 的 usage 字段）。
//
// 它是**只读的一组数字**：插件在 onResponse 里改不了用量，
// 因为那个字段是内核算出来的，而插件要改只能走 onUsage 的上报通道。
//
// 不带 CompletionIncludesReasoning：那是内核自己那份用量的口径，由内核在
// 重建时给出（见 UsageFacts.ChannelUsage），不需要绕一圈经过插件。
func UsageFactsFrom(u channel.Usage) *UsageFacts {
	n := func(v int64) *int64 {
		if v == 0 {
			return nil
		}
		return &v
	}
	facts := &UsageFacts{
		PromptTokens:     n(u.PromptTokens),
		CompletionTokens: n(u.CompletionTokens),
		CacheReadTokens:  n(u.CacheReadTokens),
		CacheWriteTokens: n(u.CacheWriteTokens),
		ReasoningTokens:  n(u.ReasoningTokens),
	}
	if len(u.UnitCounts) > 0 {
		units := make(map[string]int64, len(u.UnitCounts))
		for k, v := range u.UnitCounts {
			units[k] = v
		}
		facts.Units = units
	}
	return facts
}

// ChannelUsage 把用量事实重建回协议层的用量，供结算使用。
//
// # 口径为什么是必需参数
//
// 「completion 里含不含推理」在数字上看不出来，而它决定结算会不会把同一批
// token 收两次费。所以 coreIncludesReasoning——**内核自己那份用量的口径**——
// 由调用方显式给出，而不是让字段落到零值：零值是「不含」，那会让 OpenAI 系
// 上游（completion_tokens 含 reasoning_tokens）的推理量被 completion 与
// reasoning 各乘一次单价，而账单只是偏高，对账发现不了。
//
// 事实里自己声明了口径时以它为准：插件比内核更懂它代理的那个上游，
// 而它报的量可能来自与内核不同的口径（例如插件自己估的用量）。
//
// 请求路径上的调用方是 `Session.ApplyUsage`：它拿内核那份用量的事实去合并
// 插件上报，再把合并结果重建回这里，最后交给结算。
func (f UsageFacts) ChannelUsage(coreIncludesReasoning bool) channel.Usage {
	u := channel.Usage{CompletionIncludesReasoning: coreIncludesReasoning}
	if f.CompletionIncludesReasoning != nil {
		u.CompletionIncludesReasoning = *f.CompletionIncludesReasoning
	}

	// 指针的 nil 在这里就是 0：事实里的「没上报这一项」与协议层的
	// 「这一项是 0」在重建之后是同一个意思，计费侧不需要区分
	// （channel.Usage 的数字字段本来就没有三态）。
	set := func(dst *int64, src *int64) {
		if src != nil {
			*dst = *src
		}
	}
	set(&u.PromptTokens, f.PromptTokens)
	set(&u.CompletionTokens, f.CompletionTokens)
	set(&u.CacheReadTokens, f.CacheReadTokens)
	set(&u.CacheWriteTokens, f.CacheWriteTokens)
	set(&u.ReasoningTokens, f.ReasoningTokens)

	if len(f.Units) > 0 {
		// 复制而不是共用：重建出来的用量会跟着请求走到结算，而事实仍可能被
		// 调用方持有（合并结果、日志）。共用一张 map 时，对一边的改动会
		// 静默出现在另一边。
		units := make(map[string]int64, len(f.Units))
		for k, v := range f.Units {
			units[k] = v
		}
		u.UnitCounts = units
	}
	return u
}

// ResponseContextFrom 把统一的响应翻成 onResponse 的上下文。
func ResponseContextFrom(requestID string, resp *channel.Response) ResponseContext {
	if resp == nil {
		return ResponseContext{RequestID: requestID}
	}
	facts := UsageFactsFrom(resp.Usage)
	rc := ResponseContext{
		RequestID: requestID,
		Status:    200,
		Body: map[string]any{
			"model":  resp.UpstreamModel,
			"finish": resp.FinishReason,
			"text":   messageText(resp.Message),
		},
	}
	// usage 一定给（哪怕全是 nil）：插件需要能区分「上游没报用量」
	// 与「用量是零」。前者是 nil 对象的各项 nil，后者是明确的 0。
	if facts != nil {
		rc.Usage = facts
	} else {
		rc.Usage = &UsageFacts{}
	}
	return rc
}

func messageText(m channel.Message) string {
	var out string
	for _, b := range m.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out
}
