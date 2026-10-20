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

// eventWait 是收一个流式事件的上限。
//
// 之所以不直接 `<-events`：执行器返回一个 nil 通道、或者事件流再也不来值时，
// 直接收会把整个测试进程挂住，账面表现从「少了一条断言」变成「超时」，
// 而超时说不出是哪一处漏了。
//
// 上限要明显小于变异测试给单个变异体的预算，否则一条走不到的路仍会被记成超时。
// 那个预算是「gathering（一次 `go test -cover`）的墙钟 × 系数」：
// 这个包跑一遍测试是亚秒级，系数是十，预算是十秒以内；正常路径是本机回环，
// 事件在毫秒内就到，所以取两秒——比「慢」宽得多，又明显低于预算。
const eventWait = 2 * time.Second

// nextEvent 收一个事件，ok 为 false 表示事件流已关闭。
func nextEvent(t *testing.T, events <-chan channel.StreamEvent) (channel.StreamEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-events:
		return ev, ok
	case <-time.After(eventWait):
		t.Fatalf("等了 %v 也没收到事件：事件流既没有关闭，也没有新事件", eventWait)
		return channel.StreamEvent{}, false
	}
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

// Registry 是给不发请求的调用方用的（比如按渠道类型校验配置的写入路径）。
// 它必须与 New 装出同一份注册表：两边各自装配的话，「运维命令认得的类型」
// 与「服务认得的类型」会各自漂移，表现是某个类型配不进去，或者配进去了
// 却在请求时报未知类型。
func TestRegistryMatchesNew(t *testing.T) {
	// Registry 内部用 adapterTimeout 调 New，而 New 拒绝非正数超时。这个常量
	// 一旦被算成 0（比如乘号被改成了除号），这次调用就会带着
	// 「上游超时必须为正」的错误回来，而不是给出一张空表。
	registry, err := Registry()
	if err != nil {
		t.Fatalf("Registry 出错: %v", err)
	}
	if registry == nil {
		t.Fatal("Registry 返回了 nil 注册表，err 却是 nil")
	}

	want := "anthropic,openai,openai-responses,systemone"
	got := strings.Join(registry.Types(), ",")
	if got != want {
		t.Errorf("Registry 的类型 = %q，期望 %q", got, want)
	}

	// 与 New 装出来的那一份逐项对照：清单相同才算「同一份装配」。
	e := newExecutor(t)
	if fromNew := strings.Join(e.Types(), ","); fromNew != got {
		t.Errorf("New 的类型 = %q，Registry 的类型 = %q，两处装配应当一致", fromNew, got)
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
			// nil 通道与 nil 错误是一组：调用方看到 err == nil 就会去收，
			// 而在 nil 通道上永远收不到值。先把它挡在这里，下一段的超时才有意义。
			if events == nil {
				t.Fatal("ChatStream 返回了 nil 事件流，err 却是 nil")
			}
			if gotPath != tc.wantPath {
				t.Errorf("请求路径 = %q，期望 %q", gotPath, tc.wantPath)
			}

			var text string
			for {
				ev, ok := nextEvent(t, events)
				if !ok {
					break
				}
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
	if events == nil {
		t.Fatal("ChatStream 返回了 nil 事件流，err 却是 nil")
	}

	// 带超时地读：执行器若把事件读走了，这里会等满 eventWait 才失败，
	// 于是失败信息里能直接看出原因，而不是整条用例卡住。
	ev, ok := nextEvent(t, events)
	if !ok || ev.Text != "第一片" {
		t.Fatalf("第一个事件 = %+v，通道是否已关闭 = %v，期望「第一片」——执行器把事件流读走了", ev, !ok)
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

// Quota 与 Chat / ChatStream 同一个做法：按 ch.Type 找到适配器，调它一次。
// 证据是选中那个适配器收到了哪个渠道，以及另一个一次都没被调到。
func TestQuotaDispatchesByType(t *testing.T) {
	openai := &fakeAdapter{
		typ:        string(channel.ProtocolOpenAI),
		protocol:   channel.ProtocolOpenAI,
		quotaState: quota.State{ChannelID: 7, Unit: "元", Confidence: 0.9},
	}
	systemone := &fakeAdapter{
		typ:        string(channel.ProtocolSystemOne),
		protocol:   channel.ProtocolSystemOne,
		quotaState: quota.State{ChannelID: 99},
	}
	e, err := newWith(openai, systemone)
	if err != nil {
		t.Fatalf("newWith 出错: %v", err)
	}

	ch := testChannel("http://upstream.invalid", string(channel.ProtocolOpenAI))
	got, err := e.Quota(context.Background(), ch)
	if err != nil {
		t.Fatalf("Quota 出错: %v", err)
	}
	if got.ChannelID != 7 || got.Unit != "元" || got.Confidence != 0.9 {
		t.Errorf("返回值 = %+v，期望 openai 适配器给出的那一份", got)
	}
	if openai.quotaCalls != 1 {
		t.Errorf("openai 适配器被调了 %d 次，期望 1 次", openai.quotaCalls)
	}
	if systemone.quotaCalls != 0 {
		t.Errorf("systemone 适配器被调了 %d 次，期望 0 次——分发到了另一个适配器上", systemone.quotaCalls)
	}
	if seen := openai.lastQuotaChannel; seen.ID != ch.ID || seen.Type != ch.Type || seen.BaseURL != ch.BaseURL {
		t.Errorf("适配器收到的渠道 = 第 %d 条、类型 %q、base_url %q，期望原样转交第 %d 条、类型 %q、base_url %q",
			seen.ID, seen.Type, seen.BaseURL, ch.ID, ch.Type, ch.BaseURL)
	}
}

// 未知类型要包着 channel.ErrUnknownType 返回。它与「这家上游没有查询接口」
// 是两件事：后者由适配器返回 ErrUnsupported，说明这条渠道正常，只是查不到
// 额度；前者说明库里有配置而当前二进制不认识它。两者的处置不同，所以不能
// 认成同一种错误。
func TestQuotaReportsUnknownType(t *testing.T) {
	e := newExecutor(t)
	_, err := e.Quota(context.Background(), testChannel("http://upstream.invalid", "no-such-type"))
	if !errors.Is(err, channel.ErrUnknownType) {
		t.Fatalf("Quota 的错误 = %v，期望包着 channel.ErrUnknownType", err)
	}
	if errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("不该报成「上游不支持查询额度」: %v", err)
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

	quotaCalls       int
	lastQuotaChannel channel.Channel
	quotaState       quota.State
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

func (f *fakeAdapter) Quota(_ context.Context, ch channel.Channel) (quota.State, error) {
	f.quotaCalls++
	f.lastQuotaChannel = ch
	return f.quotaState, nil
}
