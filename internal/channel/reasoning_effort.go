package channel

// reasoningEffortMapKey 是 Setting 里那张 reasoning_effort 方言映射表的键。
//
// 形状是 {"none": "off"}：左边的键是我们内部的规范取值，右边的值是该渠道
// 认的写法；映射到空字符串表示这个字段整条不出现在出站报文里。
//
// 键归适配器解释，选路层不读它（见 Channel.Setting 的注释）。
const reasoningEffortMapKey = "reasoning_effort_map"

// ReasoningEffortDialect 按渠道的映射表翻译 reasoning_effort 的取值。
//
// 返回空串表示报文里不该出现这个字段。两个适配器的字段都带 omitempty，
// 赋空串就是把它剔掉；Responses 那边还要再看整个 reasoning 对象是否因此
// 变空（见 responsesadapter 的 reasoningHasNothing）。
//
// 三条规则：
//
//   - 表里没有的取值原样透传。映射表描述的是方言差异，不是「上游支持
//     什么」，所以没配这个键的渠道，报文与没有这张表时逐字相同；
//   - 请求本来就没带这个参数时不去查表：映射表只翻译已有的取值，不该
//     凭空给一条没要求过推理的请求加上这个字段；
//   - 表或其中某一项形状不对时按「没有这一项」处理，不报错也不 panic。
//     setting 本来就不做键名校验（见 Channel.Setting），写错的
//     值静默不生效，与拼错键名是同一种结果；而一项写坏不该让整条渠道的
//     请求都发不出去。
func ReasoningEffortDialect(setting map[string]any, effort string) string {
	if effort == "" {
		return ""
	}
	// 键由 map 的类型保证是字符串；这里要防的是值：整张表可能是字符串、
	// 数组之类的别的形状，这时当作没配。
	table, ok := setting[reasoningEffortMapKey].(map[string]any)
	if !ok {
		return effort
	}
	mapped, ok := table[effort]
	if !ok {
		return effort
	}
	// 值不是字符串（例如写成 true 或一个嵌套对象）时跳过这一项，
	// 按「表里没有」处理。见函数注释最后一条。
	s, ok := mapped.(string)
	if !ok {
		return effort
	}
	return s
}
