package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 本文件是控制台账户的**凭证侧**：登录时读一个用户、记一次失败、清一次失败，
// 以及设口令（运维重置与用户自助改口令共用）。
//
// 与 user.go 的分工：那一份管用户是什么（资料、状态、角色），这一份管他能不能
// 证明自己是自己。分开是因为这两件事的读者不同——前者给运营看，后者只在登录
// 那一条路径上被读，而那条路径上的每一个值都值得单独写清楚为什么读它。

// 登录节流的参数。
//
// 「连续失败 5 次后开始锁定」是设计的取舍点：少于 5 次时，一个记错口令的人
// 不该被惩罚；从第 5 次起，在线猜口令的成本开始指数增长——1s、2s、4s……
// 上限 15 分钟。上限的存在同样重要：没有它，一个被恶意试了 30 次的口令会把
// 账号锁到几小时后，而那是一次**用户自己无法解除**的拒绝服务。
const (
	loginLockThreshold = 5
	loginLockMaxSecs   = 900 // 15 分钟
)

// LoginUser 是登录要读的那几列。
//
// PasswordHash 为空串表示这一列是 NULL：运维还没设口令。调用方据此回
// account_not_ready，而不是把 NULL 当成空口令去比。
type LoginUser struct {
	ID               int64
	Username         string
	Status           string
	PasswordHash     string
	FailedLoginCount int
	LockedUntil      *time.Time
}

// LockRemaining 返回这个账号还要锁多久；0 表示现在就能试。
func (u LoginUser) LockRemaining(now time.Time) time.Duration {
	if u.LockedUntil == nil || !u.LockedUntil.After(now) {
		return 0
	}
	return u.LockedUntil.Sub(now)
}

// LoginUserByName 按用户名读登录要的那几列。
//
// 用户名是 citext，于是这里的比较天然不区分 ASCII 大小写，与建号时的唯一约束
// 是同一套口径。查不到时报 ErrUserNotFound——调用方会把它折成与「口令不对」
// 相同的 401（见 consoleapi 的 login），所以这个哨兵不对外。
func (s *Store) LoginUserByName(ctx context.Context, username string) (LoginUser, error) {
	out, err := s.scanLoginUser(s.db.QueryRowContext(ctx, `
		SELECT `+loginUserColumns+`
		  FROM users
		 WHERE username = $1`, username))
	if err != nil && errors.Is(err, ErrUserNotFound) {
		return LoginUser{}, fmt.Errorf("%w: %q", ErrUserNotFound, username)
	}
	return out, err
}

// LoginUserByID 按 id 读登录要的那几列。
//
// 控制台改口令时手里只有会话里的 user_id，没有用户名；先按 id 读出用户名再
// 调 LoginUserByName 会把一次读变成两次，而两次之间用户名可能被改。这一条
// 与 LoginUserByName 共用同一份列清单（见 loginUserColumns），于是「登录要
// 读哪几列」只有一处说法。
func (s *Store) LoginUserByID(ctx context.Context, userID int64) (LoginUser, error) {
	if userID <= 0 {
		return LoginUser{}, fmt.Errorf("用户 ID 必须是正整数，得到 %d", userID)
	}
	return s.scanLoginUser(s.db.QueryRowContext(ctx, `
		SELECT `+loginUserColumns+`
		  FROM users
		 WHERE id = $1`, userID))
}

// loginUserColumns 是 LoginUser 的列清单，两个读法共用。
const loginUserColumns = `id, username, status, password_hash,
	       failed_login_count, locked_until`

// scanLoginUser 把一行扫成 LoginUser。
func (s *Store) scanLoginUser(row *sql.Row) (LoginUser, error) {
	var (
		out    LoginUser
		hash   sql.NullString
		locked sql.NullTime
	)
	err := row.Scan(&out.ID, &out.Username, &out.Status, &hash,
		&out.FailedLoginCount, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return LoginUser{}, fmt.Errorf("%w", ErrUserNotFound)
	}
	if err != nil {
		return LoginUser{}, fmt.Errorf("查询登录用户失败: %w", err)
	}
	out.PasswordHash = hash.String
	if locked.Valid {
		out.LockedUntil = &locked.Time
	}
	return out, nil
}

