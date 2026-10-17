package systemone

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 上界那两个常量必须被钉住。它们是预留的**唯一**依据：这条表面没有
// max_tokens 一类的参数，所以「上界是多大」这件事只写在代码里。
// 把 4096 改成别的数时，这条用例会红，改的人于是必须说明为什么。
//
// 顺带钉住「它是保守值」这件事本身：4096 不是测出来的（上游没给这个数），
// 所以这条用例的注释里也要写清它不是从实测来的——不然下一个人会以为
// 它可以按观测值调小。
func TestOutputUpperBoundIsPinned(t *testing.T) {
	if MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens = %d，期望 4096（上游未给这个数，4096 是保守估计；"+
			"要改它先向上游确认真正的上界）", MaxOutputTokens)
	}
}

// 输入上界取上游文档里较宽的那个口径：state 加全部问题 64K token
// （另一个口径是 state 加最长单个问题 32K）。
func TestInputUpperBoundIsPinned(t *testing.T) {
	if MaxInputTokens != 64*1024 {
		t.Errorf("MaxInputTokens = %d，期望 %d", MaxInputTokens, 64*1024)
	}
}

// 三种问题类型的判据在线上是**两种不同的形状**，而中间表示把它们拆成两个
// 字段。这条用例钉住来回一趟之后形状不变——只钉一边的话，「发出去是对象、
// 收回来当数组」这类错位不会被发现。
func TestQuestionCriteriaRoundTrip(t *testing.T) {
	body := []byte(`{
		"model": "decision-model",
		"state": "工单: 用户说付款后没到账",
		"questions": {
			"q_noul":   {"type": "noul",   "instructions": "这是账单问题吗"},
			"q_choice": {"type": "choice", "instructions": "路由到哪个团队",
			             "criteria": {"billing": "付款相关", "tech": "技术故障"}},
			"q_score":  {"type": "score",  "instructions": "紧急程度",
			             "criteria": ["low", "medium", "high"]}
		}
	}`)

	req, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse 出错: %v", err)
	}
	if req.Model != "decision-model" || req.State != "工单: 用户说付款后没到账" {
		t.Fatalf("顶层字段解错了: %+v", req)
	}
	if got := req.Questions["q_choice"].Choices["billing"]; got != "付款相关" {
		t.Errorf("choice 的判据 = %q，期望 付款相关", got)
	}
	if got := req.Questions["q_score"].Levels; len(got) != 3 || got[0] != "low" || got[2] != "high" {
		t.Errorf("score 的档位 = %v，期望 [low medium high]", got)
	}
	if n := req.Questions["q_noul"]; n.Choices != nil || n.Levels != nil {
		t.Errorf("noul 不该带判据，得到 %+v", n)
	}

	encoded, err := Encode(req)
	if err != nil {
		t.Fatalf("Encode 出错: %v", err)
	}

	// questions 必须是**对象**而不是数组：传数组会被上游拒
	//（Invalid input: expected record, received array）。
	var shape struct {
		Questions map[string]struct {
			Criteria json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(encoded, &shape); err != nil {
		t.Fatalf("编码出来的 questions 不是按 id 索引的对象: %v\n%s", err, encoded)
	}
	if len(shape.Questions) != 3 {
		t.Fatalf("编码后的问题数 = %d，期望 3", len(shape.Questions))
	}

	var choices map[string]string
	if err := json.Unmarshal(shape.Questions["q_choice"].Criteria, &choices); err != nil {
		t.Errorf("choice 的 criteria 编码后不是对象: %v（%s）", err, shape.Questions["q_choice"].Criteria)
	}
	var levels []string
	if err := json.Unmarshal(shape.Questions["q_score"].Criteria, &levels); err != nil {
		t.Errorf("score 的 criteria 编码后不是数组: %v（%s）", err, shape.Questions["q_score"].Criteria)
	}
	if _, present := shape.Questions["q_noul"]; present && shape.Questions["q_noul"].Criteria != nil {
		t.Errorf("noul 不该编码出 criteria，得到 %s", shape.Questions["q_noul"].Criteria)
	}
}

// 判据形状与类型对不上时要报出来，而不是猜一个。
func TestParseRejectsMismatchedCriteria(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"choice 给了数组", `{"model":"m","state":"s","questions":{"q":{"type":"choice","criteria":["a"]}}}`},
		{"score 给了对象", `{"model":"m","state":"s","questions":{"q":{"type":"score","criteria":{"a":"b"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			var invalid *Invalid
			if !errors.As(err, &invalid) {
				t.Fatalf("期望 *Invalid，得到 %v", err)
			}
			if !strings.HasPrefix(invalid.Field, "questions.q.criteria") {
				t.Errorf("报出来的字段 = %q，期望指向 questions.q.criteria", invalid.Field)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name  string
		req   Request
		field string
	}{
		{
			name:  "没有模型名",
			req:   Request{Questions: map[string]Question{"q": {Type: TypeNoul}}},
			field: "model",
		},
		{
			name:  "没有问题",
			req:   Request{Model: "m"},
			field: "questions",
		},
		{
			name: "类型不认识",
			req: Request{Model: "m", Questions: map[string]Question{
				"q": {Type: "noul2"}}},
			field: "questions.q.type",
		},
		{
			name: "类型为空",
			req: Request{Model: "m", Questions: map[string]Question{
				"q": {}}},
			field: "questions.q.type",
		},
		{
			name: "choice 没有选项",
			req: Request{Model: "m", Questions: map[string]Question{
				"q": {Type: TypeChoice}}},
			field: "questions.q.criteria",
		},
		{
			name: "score 没有档位",
			req: Request{Model: "m", Questions: map[string]Question{
				"q": {Type: TypeScore}}},
			field: "questions.q.criteria",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			var invalid *Invalid
			if !errors.As(err, &invalid) {
				t.Fatalf("期望 *Invalid，得到 %v", err)
			}
			if invalid.Field != tc.field {
				t.Errorf("字段 = %q，期望 %q", invalid.Field, tc.field)
			}
		})
	}
}

// state 空不空交给上游判断：按我们的猜测拒收会把上游支持的请求挡在门外
// （与 internal/api 的 checkRequest 同一个取舍）。
func TestValidateLeavesStateToUpstream(t *testing.T) {
	req := Request{Model: "m", Questions: map[string]Question{"q": {Type: TypeNoul}}}
	if err := req.Validate(); err != nil {
		t.Errorf("空 state 不该在这里被拒: %v", err)
	}
}

// 用量的折算是计费的口径，两边必须一一对上：input → prompt、
// output → completion。折错的表现是账单上的输入输出对调，而金额总量
// 在单价相同时完全看不出来。
func TestUsageMapsToPromptAndCompletion(t *testing.T) {
	got := Usage{InputTokens: 387, OutputTokens: 72}.ChannelUsage()
	if got.PromptTokens != 387 || got.CompletionTokens != 72 {
		t.Errorf("折算结果 = %+v，期望 prompt=387 completion=72", got)
	}
	// 这条表面没有缓存读写与推理，三项必须留在零值——填上它们会让计费
	// 从别的桶里算钱。
	if got.CacheReadTokens != 0 || got.CacheWriteTokens != 0 || got.ReasoningTokens != 0 {
		t.Errorf("不该有缓存或推理用量: %+v", got)
	}
	if got.CompletionIncludesReasoning {
		t.Error("这条表面没有把推理算进 output 的证据，CompletionIncludesReasoning 必须为 false")
	}
}

// 响应的三种答案形状都要能解出来，且各带各的字段。
func TestParseResponseKeepsAnswerShapes(t *testing.T) {
	body := []byte(`{
		"model": "decision-model",
		"answers": {
			"q_noul":   {"type":"noul","noul":0.75},
			"q_choice": {"type":"choice","choice":"billing","confidence":1,
			             "probabilities":{"billing":1,"tech":0}},
			"q_score":  {"type":"score","score":1.8,"confidence":0.7,
			             "legend":{"0":"low","1":"medium","2":"high"},
			             "probabilities":{"0":0,"1":0.2,"2":0.8}}
		},
		"usage": {"input_tokens": 387, "output_tokens": 72}
	}`)

	resp, err := ParseResponse(body)
	if err != nil {
		t.Fatalf("ParseResponse 出错: %v", err)
	}
	if resp.Usage.InputTokens != 387 || resp.Usage.OutputTokens != 72 {
		t.Errorf("用量 = %+v", resp.Usage)
	}
	if got := resp.Answers["q_noul"].Noul; got != 0.75 {
		t.Errorf("noul = %v，期望 0.75", got)
	}
	if got := resp.Answers["q_choice"].Choice; got != "billing" {
		t.Errorf("choice = %q，期望 billing", got)
	}
	score := resp.Answers["q_score"]
	if score.Score != 1.8 {
		t.Errorf("score = %v，期望 1.8（它是数值，不是档位下标）", score.Score)
	}
	if score.Legend["2"] != "high" {
		t.Errorf("legend 解错了: %v", score.Legend)
	}
	if score.Probabilities["2"] != 0.8 {
		t.Errorf("probabilities 解错了: %v", score.Probabilities)
	}
}

// 回给调用方的响应是**编回去的**，所以一道 noul 答案不能带上别的类型的
// 空字段：上游的形状里没有它们，而调用方按上游形状写解析器。
func TestEncodeResponseOmitsForeignFields(t *testing.T) {
	body, err := EncodeResponse(&Response{
		Model:   "decision-model",
		Answers: map[string]Answer{"q": {Type: TypeNoul, Noul: 0.75}},
		Usage:   Usage{InputTokens: 10, OutputTokens: 2},
	})
	if err != nil {
		t.Fatalf("EncodeResponse 出错: %v", err)
	}

	var out struct {
		Answers map[string]map[string]any `json:"answers"`
		Usage   map[string]any            `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("编出来的响应解不回来: %v", err)
	}
	answer := out.Answers["q"]
	if len(answer) != 2 {
		t.Errorf("noul 答案的字段 = %v，期望只有 type 与 noul", answer)
	}
	if answer["type"] != TypeNoul || answer["noul"] != 0.75 {
		t.Errorf("答案内容不对: %v", answer)
	}
	if out.Usage["input_tokens"] != float64(10) || out.Usage["output_tokens"] != float64(2) {
		t.Errorf("用量字段名不对: %v", out.Usage)
	}
}

