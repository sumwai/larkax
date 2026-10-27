package openaiadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/relay"
)

// pump 读取 SSE 流并翻译成统一事件。
//
// 这里是「各家兼容但不完全一致」体现得最明显的地方，所以解析按
// 「缺了怎么办」来写：
//
//   - usage 可能只在最后一个分片里出现，也可能完全不出现
//   - 结束标记可能是 [DONE]，也可能只是连接关闭
//   - 分片里可能有 choices 为空的「心跳」帧（带 usage 的那种就是）
//   - 上游可能在流中途插入一个 error 对象
//
// 因此本函数不假设任何字段一定存在，而是逐帧判断。
//
// status 是这条流对应响应的状态码（调用方已经过了 CheckStatus，所以它是
// 2xx）。流里的错误都发生在**响应头之后**，所以它们都是「拿到过响应」的
// 失败——把它带进错误里，下游才不用猜。
//
// completionIncludesReasoning 是渠道声明的用量口径（见
// channel.UsageCompletionIncludesReasoning），原样交给 relay.UsageFromRelaykit。
//
// native 表示这次走了同协议直通：每攒完一个 SSE 块，先把这一块的原始字节
// 发成一条 native 事件，再逐行解析出归一化事件。归一化事件照旧一条不少——
// 下游的中途估算与快照正文都靠它们。
func (a *Adapter) pump(ctx context.Context, status int, completionIncludesReasoning, native bool, body io.Reader, events chan<- channel.StreamEvent) {
	scanner := bufio.NewScanner(body)
	// 单个分片可能很大（长文本的一次性增量、或 base64 图片）。
	// 默认的 64KB 上限会让合法的响应被判为「读取失败」。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		// finishReason 记下上游给的结束原因，但**不在这里就结束**。
		//
		// OpenAI 的流式顺序是：finish_reason 分片 → usage 分片（choices 为空）
		// → [DONE]。一旦在 finish_reason 处 return，后面那个带 usage 的分片
		// 就永远读不到，结果是「流式请求全部没有用量」——而用量是算钱的依据。
		finishReason string
		// block 是正在攒的一个 SSE 块：空行之间的全部原始行。
		block []string
	)

	// parseBlock 把块内每一行解析成归一化事件。返回 false 表示这一块里出现了
	// 要求结束整条流的内容（[DONE] 或上游塞进来的 error 对象），pump 应当
	// 直接返回。
	//
	// 调用它的地方必须先把这一块发成 native 帧：调用方按接收顺序拼接，
	// native 帧要排在由它解析出的归一化事件之前。
	parseBlock := func(lines []string) bool {
		for _, raw := range lines {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, ":") {
				// 空行是帧分隔；以冒号开头是 SSE 注释（心跳）。
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				// event: / id: / retry: 这些字段对本项目无用，跳过。
				continue
			}

			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				// 正常结束。计费层需要知道「流结束了」，而不是靠超时去猜；
				// 结束原因在这里一并给出。
				//
				// 这一块已经作为 native 帧发过了（见 finishBlock），所以
				// 调用方拿到的是上游原样的 `[DONE]`，这里只补归一化事件。
				events <- channel.StreamEvent{Kind: "done", FinishReason: finishReason}
				return false
			}

			var chunk openAIStreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				// 单帧解析失败不代表整个流坏了；跳过它比中断整条流更合理，
				// 因为已经吐给客户端的内容无法收回。空载荷的 `data:` 行也
				// 落到这里——它没有可解析的内容。
				continue
			}

			if chunk.Error != nil {
				events <- channel.StreamEvent{
					Kind: "error",
					Err: &channel.ErrUpstream{
						StatusCode:  status,
						GotResponse: true,
						Body:        chunk.Error.Message,
						// 流已经开始了，报错之后没有「重试」的余地——
						// 重试意味着客户端会看到两段拼接的回答。
						Retryable: false,
					},
				}
				return false
			}

			if chunk.Usage != nil {
				usage := relay.UsageFromRelaykit(chunk.Usage.toRelaykit(), completionIncludesReasoning)
				events <- channel.StreamEvent{Kind: "usage", Usage: &usage}
			}

			for _, choice := range chunk.Choices {
				if choice.Delta.Content != "" {
					events <- channel.StreamEvent{Kind: "delta", Text: choice.Delta.Content}
				}
				for _, tc := range choice.Delta.ToolCalls {
					events <- channel.StreamEvent{
						Kind: "tool_use",
						ToolCall: &channel.ToolCallDelta{
							Index:     tc.Index,
							ID:        tc.ID,
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					}
				}
				if choice.FinishReason != "" {
					// 只记下来，不在这里结束：usage 分片还在后面。
					finishReason = choice.FinishReason
				}
			}
		}
		return true
	}

	// finishBlock 处理攒好的一块：先发它的 native 帧，再逐行解析。
	// 返回 false 表示 pump 应当直接返回。
	//
	// 发的是**块的原始字节**，不是解析出来的内容：块内可能有多行 data:
	// （SSE 允许一条消息由多行拼成）、也可能有注释行与 event: 行，逐行发
	// 会把这些结构拆散，调用方就拼不回上游原本的那一块。块内各行用 \n
	// 连接，末尾不带 \n——末尾那个空行是块分隔，不属于块本身。
	//
	// 只有非空块才发：连续空行之间没有内容，发出去调用方会收到空帧。
	finishBlock := func() bool {
		lines := block
		block = nil
		if len(lines) > 0 && native {
			events <- channel.StreamEvent{
				Kind:         "native",
				NativeFormat: channel.ProtocolOpenAI,
				Frame:        []byte(strings.Join(lines, "\n")),
			}
		}
		return parseBlock(lines)
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			// 客户端断开或上层取消：已经产生的费用照记，由调用方处理。
			// 这里不把攒着的块发出去：收件人已经走了，而 events 无缓冲，
			// 对方不再读时这一发会把收尾卡死。
			return
		}

		raw := scanner.Text()
		if strings.TrimSpace(raw) == "" {
			// 空行是块分隔，一块攒完了。先发它的 native 帧再解析。
			if !finishBlock() {
				return
			}
			continue
		}
		block = append(block, raw)
	}

	// 最后一块可能没有空行收尾（上游直接关掉连接、或 [DONE] 之后就没有
	// 空行）。照样先发帧再解析：不在这里发，那一块会连同它的结束语义一起
	// 凭空丢掉。
	if !finishBlock() {
		return
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		// 读取失败：可能是上游断开、也可能是分片超限。
		// 两者对调用方是同一件事——流没有正常结束。
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

	// 连接直接关闭而没有 [DONE]：不补 done 事件。
	// 缺少结束标记是「上游行为不符合协议」，如实反映比伪造一个正常结束更好——
	// 伪造会让「流被截断」看起来像「模型答完了」。
	_ = finishReason
}
