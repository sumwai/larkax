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

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/testdb"
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

// testDB 返回本包独立测试库的连接（见 internal/testdb）。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

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
	return issueKeyWithID(t, db, userID).Plaintext
}

// issueKeyWithID 与 issueKey 相同，但把 api_keys.id 也交出来。
//
// 分开一个函数是因为账单上那一列要断言的是**哪一把**（id 的字面量相等），
// 而那个 id 只有签发结果里才有——事后按明文去查表等于把「怎么定位一行」
// 再实现一遍，而那份实现与被测的那条路无关。
func issueKeyWithID(t *testing.T, db *sql.DB, userID int64) auth.Created {
	t.Helper()

	created, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: userID,
		Name:   "e2e",
	})
	if err != nil {
		t.Fatalf("签发 API Key 失败: %v", err)
	}
	return created
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
//
// # effective_from 必须显式写成一个很早的时刻
//
// 列默认是 `now()`，而**这个包的用例会注入一个冻结的时钟**（`newAppAt`）：
// 选价那条查询带 `effective_from <= $1`，于是「规则插入时的真实时刻」与
// 「用例冻结的那个时刻」谁大谁小，决定了这条规则可不可见。
//
// 冻结在**过去**的用例（`TestRateLimitRPMStopsAtTheLimit` 冻在 2026-10-17
// 12:00:30 UTC）在真实时间走过那个时刻之后就会 100% 失败，报的是
// 「没有匹配的计价规则」——**而那与用例要证的事毫无关系**。它一度真的
// 红过整条 CI，而当时看代码看不出问题：红之前那些运行都发生在那个时刻之前。
//
// 写成 1970 之后，无论用例把时钟冻在哪一个时刻，规则都已经生效。
func seedRule(t *testing.T, db *sql.DB, model, expr string) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules (scope, name, model_patterns, expr, priority, effective_from)
		VALUES ('platform', $1, to_jsonb(ARRAY[$2]::text[]), $3, 0, '1970-01-01T00:00:00Z')
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
//
// `effective_from` 与 seedRule 同一个理由显式写成很早的时刻。
func seedChannelRule(t *testing.T, db *sql.DB, channelID int64, model, expr string) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules (scope, channel_id, name, model_patterns, expr, priority, effective_from)
		VALUES ('channel', $1, $2, to_jsonb(ARRAY[$3]::text[]), $4, 0, '1970-01-01T00:00:00Z')
		RETURNING id`,
		channelID, unique("e2e-rule"), model, expr,
	).Scan(&id); err != nil {
		t.Fatalf("插渠道计价规则失败: %v", err)
	}
	return id
}

// seedCostRule 插一条渠道作用域的上游成本规则。
//
// 成本规则只按渠道挂（见 upstream_costs），所以这里没有平台兜底的那一档。
// `effective_from` 与 seedRule 同一个理由显式写成很早的时刻：用例注入的
// 冻结时钟可能落在过去。
func seedCostRule(t *testing.T, db *sql.DB, channelID int64, model, expr string) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO upstream_costs (channel_id, model_patterns, expr, priority, effective_from)
		VALUES ($1, to_jsonb(ARRAY[$2]::text[]), $3, 0, '1970-01-01T00:00:00Z')
		RETURNING id`,
		channelID, model, expr,
	).Scan(&id); err != nil {
		t.Fatalf("插成本规则失败: %v", err)
	}
	return id
}

// TestSeedRuleIsEffectiveAtAnyFrozenInstant 钉住两个 seed 帮手写进去的
// effective_from。
//
// 它守的不是「规则插进去了」，而是「无论用例把时钟冻在哪个时刻，这条规则都
// 已经生效」——见 seedRule 的注释：列默认是 now()，而用例会注入冻结的时钟，
// 于是**冻结在过去的用例会随着真实时间走过那一刻而开始 100% 失败**，报的是
// 「没有匹配的计价规则」，与它要证的事毫无关系。
//
// 判据取 2000-01-01：比它早的冻结时刻在用例里不存在，而任何「写成列默认
// now()」的实现都必然晚于它。把两处 INSERT 里的 effective_from 去掉，这条
// 立刻红。
func TestSeedRuleIsEffectiveAtAnyFrozenInstant(t *testing.T) {
	db := testDB(t)
	model := unique("e2e-model-seed-effective")

	platform := seedRule(t, db, model, `{"prompt": 0.00001}`)
	channel := seedChannelRule(t, db,
		seedChannel(t, db, testSealer(t), seedUser(t, db, true, "0"), "http://127.0.0.1:1", model),
		model, `{"prompt": 0.00002}`)

	cutoff := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		id   int64
	}{{"平台规则", platform}, {"渠道规则", channel}} {
		var from time.Time
		if err := db.QueryRowContext(context.Background(),
			`SELECT effective_from FROM pricing_rules WHERE id = $1`, c.id).Scan(&from); err != nil {
			t.Fatalf("查 %s 的 effective_from 失败: %v", c.name, err)
		}
		if !from.Before(cutoff) {
			t.Errorf("%s 的 effective_from = %v，期望早于 %v——写成列默认的 now() "+
				"会让冻结在过去时钟的用例失败", c.name, from, cutoff)
		}
	}
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

	// fail 为 true 时回一个错误响应，用来走失败路径。
	fail atomic.Bool

	// failStatus 是 fail 为 true 时回的状态码，0 表示 500。
	//
	// 单开一项而不是把 fail 换成一个整数：已有用例读的就是 fail 这个布尔，
	// 而它们要的是「失败了」这件事本身，与几号无关。只有需要分辨**哪个**
	// 状态码的用例才设它——账单上要落的就是那个数，所以它不能是编的。
	failStatus atomic.Int64

	// 下面四项只对 ProtocolOpenAIResponses 有意义。Responses 的流式是一串
	// **带事件名的事件**，与非流式那个 JSON 体是两套形状；另外两条协议的流式帧
	// （OpenAI 的裸 data: 分片、Anthropic 的带事件名帧）在 stream_e2e_test.go 的
	// streamUpstream 里，本类型不重复实现它们——那两套与这里的字段名毫无重合。
	//
	// stream 为 true 时按事件流回，而不是回一个 JSON 体。
	stream bool
	// streamChunks 是流式那条路要吐的正文分片，按顺序吐。
	streamChunks []string
	// failAfterStreamEvents > 0 时，吐完这么多条事件之后报一个流中途错误。
	//
	// 判据是**第几条事件**而不是第几个分片：这条流的第一条事件是
	// response.created，它不是一个分片，按分片数记会数错一条。
	failAfterStreamEvents int
	// toolCall 非 nil 时这次响应里带一次工具调用：非流式放在 output[] 里，
	// 流式按事件吐出来（分片由 streamToolArgs 给）。
	toolCall *fakeToolCall
	// streamToolArgs 是流式吐的工具参数分片，按顺序吐。
	// 空表示把 toolCall.Arguments 一次给全。
	streamToolArgs []string

	mu       sync.Mutex
	calls    int
	paths    []string
	bodies   [][]byte
	frozenAt []string
}

