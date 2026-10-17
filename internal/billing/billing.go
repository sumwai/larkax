// Package billing 把一次请求的用量算成金额。
//
// 它做两件事，且只做这两件：
//
//  1. **算钱**：按用量与计价规则求值，得到一个金额。
//  2. **定金额的归属**：这笔钱从哪个账户出、进哪个账户，即账本分录。
//
// 不做的事：不查库、不发 HTTP、不改余额。求值需要的规则与用量由调用方传入，
// 算出的金额也交回调用方。这样「算得对不对」可以脱离数据库与网络单独测试，
// 而它恰是这套系统里最贵的一类 bug——**算错了不会报错，只会让账目悄悄错掉**。
//
// # 两阶段
//
// 预留：请求发出前按**最坏情况**算出上界并冻结。
// 结算：响应回来后按实际用量算出真实金额，与预留的差额退回或补扣。
//
// 预留按最坏情况而不是典型值，是为了让结算**永远不可能超过预留**，
// 从而「余额不为负」这个不变量不需要例外。代价是大请求冻住的余额多，
// 但预留只在秒级内存在，而例外要长期维护。
package billing

import (
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
)

// Usage 是参与计费的用量。
//
// 与 channel.Usage 分开：那个是协议层的归一化结果（含 Raw 等排查字段），
// 这个是**计费口径**——只保留会进公式的量，且每一项的含义按计价需要定义。
// 混用会让「加了一个排查字段」意外影响计费。
type Usage struct {
	// PromptTokens 是**非缓存**的输入 token。
	// 缓存命中的部分单独记，因为它们的单价通常低得多。
	PromptTokens int64
	// CompletionTokens 是输出 token，**不含**推理 token：两者互斥，
	// 各自乘各自的单价，不会对同一批 token 收两次费。
	// 协议层报的未必是这个口径（OpenAI 的 completion_tokens 含推理），
	// 那层关系由 channel.Usage.CompletionIncludesReasoning 声明，
	// 由 FromChannelUsage 折算过来。
	CompletionTokens int64
	// CacheReadTokens 是命中缓存的输入 token。
	CacheReadTokens int64
	// CacheWriteTokens 是写入缓存的 token。
	CacheWriteTokens int64
	// ReasoningTokens 是推理模型的思考 token。与 CompletionTokens 互斥，
	// 见上一条。
	ReasoningTokens int64

	// UnitCounts 是非 token 计量：搜索次数、图片张数、视频秒数。
	UnitCounts map[string]int64
}

// TotalTokens 返回参与计费的 token 总数，用于上限校验与展示。
func (u Usage) TotalTokens() int64 {
	return u.PromptTokens + u.CompletionTokens + u.CacheReadTokens +
		u.CacheWriteTokens + u.ReasoningTokens
}

// IsEmpty 判断是否完全没有用量。
//
// 空用量是有意义的信号：它通常意味着上游返回了错误，或者解析没读到 usage。
// 计费层据此区分「这次真的没用 token」与「我们没读到用量」。
//
// **目前没有生产调用方**：上面那句话描述的区分还没有一层做出来，而它与 #267
// 是同一处缺口的两面——非流式的 billing_path 今天分不开「上游没报 usage」与
// 「上游报了 0」。那一处修的时候应当把本方法一起用上，而不是另造一个判据。
func (u Usage) IsEmpty() bool { return u.TotalTokens() == 0 && len(u.UnitCounts) == 0 }

// Prices 是各项单价。
//
// 全部是「每个 token 多少元」。按千 token 标价的供应商在录入时换算，
// 不在这里做单位换算——那会让公式里多一个「除以 1000」，
// 而它一旦漏掉就是一千倍的错账。
type Prices struct {
	Prompt     money.Amount
	Completion money.Amount
	CacheRead  money.Amount
	CacheWrite money.Amount
	Reasoning  money.Amount
	// Units 是非 token 计量的单价，键与 Usage.UnitCounts 对应。
	Units map[string]money.Amount
	// Declared 记录规则**显式写过**哪几个 token 单价，键是 prompt / completion /
	// cache_read / cache_write / reasoning。
	//
	// 为什么单独记这个：单价的零值与「规则里没写」在 money.Amount 上分不出来，
	// 而这两件事要区别对待——写了 0 是「这一项免费」，是正当配置；没写是
	// 「规则漏了」，那样这类用量会永远按 0 元计，且不报错，只在对账时表现为
	// 「用量对不上金额」。Validate 据此把后者拒掉。
	//
	// 由 internal/billing/expr 求值规则时填——只有它知道规则里到底写了哪些键。
	Declared map[string]bool
}

