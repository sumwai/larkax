package responsesadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// 这个文件钉住同协议直通这条分支：报文以调用方发来的原文为来源，只覆写必须
// 由网关决定的那几处（上游模型名、输出上界、stream、reasoning.effort 的方言）。
//
// 每条断言都成对写：同一份输入在直通与枢纽两条分支上各跑一次，哨兵字段在直通
// 这边还在、在枢纽那边确实不在。只测一边的话，两条分支共同的缺陷会被同一份
// 断言放过——差异只在缝隙里。

// nativeResponsesBody 是一条真实形状的 Responses 请求体。
//
// 字段取自官方请求体的常见组合：input 是条目数组（历史、工具调用与工具结果都在
// 里面）、instructions 是顶层字段、reasoning 是嵌套对象、include / store /
// text / metadata / safety_identifier / prompt_cache_key 这类参数在枢纽那条路
// 上根本没有承载位置。后几个正是哨兵：它们只在直通分支上能活下来。
const nativeResponsesBody = `{
  "model": "alias-model",
  "instructions": "你是助手",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"帮我查一下天气"}]},
    {"type":"function_call","call_id":"call_placeholder","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
    {"type":"function_call_output","call_id":"call_placeholder","output":"晴，26 度"}
  ],
  "reasoning": {"effort": "high", "summary": "auto"},
  "include": ["reasoning.encrypted_content", "message.output_text.logprobs"],
  "store": true,
  "max_output_tokens": 512,
  "temperature": 0.2,
  "top_p": 0.9,
  "parallel_tool_calls": true,
  "tool_choice": "auto",
  "tools": [
    {"type":"function","name":"get_weather","description":"查天气","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}
  ],
  "metadata": {"trace": "trace-placeholder"},
  "truncation": "auto",
  "text": {"format": {"type": "text"}},
  "safety_identifier": "user-placeholder",
  "prompt_cache_key": "cache-key-placeholder",
  "background": false,
  "service_tier": "default"
}`

// nativeRequest 拼一条同协议直通的请求。body 为 nil 时退化成枢纽那条路。
func nativeRequest(body []byte) *channel.Request {
	return &channel.Request{
		Protocol:   channel.ProtocolOpenAIResponses,
		Model:      "upstream-model",
		MaxTokens:  2048,
		Stream:     false,
		Messages:   textRequest().Messages,
		NativeBody: body,
	}
}

