package snapshotwire

import (
	"context"
	"fmt"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
)

// consentStub 是可控的授权来源，并把每一次判定收到的身份记下来。
//
// # 为什么要记身份
//
// 「判定发生了」与「判定拿到了正确的身份」是两件事。user_id 与 request_id
// 是授权记录上的两个范围维度（按一段时间、按单条请求），传成零值的后果是
// 按单条请求那一档**永远匹配不上**——落下来的每条都是骨架，而它与「用户
// 还没授权」在库里长得一模一样，看不出是接线漏了。
type consentStub struct {
	allow bool
	calls []consentCall
}

type consentCall struct {
	ctx       context.Context
	userID    int64
	requestID string
}

func (s *consentStub) AllowsRawCapture(ctx context.Context, userID int64, requestID string) bool {
	s.calls = append(s.calls, consentCall{ctx: ctx, userID: userID, requestID: requestID})
	return s.allow
}

// consentCtxKey 是往 ctx 里塞一个可辨认的值用的键，判「ctx 有没有一路传下去」。
type consentCtxKey struct{}

func consentRequest(marker string) *channel.Request {
	return &channel.Request{
		Model:     "gpt-4o-2024",
		MaxTokens: 512,
		Messages: []channel.Message{{
			Role:    "user",
			Content: []channel.ContentBlock{{Type: "text", Text: marker}},
		}},
	}
}

func consentResponse(marker string) *channel.Response {
	return &channel.Response{
		UpstreamModel: "gpt-4o-2024",
		Message: channel.Message{
			Role:    "assistant",
			Content: []channel.ContentBlock{{Type: "text", Text: marker}},
		},
		Usage: channel.Usage{PromptTokens: 1, CompletionTokens: 1},
	}
}

// 判定 × 方向四格：每一格同时判「落下来的采集档」与「正文里有没有原文」。
//
// # 为什么两件事要一起判
//
// 只判采集档的话，一个「档写对了但正文仍是脱敏的」实现能过——而那一档的
// 全部意义就是正文里有原文。只判正文的话，一个「留了原文却仍标 skeleton」
// 的实现也能过——那条行在库里读起来是脱敏结构，读的人不会按原文对待它。
// 两件事各判一次，任一边单独错都会红。
//
// # 为什么四格都要在
//
// 四个组合里有两种「看起来对」的错法：无授权却落了原文（泄露），有授权却
// 仍是骨架（判定没接上，而这恰好也**是安全的**，所以它不会以任何故障的
// 形式出现）。前者只在无授权那两格会红，后者只在有授权那两格会红。
func TestCaptureTierFollowsConsent(t *testing.T) {
	for _, allow := range []bool{false, true} {
		for _, dir := range []string{"request", "response"} {
			t.Run(fmt.Sprintf("%s/授权=%t", dir, allow), func(t *testing.T) {
				src := &consentStub{allow: allow}
				c := newCollectorWithConsent(t, testConfig(), &fakeDB{}, src)
				marker := "原样留下的" + dir + "内容"

				var snap *gateway.Snapshot
				var err error
				if dir == "request" {
					snap, err = c.Redact(context.Background(), consentRequest(marker),
						gateway.CaptureIdentity{})
				} else {
					snap, err = c.RedactResponse(context.Background(), consentResponse(marker),
						gateway.CaptureIdentity{})
				}
				if err != nil {
					t.Fatalf("脱敏失败: %v", err)
				}

				wantMode := captureModeSkeleton
				if allow {
					wantMode = captureModeFullConsented
				}
				if snap.CaptureMode != wantMode {
					t.Errorf("capture_mode = %q，期望 %q", snap.CaptureMode, wantMode)
				}
				if got := containsString(snap.Skeleton, marker); got != allow {
					t.Errorf("授权 = %t，正文里出现原文 = %t——两者必须一致（正文为 %v）",
						allow, got, snap.Skeleton)
				}
				// 原文无论走哪一档都不许出现在路径清单里：那份清单是给人看的
				// 字段名，混进内容等于换了个地方泄露。
				if containsString(snap.RedactedPaths, marker) {
					t.Errorf("redacted_paths 里出现了原文: %v", snap.RedactedPaths)
				}
				// 原样那一档一个字段都没替换，所以清单必须是空的；反过来，
				// 默认那一档至少要替换掉那段内容——清单为空说明内容没被替换，
				// 而那正是「有授权」与「没授权」唯一的差别。
				if allow && len(snap.RedactedPaths) != 0 {
					t.Errorf("原样采集时不该有被替换的字段，实际 %v", snap.RedactedPaths)
				}
				if !allow && dir == "request" && len(snap.RedactedPaths) == 0 {
					t.Errorf("默认那一档应当记下被替换的字段，实际 %v", snap.RedactedPaths)
				}
			})
		}
	}
}

