package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 判据：换表发生在一次请求处理**期间**时，那次请求仍用旧代次跑完，下一个请求
// 用新代次。
//
// # 阻塞点
//
// PluginHost 在 NewSession 里被调用，而注册表在那之前已经取到（见
// runRequestHook）。让它在这里停住，就得到一次「手里拿着旧注册表、还没跑完」
// 的在途请求；这时换表才检验得到「在途不受影响」。
//
// # 断言落在金额上
//
// 两代插件的唯一差别是把 max_tokens 补成 10 还是 20，而预留上界只由它决定
// （夹具规则把输入侧定价写成 0）。所以两个金额分别属于哪一代是确定的。
func TestReloadKeepsInFlightRequestOnOldGeneration(t *testing.T) {
	const body = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`

	oldGen := pluginRegistry(t, "", `onRequest(){ return { body: { max_tokens: 10 } }; }`)
	newGen := pluginRegistry(t, "", `onRequest(){ return { body: { max_tokens: 20 } }; }`)

	h := newPluginHarness(t, nil)
	h.handler.Plugins = plugin.NewHolder(oldGen)

	entered := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	h.handler.PluginHost = func(*plugin.Plugin, string, int64) sandbox.Host {
		first.Do(func() {
			close(entered)
			<-release
		})
		return sandbox.Host{}
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- post(h.handler, PathChatCompletions, body)
	}()

	// 请求已经取到旧注册表、停在宿主构造这一步；这时换表。
	<-entered
	h.handler.Plugins.Store(newGen)
	close(release)

	firstRec := <-done
	if firstRec.Code != http.StatusOK {
		t.Fatalf("在途请求状态码 = %d，响应体 %s", firstRec.Code, firstRec.Body)
	}

	// 第二个请求在换表之后开始，该走新的一代。
	secondRec := post(h.handler, PathChatCompletions, body)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("换表后的请求状态码 = %d，响应体 %s", secondRec.Code, secondRec.Body)
	}

	if len(h.reservations.saved) != 2 {
		t.Fatalf("预留落库 %d 次，期望 2 次", len(h.reservations.saved))
	}
	// 10 × 0.000002 = 2000 个最小单位；20 × 0.000002 = 4000 个（见 pluginRule）。
	for i, want := range []money.Amount{money.FromUnits(2000), money.FromUnits(4000)} {
		if got := h.reservations.saved[i].Amount; got != want {
			t.Errorf("第 %d 个请求预留 = %s，期望 %s（在途那个该用旧代次，换表后的用新代次）",
				i+1, got, want)
		}
	}
}
