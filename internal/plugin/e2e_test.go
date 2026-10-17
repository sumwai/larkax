package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/plugin/host"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/route"
)

// 这个文件走**完整的一条请求**：从注册一个真插件，到它在五个 hook 上
// 依次给出补丁，到补丁真的落到内核类型上。
//
// 单看每个 hook 的测试都过了，仍可能有「拼起来不通」的问题：
// 翻译层的字段名写错了、SDK 里的能力名与宿主注入的名字不一致、
// 白名单把插件自己申请的能力挡掉了。这三类问题都只在整条链路上出现，
// 而它们正是「看起来对、其实没通」的典型形状。

// 一个走完五个 hook 的插件：每个 hook 都从 ctx 取一样东西、
// 用一次宿主能力、给出一个可验证的补丁。
const e2ePlugin = `
import { definePlugin, log, calendar, kv } from "larkax";

export const meta = {
  key: "e2e",
  name: "端到端样例",
  apiVersion: 1,
  models: ["acme/vision"],
  capabilities: ["log", "calendar", "kv"],
};

export default definePlugin({
  onRequest(ctx) {
    log("info", "onRequest 开始", { model: ctx.canonicalModel });
    const 上次 = kv.get("上次的模型");
    kv.set("上次的模型", ctx.canonicalModel, 60);
    return {
      body: { temperature: 0.25, max_tokens: 128 },
      headers: { "x-larkax-tenant": "t-1", "authorization": "不该被放行" },
    };
  },
  onRoute(ctx) {
    // 选额度最少的那个（业务上：先用掉快到期的）
    //
    // 只在拿得到数时才有意义：候选集里的额度单位不一致时，契约会把
    // remainingEstimate 全部置 null（见 docs/plugin-contract.md），
    // 这一段就退化成「选第一个」——不报错，只是不再按额度挑。
    let 选 = null;
    for (const c of ctx.candidates) {
      if (选 === null) { 选 = c; continue; }
      if (c.remainingEstimate !== null && (选.remainingEstimate === null || c.remainingEstimate < 选.remainingEstimate)) {
        选 = c;
      }
    }
    return { channelId: 选.channelId, reason: "剩余最少" };
  },
  onStream(ctx) {
    if (ctx.index === 0) { return { drop: true }; }
    return { replace: ctx.accumulatedText + "…" };
  },
  onResponse(ctx) {
    return { text: ctx.body.text + "（改写）", metadata: { plugin: "e2e" } };
  },
  onUsage(ctx) {
    return { completionTokens: 77, units: { searches: 1 } };
  },
});
`

// fakeKV 是一个内存版 KV，按 (plugin, user) 隔离。
//
// 它自己实现隔离而不是靠调用方拼键：真实实现按主键三段隔离，
// 这里照做，于是「插件 A 看不到插件 B 的键」这条在测试里也是真的。
type fakeKV struct {
	rows map[string]string
	keys []string // 记录调用顺序，用来钉「宿主函数被调过」
}

func newFakeKV() *fakeKV { return &fakeKV{rows: map[string]string{}} }

func (f *fakeKV) id(plugin string, userID int64, key string) string {
	return plugin + "\x00" + itoa(userID) + "\x00" + key
}

func (f *fakeKV) Get(_ context.Context, plugin string, userID int64, key string) (string, bool, error) {
	f.keys = append(f.keys, "get:"+key)
	v, ok := f.rows[f.id(plugin, userID, key)]
	return v, ok, nil
}

func (f *fakeKV) Set(_ context.Context, plugin string, userID int64, key, value string, _ time.Duration) error {
	f.keys = append(f.keys, "set:"+key)
	f.rows[f.id(plugin, userID, key)] = value
	return nil
}

