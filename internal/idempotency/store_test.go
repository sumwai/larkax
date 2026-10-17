package idempotency

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/migrate"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 为什么这一层不能用内存实现测
//
// 本包要保证的性质全都**只在真库上存在**：
//
//   - 唯一键 (api_key_id, key) 挡下的重复插入——「同键并发」的判据就是
//     「插入影响 0 行」，而影响行数是数据库的行为，不是 Go 的
//   - `UPDATE ... WHERE state = 'in_progress'` 的影响行数——它同时是
//     「状态能不能转」和「转没转成」两件事的依据
//   - bytea 的逐字节往返（响应体要原样重放）
//
// 拿内存假装这些，测的是「我以为数据库会怎么做」，而不是它真的会怎么做。
//
// 没有 LARKAX_DB_DSN 时跳过，而不是失败：本机的开发环境未必起着库。
// **注意「跳过」与「通过」在输出里长得一样**（见 LESSONS.md 第 16 条），
// 所以报数时要把两者分开。
func testDB(t *testing.T) *sql.DB {
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

	if err := handle.PingContext(context.Background()); err != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", err)
	}

	// goose 自己记录版本，重复调用是幂等的。
	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.UpContext(context.Background(), handle, "."); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return handle
}

// 每个用例用独立的标识，避免彼此干扰。
//
// 用「时间戳 + 进程内计数」而不是纯计数：纯计数在同一个库上跑第二次
// `go test` 时会从头开始，撞上上一次留下的行——而测试之间不该有跨运行的依赖。
var (
	seqMu   sync.Mutex
	seqN    int64
	seqBase = time.Now().UnixNano() % 1_000_000_000
)

func unique(prefix string) string {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqN++
	return fmt.Sprintf("%s-%d-%d", prefix, seqBase, seqN)
}

// seedUser 建一个用户，返回用户 id。
//
// api_keys.user_id 有外键指向 users，所以造 key 之前得先有用户。
func seedUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (username, status) VALUES ($1, 'active') RETURNING id`,
		unique("idem-user"),
	).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// issueKey 给用户签发一把 API Key，返回 api_keys.id。
//
// 用生产代码里的那一份实现，而不是自己拼一行 INSERT：哈希口径必须与鉴权
// 那一侧完全一致，自己写一遍就等于把这个口径抄成两份，而两份早晚会分叉。
func issueKey(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()

	created, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: userID,
		Name:   "idempotency",
	})
	if err != nil {
		t.Fatalf("签发 API Key 失败: %v", err)
	}
	return created.ID
}

// seedBillingRequest 插一条最小的 billing_requests 行，返回它的 id。
//
// 只为满足 idempotency_keys.billing_request_id 的外键：本包要测的是
// 「那一列有没有把值正确地带回来」。预留自己的写入路径由
// internal/ledger/store 覆盖，本包不重复造那一套。
func seedBillingRequest(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO billing_requests (request_id, user_id, canonical_model, protocol)
		VALUES ($1, $2, 'gpt-4o', 'openai')
		RETURNING id`,
		unique("idem-req"), userID,
	).Scan(&id); err != nil {
		t.Fatalf("建账单行失败: %v", err)
	}
	return id
}

// hashOf 造一个指纹。这一层不关心指纹怎么算出来的（那是纯单测的事），
// 只关心这串字节能不能原样往返。
func hashOf(b byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = b
	}
	return h
}