// failCode 是这次失败要回的状态码。
func (f *fakeUpstream) failCode() int {
	if code := f.failStatus.Load(); code != 0 {
		return int(code)
	}
	return http.StatusInternalServerError
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
			w.WriteHeader(f.failCode())
			_, _ = w.Write([]byte(f.failureBody()))
			return
		}

		// 流式那条路：调用方要的是 SSE，而它的形状与非流式那个 JSON 体是
		// 两套。判据里带上协议，是因为本类型的流式只实现了 Responses 这一套
		//（见 stream 的说明）——协议对不上时照旧回 JSON，用例会挂在
		// 「收到的是一个 JSON 体」上，而不是挂在一个不认识的形状上。
		if f.stream && f.protocol == channel.ProtocolOpenAIResponses {
			f.serveResponsesStream(w)
			return
		}

		_ = json.NewEncoder(w).Encode(f.successBody())
	})
}

// upstreamModelName 返回假上游在响应体里**回报**的模型名。
//
// 它故意与请求里的模型名不同：真实上游回报的名字经常不是调用方要的那一个
// （别名、版本后缀、上游自己的路由）。两边用同一个字符串时，「响应里报给
// 调用方的是哪一个」就分不出来——把响应体的 model 换成上游回报的那个，
// 用例照样全绿。加上这个后缀，那条断言才真的钉得住东西。
//
// UpstreamModel 只被插件与快照消费，不参与计价（计价按渠道 + 规范模型名），
// 所以改它不影响这些用例里的金额。
func (f *fakeUpstream) upstreamModelName() string { return f.model + "-upstream" }

// stopReason 返回这次响应报的结束原因。
//
// 没显式指定时用本协议最常见的那一个。指定值由用例给，且**按上游协议的词汇**
// 给——上游说的是它自己那套，转成调用方那套是链上某一层的责任。
func (f *fakeUpstream) stopReason() string {
	if f.finishReason != "" {
		return f.finishReason
	}
	switch f.protocol {
	case channel.ProtocolAnthropic:
		return "end_turn"
	case channel.ProtocolOpenAIResponses:
		// Responses 里没有「结束原因」这个字段，它报的是 **status**
		//（completed / incomplete / failed）。所以这里返回的就是那个 status，
		// 用例要别的取值时按这一套词汇给（见 finishReason）。
		return "completed"
	default:
		return "stop"
	}
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
	if f.protocol == channel.ProtocolOpenAIResponses {
		// 有工具调用时**只给那一项**：这一次回答里没有正文，而「只说了一句
		// 工具调用」与「说了话再调工具」是两种形状。混在一起的话，output[]
		// 的下标就取决于两项的顺序，而那正是断言要看的东西之一。
		output := []any{responsesMessageItem("completed", "你好，我是假上游")}
		if f.toolCall != nil {
			output = []any{f.toolCall.item("completed")}
		}
		return f.responsesBody(f.stopReason(), output)
	}
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
			"model":         f.upstreamModelName(),
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
		"model":   f.upstreamModelName(),
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
	// Responses 的错误体与 OpenAI 那一份是**同一个形状**（两家共用同一个官方
	// SDK 的错误解析器，见 internal/api/error.go），所以这里不另写一份。
	return `{"error":{"message":"placeholder upstream failure","type":"server_error"}}`
}

// ── Responses 上游 ────────────────────────────────────────────────────

// 假上游在 Responses 那条路上用的固定标识。
//
// 写死而不是随机：调用方看到的 id 是我们自己发的（见 responsesMessageItemID），
// 这几个只出现在「上游报的那一项有没有被认出来」这类判据里。
const (
	responsesUpstreamResponseID = "resp-placeholder"
	responsesUpstreamMessageID  = "resp-placeholder_msg_0"
	responsesUpstreamCallItemID = "resp-placeholder_call_0"
)

// fakeToolCall 是假上游要报出的一次工具调用。
//
// 与 stream_e2e_test.go 的 toolCallDelta 分开：那一个是**上游分片**的模型
// （id 与名字只在首片给），而这里是一次完整的调用——非流式那条路本来就一次
// 给全，流式那边由 streamToolArgs 决定怎么切。
type fakeToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

// item 造出这次工具调用在 output[] 里的那一项。
//
// arguments 用**字符串形态的 JSON**（`"{\"a\":1}"`）：官方形状就是这样，
// 而适配器对两种形态都收（见 responsesadapter 的 arguments）。假上游该扮演的
// 是官方那一种——形态失真时测的就不是真实上游发出来的东西了。
func (t fakeToolCall) item(status string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": responsesUpstreamCallItemID, "status": status,
		"call_id": t.CallID, "name": t.Name, "arguments": t.Arguments,
	}
}

// responsesMessageItem 造一个 Responses 的正文项。
//
// text 为空时 content 是空数组而不是缺字段：官方形状里它是必填的列表，
// 而流式那条路的 output_item.added 正是「项已经建好、正文还没来」那一刻。
func responsesMessageItem(status, text string) map[string]any {
	content := []any{}
	if text != "" {
		content = append(content, map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		})
	}
	return map[string]any{
		"type": "message", "id": responsesUpstreamMessageID, "status": status,
		"role": "assistant", "content": content,
	}
}

// responsesReasoningTokens 是假上游在 Responses 用量里报的推理量。
//
// 取一个非零值：推理在 Responses 里嵌在 output_tokens_details 里，而它在计费上
// 要从输出里**减掉**再单独计价（见 billing.FromChannelUsage）。报 0 的话，
// 「推理有没有被读到」与「它有没有被重复计费」都会得到同一个金额，两处都测不出来。
const responsesReasoningTokens = 3

// responsesUsageBody 按 Responses 的口径给用量。
//
// 与 OpenAI 那一份报的是同一批 token，差别只在缓存与推理两项的**位置**：这里
// 嵌在 input_tokens_details / output_tokens_details 里，而 chat 那边叫
// prompt_tokens_details / completion_tokens_details。
//
// input_tokens **含**缓存（details 是它的子集），所以缓存量要加进去——与
// OpenAI 那一份同一个理由。output_tokens 同理**含**推理。
func (f *fakeUpstream) responsesUsageBody() map[string]any {
	input := int64(12) + f.cacheRead
	return map[string]any{
		"input_tokens":  input,
		"output_tokens": 7,
		"total_tokens":  input + 7,
		"input_tokens_details": map[string]any{
			"cached_tokens":      f.cacheRead,
			"cache_write_tokens": f.cacheWrite,
		},
		"output_tokens_details": map[string]any{"reasoning_tokens": responsesReasoningTokens},
	}
}

// responsesBody 造一个 Responses 的响应对象。
//
// 非流式的响应体与流式终态事件里嵌的那个对象是**同一个东西**，所以只写一份：
// 两处各写一份的话，它们迟早会在「用量在不在里面」这类地方分岔，而分岔的
// 那一份不会报错。
func (f *fakeUpstream) responsesBody(status string, output []any) map[string]any {
	return map[string]any{
		"id": responsesUpstreamResponseID, "object": "response", "status": status,
		"model": f.upstreamModelName(), "output": output, "usage": f.responsesUsageBody(),
	}
}

