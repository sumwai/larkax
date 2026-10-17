package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

// readLimits 直接查库读回一把 key 的两列额度。
//
// 走 SQL 而不是走本包的读路径：`quota_limit` 目前**没有读侧**（见 #175），
// 而这一层的落点正是「列里到底存了什么」——把读法也接上本包的代码，
// 两边一起错的时候这个用例会一起通过。
//
// quota_limit 显式转成 text：列是 numeric，Go 这一侧没有能直接扫进去的
// 驱动类型，而 ::text 的输出（12.34000000）恰好把标度也钉住了。
func readLimits(ctx context.Context, t *testing.T, db *sql.DB, keyID int64) (sql.NullInt64, sql.NullString) {
	t.Helper()

	var (
		rpm   sql.NullInt64
		quota sql.NullString
	)
	if err := db.QueryRowContext(ctx,
		`SELECT rate_limit_rpm, quota_limit::text FROM api_keys WHERE id = $1`, keyID,
	).Scan(&rpm, &quota); err != nil {
		t.Fatalf("读回额度失败: %v", err)
	}
	return rpm, quota
}

// assertLimits 断言两列的具体取值。want 用字符串写，NULL 写成 nil。
func assertLimits(ctx context.Context, t *testing.T, db *sql.DB, keyID int64, wantRPM, wantQuota *string) {
	t.Helper()

	rpm, quota := readLimits(ctx, t, db, keyID)

	gotRPM := "NULL"
	if rpm.Valid {
		gotRPM = strconv.FormatInt(rpm.Int64, 10)
	}
	if wantRPM == nil {
		if rpm.Valid {
			t.Errorf("rate_limit_rpm = %s，期望 NULL", gotRPM)
		}
	} else if !rpm.Valid || gotRPM != *wantRPM {
		t.Errorf("rate_limit_rpm = %s，期望 %s", gotRPM, *wantRPM)
	}

	gotQuota := "NULL"
	if quota.Valid {
		gotQuota = quota.String
	}
	if wantQuota == nil {
		if quota.Valid {
			t.Errorf("quota_limit = %s，期望 NULL", gotQuota)
		}
	} else if !quota.Valid || gotQuota != *wantQuota {
		t.Errorf("quota_limit = %s，期望 %s", gotQuota, *wantQuota)
	}
}

func strPtr(s string) *string { return &s }

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

// TestCreateWritesLimits 是 #175 的落点：建 key 时把两列真的写进去。
//
// 断言的是列里的具体值，而不是「不为空」——把 12.34 写成 1234000000
// （误把最小单位数当金额存）在「不为空」的断言下完全看不出来，而在对账时
// 它是一笔差八个数量级的账。
func TestCreateWritesLimits(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	owner := newUser(ctx, t, db, userStatusActive)
	rpm := 60
	quota := money.MustParse("12.34")

	created, err := store.Create(ctx, CreateParams{
		UserID:       owner.ID,
		Name:         "带额度",
		RateLimitRPM: &rpm,
		QuotaLimit:   &quota,
	})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	assertLimits(ctx, t, db, created.ID, strPtr("60"), strPtr("12.34000000"))
}

// 不填额度时两列必须是 NULL，而不是 0。
//
// NULL 与 0 在限流那一侧的意思正相反（不限 / 一个都不放行），而这个区别
// 只在真库上存在：内存实现里没有 NULL 这个东西，两者会合并成一个零值。
func TestCreateLeavesLimitsNullWhenNotGiven(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, _ := createKey(ctx, t, db, store)

	assertLimits(ctx, t, db, created.ID, nil, nil)
}

