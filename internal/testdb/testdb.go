// Package testdb 给集成测试一个**只属于当前包**的数据库。
//
// # 为什么需要它
//
// 所有集成测试原先共用 LARKAX_DB_DSN 指向的那一个库，而 `go test ./...`
// 默认并行跑包。于是「A 包造的夹具被 B 包看见」是一条真实存在的路，并且
// 只在并行窗口里现形（见 issue #340 与 LESSONS.md 第 26 条）。
//
// 这里按包路径建一个独立的库：`larkax_test_<包路径>`。跑完删掉，进程中途
// 挂掉留下的同名库在下一次启动时被删掉重建。
//
// # 只被测试 import
//
// 它不是库构件，而是测试夹具：生产代码永远不该 import 它。因此它出现在
// scripts/unimported-allowlist.txt 里（`lib` 类型）。
//
// # 用法
//
// 在包的某个 _test.go 里加一行 TestMain，然后把用例里的连库样板换成
// testdb.Open：
//
//	func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }
//
//	func testDB(t *testing.T) *sql.DB { return testdb.Open(t) }
//
// Main 必须在**任何 config.Load 之前**跑：它把 LARKAX_DB_DSN 换成新库的
// DSN，而配置是在进程里读环境变量的。
package testdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	// 注册 pgx 的 database/sql 驱动。
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/migrate"
)

const (
	// dbPrefix 是本助手建出来的库共有的前缀。注意：清理**不按前缀扫**，
	// 只删由本包路径算出来的那一个完整库名——见 setup 的注释。
	dbPrefix = "larkax_test_"

	// maintainDB 是建库/删库时连的维护库。与 LARKAX_DB_DSN 里的库无关，
	// 所以换主机、换端口、换账号时它跟着 DSN 走，不需要多配一个变量。
	maintainDB = "postgres"

	// identMaxBytes 是 PostgreSQL 标识符的上限（NAMEDATALEN - 1）。
	identMaxBytes = 63

	// maxOpenConns / maxIdleConns：默认 max_connections 是 100，而
	// `go test ./...` 会并行跑十几个包，每个包还有测试句柄、应用自己的池
	// 与一条维护连接。池不压住就会撞上限，表现成一条与「数据库有问题」
	// 无从区分的 connection refused / too many clients。
	//
	// 4 是「够用且留足余量」的取值：包内用例默认串行，最需要连接的
	// 并发用例（账本的死锁与重复结算）最多同时开 24 个 goroutine，它们
	// 在池上排队而不是死等——没有哪条用例会在持有一条连接时再去申请第二条。
	maxOpenConns = 4
	maxIdleConns = 1

	// setupTimeout 覆盖建库与跑迁移；迁移是本机几秒、CI 十几秒的量级。
	setupTimeout = 60 * time.Second

	// lockTimeout 是等「同名库的另一个运行」结束的上限。它要显著长于一次
	// 完整的测试运行，否则正常排队会被误判成故障；但不能无限等，否则
	// 一个卡住的进程会让后来者永远停在锁上。
	lockTimeout = 10 * time.Minute
)

// packageDSN 是 Main 给当前包建好的库的 DSN。
//
// 用它而不是直接读环境变量来判断「Main 有没有跑过」：两者在什么都没配时
// 都是空串，但「有 LARKAX_DB_DSN、却没有 Main」是本包要防的那种静默回退——
// 用例会跑在共用库上，而一切看起来正常。
var packageDSN string

