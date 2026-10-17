package systemoneadapter

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
	"github.com/sumwai/larkax/internal/systemone"
)

// 假上游。仓库里不出现任何真实的域名、账号与模型清单（见 AGENTS.md 的红线），
// 所以这里全部是 httptest 起的本地服务与占位符。
type fakeUpstream struct {
	*httptest.Server

	gotPath   string
	gotAuth   string
	gotAccept string
	gotBody   []byte

	status int
	reply  string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{status: http.StatusOK, reply: `{"model":"upstream-model","answers":{},"usage":{}}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath = r.URL.Path
		f.gotAuth = r.Header.Get("Authorization")
		f.gotAccept = r.Header.Get("Accept")
		f.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.reply)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) ch() channel.Channel {
	return channel.Channel{
		ID:         7,
		Name:       "fake",
		Type:       Type,
		BaseURL:    f.URL,
		Credential: "test-key-placeholder",
	}
}

func sampleRequest() *systemone.Request {
	return &systemone.Request{
		Model: "upstream-model",
		State: "工单: 用户说付款后没到账",
		Questions: map[string]systemone.Question{
			"q_noul": {Type: systemone.TypeNoul, Instructions: "这是账单问题吗"},
			"q_choice": {Type: systemone.TypeChoice, Instructions: "路由到哪个团队",
				Choices: map[string]string{"billing": "付款相关"}},
		},
	}
}

func TestDecideSendsTheMeasuredShape(t *testing.T) {
	f := newFakeUpstream(t)
	f.reply = `{"model":"upstream-model",
		"answers":{"q_noul":{"type":"noul","noul":0.75}},
		"usage":{"input_tokens":387,"output_tokens":72}}`

	a := New(5 * time.Second)
	resp, err := a.Decide(context.Background(), f.ch(), sampleRequest())
	if err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}

	if f.gotPath != Path {
		t.Errorf("请求路径 = %q，期望 %q", f.gotPath, Path)
	}
	if f.gotAuth != "Bearer test-key-placeholder" {
		t.Errorf("鉴权头 = %q", f.gotAuth)
	}
	// 明确要 JSON：这条表面从不流式，而有些上游按 Accept 决定回什么形状。
	if !strings.Contains(f.gotAccept, "application/json") {
		t.Errorf("Accept = %q，期望含 application/json", f.gotAccept)
	}

	// 发上去的 questions 必须是对象。上游对数组直接报错
	//（Invalid input: expected record, received array），所以这一条是
	// 「形状对不对」的核心。
	var sent struct {
		Model     string                     `json:"model"`
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(f.gotBody, &sent); err != nil {
		t.Fatalf("发上去的 body 解不开: %v\n%s", err, f.gotBody)
	}
	if sent.Model != "upstream-model" || len(sent.Questions) != 2 {
		t.Errorf("发上去的请求不对: %s", f.gotBody)
	}
	if strings.Contains(string(f.gotBody), `"stream"`) {
		t.Errorf("body 里出现了 stream: %s", f.gotBody)
	}

	if resp.Usage.InputTokens != 387 || resp.Usage.OutputTokens != 72 {
		t.Errorf("用量 = %+v", resp.Usage)
	}
	if got := resp.Answers["q_noul"].Noul; got != 0.75 {
		t.Errorf("答案 = %v，期望 0.75", got)
	}
}

// 状态码要翻成带可重试判断的错误：429 与 5xx 值得换渠道，4xx 的其余部分
// 换谁都是同一个错。判据由 httpx 给，这里钉的是「这条路径真的用它」。
func TestDecideMapsStatusToRetryable(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFakeUpstream(t)
			f.status = tc.status
			f.reply = `{"error":{"message":"boom"}}`

			_, err := New(5*time.Second).Decide(context.Background(), f.ch(), sampleRequest())
			var up *channel.ErrUpstream
			if !errors.As(err, &up) {
				t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
			}
			if up.StatusCode != tc.status {
				t.Errorf("状态码 = %d，期望 %d", up.StatusCode, tc.status)
			}
			if up.Retryable != tc.retryable {
				t.Errorf("Retryable = %v，期望 %v", up.Retryable, tc.retryable)
			}
		})
	}
}

// 响应解不开时算**这个渠道**的问题（换一个可能就好），所以标成可重试——
// 与 openaiadapter 同一个判断。
func TestDecideTreatsMalformedResponseAsRetryable(t *testing.T) {
	f := newFakeUpstream(t)
	f.reply = `not json`

	_, err := New(5*time.Second).Decide(context.Background(), f.ch(), sampleRequest())
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
	if !up.Retryable {
		t.Error("解析失败应当可重试")
	}
	// 响应体解不开，但响应本身到了：GotResponse 与状态码都要有值。
	if !up.GotResponse || up.StatusCode != http.StatusOK {
		t.Errorf("GotResponse=%v StatusCode=%d，期望 true / %d",
			up.GotResponse, up.StatusCode, http.StatusOK)
	}
}

// 这条渠道不说 chat 协议。错误要包着 ErrUnsupported：路由层据此**跳过这条
// 候选、试下一个**（见 route 的 worthNextCandidate）。
func TestChatIsUnsupported(t *testing.T) {
	a := New(time.Second)

	if _, err := a.Chat(context.Background(), channel.Channel{}, &channel.Request{}); !errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("Chat 的错误 = %v，期望包着 ErrUnsupported", err)
	}
	if _, err := a.ChatStream(context.Background(), channel.Channel{}, &channel.Request{}); !errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("ChatStream 的错误 = %v，期望包着 ErrUnsupported", err)
	}
	// 「路由层会换下一个候选」与「这条错误可重试」是**两件事**，不要合成
	// 一件：IsRetryable 说的是「这次失败该算在上游头上吗」（它决定渠道健康
	// 度，见 gateway 的 channelAtFault），而一个渠道类型不对不是它坏了。
	// 路由层换下一个候选的判据更宽，见 worthNextCandidate。
	if channel.IsRetryable(channel.ErrUnsupported) {
		t.Error("ErrUnsupported 不该是可重试的——它不算上游的故障")
	}
}

func TestTypeAndProtocol(t *testing.T) {
	a := New(time.Second)
	if a.Type() != "systemone" {
		t.Errorf("Type() = %q", a.Type())
	}
	if a.Protocol() != channel.ProtocolSystemOne {
		t.Errorf("Protocol() = %q，期望 %q", a.Protocol(), channel.ProtocolSystemOne)
	}

	// 端点那一栏：适配器自述的路径要与它声明的协议算出来的那条一致（注册表
	// 启动时也自检这一条，见 channel.Registry.Register），而**类型名必须落在
	// 协议取值上**——选路按渠道的 type 算这条渠道打哪条端点
	// （channel.Protocol(<type>).Endpoint()，见 internal/channels），两者不同名
	// 时算出来是空串（= 不知道 = 放行），这条渠道的端点判据会静默失效。
	if got := a.Endpoint(); got != channel.EndpointSystemOne {
		t.Errorf("Endpoint() = %q，期望 %q", got, channel.EndpointSystemOne)
	}
	if got := channel.Protocol(Type).Endpoint(); got != a.Endpoint() {
		t.Errorf("channel.Protocol(%q).Endpoint() = %q，与适配器自述的 %q 不一致："+
			"Type() 必须与它说的协议取值同名", Type, got, a.Endpoint())
	}
}

func TestUnsupportedCapabilities(t *testing.T) {
	a := New(time.Second)
	if _, err := a.ListModels(context.Background(), channel.Channel{}); !errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("ListModels 的错误 = %v，期望 ErrUnsupported", err)
	}
	if _, err := a.Quota(context.Background(), channel.Channel{}); !errors.Is(err, channel.ErrUnsupported) {
		t.Errorf("Quota 的错误 = %v，期望 ErrUnsupported", err)
	}
}

// 超长的响应体读不进来（有上限），于是它在解析那一步失败，而不是把内存
// 吃掉。报出来的是「解析失败」这一类，与别的形状问题同一个出口。
func TestDecideRejectsOversizedResponse(t *testing.T) {
	f := newFakeUpstream(t)
	f.reply = strings.Repeat("a", maxResponseBytes+1)

	_, err := New(5*time.Second).Decide(context.Background(), f.ch(), sampleRequest())
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("期望 *channel.ErrUpstream，得到 %v", err)
	}
}
