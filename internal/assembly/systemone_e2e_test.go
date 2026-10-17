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
	"sync"
	"sync/atomic"
	"testing"
)

// 端到端：`/v1/systemone` 这条入口走完整条链，钱真的动了。
//
// # 与 e2e_test.go 里那几条的关系
//
// 同一套装配（assembly.Build）、同一个真库、同一条计费两阶段，只是入口换成
// 决策那条路径。断言的口径也照旧：不看响应码就算完，回到库里看余额、
// 账单行与分录。
//
// # 为什么假上游要另起一个类型
//
// e2e_test.go 里的 fakeUpstream 按 channel.Protocol 回 OpenAI 或 Anthropic
// 形状的响应，两种都不是这条表面要的。硬塞一个分支进去会让那个类型同时
// 承担三套协议，而它的 successBody 已经被两条协议的口径差别占满了。
// 这里只要它回一份决策响应，另起一个更短也更清楚。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。

// systemoneUpstream 是决策那条表面上的假上游。
//
// 它记下三件在别处看不到的事，都是**正向证据**：
//
//   - 收到的请求体：证明发上去的 questions 是对象、且没有 stream
//   - 收到的路径与鉴权头：端点也是协议的一部分
//   - 处理这次请求时库里看到的冻结额：证明预留发生在调上游**之前**
type systemoneUpstream struct {
	db     *sql.DB
	userID int64
	// model 是渠道那一行登记的模型名，用来拼出「上游回报的名字」。
	model string

	// inputTokens / outputTokens 是这次响应报的用量。写死在假上游里，
	// 于是结算金额可以逐位核对，而不是「算出来多少就是多少」。
	inputTokens  int64
	outputTokens int64

	fail atomic.Bool

	mu       sync.Mutex
	calls    int
	paths    []string
	auths    []string
	bodies   [][]byte
	frozenAt []string
}

func (f *systemoneUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// 查库看这一刻冻了多少钱。判据是「冻没冻」而不是金额：
		// 金额那一侧由账单行的 reserved_amount 断言。
		var frozen string
		var isFrozen bool
		_ = f.db.QueryRowContext(r.Context(),
			`SELECT frozen::text, frozen > 0 FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
			f.userID).Scan(&frozen, &isFrozen)

		f.mu.Lock()
		f.calls++
		f.paths = append(f.paths, r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.bodies = append(f.bodies, body)
		f.frozenAt = append(f.frozenAt, fmt.Sprintf("%s (冻结=%t)", frozen, isFrozen))
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if f.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"placeholder upstream failure"}}`)
			return
		}

		// 上游回报的模型名**故意与调用方要的那个不同**：真实上游回报的名字
		// 经常不是调用方要的那一个。两边同名时，「报给调用方的是哪一个」
		// 就分不出来——把入口那一步的覆盖删掉，用例照样全绿。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": f.model + "-upstream",
			"answers": map[string]any{
				"q_noul":   map[string]any{"type": "noul", "noul": 0.75},
				"q_choice": map[string]any{"type": "choice", "choice": "billing", "confidence": 1},
				"q_score":  map[string]any{"type": "score", "score": 1.8, "confidence": 0.7},
			},
			"usage": map[string]any{
				"input_tokens":  f.inputTokens,
				"output_tokens": f.outputTokens,
			},
		})
	})
}

func (f *systemoneUpstream) snapshot() (calls int, paths, auths []string, bodies [][]byte, frozen []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls,
		append([]string(nil), f.paths...),
		append([]string(nil), f.auths...),
		append([][]byte(nil), f.bodies...),
		append([]string(nil), f.frozenAt...)
}