// TestUpdateLimitsSetsAndClears 钉住三态：设值 / 设成 0 / 清空。
//
// 三个状态两两之间都必须分得开，尤其是**清空与设成 0**：前者是不限，
// 后者是一个都不放行。把清空实现成「写成 0」会让每一次「恢复默认」的动作
// 变成一次静默的封禁，而它不会报任何错。
func TestUpdateLimitsSetsAndClears(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, _ := createKey(ctx, t, db, store)

	// 1. 两项一起设上。
	rpm := 120
	quota := money.MustParse("5")
	if err := store.UpdateLimits(ctx, created.ID, LimitParams{
		SetRateLimit: true, RateLimitRPM: &rpm,
		SetQuota: true, QuotaLimit: &quota,
	}); err != nil {
		t.Fatalf("设额度失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, strPtr("120"), strPtr("5.00000000"))

	// 2. 只改 rpm，quota 必须原样不动。
	zero := 0
	if err := store.UpdateLimits(ctx, created.ID, LimitParams{
		SetRateLimit: true, RateLimitRPM: &zero,
	}); err != nil {
		t.Fatalf("把 rpm 设成 0 失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, strPtr("0"), strPtr("5.00000000"))

	// 3. 只清空 quota，rpm 必须原样不动——而且它还是 0，不是 NULL。
	if err := store.UpdateLimits(ctx, created.ID, LimitParams{
		SetQuota: true, QuotaLimit: nil,
	}); err != nil {
		t.Fatalf("清空 quota 失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, strPtr("0"), nil)

	// 4. 再清空 rpm。
	if err := store.UpdateLimits(ctx, created.ID, LimitParams{
		SetRateLimit: true, RateLimitRPM: nil,
	}); err != nil {
		t.Fatalf("清空 rpm 失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, nil, nil)
}

// 改一把不存在的 key 要报错，而不是安静地什么都不做。
//
// 静默的成功在这里格外危险：运维以为自己刚给某把 key 上了额度，
// 而实际上那条命令什么都没碰到。
func TestUpdateLimitsRejectsMissingKey(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	rpm := 60
	err := store.UpdateLimits(ctx, 1<<62, LimitParams{SetRateLimit: true, RateLimitRPM: &rpm})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "找不到") {
		t.Errorf("错误信息 %q 没说清是找不到这把 key", err.Error())
	}
}

// 什么都没要求改时必须报错。
//
// 零值意味着「不改」，所以一条不带任何额度的 update 是一条什么都没做的
// 命令；让它安静地成功，运维会以为改生效了。
func TestUpdateLimitsRequiresAChange(t *testing.T) {
	store := New(nil) // 这条检查在连库之前就该发生

	err := store.UpdateLimits(context.Background(), 7, LimitParams{})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "至少要给一个") {
		t.Errorf("错误信息 %q 没说清缺什么", err.Error())
	}
}

// TestListKeys 钉住列表里每一栏的取值，尤其是两列额度的 NULL 与值。
func TestListKeys(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	owner := newUser(ctx, t, db, userStatusActive)
	rpm := 30
	quota := money.MustParse("1.5")

	first, err := store.Create(ctx, CreateParams{
		UserID: owner.ID, Name: "限额的",
		RateLimitRPM: &rpm, QuotaLimit: &quota,
	})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	second, err := store.Create(ctx, CreateParams{UserID: owner.ID, Name: "不限额的"})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	keys, err := store.ListKeys(ctx, &owner.ID)
	if err != nil {
		t.Fatalf("列 key 失败: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("列出 %d 把 key，期望 2", len(keys))
	}

	byID := map[int64]Key{}
	for _, k := range keys {
		byID[k.ID] = k
	}

	got, ok := byID[first.ID]
	if !ok {
		t.Fatalf("列表里没有 id=%d", first.ID)
	}
	if got.Name != "限额的" || got.Username != owner.Username || got.Status != keyStatusActive {
		t.Errorf("第一把 key 的栏位不对: name=%q username=%q status=%q",
			got.Name, got.Username, got.Status)
	}
	if got.Prefix != first.Prefix {
		t.Errorf("prefix = %q，期望 %q", got.Prefix, first.Prefix)
	}
	if got.RateLimitRPM == nil || *got.RateLimitRPM != 30 {
		t.Errorf("rate_limit_rpm = %s，期望 30", showPtr(got.RateLimitRPM))
	}
	if got.QuotaLimit == nil || got.QuotaLimit.String() != "1.50000000" {
		t.Errorf("quota_limit = %v，期望 1.50000000", got.QuotaLimit)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v，期望 nil", got.ExpiresAt)
	}
	if got.LastUsedAt != nil {
		t.Errorf("last_used_at = %v，期望 nil（还没用过）", got.LastUsedAt)
	}

	plain, ok := byID[second.ID]
	if !ok {
		t.Fatalf("列表里没有 id=%d", second.ID)
	}
	if plain.RateLimitRPM != nil || plain.QuotaLimit != nil {
		t.Errorf("没限额的那把应当是两列 NULL，得到 rpm=%s quota=%v",
			showPtr(plain.RateLimitRPM), plain.QuotaLimit)
	}
}

// 按用户过滤：不传用户时列全部，传了就只列那一个用户的。
func TestListKeysFiltersByUser(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	mine := newUser(ctx, t, db, userStatusActive)
	other := newUser(ctx, t, db, userStatusActive)
	if _, err := store.Create(ctx, CreateParams{UserID: mine.ID}); err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	if _, err := store.Create(ctx, CreateParams{UserID: other.ID}); err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	keys, err := store.ListKeys(ctx, &mine.ID)
	if err != nil {
		t.Fatalf("列 key 失败: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("列出 %d 把 key，期望 1", len(keys))
	}
	if keys[0].UserID != mine.ID {
		t.Errorf("列出来的 key 属于用户 %d，期望 %d", keys[0].UserID, mine.ID)
	}
}