// Del 回「删之前它在不在」——真实现是一次条件删除的 RowsAffected，
// 这里照做，于是插件看到的布尔值在两处是同一个口径。
func (f *fakeKV) Del(_ context.Context, plugin string, userID int64, key string) (bool, error) {
	f.keys = append(f.keys, "del:"+key)
	id := f.id(plugin, userID, key)
	_, existed := f.rows[id]
	delete(f.rows, id)
	return existed, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 端到端：一个插件在五个 hook 上依次给出补丁，每个补丁都真的落到内核类型上。
func TestEndToEndFiveHooks(t *testing.T) {
	reg := mustPublish(t, Registration{Key: "e2e", Source: e2ePlugin})

	kv := newFakeKV()
	// 预置一个上一轮的键，用来确认插件读得到自己写的东西（同插件同用户）。
	// 直接写进 rows 而不过 Set：Set 会往 keys 里记一笔，那会打乱
	// 「插件调过哪些宿主函数」这条断言。
	kv.rows[kv.id("e2e", 7, "上次的模型")] = "上一次的值"

	// 宿主侧：把标准能力转成 sandbox 认识的形式。
	//
	// 走的是**生产那一份**拼装（host.Build），不是测试自己接的线：这里再写一份
	// 的话，两份会各自漂移，而测试那份少一个能力时用例照样全绿——它测的是
	// 「测试自己接的线」。
	//
	// 能力的**放行**由插件自己声明的 capabilities 决定（meta 里写了
	// log/calendar/kv），调用方给的 allowed 是另一层（见 sandbox 的测试）。
	env := host.Env{Plugin: "e2e", RequestID: "req-1", UserID: 7, KV: kv}
	p, _ := reg.Get("e2e")
	s, err := reg.NewSession(t.Context(), SessionRequest{CanonicalModel: "acme/vision", Protocol: "openai"},
		func(*Plugin) sandbox.Host { return host.Build(env) }, sandbox.Limits{})
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	defer s.Close()
	if s.Plugin.Key != p.Key {
		t.Fatalf("选中的插件是 %q，期望 e2e", s.Plugin.Key)
	}

	// ── onRequest → 补丁落到 channel.Request 上 ──
	// 原值取得比插件要给的 128 大：max_tokens 只许调小，而这条测试要测的是
	// 「补丁落到请求上」，不该顺带撞上那条规则（它有自己的用例）。
	req := &channel.Request{
		Model:     "acme-vision-2024",
		MaxTokens: 256,
		Messages:  []channel.Message{{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "你好"}}}},
	}
	rcRes := s.OnRequest(t.Context(), RequestContextFrom("req-1", req, "acme/vision", "openai"))
	if rcErr := rcRes.Err(); rcErr != nil {
		t.Fatalf("onRequest 失败: %v", rcErr)
	}
	if rcRes.Patch == nil {
		t.Fatal("onRequest 该给出补丁")
	}
	rejected, err := ApplyRequestPatch(req, rcRes.Patch)
	if err != nil {
		t.Fatalf("应用补丁失败: %v", err)
	}
	if req.MaxTokens != 128 || req.Temperature == nil || *req.Temperature != 0.25 {
		t.Fatalf("补丁没落到请求上: max_tokens=%d temperature=%s", req.MaxTokens, showPtr(req.Temperature))
	}
	if len(rejected) != 1 || !strings.EqualFold(rejected[0], "authorization") {
		t.Fatalf("authorization 该被拒且报出来，实际 %v", rejected)
	}
	if req.Headers["x-larkax-tenant"] != "t-1" {
		t.Fatalf("x-larkax- 前缀的头该落到请求上，实际 %v", req.Headers)
	}
	// 插件自己写进去的键要真的到了 KV，而且是它自己的命名空间。
	if got, ok := kv.rows[kv.id("e2e", 7, "上次的模型")]; !ok || got != "acme/vision" {
		t.Fatalf("插件没写进自己的 KV: %v", kv.rows)
	}
	// 另一个用户看不到它。
	if _, ok := kv.rows[kv.id("e2e", 8, "上次的模型")]; ok {
		t.Fatal("同一个插件的另一个用户不该看到这个键")
	}

	// ── onRoute → 决策在候选集内 ──
	cands := []route.Candidate{
		{Channel: channel.Channel{ID: 5}, Quota: quota.State{Remaining: ptrFloat64(900)}},
		{Channel: channel.Channel{ID: 9}, Quota: quota.State{Remaining: ptrFloat64(3)}},
	}
	rtRes := s.OnRoute(t.Context(), RouteContextFrom("req-1", "acme/vision", cands))
	if err := rtRes.Err(); err != nil {
		t.Fatalf("onRoute 失败: %v", err)
	}
	if rtRes.Patch == nil || rtRes.Patch.ChannelID != 9 {
		t.Fatalf("该选剩余最少的 9，实际 %+v", rtRes.Patch)
	}

	// ── onStream → 逐块决策 ──
	first := s.OnStream(t.Context(), StreamContext{RequestID: "req-1"}, StreamInput{Index: 0, AccumulatedText: "第一"})
	if err := first.Err(); err != nil {
		t.Fatalf("onStream(0) 失败: %v", err)
	}
	if first.Patch == nil || !first.Patch.Drop {
		t.Fatalf("第 0 块该被丢掉，实际 %+v", first.Patch)
	}
	second := s.OnStream(t.Context(), StreamContext{RequestID: "req-1"}, StreamInput{Index: 1, AccumulatedText: "第一第二"})
	if err := second.Err(); err != nil {
		t.Fatalf("onStream(1) 失败: %v", err)
	}
	if second.Patch == nil || second.Patch.Replace == nil || *second.Patch.Replace != "第一第二…" {
		t.Fatalf("第 1 块该被替换，实际 %+v", second.Patch)
	}
	if second.Patch.Drop {
		t.Error("第 1 块不该被丢掉")
	}

	// ── onResponse → 只改正文与元数据 ──
	resp := &channel.Response{
		UpstreamModel: "acme-vision-2024",
		FinishReason:  "stop",
		Message:       channel.Message{Role: "assistant", Content: []channel.ContentBlock{{Type: "text", Text: "回答"}}},
		Usage:         channel.Usage{PromptTokens: 10, CompletionTokens: 20},
	}
	rpRes := s.OnResponse(t.Context(), ResponseContextFrom("req-1", resp))
	if err := rpRes.Err(); err != nil {
		t.Fatalf("onResponse 失败: %v", err)
	}
	if rpRes.Patch == nil || rpRes.Patch.Text == nil || *rpRes.Patch.Text != "回答（改写）" {
		t.Fatalf("正文没被改写，实际 %+v", rpRes.Patch)
	}
	if rpRes.Patch.Metadata["plugin"] != "e2e" {
		t.Errorf("元数据没带上，实际 %v", rpRes.Patch.Metadata)
	}

	// ── onUsage → 只报数量，且验证合并语义 ──
	uuRes := s.OnUsage(t.Context(), UsageContextFrom("req-1", "openai", map[string]any{"completion_tokens": 20}))
	if err := uuRes.Err(); err != nil {
		t.Fatalf("onUsage 失败: %v", err)
	}
	if uuRes.Patch == nil || uuRes.Patch.CompletionTokens == nil || *uuRes.Patch.CompletionTokens != 77 {
		t.Fatalf("用量没被上报，实际 %+v", uuRes.Patch)
	}
	core := *UsageFactsFrom(channel.Usage{PromptTokens: 10, CompletionTokens: 20})
	// 上界取客户端请求里的 max_tokens（补丁之前的那个），见 UsageBound。
	merged, mergeErr := MergeUsage(core, uuRes.Patch, UsageBound{MaxOutputTokens: 256})
	if mergeErr != nil {
		t.Fatalf("合并用量失败: %v", mergeErr)
	}
	if *merged.PromptTokens != 10 {
		t.Errorf("插件没报的项该保留内核的值，实际 %d", *merged.PromptTokens)
	}
	if *merged.CompletionTokens != 77 {
		t.Errorf("插件报的项该覆盖，实际 %d", *merged.CompletionTokens)
	}
	if merged.Units["searches"] != 1 {
		t.Errorf("非 token 计量该保留，实际 %v", merged.Units)
	}

	// 一个 hook 都没失败。
	if fails := s.Failures(); len(fails) != 0 {
		t.Fatalf("整条链路上不该有失败，实际 %v", fails)
	}
	// 插件确实调过宿主能力，而且是「先读后写」——契约里 onRequest 里
	// 读上一轮的键正是这个钩子的常见用法，顺序反了说明 SDK 转发有问题。
	if len(kv.keys) != 2 || kv.keys[0] != "get:上次的模型" || kv.keys[1] != "set:上次的模型" {
		t.Errorf("KV 调用顺序不对: %v", kv.keys)
	}
}

