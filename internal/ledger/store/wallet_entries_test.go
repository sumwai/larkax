package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是控制台「余额变动」清单在真库上的用例。
//
// 最要紧的一条是科目归一化：旧写路径留下过 `user:0…` 这样的科目，清单按
// 规范写法比会漏掉它们，而余额里算着——两个地方各报一个数，且都不报错。

// insertLedgerEntry 直接写一条分录。
//
// 这些行在真实流程里由 ledger 包的写路径产生；这里逐列写是为了造出**写路径
// 今天不会产生、但库里确实存在**的形状（旧写法的科目），而那正是清单要能
// 读回来的东西。
func insertLedgerEntry(ctx context.Context, t *testing.T, db *sql.DB, account string,
	amount string, entryType ledger.EntryType, memo string, billingRequestID *int64,
	createdAt time.Time) {
	t.Helper()

	var requestID any
	if billingRequestID != nil {
		requestID = *billingRequestID
	}
	var memoValue any
	if memo != "" {
		memoValue = memo
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries
		       (txn_id, account, asset, amount, entry_type, memo,
		        billing_request_id, created_at)
		VALUES ($1, $2, 'CNY', $3::numeric, $4, $5, $6, $7)`,
		string(ledger.NewTxnID()), account, amount, string(entryType),
		memoValue, requestID, createdAt,
	); err != nil {
		t.Fatalf("写分录失败: %v", err)
	}
}

func TestUserEntriesScopesToCanonicalAccount(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	userID := seed(t, db, "100")
	channelID := seedChannel(t, ctx, db)
	keyID := seedAPIKey(ctx, t, db, userID)
	at := func(value string) time.Time { return usageTestTime(t, value) }

	billID := insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, KeyID: &keyID, ChannelID: channelID,
		RequestID: requestIDOf("wallet-bill"), Model: "gpt-4o",
		Status: BillingStatusSettled, Reserved: "1.50", Final: "1.50",
		StartedAt: at("2026-10-02T10:00:00Z"),
	})

	canonical := ledger.UserAccount(userID).String()
	// 旧写法：前导零。归一化之后它属于同一个科目。
	padded := "user:0" + canonical[len("user:"):]

	insertLedgerEntry(ctx, t, db, canonical, "-1.50", ledger.EntryConsume,
		"一次调用", &billID, at("2026-10-02T10:00:05Z"))
	insertLedgerEntry(ctx, t, db, canonical, "0.50", ledger.EntryRefund,
		"上游失败退款", nil, at("2026-10-03T11:00:00Z"))
	insertLedgerEntry(ctx, t, db, padded, "2.00", ledger.EntryAdjustment,
		"人工调账", nil, at("2026-10-04T12:00:00Z"))

	// 同一个窗口里、别的科目与别的用户的账：都不该出现。
	otherUser := seed(t, db, "100")
	insertLedgerEntry(ctx, t, db, ledger.UserAccount(otherUser).String(),
		"5.00", ledger.EntryTopup, "", nil, at("2026-10-05T09:00:00Z"))
	insertLedgerEntry(ctx, t, db, ledger.PlatformRevenueAccount().String(),
		"1.00", ledger.EntrySettle, "", nil, at("2026-10-05T10:00:00Z"))

	page, err := s.UserEntries(ctx, UserEntryQuery{
		UserID: userID,
		From:   at("2026-10-01T00:00:00Z"),
		To:     at("2026-10-10T00:00:00Z"),
	})
	if err != nil {
		t.Fatalf("UserEntries 失败: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("分录数 = %d，期望 3（充值在窗口外，别人与别的科目不算）: %+v",
			len(page.Entries), page.Entries)
	}

	want := []struct {
		amount string
		typ    ledger.EntryType
		memo   string
	}{
		{"2.00", ledger.EntryAdjustment, "人工调账"},
		{"0.50", ledger.EntryRefund, "上游失败退款"},
		{"-1.50", ledger.EntryConsume, "一次调用"},
	}
	for i, expected := range want {
		got := page.Entries[i]
		if got.Amount != money.MustParse(expected.amount) ||
			got.Type != expected.typ || got.Memo != expected.memo {
			t.Errorf("Entries[%d] = %+v，期望 %s / %s / %q",
				i, got, expected.amount, expected.typ, expected.memo)
		}
	}
	// 只有消费那一行挂着计费请求；人工调账没有。
	if page.Entries[2].BillingRequestID == nil ||
		*page.Entries[2].BillingRequestID != billID {
		t.Errorf("消费分录的 billing_request_id = %v，期望 %d",
			page.Entries[2].BillingRequestID, billID)
	}
	if page.Entries[0].BillingRequestID != nil {
		t.Errorf("调账分录不该挂计费请求，得到 %v", page.Entries[0].BillingRequestID)
	}

	// 类型筛选。
	refunds, err := s.UserEntries(ctx, UserEntryQuery{
		UserID: userID, Types: []string{string(ledger.EntryRefund)},
	})
	if err != nil {
		t.Fatalf("按类型筛选失败: %v", err)
	}
	if len(refunds.Entries) != 1 || refunds.Entries[0].Type != ledger.EntryRefund {
		t.Errorf("按 refund 筛选 = %+v", refunds.Entries)
	}
}

func TestUserEntriesWindowAndCursor(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	userID := seed(t, db, "100")
	account := ledger.UserAccount(userID).String()

	// 三条同一时刻的分录：游标必须靠 id 才能不漏行。
	same := usageTestTime(t, "2026-10-02T10:00:00Z")
	for i := 0; i < 3; i++ {
		insertLedgerEntry(ctx, t, db, account, "1.00", ledger.EntryTopup,
			"", nil, same)
	}
	insertLedgerEntry(ctx, t, db, account, "2.00", ledger.EntryTopup,
		"", nil, usageTestTime(t, "2026-10-03T10:00:00Z"))

	// 窗口左闭右开：10-03 那条不在 [10-01, 10-03) 里。
	windowed, err := s.UserEntries(ctx, UserEntryQuery{
		UserID: userID,
		From:   usageTestTime(t, "2026-10-01T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-03T00:00:00Z"),
	})
	if err != nil {
		t.Fatalf("UserEntries 失败: %v", err)
	}
	if len(windowed.Entries) != 3 {
		t.Fatalf("窗口内分录数 = %d，期望 3", len(windowed.Entries))
	}

	// 只给一边的时间窗是允许的，那一侧单独生效。变异：`!q.From.IsZero() &&
	// !q.To.IsZero() && …` 的 `&&` 改成 `||`，只给起点会被当成「窗口不合法」。
	// 这里只判不报错：筛选本身由上面那条左闭右开的断言覆盖。
	if _, err = s.UserEntries(ctx, UserEntryQuery{
		UserID: userID,
		From:   usageTestTime(t, "2026-10-01T00:00:00Z"),
	}); err != nil {
		t.Errorf("只给起点应当被接受，得到 %v", err)
	}

	// 分页的两格：给了 limit 就只给这么多行；行数正好等于 limit 时 More 是
	// false。变异分别是 `if limit > EntryPageMaxLimit` 的 `>` 改成 `<=`
	// （小 limit 被抬到上限，第一页把 4 行全给回来）与 `len(page.Entries) >
	// limit` 的 `>` 改成 `>=`（正好读完时误报还有下一页）。
	// 上界判据 `if limit > EntryPageMaxLimit` 的 `>` 改成 `>=` 是等价变异：
	// limit 正好等于 EntryPageMaxLimit 时两版赋的是同一个常量。
	paged := UserEntryQuery{
		UserID: userID,
		From:   usageTestTime(t, "2026-10-01T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-04T00:00:00Z"),
		Limit:  2,
	}
	first, err := s.UserEntries(ctx, paged)
	if err != nil {
		t.Fatalf("带 limit 的查询失败: %v", err)
	}
	if len(first.Entries) != 2 || !first.More {
		t.Errorf("limit=2 时得到 %d 行、More=%v，期望 2 行且还有下一页",
			len(first.Entries), first.More)
	}
	paged.Limit = 4
	exact, err := s.UserEntries(ctx, paged)
	if err != nil {
		t.Fatalf("limit 等于行数时查询失败: %v", err)
	}
	if len(exact.Entries) != 4 || exact.More {
		t.Errorf("limit=4 时得到 %d 行、More=%v，期望正好读完且没有下一页",
			len(exact.Entries), exact.More)
	}

	query := UserEntryQuery{
		UserID: userID,
		// 不带窗口时 seed 那笔充值的时刻（now）也会进来；这里只看本用例
		// 写的那四条。
		From:  usageTestTime(t, "2026-10-01T00:00:00Z"),
		To:    usageTestTime(t, "2026-10-04T00:00:00Z"),
		Limit: 2,
	}
	var walked []int64
	for page := 0; ; page++ {
		if page > 5 {
			t.Fatal("翻页没有终止")
		}
		result, err := s.UserEntries(ctx, query)
		if err != nil {
			t.Fatalf("第 %d 页失败: %v", page, err)
		}
		for _, entry := range result.Entries {
			walked = append(walked, entry.ID)
		}
		if !result.More {
			break
		}
		last := result.Entries[len(result.Entries)-1]
		query.Cursor = &EntryCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if len(walked) != 4 {
		t.Fatalf("翻完得到 %d 行，期望 4: %v", len(walked), walked)
	}
	seen := make(map[int64]bool, len(walked))
	for _, id := range walked {
		if seen[id] {
			t.Errorf("分录 %d 出现了两次", id)
		}
		seen[id] = true
	}
}

func TestUserEntriesRejectsBadQueries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	if _, err := s.UserEntries(ctx, UserEntryQuery{UserID: 0}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("用户 ID 为 0 应当报 ErrInvalidQuery，得到 %v", err)
	}
	if _, err := s.UserEntries(ctx, UserEntryQuery{
		UserID: 1, Types: []string{"settle"},
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("非钱包分录类型应当报 ErrInvalidQuery，得到 %v", err)
	}
	if _, err := s.UserEntries(ctx, UserEntryQuery{
		UserID: 1,
		From:   usageTestTime(t, "2026-10-03T00:00:00Z"),
		To:     usageTestTime(t, "2026-10-01T00:00:00Z"),
	}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("倒置的时间窗应当报 ErrInvalidQuery，得到 %v", err)
	}
}

func TestWalletsCarriesUpdatedAt(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)
	userID := seed(t, db, "100")

	wallets, err := s.Wallets(ctx, &userID)
	if err != nil {
		t.Fatalf("Wallets 失败: %v", err)
	}
	if len(wallets) != 1 {
		t.Fatalf("钱包数 = %d，期望 1", len(wallets))
	}
	if wallets[0].UpdatedAt.IsZero() {
		t.Error("UpdatedAt 是零值：控制台会把「余额最后变动时刻」画成公元 1 年")
	}
	if wallets[0].Balance != money.MustParse("100") {
		t.Errorf("余额 = %s，期望 100", wallets[0].Balance)
	}
}
