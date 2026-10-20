package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/testdb"
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
	return testdb.OpenContext(ctx, t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

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

// TestCreateKeyForMissingUserIsConstraintConflict 钉住 api_keys 的写路径对
// **没被具名认出来的外键**的分类。
//
// 造法：直接给一个 users 里不存在的 id。生产上走到这里的路径是并发——
// `apikey create` 先把标识解析成用户，插入之前那个用户被删了。判据与
// 造法无关：撞上的都是 api_keys_user_id_fkey。
//
// 断言三件事，缺一不可：
//
//   - 错误链里有 dberr.ErrConstraintViolation（退出码因此落到 4，不是 1）
//   - 驱动错误仍在链里，约束名能取回（分类不替掉现场）
//   - 它没有被认成任何具名哨兵——这条外键本包没有更精确的名字
func TestCreateKeyForMissingUserIsConstraintConflict(t *testing.T) {
	db := testDB(t.Context(), t)

	_, err := New(db).Create(t.Context(), CreateParams{UserID: 900000000000000})
	if err == nil {
		t.Fatal("用户不存在时建 key 应当被外键挡下")
	}
	if !errors.Is(err, dberr.ErrConstraintViolation) {
		t.Fatalf("错误链里没有 dberr.ErrConstraintViolation：%v", err)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("错误链里没有驱动的原始错误：%v", err)
	}
	if pgErr.Code != "23503" {
		t.Errorf("SQLSTATE = %q，期望 23503", pgErr.Code)
	}
	if pgErr.ConstraintName != "api_keys_user_id_fkey" {
		t.Errorf("约束名 = %q，期望 api_keys_user_id_fkey", pgErr.ConstraintName)
	}
}

// TestCreateKeyRejectsNonPositiveUserID 钉住用户 ID 的检查在连库之前发生。
//
// 0 与负数在 users 里不可能有一行；放 0 到 INSERT 只会撞外键，报出来的是一句
// 驱动原文，而这里要的是「用户 ID 不对」。边界写成 < 0 时 0 会漏下去：它过得了
// 检查，最后以约束冲突收场，与「用户不存在」撞成同一个错误。
func TestCreateKeyRejectsNonPositiveUserID(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	for _, id := range []int64{0, -1} {
		_, err := store.Create(ctx, CreateParams{UserID: id})
		if err == nil {
			t.Fatalf("用户 ID=%d 期望报错", id)
		}
		if !strings.Contains(err.Error(), "正整数") {
			t.Errorf("用户 ID=%d 的错误信息 %q 没说清 ID 不对", id, err.Error())
		}
	}
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
//
// 它同时是节流里 `last_used_at IS NULL` 那个分支的落点：节流只决定
// 之后多久刷新一次，**第一次一定写**。
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

// TestAuthenticateThrottlesLastUsedAtWrite 钉住 last_used_at 的写入节流。
//
// 两个方向都要钉：窗口内不写、窗口外要写。只测前者的话，一个
// 「第一次之后再也不用写」的实现照样通过——而那会让这一列永久停在
// 第一次的值上，看起来与「这把 key 早就不用了」一模一样。
func TestAuthenticateThrottlesLastUsedAtWrite(t *testing.T) {
	db := testDB(t.Context(), t)
	store := New(db)
	owner := newUser(t.Context(), t, db, userStatusActive)

	created, err := store.Create(t.Context(), CreateParams{UserID: owner.ID})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}

	readLastUsed := func(t *testing.T) time.Time {
		t.Helper()
		var v time.Time
		if err := db.QueryRowContext(t.Context(),
			`SELECT last_used_at FROM api_keys WHERE id = $1`, created.ID).Scan(&v); err != nil {
			t.Fatalf("读 last_used_at 失败: %v", err)
		}
		return v
	}
	setLastUsed := func(t *testing.T, at time.Time) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(),
			`UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, created.ID, at); err != nil {
			t.Fatalf("改 last_used_at 失败: %v", err)
		}
	}
	authenticate := func(t *testing.T) {
		t.Helper()
		if _, err := store.Authenticate(t.Context(),
			request(t.Context(), "x-api-key", created.Plaintext)); err != nil {
			t.Fatalf("认证失败: %v", err)
		}
	}

	// 先认证一次，把 last_used_at 从 NULL 变成有值（那条分支另有用例钉住）。
	authenticate(t)

	t.Run("窗口内不写", func(t *testing.T) {
		// 手动放到窗口内，而不是用上一次认证留下的值：那个值只比 now()
		// 早几毫秒，「没被覆盖」与「被覆盖成一个几乎相同的值」分不开。
		//
		// 偏移取 5 秒而不是贴着窗口边缘：判定的是相等与否，写没写都分得清，
		// 而留出余量之后，这一条不会因为机器慢、两次查询之间拖了几秒就翻脸。
		setLastUsed(t, time.Now().Add(-5*time.Second))
		before := readLastUsed(t)

		authenticate(t)

		if after := readLastUsed(t); !after.Equal(before) {
			t.Errorf("窗口内又写了一次：last_used_at 从 %s 变成 %s", before, after)
		}
	})

	t.Run("窗口外要写", func(t *testing.T) {
		setLastUsed(t, time.Now().Add(-2*lastUsedWriteInterval))
		before := readLastUsed(t)

		authenticate(t)

		if after := readLastUsed(t); !after.After(before) {
			t.Errorf("窗口外没有更新：last_used_at 仍是 %s", after)
		}
	})
}

// TestAuthenticateRejectsUnusableKeys 逐个走一遍「不能用的 key」。
//
// 每种情况的**对外**结果都一样（errors.Is(err, ErrUnauthenticated)），
// 所以这里断言的是 Reason——它只进指标的标签，但排查时全靠它。
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

	t.Run("纯数字的写法一律按 ID", func(t *testing.T) {
		// 「哪些名字建不得」的判据就是这一条（见 ValidateNewUsername），所以
		// 这里在**真的解析路径**上钉一遍：同一个 ID 的几种数的写法必须都认到
		// 同一个人。ID 由序列给、值不可预知，于是比的是「几种写法指向同一个人」，
		// 而不是某个写死的 ID。
		id := fmt.Sprint(owner.ID)
		for _, ref := range []string{id, " " + id + " ", "+" + id} {
			got, err := store.ResolveUser(t.Context(), ref)
			if err != nil {
				t.Fatalf("ResolveUser(%q) 报错: %v", ref, err)
			}
			if got != owner {
				t.Errorf("ResolveUser(%q) 解析到 %+v，期望按 ID 解析到 %+v", ref, got, owner)
			}
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
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("错误链里没有 ErrUserNotFound：%v", err)
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

// setRateLimitRPM 把一把 key 的每分钟额度写成指定值。传 nil 写回 NULL。
func setRateLimitRPM(ctx context.Context, t *testing.T, db *sql.DB, keyID int64, rpm *int) {
	t.Helper()

	var value any
	if rpm != nil {
		value = *rpm
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE api_keys SET rate_limit_rpm = $2 WHERE id = $1`, keyID, value); err != nil {
		t.Fatalf("改 key 的 RPM 额度失败: %v", err)
	}
}

