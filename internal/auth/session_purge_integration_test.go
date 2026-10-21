package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// 会话清理的集成测试。
//
// 判据全在库上：哪一类行算到期、留存窗口按哪一列算、数一遍与真删是不是同一
// 条判据——替身回答不了这些。
//
// 库是包内共用的，所以断言收窄到「本用例自己造的那四行」：先清一遍存量可清理
// 行，再断言计数等于 2，最后逐行核对谁没了、谁还在。对着全库数数会让别的用例
// 造的行把断言拖红（LESSONS.md 第 26 条）。

// purgeRetention 是本文件用的留存窗口：24 小时。
const purgeRetention = 24 * time.Hour

func TestPurgeSessionsDeletesExpiredAndOldRevokedOnly(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	// 先清掉存量：别的用例可能留下可清理的行，而下面要断言「第二次删 0 条」。
	if _, err := store.PurgeSessions(ctx, purgeRetention); err != nil {
		t.Fatalf("清存量失败: %v", err)
	}

	// 四类行：
	//   有效           过期还早、没被吊销        → 留着
	//   已过期         过期时刻已经过去          → 删
	//   窗口内已吊销   1 小时前吊销、还没过期     → 留着（排查「什么时候被踢的」）
	//   窗口外已吊销   48 小时前吊销、还没过期    → 删
	valid := insertSession(t, db, u.ID, "+168 hours", "")
	expired := insertSession(t, db, u.ID, "-1 second", "")
	revokedRecent := insertSession(t, db, u.ID, "+168 hours", "1 hour")
	revokedOld := insertSession(t, db, u.ID, "+168 hours", "48 hours")

	// 干跑与真删共用同一条判据：两个数必须相等。
	counted, err := store.CountPurgeableSessions(ctx, purgeRetention)
	if err != nil {
		t.Fatalf("数可清理会话失败: %v", err)
	}
	if counted != 2 {
		t.Errorf("可清理会话数 = %d，期望 2（已过期的 1 条 + 窗口外已吊销的 1 条）", counted)
	}

	deleted, err := store.PurgeSessions(ctx, purgeRetention)
	if err != nil {
		t.Fatalf("清理会话失败: %v", err)
	}
	if deleted != counted {
		t.Errorf("真删 %d 条、干跑数到 %d 条：两条语句的判据不一致", deleted, counted)
	}

	for _, tc := range []struct {
		name   string
		id     int64
		exists bool
	}{
		{"有效会话", valid, true},
		{"窗口内已吊销的会话", revokedRecent, true},
		{"已过期的会话", expired, false},
		{"窗口外已吊销的会话", revokedOld, false},
	} {
		if got := sessionExists(t, db, tc.id); got != tc.exists {
			t.Errorf("%s（id=%d）存在 = %v，期望 %v", tc.name, tc.id, got, tc.exists)
		}
	}

	// 重复跑：没有可删的行时返回 0，而不是错误。
	again, err := store.PurgeSessions(ctx, purgeRetention)
	if err != nil {
		t.Fatalf("第二次清理失败: %v", err)
	}
	if again != 0 {
		t.Errorf("第二次清理删除 %d 条，期望 0", again)
	}
	left, err := store.CountPurgeableSessions(ctx, purgeRetention)
	if err != nil {
		t.Fatalf("第二次数可清理会话失败: %v", err)
	}
	if left != 0 {
		t.Errorf("第二次清理之后还剩 %d 条可清理的行，期望 0", left)
	}
}

// 留存窗口按 revoked_at 算，不按 expires_at：两条吊销时刻相同、过期时刻不同的
// 行，必须同时被删或同时被留。
func TestPurgeSessionsWindowsOnRevokedAt(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	if _, err := store.PurgeSessions(ctx, purgeRetention); err != nil {
		t.Fatalf("清存量失败: %v", err)
	}

	// 都已吊销 48 小时，但一条还有很久才过期、另一条一小时后过期：两条都算
	// 窗口外，都该删。
	soon := insertSession(t, db, u.ID, "+1 hour", "48 hours")
	later := insertSession(t, db, u.ID, "+720 hours", "48 hours")

	if _, err := store.PurgeSessions(ctx, purgeRetention); err != nil {
		t.Fatalf("清理会话失败: %v", err)
	}
	for name, id := range map[string]int64{"一小时后过期的": soon, "很久以后过期的": later} {
		if sessionExists(t, db, id) {
			t.Errorf("%s 行同样吊销了 48 小时，应当被删掉", name)
		}
	}
}

// 非正的留存时长在数一遍与真删两处都要拒绝：窗口算成 0 时判据会匹配全部
// 已吊销的行，干跑报出一个很大的数、真删把它们全删掉。
func TestPurgeSessionsRefusesNonPositiveRetention(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	id := insertSession(t, db, u.ID, "+168 hours", "1 hour")
	for _, retention := range []time.Duration{0, -time.Hour} {
		if _, err := store.PurgeSessions(ctx, retention); err == nil {
			t.Errorf("留存时长 %s 时 PurgeSessions 应当报错", retention)
		}
		if _, err := store.CountPurgeableSessions(ctx, retention); err == nil {
			t.Errorf("留存时长 %s 时 CountPurgeableSessions 应当报错", retention)
		}
	}
	if !sessionExists(t, db, id) {
		t.Error("被拒绝的清理不该删掉任何行")
	}
}

// insertSession 造一行会话。expiresIn 是相对现在的时间段（如 "+168 hours"），
// revokedAge 为空表示没被吊销，否则是「多久以前吊销的」（如 "48 hours"）。
func insertSession(t *testing.T, db *sql.DB, userID int64, expiresIn, revokedAge string) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO user_sessions (user_id, token_hash, csrf_token, expires_at, revoked_at)
		VALUES ($1, $2, 'csrf-test',
		        now() + $3::interval,
		        CASE WHEN $4 = '' THEN NULL ELSE now() - $4::interval END)
		RETURNING id`,
		userID, hashSessionToken(uniqueUsername()), expiresIn, revokedAge).Scan(&id)
	if err != nil {
		t.Fatalf("造会话失败（expires %s / revoked %q）: %v", expiresIn, revokedAge, err)
	}
	return id
}

// sessionExists 报告一行会话还在不在。
func sessionExists(t *testing.T, db *sql.DB, id int64) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM user_sessions WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("查会话 %d 失败: %v", id, err)
	}
	return n > 0
}

// 不足一秒的留存期换算成秒是 0：拦不住的话判据退化成「删掉全部已吊销行」，
// 而干跑与真删用的是同一个截断值，所以「两数相等」那条用例看不出来。
func TestPurgeSessionsRefusesSubSecondRetention(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)
	store := New(db)

	// 1 小时前吊销的行：若判据退化，它会被 «revoked_at < now()» 扫掉。
	id := insertSession(t, db, u.ID, "+168 hours", "1 hour")

	if _, err := store.PurgeSessions(ctx, 500*time.Millisecond); err == nil {
		t.Error("500ms 的留存期会截断成 0 秒，PurgeSessions 应当报错")
	}
	if _, err := store.CountPurgeableSessions(ctx, 500*time.Millisecond); err == nil {
		t.Error("500ms 的留存期会截断成 0 秒，CountPurgeableSessions 应当报错")
	}
	if !sessionExists(t, db, id) {
		t.Error("被拒绝的清理不该删掉任何行")
	}
}
