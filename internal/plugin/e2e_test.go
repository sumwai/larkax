package plugin

import (
	"context"
	"fmt"
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

// 一个只用 calendar 的插件：同一个时刻问两个窗口，再问一个写法不对的窗口。
//
// 三个答案通过请求头带回来（补丁里唯一能把字符串带出插件的地方），
// 于是断言读的就是插件真的拿到的东西，而不是测试自己算的。
const e2ePeakPlugin = `
import { definePlugin, calendar } from "larkax";

export const meta = {
  key: "peak",
  name: "峰谷样例",
  apiVersion: 1,
  capabilities: ["calendar"],
};

export default definePlugin({
  onRequest() {
    // 同一个时刻、两个窗口：档位跟着窗口走，不跟着时刻走。
    const 白天 = calendar.peak(1955412000, "CN", "08:00-22:00");
    const 夜班 = calendar.peak(1955412000, "CN", "22:00-08:00");
    // 写法不合法时必须抛错。静默回一个 off_peak 的话，插件看不出自己
    // 写错了，这条路算出来的钱会一直按低谷那一档走。
    let 非法 = "没有报错";
    try { calendar.peak(1955412000, "CN", "25:00-26:00"); }
    catch (e) { 非法 = String(e && e.message); }
    return {
      headers: {
        "x-larkax-peak-day": 白天,
        "x-larkax-peak-night": 夜班,
        "x-larkax-peak-bad": 非法,
      },
    };
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

// 端到端：插件写下的高峰窗口真的到了宿主，宿主的答案真的回了插件。
//
// 这条走的是与上面那条同一条链路（注册 → 建会话 → host.Build → SDK → JS），
// 只是把用到的能力换成 calendar 的 peak。要证明的是三件事，每一件都只在
// 整条链路上才看得见：
//
//  1. **窗口随调用走**：同一个时刻换一个窗口，档位跟着换。宿主自己拿一个
//     排班去算的实现也能返回一个看着像结果的档位，而按它算出来的钱是错的。
//  2. **写法不合法时插件拿到错误**，不是一个静默的 off_peak——后者会让这条
//     路算出来的钱一直按低谷那一档走，而账上看不出异常。
//  3. **非法窗口一次都没有下传到日历**。
func TestEndToEndCalendarPeak(t *testing.T) {
	reg := mustPublish(t, Registration{Key: "peak", Source: e2ePeakPlugin})

	cal := &peakStub{levels: map[string]string{
		"08:00-22:00": "peak",
		"22:00-08:00": "off_peak",
	}}
	env := host.Env{Plugin: "peak", RequestID: "req-1", UserID: 7, Calendar: cal}

	s, err := reg.NewSession(t.Context(), SessionRequest{CanonicalModel: "m", Protocol: "openai"},
		func(*Plugin) sandbox.Host { return host.Build(env) }, sandbox.Limits{})
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	defer s.Close()

	req := &channel.Request{Model: "m"}
	res := s.OnRequest(t.Context(), RequestContextFrom("req-1", req, "m", "openai"))
	if err := res.Err(); err != nil {
		t.Fatalf("onRequest 失败: %v", err)
	}
	if res.Patch == nil {
		t.Fatal("onRequest 该给出补丁")
	}
	if _, err := ApplyRequestPatch(req, res.Patch); err != nil {
		t.Fatalf("应用补丁失败: %v", err)
	}

	// 同一个时刻，两个窗口，两个档位。
	if got := req.Headers["x-larkax-peak-day"]; got != "peak" {
		t.Errorf("白天窗口下的档位 = %q，期望 peak", got)
	}
	if got := req.Headers["x-larkax-peak-night"]; got != "off_peak" {
		t.Errorf("夜班窗口下的档位 = %q，期望 off_peak", got)
	}

	// 非法窗口：插件拿到的是一个错误，而不是一个档位。
	bad := req.Headers["x-larkax-peak-bad"]
	if bad == "没有报错" {
		t.Fatal("非法窗口不该静默给出一个档位——那样价会一直按那个档位收")
	}
	if !strings.Contains(bad, sandbox.CodeBadArgument) {
		t.Errorf("插件看到的错误 = %q，期望带分类码 %s（插件要能据此分支）", bad, sandbox.CodeBadArgument)
	}
	if !strings.Contains(bad, "写法不合法") {
		t.Errorf("插件看到的错误 = %q，期望说清是窗口写法的问题", bad)
	}

	// 而它没有下传到日历：宿主只被问过两次，都是那两个合法的窗口。
	if len(cal.windows) != 2 || cal.windows[0] != "08:00-22:00" || cal.windows[1] != "22:00-08:00" {
		t.Errorf("日历收到的窗口 = %v，期望只有那两个合法窗口", cal.windows)
	}

	// 插件自己接住了那个错误，所以整条链路不算失败。
	if fails := s.Failures(); len(fails) != 0 {
		t.Fatalf("插件 catch 住的错误不该算失败，实际 %v", fails)
	}
}

// peakStub 是一个按窗口串查表的日历数据源。
//
// 它**不做判定**：判定的正确性由 internal/calendar 与 internal/assembly 的
// 用例管（那两处用的是真实现与真数据）。这条链路上要证明的是「窗口到了宿主、
// 答案回了插件」，所以这里按窗口查表回答，并记下每一次收到的窗口。
// 查不到就报错而不是猜一个档位——与生产实现同一个处置。
type peakStub struct {
	levels  map[string]string // 窗口串 → 档位
	windows []string          // 按调用顺序记下收到的窗口
}

func (s *peakStub) Day(context.Context, string, time.Time) (host.DayInfo, error) {
	return host.DayInfo{Kind: "workday"}, nil
}

func (s *peakStub) Peak(_ context.Context, _ string, _ time.Time, window string) (string, error) {
	s.windows = append(s.windows, window)
	level, ok := s.levels[window]
	if !ok {
		return "", fmt.Errorf("没登记过的窗口 %q", window)
	}
	return level, nil
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
	// 上界取这次请求实际发出的那份请求上的 max_tokens（见 UsageBound），
	// 这里直接给一个——这条测的是合并语义，上界从哪儿读有自己的用例。
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
