package api

import (
	"context"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/route"
)

// 这个文件钉住 onRequest / onResponse 两个挂载点的**效果**。
//
// # 为什么这里要接一个真的 gateway
//
// 预留金额是 pipeline.Reserve 算出来的，而「插件改了 max_tokens 之后上界
// 是多少」正是要钉住的东西。换成假 Chatter 就只测得到「入口把补丁交给了谁」
// ——那测的是调用发生，不是效果：补丁被应用了但预留没看见它，响应上完全
// 看不出来，而钱那一侧每次都多冻一笔。

// ── 计费链路上的桩 ─────────────────────────────────────────────────────

// pluginRule 只给输出侧定价，输入侧显式写 0。
//
// 于是预留上界**只**由 max_tokens 决定（见 billing.Reserve 的输入侧与输出侧），
// 用例可以用字面量断言金额，不必把分词估算抄进来——抄进来的话，断言与被测
// 对象一起变，它就不再钉住任何东西。
const pluginRule = `{"prompt": 0, "completion": 0.000002}`

// pluginNow 是这条链路上唯一的时刻。它必须是固定的：规则可以按时间分档，
// 而一个随当前时间变化的预留金额没法用字面量断言。
var pluginNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type pluginRules struct{ src string }

func (f pluginRules) Match(_ context.Context, _ pipeline.RuleQuery) (pipeline.Rule, error) {
	return pipeline.Rule{ID: 1, Version: 1, Source: f.src}, nil
}

func (f pluginRules) MatchAll(_ context.Context, q pipeline.RuleQuery) ([]pipeline.Rule, error) {
	out := make([]pipeline.Rule, 0, len(q.Candidates))
	for range q.Candidates {
		out = append(out, pipeline.Rule{ID: 1, Version: 1, Source: f.src})
	}
	return out, nil
}

type pluginCandidates struct{ cands []route.Candidate }

func (f *pluginCandidates) Candidates(context.Context, string) ([]route.Candidate, error) {
	return f.cands, nil
}

// pluginReservations 记下每一次预留。金额是这条链路上最该被钉住的东西，
// 所以这里存的是整条预留记录而不只是一个计数。
type pluginReservations struct {
	saved    []*pipeline.Reservation
	released []pipeline.Request

	// added 是 AddReservation（流式递进冻结的补冻）被调用的次数。
	//
	// 这些用例里它按前提应当是 0（见那个方法的注释）：夹具的阈值段（65536）
	// 远大于声明的 max_tokens，初始那一笔就把整笔冻满。断言在 plugin_stream_test.go
	// 的 onStream 用例里（见 TestOnStreamTerminateTruncatesButStillBills）——
	// 记下来就是为了让「前提变了」这件事有一个会红的落点，不能只自增没人读。
	added int

	// claimTimes 是「头几次预留落库时报键已被占住」的次数（claimErr 为 nil
	// 时无意义）。撞键那条路要它：落库层在撞键时回 ErrClaimed，而那一次预留
	// 整个回滚——所以被挡下的那几次**不记进 saved**，与生产里的形状一致
	// （记进去会让「撞键没冻钱」这条不变量在夹具里先破掉）。
	claimTimes int
	claims     int
	claimErr   error

	// calls 是 SaveReservation 被调用的总次数，也就是 serveOnce 走了几遍。
	// 撞键那条路要它：只断言「最终那一份请求是对的」的话，重试根本没发生
	// 时用例照样绿——而「没重试」正是这条用例要防的另一半。
	calls int
}

func (f *pluginReservations) SaveReservation(_ context.Context, _ pipeline.Request, r *pipeline.Reservation) (int64, error) {
	f.calls++
	if f.claims < f.claimTimes {
		f.claims++
		return 0, f.claimErr
	}
	f.saved = append(f.saved, r)
	return int64(len(f.saved)), nil
}

func (f *pluginReservations) Release(_ context.Context, req pipeline.Request, _ *pipeline.UpstreamFailure) error {
	f.released = append(f.released, req)
	return nil
}

