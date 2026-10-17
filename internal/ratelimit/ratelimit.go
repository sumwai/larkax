// Package ratelimit 按 API Key 限制每分钟的请求数。
//
// # 额度从哪来
//
// api_keys.rate_limit_rpm。这一列可空，且列上的注释只说了「NULL 表示继承
// 用户默认」——而 users 表上并没有这样一个默认值。所以它的完整语义在这里
// 定下，并且**只在这里解释一次**：
//
//   - NULL —— 没有设额度，不限流。
//     「继承用户默认」目前等价于「不限」，因为那个默认值不存在。将来真加
//     用户级默认值时，改的是 Allow 里 rpm 为 nil 的那一个分支。
//   - <= 0 —— 一次都不放行。
//     0 按字面读就是「零次每分钟」；负数是没意义的配置，两种都往严的那
//     一侧倒。**不把 0 读成「不限」**：一个设了却没生效的额度正是这个包
//     要修的那类问题，而「0 等于不限」会让它再出现一次。
//   - > 0 —— 每个窗口最多放行这么多次。
//
// # 计数放在哪
//
// **共享存储（Redis）**，配置项是 LARKAX_REDIS_DSN。所有实例读同一份计数，
// 于是多实例部署下放行总量等于配置值——这正是这个包原先做不到的那件事。
// 键名与过期见 RedisStore。
//
// 没有配 LARKAX_REDIS_DSN 时只有进程内计数这一条路径，也就是 Redis 出现
// 之前的样子：单实例正确，N 个实例下额度放大到 N 倍。启动时有一条 warn
// 把这一种说出来（见 internal/assembly）。
//
// **共享存储不可用时退回进程内计数**（见 Allow 的「降级」一段）。那时判定是
// **近似**的：各实例各算各的，放行量偏大，但请求不会失败。当前是精确还是近似
// 由指标 larkax_ratelimit_exact 说出来，启动日志里也有一行。
//
// # 窗口
//
// 固定窗口，按墙钟分钟对齐：一个窗口是 [00:00.000, 01:00.000)。
//
// 代价是**窗口边界处最多允许两倍突发**：一个 60 RPM 的 key 可以在 00:59
// 打满 60 次，在 01:00 再打满 60 次。滑动窗口没有这个问题，但它要为每个
// key 记住窗口内每一次请求的时间戳，内存与复杂度都高一个量级。额度本身
// 是分钟级的粗粒度限制，两倍突发在这个前提下是可接受的取舍。要收紧时改
// 这里，而不是去改调用方。
package ratelimit

import (
	"context"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/sumwai/larkax/internal/observe"
)

// windowLen 是窗口长度。
//
// 一分钟来自这一列的语义（RPM），所以它是常量而不是配置：改它等于改
// 这一列的含义，那不该由一个运行期参数悄悄做掉。
const windowLen = time.Minute

// keyTTL 是共享存储里一个窗口键的存活时间：两倍窗口。
//
// 为什么不是一倍：窗口按墙钟对齐，一个键在它的窗口过去之后就不该再被读到。
// 但过期是「活到那一刻」的语义，而写入它的那些请求可能落在窗口的任意位置；
// 取一倍窗口时，窗口末尾与下一个窗口开头挨得很近，任何一点时间上的偏差
// （时钟、命令排队）都会让一个还可能被读到的键提前消失——那等于把这个 key
// 在新窗口里的计数从 0 重新开始。多留一个窗口的余量，代价是键最多多留一分钟。
const keyTTL = 2 * windowLen

// storeTimeout 是一次判定允许花在共享存储上的时间。
//
// 它是**热路径预算**：一次判定做两条命令（INCR 与 EXPIRE），整个预算
// 一起算。几十毫秒的取值按最坏情况定——健康的 Redis 上这两条命令在
// 同一个机房里是亚毫秒级的，余量是留给「Redis 慢但没死」的：那种时候
// 也不能让用户请求跟着慢。超时按不可用处理（见 Allow 的降级一段）。
const storeTimeout = 50 * time.Millisecond

// probeTimeout 是启动期那一次探活的时间上限。
//
// 与 storeTimeout 分开：那一个是热路径预算，这一个只在启动时花一次，
// 取值按「运维能接受启动慢多久」定（见 internal/assembly 的探活）。
const probeTimeout = 2 * time.Second

