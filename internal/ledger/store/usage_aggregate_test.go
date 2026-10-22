package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是控制台用量聚合（汇总 / 序列 / 分组）在真库上的用例。
//
// 这些断言里有几类东西只有数据库给得出来：分区裁剪后的时间窗、numeric 的
// 十进制求和、按 request_id 去重计数，以及多组折叠成 Other 时的再加总。把它们
// 换成内存实现，测到的就是「我以为 SQL 会怎么算」。

// usageWorld 是一组跨两个用户、两个 key、三个模型的用量数据。
type usageWorld struct {
	userID    int64
	key1      int64
	key2      int64
	channelID int64
	requestA  string
	requestB  string
	requestC  string
	requestD  string
	requestE  string
	requestF  string
	requestG  string
	requestX  string
}

// usageWindow 是这组数据的主窗口：2026 年 10 月 1 日到 8 日（左闭右开）。
func usageWindow(t *testing.T, userID int64) UsageWindow {
	t.Helper()
	return UsageWindow{
		UserID: userID,
		From:   usageTestTime(t, "2026-10-01T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-08T00:00:00Z"),
	}
}

// seedUsageWorld 造出后续三个用例共用的那批行。
//
//	req-A  key1  gpt-4o        settled 0.50，面值 3.00
//	req-F  key1  gpt-4o        settled 1.00，面值 1.50
//	req-B  key1  gpt-4o        pending 预留 0.50，没有事件
//	req-C  key2  claude-3      settled 2.00，面值 4.00
//	req-E  key1  gpt-4o-mini   settled 0.25，面值 0（没有用包）
//	req-G  key1  gpt-4o-mini   settled 0，面值 2.00（用了包、实收为 0）
//	req-D  key1  gpt-4o        settled 7.00，面值 7.00，但整行落在 11 月（窗口外）
//	req-X  另一个用户           settled 100.00，面值 100.00（不该出现在这个用户的任何读里）
//
// **每个请求只落一行事件**：用量事件只有结算路径一个写入口，而它按 pending
// 账单行只走一次（见 store.Save）——同一请求两行在现在的代码里造不出来，
// 拿它当夹具测到的是一个不存在的形状。
//
// 面值故意与 cost 取不同的数：两个口径相等时，「读错了列」与「读对了」在断言
// 上长得一样。req-E 与 req-G 各占一格相反的情形——有 cost 没用包、用了包实收
// 为 0——读侧把两列混起来时，这两格会各错一次。
func seedUsageWorld(ctx context.Context, t *testing.T, db *sql.DB) usageWorld {
	t.Helper()

	world := usageWorld{
		userID:    seedUsageUser(ctx, t, db),
		channelID: seedChannel(t, ctx, db),
		requestA:  requestIDOf("usage-a"),
		requestB:  requestIDOf("usage-b"),
		requestC:  requestIDOf("usage-c"),
		requestD:  requestIDOf("usage-d"),
		requestE:  requestIDOf("usage-e"),
		requestF:  requestIDOf("usage-f"),
		requestG:  requestIDOf("usage-g"),
		requestX:  requestIDOf("usage-x"),
	}
	world.key1 = seedAPIKey(ctx, t, db, world.userID)
	world.key2 = seedAPIKey(ctx, t, db, world.userID)
	for id, name := range map[int64]string{world.key1: "第一把", world.key2: "第二把"} {
		if _, err := db.ExecContext(ctx,
			`UPDATE api_keys SET name = $2 WHERE id = $1`, id, name); err != nil {
			t.Fatalf("改 key 名字失败: %v", err)
		}
	}

	otherUser := seedUsageUser(ctx, t, db)
	otherKey := seedAPIKey(ctx, t, db, otherUser)

	settledA := usageTestTime(t, "2026-10-02T10:00:05Z")
	settledF := usageTestTime(t, "2026-10-02T10:00:25Z")
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: world.userID, KeyID: &world.key1, ChannelID: world.channelID,
		RequestID: world.requestA, Model: "gpt-4o",
		Status: BillingStatusSettled, Reserved: "0", PlanReserved: "3.00", Final: "0.50",
		Streamed: true, StartedAt: usageTestTime(t, "2026-10-02T10:00:00Z"),
		SettledAt: &settledA,
	})
	// 与 req-A 同一小时、同一模型、同一把 key 的**另一笔请求**：两次请求的
	// 面值相加才是那个桶的合计。
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: world.userID, KeyID: &world.key1, ChannelID: world.channelID,
		RequestID: world.requestF, Model: "gpt-4o",
		Status: BillingStatusSettled, Reserved: "0", PlanReserved: "1.50", Final: "1.00",
		StartedAt: usageTestTime(t, "2026-10-02T10:00:15Z"),
		SettledAt: &settledF,
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: world.userID, KeyID: &world.key1, ChannelID: world.channelID,
		RequestID: world.requestB, Model: "gpt-4o",
		Status: BillingStatusPending, Reserved: "0.50",
		StartedAt: usageTestTime(t, "2026-10-03T12:00:00Z"),
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: world.userID, KeyID: &world.key2, ChannelID: world.channelID,
		RequestID: world.requestC, Model: "claude-3",
		Status: BillingStatusSettled, Reserved: "0", PlanReserved: "4.00", Final: "2.00",
		StartedAt: usageTestTime(t, "2026-10-04T09:00:00Z"),
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: world.userID, KeyID: &world.key1, ChannelID: world.channelID,
		RequestID: world.requestE, Model: "gpt-4o-mini",
		Status: BillingStatusSettled, Reserved: "0.25", Final: "0.25",
		StartedAt: usageTestTime(t, "2026-10-05T15:00:00Z"),
	})
	// 这笔用了额度包、实收为 0：面值与 cost 的方向与 req-E 相反。
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: world.userID, KeyID: &world.key1, ChannelID: world.channelID,
		RequestID: world.requestG, Model: "gpt-4o-mini",
		Status: BillingStatusSettled, Reserved: "0", PlanReserved: "2.00", Final: "0",
		StartedAt: usageTestTime(t, "2026-10-06T08:00:00Z"),
	})
	// 窗口外：事件落在 11 月分区，账单也落在 11 月，两边都不该被选中。
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: world.userID, KeyID: &world.key1, ChannelID: world.channelID,
		RequestID: world.requestD, Model: "gpt-4o",
		Status: BillingStatusSettled, Reserved: "0", PlanReserved: "7.00", Final: "7.00",
		StartedAt: usageTestTime(t, "2026-11-02T10:00:00Z"),
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: otherUser, KeyID: &otherKey, ChannelID: world.channelID,
		RequestID: world.requestX, Model: "gpt-4o",
		Status: BillingStatusSettled, Reserved: "0", PlanReserved: "100.00", Final: "100.00",
		StartedAt: usageTestTime(t, "2026-10-02T11:00:00Z"),
	})

	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: world.requestA, UserID: world.userID, ChannelID: world.channelID,
		Model: "gpt-4o", PromptTokens: 100, CompletionTokens: 50,
		CacheReadTokens: 10, CacheWriteTokens: 20, ReasoningTokens: 5,
		Path: "measured", Cost: "0.50", PlanReserved: "3.00",
		CreatedAt: usageTestTime(t, "2026-10-02T10:00:10Z"),
	})
	// 另一笔请求，落在与 req-A 同一小时桶里：桶的面值是两笔之和。
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: world.requestF, UserID: world.userID, ChannelID: world.channelID,
		Model: "gpt-4o", PromptTokens: 200, CompletionTokens: 60,
		Path: "estimated", Cost: "1.00", PlanReserved: "1.50",
		CreatedAt: usageTestTime(t, "2026-10-02T10:00:20Z"),
	})
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: world.requestC, UserID: world.userID, ChannelID: world.channelID,
		Model: "claude-3", PromptTokens: 1000, CompletionTokens: 500,
		Path: "measured", Cost: "2.00", PlanReserved: "4.00",
		CreatedAt: usageTestTime(t, "2026-10-04T09:00:10Z"),
	})
	// 这笔没有用额度包：面值留默认 0，cost 仍是 0.25。
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: world.requestE, UserID: world.userID, ChannelID: world.channelID,
		Model: "gpt-4o-mini", PromptTokens: 50,
		Path: "measured", Cost: "0.25",
		CreatedAt: usageTestTime(t, "2026-10-05T15:00:10Z"),
	})
	// 这笔用了额度包、实收为 0：cost 与面值在这一格方向相反。
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: world.requestG, UserID: world.userID, ChannelID: world.channelID,
		Model: "gpt-4o-mini", PromptTokens: 80,
		Path: "measured", Cost: "0", PlanReserved: "2.00",
		CreatedAt: usageTestTime(t, "2026-10-06T08:00:10Z"),
	})
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: world.requestD, UserID: world.userID, ChannelID: world.channelID,
		Model: "gpt-4o", PromptTokens: 7000, Path: "measured", Cost: "7.00",
		PlanReserved: "7.00",
		CreatedAt:    usageTestTime(t, "2026-11-02T10:00:10Z"),
	})
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: world.requestX, UserID: otherUser, ChannelID: world.channelID,
		Model: "gpt-4o", PromptTokens: 9000, Path: "measured", Cost: "100.00",
		PlanReserved: "100.00",
		CreatedAt:    usageTestTime(t, "2026-10-02T11:00:10Z"),
	})

	return world
}

