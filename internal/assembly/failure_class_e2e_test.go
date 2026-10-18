package assembly

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 上游调用失败的**三类**在调用方那一边必须分得开。
//
// # 它们曾经长得一样
//
// 「一个候选都不剩」（本地判据把候选全挡在门外）、「候选的渠道类型做不了这次
// 请求的协议」、「真上游回了 400」三条路各是一次独立复现，而响应体逐字相同：
//
//	{"message":"upstream call failed","type":"api_error","code":null}
//
// 三类的下一步完全不同（改目录声明 / 换一条类型对的渠道 / 改请求或换上游），
// 而 code 恒为 null 意味着调用方只能开单问人。这条用例就是钉住那个分界。
//
// # 它断言什么，不断言什么
//
// 三条各断言**响应体里的那句话本身**，不是「有个 message 就行」：压平的那句
// 在改动之前也是绿的吗？不是——但只断言状态码的写法在改动之前是绿的（三条都是
// 502），所以判据必须落在码与话上。最后一条把三组 (code, type) 摆在一起比，
// 重复一处就报。
//
// **type 在 OpenAI 形状下分不开这三类**：它由状态码推出来（见 internal/api 的
// errorKind），三条都是 502，于是都是 api_error。分它们的是 code：那一栏才是
// 给调用方分支用的。Anthropic 形状下码写在 error.type 上（那是它唯一的机器可读
// 位置，见 writeErrorCode），所以那一侧连 type 也是不同的；两种协议都钉在
// internal/api 的单测里，这里只走公共入口的 OpenAI 形状。
//
// 钱那条线不在这里：三条路都到不了结算，账与预留的处置由相邻的用例钉着
// （见 TestUpstreamFailureReleasesReservation）。
func TestUpstreamFailureClassesReachTheCaller(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	// 一、一个候选都不剩：目录里声明这个模型**一条端点都不支持**，于是任何
	// 候选在发请求之前就被挡掉（见 route.profileRejection 的 `{}` 那一支）。
	// 端点为空数组而不是「没配过」：后者是未知，放行。
	noEndpointModel := unique("e2e-model-noendpoint")
	seedChannel(t, db, sealer, merchant, "http://127.0.0.1:1", noEndpointModel)
	seedRule(t, db, noEndpointModel, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogEndpoints(t, db, noEndpointModel, `{}`)

	// 二、渠道类型做不了这次请求的协议：systemone 渠道不说 chat 协议，它的
	// 适配器直接回 ErrUnsupported（见 systemoneadapter 的 errNoChat）。这条
	// 候选会真的被发一次（目录里没有它的声明，端点评据放行）。
	typeModel := unique("e2e-model-systemone")
	seedChannelOfType(t, db, sealer, merchant, "http://127.0.0.1:1", typeModel, "systemone", 0)
	seedRule(t, db, typeModel, `{"prompt": 0.00001, "completion": 0.00003}`)

	// 三、真上游回了 400。上游那一档与上面两条本地判定的分开：它说的是
	// 「请求或上游要改」，不是「配置没配好」。
	upstreamModel := unique("e2e-model-upstream400")
	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: upstreamModel}
	fake.fail.Store(true)
	fake.failStatus.Store(http.StatusBadRequest)
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)
	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, upstreamModel)
	seedRule(t, db, upstreamModel, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	cases := []struct {
		name    string
		model   string
		code    string
		message string
	}{
		{
			name:  "一个候选都不剩",
			model: noEndpointModel,
			code:  "no_candidate_channel",
			// 那句**日志里**的话（route.Errors.Error）说的是「N 个候选渠道都
			// 不支持这次请求」；调用方拿到的是这一句，说的是同一件事，多了
			// 「上游一次都没被调用」——那正是它与上面第二条的分界。
			message: "no candidate channel supports this request: " +
				"the model profile rejected every candidate for this model, so no upstream was called",
		},
		{
			name:  "渠道类型做不了这次协议",
			model: typeModel,
			code:  "channel_type_unsupported",
			message: "no candidate channel type supports this protocol: " +
				"every candidate for this model was tried and rejected because its channel type cannot serve this request",
		},
		{
			name:    "上游回了 400",
			model:   upstreamModel,
			code:    "upstream_http_error",
			message: "upstream call failed",
		},
	}

	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(tc.model))
			if status != http.StatusBadGateway {
				t.Fatalf("状态码 = %d，期望 502。响应体：%s", status, body)
			}

			var got struct {
				Error struct {
					Message string  `json:"message"`
					Type    string  `json:"type"`
					Code    *string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("响应不是 OpenAI 形状的错误体: %v（%s）", err, body)
			}

			// 那句话本身。只断言码的话，「把三类都写成同一句话」照样通过——
			// 而那句话正是调用方唯一读得懂的东西。
			if got.Error.Message != tc.message {
				t.Errorf("error.message = %q，期望 %q", got.Error.Message, tc.message)
			}
			code := ""
			if got.Error.Code != nil {
				code = *got.Error.Code
			}
			if code != tc.code {
				t.Errorf("error.code = %q，期望 %q", code, tc.code)
			}
			if got.Error.Type != "api_error" {
				t.Errorf("error.type = %q，期望 api_error（502 在 OpenAI 形状下的落点）", got.Error.Type)
			}

			pair := fmt.Sprintf("type=%s code=%s", got.Error.Type, code)
			if other, dup := seen[pair]; dup {
				t.Errorf("与「%s」的 error 体分不开（%s）：两类失败的下一步不同", other, pair)
			}
			seen[pair] = tc.name
		})
	}
}

// seedCatalogEndpoints 给一个规范模型插一条目录声明，只填端点那一列。
//
// 能力那六列与两个上限留 NULL（未知，不参与判据），所以这一条声明能判的只有
// 端点——用例要的正是「端点把候选全挡住」这一支。rolled back 不需要：这个包
// 的用例共用一套只读夹具与独立的库，目录多一行不影响别的用例（模型名是
// unique 出来的）。
func seedCatalogEndpoints(t *testing.T, db *sql.DB, model, endpoints string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO model_catalog (canonical_name, display_name, supported_endpoints)
		VALUES ($1, $1, $2::text[])`,
		model, endpoints)
	if err != nil {
		t.Fatalf("插模型目录声明失败: %v", err)
	}
}
