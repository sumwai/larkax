package anthropicadapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

func testChannel(baseURL string) channel.Channel {
	return channel.Channel{
		ID:         2,
		Name:       "test",
		Type:       Type,
		BaseURL:    baseURL,
		Protocol:   channel.ProtocolAnthropic,
		Credential: "placeholder-credential",
	}
}

func newAdapter() *Adapter { return New(5 * time.Second) }

func TestTypeAndProtocol(t *testing.T) {
	a := newAdapter()
	if a.Type() != Type {
		t.Errorf("Type() = %q", a.Type())
	}
	if a.Protocol() != channel.ProtocolAnthropic {
		t.Errorf("Protocol() = %q", a.Protocol())
	}
}

// Anthropic 用 x-api-key 而不是 Bearer。写成 Bearer 会得到 401，
// 而上游不会说「你用错头了」。这是两套协议里最容易复制粘贴搞错的一处。
func TestHeadersUseAPIKeyNotBearer(t *testing.T) {
	var gotAPIKey, gotAuth, gotVersion, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()

	if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{
		Model:     "claude-sonnet-4",
		MaxTokens: 100,
		Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	}); err != nil {
		t.Fatalf("出错: %v", err)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("路径 = %q，期望 /v1/messages", gotPath)
	}
	if gotAPIKey != "placeholder-credential" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Errorf("不该设置 Authorization，实际为 %q", gotAuth)
	}
	if gotVersion != AnthropicVersion {
		t.Errorf("anthropic-version = %q，期望 %q", gotVersion, AnthropicVersion)
	}
}

func TestChatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-20250101",
			"content":[{"type":"text","text":"回答"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":100,"output_tokens":20,
			         "cache_read_input_tokens":30,"cache_creation_input_tokens":10}
		}`)
	}))
	defer srv.Close()

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "claude-sonnet-4"})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if resp.UpstreamModel != "claude-sonnet-4-20250101" {
		t.Errorf("UpstreamModel = %q", resp.UpstreamModel)
	}
	if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "回答" {
		t.Errorf("内容 = %+v", resp.Message.Content)
	}
	// Anthropic 的 input_tokens 不含缓存，归一化后必须补上，否则输入被低估
	if resp.Usage.PromptTokens != 140 {
		t.Errorf("PromptTokens = %d，期望 140（100 + 30 读缓存 + 10 写缓存）", resp.Usage.PromptTokens)
	}
	if resp.Usage.CacheReadTokens != 30 || resp.Usage.CacheWriteTokens != 10 {
		t.Errorf("缓存明细 = %+v", resp.Usage)
	}
}

func TestChatWithToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"上海"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer srv.Close()

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	var found bool
	for _, b := range resp.Message.Content {
		if b.Type == "tool_use" {
			found = true
			if b.ToolUseID != "tu_1" || b.ToolName != "get_weather" {
				t.Errorf("工具块 = %+v", b)
			}
			if b.ToolInput["city"] != "上海" {
				t.Errorf("工具参数 = %+v", b.ToolInput)
			}
		}
	}
	if !found {
		t.Fatalf("未解析出工具调用: %+v", resp.Message.Content)
	}
}

// 工具参数不是合法 JSON 时保留原文而不是丢弃整条响应——
// 丢掉一个参数比丢掉整次调用轻。
func TestChatMalformedToolInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"id":"msg_1","type":"message",
			"content":[{"type":"tool_use","id":"t","name":"f","input":"{半截"}]
		}`)
	}))
	defer srv.Close()

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err != nil {
		t.Fatalf("半截参数不该让整条响应失败: %v", err)
	}
	if len(resp.Message.Content) == 0 {
		t.Fatal("内容为空")
	}
	if resp.Message.Content[0].ToolInput["_raw"] != "{半截" {
		t.Errorf("原文应保留: %+v", resp.Message.Content[0].ToolInput)
	}
}

// Anthropic 的错误体是 {"type":"error","error":{...}}，而 HTTP 状态码可能是 200。
// 不检查会把失败当成正常响应，于是账上表现为「成功但用量为零」。
func TestChatDetectsEmbeddedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"过载了"}}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("200 里的错误体应当被识别")
	}
	if !channel.IsRetryable(err) {
		t.Error("overloaded_error 应可重试：换渠道可能成功")
	}
}

