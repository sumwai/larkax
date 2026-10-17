// Package route 在多个上游渠道之间选择，并在失败时换下一个。
//
// 它做的事只有两件：**排序**与**重试**。协议转换在 internal/channel，
// 计费在 internal/billing。把选路单独放一层，是因为它的变化原因与那两者
// 都不同——换一家上游是加适配器，改计价是改规则，而改选路策略是改这里。
//
// 分层：
//
//	gateway ──► route ──► channel.Adapter ──► 上游
//	              │              │
//	              │              └── 只做协议转换
//	              └── 排序、重试、换渠道
//
// # 两条不可动摇的约束
//
//  1. **已经吐字给客户端的流不能换渠道。** 换了会让客户端收到两段拼接的
//     回答，而它无法察觉。边界是「适配器是否已经接受请求」——一旦拿到
//     事件流就不再换（见 stream.go 的 ChatStream）。至于失败发生在吐字
//     之前还是之后，由消费事件流的那一层判断——目前只有 gateway 一处
//     （见 gateway/stream.go 里 started 的注释）。
//  2. **失败的调用不计费。** 换渠道之后成功的那次才计费，前面失败的不计。
//     这意味着本包要把「哪些尝试已经花掉了费用」如实报出来——
//     上游可能在断开前已经产生了费用，只是我们没有拿到回执。
package route

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/overrides"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/tokens"
)

// Candidate 是一个可选渠道连同它的实时状态。
//
// 状态与渠道本身分开传：channel.Channel 是「这个渠道是什么」（来自配置），
// 而这里的是「它现在怎么样」（来自健康度、配额、最近的表现）。
// 混在一起会让「配置改了」与「状态变了」在缓存里分不清。
type Candidate struct {
	Channel channel.Channel

	// OwnerUserID 是渠道所属的商家。0 表示平台自营。
	//
	// 它不参与打分，但结算需要它：商家级计价规则按这个字段匹配，
	// 而结算必须用与预留同一个 owner——否则两次调用可能匹配到不同的
	// 规则，算出一个与预留无缘由的差额。
	OwnerUserID int64

	// Priority 数值大者先选。商家用它表达「这个渠道是主力」。
	Priority int
	// Weight 是同优先级内的分流比例。<= 0 视为 1。
	Weight int

	// Health 是渠道健康度，由 channel_health_rollup 汇总而来。
	Health Health
	// Quota 是额度快照，直接取自 channel_quota_state 那一行。
	//
	// 类型是 quota.State 而不是更窄的形态：**判断**「这个额度状态能不能用」
	// 的判据在 internal/quota（Routable / Usable），而 Routable 要区分
	// 「从未观测过」与「观测过却没拿到数值」——那需要 LastProbeAt 与
	// LastErrorCode 两个字段。少带一个，判据就退化成「有没有数值」，
	// 而那是本包早先自己重写的那一遍（见 issue #204）。
	//
	// 读侧只填本包用得上的列（Remaining / Unit / Confidence / ResetsAt /
	// Kind / LastProbeAt / LastErrorCode）；窗口那几列不填，本包不看它们。
	//
	// 余量只在**同一单位**下与其他候选可比（见 scoreQuota）：Remaining 是
	// 裸数字，Unit 说明它按什么计，Confidence 说明这个数有多可信——本包按
	// 后者把分数往中性分拉，所以它必须由读侧填上，否则每个候选都退成
	// 「没有意见」，额度这一维等于关掉。
	//
	// Estimated **不在**上面那张单子里：本包不再看它。它想说的「这个数是
	// 推算的」，Confidence 已经说了（自记账的置信度上限就是 0.8，永远达不到
	// 实测的 1），两处一起看等于把同一件事扣两次。它的读者只剩插件契约
	// 那一处（见 internal/plugin 的 RouteContextFrom）。
	Quota quota.State

	// CostPerUnit 是最近观察到的单位成本，用于「成本」这一维度。
	//
	// nil 表示**没有数据**，而不是「成本为 0」：两者在选路里是不同的结论，
	// 而用一个零值把「没有」与「是 0」合并正是本包此前那条缺陷
	// （三列恒为 0，于是成本这一维对所有候选都是同一个常数）。
	// 没有数据的维度不参与加权（见 score）。
	CostPerUnit *float64
	// CacheHitRate 是近期缓存命中率，0..1。nil 表示没有数据。
	CacheHitRate *float64
	// SampleCount 是上述统计的样本数。少于 minCacheSamples 时不参与打分——
	// 一两次请求的命中率不说明任何事。
	SampleCount int64

	// Profile 是模型目录对**这个候选所服务的规范模型**声明的能力与上限
	// （见 ModelProfile）。nil 表示目录里没有这个模型的记录：未知，不做
	// 目录判据。有记录而某一项没配过时，那一项**也是**未知——两种情形都
	// 放行，区别只在依据是什么。
	//
	// # 为什么它挂在这里
	//
	// 它是**模型级**的：同一个规范名下的候选拿到的是同一份，逐候选携带
	// 确实有冗余。挂在这里是因为本包不碰数据库（见 Router.Aliases 的同一条
	// 理由），而选路这一步既要知道候选是谁、也要知道这次请求要什么——
	// 两边缺一个都判不出来。候选的来源（internal/channels）读目录、把这份
	// 数据摆进来，本包只按它判。
	Profile *ModelProfile

	// Endpoint 是这条候选把请求发到哪条路径，由渠道 type 指向的协议决定
	// （channel.Protocol.Endpoint()）。空串 = 未知，不判。
	//
	// # 为什么在候选上而不在请求上
	//
	// 一条请求走哪条端点**不是请求的属性**：同一份 chat 形状的请求可以发给
	// 一家说 chat 的上游，也可以发给另一家说 responses 的上游，差别在渠道。
	// 所以它是候选的属性，由候选的来源（internal/channels）按渠道 type 算好
	// 摆进来，判据只在这里用它（见 Needs 与 profileRejection）。
	//
	// 空串只有一种来路：库里那个 type 当前版本的二进制不认识。那种候选本来
	// 就选不中（执行器会报 ErrUnknownType），所以这里不拿一条拿不准的判据
	// 再挡它一次——「不知道」站在放行这一侧。
	Endpoint channel.Endpoint
}

// ModelProfile 是模型目录对一个规范模型声明的能力与上限。
//
// # 判据只看三项能力、一份端点清单与两个上限
//
// 目录里的能力矩阵有六列，这里只带判据用得上的三项：请求里能观察到、
// 而模型可能不支持的。audio / cache / batch 没有对应的请求信号（请求里
// 看不出「这次要用缓存」），带上它们只是让这个结构体看起来像那张表。
//
// 端点清单是**另一类**判据：它不来自请求（见 Endpoints），而来自这条候选
// 往哪条路径发。两者放在一起是因为它们都挂在同一条目录记录上。
//
// # 三项都是三态，未知与「不支持」的处置相反
//
// 每一项是 `*bool`，与目录里那六列的三态一一对应（见 internal/catalog 的
// Capabilities）：
//
//	nil    未知：**这一项不参与判据**，放行
//	true   声明支持：放行
//	false  声明不支持：请求用到它时这条候选出局
//
// 「没配过」不等于「配了说不支持」。写入侧只有显式开关，所以一条只填了
// 名字的记录在库里是 NULL——按 false 处理会让带工具的请求从此选不到它，
// 哪怕那个上游其实支持工具（见 #485）。
//
// 两个上限列同一个口径，只是那里用 0 表示未知（见 Constraints）。
//
// nil 的 ModelProfile 覆盖目录里压根没有这个模型的情形：那时没有任何依据，
// 一律放行（见 Candidate.Profile）。
type ModelProfile struct {
	// Tools / Vision / Reasoning 是目录声明的三项能力。nil = 未知，不判。
	Tools     *bool
	Vision    *bool
	Reasoning *bool

	// Endpoints 是目录声明这个模型在上游哪几条端点上被服务。
	// nil = 未知，不判；非 nil 时按集合判——候选那条端点不在里面就出局。
	//
	// 三态与上面三项同一套口径，只是这里用切片表达「未知」：
	//
	//	nil    未知：这一列是 NULL，从没配过。**这一项不参与判据**，放行
	//	{}     明确声明一条都不支持：任何候选都出局
	//	非空   候选的端点不在里面就出局
	//
	// 「未知」与「一条都不支持」必须分开，理由同上：把 nil 读成空集合会让
	// 一条只填了名字的记录选不到任何候选，而那个上游其实服务着这个模型。
	Endpoints []channel.Endpoint

	// ContextLimit 是上下文窗口（输入 + 输出）。0 表示未知，不判。
	ContextLimit int64
	// OutputLimit 是单次输出的上限。0 表示未知，不判。
	OutputLimit int64
}

// Needs 是一次请求对上游的要求。
//
// 它只装「能从请求本身读出来、且目录里有对应声明」的那几项：用到哪些能力、
// 要花多少 token。余额、区域、合规之类不在其中——那些不是模型目录能回答的。
//
// **端点也不在其中**：一条请求走哪条路径不是它的属性（同一份 chat 形状的
// 请求可以发给说 chat 的上游，也可以发给说 responses 的上游），它由候选
// 决定（见 Candidate.Endpoint 与 profileRejection）。
type Needs struct {
	// Tools 表示这次请求带工具：有工具定义，或消息里有工具调用/结果块。
	Tools bool
	// Vision 表示这次请求里有图片。
	Vision bool
	// Reasoning 表示这次请求要了推理：带了推理参数，或消息里有思维链块。
	Reasoning bool

	// InputTokens 是输入侧的估算值（口径见 NeedsOf）。
	InputTokens int64
	// MaxOutputTokens 是这次请求要的输出上限，即 channel.Request.MaxTokens。
	MaxOutputTokens int64
}

