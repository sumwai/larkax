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

// users 上「admin 也是 merchant」这条 CHECK 的集成测试。
//
// # 为什么非要在真库上跑
//
// 这里要的是**库自己**拒绝一行反常数据，而这件事只有真库回答得了：约束名、
// SQLSTATE，以及「整条语句失败、那一行没被改动」。单测里那个 PgError 是自己
// 造的，约束名写错它照样通过。
//
// 没有 LARKAX_DB_DSN 时跳过（见 testDB），CI 里那个变量一直在。
//
// # 三个常量都硬编码自迁移文件
//
// 失效的方向是失败，不是静默通过：迁移文件改名或约束改名时，这一组用例会红。
const (
	// adminImpliesMerchantMigration 是建这条约束的迁移文件。版本号由文件名
	// 算出来（见 adminImpliesMerchantVersion），不在这里另写一个数字——
	// 重编号时两处不会分叉。
	adminImpliesMerchantMigration = "00030_admin_implies_merchant_check.sql"

	// adminImpliesMerchantConstraint 是那条约束的名字，与迁移文件里的字面量
	// 一致。
	adminImpliesMerchantConstraint = "users_admin_implies_merchant"

	// pgCheckViolation 是 CHECK 约束违反的 SQLSTATE。
	pgCheckViolation = "23514"
)

// adminImpliesMerchantVersion 是这条迁移的版本号，由文件名算出来。
func adminImpliesMerchantVersion(t *testing.T) int64 {
	t.Helper()
	version, err := goose.NumericComponent(adminImpliesMerchantMigration)
	if err != nil {
		t.Fatalf("从迁移文件名读版本号失败: %v", err)
	}
	return version
}

