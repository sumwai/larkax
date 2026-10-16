package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/migrate"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 为什么这一层不能用内存实现测
//
// 本包要保证的性质全都**只在真库上存在**：
//
//   - 创建与校验共用同一份哈希口径（两边各写一遍时，只有真库能暴露错配）
//   - key_hash 上的唯一索引挡下重复
//   - JOIN users 之后的用户状态、外键级联删除
//   - timestamptz 从 Go 的 time.Time 转过去再转回来（过期判定的边界）
//
// 没有 LARKAX_DB_DSN 时跳过，而不是失败：本机的开发环境未必起着库。
// CI 里那个环境变量一直在，所以这个跳过的分支不会掩盖问题。
//
// ctx 由调用方传入，测试辅助函数不自己造：同一次用例里的每个查询都属于
// 同一次操作，让它们共享一个 context 才是可用的行为——将来给用例加上
// 超时或取消时，被取消的应该是整条链，而不是其中一个查询。
func testDB(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("LARKAX_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}

	handle, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if err := handle.PingContext(ctx); err != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", err)
	}

	// goose 自己记录版本，重复调用是幂等的。
	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.UpContext(ctx, handle, "."); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return handle
}

// 每个用例用独立的用户，避免彼此干扰：清空全库会让用例之间产生顺序依赖，
// 而「时间戳 + 进程内计数」在同一个库上重复跑也不会撞上上一次留下的行。
var userSeq atomic.Int64

func uniqueUsername() string {
	return fmt.Sprintf("auth-it-%d-%d", time.Now().UnixNano(), userSeq.Add(1))
}

