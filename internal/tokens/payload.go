package tokens

// 本文件是**输入侧非正文部分**的估计量：工具定义、图片，以及历史消息里
// tool_use 块的参数。
//
// 正文那一侧的口径在 channel.Request.PromptText（只拼 Content[].Text），
// 本文件补的是它照不到、而上游**照样计入输入**的那几项。它们分成几个数、
// 各带各的依据，因为可信度差得远：工具定义与工具参数能序列化成字节、
// 按字节估，图片连分辨率都拿不到。
//
// # 只给预留用，方向一律偏保守
//
// 这几个数今天的唯一消费者是预留（internal/api 的 inputTokensOf →
// pipeline.Request.InputTokens → billing.Reserve）：请求发出前冻住一笔余额，
// 请求结束即释放。冻多了的代价是一次短暂的多冻；冻少了的代价是结算超过
// 上界（billing.Settlement.Capped）——差额由平台承担，账目仍然是平的，
// 只在事后对账时表现为「用量对不上金额」。两个方向不对称，所以这里一律取
// 偏保守的一侧。
//
// 它们**不是**给整体估算加的余量：按实际出现的那几项计，一项都没有时
// 它们都不参与，口径与没有本文件时逐字相同（有用例钉住）。

// structuredPayloadDensity 是结构化负载（JSON）按**字节**计的 token 密度下界。
//
// # 实测是什么样
//
// COUNTER.md 那张表说 JSON 与代码的真实密度是 0.26 ~ 0.29 token/字符。
// 把工具定义的几种典型形状喂给本包的估算式，得到的是 0.28 ~ 0.61 token/字节：
// 大多数形状本来就高于那条上沿（JSON 的引号、冒号、括号会切成大量短段，而
// 每段至少 1 token——BPE 就是这样），但**短键、枚举值多的形状会落到下沿**，
// 一个只有短键与 enum 的 schema 算出来是 0.28。
//
// 地板 0.35 抬的就是这一类。取 0.35 而不是 0.29：多出来的那两成覆盖的是我们
// 这一侧**看不见**的东西——各家在工具声明外面还有自己的字段名与包装（本仓的
// channel.Tool 只有 name / description / schema），而它们同样计入上游报的输入。
//
// 按字节而不是按字符：JSON 的键名与结构都是 ASCII，一个字节一个字符；中文
// 描述是三个字节一个字符，按字节算只会更高。
//
// 上面那两个密度都在 payload_test.go 里有夹具钉着，改估算式会被它们撞到。
const structuredPayloadDensity = 0.35

// ToolDefinitionReserveFrameTokens 是工具定义的**包装**在**预留**口径里记的
// token 数。
//
// # 为什么不复用 StreamEstimator 的那个常数
//
// 同一个「包装开销」在两条路上的代价方向**相反**，所以它们刻意不是同一个数：
//
//   - 流式中断那条路（`ToolDefinitionFrameTokens`）拿它去**向用户计费**（上游
//     断流、拿不到实测值时兜底）。估高了就是一笔凭空费用，还按工具条数放大，
//     所以那边取的是**下界**（8，理由写在那个常量的注释里）。
//   - 预留这条路拿它去**冻余额**。估低了的后果是预留低于结算、差额由平台承担，
//     而结算按预留封顶——这正是本条要修的那类问题。所以这边取**实测到的那个
//     数**（约 26，见 `ToolDefinitionFrameTokens` 的注释里量过的最小工具定义）。
//
// 两处共用一个数，必然让其中一边吃错方向。内容那一侧不受影响：两条路用的是
// 同一个 `payloadTokens` / `EstimatedTokens`，分叉只在这一项。
const ToolDefinitionReserveFrameTokens = 26

// ToolDefinitionInputTokens 估算一个工具定义计入输入侧的 token 数。
//
// definitionJSON 是工具定义**本体**序列化后的 JSON（name / description /
// schema 拼成的那个对象）。不要在入口各拼一份：四条入口的写法不同，而链路上
// 已经统一到 channel.Tool，这里要的是统一之后的那一份。
//
// 结果 = 每条的包装开销（ToolDefinitionReserveFrameTokens，与流式中断那条路
// **不是**同一个数，理由见那个常量）+ 内容（启发式估算与按字节地板里的较大者，
// 见 payloadTokens）。
func ToolDefinitionInputTokens(model string, definitionJSON []byte) int64 {
	n := int64(ToolDefinitionReserveFrameTokens)
	if len(definitionJSON) == 0 {
		// 没有内容也留着包装：上游照样要为一条（内容为空的）工具声明付框架开销。
		return n
	}
	return n + payloadTokens(model, string(definitionJSON))
}

// payloadTokens 估一段**结构化负载**的 token 数：取启发式估算与按字节地板里的
// 较大者。
//
// 两个都留着而不是只用地板：地板管的是短键、枚举值多的那一类（见
// structuredPayloadDensity 里的实测），而不带地板的启发式在长描述那类形状上
// 更高——取较大者只会往上抬，不会把任何一侧压低。非 ASCII 的内容（中文描述）
// 在这一步通常落在两者中的较高一侧，方向同样是保守的。
func payloadTokens(model, s string) int64 {
	n := EstimatedTokens(model, s)
	if floor := int64(ceil(structuredPayloadDensity * float64(len(s)))); floor > n {
		return floor
	}
	return n
}

