package responsesadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
)

func testChannel(baseURL string) channel.Channel {
	return channel.Channel{
		ID:         1,
		Name:       "test",
		Type:       Type,
		BaseURL:    baseURL,
		Credential: "placeholder-credential",
	}
}

func newAdapter() *Adapter { return New(5 * time.Second) }

// textRequest 是一条最小的请求，多数用例用它。
func textRequest() *channel.Request {
	return &channel.Request{
		Model:    "up-model",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
}

// decodeBody 把请求体解成 map，供逐字段断言。
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("请求体不是 JSON: %v（%s）", err, body)
	}
	return m
}

// captureServer 起一个假上游：记录请求体与路径，回一段固定响应。
func captureServer(t *testing.T, status int, response string) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotPath = string(b), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody, &gotPath
}

// sseServer 起一个假上游，把给定的行按 SSE 帧写出。
func sseServer(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range lines {
			_, _ = io.WriteString(w, line+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// collect 读完事件流并返回全部事件。
func collect(t *testing.T, events <-chan channel.StreamEvent) []channel.StreamEvent {
	t.Helper()
	var out []channel.StreamEvent
	for ev := range events {
		out = append(out, ev)
	}
	return out
}

func TestTypeAndProtocol(t *testing.T) {
	a := newAdapter()
	if a.Type() != Type {
		t.Errorf("Type() = %q", a.Type())
	}
	if Type != "openai-responses" {
		t.Errorf("注册名 = %q，渠道表里的取值要跟着它", Type)
	}
	if a.Protocol() != channel.ProtocolOpenAIResponses {
		t.Errorf("Protocol() = %q", a.Protocol())
	}
}

// 请求体的形状：messages 变成 input、system 变成 instructions、
// max_tokens 变成 max_output_tokens。这几处是 Responses 与 chat 的分界，
// 错一处的表现是上游 400，而错误信息指向的是我们发出去的字段名。
func TestChatRequestShape(t *testing.T) {
	srv, gotBody, gotPath := captureServer(t, http.StatusOK,
		`{"id":"r1","model":"up-model","status":"completed","output":[]}`)

	temp := 0.5
	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{
		Model:           "up-model",
		MaxTokens:       128,
		Temperature:     &temp,
		ReasoningEffort: "high",
		Messages: []channel.Message{
			{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "你是助手"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []channel.Tool{{Name: "f", Description: "d", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	if *gotPath != "/v1/responses" {
		t.Errorf("请求路径 = %q", *gotPath)
	}
	body := decodeBody(t, []byte(*gotBody))
	if body["instructions"] != "你是助手" {
		t.Errorf("instructions = %v（system 消息要折成顶层字段）", body["instructions"])
	}
	// max_tokens 这个名字在 Responses 里不存在，叫 max_output_tokens。
	if _, stale := body["max_tokens"]; stale {
		t.Error("请求体里不该出现 max_tokens")
	}
	if body["max_output_tokens"] != float64(128) {
		t.Errorf("max_output_tokens = %v", body["max_output_tokens"])
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v（应当只剩那条 user 消息）", body["input"])
	}
	first, _ := input[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hi" {
		t.Errorf("input[0] = %#v", first)
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning.effort = %v", reasoning["effort"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	if tool, _ := tools[0].(map[string]any); tool["type"] != "function" || tool["name"] != "f" {
		t.Errorf("tools[0] = %#v（Responses 的工具声明是平铺的，不套 function 一层）", tools[0])
	}
}

// max_output_tokens 是可选的：没指定就不填，走上游默认值。
// 这里与 anthropicadapter 补默认值不同——Claude 的 max_tokens 是必填。
func TestChatOmitsUnsetOutputBound(t *testing.T) {
	srv, gotBody, _ := captureServer(t, http.StatusOK,
		`{"id":"r1","model":"m","status":"completed","output":[]}`)
	if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest()); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if _, present := decodeBody(t, []byte(*gotBody))["max_output_tokens"]; present {
		t.Error("未指定输出上界时不该填 max_output_tokens")
	}
}

func TestChatSuccess(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"resp_1","object":"response","model":"up-model-2026","status":"completed",
		"max_output_tokens":128,
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"先想一下"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"你好"}]}
		],
		"usage":{"input_tokens":31,"output_tokens":18,"total_tokens":49,
			"input_tokens_details":{"cached_tokens":0},
			"output_tokens_details":{"reasoning_tokens":0}}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if resp.UpstreamModel != "up-model-2026" {
		t.Errorf("UpstreamModel = %q", resp.UpstreamModel)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "你好" {
		t.Errorf("内容 = %+v（reasoning 项不是可见输出）", resp.Message.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 31 || resp.Usage.CompletionTokens != 18 {
		t.Errorf("用量 = %+v", resp.Usage)
	}
}

// 用量里最要紧的一处：缓存与推理嵌在 details 里，而 chat 那边是
// prompt_tokens_details / completion_tokens_details。接错了账单金额会错，
// **而响应形状看着完全正常**——所以这里用字面量逐个钉住。
func TestChatUsageMapsDetails(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"resp_1","model":"m","status":"completed","output":[],
		"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,
			"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":7},
			"output_tokens_details":{"reasoning_tokens":25}}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	u := resp.Usage
	if u.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d（本项目口径含缓存，上游的 input_tokens 就是它）", u.PromptTokens)
	}
	if u.CompletionTokens != 40 {
		t.Errorf("CompletionTokens = %d", u.CompletionTokens)
	}
	if u.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d（input_tokens_details.cached_tokens）", u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 7 {
		t.Errorf("CacheWriteTokens = %d（input_tokens_details.cache_write_tokens）", u.CacheWriteTokens)
	}
	if u.ReasoningTokens != 25 {
		t.Errorf("ReasoningTokens = %d（output_tokens_details.reasoning_tokens）", u.ReasoningTokens)
	}
	if !u.CompletionIncludesReasoning {
		t.Error("OpenAI 口径下推理已含在输出里，必须声明出来——否则同一批 token 会被收两次钱")
	}
}

// 上面那个用例钉的是 channel.Usage，这里钉它接进计费之后**五个计价项**
// 各是什么：缓存要从输入里拆出去，推理要从输出里减掉。两处都错的话，
// 金额不对而没有任何一处报错。
func TestChatUsageFeedsBillingItems(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"resp_1","model":"m","status":"completed","output":[],
		"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,
			"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":7},
			"output_tokens_details":{"reasoning_tokens":25}}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := billing.FromChannelUsage(resp.Usage)
	// 100 = 30 缓存读 + 7 缓存写 + 63 普通输入；40 = 25 推理 + 15 普通输出。
	if got.PromptTokens != 63 || got.CompletionTokens != 15 ||
		got.CacheReadTokens != 30 || got.CacheWriteTokens != 7 || got.ReasoningTokens != 25 {
		t.Errorf("计费口径 = %+v", got)
	}
}