// decisionBody 组装一次决策请求：三个问题、三种类型，形状照实测的那一份。
func decisionBody(model string) []byte {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"state": "工单: 用户说付款后没到账",
		"questions": map[string]any{
			"q_noul": map[string]any{
				"type": "noul", "instructions": "这是账单问题吗",
			},
			"q_choice": map[string]any{
				"type": "choice", "instructions": "路由到哪个团队",
				"criteria": map[string]any{"billing": "付款相关", "tech": "技术故障"},
			},
			"q_score": map[string]any{
				"type": "score", "instructions": "紧急程度",
				"criteria": []string{"low", "medium", "high"},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// 成功路径：一次决策调用跑通，钱按上游报的用量扣了，预留上界也真的接上了。
func TestSystemOneEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-decision")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &systemoneUpstream{
		db: db, userID: user, model: model,
		inputTokens: 387, outputTokens: 72,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "systemone", 0)
	// 单价写死在规则里，于是期望金额可以逐位算出来。
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/systemone", key, decisionBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、响应形状。调用方按上游那套形状写解析器，字段名错了它读不出来。
	var got struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type   string  `json:"type"`
			Noul   float64 `json:"noul"`
			Choice string  `json:"choice"`
			Score  float64 `json:"score"`
		} `json:"answers"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	// 报给调用方的是**他要的那个**模型名，不是上游回报的那个。
	if got.Model != model {
		t.Errorf("model = %q，期望 %q（报给调用方的应当是他要的那个名字，上游回报的是 %q）",
			got.Model, model, model+"-upstream")
	}
	if len(got.Answers) != 3 {
		t.Errorf("answers = %+v，期望三个答案", got.Answers)
	}
	if got.Answers["q_noul"].Noul != 0.75 || got.Answers["q_choice"].Choice != "billing" ||
		got.Answers["q_score"].Score != 1.8 {
		t.Errorf("三种答案的形状没有原样透出来: %+v", got.Answers)
	}
	if got.Usage.InputTokens != 387 || got.Usage.OutputTokens != 72 {
		t.Errorf("usage = %+v，期望上游报的 387/72", got.Usage)
	}

	// 二、钱。387 × 0.00001 + 72 × 0.00003 = 0.00603。
	bill, ok := billingOf(t, db, user)
	if !ok {
		t.Fatal("没有账单行——请求成功了钱却没记")
	}
	if bill.status != "settled" {
		t.Errorf("账单状态 = %q，期望 settled", bill.status)
	}
	if !bill.final.Valid || !sameDecimal(bill.final.String, "0.00603") {
		t.Errorf("结算金额 = %v，期望 0.00603（387 × 0.00001 + 72 × 0.00003）", bill.final)
	}
	assertNotCapped(t, bill)

	// 三、预留的上界**真的接上了**。
	//
	// 这条表面没有 max_tokens 一类的参数，所以上界只能来自 internal/systemone
	// 的两个常量。断言具体金额而不是「大于结算」：后者在把常量改小、
	// 甚至退化成「按正文估一估」时照样成立（那些数都比结算大），
	// 而这条用例存在的理由正是「上界是那两个常量撑的」。
	//
	//	输入侧：65536 × 0.00001 = 0.65536（规则里 cache_read / cache_write 没写，
	//	        取三者最大值时它们按零算）
	//	输出侧：4096 × 0.00003 = 0.12288
	//	合计：0.77824
	if !sameDecimal(bill.reserved, "0.77824") {
		t.Errorf("预留 = %s，期望 0.77824（65536 × 0.00001 + 4096 × 0.00003）——"+
			"预留的上界没有按 internal/systemone 的两个常量取", bill.reserved)
	}

	// 四、账单上的协议是这条表面自己那一个，不是 openai 或 anthropic。
	// 记成另外两个的话，事后按协议归因时它会被算进别的表面。
	if bill.protocol != "systemone" {
		t.Errorf("账单上的协议 = %q，期望 systemone", bill.protocol)
	}
	if bill.streamed {
		t.Error("这条表面从不流式，账单上不该记成流式")
	}
	if bill.canonicalModel != model {
		t.Errorf("账单上的规范名 = %q，期望 %q", bill.canonicalModel, model)
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d（这次请求实际走的渠道）", bill.channelID, channelID)
	}

	// 五、余额：扣掉实际金额，冻结额回到零。
	assertWallet(t, db, user, "99.99397", "0")

	// 六、账本：那一笔分录在，且挂回了这次账单。
	userAccount := fmt.Sprintf("user:%d", user)
	var txnID string
	if err := db.QueryRowContext(ctx, `
		SELECT txn_id::text FROM ledger_entries
		 WHERE billing_request_id = $1 AND account = $2 AND entry_type = 'consume'
		 ORDER BY id DESC LIMIT 1`, bill.id, userAccount).Scan(&txnID); err != nil {
		t.Fatalf("账本里没有挂到账单 %d 上的消费分录: %v", bill.id, err)
	}
	var debit string
	if err := db.QueryRowContext(ctx, `
		SELECT (-amount)::text FROM ledger_entries
		 WHERE txn_id = $1 AND account = $2`, txnID, userAccount).Scan(&debit); err != nil {
		t.Fatalf("查用户那一侧的分录失败: %v", err)
	}
	if !sameDecimal(debit, "0.00603") {
		t.Errorf("从用户账上扣掉的 = %s，期望 0.00603（与结算金额一致）", debit)
	}

	// 七、上游真的收到了什么。
	calls, paths, auths, bodies, frozen := fake.snapshot()
	if calls != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", calls)
	}
	// 端点也是协议的一部分：发错路径时上游回 404，而调用方看到的是一个
	// 含糊的 502——原因只有在发出去的那一侧才看得见。
	if paths[0] != "/v1/systemone" {
		t.Errorf("上游收到的路径 = %q，期望 /v1/systemone", paths[0])
	}
	if auths[0] != "Bearer test-key-placeholder" {
		t.Errorf("鉴权头 = %q", auths[0])
	}
	// 预留发生在调上游**之前**：只看请求结束后的冻结额分不出
	// 「冻过又放开了」与「压根没冻」，而后者正是失败路径那条回归要防的。
	if !strings.Contains(frozen[0], "冻结=true") {
		t.Errorf("调上游时看到的钱包 = %q，期望钱已经冻上了", frozen[0])
	}

	var sent struct {
		Model     string                     `json:"model"`
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	// questions 必须是**对象**：传数组会被上游直接拒
	//（Invalid input: expected record, received array）。
	if len(sent.Questions) != 3 {
		t.Errorf("发上去的 questions 不是按 id 索引的对象: %s", bodies[0])
	}
	if sent.State != "工单: 用户说付款后没到账" {
		t.Errorf("发上去的 state = %q", sent.State)
	}
	if strings.Contains(string(bodies[0]), `"stream"`) {
		t.Errorf("发上去的请求体里出现了 stream: %s", bodies[0])
	}
}

// 上游失败：不计费，钱放开。
//
// 与 chat 那条路同一个口径。单独写一条是因为这条路径**另起了一份编排**
// （见 internal/gateway/decide.go），共用的是那四个方法而不是整段流程——
// 共用面越小，越需要各自钉一遍。
func TestSystemOneUpstreamFailureIsNotBilled(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-decision-fail")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &systemoneUpstream{db: db, userID: user, model: model, inputTokens: 387, outputTokens: 72}
	fake.fail.Store(true)
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "systemone", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/systemone", key, decisionBody(model))
	if status == http.StatusOK {
		t.Fatalf("上游回 500，入口却回了 200。响应体：%s", body)
	}

	// 钱一分不动，冻结额回到零。
	assertWallet(t, db, user, balance, "0")

	// 账单行可以在（预留那一行是 begin 写的），但绝不能是 settled。
	if bill, ok := billingOf(t, db, user); ok && bill.status == "settled" {
		t.Errorf("上游失败了账单却结算了：%+v", bill)
	}
}

// 规则里少写一个单价时，**整条请求失败**，而不是按写了的那个价少收。
//
// # 为什么这一条值得单独测
//
// 上游报的用量里既有输入也有输出，而规则只声明了 prompt 的价。此时有两种
// 都能「跑通」的做法：按 prompt 收一笔、把输出那部分当零（静默少收），
// 或者拒掉整条请求。前者在响应上完全看不出来——调用方拿到 200 与一份正确
// 的答案，钱却少扣了，而这类错误只在事后对账时才现形。
//
// 判据因此落在两处：调用方**没有**拿到 200，且钱一分没动。
func TestSystemOneMissingPriceSlotFailsWholeRequest(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-decision-noprice")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &systemoneUpstream{db: db, userID: user, model: model, inputTokens: 387, outputTokens: 72}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "systemone", 0)
	// 只写输入侧的价：上游报了 72 个输出 token，而规则没给 completion 的价。
	seedRule(t, db, model, `{"prompt": 0.00001}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/systemone", key, decisionBody(model))
	if status == http.StatusOK {
		t.Fatalf("规则缺 completion 的价，入口却回了 200——那等于把输出那部分当零收。"+
			"响应体：%s", body)
	}

	// 钱一分不动：既没按半条规则少收，也没冻着。
	assertWallet(t, db, user, balance, "0")

	if bill, ok := billingOf(t, db, user); ok && bill.status == "settled" {
		t.Errorf("规则缺单价却结算了：%+v", bill)
	}
}
