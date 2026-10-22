package adminapi

// 本文件是适配器枚举与模型探测的端到端用例：真库（渠道归属与凭证解密）
// 加上一个真的 HTTP 假上游（出站行为：请求头、重定向、响应体上限）。
//
// 它们钉的是「形状」：字段名与类型、空数组而不是 null、以及同一个 502
// 底下几种成因各自给什么。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/channel/openaiadapter"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/upstream"
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

// TestProbeModelsNotFoundMatchesChannelRoute 钉住商家面上「不是你的渠道」
// 的两条 404 逐字相同：探测与渠道资源走的是同一句固定正文。
//
// 与 TestProbeModelsChannelID 里那条「别人的渠道回 404」不同，这里比的是
// **正文**。只判状态码时，探测回一句带 id 的中文、渠道路由回 `not found`，
// 用例照样是绿的。
func TestProbeModelsNotFoundMatchesChannelRoute(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-probe-404-")
	other := seedUser(t, db, "m-probe-404-other-")

	id := merchantChannel(t, h, other, uniqueName("probe-404-ch"), "openai",
		"https://api.example.com/v1", []string{uniqueName("probe-404-model")})

	probed := probeCall(t, h, owner, map[string]any{"channel_id": id})
	if probed.Code != http.StatusNotFound {
		t.Fatalf("探测别人的渠道状态码 = %d，期望 404；正文：%s", probed.Code, probed.Body)
	}
	read := merchantCall(t, h, owner, http.MethodGet,
		"/api/v1/merchant/channels/"+itoa(id), nil)
	if read.Code != http.StatusNotFound {
		t.Fatalf("读别人的渠道状态码 = %d，期望 404；正文：%s", read.Code, read.Body)
	}
	if probed.Body.String() != read.Body.String() {
		t.Errorf("探测与渠道路由的 404 正文不同：\n探测：%s\n路由：%s",
			probed.Body, read.Body)
	}
	// 逐字对照整份错误体：只判「含 not found」时，多出来的 id 或换一处
	// 字段名都不会红。
	const want = `{"error":{"message":"not found","type":"invalid_request_error","param":null,"code":null}}`
	if probed.Body.String() != want {
		t.Errorf("404 正文 = %q，期望逐字 %q", probed.Body.String(), want)
	}
}

// TestProbeModelsCorruptCredential 密文损坏是 500：这一行的数据坏了，
// 换一次请求也不会好，更不是调用方写错了请求体。
func TestProbeModelsCorruptCredential(t *testing.T) {
	h := merchantSurface(t, "UTC")
	db := testDB(t)
	owner := seedUser(t, db, "m-probe-corrupt-")

	id := merchantChannel(t, h, owner, uniqueName("probe-corrupt-ch"), "openai",
		"https://api.example.com/v1", []string{uniqueName("probe-corrupt-model")})
	// 头部版本号对不上，Sealer 会报 ErrMalformed → ErrCredentialCorrupt。
	if _, err := db.ExecContext(t.Context(),
		`UPDATE channels SET credential_enc = $1 WHERE id = $2`,
		[]byte("not-a-credential-envelope"), id); err != nil {
		t.Fatalf("写坏密文失败: %v", err)
	}

	rec := probeCall(t, h, owner, map[string]any{"channel_id": id})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("坏密文的状态码 = %d，期望 500；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, rec, &body)
	if body.Error.Message != msgProbeCorruptCredential {
		t.Errorf("正文 = %q，期望逐字 %q", body.Error.Message, msgProbeCorruptCredential)
	}
}

// TestProbeModelsInvalidBaseURLIsEnglish 探测的正文统一是英文：
// base_url 的原文（NormalizeBaseURL，命令行在用）留在日志里。
func TestProbeModelsInvalidBaseURLIsEnglish(t *testing.T) {
	h := merchantSurface(t, "UTC")
	owner := seedUser(t, testDB(t), "m-probe-url-")

	rec := probeCall(t, h, owner, map[string]any{
		"type": "openai", "base_url": "https://api.example.com/v1?tenant=abc",
		"credential": "test-key-placeholder",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, rec, &body)
	if body.Error.Message != msgProbeInvalidBaseURL {
		t.Errorf("正文 = %q，期望逐字 %q", body.Error.Message, msgProbeInvalidBaseURL)
	}
	for _, r := range body.Error.Message {
		if r > unicode.MaxASCII {
			t.Errorf("正文里混进了非 ASCII 字符 %q：%s", r, body.Error.Message)
		}
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

// stallUpstream 起一个「收了请求但一直不回响应头」的假上游。
//
// 它让探测自己的时限先到；handler 在客户端断开后退出，用例不会留下悬着的连接。
func stallUpstream(t *testing.T) *probeUpstream {
	t.Helper()
	release := make(chan struct{})
	up := newProbeUpstream(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	// 清理顺序：先放 handler 出去（它可能还在等），再关假上游。
	t.Cleanup(func() { close(release) })
	return up
}

// probeErrWithClient 用一个给定的客户端装配单适配器注册表，向 baseURL 探测
// 一次，返回**真实路径**产生的错误（适配器包装、凭证脱敏都经过）。
func probeErrWithClient(t *testing.T, client *http.Client, baseURL string) error {
	t.Helper()
	registry := channel.NewRegistry()
	if err := registry.Register(openaiadapter.NewWithClient(client)); err != nil {
		t.Fatalf("注册适配器失败: %v", err)
	}
	// 五秒护栏：下面那条只设 ResponseHeaderTimeout 的客户端没有整体时限，
	// 判据失效时用它把用例停下来（时长断言会先报错）。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := channels.NewModelProbe(nil, nil, registry).Probe(ctx, channels.ProbeRequest{
		Type: "openai", BaseURL: baseURL, Credential: "test-key-placeholder",
	})
	if err == nil {
		t.Fatal("上游不回响应头，探测却成功了")
	}
	return err
}

// assertGatewayTimeout 把真实错误交给 failProbe，断言它落成 504。
func assertGatewayTimeout(t *testing.T, err error) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/merchant/channels/probe-models", nil)
	(&API{}).failProbe(rec, req, err)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d，期望 504；正文：%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), upstream.ProbeTimeout.String()) {
		t.Errorf("正文没说时限：%s", rec.Body)
	}
}

