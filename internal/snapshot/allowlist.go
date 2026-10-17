package snapshot

import (
	"slices"
	"sort"
	"strings"
)

// SkeletonVersion 是当前骨架格式的版本号，落进 request_snapshots.skeleton_version。
//
// **什么时候该递增**：凡是改变了「某个字段在骨架里会变成什么样」的改动都要加一，
// 包括三类——
//
//   - 往允许表里加字段、改字段的处理方式、删字段；
//   - 改变占位描述符的形状（`_t` 的取值、chars/bytes 的口径）；
//   - 改变骨架的整体布局（某一层从哪里开始按默认脱敏处理）。
//
// 不递增的话，库里的旧快照会与当前代码产出的快照混在一列数据里，
// 半年后没人知道某个字段当时为什么是空的、或者为什么口径不同。
// 版本号是旧快照唯一的解释依据，所以宁可多增一次。
//
// 只改注释、改错误信息这类不影响输出的改动不需要递增。
const SkeletonVersion = 1

// Root 是一次脱敏的起点：请求侧还是响应侧。
//
// 取值与 request_snapshots.direction 的 CHECK 约束一致，让调用方可以直接落库，
// 不必再做一次映射——多一次映射就多一处两边取值写歪的机会。
type Root string

// 两个方向的取值。
const (
	// RootRequest 是客户端发来的请求（统一格式，见 channel.Request）。
	RootRequest Root = "request"
	// RootResponse 是上游返回的响应（统一格式，见 channel.Response）。
	RootResponse Root = "response"
)

// rootValues 是全部已知方向的取值。
//
// # 为什么要单独声明一份
//
// 原来 valid() 里写的是 `r == RootRequest || r == RootResponse`——一个内联比较，
// 没法被遍历。于是「允许表里多了一个方向、而 valid() 没跟上」这件事在测试里
// 没有位置：用例只能拿同一组字面量去测同一组字面量，改哪一边都自洽。
//
// 声明成一份清单之后，valid() 认的取值可以被逐条走一遍，与 allowlists
// （哪个方向采哪些字段）互相比对——两处都可以改，只改一处就会挂。
// 见 TestRootValuesMatchSchema。
//
// 它与 allowlists 是两个问题：这里答「哪些取值是已知方向」，表答「每个方向
// 采哪些字段」。合成一份的话，两道判据就退化成一条恒真的自证。
var rootValues = []Root{
	RootRequest,
	RootResponse,
}

// valid 判断方向是否已知。
//
// 目前只有测试在调它。**New 不用它**：那里查的是「有没有这个方向的表」
// （`allowlists[root]`），比「取值是不是已知方向之一」更强——加了取值
// 却忘了写表时，valid 会说是，而查表会报错，后者才是想要的结论。
func (r Root) valid() bool {
	return slices.Contains(rootValues, r)
}

// fieldKind 是一个白名单条目告诉脱敏器怎么处理某个字段。
//
// 条目是**数据**而不是散在代码里的分支：上线前的检查项要求这张表能被测试直接
// 遍历（每个采集字段一条测试，断言「真实值落库」或「已脱敏」），
// 而写在 if 里的判断没法被遍历，也就会漏测。
//
// 几档「放行」的分别在于**取值的来源**。来自上游或客户端的字符串永远不能原样
// 放行：白名单条目说明的是「我们期望这里放什么」，而不是「它一定放的是那个」。
// 所以除 allowRaw（只放行布尔与数字）以外，放行字符串的档位都自带取值检查，
// 检查不过就按内容处理——这是「宁可少留一个字段」在这张表里的具体形态。
type fieldKind uint8