// AddReservation 追加冻结额。它在这些用例里**不该被调到**（夹具声明的
// max_tokens 小于递进冻结的阈值段，初始那一笔就冻满了整笔），所以只记一笔
// 调用：真被调到说明组装出来的东西与用例的前提不符，而那是个该当场看见的
// 意外，不是可以静默吞掉的分支。
func (f *pluginReservations) AddReservation(_ context.Context, _ pipeline.Request, _ money.Amount) error {
	f.added++
	return nil
}

type pluginSettlements struct{ saved []*pipeline.Charge }

func (f *pluginSettlements) Save(_ context.Context, _ pipeline.Request, c *pipeline.Charge) error {
	f.saved = append(f.saved, c)
	return nil
}

// pluginExecutor 回一份固定的上游响应，并记下它**实际收到**的请求与渠道。
//
// 渠道也要记：onRoute 的决策**是否生效**只看得到这一个量。请求 200、
// 响应正确、账单也对，而插件选的渠道被丢掉——那是「插件选了一个渠道、
// 请求发去了另一个」，从响应、日志和账上都看不出来。
type pluginExecutor struct {
	reqs     []*channel.Request
	channels []int64
	resp     *channel.Response
	// stream 是流式调用要吐的事件。为 nil 时吐一个 done——那是最短的一条
	// 合法流，不需要正文的用例用它。要测逐块决策就得给出真正的分片。
	stream []channel.StreamEvent
}

func (f *pluginExecutor) Chat(_ context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	f.reqs = append(f.reqs, req)
	f.channels = append(f.channels, ch.ID)
	return f.resp, nil
}

func (f *pluginExecutor) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	if _, err := f.Chat(ctx, ch, req); err != nil {
		return nil, err
	}
	events := f.stream
	if events == nil {
		events = []channel.StreamEvent{{Kind: "done"}}
	}
	return streamOf(events...), nil
}

// ── 组装 ──────────────────────────────────────────────────────────────

type pluginHarness struct {
	handler      *Handler
	reservations *pluginReservations
	settlements  *pluginSettlements
	executor     *pluginExecutor
}

// newPluginHarness 把入口接到一个真的 gateway 上。reg 为 nil 表示插件系统
// 没接上——那也是一种要测的状态（行为与插件出现之前完全一样）。
//
// cands 不给时只有一个候选。要测「插件选的渠道与内核会选的不是同一个」时，
// 必须给两个以上，且顺序由 priority 定死（见 route.order 的第 1 条规则：
// priority 是硬性排序键，不受打分与随机打散影响）。
//
// 别名来源固定成「同名回落、什么都不覆盖」：多数用例不关心别名。要配覆盖的
// 用 newPluginHarnessWith（见 plugin_stream_test.go 的上界用例）。
func newPluginHarness(t *testing.T, reg *plugin.Registry, cands ...route.Candidate) *pluginHarness {
	t.Helper()
	return newPluginHarnessWith(t, reg, identityAliases{}, cands...)
}

// pluginAliases 是夹具用的别名来源：既按渠道把规范名翻成上游名（route 用它
// 把覆盖应用到**实际发出去的那份**请求上），也给出逐渠道的覆盖（gateway 用它
// 算预留上界）。
//
// 两个方法必须由**同一份**配置回答：生产里装配层把 Router.Aliases 与
// Gateway.AliasOverrides 填成同一个 *catalog.Catalog。分成两个来源时，
// 「选路用的覆盖」与「算上界用的覆盖」来自两次不同的读。
type pluginAliases interface {
	route.ModelAliases
	gateway.AliasOverrideSource
}

