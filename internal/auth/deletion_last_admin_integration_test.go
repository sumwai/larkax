package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件钉住销户路径上的「最后一个 active 管理员」判据（#969）。
//
// 判据与 UpdateUser 共用同一个辅助函数：打标（RequestDeletion）与匿名化
// （PurgeDeletion）两道门都要过它，撤销（RevertDeletion）不挂——撤销让管理员
// 回来，方向是安全的。
//
// 它必须在真库上跑：判据是「锁内 count + 事务级咨询锁」，替身既模拟不出行锁，
// 也证明不了两笔并发里只有一笔成功。没有 LARKAX_DB_DSN 时整包跳过（见 testDB）。

// readUserStatusState 读回一行的状态与「打标之前是什么」，用于断言拒绝之后
// 一个字段都没动。
func readUserStatusState(t *testing.T, store *Store, id int64) (status string, prior string) {
	t.Helper()
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT status, coalesce(deletion_prior_status, '') FROM users WHERE id = $1`,
		id).Scan(&status, &prior); err != nil {
		t.Fatalf("读回用户 %d 失败: %v", id, err)
	}
	return status, prior
}

func TestRequestDeletionRejectsLastActiveAdmin(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)

	admin := createAdmin(t, store)

	_, err := store.RequestDeletion(ctx, fmt.Sprint(admin.ID))
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("对最后一个管理员的销户请求得到 %v，期望 ErrLastAdmin", err)
	}
	// 文案要指出下一步该做什么，而不只是「不允许」。
	if !strings.Contains(err.Error(), "另一个账号") {
		t.Errorf("拒绝文案没点出「先给另一个账号管理员权限」: %v", err)
	}

	// 被挡下之后那一行不该动：判据在写之前，连 deletion_prior_status 都不该有值。
	status, prior := readUserStatusState(t, store, admin.ID)
	if status != UserStatusActive || prior != "" {
		t.Errorf("被挡下之后是 (status=%q, prior=%q)，期望 (%q, 空)",
			status, prior, UserStatusActive)
	}
}

func TestRequestDeletionAllowsWhenAnotherActiveAdminExists(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)

	first := createAdmin(t, store)
	createAdmin(t, store)

	mark, err := store.RequestDeletion(ctx, fmt.Sprint(first.ID))
	if err != nil {
		t.Fatalf("还有另一个管理员时销户请求失败: %v", err)
	}
	if mark.ToStatus != UserStatusDeletionRequested {
		t.Errorf("打标之后状态 = %q，期望 %q", mark.ToStatus, UserStatusDeletionRequested)
	}
}

// 匿名化是最后一道门：绕过打标、或者打标之后另一个管理员又没了，都不能把
// 唯一的管理员匿名化掉——那一步不可逆。
func TestPurgeDeletionRejectsLastActiveAdmin(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)

	admin := createAdmin(t, store)
	peer := createAdmin(t, store)

	// 此刻还有 peer，打标放行。
	if _, err := store.RequestDeletion(ctx, fmt.Sprint(admin.ID)); err != nil {
		t.Fatalf("还有另一个管理员时打标失败: %v", err)
	}

	// 把 peer 停掉：admin 现在虽是 deletion_requested，但已是唯一还挂着管理员的
	// 那一行。不走 UpdateUser 造这一步：它自己对「最后一个管理员」会拒绝，而这里
	// 要的是匿名化那道门单独成立。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE users SET status = $2 WHERE id = $1`, peer.ID, UserStatusSuspended); err != nil {
		t.Fatalf("停用第二个管理员失败: %v", err)
	}

	_, err := store.PurgeDeletion(ctx, fmt.Sprint(admin.ID), admin.Username)
	if !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("匿名化唯一管理员得到 %v，期望 ErrLastAdmin", err)
	}
	if !strings.Contains(err.Error(), "另一个账号") {
		t.Errorf("拒绝文案没点出「先给另一个账号管理员权限」: %v", err)
	}

	// 匿名化不可逆：被挡下之后用户名与状态都得原样，deleted_at 仍为空。
	var username, status string
	var deleted bool
	if err := store.db.QueryRowContext(ctx,
		`SELECT username, status, deleted_at IS NOT NULL FROM users WHERE id = $1`,
		admin.ID).Scan(&username, &status, &deleted); err != nil {
		t.Fatalf("读回管理员失败: %v", err)
	}
	if username != admin.Username || status != UserStatusDeletionRequested || deleted {
		t.Errorf("被挡下之后是 (username=%q, status=%q, deleted=%v)，期望原样",
			username, status, deleted)
	}
}

