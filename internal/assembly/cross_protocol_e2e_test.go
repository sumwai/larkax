package assembly

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：**调用协议 ≠ 上游协议**的那两种组合。
//
// # 为什么另起一组
//
// 既有用例各自只覆盖了「调用协议 = 上游协议」那一半：
//
//	e2e_test.go           /v1/chat/completions + openai 渠道（非流式）
//	messages_e2e_test.go  /v1/messages         + anthropic 渠道（非流式，另有一条流式）
//	stream_e2e_test.go    两条入口各自配同协议的渠道（流式）
//
// 剩下两种组合走的是**协议转换**那条路，而这条路上最容易错的不是字段名，
// 是**用量口径**：
//
//	Anthropic 的 input_tokens **不含**缓存，OpenAI 的 prompt_tokens **含**。
//
// 同协议那些用例里，上游报什么就记什么，口径差不会被碰到；跨协议时这个差
// 必须在一侧补上、在另一侧拆掉，而**拆错了的表现是账单金额对不上，响应形状
// 看起来完全正常**。所以下面每一条用例的断言都落到**具体金额**上，而不只是
// 形状上——形状对而钱错，是这套系统里最贵的那类故障。
//
// # 五条用例的矩阵
//
//	调用协议             上游渠道    上游报的缓存  上游说的结束原因  调用方该看到
//	/v1/messages         openai      无            stop              end_turn
//	/v1/messages         openai      读 5          length            max_tokens
//	/v1/chat/completions anthropic   无            end_turn          stop
//	/v1/chat/completions anthropic   读 5          max_tokens        length
//	/v1/chat/completions anthropic   读 5 + 写 3   end_turn          stop
//
// 结束原因那一列覆盖了四个值：前四条各走一个映射，把两家词汇的四个映射都走了一遍
// e2e；第五条与第三条同值（`end_turn` → `stop`），它加的是缓存写入那一侧。
// 其中 `length` 与 `max_tokens` 是**非默认**的：拿各协议最常见的那一个去测，
// 一个把结束原因写死成默认值的实现照样能过（见 fakeUpstream 的 finishReason）。
//
// 假上游与既有几组共用同一个 fakeUpstream：渠道声明的类型决定我们往上发哪套
// 协议，而「调用协议 ≠ 上游协议」正是这一组要测的东西。另起一个假上游早晚会
// 与这一个分叉，而分叉的那一份不会报错。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。

// chatBodyWithSystem 组装一次 OpenAI 协议的请求体，messages 里带一条 system。
//
// OpenAI 把 system 当成 messages 里的一条，Anthropic 把它当顶层字段——这是
// 两套请求体最明显的结构差异，所以「它有没有被搬到上游那一侧的正确位置」
// 要有判据。其余用例不带它：带上会改变本地估算的输入量，而那些用例断言的是
// 具体金额，多一个与被测行为无关的变量只会让算式难核对。
func chatBodyWithSystem(model, system string) []byte {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": "你好"},
		},
		"max_tokens": 64,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// textOf 取出一个正文字段的文本，字段不在时返回空串。
//
// 两家都可能把它写成两种形态：一个字符串，或者一组正文块。OpenAI 的
// `message.content` 与 Anthropic 的顶层 `system` 都允许——所以这一个函数
// 两处都用。断言因此落在**文本**上而不是形态上：形态是编码选择，把它写进
// 断言会让一次等价的编码改动变成假失败，而真正要防的是「那段文本根本没传过去」。
//
// 字段不在（nil）返回空串而不是当场失败：调用方拿它比对的都是非空字面量，
// 空串一样会让断言挂，而挂的时候报出来的「这里是空的」比一句类型抱怨更贴题。
func textOf(t *testing.T, v any) string {
	t.Helper()

	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case []any:
		var out strings.Builder
		for _, item := range s {
			block, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("正文块是 %T，期望一个对象", item)
			}
			text, _ := block["text"].(string)
			out.WriteString(text)
		}
		return out.String()
	default:
		t.Fatalf("正文是 %T，期望字符串或正文块数组", v)
		return ""
	}
}