// 上游报的**零值**必须原样回给调用方。
//
// 这是「响应形状与上游一致」这个承诺里最容易破的一处：score 的最低档恰好
// 是 0、noul 可以是 0（「是」的概率为 0）、confidence 也可以是 0。用
// omitempty 表达「这个类型没有这个字段」会把它们一起抹掉——legend 还在、
// score 那个键却消失了，而调用方按上游形状写的严格解析器当场失败。
//
// 判据落在**键在不在**上，不是「值等不等于零」：键不在时用零值读出来也是 0，
// 只看值的话这条用例在坏掉时照样全绿。
func TestEncodeResponseKeepsZeroValues(t *testing.T) {
	upstream := []byte(`{
		"model": "decision-model",
		"answers": {
			"q_noul":   {"type":"noul","noul":0},
			"q_score":  {"type":"score","score":0,"confidence":0,
			             "legend":{"0":"low","1":"high"},
			             "probabilities":{"0":1,"1":0}},
			"q_choice": {"type":"choice","choice":"billing","confidence":0,
			             "probabilities":{"billing":1,"tech":0}}
		},
		"usage": {"input_tokens": 387, "output_tokens": 72}
	}`)

	resp, err := ParseResponse(upstream)
	if err != nil {
		t.Fatalf("ParseResponse 出错: %v", err)
	}
	body, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 出错: %v", err)
	}

	var out struct {
		Answers map[string]map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("编出来的响应解不回来: %v\n%s", err, body)
	}

	for _, tc := range []struct{ id, key string }{
		{"q_noul", "noul"},
		{"q_score", "score"},
		{"q_score", "confidence"},
		{"q_choice", "confidence"},
	} {
		raw, ok := out.Answers[tc.id][tc.key]
		if !ok {
			t.Errorf("%s 的 %q 键不见了——上游报过它，值是 0", tc.id, tc.key)
			continue
		}
		if string(raw) != "0" {
			t.Errorf("%s 的 %q = %s，期望 0", tc.id, tc.key, raw)
		}
	}

	// 与标量同一份答案里的那两个 map 也要在：只保住标量而丢掉 map 是同一类
	// 缺陷的另一半。
	if _, ok := out.Answers["q_score"]["legend"]; !ok {
		t.Errorf("score 的 legend 不见了: %s", body)
	}
	if _, ok := out.Answers["q_score"]["probabilities"]; !ok {
		t.Errorf("score 的 probabilities 不见了: %s", body)
	}
	if _, ok := out.Answers["q_choice"]["probabilities"]; !ok {
		t.Errorf("choice 的 probabilities 不见了: %s", body)
	}
}

