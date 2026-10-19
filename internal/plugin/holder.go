package plugin

import "sync/atomic"

// Holder 持有「当前发布的那一份注册表」。
//
// # 它为什么存在
//
// Registry 是不可变发布的：换表得到一份新的，旧的继续被在途请求使用（见
// Registry 的注释）。热更新要的就是这件事——一次请求在开始时取一次，整个请求
// 都用那一份；切换是原子的，换的只是这里的一个指针。
//
// 把 Registry 本身做成可变的，每一次读都得加锁，而请求路径上的读点会因此互相
// 排队；而且「这一份什么时候被换掉」会散落到每个读点里。
//
// # nil 与零值
//
// nil 的 *Holder 与零值的 Holder 里都没有注册表，Load 返回 nil。nil 注册表的
// 意思是「插件系统没接上」，与「接上了但一个插件都没有」（Empty）不是一回事。
type Holder struct {
	reg atomic.Pointer[Registry]
}

// NewHolder 用一份注册表造一个持有者。
func NewHolder(reg *Registry) *Holder {
	h := &Holder{}
	h.reg.Store(reg)
	return h
}

// Load 返回当前那一份注册表。
//
// 调用方**一次请求取一次**，不要在每个 hook 上重取：重取会让同一次请求的
// onRequest 与 onResponse 跑在两份不同的注册表上，而插件会话本来就是按「同一个
// 插件的一份运行时」组织的（见 Session）。
func (h *Holder) Load() *Registry {
	if h == nil {
		return nil
	}
	return h.reg.Load()
}

// Store 把当前注册表换成另一份。在途请求手里那一份不受影响。
func (h *Holder) Store(reg *Registry) {
	h.reg.Store(reg)
}
