package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeCmds 是 redisCmds 的替身：每条命令返回一个预先摆好的结果，并记下
// 被调用过什么。
//
// 它只有一条能执行计数的命令——Eval。加一与续期现在是同一次 EVAL 里的两步，
// 替身没必要、也没法分别模拟：这个形状本身就是断言的一部分（热路径每次判定
// 只发一条命令，见 TestRedisStoreIncrIsOneRoundTrip）。
//
// 之所以还要一个替身：真实 Redis 上没法稳定地造出「存储命令失败」这条路径，
// 而它恰恰是这里最需要被钉住的失败。
type fakeCmds struct {
	evalN   int64
	evalErr error
	getVal  int64
	getErr  error
	pingErr error

	// calls 是**所有**命令被调用的次数。它量的是热路径的往返数：一次判定
	// 只能让它加一（或者失败时也不加第二条）。
	calls int

	// evalKeys / evalArgs 记下 EVAL 每次收到的键与参数，用来钉键名与过期秒数。
	evalKeys []string
	evalArgs []string
}

func (f *fakeCmds) Eval(_ context.Context, _ string, keys []string, args ...any) *redis.Cmd {
	f.calls++
	f.evalKeys = append(f.evalKeys, keys...)
	if len(args) > 0 {
		f.evalArgs = append(f.evalArgs, fmt.Sprint(args[0]))
	}
	return redis.NewCmdResult(f.evalN, f.evalErr)
}

func (f *fakeCmds) Get(_ context.Context, _ string) *redis.StringCmd {
	f.calls++
	if f.getErr != nil {
		return redis.NewStringResult("", f.getErr)
	}
	return redis.NewStringResult(strconv.FormatInt(f.getVal, 10), nil)
}

func (f *fakeCmds) Ping(context.Context) *redis.StatusCmd {
	f.calls++
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

// TestRedisStoreIncrIsOneRoundTrip 钉住热路径的往返数：一次判定一条命令。
//
// 改之前是两条（INCR 与 EXPIRE），它们之间那两条缝（进程死在中间、第二条失败
// 留下没有过期时间的键）由合并成一次 EVAL 堵上。这条用例是原先
// TestRedisStoreDeletesKeyWhenExpireFails 的等价替换：那条用例保护的是「INCR
// 成功而 EXPIRE 失败」这个中间态，而它现在**不可能出现**了——不再是两条命令。
// 于是判据换成「就是一次原子命令」：正常一次，失败一次，两次都只发一条。
//
// 一次判定只发一条命令，也是 #191 给这条热路径定的预算（1–2 个往返）里更省
// 的那一侧。
func TestRedisStoreIncrIsOneRoundTrip(t *testing.T) {
	cmds := &fakeCmds{evalN: 1}
	store := &RedisStore{cmds: cmds}
	key := "rl:7:1792238400"

	if _, err := store.Incr(context.Background(), key); err != nil {
		t.Fatalf("Incr 失败: %v", err)
	}
	if cmds.calls != 1 {
		t.Errorf("一次判定发了 %d 条命令，期望 1", cmds.calls)
	}
	if len(cmds.evalKeys) != 1 || cmds.evalKeys[0] != key {
		t.Errorf("EVAL 的键 = %v，期望恰好 %q", cmds.evalKeys, key)
	}

	// 失败也只有一个往返：EVAL 报错时不再有第二条命令，更没有补偿删除——
	// 原子性保证失败时计数没有落库，存储里没有任何要清理的残留。
	cmds.evalErr = errors.New("存储不可用")
	if _, err := store.Incr(context.Background(), key); err == nil {
		t.Fatal("EVAL 失败时应当返回错误")
	}
	if cmds.calls != 2 {
		t.Errorf("第二次判定累计发了 %d 条命令，期望 2（失败也不加发第二条）", cmds.calls)
	}
}

// TestRedisStoreIncrRenewsExpiryEveryTime 钉住「每次调用都续过期」。
//
// 续的是两倍窗口（见 keyTTL）。只在第一次写的时候设一次的话，一个持续被用的
// 键会在第一次写入之后的两倍窗口处准时消失——键的寿命就与第一次写入挂钩，
// 而不是与最后一次使用挂钩。
//
// 续期现在是 EVAL 脚本里的一步，替身看不到单独的 EXPIRE——所以这条用例钉两件
// 事：每次调用都把「键」与「两倍窗口的秒数」交给那一次 EVAL；以及脚本里确实
// 有无条件的 EXPIRE。秒数用**字面量** 120（别拿常量测常量：改成 keyTTL 就会红）。
func TestRedisStoreIncrRenewsExpiryEveryTime(t *testing.T) {
	cmds := &fakeCmds{evalN: 1}
	store := &RedisStore{cmds: cmds}
	key := "rl:7:1792238400"

	for i := int64(1); i <= 3; i++ {
		cmds.evalN = i
		n, err := store.Incr(context.Background(), key)
		if err != nil {
			t.Fatalf("第 %d 次 Incr 失败: %v", i, err)
		}
		if n != i {
			t.Errorf("第 %d 次 Incr 返回 %d", i, n)
		}
	}

	if len(cmds.evalKeys) != 3 {
		t.Fatalf("EVAL 发了 %d 次，期望 3（每次调用都续）", len(cmds.evalKeys))
	}
	for i, k := range cmds.evalKeys {
		if k != key {
			t.Errorf("第 %d 次 EVAL 打在键 %q 上，期望 %q", i+1, k, key)
		}
	}
	for i, arg := range cmds.evalArgs {
		if arg != "120" {
			t.Errorf("第 %d 次 EVAL 的过期参数 = %q，期望 %q（两倍窗口 120 秒）", i+1, arg, "120")
		}
	}
	// 脚本里必须真的带了 EXPIRE，否则「续期」只发生在注释里。
	if !strings.Contains(incrScript, "EXPIRE") {
		t.Errorf("EVAL 脚本里没有 EXPIRE：续期不会发生\n%s", incrScript)
	}
}

// TestRedisStoreIncrFailureLeavesNothingToClean 钉住 INCR 自己失败时的处置。
//
// EVAL 失败时计数没有落库（原子性：两条命令一条都没生效），所以既不该续
// 也不该补偿删除——多删一次等于把一个可能存在的、别人正在用的键抹掉。
// 判据是「一共只发了一条命令」。
func TestRedisStoreIncrFailureLeavesNothingToClean(t *testing.T) {
	cmds := &fakeCmds{evalErr: errors.New("连不上")}
	store := &RedisStore{cmds: cmds}

	if _, err := store.Incr(context.Background(), "rl:1:1"); err == nil {
		t.Fatal("EVAL 失败时应当返回错误")
	}
	if cmds.calls != 1 {
		t.Errorf("失败时发了 %d 条命令，期望 1（原子计数失败后没有第二条命令）", cmds.calls)
	}
}

// TestLimiterDegradesWhenStoreFails 把「存储命令失败」接到判定那一层。
//
// 单看存储那一条还不够：这里要证明的是，那次失败**不会**变成一次基于可疑
// 计数的拒绝。EVAL 是原子的，所以失败时计数根本没落库，也就没有那个「INCR
// 成功而 EXPIRE 失败的中间态」需要清理（原先的用例名是 …WhenExpireFails，
// 现在失败点收拢到一次 EVAL 上，改名叫 …WhenStoreFails）。
func TestLimiterDegradesWhenStoreFails(t *testing.T) {
	cmds := &fakeCmds{evalN: 99, evalErr: errors.New("存储不可用")}
	c := &clock{at: base}
	l, reg := newTestLimiter(c, WithStore(&RedisStore{cmds: cmds}))

	// 存储本会报 99（远超额度 1），但那一次 EVAL 没成功——这个数不可信，
	// 所以这一次判定不能拿它去拒人。
	allowed, _ := l.Allow(7, rpm(1))
	if !allowed {
		t.Error("存储失败后按那个可疑的计数拒了请求：这时应当降级，而不是拒绝")
	}
	if v := reg.RateLimitExact.Value(); v != 0 {
		t.Errorf("信号 = %d，期望 0（近似）", v)
	}

	// 存储恢复正常：下一次判定又是精确的，而这一次用的是存储里那个数。
	cmds.evalErr = nil
	cmds.evalN = 100
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

// TestOpenRedisFromManyGoroutines 钉住「库的 Logger 只被接一次」。
//
// go-redis 的 Logger 是**包级**变量，而读它的是库自己的 goroutine（拨号失败后的
// 诊断路径）。原先是每调一次 OpenRedis 就写一次，那是一次数据竞争——而它**在门禁
// 的口径下照不到**（`-count=1`，一个进程里通常只构造一次）。
//
// 这条例用**并发写**把它变成必然的失败：没有同步的话，几个 goroutine 同时写同一个
// 包级变量，`-race` 一定报出来。跑它必须带 `-race`（`go test -race ./internal/ratelimit/`）。
//
// DSN 指向一个不通的地址是有意的：只解析、只造客户端，不发命令——库的连接是懒的，
// 所以这条例不需要一个真的 Redis。
func TestOpenRedisFromManyGoroutines(t *testing.T) {
	const n = 8
	stores := make([]*RedisStore, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = OpenRedis("redis://127.0.0.1:1/0")
		}()
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("第 %d 次 OpenRedis 失败: %v", i, errs[i])
		}
		_ = stores[i].Close()
	}
}

