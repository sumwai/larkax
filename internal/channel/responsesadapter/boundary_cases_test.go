package responsesadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/sumwai/larkax/internal/channel"
)

// 这个文件补的是变异测试留下的存活条目（issue #851 的 C 组）：它们都落在
// 「大部分输入都一样」的区间里——条件被取反、`&&` 换成 `||`、比较的边界挪一格，
// 原有那条用例仍然绿。所以这里不重复走一遍正常路径，只喂那些**能把两版分开**
// 的输入：缺字段、空串、nil、只有一个字段填了值的对象。
//
// 每条用例的注释都写清它钉的是哪一处判据。判为等价的几条也放在这里，附上
// 为什么观察不到差别的论证与前提。

// pumpEvents 直接驱动 pump，不经过 HTTP，也不发那条 start 事件。
//
// 有的输入在假上游那一侧不好造：读失败只能从 io.Reader 这一层喂，缺 item 的
// output_item.done 在 HTTP 上写出来也一样，但直接构造更省事。
func pumpEvents(t *testing.T, ctx context.Context, body io.Reader) []channel.StreamEvent {
	t.Helper()
	events := make(chan channel.StreamEvent)
	go func() {
		defer close(events)
		newAdapter().pump(ctx, http.StatusOK, true, false, body, events)
	}()
	return collect(t, events)
}

