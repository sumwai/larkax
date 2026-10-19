package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/migrate"
)

// 迁移 00039 装上的两条 CHECK 的集成测试。
//
// 应用层的判断与库上的约束是两道门，这里管的是后面那道：绕过 internal/auth
// 直接写这张表的调用方（脚本、手工 SQL、将来新加的一条写路径）也要被挡下。
// 名字与 SQLSTATE 只有真库回答得了，自造的 PgError 证明不了。
const (
	// deletionMigration 是加这两条约束的迁移文件。
	deletionMigration = "00039_account_deletion.sql"

	// deletedAtConstraint 要求 status = deleted 的行有删除时刻。
	deletedAtConstraint = "users_deleted_at_required"

	// priorConstraint 要求停在「要求删除」的行记得住原来的状态。
	priorConstraint = "users_deletion_prior_required"
)

// deletionMigrationVersion 由迁移文件名算出版本号，不在测试里另写一个数字。
func deletionMigrationVersion(t *testing.T) int64 {
	t.Helper()
	version, err := goose.NumericComponent(deletionMigration)
	if err != nil {
		t.Fatalf("从迁移文件名读版本号失败: %v", err)
	}
	return version
}

// deletionConstraintExists 问库：这条约束在不在 users 上。
func deletionConstraintExists(ctx context.Context, t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			 WHERE conname = $1 AND conrelid = 'users'::regclass)`,
		name).Scan(&exists); err != nil {
		t.Fatalf("查约束是否存在失败: %v", err)
	}
	return exists
}

// deletionColumnExists 问库：users 上有没有这一列。
func deletionColumnExists(ctx context.Context, t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = 'users' AND column_name = $1)`,
		name).Scan(&exists); err != nil {
		t.Fatalf("查列是否存在失败: %v", err)
	}
	return exists
}

// TestDeletionConstraintsRejectRawWrite 直接按列写，库自己会拒绝。
func TestDeletionConstraintsRejectRawWrite(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	u := newUser(ctx, t, db, UserStatusActive)

	cases := []struct {
		name       string
		statement  string
		constraint string
	}{
		{
			name:       "到了终态却没有删除时刻",
			statement:  `UPDATE users SET status = 'deleted', deleted_at = NULL WHERE id = $1`,
			constraint: deletedAtConstraint,
		},
		{
			name:       "停在要求删除却没有记下原来的状态",
			statement:  `UPDATE users SET status = 'deletion_requested', deletion_prior_status = NULL WHERE id = $1`,
			constraint: priorConstraint,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, tc.statement, u.ID)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("按列写得到 %v，期望库报一条约束冲突", err)
			}
			if pgErr.Code != pgCheckViolation {
				t.Errorf("SQLSTATE = %q，期望 %q", pgErr.Code, pgCheckViolation)
			}
			if pgErr.ConstraintName != tc.constraint {
				t.Errorf("约束名 = %q，期望 %q", pgErr.ConstraintName, tc.constraint)
			}
		})
	}

	// 两次都是整条语句失败，那一行不该被改动。
	var (
		status    string
		deletedAt sql.NullTime
		prior     sql.NullString
	)
	if err := db.QueryRowContext(ctx, `
		SELECT status, deleted_at, deletion_prior_status FROM users WHERE id = $1`,
		u.ID).Scan(&status, &deletedAt, &prior); err != nil {
		t.Fatalf("读被挡下之后的那一行失败: %v", err)
	}
	if status != UserStatusActive || deletedAt.Valid || prior.Valid {
		t.Errorf("被挡下之后那一行是 status=%q、deleted_at=%v、prior=%v，期望原样",
			status, deletedAt, prior)
	}

	// 另一侧要放行：配套写全的两档都能进去。约束写宽了会在这里现形。
	if _, err := db.ExecContext(ctx, `
		UPDATE users SET status = 'deletion_requested', deletion_prior_status = 'active'
		 WHERE id = $1`, u.ID); err != nil {
		t.Errorf("把状态与原来的值一起写应当被放行，得到 %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE users SET status = 'deleted', deleted_at = now() WHERE id = $1`,
		u.ID); err != nil {
		t.Errorf("把终态与删除时刻一起写应当被放行，得到 %v", err)
	}
}

// TestDeletionConstraintsSurviveRoundTrip 证明这条迁移的 Down 撤掉两列与两条
// 约束，再前进一次之后它们都回来。
func TestDeletionConstraintsSurviveRoundTrip(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)

	constraints := []string{deletedAtConstraint, priorConstraint}
	columns := []string{"deleted_at", "deletion_prior_status"}

	// 正向证据：开始之前它们必须在，否则下面「回滚之后不见了」证明不了什么。
	for _, name := range constraints {
		if !deletionConstraintExists(ctx, t, db, name) {
			t.Fatalf("迁移之后 users 上没有 %s：up 这一侧就没成立", name)
		}
	}

	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.DownToContext(ctx, db, ".", deletionMigrationVersion(t)-1); err != nil {
		t.Fatalf("回滚 %s 失败: %v", deletionMigration, err)
	}
	t.Cleanup(func() {
		if err := goose.UpContext(context.WithoutCancel(t.Context()), db, "."); err != nil {
			t.Logf("收尾时把迁移推回最新失败: %v", err)
		}
	})

	for _, name := range constraints {
		if deletionConstraintExists(ctx, t, db, name) {
			t.Errorf("回滚之后 %s 还在", name)
		}
	}
	for _, name := range columns {
		if deletionColumnExists(ctx, t, db, name) {
			t.Errorf("回滚之后 users.%s 还在", name)
		}
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("重新前进失败: %v", err)
	}
	for _, name := range constraints {
		if !deletionConstraintExists(ctx, t, db, name) {
			t.Errorf("重新前进之后 %s 没有回来", name)
		}
	}
	for _, name := range columns {
		if !deletionColumnExists(ctx, t, db, name) {
			t.Errorf("重新前进之后 users.%s 没有回来", name)
		}
	}
}