// showPtr 把一个可空字符串渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个 *string 打出来的是地址（`stop_reason = 0xc000...`），而报错
// 信息本身是要给人读的——那串地址看不出上游到底报了什么，也就看不出这次
// 失败是「转换错了」还是「字段根本没传过来」。
func showPtr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// ── /v1/messages → openai 渠道 ────────────────────────────────────────

// 成功路径：调用方说 Anthropic，上游说 OpenAI，两边都拿到自己那套形状。
//
// 期望金额与同协议那两条路是同一个数（12 × 0.00001 + 7 × 0.00003 = 0.00033）：
// 协议是**调用方与上游之间**的事，不该影响计费口径。金额一致本身就是判据。
func TestMessagesWithOpenAIUpstreamEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-model-messages-to-openai")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "openai", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t.Context(), t, public.URL+"/v1/messages", key, messagesBodyWith(model, "你是假上游"))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、响应按**调用协议**给。调用方按 Anthropic 的 SDK 解体，字段名或嵌套
	// 层次错了它读不出来——而上游说的是另一套，这中间隔着一次转换。
	var got struct {
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
		// OpenAI 形状的标记字段，用来验证没有混进另一套。
		Object  *string `json:"object"`
		Choices []any   `json:"choices"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}

	if got.Type != "message" {
		t.Errorf("type = %q，期望 message（Anthropic 的响应标记）", got.Type)
	}
	if got.Model != model {
		t.Errorf("model = %q，期望 %q（报给调用方的是他要的那个名字，不是上游回报的）", got.Model, model)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "你好，我是假上游" {
		t.Errorf("content = %+v，期望一个带正文的 text 块", got.Content)
	}
	// 上游说的是 OpenAI 的 stop，调用方要看到 Anthropic 的 end_turn。
	if got.StopReason == nil || *got.StopReason != "end_turn" {
		t.Errorf("stop_reason = %s，期望 end_turn（上游报的是 OpenAI 词汇里的 stop）", showPtr(got.StopReason))
	}
	if got.Usage.InputTokens != 12 || got.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v，期望上游报的 12/7", got.Usage)
	}
	if got.Object != nil || got.Choices != nil {
		t.Errorf("响应体里混进了 OpenAI 形状的字段（object=%s choices=%v）——调用方说的是 Anthropic，它解不出这套",
			showPtr(got.Object), got.Choices)
	}

	// 二、钱：12 × 0.00001 + 7 × 0.00003 = 0.00033000。
	bill, ok := billingOf(t, db, user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记")
	}
	if bill.status != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", bill.status)
	}
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003）", bill.final.String)
	}
	assertNotCapped(t, bill)
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d（这次请求实际走的那个 openai 渠道）", bill.channelID, channelID)
	}
	// 账单记的是**调用方**说的那套协议，不是上游那套。跨协议时两者不同，
	// 这一列是事后按协议归因时的唯一依据——记成上游那一侧的话，
	// 「Anthropic 入口的用量口径」这类统计会把两种组合混在一起。
	if bill.protocol != string(channel.ProtocolAnthropic) {
		t.Errorf("账单的 protocol = %q，期望 %q（记的是调用方说的那套，不是上游的 openai）",
			bill.protocol, channel.ProtocolAnthropic)
	}
	assertWallet(t, db, user, "99.99967", "0")

	// 三、上游真的收到了我们发出去的那份请求体，而且它是 **OpenAI 形状**的。
	//
	// 这一条是这条路径上唯一能证明「调用方的协议真的被转成了上游的协议」的
	// 地方：响应体对不对只说明回来的那一半，发出去的那一半错了（比如按
	// Anthropic 的形状发给了 OpenAI 上游）在响应上完全看不出来。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if sent.Model != model {
		t.Errorf("上游收到的 model = %q，期望 %q", sent.Model, model)
	}
	if sent.MaxTokens != 64 {
		t.Errorf("上游收到的 max_tokens = %d，期望 64（调用方给的上界要原样传到上游，它同时是预留的依据）",
			sent.MaxTokens)
	}
	// system 的位置是两套请求体最明显的结构差异：Anthropic 那边是顶层字段，
	// OpenAI 这边是 messages 里的**第一条**。搬错了地方，模型的行为就变了，
	// 而调用方从响应上完全看不出来。
	if len(sent.Messages) != 2 {
		t.Fatalf("上游收到的 messages 有 %d 条，期望 2 条（system + 用户消息）：%+v",
			len(sent.Messages), sent.Messages)
	}
	if sent.Messages[0].Role != "system" || textOf(t, sent.Messages[0].Content) != "你是假上游" {
		t.Errorf("上游收到的第一条消息 = %+v，期望 Anthropic 的顶层 system 被搬成 OpenAI 的第一条 system 消息",
			sent.Messages[0])
	}
	if sent.Messages[1].Role != "user" || textOf(t, sent.Messages[1].Content) != "你好" {
		t.Errorf("上游收到的第二条消息 = %+v，期望那条用户消息", sent.Messages[1])
	}
	// 顶层不该再有 system：那是 Anthropic 的形状，留着它等于把两套混着发。
	var raw map[string]any
	if err := json.Unmarshal(bodies[0], &raw); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if _, ok := raw["system"]; ok {
		t.Errorf("上游收到的请求体里有顶层的 system 字段——OpenAI 那边 system 是 messages 里的一条。请求体：%s", bodies[0])
	}
	// 端点也是协议的一部分：OpenAI 上游在 /v1/chat/completions 上。发错端点时
	// 上游多半回 404，而从我们的响应上只是一个含糊的 502。
	if paths := fake.sentPaths(); len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Errorf("上游收到的路径 = %v，期望 [/v1/chat/completions]（OpenAI 上游的端点）", paths)
	}
}

// 上游报了缓存命中时，用量要按**两套口径各自的定义**转换。
//
// 这是跨协议转换里最容易错、也最贵的一处：OpenAI 的 prompt_tokens **含**缓存
// （上游报 17），Anthropic 的 input_tokens **不含**（调用方要看到 12），而缓存
// 那 5 个 token 另报一项。链上经过三次转换，每一步都要有数字钉住：
//
//	上游报的     prompt_tokens = 17（含缓存）、cached_tokens = 5、completion = 7
//	内部口径     prompt = 17、cache_read = 5（见 relay.UsageFromRelaykit）
//	回给调用方   input_tokens = 17 − 5 = 12（见 api.claudeUsageOf）
//	计费口径     prompt = 17 − 5 = 12、cache_read = 5（缓存单独定价，不能重复计）
//	金额         12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002 = 0.00034
//
// 缓存那一项的单价特意取**非零**：取 0 的话，上面每一处漏算缓存都会得到同一个
// 数（0.00033），断言就分不出「算对了」与「缓存根本没参与」。
//
// 0.00034 与三种错法都能分开（三个数都实测过，见 report-210.md）：
//
//	0.00033  缓存没参与计价：输入按 12 算，缓存那一档是 0
//	0.00038  缓存没从 prompt 里拆出去：那 5 个 token 只按输入单价收了一遍。
//	         拆漏时单列的缓存费还会被 billing.go 的钳位压回 0（三项之和超过
//	         PromptTokens 时它先把缓存那两项压回去），于是「拆漏了」与
//	         「压根没认出来」落到同一个数
//	0.00039  那 5 个 token 被收了两遍：既留在 prompt 那一档里，又按 cache_read 单列
func TestMessagesWithOpenAIUpstreamCacheUsage(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-model-messages-to-openai-cache")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol:  channel.ProtocolOpenAI,
		cacheRead: 5,
		// 非默认的结束原因：见文件顶部那张矩阵。
		finishReason: "length",
		db:           db,
		userID:       user,
		model:        model,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "openai", 0)
	// cache_read 这一项必须**显式写出来**：规则里没写它时结算会直接报错
	//（「用量里有规则未定价的计量项」），而不是悄悄按 0 算。这条规矩本身
	// 值得留在这里——它是「上游多报了一类用量而我们没定价」的那道闸，
	// 而这类差错的表现是**少收钱**。
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003, "cache_read": 0.000002}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t.Context(), t, public.URL+"/v1/messages", key, messagesBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var got struct {
		StopReason *string `json:"stop_reason"`
		Usage      struct {
			InputTokens          int64 `json:"input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}

	// 一、回给调用方的输入量必须**减掉**缓存。填 17 就是把 OpenAI 的口径
	// 原样交给了说 Anthropic 的调用方——他按自己的定义加总时会多出 5 个 token。
	if got.Usage.InputTokens != 12 {
		t.Errorf("input_tokens = %d，期望 12（上游的 17 减掉 5 个缓存）；17 说明缓存没被拆出去",
			got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 5 {
		t.Errorf("cache_read_input_tokens = %d，期望 5（上游报了它就该原样出现在响应里，它是 Anthropic 口径里的一项）",
			got.Usage.CacheReadInputTokens)
	}
	if got.Usage.OutputTokens != 7 {
		t.Errorf("output_tokens = %d，期望 7", got.Usage.OutputTokens)
	}
	if got.StopReason == nil || *got.StopReason != "max_tokens" {
		t.Errorf("stop_reason = %s，期望 max_tokens（上游报的是 OpenAI 词汇里的 length）", showPtr(got.StopReason))
	}

	// 二、钱。缓存那 5 个 token 按 cache_read 的单价单独算，**不能**再算进
	// prompt 那一档——所以最终数不是「17 个输入 × prompt 单价」。
	bill, ok := billingOf(t, db, user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记")
	}
	if bill.status != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", bill.status)
	}
	if !sameDecimal(bill.final.String, "0.00034") {
		t.Errorf("结算金额 = %s，期望 0.00034（12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002）；"+
			"0.00033 说明缓存没参与计价，0.00038 说明缓存没被拆出去（只按输入单价收了一遍），"+
			"0.00039 说明那 5 个 token 被收了两遍", bill.final.String)
	}
	assertNotCapped(t, bill)
	assertWallet(t, db, user, "99.99966", "0")
}

// ── /v1/chat/completions → anthropic 渠道 ─────────────────────────────

// 成功路径：调用方说 OpenAI，上游说 Anthropic。
//
// 与上一条对称，只是转换的方向反过来了：这次是**我们**把 system 搬到上游的
// 顶层字段，把上游的结束原因翻成 OpenAI 的词汇。
func TestChatCompletionsWithAnthropicUpstreamEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-model-chat-to-anthropic")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolAnthropic, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t.Context(), t, public.URL+"/v1/chat/completions", key, chatBodyWithSystem(model, "你是假上游"))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、响应按**调用协议**（OpenAI）给。
	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		// Anthropic 形状的标记字段，用来验证没有混进另一套。
		Type       *string `json:"type"`
		StopReason *string `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}

	if got.Object != "chat.completion" {
		t.Errorf("object = %q，期望 chat.completion", got.Object)
	}
	if got.Model != model {
		t.Errorf("model = %q，期望 %q", got.Model, model)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices 有 %d 条，期望 1 条：%+v", len(got.Choices), got.Choices)
	}
	if got.Choices[0].Message.Content == nil || *got.Choices[0].Message.Content != "你好，我是假上游" {
		t.Errorf("choices[0].message = %+v，期望一个带正文的回复", got.Choices[0].Message)
	}
	// 上游说的是 Anthropic 的 end_turn，调用方要看到 OpenAI 的 stop。
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q，期望 stop（上游报的是 Anthropic 词汇里的 end_turn）",
			got.Choices[0].FinishReason)
	}
	if got.Usage.PromptTokens != 12 || got.Usage.CompletionTokens != 7 || got.Usage.TotalTokens != 19 {
		t.Errorf("usage = %+v，期望上游报的 12/7/19", got.Usage)
	}
	if got.Type != nil || got.StopReason != nil {
		t.Errorf("响应体里混进了 Anthropic 形状的字段（type=%s stop_reason=%s）——调用方说的是 OpenAI，它解不出这套",
			showPtr(got.Type), showPtr(got.StopReason))
	}

	// 二、钱：与另外三条路同一个数。
	bill, ok := billingOf(t, db, user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记")
	}
	if bill.status != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", bill.status)
	}
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003）", bill.final.String)
	}
	assertNotCapped(t, bill)
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d（这次请求实际走的那个 anthropic 渠道）", bill.channelID, channelID)
	}
	if bill.protocol != string(channel.ProtocolOpenAI) {
		t.Errorf("账单的 protocol = %q，期望 %q（记的是调用方说的那套，不是上游的 anthropic）",
			bill.protocol, channel.ProtocolOpenAI)
	}
	assertWallet(t, db, user, "99.99967", "0")

	// 三、上游收到的是 **Anthropic 形状**的请求体。
	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		System    any    `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if sent.Model != model {
		t.Errorf("上游收到的 model = %q，期望 %q", sent.Model, model)
	}
	if sent.MaxTokens != 64 {
		t.Errorf("上游收到的 max_tokens = %d，期望 64（调用方给的上界要原样传到上游，它同时是预留的依据）",
			sent.MaxTokens)
	}
	// OpenAI 的 system 是 messages 里的一条，Anthropic 那边是**顶层字段**。
	if got := textOf(t, sent.System); got != "你是假上游" {
		t.Errorf("上游收到的 system = %q，期望 %q（OpenAI 的 system 消息要搬成 Anthropic 的顶层字段）",
			got, "你是假上游")
	}
	// 搬过去之后**不能**还留在 messages 里：留着它等于同一条消息发了两遍，
	// 而模型会把它当成对话的一部分。
	if len(sent.Messages) != 1 {
		t.Fatalf("上游收到的 messages 有 %d 条，期望 1 条（只剩用户消息）：%+v",
			len(sent.Messages), sent.Messages)
	}
	if sent.Messages[0].Role != "user" || textOf(t, sent.Messages[0].Content) != "你好" {
		t.Errorf("上游收到的消息 = %+v，期望只有那条用户消息", sent.Messages[0])
	}
	// 端点：Anthropic 上游在 /v1/messages 上。
	if paths := fake.sentPaths(); len(paths) != 1 || paths[0] != "/v1/messages" {
		t.Errorf("上游收到的路径 = %v，期望 [/v1/messages]（Anthropic 上游的端点）", paths)
	}
}

