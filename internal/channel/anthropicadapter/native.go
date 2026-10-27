package anthropicadapter

import (
	"encoding/json"
	"fmt"

	"github.com/sumwai/larkax/internal/channel"
)

// nativeEligible 判断这次请求能不能走同协议直通。
//
// 两个条件都要成立：
//
//   - 手上有一份原始请求体（NativeBody 非 nil）。内部调用与跨协议请求没有它；
//   - 入口协议与本渠道的协议相同。跨协议请求也可能带着原文（调用方发
//     Anthropic、路由到 OpenAI 渠道），那时原文与这条渠道说的不是一套协议，
//     必须按统一字段重新编码。
//
// 只判第一个条件是不够的：那会把一份 Anthropic 报文原样发到说别的协议的
// 渠道上。
func (a *Adapter) nativeEligible(req *channel.Request) bool {
	return req != nil && req.NativeBody != nil && req.Protocol == a.Protocol()
}

// encodeNative 以调用方的原始请求体为报文来源，只改动必须由网关决定的字段。
//
// 做法是「解析原文 → 覆写 → 序列化」，不是在编好的报文上回填几个字段：
// 后者会把原文里 dto 不认识的字段（thinking、cache_control、top_k、
// stop_sequences、metadata 等）再丢一次，正好废掉直通。
//
// 覆写的字段各支撑一条链路：
//
//   - model：调用方说的是别名，上游要的是真名。req.Model 到这里已经是
//     上游名（路由层翻完名之后才交给适配器）。
//   - max_tokens：请求的上界，预留额度要它；Anthropic 也把它当必填。
//   - stream：这一次调用走非流式还是流式由入口决定，不能由原文说了算。
//
// 推理相关的字段不在这里覆写：Anthropic 用它自己的 thinking 对象表达，没有
// reasoning_effort 这个概念（枢纽那条路编码时也不写它）。原文里带了别的协议
// 的字段就原样留着，我们不改动自己不该解释的键。
func (a *Adapter) encodeNative(req *channel.Request) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(req.NativeBody, &m); err != nil {
		// 不在解析失败时退回按统一字段编码：那会让「直通的报文与原文不同」
		// 变成一次静默的行为变化，调用方无从分辨这次到底走了哪条路。
		return nil, fmt.Errorf("解析原始请求体失败: %w", err)
	}
	if m == nil {
		// 原文是 JSON 的 null：没有字段可覆写，写进去会 panic。
		return nil, fmt.Errorf("解析原始请求体失败: 不是 JSON 对象")
	}

	m["model"] = req.Model

	// 上界用 int64 而不是 int：issue 里与测试都按整数比，平台宽度不该
	// 泄漏进报文。
	if req.MaxTokens > 0 {
		m["max_tokens"] = int64(req.MaxTokens)
	} else {
		m["max_tokens"] = int64(DefaultMaxTokens)
	}

	// 总是写：原文可能是上一次调用的形状（或者根本没有这个键），而这次走
	// 哪条路只由本次调用决定。
	m["stream"] = req.Stream

	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	return body, nil
}
