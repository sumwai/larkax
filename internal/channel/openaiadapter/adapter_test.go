package openaiadapter

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

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
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

func TestTypeAndProtocol(t *testing.T) {
	a := newAdapter()
	if a.Type() != Type {
		t.Errorf("Type() = %q", a.Type())
	}
	if a.Protocol() != channel.ProtocolOpenAI {
		t.Errorf("Protocol() = %q", a.Protocol())
	}

	// 端点那一栏：适配器自述的路径要与它声明的协议算出来的那条一致（注册表
	// 启动时也自检这一条，见 channel.Registry.Register），而**类型名必须落在
	// 协议取值上**——选路按渠道的 type 算这条渠道打哪条端点
	// （channel.Protocol(<type>).Endpoint()，见 internal/channels），两者不同名
	// 时算出来是空串（= 不知道 = 放行），这条渠道的端点判据会静默失效。
	if got := a.Endpoint(); got != channel.EndpointChatCompletions {
		t.Errorf("Endpoint() = %q，期望 %q", got, channel.EndpointChatCompletions)
	}
	if got := channel.Protocol(Type).Endpoint(); got != a.Endpoint() {
		t.Errorf("channel.Protocol(%q).Endpoint() = %q，与适配器自述的 %q 不一致："+
			"Type() 必须与它说的协议取值同名", Type, got, a.Endpoint())
	}
}

