package api

import (
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 入口必须把块序列填进计费请求。
//
// 漏填的表现是**静默退化**：pipeline.Request.CachePrefix 恒为 nil，预留永远
// 退回按整段输入估——全仓没有任何一步会失败，而这条特性等于没上线。
// 这条用例就是那个缺口的门（见 Request.CachePrefix 的注释）。
//
// 块数按粒度算：tools 一块、每条 message 各一块（openai 系按原序，system 不提权）。
func TestBillingRequestCarriesCachePrefixBlocks(t *testing.T) {
	body := `{"model":"m-1","max_tokens":16,"messages":[` +
		`{"role":"system","content":"你是助手"},` +
		`{"role":"user","content":"你好"},` +
		`{"role":"assistant","content":"在"},` +
		`{"role":"user","content":"继续"}],` +
		`"tools":[{"type":"function","function":{"name":"search","description":"查资料",` +
		`"parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`
	upstream := parseChat(t, body)

	h := &Handler{DefaultAsset: "CNY"}
	got := h.billingRequest(User{ID: 1, KeyID: 2}, upstream, "req-1", channel.ProtocolOpenAI)

	if got.CachePrefix == nil {
		t.Fatal("计费请求没有带块序列——预留会静默退回旧口径")
	}
	// tools 1 + 四条 message 4。
	if len(got.CachePrefix) != 5 {
		t.Errorf("块数 = %d，期望 5（tools 1 + message 4）", len(got.CachePrefix))
	}
}
