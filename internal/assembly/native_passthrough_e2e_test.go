package assembly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 端到端：**同协议直通**（Anthropic 入口 → Anthropic 渠道）。
//
// # 这一组与别组的分工
//
// 直通的判据分两半：
//
//   - **请求那一半**只能在这里看：上游收到的报文是不是调用方的原文（覆写
//     字段除外）。internal/channel 各适配器的 native 用例钉的是
//     「encodeNative 拿一份请求编出什么」，那里没有装配、没有别名、没有参数
//     覆盖——而覆盖正是「覆写集合里有 temperature / top_p」这一条的唯一来源。
//   - **响应那一半**的字节判据也在这里，因为只有整条链路跑起来才拿得到
//     客户端真正收到的字节（渲染器、包装器、flush 全在链上）。
//
// 流式那一条的判据按任务书：**客户端收到的块序列与上游发来的块序列逐字节
// 相同，不出现任何合成帧**。所以判据实现走字节比较，不解析成帧再比：仓库
// 里的 parseFrames（internal/api/stream_test.go:44）只认 `event: ` 与
// `data: ` 两种行，遇到注释行判红、同块多条 data 只留最后一条——直通原生块
// 两者都带，解析一遍再比会把「原样转发」测成「能从里面解析出什么」。
//
// # 两处**刻意偏离**上游字节，各自都有出处
//
//  1. 含 `model` 的块要改写（docs/api.md:134 的「入站别名」一节：响应体里的
//     model 回显调用方写的名字，流式与非流式一样）。实现见 internal/api 的
//     rewriteModelKey。
//  2. `message_delta` 缺 `input_tokens` 时要补（contract/README.md 写着那一处
//     总是真值；真上游两种形状都发过）。实现见 internal/api 的
//     rewriteMessageDeltaInputTokens。
//
// 所以下面断言的是「除这两块外逐字节相同」，不是「整份相同」——把这两处也
// 纳入逐字节比较，等于让夹具替实现圆谎。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，见 e2e_test.go 顶部与 LESSONS.md 第 16 条。

// ── 假上游 ────────────────────────────────────────────────────────────

// nativeUpstream 是一个只做一件事的本地假上游：记下收到的请求体，回一段
// **写死的**字节。
//
// 与 fakeUpstream / streamUpstream 分开，而不是给它们加开关：那两个类型各自
// 被十来条用例钉着响应形状，加一个「按字段回不同内容」的入口会把两种用途
// 搅在一起（同 streamUpstream 顶部的理由）。这里要的不是形状，是「上游发来的
// 字节原样到了客户端没有」——所以回什么完全由用例决定。
type nativeUpstream struct {
	contentType string
	body        string
	// headers 是额外要回的头，用来钉「上游的头不会混进我们的响应」。
	headers map[string]string

	mu     sync.Mutex
	bodies [][]byte
	paths  []string
}

func (u *nativeUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()

		for k, v := range u.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", u.contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, u.body)
	})
}

// onlyBody 返回唯一一次调用收到的请求体。
func (u *nativeUpstream) onlyBody(t *testing.T) []byte {
	t.Helper()

	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(u.bodies))
	}
	return append([]byte(nil), u.bodies[0]...)
}

// onlyPath 返回唯一一次调用的路径。
func (u *nativeUpstream) onlyPath(t *testing.T) string {
	t.Helper()

	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.paths) != 1 {
		t.Fatalf("假上游被调用 %d 次，期望 1 次", len(u.paths))
	}
	return u.paths[0]
}

// ── 请求体 ────────────────────────────────────────────────────────────