// 整段 prompt 全命中缓存：input_tokens 与 cached_tokens **相等**。
//
// 这是 issue 点名的那一处，也是最容易错的一处——缓存要从输入里拆出去，
// 而全命中时拆完输入恰好是 0，看起来像「上游没报输入」。真到账上就是
// 「输入不计费、只计缓存」，而响应形状完全正常。
//
// 缓存数取与输入**相等**的字面量，而不是随便取一个比它小的数：后者只能
// 证明「缓存被拆出去了」，证明不了「全拆之后输入那一项是 0 而不是负数或
// 原值」——那正是这条用例要钉的边界。
func TestChatUsageFullyCachedPrompt(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"resp_1","model":"m","status":"completed","output":[],
		"usage":{"input_tokens":9000,"output_tokens":12,"total_tokens":9012,
			"input_tokens_details":{"cached_tokens":9000},
			"output_tokens_details":{"reasoning_tokens":0}}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	u := resp.Usage
	if u.PromptTokens != 9000 || u.CacheReadTokens != 9000 {
		t.Errorf("用量 = %+v，期望输入 9000 且其中 9000 命中缓存", u)
	}
	if u.CacheWriteTokens != 0 || u.ReasoningTokens != 0 {
		t.Errorf("上游没报这两项，落到账上就该是 0：%+v", u)
	}

	got := billing.FromChannelUsage(u)
	if got.PromptTokens != 0 || got.CacheReadTokens != 9000 || got.CompletionTokens != 12 {
		t.Errorf("计费口径 = %+v，期望普通输入为 0、缓存读 9000、输出 12", got)
	}
}