// NeedsOf 从一条 chat 形状的请求里读出它需要上游具备什么。
//
// # 输入 token 用的是哪一个量
//
// **与入口预留不是同一个式子，而且是刻意的。** 预留那侧这个数决定「这次冻多少
// 钱」，少算会让预留低于结算、差额由平台承担，所以它把工具定义、图片与历史里的
// tool_use 参数也算进去
// （见 internal/api 的 inputTokensOf 与 internal/tokens 的 payload.go）。
// 这里的数用在**上限判据**上，偏小只意味着判据偏宽松——不会把一个上游其实
// 接得住的请求挡下来，代价是多打一次上游。两边估错的代价方向不同，所以取的
// 不是同一个量，改一边不该顺手把另一边也改了。
//
// 这里只算消息里的**正文**（tokens.EstimatedTokens(req.Model, req.PromptText())）。
//
// # 载体请求
//
// ChatModel 那条路递进来的是只含 Model 的载体（见 ChatModel），于是这里的
// 每一项都是零值：不带能力、不判上限。这不是遗漏——决策那条表面没有 chat
// 形状的请求，它的输入预算是一个常量（见 internal/systemone 的
// MaxInputTokens），压根不经过这条载体。
func NeedsOf(req *channel.Request) Needs {
	needs := Needs{
		Tools:           len(req.Tools) > 0,
		Reasoning:       req.ReasoningEffort != "",
		InputTokens:     tokens.EstimatedTokens(req.Model, req.PromptText()),
		MaxOutputTokens: int64(req.MaxTokens),
	}

	// 消息里的块也算：一个带 tool_use / tool_result 的历史、一张图片、
	// 一段思维链，都是「这次请求用到了这项能力」——它们与参数形态的
	// 信号（Tools 定义、ReasoningEffort）在做同一件事，只是写法不同。
	for _, m := range req.Messages {
		for _, block := range m.Content {
			switch block.Type {
			case "image":
				needs.Vision = true
			case "tool_use", "tool_result":
				needs.Tools = true
			case "thinking":
				needs.Reasoning = true
			}
		}
	}
	return needs
}

// profileRejection 说明这条候选为什么接不住这次请求，空分类表示放行。
//
// 能力、端点与上限三类判据的处置相同（这条候选出局），所以在这里合成一个
// 函数，由 Select 记进 Exclusion——分成几处判会让「为什么没选那个渠道」在
// 排查时要读几个地方，而它们记的是同一份 Profile 上的结论。
//
// 判据的先后是：能力（请求要不要用）→ 端点（往哪条路径发）→ 上限（装不装
// 得下）。一条候选只记**第一个**命中的理由，所以顺序决定了现场看到哪一条。
// 端点排在能力之后是因为它更接近「这条渠道根本不该试」：能力那一栏说的是
// 「这个模型做不做得了这件事」，调参数或许能绕过；端点不对则怎么改请求都
// 没用——换一条候选才行。
//
// 两个返回值是这条排除的分类与说明（见 Exclusion.Code 与 Reason）。分类只有
// 两档：能力与端点合成 CodeCapability（它们说的都是「目录里那一条声明覆盖
// 不到这次请求」），上限是 CodeLimit。分类与说明一起算出来，理由同 DrainReason。
func (c Candidate) profileRejection(needs Needs) (ExclusionCode, string) {
	p := c.Profile
	if p == nil {
		return "", ""
	}

	switch {
	case needs.Tools && declaredUnsupported(p.Tools):
		return CodeCapability, "模型目录里不支持工具调用"
	case needs.Vision && declaredUnsupported(p.Vision):
		return CodeCapability, "模型目录里不支持图片"
	case needs.Reasoning && declaredUnsupported(p.Reasoning):
		return CodeCapability, "模型目录里不支持推理"
	}

	// 端点：目录说这个模型在哪几条端点上被服务，而这条候选把请求发到
	// 另一条上去——上游会回 400 unsupported_model，从我们这边看与「模型
	// 不存在」长得一样，且只有真打一次才发现。所以挡在选路这一层。
	//
	// # 三个状态，只有「一条都不支持」与「不在这里面」出局
	//
	//   nil   未知（没配过）：**不参与判据**，放行
	//   {}    明确一条都不支持：任何候选都出局
	//   非空  候选的端点不在里面就出局
	//
	// 判据写在 `p.Endpoints != nil` 上而不是 `len(p.Endpoints) > 0` 上：
	// 后者会把「明确一条都不支持」也判成未知，而它与声明的语义正好相反
	// ——那是 #485 修过的同一类形状（把「没配过」与「配了说没有」合成一个
	// 零值）。有了这一档，`{}` 才是一种**能用的语法**：把一个模型暂时从
	// 所有渠道上摘下来，改一处声明就够，不必逐个渠道去动 models 白名单。
	//
	// # 另一个「不知道」也放行
	//
	// 候选的端点为空串（库里那个 type 当前版本的二进制不认识）时不判。
	// 理由与三项能力的 declaredUnsupported 同一条：判据要站在放行这一侧。
	// 那种候选本来就选不中——执行器会报 ErrUnknownType 并换下一个。
	if p.Endpoints != nil && c.Endpoint != "" && !slices.Contains(p.Endpoints, c.Endpoint) {
		if len(p.Endpoints) == 0 {
			// `{}`：明确一条都不支持。它没有清单可列，所以单独说一句——
			// 「只在这些端点上服务：」后面空着的话，读的人会以为那句话
			// 没打完，而这一档的结论是「任何候选都选不中」。
			return CodeCapability, fmt.Sprintf(
				"模型目录里这个模型一条端点都不支持；这条候选走的是 %s", c.Endpoint)
		}
		return CodeCapability, fmt.Sprintf("模型目录里只在这些端点上服务：%s；这条候选走的是 %s",
			joinEndpoints(p.Endpoints), c.Endpoint)
	}

	// 上限：只有目录里填了数（> 0）时才判。0 是未知（库里是 NULL），不判
	// ——一次采集失败不该让整批模型用不了（见 ModelProfile）。
	if p.OutputLimit > 0 && needs.MaxOutputTokens > p.OutputLimit {
		return CodeLimit, fmt.Sprintf("模型目录里输出上限 %d，这次请求要 %d",
			p.OutputLimit, needs.MaxOutputTokens)
	}
	// 上下文按「输入 + 输出」比：那是窗口的两半，只比输入会让一个装不下
	// 这么多输出的请求看起来是合规的。
	if p.ContextLimit > 0 && needs.InputTokens+needs.MaxOutputTokens > p.ContextLimit {
		return CodeLimit, fmt.Sprintf("模型目录里上下文上限 %d，这次请求输入 %d + 输出 %d",
			p.ContextLimit, needs.InputTokens, needs.MaxOutputTokens)
	}
	return "", ""
}

// declaredUnsupported 判断一项能力是不是**被明确声明为不支持**。
//
// 只有 false 算：nil（没配过）与 true（支持）都放行。判据写成「明确说不」
// 而不是「不是明确说支持」，是为了让那个没配过的默认状态站在放行这一侧
// ——把 nil 当成不支持会让一条只填了名字的记录选不到任何带工具的请求，
// 而那正是这条三态要修掉的东西（见 ModelProfile 与 #485）。
func declaredUnsupported(declared *bool) bool {
	return declared != nil && !*declared
}

// joinEndpoints 把目录声明的那几条端点拼成一句供排查用的话。
//
// 取值本身就是路径，用逗号分隔而不是顿号：它们是 ASCII 的 URL 片段，与中文
// 顿号混排读起来像两个词粘在一起。
func joinEndpoints(endpoints []channel.Endpoint) string {
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parts = append(parts, string(endpoint))
	}
	return strings.Join(parts, ", ")
}

// Health 是渠道的健康度。
type Health struct {
	// ConsecutiveFailures 是连续失败次数。它在打分里权重最高——
	// 一个正在连续失败的渠道，无论多便宜都不该被选中。
	//
	// 它是一个**逐次维护的真计数**（成功清零、失败加一，见
	// internal/channels 的 RecordCall），不是从小时桶里估出来的：
	// 桶内有过成功时，那段连续长度落在 0 与 failures 之间，估不出来。
	//
	// 读取侧会把冷却期已过的计数当作 0（见 internal/channels 的
	// streakCooldown）：否则被摘掉的渠道没有流量，计数永远不会降回去。
	ConsecutiveFailures int
	// SuccessRate 是近期成功率，0..1。
	SuccessRate float64
	// LatencyP95 是近期响应时间分位。nil 表示**没有数据**——
	// 与「分位恰好是 0」不是一件事，见 scoreLatency。
	LatencyP95 *time.Duration
}