// nativeThinkingBody 是一份**开启扩展思考**的真形状 Anthropic 请求体。
//
// 字段取自真实报文（见 internal/channel/anthropicadapter 的 native 真样本）：
// `thinking` 对象、两处 `cache_control`（system 与消息正文各一处）、
// `stop_sequences`、`metadata`、`tools`、system 数组。这些键统一结构里都
// 没有对应位置——枢纽那条路会把它们丢掉，而直通必须原样发给上游。
//
// **不给 `temperature` / `top_k`**：Anthropic 在开启扩展思考时要求这两项保持
// 缺省（给了会被上游拒）。夹具造一份生产上会被上游拒掉的报文，测出来的就
// 不是真实那条路（见 LESSONS.md 第 19 条）。采样参数那几项由
// nativeSamplingBody 覆盖。
func nativeThinkingBody(model string, stream bool) []byte {
	body := map[string]any{
		"model":          model,
		"max_tokens":     1024,
		"stream":         stream,
		"stop_sequences": []any{"\n\nHuman:"},
		"system": []any{map[string]any{
			"type":          "text",
			"text":          "你是助手。",
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 512},
		"metadata": map[string]any{"user_id": "user@example.com"},
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":          "text",
				"text":          "北京天气怎么样？",
				"cache_control": map[string]any{"type": "ephemeral"},
			}},
		}},
		"tools": []any{map[string]any{
			"name":        "get_weather",
			"description": "查天气",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
	}
	return mustMarshal(body)
}

// nativeSamplingBody 是一份**带采样参数**的真形状请求体：`top_k` 与
// `temperature` 都在，没有 `thinking`（理由见上一条）。
//
// 参数由调用方给：**调用方给了**与**没给**是参数覆盖的两条相反判据，而
// 「没给」用值类型表达不出来（temperature 的 0 是合法取值）。
func nativeSamplingBody(model string, temperature *float64) []byte {
	body := map[string]any{
		"model":          model,
		"max_tokens":     1024,
		"top_k":          40,
		"stop_sequences": []any{"\n\nHuman:"},
		"system":         "你是助手。",
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "北京天气怎么样？",
		}},
	}
	if temperature != nil {
		body["temperature"] = *temperature
	}
	return mustMarshal(body)
}

func mustMarshal(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return out
}

// ── 上游回什么 ────────────────────────────────────────────────────────

// nativeAnthropicReply 是一份真形状的 Anthropic 非流式响应体，带三处
// **统一结构不认识的字段**：
//
//   - 顶层 `container`（真样本里有，见 anthropicadapter 的 native 用例）；
//   - `usage.service_tier`；
//   - 非空的 `stop_sequence`。
//
// 它们正是直通与非直通在响应上的差别：枢纽那条路按 dto 裁剪，这三处都会
// 消失。所以断言它们到了调用方，等于断言这次走的是直通。
//
// `model` 故意写成上游自己的名字：回显给调用方的必须是**调用方要的那个**，
// 两边写成同一个字符串的话，「回显的是哪一个」就分不出来。
const nativeAnthropicReply = `{
  "id": "msg_01XFDUDYJgAACzvnptvVoYEL",
  "type": "message",
  "role": "assistant",
  "model": "vendor/reported-model",
  "container": {"id": "container_011CXLX661sTp1ZaG1JvJvJv", "expires_at": "2026-01-01T00:00:00Z"},
  "content": [{"type": "text", "text": "北京晴。"}],
  "stop_reason": "end_turn",
  "stop_sequence": "\n\nHuman:",
  "usage": {"input_tokens": 12, "output_tokens": 7, "service_tier": "standard"}
}`

// ── 断言的辅助 ────────────────────────────────────────────────────────

// decodeJSONObject 把一份报文解成通用 map。数字按 float64 读——两侧都走
// JSON，比较时不会因为 int/float 的类型不同而假失败。
func decodeJSONObject(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("解析报文失败: %v\n%s", err, body)
	}
	return m
}

// assertSameFields 断言两份 JSON 对象**逐字段等价**：不多、不少、值相同。
//
// 逐键报而不是整体 DeepEqual：整体比只会说「不相等」，而这条契约要看的是
// **差在哪个键上**——多一个键是「我们偷偷加了东西」，少一个键是「原文被丢了
// 一部分」，两种错法的排查方向完全不同。
func assertSameFields(t *testing.T, label string, got, want map[string]any) {
	t.Helper()

	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("%s 少了键 %q（期望 %v）——原文里的键不能被丢", label, k, w)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s[%q] = %#v，期望 %#v", label, k, g, w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s 多出键 %q（值 %#v）——直通不该自己往上加字段", label, k, g)
		}
	}
}

