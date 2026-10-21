package responsesadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/relay"
)

// Responses 的流式是一串**带名字的事件**，而不是「每片一个 choices」：
//
//	response.created / response.in_progress    → 流开始，正文还没有
//	response.output_item.added                 → 一个输出项开始（message / reasoning / function_call）
//	response.output_text.delta                 → 正文增量
//	response.reasoning_summary_text.delta      → 思考摘要增量
//	response.reasoning.delta                   → 思考增量（上游在用的另一个名字）
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
	// Annotation 是引用标注本身，只在 output_text.annotation.added 里给。
	// 用 RawMessage 接住再解成一串字段：标注的字段集由上游决定（type 之外
	// 各家还会加 url / start_index / end_index 等），而我们的表示也是原样的
	// map（见 channel.StreamEvent.Annotations）。
	Annotation json.RawMessage `json:"annotation"`
}

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
	// 单个事件可能很大：长文本的一次性增量、或一整段工具参数。
	// 默认的 64KB 上限会把合法响应判为读取失败。
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		// finishReason 记下终态给的结束原因，与 done 一起发出。
		finishReason string
		// tools 分配工具调用的下标，并记住谁的参数已经发过。
		tools = newToolIndexer()
		// textDeltaSeen 表示上游发过正文增量，补发那条兜底因此不再需要。
		textDeltaSeen bool
		// textSent 记住哪些输出项的正文已经发过，键是它在 output[] 里的下标。
		//
		// 为什么按项记而不是一个全局布尔：有的上游一个增量都不发，正文只在
		// output_item.done 或终态事件的 response.output 里给。一个全局布尔
		// 会让「一个响应里有多个 message 项」时后面的项被静默丢掉——而这条
		// 兜底要修的正是静默丢内容。
		//
		// 为什么用下标而不是 item id：id 在有的兼容层里缺，而 output_index
		// 是这类事件的必填字段，两处都能拿到同一个数。
		textSent = make(map[int]bool)
	)

	// log 是本请求的日志器：进程级日志器加上 context 里的请求标识。
	// 取一次即可——FromContext 每次都会按 context 重建一个带字段的日志器。
	log := observe.FromContext(ctx)

	// noted 记住这条流里已经记过日志的事件名。
	//
	// 有的上游每个 token 发一个同类事件（思考增量就是这样），逐条打会把日志
	// 淹掉，而这一行要说明的只是「这条流里出现过这个事件」。同类只记一次。
	noted := make(map[string]bool)

	// noteOnce 与 warnOnce 记一次「上游发了一个我们不转发的事件」，同类只记
	// 一次。三处会走到它们上面：思考那一支、无匹配分支里的已知事件、无匹配分支
	// 里的未归类事件——**三处各带一句自己的话**，因为它们是三件事，而排查的人
	// 与写用例的人都要能把它们分开（见 TestChatStreamReasoningEventsAreRecognized
	// 与 TestChatStreamUnknownEventIsLoggedAndIgnored）。引用标注那一支另有
	// 一种用法：标注解析失败时记一条 warn（见 response.output_text.annotation.added）。
	//
	// 为什么无匹配分支必须留下痕迹：静默忽略最坏的后果是上游把正文放进一个我们
	// 不认识的事件名里，我们一字不发、也一字不记——reasoning.delta 就是这种形状，
	// 只是那一次它恰好不含正文（见下面那个 case）。
	//
	// 已知与未归类分开记：已知的（ignorableResponsesEvents）记 debug，它是正常流
	// 的一部分，默认级别下不该出现；未归类的记 warn，它可能是上游新增的事件，也
	// 可能含正文，而「正文被静默丢掉」正是这个包最怕的失效方式。
	//
	// 两者都**不中断流**，也都不转发它的 delta：不认识的 delta 里可能是思考
	// 内容，转发等于把中间产物当正文发给客户端——那是另一类、更贵的错。而中断
	// 流更坏：已经吐出去的正文收不回来，一个陌生的事件名还会让上层把这次调用
	// 当成「上游坏了」去换渠道重试，于是客户端收到两段拼接的回答。
	//
	// 只记事件名、不记载荷：载荷里是模型的输出（可能就是用户的正文），而这里
	// 要回答的问题只是「哪个事件没被处理」。
	noteOnce := func(eventType, msg string) {
		if noted[eventType] {
			return
		}
		noted[eventType] = true
		log.Debug(msg, "event", eventType)
	}
	warnOnce := func(eventType, msg string) {
		if noted[eventType] {
			return
		}
		noted[eventType] = true
		log.Warn(msg, "event", eventType)
	}

	// sendText 在没发过增量、且这一项还没发过时，把它的正文补成一个 delta。
	//
	// 判据与 relaykit 的 terminalOutputChunks 同源（hadSentText 为假时才从
	// response.output 取正文）：增量已经把正文给全了，再补一遍客户端会看到
	// 两遍，而「看到两遍」比「看到空回答」更容易被当成模型的问题。
	sendText := func(index int, item *responsesOutput) {
		if item == nil || item.Type != "message" || textDeltaSeen || textSent[index] {
			return
		}
		text := outputText([]responsesOutput{*item})
		if text == "" {
			return
		}
		textSent[index] = true
		events <- channel.StreamEvent{Kind: "delta", Text: text}
	}

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
				textDeltaSeen = true
				events <- channel.StreamEvent{Kind: "delta", Text: ev.Delta}
			}

		case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
			"response.reasoning_text.delta", "response.reasoning_text.done",
			// 上游在用的另外两个名字。它们不在 OpenAI 的 Responses 事件表里
			// （表里是上面四个），但实测会发——落到无匹配分支时行为上「碰巧
			// 对」，而碰巧对不是分支在管：哪天有人把无匹配分支改成「未知事件
			// 即报错」，这条流立刻就断。
			"response.reasoning.delta", "response.reasoning.done":
			// 思考内容不进正文事件：它是模型的中间产物，与用户可见的回答
			// 是两件事（与 chat 的 reasoning_content、Anthropic 的
			// thinking_delta 同一处置）。需要它的一方读终态事件里的
			// response.output——那里有完整的摘要。
			//
			// 这一支与无匹配分支都不转发内容，**行为上分不出来**，所以这里
			// 也记一条 debug：认得的事件与未归类的事件在日志里必须能分开
			//（见 noteOnce）。
			noteOnce(ev.Type, "跳过思考类流式事件（不进正文）")

		case "response.output_text.annotation.added":
			// 引用标注。非流式那条路把它放进 output[].content[].annotations
			// 交给调用方，这里同样要转出去——两条路口径不一致本身就是缺陷
			//（见 internal/relay 的 fromResponsesResponse 与
			// internal/api 的 responsesStream）。
			//
			// 解析失败不中断流：一条标注坏掉不该让已经吐出的正文作废，
			// 而丢弃它是静默的——所以记一条 warn，与无匹配分支同一个形状。
			if annotation, ok := ev.annotation(); ok {
				events <- channel.StreamEvent{Kind: "annotation", Annotations: []map[string]any{annotation}}
			} else if len(ev.Annotation) > 0 {
				warnOnce(ev.Type, "引用标注解析失败，内容已忽略")
			}

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
			// 兜底一：有的上游一个参数增量都不发，把完整参数放在这一项里。
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
			// 兜底二：正文同理。有的上游只发 output_item.added / .done，
			// 一个 response.output_text.delta 都不发——那时客户端拿到空回答，
			// 而用量按实测结算，用户为空回答付钱。工具参数有这道兜底而正文
			// 没有，是同一份代码里两处不一致。
			//
			// 没有 output_index 就不发：下标是「这一项发过没有」的键，缺了它
			// 就没法保证终态那一次不重复发。终态按 output[] 的位置遍历，
			// 那一支会接住它。
			if ev.OutputIndex != nil {
				sendText(*ev.OutputIndex, ev.Item)
			}

		case "response.completed", "response.incomplete", "response.done":
			// 终态。用量**只在**这里给，所以它决定这次结算的依据。
			if ev.Response != nil {
				// 兜底：正文只在 response.output 里给（一个增量都没有）时补发。
				// 放在 usage 之前，让调用方先拿到正文再看到用量。
				for i := range ev.Response.Output {
					sendText(i, &ev.Response.Output[i])
				}
				if ev.Response.Usage != nil {
					u := relay.UsageFromRelaykit(ev.Response.Usage.toRelaykit(), completionIncludesReasoning)
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
					StatusCode:  status,
					GotResponse: true,
					Body:        ev.errorText(),
					// 流已经开始，报错之后没有重试的余地——重试意味着客户端
					// 会看到两段拼接的回答。已经吐出的部分照记费用。
					Retryable: false,
				},
			}
			return

		default:
			// 本适配器没有分类的事件：可能是新协议里新增的事件，也可能含
			// 正文。记下来，但既不转发也不中断流（理由见 noteOnce）。
			if _, known := ignorableResponsesEvents[ev.Type]; known {
				noteOnce(ev.Type, "跳过已知且不必转发的流式事件")
			} else {
				warnOnce(ev.Type, "流里出现未归类的事件，内容已忽略")
			}
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		// 读取失败：可能是上游断开、也可能是单个事件超限。
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

	// 连接直接关闭而没有终态事件：不补 done，也不补 usage。
	// 缺了结束标记是「上游行为不符合协议」，如实反映比伪造一个正常结束更好——
	// 伪造会让「流被截断」看起来像「模型答完了」。
}

