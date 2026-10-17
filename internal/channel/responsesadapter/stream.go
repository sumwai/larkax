package responsesadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/relay"
)

// Responses 的流式是一串**带名字的事件**，而不是「每片一个 choices」：
//
//	response.created / response.in_progress    → 流开始，正文还没有
//	response.output_item.added                 → 一个输出项开始（message / reasoning / function_call）
//	response.output_text.delta                 → 正文增量
//	response.reasoning_summary_text.delta      → 思考摘要增量
//	response.function_call_arguments.delta     → 工具参数增量
//	response.output_item.done                  → 该输出项结束
//	response.completed / response.incomplete   → 终态，**用量跟着它一起来**
//	response.failed / response.error           → 失败
//
// 与 chat 的一处重要差别：用量**无需 opt-in**（chat 要显式带
// stream_options.include_usage），它就在终态事件里。
//
// 但估算兜底仍然要有：上游中途断开时不会有终态事件，而那时请求已经发出去、
// 钱已经预留了。所以这里**不伪造** done 与 usage——缺了就让调用方按已经收到
// 的增量估算（见 internal/gateway 的估算路径）。伪造会让「流被截断」看起来
// 像「模型答完了」，而那是两件在账上完全不同的事。

// responsesStreamEvent 是流式事件里我们要用到的部分。
//
// 一个结构接住全部事件类型，而不是按 type 各声明一个：事件类型有十几种，
// 而其中我们真正要读的字段只有几个。多余的字段按「不在就是不在」处理。
type responsesStreamEvent struct {
	Type string `json:"type"`
	// Delta 是各类增量事件的内容：正文、思考摘要、工具参数的片段。
	Delta string `json:"delta"`
	// Input 是 custom 工具的自由文本入参（.delta 给片段，.done 给全文）。
	Input *string `json:"input"`
	// Arguments 是 function_call 参数的全文，只在 .done 事件里给。
	Arguments *string `json:"arguments"`
	// Item 在 output_item.added / .done 里给，是这一项本身。
	Item *responsesOutput `json:"item"`
	// Response 在终态事件里给，**用量就在它里面**。
	Response *responsesResponse `json:"response"`
	// OutputIndex 是这一项在 output[] 里的位置，ItemID 是它的 id。
	// 两者都用来把「参数增量」关联到「哪一次工具调用」上。
	OutputIndex *int            `json:"output_index"`
	ItemID      string          `json:"item_id"`
	Error       *responsesError `json:"error"`
	// Message 是兜底的错误文案：有的兼容层在 error 之外还平铺一个 message。
	Message string `json:"message"`
}

