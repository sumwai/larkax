package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/idempotency"
)

// 这个文件钉住**入站别名解析**在入口的位置与它往下传的形状。
//
// 要钉住的是三件事，它们各自会以一种「看起来正常」的方式坏掉：
//
//  1. 解析发生在幂等指纹之前 —— 否则同一个逻辑请求写别名与写规范名算出两个
//     指纹，第二次拿到 409；
//  2. 解析结果一路是规范名（选路、计费、插件），而响应里回显的是调用方写的
//     那个名字 —— 两处都取 upstream.Model 时必有一边错；
//  3. 撞键重试那一路**补回**规范名 —— 那一轮拿到的是重新解析出来的新对象，
//     入口那一次赋值不会跟着过去。

// fakeInbound 是 InboundResolver 的桩。
//
// aliases 是一张写死的映射表：用例要的是「这个名字解析成那个名字」，不是
// 别名的存储形状（那由 internal/catalog 的用例钉）。
type fakeInbound struct {
	aliases map[string]string
	err     error
	calls   int
	// seen 记下每次被问的名字。解析只该发生一次，所以这个切片的长度也是一条
	// 判据（见 TestInboundAliasSurvivesIdempotencyRetry）。
	seen []string
}

func (f *fakeInbound) ResolveInbound(_ context.Context, name string) (string, error) {
	f.calls++
	f.seen = append(f.seen, name)
	if f.err != nil {
		return "", f.err
	}
	// 与 catalog.ResolveInbound 同一个契约：空名字是错误，不是「没命中」。
	// 少了这一条，下面那条位置用例就没有对手——真实实现拒空名、桩却放行，
	// 桩上的位置判据于是什么都证明不了。
	if name == "" {
		return "", errors.New("模型名为空")
	}
	if canonical, ok := f.aliases[name]; ok {
		return canonical, nil
	}
	return name, nil
}

// inboundHarness 给一个装了入站解析器的 handler。
func inboundHarness() (*Handler, *fakeAuth, *fakeChatter, *fakeInbound) {
	h, auth, chatter := newTestHandler()
	inbound := &fakeInbound{aliases: map[string]string{"fast": "m-1"}}
	h.Inbound = inbound
	return h, auth, chatter, inbound
}

// 给了解析器时：上游与计费看到的是规范名，响应体回显的是调用方写的别名。
//
// 两半必须一起断言。只断上游收到规范名，会把「响应也回了规范名」放过；
// 只断响应回显别名，会把「选路还按别名走」放过——而后者在一条真别名配好
// 之前根本发不出去请求。
//
// # 三条协议各跑一遍
//
// 回显那条路是共享的（serve → newStreamRenderer 收的是同一个 names.requested），
// 但那是结构证据，不是断言。三条入口里只有 chat 被钉住的话，下一笔改这条
// 路径的人不会碰到另外两条的用例——messages 或 responses 单独回错了名字，
// 没有任何东西会红。
func TestInboundAliasResolvesToCanonicalButEchoesRequested(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "chat", path: PathChatCompletions, body: validBody},
		{name: "messages", path: PathMessages, body: validBody},
		{name: "responses", path: PathResponses, body: responsesBody},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(tc.body, `"m-1"`, `"fast"`, 1)
			h, _, chatter, inbound := inboundHarness()

			rec := post(h, tc.path, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
			}

			if got := chatter.upstream.Model; got != "m-1" {
				t.Errorf("发往上游的 Model = %q，期望规范名 m-1", got)
			}
			if got := chatter.req.CanonicalModel; got != "m-1" {
				t.Errorf("计费请求的 CanonicalModel = %q，期望规范名 m-1", got)
			}

			var resp struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body)
			}
			if resp.Model != "fast" {
				t.Errorf("响应体的 model = %q，期望调用方写的 %q：别名存在的意义就是客户端有一个稳定的名字",
					resp.Model, "fast")
			}

			if inbound.calls != 1 {
				t.Errorf("解析被调用 %d 次，期望 1 次（一次请求只解析一次）", inbound.calls)
			}
		})
	}
}

