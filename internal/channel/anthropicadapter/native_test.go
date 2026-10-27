package anthropicadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 真实的 Anthropic Messages 请求体形状：thinking、cache_control、top_k、
// stop_sequences 与 system 数组都是同一份报文里会一起出现的东西。
//
// 它是两条分支对照的**同一份输入**：直通把它当报文来源，枢纽把它丢掉。
const nativeRequestBody = `{
  "model": "claude-sonnet-4-5",
  "max_tokens": 1024,
  "stream": false,
  "temperature": 0.7,
  "top_p": 0.9,
  "top_k": 40,
  "stop_sequences": ["\n\nHuman:"],
  "system": [
    {"type": "text", "text": "你是助手。", "cache_control": {"type": "ephemeral"}}
  ],
  "thinking": {"type": "enabled", "budget_tokens": 2048},
  "metadata": {"user_id": "user@example.com"},
  "messages": [
    {"role": "user", "content": [
      {"type": "text", "text": "北京天气怎么样？", "cache_control": {"type": "ephemeral"}}
    ]},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "toolu_01", "name": "get_weather",
       "input": {"city": "北京"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01", "content": "晴，22 度"}
    ]}
  ],
  "tools": [
    {"name": "get_weather", "description": "查天气",
     "input_schema": {"type": "object", "properties": {"city": {"type": "string"}},
                      "required": ["city"]}}
  ]
}`

// 直通分支要覆写的键。比对原文与直通报文之前两边各剥一次：
// 剩下的部分必须逐字段相等，那才叫「只改了必须由网关决定的字段」。
var nativeOverriddenKeys = []string{"model", "max_tokens", "stream"}

func decodeJSONMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("解析报文失败: %v（%s）", err, body)
	}
	return m
}

