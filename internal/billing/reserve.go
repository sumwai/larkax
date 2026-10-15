package billing

import (
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/money"
)

// Reserve 计算一次请求的预留金额。
//
// # 按最坏情况，而不是典型值
//
// 预留必须是一个**上界**：结算的实际金额不能超过它。理由是「余额不为负」
// 这个不变量需要它——如果结算可能超过预留，那就得处理「余额为负」这个状态，
// 而那个状态的处置（允许欠款？冻结账号？）会牵扯到每一个读余额的地方。
//
// 代价是大请求冻住的余额多。但预留只在秒级内存在（请求结束即释放），
// 而例外要长期维护——这个取舍是明确的。
//
// 上界的构成：
//
//	输入侧：已知的输入 token × 输入单价（这部分请求前就算得出来）
//	输出侧：请求的 max_tokens × 输出单价（模型最多能写这么多）
//	其余：缓存与推理按最贵的单价估，宁可多留
func Reserve(req ReserveRequest) (money.Amount, error) {
	if err := req.validate(); err != nil {
		return money.Zero, err
	}

	p := req.Prices

	// 输入侧：请求前就知道有多少 token。
	input := p.Prompt.MulInt(req.InputTokens)
	// 缓存读命中会让实际更便宜，但预留要按贵的算。
	cacheRead := p.Prompt.MulInt(req.CacheReadTokens)

	// 输出侧：按 max_tokens 估。这是预留里最大的一块，
	// 也是「大请求冻住余额多」的来源。
	//
	// 用「输入 + 输出」里输出单价的较大者来估推理：推理单价通常更高，
	// 而预留宁可多留。
	outputPrice := money.Max(p.Completion, p.Reasoning)
	output := outputPrice.MulInt(req.MaxTokens)

	total := input.Add(cacheRead).Add(output)

	// 非 token 计量的部分按规则里给的单价预留**一次**。
	// 数量在请求前未知（一张图？十次搜索？），所以只留一份的量，
	// 并在结算时补足差额——这类计量的量级远小于 token，多留一份足够覆盖。
	for _, price := range p.Units {
		total = total.Add(price)
	}

	// 固定费用与最低消费：由规则给出的保底金额。
	total = money.Max(total, req.Minimum)

	return total, nil
}

// ReserveRequest 是预留计算的输入。
type ReserveRequest struct {
	Prices Prices

	// InputTokens 是请求的输入 token 数。请求前由分词器算出。
	InputTokens int64
	// CacheReadTokens 是预估的缓存命中量。请求前通常未知，留 0 即可——
	// 预留按贵的方向算，少算缓存只会让预留更保守。
	CacheReadTokens int64

	// MaxTokens 是请求允许的最大输出。这是上界的关键：
	// 不给它就没法算出上界，此时必须由调用方给一个兜底值。
	MaxTokens int64

	// Minimum 是保底金额（固定费用、最低消费），由规则求值得到。
	Minimum money.Amount
}

func (r ReserveRequest) validate() error {
	if r.InputTokens < 0 {
		return fmt.Errorf("输入 token 为负: %d", r.InputTokens)
	}
	if r.CacheReadTokens < 0 {
		return fmt.Errorf("缓存读 token 为负: %d", r.CacheReadTokens)
	}
	if r.MaxTokens <= 0 {
		// 没有 max_tokens 就算不出上界。用典型值兜底会让结算可能超过预留，
		// 从而破坏「余额不为负」——那正是预留机制要避免的。
		return fmt.Errorf("%w: max_tokens 必须为正，当前 %d", ErrNoUpperBound, r.MaxTokens)
	}
	if r.Minimum.IsNegative() {
		return fmt.Errorf("保底金额为负: %s", r.Minimum)
	}
	for name, price := range r.Prices.Units {
		if price.IsNegative() {
			return fmt.Errorf("计量项 %q 的单价为负: %s", name, price)
		}
	}
	if r.Prices.Prompt.IsNegative() || r.Prices.Completion.IsNegative() ||
		r.Prices.CacheRead.IsNegative() || r.Prices.CacheWrite.IsNegative() ||
		r.Prices.Reasoning.IsNegative() {
		return errors.New("单价为负")
	}
	return nil
}

// ErrNoUpperBound 表示无法为这次请求算出预留上界。
//
// 是最常见的成因是请求没给 max_tokens。单独一个错误类型是为了让调用方
// 能区别处置：配置层面补一个默认值，而不是把这个错当成上游故障。
var ErrNoUpperBound = errors.New("无法确定预留上界")

// Settlement 是一次结算的结果。
type Settlement struct {
	// Actual 是按实际用量算出的金额。
	Actual money.Amount
	// Reserved 是预先冻结的金额。
	Reserved money.Amount
	// Delta 是 Actual - Reserved。
	// 正数表示要再扣，负数表示要退回。
	Delta money.Amount
	// Breakdown 是实际用量的明细。
	Breakdown Breakdown
	// Capped 为 true 表示实际金额被预留上界截住了。
	//
	// 它不该发生——预留是上界。真发生了说明上界算错了（例如上游报的
	// 输出 token 超过了我们给的 max_tokens），那是一条需要查的信号，
	// 而不是把差额静默吞掉。
	Capped bool
}

// Settle 按实际用量结算。
//
// # 超过预留时截住而不是让余额变负
//
// 预留设计成上界，所以正常情况不会超。真超了有两个可能：
// 上游报的用量超出我们给的 max_tokens（上游不守信），或者上界算错了。
// 两种情况下**截住并标记**比让余额变负更好：
//
//   - 让余额变负会破坏那条不变量，从而需要在每个读余额的地方处理它；
//   - 静默吞掉差额等于平台承担了这笔钱，而且不会有任何人知道。
//
// 截住并置 Capped，让账目对得上，同时留下一条可查的信号。
func Settle(actual Usage, prices Prices, reserved money.Amount) (Settlement, error) {
	if err := Validate(actual, prices); err != nil {
		return Settlement{}, err
	}

	breakdown := Compute(actual, prices)
	actualAmount := breakdown.Total

	s := Settlement{
		Actual:    actualAmount,
		Reserved:  reserved,
		Breakdown: breakdown,
	}

	if actualAmount.Cmp(reserved) > 0 {
		s.Capped = true
		s.Actual = reserved
	} else {
		s.Capped = false
	}
	s.Delta = s.Actual.Sub(reserved)

	if err := s.Breakdown.Verify(); err != nil {
		return Settlement{}, err
	}
	return s, nil
}

// SettleUncapped 按实际用量结算，不设上界。
//
// 用于那些不该有上界的情形：请求已经在途且上游报了真实用量，
// 那时截住金额等于少收钱。调用方必须自己保证余额足够——
// 它通常是在已经有实测用量之后才调用这个。
func SettleUncapped(actual Usage, prices Prices) (Settlement, error) {
	if err := Validate(actual, prices); err != nil {
		return Settlement{}, err
	}
	breakdown := Compute(actual, prices)
	if err := breakdown.Verify(); err != nil {
		return Settlement{}, err
	}
	return Settlement{
		Actual:    breakdown.Total,
		Breakdown: breakdown,
	}, nil
}