// claim 在**自己的事务**里插一条幂等记录。
//
// 与生产一致：ClaimInTx 不接受自开事务的调用方（它与预留必须同一个事务），
// 所以这里也得自己起一个。失败时回滚，成功时提交——「提交之后读得回来」
// 正是本包要证明的事。
func claim(t *testing.T, db *sql.DB, c *Claim, reservationID int64) error {
	t.Helper()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	if err := ClaimInTx(ctx, tx, c, reservationID); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			t.Fatalf("回滚失败: %v", rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	return nil
}

// ── 声明与读取 ──────────────────────────────────────────────────────────

func TestClaimThenLookupRoundTrip(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)
	reservationID := seedBillingRequest(t, db, userID)
	key := unique("key")
	hash := hashOf(0x11)

	if err := claim(t, db, &Claim{APIKeyID: keyID, Key: key, Hash: hash}, reservationID); err != nil {
		t.Fatalf("ClaimInTx 失败: %v", err)
	}

	rec, err := s.Lookup(ctx, keyID, key)
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if rec.State != StateInProgress {
		t.Errorf("刚声明完的状态 = %q，期望 %q", rec.State, StateInProgress)
	}
	if rec.Hash != hash {
		t.Errorf("指纹 = %x，期望 %x", rec.Hash, hash)
	}
	if rec.BillingRequestID != reservationID {
		t.Errorf("账单行 id = %d，期望 %d——这一列是排查「重试被拒但钱扣没扣」的唯一入口",
			rec.BillingRequestID, reservationID)
	}
}

// 同一个 (api_key_id, key) 第二次声明返回 ErrClaimed，且**不动**已存在那一行。
//
// 断言「没动」不是多余的：`ON CONFLICT DO NOTHING` 改成 `DO UPDATE` 之后，
// 错误码那一条依然成立（仍然返回 ErrClaimed），而现场变成「第二次请求的
// 指纹与预留 id 覆盖掉了第一次的」——那会让第一次的响应再也找不回来。
func TestSecondClaimIsRejectedAndLeavesFirstRowIntact(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)
	firstReservation := seedBillingRequest(t, db, userID)
	secondReservation := seedBillingRequest(t, db, userID)
	key := unique("key")
	firstHash := hashOf(0x11)

	if err := claim(t, db, &Claim{APIKeyID: keyID, Key: key, Hash: firstHash}, firstReservation); err != nil {
		t.Fatalf("第一次 ClaimInTx 失败: %v", err)
	}

	err := claim(t, db, &Claim{APIKeyID: keyID, Key: key, Hash: hashOf(0x22)}, secondReservation)
	if !errors.Is(err, ErrClaimed) {
		t.Fatalf("第二次 ClaimInTx 的错误 = %v，期望 ErrClaimed——它是竞态信号，"+
			"调用方该去读那条记录而不是把它回给客户端", err)
	}

	rec, err := s.Lookup(ctx, keyID, key)
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if rec.Hash != firstHash {
		t.Errorf("已存在那一行的指纹被第二次声明改掉了: %x，期望 %x", rec.Hash, firstHash)
	}
	if rec.State != StateInProgress {
		t.Errorf("已存在那一行的状态被改掉了: %q", rec.State)
	}
	if rec.BillingRequestID != firstReservation {
		t.Errorf("已存在那一行的账单行 id = %d，期望 %d", rec.BillingRequestID, firstReservation)
	}
}

// 作用域是 api_key_id，不是全局的键字符串。
//
// 少了这个条件，一次跨租户的撞键会让 A 用户拿到 B 用户的响应体——而那正是
// 「键全局唯一」这条错路会带来的后果：一个用户能用掉另一个用户的键。
func TestClaimScopeIsAPIKey(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyA := issueKey(t, db, userID)
	keyB := issueKey(t, db, userID)
	// 两把 key 用**同一个键字符串**：这正是要压的情形。
	key := unique("shared-key")
	hashA := hashOf(0xA1)
	hashB := hashOf(0xB2)

	if err := claim(t, db, &Claim{APIKeyID: keyA, Key: key, Hash: hashA}, 0); err != nil {
		t.Fatalf("第一把 key 的 ClaimInTx 失败: %v", err)
	}
	if err := claim(t, db, &Claim{APIKeyID: keyB, Key: key, Hash: hashB}, 0); err != nil {
		t.Fatalf("换一把 key、同一个键字符串不该冲突: %v", err)
	}

	recA, err := s.Lookup(ctx, keyA, key)
	if err != nil {
		t.Fatalf("用第一把 key Lookup 失败: %v", err)
	}
	if recA.Hash != hashA {
		t.Errorf("用第一把 key 读到的指纹 = %x，期望 %x——读到的是另一把 key 的记录", recA.Hash, hashA)
	}

	recB, err := s.Lookup(ctx, keyB, key)
	if err != nil {
		t.Fatalf("用第二把 key Lookup 失败: %v", err)
	}
	if recB.Hash != hashB {
		t.Errorf("用第二把 key 读到的指纹 = %x，期望 %x", recB.Hash, hashB)
	}
}

