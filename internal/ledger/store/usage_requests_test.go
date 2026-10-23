package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是控制台请求日志在真库上的用例。
//
// 它要钉住的两条都只在 SQL 里成立：LATERAL 只取**最新**一条事件（一次重试
// 落两行时不能相加），以及金额按状态取列（pending 取预留额，其余非结算状态
// 是 0）。拿 Go 侧拼出来的行测不到这两条。

func TestUsageRequestsLatestEventAndCostByStatus(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	userID := seed(t, db, "100")
	channelID := seedChannel(t, ctx, db)
	key1 := seedAPIKey(ctx, t, db, userID)
	key2 := seedAPIKey(ctx, t, db, userID)

	at := func(value string) time.Time { return usageTestTime(t, value) }

	settledID := requestIDOf("log-settled")
	pendingID := requestIDOf("log-pending")
	releasedID := requestIDOf("log-released")
	failedID := requestIDOf("log-failed")
	refundedID := requestIDOf("log-refunded")
	outsideID := requestIDOf("log-outside")
	otherID := requestIDOf("log-other")

	settledAt := at("2026-10-02T10:00:05Z")
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, KeyID: &key1, ChannelID: channelID,
		RequestID: settledID, Model: "gpt-4o", Status: BillingStatusSettled,
		Reserved: "2.00", Final: "1.50", Streamed: true,
		StartedAt: at("2026-10-02T10:00:00Z"), SettledAt: &settledAt,
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, KeyID: &key1, ChannelID: channelID,
		RequestID: pendingID, Model: "gpt-4o", Status: BillingStatusPending,
		Reserved: "0.50", StartedAt: at("2026-10-03T12:00:00Z"),
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, KeyID: &key2, ChannelID: channelID,
		RequestID: releasedID, Model: "gpt-4o", Status: BillingStatusReleased,
		Reserved: "3.00", StartedAt: at("2026-10-04T09:00:00Z"),
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, KeyID: &key2, ChannelID: channelID,
		RequestID: failedID, Model: "gpt-4o", Status: BillingStatusFailed,
		Reserved: "4.00", StartedAt: at("2026-10-05T09:00:00Z"),
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, KeyID: &key1, ChannelID: channelID,
		RequestID: refundedID, Model: "gpt-4o", Status: BillingStatusRefunded,
		Reserved: "5.00", Final: "0.20", StartedAt: at("2026-10-06T09:00:00Z"),
	})
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, KeyID: &key1, ChannelID: channelID,
		RequestID: outsideID, Model: "gpt-4o", Status: BillingStatusSettled,
		Reserved: "9.00", Final: "9.00", StartedAt: at("2026-11-02T10:00:00Z"),
	})
	otherUser := seed(t, db, "100")
	otherKey := seedAPIKey(ctx, t, db, otherUser)
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: otherUser, KeyID: &otherKey, ChannelID: channelID,
		RequestID: otherID, Model: "gpt-4o", Status: BillingStatusSettled,
		Reserved: "100.00", Final: "100.00",
		StartedAt: at("2026-10-02T11:00:00Z"),
	})

	duration900, duration950 := 900, 950
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: settledID, UserID: userID, ChannelID: channelID,
		Model: "gpt-4o", PromptTokens: 100, CompletionTokens: 10,
		Path: "measured", Cost: "0.50", DurationMS: &duration900,
		CreatedAt: at("2026-10-02T10:00:10Z"),
	})
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: settledID, UserID: userID, ChannelID: channelID,
		Model: "gpt-4o", PromptTokens: 200, CompletionTokens: 60,
		Path: "estimated", Cost: "1.00", DurationMS: &duration950,
		CreatedAt: at("2026-10-02T10:00:20Z"),
	})
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: releasedID, UserID: userID, ChannelID: channelID,
		Model: "gpt-4o", PromptTokens: 70, Path: "estimated", Cost: "0.00",
		CreatedAt: at("2026-10-04T09:00:10Z"),
	})

	window := usageWindow(t, userID)
	page, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To,
	})
	if err != nil {
		t.Fatalf("UsageRequests 失败: %v", err)
	}

	wantOrder := []string{refundedID, failedID, releasedID, pendingID, settledID}
	if len(page.Requests) != len(wantOrder) {
		t.Fatalf("行数 = %d，期望 %d（窗口外与别人的行都不该出现）: %+v",
			len(page.Requests), len(wantOrder), page.Requests)
	}
	for i, want := range wantOrder {
		if page.Requests[i].RequestID != want {
			t.Fatalf("第 %d 行是 %s，期望 %s（按 started_at DESC）",
				i, page.Requests[i].RequestID, want)
		}
	}
	if page.More {
		t.Error("恰好读完时 More 应当为 false")
	}

	// 分页的两格都必须在真库上钉住：
	//   - 给了 limit 就不能把整页都给回来。变异：`if limit > RequestPageMaxLimit`
	//     的 `>` 改成 `<=`，小 limit 会被抬到上限 200，第一页就把 5 行全返回。
	//   - 行数正好等于 limit 时 More 是 false。变异：`len(page.Requests) > limit`
	//     的 `>` 改成 `>=`，那一格会误报「还有下一页」。
	// 上界判据 `if limit > RequestPageMaxLimit` 的 `>` 改成 `>=` 是等价变异：
	// limit 正好等于 RequestPageMaxLimit 时两版赋的是同一个常量。
	half, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To, Limit: 2,
	})
	if err != nil {
		t.Fatalf("带 limit 的查询失败: %v", err)
	}
	if len(half.Requests) != 2 || !half.More {
		t.Errorf("limit=2 时得到 %d 行、More=%v，期望 2 行且还有下一页",
			len(half.Requests), half.More)
	}
	exact, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To, Limit: len(wantOrder),
	})
	if err != nil {
		t.Fatalf("limit 等于行数时查询失败: %v", err)
	}
	if len(exact.Requests) != len(wantOrder) || exact.More {
		t.Errorf("limit=%d 时得到 %d 行、More=%v，期望正好读完且没有下一页",
			len(wantOrder), len(exact.Requests), exact.More)
	}

	settled := page.Requests[4]
	// 两行事件里只取最新那行：200 / 60（10:00:20），而不是旧行的 100 / 10。
	if settled.Cost != money.MustParse("1.50") {
		t.Errorf("settled 金额 = %s，期望最终额 1.50（不是预留额 2.00）", settled.Cost)
	}
	if settled.PromptTokens != 200 || settled.CompletionTokens != 60 {
		t.Errorf("settled tokens = %d / %d，期望最新事件的 200 / 60",
			settled.PromptTokens, settled.CompletionTokens)
	}
	if settled.BillingPath == nil || *settled.BillingPath != "estimated" {
		t.Errorf("settled 计费路径 = %v，期望最新事件的 estimated", settled.BillingPath)
	}
	if settled.DurationMS == nil || *settled.DurationMS != 950 {
		t.Errorf("settled 时长 = %v，期望最新事件的 950", settled.DurationMS)
	}
	if settled.KeyID == nil || *settled.KeyID != key1 {
		t.Errorf("settled 的 key = %v，期望 %d", settled.KeyID, key1)
	}
	if settled.SettledAt == nil || !settled.SettledAt.Equal(settledAt) {
		t.Errorf("settled_at = %v，期望 %s", settled.SettledAt, settledAt)
	}
	if !settled.Streamed || settled.Protocol != "openai" {
		t.Errorf("streamed / protocol = %v / %q", settled.Streamed, settled.Protocol)
	}

	pending := page.Requests[3]
	if pending.Cost != money.MustParse("0.50") {
		t.Errorf("pending 金额 = %s，期望预留额 0.50", pending.Cost)
	}
	if pending.BillingPath != nil || pending.DurationMS != nil ||
		pending.PromptTokens != 0 || pending.SettledAt != nil {
		t.Errorf("没有事件的 pending 行应当全空: %+v", pending)
	}

	// 已释放 / 失败 / 已退款都没有从用户身上扣钱：金额为 0，不是预留额。
	for _, i := range []int{0, 1, 2} {
		if page.Requests[i].Cost != money.Zero {
			t.Errorf("%s 的金额 = %s，期望 0",
				page.Requests[i].Status, page.Requests[i].Cost)
		}
	}

	// released 的事件仍然展示出来：它解释「那一笔是怎么计量的」，
	// 只是这一笔没有向用户收钱。
	if page.Requests[2].PromptTokens != 70 ||
		page.Requests[2].BillingPath == nil ||
		*page.Requests[2].BillingPath != "estimated" {
		t.Errorf("released 行的事件 = %+v", page.Requests[2])
	}

	// 筛选：状态、key、模型。
	byStatus, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To,
		Status: BillingStatusPending,
	})
	if err != nil {
		t.Fatalf("按状态筛选失败: %v", err)
	}
	if len(byStatus.Requests) != 1 || byStatus.Requests[0].RequestID != pendingID {
		t.Errorf("按 pending 筛选 = %+v", byStatus.Requests)
	}

	byKey, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To,
		APIKeyID: &key2,
	})
	if err != nil {
		t.Fatalf("按 key 筛选失败: %v", err)
	}
	if len(byKey.Requests) != 2 {
		t.Errorf("key2 的行数 = %d，期望 2", len(byKey.Requests))
	}

	byModel, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To,
		Model: "gpt-4o",
	})
	if err != nil {
		t.Fatalf("按模型筛选失败: %v", err)
	}
	if len(byModel.Requests) != 5 {
		t.Errorf("gpt-4o 的行数 = %d，期望 5", len(byModel.Requests))
	}
}

func TestUsageRequestsCursorWalksAllRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	userID := seed(t, db, "100")
	channelID := seedChannel(t, ctx, db)
	keyID := seedAPIKey(ctx, t, db, userID)

	// 前两行**同一个 started_at**：游标必须带上 id，否则第二页会把同一时刻
	// 的另一行整批漏掉。
	timestamps := []string{
		"2026-10-02T10:00:00Z",
		"2026-10-02T10:00:00Z",
		"2026-10-02T09:00:00Z",
		"2026-10-02T08:00:00Z",
		"2026-10-02T07:00:00Z",
	}
	ids := make([]string, 0, len(timestamps))
	for _, stamp := range timestamps {
		id := requestIDOf("log-page")
		ids = append(ids, id)
		insertBillingRequest(ctx, t, db, billingFixture{
			UserID: userID, KeyID: &keyID, ChannelID: channelID,
			RequestID: id, Model: "gpt-4o", Status: BillingStatusSettled,
			Reserved: "1.00", Final: "1.00",
			StartedAt: usageTestTime(t, stamp),
		})
	}

	window := usageWindow(t, userID)
	query := UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To, Limit: 2,
	}

	var walked []string
	for page := 0; ; page++ {
		if page > 5 {
			t.Fatal("翻页没有终止")
		}
		result, err := s.UsageRequests(ctx, query)
		if err != nil {
			t.Fatalf("第 %d 页失败: %v", page, err)
		}
		for _, row := range result.Requests {
			walked = append(walked, row.RequestID)
		}
		if !result.More {
			break
		}
		last := result.Requests[len(result.Requests)-1]
		query.Cursor = &RequestCursor{StartedAt: last.StartedAt, ID: last.ID}
	}

	if len(walked) != len(ids) {
		t.Fatalf("翻完得到 %d 行，期望 %d: %v", len(walked), len(ids), walked)
	}
	seen := make(map[string]bool, len(walked))
	for _, id := range walked {
		if seen[id] {
			t.Errorf("行 %s 出现了两次", id)
		}
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("行 %s 没被翻到", id)
		}
	}
	// 同一时刻的两行按 id DESC：后插入的那行在前。
	if walked[0] != ids[1] || walked[1] != ids[0] {
		t.Errorf("同一时刻的次序 = %v，期望 [%s %s]", walked[:2], ids[1], ids[0])
	}
}