// ignorableResponsesEvents 是**已知、且忽略它不会丢内容**的流式事件。
//
// 判据是「忽略它会不会丢内容」，而不是「它有没有用」：这张表只决定无匹配分支
// 里那条日志的级别（见 pump 里的 noteOnce），转发与否由 switch 决定——表里的事件
// 也**不是**「本适配器支持的事件」清单。
//
// 名字照 OpenAI 的 Responses 事件表枚举（openapi 里 ResponseStreamEvent 的
// 全部成员）。进表的是两类：
//
//   - 生命周期：一次响应的开始与排队。它们不带内容。
//   - 结构：正文已经随 response.output_text.delta 给出；一个增量都不发的上游
//     由 output_item.done 与终态事件里的 response.output 兜底补发（那两条兜底
//     各有用例）。这里再发一遍会让客户端看到两遍正文，而「看到两遍」比「看到
//     空回答」更像模型的问题，更难查。
//
// 思考类的那几个名字不在表里：它们在 switch 里有自己的 case，永远走不到
// 无匹配分支。引用标注同理：output_text.annotation.added 有自己的 case，
// 它带内容，而且**要转发**（见那一支）。
//
// **故意不进表的**：refusal.*、audio.* / output_audio.*，以及内置工具的产物
// 事件（code_interpreter_call_code.*、image_generation_call.partial_image
// 一类）。它们确实带内容，只是本适配器没有转发它们的通道；把它们记成
// 「已知且不必转发」是假话，而它们走到无匹配分支会记一条 warn——那正是我们
// 希望有人看见的信号。内置工具的**进度**事件（web_search_call.in_progress
// 一类）同理：本适配器不转发内置工具，真见到它们说明有我们没预期的链路。
var ignorableResponsesEvents = map[string]bool{
	"response.created":     true,
	"response.in_progress": true,
	"response.queued":      true,

	"response.content_part.added": true,
	"response.content_part.done":  true,
	"response.output_text.done":   true,
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

// annotation 取出这条事件里的引用标注。
//
// ok 为 false 有三种情况：事件里没有 annotation 字段、它不是 JSON 对象、
// 或者它是个空对象。三者在处置上相同（没有可转发的东西）。
//
// 调用方据 len(Annotation) 只能区分「字段缺失」与「字段在」：`null` 与空
// 对象都解不出东西，都会走那条 warn 分支（见上面的 case）。那两条 warn 的
// 措辞因此不是「解析失败」的严格含义——它说的是「字段给了，但里面没有
// 可转发的内容」。
func (ev responsesStreamEvent) annotation() (map[string]any, bool) {
	if len(ev.Annotation) == 0 {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(ev.Annotation, &out); err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
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