// splitSSEBlocks 按空行切出 SSE 块（块内全部行，结尾空行不算）。
//
// 与 internal/api 的 sseBlockParts 不同：那是**读字段**，这里只切块边界，
// 内容一个字节都不动——本文件要的就是字节。空块跳过（连续空行不产生块），
// 与适配器的 readBlock 同一个口径。
func splitSSEBlocks(t *testing.T, body string) []string {
	t.Helper()

	var out []string
	for _, block := range strings.Split(body, "\n\n") {
		if block != "" {
			out = append(out, block)
		}
	}
	return out
}

// assertBlockDiff 报出两块第一次不同的位置。
//
// 只报「不相等」会把排查推给下一个人去手工 diff 一长串 JSON，而第一次不同
// 的那个偏移量通常一眼就能看出差在哪。
func assertBlockDiff(t *testing.T, label, got, want string) {
	t.Helper()

	if got == want {
		return
	}
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	t.Errorf("%s 与上游字节不同（第 %d 个字节起）\n got = %q\nwant = %q", label, i, got, want)
}

// ── 非流式：原文进、原文出 ────────────────────────────────────────────

// 非流式直通：上游收到的报文与调用方原文**逐字段等价**（只有上游模型名被
// 换掉），响应里统一结构不认识的字段原样到了调用方。
//
// 名字里带 OriginalBody 是因为这一条**没有配置任何参数覆盖**：它钉的是
// 绝大多数部署的样子——配了别名（翻名），没配覆盖，报文除名字外不动。
func TestNativePassthroughMessagesNonStreamKeepsOriginalBody(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-native-msg")
	upstreamName := "vendor/" + model

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &nativeUpstream{contentType: "application/json", body: nativeAnthropicReply}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	// 别名：调用方写规范名，上游收到的是上游名。没有覆盖。
	seedAlias(t, db, channelID, upstreamName, model)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	body := nativeThinkingBody(model, false)
	status, respBody, header := postChatFull(ctx, t, public.URL+"/v1/messages", key, body)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, respBody)
	}

	// 一、发出去的那一半：除 model 外逐字段等于原文。
	//
	// 这是「直通」唯一站得住的证据。只看响应体不够——按统一字段重编一份报文
	// 也能得到正确的响应（枢纽那条路就是），差别只在发出去的那份里少了
	// thinking / cache_control / stop_sequences / metadata 这些键，而它们
	// 改变的是上游行为，调用方从响应上完全看不出来。
	// 端点也是协议的一部分：Anthropic 上游在 /v1/messages 上。
	if got := fake.onlyPath(t); got != "/v1/messages" {
		t.Errorf("上游收到的路径 = %q，期望 /v1/messages", got)
	}

	want := decodeJSONObject(t, body)
	// 覆写项一：model。到适配器时它已经是**上游名**（路由层翻名之后才交给
	// 适配器，见 internal/route 的 upstreamRequest）。
	want["model"] = upstreamName
	assertSameFields(t, "上游收到的报文", decodeJSONObject(t, fake.onlyBody(t)), want)

	// 二、响应那一半：上游原文里 dto 不认识的字段到了调用方。
	got := decodeJSONObject(t, respBody)
	if got["model"] != model {
		t.Errorf("响应体的 model = %v，期望 %q（回显调用方写的名字，不是上游回报的）",
			got["model"], model)
	}
	if want := header.Get("x-request-id"); got["id"] != want {
		t.Errorf("响应体的 id = %v，x-request-id = %q：两者必须是同一个值", got["id"], want)
	}
	if _, ok := got["container"]; !ok {
		t.Errorf("响应体缺 container——统一结构不认识它，只有直通才带得回来：%s", respBody)
	}
	if usage, _ := got["usage"].(map[string]any); usage == nil || usage["service_tier"] != "standard" {
		t.Errorf("响应体的 usage = %v，期望带着上游报的 service_tier", got["usage"])
	}
	if got["stop_sequence"] != "\n\nHuman:" {
		t.Errorf("响应体的 stop_sequence = %v，期望上游报的那一个（直通不替调用方改它）",
			got["stop_sequence"])
	}
	if content, _ := got["content"].([]any); len(content) != 1 {
		t.Errorf("响应体的 content = %v，期望一个正文块", got["content"])
	}

	// 三、钱：12 × 0.00001 + 7 × 0.00003 = 0.00033000，与枢纽那条路同一个数。
	// 直通换的是报文的来源，不是计费口径——两条分支同一输入必须同一笔账。
	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033（12 × 0.00001 + 7 × 0.00003）", bill.final.String)
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, channelID)
	}
	assertNotCapped(t, bill)
	assertWallet(t, db, user, "99.99967", "0")
}

