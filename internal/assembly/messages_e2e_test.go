package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：走 **Anthropic 协议**那条入口（`POST /v1/messages`）的用例。
//
// # 为什么另起一组
//
// 入口按路径分派两条协议（见 internal/api/api.go），而两条路径的差别恰恰
// 在协议上：请求体形状、`max_tokens` 的必填性、响应体的字段集、错误体的
// 嵌套层次。OpenAI 那条路的一组用例（e2e_test.go）证明不了这一条——它们
// 断言的是另一套字段名。协议解析、请求转换、响应序列化这三段只有在
// 「从 HTTP 到落库」整条链上跑一遍，才谈得上被覆盖。
//
// 约定与另外两组完全一致：走生产装配路径（assembly.Build）、假上游是本地
// httptest、断言回到库里看钱。请求的发送用的是 e2e_test.go 里的
// postChat/postChatFull——它们本来就与协议无关（只设 Content-Type 与
// Authorization），名字里的 chat 是这两条路径里先写出来的那一条留下的。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。

// messagesBody 组装一次 Anthropic 协议的调用方请求体。
//
// 与 chatBody 的差别不只是字段名：`max_tokens` 在 Anthropic 协议里是
// **必填**的顶层字段（不在请求里就是不合法的请求），所以这里没有「省掉它」
// 的变体——缺它那一条用例自己组装请求体，见 TestMessagesMissingMaxTokensIsRejected。
func messagesBody(model string) []byte { return messagesBodyWith(model, "") }

