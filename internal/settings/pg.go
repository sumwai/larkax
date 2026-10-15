package settings

import (
	"context"
	"database/sql"
	"fmt"
)

// PgSource 从 PostgreSQL 读取 settings 表。
type PgSource struct {
	db *sql.DB
}

// NewPgSource 包装一个已打开的数据库句柄。
func NewPgSource(db *sql.DB) *PgSource { return &PgSource{db: db} }

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
