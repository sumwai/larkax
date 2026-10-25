package channel

import "testing"

// 取址只有一处判据：协议自己的地址优先，没有条目（或条目是空串）就退回
// 渠道地址。
//
// 「空条目也算没有」是必要的：列上虽然有 CHECK，读取侧仍要按同一个口径
// 解释——手写进库的一行空串否则会让这一类请求打到一个空地址上。
func TestProtocolBaseURL(t *testing.T) {
	const (
		channelURL   = "https://api.example.com/v1"
		anthropicURL = "https://api.example.com/anthropic"
	)
	overrides := map[Protocol]string{
		ProtocolAnthropic:       anthropicURL,
		ProtocolOpenAIResponses: "",
	}
	cases := []struct {
		name     string
		protocol Protocol
		want     string
	}{
		{"配了专属地址", ProtocolAnthropic, anthropicURL},
		{"没配", ProtocolOpenAI, channelURL},
		{"条目是空串", ProtocolOpenAIResponses, channelURL},
		{"不认识的协议", Protocol("gemini"), channelURL},
		{"空协议名", "", channelURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProtocolBaseURL(tc.protocol, overrides, channelURL); got != tc.want {
				t.Errorf("ProtocolBaseURL(%q) = %q，期望 %q", tc.protocol, got, tc.want)
			}
		})
	}
}

// ForProtocol 改的是副本：同一个渠道会在一次请求里被多条路径读到，
// 一次调用顺手改掉它，后读到的那条路拿到的就是别人的地址。
func TestChannelForProtocolDoesNotMutate(t *testing.T) {
	const (
		channelURL   = "https://api.example.com/v1"
		anthropicURL = "https://api.example.com/anthropic"
	)
	c := Channel{
		BaseURL:          channelURL,
		ProtocolBaseURLs: map[Protocol]string{ProtocolAnthropic: anthropicURL},
	}
	got := c.ForProtocol(ProtocolAnthropic)
	if got.BaseURL != anthropicURL {
		t.Errorf("ForProtocol 之后 BaseURL = %q，期望 %q", got.BaseURL, anthropicURL)
	}
	if c.BaseURL != channelURL {
		t.Errorf("原渠道被改了：BaseURL = %q", c.BaseURL)
	}
}
