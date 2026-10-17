package assembly

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/upstream"
)

// 端到端：一条 HTTP 请求走完整条链，钱真的动了。
//
// # 这一组用例要证明什么
//
// 它跑的是**生产用的那条装配路径**（assembly.Build），不是测试自己接的线：
// 公共 handler、鉴权、候选来源、计费两阶段、选路、上游适配器、账本落库、
// 快照链路全部是真的，只有最上游那一段换成了一个本地假上游。
//
// 因此断言不能只看响应码。响应码正确而账没记，是这套系统里最贵的那类
// 故障——请求成功了，钱却没扣。所以每条用例都回到库里看：
//
//   - user_wallets 的余额与冻结额
//   - billing_requests 那一行的状态与金额
//   - ledger_entries 里那一笔分录
//
// # 假上游绝不能连真实上游
//
// 上游是 httptest 起的本地服务，回一个固定的 OpenAI 协议响应。仓库里
// 不出现任何真实的域名、账号与模型清单（见 AGENTS.md 的红线）。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致。**注意「跳过」与「通过」在输出里长得一样**，
// 所以报数时要把两者分开（见 LESSONS.md 第 16 条）。

// ── 测试库 ────────────────────────────────────────────────────────────

// testDB 打开集成测试用的库并把迁移推到最新。
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("LARKAX_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}

	handle, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if err := handle.PingContext(context.Background()); err != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", err)
	}

	// goose 自己记录版本，重复调用是幂等的。
	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.UpContext(context.Background(), handle, "."); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return handle
}

// 每个用例用独立的标识，避免彼此干扰。
//
// 用「时间戳 + 进程内计数」而不是纯计数：纯计数在同一个库上跑第二次
// `go test` 时会从头开始，撞上上一次留下的行。行本身不删——账本与
// 账单都是只追加的，为测试去删它们反而绕开了真实的约束。
var (
	seqMu   sync.Mutex
	seqN    int64
	seqBase = time.Now().UnixNano() % 1_000_000_000
)

func unique(prefix string) string {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqN++
	return fmt.Sprintf("%s-%d-%d", prefix, seqBase, seqN)
}

// ── 造数据 ────────────────────────────────────────────────────────────

// seedUser 建一个用户与钱包，返回用户 id。
func seedUser(t *testing.T, db *sql.DB, merchant bool, balance string) int64 {
	t.Helper()
	ctx := context.Background()

	var id int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO users (username, status, is_merchant) VALUES ($1, 'active', $2) RETURNING id`,
		unique("e2e-user"), merchant,
	).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_wallets (user_id, asset, balance) VALUES ($1, 'CNY', $2::numeric)`,
		id, balance,
	); err != nil {
		t.Fatalf("建钱包失败: %v", err)
	}
	return id
}

// issueKey 给用户签发一把 API Key，返回仅此一次的明文。
//
// 用生产代码里的那一份实现，而不是自己拼一行 INSERT：哈希口径必须与
// 鉴权那一侧完全一致，自己写一遍就等于把这个口径抄成两份。
func issueKey(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()

	created, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: userID,
		Name:   "e2e",
	})
	if err != nil {
		t.Fatalf("签发 API Key 失败: %v", err)
	}
	return created.Plaintext
}

// seedChannel 插一条指向假上游的渠道，返回渠道 id。
//
// 类型固定 openai、优先级用列的默认值（0）。要别的组合用 seedChannelOfType
// （见 stream_e2e_test.go）——两份 INSERT 早晚会分叉，而分叉的那一份不会报错。
func seedChannel(t *testing.T, db *sql.DB, sealer *credential.Sealer, owner int64, baseURL, model string) int64 {
	t.Helper()
	return seedChannelOfType(t, db, sealer, owner, baseURL, model, "openai", 0)
}

