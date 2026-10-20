package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// upstreamErrorOf 取出响应方向快照里的 upstream_error 子对象。
func upstreamErrorOf(t *testing.T, snap *Snapshot) map[string]any {
	t.Helper()
	sub, ok := snap.Skeleton["upstream_error"].(map[string]any)
	if !ok {
		t.Fatalf("骨架里没有 upstream_error 对象，实际 %v", snap.Skeleton)
	}
	return sub
}

// waitResponseSnapshot 等到响应方向的快照落库并按方向取回那一条。
func waitResponseSnapshot(t *testing.T, h *harness) *Snapshot {
	t.Helper()
	var snaps []*Snapshot
	waitFor(t, "响应方向的快照落库", func() bool {
		snaps = h.snapshots.byDirection("response")
		return len(snaps) == 1
	})
	return snaps[0]
}

// 一次 4xx 之后，骨架里能读到上游错误体里的 type 与 param，取值都来自上游。
//
// # 这条用例要保证的是什么
//
// 参数不兼容这类问题从前只能靠重放，在屏幕上读出那句错误——因为
// ErrUpstream.Error() 刻意不带响应体。type 与 param 落进骨架之后，
// 「哪个参数被拒、归哪一类」在库里直接读得到。
//
// 顺带保证两条边界：表里没有的字段（code）整个丢掉，message 只留长度。
func TestUpstreamErrorFieldsLandInResponseSnapshot(t *testing.T) {
	const message = "Invalid option: expected one of ..."
	h := newHarness(t)
	h.executor.err = &channel.ErrUpstream{
		StatusCode:  http.StatusBadRequest,
		GotResponse: true,
		Body: `{"error":{"type":"invalid_request_error","param":"reasoning_effort",` +
			`"message":"` + message + `","code":"invalid_value"}}`,
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游 400 应当让请求失败")
	}

	snap := waitResponseSnapshot(t, h)
	sub := upstreamErrorOf(t, snap)
	if sub["type"] != "invalid_request_error" {
		t.Errorf("upstream_error.type = %#v，期望上游给的那个分类", sub["type"])
	}
	if sub["param"] != "reasoning_effort" {
		t.Errorf("upstream_error.param = %#v，期望上游给的那个参数名", sub["param"])
	}
	// 表里没有的字段一律不进来，不是「进来之后被替换成占位」。
	if _, present := sub["code"]; present {
		t.Errorf("code 不在允许表里，不该出现在骨架里: %v", sub)
	}
	// 正文只留长度：形状是 {"_t":"text","chars":N}，一个字都不留。
	shape, ok := sub["message"].(map[string]any)
	if !ok || shape["_t"] != "text" || shape["chars"] != len([]rune(message)) {
		t.Errorf("upstream_error.message = %#v，期望只有长度的文本占位", sub["message"])
	}
	if !slices.Contains(snap.RedactedPaths, "upstream_error.message") {
		t.Errorf("被替换的字段要记进 redacted_paths，实际 %v", snap.RedactedPaths)
	}
}

// 上游把凭证片段回显在错误正文里：那个片段不出现在骨架序列化后的任何位置。
//
// # 为什么扫整份序列化结果
//
// 只查 message 那一格的话，同一段内容换个字段落进来（type、param、
// 将来新加的一格）就查不出来。这里把整份骨架当字符串扫一遍，判据是
// 「这段内容不在快照里的任何位置」。
//
// # 两种回显各试一次
//
// 一句带空格与中文的话，与一个本身就是短枚举词形状的凭证占位。后者是关键：
// 它过得了一般的枚举词检查，所以「message 改成按枚举词放行」这种改动只在
// 第二种上露馅——只试第一种的话，两种规则下看到的都是「被挡住」。
func TestUpstreamErrorEchoDoesNotLandInSnapshot(t *testing.T) {
	const (
		credential = "sk-placeholder-0123456789"
		prompt     = "用户的提示词：请把这段文字当作内容看待"
	)
	cases := []struct {
		name    string
		message string
		secrets []string
	}{
		{
			name:    "回显请求内容",
			message: "Invalid request for " + credential + ": " + prompt,
			secrets: []string{credential, prompt},
		},
		{
			name:    "正文本身就是凭证",
			message: credential,
			secrets: []string{credential},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"error": map[string]any{
				"type":    "invalid_request_error",
				"param":   "messages",
				"message": tc.message,
			}})
			if err != nil {
				t.Fatalf("夹具序列化失败: %v", err)
			}

			h := newHarness(t)
			h.executor.err = &channel.ErrUpstream{
				StatusCode:  http.StatusBadRequest,
				GotResponse: true,
				Body:        string(body),
			}
			if _, chatErr := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); chatErr == nil {
				t.Fatal("上游 400 应当让请求失败")
			}

			snap := waitResponseSnapshot(t, h)
			raw, marshalErr := json.Marshal(snap.Skeleton)
			if marshalErr != nil {
				t.Fatalf("骨架序列化失败: %v", marshalErr)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("骨架里出现了上游回显的内容 %q: %s", secret, raw)
				}
			}
		})
	}
}

