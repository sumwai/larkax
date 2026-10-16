package idempotency

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store 是幂等记录的持久化。
//
// # 为什么读与写放在同一个包
//
// 两边读写的列是同一批（request_hash、state、response_*），分开放会让
// 「写进去的值读出来不对」这类错没有唯一的归属。同一个理由在
// internal/channels 里也用过（写入侧与读取侧共用一个包）。
type Store struct {
	db *sql.DB
}

// New 包装一个已打开的数据库句柄。
func New(db *sql.DB) *Store { return &Store{db: db} }

// ClaimInTx 在**调用方的事务里**插入一条幂等记录。
//
// # 为什么必须是调用方的事务
//
// 它与预留必须一起成或一起不成。只记了幂等而没预留，重试会被告知
// 「已处理」而钱没扣；只预留而没记幂等，重试就重复扣费。分两个事务写
// 就必然有一个窗口落在这两种状态上，而它没有任何东西会报出来。
//
// 所以本函数不自开事务，只接受一个已经在跑的事务——调用方（见
// internal/ledger/store 的 SaveReservation）把「插预留行」与这一步放在
// 同一个事务里。参数 reservationID 是那个事务里刚插进去的
// billing_requests.id，用来把两边关联起来：排查「客户端说重试被拒，
// 但这次请求到底扣没扣钱」时，它是唯一的入口。
//
// # 冲突不报错，而是一个可判别的信号
//
// 用 ON CONFLICT DO NOTHING：键已经被另一个请求占住时影响 0 行，返回
// ErrClaimed。调用方据此去读那条记录、按它的状态决定等还是重放——
// 而**不是**把这个错误回给客户端。让唯一键约束自己抛错也行，但那要靠
// 解析 PostgreSQL 的错误码（SQLSTATE 23505），而那是把一个语义判断
// 建立在一个字符串常量的稳定性上。
func ClaimInTx(ctx context.Context, tx *sql.Tx, c *Claim, reservationID int64) error {
	if tx == nil {
		return errors.New("ClaimInTx 需要一个已经在跑的事务")
	}
	if c == nil {
		return errors.New("幂等声明为空")
	}
	if err := ValidateKey(c.Key); err != nil {
		return err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO idempotency_keys
			(api_key_id, key, request_hash, state, billing_request_id)
		VALUES ($1, $2, $3, 'in_progress', NULLIF($4, 0))
		ON CONFLICT (api_key_id, key) DO NOTHING`,
		c.APIKeyID, c.Key, c.Hash[:], reservationID)
	if err != nil {
		return fmt.Errorf("写幂等记录失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return ErrClaimed
	}
	return nil
}

// Lookup 按 (api_key_id, key) 读一条记录。没有时返回 ErrNotFound。
//
// 用 api_key_id 而不是 user_id 定位：作用域就是它（见 Claim 的注释）。
// 少了这个条件，一次跨租户的撞键会让 A 用户拿到 B 用户的响应体。
func (s *Store) Lookup(ctx context.Context, apiKeyID int64, key string) (*Record, error) {
	var (
		rec         Record
		state       string
		hash        []byte
		status      sql.NullInt64
		contentType sql.NullString
		body        []byte
		billingID   sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT state, request_hash, response_status, response_content_type,
		       response_body, billing_request_id
		  FROM idempotency_keys
		 WHERE api_key_id = $1 AND key = $2`,
		apiKeyID, key,
	).Scan(&state, &hash, &status, &contentType, &body, &billingID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读幂等记录失败: %w", err)
	}

	if len(hash) != len(rec.Hash) {
		// 列是 bytea，长度由写入方决定。走到这里说明有人用别的手段写了
		// 一行——按「长度对不上就当没记录」处理会让同键不同体被静默接受，
		// 所以报出来。
		return nil, fmt.Errorf("幂等记录的指纹长度是 %d 字节，期望 %d", len(hash), len(rec.Hash))
	}
	copy(rec.Hash[:], hash)
	rec.State = State(state)
	if status.Valid {
		rec.ResponseStatus = int(status.Int64)
	}
	rec.ResponseContentType = contentType.String
	rec.ResponseBody = body
	rec.BillingRequestID = billingID.Int64
	return &rec, nil
}

// Complete 把一次**走完**的响应写回记录，状态转成 completed。
//
// 写的是原始字节而不是「结构化响应」：重放要的是逐字节相同（见 Record）。
func (s *Store) Complete(ctx context.Context, apiKeyID int64, key string, status int, contentType string, body []byte) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE idempotency_keys
		   SET state = 'completed',
		       response_status = $3,
		       response_content_type = $4,
		       response_body = $5,
		       updated_at = now()
		 WHERE api_key_id = $1 AND key = $2 AND state = 'in_progress'`,
		apiKeyID, key, status, contentType, body)
	if err != nil {
		return fmt.Errorf("写幂等响应失败: %w", err)
	}
	return requireOneRow(res, "幂等记录不在 in_progress 上，响应没有写进去")
}

// Fail 把一次**没走完**的请求标成 failed。
//
// 它不做任何清理：已经算出来的账照旧，这里只记「这一次没有形成完整响应」，
// 于是同键的后续请求不会再打一次上游（见 State 的注释）。
func (s *Store) Fail(ctx context.Context, apiKeyID int64, key string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE idempotency_keys
		   SET state = 'failed', updated_at = now()
		 WHERE api_key_id = $1 AND key = $2 AND state = 'in_progress'`,
		apiKeyID, key)
	if err != nil {
		return fmt.Errorf("标记幂等记录失败: %w", err)
	}
	return requireOneRow(res, "幂等记录不在 in_progress 上，失败状态没有写进去")
}

// requireOneRow 把「影响 0 行」当成错误。
//
// 0 行不是成功。它与「写进去了」在返回值上完全一样，而现场表现为
// 「状态没变」——一个停在 in_progress 的记录会让后续同键请求一直等到超时
// 再拿到 409，而没有任何东西指出真正的原因（见 LESSONS.md 第 16 条）。
func requireOneRow(res sql.Result, message string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return errors.New(message)
	}
	return nil
}

// Cleanup 删除超过留存期的记录，返回删除条数。
//
// # 只写不删的表在一年后就是一块石头
//
// 留存期取自 settings 的 idempotency.retention_hours，由调用方（larkaxctl
// 的子命令）读出来传进来。本函数不自己读 settings：那一层要连库、要处理
// 配置缺失，而它与「删哪些行」是两件事。
//
// # 按 created_at 而不是 updated_at
//
// created_at 是「这个键被用掉」的时刻，updated_at 是「最后一次写它」。
// 后者会被 Complete 推到请求结束那一刻，两者差一次上游调用的时长——
// 拿它当留存起点，等于让留存期随上游快慢漂移。
//
// 留存时长为 0 或负数时**什么都不删**并返回 0：那是配置写错的样子
// （见 Retention），而在这里把它翻译成「删掉全部」会让一次笔误清空整张表。
// 判断放在这里而不是只放在 Retention 里，是因为调用方可能绕过那个函数
// 直接传一个 duration 进来。
func (s *Store) Cleanup(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, fmt.Errorf("留存时长 %s 不是正数，拒绝清理", retention)
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM idempotency_keys
		 WHERE created_at < now() - ($1::bigint * interval '1 second')`,
		int64(retention.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("清理幂等记录失败: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取影响行数失败: %w", err)
	}
	return deleted, nil
}