// sseBody 把若干条 JSON 载荷拼成 SSE 请求体（每条前面加 data:）。
func sseBody(payloads ...string) string {
	var sb strings.Builder
	for _, p := range payloads {
		sb.WriteString("data: ")
		sb.WriteString(p)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// TestRawStringReadsOnlyStrings 覆盖 rawString 的两处判据。
//
// 空输入直接给空串，其余交给 json.Unmarshal 解成 string。把 `len(raw) == 0`
// 取反之后，非空的合法字符串反而直接变成空串；把 `err != nil` 取反之后，
// 解析成功的那一格返回空串。两种都在下面这张表里。
func TestRawStringReadsOnlyStrings(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"合法字符串", `"completed"`, "completed"},
		{"空输入", ``, ""},
		{"不是字符串", `123`, ""},
		{"null", `null`, ""},
		{"字符串里的空串", `""`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rawString(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("rawString(%s) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestResponsesErrorEmpty 覆盖「什么算没有错误」的三个字段。
//
// `empty` 的判据是「三个字段都为空」：上游有 `"error": null` 与 `"error": {}`
// 两种正常的空写法，而任一字段填了值就说明这次是真的失败了。逐格喂一个字段，
// 是因为变异会把 `&&` 换成 `||`（任一字段为空就算空），也会把某一个
// `== ""` 反过来——三种错误各有各的区分输入，合成一条就分不清是哪一处坏了。
func TestResponsesErrorEmpty(t *testing.T) {
	cases := []struct {
		name string
		err  *responsesError
		want bool
	}{
		{"nil", nil, true},
		{"空对象", &responsesError{}, true},
		{"只有 message", &responsesError{Message: "boom"}, false},
		{"只有 type", &responsesError{Type: "server_error"}, false},
		{"只有 code", &responsesError{Code: "server_error"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.empty(); got != tc.want {
				t.Errorf("empty() = %v，期望 %v（%+v）", got, tc.want, tc.err)
			}
		})
	}
}

// TestResponsesOutputArguments 覆盖工具参数的取法。
//
// custom 工具的入参在 input 里（自由文本），普通工具的入参在 arguments 里。
// 三格分别把两处判据分开：类型判断被取反时，非 custom 项会去读 input；
// `&&` 换成 `||` 时，没有 input 的 custom 项会返回空串；`len(o.Input) > 0`
// 的边界挪一格时，空 input 会被当成有内容。
func TestResponsesOutputArguments(t *testing.T) {
	cases := []struct {
		name string
		out  responsesOutput
		want string
	}{
		{
			name: "custom 用 input",
			out: responsesOutput{
				Type:      "custom_tool_call",
				Input:     json.RawMessage(`"自由文本"`),
				Arguments: json.RawMessage(`"{\"a\":1}"`),
			},
			want: "自由文本",
		},
		{
			name: "custom 没有 input 时退回 arguments",
			out: responsesOutput{
				Type:      "custom_tool_call",
				Arguments: json.RawMessage(`"{\"a\":1}"`),
			},
			want: `{"a":1}`,
		},
		{
			name: "非 custom 不看 input",
			out: responsesOutput{
				Type:      "function_call",
				Input:     json.RawMessage(`"自由文本"`),
				Arguments: json.RawMessage(`"{\"a\":1}"`),
			},
			want: `{"a":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.out.arguments(); got != tc.want {
				t.Errorf("arguments() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestOutputTextTwoPasses 覆盖 outputText 的两遍判据。
//
// 第一遍认官方形状：message 项、role 为空或 assistant、内容 type=output_text。
// 一个都没认出来时第二遍退回「任何非空 text」——有的兼容层不写 content 的 type，
// 而第二遍要修的正是「一个字都不发」。
//
// 四格分别对应四处变异：role 的两个否定条件（空 role 与 assistant 都要收）、
// 内容 type 的判断（只认 output_text，且要求 text 非空）、以及两遍各自的
// `item.Type != "message"`（非 message 项的 content 不是正文）。
func TestOutputTextTwoPasses(t *testing.T) {
	textPart := func(text string) responsesOutputContent {
		return responsesOutputContent{Type: "output_text", Text: text}
	}
	messageItem := func(role string, content ...responsesOutputContent) responsesOutput {
		return responsesOutput{Type: "message", Role: role, Content: content}
	}

	cases := []struct {
		name  string
		items []responsesOutput
		want  string
	}{
		{
			name:  "role 为空与 assistant 都算正文",
			items: []responsesOutput{messageItem("", textPart("A")), messageItem("assistant", textPart("B"))},
			want:  "AB",
		},
		{
			name:  "只认 output_text，拒绝文案不算",
			items: []responsesOutput{messageItem("assistant", textPart("A"), responsesOutputContent{Type: "refusal", Text: "B"})},
			want:  "A",
		},
		{
			name:  "没有 output_text 时退回任何 text",
			items: []responsesOutput{messageItem("assistant", responsesOutputContent{Type: "refusal", Text: "B"})},
			want:  "B",
		},
		{
			name:  "非 message 项的 content 不是正文",
			items: []responsesOutput{{Type: "reasoning", Content: []responsesOutputContent{textPart("X")}}},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outputText(tc.items); got != tc.want {
				t.Errorf("outputText() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestItemKey 固定的是「这一项是哪一个」的两个键。
//
// output_item.* 给 item.id，参数增量给 item_id 与 output_index。三个都可能缺：
// item_id 为空时退回 item.id；两者都在时以 item_id 为准；都没有时给空串，
// 由 toolIndexer 那条「接到最近一次调用」的退路处理，而不是去解引用 nil 的 Item。
func TestItemKey(t *testing.T) {
	idx := 3
	cases := []struct {
		name    string
		ev      responsesStreamEvent
		wantID  string
		wantIdx *int
	}{
		{
			name:   "只给 item.id 时用它",
			ev:     responsesStreamEvent{Item: &responsesOutput{ID: "item_a"}},
			wantID: "item_a",
		},
		{
			name:   "item_id 优先于 item.id",
			ev:     responsesStreamEvent{ItemID: "event_a", Item: &responsesOutput{ID: "item_b"}},
			wantID: "event_a",
		},
		{
			name:   "两个都没有时为空",
			ev:     responsesStreamEvent{},
			wantID: "",
		},
		{
			name:    "output_index 原样带出",
			ev:      responsesStreamEvent{OutputIndex: &idx},
			wantID:  "",
			wantIdx: &idx,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotIdx := tc.ev.itemKey()
			if gotID != tc.wantID {
				t.Errorf("itemID = %q，期望 %q", gotID, tc.wantID)
			}
			switch {
			case tc.wantIdx == nil && gotIdx != nil:
				t.Errorf("outputIndex = %d，期望没有", *gotIdx)
			case tc.wantIdx != nil && gotIdx == nil:
				t.Errorf("outputIndex 缺失，期望 %d", *tc.wantIdx)
			case tc.wantIdx != nil && *gotIdx != *tc.wantIdx:
				t.Errorf("outputIndex = %d，期望 %d", *gotIdx, *tc.wantIdx)
			}
		})
	}
}

// TestAnnotationParsing 覆盖「这条事件里有没有可转发的标注」。
//
// 空对象与 null 都解不出东西，应当与「字段缺失」同一种处置：没有可转发的内容。
// 把 `err != nil || len(out) == 0` 的 `||` 换成 `&&` 之后，这两种会反过来被判成
// 「有标注」，于是转发出去一个空标注。
func TestAnnotationParsing(t *testing.T) {
	cases := []struct {
		name   string
		raw    json.RawMessage
		wantOK bool
	}{
		{"没有字段", nil, false},
		{"空对象", json.RawMessage(`{}`), false},
		{"null", json.RawMessage(`null`), false},
		{"不是对象", json.RawMessage(`"text"`), false},
		{"正常对象", json.RawMessage(`{"type":"url_citation"}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (responsesStreamEvent{Annotation: tc.raw}).annotation()
			if ok != tc.wantOK {
				t.Errorf("annotation() 的 ok = %v，期望 %v（%s）", ok, tc.wantOK, tc.raw)
			}
			if !tc.wantOK && got != nil {
				t.Errorf("没有可转发的内容时不该给出对象：%+v", got)
			}
		})
	}
}

// TestErrorTextPrefersMessage 覆盖错误文案的取法。
//
// 依次看顶层 error 与 response.error 两个对象，每个对象里 message 优先于 code，
// code 优先于 type；两边都没有时退到平铺的 message，最后才是那句默认文案。
//
// 把 `e.Message != ""` 取反之后，填了 message 的对象会先返回空串；把
// responseErrorOf 的 `r == nil` 取反之后，response 里的错误对象整个被跳过、
// 直接掉到那句默认文案。
func TestErrorTextPrefersMessage(t *testing.T) {
	cases := []struct {
		name string
		ev   responsesStreamEvent
		want string
	}{
		{
			// 放在最前面：其余几格都没有 Response，而 `r == nil` 被取反的那一版
			// 在 nil 上会解引用崩溃。先跑这一格，红出来的是断言而不是 panic。
			name: "response 里的错误对象也算",
			ev:   responsesStreamEvent{Response: &responsesResponse{Error: &responsesError{Message: "对象里的错误"}}},
			want: "对象里的错误",
		},
		{
			name: "message 优先于 code",
			ev:   responsesStreamEvent{Error: &responsesError{Message: "上游内部错误", Code: "server_error"}},
			want: "上游内部错误",
		},
		{
			name: "没有 message 时用 code",
			ev:   responsesStreamEvent{Error: &responsesError{Code: "server_error"}},
			want: "server_error",
		},
		{
			name: "空错误对象退到平铺 message",
			ev:   responsesStreamEvent{Error: &responsesError{}, Message: "上游说不行"},
			want: "上游说不行",
		},
		{
			name: "什么都没有时用那句默认文案",
			ev:   responsesStreamEvent{},
			want: "上游在流中途报错",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.errorText(); got != tc.want {
				t.Errorf("errorText() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestChatStreamAcceptsLargeEvent 覆盖扫描缓冲区的大小。
//
// 单个事件可能很大：长文本的一次性增量、或一整段工具参数。默认的 64KB 上限
// 会把合法响应判成读取失败，所以这里把上限提到 4MB。上限被算小之后
// （`4*1024*1024` 被除成 0 或 4），超过 64KB 的这一行会触发 bufio.ErrTooLong，
// 正文以 error 事件收场。
//
// 起点那一段 `64*1024` 的容量算成 0 观察不到差别：Scanner 的初始缓冲区只是个
// 分配提示，它会自己长到 maxTokenSize。那一条是等价变异。
func TestChatStreamAcceptsLargeEvent(t *testing.T) {
	// 四万个汉字，UTF-8 下 120KB，稳稳超过 64KB 的初始缓冲区。
	text := strings.Repeat("长", 40_000)
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"`+text+`"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "delta":
			got.WriteString(ev.Text)
		case "error":
			t.Fatalf("一个 120KB 的事件不该让这条流失败: %v", ev.Err)
		}
	}
	if got.String() != text {
		t.Errorf("收到的正文长度 = %d，期望 %d", len(got.String()), len(text))
	}
}

// TestChatStreamTruncatedIsNotAnError 覆盖「正常结束与读失败是两件事」。
//
// 上游直接关连接（扫到 EOF）时，这条流既没有终态事件、也没有读错误：不补 done、
// 不补 usage，同样不该补一个错误事件。把 `scanner.Err() != nil` 取反、或把
// 它后面的 `&& ctx.Err() == nil` 换成 `||`，这里都会凭空多出一条错误事件——
// 而网关会把错误事件当成「上游坏了」去换渠道重试，客户端于是收到两段回答。
func TestChatStreamTruncatedIsNotAnError(t *testing.T) {
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_text.delta","output_index":0,"delta":"半截"}`,
	)))
	var text string
	for _, ev := range events {
		switch ev.Kind {
		case "delta":
			text += ev.Text
		case "error":
			t.Errorf("正常 EOF 不是错误，不该发出错误事件: %v", ev.Err)
		case "done", "usage":
			t.Errorf("缺少终态事件时不该伪造 %s", ev.Kind)
		}
	}
	if text != "半截" {
		t.Errorf("截断前收到的正文 = %q，期望「半截」", text)
	}
}

// TestPumpReportsReadFailure 覆盖读失败的报错。
//
// 读失败时上下文还活着，这正是要报错的那一格：流没有正常结束，调用方必须知道。
// 把 `ctx.Err() == nil` 取反之后，这一格反而被吞掉——调用方会把「读到一半失败」
// 当成「上游正常截断」，两者的计费与重试处置不同。
func TestPumpReportsReadFailure(t *testing.T) {
	readErr := errors.New("connection reset")
	events := pumpEvents(t, context.Background(), iotest.ErrReader(readErr))
	var errs []channel.StreamEvent
	for _, ev := range events {
		if ev.Kind == "error" {
			errs = append(errs, ev)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("读失败要恰好发一条错误事件，实际 %d 条: %+v", len(errs), events)
	}
	// 用 errors.Is 而不是只看「非 nil」：错误事件要带的是**这次读失败本身**，
	// 而不是随便一个错误。事件里的 Err 是 ErrUpstream，Unwrap 之后就是读取器
	// 返回的那个错误。
	if !errors.Is(errs[0].Err, readErr) {
		t.Errorf("错误事件里的错误 = %v，期望包含读取器返回的 %q", errs[0].Err, readErr)
	}
}

// TestSendTextIgnoresNilItem 覆盖「output_item.done 只有 output_index」这一格。
//
// 有的上游把正文放在别的帧里，output_item.done 只带下标、不带 item。sendText
// 收到 nil 时直接返回。
//
// 这一条对应 `stream.go:148:18` 的 INVERT_LOGICAL：把第一个 `||` 换成 `&&`
// 之后，`item == nil` 那一支会去读 `item.Type`，在 nil 上解引用。
//
// **这条变异体只有 panic 能观察到，不算普通的击杀。** 差别只在 item 为 nil 的
// 那一格：item 非 nil 但类型不是 message 时，变异体会越过第一个短路继续往下走，
// 而 outputText 对非 message 项两遍都跳过、返回空串，于是同样不发 delta——
// 事件、状态、返回值两版完全一样。下面只钉「不发 delta」，那次 panic 不当作
// 「用例把这条判据固定住了」的证据（靠 panic 的红可能来自别的原因）。
func TestSendTextIgnoresNilItem(t *testing.T) {
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_item.done","output_index":0}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)))
	for _, ev := range events {
		if ev.Kind == "delta" {
			t.Errorf("没有 item 就没有正文可补，不该发 delta: %q", ev.Text)
		}
	}
}

// TestChatStreamAnnotationWithoutFieldsDoesNotWarn 覆盖标注那一支的 else-if。
//
// 只有「字段给了、但里面没有可转发的内容」才记 warn；字段整个不在时什么也不做。
// 把 `len(ev.Annotation) > 0` 的边界挪一格之后，缺字段的标注事件也会记一条
// 「引用标注解析失败」——日志会把一条正常流说成坏流。
func TestChatStreamAnnotationWithoutFieldsDoesNotWarn(t *testing.T) {
	logs := captureLogs(t)
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_text.delta","output_index":0,"delta":"答"}`,
		`{"type":"response.output_text.annotation.added","output_index":0}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)))
	for _, ev := range events {
		if ev.Kind == "annotation" {
			t.Errorf("没有标注内容，不该转发标注事件: %+v", ev.Annotations)
		}
	}
	if strings.Contains(logs.String(), "引用标注解析失败") {
		t.Errorf("字段整个不在不是解析失败，不该记 warn：\n%s", logs.String())
	}
}

// TestChatStreamEmptyToolArgumentsAreNotForwarded 覆盖空参数的两处判据。
//
// `.done` 给全文、`.delta` 给片段，空串都不是内容：不转发、也不为它建一个
// 工具调用槽。把 `&&` 换成 `||` 之后，空串会走进去，于是多出一条空参数的
// tool_use，结束原因也变成 tool_calls（槽数大于 0）。
func TestChatStreamEmptyToolArgumentsAreNotForwarded(t *testing.T) {
	cases := []struct {
		name   string
		events []string
	}{
		{
			name:   "function_call_arguments.done 的空串",
			events: []string{`{"type":"response.function_call_arguments.done","item_id":"call_a","arguments":""}`},
		},
		{
			name:   "custom_tool_call_input.delta 的空串",
			events: []string{`{"type":"response.custom_tool_call_input.delta","item_id":"call_a","input":""}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payloads := append(tc.events,
				`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`)
			events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(payloads...)))
			for _, ev := range events {
				if ev.Kind == "tool_use" {
					t.Errorf("空串不是参数内容，不该发 tool_use: %+v", ev.ToolCall)
				}
				if ev.Kind == "done" && ev.FinishReason != "stop" {
					t.Errorf("FinishReason = %q，没有工具调用时应当是 stop", ev.FinishReason)
				}
			}
		})
	}
}

