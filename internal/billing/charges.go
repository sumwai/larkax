package billing

import (
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/money"
)

// Charges 是一次性费用，与单价分开。
//
// 为什么与 Prices 分开而不是混在一起：它们的**量纲不同**。
// Prices 里每项都是「每 token 多少元」，要与用量相乘；
// 而 Fixed 是「每请求多少元」，与用量无关。
// 混成一个结构会让「哪些字段要乘用量」变成需要读文档才知道的事，
// 而漏乘一次就是一笔错账。
type Charges struct {
	// Fixed 是每请求的固定费用，与用量无关。
	Fixed money.Amount
	// Minimum 是最低消费。实际金额低于它时补足到它。
	//
	// 它是**下限**而不是加项：一笔用量很小的请求不该被收两次钱
	//（一次按量、一次补差），而是直接按最低消费收。
	Minimum money.Amount
}

// IsZero 判断是否没有任何一次性费用。
func (c Charges) IsZero() bool { return c.Fixed.IsZero() && c.Minimum.IsZero() }

// Validate 检查一次性费用是否合法。
func (c Charges) Validate() error {
	if c.Fixed.IsNegative() {
		return fmt.Errorf("固定费用为负: %s", c.Fixed)
	}
	if c.Minimum.IsNegative() {
		return fmt.Errorf("最低消费为负: %s", c.Minimum)
	}
	return nil
}

// applyFixed 把固定费用写进明细的 Fixed 字段。
func (b *Breakdown) applyFixed(fixed money.Amount) {
	b.Fixed = fixed
}

// applyMinimum 把最低消费补差加进明细。
//
// 记录成一条**显式的调整项**（MinimumAdjustment）而不是直接抬高 Total：
// 那样 Total 就仍然等于所有行项之和，Verify() 不需要为例外开口子。
// 对账时也能一眼看出「这一部分是保底补的」。
func (b *Breakdown) applyMinimum(minimum money.Amount) {
	subtotal := b.Subtotal()
	if subtotal.Cmp(minimum) >= 0 {
		return
	}
	b.MinimumAdjustment = minimum.Sub(subtotal)
}

// ValidateCharges 与 Validate 合并成一个入口，避免调用方漏掉一半的校验。
func ValidateCharges(usage Usage, prices Prices, charges Charges) error {
	if err := Validate(usage, prices); err != nil {
		return err
	}
	return charges.Validate()
}

// ErrNegativeAmount 表示算出了一个负金额。
var ErrNegativeAmount = errors.New("金额为负")