// withoutKeys 深拷贝一份解出来的报文再删掉几个键，供「覆写项剥掉之后逐字段
// 比」使用。深拷贝是必须的：直接在原文上删会改掉同一份夹具的下一次比较。
func withoutKeys(t *testing.T, src map[string]any, keys ...string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("夹具无法序列化: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("夹具无法反序列化: %v", err)
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// responseDropSentinels 是问句报文的哨兵：枢纽那条路没有承载它们的位置，
// 直通必须原样留住。
//
// max_output_tokens 不在这张表里，虽然它也在任务书点名的四个哨兵之中：枢纽
// 分支同样会写这个字段名（它本来就是这个协议的上界字段），所以「在不在」
// 分辨不了两条分支。它的判别力体现在**取值来源**上，用 req.MaxTokens == 0
// 时的情形单独钉（见 TestEncodeNativeOutputBoundName）。
var responseDropSentinels = []string{"reasoning", "include", "store"}

// TestEncodeNativeKeepsOriginalFields 是直通分支的主用例。
//
// 判据是「逐字段等价」：把三处必须由网关决定的覆写（model、stream、
// max_output_tokens）从双方各自剥掉之后，两份 map 必须完全相等。只断言几个
// 哨兵字段在不在是证明不了这一点的——一个被改错类型的 temperature、一个被
// 重新排序后仍能解出来的数组，都会从那种断言下溜过去。
func TestEncodeNativeKeepsOriginalFields(t *testing.T) {
	srv, gotBody, _ := captureServer(t, http.StatusOK,
		`{"id":"r1","model":"upstream-model","status":"completed","output":[]}`)

	ch := testChannel(srv.URL)
	if _, err := newAdapter().Chat(context.Background(), ch, nativeRequest([]byte(nativeResponsesBody))); err != nil {
		t.Fatalf("出错: %v", err)
	}

	got := decodeBody(t, []byte(*gotBody))

	var original map[string]any
	if err := json.Unmarshal([]byte(nativeResponsesBody), &original); err != nil {
		t.Fatalf("夹具不是 JSON: %v", err)
	}

	// 覆写项逐一钉住取值。三项都是「必须由网关决定」的：模型名来自选路，
	// 上界来自预留，stream 决定这次走哪条路径。
	if got["model"] != "upstream-model" {
		t.Errorf("model = %v，期望上游名", got["model"])
	}
	if got["max_output_tokens"] != float64(2048) {
		t.Errorf("max_output_tokens = %v，期望 2048", got["max_output_tokens"])
	}
	if got["stream"] != false {
		t.Errorf("stream = %v，期望 false（以调用参数为准）", got["stream"])
	}
	// 错名字一定要消失：Responses 不认 max_tokens，留着它只是给上游一个
	// 不认识的字段。
	if _, stale := got["max_tokens"]; stale {
		t.Error("报文里不该出现 max_tokens")
	}

	// 双向的哨兵断言：直通这边还在且值未变（这一次没有配映射表，所以
	// reasoning 逐字保留）。
	for _, key := range responseDropSentinels {
		want, ok := original[key]
		if !ok {
			t.Fatalf("夹具里没有哨兵 %q", key)
		}
		if !reflect.DeepEqual(got[key], want) {
			t.Errorf("哨兵 %q 被改动了：得到 %#v，期望 %#v", key, got[key], want)
		}
	}
	// 第四个哨兵 max_output_tokens：原文有它，直通这边也必须还在（值被覆写，
	// 所以只看在不在；下面那条逐字段比已经把它剥掉了）。
	if _, present := got["max_output_tokens"]; !present {
		t.Error("直通时 max_output_tokens 不见了")
	}

	overwritten := []string{"model", "stream", "max_output_tokens"}
	want := withoutKeys(t, original, overwritten...)
	withoutOverwrites := withoutKeys(t, got, overwritten...)
	if !reflect.DeepEqual(want, withoutOverwrites) {
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		gotJSON, _ := json.MarshalIndent(withoutOverwrites, "", "  ")
		t.Errorf("剥掉覆写项之后报文不再与原文相等：\n期望\n%s\n得到\n%s", wantJSON, gotJSON)
	}
}

// TestEncodeNativeHubDropsSentinels 是上面那条的对照组：同一份请求，只是把
// NativeBody 置空，走枢纽那条路。
//
// 它证明那些断言真的能分辨两条分支——如果哨兵在枢纽这边也活着，上面那条用例
// 就是恒真的，它守不住任何东西。
func TestEncodeNativeHubDropsSentinels(t *testing.T) {
	srv, gotBody, _ := captureServer(t, http.StatusOK,
		`{"id":"r1","model":"upstream-model","status":"completed","output":[]}`)

	ch := testChannel(srv.URL)
	if _, err := newAdapter().Chat(context.Background(), ch, nativeRequest(nil)); err != nil {
		t.Fatalf("出错: %v", err)
	}

	got := decodeBody(t, []byte(*gotBody))
	for _, key := range responseDropSentinels {
		if _, present := got[key]; present {
			t.Errorf("枢纽分支不该有 %q（有它说明这条用例分辨不了两条分支）", key)
		}
	}
	// 枢纽分支仍要发出上界，只是来源是统一字段而不是原文。
	if got["max_output_tokens"] != float64(2048) {
		t.Errorf("枢纽分支的 max_output_tokens = %v，期望 2048", got["max_output_tokens"])
	}
}

// TestEncodeNativeRequiresSameProtocol 是跨协议护栏：原文在，但入口协议是
// 别的协议时，必须走枢纽。
//
// 只判 NativeBody != nil 会让调用方发的 Anthropic 报文被当成 Responses 报文
// 发出去——上游收到的是一个它读不懂的请求体。
func TestEncodeNativeRequiresSameProtocol(t *testing.T) {
	for _, protocol := range []channel.Protocol{channel.ProtocolOpenAI, channel.ProtocolAnthropic} {
		t.Run(string(protocol), func(t *testing.T) {
			srv, gotBody, _ := captureServer(t, http.StatusOK,
				`{"id":"r1","model":"upstream-model","status":"completed","output":[]}`)

			req := nativeRequest([]byte(nativeResponsesBody))
			req.Protocol = protocol
			if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), req); err != nil {
				t.Fatalf("出错: %v", err)
			}

			got := decodeBody(t, []byte(*gotBody))
			for _, key := range responseDropSentinels {
				if _, present := got[key]; present {
					t.Errorf("跨协议时不该有 %q，走的是枢纽分支", key)
				}
			}
		})
	}
}