// TestOpenRedisDisablesRetries 断言 OpenRedis 关掉了客户端重试。
//
// 一次判定只发一条命令，重试只会把时间花在等待上，而这段时间是用户请求在等；
// 抖一下的正确处置是降级（见 Allow 的「降级」一段）。
//
// 断言的是**客户端上生效的值 0**，不是源码里写的 `-1`。这个值经过了一次
// 归一化：`Options.init` 把 `-1` 归成 0，而 0 会归成库默认的 3 次，
// `Options()` 给的正是归一化之后那一份。所以这条用例直接看结果：源码把
// `-1` 改成 `1`（重试 1 次）时，这里读到的是 1，用例失败。
//
// 只解析 DSN 与构造客户端，不建立连接（库的连接是懒的），所以占位 DSN 就够了。
func TestOpenRedisDisablesRetries(t *testing.T) {
	store, err := OpenRedis("redis://user@localhost:6379/0")
	if err != nil {
		t.Fatalf("OpenRedis 失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	client, ok := store.cmds.(*redis.Client)
	if !ok {
		t.Fatalf("OpenRedis 造出来的不是 *redis.Client，而是 %T", store.cmds)
	}
	if got := client.Options().MaxRetries; got != 0 {
		t.Errorf("MaxRetries = %d，期望 0（一次都不重试；1 表示重试 1 次，3 是库默认值）", got)
	}
}

// 编译期断言：替身与真客户端一样满足 RedisStore 需要的那几条命令。
//
// 少了这一条，替身与真实现分岔（比如 Eval 的签名变了）时用例照样通过，
// 而「测过了」说的就只是那份替身。
var (
	_ redisCmds = (*fakeCmds)(nil)
	_ redisCmds = (*redis.Client)(nil)
)
