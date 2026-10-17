package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/observe"
)

// memStore 是一个共享存储的替身：一个进程外的 map，语义与 Redis 那份一致
// （加一、读、探活）。
//
// 它存在的理由是「两个实例」要能被测到：两个限流器各有一份进程内的 map，
// 但指向**同一个** store，于是它们看到的就是同一份计数——这正是多实例
// 部署的样子，而真 Redis 只能证明「我们连得上 Redis」，证明不了这一层
// 有没有真的把计数交出去。真实实现另有一组用例（见 redis_integration_test.go）。
type memStore struct {
	mu     sync.Mutex
	counts map[string]int64

	// down 为真时所有命令都失败，用来测降级与恢复。
	down bool
}

func newMemStore() *memStore { return &memStore{counts: map[string]int64{}} }

func (s *memStore) Incr(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return 0, errors.New("存储不可用")
	}
	s.counts[key]++
	return s.counts[key], nil
}

func (s *memStore) Get(_ context.Context, key string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return 0, false, errors.New("存储不可用")
	}
	n, ok := s.counts[key]
	return n, ok, nil
}

func (s *memStore) Ping(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return errors.New("存储不可用")
	}
	return nil
}

// value 读某个键当前的值，size 数它有几个键。走锁：用例之间不并发，但
// -race 之下「读一个别的 goroutine 可能正在写的 map」本身就是数据竞争。
func (s *memStore) value(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[key]
}

func (s *memStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.counts)
}

// setDown 切换这个存储的可用性。
func (s *memStore) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// blockStore 是一个会阻塞的存储：Incr 一直等到 context 到点（或三秒）。
//
// 用它钉「超时按不可用处理」：真实 Redis 上没法稳定地造出「连得上但很慢」，
// 而这一条恰恰是热路径预算唯一能被验证的方式。
type blockStore struct{}

func (blockStore) Incr(ctx context.Context, _ string) (int64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(3 * time.Second):
		return 0, errors.New("替身没有收到 context 的取消")
	}
}

func (blockStore) Get(ctx context.Context, _ string) (int64, bool, error) {
	_, err := blockStore{}.Incr(ctx, "")
	return 0, false, err
}

func (blockStore) Ping(context.Context) error { return nil }

// newTestLimiter 造一个限流器，并把它接到一份**独立的**指标注册表上。
//
// 不用包级那份：往包级计数器上加数会让别的用例看到不该有的初值，而
// 「现在是精确还是近似」正是这组用例要断言的数（与 internal/observe 里
// 那条注释同一个理由）。
func newTestLimiter(c *clock, opts ...Option) (*Limiter, *observe.Registry) {
	l := New(c.now, opts...)
	reg := observe.NewRegistry()
	l.metrics = reg
	return l, reg
}

// exactSignal 读「现在是精确还是近似」这个信号。
//
// 第二个返回值是「那个仪表输出了没有」——没设置过时整族不输出，而
// 「没输出」与「值是 0」是两件事（见 observe.Gauge）。判据走渲染结果而不是
// 内部字段：字段在包里、测试够得着，但「输出了没有」才是这个信号对外的样子。
func exactSignal(t *testing.T, reg *observe.Registry) (int64, bool) {
	t.Helper()
	var buf bytes.Buffer
	if err := reg.Render(&buf); err != nil {
		t.Fatalf("渲染指标失败: %v", err)
	}
	if !strings.Contains(buf.String(), "larkax_ratelimit_exact ") {
		return 0, false
	}
	return reg.RateLimitExact.Value(), true
}

// TestSharedStoreStopsTwoInstancesAtTheLimit 是两个实例共用一份计数的核心断言。
//
// 「两个实例」= 两个彼此不共享任何东西、只共享同一个存储的限流器：进程内
// 那份 map 各有一份。额度 4 时两边加起来只能过 4 次。
func TestSharedStoreStopsTwoInstancesAtTheLimit(t *testing.T) {
	store := newMemStore()
	c := &clock{at: base}
	a, _ := newTestLimiter(c, WithStore(store))
	b, _ := newTestLimiter(c, WithStore(store))
	limit := rpm(4)

	allowed := 0
	// 交替着打，两个实例各打 4 次：进程内计数下这里会放行 8 次。
	for i := 0; i < 4; i++ {
		if ok, _ := a.Allow(11, limit); ok {
			allowed++
		}
		if ok, _ := b.Allow(11, limit); ok {
			allowed++
		}
	}
	if allowed != 4 {
		t.Errorf("两个实例合计放行了 %d 次，期望 4（额度按共享计数算，不是各算一份）", allowed)
	}

	// 对照组：同样的两个实例、同样的打法，**不给共享存储**。它们各过 4 次，
	// 一共 8 次——那就是这件事出现之前的行为，也正是上面那个 4 要防的东西。
	// 两个数放在同一条用例里，共享计数有没有真的生效就有了内生的对照，
	// 而不是靠一次手工的红证。
	lonely := 0
	c2 := &clock{at: base}
	p, _ := newTestLimiter(c2)
	q, _ := newTestLimiter(c2)
	for i := 0; i < 4; i++ {
		if ok, _ := p.Allow(11, limit); ok {
			lonely++
		}
		if ok, _ := q.Allow(11, limit); ok {
			lonely++
		}
	}
	if lonely != 8 {
		t.Errorf("没有共享存储时两个实例合计放行了 %d 次，期望 8（各算一份，这正是对照）", lonely)
	}
}

