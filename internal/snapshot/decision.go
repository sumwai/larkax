package snapshot

// 决策表面（internal/systemone 那条线格式）的允许表。
//
// # 它只放判据字段
//
// 这条表面上一次请求的判据是**候选集合**，一次响应的判据是**选了谁、为什么**。
// 别的都不进：请求里的 state 与 questions、响应里的 answers 全是内容
// （前者是用户原文，后者是模型判断的结果）。
//
// 它们不在这张表里，而且比 chat 那条路更进一层——**它们连红色的形状都不留**：
// 适配层根本不把它们摊进 map（见 internal/snapshotwire），所以它们不是
// 「进了脱敏器又被替换掉」，是压根没到过。这条链路的口径因此比「表里没有的
// 一律脱敏」更强，也让这张表的完备性与用户内容的留存无关。
//
// # 各维分数为什么不在表里
//
// 路由的每一维分数算在 internal/route 的**私有**函数里，出来的只有加权总分，
// 而那个总分拿不到拆分（exports 的 Router.Score 重算一遍，用的是不同口径——
// 见那里的注释）。所以这张表记的是那几维的**输入**：健康度、额度、成本、
// 缓存命中率。它们才是「为什么选了它」能被人复核的部分；分数本身拿不到，
// 编一个出来不如没有。
//
// # 与 SkeletonVersion 的关系
//
// 版本号说的是「某个字段在骨架里会变成什么样」。**多一张表本身不构成递增的
// 理由**：决策这个表面从第一天起就是当前口径，库里不存在按旧口径产出的决策行。
// 区分决策行与 chat 行靠骨架里的 surface 标量，不是靠版本号——两者共用同一个
// SkeletonVersion，因为版本号是整条快照链路的口径，不是每张表各一份。
//
// # 记一对多与「没有数据」的写法
//
// 指针字段（余量、成本、缓存命中率、响应时间分位）为 nil 表示**没有数据**，
// 那时适配层整个键都不写。这与「是 0」必须分得开：合并两者正是路由那侧
// 修过的一条缺陷（见 route.Candidate.CostPerUnit 的注释），快照把这两种
// 现场记成一样，下一次排查就得重新踩一遍。

// decisionCommonRules 是两个方向共有的开头。
//
// 两个方向都记模型名与候选数，因为它们是这一行的**身份**：骨架被截断降级成
// 摘要层时（见 topScalars），留下的就是顶层标量，而这两个正是「这次决策长
// 什么样」的全部答案。截断后连方向都认不出来的行等于没有。
var decisionCommonRules = []fieldRule{
	{Path: "surface", Kind: allowEnumWord,
		Note: "这一行是哪条表面产出的。request_snapshots.direction 只有 request / response 两个取值，决策与 chat 落在一起，靠它区分"},
	{Path: "model", Kind: allowEnumWord, UpstreamValue: true,
		Note: "入站解析之后的规范模型名（不是调用方写的原名，也不是上游名）。与 chat 那条路的同名字段**取值不同**——chat 的请求方向记的是发出去的那份请求上的**上游名**（见 docs/development.md 的快照一节），所以两者不要混着 join；两条路共有的性质是「选错模型、别名写错都靠它复现」，取值来自客户端，所以自带枚举词检查"},
	{Path: "candidates_count", Kind: allowUint,
		Note: "候选数。它是「当时有几个可选」这句话本身，也是骨架被截断后唯一还留下的判据"},
}