// 有些上游在 HTTP 200 里塞一个 error 对象。只看状态码会把它当成正常响应，
// 于是「上游失败了」在账上表现为「成功但用量为零」。
func TestChatDetectsEmbeddedError(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK,
		`{"error":{"message":"模型不存在","type":"invalid_request_error"}}`)

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err == nil {
		t.Fatal("200 里的 error 对象应当被识别")
	}
	if channel.IsRetryable(err) {
		t.Error("参数类错误不该可重试：换渠道也是同一个错")
	}
}

// `"error": null` 是正常的「无错误」写法，不能被误判。
func TestChatIgnoresNullError(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK,
		`{"error":null,"id":"r1","model":"m","status":"completed","output":[]}`)
	if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest()); err != nil {
		t.Errorf("空 error 不应判为失败: %v", err)
	}
}

// status=failed 是 Responses 自己的失败终态，与 incomplete 不同：
// 后者是「停在了长度上限上」，那是一次正常结束，照常计费。
func TestChatStatusFailedIsError(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK,
		`{"id":"r1","model":"m","status":"failed","error":{"code":"server_error","message":"内部错误"},"output":[]}`)

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err == nil {
		t.Fatal("status=failed 应当报错")
	}
	if channel.IsRetryable(err) {
		t.Error("上游明确说这次失败了，换渠道重试不该由适配器判断为可重试")
	}
}

func TestChatIncompleteIsNotAnError(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"r1","model":"m","status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"写到一半"}]}],
		"usage":{"input_tokens":5,"output_tokens":9}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("incomplete 是正常结束，不该报错: %v", err)
	}
	if resp.FinishReason != "length" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "写到一半" {
		t.Errorf("内容 = %+v（截断的正文也要交出去，它要计费）", resp.Message.Content)
	}
}

func TestChatToolCallContent(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"r1","model":"m","status":"completed",
		"output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"天气\"}"}],
		"usage":{"input_tokens":5,"output_tokens":2}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	var found bool
	for _, b := range resp.Message.Content {
		if b.Type == "tool_use" {
			found = true
			if b.ToolUseID != "call_1" || b.ToolName != "lookup" {
				t.Errorf("工具调用 = %+v", b)
			}
			if b.ToolInput["q"] != "天气" {
				t.Errorf("工具参数 = %+v", b.ToolInput)
			}
		}
	}
	if !found {
		t.Fatalf("没有工具调用块: %+v", resp.Message.Content)
	}
}

// 远端 MCP 工具上游一律 400，而错误信息指向我们发出去的请求。
//
// 这条判据今天**走不到**：真正的拦阻在解析层（relay.checkToolType），非
// function 的声明在入口就被拒了。它活过来的那天是「channel.Tool 加了类型
// 字段、内置工具能转发」之后——那时解析层收窄，MCP 声明会真的到这里。
//
// 所以直接调它：它是发出去之前的最后一道，测不到它就等于它没被证明在工作
// （见 LESSONS.md 第 23 条）。它只拦 mcp 是刻意的——内置工具本该转发。
func TestCheckNoRemoteMCPTools(t *testing.T) {
	cases := []struct {
		name  string
		tools string
		want  bool
	}{
		{"远端 MCP", `[{"type":"mcp","server_label":"x","server_url":"https://mcp.example.com"}]`, true},
		{"普通函数工具", `[{"type":"function","name":"f","parameters":{}}]`, false},
		{"内置工具", `[{"type":"web_search"}]`, false},
		{"空工具表", ``, false},
		{"认不出的形状", `[1,2,3]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNoRemoteMCPTools(json.RawMessage(tc.tools))
			if tc.want && err == nil {
				t.Error("应当拦下")
			}
			if !tc.want && err != nil {
				t.Errorf("不该拦下: %v", err)
			}
		})
	}
}

// 目前只有测试在调 ListModels：channel.Adapter 的契约面。
func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer placeholder-credential" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m-1"},{"id":""},{"id":"m-2"}]}`)
	}))
	defer srv.Close()

	models, err := newAdapter().ListModels(context.Background(), testChannel(srv.URL))
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(models) != 2 || models[0] != "m-1" || models[1] != "m-2" {
		t.Errorf("模型列表 = %v（空 id 要被滤掉）", models)
	}
}

