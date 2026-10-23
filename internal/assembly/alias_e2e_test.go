package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：别名真的生效。
//
// # 这个缺陷原来的形状
//
// 写入路径是齐的（`larkaxctl alias create` 能把别名写进 model_aliases），
// 而请求热路径上一次都不读它——于是上游收到的仍是规范名，别名配了等于没配。
// 那种状态下，库里那行别名写得完全正确，读别名的函数也有实现、有单测。
//
// 所以判据只能落在**假上游收到的请求体**上：断言某个内部字段等于上游名，
// 或者断言库里那行别名写对了，在「读路径根本不存在」的实现上照样通过
// （见 LESSONS.md 第 23 条）。
//
// # 两条边界的口径
//
// 查不到别名时是报错还是按同名回落，判据落在**这个规范名**上：
//
//   - 这个规范名在任何作用域下都没有别名 → 按同名发出去（一条别名都没配的
//     部署走的就是这条路，它们必须照旧能打通）
//   - 配过，但没有一条覆盖**选中的那个渠道** → 报错，不回落
//
// 后一种是「配了一半」：回落会让上游回一句「这个模型不存在」，排查方向指向
// 上游，而真正要改的是我们这一侧的别名配置。两种边界下面各有一条用例钉着。

// ── 造数据 ────────────────────────────────────────────────────────────

// seedCatalogModel 往模型目录里插一行规范名。
//
// model_aliases.canonical_name 有外键指向它，所以配别名之前必须先有这一行。
// 走 SQL 而不是管理面：本文件要的只是那条外键指向的一行，而管理面需要主密钥
// 与一整套请求形状，与这里要测的东西无关。
func seedCatalogModel(t *testing.T, db *sql.DB, canonical string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO model_catalog (canonical_name, display_name) VALUES ($1, $1)`,
		canonical); err != nil {
		t.Fatalf("插入模型目录失败: %v", err)
	}
}

// seedAlias 插一条别名。channelID 传 nil 表示平台级别名（对所有渠道生效）。
func seedAlias(t *testing.T, db *sql.DB, channelID any, upstream, canonical string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO model_aliases (channel_id, upstream_model, canonical_name)
		VALUES ($1, $2, $3)`, channelID, upstream, canonical); err != nil {
		t.Fatalf("插入别名失败: %v", err)
	}
}

// sentModel 取出假上游第 i 次调用收到的请求体里的 model 字段。
//
// 解析请求体而不是比对原始字节：字段顺序、空白与转义由编码器决定，
// 按字节比会把这些无关的东西一起钉住，改一次编码器就要改一遍用例。
func sentModel(t *testing.T, fake *fakeUpstream, i int) string {
	t.Helper()

	bodies := fake.sentBodies()
	if i >= len(bodies) {
		t.Fatalf("假上游只被调用 %d 次，取不到第 %d 次", len(bodies), i+1)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(bodies[i], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[i])
	}
	return sent.Model
}

// ── 用例 ──────────────────────────────────────────────────────────────

// 配了渠道级别名之后，上游收到的是**上游名**，账单也记下了它。
//
// 三条断言缺一不可：
//
//   - 上游收到上游名 —— 别名生效的唯一直接证据
//   - 响应里回的是规范名 —— 报给调用方的必须是他要的那个名字
//   - billing_requests.upstream_model 是上游名 —— 事后按渠道对账时，
//     「这次实际用了哪个名字」只有这一列答得出来
func TestAliasReachesUpstreamAndBillingRow(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-alias")
	upstreamName := "vendor/" + model

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	// 计价规则按**规范名**匹配：售价挂在入站解析之后的规范名上，
	// 上游名只是我们与上游之间的叫法（别名换一次不该改价）。
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAlias(t, db, channelID, upstreamName, model)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、上游收到的是上游名。
	if got := sentModel(t, fake, 0); got != upstreamName {
		t.Errorf("上游收到的 model = %q，期望 %q（别名配了就要生效）", got, upstreamName)
	}

	// 二、报给调用方的仍是他要的那个名字。
	var got struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if got.Model != model {
		t.Errorf("响应里的 model = %q，期望 %q（上游名是我们与上游之间的事）", got.Model, model)
	}

	// 三、账单两列分开落：规范名与上游名各记各的。
	bill := waitForBillingStatus(t, db, user, "settled")
	if bill.canonicalModel != model {
		t.Errorf("账单的 canonical_model = %q，期望 %q", bill.canonicalModel, model)
	}
	if !bill.upstreamModel.Valid || bill.upstreamModel.String != upstreamName {
		t.Errorf("账单的 upstream_model = %+v，期望 %q", bill.upstreamModel, upstreamName)
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, channelID)
	}
	assertWallet(t, db, user, "99.99967", "0")
}

