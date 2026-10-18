package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/route"
)

// 本文件测的是**单次输出上限**：调用方在请求体里声明的 max_tokens 超过它时，
// 请求在**预留之前**就被拒（见 Gateway.checkOutputCap）。
//
// 断言分三组，缺一组这类改动就会留下一个洞：
//
//   - 拒绝本身：错误类型，以及文案里那两个数（声明了多少、允许到多少）——
//     只说「超限」调用方只能猜着改；
//   - 拒绝发生在**冻钱之前**：这是这条判据存在的理由，先冻上再拒等于没做；
//   - 上限之内的请求一个字都不变（预留金额、上游调用都照旧）。
//
// 上限取哪个数是另一条线，单独钉：模型目录登记了用目录那个，没登记用全局
// 默认值；而别名覆盖**不参与**这条判据（判的是调用方声明的那个数）。

// withMaxOutputTokens 把单次上限的全局默认值压下来。
//
// 走选项而不是组装完再改 h.gw 的字段：夹具那边是同一个指针，改了也生效，
// 但读用例的人分不出「这个值参与了组装」与「这个是事后补上去的」——而两者
// 对「上限到底从哪儿来」的含义完全不同（见 newHarness 里那行）。
func withMaxOutputTokens(n int64) func(*harness) {
	return func(h *harness) { h.maxOutputTokens = n }
}

// withDirectoryLimit 给候选挂上模型目录里登记的输出上限。
//
// 目录里那一列是**模型级**的（同一规范名下的候选拿到同一份），所以这里只
// 需要挂一个候选。
func withDirectoryLimit(limit int64) func(*harness) {
	return func(h *harness) {
		c := candidate(1, "gpt-4o")
		c.Profile = &route.ModelProfile{OutputLimit: limit}
		h.candidates = &fakeCandidates{cands: []route.Candidate{c}}
	}
}

// 超限 → 拒，而且拒在**冻钱之前**。
func TestOutputCapRejectsDeclaredOverLimit(t *testing.T) {
	h := newHarness(t, withMaxOutputTokens(1000))

	req := request()
	req.MaxTokens = 1001

	_, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{})

	var over *OutputCapExceeded
	if !errors.As(err, &over) {
		t.Fatalf("错误 = %v，期望 *OutputCapExceeded", err)
	}
	if over.Declared != 1001 || over.Limit != 1000 {
		t.Errorf("错误里带的两个数 = 声明 %d / 上限 %d，期望 1001 / 1000（有限制不许调用方猜）",
			over.Declared, over.Limit)
	}
	if over.Model != "gpt-4o" {
		t.Errorf("错误里的模型 = %q，期望调用方要的那个规范名 gpt-4o", over.Model)
	}
	// 内部错误是中文的，但两个数必须在里面：那是**日志里**查得到这条上限的
	// 唯一地方（给调用方的那句话由 internal/api 拼，见那里对这两个字段的用法）。
	if !strings.Contains(err.Error(), "1001") || !strings.Contains(err.Error(), "1000") {
		t.Errorf("错误文案里两个数都要有，实际 %q", err)
	}

	// 这条判据存在的理由：超限的请求一分钱都不该冻。
	if len(h.reservations.saved) != 0 {
		t.Errorf("超限的请求不该落预留：%+v", h.reservations.saved)
	}
	if len(h.reservations.released) != 0 {
		t.Errorf("没冻过就不该有释放：%+v", h.reservations.released)
	}
	if len(h.executor.calls) != 0 {
		t.Errorf("超限的请求不该调上游：%v", h.executor.calls)
	}
}

// 恰好等于上限不算超：判据是「超过」，不是「达到」。
//
// 与 route 那条目录判据同一个边界（`> p.OutputLimit` 才排除），两处一致才
// 不会出现「选路放行、上限拒掉」这种自相矛盾的拒绝。
func TestOutputCapAtLimitPasses(t *testing.T) {
	h := newHarness(t, withMaxOutputTokens(1000))

	req := request()
	req.MaxTokens = 1000

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("恰好等于上限该放行，实际出错: %v", err)
	}
	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
	if len(h.executor.calls) != 1 {
		t.Errorf("上游被调用 %d 次，期望 1 次", len(h.executor.calls))
	}
}

