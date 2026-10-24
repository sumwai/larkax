package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
)

// 打标（RequestDeletion）的三条前置条件：key 已全部吊销、商户应付已结清、
// 名下渠道已清理。缺一条就拒绝，且指名是哪一条。
//
// 这里只用真库证明：三条判据各自读的是 api_keys.status、ledger_entries 的分录
// 之和与 channels.deleted_at，内存里的实现证明不了其中一个字节。

// deletionUserSnapshot 是「被拒绝之后账号停在原处」要核对的那几列。
//
// updated_at 也在里面：被拒绝的那条路如果顺手写了一次 users（哪怕只改
// updated_at），状态栏看起来一样，而这一列会不一样。
type deletionUserSnapshot struct {
	status    string
	prior     sql.NullString
	deletedAt sql.NullTime
	updatedAt time.Time
}

func readDeletionUserSnapshot(ctx context.Context, t *testing.T, db *sql.DB, id int64) deletionUserSnapshot {
	t.Helper()
	var snap deletionUserSnapshot
	err := db.QueryRowContext(ctx, `
		SELECT status, deletion_prior_status, deleted_at, updated_at
		  FROM users WHERE id = $1`, id,
	).Scan(&snap.status, &snap.prior, &snap.deletedAt, &snap.updatedAt)
	if err != nil {
		t.Fatalf("读用户 %d 的状态失败: %v", id, err)
	}
	return snap
}

// requireUntouchedDeletionUser 断言这一行还是销户流程之外的样子。
func requireUntouchedDeletionUser(ctx context.Context, t *testing.T, db *sql.DB, id int64, want deletionUserSnapshot) {
	t.Helper()
	got := readDeletionUserSnapshot(ctx, t, db, id)
	if got.status != want.status {
		t.Errorf("被拒绝之后 status = %q，期望仍是 %q", got.status, want.status)
	}
	if got.prior.Valid != want.prior.Valid || got.prior.String != want.prior.String {
		t.Errorf("被拒绝之后 deletion_prior_status = %v，期望仍是 %v", got.prior, want.prior)
	}
	if got.deletedAt.Valid != want.deletedAt.Valid {
		t.Errorf("被拒绝之后 deleted_at = %v，期望仍是 %v", got.deletedAt, want.deletedAt)
	}
	if !got.updatedAt.Equal(want.updatedAt) {
		t.Errorf("被拒绝之后 updated_at = %s，期望仍是 %s（那一行不该被写过）",
			got.updatedAt, want.updatedAt)
	}
}

// seedMerchantPayableEntry 给一个商户的分成科目写一笔**平衡的**分录：
// merchant_payable 上 +amount、platform_revenue 上 -amount。
//
// 单边分录会破坏账本「同一 txn_id 求和为零」的不变式，而同一张库上的校验会把
// 它读进去。按 txn_id 登记清理。
func seedMerchantPayableEntry(ctx context.Context, t *testing.T, db *sql.DB,
	merchantID int64, asset, amount string) {
	t.Helper()
	txnID := string(ledger.NewTxnID())
	_, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, asset, amount, entry_type, memo)
		VALUES ($1, $2, $3, $4::numeric, 'adjustment', '销户前置条件用例'),
		       ($1, $5, $3, -($4::numeric), 'adjustment', '销户前置条件用例')`,
		txnID, ledger.MerchantPayableAccount(merchantID).String(), asset, amount,
		ledger.PlatformRevenueAccount().String())
	if err != nil {
		t.Fatalf("写商户应付分录失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM ledger_entries WHERE txn_id = $1`, txnID); err != nil {
			t.Logf("清理账本分录 %s 失败: %v", txnID, err)
		}
	})
}