func TestUsageSummaryTotalsAndPaths(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	world := seedUsageWorld(ctx, t, db)
	s := New(db)

	summary, err := s.UsageSummary(ctx, usageWindow(t, world.userID))
	if err != nil {
		t.Fatalf("UsageSummary 失败: %v", err)
	}

	if got, want := summary.Totals.Cost, money.MustParse("3.75"); got != want {
		t.Errorf("总额 = %s，期望 %s（0.50 + 1.00 + 2.00 + 0.25 + 0）", got, want)
	}
	// 面值与 cost 是两个口径：req-E 的 cost 是 0.25 而面值是 0，req-G 反过来。
	// 所以这一列的合计不等于 cost 的合计（10.50 vs 3.75）。读错列时会正好差出
	// 这两笔。
	if got, want := summary.Totals.PlanReservedAmount, money.MustParse("10.50"); got != want {
		t.Errorf("额度包面值 = %s，期望 %s（3.00 + 1.50 + 4.00 + 0 + 2.00）", got, want)
	}
	// A、F 是两笔请求（不是同一请求的两行），所以这里是 5。
	if summary.Totals.Requests != 5 {
		t.Errorf("请求数 = %d，期望 5（按 request_id 去重）", summary.Totals.Requests)
	}
	tokens := []struct {
		name string
		got  int64
		want int64
	}{
		{"prompt", summary.Totals.PromptTokens, 1430},
		{"completion", summary.Totals.CompletionTokens, 610},
		{"cache_read", summary.Totals.CacheReadTokens, 10},
		{"cache_write", summary.Totals.CacheWriteTokens, 20},
		{"reasoning", summary.Totals.ReasoningTokens, 5},
	}
	for _, token := range tokens {
		if token.got != token.want {
			t.Errorf("%s tokens = %d，期望 %d", token.name, token.got, token.want)
		}
	}

	if len(summary.Paths) != 3 {
		t.Fatalf("路径档数 = %d，期望 3", len(summary.Paths))
	}
	wantPaths := []struct {
		path     string
		cost     string
		requests int64
	}{
		{"measured", "2.75", 4},
		{"estimated", "1.00", 1},
		// 表上的 CHECK 只允许前两档（fallback 没有产出点），所以这一档
		// 永远是 0；响应仍然把它列出来，形状与接口约定一致。
		{"fallback", "0.00", 0},
	}
	for i, want := range wantPaths {
		got := summary.Paths[i]
		if got.Path != want.path || got.Cost != money.MustParse(want.cost) ||
			got.Requests != want.requests {
			t.Errorf("Paths[%d] = %+v，期望 %s / %s / %d 次",
				i, got, want.path, want.cost, want.requests)
		}
	}

	// 只有一档有数据时，另外两档仍然在，金额为 0——图表不该缺线。
	modelWindow := usageWindow(t, world.userID)
	modelWindow.Model = "gpt-4o-mini"
	onlyMeasured, err := s.UsageSummary(ctx, modelWindow)
	if err != nil {
		t.Fatalf("按模型汇总失败: %v", err)
	}
	if len(onlyMeasured.Paths) != 3 {
		t.Fatalf("只有一档数据时路径档数 = %d，期望仍是 3", len(onlyMeasured.Paths))
	}
	if onlyMeasured.Paths[1].Path != "estimated" ||
		onlyMeasured.Paths[1].Cost != money.Zero ||
		onlyMeasured.Paths[2].Path != "fallback" ||
		onlyMeasured.Paths[2].Cost != money.Zero {
		t.Errorf("空档没有补零: %+v", onlyMeasured.Paths)
	}

	// 按 key 过滤：req-B 是 pending 且没有事件，所以不计入；req-D 在窗口外；
	// req-C 用的是 key2。
	keyWindow := usageWindow(t, world.userID)
	keyWindow.APIKeyID = &world.key1
	byKey, err := s.UsageSummary(ctx, keyWindow)
	if err != nil {
		t.Fatalf("按 key 汇总失败: %v", err)
	}
	if byKey.Totals.Cost != money.MustParse("1.75") || byKey.Totals.Requests != 4 {
		t.Errorf("key1 汇总 = %s / %d 次，期望 1.75 / 4",
			byKey.Totals.Cost, byKey.Totals.Requests)
	}
	if byKey.Totals.PlanReservedAmount != money.MustParse("6.50") {
		t.Errorf("key1 额度包面值 = %s，期望 6.50（3.00 + 1.50 + 0 + 2.00）",
			byKey.Totals.PlanReservedAmount)
	}

	// 空窗口不是错误，是三个 0；面值也是 0（不是 NULL，也不是空串）。
	empty := UsageWindow{
		UserID: world.userID,
		From:   usageTestTime(t, "2026-10-20T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-21T00:00:00Z"),
	}
	emptySummary, err := s.UsageSummary(ctx, empty)
	if err != nil {
		t.Fatalf("空窗口汇总失败: %v", err)
	}
	if emptySummary.Totals.Cost != money.Zero || emptySummary.Totals.Requests != 0 {
		t.Errorf("空窗口 = %+v", emptySummary.Totals)
	}
	if emptySummary.Totals.PlanReservedAmount != money.Zero {
		t.Errorf("空窗口的额度包面值 = %s，期望 0",
			emptySummary.Totals.PlanReservedAmount)
	}
}