// 上游（Anthropic）报了缓存命中时，回给调用方的输入量要**补齐成 OpenAI 口径**。
//
// 与 /v1/messages 那条缓存用例正好是一对，用的是同一批 token、同一个缓存量，
// 而两边的期望数不同——因为两家对「输入」的定义不同：
//
//	上游报的     input_tokens = 12（不含缓存）、cache_read = 5、output = 7
//	内部口径     prompt = 12 + 5 = 17（补齐成 OpenAI 口径，见 relay.fromClaudeResponse）
//	回给调用方   prompt_tokens = 17（OpenAI 的 prompt_tokens 含缓存，原样给）
//	计费口径     prompt = 17 − 5 = 12、cache_read = 5（缓存单独定价，不能重复计）
//	金额         12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002 = 0.00034
//
// 这一对用例合起来才说明「口径转换是双向都对的」：把加法删掉，这条会挂；
// 把减法删掉，那一条会挂。单看任何一条都只钉住了一半。
func TestChatCompletionsWithAnthropicUpstreamCacheUsage(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-model-chat-to-anthropic-cache")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol:  channel.ProtocolAnthropic,
		cacheRead: 5,
		// 非默认的结束原因：见文件顶部那张矩阵。
		finishReason: "max_tokens",
		db:           db,
		userID:       user,
		model:        model,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003, "cache_read": 0.000002}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t.Context(), t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var got struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}

	// 一、回给调用方的输入量必须**补上**缓存。填 12 就是把 Anthropic 的口径
	// 原样交给了说 OpenAI 的调用方——他的 prompt_tokens 按定义含缓存，少 5 个
	// 会让他的上下文计费、用量看板全部偏低，而响应形状完全正常。
	if got.Usage.PromptTokens != 17 {
		t.Errorf("prompt_tokens = %d，期望 17（上游的 12 加上 5 个缓存）；12 说明缓存没被补进来",
			got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens != 7 {
		t.Errorf("completion_tokens = %d，期望 7", got.Usage.CompletionTokens)
	}
	if got.Usage.TotalTokens != 24 {
		t.Errorf("total_tokens = %d，期望 24（17 + 7：OpenAI 的 total 是两项之和，缓存不单列）",
			got.Usage.TotalTokens)
	}
	if len(got.Choices) != 1 || got.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %+v，期望 length（上游报的是 Anthropic 词汇里的 max_tokens）", got.Choices)
	}
	// OpenAI 的 usage 里没有 input_tokens / cache_read_input_tokens 这两个键。
	// 它们出现在这里说明写错了形状。
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	usage, _ := raw["usage"].(map[string]any)
	if _, ok := usage["input_tokens"]; ok {
		t.Errorf("usage 里有 Anthropic 才有的 input_tokens：%v", usage)
	}
	if _, ok := usage["cache_read_input_tokens"]; ok {
		t.Errorf("usage 里有 Anthropic 才有的 cache_read_input_tokens：%v", usage)
	}

	// 二、钱。与 /v1/messages 那条缓存用例同一个数——同一批 token，只是走的
	// 转换方向相反。
	bill, ok := billingOf(t, db, user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记")
	}
	if bill.status != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", bill.status)
	}
	if !sameDecimal(bill.final.String, "0.00034") {
		t.Errorf("结算金额 = %s，期望 0.00034（12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002）；"+
			"0.00029 说明输入被当成了 7 个（缓存补进来又整个减掉了），0.00033 说明缓存没参与计价",
			bill.final.String)
	}
	assertNotCapped(t, bill)
	assertWallet(t, db, user, "99.99966", "0")
}