// toolStreamArgs 是这次工具调用要吐的参数分片。
func (f *fakeUpstream) toolStreamArgs() []string {
	if len(f.streamToolArgs) > 0 {
		return f.streamToolArgs
	}
	return []string{f.toolCall.Arguments}
}

// serveResponsesStream 按 Responses 的事件形状吐一次流。
//
// 顺序照真实上游：created → 输出项的 added / 增量 / done → 终态。用量**只在
// 终态事件里**给（这是 Responses 与 chat 的一处差别：chat 要显式带
// stream_options.include_usage），所以这次结算的依据只能从那里来。
//
// 每条事件都带 event: 行，与真实上游一致。适配器会跳过它（事件名以载荷里的
// type 为准），所以这条流同时证明「有 event: 行时也解析得出来」。
func (f *fakeUpstream) serveResponsesStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}

	written := 0
	// write 写一条事件。已经吐够 failAfterStreamEvents 条时改写成一条错误事件
	// 并返回 false，调用方据此收工——那模拟的是「上游在流中途断了」，它与
	// 「上游在接受请求之前就失败」是两条不同的路（后者回一个非 2xx）。
	write := func(eventType string, payload map[string]any) bool {
		if f.failAfterStreamEvents > 0 && written >= f.failAfterStreamEvents {
			// type 必须写在载荷里，不能只写在 event: 行上：适配器跳过 event:
			// 行，按载荷里的 type 分支（真实上游两处都有，而载荷那一处才是
			// 它读的）。只写 event: 行的话这条事件会被当成不认识而静默丢掉。
			writeResponsesEvent(w, "error", map[string]any{
				"type": "error", "message": "placeholder upstream failure",
			})
			flush()
			return false
		}
		payload["type"] = eventType
		writeResponsesEvent(w, eventType, payload)
		flush()
		written++
		return true
	}

	if !write("response.created", map[string]any{
		"response": f.responsesBody("in_progress", []any{}),
	}) {
		return
	}

	if f.toolCall != nil {
		// 工具调用：项先建好（名字与 call_id 在这一条里给），参数靠后面的分片
		// 拼出来——与 Anthropic 的 content_block_start 同一个形状。
		if !write("response.output_item.added", map[string]any{
			"output_index": 0, "item": f.toolCall.item("in_progress"),
		}) {
			return
		}
		for _, args := range f.toolStreamArgs() {
			if !write("response.function_call_arguments.delta", map[string]any{
				"output_index": 0, "item_id": responsesUpstreamCallItemID, "delta": args,
			}) {
				return
			}
		}
		if !write("response.function_call_arguments.done", map[string]any{
			"output_index": 0, "item_id": responsesUpstreamCallItemID,
			"arguments": f.toolCall.Arguments,
		}) {
			return
		}
		item := f.toolCall.item("completed")
		if !write("response.output_item.done", map[string]any{"output_index": 0, "item": item}) {
			return
		}
		write("response.completed", map[string]any{
			"response": f.responsesBody("completed", []any{item}),
		})
		return
	}

	if !write("response.output_item.added", map[string]any{
		"output_index": 0, "item": responsesMessageItem("in_progress", ""),
	}) {
		return
	}
	var text strings.Builder
	for _, chunk := range f.streamChunks {
		if !write("response.output_text.delta", map[string]any{
			"output_index": 0, "item_id": responsesUpstreamMessageID, "delta": chunk,
		}) {
			return
		}
		text.WriteString(chunk)
	}
	if !write("response.output_text.done", map[string]any{
		"output_index": 0, "item_id": responsesUpstreamMessageID, "text": text.String(),
	}) {
		return
	}
	item := responsesMessageItem("completed", text.String())
	if !write("response.output_item.done", map[string]any{"output_index": 0, "item": item}) {
		return
	}
	write("response.completed", map[string]any{
		"response": f.responsesBody("completed", []any{item}),
	})
}

// writeResponsesEvent 写一条 Responses 的 SSE 事件。
//
// 载荷都由字面量拼成，编不出来只可能是夹具自己写错了，所以当场炸掉而不是
// 静默少发一条——少发一条事件会让用例挂在一个看不出原因的地方。
func writeResponsesEvent(w io.Writer, eventType string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw)
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
		Costs:    pricing.NewCostSource(db),
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
//
// ctx 由调用方传进来，一路传到那条 HTTP 请求（见 postChatFull）：
// 本函数不造 ctx，也就不会把调用方的超时挡在外面。
func postChat(ctx context.Context, t *testing.T, url, key string, body []byte) (int, []byte) {
	t.Helper()
	status, out, _ := postChatFull(ctx, t, url, key, body)
	return status, out
}

