package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// 建用户的集成测试。
//
// # 为什么非要在真库上跑
//
// 这里要钉住的性质全都来自**库的约束**，而不是 Go 里的判断：
//
//   - username 是 citext，唯一性大小写不敏感（Go 里没有这个语义）
//   - email 的唯一约束在**空值**上的行为：空串是一个具体的值，
//     NULL 不参与比较——「不填邮箱」存成哪个，决定了第二个不填的人
//     会不会撞上第一个
//   - 23505 的约束名（users_username_key / users_email_key）。
//     分类逻辑靠这个名字区分「用户名被占用」与「邮箱被占用」，
//     而名字对不对只有真库能回答；单测里那个 PgError 是自己造的，
//     名字写错它照样通过
//
// 没有 LARKAX_DB_DSN 时跳过（见 testDB），CI 里那个变量一直在。

// cleanupUsers 登记用例结束时删掉这些用户。
//
// 用不带取消的派生 context：调用方的 ctx 在收尾时多半已经取消，
// 带着它去 DELETE 只会静默地少删一行，而那种失败在输出里看不出来。
func cleanupUsers(ctx context.Context, t *testing.T, db *sql.DB, ids ...int64) {
	t.Helper()
	t.Cleanup(func() {
		for _, id := range ids {
			if _, err := db.ExecContext(context.WithoutCancel(ctx),
				`DELETE FROM users WHERE id = $1`, id); err != nil {
				t.Logf("清理用户 %d 失败: %v", id, err)
			}
		}
	})
}

// TestCreateUserRoundTrip 建完之后能按名字与按 ID 各查到一次。
//
// 两条路都要走：`apikey create --user` 认的就是这两种写法，
// 而 ResolveUser 对纯数字一律按 ID 解析——名字里带数字的边界不在这里测，
// 那是 ResolveUser 自己的用例。
func TestCreateUserRoundTrip(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	username := uniqueUsername()
	created, err := store.CreateUser(ctx, CreateUserParams{
		Username: username,
		Email:    username + "@example.com",
		Role:     RoleUser,
	})
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	cleanupUsers(ctx, t, db, created.ID)

	if created.ID <= 0 {
		t.Fatalf("返回的 ID 应当是正整数，得到 %d", created.ID)
	}
	if created.Username != username {
		t.Errorf("返回的用户名 = %q，期望 %q", created.Username, username)
	}

	for _, ref := range []string{username, fmt.Sprint(created.ID)} {
		got, err := store.ResolveUser(ctx, ref)
		if err != nil {
			t.Fatalf("ResolveUser(%q) 报错: %v", ref, err)
		}
		if got.ID != created.ID {
			t.Errorf("ResolveUser(%q).ID = %d，期望 %d", ref, got.ID, created.ID)
		}
	}

	// 没显式写的列由库里的默认值填，不该是空值：status 参与鉴权
	// （只有 active 的用户能用），而它写成 '' 会落到 CHECK 之外的分支上。
	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM users WHERE id = $1`, created.ID).Scan(&status); err != nil {
		t.Fatalf("读回用户失败: %v", err)
	}
	if status != userStatusActive {
		t.Errorf("status = %q，期望 %q", status, userStatusActive)
	}
}

// TestCreateUserDuplicateUsernameIsRejected 钉住「不幂等」这件事。
//
// 重名时报错，而且**已有的那个用户一个字段都不许动**：静默改一个已有用户
// 意味着另一个人的归属、账单与渠道可能被悄悄挪走，而命令的退出码还是 0。
//
// 大小写不同也算重名——username 是 citext。
func TestCreateUserDuplicateUsernameIsRejected(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	username := uniqueUsername()
	first, err := store.CreateUser(ctx, CreateUserParams{
		Username: username, Email: username + "@example.com", Role: RoleMerchant,
	})
	if err != nil {
		t.Fatalf("建第一个用户失败: %v", err)
	}
	cleanupUsers(ctx, t, db, first.ID)

	_, err = store.CreateUser(ctx, CreateUserParams{
		Username: strings.ToUpper(username), Role: RoleUser,
	})
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("重名应当报 ErrUsernameTaken，得到 %v", err)
	}
	// 分类必须可判定，且文案里要带上撞上的名字——否则「哪个名字被占了」
	// 还得人去猜。
	if !strings.Contains(err.Error(), strings.ToUpper(username)) {
		t.Errorf("错误信息里应当有撞上的用户名，实际：%v", err)
	}

	var email sql.NullString
	var isMerchant bool
	if err := db.QueryRowContext(ctx,
		`SELECT email, is_merchant FROM users WHERE id = $1`, first.ID,
	).Scan(&email, &isMerchant); err != nil {
		t.Fatalf("读回第一个用户失败: %v", err)
	}
	if email.String != username+"@example.com" || isMerchant != true {
		t.Errorf("第二次创建改动了已有用户：email=%q is_merchant=%v", email.String, isMerchant)
	}
}

// TestCreateUserDuplicateEmailIsRejected 钉住第二个唯一约束也被分类到。
//
// 邮箱撞了与用户名撞了是两件事：前者要换邮箱，后者要换名字。
// 混成一句「写入用户失败」，使用者会去改名字，而改多少次都不管用。
func TestCreateUserDuplicateEmailIsRejected(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	email := uniqueUsername() + "@example.com"
	first, err := store.CreateUser(ctx, CreateUserParams{
		Username: uniqueUsername(), Email: email, Role: RoleUser,
	})
	if err != nil {
		t.Fatalf("建第一个用户失败: %v", err)
	}
	cleanupUsers(ctx, t, db, first.ID)

	_, err = store.CreateUser(ctx, CreateUserParams{
		Username: uniqueUsername(), Email: strings.ToUpper(email), Role: RoleUser,
	})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("邮箱重名应当报 ErrEmailTaken，得到 %v", err)
	}
}

// TestCreateUserEmptyEmailIsNull 钉住「不填邮箱」存成 NULL。
//
// email 上有唯一约束，而空串在 citext 里是一个**具体的值**：
// 若存成 ”，第二个不填邮箱的用户会撞上第一个的 ”，
// 报出来的还是一句「邮箱已被占用」——而这个人根本没填邮箱。
//
// 这条只有在真库上才测得出来：Go 侧看到的都是「空字符串」，
// 差别在唯一索引怎么看它。
func TestCreateUserEmptyEmailIsNull(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	var ids []int64
	for range 2 {
		u, err := store.CreateUser(ctx, CreateUserParams{
			Username: uniqueUsername(), Role: RoleUser,
		})
		if err != nil {
			t.Fatalf("建不带邮箱的用户失败: %v", err)
		}
		ids = append(ids, u.ID)
	}
	cleanupUsers(ctx, t, db, ids...)

	for _, id := range ids {
		var email sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT email FROM users WHERE id = $1`, id).Scan(&email); err != nil {
			t.Fatalf("读回用户 %d 失败: %v", id, err)
		}
		if email.Valid {
			t.Errorf("用户 %d 的 email = %q，期望 NULL", id, email.String)
		}
	}
}