// 打标之前是 suspended 的管理员，匿名化不再按「最后一个管理员」拦。
//
// 判据保护的是「还能进管理组」的那一拨：suspended / banned 的管理员本来就不在
// 其中，撤掉它不会让谁失去入口。打标那一侧对它们不拦，所以这种行是正常能造
// 出来的；在匿名化这一侧再拦一次，效果只是一个已经没人能登的账号永远清不掉。
//
// 现场是一行**真的被打了标的 suspended 管理员**：先建管理员、把它置成
// suspended，再走 RequestDeletion（那一侧对非 active 管理员不拦），于是
// deletion_prior_status 记下的是 suspended。不是只调辅助函数。
func TestPurgeDeletionAllowsSuspendedAdminWithoutActiveAdmin(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)

	admin := createAdmin(t, store)
	// 停用走 SQL：UpdateUser 对「撤掉最后一个 active 管理员」有同一道判据，
	// 这里要的正是「库里已经没有 active 管理员」这个起点。
	if _, err := store.db.ExecContext(ctx,
		`UPDATE users SET status = $2 WHERE id = $1`, admin.ID, UserStatusSuspended); err != nil {
		t.Fatalf("停用管理员失败: %v", err)
	}

	if _, err := store.RequestDeletion(ctx, fmt.Sprint(admin.ID)); err != nil {
		t.Fatalf("对 suspended 管理员打标失败: %v", err)
	}
	status, prior := readUserStatusState(t, store, admin.ID)
	if status != UserStatusDeletionRequested || prior != UserStatusSuspended {
		t.Fatalf("打标之后是 (status=%q, prior=%q)，期望 (%q, %q)",
			status, prior, UserStatusDeletionRequested, UserStatusSuspended)
	}

	if _, err := store.PurgeDeletion(ctx, fmt.Sprint(admin.ID), admin.Username); err != nil {
		t.Fatalf("匿名化销户前已停用的管理员失败: %v", err)
	}

	// 终态：整个事务走完了，不是停在 deletion_requested 上的半截。
	var (
		afterStatus, afterPrior string
		deleted                 bool
	)
	if err := store.db.QueryRowContext(ctx, `
		SELECT status, coalesce(deletion_prior_status, ''), deleted_at IS NOT NULL
		  FROM users WHERE id = $1`, admin.ID,
	).Scan(&afterStatus, &afterPrior, &deleted); err != nil {
		t.Fatalf("读回匿名化之后的行失败: %v", err)
	}
	if afterStatus != UserStatusDeleted || afterPrior != "" || !deleted {
		t.Errorf("匿名化之后是 (status=%q, prior=%q, deleted=%v)，期望 (%q, 空, true)",
			afterStatus, afterPrior, deleted, UserStatusDeleted)
	}
}