// decisionCandidateFields 是候选集合的判据字段，请求方向与响应方向共用。
//
// 共用一份而不是各写一遍：两个方向记的是同一组判据，只是时点不同
// （选路之前 / 选路之后）。写两遍的第一个后果是加一维时只加了一处，
// 于是「请求那行有、响应那行没有」这件事没有任何东西会报出来。
var decisionCandidateFields = []fieldRule{
	{Path: "candidates", Kind: keepShape,
		Note: "候选集合是这条表面的判据主体：一个候选一段，逐段走子规则"},
	{Path: "candidates[].channel_id", Kind: allowUint,
		Note: "候选身份。它是「选了谁 / 为什么没选它」唯一的连接键，也是与账单对上所需的那个数"},
	{Path: "candidates[].priority", Kind: allowUint,
		Note: "排序的第一判据：数值大者先试。负值会被降级成描述符（表里按非负整数放行）——那本来就是个不该出现的取值"},
	{Path: "candidates[].weight", Kind: allowUint,
		Note: "渠道上配的权重。它不参与排序（排序走 priority 与分数），记进快照是为了让「当时配的是什么」可查；0 是数据，负数按内容处理"},
	{Path: "candidates[].consecutive_failures", Kind: allowUint,
		Note: "健康度里权重最高的一维：连续失败次数决定这个渠道该不该被跳过"},
	{Path: "candidates[].success_rate", Kind: allowFloat,
		Note: "近期成功率，健康度的一维"},
	{Path: "candidates[].latency_p95_ms", Kind: allowUint,
		Note: "近期响应时间分位，换算成毫秒。没有数据时这一格不出现——「分位是 0」与「没有数据」不是一件事"},
	{Path: "candidates[].quota_unit", Kind: allowEnumWord, UpstreamValue: true,
		Note: "余量的单位（tokens / requests / money）。取值来自渠道配置与上游回报，所以按枚举词放行：从上游来的字符串一律要自带取值检查"},
	{Path: "candidates[].quota_remaining", Kind: allowFloat, UpstreamValue: true,
		Note: "剩余量。它与 quota_unit 成对读——单位不同的两个数不可比，单看这个数会得出错的结论"},
	{Path: "candidates[].quota_confidence", Kind: allowFloat,
		Note: "这个余量有多可信。路由按它把额度那一维的分往中性拉，所以不记它，余量这一格就复核不了"},
	{Path: "candidates[].quota_kind", Kind: allowEnumWord,
		Note: "余量的来源：实测 / 自记账 / 错误观测。它决定上一条的可信度上限"},
	{Path: "candidates[].cost_per_unit", Kind: allowFloat,
		Note: "成本这一维的输入。没有数据时这一格不出现，与「成本是 0」必须分开"},
	{Path: "candidates[].cache_hit_rate", Kind: allowFloat,
		Note: "缓存命中率，取值 0..1"},
	{Path: "candidates[].sample_count", Kind: allowUint,
		Note: "上面两条统计的样本数：少于阈值时它们不参与打分，不记下来就复核不了"},
}

// decisionResponseTail 是响应方向独有的部分：选了谁、试过谁、为什么没成。
//
// 这一段回答的是「为什么」，而它是响应方向存在的理由——请求方向记的是
// 「我们打算怎么处理」，只有走到结局之后才有真正的答案。
var decisionResponseTail = []fieldRule{
	{Path: "selected", Kind: keepShape,
		Note: "这次实际选中并发出去的那一个。**只有成功时才有这一段**：全部失败时没有选中项，把最后试过的那个写成 selected 会让一次全军覆没读成一次成功选路"},
	{Path: "selected.channel_id", Kind: allowUint,
		Note: "选中的渠道，与账单上的 channel_id 是同一个数"},
	{Path: "selected.upstream_model", Kind: allowEnumWord, UpstreamValue: true,
		Note: "这次实际发出去的上游模型名（别名翻出来的）。取值由商家配置，所以同样自带枚举词检查——配错一个别名就是要靠它看出来的那类事"},
	{Path: "candidates[].selected", Kind: allowBool,
		Note: "这一段候选是不是被选中的那个。把「选中」与「淘汰」放进同一张清单，一行就能回答「为什么是它」——分开读两个方向是最容易被漏掉的一步"},
	{Path: "attempts", Kind: keepShape,
		Note: "按顺序记下每一次尝试，含最后成功的那一次。降级（换渠道重试）的全部经过只在这里"},
	{Path: "attempts[].channel_id", Kind: allowUint,
		Note: "这一次试的是哪个渠道"},
	{Path: "attempts[].succeeded", Kind: allowBool,
		Note: "这一次成没成。它把「试了几个」与「第几个成的」分开"},
	{Path: "attempts[].retryable", Kind: allowBool,
		Note: "这一次的失败是否值得换下一个渠道。**降级原因**的第一层：false 表示当场放弃，后面的候选连试都没试"},
	{Path: "attempts[].upstream_status", Kind: allowUint,
		Note: "这一次拿到的上游 HTTP 状态码。与快照表上那一列同一个判据（见 internal/gateway 的 upstreamEvidence）；没拿到状态码时这一格不出现"},
	{Path: "attempts[].error_kind", Kind: allowEnumWord,
		Note: "这一次的失败分类，取值与 billing_requests.error_code 是同一套（见 pipeline 的 ErrorUpstream* 常量）。**降级原因**的第二层：它说的是失败发生在哪一段"},
	{Path: "attempts[].duration_ms", Kind: allowUint,
		Note: "这一次耗了多久。超时类的失败靠它区分「真的慢」与「根本没连上」"},
}

