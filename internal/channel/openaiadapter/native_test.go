package openaiadapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// nativeChatBody 是一份真实形状的 OpenAI Chat Completions 请求体：
// 多模态内容块、工具声明与工具往返、以及一批枢纽编码发不出去的字段。
//
// 最后那个 x_vendor_extension 是刻意留的：本包与 relaykit 的 dto 都不认识
// 它，直通必须原样发出。service_tier / store / logit_bias / metadata 是
// 真实客户端会发的字段，枢纽路径同样不转发它们。
const nativeChatBody = `{
	"model": "gpt-5-alias",
	"messages": [
		{"role": "developer", "content": "回答简短。"},
		{"role": "user", "content": [
			{"type": "text", "text": "上海今天多少度？"},
			{"type": "image_url", "image_url": {"url": "https://api.example.com/cat.png", "detail": "low"}}
		]},
		{"role": "assistant", "content": null, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"上海\"}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "content": "{\"temperature\": 18}"}
	],
	"tools": [
		{"type": "function", "function": {"name": "get_weather", "description": "查天气", "strict": true, "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}
	],
	"tool_choice": "auto",
	"parallel_tool_calls": false,
	"max_completion_tokens": 4096,
	"reasoning_effort": "none",
	"response_format": {"type": "json_schema", "json_schema": {"name": "weather", "strict": true, "schema": {"type": "object", "properties": {"temp": {"type": "number"}}}}},
	"seed": 7,
	"metadata": {"tenant": "t1"},
	"service_tier": "auto",
	"store": false,
	"logit_bias": {"50256": -100},
	"modalities": ["text"],
	"user": "example-user",
	"x_vendor_extension": {"keep": ["me", 1]}
}`

// nativeRequest 是一份请求，原文就是 nativeChatBody；模型名与输出上界取
// 「路由层翻完之后」的值（上游名、正数上界），Protocol 是调用方入口。
func nativeRequest() *channel.Request {
	return &channel.Request{
		Protocol:   channel.ProtocolOpenAI,
		Model:      "gpt-5-2025-08-07",
		MaxTokens:  2048,
		NativeBody: []byte(nativeChatBody),
	}
}

// decodeBody 把出站报文解成 map，供逐字段比对。
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("出站报文不是 JSON: %v\n%s", err, body)
	}
	return m
}

// 直通：原文逐字段保真，只覆写 model / max_tokens / stream。
//
// 比法是把覆写项从两边各自剥掉之后整份 DeepEqual——只断言哨兵还在的话，
// 任何一处别的字段被悄悄丢掉都不会变红。
func TestEncodeNativeKeepsOriginalFields(t *testing.T) {
	body, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), nativeRequest(), false)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := decodeBody(t, body)
	original := decodeBody(t, []byte(nativeChatBody))

	// 覆写项：两边各自剥掉。
	delete(original, "model")
	delete(original, "max_tokens") // 原文没有，剥掉只为两边的键集合对齐
	delete(original, "max_completion_tokens")
	delete(original, "stream")
	delete(got, "model")
	delete(got, "max_tokens")
	delete(got, "max_completion_tokens")
	delete(got, "stream")

	if !reflect.DeepEqual(got, original) {
		t.Errorf("直通报文与原文不符（已剥掉覆写项）:\n  收到 %v\n  原文 %v", got, original)
	}
}

// 覆写项各自的值：模型改成上游名、输出上界统一叫 max_tokens、
// max_completion_tokens 必须消失（OpenAI 对双字段报错，枢纽分支从不发两个）。
func TestEncodeNativeOverwrites(t *testing.T) {
	body, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), nativeRequest(), false)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := decodeBody(t, body)

	if got["model"] != "gpt-5-2025-08-07" {
		t.Errorf("model = %v，期望上游名", got["model"])
	}
	if got["max_tokens"] != float64(2048) {
		t.Errorf("max_tokens = %v，期望 2048", got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens 必须消失：上游对两个字段同时出现会报错")
	}
	if got["stream"] != false {
		t.Errorf("stream = %v，期望 false（以本次调用的参数为准）", got["stream"])
	}

	// 哨兵：枢纽路径会丢、直通必须保住。
	if got["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v，期望 false", got["parallel_tool_calls"])
	}
	if _, ok := got["response_format"].(map[string]any); !ok {
		t.Errorf("response_format = %v，期望保留原文", got["response_format"])
	}
	if got["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort = %v，期望 none（没配映射表时原样透传）", got["reasoning_effort"])
	}
	if _, ok := got["x_vendor_extension"]; !ok {
		t.Error("dto 不认识的字段必须原样发出，否则直通没有意义")
	}
	if _, ok := got["tools"]; !ok {
		t.Error("tools 丢了")
	}
}

