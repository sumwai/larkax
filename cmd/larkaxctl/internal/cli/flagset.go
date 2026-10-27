package cli

import (
	"flag"
	"time"
)

// FlagSet 给每个命令的 flag.FlagSet 包一层：注册选项时把**名字**记下来，
// 顺带记下它吃不吃取值（fish 的补全靠后者决定给不给选项加 `-r`）。
//
// 补全要列出「这条命令能写哪些长选项」，而选项名只存在于各命令自己构造的
// flag.FlagSet 里（见 cmd/larkaxctl 的 channel.go 的 parseChannelCreate 这一类）。补全不能
// 另抄一份名单——两份数据迟早会分叉，分叉的表现是 Tab 补出来的选项敲下去
// 报「flag provided but not defined」。
//
// 所以收集分两步：生成补全时把每条命令路径按 `<命令> <动作> -h` 走一遍
// （见本包的 CollectFlags），`-h` 让 flag 在解析阶段就停下，走不到连库与写库；
// 这期间本文件把每次注册的名字按**当前路径**记下来。
//
// 包装器只覆盖本包用到的注册方法：Var、String、Bool、Int、Int64、Duration，
// 以及别名专用的 aliasVar / aliasBool / aliasInt（那三个不记名字，见下文）。
// 两头都要看：新增一种注册方法时漏掉这里，那一类选项不会出现在补全里，而没有
// 用例会报出来；反过来，包一层没人调用的方法本身就是一个不可达符号（deadcode
// 会报），所以加方法前先确认有调用方。
type FlagSet struct {
	*flag.FlagSet
}

// NewFlagSet 是 flag.NewFlagSet 的替代品：其余用法与它完全一致。
func NewFlagSet(name string, handling flag.ErrorHandling) *FlagSet {
	return &FlagSet{FlagSet: flag.NewFlagSet(name, handling)}
}

// TakesValue 判断一种 flag.Value 吃不吃取值。
//
// 判据是标准库的 IsBoolFlag 约定：实现了它并返回 true 的是布尔开关，后面的词
// 不是它的取值；其余（String、Int、Duration，以及别名共用的 stringStorage）
// 都要跟一个值。fish 的补全靠这个决定给不给选项加 `-r`。
func TakesValue(value flag.Value) bool {
	b, ok := value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

// record 在收集期间记下一个选项名，以及它吃不吃取值。
//
// 只问会话（见 collect.go 的 CollectFlags）：会话之外是空操作，所以命令照常
// 跑的时候这一行什么都不做。
func (f *FlagSet) record(name string, takesValue bool) {
	recordCollectedFlag(name, takesValue)
}

// Var 见 flag.FlagSet.Var。
func (f *FlagSet) Var(value flag.Value, name, usage string) {
	f.record(name, TakesValue(value))
	f.FlagSet.Var(value, name, usage)
}

// String 见 flag.FlagSet.String。
func (f *FlagSet) String(name, value, usage string) *string {
	f.record(name, true)
	return f.FlagSet.String(name, value, usage)
}

// Bool 见 flag.FlagSet.Bool。
func (f *FlagSet) Bool(name string, value bool, usage string) *bool {
	f.record(name, false)
	return f.FlagSet.Bool(name, value, usage)
}

// Int 见 flag.FlagSet.Int。
func (f *FlagSet) Int(name string, value int, usage string) *int {
	f.record(name, true)
	return f.FlagSet.Int(name, value, usage)
}

// Int64 见 flag.FlagSet.Int64。
func (f *FlagSet) Int64(name string, value int64, usage string) *int64 {
	f.record(name, true)
	return f.FlagSet.Int64(name, value, usage)
}

// 下面三个是别名专用的注册入口：旧名与简写**不记名字**。
//
// 补全只列正式的长选项（见 completionOptionNames）；把 `--owner`、`-m` 这类
// 写法也列进 Tab，等于同时推荐两套写法，而其中一套是准备淘汰的。
//
// 三个都经 inner 调到内嵌的 flag.FlagSet：写成 `f.Var` 会经过上面的包装方法，
// 旧名就被记进补全了；`f.BoolVar` / `f.IntVar` 今天没有包装方法，但以后补上
// 一个时，这里会跟着悄悄改口径——所以显式走内嵌那一层。
func (f *FlagSet) aliasVar(value flag.Value, name string) {
	inner := f.FlagSet
	inner.Var(value, name, "")
}

// aliasBool 注册一个布尔别名。
func (f *FlagSet) aliasBool(p *bool, name string) {
	inner := f.FlagSet
	inner.BoolVar(p, name, false, "")
}

// aliasInt 注册一个整数别名。
func (f *FlagSet) aliasInt(p *int, name string) {
	inner := f.FlagSet
	inner.IntVar(p, name, 0, "")
}

// Duration 见 flag.FlagSet.Duration。
func (f *FlagSet) Duration(name string, value time.Duration, usage string) *time.Duration {
	f.record(name, true)
	return f.FlagSet.Duration(name, value, usage)
}
