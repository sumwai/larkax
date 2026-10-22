package assembly

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/upstream"
)

// 端到端：**一条渠道同时声明两套协议**。
//
// # 它在钉什么
//
// 加多协议之前，同一家上游的两套端点只能各建一条渠道、各填一份凭证，于是
// 轮换密钥漏掉一处就是半个站点的 401。这一组用例把那条路径走一遍完整的：
//
//   - 同一个渠道 id、同一把凭证，OpenAI 与 Anthropic 两条入口都成功，并且在
//     billing_requests 里记在**同一条渠道**上；
//   - 换一次凭证，两条入口同时改用新凭证（旧凭证立刻失效）。
//
// # 为什么假上游要按路径回两套形状
//
// 执行器按入口协议分派到不同的适配器（见 internal/upstream）。分派错了的表现
// 不是 4xx，而是**把另一套形状的响应体交给调用方**——例如 Anthropic 入口
// 拿到一个 chat.completion 对象，客户端解析出来的正文是空的。所以假上游必须
// 对两条路径各回各的形状，用例再从响应体上判「调用方拿到的是自己那套」。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。

// dualProtocolUpstream 是一个本地假上游：按**路径**决定回哪套形状。
//
// 与 fakeUpstream 分开，是因为那个类型一个实例只说一套协议；这里要的是
// 「一个地址、两种端点」，而两者的渠道形状本来就不同（一个渠道 vs 两条）。
//
// 它额外校验凭证：两条路径收到的凭证都必须等于 require 设的那一个。这让
// 「轮换之后两条都用了新凭证」成为一条正向证据，而不是「反正都成功」。
type dualProtocolUpstream struct {
	mu        sync.Mutex
	plaintext string
	paths     []string
	seen      []string
}

func newDualProtocolUpstream(plaintext string) *dualProtocolUpstream {
	return &dualProtocolUpstream{plaintext: plaintext}
}

// require 换掉期望凭证：下一次之后的请求必须带它。
func (f *dualProtocolUpstream) require(plaintext string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plaintext = plaintext
}

// observed 返回到目前为止收到的路径与凭证（按顺序）。
func (f *dualProtocolUpstream) observed() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.seen...)
}

// reset 清空记录，便于一段用例只看自己那几次调用。
func (f *dualProtocolUpstream) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = nil
	f.seen = nil
}

const (
	// 两套协议各回各的形状，正文与用量相同：金额因此可以共用一个期望值，
	// 而形状不同足以证明分派去了哪个适配器。
	dualOpenAIBody = `{"id":"cmpl-1","model":"upstream-model",
		"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"你好"}}],
		"usage":{"prompt_tokens":12,"completion_tokens":7}}`
	dualAnthropicBody = `{"id":"msg_1","type":"message","role":"assistant",
		"content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":7}}`
)