// seedRule 插一条平台作用域的计价规则，返回规则 id。
func seedRule(t *testing.T, db *sql.DB, model, expr string) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules (scope, name, model_patterns, expr, priority)
		VALUES ('platform', $1, to_jsonb(ARRAY[$2]::text[]), $3, 0)
		RETURNING id`,
		unique("e2e-rule"), model, expr,
	).Scan(&id); err != nil {
		t.Fatalf("插计价规则失败: %v", err)
	}
	return id
}

// seedChannelRule 插一条渠道作用域的计价规则，返回规则 id。
//
// 与 seedRule 分开是因为渠道级规则要挂 channel_id：它表达的是「这个渠道
// 自己的价」，平台规则只是兜底。两者同时存在时渠道价胜出（见
// internal/pricing 的裁决次序）。
func seedChannelRule(t *testing.T, db *sql.DB, channelID int64, model, expr string) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules (scope, channel_id, name, model_patterns, expr, priority)
		VALUES ('channel', $1, $2, to_jsonb(ARRAY[$3]::text[]), $4, 0)
		RETURNING id`,
		channelID, unique("e2e-rule"), model, expr,
	).Scan(&id); err != nil {
		t.Fatalf("插渠道计价规则失败: %v", err)
	}
	return id
}

func testSealer(t *testing.T) *credential.Sealer {
	t.Helper()

	key := make([]byte, config.KeySize)
	for i := range key {
		key[i] = 0x42
	}
	sealer, err := credential.NewSealer([]config.MasterKey{{ID: "test", Key: key}})
	if err != nil {
		t.Fatalf("构造 Sealer 失败: %v", err)
	}
	return sealer
}

// ── 假上游 ────────────────────────────────────────────────────────────

// fakeUpstream 是一个本地假上游：按 protocol 回一个固定响应。
//
// 它除了回响应，还记下两件事，两者都是**正向证据**：
//
//   - 收到的请求体：证明发出去的是什么（而不是「我们以为发出去的是什么」）
//   - 处理这次请求时库里看到的冻结额：证明预留发生在调上游**之前**。
//     只看请求结束后的 frozen 无法区分「冻过又放开了」与「压根没冻」——
//     而后者正是失败路径那条回归要防的东西（见 #119 修的那条）。
//
// 两种上游协议都在这里，而不是各起一个类型：渠道声明的类型决定我们往上
// 发哪套协议，而「调用协议 ≠ 上游协议」正是要测的东西之一。两份几乎相同的
// 假上游早晚会分叉，而分叉的那一份不会报错。
type fakeUpstream struct {
	db     *sql.DB
	userID int64
	model  string

	// protocol 是这条渠道说的上游协议，决定回什么形状的响应体与错误体。
	// 每个用例都显式写出来——零值 "" 既不是 openai 也不是 anthropic。
	protocol channel.Protocol

	// cacheRead 非零时在用量里报出缓存读取量。
	//
	// 这一项是两条协议在用量口径上**唯一真正的差别**：Anthropic 的
	// input_tokens 不含缓存部分（缓存读写单独报），OpenAI 的 prompt_tokens 含。
	// 不报出它，这条差别在端到端上就没有判据。
	cacheRead int64

	// cacheWrite 非零时在用量里报出缓存写入量，与 cacheRead 同理。
	//
	// 只在 Anthropic 一侧建模：`cache_creation_input_tokens` 是 Anthropic 协议
	// 里确定的字段，而 OpenAI 那一侧对应哪个键名（`cached_creation_tokens`
	// 还是 `cache_write_tokens`）各家不一，没有用例驱动就不去编一个形状出来
	//（见 LESSONS.md 第 19 条：夹具造出生产不可能出现的数据，比不造更坏）。
	cacheWrite int64

	// finishReason 是这次响应的结束原因，按**上游协议**的词汇给。
	//
	// 空串表示用本协议最常见的那一个（见 stopReason）。跨协议的用例会显式
	// 给另一个值：两家的词汇不同（end_turn ↔ stop、max_tokens ↔ length），
	// 而用一个「默认值恰好等于期望值」的输入，测不出那个值有没有真的从上游
	// 传到调用方——把转换整个删掉、直接写死默认值的实现照样能过。
	finishReason string

	// fail 为 true 时回 500，用来走失败路径。
	fail atomic.Bool

	mu       sync.Mutex
	calls    int
	paths    []string
	bodies   [][]byte
	frozenAt []string
}