func TestQuotaUnsupported(t *testing.T) {
	if _, err := newAdapter().Quota(context.Background(), testChannel("http://x")); !errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("err = %v，应当是不支持而不是「查不到」", err)
	}
}

// ── 流式 ────────────────────────────────────────────────────────────────

// 必须在返回前确认上游已接受请求，否则调用方无法区分
// 「连不上」（可换渠道）与「流中途断了」（不能换）。
func TestChatStreamRejectsBeforeReturning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), textRequest())
	if err == nil {
		t.Fatal("上游拒绝时应在返回前报错，而不是返回一个只会发错误事件的 channel")
	}
	if channel.IsRetryable(err) {
		t.Error("401 不该可重试")
	}
}

// 用量**无需 opt-in**：chat 要显式带 stream_options.include_usage，
// 这里只要 stream:true——usage 跟着终态事件一起来。
func TestChatStreamDoesNotOptInUsage(t *testing.T) {
	srv, gotBody, _ := captureServer(t, http.StatusOK, "data: [DONE]\n\n")

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "up-model", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	body := decodeBody(t, []byte(*gotBody))
	if body["stream"] != true {
		t.Errorf("stream = %v", body["stream"])
	}
	if _, present := body["stream_options"]; present {
		t.Error("Responses 不该带 stream_options：用量不用 opt-in")
	}
}

func TestChatStreamHappyPath(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"你"}`,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"好"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	var usage *channel.Usage
	finish := ""
	sawDone := false
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "usage":
			usage = ev.Usage
		case "done":
			sawDone, finish = true, ev.FinishReason
		case "error":
			t.Fatalf("不该有错误事件: %v", ev.Err)
		}
	}
	if text.String() != "你好" {
		t.Errorf("拼出的文本 = %q", text.String())
	}
	if usage == nil {
		t.Fatal("终态事件带的用量要发出来：它是结算依据")
	}
	if usage.PromptTokens != 7 || usage.CompletionTokens != 2 {
		t.Errorf("用量 = %+v", usage)
	}
	if !sawDone || finish != "stop" {
		t.Errorf("done=%v finish=%q", sawDone, finish)
	}
}

// 有的上游一个 response.output_text.delta 都不发，正文只在
// output_item.done 或终态事件的 response.output 里给。不补发的话客户端
// 拿到空回答，而用量按实测结算——用户为空回答付钱。
//
// 同一份代码里工具参数本来就有这道兜底（output_item.done 里的 arguments），
// 正文没有，这条用例把两处拉齐。补发必须**恰好一次**：发两遍会让客户端看到
// 重复的正文，而「看到两遍」比「看到空回答」更容易被当成模型的问题。
func TestChatStreamTextOnlyInOutputItems(t *testing.T) {
	const msgItem = `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"完整回答"}]}`
	const usage = `"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}`

	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name: "正文只在 output_item.done 里",
			lines: []string{
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`,
				`data: {"type":"response.output_item.done","output_index":0,"item":` + msgItem + `}`,
				`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed",` + usage + `}}`,
			},
			want: "完整回答",
		},
		{
			name: "正文只在终态事件的 response.output 里",
			lines: []string{
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`,
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`,
				`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[` + msgItem + `],` + usage + `}}`,
			},
			want: "完整回答",
		},
		{
			name: "两处都给：只发一遍",
			lines: []string{
				`data: {"type":"response.output_item.done","output_index":0,"item":` + msgItem + `}`,
				`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[` + msgItem + `],` + usage + `}}`,
			},
			want: "完整回答",
		},
		{
			name: "发过增量就不再从终态补：不能变成两遍",
			lines: []string{
				`data: {"type":"response.output_text.delta","output_index":0,"delta":"完整"}`,
				`data: {"type":"response.output_text.delta","output_index":0,"delta":"回答"}`,
				`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[` + msgItem + `],` + usage + `}}`,
			},
			want: "完整回答",
		},
		{
			name: "两个 message 项都没有增量：两项都要发",
			lines: []string{
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"第一段"}]}}`,
				`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_2","role":"assistant","content":[{"type":"output_text","text":"第二段"}]}}`,
				`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed",` + usage + `}}`,
			},
			want: "第一段第二段",
		},
		{
			name: "终态之前连接就断了：done 里已经拿到的正文照发",
			lines: []string{
				`data: {"type":"response.output_item.done","output_index":0,"item":` + msgItem + `}`,
			},
			want: "完整回答",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseServer(t, tc.lines...)
			events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
				Model: "m", Stream: true, Messages: textRequest().Messages,
			})
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			deltas := 0
			for _, ev := range collect(t, events) {
				if ev.Kind == "delta" {
					deltas++
					text.WriteString(ev.Text)
				}
			}
			if deltas == 0 {
				t.Fatal("一个 delta 都没有：客户端会拿到空回答")
			}
			if text.String() != tc.want {
				t.Errorf("正文 = %q，期望 %q", text.String(), tc.want)
			}
		})
	}
}

