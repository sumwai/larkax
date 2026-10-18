package assembly

// 本文件是**流式递进冻结**的端到端用例（#498）：一次真的 HTTP 请求走到假上游，
// 余额在中途耗尽，断言回到库里的钱上。
//
// 它补的是 internal/gateway 那几条用例够不着的那一半：网关那几条的余额是夹具
// 里的一个数，而这里动的是**真的钱包与账本**——「余额不为负」「结算金额与
// 扣款一致」「预留不留在地上」这些不变量只在真库上才谈得上验证。
//
// # 三条各自钉什么
//
//  1. 声明上界很大、余额只够实际消耗的流式请求**跑得起来**（不再被 429 拒掉）；
//  2. 余额真的耗尽时这条流被切断，切断那一刻已产出的部分**按实际用量结算**、
//     不撞预留上界，钱包余额不为负；
//  3. 客户端收到的帧里有一条错误帧，而且**没有**正常结束标记。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件用的阈值段与单价。
//
// 段取 32：端到端这条路上的段来自 settings（出厂的 4096 太大——要产出 4096 个
// token 的假上游会让用例多跑几十帧，而这条用例要验的是「钱怎么动」，与段多大
// 无关）。单价与 stream_e2e_test.go 里那几条一致。
const (
	freezeE2EStep  = "32"
	freezeE2EPrice = `{"prompt": 0.00001, "completion": 0.00003}`
)

// freezeE2EBody 造一次流式请求：max_tokens 填得远大于余额付得起的输出量。
//
// 这正是 #498 要修的调用方形状——不少 SDK 的默认上界就很大，而按它冻满会让
// 一条余额够付实际消耗的请求在开始之前就被拒。
func freezeE2EBody(t *testing.T, model string, maxTokens int) []byte {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(chatBody(model), &body); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	body["stream"] = true
	body["max_tokens"] = maxTokens
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("编码请求体失败: %v", err)
	}
	return out
}

// freezeE2EChunks 造一串正文分片。
//
// 每一片的量要**远小于**一个阈值段（32 个 token）：敞口那条判据的前提就是
// 「一个检查窗口产出的量小于一个段」——一片就能越过整段时，任何按段补冻的
// 方案都来不及反应，而那测的不是这条路上真实发生的事（真实上游的分片是几十
// 个 token 一级的）。
func freezeE2EChunks(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "第一片")
	}
	return out
}

