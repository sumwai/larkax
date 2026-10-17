package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeCmds 是 redisCmds 的替身：每条命令返回一个预先摆好的结果，并记下被
// 调用过什么。
//
// 为什么需要它：真实 Redis 上没法只让 INCR 成功、EXPIRE 失败，而那条路径
// 恰恰是这一层最要命的失败（INCR 成功而 EXPIRE 没成功会留下一个没有过期
// 时间的键）。把命令集抽成一个窄接口就是为了这个。
type fakeCmds struct {
	incrN     int64
	incrErr   error
	expireErr error
	getVal    int64
	getErr    error
	pingErr   error
	delErr    error

	incrKeys   []string
	expireKeys []string
	expires    []time.Duration
	dels       []string
	// delCtxErr 是 Del 被调用时它那个 context 的状态——补偿必须用一个新
	// context，因为 EXPIRE 失败最常见的原因正是原来那个已经到点了。
	delCtxErr error
}

func (f *fakeCmds) Incr(_ context.Context, key string) *redis.IntCmd {
	f.incrKeys = append(f.incrKeys, key)
	return redis.NewIntResult(f.incrN, f.incrErr)
}

func (f *fakeCmds) Expire(_ context.Context, key string, d time.Duration) *redis.BoolCmd {
	f.expireKeys = append(f.expireKeys, key)
	f.expires = append(f.expires, d)
	return redis.NewBoolResult(f.expireErr == nil, f.expireErr)
}

func (f *fakeCmds) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	f.delCtxErr = ctx.Err()
	f.dels = append(f.dels, keys...)
	return redis.NewIntResult(int64(len(keys)), f.delErr)
}

func (f *fakeCmds) Get(_ context.Context, key string) *redis.StringCmd {
	if f.getErr != nil {
		return redis.NewStringResult("", f.getErr)
	}
	return redis.NewStringResult(strconv.FormatInt(f.getVal, 10), nil)
}

func (f *fakeCmds) Ping(context.Context) *redis.StatusCmd {
	if f.pingErr != nil {
		return redis.NewStatusResult("", f.pingErr)
	}
	return redis.NewStatusResult("PONG", nil)
}

// TestWindowKeyFormat 钉住共享存储里的键名。
//
// 键名是跨实例、跨进程的约定：两个实例要能在同一个键上计数，靠的就是
// 「同一个窗口里的同一个 key 算出同一个字符串」。时间用**字面量**：
// 2026-10-17T12:00:00Z 是 1792238400，写死在这里，改了窗口切法或键名格式
// 这条就会红。
func TestWindowKeyFormat(t *testing.T) {
	start := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	if got, want := windowKey(12, start), "rl:12:1792238400"; got != want {
		t.Errorf("键名 = %q，期望 %q", got, want)
	}
	// 同一个窗口里的任意时刻，**截断之后**算出的键相同。截断由调用方做
	//（见 windowKey 的注释）：传一个没截断的时刻进来会得到另一个键，而
	// 那种错在用例里看起来会像「窗口没对齐」。
	if got := windowKey(12, start.Add(59*time.Second).Truncate(windowLen)); got != "rl:12:1792238400" {
		t.Errorf("窗口内 59 秒处的键名 = %q，期望与窗口开头相同", got)
	}
	// 下一个窗口是另一个键。
	if got := windowKey(12, start.Add(time.Minute)); got != "rl:12:1792238460" {
		t.Errorf("下一个窗口的键名 = %q，期望 rl:12:1792238460", got)
	}
	// 不同的 key 是不同的键。
	if got := windowKey(13, start); got != "rl:13:1792238400" {
		t.Errorf("另一把 key 的键名 = %q", got)
	}
}

