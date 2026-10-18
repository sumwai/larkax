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
		// admin 同时也落上 merchant（见 Role.Flags 的注释）。这两列今天没有
		// 读方，所以这条断言钉的是「落库的值」，不是任何权限行为。
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
			isMerchant, isAdmin := got.Flags()
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

// TestRoleOfAndRolePredicateDescribeTheSameMapping 钉住读侧的两半是同一套映射。
//
// roleOf 按两个标记读出一个称呼，rolePredicate 按一个称呼挑标记。它们必须互相
// 是对方的反函数：不一致的表现是清单里明明打着一个 admin，`--role admin` 却
// 筛不出它——两处各自都「对」，只有放在一起才看得出来。
//
// 判据按**四个标记组合**逐个走，而不是按三个称呼走一遍：只有前者能证明这套
// 映射是全覆盖且互斥的（每一行都恰好落进一档）。手改库能造出
// (merchant=false, admin=true) 这种数据，它就落在这一格里。
func TestRoleOfAndRolePredicateDescribeTheSameMapping(t *testing.T) {
	accepts := func(r Role, isMerchant, isAdmin bool) bool {
		wantMerchant, wantAdmin := rolePredicate(r)
		if wantMerchant != nil && *wantMerchant != isMerchant {
			return false
		}
		if wantAdmin != nil && *wantAdmin != isAdmin {
			return false
		}
		return true
	}

	for _, flags := range []struct{ merchant, admin bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		fromFlags := roleOf(flags.merchant, flags.admin)

		matched := 0
		for _, r := range []Role{RoleUser, RoleMerchant, RoleAdmin} {
			if accepts(r, flags.merchant, flags.admin) {
				matched++
				if r != fromFlags {
					t.Errorf("标记 (merchant=%v, admin=%v) 读出来是 %q，"+
						"而 --role %s 又会把它筛出来",
						flags.merchant, flags.admin, fromFlags, r)
				}
			}
		}
		// 恰好一档：0 档说明这一行筛不出来，2 档说明两个 --role 会给出同一批人。
		if matched != 1 {
			t.Errorf("标记 (merchant=%v, admin=%v) 落在 %d 档里，想要 1 档",
				flags.merchant, flags.admin, matched)
		}
	}
}