// Breakdown 是一次计费的明细。
//
// 保留逐项的金额而不只保留总额：对账时要能回答「这一分钱是哪个环节产生的」。
// 只留总额的话，一次争议无从解释。
type Breakdown struct {
	Prompt     money.Amount
	Completion money.Amount
	CacheRead  money.Amount
	CacheWrite money.Amount
	Reasoning  money.Amount
	// Units 是非 token 计量的逐项金额。
	Units map[string]money.Amount

	// Fixed 是每请求的固定费用。它是一行独立的项，不是被藏进某项里：
	// 对账时要能看出「这一部分是固定费」。
	Fixed money.Amount
	// MinimumAdjustment 是保底补差。实际按量金额低于最低消费时，
	// 这里记下差额，使 Total 达到最低消费。
	//
	// 记成**显式的调整项**而不是直接抬高 Total：那样 Total 仍然等于
	// 所有行项之和，Verify() 不必为例外开口子，对账时也能一眼看出
	// 「这一部分是保底补的」。
	MinimumAdjustment money.Amount

	// Total 是所有行项之和，等于按量部分 + Fixed + MinimumAdjustment。
	// 有测试钉「Total = 逐项之和」——一旦不等，对账时就会出现
	// 一笔永远找不平的差额。
	Total money.Amount
}

// ComputeWithCharges 按规则与用量算出金额明细，算上固定费用与最低消费。
//
// 计算顺序无关紧要（加法交换律），但**逐项保留**很重要：
// 对账与争议处理都需要知道钱花在哪一项上。
//
// **没有「不带 Charges」的版本。** 一次性费用是规则的一部分，而预留与结算
// 必须用**同一组** Charges：若一侧算了保底而另一侧没算，一笔用量很小的请求
// 会被预留 1 元、结算 0.002 元，差额全额退回——保底永远收不到，而且不会报错，
// 只是长期少收钱。省掉那个参数看起来只是少写一个 `Charges{}`，代价是
// 把这条静默少收的路径留在手边。
//
// 两阶段（预留与结算）必须用**同一个**函数算金额，否则预留的上界
// 与结算的结果会因为「有没有算保底」而差开，表现为每次都撞 Capped。
func ComputeWithCharges(usage Usage, prices Prices, charges Charges) Breakdown {
	var b Breakdown

	b.Prompt = prices.Prompt.MulInt(usage.PromptTokens)
	b.Completion = prices.Completion.MulInt(usage.CompletionTokens)
	b.CacheRead = prices.CacheRead.MulInt(usage.CacheReadTokens)
	b.CacheWrite = prices.CacheWrite.MulInt(usage.CacheWriteTokens)
	b.Reasoning = prices.Reasoning.MulInt(usage.ReasoningTokens)

	if len(usage.UnitCounts) > 0 {
		b.Units = make(map[string]money.Amount, len(usage.UnitCounts))
		for name, count := range usage.UnitCounts {
			price, ok := prices.Units[name]
			if !ok {
				// 用量里有这个计量项，规则却没给单价。
				// 不能当成免费——那会让这类用量永远不产生费用，
				// 而它也不会报错，只会在对账时表现为「用量对不上金额」。
				// 这里跳过这一项（不放进明细），由 Validate 报出来。
				continue
			}
			b.Units[name] = price.MulInt(count)
		}
	}

	b.applyFixed(charges.Fixed)
	b.applyMinimum(charges.Minimum)
	b.Total = b.sum()
	return b
}

// Subtotal 是按用量算出的部分，不含固定费用与保底补差。
//
// 最低消费要拿它来比较——拿 Total 比较的话，固定费用会把补差抵消掉，
// 「固定费 0.5 元、最低消费 1 元」的规则就永远收不到补差。
func (b Breakdown) Subtotal() money.Amount {
	total := money.Sum(b.Prompt, b.Completion, b.CacheRead, b.CacheWrite, b.Reasoning)
	for _, v := range b.Units {
		total = total.Add(v)
	}
	return total
}

func (b Breakdown) sum() money.Amount {
	return b.Subtotal().Add(b.Fixed).Add(b.MinimumAdjustment)
}

// Verify 校验 Total 等于逐项之和。
//
// 单独一个方法而不是在 Compute 里断言：外部构造的 Breakdown（例如从数据库
// 读回的旧记录）也要能校验，而那正是最需要校验的地方。
func (b Breakdown) Verify() error {
	expected := b.sum()
	if b.Total.Cmp(expected) != 0 {
		return fmt.Errorf("明细总额 %s 与逐项之和 %s 不符", b.Total, expected)
	}
	return nil
}

// ErrMissingPrice 表示用量里有计量项，规则却没给它单价。
var ErrMissingPrice = errors.New("用量里有规则未定价的计量项")

