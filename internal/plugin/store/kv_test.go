package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/plugin/host"
)

// 这一组用例连真库：被测的是 SQL 本身（可见性条件、CONFLICT 覆盖、配额那两个
// 聚合查询、顾问锁），拿假的 SQL 执行器假装这些，测的是「我以为 PostgreSQL
// 会怎么做」。没有 LARKAX_DB_DSN 时整包跳过——那件事由 CI 的集成测试门禁兜住
// （见 .github/actions/check-integration-tests）。

// kvUser 建一个用户，返回 id。
//
// plugin_kv 的 user_id 有外键指向 users，所以随便编一个 id 写不进去——这是
// 表结构要的（用户注销时他的插件状态跟着走），也是这些用例必须造用户的原因。
func kvUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (username, status) VALUES ($1, 'active') RETURNING id`,
		uniqueKey("kv-user")).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// kvPlugin 造一个本次运行独一份的插件 key。
//
// plugin_kv 对 plugins 没有外键（插件可以在注销后留下自己的 KV 行，见 00003），
// 所以这里不必先往 plugins 表里插一行。
func kvPlugin(t *testing.T) string { return uniqueKey("kv-plugin") }

// kvCount 数一个命名空间里还剩几行——**不经可见性条件**，清理有没有发生要靠它。
func kvCount(t *testing.T, db *sql.DB, plugin string, userID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM plugin_kv WHERE plugin_key = $1 AND user_id = $2`,
		plugin, userID).Scan(&n); err != nil {
		t.Fatalf("数插件 KV 的行失败: %v", err)
	}
	return n
}

// kvBytes 数一个命名空间里键与值一共占多少字节，与 Set 里那条聚合同口径。
func kvBytes(t *testing.T, db *sql.DB, plugin string, userID int64) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(sum(octet_length(key) + octet_length(value)), 0)
		   FROM plugin_kv WHERE plugin_key = $1 AND user_id = $2`,
		plugin, userID).Scan(&n); err != nil {
		t.Fatalf("数插件 KV 的字节占用失败: %v", err)
	}
	return n
}

// kvExpiresAt 读一个键的到期时刻，NULL（不过期）时 Valid 为 false。
func kvExpiresAt(t *testing.T, db *sql.DB, plugin string, userID int64, key string) sql.NullTime {
	t.Helper()
	var at sql.NullTime
	if err := db.QueryRowContext(context.Background(),
		`SELECT expires_at FROM plugin_kv WHERE plugin_key = $1 AND user_id = $2 AND key = $3`,
		plugin, userID, key).Scan(&at); err != nil {
		t.Fatalf("读插件 KV 的到期时刻失败: %v", err)
	}
	return at
}

// kvSeed 直接往库里塞 n 个键（值都是 "v"，不过期），用来把用量顶到配额附近。
//
// 走 SQL 而不是 Set：Set 每次都要取锁、清理、统计，塞一千个键会慢一个数量级，
// 而那些步骤本身另有用例（见 TestKVSetPurgesExpiredRows）。
func kvSeed(t *testing.T, db *sql.DB, plugin string, userID int64, n int) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plugin_kv (plugin_key, user_id, key, value)
		 SELECT $1, $2, 'k' || i, 'v' FROM generate_series(1, $3) AS i`,
		plugin, userID, n); err != nil {
		t.Fatalf("预置插件 KV 失败: %v", err)
	}
}

func TestKVRoundTrip(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	if _, found, err := kv.Get(ctx, plugin, user, "k"); err != nil || found {
		t.Fatalf("没写过的键: found=%v err=%v，期望 false 且无错", found, err)
	}

	if err := kv.Set(ctx, plugin, user, "k", "值", 0); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, found, err := kv.Get(ctx, plugin, user, "k")
	if err != nil || !found || got != "值" {
		t.Fatalf("读回 = (%q, %v, %v)，期望 (%q, true, nil)", got, found, err, "值")
	}

	// 覆盖：同一个 (plugin, user, key) 再写一次是改值，不是新增。
	if err = kv.Set(ctx, plugin, user, "k", "新值", 0); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if got, _, _ = kv.Get(ctx, plugin, user, "k"); got != "新值" {
		t.Errorf("覆盖后读回 = %q，期望 %q", got, "新值")
	}
	if n := kvCount(t, db, plugin, user); n != 1 {
		t.Errorf("覆盖之后有 %d 行，期望 1 行", n)
	}

	deleted, err := kv.Del(ctx, plugin, user, "k")
	if err != nil || !deleted {
		t.Fatalf("删一个存在的键 = (%v, %v)，期望 (true, nil)", deleted, err)
	}
	if _, found, _ = kv.Get(ctx, plugin, user, "k"); found {
		t.Error("删完还读得到")
	}
	// 再删一次：它已经不在了，回 false——这个布尔值要准，插件才分得出
	// 「我删掉了」与「它本来就不在」。
	if deleted, err = kv.Del(ctx, plugin, user, "k"); err != nil || deleted {
		t.Fatalf("删一个不存在的键 = (%v, %v)，期望 (false, nil)", deleted, err)
	}
}

