package plugin

import (
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 这个文件钉住两个**应用入口**（ApplyRequest / ApplyResponse）与响应补丁的
// 落地语义。
//
// 它们为什么不只是「把 OnRequest 的结果转手一下」：失败要记进会话，
// 而 record 与 blocking 都没导出。在包外接一条「应用补丁 + 打日志」的路，
// 等于让「插件改了但没生效」这条静默的错法重新落回地上。

// ── ApplyResponsePatch 的正文语义 ──────────────────────────────────────

func textResponse(blocks ...channel.ContentBlock) *channel.Response {
	return &channel.Response{
		UpstreamModel: "m-1",
		Message:       channel.Message{Role: "assistant", Content: blocks},
		FinishReason:  "stop",
		Usage:         channel.Usage{PromptTokens: 10, CompletionTokens: 5},
	}
}

func texts(blocks []channel.ContentBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Type+":"+b.Text)
	}
	return out
}

// 文本块合成一个，位置取第一块文本的位置；非文本块原样留在原位。
//
// 位置这件事要钉住：把文本块挪到最后，含工具调用的回答会变成
// 「先说工具调用、再说正文」，而调用方按顺序读内容时看到的是另一个故事。
func TestApplyResponsePatchReplacesTextInPlace(t *testing.T) {
	resp := textResponse(
		channel.ContentBlock{Type: "tool_use", ToolName: "search"},
		channel.ContentBlock{Type: "text", Text: "第一段"},
		channel.ContentBlock{Type: "tool_use", ToolName: "fetch"},
		channel.ContentBlock{Type: "text", Text: "第二段"},
	)
	patched := "改写后的整段正文"
	ApplyResponsePatch(resp, &ResponsePatch{Text: &patched})

	want := []string{"tool_use:", "text:改写后的整段正文", "tool_use:"}
	if got := texts(resp.Message.Content); len(got) != len(want) {
		t.Fatalf("内容块 = %v，期望 %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("第 %d 块 = %q，期望 %q（整份：%v）", i, got[i], want[i], got)
			}
		}
	}
	if resp.Message.Content[0].ToolName != "search" || resp.Message.Content[2].ToolName != "fetch" {
		t.Errorf("非文本块该原样留着: %+v", resp.Message.Content)
	}
}

// 一个文本块都没有时追加一块：上游只回了工具调用、插件要补一句说明，
// 是这个字段说得通的用法。
func TestApplyResponsePatchAppendsWhenNoTextBlock(t *testing.T) {
	resp := textResponse(channel.ContentBlock{Type: "tool_use", ToolName: "search"})
	patched := "补一句"
	ApplyResponsePatch(resp, &ResponsePatch{Text: &patched})

	want := []string{"tool_use:", "text:补一句"}
	if got := texts(resp.Message.Content); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("内容块 = %v，期望 %v", got, want)
	}
}

// 补丁里没有 text 时响应一个字都不动：Metadata 不进协议层
// （channel.Response 上没有对应字段，也不该有）。
func TestApplyResponsePatchWithoutTextLeavesResponseAlone(t *testing.T) {
	resp := textResponse(channel.ContentBlock{Type: "text", Text: "原始"})
	ApplyResponsePatch(resp, &ResponsePatch{Metadata: map[string]any{"reason": "ok"}})

	if got := texts(resp.Message.Content); len(got) != 1 || got[0] != "text:原始" {
		t.Errorf("内容块 = %v，期望只有原始那一块", got)
	}
	if resp.Usage.PromptTokens != 10 || resp.FinishReason != "stop" {
		t.Errorf("用量与结束原因不该被补丁碰到: %+v", resp)
	}
}

// 空响应与空补丁都不 panic，也不改动任何东西。
func TestApplyResponsePatchNilSafety(t *testing.T) {
	patched := "x"
	ApplyResponsePatch(nil, &ResponsePatch{Text: &patched})

	resp := textResponse(channel.ContentBlock{Type: "text", Text: "原始"})
	ApplyResponsePatch(resp, nil)
	if got := texts(resp.Message.Content); len(got) != 1 || got[0] != "text:原始" {
		t.Errorf("内容块 = %v，期望只有原始那一块", got)
	}
}

// ── ApplyRequest ──────────────────────────────────────────────────────