// 不存在的键返回 ErrNotFound（而不是零值记录或别的错误）。
//
// 用 errors.Is 断言而不是比较错误文本：调用方按这个哨兵值分支，
// 文本改一个字就会让「键不存在」与「库出错」被混为一谈。
func TestLookupMissingKeyReturnsErrNotFound(t *testing.T) {
	db := testDB(t)
	s := New(db)

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)

	rec, err := s.Lookup(context.Background(), keyID, unique("never-claimed"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup 的错误 = %v，期望 ErrNotFound", err)
	}
	if rec != nil {
		t.Errorf("没有记录时应当返回 nil，得到 %+v", rec)
	}
}

// ── 写回响应 ────────────────────────────────────────────────────────────

// 走完之后状态转 completed，响应的三个字段逐字节读得回来。
//
// 用一段**含非 ASCII 字节**的 SSE 文本：重放要的是逐字节相同，所以
// 「长度对得上」不够——多字节字符被替换、换行被规范化，长度都可以不变。
func TestCompleteStoresResponseVerbatim(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)
	key := unique("key")

	if err := claim(t, db, &Claim{APIKeyID: keyID, Key: key, Hash: hashOf(0x33)}, 0); err != nil {
		t.Fatalf("ClaimInTx 失败: %v", err)
	}

	body := []byte("event: message_start\ndata: {\"text\":\"你好，世界\"}\n\n" +
		"event: message_stop\ndata: {}\n\n")
	const contentType = "text/event-stream; charset=utf-8"
	const status = 200

	if err := s.Complete(ctx, keyID, key, status, contentType, body); err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}

	rec, err := s.Lookup(ctx, keyID, key)
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if rec.State != StateCompleted {
		t.Errorf("状态 = %q，期望 %q", rec.State, StateCompleted)
	}
	if rec.ResponseStatus != status {
		t.Errorf("响应码 = %d，期望 %d", rec.ResponseStatus, status)
	}
	if rec.ResponseContentType != contentType {
		t.Errorf("Content-Type = %q，期望 %q", rec.ResponseContentType, contentType)
	}
	if !bytes.Equal(rec.ResponseBody, body) {
		t.Errorf("响应体不是逐字节相同:\n  写入 %q\n  读回 %q", body, rec.ResponseBody)
	}
}