// 撤销不挂判据：让管理员回来是安全的方向。这里直接把行造成 deletion_requested，
// 因为打标那条路现在会被判据挡住，而历史数据与将来的别的入口仍可能把行落到
// 这个状态。
func TestRevertDeletionLetsLastActiveAdminBack(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)

	admin := createAdmin(t, store)
	if _, err := store.db.ExecContext(ctx, `
		UPDATE users SET status = $2, deletion_prior_status = $3, updated_at = now()
		 WHERE id = $1`,
		admin.ID, UserStatusDeletionRequested, UserStatusActive); err != nil {
		t.Fatalf("造 deletion_requested 失败: %v", err)
	}

	mark, err := store.RevertDeletion(ctx, fmt.Sprint(admin.ID))
	if err != nil {
		t.Fatalf("唯一管理员撤销销户失败: %v", err)
	}
	if mark.ToStatus != UserStatusActive {
		t.Errorf("撤销之后状态 = %q，期望 %q", mark.ToStatus, UserStatusActive)
	}
	status, prior := readUserStatusState(t, store, admin.ID)
	if status != UserStatusActive || prior != "" {
		t.Errorf("撤销之后是 (status=%q, prior=%q)，期望 (%q, 空)",
			status, prior, UserStatusActive)
	}
}

// 并发：两笔事务同时把「仅剩的两个管理员」都打标，至少一笔要被拒，
// 平台上始终留一个 active 管理员。
//
// 只有起跑信号不算同步点：调度慢一点就会有一笔先提交，另一笔照样被拒，
// 用例于是绿，但证明不了咨询锁有用——把 ensureAnotherActiveAdmin 里那句
// pg_advisory_xact_lock 删掉它还是绿。所以用触发器把 T1 那条 UPDATE 在写之前
// 停住 1.5 秒，T1 已经取到咨询锁、行未提交；T2 在这段时间里进来。缺锁时 T2
// 数得到「除自己外还有 1 个 active 管理员」→ 放行 → 两笔都成功 → 本条红。
const (
	lastAdminDeletionWindowFunction = "test_last_admin_deletion_window_fn"
	lastAdminDeletionWindowTrigger  = "test_last_admin_deletion_window"
	lastAdminDeletionUserPrefix     = "last-admin-deletion-"
)

func installDeletionWindow(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	stmts := []string{
		`CREATE OR REPLACE FUNCTION ` + lastAdminDeletionWindowFunction + `() RETURNS trigger
		   LANGUAGE plpgsql AS $fn$
		 BEGIN
		   PERFORM pg_sleep(1.5);
		   RETURN NEW;
		 END
		 $fn$`,
		`DROP TRIGGER IF EXISTS ` + lastAdminDeletionWindowTrigger + ` ON users`,
		`CREATE TRIGGER ` + lastAdminDeletionWindowTrigger + `
		   BEFORE UPDATE ON users
		   FOR EACH ROW
		   WHEN (NEW.status = '` + UserStatusDeletionRequested + `'
		         AND OLD.status <> '` + UserStatusDeletionRequested + `'
		         AND OLD.username::text LIKE '` + lastAdminDeletionUserPrefix + `%')
		   EXECUTE FUNCTION ` + lastAdminDeletionWindowFunction + `()`,
	}
	for _, stmt := range stmts {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("安装并发窗口触发器失败: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := store.db.ExecContext(ctx,
			`DROP TRIGGER IF EXISTS `+lastAdminDeletionWindowTrigger+` ON users`); err != nil {
			t.Logf("删除窗口触发器失败: %v", err)
		}
		if _, err := store.db.ExecContext(ctx,
			`DROP FUNCTION IF EXISTS `+lastAdminDeletionWindowFunction+`()`); err != nil {
			t.Logf("删除窗口函数失败: %v", err)
		}
	})
}

// createDeletionWindowAdmin 建一个用户名带窗口前缀的 admin；触发器只认这个前缀。
func createDeletionWindowAdmin(t *testing.T, store *Store) User {
	t.Helper()
	username := lastAdminDeletionUserPrefix + uniqueUsername()
	created, err := store.CreateUser(t.Context(), CreateUserParams{
		Username: username,
		Email:    username + "@example.com",
		Role:     RoleAdmin,
	})
	if err != nil {
		t.Fatalf("建窗口 admin 失败: %v", err)
	}
	cleanupUsers(t.Context(), t, store.db, created.ID)
	return created
}