// 补丁正常落地：max_tokens 调小、放行的头写进去、被拒的头报出来。
func TestApplyRequestAppliesAndReportsRejectedHeaders(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onRequest(){ return { body: { max_tokens: 10 },
		                       headers: { "x-larkax-tenant": "t7", "authorization": "x" } }; }`,
		sandbox.Host{}, sandbox.Limits{})

	req := &channel.Request{Model: "m", MaxTokens: 1000}
	rejected, err := s.ApplyRequest(t.Context(), "r1", req, "m", "openai")
	if err != nil {
		t.Fatalf("非阻断型插件不该让请求失败: %v", err)
	}
	if req.MaxTokens != 10 {
		t.Errorf("max_tokens = %d，期望 10", req.MaxTokens)
	}
	if got := req.Headers["x-larkax-tenant"]; got != "t7" {
		t.Errorf("放行的头 = %q，期望 t7（整份头：%v）", got, req.Headers)
	}
	// 被拒的头必须报出去：作者写了它却没有任何反馈，会以为是上游不支持。
	if len(rejected) != 1 || rejected[0] != "authorization" {
		t.Errorf("被拒的头 = %v，期望 [authorization]", rejected)
	}
	if len(s.Failures()) != 0 {
		t.Errorf("这一路不该记失败: %v", s.Failures())
	}
}

// 越界补丁在非阻断型插件上：请求继续，补丁整个丢掉，但**失败要记下来**。
//
// 记下来是这条接线的全部意义：不记的话表现是「插件改了但没生效」，
// 而那是契约里最难查的一类静默。
func TestApplyRequestRecordsNonBlockingFailure(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onRequest(){ return { body: { model: "更贵的模型" } }; }`, sandbox.Host{}, sandbox.Limits{})

	req := &channel.Request{Model: "m", MaxTokens: 1000}
	rejected, err := s.ApplyRequest(t.Context(), "r1", req, "m", "openai")
	if err != nil {
		t.Fatalf("非阻断型插件越界不该让请求失败，实际 %v", err)
	}
	if rejected != nil {
		t.Errorf("这一路没有头被拒，实际 %v", rejected)
	}
	// 越界时**整份补丁**都丢掉，而不是只丢掉越界那一项。
	if req.Model != "m" || req.MaxTokens != 1000 {
		t.Errorf("补丁该整个丢掉，实际 req = %+v", req)
	}
	failures := s.Failures()
	if len(failures) != 1 {
		t.Fatalf("该记一条失败，实际 %v", failures)
	}
	if !errors.Is(failures[0], ErrPatchScope) {
		t.Errorf("该认出是越界，实际 kind=%v cause=%v", failures[0].Kind, failures[0].Cause)
	}
	if failures[0].Hook != HookRequest || failures[0].Plugin != "p1" {
		t.Errorf("失败里该带上哪个插件的哪个 hook，实际 %+v", failures[0])
	}
}

// 阻断型插件越界时返回错误，而且请求一个字段都没被改过。
//
// 「一个字段都没被改过」是这里的关键：阻断意味着调用方要把这次请求作废，
// 而一份被改了一半的请求留在调用方手上，会在重试或日志里变成另一种事实。
func TestApplyRequestBlockingReturnsErrorAndLeavesRequestUntouched(t *testing.T) {
	s := sessionFor(t, `key:"p1", apiVersion:1, blocking:true,`,
		`onRequest(){ return { body: { max_tokens: 0 } }; }`, sandbox.Host{}, sandbox.Limits{})

	req := &channel.Request{Model: "m", MaxTokens: 1000}
	_, err := s.ApplyRequest(t.Context(), "r1", req, "m", "openai")
	if err == nil {
		t.Fatal("阻断型插件失败时该返回错误")
	}
	if !errors.Is(err, ErrPatchScope) {
		t.Errorf("错误里该能认出是越界，实际 %v", err)
	}
	if req.Model != "m" || req.MaxTokens != 1000 {
		t.Errorf("补丁该整个丢掉，实际 req = %+v", req)
	}
}

