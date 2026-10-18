package plugin

// PublishPartial 逐个编译，**跳过编译不过的那些**，把其余发布成一份注册表。
//
// # 为什么启动路径选「跳过」而不是「整体拒绝」
//
// 两者都能做到原子发布（先全部编译完，再一次性换表），区别只在坏插件的影响面：
//
//   - 整体拒绝：库里有一条坏插件，**整个服务起不来**。而插件是可选增强
//     （plugin.enabled 默认就是 false），一个作者写坏的插件把入口一起拖下水，
//     代价与它带来的功能完全不成比例。
//   - 跳过：损失被限制在那一个插件上，其余照常上线。代价是「它没生效」这件事
//     本身是安静的，所以必须有两处把它说出来：onSkip 拿到的错误（调用方记进
//     日志，并把结果写回库里那三列——见 store 的 LoadRegistry）与
//     `larkaxctl plugin list` 里「上次加载」那一栏。
//
// 选跳过还因为**坏插件是能修好的**：修完重启即可，而整体拒绝时运维手上只有
// 一个起不来的进程，还得先去库里找出是哪一条。
//
// # 「插件坏了」与「加载器坏了」怎么分开
//
// 这里只处理前者：编译不过的插件走 onSkip，函数本身**不返回错误**去描述它。
// 后者（连不上库、SQL 写错、表不存在）发生在读表那一步，由调用方在进入本函数
// 之前就返回——两条路于是各有各的文案，不会混成一句「加载插件失败」。
//
// onSkip 为 nil 时静默跳过（只有测试会这么用；生产路径一律传一个记日志的）。
//
// provided 是这次装配真的提供的能力名，交给 CompileWith（见那个函数的注释）：
// 「插件坏了」那一档里因此包含「它申请了这份装配没有的能力」——那是注册期就该
// 失败的一类，而不是注册成功、运行期拿到 undefined。
func PublishPartial(regs []Registration, provided []string, onSkip func(Registration, error)) (*Registry, error) {
	compiled := make([]*Plugin, 0, len(regs))
	for _, reg := range regs {
		p, err := CompileWith(reg, provided)
		if err != nil {
			if onSkip != nil {
				onSkip(reg, err)
			}
			continue
		}
		compiled = append(compiled, p)
	}
	return assemble(compiled)
}