// waitForDeletionWindow 等到 T1 停在那条打标 UPDATE 的触发器里。
func waitForDeletionWindow(t *testing.T, store *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if deletionWindowOpen(t, store) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("10 秒内没看到 T1 停在触发器窗口里：触发器没建成，或 pg_sleep 没在跑")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForAdvisoryLockWait 看后到的那笔有没有停在管理员判据那把咨询锁上。
//
// 判据钉到锁键上：pg_locks 里只认 objsubid = 1（单 bigint 形式）、没有授予、
// 且键等于 hashtextextended(usersUpdateLockKey, 0) 的那一把。只判「有人在等
// 任意 advisory 锁」的话，这个包里哪天再起一个等锁的用例，就会把它顶成绿。
//
// 退出条件跟着触发器窗口走：窗口一关（先到的那笔提交了）还没看见等待，就说明
// 后到的那笔没有停在这把锁上。不用一个比剩余窗口还短的固定上限——那样会把
// 「还没来得及」误报成「没停住」。十秒那一档只在窗口一直不关（触发器挂了）时
// 才用得上。
func waitForAdvisoryLockWait(t *testing.T, store *Store) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := store.db.QueryRowContext(t.Context(), `
			SELECT count(*)
			  FROM pg_locks l
			  JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE l.locktype = 'advisory'
			   AND l.objsubid = 1
			   AND NOT l.granted
			   AND ((l.classid::bigint << 32) | l.objid::bigint) = hashtextextended($1, 0)
			   AND a.datname = current_database()`, usersUpdateLockKey).Scan(&waiting); err != nil {
			t.Fatalf("查看 pg_locks 失败: %v", err)
		}
		if waiting > 0 {
			return true
		}

		if !deletionWindowOpen(t, store) {
			return false
		}
		if time.Now().After(deadline) {
			t.Fatalf("10 秒内没看见咨询锁上的等待，而触发器窗口一直开着：信号取不到")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// deletionWindowOpen 报告先到的那笔是不是还睡在打标触发器里。
func deletionWindowOpen(t *testing.T, store *Store) bool {
	t.Helper()
	var n int
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database()
		    AND state = 'active'
		    AND wait_event_type = 'Timeout'
		    AND query LIKE '%UPDATE users%'`).Scan(&n); err != nil {
		t.Fatalf("查看 pg_stat_activity 失败: %v", err)
	}
	return n > 0
}

func TestRequestDeletionConcurrentLeavesOneActiveAdmin(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)
	installDeletionWindow(t, store)

	// 并发那条判据的成立前提之一：第二笔拿到咨询锁之后要能看见第一笔已提交的
	// 打标。READ COMMITTED 下每条语句取新快照，所以看得见；REPEATABLE READ 下
	// 计数用的是事务快照，两笔都会数到对方还 active，于是都放行。这条来自
	// 服务端配置而不是代码，所以在这里断一次——哪天有人改了配置先红在这里。
	var isolation string
	if err := store.db.QueryRowContext(ctx, `SHOW default_transaction_isolation`).Scan(&isolation); err != nil {
		t.Fatalf("读隔离级别失败: %v", err)
	}
	if isolation != "read committed" {
		t.Fatalf("默认隔离级别是 %q，期望 read committed：并发判据依赖每条语句取新快照", isolation)
	}

	first := createDeletionWindowAdmin(t, store)
	second := createDeletionWindowAdmin(t, store)

	firstDone := make(chan error, 1)
	go func() {
		_, err := store.RequestDeletion(ctx, fmt.Sprint(first.ID))
		firstDone <- err
	}()

	// 记下被拒那一笔的原始行，用来断「逐列未变」。两笔开始时都是 active 管理员。
	var secondUpdatedBefore time.Time
	if err := store.db.QueryRowContext(ctx,
		`SELECT updated_at FROM users WHERE id = $1`, second.ID).Scan(&secondUpdatedBefore); err != nil {
		t.Fatalf("读第二笔的 updated_at 失败: %v", err)
	}

	waitForDeletionWindow(t, store)

	// 后到的那笔放到另一个 goroutine：它要停在咨询锁上，主 goroutine 得能腾出
	// 手来看这件事。先到的那笔此刻握着那把锁，正睡在触发器里。
	secondDone := make(chan error, 1)
	go func() {
		_, err := store.RequestDeletion(ctx, fmt.Sprint(second.ID))
		secondDone <- err
	}()

	// 停在咨询锁上是要钉住的东西之一。只有「起跑信号 + 1.5 秒窗口」时，删掉
	// pg_advisory_xact_lock 两笔仍会一先一后地跑，用例可能照样绿——那时它证明的
	// 只是「后跑的那笔看见了先跑的」，不是「两笔被这把锁串起来了」。
	sawAdvisoryWait := waitForAdvisoryLockWait(t, store)

	secondErr := <-secondDone
	firstErr := <-firstDone

	// 恰好一笔成功：只断「还剩一个」的话，两笔都被拒也绿——那样证明不了
	// 守卫放行了该放行的那一笔。
	successes := 0
	if firstErr == nil {
		successes++
	}
	if secondErr == nil {
		successes++
	}
	if successes != 1 {
		t.Fatalf("两笔并发打标成功了 %d 笔（firstErr=%v, secondErr=%v），期望恰好 1 笔",
			successes, firstErr, secondErr)
	}
	if firstErr != nil {
		t.Fatalf("先到的那笔应当成功，实际 %v", firstErr)
	}
	if !errors.Is(secondErr, ErrLastAdmin) {
		t.Fatalf("后起的那笔得到 %v，期望 ErrLastAdmin", secondErr)
	}

	// 放在上面两条之后：锁真的被去掉时，先红在「恰好一笔成功」，那一格说的是
	// 判据失效；这一格只在判据仍然成立、但看不到它靠的那把锁时才有意义。
	if !sawAdvisoryWait {
		t.Fatalf("后到的那笔没有停在咨询锁上：锁没取，或者它在窗口里先跑完了")
	}

	// 被拒那一笔一个字段都没动：status、deletion_prior_status、deleted_at、
	// updated_at 全保持原样。只断错误值的话，「先写了一半再回滚失败」也会绿。
	var (
		status, prior string
		deleted       bool
		updatedAfter  time.Time
	)
	if err := store.db.QueryRowContext(ctx,
		`SELECT status, coalesce(deletion_prior_status, ''), deleted_at IS NOT NULL, updated_at
		   FROM users WHERE id = $1`, second.ID,
	).Scan(&status, &prior, &deleted, &updatedAfter); err != nil {
		t.Fatalf("读回被拒的那一行失败: %v", err)
	}
	if status != UserStatusActive || prior != "" || deleted || !updatedAfter.Equal(secondUpdatedBefore) {
		t.Errorf("被拒的那一行是 (status=%q, prior=%q, deleted=%v, updated_at=%s)，期望原样",
			status, prior, deleted, updatedAfter)
	}

	// 成功那笔真的落库了。
	firstStatus, firstPrior := readUserStatusState(t, store, first.ID)
	if firstStatus != UserStatusDeletionRequested || firstPrior != UserStatusActive {
		t.Errorf("成功的那一行是 (status=%q, prior=%q)，期望 (%q, %q)",
			firstStatus, firstPrior, UserStatusDeletionRequested, UserStatusActive)
	}

	// 收尾对账：成功笔数 + 最终 active 数 = 开始时的 active 数（2）。
	var left int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE id IN ($1, $2) AND is_admin AND status = $3`,
		first.ID, second.ID, UserStatusActive).Scan(&left); err != nil {
		t.Fatalf("统计剩下的管理员失败: %v", err)
	}
	if left != 1 {
		t.Errorf("并发打标之后这两个账号里还剩 %d 个 active 管理员，期望 1 个", left)
	}
	if successes+int(left) != 2 {
		t.Errorf("成功笔数 %d + 最终 active 数 %d ≠ 开始时的 2", successes, left)
	}
}