// temperature / top_p 是「框架覆写项」：渠道别名覆盖（internal/overrides）
// 与插件补丁（internal/plugin）把值写进 channel.Request，而不是写进原文。
// 直通以原文为报文来源，不覆写就等于把它们丢掉——商家配的采样参数静默失效，
// 而两边都不报错。
//
// 三个方向一起钉：原文没有这两个键时写进去；原文有别的值时以 req 为准；
// req 为 nil 时不写也不删，调用方自己写的值照原样发出。
func TestEncodeNativeOverridesSamplingParams(t *testing.T) {
	encode := func(t *testing.T, req *channel.Request) map[string]any {
		t.Helper()
		body, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, false)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		return decodeBody(t, body)
	}
	req := func(body string, temperature, topP *float64) *channel.Request {
		return &channel.Request{
			Protocol:    channel.ProtocolOpenAI,
			Model:       "m",
			MaxTokens:   16,
			NativeBody:  []byte(body),
			Temperature: temperature,
			TopP:        topP,
		}
	}

	t.Run("writes_when_absent", func(t *testing.T) {
		temp, topP := 0.3, 0.9
		got := encode(t, req(`{"model":"m","messages":[]}`, &temp, &topP))
		if got["temperature"] != 0.3 {
			t.Errorf("temperature = %v，期望写进 req 的 0.3", got["temperature"])
		}
		if got["top_p"] != 0.9 {
			t.Errorf("top_p = %v，期望写进 req 的 0.9", got["top_p"])
		}
	})

	t.Run("overrides_original", func(t *testing.T) {
		temp, topP := 0.3, 0.9
		got := encode(t, req(
			`{"model":"m","messages":[],"temperature":0.7,"top_p":0.8}`,
			&temp, &topP))
		// 原文的 0.7 / 0.8 必须被 req 覆盖：上层已经做了这个决定。
		if got["temperature"] != 0.3 {
			t.Errorf("temperature = %v，期望 req 的 0.3 覆盖原文的 0.7", got["temperature"])
		}
		if got["top_p"] != 0.9 {
			t.Errorf("top_p = %v，期望 req 的 0.9 覆盖原文的 0.8", got["top_p"])
		}
	})

	t.Run("nil_keeps_original", func(t *testing.T) {
		// nil 表示这次没有人要覆盖它：不写也不删，原文里的值仍然有效。
		got := encode(t, req(
			`{"model":"m","messages":[],"temperature":0.7,"top_p":0.8}`,
			nil, nil))
		if got["temperature"] != 0.7 {
			t.Errorf("temperature = %v，期望原文的 0.7 原样保留", got["temperature"])
		}
		if got["top_p"] != 0.8 {
			t.Errorf("top_p = %v，期望原文的 0.8 原样保留", got["top_p"])
		}
	})
}

// 没有新的上界可写时不能删原文那个名字：删了等于上界凭空消失，上游按自己的
// 默认值来，而预留是按一个上界算出来的。留着一个名字不是双字段。
func TestEncodeNativeKeepsMaxCompletionTokensWithoutNewBound(t *testing.T) {
	req := nativeRequest()
	req.MaxTokens = 0
	req.NativeBody = []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":4096}`)

	body, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, false)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := decodeBody(t, body)
	if got["max_completion_tokens"] != float64(4096) {
		t.Errorf("max_completion_tokens = %v，期望原文的 4096 留住", got["max_completion_tokens"])
	}
	if v, ok := got["max_tokens"]; ok {
		t.Errorf("没有新的上界时不该写 max_tokens，实际 %v", v)
	}
}