// 判定拿到的身份就是调用方给的那一份，ctx 也一路传下去。
//
// ctx 不是可选项：判定可能要查一次授权记录，而这是请求路径上的调用——
// 客户端走掉之后那次查库不该接着等。传 context.Background() 顶掉调用方的
// 取消信号，会让一条已经没人等的请求继续占着数据库连接。
//
// 一次 Redact 只判一次：两次判定之间授权可能变（撤销就落在那段窗口里），
// 而「这次请求」应当只有一个答案。廉价地多判几次不算错，但它让上一条
// 的性质（同一个身份得到同一档）变得依赖于「两次调用之间表没变」。
func TestConsentReceivesIdentityOnce(t *testing.T) {
	src := &consentStub{}
	c := newCollectorWithConsent(t, testConfig(), &fakeDB{}, src)
	ctx := context.WithValue(context.Background(), consentCtxKey{}, "sentinel")

	id := gateway.CaptureIdentity{UserID: 4242, RequestID: "req-consent-1"}
	if _, err := c.Redact(ctx, consentRequest("随便一段"), id); err != nil {
		t.Fatalf("脱敏失败: %v", err)
	}

	if len(src.calls) != 1 {
		t.Fatalf("判定次数 = %d，期望 1", len(src.calls))
	}
	call := src.calls[0]
	if call.userID != id.UserID {
		t.Errorf("判定收到的 user_id = %d，期望 %d", call.userID, id.UserID)
	}
	if call.requestID != id.RequestID {
		t.Errorf("判定收到的 request_id = %q，期望 %q", call.requestID, id.RequestID)
	}
	if call.ctx.Value(consentCtxKey{}) != "sentinel" {
		t.Error("判定收到的 ctx 不是调用方那一个——取消信号传不到那次查库")
	}
}

// 没有可脱敏的响应体时，采集档照记。
//
// 那一行的正文是空的（连不上、超时），但「这一次是开着授权的」仍然是事实：
// 读的人要能把它与「没授权可开」分开。空正文落成哪一档不影响内容安全，
// 所以这里判的是「档跟着判定走」，而不是「反正正文是空的随便哪档都行」。
func TestEmptyResponseStillCarriesCaptureMode(t *testing.T) {
	for _, allow := range []bool{false, true} {
		src := &consentStub{allow: allow}
		c := newCollectorWithConsent(t, testConfig(), &fakeDB{}, src)

		snap, err := c.RedactResponse(context.Background(), nil, gateway.CaptureIdentity{})
		if err != nil {
			t.Fatalf("nil 响应不该报错: %v", err)
		}
		wantMode := captureModeSkeleton
		if allow {
			wantMode = captureModeFullConsented
		}
		if snap.CaptureMode != wantMode {
			t.Errorf("授权 = %t 时 capture_mode = %q，期望 %q", allow, snap.CaptureMode, wantMode)
		}
		if len(snap.Skeleton) != 0 {
			t.Errorf("没有响应体时正文应当为空，实际 %v", snap.Skeleton)
		}
	}
}

// nil 授权来源在建链路时就拒掉，不要留到请求路径上 panic。
//
// 两者都不是「不采集」：不采集是配置里的开关（Enabled），而那一条路
// 根本不建脱敏器。nil 是「忘了接」，而它的第一次显形会是在一条真实请求
// 的 goroutine 里——那时它表现为一次 panic，与授权判定毫无关系的样子。
func TestNewRejectsNilConsent(t *testing.T) {
	if _, err := New(testConfig(), &fakeDB{}, nil, quietLogger()); err == nil {
		t.Error("nil 授权来源应当报错")
	}
	// 空实现是**一个答案**（永远没有授权），不是 nil：它就是没有授权入口
	// 那个部署的形状，必须能建起来。
	if _, err := New(testConfig(), &fakeDB{}, NoConsentSource{}, quietLogger()); err != nil {
		t.Errorf("空实现应当能建起链路: %v", err)
	}
	if (NoConsentSource{}).AllowsRawCapture(context.Background(), 1, "req-1") {
		t.Error("空实现必须回答「没有授权」——它朝安全那一边倒")
	}
}

// CaptureModeIsRaw 只认那一个取值。
//
// 读侧（snapshot get）靠它决定正文标题与降级附注怎么写，而两种正文在终端上
// 长得一样。判宽了的后果是把一份脱敏结构说成「用户原文」——读的人会按原文
// 对待它，于是一条本该看不见的内容被当成「上游就发了这些」。
func TestCaptureModeIsRawOnlyForFullConsented(t *testing.T) {
	cases := []struct {
		mode string
		raw  bool
	}{
		{captureModeFullConsented, true},
		{captureModeSkeleton, false},
		{"", false},
		{"full", false},
		{"FULL_CONSENTED", false},
		{"full_consented ", false},
	}
	for _, tc := range cases {
		if got := CaptureModeIsRaw(tc.mode); got != tc.raw {
			t.Errorf("CaptureModeIsRaw(%q) = %t，期望 %t", tc.mode, got, tc.raw)
		}
	}
}
