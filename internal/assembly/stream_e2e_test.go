package assembly

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/tokens"
)

// 流式的端到端：一次 HTTP 请求走到假上游，再从**客户端收到的帧**与**库里的账**
// 两头核对。
//
// 与非流式那一组（e2e_test.go）同一个约定：走的是生产装配路径，假上游是本地
// httptest，断言必须回到库里看钱。
//
// # 这一组要证明的四件事
//
//  1. 两套协议的帧形状各自正确，且结尾的标记（[DONE] / message_stop）真的到了
//     客户端手里。
//  2. 客户端中途断开**要计费**：账按估算用量记下，预留不留在地上。
//  3. 上游在流中途报错时，已经吐出的部分被计费，剩下的部分**不假装正常结束**。
//  4. 已经吐字之后不再换渠道——这条最容易在「加个重试」时被顺手破坏，
//     而它的表现是客户端看到两段拼接的回答。
//
// # 估算用量这件事在库里看不见
//
// billing_requests 没有「用量来源」这一列，usage_events 也还没接入 store.Save，
// 所以「这次是按估算收的」在库里读不出来——那条判据由网关的单元测试钉住
// （internal/gateway/stream_test.go 里的 Result.Estimated）。这里断言的是
// **金额**：估算出来的那个数，而不是预留的封顶值。

// ── 假上游 ────────────────────────────────────────────────────────────

// streamUpstream 是一个回 SSE 的本地假上游。
//
// 协议由 protocol 决定：OpenAI 只发 data: 行，Anthropic 每帧带 event: 行。
// 两者都按真实协议的顺序吐帧（结束标记之前给用量），因为适配器的解析对顺序
// 是有假设的——把它写错，测的就不是真实的流。
type streamUpstream struct {
	protocol channel.Protocol
	model    string
	// chunks 是正文分片，按顺序吐出。
	chunks []string
	// usage 非 nil 时在结束标记之前报一片实测用量。
	usage *channel.Usage
	// failAfter > 0 时，吐完这么多片之后报一个上游错误并结束。
	failAfter int
	// hang 为 true 时，吐完全部分片之后挂住，直到调用方断开。
	hang bool
	// status > 0 时直接回这个状态码，用来走「上游接受请求之前就失败」那条路。
	status int

	db     *sql.DB
	userID int64

	mu       sync.Mutex
	calls    int
	bodies   [][]byte
	frozenAt []string
}

func (u *streamUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.record(r, body)

		if u.status > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(u.status)
			_, _ = w.Write([]byte(`{"error":{"message":"placeholder upstream failure","type":"server_error"}}`))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		if u.protocol == channel.ProtocolAnthropic {
			u.serveAnthropic(w, r, flush)
			return
		}
		u.serveOpenAI(w, r, flush)
	})
}

