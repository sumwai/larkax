package auth

import (
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"
)

// 会话与登录节流的集成测试。
//
// 这些性质全部来自库：token_hash 的唯一约束与 bytea 口径、JOIN users 之后
// 读到的状态、UPDATE 的自增与 make_interval 算出来的锁定时刻、以及
// 「一条语句同时改口令与吊销会话」的事务边界。替身回答不了这些问题。

func TestCreateSessionStoresHashOnly(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	session, token, err := store.CreateSession(ctx, u.ID, "test-agent", nil)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	if token == "" || session.CSRFToken == "" {
		t.Fatal("令牌或 CSRF 令牌为空")
	}
	if session.UserID != u.ID || session.ExpiresAt.IsZero() {
		t.Fatalf("会话字段不对: %+v", session)
	}

	// 库里存的是 SHA-256，不是明文：明文令牌出现在库里就等于它已经泄露。
	var stored []byte
	var plain int
	if queryErr := db.QueryRowContext(ctx,
		`SELECT token_hash, (SELECT count(*) FROM user_sessions WHERE id = $1 AND token_hash::text = $2)
		   FROM user_sessions WHERE id = $1`, session.ID, token,
	).Scan(&stored, &plain); queryErr != nil {
		t.Fatalf("读会话行失败: %v", queryErr)
	}
	sum := sha256.Sum256([]byte(token))
	if string(stored) != string(sum[:]) {
		t.Error("token_hash 不是令牌的 SHA-256")
	}
	if plain != 0 {
		t.Error("库里出现了明文令牌")
	}

	// 按令牌读回来，带上 JOIN 出来的用户状态。
	got, err := store.SessionByToken(ctx, token)
	if err != nil {
		t.Fatalf("SessionByToken 失败: %v", err)
	}
	if got.ID != session.ID || got.UserStatus != UserStatusActive {
		t.Errorf("读回的会话 = %+v", got)
	}
	if _, err := store.SessionByToken(ctx, token+"x"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("错误令牌应当报 ErrSessionNotFound，得到 %v", err)
	}
}

func TestSessionRevokeAndUserStatus(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	session, token, err := store.CreateSession(ctx, u.ID, "", nil)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	if revokeErr := store.RevokeSession(ctx, session.ID); revokeErr != nil {
		t.Fatalf("吊销失败: %v", revokeErr)
	}
	// 幂等：再吊销一次不报错。
	if revokeErr := store.RevokeSession(ctx, session.ID); revokeErr != nil {
		t.Fatalf("重复吊销应当幂等，得到 %v", revokeErr)
	}

	got, err := store.SessionByToken(ctx, token)
	if err != nil {
		t.Fatalf("SessionByToken 失败: %v", err)
	}
	if got.RevokedAt == nil {
		t.Fatal("吊销之后 RevokedAt 仍然是 nil")
	}
	if usableErr := got.Usable(time.Now().UTC()); usableErr == nil {
		t.Fatal("已吊销的会话被判成可用")
	}

	// 用户被封禁：会话行本身没变，但 Usable 要拒。
	other, otherToken, err := store.CreateSession(ctx, u.ID, "", nil)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	_ = other
	if _, execErr := db.ExecContext(ctx, `UPDATE users SET status = $2 WHERE id = $1`,
		u.ID, UserStatusBanned); execErr != nil {
		t.Fatalf("封禁用户失败: %v", execErr)
	}
	banned, err := store.SessionByToken(ctx, otherToken)
	if err != nil {
		t.Fatalf("SessionByToken 失败: %v", err)
	}
	if banned.UserStatus != UserStatusBanned {
		t.Errorf("UserStatus = %q，期望 banned", banned.UserStatus)
	}
	if bannedErr := banned.Usable(time.Now().UTC()); bannedErr == nil {
		t.Fatal("封禁用户的会话被判成可用")
	}
}

