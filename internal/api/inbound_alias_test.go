package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
func TestInboundAliasResolvesToCanonicalButEchoesRequested(t *testing.T) {
	h, _, chatter, inbound := inboundHarness()

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
	rec := post(h, PathChatCompletions, body)
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
}

// 流式那条路与非流式同一个口径：帧里的 model 也是调用方写的名字。
//
// 两处分叉时的现场最难查——非流式绿、流式红，而两边渲染器收的是同一个
// 参数名，改了一处很容易漏另一处。
func TestInboundAliasEchoedInStreamFrames(t *testing.T) {
	h, _, chatter, _ := inboundHarness()
	chatter.streamResult = &gateway.Result{Events: streamOf()}

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`
	rec := post(h, PathChatCompletions, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body)
	}

	if got := chatter.upstream.Model; got != "m-1" {
		t.Errorf("发往上游的 Model = %q，期望规范名 m-1", got)
	}
	if got := chatter.streamReq.CanonicalModel; got != "m-1" {
		t.Errorf("计费请求的 CanonicalModel = %q，期望规范名 m-1", got)
	}
	if strings.Contains(rec.Body.String(), `"model":"m-1"`) {
		t.Errorf("流里出现了规范名 m-1，期望回显调用方写的 fast:\n%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"model":"fast"`) {
		t.Errorf("流里没有调用方写的 fast:\n%s", rec.Body)
	}
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