// seedUserChannel 建一条归 ownerID 的渠道；alreadyDeleted 为真时直接写成软删。
func seedUserChannel(ctx context.Context, t *testing.T, db *sql.DB, ownerID int64, alreadyDeleted bool) int64 {
	t.Helper()
	var deletedAt any
	if alreadyDeleted {
		deletedAt = time.Now()
	}
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO channels
			(owner_user_id, name, type, base_url, models, deleted_at)
		VALUES ($1, $2, 'openai', 'https://upstream.example.com/v1', '{}', $3)
		RETURNING id`,
		ownerID, uniqueUsername(), deletedAt).Scan(&id)
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM channels WHERE id = $1`, id); err != nil {
			t.Logf("清理渠道 %d 失败: %v", id, err)
		}
	})
	return id
}

// TestRequestDeletionRejectsKeysNotRevoked 钉住第一条：还有 status <> 'revoked'
// 的 key 时拒绝，而且**停用的也算**。
//
// 分两步放开：先把 active 那把改成 revoked，仍然被拒——实现若只数 status =
// 'active'，这一步就会放行。再把 disabled 那把也改成 revoked，才成功。
func TestRequestDeletionRejectsKeysNotRevoked(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)
	before := readDeletionUserSnapshot(ctx, t, db, u.ID)

	active, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("创建 active key 失败: %v", err)
	}
	disabled, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("创建 disabled key 失败: %v", err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'disabled' WHERE id = $1`, disabled.ID); err != nil {
		t.Fatalf("停用 key 失败: %v", err)
	}

	_, err = store.RequestDeletion(ctx, u.Username)
	if !errors.Is(err, ErrDeletionKeysNotRevoked) {
		t.Fatalf("还有没吊销的 key，错误 = %v，想要 ErrDeletionKeysNotRevoked", err)
	}
	// 报错要点名是哪一条、下一步做什么。
	for _, want := range []string{"API Key", "2 把", "larkaxctl apikey update --id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q: %v", want, err)
		}
	}
	requireUntouchedDeletionUser(ctx, t, db, u.ID, before)

	// 只把 active 那把吊销：停用的仍然不是 revoked，判据是 <> 'revoked'。
	if _, err = db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE id = $1`, active.ID); err != nil {
		t.Fatalf("吊销 active key 失败: %v", err)
	}
	_, err = store.RequestDeletion(ctx, u.Username)
	if !errors.Is(err, ErrDeletionKeysNotRevoked) {
		t.Fatalf("只剩一把 disabled 的 key，错误 = %v，想要仍然拒绝", err)
	}
	if !strings.Contains(err.Error(), "1 把") {
		t.Errorf("第二次拒绝应当只数到 1 把: %v", err)
	}
	requireUntouchedDeletionUser(ctx, t, db, u.ID, before)

	if _, err = db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE id = $1`, disabled.ID); err != nil {
		t.Fatalf("吊销 disabled key 失败: %v", err)
	}
	mark, err := store.RequestDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("两把都吊销之后打标失败: %v", err)
	}
	if mark.ToStatus != UserStatusDeletionRequested {
		t.Errorf("打标之后的状态 = %q，想要 %q", mark.ToStatus, UserStatusDeletionRequested)
	}
}

// TestRequestDeletionRejectsMerchantPayableNotZero 钉住第二条：科目余额从分录
// 现算，而且**按资产分别判**。
//
// 两笔分录一正一负、跨资产合计恰好是 0：把不同资产加在一起再判的实现会放行，
// 而两种资产各自都还欠着钱。
func TestRequestDeletionRejectsMerchantPayableNotZero(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)
	before := readDeletionUserSnapshot(ctx, t, db, u.ID)

	seedMerchantPayableEntry(ctx, t, db, u.ID, "CNY", "12.34")
	seedMerchantPayableEntry(ctx, t, db, u.ID, "USD", "-12.34")

	_, err := store.RequestDeletion(ctx, u.Username)
	if !errors.Is(err, ErrDeletionMerchantPayableNotZero) {
		t.Fatalf("商户应付不是 0，错误 = %v，想要 ErrDeletionMerchantPayableNotZero", err)
	}
	for _, want := range []string{"merchant_payable", "CNY 12.34", "USD -12.34", "wallet payout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q: %v", want, err)
		}
	}
	requireUntouchedDeletionUser(ctx, t, db, u.ID, before)

	// 写两笔反向的分录把两个资产各自抵平（账本只追加，不 UPDATE）。
	seedMerchantPayableEntry(ctx, t, db, u.ID, "CNY", "-12.34")
	seedMerchantPayableEntry(ctx, t, db, u.ID, "USD", "12.34")
	mark, err := store.RequestDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("两个资产都归零之后打标失败: %v", err)
	}
	if mark.ToStatus != UserStatusDeletionRequested {
		t.Errorf("打标之后的状态 = %q，想要 %q", mark.ToStatus, UserStatusDeletionRequested)
	}
}

// TestRequestDeletionRejectsLiveChannels 钉住第三条：名下还有未软删的渠道时
// 拒绝；软删过的行不算，别人的渠道也不算。
func TestRequestDeletionRejectsLiveChannels(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)
	before := readDeletionUserSnapshot(ctx, t, db, u.ID)

	// 别人的渠道：不该被数进来。
	other := newUser(ctx, t, db, UserStatusActive)
	seedUserChannel(ctx, t, db, other.ID, false)

	live := seedUserChannel(ctx, t, db, u.ID, false)
	seedUserChannel(ctx, t, db, u.ID, true) // 已经软删的一行

	_, err := store.RequestDeletion(ctx, u.Username)
	if !errors.Is(err, ErrDeletionChannelsNotCleared) {
		t.Fatalf("还有未删渠道，错误 = %v，想要 ErrDeletionChannelsNotCleared", err)
	}
	for _, want := range []string{"1 条渠道", "larkaxctl channel delete --id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错里没有 %q: %v", want, err)
		}
	}
	requireUntouchedDeletionUser(ctx, t, db, u.ID, before)

	// 软删之后不再计入。状态跟着 disabled 是 channel delete 的行为，这里直接
	// 改库只是为了把判据钉在 deleted_at 上。
	if _, err = db.ExecContext(ctx,
		`UPDATE channels SET deleted_at = now(), status = 'disabled' WHERE id = $1`, live); err != nil {
		t.Fatalf("软删渠道失败: %v", err)
	}
	mark, err := store.RequestDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("名下渠道都软删之后打标失败: %v", err)
	}
	if mark.ToStatus != UserStatusDeletionRequested {
		t.Errorf("打标之后的状态 = %q，想要 %q", mark.ToStatus, UserStatusDeletionRequested)
	}
}

// TestRequestDeletionSucceedsWhenAllPreconditionsMet 是「三条都满足」那一格：
// 一把已吊销的 key、商户应付科目归零、一条软删的渠道。
func TestRequestDeletionSucceedsWhenAllPreconditionsMet(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	key, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE id = $1`, key.ID); err != nil {
		t.Fatalf("吊销 key 失败: %v", err)
	}
	seedUserChannel(ctx, t, db, u.ID, true)
	// 一正一负、同资产归零：科目上不是没有分录，而是分录之和为 0。
	seedMerchantPayableEntry(ctx, t, db, u.ID, "CNY", "3.5")
	seedMerchantPayableEntry(ctx, t, db, u.ID, "CNY", "-3.5")

	mark, err := store.RequestDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("三条都满足时打标失败: %v", err)
	}
	if mark.FromStatus != UserStatusActive || mark.ToStatus != UserStatusDeletionRequested {
		t.Errorf("打标的两个状态是 %s → %s，想要 active → deletion_requested",
			mark.FromStatus, mark.ToStatus)
	}
}