// messagesBodyWith 与 messagesBody 相同，可另带一段 system。
//
// system 在 Anthropic 协议里是**顶层字段**，而 OpenAI 把它当成 messages 里的
// 一条——这是两套请求体最明显的结构差异，所以「它有没有被原样转给上游」
// 要有判据（见 TestMessagesEndToEnd）。
//
// 其余用例不带它：带上会改变本地估算的输入量，而那些用例断言的是具体金额，
// 多一个与被测行为无关的变量只会让算式难核对。
func messagesBodyWith(model, system string) []byte {
	body := map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": "你好"}},
	}
	if system != "" {
		body["system"] = system
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// ── 用例 ──────────────────────────────────────────────────────────────

// 成功路径：一次非流式的 Anthropic 请求跑通，响应按 Anthropic 的形状给，
// 钱按上游报的用量扣。
//
// 这条用例覆盖的是「同一个假上游、同一套账，换一条协议入口」——所以下面
// 期望的金额与 TestChatCompletionsEndToEnd 是同一个数。金额一致本身就是
// 一条判据：协议是**调用方**的事，不该影响计费口径。
func TestMessagesEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-messages")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol: channel.ProtocolAnthropic,
		db:       db,
		userID:   user,
		model:    model,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	// 每 token 的单价写死在规则里，于是下面的期望金额可以逐位算出来。
	rule := seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body, header := postChatFull(ctx, t, public.URL+"/v1/messages", key, messagesBodyWith(model, "你是假上游"))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、响应形状。调用方按 Anthropic 的 SDK 解这个体，字段名或嵌套层次
	// 错了它读不出来——而它与 OpenAI 那套的差别是**结构性**的（没有 choices、
	// 没有 object、没有 created，正文在 content 块数组里）。
	//
	// StopReason 用指针接：它必须是「在且是个字符串」，缺字段与 null 在
	// Anthropic 的客户端里不是一回事。
	var got struct {
		ID         string  `json:"id"`
		Type       string  `json:"type"`
		Role       string  `json:"role"`
		Model      string  `json:"model"`
		StopReason *string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		// 下面两个字段是 **OpenAI 形状**的标记。解析进一个结构体里不影响
		// 上面的断言，专门用来验证「没有混进另一套形状的字段」。
		Object  *string `json:"object"`
		Choices []any   `json:"choices"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}

	if got.Type != "message" {
		t.Errorf("type = %q，期望 message", got.Type)
	}
	if got.Role != "assistant" {
		t.Errorf("role = %q，期望 assistant", got.Role)
	}
	if got.Model != model {
		t.Errorf("model = %q，期望 %q（报给调用方的应当是他要的那个模型名）", got.Model, model)
	}
	// id 必须与响应头里的关联标识是同一个值：用户报问题时贴哪一个都该指向
	// 同一批日志。这一条只在两条协议各自真跑一遍时才成立——把 id 写成上游
	// 回报的那个（msg-placeholder）在形状上是看不出来的。
	if got.ID == "" {
		t.Error("响应体里的 id 是空的——它是本次请求的关联标识，不能省")
	}
	if want := header.Get("x-request-id"); got.ID != want {
		t.Errorf("响应体的 id = %q，x-request-id 头 = %q：两者必须是同一个值", got.ID, want)
	}
	if len(got.Content) != 1 || got.Content[0].Type != "text" || got.Content[0].Text != "你好，我是假上游" {
		t.Errorf("content = %+v，期望一个带正文的 text 块", got.Content)
	}
	if got.StopReason == nil || *got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %s，期望 end_turn", showPtr(got.StopReason))
	}
	// stop_sequence 恒为 null 但字段要在：缺了它有些客户端会当成字段丢失。
	// 判据是**键在不在**，不是它的值——所以拿原始 map 看，而不是看解出来的
	// 结构体：结构体里 nil 与「键不在」长得一样。
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if v, ok := raw["stop_sequence"]; !ok {
		t.Errorf("响应体里没有 stop_sequence 这个键：它在 Anthropic 的响应里始终存在。响应体：%s", body)
	} else if v != nil {
		t.Errorf("stop_sequence = %v，期望 null（我们不用自定义停止序列）", v)
	}
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v，期望上游报的 12/7", got.Usage)
	}
	if got.Object != nil || got.Choices != nil {
		t.Errorf("响应体里混进了 OpenAI 形状的字段（object=%s choices=%v）——两套协议各是一份独立的结构体",
			showPtr(got.Object), got.Choices)
	}

	// 二、钱。12 × 0.00001 + 7 × 0.00003 = 0.00033000，与 OpenAI 那条路同一个数。
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
	if !billChannel.Valid || billChannel.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d（这次请求实际走的渠道）", billChannel, channelID)
	}
	if !exprSnapshot.Valid || exprSnapshot.String == "" {
		t.Error("pricing_expr_snapshot 为空——规则事后可变，账单必须留下当时的表达式")
	}
	// 账单还要记下这次请求走的是哪条**调用协议**：两条入口的用量口径不同
	// （见 internal/api/response.go 的 claudeUsageOf），事后按协议归因时才分得开。
	var billProtocol string
	err = db.QueryRowContext(ctx,
		`SELECT protocol FROM billing_requests WHERE id = $1`, billID).Scan(&billProtocol)
	if err != nil {
		t.Fatalf("查账单协议失败: %v", err)
	}
	if billProtocol != string(channel.ProtocolAnthropic) {
		t.Errorf("账单的 protocol = %q，期望 %q（这次请求走的是 /v1/messages）",
			billProtocol, channel.ProtocolAnthropic)
	}

	// 三、余额：扣掉实际金额，冻结额回到零。
	assertWallet(t, db, user, "99.99967", "0")

	// 四、账本：那一笔分录在、挂回了这次账单，且同一个 txn 内借贷相抵。
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
	var sum string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(amount), 0)::text FROM ledger_entries WHERE txn_id = $1`, txnID,
	).Scan(&sum); err != nil {
		t.Fatalf("查分录失败: %v", err)
	}
	if !sameDecimal(sum, "0") {
		t.Errorf("分录之和 = %s，期望 0（复式记账：同一 txn 内借贷相抵）", sum)
	}

	// 五、快照链路是接上的，且记在发起这次请求的用户名下。
	snapshotUser, ok := waitForSnapshot(t, db, requestID)
	if !ok {
		t.Errorf("没有等到请求快照落库（写入计数 %+v）", app.Snapshots.Stats())
	} else if snapshotUser != user {
		t.Errorf("快照的 user_id = %d，期望 %d（发起这次请求的用户）", snapshotUser, user)
	}

	// 六、上游真的收到了我们发出去的那份请求体，而且它是 **Anthropic 形状**的。
	//
	// 这一条是这条路径上唯一能证明「调用方的协议真的被转成了上游的协议」的
	// 地方：响应体对不对只说明回来的那一半，发出去的那一半错了（比如按
	// OpenAI 的形状发给了 Anthropic 上游）在响应上完全看不出来。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		System    any    `json:"system"`
		Messages  []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if sent.Model != model {
		t.Errorf("上游收到的 model = %q，期望 %q", sent.Model, model)
	}
	if sent.MaxTokens != 64 {
		t.Errorf("上游收到的 max_tokens = %d，期望 64（调用方给的上界要原样传到上游，它同时是预留的依据）", sent.MaxTokens)
	}
	// system 是 Anthropic 与 OpenAI 在请求体上最明显的结构差异：这边是顶层
	// 字段，那边是 messages 里的一条。转换时丢掉它，模型的行为就变了，
	// 而调用方从响应上完全看不出来。
	if got := textOf(t, sent.System); got != "你是假上游" {
		t.Errorf("上游收到的 system = %q，期望 %q（Anthropic 的 system 是顶层字段，转换时不能丢）", got, "你是假上游")
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Role != "user" {
		t.Errorf("上游收到的 messages = %+v，期望**只有**一条用户消息（system 不该被塞进这里）", sent.Messages)
	}
	// 端点也是协议的一部分：Anthropic 上游在 /v1/messages 上，OpenAI 上游在
	// /v1/chat/completions 上。发错端点时上游多半回 404，但那是**上游**报的，
	// 从我们的响应上看只是一个含糊的 502——这一条把原因钉在这里。
	if paths := fake.sentPaths(); len(paths) != 1 || paths[0] != "/v1/messages" {
		t.Errorf("上游收到的路径 = %v，期望 [/v1/messages]（Anthropic 上游的端点）", paths)
	}
}

// 上游报了缓存读取量时，用量与金额都要按 Anthropic 的口径走。
//
// 这是两条协议在用量上**唯一真正的差别**：Anthropic 的 input_tokens 不含
// 缓存部分（缓存读写单独报），OpenAI 的 prompt_tokens 含。同一个上游响应
// 按两种口径读出来是两个不同的输入量，而它在报文里看不出是重复计了还是
// 真多了——所以两边都要有具体数字钉住。
//
// 期望值逐项算得出来。链上经过三次口径转换，每一步都要有数字钉住：
//
//	上游报的     input_tokens = 12（不含缓存）、cache_read = 5、output = 7
//	协议层用量   prompt = 12 + 5 = 17（补齐成 OpenAI 口径）、cache_read = 5
//	回给调用方   input_tokens = 17 − 5 = 12（减回去，还是他要的那套口径）
//	计费口径     prompt = 17 − 5 = 12、cache_read = 5（缓存单独定价，不能重复计）
//	金额         12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002 = 0.00034
//
// 缓存那一项的单价特意取**非零**：取 0 的话，上面每一处漏算缓存都会得到
// 同一个数（0.00033），断言就分不出「算对了」与「缓存根本没参与」。这正是
// 「喂进去的输入与断言里的期望来自同一个来源」那类假通过（见 LESSONS.md 第 21 条）。
//
// 0.00034 与另外两种错法都能分开：缓存不参与计价是 0.00033，
// 缓存被算进输入那一档是 0.00038。
func TestMessagesCacheUsageStaysInAnthropicTerms(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-messages-cache")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol:  channel.ProtocolAnthropic,
		cacheRead: 5,
		db:        db,
		userID:    user,
		model:     model,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	// cache_read 这一项必须**显式写出来**：规则里没写它时结算会直接报错
	// （「用量里有规则未定价的计量项」），而不是悄悄按 0 算。这条规矩本身
	// 值得留在这里——它是「上游多报了一类用量而我们没定价」的那道闸，
	// 而这类差错的表现是**少收钱**（第一次跑这条用例就是被它拦下的）。
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003, "cache_read": 0.000002}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/messages", key, messagesBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var got struct {
		Usage struct {
			InputTokens          int64 `json:"input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if got.Usage.InputTokens != 12 {
		t.Errorf("input_tokens = %d，期望 12（上游报的那个数）——17 说明缓存被重复计进了输入",
			got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 5 {
		t.Errorf("cache_read_input_tokens = %d，期望 5（上游报了它就该原样出现在响应里）",
			got.Usage.CacheReadInputTokens)
	}
	if got.Usage.OutputTokens != 7 {
		t.Errorf("output_tokens = %d，期望 7", got.Usage.OutputTokens)
	}

	// 金额。缓存那 5 个 token 按 cache_read 的单价单独算，**不能**再算进
	// prompt 那一档——所以最终数不是「17 个输入 × prompt 单价」。
	var final string
	if err := db.QueryRowContext(ctx, `
		SELECT final_amount::text FROM billing_requests
		 WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, user).Scan(&final); err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	if !sameDecimal(final, "0.00034") {
		t.Errorf("结算金额 = %s，期望 0.00034（12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002）；"+
			"0.00033 说明缓存没参与计价，0.00038 说明缓存被算进了输入那一档", final)
	}
	assertWallet(t, db, user, "99.99966", "0")
}

