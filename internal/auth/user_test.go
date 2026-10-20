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

// TestValidateNewUsernameRejectsControlChars 钉住「哪些字符不许出现在名字里」的两侧。
//
// 判据不是「名字长得难看」，而是「这个字符会不会把打它的地方打坏」：清单与回显按
// 「一条记录一行、一个字段一栏」排版，`\n`、`\r`、`\t` 会把那一栏折出一条没有栏名
// 的续行，ESC 序列则原样进执行者的终端。名字是外部输入，而 users.username 是
// text——不在这里拦，它就一路进到别人的终端里。
//
// 两头的空白不算：判的是去掉空白之后那个串，也就是 CreateUser 落库的值。
func TestValidateNewUsernameRejectsControlChars(t *testing.T) {
	rejected := []struct {
		name string
		want string // 报错里点名的那个字符
	}{
		{"a\nb", "换行（U+000A）"},
		{"a\rb", "回车（U+000D）"},
		{"a\tb", "制表符（U+0009）"},
		{"a\x1b[31mb", "ESC（U+001B）"},
		{"a\x00b", "NUL（U+0000）"},
		{"a\x7fb", "DEL（U+007F）"},
		// 没有名字的那些 C0 只报码位（见 describeControlChar）。
		{"a\x07b", "U+0007"},
		// 两头的空白先去掉，中间那个留着。
		{" a\tb ", "制表符（U+0009）"},
	}
	for _, tc := range rejected {
		err := ValidateNewUsername(tc.name)
		if err == nil {
			t.Errorf("ValidateNewUsername(%q) 通过了，期望报错", tc.name)
			continue
		}
		if !errors.Is(err, ErrUsernameHasControlChar) {
			t.Errorf("ValidateNewUsername(%q) = %v，期望 ErrUsernameHasControlChar",
				tc.name, err)
		}
		// 报错要说清**哪个字符**、为什么不行：只说一句「非法字符」的话，改的人
		// 只能一个一个字符试着删。
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateNewUsername(%q) 的报错没点名 %s：%v", tc.name, tc.want, err)
		}
		if !strings.Contains(err.Error(), "--name") {
			t.Errorf("ValidateNewUsername(%q) 的报错没说清改哪里：%v", tc.name, err)
		}
	}

	accepted := []string{
		// 这一条不是字符白名单：名字的字符集今天没有明文规定，收的照收。
		"alice", "a b", "a.b", "a-b", "a_b", "user.1",
		"张三", "李四-三",
		// 两头的空白由 TrimSpace 去掉，去掉之后就没有控制字符了。
		"\ta\t", "\na\n", " \t a \t ",
		// 控制字符之外的非 ASCII 不在这一条里：收不收它们（C1、零宽字符）是
		// 另一条判据的事，见 ValidateNewUsername 的边界。这里是把界线写下来，
		// 不是给这两种字符背书——将来要收窄，改的是这两行。
		"a\u00a0b", "a\u200bb",
	}
	for _, name := range accepted {
		if err := ValidateNewUsername(name); err != nil {
			t.Errorf("ValidateNewUsername(%q) 报错了，期望通过：%v", name, err)
		}
	}
}

// TestValidateNewUsernameAgreesWithUserRefWhere 钉住这两处认的是同一批串。
//
// 「哪些名字建不得」与「一个标识怎么解释」是同一个判据的两半：第 1 条判据正是
// 「userRefWhere 会不会把它当成 ID」。两处各写一份 strconv.ParseInt 时，写入
// 路径放过去一个纯数字的名字、解析路径又把它读成 ID，而两边各自的用例都绿——
// 症状要等到有人 `user get` 一个刚建出来的用户才会现形。
//
// # 第 2 条判据之后，两边不再是「互相蕴含」
//
// 控制字符那一条（见 ValidateNewUsername）与 userRefWhere 无关，所以这里能要求
// 的是**一个方向**的蕴含：userRefWhere 按 ID 解释的名字，ValidateNewUsername 必须
// 拒。反过来只要求「拒得出理由，而且理由是第 2 条」——按用户名解释的名字里，
// `a\nb` 正是被第 2 条拒掉的那一类，而它将来还会多出别的同类。下面那张表里那四个
// 控制字符串走的就是这一支，它钉住的是**两条判据各有各的哨兵**：第 2 条拒掉一个
// 非 ID 的名字时，报出来的必须是它自己那个错，不能借第 1 条的名义——借了的话，
// 按哨兵分档的调用方会把「去掉那个字符就能过」读成「这个名字按 ID 找不回来」。
//
// 这一支**管不到第 2 条的宽严**：把 firstControlChar 放宽到也拒空格，红的是
// TestValidateNewUsernameRejectsControlChars 那张放行表，不是这里——只要理由还是
// 那个哨兵，这一支就 continue 过去了。
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
		// 控制字符那一类：userRefWhere 一律按用户名解释（控制字符不会让一个串
		// 变成一个十进制整数），而 ValidateNewUsername 按第 2 条拒。
		"a\nb", "a\tb", "a\x1b[31mb", "a\x7fb",
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

		err := ValidateNewUsername(name)
		if err != nil {
			if !errors.Is(err, ErrUsernameHasControlChar) {
				t.Errorf("%q 在 userRefWhere 那里是用户名（%s），ValidateNewUsername "+
					"却以别的理由拒了：%v", name, where, err)
			}
			continue
		}
		// 按用户名查的那一支，谓词的实参必须是这个名字本身：实参拼错了，
		// `user get` 查的是另一个串，而两条路都「成功」。
		if got, ok := arg.(string); !ok || got != ref {
			t.Errorf("userRefWhere(%q) 的用户名支实参 = %#v，期望 %q", name, arg, ref)
		}
	}
}