// decisionExclusionFields 是被选路筛掉的候选的判据字段。
//
// # 它为什么只在响应方向
//
// 请求方向那一行采在**选路之前**（见 gateway.DecisionTrace），那时还没有
// 「筛掉了谁」这件事——排除是选路的结果。把它写进请求方向的表，等于宣示
// 一个永远不会出现的键。
//
// # 为什么要有它
//
// 候选集合与尝试列表之间的那一段从前没有出口：candidates 记的是选路之前
// 有几个候选，attempts 记的是真的发出去过哪几个，而那两条**没被发出去的**
// 为什么不在候选里，没有任何一个字段说得出来——差额本身也只是一道差额。
// 其中「有几条渠道的声明或状态不对，在选路那一步就被筛掉」正是这一节记的
// 那一类；差额里剩下的那些是没轮到就被提前放弃的（见 excluded 字段那条
// Note 与 gateway.DecisionOutcome.Excluded）。
//
// # 理由为什么是枚举词
//
// 记的不是那句给人看的话（「模型目录里输出上限 4096，这次请求要 8192」），
// 而是它的**分类**（route.Exclusion.Code）：那句话里带着模型名与数字，是自由
// 文本，按枚举词那一档会退成占位符；分类则是几个固定的 ASCII 词，过得了取值
// 检查，也能按它聚合。那句原文留在 `Exclusion.Reason` 里（今天还没有出口）。
var decisionExclusionFields = []fieldRule{
	{Path: "excluded_count", Kind: allowUint,
		Note: "被筛掉的候选数。它与 candidates_count 一起才是「当时有几个、几个被筛掉」；骨架超限降级成摘要层时，它是这一件事唯一还留下的痕迹（见 snapshot 的 topScalars）"},
	{Path: "excluded", Kind: keepShape,
		Note: "被选路筛掉的候选，一个一段。没有排除时整段不写，与 candidates 不同——「零个排除」不是一个事实，是「没有这件事」。它不是一份穷尽划分：candidates 与 attempts 的差额里还有「没轮到就被提前放弃」的候选（别名翻不出来、客户端断开、不可重试的失败后仍有未试候选），那些既不在这一段也不在 attempts 里"},
	{Path: "excluded[].channel_id", Kind: allowUint,
		Note: "被筛掉的是哪个渠道。它与 candidates[].channel_id 是同一个身份，两边对得上才说明「这个候选取过、被筛掉了」"},
	{Path: "excluded[].code", Kind: allowEnumWord,
		Note: "为什么被筛掉，取值见 internal/route 的 ExclusionCode，字面量是 ineligible / model_not_declared / capability / limit / health / quota（依次为：配置不全 / 渠道没声明支持这个模型 / 目录能力 / 目录上限 / 健康度 / 额度）。它是机器可读的分类而不是那句原文：原文带着模型名与数字，按枚举词判会退成占位符"},
}

// decisionRequestRules 是决策请求方向的允许表。
//
// 它记的是「我们打算怎么处理」：选路之前手上有哪些候选、各自什么状态。
var decisionRequestRules = append(
	append([]fieldRule{}, decisionCommonRules...),
	decisionCandidateFields...,
)

// decisionResponseRules 是决策响应方向的允许表。
//
// 它在请求方向那张表上追加两段，而不是另写一份：两份表的第一个后果是
// 加一维判据时只加一处，而两个方向记的是同一组判据。
var decisionResponseRules = append(
	append(append(append([]fieldRule{}, decisionCommonRules...), decisionCandidateFields...),
		decisionResponseTail...),
	decisionExclusionFields...,
)

// decisionAllowlists 把方向映射到决策表面的允许表。
//
// 方向取值与 chat 完全相同（request / response）：库表那一列的 CHECK 只认
// 这两个，而这条表面同样有请求与响应两个方向。表面在骨架里另有一个标量。
var decisionAllowlists = map[Root][]fieldRule{
	RootRequest:  decisionRequestRules,
	RootResponse: decisionResponseRules,
}
