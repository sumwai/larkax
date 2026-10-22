package upstream

// 本文件是探测装配的两条判据：清单与运行期同源、时限真的进了客户端。
//
// 适配器本身的出站行为（不跟随重定向、响应体上限）在 adminapi 的端到端
// 用例里；这里只证明「装配读的是同一份清单、用的客户端是 ProbeClient」，
// 那是枚举与写渠道校验不会分叉的唯一依据。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
)

// TestProbeRegistryMatchesRuntimeRegistry 钉住两份注册表逐字相等。
//
// ProbeRegistry 曾经自己列一遍四个适配器：漏列一个时 GET /adapters 少一行，
// 而写渠道的类型校验仍然认它，没有任何用例会红。清单收进 builtinAdapters
// 之后，这条用例防的是「以后又有人在这里加一份」。
func TestProbeRegistryMatchesRuntimeRegistry(t *testing.T) {
	const want = "anthropic,openai,openai-responses,systemone"

	probe, err := ProbeRegistry()
	if err != nil {
		t.Fatalf("ProbeRegistry 出错: %v", err)
	}
	got := strings.Join(probe.Types(), ",")
	if got != want {
		t.Errorf("ProbeRegistry 的类型 = %q，期望内置四项 %q", got, want)
	}

	runtimeRegistry, err := Registry()
	if err != nil {
		t.Fatalf("Registry 出错: %v", err)
	}
	if fromRegistry := strings.Join(runtimeRegistry.Types(), ","); fromRegistry != got {
		t.Errorf("Registry 的类型 = %q，ProbeRegistry 的类型 = %q，两边应当逐字相等",
			fromRegistry, got)
	}

	if fromNew := strings.Join(newExecutor(t).Types(), ","); fromNew != got {
		t.Errorf("New 的类型 = %q，ProbeRegistry 的类型 = %q，两边应当逐字相等",
			fromNew, got)
	}
}

// TestProbeClientCarriesProbeTimeout 钉住探装配用的客户端就是
// ProbeClient(ProbeTimeout)：整体时限是那个常量，且不跟随重定向。
//
// 不真的等十秒：超时是否生效由 adminapi 的端到端用例用注入的短时限客户端
// 观察，这里只判「常量确实进了客户端」。
func TestProbeClientCarriesProbeTimeout(t *testing.T) {
	client := probeClient()
	if client.Timeout != ProbeTimeout {
		t.Errorf("探测客户端的 Timeout = %v，期望 ProbeTimeout 常量 %v",
			client.Timeout, ProbeTimeout)
	}
	if client.Transport == nil {
		t.Fatal("探测客户端没有 Transport：响应体上限与连接时限都不会生效")
	}
	if client.CheckRedirect == nil {
		t.Error("探测客户端会跟随重定向：打的是用户填的地址，跟到别处读到的已是另一个事实")
	}
}

// TestProbeRegistryWiresInjectedClient 用注入的短时限客户端打一个慢上游：
// 装配必须把它原样交给适配器，而不是在 probeRegistry 里另造一个默认客户端。
//
// 证据是耗时：100ms 的客户端让这次调用在一秒内失败；如果适配器拿到的是
// 另造的客户端（比如 ProbeClient(ProbeTimeout)），这里会等满十秒。
func TestProbeRegistryWiresInjectedClient(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	// 先放 handler 出去，再关假上游（defer 是后进先出）。
	defer srv.Close()
	defer close(release)

	registry, err := probeRegistry(httpx.ProbeClient(100 * time.Millisecond))
	if err != nil {
		t.Fatalf("装配探测注册表失败: %v", err)
	}
	adapter, err := registry.Lookup("openai")
	if err != nil {
		t.Fatalf("查 openai 适配器失败: %v", err)
	}

	// 五秒护栏：注入的时限失效时用它把用例停下来，时长断言会先报错。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = adapter.ListModels(ctx, channel.Channel{
		ID: 1, Type: "openai", BaseURL: srv.URL, Credential: "test-key-placeholder",
	})
	if err == nil {
		t.Fatal("上游不回响应头，探测却成功了")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("耗时 %s：注入的短时限客户端没有进到适配器里", elapsed)
	}
}