func withoutKeys(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// captureRequest 起一个上游，把收到的请求体与头记下来。
func captureRequest(t *testing.T) (url string, gotBody *[]byte, gotHeader *http.Header) {
	t.Helper()
	body := new([]byte)
	header := new(http.Header)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = b
		*header = r.Header.Clone()
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, body, header
}

// 直通：原文是报文来源，除覆写项外逐字段与原文相等，哨兵一个不少。
//
// 用 DeepEqual 比整张 map 而不是逐个字段查：逐字段查只钉住了点名的那几个，
// 而「只改必须改的」这句话的反面是「其余的**每一个**键都还在、值都没变」。
func TestNativeEncodeIsFieldEquivalent(t *testing.T) {
	url, gotBody, gotHeader := captureRequest(t)
	ch := testChannel(url)
	req := &channel.Request{
		Model:      "claude-sonnet-4-5-20250929", // 到适配器时已是上游名
		MaxTokens:  512,
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(nativeRequestBody),
		Headers:    map[string]string{"X-Custom-Tenant": "t1"},
	}

	if _, err := newAdapter().Chat(context.Background(), ch, req); err != nil {
		t.Fatalf("出错: %v", err)
	}

	sent := decodeJSONMap(t, *gotBody)
	orig := decodeJSONMap(t, []byte(nativeRequestBody))

	// 覆写项按网关的决定写进去了。
	if sent["model"] != "claude-sonnet-4-5-20250929" {
		t.Errorf("model = %v，期望上游名", sent["model"])
	}
	if sent["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v，期望 512", sent["max_tokens"])
	}
	if sent["stream"] != false {
		t.Errorf("stream = %v，期望 false", sent["stream"])
	}

	// 剥掉覆写项之后，其余的**每一个字段**都要与原文相等。
	// 原文重排成 map 之后键序与排版都消失了，比较的是语义。
	if got, want := withoutKeys(sent, nativeOverriddenKeys...), withoutKeys(orig, nativeOverriddenKeys...); !reflect.DeepEqual(got, want) {
		t.Errorf("除覆写项外与原文不等：\n得到 %v\n期望 %v", got, want)
	}

	// 哨兵字段点名断言，读失败时能直接看出丢的是哪一个。
	for _, key := range []string{"thinking", "top_k", "stop_sequences", "metadata"} {
		if !reflect.DeepEqual(sent[key], orig[key]) {
			t.Errorf("%s 丢失或被改：得到 %v，原文 %v", key, sent[key], orig[key])
		}
	}
	// cache_control 嵌在 system 与 messages 里，不是顶层键。
	if !reflect.DeepEqual(sent["system"], orig["system"]) {
		t.Errorf("system（含 cache_control）被改：%v", sent["system"])
	}

	// 直通不改变请求头的裁决：x-api-key 来自渠道，自定义头照常附加。
	if got := gotHeader.Get("x-api-key"); got != "placeholder-credential" {
		t.Errorf("x-api-key = %q，直通分支也必须用渠道凭证", got)
	}
	if got := gotHeader.Get("anthropic-version"); got != AnthropicVersion {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := gotHeader.Get("X-Custom-Tenant"); got != "t1" {
		t.Errorf("自定义头没到上游: %q", got)
	}
}

// 枢纽：同一份 req，只是没有原文。哨兵字段必须**确实不在**——
// 这条断言同时证明上一条用例能分辨两条分支，不是恒真。
func TestHubEncodeDropsUnrecognizedFields(t *testing.T) {
	url, gotBody, _ := captureRequest(t)
	req := &channel.Request{
		Model:     "claude-sonnet-4-5-20250929",
		MaxTokens: 512,
		Protocol:  channel.ProtocolAnthropic,
		// NativeBody 为 nil：统一字段那条路。
	}

	if _, err := newAdapter().Chat(context.Background(), testChannel(url), req); err != nil {
		t.Fatalf("出错: %v", err)
	}

	sent := decodeJSONMap(t, *gotBody)
	for _, key := range []string{"thinking", "top_k", "stop_sequences", "metadata", "system", "tools"} {
		if _, ok := sent[key]; ok {
			t.Errorf("枢纽分支不该带上 %q（统一字段里没有它），实际 %v", key, sent[key])
		}
	}
	// 枢纽分支的报文只有统一字段能表达的那几个键。
	allowed := map[string]bool{"model": true, "max_tokens": true, "messages": true, "stream": true}
	for key := range sent {
		if !allowed[key] {
			t.Errorf("枢纽分支出现意外键 %q：%v", key, sent[key])
		}
	}
}

// 跨协议护栏：带着原文、但入口协议与渠道协议不同，必须走枢纽。
//
// 只判 NativeBody != nil 时这里会把一份 Anthropic 报文发到说别的协议的
// 渠道上，而上游可能恰好接受——那是一次静默发错。
func TestCrossProtocolFallsBackToHub(t *testing.T) {
	url, gotBody, _ := captureRequest(t)
	req := &channel.Request{
		Model:      "claude-sonnet-4-5-20250929",
		MaxTokens:  512,
		Protocol:   channel.ProtocolOpenAI, // 调用方发的是 OpenAI 协议
		NativeBody: []byte(nativeRequestBody),
	}

	if _, err := newAdapter().Chat(context.Background(), testChannel(url), req); err != nil {
		t.Fatalf("出错: %v", err)
	}

	sent := decodeJSONMap(t, *gotBody)
	for _, key := range []string{"thinking", "top_k", "stop_sequences", "system"} {
		if _, ok := sent[key]; ok {
			t.Errorf("跨协议请求走了直通：%q 不该出现在枢纽报文里（%v）", key, sent[key])
		}
	}
	// 上游名与上界照旧覆写，与直通分支一致。
	if sent["model"] != "claude-sonnet-4-5-20250929" {
		t.Errorf("model = %v", sent["model"])
	}
}

// 原文缺 max_tokens 时补默认值：Anthropic 把它当必填，缺了会被拒。
func TestNativeFillsMissingMaxTokens(t *testing.T) {
	url, gotBody, _ := captureRequest(t)
	req := &channel.Request{
		Model:      "m",
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(`{"model":"old","messages":[{"role":"user","content":"hi"}]}`),
	}
	if _, err := newAdapter().Chat(context.Background(), testChannel(url), req); err != nil {
		t.Fatalf("出错: %v", err)
	}
	sent := decodeJSONMap(t, *gotBody)
	if sent["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v，期望补上默认值 %d", sent["max_tokens"], DefaultMaxTokens)
	}
}

