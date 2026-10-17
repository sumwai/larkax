package assembly

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：**跨协议的流式工具调用**。假上游按 OpenAI 的帧形状吐两个交错的
// 工具调用，客户端按 Anthropic 协议读（/v1/messages，stream: true）。
//
// # 为什么另起一条
//
// 这一片是两个转换叠在一起的地方，而两处**各自都有单测**：
//
//	internal/channel/openaiadapter/stream.go  把 tool_calls[].index 原样透出
//	internal/api/stream.go                    把它映射成 Anthropic 的块下标
//
// 单测能钉住渲染器，钉不住「适配器透出来的下标与渲染器假设的下标是同一套」
// ——两个包各自绿着，缺陷长在中间那条缝里（见 LESSONS.md 第 24 条）。这条
// 用例从入口走到假上游再走回来，让那条缝暴露在同一组断言下。
//
// 既有的两组各自只覆盖了一半：cross_protocol_e2e_test.go 跨协议但全是非流式，
// stream_e2e_test.go 是流式但两条入口各自配同协议的渠道。
//
// # 断言的是帧的**形状**，不是拼出来的字符串
//
// 三件事，逐帧机械可判：
//
//  1. 每个工具调用的参数都完整：按块把 input_json_delta 拼回去逐块比对，
//     且拼出来的东西要能当 JSON 解析（半截 JSON 解析不了）。
//  2. 每个块下标只出现一次 content_block_start。
//  3. 每对 start/stop 配平：分片只落在**正开着**的块上，每个 start 恰好配一个
//     stop，没有块一直开着。
//
// 第 3 条是钉住 #291 的那一条。**前两条钉不住它**：那次缺陷的现场里，按 index
// 累加的客户端照样把参数拼得回来，重开的也只有「命中旧下标就重开」那种改法
// ——真正坏掉的是「分片落在已经配过 stop 的块上」这个形状，只有第 3 条抓得到。
// 三条一起留着，是因为它们各自还挡着别的改法（重开下标 → 第 2 条，分片归错块
// → 第 1 条），而三条都便宜。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。

// anthropicToolStreamBody 是一次**带工具定义**的 Anthropic 流式请求。
//
// tools 不能省：请求体里没有 tools 的流式请求走的是另一条（更短的）转换路径，
// 而工具调用是模型看见了工具定义才会产生的。少了它，这条用例就成了「一次
// 普通的流式请求，只是假上游恰好吐了工具分片」——那证明不了调用方声明的工具
// 真的转到了上游。
func anthropicToolStreamBody(model string) []byte {
	var body map[string]any
	if err := json.Unmarshal(messagesBody(model), &body); err != nil {
		panic(err)
	}
	body["stream"] = true
	body["tools"] = []any{
		map[string]any{
			"name":        "get_weather",
			"description": "查天气",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
			},
		},
		map[string]any{
			"name":        "get_forecast",
			"description": "查预报",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"days": map[string]any{"type": "integer"}},
			},
		},
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// blockFrame 是帧序列里一个**带块下标**的帧，按它落在哪个块上摊平。
//
// 摊平之后再判「这个下标此刻开着吗」，比在原始帧序列上写一串 if 更贴那条
// 不变式本身——客户端的累加器也是这么看的（先收 start，再按 index 找块）。
type blockFrame struct {
	// kind 是 start / delta / stop。
	kind string
	// index 是帧里的块下标。
	index int
	// block 是 start 帧的块类型（text / tool_use），其余帧为空。
	block string
	// id 与 name 是 start 帧里工具调用的标识，其余帧为空。
	id   string
	name string
	// partial 是 delta 帧带的参数片段，其余帧为空。
	partial string
}

// blockFramesOf 把帧序列摊平成块帧。
//
// 不带下标的帧（message_start / message_delta / message_stop）跳过：它们与
// 「块有没有配平」无关。
func blockFramesOf(t *testing.T, frames []sseFrame) []blockFrame {
	t.Helper()

	var out []blockFrame
	for i, f := range frames {
		m := decodeFrame(t, f)
		raw, ok := m["index"]
		if !ok {
			continue
		}
		n, ok := raw.(float64)
		if !ok {
			t.Fatalf("第 %d 帧的 index 是 %T，期望一个数字", i, raw)
		}

		bf := blockFrame{index: int(n)}
		switch f.event {
		case "content_block_start":
			bf.kind = "start"
			bf.block = strOf(dig(t, m, "content_block", "type"))
			bf.id = strOf(dig(t, m, "content_block", "id"))
			bf.name = strOf(dig(t, m, "content_block", "name"))
		case "content_block_delta":
			bf.kind = "delta"
			bf.partial = strOf(dig(t, m, "delta", "partial_json"))
		case "content_block_stop":
			bf.kind = "stop"
		default:
			t.Errorf("第 %d 帧带着 index 却是不认识的事件名 %q", i, f.event)
		}
		out = append(out, bf)
	}
	return out
}

