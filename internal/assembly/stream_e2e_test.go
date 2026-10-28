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
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// 流式的端到端：一次 HTTP 请求走到假上游，再从**客户端收到的帧**与**库里的账**
// 两头核对。
//
// 与非流式那一组（e2e_test.go）同一个约定：走的是生产装配路径，假上游是本地
// httptest，断言必须回到库里看钱。
//
// # 这一组要证明的五件事
//
//  1. 两套协议的帧形状各自正确，且结尾的标记（[DONE] / message_stop）真的到了
//     客户端手里。
//  2. 客户端中途断开**要计费**：账按估算用量记下，预留不留在地上。
//  3. 上游在流中途报错时，已经吐出的部分被计费，剩下的部分**不假装正常结束**。
//  4. 已经吐字之后不再换渠道——这条最容易在「加个重试」时被顺手破坏，
//     而它的表现是客户端看到两段拼接的回答。
//  5. 失败落在吐字之前时，账单上留下「这次为什么不计费」的依据：上游回过
//     一个 2xx（照实记下），分类是「流在接受之后断了」
//     （见 TestStreamAbortedBeforeOutputRecordsEvidence）。
//
// # 估算用量这件事在库里怎么读
//
// billing_requests 没有「用量来源」这一列，但 usage_events 已经接进
// store.Save：billing_path 与 estimator 两列记的就是「这个数是实测的、
// 还是估的，以及是怎么估出来的」。这一组用例断言的是**金额**——估算出来的
// 那个数，而不是预留的封顶值；「按什么来源记的账」那一半由
// internal/ledger/store 的集成测试钉住（它拿 Charge 与落库的那一行逐字段比），
// 网关这一侧则钉 Result.Estimated（见 internal/gateway/stream_test.go）。

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
	// toolCalls 是工具调用分片，按给定的顺序**原样**吐在正文分片之后、
	// 结束分片之前。不按 index 分组：交错的下标（0、1、0、1）是合法的，
	// 而它正是跨协议那条路上最容易出错的一种形状。
	toolCalls []toolCallDelta
	// usage 非 nil 时在结束标记之前报一片实测用量。
	usage *channel.Usage
	// failAfter > 0 时，吐完这么多片之后报一个上游错误并结束。
	failAfter int
	// hang 为 true 时，吐完全部分片之后挂住，直到调用方断开。
	hang bool
	// truncate 为 true 时，吐完全部分片之后**直接结束响应**：既不发结束
	// 分片，也不发结束标记，也没有错误帧。它模拟的是上游把连接掐断
	// （或一个不守协议的网关把流截在半路），与「正常结束」只差一个标记。
	truncate bool
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

	// 开场分片：只带 role，正文是空串。
	//
	// 真上游就是这么发的（见 internal/channel/openaiadapter 的
	// nativeStreamFrames：第一帧是 `delta:{"role":"assistant","content":""}`）。
	// 假上游以前不发它——枢纽路径会自己合成一个 role 片，所以看不出来；直通
	// 原样转发上游字节，缺了它就少一帧。这是夹具不完整，不是网关该补。
	//
	// 它同时是 failAfter 计数的第 0 片之外的东西：失败点按正文分片算，
	// 开场片不占 failAfter 的下标，见下面循环的 i 与 u.chunks 的对应。
	if u.status == 0 {
		write(fmt.Sprintf(
			`{"id":"chatcmpl-placeholder","object":"chat.completion.chunk","model":%q,`+
				`"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			u.model))
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

	for _, tc := range u.toolCalls {
		write(toolCallChunk(u.model, tc))
	}

	if u.hang {
		<-r.Context().Done()
		return
	}

	if u.truncate {
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

// toolCallDelta 是假上游吐出的一片工具调用增量。
//
// ID 与 Name 只在一次工具调用的**第一个**分片里给（OpenAI 就是这么发的），
// 后续分片只有 index 与参数片段——留空时那两个字段整个不写，与真实上游一致。
type toolCallDelta struct {
	// Index 是上游自己那条流里的工具调用下标。它**可以交错**：0、1、0、1 是
	// 合法的，而下游要靠它把分片归回各自的块。
	Index int
	ID    string
	Name  string
	Args  string
}

// openAIFakeToolCall 是分片里 tool_calls 数组的元素。
//
// 内层用结构体编码而不是拼字符串：该省哪些字段正是这条流要测的东西之一
// ——多写一个 `"name":""` 会让上游的形状失真，而失真的夹具不会报错，
// 只会让用例测的不是真实的那条路。
type openAIFakeToolCall struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openAIFakeToolFunction `json:"function"`
}

type openAIFakeToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// toolCallChunk 造一个只带工具调用增量的分片。外层形状与 serveOpenAI 里
// 那几个分片相同（choices[0].delta），只有 delta 里装的是 tool_calls。
func toolCallChunk(model string, tc toolCallDelta) string {
	call := openAIFakeToolCall{
		Index:    tc.Index,
		ID:       tc.ID,
		Function: openAIFakeToolFunction{Name: tc.Name, Arguments: tc.Args},
	}
	if tc.ID != "" {
		call.Type = "function"
	}
	calls, err := json.Marshal([]openAIFakeToolCall{call})
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(
		`{"id":"chatcmpl-placeholder","object":"chat.completion.chunk","model":%q,`+
			`"choices":[{"index":0,"delta":{"tool_calls":%s},"finish_reason":null}]}`,
		model, calls)
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

	if u.truncate {
		return
	}

	write("content_block_stop", `{"type":"content_block_stop","index":0}`)
	// message_delta **不带** input_tokens：真上游两种形状都发过（带的是
	// anthropicadapter 的 adapter_test.go:594 那份，不带的是它 native_test.go:772
	// 那份）。这里保持不带，是让这条枢纽用例覆盖「上游只报了输出」那种形状：
	// 适配器只在 input_tokens 为正时才累计它，所以渲染器那一帧的输入量是 0
	// （见 internal/api 的 claudeUsageOf），而不是拿本地估算顶上。
	//
	// usage 为 nil 时 message_start 里也报 0，走的是「上游没报过，退回本地
	// 估算」那条分支。
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
			(owner_user_id, name, type, base_url, status, models, priority)
		VALUES ($1, $2, $3, $4, 'enabled', ARRAY[$5], $6)
		RETURNING id`,
		owner, unique("e2e-channel"), chType, baseURL, model, priority,
	).Scan(&id); err != nil {
		t.Fatalf("插渠道失败: %v", err)
	}
	// 凭证在账号上（见 00067）：池子里只有一个账号时选路必定取它，端到端要它
	// 能解出明文才打得到上游。
	var accountID int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO accounts (name, credential_enc) VALUES ($1, $2) RETURNING id`,
		unique("e2e-account"), sealed).Scan(&accountID); err != nil {
		t.Fatalf("插账号失败: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO channel_accounts (channel_id, account_id) VALUES ($1, $2)`,
		id, accountID); err != nil {
		t.Fatalf("把账号挂到渠道上失败: %v", err)
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
	// computed 是**未截断**的结算金额，见迁移 00021。
	//
	// 它与 final 分开取回来，是因为 final 判不了「这一行有没有撞上预留上界」：
	// 撞上界时 final **就是** reserved，没撞时 final ≤ reserved，两种情况下
	// `final > reserved` 都取假。判据要用 computed。
	//
	// 可空：这一行还没结算（status = 'pending'）时是 NULL。已结算的行一定有值，
	// 所以 NULL 在已结算的行上是「写入点掉了」，不是「没撞上界」——见 assertNotCapped。
	computed  sql.NullString
	channelID sql.NullInt64
	streamed  bool
	// protocol 是这次请求的**调用协议**（由入口按路径决定），不是上游协议。
	// 跨协议的用例里两者不同，而这一列是事后按协议归因时的唯一依据。
	protocol string
	// canonicalModel 是**入站解析之后的规范名**（调用方写别名时它与请求里
	// 那个不同）；upstreamModel 是**实际发给上游**的那个（出站映射翻译之后）。
	// 两者分开取回来，是因为「别名生效没有」只能靠它们的差异回答——只取一个
	// 的话，翻译没做时两列一样，判据消失。
	//
	// upstreamModel 可空：预留那一行还没选路，这一列本来就是空的（见 #338）。
	canonicalModel string
	upstreamModel  sql.NullString

	// upstreamStatus 与 errorCode 是上游侧这次的依据（见 #362、#560）。
	//
	// 两列都可空，而 NULL 在这里是**正常取值**：它表示这一行没有要记的上游
	// 依据，不是「没读到」。所以断言要连着 Valid 一起判——只判 String 的话
	// 「写了个 0 / 空串进去」与「留了 NULL」在用例里长得一样，而前者会让
	// 「上游回了 0 号」与「没有这回事」在库里分不开。
	//
	// 两列在计费与不计费两种行上**都可能有值**，各自答的问题不同：不计费的
	// 行上答「这次没向上游收钱，是不是因为上游这一侧出了事」（唯一写它的是
	// store.Release）；计费的行上答
	// 「钱收了，回答是不是在调用方不知情的情况下被截短」（唯一写它的是
	// markSettled，取值只有 upstream_stream_truncated 一种，见
	// pipeline.Outcome）。
	upstreamStatus sql.NullInt64
	errorCode      sql.NullString
}

// billingOf 取这个用户最近一行账单。没有行时第二个返回值为 false。
func billingOf(t *testing.T, db *sql.DB, userID int64) (billingRow, bool) {
	t.Helper()

	var b billingRow
	err := db.QueryRowContext(context.Background(), `
		SELECT id, request_id, status, reserved_amount::text, final_amount::text,
		       computed_amount::text, channel_id, streamed, protocol,
		       canonical_model, upstream_model, upstream_status, error_code
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id DESC LIMIT 1`, userID,
	).Scan(&b.id, &b.requestID, &b.status, &b.reserved, &b.final, &b.computed,
		&b.channelID, &b.streamed, &b.protocol, &b.canonicalModel, &b.upstreamModel,
		&b.upstreamStatus, &b.errorCode)
	if err == sql.ErrNoRows {
		return billingRow{}, false
	}
	if err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	return b, true
}

// assertNotCapped 断言这一行**没有**撞上预留上界。
//
// # 为什么判据是 computed_amount 而不是 final_amount
//
// final_amount 落的是 Settlement.Actual，而它在撞上界时被改成等于预留额
// （见 internal/billing 的 SettleWithCharges）：没撞时 Actual ≤ 预留，撞了时
// Actual == 预留。两种情况 `final > reserved` 都取假——所以「断言 final 不超过
// 预留」是一条**恒真**的断言，一次都不会响，哪怕上界真的算错了。
//
// computed_amount 是**未截断**的那个数，它大于预留就说明这次真的撞了。
// 这也正是「预留 ≥ 结算」这条不变量的可验证形式：final = min(computed, reserved)
// 恒成立，所以要钉的是 computed ≤ reserved。
//
// # 已结算而 computed_amount 是 NULL 要当成失败
//
// 已结算的行两列写在同一条 UPDATE 里（见 internal/ledger/store 的 markSettled），
// 所以 computed_amount 为 NULL 只有两种解释：这一行在迁移 00021 之前就结算了
// （那批行补不回来），或者写入点掉了。
//
// **两种都不能当成「没撞上界」。** 那正是「没跑成」与「跑成了」长得一样的老
// 毛病（见 LESSONS.md 第 16 条）：写入点掉了之后这条断言会静默通过，而它本该
// 是唯一能报出「上界算错了」的地方。所以这里当场失败，不往下走。
func assertNotCapped(t *testing.T, bill billingRow) {
	t.Helper()

	if !bill.computed.Valid {
		t.Fatalf("账单 %s 已结算，而 computed_amount 是 NULL——未截断的结算金额没有落库，"+
			"「有没有撞上界」就判不出来（见迁移 00021 与 internal/ledger/store 的 markSettled）",
			bill.requestID)
	}
	if greaterThan(bill.computed.String, bill.reserved) {
		t.Errorf("账单 %s 撞上了预留上界：按实际用量算出的 %s 大于预留 %s——"+
			"预留按最坏情况算，本该是结算的上界；差出来的那一截由平台承担",
			bill.requestID, bill.computed.String, bill.reserved)
	}
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
	//
	// 五个而不是六个：上游把用量放在结束分片之后**另发的一个 choices 为空的
	// 分片**里（见 nativeStreamFrames 真样本），而我们把两者合并成一片——
	// 这是 docs/api.md 的「与上游契约的已知偏差」里写着的一处**刻意偏离**：
	// 分开写会让按 `choices[0]` 取值的朴素客户端读到空数组而崩。
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
	// 用量就在这一个结束分片里（上游是另发一片的，我们按文档合并）。
	if last.Usage == nil {
		t.Fatal("结束分片里没有 usage——流式的用量必须在结束之前给出来")
	}
	if last.Usage.PromptTokens != 12 || last.Usage.CompletionTokens != 7 ||
		last.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v，期望上游报的 12/7/19", *last.Usage)
	}
	if frames[4].data != "[DONE]" {
		t.Errorf("最后一帧 = %q，期望 [DONE]", frames[4].data)
	}

	// 没有任何一帧的 choices 是空数组。
	//
	// 这条比帧数直接：它钉的是原因（分开写会让按 choices[0] 取值的朴素客户端
	// 读到空数组而崩，见 docs/api.md 的已知偏差），不是后果（帧数是 5 还是 6）。
	// 上游确实发过一个 choices 为空的分片，我们把它合并掉了。
	for i, f := range frames {
		if f.data == "[DONE]" {
			continue
		}
		chunk := decodeChunk(t, f)
		if len(chunk.Choices) == 0 {
			t.Errorf("第 %d 帧的 choices 是空数组：%s\n"+
				"我们承诺把用量合并进结束分片，不把上游那个空 choices 的分片转给调用方", i, f.data)
		}
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

	// 正常结束的那一次没有上游侧的依据可写：两列留 NULL。
	//
	// 这一条钉的是「接上截断之后没有顺手把所有已计费的行都写上值」。写上去的
	// 值只可能来自这一次的响应归类，而这一次是干净结束，于是那个值就是编出来
	// 的；更糟的是「账单上有值」会从此不再意味着「这里有一件只有账单知道的事」。
	if bill.upstreamStatus.Valid || bill.errorCode.Valid {
		t.Errorf("正常结束的流式请求两列应当是 NULL，得到 %v / %v",
			bill.upstreamStatus, bill.errorCode)
	}

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
	// input_tokens 是**上游报的真值**，不是本地估算：上游在自己的
	// message_start 里就给了它，而那一帧是流的第一帧。本地估算（「你好」
	// 两个字在未知族的估算表上是 3）会偏低，而只读 message_start 的客户端
	// 读到的就是那个偏低的数（见 #256）。
	if got := dig(t, start, "message", "usage", "input_tokens"); got != float64(12) {
		t.Errorf("message_start 的 input_tokens = %v，期望 12（上游报的真值）", got)
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

// 跨协议那条路的**首帧延迟**：调用方说 Anthropic，上游说 OpenAI，而上游的用量
// 在结束分片里——所以第一帧不能等用量。等下去整个流就变成「等一会儿一次性返回」，
// 比报一个偏低的估算更坏（见 internal/api 的 anthropicStream.writeStart）。
//
// 判据是**上游还挂着的时候客户端已经收到第一帧**：假上游吐完两片就挂住，谁都不去
// 关它，只有第一帧真的发出来了这里才读得到。读满三帧这件事本身就是证据——与
// TestStreamClientDisconnectIsBilled 同一条判据。
//
// 读不到时不要挂到整轮超时：看门狗十秒后掐断请求，断言报出来的是「一帧都没收到」。
func TestStreamMessagesFirstFrameBeforeUpstreamFinishes(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-first-frame")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	// 上游说 OpenAI：它的用量在结束分片里，流的前面**没有**输入用量可给。
	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"第一片", "第二片"},
		usage:    &channel.Usage{PromptTokens: 12, CompletionTokens: 7},
		hang:     true,
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "openai", 0)
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
	watchdog := time.AfterFunc(10*time.Second, cancel)
	defer watchdog.Stop()

	frames := readFrames(t, resp.Body, 3)
	if len(frames) != 3 {
		t.Fatalf("上游还挂着时只收到 %d 帧，期望 3 帧（第一帧被推迟到流末尾了？）", len(frames))
	}
	if frames[0].event != "message_start" {
		t.Errorf("第 0 帧的事件名 = %q，期望 message_start（它必须是第一帧）", frames[0].event)
	}
	// 这条路上没有更早的输入用量可给，填的是本地估算（「你好」两个字在未知族的
	// 估算表上是 3）。真值那条路在同协议用例 TestStreamMessagesEndToEnd 里。
	if got := dig(t, decodeFrame(t, frames[0]), "message", "usage", "input_tokens"); got != float64(3) {
		t.Errorf("message_start 的 input_tokens = %v，期望 3（本地估算）", got)
	}
	if frames[1].event != "content_block_start" || frames[2].event != "content_block_delta" {
		t.Errorf("前两帧之后的帧序 = %q、%q，期望 content_block_start、content_block_delta",
			frames[1].event, frames[2].event)
	}
	if got := dig(t, decodeFrame(t, frames[2]), "delta", "text"); got != "第一片" {
		t.Errorf("第一片正文 = %v，期望 第一片", got)
	}
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

	// 客户端断开的那一次两列也是 NULL：停下这条流的是**我们**，不是上游。
	//
	// 这一条钉的是归属：把 ours 那几种结局（客户端断开、插件终止、余额不足的
	// 中断）写进这两列，等于把我们的动作记到上游头上——而这一列的存在理由正是
	// 挑出上游不守信的渠道，记错了会让一个健康的渠道看起来在掐流。
	if bill.upstreamStatus.Valid || bill.errorCode.Valid {
		t.Errorf("客户端断开的两列应当是 NULL，得到 %v / %v",
			bill.upstreamStatus, bill.errorCode)
	}
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

	// 账单上那两列留 NULL，尽管这次确实是上游出的问题。
	//
	// 这是有意不写的（见 gateway 的 billedStreamEvidence）：错误帧当时就送到
	// 调用方手里了，「这次没写完」他当场就知道——真正需要账单替调用方说话的
	// 只有静默截断那一种（那一次没有错误帧，要靠帧序列里没有结束标记才发现）。
	// 把报错也写进去，「账单上有值」就不再等于「这里有一件只有账单知道的事」。
	if bill.upstreamStatus.Valid || bill.errorCode.Valid {
		t.Errorf("流中途报错的两列应当是 NULL，得到 %v / %v",
			bill.upstreamStatus, bill.errorCode)
	}
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

// 上游接受了请求、一个 token 都没吐就断了：计费上的处置与上面那条一样
// （不计费、预留释放），依据里的状态码则照实记——上游确实回了一个 200，
// 「拿到过响应」这个事实成立（见 channel.ErrUpstream.GotResponse）。
// 这次的**结局**（一个字都没产生）由 error_code 表达，两列各司其职。
//
// 它同时是 `upstreamEvidence` 里**唯一一个不看 ErrUpstream 的分支**，把那一支
// 整个删掉，这条路上的依据会全部丢掉（两列都 NULL），而库里 NULL 与
// 「这一批恰好没有失败」长得一样——正是这次改动要修的那类缺陷。
func TestStreamAbortedBeforeOutputRecordsEvidence(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-abort")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	// 200 + SSE 头，一个正文分片都没有，然后一个错误帧就结束。
	fake := &streamUpstream{
		protocol:  channel.ProtocolOpenAI,
		model:     model,
		failAfter: 1,
		db:        db,
		userID:    user,
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
		t.Fatalf("状态码 = %d，期望 200（断在响应头之后，状态码已经改不了了）", resp.StatusCode)
	}

	// 一个字都没吐出来，所以调用方那边不该出现 [DONE]——那是「正常结束」的
	// 标记，而这次不是。
	for _, f := range readFrames(t, resp.Body, 0) {
		if f.data == "[DONE]" {
			t.Fatal("上游一个字都没吐，却给了调用方一个 [DONE]")
		}
	}

	bill := waitForBillingStatus(t, db, user, "released")
	if bill.final.Valid && greaterThan(bill.final.String, "0") {
		t.Errorf("final_amount = %s，期望空或零：一个字都没产生，不该计费", bill.final.String)
	}
	if !bill.upstreamStatus.Valid || bill.upstreamStatus.Int64 != http.StatusOK {
		t.Errorf("upstream_status = %v，期望 %d——上游确实回过一个响应，"+
			"丢掉它会让这一行读成「没有记录到状态码」", bill.upstreamStatus, http.StatusOK)
	}
	if !bill.errorCode.Valid || bill.errorCode.String != pipeline.ErrorUpstreamStreamAborted {
		t.Errorf("error_code = %v，期望 %q", bill.errorCode, pipeline.ErrorUpstreamStreamAborted)
	}

	// 打到哪儿同样要记：上游接受了请求（响应头是 200），那次调用真的发生过，
	// 所以渠道是确定的。这一条走的是**流式那条**释放路径——收尾在后台的
	// pumpStream 里，与非流式那条不是同一段代码，所以两边各钉一条。
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, channelID)
	}
	if !bill.upstreamModel.Valid || bill.upstreamModel.String != model {
		t.Errorf("账单的 upstream_model = %v，期望 %q", bill.upstreamModel, model)
	}

	assertWallet(t, db, user, balance, "0")
	assertNoLedgerEntry(t, db, user)
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

// 上游吐了字之后把连接掐断（没有结束标记、也没有错误帧）：两表都记
// upstream_stream_truncated，而这次**计了费**。
//
// 它走的是真链路：假上游发一条正文分片就结束响应，客户端读到流关闭。
// 断言分三块——
//
//  1. 客户端拿到正文，**没有** [DONE]（伪造一个正常结束会让半截回答
//     看起来像答完了）；
//  2. 账单是 settled，金额按估算算出来（上游没报 usage），说明这次计了费；
//  3. 快照的 error_kind 与账单的 error_code 是**同一个词**。
//
// 第 3 条是这次改动的核心：两表对同一次截断各说各的时，读的人只能靠
// 猜把两边对上；而它们的判据本该只有一处（见 gateway 的 streamTruncated）。
func TestStreamTruncatedAfterOutputRecordsSameClassification(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-truncated")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	// 一条正文分片之后直接返回：既没有结束分片，也没有 [DONE]。
	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"半句"},
		truncate: true,
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

	var sawText bool
	for _, f := range readFrames(t, resp.Body, 0) {
		if f.data == "[DONE]" {
			t.Fatal("上游没有给结束标记，却给调用方补了一个 [DONE]")
		}
		if strings.Contains(f.data, "半句") {
			sawText = true
		}
	}
	if !sawText {
		t.Fatal("调用方没收到已经吐出来的那半句")
	}

	bill := waitForBillingStatus(t, db, user, "settled")
	// 输入 7（「你好」3 + 一条消息的框架 4），输出 3（「半句」）：
	// 7 × 0.00001 + 3 × 0.00003 = 0.00016。金额真的算出来了，说明这次
	// 不是「记了条 settled 但没扣钱」。
	assertConsumeEntry(t, db, bill.id, user, "0.00016")

	waitForSnapshotRows(t, db, user, 2)
	var snapKind sql.NullString
	var snapStatus sql.NullInt64
	if err := db.QueryRowContext(context.Background(), `
		SELECT error_kind, upstream_status FROM request_snapshots
		 WHERE user_id = $1 AND direction = 'response'`, user).Scan(&snapKind, &snapStatus); err != nil {
		t.Fatalf("查响应方向的快照失败: %v", err)
	}
	if !snapKind.Valid || snapKind.String != pipeline.ErrorUpstreamStreamTruncated {
		t.Fatalf("快照的 error_kind = %v，期望 %q",
			snapKind, pipeline.ErrorUpstreamStreamTruncated)
	}
	if !snapStatus.Valid {
		t.Fatalf("快照的 upstream_status 没有值，下面那条「两表同值」就无从比起")
	}

	// 账单侧：分类与快照同值，两列都是。
	//
	// 这是 #560 接上的那一半。已计费的行不走 store.Release，它那两列由
	// markSettled 写，值由网关在流的结局那一刻拼好、经 pipeline.Settle 搬过去
	// （见 internal/gateway 的 billedStreamEvidence）。
	//
	// 改动之前这里断言的是反方向——「已结算的行上恒为 NULL」。那是接线前的
	// 事实，不是目标：它同时意味着「正常结束」与「流被截断」在账单这一侧
	// 分不开，按账单核对金额的人看不出这次回答只写了一半。接上之后反过来的
	// 这一条才是要保住的：同一次截断两表给同一个词、同一个状态码。
	if !bill.errorCode.Valid || bill.errorCode.String != pipeline.ErrorUpstreamStreamTruncated {
		t.Errorf("账单的 error_code = %v，期望 %q（与快照的 error_kind 同值）",
			bill.errorCode, pipeline.ErrorUpstreamStreamTruncated)
	}
	if !bill.upstreamStatus.Valid || bill.upstreamStatus.Int64 != snapStatus.Int64 {
		t.Errorf("账单的 upstream_status = %v，快照的 = %d：同一次截断两表必须同值",
			bill.upstreamStatus, snapStatus.Int64)
	}
}