// Router 在候选集中选择渠道并执行请求。
type Router struct {
	// Weights 是打分的权重。零值用 DefaultWeights。
	Weights Weights
	// Rand 是可注入的随机源，便于测试得到确定结果。nil 用全局源。
	Rand *rand.Rand
	// Now 可注入，便于测试时间相关的逻辑。
	Now func() time.Time

	// Aliases 把规范名翻成某个渠道上的上游名，连同这条别名上配的参数覆盖。
	// **必填**，nil 时请求失败（见 ErrNoAliasSource）。
	//
	// # 为什么这一项在路由层
	//
	// 一个规范名在不同渠道下对应不同上游名，所以「翻成什么」只有拿到
	// **选中的那个渠道**之后才答得出来，而选渠道正是本包的事。放在更上层
	// （入口）只能翻一次，翻出来的是「某个候选」的名字；放在更下层
	// （适配器）则要让适配器自己去查库，而它连渠道配置之外的东西都不该碰。
	//
	// 参数覆盖跟着名字一起来，理由与名字同一条：它是**行**上的字段，命中哪条
	// 别名就只有那条别名的覆盖（见 internal/catalog 的 Alias）。分成两次问
	// 还会多一次往返。
	//
	// # 为什么是接口而不是直接查库
	//
	// 本包不碰数据库：它只做排序与重试，脱离库与网络就能测。别名的判据
	// （渠道级优先于平台级、一条都没配过时按同名处理）只有一份实现，在
	// internal/catalog；这里只问一句「这个渠道上这个规范名该叫什么」。
	Aliases ModelAliases
}

// ModelAliases 把规范模型名翻成某个渠道上的上游模型名，并给出这条别名上
// 配的参数覆盖。
//
// 声明在本包、实现在 internal/catalog——与 CandidateSource 在 gateway 声明、
// 由 internal/channels 实现是同一个做法：本包不认识那张表，只要求一个答案。
//
// 覆盖那一项在这里只**取回来**，不在这条接口上表达它的语义：能覆盖哪些键、
// 与调用方冲突时谁赢，判据在 internal/overrides，本包只负责把它应用到这次
// 尝试的请求副本上（见 upstreamRequest）。
type ModelAliases interface {
	Alias(ctx context.Context, channelID int64, canonicalModel string) (upstreamModel string, overrides map[string]any, err error)
}

// ErrNoAliasSource 表示没有配置别名来源。
//
// 它与「这个渠道没配别名」分开：那个是配置缺失（要人去补一条别名），
// 这个是**装配缺失**（要人去接一行代码）。两者的处置完全不同。
//
// 不把 nil 当成「不做翻译、原样发规范名」：那会让「忘接了」与「这个部署
// 不用别名」在响应、日志和账单上完全一样，而前者意味着别名功能整个没生效
// ——上游收到的名字是规范名，只要两者不同就必然打不通，而现场看到的只是
// 上游说这个模型不存在。
var ErrNoAliasSource = errors.New("未配置别名来源：规范名到上游名的映射没有接上")

// Weights 是各维度在总分里的权重。
//
// 做成可配置而不是写死：权重取决于这台机器要优化什么——
// 一个成本敏感的部署与一个延迟敏感的部署不该用同一组数字。
// 但**默认值必须存在且保守**，否则第一次部署的人无从下手。
//
// 权重是**相对**的，不是绝对份额：某一维没有数据时它整条退出分母，
// 剩下的维度按各自权重归一（见 score）。所以给一个维度填 0.5 的效果，
// 取决于同一时刻还有哪几维有数据——而不是「它占总分的 0.5/N」。
type Weights struct {
	// Health 是最重的一项：一个正在失败的渠道不该因为便宜而被选中。
	Health float64
	// Quota 反映「还有多少可用」。见 scoreQuota 的注释。
	Quota float64
	// Cost 是单位成本，越小越好。
	Cost float64
	// Latency 是延迟，越小越好。
	Latency float64
	// Cache 是缓存命中率，越高越好。
	Cache float64
}

// DefaultWeights 是默认权重。
//
// 健康度占大头是有意的：其余维度都是「更好或更差」，而健康度是
// 「能用或不能用」。把它压到与其他维度同量级，会让一个正在连续失败的
// 便宜渠道仍然被选中。
//
// # 这五项不是每一刻都在起作用
//
// 成本、延迟、缓存三维的数据来自 channel_health_rollup 的三列，而写入侧
// 目前没有写它们（见 internal/channels 的 record.go）。所以**在今天**，
// 每个候选都没有这三项数据，它们整条退出加权，实际参与排序的是健康度与
// 额度两项。
//
// 这与「把这三项的权重写成 0」不是一回事，也是这里保留原值的理由：
// 写 0 是承认这三维永远不会生效；保留权重则意味着**一旦有数据，它立刻
// 按这里的数字生效**。把权重调成 0 会让接线那一天静默地什么也不发生。
var DefaultWeights = Weights{
	Health:  1.0,
	Quota:   0.6,
	Cost:    0.5,
	Latency: 0.3,
	Cache:   0.2,
}

// Eligible 判断一个候选是否**根本不可用**。
//
// 这是硬性条件，不是偏好：不满足就完全排除，而不是扣分。
// 判据必须能一眼看出对错——把这些也做成打分的话，
// 一个额度已经耗尽的渠道会以「分数略低」的形式留在候选集里，
// 而它一旦被选中就是一次注定失败的调用。
func (c Candidate) Eligible() (bool, string) {
	if c.Channel.ID == 0 {
		return false, "渠道缺少 ID"
	}
	if c.Channel.BaseURL == "" {
		return false, "渠道缺少 base_url"
	}
	if c.Channel.Credential == "" {
		return false, "渠道缺少凭证"
	}
	return true, ""
}

// DrainReason 返回「现在不该往这个渠道发请求」的分类与说明，没有则两者都是零值。
//
// 与 Eligible 分开：Eligible 是配置层面的、不会自己变的事实；
// 这里是运行状态。两者的处置也不同——配置错误要告警，
// 而状态不佳是正常现象，等一会儿自然会好。
//
// 额度那两道**判断**只有一处实现：internal/quota 的 Routable 与 Usable。
// 本包不自己读 Remaining 再判一遍——同一个语义两处实现，迟早分叉，
// 而分叉的表现是「读侧说能用、写侧说不能用」这类查不出原因的行为。
//
// 两个返回值出自**同一次判断**，所以一起在这里算出来：分成两处各判一遍的话，
// 留痕上记的分类与日志里写的那句话迟早会对不上，而那种偏差只有在下一次排查
// 时才会被发现，那时两份都已经是「现场」了。分类的读者是留痕（见
// Exclusion.Code），那句话的读者是日志与 runbook。
//
// 只关心那句话的调用方（含别的包的测试）写成 `_, reason := …`。
func (c Candidate) DrainReason() (ExclusionCode, string) {
	if c.Health.ConsecutiveFailures >= consecutiveFailureCutoff {
		return CodeHealth, fmt.Sprintf("连续失败 %d 次", c.Health.ConsecutiveFailures)
	}
	// 状态不足以支撑决策：观测过，却既没有数值也没有别的可用信息。
	// 「从未观测过」（上游不支持查询额度）**不算**这一档——把它一起挡掉
	// 等于默认停用所有查不到额度的渠道（见 quota.State.Routable 的注释）。
	if !c.Quota.Routable() {
		return CodeQuota, "额度观测过但没有可用信息"
	}
	// 有数值且已经见底：这是事实，不是偏好，必须挡住。
	// 未知（Remaining 为 nil）不挡——那说明这个渠道查不到额度，
	// 不是它没额度了。
	if !c.Quota.Usable() {
		return CodeQuota, "额度已耗尽"
	}
	return "", ""
}

// consecutiveFailureCutoff 是连续失败多少次之后暂时不再选它。
//
// 取 3 而不是 1：上游偶发的单次抖动很常见，一次失败就摘掉渠道会让
// 候选集频繁跳变，表现为「流量在渠道之间无规律地漂移」。
// 也不取更大：连续三次失败已经说明它此刻确实不可用。
const consecutiveFailureCutoff = 3

// Plan 是一次请求的选路结果：按顺序排好的候选。
type Plan struct {
	// Candidates 是按尝试顺序排好的候选。
	Candidates []Candidate
	// Excluded 是被排除的候选及其原因，用于排查「为什么没选那个渠道」。
	// 不记录它的话，商家问「我配了三个渠道为什么只用一个」时无从回答。
	Excluded []Exclusion
}

// Exclusion 记录一个候选为什么没进 Plan。
type Exclusion struct {
	ChannelID int64
	Name      string

	// Reason 是给人看的一句话，带着现场的具体取值——模型名、上限、连续失败
	// 次数。它留给日志与 runbook，**今天还没有出口**：生产里没有任何一处读它
	// （`Errors.Error()` 只用计数值与最后一次尝试的错误，不读这一段），
	// 读它的目前只有测试。它也不是留痕里的形态——那一侧要的是 Code。
	Reason string
	// Code 是这条排除的**机器可读分类**（见 ExclusionCode）。
	//
	// 它与 Reason 分成两个字段，而不是把 Reason 换成枚举词：Reason 里的模型名
	// 与数字是排查时唯一能把「为什么是它」复核下去的东西，去掉它们等于把这条
	// 记录变成一句无用的话；而留痕（request_snapshots 的 surface=decision）
	// 要的是稳定、可聚合、不需要再过一遍脱敏判据的取值——那正是 Code。
	// 一句话概括：**Reason 是给人看的，Code 是给机器看的。**
	Code ExclusionCode

	// Unsupported 表示这条排除的依据是**模型目录**（能力不匹配或超出上限）。
	//
	// 它与别的排除理由分开，只为一件事：一个候选都不剩时，错误文本要分得开
	// 「这个模型/这次请求本来就不该走这些候选」（一次都不会被试，也没人有
	// 必要重试）与「这些渠道现在都不行」（见 Errors.Unsupported）。其余场合
	// 它只是排查时的一条附加信息。
	Unsupported bool
}

