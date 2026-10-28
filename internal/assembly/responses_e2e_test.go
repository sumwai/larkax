package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：走 **OpenAI Responses 协议**那条入口（`POST /v1/responses`）的用例。
//
// # 为什么另起一组
//
// 这是第四条公共入口，而它说的协议与另外三条都不同：请求体是
// input/instructions 而不是 messages，输出上界叫 max_output_tokens，响应体是
// output[] 而不是 choices，流式是一串**带事件名**的 SSE 而不是裸 delta。
// 另外三组各自钉着另一套字段名，证明不了这一条——协议解析、请求转换、响应
// 序列化这三段只有在「从 HTTP 到落库」整条链上跑一遍，才谈得上被覆盖。
//
// # 这一组要证明的三件事
//
//  1. 协议**从入口一路走到落库**：请求被解析成管道里的请求、上游按 Responses
//     的形状收到它、响应按 Responses 的形状回给调用方，而账单上
//     `billing_requests.protocol` 落的是 `openai-responses`。最后一条是这次
//     改动的落点——落库层原先只认三个取值，漏掉这一个的表现是「入口收得下
//     请求，却在预留那一步失败」，而单看入口那一层完全看不出来。
//  2. 钱按上游报的用量扣，且与非流式那条 OpenAI 路是同一个口径：协议是
//     调用方与上游之间的事，不该影响计费。
//  3. 两处错误形状的差别真的存在：非流式的错误体是 OpenAI 那一份，而**流式**
//     的错误帧是第三套形状——一个平铺的事件（type / code / message 在顶层，
//     没有 error 外层）。
//
// 约定与另外几组一致：走生产装配路径（assembly.Build）、假上游是本地
// httptest、断言回到库里看钱。请求的发送用的是 e2e_test.go 里的
// postChat/postChatFull，读帧用的是 stream_e2e_test.go 里的 openStream 与
// readFrames——它们本来就与协议无关。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。

// ── 调用方 ────────────────────────────────────────────────────────────

// responsesBody 组装一次 Responses 协议的调用方请求体。
//
// 与 chatBody 的差别是结构性的：输入叫 input（历史与工具结果都在里面），
// 输出上界叫 max_output_tokens。后者在这一套协议里是**可选**的，但预留要有
// 输出上界才做得出来（见 pipeline.Request.validate），所以这里照给。
func responsesBody(model string) []byte { return responsesBodyWith(model, "") }