// 缺 max_tokens 时，回给调用方的必须是 4xx，且是 **Anthropic 形状**的错误体。
//
// Anthropic 协议里 max_tokens 是必填字段，但「必填」这件事要靠一条用例钉住：
// 它没有语法上的强制——缺了它请求照样能解析成结构体（值是 0），于是失败会
// 推迟到预留那一步，那时报出来的状态码与形状取决于链上谁先发现。
//
// 回 500 会把「你少写了一个字段」说成「我们内部故障」：调用方去查我们的
// 状态页，而真正要改的是他那一行请求。
func TestMessagesMissingMaxTokensIsRejected(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-messages-no-max-tokens")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol: channel.ProtocolAnthropic,
		db:       db,
		userID:   user,
		model:    model,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 与 messagesBody 相同，只是不写 max_tokens。
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	})
	if err != nil {
		t.Fatalf("组装请求体失败: %v", err)
	}

	status, out := postChat(ctx, t, public.URL+"/v1/messages", key, body)
	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400。响应体：%s", status, out)
	}

	// 错误体的形状要按调用协议给：Anthropic 是 {"type":"error","error":{...}}，
	// 顶层的 type 是这套形状的标记（OpenAI 那套没有它）。
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string  `json:"type"`
			Message string  `json:"message"`
			Param   *string `json:"param"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
	}
	if got.Type != "error" {
		t.Errorf("顶层的 type = %q，期望 error（Anthropic 错误体的标记字段）", got.Type)
	}
	if got.Error.Type != "invalid_request_error" {
		t.Errorf("错误类型 = %q，期望 invalid_request_error", got.Error.Type)
	}
	if !strings.Contains(got.Error.Message, "max_tokens") {
		t.Errorf("错误体没点名缺的是 max_tokens: %q", got.Error.Message)
	}
	// param / code 是 OpenAI 错误体的字段。它们出现在这里说明写错了形状。
	if got.Error.Param != nil || got.Error.Code != nil {
		t.Errorf("错误体里出现了 OpenAI 才有的字段（param=%s code=%s）",
			showPtr(got.Error.Param), showPtr(got.Error.Code))
	}

	// 请求在预留那一步就被挡下了，上游一次都不该被调用。
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

// 上游 5xx 时，回给调用方的是 **Anthropic 形状**的错误体，且这次请求不计费。
//
// 与非流式那条 OpenAI 的失败用例（TestUpstreamFailureReleasesReservation）
// 同一件事，只是换了一条入口。之所以值得单独跑一遍：错误体是由
// writeError 按**调用协议**渲染的（见 internal/api/error.go），而两条协议的
// 形状是两份独立的结构体——OpenAI 那条路的用例证明不了 Anthropic 这份被用上。
func TestMessagesUpstreamFailureReturnsAnthropicError(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-messages-fail")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol: channel.ProtocolAnthropic,
		db:       db,
		userID:   user,
		model:    model,
	}
	fake.fail.Store(true)
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, out := postChat(ctx, t, public.URL+"/v1/messages", key, messagesBody(model))
	if status != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502（上游失败是我们与上游之间的问题）。响应体：%s", status, out)
	}

	// 一、错误体是 Anthropic 形状：顶层 type=error，且**不回上游的错误原文**。
	// 错误类型这一栏在 Anthropic 那套里就是放码的位置（它没有 error.code，见
	// writeErrorCode），所以这一条断言的是**码**：上游那一档是
	// upstream_http_error。状态码 502 单独推出来的类型是 api_error，给出码时
	// 以码为准——两个名字指同一档，而有名字的那个才能被 SDK 用来分支。
	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string  `json:"type"`
			Message string  `json:"message"`
			Param   *string `json:"param"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
	}
	if got.Type != "error" {
		t.Errorf("顶层的 type = %q，期望 error", got.Type)
	}
	if got.Error.Type != "upstream_http_error" {
		t.Errorf("错误类型 = %q，期望 upstream_http_error（Anthropic 那套只有这一栏放得下码）", got.Error.Type)
	}
	if got.Error.Message == "" {
		t.Error("错误体没有 message")
	}
	if strings.Contains(string(out), "placeholder upstream failure") {
		t.Errorf("错误体回显了上游的错误原文：%s", out)
	}
	if got.Error.Param != nil || got.Error.Code != nil {
		t.Errorf("错误体里出现了 OpenAI 才有的字段（param=%s code=%s）——形状按调用协议给，这里该是 Anthropic 那份",
			showPtr(got.Error.Param), showPtr(got.Error.Code))
	}

	// 二、钱：冻结额必须回到零，余额一分没动。
	assertWallet(t, db, user, balance, "0")

	// 三、预留确实发生过：假上游被调用时，库里的冻结额大于零。
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

	// 四、账单那一行被标成 released，且金额是预留的那一笔（不是零）。
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
		t.Errorf("预留金额 = %s，期望大于零——等于零说明这次请求根本没冻住钱，上面两条断言就没有意义", reserved)
	}
	if final.Valid && greaterThan(final.String, "0") {
		t.Errorf("final_amount = %s，期望空或零：上游失败不计费", final.String)
	}

	// 五、失败不写分录。
	assertNoLedgerEntry(t, db, user)
}

