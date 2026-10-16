package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是 api_keys 的**管理面**：改一把已有 key 的额度，以及列出 key。
//
// 它与 store.go 的分工按调用场景来分，不按表来分：store.go 是请求热路径
// （鉴权，每个请求都走一次），这里是运维手工跑的命令，一天可能一次都没有。
// 两者共用同一个 Store 与同一个 *sql.DB——拆文件只是为了让改动落在不重叠
// 的地方，也让「这个包在热路径上要做什么」不被管理面的代码稀释。

// LimitParams 描述对一把已有 key 的额度修改。
//
// **零值表示什么都不改**，所以每一项要两个字段：一个说「这一项要不要动」，
// 一个说「动成什么」。这里没有别的办法——「不动」与「清空」都是 nil 指针，
// 靠一个字段表达不了三态（不动 / 清空 / 设值），而把「清空」和「设成 0」
// 合并是这一整件事里最不能犯的错：一个是不限，一个是一个都不放行。
type LimitParams struct {
	// SetRateLimit 为 true 时，把 rate_limit_rpm 改成 RateLimitRPM。
	SetRateLimit bool
	// RateLimitRPM 为 nil（且 SetRateLimit 为 true）表示写回 NULL。
	RateLimitRPM *int
	// SetQuota 为 true 时，把 quota_limit 改成 QuotaLimit。
	SetQuota bool
	// QuotaLimit 为 nil（且 SetQuota 为 true）表示写回 NULL。
	QuotaLimit *money.Amount
}

// UpdateLimits 改一把已有 key 的额度。
//
// 按 id 定位而不是按 key_prefix：前缀只有 12 个字符，它的用途是「在列表里
// 认出是哪把钥匙」，**不保证唯一**。拿它当主键去改数据，撞上同一个用户的
// 两把同前缀 key 时会安静地改错那一把。
func (s *Store) UpdateLimits(ctx context.Context, keyID int64, p LimitParams) error {
	if keyID <= 0 {
		return fmt.Errorf("key ID 必须是正整数，得到 %d", keyID)
	}
	if !p.SetRateLimit && !p.SetQuota {
		return errors.New("没有要改的额度：rate_limit_rpm 与 quota_limit 至少要给一个")
	}

	// 没被要求改的那一列写回它自己的值，于是一条语句就能表达「只改其中一项」，
	// 不必按参数拼 SQL。拼 SQL 的那种写法在有第三、第四个可选列时会把
	// 「哪里少了一个逗号」变成一类运行期才现形的错误。
	var rpm, quota any
	if p.SetRateLimit && p.RateLimitRPM != nil {
		rpm = *p.RateLimitRPM
	}
	if p.SetQuota && p.QuotaLimit != nil {
		quota = p.QuotaLimit.String()
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		   SET rate_limit_rpm = CASE WHEN $2 THEN $3::int ELSE rate_limit_rpm END,
		       quota_limit    = CASE WHEN $4 THEN $5::numeric ELSE quota_limit END
		 WHERE id = $1`,
		keyID, p.SetRateLimit, rpm, p.SetQuota, quota)
	if err != nil {
		return fmt.Errorf("改 API Key 额度失败: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("改 API Key 额度后取受影响行数失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("找不到 id=%d 的 API Key", keyID)
	}
	return nil
}

// Key 是 api_keys 表的一行，供运维列表展示。
//
// 它**不带 key_hash**：那个值没有任何展示价值，而把它读进一个会被打印、
// 会被日志记下的结构里，等于给「哈希意外出现在某处」留了一条路。
type Key struct {
	// ID 是 api_keys.id，改额度时用它定位。
	ID int64
	// UserID 与 Username 是归属用户。
	UserID   int64
	Username string
	// Name 是创建时给的备注。
	Name string
	// Prefix 是 key_prefix，用于在列表里认出是哪把钥匙。
	Prefix string
	// Status 取 active / disabled / revoked。
	Status string
	// RateLimitRPM 与 QuotaLimit 的 nil 表示列值为 NULL。
	RateLimitRPM *int
	QuotaLimit   *money.Amount
	// ExpiresAt 为 nil 表示不过期。
	ExpiresAt *time.Time
	// LastUsedAt 为 nil 表示建出来之后一次都没用过。
	LastUsedAt *time.Time
	// CreatedAt 是创建时刻。
	CreatedAt time.Time
}

// ListKeys 列出 API Key，按 id 排序。
//
// userID 为 nil 时列全部用户的 key。命令行那条路径会先把这个标识解析成
// 用户，所以这里收的是已经解析好的 ID——两处各解析一遍的话，将来「按用户名
// 查不到时要不要回落到 ID」这类改动就得改两遍，而漏掉一处不会报错。
func (s *Store) ListKeys(ctx context.Context, userID *int64) ([]Key, error) {
	const query = `
		SELECT k.id, k.user_id, u.username, k.name, k.key_prefix, k.status,
		       k.rate_limit_rpm, k.quota_limit::text, k.expires_at, k.last_used_at,
		       k.created_at
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE $1::bigint IS NULL OR k.user_id = $1
		 ORDER BY k.id`

	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Key
	for rows.Next() {
		var (
			k          Key
			rpm        sql.NullInt64
			quota      sql.NullString
			expiresAt  sql.NullTime
			lastUsedAt sql.NullTime
		)
		if err := rows.Scan(&k.ID, &k.UserID, &k.Username, &k.Name, &k.Prefix,
			&k.Status, &rpm, &quota, &expiresAt, &lastUsedAt, &k.CreatedAt); err != nil {
			return nil, err
		}

		// NULL 与 0 必须分得开（见 store.go 的 lookup），所以两列都用
		// 可空类型接，再转成指针。金额多一步解析：列是 numeric，而 Go
		// 这一侧的金额类型是定点数，没有能直接扫进去的驱动类型。
		if rpm.Valid {
			value := int(rpm.Int64)
			k.RateLimitRPM = &value
		}
		if quota.Valid {
			amount, parseErr := money.Parse(quota.String)
			if parseErr != nil {
				return nil, fmt.Errorf("api_keys.id=%d 的 quota_limit %q 解析失败: %w",
					k.ID, quota.String, parseErr)
			}
			k.QuotaLimit = &amount
		}
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		if lastUsedAt.Valid {
			k.LastUsedAt = &lastUsedAt.Time
		}

		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 API Key 行: %w", err)
	}
	return out, nil
}