// 上限之内：预留金额与上游调用都照旧。
//
// 这一条挡的是「顺手把上界也夹到上限」（那是另一条 issue 的事）：夹住的话
// 这里算出来的钱会从 0.002 变成别的数，而请求本身看起来完全正常。
func TestOutputCapLeavesWithinLimitUnchanged(t *testing.T) {
	h := newHarness(t, withMaxOutputTokens(1000))

	req := request()
	req.MaxTokens = 500

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
	// 输入 1000 × 1e-6 + 输出 500 × 2e-6 = 0.002。
	if got := h.reservations.saved[0].Amount.String(); got != "0.00200000" {
		t.Errorf("预留金额 = %s，期望 0.00200000（按调用方声明的 500 算，没被上限改过）", got)
	}
	if len(h.executor.calls) != 1 {
		t.Errorf("上游被调用 %d 次，期望 1 次", len(h.executor.calls))
	}
}

// 目录里登记了 output_limit 时，上限取**目录那个数**——全局默认值更大也不顶用。
func TestOutputCapUsesDirectoryLimit(t *testing.T) {
	cases := []struct {
		name     string
		declared int64
		rejected bool
	}{
		{"超过目录上限", 101, true},
		{"恰好等于目录上限", 100, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 全局默认值刻意给得大得多：判据用没用目录那个数，只有在这一对
			// 取值下才分辨得出来（都用 100 的话两条路给出同一个答案）。
			h := newHarness(t, withMaxOutputTokens(65536), withDirectoryLimit(100))

			req := request()
			req.MaxTokens = tc.declared

			_, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{})

			var over *OutputCapExceeded
			switch {
			case tc.rejected && !errors.As(err, &over):
				t.Fatalf("错误 = %v，期望 *OutputCapExceeded（目录上限 100）", err)
			case tc.rejected && over.Limit != 100:
				t.Errorf("上限 = %d，期望 100（目录里登记的那个，不是全局默认值 65536）", over.Limit)
			case !tc.rejected && err != nil:
				t.Fatalf("目录上限之内该放行，实际出错: %v", err)
			}
		})
	}
}

// 目录那一列是 0（登记了但未知）或压根没有这个模型时，落到全局默认值。
//
// 两种「未知」都要覆盖：库里是 NULL 与「目录里没有这条记录」在
// route.ModelProfile 那边是同一个处置（0 一律按未知放行），这里跟着它走。
func TestOutputCapFallsBackWhenDirectoryLimitUnknown(t *testing.T) {
	cases := []struct {
		name string
		opts []func(*harness)
	}{
		{"目录里没有这个模型", nil},
		{"目录里登记了但上限未知", []func(*harness){withDirectoryLimit(0)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]func(*harness){withMaxOutputTokens(1000)}, tc.opts...)
			h := newHarness(t, opts...)

			req := request()
			req.MaxTokens = 1001

			_, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{})

			var over *OutputCapExceeded
			if !errors.As(err, &over) {
				t.Fatalf("错误 = %v，期望 *OutputCapExceeded（全局默认值 1000）", err)
			}
			if over.Limit != 1000 {
				t.Errorf("上限 = %d，期望 1000（全局默认值）", over.Limit)
			}
		})
	}
}