// ExclusionCode 是一条候选被排除的机器可读分类。
//
// # 为什么要有它
//
// Exclusion.Reason 是给人看的一句话（「模型目录里输出上限 4096，这次请求要
// 8192」），它带着模型名与数字，是**自由文本**。留痕那一侧不能直接收它：那条
// 链路对字符串的处置是「看起来像枚举词才原样留下，否则按内容脱敏」，而一句话
// 必然落进后者——排查时看到的是一个文本占位符，等于没记。取它的**分类**则是
// 一个固定的 ASCII 词：过得了那一道判据，可以直接按它聚合，也不会因为哪天
// 有人在 Reason 里多拼一个用户输入片段就漏出去。
//
// # 取值与判据的对应
//
// 每一个取值对应 Select 里的一处判据，顺序与那里的先后一致（见 Select 的
// 注释）：配置 → 渠道声明 → 模型目录 → 运行状态。
type ExclusionCode string

const (
	// CodeIneligible 表示渠道本身配置不全（缺 ID / base_url / 凭证）。
	CodeIneligible ExclusionCode = "ineligible"
	// CodeModelNotDeclared 表示渠道没有声明支持这个模型。
	CodeModelNotDeclared ExclusionCode = "model_not_declared"
	// CodeCapability 表示模型目录声明它做不了这次请求要用到的能力（工具 /
	// 图片 / 推理），或者目录说这个模型不在候选走的那条端点上。
	//
	// 端点与三项能力共用一个取值：两处说的都是「目录里那一条声明覆盖不到
	// 这次请求」，差别只在声明的是哪一列——而那一列写在 Reason 里。
	CodeCapability ExclusionCode = "capability"
	// CodeLimit 表示请求超出模型目录声明的上限（输出上限或上下文窗口）。
	CodeLimit ExclusionCode = "limit"
	// CodeHealth 表示渠道当前健康度不佳（连续失败到了阈值）。
	CodeHealth ExclusionCode = "health"
	// CodeQuota 表示渠道当前没有可用额度（已耗尽，或观测不到可用信息）。
	CodeQuota ExclusionCode = "quota"
)

// pinFirst 返回一份把 channelID 那个候选提到最前的副本。
//
// # 为什么是「提到最前」而不是「只留它」
//
// 钉住的只是一个**顺序**：它失败了照旧换下一个，剩下的候选一个不少。
// 只留它等于把「优先用这个」变成「只能用这个」，而那个决定不该由一个
// 只看到候选集、看不到上游结果的调用方来做。
//
// # 找不到时为什么原样返回而不是报错
//
// 传进来的 channelID 不在候选集里只有一种来源：调用方把**另一份** plan
// 交给了决策者。它不是一个能在这里判对错的错——本函数看不到决策者那一侧，
// 也就分不出「调用方传错了 plan」与「这个渠道本来就不该被选中」。
// 而「插件选了候选集之外的渠道」在决策者那一侧就已经被挡下并记成一次失败
// （见 internal/plugin 的 pickChannel），走不到这里。
//
// 所以这里选的是**不改变任何东西**：顺序照旧，请求照常发出去。反过来
// （报错、或把整个候选集清空）会让一次调用方的传参失误变成一次请求失败。
//
// # 不改动入参
//
// 返回的是新切片：plan 还可能被调用方拿着（它要把 Excluded 记进观测），
// 就地排序会静默改掉那边看到的东西。同 internal/plugin 的 sortedByChannelID。
//
// # 按 channelId 找，不按下标
//
// 决策者看到的那份候选集与这里的这份**顺序不同**：交给它之前按 channelId
// 重排过（见 internal/plugin 的 sortedByChannelID）。所以它只能给出一个
// channelId，下标在两边不可比——`pinChannelID` 是 ID 而不是位置，正是为此。
func (p Plan) pinFirst(channelID int64) Plan {
	if channelID == 0 {
		return p
	}
	idx := -1
	for i, c := range p.Candidates {
		if c.Channel.ID == channelID {
			idx = i
			break
		}
	}
	if idx <= 0 {
		// -1：不在候选集里；0：本来就在最前。两种都不必动。
		return p
	}
	out := make([]Candidate, 0, len(p.Candidates))
	out = append(out, p.Candidates[idx])
	out = append(out, p.Candidates[:idx]...)
	out = append(out, p.Candidates[idx+1:]...)
	p.Candidates = out
	return p
}

// Select 按模型筛出候选并排序。
//
// 返回 Plan 而不是单个候选人：换渠道重试是调用方驱动的，它需要知道
// 下一个是谁。返回一个切片也让「为什么选了这个」可被完整解释。
//
// # 判据的先后
//
// 硬性条件（Eligible）→ 渠道是否声明支持这个模型 → 模型目录声明的能力、
// 端点与上限（needs 与 Candidate.Endpoint 对 Profile）→ 运行状态（健康度、
// 额度）。前两条是渠道级的配置，中间那条是模型级的声明，最后一条是「现在
// 怎么样」——一条候选只记**第一个**命中的理由，所以顺序决定了排查时看到的
// 是哪一条：先看到「这条配置本身就不该进来」，再看到「它此刻不可用」。
//
// needs 是这次请求对上游的要求（见 NeedsOf）。它由调用方给而不是在这里算：
// 同一个请求在插件那条路上也要被同一套判据筛（见 ChatPlan），而那一步由
// gateway 拿到的是适配器那条路上的请求。
func (r *Router) Select(candidates []Candidate, canonicalModel string, needs Needs) Plan {
	var (
		eligible []Candidate
		plan     = Plan{}
	)

	for _, c := range candidates {
		if ok, reason := c.Eligible(); !ok {
			plan.Excluded = append(plan.Excluded, Exclusion{
				ChannelID: c.Channel.ID, Name: c.Channel.Name, Reason: reason,
				Code: CodeIneligible})
			continue
		}
		if !c.Channel.Supports(canonicalModel) {
			plan.Excluded = append(plan.Excluded, Exclusion{
				ChannelID: c.Channel.ID, Name: c.Channel.Name,
				Reason: fmt.Sprintf("不支持模型 %q", canonicalModel),
				Code:   CodeModelNotDeclared})
			continue
		}
		if code, reason := c.profileRejection(needs); code != "" {
			plan.Excluded = append(plan.Excluded, Exclusion{
				ChannelID: c.Channel.ID, Name: c.Channel.Name, Reason: reason,
				Code: code, Unsupported: true})
			continue
		}
		if code, reason := c.DrainReason(); code != "" {
			plan.Excluded = append(plan.Excluded, Exclusion{
				ChannelID: c.Channel.ID, Name: c.Channel.Name, Reason: reason,
				Code: code})
			continue
		}
		eligible = append(eligible, c)
	}

	plan.Candidates = r.order(eligible)
	return plan
}

// emptyError 说明一个候选都不剩是为什么。
//
// 分两种。**全部候选都被模型目录判据挡掉**时，结论是「这些候选做不了这次
// 请求」——与「试过、上游一个个都说自己不支持」是同一件事，措辞也就用同一
// 句（见 Error 与 allCapabilityMismatch）；区别只在判据来自目录的声明而不是
// 上游的响应，所以这里一个候选都没被试过。其余情形（健康度、额度、渠道没
// 声明支持这个模型、压根没有候选）沿用原来的「没有可用的渠道」。
//
// 判据写成「所有排除都是目录判据」而不是「有目录判据的排除」：只要还有别的
// 原因说得通，就不该说「都不支持这次请求」——那句话会让人去查模型目录，
// 而真正要改的可能是某个渠道的额度。
func (p Plan) emptyError() *Errors {
	if p.unsupportedOnly() {
		return &Errors{Unsupported: len(p.Excluded), Excluded: p.Excluded}
	}
	return &Errors{Excluded: p.Excluded}
}

// unsupportedOnly 判断这次排除是不是全部来自模型目录判据。
func (p Plan) unsupportedOnly() bool {
	if len(p.Candidates) > 0 || len(p.Excluded) == 0 {
		return false
	}
	for _, e := range p.Excluded {
		if !e.Unsupported {
			return false
		}
	}
	return true
}