// 非流式直通 + 渠道别名上的**参数覆盖**：三个方向各钉一条。
//
//   - `max_tokens`：覆盖赢，哪怕调用方也给了（保护商家的成本）；
//   - `temperature`：调用方赢（调用方给了 0.7，覆盖是 0.2）；
//   - `top_p`：调用方没给，覆盖补上（0.9）。
//
// 三条必须一起测：它们的方向不同，只钉一条的话，把三条判据写成同一个方向在
// 用例里看不出来（同 alias_overrides_e2e_test.go 顶部的理由）。而直通这条
// 路上它们还有一层——**报文来源是原文**，不覆写就等于覆盖配置静默失效，
// 两边都不报错。
func TestNativePassthroughMessagesNonStreamAppliesOverrides(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-native-ov")
	upstreamName := "vendor/" + model

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &nativeUpstream{contentType: "application/json", body: nativeAnthropicReply}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)
	seedCatalogModel(t, db, model)
	seedAliasWithOverrides(t, db, channelID, upstreamName, model,
		map[string]any{"max_tokens": 512, "temperature": 0.2, "top_p": 0.9})

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	body := nativeSamplingBody(model, f64(0.7))
	status, respBody := postChat(ctx, t, public.URL+"/v1/messages", key, body)
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, respBody)
	}

	sent := decodeJSONObject(t, fake.onlyBody(t))

	// 覆写项逐条判，失败文案写明**方向**——「覆盖没生效」与「调用方的值被
	// 覆盖」在响应上都看不出来，只有这里分得开。
	if got := sent["max_tokens"]; got != float64(512) {
		t.Errorf("上游收到的 max_tokens = %v，期望 512（覆盖赢：商家限制输出上界）", got)
	}
	if got := sent["temperature"]; got != 0.7 {
		t.Errorf("上游收到的 temperature = %v，期望 0.7（调用方给了就以调用方为准）", got)
	}
	if got := sent["top_p"]; got != 0.9 {
		t.Errorf("上游收到的 top_p = %v，期望 0.9（调用方没给，取覆盖值）", got)
	}

	// 其余键仍然逐字段等于原文。期望值由原文加上这几处覆写推出来，不另写一份
	// 夹具：另写一份会让两个来源各自漂移，而漂移的那一份不会报错。
	want := decodeJSONObject(t, body)
	want["model"] = upstreamName
	want["max_tokens"] = float64(512)
	want["top_p"] = 0.9
	// stream 也在覆写集合里（见覆写清单），而这份原文**没有**这个键：适配器
	// 总是写它（internal/channel/anthropicadapter/native.go:82），因为这次走
	// 非流式还是流式只由入口决定，不能由原文说了算。所以这里期望它被补上。
	want["stream"] = false
	assertSameFields(t, "上游收到的报文", sent, want)

	// top_k 是统一结构里没有位置的键：它能不能到上游，只有这里看得见。
	if got := sent["top_k"]; got != float64(40) {
		t.Errorf("上游收到的 top_k = %v，期望 40（原文里的键要原样发出去）", got)
	}

	// 钱按**上游报的用量**算，与覆盖的上界无关：512 只影响预留，不改变结算。
	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033", bill.final.String)
	}
	assertNotCapped(t, bill)
}

// ── 流式：客户端收到的字节 ────────────────────────────────────────────