// 上游**限流**（429）走 Anthropic 入口时，`error.type` 是码 `upstream_http_error`
// ——不是 429 推出来的 `rate_limit_error`。
//
// 为什么单独一条：429 是唯一一个「状态码推出来的类型」与「我们给的码」指向
// **不同处置**的档（前者听起来像「等一下再来」，后者说的是「这条上游不行」），
// 而 Anthropic 那套只有 `error.type` 一个位置放码（见 writeErrorCode 的 `typ := code`）。
// 上面那条 500 的用例证明不了这里：500 推出来的 `api_error` 与码本来就不同名，
// 走哪一支都看不出差别；429 这一条才钉住「给码时以码为准」。
//
// OpenAI 那一侧的对应断言在 internal/api 的单测里（`error.code` 变、
// `error.type` 仍是 `rate_limit_error`），这里只走 Anthropic 这份形状。
func TestMessagesUpstreamRateLimitReturnsTheCode(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-messages-429")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol: channel.ProtocolAnthropic,
		db:       db,
		userID:   user,
		model:    model,
	}
	fake.fail.Store(true)
	fake.failStatus.Store(http.StatusTooManyRequests)
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 状态码原样透出上游的 429：调用方该退避，这个信息对他有用。
	status, out := postChat(ctx, t, public.URL+"/v1/messages", key, messagesBody(model))
	if status != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429（上游限流原样透出）。响应体：%s", status, out)
	}

	var got struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
	}
	if got.Error.Type != "upstream_http_error" {
		t.Errorf("error.type = %q，期望 upstream_http_error——这一栏放的是码，不是 429 推出来的 rate_limit_error", got.Error.Type)
	}
	if got.Error.Message == "" {
		t.Error("错误体没有 message")
	}
}