// TestRedisStoreIncrRenewsExpiryEveryTime 钉住「每次调用都续 EXPIRE」。
//
// 续的是两倍窗口（见 keyTTL）。只在第一次写的时候设一次的话，一个在
// 第一次调用上把过期时间设丢了的键会活到永远——而这正是下一条用例的事。
func TestRedisStoreIncrRenewsExpiryEveryTime(t *testing.T) {
	cmds := &fakeCmds{incrN: 1}
	store := &RedisStore{cmds: cmds}
	key := "rl:7:1792238400"

	for i := int64(1); i <= 3; i++ {
		cmds.incrN = i
		n, err := store.Incr(context.Background(), key)
		if err != nil {
			t.Fatalf("第 %d 次 Incr 失败: %v", i, err)
		}
		if n != i {
			t.Errorf("第 %d 次 Incr 返回 %d", i, n)
		}
	}

	if len(cmds.expires) != 3 {
		t.Fatalf("EXPIRE 发了 %d 次，期望 3（每次调用都续）", len(cmds.expires))
	}
	for i, d := range cmds.expires {
		if d != keyTTL {
			t.Errorf("第 %d 次 EXPIRE 的时长 = %v，期望 %v（两倍窗口）", i+1, d, keyTTL)
		}
	}
	for _, k := range cmds.expireKeys {
		if k != key {
			t.Errorf("EXPIRE 打在键 %q 上，期望 %q", k, key)
		}
	}
	if len(cmds.dels) != 0 {
		t.Errorf("一切正常时不该删键，实际删了 %v", cmds.dels)
	}
}

// TestRedisStoreIncrFailureLeavesNothingToClean 钉住 INCR 自己失败时的处置。
//
// 这时计数没有加上去（命令整个失败），所以既不该续 EXPIRE 也不该补偿删除：
// 多删一次等于把一个可能存在的、别人正在用的键抹掉。
func TestRedisStoreIncrFailureLeavesNothingToClean(t *testing.T) {
	cmds := &fakeCmds{incrErr: errors.New("连不上")}
	store := &RedisStore{cmds: cmds}

	if _, err := store.Incr(context.Background(), "rl:1:1"); err == nil {
		t.Fatal("INCR 失败时应当返回错误")
	}
	if len(cmds.expires) != 0 || len(cmds.dels) != 0 {
		t.Errorf("INCR 失败后不该再有别的命令：EXPIRE %d 次、DEL %v", len(cmds.expires), cmds.dels)
	}
}

// TestRedisStoreDeletesKeyWhenExpireFails 钉住这一层最要命的那条失败。
//
// INCR 成功、EXPIRE 失败 → 留下一个**没有过期时间**的键。用例钉三件事：
//
//   - 错误要报上去（调用方据此降级，而不是拿一个可疑的数去拒人）；
//   - 那个键要被**删掉**（不是留着等它自己消失——它不会）；
//   - 删除用的必须是一个新的 context（EXPIRE 失败最常见的原因是原来那个
//     已经到点了，复用它等于让这次补偿也发不出去）。
func TestRedisStoreDeletesKeyWhenExpireFails(t *testing.T) {
	cmds := &fakeCmds{incrN: 1, expireErr: errors.New("EXPIRE 失败")}
	store := &RedisStore{cmds: cmds}
	key := "rl:7:1792238400"

	if _, err := store.Incr(context.Background(), key); err == nil {
		t.Fatal("EXPIRE 失败时 Incr 应当返回错误：调用方要据此降级")
	}
	if len(cmds.dels) != 1 || cmds.dels[0] != key {
		t.Fatalf("补偿删除的是 %v，期望恰好删掉 %q", cmds.dels, key)
	}
	if cmds.delCtxErr != nil {
		t.Errorf("补偿用的 context 已经不可用（%v）：它必须新起一个，否则这条补救根本发不出去", cmds.delCtxErr)
	}

	// 超时（context 已到点）之后再走一遍：删除仍然要发生。这一条与上面那条
	// 分开，是因为「EXPIRE 失败」与「context 超时」在真实场景里常常同时出现，
	// 而错就错在复用同一个 context 上。
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	cmds2 := &fakeCmds{incrN: 1, expireErr: context.DeadlineExceeded}
	store2 := &RedisStore{cmds: cmds2}
	if _, err := store2.Incr(expired, key); err == nil {
		t.Fatal("EXPIRE 失败时 Incr 应当返回错误")
	}
	if len(cmds2.dels) != 1 {
		t.Fatalf("context 已超时时补偿删除发生了 %d 次，期望 1", len(cmds2.dels))
	}
	if cmds2.delCtxErr != nil {
		t.Errorf("补偿用的 context 不可用（%v）", cmds2.delCtxErr)
	}
}