// strOf 把一个 JSON 值当字符串取出来，不是字符串时返回空串。
//
// 不在这里报类型错：调用方拿它比对的都是非空字面量，空串一样会让断言挂，
// 而挂的时候报出来的「这里是空的」比一句类型抱怨更贴题。
func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// checkBlockBrackets 逐帧检查块的开合。
//
// 判据来自协议：每个块有一个 index，对应它在最终 Message 的 content 数组里的
// 位置，而分片按帧里的 index 找块。于是三条同时成立：
//
//   - 一个下标只被 start 一次。重开会让最终消息里多出一个同 id 的空 tool_use
//     块，客户端会拿它再调一次工具（见 internal/api/stream.go 的说明）。
//   - 分片只落在**正开着**的块上。落在配过 stop 的块上，按「当前打开的块」
//     渲染的客户端会把它丢掉，严格校验的客户端会直接报错。
//   - 每个 start 恰好配一个 stop，且没有块一直开着。
//
// 这里**不**断言帧的先后顺序（工具块与正文块谁先关）：关的顺序不影响按 index
// 取块的客户端，那是取舍而不是不变式，钉住它会挡掉另一个同样合法的做法。
func checkBlockBrackets(t *testing.T, bfs []blockFrame) {
	t.Helper()

	starts := map[int]int{}
	open := map[int]bool{}
	for _, bf := range bfs {
		switch bf.kind {
		case "start":
			starts[bf.index]++
			if starts[bf.index] > 1 {
				t.Errorf("下标 %d 的块被打开了 %d 次——重开同一个下标会让客户端在最终消息里"+
					"多出一个同 id 的空 tool_use 块，并拿它再调一次工具", bf.index, starts[bf.index])
			}
			open[bf.index] = true
		case "delta":
			if !open[bf.index] {
				t.Errorf("下标 %d 上的分片 %q 落在块之外：这个块还没 start，或者已经配过 content_block_stop 了",
					bf.index, bf.partial)
			}
		case "stop":
			if !open[bf.index] {
				t.Errorf("下标 %d 的块在打开之前（或第二次）被关掉了", bf.index)
			}
			delete(open, bf.index)
		}
	}
	for index := range open {
		t.Errorf("下标 %d 的块一直开着，没等到 content_block_stop", index)
	}
}

// assembleToolUseBlocks 按块把工具调用拼回来：id 与 name 来自
// content_block_start，参数来自落在同一个下标上的 input_json_delta。
//
// 这正是两个官方 SDK 的累加器做的事（start 往 content 数组里追加，delta 与
// stop 按帧里的 index 取块），所以它同时是「客户端会看到什么」的模型——断言
// 落在它的输出上，等于断言客户端拿到的那份结果。
func assembleToolUseBlocks(bfs []blockFrame) []blockFrame {
	var (
		out     []blockFrame
		pos     = map[int]int{}
		blockOf = map[int]string{}
	)
	for _, bf := range bfs {
		switch bf.kind {
		case "start":
			blockOf[bf.index] = bf.block
			if bf.block != "tool_use" {
				continue
			}
			pos[bf.index] = len(out)
			out = append(out, bf)
		case "delta":
			at, ok := pos[bf.index]
			if !ok || blockOf[bf.index] != "tool_use" {
				// 正文块的分片：它的 start 不是 tool_use，参数不在这里拼。
				continue
			}
			out[at].partial += bf.partial
		}
	}
	return out
}

