package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
)

// 销户那一族的集成测试。
//
// 这里要证明的性质全都来自**库**：status 上的 CHECK 扩了哪几个取值、唯一约束
// 让占位用户名撞不撞、一个事务里动五张表之后它们各自停在哪一档。内存里的实现
// 证明不了其中任何一条。
//
// 没有 LARKAX_DB_DSN 时跳过（见 testDB），CI 里那个变量一直在。

// newDeletionUser 建一个带邮箱与展示名的用户，登记清理。
//
// 与 newUser 分开：销户要核对的就是这两列被换成了什么，而 newUser 只给一个
// 用户名。
func newDeletionUser(ctx context.Context, t *testing.T, db *sql.DB, status, email, displayName string) User {
	t.Helper()

	u := User{Username: uniqueUsername()}
	err := db.QueryRowContext(ctx, `
		INSERT INTO users (username, email, display_name, status)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4)
		RETURNING id`, u.Username, email, displayName, status,
	).Scan(&u.ID)
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM users WHERE id = $1`, u.ID); err != nil {
			t.Logf("清理用户 %d 失败: %v", u.ID, err)
		}
	})
	return u
}

func seedWallet(ctx context.Context, t *testing.T, db *sql.DB, userID int64, balance, frozen string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_wallets (user_id, asset, balance, frozen)
		VALUES ($1, 'CNY', $2::numeric, $3::numeric)`, userID, balance, frozen,
	); err != nil {
		t.Fatalf("建钱包失败: %v", err)
	}
}

// seedLedgerPair 写一笔**平衡的**分录：用户账户 +1、平台收入 -1，共用一个
// txn_id，并按 txn_id 登记清理。
//
// 单边分录会让账本的不变式（每个 txn_id 求和为零）不成立：销户本身不写分录，
// 但同一张库上的其他用例与校验会把这几行读进去，而它们不是一笔真实交易。
func seedLedgerPair(ctx context.Context, t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	txnID := string(ledger.NewTxnID())
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, amount, entry_type, memo)
		VALUES ($1, $2, 1, 'adjustment', '销户用例'),
		       ($1, $3, -1, 'adjustment', '销户用例')`,
		txnID, ledger.UserAccount(userID).String(), ledger.PlatformRevenueAccount().String(),
	); err != nil {
		t.Fatalf("写账本分录失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM ledger_entries WHERE txn_id = $1`, txnID); err != nil {
			t.Logf("清理账本分录 %s 失败: %v", txnID, err)
		}
	})
	return txnID
}