// nativeAnthropicStream 是一条真形状的 Anthropic SSE。
//
// 每一块都来自真实报文的形状（见 anthropicadapter 的 native 真样本）：
//
//   - 纯注释块（`: keep-alive`）——它不产生归一化事件，直通下必须照样转发；
//   - `event: ping` 心跳块；
//   - 扩展思考的 `content_block_start` / `thinking_delta`；
//   - `message_delta` 块里多一行**空 data 行**（真样本里就有，见 native_test.go
//     「块边界」那条用例的 message_delta 块）；
//   - `message_delta` **不带** `input_tokens`（真上游两种形状都发过，这一种
//     是 native_test.go 里那份）。
//
// `model` 故意写成上游自己的名字：改名那一块是两处刻意偏离之一，下面按
// 「只改这一个键」判它。
const nativeAnthropicStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"vendor/reported-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}

: keep-alive

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

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

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":7}}
data:

event: message_stop
data: {"type":"message_stop"}` + "\n\n"

// 流式直通：客户端收到的块序列与上游发来的块序列逐字节相同，两处刻意偏离
// 之外一个字节都不差。
//
// # 判据怎么实现
//
// 先把响应体按空行切成块，再把每一块与上游同一位置的块逐字节比。**不解析
// 成帧**——理由见文件顶部。块数相等本身就是「零合成帧」的一半：多写一帧
// 合成信封、多渲一条归一化事件，块数就对不上；而逐字节比抓住了另一半
// 「合成帧恰好替换了某帧」。
//
// 两处偏离按「最小改动」判：
//
//   - message_start：只把 `message.model` 换成调用方的名字，其余键一个不动；
//   - message_delta：只补 `usage.input_tokens`，且补的是**上游在
//     message_start 报过的 12**，不是本地按字数估出来的那个数。
func TestNativePassthroughMessagesStreamForwardsUpstreamBytes(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-native-stream")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &nativeUpstream{
		contentType: "text/event-stream",
		body:        nativeAnthropicStream,
		// 上游回一个自己的头：它**不该**出现在发给调用方的响应里（响应头是
		// 我们自己产生的，见 internal/api 的 streamHeaders）。
		headers: map[string]string{"X-Upstream-Only": "should-not-leak"},
	}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	channelID := seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/messages", key, nativeThinkingBody(model, true))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("X-Upstream-Only"); got != "" {
		t.Errorf("响应里带上了上游的头 X-Upstream-Only = %q——响应头由我们自己产生", got)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	got := string(raw)

	// 一、逐块比较。
	upstreamBlocks := splitSSEBlocks(t, nativeAnthropicStream)
	gotBlocks := splitSSEBlocks(t, got)
	if len(gotBlocks) != len(upstreamBlocks) {
		t.Fatalf("客户端收到 %d 块，上游发了 %d 块：出现了合成帧或丢了帧\n got = %q",
			len(gotBlocks), len(upstreamBlocks), got)
	}
	// 切块再拼回来必须还是原来那份字节：保证「按块比」不是把某些字节藏在了
	// 分隔符上（比如结尾多一个换行、块之间空了两行）。
	if rejoined := strings.Join(gotBlocks, "\n\n") + "\n\n"; rejoined != got {
		t.Errorf("响应体按块切分后拼不回原字节\n got = %q\n拼回 = %q", got, rejoined)
	}

	// 两处刻意偏离的位置：model 在 message_start，input_tokens 在
	// message_delta。其余每一块都逐字节等于上游。
	for i, want := range upstreamBlocks {
		switch {
		case strings.HasPrefix(want, "event: message_start"):
			assertRewrittenModelBlock(t, i, gotBlocks[i], want, model)
		case strings.HasPrefix(want, "event: message_delta"):
			assertFilledDeltaBlock(t, i, gotBlocks[i], want)
		default:
			assertBlockDiff(t, fmt.Sprintf("第 %d 块", i), gotBlocks[i], want)
		}
	}

	// 二、任务书点名的四项要原样到调用方。
	//
	// thinking 增量只在直通下存在：统一事件流把它当模型的中间产物扔掉了
	// （见 anthropicadapter 的 pump 里 thinking_delta 那一支），所以枢纽那条
	// 路上调用方看不到它。
	if !strings.Contains(got, `"type":"thinking_delta","thinking":"想一想。"`) {
		t.Errorf("响应里没有 thinking 增量——统一事件流会丢掉它，只有直通才带得回来：%s", got)
	}
	if !strings.Contains(got, `: keep-alive`) {
		t.Errorf("响应里没有心跳注释块——它不是 SSE 字段，parseFrames 那类解析器会判红，正因如此要按字节转发：%s", got)
	}

	sent := decodeJSONObject(t, fake.onlyBody(t))
	for _, key := range []string{"thinking", "stop_sequences"} {
		if _, ok := sent[key]; !ok {
			t.Errorf("上游收到的报文缺 %q——它没有统一结构里的位置，直通必须原样发", key)
		}
	}
	if !bytes.Contains(fake.onlyBody(t), []byte(`"cache_control"`)) {
		t.Error("上游收到的报文里没有 cache_control——它是统一结构读不到的键，直通必须原样发")
	}
	if sent["stream"] != true {
		t.Errorf("上游收到的 stream = %v，期望 true", sent["stream"])
	}

	// 三、钱：用量跨两个事件（输入在 message_start、输出在 message_delta），
	// 结算是 12 × 0.00001 + 7 × 0.00003 = 0.00033，与非流式同一个数。
	bill := waitForBillingStatus(t, db, user, "settled")
	if !sameDecimal(bill.final.String, "0.00033") {
		t.Errorf("结算金额 = %s，期望 0.00033", bill.final.String)
	}
	if !bill.streamed {
		t.Error("billing_requests.streamed = false——这一列是事后分辨两条路的唯一依据")
	}
	if !bill.channelID.Valid || bill.channelID.Int64 != channelID {
		t.Errorf("账单的 channel_id = %v，期望 %d", bill.channelID, channelID)
	}
	assertNotCapped(t, bill)
	assertWallet(t, db, user, "99.99967", "0")
}

// assertRewrittenModelBlock 判「只改了 model 这一个键」。
//
// 期望值由**上游那一块**加上一处改动推出来，而不是另写一份夹具：另写一份
// 会让两个来源各自漂移，而漂移的那一份不会报错。重建成 `event:` 行 + 一行
// data（与 rewriteModelKey 的重建方式相同）之后逐字节比——键序由
// json.Marshal 的字典序决定，两侧走的是同一个序列化器。
func assertRewrittenModelBlock(t *testing.T, i int, got, upstream, wantModel string) {
	t.Helper()

	payload := decodeJSONObject(t, []byte(dataPayloadOf(t, upstream)))
	message, _ := payload["message"].(map[string]any)
	if message == nil {
		t.Fatalf("第 %d 块（message_start）里没有 message 对象：%q", i, upstream)
	}
	message["model"] = wantModel

	want := "event: message_start\ndata: " + string(mustMarshal(payload))
	assertBlockDiff(t, fmt.Sprintf("第 %d 块（message_start，只改 model）", i), got, want)
}

// assertFilledDeltaBlock 判「只补了 input_tokens 这一个键」。
//
// 补的值必须是**上游报过的** 12，不是本地估算。判据写成具体数字而不是
// 「非零」：本地估算按字数算，与上游报的数不是同一个东西，而「非零」对两者
// 一视同仁（同 contract/check.py 里那段注释的理由）。
func assertFilledDeltaBlock(t *testing.T, i int, got, upstream string) {
	t.Helper()

	wantPayload := decodeJSONObject(t, []byte(dataPayloadOf(t, upstream)))
	wantUsage, _ := wantPayload["usage"].(map[string]any)
	if wantUsage == nil {
		t.Fatalf("第 %d 块（message_delta）的夹具里没有 usage：补 input_tokens 的那条路走不到", i)
	}
	if _, ok := wantUsage["input_tokens"]; ok {
		t.Fatalf("第 %d 块的夹具自己带了 input_tokens，补缺的那个分支就成了死代码", i)
	}
	wantUsage["input_tokens"] = float64(12)

	// 客户端收到的这一块：event 行保留，载荷按字段等价判（这一块是被重建的，
	// 块里那行空 `data:` 不再保留——它是空隙不是内容）。
	lines := strings.Split(got, "\n")
	if len(lines) == 0 || lines[0] != "event: message_delta" {
		t.Fatalf("第 %d 块的首行 = %q，期望 event: message_delta", i, got)
	}
	var gotPayload map[string]any
	for _, line := range lines {
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			if p := strings.TrimSpace(payload); p != "" {
				gotPayload = decodeJSONObject(t, []byte(p))
			}
		}
	}
	if gotPayload == nil {
		t.Fatalf("第 %d 块里找不到 data 载荷：%q", i, got)
	}
	assertSameFields(t, fmt.Sprintf("第 %d 块（message_delta）的载荷", i), gotPayload, wantPayload)

	gotUsage, _ := gotPayload["usage"].(map[string]any)
	if gotUsage["input_tokens"] != float64(12) {
		t.Errorf("第 %d 块的 usage.input_tokens = %v，期望 12（上游在 message_start 报过的真值，"+
			"不是本地按字数估出来的数）", i, gotUsage["input_tokens"])
	}
}

// dataPayloadOf 取出一块里第一行非空 data 的载荷（夹具都是单行载荷）。
func dataPayloadOf(t *testing.T, block string) string {
	t.Helper()

	for _, line := range strings.Split(block, "\n") {
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			if p := strings.TrimSpace(payload); p != "" {
				return p
			}
		}
	}
	t.Fatalf("块里没有 data 载荷：%q", block)
	return ""
}

// ── CRLF ─────────────────────────────────────────────────────────────

// nativeCRLFStream 与上面那条流同形，但行尾是 CRLF，且 `model` 报的就是
// 调用方写的名字（很多上游就是这么回显的）——于是这条用例里**没有**任何
// 刻意偏离，期望值可以整份按字节写出来。
const nativeCRLFStream = "event: message_start\r\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"%s","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":1}}}` + "\r\n" +
	"\r\n" +
	"event: content_block_delta\r\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"北京晴。"}}` + "\r\n" +
	"\r\n" +
	"event: message_stop\r\n" +
	`data: {"type":"message_stop"}` + "\r\n\r\n"

