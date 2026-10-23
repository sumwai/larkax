package store

import (
	"context"
	"testing"
	"time"
)

// 本文件是命令行计费请求清单（usage_read.go 的 BillingRequests）在真库上的用例。
//
// 它要钉住的是投影里那两列：额度包覆盖的请求在 reserved_amount 上是 0，占着的
// 面值在 plan_reserved_amount 上。少读后一列时，看清单的人会把「钱包没冻钱」
// 读成「什么也没占」。
func TestBillingRequestsReadsBothReservationColumns(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	userID := seed(t, db, "100")
	at := func(value string) time.Time { return usageTestTime(t, value) }

	plannedID := requestIDOf("read-plan")
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, RequestID: plannedID, Model: "gpt-4o",
		Status: BillingStatusPending, Reserved: "0", PlanReserved: "3.25",
		StartedAt: at("2026-10-07T09:00:00Z"),
	})
	walletID := requestIDOf("read-wallet")
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, RequestID: walletID, Model: "gpt-4o",
		Status: BillingStatusPending, Reserved: "1.50",
		StartedAt: at("2026-10-07T09:01:00Z"),
	})

	page, err := s.BillingRequests(ctx, BillingRequestFilter{
		UserID: &userID,
		From:   at("2026-10-01T00:00:00Z"),
		To:     at("2026-10-08T00:00:00Z"),
	})
	if err != nil {
		t.Fatalf("BillingRequests 失败: %v", err)
	}
	if len(page.Requests) != 2 {
		t.Fatalf("行数 = %d，期望 2: %+v", len(page.Requests), page.Requests)
	}

	byID := make(map[string]BillingRequestRow, len(page.Requests))
	for _, row := range page.Requests {
		byID[row.RequestID] = row
	}
	planned, ok := byID[plannedID]
	if !ok {
		t.Fatalf("清单里没有额度包覆盖的请求 %s: %+v", plannedID, page.Requests)
	}
	if planned.ReservedAmount.String() != "0.00000000" ||
		planned.PlanReservedAmount.String() != "3.25000000" {
		t.Errorf("额度包行读到 钱包=%s 包=%s，期望 0 与 3.25000000",
			planned.ReservedAmount, planned.PlanReservedAmount)
	}
	wallet, ok := byID[walletID]
	if !ok {
		t.Fatalf("清单里没有钱包预留的请求 %s: %+v", walletID, page.Requests)
	}
	if wallet.ReservedAmount.String() != "1.50000000" ||
		wallet.PlanReservedAmount.String() != "0.00000000" {
		t.Errorf("钱包行读到 钱包=%s 包=%s，期望 1.50000000 与 0",
			wallet.ReservedAmount, wallet.PlanReservedAmount)
	}
}

// 不给任何过滤条件时不能拼出空 WHERE 子句。
//
// 过滤条件是逐个追加到 `conds` 里的，`len(conds) > 0` 这一句决定拼不拼
// `WHERE`。变异：`>` 改成 `>=`，没有条件时也拼，SQL 变成
// `... FROM billing_requests WHERE  ORDER BY ...`——真库直接报语法错。
//
// 这条**必须在真库上跑**：它判的是拼出来的 SQL 能不能执行，而不是某个
// 返回值。假库只会把「我以为 Postgres 会怎么读」测一遍。
//
// 上面那条用例带了 UserID，碰不到空条件这一格，所以这里单独写一条。
func TestBillingRequestsWithoutFiltersBuildsNoWhereClause(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100")
	insertBillingRequest(ctx, t, db, billingFixture{
		UserID: userID, RequestID: requestIDOf("read-nofilter"), Model: "gpt-4o",
		Status: BillingStatusPending, Reserved: "1.00",
		StartedAt: usageTestTime(t, "2026-10-07T09:00:00Z"),
	})

	page, err := New(db).BillingRequests(ctx, BillingRequestFilter{})
	if err != nil {
		t.Fatalf("不带过滤条件查询失败: %v", err)
	}
	// 不限条数时整张表都在，至少刚写的那一条要看得到。
	if len(page.Requests) == 0 {
		t.Fatalf("不带过滤条件时读到 0 行，期望至少刚写的那一条")
	}
	if page.More {
		t.Errorf("没有给 limit 时 More = true，期望 false")
	}
}

// 行数正好等于 limit 时不算「还有下一页」。
//
// 多读一行（SQL 里 `LIMIT limit+1`）就是为了分开「刚好读完」与「恰好被截住」：
// 这两个状态在「打出来的条数等于上限」上长得一样。变异：末判据的
// `len(page.Requests) > f.Limit` 改成 `>=` 或 `<=`，前一格会误报 More。
//
// usage_requests.go 与 wallet_entries.go 各有一份同形判据，那两份在各自的
// 用例里已经钉过（见 usage_requests_test.go、wallet_entries_test.go 的分页两格）；
// 这一份是第三条查询，之前只有「有 limit / 没 limit」没有「正好等于」。
func TestBillingRequestsExactLimitHasNoNextPage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seed(t, db, "100")

	ids := []string{"read-exact-a", "read-exact-b"}
	times := []string{"2026-10-07T09:00:00Z", "2026-10-07T09:01:00Z"}
	for i, id := range ids {
		insertBillingRequest(ctx, t, db, billingFixture{
			UserID: userID, RequestID: requestIDOf(id), Model: "gpt-4o",
			Status: BillingStatusPending, Reserved: "1.00",
			StartedAt: usageTestTime(t, times[i]),
		})
	}

	exact, err := New(db).BillingRequests(ctx, BillingRequestFilter{UserID: &userID, Limit: len(ids)})
	if err != nil {
		t.Fatalf("limit 等于行数时查询失败: %v", err)
	}
	if len(exact.Requests) != len(ids) || exact.More {
		t.Errorf("limit=%d 时得到 %d 行、More=%v，期望正好读完且没有下一页",
			len(ids), len(exact.Requests), exact.More)
	}

	// 反过来那一格：limit 比行数少一行时确实还有下一页，别把判据写成恒 false。
	half, err := New(db).BillingRequests(ctx, BillingRequestFilter{UserID: &userID, Limit: len(ids) - 1})
	if err != nil {
		t.Fatalf("limit 小于行数时查询失败: %v", err)
	}
	if len(half.Requests) != len(ids)-1 || !half.More {
		t.Errorf("limit=%d 时得到 %d 行、More=%v，期望 %d 行且还有下一页",
			len(ids)-1, len(half.Requests), half.More, len(ids)-1)
	}
}