// TestCreateUserRoleFlags 钉住角色到两个布尔标记的对应关系。
//
// 它只证明「写进去的值读得回来」。这两列今天**没有生产读方**（见 Role 的
// 注释），所以这条用例不证明任何权限行为——将来接上判断时，要另写按角色
// 放行/拒绝的用例，而不是把这一条当成那件事已经生效的证据。
//
// admin 同时落上 merchant：建一个 admin 却不同时打上 merchant 标记，会让
// 将来接上的「谁能挂渠道」在两种建法下给出不同答案。
func TestCreateUserRoleFlags(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	for _, tc := range []struct {
		role       Role
		isMerchant bool
		isAdmin    bool
	}{
		{RoleUser, false, false},
		{RoleMerchant, true, false},
		{RoleAdmin, true, true},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			u, err := store.CreateUser(ctx, CreateUserParams{
				Username: uniqueUsername(), Role: tc.role,
			})
			if err != nil {
				t.Fatalf("建用户失败: %v", err)
			}
			cleanupUsers(ctx, t, db, u.ID)

			var isMerchant, isAdmin bool
			if err := db.QueryRowContext(ctx,
				`SELECT is_merchant, is_admin FROM users WHERE id = $1`, u.ID,
			).Scan(&isMerchant, &isAdmin); err != nil {
				t.Fatalf("读回用户失败: %v", err)
			}
			if isMerchant != tc.isMerchant || isAdmin != tc.isAdmin {
				t.Errorf("角色 %q 的标记 = (merchant=%v, admin=%v)，期望 (%v, %v)",
					tc.role, isMerchant, isAdmin, tc.isMerchant, tc.isAdmin)
			}
		})
	}
}