// TestDeletionMarkLeavesKeysAloneAndRevertDoesNotUnrevoke 证明两件事：
// 打标本身不改任何一把 key 的状态；撤销也不会把吊销改回来。
//
// 打标的前置条件要求 key 已全部吊销，所以「撤销之后 key 又能用」这句话不再
// 对一把被操作者吊销过的 key 成立——吊销是那个动作，不属于撤销的范围。用例
// 分两段证明：
//
//   - 打标之前就存在的 key，吊销之后再打标，行仍是 revoked；
//   - 打标之后新建的 key（命令不挡这种建法）是 active，它因为 users.status
//     不是 active 而鉴权失败，撤销之后又能用——这是「鉴权只认 status = active」
//     那一半，也是撤销真的回到原样的证据。
func TestDeletionMarkLeavesKeysAloneAndRevertDoesNotUnrevoke(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	// 第一把：打标之前就必须吊销的那一类。
	revoked, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	if _, err = db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE id = $1`, revoked.ID); err != nil {
		t.Fatalf("吊销 key 失败: %v", err)
	}
	authRevoked := func() error {
		_, authErr := store.Authenticate(ctx, request(ctx, "x-api-key", revoked.Plaintext))
		return authErr
	}
	if err = authRevoked(); err == nil {
		t.Fatal("吊销的 key 还能认证")
	}

	mark, err := store.RequestDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	if mark.FromStatus != UserStatusActive || mark.ToStatus != UserStatusDeletionRequested {
		t.Errorf("打标的两个状态是 %s → %s，想要 active → deletion_requested",
			mark.FromStatus, mark.ToStatus)
	}

	var keyStatus string
	if err = db.QueryRowContext(ctx,
		`SELECT status FROM api_keys WHERE id = $1`, revoked.ID).Scan(&keyStatus); err != nil {
		t.Fatalf("读 key 状态失败: %v", err)
	}
	if keyStatus != "revoked" {
		t.Errorf("打标把 key 的状态改成了 %q；它停在哪一档就该留在哪一档", keyStatus)
	}
	if err = authRevoked(); err == nil {
		t.Fatal("打标之后吊销的 key 又能用了")
	}

	// 第二把：打标之后才建。它证明鉴权侧的效果，以及撤销能不能还回去。
	late, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("打标之后建 key 失败: %v", err)
	}
	authLate := func() error {
		_, authErr := store.Authenticate(ctx, request(ctx, "x-api-key", late.Plaintext))
		return authErr
	}
	if err = authLate(); err == nil {
		t.Fatal("打标之后新建的 key 还能通过鉴权")
	} else if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("打标之后的失败不是「凭证无效」那一类: %v", err)
	}

	mark, err = store.RevertDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if mark.FromStatus != UserStatusDeletionRequested || mark.ToStatus != UserStatusActive {
		t.Errorf("撤销的两个状态是 %s → %s，想要 deletion_requested → active",
			mark.FromStatus, mark.ToStatus)
	}
	if err := authLate(); err != nil {
		t.Fatalf("撤销之后打标之后建的那把 key 不能用: %v", err)
	}
	// 吊销是操作者做的，撤销不替谁解除它。
	if err := authRevoked(); err == nil {
		t.Fatal("撤销把操作者吊销的 key 改回来了")
	}
}

// TestDeletionRevertRestoresPriorStatus 证明「改回来」回到的是**原来那一个**
// 状态，而不是一律 active。一个 suspended 的账号被顺手放回 active，是一次
// 权限提升，而输出上只会写着「已撤销」。
func TestDeletionRevertRestoresPriorStatus(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusSuspended)

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	mark, err := store.RevertDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if mark.ToStatus != UserStatusSuspended {
		t.Errorf("撤销之后的状态 = %q，想要 suspended", mark.ToStatus)
	}

	var prior sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT deletion_prior_status FROM users WHERE id = $1`, u.ID).Scan(&prior); err != nil {
		t.Fatalf("读 deletion_prior_status 失败: %v", err)
	}
	if prior.Valid {
		t.Errorf("撤销之后 deletion_prior_status 还是 %q，应当清回 NULL", prior.String)
	}
}

// TestDeletionRefusesNonZeroBalance 证明余额前置条件的两侧：余额或冻结额不为 0
// 时打标与匿名化都拒绝，且一个字节都不写。
//
// 冻结额单独占一条：余额与冻结额相等时「可用余额」按冻结判据是 0，但钱还在
// 那一行里，而打款与结算之后它们会落到一个已经认不出主人的账号上。
func TestDeletionRefusesNonZeroBalance(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	cases := []struct {
		name            string
		balance, frozen string
	}{
		{"余额不是 0", "12.34", "0"},
		{"冻结额不是 0", "10", "10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUser(ctx, t, db, UserStatusActive)
			seedWallet(ctx, t, db, u.ID, tc.balance, tc.frozen)

			_, err := store.RequestDeletion(ctx, u.Username)
			if err == nil {
				t.Fatal("有钱的账号不该打标成功")
			}
			if !errors.Is(err, ErrDeletionBalanceNotZero) {
				t.Fatalf("错误 %v 不是 ErrDeletionBalanceNotZero", err)
			}
			// 按字面值断言那一句：调用脚本与照着帮助改的人认的就是它。
			if !strings.Contains(err.Error(), "可用余额必须为 0") {
				t.Errorf("报错里没有那句判据：%v", err)
			}

			var status string
			if err := db.QueryRowContext(ctx,
				`SELECT status FROM users WHERE id = $1`, u.ID).Scan(&status); err != nil {
				t.Fatalf("读状态失败: %v", err)
			}
			if status != UserStatusActive {
				t.Errorf("被拒绝的打标把状态改成了 %q", status)
			}
		})
	}
}