// window 是一把 key 在当前窗口里的计数。
type window struct {
	// start 是这个窗口的起点，即 now 按 windowLen 向下取整的结果。
	start time.Time
	// count 是这个窗口内已经记下的请求次数，**包括被拒的那些**。
	//
	// 把被拒的也记上是有意的：计数器记的是「这个窗口里来了多少次」，
	// 而不是「放行了多少次」。两者的放行判定完全相同（超过额度之后
	// 继续加只会继续超），但前者在排查时能回答「被打得多狠」。
	count int
}

// Store 是限流计数的共享存储。
//
// 它只有这几条方法，是因为调用方只用得上这几条：加一次、读一次、探活。
// 键名由本包拼（见 windowKey），存储实现不需要知道窗口是怎么切的。
type Store interface {
	// Incr 把 key 的计数加一并返回加后的值。失败时调用方按存储不可用处理。
	Incr(ctx context.Context, key string) (int64, error)

	// Get 读 key 的当前值，键不存在时返回 (0, false, nil)。
	Get(ctx context.Context, key string) (int64, bool, error)

	// Ping 探活。
	Ping(ctx context.Context) error
}

// Limiter 是按 key 的固定窗口计数器。
//
// # 两种计数来源
//
// 计数在共享存储里（通常是 Redis），没有共享存储时在进程内的一个 map 里；
// 前者是精确的，后者只是本进程看到的那一份。两者的取舍见包的注释与 Allow。
//
// 进程内那份不需要清理协程：每个 key 在 map 里只留一条「当前窗口」的记录，
// 上一个窗口的残留在下一次窗口翻转时被顺手扫掉（见 sweep）。
//
// 并发安全由一把互斥锁保证。锁里做的只有几次 map 操作与整数比较；共享存储
// 那一次调用**不在锁里**（在锁里等 Redis 会让所有请求排在同一个 key 后面）。
type Limiter struct {
	// now 可注入，见 New。
	now func() time.Time

	mu      sync.Mutex
	windows map[int64]window
	// lastSweep 是上一次清扫时的窗口起点，用来把清扫压到每窗口最多一次。
	lastSweep time.Time

	// store 是共享存储。为 nil 时只用进程内计数（没配 LARKAX_REDIS_DSN，
	// 或者构造时没给 WithStore）。
	store Store

	// metrics 是「现在是精确还是近似」这个信号的落点。为 nil 时不报指标。
	metrics *observe.Registry
}

// Option 是 New 的可选参数。
type Option func(*Limiter)

// WithStore 让计数走共享存储。
//
// 不给时只有进程内计数——那对**单实例**是正确的，而多实例下每个实例各记
// 一份，额度放大到实例数倍。装配层因此会在这种配置下打一条 warn。
func WithStore(s Store) Option {
	return func(l *Limiter) { l.store = s }
}

