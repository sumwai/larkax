package ratelimit

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 这一组用例跑在**真的 Redis** 上：没有 LARKAX_REDIS_DSN 时整组跳过
// （与仓库里连库的那些包同一个做法）。
//
// 跳过的代价要说清楚：**「跳过」在 go test 的输出里与「通过」长得一样**，
// 退出码也都是 0。所以真 Redis 上才存在的那几条性质——键的过期时间真的
// 设上了、两个实例真的共用一份计数、反复跑之后真的不留过期键——在没有
// DSN 的环境里等于没测。CI 里加不加一个 Redis 服务，见 docs/development.md。
//
// 需要真 Redis 的理由和连库那些包是同一条：这些性质的载体是存储本身
// （过期时间、跨连接共享、键空间），拿内存装作不出来。

// redisFixture 是一份可用的共享存储，外加一个直连的客户端。
//
// 客户端用来从**这一层之外**看存储里的状态：键在不在、还剩多久过期。
// 用 RedisStore 自己去看的话，「键有没有过期时间」会变成「它自己说有没有」
// ——而那正是被测对象。
type redisFixture struct {
	store  *RedisStore
	client *redis.Client
}

func newRedisFixture(t *testing.T) redisFixture {
	t.Helper()
	dsn := os.Getenv("LARKAX_REDIS_DSN")
	if dsn == "" {
		t.Skip("没有 LARKAX_REDIS_DSN，跳过需要真 Redis 的用例")
	}
	store, err := OpenRedis(dsn)
	if err != nil {
		t.Fatalf("按 DSN 构造共享计数失败: %v", err)
	}
	raw, err := OpenRedis(dsn)
	if err != nil {
		t.Fatalf("构造直连客户端失败: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = raw.Close()
	})
	// 直连客户端就是从 RedisStore 里那一份拿去用：同一份命令集接口，
	// 测试要的是「从这一层之外看存储里的状态」。
	return redisFixture{store: store, client: raw.cmds.(*redis.Client)}
}

// midWindow 是**当前窗口的中点**。
//
// 用例里的表取它而不是 time.Now()：窗口按墙钟对齐，直接拿「现在」时，
// 一个恰好在整分钟前后几百毫秒里跑到的用例会跨过边界——那一跨，计数在新
// 窗口里从头开始，断言的数字就变成了「有时候对」。中点到两侧边界各 30 秒，
// 这些用例跑不到那里去。
func midWindow() time.Time { return time.Now().Truncate(windowLen).Add(windowLen / 2) }

// freshKeyID 给每个用例一把自己的 key，避免彼此干扰。
//
// 用纳秒时间戳而不是小整数：同一个 Redis 上可能同时跑着别的用例（以及
// 上一次运行留下来的键），固定的小整数会撞上。
func freshKeyID() int64 { return time.Now().UnixNano() }

// keyPrefix 是某个 key 在这一层写下的全部键的前缀。
func keyPrefix(keyID int64) string { return "rl:" + strconv.FormatInt(keyID, 10) + ":" }

// scanKeys 列出匹配的键（KEYS 只在这几组用例里用：数据量小，且它们本来就
// 是在验证键空间）。
func (f redisFixture) scanKeys(t *testing.T, pattern string) []string {
	t.Helper()
	keys, err := f.client.Keys(context.Background(), pattern).Result()
	if err != nil {
		t.Fatalf("列键失败: %v", err)
	}
	return keys
}

// TestRedisCountsAndExpires 是这一层最基础的一条：真的写进去了，而且真的会过期。
func TestRedisCountsAndExpires(t *testing.T) {
	f := newRedisFixture(t)
	keyID := freshKeyID()
	c := &clock{at: midWindow()}
	l, _ := newTestLimiter(c, WithStore(f.store))
	limit := rpm(5)

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow(keyID, limit); !ok {
			t.Fatalf("第 %d 次被拒", i+1)
		}
	}

	key := windowKey(keyID, c.now().Truncate(windowLen))
	got, err := f.client.Get(context.Background(), key).Int64()
	if err != nil {
		t.Fatalf("读 %s 失败: %v", key, err)
	}
	if got != 3 {
		t.Errorf("存储里的计数 = %d，期望 3", got)
	}

	// 过期时间要真的设上，而且在两倍窗口之内（见 keyTTL）。
	ttl, err := f.client.TTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("%s 的 TTL = %v：没有过期时间的键不会被清掉", key, ttl)
	}
	if ttl > keyTTL {
		t.Errorf("TTL = %v，超过了两倍窗口 %v", ttl, keyTTL)
	}

	// 用量查询读到的就是存储里那个数，而且是精确的。
	u := l.Usage(context.Background(), keyID)
	if u.Count != 3 || !u.Exact {
		t.Errorf("查询读到 %+v，期望 count=3、exact=true", u)
	}
}

// TestRedisTwoInstancesShareOneCount 是判据里那条「跨实例一致」。
//
// 两个实例 = 两个进程内 map 各一份、只共享这个 Redis 的限流器。额度 4 时
// 两边加起来恰好过 4 次。（真 Redis 与替身各测一遍：替身证明这一层有没有
// 把计数交出去，真 Redis 证明交出去之后确实落在同一个键上。）
func TestRedisTwoInstancesShareOneCount(t *testing.T) {
	f := newRedisFixture(t)
	keyID := freshKeyID()
	c := &clock{at: midWindow()}
	a, _ := newTestLimiter(c, WithStore(f.store))
	b, _ := newTestLimiter(c, WithStore(f.store))
	limit := rpm(4)

	allowed := 0
	for i := 0; i < 6; i++ {
		if ok, _ := a.Allow(keyID, limit); ok {
			allowed++
		}
		if ok, _ := b.Allow(keyID, limit); ok {
			allowed++
		}
	}
	if allowed != 4 {
		t.Errorf("两个实例合计放行了 %d 次，期望 4", allowed)
	}
}