func (f *fakeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// 查库看这一刻冻了多少钱。用 > 0 判定而不是比较文本：numeric
		// 的文本形式带不带尾零由列的标度决定，而这里要判断的是「冻没冻」。
		var frozen string
		var isFrozen bool
		_ = f.db.QueryRowContext(r.Context(),
			`SELECT frozen::text, frozen > 0 FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
			f.userID).Scan(&frozen, &isFrozen)

		f.mu.Lock()
		f.calls++
		f.paths = append(f.paths, r.URL.Path)
		f.bodies = append(f.bodies, body)
		f.frozenAt = append(f.frozenAt, fmt.Sprintf("%s (冻结=%t)", frozen, isFrozen))
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if f.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(f.failureBody()))
			return
		}

		_ = json.NewEncoder(w).Encode(f.successBody())
	})
}

// stopReason 返回这次响应报的结束原因。
//
// 没显式指定时用本协议最常见的那一个。指定值由用例给，且**按上游协议的词汇**
// 给——上游说的是它自己那套，转成调用方那套是链上某一层的责任。
func (f *fakeUpstream) stopReason() string {
	if f.finishReason != "" {
		return f.finishReason
	}
	if f.protocol == channel.ProtocolAnthropic {
		return "end_turn"
	}
	return "stop"
}

// successBody 按上游协议给出一次成功的响应体。
//
// 用量写死在假上游里：结算金额因此可以逐分核对，而不是「算出来多少就是
// 多少」。两套协议报的是**同一批 token**（12 个非缓存输入 / 7 个输出），
// 所以走哪条路结算金额都该是同一个数——这正是「协议不影响计费口径」那一条
// 的判据。两家的差别只在缓存那一项的报法上，而这正是跨协议转换要拆/补的：
//
//	Anthropic：input_tokens = 12（**不含**缓存）+ cache_read / cache_creation
//	OpenAI   ：prompt_tokens = 12 + 缓存（**含**缓存）+ prompt_tokens_details.cached_tokens
//
// 于是「上游报了 5 个缓存命中」在两条协议下是两个不同的输入数（12 与 17），
// 而它们说的是同一件事。转换里少补或多减一次，账单就会差出那 5 个 token 的钱。
// 缓存**写入**那一侧同理，只是它只在 Anthropic 一侧报（见 cacheWrite）。
func (f *fakeUpstream) successBody() map[string]any {
	if f.protocol == channel.ProtocolAnthropic {
		usage := map[string]any{"input_tokens": 12, "output_tokens": 7}
		// 不报缓存时**不带这个字段**：上游没报与报了 0 是两件事，
		// 而适配器把「字段不在」当成「这家没有这个概念」（见 claudeUsage）。
		if f.cacheRead > 0 {
			usage["cache_read_input_tokens"] = f.cacheRead
		}
		if f.cacheWrite > 0 {
			usage["cache_creation_input_tokens"] = f.cacheWrite
		}
		return map[string]any{
			"id":            "msg-placeholder",
			"type":          "message",
			"role":          "assistant",
			"model":         f.model,
			"content":       []any{map[string]any{"type": "text", "text": "你好，我是假上游"}},
			"stop_reason":   f.stopReason(),
			"stop_sequence": nil,
			// Anthropic 的输入侧不含缓存部分（见 internal/api/response.go 的
			// claudeUsageOf），所以这里给的就是纯输入量。
			"usage": usage,
		}
	}
	// OpenAI 的 prompt_tokens **含**缓存部分，所以缓存量要加进去。报成 12
	// 会让同一个 cacheRead 在两条协议上表示两件不同的事，而那个差别正是
	// 「拆缓存」这一步的判据所要依赖的前提。
	prompt := int64(12) + f.cacheRead
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": 7,
		"total_tokens":      prompt + 7,
	}
	if f.cacheRead > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": f.cacheRead}
	}
	return map[string]any{
		"id":      "chatcmpl-placeholder",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   f.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "你好，我是假上游"},
			"finish_reason": f.stopReason(),
		}},
		"usage": usage,
	}
}

// failureBody 按上游协议给出一个错误体。
//
// 两家的错误体形状不同，而上游的适配器是**按自己那套**去解它的：Anthropic
// 的适配器还认得出「HTTP 200 但体内是 error」这种失败（见 anthropicadapter
// 的注释），靠的就是这个形状。回错形状等于让假上游扮演一个不存在的上游。
func (f *fakeUpstream) failureBody() string {
	if f.protocol == channel.ProtocolAnthropic {
		return `{"type":"error","error":{"type":"api_error","message":"placeholder upstream failure"}}`
	}
	return `{"error":{"message":"placeholder upstream failure","type":"server_error"}}`
}

// observedFrozen 返回每次被调用时看到的冻结额原文。
func (f *fakeUpstream) observedFrozen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.frozenAt...)
}

// sentBodies 返回每次被调用时收到的请求体。
func (f *fakeUpstream) sentBodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.bodies...)
}

// sentPaths 返回每次被调用时收到的路径。
//
// 记录它是因为**端点也是协议的一部分**：Anthropic 上游在 /v1/messages 上，
// OpenAI 上游在 /v1/chat/completions 上。发错端点时上游回 404，而调用方
// 看到的是一个含糊的 502——原因只有在发出去的那一侧才看得见。
func (f *fakeUpstream) sentPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// callCount 返回上游被调用的次数。
func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ── 装配 ──────────────────────────────────────────────────────────────

// newApp 按生产路径装配一次服务。
//
// 执行器用真的 internal/upstream（适配器注册表也是被测对象的一部分），
// 只是它指向的渠道 base_url 是假上游——「不连真实上游」由渠道那一行保证。
func newApp(t *testing.T, db *sql.DB, sealer *credential.Sealer) *App {
	t.Helper()
	return newAppAt(t, db, sealer, nil)
}

// newAppAt 与 newApp 相同，但指定可注入的时钟。now 为 nil 时用真实时间。
//
// 限流按墙钟分钟切窗口。用例若用真实时间，恰好跑在整点附近的那一次会在
// 测试中途翻窗口——于是「第 61 次被拒」变成「第 61 次被放行」，用例挂在
// 一个与被测行为无关的时刻上。给一个固定时刻，这件事就从碰运气变成必然。
func newAppAt(t *testing.T, db *sql.DB, sealer *credential.Sealer, now func() time.Time) *App {
	t.Helper()

	values, err := settings.Load(context.Background(), settings.NewPgSource(db))
	if err != nil {
		t.Fatalf("加载运行期配置失败: %v", err)
	}

	executor, err := upstream.New(10 * time.Second)
	if err != nil {
		t.Fatalf("构造上游执行器失败: %v", err)
	}

	app, err := Build(Config{
		DB:       db,
		Sealer:   sealer,
		Settings: values,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:  "e2e",
		Executor: executor,
		Rules:    pricing.New(db),
		Now:      now,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	return app
}

// chatBody 组装一次调用方的请求体。
//
// max_tokens 是必须的：预留要一个输出上界，缺了它计费无法开始。
func chatBody(model string) []byte {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
		"max_tokens": 64,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// postChat 打一次公共入口，返回状态码与响应体。
func postChat(t *testing.T, url, key string, body []byte) (int, []byte) {
	t.Helper()
	status, out, _ := postChatFull(t, url, key, body)
	return status, out
}

// postChatFull 与 postChat 相同，但把响应头也交出来。
//
// 限流那一条要用 Retry-After，而它只出现在响应头里——只看状态码的话，
// 「回 429 但没告诉客户端该等多久」与「回 429 并给了间隔」在断言里一样。
func postChatFull(t *testing.T, url, key string, body []byte) (int, []byte, http.Header) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}
	return resp.StatusCode, out, resp.Header
}

// ── 断言辅助 ──────────────────────────────────────────────────────────

// sameDecimal 比较两个 numeric 的文本形式。
//
// 用数值比较而不是字符串相等：这里要断言的是金额，不是格式——尾零的
// 多少由列的标度决定，把格式写进断言会让「改一下标度」变成一堆假失败。
func sameDecimal(a, b string) bool {
	pa, errA := strconv.ParseFloat(a, 64)
	pb, errB := strconv.ParseFloat(b, 64)
	return errA == nil && errB == nil && pa == pb
}

// assertWallet 断言钱包的余额与冻结额。
func assertWallet(t *testing.T, db *sql.DB, userID int64, wantBalance, wantFrozen string) {
	t.Helper()

	var balance, frozen string
	err := db.QueryRowContext(context.Background(),
		`SELECT balance::text, frozen::text FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		userID).Scan(&balance, &frozen)
	if err != nil {
		t.Fatalf("查钱包失败: %v", err)
	}

	if !sameDecimal(balance, wantBalance) {
		t.Errorf("余额 = %s，期望 %s", balance, wantBalance)
	}
	if !sameDecimal(frozen, wantFrozen) {
		t.Errorf("冻结额 = %s，期望 %s", frozen, wantFrozen)
	}
}