func TestUsageSummaryRejectsBadQueries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	zeroUser := UsageWindow{
		UserID: 0,
		From:   usageTestTime(t, "2026-10-01T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-08T00:00:00Z"),
	}
	if _, err := s.UsageSummary(ctx, zeroUser); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("用户 ID 为 0 应当报 ErrInvalidQuery，得到 %v", err)
	}

	emptyWindow := UsageWindow{
		UserID: 1,
		From:   usageTestTime(t, "2026-10-01T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-01T00:00:00Z"),
	}
	if _, err := s.UsageSummary(ctx, emptyWindow); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("空窗口应当报 ErrInvalidQuery，得到 %v", err)
	}

	// 只给一边也要拒绝：零值时刻是公元 1 年，它总在另一边之前，后面那道
	// 「起点不早于终点」的检查拦不住少给一边的窗口——缺的那一边会安静地
	// 变成「从有时间以来」。变异：`From.IsZero() || To.IsZero()` 的 `||`
	// 改成 `&&`，只缺起点时这一句不再报错。
	for name, only := range map[string]UsageWindow{
		"只给起点": {UserID: 1, From: usageTestTime(t, "2026-10-01T00:00:00Z")},
		"只给终点": {UserID: 1, To: usageTestTime(t, "2026-10-08T00:00:00Z")},
	} {
		if _, err := s.UsageSummary(ctx, only); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s 应当报 ErrInvalidQuery，得到 %v", name, err)
		}
	}
}

