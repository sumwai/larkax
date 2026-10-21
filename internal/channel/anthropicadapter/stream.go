package anthropicadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/relay"
)

// Anthropic 的流式是一串**命名事件**，而不是「每片一个 choices」：
//
//	event: message_start        → message 对象，含 usage.input_tokens
//	event: content_block_start  → 一个内容块的开始（text / tool_use / thinking）
//	event: content_block_delta  → 该块的增量（text_delta / input_json_delta / thinking_delta）
//	event: content_block_stop   → 该块结束
//	event: message_delta        → stop_reason，含 usage.output_tokens
//	event: message_stop         → 整个消息结束
//	event: ping                 → 心跳
//	event: error                → {"type":"error","error":{...}}
//
// 用量跨**两个**事件：输入在 message_start，输出在 message_delta。
// 只读其中一个会得到「有输入没输出」或反之，而两者都是算钱的依据——
// 所以这里跨事件累计，直到 message_stop 才发出完整的用量。
//
// 输入侧例外：它在 message_start 就确定了，所以那一帧到达时**另外**发一个
// usage_input 事件。它不是结算依据（只有输入，没有输出），用处是让调用方
// 在流的最前面就能报出输入用量——那个数在 message_start 里就要填，
// 而完整的用量要到流末尾才有（见 channel.StreamEvent 的 Usage）。

// pump 读取 SSE 流并翻译成统一事件。
//
// status 是这条流对应响应的状态码（调用方已经过了 CheckStatus，所以它是
// 2xx）。流里的错误都发生在**响应头之后**，所以它们都是「拿到过响应」的
// 失败——把它带进错误里，下游才不用猜。
//
// completionIncludesReasoning 是渠道声明的用量口径（见
// channel.UsageCompletionIncludesReasoning），原样交给 relay.UsageFromRelaykit。
func (a *Adapter) pump(ctx context.Context, status int, completionIncludesReasoning bool, body io.Reader, events chan<- channel.StreamEvent) {
	scanner := bufio.NewScanner(body)
	// 单个事件可能很大：长文本的一次性增量、或一整段 tool 参数。
	// 默认的 64KB 上限会把合法响应判为读取失败。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		// 跨事件累计的用量。两者在 message_stop 时一并发出。
		inputTokens  int64
		outputTokens int64
		cacheRead    int64
		cacheWrite   int64

		stopReason string
		usageSent  bool
	)

	// event 名与 data 载荷分两行到达，需要记住当前的 event 名。
	// 有些上游会省略 event: 行只发 data:，那时只能靠载荷里的 type 字段判断，
	// 所以下面的分派优先用载荷里的 type。
	var eventName string

	// usageOf 把**此刻**累计到的数字换算成统一用量。
	//
	// 输入与输出是两个时刻给的（message_start / message_delta），所以同一件换算
	// 要用两次：一次在输入刚确定时（输出那部分是 0），一次在流末尾。抽出来是
	// 为了让两次走**同一个**入口——各写一份的话，以后改口径只会改到其中一处。
	//
	// 与 fromClaudeResponse 也走同一个换算入口：口径由 UsageSemantic 声明，
	// 「把缓存补回输入」只在 relay.UsageFromRelaykit 里发生一次。
	//
	// 这里曾经自己拼 channel.Usage 并自己加一遍缓存，那是同一件换算的
	// 第三份实现——两份实现各加一次时，整段 prompt 命中缓存
	// （input_tokens == 0）的请求会把缓存算两遍。
	//
	// 推理口径由渠道声明（completionIncludesReasoning）：Anthropic 的
	// output_tokens 含扩展思考的 token，而协议不单独报推理量，所以这里也
	// 不填 ReasoningTokens；声明缺省时沿用「已含」。
	usageOf := func() channel.Usage {
		return relay.UsageFromRelaykit(&dto.Usage{
			PromptTokens:     int(inputTokens),
			CompletionTokens: int(outputTokens),
			PromptTokensDetails: dto.InputTokenDetails{
				CachedTokens:         int(cacheRead),
				CachedCreationTokens: int(cacheWrite),
			},
			UsageSource:   dto.BillingUsageSourceClaudeMessages,
			UsageSemantic: dto.BillingUsageSemanticAnthropic,
		}, completionIncludesReasoning)
	}

	emitUsage := func() {
		if usageSent {
			return
		}
		usageSent = true
		u := usageOf()
		events <- channel.StreamEvent{Kind: "usage", Usage: &u}
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			// 客户端断开或上层取消：已产生的费用照记，由调用方处理。
			return
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			emitUsage()
			events <- channel.StreamEvent{Kind: "done", FinishReason: stopReason}
			return
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			// 单帧解析失败不代表整个流坏了；跳过比中断更合理，
			// 因为已吐给客户端的内容无法收回。
			continue
		}

		// 载荷里的 type 比 event: 行可信：它在每个事件里都有，
		// 而 event: 行可能被省略。
		kind, _ := raw["type"].(string)
		if kind == "" {
			kind = eventName
		}

		switch kind {
		case "error":
			events <- channel.StreamEvent{
				Kind: "error",
				Err: &channel.ErrUpstream{
					StatusCode:  status,
					GotResponse: true,
					Body:        errorTextFromMap(raw),
					// 流已经开始，报错之后没有重试的余地——
					// 重试意味着客户端会看到两段拼接的回答。
					Retryable: false,
				},
			}
			return

		case "message_start":
			if msg, ok := raw["message"].(map[string]any); ok {
				readUsage(msg["usage"], &inputTokens, &outputTokens, &cacheRead, &cacheWrite)
			}
			// 输入的用量在这里就确定了，**先单独发一次**：我们对外发的第一帧
			// （Anthropic 的 message_start）里填的就是它，而等到流末尾那个完整
			// 用量才发的话，那一帧只能填本地估算——估算偏低，而只读第一帧的
			// 客户端会把它当成真的（见 internal/api 的 writeStart）。
			//
			// 走的是另一个 Kind，不是 "usage"：它只有输入、没有输出，被当成
			// 完整的用量会把输出算成 0（见 channel.StreamEvent 的 Usage）。
			//
			// 判据是 inputTokens > 0：readUsage 只在字段存在且为正时才覆盖，
			// 所以 0 的含义是「上游这一帧没报输入量」——那时不发，让调用方
			// 回落到本地估算，而不是拿一个 0 当真值。
			if inputTokens > 0 {
				u := usageOf()
				events <- channel.StreamEvent{Kind: "usage_input", Usage: &u}
			}

		case "content_block_delta":
			if delta, ok := raw["delta"].(map[string]any); ok {
				switch t, _ := delta["type"].(string); t {
				case "text_delta":
					if text, _ := delta["text"].(string); text != "" {
						events <- channel.StreamEvent{Kind: "delta", Text: text}
					}
				case "input_json_delta":
					// 工具参数是**分片吐出**的 JSON 字符串片段。
					// 中途解析必然失败——那时 JSON 还不完整，交给调用方拼接。
					idx := 0
					if v, ok := raw["index"].(float64); ok {
						idx = int(v)
					}
					partial, _ := delta["partial_json"].(string)
					events <- channel.StreamEvent{
						Kind:     "tool_use",
						ToolCall: &channel.ToolCallDelta{Index: idx, Arguments: partial},
					}
				case "thinking_delta":
					// 思考内容不进正文事件：它是模型的中间产物，
					// 与用户可见的回答是两件事。需要它的一方读 Raw。
				}
			}

		case "content_block_start":
			// tool_use 块的名字与 id 在这里给出，参数要到后面的 delta 里才来。
			if block, ok := raw["content_block"].(map[string]any); ok {
				if t, _ := block["type"].(string); t == "tool_use" {
					idx := 0
					if v, ok := raw["index"].(float64); ok {
						idx = int(v)
					}
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					events <- channel.StreamEvent{
						Kind:     "tool_use",
						ToolCall: &channel.ToolCallDelta{Index: idx, ID: id, Name: name},
					}
				}
			}

		case "message_delta":
			if delta, ok := raw["delta"].(map[string]any); ok {
				if sr, _ := delta["stop_reason"].(string); sr != "" {
					stopReason = sr
				}
			}
			readUsage(raw["usage"], &inputTokens, &outputTokens, &cacheRead, &cacheWrite)

		case "message_stop":
			emitUsage()
			events <- channel.StreamEvent{Kind: "done", FinishReason: stopReason}
			return

		case "ping", "content_block_stop":
			// 心跳与块结束标记，对统一事件流没有对应物。
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		events <- channel.StreamEvent{
			Kind: "error",
			Err: &channel.ErrUpstream{
				StatusCode:  status,
				GotResponse: true,
				Err:         err,
				Retryable:   false,
			},
		}
		return
	}

	// 连接直接关闭而没有 message_stop：不补 done 事件。
	// 伪造正常结束会让「流被截断」看起来像「模型答完了」。
}