// reasoningBody 拼一条只带给定 reasoning 对象的原文。
func reasoningBody(reasoning string) []byte {
	if reasoning == "" {
		return []byte(`{"model":"alias-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	}
	return []byte(`{"model":"alias-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"reasoning":` + reasoning + `}`)
}

// TestEncodeNativeTranslatesNestedReasoningEffort 钉住方言翻译作用在**嵌套的**
// reasoning.effort 上，以及映射到空串时那两处收尾：删掉 effort 键，reasoning
// 因此变空则整个删掉。
//
// 顶层 reasoning_effort 是 chat 的形状，Responses 读不到它——把这个翻译写错
// 位置时，报文里会多出一个谁都不认的字段，而 effort 还是原文那个值。
func TestEncodeNativeTranslatesNestedReasoningEffort(t *testing.T) {
	cases := []struct {
		name     string
		setting  map[string]any
		body     []byte
		wantEff  string // 空串表示 effort 键不该出现
		wantKeep bool   // reasoning 对象整个在不在
	}{
		{
			name:     "表里没有这个取值时原样透传",
			setting:  effortMap(map[string]any{"low": "minimal"}),
			body:     reasoningBody(`{"effort":"high","summary":"auto"}`),
			wantEff:  "high",
			wantKeep: true,
		},
		{
			name:     "配了 high 到 xhigh 时发出 xhigh",
			setting:  effortMap(map[string]any{"high": "xhigh"}),
			body:     reasoningBody(`{"effort":"high","summary":"auto"}`),
			wantEff:  "xhigh",
			wantKeep: true,
		},
		{
			name:     "映射到空串时删掉 effort、留下 summary",
			setting:  effortMap(map[string]any{"high": ""}),
			body:     reasoningBody(`{"effort":"high","summary":"auto"}`),
			wantEff:  "",
			wantKeep: true,
		},
		{
			name:     "映射到空串且 reasoning 因此变空时整个删掉",
			setting:  effortMap(map[string]any{"high": ""}),
			body:     reasoningBody(`{"effort":"high"}`),
			wantEff:  "",
			wantKeep: false,
		},
		{
			name:     "原文没有 reasoning 时不凭空造一个",
			setting:  effortMap(map[string]any{"high": "xhigh"}),
			body:     reasoningBody(""),
			wantEff:  "",
			wantKeep: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, gotBody, _ := captureServer(t, http.StatusOK,
				`{"id":"r1","model":"upstream-model","status":"completed","output":[]}`)

			ch := testChannel(srv.URL)
			ch.Setting = tc.setting
			if _, err := newAdapter().Chat(context.Background(), ch, nativeRequest(tc.body)); err != nil {
				t.Fatalf("出错: %v", err)
			}

			got := decodeBody(t, []byte(*gotBody))
			reasoning, present := got["reasoning"].(map[string]any)
			if present != tc.wantKeep {
				t.Fatalf("reasoning 对象在不在 = %v，期望 %v（报文 %v）", present, tc.wantKeep, got)
			}
			if !present {
				return
			}
			effort, hasEffort := reasoning["effort"]
			if tc.wantEff == "" {
				if hasEffort {
					t.Errorf("effort 应当被删掉，实际是 %v", effort)
				}
			} else if effort != tc.wantEff {
				t.Errorf("effort = %v，期望 %q", effort, tc.wantEff)
			}
			if tc.name == "映射到空串时删掉 effort、留下 summary" && reasoning["summary"] != "auto" {
				t.Errorf("summary = %v，effort 映射掉之后它必须留下", reasoning["summary"])
			}
		})
	}
}

// TestEncodeNativeOutputBoundName 钉住输出上界的字段名与「不补默认值」。
//
// 改名这一步最容易错成「照着 chat 那边的 max_tokens 写」：那时上游会收到一个
// 它不认识的名字，而真正的上界停在原文那个旧值上——两处都不报错。
func TestEncodeNativeOutputBoundName(t *testing.T) {
	base := `{"model":"alias-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`

	cases := []struct {
		name      string
		body      string
		maxTokens int
		wantBound any // nil 表示不该出现 max_output_tokens
	}{
		{
			name:      "原文用了错名字 max_tokens 时删掉它并写上新名字",
			body:      base + `,"max_tokens":99}`,
			maxTokens: 2048,
			wantBound: float64(2048),
		},
		{
			name:      "原文原样带了 max_output_tokens 时覆写成网关定的上界",
			body:      base + `,"max_output_tokens":512}`,
			maxTokens: 2048,
			wantBound: float64(2048),
		},
		{
			name:      "上界为 0 时不补默认值",
			body:      base + `}`,
			maxTokens: 0,
			wantBound: nil,
		},
		{
			name:      "上界为 0 时错名字 max_tokens 照样删掉，也不改写成新名字",
			body:      base + `,"max_tokens":99}`,
			maxTokens: 0,
			wantBound: nil,
		},
		{
			name:      "上界为 0 且原文带了取值时原样留着（网关没有要改的东西）",
			body:      base + `,"max_output_tokens":512}`,
			maxTokens: 0,
			wantBound: float64(512),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, gotBody, _ := captureServer(t, http.StatusOK,
				`{"id":"r1","model":"upstream-model","status":"completed","output":[]}`)

			req := nativeRequest([]byte(tc.body))
			req.MaxTokens = tc.maxTokens
			if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), req); err != nil {
				t.Fatalf("出错: %v", err)
			}

			got := decodeBody(t, []byte(*gotBody))
			if _, stale := got["max_tokens"]; stale {
				t.Errorf("报文里不该出现 max_tokens（得到 %v）", got)
			}
			bound, present := got["max_output_tokens"]
			if tc.wantBound == nil {
				if present {
					t.Errorf("不该出现 max_output_tokens，实际是 %v", bound)
				}
				return
			}
			if bound != tc.wantBound {
				t.Errorf("max_output_tokens = %v，期望 %v", bound, tc.wantBound)
			}
		})
	}
}

