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

	// 端点那一栏：适配器自述的路径要与它声明的协议算出来的那条一致（注册表
	// 启动时也自检这一条，见 channel.Registry.Register），而**类型名必须落在
	// 协议取值上**——选路按渠道的 type 算这条渠道打哪条端点
	// （channel.Protocol(<type>).Endpoint()，见 internal/channels），两者不同名
	// 时算出来是空串（= 不知道 = 放行），这条渠道的端点判据会静默失效。
	if got := a.Endpoint(); got != channel.EndpointMessages {
		t.Errorf("Endpoint() = %q，期望 %q", got, channel.EndpointMessages)
	}
	if got := channel.Protocol(Type).Endpoint(); got != a.Endpoint() {
		t.Errorf("channel.Protocol(%q).Endpoint() = %q，与适配器自述的 %q 不一致："+
			"Type() 必须与它说的协议取值同名", Type, got, a.Endpoint())
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
	// 输出侧同样要照抄。CompletionTokens 为 0 时 UsageFromRelaykit 会改读
	// OutputTokens，而这条路径上两个字段是同一个来源——漏抄救不回来，
	// 只会静默地少算一次调用的输出。
	if resp.Usage.CompletionTokens != 20 {
		t.Errorf("CompletionTokens = %d，期望 20（来自上游的 output_tokens）", resp.Usage.CompletionTokens)
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
	// 拿到过响应：状态码 200 是真的，要跟着错误带出去。
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if !up.GotResponse || up.StatusCode != http.StatusOK {
		t.Errorf("GotResponse=%v StatusCode=%d，期望 true / %d",
			up.GotResponse, up.StatusCode, http.StatusOK)
	}
	// 错误体要带上上游自己写的那句话：排障时它比「上游返回错误」有用得多，
	// 而它只在 Body 里，不在 Error() 文本里（后者会进日志，见 channel_test）。
	if up.Body != "过载了" {
		t.Errorf("Body = %q，期望上游的错误原文", up.Body)
	}
}

// 「是不是错误体」由两个信号共同判定：type == "error" **或** error 对象存在。
//
// 两个信号各自都可能单独出现：只写 type、或只写 error 对象。只认一种的话，
// 另一种会被当成正常响应——状态码 200、内容为空、用量为零，账上表现为
// 「成功」，没有任何一处会失败。
//
// 两种形态落到 ErrUpstream.Body 上的取值不同，所以按形态逐条断言具体值：
// 只看「有没有报错」的话，认错分支也照样通过。
func TestChatDetectsEmbeddedErrorByEitherField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		// 只有 type：error 是 null，错误文本退到本地文案。
		{name: "只有 type", body: `{"type":"error","error":null}`, want: "上游返回错误"},
		// 只有 error 对象：type 写的是正常响应，错误文本来自 message。
		{name: "只有 error 对象", body: `{"type":"message","error":{"type":"overloaded_error","message":"过载"}}`, want: "过载"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
			if err == nil {
				t.Fatalf("应报错，期望 Body = %q", tc.want)
			}
			var up *channel.ErrUpstream
			if !errors.As(err, &up) {
				t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
			}
			if up.Body != tc.want {
				t.Errorf("Body = %q，期望 %q", up.Body, tc.want)
			}
		})
	}
}

// 错误体里的 message 与 type 各自可能缺：有 message 用它，没有就退到 type，
// 两者都没有才用本地文案。message 是给排障的人看的那句话，被吞掉之后
// 只剩一个分类名。
func TestChatEmbeddedErrorBodyFallback(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "message 优先于 type", body: `{"type":"error","error":{"type":"overloaded_error","message":"过载了"}}`, want: "过载了"},
		{name: "没有 message 用 type", body: `{"type":"error","error":{"type":"overloaded_error"}}`, want: "overloaded_error"},
		{name: "两者都没有", body: `{"type":"error","error":{}}`, want: "上游返回错误"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
			if err == nil {
				t.Fatal("应报错")
			}
			var up *channel.ErrUpstream
			if !errors.As(err, &up) {
				t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
			}
			if up.Body != tc.want {
				t.Errorf("Body = %q，期望 %q", up.Body, tc.want)
			}
		})
	}
}

