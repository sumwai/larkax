package idempotency

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// floatPtr 造一个浮点指针，用来把「未指定」（nil）与「指定为 0」分开。
func floatPtr(v float64) *float64 { return &v }

// baseRequest 造一份**每个进指纹的字段都有非零值**的请求。
//
// 字段覆盖用例从它出发、每次只改一个字段。之所以要求每个字段都非零：
// 若某个字段留了零值，「把它改成另一个零值」与「根本没改」在指纹上一样，
// 那条用例挂掉时会指向被测代码，而真正的原因是用例自己没给值。
//
// 每个字段都带上真实的取值（而不是空串、0、nil），包括 ContentBlock 里
// 图片与工具那几组**互斥**的字段——本包要的是「改了它就变」，不是「这个
// 组合在语义上合法」，所以它们可以同时有值。
func baseRequest() *channel.Request {
	return &channel.Request{
		Model: "gpt-4o",
		Messages: []channel.Message{{
			Role: "user",
			Content: []channel.ContentBlock{{
				Type:           "text",
				Text:           "你好，世界",
				ToolUseID:      "call_01",
				ToolName:       "get_weather",
				ToolInput:      map[string]any{"city": "北京", "unit": "celsius"},
				ImageURL:       "https://api.example.com/image.png",
				ImageBase64:    "aGVsbG8gd29ybGQ=",
				ImageMediaType: "image/png",
			}},
		}},
		MaxTokens:       512,
		Temperature:     floatPtr(0.7),
		TopP:            floatPtr(0.9),
		ReasoningEffort: "medium",
		Tools: []channel.Tool{{
			Name:        "get_weather",
			Description: "查询指定城市的天气",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
		Stream: true,
		Extra:  map[string]any{"seed": 7, "top_k": 40},
	}
}

// ── ValidateKey ─────────────────────────────────────────────────────────

// 幂等键的取值边界。
//
// 上下界用**字面量**（256 / 257、0x21 / 0x7e）而不是 MaxKeyBytes：
// 拿常量当期望值的话，把常量本身改掉用例照样通过，测的是「它等于它自己」。
func TestValidateKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "uuid 形态", key: "550e8400-e29b-41d4-a716-446655440000"},
		{name: "恰好 256 字节", key: strings.Repeat("a", 256)},
		{name: "257 字节超限", key: strings.Repeat("a", 257), wantErr: true},
		{name: "空串", key: "", wantErr: true},
		{name: "含空格", key: "abc def", wantErr: true},
		{name: "含换行", key: "abc\ndef", wantErr: true},
		{name: "含回车", key: "abc\rdef", wantErr: true},
		{name: "含制表符", key: "abc\tdef", wantErr: true},
		{name: "含中文", key: "幂等键", wantErr: true},
		{name: "可打印区间下界 0x21", key: "!"},
		{name: "0x20 空格被拒", key: " ", wantErr: true},
		{name: "可打印区间上界 0x7e", key: "~"},
		{name: "0x7f DEL 被拒", key: "a\x7fb", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKey(tc.key)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateKey(%q) 应当报错", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateKey(%q) 不该报错: %v", tc.key, err)
			}
		})
	}
}

// ── HashRequest：字段覆盖 ───────────────────────────────────────────────