// record 记下这次调用，以及**这一刻**库里的冻结额。
//
// 后者是「预留发生在调上游之前」的正向证据：请求结束之后再看 frozen 分不出
// 「冻过又放开了」与「压根没冻」。
func (u *streamUpstream) record(r *http.Request, body []byte) {
	var frozen string
	var isFrozen bool
	_ = u.db.QueryRowContext(r.Context(),
		`SELECT frozen::text, frozen > 0 FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		u.userID).Scan(&frozen, &isFrozen)

	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.bodies = append(u.bodies, body)
	u.frozenAt = append(u.frozenAt, fmt.Sprintf("%s (冻结=%t)", frozen, isFrozen))
}

func (u *streamUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *streamUpstream) observedFrozen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.frozenAt...)
}

func (u *streamUpstream) sentBodies() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// serveOpenAI 按 OpenAI 的顺序吐帧：正文分片 → 结束分片 → 用量分片 → [DONE]。
func (u *streamUpstream) serveOpenAI(w io.Writer, r *http.Request, flush func()) {
	write := func(payload string) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		flush()
	}

	for i, text := range u.chunks {
		if u.failAfter > 0 && i == u.failAfter {
			write(`{"error":{"message":"placeholder upstream failure","type":"server_error"}}`)
			return
		}
		write(fmt.Sprintf(
			`{"id":"chatcmpl-placeholder","object":"chat.completion.chunk","model":%q,`+
				`"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`,
			u.model, text))
	}
	// 分片吐完之后才失败：这是「已经吐字之后中断」最常见的形状，而只在循环里
	// 判断会漏掉它——failAfter 等于分片数时循环里一次都不触发。
	if u.failAfter > 0 && len(u.chunks) <= u.failAfter {
		write(`{"error":{"message":"placeholder upstream failure","type":"server_error"}}`)
		return
	}

	if u.hang {
		<-r.Context().Done()
		return
	}

	write(fmt.Sprintf(
		`{"id":"chatcmpl-placeholder","object":"chat.completion.chunk","model":%q,`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, u.model))
	if u.usage != nil {
		write(fmt.Sprintf(
			`{"id":"chatcmpl-placeholder","object":"chat.completion.chunk","model":%q,"choices":[],`+
				`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			u.model, u.usage.PromptTokens, u.usage.CompletionTokens,
			u.usage.PromptTokens+u.usage.CompletionTokens))
	}
	write("[DONE]")
}

// serveAnthropic 按 Anthropic 的顺序吐事件。
//
// 用量跨两个事件：输入在 message_start，输出在 message_delta——适配器就是
// 这么累加的，只在一个事件里给会让「实测用量」那条路走不到。
func (u *streamUpstream) serveAnthropic(w io.Writer, r *http.Request, flush func()) {
	write := func(event, payload string) {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		flush()
	}

	var input, output int64
	if u.usage != nil {
		input, output = u.usage.PromptTokens, u.usage.CompletionTokens
	}

	write("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg-placeholder","type":"message","role":"assistant",`+
			`"model":%q,"content":[],"usage":{"input_tokens":%d,"output_tokens":0}}}`, u.model, input))
	write("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)

	for i, text := range u.chunks {
		if u.failAfter > 0 && i == u.failAfter {
			write("error",
				`{"type":"error","error":{"type":"api_error","message":"placeholder upstream failure"}}`)
			return
		}
		write("content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, text))
	}
	if u.failAfter > 0 && len(u.chunks) <= u.failAfter {
		write("error",
			`{"type":"error","error":{"type":"api_error","message":"placeholder upstream failure"}}`)
		return
	}

	if u.hang {
		<-r.Context().Done()
		return
	}

	write("content_block_stop", `{"type":"content_block_stop","index":0}`)
	write("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},`+
			`"usage":{"output_tokens":%d}}`, output))
	write("message_stop", `{"type":"message_stop"}`)
}

// ── 渠道 ──────────────────────────────────────────────────────────────

// seedChannelOfType 插一条渠道，可指定适配器类型与优先级，返回渠道 id。
//
// priority 是选路的**第一判据**（数值大者先选），用它把「先试哪个渠道」钉死：
// 同优先级之间按分数降序、同分随机打散，靠声明顺序的断言会随机失败。
func seedChannelOfType(t *testing.T, db *sql.DB, sealer *credential.Sealer, owner int64, baseURL, model, chType string, priority int) int64 {
	t.Helper()

	sealed, err := sealer.Seal([]byte("test-key-placeholder"))
	if err != nil {
		t.Fatalf("封装凭证失败: %v", err)
	}

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO channels
			(owner_user_id, name, type, base_url, credential_enc, status, models, priority)
		VALUES ($1, $2, $3, $4, $5, 'enabled', ARRAY[$6], $7)
		RETURNING id`,
		owner, unique("e2e-channel"), chType, baseURL, sealed, model, priority,
	).Scan(&id); err != nil {
		t.Fatalf("插渠道失败: %v", err)
	}
	return id
}

// ── 调用方 ────────────────────────────────────────────────────────────

// streamBody 与非流式那份的唯一区别是 stream: true。
func streamBody(model string) []byte {
	var body map[string]any
	if err := json.Unmarshal(chatBody(model), &body); err != nil {
		panic(err)
	}
	body["stream"] = true
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// anthropicStreamBody 与非流式那份（messagesBody）的唯一区别是 stream: true。
func anthropicStreamBody(model string) []byte {
	var body map[string]any
	if err := json.Unmarshal(messagesBody(model), &body); err != nil {
		panic(err)
	}
	body["stream"] = true
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// openStream 发起一次流式请求，返回响应与取消函数。
//
// 返回响应而不是读完的体：流式的断言要看着帧**一帧帧**到，全读进内存再切
// 就测不出「有没有边生成边发」。取消函数是「客户端中途断开」那条用例的全部
// 手段——真实的断开就是客户端关连接，在这里等于取消请求的 context。
//
// **响应体由调用方负责关**（读完帧之后 defer）。它不替调用方关，因为「读完了」
// 这个时刻只有调用方知道：中途断开那条用例正是要留着一个没读完的体。
func openStream(t *testing.T, url, key string, body []byte) (*http.Response, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		cancel()
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("发流式请求失败: %v", err)
	}
	// 响应体**不在这里关**，由调用点关：流式的体一直挂到流结束，而「读到该读的
	// 东西了」只有调用方知道是哪个时刻。这里替它关掉会让「读到一半就断开」
	// 那条用例连读都读不下去。
	return resp, cancel
}

// sseFrame 是客户端收到的一帧。
type sseFrame struct {
	// event 为空表示这一帧没有事件名（OpenAI 的帧都是这样）。
	event string
	data  string
}

// readFrames 读事件流。n > 0 时读满 n 帧就返回（剩下的留在体里，
// 用来模拟「读到一半就断开」）；n <= 0 时读到流结束。
func readFrames(t *testing.T, r io.Reader, n int) []sseFrame {
	t.Helper()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		out    []sseFrame
		cur    sseFrame
		opened bool
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if opened {
				out = append(out, cur)
				cur, opened = sseFrame{}, false
				if n > 0 && len(out) >= n {
					return out
				}
			}
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
			opened = true
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
			opened = true
		default:
			t.Errorf("收到不认识的一行 %q：SSE 只该有 event: 与 data: 两种字段", line)
			return out
		}
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("读事件流失败: %v", err)
	}
	return out
}

// openAIChunk 是 OpenAI 流式分片里我们关心的部分。
type openAIChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func decodeChunk(t *testing.T, f sseFrame) openAIChunk {
	t.Helper()

	var chunk openAIChunk
	if err := json.Unmarshal([]byte(f.data), &chunk); err != nil {
		t.Fatalf("分片不是合法 JSON: %v\n%s", err, f.data)
	}
	return chunk
}

// dig 沿路径取一个 JSON 字段，路径里的数字表示数组下标。
func dig(t *testing.T, v any, path ...string) any {
	t.Helper()

	for _, key := range path {
		switch cur := v.(type) {
		case map[string]any:
			v = cur[key]
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(cur) {
				t.Fatalf("路径里的 %q 不是合法的数组下标（数组长度 %d）", key, len(cur))
			}
			v = cur[i]
		default:
			t.Fatalf("路径里的 %q 走到了 %T，取不出字段", key, v)
		}
	}
	return v
}

// decodeFrame 把一个帧的载荷解成通用结构。
func decodeFrame(t *testing.T, f sseFrame) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(f.data), &out); err != nil {
		t.Fatalf("帧载荷不是合法 JSON: %v\n%s", err, f.data)
	}
	return out
}

// ── 账单 ──────────────────────────────────────────────────────────────

type billingRow struct {
	id        int64
	requestID string
	status    string
	reserved  string
	final     sql.NullString
	channelID sql.NullInt64
	streamed  bool
	// protocol 是这次请求的**调用协议**（由入口按路径决定），不是上游协议。
	// 跨协议的用例里两者不同，而这一列是事后按协议归因时的唯一依据。
	protocol string
}

// billingOf 取这个用户最近一行账单。没有行时第二个返回值为 false。
func billingOf(t *testing.T, db *sql.DB, userID int64) (billingRow, bool) {
	t.Helper()

	var b billingRow
	err := db.QueryRowContext(context.Background(), `
		SELECT id, request_id, status, reserved_amount::text, final_amount::text,
		       channel_id, streamed, protocol
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id DESC LIMIT 1`, userID,
	).Scan(&b.id, &b.requestID, &b.status, &b.reserved, &b.final, &b.channelID, &b.streamed, &b.protocol)
	if err == sql.ErrNoRows {
		return billingRow{}, false
	}
	if err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	return b, true
}

// waitForBillingStatus 等到最近一行账单变成某个状态。
//
// 流式的收尾发生在**事件流关闭之前**的后台 goroutine 里，所以调用方拿到
// 最后一帧之后，账可能还没写完。这里等它——等不到就是真的没写，而那正是
// 「预留被漏在地上」的表现。
func waitForBillingStatus(t *testing.T, db *sql.DB, userID int64, want string) billingRow {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if b, ok := billingOf(t, db, userID); ok {
			if b.status == want {
				return b
			}
			last = b.status
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等账单变成 %q 超时，最后看到的状态是 %q", want, last)
	return billingRow{}
}

// assertConsumeEntry 断言账本里有挂到这次账单上的消费分录，金额是指定值。
func assertConsumeEntry(t *testing.T, db *sql.DB, billID, userID int64, wantAmount string) {
	t.Helper()

	userAccount := fmt.Sprintf("user:%d", userID)
	var txnID, debit string
	err := db.QueryRowContext(context.Background(), `
		SELECT txn_id::text, (-amount)::text FROM ledger_entries
		 WHERE billing_request_id = $1 AND account = $2 AND entry_type = 'consume'
		 ORDER BY id DESC LIMIT 1`, billID, userAccount).Scan(&txnID, &debit)
	if err != nil {
		t.Fatalf("账本里没有挂到账单 %d 上的消费分录: %v", billID, err)
	}
	if !sameDecimal(debit, wantAmount) {
		t.Errorf("从用户账上扣掉的 = %s，期望 %s", debit, wantAmount)
	}

	var sum string
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(sum(amount), 0)::text FROM ledger_entries WHERE txn_id = $1`, txnID,
	).Scan(&sum); err != nil {
		t.Fatalf("查分录失败: %v", err)
	}
	if !sameDecimal(sum, "0") {
		t.Errorf("分录之和 = %s，期望 0（复式记账：同一 txn 内借贷相抵）", sum)
	}
}