// TestValidateEmailReasonsDoNotOverlap 钉住「每句只在它真正不成立时才报」。
//
// 上面那张表问的是「该出现的那一句在不在」，这一条问的是「不该出现的那几句在不在」。
// 拒绝的理由是分开的五句（见 ValidateEmail）：一句里捎带上别的条件，被拒的人就会
// 去改一处根本没写错的地方——`alice@gmail` 那种漏了半截的写法最容易读到这个。
func TestValidateEmailReasonsDoNotOverlap(t *testing.T) {
	cases := []struct {
		email   string
		want    string
		notWant []string
	}{
		// 没有 @：不该提域名里的点与连字符。
		{"not-an-email", "@ 应当恰好一个",
			[]string{"域名里要有一个点", "连字符", "以点开头"}},
		// 域名少一个点：不该提点的摆法与连字符。
		{"a@example", "域名里要有一个点", []string{"连着两个点", "不能以点开头", "连字符"}},
		// 点摆在两头：域名里有点，不该反过来提「要有一个点」。
		{"a@b.", "不能以点开头或结尾", []string{"要有一个点", "连字符"}},
		// 连字符摆在段的两头：点这一路全对，不该提点。
		{"a@-b.example.com", "不能以连字符开头或结尾",
			[]string{"要有一个点", "以点开头", "连着两个点"}},
	}

	for _, tc := range cases {
		err := ValidateEmail(tc.email)
		if err == nil {
			t.Errorf("ValidateEmail(%q) 通过了，期望报错", tc.email)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateEmail(%q) 的报错里没有 %q：%v", tc.email, tc.want, err)
		}
		for _, extra := range tc.notWant {
			if strings.Contains(err.Error(), extra) {
				t.Errorf("ValidateEmail(%q) 的报错里捎带了 %q，那不是它这一处的问题：%v",
					tc.email, extra, err)
			}
		}
	}
}

// nbsp 是 U+00A0，不断行空格。
//
// 按码点拼出来，不在源码里直接写那个字符：它印出来与普通空格看不出差别，摆在用例
// 里读的人无从判断那是空格还是 NBSP——而下面那条要钉的正是「夹在地址里的 NBSP 也
// 要拒」。写成反斜杠转义同样不行：多写一个反斜杠就变成「地址里夹一个反斜杠」，
// 用例照样绿，覆盖却没了。
const nbsp = string(rune(0x00A0))

