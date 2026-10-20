package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// base 是所有用例的时间基准：一个窗口的正中间，离两侧边界都够远。
//
// 用具体时刻而不是 time.Now()：窗口判定依赖「现在是哪一分钟」，
// 而用当前时刻的用例会在整点附近给出随运行时间变化的结论。
var base = time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC)

// clock 是一个可以拨的时钟。
//
// 窗口边界是这套逻辑里唯一与时间有关的部分，必须能直接拨过去，
// 而不是 sleep 到那一刻——sleep 的用例在边界上必然不稳。
type clock struct{ at time.Time }

func (c *clock) now() time.Time  { return c.at }
func (c *clock) set(t time.Time) { c.at = t }

func rpm(v int) *int { return &v }

// TestNullMeansNoLimit 钉住 NULL 的语义：列值为 NULL 的 key 不限流。
//
// 次数取一个明显超过任何合理额度的数，并且比 60 大得多——如果实现里
// 悄悄按某个默认额度计数，这条用例会挂。
func TestNullMeansNoLimit(t *testing.T) {
	c := &clock{at: base}
	l := New(c.now)

	for i := 1; i <= 1000; i++ {
		allowed, retryAfter := l.Allow(1, nil)
		if !allowed {
			t.Fatalf("第 %d 次被拒：NULL 表示不限流，不该有第几次之说", i)
		}
		if retryAfter != 0 {
			t.Errorf("第 %d 次放行时 retryAfter = %v，期望 0", i, retryAfter)
		}
	}

	// 不限流的 key 不该在计数器里留下状态：留了就是内存随请求数增长。
	if got := len(l.windows); got != 0 {
		t.Errorf("计数器里留下了 %d 条记录，期望 0（NULL 的 key 不进计数）", got)
	}
}

// TestSixtyPerMinuteStopsAtSixtyOne 是这次改动的核心断言。
//
// 「设了 60，实际是无限」正是要修的 bug，所以这里断言的是**具体数字**：
// 前 60 次通过、第 61 次被拒，而不是「A 比 B 少」这类相对关系。
//
// 变异：`count <= int64(limit)` 的 `<=` 改成 `<`（同一行第 24 列）。第 60 次
// 是 count 正好等于额度的那一格，变异版会把它误拒，而这一次就在下面那个
// 循环的 i == 60 上，用例会在那里报错。
func TestSixtyPerMinuteStopsAtSixtyOne(t *testing.T) {
	c := &clock{at: base}
	l := New(c.now)
	limit := rpm(60)

	for i := 1; i <= 60; i++ {
		if allowed, _ := l.Allow(7, limit); !allowed {
			t.Fatalf("第 %d 次被拒：额度是 60，前 60 次都该通过", i)
		}
	}

	allowed, retryAfter := l.Allow(7, limit)
	if allowed {
		t.Fatal("第 61 次通过了：额度是 60，第 61 次必须被拒")
	}
	// 12:00:30 被拒 → 当前窗口还剩 30 秒。
	if retryAfter != 30*time.Second {
		t.Errorf("retryAfter = %v，期望 30s（12:00:30 距窗口结束 30 秒）", retryAfter)
	}

	// 再打几次仍然被拒：额度不会因为继续请求而松开。
	for i := 62; i <= 70; i++ {
		if allowed, _ := l.Allow(7, limit); allowed {
			t.Fatalf("第 %d 次通过了：超限之后这个窗口内不该再放行", i)
		}
	}
}

// TestZeroMeansNothingAllowed 钉住 0 的语义：一个都不放行。
//
// 它与 NULL 必须分开：NULL 扫进 int 会变成 0，而这两者的意思正相反。
//
// 变异：Allow 里 `limit > 0` 的 `>` 改成 `>=`（第 207 行第 11 列），是
// 一个**等价变异**。额度为 0 时变异版接着算 `count <= 0`，而 count 是
// **加一之后**的值——进程内那条先自增再返回，Redis 的 INCR 在缺键时返回 1，
// Store 的注释也写明「返回加后的值」——所以它恒大于 0，两版都会拒绝，
// retryAfter 也由同一个调用算出；额度非 0 时两个条件同真同假。在 Store
// 契约内没有能分开两版的输入，额度为 0 与负数的两条用例在变异版下照样全绿。
func TestZeroMeansNothingAllowed(t *testing.T) {
	c := &clock{at: base}
	l := New(c.now)

	if allowed, _ := l.Allow(1, rpm(0)); allowed {
		t.Fatal("额度为 0 时第 1 次就通过了：0 是「零次每分钟」，不是「不限」")
	}
}

// TestNegativeMeansNothingAllowed 负数是没有意义的配置，往严的一侧倒。
func TestNegativeMeansNothingAllowed(t *testing.T) {
	c := &clock{at: base}
	l := New(c.now)

	if allowed, _ := l.Allow(1, rpm(-5)); allowed {
		t.Fatal("额度为负数时放行了：负额度按禁止处理")
	}
}

// TestWindowResetsAtMinuteBoundary 钉住窗口的边界在哪。
//
// 窗口按墙钟分钟对齐：12:00:00.000–12:00:59.999 是一个窗口，
// 12:01:00.000 开始是下一个。用例把表拨到边界两侧各打一次，
// 数字是具体的：59 秒那一刻额度用完，0.5 秒后仍被拒，1 秒后重新放行。
func TestWindowResetsAtMinuteBoundary(t *testing.T) {
	c := &clock{at: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	l := New(c.now)
	limit := rpm(2)

	if allowed, _ := l.Allow(3, limit); !allowed {
		t.Fatal("窗口开头第 1 次被拒")
	}
	if allowed, _ := l.Allow(3, limit); !allowed {
		t.Fatal("窗口开头第 2 次被拒")
	}

	c.set(time.Date(2026, 10, 17, 12, 0, 59, 0, time.UTC))
	if allowed, _ := l.Allow(3, limit); allowed {
		t.Fatal("12:00:59 仍在同一个窗口内，第 3 次不该通过")
	}

	// 跨过边界：同一个 key 在新窗口里重新拿到完整的额度。
	c.set(time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC))
	for i := 1; i <= 2; i++ {
		if allowed, _ := l.Allow(3, limit); !allowed {
			t.Fatalf("12:01:00 是新窗口的第 %d 次，应当通过", i)
		}
	}
	if allowed, _ := l.Allow(3, limit); allowed {
		t.Fatal("12:01:00 之后的第 3 次通过了：新窗口的额度也是 2")
	}
}

