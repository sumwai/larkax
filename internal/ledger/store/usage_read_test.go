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
