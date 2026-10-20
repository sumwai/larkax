package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"time"
)

// 本文件是控制台会话的存储：建、按令牌读、刷新、吊销。
//
// 会话与 API Key 是两套凭证（见 consoleapi 的包注释）：key 用来发推理请求，
// 会话用来读控制台。它们不共用哈希口径，也不共用吊销路径——一个改口令的人
// 希望「别处的浏览器都下线」，而不希望手里的 API Key 一起失效。

// 会话的形态与生命周期。
const (
	// SessionTokenBytes 是会话令牌的熵：32 字节 = 256 位。
	SessionTokenBytes = 32

	// SessionCSRFBytes 是 CSRF 同步令牌的熵。它不单独构成凭证，所以位数只要
	// 够挡住「猜一个」即可；与令牌取同一个长度是为了让两处不必各记一个数。
	SessionCSRFBytes = 32

	// SessionIdleTTL 是空闲窗：超过这么久没访问，会话失效。
	SessionIdleTTL = 7 * 24 * time.Hour
	// SessionAbsoluteTTL 是绝对窗：从建立那一刻起算，无论怎么活跃都不超过它。
	SessionAbsoluteTTL = 30 * 24 * time.Hour

	// sessionTouchInterval 是 last_seen_at 的最小写入间隔。
	//
	// 与 api_keys.last_used_at 同一做法（见 lastUsedWriteInterval）：每个请求
	// 都写一次会把会话表变成热写点，而「最后活跃时间」到分钟就够。
	sessionTouchInterval = time.Minute
)

// SessionCookieName 是会话 Cookie 的名字。
//
// `__Host-` 前缀不是装饰：浏览器只在「Secure + Path=/ + 不带 Domain」时才
// 接受这个前缀写下的 Cookie，于是子域无法覆盖它。它把三条属性从「我们的
// 约定」变成「浏览器替我们检查的约束」。
const SessionCookieName = "__Host-larkax_session"

// ErrSessionNotFound 表示按令牌找不到一行会话（从来不存在，或已被删除）。
//
// 「已吊销」「已过期」不报这个：那一行还在，调用方要能区分「没有这个会话」
// 与「这个会话不该再用」——两者的下一步不同（前者是重新登录，后者也是，
// 但观测上一个是伪造的令牌、一个是自然过期）。
var ErrSessionNotFound = errors.New("会话不存在")

// Session 是一行会话里鉴权与展示要用的列。
//
// 不带 token_hash：它没有展示价值，而把它读进一个会被打印、会被日志记下的
// 结构里，等于给「哈希意外出现在某处」留了一条路。
type Session struct {
	ID        int64
	UserID    int64
	CSRFToken string
	CreatedAt time.Time
	// LastSeenAt 是最后一次刷新时刻，只用于观测。
	LastSeenAt time.Time
	// ExpiresAt 是空闲窗与绝对窗里来得早的那一个。
	ExpiresAt time.Time
	// RevokedAt 非 nil 表示会话已被吊销。
	RevokedAt *time.Time
	// UserStatus 是建立会话时一并读回来的 users.status。
	//
	// 把用户状态放在这一次查询里，是为了让「会话有效」与「用户还能用」在
	// 同一个读里成立。分两次查的话，两次之间用户被封禁时，这次请求会按
	// 「会话有效」放行——封禁晚一个请求生效，而那个请求恰好是它最该挡住的那一个。
	UserStatus string
}

// Usable 判断这个会话现在还能不能用。
//
// 判据三条：未吊销、未过期、用户仍是 active。状态不是 active 时与「会话无效」
// 对外一律是 401/403，但原因要能分开（见 consoleapi 的 sessionError）。
func (s Session) Usable(now time.Time) error {
	if s.RevokedAt != nil {
		return ErrSessionNotFound
	}
	if !now.Before(s.ExpiresAt) {
		return ErrSessionNotFound
	}
	if s.UserStatus != UserStatusActive {
		return fmt.Errorf("%w: 用户状态是 %s", ErrSessionNotFound, s.UserStatus)
	}
	return nil
}