// 流式那条路与非流式同一个口径：帧里的 model 也是调用方写的名字。
//
// 两处分叉时的现场最难查——非流式绿、流式红，而两边渲染器收的是同一个
// 参数名，改了一处很容易漏另一处。三条协议同样各跑一遍，理由与非流式那张
// 表相同：共享路径不等于被钉住。
func TestInboundAliasEchoedInStreamFrames(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{name: "chat", path: PathChatCompletions, body: streamBody},
		{name: "messages", path: PathMessages, body: streamBody},
		{name: "responses", path: PathResponses, body: responsesStreamBody},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(tc.body, `"m-1"`, `"fast"`, 1)
			h, _, chatter, _ := inboundHarness()
			// 三类协议都要有一条**会产出首帧**的事件流：Anthropic 的
			// message_start 挂在第一个事件上（空流一帧都不写），用空流时
			// 那一格会以「一帧都没有 model」的样子假红。
			usage := channel.Usage{PromptTokens: 12, CompletionTokens: 7}
			chatter.streamResult = &gateway.Result{Events: streamOf(
				channel.StreamEvent{Kind: "delta", Text: "你好"},
				channel.StreamEvent{Kind: "usage", Usage: &usage},
				channel.StreamEvent{Kind: "done", FinishReason: "end_turn"},
			)}

			rec := post(h, tc.path, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
			}

			if got := chatter.upstream.Model; got != "m-1" {
				t.Errorf("发往上游的 Model = %q，期望规范名 m-1", got)
			}
			if got := chatter.streamReq.CanonicalModel; got != "m-1" {
				t.Errorf("计费请求的 CanonicalModel = %q，期望规范名 m-1", got)
			}

			// 逐帧看：三条协议的帧形状不同（chat 每块带 model，responses 只在
			// 首帧的 response 对象里带一次），所以按「这一帧里出现的每一个 model
			// 取值」判，而不是写死它在第几层。至少出现一次也要断——一帧都没有
			// model 时上面那两条字符串判据仍然可能命中别处。
			seen := 0
			for _, frame := range parseFrames(t, rec.Body.String()) {
				var payload map[string]any
				if err := json.Unmarshal([]byte(frame.data), &payload); err != nil {
					continue
				}
				for _, got := range collectModels(payload) {
					seen++
					if got != "fast" {
						t.Errorf("流里出现 model = %q，期望调用方写的 fast（整帧 %s）", got, frame.data)
					}
				}
			}
			if seen == 0 {
				t.Errorf("流里一帧都没有 model，期望回显调用方写的 fast：\n%s", rec.Body)
			}
			if strings.Contains(rec.Body.String(), `"model":"m-1"`) {
				t.Errorf("流里出现了规范名 m-1，期望回显调用方写的 fast:\n%s", rec.Body)
			}
		})
	}
}

// collectModels 把一帧 JSON 里所有名为 model 的取值收集出来，不论在第几层。
//
// 按名字递归而不是写死路径：三条协议的帧层级不同，写死一条路径时另外两条
// 会以「一帧都没收到」的形式静默通过。
func collectModels(v any) []string {
	var out []string
	switch obj := v.(type) {
	case map[string]any:
		for key, val := range obj {
			if key == "model" {
				if name, ok := val.(string); ok {
					out = append(out, name)
				}
				continue
			}
			out = append(out, collectModels(val)...)
		}
	case []any:
		for _, item := range obj {
			out = append(out, collectModels(item)...)
		}
	}
	return out
}

// 没装解析器时行为与别名出现之前完全一致：请求里的名字原样往下走。
//
// 这一条与上一条是一对：只有它们同时成立，「nil 时不做解析」才不是一句
// 自我声明。
func TestInboundNilResolverKeepsRequestName(t *testing.T) {
	h, _, chatter := newTestHandler()

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	if got := chatter.upstream.Model; got != "m-1" {
		t.Errorf("发往上游的 Model = %q，期望请求里那个 m-1", got)
	}
	if got := chatter.req.CanonicalModel; got != "m-1" {
		t.Errorf("计费请求的 CanonicalModel = %q，期望 m-1", got)
	}
}