// newUser 建一个用户并登记清理。清理靠 ON DELETE CASCADE 把它的 key 一起带走。
func newUser(ctx context.Context, t *testing.T, db *sql.DB, status string) User {
	t.Helper()

	u := User{Username: uniqueUsername()}
	err := db.QueryRowContext(ctx,
		`INSERT INTO users (username, status) VALUES ($1, $2) RETURNING id`,
		u.Username, status,
	).Scan(&u.ID)
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	t.Cleanup(func() {
		// 清理必须跑完，所以用一份不带取消的派生 context：调用方的 ctx
		// 在测试收尾时多半已经取消，带着它去 DELETE 只会静默地少删一行，
		// 而那种失败在输出里看不出来（LESSONS.md 第 16 条）。
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM users WHERE id = $1`, u.ID); err != nil {
			t.Logf("清理用户 %d 失败: %v", u.ID, err)
		}
	})
	return u
}

// request 造一个带着指定头的最小请求。
func request(ctx context.Context, header, value string) *http.Request {
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(header, value)
	return r
}

// TestCreateThenAuthenticate 是「创建与校验同口径」的落点。
//
// 两处哈希口径不一致的表现就是这条用例失败：刚建的 key 立刻说无效。
// 它是这一层最值钱的用例——那种错配在别处极难定位，两端各自看都对。
func TestCreateThenAuthenticate(t *testing.T) {
	db := testDB(t.Context(), t)
	store := New(db)
	owner := newUser(t.Context(), t, db, userStatusActive)

	created, err := store.Create(t.Context(), CreateParams{
		UserID: owner.ID,
		Name:   "集成测试",
	})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	if created.Plaintext == "" {
		t.Fatal("创建时没有返回明文：明文只返回一次，这里空掉就再也拿不到了")
	}

	// 两套协议的头各走一遍：客户端用哪个，鉴权都得认。
	for _, tc := range []struct{ name, header, value string }{
		{"Bearer", "Authorization", "Bearer " + created.Plaintext},
		{"x-api-key", "x-api-key", created.Plaintext},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.Authenticate(t.Context(), request(t.Context(), tc.header, tc.value))
			if err != nil {
				t.Fatalf("刚创建的 key 认证失败: %v", err)
			}
			if got.ID != owner.ID {
				t.Errorf("用户 ID = %d，期望 %d", got.ID, owner.ID)
			}
			if got.Username != owner.Username {
				t.Errorf("用户名 = %q，期望 %q", got.Username, owner.Username)
			}
			if got.KeyID != created.ID {
				t.Errorf("KeyID = %d，期望 %d", got.KeyID, created.ID)
			}
		})
	}
}

// TestCreateStoresOnlyHash 检查库里**没有**明文，且存的东西口径正确。
func TestCreateStoresOnlyHash(t *testing.T) {
	db := testDB(t.Context(), t)
	store := New(db)
	owner := newUser(t.Context(), t, db, userStatusActive)

	const name = "集成测试"
	created, err := store.Create(t.Context(), CreateParams{UserID: owner.ID, Name: name})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	var (
		keyHash   string
		keyPrefix string
		gotName   string
		status    string
	)
	err = db.QueryRowContext(t.Context(), `
		SELECT key_hash, key_prefix, name, status FROM api_keys WHERE id = $1`, created.ID,
	).Scan(&keyHash, &keyPrefix, &gotName, &status)
	if err != nil {
		t.Fatalf("读回 key 失败: %v", err)
	}

	if keyHash != HashKey(created.Plaintext) {
		t.Errorf("库里的 key_hash 与 HashKey(明文) 不一致：创建与校验的口径不同")
	}
	if keyHash == created.Plaintext {
		t.Error("明文被原样存进了 key_hash")
	}
	if keyPrefix != KeyPrefix(created.Plaintext) {
		t.Errorf("key_prefix = %q，期望 %q", keyPrefix, KeyPrefix(created.Plaintext))
	}
	if gotName != name {
		t.Errorf("name = %q，期望 %q", gotName, name)
	}
	// 默认状态必须是可以用的：建完不能用的 key 等于没建。
	if status != keyStatusActive {
		t.Errorf("status = %q，期望 %q", status, keyStatusActive)
	}
}

// TestCreateKeysAreDistinct 一次建两把：它们都能用，而且是两把。
func TestCreateKeysAreDistinct(t *testing.T) {
	db := testDB(t.Context(), t)
	store := New(db)
	owner := newUser(t.Context(), t, db, userStatusActive)

	first, err := store.Create(t.Context(), CreateParams{UserID: owner.ID})
	if err != nil {
		t.Fatalf("创建第一把 key 失败: %v", err)
	}
	second, err := store.Create(t.Context(), CreateParams{UserID: owner.ID})
	if err != nil {
		t.Fatalf("创建第二把 key 失败: %v", err)
	}

	if first.Plaintext == second.Plaintext {
		t.Fatal("两次创建得到同一把明文")
	}
	if first.Prefix == second.Prefix {
		t.Errorf("两把 key 的前缀相同（%q）：前缀的用途正是在列表里区分它们", first.Prefix)
	}

	for _, created := range []Created{first, second} {
		got, err := store.Authenticate(t.Context(),
			request(t.Context(), "x-api-key", created.Plaintext))
		if err != nil {
			t.Fatalf("key id=%d 认证失败: %v", created.ID, err)
		}
		if got.KeyID != created.ID {
			t.Errorf("KeyID = %d，期望 %d", got.KeyID, created.ID)
		}
	}
}

// TestAuthenticateUpdatesLastUsedAt 检查「用过」这件事被记下来了。
func TestAuthenticateUpdatesLastUsedAt(t *testing.T) {
	db := testDB(t.Context(), t)
	store := New(db)
	owner := newUser(t.Context(), t, db, userStatusActive)

	created, err := store.Create(t.Context(), CreateParams{UserID: owner.ID})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	lastUsed := func() sql.NullTime {
		t.Helper()
		var v sql.NullTime
		if err := db.QueryRowContext(t.Context(),
			`SELECT last_used_at FROM api_keys WHERE id = $1`, created.ID).Scan(&v); err != nil {
			t.Fatalf("读 last_used_at 失败: %v", err)
		}
		return v
	}

	if lastUsed().Valid {
		t.Fatal("刚创建的 key 已经有 last_used_at")
	}

	if _, err := store.Authenticate(t.Context(),
		request(t.Context(), "Authorization", "Bearer "+created.Plaintext)); err != nil {
		t.Fatalf("认证失败: %v", err)
	}

	if !lastUsed().Valid {
		t.Fatal("认证成功后 last_used_at 仍是空")
	}
}

// TestAuthenticateRejectsUnusableKeys 逐个走一遍「不能用的 key」。
//
// 每种情况的**对外**结果都一样（errors.Is(err, ErrUnauthenticated)），
// 所以这里断言的是 Reason——它只进日志，但排查时全靠它。
func TestAuthenticateRejectsUnusableKeys(t *testing.T) {
	ctx := t.Context()

	cases := []struct {
		name   string
		reason Reason
		// setup 造出这个场景，返回要拿去认证的明文。
		setup func(t *testing.T, db *sql.DB, store *Store) string
	}{
		{
			name:   "哈希查不到",
			reason: ReasonKeyNotFound,
			setup: func(t *testing.T, _ *sql.DB, _ *Store) string {
				// 形状合法但从没建过的 key。它的哈希不在库里。
				key, err := NewKey()
				if err != nil {
					t.Fatalf("生成 key 失败: %v", err)
				}
				return key
			},
		},
		{
			name:   "已停用",
			reason: ReasonKeyDisabled,
			setup: func(t *testing.T, db *sql.DB, store *Store) string {
				created, _ := createKey(ctx, t, db, store)
				setStatus(ctx, t, db, created.ID, keyStatusDisabled)
				return created.Plaintext
			},
		},
		{
			name:   "已吊销",
			reason: ReasonKeyRevoked,
			setup: func(t *testing.T, db *sql.DB, store *Store) string {
				created, _ := createKey(ctx, t, db, store)
				setStatus(ctx, t, db, created.ID, keyStatusRevoked)
				return created.Plaintext
			},
		},
		{
			name:   "已过期",
			reason: ReasonKeyExpired,
			setup: func(t *testing.T, db *sql.DB, store *Store) string {
				owner := newUser(ctx, t, db, userStatusActive)
				past := time.Now().Add(-time.Minute)
				created, err := store.Create(ctx, CreateParams{UserID: owner.ID, ExpiresAt: &past})
				if err != nil {
					t.Fatalf("创建 key 失败: %v", err)
				}
				return created.Plaintext
			},
		},
		{
			name:   "用户已封禁",
			reason: ReasonUserInactive,
			setup: func(t *testing.T, db *sql.DB, store *Store) string {
				banned := newUser(ctx, t, db, "banned")
				created, err := store.Create(ctx, CreateParams{UserID: banned.ID})
				if err != nil {
					t.Fatalf("创建 key 失败: %v", err)
				}
				return created.Plaintext
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(ctx, t)
			store := New(db)
			plaintext := tc.setup(t, db, store)

			_, err := store.Authenticate(ctx, request(ctx, "Authorization", "Bearer "+plaintext))
			assertReason(t, err, tc.reason)
		})
	}
}

// TestAuthenticateUnusedKeyIsNotNotFound 反向确认：停用之后**重新启用**能用。
//
// 没有这一条时，「停用」与「永久失效」在测试里分不开——
// 而它们是两种不同的操作，前者要能撤销。
func TestAuthenticateReenabledKeyWorks(t *testing.T) {
	db := testDB(t.Context(), t)
	store := New(db)

	created, _ := createKey(t.Context(), t, db, store)
	setStatus(t.Context(), t, db, created.ID, keyStatusDisabled)

	if _, err := store.Authenticate(t.Context(),
		request(t.Context(), "x-api-key", created.Plaintext)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("停用后仍能用: %v", err)
	}

	setStatus(t.Context(), t, db, created.ID, keyStatusActive)

	if _, err := store.Authenticate(t.Context(),
		request(t.Context(), "x-api-key", created.Plaintext)); err != nil {
		t.Fatalf("重新启用后仍然不能用: %v", err)
	}
}

// TestAuthenticateDatabaseFailureIsNotUnauthenticated 钉住错误的分类。
//
// 把查询失败也报成 401，一次数据库抖动就会表现为「全站密钥突然失效」，
// 而这两种故障的处理方式完全相反：前者该重试或告警，后者该让用户换 key。
func TestAuthenticateDatabaseFailureIsNotUnauthenticated(t *testing.T) {
	db := testDB(t.Context(), t)

	// 关掉这个句柄，让下一次查询必然失败。testDB 为每个用例开的是独立句柄。
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	_, err := New(db).Authenticate(t.Context(),
		request(t.Context(), "Authorization", "Bearer test-key-placeholder"))
	if err == nil {
		t.Fatal("库已关闭却认证成功了")
	}
	if errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("数据库故障被归为认证失败（%v），调用方会回 401", err)
	}
}

func TestResolveUser(t *testing.T) {
	db := testDB(t.Context(), t)
	store := New(db)
	owner := newUser(t.Context(), t, db, userStatusActive)

	t.Run("按 ID", func(t *testing.T) {
		got, err := store.ResolveUser(t.Context(), fmt.Sprint(owner.ID))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if got != owner {
			t.Fatalf("解析到 %+v，期望 %+v", got, owner)
		}
	})

	t.Run("按用户名", func(t *testing.T) {
		got, err := store.ResolveUser(t.Context(), owner.Username)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if got != owner {
			t.Fatalf("解析到 %+v，期望 %+v", got, owner)
		}
	})

	t.Run("空标识", func(t *testing.T) {
		if _, err := store.ResolveUser(t.Context(), "  "); err == nil {
			t.Fatal("空标识没有报错")
		}
	})

	t.Run("不存在", func(t *testing.T) {
		_, err := store.ResolveUser(t.Context(), uniqueUsername())
		if err == nil {
			t.Fatal("不存在的用户没有报错")
		}
		if errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("「用户不存在」被归为认证失败: %v", err)
		}
	})
}

// createKey 建一个用户并给它建一把可用的 key。
func createKey(ctx context.Context, t *testing.T, db *sql.DB, store *Store) (Created, User) {
	t.Helper()

	owner := newUser(ctx, t, db, userStatusActive)
	created, err := store.Create(ctx, CreateParams{UserID: owner.ID})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	return created, owner
}

func setStatus(ctx context.Context, t *testing.T, db *sql.DB, keyID int64, status string) {
	t.Helper()

	if _, err := db.ExecContext(ctx,
		`UPDATE api_keys SET status = $2 WHERE id = $1`, keyID, status); err != nil {
		t.Fatalf("改 key 状态失败: %v", err)
	}
}
