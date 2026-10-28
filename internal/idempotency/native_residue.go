package idempotency

import (
	"encoding/json"
	"fmt"

	"github.com/sumwai/larkax/internal/channel"
)

// 读键表：原文里哪些键落在 hashInput 的哪个字段上。
//
//	原文键                                    hashInput 里的字段                 处置
//	顶层                                     见 nativeResidueOf 的按协议扣除表   按表
//	messages[] / input[] 的 role             Message.Role                       扣
//	messages[] / input[] 的 content 是字符串  ContentBlock.Text                  扣整个键
//	块 type=text / input_text + text          ContentBlock.Text                  扣 type、text
//	块 type=image / input_image + 图片地址    ImageURL / ImageBase64 /           扣
//	                                          ImageMediaType
//	块 type=tool_use + id / name / input      ToolUseID / ToolName / ToolInput   扣
//	块 type=tool_result + tool_use_id         ToolUseID                          扣
//	块 type=tool_result + content             ContentBlock.Text                  仅字符串或
//	                                                                             单块纯文本时扣
//	块 type=thinking                          **无**                             留
//	cache_control（任意层）                   **无**                             留
//	Responses input[] 的 id / status          **无**                             留
//	其余任何指不到上面任一字段的键            —                                  留
//
// 「扣」是把键从残差里删掉：它的值已经由 hashInput 的某个字段携带，留着只是
// 把同一件事算两遍。判据是**这个键能不能指到 hashInput 的哪个字段上**——
// 指得到就扣，指不到就留。枢纽读到、却没进 hashInput 的键（cache_control 是
// 典型：上游转换器确实读它，而 channel.ContentBlock 没有这个字段），扣掉就是
// 丢信息，两份不同的报文会共用指纹。
//
// # 遍历只认已知结构
//
// content 既不是字符串也不是数组的消息、或块 type 不在上表里的消息，**整条
// 原样留残差、一个键都不删**。猜一种形状去扣，等于在这里另造一张「枢纽的
// 解析器」，而那张表一旦与真实解析器分岔，表现是假合并——看不出来。
//
// # tool_result 的 content 为什么只扣两种写法
//
// 块数组的多块写法经枢纽按换行拼成一段（["a","b"] 折成 "a\nb"），是多对一：
// 扣掉之后 ["a","b"] 与 ["a\nb"] 的残差就相同了，而直通把它们原样发给上游，
// 上游看到的是两份不同的报文。所以只有「字符串」与「恰好一个只有 type+text
// 的块」这两种同形写法扣——它们与枢纽读出来的 Text 逐字相同。
//
// 已知边界：tool_result 不带 content 时枢纽把它折成字符串 "null"（见
// relay 的 messageFromRelaykit），于是「不带 content」与「content 是字面量
// "null"」在枢纽那边本来就同形；残差按「字符串就扣」处理，这两种写法因而
// 共用指纹。要分开就得为字面量 "null" 单开一条规则，而它只在那一个变量名
// 恰好是四个字母时才生效——不值一条规则。

