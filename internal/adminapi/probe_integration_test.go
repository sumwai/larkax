package adminapi

// 本文件是适配器枚举与模型探测的端到端用例：真库（渠道归属与凭证解密）
// 加上一个真的 HTTP 假上游（出站行为：请求头、重定向、响应体上限）。
//
// 它们钉的是「形状」：字段名与类型、空数组而不是 null、以及同一个 502
// 底下几种成因各自给什么。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel/httpx"
)

// probeUpstream 起一个假上游，并对每次请求记下 Authorization。
type probeUpstream struct {
	*httptest.Server
	gotAuth string
	paths   []string
}

func newProbeUpstream(t *testing.T, handler http.HandlerFunc) *probeUpstream {
	t.Helper()
	up := &probeUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.gotAuth = r.Header.Get("Authorization")
		up.paths = append(up.paths, r.URL.Path)
		handler(w, r)
	}))
	t.Cleanup(up.Close)
	return up
}

func modelsBody(models ...string) string {
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{"id": model, "object": "model"})
	}
	raw, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return string(raw)
}

type probeModelsResponse struct {
	Supported bool     `json:"supported"`
	Models    []string `json:"models"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated"`
}

// probeCall 以某位商家的身份发一次探测。
func probeCall(t *testing.T, h http.Handler, owner int64, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return merchantCall(t, h, owner, http.MethodPost,
		"/api/v1/merchant/channels/probe-models", body)
}

// TestListAdapters 枚举读的是注册表：四个内置类型，字段名与取值都是契约。
func TestListAdapters(t *testing.T) {
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-adapters-")

	rec := merchantCall(t, h, owner, http.MethodGet, "/api/v1/merchant/adapters", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		Items []struct {
			Type     string `json:"type"`
			Protocol string `json:"protocol"`
			Endpoint string `json:"endpoint"`
		} `json:"items"`
	}
	decode(t, rec, &body)
	if len(body.Items) != 4 {
		t.Fatalf("枚举到 %d 个适配器，期望 4 个：%s", len(body.Items), rec.Body)
	}
	for i := 1; i < len(body.Items); i++ {
		if body.Items[i-1].Type > body.Items[i].Type {
			t.Errorf("类型没有升序：%q 在 %q 之前",
				body.Items[i-1].Type, body.Items[i].Type)
		}
	}
	// 每一项的三个字段都必须是事实，不是空串——空 endpoint 会让前端的
	// 「选了这个就打这条路径」变成一句空话。
	for _, item := range body.Items {
		if item.Type == "" || item.Protocol == "" || item.Endpoint == "" {
			t.Errorf("有字段为空：%+v", item)
		}
		if item.Type != item.Protocol {
			t.Errorf("类型 %q 与协议 %q 不是同一个名字", item.Type, item.Protocol)
		}
	}
}

// TestProbeModelsRawForm 只给地址与凭证：打的是 /v1/models，凭证在
// Authorization 里，清单去重升序后返回。
func TestProbeModelsRawForm(t *testing.T) {
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, modelsBody("z-model", "a-model", "a-model"))
	})
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-raw-")

	rec := probeCall(t, h, owner, map[string]any{
		"type": "openai", "base_url": up.URL, "credential": "sk-probe-placeholder",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var body probeModelsResponse
	decode(t, rec, &body)
	if !body.Supported || body.Total != 2 || len(body.Models) != 2 || body.Truncated {
		t.Errorf("正文 = %+v，期望 supported、两条、未截断", body)
	}
	if strings.Join(body.Models, ",") != "a-model,z-model" {
		t.Errorf("清单 = %v，期望去重后升序", body.Models)
	}
	if up.gotAuth != "Bearer sk-probe-placeholder" {
		t.Errorf("上游收到的 Authorization = %q，期望这次提交的凭证", up.gotAuth)
	}
	if len(up.paths) != 1 || up.paths[0] != "/v1/models" {
		t.Errorf("上游收到的路径 = %v，期望 /v1/models", up.paths)
	}
}

// TestProbeModelsUnsupported 没有列表能力的类型回 200 + supported:false，
// 清单是空数组而不是 null。
func TestProbeModelsUnsupported(t *testing.T) {
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-unsup-")

	rec := probeCall(t, h, owner, map[string]any{
		"type": "systemone", "base_url": "https://api.example.com/v1",
		"credential": "test-key-placeholder",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（「没有这个能力」不是失败）；正文：%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"models":[]`) {
		t.Errorf("模型清单不是空数组：%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"supported":false`) {
		t.Errorf("supported 不是 false：%s", rec.Body)
	}
}

// TestProbeModelsChannelID 给了 id 就从库里取地址与凭证；覆盖凭证只在
// 归属判据之后生效。
func TestProbeModelsChannelID(t *testing.T) {
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, modelsBody("stored-model"))
	})
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-probe-id-")
	other := seedUser(t, db, "m-probe-id-other-")

	channelID := merchantChannel(t, h, owner, uniqueName("probe-ch"), "openai", up.URL,
		[]string{uniqueName("probe-model")})

	// 只给 id：凭证是建渠道时那把占位符。
	rec := probeCall(t, h, owner, map[string]any{"channel_id": channelID})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	if up.gotAuth != "Bearer sk-placeholder" {
		t.Errorf("上游收到的 Authorization = %q，期望库里那一把凭证", up.gotAuth)
	}

	// 覆盖凭证：编辑中的表单还没保存。同一调用方两次探测之间要隔开限流窗口
	// （限流本身由 TestProbeModelsRateLimited 钉）。
	time.Sleep(probeInterval + 100*time.Millisecond)
	rec = probeCall(t, h, owner, map[string]any{
		"channel_id": channelID, "credential": "new-key-placeholder",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("覆盖凭证后状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	if up.gotAuth != "Bearer new-key-placeholder" {
		t.Errorf("覆盖后上游收到 = %q，期望这次提交的凭证", up.gotAuth)
	}

	// 别人的渠道：按不存在处理，而且不能借「换凭证」绕过归属。
	callsBefore := len(up.paths)
	rec = probeCall(t, h, other, map[string]any{
		"channel_id": channelID, "credential": "attacker-placeholder",
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("别人的渠道状态码 = %d，期望 404；正文：%s", rec.Code, rec.Body)
	}
	if len(up.paths) != callsBefore {
		t.Error("归属判据之前就打了上游")
	}
}

// TestProbeModelsUpstreamError 上游报错：502 带状态码与原文，但原文里
// 回显的凭证要被换掉。
func TestProbeModelsUpstreamError(t *testing.T) {
	const credential = "sk-echo-placeholder"
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":"invalid key %s"}`, credential)
	})
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-err-")

	rec := probeCall(t, h, owner, map[string]any{
		"type": "openai", "base_url": up.URL, "credential": credential,
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；正文：%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), credential) {
		t.Errorf("响应体里带着凭证：%s", rec.Body)
	}
	// 按 JSON 解码再判，不拿原文找：编码器会把尖括号转义成 ASCII 转义序列，
	// 原文里根本没有 `<redacted>` 这七个字节。
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if !strings.Contains(body.Error.Message, "401") {
		t.Errorf("响应体没说上游状态码：%q", body.Error.Message)
	}
	if !strings.Contains(body.Error.Message, "<redacted>") {
		t.Errorf("响应体里的凭证没有被换掉：%q", body.Error.Message)
	}
}