// 每一个进指纹的字段，改它都必须让指纹变。
//
// # 为什么值得逐个字段写一条
//
// 漏掉一个字段的后果不是「指纹算得不准」，而是**同键不同体被当成同一个
// 请求**：第二个请求会拿到第一个的响应。那是这套机制对调用方撒谎，
// 而且看不出来——响应码 200，内容完整，只是答的是别的问题。
//
// 所以断言落在**具体的东西**上：指纹不相等。不是「返回了非 nil」。
func TestHashRequestCoversEveryField(t *testing.T) {
	base, err := HashRequest(channel.ProtocolOpenAI, baseRequest())
	if err != nil {
		t.Fatalf("基准请求算指纹失败: %v", err)
	}

	cases := []struct {
		name     string
		protocol channel.Protocol // 留空表示沿用基准的 openai
		mutate   func(*channel.Request)
	}{
		{
			name:     "Protocol",
			protocol: channel.ProtocolAnthropic,
		},
		{
			name:   "Model",
			mutate: func(r *channel.Request) { r.Model = "gpt-4o-mini" },
		},
		{
			name: "Messages 条数",
			mutate: func(r *channel.Request) {
				r.Messages = append(r.Messages, channel.Message{Role: "assistant"})
			},
		},
		{
			name:   "Messages.Role",
			mutate: func(r *channel.Request) { r.Messages[0].Role = "assistant" },
		},
		{
			name:   "Content.Type",
			mutate: func(r *channel.Request) { r.Messages[0].Content[0].Type = "image" },
		},
		{
			name:   "Content.Text",
			mutate: func(r *channel.Request) { r.Messages[0].Content[0].Text = "你好，世界！" },
		},
		{
			name:   "Content.ToolUseID",
			mutate: func(r *channel.Request) { r.Messages[0].Content[0].ToolUseID = "call_02" },
		},
		{
			name:   "Content.ToolName",
			mutate: func(r *channel.Request) { r.Messages[0].Content[0].ToolName = "get_time" },
		},
		{
			name: "Content.ToolInput",
			mutate: func(r *channel.Request) {
				r.Messages[0].Content[0].ToolInput = map[string]any{"city": "上海", "unit": "celsius"}
			},
		},
		{
			name: "Content.ImageURL",
			mutate: func(r *channel.Request) {
				r.Messages[0].Content[0].ImageURL = "https://api.example.com/other.png"
			},
		},
		{
			name: "Content.ImageBase64",
			mutate: func(r *channel.Request) {
				r.Messages[0].Content[0].ImageBase64 = "Z29vZGJ5ZSB3b3JsZA=="
			},
		},
		{
			name:   "Content.ImageMediaType",
			mutate: func(r *channel.Request) { r.Messages[0].Content[0].ImageMediaType = "image/jpeg" },
		},
		{
			name:   "MaxTokens",
			mutate: func(r *channel.Request) { r.MaxTokens = 1024 },
		},
		{
			name:   "Temperature",
			mutate: func(r *channel.Request) { r.Temperature = floatPtr(0.2) },
		},
		{
			name:   "TopP",
			mutate: func(r *channel.Request) { r.TopP = floatPtr(0.5) },
		},
		{
			name:   "ReasoningEffort",
			mutate: func(r *channel.Request) { r.ReasoningEffort = "high" },
		},
		{
			name: "Tools 条数",
			mutate: func(r *channel.Request) {
				r.Tools = append(r.Tools, channel.Tool{Name: "get_time"})
			},
		},
		{
			name:   "Tools.Name",
			mutate: func(r *channel.Request) { r.Tools[0].Name = "get_time" },
		},
		{
			name:   "Tools.Description",
			mutate: func(r *channel.Request) { r.Tools[0].Description = "查询指定城市的天气（摄氏度）" },
		},
		{
			name: "Tools.InputSchema",
			mutate: func(r *channel.Request) {
				r.Tools[0].InputSchema = map[string]any{"type": "object", "required": []any{"city"}}
			},
		},
		{
			name:   "Stream",
			mutate: func(r *channel.Request) { r.Stream = false },
		},
		{
			name:   "Extra 新增一个键",
			mutate: func(r *channel.Request) { r.Extra["presence_penalty"] = 0.1 },
		},
		{
			name:   "Extra 改一个已有键的值",
			mutate: func(r *channel.Request) { r.Extra["seed"] = 8 },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			if tc.mutate != nil {
				tc.mutate(req)
			}
			protocol := tc.protocol
			if protocol == "" {
				protocol = channel.ProtocolOpenAI
			}

			got, err := HashRequest(protocol, req)
			if err != nil {
				t.Fatalf("算指纹失败: %v", err)
			}
			if got == base {
				t.Errorf("只改了 %s，指纹却没变——这个字段没有进哈希，"+
					"于是同键不同体会被当成同一个请求，第二个请求会拿到第一个的响应", tc.name)
			}
		})
	}
}