// score 是**数值**，我们原样透传，不换算成档位下标。
//
// 上游报的 1.8 落在下标 1 与 2 之间：取整成 2、截断成 1、或者干脆按 legend
// 换成标签，都是把一条上游没做过的判断加到响应里——而调用方按上游形状读，
// 它期望的就是那个小数。这条语义原来只写在 Answer.Score 的注释里，没有用例。
func TestEncodeResponseKeepsScoreAsNumber(t *testing.T) {
	upstream := []byte(`{
		"model": "decision-model",
		"answers": {
			"q_score": {"type":"score","score":1.8,"confidence":0.7,
			            "legend":{"0":"low","1":"medium","2":"high"},
			            "probabilities":{"0":0,"1":0.2,"2":0.8}}
		},
		"usage": {"input_tokens": 387, "output_tokens": 72}
	}`)

	resp, err := ParseResponse(upstream)
	if err != nil {
		t.Fatalf("ParseResponse 出错: %v", err)
	}
	body, err := EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse 出错: %v", err)
	}

	var out struct {
		Answers map[string]struct {
			Score  float64           `json:"score"`
			Legend map[string]string `json:"legend"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("编出来的响应解不回来: %v\n%s", err, body)
	}
	if got := out.Answers["q_score"].Score; got != 1.8 {
		t.Errorf("score = %v，期望 1.8——它是数值，不该被取整成档位下标", got)
	}
	if got := out.Answers["q_score"].Legend["2"]; got != "high" {
		t.Errorf("legend = %v，期望原样透传", out.Answers["q_score"].Legend)
	}
	// 编码成文本之后它仍是一个小数：这一条挡的是「把 1.8 写成字符串或者
	// 换成了标签」这类改法，它们在解成 float64 时都会失败或变成零值。
	if !strings.Contains(string(body), "1.8") {
		t.Errorf("编出来的响应里没有 1.8: %s", body)
	}
}

// stream 只用来识别调用方要了流式，**不编码进上游请求**：入口在更前面就
// 把带 stream 的请求拒了（见 internal/api 的 serveDecision），所以真走到
// 编码这一步时它必须是 false。这条用例钉住「它不会被原样发上去」。
func TestStreamIsNotEncodedUpstream(t *testing.T) {
	req, err := Parse([]byte(`{"model":"m","state":"s","stream":true,
		"questions":{"q":{"type":"noul"}}}`))
	if err != nil {
		t.Fatalf("Parse 出错: %v", err)
	}
	if !req.Stream {
		t.Fatal("stream 没被解出来，入口就无从拒绝")
	}
	req.Stream = false

	encoded, err := Encode(req)
	if err != nil {
		t.Fatalf("Encode 出错: %v", err)
	}
	if strings.Contains(string(encoded), "stream") {
		t.Errorf("编码结果里出现了 stream: %s", encoded)
	}
}

// 按正文拼出来的文本要稳定：顺序不固定的话，同一个请求两次估出来的数
// 不一样，而两个不同的估算值在日志里看起来像两次不同的请求。
func TestTextIsStable(t *testing.T) {
	req := &Request{
		State: "s",
		Questions: map[string]Question{
			"b": {Type: TypeChoice, Instructions: "i2", Choices: map[string]string{"y": "2", "x": "1"}},
			"a": {Type: TypeScore, Instructions: "i1", Levels: []string{"low"}},
		},
	}
	first := req.Text()
	for i := 0; i < 20; i++ {
		if got := req.Text(); got != first {
			t.Fatalf("第 %d 次拼出来的不一样: %q != %q", i, got, first)
		}
	}
	if !strings.Contains(first, "i1") || !strings.Contains(first, "i2") {
		t.Errorf("拼出来的文本漏了问题正文: %q", first)
	}
}

// 这条表面是第三条协议族，账单要按它自己那个取值记——记成 openai 或
// anthropic 的话，事后按协议统计时它会被算进另一条表面。
func TestProtocolValueIsItsOwn(t *testing.T) {
	if channel.ProtocolSystemOne == channel.ProtocolOpenAI ||
		channel.ProtocolSystemOne == channel.ProtocolAnthropic {
		t.Fatalf("systemone 与已有协议重名: %q", channel.ProtocolSystemOne)
	}
}