// 候选里混进一个「登记了但上限未知」（0）时，取最小要**跳过它**，而不是把它
// 当成一个 0 拉进来。
//
// 目录那一列是模型级的，今天同一份 Profile 发给每一个候选，所以「候选之间取值
// 不同」这件事平时看不出来；而 0 一旦被当成候选值参与比较，limit 会被拉到 0，
// 再过下面那道「0 就落全局默认值」——判据于是从目录里那个 100 悄悄放宽到全局
// 默认的 65536，超限的请求就这么过了，而账上、日志上都看不出来。
//
// 变异记录：`c.Profile.OutputLimit <= 0` 改成 `< 0` 时，正是这一格被放进来
// ——真缺口，这条用例钉的就是它。同一段里那个 `< limit` 的另外两支：
//
//   - 改成 `>=` 也是真缺口：取最小变成取最大，第三个候选（200）会把上限从
//     100 抬到 200；
//   - 改成 `<=` 是**等价变异**：多出来的一格是取值恰好等于 limit，而那一支
//     赋的就是 limit 自己，值不变。
func TestOutputCapMinSkipsUnknownDirectoryLimit(t *testing.T) {
	// 全局默认值刻意给得大：0 漏进来时，下面那道「0 落默认值」会把上限抬到
	// 65536，与目录里那个 100 分得开。
	h := newHarness(t, withMaxOutputTokens(65536), func(h *harness) {
		known := candidate(1, "gpt-4o")
		known.Profile = &route.ModelProfile{OutputLimit: 100}
		unknown := candidate(2, "gpt-4o")
		unknown.Profile = &route.ModelProfile{OutputLimit: 0} // 库里是 NULL
		bigger := candidate(3, "gpt-4o")
		bigger.Profile = &route.ModelProfile{OutputLimit: 200}
		h.candidates = &fakeCandidates{cands: []route.Candidate{known, unknown, bigger}}
	})

	req := request()
	req.MaxTokens = 101

	_, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{})

	var over *OutputCapExceeded
	if !errors.As(err, &over) {
		t.Fatalf("错误 = %v，期望 *OutputCapExceeded（已登记的那些值里最小的是 100）", err)
	}
	if over.Limit != 100 {
		t.Errorf("上限 = %d，期望 100（0 是未知，不参与取最小；取最大会得到 200，"+
			"把 0 当成一个值会落回全局默认的 65536）", over.Limit)
	}

	// 反方向：恰好等于目录那个值要放行。只钉上面那一条的话，一个把上限压到
	// 别的数的实现照样能过。
	req.MaxTokens = 100
	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("100 是已登记的上限里最小的那个，该放行，实际出错: %v", err)
	}
}

// 别名覆盖把上界**抬到上限之上**时，调用方在上限之内的请求照旧放行。
//
// 判的是调用方声明的那个数，覆盖不参与（理由见 checkOutputCap）。代价也在
// 这一条里钉住：预留仍按覆盖之后的 100000 算，所以那笔多冻出来的钱是商家
// 配置的结果——要改的是那条别名，不是把调用方的请求拒掉。
func TestOutputCapIgnoresRaisedOverride(t *testing.T) {
	overrides := &fixedOverrides{byChannel: map[int64]map[string]any{
		1: {"max_tokens": 100000},
	}}
	h := newHarness(t, withMaxOutputTokens(1000), withOverrides(overrides))

	req := request()
	req.InputTokens = 0 // 让上界只剩输出侧那一项，金额好算
	req.MaxTokens = 500

	if _, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{}); err != nil {
		t.Fatalf("调用方声明的 500 在上限之内，不该被拒: %v", err)
	}
	if len(h.reservations.saved) != 1 {
		t.Fatalf("预留落库 %d 次，期望 1 次", len(h.reservations.saved))
	}
	// 100000 × 2e-6 = 0.2。按调用方声明的 500 算会是 0.001——两者差 200 倍。
	if got := h.reservations.saved[0].Amount.String(); got != "0.20000000" {
		t.Errorf("预留金额 = %s，期望 0.20000000（按覆盖之后的 100000 冻，这是这条选法的代价）", got)
	}
}

// 反方向：别名覆盖把上界**压小**，调用方声明的数仍然超限 → 照拒。
//
// 与上一条合起来才说明「判的是声明的那个数」：只看上一条的话，一个按覆盖
// 之后的数判、且只判「是不是比调用方给的大」的实现也能过。
func TestOutputCapRejectsDespiteLoweredOverride(t *testing.T) {
	overrides := &fixedOverrides{byChannel: map[int64]map[string]any{
		1: {"max_tokens": 10},
	}}
	h := newHarness(t, withMaxOutputTokens(1000), withOverrides(overrides))

	req := request()
	req.MaxTokens = 2000

	_, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{})

	var over *OutputCapExceeded
	if !errors.As(err, &over) {
		t.Fatalf("错误 = %v，期望 *OutputCapExceeded（声明 2000 > 上限 1000）", err)
	}
	if over.Declared != 2000 || over.Limit != 1000 {
		t.Errorf("错误里带的两个数 = 声明 %d / 上限 %d，期望 2000 / 1000（报的是调用方写的那个数）",
			over.Declared, over.Limit)
	}
	if len(h.reservations.saved) != 0 {
		t.Errorf("超限的请求不该落预留：%+v", h.reservations.saved)
	}
}