// TestRetryAfterRoundsUpToWholeSeconds 钉住 retryAfter 的取整方向。
//
// 向下取整会让客户端在窗口结束前重试，而那次请求必然又被拒一次。
// 边界取三个具体时刻：剩 500 毫秒时回 1 秒（不是 0），
// 恰好剩 1 秒时回 1 秒，剩 30 秒时回 30 秒。
//
// 变异：retryAfterUntil 里 `remaining < time.Second` 改成 `<=`，是一个
// **等价变异**。恰好剩 1 秒那一格原版不进「至少 1 秒」那一条提前返回，
// 而是走下面的向上取整，而 `((1s + 1s - 1) / 1s) * 1s` 正好是 1s，与提前
// 返回的 1s 同值；其余输入两版落在同一分支——不到 1 秒的都在提前返回那一条
// 里，超过 1 秒的都取整。所以「恰好剩一秒」那一档在变异版下照样通过，
// 不是用例写得不够狠。
func TestRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want time.Duration
	}{
		{"整秒边界", time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC), 30 * time.Second},
		{"不足一秒向上取整", time.Date(2026, 10, 17, 12, 0, 59, 500_000_000, time.UTC), time.Second},
		{"恰好剩一秒", time.Date(2026, 10, 17, 12, 0, 59, 0, time.UTC), time.Second},
		{"有零头也向上取整", time.Date(2026, 10, 17, 12, 0, 0, 1, time.UTC), 60 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{at: tc.at}
			l := New(c.now)

			if allowed, _ := l.Allow(1, rpm(0)); allowed {
				t.Fatal("额度为 0 时放行了")
			}
			_, retryAfter := l.Allow(1, rpm(0))
			if retryAfter != tc.want {
				t.Errorf("retryAfter = %v，期望 %v", retryAfter, tc.want)
			}
		})
	}
}

// TestKeysAreCountedSeparately 额度是按 key 计的，一把打满不影响另一把。
func TestKeysAreCountedSeparately(t *testing.T) {
	c := &clock{at: base}
	l := New(c.now)
	limit := rpm(1)

	if allowed, _ := l.Allow(100, limit); !allowed {
		t.Fatal("key 100 的第 1 次被拒")
	}
	if allowed, _ := l.Allow(100, limit); allowed {
		t.Fatal("key 100 的第 2 次通过了：额度是 1")
	}
	// 另一把 key 有它自己的额度。
	if allowed, _ := l.Allow(200, limit); !allowed {
		t.Fatal("key 200 的第 1 次被拒：额度按 key 各算一份")
	}
}

// TestSweepDropsStaleWindows 钉住「计数不随历史 key 数增长」。
//
// 这是白盒用例：它看的是内部 map 的大小。之所以要测，是因为泄漏的表现
// 在行为上完全看不出来——判定结果全对，只是内存一直长。
func TestSweepDropsStaleWindows(t *testing.T) {
	c := &clock{at: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	l := New(c.now)
	limit := rpm(10)

	for id := int64(1); id <= 100; id++ {
		l.Allow(id, limit)
	}
	if got := len(l.windows); got != 100 {
		t.Fatalf("一个窗口内 100 把 key 各打一次，计数器里有 %d 条记录，期望 100", got)
	}

	// 下一个窗口只来一把 key：上一个窗口的 100 条都该被扫掉。
	c.set(time.Date(2026, 10, 17, 12, 1, 0, 0, time.UTC))
	l.Allow(1, limit)

	if got := len(l.windows); got != 1 {
		t.Errorf("新窗口里只剩 1 把 key 在用，计数器里有 %d 条记录，期望 1", got)
	}
}

// TestConcurrentAllowStopsExactlyAtLimit 并发下额度也是准的。
//
// 固定窗口的计数器是每个请求都要写一次的共享状态，而它在 -race 下最容易
// 暴露问题。断言的是**具体数字**：额度 50、并发 200 次，恰好 50 次通过。
// 计数不准（丢更新）时通过数会大于 50，那正是限流最坏的失效方向。
func TestConcurrentAllowStopsExactlyAtLimit(t *testing.T) {
	const (
		limit = 50
		calls = 200
	)
	fixed := base
	l := New(func() time.Time { return fixed })

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow(1, rpm(limit)); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Errorf("并发 %d 次里有 %d 次通过，期望恰好 %d 次", calls, allowed, limit)
	}
}

// TestUnlimitedKeysDoNotGrowState 顺手钉住「不限流的 key 不进 map」在并发下也成立。
func TestUnlimitedKeysDoNotGrowState(t *testing.T) {
	fixed := base
	l := New(func() time.Time { return fixed })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if ok, _ := l.Allow(id, nil); !ok {
					t.Errorf("key %d 的 NULL 额度不该拒请求", id)
					return
				}
			}
		}(int64(i))
	}
	wg.Wait()

	if got := len(l.windows); got != 0 {
		t.Errorf("计数器里有 %d 条记录，期望 0", got)
	}
}