// 枢纽：同一份 req 只把 NativeBody 置 nil，哨兵必须确实不在。
// 这一条证明上面那条用例真的能分辨两条分支，而不是恒真。
func TestEncodeHubDropsSentinels(t *testing.T) {
	req := nativeRequest()
	req.NativeBody = nil

	body, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, false)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := decodeBody(t, body)

	for _, key := range []string{
		"max_completion_tokens",
		"parallel_tool_calls",
		"response_format",
		"reasoning_effort",
		"service_tier",
		"store",
		"logit_bias",
		"metadata",
		"x_vendor_extension",
	} {
		if v, ok := got[key]; ok {
			t.Errorf("枢纽分支不该出现 %q，实际 %v", key, v)
		}
	}
	// 枢纽分支照常写它认识的那几项。
	if got["model"] != "gpt-5-2025-08-07" || got["max_tokens"] != float64(2048) {
		t.Errorf("枢纽报文的 model/max_tokens = %v/%v", got["model"], got["max_tokens"])
	}
}

// 跨协议护栏：调用方发的是 Anthropic，路由到这条 OpenAI 渠道。
// 原文虽然带着，但那不是上游要的协议，必须走枢纽。
func TestEncodeNativeRequiresSameProtocol(t *testing.T) {
	req := nativeRequest()
	req.Protocol = channel.ProtocolAnthropic

	body, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, false)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := decodeBody(t, body)
	if _, ok := got["x_vendor_extension"]; ok {
		t.Error("跨协议时原文不该被使用")
	}
	if _, ok := got["parallel_tool_calls"]; ok {
		t.Error("跨协议必须走枢纽分支")
	}
}

// stream_options：只在流式强设 include_usage；原文已有的其它子键保留；
// 非流式不凭空新增。
func TestEncodeNativeStreamOptions(t *testing.T) {
	t.Run("直通流式新建 stream_options", func(t *testing.T) {
		body, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), nativeRequest(), true)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		got := decodeBody(t, body)
		options, ok := got["stream_options"].(map[string]any)
		if !ok {
			t.Fatalf("stream_options = %v，期望一个对象", got["stream_options"])
		}
		if options["include_usage"] != true {
			t.Errorf("include_usage = %v，期望 true", options["include_usage"])
		}
		if got["stream"] != true {
			t.Errorf("stream = %v，期望 true（以本次调用的参数为准）", got["stream"])
		}
	})

	t.Run("直通流式保留原子键并覆盖 include_usage", func(t *testing.T) {
		const body = `{"model":"alias","messages":[{"role":"user","content":"hi"}],"stream":false,"stream_options":{"include_usage":false,"chunk_extension":"keep"}}`
		req := nativeRequest()
		req.NativeBody = []byte(body)

		out, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, true)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		got := decodeBody(t, out)
		options, ok := got["stream_options"].(map[string]any)
		if !ok {
			t.Fatalf("stream_options = %v", got["stream_options"])
		}
		if options["include_usage"] != true {
			t.Errorf("include_usage = %v，期望被覆盖成 true", options["include_usage"])
		}
		if options["chunk_extension"] != "keep" {
			t.Errorf("chunk_extension = %v，原文的其它子键要保留", options["chunk_extension"])
		}
	})

	t.Run("直通非流式不出现 stream_options", func(t *testing.T) {
		const body = `{"model":"alias","messages":[{"role":"user","content":"hi"}]}`
		req := nativeRequest()
		req.NativeBody = []byte(body)

		out, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, false)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		got := decodeBody(t, out)
		if v, ok := got["stream_options"]; ok {
			t.Errorf("非流式不该出现 stream_options，实际 %v", v)
		}
		// 原文两个名字都没有时新建 max_tokens：预留要一个上界才做得出来。
		if got["max_tokens"] != float64(2048) {
			t.Errorf("max_tokens = %v，期望 2048", got["max_tokens"])
		}
	})

	t.Run("直通非流式保留原文已有的 stream_options", func(t *testing.T) {
		// 非流式不新增这个键，但调用方自己带了就照发：删掉它是改写调用方
		// 的请求，而它不影响这次调用走哪条路。
		const body = `{"model":"alias","messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":false}}`
		req := nativeRequest()
		req.NativeBody = []byte(body)

		out, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, false)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		options, ok := decodeBody(t, out)["stream_options"].(map[string]any)
		if !ok {
			t.Fatal("原文的 stream_options 被删掉了")
		}
		if options["include_usage"] != false {
			t.Errorf("非流式不该改写 include_usage，实际 %v", options["include_usage"])
		}
	})

	t.Run("枢纽流式只有 include_usage", func(t *testing.T) {
		// 同一份原文，NativeBody 置 nil：枢纽分支重编报文，原文里 stream_options
		// 的其它子键跟着一起没了。这一条与上面那条对照，证明该子键的去留真能
		// 分辨两条分支。
		req := nativeRequest()
		req.NativeBody = nil
		out, err := newAdapter().encodeRequest(testChannel("https://api.example.com"), req, true)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		options, ok := decodeBody(t, out)["stream_options"].(map[string]any)
		if !ok {
			t.Fatal("枢纽流式应当写 stream_options")
		}
		if len(options) != 1 || options["include_usage"] != true {
			t.Errorf("枢纽 stream_options = %v，期望只有 include_usage", options)
		}
	})
}