func TestTouchSessionExtendsIdleButNotAbsolute(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	session, token, err := store.CreateSession(ctx, u.ID, "", nil)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	// 把这一行挪到「一天前建的、last_seen 也旧了」，再 touch。
	createdAt := time.Now().UTC().Add(-24 * time.Hour)
	if _, execErr := db.ExecContext(ctx, `
		UPDATE user_sessions SET created_at = $2::timestamptz, last_seen_at = $2::timestamptz - interval '5 minutes'
		 WHERE id = $1`, session.ID, createdAt); execErr != nil {
		t.Fatalf("改会话时刻失败: %v", execErr)
	}
	session.CreatedAt = createdAt

	now := time.Now().UTC()
	if touchErr := store.TouchSession(ctx, session, now); touchErr != nil {
		t.Fatalf("TouchSession 失败: %v", touchErr)
	}
	got, err := store.SessionByToken(ctx, token)
	if err != nil {
		t.Fatalf("SessionByToken 失败: %v", err)
	}
	if !got.LastSeenAt.After(createdAt) {
		t.Errorf("last_seen_at 没有刷新: %s", got.LastSeenAt)
	}
	if d := got.ExpiresAt.Sub(now); d < SessionIdleTTL-time.Minute || d > SessionIdleTTL+time.Minute {
		t.Errorf("expires_at 距 now %s，期望约 %s", d, SessionIdleTTL)
	}

	// 绝对窗：建了 29 天 20 小时之后 touch，空闲窗要往后推，但封顶在
	// created_at + 30 天。
	oldCreated := time.Now().UTC().Add(-SessionAbsoluteTTL + 4*time.Hour)
	if _, execErr := db.ExecContext(ctx, `
		UPDATE user_sessions SET created_at = $2::timestamptz, last_seen_at = $2::timestamptz - interval '5 minutes'
		 WHERE id = $1`, session.ID, oldCreated); execErr != nil {
		t.Fatalf("改会话时刻失败: %v", execErr)
	}
	session.CreatedAt = oldCreated
	if touchErr := store.TouchSession(ctx, session, time.Now().UTC()); touchErr != nil {
		t.Fatalf("TouchSession 失败: %v", touchErr)
	}
	var expiresAt time.Time
	if queryErr := db.QueryRowContext(ctx,
		`SELECT expires_at FROM user_sessions WHERE id = $1`, session.ID).Scan(&expiresAt); queryErr != nil {
		t.Fatalf("读 expires_at 失败: %v", queryErr)
	}
	absolute := oldCreated.Add(SessionAbsoluteTTL)
	if expiresAt.Sub(absolute).Abs() > time.Minute {
		t.Errorf("expires_at = %s，应当封顶在 %s", expiresAt, absolute)
	}
}

func TestSetPasswordRevokesOtherSessions(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	current, _, err := store.CreateSession(ctx, u.ID, "", nil)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	other, otherToken, err := store.CreateSession(ctx, u.ID, "", nil)
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	hash, err := HashPassword(testKDF, "correct horse battery staple")
	if err != nil {
		t.Fatalf("算口令哈希失败: %v", err)
	}
	if setErr := store.SetPassword(ctx, u.ID, hash, &current.ID); setErr != nil {
		t.Fatalf("SetPassword 失败: %v", setErr)
	}

	otherAfter, err := store.SessionByToken(ctx, otherToken)
	if err != nil {
		t.Fatalf("SessionByToken 失败: %v", err)
	}
	if otherAfter.RevokedAt == nil {
		t.Errorf("被吊销的会话 %d 仍然是有效的", other.ID)
	}

	var revokedCurrent bool
	if queryErr := db.QueryRowContext(ctx,
		`SELECT revoked_at IS NOT NULL FROM user_sessions WHERE id = $1`,
		current.ID).Scan(&revokedCurrent); queryErr != nil {
		t.Fatalf("读当前会话失败: %v", queryErr)
	}
	if revokedCurrent {
		t.Error("keep 指定的那一行也被吊销了")
	}

	// 口令也写进去了。
	login, err := store.LoginUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("LoginUserByID 失败: %v", err)
	}
	result, err := VerifyPassword(testKDF, login.PasswordHash, "correct horse battery staple")
	if err != nil || !result.OK {
		t.Errorf("新口令验证失败: %v / %+v", err, result)
	}

	// 渐进重哈希不动会话：它不是一次口令变更。
	if err := store.RehashPassword(ctx, u.ID, hash); err != nil {
		t.Fatalf("RehashPassword 失败: %v", err)
	}
	if queryErr := db.QueryRowContext(ctx,
		`SELECT revoked_at IS NOT NULL FROM user_sessions WHERE id = $1`,
		current.ID).Scan(&revokedCurrent); queryErr != nil {
		t.Fatalf("读当前会话失败: %v", queryErr)
	}
	if revokedCurrent {
		t.Error("重哈希把会话吊销了")
	}
}