// TestEncodeNativeStreamOptionsPassthrough 钉住「stream_options 不新增、
// 原文带了就保留」。
//
// Responses 的用量跟着终态事件来，不需要 chat 那个 opt-in 参数。凭空新增会让
// 上游收到一个它这个表面不认的字段；而调用方自己带的（例如别的兼容层要的子键）
// 则要原样过去。
func TestEncodeNativeStreamOptionsPassthrough(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示 stream_options 不该出现
	}{
		{
			name: "原文没有 stream_options 时不新增",
			body: `{"model":"alias-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			want: nil,
		},
		{
			name: "原文带了 stream_options 时原样保留，子键一个不少",
			body: `{"model":"alias-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
				`"stream_options":{"include_usage":true,"custom_subkey":"keep-me"}}`,
			want: map[string]any{"include_usage": true, "custom_subkey": "keep-me"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, gotBody, _ := captureServer(t, http.StatusOK, `{"id":"r1","model":"m","status":"completed","output":[]}`)
			if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), nativeRequest([]byte(tc.body))); err != nil {
				t.Fatalf("出错: %v", err)
			}

			got := decodeBody(t, []byte(*gotBody))
			if tc.want == nil {
				if v, present := got["stream_options"]; present {
					t.Errorf("不该出现 stream_options，实际是 %v", v)
				}
				return
			}
			if !reflect.DeepEqual(got["stream_options"], tc.want) {
				t.Errorf("stream_options = %#v，期望 %#v", got["stream_options"], tc.want)
			}
		})
	}
}

// nativeRawResponse 是一条真实形状的 Responses 响应体，含几个转换层的 dto
// 不认识的字段：background、service_tier、safety_identifier、text。
//
// 直通时 Raw 是原文解出的 map，这四个都能看到；枢纽那条路经 dto 再序列化，
// 它们已经在那一层丢了。下面用 service_tier 作哨兵。
const nativeRawResponse = `{"id":"resp_placeholder","object":"response","created_at":1700000000,` +
	`"status":"completed","model":"upstream-model","background":false,"service_tier":"default",` +
	`"safety_identifier":"user-placeholder","instructions":"你是助手","max_output_tokens":512,` +
	`"output":[{"type":"message","id":"msg_placeholder","status":"completed","role":"assistant",` +
	`"content":[{"type":"output_text","text":"你好","annotations":[]}]}],` +
	`"parallel_tool_calls":true,"previous_response_id":null,` +
	`"reasoning":{"effort":"high","summary":"auto"},"store":true,"temperature":0.2,` +
	`"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":0.9,` +
	`"truncation":"auto","metadata":{"trace":"trace-placeholder"},` +
	`"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,` +
	`"input_tokens_details":{"cached_tokens":30},"output_tokens_details":{"reasoning_tokens":25}},` +
	`"error":null}`