// readUsage 把上游的 usage 对象读进累计值。
//
// 用「取到就覆盖、取不到就不动」而不是直接赋值：输入在 message_start 给出，
// 输出在 message_delta 给出，两次调用的字段互不重叠，但**后一次可能带一个
// 只含 output_tokens 的对象**——直接赋值会把已经读到的输入清零。
func readUsage(raw any, input, output, cacheRead, cacheWrite *int64) {
	m, ok := raw.(map[string]any)
	if !ok || m == nil {
		return
	}
	if v := numberFrom(m["input_tokens"]); v > 0 {
		*input = v
	}
	if v := numberFrom(m["output_tokens"]); v > 0 {
		*output = v
	}
	if v := numberFrom(m["cache_read_input_tokens"]); v > 0 {
		*cacheRead = v
	}
	if v := numberFrom(m["cache_creation_input_tokens"]); v > 0 {
		*cacheWrite = v
	}
}

func numberFrom(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func errorTextFromMap(raw map[string]any) string {
	if e, ok := raw["error"].(map[string]any); ok {
		if msg, _ := e["message"].(string); msg != "" {
			return msg
		}
		if t, _ := e["type"].(string); t != "" {
			return t
		}
	}
	if msg, _ := raw["message"].(string); msg != "" {
		return msg
	}
	return "上游返回错误"
}

// 编译期断言。
var _ channel.Adapter = (*Adapter)(nil)
