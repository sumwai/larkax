package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// readLimits 直接查库读回一把 key 的两列额度。
//
// 走 SQL 而不是走本包的读路径：这一层的落点正是「列里到底存了什么」——
// 读法也走本包的代码时，两边一起错（比如把 12.34 写成 1234000000 又原样
// 读回来）这个用例会跟着一起通过。读路径自己的那一份断言在
// TestGetKeyReadsEveryColumn 里。
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

// TestUpdateKeySetsAndClearsLimits 钉住三态：设值 / 设成 0 / 清空。
//
// 三个状态两两之间都必须分得开，尤其是**清空与设成 0**：前者是不限，
// 后者是一个都不放行。把清空实现成「写成 0」会让每一次「恢复默认」的动作
// 变成一次静默的封禁，而它不会报任何错。
func TestUpdateKeySetsAndClearsLimits(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, _ := createKey(ctx, t, db, store)

	// 1. 两项一起设上。
	rpm := 120
	quota := money.MustParse("5")
	if _, err := store.UpdateKey(ctx, created.ID, KeyParams{
		SetRateLimit: true, RateLimitRPM: &rpm,
		SetQuota: true, QuotaLimit: &quota,
	}); err != nil {
		t.Fatalf("设额度失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, strPtr("120"), strPtr("5.00000000"))

	// 2. 只改 rpm，quota 必须原样不动。
	zero := 0
	if _, err := store.UpdateKey(ctx, created.ID, KeyParams{
		SetRateLimit: true, RateLimitRPM: &zero,
	}); err != nil {
		t.Fatalf("把 rpm 设成 0 失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, strPtr("0"), strPtr("5.00000000"))

	// 3. 只清空 quota，rpm 必须原样不动——而且它还是 0，不是 NULL。
	if _, err := store.UpdateKey(ctx, created.ID, KeyParams{
		SetQuota: true, QuotaLimit: nil,
	}); err != nil {
		t.Fatalf("清空 quota 失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, strPtr("0"), nil)

	// 4. 再清空 rpm。
	if _, err := store.UpdateKey(ctx, created.ID, KeyParams{
		SetRateLimit: true, RateLimitRPM: nil,
	}); err != nil {
		t.Fatalf("清空 rpm 失败: %v", err)
	}
	assertLimits(ctx, t, db, created.ID, nil, nil)
}

// 改一把不存在的 key 要报错，而不是安静地什么都不做。
//
// 静默的成功在这里格外危险：运维以为自己刚把某把 key 吊销了，
// 而实际上那条命令什么都没碰到。
func TestUpdateKeyRejectsMissingKey(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	rpm := 60
	_, err := store.UpdateKey(ctx, 1<<62, KeyParams{SetRateLimit: true, RateLimitRPM: &rpm})
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("错误 = %v，期望满足 errors.Is(err, ErrKeyNotFound)", err)
	}
}

// 什么都没要求改时必须报错。
//
// 零值意味着「不改」，所以一条不带任何字段的 update 是一条什么都没做的
// 命令；让它安静地成功，运维会以为改生效了。
func TestUpdateKeyRequiresAChange(t *testing.T) {
	store := New(nil) // 这条检查在连库之前就该发生

	_, err := store.UpdateKey(context.Background(), 7, KeyParams{})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "至少要给一项") {
		t.Errorf("错误信息 %q 没说清缺什么", err.Error())
	}
}

// TestUpdateKeyWritesStatusNameAndExpiry 钉住另外三列真的写得进去。
//
// 这一条是 #693 的落点：状态、备注、过期时间原先只有读侧（鉴权那边会按
// 它们拒绝），没有任何一条路径能把它们写进去。断言的是改完**读回来**的
// 具体值，而不是「没报错」——一条把 SET 写成别的列的 UPDATE 同样不报错。
func TestUpdateKeyWritesStatusNameAndExpiry(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, owner := createKey(ctx, t, db, store)
	expires := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)

	got, err := store.UpdateKey(ctx, created.ID, KeyParams{
		SetStatus: true, Status: keyStatusDisabled,
		SetName:      true,
		Name:         "换过备注",
		SetExpiresAt: true, ExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("改状态/备注/过期失败: %v", err)
	}

	// 交回来的那一行就是改完的样子，不必再查一次。
	if got.Status != keyStatusDisabled {
		t.Errorf("交回的 status = %q，期望 %q", got.Status, keyStatusDisabled)
	}
	if got.Name != "换过备注" {
		t.Errorf("交回的 name = %q，期望 %q", got.Name, "换过备注")
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("交回的 expires_at = %s，期望 %s", showPtr(got.ExpiresAt), expires)
	}
	// 被要求改的三列之外，其余列必须原样：另一条读路径给的是同一行。
	if got.Prefix != created.Prefix || got.UserID == 0 || got.Username == "" {
		t.Errorf("交回的行丢了不该动的列: prefix=%q user_id=%d username=%q",
			got.Prefix, got.UserID, got.Username)
	}

	// 再独立读一次库：交回的值来自 UPDATE … RETURNING，而这一条来自 SELECT。
	reread, err := store.GetKey(ctx, created.ID)
	if err != nil {
		t.Fatalf("读回 key 失败: %v", err)
	}
	if reread.Status != keyStatusDisabled || reread.Name != "换过备注" {
		t.Errorf("库里读回的是 status=%q name=%q，期望 %q / %q",
			reread.Status, reread.Name, keyStatusDisabled, "换过备注")
	}
	if reread.ExpiresAt == nil || !reread.ExpiresAt.Equal(expires) {
		t.Errorf("库里读回的 expires_at = %s，期望 %s", showPtr(reread.ExpiresAt), expires)
	}
	if reread.Prefix != created.Prefix || reread.UserID != owner.ID {
		t.Errorf("库里读回的行身份栏不对: prefix=%q user_id=%d，期望 %q / %d",
			reread.Prefix, reread.UserID, created.Prefix, owner.ID)
	}

	// 清空过期时间：写回 NULL，而不是把某一列的零值写进去。
	cleared, err := store.UpdateKey(ctx, created.ID, KeyParams{SetExpiresAt: true, ExpiresAt: nil})
	if err != nil {
		t.Fatalf("清空过期失败: %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("清空之后交回的 expires_at = %s，期望 nil", showPtr(cleared.ExpiresAt))
	}
	if cleared.Status != keyStatusDisabled || cleared.Name != "换过备注" {
		t.Errorf("只清过期时另外两列被动过了: status=%q name=%q",
			cleared.Status, cleared.Name)
	}
}

// TestUpdateKeyLeavesLimitsAloneWhenOnlyMetaChanges 钉住「只改备注不碰额度」。
//
// 三列额度与三列元信息共用一条语句，参数错位（$6/$7 接到 $8/$9 上）在
// 类型上未必报错：都能是 text 的时候，备注会被写进状态列，而语句照样返回
// 一行。所以要有一把**带额度**的 key 做底，改完之后逐列比对。
func TestUpdateKeyLeavesLimitsAloneWhenOnlyMetaChanges(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	owner := newUser(ctx, t, db, userStatusActive)
	rpm := 45
	quota := money.MustParse("7.25")
	created, err := store.Create(ctx, CreateParams{
		UserID: owner.ID, Name: "原备注",
		RateLimitRPM: &rpm, QuotaLimit: &quota,
	})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	got, err := store.UpdateKey(ctx, created.ID, KeyParams{SetName: true, Name: "新备注"})
	if err != nil {
		t.Fatalf("改备注失败: %v", err)
	}
	if got.Name != "新备注" {
		t.Errorf("name = %q，期望 %q", got.Name, "新备注")
	}
	if got.Status != keyStatusActive {
		t.Errorf("status = %q，期望 %q", got.Status, keyStatusActive)
	}
	if got.RateLimitRPM == nil || *got.RateLimitRPM != 45 {
		t.Errorf("rate_limit_rpm = %s，期望 45", showPtr(got.RateLimitRPM))
	}
	if got.QuotaLimit == nil || got.QuotaLimit.String() != "7.25000000" {
		t.Errorf("quota_limit = %v，期望 7.25000000", got.QuotaLimit)
	}
	assertLimits(ctx, t, db, created.ID, strPtr("45"), strPtr("7.25000000"))
}

// TestUpdateKeyKeepsTheRightOwner 钉住那条 UPDATE 的连接条件。
//
// 语句是 `UPDATE api_keys … FROM users …`，两表靠 `u.id = k.user_id` 连起来。
// 这个条件一旦写松（比如只留 `WHERE k.id = $1`），两表就成了叉积：被改的那一
// 行仍然只有一行、`RETURNING` 也只回一行，但交回来的 username 会变成**另一个
// 用户**的名字——实测取到的是 users 里扫到的头一行。而 `QueryRowContext` 对多
// 出来的行静默取第一行，所以症状是「回显报了别人的属主」，不是报错。库里那一行
// 其实没被写坏，坏的是命令告诉运维的那句话，只断言「改成功」看不出来。
//
// 安排上有一处是刻意的：建两个用户，改**后建的**那一个的 key。叉积取到的是头
// 一行（先建的那个），被改的若是头一行，这个错误就正好看不出来——实测过：改先
// 建的那把时，写松的语句照样交回正确的用户名，用例是绿的。
func TestUpdateKeyKeepsTheRightOwner(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	alice := newUser(ctx, t, db, userStatusActive)
	bob := newUser(ctx, t, db, userStatusActive)

	hers, err := store.Create(ctx, CreateParams{UserID: alice.ID, Name: "alice 的"})
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	his, err := store.Create(ctx, CreateParams{UserID: bob.ID, Name: "bob 的"})
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}

	got, err := store.UpdateKey(ctx, his.ID, KeyParams{SetStatus: true, Status: keyStatusRevoked})
	if err != nil {
		t.Fatalf("吊销失败: %v", err)
	}

	if got.ID != his.ID {
		t.Errorf("交回的 id = %d，期望 %d", got.ID, his.ID)
	}
	if got.UserID != bob.ID {
		t.Errorf("交回的 user_id = %d，期望 %d", got.UserID, bob.ID)
	}
	if got.Username != bob.Username {
		t.Errorf("交回的 username = %q，期望 %q："+
			"那条语句的连接条件写松了，两表成了叉积，username 取到了另一个用户"+
			"（alice 是 %q）", got.Username, bob.Username, alice.Username)
	}

	// 另一个用户的那把必须原样：改错了行与报错了名字是两件事，两件都要挡。
	untouched, err := store.GetKey(ctx, hers.ID)
	if err != nil {
		t.Fatalf("读另一把 key 失败: %v", err)
	}
	if untouched.UserID != alice.ID || untouched.Username != alice.Username {
		t.Errorf("另一把的属主被动过了：id=%d username=%q，期望 id=%d username=%q",
			untouched.UserID, untouched.Username, alice.ID, alice.Username)
	}
	if untouched.Status != keyStatusActive || untouched.Name != "alice 的" {
		t.Errorf("另一把被动过了：status=%q name=%q，期望 %q / %q",
			untouched.Status, untouched.Name, keyStatusActive, "alice 的")
	}
}

// TestUpdateKeyRejectsNonPositiveID 钉住 id 的检查在连库之前发生。
//
// 0 与负数在库里不可能有一行（id 是 bigserial），放它进查询只会得到一句
// 「找不到」——那句话对着一个不可能存在的 id 是误导。
func TestUpdateKeyRejectsNonPositiveID(t *testing.T) {
	store := New(nil)

	for _, id := range []int64{0, -1} {
		_, err := store.UpdateKey(context.Background(), id,
			KeyParams{SetStatus: true, Status: keyStatusRevoked})
		if err == nil {
			t.Fatalf("id=%d 期望报错", id)
		}
		if !strings.Contains(err.Error(), "正整数") {
			t.Errorf("id=%d 的错误信息 %q 没说清 id 不对", id, err.Error())
		}
	}
}

// TestGetKeyReadsEveryColumn 钉住读一把 key 时每一栏都取到了。
//
// 逐列比对而不是比「不为空」：SELECT 里漏掉一列、或者 Scan 的参数对不上
// 位置时，那一栏会静默停在零值上（过期变 0001-01-01、额度变 nil），
// 而整个结构体看起来是「读到了」。
func TestGetKeyReadsEveryColumn(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	var dbBefore time.Time
	if err := db.QueryRowContext(ctx, "SELECT now()").Scan(&dbBefore); err != nil {
		t.Fatalf("读数据库时钟失败: %v", err)
	}

	owner := newUser(ctx, t, db, userStatusActive)
	rpm := 15
	quota := money.MustParse("3.5")
	expires := time.Now().Add(time.Hour).Truncate(time.Microsecond)

	created, err := store.Create(ctx, CreateParams{
		UserID: owner.ID, Name: "带全套",
		ExpiresAt: &expires, RateLimitRPM: &rpm, QuotaLimit: &quota,
	})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	got, err := store.GetKey(ctx, created.ID)
	if err != nil {
		t.Fatalf("读 key 失败: %v", err)
	}

	if got.ID != created.ID || got.UserID != owner.ID || got.Username != owner.Username {
		t.Errorf("身份栏不对: id=%d user_id=%d username=%q，期望 %d / %d / %q",
			got.ID, got.UserID, got.Username, created.ID, owner.ID, owner.Username)
	}
	if got.Name != "带全套" {
		t.Errorf("name = %q，期望 %q", got.Name, "带全套")
	}
	if got.Prefix != created.Prefix {
		t.Errorf("prefix = %q，期望 %q", got.Prefix, created.Prefix)
	}
	if got.Status != keyStatusActive {
		t.Errorf("status = %q，期望 %q", got.Status, keyStatusActive)
	}
	if got.RateLimitRPM == nil || *got.RateLimitRPM != 15 {
		t.Errorf("rate_limit_rpm = %s，期望 15", showPtr(got.RateLimitRPM))
	}
	if got.QuotaLimit == nil || got.QuotaLimit.String() != "3.50000000" {
		t.Errorf("quota_limit = %v，期望 3.50000000", got.QuotaLimit)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %s，期望 %s", showPtr(got.ExpiresAt), expires)
	}
	if got.LastUsedAt != nil {
		t.Errorf("last_used_at = %s，期望 nil（还没用过）", showPtr(got.LastUsedAt))
	}
	if got.CreatedAt.Before(dbBefore) {
		t.Errorf("created_at = %s，早于建 key 时的库时钟 %s：这一栏没从库里取出来",
			got.CreatedAt, dbBefore)
	}

	// 两列额度为 NULL 的那一把：nil 与「0」必须分得开。
	plain, err := store.Create(ctx, CreateParams{UserID: owner.ID})
	if err != nil {
		t.Fatalf("创建第二把 key 失败: %v", err)
	}
	bare, err := store.GetKey(ctx, plain.ID)
	if err != nil {
		t.Fatalf("读第二把 key 失败: %v", err)
	}
	if bare.RateLimitRPM != nil || bare.QuotaLimit != nil || bare.ExpiresAt != nil {
		t.Errorf("不限额的 key 应当是三列 NULL，得到 rpm=%s quota=%v expires=%s",
			showPtr(bare.RateLimitRPM), bare.QuotaLimit, showPtr(bare.ExpiresAt))
	}
}

// 读一把不存在的 key 与改一把不存在的 key 报同一个错。
//
// 两处各判一次「找不到」时，命令行那侧要按退出码分流，两处给出的错误值
// 必须是同一个可用 errors.Is 认出来的哨兵。
func TestGetKeyRejectsMissingKey(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	_, err := store.GetKey(ctx, 1<<62)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("错误 = %v，期望满足 errors.Is(err, ErrKeyNotFound)", err)
	}

	if _, err := store.GetKey(ctx, 0); err == nil {
		t.Fatal("id=0 期望报错")
	}
}

// TestUpdateKeyRevocationReachesAuthenticate 把「吊销」这件事从写侧串到鉴权侧。
//
// 断言的是鉴权给出的**原因**（ReasonKeyRevoked），不是库里的那一列：
// 列改对了而鉴权没读它，运维会看到一把「已吊销」的 key 继续能用，
// 而只查列的用例照样全绿。同一把 key 走同一条 UPDATE，前后各鉴权一次，
// 所以「变了」这件事不可能是别的路径造成的。
func TestUpdateKeyRevocationReachesAuthenticate(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, _ := createKey(ctx, t, db, store)
	auth := func() (User, error) {
		return store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext))
	}

	if _, err := auth(); err != nil {
		t.Fatalf("吊销之前鉴权就失败了: %v", err)
	}

	if _, err := store.UpdateKey(ctx, created.ID,
		KeyParams{SetStatus: true, Status: keyStatusRevoked}); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	_, err := auth()
	assertReason(t, err, ReasonKeyRevoked)

	// 改回去再验一次：上面那次失败不能是「更新把行写坏了」造成的。
	if _, err := store.UpdateKey(ctx, created.ID,
		KeyParams{SetStatus: true, Status: keyStatusActive}); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if _, err := auth(); err != nil {
		t.Fatalf("恢复之后鉴权仍然失败: %v", err)
	}
}

// TestUpdateKeyStatusDisabledReachesAuthenticate 同样串一次 disabled。
//
// 它与 revoked 在鉴权侧是两个不同的原因，而两者都是「不能用」——只测其中
// 一条时，把两种状态写反（disabled 被当成 revoked）不会红。两者都不该
// 变成 ReasonKeyInactive：那一条是留给「库里出现了 CHECK 之外的值」的。
func TestUpdateKeyStatusDisabledReachesAuthenticate(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, _ := createKey(ctx, t, db, store)

	if _, err := store.UpdateKey(ctx, created.ID,
		KeyParams{SetStatus: true, Status: keyStatusDisabled}); err != nil {
		t.Fatalf("停用失败: %v", err)
	}

	_, err := store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext))
	assertReason(t, err, ReasonKeyDisabled)
}

