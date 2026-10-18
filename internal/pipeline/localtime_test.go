package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/tokens"
)

// 这一组钉住 Charge.LocalTime：结算要把**这次求值用到的**当地口径带走
// （地区时区的钟点与那一天算不算休息日），落库层照着写 usage_events 的
// local_hour / is_off_day。那两列是事后复算一笔分时单子缺的输入，
// 所以它们的来路必须说得清：取自同一次求值，而不是结算这一侧按 Region 重算。
//
// 取值的地方在 expr.Result.LocalTime（那边有一组用例钉判定与边界），这里钉的是
// **装配之后**这一段：求值结果 → Charge → 落库层要读的那两个值。测试自己接的线
// 与生产那条接得不一样时，只有跑装配的用例看得出来。

// peakCalendar 在 fakeCalendar 之上回答峰谷。
//
// 判定的实现在 internal/calendar 里测（窗口怎么算、跨零点算到哪一天），
// 这里只要一个能答的日历：本条用例问的是「峰谷那一档的当地口径有没有传下来」。
type peakCalendar struct {
	*fakeCalendar
	peak bool
}

func (c *peakCalendar) Peak(string, time.Time, []expr.PeakWindow) (bool, error) {
	return c.peak, nil
}

// 当地口径取的是**受理那一刻**的那次求值，不是结算那一刻重算的。
//
// 受理是周五 23:59、结算时墙钟已走过午夜（周六 00:01）。把「当地钟点」或
// 「哪一天」换成结算那一刻重算，这里拿到的会是 0 点与「休息日」——
// 而这两个数与这次的价没有任何关系：价是按受理那一刻的档位算的。
func TestSettleCarriesLocalBasisFromTheAcceptanceEvaluation(t *testing.T) {
	const rule = `isWorkday() ? tier("workday", {"prompt": 0.000001}) : tier("rest_day", {"prompt": 0.000004})`
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: rule}},
		func(p *Pipeline) {
			p.Region = "CN"
			p.Calendar = &fakeCalendar{
				tz: "Asia/Shanghai",
				// 2026-10-16 是周五，10-17 是周六。
				offDays: map[string]bool{"2026-10-17": true},
			}
		})

	accepted := shanghaiAt(t, 2026, 10, 16, 23, 59)
	p.Now = func() time.Time { return accepted }

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}

	p.Now = func() time.Time { return shanghaiAt(t, 2026, 10, 17, 0, 1) }

	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	// 先看这次求值判成了哪一档：下面那两个值必须与它说的是同一件事。
	if charge.Tier != "workday" {
		t.Fatalf("档位 = %q，期望 workday（受理那一刻是周五）", charge.Tier)
	}
	if charge.LocalTime == nil {
		t.Fatal("这一档是按工作日判出来的，当地口径却是 nil——" +
			"usage_events 那两格会写成 NULL，而这次请求的价正是按那一天算的")
	}
	if charge.LocalTime.Hour != 23 {
		t.Errorf("当地钟点 = %d，期望 23（受理那一刻，不是结算时刻的 0 点）",
			charge.LocalTime.Hour)
	}
	if charge.LocalTime.OffDay {
		t.Error("OffDay = true，但这次判的是工作日那一档——" +
			"那两个值与规则判出来的那一档必须是同一件事")
	}
	if !charge.PricedAt.Equal(accepted) {
		t.Errorf("PricedAt = %v，期望受理时刻 %v——它与当地口径描述的是同一次求值",
			charge.PricedAt, accepted)
	}
}

// 峰谷那一档同样要带走当地口径：窗口按当地零点起算，所以「当时当地几点」
// 正是复算这次分时计价缺的那个输入。
func TestSettleCarriesLocalBasisForPeakTier(t *testing.T) {
	const rule = `isPeak("08:00-22:00") ? tier("peak", {"prompt": 0.000004}) : tier("off_peak", {"prompt": 0.000001})`
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: rule}},
		func(p *Pipeline) {
			p.Region = "CN"
			p.Calendar = &peakCalendar{
				fakeCalendar: &fakeCalendar{tz: "Asia/Shanghai"},
				peak:         true,
			}
		})

	accepted := shanghaiAt(t, 2026, 10, 16, 17, 30)
	p.Now = func() time.Time { return accepted }

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	if charge.Tier != "peak" {
		t.Fatalf("档位 = %q，期望 peak", charge.Tier)
	}
	if charge.LocalTime == nil {
		t.Fatal("这一档是按峰谷判出来的，当地口径却是 nil")
	}
	if charge.LocalTime.Hour != 17 {
		t.Errorf("当地钟点 = %d，期望 17（受理那一刻的当地钟点）", charge.LocalTime.Hour)
	}
}

// 规则没用到地区那一套口径时是 nil，落库层写 NULL。
//
// 这里用 hour(tz) 的规则：它**是**分时规则，但问的是规则作者给的那个钟表，
// 与地区的日历不是一套口径（见 expr.Result.LocalTime）。按地区填一个值上去
// 会得到「看起来有依据、其实与判档用的那个不同」的数。
func TestSettleLocalBasisIsNilWhenRuleUsesNoRegionClock(t *testing.T) {
	const rule = `hour("Asia/Shanghai") >= 17
		? tier("peak", {"prompt": 0.000004})
		: tier("off_peak", {"prompt": 0.000001})`
	p := newPipeline(t, &ruleSource{rule: Rule{ID: 1, Version: 1, Source: rule}})

	p.Now = func() time.Time { return shanghaiAt(t, 2026, 10, 16, 17, 30) }

	req := baseRequest()
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatalf("预留出错: %v", err)
	}
	charge, err := p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000},
		UsageSource: tokens.SourceMeasured,
	})
	if err != nil {
		t.Fatalf("结算出错: %v", err)
	}

	if charge.Tier != "peak" {
		t.Fatalf("档位 = %q，期望 peak", charge.Tier)
	}
	if charge.LocalTime != nil {
		t.Errorf("当地口径 = %+v，期望 nil——这条规则只问了 hour(tz)，"+
			"没有用到地区那一套口径", *charge.LocalTime)
	}
}
