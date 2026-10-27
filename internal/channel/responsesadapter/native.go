package responsesadapter

import (
	"encoding/json"
	"fmt"

	"github.com/sumwai/larkax/internal/channel"
)

// nativeEligible 判断这次调用能不能直接拿调用方发来的原文当报文。
//
// 两个条件都要判：
//
//   - 原文不为 nil。跨协议请求也可能带着原文（调用方发 Anthropic、选中的却是
//     Responses 渠道），那时报文必须按本适配器的协议重新编一份。
//   - req.Protocol 与 a.Protocol() 相等。前者是**调用方走的入口**，后者是
//     这条渠道声明的协议，两者相同才是同协议直通。
func (a *Adapter) nativeEligible(req *channel.Request) bool {
	return req != nil && req.NativeBody != nil && req.Protocol == a.Protocol()
}

// encodeNative 以调用方发来的原文为报文来源，只覆写必须由网关决定的字段。
//
// 报文由此生成，而不是按统一字段编一份再从原文回填几处：后者会把原文里
// 转换层不认识的字段再丢一次，直通就没有意义了。
//
// 覆写的几处，每一处都撑着一条链路：
//
//   - model：调用方发的是别名，上游要的是这个渠道上的真名（选路层已经翻好）。
//   - max_output_tokens：预留额靠输出上界算，没有它算不出这一笔。为 0 时不写，
//     走上游默认值——与枢纽分支同样不补默认值。
//   - stream：这次调用走哪条路径由适配器决定，不取原文里的取值。
//   - reasoning.effort：渠道配置的方言映射（见 channel.ReasoningEffortDialect）。
//   - temperature / top_p：上层已经决定了的采样参数（渠道别名覆盖、插件
//     补丁），非 nil 时无条件覆盖原文里的同名键。这与 reasoning.effort 那类
//     「按原文携带才翻译」的字段不同：这里是上层已经作出的决定，不是方言换算。
//
// 原文里的 max_tokens 要删掉：Responses 认的是 max_output_tokens，留着它会让
// 上游收到一个它不认的字段，而真正的上界可能还停在原处。
//
// 原文若带 stream_options 就原样保留，这里不新增它：Responses 的用量跟着终态
// 事件一起来，不需要 chat 那边的 opt-in 参数。
func (a *Adapter) encodeNative(ch channel.Channel, req *channel.Request, stream bool) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(req.NativeBody, &m); err != nil {
		return nil, fmt.Errorf("解析原始请求体失败: %w", err)
	}
	if m == nil {
		// 字面量 null 能解进 map 而不报错，但它不是一条请求报文。
		return nil, fmt.Errorf("解析原始请求体失败: 原文不是一个 JSON 对象")
	}

	m["model"] = req.Model
	m["stream"] = stream

	// temperature / top_p：渠道别名覆盖与插件补丁都写这两个字段（见
	// internal/overrides 与 internal/plugin）。直通以原文为报文来源，不覆写
	// 就等于把它们丢掉——商家配的采样参数会静默失效，而两边都不报错。
	// nil 表示这次没有人要覆盖它，此时不动原文里的同名键：不写也不删，
	// 调用方自己写在原文里的值仍然有效。这与上面 max_tokens 那一格不同：
	// 那是「同一个上界有两个名字」，删错会让值凭空消失；这里同名同义，
	// 没有要替调用方做的决定。
	if req.Temperature != nil {
		m["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		m["top_p"] = *req.TopP
	}

	// 原文若用了 chat 那边的错名字 max_tokens，删掉它：Responses 认的是
	// max_output_tokens，留着它只会让上游收到一个它不认识的字段，而真正的上界
	// 还停在原处。网关这一侧的上界一律写成新名字；没有上界时不写，走上游默认值。
	delete(m, "max_tokens")
	if req.MaxTokens > 0 {
		m["max_output_tokens"] = int64(req.MaxTokens)
	}

	// 原文里的 tools 也要过同一道判据：枢纽那条路在序列化之前查它，直通
	// 绕过去就少一道。入口解析层今天已经拦下非 function 的声明
	// （relay.checkResponsesTools），所以这条判据在这里打不着；但它是
	// 「发出去之前的最后一道」，两条路都该有。
	if tools, ok := m["tools"]; ok {
		encoded, err := json.Marshal(tools)
		if err != nil {
			return nil, fmt.Errorf("序列化工具声明失败: %w", err)
		}
		if mcpErr := checkNoRemoteMCPTools(encoded); mcpErr != nil {
			return nil, mcpErr
		}
	}

	// reasoning.effort 嵌在 reasoning 对象里（顶层 reasoning_effort 是 chat 的
	// 形状）。只在原文本来就带了字符串 effort 时翻译：没带就没有要翻的东西，
	// 也不该因为配了映射表就凭空多出一项推理配置。
	if reasoning, ok := m["reasoning"].(map[string]any); ok {
		if effort, isString := reasoning["effort"].(string); isString {
			if translated := channel.ReasoningEffortDialect(ch.Setting, effort); translated == "" {
				delete(reasoning, "effort")
			} else {
				reasoning["effort"] = translated
			}
			// 留一个 `{}` 给上游，等于让它去猜这次到底要不要推理。
			if nativeReasoningEmpty(reasoning) {
				delete(m, "reasoning")
			}
		}
	}

	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	return body, nil
}

// nativeReasoningEmpty 判断原文里的 reasoning 对象是否已经没有任何要发出去的
// 内容。判据与 reasoningHasNothing 同源：effort 与 summary 是空串、mode 与
// context 是 null、其余键不存在，都算没有内容。
//
// 与那个函数的一处差别：这里的对象来自原文，键集不受我们控制。不认识的键
// 一律算「有内容」——原文里有它，就说明上游那边要收它，删掉整块等于静默丢一个
// 我们不理解的参数。
func nativeReasoningEmpty(reasoning map[string]any) bool {
	for key, v := range reasoning {
		switch key {
		case "effort", "summary":
			if s, ok := v.(string); ok {
				if s != "" {
					return false
				}
				continue
			}
			if v != nil {
				return false
			}
		default:
			if v != nil {
				return false
			}
		}
	}
	return true
}
