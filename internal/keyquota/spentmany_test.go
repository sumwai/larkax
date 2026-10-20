package keyquota

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// TestSpentManyMatchesSpent 钉住批量读与单条读是同一个口径。
//
// 两者各写一份 SQL 时最容易出现的偏差是「批量那份忘了 CASE 的某一支」——
// 它不会让任何调用方报错，只会让清单里的累计花费与额度判定用的数不一样，
// 而界面上那两栏本来就该是同一个数。
func TestSpentManyMatchesSpent(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	userID := seedUser(ctx, t, db)
	key1 := seedKey(ctx, t, db, userID, "k1")
	key2 := seedKey(ctx, t, db, userID, "k2")
	key3 := seedKey(ctx, t, db, userID, "k3")
	key4 := seedKey(ctx, t, db, userID, "k4") // 一笔都没有

	// key1：只有 settled 的最终额与 pending 的预留额算数，另外三态是 0。
	insertBill(ctx, t, db, userID, &key1, "settled", "2.00", "1.50")
	insertBill(ctx, t, db, userID, &key1, "pending", "0.50", "")
	insertBill(ctx, t, db, userID, &key1, "released", "3.00", "")
	insertBill(ctx, t, db, userID, &key1, "failed", "4.00", "")
	insertBill(ctx, t, db, userID, &key1, "refunded", "5.00", "0.10")

	// key2：8 位小数的两个数相加，证明走的是十进制文本而不是浮点。
	insertBill(ctx, t, db, userID, &key2, "settled", "0.00000002", "0.00000001")
	insertBill(ctx, t, db, userID, &key2, "pending", "0.00000002", "")

	// key3：全零金额也要算成 0，而不是从 map 里缺席。
	insertBill(ctx, t, db, userID, &key3, "settled", "0", "0")

	// 不属于任何 key 的历史行：任何一把 key 的累计里都不该有它。
	insertBill(ctx, t, db, userID, nil, "settled", "100.00", "100.00")

	got, err := SpentMany(ctx, db, []int64{key1, key2, key3, key4})
	if err != nil {
		t.Fatalf("SpentMany 失败: %v", err)
	}
	want := map[int64]string{
		key1: "2.00",       // 1.50 + 0.50
		key2: "0.00000003", // 0.00000001 + 0.00000002
		key3: "0",
		key4: "0",
	}
	if len(got) != len(want) {
		t.Fatalf("返回 %d 项，期望 %d 项（每个传入的 key 都要有一项）: %+v",
			len(got), len(want), got)
	}
	for id, expected := range want {
		amount, ok := got[id]
		if !ok {
			t.Errorf("key %d 在返回的 map 里缺席", id)
			continue
		}
		if amount != money.MustParse(expected) {
			t.Errorf("key %d 的累计 = %s，期望 %s", id, amount, expected)
		}
	}

	// 逐把与单条读对拍：两个函数回答的是同一个问题。
	for _, id := range []int64{key1, key2, key3, key4} {
		one, err := Spent(ctx, db, id)
		if err != nil {
			t.Fatalf("Spent(%d) 失败: %v", id, err)
		}
		if one != got[id] {
			t.Errorf("key %d：Spent = %s，SpentMany = %s", id, one, got[id])
		}
	}
}

func TestSpentManyEmptySliceSkipsTheDatabase(t *testing.T) {
	// 传一个 nil 的 Querier：这一条要证明的是「空切片不会走到查询」，
	// 而不是「空切片查出来是空」。走到查询会在这里 nil 解引用并崩掉。
	got, err := SpentMany(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("空切片不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空切片返回了 %d 项", len(got))
	}
}

// seedUser 建一行用户（billing_requests.user_id 的外键）。
func seedUser(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO users (username, email) VALUES ('spent-many', 'spent-many@example.com')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// seedKey 建一把 key，只要 id——这里的 key 从不用于鉴权。
func seedKey(ctx context.Context, t *testing.T, db *sql.DB, userID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id, name, key_hash, key_prefix)
		VALUES ($1, $2, $3, 'placeholder')
		RETURNING id`, userID, name, "hash-"+name).Scan(&id); err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	return id
}

// insertBill 写一条账单行。final 为空串表示 NULL（pending 与失败态本来就没有）。
func insertBill(ctx context.Context, t *testing.T, db *sql.DB, userID int64,
	keyID *int64, status, reserved, final string) {
	t.Helper()

	var (
		key        any
		finalValue any
	)
	if keyID != nil {
		key = *keyID
	}
	if final != "" {
		finalValue = final
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO billing_requests
		       (request_id, user_id, api_key_id, canonical_model, protocol,
		        reserved_amount, status, final_amount)
		VALUES (gen_random_uuid()::text, $1, $2, 'gpt-4o', 'openai',
		        $3::numeric, $4, $5::numeric)`,
		userID, key, reserved, status, finalValue,
	); err != nil {
		t.Fatalf("写账单失败: %v", err)
	}
}
