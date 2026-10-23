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

	// SettingKeyUsageCompletionIncludesReasoning 是 SettingKeyUsage 那一块里的
	// 内层键，回答「上游报的输出 token 里含不含推理」。
	//
	// 它是**内层**键：单独写出来没有意义，要连着外层写成
	// {"usage": {"completion_includes_reasoning": false}}。管理面点名时把它
	// 与外层用点号连起来（usage.completion_includes_reasoning），否则同一句话
	// 里的「usage」与「completion_includes_reasoning」分不出谁是谁。
	//
	// 读者同样是那三个适配器，读法还是 UsageCompletionIncludesReasoning。
	SettingKeyUsageCompletionIncludesReasoning SettingKey = "completion_includes_reasoning"
)

// settingKeys 是一个渠道 type 认的 setting 键。
//
// top 是这一层的键；inner 是其中带内层键的那几个键 → 它那一块里认的内层键。
// 两层都只列**有读者**的键：写进去的键在名单里找不到，就会被点名（见
// SettingUnreadNote）。
//
// inner 里没有出现的顶层键，管理面不下探它那一层：那不代表「一个内层键都不
// 认」，而是「那一层的内层键名没有读者解释」。reasoning_effort_map 就是这种
// 开放映射——它的键由上游决定（见 SettingKeyReasoningEffortMap），逐个点名
// 只会把真正写错的那一处淹掉。
type settingKeys struct {
	top   []SettingKey
	inner map[SettingKey][]SettingKey
}

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
// setting_test.go）。内层键也在这两条判据里：用例认得出「先从 setting 取出
// 一块、再从那一块按下标取键」的读法，按点号路径与表里的 inner 对账。
//
// **没有键的 type 也要在这里占一行**（空结构）：一行都没有的类型会被当成
// 「本版本不认识的 type」而跳过判据，于是那个类型上的每个键都能静默成功。
// 表覆盖的类型集合与 protocolEndpoints 相同，有用例逐个核对。
var settingKeysByType = map[Protocol]settingKeys{
	ProtocolOpenAI: {
		top: []SettingKey{SettingKeyReasoningEffortMap, SettingKeyUsage},
		inner: map[SettingKey][]SettingKey{
			SettingKeyUsage: {SettingKeyUsageCompletionIncludesReasoning},
		},
	},
	ProtocolAnthropic: {
		top: []SettingKey{SettingKeyUsage},
		inner: map[SettingKey][]SettingKey{
			SettingKeyUsage: {SettingKeyUsageCompletionIncludesReasoning},
		},
	},
	ProtocolOpenAIResponses: {
		top: []SettingKey{SettingKeyReasoningEffortMap, SettingKeyUsage},
		inner: map[SettingKey][]SettingKey{
			SettingKeyUsage: {SettingKeyUsageCompletionIncludesReasoning},
		},
	},
	ProtocolSystemOne: {},
}

// unreadSettingKeys 返回 setting 里该渠道类型不认的键，按字典序排好。
//
// 内层键按点号连起来返回（usage.completion_includes_reasoning）：话里只有
// 「completion_includes_reasoning」的话，读的人不知道它是哪一块下面的键，而
// 同一个键名在别的块里可能有读者。
//
// 只在顶层键**有读者、且这一层声明了内层键**时下探。整块不是对象时按「没
// 声明」处理，不报错也不 panic——值的形状没有判据（见
// UsageCompletionIncludesReasoning）。
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
	for key, value := range setting {
		top := SettingKey(key)
		if !slices.Contains(known.top, top) {
			unknown = append(unknown, key)
			continue
		}
		inner, nested := known.inner[top]
		if !nested {
			continue
		}
		block, ok := value.(map[string]any)
		if !ok {
			continue
		}
		for innerKey := range block {
			if !slices.Contains(inner, SettingKey(innerKey)) {
				unknown = append(unknown, key+"."+innerKey)
			}
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
//
// 点的是 setting 里的键名；内层键与外层用点号连起来
// （usage.completion_includes_reasoning），因为同一个内层键名在别的块里
// 可能有读者，只写后半截分不出说的是哪一处。
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
