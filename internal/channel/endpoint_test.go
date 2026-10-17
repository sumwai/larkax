package channel

import (
	"strings"
	"testing"
)

// 每一个有适配器实现的协议都要给出一条端点。
//
// 少一个的话，那个协议算出来的端点是空串——而空串在端点评据里是「不知道」，
// 处置是**放行**。于是「新增协议时忘了给端点」不会报错，只会让这个协议下的
// 渠道绕过端点判据：往一条这个模型并不在上面被服务的端点发请求，上游回
// 400 unsupported_model，而现场看到的是「模型不存在」。
func TestProtocolEndpoint(t *testing.T) {
	want := map[Protocol]Endpoint{
		ProtocolOpenAI:          EndpointChatCompletions,
		ProtocolAnthropic:       EndpointMessages,
		ProtocolOpenAIResponses: EndpointResponses,
		ProtocolSystemOne:       EndpointSystemOne,
	}
	// 按协议取值清单遍历，不按这张 map：map 里少写一个键，用例会一声不响地
	// 少测一个协议（上面那条注释说的正是这件事）。
	for _, p := range []Protocol{
		ProtocolOpenAI, ProtocolAnthropic, ProtocolOpenAIResponses, ProtocolSystemOne,
	} {
		expected, ok := want[p]
		if !ok {
			t.Fatalf("协议 %q 没有期望的端点：这条用例要跟着新增的协议一起改", p)
		}
		got := p.Endpoint()
		if got != expected {
			t.Errorf("Protocol(%q).Endpoint() = %q，期望 %q", p, got, expected)
		}
		if !got.Valid() {
			t.Errorf("协议 %q 算出的端点 %q 不在合法取值里", p, got)
		}
	}

	// 未知协议给空串，那是「不知道」而不是「一个叫空串的端点」：调用方
	// （route 的端点评据）据此放行。
	if got := Protocol("gemini").Endpoint(); got != "" {
		t.Errorf("未知协议的端点 = %q，期望空串", got)
	}
	if Endpoint("").Valid() {
		t.Error("空串不该是合法端点：它在本包的意思是「不知道」，而 Valid 说的是「是不是那几条之一」")
	}
}

func TestParseEndpoint(t *testing.T) {
	for _, raw := range []string{
		string(EndpointChatCompletions), string(EndpointResponses),
		string(EndpointMessages), string(EndpointSystemOne),
	} {
		got, err := ParseEndpoint(raw)
		if err != nil {
			t.Errorf("ParseEndpoint(%q) 出错: %v", raw, err)
		}
		if string(got) != raw {
			t.Errorf("ParseEndpoint(%q) = %q", raw, got)
		}
	}

	// 解析不出来要报错，**不能**折成空串：空串在端点评据里是「不知道」
	// （放行），而这里要表达的是「这一串是错的」——折平会让一行写错的端点
	// 声明在选路里表现成「这一行没配过」，从此静默不生效。
	for _, raw := range []string{"", "/v1/embeddings", "chat/completions", "openai"} {
		got, err := ParseEndpoint(raw)
		if err == nil {
			t.Errorf("ParseEndpoint(%q) 应报错，得到 %q", raw, got)
			continue
		}
		if got != "" {
			t.Errorf("报错时应当返回空串，得到 %q", got)
		}
		if raw != "" && !strings.Contains(err.Error(), raw) {
			t.Errorf("报错里没提那个非法的取值 %q：%v", raw, err)
		}
	}
}

// 注册表自检：适配器自述的端点必须与它声明的协议算出来的那条一致。
//
// 抄错的表现是静默的——选路按**协议**算出端点去比对目录，而请求实际发去了
// 另一条路径，于是那次比对判的是另一个字符串；上游那边回 404/400，看不出是
// 我们这边配错了。所以挡在注册这一步，进程启动时就报出来。
func TestRegistryRejectsEndpointProtocolMismatch(t *testing.T) {
	r := NewRegistry()
	err := r.Register(&fakeAdapter{
		typ:      "mismatched",
		protocol: ProtocolOpenAI,
		endpoint: EndpointResponses,
	})
	if err == nil {
		t.Fatal("协议说 openai、端点说 responses 时注册应当失败")
	}
	// 报错要同时说出两处各是什么，否则看日志的人不知道该改哪一边。
	for _, want := range []string{
		string(ProtocolOpenAI), string(EndpointResponses), string(EndpointChatCompletions),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没出现 %q：%v", want, err)
		}
	}
	// 失败之后那一条**没有**被注册进去：注册表里留一条自检不过的适配器，
	// 等于把「启动时报错」变成「这台机器上一直有一条配错的渠道」。
	if _, lookupErr := r.Lookup("mismatched"); lookupErr == nil {
		t.Error("自检失败的适配器不该留在注册表里")
	}
}

// 按协议推端点的适配器（真实适配器走的就是这条路）要能通过自检。
func TestRegistryAcceptsMatchingEndpoint(t *testing.T) {
	for _, p := range []Protocol{
		ProtocolOpenAI, ProtocolAnthropic, ProtocolOpenAIResponses, ProtocolSystemOne,
	} {
		r := NewRegistry()
		if err := r.Register(&fakeAdapter{typ: "t-" + string(p), protocol: p}); err != nil {
			t.Errorf("协议 %q 的适配器注册失败: %v", p, err)
		}
	}
}