// TestValidateEmail 钉住「哪些串收、哪些拒」的两侧。
//
// 两侧都逐字列出来，不写成「看起来像邮箱的都收」。拒的那一张还带着**报错里必须
// 出现的那一句**：拒绝的理由分五句（见 ValidateEmail），写错哪一句都会让使用者
// 对着一条根本不是他碰坏了的判据改。
func TestValidateEmail(t *testing.T) {
	accepted := []string{
		// 常见形态。
		"a@example.com", "a@b.c.d", "user+tag@example.com", "first.last@example.com",
		"A.B@Example.COM", "a@b-c.example.com",
		// 字母表两端的那两个字母本身也是字母数字，用例把它们放进本地部分：
		// isASCIILetterDigit 的上界若写成「小于 z」，本地部分里的 z 会被拒；
		// 大写那边同理。两条各压一边。
		"z@example.com", "Z@example.com",
		// 本地部分里的这些符号标准里都允许，而一张自己列的字符表容易漏掉其中一个。
		"o'brien@example.com", "a_b@example.com", "a!b@example.com",
		// 非 ASCII 的字母与数字也收：本地化的域名与本地部分是真实存在的写法。
		"用户@例え.jp",
		// 两头空白先去掉再判，落库的也是去掉空白之后的那个值。
		"  a@example.com  ", "\ta@example.com\n",
		// 空串与只有空白的串通过：这一列「没填」在库里的样子是 NULL，由调用方
		// 按那个落库（见 ValidateEmail）。
		"", "   ",
	}
	for _, email := range accepted {
		if err := ValidateEmail(email); err != nil {
			t.Errorf("ValidateEmail(%q) 报错了，期望通过：%v", email, err)
		}
	}

	rejected := []struct {
		email string
		why   string // 报错里必须出现的那一句
	}{
		{"not-an-email", "@ 应当恰好一个"},
		{"@b.com", "@ 应当恰好一个"},
		{"a@", "@ 应当恰好一个"},
		{"first@second@example.com", "@ 应当恰好一个"},
		// 字符表之外的字符。空白（含去掉两头之后中间还剩的）也在这一条里，
		// 因为空格与制表符本来就不在表里。
		{"a b@example.com", "不该出现在地址里的字符"},
		{"a@b c.com", "不该出现在地址里的字符"},
		{"a\tb@example.com", "不该出现在地址里的字符"},
		// U+00A0 那种不断行空格。它印出来与普通空格看不出差别，所以按码点拼出来
		// （见 nbsp）：直接在源码里写那个字符的话，那一格看起来就是个空格，读的人
		// 认不出它是不是真的 NBSP——而这条要钉的正是「夹在地址里也要拒」。
		{"a@b" + nbsp + "c.com", "不该出现在地址里的字符"},
		{"a,b@example.com", "不该出现在地址里的字符"},
		{"a@b,c.com", "不该出现在地址里的字符"},
		{"a;b@example.com", "不该出现在地址里的字符"},
		{"a@b:c.com", "不该出现在地址里的字符"},
		{"a@b/c.com", "不该出现在地址里的字符"},
		{"a@[127.0.0.1]", "不该出现在地址里的字符"},
		// DEL（0x7F）是 ASCII 的最后一个码点。按哪一边判它都不在允许的字母
		// 数字里，所以「非 ASCII」那条分界的两版在这里都拒；加这一格是为了让
		// 「拒」这件事在分界本身上也有用例。
		{"a\x7fb@example.com", "不该出现在地址里的字符"},
		// 带了别的东西的写法：这一列存的是地址本身。
		{"Bob <bob@example.com>", "不该出现在地址里的字符"},
		{"<bob@example.com>", "不该出现在地址里的字符"},
		{"bob@example.com (comment)", "不该出现在地址里的字符"},
		{`"quoted"@example.com`, "不该出现在地址里的字符"},
		{`"a b"@example.com`, "不该出现在地址里的字符"},
		// 点号。
		{"a@b", "域名里要有一个点"},
		{"a@b.", "不能以点开头或结尾"},
		{"a@.b", "不能以点开头或结尾"},
		{".alice@example.com", "不能以点开头或结尾"},
		{"alice.@example.com", "不能以点开头或结尾"},
		{"a@b..c", "不能连着两个点"},
		{"a..b@example.com", "不能连着两个点"},
		// 连字符的摆放：每一段不能以它开头或结尾（段中间收，见上面那一张）。
		{"a@-b.example.com", "不能以连字符开头或结尾"},
		{"a@b-.example.com", "不能以连字符开头或结尾"},
		{"a@example.com-", "不能以连字符开头或结尾"},
	}
	for _, tc := range rejected {
		err := ValidateEmail(tc.email)
		if err == nil {
			t.Errorf("ValidateEmail(%q) 通过了，期望报错", tc.email)
			continue
		}
		if !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("ValidateEmail(%q) = %v，期望 ErrInvalidEmail", tc.email, err)
		}
		if !strings.Contains(err.Error(), tc.why) {
			t.Errorf("ValidateEmail(%q) 的报错里没有 %q：%v", tc.email, tc.why, err)
		}
	}

	// 两条边界上的存活变异各有一句等价说明：
	//
	//   - `at < 0` 改成 `at <= 0`：strings.Index 只返回 -1 或非负值，两版只在
	//     at==0 时不同。at==0 时要么后面还有一个 @（at != LastIndex），要么
	//     本地部分是空串，两版都走同一句 invalidEmail(trimmed, shapeReason)，
	//     输出逐字相同，所以没有输入能把它们分开。
	//   - `r > unicode.MaxASCII` 改成 `r >= unicode.MaxASCII`：两版只在 r 恰好
	//     是 0x7F 时不同，而 0x7F 在两边都不算字母或数字，两版都返回假。
	//     上面 DEL 那一格就是这条分界的落点。

	// 报错里要把允许的那一组符号逐个列出来：帮助文本与 docs/cli-reference.md 的
	// user 一节都省掉了逐个符号，两处都写着「完整的那一组写在报错里」，指的是这里。
	// 这里写成字面量而不是引 emailLocalSymbols：这一条要挡的正是「实现里那张表改了、
	// 帮助与文档没跟上」，从常量取的话两边一起变就永远绿。
	const symbols = "!#$%&'*+-/=?^_`{|}~"
	if err := ValidateEmail("a,b@example.com"); err == nil ||
		!strings.Contains(err.Error(), symbols) {
		t.Errorf("拒一个字符表之外的字符时，报错里要逐个列出允许的那些符号：%v", err)
	}

	// 报的是去掉两头空白之后的那个串：判据看的是它，人照着改的也是它。
	if err := ValidateEmail("  not-an-email  "); err == nil ||
		!strings.Contains(err.Error(), `"not-an-email"`) {
		t.Errorf("报错里应当引出去掉空白之后的那个串，得到 %v", err)
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
// ——nil 解引用会结束整个测试进程，同包别的用例连跑都没跑），而三条要走 nil 句柄的
// 用例排在本文件最后，是给「连库之前」这句话留一条独立的证据：前面那几条纯函数
// 用例先跑完、各自报出自己的红，不必等它们不再 panic。
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

// TestCreateUserRejectsControlCharUsername 钉住建用户的路上也判名字里有没有控制字符。
//
// 与上面那条同一条理由：CreateUser 是导出方法，别的调用方（将来的 HTTP 管理
// 接口、导入脚本）也会走它，所以判据不能只长在命令行那一层。同样是 New(nil)——
// 校验在连库之前，nil 句柄走不到；往下走了一步就会以 panic 收场。
func TestCreateUserRejectsControlCharUsername(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("校验缺席：nil 句柄走到了连库那一步（panic: %v）", r)
		}
	}()

	s := New(nil)

	for _, name := range []string{"a\nb", "a\tb", "a\x1b[31mb", " a\x7fb "} {
		_, err := s.CreateUser(t.Context(), CreateUserParams{Username: name, Role: RoleUser})
		if !errors.Is(err, ErrUsernameHasControlChar) {
			t.Errorf("CreateUser(%q) 的错误 = %v，期望 ErrUsernameHasControlChar", name, err)
		}
	}
}