// RecordLoginFailure 记一次失败，并在达到阈值后把锁定时刻往后推。
//
// 计数与锁定在同一条 UPDATE 里完成：分两步写时，两个并发的失败请求可能都读到
// 「还差一次」，于是两次都只加了 1 而没有锁定——阈值被并发绕过，且事后从
// 计数上看不出来。一条语句读的就是它自己写进去的那个值。
//
// 返回新的计数与锁定时刻；没有锁定时第二个返回值为 nil。锁定时长由 SQL 里的
// 指数算出（1s、2s、4s……上限 900s），**不用 Go 侧再算一遍**：两处各算一遍时，
// 判据与记录会分岔，而分岔的表现是「计数在涨，锁定时间却不变」。
func (s *Store) RecordLoginFailure(ctx context.Context, userID int64, now time.Time) (int, *time.Time, error) {
	if userID <= 0 {
		return 0, nil, fmt.Errorf("用户 ID 必须是正整数，得到 %d", userID)
	}
	var (
		count  int
		locked sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		UPDATE users
		   SET failed_login_count = failed_login_count + 1,
		       locked_until = CASE
		           WHEN failed_login_count + 1 >= $2
		           THEN $3::timestamptz + make_interval(secs => LEAST(
		                    $4::float8,
		                    power(2::float8, GREATEST(0, failed_login_count + 1 - $2))))
		           ELSE locked_until
		       END
		 WHERE id = $1
		RETURNING failed_login_count, locked_until`,
		userID, loginLockThreshold, now, float64(loginLockMaxSecs),
	).Scan(&count, &locked)
	if err != nil {
		return 0, nil, fmt.Errorf("记录登录失败失败: %w", err)
	}
	if !locked.Valid {
		return count, nil, nil
	}
	return count, &locked.Time, nil
}

// ClearLoginFailures 在登录成功后清零失败计数与锁定。
//
// 它不校验这一行是否真的存在：调用方手里有一条刚验证通过的口令哈希，那一行
// 一定在。多一次「先查再改」只会多一个窗口，而没有哪个下一步依赖它。
func (s *Store) ClearLoginFailures(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		   SET failed_login_count = 0, locked_until = NULL
		 WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("清除登录失败计数失败: %w", err)
	}
	return nil
}

// RehashPassword 在登录成功后按当前参数重算并替换口令哈希（渐进重哈希）。
//
// 它**不动会话、不清失败计数**：这不是一次口令变更，用户也没有要求别处下线。
// 与 SetPassword 分开正是为此——复用那一条会把「登录成功」变成「所有其他
// 浏览器被踢掉」，而用户对此毫不知情。
func (s *Store) RehashPassword(ctx context.Context, userID int64, encodedHash string) error {
	if userID <= 0 {
		return fmt.Errorf("用户 ID 必须是正整数，得到 %d", userID)
	}
	if encodedHash == "" {
		return errors.New("口令哈希为空")
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE users
		   SET password_hash = $2, updated_at = now()
		 WHERE id = $1`, userID, encodedHash); err != nil {
		return fmt.Errorf("重算口令哈希失败: %w", err)
	}
	return nil
}

// SetPassword 写一条新的口令哈希，并吊销会话（keep 非 nil 时留着那一行）。
//
// # 为什么是一把事务
//
// 「改口令」与「别的会话失效」必须一起成立。分两次写时，第二次失败会让
// 口令已经改变、旧会话却还能用——而那正是改口令的人最想结束的状态。
//
// # 也清失败计数
//
// 改口令通常发生在「用户自己被锁在外面」之后（运维重置）。不清计数的话，
// 他拿着新口令第一次登录仍然撞在锁定上，而界面上只会说「稍后再试」。
func (s *Store) SetPassword(ctx context.Context, userID int64, encodedHash string, keep *int64) error {
	if userID <= 0 {
		return fmt.Errorf("用户 ID 必须是正整数，得到 %d", userID)
	}
	if encodedHash == "" {
		return errors.New("口令哈希为空")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始改口令事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE users
		   SET password_hash = $2,
		       password_updated_at = now(),
		       failed_login_count = 0,
		       locked_until = NULL,
		       updated_at = now()
		 WHERE id = $1`, userID, encodedHash)
	if err != nil {
		return fmt.Errorf("写入口令失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认口令写入失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: id=%d", ErrUserNotFound, userID)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = now()
		 WHERE user_id = $1
		   AND revoked_at IS NULL
		   AND ($2::bigint IS NULL OR id <> $2)`, userID, keep); err != nil {
		return fmt.Errorf("吊销旧会话失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交改口令事务失败: %w", err)
	}
	return nil
}

// UnlockUser 清一个账号的失败计数与锁定。
//
// 运维命令：账号被自己的错误尝试锁住时，等待上限是 15 分钟，而人不一定愿意
// 等。它只动这两列，不动口令、不动会话——「解锁」不改变谁能登录，只把
// 「现在能不能试」恢复成默认。
func (s *Store) UnlockUser(ctx context.Context, ref string) (UserInfo, error) {
	info, err := s.UserByRef(ctx, ref)
	if err != nil {
		return UserInfo{}, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE users
		   SET failed_login_count = 0, locked_until = NULL
		 WHERE id = $1`, info.ID); err != nil {
		return UserInfo{}, fmt.Errorf("解锁用户失败: %w", err)
	}
	return s.UserByRef(ctx, ref)
}