// TestProbeTimeoutMapsToGatewayTimeout 上游不回响应头：探测的时限先到，回 504
// 而不是 502。
//
// 两条时限分开测：http.Client 的整体 Timeout 与 transport 的
// ResponseHeaderTimeout 是 http 包里两个不同的机制，抛出的错误形状也不一样
// （前者由客户端计时器取消，后者由 transport 报「等响应头超时」），但都
// **不含 context.DeadlineExceeded**——只测其中一条会漏掉另一条。
//
// 用真的客户端与真的适配器，把时限压到 100ms 让用例快；不拿构造的
// context.DeadlineExceeded 喂分类函数，那恰好会掩盖这次的问题。
func TestProbeTimeoutMapsToGatewayTimeout(t *testing.T) {
	t.Run("Client.Timeout", func(t *testing.T) {
		up := stallUpstream(t)
		// 裸客户端只设整体时限：ProbeClient 会同时设 Client.Timeout 与
		// ResponseHeaderTimeout，两个都是 100ms 时分不清是谁先到。
		client := &http.Client{Timeout: 100 * time.Millisecond}
		start := time.Now()
		err := probeErrWithClient(t, client, up.URL)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("超时没有按客户端时限发生：%s", elapsed)
		}
		assertGatewayTimeout(t, err)
	})

	t.Run("ResponseHeaderTimeout", func(t *testing.T) {
		up := stallUpstream(t)
		// 只设响应头时限：这条路径的错误不经过 Client.Timeout。
		client := &http.Client{
			Transport: &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond},
		}
		start := time.Now()
		err := probeErrWithClient(t, client, up.URL)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("ResponseHeaderTimeout 没有生效：%s", elapsed)
		}
		assertGatewayTimeout(t, err)
	})
}

// TestProbeConnectionRefusedIsNotTimeout 连接被拒不是超时：502 带
// upstream_unreachable，不能因为「都是网络错误」就报成 504。
func TestProbeConnectionRefusedIsNotTimeout(t *testing.T) {
	// 拿一个刚关掉的端口：连过去必被拒，没有任何响应。
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("关闭监听失败: %v", err)
	}

	probeErr := probeErrWithClient(t, httpx.ProbeClient(time.Second), "http://"+addr)
	if isUpstreamTimeout(probeErr) {
		t.Fatalf("连接被拒被判成了超时：%v", probeErr)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/merchant/channels/probe-models", nil)
	(&API{}).failProbe(rec, req, probeErr)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；正文：%s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Error.Code == nil || *body.Error.Code != "upstream_unreachable" {
		t.Errorf("错误码 = %v，期望 upstream_unreachable", body.Error.Code)
	}
}
