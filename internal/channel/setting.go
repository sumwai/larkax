package channel

// SettingReadByAdapters 声明「今天有没有适配器读 Channel.Setting」。
//
// 它是**代码的事实**，不是一条能被运行时检查出来的判据，所以与
// Channel.Setting 的注释放在同一个包里：接线的人改的正是适配器，
// 而这条声明就在它旁边。第一个适配器开始读某个键时把它改成 true，
// SettingUnreadNote 随之返回空串，管理面那两处提示一起消失。
//
// 它不靠自己守信用：internal/channel 的用例会扫一遍各适配器与选路层，
// 看见有人读 Setting 而这一位还是 false 时判失败（见 setting_test.go）。
const SettingReadByAdapters = false

// SettingUnreadNote 说明 setting 里的键今天没有读者。
//
// 返回空串表示已经有适配器在读它，调用方不必再打提示。
//
// 管理人面只有两处会打它：写入成功之后（`channel create` / `update`）与
// `channel list` 的记号。两处用的是同一句话——各写一份的话，一处改了一处
// 不动，而「配了没生效」正是这句话要挡的那种问题。
//
// 措辞里给出下一步，而不只是「不支持」：键归适配器解释，所以「什么时候生效」
// 有确定的答案（某个适配器实现它的时候），而这也解释了为什么写进去的值
// 不该被当成配置错误。
func SettingUnreadNote() string {
	if SettingReadByAdapters {
		return ""
	}
	return "今天没有任何适配器读取 setting，这次配置不会影响请求路径" +
		"（键由适配器解释，要等某个适配器实现它才会生效）"
}