// 在 completed 上再来一次 Complete 要报错。
//
// 「影响 0 行」不是成功。它与「写进去了」在返回值上完全一样，而现场是
// 「第二次的响应把第一次的覆盖掉了」或者「什么都没发生」——两者都不会
// 有任何东西报出来（见 LESSONS.md 第 16 条）。
func TestCompleteTwiceIsRejected(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)
	key := unique("key")

	if err := claim(t, db, &Claim{APIKeyID: keyID, Key: key, Hash: hashOf(0x44)}, 0); err != nil {
		t.Fatalf("ClaimInTx 失败: %v", err)
	}
	firstBody := []byte(`{"ok":true}`)
	if err := s.Complete(ctx, keyID, key, 200, "application/json", firstBody); err != nil {
		t.Fatalf("第一次 Complete 失败: %v", err)
	}

	err := s.Complete(ctx, keyID, key, 500, "application/json", []byte(`{"ok":false}`))
	if err == nil {
		t.Fatal("在 completed 上再 Complete 一次应当报错——影响 0 行不是成功")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("错误 = %v，但它**不该**是 ErrNotFound：记录在，只是状态不对。"+
			"这两种 0 行的处置相反——记录不在是正常路径（预留回滚了）该静默，"+
			"状态不对是真异常该报警；混在一起就把那条告警淹掉了", err)
	}

	// 而且第一次的响应没有被覆盖
	rec, lookupErr := s.Lookup(ctx, keyID, key)
	if lookupErr != nil {
		t.Fatalf("Lookup 失败: %v", lookupErr)
	}
	if rec.ResponseStatus != 200 || !bytes.Equal(rec.ResponseBody, firstBody) {
		t.Errorf("第一次的响应被改掉了: 状态 %d 响应体 %q", rec.ResponseStatus, rec.ResponseBody)
	}
}

// 没走完的请求标成 failed，且在 failed 上再 Fail 一次同样报错。
func TestFailThenFailAgainIsRejected(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)
	key := unique("key")

	if err := claim(t, db, &Claim{APIKeyID: keyID, Key: key, Hash: hashOf(0x55)}, 0); err != nil {
		t.Fatalf("ClaimInTx 失败: %v", err)
	}

	if err := s.Fail(ctx, keyID, key); err != nil {
		t.Fatalf("Fail 失败: %v", err)
	}
	rec, err := s.Lookup(ctx, keyID, key)
	if err != nil {
		t.Fatalf("Lookup 失败: %v", err)
	}
	if rec.State != StateFailed {
		t.Errorf("状态 = %q，期望 %q", rec.State, StateFailed)
	}

	if err := s.Fail(ctx, keyID, key); err == nil {
		t.Error("在 failed 上再 Fail 一次应当报错——影响 0 行不是成功")
	} else if errors.Is(err, ErrNotFound) {
		t.Errorf("错误 = %v，但它**不该**是 ErrNotFound：记录在，只是状态不对（见 Complete 那条）", err)
	}
}

// 记录根本不在时，Complete 与 Fail 都返回 ErrNotFound，而不是那句状态错误。
//
// # 为什么要分开
//
// 这两种 0 行的处置**相反**：
//
//   - 记录不在，说明这一次请求的预留整个回滚了（余额不足、没有可用渠道、
//     缺 max_tokens）。这是**正常路径**——用户余额不足是常态——调用方据此静默。
//   - 记录在而状态不对，才是真异常，调用方据此报警。
//
// 而那条告警是这个设计里**唯一**能指出「记录停在 in_progress」的信号。
// 不分开的话，它在每次余额不足时都会响一遍，等于被淹掉（一个总在喊的告警
// 与一个从不喊的告警一样没用）。
//
// 这条用例同时是「幂等记录与预留同一个事务」的另一半证据：预留失败之后
// 库里就是这个样子——一条记录都没有。
func TestTransitionsOnMissingRecordReturnErrNotFound(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)

	// 刻意**不** claim：这正是「预留回滚了」之后库里的样子。
	missing := unique("never-claimed")

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{name: "Complete", call: func() error {
			return s.Complete(ctx, keyID, missing, 200, "application/json", []byte(`{}`))
		}},
		{name: "Fail", call: func() error {
			return s.Fail(ctx, keyID, missing)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s 的错误 = %v，期望 ErrNotFound——记录不存在是正常路径，"+
					"调用方据此静默；混成状态错误会让那条唯一的告警被淹掉", tc.name, err)
			}
		})
	}
}

// ── 清理 ────────────────────────────────────────────────────────────────