// TestCreateUserRejectsBadEmail 钉住建用户的路上也判邮箱的形状。
//
// 与上面那条同一条理由：CreateUser 是导出方法，别的调用方（将来的 HTTP 管理
// 接口、导入脚本）也会走它，所以判据不能只长在命令行那一层。同样是 New(nil)——
// 校验在连库之前，nil 句柄走不到；往下走了一步就会以 panic 收场。
func TestCreateUserRejectsBadEmail(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("校验缺席：nil 句柄走到了连库那一步（panic: %v）", r)
		}
	}()

	s := New(nil)

	for _, email := range []string{"not-an-email", "a@b", "  a b@example.com  "} {
		_, err := s.CreateUser(t.Context(), CreateUserParams{
			Username: "alice", Email: email, Role: RoleUser,
		})
		if !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("CreateUser(email=%q) 的错误 = %v，期望 ErrInvalidEmail", email, err)
		}
	}
}

// TestUpdateUserRejectsBadEmail 钉住改这一列时也在落库之前判。
//
// UpdateUser 同样是导出方法。它先把要改的列折成 SET 片段与参数、折得出来才碰库，
// 所以 New(nil) 上走一遍就够了：判据在折的那一步，不在写的那一步。
func TestUpdateUserRejectsBadEmail(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("校验缺席：nil 句柄走到了连库那一步（panic: %v）", r)
		}
	}()

	s := New(nil)

	_, err := s.UpdateUser(t.Context(), "alice",
		UpdateUserParams{Email: strPtr("not-an-email")})
	if !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("UpdateUser 的错误 = %v，期望 ErrInvalidEmail", err)
	}
}

// TestUpdateUserEmptyEmailMeansNULL 钉住空串与只有空白的串都不算格式错。
//
// 落库那一头由 TestUpdateUserClearsTextColumnsToNull 在真库上回答（读回来是 NULL
// 而不是空串）；这一条问的是**校验放不放它过去**，所以直接调 userUpdateSets、
// 不碰库，也好在没有 DSN 的机器上照样跑。给空串却在写之前报格式错，使用者的
// 出路就只剩改库一条，而这一列的「清掉」本来就该由这条命令表达。
func TestUpdateUserEmptyEmailMeansNULL(t *testing.T) {
	for _, email := range []string{"", "   ", "\t"} {
		sets, args, err := userUpdateSets(UpdateUserParams{Email: strPtr(email)})
		if err != nil {
			t.Errorf("userUpdateSets(email=%q) 报错: %v", email, err)
			continue
		}
		if len(sets) != 1 || sets[0] != "email = $1" {
			t.Errorf("userUpdateSets(email=%q) 的 SET = %v，期望只动 email 一列", email, sets)
			continue
		}
		if len(args) != 1 || args[0] != nil {
			t.Errorf("userUpdateSets(email=%q) 的参数 = %#v，期望一个 nil（落成 NULL）", email, args)
		}
	}
}
