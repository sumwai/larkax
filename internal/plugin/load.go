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
//     日志）与 `larkaxctl plugin list` 里那一行的状态。
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
func PublishPartial(regs []Registration, onSkip func(Registration, error)) (*Registry, error) {
	compiled := make([]*Plugin, 0, len(regs))
	for _, reg := range regs {
		p, err := Compile(reg)
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
