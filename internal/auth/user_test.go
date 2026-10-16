package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestParseRole 钉住「称呼 → 标记」的对应关系。
//
// 这几条用例不碰数据库。角色拼错是要在连库之前挡住的，
// 所以解析必须是一条独立的、能被单测直接走到的路径。
func TestParseRole(t *testing.T) {
	ok := []struct {
		in         string
		want       Role
		isMerchant bool
		isAdmin    bool
	}{
		{"user", RoleUser, false, false},
		{"merchant", RoleMerchant, true, false},
		// admin 天然也是 merchant（迁移里 users.is_admin 的注释）。
		// 这条断言就是那句话的可执行版本。
		{"admin", RoleAdmin, true, true},
		{"  admin  ", RoleAdmin, true, true},
	}

	for _, tc := range ok {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRole(tc.in)
			if err != nil {
				t.Fatalf("ParseRole(%q) 报错: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseRole(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
			isMerchant, isAdmin := got.flags()
			if isMerchant != tc.isMerchant || isAdmin != tc.isAdmin {
				t.Errorf("%q 的标记 = (merchant=%v, admin=%v)，期望 (%v, %v)",
					got, isMerchant, isAdmin, tc.isMerchant, tc.isAdmin)
			}
		})
	}

	// 空串与大小写变体都算不认识：角色是枚举，不是自由文本。
	// 大小写不敏感地接受 "ADMIN" 会让 users 表里多出一种拼法，
	// 而下游判断读的是布尔标记，不是这个字符串——多一种拼法只有坏处。
	for _, in := range []string{"", "  ", "root", "ADMIN", "users"} {
		if _, err := ParseRole(in); err == nil {
			t.Errorf("ParseRole(%q) 应当报错，却通过了", in)
		}
	}
}

// TestCreateUserRejectsEmptyInput 走的是不碰库的校验分支。
//
// CreateUser 是导出方法，别的调用方（将来的 HTTP 管理接口、导入脚本）
// 也会走它，所以「空用户名」与「未知角色」必须在**它自己**这里就被挡住，
// 而不是只靠命令行那层拦着。
func TestCreateUserRejectsEmptyInput(t *testing.T) {
	s := New(nil) // 校验在连库之前，所以 nil 句柄走不到

	if _, err := s.CreateUser(t.Context(), CreateUserParams{Username: "  ", Role: RoleUser}); err == nil {
		t.Error("空用户名应当报错，却通过了")
	}

	_, err := s.CreateUser(t.Context(), CreateUserParams{Username: "alice"})
	if err == nil || !strings.Contains(err.Error(), "未知的角色") {
		t.Errorf("零值角色应当报错（没有默认角色），得到 %v", err)
	}
}

// TestCreateUserFailureClassifies 钉住错误分类：名字被占用与其它失败
// 必须分得开，否则第一次建库的人会去改用户名，而真正的问题是连不上库。
//
// 这里直接喂构造出来的错误给分类函数，是因为要造出一个真的
// 「唯一约束冲突」需要真库，而这条分类逻辑本身与库无关。
// 用的是驱动自己的 pgconn.PgError——分类靠 errors.As，形状必须一模一样。
func TestCreateUserFailureClassifies(t *testing.T) {
	unique := func(constraint string) error {
		return &pgconn.PgError{Code: "23505", ConstraintName: constraint}
	}

	t.Run("用户名撞了", func(t *testing.T) {
		err := createUserFailure(unique("users_username_key"), "alice", "")
		if !errors.Is(err, ErrUsernameTaken) {
			t.Errorf("期望 ErrUsernameTaken，得到 %v", err)
		}
		if !strings.Contains(err.Error(), "alice") {
			t.Errorf("错误信息里应当有撞上的用户名，实际：%v", err)
		}
	})

	t.Run("邮箱撞了", func(t *testing.T) {
		err := createUserFailure(unique("users_email_key"), "alice", "user@example.com")
		if !errors.Is(err, ErrEmailTaken) {
			t.Errorf("期望 ErrEmailTaken，得到 %v", err)
		}
	})

	t.Run("别的 23505 不猜", func(t *testing.T) {
		// 认不出的约束名按原错误上抛：猜错分类比不分类更坏，
		// 它会把一个内部故障说成「换个名字就好」。
		err := createUserFailure(unique("some_other_key"), "alice", "")
		if errors.Is(err, ErrUsernameTaken) || errors.Is(err, ErrEmailTaken) {
			t.Errorf("认不出的约束名不该被分类成占用，得到 %v", err)
		}
	})

	t.Run("连不上库", func(t *testing.T) {
		err := createUserFailure(errors.New("connection refused"), "alice", "")
		if errors.Is(err, ErrUsernameTaken) || errors.Is(err, ErrEmailTaken) {
			t.Errorf("连接失败不该被分类成占用，得到 %v", err)
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("原错误必须留在链里，实际：%v", err)
		}
	})
}