// 流中途失败时，Anthropic 那条路给出的是**带事件名的错误帧**，且已吐出的
// 部分被计费。
//
// 与非流式的区别是结构性的：状态码与响应头在第一个事件之前就发出去了
// （见 internal/api/stream.go），此后的一切失败只能作为流里的事件表达。
// 而两家的事件形状不同——OpenAI 是一行没有事件名的 data:，Anthropic 是
// `event: error` 加一个 {"type":"error",...} 的载荷（见 streamErrorFrame）。
// 这一条是**唯一**能在端到端层面证明 Anthropic 那份被用上的地方：单元测试
// 直接调渲染器，而这里证明的是「这条链上真的会走到那个渲染器」。
//
// 期望金额与非流式那条同口径：输入 7（「你好」3 + 一条消息的框架 4），
// 输出 3（「半句」），7 × 0.00001 + 3 × 0.00003 = 0.00016。
func TestStreamMessagesUpstreamErrorMidStream(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-stream-messages-midfail")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol:  channel.ProtocolAnthropic,
		model:     model,
		chunks:    []string{"半句"},
		failAfter: 1,
		db:        db,
		userID:    user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/messages", key, anthropicStreamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200（错误发生在流开始之后，改不了状态码了）。响应体：%s", resp.StatusCode, body)
	}

	// 一、帧序列。错误发生在正文块还没关的时候，所以收尾那两帧
	//（content_block_stop / message_delta / message_stop）一个都不该出现——
	// **没有 message_stop 就是截断的信号**（见 internal/api/stream.go）。
	frames := readFrames(t, resp.Body, 0)
	wantEvents := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"error",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("收到 %d 帧，期望 %d 帧：%+v", len(frames), len(wantEvents), frames)
	}
	for i, want := range wantEvents {
		if frames[i].event != want {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, frames[i].event, want)
		}
	}
	if got := dig(t, decodeFrame(t, frames[2]), "delta", "text"); got != "半句" {
		t.Errorf("第 3 帧的正文 = %v，期望已经吐出的那半句", got)
	}

	// 二、错误帧的形状是 Anthropic 的：载荷顶层有 type=error，
	// 事件名是 error（不是一行光秃秃的 data:）。
	errFrame := decodeFrame(t, frames[3])
	if got := dig(t, errFrame, "type"); got != "error" {
		t.Errorf("错误帧的顶层 type = %v，期望 error", got)
	}
	if got := dig(t, errFrame, "error", "type"); got != "api_error" {
		t.Errorf("错误帧的 error.type = %v，期望 api_error", got)
	}
	errMessage, _ := dig(t, errFrame, "error", "message").(string)
	if !strings.Contains(errMessage, "interrupted") {
		t.Errorf("错误帧的措辞 = %q，期望说明回答被截断了（已经吐过字）", errMessage)
	}
	if strings.Contains(frames[3].data, "placeholder upstream failure") {
		t.Errorf("错误帧回显了上游的错误原文：%q", frames[3].data)
	}

	// 三、已经吐出的部分要计费，金额按估算——不是预留的封顶值。
	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00016") {
		t.Errorf("结算金额 = %s，期望 0.00016（7 输入 + 3 输出，按估算）", bill.final.String)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false——这一列是事后分辨两条路的唯一依据")
	}
	assertWallet(t, db, user, "99.99984", "0")
	assertConsumeEntry(t, db, bill.id, user, "0.00016")
}