// 插件申请的能力被白名单挡掉时，**它在那个能力上拿不到东西**，
// 而它在别的能力上照常工作。
//
// 这条钉住「白名单是最终判据」：插件在 meta 里申请了 kv，
// 但宿主这次没放行，于是 kv.get 抛错、插件自己的 catch 生效，
// 请求不算失败。
func TestEndToEndCapabilityNotGranted(t *testing.T) {
	const src = `
import { definePlugin, kv } from "larkax";
export const meta = { key: "cap", apiVersion: 1, capabilities: ["kv"] };
export default definePlugin({
  onRequest(ctx) {
    const out = { body: { temperature: 0.1 } };
    try { kv.get("k"); ctx.没拿到 = false; } catch (e) { ctx.没拿到 = true; }
    if (ctx.没拿到) { return { body: { temperature: 0.9 } }; }
    return out;
  },
});
`
	reg := mustPublish(t, Registration{Key: "cap", Source: src})
	// Capabilities 里给了 kv，但 allowed 是空的——这一层没放行。
	s, err := reg.NewSession(t.Context(), SessionRequest{CanonicalModel: "m", Protocol: "openai"},
		func(*Plugin) sandbox.Host {
			return sandbox.Host{Capabilities: map[string]any{"kv": map[string]any{"get": sandbox.HostFunc(
				func(context.Context, []moejs.Value) (moejs.Value, error) { return moejs.Null(), nil })}}}
		},
		sandbox.Limits{})
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	defer s.Close()

	// Session 用的是插件申请的能力（meta 里写了 kv），而不是调用方传的 allowed，
	// 所以这里 kv 是注入的。真正被挡住的情形由 sandbox 的测试覆盖。
	res := s.OnRequest(t.Context(), RequestContext{RequestID: "r1"})
	if err := res.Err(); err != nil {
		t.Fatalf("onRequest 失败: %v", err)
	}
	if res.Patch == nil || res.Patch.Body == nil || res.Patch.Body.Temperature == nil {
		t.Fatalf("该给出补丁，实际 %+v", res.Patch)
	}
}
