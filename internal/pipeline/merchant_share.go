package pipeline

import (
	"context"
	"strconv"
	"sync"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
)

// MerchantShare 是商户分成的比例来源：售价里有多大一部分分给渠道商户。
//
// 分给商户的金额由结算那一侧算（见 entries）：售价乘以比例，向下取整，
// 零头归平台。比例存在 settings 表里，单位是基点（1 bp = 0.01%），
// 取值 0…10000；0 表示不分账。
//
// # 为什么取值是一个函数
//
// 今天只有一张固定比例，与商户无关。按商户分档（例如按等级 8% / 7%）
// 是以后的事，取值点收在 BasisPointsFor 这一处——加档位时只改这里，
// 结算不必知道比例是按什么分的。
type MerchantShare struct {
	// BasisPoints 取当前配置的比例（基点）。返回 error 表示这一项读不出来：
	// 缺行，或者不是整数。为 nil 时一律按 0 处理，也就是不分账。
	BasisPoints func() (int64, error)

	// mu 保护 warned：结算由 gateway 单实例并发共用，告警的去重状态
	// 是被并发读写的同一个字段。
	mu sync.Mutex
	// warned 是上一次已经报过的那一个非法值，按原始文本记（越界时是数字，
	// 读不出来时是错误文本）。
	//
	// 每个请求报一条会把日志淹没，真正需要看见的那几条反而找不到；
	// 同一个值只报一次，值变了再报一次。值回到合法时清空，于是同一个
	// 非法值再次出现时会重新报——「一直是坏的」与「坏掉又好了又坏」
	// 在日志里应当分得开。
	warned string
}

// BasisPointsFor 返回这个商户这一笔消费适用的分成比例（基点）。
//
// 商户 id 为 0 表示渠道属于平台自己（见 RuleCandidate.OwnerUserID），
// 这时不分账：钱本来就留在平台，写一笔 merchant_payable 只会让它扮成
// 一个不存在的债权人。
//
// 比例读不出来或越界时返回 0 并留一条告警，而不是让这次结算失败：
// 一笔已经服务完的请求不该因为一个配置项写错而收不了钱，分成恒为 0
// 才是要有人看见的那件事——没有告警的话，它与「还没配置」在账面上
// 长得一模一样。
func (m *MerchantShare) BasisPointsFor(ctx context.Context, merchantID int64) int64 {
	if merchantID <= 0 {
		return 0
	}
	if m.BasisPoints == nil {
		return 0
	}

	bp, err := m.BasisPoints()
	if err != nil {
		m.warnOnce(ctx, err.Error(),
			"商户分成比例读不出来，这一次不分账", "err", err)
		return 0
	}
	if bp < 0 || bp > money.MaxBasisPoints {
		m.warnOnce(ctx, strconv.FormatInt(bp, 10),
			"商户分成比例超出 0…10000 基点，这一次不分账", "value", bp)
		return 0
	}
	m.clearWarned()
	return bp
}

// warnOnce 按 token 去重：同一个 token 只报一次。
//
// token 是那个非法值本身，所以「值变了」自然就是「token 变了」。
func (m *MerchantShare) warnOnce(ctx context.Context, token, msg string, args ...any) {
	m.mu.Lock()
	seen := m.warned != "" && m.warned == token
	m.warned = token
	m.mu.Unlock()
	if seen {
		return
	}
	observe.FromContext(ctx).Warn(msg, args...)
}

// clearWarned 表示比例回到了合法范围。清空之后，同一个非法值再次出现
// 会重新报一条（见 warned 的注释）。
func (m *MerchantShare) clearWarned() {
	m.mu.Lock()
	m.warned = ""
	m.mu.Unlock()
}
