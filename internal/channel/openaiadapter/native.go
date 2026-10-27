package openaiadapter

import (
	"bytes"
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
//   - reasoning_effort：渠道的方言映射；
//   - temperature / top_p：上层已经决定了的采样参数（渠道别名覆盖、插件
//     补丁），非 nil 时无条件覆盖原文里的同名键。这与 reasoning_effort 那类
//     「按原文携带才翻译」的字段不同：这里是上层已经作出的决定，不是方言换算。
//
// 其余字段一律照原文发出，包括本包与枢纽编码都不认识的字段。
func (a *Adapter) encodeNative(ch channel.Channel, req *channel.Request, stream bool) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(req.NativeBody, &m); err != nil {
		return nil, fmt.Errorf("解析原始请求体失败: %w", err)
	}
	if m == nil {
		// 字面量 null 能解进 map 而不报错，但它不是一条请求报文。
		return nil, fmt.Errorf("解析原始请求体失败: 不是 JSON 对象")
	}

	m["model"] = req.Model

	// temperature / top_p：渠道别名覆盖与插件补丁都写这两个字段（见
	// internal/overrides 与 internal/plugin）。直通以原文为报文来源，不覆写
	// 就等于把它们丢掉——商家配的采样参数会静默失效，而两边都不报错。
	// nil 表示这次没有人要覆盖它，此时不动原文里的同名键：不写也不删，
	// 调用方自己写在原文里的值仍然有效。这与上面 max_completion_tokens 那一格
	// 不同：那是「同一个上界有两个名字」，删错会让值凭空消失；这里同名同义，
	// 没有要替调用方做的决定。
	if req.Temperature != nil {
		m["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		m["top_p"] = *req.TopP
	}

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
//
// 用解码器而不是 json.Unmarshal：这个函数的名字与另两个适配器里同名的那一处
// （internal/channel/anthropicadapter 与 responsesadapter）说的是同一件事——
// 输入是响应体。那边传进来的是 io.ReadAll 读出的整段字节；Unmarshal 要求
// 整段恰好是一个值，尾随内容会让它报错，而那条错误带 Retryable，路由于是
// 换下一个候选重发，同一笔钱付两次。解码器读出一个值就停，与响应入口那次
// 解析同宽严。
//
// 本包的调用方目前传的是入口已经解出来的那一个值（见 adapter.go 的 Chat，
// parsed 是 json.RawMessage），尾随字节到不了这里。这条约束是留给整段字节
// 那种输入的，也让三处同名的函数保持同一形状。
func nativeRaw(body []byte) (map[string]any, error) {
	var raw map[string]any
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析原始响应失败: %w", err)
	}
	return raw, nil
}
