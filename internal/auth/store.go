package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// api_keys.status 与 users.status 的取值，与迁移里的 CHECK 约束一致。
const (
	keyStatusActive   = "active"
	keyStatusDisabled = "disabled"
	keyStatusRevoked  = "revoked"

	userStatusActive = "active"
)

// lookup 按哈希定位一行，再逐条判断它能不能用。
//
// # 为什么状态与过期不写进 WHERE
//
// 写进 WHERE 的话，「查不到」只剩一种解释，日志里就分不清
// 「没有这个 key」和「这个 key 过期了」——而这两种情况指向的排查动作不同。
// 判断留在 Go 里，代价是每次鉴权读回几列并多做几次比较，
// 换到的是日志里能直接看出原因。
//
// 判断的**顺序**也固定：状态 → 过期 → 用户。它决定了两项都不满足时
// 日志里出现哪个原因，而固定的顺序让同一把 key 每次都给出同一个答案。
func (s *Store) lookup(ctx context.Context, plaintext string) (User, error) {
	var (
		user       User
		keyStatus  string
		userStatus string
		expiresAt  sql.NullTime
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT k.id, k.status, k.expires_at, u.id, u.username, u.status
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE k.key_hash = $1`, HashKey(plaintext),
	).Scan(&user.KeyID, &keyStatus, &expiresAt, &user.ID, &user.Username, &userStatus)

	if errors.Is(err, sql.ErrNoRows) {
		return User{}, &Error{Reason: ReasonKeyNotFound}
	}
	if err != nil {
		// 这里**不**包成 ErrUnauthenticated：查询失败不是「凭证不对」。
		// 包上去会让调用方按 401 回应一次数据库故障。
		return User{}, fmt.Errorf("查询 API Key 失败: %w", err)
	}

	switch keyStatus {
	case keyStatusActive:
	case keyStatusDisabled:
		return User{}, &Error{Reason: ReasonKeyDisabled}
	case keyStatusRevoked:
		return User{}, &Error{Reason: ReasonKeyRevoked}
	default:
		// CHECK 约束之外的值只可能来自直接改库。当成不可用，
		// 但理由单独记一条：它说明有人在绕过应用写数据。
		return User{}, &Error{Reason: ReasonKeyInactive}
	}

	// 过期判定用应用侧的时钟，而不是 SQL 里的 now()：判断要留在 Go 里
	// 才能区分原因（见上）。代价是应用与数据库的时钟必须大体一致——
	// 差到分钟级时，过期边界的判定会跟着漂。
	if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		return User{}, &Error{Reason: ReasonKeyExpired}
	}

	// key 有效不代表能用：封禁一个用户之后，他手里所有 key 必须一起失效，
	// 否则「封禁」只是让他换个 key 继续用。
	if userStatus != userStatusActive {
		return User{}, &Error{Reason: ReasonUserInactive}
	}

	s.touch(ctx, user.KeyID)
	return user, nil
}

// touch 更新 last_used_at。
//
// 它是**观测数据**，不是正确性的一部分，所以错误显式丢弃：
// 一个本来有效的请求不该因为一列统计值写不进去而变成 401——
// 那等于把「记录它被用过」的重要性抬到了「它能用」之上。
//
// 代价是这一列可能悄悄停在旧值上。它只用于「这把 key 还在用吗」这类判断，
// 真需要精确时可以按 created_at 与访问日志对。
//
// 每次鉴权都写一次是刻意的：这里没有节流，因为写入量按请求数走，
// 而更新的是同一行的一列——真成了瓶颈时该加的是节流，不是异步。
func (s *Store) touch(ctx context.Context, keyID int64) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
}

// CreateParams 描述要创建的一把 key。
type CreateParams struct {
	// UserID 是归属用户（users.id）。
	UserID int64
	// Name 是给人看的备注，可为空。
	Name string
	// ExpiresAt 为 nil 表示不过期。
	ExpiresAt *time.Time
}

// Created 是一次创建的结果。
type Created struct {
	// ID 是 api_keys.id。
	ID int64
	// Prefix 是入库的 key_prefix，用于日后在列表里认出这把 key。
	Prefix string
	// Plaintext 是**仅此一次**的明文。
	Plaintext string
}

// Create 生成一把新 key，只把哈希落库，并返回仅此一次的明文。
//
// 明文不落库也不写日志：库里只有 key_hash，它不可逆——这正是这个设计的目的。
// 所以调用方拿到明文之后唯一的正确动作是立刻交给用户，
// 任何「先存下来回头再说」都是在库外面又造了一份明文副本。
func (s *Store) Create(ctx context.Context, p CreateParams) (Created, error) {
	if p.UserID <= 0 {
		return Created{}, fmt.Errorf("用户 ID 必须是正整数，得到 %d", p.UserID)
	}

	plaintext, err := NewKey()
	if err != nil {
		return Created{}, err
	}

	// 不先查用户是否存在：外键约束会挡下不存在的 user_id，
	// 而先查一次多一个竞态窗口（查完到插入之间用户可能被删）。
	// 代价是错误信息来自数据库，调用方（命令行）因此会先解析一次用户。
	var expiresAt sql.NullTime
	if p.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: *p.ExpiresAt, Valid: true}
	}

	prefix := KeyPrefix(plaintext)
	var id int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id, name, key_hash, key_prefix, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		p.UserID, p.Name, HashKey(plaintext), prefix, expiresAt,
	).Scan(&id)
	if err != nil {
		return Created{}, fmt.Errorf("写入 API Key 失败: %w", err)
	}

	return Created{ID: id, Prefix: prefix, Plaintext: plaintext}, nil
}

// ResolveUser 把「用户 ID 或用户名」解析成一个用户。
//
// 命令行便利：运维手里通常只有用户名，而 api_keys 认的是 user_id。
//
// 纯数字一律按 ID 解析。于是用户名恰好全是数字时无法按名字指定——
// 那是个取舍：另一种做法（先按名字查、查不到再按 ID）会让
// 「用户名是 12345 的用户」与「ID 为 12345 的用户」静默地互相覆盖。
func (s *Store) ResolveUser(ctx context.Context, ref string) (User, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return User{}, errors.New("用户标识为空")
	}

	var (
		user User
		err  error
	)
	if id, parseErr := strconv.ParseInt(ref, 10, 64); parseErr == nil {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, username FROM users WHERE id = $1`, id,
		).Scan(&user.ID, &user.Username)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, username FROM users WHERE username = $1`, ref,
		).Scan(&user.ID, &user.Username)
	}

	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("找不到用户 %q", ref)
	}
	if err != nil {
		return User{}, fmt.Errorf("查询用户失败: %w", err)
	}
	return user, nil
}