const (
	// allowBool 是开关类字段的放行档：只放行布尔。
	allowBool fieldKind = iota
	// allowUint 是计数与长度类字段的放行档：只放行非负整数。
	//
	// token 数、消息条数、状态码都不是负数。遇到负数说明这一格收到的
	// 不是本来的那个东西，按内容处理而不是照样写进库。
	allowUint
	// allowFloat 包括采样参数与偏置：只放行有限数字。
	//
	// temperature、top_p、top_k 的取值可正可负，但不会是 NaN/Inf
	//（那两个值序列化出来不是合法 JSON，会让整条骨架在落库那一步失败）。
	// 偏置字典走 allowNumberMap，因为它的键名有复现价值。
	allowFloat
	// allowEnumWord 放行**看起来像枚举词**的字符串取值，其余按内容处理。
	//
	// 用在 model、finish_reason、error.code 这类位置上。规则说它们可以保留
	// 真实值，前提是「它们是枚举」；把前提写成代码里的检查，
	// 才能在前提不成立时（上游往这个位置回显了一段用户输入）挡住它。
	allowEnumWord
	// allowNumberMap 是「键名保留、值必须是数字」的字典。
	//
	// 用在 logit_bias（键是 token id，值是偏置）与 usage.unit_counts
	//（键是单位名，值是数量）上：两者都不是内容，而键名本身有复现价值。
	// 值一旦不是数字，说明实际结构与我们以为了的不一样，换描述符。
	allowNumberMap
	// keepShape 放行结构：容器照常递归，子字段没有自己的条目时按
	// 「键名未知」的规则处理（键名与类型留下，取值一律换成描述符）。
	keepShape
	// hideText 替换为文本占位（`{"_t":"text","chars":N}`）。
	// 用于可承载任意用户字符串的字段：工具描述、stop 序列、错误正文。
	hideText
	// hideValue 替换为通用脱敏记号（`{"_t":"string","redacted":true}`），
	// 不留长度。用于姓名、标识符这类连长度都算内容的字段。
	hideValue
	// hideSubtree 整棵子树替换为记号。用于「内部结构本身是内容」的字段，
	// 例如上游原样透传的思考签名、逐 token 的 logprobs。
	hideSubtree
	// hideKeysKeepShape 保留键名与嵌套结构，标量取值一律换成
	// 「类型 + 长度」描述符。
	//
	// 保留参数键名是有意的：schema 校验类错误（漏传必填项、类型不符）
	// 必须能看到键名才能复现。代价是键名本身会落库，所以这一条是**显式策略**——
	// 想收紧的接入方把表里的 hideKeysKeepShape 换成 hideSubtree 即可，
	// 代价是那类错误变得难以复现。
	hideKeysKeepShape
	// toolRef 把工具名映射为请求内稳定的引用（T1、T2）。
	//
	// 工具名是用户最私有的一类信息，往往直接暴露业务逻辑；而引用号让
	// tool_use 与 tool_result 的对应关系不丢——这是复现多轮工具调用错误的关键。
	// 编号只在一次调用内稳定，跨请求不关联。
	toolRef
	// toolRefID 处理工具调用 ID：本次请求里出现过的名字都换成同一个引用号。
	//
	// 工具调用 id 由上游生成，本身不是用户内容；引用化之后
	// 「tool_result 引用了一个不存在的 tool_use」这类错误才能复现。
	toolRefID
	// hideStringList 处理字符串数组：每个元素只留长度，单个字符串同样处理。
	hideStringList
	// keepEnumList 处理取值是枚举词的字符串数组（例如 modalities）：
	// 字符串元素放行，其余元素按未知取值处理。
	keepEnumList
	// roleField 处理消息角色：已知的角色词原样留，其余按工具引用处理
	//（没有名字时，工具名会被塞进 `role` 位置）。逻辑见 walker.role。
	roleField
	// blockListField 处理内容块序列：接住块数组、纯字符串、单个对象三种线格式，
	// 逐块分派。逻辑见 walker.blocks。
	blockListField
	// schemaField 处理 JSON Schema：形状（键名、类型、required）保留，
	// 携带具体取值的分支（enum、const、default）脱敏。逻辑见 walker.schema。
	schemaField
	// headersField 处理请求头：只留头名的短哈希，deniedHeaders 里的头连哈希都不进。
	// 逻辑见 walker.headers。
	headersField
	// responseHeadersField 同 headersField，另外放行额度监控要用的响应头（上游生成的数字与时间戳）。
	responseHeadersField
	// extraField 处理上游特有参数：键名不留（配置方自定的名字往往就是业务名词），
	// 只留每个取值的形状。逻辑见 walker.extra。
	extraField
	// deny 表示永不采集。取值与长度都不记——「这个请求带了 authorization」
	// 本身也是一条信息，而它不该落进快照。
	deny
)

// fieldRule 是允许表里的一行。
type fieldRule struct {
	// Path 是这个字段在**入口协议**里的路径，`[]` 表示数组元素，
	// 例如 `messages[].content[].type`。只写键名不写层级的条目靠键名匹配生效，
	// 见 fieldTable.lookup。
	Path string
	// Kind 是处理方式。
	Kind fieldKind
	// UpstreamValue 为 true 表示这个字段的取值来自上游或客户端，
	// 而不是本项目自己写进去的。
	//
	// 这个标记不是文档：测试会给每一个 UpstreamValue 的放行条目喂一段敌对取值
	//（含空格、含勾选标记的字符串、超长串），断言它不会落到骨架里。
	// 打进骨架的值里，来自上游的那些是唯一无法在代码里穷举的，
	// 所以它们需要一条能自动跑的断言。
	UpstreamValue bool
	// Note 说明这一行为什么放行、为什么脱敏。空的 Note 会被构造期检查拦下：
	// 这张表的每一行都要能解释自己为什么在那里。
	Note string
}