// reasoning_effort 的方言翻译：原文有才翻译，翻成空串就删键。
func TestEncodeNativeReasoningEffortDialect(t *testing.T) {
	cases := []struct {
		name    string
		setting map[string]any
		want    string
		wantHas bool
	}{
		{"没配映射表时原样透传", nil, "none", true},
		{"映射到 off", effortMap(map[string]any{"none": "off"}), "off", true},
		{"映射到空串时删键", effortMap(map[string]any{"none": ""}), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := testChannel("https://api.example.com")
			ch.Setting = tc.setting
			body, err := newAdapter().encodeRequest(ch, nativeRequest(), false)
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			got := decodeBody(t, body)
			v, ok := got["reasoning_effort"]
			if ok != tc.wantHas {
				t.Fatalf("reasoning_effort 在不在 = %v，期望 %v（值 %v）", ok, tc.wantHas, v)
			}
			if ok && v != tc.want {
				t.Errorf("reasoning_effort = %v，期望 %q", v, tc.want)
			}
		})
	}

	t.Run("原文没有时不凭空造", func(t *testing.T) {
		req := nativeRequest()
		req.NativeBody = []byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)
		ch := testChannel("https://api.example.com")
		ch.Setting = effortMap(map[string]any{"none": "off"})
		body, err := newAdapter().encodeRequest(ch, req, false)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if v, ok := decodeBody(t, body)["reasoning_effort"]; ok {
			t.Errorf("原文没有 reasoning_effort，不该造出 %v", v)
		}
	})
}

// nativeChatResponse 是一份真实形状的 OpenAI 响应：带 dto 不认识的
// system_fingerprint 与 service_tier（relaykit 的 OpenAITextResponse 只有
// id/model/object/created/choices/error/usage，没有这两个）。
const nativeChatResponse = `{
	"id": "chatcmpl-1",
	"object": "chat.completion",
	"created": 1730000000,
	"model": "gpt-5-2025-08-07",
	"system_fingerprint": "fp_abc123",
	"service_tier": "default",
	"choices": [{
		"index": 0,
		"finish_reason": "stop",
		"logprobs": null,
		"message": {"role": "assistant", "content": "上海 18 度", "refusal": null, "annotations": []}
	}],
	"usage": {
		"prompt_tokens": 10,
		"completion_tokens": 5,
		"total_tokens": 15,
		"prompt_tokens_details": {"cached_tokens": 0},
		"completion_tokens_details": {"reasoning_tokens": 0}
	}
}`

