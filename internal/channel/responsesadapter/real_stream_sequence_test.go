package responsesadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
)

// 这个文件是 issue #395 要的那份夹具：**真实上游一次流式响应的完整帧序列**。
//
// 为什么要有它：#382 的根因是**夹具按文档造**。适配器认的思考事件名是从 OpenAI
// 官方 openapi 的 ResponseStreamEvent 表里读来的，而真实上游发的是
// `response.reasoning.delta` / `response.reasoning.done`——**这两个名字不在那张表里**，
// 于是落进无匹配分支被静默忽略。文档没有的名字，按文档造的夹具永远造不出来。
//
// 所以这份夹具不按文档造：它逐帧抄自一次对真实上游的 Responses 流式请求
// （出思考的模型），只把正文、id 与模型名换成占位符。事件名本身是协议事实
//（#382 的代码里已经有了），原样保留。
//
// 今天各用例只喂自己关心的那几帧，于是「顺序」与「重复出现」这两个性质没有被钉住
// ——`output_item.added` / `.done` 各出现两次（思考项一次、消息项一次），而正文只由
// `output_text.delta` 给一次。这份夹具把整条流一次喂进去。
//
// 三件事要一起看：
//
//   - realUpstreamStreamFrames —— 夹具本身，20 帧，顺序即真实顺序；
//   - TestRealStreamFixtureMatchesObservedSequence —— 夹具与下面那份事件名清单必须
//     逐帧对上（改了顺序、删了一帧、event: 行与载荷的 type 写岔了都要红）；
//   - TestChatStreamRealUpstreamSequence —— 整条流喂进适配器，正文/用量/处置逐个钉住。
//
// 夹具里**不含**真实上游的域名、账号、模型名、套餐与请求 id：模型名是
// model-placeholder，各 id 是 *_placeholder。

// realUpstreamStreamSequence 是那次真实流式响应里事件名的完整有序序列。
//
// 20 项，重复照记（`response.reasoning.delta` 8 次、`output_item.*` 各 2 次）。
// 思考增量的**条数**逐次不同（同一次上游调用换个问题就可能少几条），但事件名的
// 序列与顺序是稳定的——这份清单记的是后者。
var realUpstreamStreamSequence = []string{
	"response.created",
	"response.in_progress",
	"response.output_item.added", // 思考项开始
	"response.reasoning.delta",   // 以下 8 条
	"response.reasoning.delta",
	"response.reasoning.delta",
	"response.reasoning.delta",
	"response.reasoning.delta",
	"response.reasoning.delta",
	"response.reasoning.delta",
	"response.reasoning.delta",
	"response.reasoning.done",
	"response.output_item.done",  // 思考项结束
	"response.output_item.added", // 消息项开始
	"response.content_part.added",
	"response.output_text.delta", // 正文只在这里给一次
	"response.output_text.done",
	"response.content_part.done",
	"response.output_item.done", // 消息项结束
	"response.completed",        // 终态，用量跟着它一起来
}