// TestPurgeChecksBalanceAgain 证明第二次校验。
//
// 打标之后仍可能有退款或调账进来；只在打标那一步判过的话，匿名化会把那笔钱
// 留在一个再也认不出主人的账号上。
func TestPurgeChecksBalanceAgain(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)
	seedWallet(ctx, t, db, u.ID, "0", "0")

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE user_wallets SET balance = 5 WHERE user_id = $1`, u.ID); err != nil {
		t.Fatalf("打标之后入账失败: %v", err)
	}

	_, err := store.PurgeDeletion(ctx, u.Username, u.Username)
	if !errors.Is(err, ErrDeletionBalanceNotZero) {
		t.Fatalf("余额不是 0 时匿名化应当拒绝，得到 %v", err)
	}
	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM users WHERE id = $1`, u.ID).Scan(&status); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if status != UserStatusDeletionRequested {
		t.Errorf("被拒绝的匿名化把状态改成了 %q", status)
	}
}

// TestPurgeAnonymizesAndKeepsFinancialRows 是终态那一张核对表：逐项断言每一张
// 表在销户之后停在哪一档。
func TestPurgeAnonymizesAndKeepsFinancialRows(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	const email = "purgeme@example.com"
	u := newDeletionUser(ctx, t, db, UserStatusActive, email, "要销户的人")
	seedWallet(ctx, t, db, u.ID, "0", "0")
	seedLedgerPair(ctx, t, db, u.ID)
	seedLedgerPair(ctx, t, db, u.ID)

	// 两条用户态插件 KV、两条采集授权（一条有效、一条早就撤销过）。
	_, err := db.ExecContext(ctx, `
		INSERT INTO plugin_kv (plugin_key, user_id, key, value) VALUES
			('demo', $1, 'a', '1'), ('demo', $1, 'b', '2')`, u.ID)
	if err != nil {
		t.Fatalf("写插件 KV 失败: %v", err)
	}
	const revokedAt = "2026-10-01T00:00:00Z"
	_, err = db.ExecContext(ctx, `
		INSERT INTO capture_consents
			(user_id, scope, request_id, consent_text, consent_version, expires_at, revoked_at)
		VALUES ($1, 'single_request', 'req-1', '同意', 1, now() + interval '1 day', NULL),
		       ($1, 'single_request', 'req-2', '同意', 1, now() + interval '1 day', $2::timestamptz)`,
		u.ID, revokedAt)
	if err != nil {
		t.Fatalf("写采集授权失败: %v", err)
	}

	// 另一个用户：他的 key、插件 KV 与授权都不该被这条命令带走。销户的几条
	// UPDATE/DELETE 都带 user_id 条件，这里就是钉住那个条件的那一半。
	other := newDeletionUser(ctx, t, db, UserStatusActive, "", "")
	otherKey, err := store.Create(ctx, CreateParams{UserID: other.ID})
	if err != nil {
		t.Fatalf("创建另一个用户的 key 失败: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO plugin_kv (plugin_key, user_id, key, value) VALUES ('demo', $1, 'keep', 'v')`,
		other.ID)
	if err != nil {
		t.Fatalf("写另一个用户的插件 KV 失败: %v", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO capture_consents
			(user_id, scope, request_id, consent_text, consent_version, expires_at)
		VALUES ($1, 'single_request', 'req-other', '同意', 1, now() + interval '1 day')`,
		other.ID)
	if err != nil {
		t.Fatalf("写另一个用户的采集授权失败: %v", err)
	}

	_, err = store.RequestDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("打标失败: %v", err)
	}

	// 两把 key 建在**打标之后**：打标的前置条件要求此刻名下没有非 revoked 的
	// key，所以这两把只能后建。它们证明匿名化那一侧仍会作废「打标之后冒出来的
	// key」：一把 active 要被置为 revoked，一把此前就 disabled 的保留原状态
	// ——「此前停用过」也是一条该留下的记录。
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

	result, err := store.PurgeDeletion(ctx, u.Username, u.Username)
	if err != nil {
		t.Fatalf("匿名化失败: %v", err)
	}

	// ── users 行：终态与三个占位值 ───────────────────────────────────
	if result.NewUsername != fmt.Sprintf("deleted-user-%d", u.ID) {
		t.Errorf("新用户名 = %q，想要 deleted-user-%d", result.NewUsername, u.ID)
	}
	if result.NewEmail != fmt.Sprintf("deleted-user-%d@example.com", u.ID) {
		t.Errorf("新邮箱 = %q", result.NewEmail)
	}
	if result.NewDisplayName != fmt.Sprintf("已注销用户 #%d", u.ID) {
		t.Errorf("新展示名 = %q", result.NewDisplayName)
	}
	info, err := store.UserByRef(ctx, result.NewUsername)
	if err != nil {
		t.Fatalf("按新用户名读不到那一行: %v", err)
	}
	if info.Status != UserStatusDeleted {
		t.Errorf("status = %q，想要 deleted", info.Status)
	}
	if info.DeletedAt == nil {
		t.Error("deleted_at 没有写下")
	}
	if info.Username != result.NewUsername || info.Email != result.NewEmail ||
		info.DisplayName != result.NewDisplayName {
		t.Errorf("读回来的三个身份字段对不上：%+v", info)
	}

	// ── API Key：active 全部作废，行都保留 ───────────────────────────
	if result.KeysRevoked != 1 || result.KeysTotal != 2 {
		t.Errorf("Key 计数 = revoked %d / total %d，想要 1 / 2",
			result.KeysRevoked, result.KeysTotal)
	}
	var (
		liveKeys int
		revoked  int
		stillOff int
	)
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE status = 'active'),
		       count(*) FILTER (WHERE status = 'revoked'),
		       count(*) FILTER (WHERE status = 'disabled')
		  FROM api_keys WHERE user_id = $1`, u.ID,
	).Scan(&liveKeys, &revoked, &stillOff); err != nil {
		t.Fatalf("统计 key 状态失败: %v", err)
	}
	if liveKeys != 0 || revoked != 1 || stillOff != 1 {
		t.Errorf("key 状态 = active %d / revoked %d / disabled %d，想要 0 / 1 / 1",
			liveKeys, revoked, stillOff)
	}
	for _, created := range []Created{active, disabled} {
		if _, err := store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext)); err == nil {
			t.Errorf("销户之后 key id=%d 还能用", created.ID)
		}
	}

	// ── 插件 KV：清掉 ───────────────────────────────────────────────
	if result.PluginKVDeleted != 2 {
		t.Errorf("插件 KV 删了 %d 个键，想要 2", result.PluginKVDeleted)
	}
	var kvRows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_kv WHERE user_id = $1`, u.ID).Scan(&kvRows); err != nil {
		t.Fatalf("统计插件 KV 失败: %v", err)
	}
	if kvRows != 0 {
		t.Errorf("还剩下 %d 条插件 KV", kvRows)
	}

	// ── 采集授权：行保留，未撤销的撤销，已撤销的时刻不动 ─────────────
	if result.ConsentsRevoked != 1 {
		t.Errorf("撤销的采集授权 = %d 条，想要 1", result.ConsentsRevoked)
	}
	var consentRows, stillActive int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), count(*) FILTER (WHERE revoked_at IS NULL)
		  FROM capture_consents WHERE user_id = $1`, u.ID,
	).Scan(&consentRows, &stillActive); err != nil {
		t.Fatalf("统计采集授权失败: %v", err)
	}
	if consentRows != 2 || stillActive != 0 {
		t.Errorf("采集授权 = %d 行、其中未撤销 %d 行，想要 2 / 0", consentRows, stillActive)
	}
	var keptRevoked time.Time
	if err := db.QueryRowContext(ctx, `
		SELECT revoked_at FROM capture_consents
		 WHERE user_id = $1 AND request_id = 'req-2'`, u.ID).Scan(&keptRevoked); err != nil {
		t.Fatalf("读已撤销那一条失败: %v", err)
	}
	if keptRevoked.UTC().Format(time.RFC3339) != revokedAt {
		t.Errorf("早已撤销那一条的时刻被改成了 %s，想要 %s",
			keptRevoked.UTC().Format(time.RFC3339), revokedAt)
	}

	// ── 钱包与账本：行保留 ─────────────────────────────────────────
	var (
		walletRows          int
		balance, frozenText string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT count(*), COALESCE(min(balance::text), ''), COALESCE(min(frozen::text), '')
		  FROM user_wallets WHERE user_id = $1`, u.ID,
	).Scan(&walletRows, &balance, &frozenText); err != nil {
		t.Fatalf("读钱包失败: %v", err)
	}
	if walletRows != 1 || balance != "0.00000000" || frozenText != "0.00000000" {
		t.Errorf("钱包 = %d 行，余额 %s、冻结 %s，想要 1 行、两个 0",
			walletRows, balance, frozenText)
	}
	var (
		ledgerRows   int
		ledgerAmount string
	)
	account := ledger.UserAccount(u.ID).String()
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), sum(amount)::text FROM ledger_entries WHERE account = $1`,
		account).Scan(&ledgerRows, &ledgerAmount); err != nil {
		t.Fatalf("统计账本分录失败: %v", err)
	}
	if ledgerRows != 2 || ledgerAmount != "2.00000000" {
		t.Errorf("账本分录 = %d 条、用户侧合计 %s，想要 2 条、+2",
			ledgerRows, ledgerAmount)
	}
	if result.LedgerEntries != 2 {
		t.Errorf("回执说 %d 条分录，想要 2", result.LedgerEntries)
	}
	// 平台侧那两行是 -1 各一笔：单边分录会让「每个 txn_id 求和为零」不成立。
	var platformAmount string
	if err := db.QueryRowContext(ctx,
		`SELECT sum(amount)::text FROM ledger_entries
		  WHERE account = $1 AND txn_id IN
		        (SELECT txn_id FROM ledger_entries WHERE account = $2)`,
		ledger.PlatformRevenueAccount().String(), account).Scan(&platformAmount); err != nil {
		t.Fatalf("读平台侧合计失败: %v", err)
	}
	if platformAmount != "-2.00000000" {
		t.Errorf("平台侧合计 = %s，想要 -2（每笔都应当平衡）", platformAmount)
	}

	// ── 另一个用户：一行都不该变 ───────────────────────────────────
	var otherKV int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_kv WHERE user_id = $1`, other.ID,
	).Scan(&otherKV); err != nil {
		t.Fatalf("统计另一个用户的插件 KV 失败: %v", err)
	}
	if otherKV != 1 {
		t.Errorf("另一个用户的插件 KV 剩 %d 行，想要 1 行", otherKV)
	}
	var otherRevoked sql.NullTime
	if err := db.QueryRowContext(ctx, `
		SELECT revoked_at FROM capture_consents
		 WHERE user_id = $1 AND request_id = 'req-other'`, other.ID,
	).Scan(&otherRevoked); err != nil {
		t.Fatalf("读另一个用户的采集授权失败: %v", err)
	}
	if otherRevoked.Valid {
		t.Errorf("另一个用户的采集授权被撤销了：%s", otherRevoked.Time)
	}
	if _, err := store.Authenticate(ctx, request(ctx, "x-api-key", otherKey.Plaintext)); err != nil {
		t.Errorf("另一个用户的 key 失效了: %v", err)
	}
}

// TestPurgeRequiresStateAndConfirmation 证明两道门：没有打标不能匿名化，
// --confirm 与当前用户名不一致时什么都不写。
func TestPurgeRequiresStateAndConfirmation(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	if _, err := store.PurgeDeletion(ctx, u.Username, u.Username); !errors.Is(err, ErrDeletionNotRequested) {
		t.Fatalf("没打标就匿名化，错误 = %v，想要 ErrDeletionNotRequested", err)
	}
	if _, err := store.RevertDeletion(ctx, u.Username); !errors.Is(err, ErrDeletionNotRequested) {
		t.Fatalf("没打标就撤销，错误 = %v，想要 ErrDeletionNotRequested", err)
	}

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	if _, err := store.RequestDeletion(ctx, u.Username); !errors.Is(err, ErrDeletionAlreadyRequested) {
		t.Fatalf("重复打标，错误 = %v，想要 ErrDeletionAlreadyRequested", err)
	}
	if _, err := store.PurgeDeletion(ctx, u.Username, "另一个名字"); !errors.Is(err, ErrDeletionConfirmMismatch) {
		t.Fatalf("--confirm 写错，错误 = %v，想要 ErrDeletionConfirmMismatch", err)
	}
	// 写错确认之后那一行必须原样——不可逆的那一步一个字节都不该留下。
	if _, err := store.UserByRef(ctx, u.Username); err != nil {
		t.Fatalf("确认失败之后用户名变了: %v", err)
	}

	result, err := store.PurgeDeletion(ctx, u.Username, u.Username)
	if err != nil {
		t.Fatalf("匿名化失败: %v", err)
	}
	// 用户名已经换掉了，所以第二次按新用户名定位；它应当报「已经销户」，
	// 而不是落到「找不到用户」——后者会让人以为那一行被删了。
	if _, err := store.PurgeDeletion(ctx, result.NewUsername, result.NewUsername); !errors.Is(err, ErrDeletionAlreadyDeleted) {
		t.Fatalf("重复匿名化，错误 = %v，想要 ErrDeletionAlreadyDeleted", err)
	}
}

// TestPurgeSkipsATakenPlaceholderName 证明占位用户名撞上已有用户时的处置。
//
// 占位名按 ID 造，而一个用户完全可能在别人销户之前就注册了这个名字。撞上时
// 往后找下一个空位，而不是让整条命令死在唯一约束上。
func TestPurgeSkipsATakenPlaceholderName(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	taken := fmt.Sprintf("deleted-user-%d", u.ID)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (username) VALUES ($1)`, taken); err != nil {
		t.Fatalf("占住占位用户名失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM users WHERE username = $1`, taken); err != nil {
			t.Logf("清理占位用户失败: %v", err)
		}
	})

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	result, err := store.PurgeDeletion(ctx, u.Username, u.Username)
	if err != nil {
		t.Fatalf("匿名化失败: %v", err)
	}
	if want := fmt.Sprintf("deleted-user-%d-2", u.ID); result.NewUsername != want {
		t.Errorf("新用户名 = %q，想要 %q", result.NewUsername, want)
	}
}