// 非流式 Raw：直通放上游原始形状，dto 不认识的字段看得见；枢纽是再序列化
// 出来的副本，那两个字段已经没了。
func TestNativeChatRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeChatResponse)
	}))
	defer srv.Close()

	cases := []struct {
		name        string
		native      bool
		wantFinger  bool
		wantService bool
	}{
		{"直通保留原始响应字段", true, true, true},
		{"枢纽丢掉了 dto 不认识的字段", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := nativeRequest()
			if !tc.native {
				req.NativeBody = nil
			}
			resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), req)
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if len(resp.Raw) == 0 {
				t.Fatal("Raw 为空")
			}
			_, hasFinger := resp.Raw["system_fingerprint"]
			_, hasService := resp.Raw["service_tier"]
			if hasFinger != tc.wantFinger || hasService != tc.wantService {
				t.Errorf("Raw 里 system_fingerprint/service_tier 在不在 = %v/%v，期望 %v/%v",
					hasFinger, hasService, tc.wantFinger, tc.wantService)
			}
			if tc.native {
				if resp.Raw["system_fingerprint"] != "fp_abc123" {
					t.Errorf("system_fingerprint = %v", resp.Raw["system_fingerprint"])
				}
				// 归一化那一侧照旧：正文、用量与状态码不受 Raw 的来源影响。
				if resp.UpstreamModel != "gpt-5-2025-08-07" {
					t.Errorf("UpstreamModel = %q", resp.UpstreamModel)
				}
				if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "上海 18 度" {
					t.Errorf("正文 = %+v", resp.Message.Content)
				}
				if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
					t.Errorf("用量 = %+v", resp.Usage)
				}
				if resp.StatusCode != http.StatusOK {
					t.Errorf("StatusCode = %d", resp.StatusCode)
				}
			}
		})
	}
}

// TestNativeChatTolerantOfTrailingBytes 钉住直通与枢纽对「响应体后面还有
// 东西」的宽严一致。
//
// 两条分支读的是同一条上游响应，结果不该由哪条分支决定。直通的 Raw 走的是
// 另一次解析（nativeRaw），它必须和入口那个解码器一样读出一个值就停：换成
// json.Unmarshal 时尾随内容会报错，而那条错误带 Retryable——路由会换下一个
// 候选再发一次，同一笔钱付两次，而且一份可用的 2xx 被丢掉。
func TestNativeChatTolerantOfTrailingBytes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"尾随空白", nativeChatResponse + "\n\n"},
		{"尾随内容", nativeChatResponse + "\ntrailing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), nativeRequest())
			if err != nil {
				t.Fatalf("直通应当照常解析成功，实际报错: %v", err)
			}
			// 正文、用量与 Raw 都要对：解析成功但下游字段空着，同样是坏的。
			if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "上海 18 度" {
				t.Errorf("正文 = %+v，期望「上海 18 度」", resp.Message.Content)
			}
			if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
				t.Errorf("用量 = %+v", resp.Usage)
			}
			// system_fingerprint 只有原文才有：dto 不认识它，Raw 里有它就说明
			// 这份 Raw 确实来自上游原文，而不是从窄结构编回来的副本。
			if v := resp.Raw["system_fingerprint"]; v != "fp_abc123" {
				t.Errorf("Raw[system_fingerprint] = %v，期望 fp_abc123（直通的 Raw 来自原文）", v)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("StatusCode = %d", resp.StatusCode)
			}
		})
	}
}

// TestHubChatTolerantOfTrailingBytes 是上面那条的对照组：同一份带尾随内容的
// 响应体，走枢纽（NativeBody 为 nil）也要成功。它证明这条用例真的在测
// 「两条分支一致」，而不是只把直通这一侧的宽严固定下来。
func TestHubChatTolerantOfTrailingBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeChatResponse+"\ntrailing")
	}))
	defer srv.Close()

	req := nativeRequest()
	req.NativeBody = nil
	resp, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), req)
	if err != nil {
		t.Fatalf("枢纽应当照常解析成功，实际报错: %v", err)
	}
	if len(resp.Message.Content) == 0 || resp.Message.Content[0].Text != "上海 18 度" {
		t.Errorf("正文 = %+v，期望「上海 18 度」", resp.Message.Content)
	}
}

// TestNativeRawTolerantOfTrailingBytes 直接钉住 nativeRaw 的宽严：传整段
// 响应体进去（含尾随内容），它必须和入口那次解析一样读出一个值就停。
//
// 上面那两条走 Chat 的用例证明不了这件事：Chat 交给 nativeRaw 的是入口已经
// 解好的那一个值，尾随字节在那里已经被切掉了，换成 json.Unmarshal 也照样绿。
// 这条用例喂的是 io.ReadAll 形状的字节，另两个适配器里的同名函数收的就是它。
func TestNativeRawTolerantOfTrailingBytes(t *testing.T) {
	raw, err := nativeRaw([]byte(nativeChatResponse + "\ntrailing"))
	if err != nil {
		t.Fatalf("尾随内容不该让解析失败，实际报错: %v", err)
	}
	if v := raw["system_fingerprint"]; v != "fp_abc123" {
		t.Errorf("Raw[system_fingerprint] = %v，期望 fp_abc123", v)
	}
	if v := raw["id"]; v != "chatcmpl-1" {
		t.Errorf("Raw[id] = %v，期望 chatcmpl-1", v)
	}
}

