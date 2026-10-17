package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/route"
	"github.com/sumwai/larkax/internal/systemone"
)

type fakeDecider struct {
	result *gateway.DecideResult
	err    error

	req      pipeline.Request
	decision *systemone.Request
	calls    int
}

func (f *fakeDecider) Decide(_ context.Context, req pipeline.Request, decision *systemone.Request) (*gateway.DecideResult, error) {
	f.calls++
	f.req = req
	f.decision = decision
	return f.result, f.err
}

func fakeDecideResult() *gateway.DecideResult {
	return &gateway.DecideResult{
		Response: &systemone.Response{
			Model: "upstream-decision-model",
			Answers: map[string]systemone.Answer{
				"q_noul": {Type: systemone.TypeNoul, Noul: 0.75},
			},
			Usage: systemone.Usage{InputTokens: 387, OutputTokens: 72},
		},
		UpstreamModel: "upstream-decision-model",
	}
}

// newDecisionHandler 在既有的 handler 上接一个决策执行器。
func newDecisionHandler() (*Handler, *fakeDecider) {
	h, _, _ := newTestHandler()
	d := &fakeDecider{result: fakeDecideResult()}
	h.Decider = d
	return h, d
}

// 这条路径的合法请求体：形状照 issue 里实测的那一份（三个问题、三种类型）。
const validDecisionBody = `{
	"model": "decision-model",
	"state": "工单: 用户说付款后没到账",
	"questions": {
		"q_noul":   {"type": "noul",   "instructions": "这是账单问题吗"},
		"q_choice": {"type": "choice", "instructions": "路由到哪个团队",
		             "criteria": {"billing": "付款相关", "tech": "技术故障"}},
		"q_score":  {"type": "score",  "instructions": "紧急程度",
		             "criteria": ["low", "medium", "high"]}
	}
}`

// 路径要分派到决策那条路上，而不是被当成 chat 或未知路径。
func TestSystemOnePathDispatchesToDecider(t *testing.T) {
	h, d := newDecisionHandler()

	rec := post(h, PathSystemOne, validDecisionBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body.String())
	}
	if d.calls != 1 {
		t.Fatalf("决策执行器被调了 %d 次，期望 1", d.calls)
	}
	if d.decision.State != "工单: 用户说付款后没到账" || len(d.decision.Questions) != 3 {
		t.Errorf("交给执行器的请求不对: %+v", d.decision)
	}

	var got struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type string  `json:"type"`
			Noul float64 `json:"noul"`
		} `json:"answers"`
		Usage struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体解不开: %v\n%s", err, rec.Body.String())
	}
	// 报给调用方的是**他要的那个**模型名，不是上游回报的那个。
	if got.Model != "decision-model" {
		t.Errorf("响应里的 model = %q，期望 decision-model", got.Model)
	}
	if got.Answers["q_noul"].Noul != 0.75 {
		t.Errorf("答案 = %+v", got.Answers)
	}
	if got.Usage.Input != 387 || got.Usage.Output != 72 {
		t.Errorf("用量 = %+v", got.Usage)
	}
}

// 计费请求的形状：协议、上界、规范名、品种、非流式，逐项钉住。
//
// 上界那两项尤其要钉：这条表面没有 max_tokens 一类的参数，上界只能来自
// internal/systemone 的两个常量。谁把它们改成别的数，这里会红。
func TestSystemOneBillingRequest(t *testing.T) {
	h, d := newDecisionHandler()
	h.DefaultAsset = "CNY"

	if rec := post(h, PathSystemOne, validDecisionBody); rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body.String())
	}

	req := d.req
	if req.Protocol != channel.ProtocolSystemOne {
		t.Errorf("协议 = %q，期望 %q", req.Protocol, channel.ProtocolSystemOne)
	}
	if req.MaxTokens != systemone.MaxOutputTokens {
		t.Errorf("输出上界 = %d，期望 %d", req.MaxTokens, systemone.MaxOutputTokens)
	}
	if req.InputTokens != systemone.MaxInputTokens {
		t.Errorf("输入上界 = %d，期望 %d（上游自己的预算，不是按正文估出来的数）",
			req.InputTokens, systemone.MaxInputTokens)
	}
	if req.CanonicalModel != "decision-model" {
		t.Errorf("规范名 = %q", req.CanonicalModel)
	}
	if req.Asset != "CNY" {
		t.Errorf("品种 = %q", req.Asset)
	}
	if req.Streamed {
		t.Error("这条表面从不流式，Streamed 必须是 false")
	}
	if req.RequestID == "" {
		t.Error("请求标识为空：它是幂等键，也是事后查日志的入口")
	}
}

