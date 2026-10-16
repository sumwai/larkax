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
// 进程内。**多实例部署时每个实例各计一份，额度不共享**——这个限制没有被
// 解决，只是被写下来了：共享计数要有一个所有实例都连得上的存储（Redis），
// 而目前没有任何 Redis 客户端，引入一个不在本次改动范围内。
// 启动时有一条 warn 把这件事说出来（见 internal/assembly）。
//
// 因此这一层只保证「单个实例内的额度是准的」。
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
	"sync"
	"time"
)

// windowLen 是窗口长度。
//
// 一分钟来自这一列的语义（RPM），所以它是常量而不是配置：改它等于改
// 这一列的含义，那不该由一个运行期参数悄悄做掉。
const windowLen = time.Minute

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

// Limiter 是按 key 的固定窗口计数器。
//
// 它没有外部依赖，也不需要清理协程：每个 key 在 map 里只留一条「当前窗口」
// 的记录，上一个窗口的残留在下一次窗口翻转时被顺手扫掉（见 sweep）。
//
// 并发安全由一把互斥锁保证。锁里做的只有几次 map 操作与整数比较，
// 而这个方法在每个请求上只调用一次，不在锁里做任何 I/O。
type Limiter struct {
	// now 可注入，见 New。
	now func() time.Time

	mu      sync.Mutex
	windows map[int64]window
	// lastSweep 是上一次清扫时的窗口起点，用来把清扫压到每窗口最多一次。
	lastSweep time.Time
}

// New 构造一个计数器。
//
// now 可注入：窗口边界是这套逻辑里唯一与时间有关的部分，测试要能把表
// 直接拨到边界两侧，而不是 sleep 到那一刻。为 nil 时用 time.Now。
func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, windows: make(map[int64]window)}
}

// Allow 记一次请求，并判断它是否放行。
//
// keyID 是 api_keys.id；rpm 是 api_keys.rate_limit_rpm 的**原样**值，
// nil 表示列值为 NULL。两者的含义见包注释。
//
// retryAfter 只在 allowed 为 false 时有意义：它是到当前窗口结束为止的时间，
// 向上取整到秒，至少 1 秒。写 0 会让客户端立刻重试，而它必然还在同一个
// 窗口里，白白多一次往返。
func (l *Limiter) Allow(keyID int64, rpm *int) (allowed bool, retryAfter time.Duration) {
	if rpm == nil {
		// 不限流的 key 不进 map：没有额度就没有要记的状态，热路径上也
		// 不该为它多一次加锁。
		return true, 0
	}
	limit := *rpm

	now := l.now()
	start := now.Truncate(windowLen)

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

	if limit > 0 && w.count <= limit {
		return true, 0
	}
	return false, retryAfterUntil(start, now)
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