func (f *dualProtocolUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)

		// 两条协议把凭证放在不同的头里：OpenAI 用 Authorization，
		// Anthropic 用 x-api-key。按路径取，不混着看。
		sent := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if sent == "" {
			sent = r.Header.Get("x-api-key")
		}

		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.seen = append(f.seen, sent)
		want := f.plaintext
		f.mu.Unlock()

		if sent != want {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad credential","type":"authentication_error"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, dualOpenAIBody)
		case "/v1/messages":
			_, _ = io.WriteString(w, dualAnthropicBody)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"no such endpoint"}}`)
		}
	})
}

// seedDualProtocolChannel 用写入路径建一条声明两套协议的渠道，返回它的 id。
//
// 走 channels.Admin 而不是直接 INSERT：这一组要证明的正是「写入侧写进去的
// 声明，读取侧按它分派」，手写两行 INSERT 会绕开被跳过来的那一半。
func seedDualProtocolChannel(t *testing.T, db *sql.DB, sealer *credential.Sealer, owner int64, baseURL, model, plaintext string) int64 {
	t.Helper()

	registry, err := upstream.Registry()
	if err != nil {
		t.Fatalf("装配适配器注册表失败: %v", err)
	}
	admin := channels.NewAdmin(db, sealer, registry)

	id, err := admin.Create(t.Context(), channels.CreateRequest{
		OwnerUserID: owner,
		Name:        unique("e2e-dual-channel"),
		Protocols:   []string{string(channel.ProtocolOpenAI), string(channel.ProtocolAnthropic)},
		BaseURL:     baseURL,
		Credential:  plaintext,
		Models:      []string{model},
		Weight:      1,
	})
	if err != nil {
		t.Fatalf("建双协议渠道失败: %v", err)
	}
	return id
}

// 同一渠道下两条入口都成功，且两次记账落在同一条渠道上。
//
// 这一条同时钉住三件事：
//
//   - 分派按入口协议走（响应形状各自对）；
//   - 凭证是同一把（假上游两条路径都校验它）；
//   - 账单的 channel_id 相同（criterion 1 的字面判据）。
func TestChannelServesTwoProtocolsOnOneCredential(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const plaintext = "test-key-placeholder"
	const balance = "100"
	model := unique("e2e-model-dual")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	upstreamSrv := newDualProtocolUpstream(plaintext)
	srv := httptest.NewServer(upstreamSrv.handler())
	t.Cleanup(srv.Close)

	channelID := seedDualProtocolChannel(t, db, sealer, merchant, srv.URL, model, plaintext)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	ctx := t.Context()

	// ── 一、OpenAI 入口 ────────────────────────────────────────────────
	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("OpenAI 入口状态码 = %d，期望 200。响应体：%s", status, body)
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil {
		t.Fatalf("OpenAI 响应不是合法 JSON: %v\n%s", err, body)
	}
	if completion.Object != "chat.completion" || len(completion.Choices) != 1 ||
		completion.Choices[0].Message.Content != "你好" {
		t.Errorf("OpenAI 入口拿到的是另一套形状的响应: %s", body)
	}

	// ── 二、Anthropic 入口 ─────────────────────────────────────────────
	status, body = postChat(ctx, t, public.URL+"/v1/messages", key, messagesBody(model))
	if status != http.StatusOK {
		t.Fatalf("Anthropic 入口状态码 = %d，期望 200。响应体：%s", status, body)
	}
	var message struct {
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatalf("Anthropic 响应不是合法 JSON: %v\n%s", err, body)
	}
	if message.Type != "message" || len(message.Content) != 1 || message.Content[0].Text != "你好" {
		t.Errorf("Anthropic 入口拿到的是另一套形状的响应: %s", body)
	}

	// 上游收到的路径要各是各的：两次都打到同一条路径说明分派用错了协议
	// （判据 5 的红证就在这里现形）。
	paths, seen := upstreamSrv.observed()
	wantPaths := []string{"/v1/chat/completions", "/v1/messages"}
	if len(paths) != 2 || paths[0] != wantPaths[0] || paths[1] != wantPaths[1] {
		t.Errorf("上游收到的路径 = %v，期望 %v", paths, wantPaths)
	}
	for i, got := range seen {
		if got != plaintext {
			t.Errorf("第 %d 次请求带的凭证 = %q，期望同一条渠道的那把", i+1, got)
		}
	}

	// 两次记账落在同一条渠道上。
	bills := billingRowsOf(t, db, user)
	if len(bills) != 2 {
		t.Fatalf("账单 %d 行，期望 2 行", len(bills))
	}
	for i, bill := range bills {
		if bill.status != "settled" {
			t.Errorf("第 %d 行状态 = %q，期望 settled", i+1, bill.status)
		}
		if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
			t.Errorf("第 %d 行的 channel_id = %v，期望 %d（两条入口都该记在同一个渠道上）",
				i+1, bill.channelID, channelID)
		}
	}
	if bills[0].channelID.Int64 != bills[1].channelID.Int64 {
		t.Errorf("两次的 channel_id 不同：%v vs %v", bills[0].channelID, bills[1].channelID)
	}
}

// 换一次凭证，两条入口同时改用新的（旧凭证立刻失效）。
//
// 判据分三步，缺一步都证明不了「轮换生效」：
//
//  1. 旧凭证能用（上面的用例已经走过一遍）；
//  2. 假上游改成只认新凭证之后，**还是旧凭证的库**让两条入口都失败——
//     这一步证明假上游确实在校验，第 3 步的 200 不是「它根本没看」；
//  3. 写入侧换一次凭证之后，两条入口都成功。
func TestRotatedCredentialAppliesToEveryProtocol(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const oldPlaintext = "test-key-placeholder"
	newPlaintext := "rotated-key-placeholder"
	const balance = "100"
	model := unique("e2e-model-rotate")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := newDualProtocolUpstream(oldPlaintext)
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	channelID := seedDualProtocolChannel(t, db, sealer, merchant, srv.URL, model, oldPlaintext)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	ctx := t.Context()
	calls := []struct {
		name string
		path string
		body []byte
	}{
		{"OpenAI", "/v1/chat/completions", chatBody(model)},
		{"Anthropic", "/v1/messages", messagesBody(model)},
	}

	// 先把假上游切到只认新凭证：库里的还是旧的，两条入口都该失败。
	fake.require(newPlaintext)
	fake.reset()
	for _, call := range calls {
		status, body := postChat(ctx, t, public.URL+call.path, key, call.body)
		if status == http.StatusOK {
			t.Fatalf("%s 入口在假上游只认新凭证时仍然成功，这一步的前提不成立：%s", call.name, body)
		}
	}
	if paths, _ := fake.observed(); len(paths) == 0 {
		t.Fatal("旧凭证那两次一次都没发到上游，这一步的前提不成立")
	}

	// 换凭证。
	registry, err := upstream.Registry()
	if err != nil {
		t.Fatalf("装配适配器注册表失败: %v", err)
	}
	admin := channels.NewAdmin(db, sealer, registry)
	if err := admin.Update(ctx, channels.UpdateRequest{ID: channelID, Credential: &newPlaintext}); err != nil {
		t.Fatalf("换凭证失败: %v", err)
	}

	// 两条入口都改用新凭证。
	fake.reset()
	for _, call := range calls {
		status, body := postChat(ctx, t, public.URL+call.path, key, call.body)
		if status != http.StatusOK {
			t.Errorf("%s 入口在换凭证之后状态码 = %d，期望 200。响应体：%s", call.name, status, body)
		}
	}
	paths, seen := fake.observed()
	if len(seen) < len(calls) {
		t.Fatalf("换凭证之后只有 %d 次上游调用，两条入口各该至少一次", len(seen))
	}
	// 两条入口都真的走到了对应的上游路径。万一执行器对某一次失败做了重试，
	// 这里也只在「两条路径都到过」上判，不锁死次数。
	for _, want := range []string{"/v1/chat/completions", "/v1/messages"} {
		if !slices.Contains(paths, want) {
			t.Errorf("换凭证之后上游没收到 %s（路径 %v）", want, paths)
		}
	}
	for i, got := range seen {
		if got != newPlaintext {
			t.Errorf("换凭证后第 %d 次请求带的仍是 %q，期望新凭证", i+1, got)
		}
	}
}
