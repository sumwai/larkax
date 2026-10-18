package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件是 users 表的**写入**路径：建一个用户，以及「角色」这个称呼到
// 表上两个布尔标记的对应关系；同样的表也有**读**路径（列用户、按标识读一个），
// 它们与写入路径落在同一个文件里，因为两者共用角色称呼与标记之间那一套映射
// （见 Role.flags 与 roleOf）——分到两个文件里，这套映射就会长成两份。
//
// 它与 store.go 的分工按**表**来分，不按操作来分：store.go 管 api_keys
// 的鉴权与创建，这里管 users 的创建与读取。两边共用 User 与同一个 *sql.DB，
// 所以拆文件只是为了让改动落在不重叠的地方，不是拆包。

// PostgreSQL 唯一约束冲突的错误码，以及 users 表上两个唯一约束的名字。
//
// 约束名硬编码自迁移 00001_init.sql 里 users 的列定义
// （`username citext NOT NULL UNIQUE` 与 `email citext UNIQUE` 生成的
// 就是这两个名字）。**改那张表就要同步改这里**，否则下面的识别失效。
// 失效的方向是安全的：认不出来就按原错误上抛，不会静默当成功，
// 只是少一层分类，报错从「用户名已被占用」退化成一句驱动原文。
//
// 只认错误码不够——23505 的含义是「有某个唯一约束被违反」，
// 分不出撞的是用户名还是邮箱，而这两者给人的下一步动作不同。
const (
	pgUniqueViolation   = "23505"
	usersUsernameUnique = "users_username_key"
	usersEmailUnique    = "users_email_key"
)

// 唯一约束冲突的分类错误。
//
// 它们与「库连不上」必须分得开：前者是使用者改个名字就能继续的错误，
// 后者是环境问题。两者混在一条消息里，第一次建库的人会去改用户名，
// 而真正的问题是 DSN 写错了。
var (
	// ErrUsernameTaken 表示这个用户名已经有人用了。
	ErrUsernameTaken = errors.New("用户名已被占用")
	// ErrEmailTaken 表示这个邮箱已经挂在另一个用户上。
	ErrEmailTaken = errors.New("邮箱已被占用")
)

// Role 是用户角色的称呼。
//
// 迁移里写得很清楚：**所有角色都是用户**，merchant 与 admin 只是 users 表上
// 的布尔标记，不是独立实体。所以这里不建三张表，只把「称呼 → 标记」的
// 对应关系收在一处，免得命令行与将来别的调用方各写一份、又各自漂移。
//
// # 这两个标记今天还是**预留**，不是权限
//
// 全仓没有一处**鉴权判断**读 users.is_merchant / users.is_admin：写入点在
// CreateUser，读方只有展示与筛选那一侧（`user list` 的角色栏与 `--role`
// 筛选，见 roleOf 与 rolePredicate），没有任何代码按它放行或拒绝。所以
// `--role` 今天只是**把称呼记下来**，它不改任何一处行为——`channel create
// --user` 接受任何存在的用户，建完也不会去问这个人是不是 merchant。
//
// 它们打算给管理面的鉴权用（#158）：那时「谁能挂渠道、谁能进管理接口」
// 才是被检查的。接上之前别把这里的注释读成「已经生效」——一句写下来却
// 没人执行的声明，比没有声明更容易让下一个人以为有人管。
type Role string

const (
	// RoleUser 是普通用户：能调推理、能被计费。
	RoleUser Role = "user"
	// RoleMerchant 是 merchant 角色的称呼：**预留**指「可以挂载上游渠道并获得分成」，
	// 今天不生效（没有一处代码按它放行或拒绝，见 Role 的注释）。
	RoleMerchant Role = "merchant"
	// RoleAdmin 是 admin 角色的称呼：**预留**指「可以进管理面」，今天不生效
	// （管理面本身还不存在，见 #158）。
	RoleAdmin Role = "admin"
)

// valid 判断 r 是不是三个已知角色之一。
func (r Role) valid() bool {
	switch r {
	case RoleUser, RoleMerchant, RoleAdmin:
		return true
	}
	return false
}

// ParseRole 把外部（命令行、配置文件）给的字符串解析成角色。
//
// 它不碰数据库，所以调用方能在连库之前就用它挡住写错的参数——
// 一个拼错的角色不该先连上库再报错。
func ParseRole(s string) (Role, error) {
	r := Role(strings.TrimSpace(s))
	if !r.valid() {
		return "", fmt.Errorf("未知的角色 %q：可取 %s、%s、%s",
			s, RoleUser, RoleMerchant, RoleAdmin)
	}
	return r, nil
}