// postChatFull 与 postChat 相同，但把响应头也交出来。
//
// 限流那一条要用 Retry-After，而它只出现在响应头里——只看状态码的话，
// 「回 429 但没告诉客户端该等多久」与「回 429 并给了间隔」在断言里一样。
//
// ctx 由调用方传进来，不在这里造一个 context.Background()：这一发属于哪次
// 用例、受不受那次用例的超时约束，是调用方才知道的事（contextcheck 报的
// 就是这一条，本仓的 clientCanceledAt 是同一个形状）。自己造一个的话，
// 用例超时了这次请求还挂在连接上。
func postChatFull(ctx context.Context, t *testing.T, url, key string, body []byte) (int, []byte, http.Header) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
	// 上游成本是**另一条规则线**，单价有意与售价不同：把售价顶上去的实现
	// 算出来的数也会「看起来正常」，只有两个数不相等才拆得穿。
	seedCostRule(t, db, channelID, model, `{"prompt": 0.000002, "completion": 0.000004}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
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
		computed     sql.NullString
		billChannel  sql.NullInt64
		ruleID       sql.NullInt64
		exprSnapshot sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, request_id, status, reserved_amount::text, final_amount::text,
		       computed_amount::text,
		       channel_id, pricing_rule_id, pricing_expr_snapshot
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id DESC LIMIT 1`, user,
	).Scan(&billID, &requestID, &status_, &reserved, &final, &computed,
		&billChannel, &ruleID, &exprSnapshot)
	if err != nil {
		t.Fatalf("查账单失败: %v", err)
	}

	if status_ != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", status_)
	}
	if !sameDecimal(final, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003）", final)
	}
	assertNotCapped(t, billingRow{requestID: requestID, reserved: reserved, computed: computed})
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
	// 上游成本落在用量事件上，按（渠道，规范模型）那条成本规则乘实际用量：
	// 12 × 0.000002 + 7 × 0.000004 = 0.00005200。它与上面那个售价（0.00033）
	// 是两列不同的事——毛利算的是两者之差，混起来永远是零。
	var upstreamCost sql.NullString
	if costErr := db.QueryRowContext(ctx, `
		SELECT upstream_cost::text FROM usage_events WHERE request_id = $1`, requestID,
	).Scan(&upstreamCost); costErr != nil {
		t.Fatalf("查用量事件的上游成本失败: %v", costErr)
	}
	if !upstreamCost.Valid || !sameDecimal(upstreamCost.String, "0.000052") {
		t.Errorf("usage_events.upstream_cost = %+v，期望 0.000052"+
			"（12 × 0.000002 + 7 × 0.000004，不是售价 0.00033）", upstreamCost)
	}
	// 汇总那一半：结算写完用量事件之后，Recorder 按桶重算健康度，成本那一列
	// 取同一个桶里 upstream_cost 之和。两半各自绿而中间那条连接断了的话，
	// 成本这一维仍然永远没有数据——所以这里按渠道读回库里那一行。
	var bucketCost sql.NullString
	if costErr := db.QueryRowContext(ctx, `
		SELECT cost::text FROM channel_health_rollup WHERE channel_id = $1`, channelID,
	).Scan(&bucketCost); costErr != nil {
		t.Fatalf("查渠道健康度桶的成本失败: %v", costErr)
	}
	if !bucketCost.Valid || !sameDecimal(bucketCost.String, "0.000052") {
		t.Errorf("channel_health_rollup.cost = %+v，期望 0.000052"+
			"（这个桶里只有这一次请求）", bucketCost)
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

	// 六、一次请求落**两行**：请求方向与响应方向，后者带着上游那个 200。
	//
	// 两行是配对读的——一份说发出去的是什么，一份说回来的是什么。只落一行
	// 时 `direction = 'response'` 这个枚举值在表里等于不存在，而那正是这张
	// 表此前几个月的形状。
	waitForSnapshotRows(t, db, user, 2)
	breakdown := snapshotBreakdownOf(t, db, user)
	byDir := make(map[string]snapshotDirectionRow, len(breakdown))
	for _, b := range breakdown {
		byDir[b.direction] = b
	}
	if len(byDir) != 2 {
		t.Fatalf("按 direction 分组得到 %d 组，期望 2 组（request 与 response）：%+v",
			len(byDir), breakdown)
	}
	if b := byDir["request"]; b.rows != 1 || b.withStatus != 0 || b.withKind != 0 {
		t.Errorf("request 那一组 = %+v，期望 1 行且两列都为空（那两列说的是上游回来的什么）", b)
	}
	if b := byDir["response"]; b.rows != 1 || b.withStatus != 1 || b.withKind != 0 {
		t.Errorf("response 那一组 = %+v，期望 1 行、upstream_status 非空、error_kind 为空", b)
	}
	var gotStatus sql.NullInt64
	var gotKind sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT upstream_status, error_kind
		  FROM request_snapshots
		 WHERE request_id = $1 AND direction = 'response'`, requestID).
		Scan(&gotStatus, &gotKind); err != nil {
		t.Fatalf("查响应方向的快照失败: %v", err)
	}
	if !gotStatus.Valid || gotStatus.Int64 != http.StatusOK {
		t.Errorf("响应快照的 upstream_status = %+v，期望 %d（上游真的回了它）",
			gotStatus, http.StatusOK)
	}
	if gotKind.Valid {
		t.Errorf("成功时响应快照的 error_kind 应当为空，实际 %q", gotKind.String)
	}

	// 七、上游真的收到了我们发出去的那份请求体。
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

// 账单要记下这次请求用的是**哪一把** API Key。
//
// 这一列此前有列、有外键、有注释，却没有任何写入点：SaveReservation 的
// 列清单里没有它，而那是这张表唯一的 INSERT。于是每一行都是 NULL，
// 「按 API Key 查用量、成本与异常」在账本上做不了，只能退到按用户——
// 而一个用户本来就可能有多把 key，那正是要区分它们的原因。
//
// 这条用例走的是**生产用的那条装配路径**（HTTP → 鉴权 → 预留 → 落库），
// 因为这一列的值来自鉴权那一步：链上任何一层没把它传下去，库里那一格就是
// NULL，而单看哪一层都像是写了的。
//
// 断言是**那把 key 的 id 字面量**，不是「非 NULL」：一个把所有请求都写成
// 同一把 key 的实现也能过「非 NULL」，而它给出的答案同样是错的。所以这里
// 给**同一个用户**签发两把 key，各打一次请求，各自认自己那一个 id。
func TestBillingRequestRecordsAPIKeyID(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-apikey")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	first := issueKeyWithID(t, db, user)
	second := issueKeyWithID(t, db, user)
	if first.ID == second.ID {
		t.Fatalf("两把 key 拿到了同一个 id %d，用例本身不成立", first.ID)
	}

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	cases := []struct {
		name string
		key  auth.Created
	}{
		{name: "第一把 key", key: first},
		{name: "第二把 key", key: second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", tc.key.Plaintext, chatBody(model))
			if status != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
			}

			// 按响应里的 id 定位这次请求那一行，而不是「取最新的一行」：
			// 后者在两次请求之间没有任何东西保证顺序，而顺序一变，用例就会
			// 拿第一把 key 的账单去对第二把的期望。
			var echoed struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(body, &echoed); err != nil {
				t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
			}
			if echoed.ID == "" {
				t.Fatalf("响应里没有 id，无法定位这次请求的账单：%s", body)
			}

			var apiKeyID sql.NullInt64
			err := db.QueryRowContext(ctx,
				`SELECT api_key_id FROM billing_requests WHERE request_id = $1`,
				echoed.ID).Scan(&apiKeyID)
			if err != nil {
				t.Fatalf("查账单 %s 失败: %v", echoed.ID, err)
			}
			if !apiKeyID.Valid {
				t.Fatalf("账单 %s 的 api_key_id 是 NULL，期望 %d——这一列没有写入点",
					echoed.ID, tc.key.ID)
			}
			if apiKeyID.Int64 != tc.key.ID {
				t.Errorf("账单 %s 的 api_key_id = %d，期望 %d（这次请求用的那把 key）",
					echoed.ID, apiKeyID.Int64, tc.key.ID)
			}
		})
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

	status, out := postChat(ctx, t, public.URL+"/v1/chat/completions", key, body)
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

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var (
		requestID string
		status_   string
		reserved  string
		final     string
		computed  sql.NullString
	)
	if err := db.QueryRowContext(ctx, `
		SELECT request_id, status, reserved_amount::text, final_amount::text,
		       computed_amount::text
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id DESC LIMIT 1`, user,
	).Scan(&requestID, &status_, &reserved, &final, &computed); err != nil {
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
	assertNotCapped(t, billingRow{requestID: requestID, reserved: reserved, computed: computed})

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

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
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

// 不计费的结局要在账单上留下「上游回了什么」。
//
// 这两列（upstream_status / error_code）此前恒为 NULL：写入那条 UPDATE 的
// 参数位写的是字面量 0 与空串，而失败那条路根本不走结算。于是事后核对
// 「这次结算对应的上游回了什么」时，账单表答不出来——得去 join 快照表，
// 而快照是可以被关掉、按月清掉的。
//
// # 为什么判据是「有一条已知该有值的记录」
//
// 「永远是 NULL」与「这一批恰好没有值」在单次查询里长得一样，所以「有没有
// 非空的」证明不了任何东西。这里跑的是当初发现这个缺陷的那个场景（几条成功
// 加两条不同的失败），断言落在**那两条已知该有值的行**上。
//
// 两条失败的落法不同，必须都钉：上游回 400 那一条有状态码，连不上那一条
// **没有**——上游一个响应都没回过。只钉前一条的话，把状态码无条件写成
// 取回来的值（连不上时是 0）同样能过，而 0 在库里会读成「上游回了 0 号」。
func TestUpstreamFailureIsRecordedInBillingRequest(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-status")
	// 连不上那条渠道服务另一个模型：选路按模型筛候选，所以两个模型各走各的
	// 渠道，两条失败不会互相顶掉（见 route.Select）。
	unreachableModel := unique("e2e-model-unreachable")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 127.0.0.1:1 上不会有人监听，于是这一条走的是「一个 HTTP 响应都没拿到」。
	seedChannel(t, db, sealer, merchant, "http://127.0.0.1:1", unreachableModel)
	seedRule(t, db, unreachableModel, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	const successes = 3
	for i := 0; i < successes; i++ {
		status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
		if status != http.StatusOK {
			t.Fatalf("第 %d 次成功请求的状态码 = %d，期望 200。响应体：%s", i+1, status, body)
		}
	}

	// 上游回 400。调用方拿到的是 502 而不是 400：上游说「你的请求不对」时
	// 回 400，会让调用方去改一个本来正确的请求（见 internal/api 的
	// httpStatusOfUpstream），所以这一条不能拿响应码当判据。
	fake.failStatus.Store(http.StatusBadRequest)
	fake.fail.Store(true)
	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model)); status != http.StatusBadGateway {
		t.Fatalf("上游 400 时调用方拿到的状态码 = %d，期望 502。响应体：%s", status, body)
	}

	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(unreachableModel)); status != http.StatusBadGateway {
		t.Fatalf("连不上上游时调用方拿到的状态码 = %d，期望 502。响应体：%s", status, body)
	}

	rows := billingRowsOf(t, db, user)
	if len(rows) != successes+2 {
		t.Fatalf("账单行数 = %d，期望 %d（%d 条成功 + 上游 400 + 连不上）",
			len(rows), successes+2, successes)
	}

	// 一、成功的那几条：状态是 settled，两列留 NULL。
	//
	// 成功那些行没有可记的依据：不计费的行上这两列说的是
	// 「因上游失败而不计费」的证据，计费的行上说的是
	// 「钱收了，回答是不是在调用方不知情的情况下被截短」（唯一取值是
	// upstream_stream_truncated，见 #560）——成功那些行上两条都不成立。
	// 上游那个 200 记在**快照**的响应方向那一行上（见下面第五节）。
	//
	// 注意这句话不等于「这次没有上游失败」：上游吐了字之后才失败的那一次
	// 要计费，账单同样是 settled、两列同样是 NULL（见
	// stream_e2e_test.go 的 TestStreamUpstreamErrorMidStream）。所以
	// `error_code IS NOT NULL` 不是「上游出过问题的请求」的判据。
	for i, row := range rows[:successes] {
		if row.status != "settled" {
			t.Errorf("第 %d 条成功请求的账单状态 = %q，期望 settled", i+1, row.status)
		}
		if row.upstreamStatus.Valid || row.errorCode.Valid {
			t.Errorf("第 %d 条成功请求的两列应当是 NULL，得到 %v / %v",
				i+1, row.upstreamStatus, row.errorCode)
		}
	}

	// 二、上游 400：状态码就是上游回的那个，分类是「上游回了个错误状态码」。
	//
	// 失败的那一行是 released 而不是 settled——上游失败不计费（见
	// pipeline.Settle 的 UpstreamFailed 分支），预留被放开。
	rejected := rows[successes]
	if rejected.status != "released" {
		t.Errorf("上游 400 那一行的状态 = %q，期望 released", rejected.status)
	}
	if !rejected.upstreamStatus.Valid || rejected.upstreamStatus.Int64 != http.StatusBadRequest {
		t.Errorf("上游 400 那一行的 upstream_status = %v，期望 400", rejected.upstreamStatus)
	}
	if !rejected.errorCode.Valid || rejected.errorCode.String != pipeline.ErrorUpstreamHTTPError {
		t.Errorf("上游 400 那一行的 error_code = %v，期望 %q",
			rejected.errorCode, pipeline.ErrorUpstreamHTTPError)
	}

	// 三、连不上：**没有**状态码，两列里只有分类有值。
	//
	// 写成 0 会让「上游回了 0 号」与「上游根本没回过」在库里分不开，而 HTTP
	// 里没有 0 这个状态码——0 是「没有值」的编码，落库时要变成 NULL。
	unreachable := rows[successes+1]
	if unreachable.status != "released" {
		t.Errorf("连不上那一行的状态 = %q，期望 released", unreachable.status)
	}
	if unreachable.upstreamStatus.Valid {
		t.Errorf("上游没给出状态码时 upstream_status = %v，期望 NULL",
			unreachable.upstreamStatus)
	}
	if !unreachable.errorCode.Valid || unreachable.errorCode.String != pipeline.ErrorUpstreamNoAnswer {
		t.Errorf("连不上上游时的 error_code = %v，期望 %q",
			unreachable.errorCode, pipeline.ErrorUpstreamNoAnswer)
	}

	// 四、按 status 分组数一遍——当初发现这个缺陷用的就是这个形状的查询。
	// 它同时钉住两件事：两组的行数（少一行说明有请求没落账单），以及那两条
	// 非空的行落在哪一组——它们属于不计费的那一组。
	//
	// 计费的那些行为什么在这里也是 0：结算那条路**可以**写这两列（流被截断
	// 的那一种，见 #560），但这一组里的请求都是正常结束，没有依据可写。所以
	// 往 settled 那组挪一个非空值，就是结算那条路写了不该写的值。
	breakdown := billingBreakdownOf(t, db, user)
	t.Logf("status  | rows | non_null(upstream_status) | non_null(error_code)")
	for _, b := range breakdown {
		t.Logf("%-7s | %4d | %25d | %21d", b.status, b.rows, b.withStatus, b.withCode)
	}
	// 按 status 取而不是按下标：分组的顺序是 SQL 给的（这里按 status 排），
	// 拿它当判据的话，加一条 ORDER BY 就会让断言指向另一组。
	byStatus := make(map[string]billingStatusRow, len(breakdown))
	for _, b := range breakdown {
		byStatus[b.status] = b
	}
	if len(byStatus) != 2 {
		t.Fatalf("按 status 分组得到 %d 组，期望 2 组（settled 与 released）：%+v",
			len(byStatus), breakdown)
	}
	if b := byStatus["settled"]; b.rows != successes || b.withStatus != 0 || b.withCode != 0 {
		t.Errorf("settled 那一组 = %+v，期望 %d 行且两列都非空 0 行", b, successes)
	}
	if b := byStatus["released"]; b.rows != 2 || b.withStatus != 1 || b.withCode != 2 {
		t.Errorf("released 那一组 = %+v，期望 2 行、upstream_status 非空 1 行、error_code 非空 2 行", b)
	}

	// 五、同一次失败在**快照**那一侧也要有说法，且与账单一致。
	//
	// 这一段是本条用例存在的理由的另一半：曾经 `request_snapshots` 里一行
	// 响应方向的快照都没有，于是同一次上游 400，账单说 400、快照那一侧
	// 连行都不存在。现在两条路都写，两列的值也对得上。
	//
	// 按 direction 分组数：非空的那几条必须**全部**落在 response 那一组。
	// 请求方向那一行记的是发出去的东西，那两列（上游回了几号、这次响应怎么
	// 结束的）在那条路上不适用——它要是也有值，说明写入路径绕过了采集点。
	want := successes + 2
	waitForSnapshotRows(t, db, user, want*2)

	snapBreak := snapshotBreakdownOf(t, db, user)
	t.Logf("direction | rows | non_null(upstream_status) | non_null(error_kind)")
	for _, b := range snapBreak {
		t.Logf("%-9s | %4d | %25d | %19d", b.direction, b.rows, b.withStatus, b.withKind)
	}
	byDir := make(map[string]snapshotDirectionRow, len(snapBreak))
	for _, b := range snapBreak {
		byDir[b.direction] = b
	}
	if len(byDir) != 2 {
		t.Fatalf("按 direction 分组得到 %d 组，期望 2 组（request 与 response）：%+v",
			len(byDir), snapBreak)
	}
	if b := byDir["request"]; b.rows != want || b.withStatus != 0 || b.withKind != 0 {
		t.Errorf("request 那一组 = %+v，期望 %d 行且两列都非空 0 行（那两列说的是上游回来的什么）", b, want)
	}
	// 成功的那几条也拿到过上游的 200，所以非空的状态码是 successes+1 条；
	// 分类只有两条失败有。
	if b := byDir["response"]; b.rows != want || b.withStatus != successes+1 || b.withKind != 2 {
		t.Errorf("response 那一组 = %+v，期望 %d 行、upstream_status 非空 %d 行、error_kind 非空 2 行",
			b, want, successes+1)
	}

	// 逐行对照：两表对同一次失败给的是同一对值。
	//
	// 分组计数证明不了这件事——把 400 那一行的分类写成别的字符串，计数
	// 一样对得上。
	type evidence struct {
		status sql.NullInt64
		kind   sql.NullString
	}
	var got []evidence
	snapRows, err := db.QueryContext(ctx, `
		SELECT upstream_status, error_kind
		  FROM request_snapshots
		 WHERE user_id = $1 AND direction = 'response'
		 ORDER BY id`, user)
	if err != nil {
		t.Fatalf("查响应方向的快照失败: %v", err)
	}
	defer func() { _ = snapRows.Close() }()
	for snapRows.Next() {
		var e evidence
		if err := snapRows.Scan(&e.status, &e.kind); err != nil {
			t.Fatalf("读响应快照失败: %v", err)
		}
		got = append(got, e)
	}
	if err := snapRows.Err(); err != nil {
		t.Fatalf("读响应快照失败: %v", err)
	}
	if len(got) != want {
		t.Fatalf("响应方向的快照 = %d 条，期望 %d", len(got), want)
	}
	counts := map[string]int{}
	for _, e := range got {
		counts[evidenceKey(e.status, e.kind)]++
	}
	wantCounts := map[string]int{
		evidenceKey(sql.NullInt64{Int64: http.StatusOK, Valid: true}, sql.NullString{}): successes,
		evidenceKey(sql.NullInt64{Int64: http.StatusBadRequest, Valid: true},
			sql.NullString{String: pipeline.ErrorUpstreamHTTPError, Valid: true}): 1,
		evidenceKey(sql.NullInt64{}, sql.NullString{String: pipeline.ErrorUpstreamNoAnswer, Valid: true}): 1,
	}
	for k, n := range wantCounts {
		if counts[k] != n {
			t.Errorf("响应方向的快照里 %s 有 %d 条，期望 %d 条（实际分布 %v）", k, counts[k], n, counts)
		}
	}
}

// evidenceKey 把两列拼成一个可比较的键，供分布断言用。
func evidenceKey(status sql.NullInt64, kind sql.NullString) string {
	s := "NULL"
	if status.Valid {
		s = strconv.FormatInt(status.Int64, 10)
	}
	k := "NULL"
	if kind.Valid {
		k = kind.String
	}
	return "status=" + s + " kind=" + k
}

// 上游失败那一行也要记下「这次打的是哪个渠道、发的是哪个上游模型名」。
//
// # 缺陷原来的形状
//
// 这两列只有结算那一条路写（见 internal/ledger/store 的 markSettled），
// 而**不计费的结局根本不走结算**——它走 Release，那里只写「为什么不计费」。
// 于是成功行两列有值、失败行两列恒为 NULL，恰恰是「某个渠道对某批模型
// 全在报错」时最需要它们的地方没有它们。
//
// # 判据为什么必须落到具体值上
//
// 「非 NULL」证明不了任何东西：写了个别的渠道、或者把规范名当成上游名
// 写进去，它一样成立。所以这里给这个模型配一条别名（上游名 ≠ 规范名），
// 再把渠道 id 取回来，两个值都对着**那一个**断言——上游名对不上就说明
// 写的是 canonical_model，渠道 id 对不上就说明写的是别的候选。
//
// # 成功那一次是对照，不是重复
//
// 两条路（结算与释放）写的必须是同一对值。只钉失败那一条的话，把两处
// 各写成不同的来源也能过，而它们在库里读起来是「同一个请求走了两个渠道」。
func TestUpstreamFailureRecordsChannelAndModel(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-chan")
	upstreamName := unique("e2e-upstream-name")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	// 别名让「实际发出去的名字」与规范名不同。两者相同时，
	// 「从选路结果里取」与「把规范名抄进去」在库里长得一模一样。
	seedCatalogModel(t, db, model)
	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedAlias(t, db, channelID, upstreamName, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model)); status != http.StatusOK {
		t.Fatalf("成功请求的状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 上游回 400。调用方拿到的是 502 而不是 400（见 httpStatusOfUpstream），
	// 所以判据不能落在响应码上。
	fake.failStatus.Store(http.StatusBadRequest)
	fake.fail.Store(true)
	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model)); status != http.StatusBadGateway {
		t.Fatalf("上游 400 时调用方拿到的状态码 = %d，期望 502。响应体：%s", status, body)
	}

	rows := billingRowsOf(t, db, user)
	if len(rows) != 2 {
		t.Fatalf("账单行数 = %d，期望 2（1 条成功 + 1 条上游 400）", len(rows))
	}
	// 先钉住这两行确实是那两种结局：否则下面两条断言在一行「其实是别的
	// 请求留下的」记录上也照样成立。
	if rows[0].status != "settled" {
		t.Errorf("成功那一行的状态 = %q，期望 settled", rows[0].status)
	}
	if rows[1].status != "released" {
		t.Errorf("上游 400 那一行的状态 = %q，期望 released", rows[1].status)
	}

	for i, row := range rows {
		if !row.channelID.Valid || row.channelID.Int64 != channelID {
			t.Errorf("第 %d 行的 channel_id = %v，期望 %d（这次请求只配了这一个渠道）",
				i+1, row.channelID, channelID)
		}
		if !row.upstreamModel.Valid || row.upstreamModel.String != upstreamName {
			t.Errorf("第 %d 行的 upstream_model = %v，期望 %q（别名翻译之后的名字）；"+
				"等于 %q 说明写进去的是规范名，不是这次实际发出去的那个",
				i+1, row.upstreamModel, upstreamName, model)
		}
	}
}

// 一个候选都没选中时，那两列只能是 NULL——而且理由要说清楚。
//
// # 这里的失败发生在选出渠道之前
//
// 候选集非空（所以预留照常发生、账单行照常插入），但选路把它们全筛掉了：
// 这个渠道连续失败到了阈值（见 route.Candidate.DrainReason）。于是这次请求
// **一次上游调用都没发生**，没有任何渠道可以被记进去——硬编一个 id 出来
// 比留空更坏，那会让「这个渠道在报错」这个结论指向一个没被碰过的渠道。
//
// 与上面那条合起来才是完整的判据：只钉「有值」的那一条，把 NULLIF 换成
// 无条件写入、或者在没有渠道时也写一个进去，都不会有人报出来。
func TestUpstreamFailureWithoutSelectedChannelLeavesNull(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-drained")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 把这条渠道摘掉。两列必须**一起**写：只有计数而没有时间戳的行按
	// 「冷却期已过」处理（见 internal/channels 的 consecutiveFailures），
	// 于是它会照旧进入候选集，这一条用例就测不到「一个候选都没选中」。
	if _, err := db.ExecContext(ctx,
		`UPDATE channels SET consecutive_failures = 3, last_failure_at = now() WHERE id = $1`,
		channelID); err != nil {
		t.Fatalf("把渠道标成连续失败失败: %v", err)
	}

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	if status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model)); status != http.StatusBadGateway {
		t.Fatalf("没有可用候选时调用方拿到的状态码 = %d，期望 502。响应体：%s", status, body)
	}

	// 上游一次都不该被调用：上面那次 502 必须来自「没选出渠道」，
	// 而不是来自上游回的错——那两种失败在账单上长得一样，而这一条
	// 用例断言的是前一种。
	if bodies := fake.sentBodies(); len(bodies) != 0 {
		t.Fatalf("假上游被调用了 %d 次，期望 0 次（这个渠道已经被摘掉）", len(bodies))
	}

	rows := billingRowsOf(t, db, user)
	if len(rows) != 1 {
		t.Fatalf("账单行数 = %d，期望 1", len(rows))
	}
	row := rows[0]
	// 预留确实发生过：没有这一条，下面两条 NULL 断言在一个「压根没预留」
	// 的实现上也成立，而那种实现里 NULL 的含义完全不同。
	if row.status != "released" {
		t.Errorf("账单状态 = %q，期望 released（预留过、又放开了）", row.status)
	}
	if row.channelID.Valid {
		t.Errorf("channel_id = %v，期望 NULL：这次请求一个候选都没选中，没有渠道可记", row.channelID)
	}
	if row.upstreamModel.Valid {
		t.Errorf("upstream_model = %v，期望 NULL：没有选路就没有上游名可记", row.upstreamModel)
	}
}

// snapshotDirectionRow 是快照按 direction 分组之后的一组。
type snapshotDirectionRow struct {
	direction string
	rows      int
	// withStatus 与 withKind 是这一组里那两列非空的行数。
	//
	// 与账单那一侧同源：数的是非空而不是「等于某个值」。
	withStatus int
	withKind   int
}

// snapshotBreakdownOf 按 direction 分组数这个用户的快照，按 direction 排序。
//
// 形状与 billingBreakdownOf 一样，因为当初发现「响应方向一行都没有」用的
// 就是这个形状的查询：一张分组表能同时说明「有没有这一类行」与「两列有没有
// 值」，而只看总行数会把两件事混在一起。
func snapshotBreakdownOf(t *testing.T, db *sql.DB, userID int64) []snapshotDirectionRow {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT direction,
		       count(*),
		       count(upstream_status),
		       count(error_kind)
		  FROM request_snapshots
		 WHERE user_id = $1
		 GROUP BY direction
		 ORDER BY direction`, userID)
	if err != nil {
		t.Fatalf("按方向分组查快照失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []snapshotDirectionRow
	for rows.Next() {
		var b snapshotDirectionRow
		if err := rows.Scan(&b.direction, &b.rows, &b.withStatus, &b.withKind); err != nil {
			t.Fatalf("读分组结果失败: %v", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读分组结果失败: %v", err)
	}
	return out
}

// waitForSnapshotRows 等到这个用户的快照总数达到 want。
//
// 落库是异步的（见 gateway.snapshotAsync），所以「一条都没等到」与「还没写
// 完」必须分开：等不到时把当前的分组打出来，看的人一眼能分辨是没接上还是
// 慢了一拍。
func waitForSnapshotRows(t *testing.T, db *sql.DB, userID int64, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		total := 0
		for _, b := range snapshotBreakdownOf(t, db, userID) {
			total += b.rows
		}
		if total >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等到超时：快照总数 = %d，期望 %d。当前分组：%+v",
				total, want, snapshotBreakdownOf(t, db, userID))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// billingStatusRow 是账单按 status 分组之后的一组。
type billingStatusRow struct {
	status string
	rows   int
	// withStatus 与 withCode 是这一组里那两列非空的行数。
	//
	// 数的是非空而不是「等于某个值」：这两列的取值口径会变（上游状态码是
	// 上游给的），而「有没有值」不会。
	withStatus int
	withCode   int
}

// billingBreakdownOf 按 status 分组数这个用户的账单行，按 status 排序。
func billingBreakdownOf(t *testing.T, db *sql.DB, userID int64) []billingStatusRow {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT status,
		       count(*),
		       count(upstream_status),
		       count(error_code)
		  FROM billing_requests
		 WHERE user_id = $1
		 GROUP BY status
		 ORDER BY status`, userID)
	if err != nil {
		t.Fatalf("按状态分组查账单失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []billingStatusRow
	for rows.Next() {
		var b billingStatusRow
		if err := rows.Scan(&b.status, &b.rows, &b.withStatus, &b.withCode); err != nil {
			t.Fatalf("读分组结果失败: %v", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读分组结果失败: %v", err)
	}
	return out
}

// billingRowsOf 按 id 升序取这个用户全部账单行的状态、去向与那两列依据。
//
// 与 billingOf 分开：那个取的是**最近一行**（流式收尾要等的那一条），
// 这里要的是一批请求跑完之后每一行的样子——分组统计与逐行对照要的就是整批。
//
// channel_id 与 upstream_model 是**可空**的，扫进 sql.Null*：NULL 在
// 已结算的行上是「写入点掉了」，而在选路之前就失败的行上是正常取值。
// 两者的区分见各条用例的注释，别用零值把「没有」与「是 0」合并。
func billingRowsOf(t *testing.T, db *sql.DB, userID int64) []billingRow {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT status, channel_id, canonical_model, upstream_model,
		       upstream_status, error_code
		  FROM billing_requests
		 WHERE user_id = $1
		 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []billingRow
	for rows.Next() {
		var b billingRow
		if err := rows.Scan(&b.status, &b.channelID, &b.canonicalModel, &b.upstreamModel,
			&b.upstreamStatus, &b.errorCode); err != nil {
			t.Fatalf("读账单失败: %v", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读账单失败: %v", err)
	}
	return out
}

// waitForSnapshot 等到这次请求的快照落库，返回它记下的用户 id。
//
// 快照是异步写的（网关起 goroutine，不等结果），所以这里要等。
// 判据是按 request_id 查行：一次请求落两条（请求方向与响应方向），两条
// 同属这次请求，所以取回哪一条的 user_id 都一样，不会认成别人留下的。
// 要区分方向的断言用 snapshotBreakdownOf，不要在这里加方向条件。
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
		status, out := postChat(ctx, t, public.URL+"/v1/chat/completions", key, body)
		if status != http.StatusOK {
			t.Fatalf("第 %d 次状态码 = %d，期望 200。响应体：%s", i, status, out)
		}
	}

	// 第 61 次：429，并且给出重试间隔。
	status, out, header := postChatFull(ctx, t, public.URL+"/v1/chat/completions", key, body)
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

// TestQuotaLimitStopsAtTheLimit 是「给一把 key 设了上限，请求真的被拦住」的落点。
//
// 这一条存在的理由与上面那条限流的相同，但更硬一点：quota_limit 曾经**写得
// 进去、列得出来，而没有任何代码读它**（#200）。那种缺口在库里看不出来，在
// 命令行的输出里也看不出来——`apikey update --quota-limit` 会回一句「已更新」，
// 然后什么都没发生。所以「读到了、而且真的拦住了」必须有一条走**生产装配
// 路径**的用例钉住，而不是只靠各层各自用桩拼出来的断言。
//
// # 边界从库里读回来，不在用例里重算
//
// 预留额是上界（见 internal/pipeline 的 Reserve），而这个上界取决于模型、
// token 上限与计价规则三样东西。在用例里把那个算法再写一遍，等于把被测的
// 逻辑抄成第二份——抄错了这条用例照样通过。所以先跑一笔（不设上限），再从
// 那一行的 reserved_amount 取回边界值。
//
// # 两个阶段各挡一层
//
// 第二笔的上限设成「恰好一笔预留额」，此时**入口那一层会放行**（已结算的
// 最终额 ≤ 预留额），拦住它的是预留事务里的权威判定——那正是并发穿透要靠的
// 那一层。最后一段再把上限压到已花之下，用一段不合法的请求体去证明入口那一层
// 真的接上了：没有它，这段请求体会得到 400（解析失败）而不是 429。
func TestQuotaLimitStopsAtTheLimit(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-quota")
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

	body := chatBody(model)
	endpoint := public.URL + "/v1/chat/completions"

	setQuota := func(value string) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`UPDATE api_keys SET quota_limit = $2 WHERE key_hash = $1`,
			auth.HashKey(key), value); err != nil {
			t.Fatalf("设置额度上限失败: %v", err)
		}
	}

	// 一、没设上限时照常放行。这一笔同时给出下面要用的边界值。
	if status, out := postChat(ctx, t, endpoint, key, body); status != http.StatusOK {
		t.Fatalf("没设上限时状态码 = %d，期望 200。响应体：%s", status, out)
	}

	var reservedText, settledText string
	if err := db.QueryRowContext(ctx, `
		SELECT reserved_amount::text, COALESCE(final_amount, 0)::text
		  FROM billing_requests WHERE user_id = $1`, user).Scan(&reservedText, &settledText); err != nil {
		t.Fatalf("读账单金额失败: %v", err)
	}
	reserved, settled := money.MustParse(reservedText), money.MustParse(settledText)
	// 用例的前提：这一笔真的花了钱，而且预留额确实是上界。两者都不成立时，
	// 下面的「超限」会以别的原因通过，而那条断言就什么也没证明。
	if !settled.IsPositive() {
		t.Fatalf("这一笔的最终金额是 %s，用例的前提（已花 > 0）不成立", settled)
	}
	if settled.Cmp(reserved) > 0 {
		t.Fatalf("最终金额 %s 超过了预留额 %s：预留本该是上界", settled, reserved)
	}

	// 二、上限设成恰好一笔预留额。已花的加上再要的这一笔必然超过它。
	setQuota(reserved.String())

	status, out, header := postChatFull(ctx, t, endpoint, key, body)
	if status != http.StatusTooManyRequests {
		t.Fatalf("超出额度后状态码 = %d，期望 429。响应体：%s", status, out)
	}
	// 额度不会自己回来，所以**没有**可等的时长可给（见 writeInsufficientQuota）。
	if got := header.Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q，期望不出现：回一个等待时间等于让客户端反复重试", got)
	}
	var errBody struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &errBody); err != nil {
		t.Fatalf("解析错误体失败: %v（原文 %s）", err, out)
	}
	if errBody.Error.Code == nil || *errBody.Error.Code != "insufficient_quota" {
		t.Errorf("error.code = %v，期望 %q", errBody.Error.Code, "insufficient_quota")
	}

	// 三、被拒的那一笔没有走到上游，也没有留下账单行。
	if got := fake.callCount(); got != 1 {
		t.Errorf("上游被调用 %d 次，期望 1（被额度拒掉的那一笔不该走到上游）", got)
	}
	var bills int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_requests WHERE user_id = $1`, user).Scan(&bills); err != nil {
		t.Fatalf("查账单条数失败: %v", err)
	}
	if bills != 1 {
		t.Errorf("账单行数 = %d，期望 1（被拒的请求不该产生预留）", bills)
	}

	// 四、把上限压到已花之下，入口那一层就该在**读请求体之前**拦住它。
	//
	// 请求体故意给一段不合法的 JSON：没有这道早拒绝时它会得到 400（解析失败）。
	// 拿到 429 才说明 handler.Quota 真的接上了——接线漏掉时，上面三段全都照样
	// 通过，因为权威判定自己就能拦住超限的请求。
	setQuota("0.00000001")

	status, out = postChat(ctx, t, endpoint, key, []byte("{"))
	if status != http.StatusTooManyRequests {
		t.Fatalf("已超限时状态码 = %d，期望 429（若是 400，说明入口那道早拒绝没接上）。响应体：%s",
			status, out)
	}
	if got := fake.callCount(); got != 1 {
		t.Errorf("上游被调用 %d 次，期望 1", got)
	}
}