// TestSharedStoreKeepsRetryAfter 钉住「判定变了，Retry-After 的算法没变」。
//
// 额度用完时回给客户端的是到当前窗口结束的时间，与计数放在哪无关。
func TestSharedStoreKeepsRetryAfter(t *testing.T) {
	store := newMemStore()
	c := &clock{at: base} // 12:00:30，距窗口结束 30 秒
	l, _ := newTestLimiter(c, WithStore(store))
	limit := rpm(1)

	if ok, _ := l.Allow(1, limit); !ok {
		t.Fatal("第 1 次被拒")
	}
	ok, retryAfter := l.Allow(1, limit)
	if ok {
		t.Fatal("第 2 次通过了：额度是 1")
	}
	if retryAfter != 30*time.Second {
		t.Errorf("retryAfter = %v，期望 30s", retryAfter)
	}
}

// TestStoreFailureDegradesButStillLimits 是降级那一条的核心断言。
//
// 三件事必须同时成立：
//
//   - 请求**不失败**（存储挂了是平台的问题，不该让用户吃 5xx）；
//   - 「近似」这个信号为真——不然一次降级在指标上与正常完全一样；
//   - 它**不等于关掉限流**：本进程仍然按自己的计数拦。
func TestStoreFailureDegradesButStillLimits(t *testing.T) {
	store := newMemStore()
	c := &clock{at: base}
	l, reg := newTestLimiter(c, WithStore(store))
	limit := rpm(2)

	// 先正常地打满：两个实例共用一份计数，这里只有本进程。
	if ok, _ := l.Allow(1, limit); !ok {
		t.Fatal("存储可用时第 1 次被拒")
	}
	if v := reg.RateLimitExact.Value(); v != 1 {
		t.Errorf("存储可用时信号 = %d，期望 1（精确）", v)
	}

	store.setDown(true)

	// 存储挂了：这一次仍然放行，信号翻成近似。读到的是进程内那份，而它
	// **从这个时刻才开始记**——共享存储里那一次它是看不到的。这正是「近似」
	// 的含义：这一分钟里这个 key 实际被打了几次，只有把两个数加起来才知道。
	if ok, _ := l.Allow(1, limit); !ok {
		t.Fatal("存储不可用时被拒：降级的方向是宁可少拦，也不让请求失败")
	}
	if v := reg.RateLimitExact.Value(); v != 0 {
		t.Errorf("存储不可用时信号 = %d，期望 0（近似）", v)
	}
	if got := reg.RateLimitStoreErrors.Value(); got != 1 {
		t.Errorf("存储失败计数 = %d，期望 1", got)
	}

	// 继续在这个进程里打：本地的计数会重新数到额度上，然后拦住。
	// 降级**不是**关掉限流——少拦的是「别的实例那部分」，不是全部。
	if ok, _ := l.Allow(1, limit); !ok {
		t.Error("降级后本进程第 2 次被拒：进程内计数从头开始，第 2 次应当在额度之内")
	}
	if ok, _ := l.Allow(1, limit); ok {
		t.Error("降级之后本进程的额度失效了：2 次额度在本进程里被打到第 3 次仍然放行")
	}
	if got := reg.RateLimitStoreErrors.Value(); got != 3 {
		t.Errorf("存储失败计数 = %d，期望 3（每一次判定各记一次）", got)
	}
}

// TestRecoversToExactWhenStoreComesBack 钉住「恢复不需要任何显式切换」。
//
// 信号要随存储的健康状态**来回变化**，而不是一旦降级就停在近似上——停在
// 近似上的话，运维看到的那一行就永远说不清现在到底准不准。
func TestRecoversToExactWhenStoreComesBack(t *testing.T) {
	store := newMemStore()
	c := &clock{at: base}
	l, reg := newTestLimiter(c, WithStore(store))
	limit := rpm(100)

	if ok, _ := l.Allow(1, limit); !ok {
		t.Fatal("第 1 次被拒")
	}
	if v := reg.RateLimitExact.Value(); v != 1 {
		t.Fatalf("第 1 次之后信号 = %d，期望 1", v)
	}

	store.setDown(true)
	l.Allow(1, limit)
	if v := reg.RateLimitExact.Value(); v != 0 {
		t.Fatalf("存储挂掉之后信号 = %d，期望 0", v)
	}

	store.setDown(false)
	l.Allow(1, limit)
	if v := reg.RateLimitExact.Value(); v != 1 {
		t.Errorf("存储恢复之后信号 = %d，期望 1（不需要任何显式切换）", v)
	}
}