// realUpstreamStreamFrames 是同一份样本的原始帧，每帧一行 `event:` 加一行 `data:`。
//
// 真实上游两个都发：`event:` 行给事件名，载荷的 `type` 字段再给一遍。适配器只读载荷
// 里那个（见 pump：`event:` 行被跳过），但夹具照抄两行——真形状就是这样，而且
// TestRealStreamFixtureMatchesObservedSequence 顺带钉住「两个说法一致」。
//
// 载荷字段裁剪到适配器会读的那些，外加少数用来证明「多余字段被忽略」的：真实上游
// 还在 response 对象上发另外二十来个顶层字段（temperature / top_p / service_tier /
// metadata / safety_identifier 一类），本适配器一个都不读，抄进来只是噪音。
var realUpstreamStreamFrames = []string{
	`event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_placeholder","object":"response","created_at":1700000000,"status":"in_progress","model":"model-placeholder","output":[],"error":null,"incomplete_details":null,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"tools":[],"tool_choice":"auto","parallel_tool_calls":false}}`,

	`event: response.in_progress
data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_placeholder","object":"response","created_at":1700000000,"status":"in_progress","model":"model-placeholder","output":[],"error":null,"incomplete_details":null,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"tools":[],"tool_choice":"auto","parallel_tool_calls":false}}`,

	`event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"reasoning","id":"rs_placeholder","summary":[]}}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":3,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考1"}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":4,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考2"}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":5,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考3"}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":6,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考4"}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":7,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考5"}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":8,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考6"}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":9,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考7"}`,

	`event: response.reasoning.delta
data: {"type":"response.reasoning.delta","sequence_number":10,"item_id":"rs_placeholder","output_index":0,"content_index":0,"delta":"思考8"}`,

	`event: response.reasoning.done
data: {"type":"response.reasoning.done","sequence_number":11,"item_id":"rs_placeholder","output_index":0,"content_index":0,"text":"思考1思考2思考3思考4思考5思考6思考7思考8"}`,

	`event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":12,"output_index":0,"item":{"type":"reasoning","id":"rs_placeholder","summary":[],"content":[{"type":"reasoning_text","text":"思考1思考2思考3思考4思考5思考6思考7思考8"}]}}`,

	`event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":13,"output_index":1,"item":{"type":"message","id":"msg_placeholder","status":"in_progress","role":"assistant","content":[{"type":"output_text","text":"","annotations":[],"logprobs":[]}]}}`,

	`event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":14,"item_id":"msg_placeholder","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[],"logprobs":[]}}`,

	`event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":15,"item_id":"msg_placeholder","output_index":1,"content_index":0,"delta":"你好","logprobs":[]}`,

	`event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":16,"item_id":"msg_placeholder","output_index":1,"content_index":0,"text":"你好","logprobs":[]}`,

	`event: response.content_part.done
data: {"type":"response.content_part.done","sequence_number":17,"item_id":"msg_placeholder","output_index":1,"content_index":0,"part":{"type":"output_text","text":"你好","annotations":[],"logprobs":[]}}`,

	`event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":18,"output_index":1,"item":{"type":"message","id":"msg_placeholder","status":"completed","role":"assistant","content":[{"type":"output_text","text":"你好","annotations":[],"logprobs":[]}]}}`,

	// 终态。注意两处真形状：
	//   - 用量**只在**这里给（与 chat 不同，不需要 stream_options 那样的 opt-in）；
	//   - input_tokens_details 里**只有** cached_tokens，没有 cache_write_tokens；
	//   - output[] 里思考项排在消息项**之前**——流式这一支与非流式那一支的顺序相反
	//     （非流式见 TestChatRealShapeReasoningAfterMessage）。
	`event: response.completed
data: {"type":"response.completed","sequence_number":19,"response":{"id":"resp_placeholder","object":"response","created_at":1700000000,"status":"completed","model":"model-placeholder","output":[{"type":"reasoning","id":"rs_placeholder","summary":[],"content":[{"type":"reasoning_text","text":"思考1思考2思考3思考4思考5思考6思考7思考8"}]},{"type":"message","id":"msg_placeholder","status":"completed","role":"assistant","content":[{"type":"output_text","text":"你好","annotations":[]}]}],"error":null,"incomplete_details":null,"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":25}},"tools":[],"tool_choice":"auto","parallel_tool_calls":false}}`,
}

// frameNames 从一帧里取两个事件名：`event:` 行的与载荷 `type` 字段的。
//
// 两个都要，而且必须相等：真实上游两个都发，适配器只读载荷里那个。只钉一个的话，
// 「有人把 event: 行改错」或者「有人把载荷的 type 改错」这两种改法里总有一种漏网，
// 而漏掉的那一种恰恰会让夹具不再像真实上游。
func frameNames(t *testing.T, frame string) (eventLine, payloadType string) {
	t.Helper()
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			eventLine = rest
		}
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			var ev struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(rest), &ev); err != nil {
				t.Fatalf("载荷不是 JSON: %v（%s）", err, rest)
			}
			payloadType = ev.Type
		}
	}
	if eventLine == "" || payloadType == "" {
		t.Fatalf("这一帧缺 event: 行或 data: 行：%q", frame)
	}
	return eventLine, payloadType
}