// TestKVIsolation 钉住三段隔离里的后两段：同一个插件的两个用户、同一个用户的
// 两个插件，都不该看见对方的值。
//
// 隔离写在主键上（见 00003 的注释），所以这一条同时是「实现真的带上了这两个
// 参数」的证据——漏传一个参数时，两次写会落在同一行上，第二个用户读到的就是
// 第一个用户的值，而那是跨用户的数据泄露。
func TestKVIsolation(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, other := kvPlugin(t), kvPlugin(t)
	alice, bob := kvUser(t, db), kvUser(t, db)

	if err := kv.Set(ctx, plugin, alice, "k", "alice 的值", 0); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	if _, found, err := kv.Get(ctx, plugin, bob, "k"); err != nil || found {
		t.Errorf("同一个插件的另一个用户读到了别人的值: found=%v err=%v", found, err)
	}
	if _, found, err := kv.Get(ctx, other, alice, "k"); err != nil || found {
		t.Errorf("同一个用户的另一个插件读到了别人的值: found=%v err=%v", found, err)
	}
	if got, found, _ := kv.Get(ctx, plugin, alice, "k"); !found || got != "alice 的值" {
		t.Errorf("自己读自己的值 = (%q, %v)，期望 (%q, true)", got, found, "alice 的值")
	}
	if n := kvCount(t, db, plugin, alice); n != 1 {
		t.Errorf("三次读写之后这个命名空间有 %d 行，期望 1 行", n)
	}
}

// TestKVTTLExpiry 钉住「到期之后读不到」。
//
// 这里真的等一个 TTL 过去（1.1 秒），而不是直接往库里的 expires_at 写一个过去
// 的时刻：后者只测了可见性条件，测不到 Set 算出来的那个到期时刻。两半一起要
// 用两组用例——这一条管「Set 写的 TTL 真的会到期」，下一条管「已经过期的行
// 读不到」。
func TestKVTTLExpiry(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	if err := kv.Set(ctx, plugin, user, "k", "一会儿就没了", time.Second); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, found, err := kv.Get(ctx, plugin, user, "k"); err != nil || !found {
		t.Fatalf("刚写完就该读得到: found=%v err=%v", found, err)
	}

	// 到期时刻落在未来的一小段之内：断言上下界而不是一个精确值，因为它是
	// 库的 now() 算的（见 KV.Set 的注释）。
	at := kvExpiresAt(t, db, plugin, user, "k")
	if !at.Valid {
		t.Fatal("带 TTL 写入之后 expires_at 还是 NULL")
	}
	if d := time.Until(at.Time); d <= 0 || d > 5*time.Second {
		t.Errorf("到期时刻距现在 %v，期望落在 (0, 5s] 内", d)
	}

	time.Sleep(1100 * time.Millisecond)

	if _, found, err := kv.Get(ctx, plugin, user, "k"); err != nil || found {
		t.Errorf("TTL 到期之后还读得到: found=%v err=%v", found, err)
	}
	// 已经过期的键算「不存在」：删它回 false（见 host.KVStore.Del）。
	if deleted, err := kv.Del(ctx, plugin, user, "k"); err != nil || deleted {
		t.Errorf("删一个已经过期的键 = (%v, %v)，期望 (false, nil)", deleted, err)
	}
	// 而它**没有**被删掉——清理是 Set 那一步的事（见那一边的顺手清）。
	// 两处都清的话，出问题时得先分辨是谁清的。
	if n := kvCount(t, db, plugin, user); n != 1 {
		t.Errorf("过期之后这一行还在吗: 现有 %d 行，期望 1 行（Del 不管过期行）", n)
	}
}