// 插件跑挂了（而不是给了越界补丁）时同样是「这次补丁不生效」。
func TestApplyRequestSwallowsHookFailure(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onRequest(){ throw new Error("我自己挂了"); }`,
		sandbox.Host{}, sandbox.Limits{})

	req := &channel.Request{Model: "m", MaxTokens: 1000}
	_, err := s.ApplyRequest(t.Context(), "r1", req, "m", "openai")
	if err != nil {
		t.Fatalf("非阻断型插件失败不该让请求失败，实际 %v", err)
	}
	if req.MaxTokens != 1000 {
		t.Errorf("补丁不该生效，实际 max_tokens = %d", req.MaxTokens)
	}
	if len(s.Failures()) != 1 {
		t.Errorf("失败要记下来，实际 %v", s.Failures())
	}
}

// 没有 onRequest 的插件什么都不做，也不算失败。
func TestApplyRequestWithoutHook(t *testing.T) {
	s := sessionFor(t, minimalMeta, `onResponse(){ return; }`, sandbox.Host{}, sandbox.Limits{})

	req := &channel.Request{Model: "m", MaxTokens: 1000}
	rejected, err := s.ApplyRequest(t.Context(), "r1", req, "m", "openai")
	if err != nil || rejected != nil {
		t.Fatalf("rejected=%v err=%v，期望都没有", rejected, err)
	}
	if len(s.Failures()) != 0 {
		t.Errorf("没有这个 hook 不该记失败: %v", s.Failures())
	}
}

// ── ApplyResponse ─────────────────────────────────────────────────────

// 补丁落到响应上，元数据**作为返回值**交出来。
//
// 元数据为什么不直接写进响应：channel.Response 是协议转换与渲染的输入，
// 往里加一个插件专用的 map 会让它跟着响应一路走到调用方，而契约里它只是
// 插件留给自己的一段附注。落点是日志与快照，由调用方决定。
func TestApplyResponseAppliesPatchAndReturnsMetadata(t *testing.T) {
	s := sessionFor(t, minimalMeta,
		`onResponse(){ return { text: "改写后的回答", metadata: { reason: "ok" } }; }`,
		sandbox.Host{}, sandbox.Limits{})

	resp := textResponse(channel.ContentBlock{Type: "text", Text: "原始回答"})
	metadata, err := s.ApplyResponse(t.Context(), "r1", resp)
	if err != nil {
		t.Fatalf("非阻断型插件不该让请求失败: %v", err)
	}
	if got := texts(resp.Message.Content); len(got) != 1 || got[0] != "text:改写后的回答" {
		t.Errorf("内容块 = %v，期望改写后的那一块", got)
	}
	if metadata["reason"] != "ok" {
		t.Errorf("元数据 = %v，期望 reason=ok", metadata)
	}
}

// 阻断型插件的 onResponse 失败时返回错误：调用方据此让这次请求失败。
func TestApplyResponseBlockingReturnsError(t *testing.T) {
	s := sessionFor(t, `key:"p1", apiVersion:1, blocking:true,`,
		`onResponse(){ throw new Error("检查没过"); }`, sandbox.Host{}, sandbox.Limits{})

	resp := textResponse(channel.ContentBlock{Type: "text", Text: "原始回答"})
	if _, err := s.ApplyResponse(t.Context(), "r1", resp); err == nil {
		t.Fatal("阻断型插件失败时该返回错误")
	}
	// 失败时补丁不生效——这里连补丁都没有，响应该是原样。
	if got := texts(resp.Message.Content); len(got) != 1 || got[0] != "text:原始回答" {
		t.Errorf("内容块 = %v，期望原样", got)
	}
}

// 空会话与空响应都安全返回：调用方不必在外面判两次。
func TestApplyResponseNilSafety(t *testing.T) {
	var s *Session
	if metadata, err := s.ApplyResponse(t.Context(), "r1", textResponse()); metadata != nil || err != nil {
		t.Errorf("空会话该安全返回，实际 metadata=%v err=%v", metadata, err)
	}
	s = sessionFor(t, minimalMeta, `onResponse(){ return { text: "x" }; }`,
		sandbox.Host{}, sandbox.Limits{})
	if metadata, err := s.ApplyResponse(t.Context(), "r1", nil); metadata != nil || err != nil {
		t.Errorf("空响应该安全返回，实际 metadata=%v err=%v", metadata, err)
	}
}