// nativeResidueOf 从原始请求体里算出「没进 hashInput 的那部分」。
//
// 原文 Unmarshal 成 map、按入口协议扣掉已经进了 hashInput 的部分、剩下的
// 原样返回。哈希的对象是**规范化之后**的这一份（Unmarshal → 扣键 → Marshal，
// 见 HashRequest），于是 40.0 与 40 统一、嵌套 map 的键序由 json.Marshal 排定。
//
// 不用 json.Decoder 的 UseNumber：float64 往返对超过 2^53 的整数降精度，
// 两份只差这种大整数的原文会假同指纹。真正生效的残差字段不会是大整数，
// 而拿 UseNumber 换来的是「每个数字都要自己比大小」的一整层处理。
//
// body 为 nil 时返回 nil：直通之外（内部调用、跨协议）手上没有原文，残差为零值，
// 于是 hashInput 序列化出来的字节与加这个字段之前逐字节相同。
//
// 原文不是 JSON 对象时返回错误（见 HashRequest 的注释），不落回 nil 残差：
// 落回就是「两份不同原文共用指纹」的那条路。
func nativeResidueOf(protocol channel.Protocol, body []byte) (map[string]any, error) {
	if body == nil {
		// nil 残差是合法值，不是「出错了但没报」——直通之外（内部调用、
		// 跨协议）手上本来就没有原文。所以这里显式 nolint，而不是造一个
		// 哨兵错误让每个调用方多判一条永不可达的分支。
		return nil, nil //nolint:nilnil // 见上
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("解析原始请求体以计算幂等指纹: %w", err)
	}
	if len(m) == 0 {
		// JSON 的 null 解进 map 得到 nil 而不报错；它没有字段，也就没有残差。
		// 同上：空对象是合法的无残差，不是错误。
		return nil, nil //nolint:nilnil // 见上
	}

	// 顶层的扣除集合**按协议取，不取并集**：并集会把另一种协议的键也扣掉，
	// 而那些键在这一条入口上枢纽根本没读，扣了就是假合并。
	//
	// 下面没列出来的顶层键一律留残差，包括各家的「读不到」与「读得粗」两类：
	// 直通把原文原样发上游，枢纽读不到的键（max_tokens_to_sample、top_k、
	// cache_control、stop_sequences、metadata、seed）对上游可能是活的；
	// 枢纽读得粗的键（Responses 的 reasoning.budget_tokens）扣了就是多对一。
	// 同理，Responses 的顶层 reasoning_effort 与 chat 的 max_output_tokens
	// 也是「另一种协议的名字」，在这条入口上枢纽不读，留。
	switch protocol {
	case channel.ProtocolOpenAI:
		deductMessageArray(m, "messages")
		deductKeys(m,
			"model",                 // Model
			"max_tokens",            // MaxTokens
			"max_completion_tokens", // MaxTokens（同一个上界的另一个名字）
			"temperature",           // Temperature
			"top_p",                 // TopP
			"reasoning_effort",      // ReasoningEffort
			"tools",                 // Tools
			"stream",                // Stream
		)
	case channel.ProtocolAnthropic:
		deductMessageArray(m, "messages")
		deductAnthropicSystem(m)
		deductKeys(m,
			"model",       // Model
			"max_tokens",  // MaxTokens
			"temperature", // Temperature
			"top_p",       // TopP
			"tools",       // Tools
			"stream",      // Stream
		)
	case channel.ProtocolOpenAIResponses:
		deductResponsesInput(m)
		deductResponsesInstructions(m)
		deductResponsesReasoning(m)
		deductKeys(m,
			"model",             // Model
			"max_output_tokens", // MaxTokens
			"tools",             // Tools
			"stream",            // Stream
		)
	default:
		// 不认识的协议（内部调用、systemone、自定义适配器）没有读键表可依，
		// 整份原文留残差。留着只会让指纹更细，不会把两份报文合成一份。
	}

	if len(m) == 0 {
		// 原文的键全部进了 hashInput——没有残差可留，是正常的合取结果，
		// 不是「出错了但没报」。
		return nil, nil //nolint:nilnil // 见上
	}
	return m, nil
}

// deductKeys 把已经由 hashInput 的某个字段携带的顶层键从残差里删掉。
func deductKeys(m map[string]any, keys ...string) {
	for _, k := range keys {
		delete(m, k)
	}
}

// deductMessageArray 把 messages / input 数组扣到叶子。
//
// 逐条处理：能扣的键删掉，删完还剩任何键就整条留残差，删空则整条删除；
// 数组整批删空时，那个键本身也删。
//
// 非数组的取值原样留着：Responses 的 input 可以是字符串，那条写法由
// deductResponsesInput 整键处理；其余形态（对象、数字）不是我们认识的
// 消息序列，按「只认已知结构」留残差。
func deductMessageArray(m map[string]any, key string) {
	raw, ok := m[key]
	if !ok {
		return
	}
	items, ok := raw.([]any)
	if !ok {
		return
	}

	kept := make([]any, 0, len(items))
	for _, item := range items {
		msg, isMap := item.(map[string]any)
		if !isMap || !reduceMessage(msg) {
			kept = append(kept, item)
			continue
		}
		if len(msg) > 0 {
			kept = append(kept, msg)
		}
	}

	if len(kept) == 0 {
		delete(m, key)
		return
	}
	m[key] = kept
}