// responsesBodyWith 与 responsesBody 相同，可另带一段 instructions。
//
// instructions 在 Responses 里是**顶层字段**，而 OpenAI 的 chat 把它当成
// messages 里的一条——这是两套请求体最明显的结构差异，所以「它有没有被原样
// 转给上游」要有判据（见 TestResponsesEndToEnd）。
//
// 其余用例不带它：带上会改变本地估算的输入量，而那些用例断言的是具体金额，
// 多一个与被测行为无关的变量只会让算式难核对。
func responsesBodyWith(model, instructions string) []byte {
	body := map[string]any{
		"model":             model,
		"input":             "你好",
		"max_output_tokens": 64,
	}
	if instructions != "" {
		body["instructions"] = instructions
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// responsesToolBody 是一次**带工具声明**的 Responses 请求。
//
// 声明不能省：调用方没有声明工具时，上游不会产生工具调用，而这条用例要证明的
// 正是「调用方声明的那份工具转成了上游能读的那一份」。声明在这一套协议里是
// **平铺**的（{"type":"function","name":…}），chat 则把它包在 function 里。
func responsesToolBody(model string) []byte {
	var body map[string]any
	if err := json.Unmarshal(responsesBody(model), &body); err != nil {
		panic(err)
	}
	body["tools"] = []any{
		map[string]any{
			"type":        "function",
			"name":        "get_weather",
			"description": "查天气",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
			},
		},
	}
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// responsesStreamBody 与非流式那份的唯一区别是 stream: true。
func responsesStreamBody(model string) []byte {
	var body map[string]any
	if err := json.Unmarshal(responsesBody(model), &body); err != nil {
		panic(err)
	}
	body["stream"] = true
	out, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return out
}

// ── 环境 ──────────────────────────────────────────────────────────────

// responsesRule 是本组用例共用的计价规则。
//
// 三个单价**互不相同**，于是金额能区分出「哪一项算错了」：
//
//	输入  12 × 0.00001 = 0.00012
//	输出   4 × 0.00003 = 0.00012   （上游报的 7 减掉推理 3）
//	推理   3 × 0.00005 = 0.00015
//	                   = 0.00039
//
// reasoning 那一项**必须显式写出来**：用量里有规则未定价的计量项时结算会直接
// 报错，而不是悄悄按 0 算（见 billing.ErrMissingPrice）。这条规矩本身值得留在
// 这里——它是「上游多报了一类用量而我们没定价」的那道闸。
const responsesRule = `{"prompt": 0.00001, "completion": 0.00003, "reasoning": 0.00005}`

// responsesAmount 是上面那条规则下的期望金额，算式见 responsesRule。
const responsesAmount = "0.00039"

// responsesBalance 是本组用例给用户的初始余额。
const responsesBalance = "100"

// responsesBalanceAfter 是扣掉一笔之后的余额。
const responsesBalanceAfter = "99.99961"

// responsesEnv 是一次 Responses 用例搭好的那套环境。
type responsesEnv struct {
	db        *sql.DB
	app       *App
	public    *httptest.Server
	user      int64
	key       string
	channelID int64
	rule      int64
}

// responsesSetup 搭起一次 Responses 用例要的环境：一个商家、一个用户与它的
// key、一条指向假上游的渠道、一条计价规则，以及按生产路径装配的公共入口。
//
// 抽出来是因为这一组有几条用例，而它们的环境逐字相同——只有假上游的行为与
// 计价规则不同。抄几遍的话，改一处（例如给规则加一个计价项）要改好几个地方，
// 而漏掉的那一处不会报错。
//
// 返回的假上游已经指向这个用户与这个模型，用例只需再设它的行为
// （stream / toolCall / fail 那几个字段）——handler 是每次请求时才读它们的。
func responsesSetup(t *testing.T, model, ruleExpr string) (*responsesEnv, *fakeUpstream) {
	t.Helper()

	db := testDB(t)
	sealer := testSealer(t)

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, responsesBalance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAIResponses, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	// 渠道类型写的是适配器的注册名（见 responsesadapter.Type）：它决定我们按
	// 哪套协议跟上游说话，与调用方那一边是哪套无关。
	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "openai-responses", 0)
	rule := seedRule(t, db, model, ruleExpr)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	return &responsesEnv{
		db: db, app: app, public: public, user: user, key: key,
		channelID: channelID, rule: rule,
	}, fake
}

// ── 用例 ──────────────────────────────────────────────────────────────

// 成功路径：一次非流式的 Responses 请求跑通，响应按 Responses 的形状给，
// 钱按上游报的用量扣，而账单上记的是**这条入口**的协议。
func TestResponsesEndToEnd(t *testing.T) {
	model := unique("e2e-model-responses")
	env, fake := responsesSetup(t, model, responsesRule)
	ctx := context.Background()

	status, body, header := postChatFull(ctx, t, env.public.URL+"/v1/responses", env.key,
		responsesBodyWith(model, "你是假上游"))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、响应形状。调用方按 Responses 的 SDK 解这个体，字段名或嵌套层次错了
	// 它读不出来——而它与另外两套的差别是**结构性**的：没有 choices，正文在
	// output[] 的 content 块数组里，用量那两项嵌在 details 里。
	var got struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		CreatedAt int64  `json:"created_at"`
		Status    string `json:"status"`
		Model     string `json:"model"`
		Output    []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Role    string `json:"role"`
			Content []struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Annotations []any  `json:"annotations"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens int64 `json:"input_tokens"`
			// 子字段用指针接：它们是**必填**的（官方 SDK 的 ResponseUsage
			// 没有默认值），所以判据是「键在不在」，而 nil 与 0 在解出来的
			// 结构体里长得一样。relaykit 的转换器正是在缓存量为 0 时把
			// input_tokens_details 整个省掉（见 responses.go 开篇），所以
			// 「值为 0 但键在」本身就是一条判据。
			InputTokensDetails struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails struct {
				ReasoningTokens *int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
		// Choices 是 OpenAI chat 形状的标记。解析进这个结构体不影响上面的
		// 断言，专门用来验证「没有混进另一套形状的字段」。
		Choices []any `json:"choices"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}

	if got.Object != "response" {
		t.Errorf("object = %q，期望 response", got.Object)
	}
	if got.Status != "completed" {
		t.Errorf("status = %q，期望 completed（上游报的 status 是 completed）", got.Status)
	}
	// 报给调用方的模型名是**他要的那个**，不是上游回报的那个：上游名是我们与
	// 上游之间的事（假上游回报的名字带 -upstream 后缀），而调用方按自己请求里
	// 的名字理解响应。
	if got.Model != model {
		t.Errorf("model = %q，期望 %q（报给调用方的应当是他要的那个模型名）", got.Model, model)
	}
	if got.ID == "" {
		t.Error("响应体里的 id 是空的——它是本次请求的关联标识，不能省")
	}
	if want := header.Get("x-request-id"); got.ID != want {
		t.Errorf("响应体的 id = %q，x-request-id 头 = %q：两者必须是同一个值", got.ID, want)
	}
	if got.CreatedAt == 0 {
		t.Error("created_at 是 0——它是这条响应的时间戳，SDK 拿它排序与去重")
	}
	if len(got.Output) != 1 {
		t.Fatalf("output 有 %d 项，期望 1 项（一条助手消息）: %+v", len(got.Output), got.Output)
	}
	if got.Output[0].Type != "message" {
		t.Errorf("output[0].type = %q，期望 message", got.Output[0].Type)
	}
	if got.Output[0].Status != "completed" {
		t.Errorf("output[0].status = %q，期望 completed（与响应体的 status 同一个词）", got.Output[0].Status)
	}
	if got.Output[0].Role != "assistant" {
		t.Errorf("output[0].role = %q，期望 assistant", got.Output[0].Role)
	}
	if len(got.Output[0].Content) != 1 ||
		got.Output[0].Content[0].Type != "output_text" ||
		got.Output[0].Content[0].Text != "你好，我是假上游" {
		t.Errorf("output[0].content = %+v，期望一个带正文的 output_text 块", got.Output[0].Content)
	}
	// annotations 必须是数组：SDK 的模型里它是必填列表，null 会让客户端读到
	// nil 而不是空列表。
	if got.Output[0].Content[0].Annotations == nil {
		t.Error("output[0].content[0].annotations 是 null，期望 []")
	}
	if got.Choices != nil {
		t.Errorf("响应体里混进了 OpenAI chat 形状的字段（choices=%v）——两套协议各是一份独立的结构体", got.Choices)
	}

	// 用量：四个子字段都要在，而且值要是上游报的那一批。
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 7 || got.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v，期望上游报的 12 输入 / 7 输出 / 19 总量", got.Usage)
	}
	if got.Usage.InputTokensDetails.CachedTokens == nil {
		t.Error("usage.input_tokens_details.cached_tokens 不在——官方 SDK 的 ResponseUsage 把它标成必填，" +
			"缺了客户端读到的是一个不存在的属性（relaykit 的转换器正是在缓存量为 0 时省掉它）")
	} else if *got.Usage.InputTokensDetails.CachedTokens != 0 {
		t.Errorf("cached_tokens = %d，期望 0（假上游没报缓存命中）", *got.Usage.InputTokensDetails.CachedTokens)
	}
	if got.Usage.OutputTokensDetails.ReasoningTokens == nil {
		t.Error("usage.output_tokens_details.reasoning_tokens 不在——它是必填字段，" +
			"而且 relaykit 的转换器会把推理量写到 chat 名字的 completion_tokens_details 里")
	} else if *got.Usage.OutputTokensDetails.ReasoningTokens != responsesReasoningTokens {
		t.Errorf("reasoning_tokens = %d，期望 %d（上游报在 output_tokens_details 里的那个数）",
			*got.Usage.OutputTokensDetails.ReasoningTokens, responsesReasoningTokens)
	}

	// 二、钱。算式见 responsesRule：输入 12 × 0.00001 + 输出 4 × 0.00003 +
	// 推理 3 × 0.00005 = 0.00039。
	//
	// 0.00033 说明推理**整个没被读到**（它会落进输出那一档：12 × 0.00001 +
	// 7 × 0.00003），0.00048 说明推理被算了两遍。两种错法都会给出一个形状完全
	// 正常的响应，所以金额要逐位钉住。
	bill, ok := billingOf(t, env.db, env.user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记账")
	}
	if bill.status != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", bill.status)
	}
	if !sameDecimal(bill.final.String, responsesAmount) {
		t.Errorf("结算金额 = %s，期望 %s（12 × 0.00001 + 4 × 0.00003 + 3 × 0.00005）；"+
			"0.00033 说明推理量没被读到，0.00048 说明推理被算了两遍", bill.final.String, responsesAmount)
	}
	assertNotCapped(t, bill)
	if !bill.channelID.Valid || bill.channelID.Int64 != env.channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d（这次请求实际走的渠道）", bill.channelID, env.channelID)
	}
	if bill.streamed {
		t.Error("billing_requests.streamed = true——这次请求没有 stream: true")
	}
	// 账单要能回答「按哪条规则、当时的表达式是什么」：规则事后可变，账单必须
	// 留下算这一笔时用的那一份。
	var (
		ruleID       sql.NullInt64
		exprSnapshot sql.NullString
	)
	if err := env.db.QueryRowContext(ctx,
		`SELECT pricing_rule_id, pricing_expr_snapshot FROM billing_requests WHERE id = $1`,
		bill.id).Scan(&ruleID, &exprSnapshot); err != nil {
		t.Fatalf("查账单的规则快照失败: %v", err)
	}
	if !ruleID.Valid || ruleID.Int64 != env.rule {
		t.Errorf("pricing_rule_id = %v，期望 %d", ruleID, env.rule)
	}
	if !exprSnapshot.Valid || exprSnapshot.String == "" {
		t.Error("pricing_expr_snapshot 为空——规则事后可变，账单必须留下当时的表达式")
	}
	// **这一条是这次改动的落点。** 落库层按一张白名单校验这个值（见
	// internal/ledger/store 的 checkProtocol），而那张白名单原先只认三个取值：
	// 漏掉 `openai-responses` 的表现是「入口收得下请求，却在预留那一步失败」，
	// 每一个 Responses 请求都失败，而错误信息说的是「协议不认识」。
	if bill.protocol != string(channel.ProtocolOpenAIResponses) {
		t.Errorf("账单的 protocol = %q，期望 %q（这次请求走的是 /v1/responses）",
			bill.protocol, channel.ProtocolOpenAIResponses)
	}

	// 三、余额与账本：扣掉实际金额，冻结额回到零，那一笔分录挂回了这次账单。
	assertWallet(t, env.db, env.user, responsesBalanceAfter, "0")
	assertConsumeEntry(t, env.db, bill.id, env.user, responsesAmount)

	// 四、快照链路是接上的，且记在发起这次请求的用户名下。判据是「按这次请求的
	// request_id 查得到行」——request_id 唯一，所以查得到就是这一次写的。
	snapshotUser, ok := waitForSnapshot(t, env.db, bill.requestID)
	if !ok {
		t.Errorf("没有等到请求快照落库（写入计数 %+v）", env.app.Snapshots.Stats())
	} else if snapshotUser != env.user {
		t.Errorf("快照的 user_id = %d，期望 %d（发起这次请求的用户）", snapshotUser, env.user)
	}

	// 五、上游真的收到了我们发出去的那份请求体，而且它是 **Responses 形状**的。
	//
	// 这一条是这条路径上唯一能证明「调用方的协议真的被转成了上游的协议」的地方：
	// 响应体对不对只说明回来的那一半，发出去的那一半错了在响应上完全看不出来。
	// 端点也是协议的一部分——Responses 上游在 /v1/responses 上，发错端点时上游
	// 多半回 404，而调用方看到的是一个含糊的 502。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Model           string `json:"model"`
		Instructions    any    `json:"instructions"`
		MaxOutputTokens int64  `json:"max_output_tokens"`
		// input 按 any 接：同协议直通把调用方原文原样送上游，而 Responses 的
		// input 允许**字符串简写**，于是上游收到的是字符串而不是消息数组。
		// 形状不同是直通的正常结果，但「内容对不对」仍然要判——见下面那段。
		Input  any  `json:"input"`
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if sent.Model != model {
		t.Errorf("上游收到的 model = %q，期望 %q", sent.Model, model)
	}
	if sent.MaxOutputTokens != 64 {
		t.Errorf("上游收到的 max_output_tokens = %d，期望 64（调用方给的上界要原样传到上游，它同时是预留的依据）",
			sent.MaxOutputTokens)
	}
	// instructions 是 Responses 与 chat 在请求体上最明显的结构差异：这边是顶层
	// 字段，那边是 messages 里的一条。转换时丢掉它，模型的行为就变了，而调用方
	// 从响应上完全看不出来。
	if got := textOf(t, sent.Instructions); got != "你是假上游" {
		t.Errorf("上游收到的 instructions = %q，期望 %q（Responses 的 system 是顶层字段，转换时不能丢）",
			got, "你是假上游")
	}
	// input 的**内容**要等于调用方写的那一句话，一处都不能多。
	//
	// 形状（字符串简写 vs 消息数组）不判：直通把调用方原文原样发上游，简写
	// 是最自然的结果。但内容必须还是调用方那一句——instructions 被塞进
	// input、或者 input 被丢掉，都是上游行为变了而调用方在响应上看不出来。
	// 所以这里按内容判，不按形状判。
	if got := responsesInputText(t, sent.Input); got != "你好" {
		t.Errorf("上游收到的 input = %v（取文本 %q），期望只有调用方写的那一句 你好", sent.Input, got)
	}
	if sent.Stream {
		t.Error("上游收到的请求里 stream 是 true——调用方没有要流式")
	}
	if paths := fake.sentPaths(); len(paths) != 1 || paths[0] != "/v1/responses" {
		t.Errorf("上游收到的路径 = %v，期望 [/v1/responses]（Responses 上游的端点）", paths)
	}
}