// 流式不在这一条里：它有中途观测点，钱的处置是随流递进冻结，与这条判据
// 不是一回事。所以声明得再大也**不在这条判据上**被拒。
func TestOutputCapSkipsStream(t *testing.T) {
	h := newHarness(t, withMaxOutputTokens(1000))
	useStreamExecutor(h, &scriptedExecutor{script: map[int64][]channel.StreamEvent{
		1: {delta("你好"), {Kind: "done", FinishReason: "stop"}},
	}})

	req := request()
	req.MaxTokens = 100000
	// 生产里这两个位是同一件事：入口按 upstream.Stream 决定走哪条入口，
	// 而 Streamed 也是从它填的（见 internal/api 的 serveOnce 与
	// billingRequest）。用例照着那个形状给。
	req.Streamed = true
	upstream := upstreamRequest()
	upstream.Stream = true

	res, err := h.gw.ChatStream(context.Background(), req, upstream, Hooks{})
	if err != nil {
		t.Fatalf("流式不该被单次上限拒: %v", err)
	}
	collect(t, res)
}

// 决策那条表面也不在这一条里：它的 MaxTokens 是我们按上游预算写的常量
// （见 internal/api 的 decideBillingRequest），调用方从来没有声明过它。
func TestOutputCapSkipsDecision(t *testing.T) {
	d := &fakeDecider{resp: decisionResponse()}
	// 上限压到 1000，而这条请求的 MaxTokens 给到 100000：判据漏进这条路的话
	// 它会当场被拒。
	h := newDecideHarness(t, d, withMaxOutputTokens(1000))

	req := decisionBilling()
	req.MaxTokens = 100000

	if _, err := h.gw.Decide(context.Background(), req, decision()); err != nil {
		t.Fatalf("决策那条表面不该被单次上限拒: %v", err)
	}
	if len(d.channels) != 1 {
		t.Errorf("上游被调用 %d 次，期望 1 次", len(d.channels))
	}
}

// 全局默认值没配（0）时直接失败，而不是静默地不判。
//
// 「判据没接上」与「这次请求没超限」在请求上长得一模一样，所以这一条要钉的
// 是**失败**本身。
func TestOutputCapUnwiredFails(t *testing.T) {
	h := newHarness(t, withMaxOutputTokens(0))

	_, err := h.gw.Chat(context.Background(), request(), upstreamRequest(), Hooks{})
	if !errors.Is(err, ErrNoOutputCap) {
		t.Fatalf("错误 = %v，期望 ErrNoOutputCap", err)
	}
	// 装配错误也不该顺手冻一笔钱：判据排在预留之前。
	if len(h.reservations.saved) != 0 {
		t.Errorf("没配上限时不该落预留：%+v", h.reservations.saved)
	}
}

// 非正数的 max_tokens 仍走原来那条路：措辞与状态码都更准（「必须为正」比
// 「超过上限」说的是同一件事的两个方向）。这条判据不许把它接管过来。
func TestOutputCapLeavesNonPositiveMaxTokensAlone(t *testing.T) {
	h := newHarness(t, withMaxOutputTokens(1000))

	for _, declared := range []int64{0, -1} {
		req := request()
		req.MaxTokens = declared

		_, err := h.gw.Chat(context.Background(), req, upstreamRequest(), Hooks{})

		var over *OutputCapExceeded
		if errors.As(err, &over) {
			t.Errorf("max_tokens = %d 被报成了超限，期望仍报 ErrNoUpperBound", declared)
		}
		if !errors.Is(err, billing.ErrNoUpperBound) {
			t.Errorf("max_tokens = %d 的错误 = %v，期望 ErrNoUpperBound", declared, err)
		}
	}
}
