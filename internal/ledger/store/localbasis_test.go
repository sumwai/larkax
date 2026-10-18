package store

import (
	"context"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/tokens"
)

// 当地口径那两列写的是 Charge 带来的值：地区时区的钟点与那一天算不算休息日
// （由 Settle 从**同一次求值**的结果搬过来，见 pipeline.Charge.LocalTime）。
// 它们是事后复算一笔分时单子缺的输入，所以取值与「有没有值」都要照实落库。
//
// # 两个方向都钉
//
// 只钉「有值」的话，把两列一律写成 NULL 也能通过。只钉「值写进去了」的话，
// 把「没填」与「当地 0 点、不是休息日」写成同一个东西也能通过——而这两种
// 读法在库里分不出，复算的人会把「不知道」当成一个具体的判定。
//
// 判定那一半（什么算「用到当地口径」）在 expr 的用例里钉；这里钉的是落库。
func TestSaveWritesLocalBasis(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	channelID := seedChannel(t, ctx, db)
	// 受理时刻取北京时间 23:30：钟点这一格不是 0，写没写看得出来。
	acceptAt := time.Date(2026, 10, 16, 15, 30, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		local *expr.LocalTime
	}{
		{"当地 23 点、是休息日", &expr.LocalTime{Hour: 23, OffDay: true}},
		// 0 点与「不是休息日」是**具体的判定**，不是「没填」：这两格在库里
		// 必须是 0 与 false 而不是 NULL，否则复算会把「不知道」当成它们。
		{"当地 0 点、不是休息日", &expr.LocalTime{Hour: 0, OffDay: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestID := reqID(t)
			req := testRequest(ctx, t, db, userID, requestID)
			billID, err := s.SaveReservation(ctx, req,
				reservation(t, userID, requestID, "30", "CNY"))
			if err != nil {
				t.Fatal(err)
			}

			settleReq := req
			settleReq.ChannelID = channelID
			charge := billedCharge(t, userID, requestID, billID, "12", tokens.SourceMeasured, acceptAt)
			charge.LocalTime = tc.local

			if err := s.Save(ctx, settleReq, charge); err != nil {
				t.Fatalf("结算失败: %v", err)
			}

			got := usageEventOf(ctx, t, db, requestID)
			if !got.LocalHour.Valid || int(got.LocalHour.Int64) != tc.local.Hour {
				t.Errorf("local_hour = %v，期望 %d（Charge 带来的那个钟点）",
					got.LocalHour, tc.local.Hour)
			}
			if !got.IsOffDay.Valid || got.IsOffDay.Bool != tc.local.OffDay {
				t.Errorf("is_off_day = %v，期望 %t（Charge 带来的那一格）",
					got.IsOffDay, tc.local.OffDay)
			}
		})
	}
}
