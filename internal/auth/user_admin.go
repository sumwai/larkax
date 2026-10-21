package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 本文件是控制台「用户」那一页的读侧：分页的用户清单与「最近登录」。
//
// 它与 ListUsers 分开，因为两者的调用方要的东西不同：命令行列全表、
// 按角色/状态筛；控制台要多一个模糊搜索、id keyset 分页，还要每个用户的
// 最近登录时刻。把后者塞进 ListUsers 会让命令行多扫一列它不显示的聚合。

// UserPageQuery 是一页用户清单的查询条件。
//
// 取值由调用方先校验（角色、状态、limit）：命令行那一侧的做法也是如此
// （见 ListUsers），两处各校验一遍的话，改一处漏一处的表现是同一个参数
// 从两条路进来给出不同的结果。
type UserPageQuery struct {
	// Query 在 username 与 email 上做不区分大小写的包含匹配；空串表示不筛。
	// `%`、`_`、`\` 由这里转义，调用方传原文即可。
	Query string
	// Role 为 nil 表示不按角色筛；口径与 ListUsers 共用 rolePredicate。
	Role *Role
	// Status 为空表示不按状态筛。
	Status string
	// AfterID 是上一页最后一行的 id；0 表示从第一页开始。
	AfterID int64
	// Limit 是这一页最多要几行，必须为正。调用方多要一行来判「还有没有
	// 下一页」，这里不替它猜。
	Limit int
}

// UserRow 是清单里的一行：读侧字段加最近一次登录。
type UserRow struct {
	UserInfo
	// LastSeenAt 是这名用户所有会话里 last_seen_at 的最大值；nil 表示从没
	// 登录过控制台，或者会话行已被清理——过期会话会被删掉（见会话留存），
	// 所以「长期没登录」与「从没登录」在这里都是 nil。
	LastSeenAt *time.Time
}

// likeEscaper 转义 LIKE 的通配符。
//
// 反斜杠必须**先**换：先把 `%` 换成 `\%` 再换 `\` 的话，刚加进去的那个
// 反斜杠会被再转义一次，`%` 又变回通配符。NewReplacer 一次扫描，不会回头
// 处理已经替换出来的文本，所以次序按「原文里的每个字符」算，这里是对的。
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likeContains 把一段原文包成「包含它」的 LIKE 模式。
func likeContains(s string) string {
	return "%" + likeEscaper.Replace(s) + "%"
}

// ListUserPage 读一页用户，按 id 升序，从 AfterID 之后开始。
//
// # 为什么走不了索引
//
// username 与 email 上只有 citext 的**唯一**索引，包含匹配用不上它；
// 这张表是「一个部署里的账号数」，不是逐请求的日志，全表扫一遍可以接受。
// 用户量大到这一步成为瓶颈时，先加 pg_trgm 的 GIN 索引，而不是把这一页
// 改成前缀匹配——搜索框的语义是「记得名字里有一段」。
func (s *Store) ListUserPage(ctx context.Context, q UserPageQuery) ([]UserRow, error) {
	if q.Limit <= 0 {
		return nil, fmt.Errorf("limit 必须是正整数，得到 %d", q.Limit)
	}

	var (
		pattern    *string
		status     *string
		isMerchant *bool
		isAdmin    *bool
		afterID    *int64
	)
	if trimmed := strings.TrimSpace(q.Query); trimmed != "" {
		p := likeContains(trimmed)
		pattern = &p
	}
	if q.Status != "" {
		status = &q.Status
	}
	if q.Role != nil {
		isMerchant, isAdmin = rolePredicate(*q.Role)
	}
	if q.AfterID > 0 {
		afterID = &q.AfterID
	}

	// 最近登录用 LATERAL 而不是 GROUP BY：分组要把上面那十列全写进
	// GROUP BY（或者挑一列做聚合），漏一列就是 SQL 错误；LATERAL 只把
	// 「每个用户一行」的聚合贴在旁边，主查询的形状不变。
	const query = `
		SELECT u.id, u.username, COALESCE(u.email, ''), u.status,
		       u.is_merchant, u.is_admin, COALESCE(u.display_name, ''),
		       u.deleted_at, u.created_at, u.updated_at,
		       seen.last_seen_at
		  FROM users u
		  LEFT JOIN LATERAL (
		        SELECT max(s.last_seen_at) AS last_seen_at
		          FROM user_sessions s
		         WHERE s.user_id = u.id
		       ) seen ON true
		 WHERE ($1::text IS NULL
		        OR u.username::text ILIKE $1 ESCAPE '\'
		        OR u.email::text ILIKE $1 ESCAPE '\')
		   AND ($2::boolean IS NULL OR u.is_merchant = $2)
		   AND ($3::boolean IS NULL OR u.is_admin = $3)
		   AND ($4::text IS NULL OR u.status = $4)
		   AND ($5::bigint IS NULL OR u.id > $5)
		 ORDER BY u.id
		 LIMIT $6`

	rows, err := s.db.QueryContext(ctx, query,
		pattern, isMerchant, isAdmin, status, afterID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("查询用户页失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]UserRow, 0, q.Limit)
	for rows.Next() {
		var seenAt sql.NullTime
		// 前十列交给 scanUserInfo（与 ListUsers / UserByRef 同一份清单），
		// 第十一列在这里接。各扫一遍的话，漏一列的表现是后面所有列串位。
		info, err := scanUserInfo(func(dest ...any) error {
			return rows.Scan(append(dest, &seenAt)...)
		})
		if err != nil {
			return nil, err
		}
		row := UserRow{UserInfo: info}
		if seenAt.Valid {
			seen := seenAt.Time
			row.LastSeenAt = &seen
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户页失败: %w", err)
	}
	return out, nil
}

// UserLastSeenAt 读一名用户所有会话里最晚的一次活动。
//
// 第二个返回值区分「从没登录过」与「登录时刻是零值」——后者不是一个
// 可能出现在库里的时间，但用 bool 说出来比让调用方去比零值清楚。
func (s *Store) UserLastSeenAt(ctx context.Context, userID int64) (time.Time, bool, error) {
	var seenAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT max(last_seen_at) FROM user_sessions WHERE user_id = $1`,
		userID).Scan(&seenAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("查询最近登录失败: %w", err)
	}
	return seenAt.Time, seenAt.Valid, nil
}
