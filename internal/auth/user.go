package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// 本文件是 users 表的**写入**路径：建一个用户，以及「角色」这个称呼到
// 表上两个布尔标记的对应关系。
//
// 它与 store.go 的分工按**表**来分，不按操作来分：store.go 管 api_keys
// 的鉴权与创建，这里管 users 的创建。两边共用 User 与同一个 *sql.DB，
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
type Role string

const (
	// RoleUser 是普通用户：能调推理、能被计费。
	RoleUser Role = "user"
	// RoleMerchant 表示该用户可以挂载上游渠道并获得分成。
	RoleMerchant Role = "merchant"
	// RoleAdmin 表示该用户是管理员。
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
func (r Role) flags() (isMerchant, isAdmin bool) {
	switch r {
	case RoleMerchant:
		return true, false
	case RoleAdmin:
		// admin 天然也是 merchant（迁移里 users.is_admin 的注释就是这么写的）。
		// 建一个 admin 却不同时打上 merchant 标记，会让「谁能挂渠道」这类判断
		// 在两种建法下给出不同答案，而那个差异要等到有人去挂渠道时才暴露。
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
	// 这是个访问控制字段，让它随零值静默变成普通用户，
	// 等于把「忘了传」和「特意要普通用户」写成同一件事。
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