// Headers 刻意**不进**指纹，这一条钉住那个决定。
//
// 理由写在 hashInput 的注释里：Headers 不是调用方给的东西，是插件层加上游
// 请求的头；同一份请求经同一批插件得到的头是一样的，它不携带独立信息，
// 而且在入口算指纹的那一刻它还是空的。把它算进去只会让「同一个请求重发」
// 被判成「同键不同体」，幂等键就白设了。
//
// 这一条是防「顺手补全」的：哪天有人觉得字段清单漏了一个把它加进 hashInput，
// 它就该挂。
func TestHashRequestIgnoresHeaders(t *testing.T) {
	without, err := HashRequest(channel.ProtocolOpenAI, baseRequest())
	if err != nil {
		t.Fatalf("算指纹失败: %v", err)
	}

	req := baseRequest()
	req.Headers = map[string]string{"x-larkax-trace": "abc", "x-larkax-plugin": "v1"}
	with, err := HashRequest(channel.ProtocolOpenAI, req)
	if err != nil {
		t.Fatalf("算指纹失败: %v", err)
	}

	if with != without {
		t.Error("Headers 不该进指纹：它是插件层加上游请求的头，" +
			"把它算进去会让同一个请求的重发被判成「同键不同体」")
	}
}

// ── HashRequest：确定性 ─────────────────────────────────────────────────

// 同一份请求两次算出来的指纹必须相同。
//
// # 为什么要跑 20 次
//
// Extra / ToolInput / InputSchema 都是 map，而 Go 的 map 迭代顺序是随机的。
// 一个「按 map 迭代顺序拼字节」的实现，在单次调用里有相当大概率碰巧拼出
// 相同的顺序——所以**跑一次通过说明不了问题**。跑 20 次、每次都从新造的
// 请求出发，才轮到那条实现出错。
func TestHashRequestIsDeterministic(t *testing.T) {
	const rounds = 20

	want, err := HashRequest(channel.ProtocolOpenAI, baseRequest())
	if err != nil {
		t.Fatalf("算指纹失败: %v", err)
	}
	for i := 2; i <= rounds; i++ {
		got, err := HashRequest(channel.ProtocolOpenAI, baseRequest())
		if err != nil {
			t.Fatalf("第 %d 次算指纹失败: %v", i, err)
		}
		if got != want {
			t.Fatalf("第 %d 次算出的指纹与第一次不同：\n  第一次 %x\n  第 %d 次 %x\n"+
				"同一份请求两次算出不同的指纹，会让一次重发被判成「同键不同体」", i, want, i, got)
		}
	}
}

// ── HashRequest：语义等价 ───────────────────────────────────────────────

// 语义相同的两份请求，即使**请求体原文**不同，指纹也必须相同。
//
// 指纹的对象是**解析后**的请求，不是请求体原文（见 hashInput 的注释）：
// 解析器只把管道会用到的字段搬过来，客户端自己填的请求 id、时间戳之类
// 根本进不来。所以下面这两段 JSON 是同一个请求——map 的键序不同、
// 数值写成 0.5 与 0.50——它们必须得到同一个指纹。
func TestHashRequestIsStableAcrossEquivalentBodies(t *testing.T) {
	const bodyA = `{
		"Model": "gpt-4o",
		"Temperature": 0.5,
		"Extra": {"seed": 7, "top_k": 40},
		"Tools": [{"Name": "get_weather", "InputSchema": {"type": "object", "properties": {"city": {"type": "string"}}}}]
	}`
	const bodyB = `{
		"Tools": [{"InputSchema": {"properties": {"city": {"type": "string"}}, "type": "object"}, "Name": "get_weather"}],
		"Extra": {"top_k": 40, "seed": 7},
		"Temperature": 0.50,
		"Model": "gpt-4o"
	}`

	var reqA, reqB channel.Request
	if err := json.Unmarshal([]byte(bodyA), &reqA); err != nil {
		t.Fatalf("解析请求体 A 失败: %v", err)
	}
	if err := json.Unmarshal([]byte(bodyB), &reqB); err != nil {
		t.Fatalf("解析请求体 B 失败: %v", err)
	}

	// 先证明这两段原文**解析出来确实是同一个请求**。少了这一步，
	// 下面那条断言在「两段都解析失败、各自得到一个零值请求」时也会通过。
	if !reflect.DeepEqual(reqA, reqB) {
		t.Fatalf("两段请求体解析出来的结构不同，用例的前提不成立：\n  A %+v\n  B %+v", reqA, reqB)
	}

	hashA, err := HashRequest(channel.ProtocolOpenAI, &reqA)
	if err != nil {
		t.Fatalf("算 A 的指纹失败: %v", err)
	}
	hashB, err := HashRequest(channel.ProtocolOpenAI, &reqB)
	if err != nil {
		t.Fatalf("算 B 的指纹失败: %v", err)
	}
	if hashA != hashB {
		t.Errorf("语义相同的两份请求算出了不同的指纹：\n  A %x\n  B %x\n"+
			"键序与数值写法不该影响指纹——指纹的对象是解析后的请求，不是请求体原文", hashA, hashB)
	}
}