// pump 读取 SSE 流并翻译成统一事件。
func (a *Adapter) pump(ctx context.Context, body io.Reader, events chan<- channel.StreamEvent) {
	scanner := bufio.NewScanner(body)
	// 单个事件可能很大：长文本的一次性增量、或一整段工具参数。
	// 默认的 64KB 上限会把合法响应判为读取失败。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		// finishReason 记下终态给的结束原因，与 done 一起发出。
		finishReason string
		// tools 分配工具调用的下标，并记住谁的参数已经发过。
		tools = newToolIndexer()
	)

	for scanner.Scan() {
		if ctx.Err() != nil {
			// 客户端断开或上层取消：已经产生的费用照记，由调用方处理。
			return
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			// 空行是帧分隔；以冒号开头是 SSE 注释（心跳）。
			continue
		}
		if strings.HasPrefix(line, "event:") {
			// 事件名在载荷的 type 字段里，而且比 event: 行可信——它每个
			// 事件都有，event: 行有的兼容层不发。这里跳过即可。
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// id: / retry: 这些字段对本项目无用。
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			// 终态事件之后有的兼容层还补一个 [DONE]，正常路径下
			// response.completed 已经让我们返回了。走到这里说明**没有**
			// 终态事件：结束标记照发，但不伪造用量——用量按增量估算，
			// 而那是调用方的事。
			events <- channel.StreamEvent{Kind: "done", FinishReason: finishReason}
			return
		}

		var ev responsesStreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// 单帧解析失败不代表整个流坏了；跳过它比中断整条流更合理，
			// 因为已经吐给客户端的内容无法收回。
			continue
		}

		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				events <- channel.StreamEvent{Kind: "delta", Text: ev.Delta}
			}

		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			// 思考内容不进正文事件：它是模型的中间产物，与用户可见的回答
			// 是两件事（与 chat 的 reasoning_content、Anthropic 的
			// thinking_delta 同一处置）。需要它的一方读终态事件里的
			// response.output——那里有完整的摘要。

		case "response.output_item.added":
			// 工具调用的名字与 id 在这里给出，参数要到后面的增量里才来
			//（与 Anthropic 的 content_block_start 同一形状）。
			if ev.Item != nil && isToolCallItem(ev.Item.Type) {
				slot := tools.slotFor(ev.itemKey())
				events <- channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
					Index: slot.index,
					ID:    ev.Item.callID(),
					Name:  ev.Item.Name,
				}}
			}

		case "response.function_call_arguments.delta":
			if ev.Delta != "" {
				slot := tools.slotFor(ev.itemKey())
				tools.markArgsSent(slot)
				events <- channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
					Index:     slot.index,
					Arguments: ev.Delta,
				}}
			}

		case "response.function_call_arguments.done":
			// 有的上游只发这一个（参数一次性给全，没有增量）。已经发过增量
			// 就不再发一遍，否则调用方拼出来的参数会重复。
			if ev.Arguments != nil && *ev.Arguments != "" {
				slot := tools.slotFor(ev.itemKey())
				if !slot.argsSent {
					tools.markArgsSent(slot)
					events <- channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
						Index:     slot.index,
						Arguments: *ev.Arguments,
					}}
				}
			}

		case "response.custom_tool_call_input.delta":
			// custom 工具的参数是自由文本，形状与 function 的 JSON 片段不同，
			// 但对调用方是同一件事：参数的一段。
			if ev.Input != nil && *ev.Input != "" {
				slot := tools.slotFor(ev.itemKey())
				tools.markArgsSent(slot)
				events <- channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
					Index:     slot.index,
					Arguments: *ev.Input,
				}}
			}

		case "response.custom_tool_call_input.done":
			if ev.Input != nil && *ev.Input != "" {
				slot := tools.slotFor(ev.itemKey())
				if !slot.argsSent {
					tools.markArgsSent(slot)
					events <- channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
						Index:     slot.index,
						Arguments: *ev.Input,
					}}
				}
			}

		case "response.output_item.done":
			// 兜底：有的上游一个参数增量都不发，把完整参数放在这一项里。
			if ev.Item != nil && isToolCallItem(ev.Item.Type) {
				slot := tools.slotFor(ev.itemKey())
				if args := ev.Item.arguments(); args != "" && !slot.argsSent {
					tools.markArgsSent(slot)
					events <- channel.StreamEvent{Kind: "tool_use", ToolCall: &channel.ToolCallDelta{
						Index:     slot.index,
						Arguments: args,
					}}
				}
			}

		case "response.completed", "response.incomplete", "response.done":
			// 终态。用量**只在**这里给，所以它决定这次结算的依据。
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					u := relay.UsageFromRelaykit(ev.Response.Usage.toRelaykit())
					events <- channel.StreamEvent{Kind: "usage", Usage: &u}
				}
				finishReason = ev.Response.finishReason(tools.count() > 0)
			}
			events <- channel.StreamEvent{Kind: "done", FinishReason: finishReason}
			return

		case "response.failed", "response.error", "error":
			events <- channel.StreamEvent{
				Kind: "error",
				Err: &channel.ErrUpstream{
					Body: ev.errorText(),
					// 流已经开始，报错之后没有重试的余地——重试意味着客户端
					// 会看到两段拼接的回答。已经吐出的部分照记费用。
					Retryable: false,
				},
			}
			return
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		// 读取失败：可能是上游断开、也可能是单个事件超限。
		// 两者对调用方是同一件事——流没有正常结束。
		events <- channel.StreamEvent{
			Kind: "error",
			Err:  &channel.ErrUpstream{Err: err, Retryable: false},
		}
		return
	}

	// 连接直接关闭而没有终态事件：不补 done，也不补 usage。
	// 缺了结束标记是「上游行为不符合协议」，如实反映比伪造一个正常结束更好——
	// 伪造会让「流被截断」看起来像「模型答完了」。
}