// 上游（Anthropic）报了缓存**写入**时，它同样要补进回给调用方的输入量。
//
// 上一条只测了缓存读取（`cache_read_input_tokens`），而缓存写入是另一项：
// Anthropic 把它报在 `cache_creation_input_tokens` 里，与读取分开。转换里
// 「补上缓存」那一步若只加了读取那一项，账单会少收写入那部分——**响应形状
// 完全正常**，只是 prompt_tokens 少了 3 个 token 的钱。
//
//	上游报的     input_tokens = 12（不含缓存）、cache_read = 5、cache_creation = 3、output = 7
//	内部口径     prompt = 12 + 5 + 3 = 20（缓存读写都要补进来）
//	回给调用方   prompt_tokens = 20（OpenAI 的 prompt_tokens 含全部缓存）
//	计费口径     prompt = 20 − 5 − 3 = 12、cache_read = 5、cache_write = 3
//	金额         12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002 + 3 × 0.000004 = 0.000352
//
// 两个缓存单价都取**非零且互不相等**：相等的话，把写入当成读取计价、或者两项
// 换了个位置，金额都还是同一个数，断言就分不出是哪一项错了。
func TestChatCompletionsWithAnthropicUpstreamCacheWriteUsage(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-model-chat-to-anthropic-cache-write")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{
		protocol:   channel.ProtocolAnthropic,
		cacheRead:  5,
		cacheWrite: 3,
		db:         db,
		userID:     user,
		model:      model,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model,
		`{"prompt": 0.00001, "completion": 0.00003, "cache_read": 0.000002, "cache_write": 0.000004}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t.Context(), t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	var got struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}

	// 一、输入量把**两种**缓存都补进来。17 说明只补了读取那一项。
	if got.Usage.PromptTokens != 20 {
		t.Errorf("prompt_tokens = %d，期望 20（上游的 12 加上 5 个缓存读取与 3 个缓存写入）；"+
			"17 说明缓存写入没被补进来，12 说明两种缓存都没补", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens != 7 {
		t.Errorf("completion_tokens = %d，期望 7", got.Usage.CompletionTokens)
	}
	if got.Usage.TotalTokens != 27 {
		t.Errorf("total_tokens = %d，期望 27（20 + 7）", got.Usage.TotalTokens)
	}
	if len(got.Choices) != 1 || got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %+v，期望 stop（上游报的是 Anthropic 的 end_turn）", got.Choices)
	}

	// 二、钱。两种缓存各按自己的单价算，都不能算进 prompt 那一档。
	bill, ok := billingOf(t, db, user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记")
	}
	if bill.status != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", bill.status)
	}
	if !sameDecimal(bill.final.String, "0.000352") {
		t.Errorf("结算金额 = %s，期望 0.000352（12 × 0.00001 + 7 × 0.00003 + 5 × 0.000002 + 3 × 0.000004）；"+
			"0.000322 说明写入没补进 prompt 而计费那侧仍在减它（输入被少算了 3 个），"+
			"0.00034 说明写入整项丢了", bill.final.String)
	}
	assertNotCapped(t, bill)
	assertWallet(t, db, user, "99.999648", "0")
}