// nativeStreamFrames 是一串真实形状的 OpenAI SSE 分片。system_fingerprint
// 与注释行都在里面：它们是「原始流长什么样」的一部分。
var nativeStreamFrames = []string{
	`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-5-2025-08-07","system_fingerprint":"fp_abc123","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-5-2025-08-07","system_fingerprint":"fp_abc123","choices":[{"index":0,"delta":{"content":"上海"},"finish_reason":null}]}`,
	`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-5-2025-08-07","system_fingerprint":"fp_abc123","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-5-2025-08-07","system_fingerprint":"fp_abc123","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-5-2025-08-07","system_fingerprint":"fp_abc123","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
}

func streamServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range nativeStreamFrames {
			_, _ = io.WriteString(w, "data: "+frame+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

func collectEvents(t *testing.T, events <-chan channel.StreamEvent) []channel.StreamEvent {
	t.Helper()
	var out []channel.StreamEvent
	for ev := range events {
		out = append(out, ev)
	}
	return out
}

// 流式直通：每个非空 SSE 块发一条 native 帧（[DONE] 块也发），归一化事件
// 一条不少，且 native 帧排在由它解析出的归一化事件之前。
func TestNativeStreamEmitsFrames(t *testing.T) {
	srv := streamServer(t)
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), nativeRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	got := collectEvents(t, events)

	var frames []channel.StreamEvent
	for _, ev := range got {
		if ev.Kind == "native" {
			frames = append(frames, ev)
		}
	}
	// 块数 = 5 个数据块 + 1 个 [DONE] 块。
	if len(frames) != len(nativeStreamFrames)+1 {
		t.Fatalf("native 帧数 = %d，期望 %d（数据块 %d 个 + [DONE] 块）",
			len(frames), len(nativeStreamFrames)+1, len(nativeStreamFrames))
	}
	for i, frame := range nativeStreamFrames {
		// 帧是块的原始字节，含 `data: ` 前缀：调用方原样写出即可。
		want := "data: " + frame
		if ev := frames[i]; ev.NativeFormat != channel.ProtocolOpenAI {
			t.Errorf("第 %d 帧 NativeFormat = %q", i, ev.NativeFormat)
		}
		if string(frames[i].Frame) != want {
			t.Errorf("第 %d 帧与原文块不符:\n  收到 %q\n  原文 %q", i, frames[i].Frame, want)
		}
	}
	// [DONE] 块的终止符必须原生直达：调用方拿到的就是上游那一行。
	if last := string(frames[len(frames)-1].Frame); last != "data: [DONE]" {
		t.Errorf("最后一帧 = %q，期望 [DONE] 块", last)
	}
	for i, ev := range frames {
		if strings.HasSuffix(string(ev.Frame), "\n") {
			t.Errorf("第 %d 帧结尾带了换行，块的末尾空行不属于块本身", i)
		}
	}

	// 顺序固定：每一块的 native 帧排在它解析出的归一化事件之前。
	kinds := make([]string, len(got))
	for i, ev := range got {
		kinds[i] = ev.Kind
	}
	wantOrder := []string{
		"native",
		"native", "delta",
		"native", "tool_use",
		"native",
		"native", "usage",
		"native", "done",
	}
	var order []string
	for _, k := range kinds {
		if k != "start" {
			order = append(order, k)
		}
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("事件顺序 = %v，期望 %v", order, wantOrder)
	}

	// 归一化事件的内容本身不变。
	var text strings.Builder
	var finish string
	for _, ev := range got {
		switch ev.Kind {
		case "delta":
			text.WriteString(ev.Text)
		case "tool_use":
			if ev.ToolCall == nil || ev.ToolCall.Name != "get_weather" || ev.ToolCall.Arguments != `{"ci` {
				t.Errorf("tool_use 事件 = %+v", ev.ToolCall)
			}
		case "usage":
			if ev.Usage == nil || ev.Usage.PromptTokens != 10 || ev.Usage.CompletionTokens != 5 {
				t.Errorf("usage 事件 = %+v", ev.Usage)
			}
		case "done":
			finish = ev.FinishReason
		}
	}
	if text.String() != "上海" {
		t.Errorf("拼出的文本 = %q", text.String())
	}
	if finish != "tool_calls" {
		t.Errorf("done 的 FinishReason = %q", finish)
	}
}

