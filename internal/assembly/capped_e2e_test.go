package assembly

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：**一次真的撞上预留上界**的结算，事后在库里看得出来。
//
// # 为什么单独一条
//
// 其余端到端用例断言的都是「这一行没有撞上界」，而撞上界这条路在库里原本
// 一点痕迹都没有：final_amount 落的是 Settlement.Actual，它在撞上界时被改成
// 等于预留额（见 internal/billing 的 SettleWithCharges），超出预留的那一截
// 到落库时已经被抹平。于是「这次结算撞了上界」只活在进程内的一条计数器里
// （internal/pipeline 的 CappedSettlements），事后从库里既查不出是**哪一次**
// 请求撞的，也查不出**撞了多少**。
//
// 迁移 00021 加了 computed_amount（未截断的结算金额）。这条用例证明它真的
// 落库了，而且只在撞上界时与 final_amount 分叉。
//
// # 怎么造出一次真的撞上界
//
// 预留的上界由**请求里的 max_tokens** 撑着：输出侧按 max_tokens × 输出单价估
// （见 internal/billing 的 Reserve）。把 max_tokens 压到 1，而假上游照旧报
// 7 个输出 token，结算就必然超过预留——这正是 Capped 的注释里写的那种情形：
// 上游报的用量超过了我们给的 max_tokens。
//
// 走这条而不是「给假上游加一个旋钮让它多报输出」：压 max_tokens 是在**压上界
// 本身**，用的还是生产路上真实存在的输入（客户端要 1 个输出 token 是合法
// 请求），不必给夹具加一个只为这条用例存在的字段。
//
// 也不能走「规则把 cache_read 写成 prompt 的几倍」那条路：预留的输入侧取的是
// prompt / cache_read / cache_write 三者里**最贵**的单价（见 Reserve 里那一段），
// 结算怎么拆都盖得住，那条路今天撞不出上界。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。
func TestSettlementCapIsObservableInBillingRequest(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const startBalance = "100"
	model := unique("e2e-model-capped")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, startBalance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t.Context(), t, public.URL+"/v1/chat/completions", key,
		chatBodyMaxTokens(model, 1))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200——撞上界是一次静默少收，不该让请求失败。响应体：%s", status, body)
	}

	bill, ok := billingOf(t, db, user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记")
	}
	if bill.status != "settled" {
		t.Fatalf("账单状态 = %q，期望 settled", bill.status)
	}

	// 一、computed_amount 是**未截断**的结算金额：12 × 0.00001 + 7 × 0.00003
	// = 0.00033，按上游报的实际用量算。
	//
	// 断言具体金额而不是「比预留大」：写入点把 final_amount 抄进 computed_amount
	// 时后者也成立（那一行本来就被截过），而那样这一列就白加了。
	if !bill.computed.Valid {
		t.Fatal("computed_amount 是 NULL——未截断的结算金额没有落库，这条用例就没有判据可依")
	}
	if !sameDecimal(bill.computed.String, "0.00033") {
		t.Errorf("computed_amount = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003，按上游报的实际用量算）；"+
			"等于预留说明写进去的是截断之后的金额", bill.computed.String)
	}

	// 二、这一次真的撞上了：截断后的金额等于预留，而计算值大于它。
	//
	// 两条都要。只有前者时「预留恰好等于按用量算出的金额」也成立，而那一次
	// 根本没撞；只有后者时看不出截断有没有真的落到 final_amount 上。
	if !sameDecimal(bill.final.String, bill.reserved) {
		t.Errorf("final_amount = %s，预留 = %s，期望两者相等——超过预留的那一截被截住了",
			bill.final.String, bill.reserved)
	}
	if !greaterThan(bill.computed.String, bill.reserved) {
		t.Errorf("computed_amount = %s 不大于预留 %s——这一次没撞上界，用例的前提不成立"+
			"（max_tokens = 1 时输出侧上界只有 1 × 0.00003，而上游报了 7 个输出 token）",
			bill.computed.String, bill.reserved)
	}

	// 三、钱只动了预留那么多，差额由平台承担。
	//
	// 这一条与「final_amount == reserved」不是同一件事：那条说的是账单行上记了
	// 什么，这条说的是账本真的动了多少。少扣的那一截在响应上完全看不出来，
	// 而它正是这套系统里最贵的那类故障。
	var gotBalance, wantBalance string
	if err := db.QueryRowContext(t.Context(), `
		SELECT w.balance::text, ($1::numeric - b.reserved_amount)::text
		  FROM user_wallets w
		  JOIN billing_requests b ON b.user_id = w.user_id
		 WHERE b.request_id = $2 AND w.asset = 'CNY'`,
		startBalance, bill.requestID).Scan(&gotBalance, &wantBalance); err != nil {
		t.Fatalf("查钱包失败: %v", err)
	}
	if !sameDecimal(gotBalance, wantBalance) {
		t.Errorf("钱包余额 = %s，期望 %s（%s 减去预留 %s）——撞上界时扣的是预留额，"+
			"不是按用量算出来的 %s", gotBalance, wantBalance, startBalance, bill.reserved, bill.computed.String)
	}
}

// chatBodyMaxTokens 组装一次 OpenAI 协议的请求，max_tokens 由调用方给。
//
// 与 chatBody 分开是因为这里要的正是那个**小的** max_tokens：预留的输出侧按它
// 估，压小它才能造出一次撞上界的结算（见上面那条用例）。
func chatBodyMaxTokens(model string, maxTokens int64) []byte {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "你好"}},
		"max_tokens": maxTokens,
	})
	if err != nil {
		panic(err)
	}
	return body
}
