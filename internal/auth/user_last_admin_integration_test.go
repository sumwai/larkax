package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// 本文件钉住「最后一个 active 管理员」这条判据，以及它的并发形态。
//
// 它必须在真库上跑：判据本身是两条 SQL（目标行 FOR UPDATE + 咨询锁下的
// count），单测替身既模拟不出行锁，也证明不了两笔并发里只有一笔成功。

// createAdmin 建一个 admin 并登记收尾清理。
func createAdmin(t *testing.T, store *Store) User {
	t.Helper()
	created, err := store.CreateUser(t.Context(), CreateUserParams{
		Username: uniqueUsername(),
		Email:    uniqueUsername() + "@example.com",
		Role:     RoleAdmin,
	})
	if err != nil {
		t.Fatalf("建 admin 失败: %v", err)
	}
	cleanupUsers(t.Context(), t, store.db, created.ID)
	return created
}

// requireNoOtherActiveAdmins 确保这个包自己的测试库里没有别人留下的管理员。
//
// 判据是按整个 users 表数「还剩下几个 active 管理员」，留下一个就会让
// 「最后一格」永远不为零，用例于是永远不会走到拒绝那一支——那种失败看起来
// 像「判据没生效」，实际是夹具没清干净。
func requireNoOtherActiveAdmins(t *testing.T, store *Store) {
	t.Helper()
	var n int64
	if err := store.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM users WHERE is_admin AND status = $1`,
		UserStatusActive).Scan(&n); err != nil {
		t.Fatalf("统计现有管理员失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("测试库里有 %d 个遗留的 active 管理员，先清干净再跑这条用例", n)
	}
}

func TestUpdateUserRejectsRemovingLastActiveAdmin(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)

	admin := createAdmin(t, store)
	ref := fmt.Sprint(admin.ID)

	no := false
	suspended, banned := UserStatusSuspended, UserStatusBanned
	cases := []struct {
		name   string
		params UpdateUserParams
	}{
		{"降级", UpdateUserParams{IsAdmin: &no, IsMerchant: &no}},
		{"停用", UpdateUserParams{Status: &suspended}},
		{"封禁", UpdateUserParams{Status: &banned}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.UpdateUser(ctx, ref, tc.params); !errors.Is(err, ErrLastAdmin) {
				t.Fatalf("对最后一个管理员%s得到 %v，期望 ErrLastAdmin", tc.name, err)
			}
			// 被挡下之后那一行一个字段都不该动：先锁后判的意思是「判完再写」，
			// 不是「写一半再回滚」之外的什么东西——真回滚了也不能留下痕迹。
			var isAdmin, isMerchant bool
			var status string
			if err := store.db.QueryRowContext(ctx,
				`SELECT is_admin, is_merchant, status FROM users WHERE id = $1`,
				admin.ID).Scan(&isAdmin, &isMerchant, &status); err != nil {
				t.Fatalf("读回管理员失败: %v", err)
			}
			if !isAdmin || !isMerchant || status != UserStatusActive {
				t.Errorf("被挡下之后是 (admin=%v, merchant=%v, status=%q)，期望原样",
					isAdmin, isMerchant, status)
			}
		})
	}

	// 不撤 admin、也不改状态的修改仍要放行：守的是「撤掉最后一格」，
	// 不是「最后一个管理员什么都改不了」。
	name := "改名-" + uniqueUsername()
	info, err := store.UpdateUser(ctx, ref, UpdateUserParams{DisplayName: &name})
	if err != nil {
		t.Fatalf("改最后一个管理员的显示名失败: %v", err)
	}
	if info.DisplayName != name {
		t.Errorf("显示名 = %q，期望 %q", info.DisplayName, name)
	}
}

// lastAdminWindow* 是一次性的触发器，把那笔撤 admin 的 UPDATE 在触发器里
// 停住 1.5 秒。
//
// 为什么需要它：两条 goroutine 只有一个起跑信号的话，「两笔重叠」是碰运气。
// 调度慢一点、连接建链慢一点，就会有一笔先提交，另一笔照样被拒——用例于是
// 绿，但证明不了咨询锁有用：把 UpdateUser 里那句 pg_advisory_xact_lock 删
// 掉，它还是绿。触发器把窗口撑成确定的一段时间，删锁之后 T2 会在 T1 未提交
// 时数到「除自己外还有 1 个管理员」→ 放行 → 两笔都成功 → 本条用例红。
//
// 名字带 test_ 前缀，收尾时删掉：真库是同包共用的，留下触发器会让别的用例
// 的 UPDATE 也停半秒。
//
// 触发器只对 lastAdminWindowUserPrefix 开头的用户名生效：`go test ./...`
// 会并行跑包，别的包同一时刻也可能在撤管理员。它们停在触发器里不会出错，
// 但那条 UPDATE 也会出现在 pg_stat_activity 里，可能被当成「T1 已就位」。
// 把窗口收窄到本用例自己造的两个账号，信号就只可能来自 T1。
const (
	lastAdminWindowFunction   = "test_last_admin_window_fn"
	lastAdminWindowTrigger    = "test_last_admin_window"
	lastAdminWindowUserPrefix = "last-admin-window-"
)

// installLastAdminWindow 装上上述触发器，并登记收尾删除。
func installLastAdminWindow(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	stmts := []string{
		`CREATE OR REPLACE FUNCTION ` + lastAdminWindowFunction + `() RETURNS trigger
		   LANGUAGE plpgsql AS $fn$
		 BEGIN
		   PERFORM pg_sleep(1.5);
		   RETURN NEW;
		 END
		 $fn$`,
		`DROP TRIGGER IF EXISTS ` + lastAdminWindowTrigger + ` ON users`,
		`CREATE TRIGGER ` + lastAdminWindowTrigger + `
		   BEFORE UPDATE ON users
		   FOR EACH ROW
		   WHEN (OLD.is_admin AND NOT NEW.is_admin
		         AND OLD.username::text LIKE '` + lastAdminWindowUserPrefix + `%')
		   EXECUTE FUNCTION ` + lastAdminWindowFunction + `()`,
	}
	for _, stmt := range stmts {
		if _, err := store.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("安装并发窗口触发器失败: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := store.db.ExecContext(ctx,
			`DROP TRIGGER IF EXISTS `+lastAdminWindowTrigger+` ON users`); err != nil {
			t.Logf("删除窗口触发器失败: %v", err)
		}
		if _, err := store.db.ExecContext(ctx,
			`DROP FUNCTION IF EXISTS `+lastAdminWindowFunction+`()`); err != nil {
			t.Logf("删除窗口函数失败: %v", err)
		}
	})
}

// createWindowAdmin 建一个用户名带窗口前缀的 admin；触发器只认这个前缀。
func createWindowAdmin(t *testing.T, store *Store) User {
	t.Helper()
	username := lastAdminWindowUserPrefix + uniqueUsername()
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

// waitForDemotionWindow 等到 T1 停在那条 UPDATE 的触发器里。
//
// 判据是 pg_stat_activity：语句仍是 UPDATE users、状态 active、且正在等
// pg_sleep（wait_event_type = Timeout）。这时 T1 已经取到咨询锁、行还没
// 提交，正是要的窗口。
func waitForDemotionWindow(t *testing.T, store *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := store.db.QueryRowContext(t.Context(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND state = 'active'
			    AND wait_event_type = 'Timeout'
			    AND query LIKE '%UPDATE users%'`).Scan(&n); err != nil {
			t.Fatalf("查看 pg_stat_activity 失败: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("10 秒内没看到 T1 停在触发器窗口里：触发器没建成，或 pg_sleep 没在跑")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpdateUserConcurrentDemotionsLeaveOneAdmin(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)
	installLastAdminWindow(t, store)

	first := createWindowAdmin(t, store)
	second := createWindowAdmin(t, store)

	no := false
	firstDone := make(chan error, 1)
	go func() {
		_, err := store.UpdateUser(ctx, fmt.Sprint(first.ID),
			UpdateUserParams{IsAdmin: &no, IsMerchant: &no})
		firstDone <- err
	}()

	// T1 停住后再起 T2：此时 T1 的咨询锁在手里、行未提交。T2 要么被锁挡到
	// T1 提交之后（正确实现），要么在 T1 未提交时就数管理员（缺锁）——后者
	// 会看到 first 还是管理员，于是放行，两笔都成功。
	waitForDemotionWindow(t, store)
	_, secondErr := store.UpdateUser(ctx, fmt.Sprint(second.ID),
		UpdateUserParams{IsAdmin: &no, IsMerchant: &no})
	firstErr := <-firstDone

	if firstErr != nil {
		t.Fatalf("先起的那笔降级失败了: %v", firstErr)
	}
	if !errors.Is(secondErr, ErrLastAdmin) {
		t.Fatalf("后起的那笔得到 %v，期望 ErrLastAdmin", secondErr)
	}

	var left int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE id IN ($1, $2) AND is_admin AND status = $3`,
		first.ID, second.ID, UserStatusActive).Scan(&left); err != nil {
		t.Fatalf("统计剩下的管理员失败: %v", err)
	}
	if left != 1 {
		t.Errorf("并发降级之后这两个账号里还剩 %d 个 active 管理员，期望 1 个", left)
	}
}
