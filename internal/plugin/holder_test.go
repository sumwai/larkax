package plugin

import (
	"sync"
	"testing"
)

// 判据：nil 的持有者与「接上了但一个插件都没有」是两件事。
//
// api.Handler 拿 nil 表示插件系统没接上，行为要与插件出现之前完全一样；
// 而 Empty() 是一份真的注册表，只是里面没有插件。两者都能 Load，结果一个
// 是 nil、一个非 nil——调用方据此走的路径不同。
func TestHolderNilAndEmptyAreDifferent(t *testing.T) {
	var nilHolder *Holder
	if reg := nilHolder.Load(); reg != nil {
		t.Fatalf("nil 持有者该返回 nil，实际返回了 %v", reg)
	}

	var zero Holder
	if reg := zero.Load(); reg != nil {
		t.Fatalf("零值持有者该返回 nil，实际返回了 %v", reg)
	}

	empty := NewHolder(Empty())
	reg := empty.Load()
	if reg == nil {
		t.Fatal("装着空注册表的持有者不该返回 nil：那与「没接上」分不开")
	}
	if reg.Len() != 0 {
		t.Fatalf("空注册表该有 0 个插件，实际 %d 个", reg.Len())
	}
}

// 判据：换表之后 Load 返回新的那一份，而**换表之前取到的**那一份不受影响。
//
// 「在途请求继续用旧代次」这条性质就落在这一点上：请求开始时 Load 一次，
// 之后换成什么都与它手里那份无关。
func TestHolderStoreLeavesLoadedPointerAlone(t *testing.T) {
	first := mustPublish(t, goodReg("p1", 0))
	second := mustPublish(t, goodReg("p2", 0))

	holder := NewHolder(first)
	inFlight := holder.Load()
	if inFlight != first {
		t.Fatal("Load 该返回刚放进去的那一份")
	}

	holder.Store(second)
	if got := holder.Load(); got != second {
		t.Fatal("Store 之后 Load 该返回新的那一份")
	}
	if inFlight != first {
		t.Fatal("换表之前取到的那一份不该跟着变")
	}
	if _, ok := inFlight.Get("p1"); !ok {
		t.Error("在途请求手里那一份该还是旧的那一代（p1）")
	}
	if _, ok := holder.Load().Get("p2"); !ok {
		t.Error("新的那一份该是 p2")
	}
}

// 并发读写不该有数据竞争，也不该让任何一次读看到「半份」注册表。
//
// 这条不判内容对错，只判在 -race 下不红、并且读到的永远是一份完整的注册表。
func TestHolderConcurrentLoadAndStore(t *testing.T) {
	a := mustPublish(t, goodReg("a", 0))
	b := mustPublish(t, goodReg("b", 0))
	holder := NewHolder(a)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				reg := holder.Load()
				if reg == nil {
					t.Error("装着注册表的持有者返回了 nil")
					return
				}
				if reg.Len() != 1 {
					t.Errorf("读到一份 %d 个插件的注册表，期望 1 个", reg.Len())
					return
				}
			}
		}()
	}
	for j := 0; j < 100; j++ {
		holder.Store(a)
		holder.Store(b)
	}
	wg.Wait()
}
