package channel

// usageSettingKey 是 Setting 里「用量口径」那一块的键。
//
// 形状是 {"completion_includes_reasoning": false}：块的键归本函数解释，
// 选路层不读它（见 Channel.Setting）。取值声明在 SettingKeyUsage，管理面按
// 「渠道 type 认哪些键」判一条 setting 有没有读者，那一侧的判据必须与这里
// 读的键是同一个值。
const usageSettingKey = string(SettingKeyUsage)

// UsageCompletionIncludesReasoning 读渠道对「推理是否已算在输出里」的声明。
//
// 返回 true 表示 CompletionTokens 已经含 ReasoningTokens（OpenAI 与 Anthropic
// 的口径），false 表示两者互斥、上游各报各的。
//
// 三条规则：
//
//   - 没声明时返回 true。缺声明落到的那一边沿用改动前的口径：relaykit 的
//     dto.Usage 是 OpenAI 语义的形状，Anthropic 的 output_tokens 也含思考
//     token，两者都是「已含」。只有知道自己是互斥口径的渠道需要显式写 false。
//   - 声明写成非布尔值时按「没声明」处理，不报错也不 panic。setting 的键名由
//     管理面负责点名（见 SettingUnreadNote），值的形状没有人判：写错的值静默
//     不生效，而一项写坏不该让整条渠道的请求都发不出去。
//   - 只认嵌套一层的形状。整块写成字符串、数组之类的别的形状时同样按
//     「没声明」处理。
func UsageCompletionIncludesReasoning(setting map[string]any) bool {
	table, ok := setting[usageSettingKey].(map[string]any)
	if !ok {
		return true
	}
	declared, ok := table["completion_includes_reasoning"].(bool)
	if !ok {
		return true
	}
	return declared
}
