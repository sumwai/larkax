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

	// Role 是同一个读里带回来的 users.is_merchant / is_admin 折出来的角色。
	//
	// 角色必须与控制台路由判定出自同一次读：先读会话、再另查一次用户资料的
	// 话，两次之间用户被降级时，这次请求会按旧角色放行——与 UserStatus
	// 分两次查是同一种错误。请求里带的任何「当前视图」都不参与判定。
	Role Role
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
		out        Session
		revokedAt  sql.NullTime
		isMerchant bool
		isAdmin    bool
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.csrf_token, s.created_at, s.last_seen_at,
		       s.expires_at, s.revoked_at, u.status, u.is_merchant, u.is_admin
		  FROM user_sessions s
		  JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1`, hashSessionToken(token),
	).Scan(&out.ID, &out.UserID, &out.CSRFToken, &out.CreatedAt, &out.LastSeenAt,
		&out.ExpiresAt, &revokedAt, &out.UserStatus, &isMerchant, &isAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("查询会话失败: %w", err)
	}
	if revokedAt.Valid {
		out.RevokedAt = &revokedAt.Time
	}
	out.Role = RoleOf(isMerchant, isAdmin)
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

// sessionPurgePredicate 是「哪些会话行可以被删掉」这个判断，数一遍与真删共用。
//
// 两条**或**在一起：
//
//   - 已经过期的行到点即删，与留存窗口无关——它们对任何查询都没有价值；
//   - 已吊销但还没过期的行要等 revoked_at 超出留存窗口才删。窗口内那几行是
//     「这个会话什么时候被踢的」的证据，删早了就查不到了。
//
// 为什么不写成两份 SQL（数一遍一份、删一份）：条件一旦漂移（把 `<` 写成
// `<=`、把天数换成小时），干跑报出的条数就与真删的条数对不上，而使用者正是
// 拿前者决定要不要跑后者。这里让两条语句共用同一个常量，就没有漂移的余地。
//
// 索引：user_sessions_expiry_idx 是 `WHERE revoked_at IS NULL` 的偏索引，
// 只覆盖上一条；已吊销的那一半走顺序扫描。清理是低频的维护动作，为它再加
// 一条索引不值。
const sessionPurgePredicate = `(expires_at < now()
		   OR (revoked_at IS NOT NULL
		       AND revoked_at < now() - ($1::bigint * interval '1 second')))`

// CountPurgeableSessions 数一遍 PurgeSessions 会删掉的行，供 --dry-run 使用。
func (s *Store) CountPurgeableSessions(ctx context.Context, retention time.Duration) (int64, error) {
	if err := checkSessionRetention(retention); err != nil {
		return 0, err
	}
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_sessions WHERE `+sessionPurgePredicate,
		int64(retention.Seconds())).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计可清理的会话失败: %w", err)
	}
	return n, nil
}

// PurgeSessions 删掉已过期的会话，以及吊销超过留存窗口的会话。
//
// 留存期只针对已吊销的行（见 sessionPurgePredicate）。重复跑是安全的：
// 第二次没有可删的行，返回 0 而不是错误。
func (s *Store) PurgeSessions(ctx context.Context, retention time.Duration) (int64, error) {
	if err := checkSessionRetention(retention); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM user_sessions WHERE `+sessionPurgePredicate,
		int64(retention.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("清理会话失败: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取影响行数失败: %w", err)
	}
	return deleted, nil
}

// checkSessionRetention 拒绝换算成秒之后不是正数的留存时长。
//
// 判据看的是**落到 SQL 里的那个值**（`int64(retention.Seconds())`），不是传进来的
// Duration：0.5 秒能过「Duration 是正数」，截断之后却是 0 秒，判据退化成
// `revoked_at < now()`——正是这里要挡的「删掉全部已吊销行」。干跑与真删用的是
// 同一个截断值，所以「两数相等」那条用例抓不到它，只能在这里拦。
func checkSessionRetention(retention time.Duration) error {
	seconds := int64(retention.Seconds())
	if seconds <= 0 {
		return fmt.Errorf("留存时长 %s（%d 秒）不是正数，拒绝清理", retention, seconds)
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