// order 把候选排成尝试顺序。
//
// 排序规则：
//
//  1. priority 降序——这是商家的显式意图，不该被任何打分盖过。
//  2. 同 priority 内按分数降序。分数是各维度的加权和。
//  3. 同分之间按 weight 加权随机——**不是**严格顺序。
//
// 为什么第 3 步是加权随机而不是固定顺序：固定顺序会让所有流量压在
// 排序第一的那个渠道上，于是
//
//   - 一台坏渠道影响的是全部流量，而不是一部分；
//   - 其余渠道长期空转，健康数据陈旧，等主力渠道挂了才第一次被使用，
//     而那时才发现它也不行。
//
// 加权随机让每个渠道都在持续被使用。代价是同一条请求两次走不同渠道，
// 因此对上游缓存的命中率会下降——这一点在缓存亲和那一层另议。
func (r *Router) order(candidates []Candidate) []Candidate {
	if len(candidates) == 0 {
		return nil
	}
	type scored struct {
		c     Candidate
		score float64
		// tie 是组内随机排序用的键，避免了排序不稳定带来的偏差。
		tie float64
	}
	// 额度能不能比，是**整个候选集**的性质：两个余量只有在同一单位下才有
	// 大小之分。所以这个判断在循环外算一次，再交给每个候选打分。
	// 插件契约用的是同一个函数（见 MixedQuotaUnits）。
	mixedUnits := MixedQuotaUnits(candidates)

	grouped := make(map[int][]scored)
	for _, c := range candidates {
		grouped[c.Priority] = append(grouped[c.Priority], scored{
			c:     c,
			score: r.score(c, mixedUnits),
			tie:   r.float64(),
		})
	}

	priorities := make([]int, 0, len(grouped))
	for p := range grouped {
		priorities = append(priorities, p)
	}
	// priority 大者先选。
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	out := make([]Candidate, 0, len(candidates))
	for _, p := range priorities {
		group := grouped[p]
		// 同 priority 内：分数降序。同分之间用 tie 打散——
		// 本轮请求内不再考虑 weight，因为 weight 表达的是**长期比例**，
		// 而这里排的是「这一条请求的尝试顺序」。
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].score != group[j].score {
				return group[i].score > group[j].score
			}
			return group[i].tie < group[j].tie
		})
		for _, s := range group {
			out = append(out, s.c)
		}
	}
	return out
}

// Score 计算一个候选的分数，越大越先选。
//
// 每一项都归一到 0..1 再乘权重，而不是用原始量纲相加：
// 成本与延迟的量纲差着几个数量级，直接相加会让成本完全主导结果，
// 而那多半不是使用者的本意。
//
// 没有数据的维度**不参与**，而不是给一个中性分：中性分仍然会进分母，
// 于是「这一维没有数据」会稀释其余维度的差别。剔除之后，有数据的维度
// 按各自权重归一，分数的量纲仍是 0..1。
//
// 它按「所有候选的额度单位相同」来算：跨单位的判断需要看到整个候选集，
// 而这里只有一个候选。真正排序时走的是 score（由 order 传进那个判断），
// 所以排序结果与直接调本函数得到的分数**可能不同**——差别只在额度那一项，
// 而且只在候选集里存在多种单位时出现（见 scoreQuota）。
//
// 目前只有测试在调它：它是「解释某个候选为什么排在这里」的出口，
// 生产排序走的是 order→score，还没有哪个界面或接口把分数摆出来。
func (r *Router) Score(c Candidate) float64 {
	return r.score(c, false)
}

// score 是 Score 的内部形态：mixedUnits 由调用方给出。
//
// # 没有数据的维度整条剔除
//
// 每个维度先回答「我这一维有没有数据」：成本、延迟、缓存三项各自可能
// 没有（见 Candidate 上的三个字段），健康度与额度则总是有意见——
// 它们对「没观测过」给出的中性分本身就是一种判断，而不是缺失。
//
// 没有数据的维度连权重一起从分母里去掉，剩下的按各自权重归一。这样
// 「权重写着在起作用、实际是常数」这件事不可能再发生：一个维度的权重
// 只有在它真的投了票时才进分母。
//
// 权重非正的维度同样剔除：负权重会把「越小越好」反过来，还会让分母
// 失去意义；0 权重本来就不该影响结果。
func (r *Router) score(c Candidate, mixedUnits bool) float64 {
	w := r.Weights
	if w == (Weights{}) {
		w = DefaultWeights
	}

	var weighted, total float64
	add := func(weight, s float64, ok bool) {
		if !ok || weight <= 0 {
			return
		}
		weighted += weight * s
		total += weight
	}

	// 健康度与额度总是参与：它们对「没有观测过」有自己的意见
	// （中性分），那是判断而不是缺失。
	add(w.Health, scoreHealth(c.Health), true)
	add(w.Quota, scoreQuota(c.Quota, mixedUnits), true)

	if s, ok := scoreCost(c.CostPerUnit); ok {
		add(w.Cost, s, true)
	}
	if s, ok := scoreLatency(c.Health.LatencyP95); ok {
		add(w.Latency, s, true)
	}
	if s, ok := scoreCache(c.CacheHitRate, c.SampleCount); ok {
		add(w.Cache, s, true)
	}

	if total <= 0 {
		// 一维都没有数据（或权重全为 0）：所有候选同分，由 tie 打散。
		// 返回 0 而不是 NaN——NaN 进排序会让顺序变成未定义的。
		return 0
	}
	return weighted / total
}

// MixedQuotaUnits 判断候选集里是不是存在多种额度单位。
//
// 单位未知（空）也算一种：一个「不知道按什么计」的渠道与一个「按元计」的
// 渠道之间同样不可比，所以它与任何已知单位放在一起都算混合。
//
// 只统计**有数值**的候选：没有数值的渠道那一项本来就是中性分，
// 与单位无关，把它算进来会让「一个查不到额度的渠道」把其余渠道的额度
// 全部降成中性分——那等于让「不知道」污染「知道」。
//
// # 这是这条规则的唯一一处实现
//
// 「跨单位不可比」有两个消费者，它们必须看到同一件事：
//
//   - route 自己的打分：混合时 scoreQuota 给中性分（order 调用）
//   - 插件契约：混合时 plugin.RouteContextFrom 把 Remaining 全部置 null，
//     让插件看到「这里没有可用的信息」，而不是一组不可比的数
//
// 两处各写一遍的话，插件的行为会与内部打分悄悄分叉，而那时没有人能解释
// 「插件为什么这么选」——解释的人会拿「内核是怎么算的」去对，然后对不上。
// 所以它是导出的：插件侧调它，而不是照着抄一遍。
func MixedQuotaUnits(candidates []Candidate) bool {
	var first string
	seen := false
	for _, c := range candidates {
		if c.Quota.Remaining == nil {
			continue
		}
		if !seen {
			first, seen = c.Quota.Unit, true
			continue
		}
		if c.Quota.Unit != first {
			return true
		}
	}
	return false
}

// scoreHealth 把健康度映射到 0..1。
//
// 成功率的权重比连续失败低：成功率是历史，而连续失败是**当下**。
// 一个历史上很稳但现在连续失败的渠道，比一个一直不太稳的渠道更危险。
func scoreHealth(h Health) float64 {
	// 连续失败直接扣到很低，但不为零——为零会让它与「刚上线、无数据」
	// 无法区分，而后者应该得到中性分数。
	streak := 1.0 / (1.0 + float64(h.ConsecutiveFailures))
	base := h.SuccessRate
	if base <= 0 {
		// 没有样本时给中性值，而不是 0：新品没有历史不等于它不好。
		base = 0.5
	}
	return 0.4*base + 0.6*streak
}

