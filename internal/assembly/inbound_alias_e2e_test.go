package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 端到端：入站别名在**生产装配路径**上真的生效。
//
// # 为什么这条必须走装配
//
// 解析器是 Handler 上的一个字段，装配时漏接的症状是「配了别名却不生效」——
// 没有任何报错，请求拿别名去选路，然后报「查无此模型」。internal/api 的用例
// 给的是一个桩，证明不了装配层真的把 catalog 接上了。
//
// # 判据落在三处
//
// 一、上游收到的是**上游名**（规范名翻出来的那个），不是别名也不是规范名；
// 二、响应体回显的是**调用方写的别名**；
// 三、账单的 canonical_model 是**规范名**——用量与链路诊断按规范名走，这条
//     不能只靠实现约定。
//
// 三条缺任何一条，「同一份用量不会因为客户端写短名还是全名分成两行」这句话
// 都还有一半没被证明。

// seedInboundAlias 插一条入站别名。
//
// 走 SQL 而不是管理面：这里要的只是「库里有一行」，而写入校验（别名不能与
// 规范名同名、规范名必须存在）由 internal/catalog 的用例钉。
func seedInboundAlias(t *testing.T, db *sql.DB, alias, canonical string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO inbound_model_aliases (alias, canonical_name)
		VALUES ($1, $2)`, alias, canonical); err != nil {
		t.Fatalf("插入入站别名失败: %v", err)
	}
}

// 用别名调一次：上游收到上游名、响应回显别名、账单记规范名。
func TestInboundAliasEndToEnd(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-inbound")
	alias := "short-" + model
	upstreamName := "vendor/" + model

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	// 计价规则与出站映射都挂在**规范名**上：入站别名只是入口那一层的
	// 一个翻译，它不该改变选路与计价的键。
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAlias(t, db, channelID, upstreamName, model)
	seedInboundAlias(t, db, alias, model)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(alias))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 一、上游收到的是上游名。
	if got := sentModel(t, fake, 0); got != upstreamName {
		t.Errorf("上游收到的 model = %q，期望 %q", got, upstreamName)
	}

	// 二、响应回显调用方写的别名。
	var got struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if got.Model != alias {
		t.Errorf("响应里的 model = %q，期望调用方写的 %q", got.Model, alias)
	}

	// 三、账单按规范名记。
	bill := waitForBillingStatus(t, db, user, "settled")
	if bill.canonicalModel != model {
		t.Errorf("账单的 canonical_model = %q，期望规范名 %q：用量按规范名记，"+
			"否则同一份用量会因为客户端写短名还是全名分成两行", bill.canonicalModel, model)
	}
	if !bill.upstreamModel.Valid || bill.upstreamModel.String != upstreamName {
		t.Errorf("账单的 upstream_model = %+v，期望 %q", bill.upstreamModel, upstreamName)
	}
}

// 别名表为空时网关行为与今天完全一致。
//
// 这一条看起来与上面重复，其实不是：上面证明「接上之后能解析」，这一条证明
// 「没配别名时那一次查询不会改变任何东西」——包括不会把请求拒掉。表里一条
// 都没有是大多数部署的样子，它必须逐字照旧能打通。
func TestInboundAliasTableEmptyKeepsBehaviour(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-inbound-empty")
	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（别名表为空不该改变任何行为）。响应体：%s", status, body)
	}
	if got := sentModel(t, fake, 0); got != model {
		t.Errorf("上游收到的 model = %q，期望原样 %q", got, model)
	}

	var got struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if got.Model != model {
		t.Errorf("响应里的 model = %q，期望 %q", got.Model, model)
	}
}
