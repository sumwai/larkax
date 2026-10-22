package responsesadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/observe"
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

func newAdapter() *Adapter { return NewWithClient(httpx.Client(5 * time.Second)) }

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

// captureLogs 把进程日志器换成写进缓冲区的测试日志器（debug 级），返回那个
// 缓冲区。
//
// 日志器是**进程级**的（observe.FromContext 取的就是 slog.Default()），所以用完
// 必须还原。本包的用例都不并行跑（没有 t.Parallel），换默认日志器因此是安全的。
//
// 用例靠它把「适配器认得这个事件」变成可观察的：认得的事件记 debug、未归类的
// 记 warn，而两者都不转发内容——只看正文是分不出来的。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(observe.NewLoggerWithWriter(&buf, "debug", "text", false))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
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

	// 端点那一栏：适配器自述的路径要与它声明的协议算出来的那条一致（注册表
	// 启动时也自检这一条，见 channel.Registry.Register），而**类型名必须落在
	// 协议取值上**——选路按渠道的 type 算这条渠道打哪条端点
	// （channel.Protocol(<type>).Endpoint()，见 internal/channels），两者不同名
	// 时算出来是空串（= 不知道 = 放行），这条渠道的端点判据会静默失效。
	if got := a.Endpoint(); got != channel.EndpointResponses {
		t.Errorf("Endpoint() = %q，期望 %q", got, channel.EndpointResponses)
	}
	if got := channel.Protocol(Type).Endpoint(); got != a.Endpoint() {
		t.Errorf("channel.Protocol(%q).Endpoint() = %q，与适配器自述的 %q 不一致："+
			"Type() 必须与它说的协议取值同名", Type, got, a.Endpoint())
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

// 真上游的非流式形状：思考正文在 output[].content[].reasoning_text 里，而
// summary 是**空数组**；而且 reasoning 项排在 message 项**之后**。
//
// 夹具原来造的是 summary[].summary_text 且 reasoning 在前，适配器两种都取对了
// ——但那是碰巧一致：没有任何用例证明过「换一种形状它还取对」。这条用例把真形状
// 钉住，顺序也一并钉住（正文按 type 挑，不是按位置挑）。
func TestChatRealShapeReasoningAfterMessage(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"resp_1","model":"m","status":"completed",
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"答案"}]},
			{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"思考内容"}]}
		],
		"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,
			"input_tokens_details":{"cached_tokens":6}}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(resp.Message.Content) != 1 || resp.Message.Content[0].Text != "答案" {
		t.Errorf("内容 = %+v（reasoning 项排在 message 之后也一样不是可见输出）", resp.Message.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}

	// 正面钉住「夹具这个形状真的被解析了」：思考正文落在 Raw 里，说明适配器读的
	// 是 content[].reasoning_text 而不是 summary[]。缺了这条，上面那条断言在
	// 「reasoning 项整条没解析出来」时也会通过——那样这条用例就没在测真形状。
	raw, err := json.Marshal(resp.Raw)
	if err != nil {
		t.Fatalf("序列化 Raw 失败: %v", err)
	}
	if !strings.Contains(string(raw), "reasoning_text") || !strings.Contains(string(raw), "思考内容") {
		t.Errorf("夹具里的 reasoning_text 没被解析出来，这条用例就没在测真形状：%s", raw)
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

// 真上游的 input_tokens_details **只有** cached_tokens，从不发
// cache_write_tokens。
//
// 上面那几条用例都带了这个字段，钉的是「字段在时怎么读」；这一条钉「字段不在
// 就是 0」。两者混了的话，账单上会多出一笔并不存在的缓存写入——而响应形状、
// 甚至缓存读那一项都完全正常，只有金额不对。
func TestChatUsageRealShapeWithoutCacheWrite(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, `{
		"id":"resp_1","model":"m","status":"completed","output":[],
		"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,
			"input_tokens_details":{"cached_tokens":30},
			"output_tokens_details":{"reasoning_tokens":25}}
	}`)

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d（上游没报这个字段，落到账上就该是 0）", resp.Usage.CacheWriteTokens)
	}
	got := billing.FromChannelUsage(resp.Usage)
	// 100 = 30 缓存读 + 70 普通输入；缓存写入那一项应当是 0。
	if got.PromptTokens != 70 || got.CacheReadTokens != 30 || got.CacheWriteTokens != 0 {
		t.Errorf("计费口径 = %+v，期望普通输入 70、缓存读 30、缓存写 0", got)
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
	// 拿到过响应：状态码 200 是真的，要跟着错误带出去。
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if !up.GotResponse || up.StatusCode != http.StatusOK {
		t.Errorf("GotResponse=%v StatusCode=%d，期望 true / %d",
			up.GotResponse, up.StatusCode, http.StatusOK)
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

// 200 里的错误对象是**限流**形态时，这次失败值得换渠道重试。
//
// 这条用的是 Responses 真实的失败形状：`status: "failed"` 带一个 error 对象。
// 该不该换渠道再试判在 error 的 type / code 上——`status` 只说明「这一次失败
// 了」，答不了「是不是上游这一侧暂时答不上来」。判成不可重试的后果有两条
// （见 internal/gateway 的 channelAtFault）：路由层不换下一个候选，一次本可
// 成功的请求直接失败；这个渠道也不记失败，正在限流的它照旧拿流量。
func TestChatEmbeddedRateLimitIsRetryable(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK,
		`{"id":"r1","model":"m","status":"failed",`+
			`"error":{"code":"rate_limit_exceeded","message":"Rate limit reached"},"output":[]}`)

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), textRequest())
	if err == nil {
		t.Fatal("200 里的 error 对象应当被识别")
	}
	if !channel.IsRetryable(err) {
		t.Error("限流是上游这一侧暂时答不上来，换一个渠道可能成功，应当可重试")
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
	// 这一条里的 error.code 是 server_error：上游这一侧自己出错了，换一个渠道
	// 可能成功，所以**可重试**。「上游明确说这次失败了」不等于「不可重试」——
	// 不可重试指的是「换到别的渠道也是同一个错」（参数、鉴权那一类），而判据
	// 在 error 的 type / code 上（见 channel.IsEmbeddedErrorRetryable）。
	//
	// 这里原先断言的是反方向，而当时那个 false 并不是「看过 server_error 之后
	// 判它不该重试」：这一家对**所有**内嵌错误一律给 false，压根没看 error 的
	// type / code。所以那条断言的覆盖面其实是「一律不可重试」，而这个形状
	// （failed 带一个 error 对象）正是 Responses 的失败终态——照它判，限流也
	// 永远记成不可重试：既不换下一个候选，也不记这个渠道正在限流。
	if !channel.IsRetryable(err) {
		t.Error("server_error 是上游这一侧出的错，应当可重试")
	}
	// 这一条与上面那条同路：拿到了响应，状态码 200 要带出去。
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if !up.GotResponse || up.StatusCode != http.StatusOK {
		t.Errorf("GotResponse=%v StatusCode=%d，期望 true / %d",
			up.GotResponse, up.StatusCode, http.StatusOK)
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

// reasoningEventNames 是适配器认得并决定不转发的思考类事件名。
//
// 前四个来自 OpenAI 的 Responses 事件表（openapi 里 ResponseStreamEvent 的
// ResponseReasoningSummaryText/Text 两个 delta 与两个 done）；后两个是上游在用的
// 名字，不在那张表里。
var reasoningEventNames = []string{
	"response.reasoning_summary_text.delta",
	"response.reasoning_summary_text.done",
	"response.reasoning_text.delta",
	"response.reasoning_text.done",
	"response.reasoning.delta",
	"response.reasoning.done",
}

// 思考摘要不进正文事件：它是模型的中间产物，与用户可见的回答是两件事。
// 六个名字逐个走一遍，每个都配一次真实载荷。
func TestChatStreamSkipsReasoningDeltas(t *testing.T) {
	for _, name := range reasoningEventNames {
		t.Run(name, func(t *testing.T) {
			srv := sseServer(t,
				`data: {"type":"`+name+`","output_index":0,"delta":"先想"}`,
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
		})
	}
}

// hasLogLine 判断日志里有没有一行同时含这些片段。
//
// 逐行而不是全文匹配：slog 把一条日志写在一行上，而这里要问的正是「某个事件名
// 与某句话出现在**同一条**日志里」——全文匹配会把「A 出现在这一行、B 出现在另
// 一行」也算命中。
func hasLogLine(logs string, parts ...string) bool {
	for _, line := range strings.Split(logs, "\n") {
		matched := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// 上面那条用例只说明「思考内容没进正文」——而适配器**什么都不做**时它也成立，
// 所以它证明不了「适配器认得这个名字」。这条用例钉的是认得：思考那一支、无匹配
// 分支里的已知事件、无匹配分支里的未归类事件**三处都不转发内容**，只有日志能把
// 它们分开，所以这里按日志钉。
//
// 为什么值得钉：上游发的是 response.reasoning.delta / .done，而适配器认的名字里
// 原本没有它们，它们落到无匹配分支，行为上「碰巧对」。碰巧对不是分支在管——哪天
// 有人把无匹配分支改成「未知事件即报错」，这条流立刻就断；反过来，如果上游把正文
// 放进这个事件名，我们会静默丢掉正文，而日志里一个字都没有。
//
// 判据取「那条日志得是思考这一支的」，而不是「日志里有这个事件名」：后者在名字
// 只留在 ignorableResponsesEvents 表里、case 被人删掉时照样通过。
func TestChatStreamReasoningEventsAreRecognized(t *testing.T) {
	logs := captureLogs(t)

	lines := []string{
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant"}}`,
	}
	for _, name := range reasoningEventNames {
		lines = append(lines, `data: {"type":"`+name+`","output_index":0,"delta":"思考"}`)
	}
	lines = append(lines,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"答案"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
	srv := sseServer(t, lines...)

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

	for _, name := range reasoningEventNames {
		if !hasLogLine(logs.String(), "跳过思考类流式事件", "event="+name) {
			t.Errorf("日志里没有「%s 落进思考那一支」：\n%s", name, logs.String())
		}
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("有事件被记成「未归类」：\n%s", logs.String())
	}
}

// 未归类的事件必须留下痕迹，且不改变流的走向。
//
// 今天的行为是「无匹配分支静默忽略」：真出了丢正文的事，日志里一个字都没有。
// 这条用例钉三件事：记一条 warn（只记事件名）、不转发它的 delta、不中断流——
// 已经吐出去的正文收不回来，而一个陌生的事件名不该让整条回答失败（失败在上层
// 看起来是「上游坏了」，会去换渠道重试，于是客户端收到两段拼接的回答）。
func TestChatStreamUnknownEventIsLoggedAndIgnored(t *testing.T) {
	logs := captureLogs(t)
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"答"}`,
		`data: {"type":"response.some_future_event","output_index":0,"delta":"未知内容"}`,
		`data: {"type":"response.some_future_event","output_index":0,"delta":"又一段"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	sawUsage, sawDone := false, false
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "usage":
			sawUsage = true
		case "done":
			sawDone = true
		case "error":
			t.Fatalf("陌生的事件名不该让这条流报错: %v", ev.Err)
		}
	}
	if text.String() != "答" {
		t.Errorf("正文 = %q：未归类事件的 delta 不该转发（它可能就是思考内容）", text.String())
	}
	if !sawUsage || !sawDone {
		t.Errorf("usage=%v done=%v：一个陌生的事件名不该让这条流失去用量与结束标记", sawUsage, sawDone)
	}

	log := logs.String()
	if !hasLogLine(log, "未归类", "event=response.some_future_event") {
		t.Errorf("未归类的事件要记一条 warn：\n%s", log)
	}
	if hasLogLine(log, "跳过已知", "event=response.some_future_event") {
		t.Errorf("这个事件名不在已知表里，不该被记成「已知且不必转发」：\n%s", log)
	}
	if n := strings.Count(log, "response.some_future_event"); n != 1 {
		t.Errorf("同类事件只记一次（逐条打会被每个 token 一个的事件淹掉），实际 %d 次：\n%s", n, log)
	}
	if strings.Contains(log, "未知内容") {
		t.Errorf("日志里不该出现事件的载荷——那可能就是用户的正文：\n%s", log)
	}
}

// 引用标注要转发，而且要带上它自己的内容。
//
// 非流式那条路把 output[].content[].annotations 交给调用方，流式这条以前把它
// 当「已知且不用转发」的噪音吞掉——同一个协议的两条路口径不一致，而用户看到
// 的是「流式里没有引用」。这条用例钉住：标注以独立事件发出，内容原样。
//
// 判据落在**事件的内容**上而不是「有事件流出」：把这一支的 case 删掉，它会落进
// 无匹配分支——那里同样不转发、且这条流里不会有一条 annotate 事件，所以
// 「出现了一条 annotation 事件」本身就足以区分两条分支（见 LESSONS.md 第 33 条）。
func TestChatStreamAnnotationIsForwarded(t *testing.T) {
	logs := captureLogs(t)
	srv := sseServer(t,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","delta":"见 [1]"}`,
		`data: {"type":"response.output_text.annotation.added","output_index":0,"item_id":"msg_1","annotation_index":0,`+
			`"annotation":{"type":"url_citation","url":"https://example.com","start_index":0,"end_index":1}}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	var annotations []map[string]any
	sawDone := false
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "annotation":
			annotations = append(annotations, ev.Annotations...)
		case "done":
			sawDone = true
		case "error":
			t.Fatalf("标注不该让这条流报错: %v", ev.Err)
		}
	}

	if text.String() != "见 [1]" {
		t.Errorf("正文 = %q，期望 %q（标注不是正文，不该混进来）", text.String(), "见 [1]")
	}
	if !sawDone {
		t.Error("带标注的流同样要给结束标记")
	}
	if len(annotations) != 1 {
		t.Fatalf("转发出去的标注有 %d 条，期望 1 条: %+v", len(annotations), annotations)
	}
	got := annotations[0]
	if got["type"] != "url_citation" || got["url"] != "https://example.com" {
		t.Errorf("标注内容 = %+v，期望原样带上 type 与 url", got)
	}
	// 标注的字段集由上游决定，我们不解释它——所以上下界那两项也要原样到。
	if got["start_index"] != float64(0) || got["end_index"] != float64(1) {
		t.Errorf("标注内容 = %+v，期望原样带上 start_index / end_index", got)
	}
	if strings.Contains(logs.String(), "未归类") {
		t.Errorf("标注有自己的一支，不该被记成未归类：\n%s", logs.String())
	}
}

// 拒绝文案（refusal.*）**仍然只记 warn**，不转发。
//
// 它带正文、用户可见，而它进不进 output_text、进不进计费口径是另一件事
// （与标注不同：标注明确不参与计费）。这一条钉住「这一轮只补了标注，没有顺手
// 把拒绝文案也转出去」——把 refusal.delta 加进 switch 并转发，这条用例红。
func TestChatStreamRefusalStillOnlyWarns(t *testing.T) {
	logs := captureLogs(t)
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"答"}`,
		`data: {"type":"response.refusal.delta","output_index":0,"delta":"我不能帮你做这件事"}`,
		`data: {"type":"response.refusal.done","output_index":0,"refusal":"我不能帮你做这件事"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
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
		if ev.Kind == "annotation" {
			t.Errorf("拒绝文案不是标注，不该以 annotation 事件出现: %+v", ev.Annotations)
		}
	}
	if text.String() != "答" {
		t.Errorf("正文 = %q，期望只有 %q（拒绝文案不进正文，口径未定）", text.String(), "答")
	}

	log := logs.String()
	for _, name := range []string{"response.refusal.delta", "response.refusal.done"} {
		if !hasLogLine(log, "未归类", "event="+name) {
			t.Errorf("%s 应当记一条「未归类」的 warn——这一轮没有转发它，也不能让它静默消失：\n%s", name, log)
		}
	}
	if strings.Contains(log, "我不能帮你做这件事") {
		t.Errorf("日志里不该出现事件的载荷（那就是模型的输出）：\n%s", log)
	}
}

// 标注解析不出来时记一条 warn，且不中断流、不影响正文与用量。
//
// 一个坏掉的标注不该让已经吐出的正文作废；而静默丢掉它是这个包最怕的失效
// 方式之一——所以它要有痕迹（与无匹配分支同一个形状）。
func TestChatStreamBrokenAnnotationWarnsAndContinues(t *testing.T) {
	logs := captureLogs(t)
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"答"}`,
		`data: {"type":"response.output_text.annotation.added","output_index":0,"annotation":"不是一个对象"}`,
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	)

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m", Stream: true, Messages: textRequest().Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	sawDone, sawUsage := false, false
	for _, ev := range collect(t, events) {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "annotation":
			t.Errorf("解析不出来的标注不该转发: %+v", ev.Annotations)
		case "usage":
			sawUsage = true
		case "done":
			sawDone = true
		}
	}
	if text.String() != "答" || !sawDone || !sawUsage {
		t.Errorf("正文=%q usage=%v done=%v：一条坏标注不该改变这条流的走向", text.String(), sawUsage, sawDone)
	}
	if !hasLogLine(logs.String(), "引用标注解析失败", "event=response.output_text.annotation.added") {
		t.Errorf("解析失败的标注要记一条 warn：\n%s", logs.String())
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