// New 构造一个计数器。
//
// now 可注入：窗口边界是这套逻辑里唯一与时间有关的部分，测试要能把表
// 直接拨到边界两侧，而不是 sleep 到那一刻。为 nil 时用 time.Now。
func New(now func() time.Time, opts ...Option) *Limiter {
	if now == nil {
		now = time.Now
	}
	l := &Limiter{
		now:     now,
		windows: make(map[int64]window),
		// 默认落在包级注册表上：指标「忘了接」与「接了但一直是 0」在输出上
		// 长得一样，所以默认值不能是「不报」。要断言具体数值的测试自己
		// 传一个 observe.NewRegistry()。
		metrics: observe.Metrics,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Allow 记一次请求，并判断它是否放行。
//
// keyID 是 api_keys.id；rpm 是 api_keys.rate_limit_rpm 的**原样**值，
// nil 表示列值为 NULL。两者的含义见包注释。
//
// retryAfter 只在 allowed 为 false 时有意义：它是到当前窗口结束为止的时间，
// 向上取整到秒，至少 1 秒。写 0 会让客户端立刻重试，而它必然还在同一个
// 窗口里，白白多一次往返。
//
// # 降级
//
// 共享存储不可用（连不上、超时、命令报错）时，这一次判定退回进程内计数，
// 也就是变成**近似**的：各实例各算各的，放行量偏大。方向是有意选的——
// 限流的目的是防滥用与控成本，不是把用户挡在门外，所以存储出问题时宁可
// 少拦几个，也不让正常请求失败（与额度那一处的取舍相同，见 internal/api
// 的 QuotaChecker）。它**不等于关掉限流**：本进程仍然按自己的计数拦，
// 只是拦住的是它自己看到的那一份。
//
// 恢复不需要任何显式的切换：每一次判定都会重新试一次共享存储，成功的那
// 一次起就又是精确的。
func (l *Limiter) Allow(keyID int64, rpm *int) (allowed bool, retryAfter time.Duration) {
	if rpm == nil {
		// 不限流的 key 不进计数：没有额度就没有要记的状态，热路径上也不该
		// 为它多一次加锁，更不该为它多一次网络往返。
		return true, 0
	}
	limit := *rpm

	now := l.now()
	start := now.Truncate(windowLen)

	count := l.count(keyID, start)

	if limit > 0 && count <= int64(limit) {
		return true, 0
	}
	return false, retryAfterUntil(start, now)
}

// count 返回这个 key 在当前窗口里已经记下的请求次数，**含这一次**。
//
// 它顺带把「这一次判定是精确还是近似」写进指标（见 mark）。
func (l *Limiter) count(keyID int64, start time.Time) int64 {
	if l.store == nil {
		// 没有共享存储：这个部署本来就只能按进程内计数，也就是近似。
		l.mark(false)
		return l.localCount(keyID, start)
	}

	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()

	n, err := l.store.Incr(ctx, windowKey(keyID, start))
	if err != nil {
		// 存储不可用。**不**在这个错误上拒人：连不上 Redis 是平台的问题，
		// 不该变成用户的请求失败（见 Allow 的降级一段）。
		if l.metrics != nil {
			l.metrics.RateLimitStoreErrors.Inc()
		}
		l.mark(false)
		return l.localCount(keyID, start)
	}
	l.mark(true)
	return n
}

// localCount 在进程内那份计数上加一次并返回加后的值。
func (l *Limiter) localCount(keyID int64, start time.Time) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(start)

	w := l.windows[keyID]
	if !w.start.Equal(start) {
		// 上一个窗口的记录到这里作废。判断用 Equal 而不是 Before/After：
		// 时钟被往回拨时（测试里会这么做）「不等于」仍然是对的判据。
		w = window{start: start}
	}
	w.count++
	l.windows[keyID] = w

	return int64(w.count)
}

// mark 把「这一次判定取自哪里」写进指标。
//
// 每次判定都写，不只在翻转时写：仪表是一次原子写，省下它没有意义，而
// 「只在变化时写」要在进程里多存一份状态、还要处理「第一次就是 0」这个
// 初值与本值分不开的情形。
func (l *Limiter) mark(exact bool) {
	if l.metrics == nil {
		return
	}
	if exact {
		l.metrics.RateLimitExact.Set(1)
		return
	}
	l.metrics.RateLimitExact.Set(0)
}

// Usage 是一个 key 在当前窗口里的用量读数。
//
// 它是运维读的那一份：答的是「这个 key 这一分钟里被打了多少次、这个数准不准」。
// 它答不了「这个 key 的额度是多少」——额度在 api_keys.rate_limit_rpm 里，
// 要查库（走既有的运维命令，见 docs/limits.md），两边的数由读的人拼起来。
type Usage struct {
	// KeyID 是 api_keys.id。
	KeyID int64

	// WindowStart 与 WindowEnd 是这次读到的窗口，左闭右开。
	WindowStart time.Time
	WindowEnd   time.Time

	// Count 是这个窗口里已经记下的请求次数，**含被拒的那些**（与判定用的
	// 是同一个数）。
	Count int64

	// Exact 说明这个数是从哪来的：true 表示取自共享存储（所有实例共用
	// 那一份），false 表示只是**本进程**记的那一份——共享存储不可用，
	// 或者这个部署根本没配。
	Exact bool
}

// Usage 读某个 key 在当前窗口的用量。
//
// # 它只读
//
// 不 INCR、不续过期时间、不改进程内那份计数，所以查一次不会让任何 key
// 更接近被限流，也不会让共享存储里的键活得更久。
//
// 它**不更新**「现在是精确还是近似」那个指标：那说的是请求路径当前走哪条路，
// 由 Allow 更新；而这里给的 Exact 是**这一次读数**的来源，两者可以不同
// （例如 Redis 刚刚出问题、还没来过一个请求）。把它写进指标会让一个只读的
// 查询改掉告警信号，那是这一层不该有的副作用。
func (l *Limiter) Usage(ctx context.Context, keyID int64) Usage {
	now := l.now()
	start := now.Truncate(windowLen)
	u := Usage{KeyID: keyID, WindowStart: start, WindowEnd: start.Add(windowLen)}

	if l.store != nil {
		// 从调用方的 context 派生，于是它有两条上限：这一条自己的预算，
		// 以及调用方的取消（HTTP 请求断开时不必再等这个数）。
		cctx, cancel := context.WithTimeout(ctx, storeTimeout)
		defer cancel()

		if n, ok, err := l.store.Get(cctx, windowKey(keyID, start)); err == nil {
			if ok {
				u.Count = n
			}
			u.Exact = true
			return u
		}
	}

	u.Count = l.localUsage(keyID, start)
	return u
}

// localUsage 读进程内那份计数。没有记录时是 0。
//
// 它不 sweep：清理是写操作（它会删记录），而这条路径是只读的。上一个窗口
// 的残留由下一次 Allow 顺手扫掉（见 sweep），所以这里不清也不会长起来。
func (l *Limiter) localUsage(keyID int64, start time.Time) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	w := l.windows[keyID]
	if !w.start.Equal(start) {
		return 0
	}
	return int64(w.count)
}