// ── 用例 ──────────────────────────────────────────────────────────────

// 成功路径：一次非流式请求跑通，并且钱按上游报的用量扣了。
func TestChatCompletionsEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-success")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	// 每 token 的单价写死在规则里，于是下面的期望金额可以逐位算出来。
	rule := seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、响应形状。调用方按 OpenAI 的 SDK 解这个体，字段名错了它读不出来。
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q，期望 chat.completion", got.Object)
	}
	if got.Model != model {
		t.Errorf("model = %q，期望 %q（报给调用方的应当是他要的那个模型名）", got.Model, model)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "你好，我是假上游" {
		t.Errorf("choices = %+v，期望一条带正文的回复", got.Choices)
	}
	if got.Usage.PromptTokens != 12 || got.Usage.CompletionTokens != 7 || got.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v，期望上游报的 12/7/19", got.Usage)
	}

	// 二、钱。12 × 0.00001 + 7 × 0.00003 = 0.00033000。
	var (
		billID       int64
		requestID    string
		status_      string
		reserved     string
		final        string
		billChannel  sql.NullInt64
		ruleID       sql.NullInt64
		exprSnapshot sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, request_id, status, reserved_amount::text, final_amount::text,
		       channel_id, pricing_rule_id, pricing_expr_snapshot
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id DESC LIMIT 1`, user,
	).Scan(&billID, &requestID, &status_, &reserved, &final, &billChannel, &ruleID, &exprSnapshot)
	if err != nil {
		t.Fatalf("查账单失败: %v", err)
	}

	if status_ != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", status_)
	}
	if !sameDecimal(final, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003）", final)
	}
	// 预留是上界：结算是按实际用量算的，不可能超过它。
	if !sameDecimal(reserved, "0.00033") && !greaterThan(reserved, final) {
		t.Errorf("预留 %s 应当不小于结算 %s——预留按最坏情况算，是结算的上界", reserved, final)
	}
	if !ruleID.Valid || ruleID.Int64 != rule {
		t.Errorf("pricing_rule_id = %v，期望 %d（账单要能回答按哪条规则算的）", ruleID, rule)
	}
	// 账单还要记下这次请求**实际走的渠道**。预留发生在选路之前，那时还不知道
	// 会选中谁，所以这一列是结算那一步写回去的；写成 NULL 的话，按渠道归因
	// 用量、渠道级对账都没有数据，而单看哪一层都像是写了的。
	if !billChannel.Valid || billChannel.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d（这次请求实际走的渠道）", billChannel, channelID)
	}
	if !exprSnapshot.Valid || exprSnapshot.String == "" {
		t.Error("pricing_expr_snapshot 为空——规则事后可变，账单必须留下当时的表达式")
	}
	// 三、余额：扣掉实际金额，冻结额回到零。
	assertWallet(t, db, user, "99.99967", "0")

	// 四、账本：那一笔分录在、挂回了这次账单，且同一个 txn 内借贷相抵。
	//
	// 按 billing_request_id 找，不按科目找：分录与账单之间**必须有**这条
	// 连接，否则「这笔钱是为什么扣的」只能靠时间和金额去猜。它也是下面
	// 失败用例那条「不写分录」断言的前提——那条断言正是按这个字段查的，
	// 字段恒为 NULL 时它永远查不出东西，于是永远不失败。
	userAccount := fmt.Sprintf("user:%d", user)
	var txnID string
	err = db.QueryRowContext(ctx, `
		SELECT txn_id::text FROM ledger_entries
		 WHERE billing_request_id = $1 AND account = $2 AND entry_type = 'consume'
		 ORDER BY id DESC LIMIT 1`, billID, userAccount).Scan(&txnID)
	if err != nil {
		t.Fatalf("账本里没有挂到账单 %d 上的 %s 消费分录——请求成功了钱却没记账，或者分录与账单断了关联: %v",
			billID, userAccount, err)
	}

	var (
		entries int
		sum     string
		debit   string
	)
	err = db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(sum(amount), 0)::text
		  FROM ledger_entries WHERE txn_id = $1`, txnID).Scan(&entries, &sum)
	if err != nil {
		t.Fatalf("查分录失败: %v", err)
	}
	if entries < 2 {
		t.Errorf("同一个 txn 下有 %d 条分录，期望至少两条（一借一贷）", entries)
	}
	if !sameDecimal(sum, "0") {
		t.Errorf("分录之和 = %s，期望 0（复式记账：同一 txn 内借贷相抵）", sum)
	}

	err = db.QueryRowContext(ctx, `
		SELECT (-amount)::text FROM ledger_entries
		 WHERE txn_id = $1 AND account = $2`, txnID, userAccount).Scan(&debit)
	if err != nil {
		t.Fatalf("查用户那一侧的分录失败: %v", err)
	}
	if !sameDecimal(debit, "0.00033") {
		t.Errorf("从用户账上扣掉的 = %s，期望 0.00033（与结算金额一致）", debit)
	}

	// 五、快照链路是接上的：请求快照异步落库，所以要等它出现。
	//
	// 判据是「按这次请求的 request_id 查得到行」。报错时附上写入计数，
	// 是为了把「链路根本没接上」与「接上了但还没写完」分开——
	// 计数为零说明是前者，计数非零说明是后者。
	snapshotUser, ok := waitForSnapshot(t, db, requestID)
	if !ok {
		t.Errorf("没有等到请求快照落库（写入计数 %+v）", app.Snapshots.Stats())
	} else if snapshotUser != user {
		// 快照要记在**发起这次请求的用户**名下。这一列没有外键，填 0 或者
		// 填成商家都插得进去、不报错——所以只能断言落库的那个值。
		t.Errorf("快照的 user_id = %d，期望 %d（发起这次请求的用户）", snapshotUser, user)
	}

	// 六、上游真的收到了我们发出去的那份请求体。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if sent.Model != model {
		t.Errorf("上游收到的 model = %q，期望 %q", sent.Model, model)
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Content != "你好" {
		t.Errorf("上游收到的 messages = %+v，期望原样转发的一条用户消息", sent.Messages)
	}
}