// 原文里带着不属于 Anthropic 的字段时原样保留、不翻译。
//
// 最容易误伤的一处是推理：Anthropic 用自己的 thinking 对象表达，没有
// reasoning_effort 这个概念，枢纽那条路编码时也不写它。直通同样不碰——
// 调用方塞了别的协议的键，我们既不删也不改写，因为解释它不是这条分支的活；
// 而 thinking 必须原样在，那才是 Anthropic 认的那一个。
//
// 渠道就算配了 reasoning_effort_map 也不影响：那张表服务的是 OpenAI 一侧的
// 适配器，Anthropic 报文里没有它的适用面。
func TestNativeKeepsForeignFieldsUntranslated(t *testing.T) {
	url, gotBody, _ := captureRequest(t)
	ch := testChannel(url)
	ch.Setting = map[string]any{
		string(channel.SettingKeyReasoningEffortMap): map[string]any{
			"high": "max",
			"low":  "",
		},
	}
	req := &channel.Request{
		Model:     "m",
		MaxTokens: 1,
		Protocol:  channel.ProtocolAnthropic,
		NativeBody: []byte(`{
			"model": "m",
			"max_tokens": 1,
			"messages": [],
			"thinking": {"type": "enabled", "budget_tokens": 2048},
			"reasoning_effort": "high"
		}`),
	}
	if _, err := newAdapter().Chat(context.Background(), ch, req); err != nil {
		t.Fatalf("出错: %v", err)
	}

	sent := decodeJSONMap(t, *gotBody)
	// 取值的方言映射表不该碰它：既不删，也不改写。
	if got := sent["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v，期望原样保留 high", got)
	}
	// thinking 是 Anthropic 自己的推理表达，必须原样在。
	want := map[string]any{"type": "enabled", "budget_tokens": float64(2048)}
	if got := sent["thinking"]; !reflect.DeepEqual(got, want) {
		t.Errorf("thinking = %v，期望 %v", got, want)
	}
}

// 原文解析不了时直接报错，不静默退回统一字段编码：退回会让「这次走的不是
// 直通」变成一次看不出来的行为变化。
func TestNativeRejectsUnparsableBody(t *testing.T) {
	_, err := newAdapter().encodeRequest(context.Background(), &channel.Request{
		Model:      "m",
		MaxTokens:  1,
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(`{"model":`),
	})
	if err == nil {
		t.Fatal("坏掉的原文应报错")
	}
	if !strings.Contains(err.Error(), "解析原始请求体失败") {
		t.Errorf("错误 = %v，期望指出解析原始请求体失败", err)
	}
}

// 直通时 Raw 是上游原始响应体，dto 不认识的字段必须能看到。
// 同一条响应在枢纽分支上会丢掉它们——两条一起断言才证明这条用例能分辨。
func TestNativeRawKeepsUnrecognizedResponseFields(t *testing.T) {
	const respBody = `{
	  "id": "msg_01XFDUDYJgAACzvnptvVoYEL",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-sonnet-4-5-20250929",
	  "container": {"id": "container_011CXLX661sTp1ZaG1JvJvJv", "expires_at": "2026-01-01T00:00:00Z"},
	  "content": [{"type": "text", "text": "北京晴。"}],
	  "stop_reason": "end_turn",
	  "stop_sequence": "\n\nHuman:",
	  "usage": {
	    "input_tokens": 2095,
	    "cache_creation_input_tokens": 1024,
	    "cache_read_input_tokens": 0,
	    "cache_creation": {"ephemeral_5m_input_tokens": 1024, "ephemeral_1h_input_tokens": 0},
	    "output_tokens": 89,
	    "service_tier": "standard"
	  }
	}`

	run := func(native bool) *channel.Response {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, respBody)
		}))
		defer srv.Close()
		req := &channel.Request{Model: "m", MaxTokens: 10, Protocol: channel.ProtocolAnthropic}
		if native {
			req.NativeBody = []byte(`{"model":"m","max_tokens":10,"messages":[]}`)
		}
		resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), req)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		return resp
	}

	native := run(true)
	hub := run(false)

	// 正文与用量两条路要一样：Raw 的差异不该影响它们。
	if native.Message.Content[0].Text != "北京晴。" || hub.Message.Content[0].Text != "北京晴。" {
		t.Errorf("正文 = %q / %q", native.Message.Content[0].Text, hub.Message.Content[0].Text)
	}
	if native.Usage.PromptTokens != hub.Usage.PromptTokens || native.Usage.CompletionTokens != hub.Usage.CompletionTokens {
		t.Errorf("用量两条路不一致：%+v / %+v", native.Usage, hub.Usage)
	}

	// 直通：dto 不认识的形状都在。
	if native.Raw["container"] == nil {
		t.Errorf("直通 Raw 缺 container：%v", native.Raw)
	}
	if native.Raw["stop_sequence"] != "\n\nHuman:" {
		t.Errorf("直通 Raw 的 stop_sequence = %v", native.Raw["stop_sequence"])
	}
	usage, _ := native.Raw["usage"].(map[string]any)
	if usage == nil || usage["cache_creation"] == nil {
		t.Errorf("直通 Raw 缺 usage.cache_creation：%v", native.Raw["usage"])
	}
	if usage != nil && usage["service_tier"] != "standard" {
		t.Errorf("直通 Raw 的 usage.service_tier = %v", usage["service_tier"])
	}

	// 枢纽：同一份响应，这三处都看不到——证明上一条断言确实在分辨两条路。
	if _, ok := hub.Raw["container"]; ok {
		t.Errorf("枢纽 Raw 不该有 container：%v", hub.Raw)
	}
	if _, ok := hub.Raw["stop_sequence"]; ok {
		t.Errorf("枢纽 Raw 不该有 stop_sequence：%v", hub.Raw)
	}
	hubUsage, _ := hub.Raw["usage"].(map[string]any)
	if hubUsage != nil {
		if _, ok := hubUsage["cache_creation"]; ok {
			t.Errorf("枢纽 Raw 不该有 usage.cache_creation：%v", hubUsage)
		}
	}
}