// 夹具本身也要被钉住。没有这条用例，realUpstreamStreamSequence 只是一份注释：
// 谁把帧顺序调一下、删一帧、或者只改 event: 行不改载荷，整包测试照样全绿，而夹具
// 已经不是真实上游的样子了——那正是 #382 要修的那种「看起来在测、其实没测」。
func TestRealStreamFixtureMatchesObservedSequence(t *testing.T) {
	if len(realUpstreamStreamFrames) != len(realUpstreamStreamSequence) {
		t.Fatalf("夹具 %d 帧，事件名清单 %d 项", len(realUpstreamStreamFrames), len(realUpstreamStreamSequence))
	}
	for i, frame := range realUpstreamStreamFrames {
		eventLine, payloadType := frameNames(t, frame)
		if eventLine != realUpstreamStreamSequence[i] {
			t.Errorf("第 %d 帧的 event: 行 = %q，清单里是 %q", i+1, eventLine, realUpstreamStreamSequence[i])
		}
		if payloadType != realUpstreamStreamSequence[i] {
			t.Errorf("第 %d 帧载荷的 type = %q，event: 行是 %q", i+1, payloadType, eventLine)
		}
	}
}

// streamDisposition 是一个事件名在适配器里的处置。四种，判据见各自的注释。
type streamDisposition int

const (
	// streamForwarded：转成了发给客户端的事件。由正文与用量断言钉住——
	// 删掉对应的 case，正文变空或用量缺失，用例立刻红。
	streamForwarded streamDisposition = iota

	// streamIgnoredKnown：落进无匹配分支、命中 ignorableResponsesEvents，记一条
	// debug。把名字从表里删掉，它就变成「未归类」的 warn，下面那条「零 warn」红。
	streamIgnoredKnown

	// streamIgnoredReasoning：命中思考类 case，记一条 debug。删掉那一支，两个名字
	// 就落进无匹配分支，而它们**不在** ignorableResponsesEvents 里（见那张表的注释），
	// 于是记 warn——「零 warn」红。这正是 #382 的形状。
	streamIgnoredReasoning

	// streamHandledSilently：switch 里认下了，但对这条流没有客户端可见效果，也不记
	// 日志。只有 output_item.added / .done 落在这一类：这条流里两个输出项都是非工具项，
	// 而这两支只对工具项与「上游一个正文增量都不发」的兜底做事。
	//
	// 它对这条流没有可见效果，但**不是不可观察的**：这两个名字不在
	// ignorableResponsesEvents 里，所以删掉 case 之后它们会落进无匹配分支记一条
	// warn——下面那条「零 warn」因此会红（实测过）。这一支真正管的行为（工具项、
	// 上游一个正文增量都不发时的兜底）另有用例，见
	// TestChatStreamToolCallIndexStartsAtZero 与 TestChatStreamTextOnlyInOutputItems。
	streamHandledSilently
)

// realStreamDisposition 是这份样本里每个事件名的处置。
//
// 有名字不在这张表里 → 用例报「没有登记处置」：新加进清单的事件必须先想清楚它归哪一类，
// 而「想不清楚」本身就是要红的那种情况。
var realStreamDisposition = map[string]streamDisposition{
	"response.created":            streamIgnoredKnown,
	"response.in_progress":        streamIgnoredKnown,
	"response.output_item.added":  streamHandledSilently,
	"response.reasoning.delta":    streamIgnoredReasoning,
	"response.reasoning.done":     streamIgnoredReasoning,
	"response.output_item.done":   streamHandledSilently,
	"response.content_part.added": streamIgnoredKnown,
	"response.output_text.delta":  streamForwarded,
	"response.output_text.done":   streamIgnoredKnown,
	"response.content_part.done":  streamIgnoredKnown,
	"response.completed":          streamForwarded,
}