// ── HashRequest：指针语义 ───────────────────────────────────────────────

// Temperature / TopP 的 nil 与 &0 必须得到**不同**的指纹。
//
// 0 是合法取值（「temperature 取 0」是一次明确的采样设定），与「未指定、
// 交给上游默认值」不是一回事——上游对这两种情况的行为不同，所以答案与
// 价格都可能不同。把两者算成同一个请求，等于让一次「temperature=0」的重发
// 拿到第一次（未指定）的响应。
func TestHashRequestDistinguishesZeroFromUnset(t *testing.T) {
	cases := []struct {
		name string
		set  func(*channel.Request, *float64)
	}{
		{name: "Temperature", set: func(r *channel.Request, p *float64) { r.Temperature = p }},
		{name: "TopP", set: func(r *channel.Request, p *float64) { r.TopP = p }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unset := baseRequest()
			tc.set(unset, nil)
			unsetHash, err := HashRequest(channel.ProtocolOpenAI, unset)
			if err != nil {
				t.Fatalf("算「未指定」的指纹失败: %v", err)
			}

			zero := baseRequest()
			tc.set(zero, floatPtr(0))
			zeroHash, err := HashRequest(channel.ProtocolOpenAI, zero)
			if err != nil {
				t.Fatalf("算「取值为 0」的指纹失败: %v", err)
			}

			if zeroHash == unsetHash {
				t.Errorf("%s 的 nil 与 &0 算出了同一个指纹：0 是合法取值，"+
					"与「未指定」不是一回事", tc.name)
			}
		})
	}
}

// ── HashRequest：nil 请求 ───────────────────────────────────────────────

// 请求为 nil 时报错，而不是回落到一个默认指纹、也不是 panic。
//
// 回落会让**所有** nil 请求共用一个指纹，而那意味着「同键不同体」这条判断
// 直接失效。宁可报错。
func TestHashRequestRejectsNilRequest(t *testing.T) {
	// 第一个参数是 channel.Protocol（字符串类型），传不了 nil；
	// 这里连空协议一起覆盖，因为那是它最接近「没有值」的形态。
	for _, protocol := range []channel.Protocol{channel.ProtocolOpenAI, channel.ProtocolAnthropic, ""} {
		got, err := HashRequest(protocol, nil)
		if err == nil {
			t.Errorf("协议 %q 下 HashRequest(protocol, nil) 应当报错，却返回了指纹 %x", protocol, got)
		}
		if got != ([32]byte{}) {
			t.Errorf("报错时不该返回非零指纹: %x", got)
		}
	}
}

// ── Retention ───────────────────────────────────────────────────────────

// 非正的小时数是「不留存」，不是「留到永远」。
//
// 期望值写**字面量**（24 * time.Hour）而不是拿 Retention 自己算一遍：
// 后者的期望与被测来自同一个来源，把 24 改成 15 用例照样通过。
func TestRetention(t *testing.T) {
	cases := []struct {
		name  string
		hours int64
		want  time.Duration
	}{
		{name: "24 小时", hours: 24, want: 24 * time.Hour},
		{name: "1 小时", hours: 1, want: time.Hour},
		{name: "48 小时", hours: 48, want: 48 * time.Hour},
		{name: "0 表示不留存", hours: 0, want: 0},
		{name: "负数表示不留存", hours: -1, want: 0},
		{name: "大负数表示不留存", hours: -24, want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Retention(tc.hours); got != tc.want {
				t.Errorf("Retention(%d) = %s，期望 %s", tc.hours, got, tc.want)
			}
		})
	}
}