// 带 stream 的请求要**拒掉**，而不是忽略它：忽略的结果是调用方按 SSE 去
// 解析一份 JSON，错误出现在他那一侧而且离原因很远。
func TestSystemOneRejectsStream(t *testing.T) {
	h, d := newDecisionHandler()

	body := `{"model":"decision-model","state":"s","stream":true,
		"questions":{"q":{"type":"noul","instructions":"i"}}}`
	rec := post(h, PathSystemOne, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应体 %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stream") {
		t.Errorf("错误信息没点名 stream 字段: %s", rec.Body.String())
	}
	if d.calls != 0 {
		t.Errorf("被拒的请求不该调上游，实际调了 %d 次", d.calls)
	}
}

// 校验失败的措辞是给调用方看的（他改请求就能过），所以要点名出问题的字段。
func TestSystemOneValidation(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "没有模型名",
			body:  `{"state":"s","questions":{"q":{"type":"noul"}}}`,
			field: "model",
		},
		{
			name:  "没有问题",
			body:  `{"model":"m","state":"s","questions":{}}`,
			field: "questions",
		},
		{
			name:  "问题类型不认识",
			body:  `{"model":"m","state":"s","questions":{"q":{"type":"maybe"}}}`,
			field: "questions.q.type",
		},
		{
			name:  "choice 没有选项",
			body:  `{"model":"m","state":"s","questions":{"q":{"type":"choice"}}}`,
			field: "questions.q.criteria",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, d := newDecisionHandler()
			rec := post(h, PathSystemOne, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400；响应体 %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.field) {
				t.Errorf("错误信息没点名 %q: %s", tc.field, rec.Body.String())
			}
			if d.calls != 0 {
				t.Errorf("被拒的请求不该调上游，实际调了 %d 次", d.calls)
			}
		})
	}
}

// 请求体不是合法 JSON 时回一句概括，细节（内部类型名、字段名）只进日志。
func TestSystemOneInvalidBody(t *testing.T) {
	h, _ := newDecisionHandler()
	rec := post(h, PathSystemOne, `{"model":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid request body") {
		t.Errorf("响应体 = %s", rec.Body.String())
	}
}

// 没接上决策执行器时回 501，而不是让请求走到一半失败。
func TestSystemOneUnwired(t *testing.T) {
	h, _, _ := newTestHandler() // Decider 留空
	rec := post(h, PathSystemOne, validDecisionBody)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501；响应体 %s", rec.Code, rec.Body.String())
	}
}

// 解析之前的那些检查与 chat 那条路共用一份（见 admit）：方法、Content-Type、
// 鉴权。这里逐条确认决策这条路径也走它们。
func TestSystemOneSharesThePreChecks(t *testing.T) {
	h, d := newDecisionHandler()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, PathSystemOne, nil)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET 的状态码 = %d，Allow = %q", rec.Code, rec.Header().Get("Allow"))
	}

	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, PathSystemOne,
		strings.NewReader(validDecisionBody))
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("Content-Type 不对时状态码 = %d，期望 415", rec.Code)
	}

	auth := &fakeAuth{err: errors.New("库里没有这个哈希")}
	auth.user = User{ID: 7}
	h2 := New(auth, &fakeChatter{result: fakeResult()})
	h2.Decider = d
	h2.DefaultAsset = "CNY"
	rec = post(h2, PathSystemOne, validDecisionBody)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("鉴权查询失败时状态码 = %d，期望 500", rec.Code)
	}
	if d.calls != 0 {
		t.Errorf("没通过鉴权的请求不该调上游，实际调了 %d 次", d.calls)
	}
}

// 上游那一侧的失败按既有映射回码：没有可用渠道是 503，缺上界是 400。
func TestSystemOneUpstreamErrorStatus(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"没有可用渠道", route.ErrNoCandidates, http.StatusServiceUnavailable},
		{"缺上界", billing.ErrNoUpperBound, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, d := newDecisionHandler()
			d.err = tc.err
			rec := post(h, PathSystemOne, validDecisionBody)
			if rec.Code != tc.status {
				t.Fatalf("状态码 = %d，期望 %d；响应体 %s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

// 拿到没有响应体的结果只可能是装配错误。
func TestSystemOneResultWithoutResponse(t *testing.T) {
	h, d := newDecisionHandler()
	d.result = &gateway.DecideResult{}
	rec := post(h, PathSystemOne, validDecisionBody)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
}

// 错误体用 OpenAI 形状：这条表面没有自己的错误体形状，取两套里更通用的
// 那一套（与未知路径那一支同一个选择）。
func TestSystemOneErrorShape(t *testing.T) {
	h, _ := newDecisionHandler()
	rec := post(h, PathSystemOne, `{"model":"m","state":"s","questions":{}}`)

	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 OpenAI 形状: %v\n%s", err, rec.Body.String())
	}
	if body.Error.Message == "" || body.Error.Type == "" {
		t.Errorf("错误体缺字段: %s", rec.Body.String())
	}
}
