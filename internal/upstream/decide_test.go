package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/systemone"
)

func decision() *systemone.Request {
	return &systemone.Request{
		Model: "upstream-model",
		State: "工单: 用户说付款后没到账",
		Questions: map[string]systemone.Question{
			"q_noul": {Type: systemone.TypeNoul, Instructions: "这是账单问题吗"},
		},
	}
}

// 决策请求要按渠道类型分发到 systemone 适配器上：判据是「落在了哪条路径」。
func TestDecideDispatchesToSystemOne(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"model":"upstream-model",
			"answers":{"q_noul":{"type":"noul","noul":0.75}},
			"usage":{"input_tokens":387,"output_tokens":72}}`)
	}))
	defer srv.Close()

	resp, err := newExecutor(t).Decide(context.Background(), testChannel(srv.URL, "systemone"), decision())
	if err != nil {
		t.Fatalf("Decide 出错: %v", err)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("请求路径 = %q，期望 /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer <CHANNEL_CREDENTIAL>" {
		t.Errorf("鉴权头 = %q", gotAuth)
	}
	if resp.Usage.InputTokens != 387 || resp.Usage.OutputTokens != 72 {
		t.Errorf("用量 = %+v", resp.Usage)
	}

	// 发上去的 questions 是**对象**而不是数组。上游对数组直接报错，
	// 所以这一条是「分发对了但形状错了」那类故障的唯一拦路处。
	var sent struct {
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("发上去的 body 解不开: %v\n%s", err, gotBody)
	}
	if len(sent.Questions) != 1 {
		t.Errorf("questions 不是按 id 索引的对象: %s", gotBody)
	}
}

// chat 那两套协议的适配器不实现决策。错误要包着 ErrUnsupported，
// 这样路由层不换下一个候选——换谁都是同一个错。
func TestDecideRejectsChatOnlyChannels(t *testing.T) {
	for _, typ := range []string{"openai", "anthropic"} {
		t.Run(typ, func(t *testing.T) {
			_, err := newExecutor(t).Decide(context.Background(), testChannel("http://127.0.0.1:1", typ), decision())
			if !errors.Is(err, channel.ErrUnsupported) {
				t.Fatalf("错误 = %v，期望包着 ErrUnsupported", err)
			}
			// 「这个类型不支持决策」与「库里那条配置不认识」必须分得开：
			// 前者是设计如此，后者是回滚版本之后的残留。
			if errors.Is(err, channel.ErrUnknownType) {
				t.Errorf("不该报成未知类型: %v", err)
			}
		})
	}
}

// 库里有一条当前二进制不认识的渠道类型时，错误要包着 ErrUnknownType。
func TestDecideReportsUnknownType(t *testing.T) {
	_, err := newExecutor(t).Decide(context.Background(), testChannel("http://127.0.0.1:1", "no-such-type"), decision())
	if !errors.Is(err, channel.ErrUnknownType) {
		t.Fatalf("错误 = %v，期望包着 ErrUnknownType", err)
	}
}