// newPluginHarnessWith 与 newPluginHarness 同一件事，只是换掉别名来源。
func newPluginHarnessWith(t *testing.T, reg *plugin.Registry, aliases pluginAliases, cands ...route.Candidate) *pluginHarness {
	t.Helper()

	if len(cands) == 0 {
		cands = []route.Candidate{pluginCandidate(1, "m-1")}
	}

	exec := &pluginExecutor{resp: &channel.Response{
		UpstreamModel: "m-1-20250101",
		Message: channel.Message{
			Role:    "assistant",
			Content: []channel.ContentBlock{{Type: "text", Text: "原始回答"}},
		},
		FinishReason: "stop",
		Usage:        channel.Usage{PromptTokens: 10, CompletionTokens: 5},
	}}
	reservations := &pluginReservations{}
	settlements := &pluginSettlements{}

	now := func() time.Time { return pluginNow }
	gw := &gateway.Gateway{
		Pipeline: &pipeline.Pipeline{
			Rules:    pluginRules{src: pluginRule},
			Programs: expr.NewCache(8),
			Now:      now,
		},
		Router: &route.Router{
			Rand: rand.New(rand.NewSource(1)), Now: now,
			Aliases: aliases,
		},
		Candidates:     &pluginCandidates{cands: cands},
		AliasOverrides: aliases,
		// 单次输出上限的全局默认值（见 gateway.Gateway.MaxOutputTokens）。
		// 这个文件测的是插件钩子，取值给一个**远高于**夹具里 max_tokens 的数：
		// 那条判据不该在这些用例上绊住任何东西。
		MaxOutputTokens: 65536,
		// 流式递进冻结的阈值段（见 gateway.Gateway.StreamFreezeStep）。
		// 同一个理由取一个**远高于**夹具里 max_tokens 的数：那个值必须为正，
		// 不给的话每一个流式请求都会在开始之前失败——而这里测的是插件钩子。
		StreamFreezeStep: 65536,
		Reservations:     reservations,
		Settlements:      settlements,
		Executor:         exec,
		Now:              now,
	}

	h := New(&fakeAuth{user: User{ID: 7}}, gw)
	h.Version = "v0.1.0-test"
	h.DefaultAsset = "CNY"
	h.Plugins = reg
	h.Now = now

	return &pluginHarness{
		handler:      h,
		reservations: reservations,
		settlements:  settlements,
		executor:     exec,
	}
}

// identityAliases 是「规范名就是上游名」的别名来源，代表一条别名都没配的
// 部署。选路要求必须接一个来源（nil 时请求直接失败），所以每个组装 Router
// 的测试夹具都要给一个；这个文件测的是插件钩子，不关心映射怎么算。
//
// 它同时当预留上界那一路的来源用（见 Gateway.AliasOverrides），方法是
// 「一个渠道都没有覆盖」。
type identityAliases struct{}

func (identityAliases) Alias(_ context.Context, _ int64, canonical string) (string, map[string]any, error) {
	return canonical, nil, nil
}

func (identityAliases) OverridesFor(_ context.Context, _ string, _ []int64) (map[int64]map[string]any, error) {
	// 空 map 而不是 nil：两个都表示「没有渠道配了覆盖」，而 nil 会撞上
	// nilnil 那条 linter（同 internal/gateway 的夹具）。
	return map[int64]map[string]any{}, nil
}

func pluginCandidate(id int64, model string) route.Candidate {
	return route.Candidate{
		Channel: channel.Channel{
			ID: id, Name: "ch", BaseURL: "https://api.example.com",
			Credential: "placeholder",
			Models:     []string{model},
		},
		OwnerUserID: 7,
		Health:      route.Health{SuccessRate: 1.0},
	}
}

// pluginRegistry 注册一个插件。hooks 是原样插进 definePlugin 的 JS 片段，
// meta 是原样插进 meta 字面量的片段（要 `blocking: true` 之类的开关时用它，
// 它自带开头的逗号）。
//
// prelude 原样插在模块顶层（import 之后、meta 之前），用于声明跨 hook 共享
// 的模块级状态——会话与运行时一对一，所以那份状态是「同一个会话」的探针。
func pluginRegistry(t *testing.T, meta, hooks string, prelude ...string) *plugin.Registry {
	t.Helper()
	reg, err := plugin.Publish([]plugin.Registration{{
		Key: "p1",
		Source: "import { definePlugin } from \"larkax\";\n" +
			strings.Join(prelude, "") +
			"export const meta = { key: \"p1\", apiVersion: 1" + meta + " };\n" +
			"export default definePlugin({" + hooks + "});\n",
	}})
	if err != nil {
		t.Fatalf("注册插件失败: %v", err)
	}
	return reg
}

// ── onRequest ─────────────────────────────────────────────────────────

