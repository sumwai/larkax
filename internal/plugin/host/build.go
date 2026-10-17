package host

import (
	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// Build 把标准能力组装成 sandbox 认识的那份宿主描述。
//
// # 为什么这一步要提成生产代码
//
// 这段拼装原先只存在于测试里（internal/plugin/e2e_test.go）。测试里再写一份
// 的代价不是重复几行，而是**两份实现会各自漂移**：测试那份少一个能力时，
// 用例照样全绿（它测的是「测试自己接的线」），而生产那份少一个能力时，
// 表现是某个插件调 log() 拿到「能力未启用」——一处不报错、另一处报在
// 插件作者面前。所以只有一处实现，测试也用这一处。
//
// # 编译期与请求期
//
// 这个函数是**请求期**用的：它要把用户身份与请求标识带进日志与 KV 命名空间。
// 编译期那一次求值（sandbox.Compile 的 inspect）不注入任何能力，也不该注入
// ——那时还没有用户，也不该往任何人的命名空间里写东西。所以加载器与
// `larkaxctl plugin publish` 都不调它。
//
// Env 里的 RequestID 与 UserID 在装配期是空的：调用方按请求填。这一点是预期的，
// 不是漏了——一个在装配期就带着空 UserID 建出来的 Host 会把 KV 写进
// user_id=0 的命名空间，而那个命名空间谁也不会去读。
//
// OnError 必须接上：能力自身的失败原文（连接串、驱动细节、上游地址）只走
// 这一条路进宿主日志。插件那一侧拿到的是固定文案（见 host.unavailable），
// 所以不接 OnError 的话，排查时两边都看不到原因。
func Build(env Env) sandbox.Host {
	caps := make(map[string]any)
	for _, c := range Standard(env) {
		// 依赖不齐的能力不注入：插件侧于是收到「能力未启用」，
		// 而不是一个永远失败的空壳。
		if !c.Available() {
			continue
		}
		caps[c.Name] = c.HostValue()
	}
	return sandbox.Host{
		Capabilities: caps,
		OnError: func(name string, err error) {
			env.logger().Error("插件能力失败",
				"plugin", env.Plugin,
				"request_id", env.RequestID,
				"capability", name,
				"err", err)
		},
	}
}