// reduceMessage 扣掉一条消息里已经进了 hashInput 的键。
//
// 返回 false 表示这条消息的形状不在读键表覆盖的范围内，**一个键都没删**
// （判定结构与删键是分开的两步，就是为了守住这个「要么全扣、要么全不动」）。
func reduceMessage(msg map[string]any) bool {
	content, hasContent := msg["content"]
	var blocks []map[string]any
	if hasContent {
		switch c := content.(type) {
		case string:
			// 字符串正文：整条落在 ContentBlock.Text 上。
		case []any:
			var ok bool
			blocks, ok = deductibleBlocks(c)
			if !ok {
				return false
			}
		default:
			return false
		}
	}

	delete(msg, "role") // Message.Role

	if !hasContent {
		return true
	}
	if _, isString := content.(string); isString {
		delete(msg, "content") // ContentBlock.Text
		return true
	}

	kept := make([]any, 0, len(blocks))
	for _, block := range blocks {
		reduceBlock(block)
		if len(block) > 0 {
			kept = append(kept, block)
		}
	}
	if len(kept) == 0 {
		delete(msg, "content")
	} else {
		msg["content"] = kept
	}
	return true
}

// deductibleBlocks 检查一个 content 数组是不是读键表认识的结构。
//
// 只认 type 在白名单里的块。其余任何一种（缺 type、未知类型、元素不是对象）
// 都让**整条消息**原样留在残差里——猜形状就是在这里造第二张解析表。
func deductibleBlocks(items []any) ([]map[string]any, bool) {
	out := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		block, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		typ, _ := block["type"].(string)
		switch typ {
		case "text", "input_text", "image", "input_image", "tool_use", "tool_result", "thinking":
			out = append(out, block)
		default:
			return nil, false
		}
	}
	return out, true
}

// reduceBlock 扣掉一个内容块里已经进了 hashInput 的键。
//
// 删完还剩的键（cache_control、thinking 的正文与签名、图片的 source.type、
// Responses 的块级 detail 之类）就是这块留进残差的部分。
func reduceBlock(block map[string]any) {
	typ, _ := block["type"].(string)
	switch typ {
	case "text", "input_text":
		delete(block, "type")
		delete(block, "text") // ContentBlock.Text
	case "image", "input_image":
		reduceImageBlock(block, typ)
	case "tool_use":
		delete(block, "type")
		delete(block, "id")    // ToolUseID
		delete(block, "name")  // ToolName
		delete(block, "input") // ToolInput
	case "tool_result":
		delete(block, "type")
		delete(block, "tool_use_id") // ToolUseID
		if content, ok := block["content"]; ok && toolResultTextDeductible(content) {
			delete(block, "content") // Text
		}
	case "thinking":
		// 枢纽把它整块丢掉（见 relay 的 messageFromRelaykit），直通却会把它
		// 原样发给上游：扣掉就等于宣称它不影响结果，而那只在枢纽那条路上成立。
	}
}

// reduceImageBlock 扣掉图片块里已经进了 ImageURL / ImageBase64 /
// ImageMediaType 的那几个键。
//
// 两种写法的地址来源不同：Anthropic 走 source 对象（url 或 data+media_type），
// Responses 走 image_url。指不到的那部分留着——source.type 决定枢纽怎么解释
// 另外两个键，而 hashInput 里没有它；Responses 的 image_url 若是 {url,detail}
// 的对象写法，detail 同样指不到任何字段，整个键都不扣。
func reduceImageBlock(block map[string]any, typ string) {
	if typ == "input_image" {
		if _, ok := block["image_url"].(string); !ok {
			return
		}
		delete(block, "image_url")
		delete(block, "type")
		return
	}

	source, ok := block["source"].(map[string]any)
	if !ok {
		return
	}
	delete(source, "url")        // ImageURL
	delete(source, "data")       // ImageBase64
	delete(source, "media_type") // ImageMediaType
	delete(block, "type")
	if len(source) == 0 {
		// 三个键都扣干净了：留一个空对象只是让普通图片请求的残差非空，
		// 而它不携带任何信息。
		delete(block, "source")
	}
}