// flags 把角色翻译成 users 表上的两个标记。
//
// 生产代码里这里是这两列**唯一**的写入点，而读方只有展示与筛选那一侧
// （见 Role 的注释）：翻出来的值只是落库，没有任何鉴权判断读它。所以它现在
// 的作用是「把 --role 记下来」，把对应关系收在一处是为了将来接上时有唯一
// 一处可改，而不是因为它已经生效。
func (r Role) flags() (isMerchant, isAdmin bool) {
	switch r {
	case RoleMerchant:
		return true, false
	case RoleAdmin:
		// admin 同时也是 merchant：迁移里 users.is_admin 的注释这么写，而
		// 「admin 天然是 merchant」这条不变式只在**落库时**能保证——将来接上
		// 「谁能挂渠道」的判断时，两种建法（--role admin / --role merchant 再改）
		// 才不会给出不同答案。今天读它的只有展示与筛选，没有鉴权判断读它，
		// 所以这条也只是预留。
		return true, true
	default:
		return false, false
	}
}

// CreateUserParams 描述要创建的一个用户。
type CreateUserParams struct {
	// Username 是用户名，唯一（citext，大小写不敏感）。必填。
	Username string
	// Email 为空表示不填。填了就唯一（同样大小写不敏感）。
	Email string
	// Role 必须是三个已知角色之一，**没有默认值**：
	// 它是将来用于访问控制的那个字段（今天只落库、不参与鉴权判断，见 Role 的注释），
	// 让它随零值静默变成普通用户，等于把「忘了传」和「特意要普通用户」
	// 写成同一件事——而将来接上判断时，这个差别会变成权限的差别。
	Role Role
}