// TestKVReadsAndDeletesSkipExpiredRows 是上一条的另一半：库里的行已经过期时
// 读不到、也删不掉，而这与「它是什么时候写的」无关。
func TestKVReadsAndDeletesSkipExpiredRows(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO plugin_kv (plugin_key, user_id, key, value, expires_at)
		 VALUES ($1, $2, 'k', '早就过期了', now() - interval '1 hour')`,
		plugin, user); err != nil {
		t.Fatalf("预置一行过期的数据失败: %v", err)
	}

	if _, found, err := kv.Get(ctx, plugin, user, "k"); err != nil || found {
		t.Errorf("过期的行不该读得到: found=%v err=%v", found, err)
	}
	if deleted, err := kv.Del(ctx, plugin, user, "k"); err != nil || deleted {
		t.Errorf("过期的行算不存在，删它该回 false: (%v, %v)", deleted, err)
	}
}

// TestKVSetPurgesExpiredRows 钉住写入时的顺手清，以及它的范围。
//
// **只清这个命名空间**：范围写大了（比如只按 plugin 清）会把另一个用户还没
// 到期的行一起删掉，而那是丢数据；写小了（比如只清同键）等于没清。
func TestKVSetPurgesExpiredRows(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin := kvPlugin(t)
	mine, other := kvUser(t, db), kvUser(t, db)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO plugin_kv (plugin_key, user_id, key, value, expires_at) VALUES
		   ($1, $2, '烂在那儿的', 'v', now() - interval '1 hour'),
		   ($1, $3, '别人的', 'v', now() - interval '1 hour')`,
		plugin, mine, other); err != nil {
		t.Fatalf("预置过期数据失败: %v", err)
	}

	if err := kv.Set(ctx, plugin, mine, "新键", "v", 0); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	if n := kvCount(t, db, plugin, mine); n != 1 {
		t.Errorf("这个用户的命名空间里还剩 %d 行（过期的该被清掉、新写的该在），期望 1 行", n)
	}
	if n := kvCount(t, db, plugin, other); n != 1 {
		t.Errorf("另一个用户的过期行被顺手清掉了: 还剩 %d 行，期望 1 行", n)
	}
}

// TestKVOverwriteFollowsThisTTL 钉住「覆盖时到期时刻跟着这次的值走」。
//
// 留着上一个 TTL 是更容易写出来的那一种（ON CONFLICT 里少写一列），而它的
// 表现是：插件把 ttl 从 1 小时改成 0（不过期）之后，那个键照样在一小时后
// 消失——一个「我明明写了不过期」的静默。
func TestKVOverwriteFollowsThisTTL(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	if err := kv.Set(ctx, plugin, user, "k", "v", time.Hour); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if at := kvExpiresAt(t, db, plugin, user, "k"); !at.Valid {
		t.Fatal("带 TTL 写入之后 expires_at 是 NULL")
	}

	// 改成不过期：到期时刻必须被清掉。负数同样按不过期处置（host 那一层已经
	// 拒过负的 ttl，见 host/kv.go；存储这一层没有第二个合理的解释）。
	for _, ttl := range []time.Duration{0, -time.Second} {
		if err := kv.Set(ctx, plugin, user, "k", "v", ttl); err != nil {
			t.Fatalf("ttl=%v 写入失败: %v", ttl, err)
		}
		if at := kvExpiresAt(t, db, plugin, user, "k"); at.Valid {
			t.Errorf("ttl=%v 覆盖之后 expires_at 还是 %v，期望 NULL", ttl, at.Time)
		}
	}
	if _, found, _ := kv.Get(ctx, plugin, user, "k"); !found {
		t.Error("不过期之后读不到了")
	}
}