// Main 给包的 TestMain 用。
//
// 没设 LARKAX_DB_DSN 时原样跑测试（用例会自己 t.Skip，与改动前一致）；
// 设了的话：按包路径建一个独立的库、跑迁移、把 LARKAX_DB_DSN 指过去、
// 跑完删库。建库或迁移失败时报出清楚的错并返回非 0——DSN 既然给了，
// 「跑不起来」就不该伪装成「跳过」。
func Main(m *testing.M) int {
	if os.Getenv("LARKAX_DB_DSN") == "" {
		return m.Run()
	}

	dir, err := callerPackageDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		return 1
	}
	name, err := databaseName(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		return 1
	}

	handle, err := setup(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		return 1
	}
	packageDSN = handle.dsn
	if err := os.Setenv("LARKAX_DB_DSN", handle.dsn); err != nil {
		fmt.Fprintf(os.Stderr, "testdb: 设置 LARKAX_DB_DSN 失败: %v\n", err)
		_ = handle.drop()
		return 1
	}

	// 正向证据：这一行让人一眼看出这个包跑在哪个库上（`go test -v` 或失败时
	// 会打出来）。没有它，「隔离生效了」与「这一层根本没跑」在输出里长得一样
	// （见 LESSONS.md 第 16 条）。
	fmt.Fprintf(os.Stderr, "testdb: 本包跑在 %s 上（跑完删掉）\n", name)

	code := m.Run()

	if err := handle.drop(); err != nil {
		// 删库失败不能让一次全绿的运行变成绿：留下的库会挡住下一次
		// （下一次启动会先删掉它，但那是「靠下一轮兜底」，不是干净收尾）。
		fmt.Fprintf(os.Stderr, "testdb: 删除本包的测试库 %s 失败: %v\n", name, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// Open 返回本包独立测试库的连接，用例结束（t.Cleanup）时关闭。
//
// 没设 LARKAX_DB_DSN 时跳过，与改动前一致：本机未必起着库。
// 设了 DSN 却没有调用 Main 时**失败**而不是跳过——那时用例会悄悄跑在
// 共用库上，正是这一层要防的事。
func Open(t testing.TB) *sql.DB {
	t.Helper()
	return OpenContext(context.Background(), t)
}

// OpenContext 与 Open 相同，但用调用方的 context 做连接探测。
func OpenContext(ctx context.Context, t testing.TB) *sql.DB {
	t.Helper()

	if os.Getenv("LARKAX_DB_DSN") == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}
	if packageDSN == "" {
		t.Fatalf("这个包没有调用 testdb.Main：加一行 TestMain 是这一层的入口。" +
			"缺了它，用例会跑在 LARKAX_DB_DSN 指向的共用库上，而它们之间会互相干扰。")
	}

	handle, err := sql.Open("pgx", packageDSN)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	handle.SetMaxOpenConns(maxOpenConns)
	handle.SetMaxIdleConns(maxIdleConns)
	t.Cleanup(func() { _ = handle.Close() })

	if err := handle.PingContext(ctx); err != nil {
		t.Fatalf("连不上本包的测试库: %v", err)
	}
	return handle
}

// ── 建库 / 删库 ───────────────────────────────────────────────────────────

// packageDB 是一次运行里属于某个包的那个库。
type packageDB struct {
	name string
	dsn  string

	admin *sql.DB
	// lock 是持有会话级 advisory lock 的那条连接：同名库同时只能有一个
	// 运行在用。进程被杀死时会话结束，锁由 PostgreSQL 自动释放。
	lock *sql.Conn
}

// setup 建库、跑迁移，返回可直接用的句柄。
func setup(name string) (*packageDB, error) {
	base := os.Getenv("LARKAX_DB_DSN")
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf(
			"LARKAX_DB_DSN 必须是 URL 形式（postgres://user:pass@host:port/db?...），当前是 %q：\n"+
				"  本助手要从中推出维护库（%s）与新库的连接串。", base, maintainDB)
	}

	adminURL := *u
	adminURL.Path = "/" + maintainDB
	admin, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		return nil, fmt.Errorf("打开维护库失败: %w", err)
	}
	admin.SetMaxOpenConns(2) // 见下面 lock 的注释：2 是下限，不是随手取的
	admin.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	err = admin.PingContext(ctx)
	if err != nil {
		_ = admin.Close()
		return nil, fmt.Errorf(
			"连不上维护库 %s（由 LARKAX_DB_DSN 推出）: %w\n"+
				"  集成测试需要一个能连上的 PostgreSQL；本机环境的搭法见 docs/development.md。",
			adminURL.Redacted(), err)
	}

	// 权限先问清楚：没有 CREATEDB 时 CREATE DATABASE 只会回一句
	// 「permission denied to create database」，看不出该去找谁开权限。
	var canCreate bool
	err = admin.QueryRowContext(ctx,
		`SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&canCreate)
	if err != nil {
		_ = admin.Close()
		return nil, fmt.Errorf("查询当前角色的 CREATEDB 权限失败: %w", err)
	}
	if !canCreate {
		var role string
		_ = admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&role)
		_ = admin.Close()
		return nil, fmt.Errorf(
			"角色 %s 没有 CREATEDB 权限，建不了每个包自己的测试库。\n"+
				"  以超级用户执行一次：ALTER ROLE %s CREATEDB;\n"+
				"  随后重新跑测试。详见 docs/development.md 的「集成测试」一节。", role, role)
	}

	// 会话级 advisory lock：同名库同时只能有一个 runner。锁键由库名算出，
	// 所以不同包（不同库名）之间互不阻塞。
	//
	// 这条连接从拿到锁起一直被占着，所以维护库的池**至少要有两条**连接：
	// 一条持锁，其余用来执行 CREATE / DROP DATABASE。只有一条的话下面
	// 第一次 ExecContext 就会永远等下去（池被锁占满），而那看起来像卡死。
	lockCtx, lockCancel := context.WithTimeout(context.Background(), lockTimeout)
	defer lockCancel()
	lock, err := admin.Conn(lockCtx)
	if err != nil {
		_ = admin.Close()
		return nil, fmt.Errorf("取维护库连接失败: %w", err)
	}
	if _, err := lock.ExecContext(lockCtx,
		`SELECT pg_advisory_lock($1)`, advisoryKey(name)); err != nil {
		_ = lock.Close()
		_ = admin.Close()
		return nil, fmt.Errorf("等待同名测试库 %s 的上一个运行结束失败"+
			"（等了 %s）: %w", name, lockTimeout, err)
	}

	// **只删这一个库**：name 是由本包的路径算出来的完整库名，不是按前缀
	// 扫出来的集合。别的包的库、开发库、生产库都不可能叫这个名字，
	// 所以下面这条 DROP 碰不到它们。
	//
	// 同名库有残留 = 上一次运行中途被杀（-race 崩溃、Ctrl-C、SIGKILL）而
	// 没跑到删除那一步。这里先删后建，让它不会影响这一次。
	if _, err := admin.ExecContext(ctx,
		`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
		releaseLock(lock)
		_ = admin.Close()
		return nil, fmt.Errorf("清理上次留下的测试库 %s 失败: %w", name, err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		releaseLock(lock)
		_ = admin.Close()
		return nil, fmt.Errorf("建测试库 %s 失败: %w", name, err)
	}

	dsn := *u
	dsn.Path = "/" + name
	targetDSN := dsn.String()

	if err := migrateUp(ctx, targetDSN); err != nil {
		// 建出来了却迁不上，留着毫无用处；尽力删掉再报错。
		_, _ = admin.ExecContext(context.WithoutCancel(ctx),
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`)
		releaseLock(lock)
		_ = admin.Close()
		return nil, fmt.Errorf("在测试库 %s 上跑迁移失败: %w", name, err)
	}

	return &packageDB{name: name, dsn: targetDSN, admin: admin, lock: lock}, nil
}

// migrateUp 把仓库内嵌的迁移推到最新。
func migrateUp(ctx context.Context, dsn string) error {
	handle, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("打开新库失败: %w", err)
	}
	defer func() { _ = handle.Close() }()
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)

	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		return fmt.Errorf("设置方言失败: %w", err)
	}
	if err := goose.UpContext(ctx, handle, "."); err != nil {
		return err
	}
	return nil
}

// drop 删掉这个库并释放锁。
func (d *packageDB) drop() error {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	_, err := d.admin.ExecContext(ctx,
		`DROP DATABASE IF EXISTS `+quoteIdent(d.name)+` WITH (FORCE)`)
	releaseLock(d.lock)
	_ = d.admin.Close()
	return err
}

// releaseLock 关掉持有 advisory lock 的连接。会话级锁在会话结束时由
// PostgreSQL 自动释放，所以不必单独发一条 unlock。
func releaseLock(conn *sql.Conn) {
	if conn == nil {
		return
	}
	_ = conn.Close()
}

// ── 库名 ──────────────────────────────────────────────────────────────────

// callerPackageDir 取调用 Main 的那个包的源码目录。
//
// 正常构建里 runtime.Caller 给的是绝对路径；带 -trimpath 时会是模块内
// 相对路径，那时退回 os.Getwd()——`go test` 的工作目录就是包目录。
func callerPackageDir() (string, error) {
	// Caller(2)：跳过 callerPackageDir 自己与 Main，落在包的 TestMain 上。
	if _, file, _, ok := runtime.Caller(2); ok && filepath.IsAbs(file) {
		return filepath.Dir(file), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("取不到当前包的路径: %w", err)
	}
	return wd, nil
}

// databaseName 把包目录变成库名：`larkax_test_<模块内路径>`。
//
// 用包路径而不是包名（`store` 在三个包上重名），并且用模块内相对路径而不是
// 绝对路径（同一个包在 main 与各个 worktree 里得到同一个库名）。
func databaseName(dir string) (string, error) {
	root, err := moduleRoot(dir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("算不出 %s 相对模块根 %s 的路径: %w", dir, root, err)
	}
	return databaseNameFor(filepath.ToSlash(rel)), nil
}

// databaseNameFor 是 databaseName 的纯函数部分，便于自测。
func databaseNameFor(rel string) string {
	slug := sanitizeSlug(rel)
	full := dbPrefix + slug
	if len(full) <= identMaxBytes {
		return full
	}
	// 截断之后不能撞名：路径前缀相同的包可能只差最后一段。用完整路径的
	// 哈希做后缀，长度取 8 个十六进制字符（32 位）——包路径的数量远小于
	// 这个空间，撞名在实践中不会发生，而真撞上时表现是一次「库名不对」
	// 而不是静默共享（见下面的自测）。
	sum := sha256.Sum256([]byte(rel))
	suffix := hex.EncodeToString(sum[:4])
	keep := identMaxBytes - len(dbPrefix) - 1 - len(suffix)
	return dbPrefix + slug[:keep] + "_" + suffix
}

// sanitizeSlug 把模块内路径变成标识符可用的小写串。
//
// 只保留 [a-z0-9] 与下划线；其余（`/`、`.`、`-`、大写）一律换成下划线。
// 输出是 ASCII，所以按字节截断不会切坏多字节字符。
func sanitizeSlug(rel string) string {
	if rel == "" || rel == "." {
		return "root"
	}
	var b strings.Builder
	b.Grow(len(rel))
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b.WriteByte(c)
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c - 'A' + 'a')
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// moduleRoot 从 dir 往上找 go.mod。
func moduleRoot(dir string) (string, error) {
	d := dir
	for i := 0; i < 64; i++ {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", fmt.Errorf("从 %s 往上没找到 go.mod：测试库名由包路径派生，缺少模块根就无从算起", dir)
}

// quoteIdent 给标识符加双引号。库名由本包生成（只含 [a-z0-9_]），
// 这里仍然做转义，是为了让「名字来自别处」时也不会成为注入点。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// advisoryKey 把库名映射成 advisory lock 的键。同一库名在任何进程里
// 得到同一个键，所以同名库的并发运行会排队而不是互相删库。
func advisoryKey(name string) int64 {
	sum := sha256.Sum256([]byte("larkax-testdb:" + name))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