// TestChatNativeRawKeepsUnknownFields 钉住非流式 Raw 的两种来源。
//
// 直通时它必须是上游响应的原文（不经 dto 裁剪），枢纽时保持现状。判据用
// service_tier：dto.OpenAIResponsesResponse 没有这个字段。
func TestChatNativeRawKeepsUnknownFields(t *testing.T) {
	t.Run("直通时 Raw 里有 dto 不认识的字段", func(t *testing.T) {
		srv, _, _ := captureServer(t, http.StatusOK, nativeRawResponse)
		resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL),
			nativeRequest([]byte(nativeResponsesBody)))
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if got := resp.Raw["service_tier"]; got != "default" {
			t.Errorf("Raw[service_tier] = %v，期望 default（原文没被 dto 裁剪）", got)
		}
		// 另外三个同样不在 dto 里，一并钉住，免得只留一个字段活着。
		for _, key := range []string{"background", "safety_identifier", "text"} {
			if _, present := resp.Raw[key]; !present {
				t.Errorf("Raw 里丢了 %q", key)
			}
		}
		// 归一化那一侧照常工作：Raw 是排查用的，正文与用量不能因此变。
		if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "你好" {
			t.Errorf("正文 = %+v，期望「你好」", resp.Message.Content)
		}
		if resp.Usage.CompletionTokens != 40 || resp.Usage.ReasoningTokens != 25 {
			t.Errorf("用量 = %+v", resp.Usage)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("StatusCode = %d", resp.StatusCode)
		}
	})

	t.Run("枢纽时 Raw 里没有那些字段", func(t *testing.T) {
		srv, _, _ := captureServer(t, http.StatusOK, nativeRawResponse)
		resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL),
			nativeRequest(nil))
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if v, present := resp.Raw["service_tier"]; present {
			t.Errorf("枢纽分支的 Raw 里不该有 service_tier（实际 %v）", v)
		}
	})
}

// dataPayloadOf 取出一行 SSE 帧里的 `data:` 载荷（去掉前缀与首尾空白）。
func dataPayloadOf(t *testing.T, line string) string {
	t.Helper()
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		t.Fatalf("这一行不是 data: 行：%q", line)
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
}

// nativeFrames 从事件流里挑出原生帧，并逐条核对协议标记。
func nativeFrames(t *testing.T, events []channel.StreamEvent) [][]byte {
	t.Helper()
	var out [][]byte
	for _, ev := range events {
		if ev.Kind != "native" {
			continue
		}
		if ev.NativeFormat != channel.ProtocolOpenAIResponses {
			t.Errorf("native 帧的 NativeFormat = %q", ev.NativeFormat)
		}
		out = append(out, ev.Frame)
	}
	return out
}