// TestProbeModelsResponseTooLarge 响应体超过上限：502 带自己的错误码，
// 与「上游挂了」分开。
func TestProbeModelsResponseTooLarge(t *testing.T) {
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		// 不给 Content-Length，让上限在读的时候生效。正文必须是**合法的 JSON
		// 前缀**：坏 JSON 会在第一个字节就被解码器拒掉，走成「上游的 JSON 坏了」，
		// 根本读不到上限那一层。这里一直吐合法的数组元素，直到读侧先喊停。
		_, _ = io.WriteString(w, `{"object":"list","data":[`)
		entry := `{"id":"m"},`
		for written := 0; written <= httpx.ProbeBodyLimit; written += len(entry) {
			if _, err := io.WriteString(w, entry); err != nil {
				return
			}
		}
	})
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-big-")

	rec := probeCall(t, h, owner, map[string]any{
		"type": "openai", "base_url": up.URL, "credential": "test-key-placeholder",
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Code    *string `json:"code"`
			Message string  `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Error.Code == nil || *body.Error.Code != "upstream_response_too_large" {
		t.Errorf("错误码 = %v，期望 upstream_response_too_large", body.Error.Code)
	}
}

// TestProbeModelsDoesNotFollowRedirects 上游回 302：探测不跟，按上游 3xx
// 失败处理。
func TestProbeModelsDoesNotFollowRedirects(t *testing.T) {
	var followed bool
	up := newProbeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			_, _ = io.WriteString(w, modelsBody("model-from-elsewhere"))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-redir-")

	rec := probeCall(t, h, owner, map[string]any{
		"type": "openai", "base_url": up.URL, "credential": "test-key-placeholder",
	})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；正文：%s", rec.Code, rec.Body)
	}
	if followed {
		t.Error("探测跟随了重定向，打到了另一个地址")
	}
	if strings.Contains(rec.Body.String(), "model-from-elsewhere") {
		t.Errorf("响应里出现了跳转目标那一侧的内容：%s", rec.Body)
	}
}

// TestProbeModelsRateLimited 同一调用方连续两次：第二次 429 并给
// Retry-After。
func TestProbeModelsRateLimited(t *testing.T) {
	up := newProbeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, modelsBody("m"))
	})
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-rate-")
	body := map[string]any{
		"type": "openai", "base_url": up.URL, "credential": "test-key-placeholder",
	}

	if rec := probeCall(t, h, owner, body); rec.Code != http.StatusOK {
		t.Fatalf("第一次状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	rec := probeCall(t, h, owner, body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第二次状态码 = %d，期望 429；正文：%s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 没有 Retry-After")
	}
}

// TestProbeModelsRejectsUnknownFields 请求体里写错字段名是 400，不是
// 静默忽略——与其余管理端点同一条判据。
func TestProbeModelsRejectsUnknownFields(t *testing.T) {
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-fields-")
	rec := probeCall(t, h, owner, map[string]any{
		"type": "openai", "base_url": "https://api.example.com/v1", "api_key": "x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d，期望 400；正文：%s", rec.Code, rec.Body)
	}
}