// TestListUsersAndUserByRef 把读侧那一半过一遍真库。
//
// # 为什么非要在真库上跑
//
// 要钉住的几件事都来自列本身，而不是 Go 里的判断：
//
//   - 两列可空文本（email / display_name）读回来是 NULL，而它在读侧要折成空串
//   - citext 的用户名与 id 各是一条 WHERE 分支，两条得指向同一行
//   - roleOf 折出来的称呼与 SQL 里那对标记条件挑出的是同一批行
//
// 库是这个包独占的，但同一个包里别的用例也在建用户，所以每一条断言都按
// 本用例建的那几个 id 找，不比全量。
func TestListUsersAndUserByRef(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	byRole := map[Role]UserInfo{}
	for _, role := range []Role{RoleUser, RoleMerchant, RoleAdmin} {
		u, err := store.CreateUser(ctx, CreateUserParams{
			Username: uniqueUsername(), Role: role,
		})
		if err != nil {
			t.Fatalf("建用户失败: %v", err)
		}
		cleanupUsers(ctx, t, db, u.ID)
		byRole[role] = UserInfo{ID: u.ID, Username: u.Username, Role: role}
	}

	// 不带筛选：本用例建的三个都在，每一栏都是库里那一行的值。
	all, err := store.ListUsers(ctx, ListUsersFilter{})
	if err != nil {
		t.Fatalf("ListUsers 失败: %v", err)
	}
	seen := map[int64]UserInfo{}
	for _, u := range all {
		seen[u.ID] = u
	}
	for role, want := range byRole {
		got, ok := seen[want.ID]
		if !ok {
			t.Fatalf("清单里没有 %s（id=%d）", want.Username, want.ID)
		}
		if got.Username != want.Username || got.Role != role {
			t.Errorf("id=%d 那一行是 (username=%q, role=%q)，想要 (%q, %q)",
				want.ID, got.Username, got.Role, want.Username, role)
		}
		if got.Status != UserStatusActive {
			t.Errorf("刚建的用户状态是 %q，想要 %q", got.Status, UserStatusActive)
		}
		// 没填邮箱与展示名：两列都是 NULL，读侧折成空串（不是 "<nil>"，
		// 也不是别的占位符）。
		if got.Email != "" || got.DisplayName != "" {
			t.Errorf("没填过的两列应当是空串，得到 email=%q display_name=%q",
				got.Email, got.DisplayName)
		}
	}

	// 按角色筛：本用例的三个人各落在自己那一档里，且只落在一档。
	for role, want := range byRole {
		users, listErr := store.ListUsers(ctx, ListUsersFilter{Role: &role})
		if listErr != nil {
			t.Fatalf("ListUsers(role=%s) 失败: %v", role, listErr)
		}
		for _, u := range users {
			// 每一个筛出来的行，它自己那一栏必须正是筛的那个角色。
			if u.Role != role {
				t.Errorf("--role %s 筛出了 role=%s 的行（id=%d）", role, u.Role, u.ID)
			}
		}
		hits := 0
		for _, u := range users {
			if u.ID == want.ID {
				hits++
			}
		}
		if hits != 1 {
			t.Errorf("--role %s 筛出 %d 个本用例的用户，想要 1 个", role, hits)
		}
	}

	// 按状态筛。状态没有写入命令，直接改库造出第二个取值。
	if _, execErr := db.ExecContext(ctx,
		`UPDATE users SET status = $1 WHERE id = $2`,
		UserStatusBanned, byRole[RoleUser].ID); execErr != nil {
		t.Fatalf("改状态失败: %v", execErr)
	}
	banned, err := store.ListUsers(ctx, ListUsersFilter{Status: UserStatusBanned})
	if err != nil {
		t.Fatalf("ListUsers(status=banned) 失败: %v", err)
	}
	found := false
	for _, u := range banned {
		if u.ID == byRole[RoleUser].ID {
			found = true
		}
		if u.ID == byRole[RoleAdmin].ID {
			t.Errorf("按 banned 筛出了 active 的 id=%d", u.ID)
		}
	}
	if !found {
		t.Errorf("按 banned 没筛出刚改成 banned 的 id=%d", byRole[RoleUser].ID)
	}

	// 两种标识指向同一个人：一条走 id，一条走 citext 用户名。
	target := byRole[RoleMerchant]
	byID, err := store.UserByRef(ctx, strconv.FormatInt(target.ID, 10))
	if err != nil {
		t.Fatalf("UserByRef(id) 失败: %v", err)
	}
	byName, err := store.UserByRef(ctx, target.Username)
	if err != nil {
		t.Fatalf("UserByRef(用户名) 失败: %v", err)
	}
	if byID != byName {
		t.Errorf("同一个人的两条读法不同：\n按 id：%+v\n按名字：%+v", byID, byName)
	}
	if byID.ID != target.ID || byID.Username != target.Username ||
		byID.Role != RoleMerchant || byID.Status != UserStatusActive {
		t.Errorf("读回来的字段不对：%+v", byID)
	}
	if byID.CreatedAt.IsZero() || byID.UpdatedAt.IsZero() {
		t.Errorf("两个时刻不该是零值：%+v", byID)
	}

	// 找不到时是那个具名哨兵，不是一句驱动原文——命令行的退出码按它分类。
	if _, err := store.UserByRef(ctx, uniqueUsername()); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("读一个不存在的用户得到 %v，想要 ErrUserNotFound", err)
	}
}
