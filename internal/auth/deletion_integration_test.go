package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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

func seedWallet(t *testing.T, db *sql.DB, userID int64, balance, frozen string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO user_wallets (user_id, asset, balance, frozen)
		VALUES ($1, 'CNY', $2::numeric, $3::numeric)`, userID, balance, frozen,
	); err != nil {
		t.Fatalf("建钱包失败: %v", err)
	}
}

func seedLedgerEntry(t *testing.T, db *sql.DB, userID int64, txnID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO ledger_entries (txn_id, account, amount, entry_type, memo)
		VALUES ($1, $2, 1, 'adjustment', '销户用例')`,
		txnID, fmt.Sprintf("user:%d", userID),
	); err != nil {
		t.Fatalf("写账本分录失败: %v", err)
	}
}

// TestDeletionRequestBlocksKeysAndRevertRestoresThem 证明「打标可撤」这件事的
// 两端：打标之后同一个 key 用不了，撤销之后它还是那一把、还能用。
//
// 只断言 status 是不够的：一个顺手把 key 也作废了的实现，status 会照常回到
// active，而用户手里的 key 已经永久失效——那正是「可撤」要避免的。
func TestDeletionRequestBlocksKeysAndRevertRestoresThem(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)
	u := newUser(ctx, t, db, UserStatusActive)

	created, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	authOK := func() error {
		_, err := store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext))
		return err
	}

	if err := authOK(); err != nil {
		t.Fatalf("刚创建的 key 就认证失败: %v", err)
	}

	mark, err := store.RequestDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("打标失败: %v", err)
	}
	if mark.FromStatus != UserStatusActive || mark.ToStatus != UserStatusDeletionRequested {
		t.Errorf("打标的两个状态是 %s → %s，想要 active → deletion_requested",
			mark.FromStatus, mark.ToStatus)
	}

	if err := authOK(); err == nil {
		t.Fatal("打标之后 key 还能用")
	} else if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("打标之后的失败不是「凭证无效」那一类: %v", err)
	}

	var keyStatus string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM api_keys WHERE id = $1`, created.ID).Scan(&keyStatus); err != nil {
		t.Fatalf("读 key 状态失败: %v", err)
	}
	if keyStatus != keyStatusActive {
		t.Errorf("打标把 key 的状态改成了 %q：撤销之后它就回不来了", keyStatus)
	}

	mark, err = store.RevertDeletion(ctx, u.Username)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if mark.FromStatus != UserStatusDeletionRequested || mark.ToStatus != UserStatusActive {
		t.Errorf("撤销的两个状态是 %s → %s，想要 deletion_requested → active",
			mark.FromStatus, mark.ToStatus)
	}
	if err := authOK(); err != nil {
		t.Fatalf("撤销之后原来的 key 不能用: %v", err)
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
			seedWallet(t, db, u.ID, tc.balance, tc.frozen)

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
	seedWallet(t, db, u.ID, "0", "0")

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
	seedWallet(t, db, u.ID, "0", "0")
	seedLedgerEntry(t, db, u.ID, "00000000-0000-8000-8000-000000000001")
	seedLedgerEntry(t, db, u.ID, "00000000-0000-8000-8000-000000000002")

	// 两把 key：一把 active、一把此前就 disabled。前者要被作废，后者保留它
	// 原来的状态——「此前停用过」也是一条该留下的记录。
	active, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("创建 active key 失败: %v", err)
	}
	disabled, err := store.Create(ctx, CreateParams{UserID: u.ID})
	if err != nil {
		t.Fatalf("创建 disabled key 失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE api_keys SET status = 'disabled' WHERE id = $1`, disabled.ID); err != nil {
		t.Fatalf("停用 key 失败: %v", err)
	}

	// 两条用户态插件 KV、两条采集授权（一条有效、一条早就撤销过）。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plugin_kv (plugin_key, user_id, key, value) VALUES
			('demo', $1, 'a', '1'), ('demo', $1, 'b', '2')`, u.ID); err != nil {
		t.Fatalf("写插件 KV 失败: %v", err)
	}
	const revokedAt = "2026-10-01T00:00:00Z"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO capture_consents
			(user_id, scope, request_id, consent_text, consent_version, expires_at, revoked_at)
		VALUES ($1, 'single_request', 'req-1', '同意', 1, now() + interval '1 day', NULL),
		       ($1, 'single_request', 'req-2', '同意', 1, now() + interval '1 day', $2::timestamptz)`,
		u.ID, revokedAt); err != nil {
		t.Fatalf("写采集授权失败: %v", err)
	}

	if _, err := store.RequestDeletion(ctx, u.Username); err != nil {
		t.Fatalf("打标失败: %v", err)
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
	var ledgerRows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE account = $1`,
		fmt.Sprintf("user:%d", u.ID)).Scan(&ledgerRows); err != nil {
		t.Fatalf("统计账本分录失败: %v", err)
	}
	if ledgerRows != 2 || result.LedgerEntries != 2 {
		t.Errorf("账本分录 = %d 条（回执说 %d 条），想要 2", ledgerRows, result.LedgerEntries)
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