// 插件调小 max_tokens 之后，**预留上界**要跟着变小。
//
// 这是这一条接线上最容易骗过自己的一处：补丁应用在预留之后时，请求照样
// 成功、响应照样正确、日志里插件照样跑了，只有冻住的那笔钱是错的。
// 所以断言必须落在金额上，而且**不能是相对关系**——「有插件时比没插件时小」
// 在「补丁根本没进预留」时也可能成立（例如补丁改了别的东西）。
func TestOnRequestShrinksReservation(t *testing.T) {
	const body = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`

	// 预留金额按「最大可能用量」算，所以这两个数只差在 max_tokens 上：
	// 1000 × 0.000002 = 0.002，即 200000 个最小单位；
	// 10 × 0.000002 = 0.00002，即 2000 个。
	t.Run("插件把 max_tokens 调小", func(t *testing.T) {
		h := newPluginHarness(t, pluginRegistry(t, "",
			`onRequest(){ return { body: { max_tokens: 10 } }; }`))

		rec := post(h.handler, PathChatCompletions, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		if len(h.reservations.saved) != 1 {
			t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
		}
		want := money.FromUnits(2000)
		if got := h.reservations.saved[0].Amount; got != want {
			t.Errorf("预留金额 = %s，期望 %s（插件把上界从 1000 调到 10 之后）", got, want)
		}
		// 补丁也要真的发到上游：预留按 10 算而上游仍按 1000 生成的话，
		// 「预留是上界」这条不变量当场就破了。
		if len(h.executor.reqs) != 1 {
			t.Fatalf("上游被调用 %d 次，期望 1 次", len(h.executor.reqs))
		}
		if got := h.executor.reqs[0].MaxTokens; got != 10 {
			t.Errorf("上游收到的 max_tokens = %d，期望 10", got)
		}
	})

	// 对照组：插件没给补丁时上界是客户端给的那个数。没有这一组，上面那条
	// 断言在「预留恒等于 2000」这种错法下也是绿的。
	t.Run("插件没给补丁", func(t *testing.T) {
		h := newPluginHarness(t, pluginRegistry(t, "", `onRequest(){ return; }`))

		rec := post(h.handler, PathChatCompletions, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		if len(h.reservations.saved) != 1 {
			t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
		}
		want := money.FromUnits(200000)
		if got := h.reservations.saved[0].Amount; got != want {
			t.Errorf("预留金额 = %s，期望 %s（补丁没生效，上界就是客户端的 1000）", got, want)
		}
	})
}

// 撞键重试时，插件补丁**不许叠加**。
//
// # 这条防的是什么
//
// serveIdempotent 的循环在键被并发请求抢走时会重进一次 serveOnce，而
// serveOnce 会把交进来的那份请求**就地**交给插件改（补丁直接写在
// channel.Request 上）。拿同一个对象重进一遍，变换式的补丁就叠加——
// 「把 max_tokens 减 1」跑两轮变成减 2。它不报错：请求成功、响应正确、
// 日志里插件也跑了，只有发到上游的那个值悄悄少了一截。
//
// # 为什么断言的是「原值 - 1」而不是「插件被调过」
//
// 「插件跑过」在叠加了与没叠加时都成立。真正要钉住的是上游收到多少，
// 所以断言落在 executor 实际收到的 max_tokens 上，并连带断言那次预留的
// 金额（上界跟着这个值走）。
//
// # 两个让这条用例不空转的守卫
//
// 一是 h.reservations.calls == 2：重试必须真的发生过。少了它，夹具哪天
// 不再回 ErrClaimed，用例会退化成「跑了一遍、值也对」而照样绿。
// 二是对照组里的 1000：不减去 1 时上游收到的就是它（见 TestOnRequestShrinksReservation
// 的另一组子用例），所以这里的 999 不是「随便哪个数都对」。
func TestOnRequestPatchDoesNotStackAcrossRetry(t *testing.T) {
	const body = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`

	// 变换式补丁：按**当时读到的**值减一。读到的值每轮都对时它给出 999，
	// 而叠加时第二轮的输入已经是 999、结果变成 998。
	h := newPluginHarness(t, pluginRegistry(t, "",
		`onRequest(ctx){ return { body: { max_tokens: ctx.body.max_tokens - 1 } }; }`))

	// 第一次预留落库时键已被并发请求占住（落库层回 ErrClaimed），那一次
	// 预留整个回滚；第二次查仍然没有可重放的记录，于是原路径再走一遍。
	// 这中间那个窗口是真实存在的：赢家的记录还没提交时，按 READ COMMITTED
	// 查不到它，而插入已经会撞上唯一索引。
	h.reservations.claimErr = idempotency.ErrClaimed
	h.reservations.claimTimes = 1
	h.handler.Idempotency = &fakeIdem{script: []idemReply{
		{err: idempotency.ErrNotFound},
		{err: idempotency.ErrNotFound},
	}}

	rec := postIdem(t, h.handler, PathChatCompletions, body, "key-patch-retry")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（撞键不是失败）。响应体 %s", rec.Code, rec.Body)
	}

	if got := h.reservations.calls; got != 2 {
		t.Fatalf("预留落库被调用 %d 次，期望 2 次——这条用例的全部内容就是那第二次。"+
			"只调用了一次说明重试没发生，下面的断言也就什么都没钉住", got)
	}
	if len(h.executor.reqs) != 1 {
		t.Fatalf("上游被调用 %d 次，期望 1 次（第一次撞键时预留整个回滚，上游一次都没调）",
			len(h.executor.reqs))
	}
	if got := h.executor.reqs[0].MaxTokens; got != 999 {
		t.Errorf("上游收到的 max_tokens = %d，期望 999（客户端给的 1000 减一）。"+
			"998 说明第二轮读到的已经是第一轮改过的请求——补丁叠加了", got)
	}
	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留落库 %d 次，期望 1 次（被撞键挡下的那次整个回滚了）", len(h.reservations.saved))
	}
	// 预留上界跟着那个值走：999 × 0.000002 = 0.001998，即 199800 个最小单位。
	// 叠加时这里是 199600，而响应上看不出任何差别。
	want := money.FromUnits(199800)
	if got := h.reservations.saved[0].Amount; got != want {
		t.Errorf("预留金额 = %s，期望 %s（上界按 999 算）", got, want)
	}
}

