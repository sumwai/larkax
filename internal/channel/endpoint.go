package channel

import (
	"fmt"
	"slices"
)

// Endpoint 是一次上游请求打到哪条路径。
//
// 它与 Protocol 是两个维度，今天一一对应：一套协议族的适配器只往一条路径发。
// 分开命名是因为判据的两侧不是同一件事——一侧是**渠道**（它的 type 决定适配器
// 打哪条路径），另一侧是**模型**（模型目录声明这个模型在哪几条端点上被服务）。
// 两边对上才放行。
//
// 取值就是适配器实际发的路径，所以与上游模型清单里那份 supported_endpoints
// 是同一套口径。
//
// # 为什么渠道 type 不等于端点
//
// 一个上游同时有 /v1/chat/completions、/v1/responses、/v1/messages 三套端点，
// **由模型决定走哪一套**：同一个账号下 A 模型只在 chat 那边被服务，B 模型只在
// responses 那边。往模型不支持的那条端点发，上游回 400 unsupported_model——
// 从我们这边看与「模型不存在」长得一样，要等真打一次才发现。
//
// 所以渠道的 type 只承担协议含义（用哪个适配器），「这条渠道服务哪些端点」由
// 适配器实际打的那条路径回答（见 Protocol.Endpoint），而「这个模型在哪几条
// 端点上被服务」是模型目录里的一条声明（见 internal/catalog 的 Capabilities）。
type Endpoint string

// 各端点的取值。
//
// 写的是**路径**而不是一个抽象名字，因为上游的模型清单也是按路径报的
// （supported_endpoints 那一条），两套口径合一套才不会在中间多一层翻译。
//
// 常量名带 Endpoint 前缀而不是叫 ChatCompletions：这四个取值会与协议名一起
// 出现在调用点上，没有前缀时 `channel.Responses` 读不出它说的是协议还是路径。
const (
	EndpointChatCompletions Endpoint = "/v1/chat/completions"
	EndpointResponses       Endpoint = "/v1/responses"
	EndpointMessages        Endpoint = "/v1/messages"
	EndpointSystemOne       Endpoint = "/v1/systemone"
)

// Valid 报告它是不是上面这几个取值之一。
//
// 它是「外部输入」那道门的判据：库里的 supported_endpoints 是人工/采集写进去的
// 一段文本，读到别的串时调用方要能分辨「这一项是错的」与「这一项没配过」。
// 后者是空 endpoints（nil 切片），不是空串——空串在这套取值里不合法。
func (e Endpoint) Valid() bool {
	switch e {
	case EndpointChatCompletions, EndpointResponses, EndpointMessages, EndpointSystemOne:
		return true
	}
	return false
}

// ParseEndpoint 把一个字符串解析成一个端点取值。
//
// # 解析不出来就报错，不返回空串
//
// 空串在这里的意思是「不知道」（见 Protocol.Endpoint），与「这一串是错的」
// 必须分开：前者的处置是放行（不知道就别拦），后者是这一行数据有问题。
// 把非法串折成空串会让它在选路里表现成「未知」，于是「这一行写错了」变成
// 「这一行没配过」——一条拼错的端点声明从此静默不生效。
func ParseEndpoint(raw string) (Endpoint, error) {
	e := Endpoint(raw)
	if !e.Valid() {
		return "", fmt.Errorf("未知的上游端点 %q（取值只能是 %s、%s、%s、%s）",
			raw, EndpointChatCompletions, EndpointResponses, EndpointMessages, EndpointSystemOne)
	}
	return e, nil
}

// protocolEndpoints 是「协议取值 → 它打的那条端点」这张表。
//
// # 它是协议取值的唯一来源
//
// 本包关于协议取值要说两件事：**认哪些**（valid，以及注册自检报错里列的那串）
// 与**每个协议打哪条端点**（Endpoint）。两件事都从这一张表回答——键是前者，
// 值是后者。
//
// 先前它们各自维护（`valid` 一个 switch、`protocolNames` 一个包级变量、
// `Endpoint` 又一个 switch），三份对不上时端点算出来是空串，而空串在端点评据
// 里等于**放行**，且没有任何自检照到：协议名是合法的、类型名与声明也一致，
// 注册与选路都不响。收成一张表之后，「加一个协议取值」只有这一处要改，
// 另一处不可能忘。
//
// 表漏没漏，由 TestEveryProtocolConstantIsInTable 兜着：它扫本包源码里的
// Protocol 常量与变量声明，逐个要求在这里有一行。
var protocolEndpoints = map[Protocol]Endpoint{
	ProtocolAnthropic:       EndpointMessages,
	ProtocolOpenAI:          EndpointChatCompletions,
	ProtocolOpenAIResponses: EndpointResponses,
	ProtocolSystemOne:       EndpointSystemOne,
}

// protocolNames 是表里那批协议名，已排序。
//
// 它只用在报错信息里（「本包认的只有 …」）：那个顺序要稳定，否则同一份配置在
// 两次启动里给出顺序不同的提示，读日志的人会以为中间有什么变了。
var protocolNames = func() []string {
	names := make([]string, 0, len(protocolEndpoints))
	for p := range protocolEndpoints {
		names = append(names, string(p))
	}
	slices.Sort(names)
	return names
}()

// Endpoint 返回这套协议族的适配器往哪条路径发请求。
//
// 未知协议拿到空串。空串在端点评据里是「不知道」，与 Valid 为 false 一起由
// 调用方决定怎么处置（本仓的处置是放行，见 route.profileRejection）。
//
// # 空串现在只有一个来源
//
// 「本版本不认识这个协议」——库里写着一个更新的版本才有的 type 时就是这种，
// 而那条渠道本来就选不中（执行器报 ErrUnknownType）。表里有的协议**不可能**
// 算不出端点：端点就是表的一列，两者同源。先前那种「协议本身合法、只是忘了给
// 它配端点」的写法已经没有位置了。
func (p Protocol) Endpoint() Endpoint {
	return protocolEndpoints[p]
}
