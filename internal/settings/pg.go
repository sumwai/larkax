package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PgSource 从 PostgreSQL 读写 settings 表。
type PgSource struct {
	db *sql.DB
}

// NewPgSource 包装一个已打开的数据库句柄。
func NewPgSource(db *sql.DB) *PgSource { return &PgSource{db: db} }

// 编译期断言：同一个句柄既供读也供写，读到的与写进去的是同一张表。
var _ Setter = (*PgSource)(nil)

// Load 读全部配置行。
//
// 不带缓存也不带超时：调用方拿到的 ctx 就是超时的来源。
// 在这里自作主张加一层缓存，会让「改了配置但没生效」变成一件
// 需要读代码才能解释的事。
func (s *PgSource) Load(ctx context.Context) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	// rows.Close 的错误在这里无关紧要：遍历之后紧跟着 rows.Err()，
	// 它已经把「这一轮读取有没有出错」汇总过了（Close 的错误同样记在
	// lasterr 里），而这个查询是只读的、没有需要提交的收尾动作。
	// 下面每一处提前返回也都会经过 Close（defer），不会漏掉释放连接。
	defer func() { _ = rows.Close() }()

	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Key, &r.Value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 settings 行: %w", err)
	}
	return out, nil
}

// Entry 是 settings 表的一行，连同它的元数据。
//
// 与 Row 分开：Row 是读取路径要的东西（键与值），Entry 是给人核对用的
// （还带说明、可改性、谁在什么时候改的）。把两者合成一个类型，读取路径
// 就得为四列它不用的数据付出代价，而 List 那条查询是管理面的、跑得很少。
type Entry struct {
	Key   string
	Value []byte // jsonb 原文
	// Description 是库里那一列的原文，也是配置说明的唯一来源——迁移写下它，
	// 写入路径不碰它（见 write.go 的「为什么不动 description 列」）。
	Description string
	Editable    bool
	// UpdatedBy 是上一次写入的 users.id。nil 表示没有记录——迁移或手工 SQL
	// 写下的行就是 NULL。
	UpdatedBy *int64
	UpdatedAt time.Time
}

// List 读全部配置行及其元数据，按键名排序。
//
// 它**不做 Load 那套校验**：这是一个核对用的清单，恰恰在配置有问题的时候
// 最需要看到内容。校验不过就什么都不列，等于在最需要它的时候让它闭嘴。
// 键名与类型对不对，由调用方拿 Registry 逐条比——那一步不该把整份清单
// 一起吞掉。
func (s *PgSource) List(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, value, description, editable, updated_by, updated_at
		  FROM settings
		 ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Entry
	for rows.Next() {
		var (
			e         Entry
			updatedBy sql.NullInt64
		)
		if err := rows.Scan(&e.Key, &e.Value, &e.Description, &e.Editable,
			&updatedBy, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if updatedBy.Valid {
			id := updatedBy.Int64
			e.UpdatedBy = &id
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 settings 行: %w", err)
	}
	return out, nil
}

// Set 改写一个配置项的值。
//
// # 为什么把 editable 写进 WHERE 而不是先查后写
//
// 先 SELECT editable 再 UPDATE 的话，两次查询之间那一列可能被改掉，于是
// 「查的时候允许写、写的时候已经不允许了」这个窗口是真实存在的。放进
// 同一条语句的 WHERE 里就没有这个窗口：改到了就是改到了。
//
// 代价是失败时得再查一次库才知道为什么没改到，但那一次只发生在失败路径上。
func (s *PgSource) Set(ctx context.Context, key string, value json.RawMessage, updatedBy int64) error {
	if updatedBy <= 0 {
		return fmt.Errorf("updated_by 必须是正整数，得到 %d：这一列记的是谁改的配置", updatedBy)
	}

	// 值按 string 传，不按 []byte：驱动会把 []byte 编码成 bytea，而
	// 「column is of type jsonb but expression is of type bytea」这句话
	// 不会告诉写代码的人该改哪里。
	res, err := s.db.ExecContext(ctx, `
		UPDATE settings
		   SET value = $2, updated_by = $3, updated_at = now()
		 WHERE key = $1 AND editable`, key, string(value), updatedBy)
	if err != nil {
		return fmt.Errorf("写入 settings.%s 失败: %w", key, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("写入 settings.%s 后取受影响行数失败: %w", key, err)
	}
	if n == 1 {
		return nil
	}
	return s.explainRefusal(ctx, key)
}

// explainRefusal 在 UPDATE 一行都没改到时，查清楚是为什么。
//
// 库本身分不出「没有这个键」与「这个键不允许改」——两者都是零行，而它们
// 该做的事没有一件是共同的。
func (s *PgSource) explainRefusal(ctx context.Context, key string) error {
	var editable bool
	err := s.db.QueryRowContext(ctx,
		`SELECT editable FROM settings WHERE key = $1`, key).Scan(&editable)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：%q 要先在 settings.Registry 里声明，再由迁移插进库里",
			ErrUnknownKey, key)
	}
	if err != nil {
		return fmt.Errorf("查 settings.%s 失败: %w", key, err)
	}
	if !editable {
		return fmt.Errorf("%w：settings.%s 的 editable 为 false，它只允许由迁移写入——"+
			"命令行与将来的控制台都不改它", ErrNotEditable, key)
	}
	// 行在、editable 也是 true，却一行都没改到：只可能是这一行在两次查询
	// 之间被删掉了。报出来而不是当成功——静默地「改了一个不存在的键」
	// 正是这个包最不想有的行为。
	return fmt.Errorf("settings.%s 没写进去：这一行在这次更新期间消失了，重试一次", key)
}
