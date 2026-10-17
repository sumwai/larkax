package upstream

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
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/route"
)

// 执行器要能被 route 的两种执行器接口直接使用。写成编译期断言而不是
// 运行期反射：接口改了之后是构建失败，而不是某个测试报一句看不懂的错。
// gateway.Executor 就是这两个接口的并集，所以这里成立即满足它。
var (
	_ route.Executor       = (*Executor)(nil)
	_ route.StreamExecutor = (*Executor)(nil)
)

func newExecutor(t *testing.T) *Executor {
	t.Helper()
	e, err := New(5 * time.Second)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}
	return e
}

// 内置适配器都要真的被注册上。这条是这次改动的核心：它们此前从未被构造过，
// 而「没注册上」在运行期的表现是每个请求都报未知渠道类型。
func TestNewRegistersBuiltinAdapters(t *testing.T) {
	e := newExecutor(t)
	got := strings.Join(e.Types(), ",")
	if got != "anthropic,openai,openai-responses,systemone" {
		t.Errorf("已注册类型 = %q，期望 anthropic,openai,openai-responses,systemone", got)
	}
}

// 0 超时等于「永远不超时」，上游卡住时表现为请求永不返回。
func TestNewRejectsNonPositiveTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := New(d); err == nil {
			t.Errorf("New(%v) 应当报错", d)
		}
	}
}

// 按类型分发的证据是「请求落在了哪条路径、带了哪个鉴权头」：
// 两套协议在这两点上不同，所以能区分出到底调的是哪个适配器。
func TestChatDispatchesByType(t *testing.T) {
	cases := []struct {
		typ         string
		wantPath    string
		wantAuthKey string // 期望出现的头名
		wantAuthVal string
		body        string
	}{
		{
			typ:         "openai",
			wantPath:    "/v1/chat/completions",
			wantAuthKey: "Authorization",
			wantAuthVal: "Bearer <CHANNEL_CREDENTIAL>",
			body: `{"id":"cmpl-1","model":"upstream-model",
				"choices":[{"index":0,"finish_reason":"stop",
					"message":{"role":"assistant","content":"你好"}}],
				"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		},
		{
			typ:         "anthropic",
			wantPath:    "/v1/messages",
			wantAuthKey: "x-api-key",
			wantAuthVal: "<CHANNEL_CREDENTIAL>",
			body: `{"id":"msg_1","type":"message","role":"assistant",
				"content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":5}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			var gotPath, gotAuth, gotBearer string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get(tc.wantAuthKey)
				gotBearer = r.Header.Get("Authorization")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			resp, err := newExecutor(t).Chat(context.Background(), testChannel(srv.URL, tc.typ), request())
			if err != nil {
				t.Fatalf("Chat 出错: %v", err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("请求路径 = %q，期望 %q——说明分发到了另一个适配器", gotPath, tc.wantPath)
			}
			if gotAuth != tc.wantAuthVal {
				t.Errorf("%s = %q，期望 %q", tc.wantAuthKey, gotAuth, tc.wantAuthVal)
			}
			// Anthropic 用 x-api-key 而不是 Bearer。两条头同时出现说明
			// 分发到了 openai 适配器上，是这条用例真正要拦的情形。
			if tc.typ == "anthropic" && gotBearer != "" {
				t.Errorf("Anthropic 渠道不该设置 Authorization，实际为 %q", gotBearer)
			}
			// 凭证是原样传下去的：执行器不持有、不加工、不解密。
			if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
				t.Errorf("用量 = %+v", resp.Usage)
			}
		})
	}
}