// 余额只够实际消耗的一条长输出请求**跑得起来**，余额耗尽时被切断，已产出的
// 部分按实际用量结算。
func TestStreamFreezeCutsOffWhenBalanceRunsOut(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	// 余额 0.005 元。段 32 个 token × 0.00003 = 一段 0.00096 元，所以初始那一段
	// 加输入侧之后，还够四五次补冻——这条流会在产出两三百个 token 之后被切断，
	// 而不是"跑完"（假上游有 200 片，够它跑很远）。
	const balance = "0.005"
	model := unique("e2e-stream-freeze")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   freezeE2EChunks(200),
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, freezeE2EPrice)

	app, err := buildAppWithSettings(t, db, map[string]string{streamFreezeStepKey: freezeE2EStep})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if got := app.Gateway.StreamFreezeStep; got != 32 {
		t.Fatalf("阈值段 = %d，期望 32——这条用例的前提是段被压小了", got)
	}
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 声明上界 60000：远大于余额付得起的输出量。按老口径（冻满声明上界）这一次
	// 请求会被 429 拒掉——所以「状态码是 200」本身就是那条修复的判据。
	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key,
		freezeE2EBody(t, model, 60000))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200（余额够付实际消耗就不该被拒）。响应体：%s",
			resp.StatusCode, body)
	}

	frames := readFrames(t, resp.Body, -1)

	// 1. 客户端收到一条错误帧
	errFrame := findErrorFrame(t, frames)
	if errFrame == "" {
		t.Fatalf("帧序列里没有错误帧——被切断的流必须告诉调用方。收到 %d 帧，最后一帧是 %+v",
			len(frames), lastFrame(frames))
	}
	// 文案要说清「被截断了」与「去充值」，而不是「上游失败」或「请重试」
	if !strings.Contains(errFrame, "balance") {
		t.Errorf("错误帧文案没说是余额问题：%s", errFrame)
	}

	// 2. **没有正常结束标记**：伪造一个会让调用方把半截回答当成完整回答
	for _, f := range frames {
		if strings.TrimSpace(f.data) == "[DONE]" {
			t.Error("被切断的流里出现了 [DONE]——那会让「回答被截断」看起来像「答完了」")
		}
	}

	// 切断**真的发生在中途**：假上游准备了 200 片正文，而这条流只该收到其中
	// 一小截（初始那一段加几次补冻）。收到 200 片说明它其实是跑完了——那这条
	// 用例什么都没测到（一个「跑完的流 + 一条错误帧」在断言上长得也像切断）。
	content := countContentFrames(frames)
	if content == 0 || content >= 200 {
		t.Errorf("收到 %d 片正文（假上游一共 200 片），期望中途被切断", content)
	}

	// 3. 账：按实际用量结算、不撞预留上界、状态是 settled
	bill := waitForBillingStatus(t, db, user, "settled")
	if !bill.streamed {
		t.Error("billing_requests.streamed = false")
	}
	assertNotCapped(t, bill)
	if !sameDecimal(bill.final.String, bill.computed.String) {
		t.Errorf("结算金额 %s 不等于按实际用量算出的 %s——已产出的部分要按实际用量收",
			bill.final.String, bill.computed.String)
	}
	if bill.final.String == bill.reserved {
		t.Error("结算金额恰好等于预留额：这条没有说服力（两种情况在库里长得一样）")
	}

	// 4. 钱：余额不为负，且恰好少了结算金额那一些；冻结额归零
	var walletBalance, frozen string
	if qerr := db.QueryRowContext(t.Context(),
		`SELECT balance::text, frozen::text FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		user).Scan(&walletBalance, &frozen); qerr != nil {
		t.Fatalf("查钱包失败: %v", qerr)
	}
	if !sameDecimal(frozen, "0") {
		t.Errorf("冻结额 = %s，期望 0：切断之后预留必须被处置掉，不能留在地上", frozen)
	}
	start, err := money.Parse(balance)
	if err != nil {
		t.Fatalf("解析初始余额失败: %v", err)
	}
	charged, err := money.Parse(bill.final.String)
	if err != nil {
		t.Fatalf("解析结算金额失败: %v", err)
	}
	if charged.Cmp(start) >= 0 {
		t.Errorf("结算金额 %s 不小于初始余额 %s：这一次不是「余额够付实际消耗」的形状",
			bill.final.String, balance)
	}
	assertWallet(t, db, user, start.Sub(charged).String(), "0")
	assertConsumeEntry(t, db, bill.id, user, bill.final.String)

	// 5. 上游**只被调了一次**：切断不等于换渠道重试
	if calls := fake.callCount(); calls != 1 {
		t.Errorf("上游被调用 %d 次，期望 1 次：切断之后不能换渠道重来"+
			"（调用方已经看到半截回答，重来会给它两段拼接的内容）", calls)
	}
}

// findErrorFrame 找出帧序列里的错误帧，返回它的 data；没有则返回空串。
func findErrorFrame(t *testing.T, frames []sseFrame) string {
	t.Helper()
	for _, f := range frames {
		if f.event == "error" {
			return f.data
		}
		// OpenAI 那套没有事件名，错误帧是一个带 error 字段的 data 行。
		if strings.Contains(f.data, `"error"`) {
			return f.data
		}
	}
	return ""
}

// countContentFrames 数一数客户端收到了几片正文。
//
// OpenAI 那套的正文分片在 choices[0].delta.content 里，而这一层不去解 JSON：
// 数「data 行里带 content 的帧」就够回答「这条流走了多远」，而为此引一个
// 解析器只会把这条断言的落点从「收到多少片」挪到「字段名写得对不对」。
func countContentFrames(frames []sseFrame) int {
	n := 0
	for _, f := range frames {
		if strings.Contains(f.data, `"content"`) {
			n++
		}
	}
	return n
}

// lastFrame 取最后一帧（断言失败时打出来）。
func lastFrame(frames []sseFrame) sseFrame {
	if len(frames) == 0 {
		return sseFrame{}
	}
	return frames[len(frames)-1]
}

// 余额够付实际消耗的一条流式请求**跑完**——即便调用方声明的上界远大于它。
//
// 与上一条是同一个场景的两半：那条的余额不够、这一条的够。两条合起来才说明
// 「按声明上界冻满」这件事真的不再决定一条流能不能跑起来。
func TestStreamFreezeRunsToCompletionOnSmallBalance(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	// 余额 0.005。这一段流产出约 80 个 token（20 片 × 约 4），按 0.00003 算
	// 约 0.0024，加上冻结的领先量（最多两个段）也在余额之内。
	const balance = "0.005"
	model := unique("e2e-stream-freeze-ok")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   freezeE2EChunks(20),
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, freezeE2EPrice)

	app, err := buildAppWithSettings(t, db, map[string]string{streamFreezeStepKey: freezeE2EStep})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 声明上界 60000 个 token。按声明冻满要 1.8 元，而余额只有 0.005——老口径下
	// 这一条会被 429 拒掉。
	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key,
		freezeE2EBody(t, model, 60000))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200（余额够付实际消耗）。响应体：%s", resp.StatusCode, body)
	}
	frames := readFrames(t, resp.Body, -1)

	// 1. 开始之前冻住的**只有第一段**，不是声明的上界
	observed := fake.observedFrozen()
	if len(observed) == 0 {
		t.Fatal("假上游一次都没被调用")
	}
	frozenAtCall, _, _ := strings.Cut(observed[0], " ")
	initial, err := money.Parse(frozenAtCall)
	if err != nil {
		t.Fatalf("解析「调上游那一刻的冻结额」失败（%q）: %v", observed[0], err)
	}
	if initial.Cmp(money.MustParse("0.002")) > 0 {
		t.Errorf("调上游时冻住了 %s，期望一个阈值段那么多（约 0.001）——"+
			"这不是按声明上界冻满（那会是 1.8 元，这次请求根本发不出去）", initial)
	}

	// 2. 流正常跑完：有结束标记、没有错误帧
	done := false
	for _, f := range frames {
		if strings.TrimSpace(f.data) == "[DONE]" {
			done = true
		}
		if f.event == "error" || strings.Contains(f.data, `"error"`) {
			t.Errorf("正常跑完的流里出现了错误帧：%s", f.data)
		}
	}
	if !done {
		t.Error("流跑完了却没有 [DONE]——调用方会把一次完整回答当成截断")
	}

	// 3. 账：按实际用量结算，不撞上界，冻结额归零
	bill := waitForBillingStatus(t, db, user, "settled")
	assertNotCapped(t, bill)
	if !sameDecimal(bill.final.String, bill.computed.String) {
		t.Errorf("结算金额 %s 不等于按实际用量算出的 %s", bill.final.String, bill.computed.String)
	}
	if !greaterThan(bill.reserved, bill.final.String) {
		t.Errorf("预留 %s 不大于结算 %s：更贵的那个数说明用量没有被估过", bill.reserved, bill.final.String)
	}
	assertWallet(t, db, user,
		mustSub(t, balance, bill.final.String), "0")
}

// mustSub 算 a − b（两个 numeric 文本），失败即让用例挂掉。
func mustSub(t *testing.T, a, b string) string {
	t.Helper()
	x, err := money.Parse(a)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", a, err)
	}
	y, err := money.Parse(b)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", b, err)
	}
	return x.Sub(y).String()
}