// 上游用 CRLF 分行时，客户端拿到的是 LF——**网关没有做归一化，那个 \r 在
// 扫描阶段就没了**。
//
// 这条钉的是现状与责任归属：channel 的扫描器（bufio.Scanner 的 ScanLines，
// 三份 stream.go 里的 readBlock）切行时吃掉行尾 `\r`，所以 Frame 里本来就不
// 带它；internal/api 写块时只补 `\n\n`，不做替换。将来谁为了「保留原始字节」
// 改成自己切行，客户端可见的形状变化会在这里显形。
//
// **块内容本身没有被碰**：去掉的只有行尾那一个 `\r`。
func TestNativePassthroughStreamCRLFBecomesLFAtScan(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	const balance = "100"
	model := unique("e2e-native-crlf")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	crlf := fmt.Sprintf(nativeCRLFStream, model)
	want := strings.ReplaceAll(crlf, "\r\n", "\n")

	fake := &nativeUpstream{contentType: "text/event-stream", body: crlf}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannelOfType(t, db, sealer, merchant, upstreamSrv.URL, model, "anthropic", 0)
	seedRule(t, db, model, `{"prompt": 0.00001, "completion": 0.00003}`)

	app := newApp(t, db, sealer)
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	resp, cancel := openStream(t, public.URL+"/v1/messages", key, nativeThinkingBody(model, true))
	defer cancel()
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	if got := string(raw); got != want {
		t.Errorf("客户端收到的字节 = %q，期望 %q（行尾 \\r 在扫描阶段被吃掉，块内容不变）", got, want)
	}
	if strings.Contains(string(raw), "\r") {
		t.Errorf("响应体里还留着 \\r：%q", raw)
	}
}