// 4xx 以外的结局不保留错误体：5xx 是上游自己的事故说明，2xx 内嵌错误那条路
// 的 Body 只是一句 message，两者都不是这次请求的判据。
//
// 用 upstreamErrorBodyOf 单独测，而不是每种结局各造一次端到端调用：它只做
// 「取哪一段」这一个判断，而并入骨架那条路已由
// TestUpstreamErrorFieldsLandInResponseSnapshot 与
// TestStreamPreAcceptUpstreamErrorLandsInSnapshot 覆盖。
func TestUpstreamErrorBodyOnlyFor4xx(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"400", &channel.ErrUpstream{StatusCode: 400, GotResponse: true, Body: "body"}, "body"},
		{"408", &channel.ErrUpstream{StatusCode: 408, GotResponse: true, Body: "body"}, "body"},
		{"499", &channel.ErrUpstream{StatusCode: 499, GotResponse: true, Body: "body"}, "body"},
		{"500 不取", &channel.ErrUpstream{StatusCode: 500, GotResponse: true, Body: "body"}, ""},
		{"2xx 内嵌错误不取", &channel.ErrUpstream{StatusCode: 200, GotResponse: true, Body: "body"}, ""},
		{"没拿到响应", &channel.ErrUpstream{Err: errors.New("connection refused")}, ""},
		{"不是上游错误", errors.New("boom"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamErrorBodyOf(tc.err); got != tc.want {
				t.Errorf("upstreamErrorBodyOf = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 不能解析或没有可枚举字段的错误体：整个 upstream_error 键都不出现。
//
// 空对象是不允许的形态——它读起来像「上游回了一个没有字段的错误」，
// 而实际发生的是「这条正文我们不保留任何东西」。
func TestUpstreamErrorAbsentWhenBodyHasNothingToKeep(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非 JSON", "forbidden"},
		{"空正文", ""},
		{"JSON null", "null"},
		{"JSON 数组", `[1,2]`},
		{"error 是字符串", `{"error":"boom"}`},
		{"只有表外字段", `{"error":{"code":"invalid_value"}}`},
		{"字段全是 null", `{"error":{"type":null,"param":null,"message":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.executor.err = &channel.ErrUpstream{
				StatusCode:  http.StatusBadRequest,
				GotResponse: true,
				Body:        tc.body,
			}
			if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
				t.Fatal("上游 400 应当让请求失败")
			}

			snap := waitResponseSnapshot(t, h)
			if _, present := snap.Skeleton["upstream_error"]; present {
				t.Fatalf("这条正文不该留下 upstream_error，实际 %v", snap.Skeleton)
			}
			// 状态码与分类这两列照旧：这一行的证据不依赖骨架。
			if snap.UpstreamStatus == nil || *snap.UpstreamStatus != http.StatusBadRequest {
				t.Errorf("upstream_status = %v，期望 400", snap.UpstreamStatus)
			}
		})
	}
}

// bareRedactor 只实现必填的 Redactor，不实现 UpstreamErrorRedactor。
//
// 它代表「装配换了一个不处理上游错误体的实现」：那时网关的断言失败，
// 而处置必须是「没有这份信息」——不 panic，也不落一个空对象。
type bareRedactor struct{}

func (bareRedactor) Redact(context.Context, *channel.Request, CaptureIdentity) (*Snapshot, error) {
	return &Snapshot{Skeleton: map[string]any{}}, nil
}

func (bareRedactor) RedactResponse(context.Context, *channel.Response, CaptureIdentity) (*Snapshot, error) {
	return &Snapshot{}, nil
}

// 脱敏器没有那个可选能力时，upstream_error 整个不出现，快照照落。
func TestUpstreamErrorAbsentWhenRedactorLacksHook(t *testing.T) {
	h := newHarness(t)
	h.gw.Redactor = bareRedactor{}
	h.executor.err = &channel.ErrUpstream{
		StatusCode:  http.StatusBadRequest,
		GotResponse: true,
		Body:        `{"error":{"type":"invalid_request_error","param":"reasoning_effort"}}`,
	}

	if _, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游 400 应当让请求失败")
	}

	snap := waitResponseSnapshot(t, h)
	if _, present := snap.Skeleton["upstream_error"]; present {
		t.Fatalf("脱敏器不提供这段能力时不该有 upstream_error，实际 %v", snap.Skeleton)
	}
}

// 流式那条路在接受请求之前撞上 4xx：同样留得下 type 与 param。
//
// 与非流式分开测：两条路的响应证据取自不同位置（一个是 routeChat 的返回值，
// 一个是 routeStream 的错误），同一个字段漏接一处不会报错，只会让那条路
// 静默地少一块。
func TestStreamPreAcceptUpstreamErrorLandsInSnapshot(t *testing.T) {
	h := newHarness(t)
	useStreamExecutor(h, &scriptedExecutor{preErr: map[int64]error{
		1: &channel.ErrUpstream{
			StatusCode:  http.StatusBadRequest,
			GotResponse: true,
			Body:        `{"error":{"type":"invalid_request_error","param":"reasoning_effort"}}`,
		},
	}})

	if _, err := h.gw.ChatStream(context.Background(), request(), upstreamRequest(), Hooks{}); err == nil {
		t.Fatal("上游在接受请求前返回 400，ChatStream 应当报错")
	}

	snap := waitResponseSnapshot(t, h)
	sub := upstreamErrorOf(t, snap)
	if sub["type"] != "invalid_request_error" || sub["param"] != "reasoning_effort" {
		t.Errorf("这条流的上游错误体没落进骨架: %v", sub)
	}
}