// scoreQuota 把额度映射到 0..1。
//
// # 这里只回答「怎么排」
//
// 「这个额度状态能不能用」是**判断**，它只有一处实现：internal/quota 的
// Routable 与 Usable，由 DrainReason 调用。本函数回答的是另一半问题——
// 在都能用的候选里，谁的额度更该先用掉。两者不互相替代：判断是布尔的、
// 硬性的，排序是连续的、可权衡的。
//
// 两道兜底仍然留着，因为 Score 是导出 API，调用方可以绕过 DrainReason
// 直接调它。它们**不是**第二处判断，而是把同一个判断的结果映射成分数：
//
//   - 见底的候选给最低分（Usable 为假）。给 0 而不是负分：负分会让权重
//     之和的意义变得难解释。这一档与单位无关，所以排在下面那道跨单位的
//     判断之前——余量见底就是见底，与它按什么计无关。
//   - 没有数值的候选给中性分。走到这里的是「从未观测过额度」那一档
//     （上游不支持查询额度）；「观测过却没有数值」已经被 DrainReason
//     挡在候选集之外了。
//
// # 这里的数是什么单位
//
// Remaining 的单位由 State.Unit 说明，而候选渠道来自不同上游，
// 口径可能不同：按 token 计的套餐（百万量级）、按金额计的账户（个位到百位）、
// 按请求数计的配额（千量级）。把「剩余 800000 token」与「剩余 5 元」放进
// 同一个 1/(1+x) 里，前者永远得 0 分、后者永远接近满分——**分差来自单位，
// 而不是来自余量**，排序被一个与「还剩多少」无关的维度支配。
//
// 所以 mixedUnits 为 true（候选集里存在多种单位）时这一项给中性分，
// 与「没有数值」同一档：不比，而不是乱比。
//
// 为什么不归一化折算：那需要一个「1 元值多少 token」的假设，而那个假设
// 没有依据——把它写进代码等于把一个错换成另一个错，还多了一处没人能验证
// 的换算。**单位相同就可以比**，那时 1/(1+x) 的语义是明确的。
//
// # 可信度是权重，不是闸门
//
// 这个数有多可信由 State.Confidence 说明：实测是 1，自记账按距上次实测的
// 时长从 0.8 衰减到 0.2，从错误里推断是 0.6（见 internal/quota）。这里把它
// 当**权重**：算出来的分是「这次观测说出的分」与中性分 0.5 的凸组合，
// 可信度越低越靠近中性分。
//
// 为什么不按阈值排除：按间隔自动探测上游的调度器**还没有实现**，一次陈旧的
// 观测可能永远不会被刷新，排除等于让一个渠道因为一个没人会再看的旧数字
// 永久不可用。降权保留了「它仍然可能被选中」，只是不再因为一个陈旧数字被
// 优先或被排斥。
//
// 为什么退到 0.5 而不是别的档：0.5 就是上面那道兜底里「额度未知」的分。
// 可信度低意味着**没有意见**，与「不知道」是同一件事，不该为它另立一档。
//
// 两处刻意不参与加权：
//
//   - **见底**（Usable 为假）给 0。它是事实而不是偏好，与它有多可信无关；
//     而且这一档的处置在 DrainReason（判断）里，本函数只是把那个判断映射成
//     分数。让低可信度把「已经用完了」抬回中性，等于让一个旧数字重新放行
//     一个见底的渠道。
//   - **没有数值**与**跨单位**本来就是中性分，往中性分靠是它自己。
func scoreQuota(q quota.State, mixedUnits bool) float64 {
	if !q.Usable() {
		return 0
	}
	if q.Remaining == nil {
		return 0.5
	}
	if mixedUnits {
		return 0.5
	}

	// 「用掉它」的紧迫度：剩余越少越紧迫。用 1/(1+remaining) 之类的
	// 反比会让「剩余 0.5 与剩余 0.6」的差别大于「1000 与 2000」，
	// 这正是想要的——要到见底时才真的着急。
	urgency := 1.0 / (1.0 + *q.Remaining)

	// 到期越近越紧迫。没有到期时间时按中性处理。
	expiry := 0.5
	if q.ResetsAt != nil {
		remaining := time.Until(*q.ResetsAt)
		switch {
		case remaining <= 0:
			expiry = 1.0
		case remaining < time.Hour:
			expiry = 0.95
		case remaining < 24*time.Hour:
			expiry = 0.8
		case remaining < 7*24*time.Hour:
			expiry = 0.6
		default:
			expiry = 0.4
		}
	}

	// 这次观测说出的分：紧迫度与到期各占一半。
	raw := 0.5*urgency + 0.5*expiry

	// 再按可信度把它往中性分拉。写成凸组合而不是 0.5 + c*(raw-0.5)，
	// 是为了让两个端点**精确**落在 raw 与 0.5 上：后者在 raw 很小时
	// （例如余量 1000、十天后再到期，raw 约 0.2）会因两次舍入差最后一个
	// ulp，而「实测值的行为一字不变」是这条判据的硬要求。
	const neutral = 0.5
	c := confidenceWeight(q.Confidence)
	return raw*c + neutral*(1-c)
}

// confidenceWeight 把 State.Confidence 收进 0..1。
//
// 越界的值只可能来自本包之外（库里的列有 CHECK 0..1，quota.Merge 写的是
// 三个常量）。仍然要收：大于 1 会让一个候选拿到比实测还高的分，负值会把
// 「分数向中性分靠拢」反过来——余量多的反而更该先用。
func confidenceWeight(c float64) float64 {
	switch {
	case c <= 0:
		// 零值也走这里。State 的零值 Confidence 是 0，于是「没填可信度」
		// 与「可信度为 0」是同一个结果：退成中性分。这是有意的——一个
		// 不知道有多可信的数，不该比一个已知不可信的数更受优待。
		return 0
	case c >= 1:
		return 1
	default:
		return c
	}
}

// scoreCost 把单位成本映射到 0..1，越小越好。
//
// cost 为 nil 表示没有数据，ok 为假——这一维不参与加权，而不是给中性分。
// 给中性分等于让「不知道多少钱」占掉一份权重：它既没有把未知成本的渠道
// 排在已知便宜的前面，也稀释了其余维度的差别，而这两件事都不会报错。
//
// 有数据且恰好为 0 就是**免费**，给满分。这与「没有数据」是两个不同的
// 输入，不该有同一个输出。
func scoreCost(cost *float64) (float64, bool) {
	if cost == nil || *cost < 0 {
		// 负成本只可能来自坏数据（库里那一列是 numeric 且写入方算的是
		// 总额除以请求数）。按「没有数据」处理：宁可少一维，
		// 也不要让一个读不通的数影响排序。
		return 0, false
	}
	// 1/(1+cost) 在有量纲的成本上不理想，但它的好处是**不需要知道全局范围**：
	// 归一化需要所有候选的成本，而那要求先收集全部候选才能算一个人的分。
	// 现在这样可以让每个候选独立打分，也便于测试。
	return 1.0 / (1.0 + *cost), true
}

// scoreLatency 把延迟映射到 0..1，越小越好。
//
// p95 为 nil 表示没有数据，这一维不参与加权。与 scoreCost 同一条理由：
// 未知不是「中等」，而是「这一维没有意见」。
func scoreLatency(p95 *time.Duration) (float64, bool) {
	if p95 == nil || *p95 < 0 {
		return 0, false
	}
	// 以秒为单位代入反比函数。1 秒以内接近满分，
	// 10 秒时降到约 0.09——足够把慢渠道压下去，但不会压成 0。
	return 1.0 / (1.0 + p95.Seconds()), true
}

// minCacheSamples 是缓存命中率被采信所需的最小样本数。
//
// 一两次请求的命中率不说明任何事，用它去影响路由会让流量在两个渠道之间
// 无意义地摇摆。样本不足与「没有数据」同一档：这一维不参与加权。
const minCacheSamples = 20

// scoreCache 把缓存命中率映射到 0..1。
//
// 没有数据（hitRate 为 nil）或样本不足时 ok 为假，这一维不参与加权。
// 有数据且样本足够时，命中率**恰好为 0** 是「一次都没命中」这个事实，
// 给 0 分——它与「没有数据」必须分得开。
func scoreCache(hitRate *float64, sampleCount int64) (float64, bool) {
	if hitRate == nil || sampleCount < minCacheSamples {
		return 0, false
	}
	if *hitRate < 0 {
		return 0, true
	}
	if *hitRate > 1 {
		return 1, true
	}
	return *hitRate, true
}

func (r *Router) float64() float64 {
	if r.Rand != nil {
		return r.Rand.Float64()
	}
	return rand.Float64()
}

// ── 执行 ────────────────────────────────────────────────────────────────

// Attempt 是一次尝试的记录。
type Attempt struct {
	ChannelID   int64
	ChannelName string
	Err         error
	// Retryable 是这次失败是否值得换下一个渠道。
	Retryable bool
	// StartedAt 用于统计与排查。
	StartedAt time.Time
	Duration  time.Duration
}

// Result 是一次 Chat 调用的结果。
type Result struct {
	Response *channel.Response
	// Channel 是最终成功（或最后失败）的那个渠道。
	Channel channel.Channel
	// UpstreamModel 是这次请求**实际发出去**的上游模型名（见 upstreamRequest）。
	//
	// 它由本包给出来，因为只有这里知道最终用的是哪一次翻译：别名逐渠道配，
	// 换渠道重试时每个候选各翻一次，翻出来的名字可能不同。上层据此记下
	// billing_requests.upstream_model——那一列要回答的是「这次实际用了哪个
	// 上游名」，而事后重查一次别名答不出这个问题（配置可能已经改过）。
	//
	// 它与 Channel **成对**：两者在同一个位置落，成功与失败一样有值。
	// 失败路径上它说的是「最后一次真的发出去的那次用的是哪个名字」——
	// 那一次调用发生过，也就有名字可记；而账单上那两列正是成对读的
	// （见 internal/ledger/store 的 Release）。只在成功时填它，失败行上
	// 就只剩渠道这一半，而需要这一对的那类排查（某个渠道对某批模型全在
	// 报错）恰好落在失败行上。
	//
	// 一次调用都没发生时它是空的（候选集为空、别名翻不出来、context 已取消），
	// 与 Channel 的零值同时出现。
	UpstreamModel string
	// OutboundRequest 是**最后一次尝试真正发出去的那份请求**，就是交给执行器
	// 的那一个对象（见 upstreamRequest），不是照着它再拼一份。
	//
	// # 为什么要有它
	//
	// 发出去的那份请求与本包收到的那份**不是同一份**：上游名按选中的渠道翻过，
	// 别名上配的参数覆盖也应用上去了（见 upstreamRequest）。也就是说「上游实际
	// 收到了什么」只有这里答得出来，而收到本包入参的地方（internal/gateway 采
	// 请求快照那一步）答不出来——它手上只有调用方发来的那份。
	//
	// 再拼一份是不行的：覆盖的判据只该有一处（见 internal/overrides），拼第二份
	// 就是第二处，两处迟早不一致，而那种不一致的表现是快照与实物对不上。
	//
	// # 它与 UpstreamModel / Channel 的分工
	//
	// 三者口径相同：**最后一次真的发出去的那一次**，失败的那次也算（那次调用
	// 发生过，也就有东西可记）。UpstreamModel 是从这份请求上摘下来的一个字段，
	// 留它是因为账单那一列读的是名字而不是整份请求。
	//
	// 一次都没发出去时它是 nil（候选集为空、别名翻不出来、context 已取消——
	// 与 UpstreamModel 为空、Channel 为零值同时发生）。它必须是 nil 而不是一份
	// 空的请求：调用方据此分辨「这次请求根本没发出去」与「发出去的那份恰好是
	// 零值」，前者要退回调用方发来的那份去采快照。
	OutboundRequest *channel.Request
	// Attempts 按顺序记录了每一次尝试，包括失败的。
	// 计费与排查都需要它：失败的调用不计费，但要知道发生过。
	Attempts []Attempt

	// Excluded 是被选路筛掉的候选及其原因（见 Plan.Excluded）。
	//
	// # 它与 Attempts 分开
	//
	// Attempts 记的是**真的发出去过**的候选；被筛掉的那些连试都没试过，
	// 在上面的列表里没有影子。少了这一份，「我配了三个渠道为什么只用一个」
	// 与「我只配了一个渠道」在调用方看到的现场上完全一样——而前者是配置
	// 问题，要改的是被筛掉那几条渠道的配置。
	//
	// 它随第一次成功的选路就一直带着（不进 Attempts 的循环），因为筛选发生
	// 在循环之前，与被选中谁无关。
	Excluded []Exclusion
}