// 插件系统没接上时，行为与它出现之前完全一样。
func TestPluginRegistryAbsent(t *testing.T) {
	h := newPluginHarness(t, nil)

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
	if len(h.settlements.saved) != 1 {
		t.Fatalf("结算落库 %d 次，期望 1 次", len(h.settlements.saved))
	}
}

// 没有匹配的插件时请求照常走，预留按客户端给的数。
func TestNoMatchingPlugin(t *testing.T) {
	reg, err := plugin.Publish([]plugin.Registration{{
		Key: "p1",
		Source: "import { definePlugin } from \"larkax\";\n" +
			"export const meta = { key: \"p1\", apiVersion: 1, models: [\"别的模型\"] };\n" +
			"export default definePlugin({ onRequest(){ return { body: { max_tokens: 10 } }; } });\n",
	}})
	if err != nil {
		t.Fatalf("注册插件失败: %v", err)
	}
	h := newPluginHarness(t, reg)

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if got := h.executor.reqs[0].MaxTokens; got != 16 {
		t.Errorf("上游收到的 max_tokens = %d，期望 16（没有匹配的插件，补丁不该生效）", got)
	}
}

// ── onResponse ────────────────────────────────────────────────────────

// 插件改的正文要落到**发给调用方的那份响应**上。
func TestOnResponsePatchReachesCaller(t *testing.T) {
	h := newPluginHarness(t, pluginRegistry(t, "",
		`onResponse(){ return { text: "改写后的回答" }; }`))

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "改写后的回答") {
		t.Errorf("响应体里该有插件改过的正文: %s", rec.Body)
	}
	// 插件给的是一整段文本，不是一段增量：原始正文该被换掉而不是接在后面。
	if strings.Contains(rec.Body.String(), "原始回答") {
		t.Errorf("插件给的是整段替换，原始正文不该还在: %s", rec.Body)
	}
}