// 真实形状的 Anthropic SSE：thinking_delta、ping、content_block_stop 都在，
// 工具参数分片也是真实的两段形状。
const nativeSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"usage":{"input_tokens":2095,"cache_creation_input_tokens":1024,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":1024,"ephemeral_1h_input_tokens":0},"output_tokens":1,"service_tier":"standard"}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想一想。"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"北京"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"晴。"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com/a","title":"天气"}}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":89}}

event: message_stop
data: {"type":"message_stop"}
`

// sseDataPayloads 按流式解析的同一口径（trim 掉 "data:" 前缀与两端空白）
// 取出一份 SSE 里所有的 data 载荷，用于与 native 帧逐字比对。
func sseDataPayloads(sse string) []string {
	var out []string
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	return out
}

func collectStream(t *testing.T, req *channel.Request, sse string) []channel.StreamEvent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)
	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), req)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	var out []channel.StreamEvent
	for ev := range events {
		out = append(out, ev)
	}
	return out
}

// 直通流：每一帧 data 载荷都发一条 native 事件，载荷逐字等于原文那一行；
// 归一化事件一个不少、顺序不变；thinking_delta 仍不进 delta，但 native 帧照发。
func TestNativeStreamEmitsRawFrames(t *testing.T) {
	req := &channel.Request{
		Model:      "m",
		MaxTokens:  1024,
		Stream:     true,
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(nativeRequestBody),
	}
	events := collectStream(t, req, nativeSSE)

	var frames []string
	var kinds []string
	var text strings.Builder
	var toolArgs strings.Builder
	var usage *channel.Usage
	var done *channel.StreamEvent
	for _, ev := range events {
		if ev.Kind == "native" {
			if ev.NativeFormat != channel.ProtocolAnthropic {
				t.Errorf("NativeFormat = %q", ev.NativeFormat)
			}
			frames = append(frames, string(ev.Frame))
			kinds = append(kinds, "native")
			continue
		}
		kinds = append(kinds, ev.Kind)
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "tool_use":
			if ev.ToolCall != nil {
				toolArgs.WriteString(ev.ToolCall.Arguments)
			}
		case "usage":
			u := *ev.Usage
			usage = &u
		case "done":
			e := ev
			done = &e
		case "error":
			t.Fatalf("不该有错误: %v", ev.Err)
		}
	}

	want := sseDataPayloads(nativeSSE)
	if len(frames) != len(want) {
		t.Fatalf("native 帧 %d 条，期望 %d 条（每个 data 行一条）", len(frames), len(want))
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("第 %d 帧载荷不等于原文 data 行：\n得到 %s\n期望 %s", i, frames[i], want[i])
		}
	}

	// 归一化事件一条不少：正文、工具参数、用量、结束。
	if text.String() != "北京晴。" {
		t.Errorf("正文 = %q，期望「北京晴。」（thinking_delta 不进正文）", text.String())
	}
	if toolArgs.String() != `{"city":"北京"}` {
		t.Errorf("工具参数 = %q", toolArgs.String())
	}
	if usage == nil || usage.CompletionTokens != 89 || usage.PromptTokens != 3119 {
		t.Errorf("用量 = %+v，期望输入 2095+1024、输出 89", usage)
	}
	if done == nil || done.FinishReason != "tool_use" {
		t.Errorf("done = %+v，期望 stop_reason=tool_use", done)
	}

	// 顺序：每条 native 帧都在它对应的归一化事件**之前**，且相邻。
	// 逐帧核对「native 后面紧跟一条非 native」不足以钉顺序——它抓不到
	// 「先发归一化再发 native」，因为那样会长成同一个序列。所以从 kinds
	// 里找出每一个 native 的位置，再按语义事件的出现次序比对。
	firstDelta := indexOf(kinds, "delta")
	if firstDelta < 1 || kinds[firstDelta-1] != "native" {
		t.Errorf("delta 之前应紧挨一条 native 帧: %v", kinds)
	}
	firstToolStart := indexOf(kinds, "tool_use")
	if firstToolStart < 1 || kinds[firstToolStart-1] != "native" {
		t.Errorf("第一条 tool_use 之前应紧挨一条 native 帧: %v", kinds)
	}
	if idx := indexOf(kinds, "usage"); idx < 1 || kinds[idx-1] != "native" {
		t.Errorf("usage 之前应紧挨一条 native 帧: %v", kinds)
	}
	// message_stop 的那一帧先发 native，然后才是它产出的 usage 与 done；
	// done 紧跟在 usage 之后，两条都属于同一帧。
	if u, d := indexOf(kinds, "usage"), indexOf(kinds, "done"); d != u+1 {
		t.Errorf("done 应紧跟在 usage 之后: %v", kinds)
	}

	// thinking_delta 不产 delta，但它的 native 帧确实在：这正是直通要救的东西。
	var sawThinkingFrame bool
	for _, f := range frames {
		if strings.Contains(f, `"thinking_delta"`) {
			sawThinkingFrame = true
		}
	}
	if !sawThinkingFrame {
		t.Error("thinking_delta 的 native 帧丢了")
	}
	if strings.Contains(text.String(), "想一想") {
		t.Error("thinking 内容不该进 delta")
	}

	// ping 与 content_block_stop 不产归一化事件，但照样有 native 帧。
	var pingFrames int
	for _, f := range frames {
		if f == `{"type":"ping"}` {
			pingFrames++
		}
	}
	if pingFrames != 1 {
		t.Errorf("ping 的 native 帧 %d 条，期望 1 条", pingFrames)
	}
	var blockStopFrames int
	for _, f := range frames {
		if strings.Contains(f, `"content_block_stop"`) {
			blockStopFrames++
		}
	}
	if blockStopFrames != 3 {
		t.Errorf("content_block_stop 的 native 帧 %d 条，期望 3 条", blockStopFrames)
	}
}

// 同一条流、没有原文：一条 native 帧都不该有，归一化事件与直通时完全相同。
// 两条一起看，才能证明 native 帧是叠加而不是替代。
func TestHubStreamEmitsNoNativeFrames(t *testing.T) {
	req := &channel.Request{Model: "m", MaxTokens: 1024, Stream: true, Protocol: channel.ProtocolAnthropic}
	events := collectStream(t, req, nativeSSE)

	var text strings.Builder
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == "native" {
			t.Fatalf("没有原文时不该有 native 帧: %v", ev.Frame)
		}
		if ev.Kind == "delta" {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "北京晴。" {
		t.Errorf("正文 = %q", text.String())
	}
	if indexOf(kinds, "done") < 0 {
		t.Errorf("枢纽流缺 done: %v", kinds)
	}
}

// 跨协议：带着原文但入口协议不同，流式同样不许产 native 帧——
// 那些帧的协议与调用方要的不是同一套。
func TestCrossProtocolStreamEmitsNoNativeFrames(t *testing.T) {
	req := &channel.Request{
		Model:      "m",
		MaxTokens:  1024,
		Stream:     true,
		Protocol:   channel.ProtocolOpenAI,
		NativeBody: []byte(nativeRequestBody),
	}
	for _, ev := range collectStream(t, req, nativeSSE) {
		if ev.Kind == "native" {
			t.Fatalf("跨协议的流不该有 native 帧: %v", ev.Frame)
		}
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

// 坏掉的原文在流式入口同样报错，而不是悄悄退回枢纽。
func TestNativeStreamRejectsUnparsableBody(t *testing.T) {
	_, err := newAdapter().ChatStream(context.Background(), testChannel("https://api.example.com"), &channel.Request{
		Model:      "m",
		MaxTokens:  1,
		Stream:     true,
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(`null`),
	})
	if err == nil {
		t.Fatal("原文是 JSON null 时应报错")
	}
	var up *channel.ErrUpstream
	if errors.As(err, &up) {
		t.Errorf("这是本地原文的问题，不该包成上游错误: %v", err)
	}
}