// constraintExists 问库：这条约束在不在。
//
// 判据是 pg_constraint 里的那一行，不是本进程里的某个变量——「迁移跑过了」与
// 「约束装上了」是两件事，而只有后者管得住写入。
func constraintExists(ctx context.Context, t *testing.T, db *sql.DB) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			 WHERE conname = $1 AND conrelid = 'users'::regclass)`,
		adminImpliesMerchantConstraint).Scan(&exists); err != nil {
		t.Fatalf("查约束是否存在失败: %v", err)
	}
	return exists
}

// migrateToConstraint 把本包的库退到这条迁移之前。
//
// 回滚指名版本，不说「退回最近一次」：再添一条迁移时，那句退的就是别人了，
// 而这条用例要撤的始终是这一条（见 LESSONS.md 第 37 条）。
//
// 收尾时把库推回最新：用例中途红掉会停在回滚态，本包下一个碰这条约束的用例就
// 会跟着红——那是把一条用例的失败算到别的用例头上。已经在最新版本时这一步是
// 空操作。
func migrateToConstraint(t *testing.T, db *sql.DB) {
	t.Helper()

	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}

	version := adminImpliesMerchantVersion(t) - 1
	if err := goose.DownToContext(t.Context(), db, ".", version); err != nil {
		t.Fatalf("回滚 %s 失败: %v", adminImpliesMerchantMigration, err)
	}
	if constraintExists(t.Context(), t, db) {
		t.Fatal("回滚之后约束还在：退的不是这一条")
	}

	t.Cleanup(func() {
		if err := goose.UpContext(context.WithoutCancel(t.Context()), db, "."); err != nil {
			t.Logf("收尾时把迁移推回最新失败: %v", err)
		}
	})
}

// 直接按列写这一列时，库自己会拒绝。
//
// 命令行的 user update 挡住的正是同一个形状，但绕过它的调用方（别的命令、脚本、
// 手工 SQL）不经过那一层：这条约束管的是**数据本身**，不问是谁写的。
func TestAdminImpliesMerchantRejectsRawWrite(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, _ := newUpdateFixture(ctx, t, db, store, RoleAdmin, uniqueUsername()+"@example.com")
	if m, a := readMarkers(ctx, t, db, created.ID); !m || !a {
		t.Fatalf("刚建出来的 admin 标记是 (merchant=%v, admin=%v)，期望两个都真", m, a)
	}

	_, err := db.ExecContext(ctx,
		`UPDATE users SET is_merchant = false WHERE id = $1`, created.ID)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("把 admin 的 is_merchant 置假得到 %v，期望库报一条约束冲突", err)
	}
	if pgErr.Code != pgCheckViolation {
		t.Errorf("SQLSTATE = %q，期望 %q", pgErr.Code, pgCheckViolation)
	}
	if pgErr.ConstraintName != adminImpliesMerchantConstraint {
		t.Errorf("约束名 = %q，期望 %q", pgErr.ConstraintName, adminImpliesMerchantConstraint)
	}

	// 整条语句失败，那一行不该被改动。
	if m, a := readMarkers(ctx, t, db, created.ID); !m || !a {
		t.Errorf("被挡下之后库里是 (merchant=%v, admin=%v)，期望 (true, true)", m, a)
	}

	// 另一侧要放行：只撤 is_admin 留下的 (merchant=true, admin=false) 是合法的
	// merchant。一刀切地要求两列相等会把这一格也挡掉，而那是命令行明说了要支持
	// 的一条出路（先 --no-admin）。约束写宽了在这里现形。
	if _, err := db.ExecContext(ctx,
		`UPDATE users SET is_admin = false WHERE id = $1`, created.ID); err != nil {
		t.Errorf("只把 is_admin 置假应当被放行，得到 %v", err)
	}
}

// 这条迁移的 Down 去掉约束，再前进一次之后约束还在。
func TestAdminImpliesMerchantConstraintSurvivesRoundTrip(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)

	// 正向证据：开始之前约束必须在，否则下面的回滚什么都没撤、而前进也看不出
	// 装回来了什么（见 LESSONS.md 第 16 条）。
	if !constraintExists(ctx, t, db) {
		t.Fatal("迁移之后 users 上没有这条约束：up 这一侧就没成立")
	}

	migrateToConstraint(t, db)
	if constraintExists(ctx, t, db) {
		t.Error("回滚之后约束还在")
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("重新前进失败: %v", err)
	}
	if !constraintExists(ctx, t, db) {
		t.Error("重新前进之后约束没有回来")
	}
}

// 前进时先把存量反例行对齐，再装约束；回滚时不去动那一列。
//
// 少了那句 UPDATE，这条迁移在**有反例的库上根本装不上**：ADD CONSTRAINT 会校验
// 存量行并直接失败。反例只能在约束不在的时候造出来，所以这里先退到这条迁移之前，
// 写一行 (merchant=false, admin=true)，再前进一次。
//
// 回滚那一半是为了「Down 只去掉约束」：Up 那条对齐语句事后再看认不出自己补过哪
// 几行（判据就是反例本身），所以 Down 里补一句「改回去」只能是别的形状——那形状
// 一旦落到这一行上，这里会红。
func TestAdminImpliesMerchantAlignsRowsAndDownKeepsThem(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)

	migrateToConstraint(t, db)

	// 这一行就是 auth.UpdateUser 按列写能留下的形状：roleOf 先看 is_admin，
	// 读出来仍是 admin，而 is_merchant 是假。
	username := uniqueUsername()
	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO users (username, email, is_merchant, is_admin)
		VALUES ($1, $2, false, true) RETURNING id`,
		username, username+"@example.com").Scan(&id); err != nil {
		t.Fatalf("造反例行失败: %v", err)
	}
	cleanupUsers(ctx, t, db, id)

	m, a := readMarkers(ctx, t, db, id)
	if m || !a {
		t.Fatalf("造出来的那一行是 (merchant=%v, admin=%v)，期望 (false, true)——反例没造出来", m, a)
	}
	// 它今天就已经按 admin 读：对齐那一列不改变任何既有语义。
	//
	// 这里拿两个标记直接走 roleOf，而不是走 UserByRef：库停在这条迁移之前，
	// 那张表上还没有后续迁移加的列，而 UserByRef 读的是最新 schema。
	if role := roleOf(m, a); role != RoleAdmin {
		t.Fatalf("反例行折出来的称呼是 %q，期望 %q", role, RoleAdmin)
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		t.Fatalf("重新前进失败（存量反例行会让 ADD CONSTRAINT 失败，除非先对齐它们）: %v", err)
	}

	if m, a := readMarkers(ctx, t, db, id); !m || !a {
		t.Errorf("前进之后那一行是 (merchant=%v, admin=%v)，期望 (true, true)", m, a)
	}
	if !constraintExists(ctx, t, db) {
		t.Error("前进之后约束不在")
	}

	// 再退一次：补过的行本来就是合法的 admin 行（roleOf 按 is_admin 读），
	// 回滚撤的是约束，不是那一列。
	if err := goose.DownToContext(ctx, db, ".", adminImpliesMerchantVersion(t)-1); err != nil {
		t.Fatalf("再次回滚失败: %v", err)
	}
	if m, a := readMarkers(ctx, t, db, id); !m || !a {
		t.Errorf("回滚之后那一行是 (merchant=%v, admin=%v)，期望仍是 (true, true)：Down 不该把补过的列改回去", m, a)
	}
}