func TestEmbeddedErrorRetryability(t *testing.T) {
	cases := map[string]bool{
		"overloaded_error":      true,
		"rate_limit_error":      true,
		"api_error":             true,
		"invalid_request_error": false,
		"authentication_error":  false,
		"permission_error":      false,
		"not_found_error":       false,
		"request_too_large":     false,
		"没见过的类型":                false,
	}
	for typ, wantRetry := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"`+typ+`","message":"x"}}`)
		}))
		_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
		srv.Close()

		if err == nil {
			t.Errorf("%s：应报错", typ)
			continue
		}
		if got := channel.IsRetryable(err); got != wantRetry {
			t.Errorf("%s：Retryable = %v，期望 %v", typ, got, wantRetry)
		}
	}
}

func TestChatStatusHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("401 应报错")
	}
	if channel.IsRetryable(err) {
		t.Error("401 不该可重试")
	}
}

// Claude 需要 max_tokens，缺了会被拒。补一个保守的默认值，
// 而不是让「客户端没传」变成一次失败的请求。
func TestMissingMaxTokensGetsDefault(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	req := &channel.Request{Model: "m"}
	if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), req); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if !strings.Contains(gotBody, `"max_tokens":4096`) {
		t.Errorf("请求体未补上 max_tokens: %s", gotBody)
	}
}

func TestQuotaIsUnsupported(t *testing.T) {
	if _, err := newAdapter().Quota(context.Background(), testChannel("https://api.example.com")); !errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("Quota 应返回 ErrUnsupported，得到 %v", err)
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-4"},{"id":"claude-opus-4"}]}`)
	}))
	defer srv.Close()

	models, err := newAdapter().ListModels(context.Background(), testChannel(srv.URL))
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("模型列表 = %v", models)
	}
}

func TestChatStreamRejectsBeforeReturning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("上游拒绝时应在返回前报错")
	}
	if channel.IsRetryable(err) {
		t.Error("401 不该可重试")
	}
}

// 用量跨两个事件：输入在 message_start、输出在 message_delta。
// 只读其中一个会得到「有输入没输出」或反之，而两者都是算钱的依据。
func TestChatStreamUsageAccumulatesAcrossEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":30}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`,
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		} {
			_, _ = io.WriteString(w, line+"\n")
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	var text strings.Builder
	var usage *channel.Usage
	var done *channel.StreamEvent
	for ev := range events {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "usage":
			u := *ev.Usage
			usage = &u
		case "done":
			e := ev
			done = &e
		case "error":
			t.Fatalf("不该有错误: %v", ev.Err)
		}
	}

	if text.String() != "你好" {
		t.Errorf("文本 = %q", text.String())
	}
	if usage == nil {
		t.Fatal("应发出 usage 事件")
	}
	// 输入 100 + 读缓存 30 = 130（Anthropic 的 input_tokens 不含缓存）
	if usage.PromptTokens != 130 {
		t.Errorf("PromptTokens = %d，期望 130", usage.PromptTokens)
	}
	if usage.CompletionTokens != 25 {
		t.Errorf("CompletionTokens = %d，期望 25（来自 message_delta）", usage.CompletionTokens)
	}
	if done == nil || done.FinishReason != "end_turn" {
		t.Errorf("done 事件 = %+v", done)
	}
}

// 后一个 usage 对象只含 output_tokens，直接赋值会把已读到的输入清零。
func TestChatStreamUsageIsNotOverwrittenByLaterPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"usage":{"input_tokens":77}}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"message_delta","usage":{"output_tokens":5}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var usage *channel.Usage
	for ev := range events {
		if ev.Kind == "usage" {
			u := *ev.Usage
			usage = &u
		}
	}
	if usage == nil {
		t.Fatal("应发出 usage 事件")
	}
	if usage.PromptTokens != 77 {
		t.Errorf("PromptTokens = %d，期望 77（不被后续部分 usage 清零）", usage.PromptTokens)
	}
	if usage.CompletionTokens != 5 {
		t.Errorf("CompletionTokens = %d", usage.CompletionTokens)
	}
}

