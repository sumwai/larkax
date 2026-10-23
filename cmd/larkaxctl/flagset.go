package main

import (
	"flag"
	"time"
)

// 这一层给每个命令的 flag.FlagSet 包一层：注册选项时把**名字**记下来。
//
// 补全要列出「这条命令能写哪些长选项」，而选项名只存在于各命令自己构造的
// flag.FlagSet 里（见 channel.go 的 parseChannelCreate 这一类）。补全不能
// 另抄一份名单——两份数据迟早会分叉，分叉的表现是 Tab 补出来的选项敲下去
// 报「flag provided but not defined」。
//
// 所以收集分两步：生成补全时把每条命令路径按 `<命令> <动作> -h` 走一遍
// （见 completion.go 的 collectCommandFlags），`-h` 让 flag 在解析阶段就
// 停下，走不到连库与写库；这期间本文件把每次注册的名字按**当前路径**记下来。
//
// 包装器只覆盖本包用到的注册方法（见 usage 里那几个 helper 与各 parse 函数）。
// 新增一种注册方法时漏掉这里，表现是那一类选项不出现在补全里——没有用例会
// 报出来，所以加新方法时顺手看一眼这里。
type flagSet struct {
	*flag.FlagSet
}

// newFlagSet 是 flag.NewFlagSet 的替代品：其余用法与它完全一致。
func newFlagSet(name string, handling flag.ErrorHandling) *flagSet {
	return &flagSet{FlagSet: flag.NewFlagSet(name, handling)}
}

// record 在收集期间记下一个选项名。
func (f *flagSet) record(name string) {
	if !collectingCommandFlags {
		return
	}
	collectedFlags[currentCommandPath] = append(collectedFlags[currentCommandPath], name)
}

// Var 见 flag.FlagSet.Var。
func (f *flagSet) Var(value flag.Value, name, usage string) {
	f.record(name)
	f.FlagSet.Var(value, name, usage)
}

// String 见 flag.FlagSet.String。
func (f *flagSet) String(name, value, usage string) *string {
	f.record(name)
	return f.FlagSet.String(name, value, usage)
}

// StringVar 见 flag.FlagSet.StringVar。
func (f *flagSet) StringVar(p *string, name, value, usage string) {
	f.record(name)
	f.FlagSet.StringVar(p, name, value, usage)
}

// Bool 见 flag.FlagSet.Bool。
func (f *flagSet) Bool(name string, value bool, usage string) *bool {
	f.record(name)
	return f.FlagSet.Bool(name, value, usage)
}

// BoolVar 见 flag.FlagSet.BoolVar。
func (f *flagSet) BoolVar(p *bool, name string, value bool, usage string) {
	f.record(name)
	f.FlagSet.BoolVar(p, name, value, usage)
}

// Int 见 flag.FlagSet.Int。
func (f *flagSet) Int(name string, value int, usage string) *int {
	f.record(name)
	return f.FlagSet.Int(name, value, usage)
}

// IntVar 见 flag.FlagSet.IntVar。
func (f *flagSet) IntVar(p *int, name string, value int, usage string) {
	f.record(name)
	f.FlagSet.IntVar(p, name, value, usage)
}

// Int64 见 flag.FlagSet.Int64。
func (f *flagSet) Int64(name string, value int64, usage string) *int64 {
	f.record(name)
	return f.FlagSet.Int64(name, value, usage)
}

// 下面三个是别名专用的注册入口：旧名与简写**不记名字**。
//
// 补全只列正式的长选项（见 completionOptionNames）；把 `--owner`、`-m` 这类
// 写法也列进 Tab，等于同时推荐两套写法，而其中一套是准备淘汰的。
func (f *flagSet) aliasVar(value flag.Value, name string) {
	f.FlagSet.Var(value, name, "")
}

// aliasBool 注册一个布尔别名。
func (f *flagSet) aliasBool(p *bool, name string) {
	f.FlagSet.BoolVar(p, name, false, "")
}

// aliasInt 注册一个整数别名。
func (f *flagSet) aliasInt(p *int, name string) {
	f.FlagSet.IntVar(p, name, 0, "")
}

// Duration 见 flag.FlagSet.Duration。
func (f *flagSet) Duration(name string, value time.Duration, usage string) *time.Duration {
	f.record(name)
	return f.FlagSet.Duration(name, value, usage)
}
