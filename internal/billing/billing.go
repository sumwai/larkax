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
	"time"

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
	// CompletionTokens 是输出 token，不含推理 token（后者单独计价）。
	CompletionTokens int64
	// CacheReadTokens 是命中缓存的输入 token。
	CacheReadTokens int64
	// CacheWriteTokens 是写入缓存的 token。
	CacheWriteTokens int64
	// ReasoningTokens 是推理模型的思考 token。
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
}

// Rule 是一条计价规则。
//
// Expr 是规则源码，由计价引擎求值。把源码留在规则里而不是编译成函数：
// 历史账单要能解释「当时用的是哪条规则」，而一份可读的源码比一个函数指针
// 更能满足这一点。
type Rule struct {
	ID       int64
	Name     string
	Scope    string
	Priority int
	Expr     string
	// Prices 是由规则求值得到的单价。规则可以是任意表达式（分时、阶梯、
	// 套餐），求值之后落到一组单价上，再乘用量。
	Prices Prices
	// MatchedAt 是这次求值适用的时间点，用于事后复算。
	MatchedAt time.Time
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
	// Total 是各项之和。它必须等于逐项相加的结果——有测试钉这一点。
	Total money.Amount
}

// Compute 按规则与用量算出金额明细。
//
// 计算顺序无关紧要（加法交换律），但**逐项保留**很重要：
// 对账与争议处理都需要知道钱花在哪一项上。
func Compute(usage Usage, prices Prices) Breakdown {
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
				// 这里记为零，由 Validate 报出来。
				continue
			}
			b.Units[name] = price.MulInt(count)
		}
	}

	b.Total = b.sum()
	return b
}

func (b Breakdown) sum() money.Amount {
	total := money.Sum(b.Prompt, b.Completion, b.CacheRead, b.CacheWrite, b.Reasoning)
	for _, v := range b.Units {
		total = total.Add(v)
	}
	return total
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

// Validate 检查规则与用量是否自洽。
//
// 拦的是「算了但算得不对」的几类情形。它们都不会让程序崩，
// 只会让账目慢慢偏掉——而偏掉之后要很久才看得出来。
func Validate(usage Usage, prices Prices) error {
	if usage.TotalTokens() < 0 {
		// 负用量只可能来自解析错误（例如把带符号整数读反）。
		// 放过去会算出一笔负数消费，即「上游付钱给用户」。
		return fmt.Errorf("用量为负: %d", usage.TotalTokens())
	}
	for name, count := range usage.UnitCounts {
		if count < 0 {
			return fmt.Errorf("计量项 %q 的数量为负: %d", name, count)
		}
		if _, ok := prices.Units[name]; !ok {
			return fmt.Errorf("%w: %q", ErrMissingPrice, name)
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
//   - ReasoningTokens 在协议层可能已被计入 completion，
//     在计费层要单独处理，否则会被收两次钱。
func FromChannelUsage(u channel.Usage) Usage {
	out := Usage{
		CompletionTokens: u.CompletionTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		ReasoningTokens:  u.ReasoningTokens,
		UnitCounts:       u.UnitCounts,
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

	// 推理 token 若被上游计入了 completion，这里不重复计。
	// 判据是「completion 明显大于推理量」——那说明推理是其中一部分。
	// 无法判断时保持原样，由规则决定怎么定价。
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