// TestPurgeSkipsATakenPlaceholderEmail 钉住占位邮箱撞上已有用户时的处置：
// 用户名空着、但那个邮箱已经被别人占了，同样要往后找——两列都有唯一约束，
// 只查用户名会在写 users 那一句上撞回来。
func TestPurgeSkipsATakenPlaceholderEmail(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	takenEmail := fmt.Sprintf("deleted-user-%d@example.com", u.ID)
	var occupierID int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO users (username, email) VALUES ($1, $2) RETURNING id`,
		uniqueUsername(), takenEmail).Scan(&occupierID); err != nil {
		t.Fatalf("占住占位邮箱失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM users WHERE id = $1`, occupierID); err != nil {
			t.Logf("清理占位用户 %d 失败: %v", occupierID, err)
		}
	})

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	result, err := store.PurgeDeletion(ctx, u.Username, u.Username)
	if err != nil {
		t.Fatalf("匿名化失败: %v", err)
	}
	if want := fmt.Sprintf("deleted-user-%d-2", u.ID); result.NewUsername != want {
		t.Errorf("新用户名 = %q，想要 %q", result.NewUsername, want)
	}
	if want := fmt.Sprintf("deleted-user-%d-2@example.com", u.ID); result.NewEmail != want {
		t.Errorf("新邮箱 = %q，想要 %q", result.NewEmail, want)
	}
}