// 块边界：空行之间的一整块原样成帧，块内的注释行、event: 行与多行 data:
// 都跟着走；空行本身不属于块。
func TestNativeStreamBlockBoundaries(t *testing.T) {
	const world = `{"choices":[{"index":0,"delta":{"content":"x"}}]}`
	blocks := []string{
		`: keep-alive`,
		"event: message\n" +
			`data: {"a":1}` + "\n" +
			`data: {"b":2}`,
		`data:`,
		"data: " + world,
		`data: [DONE]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i, block := range blocks {
			_, _ = io.WriteString(w, block+"\n\n")
			// 块之间多出来的空行只形成空块，不该多发一帧。放在中间而不是
			// 流末尾：末尾的空行在 [DONE] 那一块之后就 return 了，扫不到。
			if i < len(blocks)-1 {
				_, _ = io.WriteString(w, "\n\n\n")
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), nativeRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	var frames [][]byte
	var text strings.Builder
	var done bool
	for _, ev := range collectEvents(t, events) {
		switch ev.Kind {
		case "native":
			frames = append(frames, ev.Frame)
		case "delta":
			text.WriteString(ev.Text)
		case "done":
			done = true
		}
	}
	if len(frames) != len(blocks) {
		t.Fatalf("native 帧数 = %d，期望 %d（非空块数）", len(frames), len(blocks))
	}
	for i, want := range blocks {
		if string(frames[i]) != want {
			t.Errorf("第 %d 帧:\n  收到 %q\n  期望 %q", i, frames[i], want)
		}
	}
	// 上面那一轮逐字节相等已经钉住了两件事：块内的 `event:` 行与注释行跟着
	// 块一起走，多行 data: 在帧里以 \n 相连（不是各自成帧、也不是拼成一行）。
	if strings.Count(string(frames[1]), "\n") != 2 {
		t.Errorf("第 1 帧的块内行数不对: %q", frames[1])
	}
	// 空 `data:` 行是块的一部分，随块原样发出，不单独跳过。
	if string(frames[2]) != "data:" {
		t.Errorf("空 data 行那一块 = %q", frames[2])
	}
	if text.String() != "x" {
		t.Errorf("拼出的文本 = %q", text.String())
	}
	if !done {
		t.Error("[DONE] 块应照旧发归一化 done 事件")
	}
}

// [DONE] 块发 native 帧，归一化 done 事件照发：终止符要原生直达，而计费层
// 仍然需要那一条 done。
func TestNativeStreamDoneBlockEmitsFrame(t *testing.T) {
	srv := streamServer(t)
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), nativeRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	var doneFrames int
	var doneEvents int
	for _, ev := range collectEvents(t, events) {
		switch ev.Kind {
		case "native":
			if string(ev.Frame) == "data: [DONE]" {
				doneFrames++
			}
		case "done":
			doneEvents++
		}
	}
	if doneFrames != 1 {
		t.Errorf("[DONE] 块的 native 帧数 = %d，期望 1", doneFrames)
	}
	if doneEvents != 1 {
		t.Errorf("归一化 done 事件数 = %d，期望 1", doneEvents)
	}
}

// 每一块的 native 帧排在它解析出的归一化事件之前：调用方按接收顺序拼接，
// 先看到原文那一块才可能与归一化事件对得上。
func TestNativeStreamBlockPrecedesEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{
			`data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`,
			`data: {"choices":[{"index":0,"delta":{"content":"b"}}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, line+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), nativeRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	var order []string
	for _, ev := range collectEvents(t, events) {
		if ev.Kind != "start" {
			order = append(order, ev.Kind)
		}
	}
	want := []string{"native", "delta", "native", "delta", "native", "done"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("事件顺序 = %v，期望 %v", order, want)
	}
}

// 最后一块没有空行收尾（上游写完 [DONE] 就关连接）时也要发出去：不发的话
// 那一块连同它的结束语义一起凭空丢掉。
func TestNativeStreamFlushesFinalBlock(t *testing.T) {
	last := `data: {"choices":[{"index":0,"delta":{"content":"z"},"finish_reason":"stop"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 整个流没有结尾空行。
		_, _ = io.WriteString(w, last+"\n"+"data: [DONE]")
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), nativeRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	var frames [][]byte
	var text, finish string
	for _, ev := range collectEvents(t, events) {
		switch ev.Kind {
		case "native":
			frames = append(frames, ev.Frame)
		case "delta":
			text += ev.Text
		case "done":
			finish = ev.FinishReason
		}
	}
	if len(frames) != 1 {
		t.Fatalf("native 帧数 = %d，期望 1（最后一块没有空行收尾也要发）", len(frames))
	}
	want := last + "\n" + "data: [DONE]"
	if string(frames[0]) != want {
		t.Errorf("最后一帧:\n  收到 %q\n  期望 %q", frames[0], want)
	}
	if text != "z" || finish != "stop" {
		t.Errorf("归一化事件 = %q / %q，期望 z / stop", text, finish)
	}
}

// 枢纽流式：同一串上游分片，一条 native 帧都不发。
func TestHubStreamHasNoNativeFrames(t *testing.T) {
	srv := streamServer(t)
	defer srv.Close()

	req := nativeRequest()
	req.NativeBody = nil
	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), req)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	for _, ev := range collectEvents(t, events) {
		if ev.Kind == "native" {
			t.Fatalf("枢纽分支不该有 native 帧: %s", ev.Frame)
		}
	}
}

// 块内的噪音行（心跳注释、只有前缀的空 data 行）随块原样发出，但不产归一化
// 事件：调用方要看到与上游一致的流，而解析只要认得 data: 行就够了。
func TestNativeStreamCarriesNoiseInBlocks(t *testing.T) {
	payloads := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"a"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"b"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	blocks := []string{
		": keep-alive\n" + "data: " + payloads[0],
		"data:",
		": ping\n" + "data: " + payloads[1],
		"data: " + payloads[2],
		"data: [DONE]",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, block := range blocks {
			_, _ = io.WriteString(w, block+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	events, err := newAdapter().ChatStream(context.Background(), testChannel(srv.URL), nativeRequest())
	if err != nil {
		t.Fatalf("出错: %v", err)
	}

	var frames []channel.StreamEvent
	var text strings.Builder
	for _, ev := range collectEvents(t, events) {
		switch ev.Kind {
		case "native":
			frames = append(frames, ev)
		case "delta":
			text.WriteString(ev.Text)
		}
	}
	if len(frames) != len(blocks) {
		t.Fatalf("native 帧数 = %d，期望 %d（非空块数）", len(frames), len(blocks))
	}
	for i, want := range blocks {
		if string(frames[i].Frame) != want {
			t.Errorf("第 %d 帧 =\n  %q\n期望\n  %q", i, frames[i].Frame, want)
		}
	}
	if text.String() != "ab" {
		t.Errorf("拼出的文本 = %q，期望 ab（噪音行不该打断解析）", text.String())
	}
}

// 直通时请求头那一层的裁决不变：插件加的 x-larkax-* 头照样到上游，
// 鉴权头照样由渠道凭证说了算。
func TestNativeChatKeepsHeaderHandling(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"x"}}]}`)
	}))
	defer srv.Close()

	req := nativeRequest()
	req.Headers = map[string]string{
		"X-Custom-Tenant": "t1",
		"Authorization":   "Bearer 冒充的凭证",
	}
	if _, err := newAdapter().Chat(context.Background(), testChannel(srv.URL), req); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if got.Get("X-Custom-Tenant") != "t1" {
		t.Errorf("直通时自定义头没到上游: %v", got)
	}
	if got.Get("Authorization") != "Bearer placeholder-credential" {
		t.Errorf("直通时鉴权头被覆盖了: %q", got.Get("Authorization"))
	}
}