// 同一个规范名在两个作用域下都配了别名时，**渠道级胜出**。
//
// 这条判据本身在 internal/catalog 里（两条偏唯一索引保证的），这里钉的是
// 「端到端走下来确实是那一条」：平台级别名对所有渠道生效，一旦它盖过渠道级，
// 「这家上游叫 vendor-a/x、那家叫 x」就没法表达了，而两家叫法恰好相同时
// 请求照样成功——错误没有任何地方报出来。
func TestChannelAliasBeatsPlatformAlias(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-alias-scope")
	platformName := "platform/" + model
	channelName := "channel/" + model

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAlias(t, db, nil, platformName, model)
	seedAlias(t, db, channelID, channelName, model)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}
	if got := sentModel(t, fake, 0); got != channelName {
		t.Errorf("上游收到的 model = %q，期望渠道级那条 %q（不是平台级那条 %q）",
			got, channelName, platformName)
	}
	if bill := waitForBillingStatus(t, db, user, "settled"); bill.upstreamModel.String != channelName {
		t.Errorf("账单的 upstream_model = %+v，期望渠道级那条 %q", bill.upstreamModel, channelName)
	}
}

// 「配了一半」：这个规范名配过别名，但没有一条覆盖**选中的那个渠道**。
//
// 这是边界口径里报错的那一侧。回落成规范名的表现是请求成功、上游回一句
// 「这个模型不存在」——排查方向指向上游，而真正要改的是我们这一侧的别名
// 配置。所以这条用例同时钉三件事：状态码是 500（我们自己的配置问题，
// 不是上游的 502）、**一次上游调用都没发生**、预留被放开。
func TestAliasMissingForSelectedChannelFailsRequest(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-alias-half")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	// 两个渠道都提供这个模型，选路按优先级取数值大的那个（10 > 1）。
	// 别名只配在**落选的那个**渠道上，于是选中渠道翻不出名字来。
	selected := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "openai", 10)
	other := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "openai", 1)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAlias(t, db, other, "other/"+model, model)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（别名配置是我们这一侧的问题，不是上游的）。响应体：%s",
			status, body)
	}

	// 一次上游调用都不该发生：回落会真的把规范名发出去。
	if n := fake.callCount(); n != 0 {
		t.Errorf("假上游被调用 %d 次，期望 0 次——查不到别名时不该发出去", n)
	}

	// 钱要放开，余额一分没动。
	assertWallet(t, db, user, balance, "0")

	bill := waitForBillingStatus(t, db, user, "released")
	if bill.channelID.Valid && bill.channelID.Int64 == selected {
		t.Error("被放开的预留不该记下渠道——选路根本没走完")
	}
	if bill.upstreamModel.Valid {
		t.Errorf("被放开的预留 upstream_model = %q，期望 NULL", bill.upstreamModel.String)
	}
}

// 一条别名都没配的部署照旧按同名发出去。
//
// 这是边界口径里回落的那一侧，也是**大多数部署**的形状：它们从没碰过别名，
// 而「查不到就报错」会让它们全部 500。库里其它模型的别名行不参与判断——
// 判据落在**这个规范名**上，所以下面先给另一个模型配一条别名。
func TestNoAliasForThisModelSendsCanonicalName(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-alias-none")
	otherModel := unique("e2e-alias-other")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, otherModel)
	seedAlias(t, db, channelID, "other/"+otherModel, otherModel)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（这个规范名一条别名都没配过）。响应体：%s", status, body)
	}
	if got := sentModel(t, fake, 0); got != model {
		t.Errorf("上游收到的 model = %q，期望按同名发出 %q", got, model)
	}
	// 同名时这一列也照样落值：它记的是「实际发出去的名字」，
	// 而不是「与规范名不同的那个名字」——留空会让「漏传」与「同名」混在一起。
	if bill := waitForBillingStatus(t, db, user, "settled"); bill.upstreamModel.String != model {
		t.Errorf("账单的 upstream_model = %+v，期望 %q", bill.upstreamModel, model)
	}
}

// 流式那条路同样要翻，并同样把上游名落进账单。
//
// 两条路各写一份选路与结算的调用，只钉一条的话另一条漏了不会有任何迹象。
func TestStreamAliasReachesUpstreamAndBillingRow(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	model := unique("e2e-alias-stream")
	upstreamName := "vendor/" + model

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &streamUpstream{
		protocol: channel.ProtocolOpenAI,
		model:    model,
		chunks:   []string{"你好"},
		usage:    &channel.Usage{PromptTokens: 12, CompletionTokens: 7},
		db:       db,
		userID:   user,
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAlias(t, db, channelID, upstreamName, model)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/chat/completions", key, streamBody(model))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, out)
	}
	// 读到底：网关在关闭事件通道**之前**完成结算，所以读完就说明账写完了。
	readFrames(t, resp.Body, 0)

	bodies := fake.sentBodies()
	if len(bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(bodies))
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(bodies[0], &sent); err != nil {
		t.Fatalf("上游收到的请求体不是合法 JSON: %v\n%s", err, bodies[0])
	}
	if sent.Model != upstreamName {
		t.Errorf("上游收到的 model = %q，期望 %q", sent.Model, upstreamName)
	}

	bill := waitForBillingStatus(t, db, user, "settled")
	if !bill.streamed {
		t.Error("billing_requests.streamed = false——这次走的确实是流式那条路")
	}
	if !bill.upstreamModel.Valid || bill.upstreamModel.String != upstreamName {
		t.Errorf("账单的 upstream_model = %+v，期望 %q", bill.upstreamModel, upstreamName)
	}
}