// responsesInputText 取 Responses 请求里 input 的文本，两种合法形态都认。
//
// 字符串简写（`"input":"你好"`）与消息数组（`[{"role":"user","content":…}]`）
// 都是合法的 Responses 请求：枢纽那条路把它规范化成数组，同协议直通把调用方
// 原文原样发上去，于是这里是简写。形状不是这个用例要判的东西，内容才是，
// 所以两者都在这里收成一个字符串。
func responsesInputText(t *testing.T, v any) string {
	t.Helper()

	switch in := v.(type) {
	case string:
		return in
	case []any:
		var out strings.Builder
		for _, item := range in {
			msg, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("input 的每一项是 %T，期望一个消息对象", item)
			}
			out.WriteString(textOf(t, msg["content"]))
		}
		return out.String()
	default:
		t.Fatalf("input 是 %T，期望字符串简写或消息数组", v)
		return ""
	}
}

// 流式成功路径：事件名与顺序按 Responses 的流式形状给，每个事件的
// sequence_number 从 0 起递增，钱按上游在**终态事件**里报的用量扣。
//
// 与非流式那条的区别不只是「逐帧发」：Responses 的用量只跟着终态事件来
// （chat 那边要显式带 stream_options.include_usage），所以这次结算的依据只能
// 从那一帧里拿到——上游报了而链路没把它读出来时，表现是「按估算扣钱」，
// 而帧序列看上去完全正常。
func TestResponsesStreamEndToEnd(t *testing.T) {
	model := unique("e2e-stream-responses")
	env, fake := responsesSetup(t, model, responsesRule)
	fake.stream = true
	fake.streamChunks = []string{"你好", "，我是假上游"}

	resp, cancel := openStream(t, env.public.URL+"/v1/responses", env.key, responsesStreamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}

	// 一、事件序列。正文项的 added / delta / done 是三个事件，收尾两帧是
	// output_item.done 与终态——顺序反了或少了 done，客户端的累加器拼出来的
	// 东西就不对，而它不会报错。
	frames := readFrames(t, resp.Body, 0)
	wantEvents := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("收到 %d 帧，期望 %d 帧：%+v", len(frames), len(wantEvents), frames)
	}
	for i, want := range wantEvents {
		if frames[i].event != want {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, frames[i].event, want)
		}
	}

	// 二、sequence_number 从 0 起递增，一个不漏。官方 SDK 的事件模型把它标成
	// 必填，客户端按它排序；漏掉一个或重复一个都不会让报文看起来不对。
	for i, f := range frames {
		if got := decodeFrame(t, f)["sequence_number"]; got != float64(i) {
			t.Errorf("第 %d 帧的 sequence_number = %v，期望 %d", i, got, i)
		}
	}

	// 三、正文按帧拼得回来，且终态事件里的那一项就是拼出来的结果。
	if got := dig(t, decodeFrame(t, frames[2]), "delta"); got != "你好" {
		t.Errorf("第 2 帧的正文增量 = %v，期望 你好", got)
	}
	if got := dig(t, decodeFrame(t, frames[3]), "delta"); got != "，我是假上游" {
		t.Errorf("第 3 帧的正文增量 = %v，期望 ，我是假上游", got)
	}
	if got := dig(t, decodeFrame(t, frames[4]), "text"); got != "你好，我是假上游" {
		t.Errorf("output_text.done 的 text = %v，期望拼起来的全文", got)
	}
	// created 与终态报的是同一个 id 与同一个模型名：客户端拿 created 那一帧
	// 建自己的状态，两处不一致时它关联不上后面的事件。
	created := decodeFrame(t, frames[0])
	done := decodeFrame(t, frames[6])
	if got := dig(t, created, "response", "model"); got != model {
		t.Errorf("response.created 的 model = %v，期望 %q（调用方要的那个名字）", got, model)
	}
	if got := dig(t, done, "response", "status"); got != "completed" {
		t.Errorf("终态事件的 status = %v，期望 completed", got)
	}
	if got := dig(t, done, "response", "id"); got != dig(t, created, "response", "id") {
		t.Errorf("终态事件的 id = %v，与 created 的 %v 不同：同一条流的两个事件必须报同一个 id",
			dig(t, done, "response", "id"), dig(t, created, "response", "id"))
	}
	if got := dig(t, done, "response", "output", "0", "content", "0", "text"); got != "你好，我是假上游" {
		t.Errorf("终态事件里正文项的内容 = %v，期望拼起来的全文", got)
	}
	// 用量在终态事件里（这一条就是「实测用量真的读到了」的判据）。
	if got := dig(t, done, "response", "usage", "output_tokens"); got != float64(7) {
		t.Errorf("终态事件的 usage.output_tokens = %v，期望上游报的 7", got)
	}
	if got := dig(t, done, "response", "usage", "output_tokens_details", "reasoning_tokens"); got != float64(responsesReasoningTokens) {
		t.Errorf("终态事件的 usage.output_tokens_details.reasoning_tokens = %v，期望 %d",
			got, responsesReasoningTokens)
	}

	// 四、钱：与非流式那条路同一个数（算式见 responsesRule）。
	bill := waitForBillingStatus(t, env.db, env.user, "settled")
	if !sameDecimal(bill.final.String, responsesAmount) {
		t.Errorf("结算金额 = %s，期望 %s（12 × 0.00001 + 4 × 0.00003 + 3 × 0.00005）",
			bill.final.String, responsesAmount)
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != env.channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, env.channelID)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false——这一列是事后分辨两条路的唯一依据")
	}
	if bill.protocol != string(channel.ProtocolOpenAIResponses) {
		t.Errorf("账单的 protocol = %q，期望 %q", bill.protocol, channel.ProtocolOpenAIResponses)
	}
	assertWallet(t, env.db, env.user, responsesBalanceAfter, "0")
	assertConsumeEntry(t, env.db, bill.id, env.user, responsesAmount)

	// 五、发出去的那份请求真的是流式的。
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