// ToolUseReserveFrameTokens 是历史消息里**每一块** tool_use 在**预留**口径里
// 记的包装开销。
//
// # 为什么不复用流式中断那条路的常数
//
// 同一个「工具调用的包装」在两条路上的代价方向**相反**，所以它们刻意不是
// 同一个数（这一条是 #451 在工具定义上已经踩过的）：
//
//   - 流式中断那条路（`ToolCallFrameTokens`，= 6）拿它去**向用户计费**：那是
//     上游断流、拿不到实测值时按已收到的分片兜底。估高了就是一笔凭空费用，
//     还按工具调用条数放大，所以那边取的是**下界**。
//   - 预留这条路拿它去**冻余额**。估低了的后果是预留低于结算、差额由平台
//     承担，而结算按预留封顶——这正是本项要修的那类问题。所以这边取的是
//     **上游那份形状比我们这份多出来的那一截**（下一节量了），而不是那边那个下界。
//
// 内容那一侧不受影响：两条路都把参数本身当文本计，分叉只在这一项。
//
// # 12 这个数怎么来的
//
// 实测（o200k_base）：把同一段工具参数按两种形状分词——一种是我们序列化出来
// 的那份（id / name / input 三个字段），另一种是上游实际收到的那份
// （OpenAI 把参数**重新编码成转义字符串**再套一层 function 对象）——后者每块
// 多出 4 ~ 7 token（短参数 4、嵌套得深的参数 7）。这一项就是那层差。
//
// 取 12 而不是 7：上面的样本只有五种形状，而这一层是随参数内容变的——转义
// 是逐字符加的，参数里的引号越多、这一层越厚。贴着实测的那一头走，换个形状就
// 会落到它下面；而这里是预留侧，估低的代价由平台承担，留一倍余量便宜得多。
//
// 它**不是**消息框架（`MessageFrameTokens`）那样的每条消息开销：一块
// tool_use 记一次，多轮工具调用的历史里按块数走。
const ToolUseReserveFrameTokens = 12

// ToolUseInputTokens 估算历史消息里**一块** tool_use 计入输入侧的 token 数。
//
// blockJSON 是这一块序列化后的 JSON（id / name / input 拼成的那个对象，见
// internal/api 的 toolUseBlockJSON）。不要在入口各拼一份：四条入口的写法不同，
// 而链路上已经统一到 channel.ContentBlock，这里要的是统一之后的那一份。
//
// 结果 = 每块的包装开销（ToolUseReserveFrameTokens，与流式中断那条路的
// ToolCallFrameTokens **不是**同一个数，理由见那个常量）+ 内容（启发式估算与
// 按字节地板里的较大者，见 payloadTokens）。
//
// 上游确实为这一块付费，且它**随轮数增长**：多轮工具调用的会话里，每一轮调用的
// 参数都会随后续请求重复出现在历史里，不是一个常数。
func ToolUseInputTokens(model string, blockJSON []byte) int64 {
	n := int64(ToolUseReserveFrameTokens)
	if len(blockJSON) == 0 {
		// 没有内容也留着包装：一块（参数为空的）工具调用照样有 id 与 name，
		// 上游也为它们计 token。
		return n
	}
	return n + payloadTokens(model, string(blockJSON))
}

// ImageInputTokens 是**每一张**图片计入输入侧的 token 数。
//
// # 为什么是一个常数，而不是按分辨率算出来
//
// 图片在链路上有两种形态，两种都拿不到分辨率：URL 形态我们只看得见一个链接；
// base64 形态拿得到字节数，但字节数与分辨率的对应关系很差——同分辨率的 PNG
// 截图与 JPEG 照片能差十倍，而两张尺寸差四倍的 JPEG 也可能同样大。既然分辨率
// 无从得知，就取一个**常见尺寸的下沿**，而不是造一张看起来更细、实际更不可靠
// 的分档表。
//
// # 1400 这个数怎么来的
//
// 两家公开口径在 1024×1024 上的值：
//
//	Anthropic  (w×h)/750      = 1024×1024/750 ≈ 1398
//	OpenAI     85 + 170×tile  = 85 + 4×170    =  765
//
// 取较大者再向上取整到 1400。取较大者而不是按模型族分开，理由与
// billing.Reserve 输入侧取最贵单价相同：预留发生在**选路之前**，那时还不知道
// 会选中哪一家，上界要与「这次走谁」无关。
//
// # 已知边界，别当成 bug
//
// 它既不是任何图片的上界也不是下界，它是常见尺寸（约 1024×1024）的位置：
//
//   - **比 1024×1024 更大的图片仍然会低估**（Anthropic 在 1568×1568 上要
//     3277）。分辨率拿不到就只能到这里——这一档仍是「少收」，与本条要修的
//     问题同向，只是小得多，且不再是「一个都没算」。
//   - **图标级的小图会多冻一点**。方向可接受：多冻的那部分只在请求期间存在，
//     结束即释放。
const ImageInputTokens = 1400