// 两个工具调用的分片**交错**到达（0、1、0、1），客户端按 Anthropic 协议读。
//
// 交错是合法的：OpenAI 的 tool_calls[].index 不保证一个用完再出现下一个，而
// 渲染器为每个上游下标各分配一个块——两条流的下标必须对得上，这正是单测照
// 不到的那条缝。
//
// 期望值里两个工具调用的参数都是**两个分片拼出来的**：只吐一个完整分片的话，
// 「分片有没有落回自己那个块」这件事根本不会被碰到。
func TestStreamMessagesToolCallsWithOpenAIUpstreamEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-cross-tool")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		toolCalls: []toolCallDelta{
			{Index: 0, ID: "call_1", Name: "get_weather", Args: `{"city":`},
			{Index: 1, ID: "call_2", Name: "get_forecast", Args: `{"days":`},
			{Index: 0, Args: `"北京"}`},
			{Index: 1, Args: `3}`},
		},
		usage:  &channel.Usage{PromptTokens: 12, CompletionTokens: 7},
		db:     db,
		userID: user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "openai", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/messages", key, anthropicToolStreamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}

	frames := readFrames(t, resp.Body, 0)
	if len(frames) == 0 {
		t.Fatal("一帧都没收到")
	}
	// 结束标记要真的到客户端手里：没有它，客户端看到的是截断（见
	// internal/api/stream.go 的「结束标记不能伪造」）。
	if last := frames[len(frames)-1]; last.event != "message_stop" {
		t.Errorf("最后一帧的事件名 = %q，期望 message_stop", last.event)
	}

	bfs := blockFramesOf(t, frames)

	// 正向证据：下面三条断言都是「遍历这一堆帧」的形状，帧一条都没取到时它们
	// 会**全部静默通过**——「没跑成」与「跑成了」长得一样（LESSONS.md 第 16 条）。
	// 条数由假上游吐的帧定死，写在这里当哨兵。
	//
	// 用 Errorf 而不是 Fatalf：条数不对时下面那三条的具体报错更贴题，而
	// Fatalf 会把它们一起挡掉——只留下「条数不对」，读者还是不知道该看哪儿。
	byKind := map[string]int{}
	for _, bf := range bfs {
		byKind[bf.kind]++
	}
	if byKind["start"] != 2 || byKind["delta"] != 4 || byKind["stop"] != 2 {
		t.Errorf("块帧的条数 = start %d / delta %d / stop %d，期望 2 / 4 / 2"+
			"（两个工具块，各两个参数分片）：%+v",
			byKind["start"], byKind["delta"], byKind["stop"], bfs)
	}

	// 一、块的形状：每个下标只开一次，分片只落在正开着的块上，每对 start/stop
	// 配平。
	checkBlockBrackets(t, bfs)

	// 二、参数完整，且分片落回了**自己那次**工具调用的块里。
	//
	// 这条流里没有正文，所以两个工具块就是下标 0 与 1——「index 就是它在最终
	// content 数组里的位置」这一条在这里是可判的。
	got := assembleToolUseBlocks(bfs)
	want := []struct{ id, name, partial string }{
		{"call_1", "get_weather", `{"city":"北京"}`},
		{"call_2", "get_forecast", `{"days":3}`},
	}
	if len(got) != len(want) {
		t.Fatalf("收到 %d 个工具块，期望 %d 个（每个上游下标一个块）：%+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].index != i {
			t.Errorf("第 %d 个工具块的下标 = %d，期望 %d（块下标就是它在最终 content 数组里的位置）",
				i, got[i].index, i)
		}
		if got[i].id != w.id || got[i].name != w.name || got[i].partial != w.partial {
			t.Errorf("第 %d 个工具块（下标 %d）= id %q name %q 参数 %q，期望 id %q name %q 参数 %q",
				i, got[i].index, got[i].id, got[i].name, got[i].partial, w.id, w.name, w.partial)
		}
		// 拼出来的东西要能当 JSON 解析：分片错位或漏掉一片，这里就是一个
		// 半截的 JSON。
		var parsed map[string]any
		if err := json.Unmarshal([]byte(got[i].partial), &parsed); err != nil {
			t.Errorf("第 %d 个工具块的参数不是完整的 JSON: %v（原文 %q）", i, err, got[i].partial)
		}
	}

	// 三、发出去的那份请求真的是流式的，且调用方声明的工具真的到了上游。
	// 没有这一条，假上游吐的工具分片就成了「一次普通的流式请求碰巧收到了
	// 工具分片」——上游那一侧的形状没有被钉住。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if !sent.Stream {
		t.Error("上游收到的请求里 stream 不是 true——调用方要的流式没有传到上游")
	}
	names := make([]string, 0, len(sent.Tools))
	for _, tool := range sent.Tools {
		names = append(names, tool.Function.Name)
	}
	if !slices.Equal(names, []string{"get_weather", "get_forecast"}) {
		t.Errorf("上游收到的工具定义 = %v，期望 get_weather 与 get_forecast 两个（顺序与声明一致）", names)
	}

	// 四、钱：与非流式那条路同一个数——上游报过用量时按实测值结算，工具调用
	// 那一侧的分片不改变口径。
	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003）", bill.final.String)
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, channelID)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false——这一列是事后分辨两条路的唯一依据")
	}
	assertWallet(t, db, user, "99.99967", "0")
}