// TestAuthenticateReadsRateLimitRPM 是「这一列真的被读了」的落点。
//
// 它必须在真库上跑，因为要钉住的是**扫描**这一层：rate_limit_rpm 可空，
// 而 NULL 与 0 在限流那一侧的意思正相反（不限流 / 一个都不放行）。
// 把 NULL 扫进 int 会得到 0，两者就此合并成一个值——这个错只有在真库上
// 才能暴露，内存实现里没有 NULL 这个东西。
//
// 断言的是具体数字：列里写 60，读出来就是 60；列里写 0，读出来是 0
// 而不是 nil。
func TestAuthenticateReadsRateLimitRPM(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, owner := createKey(ctx, t, db, store)

	read := func() *int {
		t.Helper()
		got, err := store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext))
		if err != nil {
			t.Fatalf("认证失败: %v", err)
		}
		if got.ID != owner.ID {
			t.Fatalf("用户 ID = %d，期望 %d", got.ID, owner.ID)
		}
		return got.RateLimitRPM
	}

	// 一、默认是 NULL（这一列没有任何代码写它，新 key 必然是 NULL）。
	if got := read(); got != nil {
		t.Errorf("新 key 的 RateLimitRPM = %v，期望 nil（列值为 NULL）", *got)
	}

	// 二、写进去的数原样读出来。
	for _, want := range []int{60, 1, 0, -1} {
		setRateLimitRPM(ctx, t, db, created.ID, &want)

		got := read()
		if got == nil {
			t.Fatalf("列里写了 %d，读出来是 nil——NULL 与非 NULL 被合并了", want)
		}
		if *got != want {
			t.Errorf("RateLimitRPM = %d，期望 %d", *got, want)
		}
	}

	// 三、写回 NULL 之后再读，必须回到 nil：否则「取消额度」做不到。
	setRateLimitRPM(ctx, t, db, created.ID, nil)
	if got := read(); got != nil {
		t.Errorf("把列写回 NULL 之后 RateLimitRPM = %v，期望 nil", *got)
	}
}