// Errors 是候选都试不下去时返回的错误。
type Errors struct {
	Attempts []Attempt
	// Untried 是**没有试过**的候选数。提前返回（遇到不可重试的错误）时它
	// 大于零，试完为止时它是零。
	//
	// 它必须与 Attempts 分开：只报「试过几个」会让「全部 1 个候选渠道都失败」
	// 出现在还有 2 个候选没试过的时候——那句话把一次提前放弃说成了「都试过了」，
	// 看日志的人据此去查上游，而真正要改的是排在前面的那条配置。
	Untried int
	// Unsupported 是被模型目录判据排除、**一次都没试过**的候选数。
	//
	// 它与 Untried 分开：Untried 说的是「试了几个之后提前放弃，还有几个没试」，
	// 而这里说的是「一个都没试，因为目录里那些声明说它们做不了这件事」。
	// 两者都会让「全部 N 个候选渠道都失败」失真，但要说的话不同——后者与
	// 「试过、上游一个个都说自己不支持」是同一个结论（见 Error 的
	// allCapabilityMismatch 那一支），措辞也就同一句。
	Unsupported int
	// Excluded 是**一个候选都不剩**时被筛掉的那些候选与原因（见 Plan.Excluded）。
	//
	// 它只在这一支非空：一个候选都没剩下时，Plan 不进 Result，被筛掉的那些
	// 就只剩这里有——而「我配了三个渠道为什么一个都没用上」正是这一支要回答
	// 的问题。提前放弃与试完为止那两支里它是空的：那时调用方手上的
	// Result.Excluded 才是那一份（见 Result.Excluded）。
	Excluded []Exclusion
}

func (e *Errors) Error() string {
	if len(e.Attempts) == 0 {
		if e.Unsupported > 0 {
			// 与下面 allCapabilityMismatch 那一支同一句话、同一个结论：
			// 上游一次都没被调用过，是我们这边的配置/目录声明覆盖不到
			// 这次请求。
			return fmt.Sprintf("%d 个候选渠道都不支持这次请求", e.Unsupported)
		}
		return "没有可用的渠道"
	}
	last := e.Attempts[len(e.Attempts)-1].Err
	switch {
	case e.Untried > 0:
		return fmt.Sprintf("第 %d 个候选渠道失败，还有 %d 个候选没试: %v",
			len(e.Attempts), e.Untried, last)
	case allCapabilityMismatch(e.Attempts):
		// 一句「都失败」会让人去查上游，而这里上游一次都没有真的被调用过
		// ——是我们这边的配置没有覆盖这条入口。
		return fmt.Sprintf("%d 个候选渠道都不支持这次请求: %v", len(e.Attempts), last)
	default:
		return fmt.Sprintf("全部 %d 个候选渠道都失败，最后一个错误: %v",
			len(e.Attempts), last)
	}
}

// allCapabilityMismatch 判断这几次尝试是不是都属于「这条候选做不了这件事」。
func allCapabilityMismatch(attempts []Attempt) bool {
	for _, a := range attempts {
		if !capabilityMismatch(a.Err) {
			return false
		}
	}
	return true
}

// Unwrap 返回最后一个错误，让 errors.Is/As 能对上游错误类型生效。
//
// 一个候选都没试过、但全部被目录判据挡掉时返回 channel.ErrUnsupported：
// 那一支与下面 allCapabilityMismatch 那一支说的是同一件事（这条候选做不了
// 这件事），只是判据来自目录的声明而不是上游的响应。调用方据此把「配置/
// 目录没覆盖这条入口」与「上游都挂了」分开。
func (e *Errors) Unwrap() error {
	if len(e.Attempts) == 0 {
		if e.Unsupported > 0 {
			return channel.ErrUnsupported
		}
		return nil
	}
	return e.Attempts[len(e.Attempts)-1].Err
}

// ErrNoCandidates 表示没有可用候选。
var ErrNoCandidates = errors.New("没有可用的渠道")

// Executor 是真正执行请求的东西。抽成接口是为了让路由逻辑能脱离 HTTP 测试，
// 也为了让「重试到第几个为止」这类规则可以被单测覆盖。
type Executor interface {
	Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error)
}

// worthNextCandidate 判断一次失败是否值得换下一个候选渠道。
//
// 它比 channel.IsRetryable 多认两种错误：ErrUnknownType 与 ErrUnsupported。
// 两者都不是「上游坏了」，而是「这一条候选做不了这件事」——前者是库里那条
// 配置当前版本的二进制不认识，后者是渠道类型与这条入口对不上。它们属于
// **这条候选**，不属于**这次请求**。
//
// # ErrUnsupported 为什么也在这里（#348）
//
// 它以前只从 ListModels / Quota 出来，两个都不在请求热路径上，所以「不换
// 下一个」当时是安全的。现在 Chat / Decide 也会返回它，而**候选池是按模型名
// 筛的、不看渠道类型**：同一个规范名可以同时挂在 chat 渠道与决策渠道上。
// 此时「不换下一个」意味着一个配错类型的渠道能打断该模型的**全部**流量，
// 而后面明明还有能用的候选。老论证「换谁都是同一个错」在多协议候选池里
// 不再成立——换一个类型对的候选就是另一个结果。
//
// 注意它与 channel.IsRetryable 的分工，别把两者合成一个：IsRetryable 回答
// 「这次失败该算在上游头上吗」（它决定渠道健康度，见 gateway 的
// channelAtFault），而一个渠道类型不对不是它坏了。所以 ErrUnsupported 在
// 那边仍然不是「可重试」，在这里却是「值得换下一个」。
//
// 判据放在这里而不是适配器或执行器里：执行器只看得到自己那一次调用，
// 只有路由层知道后面还有谁，也就只有它能决定「值不值得换下一个」。
func worthNextCandidate(err error) bool {
	return channel.IsRetryable(err) || capabilityMismatch(err)
}

// capabilityMismatch 判断一次失败是不是「这条候选做不了这件事」。
//
// 与 worthNextCandidate 分开，是因为它还有第二个读者：错误文本要按它区分
// 「试过但失败了」与「压根不支持」（见 Errors.Error）。两处用同一个判据，
// 免得「换下一个」与「怎么措辞」各判各的。
func capabilityMismatch(err error) bool {
	return errors.Is(err, channel.ErrUnknownType) || errors.Is(err, channel.ErrUnsupported)
}

// ChatModel 按一个规范模型名执行一次选路，请求由本包自己合成。
//
// # 它为什么存在
//
// 决策那条表面（见 internal/gateway 的 Decide）没有 chat 形状的请求：它要的
// 只是「按这个规范名选一个渠道、按渠道翻出上游名、发出去」。走 Chat 的话，
// 调用方得自己造一个 channel.Request 当**载体**递进来，于是「本包除 Model
// 之外不读载体的别的字段」成了一条跨包的口头约定——它只写在调用方的注释里，
// 改本包的人看不见。
//
// 合成搬进本包之后，那条约定变成这里就能读到的事实：载体是本函数造的一个
// 只含 Model 的结构体字面量，而它唯一的读者是 upstreamRequest（它只做
// `attempt := *req; attempt.Model = 上游名`）。
//
// **改 upstreamRequest、让它开始读 req 的别的字段之前，先看这条路径。**
// 这条路径递进来的载体除 Model 之外全是零值，读到的会是零值本身，而且不会
// 报错。chat 那条路径的载体是真请求，读错了至少读到的还是真数据；这里不是。
//
// 别名覆盖（#354）正是这个动作，当时按上面这句看过一次：upstreamRequest
// 开始读 req 的 Temperature / TopP、并把覆盖算出的 max_tokens 写进 attempt。
// 本函数合成的载体上那些字段是零值，而它唯一的调用方 decideCall 明确只读
// Model——所以覆盖在这条路上**不生效**。风险没有消失，只是有人看过一次：
// 决定路径今天仍然不适合承载「本包读载体的别的字段」。
func (r *Router) ChatModel(ctx context.Context, exec Executor, canonicalModel string, candidates []Candidate) (*Result, error) {
	return r.Chat(ctx, exec, &channel.Request{Model: canonicalModel}, candidates, canonicalModel)
}

