package channels

import (
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 多协议渠道的端点判据：只有**每一条**协议都对不上才算对不上。
//
// 这与「一条渠道声明了两种端点，模型挂在其中一条上」是同一句话：模型只要
// 能被其中一条服务，这次写入就是有意义的；拦下它等于把一个能用的配置拒掉。
//
// 单协议那一档一并钉住：它必须与加多协议之前完全一致（同一份判据、同一句
// 话），否则既有渠道的行为会因为一条与它们无关的改动而变化。
func TestEndpointFitForProtocols(t *testing.T) {
	cases := []struct {
		name      string
		protocols []string
		declared  []channel.Endpoint
		want      bool
	}{
		{
			name:      "单协议：对得上",
			protocols: []string{"openai"},
			declared:  []channel.Endpoint{channel.EndpointChatCompletions},
			want:      false,
		},
		{
			name:      "单协议：对不上",
			protocols: []string{"openai"},
			declared:  []channel.Endpoint{channel.EndpointMessages},
			want:      true,
		},
		{
			name:      "单协议：算不出端点（未知，放行）",
			protocols: []string{"no-such-protocol"},
			declared:  []channel.Endpoint{channel.EndpointMessages},
			want:      false,
		},
		{
			name:      "单协议：目录没配过端点（未知，放行）",
			protocols: []string{"openai"},
			declared:  nil,
			want:      false,
		},
		{
			name:      "多协议：其中一条对得上就放行",
			protocols: []string{"openai", "anthropic"},
			declared:  []channel.Endpoint{channel.EndpointMessages},
			want:      false,
		},
		{
			name:      "多协议：一条都对不上",
			protocols: []string{"openai", "anthropic"},
			declared:  []channel.Endpoint{channel.EndpointResponses},
			want:      true,
		},
		{
			name:      "多协议：有一条算不出端点（未知，放行）",
			protocols: []string{"openai", "no-such-protocol"},
			declared:  []channel.Endpoint{channel.EndpointMessages},
			want:      false,
		},
		{
			name:      "多协议：目录明确一条都不支持",
			protocols: []string{"openai", "anthropic"},
			declared:  []channel.Endpoint{},
			want:      true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EndpointFitForProtocols(tc.protocols, tc.declared).Mismatch(); got != tc.want {
				t.Errorf("Mismatch() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 单协议渠道的报错措辞必须与加多协议之前逐字相同。
//
// 这句同时经命令行与管理面返回给使用者，而它此前是按「渠道的类型是 T」写
// 的。多协议后来加了另一句（列出整组协议与端点），但那一句不能把单协议
// 那条路也改掉——单协议的渠道占绝大多数，措辞一变，读日志的人会以为它们
// 的配置也变了。
func TestEndpointFitSummaryWording(t *testing.T) {
	declared := []channel.Endpoint{channel.EndpointChatCompletions}

	single := EndpointFitFor("anthropic", declared).Summary()
	wantSingle := `在目录里声明的端点是 /v1/chat/completions，而这条渠道的类型是 "anthropic"（它把请求发到 /v1/messages）`
	if single != wantSingle {
		t.Errorf("单协议措辞 = %q，期望 %q", single, wantSingle)
	}

	multi := EndpointFitForProtocols([]string{"anthropic", "openai-responses"}, declared).Summary()
	for _, want := range []string{
		`"anthropic"`, `"openai-responses"`,
		string(channel.EndpointMessages), string(channel.EndpointResponses),
		"声明的协议",
	} {
		if !strings.Contains(multi, want) {
			t.Errorf("多协议措辞里没出现 %q：%s", want, multi)
		}
	}
}
