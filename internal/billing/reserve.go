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
//	输入侧：已知的输入 token × prompt / cache_read / cache_write 里最贵的单价
//	输出侧：请求的 max_tokens × max(completion, reasoning) 单价
//	其余：非 token 计量各留一份，宁可多留
//
// 两侧都取「最贵的那个单价」，理由相同：请求前不知道结算会怎么拆，而按贵的估
// 才是与拆法无关的上界。逐条理由写在各自那一段的注释里。
func Reserve(req ReserveRequest) (money.Amount, error) {
	if err := req.validate(); err != nil {
		return money.Zero, err
	}

	p := req.Prices

	// 输入侧：请求前就知道有多少 token。
	//
	// InputTokens 估的是**整段** prompt（见 pipeline 的 inputTokensOf），而结算时
	// 那一段会按 prompt / cache_read / cache_write 三种单价拆开算。所以这一侧要按
	// **三者里最贵的**那个单价估，上界才与「结算怎么拆」无关。
	//
	// 原来这里固定用 p.Prompt，那等于假设「缓存读、缓存写的单价都不高于输入单价」。
	// 这个假设**没有任何地方被校验**——规则里把 cache_read 写成 prompt 的几倍是
	// 合法配置（两个数填反、抄错行都会这样），此时预留小于结算，每次命中缓存的
	// 请求都撞 Capped，差额静默由平台吃掉（账目仍然平，只在对账时表现为
	// 「用量对不上金额」）。
	//
	// 取三者最大值让上界不再依赖那个假设。这也是**与拆法无关的最紧上界**：
	// 把 N 个 token 全塞进最贵的那个桶，就是所有拆法的最大值。
	//
	// 代价要说清楚：**这不是「只在异常配置下才生效」的兜底。** 缓存写入的单价
	// 普遍**高于**输入（常见是 1.25～2 倍），所以正常的规则下取到的就是
	// p.CacheWrite，输入侧的预留比原来（固定按 p.Prompt）大一截。也就是说，
	// 改了之后每一条按缓存写入定价的规则都会多冻一点余额。
	//
	// 这个方向是必须的：宁可多冻，不可静默少收。而多冻的那部分只在请求期间
	// 存在（结束即释放），且输入侧通常不是预留里最大的一块（输出侧按 max_tokens
	// 估，往往更大）。
	inputPrice := money.Max(money.Max(p.Prompt, p.CacheRead), p.CacheWrite)
	input := inputPrice.MulInt(req.InputTokens)
	// 缓存读、缓存写这两项是**在整段 prompt 之外**额外给的估计量，所以照旧各按
	// 自己的单价再算一遍。生产里它们目前恒为 0——入口没有填 pipeline.Request.Usage，
	// 所以上界实际由上面那一侧单独撑着。这两项加不加都不改变今天的结论；
	// 一旦有人把估计量填起来，多算的这一点是偏保守的方向。
	cacheRead := money.Max(p.Prompt, p.CacheRead).MulInt(req.CacheReadTokens)
	cacheWrite := p.CacheWrite.MulInt(req.CacheWriteTokens)

	// 输出侧：按 max_tokens 估。这是预留里最大的一块，
	// 也是「大请求冻住余额多」的来源。
	//
	// 用输出侧两个单价（Completion 与 Reasoning）的较大者来估：推理单价
	// 通常更高，而预留宁可多留。
	outputPrice := money.Max(p.Completion, p.Reasoning)
	output := outputPrice.MulInt(req.MaxTokens)

	// 推理 token：有的上游把它算在 completion 里，有的单独报。
	// 预留按 max_tokens 那一块已经用 max(Completion, Reasoning) 覆盖了，
	// 所以这里**不重复加**——重复加会让预留虚高，那本身无害，
	// 但会让「预留是不是上界」这条不变量变得难以验证。
	total := input.Add(cacheRead).Add(cacheWrite).Add(output)

	// 非 token 计量的部分按规则里给的单价预留**一次**。
	// 数量在请求前未知（一张图？十次搜索？），所以只留一份的量。
	//
	// 这是**已知会低估**：结算在上界处是截断（Capped 时 Actual 就等于预留），
	// 不会补收差额。之所以接受，是因为这类计量的量级远小于 token，
	// 且没有别的可用估计；但别把它读成「多留一份足够覆盖」。
	for _, price := range p.Units {
		total = total.Add(price)
	}

	// 一次性费用。顺序很要紧，写反了预留就会小于结算。
	//
	// 最低消费是**按量部分的下限**，固定费永远另加：先取 max 再加，
	// 与 ComputeWithCharges 里的 applyMinimum 一致。
	// 写成「先加固定费再取 max」的话，`固定费 0.5 + 最低消费 1` 的规则
	// 会给出预留 1 而结算 1.5——每次请求都撞 Capped，
	// 而 Capped 本该是「上界算错了」的信号，不该成为常态。
	total = money.Max(total, req.Charges.Minimum)
	total = total.Add(req.Charges.Fixed)

	return total, nil
}