// 阻断型插件的 onResponse 失败时这次请求失败，且**不计费**。
//
// 上游其实成功了、钱也花掉了，但调用方拿到的是错误而不是那份回答——
// 把一次没送出去的回答记成消费，在对账上是一笔解释不通的扣费。
func TestBlockingOnResponseFailsRequestWithoutCharge(t *testing.T) {
	reg, err := plugin.Publish([]plugin.Registration{{
		Key: "p1",
		Source: "import { definePlugin } from \"larkax\";\n" +
			"export const meta = { key: \"p1\", apiVersion: 1, blocking: true };\n" +
			"export default definePlugin({ onResponse(){ throw new Error(\"检查没过\"); } });\n",
	}})
	if err != nil {
		t.Fatalf("注册插件失败: %v", err)
	}
	h := newPluginHarness(t, reg)

	rec := post(h.handler, PathChatCompletions, validBody)
	// 恰好 500，而不是「不是 200」。
	//
	// 判据是「谁的错回谁的错」（见 httpStatusOfUpstream）：插件是我们这一侧
	// 装的，它把一份已经拿到的回答拦下来不是调用方的请求有问题，所以回 5xx。
	// 5xx 里具体是 500 而不是 503：503 在本入口已经被「没有可用上游」占用
	// （route.ErrNoCandidates），而这次候选与渠道都好着，把插件失败说成
	// 「没有可用上游」会让运维去查渠道。
	//
	// 只断言「不是 200」的话，把这条分支改成回 503、回 502 甚至回 400
	// 都照样绿——而那三种错法各自把排查引向不同的方向。
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（响应体 %s）", rec.Code, rec.Body)
	}
	if len(h.settlements.saved) != 0 {
		t.Errorf("结算落库 %d 次，期望 0 次（没送出去的回答不计费）", len(h.settlements.saved))
	}
	if len(h.reservations.released) != 1 {
		t.Errorf("预留释放 %d 次，期望 1 次（冻住的钱要放开）", len(h.reservations.released))
	}
}

// 非阻断型插件的 onResponse 失败时请求照常成功：这是契约里
// 「插件出错不能让请求失败，除非显式声明为阻断型」的落点。
func TestNonBlockingOnResponseFailureKeepsRequest(t *testing.T) {
	h := newPluginHarness(t, pluginRegistry(t, "",
		`onResponse(){ throw new Error("我自己挂了"); }`))

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("非阻断型插件失败不该让请求失败，状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "原始回答") {
		t.Errorf("补丁没生效时响应该是上游原样: %s", rec.Body)
	}
	if len(h.settlements.saved) != 1 {
		t.Errorf("结算落库 %d 次，期望 1 次", len(h.settlements.saved))
	}
}

// 阻断型插件的 onRequest 失败时请求失败，而且**上游一次都没被调过**：
// 那时还没有任何副作用，调用方重试是安全的。
func TestBlockingOnRequestFailsBeforeUpstream(t *testing.T) {
	reg, err := plugin.Publish([]plugin.Registration{{
		Key: "p1",
		Source: "import { definePlugin } from \"larkax\";\n" +
			"export const meta = { key: \"p1\", apiVersion: 1, blocking: true };\n" +
			"export default definePlugin({ onRequest(){ throw new Error(\"不接受这次请求\"); } });\n",
	}})
	if err != nil {
		t.Fatalf("注册插件失败: %v", err)
	}
	h := newPluginHarness(t, reg)

	rec := post(h.handler, PathChatCompletions, validBody)
	// 与 onResponse 那条同一个判据：插件在我们这一侧，它拒了这次请求不是
	// 调用方的请求有问题，所以是 500 而不是 4xx；也不是 503——503 在本入口
	// 是「没有可用上游」，而这次连预留都还没发生。
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（响应体 %s）", rec.Code, rec.Body)
	}
	if len(h.executor.reqs) != 0 {
		t.Errorf("上游被调用 %d 次，期望 0 次", len(h.executor.reqs))
	}
	// 预留发生在 gateway.begin 里，而钩子排在它**之前**，所以这里连一次
	// 预留都不该有——钱一分没冻，也就没有要释放的东西。
	if len(h.reservations.saved) != 0 {
		t.Errorf("预留落库 %d 次，期望 0 次（钩子在预留之前）", len(h.reservations.saved))
	}
}

// ── 流式 ──────────────────────────────────────────────────────────────

