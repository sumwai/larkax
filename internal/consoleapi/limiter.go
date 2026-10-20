package consoleapi

import (
	"net"
	"sync"
	"time"
)

// 本文件是登录的**按 IP** 节流：一个进程内的令牌桶。
//
// # 它是辅助，不是主防线
//
// 主防线是按账号的锁定（见 auth.RecordLoginFailure）：它落在库里，跨进程、
// 重启不丢。这一层挡的是「同一个来源换着用户名猜」——那种攻击不会让任何
// 单个账号的计数涨起来，于是账号侧的锁完全看不见它。
//
// # 没有受信代理时不生效
//
// X-Forwarded-For 只有具备受信代理时才可读（见 API.remoteIP）。直接暴露在
// 公网时，所有连接看起来都来自反代地址，这一层会把所有人算成一个人——
// 那时它要么处处误伤、要么处处放行，两种都比不开更糟。
//
// 这也解释了为什么它只存在进程内存里：它本来就是近似的，而一份「近似的、
// 跨进程共享的」计数要在加锁与网络往返之间二选一，两个都不值得为一个辅助
// 手段付。

// 令牌桶的参数。
const (
	// loginBurst 是一次可以连续试几次。给 10 是为了让一个刚想起来口令的人
	// 连着改几遍不被拦——那几步不值得等。
	loginBurst = 10
	// loginRefill 是每补一个令牌的间隔：每分钟 6 次。
	loginRefill = 10 * time.Second
	// limiterSweepEvery 是清理空闲桶的间隔。
	limiterSweepEvery = 10 * time.Minute
	// limiterSweepAbove 是触发清理的桶数下限。低于它时清理只是白扫一遍。
	limiterSweepAbove = 1024
)

// tokenBucket 是一个来源的剩余令牌。
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// ipLimiter 是按来源地址的令牌桶集合。
//
// 键是 IP 的字符串形式（`net.IP.String()`），于是 IPv4 与 IPv4-mapped IPv6
// 折成同一个键——不折的话，::ffff:1.2.3.4 与 1.2.3.4 各有一份额度，
// 换个写法就能绕开。
type ipLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	lastSweep time.Time
}

// newIPLimiter 建一个空的限流器。
func newIPLimiter() *ipLimiter {
	return &ipLimiter{
		buckets:   make(map[string]*tokenBucket),
		lastSweep: time.Now(),
	}
}

// allow 判断这次尝试放不放行；不放行时给出建议的重试间隔。
//
// ip 为 nil（没解析出来）时放行：无法归属来源的请求，按来源节流没有意义，
// 而拒掉它等于让所有解析失败的请求都撞在同一堵墙上。
func (l *ipLimiter) allow(ip net.IP, now time.Time) (time.Duration, bool) {
	if ip == nil {
		return 0, true
	}
	key := ip.String()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	bucket, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &tokenBucket{tokens: loginBurst - 1, last: now}
		return 0, true
	}

	elapsed := now.Sub(bucket.last)
	if elapsed > 0 {
		bucket.tokens += elapsed.Seconds() / loginRefill.Seconds()
		if bucket.tokens > loginBurst {
			bucket.tokens = loginBurst
		}
		bucket.last = now
	}
	if bucket.tokens >= 1 {
		bucket.tokens--
		return 0, true
	}
	// 还差多少才能攒出下一个令牌。
	wait := time.Duration((1 - bucket.tokens) * float64(loginRefill))
	return wait.Round(time.Second), false
}

// reset 在登录成功后清空这个来源的桶。
//
// 不清的话，一个刚成功登录的人还背着之前几次手误的消耗；而那条路径上
// 他接下来要做的事（改口令）与「刚证明过自己是本人」是同一档信誉。
func (l *ipLimiter) reset(ip net.IP) {
	if ip == nil {
		return
	}
	l.mu.Lock()
	delete(l.buckets, ip.String())
	l.mu.Unlock()
}

// sweepLocked 清掉已经补满的桶。
//
// 补满的桶与「从没出现过」在行为上完全一样，所以删掉它不改变判据，
// 只把内存从「见过多少个 IP」收回到「最近在试的有多少个」。
// 调用方已持有锁。
func (l *ipLimiter) sweepLocked(now time.Time) {
	if len(l.buckets) < limiterSweepAbove || now.Sub(l.lastSweep) < limiterSweepEvery {
		return
	}
	refillAll := time.Duration(float64(loginBurst) * float64(loginRefill))
	for key, bucket := range l.buckets {
		if now.Sub(bucket.last) >= refillAll {
			delete(l.buckets, key)
		}
	}
	l.lastSweep = now
}