func TestChatSuccess(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"cmpl-1","model":"gpt-4o-2024",
			"choices":[{"index":0,"finish_reason":"stop",
				"message":{"role":"assistant","content":"你好"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
		}`)
	}))
	defer srv.Close()

	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{
		Model:    "gpt-4o",
		Messages: []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("请求路径 = %q", gotPath)
	}
	if gotAuth != "Bearer placeholder-credential" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if resp.UpstreamModel != "gpt-4o-2024" {
		t.Errorf("UpstreamModel = %q", resp.UpstreamModel)
	}
	if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "你好" {
		t.Errorf("内容 = %+v", resp.Message.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
		t.Errorf("用量 = %+v", resp.Usage)
	}
}

// 有些上游在 HTTP 200 里塞一个 error 对象。只看状态码会把它当成正常响应，
// 于是「上游失败了」在账上表现为「成功但用量为零」——那比直接报错更难查。
func TestChatDetectsEmbeddedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"message":"模型不存在","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "nope"})
	if err == nil {
		t.Fatal("200 里的 error 对象应当被识别")
	}
	if channel.IsRetryable(err) {
		t.Error("参数类错误不该可重试：换渠道也是同一个错")
	}
	// 这是一次**拿到过响应**的失败：上游真的回了 200。状态码要跟着错误
	// 一起带出去，否则账单上那一行会读成「没有记录到状态码」。
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if !up.GotResponse || up.StatusCode != http.StatusOK {
		t.Errorf("GotResponse=%v StatusCode=%d，期望 true / %d",
			up.GotResponse, up.StatusCode, http.StatusOK)
	}
}

// 只有 type 没有 message 的错误对象同样是错误。
//
// 三字段全空才当「无错误」（`{"error":null}`、`{"error":{}}` 的写法）；
// 最外层的 `&&` 改成 `||` 之后，只要 message 为空就会被当成没有错误，
// 这一类上游报错于是变成正常响应：客户拿到一份空回答，账上是一条成功记录。
func TestChatDetectsTypedErrorWithoutMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("只有 type 的 error 对象也该被识别")
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if up.Body != "" {
		t.Errorf("Body = %q，没有 message 时应当是空串", up.Body)
	}
}

// 200 里的错误对象是**限流**形态时，这次失败值得换渠道重试。
//
// 限流这件事写在哪个字段上、以什么类型写，各家不同：写在 type 上，写在 code
// 上（OpenAI 真实的 429 响应体里 type 是 "requests"，限流写在 code 里），也有
// 上游把状态码写成数字塞进 code。三种都要认出来。
//
// 判成不可重试的后果有两条（见 internal/gateway 的 channelAtFault）：路由层不换
// 下一个候选，一次本可成功的请求直接失败；这个渠道也不记失败，正在限流的它照旧
// 拿流量。
func TestChatEmbeddedRateLimitIsRetryable(t *testing.T) {
	bodies := []string{
		`{"error":{"message":"Rate limit reached","type":"rate_limit_error"}}`,
		`{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`,
		`{"error":{"message":"Rate limit reached","code":429}}`,
	}
	for _, body := range bodies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))

		_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
		srv.Close()

		if err == nil {
			t.Fatalf("%s：200 里的 error 对象应当被识别", body)
		}
		if !channel.IsRetryable(err) {
			t.Errorf("%s：限流是上游这一侧暂时答不上来，换一个渠道可能成功，应当可重试", body)
		}
	}
}

// `"error": null` 与空对象是正常的「无错误」写法，不能被误判。
func TestChatIgnoresNullError(t *testing.T) {
	for _, body := range []string{
		`{"error":null,"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
		`{"error":{},"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
		srv.Close()
		if err != nil {
			t.Errorf("空 error 不应判为失败: %v（body=%s）", err, body)
		}
	}
}

func TestChatStatusHandling(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusUnprocessableEntity, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "上游拒绝了", tc.status)
			}))
			defer srv.Close()

			_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
			if err == nil {
				t.Fatal("非 2xx 应报错")
			}
			if got := channel.IsRetryable(err); got != tc.wantRetryable {
				t.Errorf("状态 %d 的 Retryable = %v，期望 %v", tc.status, got, tc.wantRetryable)
			}
		})
	}
}

// 上游的错误响应体里可能回显请求内容，而错误会进日志。
func TestChatErrorKeepsBodySeparate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream echoed: prompt=机密内容", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("应报错")
	}
	// Body 保留在结构体里供上层按需截断，但 Error() 不该带出来
	if strings.Contains(err.Error(), "机密内容") {
		t.Errorf("错误文本里含响应体: %s", err.Error())
	}
	var ue *channel.ErrUpstream
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型为 %T", err)
	}
	if !strings.Contains(ue.Body, "机密内容") {
		t.Error("响应体应保留在 Body 字段里供排查")
	}
}

// 上游不提供查额度的标准端点，猜一个路径会让 404 被当成「额度查不到」，
// 从而让自记账永远接不上。
func TestQuotaIsUnsupported(t *testing.T) {
	_, err := newAdapter().Quota(context.Background(), testChannel("https://api.example.com"))
	if !errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("Quota 应返回 ErrUnsupported，得到 %v", err)
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"},{"id":""}]}`)
	}))
	defer srv.Close()

	models, err := newAdapter().ListModels(context.Background(), testChannel(srv.URL))
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("模型列表 = %v，空 id 应被跳过", models)
	}
}

func TestListModelsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newAdapter().ListModels(context.Background(), testChannel(srv.URL))
	if err == nil {
		t.Fatal("401 应报错")
	}
	if channel.IsRetryable(err) {
		t.Error("401 不该可重试")
	}
}

// 流式：必须在返回前确认上游已接受请求，否则调用方无法区分
// 「连不上」（可换渠道）与「流中途断了」（不能换）。
func TestChatStreamRejectsBeforeReturning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err == nil {
		t.Fatal("上游拒绝时应在返回前报错，而不是返回一个只会发错误事件的 channel")
	}
	if channel.IsRetryable(err) {
		t.Error("401 不该可重试")
	}
}

func TestChatStreamHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"choices":[{"index":0,"delta":{"content":"你"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"好"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	var text strings.Builder
	var sawUsage bool
	var sawDone bool
	for ev := range events {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "usage":
			sawUsage = true
			if ev.Usage.PromptTokens != 7 || ev.Usage.CompletionTokens != 2 {
				t.Errorf("流内用量 = %+v", ev.Usage)
			}
		case "done":
			sawDone = true
			// 结束原因要一路带到 done 事件：计费与对外的 finish_reason
			// 都读它，丢在这里等于上游说了什么、我们记成「不知道」。
			if ev.FinishReason != "stop" {
				t.Errorf("done 事件的 FinishReason = %q，期望 %q", ev.FinishReason, "stop")
			}
		case "error":
			t.Fatalf("不该有错误事件: %v", ev.Err)
		}
	}
	if text.String() != "你好" {
		t.Errorf("拼出的文本 = %q", text.String())
	}
	if !sawUsage {
		t.Error("应发出 usage 事件")
	}
	if !sawDone {
		t.Error("应发出 done 事件")
	}
}

// 心跳帧、注释帧、非 data 行都不该打断解析。
//
// 空行与冒号注释这一条的 `||` 没有能把两版分开的输入：两个子条件为真时
// 都只是 continue，而紧接着还有一道「不以 data: 开头就跳过」——空串与
// `:...` 都落进它里面。所以把 `||` 改成 `&&` 之后，没有任何输入会走出
// 另一条路，这一格按结构性等价判。前提是那道判据在第一道之后、且判的正是
// 同一个前缀；任一处改动都要重新看这一条。
func TestChatStreamSkipsNoise(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range []string{
			`: keep-alive`,
			``,
			`event: ping`,
			`data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`,
			`data: {坏 JSON`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line+"\n")
		}
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
		if ev.Kind == "error" {
			t.Fatalf("噪音不该导致错误: %v", ev.Err)
		}
	}
	if text.String() != "x" {
		t.Errorf("文本 = %q", text.String())
	}
}

// 流中途的错误要作为 error 事件发出，且不可重试——
// 重试意味着客户端会看到两段拼接的回答。
func TestChatStreamMidStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"部分"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"error":{"message":"上游内部错误"}}`+"\n\n")
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
				t.Error("流中途的错误不该可重试")
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

// 连接直接关闭而没有 [DONE]：不补 done 事件。
// 伪造正常结束会让「流被截断」看起来像「模型答完了」。
//
// 同时要求**没有 error 事件**：EOF 是正常结束的一种，不是读取失败。
// 把它报成失败会让一次已经推送完成的回答在计费与渠道健康度上各记一笔错。
func TestChatStreamTruncatedHasNoDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"半截"}}]}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	// 事件流为空时整条用例会空转通过，所以先确认至少收到一帧：
	// 断言「没有 done、没有 error」只有在真的读到过分片时才有意义。
	var sawDelta bool
	for ev := range events {
		switch ev.Kind {
		case "delta":
			sawDelta = true
		case "done":
			t.Error("缺少 [DONE] 时不应伪造 done 事件")
		case "error":
			t.Errorf("连接正常关闭不是读取失败，不该报错: %v", ev.Err)
		}
	}
	if !sawDelta {
		t.Error("一帧 delta 都没收到，这条用例什么都没证明")
	}
}

// 工具调用在流里是分片吐出的，参数是字符串片段，不能在中途解析。
func TestChatStreamToolCallDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, line := range []string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line+"\n\n")
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var args strings.Builder
	for ev := range events {
		if ev.Kind == "tool_use" {
			args.WriteString(ev.ToolCall.Arguments)
		}
	}
	if args.String() != `{"a":1}` {
		t.Errorf("拼出的参数 = %q", args.String())
	}
}

// 上游可能只给 usage 不给 [DONE]，也可能反过来。
// 计费层需要的是「用量拿到了」与「流结束了」两件事各自被如实反映。
func TestChatStreamUsageWithoutDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`+"\n\n")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawUsage bool
	for ev := range events {
		if ev.Kind == "usage" {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Error("应发出 usage 事件")
	}
}

// failingReader 是「读到一半连接被对端关掉」的响应体：TCP 复位、上游进程
// 退出都从 Read 上以错误返回，而不是干净地 EOF。
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("连接被重置") }