// TestChatStreamOutputItemDoneWithoutArgumentsIsSilent 覆盖参数补发那条退路：
// 只有 `.done` 里给了全文、且前面没发过增量时才发一次。
//
// 参数只在 `.done` 里给全文、且前面没有发过增量时才补发一次。空参数没有可补的
// 内容，不该发事件；把 `args != "" && !slot.argsSent` 的 `&&` 换成 `||` 之后，
// 空参数也会被补发出去，调用方拿到一个空参数的工具调用。
func TestChatStreamOutputItemDoneWithoutArgumentsIsSilent(t *testing.T) {
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"call_a","name":"f"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"call_a","name":"f"}}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)))
	var calls []*channel.ToolCallDelta
	for _, ev := range events {
		if ev.Kind == "tool_use" {
			calls = append(calls, ev.ToolCall)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("只有 added 那一条该发 tool_use，实际 %d 条: %+v", len(calls), calls)
	}
	if calls[0].Name != "f" {
		t.Errorf("唯一那条 tool_use 应当是 added 给的调用名，实际 %+v", calls[0])
	}
}

// TestChatStreamToolArgumentsFollowOutputIndex 覆盖「按 output_index 关联参数」。
//
// 有的上游只给 output_index，不给 item_id 也不给 item.id。added 那一帧必须把
// 下标记下来，后面的参数增量才接得上同一次调用；不记的话增量会新建一个槽，
// 下标从 1 开始，客户端会建出一个带空洞的数组。
func TestChatStreamToolArgumentsFollowOutputIndex(t *testing.T) {
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"f"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"a\":1}"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)))
	calls := toolCalls(events)
	if len(calls) != 2 {
		t.Fatalf("期望 added 与增量各一条 tool_use，实际 %d 条: %+v", len(calls), calls)
	}
	for i, c := range calls {
		if c.Index != 0 {
			t.Errorf("第 %d 条 tool_use 的下标 = %d，期望 0（两次事件是同一次调用）", i, c.Index)
		}
	}
	if calls[1].Arguments != `{"a":1}` {
		t.Errorf("参数 = %q，期望 %q", calls[1].Arguments, `{"a":1}`)
	}
}