// TestProbeSetsTheSignalWithoutTraffic 钉住启动那一次探活。
//
// 不探的话，那个信号要等到第一个请求才出现，而「还没来过请求」与「共享
// 存储根本不该有这一行」在 /metrics 上没有任何分别。
func TestProbeSetsTheSignalWithoutTraffic(t *testing.T) {
	store := newMemStore()
	l, reg := newTestLimiter(&clock{at: base}, WithStore(store))

	if _, ok := exactSignal(t, reg); ok {
		t.Error("还没探活就输出了那个仪表：它应当是「没设置过」")
	}

	if err := l.Probe(context.Background()); err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if v, _ := exactSignal(t, reg); v != 1 {
		t.Errorf("探活通过之后信号 = %d，期望 1", v)
	}

	store.setDown(true)
	if err := l.Probe(context.Background()); err == nil {
		t.Error("存储不可用时探活应当报错")
	}
	if v, _ := exactSignal(t, reg); v != 0 {
		t.Errorf("探活失败之后信号 = %d，期望 0", v)
	}
	// 探活失败不计进「判定退回进程内计数」那个数：它说的是判定，而这一次
	// 还没有请求。
	if got := reg.RateLimitStoreErrors.Value(); got != 0 {
		t.Errorf("探活失败把存储错误计数加到了 %d，期望 0（那是判定的计数）", got)
	}

	// 没有共享存储的部署：探活永远成功，而信号是近似。
	bare, bareReg := newTestLimiter(&clock{at: base})
	if err := bare.Probe(context.Background()); err != nil {
		t.Fatalf("没有共享存储时探活不该失败: %v", err)
	}
	if v, _ := exactSignal(t, bareReg); v != 0 {
		t.Errorf("没有共享存储时信号 = %d，期望 0（近似）", v)
	}
}

// TestSlowStoreFallsBackWithinBudget 钉住热路径的时间预算。
//
// 存储「连得上但很慢」时，请求不能跟着一起慢：超时按不可用处理，判定退回
// 进程内计数。这里断言的是一个宽松的上界（一秒），因为要钉的是「有没有
// 超时这件事」，而不是那个常数本身——常数写死在一处，改它要连着这一条一起看。
func TestSlowStoreFallsBackWithinBudget(t *testing.T) {
	c := &clock{at: base}
	l, reg := newTestLimiter(c, WithStore(blockStore{}))
	limit := rpm(1)

	start := time.Now()
	allowed, _ := l.Allow(1, limit)
	elapsed := time.Since(start)

	if !allowed {
		t.Error("存储慢导致请求被拒：超时应当按不可用处理，放行而不是拒绝")
	}
	if elapsed > time.Second {
		t.Errorf("一次判定花了 %v，超出热路径预算（期望几十毫秒量级）", elapsed)
	}
	if v := reg.RateLimitExact.Value(); v != 0 {
		t.Errorf("存储超时之后信号 = %d，期望 0", v)
	}
}

// TestUsageIsReadOnly 钉住那条只读查询什么也不改。
//
// 判据是三个**不变**：计数不变、共享存储里的数不变、那个健康信号不变。
// 只断言「返回了一个数」是不够的——一个会顺手 INCR 的实现也能返回一个数，
// 而它会让「查一下用量」变成「把自己算进额度里」。
func TestUsageIsReadOnly(t *testing.T) {
	store := newMemStore()
	c := &clock{at: base}
	l, reg := newTestLimiter(c, WithStore(store))
	limit := rpm(10)

	l.Allow(5, limit)
	l.Allow(5, limit)

	key := windowKey(5, base)
	before := store.value(key)

	for i := 0; i < 5; i++ {
		u := l.Usage(context.Background(), 5)
		if u.Count != 2 {
			t.Fatalf("第 %d 次查询读到 %d，期望 2", i+1, u.Count)
		}
		if !u.Exact {
			t.Error("共享存储可用时 Exact 应当为真")
		}
		if u.KeyID != 5 {
			t.Errorf("KeyID = %d，期望 5", u.KeyID)
		}
		if !u.WindowStart.Equal(base.Truncate(windowLen)) || !u.WindowEnd.Equal(base.Truncate(windowLen).Add(windowLen)) {
			t.Errorf("窗口 = [%v, %v)，期望 [%v, %v)",
				u.WindowStart, u.WindowEnd, base.Truncate(windowLen), base.Truncate(windowLen).Add(windowLen))
		}
	}

	if after := store.value(key); after != before {
		t.Errorf("查了 5 次之后共享存储里的计数从 %d 变成了 %d：这条路径只能读", before, after)
	}
	// 那个健康信号由 Allow 更新。这里是先 Allow 过的，所以它是 1；查询
	// 若干次之后它不该变。
	if v := reg.RateLimitExact.Value(); v != 1 {
		t.Errorf("只读查询把健康信号改成了 %d，期望 1（它不该被这条路径动）", v)
	}
	// 额度是 10，前面只打了 2 次：查询没有把自己算进去。
	if ok, _ := l.Allow(5, limit); !ok {
		t.Error("第 3 次被拒：查询把自己算进额度里了")
	}
}