// 留存期外的记录被删，期内的留着。
func TestCleanupDeletesOnlyExpiredRecords(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)
	oldKey := unique("old")
	newKey := unique("new")

	for _, key := range []string{oldKey, newKey} {
		if err := claim(t, db, &Claim{APIKeyID: keyID, Key: key, Hash: hashOf(0x66)}, 0); err != nil {
			t.Fatalf("ClaimInTx(%q) 失败: %v", key, err)
		}
	}

	// 留存按 created_at 算（见 Cleanup 的注释），所以拨的是这一列。
	if _, err := db.ExecContext(ctx, `
		UPDATE idempotency_keys SET created_at = now() - interval '48 hours'
		 WHERE api_key_id = $1 AND key = $2`, keyID, oldKey); err != nil {
		t.Fatalf("把记录拨到 48 小时前失败: %v", err)
	}

	deleted, err := s.Cleanup(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup 失败: %v", err)
	}
	// 判据收窄到**本用例自己造的那两行**，不数全库。
	//
	// Cleanup 返回的是全库删掉的条数，而库是共用的：cmd/larkaxctl 的用例
	// 也会真删一次，两个包在 `go test ./...` 里并行跑在同一个 DSN 上。拿
	// 「清理前数出来的全库到期行数 + 1」做等式，中间任何一次并发清理都会
	// 把左边改小，用例于是随机变红——而红的是它自己（见 LESSONS.md 第 26 条：
	// 断言不许对着全库共享的表做全局计数）。
	//
	// 下限仍然是有内容的：「到期的一条都没删」会让它挂，而那种情况下
	// 下面两条 Lookup 也一起挂。
	if deleted < 1 {
		t.Errorf("Cleanup 报删除 %d 条，本用例拨到 48 小时前的那 1 条至少该被删掉", deleted)
	}

	if _, err := s.Lookup(ctx, keyID, oldKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("48 小时前的那条应当被删掉，Lookup 却返回 %v", err)
	}
	if _, err := s.Lookup(ctx, keyID, newKey); err != nil {
		t.Errorf("24 小时内的那条不该被删掉: %v", err)
	}
}

// 留存时长非正数时**报错且一条都不删**。
//
// 这是配置写错的样子。把它翻译成「删掉全部」会让一次笔误清空整张表——
// 幂等记录没了不会报错，只会让重试变成第二次真实上游调用，而那是多收钱。
func TestCleanupRefusesNonPositiveRetention(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seedUser(t, db)
	keyID := issueKey(t, db, userID)
	oldKey := unique("old")

	if err := claim(t, db, &Claim{APIKeyID: keyID, Key: oldKey, Hash: hashOf(0x77)}, 0); err != nil {
		t.Fatalf("ClaimInTx 失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE idempotency_keys SET created_at = now() - interval '48 hours'
		 WHERE api_key_id = $1 AND key = $2`, keyID, oldKey); err != nil {
		t.Fatalf("把记录拨到 48 小时前失败: %v", err)
	}

	rowsOf := func() int64 {
		var n int64
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM idempotency_keys WHERE api_key_id = $1`, keyID).Scan(&n); err != nil {
			t.Fatalf("统计本用例的记录失败: %v", err)
		}
		return n
	}
	before := rowsOf()

	for _, retention := range []time.Duration{0, -time.Hour} {
		deleted, err := s.Cleanup(ctx, retention)
		if err == nil {
			t.Errorf("留存时长 %s 应当报错", retention)
		}
		if deleted != 0 {
			t.Errorf("留存时长 %s 被拒绝的清理返回了 %d 条删除，期望 0", retention, deleted)
		}
	}

	if after := rowsOf(); after != before {
		t.Errorf("被拒绝的清理删掉了 %d 行——一条都不该删", before-after)
	}
	// 正面证据：那条**已经过期**的记录还在。只数行数的话，「删了又补回来」
	// 这种不可能的事也会通过，而这里读的是那一行本身。
	if _, err := s.Lookup(ctx, keyID, oldKey); err != nil {
		t.Errorf("已经过期的记录被非正数留存期的清理删掉了: %v", err)
	}
}