func TestUsageSeriesFillsEmptyBuckets(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	world := seedUsageWorld(ctx, t, db)
	s := New(db)

	points, err := s.UsageSeries(ctx, usageWindow(t, world.userID), "day", time.UTC)
	if err != nil {
		t.Fatalf("UsageSeries 失败: %v", err)
	}
	if len(points) != 7 {
		t.Fatalf("日序列点数 = %d，期望 7（10-01 到 10-07）", len(points))
	}
	for i, point := range points {
		want := usageTestTime(t, "2026-10-01T00:00:00Z").AddDate(0, 0, i)
		if !point.Start.Equal(want) {
			t.Errorf("points[%d].Start = %s，期望 %s", i, point.Start, want)
		}
	}
	// 10-02 有 req-A 与 req-F 两笔请求：次数是 2，金额是两笔之和。
	if points[1].Cost != money.MustParse("1.50") || points[1].Requests != 2 {
		t.Errorf("10-02 桶 = %s / %d 次，期望 1.50 / 2",
			points[1].Cost, points[1].Requests)
	}
	if points[1].PlanReservedAmount != money.MustParse("4.50") {
		t.Errorf("10-02 桶的额度包面值 = %s，期望 4.50（3.00 + 1.50）",
			points[1].PlanReservedAmount)
	}
	if points[3].Cost != money.MustParse("2.00") || points[3].PromptTokens != 1000 {
		t.Errorf("10-04 桶 = %+v", points[3].UsageTotals)
	}
	if points[3].PlanReservedAmount != money.MustParse("4.00") {
		t.Errorf("10-04 桶的额度包面值 = %s，期望 4.00",
			points[3].PlanReservedAmount)
	}
	if points[4].Cost != money.MustParse("0.25") {
		t.Errorf("10-05 桶 = %s，期望 0.25", points[4].Cost)
	}
	// 这一桶有 cost 但没有用额度包：面值是 0，不是继承来的 0.25。
	if points[4].PlanReservedAmount != money.Zero {
		t.Errorf("10-05 桶的额度包面值 = %s，期望 0（这一笔没有用包）",
			points[4].PlanReservedAmount)
	}
	// 10-06 是 req-G：用了包但实收为 0，与上一桶方向相反。
	if points[5].Cost != money.Zero || points[5].Requests != 1 {
		t.Errorf("10-06 桶 = %s / %d 次，期望 0 / 1", points[5].Cost, points[5].Requests)
	}
	if points[5].PlanReservedAmount != money.MustParse("2.00") {
		t.Errorf("10-06 桶的额度包面值 = %s，期望 2.00（req-G 的面值）",
			points[5].PlanReservedAmount)
	}
	for _, i := range []int{0, 2, 6} {
		if points[i].Cost != money.Zero || points[i].Requests != 0 ||
			points[i].PlanReservedAmount != money.Zero {
			t.Errorf("空桶 points[%d] = %+v，期望全零", i, points[i].UsageTotals)
		}
	}

	// 小时粒度：09:00 与 11:00 都是空桶（另一个用户的行在 11:00）。
	hourWindow := UsageWindow{
		UserID: world.userID,
		From:   usageTestTime(t, "2026-10-02T09:00:00Z"),
		To:     usageTestTime(t, "2026-10-02T12:00:00Z"),
	}
	hours, err := s.UsageSeries(ctx, hourWindow, "hour", time.UTC)
	if err != nil {
		t.Fatalf("小时序列失败: %v", err)
	}
	if len(hours) != 3 {
		t.Fatalf("小时序列点数 = %d，期望 3", len(hours))
	}
	if hours[0].Cost != money.Zero || hours[2].Cost != money.Zero {
		t.Errorf("小时空桶 = %+v / %+v", hours[0], hours[2])
	}
	if hours[1].Cost != money.MustParse("1.50") || hours[1].Requests != 2 {
		t.Errorf("10:00 桶 = %s / %d", hours[1].Cost, hours[1].Requests)
	}
	if hours[1].PlanReservedAmount != money.MustParse("4.50") {
		t.Errorf("10:00 桶的额度包面值 = %s，期望 4.50", hours[1].PlanReservedAmount)
	}
}