// 解析失败 fail closed：500，且响应体不带内部细节。
//
// 吞成「原样放行」的现场是：请求拿别名去选路，报「查无此模型」——排查方向
// 整个指向另一边，而真正的原因（一次数据库故障）没有任何人看见。
func TestInboundResolveFailureFailsClosed(t *testing.T) {
	h, _, chatter, inbound := inboundHarness()
	inbound.err = errors.New("库连不上")

	rec := post(h, PathChatCompletions, validBody)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if chatter.calls != 0 || chatter.streamCalls != 0 {
		t.Error("解析失败时上游不该被调到：那意味着请求被原样放行了")
	}
	if strings.Contains(rec.Body.String(), "库连不上") {
		t.Errorf("响应体带上了内部细节: %s", rec.Body)
	}
}

// 幂等指纹按解析后的请求算：别名与规范名两种写法是同一个指纹。
//
// 解析若挪到 serveIdempotent 之后，两条请求会算出两个指纹，第二次拿到 409
// ——而两次请求的内容逐字相同，调用方看不出为什么。
func TestInboundAliasSharesIdempotencyFingerprint(t *testing.T) {
	h, _, _, inbound := inboundHarness()
	chatter := &fakeChatter{result: fakeResult()}
	h.Chatter = chatter

	// 走一次真实的入口，让 handler 把解析做完，再拿它交出去的那份请求算指纹
	// ——自己解析一份再算，那条断言与入口无关，证明不了「入口先解析」。
	aliasBody := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
	rec := post(h, PathChatCompletions, aliasBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	if inbound.calls != 1 {
		t.Fatalf("解析被调用 %d 次，期望 1 次", inbound.calls)
	}

	fromAlias, err := idempotency.HashRequest(channel.ProtocolOpenAI, chatter.upstream)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	// 对照组：同一份请求体、把 model 换成规范名。
	fromCanonical := hashOf(t, channel.ProtocolOpenAI,
		`{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)

	if fromAlias != fromCanonical {
		t.Errorf("别名与规范名两种写法算出的指纹不同：\n  别名 %x\n  规范名 %x\n"+
			"解析排在幂等指纹之后时就是这个样子——同一个逻辑请求会被当成两个", fromAlias, fromCanonical)
	}
}

// 撞键重试那一路要拿回规范名。
//
// # 这条防的是什么
//
// serveIdempotent 在键被并发请求抢走时会重解析 body 再重进一次 serveOnce。
// 重解析出来的是**调用方写的名字**，而入口那一次解析发生在 serve 里，不会
// 跟着过去。少了补回这一步，第二轮会拿别名去选路——两次尝试落在两个不同的
// 模型上，而第一次什么都没做（预留整个回滚），所以最终结果是「请求按一个
// 不存在的模型名失败」。
//
// # 幂等命中重放那一路不用补
//
// resolveExisting 直接重放第一次写出的响应字节，不重新选路——那不是漏了，
// 重放的就是当时那份答案。
func TestInboundAliasSurvivesIdempotencyRetry(t *testing.T) {
	// 两个候选都造出来：少了别名那个，重试一旦拿别名去选路就会在**选路**处
	// 失败（502 没有可用渠道），于是用例红在状态码上——那证明的是「请求发不
	// 出去」，而不是这条要钉的「重试那一次用了哪个名字」。两个候选都在时，
	// 两种实现都能发出请求，差别只落在上游收到的 model 上。
	h := newPluginHarness(t, pluginRegistry(t, "", `onRequest(){ return; }`),
		pluginCandidate(1, "m-1"), pluginCandidate(2, "fast"))
	inbound := &fakeInbound{aliases: map[string]string{"fast": "m-1"}}
	h.handler.Inbound = inbound

	// 第一次预留落库时键已被并发请求占住，第二次查仍然没有可重放的记录，
	// 于是原路径再走一遍（与 TestOnRequestPatchDoesNotStackAcrossRetry 同一个夹具）。
	h.reservations.claimErr = idempotency.ErrClaimed
	h.reservations.claimTimes = 1
	h.handler.Idempotency = &fakeIdem{script: []idemReply{
		{err: idempotency.ErrNotFound},
		{err: idempotency.ErrNotFound},
	}}

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`
	rec := postIdem(t, h.handler, PathChatCompletions, body, "key-inbound-retry")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（撞键不是失败）。响应体 %s", rec.Code, rec.Body)
	}

	if got := h.reservations.calls; got != 2 {
		t.Fatalf("预留落库被调用 %d 次，期望 2 次——这条用例的全部内容就是那第二次。"+
			"只调用了一次说明重试没发生，下面的断言也就什么都没钉住", got)
	}
	if len(h.executor.reqs) != 1 {
		t.Fatalf("上游被调用 %d 次，期望 1 次（第一次撞键时预留整个回滚）", len(h.executor.reqs))
	}
	if got := h.executor.reqs[0].Model; got != "m-1" {
		t.Errorf("重试那一次发给上游的 Model = %q，期望规范名 m-1。"+
			"拿到 fast 说明重解析出来的请求没有补回规范名", got)
	}
	if inbound.calls != 1 {
		t.Errorf("解析被调用 %d 次，期望 1 次：重试那一路不许再解析一遍", inbound.calls)
	}

	// 回显仍是调用方写的名字，重试那一次也一样。
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body)
	}
	if resp.Model != "fast" {
		t.Errorf("响应体的 model = %q，期望 fast", resp.Model)
	}
}