// requestRules 是请求侧的允许表，覆盖 OpenAI 与 Anthropic 两套线格式。
//
// 这张表是包的核心：表里没有的一律脱敏，所以少写一行是安全的（少留一个字段），
// 多写一行才是风险。新增条目必须在 Note 里说清「复现这个错误为什么需要它」。
var requestRules = []fieldRule{
	{Path: "model", Kind: allowEnumWord, UpstreamValue: true,
		Note: "请求的模型名是复现的入口：选错模型、别名写错都靠它"},
	{Path: "stream", Kind: allowBool,
		Note: "流式与非流式的故障现场完全不同"},
	{Path: "stream_options", Kind: keepShape,
		Note: "开关集合。"},
	{Path: "stream_options.include_usage", Kind: allowUint,
		Note: "决定最后一片有没有 usage，缺了它是常见故障"},
	{Path: "max_tokens", Kind: allowUint,
		Note: "截断类错误靠它判断"},
	{Path: "max_completion_tokens", Kind: allowUint,
		Note: "同上，新协议里的别名"},
	{Path: "temperature", Kind: allowFloat,
		Note: "采样参数，取值本身就是复现条件"},
	{Path: "top_p", Kind: allowFloat,
		Note: "采样参数。"},
	{Path: "top_k", Kind: allowFloat,
		Note: "采样参数，Anthropic 侧才有"},
	{Path: "frequency_penalty", Kind: allowFloat,
		Note: "采样参数。"},
	{Path: "presence_penalty", Kind: allowFloat,
		Note: "采样参数。"},
	{Path: "seed", Kind: allowUint,
		Note: "复现随机性问题需要它"},
	{Path: "n", Kind: allowUint,
		Note: "候选数量；返回条数与它对不上时要看这个值"},
	{Path: "logit_bias", Kind: allowNumberMap, UpstreamValue: true,
		Note: "键是 token id，值是数字偏置，两者都不是内容"},
	{Path: "logprobs", Kind: allowBool,
		Note: "开关。"},
	{Path: "top_logprobs", Kind: allowBool,
		Note: "与它配套的数量。"},
	{Path: "response_format", Kind: keepShape,
		Note: "结构约束的形状是复现解析失败的关键"},
	{Path: "response_format.type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "取值是枚举（json_object、json_schema 之类）"},
	{Path: "reasoning_effort", Kind: allowEnumWord, UpstreamValue: true,
		Note: "推理深度直接决定上游行为"},
	{Path: "thinking", Kind: keepShape,
		Note: "Anthropic 的推理配置。"},
	{Path: "thinking.type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举：enabled / disabled"},
	{Path: "thinking.budget_tokens", Kind: allowUint,
		Note: "预算不足报错时要看它"},
	{Path: "modalities", Kind: keepEnumList, UpstreamValue: true,
		Note: "能力开关，取值是枚举词"},
	{Path: "service_tier", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举词，决定上游走哪档资源"},
	{Path: "parallel_tool_calls", Kind: allowBool,
		Note: "开关，与工具调用的分片行为有关"},
	{Path: "tool_choice", Kind: keepShape,
		Note: "选了哪类工具是复现工具调用错误的一环"},
	{Path: "tool_choice.type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举。"},
	{Path: "tool_choice.name", Kind: toolRef,
		Note: "工具名一律引用化，与 tools 共用同一套编号"},
	{Path: "user", Kind: hideValue,
		Note: "终端用户标识属个人信息"},
	{Path: "metadata", Kind: hideSubtree,
		Note: "客户端自定义元数据，内部结构不受控"},

	{Path: "messages", Kind: keepShape,
		Note: "消息序列是复现的主体，逐条走子规则"},
	{Path: "messages[].role", Kind: roleField, UpstreamValue: true,
		Note: "角色序列是形状；没有名字时工具名会被塞进这个位置，所以单独处理"},
	{Path: "messages[].name", Kind: hideValue, UpstreamValue: true,
		Note: "参与者名属个人信息，且常直接是业务名词"},
	{Path: "messages[].tool_call_id", Kind: toolRefID, UpstreamValue: true,
		Note: "对上引用才能复现工具往返"},
	{Path: "messages[].content", Kind: blockListField, UpstreamValue: true,
		Note: "内容块序列：类型与顺序保留，各段内容按块类型处理"},
	{Path: "messages[].content[].type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "块类型不是内容，却是复现时要看的第一件事"},
	{Path: "messages[].content[].thinking", Kind: hideText, UpstreamValue: true,
		Note: "模型思考内容"},
	{Path: "messages[].content[].signature", Kind: hideSubtree, UpstreamValue: true,
		Note: "思考签名的内部结构不可控"},
	{Path: "messages[].content[].tool_use_id", Kind: toolRefID, UpstreamValue: true,
		Note: "工具往返的关联关系，引用化后保留"},
	{Path: "messages[].content[].id", Kind: toolRefID, UpstreamValue: true,
		Note: "工具调用的 id；它与 tool_use_id 是同一个东西，共用编号"},
	{Path: "messages[].content[].name", Kind: toolRef, UpstreamValue: true,
		Note: "工具名引用化"},
	{Path: "messages[].content[].input", Kind: hideKeysKeepShape, UpstreamValue: true,
		Note: "工具参数：键名保留（schema 错误必须看到键名），值一律换描述符"},
	{Path: "messages[].content[].content", Kind: hideKeysKeepShape, UpstreamValue: true,
		Note: "工具结果的内容形状，与参数同理"},
	{Path: "messages[].content[].is_error", Kind: allowBool, UpstreamValue: true,
		Note: "布尔量，不是内容"},

	{Path: "tools", Kind: keepShape,
		Note: "工具定义序列，逐个走子规则"},
	{Path: "tools[].type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举。"},
	{Path: "tools[].function", Kind: keepShape,
		Note: "OpenAI 把工具定义包在 function 里。"},
	{Path: "tools[].function.name", Kind: toolRef,
		Note: "工具名引用化"},
	{Path: "tools[].function.description", Kind: hideText,
		Note: "工具描述常直接写出业务逻辑，只留长度"},
	{Path: "tools[].function.parameters", Kind: schemaField,
		Note: "参数 schema：形状（参数名、类型、required）保留，enum 取值脱敏"},
	{Path: "tools[].name", Kind: toolRef,
		Note: "Anthropic 侧的工具名"},
	{Path: "tools[].description", Kind: hideText,
		Note: "同上，只留长度"},
	{Path: "tools[].input_schema", Kind: schemaField,
		Note: "Anthropic 侧的参数 schema"},
	{Path: "tools[].strict", Kind: allowBool, UpstreamValue: true,
		Note: "布尔量，但它会改变工具调用的校验行为"},

	// 头整体走「只留头名哈希」的规则：头名本身可能就是自定义的私有记号。
	{Path: "headers", Kind: headersField,
		Note: "请求头只留哈希；Authorization 一类在 deniedHeaders 里连哈希都不进"},

	{Path: "extra", Kind: extraField,
		Note: "本项目扩展出来的参数容器：键名由配置方自定，只留每个取值的形状"},
	{Path: "extra.extra_body", Kind: hideKeysKeepShape, UpstreamValue: true,
		Note: "客户端塞进来的扩展体，形状（键名）留、值不留"},
	{Path: "extra.safety_identifier", Kind: hideValue, UpstreamValue: true,
		Note: "标识符，属个人信息"},
	{Path: "extra.prompt_cache_key", Kind: hideValue, UpstreamValue: true,
		Note: "缓存键由客户端生成，可能含用户内容的摘要"},

	// stop 是规范里点名的例外：字段本身有形状（可能是字符串也可能是一串），
	// 而取值可承载任意用户字符串，所以两者都不留内容，只留长度。
	{Path: "stop", Kind: hideStringList, UpstreamValue: true,
		Note: "可承载任意用户字符串，逐个只留长度（字符串形态同样只留长度）"},
	{Path: "stop_sequences", Kind: hideStringList, UpstreamValue: true,
		Note: "同上，Anthropic 侧的写法"},
}

// responseRules 是响应侧的允许表。
//
// 响应侧最容易出事的是错误正文：上游回显用户输入时正文里带着原话，
// 所以 error.message 整体脱敏，只留 type/code/param 这类枚举字段。
var responseRules = []fieldRule{
	{Path: "id", Kind: hideValue, UpstreamValue: true,
		Note: "上游的内部标识，不是内容但也没有复现价值"},
	{Path: "object", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举词（chat.completion 之类），用来确认拿到的不是另一种对象"},
	{Path: "model", Kind: allowEnumWord, UpstreamValue: true,
		Note: "上游实际用的模型，用来发现上游偷换模型"},
	{Path: "upstream_model", Kind: allowEnumWord, UpstreamValue: true,
		Note: "同上，本项目归一化后的名字"},
	{Path: "created", Kind: allowUint, UpstreamValue: true,
		Note: "时间戳，与上游日志对齐用"},
	{Path: "system_fingerprint", Kind: allowEnumWord, UpstreamValue: true,
		Note: "上游后端的版本指纹，排查「同一请求结果突变」全靠它"},

	{Path: "status", Kind: allowUint, UpstreamValue: true,
		Note: "上游 HTTP 状态码，复现的入口"},
	{Path: "status_code", Kind: allowUint, UpstreamValue: true,
		Note: "同上，另一种叫法"},
	{Path: "upstream_status", Kind: allowUint, UpstreamValue: true,
		Note: "同上，本项目归一化后的名字"},

	{Path: "usage", Kind: keepShape,
		Note: "这是计费依据，必须真实"},
	{Path: "usage.prompt_tokens", Kind: allowUint,
		Note: "计费依据。"},
	{Path: "usage.completion_tokens", Kind: allowUint,
		Note: "计费依据。"},
	{Path: "usage.total_tokens", Kind: allowUint,
		Note: "计费依据。"},
	{Path: "usage.input_tokens", Kind: allowUint,
		Note: "Anthropic 侧的输入计数"},
	{Path: "usage.output_tokens", Kind: allowUint,
		Note: "Anthropic 侧的输出计数"},
	{Path: "usage.cache_read_input_tokens", Kind: allowUint,
		Note: "缓存明细，计费口径里含它"},
	{Path: "usage.cache_creation_input_tokens", Kind: allowUint,
		Note: "缓存明细。"},
	{Path: "usage.completion_tokens_details", Kind: keepShape,
		Note: "输出侧的明细。"},
	{Path: "usage.completion_tokens_details.reasoning_tokens", Kind: allowUint,
		Note: "推理 token 的计费依据。"},
	{Path: "usage.prompt_tokens_details", Kind: keepShape,
		Note: "输入侧的明细。"},
	{Path: "usage.prompt_tokens_details.cached_tokens", Kind: allowUint,
		Note: "缓存命中数，计费要用"},
	{Path: "usage.prompt_tokens_details.cache_creation_tokens", Kind: allowUint,
		Note: "缓存写入数，计费要用"},
	{Path: "usage.server_tool_use", Kind: allowNumberMap, UpstreamValue: true,
		Note: "服务端工具的调用计数，键是工具类别、值是次数"},
	{Path: "usage.unit_counts", Kind: allowNumberMap, UpstreamValue: true,
		Note: "非 token 计量（搜索次数、图片张数），键是单位名、值是数量"},
	{Path: "usage.service_tier", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举词，与用量口径有关"},

	{Path: "choices", Kind: keepShape,
		Note: "候选序列，逐个走子规则"},
	{Path: "choices[].index", Kind: allowUint, UpstreamValue: true,
		Note: "下标，与请求里的 n 对照"},
	{Path: "choices[].finish_reason", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举，决定「为什么停」"},
	{Path: "choices[].stop_reason", Kind: allowEnumWord, UpstreamValue: true,
		Note: "同上，Anthropic 侧叫法"},
	{Path: "choices[].stop_sequence", Kind: hideText, UpstreamValue: true,
		Note: "命中的停止序列是客户端传进来的字符串，只留长度"},
	{Path: "choices[].message", Kind: keepShape,
		Note: "归一化后的消息体，走消息子规则"},
	{Path: "choices[].delta", Kind: keepShape,
		Note: "流式增量，走消息子规则"},
	{Path: "choices[].logprobs", Kind: hideSubtree, UpstreamValue: true,
		Note: "logprobs 里嵌着逐 token 的原文，整块脱敏"},

	{Path: "content", Kind: blockListField, UpstreamValue: true,
		Note: "Anthropic 的非流式响应把内容块放在这一层"},
	{Path: "content[].type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "块类型。"},
	{Path: "content[].thinking", Kind: hideText, UpstreamValue: true,
		Note: "模型思考内容。"},
	{Path: "content[].signature", Kind: hideSubtree, UpstreamValue: true,
		Note: "签名内部结构不可控。"},
	{Path: "content[].id", Kind: toolRefID, UpstreamValue: true,
		Note: "工具调用 id，引用化后保留"},
	{Path: "content[].name", Kind: toolRef, UpstreamValue: true,
		Note: "工具名引用化。"},
	{Path: "content[].input", Kind: hideKeysKeepShape, UpstreamValue: true,
		Note: "工具参数：键名保留，值换描述符"},
	{Path: "content[].content", Kind: hideKeysKeepShape, UpstreamValue: true,
		Note: "工具结果的内容形状。"},
	{Path: "content[].is_error", Kind: allowBool, UpstreamValue: true,
		Note: "布尔量。"},

	{Path: "role", Kind: roleField, UpstreamValue: true,
		Note: "归一化后的消息角色"},
	{Path: "message", Kind: keepShape,
		Note: "归一化后的消息体。"},
	{Path: "message.role", Kind: roleField, UpstreamValue: true,
		Note: "角色序列。"},
	{Path: "message.content", Kind: blockListField, UpstreamValue: true,
		Note: "内容块序列。"},
	{Path: "message.content[].type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "块类型。"},
	{Path: "message.content[].thinking", Kind: hideText, UpstreamValue: true,
		Note: "模型思考内容。"},
	{Path: "message.content[].signature", Kind: hideSubtree, UpstreamValue: true,
		Note: "签名内部结构不可控。"},
	{Path: "message.content[].tool_use_id", Kind: toolRefID, UpstreamValue: true,
		Note: "工具往返关联。"},
	{Path: "message.content[].id", Kind: toolRefID, UpstreamValue: true,
		Note: "工具调用 id。"},
	{Path: "message.content[].name", Kind: toolRef, UpstreamValue: true,
		Note: "工具名引用化。"},
	{Path: "message.content[].input", Kind: hideKeysKeepShape, UpstreamValue: true,
		Note: "工具参数。"},
	{Path: "message.content[].content", Kind: hideKeysKeepShape, UpstreamValue: true,
		Note: "工具结果形状。"},
	{Path: "message.content[].is_error", Kind: allowBool, UpstreamValue: true,
		Note: "布尔量。"},

	{Path: "finish_reason", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举。"},
	{Path: "stop_reason", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举。"},
	{Path: "kind", Kind: allowEnumWord, UpstreamValue: true,
		Note: "流式事件的类型，枚举。"},

	// 错误对象：正文整体脱敏，只留枚举字段。
	{Path: "error", Kind: keepShape,
		Note: "错误对象的骨架，只放行枚举与长度"},
	{Path: "error.type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举，复现时首先看它"},
	{Path: "error.code", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举，比 type 精确"},
	{Path: "error.param", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举，指出是哪个参数错了——它本身不是用户内容"},
	{Path: "error.message", Kind: hideText, UpstreamValue: true,
		Note: "上游错误正文经常回显用户输入，整体脱敏只留长度"},
	{Path: "error.status", Kind: allowUint, UpstreamValue: true,
		Note: "状态码。"},
	{Path: "type", Kind: allowEnumWord, UpstreamValue: true,
		Note: "Anthropic 的错误类型，枚举。"},
	{Path: "code", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举。"},
	{Path: "param", Kind: allowEnumWord, UpstreamValue: true,
		Note: "枚举。"},
	{Path: "message_text", Kind: hideText, UpstreamValue: true,
		Note: "错误正文的另一种叫法，同样按内容处理"},

	// 响应头按允许表取：额度监控要的是 x-ratelimit-* 这一组，其余一律不采——
	// 自定义头的头名也可能带私有信息。
	{Path: "headers", Kind: responseHeadersField,
		Note: "只保留额度监控需要的响应头，其余不采"},
}

// allowlists 把方向映射到允许表。测试直接遍历它；新增方向时也必须同时加表——
// 没有表的那个方向会在 New 里被显式拒绝，而不是落到某张默认表上。
var allowlists = map[Root][]fieldRule{
	RootRequest:  requestRules,
	RootResponse: responseRules,
}

// FieldRule 是允许表在包外可见的一条规则。
//
// 导出这份视图是为了让「这张表里有哪些字段」能在包外被检查与展示——
// 上线前的检查项要求每个采集字段都有一条测试，而那条测试与将来的审计命令
// 都需要遍历它。包里用的是带档位常量的内部形态（fieldRule），
// 这里给的是它的可读投影：档位换成名字，别的原样。
type FieldRule struct {
	// Path 是这个字段在入口协议里的路径，`[]` 表示数组元素。
	Path string
	// Kind 是处理方式的名字（见各档位的 String 实现）。
	Kind string
	// UpstreamValue 为 true 表示这一格的取值来自上游或客户端。
	UpstreamValue bool
	// Note 说明这一行为什么放行、为什么脱敏。
	Note string
}

// Allowlist 返回某个方向的允许表。
//
// 返回副本：调用方改了结果不该影响脱敏行为，脱敏器也不该有可被外部篡改的状态。
func Allowlist(root Root) []FieldRule {
	src, ok := allowlists[root]
	if !ok {
		return nil
	}
	out := make([]FieldRule, len(src))
	for i, rule := range src {
		out[i] = FieldRule{
			Path:          rule.Path,
			Kind:          rule.Kind.String(),
			UpstreamValue: rule.UpstreamValue,
			Note:          rule.Note,
		}
	}
	return out
}

// String 给出处理方式的名字，用于错误信息与包外的展示。
func (k fieldKind) String() string {
	switch k {
	case allowBool:
		return "allow-bool"
	case allowUint:
		return "allow-uint"
	case allowFloat:
		return "allow-float"
	case allowEnumWord:
		return "allow-enum-word"
	case allowNumberMap:
		return "allow-number-map"
	case keepShape:
		return "keep-shape"
	case hideText:
		return "hide-text"
	case hideValue:
		return "hide-value"
	case hideSubtree:
		return "hide-subtree"
	case hideKeysKeepShape:
		return "hide-keys-keep-shape"
	case toolRef:
		return "tool-ref"
	case toolRefID:
		return "tool-ref-id"
	case hideStringList:
		return "hide-string-list"
	case keepEnumList:
		return "keep-enum-list"
	case roleField:
		return "role-field"
	case blockListField:
		return "block-list-field"
	case schemaField:
		return "schema-field"
	case headersField:
		return "headers-field"
	case responseHeadersField:
		return "response-headers-field"
	case extraField:
		return "extra-field"
	case deny:
		return "deny"
	default:
		return "unknown"
	}
}

// Roots 返回全部已知方向，顺序固定。
//
// 它取的是允许表的键（哪些方向有字段规则）；「哪些取值算已知方向」声明在
// rootValues 里。两者由 TestRootValuesMatchSchema 互相比对——只改一处会挂。
func Roots() []Root {
	out := make([]Root, 0, len(allowlists))
	for r := range allowlists {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// deniesKeep 判断一个条目的处理方式是否属于「永不采集」。
func deniesKeep(kind fieldKind) bool {
	return kind == deny
}

// isAllowKind 判断一个处理方式是否会把取值原样放进骨架。
//
// 测试用它筛出「需要顶住敌对取值」的条目：凡是可能放行真实取值的地方，
// 都要能被一段内容闯进去而不留痕。
func isAllowKind(kind fieldKind) bool {
	switch kind {
	case allowBool, allowUint, allowFloat,
		allowEnumWord, allowNumberMap, keepEnumList:
		return true
	default:
		return false
	}
}

// lookup 在表里找一条规则。
//
// 匹配按三层走，越靠前越具体：
//
//  1. 把运行期的路径归一化（`messages[0].content[1].name` → `messages[].content[].name`）
//     后与表里的路径精确比对；
//  2. 去掉开头若干段再比对——表里写的是「常见位置的写法」，
//     同一层的字段在别的容器里再出现一次时（`choices[].message.role`
//     与归一化的 `message.role`）不该因为少写一行就退化成默认档；
//  3. 只按最后一段的键名比对，作为兜底。
//
// 命中顺序是从最具体到最宽：
//
//  1. 把运行期路径归一化（`messages[0].content[1].name` → `messages[].content[].name`）
//     后精确比对；
//  2. 逐段去掉开头再比对——表里写的是「常见位置的写法」，而同一层的字段
//     可能出现在别的容器里（`choices[].message.role` 与归一化后的
//     `message.role`）。从去掉一段开始扫，先命中的就是最具体的那条；
//  3. 只写键名的兜底规则。
//
// 先前只按键名匹配，于是同名但不同位置的字段被强行合并：请求侧
// `tools[].name`（要引用化的工具名）被 `messages[].name`（脱敏）挡住，
// 响应侧 `error.param`（要保留的枚举）被 `content[].name`（引用化）挡住。
// 两次都是「一个位置的定义顺手决定了另一个位置」，而且都不报错。
func (t fieldTable) lookup(path, key string) (fieldKind, bool) {
	if len(t) == 0 {
		return 0, false
	}
	normalized := normalizePath(path)
	if k, ok := t[normalized]; ok {
		return k, true
	}

	// 逐步去掉开头的层级，取第一段能匹配上的（也就是最长的）那条。
	segs := strings.Split(normalized, ".")
	for i := 1; i < len(segs); i++ {
		if k, ok := t[strings.Join(segs[i:], ".")]; ok {
			return k, true
		}
	}

	// 兜底：只写了键名的规则。
	k, ok := t[nameOnlyKey(key)]
	return k, ok
}

// nameOnlyKey 是「只按键名」的兜底规则在表里的键前缀。
//
// 加前缀是必要的：不加的话 `content`（键名兜底）会与 `content`（归一化路径）
// 撞成同一个键，而这两个键一个是「随便哪个叫 content 的位置」、
// 一个是「顶层那个 content」，含义并不相同。
func nameOnlyKey(key string) string {
	return "\x00" + key
}

// normalizePath 把运行期的具体路径换成表里那种带 `[]` 的写法。
func normalizePath(path string) string {
	if !strings.Contains(path, "[") {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		if path[i] != '[' {
			b.WriteByte(path[i])
			continue
		}
		// 下标一律折叠成 `[]`：表里不区分第几个元素，
		// 也不该因为「写到第三个元素时行为不同」而多出一种情形。
		j := strings.IndexByte(path[i:], ']')
		if j < 0 {
			b.WriteString(path[i:])
			break
		}
		b.WriteString("[]")
		i += j
	}
	return b.String()
}

// buildTable 把条目列表编成查找表：归一化路径 → 处理方式。
//
// 同一个归一化路径重复出现时不报错（校验在更早的一步做），
// 这里取会留下更少信息的那条——同名同位置的两条规则意味着表写重了，
// 而「挑保守的那条」保证重写不会放宽任何一条。
func buildTable(rules []fieldRule) fieldTable {
	out := make(fieldTable, len(rules))
	for _, r := range rules {
		key := normalizePath(r.Path)
		if existing, dup := out[key]; dup {
			out[key] = moreConservative(existing, r.Kind)
			continue
		}
		out[key] = r.Kind
	}
	// 兜底档：只按最后一段的键名。表里没有这一段的任何写法时它才起作用
	//（例如上游把消息体嵌进一个新容器），与上面两层分开存，避免把
	// 「具体位置」与「随便哪个位置」混在同一张表里。
	for _, r := range rules {
		key := lastSegment(r.Path)
		if _, exists := out[key]; exists {
			continue
		}
		fb := nameOnlyKey(key)
		if existing, dup := out[fb]; dup {
			out[fb] = moreConservative(existing, r.Kind)
			continue
		}
		out[fb] = r.Kind
	}
	return out
}

// moreConservative 在两条规则里挑会留下更少信息的那一条。
func moreConservative(a, b fieldKind) fieldKind {
	if revealRank(a) <= revealRank(b) {
		return a
	}
	return b
}

// revealRank 是各档处理方式「会留下多少信息」的粗略次序，数值越小留得越少。
//
// 次序本身不必精确，它只用来在两条同名规则并存时把天平压向信息更少的那一侧：
// 全脱敏 < 只留长度 < 只留结构 < 认得出的枚举词/数字 < 保留全部形状。
func revealRank(kind fieldKind) int {
	switch kind {
	case hideValue, hideSubtree:
		return 0
	case hideText, hideStringList:
		return 1
	case toolRef, toolRefID:
		return 2
	case hideKeysKeepShape, extraField, schemaField, blockListField, roleField,
		headersField, responseHeadersField:
		return 3
	case allowEnumWord, allowNumberMap, keepEnumList:
		return 4
	case allowUint, allowBool:
		return 5
	case allowFloat:
		return 6
	case keepShape:
		return 7
	default:
		// 认不出的档位当成「留得最多」：同名冲突时它会输给任何一条已知规则。
		return 8
	}
}

// fieldTable 是编译后的允许表：归一化路径或 `\x00键名` → 处理方式。
type fieldTable map[string]fieldKind

// lastSegment 取路径的最后一段，去掉数组下标：`messages[].content[].type` → `type`。
func lastSegment(path string) string {
	out := path
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] == '.' {
			return trimIndex(out[i+1:])
		}
	}
	return trimIndex(out)
}

// trimIndex 去掉一段路径尾巴上的 `[]` 或 `[N]`。
func trimIndex(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '[' {
			return s[:i]
		}
	}
	return s
}