// CreateSession 为一次登录建立一行会话。
//
// 返回的明文令牌只出现在这一次返回值里，库里存的是它的 SHA-256——与会话令牌
// 同一条约定（见 key.go 的 HashKey）。`user_agent` 与 `ip` 只用于排查
// 「这个会话是从哪来的」，不参与任何判据；它们可以为空。
func (s *Store) CreateSession(ctx context.Context, userID int64, userAgent string, ip net.IP) (Session, string, error) {
	if userID <= 0 {
		return Session{}, "", fmt.Errorf("用户 ID 必须是正整数，得到 %d", userID)
	}

	token, err := randomToken(SessionTokenBytes)
	if err != nil {
		return Session{}, "", err
	}
	csrf, err := randomToken(SessionCSRFBytes)
	if err != nil {
		return Session{}, "", err
	}

	now := time.Now().UTC()
	expiresAt := now.Add(SessionIdleTTL)
	if absolute := now.Add(SessionAbsoluteTTL); absolute.Before(expiresAt) {
		expiresAt = absolute
	}

	var ipValue any
	if len(ip) > 0 {
		ipValue = ip.String()
	}

	var out Session
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO user_sessions
		       (user_id, token_hash, csrf_token, created_at, last_seen_at,
		        expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7)
		RETURNING id, user_id, csrf_token, created_at, last_seen_at, expires_at, revoked_at`,
		userID, hashSessionToken(token), csrf, now, expiresAt,
		nullString(userAgent), ipValue,
	).Scan(&out.ID, &out.UserID, &out.CSRFToken, &out.CreatedAt,
		&out.LastSeenAt, &out.ExpiresAt, &out.RevokedAt)
	if err != nil {
		return Session{}, "", fmt.Errorf("建立会话失败: %w", err)
	}
	out.UserStatus = UserStatusActive
	return out, token, nil
}

// SessionByToken 按明文令牌读一行会话（含用户状态）。
//
// 令牌先做 SHA-256 再比：库里存的是哈希，按哈希走唯一索引定位一行，
// 与会话令牌的取法同构。
func (s *Store) SessionByToken(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrSessionNotFound
	}
	var (
		out       Session
		revokedAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.csrf_token, s.created_at, s.last_seen_at,
		       s.expires_at, s.revoked_at, u.status
		  FROM user_sessions s
		  JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1`, hashSessionToken(token),
	).Scan(&out.ID, &out.UserID, &out.CSRFToken, &out.CreatedAt, &out.LastSeenAt,
		&out.ExpiresAt, &revokedAt, &out.UserStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("查询会话失败: %w", err)
	}
	if revokedAt.Valid {
		out.RevokedAt = &revokedAt.Time
	}
	return out, nil
}

// TouchSession 刷新 last_seen_at，并把空闲窗往后推。
//
// 每分钟至多写一次：条件 `last_seen_at < now - 1min` 让并发的N个请求里只有
// 一个真的写。错误由调用方决定怎么处置——控制台把它显式丢弃，理由与
// 会话令牌的 touch 相同：一个本来有效的请求不该因为一列观测值写不进去而失败。
//
// `$2::timestamptz` 的显式转换不是装饰：驱动不声明参数类型时，`$2 - interval`
// 里的 `$2` 会被解析成 `interval - interval`（unknown 向 interval 靠），
// 于是整个条件变成 `timestamptz < interval` 而报「operator does not exist」。
// 声明了类型的准备语句不会走到那条推断，所以这个错只在真库上现形。
func (s *Store) TouchSession(ctx context.Context, session Session, now time.Time) error {
	expiresAt := now.Add(SessionIdleTTL)
	if absolute := session.CreatedAt.Add(SessionAbsoluteTTL); absolute.Before(expiresAt) {
		expiresAt = absolute
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE user_sessions
		   SET last_seen_at = $2, expires_at = $3
		 WHERE id = $1
		   AND revoked_at IS NULL
		   AND last_seen_at < $2::timestamptz - make_interval(secs => $4)`,
		session.ID, now, expiresAt, sessionTouchInterval.Seconds())
	if err != nil {
		return fmt.Errorf("刷新会话失败: %w", err)
	}
	return nil
}

// RevokeSession 吊销一行会话。已经吊销过的不报错（幂等）。
func (s *Store) RevokeSession(ctx context.Context, sessionID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE user_sessions SET revoked_at = now()
		 WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	if err != nil {
		return fmt.Errorf("吊销会话失败: %w", err)
	}
	return nil
}

// randomToken 生成一枚 base64url 随机串。
func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成随机令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// hashSessionToken 是会话令牌的存储口径：SHA-256 的 32 字节。
//
// 与 API Key 的 HashKey 不同，这里存 bytea 而不是十六进制文本：令牌没有
// 方案前缀、不需要人眼辨认，而 bytea 让这一列的长度固定、比较不必先解码。
func hashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// nullString 把空串写成 NULL。
//
// user_agent 为空时写 NULL 而不是空串：读侧两种写法都会得到「没有」，
// 而库里多一种「空串」写法只会让统计时多一个要排除的值。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