// ── /v1/systemone ──────────────────────────────────────────────────────
//
// 决策表面是**第四条**按模型名选路并计费的入口，但它不走 serve 而是走
// serveDecision，所以解析要在那里单独接一次。少了这一处，同一个别名在三条
// chat 路径上能解析、在这条上不能——配了却不生效，而且只在一条表面上现形。

// decisionBodyFor 造一条决策请求体，只让调用方改模型名。
func decisionBodyFor(model string) string {
	return `{"model":` + strconv.Quote(model) + `,
		"state": "工单: 用户说付款后没到账",
		"questions": {"q": {"type": "noul", "instructions": "这是账单问题吗"}}}`
}

// 别名请求在决策表面上：选路与计费拿规范名，响应回显调用方写的名字。
//
// 两半一起断言，与 chat 那条同一个理由：只断上游收到规范名，会放过「响应
// 也回了规范名」；只断回显，会放过「选路还按别名走」。
func TestSystemOneResolvesInboundAlias(t *testing.T) {
	h, d := newDecisionHandler()
	h.Inbound = &fakeInbound{aliases: map[string]string{"fast": "m-1"}}

	rec := post(h, PathSystemOne, decisionBodyFor("fast"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	if d.calls != 1 {
		t.Fatalf("决策执行器被调了 %d 次，期望 1", d.calls)
	}

	// 选路用的就是 CanonicalModel（见 gateway.Decide），所以它必须是规范名。
	if got := d.req.CanonicalModel; got != "m-1" {
		t.Errorf("计费与选路用的规范名 = %q，期望 m-1：拿到 fast 说明这条表面没解析别名",
			got)
	}
	// 上游收到的决策请求里的模型名同样是规范名。
	if got := d.decision.Model; got != "m-1" {
		t.Errorf("发给上游的模型名 = %q，期望规范名 m-1", got)
	}

	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body)
	}
	if resp.Model != "fast" {
		t.Errorf("响应体的 model = %q，期望调用方写的 %q：别名存在的意义就是客户端有一个稳定的名字",
			resp.Model, "fast")
	}
}

// 没装解析器时这条表面的行为与别名出现之前逐字相同。
func TestSystemOneWithoutInboundKeepsRequestName(t *testing.T) {
	h, d := newDecisionHandler()

	rec := post(h, PathSystemOne, decisionBodyFor("decision-model"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}
	if got := d.req.CanonicalModel; got != "decision-model" {
		t.Errorf("规范名 = %q，期望请求里那个 decision-model", got)
	}
	if got := d.decision.Model; got != "decision-model" {
		t.Errorf("发给上游的模型名 = %q，期望 decision-model", got)
	}

	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body)
	}
	if resp.Model != "decision-model" {
		t.Errorf("响应体的 model = %q，期望 decision-model", resp.Model)
	}
}

