package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/gateway"
)

// 超过单次输出上限是**调用方的**请求问题：他改小 max_tokens 就能成功。
// 所以状态码要落在 4xx，而且那句话说全——这次声明了多少、允许到多少。
//
// 只说一句「超限」调用方只能猜着改，猜错一次就是一次 400；回 500 的话他会
// 去查我们的日志，而真正要改的是他自己那一行请求。
func TestMaxTokensOverCapIsClientError(t *testing.T) {
	h, _, _ := newTestHandler()
	h.Chatter = &fakeChatter{err: &gateway.OutputCapExceeded{
		Model:    "gpt-4o",
		Declared: 100000,
		Limit:    8192,
	}}

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
	// 与缺 max_tokens 那条同一个类型：两者都是「这个字段不能用」，
	// 调用方按类型分支时不该再去分辨两个都表示「改请求」的类型。
	if body.Error.Type != "invalid_request_error" {
		t.Errorf("错误类型 = %q，期望 invalid_request_error", body.Error.Type)
	}
	// 三个都要在：字段名（改哪个）、声明的值（他写了什么）、上限（改成多少
	// 以内）。少一个都得猜。
	for _, want := range []string{"max_tokens", "100000", "8192"} {
		if !strings.Contains(body.Error.Message, want) {
			t.Errorf("错误体里没有 %q: %q", want, body.Error.Message)
		}
	}
}
