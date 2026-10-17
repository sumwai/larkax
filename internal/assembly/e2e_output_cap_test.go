package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：单次输出上限那条判据（见 gateway.checkOutputCap）在生产装配下的样子。
//
// 走的是真的那条链（HTTP → 鉴权 → 候选来源 → 预留），所以这几条同时钉住三件
// 在单测里各只能钉一半的事：
//
//   - 超限回的是 **4xx**，不是 502、也不是「算不出预留上界」那一条：映射那一步
//     在 internal/api，而这一条会穿过它；
//   - 拒在**冻钱之前**：钱包的冻结额与账单行都不该动；
//   - 上限取哪个数：目录里登记了 output_limit 就按目录那个数，没登记按
//     settings 里那个全局默认值——而后者是**运行期配置**，改了立刻生效。
//
// 上限之内那条也要有一条正向用例：判据装歪了（例如顺手把上界也夹到上限）
// 在拒绝那几条上完全看不出来。

// capBody 组装一次带指定 max_tokens 的请求体。
//
// 与 chatBody 分开而不是给它加一个参数：那条 helper 的 64 是「够用就行」的
// 随手值，而这里那个数是**被测的输入**，写错一个数就会让「超限」与「没超限」
// 互换，而被测的行为看起来照样正常。
func capBody(model string, maxTokens int) []byte {
	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
		"max_tokens": maxTokens,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// frozenOf 读一个用户的冻结额。
func frozenOf(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	var frozen string
	if err := db.QueryRowContext(context.Background(),
		`SELECT frozen::text FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		userID).Scan(&frozen); err != nil {
		t.Fatalf("读冻结额失败: %v", err)
	}
	return frozen
}

// 声明超过上限的非流式请求：400、点名这个字段、给出允许的上限，而且上游一次
// 都不被调用、钱一分都不动。
func TestMaxTokensOverCapIsRejectedBeforeUpstream(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-over-cap")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 上限压到 64，这次请求声明 65。走生产装配路径：这个键从库里读出来、
	// 装进网关，判据用的就是它。
	app, err := buildAppWithSettings(t, db, map[string]string{
		"billing.max_output_tokens": `64`,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, out := postChat(ctx, t, public.URL+"/v1/chat/completions", key, capBody(model, 65))
	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应体 %s）", status, out)
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
	// 三个都要在：改哪个字段、他写了多少、允许到多少。少一个都得猜。
	for _, want := range []string{"max_tokens", "65", "64"} {
		if !strings.Contains(got.Error.Message, want) {
			t.Errorf("错误体里没有 %q: %q", want, got.Error.Message)
		}
	}

	// 拒在预留之前：上游一次都没被调用（调用方自己的问题不该消耗上游额度），
	// 钱也一分没动。
	if n := fake.callCount(); n != 0 {
		t.Errorf("上游被调用 %d 次，期望 0 次", n)
	}
	assertWallet(t, db, user, balance, "0")

	var bills int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_requests WHERE user_id = $1`, user).Scan(&bills); err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	if bills != 0 {
		t.Errorf("billing_requests 有 %d 行，期望 0（请求在预留之前就被拒了）", bills)
	}
}

// 上限之内：请求照常走完整条链，钱照常结算，冻结回到零。
//
// 上限是 64，这次也声明 64——**恰好等于不算超**，与 route 那条目录判据的边界
// 一致（两处不一致的话，会出现「选路放行、上限却拒掉」这种自相矛盾的拒绝）。
func TestMaxTokensAtCapPassesEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-at-cap")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app, err := buildAppWithSettings(t, db, map[string]string{
		"billing.max_output_tokens": `64`,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, out := postChat(ctx, t, public.URL+"/v1/chat/completions", key, capBody(model, 64))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", status, out)
	}
	if n := fake.callCount(); n != 1 {
		t.Errorf("上游被调用 %d 次，期望 1 次", n)
	}
	// 结算之后冻结要回到零：停在冻结上说明这次请求没有收尾。
	if frozen := frozenOf(t, db, user); !sameDecimal(frozen, "0") {
		t.Errorf("冻结额 = %s，期望 0（请求已结算，那笔预留该放开）", frozen)
	}
}

// 模型目录里登记了 output_limit 时，判据取**目录那个数**——settings 里那个
// 全局默认值更大也不顶用。
//
// 这一条同时钉住一个顺序：目录里登记了上限的模型，调用方超限时拿到的仍然是
// 这个 **400**，而不是选路那条目录判据给出的 503（那条判据在预留之后，报的是
// 「没有可用渠道」——把一个字段填得离谱说成了我们没有渠道）。
func TestMaxTokensOverCapUsesDirectoryLimit(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-cap-dir")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 目录里登记：这个模型单次最多输出 32 个 token。全局默认值留在库里那个
	// 65536——如果判据取的是它，下面这次请求会被放行。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO model_catalog (canonical_name, display_name, output_limit) VALUES ($1, $1, 32)`,
		model); err != nil {
		t.Fatalf("插入模型目录失败: %v", err)
	}

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, out := postChat(ctx, t, public.URL+"/v1/chat/completions", key, capBody(model, 33))
	if status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应体 %s）", status, out)
	}

	var got struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, out)
	}
	for _, want := range []string{"33", "32"} {
		if !strings.Contains(got.Error.Message, want) {
			t.Errorf("错误体里没有 %q（上限要取目录那个数）: %q", want, got.Error.Message)
		}
	}
	if n := fake.callCount(); n != 0 {
		t.Errorf("上游被调用 %d 次，期望 0 次", n)
	}
	assertWallet(t, db, user, balance, "0")
}

// 上限是**运行期配置**：改库里的值，下一次装配就按新值判。
//
// 这一条与上面几条分开的理由是它有自己的一句话要说——「上限可配」这件事只有
// 换一个值再跑一遍才证明得了，而在同一个值上跑十次都不行。
func TestMaxTokensCapFollowsSettings(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-cap-settings")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 第一轮：上限 64，声明 65 被拒。
	app, err := buildAppWithSettings(t, db, map[string]string{
		"billing.max_output_tokens": `64`,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	if status, out := postChat(ctx, t, public.URL+"/v1/chat/completions", key,
		capBody(model, 65)); status != http.StatusBadRequest {
		t.Fatalf("上限 64 时状态码 = %d，期望 400（响应体 %s）", status, out)
	}

	// 第二轮：把上限提到 128，同一个请求（声明 65）现在要放行。
	//
	// 这条断言的方向很重要：只钉「被拒」的话，一个把上限写成常量 64 的实现
	// 照样能过。换一个值之后**行为跟着变**才说明这个键真的被读了。
	app2, err := buildAppWithSettings(t, db, map[string]string{
		"billing.max_output_tokens": `128`,
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	public2 := httptest.NewServer(app2.Public)
	t.Cleanup(public2.Close)

	if status, out := postChat(ctx, t, public2.URL+"/v1/chat/completions", key,
		capBody(model, 65)); status != http.StatusOK {
		t.Fatalf("上限提到 128 之后状态码 = %d，期望 200（响应体 %s）", status, out)
	}
	// 两轮之间没有别的变量：同一个模型、同一个 key、同一份请求体。第一轮被拒
	// 的那次也没留下什么（拒在预留之前，见上一条用例）。
	if frozen := frozenOf(t, db, user); !sameDecimal(frozen, "0") {
		t.Errorf("冻结额 = %s，期望 0（两轮都收尾了）", frozen)
	}
}
