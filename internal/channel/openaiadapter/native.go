package openaiadapter

import (
	"encoding/json"
	"fmt"

	"github.com/sumwai/larkax/internal/channel"
)

// nativeEligible 报告这次请求能不能走同协议直通。
//
// 两个条件缺一不可：NativeBody 只说明手上有没有原文，Protocol 与适配器
// 自述的协议相等才说明原文就是上游要的那套报文。跨协议请求也可能带着原文
// （调用方发 Anthropic、路由到这条 OpenAI 渠道），那时必须由枢纽重新编码。
func (a *Adapter) nativeEligible(req *channel.Request) bool {
	return req != nil && req.NativeBody != nil && req.Protocol == a.Protocol()
}

// encodeNative 把调用方原文改成发往上游的报文：解析原文、覆写必须由网关
// 决定的字段、再序列化。
//
// 不先按统一字段编一份再回填：那样原文里统一结构不认识的字段会再丢一次，
// 直通也就没有意义了。
//
// 覆写的只有管道与渠道配置赖以运转的字段：
//
//   - model：选路之后这里是上游模型名（见 internal/route 的 upstreamRequest）；
//   - max_tokens：预留要一个输出上界；
//   - stream：这次调用走的是 Chat 还是 ChatStream，以参数为准；
//   - 流式时的 stream_options.include_usage：让上游在最后一片回 usage；
//   - reasoning_effort：渠道的方言映射。
//
// 其余字段一律照原文发出，包括本包与枢纽编码都不认识的字段。
func (a *Adapter) encodeNative(ch channel.Channel, req *channel.Request, stream bool) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(req.NativeBody, &m); err != nil {
		return nil, fmt.Errorf("解析原始请求体失败: %w", err)
	}

	m["model"] = req.Model

	// 输出上界统一叫 max_tokens：OpenAI 对 max_tokens 与
	// max_completion_tokens 同时出现会报错，而枢纽路径从来只发前者。
	//
	// 删另一个名字这件事与写入绑在一起：没有新的上界可写时不能删。否则
	// 原文只写了 max_completion_tokens、而这次调用没有上界时，那个键会
	// 凭空消失，上游于是按自己的默认值来——预留算出来的额度就落在一个
	// 上游随时可以超过的上界上。留着一个名字（它不是双字段）至少保住了
	// 原文那个值。
	if req.MaxTokens > 0 {
		delete(m, "max_completion_tokens")
		m["max_tokens"] = int64(req.MaxTokens)
	}

	// stream 取本次调用的参数，不取原文里的同名字段：Chat 与 ChatStream
	// 各自传一个常量，那才是这次要走哪条的判据。
	m["stream"] = stream

	if stream {
		// 要求上游在最后一个分片里带上 usage。原文已经带了
		// stream_options 时保留它的其它子键，只覆盖这一项。
		options, _ := m["stream_options"].(map[string]any)
		if options == nil {
			options = map[string]any{}
		}
		options["include_usage"] = true
		m["stream_options"] = options
	}

	applyNativeReasoningEffort(ch.Setting, m)

	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	return body, nil
}

// applyNativeReasoningEffort 按渠道的映射表翻译原文里的 reasoning_effort。
//
// 原文没有这个键、或它的值不是字符串时不动它。翻译成空串时删掉整个键，
// 与枢纽路径上该字段 omitempty 的效果一致。
func applyNativeReasoningEffort(setting map[string]any, m map[string]any) {
	effort, ok := m["reasoning_effort"].(string)
	if !ok {
		return
	}
	mapped := channel.ReasoningEffortDialect(setting, effort)
	if mapped == "" {
		delete(m, "reasoning_effort")
		return
	}
	m["reasoning_effort"] = mapped
}

// nativeRaw 把上游响应体解成排查用的 map，不做任何裁剪。
func nativeRaw(body []byte) (map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析原始响应失败: %w", err)
	}
	return raw, nil
}