// ReserveRequest 是预留计算的输入。
type ReserveRequest struct {
	Prices Prices

	// InputTokens 是请求的输入 token 数。请求前由分词器算出。
	InputTokens int64
	// CacheReadTokens 是**在 InputTokens 之外**额外预估的缓存命中量。
	//
	// 它与 InputTokens 不是「总量与其中一部分」的关系：InputTokens 是整段
	// prompt 的估计量，结算时那一段会按 prompt / cache_read / cache_write
	// 三种单价拆开算，而拆法请求前未知。所以输入侧已经按三者里最贵的单价
	// 估过一遍（见 Reserve 里那段的说明），这里给的值是**再叠上去**的。
	//
	// 请求前通常未知，留 0 即可——留 0 不会让上界失效，输入侧自己就撑得住。
	CacheReadTokens int64

	// CacheWriteTokens 是**在 InputTokens 之外**额外预估的缓存写入量。
	//
	// 与 CacheReadTokens 同理：输入侧已经按三者里最贵的单价估过一遍，
	// 这里给的是再叠上去的。请求前通常未知，留 0 即可。
	//
	// 它与缓存读分成两个字段而不是合成一个，是因为两者的单价不同——
	// 缓存写入通常**高于**输入（有的上游是 1.25 倍、有的是 2 倍），
	// 真要填估计量时两个量得分开算。
	CacheWriteTokens int64

	// MaxTokens 是请求允许的最大输出。这是上界的关键：
	// 不给它就没法算出上界，此时必须由调用方给一个兜底值。
	MaxTokens int64

	// Charges 是一次性费用（固定费、最低消费），由规则求值得到。
	//
	// 预留必须把它算进去，否则预留会小于结算——固定费与保底都是
	// 结算时会收的钱，而预留是上界。
	Charges Charges
}

func (r ReserveRequest) validate() error {
	if r.InputTokens < 0 {
		return fmt.Errorf("输入 token 为负: %d", r.InputTokens)
	}
	if r.CacheReadTokens < 0 {
		return fmt.Errorf("缓存读 token 为负: %d", r.CacheReadTokens)
	}
	if r.CacheWriteTokens < 0 {
		return fmt.Errorf("缓存写 token 为负: %d", r.CacheWriteTokens)
	}
	if r.MaxTokens <= 0 {
		// 没有 max_tokens 就算不出上界。用典型值兜底会让结算可能超过预留，
		// 从而破坏「余额不为负」——那正是预留机制要避免的。
		return fmt.Errorf("%w: max_tokens 必须为正，当前 %d", ErrNoUpperBound, r.MaxTokens)
	}
	if err := r.Charges.Validate(); err != nil {
		return err
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
//
// # 「算出来多少」与「收了多少钱」是两个数
//
// 没撞上界时它们相等；撞上界时 Actual 被截成 Reserved，而**算出来的那个数
// 还在 Breakdown.Total 里**——截断只改 Actual，明细一项都不动。所以
//
//	Actual = min(Breakdown.Total, Reserved)
//
// 在每次结算上都成立，而「这次撞了上界」等价于 Breakdown.Total > Reserved。
//
// 需要这个差的不只是排查：落库时 computed_amount 取自 Breakdown.Total
// （见 internal/ledger/store 的 markSettled），差额就是被预留截住、由平台
// 承担掉的那笔钱。**别把它读成「Breakdown 是没用的中间产物」**——它是这两个
// 数里唯一那个「按用量该收多少」的答案。
//
// 这里**没有**「差额」字段：退回多少就是 Reserved - Actual，而这两个数都是
// 本结构体自己的字段，任何读者都能当场减出来。再存一份同义的数，只是多出
// 一处迟早与它们不一致的地方。落库时那次退回也不在 Go 里算——解冻走的是
// SQL 里的 reserved_amount（见 internal/ledger/store 的 releaseFrozen）。
type Settlement struct {
	// Actual 是按实际用量算出的金额，**撞上上界时是 Reserved**（被截断）。
	// 未截断的那个数在 Breakdown.Total 里。
	Actual money.Amount
	// Reserved 是预先冻结的金额。
	Reserved money.Amount
	// Breakdown 是实际用量的明细。它的 Total 是**未截断**的计算金额，
	// 见类型注释。
	Breakdown Breakdown
	// Capped 为 true 表示实际金额被预留上界截住了。
	//
	// 它不该发生——预留是上界。真发生了说明上界算错了（例如上游报的
	// 输出 token 超过了我们给的 max_tokens），那是一条需要查的信号，
	// 而不是把差额静默吞掉。
	//
	// 它与 Breakdown.Total.Cmp(Reserved) > 0 是同一件事，只是给调用方一个
	// 不用自己比的口径。**进程内的聚合读者是 internal/pipeline 的
	// CappedSettlements 计数**；要查「是哪一次请求撞的、撞了多少」，那个
	// 计数答不了——那是落库的 computed_amount 的活（见迁移 00021）。
	Capped bool
}

// SettleWithCharges 按实际用量结算，算上固定费用与最低消费。
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
//
// **结算必须用与预留同一组 Charges。** 若预留算了保底而结算没算，
// 一笔用量很小的请求会被预留 1 元、结算 0.002 元，差额全额退回——
// 保底永远收不到，而且不会报错，只是长期少收钱。
func SettleWithCharges(actual Usage, prices Prices, charges Charges, reserved money.Amount) (Settlement, error) {
	if err := ValidateCharges(actual, prices, charges); err != nil {
		return Settlement{}, err
	}

	breakdown := ComputeWithCharges(actual, prices, charges)
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

	if err := s.Breakdown.Verify(); err != nil {
		return Settlement{}, err
	}
	return s, nil
}