// 工具调用分两处给出：名字与 id 在 content_block_start，参数在后续 delta 里。
func TestChatStreamToolUseAssembly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range []string{
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"f"}}`,
			``,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
			``,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
			``,
			`data: {"type":"message_stop"}`,
			``,
		} {
			_, _ = io.WriteString(w, line+"\n")
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}

	var id, name string
	var args strings.Builder
	for ev := range events {
		if ev.Kind == "tool_use" {
			if ev.ToolCall.ID != "" {
				id = ev.ToolCall.ID
			}
			if ev.ToolCall.Name != "" {
				name = ev.ToolCall.Name
			}
			args.WriteString(ev.ToolCall.Arguments)
		}
	}
	if id != "tu_1" || name != "f" {
		t.Errorf("工具标识 =(id=%q, name=%q)", id, name)
	}
	if args.String() != `{"a":1}` {
		t.Errorf("拼出的参数 = %q", args.String())
	}
}

// 思考内容不进正文事件：它是模型的中间产物，与用户可见的回答是两件事。
func TestChatStreamThinkingNotInText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"思考中"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"答案"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for ev := range events {
		if ev.Kind == "delta" {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "答案" {
		t.Errorf("文本 = %q，思考内容不该混进来", text.String())
	}
}

func TestChatStreamMidStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"部分"}}`+"\n\n")
		_, _ = io.WriteString(w, `event: error`+"\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"过载"}}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawText, sawErr bool
	for ev := range events {
		switch ev.Kind {
		case "delta":
			sawText = true
		case "error":
			sawErr = true
			if channel.IsRetryable(ev.Err) {
				t.Error("流中途的错误不该可重试：重试会让客户端看到两段拼接的回答")
			}
		}
	}
	if !sawText {
		t.Error("已产出的文本仍要发出，它要计费")
	}
	if !sawErr {
		t.Error("应发出错误事件")
	}
}

// 心跳与块结束标记对统一事件流没有对应物，不该产生任何事件。
func TestChatStreamSkipsPingAndBlockStop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range []string{
			`event: ping`,
			`data: {"type":"ping"}`,
			``,
			`: 注释行`,
			``,
			`data: {坏 JSON`,
			``,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`data: {"type":"message_stop"}`,
			``,
		} {
			_, _ = io.WriteString(w, line+"\n")
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for ev := range events {
		count++
		if ev.Kind != "done" && ev.Kind != "usage" {
			t.Errorf("意外事件: %+v", ev)
		}
	}
	if count == 0 {
		t.Error("应至少发出 done 事件")
	}
}

// 连接直接关闭而没有 message_stop：不补 done 事件。
// 伪造正常结束会让「流被截断」看起来像「模型答完了」。
func TestChatStreamTruncatedHasNoDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半截"}}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Kind == "done" {
			t.Error("缺少 message_stop 时不应伪造 done 事件")
		}
	}
}

// 只发 data: 不发 event: 行也要能解析——载荷里的 type 更可信。
func TestChatStreamWithoutEventLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var sawDone bool
	for ev := range events {
		if ev.Kind == "delta" {
			text.WriteString(ev.Text)
		}
		if ev.Kind == "done" {
			sawDone = true
		}
	}
	if text.String() != "x" || !sawDone {
		t.Errorf("文本 = %q，done = %v", text.String(), sawDone)
	}
}

func TestChatNetworkFailureIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(url), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("连不上应报错")
	}
	if !channel.IsRetryable(err) {
		t.Error("网络层失败应可重试")
	}
}

// 没有内容的响应（例如只有 stop_reason）不该让解析失败。
func TestChatHandlesEmptyContent(t *testing.T) {
	bodies := map[string]string{
		"空数组":       `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":0}}`,
		"缺 content": `{"type":"message","usage":{"input_tokens":1,"output_tokens":0}}`,
		"缺 usage":   `{"type":"message","content":[{"type":"text","text":"x"}]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"}); err != nil {
				t.Errorf("这种响应形态不该报错: %v", err)
			}
		})
	}
}

// 插件加的头要能到上游。
func TestCustomHeadersReachUpstream(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{
		Model:   "m",
		Headers: map[string]string{"X-Custom-Tenant": "t1"},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Get("X-Custom-Tenant") != "t1" {
		t.Errorf("自定义头没到上游: %v", got)
	}
}

// Anthropic 的鉴权头是 x-api-key，它同样不能被覆盖。
func TestCustomHeadersCannotOverrideAPIKey(t *testing.T) {
	var gotKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m",
		Headers: map[string]string{
			"x-api-key":         "冒充的钥匙",
			"X-API-KEY":         "另一种大小写",
			"anthropic-version": "1999-01-01",
		},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if gotKey != "placeholder-credential" {
		t.Errorf("x-api-key 被覆盖了，实际 %q", gotKey)
	}
	if gotVersion != AnthropicVersion {
		t.Errorf("anthropic-version 被覆盖了，实际 %q", gotVersion)
	}
}
