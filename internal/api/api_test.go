package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/tokens"
)

// ── 桩 ────────────────────────────────────────────────────────────────

type fakeAuth struct {
	user  User
	err   error
	calls int
}

func (f *fakeAuth) Authenticate(_ context.Context, _ *http.Request) (User, error) {
	f.calls++
	return f.user, f.err
}

type fakeChatter struct {
	result   *gateway.Result
	err      error
	req      pipeline.Request
	upstream *channel.Request
	hooks    gateway.Hooks
	calls    int

	// 流式那一侧单独一份计数与结果：两条路走的是同一个接口的两个方法，
	// 而「流式请求被当成非流式发出去」正是要防的那件事，所以调用次数要
	// 分开断言。
	streamResult *gateway.Result
	streamErr    error
	streamReq    pipeline.Request
	streamCalls  int
}

func (f *fakeChatter) Chat(_ context.Context, req pipeline.Request, upstream *channel.Request, hooks gateway.Hooks) (*gateway.Result, error) {
	f.calls++
	f.req = req
	f.upstream = upstream
	f.hooks = hooks
	return f.result, f.err
}

func (f *fakeChatter) ChatStream(_ context.Context, req pipeline.Request, upstream *channel.Request) (*gateway.Result, error) {
	f.streamCalls++
	f.streamReq = req
	f.upstream = upstream
	return f.streamResult, f.streamErr
}