// ReasoningExceedsCompletion 判断协议层用量是否自相矛盾：声明说推理已经算在
// 输出里，而报出来的推理量却大于输出总量。这两件事不可能同时成立。
//
// 它**不是**一条校验规则：这里返回 true 不代表这次用量算错了，只代表上游报的
// 两个数对不上。所以它不该让结算失败——那会把一条渠道的所有请求都变成失败，
// 而这是一次运行时来自上游的数据状况，不是几分钟能修好的配置错误
// （与 ErrMissingPrice 那类正好相反）。调用方拿它去**告警**。
//
// 为什么要单独一个函数：拆分（FromChannelUsage）会把矛盾的输出钳到 0，
// 钳过之后 (输出 50、推理 80) 这种矛盾输入与 (输出 100、推理 100) 这种
// 「整段输出都是推理」的合法输入得到同一个计费口径用量，**判断只能在
// 协议层的用量上做**。
func ReasoningExceedsCompletion(u channel.Usage) bool {
	return u.CompletionIncludesReasoning && u.ReasoningTokens > u.CompletionTokens
}

// TokenSlot 是一个按 token 计费的槽位。
//
// 一份槽位清单要回答三个问题：它在规则里叫什么、从 Usage 的哪个字段读数量、
// 写进 Prices 的哪个字段。三者放在同一行，是因为它们只对得上一半时不会
// 编译失败——例如把 cache_write 的读取函数写成 CacheReadTokens，
// 每个槽位单独看都「有」，只有账单上的数字会悄悄错掉。
type TokenSlot struct {
	// Name 是槽位在规则与 Prices.Declared 里的键名。
	Name string
	// Count 读出一次用量里这一项的 token 数。
	Count func(Usage) int64
	// SetCount 写回一次用量里这一项的 token 数。与 Count 成对，供测试
	// 逐槽造用量——加一个槽位时，「清单里有、算术里没有」不会编译失败。
	SetCount func(*Usage, int64)
	// SetPrice 把这一项的单价写进 Prices。
	SetPrice func(*Prices, money.Amount)
	// Price 读回 Prices 里这一项的单价。与 SetPrice 成对，供测试核对
	// 「写进去的确实是这一项」——只写一个函数的话，指错字段的写法
	// 在行为上仍然自洽（写和读都错到同一处），用例分辨不出来。
	Price func(Prices) money.Amount
}

// TokenSlots 是按 token 计费的全部槽位，按用量字段的定义顺序排列。
//
// **这是「有哪些 token 槽位、它们在规则里叫什么」的唯一一份清单。**
// Validate 的逐项判负与漏写单价、internal/billing/expr 的结果键白名单与
// Prices.Declared 都从它派生。那些地方各抄一份的话，加一个槽位时漏改一处
// 不会编译失败：漏在键名白名单上，规则写这个键会被当成未知键；漏在
// Declared 上，这一项永远被当成「规则没写」而拒绝；漏在 Validate 上，
// 负用量与漏写的单价会静默放过。三种都不是崩溃，只是账慢慢错掉。
//
// ComputeWithCharges 是例外：金额要落到 Breakdown 的具名字段上，没法只靠
// 这张表派生。所以另有一条用例按表逐槽造用量、核对金额——「清单加了、
// 乘法没加」会在那里挂。
//
// 调用方不得修改这个切片。
var TokenSlots = []TokenSlot{
	{
		Name:     "prompt",
		Count:    func(u Usage) int64 { return u.PromptTokens },
		SetCount: func(u *Usage, n int64) { u.PromptTokens = n },
		SetPrice: func(p *Prices, v money.Amount) { p.Prompt = v },
		Price:    func(p Prices) money.Amount { return p.Prompt },
	},
	{
		Name:     "completion",
		Count:    func(u Usage) int64 { return u.CompletionTokens },
		SetCount: func(u *Usage, n int64) { u.CompletionTokens = n },
		SetPrice: func(p *Prices, v money.Amount) { p.Completion = v },
		Price:    func(p Prices) money.Amount { return p.Completion },
	},
	{
		Name:     "cache_read",
		Count:    func(u Usage) int64 { return u.CacheReadTokens },
		SetCount: func(u *Usage, n int64) { u.CacheReadTokens = n },
		SetPrice: func(p *Prices, v money.Amount) { p.CacheRead = v },
		Price:    func(p Prices) money.Amount { return p.CacheRead },
	},
	{
		Name:     "cache_write",
		Count:    func(u Usage) int64 { return u.CacheWriteTokens },
		SetCount: func(u *Usage, n int64) { u.CacheWriteTokens = n },
		SetPrice: func(p *Prices, v money.Amount) { p.CacheWrite = v },
		Price:    func(p Prices) money.Amount { return p.CacheWrite },
	},
	{
		Name:     "reasoning",
		Count:    func(u Usage) int64 { return u.ReasoningTokens },
		SetCount: func(u *Usage, n int64) { u.ReasoningTokens = n },
		SetPrice: func(p *Prices, v money.Amount) { p.Reasoning = v },
		Price:    func(p Prices) money.Amount { return p.Reasoning },
	},
}