// 流式请求跑得到 onRequest，**跑不到 onResponse**。
//
// # 两半各由什么钉住
//
// onRequest 生效：断言 executor 实际收到的 max_tokens。这一条不是废话——
// 它排在流式与非流式的分岔**之前**，接错位置（挪进非流式那条分支里）
// 就是流式请求完全绕过插件，而那在响应上看不出来。
//
// onResponse 没被调用：靠日志里那条元数据。插件在 onResponse 里回一段
// metadata，入口把它记成一条带 plugin / request_id 的日志（见 responseHooks）
// ——元数据在协议层没有对应字段，那条日志是它唯一的出口，所以「日志里没有」
// 就是「onResponse 没跑」。
//
// # 为什么对照组是必须的
//
// 「日志里没有那句话」在「onResponse 确实没跑」与「日志根本没被接出来」
// 之间分不出来（LESSONS.md 第 16 条）。所以同一个插件要先在**非流式**那条
// 路上跑一遍，把那条日志逼出来——它同时证明这个捕获器在工作、这个插件真的
// 匹配上了。少了这一半，流式那半即使把断言删掉也没人会发现。
func TestStreamingRequestRunsOnRequestButNotOnResponse(t *testing.T) {
	const streamBody = `{"model":"m-1","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":1000}`

	// 两条 hook 写在同一个插件里：对照组与实验组跑的是**同一个**插件，
	// 差别只在请求是不是流式。
	const hooks = `onRequest(ctx){ return { body: { max_tokens: 10 } }; },
		onResponse(){ return { metadata: { seen: 1 } }; }`

	// metadataMarker 是那条日志里的固定措辞，也是这里数调用次数的那根探针。
	const metadataMarker = "插件给了响应元数据"

	t.Run("非流式：两个 hook 都跑", func(t *testing.T) {
		cap := &logCapture{}
		adoptLogCapture(t, cap)

		h := newPluginHarness(t, pluginRegistry(t, "", hooks))
		rec := post(h.handler, PathChatCompletions,
			`{"model":"m-1","messages":[{"role":"user","content":"hi"}],"max_tokens":1000}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}
		if !strings.Contains(cap.text(), metadataMarker) {
			t.Fatalf("非流式这条路上 onResponse 该跑过，日志里却没有 %q——"+
				"捕获器没接出来时，流式那条断言什么都没证明。日志：%s",
				metadataMarker, cap.text())
		}
	})

	t.Run("流式：只跑 onRequest", func(t *testing.T) {
		cap := &logCapture{}
		adoptLogCapture(t, cap)

		h := newPluginHarness(t, pluginRegistry(t, "", hooks))
		rec := post(h.handler, PathChatCompletions, streamBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
		}

		if len(h.executor.reqs) != 1 {
			t.Fatalf("上游被调用 %d 次，期望 1 次", len(h.executor.reqs))
		}
		if got := h.executor.reqs[0].MaxTokens; got != 10 {
			t.Errorf("上游收到的 max_tokens = %d，期望 10（onRequest 在流式那条路上也该生效）", got)
		}

		if strings.Contains(cap.text(), metadataMarker) {
			t.Errorf("流式请求不该跑 onResponse：响应是边生成边发的，没有「整份响应」"+
				"这个时刻，钩子拿不到可交付的对象（见 docs/plugin-contract.md）。日志：%s", cap.text())
		}
	})
}

// 插件加的请求头要真的进到上游请求里。
//
// 与前面几条不同，这一条只到 channel.Request 为止——请求头怎么变成上游的
// HTTP 头是适配器的事，不在这条链路上。
func TestOnRequestHeadersReachUpstreamRequest(t *testing.T) {
	h := newPluginHarness(t, pluginRegistry(t, "",
		`onRequest(){ return { headers: { "x-larkax-tenant": "t7" } }; }`))

	rec := post(h.handler, PathChatCompletions, validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应体 %s", rec.Code, rec.Body)
	}
	if got := h.executor.reqs[0].Headers["x-larkax-tenant"]; got != "t7" {
		t.Errorf("上游请求头 x-larkax-tenant = %q，期望 t7（整份头：%v）",
			got, h.executor.reqs[0].Headers)
	}
}
