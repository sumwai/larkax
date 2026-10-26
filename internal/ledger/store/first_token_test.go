package store

import (
	"context"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// TestSettleRecordsFirstTokenMS 钉住结算把 TTFT 按**毫秒**写进 first_token_ms。
//
// 这个字段只有一条写入路径：markSettled 的 UPDATE（SET 在 store.go:1181，
// 换算在 :1189）。正因如此「单位对不对」只能在这一条路上测——它在 Go 侧换算
// 成毫秒（`int64(charge.FirstToken/time.Millisecond)`），库里存的是那个整数。
//
// 单位写错这一类错**不会自己暴露**：读回来的仍是「一个整数」，看着像毫秒，
// 只是大了六个数量级；按它做慢请求告警或分位数统计的人才会撞上。变异体
// 把除号改成乘号，写进去的是纳秒数再乘 10^6，对 `integer` 列直接溢出——
// 所以本用例断言的是**读回来的值等于 1500**，而不是「非 NULL」：后者在
// 单位错得离谱时也可能成立。
//
// 值刻意挑 1500ms：整毫秒，因此除法的结果不依赖取整口径，定下来的数字
// 只能是 1500。
func TestSettleRecordsFirstTokenMS(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	// 只有流式入口才测得到 TTFT（见 pipeline.Request.Streamed 的注释），而
	// streamed 这一列在预留时就落库了。不设它的话，造出来的行是「非流式却有
	// first_token_ms」——生产里不可能出现的组合。
	req.Streamed = true

	billID, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "10", billID),
		// 上游在 1.5 秒后吐出第一个内容块。只有流式请求才有这个值，非流式
		// 与「没吐过字」的零值落成 NULL（见 markSettled 里 NULLIF 那段注释）。
		FirstToken: 1500 * time.Millisecond,
	}
	charge.Settlement.Actual = money.MustParse("10")
	// 结算失败不用 Fatalf 挡在这里：本用例钉的是**读回来的毫秒数**，而变异体
	// 恰好先在写这一格溢出（纳秒 × 10^6 装不进 integer）。让它继续往下走，
	// 红才落在下面那句断言上，而不是落在一个「结算没成功」的过场里。
	// 这里的 err 单独起名：外层那个 err 是预留那一步的，复用会让 govet 的
	// shadow 检查报「遮蔽」，而把结算的错记到预留的变量上读起来也不对。
	if saveErr := s.Save(ctx, req, charge); saveErr != nil {
		t.Errorf("结算失败: %v", saveErr)
	}

	now := time.Now()
	page, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: now.Add(-time.Hour), To: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("读请求日志失败: %v", err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("请求日志有 %d 行，期望 1 行", len(page.Requests))
	}
	row := page.Requests[0]
	if row.FirstTokenMS == nil {
		t.Fatalf("first_token_ms 是 NULL，期望 1500：这笔请求上游已经吐过字，结算该把它记下来")
	}
	if *row.FirstTokenMS != 1500 {
		t.Errorf("first_token_ms = %d，期望 1500（受理到第一个内容块的毫秒数）", *row.FirstTokenMS)
	}
}