// 客户端不给 max_tokens 时，回给它的必须是 4xx 而不是 500。
//
// 预留要有输出上界才做得出来，所以缺了它这次请求确实做不成——但**漏填字段
// 是调用方的问题**。回 500 会把「你少写了一个参数」说成「我们内部故障」：
// 调用方去查我们的状态页，而真正要改的是他那一行请求。OpenAI 兼容客户端
// 省略 max_tokens 很常见，所以这条路径不是理论上的。
//
// 走的是真实的装配路径（HTTP → 鉴权 → 预留），而不是直接调映射函数：
// 「映射表里写了 400」与「这条链上真的会走到那张表」是两件事，错误可能在
// 更早的一层就被换成别的，那样映射表再对也没用。
func TestMissingMaxTokensIsRejectedBeforeUpstream(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-no-max-tokens")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 与 chatBody 相同，只是不写 max_tokens。
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
	})
	if err != nil {
		t.Fatalf("组装请求体失败: %v", err)
	}

	status, out := postChat(t, public.URL+"/v1/chat/completions", key, body)
	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400。响应体：%s", status, out)
	}

	var got struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Errorf("错误类型 = %q，期望 invalid_request_error", got.Error.Type)
	}
	if !strings.Contains(got.Error.Message, "max_tokens") {
		t.Errorf("错误体没点名缺的是 max_tokens: %q", got.Error.Message)
	}

	// 请求在预留那一步就被挡下了，上游一次都不该被调用——调用方自己的问题
	// 不该消耗我们的上游额度与延迟。
	if n := fake.callCount(); n != 0 {
		t.Errorf("上游被调用 %d 次，期望 0 次", n)
	}
	// 钱一分都不该动：没有预留，也就没有账单行。
	assertWallet(t, db, user, "100", "0")
	var bills int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_requests WHERE user_id = $1`, user).Scan(&bills); err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	if bills != 0 {
		t.Errorf("billing_requests 有 %d 行，期望 0（请求在预留之前就被拒了）", bills)
	}
}

// 预留的上界要覆盖候选里**更贵的**那条规则，而不是只按平台规则算。
//
// 平台价便宜、这条候选渠道自己的价贵一百倍。只按平台规则预留的话，预留
// 会小于结算，结算撞上 Capped、实际金额被预留截住、差额由平台吃掉——
// 而账目仍然是平的，所以这是**静默少收**：请求成功、响应正常、钱没扣够。
//
// 断言要落到具体金额，而且要看库里的 final_amount：响应码与它无关。
func TestReserveCoversPricierCandidateRule(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-candidates")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	ch := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	// 平台兜底价：每 token 1e-5 / 3e-5。
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	// 这条渠道自己的价：贵一百倍。结算是按它算的。
	seedChannelRule(t, db, ch, model, `{"prompt": 0.001, "completion": 0.002}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var (
		status_  string
		reserved string
		final    string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT status, reserved_amount::text, final_amount::text
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id DESC LIMIT 1`, user,
	).Scan(&status_, &reserved, &final); err != nil {
		t.Fatalf("查账单失败: %v", err)
	}

	if status_ != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", status_)
	}

	// 一、结算金额按渠道价算：假上游报的是 12 输入 / 7 输出，
	// 12 × 0.001 + 7 × 0.002 = 0.012 + 0.014 = 0.026。
	//
	// 这一条是核心。只按平台价预留时，金额会被 Capped 截成平台价那个数
	// （0.00194 上下），对不上 0.026——而请求照样 200。
	if !sameDecimal(final, "0.026") {
		t.Errorf("结算金额 = %s，期望 0.026（12 × 0.001 + 7 × 0.002，渠道自己的价）；"+
			"小于它说明预留按平台价算小了，差额被平台吃掉", final)
	}

	// 二、预留必须覆盖渠道价的输出侧：max_tokens = 64，64 × 0.002 = 0.128。
	// 平台价那一侧只有 64 × 0.00003 = 0.00192，差两个数量级。
	if greaterThan("0.128", reserved) {
		t.Errorf("预留 = %s，期望不小于 0.128（按渠道价与 max_tokens = 64 算的输出侧）", reserved)
	}
	if greaterThan(final, reserved) {
		t.Errorf("预留 %s 小于结算 %s——预留是结算的上界", reserved, final)
	}

	// 三、钱：扣掉实际金额，冻结额回到零。
	assertWallet(t, db, user, "99.974", "0")
}

// 失败路径的回归：上游失败时那笔预留必须被释放。
//
// 对应 #119 修的那个缺陷——上游失败时钱永久冻在 user_wallets.frozen 上，
// 界面上表现为「用户说他的钱不见了」。断言要查库，只看响应码证明不了
// 钱有没有放开。
func TestUpstreamFailureReleasesReservation(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-fail")
	const balance = "100"

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	fake.fail.Store(true)
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502（上游失败是我们与上游之间的问题）。响应体：%s", status, body)
	}

	// 一、钱：冻结额必须回到零，余额一分没动。
	//
	// 「冻过又放开」与「压根没冻」在这里都表现为 frozen = 0，所以还要
	// 看下面那条证据——假上游在被调用时看到的冻结额。
	assertWallet(t, db, user, balance, "0")

	// 二、预留确实发生过：假上游被调用时，库里的冻结额大于零。
	// 没有这一条，上面那条断言在一个「预留根本没落库」的实现上也会通过。
	observed := fake.observedFrozen()
	if len(observed) == 0 {
		t.Fatal("假上游一次都没被调用，这次失败不是上游造成的")
	}
	for i, s := range observed {
		if !strings.Contains(s, "冻结=true") {
			t.Errorf("第 %d 次调用上游时库里的冻结额是 %s——预留必须先冻住再调上游", i+1, s)
		}
	}

	// 三、账单那一行被标成 released，且金额是预留的那一笔（不是零）。
	var (
		status_  string
		reserved string
		final    sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT status, reserved_amount::text, final_amount::text
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id DESC LIMIT 1`, user,
	).Scan(&status_, &reserved, &final)
	if err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	if status_ != "released" {
		t.Errorf("账单状态 = %q，期望 released", status_)
	}
	if !greaterThan(reserved, "0") {
		t.Errorf("预留金额 = %s，期望大于零——小于零说明这次请求根本没冻住钱，上面两条断言就没有意义", reserved)
	}
	if final.Valid && greaterThan(final.String, "0") {
		t.Errorf("final_amount = %s，期望空或零：上游失败不计费", final.String)
	}

	// 四、失败不写分录：账本里出现零金额或失败请求的分录，
	// 会让「有分录就说明发生过资金流动」这个判据失效。
	//
	// 判据是 billing_request_id 这条连接，不是金额或时间——只有它能证明
	// 「这笔分录属于这次请求」。也因此它**依赖**上面成功用例钉住的那个
	// 关联字段：字段一旦退回 NULL，`NULL IN (...)` 是 NULL 而不是 true，
	// 这里会静默变成一条恒过的断言。两条用例要一起看。
	var entries int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM ledger_entries
		 WHERE billing_request_id IN (SELECT id FROM billing_requests WHERE user_id = $1)`,
		user).Scan(&entries); err != nil {
		t.Fatalf("查分录失败: %v", err)
	}
	if entries != 0 {
		t.Errorf("失败请求写了 %d 条分录，期望 0 条", entries)
	}
}

// greaterThan 判断两个 numeric 文本形式的大小关系。
func greaterThan(a, b string) bool {
	pa, errA := strconv.ParseFloat(a, 64)
	pb, errB := strconv.ParseFloat(b, 64)
	return errA == nil && errB == nil && pa > pb
}

// waitForSnapshot 等到这次请求的快照落库，返回它记下的用户 id。
//
// 快照是异步写的（网关起 goroutine，不等结果），所以这里要等。
// 判据是按 request_id 查行：request_id 唯一，一次请求最多落一条请求方向的
// 快照，所以查得到行就是这次请求写进去的那一条，不会认成别人留下的。
//
// 顺带把 user_id 取回来：调用方要断言的就是它，而多查一次列比多跑一次
// 查询简单。取不到时第二个返回值为 false。
func waitForSnapshot(t *testing.T, db *sql.DB, requestID string) (int64, bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var userID int64
		err := db.QueryRowContext(context.Background(),
			`SELECT user_id FROM request_snapshots WHERE request_id = $1`, requestID).Scan(&userID)
		if err == nil {
			return userID, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, false
}

// TestRateLimitRPMStopsAtTheLimit 是这次改动的落点：写进库里的额度真的生效。
//
// api_keys.rate_limit_rpm 此前没有任何代码读它，于是「管理员设了 60，
// 实际是无限」——一个设了却不生效的额度比没有它更坏。这一条用例走的是
// 生产用的那条装配路径（assembly.Build）与真库，断言的是**具体数字**：
// 前 60 次 200、第 61 次 429，而不是「后面比前面少」这类相对关系。
//
// 被拒的那一次还必须**没有留下任何痕迹**：限流接在鉴权之后、预留之前，
// 所以它既不该调用上游（上游那一侧就是预留发生的地方），也不该在
// billing_requests 里留下账单行——超限的请求先冻住余额再被拒掉，
// 是这条链路上最容易写错的顺序。
func TestRateLimitRPMStopsAtTheLimit(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const limit = 60
	// 固定时刻，见 newAppAt 的注释：跨整点会让窗口在用例中途翻，
	// 那一次运行挂在一个与被测行为无关的时刻上。
	frozen := time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC)

	model := unique("e2e-model-ratelimit")
	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 额度直接写进列里：这一列没有写入路径（命令行也没有参数），
	// 而这次要证明的正是「列里的那个数被读到了」。
	if _, err := db.ExecContext(ctx,
		`UPDATE api_keys SET rate_limit_rpm = $2 WHERE key_hash = $1`,
		auth.HashKey(key), limit); err != nil {
		t.Fatalf("设置 RPM 额度失败: %v", err)
	}

	app := newAppAt(t, db, sealer, func() time.Time { return frozen })
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	body := chatBody(model)
	for i := 1; i <= limit; i++ {
		status, out := postChat(t, public.URL+"/v1/chat/completions", key, body)
		if status != http.StatusOK {
			t.Fatalf("第 %d 次状态码 = %d，期望 200。响应体：%s", i, status, out)
		}
	}

	// 第 61 次：429，并且给出重试间隔。
	status, out, header := postChatFull(t, public.URL+"/v1/chat/completions", key, body)
	if status != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次状态码 = %d，期望 429。响应体：%s", limit+1, status, out)
	}
	// 时钟固定在 12:00:30，当前窗口还剩 30 秒。
	if got := header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q，期望 %q", got, "30")
	}

	if got := fake.callCount(); got != limit {
		t.Errorf("上游被调用 %d 次，期望 %d（第 %d 次不该走到上游）", got, limit, limit+1)
	}

	var bills int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_requests WHERE user_id = $1`, user).Scan(&bills); err != nil {
		t.Fatalf("查账单条数失败: %v", err)
	}
	if bills != limit {
		t.Errorf("账单行数 = %d，期望 %d（被拒的请求不该产生预留）", bills, limit)
	}
}