func TestUsageRequestsRejectsBadQueries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	window := usageWindow(t, 1)
	if _, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: 0, From: window.From, To: window.To,
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("用户 ID 为 0 应当报 ErrInvalidQuery，得到 %v", err)
	}
	if _, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: 1, From: window.To, To: window.From,
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("倒置的窗口应当报 ErrInvalidQuery，得到 %v", err)
	}
	// 只给一边同样不合法：缺的那一边会变成零值时刻（公元 1 年），窗口从
	// 「有时间以来」开始。变异：`From.IsZero() || To.IsZero() || …` 的 `||`
	// 改成 `&&`，只缺一边时这一句不再报错。
	if _, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: 1, From: window.From,
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("只给起点应当报 ErrInvalidQuery，得到 %v", err)
	}
	if _, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: 1, To: window.To,
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("只给终点应当报 ErrInvalidQuery，得到 %v", err)
	}
	// 未知状态是**参数写错**，不是库不可用：调用方要能把它翻成 400。
	if _, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: 1, From: window.From, To: window.To, Status: "unknown",
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("未知状态应当报 ErrInvalidQuery，得到 %v", err)
	}
}

// TestUsageRequestsShowsPlanReservedAlongsideWallet 钉住「钱包一分没冻、额度包
// 占着面值」这一格：两笔预留是并排的两个数，成本仍然只算钱包那一部分。
//
// 判据放在真库上，因为这一格由 SELECT 里的两列决定：少读 plan_reserved_amount、
// 或者把它并进 reserved_amount，下面的断言都会红。
func TestUsageRequestsShowsPlanReservedAlongsideWallet(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	userID := seed(t, db, "100")
	at := func(value string) time.Time { return usageTestTime(t, value) }

	plannedID := requestIDOf("log-plan")
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, RequestID: plannedID, Model: "gpt-4o",
		Status: BillingStatusPending, Reserved: "0", PlanReserved: "2.50",
		StartedAt: at("2026-10-07T09:00:00Z"),
	})

	window := usageWindow(t, userID)
	page, err := s.UsageRequests(ctx, UsageRequestQuery{
		UserID: userID, From: window.From, To: window.To,
	})
	if err != nil {
		t.Fatalf("UsageRequests 失败: %v", err)
	}
	if len(page.Requests) != 1 {
		t.Fatalf("行数 = %d，期望 1: %+v", len(page.Requests), page.Requests)
	}

	row := page.Requests[0]
	if got := row.ReservedAmount.String(); got != "0.00000000" {
		t.Errorf("钱包预留 = %s，额度包覆盖的请求在这一列应当是 0", got)
	}
	if got := row.PlanReservedAmount.String(); got != "2.50000000" {
		t.Errorf("额度包预留面值 = %s，期望库里的 2.50000000", got)
	}
	// 成本仍是钱包那一部分：包的面值不是「从钱包花了多少钱」。
	if got := row.Cost.String(); got != "0.00000000" {
		t.Errorf("pending 的成本 = %s，期望 0：额度包覆盖不等于从钱包扣了钱", got)
	}
}