// 流式同样要按类型分发，且事件要能原样读出来。
func TestChatStreamDispatchesByType(t *testing.T) {
	cases := []struct {
		typ      string
		wantPath string
		body     string
	}{
		{
			typ:      "openai",
			wantPath: "/v1/chat/completions",
			body: strings.Join([]string{
				`data: {"choices":[{"index":0,"delta":{"content":"你"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"好"}}]}`,
				`data: [DONE]`,
			}, "\n\n") + "\n\n",
		},
		{
			typ:      "anthropic",
			wantPath: "/v1/messages",
			body: strings.Join([]string{
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}`,
				``,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`,
				``,
				`event: message_stop`,
				`data: {"type":"message_stop"}`,
				``,
			}, "\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			events, err := newExecutor(t).ChatStream(context.Background(), testChannel(srv.URL, tc.typ), request())
			if err != nil {
				t.Fatalf("ChatStream 出错: %v", err)
			}
			if gotPath != tc.wantPath {
				t.Errorf("请求路径 = %q，期望 %q", gotPath, tc.wantPath)
			}

			var text string
			for ev := range events {
				if ev.Kind == "delta" {
					text += ev.Text
				}
			}
			if text != "你好" {
				t.Errorf("正文 = %q，期望 你好", text)
			}
		})
	}
}