// TestChatStreamNativeFramesMatchUpstream 把真实上游序列整条喂进直通分支：
// 每一帧原生载荷必须与上游那一行 `data:` 逐字一致，而归一化事件一条不少。
//
// 夹具里既有 `event:` 行也有 `data:` 行：这个包跳过前者（事件名在载荷的 type
// 里），所以原生帧只能由后者的载荷产生，数量是帧数而不是行数的两倍。
func TestChatStreamNativeFramesMatchUpstream(t *testing.T) {
	srv := sseServer(t, realUpstreamStreamFrames...)
	req := nativeRequest([]byte(nativeResponsesBody))
	req.Stream = true

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), req)
	if err != nil {
		t.Fatal(err)
	}
	collected := collect(t, events)

	var wantPayloads []string
	for _, frame := range realUpstreamStreamFrames {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "data:") {
				wantPayloads = append(wantPayloads, dataPayloadOf(t, line))
			}
		}
	}

	frames := nativeFrames(t, collected)
	if len(frames) != len(wantPayloads) {
		t.Fatalf("原生帧 %d 条，上游 data: 行 %d 条", len(frames), len(wantPayloads))
	}
	for i, want := range wantPayloads {
		if string(frames[i]) != want {
			t.Errorf("第 %d 条原生帧 =\n  %s\n期望\n  %s", i+1, frames[i], want)
		}
	}

	// 归一化事件照旧：正文、用量、结束，一条都不能因为加了原生帧而少。
	var text strings.Builder
	var usage *channel.Usage
	finish := ""
	dones := 0
	for _, ev := range collected {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "usage":
			usage = ev.Usage
		case "done":
			finish = ev.FinishReason
			dones++
		}
	}
	if text.String() != "你好" {
		t.Errorf("正文 = %q，期望「你好」", text.String())
	}
	if usage == nil || usage.PromptTokens != 100 || usage.CompletionTokens != 40 || usage.CacheReadTokens != 30 {
		t.Errorf("用量 = %+v", usage)
	}
	if finish != "stop" || dones != 1 {
		t.Errorf("结束原因 = %q，done 事件 %d 条", finish, dones)
	}
}

// TestChatStreamNativeOrderAndHubCounterpart 钉住两件事：原生帧排在对应归一化
// 事件**之前**；以及同一份流走枢纽分支时一条原生帧都不发。
//
// 无对照的话，「原生帧排在哪」这个问题在只有直通分支的用例里看不出来——它总是
// 有帧、总是有 delta，顺序反了也全绿。
func TestChatStreamNativeOrderAndHubCounterpart(t *testing.T) {
	body := realUpstreamStreamFrames

	t.Run("直通时原生帧先于归一化事件", func(t *testing.T) {
		srv := sseServer(t, body...)
		req := nativeRequest([]byte(nativeResponsesBody))
		req.Stream = true
		events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), req)
		if err != nil {
			t.Fatal(err)
		}

		// 正文那一片：它的原生帧必须紧挨在 delta 之前。
		collected := collect(t, events)
		deltaAt := -1
		for i, ev := range collected {
			if ev.Kind == "delta" {
				deltaAt = i
				break
			}
		}
		if deltaAt <= 0 {
			t.Fatalf("找不到 delta（或它排在第一位，前面没有原生帧）：%d", deltaAt)
		}
		if collected[deltaAt-1].Kind != "native" {
			t.Errorf("delta 之前那一条是 %q，期望 native", collected[deltaAt-1].Kind)
		}
		if !strings.Contains(string(collected[deltaAt-1].Frame), `"response.output_text.delta"`) {
			t.Errorf("紧挨 delta 的原生帧不是同一片：%s", collected[deltaAt-1].Frame)
		}
	})

	t.Run("枢纽时一条原生帧都没有", func(t *testing.T) {
		srv := sseServer(t, body...)
		req := nativeRequest(nil)
		req.Stream = true
		events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), req)
		if err != nil {
			t.Fatal(err)
		}
		collected := collect(t, events)
		if frames := nativeFrames(t, collected); len(frames) != 0 {
			t.Errorf("枢纽分支发了 %d 条原生帧", len(frames))
		}
		// 正文仍要发出来，证明这条对照确实跑完了一条流。
		var text strings.Builder
		for _, ev := range collected {
			if ev.Kind == "delta" {
				text.WriteString(ev.Text)
			}
		}
		if text.String() != "你好" {
			t.Errorf("枢纽分支的正文 = %q", text.String())
		}
	})
}