// assertNoLedgerEntry 断言这个用户名下没有任何分录。
func assertNoLedgerEntry(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()

	var entries int
	if err := db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM ledger_entries
		 WHERE billing_request_id IN (SELECT id FROM billing_requests WHERE user_id = $1)`,
		userID).Scan(&entries); err != nil {
		t.Fatalf("查分录失败: %v", err)
	}
	if entries != 0 {
		t.Errorf("写了 %d 条分录，期望 0 条", entries)
	}
}

// assertReservedBeforeUpstream 断言假上游被调用时库里已经冻住了钱。
func assertReservedBeforeUpstream(t *testing.T, fake *streamUpstream) {
	t.Helper()

	observed := fake.observedFrozen()
	if len(observed) == 0 {
		t.Fatal("假上游一次都没被调用")
	}
	for i, s := range observed {
		if !strings.Contains(s, "冻结=true") {
			t.Errorf("第 %d 次调用上游时库里的冻结额是 %s——预留必须先冻住再调上游", i+1, s)
		}
	}
}

// ── 用例 ──────────────────────────────────────────────────────────────

// OpenAI 协议的流式成功路径：帧形状正确，钱按上游报的用量扣。
func TestStreamChatCompletionsEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-openai")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"你好", "，我是假上游"},
		usage:    &channel.Usage{PromptTokens: 12, CompletionTokens: 7},
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key, streamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}

	// 一、帧序列。五帧：角色分片、两片正文、带用量的结束分片、[DONE]。
	frames := readFrames(t, resp.Body, 0)
	if len(frames) != 5 {
		t.Fatalf("收到 %d 帧，期望 5 帧：%+v", len(frames), frames)
	}
	for i, f := range frames {
		if f.event != "" {
			t.Errorf("第 %d 帧带了事件名 %q——OpenAI 的流式分片不带 event: 行", i, f.event)
		}
	}

	first := decodeChunk(t, frames[0])
	if first.Object != "chat.completion.chunk" {
		t.Errorf("第一帧的 object = %q，期望 chat.completion.chunk", first.Object)
	}
	if first.Model != model {
		t.Errorf("第一帧的 model = %q，期望 %q（报给调用方的是他要的那个名字）", first.Model, model)
	}
	if len(first.Choices) != 1 || first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("第一帧 = %+v，期望一片只带 role 的开场分片", first.Choices)
	}

	for i, want := range []string{"你好", "，我是假上游"} {
		chunk := decodeChunk(t, frames[i+1])
		if len(chunk.Choices) != 1 || chunk.Choices[0].Delta.Content != want {
			t.Errorf("第 %d 帧的正文 = %+v，期望 %q", i+1, chunk.Choices, want)
		}
	}

	last := decodeChunk(t, frames[3])
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Errorf("结束分片的 finish_reason = %s，期望 stop", showPtr(last.Choices[0].FinishReason))
	}
	if last.Usage == nil {
		t.Fatal("结束分片里没有 usage——流式的用量必须在结束之前给出来")
	}
	if last.Usage.PromptTokens != 12 || last.Usage.CompletionTokens != 7 || last.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v，期望上游报的 12/7/19", *last.Usage)
	}
	if frames[4].data != "[DONE]" {
		t.Errorf("最后一帧 = %q，期望 [DONE]", frames[4].data)
	}

	// 二、钱：12 × 0.00001 + 7 × 0.00003 = 0.00033000，与非流式那条路同一个数。
	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003）", bill.final.String)
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, channelID)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false——这一列是事后分辨两条路的唯一依据")
	}
	assertWallet(t, db, user, "99.99967", "0")
	assertConsumeEntry(t, db, bill.id, user, "0.00033")
	assertReservedBeforeUpstream(t, fake)

	// 三、发出去的那份请求体真的是流式的。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if !sent.Stream {
		t.Error("上游收到的请求里 stream 不是 true——调用方要的流式没有传到上游")
	}
}

// Anthropic 协议的流式成功路径：事件名与顺序正确，钱与非流式同口径。
func TestStreamMessagesEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-anthropic")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol: channel.ProtocolAnthropic,
		model:    model,
		chunks:   []string{"你好", "，我是假上游"},
		usage:    &channel.Usage{PromptTokens: 12, CompletionTokens: 7},
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/messages", key, anthropicStreamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, body)
	}

	// 一、事件序列：开消息 → 开正文块 → 两片正文 → 关块 → 收尾 → 结束。
	frames := readFrames(t, resp.Body, 0)
	wantEvents := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("收到 %d 帧，期望 %d 帧：%+v", len(frames), len(wantEvents), frames)
	}
	for i, want := range wantEvents {
		if frames[i].event != want {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, frames[i].event, want)
		}
	}

	start := decodeFrame(t, frames[0])
	if got := dig(t, start, "message", "model"); got != model {
		t.Errorf("message_start 的 model = %v，期望 %q", got, model)
	}
	// input_tokens 是**本地估算**：上游的实测用量要到流末尾才拿得到。
	// 「你好」两个字在未知族的估算表上是 3，这个数写死是为了让改动估算表时
	// 有人会来看一眼这一行（同口径的估算见 tokens.EstimatedTokens）。
	if got := dig(t, start, "message", "usage", "input_tokens"); got != float64(3) {
		t.Errorf("message_start 的 input_tokens = %v，期望 3（本地估算）", got)
	}
	if got := dig(t, decodeFrame(t, frames[2]), "delta", "text"); got != "你好" {
		t.Errorf("第一片正文 = %v，期望 你好", got)
	}
	if got := dig(t, decodeFrame(t, frames[3]), "delta", "text"); got != "，我是假上游" {
		t.Errorf("第二片正文 = %v，期望 ，我是假上游", got)
	}
	if got := dig(t, decodeFrame(t, frames[4]), "index"); got != float64(0) {
		t.Errorf("content_block_stop 的 index = %v，期望 0（正文块的块下标）", got)
	}
	// 上游报过 usage，所以收尾帧里的 output_tokens 是实测值。
	delta := decodeFrame(t, frames[5])
	if got := dig(t, delta, "delta", "stop_reason"); got != "end_turn" {
		t.Errorf("stop_reason = %v，期望 end_turn", got)
	}
	if got := dig(t, delta, "usage", "output_tokens"); got != float64(7) {
		t.Errorf("message_delta 的 output_tokens = %v，期望上游报的 7", got)
	}
	if got := dig(t, delta, "usage", "input_tokens"); got != float64(12) {
		t.Errorf("message_delta 的 input_tokens = %v，期望上游报的 12", got)
	}

	// 二、钱：与非流式那条路同一个数。
	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033", bill.final.String)
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, channelID)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false")
	}
	assertWallet(t, db, user, "99.99967", "0")
	assertConsumeEntry(t, db, bill.id, user, "0.00033")
}

// 客户端在流中途断开：已经产生的部分**要计费**，用量是本地估算的。
//
// 判据分两头：
//
//   - 账真的记了。金额是估算值，不是预留的封顶值——写成封顶值的话这条用例
//     在「按预留全额收」的实现上也会通过，而那是另一种错账。
//   - 预留被处置了：状态是 settled（不是 released、更不是一直 pending），
//     冻结额回到零。
//
// 估算的期望值逐项算得出来：输入是「你好」加一条消息的框架（3 + 4 = 7），
// 输出是两片正文各自计数（4 + 4 = 8），7 × 0.00001 + 8 × 0.00003 = 0.00031。
// 片是**分开计数**的，把两片拼起来再算会得到另一个数——那正是这条断言要
// 钉住的口径（见 tokens 包「增量计数，不留全文」）。
func TestStreamClientDisconnectIsBilled(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-disconnect")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	// 吐完两片就挂住：调用方不断开，这条流永远不会自己结束。
	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"第一片", "第二片"},
		hang:     true,
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key, streamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, body)
	}

	// 读满三帧（角色分片 + 两片正文）再断开。读满这件事本身就是证据：
	// 客户端收到第 k 帧意味着网关已经把它计入估算，所以断开时估算里
	// **一定**有这两片——不必等一个时长，也不会有竞态。
	frames := readFrames(t, resp.Body, 3)
	if len(frames) != 3 {
		t.Fatalf("断开前只收到 %d 帧，期望 3 帧", len(frames))
	}
	cancel()

	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00031") {
		t.Errorf("结算金额 = %s，期望 0.00031（估算：7 输入 + 8 输出）", bill.final.String)
	}
	// 预留是上界，结算是按实际用量算的——按预留全额收是另一种错。
	if !greaterThan(bill.reserved, bill.final.String) {
		t.Errorf("预留 %s 不大于结算 %s：按封顶值收费说明用量根本没被估过", bill.reserved, bill.final.String)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false")
	}
	assertWallet(t, db, user, "99.99969", "0")
	assertConsumeEntry(t, db, bill.id, user, "0.00031")
	assertReservedBeforeUpstream(t, fake)
}

// 上游在流中途报错：已经吐出的部分被计费，剩下的部分**不假装正常结束**。
//
// 这是「已经吐字之后不能再换渠道」那条约束的另一面：不能换渠道，也不能
// 把它当成一次干净的失败去释放预留——上游已经为那些 token 计了费。
//
// 期望金额：输入 7（3 + 一条消息的框架），输出 3（「半句」），
// 7 × 0.00001 + 3 × 0.00003 = 0.00016。
func TestStreamUpstreamErrorMidStream(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-midfail")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol:  channel.ProtocolOpenAI,
		model:     model,
		chunks:    []string{"半句"},
		failAfter: 1,
		db:        db,
		userID:    user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key, streamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（错误发生在流开始之后，改不了状态码了）", resp.StatusCode)
	}

	frames := readFrames(t, resp.Body, 0)
	if len(frames) != 3 {
		t.Fatalf("收到 %d 帧，期望 3 帧（开场、正文、错误）：%+v", len(frames), frames)
	}
	if got := decodeChunk(t, frames[1]); len(got.Choices) != 1 || got.Choices[0].Delta.Content != "半句" {
		t.Errorf("第 2 帧 = %+v，期望已经吐出的那半句", got.Choices)
	}

	// 最后一帧必须是错误，且**没有** [DONE]：伪造一个正常结束会让调用方
	// 把半截回答当成完整回答用。
	last := frames[2].data
	if last == "[DONE]" {
		t.Fatal("上游中途报错，却给了一个 [DONE]——这等于把截断说成正常结束")
	}
	if !strings.Contains(last, "error") {
		t.Errorf("最后一帧 = %q，期望一个错误帧", last)
	}
	if !strings.Contains(last, "interrupted") {
		t.Errorf("错误帧 = %q，期望措辞说明回答被截断了（已经吐过字）", last)
	}
	if strings.Contains(last, "placeholder upstream failure") {
		t.Errorf("错误帧回显了上游的错误原文：%q", last)
	}

	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00016") {
		t.Errorf("结算金额 = %s，期望 0.00016（7 输入 + 3 输出，按估算）", bill.final.String)
	}
	assertWallet(t, db, user, "99.99984", "0")
	assertConsumeEntry(t, db, bill.id, user, "0.00016")
}

// 已经吐字之后不再换渠道。
//
// 第一个渠道（priority 10）吐一片之后失败，第二个渠道（priority 0）是健康的。
// 断言第二个**一次都没被调用**：换渠道会让调用方看到两段拼接的回答，而它
// 无法察觉。这条断言也是唯一能证明「ErrOutputStarted 真的被用上了」的地方——
// 只看金额分不出「没换渠道」与「换了但没用上」。
func TestStreamDoesNotFailoverAfterOutputStarted(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-nofailover")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	broken := &streamUpstream{
		protocol:  channel.ProtocolOpenAI,
		model:     model,
		chunks:    []string{"半句"},
		failAfter: 1,
		db:        db,
		userID:    user,
	}
	brokenSrv := httptest.NewServer(broken.handler())
	t.Cleanup(brokenSrv.Close)

	healthy := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"完整回答"},
		usage:    &channel.Usage{PromptTokens: 12, CompletionTokens: 7},
		db:       db,
		userID:   user,
	}
	healthySrv := httptest.NewServer(healthy.handler())
	t.Cleanup(healthySrv.Close)

	brokenID := seedChannelOfType(t, db, sealer, merchant, brokenSrv.URL, model, "openai", 10)
	seedChannelOfType(t, db, sealer, merchant, healthySrv.URL, model, "openai", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key, streamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, body)
	}

	frames := readFrames(t, resp.Body, 0)
	if len(frames) != 3 {
		t.Fatalf("收到 %d 帧，期望 3 帧：%+v", len(frames), frames)
	}
	if frames[2].data == "[DONE]" {
		t.Fatal("半截回答后面跟了一个 [DONE]")
	}

	if got := healthy.callCount(); got != 0 {
		t.Errorf("第二个渠道被调用 %d 次，期望 0 次——已经吐字之后换渠道会让调用方看到两段拼接的回答", got)
	}
	if got := broken.callCount(); got != 1 {
		t.Errorf("第一个渠道被调用 %d 次，期望 1 次", got)
	}

	bill := waitForBillingStatus(t, db, user, "settled")
	if !bill.channelID.Valid || bill.channelID.Int64 != brokenID {
		t.Errorf("账单的 channel_id = %v，期望 %d（那次调用实际走的渠道）", bill.channelID, brokenID)
	}
	if !sameDecimal(bill.final.String, "0.00016") {
		t.Errorf("结算金额 = %s，期望 0.00016（按已经吐出的部分估算）", bill.final.String)
	}
}

// 上游在接受请求之前就失败：一个字都没吐，走的是与非流式完全一样的那条路
// ——不计费、预留释放、回一个错误响应。
//
// 与非流式那条失败用例（TestUpstreamFailureReleasesReservation）的区别只在
// 请求里带了 stream: true：失败发生在响应头之前，所以错误仍然是**普通响应**
// 而不是一个流里的错误事件。
func TestStreamUpstreamFailureBeforeOutput(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-prefail")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		status:   http.StatusInternalServerError,
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key, streamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502。响应体：%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q：失败发生在流开始之前，这里该是一个普通的错误响应", ct)
	}

	bill := waitForBillingStatus(t, db, user, "released")
	if !greaterThan(bill.reserved, "0") {
		t.Errorf("预留金额 = %s，期望大于零——等于零说明这次请求根本没冻住钱", bill.reserved)
	}
	if bill.final.Valid && greaterThan(bill.final.String, "0") {
		t.Errorf("final_amount = %s，期望空或零：上游失败不计费", bill.final.String)
	}
	assertWallet(t, db, user, balance, "0")
	assertNoLedgerEntry(t, db, user)
	assertReservedBeforeUpstream(t, fake)
}

// 估算口径与预留口径必须一致到「同一个数」。
//
// 预留用的是 tokens.EstimatedTokens（纯估算），网关的流式估算器在拿到词表时
// 会走精确路径——两者对同一个请求给出不同的输入量是**已知且可接受**的偏差，
// 但模型族未知时必须完全相同：那时两边走的是同一张标定表。
func TestStreamEstimateMatchesReservationForUnknownFamily(t *testing.T) {
	const model = "e2e-model-unknown-family"

	if fam := tokens.FamilyOf(model); fam != tokens.FamilyUnknown {
		t.Fatalf("这个模型名落在了 %v 族上，用例的前提不成立", fam)
	}

	est := tokens.NewStreamEstimator(tokens.NewCounter(), model)
	est.ObserveRequest("你好", 1, 0)

	if got, want := est.Result().Input.Tokens, tokens.EstimatedTokens(model, "你好")+tokens.MessageFrameTokens; got != want {
		t.Errorf("估算器的输入 = %d，预留口径 = %d：未知族时两者必须相同", got, want)
	}
}
