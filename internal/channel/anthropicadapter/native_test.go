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

// sseBlocks 把一份 SSE 切成块，返回每块的原始字节：空行分隔之间的全部行
// 用 \n 拼起来，不含结尾空行。连续空行形成的空块不计。
//
// 这是 native 帧的期望值口径：帧是**整块**的原文（含 event: 行、注释行、
// 同块内的多行 data），不是某一条 data 行的载荷。
func sseBlocks(sse string) []string {
	var (
		out   []string
		block []string
	)
	flush := func() {
		if len(block) == 0 {
			return
		}
		out = append(out, strings.Join(block, "\n"))
		block = nil
	}
	for _, line := range strings.Split(sse, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		block = append(block, line)
	}
	flush()
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

// 直通流：每个 SSE 块发一条 native 事件，帧逐字等于该块的原文（含 event: 行）；
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

	want := sseBlocks(nativeSSE)
	if len(frames) != len(want) {
		t.Fatalf("native 帧 %d 条，期望 %d 条（每个非空 SSE 块一条）", len(frames), len(want))
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("第 %d 帧不等于原文那一块：\n得到 %s\n期望 %s", i, frames[i], want[i])
		}
	}

	// 帧里带着块内的 event: 行：这是块模型与前一个逐行模型最显眼的差别。
	// 只断言「载荷相等」的话，两种模型在只有 data 行的流上长得一模一样。
	if !strings.HasPrefix(frames[0], "event: message_start\ndata: ") {
		t.Errorf("首帧应以块内的 event: 行开头：%q", frames[0])
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
		if strings.Contains(f, `{"type":"ping"}`) {
			pingFrames++
		}
	}
	if pingFrames != 1 {
		t.Errorf("ping 块的 native 帧 %d 条，期望 1 条", pingFrames)
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

// 块边界 = 空行分隔的位置。native 帧逐字节等于对应那一段原文：块内的
// `event:` 行、注释行、空 `data:` 行、以及同块内的多行 `data:` 都原样带着走，
// 结尾空行不带；连续空行形成的空块不发帧。
func TestNativeStreamBlocksMatchUpstream(t *testing.T) {
	const sse = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":1}}}

: ping

event: ping
data: {"type":"ping"}


: keep-alive

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}
: 同一块里的注释行也属于这一块

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}
data:

event: message_stop
data: {"type":"message_stop"}
`

	req := &channel.Request{
		Model:      "m",
		MaxTokens:  64,
		Stream:     true,
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(nativeRequestBody),
	}
	events := collectStream(t, req, sse)

	var frames []string
	for _, ev := range events {
		if ev.Kind != "native" {
			continue
		}
		frames = append(frames, string(ev.Frame))
	}

	want := sseBlocks(sse)
	if len(want) != 7 {
		t.Fatalf("这份流应有 7 个非空块，夹具切出 %d 个：%q", len(want), want)
	}
	if len(frames) != len(want) {
		t.Fatalf("native 帧 %d 条，期望 %d 条（= 非空块数）：\n%q", len(frames), len(want), frames)
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("第 %d 帧不等于原文那一块：\n得到 %q\n期望 %q", i, frames[i], want[i])
		}
	}

	// 点名三处容易退化的边界，读失败时能直接看出是哪一条：
	// 1. 只含注释行的块照样发帧，且块内容就是那行注释；
	if frames[1] != ": ping" {
		t.Errorf("注释块 = %q，期望 %q", frames[1], ": ping")
	}
	// 2. event: 行与 data: 行在同一条帧里（按行发帧时它们会被拆开）；
	if frames[2] != `event: ping`+"\n"+`data: {"type":"ping"}` {
		t.Errorf("event+data 块 = %q", frames[2])
	}
	// 3. 空 data 行与别的行同块时随块原样带走，不单独成帧也不被丢弃。
	if !strings.Contains(frames[5], "\ndata:") || !strings.HasSuffix(frames[5], "\ndata:") {
		t.Errorf("含空 data 行的块 = %q，期望它以那条空 data 行结尾", frames[5])
	}
}

// [DONE] 块照发 native 帧，归一化的 done 事件也照发一条：前者是调用方要的
// 原始终止符，后者是结算与快照要的结束标记。
func TestNativeStreamEmitsFrameForDoneBlock(t *testing.T) {
	const sse = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

data: [DONE]
`

	req := &channel.Request{
		Model:      "m",
		MaxTokens:  64,
		Stream:     true,
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(nativeRequestBody),
	}
	events := collectStream(t, req, sse)

	var frames []string
	var doneCount int
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		switch ev.Kind {
		case "native":
			frames = append(frames, string(ev.Frame))
		case "done":
			doneCount++
		}
	}

	if len(frames) != 2 {
		t.Fatalf("native 帧 %d 条，期望 2 条（message_start 块与 [DONE] 块）：%q", len(frames), frames)
	}
	if frames[1] != "data: [DONE]" {
		t.Errorf("[DONE] 块的帧 = %q，期望 %q", frames[1], "data: [DONE]")
	}
	if doneCount != 1 {
		t.Errorf("done 事件 %d 条，期望 1 条", doneCount)
	}
	// 顺序：最后一条 native 帧（[DONE] 那一块）在 done 之前。
	lastNative := indexOf(kinds, "native")
	for i, k := range kinds {
		if k == "native" {
			lastNative = i
		}
	}
	if d := indexOf(kinds, "done"); lastNative < 0 || d < 0 || lastNative > d {
		t.Errorf("[DONE] 的帧应先于 done 事件: %v", kinds)
	}
}