// TestChatStreamNativeKeepsTextFallback 钉住「兜底不因直通而变」。
//
// 有的上游一个 response.output_text.delta 都不发，正文只在 output_item.done
// （或终态事件的 response.output）里给。直通加的是原生帧，归一化的兜底必须
// 照旧把正文补成 delta——否则调用方拿到一条只含原生帧、一个 delta 都没有的
// 流，而下游的中途冻结估算与快照正文都只认 delta。
func TestChatStreamNativeKeepsTextFallback(t *testing.T) {
	payloads := []string{
		`{"type":"response.output_item.done","sequence_number":1,"output_index":0,` +
			`"item":{"type":"message","id":"msg_placeholder","status":"completed","role":"assistant",` +
			`"content":[{"type":"output_text","text":"兜底正文","annotations":[]}]}}`,
		`{"type":"response.completed","sequence_number":2,"response":{"id":"resp_placeholder",` +
			`"model":"upstream-model","status":"completed","output":[],` +
			`"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
	}
	srv := sseServer(t, sseBody(payloads...))
	req := nativeRequest([]byte(nativeResponsesBody))
	req.Stream = true

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), req)
	if err != nil {
		t.Fatal(err)
	}
	collected := collect(t, events)

	var text strings.Builder
	delas := 0
	var usage *channel.Usage
	dones := 0
	for _, ev := range collected {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
			delas++
		case "usage":
			usage = ev.Usage
		case "done":
			dones++
		}
	}
	if text.String() != "兜底正文" || delas != 1 {
		t.Errorf("兜底正文 = %q（%d 条 delta），期望「兜底正文」一条", text.String(), delas)
	}
	if usage == nil || usage.CompletionTokens != 3 {
		t.Errorf("用量 = %+v", usage)
	}
	if dones != 1 {
		t.Errorf("done 事件 %d 条", dones)
	}

	// 原生帧与 payload 逐字对应，一条不多一条不少。
	frames := nativeFrames(t, collected)
	if len(frames) != len(payloads) {
		t.Fatalf("原生帧 %d 条，data: 行 %d 条", len(frames), len(payloads))
	}
	for i := range payloads {
		if string(frames[i]) != payloads[i] {
			t.Errorf("第 %d 条原生帧 = %s，期望 %s", i+1, frames[i], payloads[i])
		}
	}
}

// TestChatStreamNativeSkipsDoneSentinel 钉住 `[DONE]` 不发原生帧。
//
// 它不是 JSON 载荷，而调用方那侧的渲染层自己会写终止帧；把它原样转出去会让
// 客户端收到两个终止符。
func TestChatStreamNativeSkipsDoneSentinel(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"response.output_text.delta","sequence_number":0,"output_index":0,"content_index":0,"delta":"在"}`,
		`data: [DONE]`,
	)
	req := nativeRequest([]byte(nativeResponsesBody))
	req.Stream = true

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), req)
	if err != nil {
		t.Fatal(err)
	}
	collected := collect(t, events)

	if frames := nativeFrames(t, collected); len(frames) != 1 {
		t.Errorf("原生帧 %d 条，期望 1 条（[DONE] 不算）", len(frames))
	}
	dones := 0
	for _, ev := range collected {
		if ev.Kind == "done" {
			dones++
		}
	}
	if dones != 1 {
		t.Errorf("done 事件 %d 条，期望 1 条", dones)
	}
}