// CreateUser 建一个新用户，返回它的 ID 与用户名。
//
// # 为什么不幂等
//
// 重名时报错，而不是把已有用户改成这次的参数。静默改一个已有用户
// 比报错危险得多：用户名是人的标识，改掉它意味着**另一个人的**
// 归属、账单与渠道可能被悄悄挪走，而命令的退出码还是 0。
//
// # 错误分两类
//
//   - errors.Is(err, ErrUsernameTaken) / ErrEmailTaken：名字被占了，
//     换个名字就能继续，是使用者能处理的
//   - 其它：写入失败（连不上库、约束之外的错误），要人去查
func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) (User, error) {
	username := strings.TrimSpace(p.Username)
	if username == "" {
		return User{}, errors.New("用户名不能为空")
	}
	if !p.Role.valid() {
		return User{}, fmt.Errorf("未知的角色 %q", p.Role)
	}

	// 空邮箱存 NULL 而不是空串：email 上有唯一约束，而空串在 citext 里
	// 是一个**具体的值**——第二个不填邮箱的用户会撞上第一个的 ''。
	// NULL 不参与唯一性比较，所以「不填」必须真的落成 NULL。
	var email sql.NullString
	if e := strings.TrimSpace(p.Email); e != "" {
		email = sql.NullString{String: e, Valid: true}
	}

	// 这两列是**预留**：按角色写进去，读它的只有展示与筛选那一侧（见 Role 的注释）。
	isMerchant, isAdmin := p.Role.flags()

	// status、created_at、updated_at 不写：它们都有默认值（active / now()），
	// 让默认值留在库里一处定义，命令行不必跟着抄一遍。
	var user User
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO users (username, email, is_merchant, is_admin)
		VALUES ($1, $2, $3, $4)
		RETURNING id, username`,
		username, email, isMerchant, isAdmin,
	).Scan(&user.ID, &user.Username)
	if err != nil {
		return User{}, createUserFailure(err, username, email.String)
	}
	return user, nil
}

// createUserFailure 把写用户语句的错误翻译成调用方认得出的错误。
//
// **它只翻译，不吞**：认不出来的错误原样包一层返回，绝不因为读不懂而
// 当成成功。
func createUserFailure(err error, username, email string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		switch pgErr.ConstraintName {
		case usersUsernameUnique:
			return fmt.Errorf("%w: %q", ErrUsernameTaken, username)
		case usersEmailUnique:
			return fmt.Errorf("%w: %q", ErrEmailTaken, email)
		}
	}
	return fmt.Errorf("写入用户失败: %w", err)
}

// ── 读侧 ────────────────────────────────────────────────────────────────
//
// 下面这一半与上面的写入路径服务同一张表上同一批列。分不开的两处是**角色**：
// `--role` 怎么写成两个标记（Role.flags）与两个标记怎么读回一个称呼（roleOf）
// 是同一套映射的两半，它们必须一起改，所以放在同一个文件里。

// 用户状态，与迁移里 users.status 的 CHECK 约束一致。
const (
	// UserStatusActive 是正常状态：key 有效时就能调推理、能被计费。
	UserStatusActive = "active"
	// UserStatusSuspended 是暂停：他手里的 key 全部失效（见 store.go 的 lookup），
	// 但行还在、数据都留着，恢复只要把这一列改回去。
	UserStatusSuspended = "suspended"
	// UserStatusBanned 是封禁。它与暂停在今天的行为**完全一样**（都是
	// lookup 里那个 `!= active` 判据），分开只因为记录下来的原因不同：
	// 「临时停一下」与「不许再用」是两件事，而写进库里的东西要能区分它们。
	UserStatusBanned = "banned"
)

// UserStatuses 是三个用户状态，按 正常 → 暂停 → 封禁 排。
//
// 单开一个函数而不是在判据与报错信息里各写三遍：漏掉一处时，报错会说
// 「只认 active、suspended」，而 banned 其实也认——一句说错了自己边界的
// 错误信息比没有更坏。
func UserStatuses() []string {
	return []string{UserStatusActive, UserStatusSuspended, UserStatusBanned}
}

// ParseUserStatus 校验一个用户状态的取值。
//
// 取值不认识时**当场报错**，不静默回落成「不筛」。回落的那一面很坏：
// 一个写错的 --status 会让筛选少一层，命令照样退出 0，打出来的却是全部
// 用户，看起来像「原来这么多人是这个状态」。
func ParseUserStatus(s string) (string, error) {
	for _, known := range UserStatuses() {
		if s == known {
			return s, nil
		}
	}
	return "", fmt.Errorf("未知的用户状态 %q：只认 %s", s, strings.Join(UserStatuses(), "、"))
}

// roleOf 把 users 表上那两个标记反推成一个角色称呼。
//
// 它是 Role.flags 在**读侧**的另一半：那一处按称呼写标记，这里按标记读称呼。
//
// 先看 is_admin：迁移里写着 admin 天然也是 merchant，所以一行只写了 is_admin
// 没写 is_merchant 的数据（只能由手改库产生）仍然算 admin。读侧给不出角色时，
// 命令行那一栏就只能空着，而空着与「这一栏没填」在输出里分不出来。
func roleOf(isMerchant, isAdmin bool) Role {
	switch {
	case isAdmin:
		return RoleAdmin
	case isMerchant:
		return RoleMerchant
	default:
		return RoleUser
	}
}

// rolePredicate 把一个角色称呼折成「要挑哪些行」的两个标记条件。
//
// 它是 roleOf 的另一半，两者必须描述同一套映射：一个按标记读称呼，一个按称呼
// 挑标记。不一致的表现是清单里明明打着一个 admin，`--role admin` 却筛不出它。
//
// 返回的两个条件可以为 nil，nil 表示「这一列不参与筛选」。不能只返回一对 bool：
// merchant 的意思是「is_merchant 为真**且** is_admin 为假」，一个 bool 说不出来。
func rolePredicate(r Role) (isMerchant, isAdmin *bool) {
	yes, no := true, false
	switch r {
	case RoleAdmin:
		// 不要求 is_merchant 为真，理由见 roleOf。
		return nil, &yes
	case RoleMerchant:
		return &yes, &no
	default:
		return &no, &no
	}
}

// UserInfo 是一个用户在**读侧**的字段。
//
// 与 CreateUserParams 是两件事：那个是「要写成什么」，这个是「库里现在是什么」，
// 所以它带着 status、created_at 这些命令行写不了、只能读的列。
type UserInfo struct {
	// ID 是 users.id。
	ID int64
	// Username 是 users.username。
	Username string
	// Email 为空串表示这一列是 NULL：不填邮箱的用户存的是 NULL 而不是空串
	// （见 CreateUser），所以在这一侧「空串」只可能是「没填」。
	Email string
	// Status 取 UserStatuses 里的一个。
	Status string
	// Role 由 is_merchant / is_admin 两列反推（见 roleOf）。
	Role Role
	// DisplayName 为空串表示这一列是 NULL。
	DisplayName string
	// CreatedAt 与 UpdatedAt 是 users 表上那两列。
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ListUsersFilter 是 ListUsers 的筛选条件。
//
// 两个字段各自「为空 = 不按它筛」：不给筛选条件与「筛一个空集」是两件事，
// 所以这里不用零值表示「一行都不要」。
type ListUsersFilter struct {
	// Status 为 "" 表示不按状态筛。
	Status string
	// Role 为 nil 表示不按角色筛。
	Role *Role
}

// userInfoColumns 是读侧要的那几列，ListUsers 与 UserByRef 共用。
//
// 两列可空文本在 SQL 里就折成空串：读侧只有「填了」与「没填」两种状态，而
// sql.NullString 那种「第三态」传出去以后每个调用点都要再判一次，判漏的
// 那一处会把 NULL 打成 `<nil>` 打给使用者看。
//
// is_merchant / is_admin 读的是**原始标记**，折成称呼的事只在 roleOf 里做一次。
const userInfoColumns = `id, username, COALESCE(email, ''), status,
	       is_merchant, is_admin, COALESCE(display_name, ''), created_at, updated_at`

// ListUsers 列出用户，按 id 排序。
//
// 与 ListKeys 同一条约定：标识先由命令行解析、校验成具体取值再查，这里收的是
// 已经校验过的条件——两处各校验一遍的话，将来「--status 接受简写」这类改动
// 就得改两遍，而漏掉一处不会报错。
func (s *Store) ListUsers(ctx context.Context, f ListUsersFilter) ([]UserInfo, error) {
	var (
		status     *string
		isMerchant *bool
		isAdmin    *bool
	)
	if f.Status != "" {
		status = &f.Status
	}
	if f.Role != nil {
		isMerchant, isAdmin = rolePredicate(*f.Role)
	}

	// 三个条件都写成「为 NULL 就不过滤」，与 ListKeys 的
	// `$1::bigint IS NULL OR ...` 同一个写法：一条语句覆盖全部组合，
	// 而不是按有没有筛选拼字符串——拼出来的 SQL 每个分支都要各自看一遍。
	const query = `
		SELECT ` + userInfoColumns + `
		  FROM users
		 WHERE ($1::text IS NULL OR status = $1)
		   AND ($2::boolean IS NULL OR is_merchant = $2)
		   AND ($3::boolean IS NULL OR is_admin = $3)
		 ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query, status, isMerchant, isAdmin)
	if err != nil {
		return nil, fmt.Errorf("查询用户清单失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UserInfo
	for rows.Next() {
		info, err := scanUserInfo(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户行失败: %w", err)
	}
	return out, nil
}

// UserByRef 按「用户 ID 或用户名」读一个用户的全部读侧字段。
//
// 标识怎么解释与 ResolveUser 共用一处（见 userRefWhere），所以 `user get 12345`
// 与 `apikey create --user 12345` 认的是同一个人。
func (s *Store) UserByRef(ctx context.Context, ref string) (UserInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return UserInfo{}, errors.New("用户标识为空")
	}

	where, arg := userRefWhere(ref)
	info, err := scanUserInfo(s.db.QueryRowContext(ctx,
		`SELECT `+userInfoColumns+` FROM users WHERE `+where, arg).Scan)

	if errors.Is(err, sql.ErrNoRows) {
		return UserInfo{}, fmt.Errorf("%w: %q", ErrUserNotFound, ref)
	}
	if err != nil {
		return UserInfo{}, fmt.Errorf("查询用户失败: %w", err)
	}
	return info, nil
}

// scanUserInfo 把一行用户读成 UserInfo。
//
// 收一个 scan 函数而不是 *sql.Rows，是因为 ListUsers 与 UserByRef 拿到的
// 是两个不同的东西（*sql.Rows 与 *sql.Row），而它们的 Scan 方法是同一个形状。
// 两处各扫一遍的话，下面是九列、手工对齐，漏一列的表现是后面所有列都串位。
func scanUserInfo(scan func(...any) error) (UserInfo, error) {
	var (
		info       UserInfo
		isMerchant bool
		isAdmin    bool
	)
	if err := scan(&info.ID, &info.Username, &info.Email, &info.Status,
		&isMerchant, &isAdmin, &info.DisplayName, &info.CreatedAt, &info.UpdatedAt); err != nil {
		return UserInfo{}, err
	}
	info.Role = roleOf(isMerchant, isAdmin)
	return info, nil
}