func TestUsageSeriesRejectsBadWindows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	base := UsageWindow{
		UserID: 1,
		From:   usageTestTime(t, "2026-10-01T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-08T00:00:00Z"),
	}

	if _, err := s.UsageSeries(ctx, base, "week", time.UTC); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("未知粒度应当报 ErrInvalidQuery，得到 %v", err)
	}
	if _, err := s.UsageSeries(ctx, base, "day", nil); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("时区为空应当报 ErrInvalidQuery，得到 %v", err)
	}

	// 638 天：日序列超过 366 个点。
	long := base
	long.From = usageTestTime(t, "2025-01-01T00:00:00Z")
	long.To = usageTestTime(t, "2026-10-01T00:00:00Z")
	if _, err := s.UsageSeries(ctx, long, "day", time.UTC); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("超过日上限应当报 ErrInvalidQuery，得到 %v", err)
	}

	// 32 天：768 个小时桶，超过 744。
	longHours := base
	longHours.To = base.From.Add(32 * 24 * time.Hour)
	if _, err := s.UsageSeries(ctx, longHours, "hour", time.UTC); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("超过小时上限应当报 ErrInvalidQuery，得到 %v", err)
	}

	// 正好 366 个点要放行：上限说的是「最多这么多点」。变异：
	// `len(starts) > maxPoints` 改成 `>=`，把边界上的窗口也拒掉。
	// 窗口是左闭右开，所以 366 个日桶对应 366 天的跨度。
	edge := base
	edge.From = usageTestTime(t, "2025-10-01T00:00:00Z")
	edge.To = edge.From.AddDate(0, 0, 366)
	points, err := s.UsageSeries(ctx, edge, "day", time.UTC)
	if err != nil {
		t.Fatalf("正好 %d 个点的窗口应当放行，得到 %v", UsageSeriesMaxDays, err)
	}
	if len(points) != UsageSeriesMaxDays {
		t.Errorf("边界窗口点数 = %d，期望 %d", len(points), UsageSeriesMaxDays)
	}

	// 窗口本身自洽性：起点不早于终点。
	bad := base
	bad.From = base.To
	if _, err := s.UsageSeries(ctx, bad, "day", time.UTC); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("空窗口应当报 ErrInvalidQuery，得到 %v", err)
	}
}