// TestChatNativeHeadersAndUsageDeclaration 钉住直通分支同样要过的三处：
// 自定义头、鉴权头、以及用量口径。
//
// 它们都不在 encodeRequest 里，而是各自在别的函数上；漏掉任何一处都会让直通
// 变成「报文对了，但请求头少了插件要的那个」或「用量口径退回默认值」——两种
// 都只在直通上出问题，枢纽分支的用例看不出来。
func TestChatNativeHeadersAndUsageDeclaration(t *testing.T) {
	var gotAuth, gotTrace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTrace = r.Header.Get("x-trace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","model":"upstream-model","status":"completed","output":[],` +
			`"usage":{"input_tokens":10,"output_tokens":18,"total_tokens":58,` +
			`"output_tokens_details":{"reasoning_tokens":30}}}`))
	}))
	t.Cleanup(srv.Close)

	req := nativeRequest([]byte(nativeResponsesBody))
	req.Headers = map[string]string{"x-trace": "trace-placeholder"}

	ch := testChannel(srv.URL)
	ch.Setting = usageSetting(false)
	resp, err := newAdapter().Chat(context.Background(), ch, req)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	if gotAuth != "Bearer placeholder-credential" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotTrace != "trace-placeholder" {
		t.Errorf("自定义头 x-trace = %q，直通时也必须带上", gotTrace)
	}
	if resp.Usage.CompletionIncludesReasoning {
		t.Error("用量口径没跟着渠道声明走")
	}
	if resp.Usage.CompletionTokens != 18 || resp.Usage.ReasoningTokens != 30 {
		t.Errorf("用量 = %+v", resp.Usage)
	}
}

// TestChatNativeTolerantOfTrailingBytes 钉住直通与枢纽对「响应体后面还有东西」
// 的宽严一致。
//
// 两条分支读的是同一条上游响应，结果不该由哪条分支决定。直通那条先把整段
// 读出来，如果接着用 json.Unmarshal，尾随内容会报错——而枢纽那条用解码器
// 读 resp.Body，同样的字节它接受。所以这里对两种尾部各跑一次直通：
// 尾随空白与尾随内容都必须照常解析成功。
func TestChatNativeTolerantOfTrailingBytes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"尾随空白", nativeRawResponse + "\n\n"},
		{"尾随内容", nativeRawResponse + "\ntrailing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := captureServer(t, http.StatusOK, tc.body)
			resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL),
				nativeRequest([]byte(nativeResponsesBody)))
			if err != nil {
				t.Fatalf("直通应当照常解析成功，实际报错: %v", err)
			}
			// 正文、用量与 Raw 都要对：解析成功但下游字段空着，同样是坏的。
			if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "你好" {
				t.Errorf("正文 = %+v，期望「你好」", resp.Message.Content)
			}
			if resp.Usage.CompletionTokens != 40 || resp.Usage.PromptTokens != 100 {
				t.Errorf("用量 = %+v", resp.Usage)
			}
			if resp.Raw["service_tier"] != "default" {
				t.Errorf("Raw[service_tier] = %v，期望 default", resp.Raw["service_tier"])
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("StatusCode = %d", resp.StatusCode)
			}
		})
	}
}

// TestChatHubTolerantOfTrailingBytes 是上面那条的对照组：同一份带尾随内容的
// 响应体，走枢纽也要成功。它证明这条用例真的在测「两条分支一致」，而不是
// 只把直通这一侧的宽严固定下来。
func TestChatHubTolerantOfTrailingBytes(t *testing.T) {
	srv, _, _ := captureServer(t, http.StatusOK, nativeRawResponse+"\ntrailing")
	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), nativeRequest(nil))
	if err != nil {
		t.Fatalf("枢纽应当照常解析成功，实际报错: %v", err)
	}
	if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "你好" {
		t.Errorf("正文 = %+v，期望「你好」", resp.Message.Content)
	}
}

// TestEncodeNativeRejectsRemoteMCPTools 钉住直通分支同样过远端 MCP 那道判据。
//
// 枢纽那条路在序列化之前查工具类型；直通绕过去就少一道。入口解析层今天已经
// 拦下非 function 的声明，所以这条判据在正常链路上打不着——它守的是「入口
// 那一道将来收窄之后」的形状，两条路都该有。
func TestEncodeNativeRejectsRemoteMCPTools(t *testing.T) {
	body := `{"model":"alias-model","input":[],"tools":[{"type":"mcp","server_label":"s"}]}`
	srv, gotBody, _ := captureServer(t, http.StatusOK, `{"id":"r1","model":"m","status":"completed","output":[]}`)

	_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), nativeRequest([]byte(body)))
	if err == nil {
		t.Fatalf("应当报错，实际把报文发了出去：%s", *gotBody)
	}
	if !strings.Contains(err.Error(), "mcp") {
		t.Errorf("错误 = %v，期望点名 mcp", err)
	}
}

// TestEncodeNativeRejectsBrokenBody 钉住「原文解析不了就报错，不静默退回
// 枢纽分支」。
//
// 静默退回会让直通的失败表现为「行为悄悄变了」：调用方发了什么，上游收到的
// 是另一份，而两边都不报错。
func TestEncodeNativeRejectsBrokenBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"不是 JSON", `{`},
		{"是 JSON 但不是对象", `[1,2,3]`},
		{"字面量 null", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, gotBody, _ := captureServer(t, http.StatusOK, `{"id":"r1","model":"m","status":"completed","output":[]}`)
			_, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), nativeRequest([]byte(tc.body)))
			if err == nil {
				t.Fatalf("应当报错，实际把报文发了出去：%s", *gotBody)
			}
			if !strings.Contains(err.Error(), "解析原始请求体失败") {
				t.Errorf("错误 = %v，期望说明是原始请求体解析失败", err)
			}
			if *gotBody != "" {
				t.Errorf("解析失败时不该发出请求，实际发了 %s", *gotBody)
			}
		})
	}
}