// 非流式的工具调用：上游回一次 function_call，调用方在 output[] 里拿到它。
//
// 与 chat 的 tool_calls 有两处形状差异，都是契约的一部分：参数叫 arguments，
// 关联 id 叫 call_id（项自己的 id 是另一个东西）。而这两处只有走到调用方那
// 一端才看得见——适配器与渲染器各自有单测，但它们之间那条缝没有。
func TestResponsesToolCallEndToEnd(t *testing.T) {
	const (
		callID = "call_placeholder"
		args   = `{"city":"北京"}`
	)
	model := unique("e2e-model-responses-tool")
	env, fake := responsesSetup(t, model, responsesRule)
	fake.toolCall = &fakeToolCall{CallID: callID, Name: "get_weather", Arguments: args}

	status, body := postChat(context.Background(), t, env.public.URL+"/v1/responses", env.key,
		responsesToolBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var got struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Status    string `json:"status"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	// 这一次回答里没有正文，所以 output[] 里**只有**那一项工具调用：混进一个
	// 空的正文项会让客户端渲染出一段空白，而它看着像是模型答了个空串。
	if len(got.Output) != 1 {
		t.Fatalf("output 有 %d 项，期望 1 项（一次工具调用）: %+v", len(got.Output), got.Output)
	}
	item := got.Output[0]
	if item.Type != "function_call" {
		t.Errorf("output[0].type = %q，期望 function_call", item.Type)
	}
	if item.CallID != callID {
		t.Errorf("output[0].call_id = %q，期望 %q（客户端拿它回传工具结果，缺了这次往返就断了）", item.CallID, callID)
	}
	if item.Name != "get_weather" {
		t.Errorf("output[0].name = %q，期望 get_weather", item.Name)
	}
	if item.Arguments != args {
		t.Errorf("output[0].arguments = %q，期望 %q（参数是**字符串**形态的 JSON）", item.Arguments, args)
	}
	if item.ID == "" || item.ID == item.CallID {
		t.Errorf("output[0].id = %q，期望一个非空且与 call_id 不同的值（项自己的 id 与关联 id 不是一个东西）", item.ID)
	}
	if item.Status != "completed" {
		t.Errorf("output[0].status = %q，期望 completed", item.Status)
	}

	// 上游真的收到了调用方声明的工具，而且是**平铺**的那一份声明（chat 会把它
	// 包在 function 里）。没有这一条，假上游吐的工具调用就成了「一次普通请求
	// 碰巧收到了工具分片」。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if len(sent.Tools) != 1 || sent.Tools[0].Type != "function" || sent.Tools[0].Name != "get_weather" {
		t.Errorf("上游收到的工具声明 = %+v，期望一个平铺的 {type:function, name:get_weather}", sent.Tools)
	}

	// 钱与不带工具那一条同一个数：工具调用不改变用量口径。
	bill, ok := billingOf(t, env.db, env.user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记账")
	}
	if bill.status != "settled" || !sameDecimal(bill.final.String, responsesAmount) {
		t.Errorf("账单 = %q / %s，期望 settled / %s", bill.status, bill.final.String, responsesAmount)
	}
	assertWallet(t, env.db, env.user, responsesBalanceAfter, "0")
}

// 流式的工具调用：参数分片逐帧给，调用方要能拼回完整的 JSON。
//
// 上游先把项建好（名字与 call_id 在那一条里给），参数靠后面的分片拼出来——
// 与 Anthropic 的 content_block_start + input_json_delta 同一个形状。**分两片**
// 而不是一片给全：「分片有没有落回自己那一项」这件事只有分片时才测得到，
// 而它就是客户端按 item_id 累加的那个判据。
func TestResponsesStreamToolCallEndToEnd(t *testing.T) {
	const (
		callID = "call_placeholder"
		args   = `{"city":"北京"}`
	)
	model := unique("e2e-stream-responses-tool")
	env, fake := responsesSetup(t, model, responsesRule)
	fake.stream = true
	fake.toolCall = &fakeToolCall{CallID: callID, Name: "get_weather", Arguments: args}
	fake.streamToolArgs = []string{`{"city":`, `"北京"}`}

	resp, cancel := openStream(t, env.public.URL+"/v1/responses", env.key, responsesStreamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, body)
	}

	frames := readFrames(t, resp.Body, 0)
	wantEvents := []string{
		"response.created",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("收到 %d 帧，期望 %d 帧：%+v", len(frames), len(wantEvents), frames)
	}
	for i, want := range wantEvents {
		if frames[i].event != want {
			t.Errorf("第 %d 帧的事件名 = %q，期望 %q", i, frames[i].event, want)
		}
	}

	// 一、项在 added 那一帧里就把名字与 call_id 给了——客户端要靠它建自己的
	// 那一项，而参数要到后面的分片里才来。
	added := decodeFrame(t, frames[1])
	if got := dig(t, added, "item", "type"); got != "function_call" {
		t.Errorf("output_item.added 的 item.type = %v，期望 function_call", got)
	}
	if got := dig(t, added, "item", "call_id"); got != callID {
		t.Errorf("output_item.added 的 call_id = %v，期望 %q", got, callID)
	}
	if got := dig(t, added, "item", "name"); got != "get_weather" {
		t.Errorf("output_item.added 的 name = %v，期望 get_weather", got)
	}

	// 二、分片拼得回来，且两片落在**同一个** item_id 与同一个 output_index 上。
	// 客户端就是按这两个键把增量归回那一项的。
	first, second := decodeFrame(t, frames[2]), decodeFrame(t, frames[3])
	partial := strOf(dig(t, first, "delta")) + strOf(dig(t, second, "delta"))
	if partial != args {
		t.Errorf("两片参数拼出来 = %q，期望 %q", partial, args)
	}
	itemID := strOf(dig(t, first, "item_id"))
	if itemID == "" {
		t.Error("参数增量里没有 item_id——客户端无从知道这段参数属于哪一项")
	}
	if strOf(dig(t, second, "item_id")) != itemID {
		t.Errorf("两片的 item_id 不同（%q / %q）：同一个工具调用的分片必须落在同一项上",
			itemID, strOf(dig(t, second, "item_id")))
	}
	if first["output_index"] != second["output_index"] {
		t.Errorf("两片的 output_index 不同（%v / %v）", first["output_index"], second["output_index"])
	}

	// 三、done 与终态事件给的是**完整**参数，而且与拼出来的那一段相同。
	argDone := decodeFrame(t, frames[4])
	if got := dig(t, argDone, "arguments"); got != args {
		t.Errorf("function_call_arguments.done 的 arguments = %v，期望 %q", got, args)
	}
	if strOf(dig(t, argDone, "item_id")) != itemID {
		t.Errorf("done 的 item_id = %v，与增量的 %q 不同", dig(t, argDone, "item_id"), itemID)
	}
	item := decodeFrame(t, frames[5])
	if got := dig(t, item, "item", "call_id"); got != callID {
		t.Errorf("output_item.done 的 call_id = %v，期望 %q", got, callID)
	}
	if got := dig(t, item, "item", "arguments"); got != args {
		t.Errorf("output_item.done 的 arguments = %v，期望 %q（拼起来的完整参数）", got, args)
	}
	final := decodeFrame(t, frames[6])
	if got := dig(t, final, "response", "output", "0", "arguments"); got != args {
		t.Errorf("终态事件里那一项的 arguments = %v，期望 %q", got, args)
	}

	// 四、钱：与非流式那条路同一个数。
	bill := waitForBillingStatus(t, env.db, env.user, "settled")
	if !sameDecimal(bill.final.String, responsesAmount) {
		t.Errorf("结算金额 = %s，期望 %s", bill.final.String, responsesAmount)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false")
	}
	assertWallet(t, env.db, env.user, responsesBalanceAfter, "0")
}

// 上游失败时，调用方拿到的是 **OpenAI 形状**的错误体（Responses 的非流式错误
// 体与它是同一份，两家共用同一个官方 SDK 的错误解析器），且这次请求不计费。
//
// 与非流式那条 OpenAI 的失败用例（TestUpstreamFailureReleasesReservation）是
// 同一件事，只是换了一条入口。之所以值得单独跑一遍：错误体是按**调用协议**
// 渲染的（见 internal/api/error.go），而这条入口在三套形状里选哪一套，只有
// 走一遍才知道。
func TestResponsesUpstreamFailureReturnsOpenAIError(t *testing.T) {
	model := unique("e2e-model-responses-fail")
	env, fake := responsesSetup(t, model, responsesRule)
	fake.fail.Store(true)

	status, out := postChat(context.Background(), t, env.public.URL+"/v1/responses", env.key,
		responsesBody(model))
	if status != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502（上游失败是我们与上游之间的问题）。响应体：%s", status, out)
	}

	// 一、错误体是 OpenAI 那一份：{"error":{message,type,param,code}}，四个键
	// 都在。判据是**键在不在**，所以拿原始 map 看而不是解出来的结构体——那里
	// nil 与「键不在」长得一样。
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
	}
	errObj, ok := raw["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应体里没有 error 对象：%s", out)
	}
	for _, key := range []string{"message", "type", "param", "code"} {
		if _, present := errObj[key]; !present {
			t.Errorf("错误体里缺 %q 这个键：%s", key, out)
		}
	}
	// 502 落到 errorKind 的默认分支，两套协议都是 api_error。
	if got := errObj["type"]; got != "api_error" {
		t.Errorf("error.type = %v，期望 api_error", got)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Error("错误体没有 message")
	}
	// 不回上游的错误原文：那里面可能回显请求内容（与非流式的 writeError 同
	// 一条规矩）。
	if strings.Contains(string(out), "placeholder upstream failure") {
		t.Errorf("错误体回显了上游的错误原文：%s", out)
	}

	// 二、钱：冻结额必须回到零，余额一分没动。
	assertWallet(t, env.db, env.user, responsesBalance, "0")

	// 三、预留确实发生过：假上游被调用时，库里的冻结额大于零。没有这一条，
	// 上面那条断言在一个「预留根本没落库」的实现上也会通过。
	observed := fake.observedFrozen()
	if len(observed) == 0 {
		t.Fatal("假上游一次都没被调用，这次失败不是上游造成的")
	}
	for i, s := range observed {
		if !strings.Contains(s, "冻结=true") {
			t.Errorf("第 %d 次调用上游时库里的冻结额是 %s——预留必须先冻住再调上游", i+1, s)
		}
	}

	// 四、账单那一行被标成 released，且金额是预留的那一笔（不是零）；失败不写
	// 分录。
	bill, ok := billingOf(t, env.db, env.user)
	if !ok {
		t.Fatal("没有账单行——上游失败也要在账上留下依据")
	}
	if bill.status != "released" {
		t.Errorf("账单状态 = %q，期望 released", bill.status)
	}
	if !greaterThan(bill.reserved, "0") {
		t.Errorf("预留金额 = %s，期望大于零——等于零说明这次请求根本没冻住钱", bill.reserved)
	}
	if bill.final.Valid && greaterThan(bill.final.String, "0") {
		t.Errorf("final_amount = %s，期望空或零：上游失败不计费", bill.final.String)
	}
	assertNoLedgerEntry(t, env.db, env.user)
}

// 流**中途**失败时，调用方拿到的是一个**平铺**的 error 事件——type / code /
// message 在顶层，没有另外两套那样的 error 外层。
//
// 这是第三套形状，不能与前两套合并（见 internal/api/stream.go 的
// responsesStream.errorFrame），而这一条是端到端层面唯一能证明「这条链上真的
// 会走到那个渲染器」的地方：单元测试直接调渲染器。
//
// 失败必须发生在**吐字之后**：响应头在第一个事件之前就发出去了，此后再想回
// 502 只能靠一个已经发出的 200 去解释，所以流开始之后的失败只能是流里的一个
// 事件。反过来（失败在流开始之前）走的是普通错误响应那条路，与上面那条用例
// 同一件事。
func TestResponsesStreamMidFailureIsFlatErrorEvent(t *testing.T) {
	model := unique("e2e-stream-responses-midfail")
	env, fake := responsesSetup(t, model, responsesRule)
	fake.stream = true
	fake.streamChunks = []string{"半句"}
	// 吐满三帧（created / output_item.added / 正文增量）之后报错。
	fake.failAfterStreamEvents = 3

	resp, cancel := openStream(t, env.public.URL+"/v1/responses", env.key, responsesStreamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200（错误发生在流开始之后，改不了状态码了）。响应体：%s", resp.StatusCode, body)
	}

	frames := readFrames(t, resp.Body, 0)
	wantEvents := []string{
		"response.created",
		"response.output_item.added",
		"response.output_text.delta",
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
	// 已经吐出去的那半句照样到了调用方手里，然后才是错误帧。
	if got := dig(t, decodeFrame(t, frames[2]), "delta"); got != "半句" {
		t.Errorf("第 2 帧的正文增量 = %v，期望已经吐出的那半句", got)
	}

	// 一、错误帧的形状：平铺，且**没有** error 外层、没有 response 外层。
	// 帧的载荷本身就是那个错误对象，所以这几个键直接在顶层。
	errFrame := decodeFrame(t, frames[3])
	if _, nested := errFrame["error"]; nested {
		t.Errorf("错误帧里出现了 error 外层：%v（Responses 的流式错误是平铺的）", errFrame)
	}
	if got := dig(t, errFrame, "type"); got != "error" {
		t.Errorf("错误帧的 type = %v，期望 error", got)
	}
	if got := dig(t, errFrame, "code"); got != "server_error" {
		t.Errorf("错误帧的 code = %v，期望 server_error", got)
	}
	errMessage := strOf(dig(t, errFrame, "message"))
	if !strings.Contains(errMessage, "interrupted") {
		t.Errorf("错误帧的措辞 = %q，期望说明回答被截断了（已经吐过字）——"+
			"说成「服务暂时不可用」会让客户端直接重试，而重试会把两段回答接在一起", errMessage)
	}
	if strings.Contains(frames[3].data, "placeholder upstream failure") {
		t.Errorf("错误帧回显了上游的错误原文：%q", frames[3].data)
	}
	// 一个 sequence_number 也要在，且接着前面的号往下走：客户端按它排序，
	// 少了它这一帧会被当成乱序。
	if got := errFrame["sequence_number"]; got != float64(3) {
		t.Errorf("错误帧的 sequence_number = %v，期望 3", got)
	}

	// 二、**没有** response.completed：伪造一个正常结束会让调用方把半截回答
	// 当成完整回答用（见 internal/api/stream.go 的「结束标记不能伪造」）。
	for _, f := range frames {
		if f.event == "response.completed" {
			t.Fatal("上游中途报错，却给了调用方一个 response.completed")
		}
	}

	// 三、已经吐出的部分要计费（按估算），预留不留在地上。金额不在这里钉：
	// 那是估算器的口径，与非流式那条路同一处（见 stream_e2e_test.go）。
	bill := waitForBillingStatus(t, env.db, env.user, "settled")
	if !bill.streamed {
		t.Error("billing_requests.streamed = false——这一列是事后分辨两条路的唯一依据")
	}
	if bill.protocol != string(channel.ProtocolOpenAIResponses) {
		t.Errorf("账单的 protocol = %q，期望 %q", bill.protocol, channel.ProtocolOpenAIResponses)
	}
}