// 真实上游的整条流一次喂进适配器：正文拼得对、用量取得对、**一条 warn 都没有**。
//
// 「零 warn」是这条用例真正的判据，也是它区别于「喂进去不报错」的地方。无匹配分支对
// 每个走到它上面的事件都会留痕——认得的记 debug、未归类的记 warn（见 pump 里的
// noteOnce）——所以「零 warn」等价于「这条流里没有一个事件是适配器没归类的」。
// 少了它，这份夹具只是又一份「跑通不报错」的样本，而 #382 的失效方式（一个没归类的
// 名字被静默忽略）在「跑通不报错」下完全看不出来。
func TestChatStreamRealUpstreamSequence(t *testing.T) {
	logs := captureLogs(t)

	srv := sseServer(t, realUpstreamStreamFrames...)
	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "model-placeholder", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	var usage *channel.Usage
	finish := ""
	dones := 0
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "usage":
			usage = ev.Usage
		case "done":
			dones++
			finish = ev.FinishReason
		case "error":
			t.Fatalf("这条流不该出错：%v", ev.Err)
		}
	}

	// 8 条思考增量一条都不许进正文。这条断言在「思考那一支被删掉」时**不会**变红
	//（无匹配分支同样不转发内容），所以它钉的是「思考不进正文」这个产品判断，
	// 而「那一支还在不在」由下面的处置断言与「零 warn」钉。
	if text.String() != "你好" {
		t.Errorf("拼出的正文 = %q，期望 %q——8 条思考增量不能进正文", text.String(), "你好")
	}
	if dones != 1 {
		t.Errorf("done 事件发了 %d 次，期望 1 次（终态只该结束一次）", dones)
	}
	if finish != "stop" {
		t.Errorf("FinishReason = %q，期望 stop", finish)
	}

	// 用量只在终态事件里给，所以它取不到就等于这次请求没得结算。
	if usage == nil {
		t.Fatal("终态事件带的用量要发出来：它是结算依据")
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 40 {
		t.Errorf("用量 = %+v，期望 prompt 100、completion 40（上游原值）", usage)
	}
	if usage.CacheReadTokens != 30 || usage.ReasoningTokens != 25 {
		t.Errorf("用量 = %+v，期望缓存读 30、推理 25", usage)
	}
	if usage.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d：上游的 input_tokens_details 里没有这个字段，落到这里就该是 0", usage.CacheWriteTokens)
	}
	if !usage.CompletionIncludesReasoning {
		t.Error("Responses 口径里 completion 含 reasoning，这一位没声明的话推理那批 token 会被收两次钱")
	}

	// 协议口径 → 计费口径。100 含 30 缓存读，所以普通输入是 70；40 含 25 推理，
	// 所以可见输出是 15。这两步减法错了响应形状完全正常，只有金额不对。
	got := billing.FromChannelUsage(*usage)
	if got.PromptTokens != 70 || got.CompletionTokens != 15 ||
		got.CacheReadTokens != 30 || got.CacheWriteTokens != 0 || got.ReasoningTokens != 25 {
		t.Errorf("计费口径 = %+v，期望普通输入 70、输出 15、缓存读 30、缓存写 0、推理 25", got)
	}

	// 逐帧钉住处置。
	for _, name := range realUpstreamStreamSequence {
		switch realStreamDisposition[name] {
		case streamForwarded:
			// 客户端可见，由上面那些断言钉住。
		case streamIgnoredKnown:
			if !hasLogLine(logs.String(), "跳过已知且不必转发的流式事件", name) {
				t.Errorf("%s 应当记一条「已知且不必转发」的 debug：把它从 ignorableResponsesEvents 里删掉，它就成了未归类", name)
			}
		case streamIgnoredReasoning:
			if !hasLogLine(logs.String(), "跳过思考类流式事件（不进正文）", name) {
				t.Errorf("%s 应当记一条「跳过思考类」的 debug：这一支是 #382 补的，删了它就落回无匹配分支", name)
			}
		case streamHandledSilently:
			// 这条流里没有可见效果，也不记日志；它还在不在由下面那条「零 warn」钉住。
		default:
			t.Errorf("%s 没有登记处置：新加进样本的事件要先想清楚它归哪一类", name)
		}
	}

	if strings.Contains(logs.String(), "未归类") {
		t.Errorf("这条流里出现了未归类的事件——真实上游发的名字一个都不该漏在分类之外：\n%s", logs.String())
	}

	// 反向：日志里不该出现清单之外的事件名。正向那条只能证明「该记的都记了」，
	// 这条证明「没多记」——例如有人把某个名字挪进表里、或加了一支不该有的 case。
	for _, line := range strings.Split(logs.String(), "\n") {
		i := strings.Index(line, "event=")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[i+len("event="):])
		if _, ok := realStreamDisposition[name]; !ok {
			t.Errorf("日志里出现了样本之外的事件名 %q：%s", name, line)
		}
	}
}
