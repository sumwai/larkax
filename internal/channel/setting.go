package channel

import (
	"slices"
	"strings"
)

// SettingKey 是一个渠道私有配置键，例如 reasoning_effort_map。
//
// 键归**该渠道 type 指向的那个适配器**解释，选路层不认（见 Channel.Setting）。
// 取值在这里声明，适配器读它时引用同一个常量，不另写一份字符串：两处各写
// 一遍时，改了一处而另一处不动，表现是「管理面说这个键有读者，请求路径上
// 却没人读它」——或者反过来。
type SettingKey string

// 今天有读者的 setting 键。
const (
	// SettingKeyReasoningEffortMap 是 reasoning_effort 的方言映射表，
	// 形状 {"none": "off"}：左边的键是我们内部的规范取值，右边的值是该
	// 渠道认的写法；映射到空字符串表示这个字段整条不出现在出站报文里。
	//
	// 由 openai 与 openai-responses 两个适配器读，翻译共用
	// ReasoningEffortDialect（见 reasoning_effort.go）。
	SettingKeyReasoningEffortMap SettingKey = "reasoning_effort_map"

	// SettingKeyUsage 是用量口径的声明，形状
	// {"completion_includes_reasoning": false}。
	//
	// 它回答的是「上游报的 completion_tokens 里含不含推理」——两种口径在
	// 上游都真实存在，光看数字判不出来（见 channel.Usage 的注释）。不带这个
	// 声明时按 OpenAI / Anthropic 口径（含）读，只有显式写 false 才改。
	//
	// 三个适配器都读它（openai、anthropic、openai-responses），读法共用
	// UsageCompletionIncludesReasoning（见 usage_setting.go）。
	SettingKeyUsage SettingKey = "usage"
)

// settingKeysByType 是「渠道 type → 它的适配器认的键」这张表。
//
// 管理面据它判断一条渠道的 setting 里哪些键没有读者（见 SettingUnreadNote）。
// 键是渠道类型，与 Protocol 的取值是同一套字符串：Registry.Register 会拦住
// 类型名与协议名不一致的适配器，所以这里按 Protocol 做键与按 type 做键是
// 同一件事。
//
// 为什么是这张静态表，而不是 Adapter 接口上再加一个方法：管理面回答这个问题
// 时并不需要装配适配器。`channel list` 只读库；把「哪些键有读者」挂到注册表
// 上，答案就跟着进程有没有装配走——没装配时拿到的是「没法回答」，调用方要为
// 这种情形另写一条处置。静态表不依赖装配顺序，漏一行的后果由用例兜住，见
// TestSettingTypesCoverEveryProtocol。
//
// 加一个键只有两处：上面加常量，这里加一行。**适配器那一侧要真的读它**，
// 否则就是在替一条写进去也不会生效的配置作保。这条约定由同包的用例盯着，
// 两个方向都查：表里声明了而适配器没读、适配器读了而表里没写（见
// setting_test.go）。
//
// **没有键的 type 也要在这里占一行**（值为 nil）：一行都没有的类型会被当成
// 「本版本不认识的 type」而跳过判据，于是那个类型上的每个键都能静默成功。
// 表覆盖的类型集合与 protocolEndpoints 相同，有用例逐个核对。
var settingKeysByType = map[Protocol][]SettingKey{
	ProtocolOpenAI:          {SettingKeyReasoningEffortMap, SettingKeyUsage},
	ProtocolAnthropic:       {SettingKeyUsage},
	ProtocolOpenAIResponses: {SettingKeyReasoningEffortMap, SettingKeyUsage},
	ProtocolSystemOne:       nil,
}

// unreadSettingKeys 返回 setting 里该渠道类型不认的键，按字典序排好。
//
// 类型未知时返回 nil：那不是「所有键都没有读者」，而是「判不了」。这种渠道
// 本来就选不中（见 channels.Admin.validateType），再点它一句「键没读者」
// 只会把注意力引到错的地方。
func unreadSettingKeys(channelType string, setting map[string]any) []string {
	known, ok := settingKeysByType[Protocol(channelType)]
	if !ok {
		return nil
	}
	var unknown []string
	for key := range setting {
		if !slices.Contains(known, SettingKey(key)) {
			unknown = append(unknown, key)
		}
	}
	slices.Sort(unknown)
	return unknown
}

// SettingUnreadNote 说明 setting 里哪些键对该渠道类型没有读者。
//
// 返回空串表示没有这样的键，调用方不必打提示。
//
// # 为什么按 type 判，而不是「今天有没有适配器读 setting」
//
// 先前这里是一个全局布尔：只要有**任何一个**适配器读了 setting，提示就对所有
// 渠道消失。于是给一条渠道打开提示，等于替所有渠道关掉它——`anthropic` 渠道
// 配 `reasoning_effort_map`、或者 `openai` 渠道把键名拼错，都静默成功、永不
// 生效，而从库里与清单上看，它们与一条生效的配置一模一样。
//
// 现在判据落在「这条渠道的这个键」上：键是否有人读，取决于渠道的 type 指向
// 哪个适配器，而这张对应关系是本包的 settingKeysByType。
//
// # 措辞里说明两种可能
//
// 「没有读者」有两个来源：键名拼错了，或者该 type 的适配器还没接这个键。
// 只写「不支持」的话，前一种人会去等一个永远不会来的实现。两种情况下配置
// 都不会被拒绝、也不会被改写——它只是不影响请求路径。
//
// 管理面有两处会打它：写入成功之后（`channel create`，以及给了 `--setting`
// 或 `--type` 的 `channel update`）与 `channel list` 的记号。两处用的是同一
// 句话——各写一份的话，一处改了一处不动，而「配了没生效」正是这句话要挡的
// 那种问题。
func SettingUnreadNote(channelType string, setting map[string]any) string {
	unknown := unreadSettingKeys(channelType, setting)
	if len(unknown) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(unknown))
	for _, key := range unknown {
		quoted = append(quoted, "「"+key+"」")
	}
	return "键" + strings.Join(quoted, "、") + "对 type=" + channelType +
		" 的渠道没有读者——要么键名拼错了，要么该 type 的适配器还没接这个键，" +
		"两种情况都不会影响请求路径"
}