// TestKVQuotaRejectsNewKeys 钉住键数配额：到顶之后**新增**被拒，而**覆盖**照旧。
//
// 覆盖那一半不是凑数的：配额按「写完之后的状态」算，写成「每写一次就加一」的话
// 插件在到顶之后连改一个已存在的键都做不到，而它唯一的自救手段正是改小值。
func TestKVQuotaRejectsNewKeys(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	kvSeed(t, db, plugin, user, host.KVMaxKeys)

	err := kv.Set(ctx, plugin, user, "多出来的", "v", 0)
	if !errors.Is(err, host.ErrKVQuotaExceeded) {
		t.Fatalf("键数到顶时的写入错误 = %v，期望包住 host.ErrKVQuotaExceeded", err)
	}
	if n := kvCount(t, db, plugin, user); n != host.KVMaxKeys {
		t.Errorf("被拒的写入却落库了: 现在 %d 行，期望 %d 行", n, host.KVMaxKeys)
	}

	if err := kv.Set(ctx, plugin, user, "k1", "改一下已存在的键", 0); err != nil {
		t.Errorf("到顶之后覆盖一个已存在的键不该被拒: %v", err)
	}
	if n := kvCount(t, db, plugin, user); n != host.KVMaxKeys {
		t.Errorf("覆盖之后变成 %d 行，期望 %d 行", n, host.KVMaxKeys)
	}
}

// TestKVQuotaRejectsBytes 钉住字节配额，以及它是按**写完之后的状态**算的：
// 把一个大值改小必须放行。
func TestKVQuotaRejectsBytes(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	// 一个键就把字节数顶到上限（键 3 字节 + 值），刚好在上限之内。
	big := strings.Repeat("v", host.KVMaxBytes-3)
	if err := kv.Set(ctx, plugin, user, "big", big, 0); err != nil {
		t.Fatalf("写到上限之内的写入失败: %v", err)
	}

	err := kv.Set(ctx, plugin, user, "另一个", "v", 0)
	if !errors.Is(err, host.ErrKVQuotaExceeded) {
		t.Fatalf("字节数到顶时的写入错误 = %v，期望包住 host.ErrKVQuotaExceeded", err)
	}

	// 把大值改小：写完之后的用量远在上限之内，按增量判会误拒。
	if err := kv.Set(ctx, plugin, user, "big", "小值", 0); err != nil {
		t.Errorf("把大值改小不该被拒: %v", err)
	}
	if got, found, _ := kv.Get(ctx, plugin, user, "big"); !found || got != "小值" {
		t.Errorf("改小之后读回 = (%q, %v)", got, found)
	}
}

// TestKVQuotaByteSumCountsTheNewValueNotTheDifference 钉住字节配额里那个**加号**：
// 用量是「其它键的字节 + 新写的键与值」，不是「其它键的字节 + 键减值」。
//
// 上一条用例（TestKVQuotaRejectsBytes）构造不出能分开两版的输入：
// 那时占用已经顶到上限、后写的键与值又都很短，两种算式都超限。
// 这里让**值**足够长、其它键的占用卡在两个算式之间——正确的那一版超限
// （S+k+v > 上限），把内层加号改成减号的那一版不超限（S+k-v ≤ 上限）。
// 于是「拒掉」这件事本身就在挑版本。
//
// 断言落在报出来的具体数值与「被拒之后库里一格没动」上：
// 只看错误非零的话，一个先写进去再报错的实现也能过。
func TestKVQuotaByteSumCountsTheNewValueNotTheDifference(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	const (
		seedKey    = "seed"  // 4 字节
		probeKey   = "probe" // 5 字节
		seedSlack  = 500     // 其它键的占用压在上限之下这么多
		probeBytes = 600     // 值的长度：两种算式的结果相差 2*600
	)
	occupy := int64(host.KVMaxBytes - seedSlack)
	if err := kv.Set(ctx, plugin, user, seedKey,
		strings.Repeat("v", int(occupy)-len(seedKey)), 0); err != nil {
		t.Fatalf("预置 %d 字节的占用失败: %v", occupy, err)
	}
	if got := kvBytes(t, db, plugin, user); got != occupy {
		t.Fatalf("预置之后占用 = %d 字节，期望 %d", got, occupy)
	}

	value := strings.Repeat("v", probeBytes)
	used := occupy + int64(len(probeKey)+len(value))
	// 夹具自检：正确的那一版必须超限，把加号改成减号的那一版必须放行。
	if used <= int64(host.KVMaxBytes) {
		t.Fatalf("夹具错了：正确算式的结果 %d 没超过上限 %d", used, host.KVMaxBytes)
	}
	if reduced := used - 2*int64(probeBytes); reduced > int64(host.KVMaxBytes) {
		t.Fatalf("夹具错了：减号那一版的结果 %d 也超过上限 %d，两版分不开", reduced, host.KVMaxBytes)
	}

	err := kv.Set(ctx, plugin, user, probeKey, value, 0)
	if !errors.Is(err, host.ErrKVQuotaExceeded) {
		t.Fatalf("写入错误 = %v，期望包住 host.ErrKVQuotaExceeded", err)
	}
	if want := fmt.Sprintf("字节数会达到 %d", used); !strings.Contains(err.Error(), want) {
		t.Errorf("错误信息 %q 里没有 %q：报出来的必须是写完之后的总字节数", err.Error(), want)
	}
	if n := kvCount(t, db, plugin, user); n != 1 {
		t.Errorf("被拒的写入却改了行数：现在 %d 行，期望 1 行（只有预置那一行，探针不该落库）", n)
	}
	if got := kvBytes(t, db, plugin, user); got != occupy {
		t.Errorf("被拒的写入却改了占用：现在 %d 字节，期望 %d", got, occupy)
	}
}