// toolResultTextDeductible 判断 tool_result 的 content 能不能整个扣掉。
//
// 只有两种：字符串，以及「恰好一个只有 type+text 的块」。多块数组经枢纽按
// 换行拼接（多对一，见文件头读键表的说明），不扣；空数组与 null 经枢纽被
// 序列化成字面量文本，也不扣。
func toolResultTextDeductible(content any) bool {
	switch c := content.(type) {
	case string:
		return true
	case []any:
		if len(c) != 1 {
			return false
		}
		block, ok := c[0].(map[string]any)
		if !ok || len(block) != 2 {
			return false
		}
		typ, okType := block["type"].(string)
		_, okText := block["text"].(string)
		return okType && okText && typ == "text"
	}
	return false
}

// deductAnthropicSystem 扣掉顶层的 system。
//
// 只扣两种写法：字符串，以及「块数组、每块只有 type=text 与 text」。其它写法
// （块里还带 cache_control，或块类型不是 text）留在残差：前者的 cache_control
// 指不到 hashInput 的任何字段，后者枢纽根本不会搬进 Messages。
//
// 空数组不扣：它和「没有 system」在上游那边不是必然同一件事，留着只会让指纹
// 更细，而更细不会假合并。
func deductAnthropicSystem(m map[string]any) {
	v, ok := m["system"]
	if !ok {
		return
	}
	switch s := v.(type) {
	case string:
		delete(m, "system") // 折进 Messages 的首条
	case []any:
		if len(s) == 0 {
			return
		}
		for _, raw := range s {
			block, isMap := raw.(map[string]any)
			if !isMap || len(block) != 2 {
				return
			}
			typ, okType := block["type"].(string)
			_, okText := block["text"].(string)
			if !okType || !okText || typ != "text" {
				return
			}
		}
		delete(m, "system") // 块数组的文本经枢纽折成 Messages 的首条
	}
}

// deductResponsesInstructions 扣掉顶层的 instructions。
//
// 两种写法：字符串，以及「块数组、每块只有 type 与 text」。块数组经枢纽被
// 序列化成一段文本（不是拆成消息），所以它与字符串写法在 hashInput.Messages
// 里本来就不同——扣掉它不会让两种写法合并，只会让「同一个块数组、键序不同」
// 这种差异不进残差。
//
// 块里带别的键（cache_control 之类）就留在残差：那些键指不到 hashInput。
func deductResponsesInstructions(m map[string]any) {
	v, ok := m["instructions"]
	if !ok {
		return
	}
	switch s := v.(type) {
	case string:
		delete(m, "instructions")
	case []any:
		if len(s) == 0 {
			return
		}
		for _, raw := range s {
			block, isMap := raw.(map[string]any)
			if !isMap || len(block) != 2 {
				return
			}
			_, okType := block["type"].(string)
			_, okText := block["text"].(string)
			if !okType || !okText {
				return
			}
		}
		delete(m, "instructions")
	}
}

// deductResponsesReasoning 扣掉 reasoning 对象里的 effort 子键。
//
// **只扣 effort**：它落在 ReasoningEffort 上。同一个对象里的 budget_tokens
// 不在 hashInput 里（这条路径上枢纽也不拿它折算，见 relay 的
// parseResponsesRequest），扣掉会让两个不同预算的请求共用指纹。summary
// 同样指不到任何字段，留。
//
// effort 删空之后整个 reasoning 键也删：留下一个空对象只会让「写了
// reasoning:{effort:...}」与「没写」在残差上一样、却多出一个空壳。
func deductResponsesReasoning(m map[string]any) {
	reasoning, ok := m["reasoning"].(map[string]any)
	if !ok {
		return
	}
	delete(reasoning, "effort")
	if len(reasoning) == 0 {
		delete(m, "reasoning")
	}
}

// deductResponsesInput 扣掉顶层的 input。
//
// 字符串写法经枢纽折成一条 user 消息，整键落在 Messages 上；数组写法要扣到
// 叶子（见 deductMessageArray）。
func deductResponsesInput(m map[string]any) {
	if _, ok := m["input"].(string); ok {
		delete(m, "input")
		return
	}
	deductMessageArray(m, "input")
}