// 流式的用量同样走 details，与非流式是同一处映射。
func TestChatStreamUsageMapsDetails(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":25}}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	var usage *channel.Usage
	for _, ev := range collect(t, events) {
		if ev.Kind == "usage" {
			usage = ev.Usage
		}
	}
	if usage == nil {
		t.Fatal("没有 usage 事件")
	}
	if usage.PromptTokens != 100 || usage.CacheReadTokens != 30 || usage.ReasoningTokens != 25 ||
		usage.CompletionTokens != 40 || !usage.CompletionIncludesReasoning {
		t.Errorf("用量 = %+v", usage)
	}
}

// 工具调用的下标由我们自己从 0 起分配，而不是透传上游的 output_index：
// 后者是这一项在 output[] 里的位置，reasoning 与 message 也占位置，
// 于是第一个函数调用常常是 1 或 2。透传会把这个稀疏下标交给客户端。
func TestChatStreamToolCallIndexStartsAtZero(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"q\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"\"天气\"}"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}

	var args strings.Builder
	seen := map[int]string{}
	finish := ""
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "tool_use":
			if ev.ToolCall.ID != "" {
				seen[ev.ToolCall.Index] = ev.ToolCall.Name
			}
			args.WriteString(ev.ToolCall.Arguments)
		case "done":
			finish = ev.FinishReason
		}
	}
	if len(seen) != 1 || seen[0] != "lookup" {
		t.Errorf("工具调用下标与名字 = %v（下标要从 0 起，不能透传 output_index=2）", seen)
	}
	if args.String() != `{"q":"天气"}` {
		t.Errorf("拼出的参数 = %q", args.String())
	}
	if finish != "tool_calls" {
		t.Errorf("FinishReason = %q", finish)
	}
}

// 上游可能只发 .done（参数一次性给全），也可能既发增量又发全文。
// 两种都不该让参数被拼两遍。
func TestChatStreamToolArgsNotDuplicated(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name: "只有 done",
			lines: []string{
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f"}}`,
				`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_1","arguments":"{\"a\":1}"}`,
			},
			want: `{"a":1}`,
		},
		{
			name: "增量加全文",
			lines: []string{
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f"}}`,
				`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"a\":"}`,
				`data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"1}"}`,
				`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_1","arguments":"{\"a\":1}"}`,
			},
			want: `{"a":1}`,
		},
		{
			name: "参数只在 output_item.done 里",
			lines: []string{
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f"}}`,
				`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"f","arguments":"{\"a\":1}"}}`,
			},
			want: `{"a":1}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := append(append([]string{}, tc.lines...),
				`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`)
			srv := sseServer(t, lines...)

			events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
				Model: "m", Stream: true, Messages: textRequest().Messages,
			})
			if err != nil {
				t.Fatal(err)
			}
			var args strings.Builder
			for _, ev := range collect(t, events) {
				if ev.Kind == "tool_use" {
					args.WriteString(ev.ToolCall.Arguments)
				}
			}
			if args.String() != tc.want {
				t.Errorf("拼出的参数 = %q，期望 %q", args.String(), tc.want)
			}
		})
	}
}

// custom 工具的参数是自由文本，事件名与字段都与 function 不同。
func TestChatStreamCustomToolCall(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ct_1","call_id":"call_9","name":"shell"}}`,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ct_1","input":"ls -"}`,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ct_1","input":"la"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	var args strings.Builder
	id, name := "", ""
	for _, ev := range collect(t, events) {
		if ev.Kind == "tool_use" {
			if ev.ToolCall.ID != "" {
				id, name = ev.ToolCall.ID, ev.ToolCall.Name
			}
			args.WriteString(ev.ToolCall.Arguments)
		}
	}
	if id != "call_9" || name != "shell" {
		t.Errorf("工具调用 = %q/%q", id, name)
	}
	if args.String() != "ls -la" {
		t.Errorf("参数 = %q", args.String())
	}
}