// 读体出错要变成 error 事件——流没有正常结束，计费层与对外响应都不能把
// 它当成「模型答完了」。
//
// 这里直接调 pump 而不走 ChatStream：httptest 的响应体很难在写入之后
// 再制造一次 Read 错误，而这条判据看的正是「Read 返回错误时发不发事件」。
// 用带缓冲的通道收事件，pump 同步跑完即可，不依赖 goroutine 时序。
func TestPumpReportsReadError(t *testing.T) {
	events := make(chan channel.StreamEvent, 8)
	newAdapter().pump(context.Background(), http.StatusBadGateway, true, failingReader{}, events)
	close(events)

	var got []channel.StreamEvent
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("事件数 = %d（%+v），期望只有一条 error", len(got), got)
	}
	if got[0].Kind != "error" {
		t.Fatalf("事件类型 = %q，期望 error", got[0].Kind)
	}
	var up *channel.ErrUpstream
	if !errors.As(got[0].Err, &up) {
		t.Fatalf("错误类型为 %T", got[0].Err)
	}
	if !up.GotResponse || up.StatusCode != http.StatusBadGateway {
		t.Errorf("GotResponse=%v StatusCode=%d，期望 true / %d",
			up.GotResponse, up.StatusCode, http.StatusBadGateway)
	}
	if up.Retryable {
		t.Error("流已经开始之后没有重试的余地，不该可重试")
	}
	if up.Err == nil {
		t.Error("原始的读取错误应带在 Err 里")
	}
}

// 单个分片可以很大（长文本的一次性增量、或 base64 图片）：默认的 64KB
// 上限会把合法的响应判成读取失败。
//
// 载荷取 1.2MB：上限那一处有两个乘号，把乘号换成除号之后
// `4/1024*1024` 是 0、`4*1024/1024` 是 4，1.2MB 的分片两边都放不进去。
// 初始缓冲的容量（`make([]byte, 0, 64*1024)`）不在此列：bufio.Scanner
// 会按需扩容到上限，那个值只是第一次分配的提示，改小不改变任何可观测
// 行为，这一格按结构性等价判。
func TestPumpAcceptsLargeFrame(t *testing.T) {
	content := strings.Repeat("长", 400*1024) // 每个汉字 3 字节，共约 1.2MB
	line := `data: {"choices":[{"index":0,"delta":{"content":"` + content + `"}}]}` + "\n\n"

	events := make(chan channel.StreamEvent, 8)
	newAdapter().pump(context.Background(), http.StatusOK, true, strings.NewReader(line), events)
	close(events)

	var text strings.Builder
	for ev := range events {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "error":
			t.Fatalf("大分片不该被判为读取失败: %v", ev.Err)
		}
	}
	if text.String() != content {
		t.Errorf("拼出的文本长度 = %d 字节，期望 %d 字节", text.Len(), len(content))
	}
}

func TestChatRejectsUnserializableRequest(t *testing.T) {
	_, err := newAdapter().Chat(context.Background(), testChannel("https://api.example.com"), &channel.Request{
		Model: "m",
		Extra: map[string]any{"top_k": func() {}},
	})
	if err == nil {
		t.Error("无法序列化的请求应在发出去之前报错")
	}
}

func TestChatNetworkFailureIsRetryable(t *testing.T) {
	// 指向一个已经关闭的端口
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(url), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("连不上应报错")
	}
	if !channel.IsRetryable(err) {
		t.Error("网络层失败应可重试：换渠道可能成功")
	}
	// 一个 HTTP 响应都没有过：GotResponse 必须是 false。下游靠它把这一档
	// 与「拿到了响应但用不了」分开。
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if up.GotResponse {
		t.Error("连不上时不该说拿到过响应")
	}
	if up.StatusCode != 0 {
		t.Errorf("StatusCode = %d，期望 0（没有响应就没有状态码）", up.StatusCode)
	}
}

func TestChatFailsFastOnBadStatusCodeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `不是 JSON`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
	if err == nil {
		t.Fatal("响应不是 JSON 应报错")
	}
	if !channel.IsRetryable(err) {
		t.Error("上游返回垃圾内容时应可重试：换渠道可能给出正常响应")
	}
}

// 助手消息里的 content 可能是字符串，也可能是内容块数组，还可能缺失。
func TestChatHandlesVariedContentShapes(t *testing.T) {
	bodies := map[string]string{
		"字符串":      `{"model":"m","choices":[{"message":{"role":"assistant","content":"纯文本"}}]}`,
		"块数组":      `{"model":"m","choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"块文本"}]}}]}`,
		"缺失":       `{"model":"m","choices":[{"message":{"role":"assistant"}}]}`,
		"空数组":      `{"model":"m","choices":[{"message":{"role":"assistant","content":[]}}]}`,
		"无choices": `{"model":"m","choices":[]}`,
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

// 上游没报的用量字段必须与「报了 0」区分开。
func TestUsageFieldPresenceDistinguishedFromZero(t *testing.T) {
	cases := map[string]struct {
		usage         string
		wantCacheRead int64
		wantReasoning int64
	}{
		"只报 prompt":   {`{"prompt_tokens":10}`, 0, 0},
		"显式零":         {`{"prompt_tokens":10,"completion_tokens":0}`, 0, 0},
		"带缓存明细":       {`{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":4}}`, 4, 0},
		"DeepSeek 写法": {`{"prompt_tokens":10,"prompt_cache_hit_tokens":6}`, 6, 0},
		"带推理 token":   {`{"prompt_tokens":10,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":5}}`, 0, 5},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"m","choices":[{"message":{"role":"assistant","content":"x"}}],"usage":` + tc.usage + `}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{Model: "m"})
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if resp.Usage.PromptTokens != 10 {
				t.Errorf("PromptTokens = %d", resp.Usage.PromptTokens)
			}
			// 缓存命中落在哪个字段名上，账上要看得出来：只报
			// prompt_cache_hit_tokens 的上游（DeepSeek 一类）如果这一支走不到，
			// 那几个 token 就按普通输入价计费——多收钱，而且不报错。
			if resp.Usage.CacheReadTokens != tc.wantCacheRead {
				t.Errorf("CacheReadTokens = %d, 期望 %d", resp.Usage.CacheReadTokens, tc.wantCacheRead)
			}
			// 推理 token 单独一列：它已经含在 completion_tokens 里，但账单
			// 明细要分开看。丢在这一段等于「上游报了、我们没记」。
			if resp.Usage.ReasoningTokens != tc.wantReasoning {
				t.Errorf("ReasoningTokens = %d, 期望 %d", resp.Usage.ReasoningTokens, tc.wantReasoning)
			}
		})
	}
}

var _ = json.Marshal

// 插件加的 x-larkax-* 头要真能到上游——它存在的全部意义就是这个。
func TestCustomHeadersReachUpstream(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"x"}}]}`)
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

// **插件不能覆盖鉴权头**：那意味着它能冒充别的渠道身份。
// 适配器是最后一道——插件层的白名单写错时靠这里挡。
func TestCustomHeadersCannotOverrideAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"x"}}]}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{
		Model: "m",
		Headers: map[string]string{
			"Authorization": "Bearer 冒充的凭证",
			"authorization": "Bearer 小写写法",
		},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if gotAuth != "Bearer placeholder-credential" {
		t.Errorf("鉴权头被覆盖了，实际 %q", gotAuth)
	}
}

// 头值里带换行等于能注入出额外的头。
func TestCustomHeaderInjectionBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Injected") != "" {
			t.Error("头注入成功了")
		}
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"x"}}]}`)
	}))
	defer srv.Close()

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), &channel.Request{
		Model:   "m",
		Headers: map[string]string{"X-Bad": "值\r\nX-Injected: evil"},
	})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
}