// TestUsageFallsBackToLocalWhenStoreIsDown 钉住降级时那条查询也说得清。
//
// 存储不可用时它回的是本进程看到的那一份，并把 Exact 置假——回一个 0 而
// 不说来源，会让运维以为「这个 key 一次都没被打过」。
func TestUsageFallsBackToLocalWhenStoreIsDown(t *testing.T) {
	store := newMemStore()
	c := &clock{at: base}
	l, _ := newTestLimiter(c, WithStore(store))
	limit := rpm(10)

	l.Allow(9, limit) // 共享存储里记 1 次
	store.setDown(true)
	l.Allow(9, limit) // 这一次退回进程内，本地记 1 次

	u := l.Usage(context.Background(), 9)
	if u.Exact {
		t.Error("存储不可用时 Exact 应当为假")
	}
	if u.Count != 1 {
		t.Errorf("Count = %d，期望 1（本进程记的那一次）", u.Count)
	}
}

// TestUsageWithoutStoreIsApproximate 钉住「没配共享存储」那一档。
func TestUsageWithoutStoreIsApproximate(t *testing.T) {
	c := &clock{at: base}
	l, _ := newTestLimiter(c)
	l.Allow(3, rpm(10))

	u := l.Usage(context.Background(), 3)
	if u.Exact {
		t.Error("没有共享存储时 Exact 应当为假")
	}
	if u.Count != 1 {
		t.Errorf("Count = %d，期望 1", u.Count)
	}

	// 从没见过的 key：0 次，也是近似。
	if u := l.Usage(context.Background(), 4); u.Count != 0 || u.Exact {
		t.Errorf("没见过的 key 读到 %+v，期望 count=0、exact=false", u)
	}
}

// TestLocalUsageIgnoresStaleWindow 钉住只读查询不会把上一个窗口的残留读出来。
//
// 进程内那份记录只留「当前窗口」一条，跨窗口之后旧的那条要当作不存在——
// 它还没被 sweep 扫掉，而 sweep 是写操作，这条路径不能调它。
func TestLocalUsageIgnoresStaleWindow(t *testing.T) {
	c := &clock{at: base}
	l, _ := newTestLimiter(c)

	for i := 0; i < 3; i++ {
		l.Allow(1, rpm(10))
	}
	if u := l.Usage(context.Background(), 1); u.Count != 3 {
		t.Fatalf("当前窗口读到 %d，期望 3", u.Count)
	}

	c.set(base.Add(windowLen))
	if u := l.Usage(context.Background(), 1); u.Count != 0 {
		t.Errorf("跨窗口之后读到 %d，期望 0（上一个窗口的残留不算）", u.Count)
	}
	if got := len(l.windows); got != 1 {
		t.Errorf("只读查询顺手清了 map：现在有 %d 条记录，期望 1（清理是写操作，由 Allow 做）", got)
	}
}

// TestNilRPMNeverTouchesTheStore 钉住「不限流的 key 不进计数」在共享存储上也成立。
//
// 没有额度就没有要记的状态：为它多一次网络往返，是拿热路径的预算换一个
// 谁都不会读的数。
func TestNilRPMNeverTouchesTheStore(t *testing.T) {
	store := newMemStore()
	c := &clock{at: base}
	l, _ := newTestLimiter(c, WithStore(store))

	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow(1, nil); !ok {
			t.Fatal("NULL 额度不该拒请求")
		}
	}
	if n := store.size(); n != 0 {
		t.Errorf("共享存储里留下了 %d 条记录，期望 0", n)
	}
	if got := len(l.windows); got != 0 {
		t.Errorf("进程内计数器里留下了 %d 条记录，期望 0", got)
	}
}