// Chat 按选路结果依次尝试，直到成功或用尽候选。
//
// 重试的边界由 worthNextCandidate 决定：429、5xx、网络层失败换下一个；
// 400、401 这类换渠道也是同一个错，重试只会把费用与延迟乘以候选数。
// 唯一的例外是 ErrUnknownType——见 worthNextCandidate。
//
// req.Model 是**规范名**：本包在每次尝试之前按那个候选的渠道把它翻成上游名
// （见 upstreamRequest），并且**不改动 req 本身**。
//
// 它是「筛 + 排 + 执行」一步到位的形态。要在这两步之间插一件事（今天只有
// 插件）的调用方走 ChatPlan。
func (r *Router) Chat(ctx context.Context, exec Executor, req *channel.Request, candidates []Candidate, canonicalModel string) (*Result, error) {
	return r.ChatPlan(ctx, exec, req, r.Select(candidates, canonicalModel, NeedsOf(req)), 0)
}

// ChatPlan 按一份**已经算好的**选路结果执行，并把 pinChannelID 钉在第一位。
//
// # 它为什么存在
//
// 选路要分两步做：先由 Select 筛出候选，再把「筛过的候选」交给一个外部
// 决策者，最后才执行它给出的顺序。中间那一步**必须在包外发生**——
// internal/plugin 已经 import 本包（见它的 adapter.go），本包反向 import
// 它就成环。
//
// 没有这个入口时，包外的调用方只有两条错路：自己重写一遍 Eligible /
// DrainReason 的筛选（那是第二份实现，两份迟早不一致），或者把筛选结果
// 丢掉、重新走一次 Chat（那样决策者看到的候选与真正执行的不是同一个东西，
// 而两者分叉的表现是「它选了一个渠道，请求却发去了另一个」）。
//
// # pinChannelID 的语义
//
// 0 表示不钉，顺序与 Select 排出来的完全一致；非 0 时把那个候选提到最前，
// 其余候选保持原顺序跟在后面（它失败了照旧换下一个，见 pinFirst）。
//
// **调用方应当把同一个 plan 先交给决策者、再拿回来执行**：那样「决策者看到
// 的候选」与「被执行的候选」是同一份，这一点由调用约定保证，本方法不重复
// 校验——决策者那一侧已经挡下了候选集之外的决策（见 internal/plugin 的
// pickChannel），而本方法看不到那一侧的上下文。
//
// # 规范名在这里翻成上游名
//
// 每个候选在被执行**之前**按它自己的渠道翻一次（见 upstreamRequest），
// 所以决策者看到的是规范名（那是调用方要的名字），上游收到的是上游名。
// 翻不出来时这次请求失败，不换下一个候选——见 chatWithPlan。
func (r *Router) ChatPlan(ctx context.Context, exec Executor, req *channel.Request, plan Plan, pinChannelID int64) (*Result, error) {
	return r.chatWithPlan(ctx, exec, req, plan.pinFirst(pinChannelID))
}

func (r *Router) chatWithPlan(ctx context.Context, exec Executor, req *channel.Request, plan Plan) (*Result, error) {
	if len(plan.Candidates) == 0 {
		return nil, plan.emptyError()
	}

	result := &Result{Excluded: plan.Excluded}
	for _, c := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			// 客户端断开：不再尝试剩余渠道。已经发生的尝试照常记录，
			// 因为其中可能有已经产生费用的。
			//
			// 所以交回 result 而不是 nil，即使它里面一条尝试都没有：下面
			// 每一个分支交回的也是 result（含 aliasErr 那两支），调用方
			// 据此把「打到了哪个渠道」写进这一行的收尾。交回 nil 的话，
			// 一个真的发出去过的请求在库里读成「一个候选都没选中」，与
			// 「一次调用都没发生」逐列相同。
			return result, err
		}

		attempt, aliasErr := r.upstreamRequest(ctx, req, c)
		if aliasErr != nil {
			// 翻不出上游名是**配置/装配**问题，不是上游抖动：不换下一个
			// 候选，直接失败。换下去只会把「这个渠道没配别名」说成
			// 「所有候选都试过了」，而后者看起来像上游全挂了。
			//
			// 已经试过的那几次照常带回去（result 里），调用方要按它们记
			// 渠道健康度——那些调用真的发生过。
			return result, aliasErr
		}

		started := r.now()
		resp, err := exec.Chat(ctx, c.Channel, attempt)
		result.Attempts = append(result.Attempts, Attempt{
			ChannelID:   c.Channel.ID,
			ChannelName: c.Channel.Name,
			Err:         err,
			Retryable:   worthNextCandidate(err),
			StartedAt:   started,
			Duration:    r.now().Sub(started),
		})
		result.Channel = c.Channel
		// 上游名与渠道在**同一个位置**落，成功失败都一样：这一对说的是
		// 「这次实际发去了哪里、用哪个名字」，而失败的那一次调用同样发出去过。
		// 放进下面那个 if 里的话，失败路径上就只剩渠道这一半（见
		// Result.UpstreamModel 的注释）。
		//
		// 别名翻不出来时走的是上面那条 return，两列都还是上一次尝试留下的值
		// ——那次尝试真的发生过，记它是对的；一次都没发生时两者同时是零值。
		result.UpstreamModel = attempt.Model
		// 发出去的那份请求也在同一个位置落：它与上面那一对说的是同一次尝试
		// （见 Result.OutboundRequest）。记的是 attempt 本身而不是它的副本——
		// 上层要的正是「执行器收到的那一个」，复制一份只会多出一份可能走样的
		// 东西。
		result.OutboundRequest = attempt

		if err == nil {
			result.Response = resp
			return result, nil
		}
		if !worthNextCandidate(err) {
			// 不可重试：换渠道也是同一个错，立刻返回。
			// 已经记录的尝试保留下来，调用方需要知道试过谁；没试过的那些
			// 记进 Untried，免得错误文本把「提前放弃」说成「都试过了」。
			return result, &Errors{
				Attempts: result.Attempts,
				Untried:  len(plan.Candidates) - len(result.Attempts),
			}
		}
	}
	return result, &Errors{Attempts: result.Attempts}
}

// upstreamRequest 给出这次尝试要发出去的那份请求：规范名已按**这个渠道**
// 翻成上游名，这条别名上配的参数覆盖也已经应用上去。
//
// # 为什么每次尝试各翻一次
//
// 别名是逐渠道配的（同一个规范名在这家上游叫 vendor/x、在那家叫 x），
// 而换渠道重试意味着同一个请求会发给不同的渠道。翻一次、所有候选共用的话，
// 第二个渠道收到的会是第一个渠道的名字——那是一次**静默发错**：上游可能
// 恰好也认识那个名字（两家叫法偶尔相同），于是请求成功、账单正常，只有
// 用的是别家的名字这件事没有任何地方报出来。
//
// 参数覆盖同理，而且更硬：它也是**行**上的字段，第二个渠道那条别名上的
// max_tokens 与第一家配的可能不同。翻一次共用会把第一家配的上限用到别家身上。
//
// # 为什么返回副本
//
// req 是调用方的东西，它还要用它去渲染响应（报给调用方的模型名是**他要的
// 那个**，见 internal/api 的 renderSuccess）。就地改 Model 会让响应里的 model
// 变成上游名，也会让重试循环的第二圈拿一个已经翻过的名字再翻一次。覆盖也写在
// 这份副本上：就地改调用方那份的话，重试循环的第二圈会带着第一个渠道的上限
// 去发第二个渠道。
//
// 覆盖**只**落在这份副本上。上游名同样是这份副本上的东西，两者一起构成
// 「上游实际收到了什么」，而那份请求由 Result.OutboundRequest 交出去——上层
// 采请求快照时用的就是它，不是调用方发来的那份。
func (r *Router) upstreamRequest(ctx context.Context, req *channel.Request, c Candidate) (*channel.Request, error) {
	if r.Aliases == nil {
		return nil, ErrNoAliasSource
	}
	// 变量名写全成 upstreamName：本包与 internal/gateway、internal/api 里一路
	// 叫 upstream 的形参指的是**调用方发来的那份请求**（它的 Model 是规范名），
	// 而这里要的是「上游模型名」这一个字符串。同一个词在相邻两个包里指相反
	// 的东西，而本包反复强调的正是「上游名与规范名别混」。
	upstreamName, override, err := r.Aliases.Alias(ctx, c.Channel.ID, req.Model)
	if err != nil {
		// 原样返回，不再包一层：internal/catalog 的错误已经写明了渠道 id 与
		// 规范名，再包一次只是把同一句话重复两遍。
		return nil, err
	}

	attempt := *req
	attempt.Model = upstreamName
	// 覆盖在**翻名之后**应用：它改的是发出去的那份请求，而不是调用方那份
	// ——上游名也是这么处理的。非法覆盖会在这里报错（只可能来自绕过写入
	// 校验的历史行或手工改库），与别名翻不出来一样按配置问题处置：不换下一个
	// 候选，直接失败。静默跳过的话，那条配置的表现是「配了没生效」，正是
	// 这一列此前的问题。
	if err := overrides.Apply(&attempt, override); err != nil {
		return nil, err
	}
	return &attempt, nil
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