// TestRequestDeletionRejectionLeavesAccountUntouchedAndRevertStillWorks 证明
// 被拒是**整笔回滚**，而不是停在半路；撤销那条路不受新判据影响。
func TestRequestDeletionRejectionLeavesAccountUntouchedAndRevertStillWorks(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)
	before := readDeletionUserSnapshot(ctx, t, db, u.ID)

	live := seedUserChannel(ctx, t, db, u.ID, false)
	if _, err := store.RequestDeletion(ctx, u.Username); !errors.Is(err, ErrDeletionChannelsNotCleared) {
		t.Fatalf("有未删渠道时的错误 = %v，想要 ErrDeletionChannelsNotCleared", err)
	}
	requireUntouchedDeletionUser(ctx, t, db, u.ID, before)

	if _, err := db.ExecContext(ctx,
		`UPDATE channels SET deleted_at = now(), status = 'disabled' WHERE id = $1`, live); err != nil {
		t.Fatalf("软删渠道失败: %v", err)
	}
	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("清理完之后打标失败: %v", err)
	}

	mark, err := store.RevertDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if mark.ToStatus != UserStatusActive {
		t.Errorf("撤销之后的状态 = %q，想要 active", mark.ToStatus)
	}
	after := readDeletionUserSnapshot(ctx, t, db, u.ID)
	if after.status != UserStatusActive || after.prior.Valid || after.deletedAt.Valid {
		t.Errorf("撤销之后那一行是 %+v，想要 status=active、prior 与 deleted_at 都是空", after)
	}
}

