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
// messages / tools / tool_choice 这三项**不往 channel.Request 上写**：
// 它们在上游请求体里是结构化数据，而 channel.Request 用的是自己的类型
// （[]Message、[]Tool）。从插件的 JSON 视图反解回那些类型需要一份
// 「JSON → 结构化消息」的解析器，而那份解析器的每一条规则都是一个新的
// 契约面（插件能不能改 tool_use 的 id？能不能插一条 system 消息？）。
//
// 所以当前只应用**标量字段**：max_tokens、temperature、top_p。
// 结构化的那几项留在补丁里，由调用方按自己的需要决定——这是有意留的口子，
// 而不是漏掉了。见报告里「需要人决定」那一节。
func applyBodyToChannel(req *channel.Request, body *RequestBodyPatch) error {
	if body.MaxTokens != nil {
		if *body.MaxTokens <= 0 {
			return errNonPositiveMaxTokens(*body.MaxTokens)
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

// RouteContextFrom 把候选集翻成 onRoute 的上下文。
func RouteContextFrom(requestID, canonicalModel string, cands []route.Candidate) RouteContext {
	rc := RouteContext{
		RequestID:      requestID,
		CanonicalModel: canonicalModel,
		Candidates:     make([]RouteCandidate, 0, len(cands)),
	}
	for _, c := range cands {
		cand := RouteCandidate{
			ChannelID:   c.Channel.ID,
			OwnerUserID: c.OwnerUserID,
			Weight:      c.Weight,
			Priority:    c.Priority,
			Health:      healthName(c.Health),
			Confidence:  1,
		}
		if c.Quota.Known {
			// 查得到额度才给数字。**「查不到」不进这个字段**：
			// 给 0 会让插件以为「额度为零」，而那与「这家不支持查询」是两件事。
			remaining := c.Quota.Remaining
			cand.Remaining = &remaining
			if c.Quota.Estimated {
				// 估算值的可信度打折。插件据此决定要不要信它。
				cand.Confidence = 0.7
			}
		} else {
			cand.Confidence = 0
		}
		if c.CostPerUnit > 0 {
			cost := c.CostPerUnit
			cand.UpstreamCost = &cost
		}
		if c.Quota.ExpiresAt != nil {
			s := c.Quota.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
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
	case h.SuccessRate >= 0.9 && h.LatencyP95 > 0:
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
func UsageContextFrom(requestID, protocol string, raw map[string]any) UsageContext {
	return UsageContext{RequestID: requestID, Protocol: protocol, Raw: raw}
}

// UsageFactsFrom 把内核的用量翻成插件看到的事实（onResponse 的 usage 字段）。
//
// 它是**只读的一组数字**：插件在 onResponse 里改不了用量，
// 因为那个字段是内核算出来的，而插件要改只能走 onUsage 的上报通道。
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