// TestParseUserStatus 钉住三个取值都认、别的都拒。
func TestParseUserStatus(t *testing.T) {
	for _, s := range UserStatuses() {
		got, err := ParseUserStatus(s)
		if err != nil {
			t.Errorf("%q 应当被接受: %v", s, err)
		}
		if got != s {
			t.Errorf("%q 解析成了 %q", s, got)
		}
	}

	// 两头空白也认，与 ParseRole 同一口径（那边有 "  admin  " 那一条）：两个解析
	// 函数各一种规矩时，同一份配置里 --role 与 --status 的容忍度就不一样。
	for _, s := range []string{"  active  ", "\tsuspended\n"} {
		got, err := ParseUserStatus(s)
		if err != nil {
			t.Errorf("%q 应当被接受（去掉两头空白）: %v", s, err)
		}
		if got != strings.TrimSpace(s) {
			t.Errorf("%q 解析成了 %q，期望去掉空白后的取值", s, got)
		}
	}

	// 空串也不认：命令行的「没给这个筛选项」由调用方判空，不靠这里回落——
	// 回落的表现是一个空取值被当成「不过滤」，而它读起来像一次成功的筛选。
	for _, bad := range []string{"", "  ", "Active", "frozen", "banned!", "banned x"} {
		if _, err := ParseUserStatus(bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}

// TestValidateNewUsernameRejectsOnlyIDRefs 钉住「哪些名字建不得」的两侧。
//
// 判据不是「看起来像数字」，而是「`--user` 会把它按 ID 解释」。所以 `007` 与
// `+7` 一起拒（strconv 那一套十进制认它们），而 `1a` / `a1` 只是名字里有数字，
// 按名字读得回来，照收。
func TestValidateNewUsernameRejectsOnlyIDRefs(t *testing.T) {
	rejected := []string{
		"123", "007", "0",
		// 正负号也在 strconv 那套十进制里，于是 `user get --user +7` 同样按 ID 走。
		"+7", "-7",
		// int64 的两端仍然是一个数；再出去一位就不是了（见下面 accepted）。
		"9223372036854775807", "-9223372036854775808",
		// 两头空白先去掉：落库的是 `123`，那正是读不回来的那一个。
		" 123 ",
	}
	for _, name := range rejected {
		err := ValidateNewUsername(name)
		if err == nil {
			t.Errorf("ValidateNewUsername(%q) 通过了，期望报错", name)
			continue
		}
		if !errors.Is(err, ErrUsernameIsIDRef) {
			t.Errorf("ValidateNewUsername(%q) = %v，期望 ErrUsernameIsIDRef", name, err)
		}
		// 报错要说清**为什么**，不是一句「名字不合法」：这个名字 `--user` 找不回来。
		if !strings.Contains(err.Error(), "--user") {
			t.Errorf("ValidateNewUsername(%q) 的报错没说清原因：%v", name, err)
		}
	}

	accepted := []string{
		"1a", "a1", "a", "a b", "123a456",
		// 不是 strconv 那套十进制：下划线、进制前缀、小数点、科学计数法都算名字。
		"1_0", "0x10", "1.0", "1e3",
		// 全角数字不是十进制数字。
		"١٢٣",
		// 走出 int64 的那两串两边都落回用户名（见 userRefIsID）。
		"9223372036854775808", "-9223372036854775809",
		// 收侧也要看 trim：去掉空白之后是 `1a`，那是个名字。
		" 1a ",
		// 只有空白（以及空串）在这里通过：名字为空由各自的调用方拦
		// （CreateUser 报「用户名不能为空」，命令行报「缺少 --name」），
		// 这一条判的是形状。
		"", "  ", "\t",
	}
	for _, name := range accepted {
		if err := ValidateNewUsername(name); err != nil {
			t.Errorf("ValidateNewUsername(%q) 报错了，期望通过：%v", name, err)
		}
	}
}

// TestValidateNewUsernameAgreesWithUserRefWhere 钉住这两处认的是同一批串。
//
// 「哪些名字建不得」与「一个标识怎么解释」是同一个判据的两半：前者的判据正是
// 「userRefWhere 会不会把它当成 ID」。两处各写一份 strconv.ParseInt 时，写入
// 路径放过去一个纯数字的名字、解析路径又把它读成 ID，而两边各自的用例都绿——
// 症状要等到有人 `user get` 一个刚建出来的用户才会现形。
//
// 比的是**去掉两头空白之后**的串：三条解析路径都先 trim 再调 userRefWhere
// （见各自的 strings.TrimSpace），而 CreateUser 落库的也是 trim 之后的名字，
// 所以两边看的是同一个值。
func TestValidateNewUsernameAgreesWithUserRefWhere(t *testing.T) {
	names := []string{
		"123", "007", "0", "+7", "-7", " 123 ",
		"9223372036854775807", "-9223372036854775808",
		"9223372036854775808", "-9223372036854775809",
		"1a", "a1", "a", "a b", "123a456", "1_0", "0x10", "1.0", "1e3", "١٢٣",
		" 1a ", "", "  ", "\t",
	}

	for _, name := range names {
		ref := strings.TrimSpace(name)
		where, arg := userRefWhere(ref, 1)

		if strings.HasPrefix(where, "id = ") {
			if err := ValidateNewUsername(name); err == nil {
				t.Errorf("%q 会被 userRefWhere 按 ID 解释（%s），ValidateNewUsername "+
					"却放过了它：这样一个名字建出来就够不着了", name, where)
			}
			continue
		}

		if err := ValidateNewUsername(name); err != nil {
			t.Errorf("%q 在 userRefWhere 那里是用户名（%s），ValidateNewUsername "+
				"却拒了：%v", name, where, err)
			continue
		}
		// 按用户名查的那一支，谓词的实参必须是这个名字本身：实参拼错了，
		// `user get` 查的是另一个串，而两条路都「成功」。
		if got, ok := arg.(string); !ok || got != ref {
			t.Errorf("userRefWhere(%q) 的用户名支实参 = %#v，期望 %q", name, arg, ref)
		}
	}
}

// TestCreateUserRejectsIDRefUsername 钉住包自己那一层也拦得住。
//
// CreateUser 是导出方法，别的调用方（将来的 HTTP 管理接口、导入脚本）也会走它，
// 所以「这样一个名字建不得」必须在**它自己**这里挡住，而不是只靠命令行拦着。
// 命令行那层挡不挡是另一条用例（见 cmd/larkaxctl 的 TestUserCreateRejectsBadFlags）。
//
// 用 New(nil)：这条校验在连库之前，所以 nil 句柄走不到。它要是往下走了一步，
// 这次调用会以 panic 收场，而不是安静地通过——下面那个 recover 就是把「收场」
// 翻成一句能读的失败。
//
// **两条防线都留着**：recover 让校验缺席时红的是这条用例自己（而不是「整包挂了」
// ——nil 解引用会结束整个测试进程，同包别的用例连跑都没跑），而它排在本文件最后
// 一条，是给「连库之前」这句话留一条独立的证据：同文件前面那两条纯函数用例先跑完、
// 各自报出自己的红，不必等这一条不再 panic。
func TestCreateUserRejectsIDRefUsername(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("校验缺席：nil 句柄走到了连库那一步（panic: %v）", r)
		}
	}()

	s := New(nil)

	for _, name := range []string{"123", "007", " 123 "} {
		_, err := s.CreateUser(t.Context(), CreateUserParams{Username: name, Role: RoleUser})
		if !errors.Is(err, ErrUsernameIsIDRef) {
			t.Errorf("CreateUser(%q) 的错误 = %v，期望 ErrUsernameIsIDRef", name, err)
		}
	}
}