// 解析失败在决策表面上同样 fail closed：500、不调上游、不回退成原样。
func TestSystemOneResolveFailureFailsClosed(t *testing.T) {
	h, d := newDecisionHandler()
	inbound := &fakeInbound{aliases: map[string]string{"fast": "m-1"}}
	inbound.err = errors.New("库连不上")
	h.Inbound = inbound

	rec := post(h, PathSystemOne, decisionBodyFor("fast"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（响应体 %s）", rec.Code, rec.Body)
	}
	if d.calls != 0 {
		t.Error("解析失败时上游不该被调到：那意味着请求被原样放行了")
	}
	if strings.Contains(rec.Body.String(), "库连不上") {
		t.Errorf("响应体带上了内部细节: %s", rec.Body)
	}
}

// 解析排在 Validate **之后**：缺 model 的请求拿 400，不是解析层的 500。
//
// 挪到前面时，一个 `model: ""` 的请求会先被解析层拒成 500（空名字是错误），
// 调用方看到的是「网关坏了」，而真正的原因是它自己少写了一个字段——两种结论
// 的下一步完全不同。
func TestSystemOneValidatesBeforeResolving(t *testing.T) {
	h, d := newDecisionHandler()
	inbound := &fakeInbound{aliases: map[string]string{"fast": "m-1"}}
	h.Inbound = inbound

	body := `{"state":"s","questions":{"q":{"type":"noul","instructions":"i"}}}`
	rec := post(h, PathSystemOne, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400「model is required」（响应体 %s）——500 说明解析"+
			"排在了 Validate 前面", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model is required") {
		t.Errorf("响应体 = %s，期望点名 model 这个字段", rec.Body)
	}
	if inbound.calls != 0 {
		t.Errorf("解析被调用 %d 次，期望 0 次：字段都不全的请求不该为它多一次查询", inbound.calls)
	}
	if d.calls != 0 {
		t.Error("校验没过的请求调到了上游")
	}
}

// 插件看到的是**入站解析之后**的规范名，不是调用方写的名字。
//
// # 这条防的是什么
//
// 插件在 onRequest 拿到的三个模型名（ctx.canonicalModel / ctx.upstreamModel /
// ctx.body.model）都由入口那份 upstream.Model 派生。入口先解析别名、再进
// serveOnce，所以三个都该是规范名。解析挪到插件之后（或者在 hook 那一路改回
// 传调用方原名）时，注册表的 meta.models 匹配、插件按模型名写的判断、以及它
// 从 body.model 读到的东西会一起落到别名上——而请求照样成功、账单照样按规范名
// 记，只有插件的判断是错的。这类错在响应上完全看不出来。
//
// # 为什么用请求头带出来
//
// 三个值要一起看。用 log() 带出来要另接一个宿主能力，而请求头那条路是现成的
// （插件加的 x-larkax-* 头会落进 channel.Request.Headers），并且顺带证明了
// 「插件读到的那份 body 就是它会改的那份请求」。
func TestInboundAliasVisibleToPluginAsCanonical(t *testing.T) {
	h := newPluginHarness(t, pluginRegistry(t, "", `onRequest(ctx){
		return { headers: {
			"x-larkax-canonical": String(ctx.canonicalModel),
			"x-larkax-upstream": String(ctx.upstreamModel),
			"x-larkax-body": String(ctx.body.model),
		} };
	}`))
	inbound := &fakeInbound{aliases: map[string]string{"fast": "m-1"}}
	h.handler.Inbound = inbound

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`
	rec := post(h.handler, PathChatCompletions, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if len(h.executor.reqs) != 1 {
		t.Fatalf("上游被调用 %d 次，期望 1 次", len(h.executor.reqs))
	}

	headers := h.executor.reqs[0].Headers
	for _, tc := range []struct{ name, key string }{
		{"ctx.canonicalModel", "x-larkax-canonical"},
		{"ctx.upstreamModel", "x-larkax-upstream"},
		{"ctx.body.model", "x-larkax-body"},
	} {
		if got := headers[tc.key]; got != "m-1" {
			t.Errorf("插件拿到的 %s = %q，期望入站解析之后的规范名 m-1："+
				"拿到 fast 说明入口在插件之后才解析别名，插件按模型名做的判断全部落在别名上",
				tc.name, got)
		}
	}

	// 回显仍是调用方写的名字：插件那一侧用规范名，不等于响应也换成规范名。
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body)
	}
	if resp.Model != "fast" {
		t.Errorf("响应体的 model = %q，期望调用方写的 fast", resp.Model)
	}
}
