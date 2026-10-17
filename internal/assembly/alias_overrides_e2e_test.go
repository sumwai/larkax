package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：别名上的**参数覆盖**真的生效。
//
// # 判据只能落在假上游收到的请求体上
//
// 这与别名那条端到端用例是同一个理由（见 alias_e2e_test.go 顶部）：断言
// 「库里那行配了覆盖」「网关读到了覆盖」在**读路径根本不存在**的实现上照样
// 通过。发出去的那份请求体是唯一直接证据。
//
// # 三条语义各一条用例
//
//   - 调用方没给 → 覆盖值生效
//   - 调用方给了采样参数 → **调用方赢**
//   - 调用方给了 max_tokens → **覆盖赢**（与上一条相反，所以两条都要有）
//
// 后两条的方向相反，只钉一条的话，把两条判据写成同一个方向在用例里看不出来。
//
// # 还有两条别的形状
//
//   - 一条覆盖都没配 → 请求体逐字不变（回归：绝大多数部署长这样）
//   - 覆盖把 max_tokens 调大 → **预留上界跟着变大**（否则结算撞上界、静默少收）

// seedAliasWithOverrides 插一条带参数覆盖的别名。
//
// 与 seedAlias 分开而不是给它加一个参数：本文件要用的每条别名都带覆盖，
// 而上面那个被十来条用例用着——给它们各加一个 nil 只会让那些调用点更啰嗦。
func seedAliasWithOverrides(t *testing.T, db *sql.DB, channelID any, upstream, canonical string, overrides map[string]any) {
	t.Helper()

	raw, err := json.Marshal(overrides)
	if err != nil {
		t.Fatalf("组装覆盖失败: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO model_aliases (channel_id, upstream_model, canonical_name, overrides)
		VALUES ($1, $2, $3, $4::jsonb)`, channelID, upstream, canonical, raw); err != nil {
		t.Fatalf("插入别名失败: %v", err)
	}
}

// sentParams 取出假上游第 i 次调用收到的请求体里的采样参数。
//
// 取字段而不是比对原始字节：字段顺序、空白与转义由编码器决定，按字节比会把
// 这些无关的东西一起钉住，改一次编码器就要改一遍用例。
//
// 三个字段都返回「有没有」：**没给**与「给了 0」必须分得开（temperature 的 0
// 是合法取值），而 map 取值分不出这两者。
func sentParams(t *testing.T, fake *fakeUpstream, i int) map[string]any {
	t.Helper()

	bodies := fake.sentBodies()
	if i >= len(bodies) {
		t.Fatalf("假上游只被调用 %d 次，取不到第 %d 次", len(bodies), i+1)
	}
	var sent map[string]any
	if err := json.Unmarshal(bodies[i], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[i])
	}
	return sent
}

// chatBodyWithParams 组装一次带采样参数的调用方请求。
//
// maxTokens 必填（预留要上界，见 chatBody）；temperature 与 topP 传 nil 表示
// **调用方没给这个字段**——而「没给」正是覆盖要生效的那一半。
func chatBodyWithParams(model string, maxTokens int, temperature, topP *float64) []byte {
	body := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "你好"}},
		"max_tokens": maxTokens,
	}
	if temperature != nil {
		body["temperature"] = *temperature
	}
	if topP != nil {
		body["top_p"] = *topP
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

// f64 取一个 float64 的指针，用来表达「调用方给了这个字段」。
//
// chatBodyWithParams 收指针而不是收值：**没给**与「给了 0」是两种不同的
// 输入（temperature 的 0 是合法取值），而值类型表达不出前者。
func f64(v float64) *float64 { return &v }

// 调用方**没给**采样参数时，别名上的覆盖值真的发到了上游。
//
// 这是这一列存在的意义：商家配一个默认采样参数，让不传它的调用方也走这个值。
func TestAliasOverrideReachesUpstreamWhenCallerOmittedIt(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-alias-ov")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAliasWithOverrides(t, db, channelID, "vendor/"+model, model,
		map[string]any{"temperature": 0.2, "top_p": 0.9})

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 调用方一个采样参数都不给。
	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key,
		chatBodyWithParams(model, 64, nil, nil))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	sent := sentParams(t, fake, 0)
	if got, ok := sent["temperature"]; !ok || got != 0.2 {
		t.Errorf("上游收到的 temperature = %v（给了没有：%v），期望 0.2——调用方没给，取覆盖值", got, ok)
	}
	if got, ok := sent["top_p"]; !ok || got != 0.9 {
		t.Errorf("上游收到的 top_p = %v（给了没有：%v），期望 0.9——调用方没给，取覆盖值", got, ok)
	}
}

// 调用方**给了**采样参数时，**调用方赢**。
//
// 反过来（覆盖赢）的表现是「我设的 temperature 没生效」——一件无法解释的事，
// 而且与「这一列根本没读」在调用方看来一模一样。
func TestAliasOverrideCallerWinsOnSamplingParams(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-alias-ov")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAliasWithOverrides(t, db, channelID, "vendor/"+model, model,
		map[string]any{"temperature": 0.2, "top_p": 0.9})

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key,
		chatBodyWithParams(model, 64, f64(0.7), f64(0.5)))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	sent := sentParams(t, fake, 0)
	if got := sent["temperature"]; got != 0.7 {
		t.Errorf("上游收到的 temperature = %v，期望 0.7——调用方给了就以调用方为准", got)
	}
	if got := sent["top_p"]; got != 0.5 {
		t.Errorf("上游收到的 top_p = %v，期望 0.5——调用方给了就以调用方为准", got)
	}
}

// max_tokens 反过来：**覆盖赢**，哪怕调用方给了、哪怕覆盖值更大。
//
// 与上一条成对钉着（两者方向相反）。这一条还有一层：发出去的是 1000，而调用方
// 要的是 64——所以预留上界也必须按 1000 算，那是下面那条用例钉的事。
func TestAliasOverrideMerchantWinsOnMaxTokens(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-alias-ov")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAliasWithOverrides(t, db, channelID, "vendor/"+model, model,
		map[string]any{"max_tokens": 1000})

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key,
		chatBodyWithParams(model, 64, nil, nil))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	sent := sentParams(t, fake, 0)
	// JSON 数字解出来是 float64。
	if got := sent["max_tokens"]; got != float64(1000) {
		t.Errorf("上游收到的 max_tokens = %v，期望 1000——这个字段商家赢", got)
	}
}

// 一条覆盖都没配时，请求体**逐字不变**（回归判据）。
//
// 绝大多数部署长这样，而它们的行为必须与这个功能出现之前相同。这里把三个
// 采样参数都断言一遍：少了任何一个，那一项「被默认值顶掉」都看不出来。
func TestAliasWithoutOverridesPassesTheCallerBodyThrough(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-alias-ov")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	// 别名照配，但覆盖是空对象——与库里那一列的默认值一致（'{}'::jsonb）。
	seedAliasWithOverrides(t, db, channelID, "vendor/"+model, model, map[string]any{})

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key,
		chatBodyWithParams(model, 64, f64(0.4), nil))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	sent := sentParams(t, fake, 0)
	if got := sent["max_tokens"]; got != float64(64) {
		t.Errorf("上游收到的 max_tokens = %v，期望调用方给的 64", got)
	}
	if got := sent["temperature"]; got != 0.4 {
		t.Errorf("上游收到的 temperature = %v，期望调用方给的 0.4", got)
	}
	// 调用方没给 top_p，覆盖也没配——那它就不该出现在请求体里。
	if got, ok := sent["top_p"]; ok {
		t.Errorf("上游收到了没配过的 top_p = %v，期望这个字段不出现", got)
	}
}

// 覆盖把 max_tokens 调大时，**预留上界跟着变大**。
//
// # 这一条钉的是复算口径
//
// 事后复算的输入是「当时那份请求」加上账单上留下的规则快照，而单据里唯一
// 与输出规模有关的数是**预留金额**（复算入口本身还没写，见 pipeline 的注释）。
// 上界按调用方给的 64 算、而实际发出去的请求允许生成 1000 个 token 时，结算
// 会撞上界：差额被截掉，账上那一行却完全正常——**少收**，没有东西会报出来。
//
// 规则只给输出侧定价（prompt 为 0），这样预留金额就是 max_tokens × 输出单价，
// 不掺输入侧的估算——否则断言里会多出一个「估出来几个 token」的变量。
func TestAliasOverrideMaxTokensRaisesTheReservationBound(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const startBalance = "100"
	model := unique("e2e-alias-ov")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, startBalance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAliasWithOverrides(t, db, channelID, "vendor/"+model, model,
		map[string]any{"max_tokens": 1000})

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 调用方要 64 个输出 token，而别名把它顶成 1000。
	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key,
		chatBodyWithParams(model, 64, nil, nil))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	bill := waitForBillingStatus(t, db, user, "settled")

	// 一、预留按**覆盖之后**的 1000 算：1000 × 0.00003 = 0.03。
	//
	// 断言具体金额而不是「比按 64 算的大」：后者在「按 64 算 + 别处多冻一点」
	// 的实现上也成立，而那样这一条就白写了。按调用方给的 64 算是 0.00192。
	if !sameDecimal(bill.reserved, "0.03") {
		t.Errorf("预留金额 = %s，期望 0.03（覆盖之后的 1000 × 0.00003）；"+
			"等于 0.00192 说明上界仍按调用方给的 64 算", bill.reserved)
	}

	// 二、结算按**上游实际报的用量**算，不是按上界：假上游报 7 个输出 token，
	// 7 × 0.00003 = 0.00021。
	//
	// 这一条与上一条一起才说明白「上界变大不等于多收」：上界只管冻多少，
	// 收多少由实际用量定。
	if !sameDecimal(bill.final.String, "0.00021") {
		t.Errorf("结算金额 = %s，期望 0.00021（7 × 0.00003，按上游报的用量算）", bill.final.String)
	}

	// 三、这次**没有**撞上界：按用量算出来的钱小于预留。
	//
	// 上界算错的后果正是撞上界，所以这一条是上一条的补充判据（见
	// assertNotCapped 的注释：final ≤ reserved 恒成立，钉不出任何东西）。
	assertNotCapped(t, bill)

	// 四、钱包真的只扣了结算的那一份，冻的那部分放回去了。
	assertWallet(t, db, user, "99.99979", "0")
}

// 请求快照记的是**发出去的那一份**请求，不是调用方发来的那一份。
//
// # 为什么这件事要端到端地钉
//
// 快照是事后复现的唯一凭据，而它最该回答的正是覆盖这类配置上的问题：「这次
// 到底按什么参数发的」。采错那一份不会报错——快照照常落库、字段照常齐全，
// 只是内容与实物不同；而在覆盖生效之前两者恰好相等，所以这个偏差只有在配上
// 覆盖之后才看得出来。
//
// # 判据
//
// 库里那一行 skeleton、假上游收到的请求体、调用方发的请求，三者摆在一起比：
// 快照要等于**上游收到的**那组值，且不等于调用方给的。两组值在本用例里都不同
// （调用方没给 temperature，给的 max_tokens 是 64），所以「采哪一份」在快照
// 内容上分得开。
//
// 上游那份是一起断言的，不能省：少了它，「快照 = 覆盖值」在「覆盖根本没生效
// 而快照按配置算了一遍」的实现上照样通过。
func TestAliasOverrideSnapshotRecordsWhatWentOut(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-alias-ov")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAliasWithOverrides(t, db, channelID, "vendor/"+model, model,
		map[string]any{"temperature": 0.2, "max_tokens": 2048})

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	// 调用方**没给** temperature（走覆盖值），max_tokens 给的是 64（被覆盖顶成 2048）。
	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key,
		chatBodyWithParams(model, 64, nil, nil))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	bill := waitForBillingStatus(t, db, user, "settled")

	// 落库是异步的，等到请求方向那一行真的在。
	var raw []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := db.QueryRowContext(ctx, `
			SELECT skeleton FROM request_snapshots
			 WHERE request_id = $1 AND direction = 'request'`, bill.requestID).Scan(&raw)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("没等到请求方向的快照落库（写入计数 %+v）: %v", app.Snapshots.Stats(), err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("快照的 skeleton 不是合法 JSON: %v\n%s", err, raw)
	}

	// 一、上游真的收到了覆盖之后的那组值。JSON 数字解出来是 float64。
	sent := sentParams(t, fake, 0)
	if got := sent["temperature"]; got != 0.2 {
		t.Errorf("上游收到的 temperature = %v，期望覆盖值 0.2", got)
	}
	if got := sent["max_tokens"]; got != float64(2048) {
		t.Errorf("上游收到的 max_tokens = %v，期望覆盖值 2048（调用方给的是 64）", got)
	}

	// 二、快照记的与它逐项相等——这才是「记的是发出去的那份」。
	if got := snap["temperature"]; got != 0.2 {
		t.Errorf("快照里的 temperature = %v，期望发出去的那个 0.2", got)
	}
	if got := snap["max_tokens"]; got != float64(2048) {
		t.Errorf("快照里的 max_tokens = %v，期望发出去的那个 2048", got)
	}

	// 三、且与调用方给的不同：采成调用方那份时这两个字段分别会缺席、会是 64。
	if _, ok := snap["temperature"]; !ok {
		t.Error("快照里没有 temperature——调用方没给这个字段，说明采的是调用方发来的那份")
	}
	if got := snap["max_tokens"]; got == float64(64) {
		t.Error("快照里的 max_tokens 等于调用方给的 64——采的是调用方发来的那份")
	}
}