// TestChatStreamToolIndexPrefersOutputIndexOverLastSlot 固定的是查找的顺序。
//
// 两个键都在时先按 output_index 找，找不到才退到「接到最近一次调用上」那条退路。
// 把 output_index 的查找整段跳过之后，下面的参数增量会被接到**第二次**调用上
// （那条退路返回最后一个槽），调用方看到参数挂错了工具。
//
// 第二次调用必须落在**另一个**槽里。不然那条退路取出的「最近一次」，
// 正好是参数本来该挂的那一次，错挂也就看不出来了。它带着自己的 item.id，
// 带不带 output_index 都会新建一个槽；这里照样给它一个下标，让两帧都
// 走到「按 output_index 记录」那条路。
func TestChatStreamToolIndexPrefersOutputIndexOverLastSlot(t *testing.T) {
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"call_a","name":"f"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"call_b","name":"g"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)))
	calls := toolCalls(events)
	if len(calls) != 3 {
		t.Fatalf("期望两条 added 与一条增量，实际 %d 条: %+v", len(calls), calls)
	}
	if calls[0].Index != 0 || calls[1].Index != 1 {
		t.Errorf("两条 added 的下标 = %d, %d，期望 0, 1", calls[0].Index, calls[1].Index)
	}
	if calls[2].Index != 0 || calls[2].Arguments != "{}" {
		t.Errorf("参数增量 = %+v，期望挂在下标 0 的那次调用上", calls[2])
	}
}