func TestUsageBreakdownGroupsAndFoldsTheRest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	world := seedUsageWorld(ctx, t, db)
	s := New(db)

	window := usageWindow(t, world.userID)
	byModel, err := s.UsageBreakdown(ctx, window, "model", 2)
	if err != nil {
		t.Fatalf("按模型分组失败: %v", err)
	}
	if len(byModel.Items) != 2 {
		t.Fatalf("分组数 = %d，期望 2", len(byModel.Items))
	}
	// 金额降序：claude-3 (2.00) 在 gpt-4o (1.50) 前面。
	if byModel.Items[0].Model != "claude-3" ||
		byModel.Items[0].Cost != money.MustParse("2.00") ||
		byModel.Items[0].PlanReservedAmount != money.MustParse("4.00") {
		t.Errorf("Items[0] = %+v，期望 claude-3 / 2.00 / 面值 4.00", byModel.Items[0])
	}
	if byModel.Items[1].Model != "gpt-4o" ||
		byModel.Items[1].Cost != money.MustParse("1.50") ||
		byModel.Items[1].PlanReservedAmount != money.MustParse("4.50") ||
		byModel.Items[1].Requests != 2 {
		t.Errorf("Items[1] = %+v，期望 gpt-4o / 1.50 / 面值 4.50 / 2 次", byModel.Items[1])
	}
	// Other 折进 gpt-4o-mini 的两笔：req-E 有 cost 没面值，req-G 有面值没 cost。
	if byModel.Other.Model != "" || byModel.Other.Cost != money.MustParse("0.25") ||
		byModel.Other.PlanReservedAmount != money.MustParse("2.00") ||
		byModel.Other.Requests != 2 {
		t.Errorf("Other = %+v，期望 gpt-4o-mini 折进来的 0.25 / 面值 2.00 / 2 次", byModel.Other)
	}

	// 分组数不超过 limit 时 Other 是全零，而不是「第 limit 组」。
	//
	// 两条等价变异（gremlins）记在这里：
	//   - `len(groups) > limit` 的 `>` 改成 `>=`：相等那一格会去折
	//     `groups[limit:]`，那是空切片，sumGroups 返回零值组，与不折时
	//     out.Other 的零值逐字段相同。
	//   - `sort.Slice` 里 `c > 0` 改成 `c >= 0`：这一句在 `c != 0` 之后就
	//     进来了，相等那一格永远到不了。
	all, err := s.UsageBreakdown(ctx, window, "model", 10)
	if err != nil {
		t.Fatalf("按模型分组失败: %v", err)
	}
	if len(all.Items) != 3 || all.Other.Cost != money.Zero || all.Other.Requests != 0 ||
		all.Other.PlanReservedAmount != money.Zero {
		t.Errorf("未截断时 Items=%d Other=%+v", len(all.Items), all.Other)
	}

	// limit 为 0 取默认值，而不是「一组都不要」。变异：`if limit <= 0` 的
	// `<=` 改成 `<`，0 被当成有效值传给 SQL（LIMIT 1），结果 Items 为空、
	// 唯一一组被折进 Other。
	defaulted, err := s.UsageBreakdown(ctx, window, "model", 0)
	if err != nil {
		t.Fatalf("limit 为 0 时应当取默认值，得到 %v", err)
	}
	if len(defaulted.Items) != 3 {
		t.Errorf("limit 为 0 时 Items = %d 组，期望默认值下的 3 组", len(defaulted.Items))
	}
	// 上界那一处的 `>` 改成 `>=` 是等价变异：limit 正好等于上限时，两版
	// 都把它赋成同一个常量，用哪一版都一样。

	// 按 key：pending 的 req-B 没有事件，所以不出现；名字从 api_keys 左连回来。
	byKey, err := s.UsageBreakdown(ctx, window, "key", 10)
	if err != nil {
		t.Fatalf("按 key 分组失败: %v", err)
	}
	if len(byKey.Items) != 2 {
		t.Fatalf("按 key 分组数 = %d，期望 2", len(byKey.Items))
	}
	if byKey.Items[0].KeyID == nil || *byKey.Items[0].KeyID != world.key2 ||
		byKey.Items[0].KeyName != "第二把" ||
		byKey.Items[0].Cost != money.MustParse("2.00") ||
		byKey.Items[0].PlanReservedAmount != money.MustParse("4.00") {
		t.Errorf("Items[0] = %+v，期望 key2 / 第二把 / 2.00 / 面值 4.00", byKey.Items[0])
	}
	if byKey.Items[1].KeyID == nil || *byKey.Items[1].KeyID != world.key1 ||
		byKey.Items[1].KeyName != "第一把" ||
		byKey.Items[1].Cost != money.MustParse("1.75") ||
		byKey.Items[1].PlanReservedAmount != money.MustParse("6.50") {
		t.Errorf("Items[1] = %+v，期望 key1 / 第一把 / 1.75 / 面值 6.50", byKey.Items[1])
	}

	if _, err := s.UsageBreakdown(ctx, window, "user", 10); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("未知分组方式应当报 ErrInvalidQuery，得到 %v", err)
	}
}
