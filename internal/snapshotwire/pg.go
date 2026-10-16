package snapshotwire

import (
	"context"
	"database/sql"
)

// PgDB 把一个 *sql.DB 适配成本包要的 DB 接口。
//
// 需要这层适配的原因是 QueryContext 的返回类型：标准库返回 *sql.Rows，
// 而本包为了让筛选逻辑可测把它收窄成了 Rows 接口。Go 的接口方法不允许
// 协变返回类型，所以 *sql.DB 本身不满足 DB，得显式包一层。
//
// 这一层的存在是刻意的：它让「真的连库」与「逻辑对不对」分开，
// 而分区名筛选这种判错就不可逆的逻辑必须能脱离真库被测。
type PgDB struct {
	db *sql.DB
}

// Wrap 包装一个已打开的数据库句柄。
func Wrap(db *sql.DB) *PgDB { return &PgDB{db: db} }

// ExecContext 见 database/sql。
func (p *PgDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return p.db.ExecContext(ctx, query, args...)
}

// QueryContext 见 database/sql，返回类型收窄为 Rows。
func (p *PgDB) QueryContext(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// 编译期断言：适配之后的类型要满足本包自己的接口。
var _ DB = (*PgDB)(nil)