// TestFreeDeletionPlaceholderReachesTheLastCandidate 钉住占位名往后找的上限。
//
// 占位名按 n 从小到大试：n=1 是 `deleted-user-<id>`，之后依次加 `-2`、`-3`……
// 上限是 100 个。用例把前 99 个都占住、第 100 个空着：找得到它，说明循环真的
// 走到了 n=100；写成 n<100 时会停在第 99 个，整条匿名化因此失败。
//
// 不把 100 个全占住：两边都会失败，而报错文案里那个 100 是常量，分不出两版。
func TestFreeDeletionPlaceholderReachesTheLastCandidate(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)

	for n := 1; n <= 99; n++ {
		name := fmt.Sprintf("deleted-user-%d", u.ID)
		if n > 1 {
			name = fmt.Sprintf("deleted-user-%d-%d", u.ID, n)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (username) VALUES ($1)`, name); err != nil {
			t.Fatalf("占住第 %d 个占位名失败: %v", n, err)
		}
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx), `
			DELETE FROM users
			 WHERE username = $1 OR username LIKE $2`,
			fmt.Sprintf("deleted-user-%d", u.ID),
			fmt.Sprintf("deleted-user-%d-%%", u.ID)); err != nil {
			t.Logf("清理占位用户失败: %v", err)
		}
	})

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	got, err := freeDeletionPlaceholder(ctx, tx, u.ID)
	if err != nil {
		t.Fatalf("第 100 个占位名还空着，不该报错：%v", err)
	}
	if want := fmt.Sprintf("deleted-user-%d-100", u.ID); got != want {
		t.Errorf("占位名 = %q，期望 %q", got, want)
	}
}

// TestCheckDeletionBalanceCountsEveryWalletRow 钉住回执里的钱包行数。
//
// walletRows 是「读到了几行」，不是「有没有钱」：回执用它区分「没有钱包行」
// 与「两个数都是 0」。计数写成递减时余额判据照样对，只有这个数会变成负数——
// 所以断言要的是一个具体的行数，而不是「非零」。
func TestCheckDeletionBalanceCountsEveryWalletRow(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	seedWallet(ctx, t, db, u.ID, "0", "0")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_wallets (user_id, asset, balance, frozen)
		VALUES ($1, 'USD', 0, 0)`, u.ID); err != nil {
		t.Fatalf("建第二个钱包失败: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	balance, frozen, rows, err := checkDeletionBalance(ctx, tx, u.ID, u.Username)
	if err != nil {
		t.Fatalf("两个钱包都是 0，不该报错：%v", err)
	}
	if rows != 2 {
		t.Errorf("walletRows = %d，期望 2（CNY 与 USD 各一行）", rows)
	}
	if !balance.IsZero() || !frozen.IsZero() {
		t.Errorf("余额 = %s、冻结 = %s，期望都是 0", balance, frozen)
	}
}

// TestPurgeWaitsForTheWalletLockAndThenRefuses 证明「余额判据在锁之后」。
//
// 另一个事务先改钱包、把行锁攥在手里，然后销户必须真的停在那把锁上：提交之前
// 先在 pg_stat_activity 里看到它等待，再提交，它才继续往下读。少了这一步，销户
// 常常还没走到钱包、提交就已经完成，用例退化成「钱包里有余额」的重复——断言
// 照样绿，而它想证明的顺序一次都没发生。两种顺序在这里是可区分的：顺序不对时
// 等不到那条锁等待，用例直接失败，而不是换个理由通过。
func TestPurgeWaitsForTheWalletLockAndThenRefuses(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)
	seedWallet(ctx, t, db, u.ID, "0", "0")

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}

	// 这笔入账不提交，先把钱包行锁住。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE user_wallets SET balance = 5 WHERE user_id = $1`, u.ID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("入账失败: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.PurgeDeletion(ctx, u.Username, u.Username)
		done <- err
	}()

	waitForWalletLock(ctx, t, db, tx)

	if err := tx.Commit(); err != nil {
		t.Fatalf("提交入账失败: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrDeletionBalanceNotZero) {
			t.Fatalf("入账之后匿名化应当拒绝，错误 = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("匿名化一直没返回：它可能卡在锁上，也可能根本没走到钱包")
	}

	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM users WHERE id = $1`, u.ID).Scan(&status); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if status != UserStatusDeletionRequested {
		t.Errorf("被拒绝的匿名化把状态改成了 %q", status)
	}
}

// waitForWalletLock 等到有别的后端被 tx 这一笔事务挡住。
//
// 判据是 pg_blocking_pids：tx 只改过 user_wallets 的一行，被它挡住的后端只可能
// 停在那一行上。不读 pg_stat_activity 的 query 文本——那一列对别的会话可能显示
// 不出来，测试账号一换就会静默匹配不上；也不按等待类型判，那只说明「在等锁」，
// 说不了等的是哪一把。
//
// 超时说明被观察的那条路根本没走到钱包：那不是「等久了一点」，而是用例想证明
// 的顺序没有发生，必须红。
func waitForWalletLock(ctx context.Context, t *testing.T, db *sql.DB, tx *sql.Tx) {
	t.Helper()
	waitForBlockedBy(ctx, t, db, tx, "匿名化停在钱包锁上：它没有按预期的顺序走到钱包")
}

// waitForBlockedBy 等到有别的后端被 tx 这一笔事务挡住；what 是超时时要说的那一句。
//
// pg_blocking_pids 对 advisory lock 同样有效：事务级咨询锁也是锁管理器里的锁。
func waitForBlockedBy(ctx context.Context, t *testing.T, db *sql.DB, tx *sql.Tx, what string) {
	t.Helper()
	var blocker int32
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatalf("读本事务的后端号失败: %v", err)
	}
	const query = `
		SELECT count(*) FROM pg_stat_activity
		 WHERE datname = current_database()
		   AND pid <> $1
		   AND $1 = ANY(pg_blocking_pids(pid))`
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, query, blocker).Scan(&waiting); err != nil {
			t.Fatalf("查被挡住的事务失败: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("10 秒内没看到%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPurgeCountsLegacyLedgerAccountForms 证明回执数的账本分录按
// UserIDFromAccount 的口径来。
//
// 旧写路径留下过两种写法：补零的 `user:042` 与带正号的 `user:+42`。它们与规范
// 写法指向同一个钱包，按字符串相等数会漏掉这两笔；只认补零、不认正号，第二种
// 照样漏。两种都要各写一笔，那一对判据才都有用例压着。
func TestPurgeCountsLegacyLedgerAccountForms(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	// 规范写法、补零写法、带正号写法各一笔（+1），平台侧各 -1，三笔都平衡。
	accounts := []string{
		ledger.UserAccount(u.ID).String(),
		fmt.Sprintf("user:0%d", u.ID),
		fmt.Sprintf("user:+%d", u.ID),
	}
	for _, account := range accounts {
		txnID := string(ledger.NewTxnID())
		if _, err := db.ExecContext(ctx, `
			INSERT INTO ledger_entries (txn_id, account, amount, entry_type, memo) VALUES
				($1, $2, 1, 'adjustment', '销户用例'),
				($1, $3, -1, 'adjustment', '销户用例')`,
			txnID, account, ledger.PlatformRevenueAccount().String()); err != nil {
			t.Fatalf("写账本分录失败: %v", err)
		}
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.WithoutCancel(ctx),
				`DELETE FROM ledger_entries WHERE txn_id = $1`, txnID); err != nil {
				t.Logf("清理账本分录 %s 失败: %v", txnID, err)
			}
		})
	}

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	result, err := store.PurgeDeletion(ctx, u.Username, u.Username)
	if err != nil {
		t.Fatalf("匿名化失败: %v", err)
	}
	if result.LedgerEntries != 3 {
		t.Errorf("回执说 %d 条分录，想要 3（规范写法、补零写法与带正号写法各一）",
			result.LedgerEntries)
	}

	// 这一条同时给 `ok && userID == row.id` 上那个存活变异（&& 改成 ||）留了
	// 等价说明：SQL 的预筛是 `^user:\+?0*<id>$`，能进这个循环的地址只可能是
	// 「user:」+ 可选正号 + 若干前导零 + row.id 的十进制写法，而 UserIDFromAccount
	// 把这一段正好读成 row.id。于是两版对每一行给出同一个布尔值：ok 为假时
	// 它返回的 userID 是 0，也等于不了 row.id（bigserial 从 1 起）。
}