// TestAuthenticateReadsQuotaLimit 是「额度上限有没有被取出来」的落点。
//
// 这一条存在的理由与上面那条相同，但代价更高一点：quota_limit 曾经**没有任何
// 读者**（写得进去、列得出来，而设了上限什么都不发生，见 issue #200）。那种
// 缺口在库里看不出来——列上有值，读的那一侧只是没人写。所以「读出来了吗」
// 必须有一条用例钉住，而不是靠「代码里搜一下有没有人读」。
//
// 与 RateLimitRPM 一样，NULL 与「0」要分得开：0 是「一分钱都不放行」，
// NULL 是「没设上限」，两者合并是这一整件事里最不能犯的错。
func TestAuthenticateReadsQuotaLimit(t *testing.T) {
	ctx := t.Context()
	db := testDB(ctx, t)
	store := New(db)

	created, owner := createKey(ctx, t, db, store)

	read := func() *money.Amount {
		t.Helper()
		got, err := store.Authenticate(ctx, request(ctx, "x-api-key", created.Plaintext))
		if err != nil {
			t.Fatalf("认证失败: %v", err)
		}
		if got.ID != owner.ID {
			t.Fatalf("用户 ID = %d，期望 %d", got.ID, owner.ID)
		}
		return got.QuotaLimit
	}

	// 一、默认是 NULL：这一列在创建时没人写，新 key 必然是 NULL。
	if got := read(); got != nil {
		t.Errorf("新 key 的 QuotaLimit = %v，期望 nil（列值为 NULL）", *got)
	}

	// 二、写进去的数原样读出来。0 要能读成一个非 nil 的 0，而不是落回 nil。
	for _, want := range []string{"100", "0", "0.00000001", "12345.67890123"} {
		setQuotaLimit(ctx, t, db, created.ID, &want)

		got := read()
		if got == nil {
			t.Fatalf("列里写了 %s，读出来是 nil——NULL 与具体金额被合并了", want)
		}
		// 比**数值**而不是比文本：列是 numeric(20, 8)，而这里的口径是
		// 「这个数对不对」，不是「它打出来几位小数」。
		if wantAmount := money.MustParse(want); got.Cmp(wantAmount) != 0 {
			t.Errorf("QuotaLimit = %s，期望 %s", got, wantAmount)
		}
	}

	// 三、写回 NULL 之后再读，必须回到 nil：否则「取消额度」做不到。
	setQuotaLimit(ctx, t, db, created.ID, nil)
	if got := read(); got != nil {
		t.Errorf("把列写回 NULL 之后 QuotaLimit = %v，期望 nil", *got)
	}
}

// setQuotaLimit 把一把 key 的额度上限写成指定值。传 nil 写回 NULL。
//
// 直接 UPDATE 而不走 auth.UpdateKey：这一条用例测的是**读**那一侧，
// 写那一侧有自己的用例（limits_integration_test.go）。从这里绕过去，读的
// 口径变了这条会失败，而写的口径变了那条会失败——两条各自指向一处。
func setQuotaLimit(ctx context.Context, t *testing.T, db *sql.DB, keyID int64, limit *string) {
	t.Helper()

	var value any
	if limit != nil {
		value = *limit
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE api_keys SET quota_limit = $2 WHERE id = $1`, keyID, value); err != nil {
		t.Fatalf("改 key 的额度上限失败: %v", err)
	}
}