// itemKey 是这次事件指的那个输出项的两个键。
//
// 上游把身份分散在两个字段里：output_item.* 给的是 item.id，
// 参数增量给的是 item_id 与 output_index。三个都可能缺，所以两个键一起带上，
// 由 toolIndexer 去关联——按其中一个键找、用另一个键记下来。
func (ev responsesStreamEvent) itemKey() (itemID string, outputIndex *int) {
	itemID = ev.ItemID
	if itemID == "" && ev.Item != nil {
		itemID = ev.Item.ID
	}
	return itemID, ev.OutputIndex
}

// errorText 取出这次错误事件的文案。
func (ev responsesStreamEvent) errorText() string {
	for _, e := range []*responsesError{ev.Error, responseErrorOf(ev.Response)} {
		if e.empty() {
			continue
		}
		if e.Message != "" {
			return e.Message
		}
		if e.Code != "" {
			return e.Code
		}
		return e.Type
	}
	if ev.Message != "" {
		return ev.Message
	}
	return "上游在流中途报错"
}

func responseErrorOf(r *responsesResponse) *responsesError {
	if r == nil {
		return nil
	}
	return r.Error
}

// toolSlot 是一次工具调用在本项目这一侧的状态。
type toolSlot struct {
	// index 是交给调用方的下标。
	index int
	// argsSent 表示这次调用的参数已经发出去过。上游可能既发增量又发全文
	//（.delta 一串 + .done 一个），两样都发出去会让参数被拼两遍。
	argsSent bool
}

// toolIndexer 给每次工具调用分配一个下标，并把上游的两种键关联到它。
//
// 下标由**我们自己**从 0 开始分配，而不是透传上游的 output_index：后者是
// 这一项在 output[] 里的位置，而 output[] 里还有 reasoning 与 message 项，
// 于是第一个函数调用常常是 1 或 2。OpenAI 的客户端契约是「tool_calls 的
// index 从 0 起、连续」，收到稀疏下标时有的客户端会建出一个带空洞的数组。
// relaykit 的 Responses→Chat 转换也是自己数的（nextToolIndex），这里与它一致。
type toolIndexer struct {
	slots []*toolSlot
	// 两个键各有一张表，指向同一个 slot。分开记是因为不同上游给哪个键
	// 并不一致，而只要在**某个**事件里同时出现过，两者就能对上。
	byItemID      map[string]*toolSlot
	byOutputIndex map[int]*toolSlot
}

func newToolIndexer() *toolIndexer {
	return &toolIndexer{
		byItemID:      make(map[string]*toolSlot),
		byOutputIndex: make(map[int]*toolSlot),
	}
}

// slotFor 找出（或新建）这次事件指的那次工具调用。
func (t *toolIndexer) slotFor(itemID string, outputIndex *int) *toolSlot {
	slot := t.lookup(itemID, outputIndex)
	if slot == nil {
		slot = &toolSlot{index: len(t.slots)}
		t.slots = append(t.slots, slot)
	}
	if itemID != "" {
		t.byItemID[itemID] = slot
	}
	if outputIndex != nil {
		t.byOutputIndex[*outputIndex] = slot
	}
	return slot
}

// lookup 按两个键找已有的调用。
func (t *toolIndexer) lookup(itemID string, outputIndex *int) *toolSlot {
	if itemID != "" {
		if slot, ok := t.byItemID[itemID]; ok {
			return slot
		}
	}
	if outputIndex != nil {
		if slot, ok := t.byOutputIndex[*outputIndex]; ok {
			return slot
		}
		return nil
	}
	if itemID == "" {
		// 两个键都没有：没法关联，接到最近一次调用上。上游总会给
		// output_index（它是这类事件的必填字段），所以这一支是给「给得更少
		// 的兼容层」留的——每次增量都新建一次调用比接到上一次更坏。
		if n := len(t.slots); n > 0 {
			return t.slots[n-1]
		}
	}
	return nil
}

func (t *toolIndexer) markArgsSent(slot *toolSlot) { slot.argsSent = true }

// count 返回已经出现过的工具调用次数，用来判断结束原因是不是 tool_calls。
func (t *toolIndexer) count() int { return len(t.slots) }