func TestRecordLoginFailureIsAtomic(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	const attempts = 12
	var wg sync.WaitGroup
	errs := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.RecordLoginFailure(ctx, u.ID, time.Now().UTC()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发记录失败: %v", err)
	}

	var (
		count  int
		locked *time.Time
	)
	if err := db.QueryRowContext(ctx,
		`SELECT failed_login_count, locked_until FROM users WHERE id = $1`, u.ID,
	).Scan(&count, &locked); err != nil {
		t.Fatalf("读计数失败: %v", err)
	}
	// 一条 UPDATE 自己读自己写进去的值，所以并发不会丢计数。
	if count != attempts {
		t.Errorf("失败计数 = %d，期望 %d", count, attempts)
	}
	if locked == nil {
		t.Fatal("连续失败超过阈值后没有锁定")
	}

	// 清零之后锁定也没了。
	if err := store.ClearLoginFailures(ctx, u.ID); err != nil {
		t.Fatalf("ClearLoginFailures 失败: %v", err)
	}
	var stillLocked bool
	if err := db.QueryRowContext(ctx,
		`SELECT failed_login_count, locked_until IS NULL FROM users WHERE id = $1`, u.ID,
	).Scan(&count, &stillLocked); err != nil {
		t.Fatalf("读计数失败: %v", err)
	}
	if count != 0 {
		t.Errorf("清零之后计数 = %d", count)
	}
	if !stillLocked {
		t.Error("清零之后仍然锁定")
	}
}

func TestUnlockUser(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	for i := 0; i < loginLockThreshold+1; i++ {
		if _, _, err := store.RecordLoginFailure(ctx, u.ID, time.Now().UTC()); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}
	info, err := store.UnlockUser(ctx, u.Username)
	if err != nil {
		t.Fatalf("UnlockUser 失败: %v", err)
	}
	if info.Username != u.Username {
		t.Errorf("解锁返回的用户 = %q", info.Username)
	}
	login, err := store.LoginUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("LoginUserByID 失败: %v", err)
	}
	if login.FailedLoginCount != 0 || login.LockedUntil != nil {
		t.Errorf("解锁之后 = %+v", login)
	}
}

// TestSetPasswordRevokesOtherSessions 同时覆盖了「吊销别人、留住当前」这一条：
// 那条 SQL 在改口令的事务里（见 account.go 的 SetPassword），没有单独导出的
// 批量吊销入口。

// TestSessionCarriesRoleFromFlags 钉住会话读回来的角色与库里的两个标记一致
// （三种取值各一条）。
//
// 角色必须与控制台路由判定出自同一次读：分两次查的话，两次之间用户被降级时，
// 这个请求会按旧角色放行。
func TestSessionCarriesRoleFromFlags(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	cases := []struct {
		name     string
		merchant bool
		admin    bool
		want     Role
	}{
		{"user", false, false, RoleUser},
		{"merchant", true, false, RoleMerchant},
		{"admin", true, true, RoleAdmin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUser(ctx, t, db, UserStatusActive)
			if _, err := db.ExecContext(ctx,
				`UPDATE users SET is_merchant = $2, is_admin = $3 WHERE id = $1`,
				u.ID, tc.merchant, tc.admin); err != nil {
				t.Fatalf("写角色标记失败: %v", err)
			}
			_, token, err := store.CreateSession(ctx, u.ID, "", nil)
			if err != nil {
				t.Fatalf("建会话失败: %v", err)
			}
			got, err := store.SessionByToken(ctx, token)
			if err != nil {
				t.Fatalf("SessionByToken 失败: %v", err)
			}
			if got.Role != tc.want {
				t.Errorf("会话角色 = %q，期望 %q", got.Role, tc.want)
			}
		})
	}
}