// TestUpdateKeyExpiryReachesAuthenticate 串一次过期时间。
//
// 过期与状态在库里是两列、判在鉴权侧的两处，而命令行把两者放在同一条
// update 上。这一条走的是「写一个**过去**的时刻就立刻失效」——
// 它同时钉住过期那一列的写入与判定用的是同一套时间语义。
func TestUpdateKeyExpiryReachesAuthenticate(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, _ := createKey(ctx, t, db, store)

	past := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := store.UpdateKey(ctx, created.ID,
		KeyParams{SetExpiresAt: true, ExpiresAt: &past}); err != nil {
		t.Fatalf("写过期的时刻失败: %v", err)
	}
	_, err := store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext))
	assertReason(t, err, ReasonKeyExpired)

	// 清空之后又能用了：写 NULL 与「写一个过去的时刻」是两件事。
	if _, err := store.UpdateKey(ctx, created.ID,
		KeyParams{SetExpiresAt: true, ExpiresAt: nil}); err != nil {
		t.Fatalf("清空过期失败: %v", err)
	}
	if _, err := store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext)); err != nil {
		t.Fatalf("清空过期之后鉴权仍然失败: %v", err)
	}
}

// TestListKeys 钉住列表里每一栏的取值，尤其是两列额度的 NULL 与值。
func TestListKeys(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	// created_at 由列的 DEFAULT now() 给，所以基准取**数据库自己的** now()，
	// 而不是 Go 的 time.Now()：取值来源是前者，两个进程的时钟不一定一致。
	//
	// 这一条是列表里「创建」那一栏唯一的门。展示那一侧的用例喂的是夹具，
	// 把 SELECT 里的 created_at 连同 Scan 一起删掉它照样通过——而列表里
	// 那一栏会静默变成 0001-01-01。
	var dbBefore time.Time
	if err := db.QueryRowContext(ctx, "SELECT now()").Scan(&dbBefore); err != nil {
		t.Fatalf("读数据库时钟失败: %v", err)
	}

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
	if got.CreatedAt.Before(dbBefore) {
		t.Errorf("created_at = %s，早于建 key 时的库时钟 %s：这一栏没从库里取出来",
			got.CreatedAt, dbBefore)
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