// return 之前必须把攒到的块发出去：message_stop 与 error 两个分支都是
// 「产出收尾事件之后立刻 return」，那一块的帧不能凭空丢。
func TestNativeStreamFlushesBlockBeforeReturn(t *testing.T) {
	t.Run("message_stop", func(t *testing.T) {
		const sse = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

event: message_stop
data: {"type":"message_stop"}
`
		req := &channel.Request{
			Model:      "m",
			MaxTokens:  64,
			Stream:     true,
			Protocol:   channel.ProtocolAnthropic,
			NativeBody: []byte(nativeRequestBody),
		}
		var frames []string
		var kinds []string
		for _, ev := range collectStream(t, req, sse) {
			kinds = append(kinds, ev.Kind)
			if ev.Kind == "native" {
				frames = append(frames, string(ev.Frame))
			}
		}
		if len(frames) != 2 {
			t.Fatalf("native 帧 %d 条，期望 2 条（message_stop 那一块不能因 return 丢掉）：%q", len(frames), frames)
		}
		if !strings.Contains(frames[len(frames)-1], `"message_stop"`) {
			t.Errorf("最后一帧不是 message_stop 那一块：%q", frames[len(frames)-1])
		}
		// 帧先于它产出的 usage 与 done。
		lastNative := -1
		for i, k := range kinds {
			if k == "native" {
				lastNative = i
			}
		}
		if u := indexOf(kinds, "usage"); u < 0 || lastNative > u {
			t.Errorf("message_stop 的帧应先于 usage/done: %v", kinds)
		}
	})

	t.Run("error", func(t *testing.T) {
		const sse = `event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"过载了"}}
`
		req := &channel.Request{
			Model:      "m",
			MaxTokens:  64,
			Stream:     true,
			Protocol:   channel.ProtocolAnthropic,
			NativeBody: []byte(nativeRequestBody),
		}
		var frames []string
		var kinds []string
		for _, ev := range collectStream(t, req, sse) {
			kinds = append(kinds, ev.Kind)
			if ev.Kind == "native" {
				frames = append(frames, string(ev.Frame))
			}
		}
		if len(frames) != 1 {
			t.Fatalf("native 帧 %d 条，期望 1 条（error 那一块不能因 return 丢掉）：%q", len(frames), frames)
		}
		if !strings.HasPrefix(frames[0], "event: error\n") {
			t.Errorf("error 块 = %q", frames[0])
		}
		if indexOf(kinds, "error") < 0 {
			t.Errorf("error 事件丢了: %v", kinds)
		}
	})
}

// 每条 native 帧都在由它解析出的归一化事件之前。
//
// 用一条块的产出各不相同的小流把**整条序列**钉住：有的块产一条事件
// （message_start 产 usage_input）、有的块产两条（message_stop 产 usage 与
// done）、有的块一条都不产（ping）。只断言「帧在事件之前」的话，「先把所有帧
// 发完再逐块解析」这个错误顺序也能过——那种序列里帧同样在事件之前。
func TestNativeStreamFramePrecedesItsEvents(t *testing.T) {
	const sse = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}
`
	req := &channel.Request{
		Model:      "m",
		MaxTokens:  64,
		Stream:     true,
		Protocol:   channel.ProtocolAnthropic,
		NativeBody: []byte(nativeRequestBody),
	}

	var kinds []string
	for _, ev := range collectStream(t, req, sse) {
		kinds = append(kinds, ev.Kind)
	}

	// start 是 pumpResponse 在流最前面发的，不属于任何块。
	want := []string{
		"start",
		"native", "usage_input", // message_start 块
		"native",          // ping 块：不产事件
		"native", "delta", // 第一条正文分片
		"native", "delta", // 第二条正文分片
		"native",                  // message_delta 块：只累计，不产事件
		"native", "usage", "done", // message_stop 块：先帧，再 usage 与 done
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("事件序列不对：\n得到 %v\n期望 %v", kinds, want)
	}
}