// TestRedisKeysAreAlwaysSelfCleaning 是判据里那条「不残留」。
//
// 判据不是「键一个都不剩」——键是活的计数，它当然在，而且会在两倍窗口之后
// 自己消失。真正的判据是**每一个键都有过期时间**：没有过期时间的键永远不会
// 消失，它们会一直攒下去。同时钉住数量有界：每个 key 最多留两条（当前窗口
// 与刚过去的那一条）。
func TestRedisKeysAreAlwaysSelfCleaning(t *testing.T) {
	f := newRedisFixture(t)
	keyID := freshKeyID()
	c := &clock{at: midWindow()}
	l, _ := newTestLimiter(c, WithStore(f.store))
	limit := rpm(100)

	// 反复跑：跨过两次窗口，模拟「这个 key 一直在用」。
	for w := 0; w < 3; w++ {
		for i := 0; i < 5; i++ {
			l.Allow(keyID, limit)
		}
		c.set(c.now().Add(windowLen))
	}

	keys := f.scanKeys(t, keyPrefix(keyID)+"*")
	if len(keys) == 0 {
		t.Fatal("一个键都没写进去：这一组用例的证据范围写错了")
	}
	for _, k := range keys {
		ttl, err := f.client.TTL(context.Background(), k).Result()
		if err != nil {
			t.Fatalf("读 %s 的 TTL 失败: %v", k, err)
		}
		if ttl <= 0 {
			t.Errorf("%s 的 TTL = %v：没有过期时间的键永远不会消失", k, ttl)
		}
		if ttl > keyTTL {
			t.Errorf("%s 的 TTL = %v，超过了两倍窗口 %v", k, ttl, keyTTL)
		}
	}
	// 每个窗口一个键，而每个键活两倍窗口——所以「当前 + 前两个窗口」这三条
	// 可以同时在。上界是关键：它是**有界**的，键不会随运行时间一直攒。
	if len(keys) > 3 {
		t.Errorf("留下了 %d 个键（%v），期望不超过 3（当前窗口加两倍窗口 TTL 覆盖到的前两个）", len(keys), keys)
	}
}

// TestRedisUsageDoesNotTouchAnything 钉住那条只读查询在真存储上也不写东西。
//
// 三件事都不该发生：给没被打过的 key 造出一个键、把一个已存在的键的过期
// 时间续上去、让计数变一变。前两件在替身上看不出来（替身里没有键空间也没
// 有过期时间）。
func TestRedisUsageDoesNotTouchAnything(t *testing.T) {
	f := newRedisFixture(t)
	keyID := freshKeyID()
	c := &clock{at: midWindow()}
	l, _ := newTestLimiter(c, WithStore(f.store))

	// 一、从没被打过的 key：查询之后存储里仍然没有这个键。
	u := l.Usage(context.Background(), keyID)
	if u.Count != 0 || !u.Exact {
		t.Errorf("没被打过的 key 读到 %+v，期望 count=0、exact=true", u)
	}
	if n, err := f.client.Exists(context.Background(), windowKey(keyID, c.now().Truncate(windowLen))).Result(); err != nil {
		t.Fatalf("EXISTS 失败: %v", err)
	} else if n != 0 {
		t.Error("一次只读查询把键造出来了")
	}

	// 二、已存在的键：计数不变，过期时间不被续上去。
	l.Allow(keyID, rpm(10))
	key := windowKey(keyID, c.now().Truncate(windowLen))
	before, err := f.client.PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("读 PTTL 失败: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if got := l.Usage(context.Background(), keyID); got.Count != 1 {
			t.Fatalf("第 %d 次查询读到 %d，期望 1", i+1, got.Count)
		}
	}

	after, err := f.client.PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("读 PTTL 失败: %v", err)
	}
	if after > before {
		t.Errorf("查询之后 TTL 从 %v 涨到了 %v：只读路径把键的寿命续上去了", before, after)
	}
	if n, err := f.client.Get(context.Background(), key).Int64(); err != nil || n != 1 {
		t.Errorf("查询之后计数 = %d（err=%v），期望 1", n, err)
	}
}

// TestRedisUnreachableDegradesWithoutFailing 是判据里那条「降级可用」。
//
// 这里用的是一个**真的连不上**的地址（127.0.0.1:1 不监听任何东西）：走的是
// 真实的客户端、真实的连接失败，而不是替身里那个开关。请求不能失败，信号
// 要变成近似。
func TestRedisUnreachableDegradesWithoutFailing(t *testing.T) {
	// 这一条不需要 LARKAX_REDIS_DSN：它要的恰恰是连不上。
	store, err := OpenRedis("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatalf("构造共享计数失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	c := &clock{at: midWindow()}
	l, reg := newTestLimiter(c, WithStore(store))

	allowed, _ := l.Allow(1, rpm(10))
	if !allowed {
		t.Error("连不上 Redis 时请求被拒了：故障不该让用户吃 4xx/5xx")
	}
	if v := reg.RateLimitExact.Value(); v != 0 {
		t.Errorf("信号 = %d，期望 0（近似）", v)
	}
	if got := reg.RateLimitStoreErrors.Value(); got != 1 {
		t.Errorf("存储失败计数 = %d，期望 1", got)
	}
	if u := l.Usage(context.Background(), 1); u.Exact {
		t.Error("连不上时查询不该说自己是精确的")
	}
}
