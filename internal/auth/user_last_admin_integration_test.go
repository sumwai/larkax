package auth

import (
	"errors"
	"fmt"
	"sync"
	"testing"
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

func TestUpdateUserConcurrentDemotionsLeaveOneAdmin(t *testing.T) {
	ctx := t.Context()
	store := New(testDB(ctx, t))
	requireNoOtherActiveAdmins(t, store)

	first := createAdmin(t, store)
	second := createAdmin(t, store)

	no := false
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []int64{first.ID, second.ID} {
		wg.Add(1)
		go func(target int64) {
			defer wg.Done()
			<-start
			_, err := store.UpdateUser(ctx, fmt.Sprint(target),
				UpdateUserParams{IsAdmin: &no, IsMerchant: &no})
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)

	var succeeded, rejected int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLastAdmin):
			rejected++
		default:
			t.Fatalf("并发降级返回了意外的错误: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("并发降级成功 %d 笔、被拒 %d 笔，期望各 1 笔", succeeded, rejected)
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