// TestChatStreamToolArgumentsFollowItemID 覆盖「按 item_id 关联参数」。
//
// 另一类上游只给 item_id，不给 output_index。added 把 item_id 记进表里，
// 增量才找得到同一个槽；不记的话增量会新建一个槽，下标从 1 开始。
func TestChatStreamToolArgumentsFollowItemID(t *testing.T) {
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_item.added","item_id":"call_a","item":{"type":"function_call","name":"f"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call_a","delta":"{}"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)))
	calls := toolCalls(events)
	if len(calls) != 2 {
		t.Fatalf("期望 added 与增量各一条 tool_use，实际 %d 条: %+v", len(calls), calls)
	}
	for i, c := range calls {
		if c.Index != 0 {
			t.Errorf("第 %d 条 tool_use 的下标 = %d，期望 0（两次事件是同一次调用）", i, c.Index)
		}
	}
}

// TestChatStreamUnknownItemIDStartsNewCall 固定的是查找在「id 没见过」时的返回值。
//
// 那条退路只管「两个键都没有」那一格：连 item_id 都没给时没法关联，
// 就把这次参数接到最近一次调用上。反过来，给了一个**没见过的** item_id，
// 说明这是另一次调用（我们漏掉了它的 added 帧），应当新建一个槽，而不是
// 把参数挂到上一次调用上——那会让调用方看到两个工具调用的参数拼在一起。
//
// 把 `if itemID == ""` 取反之后，这条增量会走进那条退路、接到下标 0 的 call_a 上。
func TestChatStreamUnknownItemIDStartsNewCall(t *testing.T) {
	events := pumpEvents(t, context.Background(), strings.NewReader(sseBody(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"call_a","name":"f"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call_unseen","delta":"{}"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)))
	calls := toolCalls(events)
	if len(calls) != 2 {
		t.Fatalf("期望 added 与增量各一条 tool_use，实际 %d 条: %+v", len(calls), calls)
	}
	if calls[0].Index != 0 {
		t.Errorf("added 的下标 = %d，期望 0", calls[0].Index)
	}
	if calls[1].Index != 1 {
		t.Errorf("没见过的 item_id 要新建一次调用（下标 1），实际 %d：参数被接到了上一次调用上", calls[1].Index)
	}
}

// TestToolIndexerKeepsCallsWithoutItemIDApart 覆盖「没有 item_id 的两次调用不合并」。
//
// 两个键都没有时不能把它们当成同一次调用，除非连 output_index 也没有
// （那一支接最近一次）。
func TestToolIndexerKeepsCallsWithoutItemIDApart(t *testing.T) {
	zero, one := 0, 1
	tools := newToolIndexer()
	a := tools.slotFor("", &zero)
	b := tools.slotFor("", &one)
	if a == b {
		t.Fatal("两个不同的 output_index 不该落到同一个槽")
	}
	if a.index != 0 || b.index != 1 {
		t.Errorf("下标 = %d, %d，期望 0, 1", a.index, b.index)
	}
}

// TestChatStreamIgnoresBlankAndCommentLines 覆盖帧分隔与心跳的跳过。
//
// 空行是帧分隔，以冒号开头的是 SSE 注释（心跳）。两者都不该产生事件，也不该
// 影响后面的正文。
//
// 这一格是等价变异，写在这里是为了留下「为什么杀不掉」的论证：
// `line == "" || strings.HasPrefix(line, ":")` 换成 `&&` 之后条件永远为假，
// 但空行与注释都过不了下面那条 `!strings.HasPrefix(line, "data:")`，同样
// continue。两支之间没有任何可观察的差别（不打日志、不发事件）。
func TestChatStreamIgnoresBlankAndCommentLines(t *testing.T) {
	body := "\n\n: heartbeat\n\n" + sseBody(
		`{"type":"response.output_text.delta","output_index":0,"delta":"答"}`,
		`{"type":"response.completed","response":{"id":"r1","model":"m","status":"completed"}}`,
	)
	events := pumpEvents(t, context.Background(), strings.NewReader(body))
	var text string
	sawDone := false
	for _, ev := range events {
		switch ev.Kind {
		case "delta":
			text += ev.Text
		case "done":
			sawDone = true
		case "error":
			t.Errorf("空行与注释不该让这条流报错: %v", ev.Err)
		}
	}
	if text != "答" || !sawDone {
		t.Errorf("正文 = %q，done = %v：空行与注释之后这条流仍要正常走完", text, sawDone)
	}
}

// toolCalls 取出全部 tool_use 事件的载荷。
func toolCalls(events []channel.StreamEvent) []*channel.ToolCallDelta {
	var out []*channel.ToolCallDelta
	for _, ev := range events {
		if ev.Kind == "tool_use" {
			out = append(out, ev.ToolCall)
		}
	}
	return out
}
