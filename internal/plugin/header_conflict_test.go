package plugin

import (
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel/httpx"
)

// 这个文件钉住的是**三处说法互相矛盾**的一件事，把它从「看起来对」变成
// 「可执行、且当前是红的」。
//
// 三处：
//
//  1. internal/channel/channel.go 的 Request.Headers 注释写着
//     「用途是插件系统里插件加的 x-larkax-* 头」；
//  2. internal/channel/httpx 的 reservedHeaderPrefixes 里有 "x-larkax-"，
//     注释是「自己的头也不该从这条路径进来：那是宿主写的，不是插件」；
//     于是这个前缀的头会被 SafeRequestHeaders 挡掉，**到不了上游**；
//  3. internal/channel/openaiadapter 里有一条叫
//     TestCustomHeadersReachUpstream 的测试，注释说「插件加的 x-larkax-* 头
//     要真能到上游——它存在的全部意义就是这个」，而它实际用的头名是
//     X-Custom-Tenant。**于是测试是绿的，而它宣称的那件事从没被验证过。**
//
// 这三条不可能同时为真。本文件不判谁对，只把事实固定下来：
// 插件层放行 x-larkax-*，而适配器层把它挡掉。等裁决之后，
// 改哪一边都会让这里的一条断言失败，于是那个改动不会悄悄发生。

// TestHeaderPrefixesContradictAdapterFilter 记录当前的实际行为。
//
// 它断言的是**矛盾本身**：插件层放行、适配器层拒绝。这不是「期望的行为」，
// 是「现在的行为」。修好之后这条测试会失败，那时应当重写它。
func TestHeaderPrefixesContradictAdapterFilter(t *testing.T) {
	const name = "x-larkax-tenant"

	// 第一层：插件层的规则（本包）放行它。
	got, rejected := ApplyHeaderPatch(nil, map[string]string{name: "t1"})
	if len(rejected) != 0 {
		t.Fatalf("插件层本该放行 %s，实际拒了：%v", name, rejected)
	}
	if got[name] != "t1" {
		t.Fatalf("插件层没有放行 %s：%v", name, got)
	}

	// 第二层：适配器的规则把同一个头挡掉。
	safe, adapterRejected := httpx.SafeRequestHeaders(map[string]string{name: "t1"})
	if len(safe) != 0 {
		t.Fatalf("适配器层本该拒绝 %s，实际放行了：%v。"+
			"如果这里改了，请回去裁决三处说法，并删掉这段注释所指的矛盾", name, safe)
	}
	if len(adapterRejected) != 1 {
		t.Fatalf("适配器层拒掉的头应有 1 个，实际 %v", adapterRejected)
	}

	// 合起来：插件在 onRequest 里加的头，最终到不了上游。
	t.Logf("矛盾已固定：插件层放行 %s，适配器层拒绝 %v —— 即插件加的头到不了上游", name, adapterRejected)
}

// TestAdapterRejectsEveryPluginHeaderPrefix 钉住「整类头」都被挡，
// 而不只是某一个具体的名字。
//
// 只钉一个名字的话，适配器把前缀规则改成「点名几个头」也能让上一条通过，
// 而那时插件的任意 x-larkax-* 头就都能到上游了——那是一处越权。
func TestAdapterRejectsEveryPluginHeaderPrefix(t *testing.T) {
	names := []string{
		"x-larkax-tenant",
		"x-larkax-internal",
		"X-Larkax-Tenant",
		"x-larkax-",
	}
	custom := make(map[string]string, len(names))
	for _, n := range names {
		custom[n] = "值"
	}
	safe, rejected := httpx.SafeRequestHeaders(custom)
	if len(safe) != 0 {
		t.Errorf("这些头都该被适配器拒，实际放行 %v", safe)
	}
	if len(rejected) != len(names) {
		t.Errorf("被拒的应有 %d 个，实际 %d 个：%v", len(names), len(rejected), rejected)
	}
}

// TestPluginHeaderPrefixIsTheOnlyAcceptedOne 钉住插件层只放行这一个前缀。
//
// 它是契约里写死的：插件冒充宿主或客户端加的头（Authorization 之类）
// 走不到适配器就已经被拒了。适配器那道过滤是最后一道，不是唯一一道。
func TestPluginHeaderPrefixIsTheOnlyAcceptedOne(t *testing.T) {
	cases := map[string]bool{
		"x-larkax-tenant":     true,
		"X-Larkax-Tenant":     true,
		"x-tenant":            false,
		"authorization":       false,
		"Authorization":       false,
		"x-api-key":           false,
		"content-type":        false,
		"":                    false,
		"x-larkax-injected\n": false,
	}
	for name, want := range cases {
		got, rejected := ApplyHeaderPatch(nil, map[string]string{name: "值"})
		accepted := len(got) > 0
		if accepted != want {
			t.Errorf("头 %q：放行=%v，期望 %v（被拒 %v）", name, accepted, want, rejected)
		}
	}
}

// TestHeaderPatchRejectsHeaderInjectionInValue 钉住值里的换行。
//
// 值里带 CRLF 等于能注入出额外的头。它与「名字不合法」是两回事：
// 名字合法、值有毒，照样要拒。
func TestHeaderPatchRejectsHeaderInjectionInValue(t *testing.T) {
	got, rejected := ApplyHeaderPatch(nil, map[string]string{
		"x-larkax-a": "干净",
		"x-larkax-b": "值\r\nX-Evil: 1",
	})
	if len(got) != 1 || got["x-larkax-a"] != "干净" {
		t.Errorf("只该放行干净的那个，实际 %v", got)
	}
	if len(rejected) != 1 || !strings.EqualFold(rejected[0], "x-larkax-b") {
		t.Errorf("注入的那个该被拒，实际 %v", rejected)
	}
}