// event: 字段按事件（块）累积，不跨块沿用。
//
// 两条块的载荷都没有 type 字段——那才会走到 kind = eventName 那条兜底。
// 第一块的 event: 行给出 content_block_delta，产出一条 delta；第二块没有
// event: 行，块级事件名已重置为空串，兜底也拿不到名字，于是落到「不认识」
// 那一支被忽略。把事件名改回跨块沿用时，第二块会重新产出 delta，这条用例红。
//
// 两种分支（直通 / 枢纽）都要跑：这条判据属于解析，与发不发 native 帧无关。
func TestNativeEventNameDoesNotLeakAcrossBlocks(t *testing.T) {
	const sse = `event: content_block_delta
data: {"index":0,"delta":{"type":"text_delta","text":"你"}}

data: {"index":0,"delta":{"type":"text_delta","text":"好"}}

event: message_stop
data: {"type":"message_stop"}
`

	for _, native := range []bool{true, false} {
		name := "hub"
		if native {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			req := &channel.Request{Model: "m", MaxTokens: 8, Stream: true, Protocol: channel.ProtocolAnthropic}
			if native {
				req.NativeBody = []byte(nativeRequestBody)
			}

			var texts []string
			var kinds []string
			for _, ev := range collectStream(t, req, sse) {
				kinds = append(kinds, ev.Kind)
				if ev.Kind == "delta" {
					texts = append(texts, ev.Text)
				}
			}

			// 第一块的事件名来自它自己的 event: 行。
			if len(texts) != 1 || texts[0] != "你" {
				t.Fatalf("delta 文本 = %q，期望恰好一条「你」（第二块的事件名不该沿用第一块）", texts)
			}
			// 第二块不产 delta：这一条同时挡住「事件名跨块沿用」那条回归。
			if strings.Contains(strings.Join(texts, ""), "好") {
				t.Errorf("第二块产出了 delta：事件名跨块沿用了（%v）", texts)
			}
			// 归一化事件除这条 delta 外仍是 start 与 done；块的帧在直通时才出现。
			if indexOf(kinds, "done") < 0 {
				t.Errorf("done 丢了: %v", kinds)
			}
			if !native && indexOf(kinds, "native") >= 0 {
				t.Errorf("枢纽分支不该有 native 帧: %v", kinds)
			}
		})
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