// 200 里的错误对象是**限流**形态时，这次失败值得换渠道重试。
//
// Anthropic 把分类放在 type 上（这个错误对象里没有 code），与 OpenAI 兼容
// 上游把分类放在 code 上是同一件事的两种写法。三家共用一处判据
// （channel.IsEmbeddedErrorRetryable），所以这条与另外两家那两条钉的是同一件事：
// 同一个 429 语义不该因为走哪条入口而分岔。
func TestChatEmbeddedRateLimitIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w,
			`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limit reached"}}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("200 里的 error 对象应当被识别")
	}
	if !channel.IsRetryable(err) {
		t.Error("限流是上游这一侧暂时答不上来，换一个渠道可能成功，应当可重试")
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
	// Anthropic 的 output_tokens 含扩展思考的 token，且协议不单独报推理量。
	if !usage.CompletionIncludesReasoning {
		t.Error("Anthropic 口径的 output_tokens 含思考 token，应声明「已含」")
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

// 输入的用量在 message_start 就到了，所以适配器在那里**另外**发一个
// usage_input：调用方对外发的第一帧里填的就是它，而完整的用量要到流末尾才有。
//
// 单独发而不是并进后面那个 usage：它只有输入、没有输出，被当成完整的会把
// 输出算成 0（见 channel.StreamEvent 的 Usage）。
func TestChatStreamEmitsInputUsageAtMessageStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":30}}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}`,
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
		t.Fatal(err)
	}

	var kinds []string
	var early, full *channel.Usage
	for ev := range events {
		kinds = append(kinds, ev.Kind)
		switch ev.Kind {
		case "usage_input":
			u := *ev.Usage
			early = &u
		case "usage":
			u := *ev.Usage
			full = &u
		}
	}

	// 位置本身就是判据：晚于第一个内容事件到达，调用方的第一帧就用不上它了。
	// 第一条是流的信封事件（start，带状态码），它不参与内容顺序——输入用量
	// 必须紧跟在它后面、早于任何正文分片。
	if len(kinds) < 2 || kinds[0] != "start" || kinds[1] != "usage_input" {
		t.Fatalf("事件序列 = %v，期望 start 之后紧跟 usage_input", kinds)
	}
	if early == nil {
		t.Fatal("应发出 usage_input 事件")
	}
	// 与末尾那个完整用量同一口径：输入 100 + 读缓存 30。
	if early.PromptTokens != 130 {
		t.Errorf("usage_input 的 PromptTokens = %d，期望 130（与完整用量同一口径）", early.PromptTokens)
	}
	if early.CompletionTokens != 0 {
		t.Errorf("usage_input 的 CompletionTokens = %d，期望 0（那时输出还没产生）", early.CompletionTokens)
	}
	if full == nil || full.PromptTokens != 130 || full.CompletionTokens != 25 {
		t.Errorf("完整用量 = %+v，期望输入 130、输出 25", full)
	}
}