// TestRequestDeletionTakesTheMerchantPayableAccountLock 证明打标真的取账本那把
// 商户应付科目锁，而且是在读余额之前。
//
// 另一笔事务先握住 merchant_payable:<id> 的事务级咨询锁，再发起打标：它必须
// 停在那把锁上，直到对方提交。没有这把锁时打标会立刻成功——余额读完就放开，
// 而退款或提现可以在那之后把它改掉。
func TestRequestDeletionTakesTheMerchantPayableAccountLock(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	acct := ledger.MerchantPayableAccount(u.ID)
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, acct.String()); err != nil {
		t.Fatalf("取科目锁失败: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, reqErr := store.RequestDeletion(ctx, u.Username)
		done <- reqErr
	}()

	waitForBlockedBy(ctx, t, db, tx, "打标停在商户应付科目锁上：它没有在写之前先取那把锁")

	if err := tx.Commit(); err != nil {
		t.Fatalf("提交科目锁事务失败: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("科目锁放开之后打标失败: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("打标一直没返回")
	}
}

// TestPurgeDeletionClearsKeyNames 证明匿名化把 api_keys.name 一起清空。
//
// 两把 key 覆盖两种来源：打标之前就存在（已吊销）的，与打标之后才建的
// （active）。后者证明 purge 侧仍会作废「打标之后冒出来的 key」，两把的备注
// 都必须清掉——备注是用户写的文本，常常写着真名。
func TestPurgeDeletionClearsKeyNames(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	before, err := store.Create(ctx, CreateParams{UserID: u.ID, Name: "alice 的 key"})
	if err != nil {
		t.Fatalf("创建打标之前的 key 失败: %v", err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE id = $1`, before.ID); err != nil {
		t.Fatalf("吊销 key 失败: %v", err)
	}
	if _, err = store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}

	late, err := store.Create(ctx, CreateParams{UserID: u.ID, Name: "后来又建的 key"})
	if err != nil {
		t.Fatalf("打标之后建 key 失败: %v", err)
	}

	result, err := store.PurgeDeletion(ctx, u.Username, u.Username)
	if err != nil {
		t.Fatalf("匿名化失败: %v", err)
	}
	if result.KeysRevoked != 1 || result.KeysTotal != 2 {
		t.Errorf("key 回执 = %d revoked / 共 %d，想要 1 / 2", result.KeysRevoked, result.KeysTotal)
	}

	for _, id := range []int64{before.ID, late.ID} {
		var name, status string
		if err := db.QueryRowContext(ctx,
			`SELECT name, status FROM api_keys WHERE id = $1`, id,
		).Scan(&name, &status); err != nil {
			t.Fatalf("读 key %d 失败: %v", id, err)
		}
		if name != "" {
			t.Errorf("key %d 的备注是 %q，匿名化之后必须清空", id, name)
		}
		if status != "revoked" {
			t.Errorf("key %d 的状态是 %q，想要 revoked", id, status)
		}
	}
}