// 思考摘要不进正文事件：它是模型的中间产物，与用户可见的回答是两件事。
func TestChatStreamSkipsReasoningDeltas(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"先想"}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"答案"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, ev := range collect(t, events) {
		if ev.Kind == "delta" {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "答案" {
		t.Errorf("文本 = %q", text.String())
	}
}

// incomplete 也是终态，结束原因是 length，用量照记（那些 token 已经花了）。
func TestChatStreamIncompleteFinishReason(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"写到一半"}`,
		`data: {"type":"response.incomplete","response":{"id":"r1","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":9}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	finish := ""
	sawUsage := false
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "done":
			finish = ev.FinishReason
		case "usage":
			sawUsage = true
		case "error":
			t.Fatalf("incomplete 不该是错误: %v", ev.Err)
		}
	}
	if finish != "length" {
		t.Errorf("FinishReason = %q", finish)
	}
	if !sawUsage {
		t.Error("incomplete 也要记用量")
	}
}

// 流中途的错误要作为 error 事件发出，且不可重试——
// 重试意味着客户端会看到两段拼接的回答。已经吐出的正文照记费用。
func TestChatStreamMidStreamError(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"部分"}`,
		`data: {"type":"response.failed","response":{"id":"r1","model":"m","status":"failed","error":{"code":"server_error","message":"上游内部错误"}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawText, sawErr, sawDone bool
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "delta":
			sawText = true
		case "error":
			sawErr = true
			if channel.IsRetryable(ev.Err) {
				t.Error("流中途的错误不该可重试")
			}
		case "done":
			sawDone = true
		}
	}
	if !sawText {
		t.Error("已产出的文本仍要发出，它要计费")
	}
	if !sawErr {
		t.Error("应发出错误事件")
	}
	if sawDone {
		t.Error("失败之后不该再补一个正常结束")
	}
}

// 连接直接关闭而没有终态事件：不补 done，也不补 usage。
// 伪造会让「流被截断」看起来像「模型答完了」，而两者在账上完全不同。
func TestChatStreamTruncatedHasNoDoneOrUsage(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"半截"}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 「没有 done、没有 usage」这两条断言，在**一条事件都不发**时也成立。
	// 所以必须同时钉住「已经吐出去的内容照发」——否则这个用例守不住任何
	// 东西，而它想守的是「截断时正文仍然要给客户端」。
	text := ""
	for _, ev := range collect(t, events) {
		if ev.Kind == "done" {
			t.Error("缺少终态事件时不应伪造 done")
		}
		if ev.Kind == "usage" {
			t.Error("没有终态事件就没有用量，不该伪造一个")
		}
		if ev.Kind == "delta" {
			text += ev.Text
		}
	}
	if text != "半截" {
		t.Errorf("截断前已经收到的正文 = %q，期望「半截」——一条事件都不发时上面两条断言也会通过", text)
	}
}

// 终态事件之后有的兼容层还补一个 [DONE]。它表示流结束了，但**不带用量**：
// 那时用量按已收到的增量估算，而不是拿一个 0 当真值。
func TestChatStreamDoneWithoutTerminalEvent(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"x"}`,
		`data: [DONE]`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	sawDone := false
	for _, ev := range collect(t, events) {
		if ev.Kind == "usage" {
			t.Errorf("没有用量可发时不该发一个空用量: %+v", ev.Usage)
		}
		if ev.Kind == "done" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Error("[DONE] 是明确的结束标记，应发出 done")
	}
}