// 上游在自己的 message_start 里没报输入用量时**不发** usage_input。
//
// 0 的含义是「这一帧没带」，不是「输入真的是 0」：把它发出去，调用方会拿这个
// 0 去填第一帧——那比本地估算还差。
func TestChatStreamWithoutInputUsageEmitsNoEarlyUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg-1","usage":{"output_tokens":1}}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":77,"output_tokens":25}}`,
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
		t.Fatal(err)
	}

	var early *channel.Usage
	var full *channel.Usage
	for ev := range events {
		switch ev.Kind {
		case "usage_input":
			u := *ev.Usage
			early = &u
		case "usage":
			u := *ev.Usage
			full = &u
		}
	}

	if early != nil {
		t.Errorf("message_start 没报输入用量时不该发 usage_input，得到 %+v", early)
	}
	// 输入在后面的 message_delta 里才到，完整用量照发——这条只是「没提前发」，
	// 不是「不发」。
	if full == nil || full.PromptTokens != 77 || full.CompletionTokens != 25 {
		t.Errorf("完整用量 = %+v，期望输入 77、输出 25", full)
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
			// 流里的错误体同样是 {"error":{"type","message"}}：有 message
			// 就用它，不该退到分类名上。
			var up *channel.ErrUpstream
			if !errors.As(ev.Err, &up) {
				t.Fatalf("期望 *channel.ErrUpstream，得到 %v", ev.Err)
			}
			if up.Body != "过载" {
				t.Errorf("Body = %q，期望上游的错误原文", up.Body)
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
//
// 空行与 ":" 开头的那道提前 continue 是冗余的：这两类行既不以 "event:" 也不以
// "data:" 开头，落到后面同样 continue。所以把那里第一道判据里的 || 取反成 &&
// 是等价变异——这两类行走不走第一道判据都一样被跳过，真正决定它们不变成
// 事件的是后缀两道前缀判据，这条用例保证的就是那两道的行为。
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
		// start 是这条流的信封事件（带状态码），不属于内容；这里要钉的是
		// ping、注释行、坏 JSON 与 content_block_stop 都不变成事件。
		if ev.Kind != "start" && ev.Kind != "done" && ev.Kind != "usage" {
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

// 单帧可能很大：长文本的一次性增量、或一整段工具参数。默认的 64KB 上限会把
// 合法响应判为读取失败，所以这里送一帧 100KB 的——它落在 bufio.Scanner 的
// 初始缓冲（64KB）之上，正是「要不要扩容」的分界。
//
// 这一条要区分的是 maxTokenSize（扫描上限），不是初始容量：初始容量被改成
// 0 时 Scanner 会从 4KB 起自己翻倍扩到同一个上限，能读到的 token 集合不变，
// 只是多分配几次（那是等价变异，见 stream.go 的 scanner.Buffer 一行）。
func TestChatStreamAcceptsEventLargerThanDefaultBuffer(t *testing.T) {
	text := strings.Repeat("a", 100*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	var sawDone, sawErr bool
	for ev := range events {
		switch ev.Kind {
		case "delta":
			got.WriteString(ev.Text)
		case "done":
			sawDone = true
		case "error":
			sawErr = true
		}
	}
	if sawErr {
		t.Error("合法的长帧不该被当成读取失败")
	}
	if got.Len() != len(text) {
		t.Errorf("文本长度 = %d，期望 %d", got.Len(), len(text))
	}
	if !sawDone {
		t.Error("长帧之后仍应读到 message_stop")
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
	// 一个 HTTP 响应都没有过。下游靠 GotResponse 把这一档与
	// 「拿到了响应但用不了」分开。
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if up.GotResponse || up.StatusCode != 0 {
		t.Errorf("GotResponse=%v StatusCode=%d，连不上时都该是零值",
			up.GotResponse, up.StatusCode)
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

// failingReader 让 scanner 在读第一行时拿到错误，用来走「读流失败」那条路。
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// runPump 直接驱动 pump：读流失败这条路径要中断底层的连接才造得出来，
// 在 httptest 里不稳定，而它的判据属于 pump 自己，与 HTTP 那一层无关。
func runPump(ctx context.Context, body io.Reader) []channel.StreamEvent {
	events := make(chan channel.StreamEvent, 8)
	newAdapter().pump(ctx, http.StatusOK, body, events)
	close(events)
	var out []channel.StreamEvent
	for ev := range events {
		out = append(out, ev)
	}
	return out
}

// 读流时底层出错：这条错误要作为 error 事件发出去，而不是静默结束——
// 静默结束会让「流断了」读成「模型答完了」，而已经发出的内容照常计费。
func TestPumpEmitsScannerError(t *testing.T) {
	readErr := errors.New("连接断了")
	events := runPump(context.Background(), failingReader{readErr})
	if len(events) != 1 {
		t.Fatalf("事件 = %+v，期望恰好一个 error", events)
	}
	if events[0].Kind != "error" {
		t.Fatalf("Kind = %q，期望 error", events[0].Kind)
	}
	var up *channel.ErrUpstream
	if !errors.As(events[0].Err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", events[0].Err)
	}
	// 响应头已经回来了，所以这是「拿到过响应」的失败，状态码要照实带上。
	if !up.GotResponse || up.StatusCode != http.StatusOK {
		t.Errorf("GotResponse=%v StatusCode=%d，期望 true / %d",
			up.GotResponse, up.StatusCode, http.StatusOK)
	}
	if !errors.Is(up.Err, readErr) {
		t.Errorf("底层错误 = %v，期望 %v", up.Err, readErr)
	}
	if up.Retryable {
		t.Error("流已经开始，读失败不该判成可重试：重试会让客户端看到两段拼接的回答")
	}
}

// ctx 已经取消时，读流的错误不再发出去：没人会收到它，而且它来自
// 「我们自己不读了」，不是上游的失败。
func TestPumpSilentWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if events := runPump(ctx, failingReader{errors.New("连接断了")}); len(events) != 0 {
		t.Errorf("ctx 已取消时不该产生事件，得到 %+v", events)
	}
}

// readUsage 是「取到就覆盖、取不到就不动」：输入在 message_start 给出，
// 输出在 message_delta 给出，两次调用的字段互不重叠，但后一次可能带一个
// 只含 output_tokens 的对象——直接赋值会把已经读到的输入清零。
//
// 0 与「没带」在计费上是同一件事（都不增加用量），所以 0 同样不覆盖。
func TestReadUsageDoesNotOverwriteWithZero(t *testing.T) {
	var input, output, cacheRead, cacheWrite int64
	readUsage(map[string]any{
		"input_tokens":                float64(100),
		"output_tokens":               float64(7),
		"cache_read_input_tokens":     float64(3),
		"cache_creation_input_tokens": float64(9),
	}, &input, &output, &cacheRead, &cacheWrite)
	if input != 100 || output != 7 || cacheRead != 3 || cacheWrite != 9 {
		t.Fatalf("首帧用量 = (%d, %d, %d, %d)，期望 (100, 7, 3, 9)",
			input, output, cacheRead, cacheWrite)
	}

	// 只带输出、且是 0 的后续事件：四个数都不动。
	readUsage(map[string]any{"output_tokens": float64(0)}, &input, &output, &cacheRead, &cacheWrite)
	if input != 100 || output != 7 || cacheRead != 3 || cacheWrite != 9 {
		t.Errorf("后续事件把已累计的用量改了 = (%d, %d, %d, %d)",
			input, output, cacheRead, cacheWrite)
	}

	// 四个字段都是 0：与「没带」同样不覆盖。
	readUsage(map[string]any{
		"input_tokens":                float64(0),
		"output_tokens":               float64(0),
		"cache_read_input_tokens":     float64(0),
		"cache_creation_input_tokens": float64(0),
	}, &input, &output, &cacheRead, &cacheWrite)
	if input != 100 || output != 7 || cacheRead != 3 || cacheWrite != 9 {
		t.Errorf("全零的用量不该覆盖 = (%d, %d, %d, %d)",
			input, output, cacheRead, cacheWrite)
	}

	// 不是 map（含类型断言成功但为 nil 的 map）：没有字段可读，一个数都不该动。
	//
	// 这里的 || 取反成 && 是等价变异：!ok 时 m 本来就是 nil，&& 那一侧为真，
	// 一样提前返回；ok 且 m 为 nil 时会继续往下读，而 nil map 的每次读取都给
	// nil、numberFrom 返回 0，四处赋值都要求 v > 0——状态不变。
	readUsage(nil, &input, &output, &cacheRead, &cacheWrite)
	readUsage(map[string]any(nil), &input, &output, &cacheRead, &cacheWrite)
	if input != 100 || output != 7 || cacheRead != 3 || cacheWrite != 9 {
		t.Errorf("不是 map 的用法不该改动累计值 = (%d, %d, %d, %d)",
			input, output, cacheRead, cacheWrite)
	}
}