// ChatStream 必须把事件流原样交出去，不能在这里读。
// 用一个「返回时就已装好一个事件」的 channel 来验证：如果执行器读了它，
// 那个事件就被它吃掉了，调用方读到的第一个事件会不对。
func TestChatStreamDoesNotConsumeEvents(t *testing.T) {
	fake := &fakeAdapter{typ: string(channel.ProtocolOpenAI), protocol: channel.ProtocolOpenAI}
	fake.events = make(chan channel.StreamEvent, 1)
	fake.events <- channel.StreamEvent{Kind: "delta", Text: "第一片"}

	e, err := newWith(fake)
	if err != nil {
		t.Fatalf("newWith 出错: %v", err)
	}

	ch := testChannel("http://upstream.invalid", string(channel.ProtocolOpenAI))
	req := request()
	events, err := e.ChatStream(context.Background(), ch, req)
	if err != nil {
		t.Fatalf("ChatStream 出错: %v", err)
	}

	// 带超时地读：执行器若把事件读走了，这里会阻塞而不是立刻返回，
	// 于是失败信息里能直接看出原因，而不是整条用例卡住。
	select {
	case ev, ok := <-events:
		if !ok || ev.Text != "第一片" {
			t.Fatalf("第一个事件 = %+v，期望「第一片」——执行器把事件流读走了", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("读不到事件：执行器把事件流读走了，或者它没有原样转交")
	}
	if fake.streamCalls != 1 {
		t.Errorf("适配器被调了 %d 次，期望 1 次", fake.streamCalls)
	}
	if fake.lastChannel.Type != ch.Type {
		t.Errorf("转交的渠道类型 = %q", fake.lastChannel.Type)
	}
	if fake.lastRequest != req {
		t.Error("请求应当原样转交，不做复制或改写")
	}
}

// 未知渠道类型要能被认出来，且**不可重试**——它是「配置与版本不匹配」，
// 不是上游故障，重试多少次都一样。
func TestUnknownTypeIsRecognizable(t *testing.T) {
	e := newExecutor(t)
	ch := testChannel("http://upstream.invalid", "不认识的类型")

	_, err := e.Chat(context.Background(), ch, request())
	if !errors.Is(err, channel.ErrUnknownType) {
		t.Fatalf("Chat 的错误 = %v，期望能用 errors.Is 认出 channel.ErrUnknownType", err)
	}
	if channel.IsRetryable(err) {
		t.Error("未知渠道类型不该可重试")
	}
	if !strings.Contains(err.Error(), "不认识的类型") {
		t.Errorf("错误信息里应当带上那个类型：%v", err)
	}
	if !strings.Contains(err.Error(), "渠道 1") {
		t.Errorf("错误信息里应当带上渠道 ID，便于定位配置行：%v", err)
	}

	_, err = e.ChatStream(context.Background(), ch, request())
	if !errors.Is(err, channel.ErrUnknownType) {
		t.Fatalf("ChatStream 的错误 = %v，期望能用 errors.Is 认出 channel.ErrUnknownType", err)
	}
}

// 上游返回的可重试判断要原样透出来：route 靠它决定换不换渠道。
// 执行器把它改写掉的话，重试策略会静默失效。
func TestUpstreamErrorKeepsRetryability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"慢一点"}}`)
	}))
	defer srv.Close()

	_, err := newExecutor(t).Chat(context.Background(),
		testChannel(srv.URL, "openai"), request())
	if err == nil {
		t.Fatal("429 应当报错")
	}
	if !channel.IsRetryable(err) {
		t.Errorf("429 应当可重试，得到 %v", err)
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) || up.StatusCode != http.StatusTooManyRequests {
		t.Errorf("错误应当是带状态码的 ErrUpstream，得到 %v", err)
	}
}

// 同一个 Type 注册两次必须被拒绝：覆盖会让「两个适配器抢一个名字」
// 表现为「其中一个静默失效」，而失效的那个通常是新加的。
//
// 这些坏适配器要从 newWith 这条路出去才算数：注册是装配时做的，而装配的
// 失败才是进程起不来——注册表自己报错、newWith 把它吞掉的话，现场看到的
// 仍是「启动无异常，那条渠道静默不判」。
func TestNewWithRejectsBadAdapters(t *testing.T) {
	ok := &fakeAdapter{typ: string(channel.ProtocolOpenAI), protocol: channel.ProtocolOpenAI}

	cases := []struct {
		name     string
		adapters []channel.Adapter
	}{
		{"nil 适配器", []channel.Adapter{nil}},
		{"Type 为空", []channel.Adapter{&fakeAdapter{typ: "", protocol: channel.ProtocolOpenAI}}},
		{"协议未知", []channel.Adapter{&fakeAdapter{typ: string(channel.ProtocolOpenAI), protocol: channel.Protocol("不存在的协议")}}},
		// custom 曾经是合法取值。它已经没有了：适配器只能说它实现的是哪套
		// 现成的协议，不能自造一个协议族——自造的那个没有任何东西读得懂。
		{"协议 custom", []channel.Adapter{&fakeAdapter{typ: string(channel.ProtocolOpenAI), protocol: channel.Protocol("custom")}}},
		// 类型名与声明的协议不是同一个字符串：注册表按类型算端点，而这一侧
		// 按声明的协议走，两端点对不上。
		{"类型名与协议不一致", []channel.Adapter{&fakeAdapter{typ: "my-vendor", protocol: channel.ProtocolOpenAI}}},
		{"重复类型", []channel.Adapter{ok, &fakeAdapter{typ: string(channel.ProtocolOpenAI), protocol: channel.ProtocolOpenAI}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newWith(tc.adapters...); err == nil {
				t.Error("应当报错，否则注册表会残缺或静默覆盖")
			}
		})
	}
}

func testChannel(baseURL, typ string) channel.Channel {
	return channel.Channel{
		ID:         1,
		Name:       "test",
		Type:       typ,
		BaseURL:    baseURL,
		Credential: "<CHANNEL_CREDENTIAL>",
	}
}

func request() *channel.Request {
	return &channel.Request{
		Model:     "upstream-model",
		MaxTokens: 100,
		Messages: []channel.Message{
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}
}

// fakeAdapter 用来观察执行器的分发与转交行为。只实现接口，不做协议转换。
type fakeAdapter struct {
	typ      string
	protocol channel.Protocol

	streamCalls int
	lastChannel channel.Channel
	lastRequest *channel.Request
	events      chan channel.StreamEvent
}

func (f *fakeAdapter) Type() string               { return f.typ }
func (f *fakeAdapter) Protocol() channel.Protocol { return f.protocol }

// Endpoint 按协议推：这个替身不做协议转换，也就没有第二条路径可声明，
// 而注册表要求自述的端点与 Protocol 对得上（见 channel.Registry.Register）。
func (f *fakeAdapter) Endpoint() channel.Endpoint { return f.protocol.Endpoint() }
func (f *fakeAdapter) Chat(context.Context, channel.Channel, *channel.Request) (*channel.Response, error) {
	return &channel.Response{}, nil
}

func (f *fakeAdapter) ChatStream(_ context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	f.streamCalls++
	f.lastChannel = ch
	f.lastRequest = req
	if f.events == nil {
		closed := make(chan channel.StreamEvent)
		close(closed)
		return closed, nil
	}
	return f.events, nil
}

func (f *fakeAdapter) ListModels(context.Context, channel.Channel) ([]string, error) {
	return nil, channel.ErrUnsupported
}

func (f *fakeAdapter) Quota(context.Context, channel.Channel) (quota.State, error) {
	return quota.State{}, nil
}