// TestKVQuotaHoldsUnderConcurrentWrites 钉住配额是**硬的**：同一个命名空间上
// 并发的两次写不会各自读到同一份旧用量、双双通过。
//
// 判据是「写完之后超没超」，不是「谁成功了」——所以断言写成「恰好一个成功、
// 总数正好到顶」。没有那把按 (plugin, user) 取名的事务级顾问锁时，两次写会
// 各读到 1023 个键、各插一个，总数变成 1025。两个 goroutine 实际重叠的时间
// 很短，所以这一条**有可能**在没锁的实现上也过（假阳性方向的不可靠），
// 但不会在正确的实现上失败。
func TestKVQuotaHoldsUnderConcurrentWrites(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, user := kvPlugin(t), kvUser(t, db)

	kvSeed(t, db, plugin, user, host.KVMaxKeys-1)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, key := range []string{"并发一", "并发二"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			err := kv.Set(ctx, plugin, user, key, "v", 0)
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}(key)
	}
	wg.Wait()

	rejected := 0
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, host.ErrKVQuotaExceeded):
			rejected++
		default:
			t.Fatalf("并发写出现了意料之外的错误: %v", err)
		}
	}
	if rejected != 1 {
		t.Errorf("两次并发写里被拒的有 %d 个，期望 1 个（另一个名额只剩一个）", rejected)
	}
	if n := kvCount(t, db, plugin, user); n != host.KVMaxKeys {
		t.Errorf("并发写之后有 %d 行，期望正好 %d 行", n, host.KVMaxKeys)
	}
}

// TestKVMethodsDoNotLeakAcrossNamespacesByKeyAlone 是给 review 看的一条：
// 两个命名空间里的同一个键名必须各自独立（值不同、删一个不影响另一个）。
func TestKVMethodsDoNotLeakAcrossNamespacesByKeyAlone(t *testing.T) {
	db := testDB(t)
	kv := NewKV(db)
	ctx := context.Background()
	plugin, other := kvPlugin(t), kvPlugin(t)
	user, otherUser := kvUser(t, db), kvUser(t, db)

	cases := []struct {
		name   string
		plugin string
		user   int64
	}{
		{"原命名空间", plugin, user},
		{"同一个插件的另一个用户", plugin, otherUser},
		{"同一个用户的另一个插件", other, user},
	}
	for i, tc := range cases {
		if err := kv.Set(ctx, tc.plugin, tc.user, "同名键", fmt.Sprintf("第 %d 个", i), 0); err != nil {
			t.Fatalf("%s 写入失败: %v", tc.name, err)
		}
	}
	for i, tc := range cases {
		got, found, err := kv.Get(ctx, tc.plugin, tc.user, "同名键")
		if err != nil || !found || got != fmt.Sprintf("第 %d 个", i) {
			t.Errorf("%s 读回 = (%q, %v, %v)，期望 (%q, true, nil)",
				tc.name, got, found, err, fmt.Sprintf("第 %d 个", i))
		}
	}

	// 删掉第一个，另外两个必须还在。
	if deleted, err := kv.Del(ctx, plugin, user, "同名键"); err != nil || !deleted {
		t.Fatalf("删除 = (%v, %v)", deleted, err)
	}
	for _, tc := range cases[1:] {
		if _, found, _ := kv.Get(ctx, tc.plugin, tc.user, "同名键"); !found {
			t.Errorf("删掉原命名空间的键之后，%s 的也没了", tc.name)
		}
	}
}