// streamOf 把一串事件做成一个已经写满并关闭的事件流。
//
// 预填缓冲而不是另起一个 goroutine：测试要断言的是「入口怎么把这些事件
// 变成帧」，而不是并发本身——那由网关那一侧测。
func streamOf(events ...channel.StreamEvent) <-chan channel.StreamEvent {
	ch := make(chan channel.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

// fakeResult 是一条最小但完整的成功结果。
func fakeResult() *gateway.Result {
	return &gateway.Result{
		Response: &channel.Response{
			UpstreamModel: "m-1-20250101",
			Message: channel.Message{
				Role:    "assistant",
				Content: []channel.ContentBlock{{Type: "text", Text: "你好"}},
			},
			FinishReason: "stop",
			Usage:        channel.Usage{PromptTokens: 10, CompletionTokens: 5},
		},
	}
}

func newTestHandler() (*Handler, *fakeAuth, *fakeChatter) {
	auth := &fakeAuth{user: User{ID: 7}}
	chatter := &fakeChatter{result: fakeResult()}
	h := New(auth, chatter)
	h.Version = "v0.1.0-test"
	// 与生产装配一致：品种由装配层从 settings 读出来填上。留空不是一种
	// 正常状态——计费层会拒绝空品种，每个请求都会失败。
	h.DefaultAsset = "CNY"
	h.Now = func() time.Time { return time.Unix(1700000000, 0) }
	return h, auth, chatter
}

const validBody = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`

// post 发一个最普通的合法请求，只让调用方改路径与请求体。
func post(h *Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ── 路径与协议 ─────────────────────────────────────────────────────────

// 同一个请求体、同一个响应，两条路径要给出两种形状：调用协议由路径决定，
// 而形状跟着调用协议走。
func TestShapeFollowsPath(t *testing.T) {
	h, _, _ := newTestHandler()

	openai := post(h, PathChatCompletions, validBody)
	if openai.Code != http.StatusOK {
		t.Fatalf("OpenAI 路径状态码 = %d，响应体 %s", openai.Code, openai.Body)
	}
	var oa map[string]any
	if err := json.Unmarshal(openai.Body.Bytes(), &oa); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if oa["object"] != "chat.completion" {
		t.Errorf("object = %v，期望 chat.completion", oa["object"])
	}
	if _, ok := oa["choices"]; !ok {
		t.Errorf("OpenAI 形状应有 choices: %v", oa)
	}
	if _, ok := oa["content"]; ok {
		t.Errorf("OpenAI 形状不该有顶层 content: %v", oa)
	}

	claude := post(h, PathMessages, validBody)
	if claude.Code != http.StatusOK {
		t.Fatalf("Anthropic 路径状态码 = %d，响应体 %s", claude.Code, claude.Body)
	}
	var cl map[string]any
	if err := json.Unmarshal(claude.Body.Bytes(), &cl); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if cl["type"] != "message" {
		t.Errorf("type = %v，期望 message", cl["type"])
	}
	if _, ok := cl["content"]; !ok {
		t.Errorf("Anthropic 形状应有 content: %v", cl)
	}
	if _, ok := cl["choices"]; ok {
		t.Errorf("Anthropic 形状不该有 choices: %v", cl)
	}
}

// 未知路径保持 501：部署验证靠「任何请求都立刻得到明确答复」确认端口通。
func TestUnknownPathIs501(t *testing.T) {
	h, auth, chatter := newTestHandler()

	rec := post(h, "/v1/embeddings", validBody)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("状态码 = %d，期望 501", rec.Code)
	}
	if auth.calls != 0 || chatter.calls != 0 {
		t.Error("未知路径不该走到鉴权或上游")
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("501 也应当是 JSON 错误体: %s", rec.Body)
	}
}

// ── 请求层面的检查 ─────────────────────────────────────────────────────

func TestMethodNotAllowed(t *testing.T) {
	h, auth, chatter := newTestHandler()

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), method, PathChatCompletions, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("状态码 = %d，期望 405", rec.Code)
			}
			// 没有 Allow 头，调用方只能去翻文档才知道该用什么方法。
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q，期望 POST", got)
			}
		})
	}
	if auth.calls != 0 || chatter.calls != 0 {
		t.Error("方法不对不该走到鉴权或上游")
	}
}

func TestContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		want        int
	}{
		{name: "标准 JSON", contentType: "application/json", want: http.StatusOK},
		{name: "带字符集", contentType: "application/json; charset=utf-8", want: http.StatusOK},
		{name: "带空格的参数", contentType: "application/json ;charset=UTF-8", want: http.StatusOK},
		{name: "大小写不敏感", contentType: "Application/JSON", want: http.StatusOK},
		{name: "表单", contentType: "application/x-www-form-urlencoded", want: http.StatusUnsupportedMediaType},
		{name: "纯文本", contentType: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "缺失", contentType: "", want: http.StatusUnsupportedMediaType},
		{name: "坏值", contentType: "application/json; charset=", want: http.StatusUnsupportedMediaType},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, PathChatCompletions, strings.NewReader(validBody))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("状态码 = %d，期望 %d（响应体 %s）", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

// 超限要在**读的时候**挡住，而不是先信 Content-Length。
func TestBodyTooLarge(t *testing.T) {
	h, auth, chatter := newTestHandler()

	big := strings.Repeat("a", maxRequestBodyBytes+1)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, PathChatCompletions, strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprint(maxRequestBodyBytes)) {
		t.Errorf("413 的响应里应当说明上限是多少: %s", rec.Body)
	}
	if chatter.calls != 0 {
		t.Error("超限的请求不该走到上游")
	}
	// 鉴权在体积检查之前，所以它已经跑过了——顺序是有意的：
	// 没通过鉴权的请求不该让我们为它读进最多 8 MiB。
	if auth.calls != 1 {
		t.Errorf("鉴权调用 %d 次，期望 1 次", auth.calls)
	}
}

// 凭证本身的问题回 401。另一半在 TestAuthBackendFailureIsNotUnauthorized：
// 两类失败分开正是这一对用例存在的理由。
func TestUnauthorized(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantType string
	}{
		{name: "OpenAI 形状", path: PathChatCompletions, wantType: "invalid_request_error"},
		{name: "Anthropic 形状", path: PathMessages, wantType: "authentication_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, chatter := newTestHandler()
			// 凭证失效在生产侧长这样（见 internal/auth）。
			h.Auth = &fakeAuth{err: &auth.Error{Reason: auth.ReasonKeyNotFound}}

			rec := post(h, tc.path, validBody)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，期望 401", rec.Code)
			}
			if chatter.calls != 0 {
				t.Error("没通过鉴权不该走到上游")
			}
			// 凭证为什么无效不回给调用方：区分开等于送出一个探测器。
			// auth.Error 的文案对所有 Reason 都是同一句，而 Reason 本身
			// 只给日志看——两个都不该出现在响应体里。
			if body := rec.Body.String(); strings.Contains(body, auth.ErrUnauthenticated.Error()) ||
				strings.Contains(body, string(auth.ReasonKeyNotFound)) {
				t.Errorf("不该回内部错误原文: %s", body)
			}

			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是 JSON: %v", err)
			}
			if body.Error.Type != tc.wantType {
				t.Errorf("错误类型 = %q，期望 %q", body.Error.Type, tc.wantType)
			}
		})
	}
}

// 库故障不是凭证问题，不能回 401。
//
// 两类失败混在一起时，一次数据库抖动在现场表现为「全站密钥突然失效」，
// 而那个排查方向与「某把 key 真的失效了」完全相反（见 internal/auth 的包注释）。
func TestAuthBackendFailureIsNotUnauthorized(t *testing.T) {
	// 按生产侧的形状包一层库错误：internal/auth 的查询失败**刻意**不包成
	// ErrUnauthenticated（见 store.go），所以这里是一个普通 error，
	// errors.Is 判不到那个哨兵。
	backendErr := fmt.Errorf("查询 API Key 失败: %w", errors.New("连接已断开"))

	cases := []struct {
		name string
		path string
	}{
		{name: "OpenAI 形状", path: PathChatCompletions},
		{name: "Anthropic 形状", path: PathMessages},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs logCapture
			adoptLogCapture(t, &logs)

			h, _, chatter := newTestHandler()
			h.Auth = &fakeAuth{err: backendErr}

			rec := post(h, tc.path, validBody)

			// 钉住具体状态码而不是「是不是 5xx」：这一条同时挡住两个方向的
			// 回归——退回 401，或者悄悄换成别的 5xx 码。
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("状态码 = %d，期望 500——库故障不是凭证问题", rec.Code)
			}
			if chatter.calls != 0 {
				t.Error("没通过鉴权不该走到上游")
			}
			// 库为什么连不上不回给调用方：写出去等于把库的可用性告诉每一个
			// 持 key 的调用方。
			if body := rec.Body.String(); strings.Contains(body, "连接已断开") {
				t.Errorf("不该回内部错误原文: %s", body)
			}

			var body struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是 JSON: %v", err)
			}
			// 错误体的形状跟着状态码走，所以两套协议在这里都是 api_error，
			// 而不是 401 那两种（invalid_request_error / authentication_error）：
			// 按类型分支的 SDK 于是不会把它当成凭证问题。
			if body.Error.Type != "api_error" {
				t.Errorf("错误类型 = %q，期望 api_error", body.Error.Type)
			}

			// 状态码之外能看见这件事的只有日志。不记的话，这类故障在生产里
			// 仍然只有一个 5xx 可看，看不出是哪一环坏的。
			if got := logs.text(); !strings.Contains(got, "连接已断开") {
				t.Errorf("库故障要带着 err 记日志，实际日志: %q", got)
			}
		})
	}
}

func TestInvalidBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "不是 JSON", body: `{`, want: http.StatusBadRequest},
		{name: "不是对象", body: `"hello"`, want: http.StatusBadRequest},
		{name: "没有模型名", body: `{"messages":[{"role":"user","content":"hi"}]}`, want: http.StatusBadRequest},
		{name: "没有消息", body: `{"model":"m-1","messages":[]}`, want: http.StatusBadRequest},
		{name: "空对象", body: `{}`, want: http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, chatter := newTestHandler()
			rec := post(h, PathChatCompletions, tc.body)
			if rec.Code != tc.want {
				t.Errorf("状态码 = %d，期望 %d（响应体 %s）", rec.Code, tc.want, rec.Body)
			}
			if chatter.calls != 0 {
				t.Error("请求体不合法不该走到上游")
			}
		})
	}
}

// 请求里的 stream 决定走哪条路，而**不能**被静默降级：流式请求若被当成
// 非流式发出去，调用方会拿到一个普通 JSON 体而它按 SSE 去解——失败在
// 客户端表现为「解析不了响应」，与服务端真正的原因隔了一层。
func TestStreamGoesToStreamPath(t *testing.T) {
	h, _, chatter := newTestHandler()
	chatter.streamResult = &gateway.Result{Events: streamOf()}

	rec := post(h, PathChatCompletions, `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	if chatter.streamCalls != 1 || chatter.calls != 0 {
		t.Errorf("ChatStream 调用 %d 次、Chat 调用 %d 次，期望 1 与 0",
			chatter.streamCalls, chatter.calls)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}
	if !chatter.upstream.Stream {
		t.Error("发往上游的请求要带上 Stream")
	}
	// 计费侧那份请求与非流式完全一样：流式不影响预留怎么算。
	if chatter.streamReq.MaxTokens != 16 {
		t.Errorf("计费请求的 MaxTokens = %d，期望 16", chatter.streamReq.MaxTokens)
	}
}

// 没有 stream 的请求仍然走非流式那条路，且响应不是事件流。
func TestNonStreamStaysOnChatPath(t *testing.T) {
	h, _, chatter := newTestHandler()

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if chatter.calls != 1 || chatter.streamCalls != 0 {
		t.Errorf("Chat 调用 %d 次、ChatStream 调用 %d 次，期望 1 与 0",
			chatter.calls, chatter.streamCalls)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("非流式响应不该是事件流: %q", ct)
	}
}

// 上游在接受请求之前就失败时，流还没开始，所以还能照常回一个错误响应。
func TestStreamUpstreamFailureBeforeStart(t *testing.T) {
	h, _, chatter := newTestHandler()
	chatter.streamErr = &channel.ErrUpstream{StatusCode: http.StatusTooManyRequests, Retryable: true}

	rec := post(h, PathChatCompletions, `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429（响应体 %s）", rec.Code, rec.Body)
	}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Error("流还没开始时不该写事件流的响应头——那之后改不了状态码")
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("应当是 JSON 错误体: %s", rec.Body)
	}
}

// 依赖没装配时回 501，而不是在请求期 panic。
func TestUnwiredDependencies(t *testing.T) {
	cases := []struct {
		name string
		h    *Handler
	}{
		{name: "没有鉴权", h: &Handler{Chatter: &fakeChatter{}}},
		{name: "没有上游", h: &Handler{Auth: &fakeAuth{}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(tc.h, PathChatCompletions, validBody)
			if rec.Code != http.StatusNotImplemented {
				t.Errorf("状态码 = %d，期望 501", rec.Code)
			}
		})
	}
}

// ── 到上游的那一跳 ─────────────────────────────────────────────────────

// 计费侧与上游侧是两个请求，别混起来：计费只带它要知道的，上游名留在
// channel.Request 里。
func TestRequestsReachingChatter(t *testing.T) {
	h, _, chatter := newTestHandler()

	// 请求标识走 observe：入口只是把它取出来。
	handler := observe.Middleware(h)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, PathChatCompletions, strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(observe.HeaderRequestID, "trace-me")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}

	if chatter.req.RequestID != "trace-me" {
		t.Errorf("计费侧 RequestID = %q，期望 trace-me", chatter.req.RequestID)
	}
	if chatter.req.UserID != 7 {
		t.Errorf("计费侧 UserID = %d，期望 7", chatter.req.UserID)
	}
	if chatter.req.CanonicalModel != "m-1" {
		t.Errorf("计费侧 CanonicalModel = %q", chatter.req.CanonicalModel)
	}
	if chatter.req.MaxTokens != 16 {
		t.Errorf("计费侧 MaxTokens = %d，期望 16", chatter.req.MaxTokens)
	}
	if want := tokens.EstimatedTokens("m-1", "hi"); chatter.req.InputTokens != want {
		t.Errorf("计费侧 InputTokens = %d，期望 %d", chatter.req.InputTokens, want)
	}
	if chatter.upstream.Model != "m-1" || chatter.upstream.MaxTokens != 16 {
		t.Errorf("上游侧请求 = %+v", chatter.upstream)
	}
	if len(chatter.upstream.Messages) != 1 {
		t.Errorf("上游侧消息 = %+v", chatter.upstream.Messages)
	}

	// 响应里的 id 与请求标识一致：用户报问题时贴的就是这个值。
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.ID != "trace-me" {
		t.Errorf("响应 id = %q，期望与请求标识一致", body.ID)
	}
}

// 协议由**路径**决定，并一路带到计费侧：/v1/chat/completions 是 openai，
// /v1/messages 是 anthropic，流式与非流式都一样。
//
// 断言的是具体取值而不是「非空」：两条路径填同一个协议也能让「非空」通过，
// 而 billing_requests.protocol 正是事后按协议统计的依据（#143）。
func TestBillingRequestCarriesProtocol(t *testing.T) {
	const streamBody = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`

	cases := []struct {
		name     string
		path     string
		body     string
		streamed bool
		want     channel.Protocol
	}{
		{name: "OpenAI 非流式", path: PathChatCompletions, body: validBody, want: channel.ProtocolOpenAI},
		{name: "OpenAI 流式", path: PathChatCompletions, body: streamBody, streamed: true, want: channel.ProtocolOpenAI},
		{name: "Anthropic 非流式", path: PathMessages, body: validBody, want: channel.ProtocolAnthropic},
		{name: "Anthropic 流式", path: PathMessages, body: streamBody, streamed: true, want: channel.ProtocolAnthropic},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, chatter := newTestHandler()
			if tc.streamed {
				chatter.streamResult = &gateway.Result{Events: streamOf()}
			}

			rec := post(h, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
			}

			got := chatter.req
			if tc.streamed {
				got = chatter.streamReq
			}
			if got.Protocol != tc.want {
				t.Errorf("计费侧 Protocol = %q，期望 %q", got.Protocol, tc.want)
			}
		})
	}
}

// 结算品种从 Handler 原样带进计费请求，两条入口都是。
//
// 品种是运行期配置（settings 的 billing.default_asset），装配层读出来填进
// Handler。这一条钉的是「入口真的把它带上了」：写死一个常量的话，
// 换个取值这里就挂——而现场的表现会是「改了配置，账单上的品种没变」。
func TestBillingRequestCarriesConfiguredAsset(t *testing.T) {
	const streamBody = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`

	cases := []struct {
		name     string
		path     string
		body     string
		streamed bool
	}{
		{name: "非流式", path: PathChatCompletions, body: validBody},
		{name: "流式", path: PathChatCompletions, body: streamBody, streamed: true},
	}

	// 两个取值，都不是「碰巧等于代码里写死的那个」：只测一个的话，
	// 把常量写回代码里也照样通过。
	for _, asset := range []string{"CNY", "USD"} {
		for _, tc := range cases {
			t.Run(asset+"/"+tc.name, func(t *testing.T) {
				h, _, chatter := newTestHandler()
				h.DefaultAsset = asset
				if tc.streamed {
					chatter.streamResult = &gateway.Result{Events: streamOf()}
				}

				rec := post(h, tc.path, tc.body)
				if rec.Code != http.StatusOK {
					t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
				}

				got := chatter.req
				if tc.streamed {
					got = chatter.streamReq
				}
				if got.Asset != asset {
					t.Errorf("计费侧 Asset = %q，期望 %q", got.Asset, asset)
				}
			})
		}
	}
}

// 调用链上没装中间件时也要有一个非空的标识：它同时是计费的幂等键，
// 空字符串在库里会与下一条撞上。
func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	h, _, chatter := newTestHandler()

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if chatter.req.RequestID == "" {
		t.Fatal("没有中间件时也该生成一个请求标识")
	}

	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.ID != chatter.req.RequestID {
		t.Errorf("响应 id = %q，与计费侧 %q 不一致", body.ID, chatter.req.RequestID)
	}
}

func TestUpstreamErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "没有可用渠道", err: route.ErrNoCandidates, want: http.StatusServiceUnavailable},
		{name: "没有执行器", err: gateway.ErrNoExecutor, want: http.StatusNotImplemented},
		{
			name: "缺 max_tokens",
			// 与 pipeline.Request.validate 一样包一层：判据要能穿过这层包装，
			// 否则它只在测试里成立。
			err:  fmt.Errorf("%w: max_tokens 必须为正，当前 %d", billing.ErrNoUpperBound, 0),
			want: http.StatusBadRequest,
		},
		{
			name: "上游限流",
			err:  &channel.ErrUpstream{StatusCode: http.StatusTooManyRequests, Retryable: true},
			want: http.StatusTooManyRequests,
		},
		{
			name: "上游说请求不对",
			err:  &channel.ErrUpstream{StatusCode: http.StatusBadRequest},
			// 不跟着上游回 400：那会让调用方去改一个本来正确的请求。
			want: http.StatusBadGateway,
		},
		{
			name: "上游挂了",
			err:  &channel.ErrUpstream{StatusCode: http.StatusBadGateway},
			want: http.StatusBadGateway,
		},
		{name: "所有候选都失败", err: &route.Errors{}, want: http.StatusBadGateway},
		{name: "认不出来的错误", err: errors.New("账本写不进去"), want: http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			h.Chatter = &fakeChatter{err: tc.err}

			rec := post(h, PathChatCompletions, validBody)
			if rec.Code != tc.want {
				t.Errorf("状态码 = %d，期望 %d（响应体 %s）", rec.Code, tc.want, rec.Body)
			}
			// 上游的错误原文（可能回显请求内容）不回给调用方。
			if strings.Contains(rec.Body.String(), tc.err.Error()) {
				t.Errorf("不该回内部错误原文: %s", rec.Body)
			}
		})
	}
}

// 缺 max_tokens 是**调用方**的请求不完整，不是我们挂了：状态码要落在 4xx，
// 而且响应体要点名是哪个字段缺了。只回一句「请求无效」的话，调用方不知道
// 该改哪里；回 500 的话，他会去查我们的日志而不是自己的请求。
func TestMissingMaxTokensIsClientError(t *testing.T) {
	h, _, _ := newTestHandler()
	h.Chatter = &fakeChatter{
		err: fmt.Errorf("%w: max_tokens 必须为正，当前 %d", billing.ErrNoUpperBound, 0),
	}

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应体 %s）", rec.Code, rec.Body)
	}

	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v（%s）", err, rec.Body)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Errorf("错误类型 = %q，期望 invalid_request_error（调用方按类型分支）", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "max_tokens") {
		t.Errorf("错误体没点名缺的是 max_tokens: %q", body.Error.Message)
	}
}

// 调用方断开之后没有人在听，写响应只是白费一次序列化。
func TestClientDisconnectWritesNothing(t *testing.T) {
	h, _, _ := newTestHandler()
	h.Chatter = &fakeChatter{err: context.Canceled}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, PathChatCompletions, strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Body.Len() != 0 {
		t.Errorf("调用方已断开，不该写响应体: %s", rec.Body)
	}
}

// 非流式入口拿到没有响应体的结果（上游那条路走了流式）是装配错误，
// 应当回 500 而不是 panic。
func TestResultWithoutResponseIsServerError(t *testing.T) {
	h, _, _ := newTestHandler()
	h.Chatter = &fakeChatter{result: &gateway.Result{}}

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("状态码 = %d，期望 500", rec.Code)
	}
}

func TestVersionHeader(t *testing.T) {
	h, _, _ := newTestHandler()

	ok := post(h, PathChatCompletions, validBody)
	if got := ok.Header().Get(HeaderVersion); got != "v0.1.0-test" {
		t.Errorf("成功响应的 %s = %q", HeaderVersion, got)
	}

	bad := post(h, "/v1/embeddings", validBody)
	if got := bad.Header().Get(HeaderVersion); got != "v0.1.0-test" {
		t.Errorf("501 响应的 %s = %q", HeaderVersion, got)
	}

	// 没注入版本时不该写一个空的头。
	bare := &Handler{}
	rec := post(bare, PathChatCompletions, validBody)
	if _, ok := rec.Header()[HeaderVersion]; ok {
		t.Error("版本为空时不该写这个头")
	}
}

// recordError 直接走错误写出路径，拿回一个 recorder。
func recordError(protocol channel.Protocol, status int) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	writeError(rec, protocol, status, "something went wrong")
	return rec
}

// 读取请求体本身失败（例如连接断了）时回 400，而不是把半截内容当合法请求解析。
func TestUnreadableBody(t *testing.T) {
	h, _, chatter := newTestHandler()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, PathChatCompletions, failingReader{})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400", rec.Code)
	}
	if chatter.calls != 0 {
		t.Error("读不到请求体不该走到上游")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("连接断了") }

// Now 没注入时用真实时间。
func TestNowDefaultsToRealTime(t *testing.T) {
	before := time.Now()
	got := (&Handler{}).now()
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("now() = %v，期望落在两次取时间之间", got)
	}
}