// TestLimiterDegradesWhenExpireFails 把「EXPIRE 失败」接到判定那一层。
//
// 单看存储那一条还不够：这里要证明的是，那次失败**不会**变成一次基于可疑
// 计数的拒绝，而那个键也不会被留在存储里没有过期时间。
func TestLimiterDegradesWhenExpireFails(t *testing.T) {
	cmds := &fakeCmds{incrN: 99, expireErr: errors.New("EXPIRE 失败")}
	c := &clock{at: base}
	l, reg := newTestLimiter(c, WithStore(&RedisStore{cmds: cmds}))

	// 共享存储里的计数是 99，远超额度 1——但那个数是不可信的（它可能不是
	// 一个完整的计数），所以这一次判定不能拿它去拒人。
	allowed, _ := l.Allow(7, rpm(1))
	if !allowed {
		t.Error("EXPIRE 失败后按那个可疑的计数拒了请求：这时应当降级，而不是拒绝")
	}
	if v := reg.RateLimitExact.Value(); v != 0 {
		t.Errorf("信号 = %d，期望 0（近似）", v)
	}
	if len(cmds.dels) != 1 {
		t.Errorf("补偿删除发生了 %d 次，期望 1：那个没有过期时间的键不能被留下", len(cmds.dels))
	}

	// 存储恢复正常：下一次判定又是精确的，而这一次用的是存储里那个数。
	cmds.expireErr = nil
	cmds.incrN = 100
	if allowed, _ := l.Allow(7, rpm(200)); !allowed {
		t.Error("存储恢复后按 100 次、额度 200 应当放行")
	}
	if v := reg.RateLimitExact.Value(); v != 1 {
		t.Errorf("存储恢复后信号 = %d，期望 1", v)
	}
}

// TestRedisStoreGetMissIsNotAnError 钉住「键不存在」不是错误。
//
// 会把它当成错误的话，一个这一分钟还没被打过的 key 查询起来就是 500，
// 而那是这条只读路径最常见的输入。
func TestRedisStoreGetMissIsNotAnError(t *testing.T) {
	store := &RedisStore{cmds: &fakeCmds{getErr: redis.Nil}}
	n, ok, err := store.Get(context.Background(), "rl:1:1")
	if err != nil {
		t.Fatalf("键不存在不该是错误: %v", err)
	}
	if ok || n != 0 {
		t.Errorf("键不存在时返回 (%d, %v)，期望 (0, false)", n, ok)
	}
}

// TestRedisStoreGetPassesThroughRealErrors 与上一条相对：真错误要报上去，
// 否则「读不出来」会与「是 0」在调用方那边长得一样。
func TestRedisStoreGetPassesThroughRealErrors(t *testing.T) {
	store := &RedisStore{cmds: &fakeCmds{getErr: errors.New("连不上")}}
	if _, _, err := store.Get(context.Background(), "rl:1:1"); err == nil {
		t.Fatal("存储出错时 Get 应当返回错误")
	}
}

// 编译期断言：替身与真客户端一样满足 RedisStore 需要的那几条命令。
//
// 少了这一条，替身与真实现分岔（比如 Del 的签名变了）时用例照样通过，
// 而「测过了」说的就只是那份替身。
var (
	_ redisCmds = (*fakeCmds)(nil)
	_ redisCmds = (*redis.Client)(nil)
)
