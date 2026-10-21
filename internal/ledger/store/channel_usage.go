package store

import (
	"context"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// ChannelUsageTotals 汇总商家名下渠道在窗口内的用量。
//
// # 与 UsageSummary 的分工
//
// UsageSummary 按 usage_events.user_id 聚合——那是**发请求的人**（终端用户）。
// 商家运营页问的是另一件事：「我名下的渠道今天被用了多少」，所以分母是
// channels.owner_user_id，走的是 usage_events.channel_id 这条索引
// （usage_events_channel_idx）。两者不能互相顶替：一个商家的渠道可能服务过
// 很多用户，一个用户也可能用过很多商家的渠道。
//
// ownerID 为 nil 表示不按归属筛（运维视角的全局合计）。
//
// # 金额口径
//
// cost 是**售价**（调用方被计的那笔钱），不是 upstream_cost，也不是按
// merchant_share_bp 分成后的所得。分成是结算侧的事，放在运营概览里会让
// 「今日消耗」看起来像「今天能拿到多少钱」。
func (s *Store) ChannelUsageTotals(ctx context.Context, ownerID *int64,
	from, to time.Time,
) (UsageTotals, error) {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return UsageTotals{}, fmt.Errorf("%w: 窗口必须非空且 from 早于 to", ErrInvalidQuery)
	}

	args := []any{from, to}
	ownerCond := ""
	if ownerID != nil {
		if *ownerID <= 0 {
			return UsageTotals{}, fmt.Errorf("%w: 用户 ID 必须是正整数，得到 %d",
				ErrInvalidQuery, *ownerID)
		}
		args = append(args, *ownerID)
		ownerCond = " AND c.owner_user_id = $3"
	}

	var (
		out      UsageTotals
		costText string
	)
	// usageTotalsColumns 与 UsageSummary 共用：两处各写一遍时，漏一个 token
	// 槽位不会报错，只会让两边对同一批行给出不同的数。
	err := s.db.QueryRowContext(ctx, `
		SELECT `+usageTotalsColumns+`,
		       COUNT(DISTINCT e.request_id)
		  FROM usage_events e
		  JOIN channels c ON c.id = e.channel_id
		 WHERE e.created_at >= $1
		   AND e.created_at < $2`+ownerCond, args...,
	).Scan(&costText, &out.PromptTokens, &out.CompletionTokens,
		&out.CacheReadTokens, &out.CacheWriteTokens, &out.ReasoningTokens,
		&out.Requests)
	if err != nil {
		return UsageTotals{}, fmt.Errorf("汇总渠道用量失败: %w", err)
	}
	out.Cost, err = money.Parse(costText)
	if err != nil {
		return UsageTotals{}, fmt.Errorf("渠道用量金额 %q 解析失败: %w", costText, err)
	}
	return out, nil
}