// Validate 检查规则与用量是否自洽。
//
// 拦的是「算了但算得不对」的几类情形。它们都不会让程序崩，
// 只会让账目慢慢偏掉——而偏掉之后要很久才看得出来。
//
// 这里的每一条都是**配置与用量对不上**（规则缺单价、用量为负），
// 运维改一条配置就能修好，所以让结算失败是合适的处置。上游报的数自相矛盾
// 属于另一类，见 ReasoningExceedsCompletion。
func Validate(usage Usage, prices Prices) error {
	// 负用量**逐项**判，而不是判五项之和。
	//
	// 负用量只可能来自解析错误（例如把带符号整数读反）。放过去会算出一笔
	// 负数消费，即「上游付钱给用户」。判和看不出来：单项为负、被另一项的正值
	// 抵消时和为 0 或为正，与「完全没用 token」分不出来。
	// 口径与 internal/billing/expr 的 validateUsage 一致——那里一直是逐项的。
	for _, slot := range TokenSlots {
		if count := slot.Count(usage); count < 0 {
			return fmt.Errorf("用量为负: %s = %d", slot.Name, count)
		}
	}

	for name, count := range usage.UnitCounts {
		if count < 0 {
			return fmt.Errorf("计量项 %q 的数量为负: %d", name, count)
		}
		if _, ok := prices.Units[name]; !ok {
			return fmt.Errorf("%w: %q", ErrMissingPrice, name)
		}
	}
	// token 单价同样不能漏。判据是「规则有没有写过这个键」，而不是「单价是不是
	// 0」——显式写 0 是「这一项免费」，正当；没写则落到零值，结果一样是 0 元，
	// 两者在 money.Amount 上分不出来，所以靠 Prices.Declared 区分。
	for _, slot := range TokenSlots {
		if slot.Count(usage) > 0 && !prices.Declared[slot.Name] {
			return fmt.Errorf("%w: %q（规则里没写这个单价；要免费就显式写 0）", ErrMissingPrice, slot.Name)
		}
	}
	return nil
}

// FromChannelUsage 把协议层的用量转成计费口径。
//
// 这一步是必须的，因为两者的口径不同：
//
//   - channel.Usage.PromptTokens 含缓存部分（OpenAI 口径），
//     而计价时缓存与非缓存单价不同，必须拆开。
//   - 计费口径下 CompletionTokens **不含**推理（两者互斥，见 Usage 的注释），
//     而协议层是否如此由 channel.Usage.CompletionIncludesReasoning 声明。
//     声明为「已含」时要在这里减掉——不减就是同一批 token 收两次费：
//     completion 与 reasoning 两个变量各自乘一次单价，而它们指的是同一批 token。
func FromChannelUsage(u channel.Usage) Usage {
	out := Usage{
		CompletionTokens: u.CompletionTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		ReasoningTokens:  u.ReasoningTokens,
		UnitCounts:       u.UnitCounts,
	}

	if u.CompletionIncludesReasoning {
		// 推理是输出的一部分，减掉之后两者才互斥。
		// 减出来为负说明上游自相矛盾（推理量大于输出总量），钳到 0——
		// 负数进金额运算会算出一笔「上游付钱给用户」。
		//
		// 钳掉不等于放过：矛盾由调用方按 ReasoningExceedsCompletion 告警。
		// 这里不做那件事，因为本函数只该算数，不该决定怎么上报。
		out.CompletionTokens = max64(u.CompletionTokens-u.ReasoningTokens, 0)
	}

	// PromptTokens 含缓存，减掉才是非缓存输入。
	nonCached := u.PromptTokens - u.CacheReadTokens - u.CacheWriteTokens
	if nonCached < 0 {
		// 缓存量大于输入量：上游口径不一致（有的把缓存算在输入里，
		// 有的不算）。取 0 而不是负数，并把缓存也压到输入以内，
		// 否则会算出一笔负数金额。
		nonCached = 0
	}
	out.PromptTokens = nonCached

	// 缓存读写也不该超过输入总量。
	if total := out.PromptTokens + out.CacheReadTokens + out.CacheWriteTokens; u.PromptTokens > 0 && total > u.PromptTokens {
		over := total - u.PromptTokens
		// 按比例压回去，先压缓存写入（它最容易缺失）。
		cut := min64(over, out.CacheWriteTokens)
		out.CacheWriteTokens -= cut
		over -= cut
		if over > 0 {
			cut = min64(over, out.CacheReadTokens)
			out.CacheReadTokens -= cut
		}
	}

	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