// Close 释放共享存储的连接池。没有共享存储、或那个实现没有连接池时是空操作。
//
// 单独一个断言而不是把 Close 放进 Store：Store 描述的是「判定需要什么」，
// 而释放连接池是生命周期的另一件事，两者的实现可以分开（测试里的替身就
// 没有连接池）。
func (l *Limiter) Close() error {
	c, ok := l.store.(io.Closer)
	if !ok {
		return nil
	}
	return c.Close()
}

// Probe 探一次共享存储，并按结果更新「现在是精确还是近似」那个信号。
//
// 启动期用它。不探的话，那个信号要等到第一个请求才出现，而「还没来过请求」
// 与「共享存储不该有这一行」在 /metrics 上没有任何分别。
//
// 探测失败**不**表示这个实例不能用（见 Allow 的降级），也不计入
// RateLimitStoreErrors——那个数说的是「有多少次判定退回了进程内计数」，
// 而这一次还没有请求。没有任何共享存储时它把信号写成近似并返回 nil：
// 那个部署本来就只有进程内计数。
func (l *Limiter) Probe(ctx context.Context) error {
	if l.store == nil {
		l.mark(false)
		return nil
	}

	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	err := l.store.Ping(cctx)
	l.mark(err == nil)
	return err
}

// windowKey 返回某个 key 在某个窗口里的共享存储键名：rl:<keyID>:<窗口起点>。
//
// start 必须是**已经取整过的窗口起点**（调用方按 windowLen 截断，见 Allow），
// 不是「窗口里的某个时刻」：不截断时同一个窗口里的两次调用会算出两个键，
// 而那种错在现场看起来像「额度忽然翻倍」，不在键名上。
//
// 窗口起点取 Unix 秒——窗口按分钟对齐，秒位总是 00，于是同一个窗口里的
// 所有请求算出同一个键。拼接而不是 Sprintf：这个函数在请求热路径上。
func windowKey(keyID int64, start time.Time) string {
	return "rl:" + strconv.FormatInt(keyID, 10) + ":" + strconv.FormatInt(start.Unix(), 10)
}

// sweep 清掉不属于当前窗口的记录。
//
// 不清理的话 map 会随「历史上用过的 key 数」一直长——被删掉的 key、
// 轮换掉的 key 都不会再回来，它们的记录却永远留着，而这是个会跑几个月的
// 进程。
//
// 触发条件是「跨过一个窗口」，于是一个窗口内最多扫一次，与请求数无关；
// 一次扫描是 O(当前活跃的 key 数)。两倍窗口之前的记录都不会再被读到，
// 所以清理不会改变任何判定结果。
func (l *Limiter) sweep(currentStart time.Time) {
	if !l.lastSweep.Before(currentStart) {
		return
	}
	l.lastSweep = currentStart
	for id, w := range l.windows {
		if w.start.Before(currentStart) {
			delete(l.windows, id)
		}
	}
}

// retryAfterUntil 返回从 now 到窗口结束的时间，向上取整到秒，至少 1 秒。
func retryAfterUntil(windowStart, now time.Time) time.Duration {
	remaining := windowStart.Add(windowLen).Sub(now)
	if remaining < time.Second {
		// 到不了这里（now 落在窗口内，剩余时间必然为正），但时钟被往回拨
		// 或调用方自己算了一个窗口起点时它会成立——那时回一个 0 会让
		// 客户端立刻重试并立刻再被拒一次。
		return time.Second
	}
	// 向上取整：向下取整会让客户端在窗口还没结束时就重试。
	return ((remaining + time.Second - 1) / time.Second) * time.Second
}
